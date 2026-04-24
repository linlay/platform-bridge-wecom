// Package client 是 bridge 连企业微信 Bot 平台的 WebSocket 客户端。
//
// 对齐 aiagent-gateway 的 WecomWebSocketClient.java：
//   - 连接后首帧发 aibot_subscribe（含 bot_id + secret），等待 errcode==0 ACK
//   - 订阅成功即调用 OnReady；同时启动心跳（每 HeartbeatInterval 一次 ping）
//   - 心跳 ACK 超时 2 次（PongTimeout 内 2 次）即重连
//   - 读循环：callback 分发给 OnMessage；subscribe/ping/respond 的 ACK 按 cmd 分派
//
// 重点：企微 Bot server 返回的 ACK 帧**只带 req_id，不带 cmd**（与 Java
// WecomWebSocketClient.handleIncomingText 中按 reqId 前缀分派一致）。因此读循环
// 在 env.Cmd 为空时，按 req_id 前缀反推出 cmd（aibot_subscribe_* / ping_* /
// aibot_upload_media_*_* / aibot_send_msg_*），再按 cmd 排 FIFO channel 做对账。
// aibot_respond_msg 的 ACK req_id 回显源 callback 的 req_id、没有固定前缀，作为
// fallback 分派到 CmdRespond 通道。SendText/SendMarkdown 在同一连接上仍需串行化。
package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-wecom-bridge/internal/diag"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
)

type ClientConfig struct {
	URL    string
	BotID  string
	Secret string
	// AppKey 是 bridge 内部给这个 bot 的路由标签，随每条 Inbound 传出。
	// 留空时 fallback 为 "default"（兼容 Phase 2 单 bot 场景）。
	AppKey string

	HeartbeatInterval time.Duration
	PongTimeout       time.Duration // ACK 超时；默认 5s
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
	HandshakeTimeout  time.Duration // 默认 10s
	SendTimeout       time.Duration // SendText/SendMarkdown 等 ACK 超时；默认 5s

	OnReady   func()
	OnMessage func(Inbound)
}

type Client struct {
	cfg ClientConfig

	mu      sync.Mutex
	conn    *gws.Conn
	writeMu sync.Mutex

	// respondMu 串行化 SendText/SendMarkdown（因为 ACK 回包不带 req_id）
	respondMu sync.Mutex

	acks     map[string]chan Ack     // cmd → FIFO ACK channel
	ackRichs map[string]chan AckRich // cmd → 带 body 的 ACK（upload init/finish）
	ackMu    sync.Mutex

	// uploadMu 串行化整个"init+chunks+finish"三段上传，防止并发上传互相踩
	uploadMu sync.Mutex

	missedPong atomic.Int32
}

func NewClient(cfg ClientConfig) *Client {
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 5 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.SendTimeout == 0 {
		cfg.SendTimeout = 5 * time.Second
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}
	c := &Client{cfg: cfg}
	c.resetAckChannels()
	return c
}

func (c *Client) resetAckChannels() {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	c.acks = map[string]chan Ack{
		CmdSubscribe:        make(chan Ack, 4),
		CmdPing:             make(chan Ack, 4),
		CmdRespond:          make(chan Ack, 8),
		CmdSend:             make(chan Ack, 8),
		CmdUploadMediaChunk: make(chan Ack, 8),
	}
	c.ackRichs = map[string]chan AckRich{
		CmdUploadMediaInit:   make(chan AckRich, 4),
		CmdUploadMediaFinish: make(chan AckRich, 4),
	}
}

func (c *Client) ackCh(cmd string) chan Ack {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	return c.acks[cmd]
}

func (c *Client) ackRichCh(cmd string) chan AckRich {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	return c.ackRichs[cmd]
}

// Run 在调用方 goroutine 里跑，直到 ctx 取消。
// AppKey 返回 client 绑定的 bot 路由标签；供 bridge 注册时反查用。
func (c *Client) AppKey() string {
	if c.cfg.AppKey == "" {
		return "default"
	}
	return c.cfg.AppKey
}

func (c *Client) Run(ctx context.Context) {
	backoff := c.cfg.ReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}
		c.resetAckChannels()
		if err := c.connectAndServe(ctx); err != nil {
			diag.Warn("wecom.client.loop", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		d := backoff
		if d > c.cfg.ReconnectMax {
			d = c.cfg.ReconnectMax
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
		backoff *= 2
		if backoff > c.cfg.ReconnectMax {
			backoff = c.cfg.ReconnectMax
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	dialer := &gws.Dialer{HandshakeTimeout: c.cfg.HandshakeTimeout}
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.HandshakeTimeout)
	defer cancel()
	conn, _, err := dialer.DialContext(dialCtx, c.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		_ = conn.Close()
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()

	// 启动读循环
	readCtx, readCancel := context.WithCancel(ctx)
	defer readCancel()
	readDone := make(chan error, 1)
	go func() { readDone <- c.readLoop(readCtx, conn) }()

	// subscribe
	subUUID := uuid.NewString()
	if err := c.writeJSON(NewSubscribe(c.cfg.BotID, c.cfg.Secret, subUUID)); err != nil {
		return fmt.Errorf("send subscribe: %w", err)
	}

	// 等 subscribe ACK
	subCh := c.ackCh(CmdSubscribe)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-readDone:
		return fmt.Errorf("read before subscribe: %w", err)
	case ack := <-subCh:
		if ack.ErrCode != 0 {
			return fmt.Errorf("subscribe rejected: errcode=%d msg=%s", ack.ErrCode, ack.ErrMsg)
		}
	case <-time.After(c.cfg.SendTimeout):
		return errors.New("subscribe ACK timeout")
	}

	diag.Info("wecom.client.subscribed", "bot", c.cfg.BotID)
	if c.cfg.OnReady != nil {
		c.cfg.OnReady()
	}

	// 启动心跳
	hbCtx, hbCancel := context.WithCancel(readCtx)
	defer hbCancel()
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); c.heartbeatLoop(hbCtx) }()

	err = <-readDone
	hbCancel()
	<-hbDone
	return err
}

