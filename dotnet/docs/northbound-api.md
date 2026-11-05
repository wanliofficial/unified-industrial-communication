# 北向契约

四个通道共享同一份数据模型：REST 返回的单条样本、SSE 的 `sample` 事件、MQTT 的主题载荷
都是同一个 `Sample` 结构。任何一个通道新增字段，其余三个同步可见。

## 公共约定

### 认证

配置了 `northbound.rest.token` 时，除下列路径外都需要 `Authorization: Bearer <token>`：

```
/healthz   /readyz   /api/v1/version   /api/v1/metrics
```

以及所有非 `/api/` 的静态资源（否则 WASM 首屏拉不到程序集）。

- 令牌比较是定长的，避免用耗时差反推；
- 配置里没写令牌 = 不鉴权，启动时会打一条告警；
- 401 响应带 `WWW-Authenticate: Bearer`。

### 限流

按来源 IP 计数，只统计 `/api/` 请求（静态资源不计入，否则首屏会把自己打成 429）：

| 类别 | 速率 |
|---|---|
| 读（GET / HEAD） | 100 次/秒，突发 100 |
| 写（POST / PUT / PATCH / DELETE） | 10 次/秒，突发 10 |

超过返回 `429` + `request.too_many_requests`。

### 错误信封

所有错误都是同一个形状，`code` 稳定、`message` 可读、`requestId` 可追踪：

```json
{
  "error": {
    "code": "device.not_found",
    "message": "device 'meter-9' is not configured",
    "requestId": "req-3f7c1d0a9b2e4c58",
    "details": null
  }
}
```

| HTTP | `code` | 场景 |
|---|---|---|
| 400 | `request.invalid` / `request.invalid_parameter` / `request.missing_parameter` | 请求体或参数不合法 |
| 400 | `request.confirm_required` | 破坏性操作缺少 `confirm: true` |
| 401 | `auth.unauthorized` | 缺少或错误的令牌 |
| 404 | `device.not_found` / `point.not_found` / `resource.not_found` | 找不到对象 |
| 422 | `config.invalid` | 配置语法或语义非法，`details` 里是逐条问题 |
| 429 | `request.too_many_requests` | 限流或 SSE 连接数超限 |
| 500 | `internal.error` | 未预期异常 |

响应头 `X-Request-Id` 会回显客户端传入的值（没传就生成一个），日志里也用同一个 id。

### 序列化

- 枚举一律小写线上名：`uint16`、`comm_error`、`abcd`；
- 时间统一 UTC + 毫秒 + `Z`；
- 时长写成 `"1s"` / `"500ms"`；
- 值为 `null` 的字段不输出。

## REST

### 系统

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 存活探针，返回 `ok` |
| GET | `/readyz` | 就绪探针，逐项列出依赖状态 |
| GET | `/api/v1/version` | 产品名、版本、API 版本、站点、运行时、启动时间、配置版本 |
| GET | `/api/v1/health` | 设备/点位/队列汇总与问题清单 |
| GET | `/api/v1/metrics` | Prometheus 文本格式 |

`/api/v1/health` 的形状：

```json
{
  "status": "degraded",
  "uptimeSeconds": 3821,
  "devices": { "total": 12, "collecting": 10, "degraded": 1, "faulted": 1 },
  "points":  { "total": 486, "good": 470, "stale": 8, "bad": 8 },
  "outbox":  { "pending": 0, "droppedLastHour": 0 },
  "problems": [
    { "device": "meter-3", "state": "faulted", "reason": "connect timed out", "since": "2026-11-05T09:12:31.882Z" }
  ]
}
```

