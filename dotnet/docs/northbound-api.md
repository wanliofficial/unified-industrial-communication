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
- 令牌本身**不接受明文**：配置里只能是 `${env:NAME}`、`${file:PATH}` 或 `enc:v1:...`，
  启动与 `config/apply` 遇到明文直接失败（见 [configuration.md](configuration.md#凭据)）；
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
  "resources": {
    "status": "degraded",
    "diskLow": true,
    "memoryHigh": false,
    "freeBytes": 104857600,
    "diskTotalBytes": 4294967296,
    "workingSetBytes": 83886080,
    "since": "2026-11-05T09:10:02.119Z"
  },
  "problems": [
    { "device": "meter-3", "state": "faulted", "reason": "connect timed out", "since": "2026-11-05T09:12:31.882Z" },
    { "device": "$resources", "state": "degraded", "reason": "free disk space is below the watermark; history and file logging are paused", "since": "2026-11-05T09:10:02.119Z" }
  ]
}
```

`resources` 只在运行时装配了资源看门狗时出现；`freeBytes` / `diskTotalBytes` /
`workingSetBytes` 为 `null` 表示平台读不到该项，不是 0。资源告急不会把已经
`faulted` 的设备状态掩盖成 `degraded`——故障优先。

### 驱动

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/drivers` | 已注册驱动的能力、角色与寻址声明 |

能力是四态而不是布尔——「某协议支持符号寻址」在不同型号上含义不同，
用 `true` / `false` 只会撒谎：

| 值 | 含义 |
|---|---|
| `supported` | 确定支持 |
| `unsupported` | 确定不支持 |
| `runtimeDependent` | 视设备型号 / 固件而定，不能当成保证 |
| `experimental` | 有实现，但还没有经过现场验证 |

```json
{
  "items": [
    {
      "id": "modbus-tcp",
      "name": "Modbus TCP",
      "version": "1.0.0",
      "sdkVersion": "1.0",
      "roles": ["master"],
      "transports": ["tcp"],
      "addressKinds": ["register"],
      "capabilities": {
        "read": "supported",
        "write": "supported",
        "subscribe": "unsupported",
        "browse": "unsupported",
        "discovery": "unsupported",
        "multiRead": "supported",
        "bitAccess": "supported",
        "structTypes": "unsupported"
      },
      "description": "Modbus TCP 主站：FC1/2/3/4/5/6/15/16，位访问与四种字序。"
    }
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
| POST | `/api/v1/points/{device}/{point}/write` | 写单个点位，支持 `X-Idempotency-Key` |
| POST | `/api/v1/points/{device}/write` | 批量写，支持 `X-Idempotency-Key` |
| POST | `/api/v1/devices/{device}/points/import` | 点表 CSV 导入（默认 dry-run） |

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
  "writable": true,
  "deadband": 5,
  "deadbandMode": "percent",
  "rangeMin": 0,
  "rangeMax": 100,
  "interval": "500ms",
  "effectiveInterval": "500ms",
  "count": 0
}
```

