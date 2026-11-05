# 领域模型

> **版本**：v0.1
> **上游**：`01-prd.md`、`02-architecture.md`
> **代码对应**：`internal/model`（类型与不变量）、`internal/config`（持久化形态）
> **原则**：模型是共同语言。驱动、调度、北向、UI 都只说这套语言，不允许各自定义「设备」「点位」的变体。

---

## 1. 模型全景

```
Site（站点）
 └── Channel（通道：一条链路）
      └── Device（设备：一个从站）
           └── Point（点位：一个数据项）
                └── Sample（采样：值 + 质量 + 时间）

Template（设备模板）──实例化──→ Device
```

| 实体 | 身份 | 生命周期 | 归属 |
|---|---|---|---|
| Site | `site` 字符串 | 与配置同生命周期 | 根 |
| Channel | `id`（站点内唯一） | 配置变更时重建 | Site |
| Device | `id`（全局唯一） | 配置变更时可单独重启 | Channel |
| Point | `id`（设备内唯一） | 可增量增删 | Device |
| Sample | `(device, point, seq)` | 瞬时（只存在快照与队列中） | Point |

**为什么 Channel 必须是实体而不是「连接字符串」**：因为并发上限、串口互斥、重连退避、熔断都以通道为边界。把通道降级为设备的一个字段，会导致「同一串口被两个设备同时写」这类问题无解（PA-10 正是用「通道 + 全局串口锁」解决的）。

---

## 2. 地址模型（五类寻址）

### 2.1 类型定义

```go
type AddressKind string

const (
    AddressRegister AddressKind = "register" // 区 + 偏移 + 长度 + 位偏移
    AddressSymbol   AddressKind = "symbol"   // 路径 + 下标
    AddressObject   AddressKind = "object"   // 对象类型 + 实例 + 属性 + 索引
    AddressFile     AddressKind = "file"     // 文件 + 记录 + 偏移
    // AddressComputed 没有物理地址：值由表达式算出（计算点）。
    // 表达式放在 Address 而不是 Point 上，是因为 driver.Read 只拿到
    // PointRef（ID + Address + DataType + Length）；放 Point 会迫使所有
    // 驱动都接收一个带表达式的结构，或给 PointRef 加一个只有一种驱动
    // 使用的字段。
    AddressComputed AddressKind = "computed"
)

type Address struct {
    Kind AddressKind `json:"kind"`

    // register
    Area   string `json:"area,omitempty"`   // holding | input | coil | discrete | db | cio | dm ...
    Offset int    `json:"offset,omitempty"`
    Length int    `json:"length,omitempty"` // 寄存器个数（字），0 表示由数据类型推导
    Bit    int    `json:"bit,omitempty"`    // -1 表示整字访问

    // symbol
    Path  string `json:"path,omitempty"`  // DB1.DBW0 / Program:Main.Tag / NS=2;s=... / plant/line1/temp
    Index int    `json:"index,omitempty"` // 数组下标，-1 表示非数组
    // Selector 是符号地址**内部**的二级选择器：MQTT 南向用它指向 JSON 载荷
    // 里的字段（"data.temperature" / "values[0].temp"）；其他符号型协议留空。
    //
    // 为什么不塞进 Path：主题里允许出现 `#` 与 `+`，用分隔符拼接会与 MQTT
    // 通配符撞车；而且「订阅哪个主题」与「取载荷里哪一段」是两件事，
    // 混成一个字符串后无法分别校验。
    Selector string `json:"selector,omitempty"`

    // object
    ObjectType string `json:"objectType,omitempty"` // analogInput / ...
    Instance   int    `json:"instance,omitempty"`
    Property   string `json:"property,omitempty"` // presentValue / ...
    PropIndex  int    `json:"propIndex,omitempty"`

    // file
    FileID   int `json:"fileId,omitempty"`
    RecordID int `json:"recordId,omitempty"`

    // computed
    // Expr 是计算点表达式，引用写作 `device:point`。用冒号而不是点号：
    // 设备 ID 与点位 ID 都允许包含点号（`line.a` 合法），用点号分隔会
    // 产生「a.b.c 指谁」的歧义。ID 也允许 `-`，因此**减号两边要留空格**
    // （`line-a:power-1` 是引用，`line-a:power - 1` 才是减法）。
    Expr string `json:"expr,omitempty"`
}
```

### 2.2 规范化字符串（Canonical）

平台统一生成，驱动不得各自实现。用途：唯一性校验、日志、北向节点名、订阅表达式。

| 类型 | 格式 | 示例 |
|---|---|---|
| register | `{area}:{offset}[.{bit}]:{len}` | `holding:100:2`、`coil:20.3:1` |
| symbol | `{path}[{index}]`（带二级选择器时追加 `#{selector}`） | `DB1.DBW0`、`Program:Main.Tags[3]`、`plant/line1/state#running` |
| object | `{type}:{instance}.{property}[{index}]` | `analogInput:1.presentValue` |
| file | `file:{fileId}/{recordId}:{offset}` | `file:1/2:0` |
| computed | `expr:{表达式原文}` | `expr:line-a:power * line-a:rate` |

