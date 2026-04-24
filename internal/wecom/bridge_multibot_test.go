package wecom

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"agent-wecom-bridge/internal/protocol"
)

// 多 bot 路由：两个 bot 同机跑，入站分属不同 appKey，
// 出站（chat.updated）必须各自送回对应 bot 的 sender。
func TestMultiBotRoutesOutboundByAppKey(t *testing.T) {
	reg, _ := OpenRegistry(filepath.Join(t.TempDir(), "reg.json"))
	fp := &fakePlatform{}
	fwA := &fakeWecom{}
	fwB := &fakeWecom{}

	b := NewBridge(BridgeConfig{
		Channel:  "wecom:multi",
		AgentKey: "zenmi",
		Platform: fp,
		Registry: reg,
		Dedup:    NewDedup(time.Minute),
	})
	b.SetWecomFor("botA", fwA)
	b.SetWecomFor("botB", fwB)

	// Bot A 收到用户 userAAA 的消息
	inA := Inbound{
		Cmd:    CmdCallback,
		AppKey: "botA",
		Headers: headers{ReqID: "rA"},
		Body: InboundBody{
			MsgType: MsgTypeText,
			ChatID:  "user-aaa",
			Text:    struct{ Content string `json:"content"` }{Content: "hi from A"},
		},
	}
	b.HandleWecomMessage(inA)

	// Bot B 收到用户 userBBB 的消息
	inB := Inbound{
		Cmd:    CmdCallback,
		AppKey: "botB",
		Headers: headers{ReqID: "rB"},
		Body: InboundBody{
			MsgType: MsgTypeText,
			ChatID:  "user-bbb",
			Text:    struct{ Content string `json:"content"` }{Content: "hi from B"},
		},
	}
	b.HandleWecomMessage(inB)

	// 两帧 /api/query 都应该发到 platform
	if len(fp.frames) != 2 {
		t.Fatalf("want 2 platform frames (one per bot), got %d", len(fp.frames))
	}

	// 提取各自的 chatId
	chatIDs := make([]string, 0, 2)
	for _, f := range fp.frames {
		req, ok := f.(protocol.Request)
		if !ok {
			t.Fatalf("not a Request: %T", f)
		}
		var body struct {
			ChatID string `json:"chatId"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(req.Payload, &body)
		chatIDs = append(chatIDs, body.ChatID)
		// 验证 params.wecomAppKey 透传
		if body.Params["wecomAppKey"] != "botA" && body.Params["wecomAppKey"] != "botB" {
			t.Errorf("wecomAppKey not propagated: %v", body.Params["wecomAppKey"])
		}
	}

	// 两个 chatId 对应两个不同 appKey 的 Target
	tA, okA := reg.Lookup(chatIDs[0])
	tB, okB := reg.Lookup(chatIDs[1])
	if !okA || !okB {
		t.Fatalf("chatIds not registered")
	}
	if tA.AppKey == tB.AppKey {
		t.Fatalf("both chatIds mapped to same appKey: %s", tA.AppKey)
	}

	// 出站路径：用 chat.updated 把消息 push 回。应该走对应 bot 的 sender。
	for i, cid := range chatIDs {
		target, _ := reg.Lookup(cid)
		expectAppKey := target.AppKey

		data, _ := json.Marshal(map[string]any{
			"chatId":         cid,
			"lastRunId":      "run-" + string(rune('1'+i)),
			"lastRunContent": "reply-" + expectAppKey,
		})
		b.handleChatUpdated(protocol.Envelope{Data: data})
	}

	if len(fwA.markdownPush) != 1 {
		t.Errorf("botA markdownPush count: got %d want 1", len(fwA.markdownPush))
	}
	if len(fwB.markdownPush) != 1 {
		t.Errorf("botB markdownPush count: got %d want 1", len(fwB.markdownPush))
	}
	if len(fwA.markdownPush) == 1 && fwA.markdownPush[0].Content != "reply-botA" {
		t.Errorf("botA got wrong content: %q", fwA.markdownPush[0].Content)
	}
	if len(fwB.markdownPush) == 1 && fwB.markdownPush[0].Content != "reply-botB" {
		t.Errorf("botB got wrong content: %q", fwB.markdownPush[0].Content)
	}
}

// 单 bot 路径（SetWecom 不带 appKey）兼容：AppKey="" 的 Inbound 归到 "default"，
// 出站也能找到 sender。
func TestSingleBotCompatUsesDefaultAppKey(t *testing.T) {
	reg, _ := OpenRegistry(filepath.Join(t.TempDir(), "reg.json"))
	fp := &fakePlatform{}
	fw := &fakeWecom{}

	b := NewBridge(BridgeConfig{
		Channel:  "wecom:single",
		AgentKey: "zenmi",
		Platform: fp,
		Registry: reg,
		Dedup:    NewDedup(time.Minute),
	})
	b.SetWecom(fw) // 不显式 appKey → "default"

	in := Inbound{
		Cmd:     CmdCallback,
		// AppKey 故意留空，走 HandleWecomMessage 的兜底
		Headers: headers{ReqID: "rS"},
		Body: InboundBody{
			MsgType: MsgTypeText,
			ChatID:  "user-x",
			Text:    struct{ Content string `json:"content"` }{Content: "legacy hi"},
		},
	}
	b.HandleWecomMessage(in)

	if len(fp.frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(fp.frames))
	}
	req := fp.frames[0].(protocol.Request)
	var body struct {
		ChatID string `json:"chatId"`
	}
	_ = json.Unmarshal(req.Payload, &body)

	target, _ := reg.Lookup(body.ChatID)
	if target.AppKey != "default" {
		t.Fatalf("single bot fallback appKey = %q want default", target.AppKey)
	}

	// chat.updated 回包应找到 default sender
	data, _ := json.Marshal(map[string]any{
		"chatId":         body.ChatID,
		"lastRunId":      "run-1",
		"lastRunContent": "legacy reply",
	})
	b.handleChatUpdated(protocol.Envelope{Data: data})
	if len(fw.markdownPush) != 1 {
		t.Fatalf("legacy default sender not hit: %d pushes", len(fw.markdownPush))
	}
}
