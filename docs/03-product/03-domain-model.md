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

Alarm（报警规则）──引用──→ Point（可跨设备、可多条规则引用同一点位）
```

| 实体 | 身份 | 生命周期 | 归属 |
|---|---|---|---|
| Site | `site` 字符串 | 与配置同生命周期 | 根 |
| Channel | `id`（站点内唯一） | 配置变更时重建 | Site |
| Device | `id`（全局唯一） | 配置变更时可单独重启 | Channel |
| Point | `id`（设备内唯一） | 可增量增删 | Device |
| Sample | `(device, point, seq)` | 瞬时（只存在快照与队列中） | Point |
| Alarm | `id`（站点内唯一） | 配置变更时可单独重置 | Site（可引用任意设备的点位） |

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

## 10. 报警（Alarm）

报警是**有生命周期的领域对象**，不是「点位值超过阈值」的派生布尔。它有自己的
标识、级别、状态、确认人、计数与时间线，并且可以跨设备引用点位。

```go
type Rule struct {
    ID         string        // ^[a-z0-9][a-z0-9._-]{0,63}$
    Name       string        // 展示名
    Severity   Severity      // info | warning | critical
    Message    string        // 给人看的处置提示
    Disabled   bool          // 默认 false（启用）

    // 条件二选一：阈值 或 布尔表达式
    Point      string        // "device:point"
    High, Low  *float64
    Hysteresis float64
    Expr       string

    OnDelay    Duration      // 连续满足多久才报警
    OffDelay   Duration      // 连续不满足多久才清除
    AutoAck    bool          // 恢复即归位（纯提示类）
}

