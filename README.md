# agent-wecom-bridge

Go 实现的个人场景企业微信 ↔ agent-platform 桥接器。替代 `aiagent-gateway` 在单用户 desktop 场景下的角色：对 `agent-platform` 透明等价，对企业微信走标准 AiBot WebSocket。

## 定位

- **对 platform 透明**：实现 `aiagent-gateway` 暴露给 platform 的全部反向 WS + HTTP 旁路协议，字节级对齐。platform 只需把 `GATEWAY_WS_URL` 指向 bridge，一行代码不改。
- **对企微直连**：连 `wss://openws.work.weixin.qq.com`，不走公司统一网关。
- **不做多租户**：一个进程对应一个 desktop 用户、一对 bot 凭证、一个会话——这是 bridge 砍掉 DB / R2DBC / Spring Context 一套的前提。

## 功能矩阵

| 方向 | 类型 | 路径 |
|---|---|---|
| 手机企微 → bridge → platform | 文本 | `wecom.handleText` → `/api/query` RequestFrame |
| 同上 | 语音（wecom 后端已 ASR） | `wecom.handleVoice` → 取 `recognized_text` → `/api/query` |
| 同上 | 图片 / 文件 | `wecom.handleMedia` → 下载 + AES-CBC 解密 + SHA256 + store → `/api/upload` RequestFrame |
| platform → bridge → 手机企微 | markdown（`chat.updated` push） | `HandlePlatformFrame` → `wecom.SendMarkdown`（stream 模式） |
| 同上 | 文件 / 图片（`/api/push` multipart） | `httpapi.OnPushed` → `wecom.UploadMedia` 三段分块 → `SendImage` / `SendFile` |

**不在 scope**（memory 标注）：
- 出站语音 TTS（Java 侧依赖外部 `TextToSpeechService`，个人场景先跳过）
- 多租户 / DB / Spring
- `/api/submit` `/api/steer` `/api/interrupt` 出站请求（企微 UI 没对应触发点）

## 目录结构

```
cmd/bridge/main.go
internal/
├── config/      # .env 加载
├── diag/        # 结构化日志
├── protocol/    # ticket + platform 帧 + chatid 格式（对 platform 的协议）
├── server/      # /ws/agent + /api/push + /api/download + 本地 blob store
└── wecom/       # 企微 bot client + 密码学 + 媒体下载/上传 + 粘合层
```

5 个包、~2500 行 Go、全 TDD 覆盖。

## 协议契约

所有"这个字段叫什么、顺序怎么排"的细节锁在 [`SPEC.md`](./SPEC.md)，包括：

- JWT ticket（无签名三段，header 字段顺序 `alg/ak/ch`）
- `/ws/agent` 握手 + `push/connected` 首帧 7 字段
- 5 种帧 `request / response / stream / push / error`
- chatId 格式 `wecom#{single|group}#{sourceId}#{base36(ts*1000+seq)}`
- `/api/push` multipart 字段名 + 响应 `Result<UploadResponse>` 结构
- `/api/download/**` 鉴权（`?ticket=` 或 `Authorization: Bearer`）+ `userId` 路径前缀防越权 + RFC5987 Content-Disposition

企微 Bot 侧对齐 `aiagent-gateway/gateway-channel/.../WecomWebSocketClient.java`：

- 订阅：`aibot_subscribe` + `{bot_id, secret}`
- 心跳：每 30s 发 `cmd:"ping"`；2 次 pong 丢失 → 重连（指数退避 1s → 30s）
- 入站：`aibot_msg_callback`，按 `msgtype` 分发 text / image / file / voice
- 出站文本/markdown：`aibot_respond_msg`
- 出站媒体：先走 `aibot_upload_media_init` / `_chunk`（512KB + base64）/ `_finish` 拿 `media_id`，再发 `aibot_send_msg`
- **ACK 帧不带 `req_id`**，按 `cmd` 排 FIFO 对账

## 环境变量

拷贝 `.env.example` 为 `.env`，关键字段：

| 变量 | 必填 | 说明 |
|---|---|---|
| `BRIDGE_HTTP_ADDR` | | 监听地址，默认 `:11970`（`:11960` 被 Container Hub 占） |
| `BRIDGE_STATE_DIR` | | 本地存储根目录 |
| `BRIDGE_AGENT_KEY` | ✅ | 对 platform 暴露的 agentKey |
| `BRIDGE_CHANNEL` | ✅ | 对 platform 暴露的 channel 标签 |
| `BRIDGE_USER_ID` | ✅ | 对 platform 暴露的 userId（ticket.payload.sub） |
| `WECOM_ENABLED` | | `true` 才真连企微；`false` 时只跑 platform 协议面 |
| `WECOM_WS_URL` | | 默认 `wss://openws.work.weixin.qq.com` |
| `WECOM_BOT_ID` | ✅（启用时） | 企微后台 AiBot 的 bot_id |
| `WECOM_SECRET` | ✅（启用时） | 企微后台 AiBot 的 secret |
| `WECOM_APP_KEY` | | 多实例区分，个人写 `default` |
| `WECOM_HEARTBEAT_SECONDS` | | 心跳间隔，默认 30 |

## 运行

```bash
make build       # → bin/agent-wecom-bridge
make run         # 读 .env 启动
make test        # go test ./...
```

启动后 stdout 会打印：

```
=========================================
GATEWAY_WS_URL=ws://<host>:11970/ws/agent?agentKey=zenmi&channel=wecom:xiaozhai
GATEWAY_JWT_TOKEN=eyJhbGciOi...
=========================================
```

把这两行塞进 `agent-platform/.env`，platform 启动即可反向连上 bridge。

## 凭证申请

企业微信后台 → 应用管理 → 智能机器人 → 创建 AiBot，拿到 `bot_id` + `secret`。

**每个 desktop 用户需要自己的凭证**——同一 bot 的同时订阅会互相挤下线。公司 `aiagent-gateway/application-dev.yaml` 里的那对小宅凭证只适合短期测试（测的时候要把公司 gateway 停掉）。

## 当前状态

- [x] Phase 0：Java 契约 → `SPEC.md`
- [x] Phase 1：Go 骨架 + `/healthz`
- [x] Phase 2：platform 协议面（`/ws/agent` + `/api/push` + `/api/download` + JWT）
- [x] Phase 3：企微文本收发 + `chat.updated` markdown 回传
- [x] Phase 4：企微媒体（AES-CBC/PKCS#7 解密、分块上传、入站 image/file → `/api/upload`、出站 `/api/push` → wecom）
- [x] Phase 5：主动推送（复用 Phase 3 路径——platform 侧 `internal/schedule` 已经有 cron，触发发 `chat.updated`，bridge 按已实现的路径回推）
- [ ] E2E：用真 bot 凭证 + 真 `agent-platform` runner 做一次完整往返

全量 `go test ./...` 绿，`go build ./...` 通过。

## 参考

- Java 参考实现：`F:\js\aiagent-gateway`
- Platform 对端 Go 客户端：`F:\js\agent-platform\internal\ws\gatewayclient`
- 字节级契约：`./SPEC.md`