**不变量**：

1. `register.Offset >= 0`，`Length >= 1`（推导后）；
2. `register.Bit` 取 `-1`（整字）或 `0..15`；
3. `symbol.Path` 非空，不含控制字符与空格；`symbol.Selector`（可空）同样不含控制字符与空格；
4. `object.Instance >= 0`，`ObjectType` 与 `Property` 非空；
5. 各类字段不得混填（校验时按 `Kind` 只检查对应字段，其余必须为零值）；
6. `computed.Expr` 非空、≤1024 字节、不含控制字符；对应点位必须**只读**且
   类型为数值或布尔——允许可写会产生一条永远失败的写入路径，允许字符串
   则得到一个没有任何运算符可消费的值。

### 2.3 为什么不用 `string` 地址

参考方案里大量使用 `Address string`（如 `"40001"`、`"DB1.DBW0"`），由驱动自己解析。代价：

- 无法在配置期校验（错误延后到运行期）；
- 无法跨协议比较与排序（批量规划失效）；
- 无法统一生成北向节点名；
- 每个驱动重复实现解析与报错。

改用结构化类型后，`40001` 这类写法仍可通过「地址解析器」导入，但**内部只存结构化形式**。

---

## 3. 数据类型与字节序

### 3.1 类型集合

| 类别 | 类型 | 字节 | 说明 |
|---|---|---|---|
| 布尔 | `bool` | 1 位 | 线圈/离散输入/位地址 |
| 有符号 | `int16` `int32` `int64` | 2/4/8 | 大端为主 |
| 无符号 | `uint16` `uint32` `uint64` | 2/4/8 | Modbus 寄存器默认 |
| 浮点 | `float32` `float64` | 4/8 | IEEE 754 |
| 文本 | `string` | N | 需指定长度与编码（ASCII/UTF-8/GBK） |
| 编码 | `bcd16` `bcd32` | 2/4 | 表计常用 |
| 时间 | `datetime` | 4–7 | 协议自定义（如 CP56Time2a） |
| 数组 | `<type>[n]` | n× | 如 `float32[4]` |

### 3.2 字节序与字序

32 位值由两个 16 位寄存器组成时，有四种排列：

| 代号 | 寄存器顺序 | 字节序 | 典型设备 |
|---|---|---|---|
| `ABCD` | 高字在前 | 大端 | 多数 PLC |
| `CDAB` | 低字在前 | 大端 | 部分电表、西门子旧设备 |
| `BADC` | 高字在前 | 小端 | 少见 |
| `DCBA` | 低字在前 | 小端 | 部分仪表 |

**规则**：

1. 字节序策略属于**点位级属性**，可继承设备级默认；
2. 64 位值扩展为 `ABCDEFGH` / `GHEFCDAB` 等 8 种，实现上按 16 位字重排；
3. 字符串不参与字序重排（只按字节序）；
4. 位地址（`Bit >= 0`）不参与任何重排。

> 现场 80% 的「读出来是乱的」源于此。PA-24 的 `ByteTransform` 与 PA-32 的 `ByteData` 都是对这一问题的回答，但都只做到「工具函数」，没有做到「点位属性」。

---

## 4. 点位（Point）

