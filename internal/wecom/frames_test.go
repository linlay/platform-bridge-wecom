package wecom

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSubscribeMarshal(t *testing.T) {
	b, _ := json.Marshal(NewSubscribe("bot-1", "sec", "req-x"))
	s := string(b)
	for _, w := range []string{`"cmd":"aibot_subscribe"`, `"req_id":"aibot_subscribe_req-x"`, `"bot_id":"bot-1"`, `"secret":"sec"`} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
}

func TestPingMarshal(t *testing.T) {
	b, _ := json.Marshal(NewPing("p1"))
	s := string(b)
	if !strings.Contains(s, `"cmd":"ping"`) || !strings.Contains(s, `"req_id":"ping_p1"`) {
		t.Errorf("ping: %s", s)
	}
	// 没有 body 字段
	if strings.Contains(s, `"body"`) {
		t.Errorf("ping must not carry body: %s", s)
	}
}

func TestReplyTextMarshal(t *testing.T) {
	b, _ := json.Marshal(NewReplyText("req-src", "hello"))
	s := string(b)
	for _, w := range []string{`"cmd":"aibot_respond_msg"`, `"req_id":"req-src"`, `"msgtype":"text"`, `"content":"hello"`} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
}

func TestReplyStreamMarshal(t *testing.T) {
	b, _ := json.Marshal(NewReplyStream("req-src", "s-uuid", "# Title\n\nbody", true))
	s := string(b)
	for _, w := range []string{`"msgtype":"stream"`, `"id":"s-uuid"`, `"finish":true`, `"# Title`} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
}

func TestDecodeInboundText(t *testing.T) {
	raw := []byte(`{
		"cmd":"aibot_msg_callback",
		"headers":{"req_id":"R-1"},
		"body":{
			"msgtype":"text",
			"msgid":"m-1",
			"msgtime":1700000000000,
			"external_userid":"wmXYZ",
			"from":{"userid":"inner-u"},
			"text":{"content":"hi"}
		}
	}`)
	var f Inbound
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Cmd != "aibot_msg_callback" || f.Headers.ReqID != "R-1" {
		t.Fatalf("%+v", f)
	}
	if f.Body.MsgType != "text" || f.Body.Text.Content != "hi" {
		t.Fatalf("body: %+v", f.Body)
	}
	if f.Body.ExternalUserID != "wmXYZ" || f.Body.From.UserID != "inner-u" {
		t.Fatalf("user ids: %+v", f.Body)
	}
}

func TestAckDecode(t *testing.T) {
	raw := []byte(`{"cmd":"aibot_subscribe","errcode":0,"errmsg":"ok"}`)
	var a Ack
	_ = json.Unmarshal(raw, &a)
	if a.Cmd != "aibot_subscribe" || a.ErrCode != 0 || a.ErrMsg != "ok" {
		t.Fatalf("ack: %+v", a)
	}
}

// ResolveChat: Java 的 resolveChatScope/resolveReplyTarget 对齐
func TestResolveChatScopeGroup(t *testing.T) {
	in := Inbound{}
	in.Body.ChatID = "grp-1"
	in.Body.ConversationID = "conv-1"
	scope := in.Body.ResolveChatScope()
	if scope.ChatType != "group" || scope.SourceID != "grp-1" {
		t.Fatalf("group priority: %+v", scope)
	}
}

func TestResolveChatScopeSingle(t *testing.T) {
	in := Inbound{}
	in.Body.ExternalUserID = "wmX"
	scope := in.Body.ResolveChatScope()
	if scope.ChatType != "single" || scope.SourceID != "wmX" {
		t.Fatalf("single: %+v", scope)
	}
}

func TestResolveChatScopeFallback(t *testing.T) {
	in := Inbound{}
	in.Body.From.UserID = "inner"
	scope := in.Body.ResolveChatScope()
	if scope.ChatType != "single" || scope.SourceID != "inner" {
		t.Fatalf("from.userid fallback: %+v", scope)
	}
}
