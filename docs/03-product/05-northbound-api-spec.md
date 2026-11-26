# 北向接口规范（v1）

> **版本**：API v1（`/api/v1`）
> **上游**：`01-prd.md` FR-NOR-*、`02-architecture.md` §4.3
> **代码对应**：`internal/northbound/httpapi`（REST + SSE）、`internal/northbound/opcua` + `internal/opcuaserver`（OPC UA）、`internal/mqtt` + `internal/runtime/mqtt_sink.go`（MQTT）、`internal/alarm`（报警引擎）
> **原则**：一个数据契约，四种传输（REST / SSE / MQTT / OPC UA）；语义一致，表达不同。

---

## 1. 四通道定位

| 通道 | 适用 | 优势 | 限制 |
|---|---|---|---|
| **REST** | 配置、查询、写入、运维 | 通用、可调试、防火墙友好 | 轮询有延迟 |
| **SSE** | 浏览器/轻客户端实时值 | 单向、自动重连、实现简单 | 单向；HTTP/1.1 连接数限制 |
| **MQTT** | 上云、跨网、多消费者 | 发布订阅、QoS、断线续传 | 需要 Broker |
| **OPC UA** | SCADA/MES 标准对接 | 信息模型、订阅、标准工具链 | 客户端复杂度高、授权/证书 |

**为什么四个都要**：PA-37 证明「HTTP + JSON」是最容易被集成方接受的形态；PA-08 证明 OPC UA 是 SCADA 对接的标准面。只做其中一个，都会丢掉一半集成方。

---

## 2. 通用约定

### 2.1 基础

| 项 | 约定 |
|---|---|
| Base URL | `https://<host>:<port>/api/v1` |
| 编码 | UTF-8；`Content-Type: application/json` |
| 时间 | RFC 3339，UTC，毫秒精度（`2026-11-04T08:30:00.123Z`） |
| 命名 | JSON 字段 `camelCase`；路径 `kebab-case` |
| 版本 | 路径版本（`/v1`）；破坏性变更升 `/v2`，`/v1` 至少保留 12 个月 |
| 请求 ID | 响应头 `X-Request-Id`；客户端可传 `X-Request-Id` 透传 |

### 2.2 认证与身份

```
Authorization: Bearer <token>
```

凭据是**配置里的静态 Token**，一个 Token 对应一个具名用户与一个角色（Wave 1
不做登录会话与 Token 过期；理由见 §4.9.1）。

| 规则 | 说明 |
|---|---|
| Token 形态 | 不透明字符串，≥16 字符，不含空白；推荐 `uic token` 生成 |
| 配置位置 | `server.users[]`，每项 `name`/`role` + `token` 或 `tokenSha256` |
| 比对方式 | 服务端先做 SHA-256，再定长比较；配置文件可只存哈希 |
| 失败 | `401` + `WWW-Authenticate: Bearer` + `auth.unauthorized` |
| 权限不足 | `403` + `auth.forbidden`，`message` 指出缺少哪个权限 |
| 开放模式 | 未配置任何凭据时认证关闭，启动横幅与 `auth_open_mode` WARN 明确提示 |
| 身份查询 | `GET /api/v1/auth/me`（公开）返回当前用户、角色、是否开放模式 |
| 凭据热更新 | 改 `server.users` 后 `POST /config/apply`，新请求立即按新表判定 |

#### 2.2.1 角色与权限

权限按**动作**定义，不按资源路径。角色是权限集合，且满足
`viewer ⊆ operator ⊆ admin`——这条包含关系是刻意的：任何「低角色能做而高
角色不能做」的设计最终都解释不清。

| 权限 | viewer | operator | admin | 覆盖的动作 |
|---|:--:|:--:|:--:|---|
| `read` | ✓ | ✓ | ✓ | 设备/点位/历史/报警/SSE 查询 |
| `point.write` | — | ✓ | ✓ | 单点与批量写入 |
| `alarm.ack` | — | ✓ | ✓ | 报警确认 |
| `command` | — | ✓ | ✓ | 普通白名单命令 |
| `command.danger` | — | — | ✓ | `outbox.drop` / `config.rollback` / `log.level` |
| `config.read` | — | ✓ | ✓ | 读配置、版本、diff |
| `config.write` | — | — | ✓ | 校验/应用/回滚配置 |
| `audit.read` | — | — | ✓ | 审计查询/校验/统计、用户枚举 |
| `diagnostics` | — | ✓ | ✓ | 日志、诊断包 |

| 角色 | 定位 |
|---|---|
| `viewer` | 只看。给值班大屏、只读看板、跨部门查询 |
| `operator` | 能作业不能改结构：写点位、确认报警、重启采集、导诊断包 |
| `admin` | 全部，含配置与审计 |

**未知角色一律拒绝**（不是默认放行）。`role` 字段缺省时按 `admin` 处理，仅
为兼容没有该字段的历史配置——新配置必须显式写角色。

### 2.3 错误响应

```json
{
  "error": {
    "code": "point.not_found",
    "message": "point 'temp' not found on device 'plc-01'",
    "requestId": "req-7f3a...",
    "details": { "device": "plc-01", "point": "temp" }
  }
}
```

| HTTP | 错误码前缀 | 含义 |
|---|---|---|
| 400 | `request.*` / `config.*` | 参数/配置非法 |
| 401 | `auth.*` | 未认证 |
| 403 | `auth.forbidden` / `point.not_writable` | 无权限 |
| 404 | `*.not_found` | 资源不存在 |
| 409 | `config.version_conflict` / `point.write_busy` | 状态冲突 |
| 422 | `config.invalid_*` | 语义校验失败 |
| 429 | `request.rate_limited` | 限流 |
| 500 | `internal.*` | 内部错误 |
| 503 | `service.not_ready` | 未就绪 |

### 2.4 分页与过滤

| 参数 | 默认 | 上限 | 说明 |
|---|---|---|---|
| `limit` | 100 | 1000 | 条数 |
| `cursor` | — | — | 游标（不透明字符串） |
| `q` | — | 128 字符 | 前缀/子串搜索（设备 ID、点位 ID） |
| `device` | — | — | 按设备过滤 |
| `quality` | — | — | 按质量码过滤 |

响应：

```json
{ "items": [ ... ], "nextCursor": "eyJvZmZzZXQiOjEwMH0", "total": 1234 }
```

### 2.5 限流

| 类别 | 限制 | 超限 |
|---|---|---|
| 读接口 | 100 req/s / IP | 429 |
| 写接口 | 10 req/s / 用户 | 429 |
| SSE 连接 | 50 / 实例 | 429 |
| 登录 | 5 次/分钟 / IP | 429 + 锁定 60s |

---

## 3. 数据契约

### 3.1 Sample（核心对象）

```json
{
  "device": "plc-01",
  "point": "temp",
  "value": { "type": "float32", "num": 23.5 },
  "quality": "good",
  "unit": "℃",
  "deviceTime": "2026-11-04T08:29:59.900Z",
  "collectedAt": "2026-11-04T08:30:00.012Z",
  "seq": 84213
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `device` | string | ✓ | 设备 ID |
| `point` | string | ✓ | 点位 ID |
| `value` | object | ✓ | `{type, num?, text?, bool?}` |
| `quality` | enum | ✓ | 见 §3.2 |
| `unit` | string | ✗ | 工程单位 |
| `deviceTime` | string | ✗ | 设备侧时标（协议携带） |
| `collectedAt` | string | ✓ | 网关采集时间（单调时钟换算） |
| `seq` | uint64 | ✓ | 设备内单调递增 |
| `error` | string | ✗ | 失败原因（仅非 good） |

### 3.2 Quality 枚举

`good` / `uncertain` / `stale` / `bad` / `timeout` / `comm_error` / `address_error` / `not_connected` / `disabled`

### 3.3 Value 类型

```json
{ "type": "uint16",  "num": 1234 }
{ "type": "float32", "num": 23.5 }
{ "type": "bool",    "bool": true }
{ "type": "string",  "text": "RUN" }
```

| 规则 | 说明 |
|---|---|
| 数值统一放 `num` | 整数也放 `num`（JSON number）；超过 2^53 用字符串 `text` |
| 布尔放 `bool` | 不混用 `num` |
| 字符串放 `text` | 不做 base64，除非二进制（`type: "bytes"` + `text` 为 base64） |
| 类型必须与配置一致 | 不一致返回 `value.type_mismatch` |

### 3.4 契约版本

| 字段 | 位置 | 当前值 |
|---|---|---|
| `schemaVersion` | 配置 | 1 |
| `apiVersion` | `/api/v1` | 1 |
| `payloadVersion` | MQTT 消息属性（可选） | 1 |

**演进规则**：新增字段向后兼容（消费者忽略未知字段）；删除/改名字段必须升 `/v2` 并提供双发过渡期（≥ 6 个月）。

---

## 4. REST 端点

### 4.1 系统

| 方法 | 路径 | 说明 | 认证 |
|---|---|---|---|
| GET | `/healthz` | 进程存活（不检查依赖） | 否 |
| GET | `/readyz` | 依赖就绪（配置/存储/驱动） | 否 |
| GET | `/api/v1/health` | 业务健康聚合 | 是 |
| GET | `/api/v1/version` | 版本与构建信息 | 否 |
| GET | `/api/v1/metrics` | Prometheus 文本格式 | 可选 |

**`/readyz` 响应**：

```json
{
  "ready": true,
  "checks": [
    { "name": "config",  "ok": true },
    { "name": "storage", "ok": true, "detail": "writable" },
    { "name": "drivers", "ok": true, "detail": "5 registered" }
  ]
}
```

**`/api/v1/health` 响应**：

```json
{
  "status": "degraded",
  "uptimeSeconds": 86400,
  "devices": { "total": 18, "collecting": 16, "degraded": 1, "faulted": 1 },
  "points":  { "total": 1200, "good": 1180, "stale": 12, "bad": 8 },
  "outbox":  { "pending": 42, "droppedLastHour": 0 },
  "scheduler": { "verdict": "healthy", "ageMs": 37, "stallAfterMs": 15000 },
  "problems": [
    { "device": "plc-07", "state": "faulted", "reason": "timeout", "since": "2026-11-04T07:10:00Z" }
  ]
}
```

#### `scheduler` 块：采集中枢活性

`/healthz` 只回答「HTTP 层还在不在」。它无法回答另一个更难、也更致命的问题：
**进程活着、端口通、`/healthz` 200，但采集调度是不是已经停摆了？**

调度器是单协程 tick 循环（默认 50ms）。一旦它因死锁或长时间阻塞而不再 tick，
所有设备都不再被调度，但：设备运行态仍是 `collecting`（没有新的失败能触发迁移）、
点位在 `stale` 判定的 3 个周期内仍显示好值、HTTP 层照常响应。对外表现为「一台
完全健康的网关，实际上一帧数据都没有」。这是最难被外部 supervisor 发现的静默失效。

因此 `/api/v1/health` 增加 `scheduler` 块，由**最近一次 tick 距今的时间**判定：

| `verdict` | 含义 | 判据 |
|---|---|---|
| `healthy` | 中枢在转 | `ageMs <= stallAfterMs` |
| `stalled` | 中枢停摆 | `ageMs > stallAfterMs`（严格大于） |
| `unknown` | 无法判定 | 调度器尚未 tick（刚启动） |

- `stallAfterMs` 默认 15000（15s）。调度器每秒 tick 二十次，15s 给了偶发长阻塞
  （如大配置热更新的短暂停顿）足够余量，又远小于人工巡检周期。
- `unknown` 不等于 `stalled`：刚启动的一瞬间不能误报卡死。
- `verdict=stalled` 时 `status` 被置为 `stalled`（**优先级最高**，压过
  `degraded`/`empty`），并追加一条 `state: "stalled"` 的 `problems` 条目——避免
  被设备态的「看似正常」掩盖。
- 该信号同时写入指标 `uic_scheduler_stalled_total`（边沿触发，一次卡死只记一次）、
  `uic_scheduler_tick_age_seconds`（gauge，可做趋势告警），并写 `scheduler_stalled`
  / `scheduler_recovered` 日志。

`/healthz` 与 `/readyz` 的既有契约不变：`/healthz` 仍无条件 200（存活探针不应因
业务卡死而重启进程），`/readyz` 仍只检查配置/存储/驱动。卡死信号只经
`/api/v1/health`、指标、日志与心跳文件对外。

#### 心跳文件：给不访问 HTTP 的 supervisor

`/healthz` 需要能访问 HTTP 端口。断网现场、老式监控脚本、裸机 cron 往往不能。
因此运行时把同一份存活性快照原子写入 `<dataDir>/heartbeat.json`（默认每 5s）：

```json
{
  "ts": 1793500800,
  "tsMs": 1793500800123,
  "tsAt": "2026-11-26T12:00:00.123Z",
  "pid": 4242,
  "instanceId": "inst-a1b2c3d4e5f6",
  "version": "1.4.0",
  "uptimeSeconds": 86400,
  "scheduler": { "verdict": "healthy", "ageMs": 37, "stallAfterMs": 15000 },
  "devices": { "total": 18, "collecting": 16, "degraded": 1, "faulted": 1, "disabled": 0 }
}
```

读法：`tsMs` 长时间不前进 ⟹ 进程疑似挂了；`tsMs` 在前进但 `scheduler.verdict`
为 `stalled` ⟹ 进程活着但采集中枢卡死。两者必须分开判断——这正是单靠「进程在不在」
无法覆盖的场景。

- 写入是 tmp + rename 的原子替换，读方不会读到写了一半的 JSON。
- 启动即写首份，不必等一个完整间隔。
- `ts`（秒）在亚秒级间隔下会重复，故另给 `tsMs`（毫秒）供新鲜度判断。
- 写入失败只记一次告警、不影响进程寿命：心跳是观测手段，不是业务前提。

**不变量：`status != "ok"` ⟹ `problems` 非空。**

设备从 `collecting` 掉到 `faulted` 需要连续 3 次失败（防抖），而点位的坏质量在
第一次失败时就已产生。因此存在一个真实窗口：`status: degraded`、`points.bad: 7`、
但没有任何设备态问题项。只靠设备态填 `problems` 会漏掉它。此时补一条聚合项
（`device` 为空）：

```json
{ "state": "degraded", "reason": "point quality degraded: bad=7 stale=0 uncertain=0 (of 7)" }
```

理由很直接：健康检查的消费者是告警系统与人。一条「说自己有病但说不出哪里疼」的
响应，最终一定会变成没人看的一行绿字。

**`/api/v1/version` 响应**：

```json
{
  "version": "1.4.0",
  "commit": "a1b2c3d",
  "buildTime": "2026-11-20T08:00:00Z",
  "goVersion": "go1.26.10",
  "schemaVersion": 1,
  "apiVersion": 1,
  "instanceId": "inst-a1b2c3d4e5f6"
}
```

`instanceId` 是**本实例的稳定身份**：首次启动时生成、持久化在数据目录
（`<dataDir>/instance.id`），之后重启复用。它有两个用途：

- **多实例消歧**：同站点跑两个实例（一台主机多容器、双机热备、灰度期新旧并存）
  时，仅凭 `site` 无法区分「这是哪台」。`instanceId` 与 MQTT `clientId` /
  Sparkplug `nodeId` 的**默认值同源**——默认派生为 `uic-<site>-<短码>`，使同站点
  多实例默认不会连到同一 Broker 会话上互相顶掉（后到的把前一个踢下线，表现为
  「数据莫名断流、日志里却无任何错误」）。
- **运维识别**：诊断包 `manifest.json` 同样带 `instanceId`，一份诊断包可直接对应
  到具体实例。

它是「部署事实」而非「业务配置」，因此**不进配置文件**（否则会进入配置内容哈希，
污染版本列表与回滚历史）。用户显式配置了 `clientId`/`nodeId` 时以其为准，实例
短码只在默认值这一层兜底。

### 4.2 设备与点位

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/devices` | 设备列表（含运行态摘要） |
| GET | `/api/v1/devices/{device}` | 设备详情（配置态 + 运行态） |
| GET | `/api/v1/devices/{device}/points` | 点位列表（含最新值） |
| GET | `/api/v1/points` | 跨设备点位查询（`device`/`q`/`quality`） |
| GET | `/api/v1/points/{device}/{point}` | 单点当前值 |
| POST | `/api/v1/points/{device}/{point}/write` | 写单点 |
| POST | `/api/v1/points/{device}/write` | 批量写（≤ 100 点） |

