package wecom

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-wecom-bridge/internal/protocol"
)

type fakeWecom struct {
	mu           sync.Mutex
	text         []struct{ ReqID, Content string }
	markdown     []struct{ ReqID, StreamID, Content string; Finish bool }
	markdownPush []struct{ ReceiveID, ReceiveIDType, Content string }
}

func (f *fakeWecom) SendText(reqID, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = append(f.text, struct{ ReqID, Content string }{reqID, content})
	return nil
}

func (f *fakeWecom) SendMarkdown(reqID, streamID, content string, finish bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markdown = append(f.markdown, struct {
		ReqID, StreamID, Content string
		Finish                   bool
	}{reqID, streamID, content, finish})
	return nil
}

func (f *fakeWecom) SendMarkdownPush(receiveID, receiveIDType, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markdownPush = append(f.markdownPush, struct {
		ReceiveID, ReceiveIDType, Content string
	}{receiveID, receiveIDType, content})
	return nil
}

func (f *fakeWecom) UploadMedia(mediaType, filename string, data []byte) (string, error) {
	return "MID_FAKE", nil
}
func (f *fakeWecom) SendImage(rid, rt, mid string) error { return nil }
func (f *fakeWecom) SendFile(rid, rt, mid string) error  { return nil }

type fakePlatform struct {
	mu     sync.Mutex
	frames []any
}

func (f *fakePlatform) SendFrame(v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, v)
	return nil
}

func newBridge(t *testing.T) (*Bridge, *fakeWecom, *fakePlatform, *Registry) {
	t.Helper()
	reg, _ := OpenRegistry(filepath.Join(t.TempDir(), "reg.json"))
	fw := &fakeWecom{}
	fp := &fakePlatform{}
	b := NewBridge(BridgeConfig{
		Channel:  "wecom:personal",
		AgentKey: "personal",
		Platform: fp,
		Registry: reg,
		Dedup:    NewDedup(time.Minute),
	})
	b.SetWecom(fw)
	return b, fw, fp, reg
}

