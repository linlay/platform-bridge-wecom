package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// 出站帧：字段顺序和 Java Jackson 一致，"frame" 字段第一个。
// 这里做 string-contains 检查关键子串顺序，不依赖完整字符串匹配（避免 omitempty 差异）。

func TestRequestFrameMarshal(t *testing.T) {
	b, _ := json.Marshal(Request{Type: "/api/query", ID: "r1", Payload: json.RawMessage(`{"foo":"bar"}`)})
	s := string(b)
	for _, want := range []string{`"frame":"request"`, `"type":"/api/query"`, `"id":"r1"`, `"payload":{"foo":"bar"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if !strings.HasPrefix(s, `{"frame":"request"`) {
		t.Errorf("frame must be first field: %s", s)
	}
}

// Connected 首帧：7 字段齐全，顺序严格
func TestConnectedFrame(t *testing.T) {
	b, _ := json.Marshal(Connected(ConnectedData{
		SessionID:      "sess-1",
		UserID:         "u1",
		AgentKey:       "ak",
		Channel:        "wecom:xiaozhai",
		Status:         "ACTIVE",
		TicketAccepted: true,
		Timestamp:      "2026-04-23T10:00:00Z",
	}))
	s := string(b)
	// 首字段 frame=push, 然后 type=connected, 然后 data 里 7 个字段按顺序
	wantOrder := []string{
		`"frame":"push"`, `"type":"connected"`, `"data":{`,
		`"sessionId":"sess-1"`, `"userId":"u1"`, `"agentKey":"ak"`,
		`"channel":"wecom:xiaozhai"`, `"status":"ACTIVE"`,
		`"ticketAccepted":true`, `"timestamp":"2026-04-23T10:00:00Z"`,
	}
	last := -1
	for _, w := range wantOrder {
		idx := strings.Index(s, w)
		if idx < 0 {
			t.Errorf("missing %s in %s", w, s)
			continue
		}
		if idx <= last {
			t.Errorf("out-of-order %s at %d (last=%d) in %s", w, idx, last, s)
		}
		last = idx
	}
}

func TestUnauthorizedFrame(t *testing.T) {
	b, _ := json.Marshal(Unauthorized())
	s := string(b)
	want := `{"frame":"error","type":"unauthorized","code":401,"msg":"Invalid or expired ticket."}`
	if s != want {
		t.Errorf("want %s\n got %s", want, s)
	}
}

// 入站 Envelope：只按 frame 字段分发
func TestEnvelopeDecode(t *testing.T) {
	in := []byte(`{"frame":"response","id":"r1","type":"/api/query","code":0,"msg":"ok","data":{"x":1}}`)
	var env Envelope
	if err := json.Unmarshal(in, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Frame != "response" || env.ID != "r1" || env.Type != "/api/query" || env.Code != 0 {
		t.Errorf("envelope: %+v", env)
	}

	stream := []byte(`{"frame":"stream","id":"r1","reason":"done","lastSeq":5}`)
	var env2 Envelope
	_ = json.Unmarshal(stream, &env2)
	if env2.Frame != "stream" || env2.Reason != "done" || env2.LastSeq == nil || *env2.LastSeq != 5 {
		t.Errorf("stream envelope: %+v", env2)
	}
}

func TestFrameConstants(t *testing.T) {
	if FrameRequest != "request" || FrameResponse != "response" || FrameStream != "stream" ||
		FramePush != "push" || FrameError != "error" {
		t.Error("frame constants do not match Java WsFrameType")
	}
}
