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

#### 3.3.1 部分失败契约（全仓统一，不可逐协议自定义）

「哪些错误该上抛、哪些只标样本」不是各驱动的口味问题，而是一条**被上层代码
强制的硬契约**。它同时被两个方向的错都咬过，所以在这里一次说清，后面各协议
章节只写「本协议的链路级判据是什么」，不再重复规则本身。

**规则**（`driver.PartialReadError` 是它的唯一实现）：

| 情形 | `Read` 返回的错误 | 样本 |
|---|---|---|
| 有任意一个点位成功 | `nil` | 失败点各自带 `Err` + 对应质量码，成功点保持 `good` |
| 全部点位失败 | 第一个错误 | 每个失败点都带 `Err`（不得缺样本） |
| 链路级失败（超时 / 断连 / ctx 到期） | 该错误，**立即返回** | 剩余未发出的点位显式标坏 |

**为什么「有成功点就返回 nil」不是宽容而是必须**：`runtime/collector.go` 拿到
`Read` 的错误后会按分类处理——

```go
switch driver.KindOf(err) {
case driver.ErrDisconnected, driver.ErrTimeout, driver.ErrProtocol:
    _ = c.conn.Close()   // 丢弃连接，下一轮重新建链
    c.conn = nil
}
```

也就是说，一个点位失败一旦升级成整轮错误，代价是**整台设备每周期断链重连**
（TCP 握手 / UDP 会话重建 / 串口重新打开），而调度器那一侧还会
`consecutive++` → `StateDegraded`/`StateFaulted` → 指数退避。一个属性名写错的
点位就能让一台正常设备看起来彻底坏掉，点表里其它点位跟着不再采集。

**反过来「全失败必须返回错误」也不能省**：只在样本上标坏、整体返回 `nil`，
熔断器与告警永远看不到异常，设备会以「在采但全坏」的状态长期存活——比直接
报错更难发现。

**链路级失败是上面闸门的例外**：一轮采集被周期超时打断本身就是「设备太慢」
的信号，必须让调度器看到；此时即便本轮已有好样本也要返回该错误。而「哪个错误
算链路级」必须由统一的谓词回答，不能各驱动各写一遍：

```go
// driver.IsLinkFailure：只有超时与断连是链路级
func IsLinkFailure(err error) bool {
    switch KindOf(err) {
    case ErrTimeout, ErrDisconnected:
        return true
    }
    return false
}
```

地址 / 权限 / 不支持是**点级**问题（点表配错、符号写错、变量类型不符），换一个
点位照样能读，因此绝不能短路整轮。它们**可以**进 `firstErr`（全轮都配错时正该
把地址错误报上去，`CountsTowardBreaker` 也恰好对它们返回 false、不熔断），但绝不能
触发「中止剩余点位」——那一分支只留给 `IsLinkFailure`。

协议错（`ErrProtocol`）要看**帧边界是否还可信**：

- 数据报 / 一问一答（BACnet/IP、SNMP、CoAP、KNX）：一帧畸形只说明这一次应答坏了，
  下一帧是独立的，按点级处理。
- 长度前缀字节流（AMS/TCP、CIP、HSMS、MMS）：帧已被完整读入才发现内容不对，说明
  帧边界仍然对齐，也可以按点级处理；但**长度字段本身不可信**时帧边界已经丢失，
  后续帧不可信，必须立即收尾并上抛。ADS 的 `linkFailure` 因此把 `protocol` 与
  超时/断连一起算作链路级——它的 `protocol` 只来自帧长/AMS 应答层的结构性错误。

> **这条契约的失效方式是「静默的」**：违反它的驱动照样能通过全部功能测试，
> 只在运行期表现为「采集很慢、设备老是重连、日志里偶发 protocol/address」。
> 因此本仓库把 `IsLinkFailure` 与 `PartialReadError` 收敛到 `internal/driver`
> 一处实现，并要求每个逐点轮询驱动都有一条「有成功点时 `err == nil`」与
> 一条「全失败时必须返回错误」的回归用例（见各协议测试矩阵）。
>
> 历史教训：这条契约最初只写在 S7（§3.7.4）与 OPC UA（§3.9.5）两节，其余
> 协议章节没有引用它，于是 BACnet、KNX、IEC 61850、EtherNet/IP、SNMP、ADS、
> CoAP、SECS 八个驱动各自实现了「无条件上抛 firstErr」的同一份模板缺陷。修复
> 时另有两条既有测试把违规行为钉成了期望值（SECS 的「坏地址 + 好点必须返回
> address 错误」、CoAP 的「混合成功与失败必须返回错误」），一并改正。

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
4. **成功项不得被同批失败「连坐」**：逐项返回的意思是「哪一条成功、哪一条
   失败」都能回答。禁止这种写法——

   ```go
   for i := range results {          // ✗ 全仓禁止
       if results[i].Err == nil {
           results[i].Err = firstErr // 把「整批失败」硬塞进成功项
       }
   }
   ```

   后果不是「更安全」，而是调用方据 `Err` 去重发一个**已经写下去**的点位：
   对计数/累计类点位就是重复累加，对脉冲类点位就是多动一次。同批有一半失败
   时，真正失败的那一条也会淹没在一片错误里，运维无从下手。批级失败只通过
   第二个返回值（`firstErr`）上抛，`results` 里每一格只描述自己。
5. **失败项必须带 `Quality`**，不能留空串：`runtime/collector.go` 对
   `Quality==""` 会兜底成 `bad`，但驱动层留空等于把「为什么坏」这条信息
   丢了（`qualityOf(err)` 本来能给出 `address_error`/`comm_error`/`timeout`）。
6. **数值越界/带小数/非有限值必须在编码时拒绝，不能静默改变值**。这一步的
   收口点在 `codec/value` 的 `Encode`：所有写型驱动（Modbus / S7 / ADS /
   ENIP / MC / FINS / Mewtocol …）都把 `float64 → 字节` 的最终落地交给它，
   因此 `Encode` 在转换前调用 `checkRange`，拒绝一切「编码后会变值」的输入——
   整数越界（`40000`→int16 静默回绕）、带小数（`1.5`→int16 丢成 `1`）、
   float32 溢出成 `±Inf` 或下溢成 `0`、`NaN`、BCD 超位或为负，并返回哨兵错误
   `value.ErrRange`。把守卫放在 `Encode` 内部而不是各驱动各写一份，是为了
   防止「某个驱动忘了调自己的 `checkRange` 就静默损坏数据」——历史上
   Modbus 与 Mewtocol 正是漏掉了这份本该人人都有的检查。驱动侧识别到
   `errors.Is(err, value.ErrRange)` 后归为 `ErrAddress`（点/配置错误，不计入
   熔断、不重试，质量 `address_error`），与 S7 / ADS / ENIP / MC / FINS 既有的
   `checkRange` 同义。各驱动原有的 `checkRange` 副本在编码前再拦一道，属
   冗余但无害的双保险。

#### 3.4.1 寄存器内的位写（读-改-写 / 掩码写）

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
5. **有原子路径时优先走它**：Modbus 的 FC22（掩码写）让设备侧一次完成位修改，
   不存在读-改-写之间的空窗。默认自动探测（`maskwrite=auto`），设备回 `0x01`
   时永久退回读-改-写并在本次连接内记住结论；`maskwrite=on` 锁定 FC22
   （设备被换掉时报错而不是静默换路径），`maskwrite=off` 连探测都不发。
   探测失败必须靠**哨兵错误**（`ErrIllegalFunction`）判断，不能比对错误文本。
6. 原生支持位写的协议（S7 的 `DB1.DBX0.3`、Modbus 线圈）**不要**走这条路径：
   它们本来就能只写一位。

Modbus 参考实现：`codec/modbus` 提供 `EncodeReadRegisters` / `DecodeRegisters` /
`WordSetBit` / `EncodeWriteRegister` / `MaskWriteWord` / `ApplyMaskWrite` /
`EncodeMaskWriteRegister` / `DecodeMaskWriteRegister` 纯函数，驱动侧 `writeBitRMW`
负责策略选择、事务顺序与错误分类。

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
| `Ref.SetDeadline(t)` | 单次事务的读/写截止时间。协议层显式给的值原样生效（可大于底层兜底），**只有零值（未设）才用兜底超时**——串口与 TCP 行为一致。TCP 早期实现把协议值兜底取更早者，会静默截断长超时（配 30s 实际 5s 超时、且只在串口服务器链路上复现），已改 |
| `Ref.ReadFull(n)` / `Write(p)` | 满足 `transport.Stream`，可直接交给编解码层 |
| `Count()` | 当前注册的物理端口数（诊断/测试用） |

`proto` 参数只用于错误信息（「modbus-rtu 与 dlt645 对 /dev/ttyUSB0 的参数不一致」比
「参数冲突」有用得多）。参数冲突的判定是逐字段比较（波特率/数据位/校验/停止位），
任一不同即拒绝，并**保留原使用者**——先到者优先，避免一个配错的通道把正在工作的
通道踢掉。

当前消费者：`modbus-rtu`、`modbus-ascii`、`dlt645`、`cjt188`、`iec101`。IEC 101 落地时
按这条约定直接复用，没有另写一套（见 §3.10.17）。

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

**部分失败的策略与 Modbus 一致**（完整规则与「为什么」见 §3.3.1）：只要有任意一项
成功，`Read` 就不向上返回错误（好样本必须保留）；**全部点位失败**才返回第一个
错误，让设备进入故障/退避。链路级错误（超时、断连）例外：立即返回，并把剩余
未发送的点位显式标坏——让它们「消失」会诱导上层报「point missing」，把真正的
断链原因盖掉。S7 的「链路级」判据直接用 `driver.IsLinkFailure`（超时/断连）。

**链路级错误必须作为顶层错误上抛，不被前一块的点级错误顶掉**：分块读时前一块
里某个点返回「对象不存在」（点级 `ErrAddress`）、随后一块链路断掉，顶层错误
**必须是链路错误本身**。否则 collector 拿到 `address` 就不会重连，链路永远不
重建（§3.3.1）。已读到的好样本原样保留。

#### 3.7.5 切块策略

两个约束同时生效，取更严的一个：

1. **项数 ≤ 20**：S7 协议参数区的项数上限。
2. **请求与响应都不超过协商 PDU**：按最坏情况估算（每项请求 12 字节规格、
   响应 4 字节头 + 数据），写入时还要加上数据区的「4 字节头 + 偶数对齐负载」。

预算按最坏情况算而不是「发出去看错误」：S7 超限通常表现为对端不响应，而不是
返回一个明确的「太长」错误码；从超时反推原因的成本远高于多发一帧。

**单项自己就超过协商 PDU 时，切块救不了**：一个字符串点声明 `length=400`、而
CPU 只协商了 240，那么无论怎么切、单项独占一帧也要 416 字节。继续发出去，对
端要么直接不回（表现为超时）、要么把响应截断。这是**地址配置错**
（`address.length` 太大），必须在成帧之前用 `fitsPDU` 拦下，报 `ErrAddress`
（不重试、不熔断整台设备），并保留诊断「item needs N bytes of PDU budget,
negotiated PDU is M」。读、写两侧共用同一套预算（`itemBudget`），避免只防一
边。

#### 3.7.6 探活

`Probe` 读 `M0.0` 一个字节。**单项返回「地址不存在」也算链路可达**：探活要回答的
是「CPU 还应答吗」，不是「探活点配得对吗」。把地址错误当断链，会让一个配错的
探活地址把整台设备永久判死——而现场往往先怀疑网络，最后才发现是自己写错了地址。

#### 3.7.6.1 写路径的部分失败与本地校验

写（§3.4 规则 1「逐项返回」）同样逐项成帧。本实现按**请求下标预分配结果槽位**，
每个失败分支只填「还没报告过」的槽位，最后按请求顺序收拢——保证每个请求恰好
一个结果、不重复、不消失。链路在第 N 块断掉时，前块已成功的条目保持成功
（规则 4），剩余条目显式标坏并从结果里返回，绝不出现「结果条数 < 请求条数」
（上层会把缺失项兜底成「driver returned no result」，丢掉真正的链路原因）。

发报文之前必须拦下三类「看起来写成功了、实际值变了」的请求（规则 3）：

| 情况 | 判据 | 归类 |
|---|---|---|
| 带小数的值写整数（`1.5`→int16） | `v != math.Trunc(v)` | `ErrAddress` |
| 整数越界（`70000`→int16 静默回绕成 `4464`） | `< Min / >= Max+1` | `ErrAddress` |
| float32 溢出成 `+Inf` / 下溢成 `0` | 转 float32 后 `IsInf` 或变 `0` | `ErrAddress` |
| STRING/bytes 超长（`"ABCDEFGHIJ"` 写进 4 字节地址只发 `"ABCD"`） | `len(value) > address.length` | `ErrAddress` |
| 单项超过协商 PDU（`address.length` 太大） | `!fitsPDU(...)` | `ErrAddress` |

这些一律归 `ErrAddress`：越界/带小数/超长/超限都是**配置错**，换一个值或改一行
配置就好，不该重试、不该熔断整台设备。底层范围校验集中在 `codec/value` 的
`Encode`（返回 `value.ErrRange`，见 §3.4 规则 6）；S7 / MC / FINS / ADS / ENIP
各自仍保留一份 `checkRange` 在编码前再拦一道（冗余双保险，行为同构），Modbus
与 Mewtocol 原先漏掉了这一步，现已随 `Encode` 的收口一并覆盖。字符串/bytes
**恰好填满**宽度仍照常写入（按宽度补零语义不变）。

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
会让反复重试逐渐耗尽 PLC 的上传会话，而错误信息只会显示「start failed」。长度
上限的**早退检查必须排在注册 End 的 defer 之后**——Start 已经成功、PLC 侧会话
已建立，此处直接 return 会跳过收尾、把会话留在 PLC 上（当前 Start 的长度字段
是 5 位十进制 ASCII，上限 99999，使这条早退路径实际不可达；但顺序本身是错的，
一旦字段格式变化就会泄漏会话，故按注释修正为「先注册收尾、再判长度」）。
分片是否结束只看 `param[1]`：`0` 是最后一片，`1` 表示后面还有——把 1 当成
最后一片不会报错，只会让备份**静默少半截**。

**写响应的数据区必须逐项给满返回码**。`ParseWriteAck` 拒绝参数区项数与请求数
不一致、以及数据区返回码数量少于项数的响应：一个被截断的响应若被当成
「剩下的都成功」，一半的写请求会显示成功、实际没落到 PLC——计数类点位重复
累加，脉冲类点位少动一次，而且现场完全看不出问题。宁可整块判坏，也不猜。

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

#### 3.7.9 回归测试矩阵

`internal/codec/s7/regress_test.go` 与 `internal/driver/s7/regress_test.go` 钉住
本轮修复。每个用例都做了变异验证（改坏实现→用例变红→还原转绿）。

| 用例 | 钉住的契约 | 变异 |
|---|---|---|
| `TestWriteAckTruncatedDataIsBad` | 写响应数据区截断判坏（不当成「剩下都成功」） | 去掉 `len(pdu.Data) < count` 检查 |
| `TestWriteAckCountMismatchIsBad` / `WrongFunc` / `WrongRosctr` | 参数区项数/功能码/帧类型不符判坏 | — |
| `TestReadLinkFailureNotMaskedByPointError` | 链路失败不被前块点级错误顶掉 | `return out, firstErr` 还原成顶掉 |
| `TestReadOversizedItemRejectedLocally` | 单项超 PDU 本地判坏（不发出请求） | 去掉读侧 `fitsPDU` 守卫 |
| `TestWritePartialFailureKeepsEveryResult` | 写分块后块失败：每个请求一个结果，链路错误显式标坏 | 早退不填后续槽位 |
| `TestWriteStringTooLongIsRejected` | STRING 超长判坏（不静默截断） | 去掉超长拒绝 |
| `TestWriteStringExactWidthStillWorks` | 恰好填满仍照常写（不误杀） | — |
| `TestWriteOutOfRangeIsAddress` | 整数越界/带小数/float32 溢出下溢判坏，归 address | 去掉 `checkRange` |
| `TestWriteOversizedItemRejectedLocally` | 写单项超 PDU 本地判坏 | 去掉写侧 `fitsPDU` 守卫 |
| `TestDecodeShortPayloadIsAddressNotBreaker` | 解码失败归 address，不熔断 | 还原成 `ErrProtocol` |
| `TestDecodeSampleSucceeds` | 正常值解出来（分类改动不误杀） | — |
| `TestFitsPDBudget` | PDU 预算公式读写/位访问一致 | — |

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
| FC22 掩码只覆盖部分点位 | 拒绝 `0x02` | 同上：半个 float 没有意义 |
| FC22 掩码落在没有缓存样本的点位 | 返回 `0x04` | 掩码是在网关快照上算的；没有快照就无法计算，用 0 顶替等于把「没有数据」伪装成某个值 |
| FC22 掩码全部为空操作 | 照常回显 | 主站可能只在探活，拒绝会让「设备支不支持 FC22」这个问题得到错误答案 |
| 未知 unit | 返回 `0x0B` | 网关目标不存在；比不响应好——主站能立刻看到确定错误码，而不是 3 秒超时后去查网络 |
| 未知功能码 | 返回 `0x01` | — |
| 报文长度/字节数与功能码不匹配 | 返回 `0x03` | 认出功能码但数据非法 |

**FC22 的已知边界**：网关从站的 FC22 在当前值取自**最近一次采样的缓存**，
不是实时回读南向设备。若南向设备在两次采集之间自己改了这个寄存器，这次
掩码写会把它带回缓存里的值。要做到设备侧原子，需要把掩码透传到南向驱动
（当前驱动契约只有值写入，没有掩码写入），这是刻意留白而不是遗漏。

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
| 多寄存器位写（掩码写，FC22） | ✅（单寄存器） | 主站默认优先用 FC22 原子掩码写，设备回 `0x01` 时退回 FC3+FC6 读-改-写（`device.labels.options=maskwrite=on|off` 可锁定策略）；网关从站实现 FC22，但只在**单寄存器**可写映射上生效，且掩码基于网关最近一次采样的缓存值计算——32 位点位仍拒绝 `0x02`，真正的设备侧原子需要把掩码透传到南向驱动 |
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

范围校验（`codec/opcua/checkRange`）覆盖四类「看起来成功了、实际值变了」：

1. **带小数的值写进整数类型**：`1.5` 写 int16 判坏，不截成 `1`；
2. **float32 溢出/下溢**：`1e300` 在 float32 里是 `+Inf`、`1e-300` 是 `0`，两者都不是
   调用方给的值，判坏；
3. **int64/uint64 的上界**：`2^63-1` 在 float64 里会舍入成 `2^63`，用 `>= 2^63` 严格判上界，
   否则 `2^63` 通过校验再 `int64(n)` 溢出成负数；
4. **编解码两侧都查**：`EncodeValue`（写）与 `DecodeValue`（读）都过同一道 `checkRange`——
   服务端把一个 double `1.5` 发来、而点位是 int16，同样判坏而不是静默截断。

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

**分块读时链路失败必须立即上抛**：`Read` 按 `maxNodesPerRead`（100）切块。若前一块
成功、后一块链路断掉，顶层错误**必须是链路错误本身**（`ErrDisconnected`/`ErrTimeout`），
不能被前一块里某个点级错误（如 `BadNodeIdUnknown`→`ErrAddress`）顶掉——否则 collector
拿到 address 就不会重连，链路永远不重建。已读到的好样本保留，剩余点位显式标坏（§3.3.1）。

#### 3.9.5.1 写路径的部分失败与本地校验

写（§3.4）同样按块切，且必须逐项返回：

| 情况 | 驱动行为 |
|---|---|
| 分块写，前块成功、后块链路失败 | 前块结果保持成功，剩余条目**显式标坏**并从结果里返回，顶层错误上抛链路错误。绝不出现「结果条数 < 请求条数」——collector 会把缺失项兜底成「driver returned no result」，丢掉真正的链路原因 |
| 编码失败（越界/带小数/类型不符） | 发请求前本地拒绝，归 `ErrAddress`，质量码 `address_error`；与 MC/FINS 的 `checkRange` 一致，不熔断 |
| 单帧超过协商缓冲区（65535） | 本驱动不支持分块，本地 `fail-closed` 报 `ErrProtocol`（非临时、不熔断、不触发重连），明确诊断「message size exceeds negotiated buffer」；发出去只会让对端按规范断连 |
| 服务端逐项拒绝 | 每个节点按自己的状态码分类回传（`BadNotWritable`→permission、`BadTypeMismatch`→address 等） |

**编码失败为什么归 `ErrAddress` 而不是 `ErrUnsupported`**：越界/带小数/类型不符都是
「这个点的值或类型配错了」，`ErrUnsupported` 会让指标把它记成「能力不支持」并（旧行为）
报成 `disabled`。归 `ErrAddress` 既表达了配置错的语义，又同样不熔断。

#### 3.9.6 状态码 → 错误分类

| 状态码 | 分类 | 理由 |
|---|---|---|
| `BadNodeIdUnknown` / `BadNodeIdInvalid` / `BadAttributeIdInvalid` | `ErrAddress` | 点位配错了，不是设备坏了；不该熔断整台设备 |
| `BadTypeMismatch` | `ErrAddress` | 点位声明的类型与服务端变量的类型不符，是配置错；归 address，不熔断 |
| `BadUserAccessDenied` / `BadSecurityChecks` / `BadNotWritable` / `BadNotReadable` | `ErrPermission` | 服务端拒绝读/写这个点（权限/策略），换一个点位照样能读，不该熔断 |
| `BadServiceUnsupported` | `ErrUnsupported` | 对端不支持该服务（能力问题，质量码报 `bad`，不报 `disabled`） |
| `BadTimeout` / `BadWaitingForInitialData` | `ErrTimeout` | 可重试 |
| `BadSessionIdInvalid` / `BadSessionClosed` / `BadSecureChannelIdInvalid` / `BadNoCommunication` / `BadNotConnected` / `BadCommunicationError` | `ErrDisconnected` | 需要重建会话或通道 |

其余未列出的 `Bad*` 归 `ErrProtocol`（会熔断）。

**为什么这里必须细分**：熔断器按连续失败次数触发（默认 5）。把「这个点没权限」「这个
点类型配错」这类**点级配置错**算成 `ErrProtocol`，会让一个写错的点位在 5 个采集周期后
把整台设备拖成 `Faulted`——设备其实是好的，只是这一个点配错了，却连累所有点一起停摆。
这与 MC 的结束码分类（§3.10.5）、FINS 的结束码分类（§3.10.6）是同一条纪律。

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

#### 3.9.9 回归测试矩阵

`internal/codec/opcua/regress_test.go`（编解码范围校验）与
`internal/driver/opcua/regress_test.go`（读写分块、分类、质量码、帧长）钉住本轮修复。
每个用例都做了变异验证（改坏实现→用例变红→还原转绿）。

| 用例 | 钉住的契约 | 变异 |
|---|---|---|
| `TestEncodeValueRejectsFractionalIntoInteger` | 带小数写整数判坏（不截断） | 去掉 `integral` 的小数检查 |
| `TestEncodeValueRejectsFloat32Overflow` / `Underflow` | float32 溢出成 Inf、下溢成 0 判坏 | 去掉 `f==0` / `IsInf` 检查 |
| `TestEncodeValueRejectsNonFiniteFloat64` | NaN/±Inf 判坏 | 已正确，作对照 |
| `TestEncodeValueInt64BoundaryIsExact` / `Uint64` | 上界严格 `< 2^63` / `< 2^64` | 把 `>=` 改成 `>`（2^63 通过再溢出） |
| `TestDecodeValueRejectsFractional/Float32Overflow/Underflow` | 解码侧同样过 `checkRange` | 同上（DecodeValue 复用 checkRange） |
| `TestEncodeDecodeRoundTrip` | 合法值往返不变（不误杀） | — |
| `TestReadLinkFailureReportsAndKeepsGoodSamples` | 分块读后块链路失败：上抛链路错误 + 保留前块好样本 | — |
| `TestReadLinkFailureNotMaskedByPointError` | 链路失败不被前块点级错误顶掉 | `return out, firstErr` 还原成顶掉 |
| `TestReadBadNotReadableIsPermissionNotBreaker` | `BadNotReadable`→permission，不熔断 | 还原成 `ErrProtocol` |
| `TestReadBadTypeMismatchIsAddressNotBreaker` | `BadTypeMismatch`→address，不熔断 | 还原成 `ErrProtocol` |
| `TestWritePartialFailureKeepsEveryResult` | 写分块后块失败：每条请求一个结果，不消失 | 丢弃后续块结果 |
| `TestWriteOutOfRangeIsAddress` | 越界/带小数写本地拒绝，归 address | 还原成 `ErrUnsupported` |
| `TestWriteOversizedFrameRejectedLocally` | 超缓冲区帧本地 fail-closed | 去掉帧长守卫 |
| `TestQualityForUnsupportedIsNotDisabled` | `ErrUnsupported`→bad，不报 disabled | 还原成 `QualityDisabled` |

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
   按错的数字决策，比没有数据更糟。`int64`/`uint64` 同样按各自的边界判（`int64`
   的上界是开区间，因为 `float64` 表示不了 `MaxInt64`），`NaN`/`±Inf` 一律判坏——
   `strconv.ParseFloat` 接受 `"NaN"`/`"Inf"` 字面量，不显式挡住就会进到模型里。
2. **布尔接受常见写法**：`true/false`、`1/0`、`ON/OFF`、`on/off`、`yes/no`。
3. **写入整数不带小数点**：`uint16` 写 `7` 发 `"7"` 而不是 `"7.0"`——大量设备
   用 `strconv` 严格解析，`7.0` 会被拒。

写入与读取走**同一条**范围校验（`textval.FormatNumber`），不是「读严格、写宽松」：
把 `40000` 写进 `int16` 点位、把 `1e30` 写进 `int64` 点位，都必须在发出报文之前
失败。这条容易被写成 `strconv.FormatInt(int64(n), 10)` 了事，而 `int64(1e30)` 在
Go 里是未定义行为，实际回绕成 `-9223372036854775808`——设备收到一个完全错误的数，
驱动却报成功。`uint64` 的上半段还要用 `FormatUint`，否则同样变成负数。

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
   单飞意味着后来的 `Ping` 会等前一次的结果——这个「结果」必须能区分**成功**
   （收到 PINGRESP）与**失败**（链路在 PINGREQ 在飞期间断开），并且必须能
   **广播给所有等待者**：`Read` 的静默自检与 `Probe` 会同时探测，而一次 PINGREQ
   只产生一条 PINGRESP。用 `chan error` 是错的——塞值只能唤醒一个接收者，另一个
   会在健康链路上白等满 `AckTimeout` 并误报断链，触发一次不必要的重连。本实现用
   `pingFlight{done chan struct{}; err error}`：结束方写 `err` 后 `close(done)`，
   PINGRESP 是 `nil`、链路断开是「connection lost before PINGRESP」，所有等待者
   读到同一个结论。

`Probe`（手工探活 / 熔断后的探测）走同一条路径。keepalive 循环也等 PINGRESP，
所以半死链路在后台就会被打断，不必等到某个点位彻底 stale 才暴露。

#### 3.10.6.1 客户端状态机的三条不变量

这三处都不是「风格问题」，违反任何一条都能编译、也能在实验室跑通，但在现场会
分别表现为永久卡死、周期性掉线和误判断链：

| 不变量 | 违反后的现场表现 | 做法 |
|---|---|---|
| 订阅窗口里分发 PUBLISH **不得再取连接锁** | `Connect` 永久卡死（`Close`/下一次 `Publish` 也一起卡在同一把锁上）；只能在「Broker 在 SUBACK 之前发了一条 QoS1 保留消息」时触发，实验室几乎撞不到 | `dispatchPublish(conn, pkt)` 由调用方把连接传进来。`Connect` 在订阅期间持有连接锁，读循环本来就知道自己读的是哪条连接，再加锁只会引入自锁。规范允许 Broker 在 SUBACK 前后任一顺序发保留消息，其他发布者也随时可能推来消息 |
| 每条报文写**自带写超时，写完清除** | ① 不清除：`net.Conn` 的写超时是粘性的，`Publish` 设的 `now + AckTimeout` 过期后，保活 PINGREQ、入站 QoS1 的 PUBACK、DISCONNECT 全部立刻 `i/o timeout`——发布间隔大于 `AckTimeout` 的连接会被自己判死、周期性重连；② 不设置：对端停止读取时 `Write` 永久阻塞，`Ping`/保活走不到自己的超时判断，链路状态被冻结 | 所有写走 `writePacket`：在 `writeMu` 内置 `SetWriteDeadline`、写、清成零值。设置与清除都在锁内，并发写各自设自己的上界，不会把对方的截止时间清掉 |
| 入站 PUBLISH 的协议违约**必须断开** | QoS=3、空主题、主题带 `+`/`#` 的消息被当成普通消息投给点位：QoS3 不回 PUBACK（Broker 无限重投），空主题无处可投，带通配符的主题在消费方看来像一个订阅——错误被藏进「收到了但语义不对」 | `ParsePublish` 按 §3.3.1.2 / §3.3.2 / §4.7.3 拒绝；`readLoop` 与订阅窗口都把它当建链/链路失败处理（规范要求关闭网络连接）。载荷解不出（`OnMessage` 内部）仍是「保留旧值 + 计数」，两者不是一回事 |

#### 3.10.6.2 回归测试矩阵（客户端状态机）

`internal/mqtt/regress_test.go` 每条用例都对应一次**实际发生过的缺陷**，且做过变异
验证（把修复改回缺陷形态，用例必须转红）：

| 用例 | 钉住的缺陷 | 变异方式 |
|---|---|---|
| `TestConnectDoesNotDeadlockOnQoS1PublishDuringSubscribe` | 订阅窗口内 QoS1 保留消息让 `Connect` 自锁 | `dispatchPublish` 恢复二次 `c.mu.Lock()` |
| `TestPublishDoesNotLeaveStickyWriteDeadline` | `Publish` 的写超时残留成粘性 deadline，空闲后保活把自己判死 | 去掉写完的 `SetWriteDeadline(零值)`，并把 `Ping` 的写改回不设上界 |
| `TestConcurrentPingBothWakeOnOnePingresp` | 一次 PINGRESP 只唤醒一个等待者，另一个健康链路上误报超时 | `pingFlight.done` 改成容量 1 的 channel（单播语义） |
| `TestConcurrentPingDuringDeadLinkBothFail` | 死链路上并发 Ping 误报成功（历史 `299f82d`） | `pingFlight` 退回 `close()`-only 语义 |
| `TestParsePublishRejectsProtocolViolations` | QoS=3 / 空主题 / 主题含通配符被当成普通消息 | 三处守卫前置 `false &&` |

测试用两种对端：`internal/sim/mqtt`（行为完整的 Broker）与 `scriptedBroker`（按
规范允许、但内置 Broker 不刻意构造的顺序发报文，例如 SUBACK 之前先发保留消息）。
后者是必须的——「本仓库 Broker 恰好这么发」不能当成协议约束来测。

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
| 结束代码 | 非零即错误 | 分类见下：地址类 → `address_error`、命令不支持 → `bad`（不支持）、写保护 → `bad`（权限）、其余 → `bad`（协议错） |

**结束码必须自己带 `Kind()`**（`mccodec.EndCodeError.Kind`）：`driver.KindOf` 对没有
`Kinded` 实现的错误兜底成 `ErrInternal`，于是 `CountsTowardBreaker` 为真——一个点位
软元件名写错（`0xC052`/`0xC056`）就会让整台 PLC 被熔断，而且质量码只能靠驱动再查一遍
结束码表，API 层与采集层对同一条错误给出不同结论。

| 结束码 | 含义 | `Kind` | 质量 |
|---|---|---|---|
| `0xC052`/`0xC053`/`0xC054`/`0xC056` | 起始号非法 / 点数非法 / 起始+点数越界 / 软元件不支持 | `ErrAddress` | `address_error` |
| `0xC058`/`0xC05B` | 数据长度不符 / 写数据非法 | `ErrAddress` | `address_error` |
| `0xC050`/`0xC051` | 命令 / 子命令不支持 | `ErrUnsupported` | `bad` |
| `0xC06F` | 设备存储器写保护 | `ErrPermission` | `bad` |
| 其余（网络号/PC 号/站号/监视定时器错、请求无法处理） | — | `ErrProtocol` | `bad` |

这些码都不带 `Temporary`：结束码是设备对「这条请求」的确定答复，重发同样的请求
只会得到同样的码。

**三个最容易写错的细节**（已固化在 `codec/mc`）：

1. **位软元件按位打包**：首点在第一个字节的 bit0，一点一字节是错的。
2. **X/Y/B/W 的编号按十六进制理解**：`X1F` 在线上是数值 31，配置里 `offset=31`。
3. **字软元件上的 bool 点位必须显式给 `address.bit`（0..15）**，驱动做「读整字 → 只改
   目标位 → 写整字」的 RMW，绝不清零邻位。没有位号的 bool 在字软元件上判地址错。

**字序**：MC 原生是「字内小端、低字在前」。驱动在解码时先把每个字的两个字节对调
（归一化到「字内大端」），再按用户配置的 `byteOrder` 走 `value.Decode`；默认 `CDAB`。
字符串/字节类型按地址宽度补零；**超长则拒绝**（`address_error`），不静默截断——截断
会让调用方以为整个值都写下去了（§3.4 规则 3）。数值写入同样在发报文前检查范围：
越界整数、带小数的整数、float32 溢出成 ±Inf 或下溢成 0 一律拒绝（`70000` 写 D 的
int16 不能变成 `4464`）。

**批量写**：`Write` 逐条执行、逐条返回。同批里一条失败**不会**把已经成功的条目标成
错误（§3.4 规则 4），整批的失败只通过第二个返回值上抛——否则调用方会把一条已经写
下去的点位当成失败去重发。碰到链路级失败（超时/断连）时**短路**剩余条目并把它们
显式标坏：链路已死还逐条各等一次超时，会让一轮写入拖成 N 倍时长。

**读路径的三条纪律**：

- **链路级失败立即收尾**：`IsLinkFailure` 为真时不再尝试后续 span，剩余点位显式标坏
  并上抛。一轮采集被周期超时打断本身就是「设备太慢」的信号。
- **跨块读保留已得样本**：一个 span 超过单次上限（字 960 / 位 7168）时按块读。后一块
  失败只作用于还没拿到值的点位，前几块已解出的好值保留（§3.3.1）。
- **成功点数按样本计，不按 span 计**：span 成功返回 `nil` 不代表拿到了值——解码期的
  错误（位软元件配成数值类型、应答比请求短）是逐点记在样本上的。按 span 计数会把
  「全员解码失败」当成成功并返回 `nil` error，正是 §3.3.1 点名禁止的静默。

**滞留帧必须排空**：`framing.RoundTrip` 读到副头部不匹配时（上一个请求的迟到应答、
别的主站的报文、流已错位），先按长度把这条报文的体读掉再报协议错。否则残留体留在
TCP 流里，下一次请求把体当头部读，整条流从此错位；而协议错不是链路级失败，上层不会
重连，于是后续每个点位都莫名其妙地协议错。

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

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/mc` | 读/写金帧（副头部、3 字节软元件号、点数）、位打包/解包、结束码解析、区表与码反查、越界拒绝 |
| `driver/mc` | 字读、结束码→质量、字软元件 bool 缺位号、写成功、非 TCP 拒绝、能力声明、批量写不回填成功项；**结束码自带 `Kind()`**（地址类不计熔断、命令不支持归不支持、写保护归权限、且都不可重试）；**副头部不匹配时排空报文体后流重新对齐**；**读/写链路失败短路且剩余点位显式标坏**；**跨块读第二块失败保留第一块好样本**；**全员解码失败时上抛错误**；**写路径越界/带小数/float32 溢出在发报文前拒绝**；**STRING/bytes 超长拒绝、正好填满反向通过** |
| `sim/mc` | 真实 TCP 回环：字读写、位读写、字软元件位 RMW 保邻位、跨块多区、结束码映射、断连、Probe |

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
交换：`"AB"` 在 DM 里就是 `0x4142`，按地址宽度补零；**超长则拒绝**（按地址宽度报
`address_error`），不静默截断——截断会让调用方以为整个值都写下去了（§3.4 规则 3）。
数值写入同理：越界整数、带小数的整数、float32 溢出成 ±Inf 或下溢成 0，都在发报文
之前拒绝，而不是靠强制转换静默落地（`70000` 写 int16 不能变成 `4464`）。

#### 3.10.6.5.1 写路径与读路径的三条共同约束

`driver/finscore` 同时被 FINS/TCP 与 Host Link 复用，以下约束对两个驱动一并生效：

- **逐块读的结果不得因后一块失败被整段丢弃**。一个 span 可能跨多块（受
  `WordsPerRead`/`BitsPerRead` 限制），前几块已经解出好值、后一块报地址错或超时时，
  失败只作用于还没拿到值的点位；已解出的好样本原样保留（§3.3.1，与 SNMP/ENIP 的
  既有处理一致）。把整个 span 回填成同一条错误，会把「设备正常、只是某一块坏了」
  放大成整段数据丢失。
- **链路级失败立即收尾**。`IsLinkFailure(err)` 为真时不再继续后续 span/条目，剩余
  点位显式标坏并上抛——一轮采集被周期超时打断本身就是「设备太慢」的信号。
- **写路径遇链路级失败短路**。否则链路已死时一轮写入会被拖成 N 倍超时；剩余条目
  显式标坏，已成功的条目保持成功（§3.4 规则 4）。

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
| `driver/finscore` | 跨块读的部分失败（前块好样本保留、失败块只标坏自己，字读/位读/链路失败三种场景）、读链路失败短路、ctx 到期标坏剩余 span、写路径数值越界/带小数/float32 溢出拒绝、STRING/bytes 超长拒绝而非截断、正好填满容量的反向用例、写路径链路失败短路且保留前序成功、字符串跨帧拆分、EM 区位写 RMW 保邻位 |
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
读时按长度截断并去掉尾部 NUL；写时**超长拒绝**（按容量报错），不静默截断——截断会让
调用方以为整个值都写下去了，设备上的值却不是它给的值（§3.4 规则 3）。`bytes` 写入同理。

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

#### 3.10.7.7.1 报文流对齐与部分失败（链路健壮性）

CIP 显式报文没有事务号，靠一问一答定界。围绕这个前提有三条硬约束：

- **滞留帧必须排空**。`roundTrip` 在读封装头后如果发现**命令不匹配**（设备主动发的通知、
  别的客户端残留报文）或**上下文不匹配**（陈旧/交错应答），会先按 `h.Length` 把这条报文的
  体读掉再报错。否则残留体留在 TCP 流里，下一次请求会把体当头部读，整条流就此错位；而
  协议错又不是链路级失败，上层会继续用错位的流读后面的点位，症状是「莫名其妙一连串协议错」。
- **已读到的好点位不能因后续失败被丢弃**（§3.3.1）。`readBatch` 在「前几批成功、后一批解析
  失败」时会同时返回成功结果与错误；驱动保留成功样本，只把失败/未发的点标坏。丢弃会把
  「设备正常、只是某一帧坏了」放大成整段数据丢失。
- **单个点位构造失败不连累同 span**。span 按标签名分组，一个标签名/长度/类型配错只标坏它
  自己，同 span 的其它好点位照常发请求——否则一个配错的标签会让整组点位一起变 `address_error`。

写路径的两条对应约束：

- **数值越界在写报文前拒绝**（`checkRange`）：`70000` 写进 INT 不会静默变 `4464`，`1e300`
  写进 REAL 不会静默变 `+Inf`，带小数的整数写入也拒绝。与 ADS/DNP3 的既有处理一致（§3.4
  规则 3「写入失败不得静默」）。
- **写路径遇链路级失败短路**：`IsLinkFailure(err)` 出现时立即把剩余条目显式标坏并返回，不
  再逐条各等一次超时（否则链路已死时一轮写入会被拖成 N 倍时长）。

#### 3.10.7.8 错误与质量映射

CIP 通用状态在 `codec/enip` 里分类，驱动只做「错误 → 质量码」的最后一步：

| CIP 状态 | 分类 | 质量码 | 说明 |
|---|---|---|---|
| 0x03 / 0x04 / 0x05 / 0x06 / 0x11 / 0x13 / 0x15 / 0x16 | `ErrAddress` | `address_error` | 参数值非法 / 路径段错 / 目标不存在 / partial transfer（length 配太大）/ 应答过大 / 数据不够 / 数据过多 / 对象不存在 |
| 0x08 / 0x14 | `ErrUnsupported` | `bad` | 服务/属性不支持 |
| 0x0E / 0x0F | `ErrPermission` | `bad` | 属性只读 / 权限不足 |
| 0x01 / 0x02 / 0x07 / 0x0C / 0x10 | `ErrDisconnected` | `comm_error` | 连接失败/资源不足/连接丢失/状态冲突 |
| 其它 | `ErrProtocol` | `bad` | 协议错 |

> 0x03 / 0x06 / 0x11 归为点级而不是协议错：它们都是「这一条请求本身有问题」（参数非法、
> 读太多、应答超出缓冲），缩小 length 或改点表就能好，重试同一请求不会变好。归为
> `ErrProtocol` 会让 `CountsTowardBreaker` 为 true，把这类配置错当成「链路坏了」去熔断
> 整台设备。0x06（partial transfer）尤其常见：`maxReadBytes=400` 的保守上限就是为避免它，
> 真收到时说明该点位 length 配得过大。

封装状态里 **0x64（会话句柄无效）单独归为可重试**：会话过期后重新建链会重新注册，
属于自愈路径；其它状态判协议错，不重试。这条区分很重要——把 0x64 当永久错误会让
一次网络闪断把设备标成坏点直到人工重启；把它当通用重试又会对着配置错误反复重连。

本轮结果按 §3.3.1 的契约汇总：`disconnected` 走 `driver.IsLinkFailure`，一出现就
立即返回并把剩余点位显式标坏（CIP 会话已断，后面的标签不可能读到）；`address` /
`permission` / `unsupported` 是点级问题，只标该点，整轮仍 `err == nil`。全失败才
把第一个错误上抛。否则点表里一个标签名写错就会让整台 PLC 每周期重连一次。

#### 3.10.7.9 能力边界（刻意的）

| 不做 | 理由 |
|---|---|
| 隐式 I/O（UDP 2222，Class 1） | 需要连接参数协商、RPI、组包与看门狗，属于另一套实时模型；与本平台的「采集」语义不同 |
| 对象/属性寻址 | 属性段编码在不同厂商文档里有 0x2C/0x30 两种说法，没有真机验证前不猜——猜错会读到**别的对象**的属性，比报错危险得多 |
| 结构体/数组自动展开 | 需要 Member/Element 段与符号实例 ID 解析；标签名 `.Member` 的编码在不同固件上行为不一致 |
| 符号列表浏览 | 需要配合 FTP/HTTP 或厂商专有服务，CIP 本身不提供统一接口 |

`Probe` 用 Get Attributes All（Identity 对象 class 0x01 / instance 0x01）验证链路：
返回 0x08/0x14 也视为「CIP 层通」——Identity 对象不是必选实现，为此拒绝连接会误伤。

#### 3.10.7.10 位置与自检

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
| `codec/enip` | 封装头/长度语义、CPF 奇数对齐与越界、SendRRData、RegisterSession、CIP 请求/应答、附加状态、符号路径偶对齐、逻辑路径 8/16/32 位、Read/Write Tag 金帧、多服务偏移表、类型表、字符串编解码（**超长拒绝而非截断**）、状态码→分类（0x03/0x06/0x11 归点级地址错） |
| `driver/enip` | 注册与状态、命令/上下文错位（**滞留帧排空后流重新对齐，下一次读仍命中正确应答**）、原子类型、大小写、STRING、bytes 两阶段读、数组 offset、位读/位写保邻位、offset 写保前缀、多服务合包与拆批、MSP 自动退回、错类型/越界位/未知标签、会话失效可重试、Probe 容忍不支持、能力声明；**部分失败语义（有成功点则 `err==nil`、全失败才返回整体错误、链路级失败立即返回且剩余 span 的点位显式标坏）**；**readBatch 后一批失败时保留前一批已得的好样本**；**单个点位构造失败（标签/类型配错）不连累同 span 其它好点位**；**写路径数值越界/带小数/float32 溢出在写报文前拒绝（70000→INT16、1.5→INT16、1e300→REAL 均判地址错，且设备值不变）**；**STRING/bytes 超长拒绝而非静默截断**；**写路径遇链路级失败短路、剩余条目显式标坏**；**0x06 partial transfer 判点级地址错、不计入熔断**；**批量写逐项语义（一条成功 + 一条未知标签：成功项不被回填 `Err`、失败项带 `Quality`、且成功项确实落到从站）** |
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

路由位的编号需要单独记：**bit5（`0x20`）才是「带目的地址」，bit1（`0x02`）是
网络优先级**。按 `0x02` 去跳过 DNET/DADR，会把「优先级被置位」的普通报文误
当成路由报文、也会让真正经路由器回包的应答整体错位——两者都表现为「探测能通、
读值必错」。

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

##### 写值的数值边界：越界/非整数在写报文前拒绝

选标签只解决「发什么类型」，不解决「发的值能不能装进那个类型」。两个静默
截断/溢出都违背「写入失败不得静默」——调用方拿到成功，设备实际收到的是
另一个值：

| 标签 | BACnet 类型 | 越界/非规范值 | 不拦截会怎样 |
|---|---|---|---|
| REAL | 4 字节 float32 | 合法 float64 超出 float32 范围（如 `1e300`）、NaN、±Inf、下溢成 0 | 设备收到 `+Inf`/0，写下去的数与请求不同 |
| UNSIGNED / ENUMERATED | 整数 | 带小数的 `1.5` | 强转 uint64 静默变 `1` |

`dataType=float64` 的点写模拟量 `present-value`（走 REAL）是最隐蔽的一条：
该值在 `textval.CheckRange` 里是合法 float64，能一路通过上游校验，却在编码
成 REAL 时溢出。因此 `encodeWriteValue` 在写报文前对 REAL 做 float32 收敛检查、
对 UNSIGNED/ENUMERATED 做整数检查，越界一律回 `ErrAddress`（本地拒绝、不熔断
不重试、质量 `address_error`），与 S7/ENIP/MC/ADS 的 `checkRange` 纪律一致。

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

注意 `disconnected` 在本驱动里同时是**链路级**判据（`driver.IsLinkFailure`）：一轮里
遇到它就立即收尾、把剩余点位一并标坏，不逐个再等超时。而 `address`（对象/属性名
写错）只是点级问题——一个属性名拼错不应该让整台设备每周期丢弃 UDP 会话重连，
见 §3.3.1。

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
| `codec/bacnet` | BVLC/NPDU 长度语义、Forwarded-NPDU、标签头六种 LVT、application/context 差异、BOOLEAN 内联、最短字节数、REAL/DOUBLE 金帧、Read/Write 请求金帧、ComplexACK 两种形态、Error/Reject/Abort 解析、参数往返、别名与编号解析、**`ParseConfirmedRequest` 截断头回错误而非 panic（畸形 3 字节确认请求不崩进程）** |
| `driver/bacnet` | 元数据与能力声明、模拟量/字符串/枚举读、类型宽松与严格、未知对象/属性、数组下标与越界、写 REAL/字符串/枚举/布尔、写只读对象、类型不匹配本地拒绝、**写 REAL 的 float32 溢出/NaN/Inf/下溢拒绝、写 UNSIGNED/ENUMERATED 的非整数拒绝（静默截断/溢出在写报文前拦下，端到端设备值不变）**、超时、迟到应答丢弃、重发、ctx 取消、invoke 环绕、错误类映射；**部分失败语义（有成功点则 `err==nil`、全失败才返回整体错误、链路级失败立即返回并把剩余点标坏）** |
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
| 累计量带时标 | 12 | 5 + 7 字节 CP56Time2a |
| 单点带时标 / 双点带时标 | 8 | 1 + 品质 + 7 字节 CP56Time2a |
| 归一化带时标 / 标度化带时标 | 10 | 3 + 7 |
| 短浮点带时标 | 12 | 5 + 7 |
| 时钟同步 | 7 | 只有 7 字节时标 |

**品质位的位序在两类元素里不一样**，而且都不在「低 5 位」——这是最容易出
「看起来能跑但质量全错」的地方。标准里 QDS 与 SIQ/DIQ 的品质描述（BL/SB/NT/IV）
都落在 **bit4..7**，区别只在 bit0：测量值的 QDS 用 bit0 表示 OV（溢出），单/双点的
SIQ/DIQ 用 bit0 表示状态（SPI/DPI）。bit1..3 是保留位，必须掩掉：

- 测量值（QDS）：`OV=bit0`、保留 `bit1..3`、`BL=bit4 / SB=bit5 / NT=bit6 / IV=bit7`；
- 单点/双点（SIQ/DIQ）：`状态=bit0`、保留 `bit1..3`、`BL=bit4 / SB=bit5 / NT=bit6 / IV=bit7`。

`decodeQDS`/`decodeSIQQuality` 逐位映射到平台品质位，`encodeQDS`/`encodeSIQQuality`
是它们的逆运算。

最隐蔽的失败模式是「把品质当低 5 位裸读」（`Quality(b & 0x1F)`）：IV 落在 bit7 整个
漏掉，一个「无效」的模拟量会被报成 **good**（fail-open）；而 BL 落在 bit4 又被误读成
IV。SIQ/DIQ 与 QDS 的 BL/SB/NT/IV 虽同位置，但 SIQ 多一个状态位、QDS 多一个 OV，仍
不能共用一个解码器——`decodeSIQQuality` 走 `(b>>3)&mask`（掩掉状态位与保留位），
`decodeQDS` 单独逐位取 OV 与 bit4..7。

归一化值是 int16 的定点数，换算关系是 `value / 32768`（**不是 32767**）。
标准定义的是「满量程 = 32767 对应的物理量」，而线上编码是 15 位有效位加符号位，
所以可表示范围是 `-1 .. 1-2⁻¹⁵`，`+1` 本身不可表示。用 32767 编码、32768 解码
（一份很常见的「两边看起来差不多」的写法）会让每一次读写往返都把值缩小
0.003%：读回来再写下去、再写回来，数值会持续爬向 0。单点上确实看不出来，
斜坡给定与闭环调试这类场景会不断累积。

编码侧还必须夹紧到 int16：`round(1.0 × 32768)` 已经是 32768，直接转 `uint16`
会回绕成 `-32768`，也就是把「满量程」写成「负满量程」——现场表现为「给定拉到
最大，阀门反而关到底」。这类回绕与「2 月 31 日被 time.Date 静默改成 3 月 3 日」
是同一类缺陷：把一个越界输入改写成另一个合法但错误的值，然后照常发出。

标度化是同宽的普通整数，不做缩放。两者都按 int16 存放，但语义完全不同，
这也是把「命令类型」放在 `address.area` 而不是由 `dataType` 推导的原因
（§3.10.9.5）。

**累计量（BCR）的第 5 字节不是「序号」那么简单**：低 5 位是序号，`CY`(0x20)
表示计数期间溢出、`CA`(0x40) 表示计数器被人工调整过、`IV`(0x80) 表示读数无效。
把它们整体丢掉，等于把设备明确说出的「这个读数不能用来结算」翻译成 `good`——
在一个按增量计费的场景里，这是最不该静默的一类错误。映射关系：

| BCR 位 | 本包品质 | 平台质量码 | 含义 |
|---|---|---|---|
| `IV` | `QInvalid` | `bad` | 读数无效，不可用 |
| `CY` | `QOverflow` | `uncertain` | 计数溢出，本周期增量是负数 |
| `CA` | `QSubstituted` | `uncertain` | 人工调整过，差值不代表真实用量 |

带时标累计量 `M_IT_TB_1`（类型 32）是同一组 BCR 语义末尾再跟 7 字节 CP56Time2a，
元素宽度 12：前 5 字节按 `M_IT_NA_1` 解码，后 7 字节必须**消费掉**——只取 BCR、
不跳过时标，会让同一个 ASDU 里后续信息对象的 IOA 整体前移 7 字节，症状是「第一
个电度对上、第二个电度开始串位」。时标本身不改变轮询主站的取值（累计量仍是累计量），
因此解码后保留在 `Object.Time` 供旁路表展示，不参与点位映射。

CP56Time2a 是 7 字节：毫秒(2 小端) + 分(1，低 7 位) + 时(1) + 日(1，低 5 位) +
月(1，低 4 位) + 年(1，低 7 位，偏移 2000)。每一位都有保留位，解码时必须掩掉，
否则「2026 年 1 月」会被读成一个 2126 年的非法时间。

只掩位还不够：`time.Date` 会把越界日期**静默规范化**（2 月 31 日 → 3 月 3 日）。
那是把「设备给了一个非法时间」改写成「一个合法但错误的时间」，比直接判无效危险
得多——错误时间会安静地进历史库，事后无法分辨。因此解码后回读一次 `time.Date`
的规范化结果，年月日时分任一字段被改动就判整条时标无效（返回 `nil`），由上层
按「无时标」处理。

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

**`Read` 的错误契约是「逐点收集 + 全失败才上抛」。** 它只读本地表，每个点位的
坏都有自己的原因（没收到过 / 纯控制方向 / 设备报 IV / 点位没配置），全部写进
各自的样本里，不升级成整轮错误——否则一个属性名写错的点位就会让整台设备每周期
重连一次（§3.3.1）。只有「这一轮一个点位都没成功」时才返回第一个错误，让熔断器
和告警看到异常。

**链路判死是这条契约唯一的例外：即便样本看起来都是好的，也必须上抛。** 此刻
缓存里最后一个值有多旧无从得知，一个可能是几小时前的值带着 `good` 进缓存与告警，
比一次明确的通信故障危险得多。上抛时**必须为每个点位补一条 `comm_error` 坏样本**，
数量与传入点位一一对应：采集器对「驱动返回的点位少了」的处理是再补一条
`protocol/bad`（`runtime/collector.go`），那条兜底消息会把真正的原因（断链/超时）
盖掉，运维看到的就变成「协议错误」而不是「连接断了」。

**刻意不做「多久没更新就降级」。** 这是 104 驱动最容易犯的错：抄一个「30 秒无更新
转 stale」的规则进来，看起来很稳健。但 104 的从站**只在值变化时上送**，一个温度
三十分钟不变是完全正常的；按时间降级会把整页点位刷成 stale，运维于是学会忽略
stale——这比不显示质量码更糟。

链路的活性由 keeper 协程负责：

| 参数 | 默认值 | 作用 |
|---|---|---|
| k | 12 | 未确认 I 帧上限；打满则发送方等确认 |
| w | 8 | 收到但未确认的 I 帧达到这个数就立即确认，不等 t2 |
| t1 | 15s | 发出 I 帧 / TESTFR act 后等确认的上限 |
| t2 | 1s | 收到数据后最迟发确认的延迟上限（标准默认 10s，见下） |
| 空闲阈值 | 20s | 链路上没有任何报文多久之后发 TESTFR act |

t2 取 1 秒而不是标准的 10 秒：t2 是**上限**，早确认永远合法，而 10 秒的延迟会让
对端的发送窗口（同样 12 帧）在整个周期里处于半空状态，一条突发上送密集的链路会
明显降速。t2 从「本批第一帧到达时刻」算起，不是从上一次确认算起——用后者会让一条
稀疏但持续的上送流永远等不到确认。

确认时机有两条并列判据，缺一都会出问题：**待确认帧数达到 w**，或**最早一帧已等满
t2**。只按 t2 判断，密集上送时对端的窗口会被拖住；只按 w 判断，稀疏上送时确认会
一直不发，对端最终判定超时。

**k 窗口的确认必须按 N(R) 精确递减，不能「收到任何帧就清零」。** 对端可以延迟确认，
一个只确认到更早一帧的 S 帧如果被当成「窗口全空」，k 就形同虚设，15 位序号会一路
写穿到回绕——现场表现是「偶发丢命令」，而日志里一切正常（每次发送都成功返回）。
实现上用 `ackPoint` 记住「对端已确认到哪一帧」，只接受单调前进的 N(R)，迟到的旧
确认不得把窗口宽度算大。

**链路判死要能立刻唤醒正在等确认的命令。** `done` 只在驱动被 `Close`（整体卸载）
时关闭，而链路断开后驱动还活着、下一轮会重连。两者语义不同，所以另有一个 `dead`
通道在第一次判死时关闭：正在等激活确认的 `Write` 立刻返回断连，而不是挂满整个
设备超时。没有这条通路，现场表现是「点一次按钮要转十几秒才有反应」——而设备其实
早就连不上了。

选 20 秒空闲探测：远小于绝大多数 NAT 表项的空闲超时（通常 2–10 分钟），
又不会让一条安静但健康的链路每分钟都被打扰。

**TESTFR 没有回应就判死，而不是继续等。** 「设备还在，只是不说话」与「中间的网络
设备悄悄断了」在主站看来是一样的；区别在于前者会在下一次 PING 时回应。所以判死
的条件是「发出 TESTFR act 之后 t1 内没收到 con」，而不是「一段时间没收到数据」。

#### 3.10.9.8 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| 非平衡式（101 串口） | 那是另一套链路层（FT1.2），与 TCP 的序号、启停、超时模型完全不同；混在一个驱动里会让两边都写不清楚。它由独立驱动承担，应用层与本驱动共享，见 §3.10.17 |
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
| `codec/iec104` | APCI 三种帧型的金帧、长度字段语义、U 帧功能码低两位、I 帧空载荷拒绝、ASDU 头与 SQ、IOA 24 位、全部元素宽度、归一化/标度化/短浮点往返、归一化满量程不回绕且往返不漂移、CP56Time2a 保留位掩码、CP56Time2a 拒绝非法日期（2 月 30 日 / 4 月 31 日 / 非闰年 2 月 29 日 / 日 0 / 时 24）、SIQ/DIQ 品质位移、QDS 品质位按标准位序逐位解出（IV@bit7→bad、NT@bit6→stale、SB/BL@bit4/5→uncertain、OV@bit0→uncertain；旧 `& 0x1F` 裸读会让 IV 漏报成 good）、累计量 BCR 的 IV/CY/CA 标志位（解码 + 重编码 + 折成 bad/uncertain）、带时标累计量 M_IT_TB_1（BCR + CP56Time2a，12 字节，时标必须消费否则后续对象错位）、命令构造（选择位）、类型/原因名与别名、越界与截断 |
| `driver/iec104` | 元数据与能力声明、地址规划与全部拒绝路径、STARTDT 握手与超时、站召唤与电能量召唤、I 帧乱序断链、S 帧越界确认、U 帧六个功能码、发送窗口打满与确认放行、k 窗口按 N(R) 精确递减（部分确认不放大窗口、迟到的旧确认与越界确认被忽略）、连接关闭中断等待、keeper 空闲探测与判死、确认时机两条判据（达到 w 立即确认 / 单帧等满 t2 再确认）、命令五种类型与三类错误、否定确认分类、缓存填充与不污染、旁路表（含未配置 IOA、一对多、上限、丢弃计数）、纯控制方向点位的说明文字、回填不覆盖更新的突发上送、**部分失败契约**（混合成功返回 nil / 全失败上抛 / 被取消的 ctx 仍补齐逐点样本 / 链路判死全部标 bad 且样本数与 spans 一致）、**写入批次中止**（链路判死与链路级超时都立即收尾并逐项给质量码，不逐条等超时）、**命令等待响应断链**（不挂满设备超时）、发送超时携带 `timeout` 质量码 |
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
每一位映射成一个独立的判断，而不是 `switch` 成一句话。位号按标准用
D0..D7 标注（D7 是保留位、D0 是「其它错误」，早期文档把 D0 与 D7 写反了，
已校正）：

| 位 | 含义 | 驱动的处置 |
|---|---|---|
| `D1` | 无请求数据 | 该 DI 在表里不存在 → `address_error`（配置问题） |
| `D2` | 密码错/未授权 | `permission`，且**不重试**（重试只会再错一次） |
| `D3` | 通信速率不能改 | `protocol`，标记可重试但无意义 |
| `D4` | 年时区数超限 | `protocol` |
| `bit5` | 日时段数超限 | `protocol` |
| `D6` | 费率数超 | `protocol` |
| `D7` | 保留 | 标准未定义；置位时按 `protocol` 处理并附上原始字节 |
| `D0` | 其它错误 | `protocol`，附原始错误字节 |

把「无请求数据」单独拎出来，是因为它是**唯一一个大概率是配置错误**的异常：地址写错
一位、DI 写错一位、或点表引用了这块表不支持的数据项。其余错误更可能是设备状态或
权限问题，处置方式不同。

异常应答是**合法帧**，因此驱动按点级处理而不是链路故障：它本身不影响后续事务，也
不该触发重连。唯一要区分的是「重发有没有意义」——`Probe` 的读通信地址若收到异常，
说明表收到了命令但拒绝执行（例如不被允许的口令/命令组合），重发结果完全相同，因此
按**不可重试**上报；`dlt645.Exception` 这个具体类型就是用来把这种情况与「帧本身坏了」
分开的，后者可能只是线路噪声，仍保留可重试。

#### 3.10.10.5 值编码：压缩 BCD、低字节在前、小数位在 DI 表

| 数据项 | 宽度 | 编码 |
|---|---|---|
| 电能（`DI3=00`） | 4 | 压缩 BCD，两位小数，最高字节 bit7 为符号位 |
| 电压 | 2 | 压缩 BCD，一位小数 |
| 电流 | 3 | 压缩 BCD，三位小数，带符号 |
| 功率 | 3 | 压缩 BCD，四位小数，带符号 |
| 功率因数 | 2 | 压缩 BCD，三位小数，带符号 |
| 频率 | 2 | 压缩 BCD，两位小数 |
| 时间 | 6 | `ss mm hh DD MM YY`，每字节 BCD，**字段有范围校验** |
| 日期（结算日等） | 4 | `MM DD hh mm`，字段序（**不是**小端整数） |
| 最大需量 | 8 | 前 4 字节需量（小端 BCD）+ 后 4 字节发生时间（`MM DD hh mm`） |
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

**时间/日期字段的范围校验与静默归一化。** `ParseTime` 在 BCD 半字节合法之后还会
校验各字段取值范围（月 1..12、日 1..31、时 0..23、分/秒 0..59），日期数据项走同一套
规则（`codec.ValidDateParts`，读写共用）。为什么必须做：`time.Date` 会把「秒=72」
这类垃圾**静默归一化**成合法时间（72 秒 → 下一分钟 12 秒），月=19 → 次年 7 月，
2 月 30 日 → 3 月 2 日，且都不报错。电表时钟是最不能「猜」的数据项——读到一个假的
07:06 比读不到更危险，因为它会参与结算与统计。

**字节序不要一概而论。** 数值型数据（电能、电压、电流）是「低字节在前」的 BCD；
而时间/日期数据项是**字段序**（`MM DD hh mm`），既不是小端整数也不是大端整数。
`Decode` 的 `KindDate`/`KindDemand` 按字段序解析，`EncodeDate` 按 `MM DD hh mm`
编码。早期 `Decode` 用 `decodeBCD(raw, true)` 把小端整数当日期——0x31 会被当成符号
位，「结算日 12-31 23:59」会被读成负数；`EncodeDate` 走 `encodeScaled` 又编出
`DD MM hh mm`，与 `Decode` 相反，是个真实的编解码不对称缺陷（已修）。

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
points = flatten(spans)   # 摊平后才能「从任意下标把剩下的点标坏」
for i, point := range points:
  if ctx 到期:        标记 i.. 全部坏点，立即上抛 timeout
  if linkErr != nil:  标记 i.. 全部坏点，立即上抛该错误
  send 0x11 (DI)
  if 异常:            标记该点坏（address_error / permission），continue
  while 应答有后续帧:
    send 0x12 (DI, SEQ++)
    append 数据
  校验长度（非数据块必须精确等于 DI 宽度）
  解码 → 平台值
if 无成功点: 上抛第一个错误
```

三处刻意的设计：

- **不合并。** 规划层按 `area`（即 DI）分组，不同 DI 不合并；同一 DI 的多个点位共用
  一次事务的结果。这与 Modbus 的「相邻地址合并成一个请求」不是一回事——645 里两个
  相邻 DI 在语义上可能毫不相干（A 相电压与 A 相电流差一个字节）。
- **必须跟完后续帧。** 一条被截断的电能读数会以「比实际少了几位」的形式入账，而且
  看不出任何异常。驱动在**同一次 `Read` 内**跟完，上限是**双重**的：帧数
  （`maxFollow`，默认 64 帧）与累计字节数（`MaxData × maxFollow`，约 12.8KB）。
  只做字节上限挡不住「每帧只回 1 字节、且永远置后续位」的坏表——按每次超时窗口
  算，一轮采集会从几十秒拖成几十分钟；只做帧数上限则挡不住满帧。
- **链路级故障短路整轮，并把收尾说完整。** 超时/断开/`ctx` 到期后，剩余点位快速
  标记为同样的错误，而不是每个点都等一次超时。300 个点位挂在 2400bps 总线上，一轮
  本该几十秒——再乘以点数就是事故。收尾必须同时满足三件事：已读到的样本**原样保留**、
  剩余点位**显式补 `bad` 样本**、链路错误**即使有成功点也要上抛**（`driver.PartialReadError`
  只管「点级失败」的部分失败语义，链路级是它的例外，见 §3.3.1）。少给样本会被采集器
  当成「驱动漏报」，而吞掉链路错误会让调度器看不见已经断掉的链路。
- **质量码按错误种类派生**（`timeout` / `comm_error` / `address_error` / `bad`），
  不一律写 `comm_error`。运维看到 `comm_error` 会去查线路，而真正要改的可能只是点表。

长度校验落在驱动而不是 codec：codec 只负责「这一段字节是什么」，驱动才知道「这个
DI 按标准应该多宽」。但 codec 自己也守一道**精确宽度**：`Decode` 拿到与 `Item.Width`
不符的字节数直接报错（数据块除外，其宽度由请求决定），`ParseTime` 也只接受 6 字节。
早先这几处只检查下界，多给的字节会被静默忽略——时间/日期/地址/需量都只取前 N 字节，
于是「DI + 时间」整段误传也能解出一个看起来正常的时间。

#### 3.10.10.7 写：三道闸门 + 一个被拒绝的开关

| 闸门 | 配置 | 控制什么 | 缺失时的行为 |
|---|---|---|---|
| 1 | `options` 含 `allow-write` | 普通写数据（`0x14`） | `Open` 失败，明确报错 |
| 2 | DI 表 `Writable` | 标准里该数据项是否可写（电压/电流/电能永远只读） | `Write` 返回 `permission` |
| 3 | 设备密码 | 电表自己的权限等级（`PA` 高 4 位 + `P0..P2`） | 由设备返回异常应答 |

三道闸门是「与」而不是「或」。一个「能读」的驱动不应该顺手能把闸拉了。

**`allow-control` 不是闸门，会被显式拒绝。** 拉合闸（`0x1C`）与清零
（`0x19`/`0x1A`/`0x1B`）是另一套数据域形状：

```
密码(4) | 操作者(4) | 控制类型(1) | 保留(1) | 时间(6)
```

控制类型 `0x1A` = 跳闸、`0x1B` = 合闸允许；时间给的是「允许执行的时间窗口」。codec
保留了 `RelayRequest` 的组装能力，sim 也实现了应答，但**驱动没有调用入口**。早期
实现里 `allow-control` 被 `parseOptions` 解析、写进 `ConfigFields.Help`、文档列为
「闸门 2」——而没有任何代码读取它。一个死掉的高危开关比没有开关更危险：运维写了
`allow-control` 会以为拉闸已经授权，实际什么也没打开；将来若有人补上控制入口，
一个陈旧配置会**默认**已经放行。因此现在 `Open` 见到它直接报 `ErrUnsupported`，
错误信息里点名 `allow-control` 并给出出路（改用厂商工具）。CLI 的 `-allow-control`
同样在命令层先拦并 `exit(2)`。

顺带一个原则：**不提供入口 > 提供入口但默认关闭。** 广播命令（见下）走的是同一条
路——没人能靠改一个布尔值把它打开。

写数据域的形状与读完全不同：

```
DI(4) | PA P0 P1 P2(4) | C0 C1 C2 C3(4) | 参数(m)
```

- `PA` 高 4 位是密码权限（`0x02` = 普通表明文），低 4 位是密码等级；
- `P0..P2` 是密码，来自 `options` 的 `password=`（6 位十六进制）；
- `C0..C3` 是操作者代码，仅用于设备侧追溯，驱动默认全 0。

写结果是**逐项结果**：失败项必须带 `Quality`（`timeout` / `address_error` / `bad`），
留空会被 `runtime/collector.go` 兜底成 `bad`，于是「超时」「参数被拒」「设备不认这个
DI」在下游分不出来。链路级失败（超时/断开）时，同批剩余写命令不再发送、逐项标坏，
并把这个错误经 `Write` 的第二个返回值上抛；`ctx` 到期同样一次收尾，而不是把「超时」
乘以条数。已经成功的条目保持 `OK=true`，不得被回填成失败（§3.4 规则 4）。

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

**适配器回显。** 部分 USB 转 485 / 两线半双工适配器会把主站刚发出的字节原样回显给
接收端，于是主站先收到自己的请求帧、再收到真正的应答。请求帧 D7=0、任何从站应答
D7=1，因此回显可以被无条件识别。驱动在事务层跳过 D7=0 的帧继续等应答；codec 层的
应答解析（`WriteAck`/`ParseReadReply`/`ParseAddrReply`）则**直接拒绝** D7=0 的帧。
两层都要有：只有 codec 拒绝，这类适配器会因「把回显当协议错」而完全不可用；只有驱动
跳过，写路径会把回显当成 `0x94` 成功应答，出现「写成功但设备没收到」。

帧间静默 `gap` 默认 30ms：标准只给了「帧间 >20ms」的宽松下限，取 30ms 覆盖 USB 转
485 的批量传输延迟（实测 5~20ms），又不至于在 300 台表的总线上把帧间隔放大到无法
接受。前导字节数可配（`preamble=0`），因为现场确有一批适配器在前导之后丢第一个字节，
表现为「不发 FE 能通、发了就不通」。

#### 3.10.10.9 能力边界（刻意的）

| 不做 | 原因 |
|---|---|
| 97 版协议 | 帧结构、控制码、DI 表都不同；混在一个驱动里会让「这块表到底按哪版解释」变成运行时猜谜 |
| 广播校时/广播冻结 | 见 §3.10.10.7：一条配错的广播会同时改掉总线上所有表的时间/需量；驱动不提供入口 |
| 主动上报（事件/报警） | 645 的事件上报依赖厂商扩展，标准未统一；不同表的上报帧形状差异极大 |
| 合并读 | 一个 DI 一次事务；DI 之间没有「连续」概念 |
| 位寻址 | 状态字按 `uint16` 整体读出，由上层按位解释；驱动不猜哪一位叫什么 |
| 自动发现 DI | 没有「列举数据项」这种服务；DI 必须来自手册/点表 |
| 表地址自动扫描 | 总线上可能有几十台表，逐个试地址会打断正常采集；应由运维显式发起 |
| 拉合闸/清零 | codec 有 `RelayRequest`、sim 有应答，但驱动没有调用入口。见 §3.10.10.7：`allow-control` 会被 `Open` 显式拒绝，而不是静默忽略 |
| 广播校时/广播冻结 | 一条配错的广播会同时改掉总线上所有表的时间/需量；驱动不提供入口 |

因此 `Meta().Capabilities` 里 `Subscribe`、`Browse`、`Discovery`、`MultiRead`、
`BitAccess`、`StructTypes` 全部声明 `unsupported`。`Read`/`Write` 声明 `supported`。

#### 3.10.10.10 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/dlt645`（`Parse`/`Build`/`ReadRequest`/`ReadNextRequest`/`ReadAddrRequest`/`WriteRequest`/`BroadcastTimeRequest`/`RelayRequest`/`ParseReadReply`/`ParseReadNextReply`/`ParseAddrReply`/`WriteAck`/`DIFromBytes`/`ParseDI`/`Lookup`/`Width`/`Decode`/`Format`/`EncodeValue`/`EncodeTime`/`ParseTime`/`EncodeAddr`/`Sum`/`Reader.Next`） |
| 主站驱动 | `internal/driver/dlt645`（`Meta`/`Open`/`Probe`/`Read`/`Write`，内部 `exchange`/`readPoint`/`readNext`/`sample`/`classify`/`qualityOf`/`resolve`/`parseOptions`） |
| 内置从站 | `internal/sim/dlt645`（`NewServer`/`ServeStream`/`SetValue`/`Value`/`Relay`/`Writes`/`Requests`/`Address`，选项 `WithAddress`/`WithPassword`/`WithDropAfter`/`WithoutValue`） |
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

`dlt645read` 的退出码可以当判据用：一个数据项都读不到、或出现链路级错误时返回 1；
部分成功且无链路错误仍是 0（§3.3.1）。JSON 输出带 `ok`（成功条数）与 `error`
（无错时为 `null`，而不是 `"unknown"` 这类非空值）。全失败也返回 0 会让脚本把「表
不认这些 DI」当成成功，这正是自检工具最不该有的行为。

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
| `driver/dlt645` | 元数据与能力声明、探针成功/错表拒绝、**探针异常应答不可重试**、电能/电压/电流/需量解码、未收录 DI、未知 DI→`address_error`、后续帧（250 字节重组 + 前 4 字节校验）、写需 opt-in、写时钟往返、结算日 8 位值原样写回、非法日期（13 月/2 月 30 日）本地拒绝、只读 DI（含状态字）拒绝、缺 `labels.address`、超时分类、**部分失败语义（坏 DI + 好点 `err==nil`、全失败上抛）**、链路中断保留已读样本并把剩余点标坏后上抛、`ctx` 到期把剩余点标坏、**写失败质量码**（`timeout`）、写链路失败中止剩余并经第二返回值上抛且不连坐已成功项、异常位→错误分类 |
| `sim/dlt645` | 真实 TCP 回环：标准宽度、忽略他表地址、无数据异常位、写权限/密码/只读、后续帧拆分与重组（500 字节）、读地址、广播校时静默且生效、拉合闸 + 非法动作拒绝、`Close` 幂等、`ServeStream` over `net.Pipe`、`WithDropAfter` 模拟中途断链、`WithoutValue` 只让单个 DI 回异常而其余 DI 仍正常 |
| 点表/规划 | DI 三种写法与全部拒绝路径、`WidthProtocol` 策略、不同 DI 不合并、同 DI 同 span |
| `cmd/uic` | DI 列表去重/非法 DI/全 FF 拒绝、时间五种写法与非法日期拒绝、日期类 8 位 `MMDDhhmm` 与完整时间、地址缩位写拒绝、数据块走 `bytes`、传输方式按参数推断、`dlt645ReadFailed` 退出码（全失败/链路错误 → 1，部分成功 → 0） |

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
| 数据域 | L | **不加 `0x33`**；地址与数值低字节先传，但**数据标识 DI 高字节先传**（大端，见下） |
| 校验和 | 1 | 从 `0x68` 到 DATA 末的模 256 和；前导与结束符不计 |
| 结束符 | 1 | 固定 `0x16` |

帧总长（不含前导）恒为 `13 + L`：`68 + T + 7 地址 + C + L + DATA + CS + 16`。金帧
（读 `00000805000001` 的 901F，SER=0）在代码里逐字节钉住：

```
FE FE 68 10 01 00 00 05 08 00 00 01 03 90 1F 00 39 16
```

三个最容易写错的点：

1. **只有一个 `68`。** 从 645 抄过来的解析器会去找第二个起始符，于是永远停在
   `ErrIncomplete`。
2. **数据域不减 `0x33`。** 多做一次 `-0x33` 会让每个数值都小 51；BCD 半字节检查
   多半还能通过，读出来是一组「合法但错」的数。
3. **CS 不含前导。** 把 `FE` 算进去，所有带前导的帧都会校验失败——而现场的表几乎
   都要求前导。

还有一处**最容易整段写反**的：

4. **数据标识 DI 是大端（高字节先传）。** 901F 在帧里是 `90 1F`（DI0=0x90 在前），
   不是 `1F 90`。标准 6.4.2「所有多字节数据域均先传送低位字节」针对的是数据**数值**
   与地址；DI 属于命令/应答的标识字段，2004 版按大端书写——标准表 4/表 5 与现场水表
   抓包（`...03 90 1F...`）都印证。发成 `1F 90` 时帧本身合法，但表把它当成一个不
   存在的标识而不应答。注意这是 **2004** 的规定；CJ/T 188-**2018** 把 DI 改成了
   小端（`1F 90`），并在标准前言里单列「明确了数据标识的字节顺序要求」作为对 2004
   的主要改动之一（同期还把 SER 更名为 SEQ）。本实现面向 2004，故取大端。

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
| 读地址 | `03` | `83` | `81 0A SER` | `81 0A SER`，地址在帧头 |
| 写数据（阀门） | `04` | `84` | `A0 17 SER 55/99` | `A0 17 SER + 状态(2)` |
| 写地址 | `15` | `95` | `A0 18 SER + 新地址(7)` | `A0 18 SER` |
| 异常应答 | — | 请求 C \| `0x80` \| `0x40` | — | `DI0 DI1 SER` |

数据标识一律按 2004 的**大端**书写（DI0 是高字节），因此三个特殊报文标识在帧里
依次是读地址 `81 0A`、阀门 `A0 17`、写地址 `A0 18`：

- 它们与 901F 走**同一套** `DI.Bytes()` 变换，不再有「字面量例外」。早先把读地址当
  成不走字节序变换的字面量，是因为（错误地）按小端理解 DI；一旦按 2004 的大端，
  `DIReadAddr.Bytes()` 就直接给出 `81 0A`，无需特殊处理；
- 写地址的数据域共 `0x0A` 字节：2 字节标识 + 1 字节 SER + 7 字节新地址。

异常应答只带标识与 SER，不带错误码：188 没有 645 那样的 `ErrCode` 字节，「为什么
被拒」只能由主站从「命令 + 参数」推断。驱动的错误信息因此保留 DI 与 SER，便于对照
是哪一次请求。

异常应答是**合法的帧**，因此驱动按「点级/命令级」处理，不当成链路故障：读数据收到
异常只把该点标 `address_error`，不重试、不计入熔断，同轮其余点照常读；写数据收到
异常标 `bad`，同样不重试（同一条被拒的命令重发多少次都是同一个结果）；`Probe` 的
读地址收到异常则按**不可重试**上报——否则上层会对着一台明确拒绝的表反复探测，每轮
都白等到超时。三种处置共用 `cjt188.Exception` 这个类型来区分「表拒绝执行」与「帧
本身坏了」：后者可能只是线路噪声，仍保留可重试。

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

- `area` 是 4 位十六进制 DI，按**手册顺序**书写；上线时按 CJ/T 188-2004 的**大端**
  传输（`901F` → `90 1F`，DI0=0x90 在前）。注意这与地址、数值的「低字节先传」相反，
  是 DI 独有的例外；2018 版才改成小端。
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

任一条不满足都归为 `protocol` 错误，并保留原始字节用于排查。

`Read` 的收尾遵循 §3.3.1 的统一契约，不是本协议自定的口味：

- 地址/字段/协议错只标当前点，其余点照常读；全轮都失败才把第一个错误上抛；
- 链路级故障（超时、断开、`ctx` 到期）**立即收尾**：已经读到的样本原样保留，
  剩余未发出的点位显式补 `bad` 样本并带上同一错误，然后把错误上抛——有成功点
  也不例外。少给样本会被采集器当成「驱动漏报」，而逐个等超时会把一轮采集拖成
  「点数 × 设备超时」；
- 失败样本的 `Quality` 按错误种类派生（`timeout` / `comm_error` /
  `address_error` / `bad`），不能一律写 `comm_error`：下游的告警分级与熔断计数
  依赖这个区分。

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

写失败同样是**逐项结果**：`OK=false` 时必须带 `Quality`（`timeout` /
`address_error` / `bad`），留空会让 `runtime/collector.go` 兜底成 `bad`，于是
「超时」「地址写错」「权限不足」在下游分不出来。链路级失败（超时/断开）时，同批
剩余写命令不再发送，逐项标坏，并把这个错误经 `Write` 的第二个返回值上抛；已经成功
的条目保持 `OK=true`，不得被回填成失败（§3.4 规则 4）。

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
| 主站驱动 | `internal/driver/cjt188`（`Meta`/`Open`/`Probe`/`Read`/`Write`，内部 `readDI`/`readPoint`/`resolveRead`/`writeValve`/`writeAddr`/`exchange`/`parseOptions`/`qualityOf`） |
| 内置从站 | `internal/sim/cjt188`（`NewServer`/`ServeStream`/`SetClock`/`SetValue`/`Value`/`Valve`/`Writes`/`Requests`/`Address`/`Type`，选项 `WithAddress`/`WithType`/`WithDropAfter`/`WithoutValue`） |
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

自检命令的退出码可以当判据用：`0` 表示至少读到一个字段且没有链路级错误；全部字段
失败、或出现链路级错误（即便部分字段已成功）都返回 `1`。只把「全部失败」当失败会
让脚本把「设备不认这些 DI」和「读到一半断链」当成一次成功的抄表。写权限也按报文
标识分别授予：`A017` 只开 `allow-control`，`A018` 只开 `allow-write`，不会因为
「这次有写操作」就同时打开两个闸门。

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
| `codec/cjt188` | 金帧（读请求/水表应答/读地址）、单 `68`、数据域无 `0x33`、CS 不含前导、地址 BCD 低字节先传、BCD 半字节校验（`FF`/`A` 拒绝）、时间 7 字节往返与非法日期、901F 水/热两套 schema 与总宽（19/43）、D120..D12B、单位换算表与未知代号拒绝、状态字文本、异常应答、噪声恢复、`Parse` 的 `consumed` 契约（带前导噪声时返回 `start+总长`，不只返回 `total`）、写阀门/写地址帧 |
| `driver/cjt188` | 探针成功/错表拒绝、水表四字段共享一次事务、时间与状态、历史月量、热量表全字段、单位换算、阀门权限与开/关、写地址后连接切换、短地址拒绝、非支持 DI、错误 offset、长度不符、错地址超时（样本质量为 `timeout`）、前导 0/4、坏配置拒绝、**部分失败语义（坏地址+好点 `err==nil`、全失败上抛）**、链路中断保留已读样本并把剩余点标坏后上抛、**写失败质量码**（`address_error`/`bad`/`timeout`）、写链路失败中止剩余并经第二返回值上抛且不连坐已成功项、异常应答的点级归属（读→`address_error` 不重试、写→`bad` 不重试、探针→不可重试）、异常应答不打断链路 |
| `sim/cjt188` | 真实 TCP 回环：标准宽度、地址过滤静默、读地址用真实地址、阀门写与状态刷新、未知命令异常、写地址后新址可读旧地址静默、通配写地址拒绝、`SetClock` 刷新、`SetValue`、`ServeStream` over `net.Pipe`、`Close` 幂等、`WithDropAfter` 模拟中途断链、`WithoutValue` 只让单个 DI 回异常而其余 DI 仍正常 |
| 点表/规划 | 4 位 DI、字段偏移允许非 0、8 位 DI 与全 `FFFF` 拒绝、bool 拒绝、`WidthProtocol` 策略、不同 DI 不合并 |
| `cmd/uic` | 类型解析、`DI[:offset[:length]]`、偏移越界/长度不符/未收录 DI、历史 DI、status 走 `uint16`、阀门值 open/close、写 DI 只认 A017/A018、传输方式按参数推断、自检退出码（全失败或链路错误 → `1`） |

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

这三件事决定了本节的两个核心设计：**读按字节预算分批**（§3.10.12.5）、
**v1 的 `noSuchName` 逐点重发**（§3.10.12.6）。v3 的安全层单独成节（§3.10.22），
它与 v1/v2c 共用同一套 OID 地址模型与批量策略，只是外面多了一层 USM 封套。

#### 3.10.12.1 版本矩阵与 v3 的边界

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

v3 的外层报文与 v1/v2c 完全不同（`HeaderData` + `msgSecurityParameters` + 数据部分），
因此 `Build` / `Parse` 仍只处理 v1/v2c：`Build`/`Parse` 遇到版本 3 直接报错并把调用方
指向 `BuildV3`/`ParseV3`（`ParseVersion` 本身接受 `3`，由驱动决定走哪条路径）。
**不退回 v2c 碰运气**——用 v2c 的报文去连一个只开 v3 的代理，得到的是超时；现场会
把它误判成「网络不通」，而真正的原因是版本配错。

团体名按凭据对待：`labels.community` 在配置表单里是 `password` 类型（界面掩码），
默认值 `public` 只是「第一次接通最少配置」，不是安全默认。文档与失败提示都要求
现场显式确认它。v3 的用户名/认证口令/加密口令同样按凭据处理，且口令只从环境变量
或配置的 Secret 字段读，不落日志（见 §3.10.22）。

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

首个子标识符还有一条容易被漏掉的边界：它是 `40 × 第一弧 + 第二弧` 的**合成值**，
当第一弧为 2 时第二弧没有上界，于是这个值最大可到 `40×2 + (2^32-1) = 2^32+79`，
**超过 32 位**。若按 32 位计算，`2.4294967295` 会回绕成 79，编出来的字节与
`1.39` 完全相同——报文合法、代理不报错，数据却悄悄落到另一个对象上。因此编码侧
用 64 位计算首个子标识符（5 字节 base-128），解码侧对首个子标识符放到 `2^32+79`、
对其余子标识符仍卡 32 位：两侧上限必须严格对称，否则就是「写得出、读不回」。

单个 OID 最多 128 个子标识符（RFC 2578 §3.5）。超长 OID 只会来自自动生成的拼接
错误或人为拼包，本地直接拒绝并指到那个点位，比发出去换一个 `tooBig`/`genErr`
更容易定位。

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

链路级失败（超时、断链）会让**本轮剩余批次全部快速失败**，不再逐批等超时，并
立即以该错误返回（§3.3.1 的例外条款）。这条规则同样适用于 §3.10.12.6 的 v1 逐点
退化路径：退化到一半超时，**前半段已经读到的点位必须保留**，只把剩下的标坏并立刻
结束本轮；不能因为「这条路径最终返回了错误」就把同批的成功样本一起丢掉。

地址级错误只标记该点，不影响其它点位的
质量码，也不进入 `Read` 的汇总错误；其余点级错误（如线类型与 `dataType` 不符
→ `protocol`）同样只标该点——只要还有一个点位成功，整轮就是 `err == nil`。
全部点位失败才上抛第一个错误，让熔断器看到「这台设备全坏了」。

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

写失败的结果同样带质量码，按 §3.10.12.8 的映射给出（`address` → `address_error`、
超时 → `timeout`、断链 → `comm_error`、其余 → `bad`）。**不留空**：上层 collector
对空值会补 `bad`，那样「点位写错」「设备拒写」「超时」在下游就成了同一个值，而
现场排查最先要分的正是这三类。读路径的 `errSample` 一直如此，写路径没有理由例外。

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
| v3 / USM（在本层） | 外层报文结构完全不同，单独在 §3.10.22 实现；本层遇到版本 3 显式拒绝，不退回 v2c |
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
| 内置代理 | `internal/sim/snmp`（`NewServer`/`NewServerWithOptions`/`Addr`/`Get`/`Set`/`SetWritable`/`Requests`/`Writes`/`Dropped`/`Close`；`Options{Community,MaxBulk,V3,V3Only}`，`V3Only` 用于复现「只开 v3」的代理：v1/v2c 静默丢弃） |
| CLI | `uic snmpread`（`-probe`/`-oid`/`-walk`/`-max-walk`/`-set`/`-setType`/`-retries`/`-raw`/`-json`）；退出码约定见下 |

**自检 CLI 的退出码**：`0` = 至少读到一个点且没有链路级失败；`1` = 一个点都没
读到，或出现链路级错误（超时/断链），或在 v1 退化路径中途断掉。部分成功且无链路
错误仍是 `0`，与 §3.3.1 的部分失败语义一致——但「读到一半链路断」不算部分成功：
这一轮不完整，脚本不能拿它当全读到了。JSON 输出同步带 `ok`（成功行数）与 `error`
（链路错误文本，无错误时为 `null`），供脚本精确判断。
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
| `codec/snmp` | GET/应答金帧、INTEGER 最短编码与负数、无符号前导零（`0x80000000`）、OID base-128（`2.39.1`→`77 01`）、OID 排序与前缀、非法 OID、**首个子标识符的 `2^32+79` 边界（`2.4294967295` 往返、与 `1.39` 不撞车、越界与超长续字节拒绝）**、**128 弧上限（编解码两侧）**、版本解析（v3 拒绝）、异常标记只允许出现在应答、GetBulk 线格式（**负数计数拒绝**）、错误码分类、字符串/字节值、截断错误可识别 |
| `driver/snmp` | 五种线类型解码、MultiRead 一条 GET、按 maxOids 与字节预算分批（顺序不变、每条 ≤1400 字节）、v1 `noSuchName` 逐点重发、**v1 退化中途超时保留已读样本并短路后续批次**、写类型映射与回显校验、**写失败带质量码（timeout/address/bad 三档）**、类型/范围拒绝不发包、9 种 error-status 分类、SET 回显被收敛、Probe 接受异常标记/超时提示、request-id 陈旧应答丢弃、链路失败停止后续批次、坏地址与线类型不符、Counter64 精度边界、`GetRaw`/`Walk`（子树边界、max、v1 表尾）；**部分失败语义（好点 + protocol 错时 `err==nil`、全失败才返回错误）** |
| `sim/snmp` | 真实 UDP 回环：GET 与计数器、错 community 静默、v1/v2c 缺 OID 差异、GETNEXT 遍历与表尾、GETBULK 三行递增、SET 可写与回读、只读/类型错/不存在、SET 原子性、坏报文与非请求丢弃、**`V3Only` 只开 v3（v1/v2c 静默丢弃 + v3 仍可用）**、`Set`/`SetWritable`/`Close` 幂等 |
| 点表/规划 | `PolicyFor("snmp")` 只允许 symbol、selector/寄存器/对象地址在导入期拒绝、`examples/snmp.json` 通过 `-check`（当时 21 个驱动） |
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
  `options.strlen`（默认 81）。CSV length 列的上限跟着协议走：ADS 这类
 `AllowSymbolLength` 协议到 65535（与地址串写法同口径），寄存器型协议
 （Modbus/S7…）仍是 125——否则同一个 200 字节变量写 `NAME:200` 能过、
 写 length 列却被「<= 125」拒，同一份点表两种写法两种结果。
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
| `string` | 点表长度或 `strlen` | 读：定长缓冲按**首个 NUL** 截断；写：右侧补 NUL，刚好占满整段时拒绝（没有结尾 NUL） |
| `bytes` | 点表长度或 `strlen` | 读：原样返回；写：右侧补 0，允许刚好占满 |
| `bcd16` / `bcd32` | — | ADS 没有 BCD 线类型，读写在发报文前报 `unsupported` |

数值用 `codec/value` 的 `Encode`/`Decode` 配合 `model.OrderDCBA`：`DCBA` 表示
「整体倒序」，对一个纯小端数值等价于不做字内交换。把这段逻辑放在统一的 value 层，
是为了保证 ADS 与 Modbus/S7 对同一个字节串的解释只在字节序上不同，而不是各写一份
符号扩展。

写入前做范围检查（`checkRange`）：`70000` 写进 INT16 会被强制转换静默变成
`4464`，写入「成功」了，但设备上的值已经不是调用方给的值。这类错误比写入失败
更难发现，必须在发报文之前拦下。整型的非整数值（`1.5`）同样拒绝。**浮点同样要
查**：`float32` 走 `codec/value` 的 `float32(v)` 强制落地，`1e300` 会被静默写成
`+Inf`、`1e-50` 会被静默写成 `0`（与 DNP3 的 `analogFitsWire` 同一类检查）；
`NaN`/`Inf` 无论 float32 还是 float64 都拒绝——一个 PLC 变量上的「非有限值」
几乎一定是上游算错了，落库后还会把 JSON 编码打成非法值。

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
设备；超时/断链会停止本轮剩余点位（链路已经不可继续，判据是 `driver.IsLinkFailure`）。
这条区分让「一个变量名写错」不会把整台 PLC 标红。

失败样本的**质量码按错误种类派生**（`qualityOf`，与 `runtime/collector` 同口径）：
超时 → `timeout`、变量名写错 → `address_error`、断链 → `comm_error`、权限/不支持/
协议 → `bad`。这与读路径的 `errSample`、写路径的 `writeErr` 都走同一函数。早期这两处
写死 `bad`，导致「PLC 没应答」与「符号写错」在运行时侧无法区分——OPC UA 客户端只能看到
`BadNoData` 而不是 `BadTimeout`/`BadNodeIDUnknown`（违反 §3.4 规则 5）。

汇总按 §3.3.1：`goodSamples > 0` 时 `Read` 返回 `nil`——否则 `collector` 会按错误
分类 `Close()` 连接，一个 `BCD`/宽度配错的点位就能让整台 PLC 每周期重新走 AMS
握手与句柄重解析。全部点位失败才上抛第一个错误。

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
| `driver/ads` | 真实 TCP 回环：9 种类型读写、数组与字符串、地址/权限/宽度错误逐点隔离、坏 NetId/端口、握手超时、Close 幂等、句柄失效重解析（读与写）、类型范围/小数/BCD/字符串 NUL/float32 溢出与 NaN 拒绝、脚本化服务端：临时 AMS 错误重试、通知帧与陈旧 invoke 丢弃、畸形帧、句柄 0/短句柄、设备信息/状态/控制的 AMS 错误与短载荷、ctx 超时、拨号失败；**部分失败语义（好点 + unsupported 错时 `err==nil`、全失败才返回错误）**；**失败样本质量码按错误种类派生（未知符号 → `address_error`、超时 → `timeout`）；定长字符串按首个 NUL 截断、拒绝内嵌 NUL；`valueLen` 错误只包一层（op 标 write、信息不二次嵌套）** |
| `sim/ads` | 真实 TCP 回环：默认变量表、GET/PUT 夹具、句柄分配/释放/失效、只读与宽度校验、数组仅 `[2]` 可写、字符串/浮点、错误 NetId/端口、未知命令、通知不应答、畸形载荷、坏帧断链后仍可服务、Close 幂等 |
| 点表/规划 | `PolicyFor("ads")` 只允许 symbol、`NAME:81` 与 length 列、`MAIN.arr[2]` 保留在 path、寄存器/selector 在导入期拒绝、`examples/ads.json` 通过 `-check`（当时 21 个驱动） |
| `cmd/uic` | `-symbol` 声明解析（类型/长度/错误写法）、`-set` 值解析（bool/int/uint/float/string/bytes、进制、越界、BCD 拒绝）、`-control` 映射、行显示与错误质量 |

### 3.10.14 DNP3（IEEE 1815：三层链路 + 类轮询 + 直接控制，TCP 20000）

DNP3 是本仓库实现的最「重」的协议：它有三层封装（链路 / 传输 / 应用）、两套序号
（传输层 6 位、应用层 4 位）、三种确认（链路 ACK、应用层 CONFIRM、以及从站对主站
确认用户数据的 ACK），还有一套与「读寄存器」完全不同的数据通路。把 DNP3 当成
「带 CRC 的 Modbus」来做，通常能连上、能拿到首批数据，然后在两小时内因为事件缓冲区
被撑爆而开始返回旧值。本节按「为什么这样设计」而不是「协议字段是什么」来组织。

#### 3.10.14.1 范围：只做主站侧的调度子集

| 做 | 不做 | 原因 |
|---|---|---|
| TCP 主站 | 串口（DNP3 Serial） | 串口需要链路层时序（RTS 翻转、字节间静默）与本地从站地址协商，与现有共享串口层不是同一种东西 |
| 类 0 总召唤 | 类 0 单组召唤（组 1 only、组 30 only） | 现场总召唤的成本是点表规模的线性函数，单组召唤只在带宽极窄的电台链路上有意义；本平台面向以太网 |
| 类 1/2/3 事件轮询 | 事件缓冲区配置（组 50 变体 1 的读取与重配） | 配置事件缓冲区需要写操作与设备厂商参数，做错会让从站丢事件；默认由工程侧一次性配好 |
| 接收并确认非请求响应 | 主动触发非请求响应 | 触发是测试动作，不是生产动作 |
| CROB 直接控制 | 选择-执行（SBO）两段式 | SBO 需要维护「已选择」的会话状态并处理超时撤销；直接控制在绝大多数现场已足够，且回显同样能证明设备接受 |
| 模拟量输出（组 41）直接控制 | 模拟量输出块（组 42，多值同时下发） | 组 42 的「块」语义要求从站原子应用一组值；单值控制已覆盖绝大部分设定值场景 |
| 接收应用层设备重启、时间同步请求并回「功能不支持」 | 实际执行时间同步写路径 | 写时间需要考虑时区、夏令时、从站时间质量与审计；半成品的时间同步比不做更危险 |
| — | Secure Authentication（安全认证） | 需要证书/预共享密钥、挑战-应答状态机与防重放窗口；单独是一个子系统 |

这条边界与 IEC 104 的那份刻意保持一致：两个协议都是调度侧主站，能力形状相同，
差别在编码与时序，而不是在「谁更全」。

#### 3.10.14.2 三层封装，每层都有自己的确认

一条 DNP3 报文从外到内是：

```
链路帧（05 64 LEN CTRL DEST(2) SRC(2) CRC(2) + 每 16 字节一块的数据 + 块尾 CRC）
  └─ 传输分片（1 字节：FIR|FIN|SEQ(6) + 应用报文片段）
       └─ 应用报文（控制字节 + 功能码 + [IIN(2)] + 对象区）
```

三层各有各的「已收到」：

| 层 | 确认方式 | 谁发 | 不确认的后果 |
|---|---|---|---|
| 链路 | 二级帧 `ACK`（功能码 0） | 收到「确认用户数据」的一方 | 发送方卡在等待，事务超时 |
| 传输 | 不单独确认；靠分片序号重组 | — | 分片丢失时整条应用报文拼不出来，最终表现为应用层超时 |
| 应用 | 响应里的 `CON` 位要求对方回 `CONFIRM`（功能码 0） | 收到带 CON 响应的一方 | **从站保留事件并在下一轮重发**，最终事件缓冲区溢出、新事件被丢弃 |

链路 ACK 与 CRC 一起，保证了「坏帧不会被当成好帧」。CRC 是 CRC-16/DNP（反射多项式
`0xA6BC`，初值 0，结果取反），校验值 `CRC("123456789") == 0xEA82`，这个已知答案被写进
单元测试。CRC 不是对整帧算一次，而是**每 16 字节用户数据一块、块尾插两个字节**；最后
不满 16 字节的块照样补 CRC。把 CRC 当成整帧校验是移植时最常见的错误之一：帧头能过，
数据一长就错。

`LEN` 字段的口径也要钉死：`LEN = 5 + 用户数据长度`（5 = 控制 1 + 目的地址 2 + 源地址
2），**不含** CRC 与起始字节。多算或少算一个字节，接收端会按错误的边界切帧，然后把
后面几十毫秒的字节流全部当成坏帧。

#### 3.10.14.3 链路控制字节：方向与「主/从」不是同一个位

链路控制字节里有两个容易混淆的位：

- `DIR`（0x80）：方向。主站发出的帧置 1，从站发出的帧置 0。
- `PRM`（0x40）：报文类型。主站发起的功能（复位链路、探活、确认用户数据、非确认
  用户数据）置 1；从站对主站的应答置 1；从站主动上送也置 1，但 **DIR 仍为 0**。

换句话说，从站上送的用户数据帧是 `PRM=1, DIR=0`，不是「从站帧就 PRM=0」。判断
「这一帧是不是给我的应答」不能只看 PRM，还要看 DIR 与功能码。本实现的 `Frame.Primary()`
就是 `DIR != 0 && PRM != 0`，二级帧（ACK/NACK/链路状态）走另一条分支。

主站功能码：`0` 复位链路、`2` 探活（TEST_LINK）、`3` 确认用户数据、`4` 非确认用户数据。
从站功能码：`0` ACK、`1` NACK、`11` 链路状态。复位链路与探活都必须收到二级帧才算成功；
复位链路还隐含「清空传输层重组状态」，所以重连后必须重发，不能跳过。

#### 3.10.14.4 传输层：6 位序号与首尾标记

应用报文可能超过单帧容量（4096 字节的链路帧并不常见，但事件批量上送很容易超过
250 字节），所以要分片：

- 首字节的 bit7 是 `FIR`（第一片），bit6 是 `FIN`（最后一片），低 6 位是传输序号；
- 每个分片序号递增并对 64 取模；
- 接收端按「FIR 开始、FIN 结束」重组，序号不连续就丢弃整条并从头再来。

传输序号与「应用层序号」是两回事。应用层序号在控制字节的低 4 位，用于请求与响应配对；
用同一个计数器既算分片又算事务，会让请求和响应的序号对不上，表现为「明明收到了响应，
驱动说超时」。

#### 3.10.14.5 应用层：功能码、限定词与对象头

应用控制字节：`FIR`（0x80）、`FIN`（0x40）、`CON`（0x20）、`UNS`（0x10）、序号（低 4 位）。
请求没有 IIN，响应在功能码之后先放 `IIN1` 再放 `IIN2`（两个字节，小端组合成一个
`uint16`）。**IIN 的位置是紧跟在功能码之后、对象区之前**，把它当成对象头的一部分会
把后面所有对象解析错位。

功能码本实现使用：`READ=0x01`、`SELECT=0x03`、`OPERATE=0x04`、`DIRECT_OPERATE=0x05`、
`RESPONSE=0x81`、`UNSOLICITED=0x82`、`CONFIRM=0x00`。`SELECT`/`OPERATE` 只用于明确
拒绝（SBO 不在范围内），回 `IIN=功能不支持`，而不是沉默。

对象头三字节：组（1 字节）、变体（1 字节）、限定词（1 字节）。限定词决定「范围怎么
编码」：

| 限定词 | 含义 | 范围字节数 |
|---|---|---|
| `0x00` | 8 位起止 | 2（起、止） |
| `0x01` | 16 位起止 | 4 |
| `0x06` | 全部对象 | 0 |
| `0x07` | 8 位计数 | 1 |
| `0x08` | 16 位计数 | 2 |

前缀码（高 4 位 `0x10` / `0x20` / `0x30`）表示每个对象前带 1 / 2 / 4 字节索引。事件对象
几乎总是带索引前缀——因为事件的索引不连续，不能用「起止范围」表达。反过来，**带时标
的事件不可能用位图编码**（每个点时标不同），遇到无前缀的带时标事件应直接报错，不能猜
一个布局把时间读错。

#### 3.10.14.6 地址语义：`offset` 就是对象索引

点位用 `kind=register` 寻址，`offset` 是对象索引（0..16777215，实际常用 0..65535），
`area` 声明它属于哪个组：

| `area` | 静态组 | 事件组 | 可写 | 平台类型 |
|---|---|---|---|---|
| `binary-input`（`bi`） | 1 | 2 | ✗ | `bool` 或数值（0/1） |
| `double-bit-input`（`dbi`） | 3 | 4 | ✗ | `bool`（1=分、2=合）或数值（0..3） |
| `binary-output`（`bo`） | 10 | 11 | ✓（CROB，组 12） | `bool` 或数值 |
| `counter`（`ctr`） | 20 | 22 | ✗ | 数值 |
| `analog-input`（`ai`） | 30 | 32 | ✗ | 数值 |
| `analog-output`（`ao`） | 40 | — | ✓（组 41） | 数值 |

注意 **`binary-output` 的可写与它的静态组是两件事**：组 10 是「读输出状态」，组 12
（CROB）才是「写输出」。同一个 `offset` 在两组里指同一个物理点，但编码完全不同。
驱动按 `area` 选组，用户不需要记组号。

同一个 `area + offset` 可以挂多个点位（例如同一路模拟量既做 `float32` 展示又做
`float64` 计算），`byKey` 维护一对多映射，一份缓存值喂给所有点位。

#### 3.10.14.7 类 0/1/2/3：一次 READ 可以同时要四类

DNP3 的「类」不是数据类型，而是**更新频率的约定**：

- 类 0：静态值（当前快照），总召唤用；
- 类 1/2/3：事件，由工程侧按重要性分配（例如跳闸信号类 1、模拟量变化类 2、电量
  类 3）。

主站发一条 `READ`，对象区可以写多个「组 60」的头，每个头指定一类：

```
60 01 06     类 0（变体 1）全部对象
60 02 06     类 1
60 03 06     类 2
60 04 06     类 3
```

从站用一条响应回全部——这就是「总召唤」。事件轮询只请求 `60 02/03/04`，总召唤请求
`60 01/02/03/04`（类 0 + 全部事件：因为总召唤本身也要把未确认的事件一起带出来，
否则事件可能永远排在静态值后面）。

节奏：`eventPoll` 默认 1s，`integrityPoll` 默认 5m，且**总召唤必须不小于事件轮询**
（否则总召唤的静态快照会不断覆盖刚收到的事件，表现为「值在跳回旧值」）。`eventPoll`
下限 100ms：再小就是在用轮询模拟推送，会把从站和网络都打满。

#### 3.10.14.8 三个线格式陷阱：双点 2 位/点、变体 2 无位图、带时标事件布局

这是本实现踩过并写进测试的两个坑，值得单独说明。

**双点静态量是 2 位一个点。** 组 3 变体 1 是纯位图：点 0 占 bit1-0，点 1 占 bit3-2，
以此类推，4 个点一字节。按「1 位一个点」解析会让后续对象整体错位，而错位后的字节
往往仍是合法值——每个值都错，但没有任何报错。测试用 `0xE4`（`0,1,2,3` 重复）跨两个
字节钉死了这个行为。

**静态二进制量的变体 2 是「每点一个状态+品质字节」，没有独立的位图字段。** 组 1/3/10
变体 1 是纯位图；变体 2 是每个点一个字节，状态在 bit7（双点量在 bit6-7），低 6 位是
品质。很多人从「变体 1 = 位图」类推，以为变体 2 = 品质块 + 位图块，多读一个字节，
于是后面所有对象都错位。本实现的解析器对变体 2 只读 `count` 个字节，从站编码器也只写
`count` 个字节，两边各自有测试；另有一个「变体 2 后面紧跟另一个对象头」的用例，专门
抓「多读一个字节」。

**计数器/模拟量是每对象 `flags + 值` 交错，不是 flags 块 + 值块。** 组 20/30/40 的变体 1
（32 位无时标）线格式是：

```
[flags(1) value(4)] [flags(1) value(4)] ...
```

不是 `[flags...][values...]`。从站编码器曾经写成后者，静态值全部读成「看起来合法的
垃圾」（计数器读成 `256001`、模拟量读成 `9.2e-41`）。发现它的不是人眼，而是「从站
编码 → 主站解析」的往返自洽测试（`internal/sim/dnp3/server_test.go` 的
`TestStaticRoundTrip`）。这类错误的共同特征是：**错位后的数字仍是合法值**，所以只能
靠往返测试或抓包对照，不能靠「看起来对」。

**带时标的计数器事件（组 22 变体 5/6）是「值 + 品质 + 6 字节绝对时标」，合计 11/9 字节，
不是组 20 无品质变体的 4/2 字节。** 组 22 与组 20 是两组不同的对象：组 20（静态计数器）
变体 5/6 是「32/16 位、**无品质**」，而组 22（计数器事件）变体 5/6 是「32/16 位 +
**品质 + 绝对时标**」。此前实现把组 22 的 5/6 误当成组 20 的布局（4/2 字节、无品质无时标），
从站回一批带时标的计数器事件时，解析按 4/2 字节切对象——第一个对象就把品质字节与时标
当成下一个计数器的值，整批对象全部读歪，且因为错位后的数字仍是合法值而不报错。修复后
用两个 22v5 对象拼一个缓冲区，既验证单对象的值/品质/时标，又验证不会错位（组 32 带时标
整型同样是 11/9 字节绝对时标，可作为交叉印证）。这类错误的判别方法与上面两条一致：
**往返自洽 + 抓包对照**，不能靠「看起来对」。

#### 3.10.14.9 品质映射：状态位不是品质位

品质字节（`flags`）：

| 位 | 名称 | 含义 | 平台质量 |
|---|---|---|---|
| 0 | ONLINE | 0 表示该点从未被扫描或已离线 | `bad` |
| 1 | RESTART | 从站重启后尚未刷新 | `uncertain` |
| 2 | COMM_LOST | 通信丢失 | `bad` |
| 3 | REMOTE_FORCED | 被主站强置 | `uncertain` |
| 4 | LOCAL_FORCED | 被本地强置 | `uncertain` |
| 5 | CHATTER / ROLLOVER / OVER_RANGE | 抖动 / 计数器回绕 / 越限 | `uncertain` |
| 6 | REF_ERR | 参考误差 | `uncertain` |
| 7 | STATE（仅二进制组） | 过程状态 | **不是品质** |

双点量的状态占 bit6-7，因此 bit6（REF_ERR 位）在双点组里也是状态的一部分。

**必须先按组屏蔽状态位，再判品质。** 否则「合」位（状态 2 → bit6 置位）会被当成
`REF_ERR`，于是所有处于合位的开关都变成 `uncertain`——现场表现为「全站开关都不可信」，
但模拟量正常。这个 bug 在本实现里真实出现过一次，测试 `TestQualityMapping` 与
`TestOpenAndReadStatic` 从两个方向把它钉住：前者改品质位断言质量码变化，后者读默认
点表断言「合」位仍是 `good`。

双点量在 `dataType=bool` 时按 1=分→`false`、2=合→`true` 映射；0/3 是中间态/不确定区，
统一映射为 `false` + `uncertain`（取两者中较差的质量）。不这么做的话，「阀门未知」
会显示成「已关闭」——这是工业网关里最危险的一类假数据。

**无标志变体（without flag）假定值有效，返回 `good`。** 标准里有一批变体
**根本没有品质字节**：计数器组 20 变体 5/6、模拟量组 30 变体 3/4 等（codec 的
`numericSpecOf` 里 `flags: false` 的那些）。它们不是「品质字节恰好为 0」，而是
**协议层面就没有这个字段**——IEEE 1815 对它们的定义就是「省略品质信息，接收方
按值有效处理」。所以 `qualityOf` 在 `HasFlags=false` 时没有 ONLINE 位可判，返回
`good` 是与协议一致的，不是漏判。

这一点必须与 MTConnect 的「条件缺级别」区分开，两者方向相反：

| | 语义存在吗？ | 读得出吗？ | 该判什么 | 理由 |
|---|---|---|---|---|
| MTConnect 条件缺级别 | 是（是报警） | 否（级别读不出） | `bad` | 告警被静默吞掉才最危险 |
| DNP3 无标志变体 | 否（协议无此字段） | — | `good` | 标准规定假定值有效 |

若把 DNP3 这里也改成判 `bad`，所有使用 without-flag 变体的真实设备（现场很常见）
会一次性上报「全部离线」，是**另一种方向**的假故障。测试 `TestQualityWithoutFlagIsGood`
双向钉住：既确认无标志对象判 `good`，又用「同组 ONLINE=0 判 `bad`」的对照证明
`qualityOf` 确实在读标志位，不是「一律 good」。

#### 3.10.14.10 读：本地表，不产生网络交互

`Read` 只查本地最新值表，与 IEC 104 / MQTT 南向同一模式。理由不是「实现简单」，而是
**DNP3 没有「读第 100 号点」这个协议动作**：能发的只有类轮询，而类轮询返回整类数据，
代价与点位数量无关。如果 `Read` 每次都发一条类轮询，10 个点位会被放大成 10 次整类
扫描，而且会打乱后台的事件节奏。

驱动在 `Open` 时同步做一次总召唤（类 0/1/2/3），失败就 `Open` 失败。这与 IEC 104 的
「STARTDT 成功后不等召唤」不同：DNP3 的链路复位之后本来就必须读一次类 0 才知道从站
有哪些点，早一点失败比让上层拿着一整张「无数据」表跑起来更有价值。

本地表未命中时返回 `bad` + 「no data received for X[n] yet (not in the outstation's class 0
set?)」，而不是返回零值。零值会被上游当成「设备读数 0」，这个提示则直接把排查方向
指向「点表配置的索引/区与从站实际点表不一致」。

**链路断开时这张表里的值全部是「断链前的快照」。** 关键细节在
`runtime/collector.go`：它会对 `Read` 返回的每一个样本调用 `apply`，即使同一轮也拿到了
错误。也就是说「把错误一并返回」并不足以保护上游——断线期间上层仍会看到一串标着
`good` 的旧值，把它们当当前值参与联锁。因此 `Read` 在链路级失败时逐点降级：质量改
`stale`、值保留（诊断需要知道断线前是多少）、原因写进 `Err`；已经带自己错误的样本
（地址错、从未收到值）保持原样，因为那更具体。这一条与「零值不是好值」是同一条
原则的两面：**看不出来的失效比看得出来的失效危险得多**。

#### 3.10.14.11 写：CROB 与模拟量输出都要检查回显

二进制输出用 CROB（组 12）：控制码 `1`=脉冲开、`2`=脉冲关、`3`=锁存开、`4`=锁存关，
本实现用锁存开/关（3/4），因为「写一个布尔值」的语义就是保持状态，而不是发一个脉冲。
CROB 还带 count、on-time、off-time，本实现固定 count=1、时间为 0。

模拟量输出用组 41（32 位有符号 / 16 位有符号 / 单精度 / 双精度四个变体）。变体由
`dataType` 选：`int16`→变体 2、`int32` 及无符号整型→变体 1、`float32`→变体 3、
`float64`→变体 4。**组 41 没有无符号变体**，所以 `uint32`/`uint64` 点位只能按 int32
下发。

**发报文前先确认值能装进选定的线格式**（`analogFitsWire`）。整型变体要求值既是整数
又落在 `int32`/`int16` 区间内，浮点变体要求有限（`NaN`/`±Inf` 拒绝，超出 `float32`
幅度的值也拒绝）。此前整型输出把值直接交给 `uint32(int32(value))`：`int32` 点位写
`1e30`、`int16` 点位写 `40000`、整数点位写 `12.5` 都会被静默截断或回绕后发出去，而
驱动报成功——现场表现为「下发了一个完全错误的设定点却显示成功」。现在这类值在
发出之前就得到 `unsupported`，且一个字节都不发（与 CoAP 写路径共用同一条原则）。对于
映射到变体 1 的 `uint32`/`uint64` 点位，超过 `int32` 上界的值只能拒绝：组 41 没有无
符号变体是协议本身的限制，需要靠点表评审改类型，而不是让驱动悄悄发一个负数。

两条写路径都等从站的应用响应，并检查两件事：

1. **IIN**：`功能不支持` / `对象未知` / `参数错误` / `已在执行` 任一置位即失败；
2. **回显状态**：CROB 的响应必须带回显对象，`status != 0` 即失败，错误信息里带状态码
   （`timeout` / `no-select` / `format-error` / `not-supported` / `already-active` /
   `hardware-error` / `local-control` / `too-many-ops` / `not-authorized`）。

只写 socket 不检查回显，会把「从站拒绝」当成「写入成功」。这类错误在有硬件联调时
通常能被现场发现（因为设备没动），但在纯软件测试里会一直绿着——所以从站对不存在的
索引故意返回 `not-supported`，驱动测试 `TestWriteUnknownOutputRejected` 断言它必须报错，
且错误分类是 `protocol` 而不是 `unknown`。

写成功后驱动把「刚写下去的值」填进本地表并标 `good`：从站已经回显成功，这个值就是
主站的权威视图；下一次类 0 刷新会用设备真实值覆盖它。**不标 `uncertain`** 与 IEC 104
的做法不同（104 标 uncertain 是因为从站的突发上送可能比确认更早到达，本地推测值可能
覆盖真实值）；DNP3 的直接控制回显本身就是「已应用」的证据，标 uncertain 反而会让
read-back 确认读到 uncertain → 上层显示「写入未确认」，与事实相反。

#### 3.10.14.12 错误分类

| 现象 | `ErrorKind` | 可重试 |
|---|---|---|
| 拨号失败、链路断开、写帧失败 | `disconnected` | 是 |
| 复位链路 / 探活 / 应用事务超时 | `timeout` | 是 |
| 链路层二级帧回 `NACK` | `protocol` | 是（请求被拒，换参数后） |
| 链路层二级帧回 `NOT_SUPPORTED` | `unsupported` | 否 |
| IIN 置位（功能不支持、对象未知、参数错误、已在执行） | `protocol` | 是（换参数后） |
| CROB 回显非零状态 | `protocol` | 是（看状态码） |
| 模拟量输出值无法装进选定线格式（越界、带小数、`NaN`/`±Inf`） | `unsupported`（**发报文前**拒绝） | 否 |
| 配置错误（地址区未知、类型不匹配、轮询参数非法、主从地址相同） | `unsupported` / `address` | 否 |
| 未知 `area`、`kind != register` | `address` | 否 |

**错误类别不得在写路径里被「洗白」。** 两条写路径（CROB 与组 41）都通过
`c.request` 与从站做一次应用事务，`request` 已经按真实原因分类好了：链路断开是
`disconnected`、事务超时是 `timeout`。此前两处都写成 `fail(driver.ErrTimeout, ...)`，
等于把所有写失败统一报成「设备慢、可重试」——一条已经断掉的链路会被调度器原地重试，
永远不触发重建连接，而 `runtime/collector.go` 恰恰是按 `ErrDisconnected` 决定丢弃连接的。
现在 `writeFailed` 保留原错误的 `KindOf`，只做质量码换算（超时→`timeout`、
断连→`comm_error`）。回归用例 `TestWriteFailureKeepsErrorKind` 杀掉从站后断言写失败
得到 `disconnected`；把 `writeFailed` 改回 `fail(driver.ErrTimeout, ...)` 必须变红。

**链路层 ACK 不等于「对方接受了请求」。** 二级帧有三种：`ACK`（0）、`NACK`（1）、
`LINK_STATUS`（11），还有 `NOT_SUPPORTED`（15）。`waitAck` 此前对**任何**二级帧都返回
成功，于是复位被从站拒绝的设备会被当成链路已就绪，真实失败被推迟到应用层总召唤超时，
`NACK` 这个真实原因在日志里一次都不出现。现在按功能码分流：`ACK`/`LINK_STATUS` 才算通过，
`NACK` 报 `protocol`、`NOT_SUPPORTED` 报 `unsupported`。回归用例
`TestLinkResetNackIsFailure` 让脚本化从站对复位回 `NACK`，断言 `Open` 失败且错误信息
指出「从站拒绝」。把 `waitAck` 的 NACK 分支改回 `return nil` 必须变红。

**链路断开后本地表里的值是快照，不是当前值。** `Read` 返回本地缓存，而
`runtime/collector.go` 会对返回的**每个**样本调用 `apply`——即便同一轮也拿到了错误。
因此「把 `ErrDisconnected` 一并上抛」不足以保护上游：断线期间上层会看到一串标着
`good` 的旧值，正是最危险的一类假数据。现在链路级失败时逐点降级为 `stale`、
值保留、原因写进 `Err`（见 `degradeSamples`）。已经带错误的样本保持自己的错误更具体。
回归用例 `TestReadAfterLinkDownIsStaleNotGood` 断言断线后没有任何样本以 `good` 或空质量
返回，且 `bi0` 是 `stale`。

重连不是「每次读失败都拨号」，而是后台轮询循环发现链路错误后进入 `recover`：退避从
200ms 开始翻倍，上限 5 秒。接不接受重连由配置决定不了——现场交换机重启通常几秒到
几十秒，退避再长会让恢复时间不可控；但零间隔重连会把一个拒绝连接的从站打成半开连接
风暴。

#### 3.10.14.13 能力边界（刻意的）

| 不做 | 理由 |
|---|---|
| 串口 / 拨号 / 电台链路 | 需要 RTS 时序、字节间静默、本地从站地址协商，是另一个传输层 |
| Secure Authentication | 证书/密钥管理 + 挑战-应答状态机 + 防重放窗口，独立子系统 |
| 文件传输（组 70） | 用于波形/录波，单次可能几 MB，与周期采集的延迟目标冲突 |
| 时间同步写路径（组 50 变体 1 写） | 时区/夏令时/从站时间质量/审计都要处理 |
| SBO 两段式控制 | 需要维护「已选择」会话状态并处理超时撤销；直接控制已覆盖绝大多数现场 |
| 事件缓冲区重配（组 50 写） | 改错会让从站丢事件，应由工程侧一次性配好 |
| 类 0 单组召唤 | 只在极窄带宽链路上有意义 |
| 主动触发非请求响应 | 测试动作，不是生产动作 |

#### 3.10.14.14 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/dnp3`（`CRC`、`EncodeFrame`/`DecodeFrame`、流式 `Decoder`、`Reassembler`、`EncodeTransport`、`EncodeRequest`/`EncodeResponse`、`DecodeApp`、`ParseObjects`、`ParseRequestHeaders`、`ClassObjects`、`CROBObjects`、`AnalogOutputObjects`、`EncodeTime48`/`decodeTime48`） |
| 主站驱动 | `internal/driver/dnp3`（`Meta`/`Open`/`Probe`/`Read`/`Write`；内部 `connect`/`linkReset`/`linkTest`/`waitAck`/`pollLoop`/`poll`/`recover`/`applyObjects`/`convertObject`/`qualityOf`/`writeFailed`/`degradeSamples`；诊断 `Observed`/`Dropped`/`IIN`） |
| 内置从站 | `internal/sim/dnp3`（`NewServer`/`Addr`/`SetPoint`/`SetValue`/`SetFlags`/`Value`/`Points`/`SetAutoChange`/`Stats`/`Inflight`/`Pending`/`BadFrames`/`Close`） |
| CLI | `uic dnp3read`（`-addr`/`-master`/`-unit`/`-wait`/`-timeout`/`-index`/`-json`/`-raw`/`-probe`/`-binary INDEX=on\|off`/`-analog INDEX=VALUE`） |
| 示例 | `examples/dnp3.json`（13 点位，REST 8098，从站 20000，主站链路地址 1024） |

内置从站的默认点表与示例配置一一对应：

| 区 | 索引 | 初值 | 类 | 行为 |
|---|---|---|---|---|
| binary-input | 0,1 | on / off | 1 | 索引 0 每秒翻转 |
| binary-input | 2,3 | on / off | — | 静态 |
| double-bit-input | 0,1 | 1（分）/ 2（合） | 1（仅 0） | 静态 |
| binary-output | 0,1 | off / off | 1 | 接受 CROB 锁存开/关 |
| counter | 0,1 | 1000 / 42 | 3（仅 0） | 索引 0 每秒自增 |
| analog-input | 0..3 | 42.5 / 7.25 / -3.5 / 1234 | 2 | 静态 |
| analog-output | 0 | 0 | — | 接受组 41 直接控制 |

自检链路（无硬件，TCP 回环）：

```bash
./uic sim -protocol dnp3 -listen 127.0.0.1:20000 &
./uic dnp3read -addr 127.0.0.1:20000 -probe
# 链路正常: 127.0.0.1:20000 主站=1024 从站=1（复位链路与 TEST_LINK 均通过）

./uic dnp3read -addr 127.0.0.1:20000
# 从站 127.0.0.1:20000  主站=1024 从站=1
# 设备上送 15 个对象
# 类别                 组/变体  索引  质量   值
# analog-input         30/5     0     good   42.5
# ...
# double-bit-input     3/2      1     good   2（合）

./uic dnp3read -addr 127.0.0.1:20000 -binary 0=on -analog 0=12.5
# OK   binary[0]=on
# OK   analog[0]=12.5

./uic dnp3read -addr 127.0.0.1:20000 -index 0 -raw
# 类别              组/变体  索引  质量   值      原始 flags
# binary-input      1/2      0     good   on      0x81
```

`dnp3read` 不依赖点表：它只放一个探针点位（`binary-input[0]`）满足 `Open` 的非空约束，
真正的对象清单来自驱动旁路表（按「区 + 索引」记录组、变体、原始 flags、时标）。初次
接陌生设备时先跑它，看清「设备到底送哪些对象、用什么组/变体」，再写点表——这比拿一份
猜出来的点表去试要快得多，也避免把「索引错位」误判成「设备没数据」。

示例配置带上网关跑真实采集：

```bash
./uic -config examples/dnp3.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8098/api/v1/points
curl -s -X POST -H 'Authorization: Bearer dev-token-0123456789' \
  -H 'Content-Type: application/json' \
  -d '{"value":{"type":"bool","bool":true}}' \
  localhost:8098/api/v1/points/rtu-dnp3/trip/write
```

写接口默认返回 `quality: uncertain`：写入已被从站回显确认，但运行时的通用写入路径
在 `confirm=false` 时统一把「乐观更新」标为 uncertain，等下一轮采集确认；加
`"confirm":true` 时会立刻回读一次，读到一致值就转 `good`。这与 IEC 104 的写入语义
保持一致，不让用户因为协议不同而记两套规则。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/dnp3` | CRC 已知答案与分块、链路帧编解码与流式收帧（粘包/半包/坏 CRC 恢复）、传输分片与重组、应用请求/响应（有无 IIN）、8/16 位起止与全部对象、计数限定词、索引前缀、静态位图（1 位/点与双点 2 位/点）、变体 2 无位图、计数器/模拟量交错、带时标事件（22v5/v6 双对象不错位）、请求头解析、CROB 与组 41 构造、未知组报错 |
| `driver/dnp3` | 真实 TCP 回环：复位 + 总召唤 + 静态值、双点中间态 uncertain、事件轮询与主站应用层确认（inflight 清零）、质量映射三态、CROB 写/读回、模拟量写、模拟量写越界/带小数/`NaN` 在发报文前拒绝（设备零写入）、合法边界值与浮点写仍成功、只读区拒绝、未知输出索引被从站拒绝、`Probe`、`Close` 后读失败、配置校验（地址范围/主从相同/轮询参数/传输/区/类型）、无 ACK 超时有限、并发 Read 与后台轮询（`-race`）；**错误分类不被洗白（写失败保 `disconnected`）**、**链路复位被 `NACK` 时 `Open` 失败且原因可见**、**链路断开后 `Read` 的旧值降级为 `stale` 而非 `good`** |
| `sim/dnp3` | 静态编码 → 主站解析的往返自洽（15 个对象逐一比对），专门抓「编码少一个字节 / 解析多读一个字节」的错位 |
| `cmd/uic` | `-probe`/`-index`/`-raw`/`-json`、`-binary`/`-analog` 解析与错误写法、`-master`/`-unit` 范围与相同地址拒绝、行显示 |

### 3.10.15 CoAP（UDP 资源模型：GET/PUT/POST + 分块接收 + 资源发现，端口 5683）

CoAP 与前面所有协议都不同：它没有「寄存器地址」，也没有「对象索引」，它的数据模型
是 **URI 资源**。一个点位就是一个路径（`/sensors/temp`），或者一个路径加一个字段
（`/sensors/env` + `temperature`）。这带来两个直接后果：第一，点表评审时要问的不是
「偏移量对不对」，而是「这个 URI 在设备上真的存在吗」；第二，`Read` 无法批量，因为
每个点位都是一次独立的 HTTP 式请求。

本节的写法与 DNP3 那节一致：先讲边界，再讲线格式里真正会踩的坑，最后讲读、写、
错误分类与自检。CoAP 看起来简单（四个字节的头 + 文本载荷），但它的**选项编码、
分块续读与分离响应**三处都能让实现「连得上、读得到第一条、然后偶发超时」。

#### 3.10.15.1 范围：只做明文 UDP 主站

| 做 | 不做 | 原因 |
|---|---|---|
| UDP 明文 CoAP | DTLS（加密传输） | DTLS 需要证书/预共享密钥、握手状态机与重放窗口，是一个独立子系统；本平台的安全边界放在北向与网络分段，而不是在每个南向驱动里各做一套 |
| GET / PUT / POST | DELETE、FETCH、iPATCH | 网关的语义是「采集 + 下发」，删除资源与部分更新属于配置工具的动作，不该由采集进程发起 |
| Block2 分块**接收** | Block1 分块**发送** | 读取大 JSON 文档是常态；写入超过单包大小的资源需要服务端配合的分块确认，现场极少见，做半套比不做更危险 |
| `/.well-known/core` 资源发现 | Observe 订阅 | 订阅要长期维护观察关系、通知序号与重连后的重新注册；用周期 GET 已能满足采集，订阅留给后续独立实现 |
| Content-Format：text/plain、application/json、application/octet-stream | CBOR、SenML 等其它格式 | 前三种覆盖了现场绝大多数设备；其它格式先明确报「不支持」，不猜结构 |
| 单播 | 组播发现、CoAP over TCP | 组播发现需要网络侧配合与去重策略；本平台面向点对点采集 |

能力声明必须与实现一致：`Subscribe` 与 `Browse` 都声明为不支持。把 Observe 标成
「支持」而实际只做一次性 GET，会让上层以为推送可用，然后在「为什么值不更新」上
浪费一整天。

#### 3.10.15.2 报文头只有四个字节，但有两套标识

```
 0                   1                   2                   3
+-------+-------+-------+-------+-------+-------+-------+-------+
| Ver   | Type  |  TKL  |      Code     |            MID            |
+-------+-------+-------+-------+-------+-------+-------+-------+
|                       Token (TKL 字节，0..8)                      |
+---------------------------------------------------------------+
|                         Options ...                           |
+---------------------------------------------------------------+
| 0xFF (仅当有载荷) |                  Payload                   |
+---------------------------------------------------------------+
```

- `Ver` 固定 1；`TKL` 是 Token 长度，0..8，超过 8 直接判为畸形；
- `Type`：CON=0（需要 ACK）、NON=1（不需要 ACK）、ACK=2（确认一条 CON）、RST=3（拒绝）；
- `Code`：请求 `0.01` GET、`0.02` POST、`0.03` PUT、`0.04` DELETE；响应 `2.01` 创建、
  `2.04` 变更、`2.05` 内容；错误 `4.00` 请求错误、`4.02` 坏选项、`4.03` 禁止、
  `4.04` 不存在、`4.05` 方法不允许、`4.06` 不可接受、`4.12` 前置条件失败、
  `4.13` 实体过大、`4.15` 不支持的格式；服务端侧 `5.00`、`5.03`；
- `MID` 是**报文**标识（16 位，用于 ACK/RST 与去重），`Token` 是**事务**标识（用于把
  响应配给请求）。两者不是一回事：ACK 必须回显请求的 MID，但响应可以带新的 MID
  （分离响应）仍然用同一个 Token。

把 MID 当事务 ID，会在「空 ACK + 分离响应」的场景下把真正的响应当成迟到包丢掉；
把 Token 当报文 ID，则无法正确回 ACK。本实现的规则是：**ACK/RST 看 MID，业务响应
看 Token**。

#### 3.10.15.3 选项编码：delta 与 length 的扩展字节顺序

选项区是 CoAP 最容易写错的地方。每个选项头一个字节：

```
 高 4 位：delta（选项号相对前一个选项号的差值）
 低 4 位：length（选项值长度）
```

`0..12` 直接表示；`13` 表示「后面再跟 1 字节，实际值 = 13 + 该字节」；`14` 表示
「后面再跟 2 字节（大端），实际值 = 269 + 该值」；`15` 是保留值，出现在 delta 位置
表示「载荷标记」（`0xFF`），出现在 length 位置是畸形。

两个坑：

1. **扩展字节的顺序是先 delta 后 length。** 当两者都需要扩展时，先写 delta 的扩展
   字节，再写 length 的扩展字节，最后才是选项值。反过来写会让解析器从第二个选项
   开始读垃圾——而垃圾往往还能解析成一个「看起来合法」的选项号，所以不会立刻报错。
2. **delta 是相对值，不是绝对选项号。** 重复的 Uri-Path 选项 delta 为 0，这是合法
   且常见的（路径有多段）。把 delta 当绝对值会让 `/sensors/temp` 的第二段解析成
   某个完全不同的选项。

本仓库的编解码测试对每个分支都钉了金字节，包括「delta 需要 1 字节扩展」「delta 需要
2 字节扩展」「length 需要 2 字节扩展」「两者同时扩展」与「重复选项 delta=0」。

#### 3.10.15.4 三种「收到」：ACK、RST 与分离响应

一次 CON 请求的正常路径是「请求 → ACK 搭载响应」。但 CoAP 允许服务端先回一条
**空 ACK**（证明请求已被接受、别重发），过一会儿再用一条新的 CON 把真正的响应发回来。
这就是分离响应（separate response）。

| 收到的报文 | 含义 | 驱动动作 |
|---|---|---|
| ACK + MID 匹配 + 空 Code | 请求已接受，响应稍后到 | 停止重传，延长等待窗口，继续读 |
| ACK + MID 匹配 + 2.xx/4.xx/5.xx | 搭载响应 | 校验 Token 后返回 |
| CON + Token 匹配 + 响应码 | 分离响应 | 回一条空 ACK，然后返回 |
| RST + MID 匹配 | 对端拒绝 | 立即失败，归类 `protocol`，不重试同一请求 |
| CON + Token 不匹配 | 与本次事务无关（迟到响应或通知） | 回 RST 让对方停止重传，**继续等自己的响应** |
| 解析失败 | 垃圾报文 | 计数后跳过，不影响当前事务 |

最后两条是稳定性关键。UDP 上「上一次超时后迟到的响应」很常见；如果把它当成当前
响应，或者因为它解析失败就让整次读失败，采集会在弱网下随机抖动。驱动为此维护一个
`BadDatagrams` 计数，测试可以直接断言「坏包被跳过而不是让请求失败」。

#### 3.10.15.5 地址语义：`path` 是资源，`selector` 是字段

点位用 `kind=symbol` 寻址：

| 字段 | 含义 |
|---|---|
| `address.path` | 资源路径，例如 `/sensors/env`；省略前导斜杠会被补上 |
| `address.selector` | 响应为 JSON 时取哪个字段，支持 `a.b` 与 `a[0]`；留空表示整个文档 |
| `address.length` | `bytes`/`string` 的接收上限（字节），0 表示不限制 |

`dataType` 覆盖 `bool`、`int16`/`uint16`/`int32`/`uint32`/`int64`/`uint64`、
`float32`/`float64`、`string`、`bytes`——与 `textval` 支持的类型一一对应。声明与实现
必须同步：枚举漏掉 `int64`/`uint64` 时驱动照样能跑，但配置界面里选不到，只能手改
JSON。`bcd16`/`bcd32` 是编码方式而不是文本类型，CoAP 不接受。

路径里不允许出现 `?query`：查询串会让「同一路径不同参数」变成两个资源，而平台的点位
模型没有参数维度。需要查询参数的设备应在网关外侧做适配，而不是把参数塞进路径。

同一个 `path + selector` 可以挂多个点位（例如同一份 JSON 文档里的温度与状态），驱动
按点位逐次 GET——它**不做响应缓存**：同一轮采集里两个点位读同一个路径，就会发两次
请求。这是刻意的取舍：缓存会引入「两份数据谁更新」的问题，而 CoAP 资源通常是几百
字节，多一次往返的代价远小于一次脏读。

#### 3.10.15.6 Content-Format：按声明解码，不猜

`Content-Format`（选项号 12）是一个整数：

| 值 | 媒体类型 | 本实现的处理 |
|---|---|---|
| 0 | `text/plain` | 按点位类型走文本规则（与 MQTT 共用同一套「文本 → 平台值」解析） |
| 40 | `application/link-format` | 只用于资源发现 |
| 42 | `application/octet-stream` | `bytes` 原样收；`string` 按字面字节转字符串；数值类型明确报错 |
| 50 | `application/json` | 按 `selector` 取字段，再按点位类型解码 |
| 60 | `application/cbor` | 明确报「不支持」，不尝试猜结构 |

缺省（响应里没有 Content-Format）按 `text/plain` 处理，这是现场最常见的写法。

「按声明解码」不是一句口号。一个 `Content-Format=50` 的响应里出现 `"42"` 是 JSON
**字符串**，不是数字；按文本解析会把引号一起变成 `string` 点位的值。反过来，一个
没有 Content-Format 的 `{"a":1}` 会按文本处理，`string` 点位拿到的是整段原文——这
也是正确行为，因为设备没有声明它是 JSON。

JSON 的标量规则集中在 `internal/codec/textval`，MQTT 与 CoAP 共用：布尔接受
`true/false/on/off/yes/no/1/0`（大小写不敏感），数值接受十进制与科学计数法，字符串接受
JSON 字符串或裸文本。把这条规则复制两份，迟早会出现「同样的 `"on"` 在 MQTT 里是
`true`、在 CoAP 里报错」的分裂。

写方向同样共用这一层的 `FormatNumber`：范围、整数性、有限性在发出报文之前就判清。
`int64`/`uint64` 的边界与 `NaN`/`±Inf` 是两道最容易漏的闸门——CoAP 的 JSON 写入
曾经自己做 `int64(n)`，`1e30` 会回绕成 `-9223372036854775808`，设备收到一个彻底
错误的数而驱动报成功。现在文本与 JSON 两条写路径只有一份格式化实现。

#### 3.10.15.7 Block2：由服务端决定分块，客户端只续读

大于单包容量的资源用 Block2 选项分块。选项值是一个 32 位整数：

```
NUM << 4 | M << 3 | SZX
```

`NUM` 是块号，`M=1` 表示后面还有块，`SZX` 决定块大小 `1 << (SZX + 4)`（16..1024）。
注意 `SZX` 不是块大小本身，而是它的对数下标；把 `SZX=2` 当成 2 字节会让续读永远
对不上偏移。

本实现的策略：

1. **首个请求默认不带 Block2**，让服务端决定是否分块、用什么块大小。对「不支持
   Block2」的极简服务端最友好。
2. 响应里出现 Block2 且 `M=1` 时，按服务端给出的块号与大小继续取；每块的偏移必须
   等于已收字节数，否则报「块乱序」而不是把数据拼错。
3. 设备标签 `blockSize` 可以指定首块就请求的大小（16..1024 的 2 的幂）。用于「服务端
   默认块太大、设备 MTU 撑不住」的场景。
4. 若服务端对带 Block2 的请求回 `4.02`（坏选项），驱动**退回不带 Block2 再取一次**，
   而不是把 `4.02` 当成资源不存在。这条降级路径有专门的测试与内置服务端的
   `DisableBlock2` 模式。
5. 重组上限 8 MiB 与块数上限同时存在：即使每块只有 16 字节，也不会无限循环。

拼装完成后，驱动把 Block2 选项从「首条响应」里剥掉再交给解码器——保留它会让上层
以为这是一条未完成的分块响应。

#### 3.10.15.8 重传：指数退避，且必须尊重设备级超时

UDP 没有连接，丢包只能靠重传。设备标签：

| 标签 | 默认 | 范围 | 说明 |
|---|---|---|---|
| `ackTimeout` | `2s` | 100ms..30s，且必须 ≤ `device.timeout` | 首次等待 ACK 的时长 |
| `maxRetransmit` | `4` | 0..8 | 重传次数；0 表示只发一次 |
| `blockSize` | `0` | 0 或 16/32/…/1024 | 首选块大小，0 表示跟随服务端 |

第 `n` 次重传的等待窗口是 `ackTimeout * 2^n`，并同时受 `ctx` 截止时间约束：

- **不受 ctx 约束的重传会把一轮采集拖成 N 倍超时。** 设备级 `timeout` 是整轮采集的
  预算，越过它只会让剩余点位全部变成 `context done`。
- **收到空 ACK 后不再重传。** 请求已被接受，重发只会让服务端执行两次。此时只延长
  等待窗口等分离响应；窗口用 `ackTimeout` 而不是翻倍值，避免一次分离响应把整轮采集
  拖死。
- **ctx 已取消时一个字节都不发。** 这不是优化，而是语义：上层明确不要这次采集了，
  驱动不应该再产生网络流量。测试直接断言「取消后写次数为 0」。

#### 3.10.15.9 写：PUT 是替换整个资源，JSON 只发子树

写方法由设备标签 `writeMethod` 决定：`put`（默认）或 `post`。绝大多数执行器用 PUT；
POST 用于「提交/触发」型资源，它不幂等，重发会产生两次动作——这也是驱动在重传上
必须谨慎的原因。

载荷格式由 `contentFormat` 决定：

| 格式 | 请求 Content-Format | 语义 |
|---|---|---|
| `text` | 0 | 值的文本表示，例如 `42`、`true`、`manual` |
| `json` | 50 | JSON 标量；带 `selector` 时包成 `{"a":{"b":值}}` |
| `octets` | 42 | `bytes`/`string` 点位的字面字节 |

JSON 写入的语义必须说清楚：**PUT 是替换整个资源，不是合并。** 带 `selector` 时驱动
只发送 `{selector: value}` 这一棵子树，因此点位的 `selector` 应当与设备资源的字段
粒度一致；资源是多字段文档时，应把 `selector` 留空并让点位对应整个文档。

驱动刻意**不做「先读回来再合并」**：那会在一次写里偷偷插入一次读，失败模式
（读成功、写失败、读到旧值）远比现在复杂，而且会掩盖「设备要求整文档替换」这个
事实。

**数组下标写入显式拒绝。** `selector` 里出现 `[` 或 `]` 时直接返回
`unsupported`：PUT 会替换整个数组，而我们只知道一个下标，发送 `{"values":[1]}` 会
把其余元素抹掉。与其替用户猜「其余元素怎么办」，不如让他配置整个数组资源。

**值不可表示时在发报文前失败。** 文本与 JSON 两条写路径共用 `textval.FormatNumber`
做范围校验：`int16` 点位写 `40000`、`int64` 点位写 `1e30`、整数点位写 `1.5`、
浮点点位写 `NaN`，都得到 `unsupported` 且**一个字节都不发**。直接 `int64(n)` 会让
`1e30` 回绕成 `-9223372036854775808`，`uint64` 的上半段还会变成负数——设备收到的是
一个完全错误的数，而驱动报成功，这类错误在现场极难定位。

写成功后返回 `good`，而不是 `uncertain`：响应码 `2.xx` 就是「服务端已接受」的证据。
这与 DNP3 的直接控制回显同理；IEC 104 标 `uncertain` 是因为突发上送可能比确认更早
到达，CoAP 没有这条通路。网关的通用写入路径仍会在 `confirm=false` 时统一标
`uncertain`，那是运行时层的行为，与驱动无关。

#### 3.10.15.10 读：逐点串行，链路失败后短路

`Read` 对每个点位发一次 GET，串行执行。CoAP 没有「批量读」的标准手段：

- 每个资源一个 URI，没有「读一段地址」的请求；
- 即使多个点位指向同一个路径，驱动也不做响应缓存（见 3.10.15.5）。

串行而不是并发，是因为一个 `conn` 只有一条 UDP 套接字与一套 MID/Token 计数器，
并发读需要额外的配对表与锁；而现场单设备的点位通常只有几十个，一次往返 10ms
量级，串行的总时长仍在采集周期内。并发读的测试只验证「多个 goroutine 调用 `Read`
不会数据竞争」，内部仍然由一把锁排队。

链路级失败（超时、断开）后，**剩余点位直接标同一个错误并结束**，不再逐个等超时，
并以该错误返回。这一条在多点位设备上非常关键：否则一个掉线的设备会让一轮采集变成
「点位数 × 超时」。地址级失败（`4.04`）与类型不匹配只影响当前点位，后面的点位继续
读；按 §3.3.1，只要还有一个点位成功，整轮就是 `err == nil`，全失败才上抛第一个
错误——否则点表里一个拼错的路径会让整台设备每周期丢弃套接字重连。

#### 3.10.15.11 发现与探测：两件不同的事

| 动作 | 请求 | 判据 | 用途 |
|---|---|---|---|
| `Probe` | `GET /.well-known/core` + Accept: link-format | 2.xx / 3.xx / 4.xx 都算存活，只有 5.xx 失败 | 判断「链路是否活着」 |
| `Discover` | 同上，但解析响应 | 必须是合法 link-format | 列出设备发布了哪些资源 |

`Probe` 把 4.xx 也算存活，是因为「这个端点不支持资源发现」恰恰证明它是一个活着的
CoAP 端点。只认 2.xx 会让大量不实现发现的老设备永远显示「离线」。5.xx 是服务端自己
出错，重试才有意义，所以算失败。

`Discover` 返回 `[{path, params}]`，其中 `params` 是 link-format 的属性（`rt` 资源
类型、`ct` Content-Format、`if` 接口等）。初次接一台陌生设备时先跑
`coapread -discover`，看清资源清单再写点表——这比拿一份猜出来的 URI 表去试要快
得多，也避免把「路径写错」误判成「设备没数据」。

#### 3.10.15.12 错误分类

| 现象 | `ErrorKind` | 可重试 | 是否计入熔断 |
|---|---|---|---|
| 拨号失败、UDP 读写失败、链路被丢弃 | `disconnected` | 是 | 是 |
| 超时（含重传耗尽）、ctx 取消 | `timeout` | 是 | 是 |
| 对端 RST | `protocol` | 否（同请求重发无意义） | 否 |
| `4.04` 不存在 | `address` | 否 | **否** |
| `4.03` 禁止、`4.01` 未认证 | `permission` | 否 | 否 |
| `4.02` 坏选项、`4.05` 方法不允许、`4.06` 不可接受、`4.15` 格式不支持 | `unsupported` | 否 | 否 |
| `4.00` 请求错误、`4.12` 前置条件失败、`4.13` 实体过大 | `protocol` | 否 | 否 |
| `5.00`、`5.03` | `protocol` | **是** | 是 |
| 写入值无法按点位类型表示（越界、带小数、`NaN`/`±Inf`、`bytes` 当 JSON 写） | `unsupported`（点位级，**发报文前**拒绝） | 否 | 否 |
| 配置错误（传输不是 UDP、标签非法、地址非法、类型不支持） | `unsupported` / `address` | 否 | — |
| 解码失败（JSON 非法、字段缺失、类型不匹配、长度超限） | `internal`（点位级） | 否 | 否 |

`4.04` 不计入熔断是刻意的：配置里写错一个路径，不应该让整台设备被判死。`5.xx`
计入熔断，因为它是服务端持续不可用，继续打只会加重问题。

#### 3.10.15.13 能力边界（刻意的）

| 不做 | 理由 |
|---|---|
| DTLS | 证书/密钥管理 + 握手状态机 + 防重放窗口，独立子系统 |
| Observe 订阅 | 需要长期维护观察关系、通知序号与重连后重新注册 |
| Block1 分块发送 | 需要服务端配合的逐块确认；现场写入极少超过单包，半套实现比不做更危险 |
| CBOR / SenML | 结构复杂，先明确报「不支持」，不猜 |
| DELETE / FETCH / iPATCH | 配置工具动作，不属于采集进程 |
| 组播发现 | 需要网络侧配合与去重策略 |
| 响应缓存 | 引入「两份数据谁更新」的问题，多一次往返可接受 |
| JSON 读-改-写 | 隐藏的读会带来更复杂的失败模式，且掩盖「整文档替换」语义 |

#### 3.10.15.14 位置与自检

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/coap`（`Encode`/`Decode`、选项 delta/length、`EncodeUint`/`DecodeUint`、`EncodeBlock`/`DecodeBlock`、`SplitURI`、`ParseLinkFormat`） |
| 主站驱动 | `internal/driver/coap`（`Meta`/`Open`/`Probe`/`Read`/`Write`/`Discover`；内部 `roundTrip`/`get`/`decodeValue`/`encodeWritePayload`/`jsonWritePayload`/`pathOptions`；诊断 `BadDatagrams`） |
| 共享解码 | `internal/codec/textval`（文本/JSON 标量 → 平台值；MQTT 与 CoAP 共用） |
| 内置服务端 | `internal/sim/coap`（`NewServer`/`NewServerWithOptions`/`Addr`/`SetValue`/`Value`/`Resources`/`Requests`/`Writes`/`BlockGets`/`BadPackets`/`SeparateAcked`/`Close`） |
| CLI | `uic coapread`（`-addr`/`-path`/`-selector`/`-type`/`-timeout`/`-block`/`-probe`/`-discover`/`-write`/`-payload`/`-method`/`-content-format`/`-json`/`-raw`） |
| 示例 | `examples/coap.json`（3 台设备 12 点位，REST 8099，服务端 5683） |

内置服务端的默认资源与示例配置一一对应：

| 路径 | 格式 | 初值 | 方法 | 行为 |
|---|---|---|---|---|
| `/sensors/temp` | text/plain | `21.5` | GET | 静态 |
| `/sensors/humidity` | text/plain | `45.0` | GET | 静态 |
| `/sensors/env` | application/json | `{"temperature":21.5,"humidity":45.0,"pressure":1013.25,"ok":true}` | GET | 可配合 `selector` |
| `/actuator/valve` | text/plain | `false` | GET/PUT | 布尔执行器 |
| `/actuator/setpoint` | text/plain | `0` | GET/PUT | 数值设定值 |
| `/actuator/trigger` | text/plain | `false` | GET/POST | POST 触发（不幂等） |
| `/config` | application/json | `{"mode":"auto","threshold":10}` | GET/PUT | JSON 配置文档 |
| `/blob` | application/octet-stream | `00 01 02 ff` | GET | 二进制资源 |
| `/large` | text/plain | 3000 字节 | GET | Block2 分块（默认 128 字节/块） |
| `/separate` | text/plain | `7.5` | GET | 空 ACK + 分离响应 |
| `/reset` | text/plain | `x` | GET | 回 RST |
| `/.well-known/core` | application/link-format | 资源清单 | GET | 发现入口（不列出自己） |

服务端的 MID 去重是这套自检最重要的一条性质：同一个客户端用同一个 MID 重发 CON 时，
服务端**重发同一条响应，不重新执行**。写请求不幂等，没有这条缓存，「客户端以为丢了、
补发一次」就会变成「阀门动作了两次」。测试用 PUT 两次同 MID、断言写计数只加一。

自检链路（无硬件，UDP 回环）：

```bash
./uic sim -protocol coap -listen 127.0.0.1:5683 &
./uic coapread -addr 127.0.0.1:5683 -probe
# 链路正常: 127.0.0.1:5683（/.well-known/core 有响应）

./uic coapread -addr 127.0.0.1:5683 -discover
# 设备 127.0.0.1:5683 发布 11 个资源
# 路径                            资源类型              格式
# /sensors/temp                   temperature           0
# /sensors/env                    environment           50
# ...

./uic coapread -addr 127.0.0.1:5683 -path /sensors/temp
# /sensors/temp = 21.5（good）

./uic coapread -addr 127.0.0.1:5683 -path /sensors/env -selector temperature
# /sensors/env = 21.5（good）

./uic coapread -addr 127.0.0.1:5683 -path /actuator/setpoint -write 42
# OK   PUT /actuator/setpoint

./uic coapread -addr 127.0.0.1:5683 -path /config -selector mode \
  -type string -content-format json -write manual
# OK   PUT /config
```

示例配置带上网关跑真实采集：

```bash
./uic -config examples/coap.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8099/api/v1/points
curl -s -X POST -H 'Authorization: Bearer dev-token-0123456789' \
  -H 'Content-Type: application/json' \
  -d '{"value":{"type":"float64","num":55.5}}' \
  localhost:8099/api/v1/points/coap-sensors/setpoint/write
```

写接口默认返回 `quality: uncertain`（运行时在 `confirm=false` 时统一把乐观更新标为
uncertain，等下一轮采集确认）；加 `"confirm":true` 时会立刻回读一次，读到一致值就
转 `good`。这与 IEC 104 / DNP3 的写入语义保持一致。

测试矩阵（当前覆盖）：

| 层 | 用例 |
|---|---|
| `codec/coap` | 头字段与 TKL 边界、Token 8 字节、选项 delta/length 的 0..12/13/14 扩展与两种同时扩展、重复 Uri-Path（delta=0）、`15` 保留值拒绝、uint 编解码边界、Block2 `NUM/M/SZX` 往返与非法 SZX、URI 拆分（有无 scheme/端口/查询）、link-format 解析（带引号属性、空清单、缺 `>`） |
| `codec/textval` | 文本/JSON 标量 → bool/各宽度整数/浮点/字符串/bytes、`on/off`、科学计数法、越界（含 `int64`/`uint64` 边界、`NaN`/`±Inf`）、写路径越界与小数拒绝、`uint64` 不带符号回绕、超 2^53 的整数推断保持文本、`a.b` 与 `a[0]` selector、非整数方括号按字段名处理、整文档紧凑化 |
| `driver/coap` | 真实 UDP 回环：纯文本/布尔/JSON selector/整文档/octets 读、Block2 3000 字节重组、服务端不认 Block2 时降级、分离响应 + 回 ACK、RST 分类、`4.04` 地址错误不计熔断、**部分失败语义（混合成功与失败时 `err==nil`、全失败才返回错误）**、PUT/POST/JSON 子树/octets 写入与回读、数组下标拒绝、只读资源拒绝、越界值在发报文前拒绝（text/json 两条路径）、`uint64` 边界回读、`Probe`/`Discover`、超时重传次数（`maxRetransmit` 0/2）、ctx 取消零发送、坏包跳过、无关 CON 回 RST、读错误后重连、`Close` 幂等、并发 Read、配置校验表、标签默认值与别名 |
| `sim/coap` | CON/NON 响应类型与 MID 规则、同 MID 去重（写不重复执行）、Block2 多块与越界 `4.02`、`4.04`/`4.05`、Block1 `4.02`、空载荷 `4.00`、发现清单自洽（自动续块、不列自己、`rt`/`ct` 正确）、分离响应 + ACK、RST、畸形计数、块大小参数与非法回落 |
| `cmd/uic` | `-discover`/`-probe`/读/写/`-json`/`-raw` 的显示、`-type` 与 `-block` 校验、`-content-format` 归一化、文本值解析 |

### 3.10.16 SECS/GEM（HSMS/TCP + SECS-II：SVID/ECID 对象寻址，端口 5000）

SECS/GEM 是半导体与电子制造设备之间的通信约定。它由两层组成：**HSMS** 负责「在 TCP
上建立并维护一条会话」（谁在发、这条消息属于哪个事务、链路选没选中），**SECS-II** 负责
「消息体里装什么」（一组带头部类型字节的数据项）。这两层必须分开看：HSMS 的坑在状态机
与配对，SECS-II 的坑在格式字节与长度字节。

本节的写法与 DNP3、CoAP 那两节一致：先讲清边界，再讲线格式里真正会踩的地方，最后讲
寻址、读写、错误分类与自检。

#### 3.10.16.1 范围：只做 HSMS/TCP 主站，只做采集与设备常数

| 做 | 不做 | 原因 |
|---|---|---|
| HSMS over TCP（设备侧端口，默认 5000） | HSMS over 串口（旧式点对点链路） | 新装设备几乎都是 TCP；串口形态需要另一套成帧与流控，属于独立实现 |
| 主站（Active）发起 Select | 被动模式（Passive，监听等设备来连） | 本平台是采集侧，连接方向由网关决定；被动模式还要处理「谁先发 Select」 |
| Select / Deselect / Linktest / Separate | T3~T8 全套定时器、Data Delimiter、Ping、Reject 的完整应答语义 | 采集只需要「选中后交换 DATA」；其余是设备侧状态机的完整性要求，网关作为主站不必全实现 |
| S1F1/S1F2、S1F13/S1F14（编解码） | 主动发送它们 | 握手用 Select 就够；建立通信（S1F13）会改变设备侧的通信状态，属于调试动作 |
| S1F3/S1F4（状态变量批量读） | S1F11 之外的列表查询 | S1F3 是 SECS 里唯一「一次读多个变量」的原生手段 |
| S1F11/S1F12（名称列表，仅自检） | 用名称做点位寻址 | 名称是给人看的；采集按编号，避免每次读都先查名字 |
| S2F13/S2F14（设备常数读） | — | 常数也需要被采集（上限、模式、配方号） |
| S2F15/S2F16（设备常数写，ACKC7=0 接受） | S2F17/S2F31（时间）、S2F33~S2F45（报告定义与链接） | 那些是「改设备侧配置」的动作：配错会让设备停止上报，属于调试工具而非采集进程 |
| S5F1/S5F2、S6F11/S6F12（编解码） | 主动订阅事件/报警 | 网关按周期采集；事件上报要长期维护订阅关系与去重，留给后续独立实现 |

能力声明与实现一致：`Subscribe`、`Browse`、`Discovery` 都是 `unsupported`，`MultiRead`
是 `supported`（同类型编号合并成一次事务）。把「能解析 S6F11」标成「支持订阅」，会让上层
以为推送可用，然后永远等不到值更新。

#### 3.10.16.2 12 字节帧头：Stream 与 W 位挤在一个字节里

```
 0                   1                   2                   3
+-------+-------+-------+-------+-------+-------+-------+-------+
|S|W| 低7位(F) | Function |            SystemBytes (4)            |
+-------+-------+-------+-------+-------+-------+-------+-------+
|              数据长度(4)                  | MsgType | ByteOrder |
+-------+-------+-------+-------+-------+-------+-------+-------+
|                         数据（仅 DATA）                        |
+---------------------------------------------------------------+
```

- 字节 0：高 1 位是 **W 位**（这条 Primary 消息是否期待一条 Secondary 应答），低 7 位是
  Stream（1..15）。把 Stream 整个写成 8 位，会让 W 位被解读成 Stream 的最高位，于是
  S1F3 变成「S129F3」——某些设备会直接回 Reject，另一些静默丢弃。
- 字节 1：Function（0..255）。
- 字节 2..5：**SystemBytes**，事务配对标识，由发起方自选，应答方原样回显。
- 字节 6..9：数据长度，**只算数据区**，不含 12 字节头。
- 字节 10：消息类型。`0x00` 是 DATA；`0x01..0x0B` 是 Select/Deselect/Linktest/Separate/
  NotSelect/Reject 的请求与应答。
- 字节 11：字节顺序，标准只定义 `1`（大端）。本实现遇到非 1 直接报错，而不是「试试看」。

**控制消息的 Stream/Function 没有意义，线上填 `0xFF`。** 这是最容易漏的一条：一个只写
`Stream=0` 的实现，在抓包工具里看起来像是「S0F0 的数据消息」，而对端可能因此把它当 DATA
处理。控制消息的区分只看 MsgType。

#### 3.10.16.3 SECS-II 数据项：格式字节的低 2 位是「长度字节数」

每个数据项是「1 字节格式 + 1..3 字节长度 + 内容」：

```
格式字节 = (format << 2) | 长度字节数(1..3)
```

- `format` 占高 6 位：L=0、B=8、BOOLEAN=9、A=16、J=17、I8=24、I1=25、I2=26、I4=28、
  F8=32、F4=36、U8=40、U1=41、U2=42、U4=44。
- 低 2 位是长度字段**本身占几个字节**，不是长度值。`00` 与 `01` 都表示 1 字节（`00` 是
  历史写法，现场仍能见到），`10` 是 2 字节，`11` 是 3 字节。

四类真正会踩的坑：

1. **把「长度字节数」当成「长度区的第一个字节」。** 解析偏移会整体错一位，于是每个项都
   从错误的位置开始读。这种错误的症状特别像「设备数据格式不对」，因为读出来的值往往还
   是个合法整数，只是不是那个变量的。
2. **L 项的长度是「子项个数」，不是字节数。** 一条 L,3 后面跟三个子项，长度字段写 3。
   把它当字节数会让解析器只读 3 字节就以为列表结束。
3. **整数是定宽大端。** U4 必须正好 4 字节；3 字节的 U4 是畸形，不是「补一个 0」。本实现
   在 `U1/U2/U4/U8/I1/...` 的取值函数里严格校验宽度，宽度不符即报格式错误——静默补零会
   让「设备回了个错的格式」这件事永远查不出来。
4. **BOOLEAN 是 1 字节，非 0 即 true。** 标准不要求恰好是 1，`0x80` 也是 true。

编解码层的测试对 `00/01/10/11` 四种长度写法和 `255/256/65535/65536/16777215` 的边界都
钉了金字节，另有一条专门断言「L 项声明 16M 个子项但一个字节都没有」时不会按声明数预
分配——长度字段是 24 位，照它分配就是一条 12 字节的畸形帧申请几百 MB。

#### 3.10.16.4 防御：长度字段必须设上限

数据长度字段是 32 位的，理论上允许 4 GiB。读循环若照着它 `make([]byte, n)`，一条被噪声
篡改长度字段的帧就能让进程 OOM。本实现有 `MaxPayload = 16 MiB`：超过即按协议错误处理，
读、解、编三处都校验。SECS-II 的实际载荷远小于此——最大的常见消息是整批状态值或报告
定义，量级在几十 KB。

这个上限不是「保护措施之一」，而是必须的：HSMS 跑在 TCP 上，第一次读到的 12 字节完全可能
来自一条错的连接（比如把一个 HTTP 端口配进来了），它前四个字节就可能是 `0xFFFFFFFF`。

#### 3.10.16.5 会话：Select 必须在 Open 里完成

TCP 连上不等于可以发 DATA。链路处于 **NOT SELECTED** 状态时，设备对 DATA 消息的合法
应答是 Reject（或干脆丢弃）。因此握手放在 `Open` 里：

```
Open ──> TCP connect ──> Select.req ──> 等 Select.rsp
                                          ├─ status=0  → 链路选中，返回 Conn
                                          ├─ status≠0  → Open 失败（disconnected）
                                          └─ 超时/错帧 → Open 失败（timeout/protocol）
```

`Select.rsp` 的 status 是 2 字节大端，常见值：`0` 成功、`1` 不支持、`2` 设备忙、`3` 已处于
该状态、`4` 链路未激活、`5` 重复的系统字节、`6` 未定义的消息类型、`7` 非法消息、`0x0200`
请稍候。本实现把**任何非 0** 都当作 Open 失败并归为 `disconnected`（可重连）：`2`「设备忙」
和 `0x0200`「请稍候」确实是暂时的，重连有意义。

反过来，**如果第一条收到的不是 Select.rsp**（比如配错了端口，对面是一个 HTTP 服务），归为
`protocol`：这不是链路抖动，是配置错误，重试一万次也不会好。

关闭时主动发 `Separate.req` 再关连接。对端据此区分「正常结束」与「断链」——它的日志与
重连退避策略会不同。发送失败不影响关闭结果。

#### 3.10.16.6 事务配对：SystemBytes + Stream + Function-1 + W 位清零

`exchange` 的匹配规则有四个条件，缺一不可：

| 条件 | 说明 |
|---|---|
| `dm.SystemBytes == sys` | 只有同一次事务的应答才算数 |
| `dm.Stream == sf.Stream` | 应答的 Stream 与请求相同 |
| `dm.Function == sf.Function + 1` | 应答的 Function 是请求的 +1（S1F3→S1F4、S2F15→S2F16） |
| `!dm.WBit` | 应答不带 W 位 |

第二条与第三条是最容易写错的：如果按「Function 相同」去匹配，真正的应答会被当成「无关
消息」跳过，然后一直读到超时。这个 bug 的现象是「设备明明回了，驱动却总超时」——抓包
一看，设备回的每一条都是对的。

第四条同样重要：`W=1` 表示「这是一条请求」，它要么是设备主动推的事件（S6F11），要么是
别人的事务。**不匹配的消息一律丢弃，继续读**，而不是报错。设备在本平台等待应答的同时
异步推送报警是常态；因为一条推送让整次采集失败，弱网下会随机抖动。

#### 3.10.16.7 寻址：对象地址承载协议原生编号

SECS 的编号（SVID、ECID）在标准里就是 32 位无符号整数，没有可以打出来的符号名。因此
点位用**对象地址**：

| 表达式 | objectType | instance | property | 语义 |
|---|---|---|---|---|
| `svid:1001.value` | `svid` | 1001 | `value` | 状态变量，S1F3 读，**只读** |
| `ecid:200.value` | `ecid` | 200 | `value` | 设备常数，S2F13 读、S2F15 写 |

用对象地址而不是发明一套符号映射，理由与 OPC UA 的对象节点、SNMP 的 OID 相同：编号就是
协议原生的寻址单位，中间加一层名字只会多一处失配。名字确实存在（S1F12 会返回），但它是
**人读的信息**，不是寻址单位；自检命令里的 `-describe` 就是用来把编号和名字对上的。

点表策略 `PolicyFor("secs")` 是 `{Width: WidthProtocol, AllowObject: true}`：宽度不可推导，
因为同一个编号在不同设备上可能是 U4、A 或 F8，由点表声明的 `dataType` 决定如何解释。
把 SECS-II 的格式表复制进点表层，只会得到两份会互相漂移的规则。

`address.length` 对 `string`/`bytes` 是**接收上限**，0 表示不限制；短于上限要接受。把上限
当定长会让「设备这次只回了 3 个字符」被判成错误。

#### 3.10.16.8 读：按对象类型分组，一个 span 最多两次事务

`Read` 把 span 里的点按 `objectType` 分成两组，每组合并编号发一次请求：

```
svid 点 ──合并──> S1F3 (L,n{U4}) ──> S1F4 (L,n{L,2{U4,值}}) ──分发──> 各点
ecid 点 ──合并──> S2F13           ──> S2F14                   ──分发──> 各点
```

三个 svid 点加两个 ecid 点是 **2 次事务**，不是 5 次。这与 Modbus「一次读多个寄存器」是
同一个优化：SECS 事务有往返成本。

分发阶段逐点独立：应答里缺某个编号 → 该点 `address_error`；项格式与声明类型不符 → 该点
`protocol`；其余点照常返回。**批量读的价值就是「一次往返拿到多数」，不能因为一个坏编号
让整组作废。**

分组阶段就把点位解析成 `pointPlan`（连同编号、类型、可写标记）随组带走，而不是在读的
时候按点位 ID 回查设备点表。原因是外部调用方（自检 CLI、北向按需读）传进来的点可能不在
设备点表里，回查会拿到零值编号然后**静默读 0 号**——这个 bug 已经犯过一次，现在的写法
用 `planFor` 明确区分「点表内的点」与「按地址临时解析的点」。

#### 3.10.16.9 写：只有设备常数，ACKC7=0 才算接受

S2F15 的请求体与 S1F4 布局相同（`L,n{L,2{U4,值}}`），但应答 S2F16 只回**一个**字节
（EAC / ACKC7）：

| ACKC7 | 含义 | 驱动处理 |
|---|---|---|
| 0 | 接受 | 该批次全部标 `good` |
| 非 0 | 拒绝 | 该批次全部标 `unsupported`（设备侧拒绝） |

一条 S2F15 携带多个常数时，S2F16 只有一个确认码——它不告诉你「哪一个被拒」。因此自检
命令的 `-write` 只允许**一个编号**：多编号写无法区分失败原因，报出来的「失败」是不可
诊断的。

写只认点表里的点。`writable` 是设备配置里声明过的写权限，凭一个地址就允许写等于让任何
能构造 API 请求的人绕过它。**读可以按地址临时解析，写不行**——两者刻意不对称。

写 SVID 一律拒绝（`permission`）。SECS/GEM 里状态变量没有写语义，写它要走 S2F49/S2F41
那套「远程命令 + 应答确认」的流程；把 S1F3 读到的编号直接塞进 S2F15，设备会当作「写一个
不存在的常数」而拒绝，或者更糟：编号碰巧是一个 ECID，于是写错了对象。

值编码严格按声明类型，不做隐式转换：

- `string` 点写入数值 → 报错。曾经的实现直接取 `v.Text`，一个数值值的 `Text` 是空字符串，
  于是**静默写入一个空标签**。设备会「成功地」收到它，比报错危险得多。
- 整数点写入小数 → 报错，不截断。
- 布尔点写入非布尔 → 报错。

#### 3.10.16.10 超时与重发

设备标签：

| 标签 | 默认 | 范围 | 说明 |
|---|---|---|---|
| `commTimeout` | `5s` | > 0 | `Open` 时等 Select.rsp 的时长；超时即 Open 失败 |
| `retries` | `1` | 0..5 | 事务超时后的额外重发次数；0 表示只发一次 |

`attemptDeadline` 取「设备超时」与「ctx 截止时间」中**更早**的那个。这一条是关闭预算的
保障：调度器的关闭预算只有 10 秒，如果重发不受 ctx 约束，一轮采集能被拖成 N 倍超时。

重发只在 `timeout` 与 `disconnected` 两类错误上发生；`protocol`、`address` 立即返回——
重发一条被拒的消息不会变成功。

HSMS 跑在 TCP 上，重发只在「对端进程卡住但连接没断」时有意义。这也是默认只重试一次、
而不是 CoAP 的 4 次的原因：TCP 自身已经处理了丢包。

#### 3.10.16.11 错误分类

| 现象 | `ErrorKind` | 可重试 | 是否计入熔断 |
|---|---|---|---|
| 拨号失败、TCP 写/读失败、对端关闭 | `disconnected` | 是 | 是 |
| Select 被拒（status≠0） | `disconnected` | 是 | 是 |
| 超时（含重发耗尽）、ctx 取消 | `timeout` | 是 | 是 |
| 第一条应答不是 Select.rsp | `protocol` | 否 | 否 |
| 畸形帧头、长度超限、解码失败 | `protocol` | 否 | 否 |
| 应答里没有被请求的编号 | `address` | 否 | **否** |
| 写只读点（svid 或未声明 writable） | `permission` | 否 | 否 |
| 点表外地址写入 | `permission` | 否 | 否 |
| 设备拒绝写（ACKC7≠0） | `unsupported` | 否 | 否 |
| 值无法按声明类型编码 | `unsupported` | 否 | 否 |
| 配置错误（非 TCP 传输、标签非法、地址非法、类型不支持） | `unsupported` / `address` | 否 | — |

`address` 不计入熔断是刻意的：点表里写错一个编号，不应该让整台设备被判死。`protocol`
也不计入：对端说的东西我们听不懂，重试不会好，但也不代表设备离线。

质量映射：`timeout` → `timeout`，`disconnected` → `comm_error`，`address` →
`address_error`，其余 → `bad`。

汇总按 §3.3.1：SECS 一个 span 只有 1..2 次事务，但一次事务里可以装一整组 SVID。
应答里少了某个编号、某个编号格式解析失败，都只让那一个点变坏，同组的其它点照常
返回（这正是批量读的价值）；整轮仍 `err == nil`。只有 `timeout` / `disconnected`
这类链路级失败才短路后续组并向上返回。`address` 甚至不进 `firstErr`——点表里写错
一个编号不该让整台设备被判死。

#### 3.10.16.12 内置设备（`internal/sim/secs`）

内置设备是一个只应答的 HSMS 设备，用来在没有硬件时跑通整条链路：

| 编号 | 类型 | 初值 | 可写 | 说明 |
|---|---|---|---|---|
| SVID 1 | U4 | 25 | 否 | Temperature，单位 C |
| SVID 2 | A | `RUN` | 否 | State |
| SVID 3 | F4 | 21.5 | 否 | Reading，单位 V |
| ECID 10 | U4 | 300 | **是** | 温度上限 |
| ECID 11 | A | `AUTO` | **是** | 运行模式 |

三条刻意保持的性质：

1. **SVID 与 ECID 是两套独立的编号空间。** 同一个数字 1 既可以既是状态变量又是设备常数，
   仿真里用 `(对象类型, 编号)` 作键。共用一张表会让「把 SVID 编号发到 S2F13」这类主站
   错误被静默接受——那正是仿真最该抓住的错误。
2. **写未知 ECID 回 ACKC7≠0。** 真实设备对未知常数编号的应答是「拒绝」，而不是「新建一个」。
3. **未选中时 DATA 回 Reject；Deselect 之后重新变回拒绝。** 链路状态是真的在维护，不是
   一个永远为真的开关。

另外两条与「稳定」直接相关：

- `S1F11` 给空列表 = **全部 SVID**（标准语义），且 ECID 不会混进来。
- 设备侧发送时**只设写截止时间**。曾经用 `SetDeadline`（同时设读截止），空闲超过 2 秒的
  连接会在下一次读取时直接超时断开——现象是「主站每几秒重连一次」，设备日志里一串
  `broken pipe`。这个 bug 在内置设备里存在时，主站侧看起来像设备不稳定。

`PushEvent` / `PushAlarm` 让测试能构造「设备在主站等应答时异步推送」的场景，钉住主站的
丢弃行为。

#### 3.10.16.13 自检：先看编号和格式，再读值

自检命令 `uic secsread` 有六种模式。前五种的顺序上刻意把「看结构」排在「读值」前面；最后一种是写——
写之前必须先读回确认，因为 S2F16 的 ACKC7 是设备**对整批**的一个结论，它不告诉你「哪一个被拒」，
一次写多个常数等于把失败原因扔掉：

| 模式 | 动作 | 用途 |
|---|---|---|
| `-probe` | Linktest | 只验链路，不看点表 |
| `-describe` | S1F11/S1F12 | 列出设备声明的编号、名称、单位 |
| `-raw` | S1F3（或 S2F13） | 打印**原始 SECS-II 项**，不做类型转换 |
| 默认 | S1F3/S2F13 + 类型转换 | 按 `-type` 解释，输出值与质量 |
| `-write` | S2F15/S2F16 | 仅 `-object ecid`，仅单编号 |
| `-write` **+** `-verify` | 写 → 重读原编号 → 比对 | 确认写下去的不只是 ACK，还有设备里的值 |

`-raw` 是排查效率最高的一档。现场最常见的问题是「设备回的格式与点表声明不一致」（点表
写 int32、设备回 `A "RUN"`）；先看原始项就能一眼定位，而不是从一个「类型不符」的错误里猜。

`-describe` 会显式提示「设备未返回编号 N」——请求了却没回，说明该编号在设备上不存在，
这比静默少一行有用。

自检链路（无硬件，TCP 回环）：

```bash
./uic sim -protocol secs -listen 127.0.0.1:5000 &
./uic secsread -addr 127.0.0.1:5000 -probe
# 链路正常: 127.0.0.1:5000（Linktest 有响应）

./uic secsread -addr 127.0.0.1:5000 -describe -id 1,2,3,9
# 设备声明 3 个状态变量
# 编号      名称                        单位
# 1         Temperature                 C
# 2         State
# 3         Reading                     V
# （设备未返回编号 9）

./uic secsread -addr 127.0.0.1:5000 -raw -id 1,2,3
# 对象    编号      格式      原始项
# svid    1         U4        U4 25
# svid    2         A         A "RUN"
# svid    3         F4        F4 21.5

./uic secsread -addr 127.0.0.1:5000 -object ecid -id 10 -write 350
# OK   ecid 10 = 350

./uic secsread -addr 127.0.0.1:5000 -id 1 -write 5
# SECS/GEM 的状态变量（svid）只读：写设备常数请加 -object ecid
```

写只报 ACKC7=0 时，加 `-verify` 重读原编号做最终确认——ACK 是设备的**意图**
（「请求合法、已受理」），不是**结果**（值已变成 X）。真实设备上这两者会分岔：
ACK 被设备接受但值随后被内部联锁拉回、写进影子副本而非生效副本、设备侧异步校验
失败——这些都不会反映在 S2F16 里，只有重读才能看见。自检命令对「重读回来的值」
与「要求写入的值」做逐字节比对，不一致时**失败退出（exit 1）**而不是打印一条
「看起来像提示」的警告后返回成功：警告会被人忽略，而一次写错工艺设定值的代价
远大于一条被误报的告警。

`-verify` 的比对规则与驱动读路径的严格程度一致：类型不符、编码不上的值一律判
不一致并输出原始项，不做「数值上近似就算通过」的宽容。

示例配置 `examples/secs.json` 带上网关跑真实采集（2 台设备 5 个点位）：

```bash
./uic -config examples/secs.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8100/api/v1/points
curl -s -X POST -H 'Authorization: Bearer dev-token-0123456789' \
  -H 'Content-Type: application/json' \
  -d '{"value":{"type":"uint32","num":360}}' \
  localhost:8100/api/v1/points/secs-constants/temp-limit/write
```

#### 3.10.16.14 代码位置

| 内容 | 路径 |
|---|---|
| 编解码 | `internal/codec/secs`（`Encode`/`Decode` HSMS 帧、`FrameDataLen`、`ParseFrame`、`EncodeItem`/`DecodeItem`、`MaxPayload`） |
| 消息语义 | `internal/codec/secs`（`EncodeIDList`/`DecodeIDList`、`EncodeStatusReply`/`DecodeStatusReply`、`EncodeStatusNameReply`/`DecodeStatusNameReply`、`EncodeConstantSend`、`EncodeConstantAck`/`DecodeAck`、`EncodeCommAck`/`DecodeCommAck`、`EncodeOnLineData`、`EncodeEventReport`、`EncodeAlarmReport`、`itemUint`） |
| 主站驱动 | `internal/driver/secs`（`Meta`/`Open`/`Read`/`Write`/`Probe`/`Close`；内部 `selectLink`/`exchange`/`awaitSecondary`/`readOne`/`readGroup`/`planFor`；值转换 `itemValue`/`itemFloat`/`valueItem`；自检扩展 `Inspector`/`Describe`/`RawRead`/`Verify`） |
| 内置设备 | `internal/sim/secs`（`NewServer`/`NewServerWithOptions`/`Addr`/`Name`/`SetVID`/`SetVIDNamed`/`SetECID`/`Value`/`ValueOf`/`IDs`/`IDsOf`/`SortedIDs`/`Requests`/`Writes`/`PushEvent`/`PushAlarm`/`Close`） |
| CLI | `uic secsread`（`-addr`/`-object`/`-id`/`-type`/`-timeout`/`-comm-timeout`/`-retries`/`-probe`/`-describe`/`-raw`/`-write`/`-verify`/`-json`） |
| 示例 | `examples/secs.json`（2 台设备 5 个点位，REST 8100，设备 5000） |

#### 3.10.16.15 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/secs` | 帧往返与金字节（含 W 位、控制消息 0xFF）、Stream 1..15 边界、字节顺序非 1 拒绝、长度字段与实际数据不一致、`FrameDataLen` 短头与超上限、`ParseFrame` 半帧/多帧、长度字节数 00/01/10/11、`encodeLen`/`decodeLen` 边界（255/256/65535/65536/16777215）、L 项子项数边界与畸形声明、各格式 `Item` 取值与宽度校验、BOOLEAN 非 0 即 true、`Int`/`Float` 的非定长格式、`FormatName`/`TypeName`/`StatusName` 兜底 |
| `codec/secs`（消息） | `L,n{U4}` 编号列表往返与 U1/U2 容忍、S1F4/S2F14 往返与金字节、S1F12 往返与第四项容忍、S2F15/S2F16 与 `B,1` 应答、S1F14 COMMACK、S1F2 机型版本、S6F11 多层报告与 `FindVID`、S5F1 ALCD 的发生/解除与类别保留、各消息体的畸形形状拒绝、空值与 nil 值 |
| `driver/secs` | 真实 TCP 回环：Select 握手成功/被拒/错应答/沉默、批量读 SVID（3 点 1 次事务）、混合 svid+ecid（2 次事务）、缺失编号 → `address`、类型不符、范围校验、字符串数字、异步推送被忽略、迟到应答被忽略、S2F15 写入与回读、只读 ECID、svid 不可写、点表外写入被拒、未知 ECID 被拒、重发一次后成功、ctx 取消、半帧/畸形帧头/对端挂断、`Probe` 的 timeout/protocol、`Close` 发 Separate 且幂等、系统字节回绕跳过 0、禁用点不入表、负 length 拒绝、**部分失败语义（坏地址 + 好点时 `err==nil` 且坏点带 `address`、全失败才返回整体错误、SVID 组链路失败后 ECID 组的点位显式标坏）**、`RawRead`（svid/ecid）、`Describe`、**`Verify`（写后重读一致 / ACK 接受但值没变 → 判失败）**、值转换全类型与全错误分支 |
| `sim/secs` | 默认值与两个独立编号空间、`SetVIDNamed` 与 S1F12、S1F11 空列表=全部 SVID、未知编号回空列表、S2F15 写入与未知 ECID 拒绝、未选中 Reject、Deselect 后 Reject、S1F1 无 W 位不应答、S5F1 报警推送与解除、S6F11 事件推送、未选中不推送、计数器语义、Separate 关闭连接、畸形请求容错、空闲 2.5 秒不断链、重复监听报错、二次 Close 幂等 |

覆盖率（`go test -cover`）：`codec/secs` 94.6%、`driver/secs` 89.1%、`sim/secs` 95.1%。

### 3.10.17 IEC 60870-5-101（FT1.2 非平衡轮询 + 总召唤 + 命令，串口 / 串口服务器）

101 常被描述成「104 的串口版」。这句话只在**应用层**成立：ASDU 的类型标识、传送
原因、品质位、时标、命令格式完全一样，本仓库因此把这一层抽成共享包（§3.10.17.13）。
但承载层是另一套物理事实，不能照搬：

| 事实 | 104（以太网） | 101（串行总线） |
|---|---|---|
| 谁可以发言 | 双方都可以，靠序号与 k/w 流控 | **只有主站**；从站被点名才回答 |
| 数据怎么来 | 从站按变化主动上送 | 主站轮询第 1/2 级；不轮询就永远没有新值 |
| 可靠性靠什么 | 15 位发送/接收序号 + S 帧确认 | 帧计数位 FCB + 链路确认 + 应用层激活确认 |
| 连接怎么建 | TCP 三次握手 + STARTDT | 复位链路（固定帧 FC=0），没有会话概念 |
| 点位地址宽度 | 固定 ORG 1 / CA 2 / IOA 3 | 每一项都是工程参数，按装置手册配 |
| 帧边界 | APCI 长度字段 | 起始符 + 长度字段 + 校验和 + 结束符 |

这六条差异决定了驱动的形状：`Read` 不再「查本地表」，而是**真的去走线**（§3.10.17.5）；
重传必须原样重发同一帧而不是重新组帧（§3.10.17.7）；写入必须等到应用层确认而不是
链路确认（§3.10.17.8）。

#### 3.10.17.1 范围：非平衡主站，只做采集与命令

| 做 | 不做 | 原因 |
|---|---|---|
| FT1.2 三种帧（固定帧 / 可变帧 / 单字节） | FT1.1、FT2 等其它帧格式 | FT1.2 是 101 的常用配套；换帧格式是另一套编解码 |
| 非平衡式主站（主站发起全部事务） | 平衡式（从站可主动发送） | 平衡式需要另一套冲突处理与从站角色，不属于采集侧 |
| 复位链路、请求链路状态、请求第 1/2 级、发送确认 | 请求制造/测试链路、复位进程的完整语义 | 这些是调试动作；采集只需要「复位 → 轮询 → 确认」 |
| 站总召唤 + 电能量召唤 | 分组召唤（QOI 21..36） | 整站快照已经覆盖采集需求；分组召唤是降低总线负载的优化，先不做 |
| 单点/双点/归一化/标度化/短浮点命令 | 带时标的命令、选择-执行两段式 | 选择-执行需要维护「已选择」状态与超时，属于独立的安全流程 |
| 串口（RS-485/RS-232）与串口服务器 TCP 透传 | 拨号、无线专网等其它承载 | 分帧相同，`link` 层已经把两种承载统一 |
| 监视方向对象缓存 + 命令回填 | 时钟同步的写路径 | 写时钟会改设备时间，必须走显式运维流程，不能藏在采集驱动里 |

`Subscribe` 声明为 `unsupported`：从站不能主动开口，没有「订阅」这件事。声明 supported
会让上层以为「不读也有新值」，然后永远等不到更新。

#### 3.10.17.2 三种帧与两个长度陷阱

```
固定帧（无用户数据）          可变帧（带 ASDU）                单字节
┌────┬────┬──────┬────┬────┐  ┌────┬───┬───┬────┬────┬─────┬────┬────┐   ┌────┐
│10  │ C  │ A..A │ CS │16  │  │68  │ L │ L │68  │ C  │A..A │ASDU│ CS │16 │   │ E5 │
└────┴────┴──────┴────┴────┘  └────┴───┴───┴────┴────┴─────┴────┴────┘   └────┘
 起始 控制 链路地址 校验 结束   起始 长度(低高) 起始 控制 地址 数据 校验 结束    肯定确认
```

三个必须按字节记住的点：

1. **长度字段只计「控制域 + 链路地址 + ASDU」**，不含两个 `68`、两个长度字节、校验和
   与结束符。整帧长度 = `L + 6`。按「含头」写会多算 6 个字节，接收方读到一半就等超时。
2. **校验和从控制域第一个字节起累加，到 ASDU 末字节止**，取低 8 位；不含起始符、长度
   字段、校验和自身与结束符。
3. **单字节帧没有地址，也没有控制域**。`E5` 是肯定确认/无数据，`A2` 是否定确认。把
   `A2` 当成 `E5`，症状是「设备明明拒绝了，主站却报成功」——本实现把原始字节留在
   `Frame.Byte` 里，调用方不必猜。

黄金字节（测试里逐字节固定）：

| 报文 | 字节 |
|---|---|
| 复位链路（地址 1） | `10 40 01 41 16` |
| 请求第 1 级数据（地址 1） | `10 5A 01 5B 16` |
| 无数据应答（固定帧） | `10 09 01 0A 16` |
| 从站用户数据（1 字节 ASDU） | `68 04 04 68 08 01 01 00 CS 16` |
| 主站带数据（FCB=1） | `68 L L 68 73 01 ASDU CS 16` |

链路地址宽度是 0..2 字节。`0` 表示不带地址（部分配套允许），此时固定帧只有 4 字节。
**不要从报文里猜宽度**：猜错就整体错位，而错位后校验和多半仍然对得上（校验只覆盖
控制域之后），于是错误会一路传到 ASDU 解析。

#### 3.10.17.3 控制域：FCB、FCV 与访问需求位

```
主站 → 从站（PRM=1）
 bit7  PRM=1
 bit6  FCB（帧计数位）
 bit5  FCV（帧计数位有效）
 bit4  FCV=1 时固定为 1
 bit3..0 功能码

从站 → 主站（PRM=0）
 bit7  PRM=0
 bit6  ACD（访问需求：还有第 1 级数据）
 bit5  DFC（数据流控制：缓冲满，暂时收不了）
 bit4  固定为 0
 bit3..0 功能码
```

| 方向 | 功能码 | 含义 |
|---|---|---|
| 主站 | 0 | 复位远方链路 |
| 主站 | 3 | 发送/确认用户数据（带 FCB/FCV） |
| 主站 | 9 | 请求链路状态 |
| 主站 | 10 | 请求第 1 级数据（事件/命令回执） |
| 主站 | 11 | 请求第 2 级数据（周期/背景数据） |
| 从站 | 0 | 肯定确认 |
| 从站 | 1 | 否定确认（链路忙） |
| 从站 | 8 | 用户数据 |
| 从站 | 9 | 无数据 |
| 从站 | 11 | 链路状态 |

三条工程约定：

- **FCB 只在「带用户数据」的帧上翻转。** 固定帧的请求没有状态，反复请求第 1 级不会
  让从站误判重复。
- **ACD 只是提示，不是数据。** 有的装置置了 ACD 却不在确认帧里带数据，必须显式请求
  第 1 级；反过来，也有装置 ACD 清零但仍有缓冲数据。本实现以「本轮还有没有拿到新
  数据」为准，ACD 只用于决定是否继续请求。
- **DFC=1 是「稍后再来」，不是错误。** 本实现把它归成可重试的协议错误，由采集层下
  一轮重试；把它当断链会让一台缓冲慢的装置永久离线。

#### 3.10.17.4 剖面：宽度是工程参数，不是协议常量

应用层字段宽度由「伴随标准」的工程配置决定，同一份代码必须能按不同装置调整：

| 字段 | 默认 | 允许值 | 设备标签 |
|---|---|---|---|
| 链路地址 | 1，1 字节 | 0..65535，宽度 0..2 | `linkAddress` / `linkAddressSize` |
| 公共地址 CA | 1（`device.unitId`），1 字节 | 1..65535，宽度 1..2 | `commonAddress` / `commonAddressSize` |
| 信息对象地址 IOA | 2 字节 | 宽度 1..3 | `ioaSize` |
| 源发地址 ORG | 0 字节 | 宽度 0..1 | `orgSize` |
| 总召唤周期 | 5 分钟 | 任意时长，0 = 只召唤一次 | `interrogate` |
| 第 2 级轮询 | 开 | true/false | `class2` |

默认值取「一台装置一个公共地址、站内点数不超过 65535」这一组现场最常见的工程参数。
把某一组当成标准写死，是接不同厂家设备时最先撞上的墙：帧能收发、校验也对，只是
所有字段整体错位一到三个字节。

**四类报文没有信息对象地址**（本仓库早期版本在这四类上多写了 IOA，等于对真实装置
发了一条畸形报文）：

| 类型 | 内容 | 101 剖面下的完整字节（CA=1） |
|---|---|---|
| 总召唤 `100` | 限定词 QOI | `64 01 06 01 14` |
| 电能量召唤 `101` | 限定词 QCC | `65 01 06 01 05` |
| 时钟同步 `103` | 7 字节时标 | `67 01 06 01 <CP56Time2a>` |
| 初始化结束 `70` | 1 字节初始化原因 | `46 01 04 01 00` |

共享编解码层用 `HasIOA(typeID)` 统一表达这条规则，编码与解码两侧都走同一个判断，
避免「写的时候不写、读的时候还读」这类单边错误。测试用黄金字节把四类报文逐字节
钉住（`internal/codec/iec60870/profile_test.go`）。

#### 3.10.17.5 一次 `Read` 到底做了什么

```
Open:
  建链（串口 / TCP 透传）
  复位链路（固定帧 FC=0）           ← 失败则开口失败
  请求链路状态（FC=9，尽力而为）     ← 失败只记日志

Read（每轮）：
  总线锁 + 排空接收缓冲
  链路未复位？→ 补一次复位
  到总召唤周期？→ 站总召唤 → 取数直到终止报文
                 → 电能量召唤 → 取数直到终止报文
  请求第 1 级 → 有数据就继续请求第 1 级（最多 32 条）
  请求第 2 级 → 同上（`class2` 打开时）
  从「点位 ID → 最新值」表组装样本
```

四个设计选择：

1. **总召唤不放在 `Open` 里。** 一台几百个点的装置在 9600 的总线上传完整站快照要
   好几秒，塞进 `Open` 会让「连接超时」与「装置没数据」两件事混在一起。总召唤由
   第一次 `Read` 触发，失败只影响那一轮。
2. **每轮都请求第 1 级。** 事件是串行总线上的主要数据源；只做周期总召唤的实现在
   现场表现为「变化要等五分钟才看到」。
3. **后续轮次一律请求第 1 级。** 协议规定第 1 级优先于第 2 级；反过来会让事件被
   周期数据挤在后面。
4. **连续取数有上限（32 条/轮）。** 这不是协议限制，而是公平性：一台疯狂上送事件的
   装置不能把整个采集周期占满，让同一进程里其它设备饿死。超出的留在从站缓冲。

**半双工纪律**：发之前先 `Drain` 再设截止时间（顺序反过来会把本次事务的截止时间
压成过去时刻，症状是「连上了但每次写都超时」）。总线锁覆盖「排空 → 发送 → 收完帧」
整段，一条 RS-485 上挂多台从站时事务之间必须互斥。

#### 3.10.17.6 总召唤与电能量召唤：两种「召唤」都要做

总召唤（`100`）只覆盖监视方向，**累计量不在站召唤范围内**：电能量必须用 `101` 单独
召唤一次。只发站召唤的实现，现场表现为「电度点位永远没有值」——它其实只是走另一条
路径。本实现总召唤后无条件补一次电能量召唤；没有累计量的装置会回否定确认或直接不
给数据，协议上完全正常。

终止报文的形态现场至少有三种：

| 形态 | 说明 |
|---|---|
| `100` + 激活终止 | 最常见；明确表示「这一轮送完了」 |
| `100` + 激活确认 | 有的装置把确认放在数据之前，**不能**当成结束 |
| `70` 初始化结束 | 装置重启后的「我准备好了」，也当作一轮结束 |

本实现只把「类型 100/101 的激活终止」与「类型 70」当作结束；激活确认只记一笔。
把确认当结束，会在第一条数据到达时就收工——症状是「总召唤只回来一个点」。

#### 3.10.17.7 帧计数位与重传：同一帧字节，而不是同一份内容

链路确认丢了怎么办？重发。但**重传必须原样重发同一帧字节**，包括 FCB 的值：

```
主站：FCB=1 命令帧 ──→ 从站：执行 + 回 E5（丢了）
主站：FCB=1 同一帧 ──→ 从站：识别为重复帧 → 只回 E5，不再执行
```

如果重传时重新组帧（FCB 翻转成 0），从站会把它当成一条**新命令**再执行一次。
症状是「确认超时后开关被拉合了两次」——现场把这归成「设备抖动」，其实主站自己发了
两条。本实现的 `sendUser` 保存整帧字节，超时后用同一份字节重发一次；从站侧用
「最近一次被接受的 FCB」去重，重复帧同样回确认（不回确认会让主站一直重发）。

复位链路是唯一的「帧序号归零」手段：重连后第一条命令如果沿用旧 FCB，会被装置当成
重复帧丢弃。因此 `Open` 第一件事就是复位链路；链路状态变差时 `ensureLink` 会补一次。

#### 3.10.17.8 命令：链路确认 **不是** 写成功

一条命令在 101 上有两层确认：

```
主站 → 单点命令（ASDU，FCB 翻转）
从站 → E5                        ← 链路层：报文到了
从站 → 单点命令 + 激活确认        ← 应用层：命令收到了，要执行
从站 → 单点命令 + 激活终止        ← 可选：执行完了
```

把第一层当成功，是这类驱动最常见的一处谎报：帧到了不代表装置执行了，装置完全可能
因为「未知 IOA」「不允许遥控」而拒绝。本实现等到应用层激活确认（`cause=7`）才返回
成功；否定确认或 `44..47`（未知类型/原因/公共地址/IOA）会按错误分类返回，而不是笼统
的「写入失败」。

写成功之后，在装置的自发上送到达之前，用请求值回填本地缓存（质量 `uncertain`），让
紧接着的 read-back 读到「刚写下去的值」。只回填「没有」或「比命令更旧」的条目：
无条件覆盖会抹掉一条更新的上送，而事件型上送可能很久都不再来一条。

#### 3.10.17.9 缓存按点位 ID，不按 IOA

一个 IOA 可以同时是遥信与遥控：`ioa:2003` 是布尔状态，`double-command:2003` 是
可写的双点命令。两套语义、两种平台类型。**按 IOA 缓存会让后写入的一方覆盖另一方**，
症状是「布尔遥信显示成 double 的 `2`」。本实现（以及 104）按**点位 ID** 缓存：

```
values["breaker"]     = bool true       ← 监视方向上送
values["breaker-cmd"] = uint16 2        ← 命令回填 / 同为 double 的上送
```

质量码只来自两处：ASDU 里的品质位（IV/NT/SB/BL/OV）与「这个点有没有值」。
不按「多久没更新」降级：一台温度半小时不变是正常的，按时间刷 stale 会把整页点位
变成陈旧——这类误报比没有质量码更糟。

故障时的措辞也是接口的一部分：只在控制方向存在的点（`single-command` 之类）永远等
不到监视方向上送，报「还没收到数据」会让人一直去查点表；本实现明确说「这个 IOA 在
监视方向不存在，写一条命令才会填充本地缓存」。

#### 3.10.17.10 两种承载：串口与串口服务器

现场有两种同样常见的接法：一根 RS-485 直接进盒子，或者一个串口服务器把同一条总线
透传到以太网。分帧、校验、事务序列完全一样，差别只有「字节从哪来」。两种都由
`internal/driver/link` 统一：

- **串口**：按物理路径去重（同一个 `/dev/ttyUSB0` 被多台设备配置时复用同一个 fd），
  参数冲突直接报错，事务串行化由端口的总线锁保证；
- **TCP**：一个连接对应一台设备或一个透传口，锁退化成每连接一把互斥量，省略端口时
  按 `4001` 处理（串口服务器的常见默认值）。

同一根物理线不可能同时跑两种分帧：共享层用 `Proto="iec101"` 拒绝静默复用，否则两种
帧会交错，症状是随机的校验错。

#### 3.10.17.11 错误分类与诊断

| 现象 | 分类 | 可重试 | 现场含义 |
|---|---|---|---|
| 复位链路无应答 | `timeout` | ✓ | 串口参数、接线、链路地址 |
| 校验和/帧结构错 | `protocol` | ✓ | 波特率/校验位不稳、总线干扰 |
| 从站回 `A2` / DFC=1 | `protocol`（忙） | ✓ | 装置缓冲满或正在初始化 |
| 公共地址不符 | `protocol` | ✓ | `commonAddress` / `commonAddressSize` 配错 |
| IOA 不在设备点表 | `address` | ✗ | 点表错；命令被装置拒绝 |
| 链路被关闭 | `disconnected` | ✓ | 串口掉线 / TCP 断开 |

写入等命令确认这一步的错误质量码按错误种类派生（超时→`timeout`、链路掉线→`comm_error`、
从站忙/其它→`bad`），而不是写死成 `timeout`：等确认时既可能是「话没说到」的超时，也可能是
等确认途中链路断掉（`ErrDisconnected`）或从站忙（`ErrProtocol`）。一律记 timeout 会让运维把
「链路断了」当成「接线/波特率」去查。

诊断接口（自检命令会打印）：

- `Observed()`：装置实际送过的信息对象（IOA、类型、传送原因、品质、时标、命中点位）；
- `BadFrames()`：帧校验通过但 ASDU 解析失败或公共地址不符的条数——它回答的是
  「帧通了、内容不对」，与「一帧都没收到」是两种完全不同的故障；
- `Frames()`：累计收发的帧数，用来区分「没在通信」与「通信了但没数据」；
- `Dropped()`：超出诊断表上限（4096 个 IOA）被丢弃的条数。

#### 3.10.17.12 自检与内置从站

```bash
# 起一个内置从站（FT1.2 over TCP；串口服务器透传同形）
uic sim -protocol iec101 -listen 127.0.0.1:4001

# 完整采集 + 对象清单（首轮总召唤，等待 2 秒后再轮询一次看事件）
uic iec101read -addr 127.0.0.1:4001

# 只验证链路层（复位链路 + 请求链路状态）
uic iec101read -transport serial -port /dev/ttyUSB0 -baud 9600 -link-addr 1 -probe

# 下发命令（等到应用层激活确认才返回成功）
uic iec101read -addr 127.0.0.1:4001 -area double-command -cmd-ioa 2003 -write on

# 剖面不同：1 字节 CA、2 字节链路地址、1 字节 IOA
uic iec101read -addr 127.0.0.1:4001 -link-size 2 -link-addr 4660 -ca-size 1 -ioa-size 1
```

自检命令刻意**不依赖点表**：初次接一台陌生装置时，点表本身就是要靠这条命令做出来
的。它列的是装置实际送过的对象，而不是「我们配置的点现在是什么值」。

内置从站（`internal/sim/iec101`）复现的是主站最容易搞错的五处，而不是「一整套协议
栈」：

1. 从站不主动开口，只回应点名；
2. FCB 去重：重复帧只回确认、不再执行命令；
3. ACD：还有数据时置位；
4. 总召唤 = 一份快照 + 一条终止报文（电能量召唤用类型 `101` 终止）；
5. 命令两段确认（链路确认 + 应用层激活确认/终止）。

调试内置从站有一个容易踩的坑：默认对象集的 IOA 是四位数，用 1 字节 IOA 剖面测试时
必须 `WithNoDefaults(true)` 再自己 `Add`，否则从站组帧失败并断连，主站只看到一个
莫名其妙的 EOF。

#### 3.10.17.13 与 104 的共享：一份应用层，两个承载

```
internal/codec/iec60870/    应用层：ASDU 编解码、Profile、品质/时标、点位语义
internal/codec/iec101/      FT1.2 帧、流式收帧、Reader
internal/codec/iec104/      APCI（I/S/U 帧、序号、k/w）+ 对应用层的别名转发
internal/driver/iec101/     非平衡轮询主站
internal/driver/iec104/     平衡式主站
```

共享包 `iec60870` 里有两层：

- **ASDU 层**（`asdu.go`、`names.go`）：类型标识、VSQ、传送原因、元素编解码、
  `Profile{OrgSize, CASize, IOASize}`、按剖面构造请求；
- **点位语义层**（`point.go`）：对象类别（`ioa` 与五种命令）、`PlanPoint`、`Convert`、
  `QualityOf`、`Worse`、`Numeric`、`DoubleState`、`BuildCommandProfile`。

为什么点位语义也要共享：「双点 0/3 是非法状态而不是中间态」「归一化设定值必须落在
-1..1」「未声明命令类型的可写点按平台类型推导命令」——这些判断与承载方式无关，写
两遍必然漂移。漂移的症状不是编译错误，而是「同一份点表走串口和走网口行为不一样」，
现场极难定位。

两个承载的地址形态完全相同，因此点表层用同一份策略：`PolicyFor("iec101")` 与
`PolicyFor("iec104")` 都返回 `{Width: WidthNone, AllowRegister: true, RegisterAreas: iecAreas}`，
白名单是 `ioa` 加五种命令类别。`WidthNone` 不只是「推导长度时返回 0」——它反转了
两条寄存器协议的习惯（`length` 不该出现、布尔量不该带位号），细节见 §3.10.17.14。

#### 3.10.17.14 点表这一层：`WidthNone` 反转了两条寄存器习惯

远动协议的地址与 Modbus/S7 长得一样（`area:offset`），语义却不同。点表层不能靠
「协议名特判」区分它们，否则每加一个承载就要改一次；本实现用的是宽度模式：
`PolicyFor("iec101")` 与 `PolicyFor("iec104")` 都返回 `Width: WidthNone`，`parseRegister`
里两个判断挂在 `pol.Width == WidthNone` 上。

```
ioa:1001                ✅ 监视方向，值宽由 ASDU 类型标识决定
ioa:1001:2              ❌ length is not expressed by this protocol
ioa:2001                ✅ 布尔量 = 单点对象，不需要位号
ioa:2001.3              ❌ bit addressing is not used by this protocol
double-command:2003     ✅ 控制方向
holding:0               ❌ area "holding" is not valid for protocol "iec101"
Plant.Temp              ❌ protocol "iec101" does not accept symbol addresses
```

**为什么必须在这一层拒绝，而不是留给驱动**：`uic import` 会在 CI 里跑（没有设备），
一份语法错的点表应当在导入时就报出来；拖到 `Open` 阶段，表现是「网关起来了，但每个
周期所有点位都返回 address_error」，排查成本高一个量级。同理，长度与位号这两种误配
都是「从寄存器点表抄过来」的症状，导入期一句话就能说清。

这一节的存在本身是一次缺陷的产物：`WidthNone` 加进点表层时，`parseRegister` 里还留着
两条寄存器习惯 —— 「bool 必须有位号」与「长度按 `dataType` 推导」。前一条让一份**完全
正确**的远动点表在导入期被全数拒绝：

```
line 3: address: bool point needs a bit (e.g. ioa:2001.0) (got "ioa:2001")
line 5: address: bool point needs a bit (e.g. single-command:4001.0) (got "single-command:4001")
```

最麻烦的地方在于它**不会**被示例配置发现：`examples/iec101.json` 直接写结构化 JSON，
绕过了地址解析。自检 `uic -check` 通过、端到端测试全绿，CSV 路径仍然是坏的。因此现在
`internal/pointtable` 里有专门的用例（`TestPolicyForIEC60870`、`TestParseAddressIEC60870`、
`TestParseAddressIEC60870Rejects`、`TestOtherProtocolsRejectIECAreas`、
`TestParseCSVIEC101`），并且断言的是**两个承载一致**：101 与 104 的每一种写法都在
同一个表里跑两遍。

还有一条刻意的「不校验」：点表层不管 IOA 的合法范围（0 与超过剖面上限都放行）。剖面
是设备标签，点表层看不到；驱动在 `Open` 阶段按 `ioaSize` 算出上限 1..16777215 并给出
准确报错（`point "temp": IOA 16777216 out of range 1..16777215`）。如果这里「顺手」
加一个 `offset >= 1`，剖面差异就测不到了。

#### 3.10.17.15 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/iec60870` | 8 种剖面组合（ORG 0..2 × CA 1..2 × IOA 1..3）编解码回环；四类无 IOA 报文的黄金字节（总召唤 `64 01 06 01 14`、电能量召唤 `65 01 06 01 05`、时钟同步 `67 01 06 01 <CP56Time2a>`、初始化结束 `46 01 04 01 00`）；无 IOA 类型带地址或多对象时拒绝；超宽地址在编码前拒绝而不是静默截断；CP56Time2a 逐位掩保留位；QDS 与 SIQ/DIQ 品质位按标准位序（BL/SB/NT/IV 在 bit4..7，QDS 的 OV 在 bit0、SIQ/DIQ 的状态在 bit0）逐位映射，IV 不漏报成 good；`HasIOA` 覆盖 100/101/103/70 |
| `codec/iec101` | 三种帧的黄金字节（复位链路 `10 40 01 41 16`、请求第 1 级 `10 5A 01 5B 16`、无数据 `10 09 01 0A 16`、从站数据帧）；`L` 只计「控制域+链路地址+ASDU」；校验和范围；链路地址 0/1/2 字节；单字节帧保留 `E5`/`A2` 的原始字节；`ParseAt` 对噪声逐字节跳过、半帧返回「需要更多字节」；`Reader` 跨多次 `ReadFull` 拼帧 |
| `driver/iec101` | 真实 TCP 回环 14 例：复位链路 → 链路状态 → 首轮总召唤 → 电能量召唤 → 第 2 级；ACD 继续取数；连续取数 32 条上限；终止只认 100/101 的激活终止与 70（激活确认不算结束）；五种命令下发与回填；未知 IOA → `address`；只读对象 → `address`；错误链路地址/公共地址/IOA 宽度 → 明确报错而非超时；无监听 → `disconnected`；`Probe` 只做链路层；`Close` 幂等；配置单测覆盖默认/覆盖/非法 labels、`maxIOAOf`、剖面范围、缓存按点位隔离、控制方向解释、无地址帧、FCB 翻转、`qualityOf` 按错误种类派生（超时→timeout / 断链→comm_error / 地址→address_error / 其余→bad） |
| `sim/iec101` | 直接按帧驱动从站 21 例：链路过程（复位/状态/未知功能码 NACK/对端 PRM=0 不应答）；总召唤快照（9 个对象、命令对象与累计量排除、ACD 边界、终止后队列空）；电能量召唤用类型 101 终止；FCB 重传只执行一次、翻转后执行、复位后清除记忆；命令两段确认 + 自发回填；未知 IOA / 只读对象 / 未知类型回否定确认；公共地址不符仍回链路 ACK 但不产生数据；`SetValue` 不把命令对象变成监视点；`SetBool` 同步双点数值；小剖面（1 字节 IOA）与无地址链路；非法剖面在监听前拒绝；`Close` 幂等 |
| `pointtable` | `PolicyFor("iec101"/"iec104")` 都是 `WidthNone` + 六个对象类别白名单；正向 11 例（含大写 area、16777215 大 IOA、布尔量无位号）× 两个承载；反向 12 例（寄存器区名、地址内长度、length 列、位号、符号/对象地址、负偏移/非数字偏移/空偏移）× 两个承载；寄存器协议反向拒 `ioa:1001`；CSV 全链路 10 点位 + length 列拒绝 |

覆盖率（`go test -cover`）：`codec/iec60870` 73.9%、`codec/iec101` 73.0%、`codec/iec104` 88.2%、
`driver/iec101` 72.7%、`driver/iec104` 92.0%、`sim/iec101` 82.9%、`sim/iec104` 95.7%、
`pointtable` 90.7%。

`codec/iec60870` 与 `driver/iec101` 的覆盖率偏低是结构性的：前者包含「剖面不合法」的
大量拒绝分支与 `TypeName`/`CauseName` 的兜底分支，后者包含串口路径（CI 无串口设备，
只能在有 `/dev/ttyUSB0` 的机器上跑）。这两处都按「分支写了就必须有用例」处理，没有为了
数字去删错误路径。

#### 3.10.17.16 位置

| 文件 | 内容 |
|---|---|
| `internal/codec/iec60870/asdu.go` | ASDU 编解码、Profile、`HasIOA`、请求构造 |
| `internal/codec/iec60870/point.go` | 对象类别、点位规划、值转换、命令构造 |
| `internal/codec/iec101/frame.go` | FT1.2 三种帧、控制域、校验和、`ParseAt`/`FrameLen` |
| `internal/codec/iec101/io.go` | 流式收帧 `Reader`（噪声跳过、半帧等待） |
| `internal/driver/iec101/driver.go` | 非平衡轮询主站 |
| `internal/sim/iec101/server.go` | 内置从站（TCP 透传承载） |
| `cmd/uic/iec101read.go` | 自检命令 |
| `examples/iec101.json` | 与内置从站一一对应的示例点表 |
| `internal/pointtable/address.go` | `WidthNone` 与 `PolicyFor("iec101"/"iec104")` 的对象类别白名单 |

### 3.10.18 Mewtocol（计算机链接 ASCII 帧：RCC/WCC 按字 + WCS 单点写，串口 / 串口服务器）

Mewtocol 是松下 FP 系列 PLC 的「计算机链接」协议（也常被叫作 MEWTOCOL-COM）。它最
容易让人误判的地方是**全 ASCII**：帧里没有一个二进制字段，抓包肉眼可读，看起来比
Modbus RTU 还简单。真正的问题是那些「看起来是十进制」的位置：

| 位置 | 进制 | 例子 |
|---|---|---|
| 站号 | 两位**十进制** | 16 号站写 `16`，不是 `10` |
| 数据区地址 | 五位**十进制** | DT100 → `00100` |
| 接点区地址 | 三位十进制 + 末位**十六进制** | X1F → `001F`，X16.3 → `0163` |
| 定时器/计数器接点 | 四位**十进制** | T10 → `0010` |
| 数据内容 | 每字节两位**十六进制**、字内**低字节在前** | H0063 → `6300` |
| BCC | 两位**十六进制** | 见 §3.10.18.2 |

同一帧里三种进制混排，任何一种读错都不会报「格式错」——PLC 会老老实实回一份**别的
地址**的数据。这是本协议在现场最常见的假绿：值有、质量好、对应关系全错。

ASCII 还带来一个副作用：没有长度字段、没有二进制同步字，帧边界只有帧头的 `%` 与
结尾的 CR。半双工总线上一份迟到的应答如果被当成下一轮的首部，症状是「数据永远滞后
一轮」——所以收帧层强制要求首字节是 `%`/`<`，其余一律当噪声丢弃（§3.10.18.9）。

#### 3.10.18.1 范围：计算机链接主站，只做裸帧

| 做 | 不做 | 原因 |
|---|---|---|
| `RD`/`WD` 字区读写（DT / LD / FL） | `SD`（整块置位写） | 一次覆盖整段而不读原值，属于初始化动作；采集侧不应有「顺手清一片内存」的能力 |
| `RCC`/`WCC` 接点区**按字**读写 | `SC`（整块置位接点） | 同上：整块写会把同字邻位一起改掉 |
| `RCS`/`RCP` 按点读接点（1..8 点） | 扩展头 `<` 的**发送** | 长帧头对端未必支持；收帧时认它，发送侧只发 `%` |
| `WCS`/`WCP` 单点/多点写接点 | FP2-ET-LAN 的 16 字节封装模式 | 那是 FP7 LAN 口的一种连接设置，报文中多一层头；正确做法是在 PLC 侧改成默认模式，而不是给驱动加一个「猜封装」的开关 |
| `RS`/`WS`/`RK`/`WK` 定时器/计数器值 | 时钟/日历读写 | 改设备时间必须走显式运维流程，不能藏在采集驱动里 |
| 串口（RS-485/RS-232）与串口服务器 TCP 透传 | 多主站、令牌传递 | 计算机链接是「一台主站 + 最多 99 台从站」的单主站模型 |
| 站号 1..99，广播 `FF` | 站号 0 | 0 在手册里不是合法单元号；接受它只会掩盖「站号没配」 |
| 裸帧 ASCII 形态 | 二进制 Mewtocol 变体 | 另一套编解码；本实现的边界是「手册里的 ASCII 帧」 |

`Subscribe` 声明为 `unsupported`：PLC 不会主动上送，所有数据都必须被点名（§3.10.18.6）。
把它标成 supported 会让上层以为「不读也有新值」。

#### 3.10.18.2 帧形状与黄金字节

```
请求   % 01 # R D D00100 00102 XX CR
应答   % 01 $ R D 6300 3344 000A XX CR
异常   % 01 ! 41 XX CR
       │ │  │ │  │                │  └ 结束符 0x0D
       │ │  │ │  │                └── BCC，两位十六进制 ASCII
       │ │  │ │  └─────────────────── 命令正文（各命令不同）
       │ │  │ └────────────────────── 命令码，两个大写字母
       │ │  └──────────────────────── `#` 请求 / `$` 正常应答 / `!` 异常应答
       │ └─────────────────────────── 站号，两位十进制 ASCII
       └───────────────────────────── 帧头（`<` 是扩展头）
```

**BCC 是从 `%` 起、到最后一位正文字符止，逐字节异或，取低 8 位**。它包含帧头与站号，
不包含结尾 CR。很多实现漏掉 `%`，得到的结果「大部分时候也对」——因为 `%` 的 0x25 只在
某些组合下才改变结果，等它出错时已经是现场了。

**`**` 只在「主站 → PLC」一侧合法。** 手册允许主站用两个星号代替真实 BCC，表示本帧
不做块校验，PLC 照样执行命令；应答侧没有这条豁免——PLC 的应答恒带真实 BCC。因此
`ParseRequest` 接受 `**`、`ParseResponse` 一律报 `ChecksumError`（可重试）。两侧共用
一份「看到 `**` 就放行」的校验，等于把一条未校验的字节流（噪声、别的协议串话、被网关
改写的帧）当成合法应答收下——而这类帧出现在总线上本身就是链路异常的信号，静默接受比
报错危险得多。

手册里的黄金字节（本仓库测试逐字节钉住，全部由实现直接打印得到）：

| 报文 | 帧 | 长度 |
|---|---|---|
| 读 DT100..DT102 | `%01#RDD001000010257\r` | 20 |
| 读应答（H0063/H3344/H000A） | `%01$RD63003344000A62\r` | 21 |
| 按字读接点 X0..X1 | `%01#RCCX000000010C\r` | 19 |
| 按字读应答（两字全 0） | `%01$RC000011\r` | 13 |
| 读单个接点 T10 | `%01#RCST001010\r` | 15 |
| 单点读应答（接点为 1） | `%01$RC120\r` | 10 |
| 读两个接点（X0.1 + T10） | `%01#RCP2X0001T001078\r` | 21 |
| 单点写 Y0.0=1 | `%01#WCSY0000128\r` | 16 |
| 单点写应答 | `%01$WC14\r` | 9 |
| 写 DT0=H0063 | `%01#WDD0000000000006355\r` | 24 |
| 按字写 Y0（一个字） | `%01#WCCY00000000000009\r` | 23 |
| 读 SV5 | `%01#RS0005000506\r` | 17 |
| 读索引寄存器 IX | `%01#RDIX00000000030\r` | 20 |
| 读索引寄存器 IY | `%01#RDIY00000000031\r` | 20 |
| 读索引寄存器 ID（32 位） | `%01#RDID0000000002C\r` | 20 |
| 读 IX 应答（IX=H1234） | `%01$RD341212\r` | 13 |
| 读 ID 应答（I0=H1234, I1=H5678） | `%01$RD341278561E\r` | 17 |
| 写 IX=H1234 | `%01#WDIX000000000123431\r` | 24 |
| 写 ID=H00010002 | `%01#WDID000000000020001002A\r` | 28 |
| 异常应答（41H 格式错） | `%01!4100\r` | 9 |

最短合法帧是 9 字节（`%` + 站号 2 + 符号 1 + 命令码 2 + BCC 2 + CR）。`MinFrame = 9`
因此不是「保守估计」，而是「空数据应答与异常应答的真实长度」——把它当成 8 会让
`%01$WC14\r` 被判成短帧。

#### 3.10.18.3 六个必须按字节记住的细节

**1. 站号是十进制。** 16 号站写 `16`；BCC 与数据是十六进制。同一帧两种进制，这是本
协议解析错误的第一来源。站号 1..99，广播是 `FF`（广播不期待应答，收到应答按协议错
处理——说明总线上有设备把 `FF` 当成了自己的号）。

**2. 接点编号是「前面十进制、末位十六进制」。** X/Y/R/L 的编号是 `字(3 位十进制) +
位(1 位十六进制)`：

| 点 | 线上接点号 |
|---|---|
| X0.0 | `0000` |
| X1.15（= X1F） | `001F` |
| X16.3 | `0163` |
| T10（定时器接点） | `0010` |
| C999 | `0999` |

所以点表里不让用户写 `X1F`，而是写 `x:1.15`（字地址 + 位号两个十进制整数）——把进制
混排关在编解码层里，就不会出现「`X1F` 与 `X1f` 算不算同一个点」这类问题。

**3. 字内低字节在前。** 寄存器值 H0063 在线上是 `6300`。`EncodeWords`/`DecodeWords`
在编解码边界做这次交换，对外一律给大端规范化的字节——让协议的小端习惯止步于这一层，
比让每个调用方都记着换一遍安全。

**4. 一次读用「起止地址」表达，不是「起点 + 个数」。** `RD` 的正文是 `区码 + 起始(5)
+ 结束(5)`，结束必须 ≥ 起始。写成「起点 + 个数」在点数为 1 时恰好等价，于是测试全过、
现场最后一个点位恒坏。

**5. 写的正文也带结束地址。** `WD` 是 `区码 + 起始(5) + 结束(5) + 数据`，不是「起始 +
数据」。长度其实由数据字符数决定，结束地址是冗余的；但协议要求它存在且与数据自洽，
否则 PLC 回 41H。本实现由数据长度**反推**结束地址，两者不可能不一致。

**6. 应答回显的命令码只有两个字符。** 请求 `RCC` 的应答是 `$RC`，不是 `$RCC`；`WCS`
的应答是 `$WC`。第三位是「单点/多点/字单位」的变体位，只在请求里出现。校验应答时只能
比前两位，按整条命令码比会把每一条接点命令都判成错帧。

另外两条不对称：

- **RCC/WCC 的地址是 4 位，RD/WD 的是 5 位。** 复制粘贴改命令码时会静默错位。
- **索引寄存器没有编号。** IX/IY/ID 的区码是**两个字符**，后面紧跟 9 个 0；起始与结束
  两段被这 9 个字符整体取代（不是各写 5 个）——`%01#RDIX00000000030\r` 里 `IX` 之后
  正好 9 个 0。也正因为那条命令里**没有地址字段**，一个索引寄存器点位占几个字只能由
  区码决定（见下一条）。
- **`ID` 是两个字的 32 位寄存器。** IX/IY 各回 4 个字符（1 字），ID 回 **8 个字符**
  （I0 的低位字 + I1 的高位字）。手册 §1.3.7 的应答帧图把这两种形态画成两张图，而不是
  「同一个区可长可短」。把它当成「所有索引寄存器都 1 字」的后果是：读 ID 恒报
  `response carries 2 words, request asked for 1`；写 ID 只发 I0、I1 保持旧值，却回
  「写成功」——而 I1 正是 32 位索引的高半部分。宽度因此收在 `codec.IndexWords(area)`
  一处，读、写、内置从站三边都问它。

#### 3.10.18.4 区域表：区码、编号体系、可用命令

| 区 | 配置键 | 区码 | 编号 | 可用命令 |
|---|---|---|---|---|
| 外部输入继电器 | `x` | `X` | 字 0..999 + 位 0..15 | `RCC`/`WCC` 按字；`RCS`/`RCP`/`WCS`/`WCP` 按点 |
| 外部输出继电器 | `y` | `Y` | 同上 | 同上 |
| 内部继电器 | `r` | `R` | 同上 | 同上 |
| 链接继电器 | `l` | `L` | 同上 | 同上 |
| 定时器接点 | `t` | `T` | 号 0..9999（十进制） | **只有** `RCS`/`RCP`/`WCS`/`WCP` |
| 计数器接点 | `c` | `C` | 号 0..9999（十进制） | 同上 |
| 数据寄存器 | `dt` | `D` | 0..99999（五位十进制） | `RD`/`WD` |
| 链接数据寄存器 | `ld` | `L` | 同上 | `RD`/`WD` |
| 文件寄存器 | `fl` | `F` | 同上 | `RD`/`WD` |
| 索引寄存器 | `ix`/`iy` | `IX`/`IY` | 无编号（区码 + 9 个 0），**1 字** | `RD`/`WD` |
| 索引寄存器（32 位） | `id` | `ID` | 无编号，**2 字**（I0 低字 + I1 高字） | `RD`/`WD` |

索引寄存器上的点位只接受 `address.offset = 0`：帧里的九个 0 就是「没有编号」的编码，
非 0 的 offset 根本发不出去。忽略它会让一个本该指向 DT100 的点位悄悄读到索引寄存器，
所以驱动在发帧之前就判地址错。同理，`ix`/`iy` 上的点位宽度超过 1 字（例如声明成
`uint32`）、`id` 上的点位宽度不是 2 字，也都在发帧之前拒绝，并直接给出该改成什么类型。
| 定时器/计数器设定值 | `sv` | 无 | 按号 | `RS`/`WS` |
| 定时器/计数器经过值 | `ev` | 无 | 按号 | `RK`/`WK` |

`SV`/`EV` 没有区码：协议为它们单开了四条命令，正文只有裸地址与数据。

现场常见别名一并接受，但**内部只认规范键**：`d`/`f`（西门子、三菱习惯）、`wx`/`wy`/
`wr`/`wl`（欧姆龙习惯）、`i0`/`i1`、`set`（SV）、`k`（EV）。如果别名各自成为一个键，
规划层会把同一片区域拆成两个 span，点位看起来「都在读」，实际每次只读一半。

#### 3.10.18.5 一次请求能带多少：`MaxWords = 24`、`MaxContacts = 8`

手册只给了帧长上限（`%` 头 118 字符），没给「一次能读多少字」。两个上限要自己推，而且
**读写受制的帧不是同一张**：

```
读请求 RD   % 01 # RD D 00100 00102 BCC CR      = 20 字节（定长）
读应答      % 01 $ RD <4n 字节数据> BCC CR      = 9 + 4n
写请求 WD   % 01 # WD D 00100 00102 <4n> BCC CR = 20 + 4n
```

- 读受**应答**限制：`9 + 4n ≤ 118` → `n ≤ 27`
- 写受**请求**限制：`20 + 4n ≤ 118` → `n ≤ 24`

取较小者 24，一个数覆盖读写两侧，调用方不需要区分。这个上限在两处各拦一次：组帧前
（错误信息能指名命令码）与驱动拆 span 时（错误信息能指名是哪台设备的哪一段）。只拦
一处的话，另一处的报错会是 PLC 回的 41H，而 41H 说不清「哪一段太长」。

接点按点读写（`RCP`/`WCP`）的上限是 **8**，来自命令正文第一位「个数」只有一位字符，
不是帧长决定的——所以它不能靠拆帧绕开，只能分批。

#### 3.10.18.6 一次 `Read` 到底做了什么

```
Read(spans)
 ├─ span 内所有点位必须同区（不同区的命令码与编号体系都不同）
 ├─ 区是 T/C 吗？
 │    ├─ 是 → readTimerContacts：每 8 点一组，1 点用 RCS、多点用 RCP
 │    └─ 否 → 算 [min,max] 字地址窗口 → 每 24 字切一刀
 │              → 依次 RCC（接点区）/ RD（字区）
 │              → **把各帧数据按 (地址-min) 拼进一个连续缓冲**
 └─ decodePoint：每个点位在缓冲里按 offset 取字、按 bit 取位
```

**为什么是「先拼接、后解码」而不是「读一帧、解一帧」。** 这是本驱动的一处真实缺陷：
逐帧解码时，`decodePoint` 会在每帧里对 span 的**全部**点位求偏移，于是第一帧为后面的点位报
`point lies before the response window`、第二帧为前面的点位报 `response shorter than
requested range`。两半各有一半点位坏，而 `Read` 因为有成功样本仍返回 `nil`——症状是
「超过 24 字的连续点位组每轮坏一半」，静默、无日志、值看起来还都在。拼接之后每个点位
只在**覆盖它的每一帧都读成功**时才解码，顺带也支持了「单个点位比一帧还宽」（例如 32 字
的字符串点位）：逐帧解码无论怎么对齐都解不出来，拼接之后就是一段连续字节。

链路级失败（超时/断连）在 span 内部也短路：后面的帧不再发出去，未发出的帧显式记为
「没发」而不是「读到 0」——少了这个区分，未发的帧会被当成成功，点位从全零缓冲里解出
一个看起来完全正常的 0。这正是 `attempted[]` 与 `frameErr[]` 要分开记的原因。

三处刻意的不对称：

1. **T/C 不走按字读。** 手册允许 `RCC` 读 T/C，但会按「每字 16 个接点」重新编号
   （0000 表示 T0..T15），这与 T/C 自身的十进制编号是两套体系；手册自己也建议不要
   这么用。所以 T/C 只走 `RCS`/`RCP`，代价是点位多时事务次数上升——T/C 通常只有几只，
   正确性优先。
2. **区间按 `[min,max]` 读，不按点位逐个读。** 读 DT0、DT5、DT6 三个点只发一次
   `RD DT0..DT6`；多读出来的字由 `decodePoint` 按 offset 丢弃。这是「规划层已合并 span」
   之后的第二次合并：规划层按 125 个寄存器合并，而本协议只能带 24 个。
3. **宽度回退到数据类型。** `pointWords` 优先用 `address.length`，缺省按
   `dataType.Registers()`（32 位 = 2 字）。这一条是必须的：手工构造的 span（自检命令、
   单元测试）通常不带长度，若一律按 1 字解，32 位点位会报「need 4 bytes, got 2」。
   它曾经真的发生过——修正后由 `TestPointWords` 与 `TestWordRoundTrip32` 两侧钉住。
   索引寄存器不走这条回退：`ix`/`iy` 上声明 32 位、`id` 上声明 16 位都按地址错拒绝，
   并直接说明「该声明成 uint16/uint32」，而不是让它静默落到别的区。

读失败时**每个点位都出一条失败样本**，而不是让它们从结果里消失：消失的点位在采集层
表现为「没有新样本」，会被当成「设备安静」，而不是「读取失败」。

`Read` 的返回值按全仓统一的部分失败契约（§3.3.1）收尾：`goodSamples` 按**样本**数，
不是按 span 数——一个 span 成功返回不等于其中有点位成功（`readSpan` 在逐点解码失败时
仍返回 `nil` error），所以「全员失败」必须真的向上报错，否则熔断与告警永不触发。
同一 span 里多帧之间的部分失败也遵守这条：前排帧读到的值保留，后排帧的点位标坏，
`Read` 不返回错误。

跨 span 时先收下已经拿到的样本、再处理失败（顺序反了会丢掉前排帧的真实数据），并且
标记失败时**跳过已经有点位的 ID**——否则同一 ID 会出现两个样本，一个真值一个错误。

#### 3.10.18.7 写：为什么布尔量只走 `WCS`

| 写入对象 | 命令 | 说明 |
|---|---|---|
| 接点区布尔量（X/Y/R/L/T/C） | `WCS` | 只改一位，不碰同字邻位；这是协议提供的**原子**单点写 |
| 接点区多个布尔量 | `WCP` | 一次最多 8 点 |
| 接点区按字 | `WCC` | 写一整字（16 个接点） |
| 字区（DT/LD/FL/IX/IY/ID） | `WD` | 每字 2 字节，最多 24 字 |
| SV | `WS` | |
| EV | `WK` | |
| **字区上的布尔点位** | **拒绝** | 协议没有「数据寄存器第 n 位」的写命令 |

最后一行是刻意的拒绝，也是本驱动最重要的边界之一。字区布尔的写入只能「读字 → 改位 →
写字」，那会把一次写变成两次事务；两个事务之间别的客户端可能改了同字的邻位，最后
写入者会把对方的位一起覆盖掉。协议既然给了原子单点写，就不该用非原子方式替代。驱动
返回 `unsupported`、质量码 `address_error`，错误信息直接给替代做法：

```
dt is not a contact area: a bool point on a word area has no atomic write in this protocol
(declare the point as uint16 and write the whole word)
```

T/C 的布尔量同样走 `WCS`，但位号必须省略（或显式写 0）：T/C 的接点号就是定时器号，
没有「第几位」。写 `t:10.3` 在驱动里直接按地址错拒绝，而不是悄悄把 3 忽略掉。

#### 3.10.18.8 `RCC` 与 `RCS` 是两套编号，别混用

`RCC`（按字读接点）与 `RCS`/`RCP`（按点读）读的是同一片 X/Y/R/L 内存，但**寻址语言不
同**：

| 方式 | 正文 | 含义 |
|---|---|---|
| `RCC` | `X` + `起始字(4)` + `结束字(4)` | 按 16 位字读，位在字内 |
| `RCS` | `X` + `接点号(4)` | 按单个接点读，1 字符 0/1 |
| `RCP` | `个数(1)` + 每点`区码 + 接点号(4)` | 一次 1..8 个接点，**可以跨区** |

驱动在读 X/Y/R/L 时用 `RCC`（一次拿 16 个接点，效率高），写布尔量时用 `WCS`（原子），
只有 T/C 用按点读。内置从站把两条路径都实现了，包括 `RCC` 对 T/C 的「每字 16 点重编号」
真实语义——如果从站图省事让 `RCC` 直接返回单点状态，主站就会以为「按字读 T/C 是安全
的」，而真 PLC 会给出一份编号完全不同、但长度合法的数据。

#### 3.10.18.9 事务纪律：静默、排空、锁、校验

半双工 ASCII 协议的正确性几乎全部落在「两次事务之间」：

| 纪律 | 做法 | 不做的症状 |
|---|---|---|
| 帧间静默 | 默认 10ms，`labels.options: "gap=20ms"` 覆盖 | RS-485 收发器还没从发送切回接收，下一帧首字节被吃掉 |
| 先排空再设截止时间 | `Drain()` 之后 `SetDeadline()` | 反序会把截止时间压成过去时刻：连得上，但每次写都 timeout |
| 总线锁覆盖整段事务 | `Lock` 从「排空」持到「收完一帧」 | 另一台设备的应答被当成本设备的，站号校验才拦得住 |
| 收帧只认 `%`/`<` | 其余字节逐字节丢弃并计数 `Dropped()` | 上一轮的迟到应答被当成本轮结果：数据永远慢一轮 |
| 校验应答站号 | 不等于本机即协议错 | 总线上两台 PLC 重号时，把 A 的数据记到 B 头上，且完全看不出异常 |
| 校验回显命令码 | 只比前两位 | 按整条比会把每条接点命令判成错帧 |
| `**` 只许出现在请求里 | `ParseRequest` 放行、`ParseResponse` 报错 | 两侧都放行 = 关掉了应答的校验位 |
| 超时文案带排查顺序 | 站号 → 波特率/校验 → PLC 是否在链接模式 | 现场只知道「没应答」，不知道先查哪一项 |

`Dropped()` 值得上报：一条 485 总线上它持续增长，说明有别的设备串话或波特率对不上；
它一直是 0 也不代表链路正常，只说明没收到过东西。

#### 3.10.18.10 错误码：四个类别与重试

异常应答是 `!` + 两位十六进制错误码。手册把它们分成四类，本实现保留分类而不是压成
一个「PLC 报错」：

| 类别 | 码 | 典型含义 | 驱动映射 | 可重试 |
|---|---|---|---|---|
| 链路系统 | `22`..`28`、`30`、`32`、`33`、`36`、`38` | 缓冲溢出、站号重号、硬件错、无应答 | `protocol`（重号/硬件错不可重试） | 多数可 |
| 基本过程 | `40`..`43` | BCC 错、格式错、命令不支持、过程错 | `protocol` | 可（BCC 错重发有意义） |
| 处理 | `50`..`53` | 链路设置错、发送禁止、忙 | `protocol` | 忙可重试 |
| PLC 应用 | `60`..`67`、`72`..`74` | 参数错、地址错、无数据、写保护、模式错 | `address`（60/61/66/67）、`permission`（63/65） | 不可 |

两个刻意的判断：

- **`60H` 参数错归 `address`，不归 `protocol`。** 它表示「区码或单元码不受支持」，是
  点表/型号配置问题，重试一万次也是同一个结果。
- **`23H` 站号重号、`24H` 硬件错、`26H` 站号设置错不可重试。** 把它们算进熔断重试，
  只会让一台配错的设备把整条通道拖黑。

质量码从错误码自身派生，不再手抄一份码表——`60/61/66/67` → `address_error`；链路类
（`ClassLink`，`22`..`38` 整段）与可重试的协议/处理类 → `comm_error`（重连或重发可能
就好，运维要查线路而不是查点表）；不可重试的应用类 → `bad`；超时 → `timeout`；断链 →
`comm_error`。

派生而非手抄是有来由的：早先的版本把链路类质量码硬编码成
`22/23/24/26/27/28/30/32/33/36/38` 这十一个码，而 `ClassLink` 实际是 `22`..`38` 整段，
漏掉了 `29/2A/2B/2C/2D/2E/2F/31/34/35/37` 共十一个。漏掉的表现是：这些链路码经 `classify`
归为可重试的 `protocol`，重发循环照跑，但 `qualityOf` 落 `bad`——运维看到的是「坏值」，
会去查点表而不是查线路。现在 `qualityOf` 直接读 `ErrorCode.Kind()`/`Retryable()`，码表
和分类逻辑只有一处事实源，不会再漂移。

#### 3.10.18.11 两种承载与「不做 FP2-ET-LAN 封装」

| 承载 | 参数 | 备注 |
|---|---|---|
| 串口 | `port`、`baud`（默认 9600）、`dataBits`（8）、`parity`（**odd**）、`stopBits`（1） | 计算机链接的出厂设置是**奇校验**；改成 none 的典型症状是「能发不能收」 |
| TCP 透传 | `address: host[:port]`，省略端口用 2000 | 串口服务器 / 以太网单元；端口各厂不同：2000、60001、9094、FP7 编程口 32769..32772 |

TCP 承载**只做裸帧**。FP7 的 LAN 口如果被设成「与 FP2-ET-LAN 连接」，会在 Mewtocol 帧
外再套一层 16 字节头（`10H 00H` + 长度 + 8 个 0 + 站号 + 连接号）。这种情况下驱动的
表现是「发出去没有任何应答」——因为 PLC 收到的是一份长度不对的怪帧。正确处理是在 PLC
侧改回默认模式，而不是给驱动加一个「猜封装」的开关：猜错时错误会更晚、更难查。

同一个 `Link` 也给多台设备共用一条总线时提供总线级互斥：同一路径同参数复用，不同参数
显式报错（不静默复用）。这条是串口层（`internal/transport/serialport`）的职责，驱动与
其它串口协议共享。

#### 3.10.18.12 自检与内置从站

`uic mewtocolread` 不依赖点表，回答「这条链路通不通、这台 PLC 说的是哪一种地址」：

```bash
uic mewtocolread -addr 127.0.0.1:2000 -station 1 -probe
uic mewtocolread -addr 127.0.0.1:2000 -area dt -offset 0 -qty 3 -json
uic mewtocolread -addr 127.0.0.1:2000 -area x -offset 0 -bit 1
uic mewtocolread -addr 127.0.0.1:2000 -area t -offset 10
uic mewtocolread -addr 127.0.0.1:2000 -area y -offset 0 -bit 0 -write on
uic mewtocolread -addr 127.0.0.1:2000 -area ix -raw
uic mewtocolread -addr 127.0.0.1:2000 -area id -qty 2 -raw
uic mewtocolread -addr 127.0.0.1:2000 -area id -write 0x00010002
uic mewtocolread -transport serial -port /dev/ttyUSB0 -baud 9600 -parity odd -probe
```

`-raw` 打的是**线上地址编码**与**原始字值**，而不是收发帧：驱动不对外暴露裸帧，
而地址编码恰好是本协议最容易错的地方。把 `X1F` 当十进制填进 `-offset` 时，`-raw` 会
显示 `线上 X0010`（应为 `X001F`），进制混排错误一眼可见；`-area id -raw` 显示
`线上 ID000000000, 原始 00010002`，直接看出 I0/I1 的高低字关系。索引寄存器没有编号，
`-offset` 必须是 0，`-qty` 被忽略（IX/IY 1 字、ID 2 字，由区码决定），写 ID 会自动按
32 位下发——`WDID` 只带一个字会把 I1 留在旧值上却回「写成功」。

`-probe` 固定读 DT0，不是随便挑的：`RD` 是最基础、所有 FP 系列都支持的读命令，DT0 在
出厂配置里一定存在；换成接点区会把「接点编号从 0 还是从 1 开始」这类型号差异混进探活
结果。探活失败时它把排查顺序直接打出来（串口参数 → 接线 → 站号 → PLC 模式 → FP7 LAN
封装模式），因为串行现场几乎所有失败都表现为同一句「没有应答」。

`uic sim -protocol mewtocol -listen 127.0.0.1:2000` 起内置从站，默认值与
`examples/mewtocol.json` 一一对应：DT0=0x0063、DT1=0x3344、DT2..DT3=25.5（float32，
低字在前）、X0.1/X0.5=on、Y0.0=off（可写）、T10=off、SV5=0。

从站复现的是主站最容易搞错的五件事：

1. **站号过滤**：不是本机站号的请求**不应答**（广播也不应答），与真 PLC 一致。
2. **两套接点编号**：`RCC` 返回 16 点一位图，`RCS`/`RCP` 返回逐点 0/1；不能互相顶替。
3. **字区位写拒绝**：`WCC` 对字区回 61H/66H，而不是「当成字写成功」。
4. **错误码**：未知区 `60H`、格式错 `41H`、命令不支持 `42H`、地址越界 `66H`、数据范围
   `61H`——让驱动的错误分类有真实的输入可测。
5. **`ID` 是 `IX`/`IY` 的 32 位视图**，不是独立寄存器：写 `WDID` 会同时改掉 I0 与 I1，
   读 `RDIX` 与读 `RDID` 的低半字必须是同一个值。从站按这个关系存储（`indexSlot`），
   否则自检时两边都正常、接真机才发现 32 位索引对不上。

主站与从站共用同一份区域表、接点编号函数与帧构造（`codec/mewtocol/request.go` 的
`ParseRequest`/`BuildResponse`）。如果两边各写一套，「主站自检通过、对接真 PLC 全错」
就会成为最坏的一种假绿。

#### 3.10.18.13 点表这一层：`WidthRegisters` + `BitlessAreas`

Mewtocol 的地址是**寄存器语义**：一个地址 = 一个 16 位字。因此 `PolicyFor("mewtocol")`
用 `WidthRegisters`（与 Modbus 同语义），而不是远动协议那套 `WidthNone`：

| 写法 | 解析结果 | 说明 |
|---|---|---|
| `dt:100` | `dt:100:1` | uint16/int16 → 1 字 |
| `dt:100`（float32） | `dt:100:2` | 32 位 → 2 字 |
| `x:1.15` | `x:1.15:1` | bool + 位号 |
| `t:10` | `t:10:1` | T/C 不带位号（`BitlessAreas`） |
| `dt:0.3` | `dt:0.3:1` | 字区上的 bool：**可读不可写** |
| `dt:0`（bool） | 导入期拒绝 | 寄存器协议的习惯：bool 必须给位号 |

新增的 `AddressPolicy.BitlessAreas` 是一个**区名集合**，而不是在解析器里按协议名硬编码
「T/C 例外」。原因：允许「布尔量不带位号」的协议会越来越多（每个协议对「接点」的定义
不同），把这条规则做成策略字段，解析器就只需要问「这个区是否在位号白名单里」。

区名白名单 `mewAreas` 是固定的十四个键。写错的区名（把 `dt` 写成 `d` 之外的拼法、把
`holding` 贴进来）在 `uic import` 阶段就被拒，而不是等到网关起来后每周期返回
`address_error`。

#### 3.10.18.14 字节序：低字节在前 + 低字在前 = 字序倒置

协议原生布局是「字内低字节在前、32 位值低字在前」：

```
值 H00010002
  线上：0200 0100      （低字 0x0002 在前，字内低字节 0x02 在前）
  规范化后：00 01 00 02
```

两处交换叠加，等价于「按 16 位字整体倒序」。`DecodeWords` 已经消掉字内交换，剩下的只是
字序——对应 32 位的 `CDAB` 与 64 位的 `DCBA`，也就是 `orderFor` 的默认值：

| 位宽 | 默认字节序 | 等价说法 |
|---|---|---|
| 16 位 | —（单字无字序） | 只需字内交换，已由 codec 完成 |
| 32 位 | `CDAB` | 低字在前 |
| 64 位 | `DCBA` | 低字在前，且 4 个字整体倒序 |

用户显式配 `byteOrder` 时以配置为准。这里有一个容易踩的坑：把 32 位值拆成两个 `uint16`
点位分别采集，会得到「高半与低半互换」的两个数——值都有、都能对上量程，但拼起来是错的。
要么用一个 32 位点位，要么明确知道自己在做什么。

#### 3.10.18.15 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/mewtocol` | 区域表与别名（`d`/`f`/`wx`/`set`/`k`）、站号校验（1..99、`FF`、0 拒绝）；BCC 覆盖 `%` 且不含 CR，手册实例 `%01#RCSX0000` → `1D`；`**` 的方向性（`ParseRequest` 接受、`ParseResponse` 判 `ChecksumError` 且错误文案说明原因）；黄金字节逐字节（RD/RCC/RCS/RCP/WCS/WCC/WD/RS/WS/RK/WK/异常应答/IX/IY/ID 的读写）；接点编号双向（`001F`/`0163`/`0010`/`0999`，非法进制与越界拒绝）；`IndexWords`（IX/IY=1、ID=2）与索引寄存器的「区码 + 9 个 0」，包括 `RDID` 读 0..1、`WDID` 两字、`WDIX` 一字，以及「索引寄存器传了非 0 偏移/错误宽度必须在组帧前拒绝」；`MaxWords=24` 与 `MaxContacts=8` 的边界（24 通过、25 拒绝；8 通过、9 拒绝）；`ParseRequest` 三字符命令优先于两字符前缀；`ParseResponse` 的 BCC 校验与异常应答；`FrameLen`/`Reader` 的分帧、噪声丢弃与半帧等待 |
| `driver/mewtocol` | 纯函数：`pointWords`（bool 恒 1 字、显式长度优先、类型宽度回退）、`reqWords`、`orderFor`（32 位 CDAB / 64 位 DCBA / 显式覆盖）、`classify`（66H→address、65H→permission、41H/53H→protocol 可重试、60H→address）、`qualityOf`（address/comm/bad/timeout）+ **派生一致性**（遍历整段链路码 `22`..`38` 与可重试协议/处理码断言一律 `comm_error`，钉住「手抄白名单漏码」这类漂移）；**批量写逐项语义**（一条成功 + 一条未知区名：成功项 `OK/Quality=good/Err=nil`、失败项带 `Quality` 与 `Err`、坏条目不上线发帧；把「回填 `firstErr`」加回去必须变红）；**多帧 span**（30 个连续寄存器：切成 24+6 两帧、拼接后 30 个样本不重不漏、值与顺序都对；第二帧超时时前排 24 个保留 good、后排 6 个 timeout 而 `Read` 不报错；第一帧就链路级失败时只发 1 帧、30 个点位全部 timeout、`Read` 报错）；**索引寄存器配置错误**（`ix` 带 offset、`ix` 声明成 uint32、`id` 声明成 uint16：一律 `address` 且**不碰总线**，错误文案指明该改成 `uint16`/`uint32`）；**写路径数值越界/带小数/float32 溢出/NaN 在发报文前拒绝**（`value.Encode` 返回 `ErrRange` → 归 `ErrAddress`，一字节不发；旧实现漏了范围校验，`70000` 写 uint16 会静默变 `4464` 且报「写成功」） |
| `sim/mewtocol` | 真实 TCP 回环：16 位字往返（写 DT0..2 再读回）、接点按字读 + 位提取、`WCS` 单点写后邻位不变、字区位写拒绝（`ErrUnsupported`）、32 位跨字往返（低字在前）、站号过滤（站号 2 的请求静默、站号 1 有应答）、T/C 按点读（T 与 C 分属两个区，分开读）、T/C 单点写不碰邻号、`WCP` 多接点写（两位同时置 1，同字其余位不变——数据位读反的症状是「写 1 变成写 0」）、**T/C 分组里一个带位号的坏点位不得顶偏后续点位的值**（旧实现按下标遍历含坏点的原组去取 `vals`，坏点会拿到邻点值、真正要读的点反而没样本）、**索引寄存器三种形态往返**（IX/IY 1 字、ID 32 位；读 ID 得到 `I1<<16|I0`，写 ID 同时改掉 I0 与 I1——从站里 ID 不是独立寄存器，而是 IX/IY 这一对的 32 位视图，与真机一致） |
| `pointtable` | `PolicyFor("mewtocol")` 是 `WidthRegisters` + 十四个区白名单 + `BitlessAreas` 只含 `t`/`c`；正向 14 例（字区、接点区、字区 bool、T/C、索引、SV、大写区名）× 往返（`Canonical()` 能重新解析成同一地址）；反向 5 例（未知区名、接点区缺位号、位号 16、字区 bool 缺位号、非 bool 带位号）；寄存器协议反向拒 `dt:0` |

覆盖率（`go test -cover`）：

| 包 | 覆盖率 | 说明 |
|---|---|---|
| `codec/mewtocol` | 78.4% | 未覆盖的主要是各命令的畸形输入分支 |
| `driver/mewtocol` | 62.4%（跨包） | I/O 路径由 `sim/mewtocol` 的真实 TCP 回环覆盖，按 `-coverpkg` 统计；只统计包内单测是 57.4%（`spanLink` 假链路把多帧切分与部分失败也纳进了包内），剩下的大多是需要真实链路的代码 |
| `sim/mewtocol` | 70.0% | |
| `pointtable` | 92.4% | |

驱动包内覆盖率低是结构性的，不是「测试没写」：驱动的价值在「一帧一帧地走线」，而
包内单测只能碰到纯函数。把它拉高的唯一办法是把从站回环搬进驱动包，那会让两份测试互相
依赖；本仓库选择「跨包统计 + 在文档里写清楚」，而不是为了数字造重复用例。

#### 3.10.18.16 位置

| 文件 | 内容 |
|---|---|
| `internal/codec/mewtocol/mewtocol.go` | 区域表与别名、站号、BCC、`RD`/`WD`/`RCC`/`WCC`/`RCS`/`RCP`/`WCS`/`RS`/`WS`/`RK`/`WK` 组帧、接点编号双向、`DecodeWords`/`EncodeWords`、`ParseResponse`、错误码分类与 `Retryable`、`FrameLen` |
| `internal/codec/mewtocol/io.go` | 流式收帧 `Reader`（只认 `%`/`<`、噪声计数、CR 定界） |
| `internal/codec/mewtocol/request.go` | `ParseRequest`/`BuildResponse`/`BuildErrorResponse`、接点数组编解码、应答回显码（主站与从站共用） |
| `internal/driver/mewtocol/driver.go` | 主站：区拆分与 24 字切分、位提取、T/C 按点读、写路径（WCS/WD/WCC/WS/WK）、站号与回显校验、错误分类 |
| `internal/sim/mewtocol/server.go` | 内置从站（TCP 透传、站位图、RCS/RCP、错误码、站号过滤） |
| `cmd/uic/mewtocolread.go` | 自检命令（probe / 读写 / json） |
| `examples/mewtocol.json` | 8 点示例（字区、float32、接点位、T 接点、SV、可写 Y） |
| `internal/pointtable/address.go` | `PolicyFor("mewtocol")`、`mewAreas`、`BitlessAreas`、区名前缀切分 |

### 3.10.19 Host Link（上位链接 FINS，头码 `FA`：ASCII 信封 + FCS，串口 / 串口服务器）

Host Link 是欧姆龙 CS/CJ/CP 系列 PLC「上位链接端口」用的协议。它最
容易让人误判的地方只有一处，但这一处错了整片全错：**它不是另一种协议，
而是 FINS 命令换了一层 ASCII 信封**。内存区、字/位寻址、读写语义、数值
字节序——与 FINS/TCP（§见 FINS 一节）逐字节相同，差别只在「怎么把一帧
发到线上」。

正因为如此，本仓库把 Host Link 的**命令语义**抽进 `driver/finscore`
（主站会话）与 `sim/finsmem`（从站内存表）与 FINS/TCP 共用，`codec/hostlink`
只负责「FINS 帧 ↔ Host Link 信封」这一层转换与收帧，驱动只负责把 FINS
帧套上信封发出去。同一份点表换个 `protocol`（`fins` ↔ `hostlink`）就能跑
——这不是巧合，而是「Host Link 就是 FINS 的一个承载」的直接推论（点表
接入见 §3.10.19.11）。

#### 3.10.19.1 范围：只做 FINS 帧（头码 `FA`），不做 C 模式

| 做 | 不做 | 原因 |
|---|---|---|
| FINS 内存区读写（`0101` 读 / `0102` 写） | C 模式（`RR`/`RD`/`WD`/`WR`/`RR`…） | C 模式是另一套命令集、另一套寻址（BCD「通道号 + 位号」），没有区码表也没有 FINS 结束码。混在一个驱动里，每个字段都要先问「现在走哪套」，而调用方只想要「读 DM100」 |
| 直连短格式 | 跨网长格式 | 直连格式精简头只有 `ICF/DA2/SA2/SID` 四个字段；跨网格式还要 `RSV/GCT/DNA/DA1/SNA/SA1` 六个（多 12 个字符）。目标由单元号承载、路由由上一跳决定，直连驱动不该去猜路由。真要跨网应走串行网关或以太网单元 |
| 串口（RS-485/RS-232）与串口服务器 TCP 透传 | 多主站、令牌传递 | 上位链接是「一台主站 + 最多 32 台从站」的单主站模型 |
| 单元号 0..31，缺省 0 | 单元号 32 及以上 | 手册上限；与 Mewtocol 缺省 1 不同（那边 0 是广播，这边 0 是合法的 CPU 单元） |

`Subscribe` 声明为 `unsupported`：PLC 不会主动上送，所有数据都必须被点名。

#### 3.10.19.2 帧形状：全 ASCII，收发不对称

**请求**（主机 → PLC）：

```
@ 00 FA 0 00 00 00 00 0101 820000000001 xx * CR
│ │  │  │ │  │  │  │  │    │            │  │ └ CR (0x0D)
│ │  │  │ │  │  │  │  │    │            │  └ 定界符 `*`
│ │  │  │ │  │  │  │  │    │            └ FCS（两位十六进制）
│ │  │  │ │  │  │  │  │    └ 命令正文（参数/数据，一字节两字符）
│ │  │  │ │  │  │  │  └ MRC/SRC（命令码）
│ │  │  │ │  │  │  └ SID（源端服务标识，重发计数用）
│ │  │  │ │  │  └ SA2（源单元地址）
│ │  │  │ │  └ DA2（目标单元地址，直连恒 0）
│ │  │  │ └ ICF（信息控制字段：直连 00，经网络中转 80）
│ │  │  └ 响应等待时间（**一位**十六进制，单位 10ms，0..F）
│ │  └ 头码 `FA`（FINS 命令；C 模式这里是两个字母）
│ └ 单元号（两位**十进制** ASCII，00..31）
└ 帧头 `@`
```

**应答**（PLC → 主机）在 `FA` 之后**多一个两位结束码、少一格等待时间**：

```
@ 00 FA 00 40 00 00 00 00 0101 0000 1234 xx * CR
      └┬┘ └────── 精简 FINS 头 ──────┘ └┬┘ └──┘
       │                                │   └ FINS 结束码（四位十六进制）
       │                                └ 数据（一字节两字符）
       └ Host Link 结束码（两位十六进制，00 正常）
```

这一节是本协议的「黄金字节」所在，其余小节都在解释它为什么长这样。

**六个必须按字节记住的细节**：

1. **收发格式不对称。** 应答比请求多一格结束码、少一格响应时间。按请求
   布局去解析应答，会把结束码当 ICF、后面每个字段整体错位两格，症状是
   「SID 对不上」或「数据永远是垃圾」。反过来把结束码写进请求，PLC 回
   0x14（格式错）或干脆不理。所以请求与应答是两条独立布局，不能用同一个
   `Parse`。

2. **FCS 的范围含 `@`。** 从帧头一直异或到正文最后一个字符，**不含** FCS
   自身、`*`、CR。少算 `@` 是最常见的写法错误，而且只在真机上暴露——本地
   回环里主从都少算，校验永远通过。

3. **正文是「一字节两字符」。** 区码 0x82 上线是字符 `'8'`、`'2'`
   （0x38、0x32），不是裸字节 0x82，因此**整帧是可打印 ASCII**，7 位数据位
   完全够用——这正是「FINS 必须 8 位数据位」这个流传很广的说法错掉的地方
   （串口缺省见 §3.10.19.8）。

4. **响应等待时间是 1 位，不是 2 位。** 取值 0..F，单位 10ms（最长 150ms）。
   写成两位会让 PLC 把第二位当成 ICF 的高位，整帧错位。

5. **精简头只有 4 个字段。** `ICF/DA2/SA2/SID`。路由用的六个字段在直连格式
   里不存在——目标就是本机单元，路由由上一跳决定。

6. **ICF 在直连时是 00，不是 80。** FINS/TCP 的请求惯用 0x80，但 Host Link
   直连要求 0x00（0x80 的含义是「要经网络中转」）。把 FINS/TCP 的头直接
   搬过来，PLC 会按「跨网」去解，然后找不到路由。应答对应回 0x40。

另外一条与 FINS/TCP 相同、但值得单列的进制事实：**单元号是十进制**。单元
号 31 上线是字符 `'3'`、`'1'`，不是 `'1'`、`'F'`。它与 FCS/数据的十六进制
混排，是这套协议第二个进制陷阱（第一个是等待时间只有一位）。

本仓库测试逐字节钉住的黄金字节（全部由实现直接打印得到，单元号 0、等待
时间 0、ICF=0x00）：

| 报文 | 帧 | 长度 |
|---|---|---|
| 读 DM0..DM1（2 字） | `@00FA00000000001018200000000027F*\r` | 34 |
| 读应答（DM0=1234、DM1=5678） | `@00FA004000000001010000123456784B*\r` | 35 |
| 写 DM0=0x1234（1 字） | `@00FA000000000010282000000000112347B*\r` | 38 |
| 按字读 CIO0（1 字） | `@00FA0000000000101B0000000000104*\r` | 34 |

对照第一条：`@` `00` `FA` `0`（**一位**等待时间，见第 4 条）`00` `00` `00`
`00`（ICF/DA2/SA2/SID）`0101`（MRC/SRC）`820000000002`（区码 82 + 起始字
0000 + 位号 00 + 数量 0002，一字节两字符展开）`7F`（FCS）`*` CR。对照第二
条：`FA` 之后先有两位 `00`（信封结束码），再有 `40`（应答 ICF），然后才是
精简头、`0000`（FINS 结束码）与数据 `12345678`。写请求把数量换成 1、末尾接
数据 `1234`，长度因此比读多 4 个字符。

**请求 34 字符、应答 35 字符，多出的正是那一格结束码**——这个「一个字符的
差」就是第 1 条「收发两条独立布局」的可执行证据。

#### 3.10.19.3 结束码：信封层与 FINS 层两套

Host Link 的错误信息分两层，处置方式完全不同，不能压成一个「PLC 报错」：

| 层 | 位置 | 取值 | 分类 |
|---|---|---|---|
| **Host Link 结束码** | 应答 `FA` 后两位 | `00` 正常 | `0x13` FCS 错 / `0x14` 格式错 / `0x15` 数据错 / `0x16` 命令不支持 / `0x18` 帧长错 等 |
| **FINS 结束码** | 应答末尾四位 | `0000` 正常 | 与 FINS/TCP 完全相同的结束码表（越界、无数据、写保护…） |

信封层结束码 `0x13/0x14/0x15/0x18` 说明**帧本身写错了**，修一次就永久消失，
不重发也无意义（`EndCodeError.Format()` 把它们标成「格式错」）。`0x03`
（写保护）/`0x23`（内存保护）归 `permission`，其余归 `protocol` 并保留一次
重试。FINS 结束码则交给共用的 FINS 结束码分类，能说出具体是哪类内存错。

**FINS 结束码为什么要按类分，而不是「非零一律协议错」**：结束码表（W342
§5-1「End Codes」）里主码 `11` 是 Parameter error，子码就是点表的四类地址
问题——`01` Area classification missing（区码不存在）、`02` Access size error
（访问宽度不对/奇数字地址）、`03` Address range error（起始地址越界）、`04`
Address range exceeded（起始 + 数量越界）；`0A` Duplicate data access、`0B`
Response too long、`0C` Parameter error 属于同一族「参数本身不对」。这些码
重发一万次结果都相同，且不该计入熔断（`driver.CountsTowardBreaker` 对
`ErrAddress` 直接返回 false）；主码 `02`（目标节点不在网、单元缺失、路由
表错）才是「重发一次可能就通了」的链路类。三类映射分别来自
`finscodec.EndCodeError.Address()` / `Link()`，Host Link 与 FINS/TCP 两个
驱动因此对同一个码给出同一个 `Kind` 与同一个重试决策——这是把信封差异隔离
在信封层、语义只留一份的直接收益。

**`0x1108` 为什么不在 `Address()` 里**：手册确实有一处「超过 498 字时按最大
值读、并以 `1108` 返回」，但它只出现在**参数区读（`02 01`）**的注释里；本驱动
只做内存区读写（`01 01`/`01 02`），内存区读的单帧上限是 999 字，撞不到那条
规则。更关键的是 `endCodeError` 的 default 分支给的是 `temporary=false`，
即「不认识就判坏、不重发、不熔断」——是 fail-closed，不是 fail-open。为一个
本驱动不会收到的码去扩白名单，只会让分类表与实际命令集脱节。

#### 3.10.19.4 单次上限：读 269 字、写 267 字（不是 999）

手册给的整帧字符上限是**跨网长格式**的数字（命令 1114 字符、应答 1115 字符）；
直连短格式在同样数据量下只有 1102/1103。但无论哪个格式，真正的约束是：
**FINS 命令不能在 Host Link 上分段**。所以上限由字符预算反推，而且**读写受
制的不是同一张表**：

```
读请求  …0101 <区码2><起始字4><位号2><数量4>…    正文固定 12 字符参数
读应答  数据 1076 字符 / 4 = 269 字              ← 读受应答帧长约束
写命令  正文 1080 字符 − 12 参数，再 / 4 = 267 字 ← 写受命令帧长约束
```

由此推出读 **269 字 / 538 位**、写 **267 字 / 534 位**。读写相差两个字不是
随意的：读看的是应答帧能带多少数据，写看的是命令帧能塞多少正文。这四个数
由 `codec/hostlink` 的 `MaxReadWords`/`MaxWriteWords`/`MaxReadBits`/
`MaxWriteBits` 承载，并通过 `Limits` 交给 `finscore` 切块——切块单位是
字/位，与 FINS/TCP 的 `fins.MaxWords = 999` 无关。照抄 FINS/TCP 的 999 分块
会稳定收到结束码 `0x18`（帧长错）。

#### 3.10.19.5 一次 `Read` 到底做了什么

```
Read(spans)
  ├─ 复用 finscore：span 内必须同内存区；按字/位拆成若干次事务
  ├─ finscore 按 Limits（269 字 / 538 位）切块 → 每块一个 FINS 命令
  └─ transactor.RoundTrip：FINS 帧 → FromFINS 套上 Host Link 信封 → 走收帧
       → 校验应答单元号、MRC/SRC 回显、FCS → 还原成标准 FINS 帧 → finscore 解码
```

命令语义（区拆分、字/位读取、EM 当前区的读字取位、低字在前字序、结束码
分类）全部在 `finscore` 里，与 FINS/TCP 共享，这里不复述。Host Link 驱动
只多做三件信封层的事：**套信封**（`FromFINS` 填单元号/等待时间/ICF=0x00）、
**收发校验**（单元号匹配、MRC/SRC 回显、FCS）、**结束码分层**。

#### 3.10.19.6 写：没有 RMW，位写走 FINS 原生位命令

| 写入对象 | 手段 | 说明 |
|---|---|---|
| 继电器区某一位（CIO/WR/HR/AR） | FINS 内存区**位写** | 原子单点位写，不碰同字邻位 |
| DM/EM 一个字 | FINS 内存区**字写** | 一次最多 267 字 |
| 多字（float32 等） | 字写，按长度切块 | 低字在前 |

关键点：FINS 的四个继电器区（CIO/WR/HR/AR）**支持原生位写**，所以 bool
点位不需要 §3.4.1 的「读-改-写」路径——这是 FINS 系协议相对 Modbus 保持
寄存器的一处结构性优势。写受命令帧约束（267 字），所以写入 span 的切块
比读取更保守，这一条由同一个 `Limits` 天然保证，不需要在驱动里再写一遍。

写路径的其余约束（数值越界/带小数/float32 溢出在发报文前拒绝、STRING/bytes
超长拒绝而非静默截断、链路级失败短路且保留前序成功、已读到的好样本不因后续
块失败被丢弃）全在 `finscore`，FINS/TCP 与 Host Link 共用同一份实现，详见
§3.10.6.5.1。

#### 3.10.19.7 事务纪律：静默、排空、单元号、SID

半双工 RS-485 上来的正确性几乎全落在「两次事务之间」：

| 纪律 | 做法 | 不做的症状 |
|---|---|---|
| 帧间静默 | `labels.options: "gap=5ms"`（缺省）覆盖 | RS-485 收发器还没从发送切回接收，下一帧首字节被吃掉 |
| 先排空再设截止时间 | `Drain()` 之后 `SetDeadline()` | 反序会把截止时间压成过去时刻：连得上，但每次写都 timeout |
| 总线锁覆盖整段事务 | `Lock` 从「排空」持到「收完一帧」 | 另一台设备的应答被当成本设备的 |
| 收帧只认 `@` 与 `*`+CR | `Reader.ReadFrame(unit, sid)` 逐字节扫、按 `*`+CR 定界 | 上一轮的迟到应答被当成本轮结果：数据永远慢一轮 |
| 校验应答单元号 | 不等即静默/报错 | 总线上两台 PLC 重号时，把 A 的数据记到 B 头上，完全看不出异常 |
| 校验 MRC/SRC 回显 | 命令码对不上即协议错 | 总线噪声或另一台主站的应答串入，被当成真实数据 |
| 超时文案带排查顺序 | 单元号 → 波特率/校验/停止位 → 端口模式 | 现场只知道「没应答」，不知道先查哪一项 |

与 Mewtocol 不同，Host Link **没有 SID 语义歧义问题**：SID 由主站自己维护
（`transactor.sid++`），只用于把应答配回请求，不承担「重发计数」的协议级
职责。但这也意味着盲目重发写没有事务号可对齐——所以驱动**不自动重试**，
重连交给调度器（见 §3.10.19.9）。

#### 3.10.19.8 串口参数：9600-7-E-2（不是 8-N-1）

这是与 Mewtocol 最容易混淆的一处。两个串口协议**出厂缺省完全相反**：

| 参数 | Host Link（上位链接） | Mewtocol（计算机链接） |
|---|---|---|
| 波特率 | 9600 | 9600 |
| 数据位 | **7** | 8 |
| 校验 | **偶（even）** | **奇（odd）** |
| 停止位 | **2** | 1 |

Host Link 用 7 位数据位是**正确**的：整帧是可打印 ASCII（正文是一字节
两字符的十六进制），7 位足够。把 FINS/TCP 用户的习惯（8/N/1）套过来会
「发得出去、收不到东西」——而且不报错。

`serialDefaults` 只在**用户完全没配**串口参数时补出厂值（9600-7-E-2）；只要
填了任一项，其余按通用缺省（8/N/1）走——那是用户已经知道自己在做什么的
信号，替他把校验位改回来反而更意外。这个策略与「驱动不替用户猜硬件参数」
的一贯立场一致：缺省给协议事实，覆盖交用户决定。

#### 3.10.19.9 不自动重试：没有事务号，重发写可能是脏写

`RoundTrip` 不做自动重发。理由与 Modbus 位写同源但更强：Host Link 没有
可靠的事务号可以排出「这条应答不是迟到的那一条」的歧义（SID 只 8 位，且
很多 PLC 固定回 00）。盲目重发一次写，可能把同一个字写两遍——对「加计数」
这类点位这是**真实的数据错误**，不是「重发无害」。

链路级错误（超时、断连）仍按 `classifyRead` 分类并允许上层重连：`ErrTimeout`
（可先查单元号、串口参数、端口模式）、`ErrDisconnected`（重连）、`ErrProtocol`
（帧格式/FCS 错，可重试但更需要查线）、`ErrPermission`（0x03/0x23 写保护）。
重连的代价由调度器决定，驱动只保证「一次事务要么完整成功、要么给出可分类
的错误」，不替它决定「要不要再发一次」。

#### 3.10.19.10 自检与内置从站

`uic hostlinkread` 不依赖点表，回答「这条链路通不通、这台 PLC 说的是不是
FINS 帧」：

```bash
uic hostlinkread -addr 127.0.0.1:2000 -probe
uic hostlinkread -addr 127.0.0.1:2000 -area dm -offset 0 -qty 4 -json
uic hostlinkread -addr 127.0.0.1:2000 -area cio -offset 0 -bit 1
uic hostlinkread -addr 127.0.0.1:2000 -limits
uic hostlinkread -transport serial -port /dev/ttyUSB0 -baud 9600 -dataBits 7 \
  -parity even -stopBits 2 -probe
```

`-probe` 固定读 DM0：它是所有欧姆龙 PLC 都存在的最基础地址，用它当探活点
不会把「探活点配得对吗」混进链路判断。`-limits` 打印四个单次上限（读 269
字/538 位、写 267 字/534 位）与推导过程，便于核对「为什么一次读不了 999
字」。探活失败时把排查顺序直接打出来（单元号 → 串口参数 → 端口模式），
因为串行现场几乎所有失败都表现为同一句「没有应答」。

写路径的命令行判定单独抽成纯函数 `hostLinkWriteRequest`，只有两条规则，
但都是「猜错就悄悄改数据」的高危分叉，因此单测钉死：

| 命令行 | 行为 | 猜错的代价 |
|---|---|---|
| 给了 `-bit N` | 一律按位写，**不判断该区有没有原生位访问码**。CIO/WR/HR/AR/DM 走 FINS 原子位写；EM 当前区没有位码，驱动退化成「读整字→改位→写整字」的 RMW | 早先 CLI 用 `bit >= 0 && a.BitCapable()` 判断，`-area em -bit 3 -write on` 掉进字写分支：`on` 按数字解析直接报错；换成 `-write 1` 更糟，它发整字写把 em5 其余 15 位全清 0 |
| 字写（没给 `-bit`） | 一次只写一个字（`Length=1`、`Bit=-1`），`-qty != 1` 直接拒 | 把 `-qty` 当写入宽度会补零清邻字，而且 8 字节宽度触发 DCBA 字节序，连本字都变形：`-qty 4 -write 5` 实测写成 `dm10=0x0500`、`dm11..13=0x0000` |

多字写入应当走点表（那里有类型与宽度）；自检命令是「写一次、看结果」，
不提供批量写。

`uic sim -protocol hostlink -listen 127.0.0.1:2000` 起内置从站，单元号
缺省 0，预置初值与 `examples/hostlink.json` 对应：DM0=0x1234、DM1=0x5678、
DM2/DM3=float32 3.14（低字在前）、CIO0.1=on、CIO0.5=on。从站逐字节扫帧、只
按 `*`+CR 定界，刻意复现三件真机会发生的事：

1. **单元号不匹配就完全沉默**（`req.Unit != s.unit` → 不出声），让「单元号
   配错」这条最常见故障测得出。
2. **FCS 错回结束码 0x13，而不是静默丢弃**——手册定义了这类结束码，回一个
   明确的码让「FCS 算错了」与「根本没收到」在自检时能分开。
3. **格式错回 0x14、认不出的头码回 0x16**——把响应等待时间写成两位（→0x14）
   与拿 C 模式帧当 FINS 发（→0x16）在真机上的表现，自检时能立刻定位。

从站与主机共用同一份内存表（`sim/finsmem`）、同一套 FINS 编解码
（`codec/fins`）与同一套信封编解码（`codec/hostlink`）。如果两边各写一套，
「主站自检通过、对接真 PLC 全错」就会成为最坏的一种假绿。

#### 3.10.19.11 点表这一层：`WidthRegisters` + `finsAreas`（与 FINS/TCP 共用）

Host Link 的地址是**寄存器语义**：一个地址 = 一个 16 位字。因此
`PolicyFor("hostlink")` 与 `PolicyFor("fins")` 返回**同一个策略**
（`WidthRegisters` + `RegisterAreas: finsAreas` + `AllowRegister`），而不是
各写一份：

| 写法 | 解析结果 | 说明 |
|---|---|---|
| `dm:0` | `dm:0:1` | uint16/int16 → 1 字 |
| `dm:100`（float32） | `dm:100:2` | 32 位 → 2 字 |
| `cio:0.5` | `cio:0.5:1` | bool + 位号（原生位访问） |
| `em:5` | `em:5:1` | 当前 EM 区 |
| `em12:9` | `em12:9:1` | 按 bank 编号的 EM 区（0..12） |
| `d:0` | `dm:0:1` | 单字母别名 D/W/H/A 归一化成规范名 |
| `dm:0`（bool） | 导入期拒绝 | 寄存器协议的习惯：bool 必须给位号 |

`finsAreas` 是可穷举的区名白名单：CIO/WR/HR/AR 四个继电器区、数据存储器
DM、当前 EM 区 `em`、以及 13 个 bank（`em0`..`em12`）。写错的区名（`dmm`、
`em13`）在 `uic import` 阶段就被拒，而不是等网关起来后每周期返回
`address_error`。别名 D/W/H/A 在 `splitRegisterPrefix` 里归一化，所以点表里
写 `d:0` 与 `dm:0` 产出同一个地址。

**为什么共用一个策略是刻意的**：Host Link 与 FINS/TCP 在应用层是同一套命令，
差别只在信封。把地址策略分成两份，迟早会漂移——某天 `fins` 支持了新区、
`hostlink` 没跟上，同一份点表换个 `protocol` 就行为不一致。共用一个
`PolicyFor("fins", "hostlink")` 分支，这个漂移就不可能发生。

#### 3.10.19.12 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/hostlink` | `Build`/`ParseRequest`/`ParseResponse` 的黄金字节逐字节钉住；FCS 覆盖 `@` 且不含 `*`/CR；收发两条独立布局（请求无结束码、应答有结束码）；等待时间一位十六进制（0..F 越界拒绝）；单元号两位十进制（0..31）；`FromFINS` 丢弃六个路由字段且 ICF 显式 0x00；`Response.FINS()` 还原标准 FINS 帧；`FrameLen`/`Reader` 的分帧、噪声丢弃与 `*`+CR 定界；结束码 `0x13/0x14/0x15/0x18` 标 `Format()`、`0x03/0x23` 归 permission；`FCSMismatch`/`UnitMismatch` 的 `Comm()` 标记 |
| `driver/hostlink` | 纯函数：`hostLinkLimits()` 四个上限（269/267/538/534）、`serialDefaults`（仅全空时补 9600-7-E-2，部分配置保留通用缺省 8/N/1）、`unitOf`（0..31、负数与越界拒绝）、`parseOptions`（`gap`/`wait` 边界）、`classifyRead`（超时文案带排查顺序、结束码分层、断链）；`RoundTrip` 对 FINS 结束码按语义分类（地址类 `0x1101..0x1104` → `ErrAddress`/不重试/不计熔断，链路类 `0x0202`/`0x0302` → 可重试，帧长错 `0x18` → 不重试）；共用 `finscore` 的写路径约束（越界/超长/短路由 `driver/finscore` 用例覆盖，同一份实现） |
| `driver/finscore` | `QualityOf`/`endCodeError` 的结束码→质量映射（地址类 `address_error`、链路类 `comm_error`、其余 `bad`）；`*TCPError` 的 `Comm()` 让封装层错误码得 `comm_error`；**部分失败语义（docs §3.3.1）**：成功点数按**样本**计数（span 成功但全员解码失败——如 EM 当前区 bool 缺位号——必须上抛错误，不能按 span 当成成功）、链路级失败（超时/断连）短路剩余 span 并把它们显式标坏、`ctx` 到期标坏剩余 span 后立即返回；**跨块读的部分失败**：前几块已解出好值时后一块失败（地址错或链路失败），只把没拿到值的点位标坏、好样本保留（字读/位读两条路径各有用例）；**写入逐项返回（docs §3.4）**：同批后一条失败不得回填成功项的 `Err`（回归用例 `TestWriteFailureKeepsEarlierSuccessesIntact`，把回填加回去必须变红）、写路径链路失败短路且保留前序成功；**写路径数值范围校验**：越界整数/带小数整数/float32 溢出在发报文前拒绝（70000→int16 不能变 4464）；**STRING/bytes 超长拒绝**而非静默截断，含「正好填满容量」的反向用例 |
| `sim/hostlink` | 真实 TCP 回环：16 位字往返（写 DM0 再读回）、位读写 + 邻位不变、float32 低字在前往返（DM2/DM3）、**EM 当前区位写走读改写往返（0xFFF7→0xFFFF→0xFFF7、只动 bit3）**、单元号不匹配完全沉默、FCS 错回 0x13、头码不对回 0x16、格式错回 0x14、读超 269 字由 finscore 切块 |
| `cmd/uic` | `hostLinkWriteRequest` 的写路径判定：给了 `-bit` 一律按位写（**不判区有无原生位码**，EM 区也走位写）、`-qty != 1` 的字写直接拒（避免补零清邻字 + 宽度变化切 DCBA 字节序）、字写 `Length=1`/`Bit=-1`、非法布尔量与非数字拒绝 |
| `pointtable` | `PolicyFor("hostlink")` == `PolicyFor("fins")`（`WidthRegisters` + `finsAreas`）；正向（字区/float32/位点/EM 当前区/EM bank/别名/大写区名）× 往返（`Canonical()` 能重新解析成同一地址）；反向（未知区名 `dmm`、`em13` 越界、bool 缺位号、位号 16、非 bool 带位号）；Modbus/S7/Mewtocol 拒绝 FINS 区名 |

覆盖率（`go test -count=1 -cover`）：

| 包 | 覆盖率 | 说明 |
|---|---|---|
| `codec/hostlink` | 73.1% | 未覆盖的主要是各畸形输入分支 |
| `driver/hostlink` | 76.7% | I/O 路径由 `sim/hostlink` 的真实 TCP 回环覆盖，按 `-coverpkg` 统计 |
| `sim/hostlink` | 85.5% | |
| `driver/finscore` | 82.4% | 与 FINS/TCP 共享，Host Link 复用同一覆盖 |

驱动包内覆盖率是结构性的：驱动的价值在「一帧一帧地走线」，而包内单测只能
碰到纯函数。本仓库选择「跨包统计 + 在文档里写清楚」，而不是为了数字造重复
用例。

#### 3.10.19.13 位置

| 文件 | 内容 |
|---|---|
| `internal/codec/hostlink/hostlink.go` | 帧常量（`MaxUnit`/`MaxWait`/`MinRequest`/`MinResponse`/`MaxCommandChars`/`MaxResponseChars`/`MaxTextChars`/`MaxDataChars`）、四个单次上限（`MaxReadWords` 等）、`FCS`、`Request`/`Response`/`Build`/`ParseRequest`/`ParseResponse`、`FromFINS`/`Reply`/`Frame.FINS()`/`WrapResponse`、`FrameLen`/`PeekUnit`/`WordsFor`、结束码分类（`EndCodeError`/`EndCodeText`/`Format`） |
| `internal/codec/hostlink/io.go` | 流式收帧 `Reader`（只认 `@` 与 `*`+CR、逐字节扫、单元号/SID 过滤、噪声容忍） |
| `internal/driver/finscore/finscore.go` | 与 FINS/TCP 共享的主站会话：区拆分、字/位读、EM 当前区读字取位、按 `Limits` 切块、结束码分类（Host Link 通过 `Limits` 与 `Transactor` 复用，不复制） |
| `internal/driver/hostlink/driver.go` | 主站：`Meta`（能力/配置字段/出厂串口参数文案）、`Open`（`unitOf`/`parseOptions`/`serialDefaults`）、`transactor`（`FromFINS` 套信封、`Build`、收帧、`MRC/SRC`/单元号校验、`hostLinkEndCodeError` 结束码按语义分类）、`classifyRead`（超时文案带排查顺序）、`hostLinkLimits` |
| `internal/sim/finsmem/finsmem.go` | 与 FINS/TCP 共享的从站内存表（字视图 + 位视图、EM bank、`Respond`） |
| `internal/sim/hostlink/server.go` | 内置从站（TCP 透传、逐字节扫帧、`*`+CR 定界、单元号过滤沉默、FCS 错 0x13、格式错 0x14、头码错 0x16） |
| `cmd/uic/hostlinkread.go` | 自检命令（probe / 读写 / limits / json）；写路径判定抽成纯函数 `hostLinkWriteRequest`（给 `-bit` 按位写、字写一次一个字）便于单测 |
| `examples/hostlink.json` | 6 点示例（DM 字、float32、CIO 可写位、可写计数） |
| `internal/pointtable/address.go` | `PolicyFor("fins", "hostlink")`、`finsAreas`、`finsAliases`、`splitRegisterPrefix` 的 FINS 区名与 EM bank 归一 |

### 3.10.20 KNXnet/IP（UDP 隧道 + 组地址读写 + DPT，端口 3671）

KNX 是楼宇自控里覆盖最广的一条总线（照明、遮阳、暖通、能耗计量都跑在同一条
双绞线上）。它对上位机暴露的接口不是「读一个地址拿到一个值」，而是网关上的
一条**隧道**：客户端先建连接拿到通道号，之后每帧带一个 8 位序号，网关确认之后
这帧才算发出去。因此这个驱动的难点不在字段编码，而在**隧道的生命周期**与
**类型的缺失**。

本仓库只做一种形态：**TUNNELING（UDP 单播隧道）+ 组地址读写**。不做 ROUTING
组播、不做设备管理连接（属性/内存下载）、不做 KNX Secure、不做 raw/busmon。

#### 3.10.20.1 为什么 DPT 必须显式声明，而且必须是地址的一部分

这是整个协议最容易做错、也最容易被「聪明」地做错的地方。

组地址（`1/2/3`）在总线上**不携带类型**。同一条 `GroupValueWrite` 只说明「把这
几个字节写进这个组地址」，字节怎么解释完全由工程的组地址表决定。所以：

| 做法 | 结果 |
|---|---|
| 由 `dataType: float32` 反推 DPT | **得不到答案**：`float32` 既可能是 9.xxx（2 字节半精度）也可能是 14.xxx（4 字节单精度），编码方式完全不同 |
| 猜一个，读回来不对就报错 | **不会报错**：2 字节半精度被当 4 字节单精度解，得到 `3.7e-41` 这种「合法的 float32」 |
| 显式声明 `1/2/3@9.001` | 宽度、编码、取值范围、单位全部确定 |

第三行是本仓库的选择，而且做了三件事让它不可能被绕过：

1. **`pointtable.PolicyFor("knx")` 要求 DPT**：策略是
   `WidthNone + AllowSymbol + AllowDPT`。`WidthNone` 表示「宽度不由平台类型
   决定」（DPT 说了算），`AllowDPT` 表示符号地址**必须**带 `@<DPT>`。缺了它
   在 `uic import` 阶段就被拒，而不是等网关起来后每周期返回 `address_error`。
2. **`Address.DPT` 是模型字段**，不是标签、不是 `labels` 里的自由文本。它参与
   `Validate()`（`^[0-9]+(\.[0-9]+)?$`）、参与 `Canonical()`、参与重复地址检查。
3. **`Address.Canonical()` 把 `@DPT` 拼回去**。这一点看似细节，实际是必须的：
   如果规范化时丢掉 DPT，`1/2/3` 与 `1/2/3@9.001` 会被当成两条不同地址，
   重复地址检查会漏判；更糟的是「同一点位改 DPT」不会被视为冲突，而是被当成
   两个点位并存，两份采样互相覆盖。

驱动的 `Open` 还把 DPT 与 `dataType` 做一次静态匹配（`DPT.Accepts`）：`9.001`
配 `bool` 直接拒。把 21.5 塞进布尔不会报错，只会变成 `true`——一个看起来完全
正常、实际毫无意义的读数。

#### 3.10.20.2 DPT 表是切片，不是 map

`9.001` 与 `9.1` 是**同一个类型**。用 `map[string]DPT` 做查表时，键必须先把
`9.1` 补零成 `9.001` 才能命中，而「补零」这件事要在三个地方各写一遍（解析、
规范化、错误提示里的「支持列表」），迟早漏一处。

因此 `ParseDPT` 用切片 + 两段数字匹配（`main` / `sub`）：

```go
type DPT struct {
    Main, Sub uint16   // 9.001 → Main=9, Sub=1
    Bits      int      // 线宽：1 / 8 / 16 / 32
    Kind      DPTKind  // Bool / Unsigned / Signed / Float16 / Float32 / Scaled / …
    Name      string
    Unit      string
    Min, Max  float64
}
```

`String()` 输出规范化形式（`9.1` → `9.001`），错误提示里打印的是切片遍历结果
——「支持哪些 DPT」因此只有一个事实来源。

支持的 DPT 只覆盖现场最常用的那批（1.001/1.002/1.003/1.009、5.001/5.003/5.004/
5.010、6.001/6.010、7.001/7.012、8.001/8.010、9.001..9.028、12.001、13.001/
13.010/13.013、14.001/14.007/14.019/14.027/14.031/14.033/14.056、20.102）。不做
「任意 DPT 自动推导」：DPT 编号有几百个，猜错的代价是一条「合法但错误」的读数，
比明确报「不支持 99.9」危险得多。

#### 3.10.20.3 1 位 DPT 的值在 APCI 字节里，不在数据域

这是 cEMI 最反直觉的一处，也是本仓库在这次实现里**真的踩到并修掉**的一个 bug。

一条 2 字节 DPT 的 `GroupValueWrite`，TPDU 是「TPCI 字节 + APCI 低字节 + 数据」：

```
TPCI(00)  APCI 低(80)  数据(0C 33)     ← 9.001 = 21.5
```

而 1 位 DPT **没有数据域**，值放在 APCI 低字节的 bit0：

```
TPCI(00)  APCI 低(80 | 01)             ← 1.001 = 开
```

于是解析时 `APCI` 会算成 `0x0081` 而不是 `0x0080`，调用方按服务码
`switch m.APCI` 直接落到 `default` 分支——报文被**静默丢弃**。现场看到的症状是
「写返回成功，但设备没动」，而日志里什么都没有。修复是解析时把这一位摘掉：

```go
out.APCI = uint16(tpci&0x03)<<8 | uint16(tpdu[1])
if length == 1 {
    out.Bit = tpdu[1]&0x01 != 0
    out.APCI &^= 0x01          // 关键：值位不能泄漏进服务码
}
```

对应的编码侧细节：**长度字段是 `len(apdu)-1`**，即「APCI 低字节 + 数据」，
**不含 TPCI 字节本身、也不含 APCI 的高 2 位**（那两位在 TPCI 字节里）。把它当成
「整个 APDU 的字节数」会让网关把最后一个数据字节当成下一条报文的一部分，症状
同样是「写成功了，但值不对」。

回归测试 `TestParseLDataNormalizesOneBitAPCI` 同时钉住三件事：`00` 与 `01` 都
归一化成 `APCIGroupValueWrite`、`Bit` 正确、以及「2 字节帧的数据末位是 `0x01`
时不会被误判成 1 位值」。

#### 3.10.20.4 序号：确认之后才加一，重传用同一个号

KNX 隧道的序号是 **8 位、每通道一个、双向独立**。规则只有三条，但三条都必须对：

| 情况 | 客户端行为 | 为什么 |
|---|---|---|
| 收到 `TUNNELLING_ACK` 且状态为 0 | 序号 +1 | 序号推进的唯一依据是「网关确认了」，不是「我发出去了」 |
| `ackTimeout` 内没收到 ACK | **用同一个序号重发** | 换号会被网关当成新帧，同一次写被执行两遍 |
| 收到「序号错乱」状态 | 丢掉整条隧道、重新 CONNECT | 通道号与序号是一体的，单独「调序号」在真网关上没有对应服务 |

第三条是刻意的：序号错乱意味着从站期望的号与客户端认为的号不一致，而这个
差值**无法可靠推断**（可能是网关重启、可能是另一条隧道占用了同一个通道号、
也可能是中途有帧被网关内部丢弃）。任何「+1 试试 / 跳到期望值试试」的做法都在
猜，而猜错的代价是「写成功但设备没动」或「同一个写执行两遍」。重建隧道的代价
只有一次 CONNECT 往返，而且它把状态**确定性地**归零。

重传次数由 `labels.maxRetries` 控制（默认 2，范围 0..5）。超过上限后返回
`ErrTimeout`，可重试。

#### 3.10.20.5 整条会话一把锁，不做并发

KNX 隧道只有一个通道号、一个 8 位序号，底层总线 9600 波特。并发发帧不但没有
收益，还会让序号竞争，把「重传」与「新帧」搅在一起。因此驱动的整条会话用一把
`sync.Mutex` 串行化，`Read` 的多点位也是逐点串行。

代价是「100 点一轮」在 KNX 上确实慢（0.5s × 100 = 50s）。但这个代价是协议本身
的，不是驱动的：总线就是这个速度，批量读也没有标准手段（`GroupValueRead` 只能
点名一个组地址）。把并发加进来只会把「慢」变成「慢 + 偶发错乱」。

#### 3.10.20.6 `Open` 只 dial，不建隧道

`Open` 完成的事情只有一件：把 UDP 的 `net.Dial` 做完（顺带补默认端口 3671）。
CONNECT 延迟到首次 `Probe`/`Read`/`Write`。

理由不是「懒加载省时间」，而是**错误分类**：

| 时机 | 失败意味着 | 应该报什么 |
|---|---|---|
| `Open` 失败 | 地址写错、端口不通、协议名不支持 | 配置错误，`ErrConfig`/`ErrUnsupported`，不该重试 |
| `Probe`/首次读写失败 | 网关隧道槽位满、网关重启中、网关拒绝连接 | 拓扑/临时问题，`ErrDisconnected`，**必须可重试** |

如果 CONNECT 放在 `Open` 里，「网关隧道槽位满」（一个完全正常、几分钟后会自己
恢复的状态）就会表现成**驱动加载失败**，进而让整台设备在配置里被判成坏设备。
内置网关的 `MaxTunnels` 测试正是钉住这一点：第二条隧道被拒时，
`KindOf(err) == ErrDisconnected` 且 `IsRetryable(err) == true`。

`Probe` 的定义是「建隧道 + CONNECTIONSTATE 往返」，**不读任何组地址**。排障时
经常需要把「网关连得上吗」与「这个工程的组地址配得对吗」拆开看：前者失败的
处理动作在网关侧（槽位、网段、防火墙），后者在点表侧。

#### 3.10.20.7 写：`L_Data.con` 的 error 位是总线级故障，不是「有没有人在听」

这是本节最需要强调的语义区分，也直接决定了驱动在写路径上的行为。

`L_Data.con` 是**链路层确认**：它说「这一帧有没有成功地发到总线上」。它的
error 位表示总线级问题——总线忙、冲突、链路发送失败。它**不表示**「这个组地址
上有没有设备在听」：

> 组地址是广播语义。写一个没人订阅的组地址，在链路层是**成功**的：帧确实上了
> 总线，没有任何设备需要为它改变状态。

把这两件事混在一起，会让「写了但没生效」与「网络断了」变成同一个错误——而
它们的处理方式完全不同（前者是点表问题、后者是网络问题）。

驱动的写路径因此是：

1. 发 `L_Data.req`，等 `TUNNELLING_ACK`（必需）。
2. 在 `confirmWindow = 300ms` 的短窗口里等同一条报文的 `L_Data.con`。
   - **没收到** → 按成功。部分网关不给隧道客户端回 con，硬等会把一半的写判成失败。
   - 收到且 error 位**未**置位 → 成功。
   - 收到且 error 位**置位** → `ErrProtocol`（帧到网关但总线级发送失败）。
3. 写操作不做「写完回读」：那是调用方（调度器/上层）的决定，驱动不该偷偷多花
   一个事务。

内置网关因此默认把 con 报成正常；只有 `FailNextConfirms(n)` 显式注入时，才把
这一位置起来。两边的测试各钉一头：`TestWriteUnknownGroupAddressSucceeds`（写
未知地址成功）与 `TestWriteFailsWhenLinkLayerConfirmReportsAnError`（总线故障
失败）。

#### 3.10.20.8 内置网关的语义：只更新已有的对象

`internal/sim/knx` 的对象表**只更新已经存在的组地址**，不会给未知组地址凭空
造一个对象。这一点是刻意的：

如果模拟器「写什么就存什么」，那「写一个不存在的组地址」在测试里会变成一次
可见的成功，而这个成功在真机上不存在——它会把「点表里地址写错了」这个最常见
的故障从测试里抹掉。真实网关不存组对象，写报文只是被转发到总线，由「拥有该
组地址的执行器」接收。

同理，读一个未知组地址时模拟器**只回 ACK 与 `L_Data.con`、不回
`GroupValueResponse`**，让客户端正常超时。那个超时正是它应该报告给运维的事实。

从站还刻意复现四件真机会发生的事：

1. **序号严格校验**（三种情况分开：期望值 → 处理；上一条 → 只重发 ACK；其它 →
   回序号错乱状态）。宽松一点会让驱动里的重传路径永远测不到。
2. **8 位回绕**（255 → 0 是新帧，不是重传）——这条边界最容易在测试里写成
   「重传」，从而掩盖掉一个真实的重复执行风险。
3. **隧道槽位满时回 `StatusNoMoreConnections`**，而不是静默丢弃：静默会让客户端
   等满一个超时，还以为是网络问题。
4. **非隧道 CRI 明确拒绝**（`StatusConnectionType`）：设备管理连接本仓库不做，
   回一个明确的码比让客户端猜要好。

#### 3.10.20.9 源地址：三级优先

| 优先级 | 来源 | 说明 |
|---|---|---|
| 1 | `labels.sourceAddress` | 工程要求固定源地址（比如网关的白名单按源地址放行）时用 |
| 2 | `CONNECT_RESPONSE` 的 CRD | 网关分配的地址，最省事 |
| 3 | `0.0.0` | 交给网关替换。真实网关通常会把 0.0.0 换成自己的地址 |

`sourceAddress` 显式拒绝 `0.0.0`（等于「不指定」，写成配置只会误导）与非法
四段（`15.15`、`gateway`）。

#### 3.10.20.10 心跳与超时

`CONNECTIONSTATE_REQUEST` 默认 60s（限制 1s..60s）。网关侧的隧道超时通常是
120s，60s 留了一半余量。

`ackTimeout` 默认 1s（100ms..10s），且**必须 ≤ `device.timeout`**：心跳窗口比
设备超时还长时，一轮采集会被心跳拖长，表现为「设备响应变慢」而不是「心跳配错
了」。这条约束在 `Open` 时就检查，报错文案里直接点出 `device.timeout`。

读结果汇总按 §3.3.1：`L_Data.con` 超时与隧道断开是链路级（`driver.IsLinkFailure`），
立即返回并把剩余组地址标坏；组地址不存在、DPT 与点位类型不匹配是点级问题，只标
该点。只要还有一个组地址读成功，`Read` 就返回 `nil`——否则点表里一个写错的组地址
会让整台网关每周期重建隧道（重新申请通道号、重置 8 位序号）。

#### 3.10.20.11 自检与内置网关

`uic knxread` 不依赖点表，回答「这条隧道建得起来吗、这个组地址现在是什么值」：

```bash
uic knxread -addr 192.168.1.10:3671 -probe
uic knxread -addr 192.168.1.10:3671 -group 2/1/1 -dpt 9.001
uic knxread -addr 192.168.1.10:3671 -group 2/1/1 -dpt 9.001 -raw
uic knxread -addr 192.168.1.10:3671 -group 1/1/1@1.001 -write on
uic knxread -addr 192.168.1.10:3671 -group 3/1/1 -dpt 5.001 -write 75 -json
uic knxread -list
```

`-group` 支持把 DPT 内联写成 `组地址@DPT`，与点表走同一条解析路径——两处语法
漂移是这类 CLI 最常见的腐化方式。

`-probe` 不需要合法的组地址与 DPT：探测往往发生在还不知道工程组地址分配的时候。

`-raw` 会把解码后的值**重新编码**成线格式打印。它不是「保存收到的字节」：这条
命令只看到值，把值再编回去恰好验证了「编码-解码是自洽的」——两边不一致会立刻
在输出里看出来。

`uic sim -protocol knx -listen 127.0.0.1:13671` 起内置网关，默认对象表与
`examples/knx.json` 一一对应：

| 组地址 | DPT | 初值 | 说明 |
|---|---|---|---|
| `1/1/1` | 1.001 | 关 | 照明开关 |
| `1/1/2` | 1.001 | 开 | 第二路开关 |
| `2/1/1` | 9.001 | 21.5 | 室内温度 °C |
| `2/1/2` | 9.007 | 45 | 相对湿度 % |
| `3/1/1` | 5.001 | 50 | 调光/阀门位置 % |
| `4/1/1` | 14.056 | 1234.5 | 有功功率 W（每秒 +10） |
| `5/1/1` | 13.010 | 8888 | 有符号计数 |

对象表只有一份（`sim/knx.DefaultObjects()`）：安装初始值、`uic knxread -list`
打印清单、`uic sim -protocol knx` 启动横幅都从它读，避免「文档里写的地址」与
「实际配的地址」各说一套。

#### 3.10.20.12 点表这一层：`WidthNone` + `AllowDPT`

| 写法 | 解析结果 | 说明 |
|---|---|---|
| `1/2/3@9.001` | 符号 `1/2/3` + DPT `9.001` | 规范形式 |
| `1/2/3@9.1` | 规范化成 `1/2/3@9.001` | `9.1` 与 `9.001` 同一类型 |
| `1/2/3` | **导入期拒绝** | 缺 DPT，组地址不携带类型 |
| `50/1/1@9.001` | 拒绝 | 主组只有 0..31 |
| `1/9/1@9.001` | 拒绝 | 中间组只有 0..7 |
| `1/2/3@abc` | 拒绝 | DPT 语法 |
| `1/2/3@9.001` + `selector` | 拒绝 | 符号地址不接受别的字段 |

组地址自身的语法（主组 0..31 / 中间组 0..7 / 子组 0..255）由 `codec/knx` 报，
点表层不重复实现一份「差不多的」校验——两份校验迟早会不一致，而不一致的那一侧
通常是宽松的那一份。

#### 3.10.20.13 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/knx` | 组地址 4/4/8 与物理地址 4/4/8 的往返与越界；DPT 解析与规范化（`9.1` ≡ `9.001`）、`Accepts` 的位宽/类型校验、`Encode`/`Decode` 各 Kind（Bool 接受 on/off/1/0 且拒绝其它数值、Unsigned/Signed/Float16/Float32/Scaled）；cEMI 的黄金字节（1 位 2 字节 TPDU 长度 = 1、2 字节 = 3）；`ParseLData` 跳过附加信息段；**1 位 APCI 归一化回归**；`DecodeGroupValue` 拒绝位宽不符与 `GroupValueRead`；KNXnet/IP 信封往返、HPAI 编解码、全部服务报文的 Parse/Build 对称、`ParseDisconnectResponse` |
| `driver/knx` | `Meta`（能力/地址类型/`address.dpt` 必填）；`Open` 补默认端口、保留显式端口、拒绝非 UDP、九类非法 settings（心跳过长/为零/不可解析、ack 过短/超过设备超时、重传过多/不可解析、源地址非法/为 0）；缺 DPT 与 DPT-类型不符都在 `Open` 拒；`planPoint` 拒绝 selector/index/length/组地址语法错/DPT 语法错；`Probe` 建隧道且**不读组地址**；1 位/2 字节/4 字节/百分比读；未知组地址超时（`ErrTimeout` + `QualityTimeout`）；错误 DPT 读按 `ErrAddress`；首个链路错误后**中止剩余点位**（不逐个等超时）；**部分失败语义（好点 + 坏地址时 `err==nil`、全失败才返回错误）**；写开关并回读、写浮点、写未知地址成功、con 报错报 `ErrProtocol`；丢 2 个 ACK 后重传成功且序号仍同步；序号错乱后重建隧道并恢复；隧道被网关回收后心跳发现并重连；网关不回 con 时写仍成功；槽位满判可重试、释放后可连；`Close` 归还槽位；源地址三级优先 |
| `sim/knx` | 手写最小客户端（**不复用驱动**）：建连分配通道与 CRD、槽位满拒绝、非隧道 CRI 拒绝且不占槽位、DISCONNECT 释放槽位、未知通道拒绝、通道状态存活与回收；序号严格校验（拒绝后不推进期望值）、重传只重发 ACK 不重复执行、8 位回绕后「新帧 vs 重传」边界、未知通道拒绝、畸形数据报计数；读返回存储载荷（含 `L_Data.con` 正常）、未知组地址只有 ACK + con、写 1 位/2 字节落地、写未知地址被转发但不落地、`FailNextConfirms` 只影响一条、`DisableConfirm` 抑制 con；`DropNextACKs` 后重传、`FailNextSequence` 只失败一次；对象表覆盖与**宽度与 DPT 一致**（`4/1/1` 必须 4 字节）、`SetValue`/`SetNumber` 越界拒绝、`SetPayload` 拒绝非法组地址、每秒自增、`Close` 幂等 |
| `pointtable` | `PolicyFor("knx")` 是 `WidthNone + AllowSymbol + AllowDPT` 且不泄漏 DI/length 标志；正向（`@9.001`、`@9.1` 规范化）× 往返（`Canonical()` 能重新解析成同一地址且保留 DPT）；反向（缺 DPT、DPT 语法、主组/中间组越界、符号地址带 selector） |

覆盖率（`go test -count=1 -cover`）：

| 包 | 覆盖率 | 说明 |
|---|---|---|
| `codec/knx` | 77.5% | 未覆盖的主要是各畸形输入分支与少量 DPT 条目 |
| `driver/knx` | 67.7% | I/O 路径由 `sim/knx` 的真实 UDP 回环覆盖；包内单测只能碰到纯函数 |
| `sim/knx` | 85.2% | |

跨包统计（`-coverpkg`）下 `driver/knx` 的 I/O 路径覆盖率显著更高。这与 Host Link
一节的处理一致：驱动的价值在「一帧一帧地走线」，本仓库选择「跨包统计 + 在文档里
写清楚」，而不是为了数字造重复用例。

#### 3.10.20.14 位置

| 文件 | 内容 |
|---|---|
| `internal/codec/knx/group.go` | 组地址（4/4/8）与物理地址（4/4/8）的解析、`String()`、`Main/Middle/Sub`、`NewGroupAddress` |
| `internal/codec/knx/dpt.go` | DPT 切片表、`ParseDPT`、`String`（规范化）、`Accepts`、`Encode`/`Decode`、`Payload{Bit,Data}`、`Kind*`、`DPTNames` |
| `internal/codec/knx/cemi.go` | `BuildLData`（TPDU 长度不含 TPCI 与 APCI 高位）、`BuildLDataCon`、`ParseLData`（1 位 APCI 归一化）、`DecodeGroupValue`、`LData.ConfirmationError()`、APCI 常量 |
| `internal/codec/knx/netip.go` | KNXnet/IP 信封（`BuildFrame`/`ParseFrame`）、HPAI（`NewHPAI`/`AppendHPAI`/`ParseHPAI`）、全部服务报文（CONNECT / CONNECTIONSTATE / DISCONNECT / TUNNELLING / ACK）、状态码、`DefaultPort`/`HostPort`、`ParseDisconnectResponse` |
| `internal/driver/knx/driver.go` | 主站：`Dialer`/`New`/`NewWithDialer`、`Meta`、`parseSettings`（心跳/ackTimeout/maxRetries/sourceAddress 边界）、`buildPlans`/`planPoint`（DPT 必填且与 dataType 匹配）、`Probe`/`Close`、`ensure`/`redial`/`localHPAI`/`connect`/`establish`/`connectStatusError`/`checkState`/`disconnect`/`dropTunnel`/`dropLink`、`writeFrame`/`readFrame`/`acceptInbound`/`answerStateRequest`/`answerServerDisconnect`、`exchange`/`sendLast`/`awaitACK`/`ackError`/`awaitValue`、`Read`/`readOne`/`Write`/`writeOne` |
| `internal/sim/knx/server.go` | 内置网关：`Options{MaxTunnels,DisableConfirm,DisableAutoIncrement}`、`NewServer`/`NewServerWithOptions`/`Addr`/`Close`、观测（`Connects`/`Refusals`/`Tunnels`/`Reads`/`Writes`/`Retransmits`/`SeqErrors`/`BadPackets`/`Requests`）、激励（`DropNextACKs`/`FailNextSequence`/`FailNextConfirms`/`ExpireTunnels`）、对象表（`SetPayload`/`Payload`/`SetValue`/`SetBool`/`SetNumber`/`Objects`）、`DefaultObjects`/`DefaultObject` |
| `internal/model/model.go` | `Address.DPT`、`dptRe`、`Canonical()` 的 `@DPT` 拼接 |
| `internal/pointtable/address.go` | `AllowDPT`、`PolicyFor("knx")`（`WidthNone + AllowSymbol + AllowDPT`）、`splitDPT` |
| `cmd/uic/knxread.go` | 自检命令（`-addr`/`-group`/`-dpt`/`-type`/`-timeout`/`-heartbeat`/`-retries`/`-source`/`-probe`/`-write`/`-list`/`-json`/`-raw`，`组地址@DPT` 内联写法） |
| `cmd/uic/main.go` | `knxread` 分派、`sim -protocol knx`、`simKNX` 启动横幅 |
| `examples/knx.json` | 7 点示例（开关 ×2、2 字节浮点 ×2、1 字节百分比、4 字节浮点、2 字节有符号计数） |
| `internal/runtime/runtime.go` | `RegisterBuiltins()` 注册 `knxdrv.New()` |

### 3.10.21 IEC 61850（MMS over TCP：五段封套 + 对象引用读写，端口 102）

IEC 61850 是变电站自动化的本体标准，它的价值不在「又一个协议」，而在**唯一一种能
表达「一台装置上有哪几个保护/测量」的层次寻址**：逻辑设备（LD）→ 逻辑节点（LN）→
数据对象（DO）→ 数据属性（DA）。上位机与 IED 之间的站控层通信在标准里指定为 MMS
（制造报文规范）承载在 ISO-on-TCP 上，于是从一条网线到一次「读 `GGIO1.ST.Ind1.stVal`
是什么值」之间，隔着**五层封套**：

```
TPKT (RFC1006)  →  COTP (ISO 8073)  →  会话层 (ISO 8327)  →  表示层 (ISO 8823)  →  ACSE (ISO 8649)  →  MMS
```

本仓库只做这一条路径上的采集子集：**ACSE 关联 + MMS 初始化 + 读/写变量 + 模型浏览
（getNameList）**。不做 GOOSE/SV、报告控制块（BRCB/URCB）、文件传输、定值组、数据集
读写、TLS。这些不是「懒得写」：GOOSE/SV 是二层/三层组播、有自己的生命周期与采样率；
报告控制块是「服务端主动推」，与本平台调度式采集（拉）是两种语义；文件传输与定值组
各有独立的权限模型。混进一个采集驱动里，超时、重连、死区三套逻辑会立刻互相干扰。

#### 3.10.21.1 五段封套：谁的帧带谁的壳

这是整节最容易做错、也最难从现象反推的地方。规则只有两条，但两条都必须精确：

| 帧 | 自带 TPKT？ | 发送前需要做什么 |
|---|---|---|
| COTP 连接请求 `CR` / 连接确认 `CC` | ✅ 是 | 直接写 |
| COTP 断开请求 `DR` | ✅ 是 | 直接写 |
| 会话 `CN`（连接请求）/ `AC`（接受） | ❌ 否 | 套表示层 → 再 `BuildCOTPData`（补 TPKT + COTP DT） |
| 会话 `DT`（数据）与所有 MMS 请求/响应 | ❌ 否 | `WrapData`（会话 DT + 表示层）→ `BuildCOTPData` → 写 |
| 会话 `FIN`（有序释放）/ `AB`（中止） | ❌ 否 | 同 `DT` |

封装顺序看起来「对称」，实际不对称：COTP 层在**连接阶段**自己产生完整的 TPKT 帧，
在**数据阶段**只负责「在用户数据前面加 3 字节 COTP 头 + 4 字节 TPKT 头」。所以驱动
里必须同时存在两个入口：`sendRaw`（发整帧，给 `CR`/`CC`/`DR` 用）与 `sendSession`
（先 `BuildCOTPData` 再发，给会话层及以上用）。

`readFrame` 反过来：它读一个**带 TPKT 头的整帧**，再按 COTP 类型（`CC` / `DT` / `DR`）
决定交给谁解析。把 TPKT 头当成「传输层的私事」在解析侧剥掉，会让 COTP 的解析函数
拿不到它需要的长度字段。

这条不对称有个副作用值得写进测试：**少套一层或多套一层，对端只会断链**。没有
「你的封套编号错了」这种诊断，只有一条连接被对端关闭。所以五段的组帧都用黄金字节钉住
（`TestConnectPDUChain` / `TestSessionDataHeader` / `TestWrapUnwrapData`），一次失败
就能定位到层。

#### 3.10.21.2 会话层 LI 与 BER 长度是两套编码

这是本仓库在这次实现里花时间最多的一个点，也是最容易「看起来对」的错误。

会话层每个 SPDU 都以一个**长度指示（LI）**开头，它的编码规则是标准自己定的，**不是
BER**：

| 取值范围 | 编码 |
|---|---|
| 0 .. 254 | 单字节，就是长度 |
| 255 及以上 | `0xFF` + **恰好两字节**大端长度 |

而 BER 的长形式是「`0x81`/`0x82`/… 后面跟 N 个字节」，N 由首字节低 7 位决定。两者
在短长度上**长得一模一样**（都是一个字节，值就是长度 < 128），所以在小型测试帧上
永远发现不了差异；一旦某个 MMS 响应超过 254 字节（读一个结构体、翻一页模型、写一组
数据），用 BER 解析器去读 LI 就会从错误的偏移继续——症状是「小响应一直正常，一读大
结构就断链」。

因此 `ParseSPDU` 里有一条独立的、**只服务会话层**的 LI 解析：

```go
// 伪代码，语义即实现
switch {
case head[0] < 0xFF:
    li = int(head[0])
    off = 1
case head[0] == 0xFF:
    li = int(binary.BigEndian.Uint16(head[1:3]))
    off = 3
default:
    return SPDU{}, fmt.Errorf("session: invalid length indicator 0x%02X", head[0])
}
```

`TestSessionConnectUsesExtendedUserData` 用一条超过 254 字节的连接帧钉住 `0xFF` 分支
——短帧测试永远走不到那条分支。这与 CoAP 的 delta/length 扩展、Modbus RTU 的字符间
静默属于同一类问题：**协议自己的长度编码看起来像通用编码，但不是**。

#### 3.10.21.3 FC 是名字的一段，不是元数据

IEC 61850 的对象引用写作 `LD/LN.FC.DO[.DA]`（示例：`simpleIO/GGIO1.MX.PhV.mag.f`），
在 MMS 层被翻译成「域 + 项名」，项名用 `$` 分隔：

```
simpleIO/GGIO1.MX.PhV.mag.f   →   domain=simpleIO, item=GGIO1$MX$PhV$mag$f
```

关键在于 **FC（功能约束）是项名的一段**，不是可以被省略、被推导的元数据：
`GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal` 是两个**不同的对象**，只有一个存在。
把 FC 当成「读的时候顺手补上」的东西，会得到稳定的 `object-non-existent`——而现场
常见的读法是「这个点位永远坏，别的都好」，看不出任何协议层的异常。

驱动因此规定：

1. 引用里显式写了 FC 就用它；
2. 没写则由 `labels.defaultFC` 补（`ST`/`MX`/`CO`/`SP`/`CF`/`DC`/`SG`/`SE`）；
3. 两者都没有 → `Open` 阶段按地址错拒绝，不进采集循环。

**绝不从数据属性名反推 FC。** 反推需要完整的 SCL 模型（`stVal` 在 `ST`/`CO`/`SP` 里
都可能出现，取决于数据对象的 CDC），而驱动手上只有一份点表。

FC 白名单严格限定为标准的八个功能约束。这一点看起来苛刻，实际是保护：`MOD`（模式）
不是功能约束而是数据对象的一部分，把它当 FC 写进引用，得到的是一条**语法合法但语义
错位**的项名，读回来是 `object-non-existent` 或者更糟——一个同名但不同语义的对象。
`IsValidFC` 直接拒绝，配置期就暴露。

#### 3.10.21.4 类型在装置侧，所以点表不写 width

这与 KNX 恰好相反，值得对照着读：

| | KNX | IEC 61850 |
|---|---|---|
| 类型在哪 | **不在总线上**，只有工程组地址表知道 | **在装置侧**，由 IED 的 MMS 模型给出 |
| 点表要求 | DPT **必填**（`1/2/3@9.001`） | **不写** length/DPT，只写对象引用 |
| 地址策略 | `WidthNone + AllowSymbol + AllowDPT` | `WidthNone + AllowSymbol` |
| 猜错的后果 | 得到「合法的错值」（21.5 → `3.7e-41`） | 得到「按错误类型解释的值」（浮点按 int32 入账） |

因为宽度由装置决定，`dataType` 在这里**只声明平台侧怎么存**，不参与线的解释。
`uic iec61850read -raw` 打印的是**装置回的类型**（`bool` / `int64` / `float64`），
而不是 `-type`：这正是自检最想看到的信息，也是唯一能验证「点表里的类型标注是否
与装置一致」的手段。

`PolicyFor("iec61850")` 是 `WidthNone + AllowSymbol`：`length` 在这条链路上没有意义
（写上去会被 `WidthNone` 拒绝），`dpt`/`selector`/`bit` 等其它协议的字段同样不接受。
把「反正 Modbus 都能写 length」的习惯带过来，会在导入阶段被拒——这比让点表带着一个
被静默忽略的字段运行要好得多。

#### 3.10.21.5 读写请求体：`listOfVariable` 与 `listOfData` 都是 `[0]`

MMS 的读请求是「给我这些变量的值」，写请求是「把这些值写进这些变量」：

```
ReadRequest  ::= SEQUENCE { listOfVariable  [0]  SEQUENCE OF VariableAccessSpecification }
WriteRequest ::= SEQUENCE { listOfVariable  [0]  ..., listOfData [0] SEQUENCE OF Data }
```

**两个字段的上下文标签都是 `[0]`，都编码成 `0xA0`。** 没有任何标签能把「变量列表」
和「数据列表」区分开，唯一的判据是**出现顺序**：第一个 `0xA0` 是变量名列表，第二个
是数据列表。写成「按标签找 `[0]`」的解析器会在写请求上把数据当成变量名，得到一个
语法合法而语义完全错位的请求——服务端通常回 `object-non-existent`，看起来像点表
错了。

同样的坑在响应侧：读响应里每个结果要么是 `[1]` 数据、要么是 `[2]` 数据访问错误，
按顺序一一对应请求里的变量；返回的结果条数与请求不等时，驱动必须拒绝而不是「尽量
配对」——错位配对会把 A 的错误报成 B 的错误，运维照着错误信息改点表就会改错点。

#### 3.10.21.6 invokeID：严格配对，迟到响应一律丢弃

MMS 是请求/应答协议，但它允许**一条关联上同时有多个在途请求**，靠 32 位 `invokeID`
把响应分回请求。因此驱动必须：

1. 每次请求前 `c.invokeID++`，把新值传给 Build 函数（**由调用方分配**，编解码层
   不自己维护计数器——否则「谁在推进」这件事会散在两个包里）；
2. 收到响应后校验 `invokeID` 与当前请求一致；
3. **不一致就丢弃并继续读下一帧**，不把它当作当前请求的答案。

第 3 条是核心。不校验 invokeID 的实现在「一切正常」时完全正确，只在出过一次超时
之后出错：那次的响应迟到到达，恰好被下一次请求读走，于是**A 的值被记到 B 头上**。
症状是「偶尔有个点位显示别的点位的值」，而且刷新一次可能就恢复正常——这是最难查的
一类缺陷。所以「迟到响应」必须有专门的测试：先发一次会超时的请求，再发一次正常请求，
断言第二次拿到的是自己的值。

串行化策略是「整条关联一把 `sync.Mutex`」：MMS 请求/应答在一条 TCP 上没有并发收益，
只有乱序风险。`Read` 的多点位、`Write` 的多请求都在这把锁内逐项完成——代价是一轮
采集等于点位数 × 单次事务，但这个代价可以解释、可以度量；引入并发则会把「慢」变成
「慢 + 偶发错位」。

#### 3.10.21.7 单项数据访问错误必须逐条分类

MMS 的读/写响应是「逐项结果」：一批 5 个变量里可以 3 个成功、2 个报错，**不是整批
失败**。因此错误分类发生在**单项**上，而不是整次 `Read` 上：

| 数据访问错误码 | 含义 | 驱动分类 | 可重试 |
|---|---|---|---|
| 10 `object-non-existent` | 对象不存在 | `address` | ❌ |
| 4 `object-undefined` | 对象未定义 | `address` | ❌ |
| 5 `invalid-address` | 地址无效 | `address` | ❌ |
| 6 `type-unsupported` | 类型不支持 | `address` | ❌ |
| 3 `access-denied` | 访问被拒 | `permission` | ❌ |
| 2 `temporarily-unavailable` | 暂时不可用 | `protocol` | ✅ |
| 1 `hardware-fault` | 硬件故障 | `protocol` | ✅ |
| 其余 | — | `protocol` | ❌ |

这样分类的理由是**处理动作不同**：`address` 类错误不计入熔断（改点表就好，重试
一万次也一样），`permission` 要改权限或换账号，只有 `2`/`1` 值得让调度器退避重试。
把「点位配错了」与「装置在重启」混成一类，运维只能两边都查。

链路级失败（超时、断链）则**中止本轮剩余点位**（判据 `driver.IsLinkFailure`）：后面的
点不可能成功，逐个等超时只会把一轮采集拖成 N 倍时长。地址类错误不影响其它点，继续
读下一个。汇总按 §3.3.1：只要还有一个点位成功，`Read` 就返回 `nil`；全失败才上抛第一
个错误——否则点表里一个拼错的对象引用会让整台 IED 每周期丢弃 MMS 关联重连。

一处实现细节：错误消息里已经带上了错误码与对象引用（`mms data access error
object-non-existent (10) on simpleIO/GGIO1.ST.Ghost.stVal`），所以构造 `driver.E` 时
**cause 必须留空**。`driver.E` 会把 cause 的文本再拼到消息尾部，非 nil 就会得到
「... on X: ... on X」这种同一句话重复两遍的文案——这是本仓库在端到端手测里真的
看到并修掉的一个缺陷，也说明自检命令的输出本身就是被测物的一部分。

#### 3.10.21.8 浏览：能力位 unsupported，但提供包级入口

`getNameList` 是 MMS 的标准服务，能枚举一个域（逻辑设备）下的全部命名变量。但驱动把
`Browse` 能力位声明为 **`unsupported`**，理由与 SECS、ADS 等驱动一致：

> 能力位描述的是**采集路径**。整棵模型枚举是运维动作（一次性、可能需要几十页、结果
> 通常用来生成点表），与「每个周期读 N 个点位」是两种东西。把它注册成 `Browse`
> 能力，只会让采集侧多一条难以解释的慢路径。

所以实现分两层：

| 层 | 入口 | 用途 |
|---|---|---|
| 包级 | `driver.Browse(ctx, cfg, domain)` | 自检/建模：独立建一条关联，枚举完即关 |
| 连接级 | `(*conn).Browse(ctx, domain)` | 已有关联上的枚举，供包级入口转调 |
| 内部 | `(*conn).listNames(ctx, domain)` | 逐页翻，处理 `moreFollows` 与游标 |

分页语义：MMS 没有偏移量，只有**字典序游标** `continueAfter`。驱动把上一页的最后一项
作为下一页的游标，直到对端说 `moreFollows=0`。这里有两个必须处理的边界：

- **对端说「还有」却回了空页**：游标无从推进，再问只会空转 → 直接返回已收内容；
- **上限 256 页**：防「永远说还有、游标却不前进」的畸形对端把自检命令变成死循环。
  超过上限报 `protocol`，明确指出「getNameList did not terminate」。

独立建链（而不是复用采集连接）是刻意的：枚举期间会长时间占用这条关联，塞进采集的
那把锁里会让一轮采集莫名变长；独立关联也让 `invokeID` 序列互不干扰。

#### 3.10.21.9 `Open` 只 dial，关联延后

与 KNX、S7 驱动同样的取舍：

| 时机 | 失败意味着 | 分类 |
|---|---|---|
| `Open` 失败 | 地址/端口/协议名写错 | 配置错误，不可重试 |
| 首次 `Probe`/`Read`/`Write` 失败 | IED 正在重启、关联数已满、MMS 服务未就绪 | 临时问题，**必须可重试** |

把五段握手放进 `Open` 会把「装置重启中」表现成「驱动加载失败」，进而让整台设备在
配置里被判成坏设备——而它三十秒后就会自己好。

`Probe` 的定义是「建立关联 +（可选）读一次探针对象」。探针对象默认为
`LLN0$ST$Health`（所有 IED 都有的通用节点健康指示，且 `ST` 是标准 FC）。**读探针
失败时降级为「仅关联成功」而不是判离线**：对象名拼错不等于链路坏，把标签笔误判成
装置离线，会把人引向完全错误的方向。探针对象留空则 Probe 只建立关联——很多场景下
「TCP 通、MMS 在应答」已经足够。

关联建立失败时 `assocCount` 不计入成功关联（内置 IED 的观测计数钉住这一点），并且
错误分类为 `protocol` 且**不可重试**：ACSE 层面的拒绝（应用上下文不匹配、认证失败、
资源不足）重试同一个配置不会变好。

#### 3.10.21.10 内置 IED 的语义

`internal/sim/iec61850` 复用 `codec/iec61850` 的编解码函数——**主站与从站各写一套**
是这类多层封套协议最容易出的问题：TPKT/COTP/会话/表示/ACSE/MMS 的长度与上下文编号
在两侧各写一遍，然后对不上。共用之后，「从站能解析主站的帧」这件事几乎是自动成立的，
剩下的测试价值就集中在**语义**上。

它刻意复现五件真机会发生的事：

1. **未知对象回 `object-non-existent`，不静默成功。** 如果模拟器「读什么给什么」，
   「点表里对象引用写错」这个最常见的故障就永远测不到。
2. **写只落在已存在的对象上。** 给未知对象凭空记账，会让一次错误的写在测试里表现为
   成功——而真 IED 会回 `object-non-existent`。
3. **`getNameList` 只回项名，不带宽前缀。** 变量树内部的键是 `simpleIO$GGIO1$ST$…`
   （域 + 项，扁平索引），但 MMS 里 **identifier 在域内唯一、域由 `objectScope` 单独
   携带**。把整键直接回出去，客户端会以为项名里还含域，拼一遍就变成
   `simpleIO/simpleIO.GGIO1.ST.Ind1.stVal`——本仓库在端到端手测里真的看到过这个输出。
4. **关联可被拒。** `RejectAssociation(true)` 让从站回一个 ACSE 永久拒绝，验证主站
   把「关联被拒」与「读超时」分成两类事故（前者重试无用、后者可能只是装置忙），
   同时钉住「拒绝的关联不计入成功关联数」。
5. **读超时可注入且不拆连接。** `DropReads(true)` 让从站刻意不回读响应，客户端只能
   靠自己的 deadline 超时；恢复投递后同一条连接还能继续读——真 IED 忙一阵之后就是
   这样，不需要重连。

还实现了有序释放（会话 `FIN` → `RLRE`）：不处理 `FIN` 会让客户端在关闭时白等一个
超时，长期反复连接/断开的运维工具会因此留下一堆半开连接。

#### 3.10.21.11 自检与示例

```bash
uic iec61850read -addr 192.168.1.10:102 -probe
uic iec61850read -addr 192.168.1.10:102 -object simpleIO/GGIO1.ST.Ind1.stVal -type bool -raw
uic iec61850read -addr 192.168.1.10:102 -object simpleIO/GGIO1.MX.PhV.mag.f -type float32
uic iec61850read -addr 192.168.1.10:102 \
    -object simpleIO/GGIO1.ST.Ind1.stVal,simpleIO/GGIO1.MX.PhV.mag.f -raw
uic iec61850read -addr 192.168.1.10:102 -object simpleIO/GGIO1.CO.SPCs.stVal -type bool -write true
uic iec61850read -addr 192.168.1.10:102 -browse -domain simpleIO
uic iec61850read -list
```

- `-object` 支持逗号分隔多对象：一次命令看「同一台装置上几个点位分别是什么」，
  每个点位独立报错误，不会因为一个坏点让整条命令失败（退出码仍反映是否有失败）。
- `-raw` 在读取时打印**装置的 MMS 项名**（`simpleIO$GGIO1$MX$PhV$mag$f`）与装置回的
  类型；在浏览时打印项名而不是渲染后的对象引用。它是排障时唯一能把「点表里写的」
  与「线上发的」对上的输出。
- `-fc` 提供默认 FC，与点表走同一条解析路径（`ParseObjectRef`），两处语法漂移是这类
  CLI 最常见的腐化方式。
- `-list` 不需要任何对端：直接打印内置 IED 的对象表，让人一眼看到「自检环境里有什么
  可读」，避免对着一个空的 `-browse` 猜。

`uic sim -protocol iec61850 -listen 127.0.0.1:10202` 起内置 IED，默认对象表与
`examples/iec61850.json` 一一对应：

| 对象引用 | MMS 项名 | 类型 | 初值 |
|---|---|---|---|
| `simpleIO/LLN0.ST.Health` | `simpleIO$LLN0$ST$Health` | integer | 1 |
| `simpleIO/GGIO1.ST.Ind1.stVal` | `simpleIO$GGIO1$ST$Ind1$stVal` | boolean | true |
| `simpleIO/GGIO1.ST.Ind2.stVal` | `simpleIO$GGIO1$ST$Ind2$stVal` | boolean | false |
| `simpleIO/GGIO1.ST.Str.Pos.stVal` | `simpleIO$GGIO1$ST$Str$Pos$stVal` | integer | 2 |
| `simpleIO/GGIO1.MX.PhV.mag.f` | `simpleIO$GGIO1$MX$PhV$mag$f` | float32 | 230.5 |
| `simpleIO/GGIO1.MX.A.mag.f` | `simpleIO$GGIO1$MX$A$mag$f` | float32 | 5.25 |
| `simpleIO/GGIO1.CO.SPCs.stVal` | `simpleIO$GGIO1$CO$SPCs$stVal` | boolean | false（可写） |
| `simpleIO/GGIO1.SP.SETV.setMag.f` | `simpleIO$GGIO1$SP$SETV$setMag$f` | float32 | 230（可写） |

对象表只有一份（`sim/iec61850.DefaultObjects()`）：安装初始值、`uic iec61850read -list`
的清单、`uic sim` 的启动横幅都从它读，避免「文档里写的引用」与「实际配的引用」各说
一套。

**端口为什么是 10202**：102 是特权端口，普通用户起不了；示例配置与内置 IED 都用
10202，避免与真实 IED 的端口打架。驱动本身默认端口仍是 102（`hostPort` 补端口时）。

#### 3.10.21.12 点表这一层

| 写法 | 结果 | 说明 |
|---|---|---|
| `simpleIO/GGIO1.ST.Ind1.stVal` | ✅ | 显式 FC |
| `simpleIO/GGIO1.Ind1.stVal` + `labels.defaultFC=ST` | ✅ | FC 由设备标签补齐 |
| `simpleIO/GGIO1.Ind1.stVal`，无 `defaultFC` | ❌ 导入期拒绝 | FC 不可推导 |
| `simpleIO/GGIO1.MOD.Beh.stVal` | ❌ 拒绝 | `MOD` 不是功能约束 |
| `simpleIO/GGIO1.ST.Ind1.stVal` + `length` | ❌ 拒绝 | `WidthNone` 不接受 length |
| `simpleIO/GGIO1.ST.Ind1.stVal` + `selector` | ❌ 拒绝 | 符号地址不接受其它字段 |
| `GGIO1.ST.Ind1.stVal`（缺 LD） | ❌ 拒绝 | 对象引用必须带逻辑设备 |

语法校验全部在 `codec/iec61850.ParseObjectRef` 里做完，点表层不重复实现一份「差不多
的」校验——两份校验迟早不一致，而不一致的那一侧通常是宽松的那一侧。

#### 3.10.21.13 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/iec61850` | BER 整数最短补码与无符号符号位、不定长拒绝、长形式长度、位串/浮点往返；TPKT 往返与版本校验；COTP CR 形状、CC/DT 往返、TPDU size 码；**会话层 `0xFF` 扩展长度**、CN/AC 形状、DT 固定四字节头、FIN/AB；表示层 CP/CPA 形状与上下文编号；ACSE AARQ/AARE 往返、带认证、拒绝、固定 PDU（RLRQ/RLRE/ABRT）；MMS Initiate 形状与响应往返；对象名两种形态；读/写/getNameList 请求形状与响应往返、**写请求变量数与数据数不等时拒绝**、读响应部分失败；类型规格与数据值往返；`Data.ToValue` 拒绝结构体、`ValueFromData` 类型放宽、位串取值；连接 PDU 全链、连接响应全链、拒绝解析、`WrapData`/`UnwrapData`、错误上下文拒绝、释放 PDU；对象引用解析（显式 FC / 默认 FC / 六类语法错）、`String`、`SplitMMSName`、`DataSetName`；**服务端侧构造与解析往返**（`BuildReadResponse`/`ParseReadResponse`、`BuildWriteResponse`/`ParseWriteResponse`、四个请求解析、`MMSServiceTag`、`ConfirmedResponse` 对称、错误服务标签拒绝、坏值降级为 `object-invalidated`） |
| `driver/iec61850` | `Meta`（能力/地址种类/`defaultFC` 与 `probeObject` 字段）；`Open` 拒绝非 TCP 与非法对象引用；`Probe` 建立关联；bool/int/float 三类读；`defaultFC` 补齐；对象不存在 → `address`；写后读回；写不存在对象 → `address`；关联被拒 → `protocol` 且 `assocCount==0`；读超时 → `timeout`；`Browse` 全量枚举且不产生读请求；`Browse` 未知域返回空列表；**部分失败语义（好点 + 坏对象引用时 `err==nil`、全失败才返回错误）** |
| `sim/iec61850` | 手写最小客户端（**不复用驱动**）：默认对象表每个值都能被 `ToValue` 读懂、名字有序、`Set`/`Get`；真实 TCP 上的多对象读、未知对象逐项 `object-non-existent`、写落地、写未知对象失败；**getNameList 分页**（每页 3 项 + continueAfter 驱动读完，且只回项名）；关联被拒；`DropReads` 超时后恢复投递仍能读；有序释放回 RLRE |
| `pointtable` | `PolicyFor("iec61850")` 是 `WidthNone + AllowSymbol` 且不泄漏 length/DPT/selector 标志；正向（显式 FC、默认 FC 补齐）× 反向（缺 LD、非标准 FC、带 length） |
| `runtime` | `TestExamplesAreValid` 覆盖 `examples/iec61850.json` |

覆盖率（`go test -count=1 -cover`）：

| 包 | 覆盖率 | 说明 |
|---|---|---|
| `codec/iec61850` | 70.0% | 未覆盖的主要是各畸形输入分支与少量数据类型 |
| `driver/iec61850` | 63.2% | I/O 路径由 `sim/iec61850` 的真实 TCP 回环覆盖 |
| `sim/iec61850` | 83.3% | |

与 KNX/Host Link 一节的处理一致：跨包统计（`-coverpkg`）下驱动的 I/O 路径覆盖率显著
更高，本仓库选择「跨包统计 + 在文档里写清楚」，而不是为了数字造重复用例。

#### 3.10.21.14 位置

| 文件 | 内容 |
|---|---|
| `internal/codec/iec61850/ber.go` | BER 基础（TLV、长度、整数最短补码、无符号符号位、位串、浮点、时间） |
| `internal/codec/iec61850/tpkt.go` | TPKT 组帧/解析/长度预读（版本 3） |
| `internal/codec/iec61850/cotp.go` | COTP CR/CC/DT/DR、TPDU size 码、`DefaultCOTPOptions`、`BuildCOTPData`/`ParseCOTPData` |
| `internal/codec/iec61850/session.go` | 会话层：**独立的 LI 解析**（`0xFF` + 恰好两字节）、CN/AC/DT/FIN/DN/AB/RF、`ParseSPDU`、SPDU 常量 |
| `internal/codec/iec61850/presentation.go` | 表示层 CP/CPA/CP-Type/UserData/Abort、上下文编号（1=ACSE、3=MMS） |
| `internal/codec/iec61850/acse.go` | AARQ/AARE/RLRQ/RLRE/ABRT、`ACSEReason`、应用上下文 OID |
| `internal/codec/iec61850/association.go` | 建链三段封装：`BuildConnectPDU`/`ParseConnectResponse`、`WrapData`/`UnwrapData`、`BuildReleasePDU`/`ParseRelease`、`BuildAbortPDU` |
| `internal/codec/iec61850/mms.go` | Initiate（请求/响应）、对象名、confirmed 封套、Read/Write/getNameList/getVarAccessAttrs、TypeSpec、ConfirmedError、Reject、`MMSServiceTag`、`ConfirmedInvokeID`、conclude |
| `internal/codec/iec61850/data.go` | `Data`/`Value`、`DecodeData`/`EncodeData`、`ToValue`/`ValueFromData`/`DataFromValue`、数据访问错误码与名称 |
| `internal/codec/iec61850/address.go` | `IsValidFC`、`ParseObjectRef`、`ObjectRef.{ObjectName,String}`、`SplitMMSName`、`DataSetName` |
| `internal/driver/iec61850/driver.go` | 主站：`Dialer`/`New`/`NewWithDialer`、`Meta`、`parseSettings`、`buildPlans`/`planPoint`、`Open`、`Probe`/`Browse`/`listNames`/包级 `Browse`、`Close`、`ensure`/`redial`/`associate`/`awaitCOTPConfirm`/`awaitAssociation`/`release`、`sendRaw`/`sendSession`/`readFrame`/`roundTrip`、`Read`/`readObject`/`Write`/`writeOne`、`classifyMMS`/`dataError` |
| `internal/sim/iec61850/server.go` | 内置 IED：`NewServer`/`Addr`/`Close`、变量树 `Set`/`SetBool`/`SetFloat`/`SetInt`/`Get`/`Names`、激励 `RejectAssociation`/`DropReads`、观测 `AssocCount`/`ReadCount`/`WriteCount`、`handleData`/`handleAssociate`/`handleRequest`/`handleRead`/`handleWrite`/`handleGetNameList` |
| `internal/sim/iec61850/objects.go` | `DefaultObjects()`（单一事实来源） |
| `internal/pointtable/address.go` | `PolicyFor("iec61850")`（`WidthNone + AllowSymbol`） |
| `cmd/uic/iec61850read.go` | 自检命令（`-addr`/`-object`/`-type`/`-fc`/`-domain`/`-write`/`-probe`/`-browse`/`-list`/`-timeout`/`-json`/`-raw`） |
| `cmd/uic/main.go` | `iec61850read` 分派、`sim -protocol iec61850`、`simIEC61850` 启动横幅 |
| `examples/iec61850.json` | 8 点示例（健康/状态/位置/电压/电流/两个可写点） |
| `internal/runtime/runtime.go` | `RegisterBuiltins()` 注册 `iec61850drv.New()` |

### 3.10.22 SNMP v3（USM 认证 + 加密：引擎发现 + 时间窗口 + 密钥本地化，UDP 161）

v3 与 v1/v2c 的差别只在**外层**：PDU、OID 地址模型、批量预算、类型映射、v1 退化
策略全部沿用（§3.10.12）。变的是报文外面多了一层封套：

```
Message ::= SEQUENCE {
    version        INTEGER (2),
    msgGlobalSys:  HeaderData      -- msgID / msgMaxSize / msgFlags / msgSecurityModel
    msgSecurityParameters   -- OCTET STRING 包裹 BER 的 USMSecurityParameters
    msgData          -- 明文时 ScopedPDU；加密时 OCTET STRING 包裹的 ScopedPDU
}
```

`msgSecurityParameters` 是 `OCTET STRING` 包裹的 BER——这一层包裹本身是协议要求，
但**加密时 `msgData` 也套一层 `OCTET STRING`**，这是最容易漏的一处：漏了它，代理解密
后拿到的字节多一个头，`ScopedPDU` 解析失败，症状是「authPriv 一律超时、authNoPriv
正常」。

三件事必须做，且全部用标准库实现（无 CGO、无第三方依赖）：**口令本地化**、**HMAC
认证**、**加密**。

#### 3.10.22.1 口令本地化：密钥是口令与 engineID 的函数

USM 的密钥不是口令本身：

```
Ku  = passwordToKey(authProto, authPassword)          -- 1 MiB 扩展 + 哈希
Kul = localize(Ku, engineID) = H(Ku ‖ engineID ‖ Ku)  -- 绑定引擎身份
```

- **1 MiB 扩展**：把口令循环扩展成 1 MiB 再哈希（RFC 3414 A.2.2）。直接对口令做
  哈希会把搜索空间限制在口令字典上，且不同口令若前缀相同会碰撞。
- **本地化**：`H(Ku ‖ engineID ‖ Ku)`。engineID 一变密钥就变——这正是「代理重装后
  必须重做发现」的根因，也是下面 `setEngine` 把 `keysReady` 打回未就绪的原因。
- **加密密钥从认证算法的哈希派生**（`localizedPrivKey` = 用认证算法哈希本地化的
  口令取前 16/24/32 字节）。因此 **AES-192/256 必须配 SHA-256 及以上**——不是运维
  选择，是密钥派生的数学约束。这条在 `newV3Session` 里硬校验。

口令 8..64 字节，过短在配置期（`newV3Session`）就拒绝，不让它伪装成认证失败。空
用户名只在 `noAuthNoPriv` 档位允许（匿名用户是合法的最弱档）。

#### 3.10.22.2 认证与加密的字节级细节

| 环节 | 做法 | 为什么 |
|---|---|---|
| 摘要计算 | 对「`authParameters` 原地清零」的**原始字节**算 HMAC，不重新编码 | 重新编码会产生不同的 TLV 字节（长度前缀、空串 vs 缺失），摘要必然不匹配 |
| 摘要比对 | `subtle.ConstantTimeCompare` | 非常量时间比较会泄露摘要前缀匹配长度，等于给口令预言机开门 |
| 摘要长度 | **两张固定表**：MD5/SHA-1 → 12 字节（96 位）；SHA-224/256/384/512 → 16/24/32/48 字节 | 见下：它不是「摘要长度的一半」 |
| DES | `privParameters = engineBoots(4) ‖ salt(4)`，IV = `preIV(key[8:16]) XOR privParameters`，CBC 补齐到 8 的倍数 | 3DES 以外唯一被 USM 承认的对称加密 |
| AES | `privParameters = salt(8)`，IV = `engineBoots(4) ‖ engineTime(4) ‖ salt(8)`，CFB **不补齐** | CFB 是流模式，补齐会破坏对齐 |
| 解密后截断 | 按外层 `SEQUENCE` 长度 `trimBERSequence` | CBC 补齐的字节要剥掉，否则 ScopedPDU 解析失败 |

DES/AES 的 IV 差异固化到签名：`encryptScoped`/`decryptScoped` 直接收
`engineBoots/engineTime` 整数与 `salt`，不传拼接字节——传拼接字节会让「IV 拼错」
这类错误在编译期不可见。

**摘要长度为什么不是「摘要长度的一半」**：这是本协议最容易想当然、而且**本地回环
永远测不出来**的一处——主站与内置代理用同一个（错的）长度，互通完全正常；接真实
设备才表现为「认证失败/无响应」，而 USM 的失败是静默的：代理直接丢包，日志里只有
超时。两张表分别来自：

| 协议 | 摘要长度 | 报文里的 authParameters | 依据 |
|---|---|---|---|
| HMAC-MD5-96 | 16 | **12**（96 位） | RFC 3414 §6 |
| HMAC-SHA-96 | 20 | **12**（96 位） | RFC 3414 §7 |
| usmHMAC128SHA224 | 28 | **16**（128 位） | RFC 7860 §3 |
| usmHMAC192SHA256 | 32 | **24**（192 位） | RFC 7860 §3 |
| usmHMAC256SHA384 | 48 | **32**（256 位） | RFC 7860 §3 |
| usmHMAC384SHA512 | 64 | **48**（384 位） | RFC 7860 §3 |

也就是说 MD5 是 12 而**不是** 8、SHA-1 是 12 而**不是** 10、SHA-224 是 16 而**不是**
14；只有 SHA-2 系列恰好是「一半」，但那个「一半」是标准**选定的值**，不是推导规则。
`TestDigestLen` 因此直接写死六个数，而不是写 `KeyLen()/2`——后者会把错误规则再证
明一遍。

#### 3.10.22.3 引擎发现：只有一条路

```
发空 engineID、无认证、无加密、带 UserName 的 GetRequest
  → 代理回 unknownEngineIDs 的 Report（带 engineID/boots/time，一次拿全三件事）
有了 engineID → 派生密钥 → 用真密钥重发请求
  → 代理若认为时间不在窗口内，回 notInTimeWindow（带新的 boots/time）
    → 刷新缓存后再试一次
```

三条不能省的理由：

- **为什么始终发空 engineID，哪怕配置里固定写了？** 「身份已知」不等于「时间基准
  已知」，而 `engineBoots/engineTime` 只能问出来。跳过它的症状是第一条请求必然吃
  一个 `notInTimeWindow`，重试一轮还是 0/0，最后报「时间窗口不同步」——而真正的原因
  是我们从没问过。
- **为什么不用「已知 engineID + 无认证请求」这条捷径？** 那条报文带了用户名却没有
  摘要，配了 auth 的代理回 `unsupportedSecLevel`；配了 authPriv 的更常见的是直接
  丢弃。两条路都可能拿不到时间基准。
- **探测包为什么要带 UserName？** 空 engineID + 空用户名会被部分代理当垃圾包静默
  丢弃，连 Report 都不回，症状是「首包必超时」。

**engineID 写错不静默纠正**：发现时若代理报的 engineID 与配置里的固定值不一致，
返回 `ErrProtocol`，错误信息里**同时打印两个 engineID** 并建议「删掉 engineID 让
驱动自动发现」。静默改用代理的值会让「配置写错」变成「看起来一切正常」，还会掩盖
那次拓扑变更。

#### 3.10.22.4 时间窗口与自愈

- 时间窗口 150 秒；`engineBoots` 必须相等（代理重启会让 boots 变化）；`engineTime`
  用「学到值 + elapsed」估算。
- **时间窗口是双向的**：不只代理检查我们的请求，我们也检查它的应答（RFC 3414
  §3.2）。USM 摘要只证明报文没被改过，不证明它是新的——一条带正确摘要的旧应答可以
  被原样重放，若主站不比 `engineID/boots/time`，就会把旧值当成新采集结果写进缓存
  （对计量/计数类点位是静默回退）。应答不符时刷新基准重试一次，仍不符则判
  `protocol` 并打印两边的时间；**Report 不查**——它的职责恰恰是带来新的 boots/time。
- 收到 `unknownEngineIDs` → `resetEngine()`（丢掉缓存身份回空 engineID 重发现）；
  收到 `notInTimeWindow` → 保留 engineID 只重学时间。
- **重发必须重新组帧**：AES-CFB / DES-CBC 要求同一把密钥下 salt（进而 IV）绝不
  重复（RFC 3414 §8），所以每轮重发都重新 `BuildV3`（`V3Keys.Salt()` 单调推进），
  **不能**复用上一轮的密文。只有超时才重发（与 v1/v2c 的 `exchange` 同一条纪律）；
  协议错、验签失败、解密失败重发一万次结果相同。重发的 msgID/request-id 不变——
  它是同一条请求，不是两条业务请求。
- **密钥是 engineID 的函数**：`setEngine` 统一把 `keysReady` 打回未就绪，绝不让调用
  方记得手动清标志。漏清一次的后果是「代理重装后一直认证失败，重启进程才好」。
- request-id 配对必须在**解密之后**——加密应答的 id 在密文里。`readV3Datagram`
  只判「结构上是不是 v3」，配对交给 `v3Round`。把配对放在读层会让所有加密应答被当
  不匹配丢掉，症状是「authPriv 永远超时、authNoPriv 正常」。读层**不重发**：它既没有
  请求字节也不重设截止时间，早先那里一个「只 Read 不 Write」的循环把「重发」伪装成
  已实现，实际效果只是超时后空转。
- `transact` 最多两轮；认证前 Report 不签名（密钥不一致签了也验不过），验签之后的
  解密失败 Report 必须签名。

Report 签名策略一句话：**认证前不签，验签后签**。

#### 3.10.22.5 错误映射

USM 的 6 类 Report 各有明确的现场含义：

| Report | 驱动判定 | 含义 |
|---|---|---|
| `usmStatsUnknownEngineIDs` | `ErrProtocol`（自愈：重发现） | 代理不认识我们的 engineID |
| `usmStatsNotInTimeWindows` | `ErrProtocol`（自愈：重学时间） | 时间基准过期或代理重启 |
| `usmStatsWrongDigests` | `ErrPermission` | 摘要错，通常是口令错 |
| `usmStatsUnknownUserNames` | `ErrPermission` | 用户不存在 |
| `usmStatsUnsupportedSecLevels` | `ErrPermission` | 代理不允许这个安全级别 |
| `usmStatsDecryptionErrors` | `ErrProtocol`（Report 已签名） | 解密失败 |

关键坑：**认证/用户/安全级别问题重试无意义，直接判权限错误**；而时间/引擎状态
值得多一轮。把这两类混在一个重试循环里，会让「口令错」表现为两次超时而不是一次
权限拒绝，排障方向完全错。

#### 3.10.22.6 配置形态

```json
{
  "id": "ch-snmp3",
  "protocol": "snmp",
  "transport": { "kind": "udp", "address": "127.0.0.1:16161" },
  "labels": {
    "version": "3",
    "username": "monitor",
    "authProtocol": "sha256",
    "authPassword": "authpass123",
    "privProtocol": "aes",
    "privPassword": "privpass123",
    "engineID": "80001f888000000002",
    "options": "retries=2,maxOids=16"
  }
}
```

`engineID` 可选：留空则自动发现；填了则固定（省一次发现往返，但写错会明确报错）。
`privPassword` 留空回退到 `authPassword`（USM 允许两者不同，回退是显式的）。所有
口令在配置里是 Secret（界面掩码），CLI 侧走环境变量（`UIC_SNMP_AUTH_PASSWORD` /
`UIC_SNMP_PRIV_PASSWORD`），不落日志。

#### 3.10.22.7 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/snmp` | RFC 3414 A.3.1 的 SHA-1/MD5 派生向量逐字节通过；**6 种认证算法的摘要截断按 RFC 3414 §6/§7 与 RFC 7860 §3 的固定表钉住**（MD5/SHA-1=12、224/256/384/512=16/24/32/48，不是 `KeyLen/2`）；DES/AES 加密→解密往返；空 engineID 只允许发现报文、带 auth/priv 拒绝；`BuildV3`/`ParseV3`/`VerifyV3` 往返；`ScopedPDU` 加解密往返 |
| `driver/snmp` | `TestV3EndToEnd`（发现 → 认证 → 加密 → 读 → 写 → 读回）；口令错 → `permission`；未知用户 → `permission`；安全级别不匹配 → `permission`；`TestV3FixedEngineID`（读写成功 + 全程只有 1 次 Report）；`TestV3FixedEngineIDMismatch`（错误含两个 engineID + `protocol`）；`TestV3EngineResetRecovers`（代理换身份后会话自愈）；`TestV3FixedEngineIDNoAuth`；**`TestStaleEngineWindow`**（engineID/boots/时间窗口六个拒绝分支 + 窗口内放行）；**`TestReadV3RoundRejectsStaleEngine`**（接线：读层真的调用检查）；**`TestV3RetransmitsWithFreshSalt`**（超时重发恰好一次、每轮密文与 privParameters 都不同、msgID 不变） |
| `sim/snmp` | 真做 HMAC 验签与 DES/AES 解密（两侧互为对端，杜绝「自己骗自己」）；6 类 Report；引擎身份可切换；`Faults.DropPercent` 模拟真机静默丢弃 |
| `cmd/uic` | v3 flag 折参、`secret()`、`securityLevelLabel()` |

### 3.10.23 MTConnect（HTTP + XML：`/probe`、`/current`、`/sample`，默认端口 5000）

MTConnect 是唯一「HTTP + XML、只读、一条请求覆盖整台设备」的协议：没有事务、没有
请求-应答的配对方程式，`/current` 一次 GET 就把全设备的所有数据项都带回来了。它的
形态与前面所有协议都不同，因此单独立一节。

```
HTTP GET /probe    → XML：设备清单 + 每台的 Component/Controller/DataItem
HTTP GET /current  → XML：DeviceStream 的 Samples/Events/Conditions
HTTP GET /sample?from=<seq>&count=<n>  → XML：按序号翻历史窗口
```

#### 3.10.23.1 寻址：只有 `symbol`，且必须只有一个

点位 `kind=symbol`，`path` 是 Agent 的 `dataItemId`（如 `Xabs`）。值有多宽
由数据项文本与点表 `dataType` 共同决定，所以点表用 `WidthNone`——与 IEC 61850
同类。四者在导入期就拒绝，不让它伪装成可读写：

- `selector`（`#`）一律拒绝——`/current` 一次就覆盖全设备，没有「子树」可言；
- `index` / `length` 拒绝——`dataItemId` 里的点号是 Agent 的命名习惯
  （`avail`、`Xabs`…），不是数组下标；
- 只读协议：`Write` 声明 `unsupported` 且每项都返回 `ErrUnsupported`（标准没有
  写数据项的服务）。`Subscribe`/`Browse`/`Discovery` 也一并标 `unsupported`——
  能解析 `/sample` 不等于能订阅，`/probe` 是一次性诊断，不是调度式浏览。

#### 3.10.23.2 单点问题不升级为链路错误（整套驱动的核心取舍）

`/current` 一条请求覆盖全设备，因此**「多一个点」和「多一百个点」成本相同**——
`MultiRead` 声明 `supported`。正因为一条请求扛全设备，最容易被写坏的地方是：把
「某个点此刻 `UNAVAILABLE`」「点表里这个 ID 根本不存在」当成 `Read` 的错误返回。
那样上层会按错误分类丢掉整条连接，一个本来不可用的点会让整台设备每周期
重连一次。所以规则是：

| 单点现象 | 落点 | `Read` 是否报错 |
|---|---|---|
| 数据项存在但此刻 `UNAVAILABLE` | `QualityBad` | 否，写进该点 |
| ID 不在本次快照里 / 文本按 `dataType` 解析失败 | `QualityAddressError` | 否，写进该点 |
| 文档拿不到 / 解析失败 / 设备选不出来 | `ErrProtocol`/`ErrAddress` | 是，`Read` 才返回错误 |

#### 3.10.23.3 条件两层结构，级别即质量

MTConnect 的条件是两层：**外层元素是条件类型**（`System`/`Actuator`…），
**内层 `Normal/Warning/Fault/Unavailable` 是级别**。同时要兼容「元素名即级别、
`type` 属性承载条件类型」的平铺写法，两种写法归一到同一个级别枚举。条件点的
**值就是级别，级别即质量**，因此质量位不来自某个独立的 quality 字段，而是从
级别直接映射：`FAULT`→`bad`、`WARNING`→`uncertain`、`NORMAL`→`good`。同一 ID
多条条件按 `FAULT > WARNING > UNAVAILABLE > NORMAL` 合并，不是「后者覆盖前者」。

这条映射有一个**必须 fail-closed** 的边界：级别读不出来时判坏，而不是回落成
「普通文本」。现场会写出 `<BogusLevel>OVERHEAT</BogusLevel>`（非标准级别名）或
`<System dataItemId="alm"/>`（只有条件类型、没有级别）。这类观测的 `Category`
仍是 `CONDITION`——语义上它确实是报警，只是严重度读不出。若把它当普通事件处理，
`OVERHEAT` 文本会按 `string` 解析成功，于是上报 `quality=good`、`value="OVERHEAT"`、
`err=nil`：**一条告警被静默吞掉，而且从数据上看完全正常**。这是本驱动最容易犯的
错（能解析、字段不空、没有异常迹象），所以 codec 提供 `Item.LevelKnown()` 把
「条件但级别缺失」显式暴露出来，驱动对它用坏质量 + 明确诊断
（「is a condition (type ...) but carries no recognised level」），而不是让它
落进空值分支误报成「carries an empty value」。测试矩阵里专门有一条
`TestConditionWithoutLevelIsBadNotGood`，用**平铺未知元素名**这个唯一可观测的
路径钉住它——两层的未知级别名文本落在外层（为空），会被空值分支提前拦成
`bad`，测不出 fail-open。

**合并阶段的同一 fail-open 陷阱。** 上面的判坏只在「该 ID 最终仍带着级别缺失」时
才生效。同一 `dataItemId` 出现多条观测时会先经过 `BuildIndex` 的合并，这里若用
`Item.IsCondition()` 判断「这是不是条件」（它额外要求已有可识别级别），一条级别
缺失的条件就会被当作普通观测参与序号比较；更糟的是排名函数曾把未知级别排成
`-1`（低于 `NORMAL` 的 `0`），于是

```
<System dataItemId="alm"><BogusLevel>OVERHEAT</BogusLevel></System>   ← 真实报警，级别读不出
<System dataItemId="alm"><Normal>ok</Normal></System>                 ← 同一 ID 的一条 Normal
```

合并后胜出的是 `NORMAL`，`Condition` 变成 `"NORMAL"`，驱动侧「级别缺失判坏」
分支根本走不到——最终上报 `good` + `"NORMAL"`。这是全套代码里最隐蔽的一条
fail-open：单看 codec 的 `LevelKnown` 测试、单看驱动的 `conditionQuality` 都是对的，
错的只在两者之间的合并。修复有两条，缺一不可：

1. 合并判定改用 `Category == CONDITION`（而不是 `IsCondition()`）——语义上是报警
   就要参与合并；
2. 未知级别的排名改成**最高**（`4`，高于 `FAULT` 的 `3`）——「有报警但严重度读不出」
   比「明确 NORMAL」更需要人看一眼，合并后保持 `Condition == ""`，由驱动判坏。

回归用例 `TestParseStreamsUnknownLevelConditionSurvivesMerge`（两个先后顺序都覆盖）
与驱动级 `TestReadUnknownLevelConditionMergedWithNormalIsBad`。变异验证：把合并
判定改回 `IsCondition()` 或把未知排名改回 `-1`，驱动用例立刻报
`quality=good value=NORMAL`。

#### 3.10.23.3.1 数值属性容错：一个坏属性不得拖垮整份快照

Header 与观测上的数值属性（`instanceId`/`bufferSize`/`firstSequence`/`sequence`…）
不能直接 `xml:",attr"` 到 `uint64`。实测一个 Agent 只要在某条 `sequence=""` 或
`sequence="abc"`，`xml.Decode` 就会返回 `strconv.ParseUint` 错误，**整份
`/current` 解析失败**——一条诊断字段的噪声升级成「全部点位每周期变坏」。这与本驱动
「单点问题不升级为链路错误」的边界直接冲突：本来只该影响一个排序号的坏值，却让
整台设备离线。

因此这些属性统一用 `tolerantUint`（实现 `xml.UnmarshalerAttr`）承接：空值或非数字
解析成 `0`，绝不中断文档。这个降级只影响诊断展示与重复观测的「取更新的一条」排序，
**不影响任何采集值**——数值正确性来自元素正文，不来自 `sequence`。良构数值仍必须
原样读入（用例同时断言 `instanceId=42`/`bufferSize=100`/`lastSequence→nextSequence=10`
与 `sequence=9`，防止修复退化成「一律清零」）。

#### 3.10.23.4 多设备 Agent 必须显式选设备

一条产线常挂多台机床。若都当单设备读，`A` 机床的 `Xabs` 会覆盖 `B` 机床的
`Xabs`，且不报错。因此 `labels.deviceUuid` 优先、其次 `labels.deviceName`；
多台且未选就报地址错误并列出可选设备，绝不静默选第一台。`Probe` 先 GET
`/probe`，遇 404/405 退回 `/current`（裁剪版 Agent 可能只实现后者）。

#### 3.10.23.5 地址规范化与探测顺序

接受 `host:port` / `http://` / `https://`，省略端口一律补 5000，支持反向代理的
基址子路径（`http://host:5000/mtconnect`），拒绝 URL 内的凭据、query、fragment。
`net/http` 自动解 gzip 并按魔数兜底；XML 按**本地名**匹配（带 `mt:` 前缀照样
解析），兼容 ISO-8859-1/latin-1/windows-1252 与 UTF-8 BOM，不支持的字符集显式报错，
不静默产出乱码。文档上限 32 MiB，`/sample` 的 `count` 上限 10000。

#### 3.10.23.6 自检与内置 Agent

`uic mtconnectread` 是不装第三方客户端即可自证链路的工具，依次跑：设备模型
（`/probe`）、整台快照（`/current`）、按 ID 列表（`-id Xabs,exec`）、按序号翻窗口
（`-sample -from 1 -count 20`）。自检 `ProbeDevices` 的 version 优先取 XML Header，
Header 没有 `version` 属性才回落响应头 `X-MTConnect-Version`。内置 Agent 刻意
复现三件事：`toolOffset` 初始即 `UNAVAILABLE`（数据项存在但无值，真机最常见
的常态，用来验证显示为坏质量而不是 0）、条件用规范两层写法渲染、`/sample` 按
环形缓冲序号窗口返回（旧观测被顶掉）；故障注入（状态码 / 探针失败 / 畸形 XML /
HTML / 延迟 / 省略数据项 / 全 `UNAVAILABLE` / 条件）各自独立，测试只打开自己
关心的那一个，绝不两个一起开。

#### 3.10.23.7 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/mtconnect` | 按本地名匹配忽略命名空间；Header 序号与 `nextSequence` 补齐；Samples/Events/Conditions 三类观测；两层与平铺条件写法归一到同一级别并按严重度合并；**条件缺级别**（未知级别元素名/外层无内层/平铺未知元素名）保留 `Category=CONDITION` 且 `LevelKnown()==false`、`IsCondition()==false`；带合法级别的条件与非条件观测 `LevelKnown()==true`；**未知级别条件与同 ID 的 NORMAL 合并后仍保持级别缺失（不得被 NORMAL 顶掉成 good，两个先后顺序都覆盖）**；**空/非数字数值属性（`sequence`/`bufferSize`/`instanceId`）降级为 0 而不使整份文档失败，良构数值原样读入**；`/probe` 设备模型；`MTConnectError` 文档；ISO-8859-1/latin-1/windows-1252 与 UTF-8 BOM 往返 |
| `driver/mtconnect` | 一条 `/current` 覆盖多点的 `MultiRead`；单点 `UNAVAILABLE`→bad、ID 不存在/类型解析失败→address_error 都只写该点；条件级别→质量四态（Fault→bad / Warning→uncertain / Normal→good / Unavailable→bad）；**条件缺级别 fail-closed**：平铺未知元素名必须判 bad 且诊断不得误报成「空值」（变异验证：删掉防护即报 `quality="good" value=OVERHEAT`）；**未知级别条件与同 ID 的 NORMAL 合并后仍判 bad（变异验证：合并判定改回 `IsCondition()` 或未知排名改回 -1，即报 `good value=NORMAL`）**；多设备未选报地址错误并列可选设备；省略端口补 5000；拒绝凭据/query/fragment；`/probe` 404 退回 `/current`；文档上限 32 MiB；`/sample` count 越界报 `unsupported`；对接真实 sim 的集成测试 |
| `sim/mtconnect` | 三轴 Agent；`/probe`/`/current`/`/sample` 序号窗口；两层条件渲染；`toolOffset` 初始 `UNAVAILABLE`；gzip 与子路径；七类故障注入各自独立 |
| `cmd/uic` | `-id` 折参、CSV→索引；自检命令三入口（probe/current/sample） |

### 3.10.24 BACnet MS/TP（RS-485 串行总线：单主站轮询从站，双 CRC 分帧）

BACnet MS/TP（Master-Slave/Token-Passing，ASHRAE 135 clause 9）与 BACnet/IP 共用
NPDU、APDU、对象模型与 ReadProperty（0x0C）/ WriteProperty（0x0F）服务，差别只在
承载：IP 上是 UDP 数据报，MS/TP 上是「preamble + MAC + 长度 + 两条 CRC」的串行帧。
因此这一节不重复对象语义（见 §3.10.8），只讲承载层与由此产生的取舍。

```
55 FF | type(1) | dest(1) | src(1) | len(2,大端) | hdrCRC(1) | data(len) | dataCRC(2,小端)
```

#### 3.10.24.1 定位：单一主站轮询从站，不做令牌环维护

标准里的 MS/TP 是一个令牌环：主站之间传递 Token，只有持牌者可以发帧。本驱动
**只做其中一种拓扑**——一条总线上只有本机一个主站，其余节点都是从站：

| 角色 | MAC 段 | 本驱动的行为 |
|---|---|---|
| 本机主站 | 0..127（默认 0） | 用 DataExpectingReply 点名问从站 |
| 被采集从站 | 128..254 | 回 DataNotExpectingReply，**不持有令牌** |
| 广播 | 255 | 只用于 TestRequest 的目的地址 |

从站不持有令牌，这正是「单主站轮询」能成立的原因：主站发问、从站回答，令牌
自始至终没有转移。驱动**刻意不做**多主站令牌环的建立与维护：

- Tslot 抢占、`NextStation` 周期探活、`Tno_token` / `Tusage_timeout` 这些定时器
  要求毫秒级常驻状态机；
- 现场最常见的接法是「串口服务器把 RS-485 透传到以太网」，TCP 往返与缓冲抖动
  会直接破坏这些时序；
- 一个只在收到请求时才醒来的驱动伪装成令牌环成员，只会把别人的环拖垮。

同理不做 Who-Is 发现、不做 Router（NPDU 的路由字段能解析，因为真实设备可能经
路由器回包，但不生成）、不做 COBS 扩展帧、不做波特率自适应、不做 ReplyPostponed
的后续上送（直接映射为协议错误，让上层重试），也不轮询主站设备（问主站要数据
需要把令牌交给它、等它用完再要回来，属于令牌环语义）。

**空闲时不监听总线**是这条拓扑的明确前提，不是遗漏。若现场确实有第二个主站，
正确做法是更换驱动，而不是打开 `labels.nextStation` 指望它维持令牌环。

但在等待从站应答的窗口内，驱动会**被动配合**，否则一个「碰巧被别的总线成员把
令牌递过来」的节点会直接卡住别人的环：

| 收到 | 条件 | 动作 |
|---|---|---|
| Token | `dest == 本机` | 记住，事务结束后交还 |
| PollForMaster | `dest == 本机` | 立即回 ReplyToPollForMaster |
| TestRequest | `dest == 本机` 或广播 | 立即回 TestResponse |
| TestResponse | 任意 | 忽略（可能是别人对别人的测试） |
| ReplyPostponed | 来自目标从站 | 报 `ErrProtocol`，不猜「过一会儿会送哪一帧」 |

交还令牌时优先用 `labels.nextStation`；没配就现场扫——从本机地址之后按环形顺序
发 PollForMaster，第一个回 ReplyToPollForMaster 的地址就是下一站。扫不到说明
总线上只有本机一个主站，此时保留令牌即可，这正是 sole-master 的稳态。单个
PollForMaster 的等待被压到 100ms，否则扫完 127 个地址会把一轮采集拖成几分钟。

#### 3.10.24.2 双 CRC：两条残差是最快的自检

同一帧里存在两种字节序（长度大端、数据 CRC 小端）与两套 CRC，是标准原文如此，
也是二手实现最容易抄错的地方：

| 项 | 算法 | 有效帧残差 |
|---|---|---|
| 头 CRC（8 位，覆盖 `type..len` 共 6 字节） | 标准给出的特殊移位展开 | `0x55` |
| 数据 CRC（16 位，覆盖 `data`） | CRC-16/X-25（反射多项式 `0x8408`、初值 `0xFFFF`、结果取反） | `0xF0B8` |

头 CRC 不是常见的 CRC-8/0x8C 反射式，也不能用「`c ^= c<<1; c ^= c<<2` 逐项累加」
的写法替代——所有移位项必须由**异或数据后的原始值**一次算出，否则会得到另一条
CRC，而且整体残差有时仍碰巧「不对但不报错」。因此编解码层直接对拍单字节向量：
`crcHeaderByte(0, 1) == 0xFE`、`crcDataByte(0, 1) == 0x1189`。

早期实现常犯的另一个错是把头 CRC 的算法套到数据上：结果是 Token 帧（数据段为空
或极短）能收，一收真正的数据帧就丢，症状看起来像「从站只应答探测」。`MSTPMaxData`
取 501，超过就视为非法长度——不是「先收下来再看」，因为长度字段本身就是帧同步的
一部分。

#### 3.10.24.3 一次事务的形状：排空 → 设限 → 静默 → 发 → 扫帧

```
bus.Lock()                        // 整段持有总线锁
  Drain()                         // 清掉上一轮迟到的字节
  SetDeadline(now + timeout)      // 必须重新设，Drain 不恢复原值
  wait(presilence)                // 默认 5ms
  Write(frame)
  for { frame := scan.next(); dispatch }
bus.Unlock()
```

四条硬性顺序，每一条都对应一个现场症状：

1. **`Drain` 必须在 `SetDeadline` 之前**。`link.tcpLink.Drain` 会把 deadline 临时
   压到 `now+1ms` 且**不恢复**；反序会让每一次写都立刻超时，表现为「永远连不上」。
2. **发送前留 `presilence`**（`labels.presilence`，默认 5ms）。RS-485 收发器从接收
   切到发送需要时间，主站与从站之间还要留 `Tturnaround`。设得太短在长线/多分支
   现场表现为「偶发首字节 0x55 丢失」→ 整帧被当噪声 → 随机超时。
3. **总线锁覆盖「排空 → 发送 → 收完应答」整段**，不是只包住 `Write`。同一条
   RS-485 上多台从站共用一个 fd，交错会让两边的字节互相污染。
4. **分帧用滚动缓冲逐字节重扫**。任何一层校验失败只丢**一个**字节再重扫，而不是
   丢掉整段候选头——后者会把紧跟在假 preamble 后面的真帧一起吞掉，症状是
   「偶发丢一帧」，在 1s 采集周期里看起来就是随机超时。

扫描器（`frameScanner`）跨事务保留缓冲：上一次超时后迟到的一帧必须留在缓冲里
继续凑，而不是每轮清空——清空会让「总线上有一帧正在路上」变成永远读不到。

重试只对超时生效，共 2 次；断链、协议错、地址错都不重试。如果窗口内收到过令牌，
则**不重试**：令牌已经还回去了，再发帧会撞上别人的发送窗口。重试发生在总线锁
之内，因此重试期间不会插进别的从站事务。

#### 3.10.24.4 寻址与配置

对象地址与 BACnet/IP 完全一致（`kind=object`：`objectType` / `instance` /
`property` / `propIndex`），点表策略共用 `WidthProtocol + AllowObject`。区别只在
「设备」这一层——MS/TP 多了一个 MAC：

| 配置 | 含义 | 默认 |
|---|---|---|
| `device.labels.address` | 从站 MAC，**必须 128..254** | 回退 `device.unitId` |
| `device.labels.master` | 本机主站 MAC，0..127 | 0 |
| `device.labels.nextStation` | 交还令牌的目标主站；不配则现场探测 | 无 |
| `device.labels.presilence` | 发送前静默 | 5ms |
| `transport.kind` | `serial`（RS-485）或 `tcp`（串口服务器/DTU 透传） | — |

从站 MAC 落在 0..127 时在**配置期**就把 `Open` 打回。理由不是洁癖：`address=3`
会被总线上真正的 3 号主站认领，症状是「读到的值属于另一台设备」，现场能查三天。
`pointtable` 侧对 `bacnet` / `bacnet-mstp` 共用同一套策略，对象类型允许连字符与
纯数字写法（`analog-input`、`0`），真正的合法性（名字对应哪个编号、编号是否在
0..4194303 内）交给 `codec/bacnet` 判定，点表层不越权。

TCP 承载省略端口时补 4001（本平台与串口服务器的约定，与 IEC 101、DL/T 645 等
串口协议一致）；串口按物理路径共享、事务由总线锁串行，因此 `concurrency` 必须是
1——写 2 不会更快，只会让两个事务在同一个 fd 上交错。

#### 3.10.24.5 与 BACnet/IP 的分工

| 维度 | BACnet/IP | BACnet MS/TP |
|---|---|---|
| 承载 | UDP（connected socket） | RS-485 串口 / 串口服务器 TCP 透传 |
| 分帧 | BVLC 长度 + NPDU | preamble + MAC + 双 CRC |
| 事务配对 | 8 位 invoke ID（UDP 无连接） | 8 位 invoke ID + 目的/源 MAC 双过滤 |
| 多读 | `MultiRead` 仍为 `unsupported`（逐条） | 同左 |
| 地址 | `kind=object` | 同左，另加设备 MAC |
| 传输故障 | 迟到应答丢弃、超时重发 2 次 | 同左，另加坏帧重扫与令牌让行 |

两者共用 `codec/bacnet`（`BuildNPDU`/`ParseNPDU` 被抽成公共层），所以同一份点表
只要改 `protocol` 与设备 MAC 就能换线；对象语义、写标签选择、错误类映射逐字节
相同，测试也复用同一批金帧。

#### 3.10.24.6 错误分类与失败传播

APDU 层错误类映射与 BACnet/IP 一致：类 1（OBJECT）→`address`；类 2（PROPERTY）
里 `unknown-property` / `invalid-array-index` / `value-out-of-range` /
`invalid-data-type`→`address`，`read/write-access-denied`→`permission`；类 3
（RESOURCES）→`disconnected` 且标记可重试；类 5（SECURITY）→`permission`；其余
类→`protocol`。Reject / Abort / 认不出的 APDU 一律 `protocol`，不猜语义。

承载层的分类是 MS/TP 独有的部分：

| 现象 | 分类 | 可重试 |
|---|---|---|
| 读超时 / ctx 到期 | `timeout` | 是（同一事务内 2 次） |
| `Write`/`ReadFull` 失败 | `disconnected` | 是（留给下一轮） |
| 头 CRC / 长度 / NPDU 解析失败 | `protocol` | 否 |
| 从站回 ReplyPostponed | `protocol` | 否 |
| 设备回 RESOURCES 错误类 | `disconnected` | 是 |

质量码由分类直接推出：`timeout`→`QualityTimeout`、`address`→`QualityAddressError`、
`disconnected`→`QualityCommError`、其余→`QualityBad`。

失败传播上与其它轮询驱动遵循同一条契约（§3.7.4 / §3.9.5），MS/TP 承载层的
具体表现是：

- **点级失败不拖垮整轮**：一轮里某个点回 Error(unknown-property) 或地址配错，
  只把该点标坏（`address_error`）；只要有任意点成功，`Read` 就**不向上返回错误**，
  好样本照常上送。**全部点位失败**才返回第一个错误，让调度器进入故障/退避——
  否则一台设备会因为点表里一个写错的属性名被整体判死、指数退避，好的点位也
  跟着不再采集。
- **链路级失败时剩余点一并失败**：超时、断连、ctx 到期是链路级，立即返回，
  并把剩余未发的点位显式标坏。一轮里第 2 个点超时，第 3..N 个点不可能成功；
  继续逐个等超时只会把一轮采集拖成 N 倍时长，让「设备掉线」看起来像「网关卡死」。
  总线上的重试留给下一轮调度。
- **链路级失败绕过上面的闸门**：即便本轮已有好样本，一旦出现超时/断连仍以
  该错误返回——一轮采集被周期超时打断本身就是「设备太慢」的信号，需要让调度器
  看到。这与「点级失败看成功计数」是两条独立的判据。

`Probe` 读 `device:1` 的 `object-name`：返回 ComplexACK 说明「总线 + 从站 + BACnet
层」三段全通；返回 Error/Reject/Abort 说明帧能到设备、只是没有 `device:1`，同样
算链路可用——只有超时与非法报文才算失败。这与 BACnet/IP 的探测判据相同，避免
「设备型号不同 → 自检永远红」的误报。

#### 3.10.24.7 自检与内置从站

`uic mstpread` 是不装第三方客户端即可自证链路的工具，与 `bacread` 同构，多了
MAC 与承载参数：

```bash
uic mstpread -transport serial -port /dev/ttyUSB0 -baud 38400 -mac 128 -probe
uic mstpread -addr 127.0.0.1:24001 -mac 128 -type analog-input -instance 1
uic mstpread -addr 127.0.0.1:24001 -mac 128 -type analog-value -instance 2 -write 55.5
```

对象类型/属性/下标的解析被抽成纯函数 `resolveMSTPPoint`，因此「命令行文本 →
结构化地址 + 平台类型」这层逻辑可以在不连串口的前提下被单测覆盖；报文往返仍由
`driver/bacnetmstp` 与 `sim/bacnetmstp` 负责，CLI 不再造第二个客户端。

内置从站（`internal/sim/bacnetmstp`）**只模拟从站**：不产生令牌、不参与令牌环、
不应答 PollForMaster，只应答发给本机的数据帧与 TestRequest。对象表与
`sim/bacnet` 的默认表逐项一致（AI-1 只读温度 42.5/名称 `AI-1`/单位 62、
AV-2 可写设定值与 16 级优先级数组、BI-1 二进制状态、CS-1 可写标牌、
AV-3 每秒自增），所以同一份点表换承载不用改地址。MAC 默认 128，越界在启动期
拒绝；帧扫描同样逐字节找 preamble、坏帧重扫。

#### 3.10.24.8 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/bacnet/mstp.go` | 头 CRC 与数据 CRC 的有效帧残差（`0x55` / `0xF0B8`）；单字节向量 `crcHeaderByte(0,1)=0xFE`、`crcDataByte(0,1)=0x1189`；长度大端与数据 CRC 小端；帧往返；坏 preamble / 坏头 CRC / 坏数据 CRC / 超长数据段；地址分段（`IsMasterMAC` / `IsSlaveMAC`） |
| `driver/bacnetmstp` | 真实 TCP 回环读写；`Open` 对 0..127 的从站地址、越界主站、非法 `presilence` 报错；超时重试与最终超时；迟到/重发 invoke 丢弃；垃圾字节跳过与半帧续读；被动 Token 应答与事务后交还；无 `nextStation` 时 PollForMaster 现场探测下一主站；ReplyPostponed→protocol；错误来源 MAC 不匹配时忽略；错误类映射（object/property/resources/security/未知）；写标签分支（present-value 由对象决定、其它属性由 dataType 推断）与本地类型/范围拒绝；**部分失败语义（有成功点则 `err==nil`、全失败才返回整体错误、链路级失败立即返回并把剩余点标坏）**；链路失败时剩余点一并失败 |
| `sim/bacnetmstp` | 镜像对象表；数组下标与越界；写权限与类型收敛；未知对象/属性回 Error；畸形 ConfirmedRequest 回 Reject 且不 panic；每秒自增与停止；`SetAnalog`/`SetAnalogValue`/`SetBinary`/`SetString`/`SetValue` 各值类型；垃圾帧重扫 |
| `pointtable` | `bacnet` / `bacnet-mstp` 共用 `WidthProtocol + AllowObject`；`analog-input:1.present-value` 与 `0:1.present-value` 都能解析；非法类型/实例/下标被拒 |
| `cmd/uic` | `resolveMSTPPoint` 的对象类型/属性/下标/dtype 组合与拒绝路径；数值写法保留原始文本、标签用规范名 |

### 3.10.25 M-Bus（EN 13757-2/3：主地址轮询 + 自描述记录，RS-485 / 串口服务器）

M-Bus 是欧洲的表计总线（热量表、水表、气表、电表），物理层是两线制、由主站供电
（M-Bus 的名字就来自 "Meter-Bus"）。它与前面所有协议的最大差别不是帧结构，而是
**数据布局由从站决定**：一条应答里没有「第 n 个寄存器」，而是一串自描述记录，
每条记录先说自己多长、是什么物理量，数据才跟在后面。这一节讲的是这条事实带来的
全部后果。

```
E5                                       单字符 ACK（对 SND_NKE 的应答）
10 | C | A | CS | 16                     短帧（无数据域）
68 | L | L | 68 | C | A | CI | DATA | CS | 16   长帧（含数据域）
68 | 03 | 03 | 68 | C | A | CI | CS | 16        控制帧（L=3，无数据域）
```

#### 3.10.25.1 定位：主地址轮询单一主站，不做次级地址

M-Bus 有两条寻址路线：

| 路线 | 地址形态 | 前置动作 | 本驱动 |
|---|---|---|---|
| **主地址**（primary） | 1 字节站号 0..250 | 无 | ✅ 采用 |
| **次级地址**（secondary） | 8 字节全球唯一 ID（表号 + 厂商 + 版本 + 介质） | 先 `SND_UD(CI=52h)` 选中 | ❌ 编解码层显式拒绝 |

只做主地址的理由不是省事：

- **主地址寻址容易排查**。站号写错时「完全没应答」与「应答了但不是这台表」是
  两个能立刻分辨的现象；次级地址下两者都表现为「选中失败」，中间还夹着一次
  `SND_UD` 的状态。
- **次级地址会改变整帧形状**。选中之后地址字段从 1 字节变成 8 字节，校验范围、
  帧长、`L` 的取值全都跟着变。半成品实现比不支持更难排查，因此
  `codec/mbus` 的 `Parse` 在 `A=FDh` 时直接返回 `ErrFrame`，在配置期就把这种
  配置打回，而不是让它在现场表现成「偶发校验和错」。
- **多表轮询是调度器的职责**。一条 RS-485 上挂 250 台表时，由采集调度器按设备
  切分轮次；驱动不需要自己维护「总线扫描表」。`-scan` 只是自检命令的一次性
  动作，不是驱动的周期性行为。

`MaxPrimaryAddr = 250`；`251` 保留、`253` 次级、`254` 网络层、`255` 广播在
`ValidatePrimaryAddress` 里分别给出**点名**的报错（"广播地址"、"需要先 SND_UD
选中"），而不是笼统的"越界"——现场看到「地址 255 不是主地址」比看到「地址越界」
更快定位到「我把广播地址填进点表了」。

#### 3.10.25.2 四种帧：两个起始符，一个单字节

分帧的难点全在「四种帧共用两个起始符」这一条上：

- `E5` 只有 **1 个字节**，没有任何长度或校验字段。它是 `SND_NKE` 的应答，
  也是 `Probe` 唯一依赖的字节。
- `0x68` 既是长帧头，也是控制帧头。区别只在 `L`：`L=3` 是控制帧（无数据域），
  `L>3` 是长帧。
- `0x10` 是短帧头，固定 5 字节。

三种情况都遇到过的一个真实故障：串口服务器把 `E5` 与紧随其后的长帧**粘在一次
read 里**返回。如果扫描器只认 `0x68`/`0x10`，它会先在 `E5` 上报一次重同步，
接着把长帧当噪声跳过，最终报「超时」——而链路其实完全正常。因此 `Reader` 的
扫描顺序是「先看首字节是不是 `E5`」，而不是「先找帧头」。

三处必须同时校验，缺一个就会在残帧上解出鬼数：

| 校验项 | 为什么不能省 |
|---|---|
| 两个 `L` 相等 | 半双工丢字节时最常见的残帧形态；只信第一个 `L` 会把半帧当整帧 |
| 整帧长 = `L + 6` | `68 L L 68` 四个字节 + `C A CI` 三个 + `DATA` `L-3` 个 + `CS` + `16` = `L+6` |
| 结束符 = `16h` | 数据域里合法出现 `16h`，所以不能只靠它定界 |

**校验和的范围**是 `C + A + CI + DATA` 的模 256 和，**不包含**两个 `L`、两个
`0x68` 与 `0x16`。按整帧算会在每个长帧上固定差 3（`68+68+16 = 0xEC`，还要再算
`L`）——这是这个协议被抄错最多的一处，也是金字节测试第一个锁住的东西。

#### 3.10.25.3 自描述记录：为什么点表必须长成符号寻址

CI=`72h` 的可变数据结构是「12 字节固定头 + 一串记录」：

```
id_bcd[4] | manufacturer[2] | version | medium | access_no | status | signature[2]
```

12 字节一个都不能少：少读一个字节，后面所有 VIF 全部错位，读出来的值**都是合法
数字**，只是量纲全错——一块热表的「12345678 Wh」会变成「456.789 m³」。这类错误
不会报错，只会让报表里的数字看起来「有点小」。

每条记录的结构：

```
DIF [DIFE...] VIF [VIFE...] DATA
 │     │        │     │      └─ 宽度由 DIF 的低 4 位决定
 │     │        │     └─ 修饰：计费单位、错误标志、通信参数…
 │     │        └─ 物理量与 10 的幂：energy(Wh) / volume(m³) / flow-temperature(°C)…
 │     └─ 存储号 / 费率 / 子单元（历史值、多费率表）
 └─ 长度编码 + 功能位（瞬时/最大/最小/故障）
```

因此**偏移取决于前一条记录的长度**：换一块表、甚至同一块表换了固件，整条记录流
都会重新排列。这是点表模型必须长出符号寻址的真实原因，不是设计偏好——用
`area:offset:length` 表达 M-Bus 会诱导人写出「寄存器 0 长 4 字节」这种协议里不
存在的概念，而读错位不会报错。

点表地址（`kind=symbol`，`AddressPolicy{Width: WidthNone, AllowSymbol: true}`）：

| 写法 | 含义 |
|---|---|
| `q:energy` | 第 1 条 `energy` 记录 |
| `q:volume@1` | 存储号 1 的 `volume`（DIFE 表达的上一期历史值） |
| `q:power-max` | 功能位=1 的记录（`-min` / `-error` 同理） |
| `q:energy/1` | 同量名第 2 次出现（多费率表常见），`/0` 可省略 |
| `r:0` | 报文第 0 条记录，语义无关——厂商自定义 VIF 不认识时的逃生口 |

`q:` 的三段语法（`name[@storage][/nth]`）与 VIF 表一一对应：`storage` 来自 DIFE，
`nth` 来自「同一物理量在同一条报文里出现多次」这一协议事实。拆成 `selector`/`index`
反而表达不了后者。查找失败时 `FindRecord` 的报错会**列出报文里实际有哪些记录**
（`RecordNames`），因为现场拿到一份读不对的报文时，唯一想知道的就是「这张表到底
有什么」。

单位与倍率由 `LookupVIF` 统一换算到工程值：`energy` 恒为 Wh、`volume` 恒为 m³、
温度按 °C。点表不需要知道这块表用了 `0.001 Wh` 还是 `10000 Wh` 的刻度——这正是
「换一块表不用改点表」的实现基础。VIF 表按**区间 + 幂次**生成（`0x00..0x07` 是
能量 Wh 的 1e-3..1e4，`0x08..0x0F` 是能量 J 的 1e0..1e7…），而不是手抄 256 行：
手抄的错漏（尤其相邻区间差 10 倍的那种）无法被任何测试发现。

#### 3.10.25.4 数据域解码：12 种长度编码

DIF 的低 4 位不是字节数，而是一个长度**编码**：

| 编码 | 含义 | 宽度 |
|---|---|---|
| `01`..`04` | 整数 | 1 / 2 / 3 / 4 字节（**不是**二的幂） |
| `05` | REAL | 4 字节 IEEE754（已经是工程值，不乘 VIF 的幂） |
| `06` / `07` | 整数 | 6 / 8 字节 |
| `09`..`0C` | BCD | 2 / 4 / 6 / 8 位（最高半字节 `Fh` 是负号） |
| `0D` | 变长 | 长度由 LVAR 决定 |
| `0E` | BCD | 12 位 |
| `00` / `08` | 无数据域 | 选择项 / 保留 |
| `0F` | 厂商特定 | 按原始字节暴露 |

LVAR 同时编码**长度与形态**（EN 13757-3 §6.5）：

| LVAR | 形态 | 数据域字节数 |
|---|---|---|
| `00h..BFh` | ASCII 文本 | = LVAR |
| `C0h..CFh` | **正 BCD 数**（`(LVAR-C0h)*2` 位十进制） | = LVAR−C0h |
| `D0h..DFh` | **负 BCD 数** | = LVAR−D0h |
| `E0h..EFh` | **二进制整数**（小端） | = LVAR−E0h |
| `F0h..FAh` | 浮点（标准标 "to be defined"） | = LVAR−F0h |
| `FBh..FFh` | 保留 | — |

驱动按这张表把变长记录分派：`≤BFh` 作文本、`C/D` 段解成有符号 BCD 数（**符号由 C/D 段给出**，与固定宽度 BCD 用末字节 `Fh` 半字节表示符号是两套约定，不能混用）、`E` 段作整数（宽度装不进 `int64` 时退回原始字节，不静默截断）、`F` 段因语义未定按原始字节暴露。`Raw` 一律含 LVAR 首字节，供 `r:` 原始导出。

> 早先实现把 `C/D/E` 段一律当 `KindBinary`（原始字节），且 `F` 段用自造的
> `(LVAR-0xEC)*4`（`F0h→16`、`F4h→32`）加 `F5h=48`/`F6h=64` 长度表——与标准完全不符。
> 后果：可变长 BCD 计数器（大电量值常用）被静默降级成字节、4 字节浮点记录因声明 32
> 字节而整帧解析失败。已修正并加 `TestVariableLengthRecords` 锁四段分派与帧推进。

`2Fh` 是按 16 字节对齐时的**填充**而不是记录——把它当记录会
读出一条长度 0、量名乱码的假记录，后面整段记录随之错位；字节流里出现 `2Fh` 必须
直接跳过。

时间字段有三种宽度：Type G 日期 2 字节、Type F 日期时间 4 字节、Type I 日期时间
6 字节。三者都有「时间无效」位：传感器没接时钟的表会把它置位，此时必须**明确
报不可用**（文本为空，CLI 显示 `(时间无效)`），而不是把 `0xFF` 解成 2159 年——
后者会被当成合法数据进入历史库，再也没法从下游退回。Type F 的世纪号编码是
`1900 + century*100 + year`，写成 `century*100 + year` 会得到「124 年」并被年份
校验判为非法，症状是「所有时间字段都空」——这个 bug 是本轮补测发现的真实缺陷。

#### 3.10.25.5 事务：FCB 是重传语义的核心

主站请求的控制字只有几个位：

| 位 | 主站方向 | 从站方向 |
|---|---|---|
| bit6 | PRM=1（主站） | PRM=0（从站） |
| bit5 | FCB：翻转的帧计数 | ACD：有 2 类数据待取 |
| bit4 | FCV：帧计数有效 | DFC：缓冲满 |
| bit3..0 | 功能码（`SND_NKE`=0，`REQ_UD2`=B，`RSP_UD`=8） | 同左 |

`FCB/FCV` 决定从站把一帧请求当成「新请求」还是「上一次请求的重传」：

| 情形 | FCB | 从站行为 | 访问号 |
|---|---|---|---|
| 新一轮 | 相对上一轮翻转 | 重新读表、重新打包 | +1 |
| **同一轮重试** | **保持** | **回放上一次的应答** | **不变** |
| 从站忘了 FCB | 任意 | 完全不应答或回错帧 | — |

这条语义直接决定驱动的重试实现：`transact` 在重发时**复用同一份请求字节**（FCB
不翻转），只在 `readOnce` 成功之后才 `c.fcb = !c.fcb`。写错的表现是「偶尔读到
上一次的值」——存储值不会被重读，但上来的访问号与数据都是旧的，极难发现。因此
`sim/mbus` 直接把这条语义做成可测的：**同 FCB 重放不改访问号**。

读失败之后驱动补发一次 `SND_NKE` 复位从站的 FCB 状态再重试一次。这一步是 M-Bus
与 Modbus 最大的运维差别：很多表在断电/复位/上位机换人之后会「忘记」自己期待哪个
FCB，表现为问答完全对不上；`SND_NKE` 是唯一能把它拉回已知状态的帧。但它会让从站
清空 FCB 状态，**每轮都发等于每轮都重新握手**，在慢速总线上白白多花一次往返，
所以默认只在失败后发，`labels.options` 的 `send-nke` 供「每隔几轮才读通一次」的
现场打开。

`REQ_UD2` 让从站把**当前整张表**打包上送，因此：

- 点位多少都只发一次请求，`MultiRead` 声明 `supported`——不是驱动帮你合并了请求，
  而是协议本身就以整表为单位；
- 同一条报文里的点位天然一致（同一次快照）。这与「每点一次读」的驱动在语义上
  不同：后者一轮里不同点位可能落在不同的表状态上；
- 报文是周期性的，两个周期之间表可能已经变了。这个边界必须写在文档里，不能靠
  「一次读整表」暗示实时性。

从站用 `DIF=1Fh` 表示「还有后续报文」。驱动跟随后续报文，上限 `max-follow`
（默认 32，`32 × 约 240 字节` 已远超任何真实表的数据量）；不设上限的话，一个
「永远说要更多」的从站会把一次事务挂死。所有分段的记录在拼接后重新编号（`reindex`），
`r:<n>` 才能对整张表连续寻址。

#### 3.10.25.6 错误分类：把「没字节」与「有字节不成帧」分开

`Probe` 用 `SND_NKE` 的**单字节应答**（`E5`）。它不依赖任何数据结构，因此能把两类
完全不同的故障分开：

| 现象 | 诊断 | 错误类别 |
|---|---|---|
| 一个字节都没回来 | 物理层 / 波特率 / 站号错 / 总线没供电 | `ErrTimeout` |
| 有字节回来，但一帧都没成 | 波特率/校验位配错（标准 2400 8E1）、终端电阻缺失、多台表同时应答 | `ErrProtocol` + 排查提示 |
| 帧成了但校验和错 | 总线上有干扰，通常是瞬态 | `ErrProtocol`（可重试） |
| 帧成了但 C/CI 不对 | 从站在正常应答，只是我们理解错了 | `ErrProtocol`（**不**重试） |
| 串口读/写本身失败 | 转换器掉线、fd 被关 | `ErrDisconnected` |

第二行是这个驱动最值得讲的一处。`Reader` 会跳过坏帧继续扫描，所以它最终报出来的
永远是「超时」——但超时的原因可能截然不同。因此 `Reader` 带了一个
`BadFrames()` 计数：只有「确实像帧、但内容不对」才计入，起始符之前的噪声**不算**
（总线上电瞬态本来就会有几字节垃圾，算进去会让驱动误报线路质量差）。驱动在
「有坏帧且从未收到好帧」时报 `ErrProtocol` 并提示查波特率/校验位/终端电阻，而不是
笼统的超时——这两种故障的处置方式完全不同，现场拿着「超时」去查线是白费时间。

校验和错与结构错的处理也不同：校验和错**重发**（瞬态干扰，而且从站已经收到请求，
重发同一 FCB 让它回放上次应答，不会让存储值被重读）；C 字段/CI 不对**不重发**
（从站在正常应答，只是我们理解错了，重试没用）。

这一条不能只靠注释表达。两类失败在错误**种类**上是同一个（都是 `ErrProtocol`），
若重发循环只看 `KindOf(err)`，就会把「不重发」的那一类一起重试——症状是每次读表
都白等 `retries × 超时`，且日志里看不出任何区别。因此实现把它落到驱动 SDK 已有的
`driver.E(..., temporary, ...)` 第 5 个参数上：`exchange` 对校验和错标 `temporary=true`，
对 C/CI 错及次级地址等 `ErrFrame` 标 `temporary=false`；`transact` 在 `KindOf` 判断
之后追加 `if !driver.IsRetryable(err) { return err }`，让标记真正生效。测试也按这个
口径写：`SetBadCI` 让从站回一个「成帧、校验和正确、CI 不受支持」的应答，断言
`ReqCount()` 每轮只增 1（不重发）；`SetBadChecksum` 则每轮增满 `retries+1`（重发）。
刻意不用「把应答地址改成 `FDh`」来构造该场景——那会在**分帧**阶段就被拒，最终表现
成超时，恰好落进「可重发」的分支，测不到本条语义。

`Write` 声明 `unsupported` 并对每项返回 `ErrUnsupported`。M-Bus 的写方向是
`SND_UD`（`CI=51h` 发数据 / `5Eh` 改地址 / `50h` 校时），不实现的理由有两条：
写什么、写成什么格式完全由表厂的应用层剖面决定（一次写整条记录需要主站构造
DIF/VIF 与数据域，写错会改变表的计费参数）；写地址/改速率这类命令会让表从当前
站号「消失」，需要现场人工介入。声明不支持让上层把它当成配置错误，比「支持但
只对某些表有效」更诚实。

#### 3.10.25.7 配置与寻址

| 配置 | 含义 | 默认 |
|---|---|---|
| `device.labels.address` | 从站主地址 0..250，优先于 `unitId`（能区分「站号就是 0」与「未配置」） | 回退 `device.unitId` |
| `device.labels.options` | `retries=N,gap=30ms,send-nke,max-follow=N` | `retries=2, gap=30ms` |
| `transport.kind` | `serial`（RS-485，走共享串口层总线锁）或 `tcp`（串口服务器/DTU 透传） | — |
| `transport.baud/parity` | 标准典型 `2400 8E1`；个别转换器/从站出厂为无校验 | `2400 / even` |
| `concurrency` | **必须为 1**：半双工总线由总线锁串行 | 1 |

两种承载的分帧、校验与命令序列完全相同，因此内置 TCP 从站可以直接当自检目标。
TCP 省略端口时补 4001（本平台与串口服务器的约定，与 IEC 101、DL/T 645 一致）。
串口按物理路径共享、事务由总线锁串行，`concurrency` 写 2 不会更快，只会让两个
事务在同一个 fd 上交错。

半双工纪律与其它串口协议相同，但这里更关键：**先 `Drain` 再 `SetDeadline` 再发送**。
`link.tcpLink.Drain` 会把 deadline 临时压到 `now+1ms` 且不恢复，反序会让每次写都
立刻超时，症状是「连上了，但每次写都 timeout」。

##### 符号前缀撞名：为什么 `AddressPolicy` 需要 `SymbolPrefixes`

`r:<n>` 是「报文里第 n 条记录」，`q:<物理量>` 是「按标准量名取」。两者都是**符号
地址**，与寄存器无关。但点表解析器识别寄存器地址用的是一张**全局区名并集**——
`holding`/`db`/`dt`/`x`/`y`/`r`/`l`/`t`/`c`/`dm`/`em`… 都在这张表里，其中 `r`、`l`、
`t`、`c` 是 Mewtocol 的接点区名，`x`/`y` 也是。而这个识别发生在「本协议是否允许
寄存器地址」的判断**之前**，于是任何纯符号协议只要符号里带冒号、且头部撞上另一个
协议的区名，就会被判成「协议不接受寄存器地址」而在导入期拒绝：

```
line 3: address: protocol "mbus" does not accept register addresses (got "r:0")
```

这条错误信息把人引向「M-Bus 为什么不支持寄存器」，而真正的原因是解析器用了别人的
命名空间。受影响的不只是 M-Bus：`opcua`/`snmp`/`mqtt`/`mtconnect`/`dlms`/`sl651`/
`toledo`/`iec61850`/`knx`/`ads`/`secs` 这些纯符号协议，全部会在符号名撞上区名时
报出同一条误导性的错误。

修法不是「把 `r` 从全局区名表里删掉」——那会让 Mewtocol 的 `r:16.3` 失去寄存器
语义。正确做法是让**协议自己声明它占有哪个前缀**：

```go
PolicyFor("mbus") = AddressPolicy{
    Width: WidthNone, AllowSymbol: true,
    SymbolPrefixes: map[string]bool{"q": true, "r": true},
}
```

解析器见到命中本协议 `SymbolPrefixes` 的前缀就跳过寄存器识别，直接按符号地址处理。
这口子刻意做**窄**：只放行本协议确实定义的写法，其余撞名（M-Bus 下的 `coil:3`、
`dt:0`）仍然按「把寄存器地址贴错了协议」拒绝——宽松放行会让真正的配置错误变成
运行期每周期一次的 `address_error`。

`SymbolPrefixes` 只比前缀字面，不校验冒号之后的部分：`r:abc` 应当是「第 abc 条记录
不存在」（驱动在 Open 时说得更准），而不是「这是寄存器地址」。这是点表层的一贯分工：
**它按地址种类放行，符号内容由驱动校验**。

#### 3.10.25.8 自检与内置从站

`uic mbusread` 五个入口：

```bash
uic mbusread -addr 127.0.0.1:8401 -address 1 -probe       # SND_NKE，只验链路
uic mbusread -addr 127.0.0.1:8401 -address 1              # 整表（全记录 + 固定头）
uic mbusread -addr 127.0.0.1:8401 -address 1 -records q:energy,q:volume@1
uic mbusread -addr 127.0.0.1:8401 -address 1 -raw         # 附 DIF/VIF/DIFE 原始字节
uic mbusread -port /dev/ttyUSB0 -baud 2400 -scan 1-20     # 扫主地址
```

`-scan` 只发 `SND_NKE`、只看 `E5`，不解析任何数据——找表时最需要的是「谁在线」，
而不是「它有哪些记录」。`Table(ctx)` 是驱动上的**公开方法**，自检与采集共用同一套
事务/重试/FCB 逻辑：自检若自己组帧，就证明不了采集路径是通的。

内置从站（`internal/sim/mbus`）是一块虚拟热表，8 条记录与 `examples/mbus.json` 的
11 个点位一一对应（多余 3 个是 `q:power-max` / `q:volume@1` / `r:0` 的语法示例，
默认不采集）。它刻意复现五件事：

1. **同 FCB 重放不改访问号**——否则「同轮重传不重复读数」这条最重要的语义永远
   测不到；
2. 访问号逐次自增（跨轮翻转后才 +1）；
3. 地址不符**完全沉默**——替别人的地址应答会让主站张冠李戴；
4. 坏校验**只改校验和**、结构与长度保持完好（验证主站真的靠校验和发现问题，而
   不是靠结构）；
5. `DIF=1Fh` 后续报文标志（验证驱动的 follow 上限与记录重新编号）。

另外 `SetStatusError`（状态字报应用错误 → 质量降为 uncertain 但记录仍然上送）、
`SetSilent`（完全不应答 → 超时路径）、`SetValue`/`Value`（改值与读回，用于验证
VIF 换算）三个注入点，测试只打开自己关心的那一个。

`SetBadCI` 与 `SetBadChecksum` 是一对，专门用来钉住「哪类错该重发」：前者把 CI
换成不受支持的值并**重算校验和**（保证帧本身是完好的，只有语义不支持），后者只
弄坏校验和。配套的计数器也必须成对看——`Reads()` 只数「生成新应答」的次数（FCB
回放不增），`ReqCount()` 数收到的请求帧（回放也增），两者之差就是重发次数。
`Requests()` 则连 `SND_NKE` 一起数，用它断言重发次数会把复位帧算进去，得不出
干净结论。

#### 3.10.25.9 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/mbus` | 金帧逐字节（SND_NKE / 短帧 / 长帧 / 控制帧）与校验和范围（**不覆盖** L 与起止符）；双 `L` 不等、结束符错、截断 → `ErrIncomplete` vs `ErrFrame` 的区分；`FDh` 次级地址被拒；`E5` 与长帧粘包时先切 `E5`；`BadFrames` 只数内容不对的帧、重同步不计数；VIF 全表与幂次（含相邻编码 10 倍关系、厂商特定 7Fh/FFh、自定义 7Ch、主 VIFE 表 7Dh）；BCD 正负号与非法数字；REAL 不乘 Scale；**LVAR 全编码表（`C/D` 段到 `CF/DF`、`E0..EF` 二进制整数、`F0..FA` 浮点按 LVAR−F0、`FB..FF` 保留）与变长记录形态分派（正/负 BCD→数值、浮点按原始字节暴露、多记录帧推进）**；Type G/F/I 日期时间与无效位、2 月 30 日拒绝；`2Fh` 填充跳过；`DIF=1Fh` 后续标志；固定结构 16 字节布局与 `type` 字节（含 J 族 `0x0B..0x13` 单调 1e3..1e11）；`q:`/`r:` 选择与失败时报候选；编码↔解码互逆 |
| `driver/mbus` | 真实 TCP 回环读写；`MultiRead` 一条事务覆盖多点；FCB 跨轮翻转、同轮回放（访问号不变的端到端验证）；读取失败后 `SND_NKE` 复位再试；同 FCB 重放不累加访问号；坏帧 → `ErrProtocol` 且文案含波特率提示；完全无应答 → `ErrTimeout`；校验和错**重发到上限**（每轮 `retries+1` 次）与 C/CI 错**不重发**（每轮 1 次）成对断言，后者同时断言 `IsRetryable(err)==false`；状态字应用错误 → uncertain；`Write` 逐项 `ErrUnsupported`；`max-follow` 上限；`q:volume@1` / `q:power-max` / `r:0` 寻址；后续报文记录重新编号；地址 251/253/255 在 `Open` 被拒 |
| `sim/mbus` | 默认 8 条记录可被 `RecordNames`→`FindRecord` 直接吃回去；同 FCB 重放不改访问号；访问号自增；地址不符静默；坏校验只改校验和；`SetBadCI` 换 CI 后重算校验和（帧完好、语义不支持）；`Reads` 与 `ReqCount` 在重发时分离；`SetStatusError`；`SetSilent`；`DIF=1Fh` 后续报文；`SetValue`/`Value` 与 VIF 换算一致 |
| `pointtable` | `mbus` 走 `WidthNone + AllowSymbol`；`q:`/`r:` 两种记录寻址都解析成 symbol 且 `Canonical` 往返一致（`r:` 靠策略里的 `SymbolPrefixes` 先一步排除寄存器识别——`r` 同时是 Mewtocol 的接点区名，不排除会被判成「不接受寄存器地址」）；漏写 `q:` 前缀按物理量名处理；`coil:`/`holding:`/`dt:`/`dm:` 与 selector 在导入期拒绝；`length` 列在导入期拒绝；`q:energy` 不被 modbus/s7/iec104/mewtocol/fins 收下，而 `r:16.3` 在 mewtocol 下仍是寄存器位地址 |
| `cmd/uic` | `parsePrimaryAddr`（253/255 的拒绝文案）、`parseScanRange`、`mbusRefs` 列拆分、`rowOf` 可粘贴地址 |

### 3.10.26 DLMS/COSEM（IEC 62056：LN 引用 + 无安全，HDLC 三层承载）

DLMS/COSEM 是电表行业的标准（IEC 62056 系列，对应 EN 13757-1 之外的欧洲电能表
规范），也是一个**被三层协议叠起来**的协议族：

```
应用层   COSEM 对象模型 + xDLMS 服务（GET / SET / ACTION / EVENT）
         A-XDR 编码（IEC 61334-6），APDU 由 AARQ/AARE 协商
会话层   ACSE 关联（application-context + authentication + user-information）
链路层   HDLC（IEC 62056-46）：类型 3 帧、HCS/FCS 两段 CRC、7 位地址域
物理层   RS-485 / 光纤 / 串口服务器，8N1
```

它与前面所有协议的最大差别不是层数，而是**地址是标准固定的六段码**：`1.0.1.8.0.255`
在任何厂商的表上都是正向有功电能。这一节讲的是这条事实带来的全部后果，以及三层叠加
之后每一层各自最容易写错的那一处。

#### 3.10.26.1 定位：LN 引用 + 无安全 + HDLC

DLMS 有两条寻址路线、两种安全等级、两种承载封装。三条选择各自独立，但只有一种组合
能做到「离线写出点表」，本驱动只做这一种：

| 维度 | 选项 | 本驱动 | 理由 |
|---|---|---|---|
| 引用方式 | **LN**（logical name，6 字节 OBIS） | ✅ | 地址是标准固定的，点表可离线书写、跨表复用 |
| | SN（short name，厂商分配的 1..2 字节编号） | ❌ | 编号由厂商在 `object_list` 里分配，同一 OBIS 在不同表上编号不同，必须先「发现再寻址」 |
| 安全 | **Lowest Level Security**（无认证） | ✅ | 只需要建链 + 读，不涉及密钥分发 |
| | HLS（High Level Security，口令挑战） | ❌ | 各厂商密钥分发方式不同，且需帧计数器 |
| | GMAC（AES-GCM 加密 + 签名） | ❌ | 同上，另有帧计数器与密钥轮换 |
| 承载 | **HDLC**（IEC 62056-46） | ✅ | 串口/RS-485 上最常见，TCP 串口服务器也走它 |
| | COSEM wrapper（TCP/UDP，version + wrapper 头） | ❌ | 另一套封装，长度语义与 HDLC 完全不同 |

**不做 SN 引用**的理由值得展开：SN 引用的地址是一个 `base_name + index` 的小整数，
看起来比 6 字节 OBIS 简单。但它不是标准固定的——同一个 OBIS 在 A 厂表上可能是
`0x0D 0x0A`，在 B 厂表上是 `0x21 0x00`。要用它必须先读 `object_list`
（`0.0.40.0.0.255` 的 attribute 2），那是一个嵌套结构，逐条解出 class/version/OBIS，
再在本地建立映射。也就是说 SN 引用必然带一套「先发现再寻址」的运行时模型，与「点表
直接写地址、离线可审」的采集方式不兼容。半成品（只支持某些厂的 SN 编号）比不支持更
难排查，因此这里直接不做，`Browse` 也诚实声明 `unsupported`。

**不做 HLS/GMAC** 的理由同样是「区分故障」：加了认证之后，「密码错」与「地址错」
在现场都表现为「建链失败」，而这两件事的处置方式完全不同（一个找运维要密钥，一个查
点表）。不加安全时，建链失败的原因只可能是地址、波特率、线序——这三件事都能靠
`-probe` 一次问清。

**不做 TCP/UDP 的 COSEM wrapper**：wrapper 是另一套封装（`version=1` + `wrapper`
头 + 长度字段语义全不同），实现它等于再写一套链路层，而现场串口服务器/DTU 绝大多数
是 HDLC 透传。

#### 3.10.26.2 HDLC 类型 3 帧：长度字段定界，两段 CRC

```
7E | A0 | LL | dest | src | ctl | [HCS] | [LLC | APDU] | [FCS] | 7E
```

`A0` 是格式字段的高字节：高 4 位是格式类型 3，bit11 是分段位，低 11 位是长度。
长度字段**跨两字节**：`int(b[0]&0x07)<<8 | int(b[1])`。

四处必须一次做对的地方：

| 要点 | 事实 | 写错的症状 |
|---|---|---|
| **长度语义** | 长度 = **两标志之间全部字节数**，含 2 字节格式字段与末尾 FCS，**不含**标志；整帧 = `len(body)+2` | 少算 2 字节 → 接收方永远等不到结束标志，「第一帧之后就超时」 |
| **HCS 覆盖范围** | HCS 覆盖 `format + dest + src + ctl`，因此**必须先算长度再算 HCS** | 先占位后回填 → 头校验与格式字段不一致，接收方判定「头校验失败」并丢弃整帧 |
| **不做透明转义** | 类型 3 帧的 info 与 FCS 里**可以合法出现 `0x7E`** | 按标志扫帧尾 → 某些应答被切成两半，且只有特定数值才触发 |
| **CRC 算法** | CRC-16/X-25：初值 `0xFFFF`、反射多项式 `0x8408`、结果取反、小端 | 用 CRC-16/CCITT-FALSE → 每个帧都差固定值 |

长度字段的第二条推论是：**有 info 时 HCS 与 FCS 都在；无 info 时长度恒为 7**
（格式 2 + 地址 2 + 控制 1 + FCS 2）。`SNRM` 的金帧 `7E A0 07 03 21 93 0F 01 7E`
就是「无 info、HCS 与 FCS 合一」的形态——它只有 9 个字节，其中 2 个是校验序列
（`HCS` 与 `FCS` 在无 info 时覆盖同一段字节，因此值相同）。

「不做透明转义」这一条是本轮修出的真实 bug：最初的 `ParseRaw` 按 `0x7E` 找帧尾，
在 `1.0.13.7.0.255` 的应答上失败——那个点的 FCS 恰好是 `7E 64`。改成按长度字段
定界 + 校验结束标志之后通过，并且 `FrameLen` 同时接受「带起始标志」与「不带起始
标志」两种视角（内部按偏移算，外部调用者不该被迫记住自己传没传标志）。

CRC 的**固定余数**性质（把整帧含校验序列再算一遍，结果恒为 `0xF0B8`）是两个独立的
校验点：`FCS(data)` 返回校验序列，`GoodFCS(frame)` 返回布尔。测试里两者都要锁：
`FCS(A0 07 03 21 93)` 必须等于 `0F 01`，顺手改一个比特之后 `GoodFCS` 必须失效。

#### 3.10.26.3 地址域与 LLC 头

HDLC 地址域是**每字节 7 位 + 1 个扩展位**（bit0 = 1 表示最后一个字节），支持
1/2/4 字节三种宽度：

| 宽度 | 编码 | 客户端 | 服务端上限 |
|---|---|---|---|
| 1 字节 | `addr << 1 \| 1` | ✅ 恒 1 字节 | `0x7F` |
| 2 字节 | 低 7 位 + 高 7 位 | — | `0x3FFF` |
| 4 字节 | 「高 7 位组 + 低 7 位组」各左移一位 | — | `0x3FFFFFFF` |

4 字节的编码最容易写反：正确的顺序是**先放高 7 位组、再放低 7 位组**
（`48 68 FE FF` 表示两个 28 位地址），而不是「低字节先传」。具体是
`(addr >> 21) & 0x7F`、`(addr >> 14) & 0x7F` … 每段左移一位并置扩展位。

**客户端地址恒为 1 字节**（标准规定），服务端地址可以是 1/2/4 字节。两者宽度不必相同
——这是地址域唯一允许不对称的地方，写反会让应答永远被地址过滤掉。

LLC 头固定 3 字节：

```
E6 E6 00   请求（主站 → 表）
E6 E7 00   应答（表 → 主站）
```

`E7` 是「这一帧是应答」的**唯一**信号。缺了它说明收到的是服务端主动上报
（EventNotification / 数据块传输）——本驱动不做这两者，会明确报协议错，而不是把
一段对不上号的字节当成 Get-Response 硬解。

#### 3.10.26.4 应用层：AARQ/AARE 协商与 A-XDR

建链两步，缺一不可：

```
SNRM → UA      HDLC 层进入正常响应模式（表才回 I 帧）
AARQ → AARE    应用层关联（表才允许 GET）
```

少 `SNRM` 的症状是「AARQ 发出去表不回」（它还在断开模式里）；少 `AARQ` 的症状是
「Get-Request 回 Data-Access-Result = read-write-denied 或直接被拒」。

**U 帧控制域是固定常量，不携带可变的 P/F 位**。这一点极易写错：`UA = 0x63`，它的
bit4 是 0；如果驱动按「请求带 P/F=1，则应答也必须 P/F=1」去校验，建链会永远等不到
应答（或把合法的 UA 判成非法）。I 帧的 P/F 才是可变的（bit4），U 帧的 P/F 由常量
自身决定。

APDU 是 BER 编码的 ASN.1，user-information 里嵌 A-XDR：

```
AARQ = 60 <len> A1 09 <app-context OID> BE <len> 04 <len> <xDLMS-Initiate>
AARE = 61 <len> A1 09 <app-context OID> A2 03 02 01 <result> A3 05 A1 03 02 01 <diag> BE ...
```

两个金向量（测试逐字节锁死）：

```
AARQ: 60 1D A1 09 06 07 60 85 74 05 08 01 01 BE 10 04 0E
      01 00 00 00 06 5F 1F 04 00 00 7E 1F 04 B0
AARE: 61 29 A1 09 06 07 60 85 74 05 08 01 01 A2 03 02 01 00
      A3 05 A1 03 02 01 00 BE 10 04 0E 08 00 06 5F 1F 04 00 00 50 1F 01 F4 00 07
```

应用上下文 OID `60 85 74 05 08 01 01` 是 LN + 无安全；末位 `02` 是 SN 引用
（本驱动不做，配置期拒绝）。

`Conformance` 是 `[APPLICATION 31] IMPLICIT BIT STRING (SIZE(24))`，线上 3 字节
大端，编码为 `5F 1F 04 00 <3 字节>`。位值是 `1 << (23 - bit)`（**位号从 0 数、
从左往右**），因此：

| 位 | 含义 | 掩码 |
|---|---|---|
| 3 | read | `1<<20` |
| 4 | write | `1<<19` |
| 8 | attribute0-set | `1<<15` |
| 10 | attribute0-get | `1<<13` |
| 11 | block-get | `1<<12` |
| 14 | multiple-references | `1<<9` |
| 19 | get | `1<<4` |
| 20 | set | `1<<3` |
| 21 | selective-access | `1<<2` |
| 22 | event-notification | `1<<1` |
| 23 | action | `1<<0` |

驱动的 AARQ 只声明真正会用的两位：`get`（读属性）与 `attribute0-get`（读
`object_list` 需要）。**`Probe` 打印的是协商后**的 conformance，不是请求的那份：
标准允许服务端砍掉一半能力，按请求值判断会得出错误结论。实测内置从站回
`00 20 10`（`attribute0-get | get`），AARE 里的 `0x00501F` **不含** `attribute0-get`
——断言必须按应答值而不是请求值。

A-XDR 标签表（IEC 62056-53 §8.6.4）：

| 标签 | 类型 | 标签 | 类型 |
|---|---|---|---|
| `00` | null | `13` | compact-array |
| `01` | array | `14` | long64 |
| `02` | structure | `15` | long64-unsigned |
| `03` | boolean | `16` | enum |
| `04` | bit-string | `17` | float32 |
| `05` | double-long（i32） | `18` | float64 |
| `06` | double-long-unsigned（u32） | `19` | date_time（12B） |
| `09` | octet-string | `1A` | date（5B） |
| `0A` | visible-string | `1B` | time（4B） |
| `0D` | bcd | `FF` | don't-care |
| `0F` | integer（i8） | `10` | long（i16） |
| `11` | unsigned（u8） | `12` | long-unsigned（u16） |

`07`/`08`/`0B`/`0C`/`0E` 是**空号**——不认识的标签必须报错而不是跳过，否则一个
标签读错会让后面整段数据结构错位成「合法但错误」的值。

**`date_time` 的 12 字节布局是最容易错位的一处**（IEC 62056-62 §4.1.6.1）：

```
年(2) 月 日 星期 时 分 秒 百分秒(2) 偏差(2) 状态
```

注意**星期在日月之后、时分之前**。写错一位会把「日」当成「时」、「星期」当成
「分」——解出来的时间看起来完全合法，只是全错。本轮就是这个 bug：最初按
`年月日时分秒星期` 解，结果时钟点显示的时间与从站放的不一致。时区偏差是**带符号
分钟**，`0x8000` 表示未指定（不能用 0 代替，0 意味着 UTC）。`date`（5 字节）是
`年(2) 月 日 星期`，`time`（4 字节）是 `时 分 秒 百分秒`。

#### 3.10.26.5 OBIS 与 `SplitRef`：唯一地址切分点

点表地址（`kind=symbol`，`AddressPolicy{Width: WidthNone, AllowSymbol: true}`）是
`<OBIS>[:<class>[:<attribute>]]`：

| 写法 | 含义 |
|---|---|
| `1.0.1.8.0.255` | 按内置表推导（Register(3) / attribute 2） |
| `1.0.1.8.0.255:3` | 显式类号，属性号默认 2（value） |
| `1.0.1.8.0.255:3:3` | 显式类号与属性号（这里是 scaler_unit） |
| `1-0:1-8-0-255:3:2` | 标准正文写法：OBIS 内部自带冒号 |

**只写 OBIS 时类号与属性号从内置表推导，查不到就报 `ErrAddress` 而不是猜。** 猜错的
类号会让表回 `object-class-inconsistent(9)`（这是一个**好**错误，明确指向类号错）；
而「猜了一个看起来能读的类」会读到另一个对象的**合法数字**，数值一切正常，永远发现
不了。`CommonObjects()` 里的 29 条是跨厂商一致的条目（电能、瞬时量、表号、逻辑设备
名、时钟…），厂商私有扩展不在表内。

OBIS 六段是**十进制**：`1.0.32.7.0.255` → `01 00 20 07 00 FF`。写成 `01 00 32 07
00 FF` 是 50 号对象，不是 32——这个错误在十六进制与十进制之间来回换算时最容易发生，
且读出来的是另一个真实对象的合法值。分隔符 `.`、`-`、`:`、空格都接受，但**段数必须
恰为 6**。

`SplitRef` 是**唯一**的地址切分点，驱动与自检命令都必须走它。原因不是「复用代码」，
而是「标准正文写法 `1-0:1-8-0-255` 自带冒号」：如果按「最后一个冒号」切分，会把
OBIS 内部的冒号当成类号分隔符；如果按「第一个冒号」切分，`1-0:1-8-0-255:3:2` 会被
切成 `1-0` + `1-8-0-255:3:2`。正确做法是**枚举每个冒号位置，看前缀能不能解析成
6 段 OBIS**，第一个能的位置就是切点。这条路只有一条，两边都走它，才不会出现
「自检通过、采集失败」——那是最难查的一类问题。

#### 3.10.26.6 scaler 与单位：一次乘除，不猜单位

Register 家族（Class 3/4/5）的 attribute 2 是**原始整数**，attribute 3 是
`structure { integer scaler, enum unit }`：

```
工程值 = 原始值 × 10^scaler          scaler 为负是除
```

驱动读完 attribute 2 之后再读一次 attribute 3 并应用，带**进程内缓存**
（`c.scalers map[string]scalerEntry`，key = `class/obis`）。缓存的理由是：scaler 是
对象的**静态配置参数**，不随测量值变化，而每轮每点多读一次会让往返数翻倍——在
9600bps 的串口上这是十几秒与几十秒的差别。缓存失效面写在文档里：表在运行中被重新
配置（改 register 的 scaler_unit）后，驱动不会自己发现，需要重连。

缓存只记**确定的结论**，绝不记一次失败的快照。`scalerOf` 的返回有三种语义，必须分清：

| 情形 | 缓存？ | 后果 |
|---|---|---|
| 类本身没有 scaler_unit（Data/Clock 等，按类号即可判定） | 记 `known=false` | 安全：这是类级别事实，与网络无关 |
| Register 家族，attribute 3 读到了 | 记 `known=true` + scaler | 正常缓存 |
| Register 家族，attribute 3 读失败/无法解析 | **不缓存**，交回调用方判坏 | 见下 |

关键在第三种。Register 家族（Class 3/4/5）按标准**一定有** scaler_unit，所以这里读
attribute 3 失败只可能是两种瞬时情形：链路抖动，或表本身这个属性缺失/畸形——两者都
**不是**「类没有 scaler」这一类级别的事实。早先实现把这种失败也写成 `known=false` 哨兵，
造成两个真实缺陷：

1. **静默回绕**：哨兵命中后 `scalerOf` 返回「无 scaler」，于是 attribute 2 的原始整数
   被当 good 直接上送。`2201`（真值 `220.1 V`，scaler=−1）会被报成 `2201 good`——整整
   差 10 倍且毫无征兆。这与 `scalerUnit` 对结构形态「不默认 0，真值可能差 1000 倍」是
   同一条既定原则，驱动层却违反了它。
2. **永久毒化**：缓存没有失效路径，一旦首轮失败写了哨兵，即便链路/点表随后恢复，
   该点也永远按未缩放的原始值上报，回不到正确缩放。

修复后：Register 家族的读失败一律**不缓存**、把错误交回 `readPoint`。`readPoint` 对
Register 取值拿不到 scaler 时**判坏**（设 quality + err）而不是报一个可能错 10^n 倍的
good 值——语义不可判定时判坏，给明确诊断。下一轮 `scalerOf` 会重试 attribute 3，
链路/点表恢复后自动回到正确缩放。注意这只在 Register 家族触发：Data/Clock 等类
没有 scaler_unit，原始值即终值，仍按 good 上送，不受影响。

`ApplyScaler` 必须**一次**乘除 `10^|scaler|`：

```go
p := 1.0
for i := 0; i < abs(scaler); i++ { p *= 10 }
if scaler > 0 { return raw * p }
return raw / p
```

不能逐次乘 10（或乘 `math.Pow(10, -scaler)`）：`5001 / 10 / 10` 得到
`50.010000000000005`，屏幕上就是一串尾数，写进时序库还会污染聚合值（求和/平均）。
`math.Pow(10, -2)` 返回的是 `0.01` 这个**不精确的 double**，`5001 * 0.01` 同样得到
`50.010000000000005`。本轮的第三个真实 bug 就是逐次除，自检时打印出
`50.010000000000005 Hz`。现在驱动层有一条 `TestReadScaledValuesExact` 用五个点
锁死「每一位都对」，比较用 `!=` 而不是近似。

单位枚举只认标准表（IEC 62056-62 Annex A 的常用项）：

| 码 | 单位 | 码 | 单位 |
|---|---|---|---|
| 26 | W | 32 | A |
| 27 | V·A | 33 | C（库仑） |
| 28 | var | 34 | V |
| **29** | **Wh** | 35 | V/m |
| **30** | **VAh** | 43 | Hz |
| **31** | **varh** | 44 | 1/(Wh) |

Wh = 29 这一条被写错过：sim 里三处电能点用了 30（VAh），自检显示「数值 12345678
一切正常」但量纲是视在电能。**这类错误不会报错**，只会让报表里的电量口径全错，
是最难发现的一类。`UnitName` 对未列出的码返回 `unit(N)` 而不是空串——空串会让自检
表格少一列，而少一列比给错单位安全。

#### 3.10.26.7 错误分类：三类「读不到」必须分开

| 现象 | 诊断 | 错误类别 | 可重试 |
|---|---|---|---|
| 一个字节都没回来 | 波特率 / 线序 / 从站地址 / 表未上电 | `ErrTimeout` | ✅ |
| 有字节回来，但一帧都没成 | 波特率/校验位配错（标准 8N1）、终端电阻缺失 | `ErrProtocol` + 排查提示 | ✅ |
| 帧成了但 HCS/FCS 错 | 线路干扰 | `ErrProtocol` | ✅ |
| `data-access-result` = object-undefined / class-inconsistent | **点表写错** | `ErrAddress` | ❌ |
| `data-access-result` = read-write-denied / 未建关联 | 关联或权限问题 | `ErrPermission` | ❌ |
| AARE `result != 0` | 应用关联被拒（上下文不支持 / 需认证） | `ErrProtocol` | ❌ |
| 帧成了但 LLC 不是 `E7` | 收到服务端主动上报（本驱动不做） | `ErrProtocol` | ❌ |
| 应答超过 max-PDU，走数据块 | 需调大 `max-pdu` | `ErrUnsupported` | ❌ |
| 串口读/写本身失败 | 转换器掉线 | `ErrDisconnected` | ✅ |

第二行与第三行的区分靠 `Reader.BadFrames()`：只有「确实像帧、但校验或结构不对」才
计数，起始符之前的噪声不算（上电瞬态本来就会有几字节垃圾）。驱动在「有坏帧且从未
收到好帧」时报 `ErrProtocol` 并**明确提示查波特率/校验位与终端电阻**，而不是笼统的
超时——现场拿着「超时」去查线是白费时间。

**`data-access-result` 归 `ErrAddress` 而不是 `ErrProtocol`** 是有意的：object-undefined
与 object-class-inconsistent 都指向「点表写错了」，重试一万次也不会好。归到协议错会
让调度器一直重试，而运维看到「协议错」会去查线路。

链路级失败（超时/断连）会记住 `c.linkErr`，同一轮里剩下的点位直接整批标坏，不再逐点
各等一次超时。单点失败（OBIS 写错、类号不匹配）**不**升级为链路失败：其它点位仍然
读到了，说明链路是通的。

「可重试」那一列不是抄的，它必须被代码真正执行。HCS/FCS 错与 DM/FRMR、LLC 头不对
在错误种类上同为 `ErrProtocol`，光看 `KindOf` 分不开；实现把可重发性标在
`driver.E(...)` 的 `temporary` 参数上（干扰类 `true`，语义类 `false`），`transact`
在 `KindOf` 判断之后追加 `if !driver.IsRetryable(err) { return err }`。少了这一句，
表拒连（DM）时配置了 `retries=3` 就会把同一个 Get 连发 4 次，每次都换来一个 DM。

#### 3.10.26.8 `data-access-result` 只有一种合法形态

这是本轮第四个真实 bug，值得单独一节：

在 A-XDR 里 `data-access-result` 是 `[1] IMPLICIT ENUMERATED`，**定长 8 位、无长度
前缀**，线上只有两个字节：

```
01 <结果码>
```

不是 `01 01 <结果码>`。最初的实现按「带长度」编码，解析端又去「兼容两种形态」，结果
在结果码为 `hardware-fault(1)` 时灾难性出错：

```
标准 hardware-fault:  01 01
带长度的错误形态:     01 01 xx
        ↑ 前两字节完全相同
```

「兼容」的结果是把所有 hardware-fault 读成「数据不完整」，于是硬件故障全部报成
超时或协议错。修法是**只认标准形态，删掉兼容分支**：宁可让非标实现明确报错，也不要
让标准实现在某一个结果码上静默错判。测试用 `hardware-fault(1)` 做向量正是为了锁住
这个「前两字节相同」的陷阱。

#### 3.10.26.9 配置与承载

| 配置 | 含义 | 默认 |
|---|---|---|
| `device.labels.address` | 服务端逻辑设备地址，优先于 `options` 的 `server=`（能区分「地址就是 0」与「未配置」） | `1` |
| `device.labels.options` | `client=16,server=1,addr-size=1,retries=2,gap=20ms,max-pdu=512` | 见左 |
| `transport.kind` | `serial`（RS-485，走共享串口层总线锁）或 `tcp`（串口服务器/DTU 透传） | — |
| `transport.baud` | 典型 `9600 8N1`，也有 `2400` / `19200` / `115200` | `9600` |
| `transport.parity` | **必须 none**：IEC 62056-46 的 HDLC profile 是 8N1 | `none` |
| `concurrency` | **必须为 1**：半双工总线由总线锁串行 | 1 |

`client` 上限 `0x7F`（1 字节地址）、`server` 上限 `0x3FFF`（2 字节）；`addr-size`
只能是 1/2/4，配成 3 在 `Open` 时就被拒（HDLC 没有 3 字节地址域）。越界或写错的
关键字一律**忽略并保留默认**，而不是猜一个近似值——猜错的表现是「地址不对」，比
「忽略了你的配置」更难查。

`max-pdu` 范围 64..4096。表在应答超过 max-PDU 时**必然**走数据块传输，驱动能识别
（解出 `last-block` 与 `block-number`）并报 `ErrUnsupported` + 提示调大到能装下该
属性，但**不去拼块**——拼块要处理块号连续性、重传与超时重取，是一套独立状态机。

半双工纪律与其它串口协议相同：**先 `Drain` 再 `SetDeadline` 再发送**。
`link.tcpLink.Drain` 会把 deadline 临时压到 `now+1ms` 且不恢复，反序会让每次写都
立刻超时。

#### 3.10.26.10 自检与内置从站

`uic dlmsread` 六个入口：

```bash
uic dlmsread -addr 127.0.0.1:4051 -probe                     # SNRM + AARQ，只验链路
uic dlmsread -addr 127.0.0.1:4051                            # 默认 7 点
uic dlmsread -addr 127.0.0.1:4051 -obis 1.0.1.8.0.255:3:2    # 指定地址
uic dlmsread -addr 127.0.0.1:4051 -raw                       # 附 A-XDR 原始字节
uic dlmsread -addr 127.0.0.1:4051 -scan                      # 试读内置 29 条常见表
uic dlmsread -list                                           # 打印内置 OBIS 表
uic dlmsread -transport serial -port /dev/ttyUSB0 -baud 9600 -address 1
```

`-scan` 不是「全表扫描」：真正的全表扫描必须读 `object_list`
（`0.0.40.0.0.255` 的 attribute 2）并解嵌套结构，本驱动没有内置这套语义
（`Browse` 声明 `unsupported`）。它只回答「库里的常见量这台表有没有」，并如实标注
`✗` 与原因——现场拿到这份输出时，最想知道的就是「这张表到底有什么、为什么这个点读
不出来」。

`-probe` 打印三行（HDLC 地址 / ACSE 协商结果 / COSEM conformance），并明确写出
「上面是服务端协商后的位块，不是本端请求的那份」。默认 7 点是电能、三相电压电流、
总功率、功率因数、频率、表号——覆盖 uint32 / uint16+scaler / int16+scaler /
octet-string 四种值形态。

内置从站（`internal/sim/dlms`）是一块虚拟三相电表，40+ 项对象与
`examples/dlms.json` 的 20 个点位一一对应（15 启用 + 5 个语法示例）。它刻意复现
五件事：

1. **未建关联就读回 `read-write-denied`**（而不是 DM 或不回）——自检能拿到明确原因；
2. **对象未定义与类号不一致两种结果码分开**（后者用于验证 `ErrAddress` 的分类）；
3. **坏校验只改 FCS 前一字节**、结构保持完好（验证主站真的靠校验序列发现问题，而不
   是靠内容）；
4. **地址不符完全沉默**（总线串扰时替别人应答会让主站张冠李戴）；
5. **时钟点是固定 12 字节**（`2026-11-13T08:30:00`），不用它断言「当前时间」。

`SetRejectAssociation` 让 AARE 带 `rejected-permanent`，用于验证「认证失败」的路径；
`SetSilent` 用于验证超时路径。`SetReplyDM` 让从站对 I 帧回 DM（断开模式），这是
「帧成了但语义是拒绝」的典型形态——用于钉住「DM 不重发」。测试只打开自己关心的
那一个，因为「同时坏两处」会让断言失去诊断力。

#### 3.10.26.11 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/dlms` | `SNRM` 金帧 `7E A0 07 03 21 93 0F 01 7E`；`FCS(A0 07 03 21 93) == 0F 01` 与 `GoodFCS` 的固定余数 `0xF0B8`；长度字段跨两字节与 bit11 分段位；info 与 FCS 里出现 `0x7E` 时仍按长度定界（FCS `7E 64` 的回归）；截断 → `ErrIncomplete`、指针前移 1 字节重同步；1/2/4 字节地址域（含 `48 68 FE FF`）；LLC `E6 E6 00` vs `E6 E7 00`；I/RR/RNR/U 帧控制域与 `NS`/`NR` 回绕；AARQ/AARE 逐字节向量与拒绝诊断；conformance 位值与 `5F 1F 04 00` 编码；A-XDR 全标签（含 `0x07`/`0x0E` 空号必须报错）；`date_time` 12 字节布局（星期位置、`0x8000` 时区）与 `date`/`time`；`data-access-result` 只认 2 字节形态（用 `hardware-fault(1)` 锁「前两字节相同」的陷阱）；Get-Response 三分支（normal / datablock / list）；OBIS 十进制六段与 `.`/`-`/`:`/空格分隔；`SplitRef` 对 `1-0:1-8-0-255:3:2` 的切点；`ApplyScaler` 单次乘除（`50.01` 精确）；`UnitName`/`ClassName` 全扫 |
| `driver/dlms` | 真实 TCP 回环：SNRM→AARQ→逐点 GET→DISC；`Probe` 复用已建关联；拒绝关联 → `ErrProtocol`；静默 → `ErrTimeout`；坏校验 → `ErrProtocol` 且文案含「坏帧」与波特率提示；**DM（断开模式）→ `ErrProtocol` 且 `IsRetryable==false`，`retries=2` 下 Get 只发 1 次**（与坏校验「重发到上限」构成成对断言）；`linkErr` 缓存后第二轮不再发包且立即返回；object-undefined → `ErrAddress`；类号不一致 → `ErrAddress`；分块 → `ErrUnsupported` 且提示调大 `max-pdu`；scaler 五个点逐位精确（`50.01`/`0.985`/`220.1`/`5.12`/`345.6`）；**Register 缺 attribute 3 时该点判坏（不报 raw good 静默回绕），补上后同一连接回到正确缩放（不永久毒化）**；`Scan` 条数等于内置表条数且带 scaler_unit 文本；`Association` 采用服务端较小的 max-PDU；`Write`/`MultiRead`/`Browse` 逐项 `ErrUnsupported`；`parseOptions` 越界全忽略；`addressOf` 的 `labels.address` 优先级与 `addr-size=3` 拒绝；空 span 返回 `(nil, nil)`；关闭后所有入口 → `ErrDisconnected` |
| `sim/dlms` | 默认对象表覆盖 40+ 项且 `examples/dlms.json` 能全部读到；AARQ 接受/拒绝两分支；未建关联读 → `read-write-denied`；对象未定义 / 类号不一致两种结果码；地址与客户端地址过滤静默；`SetSilent`/`SetBadChecksum`/`SetRejectAssociation`/`SetReplyDM`；`SetReplyDM` 下 Get 计数仍加一（证明请求确实到达，用于断言是否重发）；`SetValue`/`Value` 的坏定位报错；`AddObject` 链路上途补属性（供测试「缺 scaler_unit 判坏→补上后同一连接恢复正确缩放」）；TCP `acceptLoop`→`serveConn`→`tcpStream` 真实 socket 路径；`Close` 幂等且能清理连接；`isClosed` 对 `net.ErrClosed`/`io.EOF`/超时的分类 |
| `pointtable` | `dlms` 走 `WidthNone + AllowSymbol`；`1.0.1.8.0.255`、`:3`、`:3:2` 与连字符写法 `0-0-42-0-0-255:1:2` 都解析成 symbol 且 `Canonical` 往返一致；寄存器/对象/计算地址在导入期拒绝；`length` 列在导入期拒绝 |
| `cmd/uic` | `dlmsSplitAddr` 走 `dl.SplitRef`（标准写法与只给 OBIS 两种）；`dlmsRefs` 列拆分；`dlmsRowOf` 的「可粘贴地址」带类号与属性号；默认 7 点列表 |

### 3.10.27 SL 651（《水文监测数据通信规约》：中心站侧查询 + 应答）

SL 651 是水文行业的遥测规约（水利部行业标准，2014 年版），现场形态是「一个中心站
+ 若干遥测站挂在一根 RS-485 总线上」，或「遥测站接 DTU/串口服务器，中心站走 TCP」。
它和仓库里其它表计协议（DL/T 645、CJ/T 188、M-Bus、DLMS）的差别不在字段多少，而在
**帧的边界怎么定、校验怎么算、地址往哪个字段里写**这三件事——三件事都有一条「看起来
能自洽、其实全错」的写法，而且错了之后不会报错、只会给出一个看似合理的值。

这一节讲的是这条事实带来的全部后果，以及「中心站侧」这个定位砍掉了哪些东西、为什么砍。

#### 3.10.27.1 定位：中心站侧、查询-应答、整帧取全部

本驱动只做**中心站（主站）**：主动发查询帧、解析上行应答。

```
7E 7E | 中心站(1) | 遥测站(5) | 密码(2) | 功能码(1) | 长度(2) | STX(02) | 正文 | 结束符 | CRC(2)
```

上行帧与下行帧的**地址字段顺序相反**（见 §3.10.27.5），其余字段偏移不变。

「整帧取全部」是这个协议对采集模型最大的影响：一条 `37H`（查询实时数据）的应答里
带着当前水位、降水量、流量、电压、状态字等全部要素，与 `32H` 定时报同形。这与
M-Bus 的 `REQ_UD2`（一条取整表）属于同一类，与 DLMS 的 `Get-Request`（一条读一个
属性）相反。

因此本驱动的 `Read` 只发**一次** `37H`，把点表里所有点位映射到同一帧的要素组里。
逐点发 `37H` 不但把一轮采集拖成 N 倍，还会让同一设备的点位落在**不同快照**上——那
是一种「单独看每个点都对、放一起就自相矛盾」的错误，比读不到更难查。

`Write` 声明 `unsupported`：写方向是配置表（`40H~43H`）、控制命令（`4CH~4FH`）、
人工置数（`35H`）与时钟/密码（`49H`/`4AH`），各有参数与应答格式，写错会改变终端
采集参数或直接控制闸门水泵。半成品比不做更难排查。

自报帧（`2F` 链路维持、`30` 测试、`31` 均匀时段、`32` 定时、`33` 加报、`34` 小时）
codec 能解，但驱动**不主动发起**——中心站不发自报帧，那是遥测站的行为；解析能力
存在的理由是内置从站要用同一份编码器造帧、自检命令要能把抓到的包打印成人能看懂的
样子。

#### 3.10.27.2 分帧：按长度字段定界，不扫 `0x7E`

帧以 `7E 7E` 开头，但它**不是**定界符。正文与校验码里可以合法出现 `0x7E`——本仓库
金向量的第一帧正文首字节就是 `0x7E`：

```
7E 7E 00 9876543210 1234 2F 0008 02 7E08221011100747 03 C3AE
                                    ↑ 正文首字节
```

任何「读到 `0x7E` 就当帧尾」或「两段 `0x7E` 之间就是一帧」的写法，都会在某些应答上
把一帧切成两半，而且**只有特定数值才触发**——大部分时间能跑，一到某个水位就断，是
最典型的「实验室复现不了」的故障。

正确做法只有一条：先读长度字段，再按长度收。

```go
// FrameLen 返回整帧长度：17 个固定字段 + 正文长度。
func FrameLen(bodyLen int) int { return 17 + bodyLen }
```

`Reader.Next` 的工作方式是：在缓冲区里找 `7E 7E`，读出长度字段，算出整帧长度，等
够这么多字节再交给 `Parse`；`Parse` 失败时**指针只前移 1 字节**重新同步，而不是丢掉
整个缓冲。这样一次串口噪声不会让后面紧跟的好帧一起消失。

#### 3.10.27.3 长度字段：数的是「STX 之后、结束符之前」

长度字段是 2 字节大端，**高 4 位是上下行标识**（`0000` 上行 / `1000` 下行），低 12 位
才是长度。长度数的范围是：

```
STX(0x02) 之后、结束符之前的字节数
```

不含 STX、不含结束符、也不含 `7E 7E`。因此：

```
整帧长度 = 1(7E) + 1(7E) + 1(中心站) + 5(遥测站) + 2(密码) + 1(功能码) + 2(长度)
         + 1(STX) + 长度 + 1(结束符) + 2(CRC)
       = 17 + 长度
```

这个 `17` 不是「数出来的巧合」，而是六个固定前缀字段 + STX + 结束符 + CRC 的和。
把「17」写成 `16` 或 `18`，症状都是「第一帧之后永远超时」——因为接收方在等一个
永远不会到来的字节。

M3 多包（`SYN 0x16`）时，其后 3 字节「包总数（12 位）+ 包序号（12 位）」**计入长度
字段**。这是规约明文规定，不是实现选择；本包只解析单帧，遇到 `ETB`（`0x17`）结束符
会如实标出「后面还有包」，但不做跨包重组（重组要会话状态机，见 §3.10.27.14 的边界）。

#### 3.10.27.4 CRC：CRC-16/MODBUS 变体，结果不取反、高位在前

这是本协议最容易照着别的表计协议抄错的一处。规约附录 H.3.2 的算法原文是：

```
第一步  置 16 位 CRC 寄存器为 0FFFFH；
第二步  取数据字节与寄存器低字节异或，结果存回寄存器；
第三步  寄存器右移一位，最高位补 0，移出位待检测；
第四步  移出位为 0 则重复第三步，为 1 则与 A001H 异或；
第五步  重复第三、四步 8 次，处理完一个字节；
第六步  对每个字节重复第二到第五步。
```

**规约在第六步之后就结束了**：「最终 CRC 寄存器的值就是 CRC 的值」——没有常见的
「结果取反」这一步。这与仓库里其它表计协议（DL/T 645、CJ/T 188 用的是 CRC-16/X-25，
初值 `FFFF` 且结果取反）不同。

| 名称 | poly | init | xorout | 金向量结果 |
|---|---|---|---|---|
| CRC-16/X-25（645/188 用的） | `0x8408`（反射） | `FFFF` | `FFFF`（取反） | `3C51` |
| **SL 651（本协议）** | `0xA001`（反射 `0x8005`） | `FFFF` | **`0000`（不取反）** | **`C3AE`** |

覆盖范围是**从第一个 `7E` 起、到结束符止**（含 `7E 7E`、含结束符，不含 CRC 本身）。
传输序是**高位字节在前**——MODBUS 本身是低字节在前，这一条也不同。

因此 `internal/codec/sl651/crc.go` 的函数刻意**不叫** `CRC16X25`：X-25 与 MODBUS
只差最后一步取反，而这一步正好是「值看似正常、实际全错」的那类错误，名字里带 X25
会诱导下一个人照着 X-25 去「修正」。函数名是 `CRC16`，注释里写清等价标准名是
**CRC-16/MODBUS**（`refin=refout=true`、`xorout 0000`）。

#### 3.10.27.5 上下行地址字段反序

只看上行帧永远发现不了这一条：**下行帧的中心站地址与遥测站地址顺序相反**。

| 表 | 字段 2 | 字段 3 |
|---|---|---|
| 表 11（上行） | 中心站地址（1 字节） | 遥测站地址（5 字节） |
| 表 12（下行） | 遥测站地址（5 字节） | 中心站地址（1 字节） |

密码、功能码、长度字段的偏移**不变**。只做被动收包的实现看不出问题，一旦主动发
查询就会把 5 字节站号写进 1 字节中心站字段，从站因为地址不匹配而**完全静默**——现场
表现是「链路测试不通」，而抓包看每一帧都很像对的。

`Builder.frame` 因此按 `dir` 分支写地址域，而不是复用同一段代码。

#### 3.10.27.6 要素标识符：报文自己说明宽度与小数位

正文不是「固定偏移的寄存器」，而是一串「要素标识符 + 数据」：

```
标识符 = 引导符（1 字节） + 数据定义字节（1 字节）
数据定义字节 = (数据字节数 << 3) | 小数位数
```

也就是说报文自己告诉你每个值有多宽、有几位小数，接收方不需要预先协商点表格式。
附录 C 表 C.1 的 131 项可解释引导符（水位 `Z`、时段/累计雨量 `P1`/`PT`、流量 `Q`、
电压 `VT`、闸位 `GH`、含水率 `M10`…）就是协议的数据模型本身，`tables.go` 把它写死
在代码里——同一台终端送来的 `39H` 在任何工程里都是「瞬时河道水位、潮位」，做成配置
项只会让同一份报文在不同站点解出不同含义。

例外是「引导符类」（附录 C 注 a~f）：观测时间（`F0H`）、测站编码（`F1H`）、人工置数
（`F2H`）、图片（`F3H`）、批量雨量/水位（`F4H..FCH`）、`FDH` 批量流速、`45H` 状态字。
它们的宽度与数据定义字节由规约**写死**，**不能**从高 5 位读出来：

| 引导符 | 要素 | 固定数据定义字节 | 固定宽度 |
|---|---|---|---|
| `F0H` | 观测时间 | `F0H` | 5 字节 |
| `F1H` | 测站编码 | `F1H` | 5 字节 |
| `F4H` | 1h 内每 5min 时段雨量 | `F6H`… | 12 字节 |
| `F5H..FCH` | 1h 内 5min 间隔相对水位 1..8 | — | 24 字节 |
| `FDH` | 批量流速 | 恒 `F6H` | 由报文总长决定 |
| `45H` | 遥测站状态及报警信息 | — | 4 字节（HEX） |

按高 5 位读会把「观测时间 5 字节」当成「30 字节」，当场错位。`Guide.Bytes()` 与
`Guide.DataDef()` 因此**先查 `FixedWidth`/`FixedDef`**，再退化到 `(位数+1)/2` 与
`(字节数<<3)|小数位`。`Guide.Variable()` 标出「宽度无法从标识符推出」的三项
（`F2H`/`F3H`/`FDH`），`ParseField` 遇到它们直接报 `ErrBadElement`，而不是按高 5 位
猜一个长度出来。

时间步长码 `04H` 是另一个易错点：它是 **3 字节 BCD「日 时 分」**，数据定义字节固定
`18H`。全 `00` 表示其后跟 `DRP`/`DRZ` 类组合（均匀时段报 `31H` 的结构）。

#### 3.10.27.7 附录 C 全表与三种写法

`tables.go` 收了附录 C 表 C.1 的 **131 项**可解释引导符（14 项引导符类 + 117 项常规
要素），逐行对照原文核准过。`LookupGuide` 支持三种写法：

| 写法 | 例子 | 说明 |
|---|---|---|
| 两位十六进制 | `39`、`F4` | 只认表内的，避免把 `99` 当成引导符 `0x99` |
| ASCII 简称 | `Z`、`PT`、`DRP` | 先精确匹配再前缀，避免 `P1` 命中 `P12` |
| 中文名 | `瞬时河道水位` | 供内部使用；自检命令只用前两种 |

多命中时取编码最小者（保证结果确定）。点表地址推荐写简称：它是人能读的，也是
「同一要素在任何厂商终端上编码相同」的唯一稳定表达。

#### 3.10.27.8 批量要素：`Guide.Slot` 与 `<要素>[<段号>]`

`DRP`（`F4H`，1h 内每 5min 时段雨量）在报文里是**一个 12 字节数据**，语义上却是
12 段 × 1 字节 5min 值；`DRZ`（`F5H..FCH`，相对水位）是 24 字节 = 12 段 × 2 字节。

把它们当一个 HEX 整数解，会得到 `1844674407370955300` 这种溢出值——**不报错、值域
看着像大数**，是最危险的一类错。这是本仓库在把代码写完、跑自检时才抓到的真实缺陷：
最初 `Field.Number` 把 `Values[i]` 整块当一个数，DRP 的 12 字节被解成 `1.8e18`，
而 CRC、分帧、其它要素全部正常。

修法是给 `Guide` 增加 `Slot` 字段显式声明**段宽**：

```go
type Guide struct {
    // ...
    Slot int // 批量要素的段宽（DRP = 1 字节，DRZ = 2 字节），0 表示非批量
}
```

`Field` 随之提供 `Slots(i)` / `SlotCount(i)` / `SlotNoData(i,j)` / `SlotNumber(i,j)` /
`SlotHex(i,j)` / `SlotBytes(i,j)`。点表地址写成 `<要素>[<段号>]`：

```
DRP          → 第 0 段（等价 DRP[0]）
DRP[0]       → 第 0 段
DRP[11]      → 第 11 段（最后一段）
DRZ1[2]      → DRZ1 的第 2 段
```

段号从 0 开始，语法用**方括号**而不是冒号/斜杠：DLMS 用冒号表达类号/属性号、M-Bus
用 `@` 与 `/` 表达存储号与序号，而 SL 651 的「段」是同一数组的下标，方括号最不容易
与其它协议混淆。

段号只对批量要素有效。`Z[1]` 会在 `guideOfWithSlot` 里提前报地址错，而不是让
`SlotNumber` 返回「越界」——「这个要素没有段」比「段 1 越界」更好定位。

#### 3.10.27.9 数值解码：HEX 与 BCD 按引导符分派

常规要素是 BCD，`Field.Number` 按数据定义字节的小数位除以 `10^d`。但 `DRP`/`DRZ`/`ZT`
是 **HEX 整数**：

| 要素 | 编码 | LSB | 例 |
|---|---|---|---|
| `DRP` | 1 字节 HEX | 0.1 mm | `0x19` = 25 → 2.5 mm |
| `DRZ` | 2 字节 HEX | 0.01 m | `0x01F4` = 500 → 5.00 m |
| `ZT` | 4 字节 HEX 状态字 | 无尺度 | `0x00000001` |

按 BCD 解 `0x01F4` 会得到 `194`，正好落在「看起来合理的 194cm」上——这类错值比报错
难查得多。因此换算**按引导符分派**（`Guide.Hex` + `Guide.Scale`）而不是按数据定义
字节。`ZT` 的 `Scale == 0`，`SlotNumber` 显式报 `ErrBadElement`（没有数值含义），
自检命令按原始 HEX 输出。

**缺测**的表示是「与数据位数一样的 `F`」，不是 9999：

```go
// 规约 6.6.4.5 d)：某个要素某个时间点没有数据时，HEX/BCD 报文在对应
// 位置填「与其他数据位数一样的 F」。
func (f Field) NoData(i int) bool { /* 全 0xFF → true */ }
```

若把它当数值解出来就是 `9999…`，会直接污染数据库。驱动把缺测降级为
`quality=bad`（值取 `0`）而不是报地址错误——规约允许某个时段没有数据，那是终端的
合法行为，不是配置错误。`TestSlotNoDataBounds` 同时锁「整组缺测」与「单段缺测」
两种形态：前者是 `Values[0]` 全 `F`，后者只影响一段。

#### 3.10.27.10 报文结构：固定前缀 23 字节 + 要素组 + 电压

标准报文（`32H`/`33H`/`34H`/`37H` 应答等）的正文结构是：

```
流水号(2) + 发报时间(6, BCD YYMMDDHHmmSS) + F1 F1 + 站号(5) + 分类码(1)
+ F0 F0 + 观测时间(5, BCD YYMMDDHHmm) + 要素组
+ 电压(VT + 2 字节 BCD)
```

固定前缀 = **23 字节**（`headLen`）。其中 `F1 F1` 与 `F0 F0` 是「测站编码」与「观测
时间」两个引导符类的标识符 + 固定定义字节——它们出现在报头里，不是要素组的一项。
`parseHead` 只解这一段，`parseStdReport` 再解要素组。

**电压 `VT`（`38H`）在报文里独立编报**，位于要素组之后，不是要素组的一项。codec
因此把它放在 `Report.Voltage` 而不是 `Report.Fields`。驱动的 `decodeRef` 必须单独兜底
查一次：否则 `VT` 点位会「永远读不到」，而 CRC 与其它要素都正常，极难发现。

均匀时段报（`31H`）的结构是「固定前缀 + `04 18` 时间步长码(3) + 1 个要素标识符 +
n 个等宽数据」，走 `parseEvenReport`；小时报（`34H`）要素固定含 `DRP`(12B)、`PT`、
`DRZ`(24B)，走 `parseStdReport`。

#### 3.10.27.11 `IsResponse` 与 `IsUplinkReport` 必须分开

查询类功能码（`37H` 实时、`38H` 时段、`3AH` 指定要素）的**应答沿用查询的功能码**
（表 43/45/49），只是方向变成上行。因此「只看功能码」无法区分「下行查询」与「上行
应答」：

```go
// IsUplinkReport 判断是否为遥测站**自报**类上行帧。
// 自报是终端主动发起的（定时/加报/小时报/链路维持），中心站不索取。
// 注意它不含查询应答。
func IsUplinkReport(f byte) bool { /* 2F/30/31/32/33/34 */ }

// IsResponse 判断功能码在上行方向是否是可解析的报文。
// 查询类功能码的应答沿用查询功能码，必须配合帧的 Dir 字段。
func IsResponse(f byte) bool { /* 上面的 + 37/38/3A */ }
```

漏掉这一条的症状很隐蔽：自报能解析、查询应答全被当成「不是上行报」丢掉，而长度的
BCD 与 CRC 都正确——从「报文对不对」的角度查永远查不出来。驱动的 `query` 用
`IsResponse` 判应答，`IsUplinkReport` 只给「这是自报帧」的展示用。

#### 3.10.27.12 错误分类：三类「读不到」必须分开

| 现象 | 分类 | 排查方向 |
|---|---|---|
| 完全没字节回来 | `ErrTimeout` | 站号、密码、线序、供电 |
| **有字节回来但一帧都不成** | `ErrProtocol` | 波特率/校验位（标准 1200 8N1）、终端电阻 |
| 帧成了但报里没这个要素 | `ErrAddress` | 点表地址、终端上送配置 |
| 要素存在但该段缺测 | `quality=bad` | 不是错误（规约允许） |
| 链路级失败（断链/超时） | 缓存 `linkErr` | 本轮剩余点快速失败，不再逐点等超时 |

第二条是关键：`Reader` 会跳过坏帧继续扫描，所以它最终报出来的总是「超时」；但
「有字节回来、一帧都没成」说明终端确实在应答，只是我们没解对——最常见的是波特率/
校验位配错（1200 8N1 配成 2400 8E1 时收到的全是被采错的位），或地址错导致终端无视
查询。把这归成 `ErrProtocol` 并提示查参数，现场才会去查参数而不是去查线。

`Read` 在 `linkErr` 非空时直接返回，不再发帧：一台坏终端不该让 300 个点位各等一次
3 秒超时。

**断连与超时都要缓存 `linkErr`，且缓存的路径必须对称。** 超时在重发循环耗尽后由
循环底部的 `if driver.IsLinkFailure(lastErr) { c.linkErr = lastErr }` 兜住；断连（写失败
或读拿到 EOF/reset）走的是另一条早退路径——原先它只 `return` 不缓存，于是终端掉线后
`linkErr` 仍为 `nil`，下一轮 `Read` 会把 37H 再写进一条已经断掉的链路、然后再吃一次
EOF。这与 cjt188 等串口驱动「写失败即 `c.linkErr = ...`」不一致，也让「后续 `Read`
直接复用缓存错误」的契约在断连时不成立。修复把断连单独判出缓存：只有 `ErrDisconnected`
缓存，`ErrUnsupported`（配置错）不缓存——换一条链路/改一行配置就好，不该让整轮采集短路。

错误分类还决定**要不要重发**，这两件事不能混为一谈。CRC 错是瞬态干扰，重发同一
流水号让终端回放上一次的应答（不会让它重读数据）；而「帧成了但报文解不开」「功能码
不符」说明终端在正常应答、只是我们理解错了，重发只会拿到同样的结果。两者在错误种类
上同为 `ErrProtocol`，所以不能靠 `KindOf` 区分——实现把它们分别标在 `driver.E(...)`
的 `temporary` 参数上（前者 `true`、后者 `false`），`transact` 在 `KindOf` 判断之后
追加 `if !driver.IsRetryable(err) { return err }` 让标记生效。少了这一句，配置了
`retries=3` 的现场会在每次「报文解不开」时白等 3 个超时周期。

#### 3.10.27.13 配置与承载

```json
{
  "transport": { "type": "serial", "port": "/dev/ttyUSB0", "baud": 2400 },
  "concurrency": 1,
  "labels": { "address": "31234567", "options": "retries=2,gap=30ms,center=1,password=1234" }
}
```

| 配置项 | 含义 | 默认 |
|---|---|---|
| `transport.type` | `serial`（共享串口总线锁）/ `tcp`（串口服务器/DTU 透传） | — |
| `transport.port` | 串口设备或 `host:port` | — |
| `transport.baud` | 波特率 | `1200`（标准典型 1200/2400/9600） |
| `transport.parity` | 校验 | `none`（标准 8N1；个别终端出厂 8E1） |
| `concurrency` | 并发上限 | **必须 1**（半双工总线，同一物理串口按总线锁排队） |
| `labels.address` | 5 字节 BCD 站号（字符串，可带前导零） | 退化到 `device.unitId` |
| `labels.options` | `retries` / `gap` / `center` / `password` | `2` / `30ms` / `0` / 空 |

站号是 **5 字节 BCD，装 10 位十进制**，上限 `maxStation = 9_999_999_999`。现场用的
测站编码普遍是 8 位（如 `31234567`），因此**不能按「5 位数」卡成 `99999`**——那样会
把合法站号直接判成配置错误。`labels.address` 优先于 `unitId`，因为它能显式表达
「站号就是 `00123456`」这种带前导零的编码。

TCP 默认端口取 `4001`（串口服务器/DTU 透传的常见端口，不是标准端口——SL 651 没有
规定端口）。

#### 3.10.27.14 自检与内置从站

```bash
./uic sim -protocol sl651 -listen 127.0.0.1:4001 &
./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -probe     # 只验链路
./uic sl651read -addr 127.0.0.1:4001 -station 31234567             # 整帧全部要素
./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -points Z,PT,VT
./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -points DRP,DRP[3]
./uic sl651read -list                                               # 附录 C 全表
```

`-list` 对批量要素输出 `DRP[0..11]` 与 `slot`/`slots` 字段：不给段号只能取第 0 段，
段号是这类要素实际落点的必要条件，必须在地址列里显式提示。

内置从站 `internal/sim/sl651` 的 `defaultFields()` 刻意覆盖**四种编码分支**，一次
自检就能走到全部路径：

| 要素 | 分支 |
|---|---|
| `Z` / `P1` / `PT` / `Q` | 常规 BCD |
| `ZT` | 4 字节 HEX 状态字（`Scale=0`） |
| `DRP` | 批量 HEX（12 段，含分段缺测） |
| `Z1` | 整组缺测（全 `F`） |

站号/密码不符时**完全静默**——密码错时终端本来就静默，不做「口令预言机」。坏校验
注入只改 CRC、结构保持完好，用来验证主站真的靠校验序列发现问题。电压字面量必须是
合法 BCD（`0x1210` = 12.10V；用 `0x0A` 会造出非法 BCD，让自检「成功但值荒谬」）。

`TestTrigger` 在 `net.Pipe` 上必须配 goroutine：同步管道写会一直阻塞到对端读走，
直接调会把测试挂死。

#### 3.10.27.15 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/sl651` | 链路维持报金帧 `7E 7E 00 9876543210 1234 2F 0008 02 7E08221011100747 03 C3AE`（25 字节，正文首字节就是 `7E`）与测试报金帧（60 字节）逐字节锁死；`CRC16` 的 `C3AE`/`717A` 与**结果不取反的反向断言**（`TestCRCIsNotInverted`：`C3AE` 取反得 `3C51`，实现若被改成 X-25 取反版此测试立刻红）；`FrameLen = 17+长度` 与长度字段高 4 位方向位；分帧时正文/CRC 含 `0x7E` 仍按长度定界、截断 → `ErrBadLen`、指针前移 1 字节重同步；M3 的 3 字节计入长度；上下行地址字段反序（同一站号在两种帧里的偏移不同）；ASCII 字符编码帧 → `ErrUnsupported`（判定用 `&0xF0`）；引导符类固定宽度与固定定义字节；`Guide.Variable()` 三项显式拒绝；时间步长码 3 字节 BCD 与全 0 组合；131 项引导符全扫（编码、简称、中文名、单位）；`LookupGuide` 三种写法与多命中取最小；`ParseField` 固定宽度不按高 5 位；`DRP`/`DRZ`/`ZT` 按 HEX 解与 `Slot*` 全套（`slot_test.go`）；缺测全 `F` 识别（整组与单段两种）；均匀时段报 `31H` 与小时报 `34H`；查询帧 `37H`/`38H`/`3AH` 构造；编码解码互逆 |
| `driver/sl651` | 真实 TCP 回环：`Probe`（37H）→ 整帧读 → 关闭；`ReadWholeFrame` 一条 37H 取全部要素；`ReadSlot` 的 `DRP[0]`/`DRP[1]` 精确值与 `DRP[11]` 缺测降 `quality=bad`、`DRP[99]` 报地址错；`ReadSlotOnNonBatch` 的 `Z[1]` 提前拒绝；`ReadUnknownGuidePoint` 单点报 `addressError` 不升级为链路错；`TestWrongStationTimesOut` / `TestPasswordMismatchIsSilent` / `TestPasswordMatch`；`TestBadCRCRetriedAndReported` 坏校验重发后报 `ErrProtocol` 且文案含「坏帧」与波特率提示；`TestBadReportIsNotRetried` 正文截断（帧成了但报文解不开）**不重发**：`retries=2` 下 37H 只发 1 次且断言 `IsRetryable(err)==false`——与前者构成「可重发 / 不重发」成对断言；`TestSilentStationTimesOut`；`TestLinkErrCached` 静默超时缓存 `linkErr`、第二轮不再发包；**`TestDisconnectCachedShortCircuitsNextRead` 终端掉线（`SetDropOnQuery` 收到查询即断开）归 `ErrDisconnected` 并缓存 `linkErr`、第二轮不再发 37H——与静默超时那条对称，钉住断连早退路径也必须缓存**；`Write` 逐项 `ErrUnsupported`；`TestReadEmptySpans` 返回 `(nil, nil)`；`TestOpenRejectsBadConfig` 站号上限；`TestOpenFallsBackToUnitID`；`TestMetaContract` **断言 8 个能力位全部落在四态内（防 `Discovery`/`BitAccess` 漏写序列化成空串）**；`TestParseOptions` 越界全忽略；`TestGuideOf` 含段号与五种非法写法；`TestQualityMapping` 全错误类 |
| `sim/sl651` | 默认 7 要素覆盖 BCD/HEX/状态字/整组缺测四分支；`37H`/`38H`/`3AH` 分派；自报帧 `2F`/`30`/`32`/`34`；站号/密码不符静默；`SetSilent`/`SetBadCRC`/`SetTruncate`/`SetBadFunc`/`SetDropOnQuery`；**`SetBadFunc` 换的是合法功能码（32H 定时报），只验证「功能码与询问不符」而不验证解析失败——构造「报文解不开」要用 `SetTruncate`**；**`SetDropOnQuery` 收到下行查询即断开、不回应，模拟终端掉线/停电，中心站读拿到 EOF 归断连（区别于 `SetSilent` 的超时）**；`SetValue`/`Value` 坏定位报错；`TestTrigger` 用 goroutine + `net.Pipe`；`TestDefaultFieldsCoverBranches` 扫全部值索引；`TestSetValueAndReadBack` 复用默认 `Z1` 缺测；`Close` 幂等 |
| `pointtable` | `sl651` 走 `WidthNone + AllowSymbol`；要素标识符（`Z`/`PT`/`VT`/`39`/`DRP`）解析成 symbol 且 `Canonical` 往返一致；寄存器/对象/文件/计算/selector 地址在导入期拒绝；`length` 列在导入期拒绝 |
| `cmd/uic` | `parseSL651Station` 的 10 位上限与前导零；`parseSL651Password` 拒绝非 4 位十六进制（不做静默补齐）；`sl651Refs` 列拆分；`sl651SplitSlot` 与驱动 `guideOfWithSlot` 同一套语法；`TestSL651PointsValidation` / `TestSL651SlotValidation` 的批量要素与非批量段号；`printSL651Guides` 的 `slot`/`slots`/`pointAddress`；`sl651RowOf` 按 `Slot*` 逐段输出且 `Count = len(Values)` |
| 实机自检 | sim 起在 `127.0.0.1:4001` 后 `-probe`、无参整帧（`Z`/`P1`/`PT`/`Q`/`ZT`/`DRP` 12 段/`Z1` 缺测/`VT`）、`-raw`、`-json`、`-list`、`-points Z,PT,VT,DRP,DRP[1]`；`uic run` 运行时采集 5 个启用点位全部 `good`（`0.127`/`0.5`/`123.4`/`1234.567`/`12.1V`） |

### 3.10.28 Toledo（METTLER TOLEDO：ASCII 连续输出，只读推送）

Toledo 是称重仪表的连续输出协议。现场形态是「一台衡器接 RS-232/RS-485，或接
串口服务器/DTU 透传到以太网」，衡器上电后**自己不询问就持续往上送**帧（典型
10~25 帧/秒）。它和仓库里所有表计协议的差别不在字段多少，而在**方向反了**——
不是主站轮询、从站应答，而是从站（衡器）单向推送、主站（本驱动）被动收。这一个
差别把错误模型、质量码语义、点位寻址全部带偏，是本节要讲清楚的事。

#### 3.10.28.1 定位：只读客户端、推送模型、无握手

本驱动只做**客户端（C）侧**：连上链路、被动收帧、不发任何命令。衡器不响应查询、
不接受写入，帧里也没有站号、寄存器、功能码可供寻址。

```
ST,GS,+00149.0kg<CR><LF>
└┬┘└┬┘ └──┬───┘└┬┘
状态 毛净  符号+数值+单位
```

「TOLEDO」在业界并不对应单一帧格式，而是一个**家族**。本驱动只实现现场最通用、
也是各国产仪表标注「Toledo compatible」时普遍采用的 **ASCII 逗号
三段式**，其规范形态（D39 手册逐字节给出的实例）为：

```
53 54 2C 47 53 2C 2B 30 30 31 34 39 2E 30 6B 67 0D 0A
即  ST,GS,+00149.0kg<CR><LF>
```

三段分别是：状态（`ST` 稳定 / `US` 运动中 / `OL` 超量程 / `UL` 欠量程）、毛净
模式（`GS` 毛重 / `NT` 净重 / `TR` 皮重）、`<符号><数值><单位>`。数值段按
`WEIGHT(7)` 定宽对齐（5 位整数 + `.` + 1 位小数，即 `00149.0`）。

#### 3.10.28.2 推送模型：Read 里没有任何网络交互

这是本驱动与其它驱动最硬的一条边界。驱动契约是拉取型——`Read` 立即返回一批
样本。而衡器不会因为你「问了一次」而回答。两者的转换只有一种做法是对的：
**`Open` 后起一个后台 goroutine 持续收帧，把最新一帧读数存进 `conn`，`Read`
从 `conn` 里取**。

```go
func (c *conn) pump() {
    r := toledocodec.NewReader(c.link)
    for {
        c.link.SetDeadline(time.Now().Add(c.readTimeout))
        line, err := r.Next()
        // 每轮把 Reader 的累计量同步进 conn：Read/Probe 用它区分「没字节」与「有字节不成帧」。
        c.noteBytes(r.BytesRead(), r.Dropped())
        if err != nil {
            if c.isClosed() { return }
            if isTimeout(err) { continue } // 衡器暂时安静，不是断链
            c.setLinkErr(err); return
        }
        reading, perr := toledocodec.Parse(line)
        if perr != nil {
            // 解不出不等于链路坏（多半是波特率/校验位配错或帧格式选了二进制），
            // 不记 linkErr，但必须计数——否则「有字节但一帧都不成」会被 Read 当成
            // 「没收到」，把排查方向引到接线而不是仪表帧格式。
            c.noteBadFrame(perr)
            continue
        }
        c.mu.Lock(); c.latest = reading; c.latestAt = time.Now(); c.hasData = true
        c.mu.Unlock()
    }
}
```

这里最容易写错、也最影响现场排查的是**解析失败不能无脑 `continue`**。衡器只推送、没有命令可发，现场唯一能看的就是收帧质量。因此 `pump` 必须把两条计数维护起来：

- `bytesSeen`（= `Reader.BytesRead()`）：从链路读到的原始字节数；
- `badFrames`：收到成行但 `Parse` 失败的帧数，附最近一次错误。

有了这两条计数，`Probe` 在「从未收到帧」时才能把两种故障分开（见 §3.10.28.8）：一个字节都没回来归 `ErrTimeout`（查接线/供电/是否开连续输出），读到了字节却一帧都不成归 `ErrProtocol` 并附最近解析错误（查仪表帧格式、波特率/校验位）。`Read` 路径不靠顶层错误区分这两者（推送型驱动不触发重连，见 §3.10.28.8），而是逐点降级为 `comm_error`，区分交给 `Stats()`。混为一谈时，现场会先去改波特率，而真正要改的是仪表的帧格式设置。

由此带来一条硬性约束，与 MQTT 驱动同源：**`Read` 里没有任何网络交互**，耗时与
点位数量无关，链路安静（衡器没推帧）也不会让它超时。真正断链时（读到底层错误）
才把连接标记为重连，而不是每次 `Read` 都去「探一下」——那会让安静的正常设备被
误判为断链。读超时在连续输出下通常只是「衡器暂时安静」，由 `Read` 依据
`latestAt` 判 stale，不当作断链。

#### 3.10.28.3 点位寻址：读数字段 symbol，不是寄存器

Toledo 的点位是「当前一帧读数」的**语义字段**，不是某个寄存器。点表用 symbol
寻址，每个点位读的是同一个字段名：

| symbol | 含义 | 默认类型 |
|---|---|---|
| `weight` | 重量值（带符号，单位见 `unit`） | `float64` |
| `status` | 称量状态 `ST`/`US`/`OL`/`UL` | `string` |
| `mode` | 毛净模式 `GS`/`NT`/`TR` | `string` |
| `unit` | 计量单位（`kg`/`lb`/`g`/`t`/`oz`/`kN`/`lb_oz`…） | `string` |
| `stable` | 是否稳定（`status=ST`） | `bool` |
| `motion` | 是否运动中（`status=US`） | `bool` |
| `decimals` | 数值段小数位数 | `int64` |

`Open` 校验每个点位的字段名：未知字段名在 Open 时即报 `ErrAddress`（fail fast），
不等到 `Read` 才暴露。

#### 3.10.28.4 质量码语义：只有重量类点位随称量状态降级

推送型协议的质量码只能表达「读数有多可信」，不能表达「读到了寄存器」。映射如下：

| 条件 | 质量码 | 作用点位 |
|---|---|---|
| 从未收到过帧 | `comm_error`（`ErrTimeout`） | 全部 |
| 收到过但超过 `staleAfter` | `stale` | 全部 |
| 最新帧状态 `US`（运动中） | `uncertain` | 仅重量类 |
| 最新帧状态 `OL`/`UL`（超/欠量程） | `bad` | 仅重量类 |
| 最新帧数值段是掩码占位 | `bad`（`ErrProtocol`） | 仅重量类 |
| 其余 | `good` | 全部 |

关键取舍：**`status`/`unit`/`mode`/`decimals` 这些描述性字段不受称量状态影响**
——无论稳定与否，它们本身都是有效信息，故恒为 `good`（在读数新鲜的前提下）。
只有重量类点位随称量状态降级。掩码（数值段无数字，如 `M-------L`）不是解析错误：
衡器用占位符表达「出错」，此时 `Weight` 无意义但 `Status` 仍然有效——「超量程」
恰恰靠 `Status` 而非数值表达。

#### 3.10.28.5 收帧：CR/LF 分帧 + 首字母从严

连续输出的帧边界只有 CR/LF，**没有长度字段也没有二进制同步字**。这让「怎么从一段
字节流里认出一帧」变成一件必须写对、否则症状极其罕见的事。典型故障：一次连接
上电时衡器打印的欢迎横幅、或另一台设备吐的字节，被当成连续帧的开头，于是此后
每一帧都错位一截。

因此 `codec/toledo.Reader` 强制以「首字节是**大写**状态助记符（`S`/`T`/`U`/`O`/`L`
之一）或 STX(`0x02`)」为帧起点，其余一切丢弃并计数（`Dropped()`）。

> 为什么只认大写：真实连续帧的状态段恒为大写。若把小写也纳入，噪声里任何一个
> `l`/`o`/`s`/`t`/`u`（ASCII 输出里太常见了）都会被当成帧起点，等于没做噪声
> 过滤。`Parse` 仍接受小写状态（为健壮），但**收帧这一层从严**——宁可漏一帧也不
> 误切。

`Dropped()` 持续上报：一条串着多设备的线上它增长说明有别的设备在串话或波特率
对不上；一直是 0 也不代表链路正常，只是说明没收到过东西。

#### 3.10.28.6 明确不做

- **二进制 TOLEDO 连续格式**（`STX` 开头 18 字节：状态字 A/B/C + 6 位重量 + 6 位
  皮重 + `CR` + 校验和）：官方手册有完整记载，但本版只实现 ASCII 三段式。`Parse`
  识别到 `STX` 开头时**返回带可执行指引的错误**（提示切到 ASCII 格式），而不是
  把它当 ASCII 硬解出垃圾值。
- **SICS 命令应答**（`I0`/`IP`/`SIR` 等读命令、去皮/清零写命令）：这是交互式命令
  协议，与「被动收连续输出」是两种工作模式，不在此实现。
- **任何写操作**：衡器只读，`Write` 恒返回 `ErrUnsupported`。
- **单位换算**：单位集合开放（`kg`/`lb`/`g`/`t`/`oz`/`kN`/`lb_oz`…），不做白名单、
  不做换算，由使用方决定。`lb oz`/`lb:oz` 归一为 `lb_oz` 双单位并标记 `DualUnit`。

半成品比不做更难排查：一个只实现 ASCII 的驱动，遇到二进制帧时给出明确指引，比
「硬解出一堆看似合理的错值」有用得多。

#### 3.10.28.7 数值精度：字符串拆分，不在 float64 上取整

连续输出的整数位可能很宽（超大毛重），而 `float64` 尾数只有 15~16 位有效十进制。
若先 `乘 10^n 取整` 再格式化，会在大值上丢精度。因此 `Format`/`splitSigned`
走**字符串**拆分：`strconv.FormatFloat(v, 'f', -1, 64)` 取最短可往返表示，再手动
切分整数/小数段。`Weight` 在 `Parse` 里保存**带符号**的值（符号单独存 `Sign`
供诊断/显示，但驱动取数直接用 `Weight`，避免每处都乘一次符号而漏乘）。

#### 3.10.28.8 错误分类

| 现象 | 分类 | 排查方向 |
|---|---|---|
| 连上后一直没帧 | `ErrTimeout` | 波特率/校验位（常见 9600 8N1，也有 8E1）、接线/供电、衡器是否真的在连续输出 |
| 有字节但一帧都不成 | `ErrProtocol` | 帧格式选错（二进制 vs ASCII）、小数点/逗号分隔符被改、掺了厂商前缀 |
| 字段名不认识 | `ErrAddress` | 用 `-list` 查可用读数字段 |
| 写操作 | `ErrUnsupported` | 衡器只读 |

「有字节回来、一帧都没成」与「根本没收到」必须分开——前者说明衡器在推、只是没
解对（最常见是帧格式选成二进制），后者才是链路断。把它们混为一谈，现场会先去
改波特率而不是查仪表设置。

判据落在 `pump` 维护的两个计数上（`bytesSeen` / `badFrames`，见 §3.10.28.2），
经驱动导出的 `Stats()` 读出，`toledoread -raw` 会把它们连同最近一帧原文一起打印。
因此这条分类**不只**作用于返回的错误——即使 `Read` 按点位降级成 `comm_error` 而没抛
错，运维也能从 `-raw` 里看到「读到 0 字节」还是「读到 N 字节、坏帧 M」，两种故障的
排查方向在输出层面就已分开。

**这两类错误只在 `Probe` 路径作为顶层错误返回，`Read` 路径不返回。** `Read` 是
调度器周期性采集的入口，而本驱动与 MQTT 同为推送型（§3.10.6）：衡器此刻链路是好的，
它要么在持续推字节（帧格式配错）、要么只是暂时安静（衡器没推帧）。把这两种
`ErrProtocol`/`ErrTimeout` 当 `Read` 的顶层错误上抛，会被 collector 按 §3.3.1 的硬
契约 `Close` 拆链 + `consecutive` 熔断退避——一台正常设备就此每周期断链重连、点表里
其它点位跟着不再采集，正是 §3.10.6 点名禁止的「一轮 Read 没有好消息就返回整体错误」。
所以 `Read` 在「一帧都没收到」时一律逐点降级为 `comm_error` 并返回 `nil`，两种故障
的区分交给 `Stats()`；只有 `Probe`（命令行自检 `-probe`、熔断后的探活）才把这两类
作为顶层错误抛出，让运维在没帧时明确失败退出。

#### 3.10.28.9 配置与承载

```json
{
  "transport": { "kind": "tcp", "address": "127.0.0.1:4001" },
  "concurrency": 1,
  "labels": { "options": "staleAfter=30s,readTimeout=10s" }
}
```

| 配置项 | 含义 | 默认 |
|---|---|---|
| `transport.kind` | `serial`（直连）/ `tcp`（串口服务器/DTU 透传） | — |
| `transport.baud` | 波特率 | `9600`（连续输出常见，也有 8E1/8O1） |
| `transport.parity` | 校验 | `none`（标准 8N1；个别 8E1） |
| `labels.options.staleAfter` | 多久没新帧判 stale | `30s` |
| `labels.options.readTimeout` | 后台收帧循环的读超时 | `10s`（须 < staleAfter） |

`readTimeout` 必须**小于** `staleAfter`，否则链路真断时 `Read` 早已把点位判
stale 而收帧循环还没察觉，会掩盖断链。这里做一致性校正（`readTimeout =
staleAfter/2`）而不是报错。TCP 默认端口取 `4001`（串口服务器/DTU 透传常见端口，
不是标准端口——Toledo 没有规定端口）。

#### 3.10.28.10 自检与内置衡器

```bash
./uic sim -protocol toledo -listen 127.0.0.1:4001 &
./uic toledoread -addr 127.0.0.1:4001 -probe                       # 只验链路
./uic toledoread -addr 127.0.0.1:4001                               # 最新一帧全字段
./uic toledoread -addr 127.0.0.1:4001 -points weight,status,stable
./uic toledoread -list                                             # 可用读数字段
```

自检的重点不是「发什么、怎么配」，而是「连上后到底收到了什么帧、解成了什么」。
内置衡器 `internal/sim/toledo` 复用 `codec/toledo.Format` 组帧，连上后按 ~20 帧/秒
持续推 `ST,GS,+00149.0kg` 之类，不发命令、不应答；支持 `SetReading`/`SetWeight`/
`SetStatus`/`SetMode` 改状态、`WithSilent` 不推帧（验证「连着但没数据 →
comm_error」）。衡器写进去的、驱动解出来的共用同一套格式规则，不会出现两边各写
一套而漂移。

#### 3.10.28.11 测试矩阵

| 层 | 用例 |
|---|---|
| `codec/toledo` | D39 金帧 `53542C47532C2B30303134392E306B670D0A` = `ST,GS,+00149.0kg` 逐字节锁死；`Format`→`Parse` 往返一致；`Format` 按 `WEIGHT(7)` 定宽（`00149.0`）；`splitSigned` 字符串拆分不丢精度（含 15 位整数、大值带小数、小数补零/截断）；`splitSigned` 负 `decimals` 不 panic（原实现切片越界）；大值 `Format`→`Parse` 往返（`999999999.5`/`123456789012345`）；负值/无小数点/多位小数变体；`STX` 二进制帧 → 带「ASCII」指引的拒绝，且收帧层把 STX 当合法起点切出（不当噪声丢）；状态 `ST`/`US`/`OL`/`UL`（大小写）；毛净 `GS`/`NT`/`TR`/`GR`（`GR` 是 `GS` 别名）；掩码数值段 → `Masked` 不报错；`lb oz`/`lb:oz` → `lb_oz` + `DualUnit`；段数/状态/模式错误分类；`Reader` 按 CR/LF 分帧、噪声前缀丢弃并计 `Dropped()`、首字母只认大写、半帧多段到达、`MaxFrame` 兜底、EOF 原样返回；`Reader.BytesRead()` 记读入字节数（噪声计入、`Dropped` 计入、尾随 LF 下次才计入） |
| `driver/toledo` | 真 TCP 回环：`Probe` 等首帧；稳定重量 → `weight` good + 值；运动中 → 重量 uncertain、status 仍 good；超量程/欠量程 → 重量 bad、status 仍 good；`stable`/`motion` 布尔点位；`-points` 走驱动 Read 路径；`Write` **逐项** `ErrUnsupported`（顶层不抛错）；register 地址 Open 拒绝；未知 symbol Open 报 `ErrAddress`；`readTimeout` 一致性校正后严格 `< staleAfter`（`staleAfter` 取 100ms/1s/1.5s/2s/2.5s/10s/30s 逐点钉死）；`parseOptions` 越界/非法/未知键忽略；`WithSilent` → 全点位 `comm_error`；**只吐二进制帧的假秤 → `Read` 不抛顶层错误（推送型驱动不触发重连），逐点 `comm_error` 且 `Stats()` 显示「读到字节/有坏帧/未成帧」+ `LastError` 含「ASCII」指引**（等坏帧计数上来再断言，因为首轮 Read 无网络交互）；静默衡器 → `Read` 不抛 `ErrTimeout`、逐点 `comm_error`；`Stats()` 报 `BytesSeen`/`Dropped`/`BadFrames`/`HasFrame`/`Raw`/`FrameAge` |
| `sim/toledo` | 连上按 ~20 帧/秒推帧；`SetStatus`/`SetMode`/`SetWeight`/`SetReading` 后续帧反映新值；`WithSilent` 不推帧；`Close` 幂等 |
| `pointtable` | `toledo` 走 `WidthNone + AllowSymbol`；字段名 `weight`/`status`/… 解析成 symbol 且 `Canonical` 往返一致；寄存器/对象/文件/计算/selector 地址在导入期拒绝；`length` 列在导入期拒绝；秤的字段名不被 modbus-tcp/s7/dlt645/cjt188/bacnet 收下 |
| `cmd/uic` | `toledoRefs` 列拆分；`toledoDefaultType` 与驱动一致；`KnownSymbol` 大小写不敏感、非法字段名（中文/寄存器语法）连秤前拒绝；`printToledoFields` 全字段有说明；`hasFreshFrame` 以「至少一个非 comm_error」为判据（不是样本数>0）；`statsOf` 经可选接口取统计、驱动不实现时安全返回 false；`printToledoRaw` 打印收帧统计 + 原文 + 「读到字节但没成帧」提示；sim 起在 `127.0.0.1:4001` 后 `-probe`/默认/`-points`/`-json`/`-raw`/`-list` |

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

**错误分类（`errOf`）：bad 归 `ErrProtocol`，不归 `ErrInternal`。** 计算点没有
物理链路，运行期一个 bad 结果永远来自「表达式或输入」——除零、越界、无输入、
非数值、引用写错——这是数据/协议语义层失败，与 `ErrProtocol` 同类。`ErrInternal`
在本仓的语义是「实现 bug / 不可恢复的内部错误」且计入熔断（`CountsTowardBreaker`
为 true）；把数据层面失败标成内部错误，会让运维看到「内部错误」去查进程，而不是
查表达式/点表，也违背「结束码必须自带 Kind、不得兜底成 ErrInternal」的纪律。其余
质量码如实映射：`timeout`→`ErrTimeout`、`comm_error`/`not_connected`→
`ErrDisconnected`、`address_error`→`ErrAddress`。注意计算点的 `Read` 在顶层始终返回
`nil`（失败只落在每个样本上），因此这个分类只影响排障展示与告警，不触发调度器熔断。

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
| 段索引上下界取**极值** | `start`/`end` 是段内最小/最大记录时间，不是首条/末条 | 段内时间不单调（多设备交错落盘），末条可能早于首条；把 `end` 写成末条会得到 `end < start`，查询判重叠时整段跳过、窗口内 good 样本被漏统计 |

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
| `Capabilities` 只写「记得的几个」字段 | 漏写的字段序列化成**空串**，既不是 `supported` 也不是 `unsupported`；前端渲染成空白，调用方无法判断该不该依赖它 | 8 个字段**全部显式声明**（`Read`/`Write`/`Subscribe`/`Browse`/`Discovery`/`MultiRead`/`BitAccess`/`StructTypes`）。`CapState` 是 string 枚举，零值 `""` 不是四态之一 |
| 每个驱动只写自己的 `TestMetaContract`，断言它「记得写」的那几项 | 漏写的字段正好不在断言里，全仓测试照样全绿（SL 651 长期 6/8 就是这么漏的） | 除驱动自检外，`internal/runtime` 有一条**全仓**兜底 `TestBuiltinDriversMetaContract`：遍历 `driver.List()`，对每个驱动的 8 个能力位逐个校验落在四态内 |
| 为纯符号协议写带冒号的地址，而符号头撞上别的协议的区名 | 被误判成「本协议不接受寄存器地址」而在导入期拒绝，错误信息把人引向错误方向 | 在 `AddressPolicy.SymbolPrefixes` 里声明本协议占用的前缀（如 M-Bus 的 `q`/`r`），解析器见到即跳过寄存器识别 |
| 在 `driver.E(...)` 上标了 `temporary=false`，重发循环却只看 `KindOf(err)` | 标记形同废纸：本该「不重发」的失败照样重试到上限，每次读表白等 `retries × 超时`，且日志上看不出与「可重发」那类有任何区别（两者 `KindOf` 同为 `ErrProtocol`） | 重发循环在 `KindOf` 判断之后追加 `if !driver.IsRetryable(err) { return err }`；测试要能区分两类：可重发的每轮请求数 = `retries+1`，不重发的每轮 = 1 |
| 构造「帧内容不对」的测试用例时改地址（如 `A=FDh`） | 地址在**分帧**阶段就被拒，Reader 跳过坏帧，最终表现成超时 → 落进「可重发」分支，测不到「不重发」的语义，测试反而变成假绿或误导 | 让帧**成帧且校验和正确**，只把 CI 换成不受支持的值（sim 的 `SetBadCI`），这样才真正走到 `CheckResponse`/`ParseResponse` 的 `ErrFrame` |
| 语义字段缺失时用 `default: return good` 兜底（fail-open） | 无法识别的内容被当成正常数据入库：MTConnect 里 `<BogusLevel>OVERHEAT</BogusLevel>` 会解析成 `quality=good` + `value="OVERHEAT"` + `err=nil`，告警被静默吞掉，且从数据上看毫无异常 | 语义不可判定时判**坏**并给明确诊断。先用测试确认「缺失」是可观测状态（必要时在 codec 加 `LevelKnown()` 一类谓词），再让它 fail-closed |
| 为一个 fail-open 缺陷写回归测试，却用了会先被其他分支拦截的用例 | 测试永远绿：MTConnect 两层写法的未知级别名文本落在外层（为空），被「空值」分支提前判成 `bad`，删掉修复照样通过（变异验证能立刻发现） | 挑**唯一可观测路径**做用例（平铺写法文本非空），并做变异验证：去掉修复后测试必须红 |

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
