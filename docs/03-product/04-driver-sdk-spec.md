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

### 3.6.3 共享串口层（`internal/transport/serialport`）

§3.6.1 的六条约束如果在每个驱动里各实现一遍，第一个被漏掉的必然是 S5（事务前排空）
或 S6（总线级互斥）——因为它们在单设备联调时完全看不出来。所以把「打开/复用/加锁/
排空/设截止时间」抽成一个独立小层：

| 接口 | 语义 |
|---|---|
| `Acquire(proto, cfg, opener) (*Ref, error)` | 按设备路径引用计数复用；**同一路径不同参数直接报错**，不静默复用 |
| `Ref.Lock()` / `Ref.Unlock()` | 整条总线级互斥（不是单设备），覆盖「排空 → 发送 → 收完帧」全过程 |
| `Ref.Drain()` | `tcflush(TCIFLUSH)`；调用后截止时间会被压到现在，**必须在 `SetDeadline` 之前调用** |
| `Ref.SetDeadline(t)` | 单次事务的读/写截止时间 |
| `Ref.ReadFull(n)` / `Write(p)` | 满足 `transport.Stream`，可直接交给编解码层 |
| `Count()` | 当前注册的物理端口数（诊断/测试用） |

`proto` 参数只用于错误信息（「modbus-rtu 与 dlt645 对 /dev/ttyUSB0 的参数不一致」比
「参数冲突」有用得多）。参数冲突的判定是逐字段比较（波特率/数据位/校验/停止位），
任一不同即拒绝，并**保留原使用者**——先到者优先，避免一个配错的通道把正在工作的
通道踢掉。

当前消费者：`modbus-rtu`、`modbus-ascii`、`dlt645`、`cjt188`。IEC 101 落地时应直接
复用，不再另写一套。

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
| S7 | Setup Communication、Read Var、Write Var；USER_DATA 块目录/属性；Start/Upload/End 块上传 | 块下载、报警订阅、符号表浏览、时间同步 |

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

#### 3.7.7 块服务（目录 / 属性 / 上传）

块服务不属于周期采集：它发生在组态、备份、比对与排障时，频率低、数据量大，
因此不进入调度器。实现分两层：`codec/s7` 只做纯编解码，`driver/s7.Session`
负责一次性连接、握手、分片循环与会话释放。

| 动作 | 协议 | 应答 |
|---|---|---|
| 列全部块数量 | USER_DATA，组 0x03，子功能 0x01 | 固定 28 字节 = 7 × (0x30, 类型, 数量) |
| 列某类型块号 | 子功能 0x02，负载 `30 <type>` | 每项 4 字节，前 2 字节是块号 |
| 读块属性 | 子功能 0x03，负载 `30 <type> NNNNN A` | 78 字节（块属性结构） |
| 上传块 | Job 0x1D → 0x1E × N → 0x1F | Start 给会话 id + 5 位 ASCII 长度；Upload 给分片 |

三条最容易写错的协议细节：

| 细节 | 正确做法 | 写错的后果 |
|---|---|---|
| ACK / ACK_DATA 的头长度 | 12 字节（多 error class/code）；Job/UserData 才是 10 | 参数区整体错位两字节，单点读写「看起来能解」，块服务直接解不出 |
| ListAll 的数据区 | `0A 00 0000`（返回码 + 传输尺寸 + 长度） | 照抄 `FF 09` 会被 PLC 静默忽略 |
| 块长度 | 5 位十进制 ASCII，不是二进制 | 把 `"00123"` 当 123 的整数编码会读出天文数字 |

上传是**有状态的三段式**：Start 之后 PLC 保留一个上传会话，必须用 End 释放。
因此 `UploadBlock` 即使在分片阶段失败，也会用独立的短超时补一条 End；不清理
会让反复重试逐渐耗尽 PLC 的上传会话，而错误信息只会显示「start failed」。
分片是否结束只看 `param[1]`：`0` 是最后一片，`1` 表示后面还有——把 1 当成
最后一片不会报错，只会让备份**静默少半截**。

返回的是 PLC 的原始字节流，包含块头（真实 CPU 的第一片前有 36 字节紧凑块头）。
不替调用方剥掉，是因为剥掉后这份数据就不能再用于下载；需要裸内容时由调用方
按自己的格式处理。防御性上限为 16MB / 65536 片，避免一条坏响应把内存吃满。

CLI 自检（不需要起网关，直接连 PLC 或内置从站）：

```bash
uic s7blocks -addr 127.0.0.1:15102                       # 七类块数量
uic s7blocks -addr 127.0.0.1:15102 -type db              # DB 块号列表
uic s7blocks -addr 127.0.0.1:15102 -type db -num 1       # DB1 属性
uic s7upload -addr 127.0.0.1:15102 -type db -num 1 -out db1.bin
uic s7upload ... -out -                                  # 输出到标准输出（可管道）
```

#### 3.7.8 边界

| 项 | 状态 | 说明 |
|---|---|---|
| 订阅 / 报警 | ❌ | S7 的报警需要建立独立的 PDU 通道；不实现 |
| 块目录 / 属性 / 上传 | ✅ | USER_DATA 组 0x03 + Start/Upload/End；见 §3.7.7 |
| 块下载（写回 PLC） | ❌ | 会直接改程序/数据块，风险等级与读完全不同；需要单独的权限与确认流程 |
| 符号表 | ❌ | 需要 S7 的浏览服务；点位必须手写地址 |
| 设备发现 | ❌ | S7 无标准发现协议 |
| 时间同步 | ❌ | 不在采集链路的关键路径上 |
| TLS | 不适用 | S7 本身无 TLS；需要加密走 VPN/隧道 |
| 多连接 | ⚠️ | 多数 CPU 允许的连接数很少；`concurrency` 默认 1 |

内置从站（`internal/sim/s7`）实现连接、Setup、Read Var、Write Var、块目录/
属性与块上传，用于自测与演示。它与主站共用 `codec/s7`，所以「长度单位」「头
长度」这类语义不会在主从之间分叉——这是本项目对「自测有效性」的最低要求：
测试夹具不能是另一套实现。

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

### 3.10.5 三菱 MC（3E 二进制帧，TCP）

三菱的 MC 协议是 PLC 世界与 Modbus/S7 并列的第三极。本驱动只实现 **3E 二进制帧 + TCP**
（线上端口 5000），覆盖 D/W/R/ZR 等字软元件与 M/X/Y/B/F/L 等位软元件；ASCII 帧、4E 帧、
SLMP 的 UDP 形态一律不支持，配置期就拒绝。

**帧结构**（请求，所有多字节字段小端）：

```
50 00 | 网络号(00) | PC号(FF) | 目标模块IO(2) | 目标站号(00) | 数据长度(2) |
监视定时器(2) | 命令(2) | 子命令(2) | 设备码(1) | 起始设备号(3) | 点数(2) | [数据]
```

响应把「命令之后」换成「结束代码(2) + 数据」。

| 字段 | 说明 | 易错点 |
|---|---|---|
| 子命令 | `0x0000` 字读写 / `0x0001` 位读写 | 位/字用子命令区分，不是用设备码 |
| 起始设备号 | **3 字节小端** | 不是 4 字节，也不是大端 |
| 设备码 | `0xA8`=D、`0xB4`=W、`0x90`=M、`0x9C`=X… | 一张固定表，未收录区在配置期报错 |
| 结束代码 | 非零即错误 | `0xC052/53/54/56` → `address_error`，其余协议错 |

**三个最容易写错的细节**（已固化在 `codec/mc`）：

1. **位软元件按位打包**：首点在第一个字节的 bit0，一点一字节是错的。
2. **X/Y/B/W 的编号按十六进制理解**：`X1F` 在线上是数值 31，配置里 `offset=31`。
3. **字软元件上的 bool 点位必须显式给 `address.bit`（0..15）**，驱动做「读整字 → 只改
   目标位 → 写整字」的 RMW，绝不清零邻位。没有位号的 bool 在字软元件上判地址错。

**字序**：MC 原生是「字内小端、低字在前」。驱动在解码时先把每个字的两个字节对调
（归一化到「字内大端」），再按用户配置的 `byteOrder` 走 `value.Decode`；默认 `CDAB`。
字符串/字节类型按地址宽度补零或截断，再按字对调。

**位置**：

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/mc`（`BuildRead`/`BuildWrite`/`ParseResponse`/`PackBits`/`ByCode`） |
| 主站驱动 | `internal/driver/mc`（`Meta`/`Open`/`Read`/`Write`/`Probe`） |
| 内置从站 | `internal/sim/mc`（`NewServer`/`SetWord`/`SetBit`） |
| CLI | `uic mcread -addr 127.0.0.1:5000 -area d -offset 100 -qty 2` |
| 示例 | `examples/mc.json`（D0/D1 整数、D2 float32 CDAB、M0/M1 位） |

自检链路（无硬件）：`./uic sim -protocol mc -listen 127.0.0.1:5000 &` 起内置从站，
`./uic -config examples/mc.json` 起网关，`curl :8087/api/v1/points` 应看到 d0=1234、
d1=5678、d2=48.0、run=true、fault=false。`uic mcread` 不依赖网关即可单独读写 PLC。

### 3.10.6 欧姆龙 FINS（FINS/TCP，端口 9600）

FINS（Factory Interface Network Service）是欧姆龙的现场网络协议族。本驱动只实现
**FINS/TCP**（以太网单元，端口 9600），覆盖 CIO/WR/HR/AR/DM/EM 内存区的字/位读写；
**FINS/UDP、Host Link（串口）、FINS 的板卡/网络路由形态一律不支持**，配置期就拒绝
（`transport.kind` 必须是 `tcp`）。

只做内存区读写（`0101`/`0102`）是刻意划的边界：PLC 运行模式切换、时钟读写、强制
置位/复位、文件读写都不放进采集驱动。它们不是「读点位」的一部分，而误用的代价
（停机、写坏程序区）远高于收益。需要这些操作的现场用厂商工具。

#### 3.10.6.1 三层封装，缺一不可

FINS 的帧是「套娃」结构，三层各自有独立的长度/命令/错误语义：

```
┌─ FINS/TCP 头（16 字节）─────────────────────────────┐
│ "FINS" | 长度(4, 大端) | 命令(4) | 错误码(4)          │
├─ FINS 帧头（10 字节）───────────────────────────────┤
│ ICF RSV GCT DNA DA1 DA2 SNA SA1 SA2 SID             │
├─ FINS 命令（2 字节 + 参数/数据）────────────────────┤
│ MRC SRC | 参数区 [| 写数据]                          │
└─────────────────────────────────────────────────────┘
```

| 层 | 长度语义 | 易错点 |
|---|---|---|
| FINS/TCP | **不含 magic 与长度字段自身**：`命令+错误码+负载` | 按「整包长度」填会让对端多等 8 字节，表现为首次读写超时；反过来少填则会吞掉下一帧的头，第二帧才错位 |
| FINS 帧 | 由外层长度隐式给出，帧内无长度字段 | 只能靠外层长度切帧；没有「读到 0 就结束」这种约定 |
| 结束码 | 响应在 `MRC SRC` 之后跟 2 字节结束码 | 请求里没有这两个字节；`Data` 必须从第 14 字节起算 |

FINS/TCP 的三个命令：

| 命令 | 方向 | 长度字段 | 负载 |
|---|---|---|---|
| `0` 节点号请求 | 客户端 → PLC | `12` | 客户端节点号(4)，0 = 请 PLC 分配 |
| `1` 节点号响应 | PLC → 客户端 | `16` | 客户端节点(4) + 服务端节点(4) |
| `2` FINS 帧 | 双向 | `8 + len(FINS 帧)` | 完整 FINS 帧 |

握手失败/节点冲突时 PLC 回**只有 8 字节长度**的命令 1（无节点号），错误码放在第
12..15 字节；错误码 `0x24` 表示「客户端节点号与服务端相同」。驱动与内置从站都按
这个最短形态构造/解析，避免把错误响应当成正常响应去读节点号。

#### 3.10.6.2 节点号协商（握手）

真实 PLC 的每一帧都要填「目标节点」和「源节点」，而这两个值只能从握手拿到：

1. 客户端发命令 0，`clientNode=0`（自动分配）；
2. PLC 回命令 1，负载第 4 字节是分配给我的节点号，第 8 字节是它自己的节点号；
3. 之后每一帧：`DA1 = PLC 节点`，`SA1 = 本机节点`。

驱动的默认行为是「目标 = 握手返回的 PLC 节点」，`device.unitId > 0` 时覆盖它
（同一台 PLC 上的多块 CPU / 跨网段时用）。`unitId` 上限 254：0 是「未指定」，
255 是广播，都不是合法的单播目标。

节点号协商失败的分类是**可重试的连接错**而不是协议错：端口刚起来、PLC 还在初始化、
同一 IP 上一个进程刚断开但节点未释放，都会得到 `0x20`（连接数满）/`0x21`（节点已被
占用）。把它们当协议错会让设备永久停在 `failed`，必须靠人工介入。

#### 3.10.6.3 地址语义：`offset` 是字地址，`bit` 是字内位号

这是与三菱 MC 最大的差异。MC 的位软元件有独立编号空间（`M0`、`X1F`），FINS 的位是
**字地址 + 位号**：`CIO 0.3` 表示「CIO 第 0 个字的第 3 位」。因此配置里：

```json
{ "id": "run",   "address": { "kind": "register", "area": "cio", "offset": 0, "bit": 0 }, "dataType": "bool" }
{ "id": "dm100", "address": { "kind": "register", "area": "dm",  "offset": 100, "length": 1, "bit": -1 }, "dataType": "uint16" }
```

| 规则 | 违反时的行为 |
|---|---|
| `address.offset` 必填，范围 0..65535，表示**字**地址 | 超出范围在构造帧时判 `address_error` |
| 位点位必须给 `address.bit`（0..15，0 = LSB） | 该区支持位访问时报地址错；只有字访问码的区退化为 RMW（见下） |
| 字点位用 `address.bit = -1` 表示「整字」 | 与模型层的其余寄存器协议一致 |
| `length` 表示**字数**，不是字节数 | 多字类型（int32/float32/float64）按类型宽度推导，字符串/字节必须显式给 |

**位访问与字访问用不同的内存区码**，这是第二个易错点：

| 区 | 字访问码 | 位访问码 | 说明 |
|---|---|---|---|
| CIO | `0xB0` | `0x30` | 继电器区，最常用于状态位 |
| WR | `0xB1` | `0x31` | 工作区 |
| HR | `0xB2` | `0x32` | 保持继电器 |
| AR | `0xB3` | `0x33` | 辅助继电器 |
| DM | `0x82` | `0x02` | 数据存储器，最常用于参数与配方 |
| EM（当前区） | `0x98` | — | 只有字访问码（见下） |
| EM bank 0..12 | `0xA0..0xAC` | `0x20..0x2C` | 扩展存储器 |

配置里的区名接受 `d`/`w`/`h`/`a` 简写（映射到 `dm`/`wr`/`hr`/`ar`），大小写不敏感；
`Areas()` 返回固定顺序供表单与文档使用，`ByWordCode`/`ByBitCode`/`BitToWordCode`
供内置从站反查。主站与从站共用同一张表，避免两边对同一个区名的理解漂移。

**当前 EM 区的位访问码是刻意留空的**：不同机型/手册对 `0x98` 对应的位访问码表述
不一（有的写 `0x18`，有的要求先切 bank），猜错会读到别的区的数据——这比报错危险
得多。因此 `em` 上的 bool 走「读整字 → 只改目标位 → 写整字」的 RMW；要按位直接
访问就用带 bank 的区名（`em0`..`em12`）。

#### 3.10.6.4 命令参数与数据宽度

读与写的**命令码都是 `MRC=0x01`，靠 `SRC` 区分动作**（`0x01` 读 / `0x02` 写）：
看起来像「主命令相同、子命令不同」，但和 MC 的「子命令表示字/位」正好相反——这里
字/位由**区码**决定，子命令表示读写。写错会让 PLC 把读当写执行。

参数区统一 6 字节：

```
区码(1) | 起始字地址(2, 大端) | 位号(1) | 点数(2, 大端) [| 写数据]
```

| 访问方式 | 点数单位 | 写数据宽度 | 读响应宽度 |
|---|---|---|---|
| 字访问 | 字 | `点数 × 2` 字节，字内大端 | `点数 × 2` 字节 |
| 位访问 | 位 | `点数 × 1` 字节，每字节 0/1 | `点数 × 1` 字节，每字节 0/1 |

位访问「一位一字节」是三菱（按位打包）与 S7（按位在字节内）之外的第三种约定，
必须按协议实现，不能想当然地复用前面的打包函数。

上限：字访问 `≤ 999` 字，位访问 `≤ 1998` 位（`MaxWords`/`MaxBits`），
整帧负载 `≤ 2012` 字节（`MaxPayload`）。驱动按更保守的块切分：字读 `480 字/块`、
位读 `120 字（=1920 位）/块`，并把跨度覆盖到的整段一起读，避免同一次采集里对
同一段地址发多条请求。

#### 3.10.6.5 字序：低字在前

FINS 的多字数值是「每个字大端、**低字在前**」：

| 类型 | 线上字节 | 解码用的 `byteOrder` |
|---|---|---|
| `uint16`/`int16` | 1 字，大端 | 无关 |
| `uint32`/`int32`/`float32` | 低字在前（`0x0000 0x4240` = 48.0） | `CDAB`（默认） |
| `float64`/`int64` | 整体按字倒序 | `DCBA`（默认） |

用户显式配置 `byteOrder` 时以配置为准（有些设备做了字交换）。字符串/字节不做字内
交换：`"AB"` 在 DM 里就是 `0x4142`，按地址宽度补零或截断。

#### 3.10.6.6 结束码与质量映射

响应在 `MRC SRC` 后跟 2 字节结束码，`0x0000` 才是成功：

| 结束码 | 含义 | 映射质量 |
|---|---|---|
| `0x1101` | 区类型不匹配（区码不认识） | `address_error` |
| `0x1102` | 地址范围超限（点数过多） | `address_error` |
| `0x1103` | 地址越界 | `address_error` |
| `0x1104` | 地址写保护 | `address_error` |
| `0x0201`..`0x0205` | 本节点不在网 / 令牌超时 / 重试超限 / 路由表错 | `comm_error` |
| `0x0301`/`0x0302` | 目标节点不在网 / 单元缺失 | `comm_error` |
| `0x1001`/`0x1002` | 命令过长 / 过短 | `bad`（协议错） |
| `0x0401` | 未定义命令 | `bad`（协议错） |
| 其余 | 厂商扩展 | `bad` |

TCP 封装层错误码（`0x01` 非 FINS 头、`0x02` 长度过长、`0x03` 不支持的命令、
`0x20` 连接数满、`0x21` 节点已占用……）一律映射 `comm_error`，且**可重试**。

此外驱动会校验响应帧的 `SID` 与发送时一致：`SID` 不匹配说明收到的不是本次事务的
应答（异步/串包/旧连接残留），必须判协议错，绝不能把上一帧的数据当成本帧结果。

#### 3.10.6.7 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/fins`（`Build`/`BuildCommand`/`Parse`/`BuildMemoryRead`/`BuildMemoryWrite`/`Lookup`/`EndCodeError`） |
| 主站驱动 | `internal/driver/fins`（`Meta`/`Open`/`Read`/`Write`/`Probe`，内部 `framing` 负责封装与 SID） |
| 内置从站 | `internal/sim/fins`（`NewServer`/`SetWord`/`SetBit`/`GetWord`） |
| CLI | `uic finsread -addr 127.0.0.1:9600 -area dm -offset 0 -qty 2` |
| 示例 | `examples/fins.json`（DM0/DM1 整数、DM2 float32 CDAB、CIO0.0/0.1 位、DM10 自增计数器） |

内置从站的位/字视图**共用一张字内存表**：写 `CIO0.1` 后读 `CIO0` 字能看到第 1 位
变化，读 `CIO11` 字也不会被 `CIO10` 的位写污染。内存模型是「未配置地址读 0、写落表」，
与真实 PLC 读未定义寄存器得 0 的行为一致；越界返回 `0x1103`。

自检链路（无硬件）：

```
./uic sim -protocol fins -listen 127.0.0.1:9600 &
./uic -config examples/fins.json &
curl -H 'Authorization: Bearer dev-token-0123456789' \
     http://127.0.0.1:8088/api/v1/points
# dm0=1234  dm1=5678  temp=48  run=true  fault=false  counter=每秒自增
```

`uic finsread` 不依赖网关即可单独验证「能连上」和「能读到数」两件事：

