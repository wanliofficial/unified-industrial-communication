# 驱动 SDK 规范

> **版本**：1.0（契约版本 `sdkVersion = "1.0"`）
> **上游**：`02-architecture.md` §8、`03-domain-model.md`
> **代码对应**：`internal/driver`（契约）、`internal/driver/modbus`（参考实现）、`internal/driver/virtual`（测试驱动）
> **目标**：任何人（含第三方）在**没有真实设备**的情况下，能在 2 周内交付一个通过一致性测试的驱动。

---

## 1. 设计原则

| # | 原则 | 具体含义 |
|---|---|---|
| D1 | **三层分离** | `Codec`（纯函数）/ `Transport`（连接）/ `Driver`（语义）三层，依赖单向 |
| D2 | **纯函数可复用** | 同一份 `Codec` 同时被主站、从站、仿真器、测试用例使用 |
| D3 | **能力诚实** | 能力四态（支持/不支持/视情况/实验性），不允许布尔值撒谎 |
| D4 | **批量是默认** | `Read` 接收 `[]ReadSpan`，驱动可进一步合并但不能要求单点调用 |
| D5 | **超时是默认** | 所有 I/O 带 `context.Context`，无「无超时」路径 |
| D6 | **错误可分类** | 错误必须归入 7 类，供上层决定「重试/熔断/告警」 |
| D7 | **无全局状态** | 驱动实例不持有跨设备状态；连接状态在 `Conn` 中 |
| D8 | **可测试性优先** | 不允许在驱动内直接 `time.Sleep`、`net.Dial`、`os.Open`（必须经注入） |

---

## 2. 三层结构

```
┌─────────────────────────────────────────────────────────┐
│ Driver（语义层）                                          │
│   Meta() / Open(cfg) → Conn                              │
│   决定「用什么地址、怎么批量、怎么判定质量」                 │
├─────────────────────────────────────────────────────────┤
│ Conn（连接层）                                            │
│   Probe / Read / Write / Close                           │
│   持有 Transport，负责事务号、重试、错误分类                │
├─────────────────────────────────────────────────────────┤
│ Codec（编解码层，纯函数）                                  │
│   EncodeRead / DecodeRead / EncodeWrite / DecodeWrite    │
│   不持有状态、不做 I/O、不读时钟                            │
├─────────────────────────────────────────────────────────┤
│ Transport（传输层）                                        │
│   RoundTrip(ctx, req) → resp                             │
│   TCP / UDP / 串口 / TLS / 内存回环                        │
└─────────────────────────────────────────────────────────┘
```

**依赖方向**：`Driver → Conn → Codec + Transport`；`Codec` 与 `Transport` 互不依赖。

---

## 3. 契约定义（Go）

### 3.1 元数据

```go
package driver

type Role string

const (
    RoleMaster Role = "master"
    RoleSlave  Role = "slave"
    RoleBoth   Role = "both"
)

type CapState string

const (
    CapSupported        CapState = "supported"
    CapUnsupported      CapState = "unsupported"
    CapRuntimeDependent CapState = "runtime_dependent" // 视设备型号/固件而定
    CapExperimental     CapState = "experimental"
)

type Capabilities struct {
    Read        CapState `json:"read"`
    Write       CapState `json:"write"`
    Subscribe   CapState `json:"subscribe"`
    Browse      CapState `json:"browse"`      // 能否枚举设备内可用点位
    Discovery   CapState `json:"discovery"`   // 能否发现网段内设备
    MultiRead   CapState `json:"multiRead"`   // 是否支持一次请求多地址
    BitAccess   CapState `json:"bitAccess"`
    StructTypes CapState `json:"structTypes"` // 结构体/自定义类型
}

type Meta struct {
    ID           string              `json:"id"`           // 协议唯一标识，如 modbus-tcp
    Name         string              `json:"name"`
    Version      string              `json:"version"`      // 驱动版本，语义化
    SDKVersion   string              `json:"sdkVersion"`   // 契约版本，当前 "1.0"
    Roles        []Role              `json:"roles"`
    Transports   []string            `json:"transports"`   // tcp / serial / udp / memory
    AddressKinds []model.AddressKind `json:"addressKinds"`
    Capabilities Capabilities        `json:"capabilities"`
    ConfigFields []ConfigField       `json:"configFields"` // 用于生成配置表单
    PointFields  []ConfigField       `json:"pointFields"`
    Description  string              `json:"description"`
}

type ConfigField struct {
    Name        string   `json:"name"`
    Label       string   `json:"label"`
    Type        string   `json:"type"`        // string | int | float | bool | enum | duration
    Required    bool     `json:"required"`
    Default     any      `json:"default,omitempty"`
    Enum        []string `json:"enum,omitempty"`
    Placeholder string   `json:"placeholder,omitempty"`
    Help        string   `json:"help,omitempty"`
    Scope       string   `json:"scope"`       // channel | device | point
}
```

### 3.2 驱动与连接

```go
type Driver interface {
    Meta() Meta
    Open(ctx context.Context, cfg OpenConfig) (Conn, error)
}

type OpenConfig struct {
    Channel model.Channel
    Device  model.Device
    Log     *slog.Logger
    Metrics Metrics // 可为 nil
}

type Conn interface {
    Probe(ctx context.Context) error
    Read(ctx context.Context, spans []ReadSpan) ([]RawSample, error)
    Write(ctx context.Context, reqs []WriteRequest) ([]WriteResult, error)
    Close() error
}
```

