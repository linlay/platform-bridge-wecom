// Package frames 定义 bridge ↔ 企业微信 Bot 平台之间的 WebSocket 帧协议。
//
// 严格对齐 aiagent-gateway `gateway-channel` 下的 WecomWebSocketClient.java：
//   - 订阅：{"cmd":"aibot_subscribe","headers":{"req_id":"aibot_subscribe_<uuid>"},"body":{"bot_id":...,"secret":...}}
//   - 心跳：{"cmd":"ping","headers":{"req_id":"ping_<uuid>"}}
//   - 入站：{"cmd":"aibot_msg_callback","headers":{"req_id":...},"body":{...}}
//   - 回复：{"cmd":"aibot_respond_msg","headers":{"req_id":<同源>},"body":{msgtype:"text"|"stream", ...}}
//   - ACK： {"cmd":<原 cmd>,"errcode":0,"errmsg":"ok"}
package wecom

const (
	CmdSubscribe         = "aibot_subscribe"
	CmdPing              = "ping"
	CmdCallback          = "aibot_msg_callback"
	CmdRespond           = "aibot_respond_msg"
	CmdSend              = "aibot_send_msg"
	CmdUploadMediaInit   = "aibot_upload_media_init"
	CmdUploadMediaChunk  = "aibot_upload_media_chunk"
	CmdUploadMediaFinish = "aibot_upload_media_finish"
	MsgTypeText          = "text"
	MsgTypeStream        = "stream"
	MsgTypeImage         = "image"
	MsgTypeFile          = "file"
	MsgTypeVoice         = "voice"
	MsgTypeMarkdown      = "markdown"
)

type headers struct {
	ReqID string `json:"req_id"`
}

// Subscribe 订阅帧
type Subscribe struct {
	Cmd     string          `json:"cmd"`
	Headers headers         `json:"headers"`
	Body    SubscribeBody   `json:"body"`
}

type SubscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

func NewSubscribe(botID, secret, uuid string) Subscribe {
	return Subscribe{
		Cmd:     CmdSubscribe,
		Headers: headers{ReqID: "aibot_subscribe_" + uuid},
		Body:    SubscribeBody{BotID: botID, Secret: secret},
	}
}

// Ping 心跳帧（无 body）
type Ping struct {
	Cmd     string  `json:"cmd"`
	Headers headers `json:"headers"`
}

func NewPing(uuid string) Ping {
	return Ping{Cmd: CmdPing, Headers: headers{ReqID: "ping_" + uuid}}
}

// ReplyText: aibot_respond_msg text
type ReplyText struct {
	Cmd     string        `json:"cmd"`
	Headers headers       `json:"headers"`
	Body    replyTextBody `json:"body"`
}

type replyTextBody struct {
	MsgType string      `json:"msgtype"`
	Text    textContent `json:"text"`
}

type textContent struct {
	Content string `json:"content"`
}

func NewReplyText(sourceReqID, content string) ReplyText {
	return ReplyText{
		Cmd:     CmdRespond,
		Headers: headers{ReqID: sourceReqID},
		Body:    replyTextBody{MsgType: MsgTypeText, Text: textContent{Content: content}},
	}
}

// ReplyStream: aibot_respond_msg stream
type ReplyStream struct {
	Cmd     string          `json:"cmd"`
	Headers headers         `json:"headers"`
	Body    replyStreamBody `json:"body"`
}

type replyStreamBody struct {
	MsgType string     `json:"msgtype"`
	Stream  streamData `json:"stream"`
}

type streamData struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Finish  bool   `json:"finish"`
}

func NewReplyStream(sourceReqID, streamID, content string, finish bool) ReplyStream {
	return ReplyStream{
		Cmd:     CmdRespond,
		Headers: headers{ReqID: sourceReqID},
		Body:    replyStreamBody{MsgType: MsgTypeStream, Stream: streamData{ID: streamID, Content: content, Finish: finish}},
	}
}

// Inbound 来自企业微信的所有帧的解码；通过 Cmd 分发。
type Inbound struct {
	Cmd     string      `json:"cmd"`
	Headers headers     `json:"headers"`
	Body    InboundBody `json:"body"`

	// ACK 字段（Cmd==aibot_subscribe / ping / aibot_respond_msg 的回包）
	ErrCode int    `json:"errcode,omitempty"`
	ErrMsg  string `json:"errmsg,omitempty"`

	// AppKey 由 wecom.Client 在 OnMessage dispatch 前写入，标识消息归属的 bot。
	// bridge 多 bot 路由靠它做 chatId 和入站消息的 owner 绑定。JSON 不序列化。
	AppKey string `json:"-"`
}

