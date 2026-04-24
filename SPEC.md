# agent-wecom-bridge — 对 platform 透明协议 SPEC

本文档锁定 bridge 必须对 agent-platform 提供的**字节级**协议。
平台端（`agent-platform`）是契约权威方，本文档对齐该契约——platform 切换 GATEWAY_WS_URL 到 bridge 后应完全无感。

参考实现：`aiagent-gateway`（Java, Spring Boot），本 SPEC 的每条 **L** 标签指向 Java 源码具体行。

---

## 1. JWT Ticket

**职责**：WS 握手 + `/api/download` ticket + `/api/push` 鉴权。无签名。

**格式** (`DownstreamAgentTicketService.java:127-142`)：

```
<base64url(header)>.<base64url(payload)>.
```

- 三段用 `.` 分隔，**第三段为空**（保留 trailing dot）
- Base64URL **无 padding**，UTF-8

**header**（字段顺序固定）：
```json
{"alg":"none","ak":"<agentKey_lowercase>","ch":"<channel_lowercase>"}
```

**payload**：
```json
{"sub":"<userId>"}
```

**校验**（`DownstreamAgentTicketService.java:144-168`）：
1. split by `.`，必须 3 段
2. `alg` 必须是 `"none"`
3. `sub`/`ak`/`ch` 都非空
4. `ak`、`ch` 做 `trim().toLowerCase()` 后比较

**bridge 实现要点**：个人场景下 bridge 启动时可预生成一个 ticket 写到日志/stdout，让用户贴进 platform 的 `GATEWAY_JWT_TOKEN`，行为等同 aiagent-gateway 的连接引导。

---

## 2. `/ws/agent` WebSocket（bridge 为 server，platform 为 client）

### 2.1 握手

**URL** (platform 那边写全)：
```
ws://<bridge-host>:<port>/ws/agent?agentKey=<key>&channel=<channel>
```

**认证**（二选一，`DownstreamAgentPushWebSocketHandler.java:138-153`）：
- `Authorization: Bearer <ticket>` header（platform 默认用这个）
- query string `?ticket=<ticket>`

**query 参数**：
- `agentKey` (必填) — 从 ticket.header.ak 校验一致
- `channel` (必填) — 如 `wecom:xiaozhai`，从 ticket.header.ch 校验一致
- `userId` (可选) — 若提供，必须等于 ticket.payload.sub

**鉴权失败**（`L100-107`）：
1. 先发：`{"frame":"error","type":"unauthorized","code":401,"msg":"Invalid or expired ticket."}`
2. 再关闭连接，CloseCode = `1008 Policy Violation`

### 2.2 连接成功后首帧（server → client）

`DownstreamAgentPushWebSocketHandler.java:280-293`：

```json
{
  "frame": "push",
  "type": "connected",
  "data": {
    "sessionId": "<uuid>",
    "userId": "<from ticket>",
    "agentKey": "<from query>",
    "channel": "<from query>",
    "status": "<session status>",
    "ticketAccepted": true,
    "timestamp": "<ISO-8601>"
  }
}
```

### 2.3 帧协议（所有帧都是 JSON text frame）

所有帧顶层有 `frame` 字段用于分发。

#### 2.3.1 platform → bridge（入站）

| frame | 用途 | 处理 |
|---|---|---|
| `stream` | 流式增量 | 累积；若 `reason` 非空 → 结束该 requestId |
| `response` | 一次性响应 | 直接结束该 requestId |
| `error` | 该请求失败 | 按 `id` 失败对应 requestId；若 id 空且只有一个 active request，用它 |
| `push` | 主动推送 | 目前只处理 `type=="chat.updated"`（见 §2.4） |

**stream frame**：
```json
{"frame":"stream","id":"<reqId>","data":...,"reason":"<optional>"}
```

**response frame**（`ResponseFrame` 反序列化）：
```json
{"frame":"response","id":"<reqId>","data":...}
```

**error frame**：
```json
{"frame":"error","id":"<reqId>","code":<int>,"msg":"..."}
```

#### 2.3.2 bridge → platform（出站）

所有帧都有 `"frame"` 字段做分发，**按 Java `gateway-interface/protocol/websocket/*.java` 逐字段锁死**：

**RequestFrame**（bridge → platform 主动请求）：
```json
{
  "frame": "request",
  "type": "/api/query" | "/api/upload" | "/api/submit" | "/api/steer" | "/api/interrupt",
  "id": "<requestId>",
  "payload": { ... }
}
```