```
./uic finsread -addr 127.0.0.1:9600 -area dm -offset 0 -qty 4      # 字读
./uic finsread -addr 127.0.0.1:9600 -area cio -offset 0 -bit 0     # 位读
./uic finsread -addr 127.0.0.1:9600 -area cio -offset 0 -bit 1 -write on   # 位写
./uic finsread -addr 127.0.0.1:9600 -area dm -offset 20 -write 4660        # 字写
```

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/fins` | 节点请求/响应金帧、字/位读命令金帧、响应解析、结束码分类、区表与码反查、位=0 的写、TCP 封装、边界与上限 |
| `driver/fins` | 握手、字读、float32 低字在前、位读、位写、`em` RMW、结束码→质量、SID 不匹配、非 TCP 拒绝、能力声明 |
| `sim/fins` | 真实 TCP 回环：字读写、float32、位读写共享字内存、字/位混合 span、字符串、越界地址、`unitId` 覆盖 |

### 3.10.7 EtherNet/IP（CIP 显式报文，TCP 44818）

EtherNet/IP 是标准组织定义的工业以太网协议族，CIP（Common Industrial Protocol）是它的
应用层。本驱动只实现 **CIP 显式报文 + TCP 44818 + 符号标签访问**：

- Read Tag（0x4C）/ Write Tag（0x4D）；
- Multiple Service Packet（0x0A）把一轮采集里的多条读合并成一个报文；
- STRING（4 字节长度 + 字符容量）、原子数值类型、BOOL、整数标签里的位；
- 设备不支持多服务合包时自动退回逐条请求，并记住结论。

**不做**的事情同样重要：不做隐式 I/O（UDP 2222 的 Class 1 连接）、不做对象/属性寻址
（Identity/Message Router 对象只用于 Probe 与合包路径）、不做结构体/数组的自动展开。
理由见 §3.10.7.8。

#### 3.10.7.1 三层封装，每层的「长度」含义都不同

```
┌─ 封装头（24 字节，全部小端）──────────────────────────┐
│ 命令(2) 长度(2) 会话句柄(4) 状态(4) 发送方上下文(8) 选项(4) │
├─ CPF（通用报文格式）──────────────────────────────────┤
│ 接口句柄(4)=0 超时(2)=0 条目数(2)=2 + 条目...            │
│   条目 1：类型 0x0000（空地址），长度 0                   │
│   条目 2：类型 0x00B2（未连接数据），长度 N + CIP 报文      │
├─ CIP 报文───────────────────────────────────────────┤
│ 服务(1) 路径字数(1) 路径(2N) 请求数据                   │
└──────────────────────────────────────────────────────┘
```

三个易错点，都已固化在 `codec/enip`：

| 细节 | 正确做法 | 写错的后果 |
|---|---|---|
| 封装头 `length` | **只算负载**，不含 24 字节头 | 对端一直等数据，表现为超时 |
| CPF 条目对齐 | 奇数长度条目后补 1 个零字节 | 后一条目从奇地址开始，类型/长度全错 |
| 发送方上下文 | 8 字节原样回显，用于配对请求与应答 | 交错时把上一条应答当成这一条的 |

`roundTrip` 对这个上下文做**严格校验**，只放行全零：少数嵌入式协议栈确实不实现该字段，
为此拒绝会让设备完全不可用。非零且不匹配一律判协议错，而不是继续解析。

#### 3.10.7.2 会话与事务

建链顺序是：TCP 连接 → RegisterSession（0x65）→ 后续 SendRRData（0x6F）→ 关闭时
UnRegisterSession（0x66，无应答）。

- **协议版本必须是 1，选项必须是 0**：其它值说明对端不是我们理解的 EtherNet/IP 封装，
  继续往下走只会把错误藏到更深处；
- 会话句柄由设备分配，返回 0 视为失败（0 是「无会话」的保留值）；
- CIP 显式报文**没有事务号**，靠一问一答定界。因此 `conn.mu` 串行化整条会话，
  两个请求交错会让响应对不上请求，这类错误在现场极难定位；
- 关闭前尽力发 UnRegisterSession，失败不影响关闭。不发会让设备侧的会话表
  短暂堆积（部分 PLC 的会话数上限很低）。

#### 3.10.7.3 地址语义：标签名 + 元素偏移

CIP 的 Read Tag 请求**没有偏移参数**，只有一个「元素个数」。因此地址复用
`model.AddressRegister`，但字段含义按 CIP 重新解释：

| 字段 | 含义 |
|---|---|
| `address.area` | 标签名（大小写不敏感）。程序域标签写 `Program:Main.Tag` |
| `address.offset` | 数组的起始元素下标（0 起） |
| `address.length` | `bytes` 是字节数；`string` 是字符容量；数值/bool 固定 1 |
| `address.bit` | `-1` 表示标签本身是 BOOL；`0..63` 表示从整数标签取一位（写走 RMW） |

**为什么不用 `PointRef.Length` 当元素数**：它按「16 位寄存器数」推导，int32 会得到 2，
而 CIP 的 int32 是 1 个元素。直接把 2 传给 Read Tag 会读到两个元素，数据长度翻倍，
解码取第一个元素时看似正确，直到某天标签是数组才发现错位。驱动因此只认
`address.length`。

`offset > 0` 的读做法是「读 0..offset 的 `offset+1` 个元素，再切片」；写做法是
「读整段 → 改目标元素 → 写整段」，保留前缀元素。CIP 没有「从第 N 个元素开始写」的
参数，直接写 count=1 会把元素 0 覆盖掉。

#### 3.10.7.4 类型、字节序与 STRING

CIP 数值在线上是**小端**，而平台 `value` 包约定大端。驱动对每个元素先整体反转，
再走 `value.Decode(..., OrderABCD)`；写入反向。这样「CIP 的小端」与「Modbus/S7 的字序
配置」共享同一套解码语义，不会出现两份符号扩展实现。

| 平台类型 | CIP 类型码 | 宽度 |
|---|---|---|
| `bool` | BOOL 0xC1 | 1 字节/元素 |
| `int16` / `uint16` | INT 0xC3 / UINT 0xC7 | 2 |
| `int32` / `uint32` | DINT 0xC4 / UDINT 0xC8 | 4 |
| `int64` / `uint64` | LINT 0xC5 / ULINT 0xC9 | 8 |
| `float32` / `float64` | REAL 0xCA / LREAL 0xCB | 4 / 8 |
| `string` | STRING 0xD0 | 4 字节长度 + 字符容量 |
| `bytes` | 由设备实际类型决定 | 见下 |

**类型兼容**：同宽的有符号/无符号可以互换（现场把 DINT 标签配成 uint32 很常见），
浮点与非浮点、不同宽度一律判地址错。驱动从应答里读**实际类型**再与配置比对，
而不是盲信配置——否则 REAL 标签配成 int32 会把位模式当整数返回，值是假的但质量是 good。

**STRING** 是常见的 PLC 结构：4 字节小端长度 + 字符 + 补零。容量必须显式给（默认 82）。
读时按长度截断并去掉尾部 NUL；写时超长截断，长度字段跟着截断。

**bytes** 是唯一需要两阶段读的类型：先读 `offset+1` 个元素学实际类型与宽度，再按
`offset + ceil(length/width)` 重读并切片。原因很简单：CIP 的类型信息只在应答里，
配置里没有；而 `bytes` 点位的意义就是「不关心它是什么类型，只要这段原始字节」。
写入同理：先读覆盖区间并保留邻字节，再按实际类型写回。

#### 3.10.7.5 多服务合包与自动退回

一轮采集里每条标签一次往返，在 20 个点位、10ms RTT 的链路上就是 200ms。Multiple
Service Packet 把这些读合成一个报文：路径固定指向 Message Router
（class 0x02 / instance 0x01），请求数据是「条数 + 每条相对偏移 + 嵌入报文」。

驱动的策略：

- 单次合包的**保守上限是 450 字节**（嵌入区 + 偏移表），超过就拆批；
- 单次读/写的数据量**保守上限 400 字节**：CIP 连接的标准报文尺寸通常 504，
  超过后设备回 0x06（partial transfer）。与其让设备报错，不如在驱动侧明确拒绝
  并提示「缩小 length」；
- 设备回 0x08/0x14（服务不支持 / 属性不支持）时**关掉合包并退回逐条请求**，
  本批立刻重发；一次不支持之后不再尝试，避免每轮采集都先失败一次；
- 嵌入应答按条目解析：某一条错误只让那个点位变 bad，不牵连整批。

#### 3.10.7.6 位访问：读元素 → 改一位 → 写回

`address.bit >= 0` 表示「从整数标签里取一位」。读：读 `offset+1` 个元素，按实际类型
宽度定位元素，取第 `bit` 位（0..width*8-1）。写：读整元素、改一位、写回**整个元素**。

为什么不是「读整标签 → 改 → 写整标签」：写整个数组会把其它元素也覆盖，哪怕值没变，
也会在 PLC 侧产生一次无谓的写入（部分 PLC 会因此触发变更事件/记录）。只回写目标元素
是能做到的最小写。

`bit` 超出元素宽度判地址错，**不会**悄悄读到下一个元素——那是最难排查的一类错：
配置写错一位，读到的却是邻位的值，而且看起来完全合理。

#### 3.10.7.7 错误与质量映射

CIP 通用状态在 `codec/enip` 里分类，驱动只做「错误 → 质量码」的最后一步：

| CIP 状态 | 分类 | 质量码 | 说明 |
|---|---|---|---|
| 0x04 / 0x05 / 0x13 / 0x15 / 0x16 | `ErrAddress` | `address_error` | 路径段错 / 目标不存在 / 数据不够 / 数据过多 / 对象不存在 |
| 0x08 / 0x14 | `ErrUnsupported` | `bad` | 服务/属性不支持 |
| 0x0E / 0x0F | `ErrPermission` | `bad` | 属性只读 / 权限不足 |
| 0x01 / 0x02 / 0x07 / 0x0C / 0x10 | `ErrDisconnected` | `comm_error` | 连接失败/资源不足/连接丢失/状态冲突 |
| 其它 | `ErrProtocol` | `bad` | 协议错 |

封装状态里 **0x64（会话句柄无效）单独归为可重试**：会话过期后重新建链会重新注册，
属于自愈路径；其它状态判协议错，不重试。这条区分很重要——把 0x64 当永久错误会让
一次网络闪断把设备标成坏点直到人工重启；把它当通用重试又会对着配置错误反复重连。

#### 3.10.7.8 能力边界（刻意的）

| 不做 | 理由 |
|---|---|
| 隐式 I/O（UDP 2222，Class 1） | 需要连接参数协商、RPI、组包与看门狗，属于另一套实时模型；与本平台的「采集」语义不同 |
| 对象/属性寻址 | 属性段编码在不同厂商文档里有 0x2C/0x30 两种说法，没有真机验证前不猜——猜错会读到**别的对象**的属性，比报错危险得多 |
| 结构体/数组自动展开 | 需要 Member/Element 段与符号实例 ID 解析；标签名 `.Member` 的编码在不同固件上行为不一致 |
| 符号列表浏览 | 需要配合 FTP/HTTP 或厂商专有服务，CIP 本身不提供统一接口 |

`Probe` 用 Get Attributes All（Identity 对象 class 0x01 / instance 0x01）验证链路：
返回 0x08/0x14 也视为「CIP 层通」——Identity 对象不是必选实现，为此拒绝连接会误伤。

#### 3.10.7.9 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/enip`（`BuildPacket`/`ParseHeader`/`BuildCPF`/`ParseSendRRData`/`BuildRequest`/`BuildReadTag`/`BuildWriteTag`/`BuildMultiple`/`SymbolicPath`/`LogicalPath`/`EncodeString`/`StatusError`） |
| 主站驱动 | `internal/driver/enip`（`Meta`/`Open`/`Read`/`Write`/`Probe`，内部 `readBatch`/`sendMultiple`/`writeBit`） |
| 内置从站 | `internal/sim/enip`（`NewServer`/`SetDINT`/`SetINT`/`SetREAL`/`SetBool`/`SetString`/`SetArray`/`SetMultipleSupported`） |
| CLI | `uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG` |
| 示例 | `examples/enip.json`（DINT/INT/REAL/BOOL/STRING、整数位、自增计数器） |

内置从站的标签表是「类型 + 元素数 + 小端字节」，`Set*` 显式指定类型，避免从站自己的
猜测掩盖主站的类型校验问题。默认标签：`DINT_TAG=1234`、`INT_TAG=5678`、`REAL_TAG=48.0`、
`BOOL_TAG=true`、`STRING_TAG="PUMP-01"`（容量 82）、`DINT_ARRAY=[10,20,30,40]`、
`COUNTER` 每秒自增。

自检链路（无硬件）：

```
./uic sim -protocol enip -listen 127.0.0.1:44818 &
./uic -config examples/enip.json &
curl -H 'Authorization: Bearer dev-token-0123456789' \
     http://127.0.0.1:8089/api/v1/points
# dint=1234  int16=5678  temp=48  run=true  name="PUMP-01"
# fault=DINT_TAG 的 bit1  counter=每秒自增
```

`uic enipread` 不依赖网关即可单独验证「能连上」和「能读到数」：

```
./uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG                  # 数值读
./uic enipread -addr 127.0.0.1:44818 -tag REAL_TAG -type real       # 类型别名
./uic enipread -addr 127.0.0.1:44818 -tag REAL_TAG -type real -write 12.5
./uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG -type bool -bit 1 -write off
./uic enipread -addr 127.0.0.1:44818 -tag DINT_ARRAY -type bytes -length 8
./uic enipread -addr 127.0.0.1:44818 -tag DINT_ARRAY -type bytes -length 4 -write hex:01020304
```

`SINT/USINT` 在 CLI 里映射为 `bytes`（平台没有 1 字节整数类型），必须配 `-length`；
驱动会从应答里学实际宽度，不会把 1 字节当 2 字节。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/enip` | 封装头/长度语义、CPF 奇数对齐与越界、SendRRData、RegisterSession、CIP 请求/应答、附加状态、符号路径偶对齐、逻辑路径 8/16/32 位、Read/Write Tag 金帧、多服务偏移表、类型表、字符串编解码 |
| `driver/enip` | 注册与状态、命令/上下文错位、原子类型、大小写、STRING、bytes 两阶段读、数组 offset、位读/位写保邻位、offset 写保前缀、多服务合包与拆批、MSP 自动退回、错类型/越界位/未知标签、会话失效可重试、Probe 容忍不支持、能力声明 |
| `sim/enip` | 真实 TCP 回环：注册/注销/坏版本/未知命令、Read/Write Tag 与错误码 0x03/0x05/0x13/0x15、STRING 写、多服务与关闭、非符号路径、`SetTag` 校验、默认标签 |

### 3.10.8 BACnet/IP（对象属性读写，UDP 47808）

楼宇自控与厂务系统里，采温湿度、风机启停、阀门开度这类点位，最常见的接口就是
BACnet/IP。它与前面几种协议的根本差别在于**寻址模型**：Modbus 是「寄存器偏移」，
S7 是「区域 + 字节偏移」，CIP 是「标签名」，而 BACnet 是「对象类型 + 实例号 + 属性」。

这决定了三件事：

1. 一个点位不是「一段内存」，而是「某个对象的某个属性」。`analog-input:1` 的
   `present-value` 与 `units` 是两个独立的读请求，取值语义也不同（浮点 vs 枚举）。
2. 可写性由对象类型决定，而不是由平台配置决定。同一份点表里，`analog-input` 的
   `present-value` 永远只读、`analog-value` 的则可写；把它们都当成「可写寄存器」
   会在运行期收到「写访问被拒绝」。
3. 一个对象有几十个标准属性，绝大多数业务点位只关心 `present-value`，
   但「对象名」「工程单位」这类元数据同样值得采——它们让点表在半年后还能被读懂。

本驱动只做两个服务：**ReadProperty（0x0C）** 与 **WriteProperty（0x0F）**。
边界见 §3.10.8.8。

#### 3.10.8.1 四层封装，每层的长度含义都不同

```
┌─ BVLC（4 字节）──────────────────────────────────────┐
│ 类型(1)=0x81  功能(1)  长度(2，只算 NPDU+APDU)          │
├─ NPDU（3 字节起）────────────────────────────────────┤
│ 版本(1)=1  控制(1)=0x04  后续可选的目的/源网络信息        │
├─ APDU───────────────────────────────────────────────┤
│ 类型(1)  参数...（服务码、invoke ID、对象标识符、属性...）  │
├─ 应用值（自带标签的类型编码）─────────────────────────────┤
│ 标签头(1..5) + 负载                                    │
└──────────────────────────────────────────────────────┘
```

| 细节 | 正确做法 | 写错的后果 |
|---|---|---|
| BVLC 长度 | **只算 NPDU + APDU**，不含 BVLC 自己的 4 字节 | 与绝大多数「长度含头」的协议相反；按含头写会让对端丢弃或粘包 |
| NPDU 控制字节 | 请求固定 `0x04`（期望应答、无目的端） | 带上「有目的端」却不给地址，报文结构立即非法 |
| Forwarded-NPDU（0x0B） | 外层多 4 字节转发信息，**跳过**再解析 | 直接按 Original-Unicast 解析会把转发地址当 NPDU 版本 |
| APDU 首字节 | 高 4 位是类型，低 4 位是标志（分段、更多分段等） | 用整字节做 switch 会把「分段 ComplexACK」当成未知类型 |

`ParseMessage` 负责剥掉 BVLC 与 NPDU，返回 APDU 与 NPDU 控制字节；`ParseACK`
再把 APDU 拆成 `(kind, service, invokeID, payload)`。所有上层代码只面对
「应用值字节」，不再关心封装的三种长度字段。

#### 3.10.8.2 事务：8 位 invoke ID 与 UDP 串行化

BACnet/IP 跑在 UDP 上，没有连接、没有请求序号：

- invoke ID 是 **1 字节**（1..255，0 保留给设备主动发起的报文），
  8 位意味着它必然复用；
- 同一个套接字上并行挂两个未完成事务时，迟到的应答会张冠李戴，
  而错误现象是「值偶尔串到别的点位上」——比读不到值难查得多。

因此 `connState.mu` **串行化整条会话**，一个事务完成再发下一个。这不是性能妥协：
一台设备的采集周期通常是秒级，而一次属性读的往返在局域网内是个位数毫秒，
串行化的时间成本远低于错配的排查成本。

```go
apdu := ReadPropertyRequest(invoke, objType, instance, property, index)
resp := exchange(ctx, apdu, invoke, "read")   // 只接受 invoke 相同的应答
```

`exchange` 的规则：

| 情况 | 处理 | 理由 |
|---|---|---|
| 收到 invoke 不匹配的应答 | 丢弃，继续等 | UDP 上迟到报文是常态，报错会误伤 |
| 收到无法解析的报文 | 立即报协议错 | 对端在发垃圾，继续等不会变好 |
| 超时 | 重发，最多 2 次 | UDP 丢包；但重试过多会让一轮采集时延不可控 |
| 断链 / 协议错 | 不重试，立即上报 | 重试只会让同一错误重复 N 次 |
| ctx 已取消 | 报超时并结束 | 上层关闭时不该再等满一个超时 |

默认端口 47808，地址省略端口时自动补齐；显式写端口仍然尊重配置（跨 NAT 或调试）。
UDP 用 connected socket（`DialUDP`），这样内核会过滤掉其它源地址的报文，
减少一类「第三方设备广播包被当成本设备应答」的干扰。

#### 3.10.8.3 地址语义：对象类型 + 实例 + 属性 + 数组下标

BACnet 的点位必须用 `kind=object` 寻址：

| 字段 | 含义 | 校验 |
|---|---|---|
| `address.objectType` | 对象类型名或编号（`analog-input` / `0`） | 必须能解析；错误提示列出全部可选值 |
| `address.instance` | 对象实例号 | `0..4194302`（22 位） |
| `address.property` | 属性名或编号（`present-value` / `85`） | 必须能解析 |
| `address.propIndex` | 数组属性的下标 | `0` 表示不带下标（读整个属性）；`>0` 读该元素 |

**为什么允许写编号**：现场设备文档千差万别，有的只给编号表。让用户为了一个
属性去查别名，等于把文档工作转嫁给运维。解析时**编号优先于别名**，且只接受
纯十进制——`-1` 不会被 `normalizeName` 吃掉减号变成 `1`。这类「安静地指向
另一个对象」的错误，比直接报错危险得多。

对象类型到值语义的映射由 `KindOfObject` 统一：

| 语义 | 对象类型 | present-value 的编码 |
|---|---|---|
| 模拟量 | analog-input / analog-output / analog-value / loop | REAL（4 字节浮点） |
| 二进制量 | binary-input / binary-output / binary-value | ENUMERATED |
| 多状态量 | multi-state-input / multi-state-value / multi-state-output | UNSIGNED |
| 字符串 | character-string | CHARACTER STRING |

这张表是**写值时选标签**的依据（§3.10.8.5），也是 CLI 推断 `-dtype` 的依据。
把它分散到各处写 switch，迟早会出现「驱动按 REAL 写、从站按 ENUMERATED 收」
的自相矛盾。

#### 3.10.8.4 标签编码：application 与 context 的 LVT 含义不同

BACnet 的类型编码是最容易写错的一处：**同一个 LVT 数值，在两种标签里含义不同**。

| LVT | application 标签 | context 标签 |
|---|---|---|
| 0–4 | 负载长度 0..4（内联） | 负载长度 0..4（内联） |
| 5 | 扩展长度，后续 1 字节 | 扩展长度，后续 1 字节 |
| 6 | 扩展长度，后续 2 字节 | **opening tag**（无负载） |
| 7 | 扩展长度，后续 4 字节 | **closing tag**（无负载） |

换句话说：application 标签永远用 LVT 表示长度；context 标签只有 0..5 表示长度，
6/7 被构造形式占用。把 context 的长度 6 当成「扩展 2 字节」会读出两个字节的垃圾，
而报文长度又刚好对得上——错误会一路传到值解码才爆出来。

已验证的编码规则：

| 类型 | 编码 | 备注 |
|---|---|---|
| BOOLEAN | 标签头 LVT **直接承载值**（0/1） | 没有负载字节；当成「长度」写会多出一个字节 |
| UNSIGNED / ENUMERATED | 大端，最短字节数 | 0 也必须占 1 字节（LVT=1） |
| SIGNED | 大端补码，最短字节数 | -128 编码为 `31 80`，不是 `32 80` |
| REAL | 4 字节 IEEE754 大端 | 长度固定 |
| DOUBLE | 8 字节 IEEE754 大端 | 长度固定 |
| OCTET STRING / CHARACTER STRING | 原始字节 | 不自动补结束符；长度即字节数 |
| BIT STRING | 未使用位数(1) + 位串 | 只用于读，写入不支持 |
| OBJECT IDENTIFIER | 4 字节：`type<<22 \| instance` | 请求的对象寻址用它 |

`EncodeApplicationValue` / `DecodeApplicationValue` 是纯函数，不认识「服务」，
只认「类型 + 值」。这样同一套编码既用于请求参数，也用于应答里的属性值，
以及内置从站的回包——三处不会漂移。

#### 3.10.8.5 写值类型由对象决定，不由平台类型决定

这是 BACnet 写入最常见的失败原因。平台点表里写 `dataType=float32`，
如果据此发一个 REAL 标签去写 `binary-value` 的 `present-value`，设备会回
`invalid-data-type`——因为该属性在标准里就是 ENUMERATED。

驱动因此按**属性 + 对象语义**选标签：

| 场景 | 标签 |
|---|---|
| `present-value` + 模拟量对象 | REAL |
| `present-value` + 二进制对象 | ENUMERATED |
| `present-value` + 多状态对象 | UNSIGNED |
| `present-value` + 字符串对象 | CHARACTER STRING |
| 其它属性 + `dataType=bool` | BOOLEAN |
| 其它属性 + `dataType=string` | CHARACTER STRING |
| 其它属性 + `dataType=bytes` | OCTET STRING |
| 其它属性 + `dataType=float64` | DOUBLE |
| 其它属性 + 其它数值 | REAL |

写请求的参数标签顺序（缺一不可）：

```
[0] 对象标识符
[1] 属性标识符
[2] 数组下标（可选，改了数组元素才带）
[3] opening → 应用值 → closing
[4] 优先级 1..16（可选；0 表示不带，按 relinquish-default 处理）
```

`[3]` 必须是构造形式：它的内容是「自带标签的类型」，塞不进 context 标签的原始
负载。这也是为什么写请求比读请求多两个字节的 opening/closing。

读应答里的 `[3] propertyValue` 同样用 opening/closing 包裹；但少数老设备
直接把应用值塞在原始负载里。`ReadPropertyACK` 两种都接受——宽容读、严格写，
是现场兼容性的基本取舍。

#### 3.10.8.6 错误类 / 错误码 → 质量映射

设备的 Error APDU 带 `class` 与 `code` 两个字节。映射不是「一律 bad」，
而是尽量还原「这是配置错、权限错，还是链路问题」：

| class | code | 归为 | 质量码 | 典型含义 |
|---|---|---|---|---|
| 1（object） | 任意 | `address` | `address_error` | 对象实例不存在 |
| 2（property） | 32 unknown-property / 42 invalid-array-index / 37 value-out-of-range / 38 invalid-data-type | `address` | `address_error` | 属性名或下标写错、值域不符 |
| 2（property） | 40 write-access-denied / 27 read-access-denied | `permission` | `bad` | 点位只读或账号无权限 |
| 3（resources） | 任意 | `disconnected`（可重试） | `comm_error` | 缓冲满、分段不支持 |
| 4（security） | 任意 | `permission` | `bad` | 安全策略拒绝 |
| Reject / Abort | 任意 | `protocol` | `bad` | 报文被拒 / 事务中止 |

`resources` 归为可重试的 `disconnected` 而不是 `bad`：设备只是暂时没有缓冲，
下一轮采集大概率成功，标成坏点会让运维去查一台其实正常的设备。

#### 3.10.8.7 读的宽容与写的严格

读路径对数值类型刻意宽松：同一台设备的 `analog-input` 可能回 REAL，也可能回
UNSIGNED（老设备或网关转换过）。把配置类型当成「怎么解释数值」而不是「设备必须
回什么标签」，现场会少一大半「能连上但读不出值」。具体：

| 配置类型 | 接受的应答标签 |
|---|---|
| 数值（int/uint/float/BCD） | REAL / DOUBLE / UNSIGNED / SIGNED / ENUMERATED |
| bool | BOOLEAN / UNSIGNED / ENUMERATED / SIGNED（非零即 true） |
| string | CHARACTER STRING（其它一律报错） |
| bytes | OCTET STRING / BIT STRING / CHARACTER STRING |

`bool` 与 `string` 保持严格：把字符串当数字读出来只会产生垃圾值；把 `"2"` 当
布尔 `true` 会让「阀门关着」显示成「开着」。

写路径相反：类型不对在本地就拒绝，不发报文。理由很实际——发出去的错误请求
要占用一个采集周期等设备回错，而本地拒绝能立刻告诉用户「这个点位不能这么写」。

#### 3.10.8.8 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| Who-Is / I-Am 广播发现 | 网关只连配置里点名的设备。广播在多网段、多 BBMD 的现场不可控，且发现结果无法固化进配置 |
| COV 订阅 | 设备主动推的模型与本平台「调度式采集」的语义不同，混进来会让超时、重连、死区三套逻辑互相干扰 |
| ReadPropertyMultiple（0x0E） | 嵌入式设备的支持率远低于单属性读，且失败时无法定位是哪一个属性出的问题 |
| 分段请求 / 分段应答 | 需要段窗口与重组；一个点位几十字节，分段只会出现在异常大的属性上 |
| 优先级数组写入的优先级参数 | 驱动层暂不暴露 `[4]`，避免「写了一直不生效、其实是被更高优先级压住」 |
| 对象/属性的自动枚举 | 不做 Browse：点表是配置出来的，不是扫出来的 |

因此 `Meta().Capabilities` 里 `MultiRead`、`BitAccess`、`Browse`、`Discovery`、
`Subscribe` 全部声明 `unsupported`。声明 `supported` 会让 UI 给出「可以批量读」
的暗示，而实际行为是逐条读——这是最糟的一类不一致。

#### 3.10.8.9 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/bacnet`（`BuildMessage`/`ParseMessage`/`ParseACK`/`ReadPropertyRequest`/`WritePropertyRequest`/`ReadPropertyACK`/`BuildComplexACK`/`EncodeApplicationValue`/`DecodeApplicationValue`/`ObjectTypeFromName`/`PropertyFromName`） |
| 主站驱动 | `internal/driver/bacnet`（`Meta`/`Open`/`Read`/`Write`/`Probe`，内部 `exchange`/`readOne`/`writeOne`/`mapBacnetError`） |
| 内置从站 | `internal/sim/bacnet`（`NewServer`/`SetAnalog`/`SetAnalogValue`/`SetBinary`/`SetString`/`SetValue`/`Value`/`Objects`） |
| CLI | `uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 1` |
| 示例 | `examples/bacnet.json`（7 点位：只读温度/单位/名称、可写设定值/标牌、二进制状态、自增计数） |

