# 配置

## 文件形态

一份 JSON。启动时先读配置、再起服务：配置非法就退出（退出码 2），绝不「尽力而为」地跑。

绝大多数字段都有默认值，人只需要写想改的部分。网关自己序列化出来的是**补全默认值之后**的形态，
两者语义等价 —— 往返一次是稳定的，否则每次启动都会「产生一个新版本」。

```json
{
  "schemaVersion": 1,
  "site": "shanghai-plant-1",
  "description": "一号车间采集",
  "channels": [
    {
      "id": "modbus-tcp-1",
      "name": "本机 Modbus TCP",
      "protocol": "modbus-tcp",
      "transport": "tcp",
      "host": "127.0.0.1",
      "port": 15020,
      "maxConcurrency": 2,
      "timeout": "2s"
    }
  ],
  "devices": [
    {
      "id": "meter-1",
      "name": "电表 1",
      "channelId": "modbus-tcp-1",
      "interval": "1s",
      "unitId": 1,
      "byteOrder": "abcd",
      "points": [
        {
          "id": "voltage",
          "name": "电压",
          "address": { "kind": "register", "area": "holding", "offset": 0 },
          "dataType": "uint16",
          "scale": 0.1,
          "unit": "V",
          "deadband": 0.2
        },
        {
          "id": "energy",
          "name": "累计电量",
          "address": { "kind": "register", "area": "holding", "offset": 10, "length": 2 },
          "dataType": "float32",
          "byteOrder": "cdab",
          "unit": "kWh"
        }
      ]
    }
  ],
  "northbound": {
    "rest": { "enabled": true, "listen": "0.0.0.0:8080", "token": "换成你的令牌" },
    "mqtt": {
      "enabled": true,
      "host": "127.0.0.1",
      "port": 1883,
      "topic": "uic/{site}/{device}/{point}/value",
      "qos": 1
    },
    "opcUa": { "enabled": true, "endpoint": "opc.tcp://0.0.0.0:4840" }
  },
  "logging": {
    "file": { "enabled": true, "directory": "logs", "maxBytes": 10485760, "retain": 5 }
  }
}
```

## 顶层

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `schemaVersion` | int | `1` | 只接受 `1`；升级格式时递增 |
| `site` | string | `default` | `^[a-z][a-z0-9-]{1,31}$`，出现在 MQTT 主题与 OPC UA 命名空间里 |
| `description` | string? | — | 给人看的备注，参与版本摘要 |
| `channels` | array | `[]` | 链路 |
| `devices` | array | `[]` | 设备 |
| `northbound` | object | 见下 | 北向通道 |
| `storage` | object | 见下 | 数据目录与队列上限 |
| `logging` | object | 见下 | 日志输出（默认只有内存环形缓冲） |

## 通道 `channels[]`

| 字段 | 默认 | 说明 |
|---|---|---|
| `id` | 必填 | `^[a-z0-9][a-z0-9-]{0,31}$`，设备靠它引用 |
| `name` | — | 显示名 |
| `protocol` | 必填 | `modbus-tcp` / `modbus-rtu` / `modbus-ascii` / `virtual` |
| `enabled` | `true` | 关闭后该通道上的设备不采集 |
| `transport` | `tcp` | `tcp` / `serial` / `udp` / `none` |
| `host` / `port` | — / `0` | `tcp`/`udp` 必填；端口 1..65535 |
| `serial` | — | `transport=serial` 时必填，见下 |
| `maxConcurrency` | `4` | 通道级并发上限（`>= 1`） |
| `timeout` | `3s` | 单次请求超时 |
| `connectTimeout` | `5s` | 建链超时 |
| `idleTimeout` | `30s` | 空闲多久断开 |

串口参数 `serial`：

```json
{ "port": "/dev/ttyUSB0", "baud": 9600, "dataBits": 8,
  "parity": "none", "stopBits": "one", "readTimeoutMs": 1000 }
```

`parity` 取 `none|odd|even|mark|space`，`stopBits` 取 `one|onePointFive|two`，
`baud` 300..921600，`readTimeoutMs` 10..60000。

RTU/ASCII 主站按 3.5 个字符时间做帧间静默（9600 bps 约 4 ms；>19200 bps 按规范
固定 1.75 ms）。静默挂在物理串口上，同一根 RS-485 上多台从站交替发请求时也成立。

