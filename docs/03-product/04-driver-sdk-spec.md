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
| `s7` | supported | supported | unsupported | runtime_dependent | unsupported | supported | supported |
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
```

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