内置从站默认对象表：

| 对象 | 属性 | 值 |
|---|---|---|
| `analog-input:1` | present-value / object-name / units | 42.5 / `"AI-1"` / 62（摄氏度） |
| `analog-value:2` | present-value / priority-array | 0（可写）/ 16 级 NULL |
| `analog-value:3` | present-value | 每秒自增 |
| `binary-input:1` | present-value | 1（active） |
| `character-string:1` | present-value | `"PUMP-01"`（可写） |
| `device:1` | object-name / model-name | `"UIC-SIM"` / `"sim"` |

自检链路（无硬件）：

```
./uic sim -protocol bacnet -listen 127.0.0.1:47808 &
./uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 1
# analog-input:1.present-value = 42.5（good）
```

`bacread` 不依赖网关即可区分「连不上」「地址写错」「只读」三种失败：

```
./uic bacread -addr 127.0.0.1:47808 -type analog-value -instance 2 -write 55.5
./uic bacread -addr 127.0.0.1:47808 -type character-string -instance 1 -write PUMP-02
./uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 1 -property units
./uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 9
# 读取失败: ... read address: analog-input:9.present-value: device error class 1 code 31
```

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/bacnet` | BVLC/NPDU 长度语义、Forwarded-NPDU、标签头六种 LVT、application/context 差异、BOOLEAN 内联、最短字节数、REAL/DOUBLE 金帧、Read/Write 请求金帧、ComplexACK 两种形态、Error/Reject/Abort 解析、参数往返、别名与编号解析 |
| `driver/bacnet` | 元数据与能力声明、模拟量/字符串/枚举读、类型宽松与严格、未知对象/属性、数组下标与越界、写 REAL/字符串/枚举/布尔、写只读对象、类型不匹配本地拒绝、超时、迟到应答丢弃、重发、ctx 取消、invoke 环绕、错误类映射 |
| `sim/bacnet` | 真实 UDP 回环：默认对象表、数组下标、未知对象/属性、写权限、写类型收敛、分段拒绝、未知服务、垃圾报文静默丢弃、自增与停止、对象表排序 |

> 跨驱动的读回确认：`runtime` 的 `confirm` 用**补齐类型后的请求值**与读回值比较
> （`valuesMatch`），浮点按 float32/float64 的相对容差，整数与布尔/字符串严格相等。
> 早期版本用原始 JSON 值比较，省略 `value.type` 时会把成功写入报成「读回不一致」。

### 3.10.9 IEC 60870-5-104（总召唤 + 主动上送 + 命令，TCP 2404）

电力、水务、轨道交通的调度侧接口，绝大多数是这套协议。它与前面所有协议有一个
根本差别：**数据不是被问出来的，是被推上来的**。

- Modbus / S7 / MC / FINS / CIP / BACnet 都是「主站发一个读请求，等一个应答」，
  采集周期决定数据新鲜度；
- 104 是「建立链路 → 总召唤拿一次快照 → 之后设备按变化主动上送」，
  主站能做的只有「召唤一次全站」和「下发命令」，没有「读第 1001 号点」这种动作。

这个差别会渗透到驱动的每一层，最典型的是 `Read`：它**不做任何网络交互**，
只从本地最新值表取数（与 MQTT 南向订阅同一个模式，见 §3.10.1）。

第二个差别是**质量码的语义**。几乎所有寄存器型协议的质量只有「读到 / 没读到」，
而 104 的每一个信息对象自带品质位（IV/NT/SB/BL/OV），设备会明确告诉你
「这个值我替代过」「这个值我没刷新过」。把这些位丢掉，等于把设备已经说出口的
不确定性又藏回给用户。

第三个差别是**双向序号**。每个方向各有 15 位发送/接收序号，任何一侧错位都必须
断链重建——继续收发只会让后面的数据整体错位，而现场表现是「值偶尔串到别的点位上」。

本驱动只做：平衡式 TCP 主站 + STARTDT/STOPDT/TESTFR + 站总召唤 + 电能量召唤 +
单点/双点/设定值（归一化/标度化/短浮点）命令。边界见 §3.10.9.8。

#### 3.10.9.1 三层封装：长度字段只算控制域之后

```
┌─ APCI（6 字节固定头）───────────────────────────────┐
│ 起始(1)=0x68  长度(1，只算 4 字节控制域 + ASDU)        │
│ 控制域(4)：I 帧两个 15 位序号 / S 帧一个 / U 帧功能码   │
├─ ASDU（可变长）─────────────────────────────────────┤
│ 类型标识(1) VSQ(1) 传送原因(1) 源发地址(1) 公共地址(2) │
│ 信息对象 × N：IOA(3 字节小端) + 元素（含品质/时标）    │
└────────────────────────────────────────────────────┘
```

| 帧型 | 控制域首字节 | 含义 |
|---|---|---|
| I | 最低位 = 0 | 承载 ASDU；同时捎带 N(R) 确认对端 |
| S | `0x01 0x00` | 只确认收到的 I 帧，不承载数据 |
| U | 低两位 = `11` | STARTDT / STOPDT / TESTFR 的 act 与 con |

三个容易写错的地方：

1. **长度字段不含 `0x68` 与长度字节本身**，只算控制域 + ASDU。按「含头」写会让
   对端读到一个多两字节的帧，表现为「第一帧正常、之后全乱」。
2. **U 帧功能码低两位必须是 `11`**。`0x07/0x0B/0x13/0x23/0x43/0x83` 这六个数不是
   随便选的：低两位不是 `11` 的功能码会被解析成 I 帧或 S 帧。
3. **I 帧长度恰好 4（控制域之后无内容）必须判错**。这种帧既不是 S 帧（首字节最低位
   对不上），也不是合法 I 帧；容忍它会让接收循环拿到一条空 ASDU，然后在
   `ParseASDU` 里报一个与真正原因无关的错。

`ParseFrame` 只解析「调用方已经按长度字段切好的一整帧」，不做流式重组。
分帧与解析分开，是为了让「长度字段被写错」在驱动层就变成明确的协议错，
而不是在 ASDU 里静默错位。

#### 3.10.9.2 传输原因、公共地址与信息对象地址

ASDU 头 6 字节：

| 字节 | 内容 |
|---|---|
| 0 | 类型标识（1/3/9/11/13/15/30/31/34/35/36/45/46/48/49/50/70/100/101/103） |
| 1 | VSQ：最高位 SQ（后续对象 IOA 连续递增），低 7 位对象个数 |
| 2 | 传送原因：`P/N`(0x40) + `T`(0x80) + 6 位原因码 |
| 3 | 源发地址 |
| 4–5 | 公共地址（**小端**） |

信息对象地址 IOA 是 **3 字节小端**（1..16777215）。设备侧通常按「类型段」编号
（1001 起遥测、2001 起遥信、3001 起电度、4001 起遥控），但这是工程约定不是协议
约束——驱动不做任何编号推断，`address.offset` 就是 IOA。

传送原因里，驱动真正用到的只有这几个：

| 原因 | 值 | 何时出现 |
|---|---|---|
| 周期 | 1 | 设备定时上送 |
| 突发 | 3 | 值变化时上送（最常见） |
| 初始化 | 4 | 设备上电/重启，主站据此重新总召唤 |
| 激活 | 6 | 主站下发命令 |
| 激活确认 | 7 | 设备接受命令 |
| 激活终止 | 10 | 命令执行完成 |
| 响应站召唤 | 20 | 总召唤的应答 |
| 未知类型 / 原因 / 公共地址 / IOA | 44 / 45 / 46 / 47 | 否定确认的四种原因 |

`P/N=1` 表示否定确认。把这四种否定原因混成「写入失败」，运维就只能去查网络；
分开映射（§3.10.9.6）之后，「IOA 写错了」和「设备不支持这条命令」一眼可分。

#### 3.10.9.3 元素宽度与品质位

每种类型的元素宽度是固定的（不含 3 字节 IOA）：

| 类型 | 宽度 | 说明 |
|---|---|---|
| 单点 / 双点 / 命令（单点、双点、归一化、标度化、短浮点） | 1 / 1 / 1 / 1 | 状态值 + 品质（命令还带 S/E 选择位） |
| 归一化 / 标度化 | 3 | int16 + 1 字节品质 |
| 短浮点 / 累计量 | 5 | float32 或 uint32 + 1 字节品质 |
| 单点带时标 / 双点带时标 | 8 | 1 + 品质 + 7 字节 CP56Time2a |
| 归一化带时标 / 标度化带时标 | 10 | 3 + 7 |
| 短浮点带时标 | 12 | 5 + 7 |
| 时钟同步 | 7 | 只有 7 字节时标 |

**品质位的位序在两类元素里不一样**，这是最容易出「看起来能跑但质量全错」的地方：

- 测量值（QDS）：低 5 位是品质，`OV=1 / BL=2 / SB=4 / NT=8 / IV=16`；
- 单点/双点（SIQ/DIQ）：低 4 位是状态值与保留位，品质位**整体左移 3 位**
  （`BL=8 / SB=16 / NT=32 / IV=64`）。

`decodeSIQQuality` 先把字节右移 3 位再与掩码相与，`encodeSIQQuality` 反过来。
把两者混用，结果是「明明设备说 IV，网关显示 good」——而 IV 恰恰是设备在说
「这个值别用」。

归一化值是 int16 的定点数，换算关系是 `value / 32767`，因此取值范围是 -1..1；
标度化是同宽的普通整数，不做缩放。两者都按 int16 存放，但语义完全不同，
这也是把「命令类型」放在 `address.area` 而不是由 `dataType` 推导的原因
（§3.10.9.5）。

CP56Time2a 是 7 字节：毫秒(2 小端) + 分(1，低 7 位) + 时(1) + 日(1，低 5 位) +
月(1，低 4 位) + 年(1，低 7 位，偏移 2000)。每一位都有保留位，解码时必须掩掉，
否则「2026 年 1 月」会被读成一个 2126 年的非法时间。

#### 3.10.9.4 链路生命周期：STARTDT → 总召唤 → 电能量召唤

```
主站                                从站
 │──── STARTDT act ────────────────▶│
 │◀─── STARTDT con ─────────────────│   ← 之后才允许上送数据
 │──── 站总召唤 (QOI=20) ───────────▶│
 │◀─── 激活确认 ────────────────────│
 │◀─── 数据 ASDU × N（原因=20）──────│   ← 可能几百条，SQ=1 合并同类型连续 IOA
 │◀─── 初始化结束 (类型 70) ─────────│   ← 主站据此判断召唤完成
 │──── 电能量召唤 (QCC=5) ──────────▶│   ← 累计量不在站召唤范围内
 │◀─── 累计量 ASDU ─────────────────│
 │◀─── 突发上送（原因=3）────────────│   ← 之后一直持续
 │──── TESTFR act（空闲 20s）───────▶│   ← 链路保活
 │◀─── TESTFR con ──────────────────│
```

几个决定：

**只等 STARTDT 确认，不等总召唤完成。** 一个几千点的站，总召唤的快照可能有几百条
ASDU；把它们全部收完再返回 `Open`，会让每次重连的耗时与设备规模成正比。快照由
后台接收循环逐步填进缓存，重连后头一两轮采集出现「还没收到」是完全正常的现象，
而不是故障。

**总召唤失败不拆链。** 有些设备在 STARTDT 之后需要几百毫秒才开始处理召唤；此时
总召唤超时是暂时的，突发数据仍会到达。把「召唤超时」升级成「连接失败」会让一台
其实正常的设备反复重连。

**电能量召唤无条件发送。** 累计量（电度）不在站总召唤的范围内，必须单独召唤一次。
只发站召唤的实现在现场表现为「电度点位永远是 bad」——而它其实只是走另一条召唤
路径。主站无法从点表推断设备有没有累计量（累计量取决于设备用哪种 ASDU 上送，
不是点表属性），所以固定发一次；没有累计量的设备会回一条否定确认「未知类型」，
协议上完全正常，按调试日志处理即可，不影响连接。

**EndOfInit 触发重新总召唤，但做 1 秒去重。** 设备重启时它的值可能全变了，重新
召唤是唯一安全的做法。但设备上电瞬间可能连发几条 EndOfInit，不去重会变成召唤风暴。

#### 3.10.9.5 地址语义：`kind=register`，`area` 决定命令类型

点位一律用 `kind=register` 寻址，`offset` 即 IOA：

| 字段 | 含义 | 校验 |
|---|---|---|
| `address.kind` | 必须是 `register` | 其它寻址方式在配置期即拒 |
| `address.area` | 对象类别（见下表） | 6 个取值之一，大小写与空白不敏感 |
| `address.offset` | 信息对象地址（IOA） | `1..16777215` |
| `address.length` | 不使用 | 必须为空或 0 |
| `address.bit` | 不使用 | 必须为空（-1）或 0 |
| `device.unitId` | 公共地址 CA | `1..65535`；0 视为默认 1 |

`area` 的取值：

| `area` | 方向 | 用途 |
|---|---|---|
| `ioa` | 监视 | 读上送值；若该点可写，按 `dataType` 推导命令类型 |
| `single-command` | 控制 | 单点命令（`dataType=bool`） |
| `double-command` | 控制 | 双点命令（`dataType=uint16`，值 1/2） |
| `setpoint-float` | 控制 | 短浮点设定值 |
| `setpoint-scaled` | 控制 | 标度化设定值（int16） |
| `setpoint-normalized` | 控制 | 归一化设定值（-1..1） |

**为什么命令类型放在 `area` 而不是由 `dataType` 推导**：同一个 `float32` 点位，
可能是短浮点设定值，也可能是「放大 100 倍的标度化整数」。`dataType` 只说明平台
怎么存这个数，不说明设备接受哪种命令。让用户显式声明，比让驱动猜要可靠。

`area=ioa` 的可写点仍保留推导路径（bool→单点、float→短浮点、其它→标度化），
但这是**约定并写进文档**的默认值，不是隐式猜测：同一份点表在任何设备上都走同一种
命令，不会因为平台版本不同而变。

**同一个 IOA 可以出现在两个点位里**：一个 `area=ioa` 读状态，一个
`area=double-command` 写命令。这是 104 的常见形态（遥信号与遥控号指向同一个
物理对象），驱动按 `byIOA` 维护一对多映射，两个点位共享同一份缓存值。

#### 3.10.9.6 命令：三条错误路径必须能区分

一条命令的生命周期是：激活（原因 6）→ 激活确认（原因 7）→ 可选的激活终止（原因 10）。
驱动等到「激活确认」就认为设备已接受：

| 应答 | 归类 | 质量码 | 现场含义 |
|---|---|---|---|
| 激活确认（P/N=0） | 成功 | `good` | 设备已接受 |
| 激活终止（原因 10，P/N=0） | 成功 | `good` | 部分设备直接回终止，同样视为接受 |
| 否定确认，原因 44/45（未知类型/原因） | `protocol` | `bad` | 设备不支持这种命令 |
| 否定确认，原因 46/47（未知公共地址/IOA） | `address` | `address_error` | 点表写错了 |
| 否定确认，其它原因 | `permission` | `bad` | 设备拒绝执行（就地/联锁） |
| 超时 | `timeout` | `timeout` | 设备在线但不响应命令 |
| 链路已断 | `disconnected` | `comm_error` | 连接问题 |

把 44/45 和 46/47 归成同一类，会让「设备不支持这条命令」与「IOA 配错」在界面上
看起来一样——而这两件事的修复动作完全不同。

**双点命令只接受 1（分）/ 2（合）。** 标准里 0 与 3 是「不允许」，不是中间态。
把它们当合法值发下去，好设备会回否定确认，差设备会静默不动，两种情况在现场都
表现为「按钮点了没反应」。驱动因此在本地就拒绝，让错误在点击的那一刻暴露出来。
`on`/`off` 是给人用的别名，落到线上仍然是 2/1。

**写入成功后先填本地缓存，质量标 `uncertain`。** 设备会在几十毫秒后把新状态
突发上送，但紧接着的读回确认可能在这个窗口里发生。不填缓存的话，读回确认会读到
上一次采集的旧值，表现为「写成功但值没变」。标 `uncertain` 是因为此刻的可靠性
只来自「设备说它接受了」，还没有实测值。

**回填只补「没有」或「比命令更旧」的条目。** 从站的突发上送常常紧跟激活确认，
甚至先于等待协程被唤醒——无条件覆盖会把一条带真实品质位的上送换成本地推测值，
而 104 只在变化时上送，这个推测值可能长期得不到纠正（现场表现为「遥控之后这个
遥信一直是 uncertain」）。因此 `writeOne` 在发送命令前记下 `sentAt`，只在缓存
条目 `at < sentAt`（或压根没有条目）时才回填。

**命令对象的缓存只在写过之后才存在。** 纯控制方向的 IOA 在监视方向根本没有信息
对象，永远不会被召唤上来。这类点位初次读取返回 `bad` 并附一句明确说明
（「该 IOA 只用于控制方向，从站不上送它的状态」），而不是含糊的「还没收到数据」——
后者会让人一直去查点表和召唤范围。

#### 3.10.9.7 质量与保活：只信设备说的话和链路是否活着

104 的质量码只有两个来源：

1. **ASDU 里的品质位**：`IV` → `bad`、`NT` → `stale`、`SB`/`BL`/`OV` → `uncertain`；
2. **链路是否还活着**：TESTFR 往返失败 → 整体读返回错误，上层把全部点位标成
   `comm_error`。

优先级是 `IV > NT > SB/BL/OV`：值不可信比「被取代」更严重，过时优先于不确定。

**刻意不做「多久没更新就降级」。** 这是 104 驱动最容易犯的错：抄一个「30 秒无更新
转 stale」的规则进来，看起来很稳健。但 104 的从站**只在值变化时上送**，一个温度
三十分钟不变是完全正常的；按时间降级会把整页点位刷成 stale，运维于是学会忽略
stale——这比不显示质量码更糟。

链路的活性由 keeper 协程负责：

| 参数 | 默认值 | 作用 |
|---|---|---|
| k | 12 | 未确认 I 帧上限；打满则发送方等确认 |
| t1 | 15s | 发出 I 帧 / TESTFR act 后等确认的上限 |
| t2 | 10s | 收到数据后最迟发确认的间隔 |
| 空闲阈值 | 20s | 链路上没有任何报文多久之后发 TESTFR act |

选 20 秒空闲探测：远小于绝大多数 NAT 表项的空闲超时（通常 2–10 分钟），
又不会让一条安静但健康的链路每分钟都被打扰。

**TESTFR 没有回应就判死，而不是继续等。** 「设备还在，只是不说话」与「中间的网络
设备悄悄断了」在主站看来是一样的；区别在于前者会在下一次 PING 时回应。所以判死
的条件是「发出 TESTFR act 之后 t1 内没收到 con」，而不是「一段时间没收到数据」。

#### 3.10.9.8 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| 非平衡式（101 串口） | 那是另一套链路层（FT1.2），与 TCP 的序号、启停、超时模型完全不同；混在一个驱动里会让两边都写不清楚 |
| 文件传输 | 很少用于采集，且需要独立的传输状态机与超时预算 |
| 时钟同步的写路径 | 编码层有 `BuildClockSync`，但驱动不主动对时：生产中改设备时钟是高风险动作，应由运维显式触发 |
| 冗余通道切换 | 需要把「两套链路 + 一致性判定」做进驱动，而调度层已经有重连与退避；两层同时决定用哪条链路会互相打架 |
| 带时标的命令 | 现场用得极少，且需要设备支持；无时标命令已经能覆盖全部现有对象 |
| 组召唤（QOI 21..36） | 站召唤一次拿全量，组召唤省下的报文量在网关场景下没有意义 |
| 带品质/时标的命令回读 | 激活确认已经足够判断「设备是否接受」 |
| 自动点表发现协议动作 | 没有「列举 IOA」这种服务；点表发现靠旁路表（§3.10.9.9） |

因此 `Meta().Capabilities` 里 `Browse`、`Discovery`、`BitAccess`、`StructTypes` 全部
声明 `unsupported`。`Subscribe` 声明 `supported`——但它的含义与 MQTT/OPC UA 一致：
**协议本身是推送型，由驱动内部消化成最新值表，上层拿到的仍是 `Read` 拉取契约**。
声明 `unsupported` 会让配置界面建议用户换协议，方向就错了。

#### 3.10.9.9 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/iec104`（`ParseFrame`/`BuildIFrame`/`BuildSFrame`/`BuildUFrame`/`ParseASDU`/`BuildASDU`/`ElementSize`/`IsData`/`IsCommandType`/`IsTimeTagged`/`EncodeCP56Time2a`/`DecodeCP56Time2a`/`BuildInterrogation`/`BuildCounterInterrogation`/`BuildSingleCommand`/`BuildDoubleCommand`/`BuildSetpointNormalized`/`BuildSetpointScaled`/`BuildSetpointFloat`/`TypeName`/`CauseName`） |
| 主站驱动 | `internal/driver/iec104`（`Meta`/`Open`/`Read`/`Write`/`Probe`，内部 `startDataTransfer`/`interrogate`/`counterInterrogate`/`readLoop`/`keeper`/`dispatch`/`buildCommand`/`Observed`） |
| 内置从站 | `internal/sim/iec104`（`NewServer`/`SetValue`/`Value`/`Objects`/`SetCommonAddr`/`SetAutoIncrement`/`BadFrames`） |
| CLI | `uic iec104read -addr 127.0.0.1:2404 -ca 1` |
| 示例 | `examples/iec104.json`（14 点位：短浮点/标度化/取代值/带时标/单点/双点/累计量 + 单点命令/短浮点设定值/标度化设定值/双点遥控） |