## 设备 `devices[]`

| 字段 | 默认 | 说明 |
|---|---|---|
| `id` | 必填 | `^[a-z0-9][a-z0-9._-]{0,63}$` |
| `name` | — | 显示名（同时是 OPC UA 设备节点的 DisplayName） |
| `channelId` | 必填 | 必须指向已存在的通道 |
| `enabled` | `true` | |
| `interval` | `1s` | 采集周期，`>= 10ms` |
| `timeout` | 通道超时 | 本设备的单次请求超时 |
| `retries` | `0` | 0..10，仅对**临时性失败**（超时、链路断开）重试；地址错、协议错不重试 |

`retries` 是「一个采集周期内的重试」，不是「无限重连」：次数用完后本轮按失败上报，
下一轮仍按 `interval`/退避节奏开始。重试前会先断开连接，避免超时后迟到的响应
被当成新请求的回复。`address` 类错误（点表地址不存在/越界）既不重试也不计入
`failureThreshold`，否则一个配错的点会让整台设备被熔断。
| `unitId` | `1` | Modbus 从站地址 1..255 |
| `byteOrder` | `abcd` | `abcd` / `cdab` / `badc` / `dcba`，点位可覆盖 |
| `failureThreshold` | `3` | 连续失败多少次进入降级 |
| `recoverInterval` | `30s` | 降级后探活间隔 |
| `labels` | `{}` | 驱动私有参数 |
| `points` | 必填 | 至少一个 |

`labels` 的键由驱动声明，虚拟设备用到的几个：

| 键 | 取值 | 说明 |
|---|---|---|
| `mode` | `sine` / `ramp` / `random` / `const` | 波形 |
| `periodSeconds` | 秒 | 周期 |
| `amplitude` / `baseline` | 数值 | 幅度与基线 |
| `fault` | `none` / `drop` / `timeout` / `bad` / `address` / `disconnect` | 故障注入类型 |
| `faultAt` | 第几次请求 | 只在这一次注入 |

> 故障注入是刻意做到配置里的：验收「坏值怎么显示」「断连怎么恢复」不应该需要拔网线。

## 点位 `points[]`

| 字段 | 默认 | 说明 |
|---|---|---|
| `id` | 必填 | `^[a-z0-9][a-z0-9._-]{0,63}$`，设备内唯一 |
| `name` | — | 显示名 |
| `address` | 必填 | 见下 |
| `dataType` | `uint16` | 见下 |
| `byteOrder` | 继承设备 | 32/64 位值的排列 |
| `scale` / `offset` | `1` / `0` | 线性变换 `值 × scale + offset` |
| `deadband` | 不启用 | 死区阈值；含义由 `deadbandMode` 决定，`>= 0` |
| `deadbandMode` | `absolute` | `absolute` / `percent` / `rate`，见下 |
| `rangeMin` / `rangeMax` | — | 量程下限 / 上限；必须成对出现且 `rangeMin < rangeMax` |
| `interval` | 继承设备 | 点位自己的采集周期：空或 `0` 表示继承设备，非 0 时 `100ms..24h` |
| `unit` | — | 单位，原样带到北向 |
| `writable` | `true` | 只读点位在驱动层就被拒，不会发到线上 |
| `enabled` | `true` | |
| `length` | `0` | `string`/`bytes` 的字节长度（1..1024） |
| `count` | `0` | 数组元素个数：`0`/`1` 表示标量，`> 1` 表示定长数组，见下 |

### 点位周期

同一个 `device.interval` 不一定适合所有点位：流量、位置这类快变量要 100ms，
液位、温度这类慢变量 10s 就够。`interval` 允许点位覆盖设备周期：

```json
"points": [
  { "id": "flow",  "interval": "100ms", "address": { "area": "holding", "offset": 10 }, "dataType": "float32" },
  { "id": "level", "interval": "10s",   "address": { "area": "holding", "offset": 12 }, "dataType": "float32" }
]
```

- 每个点位按自己的有效周期读取，同一设备上的请求仍然**串行**：按周期分组后，
  每轮只读到期的组，到期的多组合并成一次往返；