**约定**：

| 方法 | 语义 | 允许的副作用 |
|---|---|---|
| `Probe` | 验证连接与基本权限（读 1 个已知地址或握手） | 可建立连接 |
| `Read` | 执行一批读；返回与输入等长或按 `PointID` 对齐的结果 | 不修改点位配置 |
| `Write` | 执行一批写；每项独立成败 | 可回读校验 |
| `Close` | 幂等关闭 | 多次调用不报错 |

### 3.3 读请求与结果

```go
type ReadSpan struct {
    ID     string     // 规划器生成的 span id（日志用）
    Points []PointRef // 同一次请求覆盖的点位
}

type PointRef struct {
    ID        string
    Address   model.Address
    DataType  model.DataType
    ByteOrder model.ByteOrder
    Length    int // 推导后的寄存器/字节数
}

type RawSample struct {
    PointID string
    Value   model.Value
    Quality model.Quality
    Err     error
}
```

**规则**：

1. 返回的 `RawSample` 数量与 `Points` 总数一致（缺失视为 `bad`）；
2. `Quality` 为空时视为 `good`；
3. 驱动可以自行决定「一个 span 发几个协议请求」，但**必须**在一次 `Read` 内完成；
4. 驱动不得在 `Read` 内 sleep 超过 `cfg.Device.Timeout`。

### 3.4 写请求与结果

```go
type WriteRequest struct {
    PointID   string
    Address   model.Address
    DataType  model.DataType
    ByteOrder model.ByteOrder
    Value     model.Value
}

type WriteResult struct {
    PointID string
    OK      bool
    Quality model.Quality
    Err     error
}
```

**规则**：

1. 写入必须**逐项**返回结果，不允许「整批成功/失败」；
2. `OK=true` 但 `Quality=uncertain` 表示「已下发未确认」；
3. 写入失败不得 panic，不得静默。

### 3.5 编解码

```go
type Codec interface {
    EncodeRead(span ReadSpan) ([]byte, error)
    DecodeRead(span ReadSpan, resp []byte) ([]RawSample, error)
    EncodeWrite(req WriteRequest) ([]byte, error)
    DecodeWrite(req WriteRequest, resp []byte) (WriteResult, error)
}
```

**纯函数约束**（CI 会检查）：

- 不调用 `time.Now`、`net.*`、`os.*`、`math/rand`；
- 不读写包级变量；
- 相同输入必须得到相同输出（可并发调用）。

### 3.6 传输

```go
type Transport interface {
    RoundTrip(ctx context.Context, req []byte) ([]byte, error)
    Close() error
}
```

**实现要求**：

| 要求 | 说明 |
|---|---|
| 读满或超时 | 按协议长度字段读满；剩余字节保留到下次（粘包） |
| 超时分层 | 连接超时 / 读超时 / 单次往返总超时 |
| 并发安全 | 同一 `Transport` 可被串行调用；并发调用由上层闸门保证 |
| 关闭幂等 | 多次 `Close` 不 panic |
| 错误分类 | 网络错误包装为 `*OpError`（见 §4） |

### 3.6.1 串口传输（共享链路）

串口与 TCP 是两种语义完全不同的链路，把二者当同一种「字节流」处理是本项目
踩过的最贵的坑。串口传输层（`internal/transport` 的 `SerialStream`）在契约
上与 TCP 一致，但额外承担下列**六条硬约束**，违反其中任何一条都会产生现场
无法复现的故障：

| 编号 | 约束 | 违反后的现象 | 实现手段 |
|---|---|---|---|
| S1 | 以 `O_RDWR\|O_NOCTTY\|O_NONBLOCK` 打开，使 `os.File` 进入 Go 运行时的 poller | 无 deadline 能力，一次掉线把采集 goroutine 永久钉在 `read` | `os.OpenFile` 带 `syscall.O_NONBLOCK` |
| S2 | 取 fd 一律走 `SyscallConn().Control()`，**绝不调用 `File.Fd()`** | `Fd()` 会把 fd 切回阻塞模式并脱离 poller，此后所有 deadline 静默失效 | 全部 ioctl 包在 `control()` 内 |
| S3 | termios 置 raw（`Iflag=IGNPAR`、`Oflag=0`、`Lflag=0`），无流控 | 内核改写 `0x0D`/`0x11`/`0x13`，二进制帧被破坏，CRC 必错 | `apply()` 显式写 Termios 全字段 |
| S4 | 写完成后 `tcdrain`（`TCSBRK` 参数 1） | 半双工 RS-485 方向切换过早，主站读回自己刚发的内容 | `Write` 末尾 ioctl |
| S5 | 每次事务发送前 `tcflush(TCIFLUSH)` | 上一轮迟到响应被当作本轮帧头，出现「CRC 正确但内容错位」 | `Flusher.Drain()` |
| S6 | 同一物理端口按路径做引用计数复用 + 总线级互斥 | 同一 tty 被多个 fd 打开，两个读者互相偷字节；单台能通、多台随机超时 | 端口注册表 + `sharedSerial.mu` |

**端口注册表的语义**（对应 S6）：

- 键是串口设备路径；同一路径第二次 `Open` 只增加引用计数，不重复打开 fd。
- 同一路径以**不同参数**再次打开时**直接报错**，不静默复用、也不另开一个 fd：
  现场把两个通道的波特率配错是常见事故，静默复用只会把故障挪到更难查的位置。