**设备详情**：

```json
{
  "id": "plc-01",
  "name": "1# 注塑机",
  "channel": "ch-plc",
  "protocol": "modbus-tcp",
  "configuredState": "enabled",
  "runtimeState": "collecting",
  "since": "2026-11-04T06:00:00Z",
  "generation": 7,
  "interval": "1s",
  "stats": {
    "readsTotal": 84213,
    "readsFailed": 12,
    "successRate": 0.99986,
    "p95DurationMs": 18,
    "lastError": null
  },
  "points": { "total": 64, "good": 63, "bad": 1 }
}
```

**`runtimeState` 与 `configuredState` 必须同时返回**：

| configuredState | runtimeState | 含义 |
|---|---|---|
| `enabled` | `collecting` | 正常 |
| `enabled` | `faulted` | 配置了但采不到（**重点告警对象**） |
| `enabled` | `connecting` | 正在连接 |
| `disabled` | `disabled` | 人为停用 |
| `enabled` | `configured` | 已加载未启动（启动中或异常） |

### 4.3 写入

**请求**：

```http
POST /api/v1/points/plc-01/write
Content-Type: application/json
X-Idempotency-Key: 0b6f...

{
  "writes": [
    { "point": "motor.speed", "value": { "type": "float32", "num": 1500 }, "confirm": true }
  ]
}
```

**响应**：

```json
{
  "results": [
    { "point": "motor.speed", "ok": true, "quality": "good", "appliedAt": "2026-11-04T08:31:02.100Z" }
  ]
}
```

| 规则 | 说明 |
|---|---|
| 权限 | 需要 `point.write` 权限；只读点返回 `point.not_writable` |
| 幂等 | `X-Idempotency-Key` 在 5 分钟内重复提交返回首次结果 |
| 确认 | `confirm: true` 时网关回读一次，不一致返回 `uncertain`。比较的是**补齐类型后**的写入值：`value.type` 可省略，由点位 `dataType` 补齐；浮点按 float32/float64 的相对容差比较（写 0.1 读回 0.10000000149 属正常二次舍入），整数、布尔与字符串严格相等 |
| 批量 | 逐项结果；部分失败返回 200 + 失败项（不用 500） |
| 审计 | 每次写入记录：谁、何时、点位、旧值、新值、结果 |

**寄存器内位写（读-改-写）**：点位是 `dataType=bool` 且地址指向保持寄存器
（`area=holding`）并给出 `address.bit`（0..15）时，网关执行「读整字 → 只改
目标位 → 写整字」。因此**不会**影响同一寄存器的其它位，也允许同一寄存器上
挂多个可写布尔点。

| 情况 | 行为 |
|---|---|
| `holding` + `bit 0..15` | 读-改-写；读失败则不写，返回该项失败 |
| `holding` 但缺 `bit` | 拒绝（`driver.write_failed`，消息要求 `address.bit`）：不会猜 0/1 覆盖整字 |
| `coil` | 直接写线圈（FC5），`bit` 不参与（整位区没有「子位」） |
| `input` / `discrete` | 只读区，返回 `driver.write_failed`，消息为 `area "input" is read-only`；不会写到 holding 的同一偏移 |

原子性边界：网关保证**同一设备**的写入串行（两个位写不会互相吞掉）；但同一
串口总线上的另一台设备、或另一个主站直连同一从站，若写同一个 `unit + 寄存器`，
仍可能插在「读」和「写」之间。一个寄存器只应有一个所有者，这是现场配置约束，
不是网关能替用户消解的。

### 4.4 配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/config` | 当前生效配置（脱敏） |
| POST | `/api/v1/config/validate` | 校验配置（不应用） |
| POST | `/api/v1/config/apply` | 应用配置（生成新版本） |
| GET | `/api/v1/config/versions` | 版本列表（时间倒序） |
| GET | `/api/v1/config/versions/{id}` | 指定版本内容 |
| GET | `/api/v1/config/diff?from=&to=` | 版本差异 |
| POST | `/api/v1/config/rollback` | 回滚到指定版本 |
| GET | `/api/v1/config/staged` | 灰度发布进度 |
| POST | `/api/v1/config/staged` | 开始灰度发布（`{"batchSize":N,"observe":"30s","maxFailureRate":0.2,"config":{...}}`） |
| POST | `/api/v1/config/staged/advance` | 下发下一批（观察窗口内被拒） |
| POST | `/api/v1/config/staged/abort` | 终止灰度并回滚到灰度前版本 |

**灰度发布**：`POST /api/v1/config/staged` 接收完整目标配置与推进策略，按设备分批
下发；每批只改动该批设备，其余设备保持当前生效配置（`reload` 按指纹只重建变化设备）。
被删除的设备不属于任何批次，在最后一批之后一次性落地。

- `batchSize`：每批设备数（`<=0` 归一为 1）。
- `observe`：每批下发后的观察窗口（如 `"30s"`）。窗口内 `advance` 返回
  `409 config.staged_observing`，即使健康指标正常也不允许提前推进——观察窗口的意义
  是「给问题留出暴露时间」，不是「等到不健康才停」。
- `maxFailureRate`：观察窗口结束时**已下发设备**的采集失败率上限（0~1）。超阈值时
  `advance` 返回 `409 config.staged_auto_aborted`，并自动回滚到 `baseVersion`。看门狗
  每秒检查一次，因此即使没有人调用 `advance`，窗口结束时也会自动回滚（自动暂停的
  强形式：暂停而不回滚会让坏配置继续跑，因此这里直接回滚）。
  失败率按**观察窗口内的增量**计算：每台设备以「本批次下发时刻」的累计读统计为基线，
  只统计基线之后新采上来的样本，而不是设备全生命周期的累计失败率。这一点很关键——用
  全生命周期口径时，一台历史上就 flaky 的设备（无论新配置多健康）会在第一批就被累计
  失败率顶过阈值、永远无法完成灰度；反过来，长期干净的设备即使新批次把链路打挂，也会
  被巨大的历史分母稀释而迟迟不触发。窗口内还没采到样本时失败率记为 0（不当作故障）。

响应体与 `GET /api/v1/config/staged` 同构：

```json
{
  "active": true, "completed": false, "aborted": false,
  "baseVersion": "sha256:...", "targetVersion": "sha256:...",
  "totalBatches": 3, "nextBatch": 1,
  "batches": [["dev-01","dev-02"], ["dev-03"], ["dev-04"]],
  "devices": ["dev-01","dev-02","dev-03","dev-04"],
  "observing": true, "observe": "30s", "observeUntil": "2026-11-24T10:00:30Z",
  "failureRate": 0.01, "maxFailureRate": 0.2, "pausedReason": "",
  "startedAt": "...", "updatedAt": "..."
}
```

同一时刻只允许一个灰度（重复开始返回 `config.staged_in_progress`）；`advance` 逐批推进，
末批后自动收敛到目标并置 `completed=true`；`abort` 回滚到 `baseVersion`。
自动回滚后 `aborted=true` 且 `pausedReason` 说明原因。

**应用配置的响应**：

```json
{
  "version": "sha256:9f2c...",
  "applied": true,
  "changed": { "devicesAdded": 2, "devicesModified": 1, "devicesRemoved": 0,
               "pointsAdded": 40, "pointsModified": 3, "pointsRemoved": 1 },
  "restartedDevices": ["plc-01"],
  "durationMs": 1840
}
```

**`***` 占位符**：GET 出来的配置里，`token`/`tokenSha256`/`password`/`secret`
类字段是 `***`。`validate` 与 `apply` 都把它理解为「保持当前值不变」，
因此「读出配置 → 改一个点位 → 写回去」是安全且可用的路径。占位符出现在当前
配置里不存在的字段上会返回 `config.invalid`，而不是猜一个值。

数组按 `id`/`name` 匹配而不是按下标还原——删掉或重排 `server.users` 时，
按下标会把 A 的凭据安到 B 头上，且不会报错。

**回滚语义**：

| 规则 | 说明 |
|---|---|
| 回滚 = 应用旧版本 | 产生一个新版本（内容等于旧版本），历史不删除 |
| 前置条件 | 目标版本必须存在且通过当前 Schema 校验 |
| 失败 | 自动恢复到回滚前状态，`rollbackCount++` |
| 权限 | 需要 `config.rollback` |

### 4.5 命令（白名单）

```http
POST /api/v1/commands
{ "action": "driver.restart", "params": { "device": "plc-01" } }
```