### 设备与点位

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/devices` | 设备列表（含运行态、成功率、P95） |
| GET | `/api/v1/devices/{device}` | 单台设备 |
| GET | `/api/v1/devices/{device}/points` | 该设备的点位（分页） |
| GET | `/api/v1/points` | 全部点位（可按 `device` 过滤） |
| GET | `/api/v1/points/{device}/{point}` | 单个点位当前值 |
| POST | `/api/v1/points/{device}/{point}/write` | 写单个点位 |
| POST | `/api/v1/points/{device}/write` | 批量写，支持 `X-Idempotency-Key` |

分页响应统一为：

```json
{ "items": [ ... ], "nextCursor": "MTAw", "total": 486 }
```

游标是不透明字符串（内部就是一个偏移量），客户端不要解析它。
列表查询支持 `limit`（1..1000，默认 100）与 `cursor`。

设备列表另支持 `q`（按 id / 名称过滤）；点位列表支持 `device`、`quality`、`q`。

点位当前值：

```json
{
  "device": "meter-1",
  "point": "voltage",
  "name": "电压",
  "value": { "type": "uint16", "num": 2201 },
  "quality": "good",
  "unit": "V",
  "deviceTime": null,
  "collectedAt": "2026-11-05T09:14:02.113Z",
  "seq": 812,
  "writable": true
}
```

「配置了但从没采到」返回 `quality: "not_connected"` 与 0 占位值，不会编一个看起来正常的读数。

### 写入

单点写法（`{device}/{point}/write`）：

```json
{ "value": { "type": "float64", "num": 77.5 }, "confirm": true }
```

批量写法（`{device}/write`）：

```json
{
  "writes": [
    { "point": "setpoint", "value": { "type": "float64", "num": 60 } },
    { "point": "enable",   "value": { "type": "bool", "bool": true } }
  ]
}
```

响应是**逐点裁决**，一个点位失败不会让整批 500：

```json
{
  "results": [
    { "point": "setpoint", "ok": true,  "quality": "good",     "appliedAt": "2026-11-05T09:15:20.004Z", "error": null },
    { "point": "enable",   "ok": false, "quality": "disabled", "appliedAt": "2026-11-05T09:15:20.004Z", "error": "point is read-only" }
  ]
}
```

- 只读点位、类型不匹配、越界在网关层就被拒，不会发到设备上；
- 写请求在通道内插队到读之前（现场按「立即执行」比「先来后到」更符合直觉）；
- 带 `X-Idempotency-Key` 时，5 分钟内的相同 key 直接返回首次结果并带
  `X-Idempotent-Replay: true`，客户端超时重试不会写两次。

### 配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/config` | 当前配置（口令类字段脱敏为 `***`） |
| GET | `/api/v1/config/versions` | 版本列表 |
| GET | `/api/v1/config/versions/{id}` | 某个版本的完整内容 |
| GET | `/api/v1/config/diff?from=&to=` | 两个版本的语义差异 |
| POST | `/api/v1/config/validate` | 校验，返回 `valid` + `problems[]` |
| POST | `/api/v1/config/apply` | 提交并热更新 |
| POST | `/api/v1/config/rollback` | 回滚到指定版本 |

`validate` 的语义刻意是「永远返回 200 + 问题清单」：

```json
{ "valid": false, "problems": ["site: must match ^[a-z][a-z0-9-]{1,31}$"] }
```

只有**请求体整体不是 JSON** 时才返回 422（那已经不是「配置写得不对」而是「调用方式不对」）。

`apply` 与 `rollback` 则相反：任何问题都抛 422，`details` 里是逐条 `{path, message}`。

```json
{
  "version": "v6d22485a2e6d",
  "applied": true,
  "changed": { "devicesAdded": 0, "devicesModified": 1, "devicesRemoved": 0,
               "pointsAdded": 2, "pointsModified": 0, "pointsRemoved": 0 },
  "restartedDevices": ["meter-1"],
  "durationMs": 143
}
```

`apply` 只重启受影响的设备 —— 这是热更新能用在产线上的前提。

### 命令

`POST /api/v1/commands`：

```json
{ "action": "driver.restart", "params": { "device": "meter-1" }, "confirm": true }
```

| `action` | 参数 | 说明 |
|---|---|---|
| `driver.restart` | `device` | 重启单台设备的 actor（重新建链） |
| `device.selftest` | `device` | 探活 + 读一个点，返回是否通过 |
| `outbox.flush` | — | 立即冲刷一轮队列 |
| `outbox.drop` | — | 需要 `confirm: true`；本版本明确返回未实现 |
| `config.rollback` | `version` | 等价于配置回滚端点 |

