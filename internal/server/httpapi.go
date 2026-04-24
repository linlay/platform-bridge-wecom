// Package httpapi 实现 bridge 对 platform 暴露的 HTTP 旁路接口：
//
//	POST /api/push         —— platform 上传运行产物到 bridge，由 bridge 转发给企微
//	GET  /api/download/**  —— platform 拉取用户上传的文件
//
// 两个端点都用同一套 ticket（无签名 JWT）校验。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-wecom-bridge/internal/protocol"
)

type Config struct {
	Channel  string
	AgentKey string
	UserID   string
	Store    *FileStore

	// OnPushed 在 multipart 落盘成功后被调用，由 wecom bridge 决定是否把
	// 文件转发到企微。返回 error 会被日志记录但不影响 platform 收到的 UploadResponse
	// （platform 那边靠 WS 协议面已经知道 artifact 落地了）。
	OnPushed func(chatID, name, mimeType string, data []byte) error
}

type Server struct {
	cfg  Config
	mux  *http.ServeMux
	once sync.Once

	idCh chan int64 // 简单计数器 for fileId/requestId suffix
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux(), idCh: make(chan int64, 1)}
	s.idCh <- time.Now().UnixNano()
	s.mux.HandleFunc("/api/push", s.handlePush)
	s.mux.HandleFunc("/api/download/", s.handleDownload)
	return s
}

// Router 返回可挂载的 http.Handler。
func (s *Server) Router() http.Handler { return s.mux }

// ---- /api/push ----

type uploadInfo struct {
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	URL       string `json:"url"`
}

type uploadResponse struct {
	RequestID string     `json:"requestId"`
	ChatID    string     `json:"chatId"`
	Upload    uploadInfo `json:"upload"`
}

type result struct {
	Code int    `json:"code"`
	Msg  string `json:"msg,omitempty"`
	Data any    `json:"data,omitempty"`
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.authenticate(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// multipart
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, 400, result{Code: 10401, Msg: "参数错误, " + err.Error()})
		return
	}
	chatID := r.FormValue("chatId")
	if strings.TrimSpace(chatID) == "" {
		writeJSON(w, 400, result{Code: 10401, Msg: "参数错误, chatId is empty"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, result{Code: 10401, Msg: "参数错误, File is empty"})
		return
	}
	defer file.Close()

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" && header != nil {
		name = header.Filename
	}
	mimeType := strings.TrimSpace(r.FormValue("type"))
	if mimeType == "" || mimeType == "application/octet-stream" {
		if header != nil {
			if ct := header.Header.Get("Content-Type"); ct != "" {
				mimeType = ct
			}
		}
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	reqID := strings.TrimSpace(r.FormValue("requestId"))
	if reqID == "" {
		reqID = s.buildUploadRequestID()
	}

	fileID := s.buildFileID()
	// 先把字节读进内存（给 OnPushed 用），再写 store
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, 500, result{Code: 500, Msg: "read: " + err.Error()})
		return
	}
	meta, err := s.cfg.Store.Put(s.cfg.UserID, chatID, fileID, strings.NewReader(string(data)), Meta{Name: name, MimeType: mimeType})
	if err != nil {
		writeJSON(w, 500, result{Code: 500, Msg: "store: " + err.Error()})
		return
	}
	if s.cfg.OnPushed != nil {
		go func() {
			if err := s.cfg.OnPushed(chatID, meta.Name, meta.MimeType, data); err != nil {
				// 不改 response；只记
				fmt.Fprintf(os.Stderr, "WARN httpapi.push.onpushed err=%v\n", err)
			}
		}()
	}

	// Phase 2：暂不实际下发到企微，等 Phase 4 接企微 adapter。这里只登记 store，返回 UploadResponse。
	urlPath := buildDownloadPath(s.cfg.UserID, chatID, fileID)
	writeJSON(w, 200, result{Code: 0, Msg: "成功", Data: uploadResponse{
		RequestID: reqID,
		ChatID:    chatID,
		Upload: uploadInfo{
			Name:      meta.Name,
			MimeType:  meta.MimeType,
			SizeBytes: meta.SizeBytes,
			URL:       urlPath,
		},
	}})
}

// ---- /api/download/** ----

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// ticket：优先 query，其次 Bearer header（平台 Go client 用 Bearer）
	var claims protocol.Claims
	var ok bool
	if tk := strings.TrimSpace(r.URL.Query().Get("ticket")); tk != "" {
		claims, ok = protocol.ValidateAny(tk)
	} else if bearer := extractBearer(r); bearer != "" {
		claims, ok = protocol.ValidateAny(bearer)
	}
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	p := r.URL.Path
	idx := strings.Index(p, "/download/")
	if idx < 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	objectPath := strings.TrimPrefix(p[idx+len("/download/"):], "/")
	if objectPath == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// 防越权：path 必须以 protocol.userId/ 开头
	if !strings.HasPrefix(objectPath, claims.UserID+"/") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 拆成 userId/chatId/fileId（fileId = 路径剩下的全部，chatId 不含 `/`）
	rest := strings.TrimPrefix(objectPath, claims.UserID+"/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	chatID, fileID := parts[0], parts[1]

	rc, meta, err := s.cfg.Store.Get(claims.UserID, chatID, fileID)
	if err != nil {
		if errors.Is(err, errNotFound) || strings.Contains(err.Error(), "not found") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", meta.MimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.SizeBytes, 10))
	w.Header().Set("Content-Disposition", buildContentDisposition(meta.Name))
	_, _ = io.Copy(w, rc)
}

// ---- helpers ----

var errNotFound = errors.New("not found")

func (s *Server) authenticate(r *http.Request) (protocol.Claims, bool) {
	tk := extractBearer(r)
	if tk == "" {
		tk = strings.TrimSpace(r.URL.Query().Get("ticket"))
	}
	if tk == "" {
		return protocol.Claims{}, false
	}
	// 对 /api/push：确保 ticket 的 channel/agentKey 和 bridge 配置一致
	c, ok := protocol.ValidateTicket(s.cfg.Channel, s.cfg.UserID, s.cfg.AgentKey, tk)
	if !ok {
		return protocol.Claims{}, false
	}
	return c, true
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func buildDownloadPath(userID, chatID, fileID string) string {
	// chatID 中含 `#`，URL 里 `#` 会被当成 fragment，必须转义
	return "/api/download/" + url.PathEscape(userID) + "/" + url.PathEscape(chatID) + "/" + url.PathEscape(fileID)
}

func buildContentDisposition(name string) string {
	if name == "" {
		name = "file"
	}
	// RFC5987: filename*=UTF-8''<percent-encoded>
	encoded := url.PathEscape(name)
	return fmt.Sprintf("attachment; filename*=UTF-8''%s", encoded)
}

var rng = rand.New(rand.NewSource(time.Now().UnixNano()))
var rngMu sync.Mutex

func (s *Server) buildUploadRequestID() string {
	rngMu.Lock()
	defer rngMu.Unlock()
	return fmt.Sprintf("upload_%d_%06x", time.Now().UnixMilli(), rng.Int31n(0x1000000))
}

func (s *Server) buildFileID() string {
	rngMu.Lock()
	defer rngMu.Unlock()
	return fmt.Sprintf("f_%d_%06x", time.Now().UnixNano(), rng.Int31n(0x1000000))
}

// path helper used during review
var _ = path.Base
