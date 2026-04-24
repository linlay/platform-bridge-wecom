package wecom

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeReplier struct {
	mu    sync.Mutex
	calls []replyCall
}

type replyCall struct {
	SourceReqID string
	StreamID    string
	Content     string
	Finish      bool
}

func (f *fakeReplier) SendMarkdown(sourceReqID, streamID, content string, finish bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, replyCall{sourceReqID, streamID, content, finish})
	return nil
}

func (f *fakeReplier) snapshot() []replyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]replyCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// 等待若干次 flush 或超时。避免 time.Sleep 魔法数字：轮询 snapshot 直到期望成立。
func waitForFlushes(t *testing.T, f *fakeReplier, want int, timeout time.Duration) []replyCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if snap := f.snapshot(); len(snap) >= want {
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	return f.snapshot()
}

// delta 200ms debounce 只发一次（多次 delta 合并）。
func TestStreamDeltasDebouncedIntoOneFlush(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.Open("run-1", "chat-1", "src-req")

	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "Hello "})
	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "world"})

	calls := waitForFlushes(t, f, 1, 500*time.Millisecond)
	if len(calls) != 1 {
		t.Fatalf("expected 1 flush, got %d: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.SourceReqID != "src-req" || got.Finish || got.Content != "Hello world" {
		t.Fatalf("flush payload: %+v", got)
	}
	if got.StreamID == "" {
		t.Fatal("streamId missing")
	}
}

// content.end 立刻 flush，不等 debounce；run.complete 以 finish=true 收尾。
func TestStreamContentEndThenRunComplete(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.Open("run-1", "chat-1", "src")

	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "hi"})
	s.HandleEvent("run-1", "content.end", nil)
	s.HandleEvent("run-1", "run.complete", nil)

	// 不等 debounce：content.end + run.complete 都是立即 flush
	snap := f.snapshot()
	if len(snap) < 2 {
		t.Fatalf("expected >=2 flushes, got %d: %+v", len(snap), snap)
	}
	// 最后一帧必须 finish=true
	last := snap[len(snap)-1]
	if !last.Finish {
		t.Fatalf("last flush should be finish=true, got %+v", last)
	}
	// 所有 flush 在同一 streamID
	for _, c := range snap {
		if c.StreamID != snap[0].StreamID {
			t.Fatalf("streamID rotated mid-phase: %+v", snap)
		}
	}
}

// reasoning 先到，再 content；合成 <think>...</think>\n\n{answer}。
func TestStreamReasoningPlusAnswerMergedMarkdown(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.Open("run-1", "chat-1", "src")

	s.HandleEvent("run-1", "reasoning.start", nil)
	s.HandleEvent("run-1", "reasoning.delta", map[string]any{"delta": "thinking"})
	s.HandleEvent("run-1", "content.start", nil)
	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "answer"})
	s.HandleEvent("run-1", "run.complete", nil)

	snap := f.snapshot()
	if len(snap) == 0 {
		t.Fatal("no flush")
	}
	last := snap[len(snap)-1]
	if !last.Finish {
		t.Fatalf("last flush not finish: %+v", last)
	}
	if !strings.Contains(last.Content, "<think>thinking</think>") || !strings.HasSuffix(last.Content, "answer") {
		t.Fatalf("markdown synthesis: %q", last.Content)
	}
}

// reasoning.start 出现在 answer 已经开始之后 → 旋转 phase，新 streamId。
func TestStreamPhaseRotatesOnReasoningAfterAnswer(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.Open("run-1", "chat-1", "src")

	// phase 1: answer a
	s.HandleEvent("run-1", "content.start", nil)
	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "a1"})
	s.HandleEvent("run-1", "content.end", nil)
	snap1 := f.snapshot()
	if len(snap1) == 0 {
		t.Fatal("phase1: no flush after content.end")
	}
	stream1 := snap1[len(snap1)-1].StreamID

	// phase 2: reasoning.start 触发旋转 → 老 phase finish=true + 新 phase 新 streamId
	s.HandleEvent("run-1", "reasoning.start", nil)
	s.HandleEvent("run-1", "reasoning.delta", map[string]any{"delta": "think2"})
	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "a2"})
	s.HandleEvent("run-1", "run.complete", nil)

	snap := f.snapshot()
	// 老 phase 至少有一帧 finish=true
	var finishedStream1 bool
	for _, c := range snap {
		if c.StreamID == stream1 && c.Finish {
			finishedStream1 = true
			break
		}
	}
	if !finishedStream1 {
		t.Fatalf("phase1 not closed with finish=true: %+v", snap)
	}
	// 最后必须是新 streamId
	last := snap[len(snap)-1]
	if last.StreamID == stream1 {
		t.Fatalf("phase2 should use new streamId; got same as phase1: %+v", snap)
	}
	if !last.Finish {
		t.Fatalf("phase2 last not finish: %+v", last)
	}
}

// HandledChat 在流式发出第一帧后为 true；ForgetChat 清理。
func TestStreamHandledChatAndForget(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.Open("run-1", "chat-1", "src")
	if s.HandledChat("chat-1") {
		t.Fatal("should be false before any flush")
	}
	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "x"})
	s.HandleEvent("run-1", "content.end", nil)
	if !s.HandledChat("chat-1") {
		t.Fatal("should be true after flush")
	}
	s.ForgetChat("chat-1")
	if s.HandledChat("chat-1") {
		t.Fatal("ForgetChat should clear handled flag")
	}
}

// unknown runId 的事件静默丢弃，不崩不调 replier。
func TestStreamUnknownRunIgnored(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.HandleEvent("nope", "content.delta", map[string]any{"delta": "x"})
	s.HandleEvent("nope", "run.complete", nil)
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("expected 0 calls, got %d", n)
	}
}

// Close 幂等且对未处理的 run 返回 false。
func TestStreamCloseIdempotent(t *testing.T) {
	f := &fakeReplier{}
	s := NewStreamSender(f)
	s.Open("run-1", "chat-1", "src")
	s.HandleEvent("run-1", "content.delta", map[string]any{"delta": "hi"})
	s.HandleEvent("run-1", "content.end", nil)

	if !s.Close("run-1") {
		t.Fatal("Close should return true for handled run")
	}
	if s.Close("run-1") {
		t.Fatal("second Close should return false (run already removed)")
	}
}
