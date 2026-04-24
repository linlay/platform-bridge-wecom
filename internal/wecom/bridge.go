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
	"sync"
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
	Channel  string // e.g. "wecom:personal"
	AgentKey string

	// 入站媒体流：bridge 要本地存一份让 platform 拉 + 在 /api/upload 帧里给一个带 ticket 的 URL
	Store          MediaStore
	UserID         string // 存盘目录 + download URL 的 path 第一段
	DownloadTicket string // 附在 URL 查询串 ?ticket=

	Platform PlatformSender
	Registry *Registry
	Dedup    *Cache
	// Streams 每个 bot 一个 StreamSender；SetStreamFor 注入。
	// 单 bot 场景下只有一条 "default" 条目。
	Streams map[string]*StreamSender
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

	// pendingRefs：chatId → 最近已 /api/upload 还没被下一次 /api/query 消费的 reference 列表。
	// 用途：用户在企微里先发文件再发问句，这两帧独立，platform 的 Runtime Context 不会自动
	// 把刚上传的文件列进 references；bridge 这边缓存一下，在下次 forwardText 带进去，
	// 让 agent 在 system prompt 的 Session.references 里直接看到文件名。
	pendingRefs   map[string][]map[string]any
	pendingRefsMu sync.Mutex

	// senders: appKey → 对应 bot 的 WecomSender。SetWecomFor / SetWecom 填充。
	// 出站路径按 chatId 反查 appKey，再用这张表找回对应 bot 的 client。
	sendersMu sync.RWMutex
	senders   map[string]WecomSender
}

func NewBridge(cfg BridgeConfig) *Bridge {
	if cfg.Streams == nil {
		cfg.Streams = map[string]*StreamSender{}
	}
	return &Bridge{
		cfg:         cfg,
		formatter:   protocol.NewFormatter(),
		pushDedup:   map[string]string{},
		pendingRefs: map[string][]map[string]any{},
		senders:     map[string]WecomSender{},
	}
}

// SetWecom 向后兼容：单 bot 场景下注入唯一 sender，绑定到 "default" appKey。
// wecom.Client 的 AppKey 留空时也 fallback 为 "default"，整条链路自洽。
func (b *Bridge) SetWecom(w WecomSender) { b.SetWecomFor("default", w) }

// SetWecomFor 多 bot 场景下按 appKey 注入对应 sender。appKey 为空回退到 "default"。
func (b *Bridge) SetWecomFor(appKey string, w WecomSender) {
	if appKey == "" {
		appKey = "default"
	}
	b.sendersMu.Lock()
	b.senders[appKey] = w
	b.sendersMu.Unlock()
}

// senderFor 按 appKey 查对应 bot 的 sender。找不到时若仅有一条条目兜底返回它（单 bot 兼容）。
func (b *Bridge) senderFor(appKey string) (WecomSender, bool) {
	b.sendersMu.RLock()
	defer b.sendersMu.RUnlock()
	if appKey != "" {
		if s, ok := b.senders[appKey]; ok {
			return s, true
		}
	}
	if len(b.senders) == 1 {
		for _, s := range b.senders {
			return s, true
		}
	}
	if s, ok := b.senders["default"]; ok {
		return s, true
	}
	return nil, false
}

// SetStream 向后兼容：单 bot 绑 "default"。
func (b *Bridge) SetStream(s *StreamSender) { b.SetStreamFor("default", s) }

// SetStreamFor 多 bot 按 appKey 注入各自的 StreamSender。
func (b *Bridge) SetStreamFor(appKey string, s *StreamSender) {
	if appKey == "" {
		appKey = "default"
	}
	if b.cfg.Streams == nil {
		b.cfg.Streams = map[string]*StreamSender{}
	}
	b.cfg.Streams[appKey] = s
}

// streamFor 按 appKey 查对应 StreamSender，找不到走单条兜底（兼容单 bot 部署）。
func (b *Bridge) streamFor(appKey string) *StreamSender {
	if appKey != "" {
		if s, ok := b.cfg.Streams[appKey]; ok {
			return s
		}
	}
	if len(b.cfg.Streams) == 1 {
		for _, s := range b.cfg.Streams {
			return s
		}
	}
	return b.cfg.Streams["default"]
}