### 诊断

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/logs` | 环形缓冲日志，按 `level` / `device` / `limit` 过滤 |
| GET | `/api/v1/diagnostics/bundle` | 打包配置、日志、指标、环境信息（zip） |
| GET | `/api/v1/diagnostics/network` | 通道连通性检查 |

日志是内存里的环形缓冲（默认 2000 条），进程重启即清空 —— 边缘设备上「翻日志」
是最常见的排障动作，但没人愿意为此部署一套日志系统。

## SSE

`GET /api/v1/stream?points=meter-1.voltage,meter-2.*&quality=good&heartbeat=15`

| 参数 | 说明 |
|---|---|
| `points` | 逗号分隔的 `device.point` 模式，支持结尾 `*`；省略表示全部 |
| `quality` | 只推送指定质量（`good` / `timeout` …） |
| `heartbeat` | 心跳间隔秒数，1..300，默认 15 |

事件类型：

| `event` | 何时发 | 载荷 |
|---|---|---|
| `hello` | 连接建立后立刻 | `{server, apiVersion, heartbeatSeconds, filters}` |
| `sample` | 有越过死区的变化 | `SampleDto` |
| `quality` | 质量码发生变化 | `{device, point, from, to, at}` |
| `heartbeat` | 空闲到间隔 | `{ts}` |

- 每条事件都有 `id`（`device:point:seq`），可用于客户端去重；
  断线重连从当前时刻开始，服务端不重放历史——重连后先拉一次
  `/api/v1/points` 补齐快照，再看增量；
- 最多 50 条并发流，超了返回 429；
- 响应头带 `Cache-Control: no-transform` 与 `X-Accel-Buffering: no`，
  否则反向代理会缓冲住事件，表现为「数据不动」。

## MQTT

只实现 3.1.1 的发布侧（QoS 0/1，无 QoS2）。

| 主题 | 载荷 | 保留 |
|---|---|---|
| `{prefix}/{site}/{device}/{point}/value` | `SampleDto` JSON | 按 `retain` |
| `{prefix}/{site}/{device}/$state` | `{schemaVersion, state, previous, at, error}` | 是 |
| `{prefix}/{site}/$birth` | `{schemaVersion, online: true, startedAt}` | 是 |
| `{prefix}/{site}/$death` | `{schemaVersion, online: false}` | 是 |

- 值主题由 `northbound.mqtt.topic` 模板决定，必须含 `{device}` 与 `{point}`；
- `$state` 在设备运行态变化时发一次，`$birth` 在每次连上（含重连）后补发，
  因为 broker 重启会清空 retained；
- 停机时发 `$death` 并把遗嘱（LWT）也指向它，这样「进程被 kill」与「正常退出」
  在订阅端看起来一致；
- 投递失败直接抛给 `Outbox` 保留位点重试 —— 这一层只负责「发出去并拿到 PUBACK」，不负责「不丢」。

## OPC UA

```
Objects
 └── UIC                        Folder
      └── {site}                Folder
           └── {device}         Object（DeviceType，DisplayName 取设备名）
                ├── _state      String：运行态（collecting / degraded / faulted …）
                ├── _quality    String：设备整体质量
                └── {point}     Variable：类型按点位映射
```

- NodeId 是**字符串**：`ns=2;s={site}.{device}.{point}`。配置迭代、服务重启后保持稳定，
  SCADA 侧的绑定不会因为标识漂移集体失效；
- 值来自点位缓存，与 REST/MQTT 看到的是同一份最后已知值，客户端订阅不会触发额外采集；
- 读取时同时返回 `StatusCode`（质量映射到 OPC UA 状态码）与源时间戳；
- 写入走调度器，与 REST 写入是同一条路径，串行化与超时语义完全一致；
- 匿名只读默认开启、匿名写默认关闭；本版本只支持 `SecurityPolicy.None`。

订阅：点位节点按缓存变化推送 DataChange 通知，发布间隔由客户端决定（推荐 100–1000 ms）。