- 每个设备持有独立的引用对象，`Close` 只减一次计数；最后一个使用者负责真正
  关闭 fd。引用计数归零后从注册表移除，下次 `Open` 重新协商参数。
- 事务串行化粒度是**整条总线**，不是单设备：从「清空接收缓冲」到「读完整帧」
  全程持锁，否则收到的字节可能来自另一台设备的响应。

**帧间静默**：RTU 规范要求帧间至少 3.5 个字符时间。本实现按实际字符位宽计算
（起始位 + 数据位 + 校验位 + 停止位），波特率高于 19200 时按规范固定取
1750 μs；并额外加 200 μs 余量吸收调度抖动。少等这几毫秒的典型现象是
「偶发首字节丢失」，现场往往被误判为从站固件问题。

**响应长度的推导方式**：RTU 没有长度字段，本实现**由请求 PDU 推导正常响应
长度**（读类按数量算字节计数，写类恒为 8 字节回显），只有异常响应（`FC|0x80`，
恒 5 字节）走独立分支。相比「逐字节读到长度可判定」，这条路径把读系统调用
从 O(帧长) 降到 2~3 次；更重要的是，从站写坏字节计数时会立刻被 CRC 拒绝，
而不是让主站挂到设备超时才失败——后者在现场表现为「偶发 3 秒延迟」，极难定位。

### 3.6.2 Modbus ASCII 的额外约束

ASCII 与 RTU 共用 connect / codec，只在分帧层不同。但有四条硬约束不能照搬：

| # | 约束 | 不做会怎样 | 代码落点 |
|---|---|---|---|
| A1 | 帧以 `:` 开头、CRLF 结尾；LRC 为「地址+PDU」按字节求和取补码 | 用 CRC 当 LRC 校验，永远不通过；现场表现为「从站明明有回包但主站报协议错」 | `codec/modbus/ascii.go` `LRC` |
| A2 | 逐字符读到 CRLF（没有长度字段），读前跳过 `:` 之前的杂散字节 | 上一次事务残留的字符被当成帧头，重启后第一帧必失败 | `asciiFraming.readFrame` |
| A3 | 端口注册表把**分帧类型**纳入冲突判定：同一路径不能既跑 RTU 又跑 ASCII | 两种帧交错，症状是随机 LRC/CRC 错，日志里看不出根因 | `acquirePort(proto, …)` |
| A4 | 十六进制解析严格：必须偶数长度、必须是十六进制字符、LRC 必须吻合 | 「尽力解析」会把半条帧解析成合法值，比读失败危险得多 | `ParseASCII` |

**默认串口参数**：ASCII 规范推荐 7 位数据位 + 偶校验，但现代设备普遍是 8N1。
`Meta().ConfigFields` 里把 7E1 作为默认值填入**字段说明**，而不是替用户改写配置：
驱动不替用户猜硬件参数，只把「手册里的推荐值」告诉他。

**与 RTU 的能力差异**：没有。ASCII 同样支持 FC1/2/3/4/5/6/15/16，同样支持
位访问与批量读。字符膨胀约 2 倍，因此在 9600 波特下吞吐约为 RTU 的一半——
这是帧格式的固有代价，不是实现取舍。

**平台边界**：串口实现只提供 Linux 版本（`serial_linux.go`）。非 Linux 平台
返回 `ErrSerialUnsupported` 的**显式失败**，不做静默降级：Windows 串口需要
DCB 结构走另一套 API，在没有真实硬件可验证之前，写一个测不了的实现比不写更
危险。需要 Windows 串口的部署走 C# 辅助进程桥接（ADR 0001）。

### 3.7 S7（S7comm over ISO-on-TCP）

S7 与 Modbus 的差别不在「寄存器怎么摆」，而在**一次事务可以带多项**。这直接
改写了驱动的切块逻辑：Modbus 的规划层负责「把连续地址合并成一次请求」，S7 的
规划层只负责分组，真正的合并单位是「一个 PDU 里能塞下几个变量说明」。

#### 3.7.1 三层封装

```
┌───────────── TPKT（RFC1006） ─────────────┐  4 字节头：version=3, length(大端)
│ ┌────────── COTP（ISO 8073） ───────────┐ │  1 字节长度 + 1 字节类型 + 参数
│ │ ┌──────── S7 PDU ───────────────────┐ │ │  10 字节头 + 参数区 + 数据区
│ │ └───────────────────────────────────┘ │ │
│ └───────────────────────────────────────┘ │
└───────────────────────────────────────────┘
```

| 层 | 本实现的范围 | 不做的事 |
|---|---|---|
| TPKT | 4 字节头、长度上下界校验（4..8192） | 分片重组；S7 不会发超过协商值的帧 |
| COTP | CR（连接请求）/ CC（连接确认）/ DT（数据传输） | 断开请求、错误 TPDU、TSAP 路由 |
| S7 | Setup Communication、Read Var、Write Var | 块上传/下载、报警订阅、符号表浏览、时间同步 |

**TPKT 长度必须校验**：长度字段由对端给出，坏帧可能声称 65535 字节。不做上界
检查，一次坏帧就会吃掉 64KB 缓冲并一直等到设备超时——而现场只会看到「设备卡了
3 秒」，不会看到「对端发了坏帧」。

#### 3.7.2 连接序列不能省

```
主站 → COTP CR（dst TSAP = 0x0100 + rack×32 + slot）
从站 → COTP CC
主站 → S7 Setup Communication（请求 PDU 长度 480）
从站 → Setup Ack（返回协商值，如 240 / 480 / 960）
主站 → Read Var / Write Var
```