```go
type Point struct {
    ID          string   `json:"id"`
    Name        string   `json:"name,omitempty"`
    Address     Address  `json:"address"`
    DataType    DataType `json:"dataType"`
    ByteOrder   ByteOrder `json:"byteOrder,omitempty"` // 空则继承设备
    Unit        string   `json:"unit,omitempty"`
    Scale       float64  `json:"scale,omitempty"`      // 默认 1
    Bias        float64  `json:"bias,omitempty"`       // 默认 0
    Deadband    float64  `json:"deadband,omitempty"`   // 0 表示不启用
    Interval    Duration `json:"interval,omitempty"`   // 0 表示继承设备
    Writable    bool     `json:"writable,omitempty"`
    Enabled     bool     `json:"enabled"`
    Description string   `json:"description,omitempty"`
    Tags        []string `json:"tags,omitempty"`
}
```

### 4.1 不变量

| # | 不变量 | 违反后果 |
|---|---|---|
| I1 | `ID` 匹配 `^[a-z0-9][a-z0-9._-]{0,63}$` | 北向 Topic/节点名非法 |
| I2 | 设备内 `ID` 唯一 | 快照覆盖 |
| I3 | `Scale != 0` | 所有值变 0 |
| I4 | `Interval` 为 0 或 ≥ 100ms | 打爆设备 |
| I5 | `Writable=false` 的点位拒绝写请求 | 误操作 |
| I6 | `Address.Kind` ∈ 驱动声明的 `acceptedAddressKinds` | 运行期必然失败 |
| I7 | `DataType` 与 `Address.Length` 兼容 | 越界读 |

### 4.2 派生规则

| 派生值 | 规则 |
|---|---|
| 有效周期 | `point.Interval > 0 ? point.Interval : device.Interval` |
| 有效字节序 | `point.ByteOrder != "" ? point.ByteOrder : device.ByteOrder` |
| 序列号 | 每设备单调递增 `uint64`，回绕时从 0 继续并记录事件 |
| 快照键 | `device.ID + "/" + point.ID` |

---

## 5. 设备（Device）

```go
type Device struct {
    ID         string   `json:"id"`
    Name       string   `json:"name,omitempty"`
    ChannelID  string   `json:"channel"`
    UnitID     int      `json:"unitId,omitempty"`     // 站号/从站地址
    Interval   Duration `json:"interval"`             // 默认采集周期
    Timeout    Duration `json:"timeout,omitempty"`    // 单次请求超时
    ByteOrder  ByteOrder `json:"byteOrder,omitempty"`
    Enabled    bool     `json:"enabled"`
    Points     []Point  `json:"points"`
    TemplateID string   `json:"templateId,omitempty"`
    Labels     map[string]string `json:"labels,omitempty"`
}
```

### 5.1 设备状态机

| 状态 | 含义 | 进入条件 | 退出条件 |
|---|---|---|---|
| `configured` | 已配置未启动 | 加载配置 | `start` |
| `connecting` | 正在建连 | 启动/重连 | 成功→`collecting`；失败→`degraded` |
| `collecting` | 正常采集 | 连接成功 | 连续失败 ≥ 3 → `faulted`；`stop` → `disabled` |
| `degraded` | 降级（部分点位失败） | 单点错误 | 全部恢复→`collecting` |
| `faulted` | 熔断 | 连续失败 ≥ 3 | 探活成功→`connecting`；`stop`→`disabled` |
| `disabled` | 已停止 | `stop` / 配置禁用 | `start` |

**规则**：状态变更必须产生一条日志 + 一次指标更新；状态跃迁必须带原因（`reason`）。

### 5.2 设备内并发不变量

同一设备的 `Read` / `Write` 不得并发。实现层用「每设备 mailbox 容量 1」保证；驱动层不得假设自己会被并发调用。

---

## 6. 通道（Channel）

```go
type Channel struct {
    ID          string    `json:"id"`
    Protocol    string    `json:"protocol"`     // modbus-tcp / modbus-rtu / s7 / opcua / mqtt / virtual
    Transport   Transport `json:"transport"`
    Concurrency int       `json:"concurrency,omitempty"` // 默认 4，范围 1..64
    Timeout     Duration  `json:"timeout,omitempty"`     // 默认 3s
    // Username/Password 是**链路级**凭据（MQTT CONNECT、未来的用户名认证）。
    // 放在通道而不是设备上：凭据属于 Broker / 服务端，同一通道下的多台设备
    // 共用一份。配置文件权限要求 0600，REST 回显时只回答「是否配置」。
    Username    string    `json:"username,omitempty"`
    Password    string    `json:"password,omitempty"`
}

type Transport struct {
    Kind     string `json:"kind"`     // tcp | serial | udp | memory
    Address  string `json:"address,omitempty"` // host:port
    Port     string `json:"port,omitempty"`    // COM3 / /dev/ttyUSB0
    Baud     int    `json:"baud,omitempty"`
    DataBits int    `json:"dataBits,omitempty"`
    Parity   string `json:"parity,omitempty"` // none|even|odd
    StopBits int    `json:"stopBits,omitempty"`
    TLS      bool   `json:"tls,omitempty"`
}
```

