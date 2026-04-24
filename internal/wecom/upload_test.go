package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"


	gws "github.com/gorilla/websocket"
)

// mockBotWithUpload 扩展 mockBot 支持上传三段握手。
type mockBotWithUpload struct {
	mu       sync.Mutex
	chunks   map[string][][]byte // upload_id → assembled chunks
	upgrader gws.Upgrader
	sends    []json.RawMessage
}

func newUploadBot(t *testing.T) (*mockBotWithUpload, *httptest.Server) {
	t.Helper()
	b := &mockBotWithUpload{
		chunks:   make(map[string][][]byte),
		upgrader: gws.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := b.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go b.loop(conn)
	}))
	t.Cleanup(ts.Close)
	return b, ts
}

func (b *mockBotWithUpload) loop(conn *gws.Conn) {
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env struct {
			Cmd  string `json:"cmd"`
			Body map[string]any `json:"body"`
		}
		_ = json.Unmarshal(raw, &env)
		switch env.Cmd {
		case CmdSubscribe:
			_ = conn.WriteJSON(Ack{Cmd: env.Cmd, ErrCode: 0, ErrMsg: "ok"})
		case CmdPing:
			_ = conn.WriteJSON(Ack{Cmd: env.Cmd, ErrCode: 0, ErrMsg: "ok"})
		case CmdUploadMediaInit:
			upID := "UP_1"
			b.mu.Lock()
			b.chunks[upID] = nil
			total, _ := env.Body["total_chunks"].(float64)
			b.chunks[upID] = make([][]byte, int(total))
			b.mu.Unlock()
			_ = conn.WriteJSON(map[string]any{
				"cmd":     env.Cmd,
				"errcode": 0,
				"body":    map[string]any{"upload_id": upID},
			})
		case CmdUploadMediaChunk:
			upID, _ := env.Body["upload_id"].(string)
			idx, _ := env.Body["chunk_index"].(float64)
			data, _ := env.Body["base64_data"].(string)
			decoded, _ := base64.StdEncoding.DecodeString(data)
			b.mu.Lock()
			b.chunks[upID][int(idx)] = decoded
			b.mu.Unlock()
			_ = conn.WriteJSON(Ack{Cmd: env.Cmd, ErrCode: 0, ErrMsg: "ok"})
		case CmdUploadMediaFinish:
			upID, _ := env.Body["upload_id"].(string)
			_ = upID
			_ = conn.WriteJSON(map[string]any{
				"cmd":     env.Cmd,
				"errcode": 0,
				"body":    map[string]any{"media_id": "MID_42"},
			})
		case CmdSend:
			b.mu.Lock()
			b.sends = append(b.sends, append(json.RawMessage(nil), raw...))
			b.mu.Unlock()
			_ = conn.WriteJSON(Ack{Cmd: env.Cmd, ErrCode: 0, ErrMsg: "ok"})
		}
	}
}

func (b *mockBotWithUpload) assembled(upID string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []byte
	for _, c := range b.chunks[upID] {
		out = append(out, c...)
	}
	return out
}

func TestUploadRoundTrip(t *testing.T) {
	bot, ts := newUploadBot(t)
	ready := make(chan struct{})
	c := NewClient(ClientConfig{
		URL:               "ws" + strings.TrimPrefix(ts.URL, "http"),
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

	// 构造 1MB+100B → 两块
	data := make([]byte, 512*1024+100)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sumIn := md5.Sum(data)
	mediaID, err := c.UploadMedia(MsgTypeImage, "x.png", data)
	if err != nil {
		t.Fatalf("UploadMedia: %v", err)
	}
	if mediaID != "MID_42" {
		t.Fatalf("mediaID: %s", mediaID)
	}
	// 重组 bot 收到的 chunks 应等于原字节
	got := bot.assembled("UP_1")
	if len(got) != len(data) {
		t.Fatalf("assembled len=%d want=%d", len(got), len(data))
	}
	sumOut := md5.Sum(got)
	if hex.EncodeToString(sumIn[:]) != hex.EncodeToString(sumOut[:]) {
		t.Fatalf("md5 mismatch")
	}
}

func TestSendImageFrameGoesOut(t *testing.T) {
	bot, ts := newUploadBot(t)
	ready := make(chan struct{})
	c := NewClient(ClientConfig{
		URL:               "ws" + strings.TrimPrefix(ts.URL, "http"),
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
	if err := c.SendImage("wmUSER", "userid", "MID_1"); err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 send, got %d", len(bot.sends))
	}
	s := string(bot.sends[0])
	for _, w := range []string{`"msgtype":"image"`, `"media_id":"MID_1"`, `"userid":"wmUSER"`} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
}