内置从站默认对象表（公共地址 1）：

| IOA | 类别 | 值 | 备注 |
|---|---|---|---|
| 1001 | 短浮点 | 42.5 | 普通遥测 |
| 1002 | 短浮点 | 7.25 | |
| 1003 | 标度化 | 1234 | int16 语义 |
| 1004 | 短浮点 | 0.5 | 品质 SB（应显示 `uncertain`） |
| 1005 | 短浮点带时标 | -3.5 | 固定时标 2026-01-02 03:04:05 |
| 2001 | 单点 | on | |
| 2002 | 单点 | off | |
| 2003 | 双点 | 2（合） | 可写，遥控后自发上送新状态 |
| 2004 | 双点 | 1（分） | |
| 3001 | 累计量 | 1000 起 | 每秒自增，只在电能量召唤里出现 |
| 4001 | 单点命令 | off | 纯控制方向，不参与站召唤 |
| 4002 | 短浮点设定值 | 0 | 纯控制方向 |
| 4003 | 标度化设定值 | 0 | 纯控制方向 |

自检链路（无硬件）：

```bash
./uic sim -protocol iec104 -listen 127.0.0.1:2404 &
./uic iec104read -addr 127.0.0.1:2404 -ca 1
# 从站 127.0.0.1:2404  CA=1
# 设备上送 10 个信息对象
#   1001  短浮点测量值   good       42.5
#   1004  短浮点测量值   uncertain  0.5
#   2003  双点信息       good       on (2)
#   3001  累计量         good       1017
```

`iec104read` 的设计与 `bacread`/`enipread` 不同：它**不依赖点表**。

理由很实际。初次接入一台陌生设备时，真正的问题是「它到底送哪些 IOA、用哪种
ASDU 类型、带不带时标」，而不是「我配置的点现在是什么值」——后者用网关的
`/api/v1/points` 看，那里才有映射、报警与历史。而前者的答案无法从配置推测：
同一个物理量既能用标度化送也能用短浮点送，点位声明的 `dataType` 只是「我们愿意
怎么存它」。

因此驱动维护一张**旁路表**（`Observed`）：每收到一条数据 ASDU，就按 IOA 记录
原始类型、传送原因与解码后的信息对象，无论这个 IOA 有没有被配置。这张表：

- 只读本地内存，不产生任何网络交互；
- 有 4096 条的硬上限——IOA 是 24 位的，一个异常从站可以合法地送出几百万个不同
  IOA，无上限会把进程内存吃光；超限后只更新已知 IOA，新 IOA 丢弃并计数；
- 与点位表完全独立，因此**点表全空也能诊断**（实现上仍放一个几乎不存在的探针
  点位，因为驱动要求至少一个可用点才肯开口）。

`iec104read` 不靠它做任何判断，只把它打印出来——包括双点的 0/3 中间态（显示为
「不确定 (3)」而不是硬转成布尔），因为这些正是设备在说「我的状态不明确」。

CLI 同时覆盖三层自检与命令下发：

```bash
./uic iec104read -addr 127.0.0.1:2404 -probe          # 只做 STARTDT + TESTFR
./uic iec104read -addr 127.0.0.1:2404 -raw -ioa 1001,2003
./uic iec104read -addr 127.0.0.1:2404 -area single-command -cmd-ioa 4001 -write on
./uic iec104read -addr 127.0.0.1:2404 -area double-command -cmd-ioa 2003 -write off
./uic iec104read -addr 127.0.0.1:2404 -area setpoint-float -cmd-ioa 4002 -write 12.5
./uic iec104read -addr 127.0.0.1:2404 -area single-command -cmd-ioa 1001 -write on
# 命令失败: ... command for IOA 1001 rejected: 未知信息对象地址
```

最后一条演示了「只读对象被当成命令目标」：设备回否定确认（原因 47），驱动归类为
`address_error`，CLI 把它原样打出来。这三层的区分正是 `-probe` 存在的意义：
TCP 能连上、STARTDT 能过、TESTFR 能往返，之后才轮到 ASDU 的问题。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/iec104` | APCI 三种帧型的金帧、长度字段语义、U 帧功能码低两位、I 帧空载荷拒绝、ASDU 头与 SQ、IOA 24 位、全部元素宽度、归一化/标度化/短浮点往返、CP56Time2a 保留位掩码、SIQ/DIQ 品质位移、QDS 品质位、命令构造（选择位）、类型/原因名与别名、越界与截断 |
| `driver/iec104` | 元数据与能力声明、地址规划与全部拒绝路径、STARTDT 握手与超时、站召唤与电能量召唤、I 帧乱序断链、S 帧越界确认、U 帧六个功能码、发送窗口打满与确认放行、连接关闭中断等待、keeper 空闲探测与判死、命令五种类型与三类错误、否定确认分类、缓存填充与不污染、旁路表（含未配置 IOA、一对多、上限、丢弃计数）、纯控制方向点位的说明文字、回填不覆盖更新的突发上送 |
| `sim/iec104` | 真实 TCP 回环：STARTDT 之前不上送、STARTDT/STOPDT/TESTFR 六个 U 帧、站召唤完整快照（含 SQ=1 分组不截断）、累计量走另一条召唤、命令三段式与否定确认、只读对象拒绝、未知类型/公共地址、乱序断链、S 帧接受、垃圾帧计数、ASDU 解析失败只计数、多连接各自维护序号、突发上送、累计量自增、`Close` 幂等；`-race` 下无数据竞争（`autoIncrement` 与 `station.started` 各自归其锁保护，锁外读会被 race 检测器抓到） |

### 3.10.10 DL/T 645-2007（多功能电能表，RS-485 / 串口服务器）

电能表是工业网关最常接、也最容易被低估的一类设备。它不像 PLC 那样有清晰的寄存器
地图，而是用一套**语义化数据标识（DI）**描述「我要 A 相电压」这件事；线路上还叠了
前导字节、双 `68` 定界、逐字节加 `0x33` 的偏移。这些特征看起来只是「麻烦一点」，
实际决定了驱动的三个结构选择：不能合并读、必须逐点串行、错误必须能落到「是哪个
数据项」。

#### 3.10.10.1 为什么不是一个「串口版 Modbus」

| 维度 | Modbus RTU | DL/T 645-2007 |
|---|---|---|
| 寻址 | 区 + 偏移 + 数量 | 4 字节数据标识（DI） |
| 值编码 | 大端二进制，类型由点表声明 | 压缩 BCD，小数位与符号位由 DI 定义 |
| 帧定界 | 静默间隔（3.5 字符） | 前导 `FE`×n + 双 `68` + 长度 + 校验和 |
| 数据混淆 | 无 | 数据域逐字节 `+0x33` |
| 批量 | 一次读连续区间 | 一次只能读一个 DI（数据块除外） |
| 分帧 | 无 | 读后续帧（`0x12`），SEQ 1..255 |

只有「拿到一段可靠字节流」这件事是共用的——那正是 §3.6.1 的 `serialport` 层；其余
一概不同。把 645 硬塞进 Modbus 的地址模型，会让「DI 表」被迫表达成「偏移区间」，
最终不得不在驱动里维护一张反向映射表，比单独写一个驱动更难维护。

#### 3.10.10.2 帧结构

```
┌──────────┬────┬──────────────────┬────┬───┬───┬─────────────┬────┬────┐
│ FE × 4   │ 68 │ A0 A1 A2 A3 A4 A5│ 68 │ C │ L │   DATA…     │ CS │ 16 │
│ 前导     │ 起始│ 地址域（BCD，低字节先传）│ 起始│控制│长度│ 数据域（+0x33）│校验│结束│
└──────────┴────┴──────────────────┴────┴───┴───┴─────────────┴────┴────┘
```

| 字段 | 宽度 | 规则 |
|---|---|---|
| 前导 | 0..8（标准 4） | 仅用于唤醒接收方；**不计入长度与校验和** |
| 起始符 | 1 | 固定 `0x68` |
| 地址域 | 6 | 压缩 BCD，**低字节先传**；缩位写 `0042` 时高位补 `AA`（通配） |
| 起始符 | 1 | 固定 `0x68`（第二个，不是冗余：它让「读地址域时错位」可被检出） |
| 控制码 | 1 | D7 方向、D6 异常、D5 后续帧、D4..D0 功能码 |
| 长度 | 1 | 数据域字节数，**不含**前导与校验 |
| 数据域 | L | 每字节已加 `0x33`；编码层收帧时统一减回 |
| 校验和 | 1 | 从**第一个 `0x68`** 到 **DATA 末字节**（含 `+0x33` 后的值）的模 256 和 |
| 结束符 | 1 | 固定 `0x16` |

三个最容易写错的地方，代码里各有一条注释与一个金帧用例钉住：

1. **前导不进校验。** 把 `FE` 算进去会让校验和对所有带前导的帧都错——而真实设备
   永远带前导，于是表现为「一条都读不到」。
2. **地址低字节先传。** `000000000042` 在线路上是 `42 00 00 00 00 00`。按人们习惯的
   高位先写会让 42 号表变成 420000000000 号表——它当然不会应答。
3. **`+0x33` 作用于整个数据域，包括 DI。** 只对「值」加偏移、忘了 DI，会让设备看到
   一个不存在的数据标识并回「无请求数据」，看起来像点位配错了。

长度边界与标准一致：读/读后续的数据域上限 200 字节（`MaxData`），写数据域上限
50 字节（`MaxWriteData`）。写请求的 12 字节头（DI 4 + 密码权限 4 + 操作者 4）也计入
这 50 字节，因此单次可写参数最多 38 字节。

#### 3.10.10.3 地址：DI 不是偏移

点表里一个 DL/T 645 点位长这样：

```json
{
  "id": "voltage-a",
  "address": { "kind": "register", "area": "02010100", "offset": 0, "bit": -1 },
  "dataType": "float64",
  "unit": "V"
}
```

- `area` 是 **8 位十六进制的 DI，按手册书写顺序 `DI3DI2DI1DI0`**。`02010100` 就是
  「A 相电压」，不是「偏移 0x02010100」。
- `offset` **只能是 0**。数据标识寻址没有「在数据项内部偏移」的概念；写别的值一定
  是从寄存器型协议抄过来没改，越早报错越好。
- `length` 通常省略——宽度是数据项的固有属性（电压 2 字节、电流 3 字节、电能 4
  字节），由 DI 表决定。只有**数据块**（DI 任一字节为 `FF`）才需要显式给出。

这带来两个点表层的新东西：

| 概念 | 取值 | 理由 |
|---|---|---|
| `AddressPolicy.Width` | `WidthProtocol`（新增） | 「宽度由协议定义」与「宽度由 DataType 推导」是两件事。没有这个取值，点表要么被迫接受一个错误的宽度比对，要么在驱动里复制一张 DI 表 |
| `AddressPolicy.DIBits` | `8` | DI 语法与 `register` 同形（`DI:offset:length`），必须在 `splitRegisterPrefix` 之前拦下，否则 `02010100:0` 会被当成 S7 的「数据块 2010100」 |

为什么不在点表层复制 DI 表：点表导入发生在 CI 里，此时没有驱动、没有设备，也不该
import 协议 codec。宽度的权威解释留在驱动里——那里报错能说出「A 相电压应为 2 字节，
实际 4 字节」，比一句「长度不对」有用得多。

地址语法接受三种写法，产出同一个结构化地址：

| 写法 | 含义 |
|---|---|
| `02010100` | 普通数据项，宽度由 DI 表决定 |
| `02010100:0` | 同上，`offset` 显式写 0（部分点表模板省不掉这一列） |
| `0201FF00:0:6` | 数据块，显式给出期望长度 |

应当被拒绝的写法（均有测试）：

| 写法 | 报错 |
|---|---|
| `020101` | DI 必须是 8 位十六进制 |
| `0201010G` | 含非十六进制字符 |
| `02010100:4` | offset 必须为 0 |
| `02010100:0:2:9` | 字段过多 |
| `FFFFFFFF` | 不是合法数据标识（通常是「地址还没填」的模板） |
| `04000402` + `bool` | 645 不做位寻址；状态字按 `uint16` 读 |

表地址不走 `unitId`，而是 `device.labels.address`：它本质上是**设备身份**（印在表壳
上），不是「同一条总线上的站号」。混用会让「换表不换配置」变成一次隐式改号。

#### 3.10.10.4 控制码与异常应答

| 命令 | 请求 | 正常应答 | 异常应答 | 说明 |
|---|---|---|---|---|
| 读数据 | `0x11` | `0x91` | `0xD1` | 数据域 = DI(4) + 数据 |
| 读后续 | `0x12` | `0x92` | `0xD2` | 请求 = DI(4) + SEQ(1)；应答 = DI(4) + 数据 + SEQ(1) |
| 读通信地址 | `0x13` | `0x93` | `0xD3` | 地址域全 `AA`，任何一台表都应答 |
| 写数据 | `0x14` | `0x94` | `0xD4` | 数据域 = DI(4) + PA P0..P2(4) + C0..C3(4) + 参数 |
| 冻结 | `0x16` | `0x96` | `0xD6` | 广播时从站执行但不应答 |
| 拉合闸 | `0x1C` | `0x9C` | `0xDC` | 备案扩展；数据域形状与普通写不同 |
| 广播校时 | `0x08` | — | — | 地址全 `99`，**从站不应答** |

D6 是异常标志：置 1 时数据域只剩 1 字节错误码，且**它是位集而不是枚举**。驱动把
每一位映射成一个独立的判断，而不是 `switch` 成一句话：

| 位 | 含义 | 驱动的处置 |
|---|---|---|
| `bit1` | 无请求数据 | 该 DI 在表里不存在 → `address_error`（配置问题） |
| `bit2` | 密码错/未授权 | `permission`，且**不重试**（重试只会再错一次） |
| `bit3` | 通信速率不能改 | `protocol`，标记可重试但无意义 |
| `bit4` | 年时区数超限 | `protocol` |
| `bit5` | 日时段数超限 | `protocol` |
| `bit6` | 费率数超 | `protocol` |
| `bit7` | 其它 | `protocol`，附原始错误字节 |

把「无请求数据」单独拎出来，是因为它是**唯一一个大概率是配置错误**的异常：地址写错
一位、DI 写错一位、或点表引用了这块表不支持的数据项。其余错误更可能是设备状态或
权限问题，处置方式不同。

#### 3.10.10.5 值编码：压缩 BCD、低字节在前、小数位在 DI 表

| 数据项 | 宽度 | 编码 |
|---|---|---|
| 电能（`DI3=00`） | 4 | 压缩 BCD，两位小数，最高字节 bit7 为符号位 |
| 电压 | 2 | 压缩 BCD，一位小数 |
| 电流 | 3 | 压缩 BCD，三位小数，带符号 |
| 功率 | 3 | 压缩 BCD，四位小数，带符号 |
| 功率因数 | 2 | 压缩 BCD，三位小数，带符号 |
| 频率 | 2 | 压缩 BCD，两位小数 |
| 时间 | 6 | `ss mm hh DD MM YY`，每字节 BCD |
| 状态字 | 1..2 | 二进制位集，**不是 BCD** |

三条实现约定：

1. **解码只做工程值换算，不施加点位 `Scale`/`Bias`。** DI 表里的小数位是协议的一部分
   （`2200` → `220.0`），而 `Scale` 是用户对工程的二次修正。两者都在驱动里做会让
   「为什么读数差了 10 倍」变成一道推理题。缩放统一由采集管线 `pipeline.scaleValue`
   施加一次。
2. **3 字节量统一用 `float64` 承载。** 平台没有 24 位整数类型；值已经换算成工程值，
   再用一个「宽度未定义」的整数类型去存，只会在北向多一层解释。
3. **数据块只按 `bytes` 交付。** 数据块是若干数据项的串联（例如电能数据块是 15 个
   4 字节项），没有单一数值含义。硬解成 `float64` 会给出一个「看起来像读数、实际是
   前 8 个字节」的假值——而错位的电能读数会被当成真实电量入账。因此点位的
   `dataType` 必须是 `bytes`（或 `string` 作诊断用），否则驱动直接报 `ErrUnsupported`。

时间在平台侧没有专用类型，压成 `YYYYMMDDhhmmss` 的数值（如 `20261107120000`）。选数值
而不是字符串，是因为北向、历史库、报警比较都能直接按大小比较——"2026-11-07 12:00:00"
按字符串比较在跨年时是对的，但没人愿意为此写特例。

**4 字节日期数据项**（结算日、时区日等）在线路上只有 `MMDDhhmm`，没有年，所以读出来
是 8 位（`11071200` = 11 月 7 日 12:00）。写回接受同形的 8 位值，也接受 14 位完整时间
（只取月/日/时/分）。这条「读出来的值必须能原样写回」的约束有专门用例：没有它，把刚
读到的结算日写回会得到一个月 = 0 的垃圾日期，而且不报错。

时间与日期的**范围校验在本地完成**：`20261307120000`（13 月）、`20260230120000`
（2 月 30 日）一律拒绝，不让 `time.Date` 的归一化行为把「13 月」悄悄变成「次年 1 月」。
改电表时钟本来就是高风险动作，一个不存在的日期被写进去，后果是这块表在错误的时间
结算，而现场几乎不可能把它和这次写操作联系起来。

#### 3.10.10.6 读：逐点串行、跟完后续帧、链路短路

```
for each span:            # 规划层保证同一 DI 的点在同一 span
  for each point:         # 但驱动仍逐点发，DI 之间没有「连续」概念
    if linkErr: 标记坏点，continue
    send 0x11 (DI)
    if 异常: 标记坏点，continue
    while 应答有后续帧:
      send 0x12 (DI, SEQ++)
      append 数据
    校验长度（非数据块必须等于 DI 宽度）
    解码 → 平台值
