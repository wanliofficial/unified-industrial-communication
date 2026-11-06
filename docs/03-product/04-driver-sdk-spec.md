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
    Secret      bool     `json:"secret,omitempty"` // 凭据字段：渲染成密码框，回显用固定掩码
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

#### 3.4.1 寄存器内的位写（读-改-写）

当协议只提供「整字写」、而点位的语义是「某一位」时（Modbus 保持寄存器最典型），
驱动必须自己完成 **读整字 → 置/清目标位 → 写整字**，不能把 `bool` 直接编码成
0/1 写下去——那会把同一寄存器里的其它 15 位一起覆盖，现象是「改了 A 点，
B 点跟着变了」，而配置与日志都看不出问题。

约束：

1. **读失败就不写**。宁可本次写入失败，也不能在不知道其它位的情况下写整字。
2. **同一驱动的写入要串行化**（`conn.wmu`）。两个并发的位写若不串行，会各自
   基于同一份旧值计算，后写的那个把先写的位改回去。
3. **缺位号必须报错**（`bit < 0`），不能猜 0 或 1。
4. **原子性边界要写进文档**：驱动只能保证「自己发出的两帧之间不插自己的写」。
   共享串口上的其它设备、或另一个主站直连同一从站，都不在保证范围内——一个
   寄存器只应有一个所有者，这是现场约束。
5. 原生支持位写的协议（S7 的 `DB1.DBX0.3`、Modbus 线圈）**不要**走这条路径：
   它们本来就能只写一位。

Modbus 参考实现：`codec/modbus` 提供 `EncodeReadRegisters` / `DecodeRegisters` /
`WordSetBit` / `EncodeWriteRegister` 四个纯函数，驱动侧 `writeBitRMW` 负责事务
顺序与错误分类。

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

### 3.8 Modbus 从站（TCP / RTU / ASCII）

从站不是「把主站反过来写」。它与主站面对的问题域有三处根本不同：

| 维度 | 主站（客户端） | 从站（服务端） |
|---|---|---|
| 数据来源 | 链路 | cache（本进程采集结果） |
| 报文边界 | 自己决定 | 对端决定，必须处理任意区间 |
| 写路径 | 发起写入 | 接收写入并**再转发**到南向设备 |
| 失败语义 | 本设备采集失败 | 对端的读请求可能跨越多个映射、多个设备 |

因此从站是 `internal/slave/modbus` 而不是 `driver/modbus` 的一个模式：两者共享
`codec/modbus` 的纯函数，但生命周期、并发模型与错误语义完全不同。

串口端点（RTU / ASCII）复用同一套映射、读路径与写穿透逻辑，但**分帧与地址语义
必须单独实现**——把 TCP 从站原样搬到串口，会在「其他从站的地址」「广播」「半帧」
三处同时出错。差异集中在 §3.8.2。

#### 3.8.1 配置即启用