// addPendingRef 在 handleMedia 把文件推给 platform 之后调用，记下一个待带入下次
// forwardText 的 reference。
func (b *Bridge) addPendingRef(chatID string, ref map[string]any) {
	if chatID == "" || ref == nil {
		return
	}
	b.pendingRefsMu.Lock()
	b.pendingRefs[chatID] = append(b.pendingRefs[chatID], ref)
	count := len(b.pendingRefs[chatID])
	b.pendingRefsMu.Unlock()
	diag.Info("bridge.wecom.refs.pending", "chatId", chatID, "name", ref["name"], "pending", count)
}

// takePendingRefs 在 forwardText 组装 /api/query payload 时调用，返回并清空当前 chatId 的
// pending references。消费即清空，避免下一条问句里重复带同一份文件。
func (b *Bridge) takePendingRefs(chatID string) []map[string]any {
	b.pendingRefsMu.Lock()
	defer b.pendingRefsMu.Unlock()
	refs := b.pendingRefs[chatID]
	delete(b.pendingRefs, chatID)
	return refs
}

// HandleWecomMessage 处理 wecom 推来的帧（aibot_msg_callback）。
func (b *Bridge) HandleWecomMessage(in Inbound) {
	if in.Cmd != CmdCallback {
		return
	}
	// Client 正常会在 OnMessage 前置 AppKey，这里兜底（单元测试直接构造 Inbound 可能漏填）。
	if in.AppKey == "" {
		in.AppKey = "default"
	}
	if b.cfg.Dedup != nil && b.cfg.Dedup.Seen(in.AppKey, in.Headers.ReqID) {
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
		if s, ok := b.senderFor(in.AppKey); ok {
			_ = s.SendText(in.Headers.ReqID, "Voice message content is empty. Please try again.")
		}
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

	chatID := b.formatter.ResolveOrCreate(in.AppKey, scope.ChatType, scope.SourceID, time.Now())
	diag.Info("bridge.wecom.forward", "chatId", chatID, "appKey", in.AppKey, "msgtype", in.Body.MsgType, "sourceReqId", in.Headers.ReqID)
	b.cfg.Registry.Register(chatID, Target{
		AppKey:        in.AppKey,
		ReceiveID:     receiveID,
		ReceiveIDType: receiveIDType,
		SourceReqID:   in.Headers.ReqID,
	})

	reqID := uuid.NewString()
	// 登记流式 sender：后续从 platform 来的 stream 事件按 requestId（= stream frame.id）归到这个 run。
	if stream := b.streamFor(in.AppKey); stream != nil {
		stream.Open(reqID, chatID, in.Headers.ReqID)
	}
	refs := b.takePendingRefs(chatID)
	if len(refs) == 0 {
		// json.Marshal 不能把 nil slice 写成 []，显式给空数组，保持对端类型。
		refs = []map[string]any{}
	} else {
		diag.Debug("bridge.wecom.forward.refs", "chatId", chatID, "count", len(refs))
	}
	payload := map[string]any{
		"requestId":  reqID,
		"message":    content,
		"agentKey":   b.cfg.AgentKey,
		"chatId":     chatID,
		"runId":      uuid.NewString(),
		"userId":     userID,
		"references": refs,
		"params": map[string]any{
			"channel":           b.cfg.Channel,
			"wecomAppKey":       in.AppKey,
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

	chatID := b.formatter.ResolveOrCreate(in.AppKey, scope.ChatType, scope.SourceID, time.Now())
	diag.Info("bridge.wecom.media", "chatId", chatID, "appKey", in.AppKey, "msgtype", in.Body.MsgType, "sourceReqId", in.Headers.ReqID)
	b.cfg.Registry.Register(chatID, Target{
		AppKey:        in.AppKey,
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
	if stream := b.streamFor(in.AppKey); stream != nil {
		stream.Open(reqID, chatID, in.Headers.ReqID)
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
		return
	}
	// 把这次 upload 记进 pendingRefs，下次 forwardText 会把它塞进 /api/query 的
	// references 字段，让 agent 的 Runtime Context.Session.references 能看到文件名。
	b.addPendingRef(chatID, map[string]any{
		"id":        fileID,
		"type":      uploadType, // "image" / "file"
		"name":      meta.Name,
		"mimeType":  meta.MimeType,
		"sizeBytes": meta.SizeBytes,
		"sha256":    meta.SHA256,
		"url":       urlPath,
	})
}

// SendMediaToWecom 由 httpapi /api/push 回调：把 platform 推过来的文件交给企微。
// 按 chatId 反查 target.AppKey，再用对应 bot 的 sender 上传 + 发送。
func (b *Bridge) SendMediaToWecom(chatID, name, mimeType string, data []byte) error {
	target, ok := b.cfg.Registry.Lookup(chatID)
	if !ok {
		diag.Warn("bridge.platform.push.unknown_chat", "chatId", chatID)
		return nil
	}
	sender, ok := b.senderFor(target.AppKey)
	if !ok {
		diag.Warn("bridge.platform.push.no_sender", "chatId", chatID, "appKey", target.AppKey)
		return nil // wecom 未启用或该 bot 未注册，静默
	}
	mediaType := "file"
	if strings.HasPrefix(mimeType, "image/") {
		mediaType = "image"
	}
	mediaID, err := sender.UploadMedia(mediaType, name, data)
	if err != nil {
		return err
	}
	if mediaType == "image" {
		return sender.SendImage(target.ReceiveID, target.ReceiveIDType, mediaID)
	}
	return sender.SendFile(target.ReceiveID, target.ReceiveIDType, mediaID)
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

// handleStreamFrame 解码 platform 的流式事件并喂给对应 bot 的 StreamSender。
// 路由：env.ID 是 requestId，StreamSender 内部已登记 chatId → appKey（Open 时写入）。
// 这里遍历所有 StreamSender 让其尝试处理（Open 过该 reqID 的 sender 会消费，其余 no-op）。
// 单 bot 场景下只有一条 StreamSender，性能等价于直接调用。
func (b *Bridge) handleStreamFrame(env protocol.Envelope) {
	if len(b.cfg.Streams) == 0 || env.ID == "" {
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
	var payload map[string]any
	_ = json.Unmarshal(env.Event, &payload)
	for _, s := range b.cfg.Streams {
		if s != nil {
			s.HandleEvent(env.ID, evt.Type, payload)
		}
	}
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
	target, ok := b.cfg.Registry.Lookup(data.ChatID)
	if !ok {
		diag.Warn("bridge.platform.chat_updated.unknown_chat", "chatId", data.ChatID)
		return
	}
	// stream 路径已处理就不做兜底（避免重复发）；按 target.AppKey 找对应 bot 的 StreamSender。
	if stream := b.streamFor(target.AppKey); stream != nil && stream.HandledChat(data.ChatID) {
		diag.Debug("bridge.platform.chat_updated.stream_handled", "chatId", data.ChatID, "runId", data.LastRunID)
		stream.ForgetChat(data.ChatID)
		b.pushDedup[data.ChatID] = data.LastRunID
		return
	}
	if last, ok := b.pushDedup[data.ChatID]; ok && last == data.LastRunID {
		diag.Debug("bridge.platform.chat_updated.dup", "chatId", data.ChatID, "runId", data.LastRunID)
		return
	}
	sender, ok := b.senderFor(target.AppKey)
	if !ok {
		diag.Warn("bridge.platform.chat_updated.no_sender", "chatId", data.ChatID, "appKey", target.AppKey)
		return
	}
	if err := sender.SendMarkdownPush(target.ReceiveID, target.ReceiveIDType, data.LastRunContent); err != nil {
		diag.Warn("bridge.platform.chat_updated.wecom_send_fail", "err", err)
		return
	}
	b.pushDedup[data.ChatID] = data.LastRunID
}