```

三处刻意的设计：

- **不合并。** 规划层按 `area`（即 DI）分组，不同 DI 不合并；同一 DI 的多个点位共用
  一次事务的结果。这与 Modbus 的「相邻地址合并成一个请求」不是一回事——645 里两个
  相邻 DI 在语义上可能毫不相干（A 相电压与 A 相电流差一个字节）。
- **必须跟完后续帧。** 一条被截断的电能读数会以「比实际少了几位」的形式入账，而且
  看不出任何异常。驱动在**同一次 `Read` 内**跟完，并有 64 帧（约 12KB）的硬上限。
- **链路级故障短路整轮。** 超时/断开后，剩余点位快速标记为同样的错误，而不是每个点
  都等一次超时。300 个点位挂在 2400bps 总线上，一轮本该几十秒——再乘以点数就是事故。

长度校验落在驱动而不是 codec：codec 只负责「这一段字节是什么」，驱动才知道「这个
DI 按标准应该多宽」。

#### 3.10.10.7 写与高危命令：四道闸门

| 闸门 | 配置 | 控制什么 |
|---|---|---|
| 1 | `options` 含 `allow-write` | 普通写数据（`0x14`） |
| 2 | `options` 含 `allow-control` | 拉合闸（`0x1C`）、清零（`0x19`/`0x1A`/`0x1B`） |
| 3 | DI 表 `Writable` | 标准里该数据项是否可写（电压/电流/电能永远只读） |
| 4 | 设备密码 | 电表自己的权限等级（`PA` 高 4 位 + `P0..P2`） |

四道闸门是「与」而不是「或」。一个「能读」的驱动不应该顺手能把闸拉了——这是
`allow-control` 单独存在、且默认关闭的全部理由。CLI 的自检命令同样遵守：`-write`
自动打开第 1 道，`-allow-control` 才打开第 2 道。

写数据域的形状与读完全不同：

```
DI(4) | PA P0 P1 P2(4) | C0 C1 C2 C3(4) | 参数(m)
```

- `PA` 高 4 位是密码权限（`0x02` = 普通表明文），低 4 位是密码等级；
- `P0..P2` 是密码，来自 `options` 的 `password=`（6 位十六进制）；
- `C0..C3` 是操作者代码，仅用于设备侧追溯，驱动默认全 0。

拉合闸（备案扩展）是另一套形状：

```
密码(4) | 操作者(4) | 控制类型(1) | 保留(1) | 时间(6)
```

控制类型 `0x1A` = 跳闸、`0x1B` = 合闸允许；时间给的是「允许执行的时间窗口」。驱动
只接受这两个值，其余本地拒绝——把控制类型当自由字段透传，等于把「拉错闸」的可能
留给了一层打字错误。

**不做广播。** 广播校时（`0x08`）与广播冻结（`0x16` + 全 `99` 地址）会同时改掉总线上
所有表的时间/需量。它们必须由运维显式发起（运维工具/CLI 的单独流程），不能出现在
任何周期任务里。驱动连入口都不提供——这比「提供了但默认关闭」更安全，因为没人能
通过改一个布尔值把它打开。

#### 3.10.10.8 两种承载：RS-485 与串口服务器

| 承载 | `transport.kind` | 锁的粒度 | 排空 |
|---|---|---|---|
| 真实串口 | `serial` | 同一物理端口全局互斥（`serialport.Ref`） | `tcflush` 级排空 |
| 串口服务器 / DTU | `tcp` | 每连接一把互斥量 | 尽力而为：读掉已到达的残留字节 |

两种承载的**分帧、校验、命令序列完全相同**，因此只有「怎么拿到一段字节流」不同。
这也是内置 TCP 从站能直接用于自检的原因：它跑的是同一条字节流路径。

TCP 上有一个必须写清楚的差别：`Drain` 做不了「清空内核缓冲」。能做的只是把已到达的
残留字节读掉。所以串口服务器上偶发的「读到上一轮应答」只能靠**帧同步**（收帧时向前
扫描第一个 `68`）兜底，而不是靠 `tcflush`。

一个实测踩到的顺序坑：`Drain()` 会把读/写截止时间压到「现在」，如果先
`SetDeadline()` 再 `Drain()`，本次事务的截止时间会被排空动作覆盖成过去时刻，表现为
**每次写都 `i/o timeout`**，而读却正常（读路径没有排空）。正确顺序是先排空、再设
截止时间。

帧间静默 `gap` 默认 30ms：标准只给了「帧间 >20ms」的宽松下限，取 30ms 覆盖 USB 转
485 的批量传输延迟（实测 5~20ms），又不至于在 300 台表的总线上把帧间隔放大到无法
接受。前导字节数可配（`preamble=0`），因为现场确有一批适配器在前导之后丢第一个字节，
表现为「不发 FE 能通、发了就不通」。

#### 3.10.10.9 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| 97 版协议 | 帧结构、控制码、DI 表都不同；混在一个驱动里会让「这块表到底按哪版解释」变成运行时猜谜 |
| 广播校时/广播冻结 | 见 §3.10.10.7：一条配错的广播会同时改掉总线上所有表 |
| 主动上报（事件/报警） | 645 的事件上报依赖厂商扩展，标准未统一；不同表的上报帧形状差异极大 |
| 合并读 | 一个 DI 一次事务；DI 之间没有「连续」概念 |
| 位寻址 | 状态字按 `uint16` 整体读出，由上层按位解释；驱动不猜哪一位叫什么 |
| 自动发现 DI | 没有「列举数据项」这种服务；DI 必须来自手册/点表 |
| 表地址自动扫描 | 总线上可能有几十台表，逐个试地址会打断正常采集；应由运维显式发起 |

因此 `Meta().Capabilities` 里 `Subscribe`、`Browse`、`Discovery`、`MultiRead`、
`BitAccess`、`StructTypes` 全部声明 `unsupported`。`Read`/`Write` 声明 `supported`。

#### 3.10.10.10 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/dlt645`（`Parse`/`Build`/`ReadRequest`/`ReadNextRequest`/`ReadAddrRequest`/`WriteRequest`/`BroadcastTimeRequest`/`RelayRequest`/`ParseReadReply`/`ParseReadNextReply`/`ParseAddrReply`/`WriteAck`/`DIFromBytes`/`ParseDI`/`Lookup`/`Width`/`Decode`/`Format`/`EncodeValue`/`EncodeTime`/`ParseTime`/`EncodeAddr`/`Sum`/`Reader.Next`） |
| 主站驱动 | `internal/driver/dlt645`（`Meta`/`Open`/`Probe`/`Read`/`Write`，内部 `exchange`/`readPoint`/`readNext`/`sample`/`classify`/`resolve`/`parseOptions`） |
| 内置从站 | `internal/sim/dlt645`（`NewServer`/`ServeStream`/`SetValue`/`Value`/`Relay`/`Writes`/`Address`） |
| CLI | `uic dlt645read`（`-probe`/`-di`/`-write`/`-raw`/`-json`，`serial`/`tcp` 双承载） |
| 示例 | `examples/dlt645.json`（13 点位：电能/电压/电流/功率/功率因数/频率/需量/时钟/状态字/数据块/结算日） |

内置从站默认数据项（表地址 `000000000042`）：

| DI | 含义 | 值 | 可写 |
|---|---|---|---|
| `00000000` | 组合有功总电能 | 123456.78 kWh | ❌ |
| `00010000` | 正向有功总电能 | 123456.78 kWh | ❌ |
| `00020000` | 反向有功总电能 | 0 | ❌ |
| `02010100/02010200/02010300` | A/B/C 相电压 | 220.0 / 219.8 / 220.3 V | ❌ |
| `02020100/02020200/02020300` | A/B/C 相电流 | 5.123 / 4.987 / 5.201 A | ❌ |
| `02030000` | 瞬时总有功功率 | 1.2345 kW | ❌ |
| `02060000` | 瞬时总功率因数 | 0.985 | ❌ |
| `02800002` | 电网频率 | 50.00 Hz | ❌ |
| `01010000` | 最大需量及发生时间 | 12.3456 kW @ 11-07 12:00 | ❌ |
| `0201FF00` | 电压数据块（三相共 6 字节） | 按 `bytes` 交付 | ❌ |
| `04000101` | 日期及时间 | 2026-11-07 12:00:00 | ✅ |
| `04000401` | 通信地址 | 000000000042 | ✅（必须写完整 12 位） |
| `04000402` | 电表运行状态字 1 | 0 | ❌ |
| `04000B01` | 第 1 结算日 | 11-07 12:00（`MMDDhhmm`） | ✅ |

自检链路（无硬件，TCP 透传）：

```bash
./uic sim -protocol dlt645 -listen 127.0.0.1:14001 &
./uic dlt645read -addr 127.0.0.1:14001 -address 42 -di 00000000,02010100,02020100
# 表地址 42  传输 tcp
#
# DI        数据项                值          质量
# 00000000  组合有功总电能        123456.78   good
# 02010100  A 相电压              220         good
# 02020100  A 相电流              5.123       good
```

```bash
# 只验物理层（读通信地址 0x13，不需要点表）：
./uic dlt645read -transport serial -port /dev/ttyUSB0 -baud 2400 -address 0042 -probe

# 数据块按原始字节显示：
./uic dlt645read -addr 127.0.0.1:14001 -address 42 -di 0201FF00 -raw
# 0201FF00  电压数据块  00 22 98 21 03 22  good

# 写时钟（自动打开 allow-write；表密码走 -password）：
./uic dlt645read -addr 127.0.0.1:14001 -address 42 -di 04000101 -write "2026-11-07 12:00:00"

# 带上网关跑真实采集：
./uic -config examples/dlt645.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8093/api/v1/points
```

写通信地址（DI `04000401`）必须给完整的 12 位数字。缩位写法 `42` 在读的时候是合法的
（高位由主站补 `AA` 通配），但写进表里等于把表号改成 `AA AA AA AA AA 42`——之后除了
通配读就再也点不到它。CLI 与驱动两层都拒绝这种写法。

`dlt645read` 的排查顺序内建在失败提示里：**波特率/校验位 → 表地址 → 前导字节 → A/B
接线**。这四步覆盖了 RS-485 上「不通」的绝大多数原因，而且每一步都能用同一条命令
单独验证（`-baud`/`-parity`/`-address`/`-preamble 0`），不需要另装抓包工具。

传输方式默认值按「用户给了哪组参数」推断：给了 `-addr` 就是 `tcp`，给了 `-port` 或
什么都不给就是 `serial`。这避免了一个很实际的摩擦——自检最常用的写法是只给 `-addr`
指向串口服务器，却因为默认串口而先撞一次 `/dev/ttyUSB0: no such file`。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/dlt645` | 金帧（读/读后续/读地址/写/拉合闸/广播校时）、前导不进校验和、地址低字节先传、`+0x33` 往返、CS 覆盖范围、L 上限（读 200/写 50）、异常位解析、DI 表宽度与小数位、压缩 BCD 符号位、3 字节量、时间 6 字节往返、4 字节日期、地址格式化、收帧扫描与截断/坏校验 |
| `driver/dlt645` | 元数据与能力声明、探针成功/错表拒绝、电能/电压/电流/需量解码、未收录 DI、未知 DI→`address_error`、后续帧（250 字节重组 + 前 4 字节校验）、写需 opt-in、写时钟往返、结算日 8 位值原样写回、非法日期（13 月/2 月 30 日）本地拒绝、只读 DI（含状态字）拒绝、缺 `labels.address`、超时分类、链路失败短路、异常位→错误分类 |
| `sim/dlt645` | 真实 TCP 回环：标准宽度、忽略他表地址、无数据异常位、写权限/密码/只读、后续帧拆分与重组（500 字节）、读地址、广播校时静默且生效、拉合闸 + 非法动作拒绝、`Close` 幂等、`ServeStream` over `net.Pipe` |
| 点表/规划 | DI 三种写法与全部拒绝路径、`WidthProtocol` 策略、不同 DI 不合并、同 DI 同 span |
| `cmd/uic` | DI 列表去重/非法 DI/全 FF 拒绝、时间五种写法与非法日期拒绝、日期类 8 位 `MMDDhhmm` 与完整时间、地址缩位写拒绝、数据块走 `bytes`、传输方式按参数推断 |

### 3.10.11 CJ/T 188-2004（户用计量仪表：水表 / 燃气表 / 热量表）

这是本仓库第二个「数据标识型」协议。它和 §3.10.10 的 645 看起来都是「一串十六进制
标识 + 压缩 BCD」，但两者**不能共用一套解析**：188 只有一个 `0x68`、数据域不加
`0x33`、DI 只有 2 字节，而且**一个 DI 是一组字段**而不是一个值。把 645 的经验直接
搬过来，第一帧就会因为校验和对不上而失败；即使勉强通了，901F 也会被按「4 字节一个
数」切开，读出来的是「流量变成温度」这类合法但错误的值。

#### 3.10.11.1 与 DL/T 645 的关键差异

| 维度 | DL/T 645-2007 | CJ/T 188-2004 |
|---|---|---|
| 起始符 | 双 `68` 定界 | 单个 `68` |
| 前导 | `FE`×n（标准 4） | `FE`×n（标准 2..4，可 0） |
| 地址 | 6 字节 BCD，可缩位补 `AA` | 7 字节 BCD，14 位十进制，**不支持缩位** |
| 仪表类型 | 无独立字段 | 帧头 `T`：10 水 / 20 热 / 30 气 / 40 其他 |
| 数据域混淆 | 每字节 `+0x33` | **无偏移** |
| 数据标识 | 4 字节 DI，一个 DI 一个值 | 2 字节 DI，**一个 DI 一组字段** |
| 单位 | 由 DI 的小数位隐含 | 数据域里的**显式单位字节** |
| 时间 | 6 字节（秒分时日月年） | 7 字节（秒分时日月年低年高） |
| 批量 | 一次一个 DI（数据块/后续帧） | 一次一个 DI；同 DI 多字段复用一次事务 |

差异最后都收敛成两个设计决定：**schema 按 (仪表类型, DI) 分派**、**点位 offset 表示
字段在 DI 数据域内的字节偏移**。前者防止水表/热量表互相串味，后者让「同一帧里的
四个字段」能各配一个点位而不需要驱动再发明一套地址语法。

#### 3.10.11.2 帧结构

```
┌────────┬────┬────┬──────────────────┬────┬───┬─────────────┬────┬────┐
│ FE × n │ 68 │ T  │ A0 A1 A2 A3 A4 A5│ A6 │ C │ L │   DATA…     │ CS │ 16 │
│ 前导   │起始│类型│ 地址域（BCD，低字节先传）│   │控制│长度│ 数据域（无偏移）│校验│结束│
└────────┴────┴────┴──────────────────┴────┴───┴─────────────┴────┴────┘
```

| 字段 | 宽度 | 规则 |
|---|---|---|
| 前导 | 0..4（标准 2） | 只用于唤醒；**不计入长度与校验和** |
| 起始符 | 1 | 固定 `0x68`（只有一个） |
| 仪表类型 `T` | 1 | 10..19 水、20..29 热、30..39 气、40..49 其他；读地址时为 `AA` |
| 地址域 | 7 | 压缩 BCD，**A0 是最低字节、线上先传**；14 位十进制含 4 位厂商码 |
| 控制码 | 1 | D7 方向、D6 异常、D5..D0 功能码 |
| 长度 `L` | 1 | 数据域字节数；读 ≤ `0x64`（100），写 ≤ `0x32`（50） |
| 数据域 | L | **不加 `0x33`**；DI 与多字节数值都是低字节先传 |
| 校验和 | 1 | 从 `0x68` 到 DATA 末的模 256 和；前导与结束符不计 |
| 结束符 | 1 | 固定 `0x16` |

帧总长（不含前导）恒为 `13 + L`：`68 + T + 7 地址 + C + L + DATA + CS + 16`。金帧
（读 `00000805000001` 的 901F，SER=0）在代码里逐字节钉住：

```
FE FE 68 10 01 00 00 05 08 00 00 01 03 1F 90 00 39 16
```

三个最容易写错的点：

1. **只有一个 `68`。** 从 645 抄过来的解析器会去找第二个起始符，于是永远停在
   `ErrIncomplete`。
2. **数据域不减 `0x33`。** 多做一次 `-0x33` 会让每个数值都小 51；BCD 半字节检查
   多半还能通过，读出来是一组「合法但错」的数。
3. **CS 不含前导。** 把 `FE` 算进去，所有带前导的帧都会校验失败——而现场的表几乎
   都要求前导。

#### 3.10.11.3 地址与仪表类型

地址是 7 字节压缩 BCD，14 位十进制，**高位在前书写、低字节先传**：

```
点表写法   00000805000001
线序       A0=01 A1=00 A2=00 A3=05 A4=08 A5=00 A6=00
```

其中 `A5`、`A6` 是厂商码。标准允许缩位/通配，但本驱动**要求完整的 14 位**，原因有
两个：

- 188 的 `AA` 只属于「读地址」这条点对点命令。把带 `AA` 的地址用于读数据，会在
  多表总线上变成一次范围不确定的广播，所有表同时应答，主站无法配对；
- 写地址时如果写入通配字节，表号会变成 `AA...`，之后除了广播就再也点不到它。

因此 `ParseAddr` 只接受 14 位数字或 `AA` 通配字节，驱动在 `Open` 与写地址两处都
显式拒绝含通配的地址。

仪表类型 `T` 决定 901F 的字段表：

| T | 含义 | 家族 |
|---|---|---|
| `10`/`11`/`12`/`13` | 冷水 / 生活热水 / 直饮 / 中水水表 | 水表 |
| `20`/`21` | 热量表（计热量 / 计冷量） | 热量表 |
| `30` | 燃气表 | 燃气表（与水表同 schema） |
| `40`..`49` | 其他仪表（如电度表） | 其他（与水表同 schema） |

类型来自设备标签 `labels.type`（默认 `10`）。类型配错的症状不是「读不到」，而是
「读到了但量纲不对」：水表的 `901F:0` 是累积流量，热量表的 `901F:0` 却是结算日
热量。驱动不做「按数值猜类型」这种事后补救——猜错的代价比报错高得多。

#### 3.10.11.4 控制码与数据域

| 功能 | 请求 C | 应答 C | 数据域（请求） | 数据域（应答） |
|---|---|---|---|---|
| 读数据 | `01` | `81` | `DI0 DI1 SER` | `DI0 DI1 SER + 数据` |
| 读地址 | `03` | `83` | `81 0A SER`（字面量） | `81 0A SER`，地址在帧头 |
| 写数据（阀门） | `04` | `84` | `17 A0 SER 55/99` | `17 A0 SER + 状态(2)` |
| 写地址 | `15` | `95` | `18 A0 SER + 新地址(7)` | `18 A0 SER` |
| 异常应答 | — | 请求 C \| `0x80` \| `0x40` | — | `DI0 DI1 SER` |

两个「字面量」必须单独处理，不能走 DI 的低字节先传规则：

- **读地址的报文标识固定 `81 0A`。** 标准与所有实现都按这个顺序写。若套用 `Bytes()`
  的变换会发成 `0A 81`，帧本身合法，但表没有这条命令，表现为超时。
- **写地址的报文标识是 `18 A0`**（即 DI `A018` 的线序），数据域共 `0x0A` 字节：
  2 字节标识 + 1 字节 SER + 7 字节新地址。

异常应答只带标识与 SER，不带错误码：188 没有 645 那样的 `ErrCode` 字节，「为什么
被拒」只能由主站从「命令 + 参数」推断。驱动的错误信息因此保留 DI 与 SER，便于对照
是哪一次请求。

#### 3.10.11.5 DI 与字段 schema

点表里一个 188 点位长这样：

```json
{
  "id": "volume-current",
  "address": { "kind": "register", "area": "901F", "offset": 0, "bit": -1 },
  "dataType": "float64",
  "unit": "m³"
}
```

- `area` 是 4 位十六进制 DI，按**手册顺序**书写；上线时低字节先传（`901F` → `1F 90`）。
- `offset` 是**DI 数据域内的字节偏移**（DI + SER 之后的第一个字节算 0），不是寄存器
  偏移，也不是「第几个字段」。
- `length` 省略时由 schema 推导；显式写出时必须与 schema 完全一致，否则本地报
  `address_error`——长度错一位就会把相邻字段当成这个字段的一部分。

**901F（水表 / 燃气表 / 其他）**，总宽 19 字节：

| offset | 字段 | 宽度 | 编码 |
|---|---|---|---|
| 0 | 当前累积流量 | 4 BCD + 1 单位 | 2 位小数 |
| 5 | 结算日累积流量 | 4 BCD + 1 单位 | 2 位小数 |
| 10 | 实时时间 | 7 | 秒分时日月年低年高 |
| 17 | 状态 | 2 | 小端状态字 |

**901F（热量表）**，总宽 43 字节：

| offset | 字段 | 宽度 | 编码 |
|---|---|---|---|
| 0 | 结算日热量 | 4 BCD + 1 单位 | 2 位小数 |
| 5 | 当前热量 | 4 BCD + 1 单位 | 2 位小数 |
| 10 | 热功率 | 4 BCD + 1 单位 | 2 位小数 |
| 15 | 瞬时流量 | 4 BCD + 1 单位 | 4 位小数 |
| 20 | 累积流量 | 4 BCD + 1 单位 | 2 位小数 |
| 25 | 供水温度 | 3 BCD | 2 位小数，无单位字节 |
| 28 | 回水温度 | 3 BCD | 2 位小数，无单位字节 |
| 31 | 累积工作时间 | 3 BCD | 整数小时，无单位字节 |
| 34 | 实时时间 | 7 | 秒分时日月年低年高 |
| 41 | 状态 | 2 | 小端状态字 |

**D120..D12B** 是上 1..12 月结算日量：水/气/其他为累积流量（m³），热量表为热量
（GJ），均为 4 BCD + 1 单位、2 位小数，总宽 5。`D12C` 不存在。

其余 DI（`911F`、价格、金额、密钥、机电同步等）**不猜**。未收录的 DI 让驱动直接报
`address_error`，并把该 DI 的可用偏移一起列出来；按 901F 的布局硬解出一组「看起来
合理」的值，是比超时更危险的失败模式。

#### 3.10.11.6 值编码：压缩 BCD、时间、单位、状态

**BCD**：低字节在前，每个半字节必须 ≤9。`0xFF` 不是数字 15，而是「无效/未使用」；
把它当 15 会让结算日流量变成一个天文数字。宽度 1..8 字节。

**时间**：7 字节 `ss mm hh dd MM yy_lo yy_hi`，各字段 BCD；年份 =
`BCD(yy_hi)*100 + BCD(yy_lo)`（`0x15 0x20` → 2015），范围 2000..2099。平台侧压成
可排序的 `YYYYMMDDhhmmss` 数值。日期在本地校验：13 月、2 月 30 日、24 时都在发帧
前拒绝，不依赖 `time.Date` 的归一化。

**单位**：字段末尾的 1 字节代号，解码后统一换算到规范单位：

| 量 | 代号 → 系数（到规范单位） |
|---|---|
| 体积（m³） | `2C`→1、`29`(L)→0.001 |
| 流量（m³/h） | `35`→1、`32`(L/h)→0.001 |
| 能量（GJ） | `11`→1、`13`→0.01、`0E`(MJ)→0.001、`0B`(kJ)→1e-6、`01`(J)→1e-9、`05`(kWh)→0.0036、`08`(MWh)→3.6、`02`(Wh)→3.6e-6、`0A`→0.036 |
| 功率（kW） | `17`→1、`14`(W)→0.001、`1A`(MW)→1000 |

**未知单位代号一律报错**，不默认成基准单位。现场见过把 L 写成 `2C` 的批次；默认
等于把 1000 倍偏差静默掉，比读不到更难发现。

**状态字**：2 字节小端 `uint16`。标准定义 D0/D1 为阀门状态（0 开、1 关、3 异常），
D2 为电池欠压；第二字节厂商自定义。驱动只按 `uint16` 交付并附人读文本，不替上层
猜每一位。

#### 3.10.11.7 读：按 DI 缓存、字段切分

`Read` 逐点串行，但**同一个 DI 的多个字段复用一次事务**：一个 span 里放了
`901F:0/5/10/17` 四个点时只发一帧。跨 DI 不合并——188 没有多 DI 批量读，硬合并
只能靠发多帧再拼，收益为零。`Meta().Capabilities.MultiRead` 因此诚实声明
`unsupported`：一轮采集耗时约等于「DI 数 × 单次事务」，这个代价必须让配点表的人
看见。

每次读都校验三件事：

1. 应答地址等于请求地址（总线上可能有另一台表在应答）；
2. DI 与 SER 回显一致（防止把上一次的迟到应答当成这一次的）；
3. 数据域长度等于 schema 推导的总宽（防止表返回旧版/裁剪版布局）。

任一条不满足都归为 `protocol` 错误，并保留原始字节用于排查。链路级故障（超时、
断开）在本轮剩余点上快速失败，不再逐点等超时。

#### 3.10.11.8 写与闸门

188 的写入口只有两个，其余一律本地拒绝：

| 命令 | DI | 值 | 闸门 |
|---|---|---|---|
| 阀门控制 | `A017` | `55` 开 / `99` 关 | `allow-control` |
| 写表地址 | `A018` | 7 字节新地址 | `allow-write` + 完整 14 位 |

- **阀门**：平台值接受 bool 或 0/1，按「1 开、0 关」翻译；其他数值本地拒绝。
- **写地址**：值为字符串，必须是完整 14 位且不含 `AA`。写成功后驱动**自动切到新
  地址**——应答可能来自旧地址，也可能已经来自新地址，两者都接受；若不切换，这条
  连接从下一帧开始就会全部超时。配置本身仍需人工更新，重连后以配置为准。
- **不提供广播**：188 的广播只用于读地址；把写命令广播出去会同时改掉总线上所有表，
  必须走单独的运维流程，不能藏在一条写命令里。

写应答必须是「请求控制码 + `0x80`」，且 DI 与 SER 回显一致；异常位则带出 DI 与 SER
供定位。

#### 3.10.11.9 两种承载

| 承载 | 说明 |
|---|---|
| `serial` | 默认 2400 8E1；经 `internal/driver/link` 使用共享串口层，整条总线级互斥 |
| `tcp` | 串口服务器 / DTU 透传；锁退化为每连接互斥 |

默认前导 2 字节，可由 `labels.options` 的 `preamble=0..4` 覆盖；帧间静默默认 30ms
（`gap=50ms` 可调）。串口与 TCP 的分帧、校验、命令序列完全相同——现场把串口服务器
当透传用时走的就是同一条字节流。

#### 3.10.11.10 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| 2018 版 | DI 字节序、安全传输、新报文都不同；配置期显式拒绝，避免运行时猜版本 |
| 价格/金额/密钥/机电同步 | 厂商差异极大，且不属于「抄表」核心；未收录 DI 直接报错 |
| 广播 | 只保留读地址；写广播会同时改掉总线上所有表 |
| 位寻址 | 状态字按 `uint16` 整体读，由上层按位解释 |
| 缩位地址 | 188 的 `AA` 只属于读地址；带通配的读数据会变成广播 |
| 跨 DI 合并读 | 协议没有多 DI 批量读 |
| DI 自动发现 | 没有「列举数据项」这种服务；DI 必须来自手册/点表 |

`Meta().Capabilities` 里 `Subscribe`、`Browse`、`Discovery`、`MultiRead`、
`BitAccess`、`StructTypes` 全部 `unsupported`，`Read`/`Write` 为 `supported`。

#### 3.10.11.11 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/cjt188`（`Parse`/`Build`/`ReadRequest`/`ReadAddrRequest`/`ValveRequest`/`WriteAddrRequest`/`ParseReadReply`/`ParseAddrReply`/`ParseErrorReply`/`ParseAddr`/`ParseDI`/`Fields`/`LookupField`/`TotalWidth`/`DecodeField`/`ParseTime`/`UnitFactor`/`Reader.Next`） |
| 主站驱动 | `internal/driver/cjt188`（`Meta`/`Open`/`Probe`/`Read`/`Write`，内部 `readDI`/`readPoint`/`resolveRead`/`writeValve`/`writeAddr`/`exchange`/`parseOptions`） |
| 内置从站 | `internal/sim/cjt188`（`NewServer`/`ServeStream`/`SetClock`/`SetValue`/`Value`/`Valve`/`Writes`/`Requests`/`Address`/`Type`） |
| 共享链路 | `internal/driver/link`（serial/tcp 统一建链、`Drain`/`SetDeadline`/总线锁） |
| CLI | `uic cjt188read`（`-probe`/`-di DI[:offset]`/`-write`/`-raw`/`-json`，`serial`/`tcp` 双承载） |
| 示例 | `examples/cjt188.json`（8 点位：当前/结算日流量、时间、状态、上 1 月/上 12 月、阀门与表地址写命令） |

