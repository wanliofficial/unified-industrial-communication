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

## 设备 `devices[]`

| 字段 | 默认 | 说明 |
|---|---|---|
| `id` | 必填 | `^[a-z0-9][a-z0-9._-]{0,63}$` |
| `name` | — | 显示名（同时是 OPC UA 设备节点的 DisplayName） |
| `channelId` | 必填 | 必须指向已存在的通道 |
| `enabled` | `true` | |
| `interval` | `1s` | 采集周期，`>= 10ms` |
| `timeout` | 通道超时 | 本设备的单次请求超时 |
| `retries` | `0` | 0..10 |
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
| `deadband` | 不启用 | 死区（绝对值）；`>= 0` |
| `unit` | — | 单位，原样带到北向 |
| `writable` | `true` | 只读点位在驱动层就被拒，不会发到线上 |
| `enabled` | `true` | |
| `length` | `0` | `string`/`bytes` 的字节长度（1..1024） |

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
`float32`、`float64`、`string`、`bcd16`、`bcd32`、`bytes`。

写法和北向值里的 `type` 完全一样（小写）。旧版本配置文件里的 `uInt16` 之类的拼写仍能读入。

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
| `journal` | `false` | 额外的本地 NDJSON 兜底通道 |
| `outboxMaxRecords` | `100000` | 队列条数上限 |
| `outboxMaxBytes` | `268435456` | 队列字节上限（256 MiB） |
| `outboxTtl` | `24h` | 超过这个时长的记录直接丢弃 |
| `outboxBatchSize` | `64` | 一次投递的最大条数 |
| `outboxFlushInterval` | `1s` | 后台冲刷间隔 |

超限时丢最旧的并计数，`/api/v1/health` 与指标里都能看到丢弃数。

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
  outbox/
    <sink>/records.ndjson
    <sink>/ack
```

- **内容寻址**：`id = "v" + sha256(canonical json)[..12]`，同样的内容永远得到同一个 id；
- **只增不改**：提交新版本不会动历史；回滚是把旧内容作为一次新提交，因此也能再回滚；
- **启动优先级**：`current` 指针 > `--config` 指定的文件 > 内置兜底配置。
  运维回滚过之后重启，不会把旧文件重新捡回来；
- **热更新**：按设备粒度 diff，只重启受影响的设备，其余采集不中断。

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