```json
{
  "slaves": [{
    "id": "scada",
    "listen": "127.0.0.1:1502",
    "maxConnections": 8,
    "identity": {
      "vendorName": "UIC",
      "productCode": "uic-gateway",
      "majorMinorRevision": "1.0",
      "vendorUrl": "https://example.com/",
      "productName": "UIC 工业通信网关",
      "modelName": "UIC-GW",
      "userApplicationName": "Line 3"
    },
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
6. 从站 `listen` 不得与 `server.listen` 或其他从站的 `listen` 冲突。端口 0 永远不算冲突
   （测试与容器部署依赖它）。
7. `listen` 与 `serial` 恰好二选一：两个都给会产生「实际监听在哪」的歧义，两个都不给是
   一条永远起不来的配置。
8. `serial.port` 不得与任何 `channels[].transport.port`（串口主站）或其他从站重复。
   半双工总线上两个角色会互相抢答。串口端点上出现 `maxConnections` 同样是配置错误。

宽度来自 `Point.RegisterWidth()`：`bool`/`uint16` 为 1，`float32` 为 2，`float64` 为 4，
`string`/`bytes` 的 `address.length` 是**寄存器个数**而不是字符数。这条规则单独抽成
方法，就是为了避免每个调用点各自解释一遍 `length`。

#### 3.8.2 串口端点（RTU / ASCII）

串口从站与 TCP 从站共用同一份映射、读写与写穿透代码，但分帧与地址语义必须单独
处理。四个差异，每一个都会让「把 TCP 从站原样搬到串口」的实现出错：

| 维度 | TCP | 串口 RTU / ASCII |
|---|---|---|
| 分帧 | MBAP 长度字段 | RTU：按功能码推导长度，推不出来时靠 3.5 字符静默；ASCII：`:` 起始、CRLF 结束 |
| 地址不是本机 | 回 `0x0B` | **必须静默**：一条总线上多台从站，替别人的地址应答就是串扰 |
| 广播（地址 0） | 无此概念 | 执行写但不回应；广播读没有意义 |
| 连接数 | `maxConnections` | 不适用：物理端口只有一个，同一时刻只服务一个请求 |

**配置**（`listen` 换成 `serial`，其余字段完全一致）：

```json
{
  "slaves": [{
    "id": "scada-rtu",
    "serial": {
      "port": "/dev/ttyUSB0", "framing": "rtu", "baud": 19200,
      "dataBits": 8, "parity": "none", "stopBits": 1
    },
    "units": [{ "unitId": 1, "mappings": [ ... ] }]
  }]
}
```

`serial.framing` 必须显式写 `rtu` 或 `ascii`，不做自动探测：两种帧的校验方式与
分帧规则完全不同，探测失败只会把故障推迟到第一次采样。

**RTU 分帧**：

1. 等地址字节，读功能码。
2. 已知功能码按表推导整帧长度，一次读满：读类固定 8 字节、FC11/FC17 为 4 字节、
   FC15/16 为 `9 + byteCount`、FC43/MEI 0x0E 为 7 字节。
3. 推导不出来（未知功能码、其他 MEI）时读到链路静默为止，再用 CRC 校验；上限
   256 字节，防止一条噪声线把缓冲区撑爆。
4. CRC 错：丢弃整帧，读到静默并 `tcflush` 残字节后重新同步。
5. 半帧（帧内超时）：丢弃并重新同步——**绝不能**把下一帧的开头接上去。

帧间静默按 3.5 个字符时间计算，下限 5 ms：USB 转串口适配器的转发延迟普遍在
1..16 ms，严格按 9600 波特下的 3.6 ms 切帧会把完整帧截断，症状是「偶发 CRC 错」。

**ASCII 分帧**：以 `:` 起始、CRLF 结束，帧外噪声逐字节丢弃；帧内字符间隔上限
1 s（规范值）。响应按**同一分帧方式**回写（RTU 二进制、ASCII 十六进制文本）——
用错对端会收到一条永远解析不了的帧。

**广播与地址过滤**：串口上 `unit=0` 的写请求会被应用到全部单元并**不回应**；
读请求直接忽略。非本机单元号的请求既不应答也不计数为错误——总线上每台从站都
会收到它，谁回应都会造成串扰。

**断线重连**：串口读返回非超时错误（USB 适配器掉线、EIO）时，从站按
1s → 2s → … → 30s 退避重开同一路径，成功会记录 `slave_serial_reopened`。
要求重启进程才能恢复一个「拔插一下就好」的适配器，是现场最不必要的停机。

**指标**：串口特有的帧失败单独计数（见 §3.8.6 的 `uic_slave_bad_frames_total`）。
把它们混进 `uic_slave_requests_total` 会让「对端发了多少请求」与「线路上坏了多少帧」
两件事无法区分。

#### 3.8.3 读路径：对端要什么就给什么

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

#### 3.8.4 写路径：必须穿透到南向

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

#### 3.8.5 连接与生命周期

- accept 后按 `maxConnections`（默认 8，上限 64）占用槽位；超限**立刻关闭**而不是排队。
  排队会把「过载」伪装成「网络慢」。
- 空闲连接按 `idleTimeout`（默认 5 分钟）回收。
- `Close` 幂等：关闭监听、关闭全部连接、等待 goroutine 退出；串口端点关闭串口句柄，
  阻塞中的读会立刻返回，循环因此能退出。
- 单个连接的事务是串行的：Modbus TCP 允许流水线，但多数主站不发；按序处理可以
  避免响应乱序带来的额外状态机。
- 串口端点没有「连接」概念：一个端口、一条循环、同一时刻一个事务（RS-485 是半双工）。

**热更新**：`syncSlaves` 比较指纹决定是否重建某个从站。指纹覆盖：

1. 从站配置本身（监听地址、单元、映射）；
2. **被映射点位的类型、宽度、地址长度、可写性**。

只比较映射会漏掉最危险的一种变化：把温度从 `uint16` 改成 `float32` 时映射表
一个字都没改，但线上字节完全不同。重建时先关旧监听再建新的；重建失败会记录
`slave_rebuild_failed` 并保留其余从站，不让一个坏配置拖垮全部服务。

#### 3.8.6 指标

| 指标 | 标签 | 含义 |
|---|---|---|
| `uic_slave_requests_total` | `slave` / `unit` / `fc` / `result` | 请求总数；`result=exception` 表示返回了异常码，`result=broadcast` 是串口广播写 |
| `uic_slave_degraded_reads_total` | `slave` | 用非 good 质量数据应答的次数（值保留，质量靠指标暴露） |
| `uic_slave_write_total` | `slave` / `result` | 写转发次数与结果 |
| `uic_slave_bad_frames_total` | `slave` / `reason` | 串口帧失败：`crc` / `lrc` / `short` / `noise`；与「请求数」分开，才能区分线路质量与对端行为 |

#### 3.8.7 诊断与设备识别（FC8 / FC11 / FC17 / FC43）

读写类功能码解决「值是多少」，这四类解决「你是谁」。缺了它们，上位机的设备树
里只有一堆无名寄存器；有它们，才能显示厂商/型号/版本，并在设备替换时自动匹配
点表。它们都不碰映射表，因此失败语义也与读写不同：**永远不会因为「没有样本」
而失败**。

**FC8 Diagnostics**

| 子功能 | 名称 | 行为 |
|---|---|---|
| `0x0000` | Return Query Data | 原样回显请求里的两个数据字节 |
| `0x000A` | Clear Counters | 清 `requests` / `exceptions` / `events` 三个**协议计数** |
| `0x000B` | Bus Message Count | 收到的消息总数（16 位截断） |
| `0x000C` | Bus Communication Error Count | TCP 上恒为 0（没有 CRC/字符层）；串口上返回 CRC/LRC 错误计数 |
| `0x000D` | Bus Exception Error Count | 返回过异常响应的请求数 |
| `0x000E` | Server Message Count | 同 `0x000B`（本实现所有消息都进同一入口） |
| `0x000F` | Server No Response Count | 恒为 0：TCP 从站总会应答 |
| `0x0010` | Server NAK Count | 恒为 0：不产生 NAK |
| `0x0011` | Server Busy Count | 恒为 0：不返回 `0x06` |
| `0x0012` | Bus Character Overrun Count | 恒为 0：字节流由内核分帧，无此概念 |

未知子功能返回异常 `0x01`；PDU 长度不是 5 字节返回 `0x03`。`0x000A` **只清协议
计数（含串口 CRC/LRC 计数），不动指标**：`uic_slave_requests_total` 是给监控系统看的水位，如果被对端
一个诊断请求清零，监控曲线会出现无法解释的断崖。

**FC11 Get Comm Event Counter**

响应固定 5 字节：`FC` + `status`（恒 0）+ `event count`。

- **自身不计数**：否则「读计数器」这个动作本身会让计数器增长，曲线变成查询频率，
  而不是设备处理消息的速率。
- **只统计成功处理的消息**：异常响应不算「成功完成」。这条语义在测试里固定下来。

**FC17 Report Server ID**

响应：`FC` + 字节数 + 产品代码（`identity.productCode`）+ 运行指示 `0xFF`（ON）。
产品代码为空时用默认值 `uic-slave`；长度受配置校验限制（≤120 字节），保证
`字节数` 字段不会溢出。

**FC43 / MEI 0x0E Read Device Identification**

请求 4 字节：`0x2B` + MEI `0x0E` + 读取码 + 对象 id。

| 读取码 | 含义 | 返回对象 |
|---|---|---|
| 1 | basic（流式） | `0x00` 厂商名、`0x01` 产品代码、`0x02` 版本 |
| 2 | regular（流式） | basic + `0x03` 厂商 URL、`0x04` 产品名、`0x05` 型号 |
| 3 | extended（流式） | regular + `0x06` 应用名 |
| 4 | 按对象 id 读取 | 只返回请求的那一个对象；不存在返回异常 `0x02` |

响应头 7 字节：`FC` + `MEI` + `读取码` + `符合级别` + `More Follows` + `Next Object Id` +
`对象个数`，随后是 `(对象 id, 长度, 值)` 序列。

- **符合级别恒为 `0x83`**（extended + 流式与单对象访问）。按规范它描述**设备能力**，
  不是「这次返回了哪些对象」；若随请求码变化，主站会误判本设备不支持按对象读取，
  把已实现的能力藏起来。
- **分页**：响应必须放进 253 字节 PDU。装不下时 `More Follows = 0xFF`，
  `Next Object Id` 指向下一页起点，主站续读即可。每个对象**恰好出现一次**。
- 未知 MEI 返回 `0x01`；未知读取码返回 `0x03`；空值对象直接省略（返回长度为 0 的
  对象等于让上位机显示一个空字段）。

**身份标识从哪来**

`slave.identity` 全部字段可选；缺省值保证 basic 三件套非空（厂商 `UIC`、产品代码
`uic-slave`、版本取运行版本）。`majorMinorRevision` 的空值会被**运行时构建版本**
覆盖——上位机屏幕上的版本必须与进程里跑的二进制一致，否则现场排障第一个问题
（「你跑的是哪个版本」）就答不上来。标识只是公开的产品信息，不要写入内部主机名、
IP、序列号等资产信息：FC43 是匿名可读的。

#### 3.8.8 边界

| 项 | 状态 | 说明 |
|---|---|---|
| Modbus TCP 从站 | ✅ | MBAP、四区映射、读写、连接上限、热更新 |
| Modbus RTU / ASCII 从站 | ✅ | 串口按功能码推导帧长（推不出来时读到静默）、非本机地址静默、广播写不回应、CRC/LRC 错后重新同步、掉线按退避重开；与主站角色按端口互斥（配置期校验） |
| 诊断与识别（FC8/FC11/FC17/FC43） | ✅ | 回显/清计数/六类计数、事件计数器、服务器 ID、读设备标识（basic/regular/extended/按对象读取 + 分页） |
| 文件记录（FC14/15/20/21） | ❌ | 需要一套独立的记录寻址与流控；现场用它的设备很少，收益不抵复杂度 |
| 多寄存器位写（掩码写，FC22） | ❌ | 从站不实现 FC22；主站侧的寄存器位写由主站驱动用 FC3+FC6 读-改-写完成，不依赖该功能码 |
| 主动上报（unsolicited） | 不适用 | Modbus 本身是请求-响应模型 |

内置从站（`internal/sim/modbus`）只用于自测上游设备，与本从站是**两个不同的角色**：
前者模拟 PLC，后者把网关变成 PLC。测试里两者可以同时在一条链路上跑，正好覆盖
「采集 → 缓存 → 反向暴露」的完整闭环。

### 3.9 OPC UA（二进制，SecurityPolicy=None）

OPC UA 与前面所有协议的**结构性差异**只有一条：它有**三层可独立失效的状态**。
把「TCP 连着」当成「能用」，是这个协议最常见的接入错误。

#### 3.9.1 三层状态必须分开判断

| 层 | 建立动作 | 失效表现 | 恢复动作 |
|---|---|---|---|
| TCP 连接 | dial | 读写直接 IO 错误 | 重新 dial |
| 安全通道（SecureChannel） | `OPN` | `BadSecureChannelIdInvalid` | 重新 `OPN`（不必重连 TCP） |
| 会话（Session） | `CreateSession` + `ActivateSession` | `BadSessionIdInvalid` / `BadSessionClosed` | 重新建会话（通道可留） |

**为什么不能只看一个 `connected bool`**：通道在 TokenLifetime 到期后失效，但 TCP
还好；会话在空闲超时后被服务端回收，但通道还好。三种失效的恢复代价差一个数量级，
合并判断会让驱动「每次都全量重连」——在一条 20 台设备的产线上，这等于每秒重连。

`Close` 的顺序同样是**反过来的**：`CloseSession` → `CLO`（关通道）→ 关 TCP。
反过来会让服务端留下悬挂会话，直到它的 SessionTimeout 到期（常见 10 分钟）。

#### 3.9.2 连接序列

```
主站 → HEL（receiveBufferSize / sendBufferSize / maxMessageSize / maxChunkCount / endpointUrl）
从站 → ACK
主站 → OPN（SecurityPolicy=None, SecurityMode=None, RequestType=Issue, RequestedLifetime=3600000）
从站 → OPN 响应（ChannelId + TokenId + RevisedLifetime）
主站 → CreateSession
主站 → ActivateSession（匿名：AnonymousIdentityToken，PolicyId="anonymous"）
从站 → ActivateSessionResponse
主站 → Read / Write / Browse / GetEndpoints
```

| 约束 | 原因 |
|---|---|
| 必须先 HEL/ACK | 跳过 HEL 直接发 OPN，多数服务端直接断链，且不给错误码 |
| TokenId 必须在每条 MSG 里带上 | 通道续订（Renew）后 TokenId 会变，继续用旧值会被 `BadSecureChannelTokenUnknown` 拒绝 |
| TokenId 寿命到 3/4 时续订 | 等到过期再续，请求会在「已发出、未响应」的窗口里失败；提前续订是规范推荐做法 |
| `ActivateSession` 不可省 | 部分服务端允许未激活会话读、拒绝写。表现是「能看不能控」，最难排查 |
| 响应必须按 `RequestId` 匹配 | 迟到的响应（前一次超时的）会串到当前请求上，把 A 点的值写成 B 点的 |

#### 3.9.3 地址：NodeId 而不是「区 + 偏移」

OPC UA 的寻址单位是**节点标识**，不是地址。因此：

```json
{ "address": { "kind": "symbol", "path": "ns=2;s=Line1.Temperature" } }
```

| 写法 | 解析结果 | 说明 |
|---|---|---|
| `ns=2;s=Line1.Temperature` | 字符串 NodeId | 现场最常见（大多数 PLC/网关按名称暴露） |
| `ns=2;i=1001` | 数值 NodeId | 紧凑，但人不可读 |
| `i=2258` | `ns=0;i=2258` | 命名空间 0 可省 |
| `Temperature` | `ns=0;s=Temperature` | 裸字符串等价于 ns=0 |
| `ns=2` / `ns=2;x=1` | **配置期报错** | 前者漏标识符、后者标识符类型未知 |

最后一行是刻意加的：这两种写法按「裸字符串」解释也能构造出一个 NodeId，但它
永远查不到任何节点，服务端只会回「节点不存在」——错误信息指不回配置。这类
笔误在批量导入点位时非常常见，必须在 `-check` 阶段就拦住。

**为什么不用 register 寻址**：OPC UA 没有「寄存器」概念。硬塞进 register 会丢掉
命名空间与字符串标识，等于只支持那一小撮用数值 NodeId 的服务端。

#### 3.9.4 类型映射与越界

| 平台类型 | OPC UA 内置类型 | 备注 |
|---|---|---|
| `bool` | Boolean | 数值写入非零即真 |
| `int16` / `uint16` / `int32` / `uint32` / `int64` / `uint64` | 对应整型 | 越界**判坏**，不截断 |
| `float32` / `float64` | Float / Double | float32 真降精度（不是先转 double 再回来） |
| `string` | String | DateTime 读成 string 时按 RFC3339Nano 格式化 |
| `bytes` | ByteString | — |
| `bcd16` / `bcd32` | **报错** | BCD 是编码方式不是数据类型，只能在协议层解码后上报 |

越界必须判坏而不是截断：一个把 70000 截成 4464 的温度点，会让操作员按错误的
数字做决策，而且不会有任何告警。判坏至少会亮出来。

#### 3.9.5 部分失败语义

OPC UA 的 `Read` 是**逐项**返回 `DataValue.Status` 的：一个节点不存在，不影响
其他节点返回值。这与 Modbus 的「异常码 = 整帧失败」完全不同。

| 情况 | 驱动行为 |
|---|---|
| 部分节点坏 | 好样本照常返回；坏节点带 `Err` 与对应质量码；整体 `err == nil` |
| 全部节点坏 | 返回样本 + 一个整体错误（熔断器需要看到「这台设备全坏了」） |
| 地址非法（配置笔误） | 在发请求前就判定，计入 `ErrAddress`；不发无效请求 |
| 服务级错误（`ServiceFault`） | 整条请求失败，按状态码分类 |

「全部失败必须返回 error」这一条容易被漏掉：如果只在 `DataValue.Status` 上标坏，
上层的熔断器与告警都看不到异常，设备会一直「在采但全坏」。

#### 3.9.6 状态码 → 错误分类

| 状态码 | 分类 | 理由 |
|---|---|---|
| `BadNodeIdUnknown` / `BadNodeIdInvalid` / `BadAttributeIdInvalid` | `ErrAddress` | 点位配错了，不是设备坏了；不该熔断整台设备 |
| `BadUserAccessDenied` / `BadSecurityChecks` / `BadNotWritable` | `ErrPermission` | 权限问题，重试无用，需要改配置 |
| `BadServiceUnsupported` | `ErrUnsupported` | 对端不支持该服务 |
| `BadTimeout` / `BadWaitingForInitialData` | `ErrTimeout` | 可重试 |
| `BadSessionIdInvalid` / `BadSessionClosed` / `BadSecureChannelIdInvalid` / `BadNoCommunication` / `BadNotConnected` / `BadCommunicationError` | `ErrDisconnected` | 需要重建会话或通道 |

其余未列出的 `Bad*` 归 `ErrProtocol`。

#### 3.9.7 明确的能力边界

| 项 | 状态 | 说明 |
|---|---|---|
| SecurityPolicy None / SecurityMode None | ✅ | 明确的产品边界，不是「暂时没做」 |
| 匿名认证 | ✅ | 用户名/证书认证未实现 |
| 批量读（≤100 节点/请求） | ✅ | 按服务端的 OperationLimits 保守取值 |
| 写（单点/批量） | ✅ | 逐项状态码回传 |
| Browse / GetEndpoints | ✅ | 驱动层提供，供后续「点位浏览」界面用 |
| 订阅（CreateSubscription / MonitoredItems / Publish） | ✅ | 通道级 `subscribe` 开关（默认 false = 轮询）；订阅走独立第二条连接，Publish 长轮询不阻塞 Read/Write；每个带序列号的通知都在下一次 Publish 中确认；`QueueSize=1 + DiscardOldest` 只保留最新值；断开自动重建，期间 Read 回退真读 |
| 历史读取（HistoryRead） | ❌ | 属 Wave 2 历史库范畴 |
| 方法调用 / 事件 / 别名 | ❌ | — |
| 证书 / 信任列表 / 签名校验 | ❌ | 属于另一套运维体系，混进来会让「连不上」的原因不可判定 |
| 分块响应（Chunked） | ❌ | 见到分块直接报错，比拼错更强 |

> 订阅的两条硬规则：
>
> 1. **建订阅失败就让 `Open` 失败**，绝不静默退化成轮询。配置里写了
>    `subscribe=true`、实际却在轮询，是那种「跑起来一切正常、三个月后有人
>    问为什么数据延迟变了」的问题。要轮询就显式关掉开关。
> 2. **推送值只在「订阅健康 + 未超保鲜期 + 类型一致」时才替代真实读**。
>    其余情况一律回退 `Read`——拿最后一次成功的读数冒充当前值，是这类
>    实现最危险的失效模式。

#### 3.9.8 内置服务端（`internal/opcuaserver`）

`uic sim -protocol opcua` 起一个最小服务端，用途与 Modbus/S7 的内置从站一致：

| 项 | 实现 |
|---|---|
| 服务 | HEL/ACK、OPN、CreateSession、ActivateSession、CloseSession、Read、Write、Browse、GetEndpoints、CloseSecureChannel；订阅：CreateSubscription、CreateMonitoredItems、DeleteMonitoredItems、DeleteSubscriptions、SetPublishingMode、Publish |
| 地址空间 | `Root/Objects/{Server/ServerStatus/CurrentTime, Plant/{Counter,Temperature,Running,Label,Setpoint}}` |
| 安全 | 仅 Policy=None + 匿名；非匿名令牌回 `BadUserAccessDenied` |
| 会话 | 未激活会话的读写回 `BadSessionIdInvalid`（不是断链） |
| 写校验 | Variant 类型必须与节点 DataType 一致，否则 `BadTypeMismatch` |
| 订阅 | 监视项建立后立刻回一次当前值；`Set` 与写穿透都触发通知；队列语义 `QueueSize=1 + DiscardOldest`；空闲按 `MaxKeepAliveCount` 个发布周期发空通知；Publish 在独立 goroutine 中等待，响应按 RequestId 乱序返回（规范允许） |
| 不支持的服务 | 回 `ServiceFault` + `BadServiceUnsupported`，不断链 |

它**与主站共用同一份 `codec/opcua`**：主从分开实现编解码，会让「编码错」与
「解码错」互相抵消，自测永远全绿。共用同一份实现才能保证测的是协议而不是笔误。

服务端内核是独立包（`internal/opcuaserver`）而不是演示脚本的一部分，因为
它有两个消费者：`NewSimulator(addr)` 起演示地址空间供 `uic sim -protocol opcua`
与驱动一致性测试使用；`New(Config{...})` 被北向 OPC UA 用来暴露网关的点位表。
同一个内核支撑两种角色，才能保证「演示里能用的东西，生产里也能用」。

内核接口（北向用的是带 `OnWrite` 的那一版）：

| 成员 | 用途 |
|---|---|
| `Config{Addr, Nodes, ApplicationName/URI, ProductURI, OnWrite, Log}` | 启动参数；`OnWrite` 为 nil 表示「直接写内存并推送」（演示语义） |
| `New(Config) (*Server, error)` | 监听失败即返回错误 |
| `Update(NodeID, Variant, StatusCode, time.Time)` | 写值 + 质量码并通知订阅者 |
| `Value(id) (Variant, StatusCode, bool)` | 读当前值与质量（三返回值，质量不能省） |
| `Set(id, Variant)` | 演示/测试用的便捷写入（Good 质量） |
| `StandardNodes()` | Root/Objects/Server/ServerStatus/CurrentTime/Types/Views；`CurrentTime` 由内核每秒刷新 |

`OnWrite` 的语义是**两阶段**：先 `RLock` 校验类型与可写性、解码出平台值，再在锁外
调用回调，最后 `Lock` 写回并通知。北向模式下回调会走到真实设备，耗时以秒计；持锁
调用会让这期间所有 Read/Browse 一起堵死。回调返回非 Good 时节点值保持不变——
「写入被接受」与「值已经改了」是两件事。

---

### 3.10 MQTT 南向（订阅取数）

这是全项目**唯一的推送型协议**：设备不会因为「你问了一次」而回答。而 `driver.Conn`
是拉取契约（`Read` 立即返回一批样本）。两种范式之间的转换只有一种做法是对的，
其余做法都能编译通过、也能在实验室里跑出值，但在现场一定会出问题。

#### 3.10.1 唯一正确的适配：订阅 + 本地最新值表

| 做法 | 结果 |
|---|---|
| **连接时订阅，消息进本地最新值表，`Read` 只读表**（本实现） | ✅ `Read` 耗时与网络无关；设备安静时也不超时；retained 的历史消息与实时消息可以按到达时间区分 |
| 在 `Read` 里发一条请求等回应 | ❌ 没有「请求」这个东西。只能等到超时（采一个点 = 一个超时时间）；retained 消息会被当成「刚刚发生的变化」 |
| `Read` 直接返回上一次的消息，不记时间 | ❌ 无法表达「这条消息是 3 小时前的」，质量码永远是 good |
| 起后台协程不停重连、把值推给 cache | ❌ 驱动不该绕过采集器直接写 cache（会跳过去重、死区、序号、代次四道治理） |

因此有一条硬性约束：**MQTT 点位的质量码只能反映「消息新鲜度」**，不能反映
「设备现在是否正常」。这是协议决定的，不是实现偷懒——同一个主题上可能既有
数据消息也有状态消息，驱动没有办法验证「这个值是此刻的真实值」。

#### 3.10.2 订阅建立

| 规则 | 原因 |
|---|---|
| 按点位的 `address.path` 收集主题，**去重后一次 SUBSCRIBE 批量发出** | 同一主题上常配多个点位（JSON 里的不同字段）。不去重会让 Broker 收到 N 条相同过滤器，QoS1 下还会收到 N 份相同消息 |
| `path` 必须是精确主题，**禁止 `+` / `#`** | 一个点位若对应 N 个主题，「这个值属于哪台设备」就无法回答；映射关系从 1:1 变成 1:N，写入也会失去目标 |
| SUBACK 返回 `0x80` 视为**打开失败**，不做静默容忍 | ACL 拒绝时若按成功处理，会得到一台「在线但永远没数据」的设备——它和「设备没发数据」在现场完全无法区分 |
| 重连后**必须重新 SUBSCRIBE** | 本实现使用 CleanSession=true（不做持久会话），Broker 不保留订阅。只重连不重订会得到一个安静的空连接 |
| QoS 取 1 | QoS0 在网络抖动时静默丢消息，而订阅驱动没有「补读」这回事：丢一条就在等下一轮上报，低频设备上表现为「数据长时间不动」 |

