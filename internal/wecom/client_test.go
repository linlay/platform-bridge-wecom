package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"


	gws "github.com/gorilla/websocket"
)

// ---------- 测试用 mock 企微 Bot server ----------

type mockBot struct {
	mu       sync.Mutex
	inbound  []json.RawMessage
	upgrader gws.Upgrader

	// 控制 subscribe 回包的 errcode
	subscribeErrCode int
	// 控制是否回 ping pong（false → 模拟丢包）
	ackPing bool
	// 手工向下游投递消息
	push chan any

	conns chan *gws.Conn
}

func newMockBot(t *testing.T) (*mockBot, *httptest.Server) {
	t.Helper()
	b := &mockBot{
		upgrader: gws.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }},
		ackPing:  true,
		push:     make(chan any, 8),
		conns:    make(chan *gws.Conn, 4),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := b.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b.conns <- conn
		b.loop(conn)
	}))
	t.Cleanup(ts.Close)
	return b, ts
}

func (b *mockBot) loop(conn *gws.Conn) {
	defer conn.Close()
	// writer：把 push 通道投递的东西直接 WriteJSON
	go func() {
		for m := range b.push {
			_ = conn.WriteJSON(m)
		}
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.inbound = append(b.inbound, append(json.RawMessage(nil), raw...))
		b.mu.Unlock()

		var env struct {
			Cmd     string `json:"cmd"`
			Headers struct {
				ReqID string `json:"req_id"`
			} `json:"headers"`
		}
		_ = json.Unmarshal(raw, &env)
		switch env.Cmd {
		case CmdSubscribe:
			_ = conn.WriteJSON(Ack{Cmd: CmdSubscribe, ErrCode: b.subscribeErrCode, ErrMsg: "ok"})
		case CmdPing:
			if b.ackPing {
				_ = conn.WriteJSON(Ack{Cmd: CmdPing, ErrCode: 0, ErrMsg: "ok"})
			}
		case CmdRespond:
			_ = conn.WriteJSON(Ack{Cmd: CmdRespond, ErrCode: 0, ErrMsg: "ok"})
		}
	}
}

func (b *mockBot) inboundFrames() []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]json.RawMessage(nil), b.inbound...)
}