| action | 参数 | 所需权限 | 说明 |
|---|---|---|---|
| `driver.restart` | `device` | `command` | 重启单设备采集 |
| `device.selftest` | `device` | `command` | 读 1 个点验证连通性 |
| `outbox.flush` | `sink?` | `command` | 手动触发补发 |
| `outbox.drop` | `sink`, `before` | `command.danger` | 丢弃指定时间前消息（不可逆） |
| `config.rollback` | `version` | `command.danger` | 同 4.4 |
| `alarm.ack` | `ids` 或 `all`, `by` | `command` | 同 4.7 的确认语义 |
| `log.level` | `level` | `command.danger` | 动态调整日志级别（会掩盖问题） |
| `diag.bundle` | — | `command` | 生成诊断包 |

**规则**：未知 action 返回 `request.unknown_action`；所有命令写审计（成功与
失败都写）；高危命令需参数 `"confirm": true`。

**两级权限判定**：路由级只要求 `command`（因为动作在请求体里），handler 解析
出 `action` 后再按上表二次判定。这样新增一个高危命令时，漏改路由也不会
让 operator 拿到它——判定表 `rbac.CommandPermission` 与命令白名单放在同一个
仓库里评审。

### 4.6 诊断

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/diagnostics/bundle` | 下载诊断包（zip） |
| GET | `/api/v1/diagnostics/probe` | 网络端点连通性探测（TCP 握手 + RTT） |
| GET | `/api/v1/logs?device=&level=&limit=` | 查询日志（尾部） |

#### 4.6.1 网络探测 `GET /api/v1/diagnostics/probe`

回答「为什么连不上」的**第一个分叉**：设备显示连接失败时，先区分故障层。

- TCP 连不上 → 路由 / 端口 / 防火墙 / DNS 问题；
- TCP 连得上但驱动读不到 → 协议参数或设备应用层问题。

这一分叉以前只能靠人 `telnet host port` 手工判断；本端点把它变成一次可审计的
调用。它和驱动层的 `Conn.Probe` 分工明确：`Conn.Probe` 走真实协议（握手、读一个
点），回答「设备应用层是否正常」；本端点只做 TCP 握手或 UDP 地址解析，**不发送
任何协议字节**。

**参数**：

| 参数 | 说明 |
|---|---|
| `channel` | 可选，可重复（`?channel=a&channel=b`）或逗号分隔（`?channel=a,b`） |
| `device` | 可选，同 `channel` 的多值/逗号写法；把设备解析到其所在通道后探测 |
| `timeout` | 可选，单端点超时，默认 `3s`，夹取到 `[100ms,10s]` |

`channel` 与 `device` 都省略时探测配置里全部通道；两者同时给出时合并去重探测。
网络可达性属于**通道**：同一条线上的多台设备只探一次。任何 `channel`/`device`
在配置里不存在都返回 `400 probe.unknown_channel`——静默少探会让「都正常」的假象
掩盖真实故障。

**响应**：

```json
{
  "generatedAt": "2026-03-01T12:00:00Z",
  "total": 1,
  "results": [
    {
      "channel": "line-1",
      "protocol": "modbus-tcp",
      "transport": "tcp",
      "address": "10.0.0.5:502",
      "status": "ok",
      "reachable": true,
      "rttMs": 3
    }
  ],
  "summary": {"ok": 1, "fail": 0, "unverified": 0, "unsupported": 0}
}
```

**`status` 语义**（刻意区分「失败」与「无法判定」）：

| status | 含义 | `reachable` |
|---|---|---|
| `ok` | TCP 握手成功 | `true` |
| `fail` | TCP 连接失败（拒绝/超时/不可达） | `false` |
| `unverified` | 探测手段无法判定（UDP 无连接握手） | `false` |
| `unsupported` | 传输不是网络端点（串口 / 内存） | `false` |

`unverified` 单列而不并入 `reachable`：UDP connected 拨号成功只说明地址可解析、
本地套接字可建，并不代表对端在线。把它标成可达会让看板误判。

**安全边界**：只探测**配置内已声明**的通道/设备，不接受任意主机——因此不存在
SSRF 面。权限与诊断包同级（`diagnostics`，operator/admin 有、viewer 无）。探测会
短暂地多开一条 TCP 连接（连上立即关闭），等价于运维手动 `telnet host port`：
对只允许单会话的设备（串口服务器、某些 PLC）有瞬时并发风险，但代价有界且由人
主动触发，故可接受。地址缺端口直接报错而不猜端口——往错误端口探测会把「配置
写错」误报成「网络不可达」。

诊断包内容：

```
manifest.json        版本/时间/站点/配置哈希
config.redacted.json 脱敏配置（口令→***）
metrics.txt          指标快照
logs/uic.log         最近 5000 行
health.json          健康快照（含报警计数）
devices.json         设备状态与统计
alarms.json          报警规则 + 当前状态 + 计数
history.json         历史库水位 + 采样策略 + 目录（启用历史时）
outbox.json          待投递队列水位（启用北向时）
```

### 4.7 报警

> **状态**：已实现（`internal/alarm` + `internal/runtime/alarm.go`）。

报警不是「点位值超过阈值」的同义词，而是**有生命周期、需要人确认**的对象。
把它做成一串无状态的事件，会让「谁在什么时候处理了哪一条」永久丢失；做成
一个布尔值，则无法区分「正在报警」与「已经恢复但没人看过」。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/alarms` | 列表（`state`/`severity`/`q`/`limit` 过滤） |
| GET | `/api/v1/alarms/{id}` | 单条当前状态 |
| POST | `/api/v1/alarms/{id}/ack` | 确认单条 |
| POST | `/api/v1/alarms/ack` | 批量确认 / 全部确认 |
| GET | `/api/v1/alarms/stream` | SSE 状态迁移流（见 5.4） |

**列表响应**：

```json
{
  "items": [
    {
      "id": "high-power",
      "name": "功率过高",
      "severity": "warning",
      "message": "压机功率超过 70kW 持续 2 秒，检查液压系统",
      "state": "active",
      "active": true,
      "pending": false,
      "acknowledged": false,
      "count": 3,
      "value": 73.4,
      "valueKnown": true,
      "quality": "good",
      "since": "2026-11-05T10:40:48.534Z",
      "updatedAt": "2026-11-05T10:41:06.736Z",
      "ackAt": null,
      "ackBy": "",
      "clearedAt": null,
      "reason": ""
    }
  ],
  "total": 1,
  "counts": { "total": 5, "normal": 3, "pending": 0, "active": 1, "acked": 1, "cleared": 0, "unknown": 0 }
}
```

| 过滤参数 | 取值 | 说明 |
|---|---|---|
| `state` | `normal,pending,active,acked,cleared` | 逗号分隔；未知值返回 400，**不静默忽略** |
| `severity` | `info,warning,critical` | 同上 |
| `q` | 子串 | 匹配 ID / 名称 / 消息，不区分大小写 |
| `limit` | 正整数 | 0 表示不限制 |

无条件返回**全部规则**（含 `normal`），不是只返回正在报警的：否则「没有报警」
与「规则没加载」在响应里长得一模一样。

**排序**：严重度降序 → 激活时间升序 → ID。这就是运维的处理顺序。

**`reason`**：只有输入不可信时才非空（如 `input src:power: input quality is timeout`）。
它不是错误码，是给人看的一句话——「为什么这条规则现在判不出来」。

`count` 是累计触发次数（不清零）；`since` 是**最近一次进入 active** 的时刻，
不是进入当前状态的时刻：运维关心的是「这个故障从什么时候开始的」。

#### 4.7.1 状态机

```
                 条件成立且满足 onDelay
      normal ───────────────────────────► active ◄──┐
        ▲                                  │        │ 条件再次成立
        │ 已确认过 / autoAck                │ ack    │
        │                                  ▼        │
        │                               acked ──────┘
        │                                  │
        │ 无条件 / 未经 onDelay             │ 条件恢复且满足 offDelay
        ▼                                  ▼
     pending ──条件消失──► normal        cleared
        ▲                                  │
        └──────── 条件再次成立 ◄────────────┘  ack
```

| 状态 | 需要人处理 | 可确认 | 含义 |
|---|---|---|---|
| `normal` | 否 | 否（409） | 条件不成立 |
| `pending` | 否 | 否（409） | 条件成立，但还在 `onDelay` 等待期内 |
| `active` | **是** | 是 | 正在报警，未确认 |
| `acked` | 否 | 是（幂等） | 已确认，条件可能仍然成立 |
| `cleared` | **是** | 是 | 条件已恢复，但这次报警还没人看过 |

三条容易被忽略、但必须写进契约的规则：

1. **`cleared` 与 `active` 都需要确认。** 只对「正在报警」要求确认，会在交接班
   时丢掉刚刚恢复的故障——而那往往是最需要复盘的。
2. **`pending` 不可确认。** 还没成为报警就确认，等于允许调用方「提前消音」，
   随后真正的报警会被误认为已处理。
3. **`acked` 的确认是幂等的。** 客户端重试返回 200，不覆盖首次的 `ackBy`/`ackAt`。

#### 4.7.2 判定语义

规则二选一（同时配置在配置期被拒绝）：

```json
{ "id": "high-power", "point": "press:power", "high": 70, "hysteresis": 5,
  "onDelay": "2s", "offDelay": "3s", "severity": "warning" }
```

```json
{ "id": "mismatch", "expr": "press:power > 50 && press:rate > 50",
  "onDelay": "500ms", "severity": "critical" }
```

| 概念 | 语义 | 为什么这样定义 |
|---|---|---|
| `high` / `low` | 严格不等号触发（`v > high`、`v < low`） | 等于阈值不是越限，否则「刚好到达上限」会一直报警 |
| `hysteresis` | 恢复阈值 = 触发阈值 ∓ 滞回量 | 值在阈值附近抖动时不会产生 raised/cleared 风暴 |
| `onDelay` | **连续**满足多久才报警 | 断续抖动不该攒够时间就报警 |
| `offDelay` | **连续**不满足多久才清除 | 同上；给「瞬时恢复又复发」一个观察窗口 |
| 双侧阈值 | 任一侧越限触发，**仍在任一侧滞回带内**就维持 | 高侧的滞回带与低侧的滞回带分别判定，二者不重叠（配置期要求 `high - low > 2 × hysteresis`） |
| 表达式 | 复用计算点的 `calc` 引擎，必须是布尔表达式 | 同一套词法/类型检查，避免第二套「迷你语言」 |

**质量门控**（这是报警最容易被做错的地方）：

| 输入质量 | 行为 |
|---|---|
| `good` / `uncertain` | 正常判定（`uncertain` 表示值可信但可能不新鲜） |
| `stale` / `timeout` / `not_connected` / `comm_error` / `address_error` | **不改变状态机**，只更新 `valueKnown=false`、`quality`、`reason`；`pending` 被取消 |

数据断了既不能当成「正常」（会掩盖真实故障），也不能当成「报警」（一次断线
就是满屏假报警）。已经 `active`/`acked` 的报警保持原状：**看不见不等于好了**。

布尔表达式做短路求值，未被求值的引用不参与质量判定：`false && dead:x > 0` 的
结果确实是 `false`，不属于「未知」。

#### 4.7.3 确认

```http
POST /api/v1/alarms/high-power/ack
{ "by": "operator-1" }
```

```http
POST /api/v1/alarms/ack
{ "ids": ["high-power", "low-flow"], "by": "operator-1" }
```

```json
{ "acknowledged": [ { "id": "high-power", "state": "acked", "ackBy": "operator-1" } ], "total": 1 }
```

| 场景 | 结果 |
|---|---|
| `active` / `cleared` | 200，转 `acked` / `normal`，发 `acknowledged` 事件 |
| `acked` | 200（幂等），不发事件，不覆盖首次确认人 |
| `normal` / `pending` | 409 `alarm.not_active` |
| ID 不存在 | 404 `alarm.not_found` |
| 批量里混入不可确认项 | 409，**整批不生效**（先整体校验再应用） |
| `all: true` 与 `ids` 同时出现 | 400 `request.invalid` |

批量操作必须原子：半完成的结果会让调用方无法判断该重试哪一条。`all: true`
则相反——静默跳过不可确认项，因为「全部确认」的语义就是「能确认的都确认」。

`by` 由调用方提供，不从 Token 推导：Token 是共享的接入凭据，不等同于
「谁按下了确认」。为空表示匿名；上限 64 字节；会进审计日志与 `ackBy`。

#### 4.7.4 配置与规则变更