`ClientID` 由 `通道 ID + 设备 ID` 稳定派生（截断到 100 字节）。**不能带时间戳或随机数**：
每次重连都换 ClientID 会让 Broker 认为是新客户端，触发上一条连接的遗嘱，消费方看到设备反复上下线。

#### 3.10.3 三档质量码

`staleAfter = max(3 × device.Interval, 5s)`，设备未配周期时兜底 30s。取 3 倍周期是因为
单次丢包不该立刻判 stale；下限 5s 是因为半秒周期的设备本来就不该走 MQTT。

| 状态 | 条件 | 返回的值 | 说明 |
|---|---|---|---|
| `good` | 收到过，且距最后一条 < `staleAfter` | 最新值 | — |
| `stale` | 收到过，但已超过 `staleAfter` | **保留最后值** | 清空会让趋势图断点，运维也看不出「最后是多少」 |
| `bad` | 从未收到过任何消息 | 零值 | 携带 `ErrProtocol` 便于排障 |

载荷解析失败**不覆盖**已有的好值：同一主题上很可能还有状态/日志类消息，
把「解不出」当成「设备坏了」是误判。失败只累加 `badPayloads` 计数，让旧值按
`staleAfter` 自然降级。

#### 3.10.4 载荷解码

| 形态 | 判定 | 例 |
|---|---|---|
| 纯文本标量 | `address.selector` 为空 | `21.5` / `ON` / `LINE-1`（首尾空白会 trim，设备常带 `\r\n`） |
| JSON 字段 | `selector` 非空 | `{"data":{"temperature":23.75}}` + `data.temperature` |