| 约束 | 原因 |
|---|---|
| 必须先 COTP 再 S7 | 跳过 COTP 的连接在部分 CPU 上会被直接 RST，且没有任何错误码 |
| 目的 TSAP 按 rack/slot 计算 | 这是 S7 把「CPU 物理位置」编码进 TSAP 的方式；S7-1200/1500 常在槽 1，S7-300/400 常在槽 2，写死会让一半设备连不上 |
| 必须完成 Setup 协商 | 未协商时按 480 发送超过 240 的请求，部分 CPU 静默丢弃；现场表现为「读几个点正常，读多了就没反应」 |
| 切块必须使用协商值 | 用请求值而不是返回值，在只支持 240 的 CPU 上等于每块都超限 |

源 TSAP 固定 `0x0100`（PG/编程器），TPDU 长度固定 256。这两个值不暴露给用户：
它们不是「现场可调参数」，而是协议惯例；开放出来只会增加配错的可能。

#### 3.7.3 地址与长度单位

点位地址映射到 S7 变量说明（S7ANY，`0x12 0x0A 0x10`）：

| 模型字段 | S7 字段 | 说明 |
|---|---|---|
| `address.area` | 区域码 | `input`→0x81、`output`→0x82、`merker`→0x83、`db`→0x84、`counter`→0x1C、`timer`→0x1D |
| `address.db` | DB 号 | 仅 `area=db` 时有效且必填（≥1）；其他区带 DB 号在配置校验期报错 |
| `address.offset` | 字节地址 | **不是位地址**；位访问时协议里的地址是 `offset×8 + bit` |
| `address.bit` | 位号 | 仅 `bool` 点位使用（0..7）；非 bool 点位带 `bit>0` 报错 |
| `dataType` + `address.length` | 传输尺寸 + 长度 | 见下表 |

**长度单位不统一是 S7 最容易写错的地方**（请求项与响应项还不一样）：

| 场景 | transport size | 长度语义 |
|---|---|---|
| 请求（按字节读） | 0x02 BYTE | 字节数 |
| 请求（按位读） | 0x01 BIT | 恒为 1 |
| 响应（BIT / BYTE / WORD / DWORD） | 0x03 / 0x04 / 0x05 / 0x06 | **位数** |
| 响应（INT / DINT / REAL 等） | 0x07 / 0x08 / 0x09 | **字节数** |
| 写请求数据区（字节） | 0x04 BYTE/WORD | **位数**，且数据偶数对齐 |
| 写请求数据区（位） | 0x03 BIT | 位数 |

把响应的「位数」当字节数，就会把 4 字节 REAL 截成 1 字节；把写数据的「位数」
当字节数，就会写出 8 倍长度的脏数据。本实现把换算收敛到
`codec/s7.payloadBytes()` 一处，主站与从站共用，避免两边各写一套。

#### 3.7.4 每项返回码与错误分类

Read Var / Write Var 的响应是**逐项**的：一个 PDU 里 20 项，可能 18 项成功、
2 项地址不存在。因此错误必须挂在样本上，而不是让整个事务失败。

| 返回码 | 含义 | 映射 | 质量码 |
|---|---|---|---|
| 0x00 | 成功 | — | good |
| 0x03 | 访问被拒（如写输入区） | `ErrPermission` | bad |
| 0x05 | 地址越界 | `ErrAddress` | address_error |
| 0x0A | 对象不存在（DB 未定义） | `ErrAddress` | address_error |
| 0x06 / 0x07 | 类型/长度不匹配 | `ErrUnsupported` | bad |
| 其他 | 硬件/协议错误 | `ErrProtocol` | bad |

为了让「逐项错误」自带分类，`driver` 包引入 `Kinded` 接口：

```go
type Kinded interface{ Kind() ErrorKind }
```

`codec/s7.ItemError` 实现它，于是 `driver.KindOf` 能直接取到分类，不需要在
驱动里逐处包 `*OpError`。少包一层就少一处漏包，错误分类是不会在正常路径上
被发现的缺陷，只能靠「只写一次」来保证。

**部分失败的策略与 Modbus 一致**：只要有任意一项成功，`Read` 就不向上返回错误
（好样本必须保留）；**全部点位失败**才返回第一个错误，让设备进入故障/退避。
链路级错误（超时、断连）例外：立即返回，并把剩余未发送的点位显式标坏——
让它们「消失」会诱导上层报「point missing」，把真正的断链原因盖掉。

#### 3.7.5 切块策略

两个约束同时生效，取更严的一个：

1. **项数 ≤ 20**：S7 协议参数区的项数上限。
2. **请求与响应都不超过协商 PDU**：按最坏情况估算（每项请求 12 字节规格、
   响应 4 字节头 + 数据），写入时还要加上数据区的「4 字节头 + 偶数对齐负载」。

预算按最坏情况算而不是「发出去看错误」：S7 超限通常表现为对端不响应，而不是
返回一个明确的「太长」错误码；从超时反推原因的成本远高于多发一帧。

#### 3.7.6 探活

`Probe` 读 `M0.0` 一个字节。**单项返回「地址不存在」也算链路可达**：探活要回答的
是「CPU 还应答吗」，不是「探活点配得对吗」。把地址错误当断链，会让一个配错的
探活地址把整台设备永久判死——而现场往往先怀疑网络，最后才发现是自己写错了地址。

#### 3.7.7 边界