type InboundBody struct {
	MsgType        string `json:"msgtype"`
	MsgID          string `json:"msgid"`
	MsgTime        int64  `json:"msgtime"`
	ChatID         string `json:"chatid"`
	ConversationID string `json:"conversation_id"`
	ExternalUserID string `json:"external_userid"`
	UserID         string `json:"userid"` // fallback 链里用
	From           struct {
		UserID string `json:"userid"`
	} `json:"from"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	Quote struct {
		MsgType string `json:"msgtype"`
		Text    struct {
			Content string `json:"content"`
		} `json:"text"`
	} `json:"quote"`

	// 媒体负载（每类 msgtype 一个）
	Image *MediaPayload `json:"image,omitempty"`
	File  *MediaPayload `json:"file,omitempty"`
	Voice *VoicePayload `json:"voice,omitempty"`
}

// MediaPayload 覆盖 image/file 可能出现的各种字段别名（Java 对齐）。
type MediaPayload struct {
	URL         string `json:"url"`
	DownloadURL string `json:"download_url"`
	DownloadURL2 string `json:"downloadUrl"`

	AESKey  string `json:"aeskey"`
	AESKey2 string `json:"aes_key"`

	FileID  string `json:"fileid"`
	MediaID string `json:"media_id"`
	MediaID2 string `json:"mediaId"`

	Filename string `json:"filename"`
	Name     string `json:"name"`

	ContentType  string `json:"content_type"`
	ContentType2 string `json:"contentType"`
	MimeType     string `json:"mimetype"`
	MimeType2    string `json:"mimeType"`
}

// ResolveURL 返回首个非空的下载 URL。
func (m *MediaPayload) ResolveURL() string {
	return firstNonBlank(m.URL, m.DownloadURL, m.DownloadURL2)
}

// ResolveAESKey 返回首个非空的 AES key。
func (m *MediaPayload) ResolveAESKey() string {
	return firstNonBlank(m.AESKey, m.AESKey2)
}

// ResolveName 返回首个非空的文件名。
func (m *MediaPayload) ResolveName() string {
	return firstNonBlank(m.Filename, m.Name)
}

// ResolveMimeType 返回首个非空的 mime type。
func (m *MediaPayload) ResolveMimeType() string {
	return firstNonBlank(m.ContentType, m.ContentType2, m.MimeType, m.MimeType2)
}

// VoicePayload 语音消息：除了和 MediaPayload 一样的文件字段，还有识别文本。
type VoicePayload struct {
	MediaPayload
	RecognizedText  string `json:"recognized_text"`
	RecognizedText2 string `json:"recognizedText"`
	Recognition     string `json:"recognition"`
	Content         string `json:"content"`
	Text            string `json:"text"`
}

// ResolveText 返回首个非空的识别文本（wecom 后端做的 ASR）。
func (v *VoicePayload) ResolveText() string {
	return firstNonBlank(v.RecognizedText, v.RecognizedText2, v.Recognition, v.Content, v.Text)
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Scope 描述入站消息的会话归属。
type Scope struct {
	ChatType string // single | group
	SourceID string
}

// ResolveChatScope 对齐 WecomWebsocket.resolveChatScope：
// chatid / conversation_id 任一非空 → group；否则 single，SourceID 取
// external_userid → from.userid 顺序降级。
func (b InboundBody) ResolveChatScope() Scope {
	if b.ChatID != "" {
		return Scope{ChatType: "group", SourceID: b.ChatID}
	}
	if b.ConversationID != "" {
		return Scope{ChatType: "group", SourceID: b.ConversationID}
	}
	if b.ExternalUserID != "" {
		return Scope{ChatType: "single", SourceID: b.ExternalUserID}
	}
	return Scope{ChatType: "single", SourceID: b.From.UserID}
}

// Ack 是 bot server 回给我们的 cmd ACK 帧（subscribe/ping/respond 都用这种）。
type Ack struct {
	Cmd     string `json:"cmd"`
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// ---- Phase 4 出站：分块上传 + 主动发送 ----

type UploadInit struct {
	Cmd     string         `json:"cmd"`
	Headers headers        `json:"headers"`
	Body    uploadInitBody `json:"body"`
}

type uploadInitBody struct {
	Type        string `json:"type"`
	Filename    string `json:"filename"`
	TotalSize   int64  `json:"total_size"`
	TotalChunks int    `json:"total_chunks"`
	MD5         string `json:"md5"`
}

func NewUploadInit(uuid, mediaType, filename string, size int64, chunks int, md5hex string) UploadInit {
	return UploadInit{
		Cmd:     CmdUploadMediaInit,
		Headers: headers{ReqID: CmdUploadMediaInit + "_" + uuid},
		Body:    uploadInitBody{Type: mediaType, Filename: filename, TotalSize: size, TotalChunks: chunks, MD5: md5hex},
	}
}

type UploadChunk struct {
	Cmd     string          `json:"cmd"`
	Headers headers         `json:"headers"`
	Body    uploadChunkBody `json:"body"`
}

type uploadChunkBody struct {
	UploadID   string `json:"upload_id"`
	ChunkIndex int    `json:"chunk_index"`
	Base64Data string `json:"base64_data"`
}

func NewUploadChunk(uuid, uploadID string, index int, base64Data string) UploadChunk {
	return UploadChunk{
		Cmd:     CmdUploadMediaChunk,
		Headers: headers{ReqID: CmdUploadMediaChunk + "_" + uuid},
		Body:    uploadChunkBody{UploadID: uploadID, ChunkIndex: index, Base64Data: base64Data},
	}
}

type UploadFinish struct {
	Cmd     string          `json:"cmd"`
	Headers headers         `json:"headers"`
	Body    uploadFinishBody `json:"body"`
}

type uploadFinishBody struct {
	UploadID string `json:"upload_id"`
}

func NewUploadFinish(uuid, uploadID string) UploadFinish {
	return UploadFinish{
		Cmd:     CmdUploadMediaFinish,
		Headers: headers{ReqID: CmdUploadMediaFinish + "_" + uuid},
		Body:    uploadFinishBody{UploadID: uploadID},
	}
}

// AckRich 是带载荷的 ACK（upload init/finish 需要取 upload_id / media_id）。
type AckRich struct {
	Cmd     string `json:"cmd"`
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Body    struct {
		UploadID string `json:"upload_id"`
		MediaID  string `json:"media_id"`
	} `json:"body"`
}

// SendMedia 是 bridge 主动发送的媒体消息（和 reply-based aibot_respond_msg 不同，
// 不需要 sourceReqID，而是显式指定 receiver）。
type SendMedia struct {
	Cmd     string         `json:"cmd"`
	Headers headers        `json:"headers"`
	Body    map[string]any `json:"body"`
}

func NewSendImage(uuid, receiveID, receiveIDType, mediaID string) SendMedia {
	return newSendMedia(uuid, receiveID, receiveIDType, MsgTypeImage, mediaID)
}

func NewSendFile(uuid, receiveID, receiveIDType, mediaID string) SendMedia {
	return newSendMedia(uuid, receiveID, receiveIDType, MsgTypeFile, mediaID)
}

// NewSendMarkdown 走 aibot_send_msg 主动发 markdown 文本。
// 用于 chat.updated 这种主动推送场景——LLM 耗时较长，原 callback req_id 已失效，
// 无法走 aibot_respond_msg，必须通过主动发送到指定 receiveId。
func NewSendMarkdown(uuid, receiveID, receiveIDType, content string) SendMedia {
	body := map[string]any{
		"msgtype":        MsgTypeMarkdown,
		MsgTypeMarkdown:  map[string]any{"content": content},
	}
	switch receiveIDType {
	case "chatid":
		body["chatid"] = receiveID
	case "userid":
		body["userid"] = receiveID
	case "external_userid":
		body["external_userid"] = receiveID
	default:
		body["chatid"] = receiveID
	}
	return SendMedia{
		Cmd:     CmdSend,
		Headers: headers{ReqID: CmdSend + "_" + uuid},
		Body:    body,
	}
}

func newSendMedia(uuid, receiveID, receiveIDType, msgType, mediaID string) SendMedia {
	body := map[string]any{
		"msgtype": msgType,
		msgType:   map[string]any{"media_id": mediaID},
	}
	// Java 里的 receiveIdType 对应 body 里的字段名
	switch receiveIDType {
	case "chatid":
		body["chatid"] = receiveID
	case "userid":
		body["userid"] = receiveID
	case "external_userid":
		body["external_userid"] = receiveID
	default:
		body["chatid"] = receiveID
	}
	return SendMedia{
		Cmd:     CmdSend,
		Headers: headers{ReqID: CmdSend + "_" + uuid},
		Body:    body,
	}
}
