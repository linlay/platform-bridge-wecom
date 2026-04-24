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