`deadband` / `deadbandMode` / `rangeMin` / `rangeMax` 原样来自点位配置：上位机既能显示量程，
也能用同一套规则自行过滤，不必猜网关用的阈值。取值范围见
[configuration.md](configuration.md#死区)。

`count` 是数组点位的元素个数，`0` 表示标量。它与 `value` 里的 `type` 正交：
`type` 说明「元素是什么类型」，`count` 说明「有几个元素」。
标量点位的 `value` 形态完全不变（`{"type":"uint16","num":2201}`），
数组点位的值放在 `array` 里：

```json
{
  "device": "line-a",
  "point": "profile",
  "value": { "type": "float32", "array": [1.5, -2.25, 0.125, 8.75] },
  "quality": "good",
  "count": 4,
  "collectedAt": "2026-11-05T09:14:02.113Z",
  "seq": 813,
  "writable": true
}
```

数组值只写 `array`，不写 `num`：两个值字段同时出现会让消费方要自己决定「以谁为准」，
两侧实现一旦不同就是静默错值。数组元素个数由数组长度表达，
`count` 与 `array` 长度在正常采集下始终一致；写入请按 `count` 给足元素
（见 [configuration.md](configuration.md#数组点位)）。

`interval` 是点位自己的周期；继承设备周期时该字段**省略**（不返回 `null`）。
`effectiveInterval` 是解析后的实际周期，始终存在。上位机用它决定「多久没更新算过期」，
不必假设全设备同一个节奏。见 [configuration.md](configuration.md#点位周期)。

「配置了但从没采到」返回 `quality: "not_connected"` 与 0 占位值，不会编一个看起来正常的读数。

### 写入

单点写法（`{device}/{point}/write`）：

```json
{ "value": { "type": "float64", "num": 77.5 }, "confirm": true }
```

数组点位按 `count` 给足元素，元素个数不符直接拒绝而不是静默补齐：

```json
{ "value": { "type": "float32", "array": [1.5, -2.25, 0.125, 8.75] }, "confirm": true }
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
    { "point": "setpoint", "ok": true,  "quality": "good",      "appliedAt": "2026-11-05T09:15:20.004Z", "error": null, "verified": true },
    { "point": "enable",   "ok": false, "quality": "disabled",  "appliedAt": "2026-11-05T09:15:20.004Z", "error": "point is read-only", "verified": null }
  ]
}
```

- 只读点位、类型不匹配、越界在网关层就被拒，不会发到设备上；
- 写请求在通道内插队到读之前（现场按「立即执行」比「先来后到」更符合直觉）；
- `confirm: true` 会在同一个通道租约内**立即回读**受影响点位并比较：
  - 读回一致 → `ok: true`、`verified: true`；
  - 读回不一致 → `ok: false`、`quality: "uncertain"`、`error` 以 `verify.mismatch` 开头；
  - 回读本身失败 → `ok: false`，质量码沿用超时/通信错误，`error` 以 `verify.readback_failed` 开头。
  写入已经下发，所以「不一致」不是「被拒绝」，现场处置是查设备是否保持该值，而不是查配置；
- 未传 `confirm`（或显式 `false`）不回读：命令寄存器常常「写一下就复位」，强制回读只会制造假故障；
- 单点与批量都接受 `X-Idempotency-Key`：5 分钟内的相同 key 直接返回首次结果并带
  `X-Idempotent-Replay: true`，客户端超时重试不会写两次。首次响应带
  `X-Idempotent-Replay: false`，两种状态都明确给出，避免调用方靠猜；
- 幂等作用域是 `设备 + 单点/批量 + key`，同一个 key 用在另一个点位上不会被误判为重放。

### 点表导入（CSV）

`POST /api/v1/devices/{device}/points/import`，默认 **dry-run**：先看会新增/覆盖什么，
确认后才落盘。现场最常见的操作失误是「一次手滑覆盖整张点表」，默认写库会把这种失误放大。

请求体两种形态都接受：

```json
{ "csv": "id,name,address,dataType,unit\n...", "dryRun": false, "comment": "从 Excel 导入" }
```

或直接把裸 CSV 当请求体（`curl --data-binary @points.csv`）。
查询串 `?dryRun=false` 优先于请求体，便于脚本显式表达「我确认要写」。

列名大小写不敏感，接受 `id`、`name`、`address`/`area`+`offset`、`bit`、`length`、
`dataType`、`byteOrder`、`unit`、`scale`、`bias`、`deadband`、`deadbandMode`、
`rangeMin`、`rangeMax`、`interval`、`writable`、`enabled`、`byteLength`。

| 地址写法 | 示例 | 说明 |
|---|---|---|
| 区域:偏移 | `holding:100`、`coil:5.2` | `.` 后是位号 0..15 |
| Modbus 参考号 | `40001`、`30010`、`00001.3` | 一基参考号，转为零基偏移 |
| 分列填写 | `area=holding` + `offset=100` | 从 Excel 分列导入时更自然 |

```json
{
  "device": "meter-1",
  "valid": true,
  "applied": false,
  "added": ["extra"],
  "updated": ["temp"],
  "points": [ ... ],
  "problems": []
}
```

- 合并而不是整表替换：CSV 没提到的点位原样保留，同 `id` 的点位被覆盖；
- 同一文件内 `id` 重复、地址无法解析、类型不认识都会在 dry-run 阶段按行号报出，
  不会先写一半再失败；
- 应用时走与 `config/apply` 相同的路径，只重启这一台设备。

### 设备模板

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/templates` | 模板列表 |
| GET | `/api/v1/templates/{id}` | 模板定义 |
| POST | `/api/v1/templates` | 保存模板（同 id 覆盖） |
| DELETE | `/api/v1/templates/{id}` | 删除模板 |
| POST | `/api/v1/templates/{id}/instantiate` | 实例化（默认只预览） |

模板保存在 `{dataDir}/templates`，与配置版本目录分开：模板是「怎么生成设备」的规则，
配置是「现在有哪些设备」的事实。改模板不会重写已经应用的设备。

模板参数只有 `string` / `int` / `bool` 三种类型，替换只发生在字符串字面量里的
`{name}`，不执行任何表达式。整串就是一个占位符时按参数类型产出原生 JSON 类型
（`int` 变数字、`bool` 变布尔），不会全部退化成字符串。

```json
{
  "instances": [ { "base": "10" }, { "base": "20" } ],
  "apply": true,
  "comment": "批量开通 2 台表计"
}
```

- 批量实例化未显式给 `index` 时自动注入 `1..N`，50 台设备不需要手写 50 个 `index`；
- `apply: true` 时，实例化出的设备 id 与现有设备冲突会被拒（422），不会静默覆盖；
- 保存模板时会静态检查「设备骨架里用了但参数表里没声明的占位符」，把错误挡在写入前。

### 历史查询

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/history` | 从本地 journal 查询历史样本 |

查询参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `device` | 全部 | 设备 id，精确匹配 |
| `point` | 全部 | 点位 id，精确匹配 |
| `from` / `to` | `to - 1h` / 现在 | ISO-8601 时间范围；带偏移时自动转 UTC，不带偏移按 UTC 解释 |
| `interval` | 不降采样 | 时间桶（`5s` / `1m` / `1h`）；每个设备+点位每桶保留最后一条，桶边界按 UTC 纪元对齐 |
| `limit` | 100 | 1..1000 |
| `cursor` | — | 上一页响应里的 `nextCursor` |

响应与列表端点一致：

```json
{
  "items": [
    {
      "device": "plc-01", "point": "temp",
      "value": { "type": "float64", "num": 77.5 },
      "quality": "good", "unit": "°C", "deviceTime": null,
      "collectedAt": "2026-11-05T09:15:20.004Z", "seq": 1024, "error": null
    }
  ],
  "nextCursor": "Mg==",
  "total": 42
}
```

- 数据源是 `storage.journal` 的 NDJSON；没开 journal 时返回空页而不是 404。
- 顺序固定为 `collectedAt` 升序，再按设备/点位/序号排序；journal 只追加，
  因此分页游标在查询期间是稳定的。
- 同一 `(device, point, seq)` 只返回一次：Outbox 是至少一次投递，重放会写出重复行。
- 掉电留下的半行会跳过，不影响同文件里其它样本。
- 读取分两段：先用字节扫描只取「时间 + 设备 + 点位 + 序号」，做过滤、去重与选桶；
  扫描结束后按文件偏移回读胜出行，才做完整 JSON 解析。所以 `interval` 降采样时
  只解析最终返回的那几万条，而不是全部百万行。
- 性能（本机 16 核 / 14 GB，Release，100 万行 / 152 MB 单文件）：
  全范围 + `5m` 降采样约 1.0–1.3 s，单设备 + `1m` 降采样约 0.4 s，最近 30 分钟约 0.3 s；
  不降采样返回 100 万行原始样本约 5–8 s（受限于输出规模本身）。
  仍建议用 `from`/`to` 收窄窗口。
- 首版没有时间索引：每次查询都从头扫文件。`total` 是过滤并降采样后的总数；
  保留策略跟随 journal 文件生命周期，暂不自动清理。数据量再上一个量级应换时序库，
  接口形状不需要变。

### 配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/config` | 当前配置（口令类字段脱敏为 `***`） |
| GET | `/api/v1/config/versions` | 版本列表 |
| GET | `/api/v1/config/versions/{id}` | 某个版本的完整内容 |
| GET | `/api/v1/config/diff?from=&to=` | 两个版本的语义差异（凭据字段的值同样脱敏为 `***`） |
| POST | `/api/v1/config/validate` | 校验，返回 `valid` + `problems[]` |
| POST | `/api/v1/config/apply` | 提交并热更新 |
| POST | `/api/v1/config/rollback` | 回滚到指定版本 |
| GET | `/api/v1/config/export?redact=false` | 导出当前配置（默认脱敏） |
| POST | `/api/v1/config/import?apply=true` | 导入配置（默认只校验与预览） |

`validate` 的语义刻意是「永远返回 200 + 问题清单」：

```json
{ "valid": false, "problems": ["site: must match ^[a-z][a-z0-9-]{1,31}$"] }
```

只有**请求体整体不是 JSON** 时才返回 422（那已经不是「配置写得不对」而是「调用方式不对」）。

`validate` 与 `apply` 走同一条还原路径：提交值里的 `"***"` 先换回当前引用，再校验。
凭据字段另外检查「引用是否真的可解析」（环境变量存在、文件可读、密文能解开），
所以「validate 通过、apply 失败」不会出现。

`apply` 与 `rollback` 则相反：任何问题都抛 422，`details` 里是逐条 `{path, message}`。
凭据是明文时同样 422，`code` 为 `config.invalid`，`path` 指向 `northbound.rest.token`
或 `northbound.mqtt.password`。

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

**脱敏占位符的往返语义**：`apply` 与 `import` 接受「读回来、改一处、再提交」的配置。
提交值里位于 `token` / `password` / `secret` 字段的 `"***"` 会被还原成当前配置中
**同路径的引用**（`${env:...}` / `enc:v1:...`，而不是明文），
因此改设备名或加点位不会把凭据覆盖成字面量 `***`，也不会把明文写回文件。
若当前配置里没有对应值，占位符保持原样交给校验；显式提交空串仍表示清空该凭据。
配置差异（`/config/diff`）只保留「哪个字段变了」，凭据字段的前后值同样显示为 `***`。

### 导出与导入

`GET /api/v1/config/export` 默认脱敏（`token` / `password` / `secret` → `***`）；
`?redact=false` 导出的是配置文件里的**原值**——也就是引用本身，明文从来不在磁盘上。

`POST /api/v1/config/import` 默认只做「迁移 + 校验 + 预览」，加 `?apply=true` 才落盘。
响应里的 `migrations` 列出实际执行的升级项，例如：

```json
{ "valid": true, "applied": false, "version": null,
  "migrations": ["v0.server.listen -> northbound.rest.listen",
                 "v0.northbound.mqtt.broker -> host/port"],
  "config": { ... }, "problems": [] }
```

- `schemaVersion` 小于当前版本时自动升级；
- 大于当前版本时直接拒绝：不能「猜」新字段的语义，猜错会静默丢配置；
- 升级项必须回显：静默改写比拒绝更难排查。

### 命令

`POST /api/v1/commands`：

```json
{ "action": "driver.restart", "params": { "device": "meter-1" }, "confirm": true }
```

| `action` | 参数 | 说明 |
|---|---|---|
| `driver.restart` | `device` | 重启单台设备的 actor（重新建链） |
| `device.selftest` | `device` | 探活 + 读一个点，返回是否通过 |
| `outbox.flush` | `sink?` | 立即冲刷一轮队列；给 `sink` 时只推那一条通道 |
| `outbox.drop` | `sink`, `before` | 需要 `confirm: true`；本版本明确返回未实现 |
| `config.rollback` | `version` | 等价于配置回滚端点 |
| `log.level` | `level` | 动态调整日志级别 |
| `diag.bundle` | — | 生成诊断包，返回落盘路径 |

`outbox.flush` 带 `sink` 是为了「只补 MQTT 不补 journal」：不同 sink 有独立的位点，
全量冲刷会把本来不需要重投的分区也推一遍。指定不存在的 sink 返回错误而不是静默成功 ——
操作员会据此判断积压是否真的清空了。

`diag.bundle` 与 `GET /api/v1/diagnostics/bundle` 共用同一份构建逻辑，
区别只是前者把 zip 写到 `{dataDir}/diagnostics/` 并返回路径：
现场排障时容器可能随时被替换，操作员需要能在下一次重启前把文件拷走。

### 诊断

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/logs` | 环形缓冲日志，按 `level` / `device` / `limit` 过滤 |
| GET | `/api/v1/diagnostics/bundle` | 打包配置、日志、指标、环境信息（zip） |
| GET | `/api/v1/diagnostics/network` | 通道连通性检查 |

日志默认是内存里的环形缓冲（默认 2000 条），进程重启即清空 —— 边缘设备上「翻日志」
是最常见的排障动作，但没人愿意为此部署一套日志系统。需要留档时在配置里打开
`logging.file.enabled`，即可额外得到按大小轮转的 JSON 行文件（见
[configuration.md](configuration.md#logging)）。

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

实现 3.1.1 的发布与订阅（QoS 0/1，无 QoS2）。

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
- 投递失败直接抛给 `Outbox` 保留位点重试 —— 这一层只负责「发出去并拿到 PUBACK」，不负责「不丢」；
- QoS1 的发布只有收到 PUBACK 才返回，3 s 内等不到就抛 `TimeoutException`；
  QoS0 发完即返回（丢一条不会卡住位点），因此对可靠性有要求的现场必须用 QoS1；
- 连接空闲时按 keepalive 的一半发 PINGREQ：只声明 keepalive 而不发心跳，
  broker 会在 1.5×keepalive 后单方面断开，表现为「没有数据时定时重连」；
- 不走 TLS 与 WebSocket：需要加密时在链路上用 stunnel / 反向代理终结 TLS，
  再连本机明文端口。配置校验阶段会拒绝 `qos=2`，避免「以为支持其实没有」。

### 下行命令（可选）

`northbound.mqtt.commands` 打开后，客户端订阅 `commandTopic`
（默认 `{prefix}/{site}/$command`）。订阅在建立连接时登记，**每次重连都会重新订阅**：
Broker 重启或 ACL 变更后不需要人工重启网关。订阅被 Broker 拒绝时只记录错误，
数据通道继续工作——命令通道失效不应该把整条链路拖进重连循环。

请求：

```json
{"id": "9f1c", "action": "driver.restart", "params": {"device": "d1"}, "confirm": false}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `id` | 是 | 幂等键；也接受 `idempotencyKey` |
| `action` | 是 | 见下方允许列表 |
| `params` | 否 | 字符串键值对，与 REST 的 `params` 同义 |
| `confirm` | 否 | 与 REST 相同的确认语义（本通道上不会用到需要确认的动作） |

结果发到 `commandResultTopic`（默认 `{prefix}/{site}/$command/result`），
**成功和失败都发**——只回成功的通道会让操作员分不清「没执行」和「结果丢了」：

```json
{"schemaVersion":1,"id":"9f1c","action":"driver.restart","ok":true,
 "detail":"device 'd1' restarted","replayed":false,"at":"2026-11-05T...","site":"plant-a"}
```

失败时多一个 `error` 对象，`code` 与 REST 错误信封同源：

```json
{"schemaVersion":1,"id":"9f1c","action":"config.rollback","ok":false,
 "replayed":false,"at":"...","site":"plant-a",
 "error":{"code":"command.forbidden","message":"command 'config.rollback' is not allowed on this channel"}}
```

- 允许的动作：`driver.restart`、`device.selftest`、`outbox.flush`、`log.level`、`diag.bundle`；
- `config.rollback` 与 `outbox.drop` 固定返回 `command.forbidden`：这一版 MQTT
  只有匿名明文，切换整份配置与丢弃队列必须留在可挂 Bearer Token 的 REST 上；
- 同一 `id` 在 **5 分钟窗口**内重复到达时不再执行，直接重放首次结果并标
  `replayed:true`。重连与人工重投都可能造成重复；
- 入站 QoS1 在入队后立即回 PUBACK，执行在独立 worker 上串行进行，不会堵住
  读循环与心跳。进程在「已确认、未执行完」之间崩溃仍会丢命令——这一版没有持久会话，
  需要严格语义时请用 REST 的幂等键 + 结果查询。

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
