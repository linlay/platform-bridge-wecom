// Package bridge 粘合 wecom client ↔ platform ws server。
//
// 两条路径：
//  1. wecom 入站文本消息 → 生成 chatId → 去重 → registry 登记 → /api/query RequestFrame → platform
//  2. platform 推来 push/chat.updated → 按 chatId 反查回复目标 → wecom 发 markdown
//
// Phase 3 只处理 msgtype=text 的入站，非 text 静默丢弃；chat.updated 按 runId 去重。
package wecom

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"

	"agent-wecom-bridge/internal/protocol"
	"agent-wecom-bridge/internal/diag"
	"agent-wecom-bridge/internal/server"

	"github.com/google/uuid"
)

// WecomSender 抽象 wecom 客户端的出站能力，便于注入 fake 测试。
type WecomSender interface {
	SendText(sourceReqID, content string) error
	SendMarkdown(sourceReqID, streamID, content string, finish bool) error
	SendMarkdownPush(receiveID, receiveIDType, content string) error
	UploadMedia(mediaType, filename string, bytes []byte) (string, error)
	SendImage(receiveID, receiveIDType, mediaID string) error
	SendFile(receiveID, receiveIDType, mediaID string) error
}

// PlatformSender 抽象 platform WS 会话的出站能力。
type PlatformSender interface {
	SendFrame(v any) error
}

type BridgeConfig struct {
	AppKey   string
	Channel  string // e.g. "wecom:personal"
	AgentKey string

	// 入站媒体流：bridge 要本地存一份让 platform 拉 + 在 /api/upload 帧里给一个带 ticket 的 URL
	Store          MediaStore
	UserID         string // 存盘目录 + download URL 的 path 第一段
	DownloadTicket string // 附在 URL 查询串 ?ticket=

	Wecom    WecomSender
	Platform PlatformSender
	Registry *Registry
	Dedup    *Cache
	Stream   *StreamSender
}

// MediaStore 是 server.FileStore 的最小契约（便于注入 fake）。
type MediaStore interface {
	Put(userID, chatID, fileID string, r io.Reader, meta server.Meta) (server.Meta, error)
	Get(userID, chatID, fileID string) (io.ReadCloser, server.Meta, error)
}

type Bridge struct {
	cfg       BridgeConfig
	formatter *protocol.Formatter

	// chat.updated dedup：chatId → lastRunId
	pushDedup map[string]string
}

func NewBridge(cfg BridgeConfig) *Bridge {
	return &Bridge{cfg: cfg, formatter: protocol.NewFormatter(), pushDedup: map[string]string{}}
}

// SetWecom 在 bridge 创建之后、wecom.Client 初始化完成时注入。
// 两者相互引用（bridge 调 wecom 发 markdown，wecom 调 bridge OnMessage），
// 必须分两步注入避免循环构造。
func (b *Bridge) SetWecom(w WecomSender) { b.cfg.Wecom = w }

// SetStream 注入 StreamSender；main 在 wecom.Client 构造完成后再构造 StreamSender。
func (b *Bridge) SetStream(s *StreamSender) { b.cfg.Stream = s }

// HandleWecomMessage 处理 wecom 推来的帧（aibot_msg_callback）。
func (b *Bridge) HandleWecomMessage(in Inbound) {
	if in.Cmd != CmdCallback {
		return
	}
	if b.cfg.Dedup != nil && b.cfg.Dedup.Seen(b.cfg.AppKey, in.Headers.ReqID) {
		diag.Debug("bridge.wecom.dup", "reqId", in.Headers.ReqID)
		return
	}
	switch in.Body.MsgType {
	case MsgTypeText:
		b.handleText(in)
	case MsgTypeImage, MsgTypeFile:
		b.handleMedia(in)
	case MsgTypeVoice:
		b.handleVoice(in)
	default:
		diag.Debug("bridge.wecom.skip_msgtype", "msgtype", in.Body.MsgType)
	}
}

func (b *Bridge) handleText(in Inbound) {
	content := strings.TrimSpace(in.Body.Text.Content)
	if content == "" {
		return
	}
	b.forwardText(in, in.Body.Text.Content)
}

// handleVoice：wecom 给我们 recognized_text；如果没识别文本就回退到
// "Voice message content is empty" 的提示（对齐 Java）。
func (b *Bridge) handleVoice(in Inbound) {
	text := ""
	if in.Body.Voice != nil {
		text = in.Body.Voice.ResolveText()
	}
	if strings.TrimSpace(text) == "" {
		diag.Warn("bridge.wecom.voice.no_text")
		_ = b.cfg.Wecom.SendText(in.Headers.ReqID, "Voice message content is empty. Please try again.")
		return
	}
	b.forwardText(in, text)
}

