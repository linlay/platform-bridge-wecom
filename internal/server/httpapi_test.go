package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent-wecom-bridge/internal/protocol"
)

func newServer(t *testing.T) (*Server, *FileStore) {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		Channel:  "wecom:personal",
		AgentKey: "personal",
		UserID:   "local",
		Store:    s,
	}), s
}

func issueToken(t *testing.T) string {
	t.Helper()
	tk, err := protocol.IssueTicket("wecom:personal", "local", "personal")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// POST /api/push 成功：multipart，Bearer ticket，预填字节后返回 UploadResponse 并落 store。
func TestPushSuccess(t *testing.T) {
	srv, fs := newServer(t)
	tk := issueToken(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chatId", "wecom#single#src#1a")
	_ = mw.WriteField("name", "report.pdf")
	_ = mw.WriteField("type", "application/pdf")
	_ = mw.WriteField("requestId", "req-123")
	fw, _ := mw.CreateFormFile("file", "report.pdf")
	_, _ = fw.Write([]byte("pdfbytes"))
	mw.Close()

	req := httptest.NewRequest("POST", "/api/push", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tk)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			RequestID string `json:"requestId"`
			ChatID    string `json:"chatId"`
			Upload    struct {
				Name      string `json:"name"`
				MimeType  string `json:"mimeType"`
				SizeBytes int64  `json:"sizeBytes"`
				URL       string `json:"url"`
			} `json:"upload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v, body=%s", err, rr.Body.String())
	}
	if resp.Code != 0 || resp.Data.RequestID != "req-123" || resp.Data.ChatID != "wecom#single#src#1a" {
		t.Fatalf("resp: %+v", resp)
	}
	if resp.Data.Upload.SizeBytes != 8 || resp.Data.Upload.MimeType != "application/pdf" {
		t.Fatalf("upload: %+v", resp.Data.Upload)
	}
	// url 必须是相对路径，chatId 的 `#` 必须 percent-encode 以便 URL 解析
	if !strings.HasPrefix(resp.Data.Upload.URL, "/api/download/local/wecom%23single%23src%231a/") {
		t.Fatalf("url format: %s", resp.Data.Upload.URL)
	}

	// 落盘成功
	fileID, _ := url.PathUnescape(lastSeg(resp.Data.Upload.URL))
	rc, _, err := fs.Get("local", "wecom#single#src#1a", fileID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "pdfbytes" {
		t.Fatalf("body mismatch: %q", b)
	}
}

func TestPushMissingAuth(t *testing.T) {
	srv, _ := newServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chatId", "wecom#single#src#1a")
	fw, _ := mw.CreateFormFile("file", "x.txt")
	_, _ = fw.Write([]byte("x"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/push", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestPushBadTicket(t *testing.T) {
	srv, _ := newServer(t)
	// ticket 是给另一个 channel 的，应被拒
	bad, _ := protocol.IssueTicket("wecom:other", "local", "personal")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chatId", "wecom#single#src#1a")
	fw, _ := mw.CreateFormFile("file", "x.txt")
	_, _ = fw.Write([]byte("x"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/push", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bad)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("want 401 on channel mismatch, got %d", rr.Code)
	}
}

func TestDownloadSuccess(t *testing.T) {
	srv, fs := newServer(t)
	_, _ = fs.Put("local", "chat-1", "f1", strings.NewReader("hello"),
		Meta{Name: "a.txt", MimeType: "text/plain"})

	tk := issueToken(t)
	req := httptest.NewRequest("GET", "/api/download/local/chat-1/f1?ticket="+tk, nil)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "hello" {
		t.Fatalf("body: %q", rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("ct: %s", rr.Header().Get("Content-Type"))
	}
	if cl := rr.Header().Get("Content-Length"); cl != "5" {
		t.Errorf("cl: %s", cl)
	}
	// RFC5987 filename* 编码
	cd := rr.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "filename*=UTF-8''a.txt") {
		t.Errorf("content-disposition: %s", cd)
	}
}

func TestDownloadBearerAlsoAccepted(t *testing.T) {
	srv, fs := newServer(t)
	_, _ = fs.Put("local", "chat-1", "f1", strings.NewReader("hi"), Meta{Name: "a.bin", MimeType: "application/octet-stream"})
	tk := issueToken(t)
	req := httptest.NewRequest("GET", "/api/download/local/chat-1/f1", nil)
	req.Header.Set("Authorization", "Bearer "+tk)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("bearer path status=%d", rr.Code)
	}
}

func TestDownloadPrefixMismatch(t *testing.T) {
	srv, fs := newServer(t)
	_, _ = fs.Put("other", "chat-1", "f1", strings.NewReader("hi"), Meta{Name: "a.txt", MimeType: "text/plain"})
	tk := issueToken(t)
	// path 第一段 "other" 不等于 protocol.sub "local" → 403
	req := httptest.NewRequest("GET", "/api/download/other/chat-1/f1?ticket="+tk, nil)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("want 403, got %d", rr.Code)
	}
}

func TestDownloadNotFound(t *testing.T) {
	srv, _ := newServer(t)
	tk := issueToken(t)
	req := httptest.NewRequest("GET", "/api/download/local/chat-1/missing?ticket="+tk, nil)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDownloadUnauth(t *testing.T) {
	srv, _ := newServer(t)
	req := httptest.NewRequest("GET", "/api/download/local/chat-1/f1", nil)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

// silence unused
var _ = http.StatusOK

func lastSeg(s string) string {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return s
	}
	return s[i+1:]
}
