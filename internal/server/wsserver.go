// Package wsserver 实现 bridge 对 platform 暴露的反向 WebSocket 端点 /ws/agent。
//
// 语义对齐 aiagent-gateway DownstreamAgentPushWebSocketHandler：
//   - upgrade 前校验 ticket（Authorization: Bearer / query ticket / query token 三取一）
//   - agentKey / channel / userId（若提供）必须与 ticket 的 claims 一致
//   - 鉴权失败：先发 {"frame":"error","type":"unauthorized","code":401,"msg":"Invalid or expired protocol."}
//     再以 CloseStatus 1008 关闭
//   - 握手通过：立即发 push/connected 首帧（7 字段齐）
//   - 帧读循环：解码 Envelope 后透传给 OnFrame hook（Phase 3/4 接入业务逻辑）
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"agent-wecom-bridge/internal/diag"
	"agent-wecom-bridge/internal/protocol"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type WSConfig struct {
	Channel  string
	AgentKey string
	UserID   string

	// OnFrame 每次收到入站帧都会被调用；Phase 2 默认为 nil（只 log）。
	OnFrame func(sessionID string, env protocol.Envelope)
}

type WSServer struct {
	cfg      WSConfig
	upgrader websocket.Upgrader
	mu       sync.Mutex
	sessions map[string]*Session
}

type Session struct {
	ID       string
	UserID   string
	AgentKey string
	Channel  string
	conn     *websocket.Conn
	writeMu  sync.Mutex
}

// Send 串行化发送（websocket 要求同一 conn 只能一个 writer）。
func (s *Session) Send(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, b)
}

func NewWSServer(cfg WSConfig) *WSServer {
	return &WSServer{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true }, // 个人 bridge 不做 origin 校验
		},
		sessions: make(map[string]*Session),
	}
}

// Broadcast 向当前所有活跃 session 发一帧。Phase 3 个人场景下最多 1 个。
// 若当前无 session，返回 nil（静默丢弃，等 platform 连上）。
func (s *WSServer) Broadcast(v any) error {
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	if len(sessions) == 0 {
		diag.Debug("wsserver.broadcast.no_session")
		return nil
	}
	for _, sess := range sessions {
		if err := sess.Send(v); err != nil {
			diag.Warn("wsserver.broadcast.send_fail", "sessionId", sess.ID, "err", err)
		}
	}
	return nil
}

// HandleAgentWS 挂在 /ws/agent 上。
func (s *WSServer) HandleAgentWS(w http.ResponseWriter, r *http.Request) {
	// 1. 提取 token：Bearer header 优先，其次 ?ticket=，其次 ?token=
	token, userIDHint := s.extractCredentials(r)
	agentKey := strings.TrimSpace(r.URL.Query().Get("agentKey"))
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))

	// 2. 先 upgrade，再在 conn 上发错误帧 + 关闭（按 Java 语义）
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		diag.Warn("wsserver.upgrade.fail", "err", err)
		return
	}

	// 3. 校验
	var claims protocol.Claims
	var ok bool
	if token == "" || agentKey == "" || channel == "" {
		ok = false
	} else if userIDHint != "" {
		claims, ok = protocol.ValidateTicket(channel, userIDHint, agentKey, token)
	} else {
		claims, ok = protocol.ValidateToken(channel, agentKey, token)
	}
	if !ok {
		_ = writeWSJSON(conn, protocol.Unauthorized())
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unauthorized"),
			time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}

	// 4. 建 session，发 connected 首帧
	sess := &Session{
		ID:       uuid.NewString(),
		UserID:   claims.UserID,
		AgentKey: claims.AgentKey,
		Channel:  claims.Channel,
		conn:     conn,
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, sess.ID)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	_ = sess.Send(protocol.Connected(protocol.ConnectedData{
		SessionID:      sess.ID,
		UserID:         sess.UserID,
		AgentKey:       sess.AgentKey,
		Channel:        sess.Channel,
		Status:         "ACTIVE",
		TicketAccepted: true,
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
	}))
	diag.Info("wsserver.session.connected", "sessionId", sess.ID, "userId", sess.UserID)

	// 5. 帧读循环
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			diag.Info("wsserver.session.closed", "sessionId", sess.ID, "err", err)
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			diag.Warn("wsserver.frame.decode_fail", "sessionId", sess.ID, "err", err)
			continue
		}
		diag.Debug("wsserver.frame.in", "sessionId", sess.ID, "frame", env.Frame, "type", env.Type, "id", env.ID)
		if s.cfg.OnFrame != nil {
			s.cfg.OnFrame(sess.ID, env)
		}
	}
}

func (s *WSServer) extractCredentials(r *http.Request) (token, userIDHint string) {
	q := r.URL.Query()
	if t := strings.TrimSpace(q.Get("ticket")); t != "" {
		return t, strings.TrimSpace(q.Get("userId"))
	}
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):]), strings.TrimSpace(q.Get("userId"))
		}
	}
	if t := strings.TrimSpace(q.Get("token")); t != "" {
		return t, strings.TrimSpace(q.Get("userId"))
	}
	return "", ""
}

func writeWSJSON(c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(websocket.TextMessage, b)
}