```json
{
  "alarms": [
    {
      "id": "high-power",
      "name": "功率过高",
      "severity": "warning",
      "message": "压机功率超过 70kW 持续 2 秒",
      "point": "press:power",
      "high": 70,
      "hysteresis": 5,
      "onDelay": "2s",
      "offDelay": "3s"
    },
    {
      "id": "spike",
      "point": "press:power",
      "high": 84,
      "hysteresis": 2,
      "autoAck": true
    },
    {
      "id": "maintenance",
      "point": "press:power",
      "high": 60,
      "disabled": true
    }
  ]
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `severity` | `warning` | `info` / `warning` / `critical` |
| `disabled` | `false` | **默认启用**——与设备相反：漏写 `enabled: true` 的设备会表现为「没数据」，而漏写的报警会表现为「一切正常」 |
| `autoAck` | `false` | 恢复即归位，只适用于纯提示类报警 |
| `onDelay` / `offDelay` | `0` | 上限 24h（48h 的延时几乎一定是把秒写成了小时） |
| `hysteresis` | `0` | 双侧阈值要求 `high - low > 2 × hysteresis` |

配置期静态校验：规则自洽、ID 唯一、引用的 `device:point` 存在、点位类型可比较
（`bool` / `string` / `bytes` 一律拒绝）。这些错误等到运行时才发现，代价是
一条永远 `unknown` 的报警——而没人会去看一条永远不响的报警。

**规则变更的热更新语义**：只有「影响判定结果」的字段变化才重置状态（严重度、
点位、阈值、滞回、延时、`autoAck`、`disabled`）；改 `name` / `message` 不会把
一条正在报警的记录打成 `normal`。把一条正在报警的规则停用同样会重置并发
`reset`——否则 retained 主题上会永远挂着一个既不能确认也不会恢复的 `active`
报警。重置时发 `reset` 事件，删除规则时发 `removed` 事件——
消费方必须能区分「故障恢复」与「规则被改掉」。

#### 4.7.5 报警相关的健康与指标

`GET /api/v1/health` 增加：

```json
{ "alarms": { "total": 5, "normal": 3, "pending": 0, "active": 1, "acked": 1, "cleared": 0, "unknown": 0 } }
```

`unknown` 统计最近一次评估时输入不可信的规则数。它不进 `status`（网关本身是
健康的），但必须可见：一片 `unknown` 说明数据链路出了问题，而不是设备都正常。

指标：

```
uic_alarm_events_total{type,severity}   状态迁移次数
uic_alarm_active{severity}              当前未恢复的报警数（active + acked）
uic_alarm_pending                       等待 onDelay 的规则数
uic_alarm_cleared                       已恢复未确认的规则数
uic_alarm_unknown                       输入不可信的规则数
uic_alarm_rules                         已加载规则数
uic_mqtt_publish_total{result="alarm"}  报警 retained 发布
```

---

### 4.8 历史查询

> **状态**：已实现（`internal/history` + `internal/runtime/history.go`）。

历史库是**有界的采样日志**，不是时序数据库：没有索引、没有压缩、没有跨点位
join、没有 SQL。它解决的是「昨天下午三点那台泵的功率是多少」这一类一线问题，
而不是「对 10 亿点做多维度下钻」。这个边界必须写在接口上，否则它一定会被当成
TSDB 用，然后在数据量上来之后变成一次事故。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/history?device=&point=&from=&to=&step=&agg=&limit=` | 查询单个点位的原始采样或聚合结果 |

#### 4.8.1 参数

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `device` | 是 | — | 设备 ID |
| `point` | 是 | — | 点位 ID |
| `from` | 否 | `to - 1h` | 起始时刻（含） |
| `to` | 否 | 服务器当前时间 | 结束时刻（含） |
| `step` | 否 | 0（原始点） | 聚合步长，如 `10s` / `1m` / `1h`；必须 > 0 |
| `agg` | 否 | `last` | 仅在有 `step` 时有效：`last` / `first` / `avg` / `min` / `max` / `count` |
| `limit` | 否 | 1000 | 返回条数上限，最大 10000 |

`from` / `to` 接受三种写法：

```
now               服务器当前时间
-1h  /  +30m      相对服务器当前时间（运维最常用的写法）
2026-03-01T12:00:00Z   RFC3339 绝对时刻
```

**区间是左闭右闭的**：`from` 与 `to` 正好命中记录时必须包含。这一点与很多
时序库的「左闭右开」相反，因为这里的查询是「给我看这两分钟内发生了什么」，
两端都是人指定的边界，丢掉边界上的点会让人怀疑数据丢了。

#### 4.8.2 原始查询

```bash
curl -s 'localhost:8092/api/v1/history?device=press&point=power&from=-5m&limit=2'
```

```json
{
  "device": "press",
  "point": "power",
  "from": "2026-03-01T11:55:00Z",
  "to": "2026-03-01T12:00:00Z",
  "step": "0s",
  "agg": "last",
  "points": [
    { "time": "2026-03-01T11:59:58.203Z",
      "value": { "type": "float64", "num": 73.4 }, "quality": "good" },
    { "time": "2026-03-01T11:59:59.204Z",
      "value": { "type": "float64", "num": 73.9 }, "quality": "good" }
  ],
  "count": 2,
  "scanned": 812,
  "skipped": 0
}
```

- `points` 按时间**升序**。写入端可能乱序（时钟回拨、补写历史），查询端负责
  排序，消费方不必再排一次。
- `scanned` / `skipped` 是诊断量：扫了多少行、跳过了多少损坏行。把它们放进
  响应而不是只写日志，是因为现场很难区分「查询变慢」与「文件损坏」。
- 没有数据时返回 `200` + `count: 0`，**不返回 404**。「这段时间没有数据」是
  一个正常答案；404 会让人以为点位不存在。
- 超过 `limit` 时返回前 `limit` 条并置 `truncated: true`。

#### 4.8.3 聚合查询

```bash
curl -s 'localhost:8092/api/v1/history?device=press&point=power&from=-1h&step=1m&agg=avg'
```

```json
{
  "device": "press", "point": "power",
  "from": "2026-03-01T11:00:00Z", "to": "2026-03-01T12:00:00Z",
  "step": "1m0s", "agg": "avg",
  "buckets": [
    { "time": "2026-03-01T11:00:00Z", "count": 12,
      "first": { "type": "float64", "num": 71.2 },
      "last":  { "type": "float64", "num": 74.8 },
      "min": 70.9, "max": 75.1, "avg": 73.05, "quality": "good" }
  ],
  "count": 1, "scanned": 812, "skipped": 0
}
```

- 桶的 `time` 是**桶起点**（对齐到 `from + k*step`），不是桶内第一条记录的时间。
  这样同一份查询在不同时间重跑，桶边界保持不变。
- `min` / `max` / `avg` 只对**数值类型**计算；文本、`bytes` 与 `bool` 点位这三项
  是 `null`。把开关量平均成 `0.37`、把文本平均成 `0` 都是编造出来的数字。
- 桶的 `quality` 取桶内**最差**的质量码，而不是最后一条。一个桶里只要有一条
  `comm_error`，它的均值就不可信；标成 `good` 等于让前端把坏数据画成好数据。
- 一次查询的桶数上限是 5000（`MaxBuckets`）。超限返回 `400` 并提示「加大 step
  或缩小范围」，而不是先扫完再说——那会在低配盒子上直接 OOM。
- `count` 是返回的桶数；`limit` 同样约束桶数，超出时置 `truncated`。

#### 4.8.4 错误码

| HTTP | code | 触发条件 |
|---|---|---|
| 400 | `history.invalid_range` | `from` 不早于 `to`，或时间格式无法解析 |
| 400 | `history.invalid_query` | `step` / `agg` 非法、桶数超限、只有 `agg` 没有 `step` |
| 400 | `request.invalid` | 缺 `device` / `point`，或 `limit` 不是正整数 |
| 409 | `history.disabled` | 配置里 `storage.history.enabled=false` |
| 503 | `history.unavailable` | 历史库已关闭（进程正在停止） |

`history.disabled` 必须是 `409` 而不是「返回空结果」。空结果会被读成「这段时间
没有数据」，而真实原因是「这台网关根本没开历史」——两者的处置方式完全不同。

#### 4.8.5 存储语义（对使用方的承诺）

| 项 | 行为 |
|---|---|
| 采样 | 事件驱动 + 心跳：值变化超过 `deadband` 就记，没变化也至少每 `heartbeat` 记一条 |
| 最小间隔 | 同一测点两次落盘至少间隔 `minInterval`（默认 1s）；被挡下的变化会以「最新值」在到期后补写 |
| 质量 | 质量码变化也会落一条（坏数据的起点是最需要查的信息）；`disabled` 点位不记录 |
| 保留 | 按总字节数（`maxBytes`）与保留期（`maxAge`）**整段淘汰**最旧的数据，永远保留至少一个段 |
| 落盘 | 每 200ms 刷一次用户态缓冲；进程崩溃最多丢 200ms；不做每条 fsync（SD 卡寿命） |
| 重启 | 重开目录后继续追加；末尾半行会被识别并另起一段，不会把新记录拼进坏行 |
| 时区 | 所有对外时间戳都是 UTC（RFC3339），与 `Sample` 侧保持一致 |

**明确不支持**：

- 跨点位聚合 / join（一条查询只服务一个 `device+point`）；
- 任意时间跨度的全量导出（用 `limit` + 分段查询，或直接拷段文件）；
- 删除单条记录（保留策略只按段淘汰）；
- 采样策略按点位覆盖（全局一套；需要区别对待时用 `deadband` 与停用点位表达）。

#### 4.8.6 配置

```json
{
  "storage": {
    "dir": "./data",
    "history": {
      "enabled": true,
      "dir": "./data/history",
      "maxBytes": 268435456,
      "maxAge": "168h0m0s",
      "segmentBytes": 16777216,
      "minInterval": "1s",
      "heartbeat": "5m0s",
      "deadband": 0
    }
  }
}
```

- `heartbeat` 未配置时默认 `5m`；显式写 `"0s"` 表示**不补点**（长时间稳定的
  点位在图上会是一段空白，这是有意的取舍）。
- `dir` 默认是 `<storage.dir>/history`。
- 改 `dir` / `maxBytes` / `segmentBytes` / `maxAge` **需要重启**（存储打开时
  就固定了），热更新只对 `minInterval` / `heartbeat` / `deadband` 生效；配置
  重载时会打一条 `history_config_changed` 警告说明这一点。
- `health.history` 暴露 `records` / `bytes` / `segments` / `oldest` / `newest` /
  `evicted` / `writeErrors`；指标为 `uic_history_bytes`、`uic_history_segments`、
  `uic_history_records_total`、`uic_history_evicted_total`、
  `uic_history_write_errors_total`、`uic_history_query_total{result}`、
  `uic_history_window_total{result}`（`ok` / `empty` / `unavailable`，窗口聚合专用）。

### 4.9 认证与授权

#### 4.9.1 为什么是静态 Token 而不是登录会话

Wave 1 的目标是「边缘网关 + 少量运维客户端」，不是面向终端用户的门户。登录
会话会引入口令存储、重置流程、会话存储与过期清理——四块都需要持久化与运维
投入，却不会让一个只有三类角色的系统更安全。静态 Token 的边界很明确：

- 每个 Token 独立可撤销（改配置即可，**下一条请求立即生效**）；
- 配置文件可只存 `tokenSha256`，泄露配置不等于泄露凭据；
- 不需要在网关上存任何口令哈希表之外的东西。

代价是 Token 不会过期、不能自助轮换。因此 `uic token` 负责生成，轮换 = 生成
新 Token → 双写过渡 → 删除旧项 → apply。

#### 4.9.2 `GET /api/v1/auth/me`

无需凭据（否则无法探测「是否需要凭据」）。它只回答「**我**是谁」，不列举
其他账号。

```json
{
  "user": "ops",
  "role": "operator",
  "open": false,
  "authenticated": true,
  "permissions": ["alarm.ack", "command", "config.read", "diagnostics", "point.write", "read"]
}
```

| 情形 | 响应 |
|---|---|
| 携带有效 Token | `authenticated: true` + 该用户的 `user`/`role`/`permissions` |
| 未带或带错 Token（认证开启） | `200` + `authenticated: false`、`user`/`role` 为空 |
| 开放模式 | `open: true`、`user: "anonymous"`、`role: "admin"`、`authenticated: false` |

未认证返回 `200` 而不是 `401`：调用方问的是「认证模式」，不是「我能进吗」——
后者由真正的业务端点回答。客户端据此决定是否显示「写入」按钮，但不能把它当作
授权依据——服务端每次请求都重新判定。

#### 4.9.3 `GET /api/v1/auth/users`