selector 支持 `a.b.c` 与 `values[0].v` 两种段：**先按键名下钻，再按下标取元素**。
只做下标会把文档本身当成数组，报出一个与真实原因无关的类型错。

数值转换的三条规则，都是从「上层按这个数字做决策」倒推的：

1. **越界判坏，不截断**：`uint16` 收到 `70000` 必须报错。截断成 `4464` 会让操作员
   按错的数字决策，比没有数据更糟。
2. **布尔接受常见写法**：`true/false`、`1/0`、`ON/OFF`、`on/off`。
3. **写入整数不带小数点**：`uint16` 写 `7` 发 `"7"` 而不是 `"7.0"`——大量设备
   用 `strconv` 严格解析，`7.0` 会被拒。

#### 3.10.5 写入 = 向同一主题发布

写入没有独立的确认通道，所以结果质量码是 **`uncertain`**：`Publish` 返回成功只说明
消息进了 Broker，不代表设备已经执行。只有回读到一致的值，采集到的样本才会变 good。
这让「写入成功」与「设备已生效」在 API 上保持可区分。

QoS 取 1：控制命令不能是 fire-and-forget。

#### 3.10.6 链路自检：一条消息都没有 ≠ 断链

这是订阅型接入最容易写错的地方，两个方向都会错：