### 6.1 通道不变量

1. `Concurrency ∈ [1, 64]`；
2. 串口通道的 `(Port, Baud, DataBits, Parity, StopBits)` 必须一致——若两个通道指向同一物理端口但参数不同，启动即拒绝（PA-06 的 `SharedSerialChannelManager` 做法）；
3. `Protocol` 必须已注册；
4. 同一通道下的设备共享连接（若驱动支持）。

---

## 7. 采样（Sample）与质量码

```go
type Sample struct {
    DeviceID    string    `json:"device"`
    PointID     string    `json:"point"`
    Value       Value     `json:"value"`
    Quality     Quality   `json:"quality"`
    DeviceTime  *time.Time `json:"deviceTime,omitempty"`
    CollectedAt time.Time `json:"collectedAt"`
    Seq         uint64    `json:"seq"`
    Unit        string    `json:"unit,omitempty"`
    Error       string    `json:"error,omitempty"`
}
```

### 7.1 质量码

| 常量 | 值 | 语义 | 是否参与计算 | 是否上报 |
|---|---|---|---|---|
| `QualityGood` | `good` | 可信 | ✓ | ✓ |
| `QualityUncertain` | `uncertain` | 可疑（写入未确认/跳变） | ✗ | ✓（标记） |
| `QualityStale` | `stale` | 超过 3 个周期未更新 | ✗ | ✓（标记） |
| `QualityBad` | `bad` | 解析失败/类型不符 | ✗ | 仅事件 |
| `QualityTimeout` | `timeout` | 请求超时 | ✗ | 仅事件 |
| `QualityCommError` | `comm_error` | 连接断/CRC 错/异常响应 | ✗ | 仅事件 |
| `QualityAddressError` | `address_error` | 地址不存在/越界 | ✗ | 仅事件 |
| `QualityNotConnected` | `not_connected` | 未连接 | ✗ | ✗ |
| `QualityDisabled` | `disabled` | 已禁用 | ✗ | ✗ |

### 7.2 状态转换（质量码）

```
not_connected ──连接成功──→ good
good ──超时──→ timeout ──恢复──→ good
good ──异常码 02──→ address_error ──配置修复──→ not_connected
good ──写入未确认──→ uncertain ──下次采集确认──→ good
good ──3 个周期未更新──→ stale
任意 ──点位禁用──→ disabled
```

**规则**：`stale` 由「最后采集时间 + 3×interval」推导，不由驱动上报。

---

## 8. 批读规划

### 8.1 目标

把 N 个离散点位合并为 M 个连续区间请求，满足：

1. 单请求不超过协议上限（Modbus：125 寄存器 / 2000 线圈）；
2. 区间内「空洞」不超过阈值（默认 8 个寄存器），否则拆分为多个请求；
3. 位地址按所属字聚合；
4. 不同数据类型不合并（除非类型一致且长度可推导）；
5. 保持请求内点位顺序，便于回填。

### 8.2 算法（寄存器型）

```
输入：points（按 address.offset 排序，已过滤 disabled）
输出：spans

spans = []
cur = nil
for p in points:
    start = p.addr.offset
    end   = start + p.length
    if cur == nil:
        cur = span(start, end, [p])
    elif end - cur.start <= limit and start - cur.end <= gap:
        cur.end = max(cur.end, end); cur.points.append(p)
    else:
        spans.append(cur); cur = span(start, end, [p])
spans.append(cur)
```

复杂度 `O(n log n)`（排序）+ `O(n)`（扫描）。

### 8.3 符号型与对象型

| 类型 | 分组策略 |
|---|---|
| 符号 | 按 `Path` 的最长公共前缀分组；同组一次请求；不推断地址连续性 |
| 对象 | 按 `Instance` 分组；同实例多属性一次读；跨实例按批量上限切分 |
| 文件 | 按 `FileID + RecordID` 分组；按偏移排序后合并 |