需要 `audit.read`（admin）。只返回 `user`/`role`，**永不返回 Token 或哈希**。

```json
{ "items": [ {"user":"ops","role":"operator"}, {"user":"root","role":"admin"} ], "open": false }
```

#### 4.9.4 权限判定的顺序

1. 解析 `Authorization`；开放模式下直接视为 `anonymous/admin`。
2. 无 Token 或 Token 不匹配 → `401 auth.unauthorized`（审计 `result=error`）。
3. 路由级权限不足 → `403 auth.forbidden`（审计 `result=denied`）。
4. handler 内的细粒度权限（高危命令、写入逐项校验）再判一次。

第 2 步与第 3 步在审计里刻意区分：401 多半是扫描器或配置错误，403 才是「有
身份的人试图越权」，后者才需要人看。

### 4.10 审计

#### 4.10.1 记录什么

| 类别 | `action` | 触发点 |
|---|---|---|
| 认证 | `auth.unauthorized` | Token 缺失/错误 |
| 授权 | `auth.forbidden` | 角色缺少所需权限 |
| 写入 | `point.write` | 每次写入的**每个点位**各一条 |
| 命令 | `command.<action>` | 成功、失败、未知 action |
| 报警 | `alarm.ack` | 单条/批量/全部确认 |
| 配置 | `config.apply` / `config.rollback` | 成功、被拒绝、内容未变 |

一条记录的字段：

```json
{
  "seq": 1024,
  "time": "2026-11-06T04:30:00.123Z",
  "user": "ops",
  "role": "operator",
  "action": "point.write",
  "target": "plc-01/setpoint",
  "result": "ok",
  "code": "",
  "message": "",
  "remote": "10.0.0.8:51422",
  "requestId": "req-...",
  "detail": { "device": "plc-01", "point": "setpoint", "oldValue": "40", "value": "42", "via": "api" },
  "prev": "…",
  "hash": "…"
}
```

`result` 三态：`ok` / `denied` / `error`。`detail` 只允许字符串键值（可检索、
可截断）；`via` 自动标注来源：`api` / `modbus-slave` / `opcua-server`。

**写入逐项记录**：一次批量写一半成功一半被拒绝时，只记一条汇总无法回答事后
最重要的问题——到底哪个点没写进去。

#### 4.10.2 完整性

审计是 NDJSON 分段文件 + **SHA-256 哈希链**：

- `seq` 连续递增，重启后从上一段尾部续号；
- `hash = SHA-256(规范化字段 + prev)`，字段边界用「长度:内容」编码，避免
  值里塞分隔符构造碰撞；
- 整段淘汰（保留策略）后链仍可从新起点校验；
- 尾部半行（掉电）不参与校验，新记录写入新段。

```
GET /api/v1/audit/verify
{ "status": "ok", "checked": 1024, "brokenAt": 0, "reason": "" }
```

被篡改或整条删除时 `brokenAt` 指向第一条不一致的记录，`reason` 说明是
`hash mismatch` 还是 `seq gap`。

#### 4.10.3 查询

```http
GET /api/v1/audit?user=ops&action=point.write&result=error&from=...&to=...&q=setpoint&limit=100&cursor=o1200
```

| 参数 | 说明 |
|---|---|
| `user` `role` `action` `result` | 精确匹配 |
| `from` `to` | RFC 3339 时间窗 |
| `q` | 子串匹配 user/action/target/message/code |
| `limit` | 默认 100，上限 1000 |
| `cursor` | 不透明游标（`o<offset>`），只保证「翻页不漏」 |

结果按时间**倒序**（最新在前），`scanned` 报告本次实际扫描的记录数——它和
`total` 的差距是「过滤发生在读出之后」的诚实说明。

#### 4.10.4 存储与保留

| 配置 | 默认 | 说明 |
|---|---|---|
| `storage.audit.enabled` | `true` | 显式 `false` 才关闭 |
| `storage.audit.dir` | `<storage.dir>/audit` | 与历史库分开目录 |
| `storage.audit.maxBytes` | 64 MiB | 总量上限，整段淘汰 |
| `storage.audit.segmentBytes` | 8 MiB | 单段上限 |
| `storage.audit.maxAge` | 90d | 按时间淘汰 |

#### 4.10.5 水位

```http
GET /api/v1/audit/stats
{ "enabled": true, "records": 1024, "appended": 12, "evicted": 3, "bytes": 20480,
  "maxBytes": 67108864, "segments": 1, "lastSeq": 1024,
  "oldest": "…", "newest": "…", "writeErrors": 0 }
```

| 字段 | 语义 |
|---|---|
| `records` | 磁盘上**当前保留**的条数（含重启前写入的；淘汰整段会同步减少） |
| `appended` | 本进程启动以来写入的条数 |
| `evicted` | 本进程淘汰的整段数 |
| `lastSeq` | 最后一条记录的序号；重启后从磁盘续号 |
| `writeErrors` | 本进程写入失败次数（对应 `uic_audit_write_errors_total`） |

区分「审计停了」和「刚重启」要看这两个字段的配合：`records` 是磁盘上的
存量事实（重启后不归零），`appended` 才是本次进程的活动量。判断是否停摆应
盯 `appended` / `writeErrors` 是否还在变化，而不是 `records` 的绝对值。

**审计写入失败不阻断业务**：写不进去时记 ERROR 日志并累加
`uic_audit_write_errors_total`，接口照常返回。反过来（因为磁盘满而拒绝写
点位）会让产线停摆——这是明确的取舍，指标与告警负责让人发现。

审计关闭时 `/api/v1/audit*` 返回 `409 audit.disabled`，而不是空列表：空列表
会让调用方以为「查过了，没有记录」。

---

## 5. SSE 实时推送

### 5.1 连接

```http
GET /api/v1/stream?points=plc-01/temp,plc-01/pressure
Accept: text/event-stream
Authorization: Bearer <token>
```

| 参数 | 说明 |
|---|---|
| `points` | 逗号分隔；支持通配 `plc-01/*`、`*/temp`；为空表示全部 |
| `quality` | 只推送指定质量码（默认全部） |
| `heartbeat` | 心跳间隔（默认 15s） |

### 5.2 事件

```
event: hello
data: {"server":"uic","apiVersion":1,"heartbeatSeconds":15}

event: sample
data: {"device":"plc-01","point":"temp","value":{"type":"float32","num":23.5},"quality":"good","collectedAt":"...","seq":84214}

event: quality
data: {"device":"plc-01","point":"temp","from":"good","to":"timeout","at":"..."}

event: heartbeat
data: {"ts":"..."}
```

### 5.3 行为约定

| 场景 | 行为 |
|---|---|
| 慢客户端 | 每连接有界缓冲（默认 1000 事件）；满则丢最旧并计数 |
| 断线 | 客户端用 `Last-Event-ID` 重连；服务端补发缓冲区内事件（尽力） |
| 无变化 | 只发心跳（不重复推相同值，除非质量变化） |
| 认证失败 | 返回 401，不建立流 |

**`id:` 字段**：使用 `device:point:seq`，用于 `Last-Event-ID`。

### 5.4 报警事件流

```http
GET /api/v1/alarms/stream?heartbeat=15s
Accept: text/event-stream
```

与 `/api/v1/stream`（点位的值）**分开**：消费方完全不同——点位流是趋势图和
实时值，报警流是弹窗、声音与确认面板。合成一路会让前端在一堆采样里筛选报警，
也会让报警被高频数据挤掉。

```
event: hello
data: {"server":"uic","apiVersion":1,"heartbeatSeconds":15,"counts":{"total":5,"active":1,"cleared":0}}

event: raised
id: high-power
data: {"type":"raised","at":"...","from":"normal","status":{ ...完整 Status... }}

event: acknowledged
id: high-power
data: {"type":"acknowledged","at":"...","from":"active","status":{ ... }}

event: cleared
id: high-power
data: {"type":"cleared","at":"...","from":"acked","status":{ ... }}

event: reset
id: high-power
data: {"type":"reset","at":"...","from":"active","status":{ ...状态已按新规则重置... }}

event: removed
id: high-power
data: {"type":"removed","at":"...","from":"active","status":{"id":"high-power","state":"normal","disabled":true,...}}
```

| 约定 | 说明 |
|---|---|
| 事件名 = 迁移类型 | `raised` / `acknowledged` / `cleared` / `reset` / `removed` |
| `id:` | 报警 ID（不是序号）：重连后按 ID 对齐本地状态 |
| 载荷 | `{type, at, from, status}`；`status` 与 REST 单条查询完全一致 |
| `reset` / `removed` | **不是**故障恢复：一个表示规则变了，一个表示规则没了 |
| 慢客户端 | 有界缓冲（256），满则丢最旧并保持最新状态可见 |
| 心跳 | 默认同 `heartbeat` 参数（1s~5min），无事件时只发 `heartbeat` |
| 无报警配置 | 建流成功但只发心跳（不返回错误，便于前端统一处理） |

**为什么没有「事件回放」**：报警是当前状态 + 迁移通知，不是消息队列。客户端
重连时应调用 `GET /api/v1/alarms` 做一次全量对齐，而不是要求服务端补齐断线
期间的每一条事件——那需要持久化，而报警的正确恢复源是「现在的状态」。

---

## 6. MQTT 契约

> **状态**：已实现（`internal/mqtt` + `internal/runtime/mqtt_sink.go`）。客户端为自写的
> MQTT 3.1.1 发布侧实现，零第三方依赖，保证单二进制交叉编译不被打破。

### 6.1 主题结构

```
uic/{site}/{device}/{point}/value     数据（QoS1，非 retained）
uic/{site}/{device}/$state            设备状态（QoS1，retained）
uic/{site}/$birth                     网关上线（QoS1，retained）
uic/{site}/$death                     网关下线（QoS1，retained，LWT）
uic/{site}/$alarm/{alarmId}           报警状态（QoS1，retained）
```

**为什么默认不用通配符前缀**：通配符（`uic/+/+/+`）会增加 Broker 的匹配成本，且不利于 ACL 按设备授权。默认规则固定且稳定，不随版本变化。

**主题渲染规则**：

| 占位符 | 取值 | 清洗 |
|---|---|---|
| `{site}` | `site` 字段 | `/`、`+`、`#`、NUL → `_` |
| `{device}` | `Sample.DeviceID` | 同上 |
| `{point}` | `Sample.PointID` | 同上 |

清洗是最后一道闸门。设备 ID 理论上受 `^[a-z][a-z0-9-]{1,31}$` 约束，但一旦有
ID 里混进 `/`，消息会静默跑到别人的主题上（甚至被别人的 ACL 放行），发送方
完全看不出问题——这类故障的排查成本远高于多写四行 `strings.Map`。

默认模板 `uic/{site}/{device}/{point}/value`，可通过 `northbound.mqtt.topic`
覆盖；模板必须同时包含 `{device}` 与 `{point}`，否则配置校验直接拒绝（所有
点位挤到一个主题上等于没有主题路由）。

### 6.2 消息体

与 REST 的 Sample 完全一致（同一契约），额外增加：

```json
{ "schemaVersion": 1 }
```

`$state` 的载荷：

```json
{
  "schemaVersion": 1,
  "state": "faulted",
  "previous": "degraded",
  "at": "2026-11-04T10:15:23.605Z",
  "error": "read timeout: ..."
}
```

`$birth` / `$death`：

```json
{ "schemaVersion": 1, "online": true,  "startedAt": "2026-11-04T10:15:20Z" }
{ "schemaVersion": 1, "online": false }
```

### 6.3 QoS 与保留

| 消息 | QoS | Retained | 说明 |
|---|---|---|---|
| `value` | 1（默认） | false | 变化上报；可配 `qos: 0` 换取吞吐 |
| `$state` | 同 `value` | **true** | 设备状态（在线/离线/降级） |
| `$birth` | 同 `value` | **true** | 上线 |
| `$death` | 同 `value` | **true** | 异常掉线时由 Broker 代发 LWT；正常停机时由网关自己补发 |

**停机为什么也要发 `$death`**：正常停机时我们发了 DISCONNECT，Broker 不会触发
LWT；而 `$birth` 是 retained 的，不补一条 `online=false` 就会让消费方一直以为
网关在线。运维视角下「网关已经停了但监控说在线」比没有状态更糟。

**为什么状态必须 retained**：状态是「当前事实」而不是事件流。消费方（刚打开的
看板、刚扩容的消费者）连上就要能读到每台设备是否在线，而不是等下一次状态变化
——后者可能几小时都不发生。`$birth`/`$death` 同理：网关是否在线必须一问即知。