内置从站默认地址 `00000805000001`、类型 `10`，预置：

| DI:offset | 含义 | 值 |
|---|---|---|
| `901F:0` | 当前累积流量 | 1234.56 m³ |
| `901F:5` | 结算日累积流量 | 1200.00 m³ |
| `901F:10` | 实时时间 | 从站时钟（默认 2026-11-07 12:00:00，随 `SetClock` 刷新） |
| `901F:17` | 状态字 | 随阀门状态刷新 |
| `D120`..`D12B` | 上 1..12 月结算日量 | `1200 - n*10` m³ |

自检链路（无硬件，TCP 透传）：

```bash
./uic sim -protocol cjt188 -listen 127.0.0.1:14001 &
./uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 -type 10 \
  -di 901F:0,901F:5,901F:10,901F:17
# 表地址 00000805000001  类型 10（冷水水表）  传输 tcp
#
# DI    偏移  字段            值
# 901F  0     当前累积流量    1234.56 m³
# 901F  5     结算日累积流量  1200 m³
# 901F  10    实时时间        2026-11-07 12:00:00
# 901F  17    状态            阀门开
```

```bash
# 只验物理层（读地址 0x03，不需要点表）：
./uic cjt188read -transport serial -port /dev/ttyUSB0 -baud 2400 \
  -address 00000805000001 -probe

# 原始值（时间紧凑数、状态字十六进制、数据类型）：
./uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 -di 901F:10 -raw

# 写阀门（必须显式 -allow-control）：
./uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 \
  -di A017 -write close -allow-control

# 写表地址（自动打开 allow-write；必须完整 14 位）：
./uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 \
  -di A018 -write 00000805000042

# 带上网关跑真实采集：
./uic -config examples/cjt188.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8094/api/v1/points
```

示例配置里阀门与表地址两个写命令点位默认 `enabled: false`：它们是**写命令**，没有
可读数据域，若默认启用，每个采集周期都会产生一条 `address_error`。要演示写入，把
`enabled` 改为 `true` 再重启网关（写表地址后还要同步修改 `labels.address`，否则下次
启动会按旧地址找不到表）。

排查顺序内建在失败提示里：**波特率/校验位（2400 8E1）→ 14 位表地址 → 前导字节
（`-preamble 0`）→ A/B 接线**。这四步覆盖了 RS-485 上「不通」的绝大多数原因，且每
一步都能用同一条命令单独验证。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/cjt188` | 金帧（读请求/水表应答/读地址）、单 `68`、数据域无 `0x33`、CS 不含前导、地址 BCD 低字节先传、BCD 半字节校验（`FF`/`A` 拒绝）、时间 7 字节往返与非法日期、901F 水/热两套 schema 与总宽（19/43）、D120..D12B、单位换算表与未知代号拒绝、状态字文本、异常应答、噪声恢复、写阀门/写地址帧 |
| `driver/cjt188` | 探针成功/错表拒绝、水表四字段共享一次事务、时间与状态、历史月量、热量表全字段、单位换算、阀门权限与开/关、写地址后连接切换、短地址拒绝、非支持 DI、错误 offset、长度不符、错地址超时、前导 0/4、坏配置拒绝、链路失败短路 |
| `sim/cjt188` | 真实 TCP 回环：标准宽度、地址过滤静默、读地址用真实地址、阀门写与状态刷新、未知命令异常、写地址后新址可读旧地址静默、通配写地址拒绝、`SetClock` 刷新、`SetValue`、`ServeStream` over `net.Pipe`、`Close` 幂等 |
| 点表/规划 | 4 位 DI、字段偏移允许非 0、8 位 DI 与全 `FFFF` 拒绝、bool 拒绝、`WidthProtocol` 策略、不同 DI 不合并 |
| `cmd/uic` | 类型解析、`DI[:offset[:length]]`、偏移越界/长度不符/未收录 DI、历史 DI、status 走 `uint16`、阀门值 open/close、写 DI 只认 A017/A018、传输方式按参数推断 |

### 3.10.12 SNMP v1/v2c（UDP 161）

这是全仓库唯一「按对象名寻址」的协议：地址不是寄存器号，也不是符号表里的变量名，
而是一串点分十进制 OID。它同时带来了三件别处没有的事：

- **协议自带错误语义。** Modbus 用异常码、S7 用返回码，但都没有 SNMP 这么细：
  `noSuchName`、`wrongType`、`notWritable`、`noAccess` 各自指向「点表错」「类型错」
  「权限错」三类完全不同的处置；
- **v1 与 v2c 不是「版本号不同」而已。** v1 没有 per-varbind 异常，一个坏 OID 会让
  整批返回 `noSuchName`，其余 OID 的值也一起丢失；
- **报文跑在 UDP 上。** 没有连接、没有序号确认，只能靠 request-id 配对；超过 MTU
  的报文会被分片，而现场大量设备与防火墙直接丢分片。

这三件事决定了本节的三个核心设计：**只做 v1/v2c**（§3.10.12.1）、**读按字节预算
分批**（§3.10.12.5）、**v1 的 `noSuchName` 逐点重发**（§3.10.12.6）。

#### 3.10.12.1 只做 v1/v2c，v3 在配置期拒绝

| 能力 | v1 | v2c | v3 |
|---|---|---|---|
| 基本 GET / SET | ✓ | ✓ | ✓ |
| 批量 GET（一帧多 varbind） | ✓ | ✓ | ✓ |
| GetBulk（一次取多行） | ✗ | ✓ | ✓ |
| per-varbind 异常标记 | ✗ | ✓ | ✓ |
| 64 位计数器 | ✗ | ✓ | ✓ |
| 扩展错误码（`wrongType` 等） | ✗（只有 0..5） | ✓ | ✓ |
| 认证 / 加密 | ✗ | ✗ | ✓（USM） |
| 团体名（community） | ✓ 明文 | ✓ 明文 | ✗ |

v3 不做，不是「优先级低」，而是它需要另一套完整的子系统：引擎发现、时间同步窗口、
认证与加密密钥的本地化、密钥轮换。把它塞进现有驱动，要么做成半成品（只支持
`noAuthNoPriv`，安全上等价于 v2c 却让人以为已经加密），要么把连接层改成一个带状态
的 USM 会话机。两者都会让「配置一份 SNMP 设备」这件事的复杂度翻倍。

因此 `ParseVersion` 只接受 `1` 与 `2c`，遇到 `3` / `3h` / `v3` 直接报
`unsupported`，并在错误信息里写明「v3/USM 未实现」。**不退回 v2c 碰运气**——用
v2c 的报文去连一个只开 v3 的代理，得到的是超时；现场会把它误判成「网络不通」，
而真正的原因是版本配错。

团体名按凭据对待：`labels.community` 在配置表单里是 `password` 类型（界面掩码），
默认值 `public` 只是「第一次接通最少配置」，不是安全默认。文档与失败提示都要求
现场显式确认它。

#### 3.10.12.2 报文结构：三层 TLV

一条 SNMP 报文是一个 BER 编码的 `SEQUENCE`：

```
Message ::= SEQUENCE {
    version    INTEGER      -- 0 = v1, 1 = v2c
    community  OCTET STRING -- 团体名，明文
    pdu        CHOICE { ... }
}
```

PDU 的标签直接编码在第一个字节里：

| 标签 | PDU | 方向 | 本实现 |
|---|---|---|---|
| `A0` | GetRequest | 请求 | ✓ |
| `A1` | GetNextRequest | 请求 | ✓（仅自检遍历） |
| `A2` | Response | 应答 | ✓ |
| `A3` | SetRequest | 请求 | ✓（单 varbind） |
| `A4` | Trap（v1） | 通知 | 识别后**显式拒绝解析**（不接收 Trap） |
| `A5` | GetBulkRequest | 请求 | 仅内置代理实现；主站不用 |
| `A6`/`A7`/`A8` | Inform / Trap（v2）/ Report | 通知 | 不接收 |

请求与应答的 PDU 头都是三个 `INTEGER`：`request-id`、`error-status`、`error-index`；
`GetBulk` 把后两个换成 `non-repeaters` 与 `max-repetitions`。varbind 列表是一个
`SEQUENCE OF SEQUENCE { OID, value }`。

三条 BER 细节必须按协议写，否则报文「看起来对、设备就是不回」：

1. **长度字段的短/长形式。** 内容 ≤127 字节用一个字节；再长要用 `81 xx` / `82 xx xx`。
   把 130 字节的内容写成单字节 `0x82`，解析方会把它读成一个长度 130 的短形式，
   然后从错误的位置开始找下一个 TLV。
2. **整数是最短二补码。** `127` 是 `7F`，`128` 必须补前导零写成 `00 80`；负数是
   二补码，`-1` 是 `FF`。少写这个前导零，`128` 会被读成 `-128`。
3. **无符号类型同样需要前导零。** Gauge32 / Counter32 / TimeTicks 在线上仍是
   「有符号 INTEGER 的写法」，最高位为 1 时必须补 `00`；`0x80000000` 编码成
   `00 80 00 00 00`，否则对端读出来是负数。这条错误的表现是「计数器突然变成
   负几十亿」，而不是解析失败。

v2c 读 `1.3.6.1.2.1.1.1.0` 的金帧（request-id `0x01020304`，community `public`），
在测试里逐字节钉住：

```
30 29
  02 01 01
  04 06 70 75 62 6c 69 63
  a0 1c
    02 04 01 02 03 04
    02 01 00
    02 01 00
    30 0e
      30 0c
        06 08 2b 06 01 02 01 01 01 00
        05 00
```

注意 varbind 的值是 `NULL`（`05 00`）：GET 请求只带「要读哪个对象」，不带值。

#### 3.10.12.3 OID 与地址

OID 的每个子标识符用 base-128 变长编码，**最高位为 1 表示还有后续字节**。前两弧
合并成 `40 × 第一弧 + 第二弧`：

```
1.3.6.1.2.1.1.1.0  →  2b 06 01 02 01 01 01 00
2.39.1             →  77 01        （40×2+39 = 119 = 0x77）
```

第二行是最容易写错的地方：`119` 的 base-128 编码就是单字节 `0x77`，不是 `0x4F`
（`0x4F` 是 79，对应 `1.39`）。首弧大于 2 的写法在标准里不合法，但现场确实存在；
解析器按「2.(值-80)」宽松接受，**编码器只产生合法值**。

点表里一个 SNMP 点位长这样：

```json
{
  "id": "sys-descr",
  "address": { "kind": "symbol", "path": "1.3.6.1.2.1.1.1.0" },
  "dataType": "string"
}
```

- `path` 必须是点分十进制，**实例号写在 OID 里**（标量的 `.0`、表项的 `.10.1`）。
- `index`（`[3]` 这种数组下标）本地拒绝：SNMP 的「第几行」本身就是 OID 的最后几弧，
  两套下标同时存在会让「`...10[1]` 到底指 `.10.1` 还是 `.1.10`」无法回答。
- `selector`（MQTT 的 `#field`）本地拒绝：SNMP 没有载荷字段这个概念。
- 点表导入用的是 `pointtable.PolicyFor("snmp")` → 只允许 symbol；写成 `holding:100`
  或 `analogInput:1.presentValue` 在导入期就报错，不会等到连上设备才失败。

#### 3.10.12.4 类型系统：线类型由设备决定

同一张表里，不同列可以是完全不同的线类型：`ifDescr` 是 OCTET STRING、`ifType` 是
INTEGER、`ifSpeed` 是 Gauge32、`ifInOctets` 是 Counter32、`ifHCInOctets` 是
Counter64、`sysUpTime` 是 TimeTicks。**点表的 `dataType` 只决定「按什么读」，不能
决定「线上是什么」。**

| 线类型（标签） | 含义 | 读成平台类型 |
|---|---|---|
| `02` INTEGER | 有符号 32 位 | int16 / int32 / int64 / uint* / bool（仅 0/1） |
| `41` Counter32 | 无符号 32 位，单调递增、会回绕 | uint32 / uint64 / int* / bool（仅 0/1） |
| `42` Gauge32 | 无符号 32 位，可增可减 | 同上 |
| `43` TimeTicks | 无符号 32 位，单位 10ms | 同上（不做单位换算） |
| `44` Opaque | 厂商自定义字节 | string / bytes |
| `46` Counter64 | 无符号 64 位 | uint64 / int* |
| `04` OCTET STRING | 字节串 | string / bytes |
| `05` NULL | 无值 | 数值型报错 |
| `06` OBJECT IDENTIFIER | OID | string |
| `40` IpAddress | 4 字节地址 | string |
| `80`/`81`/`82` | noSuchObject / noSuchInstance / endOfMibView | 地址错误 |

两条硬规则：

1. **数值点位收到 OCTET STRING 必须报错，不能硬转 0。** 现场最常见的错法是「拿
   `sysDescr` 的 OID 配了一个 `uint16`」，或者把 MAC 地址列当成流量。错误信息直接
   提示「把 dataType 改成 string/bytes」。
2. **bool 只接受 0/1。** 收到 `2` 或 `72` 时报协议错误，而不是 `!= 0` 一律为真。
   `ifAdminStatus` 的 `2` 是 down、`3` 是 testing，硬转成 bool 会把「testing」
   显示成「up」。

**Counter64 的精度边界**：平台的数值通道是 `float64`，尾数 53 位。超过 `2^53` 的
Counter64 经转换会丢低位。这不是实现疏漏，而是数值模型决定的；文档明确写出，
提醒调用方不要把大计数器当精确值做差值。Counter32 会在 `2^32` 回绕，时间序列侧
必须按「回绕检测」处理，驱动不做猜测性补偿。

#### 3.10.12.5 读：一条 GET 带多个 varbind，按双预算分批

`Read` 把所有 span 里的点位摊平，解析 OID，然后分批发 GET。分批受两个上限约束：

| 约束 | 默认 | 可调 | 目的 |
|---|---|---|---|
| 单帧 OID 个数 | 32 | `options=maxOids=1..128` | 防御性上限，避免一帧塞几百个 |
| 单帧编码字节 | 1400 | 固定 | 覆盖 1500 MTU，避免 IP 分片被中间设备丢弃 |

字节预算是**按 OID 编码长度的保守上界**估算的（每子标识符 5 字节 + TLV 头），
不是按字符串长度。一个 30 弧的 OID 字符串只有 60 多个字符，编码后加上长度前缀接近
160 字节；按字符数估会低估一半以上。

为什么字节预算比个数上限更重要：现场症状是「OID 少的时候能读，点表一多就超时」。
设备其实回了，只是应答被分片，防火墙或负载均衡丢了后续分片。把请求控制在 1400
字节以内，响应通常也在同一个量级。真实编码长度在测试里复核：每条发出的请求都会
重新编码一次并断言 ≤1400。