### 8.4 规划质量指标

| 指标 | 含义 | 目标 |
|---|---|---|
| `plan_spans` | 本次规划的请求数 | 越少越好 |
| `plan_coverage` | 有效字节 / 请求字节 | > 0.6 |
| `plan_skipped` | 因上限被拆分的点数 | 0（理想） |

规划结果缓存，配置变更或点位集合变化时失效。

---

## 9. 管线（Pipeline）

```
raw（字节/数值）
  └→ decode（按 DataType + ByteOrder）
       └→ scale（×Scale + Bias）
            └→ validate（量程/枚举/NaN）
                 └→ quality（判定）
                      └→ deadband（与上次比较）
                           └→ Sample
```

### 9.1 死区规则

| 类型 | 规则 |
|---|---|
| 数值 | `abs(v - last) < deadband` → 不产生「变化事件」（仍更新快照） |
| 布尔 | 值变化即事件 |
| 字符串 | 值不等即事件 |
| 质量变化 | 总是事件（不受死区约束） |

### 9.2 质量传播

1. 输入质量为 `bad/timeout/comm_error/address_error` → 输出直接沿用，不参与缩放；
2. `uncertain` → 参与缩放但标记；
3. 计算点（Wave 2）：任一输入非 `good` → 输出 `uncertain`，并在 `error` 中列出污染源。

---

## 10. 配置文件形态

```json
{
  "schemaVersion": 1,
  "site": "plant-a",
  "server": { "listen": ":8080", "tls": { "enabled": false } },
  "channels": [
    {
      "id": "ch-plc",
      "protocol": "modbus-tcp",
      "transport": { "kind": "tcp", "address": "192.168.1.10:502" },
      "concurrency": 4,
      "timeout": "3s"
    }
  ],
  "devices": [
    {
      "id": "plc-01",
      "channel": "ch-plc",
      "unitId": 1,
      "interval": "1s",
      "enabled": true,
      "points": [
        {
          "id": "temp",
          "address": { "kind": "register", "area": "holding", "offset": 100, "length": 2, "bit": -1 },
          "dataType": "float32",
          "byteOrder": "CDAB",
          "unit": "℃",
          "scale": 1,
          "deadband": 0.1,
          "writable": false,
          "enabled": true
        }
      ]
    }
  ],
  "northbound": {
    "mqtt": { "enabled": false, "broker": "tcp://127.0.0.1:1883", "topic": "uic/{site}/{device}/{point}/value", "qos": 1 },
    "opcua": { "enabled": false, "listen": ":4840" }
  },
  "storage": {
    "dir": "./data",
    "outboxMaxBytes": 268435456,
    "outboxMaxRecords": 200000,
    "outboxTtl": "24h",
    "journal": true
  },
  "logging": { "level": "info", "format": "json" }
}
```

### 10.0.1 `storage` 字段

| 字段 | 默认 | 含义 |
|---|---|---|
| `dir` | `./data` | 数据根目录：快照、Outbox、状态文件、日志都落在这里 |
| `outboxMaxBytes` | 64 MiB | 队列字节上限，超限按「先丢最旧、保留变化点」降采样 |
| `outboxMaxRecords` | 200000 | 队列条数上限 |
| `outboxTtl` | `24h` | 单条记录最长存活时间，过期丢弃并计数 |
| `journal` | `false` | 开启本地 NDJSON 归档 sink，见下 |

**`journal` 的定位**：它是一个**兜底 sink**，不是北向协议。开启后每条通过死区过滤的变化事件都会追加到 `<dir>/journal.ndjson`（一行一个 JSON，含 `schemaVersion`），并走与 MQTT/Webhook 完全相同的 Outbox 路径（同样的分区、确认位点、上限与重试语义）。

它解决三个具体问题：

1. **断网留痕**：现场没有 Broker 时，至少能在本地留下「什么时候、哪个点、变成了什么」的证据链。
2. **可观测性**：开发与联调阶段不必先架一个 Broker 就能验证「采集 → 死区 → Outbox → 投递」整条链路是通的。
3. **可测试性**：CI 里可以直接断言落盘内容，而不需要引入外部依赖。

