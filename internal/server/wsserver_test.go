package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent-wecom-bridge/internal/protocol"

	"github.com/gorilla/websocket"
)

func startServer(t *testing.T) (*httptest.Server, *WSServer) {
	t.Helper()
	s := NewWSServer(WSConfig{
		Channel:  "wecom:personal",
		AgentKey: "personal",
		UserID:   "local",
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleAgentWS))
	t.Cleanup(ts.Close)
	return ts, s
}

func wsURL(base string, q url.Values) string {
	u, _ := url.Parse(base)
	u.Scheme = "ws"
	u.RawQuery = q.Encode()
	return u.String()
}

func dial(t *testing.T, base string, q url.Values, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial(wsURL(base, q), hdr)
}

// 合法握手 + 首帧必须是 push/connected 且 7 字段齐。
func TestHandshakeAndConnectedFrame(t *testing.T) {
	ts, _ := startServer(t)
	tk, _ := protocol.IssueTicket("wecom:personal", "local", "personal")

	q := url.Values{}
	q.Set("agentKey", "personal")
	q.Set("channel", "wecom:personal")
	conn, _, err := dial(t, ts.URL, q, http.Header{"Authorization": []string{"Bearer " + tk}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Frame != "push" || env.Type != "connected" {
		t.Fatalf("unexpected first frame: %s", msg)
	}
	var data protocol.ConnectedData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.UserID != "local" || data.AgentKey != "personal" || data.Channel != "wecom:personal" ||
		data.Status != "ACTIVE" || !data.TicketAccepted || data.SessionID == "" || data.Timestamp == "" {
		t.Fatalf("connected data: %+v", data)
	}
}

// 无 Authorization 和无 ticket query → 401 frame + close
func TestHandshakeRejectsNoAuth(t *testing.T) {
	ts, _ := startServer(t)
	q := url.Values{}
	q.Set("agentKey", "personal")
	q.Set("channel", "wecom:personal")
	conn, _, err := dial(t, ts.URL, q, nil)
	if err != nil {
		// 某些情况下 upgrade 本身会失败；可接受
		return
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, _ := conn.ReadMessage()
	if !strings.Contains(string(msg), `"unauthorized"`) {
		t.Errorf("expected unauthorized frame, got %s", msg)
	}
}

// query.agentKey 与 protocol.header.ak 不一致 → 401
func TestHandshakeAgentKeyMismatch(t *testing.T) {
	ts, _ := startServer(t)
	tk, _ := protocol.IssueTicket("wecom:personal", "local", "other-agent")
	q := url.Values{}
	q.Set("agentKey", "personal") // 不匹配 ticket
	q.Set("channel", "wecom:personal")
	conn, _, err := dial(t, ts.URL, q, http.Header{"Authorization": []string{"Bearer " + tk}})
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, _ := conn.ReadMessage()
	if !strings.Contains(string(msg), `"unauthorized"`) {
		t.Errorf("expected unauthorized, got %s", msg)
	}
}

// ticket 通过 query ?ticket=... 也应接受
func TestHandshakeTicketQuery(t *testing.T) {
	ts, _ := startServer(t)
	tk, _ := protocol.IssueTicket("wecom:personal", "local", "personal")
	q := url.Values{}
	q.Set("agentKey", "personal")
	q.Set("channel", "wecom:personal")
	q.Set("ticket", tk)
	q.Set("userId", "local")
	conn, _, err := dial(t, ts.URL, q, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(msg), `"connected"`) {
		t.Errorf("expected connected, got %s", msg)
	}
}

// Phase 2: 入站帧仅记录并转给 OnFrame hook；不回错误即可。
func TestInboundFrameDispatchedToHook(t *testing.T) {
	tk, _ := protocol.IssueTicket("wecom:personal", "local", "personal")
	got := make(chan protocol.Envelope, 1)
	s := NewWSServer(WSConfig{
		Channel:  "wecom:personal",
		AgentKey: "personal",
		UserID:   "local",
		OnFrame: func(_ string, env protocol.Envelope) {
			select {
			case got <- env:
			default:
			}
		},
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleAgentWS))
	defer ts.Close()

	q := url.Values{"agentKey": []string{"personal"}, "channel": []string{"wecom:personal"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, q), http.Header{"Authorization": []string{"Bearer " + tk}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// 读掉 connected
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = conn.ReadMessage()

	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"frame":"response","id":"x","code":0}`))
	select {
	case env := <-got:
		if env.Frame != "response" || env.ID != "x" {
			t.Fatalf("dispatched: %+v", env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame not dispatched")
	}
}