| 项 | 状态 | 说明 |
|---|---|---|
| 订阅 / 报警 | ❌ | S7 的报警需要建立独立的 PDU 通道；不实现 |
| 符号表 / 块上传 | ❌ | 需要 S7 的浏览服务；点位必须手写地址 |
| 设备发现 | ❌ | S7 无标准发现协议 |
| 时间同步 | ❌ | 不在采集链路的关键路径上 |
| TLS | 不适用 | S7 本身无 TLS；需要加密走 VPN/隧道 |
| 多连接 | ⚠️ | 多数 CPU 允许的连接数很少；`concurrency` 默认 1 |

内置从站（`internal/sim/s7`）只实现连接、Setup、Read Var、Write Var 四种 PDU，
用于自测与演示。它与主站共用 `codec/s7`，所以「长度单位」这类语义不会在主从
之间分叉——这是本项目对「自测有效性」的最低要求：测试夹具不能是另一套实现。

### 3.8 Modbus TCP 从站

从站不是「把主站反过来写」。它与主站面对的问题域有三处根本不同：

| 维度 | 主站（客户端） | 从站（服务端） |
|---|---|---|
| 数据来源 | 链路 | cache（本进程采集结果） |
| 报文边界 | 自己决定 | 对端决定，必须处理任意区间 |
| 写路径 | 发起写入 | 接收写入并**再转发**到南向设备 |
| 失败语义 | 本设备采集失败 | 对端的读请求可能跨越多个映射、多个设备 |

因此从站是 `internal/slave/modbus` 而不是 `driver/modbus` 的一个模式：两者共享
`codec/modbus` 的纯函数，但生命周期、并发模型与错误语义完全不同。

#### 3.8.1 配置即启用

```json
{
  "slaves": [{
    "id": "scada",
    "listen": "127.0.0.1:1502",
    "maxConnections": 8,
    "units": [{
      "unitId": 1,
      "byteOrder": "ABCD",
      "mappings": [
        { "point": "sim01/reg0", "area": "holding", "offset": 0 },
        { "point": "sim01/write_me", "area": "holding", "offset": 3, "writable": true },
        { "point": "sim01/coil0", "area": "coil", "offset": 0 }
      ]
    }]
  }]
}
```

| 设计选择 | 理由 |
|---|---|
| 没有 `enabled` 字段 | 存在即启用；停用就是从配置里删掉。多一个开关就多一种「配置了但没生效」 |
| 没有逐点字节序/宽度 | 字节序属于单元，宽度由点位类型推导。逐点覆盖只会制造「同一设备上面读出 12.5、下面读出 0.0」 |
| `slug` 式的 `point` 引用 | 映射必须指向已存在的 `device/point`；指向不存在的点位在配置期报错 |
| 绑定在构造期完成 | 端口被占用是启动期错误，应带着明确日志退出，而不是后台反复重试让运维误以为可用 |

**校验规则（配置期，全部拒绝启动）**：

1. `unitId` 在 0..247；0 是广播地址，不能作为单播目标。
2. 同一从站内 `unitId` 不重复；同一文件内 `slave.id` 不重复。
3. 同一 `area` 内地址区间不允许重叠（不同区可以同偏移）。
4. `offset + RegisterWidth() <= 65536`。
5. `writable` 必须同时满足：区可写（holding/coil）**且**点位 `writable: true`。
6. 从站 `listen` 不得与 `server.listen` 冲突。端口 0 永远不算冲突（测试与容器部署依赖它）。

宽度来自 `Point.RegisterWidth()`：`bool`/`uint16` 为 1，`float32` 为 2，`float64` 为 4，
`string`/`bytes` 的 `address.length` 是**寄存器个数**而不是字符数。这条规则单独抽成
方法，就是为了避免每个调用点各自解释一遍 `length`。

#### 3.8.2 读路径：对端要什么就给什么

主站可以读任意 `[offset, offset+qty)`，这段区间可能：

- 完整覆盖一个映射；
- 只覆盖 32 位值的**一半**；
- 跨越多个相邻映射；
- 中间夹着没有映射的**空洞**。

处理策略：

| 情况 | 行为 | 理由 |
|---|---|---|
| 完整覆盖 | 正常编码 | — |
| 半个 32 位值 | 允许，返回对应字节 | 一些主站会先读 1 个寄存器试探；拒绝会让它直接放弃 |
| 跨多个映射 | 逐段拼接 | 主站不应被迫知道网关内部分段 |
| 空洞 | 异常码 `0x02` | 补零会让主站把「没配映射」记录成真实值 0，这类错误在现场只会被当成工艺数据 |
| 还没有样本 | 异常码 `0x04` | 用 0 顶替等于把「没有数据」伪装成「值为 0」 |
| 编码失败 | 异常码 `0x04` + 日志 | 宽度不一致属于配置/类型错误，不能悄悄截断 |
| 坏质量 | **返回最后一次值** + 计数 | Modbus 没有质量位；把坏值变成 0 比读到旧值危险得多。质量只能通过指标与 REST 接口暴露 |

位区的读法按位打包（每 8 位一字节，低位在前），与寄存器区共享同一套映射查找。

#### 3.8.3 写路径：必须穿透到南向

外部主站写从站，不是写本地缓存，而是要真的下发到 PLC。代码上只有一条路径：

```
Modbus 写请求 → decodeCellValue（按单元字节序还原） → Writer.Write
             → Runtime.Write（设备 actor + 可写校验 + 可选确认）
             → 驱动 → 现场设备
```