// 入站企微文本 → /api/query request frame + registry 登记
func TestHandleWecomText(t *testing.T) {
	b, _, fp, reg := newBridge(t)

	in := Inbound{}
	in.Cmd = CmdCallback
	in.Headers.ReqID = "R-1"
	in.Body.MsgType = "text"
	in.Body.MsgID = "m-1"
	in.Body.ExternalUserID = "wmUSER"
	in.Body.From.UserID = "inner-U"
	in.Body.Text.Content = "hello agent"

	b.HandleWecomMessage(in)

	if len(fp.frames) != 1 {
		t.Fatalf("expect 1 frame, got %d", len(fp.frames))
	}
	req, ok := fp.frames[0].(protocol.Request)
	if !ok {
		t.Fatalf("wrong frame type: %T", fp.frames[0])
	}
	if req.Type != "/api/query" {
		t.Fatalf("type=%s", req.Type)
	}
	var payload struct {
		RequestID string `json:"requestId"`
		Message   string `json:"message"`
		AgentKey  string `json:"agentKey"`
		ChatID    string `json:"chatId"`
		RunID     string `json:"runId"`
		UserID    string `json:"userId"`
		Params    struct {
			Channel          string `json:"channel"`
			WecomAppKey      string `json:"wecomAppKey"`
			UserID           string `json:"userId"`
			SourceReqID      string `json:"sourceReqId"`
			SourceMsgID      string `json:"sourceMsgId"`
			WecomChatIDType  string `json:"wecomChatIdType"`
			WecomChatSourceID string `json:"wecomChatSourceId"`
			ReceiveID        string `json:"receiveId"`
			ReceiveIDType    string `json:"receiveIdType"`
		} `json:"params"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if payload.Message != "hello agent" || payload.UserID != "wmUSER" {
		t.Fatalf("payload: %+v", payload)
	}
	if payload.AgentKey != "personal" || payload.Params.Channel != "wecom:personal" {
		t.Fatalf("agent/channel: %+v", payload)
	}
	if payload.Params.WecomChatIDType != "single" || payload.Params.WecomChatSourceID != "wmUSER" {
		t.Fatalf("chat scope: %+v", payload.Params)
	}
	if payload.Params.SourceReqID != "R-1" || payload.Params.SourceMsgID != "m-1" {
		t.Fatalf("source ids: %+v", payload.Params)
	}
	// 对齐 Java DownstreamAgentPushWebSocketHandler.java:266，单聊也用 chatid。
	if payload.Params.ReceiveIDType != "chatid" || payload.Params.ReceiveID != "wmUSER" {
		t.Fatalf("single → receiveIdType=chatid, receiveId=wmUSER; got %+v", payload.Params)
	}
	if !strings.HasPrefix(payload.ChatID, "wecom#single#wmUSER#") {
		t.Fatalf("chatId: %s", payload.ChatID)
	}
	if payload.RequestID == "" || payload.RunID == "" {
		t.Fatal("requestId/runId missing")
	}
	if req.ID != payload.RequestID {
		t.Fatal("frame.id must equal payload.requestId")
	}

	// registry 登记
	got, ok := reg.Lookup(payload.ChatID)
	if !ok || got.AppKey != "default" || got.ReceiveID != "wmUSER" || got.ReceiveIDType != "chatid" {
		t.Fatalf("registry: ok=%v target=%+v", ok, got)
	}
}

// 群聊
func TestHandleWecomGroup(t *testing.T) {
	b, _, fp, _ := newBridge(t)
	in := Inbound{}
	in.Cmd = CmdCallback
	in.Headers.ReqID = "R-2"
	in.Body.MsgType = "text"
	in.Body.ChatID = "grp-1"
	in.Body.From.UserID = "inner-U"
	in.Body.Text.Content = "group msg"
	b.HandleWecomMessage(in)

	req := fp.frames[0].(protocol.Request)
	var payload struct {
		Params struct {
			WecomChatIDType string `json:"wecomChatIdType"`
			ReceiveID       string `json:"receiveId"`
			ReceiveIDType   string `json:"receiveIdType"`
		} `json:"params"`
		ChatID string `json:"chatId"`
	}
	_ = json.Unmarshal(req.Payload, &payload)
	if payload.Params.WecomChatIDType != "group" || payload.Params.ReceiveID != "grp-1" || payload.Params.ReceiveIDType != "chatid" {
		t.Fatalf("group params: %+v", payload.Params)
	}
	if !strings.HasPrefix(payload.ChatID, "wecom#group#grp-1#") {
		t.Fatalf("chatId: %s", payload.ChatID)
	}
}

// 非 text 消息被忽略
func TestSkipsNonText(t *testing.T) {
	b, _, fp, _ := newBridge(t)
	in := Inbound{Cmd: CmdCallback}
	in.Headers.ReqID = "R"
	in.Body.MsgType = "image"
	b.HandleWecomMessage(in)
	if len(fp.frames) != 0 {
		t.Fatalf("expected no frame for image, got %d", len(fp.frames))
	}
}

// dedup：同 reqId 再来一次应静默丢弃
func TestDedup(t *testing.T) {
	b, _, fp, _ := newBridge(t)
	in := Inbound{Cmd: CmdCallback}
	in.Headers.ReqID = "DUP-1"
	in.Body.MsgType = "text"
	in.Body.ExternalUserID = "wm"
	in.Body.Text.Content = "a"
	b.HandleWecomMessage(in)
	b.HandleWecomMessage(in)
	if len(fp.frames) != 1 {
		t.Fatalf("dedup failed: got %d frames", len(fp.frames))
	}
}

// chat.updated push → 主动发 markdown（走 aibot_send_msg 到 receiveId）
func TestHandleChatUpdated(t *testing.T) {
	b, fw, _, reg := newBridge(t)
	reg.Register("wecom#single#wmU#1a", Target{AppKey: "default", ReceiveID: "wmU", ReceiveIDType: "userid"})

	env := protocol.Envelope{Frame: "push", Type: "chat.updated"}
	env.Data = json.RawMessage(`{"chatId":"wecom#single#wmU#1a","lastRunId":"run-1","lastRunContent":"## Hello\n\nfrom agent"}`)
	b.HandlePlatformFrame("sess", env)

	if len(fw.markdownPush) != 1 {
		t.Fatalf("expected 1 markdown push, got %d", len(fw.markdownPush))
	}
	m := fw.markdownPush[0]
	if m.Content != "## Hello\n\nfrom agent" || m.ReceiveID != "wmU" || m.ReceiveIDType != "userid" {
		t.Fatalf("markdownPush: %+v", m)
	}

	// 重复推送（同 runId）应被 dedup
	b.HandlePlatformFrame("sess", env)
	if len(fw.markdownPush) != 1 {
		t.Fatalf("chat.updated not deduped: %d", len(fw.markdownPush))
	}
}

// chat.updated 未知 chatId → 静默丢弃（无 panic）
func TestChatUpdatedUnknownChatId(t *testing.T) {
	b, fw, _, _ := newBridge(t)
	env := protocol.Envelope{Frame: "push", Type: "chat.updated"}
	env.Data = json.RawMessage(`{"chatId":"wecom#single#unknown#zz","lastRunId":"r","lastRunContent":"x"}`)
	b.HandlePlatformFrame("sess", env)
	if len(fw.markdownPush) != 0 {
		t.Fatalf("unknown chatId should not send, got %d", len(fw.markdownPush))
	}
}

// 非 chat.updated 的 push 被忽略
func TestIgnoreOtherPush(t *testing.T) {
	b, fw, _, _ := newBridge(t)
	env := protocol.Envelope{Frame: "push", Type: "heartbeat"}
	b.HandlePlatformFrame("sess", env)
	if len(fw.markdownPush) != 0 || len(fw.text) != 0 {
		t.Fatalf("unexpected sends for non-chat.updated push")
	}
}
