package protocol

import (
	"testing"
	"time"
)

func TestParseSingle(t *testing.T) {
	id := "wecom#single#abc123#1a"
	p, err := Parse(id)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.ChatType != "single" || p.SourceID != "abc123" || p.Seq != "1a" {
		t.Fatalf("got %+v", p)
	}
}

func TestParseGroup(t *testing.T) {
	// SourceID 贪婪到最后一段 base36 之前，允许内部 `#`
	id := "wecom#group#team#site-1#zzz999"
	p, err := Parse(id)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.ChatType != "group" || p.SourceID != "team#site-1" || p.Seq != "zzz999" {
		t.Fatalf("got %+v", p)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []string{
		"",
		"foo#single#x#1a",
		"wecom#invalid#x#1a",
		"wecom#single##1a",
		"wecom#single#x#UPPER", // base36 必须小写
		"wecom#single#x#",
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

// ResolveOrCreate 对同一 (appKey, chatType, sourceId) 必须返回同一 chatId，
// 跨消息复用——对齐 Java WecomSessionChatIdService.resolveOrCreate。
func TestResolveOrCreateReusesSameChatIDForSameSource(t *testing.T) {
	f := NewFormatter()
	now := time.Unix(1700000000, 0)

	a := f.ResolveOrCreate("default", "single", "user-1", now)
	b := f.ResolveOrCreate("default", "single", "user-1", now.Add(time.Hour))
	if a != b {
		t.Fatalf("same (app,type,source) must reuse chatId: %s vs %s", a, b)
	}

	// 不同 sourceId 应该产生不同 chatId
	c := f.ResolveOrCreate("default", "single", "user-2", now)
	if c == a {
		t.Fatalf("different sourceId must produce different chatId: %s", c)
	}

	// 不同 chatType 应该独立（同一用户私聊和群聊不是一个会话）
	d := f.ResolveOrCreate("default", "group", "user-1", now)
	if d == a {
		t.Fatalf("different chatType must produce different chatId: %s", d)
	}

	// 空 appKey 归一化为 default，应与显式 default 同组
	e := f.ResolveOrCreate("", "single", "user-1", now)
	if e != a {
		t.Fatalf("empty appKey should normalize to default: %s vs %s", e, a)
	}
}

// 多 bot 路由的核心：ResolveOrCreate 时记录 chatId → appKey，后续可反查。
func TestResolveOrCreateRegistersOwnerForReverseLookup(t *testing.T) {
	f := NewFormatter()
	now := time.Unix(1700000000, 0)

	idA := f.ResolveOrCreate("bot-aaa", "single", "user-1", now)
	idB := f.ResolveOrCreate("bot-bbb", "single", "user-1", now)
	if idA == idB {
		t.Fatalf("different appKeys must produce different chatIds: %s vs %s", idA, idB)
	}

	if got, ok := f.OwnerAppKey(idA); !ok || got != "bot-aaa" {
		t.Fatalf("OwnerAppKey(idA) = (%q, %v), want (bot-aaa, true)", got, ok)
	}
	if got, ok := f.OwnerAppKey(idB); !ok || got != "bot-bbb" {
		t.Fatalf("OwnerAppKey(idB) = (%q, %v), want (bot-bbb, true)", got, ok)
	}

	if _, ok := f.OwnerAppKey("wecom#single#never-seen#xyz"); ok {
		t.Fatalf("unknown chatId must not resolve")
	}
}

func TestFormatMonotonic(t *testing.T) {
	f := NewFormatter()
	a := f.Format("single", "abc", time.Unix(1700000000, 0))
	b := f.Format("single", "abc", time.Unix(1700000000, 0)) // 同秒，seq 递增
	c := f.Format("single", "abc", time.Unix(1700000001, 0)) // 下一秒

	pa, _ := Parse(a)
	pb, _ := Parse(b)
	pc, _ := Parse(c)
	if pa.Seq >= pb.Seq {
		t.Fatalf("seq must be monotonic within same second: %s vs %s", pa.Seq, pb.Seq)
	}
	if pb.Seq >= pc.Seq {
		t.Fatalf("seq must be monotonic across seconds: %s vs %s", pb.Seq, pc.Seq)
	}
	if pa.ChatType != "single" || pa.SourceID != "abc" {
		t.Fatalf("format produced wrong prefix: %+v", pa)
	}
}