**StreamFrame**（platform → bridge，流式增量）：
```json
{
  "frame": "stream",
  "id": "<reqId>",
  "streamId": "<optional>",
  "event": { ...BaseEvent... },
  "reason": "<optional, 非空表示结束>",
  "lastSeq": <optional int>
}
```

**ResponseFrame**（platform → bridge，一次性响应）：
```json
{"frame":"response","type":"<>","id":"<>","code":<int>,"msg":"<>","data":<any>}
```

**ErrorFrame**：
```json
{"frame":"error","type":"<>","id":"<>","code":<int>,"msg":"<>","data":<any>}
```

**PushFrame**：
```json
{"frame":"push","type":"<>","data":<any>}
```

**WsFrameType 常量**（`WsFrameType.java`）：`request` / `response` / `stream` / `push` / `error`。

### 2.4 `chat.updated` 主动推送处理

当 platform run 完成后会发（`DownstreamAgentPushWebSocketHandler.java:221-278`）：

```json
{
  "frame": "push",
  "type": "chat.updated",
  "data": {
    "chatId": "wecom#single#<sourceId>#<base36seq>",
    "lastRunId": "...",
    "lastRunContent": "markdown text"
  }
}
```

处理逻辑：
1. 从 `chatId` 用 §3 的规则反解出 `sourceId`
2. 用 `sessionContext.metadata.lastPushedRunId` 去重（防同一 run 重复推送）
3. 若 `lastRunContent` 非空：调企微 markdown 消息发送 → 写回 `metadata.lastPushedRunId`

### 2.5 `/api/upload` 帧（用户上传文件触发）

bridge 收到用户发文件后，先本地落存储，再 WS 推：

```json
{
  "frame": "request",
  "type": "/api/upload",
  "id": "<requestId>",
  "payload": {
    "requestId": "<same>",
    "chatId": "<wecom chatId>",
    "upload": {
      "type": "file" | "image",
      "name": "<filename>",
      "mimeType": "<content-type>",
      "sizeBytes": <long>,
      "sha256": "<lowercase hex>",
      "url": "/api/download/<userId>/<chatId>/<fileId>?ticket=<jwt>"
    }
  }
}
```

规则（`FileController.java:183-239, 666-668, 692-704`）：
- `type` 只取 `"image"` 或 `"file"`；`image/*` content-type 或扩展名推断为 image 时 → `"image"`，否则 `"file"`
- `sha256` = SHA-256 of file bytes，小写 hex，**必填**
- `url` 格式固定：`/api/download/<userId>/<chatId>/<fileId>?ticket=<jwt>`
- platform 会立即 GET 这个 url 拉字节

---

## 3. WeCom chatId 格式（**bridge 生成**）

> ⚠️ 与 aiagent-gateway Java 源码（用 `-` 分隔）**不一致**——Java 源是旧版。bridge 按用户确认的 `#` 分隔规则实现。

**格式**：
```
wecom#{chatType}#{encodedSourceId}#{base36(seq)}
```

- `chatType` ∈ {`single`, `group`}
- `encodedSourceId` = `sourceId` 中把 `#` 字符转义（TBD：确认转义规则；Java 是把 `-` 换成 `#`，`#` 版待定——**需要看网关新版的 escape 规则或直接由 bridge 定义**）
- `seq` = `epochSeconds * 1000 + secondSequence`，Base36 小写 (`Long.toString(seq, 36)`)
- `secondSequence`：每秒 0-999，超过 999 时秒数进位、序列归 0；保证单调递增

**解析正则**（反向）：
```regex
^wecom#(group|single)#(.+)#([0-9a-z]+)$
```
`.+` 是贪婪的，最后一段 base36 seq 之前的部分就是 encodedSourceId。

**Cache 规则**（`WecomSessionChatIdService.java:26-34`）：
按 `{appKey}|{chatType}|{sourceId}` 三元组 cache，同一 source 的 chatId 复用。

**TODO Phase 3**：确认 `#` 转义细节——等开写企微接入层时 grep 网关最新版本。

---

## 4. `/api/download/**` HTTP（bridge 作为 server）

**路径** (`FileController.java:311-364`)：
```
GET /api/download/<userId>/<chatId>/<fileId>?ticket=<jwt>
```

（实际代码是 `/api/download/**` 通配，取 `/download/` 后全部为 objectPath。）

**校验**：
1. `ticket` 必须通过 JWT 校验
2. 请求路径里的第一段必须等于 `ticket.payload.sub`（防越权）
   - L335-338: 若 `path` 不以 `<userId>/` 开头 → 403