**为什么不能直接操作驱动连接**：绕过 `Runtime.Write` 就等于开了一个不受命令白名单、
可写校验与幂等键约束的第二写入口。这类漏洞在审计时几乎无法解释，也无法在
日志里与北向写入对齐。

写请求的处理细节：

| 场景 | 行为 | 理由 |
|---|---|---|
| 单寄存器写 32 位点位 | 拒绝 `0x02` | 只写一半会让设备收到撕裂值；这多半是主站配置错误 |
| 多寄存器写只覆盖部分点位 | 拒绝 `0x02` | 半个 float 没有意义 |
| 多寄存器写跨多个点位 | 拆成每个点位一次写入，全部成功才回 OK | Modbus 写响应没有「部分成功」的表达方式 |
| 写只读区 / 只读点位 | 拒绝 `0x02` | 双重校验，两端都必须允许 |
| 写入转发失败 | 返回 `0x04` | 让主站明确知道没写进去，而不是收到回显后以为成功 |
| 写线圈的值不是 `0xFF00`/`0x0000` | 拒绝 `0x03` | 协议只允许这两个值 |
| 未知 unit | 返回 `0x0B` | 网关目标不存在；比不响应好——主站能立刻看到确定错误码，而不是 3 秒超时后去查网络 |
| 未知功能码 | 返回 `0x01` | — |
| 报文长度/字节数与功能码不匹配 | 返回 `0x03` | 认出功能码但数据非法 |

#### 3.8.4 连接与生命周期

- accept 后按 `maxConnections`（默认 8，上限 64）占用槽位；超限**立刻关闭**而不是排队。
  排队会把「过载」伪装成「网络慢」。
- 空闲连接按 `idleTimeout`（默认 5 分钟）回收。
- `Close` 幂等：关闭监听、关闭全部连接、等待 goroutine 退出。
- 单个连接的事务是串行的：Modbus TCP 允许流水线，但多数主站不发；按序处理可以
  避免响应乱序带来的额外状态机。

**热更新**：`syncSlaves` 比较指纹决定是否重建某个从站。指纹覆盖：

1. 从站配置本身（监听地址、单元、映射）；
2. **被映射点位的类型、宽度、地址长度、可写性**。

只比较映射会漏掉最危险的一种变化：把温度从 `uint16` 改成 `float32` 时映射表
一个字都没改，但线上字节完全不同。重建时先关旧监听再建新的；重建失败会记录
`slave_rebuild_failed` 并保留其余从站，不让一个坏配置拖垮全部服务。

#### 3.8.5 指标

| 指标 | 标签 | 含义 |
|---|---|---|
| `uic_slave_requests_total` | `slave` / `unit` / `fc` / `result` | 请求总数；`result=exception` 表示返回了异常码 |
| `uic_slave_degraded_reads_total` | `slave` | 用非 good 质量数据应答的次数（值保留，质量靠指标暴露） |
| `uic_slave_write_total` | `slave` / `result` | 写转发次数与结果 |

#### 3.8.6 边界

| 项 | 状态 | 说明 |
|---|---|---|
| Modbus TCP 从站 | ✅ | MBAP、四区映射、读写、连接上限、热更新 |
| Modbus RTU / ASCII 从站 | ❌ | 串口从站需要与主站角色互斥；本版本只做 TCP |
| 诊断功能码（FC8/FC11/FC17/FC43） | ❌ | 只实现读写类功能码 |
| 多寄存器位写（掩码写） | ❌ | 需要读-改-写的原子性保证，暂不支持 |
| 主动上报（unsolicited） | 不适用 | Modbus 本身是请求-响应模型 |

内置从站（`internal/sim/modbus`）只用于自测上游设备，与本从站是**两个不同的角色**：
前者模拟 PLC，后者把网关变成 PLC。测试里两者可以同时在一条链路上跑，正好覆盖
「采集 → 缓存 → 反向暴露」的完整闭环。

---

## 4. 错误模型

```go
type ErrorKind string

const (
    ErrTimeout      ErrorKind = "timeout"
    ErrDisconnected ErrorKind = "disconnected"
    ErrProtocol     ErrorKind = "protocol"   // 帧格式/CRC/长度错误
    ErrAddress      ErrorKind = "address"    // 地址不存在/越界/异常码 02
    ErrPermission   ErrorKind = "permission" // 设备拒绝（写保护/权限）
    ErrUnsupported  ErrorKind = "unsupported"
    ErrInternal     ErrorKind = "internal"
)

type OpError struct {
    Kind      ErrorKind
    Op        string // dial | read | write | decode | probe
    Device    string
    Protocol  string
    Msg       string
    Temporary bool   // true 表示可重试
    Err       error
}
```

### 4.1 上层的处理策略

| Kind | 重试 | 熔断计数 | 质量码 | 用户可见提示 |
|---|---|---|---|---|
| `timeout` | ✓（退避） | ✓ | `timeout` | 设备响应超时 |
| `disconnected` | ✓（重连） | ✓ | `comm_error` | 连接已断开 |
| `protocol` | ✗（可能是实现 bug） | ✓ | `bad` | 协议错误（附帧摘要） |
| `address` | ✗ | ✗ | `address_error` | 地址不存在，请检查点表 |
| `permission` | ✗ | ✗ | `bad` | 设备拒绝写入 |
| `unsupported` | ✗ | ✗ | `bad` | 当前设备不支持该操作 |
| `internal` | ✗ | ✓ | `bad` | 内部错误（附 trace id） |

