# 北向接口规范（v1）

> **版本**：API v1（`/api/v1`）
> **上游**：`01-prd.md` FR-NOR-*、`02-architecture.md` §4.3
> **代码对应**：`internal/northbound/httpapi`（REST + SSE）；MQTT 与 OPC UA 在 Wave 1 后半实现
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

### 2.2 认证

```
Authorization: Bearer <token>
```

| 规则 | 说明 |
|---|---|
| Token 来源 | 登录接口签发；或配置的静态 API Token（运维用） |
| 有效期 | 会话 Token 12h；API Token 可长期 |
| 失败 | `401` + `WWW-Authenticate: Bearer` |
| 权限不足 | `403` + 错误码 `auth.forbidden` |
| 首次登录 | 必须修改初始口令，否则除改密外全部 `403` |

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
  "problems": [
    { "device": "plc-07", "state": "faulted", "reason": "timeout", "since": "2026-11-04T07:10:00Z" }
  ]
}
```

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
| 确认 | `confirm: true` 时网关回读一次，不一致返回 `uncertain` |
| 批量 | 逐项结果；部分失败返回 200 + 失败项（不用 500） |
| 审计 | 每次写入记录：谁、何时、点位、旧值、新值、结果 |

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

| action | 参数 | 权限 | 说明 |
|---|---|---|---|
| `driver.restart` | `device` | `device.control` | 重启单设备采集 |
| `device.selftest` | `device` | `device.control` | 读 1 个点验证连通性 |
| `outbox.flush` | `sink?` | `system.control` | 手动触发补发 |
| `outbox.drop` | `sink`, `before` | `system.control` | 丢弃指定时间前消息 |
| `config.rollback` | `version` | `config.rollback` | 同 4.4 |
| `log.level` | `level` | `system.control` | 动态调整日志级别 |
| `diag.bundle` | — | `system.read` | 生成诊断包 |

**规则**：未知 action 返回 `request.unknown_action`；所有命令写审计；高危命令需二次确认参数 `"confirm": true`。

### 4.6 诊断

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/diagnostics/bundle` | 下载诊断包（zip） |
| GET | `/api/v1/diagnostics/network?device=` | 连通性诊断（DNS/TCP/延迟） |
| GET | `/api/v1/logs?device=&level=&limit=` | 查询日志（尾部） |

诊断包内容：

```
manifest.json        版本/时间/站点/配置哈希
config.redacted.json 脱敏配置（口令→***）
metrics.txt          指标快照
logs/uic.log         最近 5000 行
health.json          健康快照
devices.json         设备状态与统计
```

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

---

## 6. MQTT 契约

### 6.1 主题结构

```
uic/{site}/{device}/{point}/value     数据
uic/{site}/{device}/$state            设备状态（retained）
uic/{site}/$birth                     网关上线（retained，LWT 为 $death）
uic/{site}/$death                     网关下线（LWT）
```

**为什么默认不用通配符前缀**：通配符（`uic/+/+/+`）会增加 Broker 的匹配成本，且不利于 ACL 按设备授权。默认规则固定且稳定，不随版本变化。

### 6.2 消息体

与 REST 的 Sample 完全一致（同一契约），额外增加：

```json
{ "schemaVersion": 1 }
```

### 6.3 QoS 与保留

| 消息 | QoS | Retained | 说明 |
|---|---|---|---|
| `value` | 1 | false | 变化上报 |
| `$state` | 1 | true | 设备状态（在线/离线/降级） |
| `$birth` | 1 | true | 上线 |
| `$death` | 1 | true | LWT |

### 6.4 断线补发

由 Outbox 保证：

```
采集 → 死区过滤 → Outbox（持久）→ MQTT publish → 收到 PUBACK → 删除
```

| 规则 | 说明 |
|---|---|
| 顺序 | 单设备内有序；跨设备不保证 |
| 去重键 | `device + point + seq` |
| TTL | 默认 24h，过期丢弃并计数 |
| 恢复限速 | 默认 500 msg/s，可配 |
| 背压 | 队列 > 80% 时只保留变化点（降采样） |

### 6.5 Sparkplug B（Wave 2）

| 项 | 映射 |
|---|---|
| Group ID | `site` |
| Node ID | 网关 ID |
| Device ID | `device` |
| Metric Name | `point` |
| `NBIRTH`/`NDEATH` | 网关生命周期 |
| `DBIRTH`/`DDEATH` | 设备生命周期 |
| `DDATA` | 采样值 |

### 6.6 本地 journal 兜底（`storage.journal`）

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

详见 `03-domain-model.md` §10.0.1 与 `02-architecture.md` §7。

---

## 7. OPC UA 映射

### 7.1 地址空间

```
Objects
 └── UIC (ns=2)
      └── {site}
           └── {device}          Object（类型：DeviceType）
                ├── _state       String（collecting/faulted/...）
                ├── _quality     String
                └── {point}      Variable（类型映射见 7.2）
```

**NodeId**：`ns=2;s={site}.{device}.{point}`（稳定，不随重启变化）。

### 7.2 类型映射

| UIC DataType | OPC UA Type |
|---|---|
| `bool` | `Boolean` |
| `int16/32/64` | `Int16/Int32/Int64` |
| `uint16/32/64` | `UInt16/UInt32/UInt64` |
| `float32/64` | `Float/Double` |
| `string` | `String` |
| `bcd16/32` | `UInt16/UInt32`（值已转换） |
| `datetime` | `DateTime` |

### 7.3 质量映射

| UIC Quality | OPC UA StatusCode |
|---|---|
| `good` | `Good` |
| `uncertain` | `Uncertain` |
| `stale` | `UncertainLastUsableValue` |
| `bad` / `timeout` / `comm_error` / `address_error` | `BadCommunicationError` / `BadWaitingForInitialData` |
| `not_connected` | `BadNotConnected` |
| `disabled` | `BadOutOfService` |

### 7.4 订阅

- 支持 `MonitoredItem`（DataChange）；
- 采样间隔默认等于设备周期，可被客户端覆盖（下限 100ms）；
- 队列大小默认 10；
- 支持写：`Write` 服务映射到 REST 写入路径（同样的权限与审计）。

### 7.5 安全

| 项 | 默认 |
|---|---|
| Endpoint | `opc.tcp://0.0.0.0:4840` |
| SecurityPolicy | `Basic256Sha256`（可选 `None`，需显式开启） |
| 用户认证 | 用户名/口令（复用本地账号）或匿名只读 |
| 证书 | 首次启动自动生成，支持导入企业 CA |

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
| MQTT / OPC UA | 规范已定义 | Wave 1 后半实现 |
| RBAC / 审计 / 诊断包 | 规范已定义 | Wave 1 后半实现 |

---

## 11. 结论

1. **四种传输共享一个数据契约**，是「不让集成方为同一个值写四套解析」的唯一办法。契约的稳定性比传输的丰富性更重要。

2. **`configuredState` 与 `runtimeState` 必须分开返回。** 这是从 38 个方案里学到的最便宜、最有效的产品细节：它让「配置了但没采」从「看起来正常」变成「一眼可见」。

3. **写入必须逐项返回、必须审计、必须支持幂等键。** 工业写入是不可逆动作，接口设计要把「误操作」当作默认假设，而不是异常情况。
