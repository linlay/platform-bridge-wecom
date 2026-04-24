// StreamSender 对齐 aiagent-gateway 的 WecomMessageSender.java：
// 按 runId 维护 reasoning + answer 两个 phase buffer，合成 <think>...</think>\n\n{answer}
// 的 markdown，按 200ms debounce 或 content.end / run.complete 通过 aibot_respond_msg
// stream 模式增量回传 wecom。
//
// 关键点：
//   - 流式路径走 aibot_respond_msg，用入站 wecom callback 的 source req_id + 自生成的 streamId；
//     wecom 按 streamId 续写同一条消息气泡。
//   - reasoning.start 时若 answer 已开始 → 旋转 phase：老 phase finish=true 发出，新 phase 用新 streamId。
//   - run.complete / run.finished 必须 finish=true 收尾；若流尚未开始，fallback 单帧文本。
//   - 所有操作对单个 run 串行（每个 run 自己一把 mu），sender 整体用一把 map mu 保护 runs 表。
package wecom

import (
	"html"
	"strings"
	"sync"
	"time"

	"agent-wecom-bridge/internal/diag"

	"github.com/google/uuid"
)

const streamFlushInterval = 200 * time.Millisecond

// StreamReplier 是 StreamSender 对 wecom client 的最小依赖。
type StreamReplier interface {
	SendMarkdown(sourceReqID, streamID, content string, finish bool) error
}

// phase 表示一轮 reasoning+answer 的缓冲。phase 之间通过不同 streamId 区隔。
type phase struct {
	streamID         string
	reasoning        strings.Builder
	answer           strings.Builder
	reasoningStarted bool
	answerStarted    bool
	dirty            bool
	finished         bool
	lastSent         string
}

type runState struct {
	mu          sync.Mutex
	chatID      string
	sourceReqID string
	current     *phase
	closed      bool
	flushTimer  *time.Timer
}

type StreamSender struct {
	replier StreamReplier

	mu           sync.Mutex
	runs         map[string]*runState
	handledChats map[string]bool // chatID → 已通过流式发出过至少一帧，用于抑制 chat.updated 重发
}

// NewStreamSender 构造一个空 sender；replier 需在调用 HandleEvent 前注入。
func NewStreamSender(r StreamReplier) *StreamSender {
	return &StreamSender{replier: r, runs: map[string]*runState{}, handledChats: map[string]bool{}}
}

// HandledChat 返回 chatID 是否已由流式路径发过内容——bridge.HandlePlatformFrame 在
// 处理 push/chat.updated 时用它判断是否需要兜底发一帧 markdown。
func (s *StreamSender) HandledChat(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handledChats[chatID]
}

// ForgetChat 让 chat.updated 的兜底对某个 chatID 失效（比如单独的跨 session 推送已处理）。
func (s *StreamSender) ForgetChat(chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handledChats, chatID)
}

// Open 在 bridge 发出 /api/query 之前登记 run 的目标 chatId + wecom source req_id。
// 同一 runID 重复 Open 覆盖旧状态（理论上不应发生）。
func (s *StreamSender) Open(runID, chatID, sourceReqID string) {
	if runID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[runID] = &runState{
		chatID:      chatID,
		sourceReqID: sourceReqID,
		current:     newPhase(),
	}
}

// HandleEvent 消费一个 stream 事件（从 platform 发来的 StreamFrame.event）。
// 事件类型按 Java WecomMessageSender.processEvent 对齐。
func (s *StreamSender) HandleEvent(runID, eventType string, payload map[string]any) {
	st := s.getRun(runID)
	if st == nil {
		diag.Debug("wecom.stream.unknown_run", "runId", runID, "type", eventType)
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}

	switch eventType {
	case "reasoning.start":
		s.onReasoningStartLocked(runID, st)
	case "reasoning.delta":
		delta := stringField(payload, "delta")
		if delta == "" {
			return
		}
		st.current.reasoningStarted = true
		st.current.reasoning.WriteString(delta)
		st.current.dirty = true
		s.scheduleFlushLocked(runID, st)
	case "reasoning.end":
		// 不单独 flush；等后续 content.end 或 tick。
	case "content.start", "message.start":
		// buffer only
	case "content.delta", "message.delta":
		delta := stringField(payload, "delta")
		if delta == "" {
			return
		}
		st.current.answerStarted = true
		st.current.answer.WriteString(delta)
		st.current.dirty = true
		s.scheduleFlushLocked(runID, st)
	case "content.end", "message.end":
		s.cancelTimerLocked(st)
		s.flushLocked(runID, st, false)
	case "run.complete", "run.finished":
		s.cancelTimerLocked(st)
		s.flushLocked(runID, st, true)
		st.closed = true
	case "run.error":
		s.cancelTimerLocked(st)
		// 有内容就 flush 收尾，没有就静默（chat.updated 的 final 会兜底）
		if st.current.hasStreamContent() {
			s.flushLocked(runID, st, true)
		}
		st.closed = true
	}
}