3. 无 ticket 或无效 → 401

**响应头**：
- `Content-Type`: 存储元数据里的（fallback 用 MediaTypeFactory 猜）
- `Content-Length`: 元数据
- `Content-Disposition`: `attachment; filename*=UTF-8''<urlencoded name>`

**404**: 对象不存在

---

## 5. `/api/push` HTTP（bridge 作为 server，platform 推生成的文件回来）

**路径** (`FileController.java:241-260`)：
```
POST /api/push
Content-Type: multipart/form-data
```

**multipart fields**：
| field | 必填 | 说明 |
|---|---|---|
| `chatId` | ✅ | wecom chatId |
| `name` | ❌ | 文件名；未给 → 用 filePart.filename |
| `type` | ❌ | content-type 建议值；为空或 `application/octet-stream` → 用 Part header → MediaTypeFactory 猜 |
| `requestId` | ❌ | 未给 → 自动生成 `upload_<ms>_<6hex>` |
| `file` | ✅ | 文件字节 |

**鉴权**：platform 侧用 `Authorization: Bearer <ticket>`（见 platform `artifactpusher.Config.AuthToken = cfg.GatewayWS.JwtToken`）。bridge 这侧校验同 ticket，从 `payload.sub` 取 userId。

**处理逻辑**（对齐 Java）：
1. 校验 ticket → 取 userId
2. 从 chatId 反解 WeCom target (`appKey`, `receiveId`, `receiveIdType`)
   - 个人场景 bridge 自己维护 `chatId → {appKey, receiveId, receiveIdType}` map（Java 用 DB + context registry，bridge 用 内存 + 持久化文件）
3. 图片（content-type 以 `image/` 开头 或 扩展名被 MediaTypeFactory 判为 image）→ 调 wecom `sendImageMessageByApp`；否则 → `sendFileMessageByApp`
4. 返回 `UploadResponse`：
   ```json
   {
     "code": 0,
     "data": {
       "requestId": "<>",
       "chatId": "<>",
       "upload": {"name":"<>","mimeType":"<>","sizeBytes":<>,"url":"<objectPath>"}
     }
   }
   ```

---

## 6. bridge → platform request type 范围

**Phase 3 只实现 `/api/query`**（文本消息入站）。

**砍掉**（企微 UI 没对应触发点）：
- `/api/steer` — 运行中改方向：企微没"改方向"按钮
- `/api/interrupt` — 中断：企微没"停止"按钮
- `/api/submit` — agent 挂起提问的回应：企微里就是普通文本消息，走 `/api/query`，platform 根据 chatId + run 状态自己识别

**Phase 3 只需做的出站 request**：`/api/upload`（§2.5）+ `/api/query`。

---

## 7. 待办（Phase 0 结束前必须闭环）

以下事项在相应 Phase 开工时补齐：

- **[Phase 2]** ✅ 已确认 `RequestFrame.frame = "request"` 等所有帧都有 `frame` 字段（见 §2.3）
- **[Phase 2]** ✅ 已确认 platform `gatewayclient` 在反向 WS 模式下不主动发 `connected`，等网关发注册 ACK（`agent-platform/internal/ws/gatewayclient/client_test.go:78`）——bridge §2.2 首帧方向正确
- **[Phase 3]** 读 `WecomWebSocketClient.java` (736L) + `WecomWebsocket.java` (~700L) + `WecomMessageSender.java`，锁企微 Bot WS 帧协议、心跳、req_id、流式回复策略
- **[Phase 3]** 确认 wecom chatId 里 sourceId 的转义规则（网关最新版）
- **[Phase 4]** 读 `WecomIncomingFileService.java` (529L)，锁 AES-CBC + PKCS#7 + SHA256 + 分块上传 MD5 细节

---

## 8. 不实现（明确砍掉）

- 多租户 / 多用户并发（个人场景 = 单 user）
- R2DBC / PostgreSQL（改本地 JSON 文件 state）
- Dify / AGUI / HiAgent / GXJAI adapter
- OpenClawWeixin 回调模式
- Spring MCP Server
- `/api/submit` / `/api/steer` / `/api/interrupt` 出站请求（企微没对应交互）

## 9. 明确要做

- TTS / 语音合成（文本回复可切换语音模式）—— Phase 4
- 用户语音消息识别（企微端发语音 → bridge 转文字/或原样给 platform 让它识别）—— Phase 4
- agent 原生语音回传 —— Phase 4