| 写法 | 后果 |
|---|---|
| 一轮 `Read` 没有好消息就返回整体错误 | ❌ 采集器按「断链」处理 → 拆连接重建 → 每轮重连、重新订阅，而设备根本没故障 |
| 只看「TCP 连接标志」判断在线 | ❌ 拔网线 / 对端断电 / NAT 表项过期时，本地 socket 不会报错，写进内核缓冲还返回成功 → 「永远在线、永远没数据」 |

本实现的规则是：

1. `Read` 在链路正常时**永不返回整体错误**，逐点返回质量码（`bad` / `stale` 见 3.10.3）。
2. 只有当「一条好消息都没有」**且**「已经安静超过一个 `staleAfter`」时，才做一次
   `PINGREQ → PINGRESP` 往返；往返失败才返回 `ErrDisconnected`，让上层重建连接。
3. `Ping` **必须等 PINGRESP**。写成功不算活着——这是上面第二行的直接推论。
   PINGRESP 报文里没有 packet id，因此同一时刻只能有一次在飞的 PINGREQ（单飞）。

`Probe`（手工探活 / 熔断后的探测）走同一条路径。keepalive 循环也等 PINGRESP，
所以半死链路在后台就会被打断，不必等到某个点位彻底 stale 才暴露。

#### 3.10.7 能力边界与内置 Broker