**QoS 三态**：`northbound.mqtt.qos` 缺省视为 1，显式写 `0` 表示 QoS0。
配置结构用 `*int` 而不是 `int`，就是为了区分「没写」与「写了 0」——两者
的投递语义（at-least-once / at-most-once）差别很大，不能靠零值猜。

**不支持 QoS2**：客户端只实现 QoS0/1。配置 `qos: 2` 直接报错而不是静默降级
成 1：降级会让运维以为买到了「恰好一次」，而实际语义是「至少一次」。

### 6.3.1 配置字段

```json
{
  "northbound": {
    "mqtt": {
      "enabled": true,
      "broker": "tcp://192.168.1.10:1883",
      "clientId": "uic-plant-a",
      "topic": "uic/{site}/{device}/{point}/value",
      "qos": 1,
      "retain": false,
      "username": "uic",
      "password": "******"
    }
  }
}
```

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | 是 | `false` | 关闭时不创建 sink，也不写入 Outbox |
| `broker` | 启用时必填 | — | `tcp://host:port` 或 `host:port`；`ssl://`/`tls://`/`ws://`/`wss://` 显式拒绝 |
| `clientId` | 否 | json：`uic-{site}-{实例短码}`；sparkplug-b：取 `nodeId` | 缺省带上实例身份短码，同站点多实例默认不冲突；显式配置时以其为准 |
| `format` | 否 | `json` | `json` 或 `sparkplug-b`；见 §6.6 |
| `groupId` | 否 | `site` | 仅 `sparkplug-b`；不得含 `/`、`+`、`#` |
| `nodeId` | 否 | `uic-{site}-{实例短码}` | 仅 `sparkplug-b`；缺省带上实例短码；规则同上 |
| `topic` | 否 | `uic/{site}/{device}/{point}/value` | 必须含 `{device}` 与 `{point}`；`sparkplug-b` 下必须留空 |
| `qos` | 否 | `1` | 仅 0/1；写 2 直接报错；`sparkplug-b` 下必须省略（QoS 由消息类型定死） |
| `retain` | 否 | `false` | 只作用于 `value`；`$state`/`$birth`/`$death` 恒为 retained；`sparkplug-b` 下必须为 false |
| `username` / `password` | 否 | — | 只配 `password` 不配 `username` 会被拒绝（3.1.1 规定） |

`uic -check` 会在启动前做静态校验：缺 `broker`、QoS 越界、模板缺占位符都会
让进程以非零码退出，而不是等到运行时才发现数据发不出去。

### 6.4 断线补发

由 Outbox 保证：

```
采集 → 死区过滤 → Outbox（持久）→ MQTT publish → 收到 PUBACK → 推进位点
```

| 规则 | 说明 |
|---|---|
| 成功判定 | **必须收到 PUBACK**。QoS1 只发不等等于 QoS0，Outbox 会误以为投递成功而丢数据 |
| 损坏记录 | 读不出（反序列化失败）的记录只原地剔除它自己并计入 `dropped[corrupt]`，**绝不连带丢弃它前面的好记录、也不推进位点**：损坏只代表「读不出」，不代表「已送达」，替调用方确认会让未投递的好记录在重启后永久丢失（违反 at-least-once） |
| 顺序 | 单设备内有序；跨设备不保证 |
| 去重键 | `device + point + seq` |
| TTL | 默认 24h，过期丢弃并计数 |
| 恢复限速 | 默认 500 msg/s，可配（Wave 2） |
| 背压 | 队列 > 80% 时只保留变化点（降采样）（Wave 2） |

**断线快速失败**：连接断开时，所有在飞的 QoS1 发布立即收到错误，而不是等满
`AckTimeout`。否则一次网络闪断会让投递协程空转数秒，队列在恢复前持续堆积。
`AckTimeout` 取 3s，必须小于 Outbox 单轮 drain 的 5s 预算——否则日志里只会剩下
`context deadline exceeded`，看不出到底是网络慢还是 Broker 没回 PUBACK。

**重连自愈**：Broker 重启会清空 retained 消息，消费方会永远停在掉线前的状态。
因此客户端每次 CONNECT 成功后都会补发 `$birth` 与全部已知 `$state`（缓存最后
一次状态），而不是只在启动时发一次。

**删除设备/点位时要清掉 retained**：retained 是双向的。删掉本地快照只让本进程
不再补发，Broker 上那条 `.../$state`（或报警状态）仍然挂着，新接入的消费方照样
会读到一台已经不存在的设备。唯一能让它消失的手段是发一条**零长度 retained
载荷**（MQTT 规范定义的「删除该主题的保留消息」）。断线时删除会发不出去，因此
必须记下待清理的 ID，在下次（重）连后补发；丢弃这个记录会让幽灵状态永久留在
Broker 上。

补发与删除必须互斥（sink 内 `sendMu`）：补发是「按快照重写 Broker 上的当前
事实」，若与删除并发，「按旧快照写回」会让刚清掉的幽灵复活，而本地各处都以为
清理已经完成。

删除之后**迟到**的状态事件同样要拦：调度器的状态事件是异步派发的，reload 删掉
设备时队列里可能还压着它的事件。拦住的位置是 runtime（`onDeviceState`），因为它
按配置核对设备是否还在——只有配置能区分「删除」与「同 ID 重配后重新出现」；
若在 sink 里设墓碑，重新配置的设备会被永久静音。

### 6.5 边界（明确不支持）

| 能力 | 状态 | 原因 |
|---|---|---|
| QoS2 | 不支持，配置即报错 | 两段握手（PUBREC/PUBREL/PUBCOMP）对北向上报没有实际收益，且实现复杂度高 |
| TLS（`ssl://`、`tls://`） | 不支持，配置即报错 | 证书加载/轮换/校验是独立的一件事，不能靠「静默当 tcp」蒙混——那会变成「连上了但没加密」 |
| WebSocket（`ws://`、`wss://`） | 不支持，配置即报错 | 同上；边缘部署直连 Broker 更常见 |
| 订阅（json 格式） | 不支持 | 北向是「网关往外发」，订阅语义属于南向 |
| 订阅（sparkplug-b） | 仅订阅自身 `NCMD`/`DCMD` | 只用于 Rebirth 命令；不订阅数据主题 |
| 会话持久化（`CleanSession=false`） | 不支持 | 补发由本地 Outbox 负责，Broker 侧会话只会掩盖真实积压 |
| Sparkplug B | **已实现** | 见 §6.6 |

显式失败优于静默降级：一个配了 `ssl://` 的现场，宁可启动时报错，也不要让它
以为自己在加密传输。

### 6.6 Sparkplug B（`format: "sparkplug-b"`）

> **状态**：已实现（`internal/codec/sparkplug` 编解码、`internal/sparkplug`
> 生命周期状态机、`internal/runtime/sparkplug_sink.go` MQTT 出口）。

Sparkplug B 不是一个「换一种 JSON 结构」的选项，而是一套**基于主题的生命周期
契约**：消费方先看到节点出生（NBIRTH），再看到设备出生（DBIRTH），之后才是
DDATA；每条消息带序号，序号断了消费方就有权丢弃整段数据。所以它被实现为
MQTT 北向的第二种格式，与 json 格式**互斥**，共用 Broker、凭据与 Outbox。

本平台只扮演 **Edge Node（边缘节点）**，不扮演 Host Application。这个边界决定
了几件事：

- 出生/死亡用 NBIRTH/NDEATH 表达，设备用 DBIRTH/DDEATH；
- **不发布 STATE 主题**。`spBv1.0/STATE/{host_id}` 是 Host 的出生/死亡证书，
  由 Host 发布、`retain=true`。边缘节点发它就是冒用一个自己并不具备的身份，
  真正的 Host 会据此认为「有一个 Host 在线」。边缘节点是否在线，Host 只看
  NBIRTH/NDEATH。

```json
{
  "northbound": {
    "mqtt": {
      "enabled": true,
      "format": "sparkplug-b",
      "broker": "tcp://192.168.1.10:1883",
      "clientId": "uic-plant-a",
      "groupId": "plant-a",
      "nodeId": "gw-line1"
    }
  }
}
```

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `format` | 否 | `json` | `json` 或 `sparkplug-b`；其它值启动即报错 |
| `groupId` | 否 | `site` | 现场命名空间；不得含 `/`、`+`、`#` |
| `nodeId` | 否 | `uic-{site}` | 本网关在组内的唯一名；规则同上 |
| `topic` | 否 | 空 | Sparkplug 下**必须留空**：主题由组/节点/设备拼出，模板会破坏契约 |
| `retain` | 否 | `false` | Sparkplug 下**必须为 `false`**：边缘节点的每类消息都是非保留的 |
| `qos` | 否 | — | Sparkplug 下**必须省略**：QoS 由消息类型定死（见 §6.6.2） |

后三项都是「配置了就会生效」的字段，所以在 Sparkplug 下**显式拒绝**而不是
静默忽略：静默忽略等于允许配出一个「本地看起来正常、严格 Host 直接丢弃」的
部署，而现场完全看不出问题。

`clientId` 在 Sparkplug 下缺省取 `nodeId`（不是站点名）：同一站点内两台网关
配了不同 `nodeId` 却忘了 `clientId` 时，用站点名会让它们的 MQTT 会话互相顶掉，
而 Sparkplug 恰恰要求每个节点各自独立。

#### 6.6.1 主题与映射

```
spBv1.0/{group}/NBIRTH/{node}
spBv1.0/{group}/NDEATH/{node}
spBv1.0/{group}/DBIRTH/{node}/{device}
spBv1.0/{group}/DDEATH/{node}/{device}
spBv1.0/{group}/DDATA/{node}/{device}
spBv1.0/{group}/NCMD/{node}
spBv1.0/{group}/DCMD/{node}/{device}
```

只有这 7 类。`spBv1.0/STATE/{host_id}` 属于 Host，本平台既不发布也不解析
（`ParseTopic` 对它返回「不认识」，调用方据此忽略）。

| Sparkplug 概念 | 平台概念 |
|---|---|
| Group ID | `site`（或配置 `groupId`） |
| Node ID | 网关（配置 `nodeId`，缺省 `uic-{site}`） |
| Device ID | 设备 ID（`sanitizeTopicLevel` 之后） |
| Metric Name | 点位 ID |
| Metric Timestamp | `Sample.CollectedAt` |
| `engUnit` 属性 | `Sample.Unit` |
| `quality` 属性 | 非 good 时的质量字符串（`stale`/`bad`/`uncertain`…） |

不实现别名（alias）：Metric Name 直接用点位 ID。别名需要一份跨会话稳定的
编号表，而点位表可以在运行时增删；用名字虽然字节多一点，但永远不会出现
「编号 7 在两次重启后指向不同点位」这类无法排查的问题。

#### 6.6.2 生命周期、QoS 与序号

| 时机 | 发出的消息 |
|---|---|
| MQTT 连接建立（每次 CONNECT） | NBIRTH → 全部已知设备 DBIRTH |
| 首个点位/新增点位 | 该设备的 DBIRTH（完整点位表）→ DDATA |
| 常规变化 | DDATA |
| 设备进入 `faulted`/`disabled` | DDEATH，并忘掉该设备的点位缓存 |
| 设备从配置中删除，或同 ID 重配 | DDEATH，并忘掉该设备的点位缓存 |
| 设备恢复采集 | 下一条数据触发新的 DBIRTH → DDATA |
| 收到 NCMD `Node Control/Rebirth=true` | NBIRTH + 全部 DBIRTH |
| 收到 DCMD `Device Control/Rebirth=true` | 该设备 DBIRTH |
| 正常停机 | NDEATH（主动发布）→ DISCONNECT |
| 异常断链（进程被杀/网络断） | Broker 代发 CONNECT 里登记的 NDEATH 遗嘱 |

**QoS 与 retain 由消息类型定死，不是配置项。** 规范对每类消息逐条给出 MUST：

| 消息 | QoS | retain | 依据 |
|---|---|---|---|
| NBIRTH / DBIRTH / DDATA / DDEATH | 0 | false | `[tck-id-topics-nbirth-mqtt]` 等逐条 MUST |
| NDEATH（遗嘱与主动发布同值） | 1 | false | `[tck-id-payloads-ndeath-will-message-qos]` / `-retain` |
| NCMD / DCMD 订阅 | 1 | — | `[tck-id-message-flow-edge-node-ncmd-subscribe]` / `-dcmd-subscribe` |