批内逐点解码，每条校验三件事：

1. 应答 varbind 的 OID 与请求一致（防止代理乱序或回错对象）；
2. 值是异常标记时归为 `address`（v2c 的 `noSuchObject` / `noSuchInstance`）；
3. 线类型能被点表 `dataType` 接受（§3.10.12.4）。

链路级失败（超时、断链）会让**本轮剩余批次全部快速失败**，不再逐批等超时；地址级
错误只标记该点，不影响其它点位的质量码，也不进入 `Read` 的汇总错误。

`Meta().Capabilities.MultiRead` 声明 `supported`——这里的「多读」是协议真实支持的
「一条 GET 带多个 varbind」，与 645/188 的「一次只能问一个数据标识」相反。

#### 3.10.12.6 v1 的 `noSuchName` 必须逐点重发

v1 没有 per-varbind 异常。批量 GET 里只要有一个 OID 不存在，代理返回
`error-status = noSuchName`、`error-index = 第几个`，但**其余 OID 的值也一起不返回**。

如果驱动把整批都标成地址错误，现场会看到「同一批点位昨天还好，今天全红」——实际上
只是其中一个 OID 写错了。因此 v1 收到 `noSuchName` 后：

```
批量 GET（N 个 OID） → noSuchName
  ├─ 单点 GET OID[0] → 值 / 地址错误
  ├─ 单点 GET OID[1] → 值 / 地址错误
  └─ ...
```

逐点重发把「哪一个不存在」与「其余的值」重新分开。代价是 N 次额外事务，但只在
「v1 + 批内有坏 OID」这条路径上发生；正常批次仍然是一条 GET。v2c 不需要这条路径：
异常标记就在对应 varbind 的值位置上。

`Probe` 也利用这一点：读 `sysDescr.0`（system 组的第一项，所有代理都必须实现），
**收到任何应答都算链路通**，包括 `noSuchObject`。它证明报文到达、community 被接受、
代理回了结构正确的应答；真正失败只有「没有任何报文」或「报文结构错」。超时提示
直接写排查顺序：community → 版本 → 地址/端口 → 视图 ACL。

#### 3.10.12.7 写：单 OID SET，类型映射与回显校验

`Write` 对每个点位发一条 SET，**不把多个点位合并进一条 SET**。理由不是协议不支持
（SET 支持多 varbind 且原子），而是失败语义：一条 SET 里第二个 varbind 只读，整条
都会被拒，第一个也不生效；把互不相关的点位捆在一起，会让「写 A 成功、写 B 失败」
这种现场最常见的部分成功变成「全失败」，排查时无法定位。逐点发，每个点位拿到自己
的错误码。

平台类型到线类型的映射：

| 平台 `dataType` | 线类型 | 说明 |
|---|---|---|
| `bool` | INTEGER | `true` → 1，`false` → 0 |
| `int16` / `int32` / `int64` | INTEGER | 范围检查；小数直接拒绝 |
| `uint16` / `uint32` | Gauge32 | 无符号的可写类型 |
| `uint64` | Gauge32 | 仅 ≤ `2^32-1`；更大的值没有标准可写类型 |
| `string` | OCTET STRING | UTF-8 原样 |
| `bytes` | OCTET STRING | 不做 UTF-8 规范化 |
| `float32` / `float64` | — | 显式拒绝：没有标准浮点类型 |
| `bcd16` / `bcd32` | — | 显式拒绝：没有 BCD 类型 |

浮点之所以拒绝而不是「按私有编码猜一个」：各厂商把浮点塞进 OCTET STRING 的字节序、
长度、是否带指数位都不一样。猜错的结果是设备接受了一个错误的值，比写入被拒更难
发现。OID 与 IpAddress 两种线类型没有对应的平台 `dataType`，因此只读不写。

应答校验两步：

1. `error-status` 必须为 0；否则按 §3.10.12.8 分类；
2. 应答必须回显被写的 OID **与值**。回显值不一致时报 `protocol`——这能把「设备
   接受了请求但按自己的解释改了值」（例如把 1 收敛成 100）当场暴露，而不是等下
   一轮采集才发现值不对。

#### 3.10.12.8 错误分类：error-status 与异常标记

分类决定「要不要重试、要不要熔断、质量码是什么」。SNMP 的错误码比其它协议多，
必须逐个映射，不能一律 `protocol`：

| error-status | 分类 | 可重试 | 说明 |
|---|---|---|---|
| `tooBig` | protocol | ✓ | 响应太大；下一轮应减小批量 |
| `genErr` | protocol | ✓ | 代理内部错误，值得再试一次 |
| `commitFailed` / `undoFailed` | protocol | ✓ | SET 的两阶段提交失败 |
| `noSuchName` | address | ✗ | 对象不存在（v1 的批量语义见 §3.10.12.6） |
| `badValue` / `wrongType` / `wrongLength` | address | ✗ | 值/类型与对象定义不符 |
| `wrongEncoding` / `wrongValue` | address | ✗ | 编码或取值不在允许集合内 |
| `inconsistentValue` / `inconsistentName` | address | ✗ | 依赖对象状态，当前值不允许 |
| `readOnly` / `notWritable` | permission | ✗ | 对象只读 |
| `noAccess` / `noCreation` | permission | ✗ | 视图不允许 / 不允许创建行 |
| `authorizationError` | permission | ✗ | 代理拒绝该操作 |
| `resourceUnavailable` | permission | ✓ | 资源暂时不足 |

异常标记（v2c）全部归 `address`：`noSuchObject`、`noSuchInstance`、
`endOfMibView` 都表示「这个 OID 在这台设备上不存在」，是点表问题，不是链路问题。
把它们归成 `protocol` 会让一台设备因为一个坏 OID 被熔断。

分类结果直接影响质量码：`timeout` → `timeout`、`address` → `address_error`、
`disconnected` → `comm_error`、其余 → `bad`。

#### 3.10.12.9 UDP 事务：request-id、重试与陈旧应答

UDP 没有连接，请求与应答靠 `request-id` 配对。驱动把整条会话串行化（`mu`），原因
与 BACnet 相同：一个套接字上同时挂两个未完成事务时，迟到的应答会张冠李戴。

| 情况 | 处置 |
|---|---|
| 收到 request-id 不匹配的 Response | 丢弃，继续等当前事务 |
| 收到 Trap / 其它 PDU | 丢弃（不接收 Trap） |
| 超时 | 重发，最多 `retries` 次（默认 2，可配 0..10） |
| 非超时错误（协议/断链） | 立即上报，不重发 |
| 重试耗尽 | `timeout`，提示检查 community 与视图 ACL |

「陈旧应答」不是理论问题：上一轮超时后，代理可能在一秒后才把应答发出来；下一轮
请求刚好用了新的 request-id，如果驱动不校验，就会把上一轮的值当成这一轮的。
`request-id` 从 1 开始递增、回绕时跳过 0，保证同一连接上不会复用尚未过期的编号。

单次事务的截止时间取「通道超时」与「context deadline」的较早者。`Close` 只关闭
套接字；重连由上层调度按 `disconnected` 处理，驱动不内置重连循环——那会让「设备
真的离线」与「驱动在不停重试」这两种状态无法区分。

#### 3.10.12.10 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| v3 / USM | 另一套认证加密与密钥管理；配置期显式拒绝，不退回 v2c |
| Trap 接收 | 轮询主站的采集语义与「设备主动推」不同；v1 Trap 结构特殊，解析层直接拒绝 |
| MIB 编译 / 名字解析 | 需要一整套 MIB 文件与解析器；点表只认数字 OID |
| GetBulk 作为生产读 | 语义是「按字典序连续取」，与点表的「明确 OID」相反；只在自检遍历里用 GETNEXT |
| 多 varbind SET | 失败会整条回滚；逐点写才能给出逐点错误 |
| 自动发现对象 | 没有「列举设备对象」这种服务；OID 必须来自手册/点表 |
| 浮点 / BCD 写 | 没有标准线类型，私有编码互不兼容 |
| 位寻址 | 位操作没有标准语义；数值整体读出后由上层解释 |

`Meta().Capabilities` 里 `Subscribe`、`Browse`、`Discovery`、`BitAccess`、
`StructTypes` 全部 `unsupported`，`Read`、`Write`、`MultiRead` 为 `supported`。

#### 3.10.12.11 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/snmp`（`Parse`/`Build`/`GetRequest`/`GetNextRequest`/`SetRequest`/`GetBulkRequest`/`ParseOID`/`OID.Compare`/`OID.HasPrefix`/`ParseVersion`/`ParseOIDList`/`ErrorStatus.Kind`） |
| 主站驱动 | `internal/driver/snmp`（`Meta`/`Open`/`Probe`/`Read`/`Write`，自检用 `GetRaw`/`Walk`；内部 `parseOptions`/`readBatch`/`readOneByOne`/`decodeValue`/`encodeWriteValue`/`statusError`/`exchange`） |
| 内置代理 | `internal/sim/snmp`（`NewServer`/`NewServerWithOptions`/`Addr`/`Get`/`Set`/`SetWritable`/`Requests`/`Writes`/`Close`） |
| CLI | `uic snmpread`（`-probe`/`-oid`/`-walk`/`-max-walk`/`-set`/`-setType`/`-retries`/`-raw`/`-json`） |
| 示例 | `examples/snmp.json`（9 点位：sysDescr/UpTime/Name/Services、可写 sysContact/sysLocation、ifNumber、ifInOctets、ifAdminStatus） |

内置代理默认 community `public`，预置：

| OID | 含义 | 值 | 可写 |
|---|---|---|---|
| `1.3.6.1.2.1.1.1.0` | sysDescr | `UIC SNMP agent (simulated)` | ✗ |
| `1.3.6.1.2.1.1.2.0` | sysObjectID | `1.3.6.1.4.1.99999.1` | ✗ |
| `1.3.6.1.2.1.1.3.0` | sysUpTime | TimeTicks `123456`（固定值，不随墙钟增长） | ✗ |
| `1.3.6.1.2.1.1.4.0` | sysContact | `ops@example.invalid` | ✓ |
| `1.3.6.1.2.1.1.5.0` | sysName | `uic-sim` | ✓ |
| `1.3.6.1.2.1.1.6.0` | sysLocation | `lab` | ✓ |
| `1.3.6.1.2.1.1.7.0` | sysServices | INTEGER `72` | ✗ |
| `1.3.6.1.2.1.2.1.0` | ifNumber | INTEGER `2` | ✗ |
| `1.3.6.1.2.1.2.2.1.{1,2,3,4,5,6,8,10,16}.{1,2}` | ifTable 各列 | 两块网卡 | ✗ |
| `1.3.6.1.2.1.2.2.1.7.{1,2}` | ifAdminStatus | INTEGER `1` | ✓ |

sysUpTime 故意不随墙钟增长：测试需要一个稳定的值，会自己变的计数器会让断言变成
`sleep` 赌运气。真实设备当然会增长；这一差异只影响「演示时看到固定的 123456」，
不影响任何报文语义。

community 不匹配时**静默丢弃**，不回 `authorizationError`——真实代理就是这样，
避免成为团体名探测的预言机；这也让「超时 → 先查 community」这条排查建议可以在
仿真设备上复现。v1 与 v2c 的差异在代理里同样保留：缺 OID 时 v1 回 `noSuchName`
（带 `error-index`），v2c 在对应 varbind 回 `noSuchObject`；SET 的只读/类型错误
分别映射到 v1 的 `readOnly`/`badValue` 与 v2c 的 `notWritable`/`wrongType`。
SET 是原子的：先全部校验，再统一落地。

自检链路（无硬件，UDP 回环）：

```bash
./uic sim -protocol snmp -listen 127.0.0.1:16161 &
./uic snmpread -addr 127.0.0.1:16161 -probe
# 链路正常: 127.0.0.1:16161 版本 2c（GET sysDescr.0 收到应答）

./uic snmpread -addr 127.0.0.1:16161 -raw
# OID                 类型          值                     质量
# 1.3.6.1.2.1.1.1.0   OCTET STRING  UIC SNMP agent (…)     good
# 1.3.6.1.2.1.1.3.0   TimeTicks     123456                 good
# 1.3.6.1.2.1.1.5.0   OCTET STRING  uic-sim                good
# 1.3.6.1.2.1.1.6.0   OCTET STRING  lab                    good

./uic snmpread -addr 127.0.0.1:16161 -walk 1.3.6.1.2.1.2.2.1 -max-walk 12
./uic snmpread -addr 127.0.0.1:16161 -version 1 -oid 1.3.6.1.2.1.1.1.0,1.3.6.1.2.1.1.99.0
./uic snmpread -addr 127.0.0.1:16161 -oid 1.3.6.1.2.1.1.6.0 -set lab-2 -setType string
```

`-walk` 从子树前缀做 GETNEXT：v1 在表尾按 `noSuchName` 正常结束，v2c 按
`endOfMibView` 结束；代理返回不前进的 OID 时报协议错误并停止，避免死循环。
`-raw` 显示线类型与十六进制原始字节——MAC 地址、Opaque 这类值必须看原始字节，
按字符串打印会把不可打印字节变成问号。

示例配置带上网关跑真实采集：

```bash
./uic -config examples/snmp.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8095/api/v1/points
curl -s -X POST -H 'Authorization: Bearer dev-token-0123456789' \
  -H 'Content-Type: application/json' \
  -d '{"value":{"type":"string","text":"building-7"}}' \
  localhost:8095/api/v1/points/switch01/sys-location/write
```

写接口返回 `quality: uncertain` 是预期：写入已确认（设备回显一致），但缓存里的值要等
下一轮采集才刷新；把它标成 `good` 会让人以为「读到的新值就是刚写的值」。

排查顺序内建在失败提示里：**community（读写口令是否分开）→ 版本（v1 代理不认 v2c
报文）→ 地址/端口（是否被 ACL 限制）→ 视图是否允许该子树 → OID 是否少写/多写实例号**。
前四步用 `-probe` 与 `-oid` 就能分开，最后一步用 `-walk` 看设备上真实的 OID 长什么样。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/snmp` | GET/应答金帧、INTEGER 最短编码与负数、无符号前导零（`0x80000000`）、OID base-128（`2.39.1`→`77 01`）、OID 排序与前缀、非法 OID、版本解析（v3 拒绝）、异常标记只允许出现在应答、GetBulk 线格式、错误码分类、字符串/字节值、截断错误可识别 |
| `driver/snmp` | 五种线类型解码、MultiRead 一条 GET、按 maxOids 与字节预算分批（顺序不变、每条 ≤1400 字节）、v1 `noSuchName` 逐点重发、写类型映射与回显校验、类型/范围拒绝不发包、9 种 error-status 分类、SET 回显被收敛、Probe 接受异常标记/超时提示、request-id 陈旧应答丢弃、链路失败停止后续批次、坏地址与线类型不符、Counter64 精度边界、`GetRaw`/`Walk`（子树边界、max、v1 表尾） |
| `sim/snmp` | 真实 UDP 回环：GET 与计数器、错 community 静默、v1/v2c 缺 OID 差异、GETNEXT 遍历与表尾、GETBULK 三行递增、SET 可写与回读、只读/类型错/不存在、SET 原子性、坏报文与非请求丢弃、`Set`/`SetWritable`/`Close` 幂等 |
| 点表/规划 | `PolicyFor("snmp")` 只允许 symbol、selector/寄存器/对象地址在导入期拒绝、`examples/snmp.json` 通过 `-check`（17 个驱动） |
| `cmd/uic` | `-set` 值解析（bool/int/uint/string/bytes、进制、越界、float/BCD 拒绝）、`snmpRow` 的 raw/error 显示 |

### 3.10.13 ADS/AMS（TCP 48898）

这是全仓库唯一「地址就是 PLC 工程里的变量名、且访问分两步」的驱动。它与前面所有
协议的区别不只是报文格式：

- **地址不是位置，而是符号。** Modbus 的 `holding:100` 是「第 100 个寄存器」，
  S7 的 `db1:0` 是「DB1 的第 0 个字节」；ADS 的 `MAIN.nCounter` 是 PLC 工程里
  声明的一个变量，宽度与类型由声明决定。写错名字得到的是 `symbol not found`，
  写错宽度得到的是 `invalid size`——两者都不是「读到一个错值」，而是明确拒绝。
- **一次取值要两次往返。** 先把变量名换成句柄（`ReadWrite` + `IG=0xF003`），
  再用句柄读值（`Read` + `IG=0xF005`）。句柄属于会话，PLC 重启后全部失效。
- **寻址层在 IP 之上还有一层 AMS。** `labels.netid` 是 6 段点分十进制的 AMS
  NetId，`labels.port` 是 AMS 端口（851/801），TCP 端口固定 48898。TCP 能连上
  只证明 48898 在监听，NetId/端口错要到第一条命令才会暴露。
- **全线小端。** 长度、错误码、句柄、数值都是 little-endian；把大端字节直接喂进来，
  设备不会报错，只会给出一个不同的数。

因此本节的三个核心设计是：**只做 AMS/TCP 上的符号读写**（§3.10.13.1）、
**句柄缓存 + 失效重解析一次**（§3.10.13.4）、**逐点事务，不合并多句柄**（§3.10.13.5）。

#### 3.10.13.1 范围：AMS/TCP 主站，不做通知与浏览

| 能力 | 本驱动 | 说明 |
|---|---|---|
| 按符号名读 | ✓ | `ReadWrite(0xF003)` 取句柄 → `Read(0xF005)` |
| 按符号名写 | ✓ | 取句柄 → `Write(0xF005)`；写前做类型/范围校验 |
| 设备信息 | ✓ | `ReadDeviceInfo`，`Open` 时用作握手 |
| 运行时状态 | ✓ | `ReadState` / `WriteControl`（Run/Stop/Reset） |
| 通知订阅 | ✗ | `AddDeviceNotification` 的句柄生命周期、失效重注册与流式采样是另一套模型 |
| 符号表浏览 | ✗ | 符号表上传格式随运行时版本变化；现场点位本来就来自变量表 |
| 跨 AMS 路由发现 | ✗ | 路由是设备侧配置；驱动只按给定 NetId 直连 |
| 一次多句柄读写 | ✗ | 失败无法逐点归因，且不同运行时的合包语义有差异 |
| 位变量 / 结构体类型 | ✗ | 位在符号层不单独暴露；结构体按字节数组或逐成员声明 |

`Meta().Capabilities` 里 `Read`、`Write` 为 `supported`，`Subscribe`、`Browse`、
`Discovery`、`MultiRead`、`BitAccess`、`StructTypes` 全部 `unsupported`。

`MultiRead` 声明 `unsupported` 是刻意的：ADS 的 `ReadWrite` 可以带多个句柄，
但一次失败就不知道是哪个变量、哪一类错误；逐点事务换来的是「哪个点位错、错在
哪一类」。这和 645/188 的「协议只能一次一个 DI」不同，是主动放弃的优化，不是
协议限制。

#### 3.10.13.2 寻址：变量名就是地址

点表里的地址写法：

```text
MAIN.nCounter                 标量
MAIN.arr[2]                   数组元素（下标属于名字）
MAIN.sRecipe:81               STRING(80)，显式长度 81（含结尾 NUL）
MAIN.arrBytes:4               BYTE[4]
```

规则：

- `address.kind` 必须是 `symbol`，`path` 是 PLC 符号表里的完整路径；结构体成员
  逐级写全（`MAIN.stStatus.bReady`）。
- **数组下标写在名字里**。`Model.Address.Index` 是平台给 MQTT/OPC UA 一类协议准备
  的二级下标；ADS 的变量名本身就是 `MAIN.arr[2]`，拆成 `path=MAIN.arr, index=2`
  再拼回去会让「`[0]` 与未配置」在 JSON 里无法区分。因此
  `pointtable.PolicyFor("ads")` 使用 `SymbolIndexInPath`，解析时保留下标；
  `Address.Index > 0` 由驱动直接报 `address` 错误。
- **string/bytes 的宽度由点表给出**。PLC 变量的宽度是符号的属性，驱动猜不出来：
  STRING(80) 占 81 字节，BYTE[4] 占 4 字节。写法有两种：地址里 `NAME:81`，或
  CSV 的 length 列；两者同时给且不一致时在导入期报错。省略时用
  `options.strlen`（默认 81）。
- `selector`（MQTT 的 `#field`）在导入期拒绝：ADS 没有「载荷里的字段」这个概念。
- 写成 `holding:100` 这类寄存器地址同样在导入期拒绝；绕过导入直接构造
  `AddressRegister` 时，驱动在发报文前报 `address`。

对应 `AddressPolicy`：

```go
PolicyFor("ads") = AddressPolicy{
    AllowSymbol:       true,
    AllowSymbolLength: true,   // NAME:81 或 length 列
    SymbolIndexInPath: true,   // MAIN.arr[2] 的 [2] 属于 path
}
```

#### 3.10.13.3 线格式：6 字节 TCP 头 + 32 字节 AMS 头 + ADS 载荷

```text
AMS/TCP 头（6）
  reserved uint16 = 0
  length   uint32 = len(AMS 头 + 载荷)

AMS 头（32）
  target NetId(6) + target port(2)
  source NetId(6) + source port(2)
  command(2) + flags(2)
  length(4)        仅载荷长度
  error(4)         AMS 层错误码，0 = 正常
  invoke(4)        请求/应答配对号

ADS 载荷
  Read          IG(4) offset(4) length(4)
  Read 应答     result(4) length(4) data
  Write         IG(4) offset(4) length(4) data
  Write 应答    result(4)
  ReadWrite     IG(4) offset(4) readLen(4) writeLen(4) data
  ReadWrite 应答 同 Read
  ReadDeviceInfo 应答 result(4) major(1) minor(1) version(2) name(16, NUL 填充)
  ReadState    应答 result(4) adsState(2) deviceState(2)
  WriteControl 请求 adsState(2) deviceState(2) length(4) data；应答 result(4)
```

