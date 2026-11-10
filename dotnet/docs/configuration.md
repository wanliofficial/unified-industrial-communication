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
    "rest": { "enabled": true, "listen": "0.0.0.0:8080", "token": "${env:UIC_REST_TOKEN}" },
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
| `protocol` | 必填 | `modbus-tcp` / `modbus-rtu` / `modbus-ascii` / `s7` / `virtual` |
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

### S7 通道

`protocol: "s7"` 只走 ISO-on-TCP，`transport` 必须是 `tcp`，端口默认 **102**（不是 502）。
建链时驱动会完成 COTP 连接与 PDU 协商，协商值以 PLC 回应为准（240/480/960），
之后读请求按它自动切片——点位多的设备不需要人工拆窗口。

```json
{
  "id": "s7-1",
  "name": "1 号 PLC",
  "protocol": "s7",
  "transport": "tcp",
  "host": "192.168.1.10",
  "port": 102,
  "timeout": "2s"
}
```

S7 的会话参数放在**设备**的 `labels` 里（它们是 COTP 建链参数，不是通道参数）：

| 键 | 默认 | 说明 |
|---|---|---|
| `rack` | `0` | 机架号 0..7；S7-1200/1500 通常是 0 |
| `slot` | `1` | 槽位号 0..31；S7-1200/1500 通常是 1，S7-300 常见 2 |
| `pduSize` | `480` | 请求的 PDU 长度 240..960；**实际以 PLC 回应为准**，只有抓包联调才需要改 |

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
| `unitId` | `1` | Modbus 从站地址 1..255 |
| `byteOrder` | `abcd` | `abcd` / `cdab` / `badc` / `dcba`，点位可覆盖 |
| `failureThreshold` | `3` | 连续失败多少次进入降级 |
| `recoverInterval` | `30s` | 降级后探活间隔 |
| `labels` | `{}` | 驱动私有参数 |
| `points` | 必填 | 至少一个 |

`retries` 是「一个采集周期内的重试」，不是「无限重连」：次数用完后本轮按失败上报，
下一轮仍按 `interval`/退避节奏开始。重试前会先断开连接，避免超时后迟到的响应
被当成新请求的回复。`address` 类错误（点表地址不存在/越界）既不重试也不计入
`failureThreshold`，否则一个配错的点会让整台设备被熔断。

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

#### S7 的区名与偏移

`protocol: "s7"` 的通道用同一套 `register` 结构，但 `area` 是 **S7 的区名**，
`offset` 是**字节**偏移而不是寄存器序号：

| `area` | 含义 | 可写 | 例子 |
|---|---|---|---|
| `db<N>` | 数据块 N（1..65535） | 是 | `db1` + `offset: 16` = `DB1.DBB16` |
| `i`（别名 `e`） | 输入区 | 否 | `i` + `offset: 2` |
| `q`（别名 `a`） | 输出区 | 是 | `q` + `offset: 0` |
| `m`（别名 `merker`） | 标志区 | 是 | `m` + `offset: 6` |
| `l` | 局部数据 | 视 CPU 而定 | |
| `p`（别名 `pe`） | 外设区 | 视 CPU 而定 | |
| `c` / `t` | 计数器 / 定时器 | 视 CPU 而定 | |

```json
{ "id": "temperature", "dataType": "float64",
  "address": { "kind": "register", "area": "db1", "offset": 0 } }
{ "id": "running", "dataType": "bool",
  "address": { "kind": "register", "area": "m", "offset": 0, "bit": 3 } }
```

- **为什么是字节偏移**：S7 的地址本来就是 `DB1.DBW4` 这种「字节 + 位」，
  而 `DBW3` 这类奇数字节偏移在现场很常见。按寄存器组织地址会在每个奇地址上
  做二次换算，那正是把 Modbus 思维搬过来最容易错的地方。
