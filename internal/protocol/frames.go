// Package frames 定义 platform ↔ bridge 的 WebSocket 帧协议。
//
// 严格对齐 aiagent-gateway/gateway-interface/protocol/websocket 下的 Java 类。
// 字段顺序按 Java 声明顺序（Jackson 序列化遵循此顺序），"frame" 字段恒定第一。
package protocol

import (
	"encoding/json"
)

// WsFrameType 常量（对齐 Java WsFrameType.java）
const (
	FrameRequest  = "request"
	FrameResponse = "response"
	FrameStream   = "stream"
	FramePush     = "push"
	FrameError    = "error"
)

// Request: bridge → platform 主动请求（/api/query, /api/upload 等）。
type Request struct {
	Frame   string          `json:"frame"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (r Request) MarshalJSON() ([]byte, error) {
	type alias Request
	r.Frame = FrameRequest
	return json.Marshal(alias(r))
}

// Response: 一次性响应（platform → bridge 或反向）。
type Response struct {
	Frame string `json:"frame"`
	Type  string `json:"type,omitempty"`
	ID    string `json:"id,omitempty"`
	Code  int    `json:"code"`
	Msg   string `json:"msg,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func (r Response) MarshalJSON() ([]byte, error) {
	type alias Response
	r.Frame = FrameResponse
	return json.Marshal(alias(r))
}

// Stream: 流式增量。
type Stream struct {
	Frame    string `json:"frame"`
	ID       string `json:"id"`
	StreamID string `json:"streamId,omitempty"`
	Event    any    `json:"event,omitempty"`
	Reason   string `json:"reason,omitempty"`
	LastSeq  *int   `json:"lastSeq,omitempty"`
}

func (s Stream) MarshalJSON() ([]byte, error) {
	type alias Stream
	s.Frame = FrameStream
	return json.Marshal(alias(s))
}

// Push: 主动推送（bridge → platform 或反向）。
type Push struct {
	Frame string `json:"frame"`
	Type  string `json:"type"`
	Data  any    `json:"data,omitempty"`
}

func (p Push) MarshalJSON() ([]byte, error) {
	type alias Push
	p.Frame = FramePush
	return json.Marshal(alias(p))
}

// Error: 错误帧。
type Error struct {
	Frame string `json:"frame"`
	Type  string `json:"type,omitempty"`
	ID    string `json:"id,omitempty"`
	Code  int    `json:"code"`
	Msg   string `json:"msg,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func (e Error) MarshalJSON() ([]byte, error) {
	type alias Error
	e.Frame = FrameError
	return json.Marshal(alias(e))
}

// ConnectedData 是握手成功后首帧 push/connected 的 data 部分。字段顺序固定。
type ConnectedData struct {
	SessionID      string `json:"sessionId"`
	UserID         string `json:"userId"`
	AgentKey       string `json:"agentKey"`
	Channel        string `json:"channel"`
	Status         string `json:"status"`
	TicketAccepted bool   `json:"ticketAccepted"`
	Timestamp      string `json:"timestamp"`
}

// Connected 构造首帧。
func Connected(data ConnectedData) Push {
	return Push{Type: "connected", Data: data}
}

// Unauthorized 是 401 error frame，严格对齐 Java DownstreamAgentPushWebSocketHandler:100-107。
func Unauthorized() Error {
	return Error{Type: "unauthorized", Code: 401, Msg: "Invalid or expired ticket."}
}

// Envelope 是入站帧的通用解码结构，只按 frame 字段分发，其余字段按需读取。
type Envelope struct {
	Frame    string          `json:"frame"`
	Type     string          `json:"type,omitempty"`
	ID       string          `json:"id,omitempty"`
	Code     int             `json:"code,omitempty"`
	Msg      string          `json:"msg,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Event    json.RawMessage `json:"event,omitempty"`
	StreamID string          `json:"streamId,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	LastSeq  *int            `json:"lastSeq,omitempty"`
}

// StreamEvent 解码 StreamFrame.event 字段。platform 把事件负载直接平铺到 event 对象里
// （不嵌 payload/data），所以 JSON 反序列化用 RawMessage 再按事件类型解析具体字段。
type StreamEvent struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	// 后续字段按 Type 动态读取：content.delta / reasoning.delta 有 delta；
	// content.start 有 contentId 等。调用方用 json.Unmarshal(raw) 或 map[string]any。
}