**关键区分**：`address` 不参与熔断。因为「点表写错」是配置问题，不应导致整台设备被熔断——这正是多数方案把「地址错误」与「通信错误」混为一谈后出现的现场问题（PA-10 明确区分了这两类，值得借鉴）。

---

## 5. 能力声明规范

### 5.1 四态的使用规则

| 状态 | 何时使用 | UI 表现 |
|---|---|---|
| `supported` | 所有支持的设备型号都可用 | 绿色「支持」 |
| `unsupported` | 协议本身不具备 | 灰色「不支持」 |
| `runtime_dependent` | 取决于型号/固件/配置 | 黄色「视设备而定」+ 提示 |
| `experimental` | 已实现但未经过现场验证 | 橙色「实验性」+ 警告 |

**禁止**：在不确定时声明 `supported`。宁可标 `runtime_dependent`，也不要给用户错误承诺。

### 5.2 参考值（Wave 1 驱动）

| 驱动 | Read | Write | Subscribe | Browse | Discovery | MultiRead | BitAccess |
|---|---|---|---|---|---|---|---|
| `modbus-tcp` | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `modbus-rtu` | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `modbus-ascii` | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `s7` | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `opcua` | supported | supported | supported | supported | supported | supported | unsupported |
| `mqtt` | supported | unsupported | supported | unsupported | unsupported | unsupported | unsupported |
| `virtual` | supported | supported | supported | supported | unsupported | supported | supported |

---

## 6. 版本化与兼容性

### 6.1 两级版本

| 版本 | 位置 | 变更规则 |
|---|---|---|
| `sdkVersion` | 契约（接口签名/语义） | 主版本变更 = 破坏性；次版本 = 新增可选能力 |
| `Meta().Version` | 驱动自身 | 自由语义化；与 SDK 版本独立 |

### 6.2 兼容性承诺

| 核心变化 | 驱动是否需要改 | 说明 |
|---|---|---|
| 新增可选接口（如 `Browsable`） | 否 | 类型断言即可 |
| 新增 `CapState` 枚举值 | 否 | 未知值按 `runtime_dependent` 处理 |
| 新增 `ConfigField.Type` | 否 | 前端忽略未知类型 |
| 修改 `ReadSpan` 字段 | **是** | 需升 `sdkVersion` 主版本 |
| 修改 `Conn` 方法签名 | **是** | 需升主版本 + 提供适配层 |

### 6.3 加载校验

```
1. Meta().SDKVersion 是否在核心支持的区间内？（当前 1.x）
2. Meta().ID 是否与注册表冲突？
3. Meta().AddressKinds 是否非空？
4. Capabilities 是否至少有一项 supported？
5. ConfigFields 的 Name 是否唯一？
6. 通道的 transport.kind 是否在 Meta().Transports 内？（装配期校验）
```

第 6 条是**装配期**校验而非连接期校验：把 `modbus-tcp` 配到串口上永远不会
成功，重试一万次也没用。留到 `Open` 时失败，现场看到的是「设备一直连接失败」；
在装配期失败，看到的是「channel "x": protocol "modbus-tcp" supports transports
[tcp], got "serial"」——同一件事，排查成本差一个数量级。错误码
`config.unsupported_transport`（HTTP 422）。

任一失败 → 拒绝加载，错误信息必须包含「哪个字段、期望什么、实际什么」。

---

## 7. 一致性测试套件

每个驱动必须通过以下用例（`internal/drivertest` 提供夹具）：

| # | 用例 | 断言 |
|---|---|---|
| T1 | `Probe` 成功 | 无错误；耗时 < timeout |
| T2 | `Probe` 超时 | 返回 `ErrTimeout` 且 `Temporary=true` |
| T3 | 单点读 | 值类型与 `DataType` 一致；质量 `good` |
| T4 | 批量读 | 结果数量 = 点数；顺序对齐 |
| T5 | 空批量读 | 不 panic，返回空 |
| T6 | 地址越界 | 返回 `ErrAddress`，不重试 |
| T7 | 写入回读 | 写入后读回一致（允许量化误差） |
| T8 | 写入只读点 | 返回 `ErrPermission` 或 `ErrUnsupported` |
| T9 | 取消上下文 | `ctx.Cancel` 后 100ms 内返回 |
| T10 | `Close` 幂等 | 连续 3 次不 panic |
| T11 | 并发保护 | 上层保证串行；驱动在串行下结果稳定 |
| T12 | 半包响应 | 传输层正确拼帧（由 `Transport` 测试覆盖） |
| T13 | 异常码映射 | 每个异常码映射到正确 `ErrorKind` |
| T14 | 金报文 | 请求字节与期望响应字节完全匹配 |
| T15 | 半双工并发 | 同串口多设备并发读，响应不得张冠李戴 |
| T16 | 沉默从站 | 地址不存在时在 `timeout` 内返回 `ErrTimeout`，不得挂死 |
| T17 | 串口 deadline | 无数据时读在 deadline 附近返回（非 0、非 timeout×N） |
| T18 | 关闭打断读 | `Close` 后阻塞中的 `ReadFull` 在 2s 内返回 |

T15~T18 是串口链路专属用例。它们无法用内存流复现——内存流没有内核 tty
层，也没有真实的 poller 行为——因此用伪终端（pty）跑：pty 走的是完全相同
的 termios 与 tty 驱动路径，但不需要硬件，可在 CI 上执行。

### 7.1 金报文格式