| 项 | 状态 | 说明 |
|---|---|---|
| MQTT 3.1.1 明文 TCP | ✅ | 不支持 TLS / WebSocket（配置期显式报错，不静默降级） |
| QoS0 / QoS1 | ✅ | QoS2 在编解码层直接拒绝：客户端等的是 PUBACK，等 QoS2 会等成超时 |
| 订阅取数 | ✅ | 批量 SUBSCRIBE、去重、重连重订、三档质量码 |
| 纯文本 / JSON 载荷 | ✅ | selector 二级寻址，`a.b` 与 `a[0].b` |
| 写入（发布） | ✅ | 结果质量码 `uncertain`，回读一致才转 good |
| 持久会话 / ACL / 共享订阅 | ❌ | CleanSession=true；订阅权限交给 Broker |
| 二进制协议绑定（CBOR / Protobuf / Sparkplug B） | ❌ | 需要 schema 协商，属另一层产品功能 |
| 通配符点位（`plant/+/temp`） | ❌ | 见 3.10.2：值无法归属到具体点位 |

内置 Broker（`internal/sim/mqtt`）同时是三样东西：`uic sim -protocol mqtt` 的运行时、
集成测试的对端、以及 `examples/mqtt-southbound.json` 的零依赖依赖项。它**只有一份
实现**——测试封装（`internal/mqtt/mqtttest`）只负责绑定 `*testing.T` 的生命周期与
等待断言。两份 Broker 会慢慢长成不同行为，最终制造出「客户端测试通过、换真 Broker 失败」
这类最难查的分歧。

### 3.11 计算点（`computed`）