NDEATH 主动发布沿用遗嘱的 QoS 1：否则「主动告知的死亡」反而比「Broker 代发
的遗嘱」更容易丢。命令订阅必须是 QoS1：QoS0 的订阅会在链路抖动时静默丢掉
重生请求，表现为「点了重生没反应」。

**`degraded` 不发 DDEATH。** 运行态与 Sparkplug 语义的对应关系只有三种：

| 运行态 | Sparkplug 动作 |
|---|---|
| `collecting` | 无（下一条 DDATA 会带值；若此前已 DDEATH，则由它触发新 DBIRTH） |
| `faulted` / `disabled` | DDEATH，并忘掉点位 |
| `connecting` / `degraded` / `configured` | 无 |

`degraded` 是「部分点位失败、其余仍在采集」。发 DDEATH 会让 Host 把整台设备
从模型里摘掉，正在正常上报的点位也跟着消失——这比不发严重得多。单个点位
没有好值这件事，已经由 DDATA 里的 `is_null` 表达（见 §6.6.3）。

序号规则（**一个边缘节点一个计数器**，不是每台设备一个）：

| 消息 | 规则 |
|---|---|
| NBIRTH | 必须带序号且为 0；同时把计数器重置为 0 |
| DBIRTH / DDATA / DDEATH | 取「上一条消息（含节点消息与其它设备的消息）的序号 + 1」；255 之后回绕到 0 |
| NDEATH | **不带序号，也不消耗序号** |

规范对每种消息都逐条写明「one greater than the previous MQTT message from the
Edge Node」，并有三条可测的 TCK 约束：
`[tck-id-payloads-sequence-num-always-included]`（除 NDEATH 外每条消息都必须带
序号）、`[tck-id-topics-nbirth-seq-num]`（NBIRTH 的序号必须是 0）、
`[tck-id-topics-ndeath-seq]`（NDEATH 不得带序号）。

两个因此容易写错的地方：

- **序号 0 必须真的写进载荷。** 载荷是 Protobuf（proto2，`optional uint64
  seq = 3`），「字段缺失」与「字段为 0」在消费方是两件事（Tahu 生成的代码用
  `hasSeq()` 区分）。把 0 当「缺省字段」省略，会让每一条 NBIRTH/DBIRTH 都
  丢掉必需的序号——而它们恰好全是 0。
- **设备消息不能各自从 0 开始。** 若按设备维护计数器，两台设备交替发布时线上
  会是 `0,1,0,1`；严格的主机把「序号不连续」当成丢包，会请求重生或丢弃数据，
  而发送方完全看不出问题。

**`bdSeq` 每个 CONNECT 前进一格**（从 0 开始、255 之后回绕 0），同时出现在该次
CONNECT 的 NDEATH 遗嘱载荷与随后的 NBIRTH 里
（`[tck-id-topics-nbirth-bdseq-increment]`、`[tck-id-payloads-ndeath-bdseq]`）。
Host 靠它把一对 NDEATH/NBIRTH 配对，并在消息乱序时区分「这次掉线」与「上一次
的旧遗嘱」。所以它**不能**是进程级常量：常量会让重连之后所有死亡消息都带同一
个 bdSeq，Host 无法分辨。实现上 MQTT 客户端的遗嘱是 `Options.Will func() *Will`
回调，每次构造 CONNECT 时重新求值，回调里调用 `Node.BeginSession()` 同时推进并
记录 bdSeq。

`CleanSession` 恒为 `true` 且 CONNECT 必须带遗嘱：前者保证 Broker 不保留上一次
会话的离线消息与订阅（否则 BIRTH 语义就乱了），后者是 Broker 能代发 NDEATH 的
唯一途径。

**为什么 BIRTH 必须带完整点位表**：DBIRTH 是消费方建立「设备有哪些指标」的
唯一时机——指标名、datatype 与别名的解析都在这一步完成，DDATA 只负责更新值。
只带当前这一个点会让设备在出生时就不完整：消费方在 DDATA 里看到一个 BIRTH
从未声明过的指标名，只能当成未知数据丢弃（再去补一帧 BIRTH 也没用，那一帧
DDATA 已经过去了）。实现上为每台设备保存「最后一个采样」，点位集合变化时
重发完整 BIRTH，再发 DDATA。

**为什么点位删除必须走 DDEATH**：DBIRTH 会把它列出的全部点位重新告诉消费方。
设备已从配置里删掉、点位已从设备上删掉，但点位缓存还留着的话，下一次（重连
的）DBIRTH 就会把一台不存在的设备 / 一批不存在的点位重新宣布为在线。所以
配置热加载检测到「设备消失」或「同 ID 重配」时，会先 DDEATH 并清空该设备的
点位表，之后的新数据重新走「完整 BIRTH」。规范允许设备被整体移除时发 DDEATH
（`op.adoc` 的 device-removal 段落）。

**为什么等待重连后要重发 BIRTH**：MQTT 会话是 `CleanSession=true`，Broker
重启后订阅与 retained 状态都可能丢失。`OnConnect` 回调是异步的，不能保证它
先于第一条数据执行；因此 sink 用连接代数（`mqtt.Client.Epoch()`）判断「这是
新会话」，投递路径与回调路径谁先到都只触发一次 BIRTH。

#### 6.6.3 值与质量

| 平台类型 | Sparkplug DataType | 承载字段 |
|---|---|---|
| `bool` | Boolean (11) | `boolean_value` |
| `int16` / `int32` | Int16 / Int32 | `int_value` |
| `int64` | Int64 | `long_value` |
| `uint16` / `uint32` | UInt16 / UInt32 | `int_value` |
| `uint64` | UInt64 | `long_value` |
| `float32` | Float (9) | `float_value` |
| `float64` | Double (10) | `double_value` |
| `string` | String (12) | `string_value` |
| `bytes` | Bytes (17) | `bytes_value` |
| `bcd16` / `bcd32` | Double (10) | `double_value` |

两条不太直观的映射：

- **bytes 用 Bytes (17) + `bytes_value`**。不要把它声明成 String：一旦按
  文本编码，`0x00`/`0xFF` 这类字节会被 UTF-8 改写或截断。枚举与承载字段
  必须同时正确，只对一半会让消费方读到「类型是 String、内容却是二进制」。
  属性集（PropertySet）是 keys/values 两组平行数组，不是键值对对象；编码
  时必须保持下标对应并排序，否则同一指标每次的字节都不同。
- **BCD 按 Double 发**。平台内 BCD 是数值语义，Sparkplug 没有对应枚举；
  转成文本反而要求消费方知道这是压缩 BCD。按 double 发是唯一不丢语义的选择。

质量映射：

| 平台质量 | Sparkplug 表达 |
|---|---|
| `good` | 正常值 |
| `stale` / `bad` / `uncertain` | `is_null=true`，另带 `quality` 属性 |

Sparkplug 没有质量位。把断线前的旧值当成新值发出去，比不发更危险——消费方
会据此做联锁或统计；`is_null` 是协议提供的「这一轮没有可用值」表达。原始
质量放进属性，保留诊断信息但不影响主值语义。

#### 6.6.4 命令

只实现 Sparkplug 的 Rebirth 命令：

| 主题 | 指标 | 行为 |
|---|---|---|
| `NCMD/{node}` | `Node Control/Rebirth = true` | 重发 NBIRTH 与全部 DBIRTH |
| `DCMD/{node}/{device}` | `Device Control/Rebirth = true` | 重发该设备 DBIRTH |

其余命令（包括 `Rebirth=false`、未知指标、本节点不认识的设备）**静默忽略**。
这是协议约定：订阅方可以发任意命令，边缘节点只回应自己实现的那几个。命令
回调运行在 MQTT 读循环里，真正的发布放到独立 goroutine——否则一条慢 Broker
会卡住 PUBACK 处理，表现为「Broker 明明在线，数据却停了」。

#### 6.6.5 边界

| 能力 | 状态 | 原因 |
|---|---|---|
| Host Application 角色（STATE 主题） | 不做 | STATE 是 Host 的出生/死亡证书（`spBv1.0/STATE/{host_id}`）。边缘节点发它就是冒用身份，真正的 Host 会以为「有一个 Host 在线」。本节点在线与否由 NBIRTH/NDEATH 表达 |
| DataSet / 模板 / 别名 | 不做 | 需要跨会话稳定的编号与模板管理；点位表可变时半成品比不做更难排查 |
| retained 生命周期消息 | 不做，配置即拒绝 | 边缘节点的每类消息规范要求 QoS0 且非保留；retained 会让新订阅者看到没有上下文的历史状态 |
| 可配 QoS | 不做，配置即拒绝 | QoS 由消息类型定死；做成可配等于允许配出一个「本地正常、严格 Host 丢弃」的部署 |
| 报警出口 | 不做 | Sparkplug 没有报警主题；硬塞进 DDATA 会污染指标空间。需要报警请用 json 格式或 REST/SSE |
| 自定义主题模板 | 不做，配置即拒绝 | 主题是协议契约的一部分，模板化等于允许配出一个「本地正常、对端收不到」的现场 |

#### 6.6.6 验证

```bash
./uic sim -protocol mqtt -listen 127.0.0.1:18832 &   # 任意 MQTT 3.1.1 Broker 均可
./uic -check -config examples/sparkplug.json
./uic -config examples/sparkplug.json
#   用任意 Sparkplug B 消费端订阅 spBv1.0/plant-a/#
#   期望顺序：NBIRTH → DBIRTH(line-a) → DDATA…
#             → DBIRTH(line-b) → DDATA…
```

集成测试（`internal/runtime/sparkplug_sink_test.go`）覆盖：BIRTH 顺序、DBIRTH
完整点位表、DDEATH（faulted / disabled / 配置删除）、`degraded` 不误杀设备、
NCMD 重生、断线后重发 BIRTH、停机 NDEATH 与 DISCONNECT、异常断链时 Broker
代发遗嘱、**每次 CONNECT 的遗嘱 bdSeq 递增且与 NBIRTH 一致**，以及**两台设备
交替发布时的序号连续性**（节点级计数器，不允许各自从 0 开始）。

编解码金帧、`ParseConnect` 的遗嘱/CleanSession 校验在 `internal/codec/sparkplug`
与 `internal/mqtt`；序号与 bdSeq 规则本身在 `internal/sparkplug` 的状态机单测里
（含 NDEATH 不带序号、两种计数器各自的 255 回绕、NBIRTH 重置序号）。

`Metric` 解码是**加严**的，三层校验缺一不可：oneof 只能有一个成员、每个承载
字段的线类型必须与字段号匹配、声明的 datatype 必须与承载字段一致（`int_value`
是 int32 语义、`long_value` 是 int64 语义）。编码侧 `validate` 已经拒绝非法
组合，解码侧不校验就成了「自己发不出去，别人发过来照收」。

编码侧还要在发出前拦住两类**静默数据损坏**：

- **整型越界截断。** `Int8/16/32`、`UInt8/16/32` 在线上走 `int_value`（uint32
  承载），编码时按补码截断——声明 `Int8` 收 300 会发出 44、声明 `Int32` 收
  `2^40` 会发出 0。截断后的值是一个「看起来合法的另一个值」，消费方无从
  察觉，比直接报错危险得多，因此 `validate` 按位宽收敛、越界一律拒绝；只有
  `Int64`/`DateTime`（走 `long_value`）不做位宽检查。
- **空 PropertySet 键导致键值错位。** PropertySet 是 `keys`/`values` 两条平行
  数组，解码按下标一一配对。编码用 `str()` 写键，而 `str()` 把空串当「缺省
  字段」省略——某个键是空串时它会被丢掉、它对应的值却照样写进 `values`，
  于是 `keys` 比 `values` 短一格，后面所有键值整体错位（`{"":"blank",
  "engUnit":"V"}` 会解出 `{"engUnit":"blank"}`）。空键在 Sparkplug 里没有意义
  （键用来标识属性），`validate` 因此直接拒绝。

两者都是 fail-closed：宁可让这一条消息编码失败（sink 计为失败、记录保留待
重试），也不把错位的值或回绕的数发上总线让下游 SCADA 当成真值。

#### 6.6.7 点位缓存不随配置删除收缩

`internal/cache` 只提供 `Set`/`Get`/`List`/`Count`/`Subscribe`，**没有 `Delete`**。
删点或删设备之后，那份快照仍留在内存里，不会再被任何人读到：

- `Points()` / `Point()` / `markStale` / OPC UA 地址空间 / 从站映射全部按**配置
  枚举**再 `cache.Get`，配置里没有的键永远不会被取出来；
- 因此陈旧条目只占内存，不会出现在 REST、SSE、历史或任何北向出口上。

