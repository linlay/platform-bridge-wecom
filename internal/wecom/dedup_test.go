package wecom

import (
	"testing"
	"time"
)

func TestSeenFirstTime(t *testing.T) {
	c := NewDedup(10 * time.Minute)
	if c.Seen("default", "req-1") {
		t.Fatal("first time should not be seen")
	}
	if !c.Seen("default", "req-1") {
		t.Fatal("second time should be seen")
	}
}

func TestSeenDifferentKeys(t *testing.T) {
	c := NewDedup(10 * time.Minute)
	_ = c.Seen("a", "r")
	if c.Seen("b", "r") {
		t.Fatal("different appKey same reqId should not collide")
	}
	if c.Seen("a", "r2") {
		t.Fatal("same appKey different reqId should not collide")
	}
}

func TestExpiry(t *testing.T) {
	c := NewDedup(10 * time.Millisecond)
	_ = c.Seen("a", "r")
	time.Sleep(30 * time.Millisecond)
	if c.Seen("a", "r") {
		t.Fatal("entry should have expired")
	}
}

func TestBlankInputsAreNotDeduped(t *testing.T) {
	// 空 reqId 不能全都认为是重复（Java 的语义也是 trim 后为空则放行）
	c := NewDedup(time.Minute)
	if c.Seen("a", "") {
		t.Fatal("blank reqId first call")
	}
	if c.Seen("a", "") {
		t.Fatal("blank reqId should never be deduped")
	}
}