这是全项目**唯一没有物理链路**的驱动：点位不从任何设备读取，而是由其他点位的值
经表达式算出——能耗合计、温差、效率、简单的阈值判定。它存在的理由不是「省一次
采集」，而是把现场公式从 PLC 程序里搬出来，让公式可审计、可版本化、可跨协议复用。

#### 3.11.1 为什么是一个驱动，而不是加工链里的一个算子

`internal/pipeline` 已经有一个加工链（缩放 → 偏移 → 质量 → 死区），看起来「加一个
公式算子」更省事。但两者能拿到的输入根本不同：

| 方案 | 能拿到什么 | 结论 |
|---|---|---|
| 加工链算子 | 只有**当前这一个点位**的原始值 | ❌ `功率 × 费率` 拿不到第二个点 |
| 采集器特判 | 能拿到 cache，但要为它写一套调度、依赖排序、错误处理 | ❌ 与已有点位治理并行出第二套实现 |
| **独立驱动**（本实现） | `PointRef` + 只读的 `Lookup` | ✅ 复用 planner / 调度 / 缓存 / 死区 / 北向 / 权限的全套治理 |

关键收益是**形态同构**：计算点在配置、API、SSE、MQTT 主题、健康检查里与其他点位
完全一样，运维不需要学第二套概念。代价是它必须遵守驱动的全部契约（包括那条
「一次 `Read` 只拿到一批 `PointRef`」）——这恰好也是下面几条设计的成因。

#### 3.11.2 配置形态

```json
{
  "id": "ch-calc",
  "protocol": "computed",
  "transport": { "kind": "memory" },
  "concurrency": 1,
  "timeout": "1s"
}
```

```json
{
  "id": "cost",
  "dataType": "float64",
  "unit": "元",
  "writable": false,
  "address": { "kind": "computed", "expr": "line-a:power * line-a:rate" }
}
```

| 规则 | 违反时的行为 | 原因 |
|---|---|---|
| `address.kind` 必须是 `computed` | 配置期报 `protocol "computed" does not accept ...` | 驱动只接受这一种寻址，混配说明配置写错了 |
| `address.expr` 必填、≤1024 字节、不含控制字符 | 配置期报错 | 上限不是省内存，是防止深层嵌套把求值拖成指数级 |
| 其余地址字段必须为零值 | `address: mixed fields for computed kind` | 把计算点当寄存器写、或改类型时忘了删 `offset`，是最常犯的错 |
| `writable` 必须为 false | `computed points are read-only` | 否则会产生一条永远失败的写入路径，运维点了「下发」只得到权限错 |
| `dataType` ⊆ {bool, int16/uint16/int32/uint32/int64/uint64, float32/float64} | 配置期报错 | 字符串没有运算符可消费；BCD 是寄存器编码而不是「值」，算出来没有意义 |

#### 3.11.3 引用语法与减号空格规则

引用写作 `device:point`，**用冒号，不用点号**。

理由不是美观：设备 ID 与点位 ID 都允许包含点号（`line.a` 是合法 ID），用点号
分隔会产生「`a.b.c` 到底指谁」的歧义——是本设备 `a` 的 `b.c`，还是设备 `a.b`
的点位 `c`？冒号在 ID 字符集之外，因此没有歧义。

代价是 ID 允许 `-`，而 `-` 同时是减号。消歧规则只有一条，但必须写进文档：

> **减号两边要留空格。** 词法按「连续段里有没有冒号」判定引用：`line-a:power-1`
> 是一个引用（设备的 `line-a`、点位 `power-1`），`10-3` 是减法。

| 写法 | 解析为 | 备注 |
|---|---|---|
| `line-a:power * 2` | 引用 × 2 | — |
| `line-a:power-1` | 引用 `line-a` 的 `power-1` | 不是减法 |
| `line-a:power - 1` | 引用 − 1 | 要减 1 就必须加空格 |
| `line.a.power` | 报错并提示「用冒号而不是点号」 | 常见写法错误，报错要能直接照做 |
| `pow-1`（无冒号） | 以字母开头 ⇒ 标识符 ⇒ 报 unknown identifier | 不会被切成减法 |

失败方式是刻意选的：减号漏空格得到「引用不存在」，一眼能改；反过来若把
`line-a:power-1` 切成减法，配置校验会一路绿灯，算出一个**看起来正常**的错误数字。

#### 3.11.4 类型规则（严格）

表达式引擎不持有符号表：引用的静态类型**恒为 number**（布尔点位在算术语境里
是 1/0）。因此：

| 规则 | 例 | 违反时 |
|---|---|---|
| 布尔点位必须接**布尔表达式** | `"line-a:power > 80"` ✅ / `"line-a:power"` ❌ | 配置期报 `dataType is bool but the expression is number` |
| 数值点位必须接数值表达式 | `"1 < 2"` 配 `float64` ❌ | 配置期报 `dataType is float64 but the expression is bool` |
| 拷贝布尔点要显式比较 | `"line-a:running != 0"` | 直接写 `"line-a:running"` 会被上一条拒绝 |
| 布尔**字面量**不参与算术 | `"true + true"` ❌；`"m:running * 100"` ✅ | 编译期报 `operator + needs numbers, got bool and bool` |

为什么不给引擎加一套「点位 → 布尔」类型环境：那需要 config 与 driver 两级各维护
一份类型推断，凡是两边可能漂移的地方，最终都会漂移。现在只有一条规则——**引用
是数，字面量才有类型**——它简单到不会写错，且失败发生在配置期而非运行期。

#### 3.11.5 求值顺序与依赖

计算点之间可以互相引用（`cost_with_tax = cost × 1.13`）。规则按作用域分两种：

| 作用域 | 语义 | 为什么 |
|---|---|---|
| **同设备** | 同一次 `Read` 内按**拓扑序**求值，被依赖者先算 | 用户看到的一批点位必须自洽：`cost_with_tax / cost` 恒等于 1.13，而不是差一轮的比值 |
| **跨设备** | 最终一致，下一轮生效 | 设备之间没有全局时序。强行同步会让一台慢设备拖垮整张依赖图 |