这是有意的取舍：缓存里存的是「点位当前值」，不是「点位清单」，清单的唯一事实
是配置。为此补一个 `Delete` 只会引入「谁负责在什么时机删」的一致性负担，而收益
是回收几 KB 内存。代价要说清楚：**长时间反复增删大量点位会缓慢增长内存**，且
这些字节不会被任何指标暴露。若将来出现「配置频繁大批量变更」的现场，正确的做法
是按配置定期重建缓存（`Cache` 换新），而不是给每次删除都加一条同步删键路径。

### 6.7 本地 journal 兜底（`storage.journal`）

在与 MQTT/Webhook 并列的位置，还有一个**不需要网络**的 sink：

```json
{ "storage": { "dir": "./data", "journal": true } }
```

开启后，每条通过死区过滤的变化事件会追加到 `<dir>/journal.ndjson`，一行一条：

```json
{"schemaVersion":1,"deviceId":"plc-01","pointId":"temp","value":{"type":"float32","num":23.5},"quality":"good","collectedAt":"2026-11-04T10:15:23.605Z","seq":42}
```

| 规则 | 说明 |
|---|---|
| 写入路径 | 与 MQTT/Webhook 共用 Outbox（独立 sink 分区、独立确认位点） |
| 批量 | 攒批 ≤64 条一次 `write`；`Stop` 时 flush + `Sync` |
| 上限 | 受 `outboxMaxBytes` / `outboxMaxRecords` / `outboxTtl` 约束 |
| 文件生命周期 | 只追加、不删除；轮转与清理交给外部（logrotate / 运维脚本） |
| 语义 | at-least-once；崩溃后可能重复一行，消费方按 `device+point+seq` 去重 |

**它不是北向协议**。定位是：断网留痕、开发联调时无需架 Broker 即可验证整条链路、CI 里可直接断言落盘内容。默认关闭，因为它是磁盘写入路径，不该在为「只是想采个数」的部署里默默产生 I/O。

详见 `03-domain-model.md` §12.0.1 与 `02-architecture.md` §7。

---

### 6.8 报警状态主题

> **状态**：已实现（`internal/runtime/mqtt_sink.go` 的 `$alarm` 出口）。

```
uic/{site}/$alarm/{alarmId}     QoS1, retained
```

```json
{
  "schemaVersion": 1,
  "id": "high-power",
  "name": "功率过高",
  "severity": "warning",
  "message": "压机功率超过 70kW 持续 2 秒",
  "state": "active",
  "active": true,
  "acknowledged": false,
  "count": 3,
  "value": 73.4,
  "valueKnown": true,
  "quality": "good",
  "since": "2026-11-05T10:40:48.534Z",
  "updatedAt": "2026-11-05T10:41:06.736Z"
}
```

| 约定 | 说明 |
|---|---|
| retained | **是**。报警是「当前事实」，新接入的看板连上就该看到 |
| 主题 | 每条规则一个主题，`{alarmId}` 经过 `/`、`+`、`#`、NUL 清洗 |
| 不进 Outbox | 报警不需要 at-least-once 的事件流：积压一条过期报警毫无价值，而漏掉「当前正在报警」是致命的。重连后整体重发即可 |
| 重连补发 | `$birth` 之后重发**全部**规则状态（含 `normal`），保证消费方看到完整集合 |
| 规则删除 | 发布**零长度 retained 载荷**清掉主题；断线时记入待清理表，重连补发 |
| 规则重置 | 发 `reset` 事件并覆盖 retained，不把「规则被改掉」伪装成「恢复正常」 |
| 发布失败 | 只计数（`result="alarm_error"`），不影响采集；下次迁移或重连补发 |

**为什么用 `$alarm` 而不是 `uic/{site}/{device}/alarm/{id}`**：报警规则可以跨设备
引用点位（`press:power > 50 && pump:flow < 10`），挂在某台设备下会暗示「它只属于
那台设备」。`$` 前缀同时把控制面（`$state`/`$birth`/`$death`/`$alarm`）与数据面
（`.../value`）分开，消费方订阅 `uic/{site}/#` 取数据时不会把报警混进来。

---

## 7. OPC UA 映射

> **代码对应**：`internal/northbound/opcua`（地址空间与推送）、`internal/opcuaserver`（服务端内核）。

### 7.1 地址空间

```
Root
└── Objects
     └── {site}                  Object（站点）
          └── {device}           Object（设备，DisplayName 用 device.name）
               └── {point}       Variable（点位，DisplayName 用 point.name）
     └── Server
          ├── ServerStatus
          │    └── CurrentTime   Variable（服务端每秒自动刷新）
          ├── Types
          └── Views
```

**NodeId**：变量为 `ns=2;s={site}/{device}/{point}`，目录同理少一级。
用 `/` 而不是 `.`：点位 ID 允许 `-`（`line-a:power - 1` 里的减号有歧义问题），
而设备 / 点位 ID 不允许 `/`，所以斜杠是**唯一不会与合法 ID 冲突**的分隔符。

**只暴露 enabled 的设备与点位**。禁用的项不出现在地址空间里，而不是「出现但永远坏」——
后者会让运维在客户端侧无法区分「这个点本来就是关的」与「这个点坏了」。

**不暴露设备状态变量**：地址空间只承载点位值。设备运行态（`collecting` / `faulted` …）
由 REST `/api/v1/devices` 与 MQTT retained `$state` 承载——把两套语义混在一个地址空间里，
会让客户端无法区分「点位坏」与「设备坏」。

### 7.2 类型映射

| UIC DataType | OPC UA Type |
|---|---|
| `bool` | `Boolean` |
| `int16/32/64` | `Int16/Int32/Int64` |
| `uint16/32/64` | `UInt16/UInt32/UInt64` |
| `float32/64` | `Float/Double` |
| `string` | `String` |
| `bcd16/32` | `UInt16/UInt32`（值已转换） |

写入按 OPC UA 规则**严格比对 Variant 类型**，不做隐式转换：把 Double 写进 String 节点
必须返回 `BadTypeMismatch`，而不是「成功地」存进去一串 `"1"`。

### 7.3 质量映射

| UIC Quality | OPC UA StatusCode |
|---|---|
| `good` | `Good` |
| `uncertain` / `stale` | `Uncertain`（值可信，只是不再新鲜；源时间戳如实反映年龄） |
| `timeout` | `BadTimeout` |
| `comm_error` | `BadNoCommunication` |
| `address_error` | `BadNodeIDUnknown` |
| `not_connected` / `disabled` | `BadNotConnected` |
| 其他 | `BadNoData` |

**Bad 状态不携带值**。带上最后一个好值，客户端会把「断了」显示成「还在跑」——
这是现场最难察觉的一类错误。

### 7.4 订阅

- 支持 `CreateSubscription` / `CreateMonitoredItems` / `Publish` / `DeleteSubscriptions` / `SetPublishingMode`；
- 监视项建立后立刻上报一次初始样本（含质量码），客户端不必等第一个发布周期；
- 每个带序列号的通知在下一次 Publish 中确认；
- 空闲时按 `MaxKeepAliveCount` 个发布周期发一条空通知，客户端靠它区分「没变化」与「失联」；
- 值变化与质量迁移都推送——只推值变化会让「设备断了」在画面上表现为「值不动了」。

### 7.5 写入

`Write` 服务映射到 `Runtime.Write`（与 REST 写入同一条链路：设备 actor、类型校验、幂等），
5 秒超时。状态码映射：

| 网关错误码 | OPC UA StatusCode |
|---|---|
| `point.not_found` | `BadNodeIDUnknown` |
| `point.not_writable` / `point.disabled` | `BadNotWritable` |
| `value.type_mismatch` | `BadTypeMismatch` |
| `device.not_found` / `device.not_ready` / `service.not_ready` | `BadNoCommunication` |
| 其他 | `BadInternalError` |

**返回 Good 表示设备接受了写入**，不是「网关内存里改了个数」。设备没确认就返回 Good，
SCADA 会显示一个从未生效的设定值。

### 7.6 安全与边界

| 项 | 当前实现 |
|---|---|
| Endpoint | `opc.tcp://{northbound.opcua.listen}`（默认 `:4840`） |
| SecurityPolicy | **只支持 `None`**；配其他值在配置期报错 |
| 用户认证 | **只支持匿名**；`allowAnonymous` 必须显式为 `true` |
| 证书 | 不支持（证书轮换、信任列表属于另一套运维域） |
| 历史 / 方法 / 事件 / 别名 / 分块 | 不支持；请求会得到 `BadServiceUnsupported` |

不支持的组合在**配置期**失败，而不是运行期静默降级成无加密——后者是最危险的一种「兼容」。

### 7.7 配置变更

地址空间在启动时固化（NodeId 与 DataType 都写进了服务端内核）。配置热更新时按指纹
（监听地址、站点、点位集合、类型、可写性）判断是否重建：

- 指纹不变 → 不动服务端，SCADA 无感；
- 指纹变化 → 关闭旧服务端、按新配置重建，SCADA 会经历一次重连；
- 重建失败（例如新端口被占）→ **回退到上一份配置继续服务**，并留下 `opcua_server_restart_failed` 日志。
  配置已落盘，不能因为北向抢不到端口就让整次 Apply 失败；但也绝不能让北向就这么消失。

---

## 8. 一致性与幂等

| 操作 | 幂等 | 说明 |
|---|---|---|
| GET | ✓ | — |
| 写入 | 条件幂等 | `X-Idempotency-Key` 5 分钟窗口 |
| 应用配置 | 条件幂等 | 相同内容哈希 → 返回已存在版本，不重复应用 |
| 回滚 | ✗ | 每次回滚产生新版本 |
| 命令 | ✗ | 除 `outbox.flush` 外均非幂等 |

---

## 9. 版本演进与弃用

| 阶段 | 时长 | 行为 |
|---|---|---|
| Announce | 发布前 1 个版本 | 文档标注 `Deprecated`，响应头 `Deprecation: true` |
| Dual | ≥ 6 个月 | 新旧接口并存，数据双发 |
| Remove | — | 升 `/v2`，`/v1` 保留只读 12 个月 |

---

## 10. 最小实现清单（Wave 1 骨架）

| 能力 | 状态 | 说明 |
|---|---|---|
| `/healthz` `/readyz` `/api/v1/health` | 已实现 | 见代码 |
| `/api/v1/devices` `/devices/{id}` `/devices/{id}/points` | 已实现 | — |
| `/api/v1/points/{device}/{point}` | 已实现 | — |
| `POST /points/{device}/{point}/write` | 已实现 | 单点 |
| `GET /api/v1/stream` | 已实现 | SSE + 通配 |
| `GET /api/v1/metrics` | 已实现 | 文本格式 |
| `POST /api/v1/commands` | 已实现 | 白名单子集 |
| 配置版本/回滚 | 已实现 | 文件快照 |
| 报警 REST + SSE | 已实现 | 见 §4.7 / §5.4（五态机、确认、质量门控） |
| 报警 MQTT retained | 已实现 | 见 §6.8（重连补发、删除清理） |
| `GET /api/v1/history` | 已实现 | 见 §4.8（原始点 + 定步长聚合、有界保留） |
| `alarm.ack` 命令 | 已实现 | 同 §4.7.3 语义 |
| MQTT | 已实现 | 见 §6（QoS0/1、retained 状态、断线补投） |
| OPC UA 北向 | 已实现 | 见 §7（地址空间、质量映射、写穿透、订阅、热重建） |
| RBAC（三角色九权限） | 已实现 | 见 §2.2.1 / §4.9 |
| 审计（哈希链 + 查询/校验） | 已实现 | 见 §4.10（NDJSON 分段、at-least-once 语义） |
| `uic token` | 已实现 | 生成 Token + `tokenSha256` + 可粘贴片段 |
| 诊断包 | 已实现 | 见 §4.6 |
| 网络探测 | 已实现 | 见 §4.6.1（`/api/v1/diagnostics/probe`，TCP 握手 + RTT，仅探测配置内通道） |

---

## 11. 结论

1. **四种传输共享一个数据契约**，是「不让集成方为同一个值写四套解析」的唯一办法。契约的稳定性比传输的丰富性更重要。

2. **`configuredState` 与 `runtimeState` 必须分开返回。** 这是从 38 个方案里学到的最便宜、最有效的产品细节：它让「配置了但没采」从「看起来正常」变成「一眼可见」。

3. **写入必须逐项返回、必须审计、必须支持幂等键。** 工业写入是不可逆动作，接口设计要把「误操作」当作默认假设，而不是异常情况。