```json
{
  "name": "read_holding_2_regs",
  "request": "000100000006010300640002",
  "response": "000100000007010304000A000B",
  "expect": [
    { "point": "p1", "type": "uint16", "num": 10 },
    { "point": "p2", "type": "uint16", "num": 11 }
  ]
}
```

存放于 `internal/driver/<proto>/testdata/golden/*.json`，由 `go test` 自动加载。

---

## 8. 虚拟设备规范

### 8.1 目的

1. 无真机开发与 CI；
2. 故障注入（唯一可控的健壮性验证手段）；
3. 驱动作者自测；
4. 演示与培训。

### 8.2 要求

| 要求 | 说明 |
|---|---|
| 与真实驱动同源 | 从站复用同一份 `Codec`（编码/解码方向相反） |
| 内存模型 | 支持寄存器区、线圈区、输入区；可预置初值 |
| 脚本化响应 | 支持「第 N 次请求返回异常码 X」「延迟 D 毫秒」 |
| 故障注入 | 丢包、半包、错 CRC、连接重置、慢响应 |
| 可观测 | 记录收到的请求数与最后请求（供断言） |

### 8.3 故障注入配置

```json
{
  "faults": [
    { "at": 3,  "kind": "exception", "code": 2 },
    { "at": 5,  "kind": "delay", "ms": 3200 },
    { "at": 7,  "kind": "drop" },
    { "at": 9,  "kind": "truncate", "bytes": 3 },
    { "at": 11, "kind": "reset" }
  ]
}
```

### 8.4 与一致性套件的关系

```
drivertest.Run(t, driver, virtualDevice)
  ├─ 启动虚拟设备（真实端口）
  ├─ 用驱动连接
  ├─ 逐条执行 T1..T14
  └─ 注入故障，断言错误分类正确
```

---

## 9. 开发流程（驱动作者视角）

```
1. 读协议知识库条目（帧结构/命令码/错误码/限制）
2. 写 Codec（纯函数）+ 金报文测试  ← 先于任何 I/O 代码
3. 写 Transport（或复用 tcp/serial）
4. 写 Conn（事务/重试/错误分类）
5. 写 Driver.Meta()（能力/地址类型/表单字段）
6. 实现虚拟设备（复用 Codec 的反向）
7. 跑一致性套件
8. 接入真实设备做「冒烟」（可选，不阻塞发布）
```

**顺序不能颠倒**：先写 `Codec` 与金报文，可以在没有网络的情况下验证 80% 的协议逻辑；先写 socket 循环则会陷入「连不上就不知道对不对」的困境（PA-28 的教训）。

---

## 10. 反模式清单

| 反模式 | 后果 | 正确做法 |
|---|---|---|
| 在 `Codec` 里 `time.Sleep` | 无法单测、阻塞调用方 | 延时属于 `Transport` 或配置 |
| 用包级变量存连接 | 多设备互相污染 | 连接在 `Conn` 实例 |
| `Read` 内自己重连 | 重连风暴、状态不一致 | 返回 `ErrDisconnected`，由运行时重连 |
| 返回 `error` 但不分类 | 上层无法决定重试/熔断 | 统一 `*OpError` |
| 批量接口内部退化为单点循环 | 性能崩塌 | 真正合并请求，或声明 `MultiRead=unsupported` |
| 写入「尽力而为」不返回结果 | 上层不知道成败 | 逐项返回 |
| 在驱动里读配置文件 | 无法测试、配置漂移 | 一切经 `OpenConfig` 注入 |
| 用 `panic` 处理协议错误 | 进程崩溃 | 返回 `ErrProtocol` |

---

## 11. 目录结构（驱动的标准形态）

```
internal/driver/<proto>/
  ├── driver.go        // Driver 实现 + Meta
  ├── conn.go          // Conn 实现（事务/重试）
  ├── codec.go         // Codec 实现（纯函数）
  ├── codec_test.go
  ├── golden_test.go
  ├── conformance_test.go
  ├── testdata/golden/*.json
  └── virtual/         // 可选：该协议的虚拟设备
```

---

## 12. 检查单（提交前）

- [ ] `Meta()` 的 8 个字段全部填写，`SDKVersion = "1.0"`
- [ ] `AddressKinds` 与实际支持的寻址一致
- [ ] 每种能力都有四态声明，未验证的标 `experimental`
- [ ] `Codec` 无 I/O、无时钟、无全局状态
- [ ] 所有错误包装为 `*OpError` 且分类正确
- [ ] 至少 10 条金报文
- [ ] 通过 T1–T14 一致性套件
- [ ] 虚拟设备可注入 5 类故障
- [ ] `go test -race ./internal/driver/<proto>/...` 通过
- [ ] 无 CGO、无新增 GPL/AGPL 依赖
- [ ] 有 `README.md` 说明协议限制与已知怪癖

---

## 13. 结论

1. **三层分离不是为了「好看」，而是为了让编解码成为可复用资产。** 同一份 `Codec` 被主站、从站、仿真器、金报文测试四处使用，这是「无设备也能开发」的技术前提。

2. **能力四态是对工业现实的尊重。** 「S7 支持符号寻址」这句话在 S7-300 与 S7-1200 上含义不同；布尔能力位只能撒谎，四态可以诚实。

3. **一致性套件 + 虚拟设备把「驱动质量」从主观判断变成可执行断言。** 这是本 SDK 与参考方案最大的差别：它们交付的是代码，我们交付的是「代码 + 可验证性」。