实现上有两个容易踩空的点：

1. **`planner` 必须把所有计算点合进同一个 span**（`ID: "expr"`）。分成多个 span 后，
   驱动虽然仍能按编译好的依赖图自行排序，但「一次 Read = 一组自洽求值」这件事在
   计划里就看不见了。更要紧的是：计算点若落进 `planSymbols` 的默认分支会**静默消失**，
   表现为「配置里有 3 个计算点，API 里 3 个坏点，日志里什么都没有」——采集器只会
   把「驱动没返回的点位」降级为 bad。
2. **未被请求但被依赖的计算点也要算。** 全部算一遍的成本可以忽略（纯算术，无 IO），
   换来的是同一次 `Read` 内的值互相自洽。

环（自引用、两点互引、跨设备互引）在**配置期**拒绝，且报错打印完整路径
（`a -> b -> a`）。放到运行时才发现，表现是「有时好有时坏」——环上的点位按 map
顺序求值，这是现场最难定位的一类问题。

#### 3.11.6 质量传播

| 输入情况 | 输出质量 | 输出值 | 说明 |
|---|---|---|---|
| 全部 good | good | 计算结果 | — |
| 有 uncertain（写入后未回读确认） | uncertain | 计算结果 | 算式没问题，不代表输入已确认 |
| 有不可用（stale / timeout / comm_error / …） | **最差的那一个** | **空** | 不计算 |
| 求值失败（除零、溢出、越界） | bad | 空 | 错误信息带表达式原文 |
| 类型不符（输入不是数值） | bad | 空 | 指名是哪个输入 |

三条硬规则：

1. **绝不用 0 代替缺失输入。** 那会让「A 没数据」变成「合计 = B」——一个看起来完全
   正常的错误数字，比一个明显的空洞危险得多。
2. **质量取最差值，不是平均值。** timeout 与 bad 在排障时指向完全不同的动作，
   不能被「多数输入是好的」稀释。
3. **错误信息必须指名输入**（`input dead:x: input quality is comm_error`）。只说
   「计算失败」等于让运维去逐个点位猜。

实现顺序上有个细节：先判输入状态，再判求值错误。输入不可用时求值必然报
「引用没有值」，那是**结果**不是原因；先报它会把一个 timeout 说成 bad。

#### 3.11.7 结果编码

| 目标类型 | 规则 |
|---|---|
| `float64` | 原样；NaN / ±Inf 判 bad |
| `float32` | 转 `float32`；溢出为 Inf 判 bad |
| 整数类型 | **四舍五入**后做范围检查；越界判 bad，**不截断** |
| `bool` | 非零即真 |

整数做范围检查而不是截断，理由与 §3.10.4 相同：`70000` 写进 `uint16` 变成 `4464`，
会让操作员按一个错的数字决策。四舍五入而不是向下取整，是因为配置声明的是整数
类型，用户要的是**最近的**整数。

#### 3.11.8 只读注入：`OpenConfig.Lookup`

驱动需要读别的点位，但**不能**持有 cache：

```go
type OpenConfig struct {
    // ...
    Lookup func(device, point string) (model.Sample, bool)
}
```

`Lookup` 由 `runtime` 装配期注入 `cache.Get`。它是一个**只读**入口，拿不到写入与
订阅能力，因此驱动不可能绕过采集层的去重、死区、序号与代次四道治理。为 nil 时
（单测、极简装配）驱动必须按「没有值」处理，**不能 panic**。

#### 3.11.9 边界

| 项 | 状态 | 说明 |
|---|---|---|
| 只读 | ✅（且强制） | 计算点没有可写回的地址 |
| 数值与布尔 | ✅ | 字符串 / Bytes / BCD 拒绝 |
| 同设备拓扑求值 | ✅ | 一次 `Read` 内闭合 |
| 跨设备引用 | ✅（最终一致） | 慢一拍，文档明示 |
| 环检测 | ✅ | 配置期拒绝，含跨设备 |
| 历史窗口聚合（`avg(近 5 分钟)`） | ❌ | 需要历史库，属 Wave 2 |
| 字符串拼接 / 格式化 | ❌ | 需要另一套类型系统；用北向侧模板更合适 |
| 写回物理点位（`写 x := 公式`） | ❌ | 这是控制逻辑，不是采集；误用会绕过联锁 |

---

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
| `opcua` | supported | supported | supported ⁴ | unsupported | experimental | supported | unsupported |
| `mqtt`（南向订阅） | supported | supported | supported ¹ | unsupported | unsupported | supported ² | unsupported |
| `computed`（计算点） | supported | unsupported ³ | unsupported | unsupported | unsupported | supported | unsupported |
| `virtual` | supported | supported | supported | supported | unsupported | supported | supported |

> ¹ `mqtt` 的 `Subscribe` 指「协议是推送型、由驱动内部订阅」，**不是**驱动对外提供推送回调：
> 上层拿到的仍然是 `Read` 拉取契约，推送在同包内被消化成最新值表（见 §3.10.1）。
> 声明 `unsupported` 会让配置界面建议用户换协议，方向就错了。
>
> ² MQTT 的批量读没有协议交互——一次 `Read` 取任意多点位都是查内存表，
> 成本与点位数量无关。因此 `MultiRead` 声明 `supported`，但语义与 Modbus 的
> 「一个请求读多地址」不同。
>
> ³ 计算点的 `Write` 声明 `unsupported` 而不是 `runtime_dependent`：这不是
> 「取决于型号」，而是**语义上不存在**——没有可写回的地址（见 §3.11.9）。
>
> ⁴ OPC UA 的 `Subscribe` 同样是指「服务端推送、驱动内部消化、上层仍是
> `Read` 拉取契约」，并且需要通道上显式打开 `subscribe`。之所以不做成
> 自动切换：轮询与订阅的失败模式、带宽特征、服务端负载完全不同，自动切换
> 会让同一份配置在两种行为之间漂移，现场无法判断「数据变慢了」的原因。

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