- **`bit` 限 0..7**：S7 的一个字节只有 8 位。`bit` 超过 7 会被校验拒绝。
- **`bool` 点位必须带 `bit`**：整字节非零不等于「ON」，猜出来的值比读不出更危险。
- `length` 对 `string`/`bytes` 表示字节数；对定宽数值类型仍然由类型推导。
- 读请求会按协商出的 PDU 自动切片（请求项 12 字节 + 响应项都计入预算）。
  单个点位本身就装不下时**只让那个点位失败**并说明 PDU 上限，不拖垮整窗。
- 符号寻址（`kind: "symbol"` 的 0xB2）与 S7Plus 明确未实现，配置成那样会在运行期明确报错。

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
| `token` | 空 | 静态 API Token（给运维脚本用，全权限、不过期）；只能是凭据引用，见 [凭据](#凭据)。为空只表示「不提供静态令牌」——本地账号始终必需，不存在匿名访问 |
| `staticRoot` | 空 | 前端静态目录；为空时用宿主目录下的 `wwwroot` |
| `tls` | `{ "enabled": false }` | 内置 HTTPS，见 [`rest.tls`](#resttls) |

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
| `tls` | `{ "enabled": false }` | 客户端 TLS，见 [`mqtt.tls`](#mqtttls) |

主题模板必须含 `{device}` 与 `{point}`，且以 `topicPrefix` 开头或含 `{site}` ——
否则同一套主题可能把两个站点的数据混在一起。

### `rest.tls`

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `false` | 打开后 Kestrel 按 HTTPS 监听；端口仍取 `rest.listen` |
| `certificateFile` | 空 | **必填**。PFX/P12，或含证书（与可选私钥）的 PEM |
| `certificatePassword` | 空 | PFX 口令；只能是凭据引用，见 [凭据](#凭据) |
| `keyFile` | 空 | PEM 私钥文件；证书自带私钥（PFX、或证书与私钥在同一个 PEM）时留空 |

- 路径是**文件路径**，不是凭据：写成 `${env:...}` / `enc:v1:` 会被校验拒绝。
  相对路径按 `storage.dataDir` 解析，与账号、审计、outbox 的基准一致；
- 证书在启动时装载，读不出来按「配置非法」退出（退出码 2），不会带着明文端口继续跑；
- 证书与监听地址的变更需要**重启**：它们属于 Kestrel 的启动参数，`config/apply` 只会
  在日志里明确告警「REST 的 TLS 设置没有生效」；
- 只做服务端证书，不做 mTLS（不校验客户端证书）。需要双向认证时在前置代理上做。

```json
{
  "rest": {
    "enabled": true,
    "listen": "0.0.0.0:8443",
    "tls": {
      "enabled": true,
      "certificateFile": "security/gateway.pfx",
      "certificatePassword": "${env:UIC_TLS_PFX_PASSWORD}"
    }
  }
}
```

### `mqtt.tls`

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `false` | 打开后整条连接走 TLS（通常接 Broker 的 8883） |
| `caFile` | 空 | 自定义信任根（PEM）；为空时用系统信任库。自签 Broker 必须在这里给出 CA |
| `insecureSkipVerify` | `false` | 跳过证书链与主机名校验；**只能联调**，且要求 `enabled: true` |

- 校验服务端证书链与主机名；`caFile` 通过自定义信任根（`CustomRootTrust`）参与链构建，
  不会因为「用了自签证书」就顺手关掉主机名校验；
- `insecureSkipVerify: true` 启动时在日志里明确告警一次：流量加密了，但对端没有被认证；
- CA 与开关的变更在 `config/apply` 时重建客户端并重连后生效（不需要重启进程）；
- 只做服务端认证，不做客户端证书；MQTT 命令这一版没有用户身份，TLS 只保护链路。

## 凭据

凭据字段有三个：`northbound.rest.token`、`northbound.mqtt.password` 与
`northbound.rest.tls.certificatePassword`。配置文件里**不允许出现明文**：
启动、`uic check`、`config/validate` 与 `config/apply` 都会拒绝，并指出具体字段路径。

证书文件与 CA 文件**不是**凭据，它们是文件路径：不要写成 `${env:...}`，否则校验会
以「must be a file path」拒绝。路径可以相对 `storage.dataDir`。

### 三种写法

| 写法 | 例 | 取值来源 | 适用 |
|---|---|---|---|
| 环境变量 | `${env:UIC_REST_TOKEN}` | 进程环境 | compose / systemd `EnvironmentFile` / K8s `env` |
| 文件 | `${file:/run/secrets/rest_token}` | 文件内容（去掉行尾换行） | Docker secret、K8s `secret` 挂载 |
| 密文 | `enc:v1:AQID...` | AES-256-GCM，密钥见下 | 配置要能整体拷走、但不想让凭据以明文躺在文件里 |

```bash
export UIC_REST_TOKEN='...'          # 方式一
uic check --config gateway.json      # 会打印每项引用的来源，确认都解析得出来

uic secret keygen --out /etc/uic/secret.key   # 方式三：32 字节密钥，尽力设为 0600
export UIC_SECRET_KEY_FILE=/etc/uic/secret.key
printf '%s' '...' | uic secret seal  # 输出 enc:v1:...，把它填进配置
```

`uic secret keygen` 不带 `--out` 时打印到 stdout，便于
`uic secret keygen > key.b64`。密钥也可以直接放在 `UIC_SECRET_KEY` 里
（base64 或 hex，32 字节）。文件系统不记录权限位时（exFAT、部分 NTFS/FUSE
挂载）keygen 会明确告警，而不是假装设成了 0600。

### 解析失败必须硬失败

环境变量拼错、密钥文件没挂上、密文被改动，都直接失败并说明原因。
**不要**退回空串：空的静态令牌并不会关闭鉴权（账号始终必需），它只会让依赖静态令牌的
运维脚本在第一次请求时收到 401——那是最坏的一种「看起来正常」。

### 明文只在内存里

落盘的配置版本、`config/diff`、`config/export`、诊断包与 `GET /api/v1/config`
里只有引用（读接口统一显示 `***`）；解析出的明文只存在进程内存中，`GET /api/v1/config`
返回的 `***` 在提交时会被还原成原来的引用，而不是明文。

旧配置里的明文令牌仍能被**读**（为了迁移），但任何一条生效路径都会拒绝它。
迁移方式：改成 `${env:...}` / `${file:...}`，或用 `uic secret seal` 加密。

命令主题与结果主题同样以 `topicPrefix` 开头或含 `{site}`。`commands` 默认关闭：
这一版 MQTT 命令没有用户身份（TLS 只保护链路，不认证发布者），开启前请确认
Broker 侧已经做了网络隔离或 ACL。即便打开，`config.rollback` 与 `outbox.drop`
也会被拒绝，只能走 REST。
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
| `audit.enabled` | `true` | 审计落盘开关。默认开启：审计是「谁改了什么」的唯一答案，默认关闭等于没有 |
| `audit.maxBytes` | `8388608` | 单个审计文件的上限（≥1024）；写满后开新文件 |
| `audit.retain` | `6` | 最多保留几份（含当前文件），范围 `1..1000` |
| `audit.directory` | `audit` | 相对 `dataDir` 的子目录；也接受绝对路径 |

超限时丢最旧的并计数，`/api/v1/health` 与指标里都能看到丢弃数。

### 审计

审计与文件日志是两件事：日志会按级别过滤、会轮转、会丢；审计是事后追责的落盘记录，
默认开启且每条写入后立即 flush。文件按天与按大小切分（一天内写满就换新文件），
按 `retain` 只保留最近几份，最旧的文件先删——保留策略只删旧文件，绝不会删正在写的那个。

```json
{
  "storage": {
    "dataDir": "/var/lib/uic",
    "audit": {
      "enabled": true,
      "maxBytes": 8388608,
      "retain": 6,
      "directory": "audit"
    }
  }
}
```

- 目录随 `dataDir` 走，相对路径按 `dataDir` 解析；`enabled: false` 时只保留内存里的序号，
  不写盘、不创建目录，已有文件不会被删除。
- 配置可以热更新：改目录 / 上限 / 开关后立即生效，不必重启进程。改动审计设置的那一次
  `config.apply` 本身会记在新的目录里。
- 写失败（磁盘满、权限错）只告警一次并把指标 `uic_audit_write_failures` 置为非零，
  业务继续：用观测问题制造一次写入失败或配置回滚失败，代价更大。
- 查询与导出见 [northbound-api.md](northbound-api.md#审计)。

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
  security/
    users.json                  # 本地账号（PBKDF2 派生值，无机密文）
    initial-admin-password.txt  # 仅首次启动生成；admin 改密成功后自动删除
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

`security/` 同样独立于配置版本：账号是运行期状态，回滚配置不应把口令也一起回滚。

- **内容寻址**：`id = "v" + sha256(canonical json)[..12]`，同样的内容永远得到同一个 id；
- **只增不改**：提交新版本不会动历史；回滚是把旧内容作为一次新提交，因此也能再回滚；
- **启动优先级**：`current` 指针 > `--config` 指定的文件 > 内置兜底配置。
  运维回滚过之后重启，不会把旧文件重新捡回来；
- **热更新**：按设备粒度 diff，只重启受影响的设备，其余采集不中断。

改配置有三条等效入口：直接编辑文件后启动/`config/apply`、控制台的配置页（编辑 JSON）、
控制台设备页的「新建设备」。三者最后都落到同一个提交函数上（解析 → 校验 → 内容寻址 →
按设备 diff 重启），因此版本号、回滚与 `/config/versions` 历史完全一致；设备页只是把
「追加一条通道 + 一台设备 + 首个点位」这一小段拼装做成了表单。

### 账号与初始口令

首次启动（`data/security/users.json` 不存在）时会：

1. 创建 `admin`（角色 `admin`），口令是 16 位随机串；
2. 把口令写进 `data/security/initial-admin-password.txt`（尝试 `0600`，文件系统不支持
   Unix 权限位时启动日志会告警）；
3. 给 `admin` 打上「必须改密」标记：改密之前除 `auth/me`、`auth/password`、`auth/logout`
   之外的请求都返回 `403 auth.password_change_required`；
4. 改密成功后删除初始口令文件——它继续留在磁盘上等于口令从未改过。

会话 Token 只保存在进程内存（12 小时），进程重启即失效；账号与口令派生值落盘，
口令本身从不落盘。账号库损坏时启动直接失败，不会悄悄重建覆盖现场账号。

忘记管理员口令时，在网关本机执行：

```bash
uic user list                          # 列出账号、角色、启用状态、最近登录
uic user reset-password --user admin   # 重置为一次性口令，口令走 stdout，提示走 stderr
```

重置后的口令同样带强制改密标记；`uic user reset-password` 与 REST 的管理员重置走同一份
存储与派生实现，不会出现「命令行能登录、REST 不认」的分叉。

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

`server.token` 迁移成引用之前的明文令牌仍能读进来（迁移不能凭空造一个环境变量），
但 `check`、启动与 apply 都会拒绝它，见 [凭据](#凭据)。

## 校验

`uic check` 与 `POST /api/v1/config/validate` 走的是同一份校验器（`ModelValidator`），
所以「控制台能存、重启起不来」不会发生。凭据引用由 `Secrets.Validate` 另外检查
（环境变量是否存在、文件是否可读、密文能否解开），同样是两边一致。

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

成功时会额外打印每个凭据字段的来源（`environment variable X` / `file Y` /
`sealed (enc:v1)` / `not set`），便于确认「配置里写的是哪个变量、它到底有没有注入」：

```
ok: ./gateway.json
  site: demo-site
  channels: 1, devices: 1, points: 2
  secret northbound.rest.token: environment variable UIC_REST_TOKEN
  secret northbound.mqtt.password: not set
  secret northbound.rest.tls.certificatePassword: not set
  northbound: rest(https://0.0.0.0:8443), mqtts(broker.local:8883)
```

最后一行把**实际监听方案**写出来：`rest(...)` 带 `http://` 或 `https://`，
`mqtt(...)` 与 `mqtts(...)` 区分是否走 TLS。部署评审看的是这一行，
而不是「配了 8443 端口」这种猜测。