// forwardText 把一段用户输入（text / voice recognized_text）包成 /api/query 发给 platform。
func (b *Bridge) forwardText(in Inbound, content string) {
	scope := in.Body.ResolveChatScope()

	// 发送者身份（platform payload.userId 用）
	userID := in.Body.ExternalUserID
	if userID == "" {
		userID = in.Body.From.UserID
	}

	// 回复目标：对齐 Java DownstreamAgentPushWebSocketHandler.java:266，
	// 不分 single / group，一律用 receiveIdType="chatid" + scope.SourceID。
	// 企微 aibot_send_msg 要求 body.chatid 字段，发成 userid 会被拒（errcode 86201）。
	receiveID := scope.SourceID
	receiveIDType := "chatid"

	chatID := b.formatter.Format(scope.ChatType, scope.SourceID, time.Now())
	b.cfg.Registry.Register(chatID, Target{
		AppKey:        b.cfg.AppKey,
		ReceiveID:     receiveID,
		ReceiveIDType: receiveIDType,
		SourceReqID:   in.Headers.ReqID,
	})

	reqID := uuid.NewString()
	// 登记流式 sender：后续从 platform 来的 stream 事件按 requestId（= stream frame.id）归到这个 run。
	if b.cfg.Stream != nil {
		b.cfg.Stream.Open(reqID, chatID, in.Headers.ReqID)
	}
	payload := map[string]any{
		"requestId":  reqID,
		"message":    content,
		"agentKey":   b.cfg.AgentKey,
		"chatId":     chatID,
		"runId":      uuid.NewString(),
		"userId":     userID,
		"references": []any{},
		"params": map[string]any{
			"channel":           b.cfg.Channel,
			"wecomAppKey":       b.cfg.AppKey,
			"userId":            userID,
			"sourceReqId":       in.Headers.ReqID,
			"sourceMsgId":       in.Body.MsgID,
			"wecomChatIdType":   scope.ChatType,
			"wecomChatSourceId": scope.SourceID,
			"sourceChatId":      receiveID,
			"receiveId":         receiveID,
			"receiveIdType":     receiveIDType,
			"targetId":          receiveID,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		diag.Error("bridge.wecom.payload_marshal", "err", err)
		return
	}
	req := protocol.Request{Type: "/api/query", ID: reqID, Payload: raw}
	if err := b.cfg.Platform.SendFrame(req); err != nil {
		diag.Warn("bridge.wecom.platform_send_fail", "err", err)
	}
}

// handleMedia 处理入站 image/file：下载解密 → 落 store → 推 /api/upload 帧给 platform。
func (b *Bridge) handleMedia(in Inbound) {
	payload := in.Body.File
	uploadType := "file"
	if in.Body.MsgType == MsgTypeImage {
		payload = in.Body.Image
		uploadType = "image"
	}
	if payload == nil {
		diag.Warn("bridge.wecom.media.no_payload", "msgtype", in.Body.MsgType)
		return
	}
	url := payload.ResolveURL()
	if url == "" {
		diag.Warn("bridge.wecom.media.no_url", "msgtype", in.Body.MsgType)
		return
	}
	if b.cfg.Store == nil {
		diag.Warn("bridge.wecom.media.no_store")
		return
	}

	fetched, err := Fetch(url, payload.ResolveAESKey())
	if err != nil {
		diag.Warn("bridge.wecom.media.fetch_fail", "err", err)
		return
	}

	name := payload.ResolveName()
	if name == "" {
		name = fetched.Filename
	}
	mimeType := payload.ResolveMimeType()
	if mimeType == "" {
		mimeType = fetched.ContentType
	}
	// image vs file 按 mimeType 兜底（Java 的规则）
	if uploadType == "file" && strings.HasPrefix(mimeType, "image/") {
		uploadType = "image"
	}

	scope := in.Body.ResolveChatScope()
	userSender := in.Body.ExternalUserID
	if userSender == "" {
		userSender = in.Body.From.UserID
	}
	// 回复目标：对齐 Java，不分 single / group 一律 receiveIdType="chatid"。
	receiveID := scope.SourceID
	receiveIDType := "chatid"

	chatID := b.formatter.Format(scope.ChatType, scope.SourceID, time.Now())
	b.cfg.Registry.Register(chatID, Target{
		AppKey:        b.cfg.AppKey,
		ReceiveID:     receiveID,
		ReceiveIDType: receiveIDType,
		SourceReqID:   in.Headers.ReqID,
	})

	fileID := "f_" + uuid.NewString()
	meta, err := b.cfg.Store.Put(b.cfg.UserID, chatID, fileID, bytes.NewReader(fetched.Bytes), server.Meta{Name: name, MimeType: mimeType})
	if err != nil {
		diag.Warn("bridge.wecom.media.store_put_fail", "err", err)
		return
	}

	reqID := "upload_" + uuid.NewString()
	if b.cfg.Stream != nil {
		b.cfg.Stream.Open(reqID, chatID, in.Headers.ReqID)
	}
	urlPath := buildDownloadURL(b.cfg.UserID, chatID, fileID, b.cfg.DownloadTicket)
	payloadJSON := map[string]any{
		"requestId": reqID,
		"chatId":    chatID,
		"upload": map[string]any{
			"type":      uploadType,
			"name":      meta.Name,
			"mimeType":  meta.MimeType,
			"sizeBytes": meta.SizeBytes,
			"sha256":    meta.SHA256,
			"url":       urlPath,
		},
	}
	raw, err := json.Marshal(payloadJSON)
	if err != nil {
		diag.Error("bridge.wecom.media.payload_marshal", "err", err)
		return
	}
	req := protocol.Request{Type: "/api/upload", ID: reqID, Payload: raw}
	if err := b.cfg.Platform.SendFrame(req); err != nil {
		diag.Warn("bridge.wecom.media.platform_send_fail", "err", err)
	}
}

// SendMediaToWecom 由 httpapi /api/push 回调：把 platform 推过来的文件交给企微。
func (b *Bridge) SendMediaToWecom(chatID, name, mimeType string, data []byte) error {
	if b.cfg.Wecom == nil {
		return nil // wecom 未启用，静默
	}
	target, ok := b.cfg.Registry.Lookup(chatID)
	if !ok {
		diag.Warn("bridge.platform.push.unknown_chat", "chatId", chatID)
		return nil
	}
	mediaType := "file"
	if strings.HasPrefix(mimeType, "image/") {
		mediaType = "image"
	}
	mediaID, err := b.cfg.Wecom.UploadMedia(mediaType, name, data)
	if err != nil {
		return err
	}
	if mediaType == "image" {
		return b.cfg.Wecom.SendImage(target.ReceiveID, target.ReceiveIDType, mediaID)
	}
	return b.cfg.Wecom.SendFile(target.ReceiveID, target.ReceiveIDType, mediaID)
}

func buildDownloadURL(userID, chatID, fileID, ticket string) string {
	u := "/api/download/" + url.PathEscape(userID) + "/" + url.PathEscape(chatID) + "/" + url.PathEscape(fileID)
	if ticket != "" {
		u += "?ticket=" + url.QueryEscape(ticket)
	}
	return u
}

// HandlePlatformFrame 处理 platform 发给 bridge 的帧；Phase 3 只处理 push/chat.updated。
func (b *Bridge) HandlePlatformFrame(sessionID string, env protocol.Envelope) {
	switch env.Frame {
	case protocol.FrameStream:
		b.handleStreamFrame(env)
	case protocol.FramePush:
		switch env.Type {
		case "chat.updated":
			b.handleChatUpdated(env)
			// 其他 push（run.finished / chat.unread 等）目前不需要特殊处理；
			// run 收尾走 stream 里的 run.complete 事件。
		}
	}
}

// handleStreamFrame 解码 platform 的流式事件并喂给 StreamSender。
func (b *Bridge) handleStreamFrame(env protocol.Envelope) {
	if b.cfg.Stream == nil || env.ID == "" {
		return
	}
	if len(env.Event) == 0 {
		// 终止帧（携带 reason/lastSeq，无 event 负载），sender 不需要额外动作。
		return
	}
	var evt struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(env.Event, &evt); err != nil {
		diag.Warn("bridge.platform.stream.decode", "err", err)
		return
	}
	// 事件负载字段是平铺的，用 map 再读一遍拿到具体字段（delta / contentId 等）。
	var payload map[string]any
	_ = json.Unmarshal(env.Event, &payload)
	b.cfg.Stream.HandleEvent(env.ID, evt.Type, payload)
}

// handleChatUpdated 对齐 Java DownstreamAgentPushWebSocketHandler.java:266：
// 当流式路径（StreamSender）已经把本轮 run 的内容发过了，chat.updated 就不再兜底，
// 避免重复消息。只有 StreamSender 未处理的 chatID（典型：跨 session 的定时推送）才落 aibot_send_msg。
func (b *Bridge) handleChatUpdated(env protocol.Envelope) {
	var data struct {
		ChatID         string `json:"chatId"`
		LastRunID      string `json:"lastRunId"`
		LastRunContent string `json:"lastRunContent"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		diag.Warn("bridge.platform.chat_updated.decode", "err", err)
		return
	}
	if data.ChatID == "" || data.LastRunContent == "" {
		return
	}
	if b.cfg.Stream != nil && b.cfg.Stream.HandledChat(data.ChatID) {
		diag.Debug("bridge.platform.chat_updated.stream_handled", "chatId", data.ChatID, "runId", data.LastRunID)
		b.cfg.Stream.ForgetChat(data.ChatID) // 本轮已处理，清除标记以便下一轮跨 session 推送仍能兜底
		b.pushDedup[data.ChatID] = data.LastRunID
		return
	}
	if last, ok := b.pushDedup[data.ChatID]; ok && last == data.LastRunID {
		diag.Debug("bridge.platform.chat_updated.dup", "chatId", data.ChatID, "runId", data.LastRunID)
		return
	}
	target, ok := b.cfg.Registry.Lookup(data.ChatID)
	if !ok {
		diag.Warn("bridge.platform.chat_updated.unknown_chat", "chatId", data.ChatID)
		return
	}
	if err := b.cfg.Wecom.SendMarkdownPush(target.ReceiveID, target.ReceiveIDType, data.LastRunContent); err != nil {
		diag.Warn("bridge.platform.chat_updated.wecom_send_fail", "err", err)
		return
	}
	b.pushDedup[data.ChatID] = data.LastRunID
}