type Status struct {
    ID, Name, Severity, Message string
    State      State         // normal | pending | active | acked | cleared
    Active     bool          // active 或 acked
    Acknowledged bool
    Count      uint64        // 累计触发次数
    Value      *float64
    ValueKnown bool
    Quality    Quality
    Since      *time.Time    // 最近一次进入 active
    AckAt      *time.Time
    AckBy      string
    ClearedAt  *time.Time
    Reason     string        // 仅输入不可信时非空
}
```

### 10.1 状态机不变量

| 不变量 | 内容 |
|---|---|
| I-AL-1 | 状态只在五态之间迁移；不存在「未知」态——输入不可信时状态**保持不变** |
| I-AL-2 | `active` 与 `cleared` 都需要人确认；`acked` 的确认幂等 |
| I-AL-3 | `pending` 不可确认，且输入变坏时立即取消（未知时间不能凑够 `onDelay`） |
| I-AL-4 | `onDelay` / `offDelay` 是**连续**满足，中途抖动清零 |
| I-AL-5 | 滞回只在「维持/恢复」判定里生效，触发判定永远用原始阈值 |
| I-AL-6 | 每次 `normal|cleared → active` 迁移 `count++`，并清空上一次的确认信息 |
| I-AL-7 | `AutoAck` 只影响恢复路径：`clear → normal` 时自动写 `ackBy="auto"` |
| I-AL-8 | 状态只存内存；重启后仍成立的条件经 `onDelay` 重新报警（不做状态恢复） |
| I-AL-9 | 规则变更按「判定条件哈希」决定保留或重置；改名字/说明不重置；停用/启用会重置 |
| I-AL-10 | 删除规则必须发 `removed` 事件，让消费方清理自己的记录与 retained 主题 |

### 10.2 与点位质量的关系

| 输入 | 阈值规则 | 表达式规则 |
|---|---|---|
| `good` | 正常判定 | 正常判定 |
| `uncertain` | 正常判定（值可信，可能不新鲜） | 正常判定 |
| `stale` / `timeout` / `comm_error` / `address_error` / `not_connected` | `valueKnown=false`，状态机不动 | 同左；短路未求值的引用不参与 |
| 点位不存在 / 非数值 | `valueKnown=false` + `reason` | 同左 |

「数据断了」与「数据说正常」是两件事。把前者当后者会掩盖真实故障，把前者
当「报警」会让一次断线变成满屏假报警——两种都是运维最讨厌的噪声。

### 10.3 排序与可见性

- 列表按「严重度降序 → 激活时间升序 → ID」排序，这就是处置顺序。
- 无条件查询返回**全部规则**（含 `normal`）：否则「没有报警」与「规则没加载」
  在响应里无法区分。
- `health.alarms.unknown` 是链路健康的一等公民；它不进 `status`，但必须可见。

### 10.4 与设备状态机的区别

| 维度 | 设备状态机 | 报警状态机 |
|---|---|---|
| 输入 | 采集成功/失败 | 点位值 + 质量 + 延时 |
| 状态 | configured/connecting/collecting/degraded/faulted/disabled | normal/pending/active/acked/cleared |
| 需要人处理 | 故障需要，但无「确认」语义 | 显式 ack，记录 `ackBy`/`ackAt` |
| 持久化 | 运行态，重启重建 | 状态只存内存，计数与确认也不落盘 |
| 目的 | 「数据在不在流」 | 「这件事有没有人管」 |

---

## 11. 历史（History）

历史是**缓存事件的有界投影**，不是第二个事实来源。当前值永远以
`cache` 为准；历史只是「把它按时间记下来」的一条旁路。因此历史写失败、
磁盘满、目录被删，都不允许影响采集——这是与 Outbox 最本质的区别。

```go
type Record struct {
    Time    time.Time    // 采集时刻（UTC），不是落盘时刻
    Device  string
    Point   string
    Value   Value
    Quality Quality
}
```

### 11.1 采样策略

采样是「事件驱动 + 心跳补点」。只靠事件会让一条稳定运行的产线在图上变成一段
空白；只靠定时轮询又会把「值没变」写成海量重复点。

| 触发 | 条件 | 理由 |
|---|---|---|
| 首次观测 | 该点位还没有任何记录 | 图表的起点 |
| 数值变化 | `|Δ| > 0 且 |Δ| ≥ deadband` | 死区过滤测量噪声 |
| 非数值变化 | `bool` / `string` / `bytes` 不相等 | 这些类型没有「死区」概念 |
| 类型变化 | `Value.Type` 改变 | 点位被重新配置过，旧类型的值不可比 |
| 质量变化 | `Quality` 改变 | 坏数据的**起点**是最需要查的信息 |
| 心跳 | 距上次落盘 ≥ `heartbeat`，且样本本身仍新鲜 | 值没变也要有基线 |
| 不记录 | `Quality == disabled` | 点位被显式关停，不是数据 |

两条容易写错的细节：

1. **死区比较的参照物是「上次落盘的值」，不是「上次采到的值」。** 否则一条缓慢
   爬升的曲线（每步都小于死区）会永远不落盘。
2. **被 `minInterval` 挡下的变化进入 pending，而不是被丢弃。** 否则「快速变化
   之后立刻稳定」的最终值永远不会落盘，曲线会停在变化前的那一刻。

心跳还有一条硬约束：**只有样本本身仍新鲜（`CollectedAt` 在 `heartbeat` 以内）
才补点。** 设备已经停了、缓存里的值停在十分钟前，这时按心跳写一条「此刻仍然
是 42」就是在编造数据——历史可以稀疏，但不能撒谎。

### 11.2 存储与保留不变量

| 不变量 | 内容 |
|---|---|
| I-HS-1 | 段文件内记录按追加顺序排列；段按「首条记录时间」升序索引 |
| I-HS-2 | 一条记录不跨段：放不下就换新段，绝不把半行写进两个文件 |
| I-HS-3 | 淘汰以**整段**为单位，按字节上限与保留期判定，永远保留至少一个段 |
| I-HS-4 | 重启后继续追加；末尾半行（掉电残骸）触发新段，不拼接 |
| I-HS-5 | 写入是「用户态缓冲 + 定期 flush」，不逐条 fsync；崩溃最多丢一个 flush 周期 |
| I-HS-6 | 历史写失败只计数并记日志，绝不反压采集 |
| I-HS-7 | 所有对外时间戳为 UTC；写入端保证同一测点的时间单调不减 |

`I-HS-7` 的单调性由写入端兜底：采集时刻缺失、或早于上一条时，退化为落盘时刻。
查询端仍会排序一次（容忍外部导入与时钟回拨），因为「按时间升序返回」是北向
契约，不能依赖磁盘顺序。

### 11.3 查询不变量

| 不变量 | 内容 |
|---|---|
| I-HQ-1 | 区间左闭右闭；`from` 必须早于 `to` |
| I-HQ-2 | 原始点按时间升序，`limit` 截断并置 `truncated` |
| I-HQ-3 | 桶起点对齐到 `from + k*step`，不随查询时刻漂移 |
| I-HQ-4 | 桶质量取桶内**最差**质量码 |
| I-HQ-5 | `min` / `max` / `avg` 只对数值类型计算，其余为 `null` |
| I-HQ-6 | 桶数上限 5000；超限在参数层拒绝，不先扫后判 |
| I-HQ-7 | 没有数据返回 `200 + count:0`，不是 404 |

### 11.4 三个「写磁盘」的东西，职责不同

| | Outbox | journal | history |
|---|---|---|---|
| 定位 | 还没送出去的队列 | 已经发生的事实留痕 | 有界的时间序列缓存 |
| 写入 | 每条变化事件 | 每条通过死区的事件 | 按采样策略抽取 |
| 生命周期 | 确认后删除 | 只追加，外部轮转 | 超限/过期整段淘汰 |
| 失败影响 | 积压、告警 | 丢证据 | 丢曲线 |
| 是否反压采集 | 否（有上限与降采样） | 否（缓冲写） | 否（失败只计数） |
| 默认 | 有 sink 时开启 | 关闭 | 关闭 |

---

## 12. 配置文件形态

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

### 12.0.1 `storage` 字段

| 字段 | 默认 | 含义 |
|---|---|---|
| `dir` | `./data` | 数据根目录：快照、Outbox、状态文件、日志都落在这里 |
| `outboxMaxBytes` | 64 MiB | 队列字节上限，超限按「先丢最旧、保留变化点」降采样 |
| `outboxMaxRecords` | 200000 | 队列条数上限 |
| `outboxTtl` | `24h` | 单条记录最长存活时间，过期丢弃并计数 |
| `journal` | `false` | 开启本地 NDJSON 归档 sink，见下 |
| `history` | 关闭 | 有界采样日志，见 §11 |

**`journal` 的定位**：它是一个**兜底 sink**，不是北向协议。开启后每条通过死区过滤的变化事件都会追加到 `<dir>/journal.ndjson`（一行一个 JSON，含 `schemaVersion`），并走与 MQTT/Webhook 完全相同的 Outbox 路径（同样的分区、确认位点、上限与重试语义）。

它解决三个具体问题：

1. **断网留痕**：现场没有 Broker 时，至少能在本地留下「什么时候、哪个点、变成了什么」的证据链。
2. **可观测性**：开发与联调阶段不必先架一个 Broker 就能验证「采集 → 死区 → Outbox → 投递」整条链路是通的。
3. **可测试性**：CI 里可以直接断言落盘内容，而不需要引入外部依赖。

代价是磁盘写入。因此它默认关闭，且写入走批量 flush（≤64 条一次 `write`，`Stop` 时 `Sync`），不逐条 fsync。开启前应确认 `<dir>` 所在分区有足够空间，并配合 `outboxMaxBytes` 使用。

> 注意：`journal` 与 MQTT/Webhook 是**并列**关系，不是替代关系。同时开启时，同一条事件会分别进入各自的 sink 分区，各自独立确认，互不影响。

### 11.1 命名规则

| 对象 | 规则 | 示例 |
|---|---|---|
| Site | `^[a-z][a-z0-9-]{1,31}$` | `plant-a` |
| Channel | `^[a-z0-9][a-z0-9-]{0,31}$` | `ch-plc` |
| Device | `^[a-z0-9][a-z0-9._-]{0,63}$` | `plc-01`、`line1.plc2` |
| Point | 同 Device | `temp`、`motor.speed` |

**保留字**：`_system`、`_internal`、`uic` 不允许作为 ID 前缀外的完整 ID。

### 11.2 迁移

配置带 `schemaVersion`。加载时：

1. 版本等于当前 → 直接使用；
2. 版本小于当前 → 执行迁移链（`v1→v2→v3`），迁移结果写回并备份原文件；
3. 版本大于当前 → 拒绝启动，提示升级程序。

---

## 13. 模板（Template）

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

## 14. 不变量汇总（实现检查单）

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
| 13 | 报警条件二选一（阈值 XOR 表达式） | 配置加载 | `config.invalid_alarm` |
| 14 | 报警引用的点位存在且类型可比较 | 配置加载 | `config.invalid_alarm` |
| 15 | 报警 ID 唯一 | 配置加载 | `config.invalid_alarm` |
| 16 | `high - low > 2 × hysteresis` | 配置加载 | `config.invalid_alarm` |
| 17 | 输入不可信时不更改报警状态机 | 运行期 | 仅更新 `reason`/`quality` |
| 18 | `pending` 不可确认 | API 层 | `alarm.not_active` |
| 19 | 历史段内记录有序、一条记录不跨段 | 运行期 | 由轮转保证 |
| 20 | 每段不超过 `segmentBytes`（单条超长除外） | 运行期 | 写前判定后换段 |
| 21 | 历史查询区间左闭右闭、返回升序 | API 层 | `history.invalid_range` |
| 22 | 聚合桶数 ≤ 5000 | API 层 | `history.invalid_query` |

---

## 15. 与参考方案的模型差异

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