func wsURLOf(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

// ---------- 测试 ----------

func TestSubscribeHappyPath(t *testing.T) {
	bot, ts := newMockBot(t)
	ready := make(chan struct{})
	c := NewClient(ClientConfig{
		URL:               wsURLOf(ts),
		BotID:             "bot-x",
		Secret:            "sec-x",
		HeartbeatInterval: time.Hour, // 测试里别发心跳
		ReconnectMin:      50 * time.Millisecond,
		ReconnectMax:      200 * time.Millisecond,
		OnReady:           func() { close(ready) },
		OnMessage:         func(_ Inbound) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe OnReady timeout")
	}
	// 第一帧必须是 subscribe
	all := bot.inboundFrames()
	if len(all) < 1 || !strings.Contains(string(all[0]), `"cmd":"aibot_subscribe"`) {
		t.Fatalf("first frame not subscribe: %s", all)
	}
	if !strings.Contains(string(all[0]), `"bot_id":"bot-x"`) || !strings.Contains(string(all[0]), `"secret":"sec-x"`) {
		t.Fatalf("subscribe body wrong: %s", all[0])
	}
}

func TestInboundCallbackDispatched(t *testing.T) {
	bot, ts := newMockBot(t)
	ready := make(chan struct{})
	got := make(chan Inbound, 1)
	c := NewClient(ClientConfig{
		URL:               wsURLOf(ts),
		BotID:             "b",
		Secret:            "s",
		HeartbeatInterval: time.Hour,
		ReconnectMin:      50 * time.Millisecond,
		ReconnectMax:      200 * time.Millisecond,
		OnReady:           func() { close(ready) },
		OnMessage:         func(in Inbound) { got <- in },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	<-ready

	bot.push <- map[string]any{
		"cmd":     "aibot_msg_callback",
		"headers": map[string]string{"req_id": "R-1"},
		"body": map[string]any{
			"msgtype":         "text",
			"external_userid": "wmABC",
			"text":            map[string]string{"content": "hello"},
		},
	}
	select {
	case in := <-got:
		if in.Body.Text.Content != "hello" || in.Headers.ReqID != "R-1" {
			t.Fatalf("dispatched: %+v", in)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not dispatched")
	}
}

func TestSendTextRoundTrip(t *testing.T) {
	bot, ts := newMockBot(t)
	ready := make(chan struct{})
	c := NewClient(ClientConfig{
		URL:               wsURLOf(ts),
		BotID:             "b",
		Secret:            "s",
		HeartbeatInterval: time.Hour,
		ReconnectMin:      50 * time.Millisecond,
		ReconnectMax:      200 * time.Millisecond,
		OnReady:           func() { close(ready) },
		OnMessage:         func(_ Inbound) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	<-ready
	if err := c.SendText("r-src", "hi-user"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	// 验证 bot 收到带同 req_id 的 aibot_respond_msg
	time.Sleep(50 * time.Millisecond)
	found := false
	for _, f := range bot.inboundFrames() {
		s := string(f)
		if strings.Contains(s, `"cmd":"aibot_respond_msg"`) && strings.Contains(s, `"req_id":"r-src"`) && strings.Contains(s, `"content":"hi-user"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("reply not received, got: %s", bot.inboundFrames())
	}
}

func TestHeartbeatPingSent(t *testing.T) {
	bot, ts := newMockBot(t)
	ready := make(chan struct{})
	c := NewClient(ClientConfig{
		URL:               wsURLOf(ts),
		BotID:             "b",
		Secret:            "s",
		HeartbeatInterval: 80 * time.Millisecond,
		ReconnectMin:      50 * time.Millisecond,
		ReconnectMax:      200 * time.Millisecond,
		OnReady:           func() { close(ready) },
		OnMessage:         func(_ Inbound) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	<-ready
	time.Sleep(250 * time.Millisecond) // 应当至少 2 个 ping 跑过
	pingCount := 0
	for _, f := range bot.inboundFrames() {
		if strings.Contains(string(f), `"cmd":"ping"`) {
			pingCount++
		}
	}
	if pingCount < 2 {
		t.Fatalf("expected at least 2 pings, got %d, frames=%v", pingCount, bot.inboundFrames())
	}
}

func TestHeartbeatTimeoutReconnects(t *testing.T) {
	bot, ts := newMockBot(t)
	bot.ackPing = false // 永不回 pong
	ready := make(chan struct{}, 4)
	c := NewClient(ClientConfig{
		URL:               wsURLOf(ts),
		BotID:             "b",
		Secret:            "s",
		HeartbeatInterval: 30 * time.Millisecond,
		PongTimeout:       30 * time.Millisecond,
		ReconnectMin:      20 * time.Millisecond,
		ReconnectMax:      100 * time.Millisecond,
		OnReady:           func() { ready <- struct{}{} },
		OnMessage:         func(_ Inbound) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	<-ready // 第一次 ready
	// 2 次 ping 不回 → 重连；第二次 connect 再发 subscribe → 第二次 ready
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect did not happen on heartbeat timeout")
	}
}

func TestSubscribeRejectSchedulesReconnect(t *testing.T) {
	bot, ts := newMockBot(t)
	bot.subscribeErrCode = 1001 // 任意非 0 → 断开重连
	c := NewClient(ClientConfig{
		URL:               wsURLOf(ts),
		BotID:             "b",
		Secret:            "s",
		HeartbeatInterval: time.Hour,
		ReconnectMin:      20 * time.Millisecond,
		ReconnectMax:      80 * time.Millisecond,
		OnReady:           func() {},
		OnMessage:         func(_ Inbound) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// 应当看到多次 subscribe 尝试（至少 2 次）
	time.Sleep(400 * time.Millisecond)
	cnt := 0
	for _, f := range bot.inboundFrames() {
		if strings.Contains(string(f), `"cmd":"aibot_subscribe"`) {
			cnt++
		}
	}
	if cnt < 2 {
		t.Fatalf("expected >=2 subscribe attempts on rejection, got %d", cnt)
	}
}