- 设备仍然只有一个连接与一个采集循环，不存在「快慢两条链路互相抢串口」；
- 读窗口按组分别规划：快慢点位即使地址相邻也不会被合成一个请求，这是周期的代价；
- 某一组失败只把该组的点位标成坏质量，不会牵连还没到期的慢变量；
- 看门狗按设备内最慢的点位周期判断「采集停滞」，只挂慢变量的设备不会误报；
- 低于 `100ms` 属于「该用订阅型协议」的区间，校验会直接拒绝；
- CSV 导入用 `interval` 列，写法与 JSON 一致（`100ms` / `1s` / `2m`，纯数字按毫秒）。

### 死区

`deadband` 不是「有变化就报」，而是「变化超过阈值才报」。三种模式对应三种现场噪声：

| `deadbandMode` | 判定 | 适用 |
|---|---|---|
| `absolute`（默认） | `\|Δ\| > deadband` | 噪声幅度稳定，阈值就是工程量 |
| `percent` | `\|Δ\| > deadband% × (rangeMax - rangeMin)` | 同一套点表跨不同量程复用；要求量程，`deadband` 取 `(0, 100]` |
| `rate` | `\|Δ\| / Δt(秒) > deadband` | 抑制慢漂移、保留突跳；`deadband` 是「单位/秒」，必须 `> 0` |

- `Δ` 是**相邻两次采集**的差，不是「上次上报」的差；`Δt` 取两条样本的 `collectedAt`。
- 阈值相等不报（严格大于）。
- `Δt <= 0`（时钟回拨、同一时刻的两条样本）时按「上报」处理——宁可多一条，也不把变化吞掉。
- 质量变化永远上报，即使数值没变；坏质量之间的反复跳变不重复上报。
- 百分比模式必须配量程，否则阈值无法解释；这些组合在启动前由配置校验拦下。

```json
{
  "id": "temperature",
  "dataType": "float32",
  "unit": "℃",
  "deadband": 2,
  "deadbandMode": "percent",
  "rangeMin": -20,
  "rangeMax": 120
}
```

上面这个点位在 -20..120 的量程上，变化超过 `2% × 140 = 2.8 ℃` 才上报。

### 地址

四种寻址范式统一在一个结构里，新增协议只需要用其中一种：

```json
{ "kind": "register", "area": "holding", "offset": 100, "length": 2, "bit": -1 }
{ "kind": "symbol",   "path": "DB1.DBD0" }
{ "kind": "object",   "objectType": "AnalogInput", "instance": 1, "property": "PresentValue" }
{ "kind": "file",     "fileId": 1, "recordId": 0, "offset": 0 }
```

寄存器寻址的 `area` 取值：

| `area` | 读功能码 | 写功能码 | 说明 |
|---|---|---|---|
| `holding` | 03 | 06 / 16 | 保持寄存器 |
| `input` | 04 | — | 输入寄存器（只读） |
| `coil` | 01 | 05 / 15 | 线圈（按位） |
| `discrete` | 02 | — | 离散输入（按位，只读） |

- `length` 为 `0` 表示「按数据类型推导」；只有要覆盖默认宽度时才显式填写。
  填得比类型需要的短会被拒绝（`uint32` 需要 2 个寄存器）；
- `bit` 只在字区（`holding`/`input`）上有效，用来取某个寄存器的第 n 位。

### 数据类型

`bool`、`int16`、`uint16`、`int32`、`uint32`、`int64`、`uint64`、
`float32`、`float64`、`string`、`bcd16`、`bcd32`、`bytes`、`datetime`。

写法和北向值里的 `type` 完全一样（小写）。旧版本配置文件里的 `uInt16` 之类的拼写仍能读入。

`datetime` 固定占 4 个寄存器，线上编码是有符号 64 位 Unix 毫秒（UTC），字节序同样受
`byteOrder` 影响；北向值用 ISO-8601 UTC 文本（`2026-11-05T12:47:46.123Z`）表示，
消费方不必猜时区或单位。写入非法时间会直接报错，不会退化成 1970-01-01。

### 数组点位

`count` 表达「这个点位是几个同类型元素」，与 `dataType` 正交：

```json
{ "id": "profile", "count": 4, "dataType": "float32",
  "address": { "area": "holding", "offset": 100 }, "name": "四段曲线" }
```