命令号：1 `ReadDeviceInfo`、2 `Read`、3 `Write`、4 `ReadState`、5 `WriteControl`、
6 `AddDeviceNotification`、7 `DeleteDeviceNotification`、8 `DeviceNotification`、
9 `ReadWrite`。本驱动只发送 1/2/3/4/5/9；收到 8 时静默丢弃（同一 NetId 的其它
客户端可能仍有历史订阅），6/7 从不发送。

长度有两层且必须一致：TCP 头的 length 是「AMS 头 + 载荷」，AMS 头的 length 只是
「载荷」。少写一层会让对端把载荷开头当成下一条报文的长度。解析层对三层长度、
TCP reserved、载荷长度逐项校验；任一不符都归类为 `protocol`，不尝试「尽量解析」。

flags 的最低位是应答位（请求 0、应答 1），`0x0004` 表示载荷是 ADS 命令。所有请求
都带 ADS 位；应答必须同时带应答位与 ADS 位，且 command 与 invoke 都与请求一致。
invoke 由驱动递增分配，0 保留为「未分配」，回绕时跳到 1。

#### 3.10.13.4 两步访问：变量名 → 句柄 → 值

按名字取句柄：

```text
ReadWrite(IG=0xF003, offset=0, readLen=4, writeLen=len(name)+1, data=name+"\0")
→ result(4) length(4)=4 data=handle(4, 小端)
```

变量名必须以 NUL 结尾。少了这个字节，PLC 会把符号表里相邻的内存当成名字的一部分，
得到的可能是 `symbol not found`，也可能是另一个变量——后者更危险。

按句柄读写：

```text
Read (IG=0xF005, offset=handle, length=n)          → result length data
Write(IG=0xF005, offset=handle, length=n, data)    → result
```

释放句柄用 `Write(IG=0xF006, offset=0, length=4, data=handle)`。驱动**不在
`Close` 时批量释放**：句柄属于会话，TCP 断开后由运行时回收；对端已经不可达时，
每个 Release 都要等一次超时，会把退出时间拖长一个数量级。

句柄按变量名缓存在连接内。读或写返回 `invalid offset (0x0703)` / `invalid group
(0x0702)` 时，视为「PLC 重启导致句柄失效」：清掉该条目、重新按名字解析一次，再
重试原操作。**只重试一次**——第二次还失败就是变量本身的问题，继续重试只会把
「PLC 一直重启」变成「驱动一直转圈」。`invalid size` 不触发重解析：那是长度配错，
重新取句柄不会让它变好。

#### 3.10.13.5 类型与字节序：小端、宽度由声明决定

| 平台类型 | 线上宽度 | 说明 |
|---|---|---|
| `bool` | 1 | PLC 的 BOOL 在符号层占 1 字节；只接受 0/1，其它值报协议错 |
| `int16` / `uint16` | 2 | 小端补码 |
| `int32` / `uint32` | 4 | 小端 |
| `int64` / `uint64` | 8 | 小端 |
| `float32` | 4 | IEEE-754，小端 |
| `float64` | 8 | IEEE-754，小端 |
| `string` | 点表长度或 `strlen` | 写入时右侧补 NUL；字符串刚好占满整段时拒绝（没有结尾 NUL） |
| `bytes` | 点表长度或 `strlen` | 写入时右侧补 0；允许刚好占满 |
| `bcd16` / `bcd32` | — | ADS 没有 BCD 线类型，读写在发报文前报 `unsupported` |

数值用 `codec/value` 的 `Encode`/`Decode` 配合 `model.OrderDCBA`：`DCBA` 表示
「整体倒序」，对一个纯小端数值等价于不做字内交换。把这段逻辑放在统一的 value 层，
是为了保证 ADS 与 Modbus/S7 对同一个字节串的解释只在字节序上不同，而不是各写一份
符号扩展。

写入前做范围检查（`checkRange`）：`70000` 写进 INT16 会被强制转换静默变成 `4464`，
写入「成功」了，但设备上的值已经不是调用方给的值。这类错误比写入失败更难发现，
必须在发报文之前拦下。整型的非整数值（`1.5`）同样拒绝。

#### 3.10.13.6 错误分类：AMS 与 ADS 两套码

AMS 头里的 error 表示「报文没有被目标处理」；ADS 载荷里的 result 表示「命令执行
结果」。两套码数值范围不重叠，共用一套分类：

| 范围 | 例子 | 分类 | 重试 |
|---|---|---|---|
| AMS 0x0001..0x0018 | `0x0004` mailbox full、`0x0006` target port not found、`0x0007` target machine not found、`0x0016` AMS timeout | 资源/超时类 → `protocol`/`disconnected`/`timeout`；端口/NetId → `address` | 只重试临时性错误 |
| ADS 0x0700..0x072F | `0x0702` invalid group、`0x0703` invalid offset、`0x0704` invalid access、`0x0705` invalid size、`0x0710` symbol not found | 地址/权限/不支持/协议 | 不重试 |
| 其它 | 未知码 | `protocol`，保留原值 | 不重试 |

最常踩的几个码直接在错误信息里给了排查动作：

| 码 | 含义 | 提示 |
|---|---|---|
| `0x0006` | target port not found | 检查 `labels.port`（851 第一个运行时 / 801 第二个）与 PLC 是否在运行 |
| `0x0007` | target machine not found | AMS 路由找不到目标 NetId，检查 `labels.netid` 与设备侧路由 |
| `0x0702` / `0x0703` | invalid group / offset | 句柄失效（PLC 重启）或 IndexGroup 不被支持 |
| `0x0704` | invalid access | 符号在 PLC 侧不可写（需要 VAR_GLOBAL 且允许读写） |
| `0x0705` | invalid size | 读长度与变量宽度不符：STRING 用 `options.strlen`，bytes 用点位 length |
| `0x0710` | symbol not found | 符号名区分大小写；数组写 `NAME[0]`；结构体成员逐级写全 |

错误分三类处置：地址/权限/不支持只影响当前点位，不进入整轮 `firstErr`，也不熔断
设备；超时/断链/协议错会停止本轮剩余点位（链路已经不可继续）。这条区分让「一个
变量名写错」不会把整台 PLC 标红。

#### 3.10.13.7 标签与握手

| 标签 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `netid` | ✓ | — | 目标 AMS NetId，6 段 0..255 点分十进制 |
| `port` | | 851 | 目标 AMS 端口；851/801 是两个 PLC 运行时 |
| `localnetid` | | `127.0.0.1.1.1` | 本地 AMS NetId；设备做了白名单/路由时必须匹配 |
| `localport` | | 32905 | 本地 AMS 端口；只需本机唯一 |
| `options` | | `retries=1,strlen=81` | `retries=0..10`；`strlen=1..65535` |

`transport.kind` 必须是 `tcp`，`transport.address` 是 `host:port`（省略端口按
48898）。TCP 端口不是 AMS 端口：把 851 填进 `transport.address` 会连到一个没有
监听的位置；把 48898 填进 `labels.port` 会得到 `target port not found`。配置表单
把两者分成两个字段，就是为了让这个区别在界面上可见。

`Open` 在 TCP 连接建立后立即做一次 `ReadDeviceInfo` 握手。握手不是可选的：TCP
能连上只说明 48898 在监听，NetId/AMS 端口配错要等到第一次读才会暴露。把失败提前
到启动期，运维第一时间看到的是「target port not found」而不是一条条红色的点位。
握手超时的错误信息会同时提示检查 `transport.address`、`labels.netid`、
`labels.port` 与 AMS 路由。

#### 3.10.13.8 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| 通知订阅 | 句柄生命周期、失效重注册、流式采样是另一套模型；采集侧是拉取语义 |
| 符号表上传 / 浏览 | 格式随运行时版本变化；现场点位来自变量表，不需要驱动再发现一次 |
| 跨 AMS 路由发现 | 路由是设备侧配置；驱动只按给定 NetId 直连 |
| 多句柄合并读写 | 失败无法逐点归因；逐点事务换来准确的错误定位 |
| 写后回读校验 | ADS 的 Write 应答只有 result，没有回显；回读是额外一次事务，由上层决定 |
| BCD / 位寻址 / 结构体自动展开 | 没有对应线类型或符号层语义；用 uint/bytes/逐成员声明表达 |
| OPC DA 桥接 | 需要 DCOM/COM，零依赖 Go 不可实现；应作为独立的 Windows 桥接进程，不在本驱动范围内 |

#### 3.10.13.9 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/ads`（`NetID.ParseNetID`、`Frame.Encode/Decode`、`FrameLen`、`Peer` 的 7 类请求构造、`ParseRead/ParseWrite/ParseReadWrite/ParseReadDeviceInfo/ParseReadState/ParseWriteControl`、`Code.Kind/Temporary`） |
| 主站驱动 | `internal/driver/ads`（`Meta`/`Open`/`Probe`/`Read`/`Write`，自检用 `DeviceInfo`/`DeviceState`/`WriteControl`；内部 `parseOptions`/`exchange`/`readFrame`/`handleLocked`/`checkRange`） |
| 内置服务端 | `internal/sim/ads`（`NewServer`/`NewServerWithOptions`/`Addr`/`NetID`/`Port`/`Get`/`SetValue`/`SetWritable`/`Variables`/`InvalidateHandles`/`Requests`/`Writes`/`Close`） |
| CLI | `uic adsread`（`-probe`/`-info`/`-state`/`-control`/`-allow-control`/`-symbol`/`-type`/`-strlen`/`-set`/`-setType`/`-netid`/`-port`/`-local-netid`/`-local-port`/`-retries`/`-json`/`-timeout`） |
| 示例 | `examples/ads.json`（9 点位：INT16/REAL/LREAL/BOOL/STRING/数组元素，其中 speed/setpoint/run/name/arr[2] 可写） |

内置服务端默认 NetId `127.0.0.1.1.1`、AMS 端口 851、状态 Run，预置：

| 符号 | 类型 | 初值 | 可写 |
|---|---|---|---|
| `MAIN.nCounter` | INT16 | 1234 | ✗ |
| `MAIN.nSetpoint` | INT16 | 500 | ✓ |
| `MAIN.fTemperature` | REAL | 23.5 | ✗ |
| `MAIN.fSpeed` | LREAL | 1450.75 | ✓ |
| `MAIN.bEnable` | BOOL | 1 | ✗ |
| `MAIN.bRun` | BOOL | 0 | ✓ |
| `MAIN.sRecipe` | STRING(80) | `batch-42` | ✗ |
| `MAIN.sName` | STRING(80) | `uic-sim` | ✓ |
| `MAIN.arr[0..3]` | INT16 | 10 / 20 / 30 / 40 | 仅 `[2]` 可写 |
| `MAIN.arrBytes` | BYTE[4] | `01 02 03 04` | ✗ |

变量表是扁平的，每个数组元素单独占一个符号名（与真实符号表一致）。句柄全局编号
而不是按连接隔离：测试要能一调用就让所有句柄失效，模拟 PLC 重启；真实设备重启后
旧句柄同样全部失效，按句柄读写返回 `invalid offset`。代理不实现符号表上传与通知
订阅，未知 IndexGroup 一律回 `invalid group`。

自检链路（无硬件，TCP 回环）：

```bash
./uic sim -protocol ads -listen 127.0.0.1:48898 &
./uic adsread -addr 127.0.0.1:48898 -probe
# 链路正常: 127.0.0.1:48898（NetId 127.0.0.1.1.1，AMS 端口 851，ReadDeviceInfo 收到应答）

./uic adsread -addr 127.0.0.1:48898 -info
./uic adsread -addr 127.0.0.1:48898 -state
./uic adsread -addr 127.0.0.1:48898   -symbol MAIN.nCounter:int16,MAIN.fTemperature:float32,MAIN.sRecipe:string:81
# 符号            类型      值          质量
# MAIN.nCounter   int16     1234        good
# MAIN.fTemperature float32 23.5        good
# MAIN.sRecipe    string    batch-42    good

./uic adsread -addr 127.0.0.1:48898 -symbol MAIN.nSetpoint:int16 -set 750
./uic adsread -addr 127.0.0.1:48898 -symbol MAIN.sName:string:81 -set line-2
./uic adsread -addr 127.0.0.1:48898 -control stop -allow-control
```

`-symbol` 的写法是 `NAME[:type[:length]]`，逗号分隔；类型省略时用 `-type`，
长度省略时 string/bytes 用 `-strlen`。同一个 PLC 里不同变量本来就有不同类型，
按符号逐个声明比「一刀切 -type」更接近现场。读/写失败时命令的退出码为 1，脚本
可以直接判断「有没有坏点」，不需要解析表格。

示例配置带上网关跑真实采集：

```bash
./uic -config examples/ads.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8096/api/v1/points?device=plc01
curl -s -X POST -H 'Authorization: Bearer dev-token-0123456789' \
  -H 'Content-Type: application/json' \
  -d '{"value":{"type":"int16","num":640}}' \
  localhost:8096/api/v1/points/plc01/setpoint/write
```

写接口返回 `quality: uncertain` 是预期：写入已确认（ADS 的 Write 应答只有
result，没有回显），但缓存里的值要等下一轮采集才刷新；把它标成 `good` 会让人以为
「读到的新值就是刚写的值」。

排查顺序内建在失败提示里：**TCP 48898 是否可达 → `labels.netid` 是否与 PLC 配置
一致 → `labels.port`（851/801）→ PLC 运行时是否在运行 → 符号名与宽度是否与声明
一致**。`-probe` 能分开前四层，`-info`/`-state` 能确认握手之后设备是否正常，
`-symbol` 的单点错误能定位最后一层。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/ads` | AMS/TCP 与 AMS 头金帧（HandleByName `MAIN.nCounter`）、两层长度一致性、reserved 非零拒绝、NetID 解析（6 段/0..255/非法）、7 类请求构造、Read/Write/ReadWrite/DeviceInfo/State/Control 应答解析、错误码分类与临时性、Command/State 字符串 |
| `driver/ads` | 真实 TCP 回环：9 种类型读写、数组与字符串、地址/权限/宽度错误逐点隔离、坏 NetId/端口、握手超时、Close 幂等、句柄失效重解析（读与写）、类型范围/小数/BCD/字符串 NUL 拒绝、脚本化服务端：临时 AMS 错误重试、通知帧与陈旧 invoke 丢弃、畸形帧、句柄 0/短句柄、设备信息/状态/控制的 AMS 错误与短载荷、ctx 超时、拨号失败 |
| `sim/ads` | 真实 TCP 回环：默认变量表、GET/PUT 夹具、句柄分配/释放/失效、只读与宽度校验、数组仅 `[2]` 可写、字符串/浮点、错误 NetId/端口、未知命令、通知不应答、畸形载荷、坏帧断链后仍可服务、Close 幂等 |
| 点表/规划 | `PolicyFor("ads")` 只允许 symbol、`NAME:81` 与 length 列、`MAIN.arr[2]` 保留在 path、寄存器/selector 在导入期拒绝、`examples/ads.json` 通过 `-check`（17 个驱动） |
| `cmd/uic` | `-symbol` 声明解析（类型/长度/错误写法）、`-set` 值解析（bool/int/uint/float/string/bytes、进制、越界、BCD 拒绝）、`-control` 映射、行显示与错误质量 |

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

#### 3.11.7 历史窗口聚合

计算点的表达式可以引用**历史**而不是当前值：

```json
{ "kind": "computed", "expr": "avg(press:power, 5m)" }
```

语法是 `func(ref, duration)`：第二个参数是时长字面量时，函数从「对当前值做聚合」
变成「对最近一个窗口内的历史样本做聚合」。支持 `avg` / `min` / `max` / `sum` /
`count`，窗口限定 `1s .. 24h`，单位只认小写 `ms` / `s` / `m` / `h`。

| 写法 | 含义 | 备注 |
|---|---|---|
| `avg(press:power, 5m)` | 最近 5 分钟的平均功率 | 单位必须紧跟数字，`5 m` 是两个 token |
| `max(press:power, 1h)` | 最近 1 小时的峰值 | 窗口上限 24h |
| `count(press:power, 5m)` | 窗口内的 good 样本数 | **只有**窗口形式，`count(ref)` 报错 |
| `press:power - avg(press:power, 5m)` | 当前值相对均值的偏差 | 窗口与当前值可以混用 |

语义是刻意收紧的：

| 规则 | 内容 | 为什么 |
|---|---|---|
| 只统计 `good` | `uncertain` / `stale` / `timeout` 一律不计 | 把断线时保留的最后值算进平均，等于让一段没有数据的时间也参与统计 |
| 只统计数值 | `bool` / `string` / `bytes` 不计 | 开关量平均成 0.37 没有工程意义；文本没有序 |
| 空窗口是**失败** | 计算点 → `bad`；报警 → `unknown` | 返回 0 会让「没有数据」伪装成一个合法读数 |
| 窗口是闭区间 | `[now-window, now]` | 与历史查询一致，整分钟采样下 5m 稳定覆盖 5 个点 |
| 窗口引用不算依赖 | `Windows()` 单独给出，不进入 `Refs()` | 读的是「过去」，与当前值不构成环：`a = avg(b, 5m)`、`b = a + 1` 合法 |

配置期检查（全部静态，避免「通过校验但永远算不出值」）：

1. `storage.history.enabled` 必须为 `true`；
2. 被聚合的引用必须存在，且 `dataType` 是数值；
3. 窗口不得超过 `storage.history.maxAge` —— 否则老样本已被淘汰，聚合会悄悄
   只覆盖最近一段，结果「看起来正常」但含义与配置不同。

代价与缓存：每次求值都要扫历史段。runtime 侧因此包了一层短 TTL 缓存
（`clamp(window/60, 1s, 30s)`，空窗口压到 1s），让 200ms 的报警兜底扫描不会把
磁盘打满。另一面是**样本密度**：历史只记「变化 + 心跳」，稳定点位在 5 分钟窗口里
可能只有 1 个样本，此时 `avg` 就等于那个值；需要判断数据够不够时用 `count()`。

#### 3.11.8 结果编码

| 目标类型 | 规则 |
|---|---|
| `float64` | 原样；NaN / ±Inf 判 bad |
| `float32` | 转 `float32`；溢出为 Inf 判 bad |
| 整数类型 | **四舍五入**后做范围检查；越界判 bad，**不截断** |
| `bool` | 非零即真 |

整数做范围检查而不是截断，理由与 §3.10.4 相同：`70000` 写进 `uint16` 变成 `4464`，
会让操作员按一个错的数字决策。四舍五入而不是向下取整，是因为配置声明的是整数
类型，用户要的是**最近的**整数。

#### 3.11.9 只读注入：`OpenConfig.Lookup`

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

#### 3.11.10 边界

| 项 | 状态 | 说明 |
|---|---|---|
| 只读 | ✅（且强制） | 计算点没有可写回的地址 |
| 数值与布尔 | ✅ | 字符串 / Bytes / BCD 拒绝 |
| 同设备拓扑求值 | ✅ | 一次 `Read` 内闭合 |
| 跨设备引用 | ✅（最终一致） | 慢一拍，文档明示 |
| 环检测 | ✅ | 配置期拒绝，含跨设备 |
| 历史窗口聚合（`avg(近 5 分钟)`） | ✅ | 需要 `storage.history.enabled`；只统计 good 的数值样本；空窗口判失败（见 §3.11.7） |
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
| `mc`（MC 3E） | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `fins`（FINS/TCP） | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `enip`（CIP 显式报文） | supported | supported | unsupported | unsupported | unsupported | supported | supported |
| `bacnet`（BACnet/IP 对象属性） | supported | supported | unsupported | unsupported | unsupported | unsupported | unsupported |
| `iec104`（IEC 60870-5-104 主站） | supported | supported | supported ⁵ | unsupported | unsupported | supported ⁶ | unsupported |
| `dlt645`（多功能电能表 645-2007） | supported | supported | unsupported | unsupported | unsupported | unsupported ⁷ | unsupported |
| `cjt188`（户用计量仪表 188-2004） | supported | supported | unsupported | unsupported | unsupported | unsupported ⁸ | unsupported |
| `snmp`（v1/v2c 对象读写） | supported | supported | unsupported | unsupported | unsupported | supported ⁹ | unsupported |
| `ads`（AMS/TCP 符号读写） | supported | supported | unsupported | unsupported | unsupported | unsupported ¹⁰ | unsupported |
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
>
> ⁵ IEC 104 的从站按变化主动上送，驱动内部把它消化成最新值表，上层拿到的仍是
> `Read` 拉取契约。声明 `unsupported` 会让配置界面建议换协议，方向就错了。
>
> ⁶ 与 MQTT 同理：`Read` 只查内存表、不做网络交互，一次读任意多点位的成本与点位
> 数量无关。这与 Modbus 的「一个请求读多地址」不是一回事，但对外契约一致。
>
> ⁷ 645 的 `MultiRead` 必须声明 `unsupported`：协议一次请求只能带一个数据标识（DI），
> 逐点串行是协议的固有代价，不是实现偷懒。声明 `supported` 会让调度层以为可以靠
> 「一次多读」压采集周期，实际只会把一轮采集拖得更长（见 §3.10.10.6）。
>
> ⁸ 188 同理，但有一层额外优化：同一个 DI 的多个字段在同一次 `Read` 内复用一次事务
> （例如 901F 的当前流量/结算日流量/时间/状态只发一帧）。跨 DI 仍然不合并；把
> `MultiRead` 声明为 `supported` 会让调度层误以为任意多点位都只付一次事务成本
> （见 §3.10.11.7）。
>
> ⁹ SNMP 的 `MultiRead` 是**协议真实支持**的「一条 GET 带多个 varbind」，与 645/188
> 的逐点串行相反。但一帧仍受两个上限约束：`options.maxOids`（默认 32）与 1400 字节
> 的编码预算（避免 IP 分片被中间设备丢弃）。因此「一次多读」不等于「任意多点位都只
> 付一次事务」——点位多时会被切成多条 GET（见 §3.10.12.5）。
>
> ¹⁰ ADS 的 `MultiRead` 声明 `unsupported` 是**主动放弃**：`ReadWrite` 可以带多个
> 句柄，但一次失败无法逐点归因；逐点事务换来「哪个变量、哪一类错误」。这与 645/188
> 的「协议只能一次一个 DI」不同（见 §3.10.13.1）。

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