// Close 对应 run.finished push；幂等。确保最后发一次 finish=true。
// 返回该 run 是否由 StreamSender 实际处理过（非空 phase 或已 closed），
// 供 bridge 判断 chat.updated 是否需要兜底。
func (s *StreamSender) Close(runID string) bool {
	st := s.takeRun(runID)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	handled := st.closed || (st.current != nil && st.current.hasStreamContent())
	if !st.closed {
		s.cancelTimerLocked(st)
		if st.current.hasStreamContent() {
			s.flushLocked(runID, st, true)
		}
		st.closed = true
	}
	return handled
}

// Handled 返回给定 runID 是否已由 sender 发出过至少一帧 stream 内容。
func (s *StreamSender) Handled(runID string) bool {
	s.mu.Lock()
	st, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed || (st.current != nil && st.current.lastSent != "")
}

func (s *StreamSender) getRun(runID string) *runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[runID]
}

func (s *StreamSender) takeRun(runID string) *runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.runs[runID]
	delete(s.runs, runID)
	return st
}

func (s *StreamSender) onReasoningStartLocked(runID string, st *runState) {
	// 若 answer 已开始，老 phase finish=true 发走，再开新 phase。
	if st.current.answerStarted {
		s.cancelTimerLocked(st)
		s.flushLocked(runID, st, true)
		st.current = newPhase()
	}
	st.current.reasoningStarted = true
}

// scheduleFlushLocked 启动一个 200ms 的节流 tick——和 Java 的 switchMap 不同，
// 这里**不在后续 delta 到达时重置计时器**，而是让第一个 delta 点起计时器，到期就 flush；
// 下一个 delta 会再开一个新 tick。这样快 LLM（token 间隔 << 200ms）也能保持 ~200ms
// 一帧的可见节奏，而不是等到首次 token 停顿才一次性吐整段。
func (s *StreamSender) scheduleFlushLocked(runID string, st *runState) {
	if st.flushTimer != nil {
		return // tick 已排队，等它自然到期
	}
	st.flushTimer = time.AfterFunc(streamFlushInterval, func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.closed || st.current == nil {
			return
		}
		st.flushTimer = nil
		s.flushLocked(runID, st, false)
	})
}

func (s *StreamSender) cancelTimerLocked(st *runState) {
	if st.flushTimer != nil {
		st.flushTimer.Stop()
		st.flushTimer = nil
	}
}

// flushLocked 在 st.mu 持有状态下调用。
func (s *StreamSender) flushLocked(runID string, st *runState, finish bool) {
	p := st.current
	if p == nil || p.finished {
		return
	}
	if !p.hasStreamContent() && !finish {
		return
	}
	// 空内容 + finish=true 时也跳过（避免发出空帧）
	if !p.hasStreamContent() && finish {
		p.finished = true
		return
	}
	content := buildMarkdown(p.reasoningStarted, p.reasoning.String(), p.answerStarted, p.answer.String())
	// dedupe：非 finish 时，内容没变就不重发
	if !finish && content == p.lastSent {
		p.dirty = false
		return
	}
	p.dirty = false
	p.lastSent = content
	if finish {
		p.finished = true
	}
	if err := s.replier.SendMarkdown(st.sourceReqID, p.streamID, content, finish); err != nil {
		diag.Warn("wecom.stream.flush_fail", "runId", runID, "streamId", p.streamID, "finish", finish, "err", err)
		return
	}
	if st.chatID != "" {
		s.mu.Lock()
		s.handledChats[st.chatID] = true
		s.mu.Unlock()
	}
}

func newPhase() *phase {
	return &phase{streamID: uuid.NewString()}
}

func (p *phase) hasStreamContent() bool {
	return p.reasoning.Len() > 0 || p.answer.Len() > 0
}

func buildMarkdown(reasoningStarted bool, reasoning string, answerStarted bool, answer string) string {
	var b strings.Builder
	if reasoningStarted {
		b.WriteString("<think>")
		if reasoning != "" {
			b.WriteString(html.EscapeString(reasoning))
		} else {
			b.WriteString("Thinking is being generated...")
		}
		b.WriteString("</think>")
	}
	if answerStarted {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(answer)
	}
	return b.String()
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