- 用**元素个数**而不是「占用寄存器数」：后者随类型变化（`uint16` 是 1 个寄存器、
  `float64` 是 4 个），写点表的人得自己乘一遍，乘错就整段读偏。
  实际窗口由类型推导：上面这个点位占 `4 × 2 = 8` 个寄存器。
- 只有**定宽数值类型**能成数组（含 `bcd16`/`bcd32`）。`bool` 按位寻址、
  `string`/`bytes` 靠 `length`、`datetime` 的线上值不是数字，`count > 1` 会被校验拒绝。
- 单个点位最多 125 个寄存器（Modbus 单次读的硬限制），超了必须拆成多个点位。
- 编解码**逐元素**套 `byteOrder`，不做整段反转：`cdab` 的 2 元素 `float32`
  是「每个元素各自字交换」，不是把 4 个字整体反转。
- 死区按「逐元素比、取最大变化」判定：任一元素越过阈值即上报。
  取最大而不是平均，是为了不让「一个通道跳变、其余不变」被平均掉。
- `scale`/`offset` 同样逐元素应用。
- CSV 导入用 `count` 列，写法与 JSON 一致。

### 字节序

32/64 位值跨多个 16 位寄存器时，现场有四种常见排列：

| 值 | 寄存器里看到的顺序 |
|---|---|
| `abcd` | 高字在前，字内高字节在前（Modbus 标准） |
| `cdab` | 字交换 |
| `badc` | 字节交换 |
| `dcba` | 完全反转 |

## 北向 `northbound`

### `rest`

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `true` | |
| `listen` | `0.0.0.0:8080` | `host:port`，端口 1..65535 |
| `token` | 空 | Bearer 令牌；为空表示不鉴权（启动时会告警） |
| `staticRoot` | 空 | 前端静态目录；为空时用宿主目录下的 `wwwroot` |

### `mqtt`

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `false` | |
| `host` / `port` | `127.0.0.1` / `1883` | |
| `username` / `password` | 空 | 设了密码就必须有用户名 |
| `clientId` | `uic-gateway` | |
| `topic` | `uic/{site}/{device}/{point}/value` | 必须含 `{device}` 与 `{point}` |
| `qos` | `1` | 只支持 0 与 1（QoS2 不实现） |
| `retain` | `false` | |
| `keepAliveSeconds` | `30` | 5..600 |
| `reconnectDelayMs` | `2000` | 重连退避 |
| `publishedSnapshotOnStart` | `true` | 启动时补发一次当前快照 |
| `commands` | `false` | 打开下行命令通道（订阅 `commandTopic`） |
| `commandTopic` | `{prefix}/{site}/$command` | 不能含 `{device}` / `{point}` |
| `commandResultTopic` | `{prefix}/{site}/$command/result` | 必须与 `commandTopic` 不同 |

主题模板必须含 `{device}` 与 `{point}`，且以 `topicPrefix` 开头或含 `{site}` ——
否则同一套主题可能把两个站点的数据混在一起。