代价是磁盘写入。因此它默认关闭，且写入走批量 flush（≤64 条一次 `write`，`Stop` 时 `Sync`），不逐条 fsync。开启前应确认 `<dir>` 所在分区有足够空间，并配合 `outboxMaxBytes` 使用。

> 注意：`journal` 与 MQTT/Webhook 是**并列**关系，不是替代关系。同时开启时，同一条事件会分别进入各自的 sink 分区，各自独立确认，互不影响。

### 10.1 命名规则

| 对象 | 规则 | 示例 |
|---|---|---|
| Site | `^[a-z][a-z0-9-]{1,31}$` | `plant-a` |
| Channel | `^[a-z0-9][a-z0-9-]{0,31}$` | `ch-plc` |
| Device | `^[a-z0-9][a-z0-9._-]{0,63}$` | `plc-01`、`line1.plc2` |
| Point | 同 Device | `temp`、`motor.speed` |

**保留字**：`_system`、`_internal`、`uic` 不允许作为 ID 前缀外的完整 ID。

### 10.2 迁移

配置带 `schemaVersion`。加载时：

1. 版本等于当前 → 直接使用；
2. 版本小于当前 → 执行迁移链（`v1→v2→v3`），迁移结果写回并备份原文件；
3. 版本大于当前 → 拒绝启动，提示升级程序。

---

## 11. 模板（Template）

```json
{
  "id": "modbus-meter",
  "protocol": "modbus-tcp",
  "parameters": [
    { "name": "host", "type": "string", "required": true },
    { "name": "unitId", "type": "int", "default": 1 },
    { "name": "base", "type": "int", "default": 0 }
  ],
  "device": {
    "id": "meter-{index}",
    "interval": "5s",
    "points": [
      {
        "id": "voltage",
        "address": { "kind": "register", "area": "holding", "offset": "{base}", "length": 2, "bit": -1 },
        "dataType": "float32", "byteOrder": "CDAB", "unit": "V"
      }
    ]
  }
}
```

**规则**：

1. 参数替换只发生在**字符串字面量**中，不执行表达式（避免注入）；
2. 数值参数参与 `offset` 计算时必须是整数；
3. 实例化后必须通过完整校验（等价于手工配置）；
4. 模板定义与实例配置分开存储，模板变更不影响已实例化的设备（除非显式「同步模板」）。

---

## 12. 不变量汇总（实现检查单）

| # | 不变量 | 检查时机 | 错误码 |
|---|---|---|---|
| 1 | 所有 ID 符合命名规则 | 配置加载 | `config.invalid_id` |
| 2 | Device.ChannelID 存在 | 配置加载 | `config.channel_missing` |
| 3 | 点位地址类型被驱动接受 | 配置加载 | `config.address_kind_unsupported` |
| 4 | 点位 ID 设备内唯一 | 配置加载 | `config.duplicate_point` |
| 5 | Scale ≠ 0 | 配置加载 | `config.invalid_scale` |
| 6 | Interval ∈ [100ms, 24h] | 配置加载 | `config.invalid_interval` |
| 7 | 串口参数一致性 | 启动 | `config.serial_conflict` |
| 8 | 同设备不并发 | 运行期 | 由 mailbox 保证 |
| 9 | 代次匹配才写快照 | 运行期 | 丢弃 + 计数 |
| 10 | 写入需 Writable | API 层 | `point.not_writable` |
| 11 | 值类型与 DataType 一致 | 写入/解码 | `value.type_mismatch` |
| 12 | 队列不超上限 | 运行期 | 触发降级 |

---

## 13. 与参考方案的模型差异

| 维度 | 常见做法 | UIC |
|---|---|---|
| 地址 | 字符串，驱动自解析 | 四类结构化地址 + 规范字符串 |
| 类型 | 每协议一套枚举 | 统一类型 + 协议映射表 |
| 字节序 | 全局配置或工具函数 | 点位级属性 + 设备级默认 |
| 质量码 | 布尔成功/失败 | 9 态 + 传播规则 |
| 时间 | 单一时间戳 | 设备时间/采集时间/写入时间 |
| 设备状态 | 在线/离线 | 6 态 + 原因 |
| 模板 | 复制粘贴配置 | 参数化模板 + 校验 |

**核心判断**：模型的价值不在于「字段多」，而在于**每个字段都有明确的语义与校验位置**。38 个方案中最常见的失败模式是「字段存在但没有不变量」——于是配置能存进去，运行期才报错。