func (c *Client) readLoop(ctx context.Context, conn *gws.Conn) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env Inbound
		if err := json.Unmarshal(raw, &env); err != nil {
			diag.Warn("wecom.client.decode_fail", "err", err, "raw", string(raw))
			continue
		}
		if env.Cmd == "" {
			env.Cmd = inferCmdFromReqID(env.Headers.ReqID)
		}
		switch env.Cmd {
		case CmdCallback:
			env.AppKey = c.cfg.AppKey
			if env.AppKey == "" {
				env.AppKey = "default"
			}
			if c.cfg.OnMessage != nil {
				go c.cfg.OnMessage(env) // 避免回调阻塞读循环
			}
		case CmdUploadMediaInit, CmdUploadMediaFinish:
			var rich AckRich
			if err := json.Unmarshal(raw, &rich); err != nil {
				diag.Warn("wecom.client.rich_ack.decode_fail", "err", err)
				continue
			}
			if ch := c.ackRichCh(env.Cmd); ch != nil {
				select {
				case ch <- rich:
				default:
					diag.Warn("wecom.client.rich_ack.drop", "cmd", env.Cmd)
				}
			}
		case CmdSubscribe, CmdRespond, CmdPing, CmdSend, CmdUploadMediaChunk:
			if ch := c.ackCh(env.Cmd); ch != nil {
				select {
				case ch <- Ack{Cmd: env.Cmd, ErrCode: env.ErrCode, ErrMsg: env.ErrMsg}:
				default:
					diag.Warn("wecom.client.ack.drop", "cmd", env.Cmd)
				}
			}
		default:
			diag.Debug("wecom.client.unknown_cmd", "cmd", env.Cmd)
		}
	}
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.HeartbeatInterval)
	defer t.Stop()
	// 清空 ping ACK 通道，避免上一轮残留
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.writeJSON(NewPing(uuid.NewString())); err != nil {
				diag.Warn("wecom.client.ping.write_fail", "err", err)
				_ = c.closeConn()
				return
			}
			select {
			case <-c.ackCh(CmdPing):
				c.missedPong.Store(0)
			case <-time.After(c.cfg.PongTimeout):
				n := c.missedPong.Add(1)
				diag.Warn("wecom.client.ping.timeout", "missed", n)
				if n >= 2 {
					_ = c.closeConn()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

// SendText 发送一次性文本回复，等待 ACK。
func (c *Client) SendText(sourceReqID, content string) error {
	c.respondMu.Lock()
	defer c.respondMu.Unlock()
	return c.sendWithAck(NewReplyText(sourceReqID, content))
}

// SendMarkdown 发送 stream 模式的 markdown 回复。
func (c *Client) SendMarkdown(sourceReqID, streamID, content string, finish bool) error {
	c.respondMu.Lock()
	defer c.respondMu.Unlock()
	return c.sendWithAck(NewReplyStream(sourceReqID, streamID, content, finish))
}

// SendImage / SendFile 走 aibot_send_msg，需要显式 receiveId + receiveIdType。
func (c *Client) SendImage(receiveID, receiveIDType, mediaID string) error {
	return c.sendMsgWithAck(NewSendImage(uuid.NewString(), receiveID, receiveIDType, mediaID))
}

func (c *Client) SendFile(receiveID, receiveIDType, mediaID string) error {
	return c.sendMsgWithAck(NewSendFile(uuid.NewString(), receiveID, receiveIDType, mediaID))
}

// SendMarkdownPush 主动发 markdown 文本（用于 chat.updated 场景，见 NewSendMarkdown）。
func (c *Client) SendMarkdownPush(receiveID, receiveIDType, content string) error {
	return c.sendMsgWithAck(NewSendMarkdown(uuid.NewString(), receiveID, receiveIDType, content))
}

func (c *Client) sendMsgWithAck(payload any) error {
	// aibot_send_msg 的 ACK 独立于 respond，用自己的 cmd channel。
	// 多并发发送需要串行化：共用 respondMu 简单（personal 单用户场景无瓶颈）
	c.respondMu.Lock()
	defer c.respondMu.Unlock()
	if err := c.writeJSON(payload); err != nil {
		return err
	}
	select {
	case ack := <-c.ackCh(CmdSend):
		if ack.ErrCode != 0 {
			return fmt.Errorf("send rejected: errcode=%d msg=%s", ack.ErrCode, ack.ErrMsg)
		}
		return nil
	case <-time.After(c.cfg.SendTimeout):
		return errors.New("send ACK timeout")
	}
}

const uploadChunkSize = 512 * 1024

// UploadMedia 执行 init/chunk/finish 三段分块上传，返回 media_id。
func (c *Client) UploadMedia(mediaType, filename string, data []byte) (string, error) {
	c.uploadMu.Lock()
	defer c.uploadMu.Unlock()

	total := len(data)
	chunks := (total + uploadChunkSize - 1) / uploadChunkSize
	if chunks == 0 {
		chunks = 1 // 空文件一块空内容
	}
	sum := md5.Sum(data)
	md5hex := hex.EncodeToString(sum[:])

	// 1) init
	if err := c.writeJSON(NewUploadInit(uuid.NewString(), mediaType, filename, int64(total), chunks, md5hex)); err != nil {
		return "", fmt.Errorf("upload init write: %w", err)
	}
	var uploadID string
	select {
	case ack := <-c.ackRichCh(CmdUploadMediaInit):
		if ack.ErrCode != 0 {
			return "", fmt.Errorf("upload init rejected: errcode=%d msg=%s", ack.ErrCode, ack.ErrMsg)
		}
		uploadID = ack.Body.UploadID
		if uploadID == "" {
			return "", errors.New("upload init: missing upload_id")
		}
	case <-time.After(c.cfg.SendTimeout):
		return "", errors.New("upload init ACK timeout")
	}

	// 2) chunks
	for i := 0; i < chunks; i++ {
		start := i * uploadChunkSize
		end := start + uploadChunkSize
		if end > total {
			end = total
		}
		chunkB64 := base64.StdEncoding.EncodeToString(data[start:end])
		if err := c.writeJSON(NewUploadChunk(uuid.NewString(), uploadID, i, chunkB64)); err != nil {
			return "", fmt.Errorf("upload chunk %d write: %w", i, err)
		}
		select {
		case ack := <-c.ackCh(CmdUploadMediaChunk):
			if ack.ErrCode != 0 {
				return "", fmt.Errorf("upload chunk %d rejected: errcode=%d", i, ack.ErrCode)
			}
		case <-time.After(c.cfg.SendTimeout):
			return "", fmt.Errorf("upload chunk %d ACK timeout", i)
		}
	}

	// 3) finish（Java 有重试 3 次、指数延迟；personal 场景先不重试，简化）
	if err := c.writeJSON(NewUploadFinish(uuid.NewString(), uploadID)); err != nil {
		return "", fmt.Errorf("upload finish write: %w", err)
	}
	select {
	case ack := <-c.ackRichCh(CmdUploadMediaFinish):
		if ack.ErrCode != 0 {
			return "", fmt.Errorf("upload finish rejected: errcode=%d msg=%s", ack.ErrCode, ack.ErrMsg)
		}
		if ack.Body.MediaID == "" {
			return "", errors.New("upload finish: missing media_id")
		}
		return ack.Body.MediaID, nil
	case <-time.After(c.cfg.SendTimeout):
		return "", errors.New("upload finish ACK timeout")
	}
}

func (c *Client) sendWithAck(payload any) error {
	if err := c.writeJSON(payload); err != nil {
		return err
	}
	select {
	case ack := <-c.ackCh(CmdRespond):
		if ack.ErrCode != 0 {
			return fmt.Errorf("respond rejected: errcode=%d msg=%s", ack.ErrCode, ack.ErrMsg)
		}
		return nil
	case <-time.After(c.cfg.SendTimeout):
		return errors.New("respond ACK timeout")
	}
}

func (c *Client) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("wecom client: not connected")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(gws.TextMessage, b)
}

func (c *Client) closeConn() error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// inferCmdFromReqID 把 ACK 帧的 req_id 前缀反推回它对应的发送 cmd。
// 企微 Bot server 的 ACK 帧不带 `cmd` 字段，只带 `req_id`，所以必须按
// 我们发送时生成的前缀匹配。respond 的 req_id 回显源 callback 的 req_id、
// 没有固定前缀，返回空串由 caller 兜底到 CmdRespond。
func inferCmdFromReqID(reqID string) string {
	switch {
	case strings.HasPrefix(reqID, "aibot_subscribe_"):
		return CmdSubscribe
	case strings.HasPrefix(reqID, "aibot_upload_media_init_"):
		return CmdUploadMediaInit
	case strings.HasPrefix(reqID, "aibot_upload_media_chunk_"):
		return CmdUploadMediaChunk
	case strings.HasPrefix(reqID, "aibot_upload_media_finish_"):
		return CmdUploadMediaFinish
	case strings.HasPrefix(reqID, "aibot_send_msg_"):
		return CmdSend
	case strings.HasPrefix(reqID, "ping_"):
		return CmdPing
	}
	return CmdRespond
}