命令主题与结果主题同样以 `topicPrefix` 开头或含 `{site}`。`commands` 默认关闭：
这一版 MQTT 只有匿名明文，开启前请确认 Broker 侧已经做了网络隔离或 ACL。
即便打开，`config.rollback` 与 `outbox.drop` 也会被拒绝，只能走 REST。
契约见 [northbound-api.md](northbound-api.md#下行命令可选)。

### `opcUa`

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `false` | |
| `endpoint` | `opc.tcp://0.0.0.0:4840` | 必须以 `opc.tcp://` 开头 |
| `applicationName` | `UIC Gateway` | |
| `namespaceUri` | `urn:uic:gateway` | 地址空间命名空间 |
| `namespaceIndex` | `2` | 期望值，实际以服务端分配为准 |
| `allowAnonymousRead` | `true` | 本版本必须为 `true` |
| `allowAnonymousWrite` | `false` | 默认只读 |
| `securityPolicy` | `None` | 本版本只支持 `None` |

> 这两条不是「暂时没做」，而是**显式的能力边界**：配成签名/加密或关闭匿名读会在校验阶段被拒绝，
> 而不是等到客户端握手失败。证书生命周期是独立的运维负担，不该被悄悄降级成明文。

## `storage`

| 字段 | 默认 | 说明 |
|---|---|---|
| `dataDir` | `./data` | 配置版本、队列、证书都在它下面 |
| `journal` | `false` | 额外的本地 NDJSON 兜底通道，同时作为 `/api/v1/history` 的数据源 |
| `outboxMaxRecords` | `100000` | 队列条数上限 |
| `outboxMaxBytes` | `268435456` | 队列字节上限（256 MiB） |
| `outboxTtl` | `24h` | 超过这个时长的记录直接丢弃 |
| `outboxBatchSize` | `64` | 一次投递的最大条数 |
| `outboxFlushInterval` | `1s` | 后台冲刷间隔 |
| `minFreeBytes` | `0` | 数据目录剩余空间低于该值进入降级；`0` = 不检查 |
| `resumeFreeBytes` | `0` | 恢复到该值以上才解除降级；`0` = 取 `minFreeBytes` 的两倍 |
| `maxMemoryBytes` | `0` | 进程工作集超过该值进入降级；`0` = 不检查 |
| `resourceCheckInterval` | `10s` | 资源水位检查间隔，`1s .. 1h` |

超限时丢最旧的并计数，`/api/v1/health` 与指标里都能看到丢弃数。

### 资源水位降级

磁盘与内存的水位都必须显式配置：1 GB 的盒子和机房虚拟机对「多少算少」的答案不同，
给一个默认值只会在某类现场变成误报。进入降级后：

- **磁盘低于 `minFreeBytes`**：文件日志立即停写（`Suspend`），journal 通道摘掉，
  新样本不再往历史里追加；已排队的记录不删除，恢复后继续投递。区域空间留给 Outbox 落盘。
  `/api/v1/health` 的 `status` 转 `degraded`，`problems` 里出现 `device: "$resources"`，
  并记 `uic_resource_pressure_total{kind="disk"}`。
- **工作集超过 `maxMemoryBytes`**：只把健康状态转 `degraded` 并告警，不主动释放内存
  （强停采集或清缓存会让数据缺口比内存压力更糟）。

恢复到 `resumeFreeBytes`（内存为上限的 90%）以上自动解除：文件日志重新打开，
journal 重新挂载。进入线与恢复线之间的回差是刻意的——单阈值会在边界反复停/开，
产生的日志比要防的问题还多。

```json
{
  "storage": {
    "dataDir": "/var/lib/uic",
    "minFreeBytes": 268435456,
    "resumeFreeBytes": 536870912,
    "maxMemoryBytes": 134217728,
    "resourceCheckInterval": "10s"
  }
}
```

阈值支持热更新：改完配置立即按新线判定一次，不必重启进程。

## `logging`

控制台与 `/api/v1/logs` 的环形缓冲始终开启；这里只决定**要不要额外写文件**。

| 字段 | 默认 | 说明 |
|---|---|---|
| `file.enabled` | `false` | 默认关闭：边缘设备磁盘小，未显式配置时不应悄悄落盘 |
| `file.directory` | `logs` | 相对 `storage.dataDir`；绝对路径原样使用 |
| `file.fileName` | `uic.log` | 当前活动文件名，必须是纯文件名（不能带路径） |
| `file.maxBytes` | `10485760` | 单文件上限，4 KiB .. 1 GiB |
| `file.retain` | `5` | 保留的归档份数（不含当前文件），1..100 |
| `file.minimumLevel` | — | `trace` / `debug` / `information` / `warning` / `error` / `critical`；为空则沿用全局级别 |

格式是一行一个 JSON 对象，异常堆栈被转义进同一个字段，不会把一条日志拆成多行：

```json
{"ts":"2026-11-06T02:31:12.482+00:00","level":"Information","category":"uic.device.meter-1","msg":"collection recovered","device":"meter-1"}
{"ts":"2026-11-06T02:31:13.004+00:00","level":"Error","category":"uic.device.meter-1","msg":"read failed","device":"meter-1","exception":"System.TimeoutException: ..."}
```

轮转只在新行会超过 `maxBytes` 时发生：当前文件改名为 `uic-20261106-023112-482.log`，
再按修改时间删掉超出 `retain` 的旧归档；当前文件永不参与裁剪。日志目录里因此
最多有 `retain + 1` 个文件。逐行 flush，便于 `tail -f`；写失败只在 stderr 提醒一次，
不会因为磁盘满把采集进程拖垮。

`logging` 支持热更新：应用新配置后立即切换目录/上限/开关，不需要重启进程。
目录切换只影响之后写入的行，旧目录里的文件原样保留。

## 时长写法

`"500ms"`、`"2s"`、`"5m"`、`"1h"`、`"1d"`，也接受：

- 纯数字 —— 按毫秒（`250` 等于 `250ms`），旧配置与手写脚本都这么写；
- `TimeSpan` 标准写法 —— `"01:00:00"`，从别处拷来的值能直接读。

写错单位会直接报错，不会静默当成毫秒：把「3 秒」读成 3 毫秒是采集侧最难发现的错误之一。

## 版本管理

数据目录结构：

```
data/
  config/
    current              # 当前版本 id（内容寻址，形如 v6d22485a2e6d）
    versions/
      v6d22485a2e6d.json
      v6d22485a2e6d.meta.json   # 创建时间、注释、大小
  templates/
    modbus-meter.json    # 设备模板，与配置版本分开
  diagnostics/
    uic-diagnostics-20261105-091402.zip
  logs/
    uic.log                     # 当前 JSON 行日志（logging.file.enabled=true 时）
    uic-20261105-091402-118.log # 轮转归档，最多 retain 份
  outbox/
    <sink>/records.ndjson
    <sink>/ack
```

模板单独存放是有意的：模板是「怎么生成设备」的规则，配置是「现在有哪些设备」的事实。
混在一起会让回滚把模板也带回去，也会让「改一个默认值」变成「现场 50 台设备一起变」。

- **内容寻址**：`id = "v" + sha256(canonical json)[..12]`，同样的内容永远得到同一个 id；
- **只增不改**：提交新版本不会动历史；回滚是把旧内容作为一次新提交，因此也能再回滚；
- **启动优先级**：`current` 指针 > `--config` 指定的文件 > 内置兜底配置。
  运维回滚过之后重启，不会把旧文件重新捡回来；
- **热更新**：按设备粒度 diff，只重启受影响的设备，其余采集不中断。

## 旧配置迁移

加载与导入都会先过一遍迁移链（`ConfigMigrations`）：

| 旧写法 | 新写法 |
|---|---|
| `server.listen` | `northbound.rest.listen` |
| `server.token` | `northbound.rest.token` |
| `northbound.mqtt.broker`（`tcp://host:port`） | `northbound.mqtt.host` + `port` |
| `storage.dir` | `storage.dataDir` |
| `channels[].concurrency` | `channels[].maxConcurrency` |
| `devices[].channel` | `devices[].channelId` |
| `channels[].transport`（`{kind,address}` / `{kind,port,baud,…}` 对象） | 扁平化：`transport` 变字符串，`address` 拆成 `host`/`port`，串口参数落到 `channels[].serial` |
| `serial.stopBits`（数字 `1` / `1.5` / `2`） | 字符串 `one` / `onePointFive` / `two` |

时长也接受 Go 风格的复合写法（`1h0m0s`、`1h30m`、`2m30s`），
因为早期示例配置就是这么写的。

规则：

1. `schemaVersion` 等于当前版本 → 原样使用；
2. 小于当前版本 → 执行迁移，升级项在导入响应里逐条回显（静默改写比拒绝更难排查）；
3. 大于当前版本 → 拒绝启动：不能「猜」新字段的语义，猜错会静默丢配置。

迁移**不删字段以外的东西**：`northbound.webhook` 这类当前版本还不支持的段落
原样保留、只是被忽略，等它们变成正式功能时再读，避免「升级即丢数据」。
（`logging` 已是正式段落，不再属于这一类。）

## 校验

`uic check` 与 `POST /api/v1/config/validate` 走的是同一份校验器（`ModelValidator`），
所以「控制台能存、重启起不来」不会发生。

```bash
dotnet run --project src/Uic.Host -- check --config ./gateway.json
```

```
invalid: ./gateway.json
  - site: must match ^[a-z][a-z0-9-]{1,31}$
  - devices[meter-1].channelId: channel 'modbus-tcp-2' not found
  - devices[meter-1].points[energy].address.length: float32 needs 2 register(s), got 1
```

一次列全所有问题，而不是遇到第一条就退出。
