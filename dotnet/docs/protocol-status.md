# 协议实现现状

这份文档回答一个问题：**列出来的协议，这个版本到底做到哪一步了。**
「做到了」按代码与测试说话，不按文档承诺说话；没做的部分写清楚是「产品明确不做」
还是「本版本没做完」，两者的补法完全不同。

状态口径：

| 状态 | 含义 |
|---|---|
| **已实现** | 代码 + 黄金帧/端到端用例都在，能按文档配置直接跑 |
| **部分实现** | 主路径可用，但有明确的缺口（下面逐条列） |
| **仅服务端** | 只实现服务端角色，客户端（取数）没有 |
| **未实现** | 没有代码；给出扩展点与工作量级 |
| **不适合本架构** | 不是「没排期」，而是与「请求/响应 + 点位轮询」的模型冲突，需要独立进程或专用硬件 |

## 1. 总览

| 协议 | 角色 | 状态 | 代码位置 | 对应需求 |
|---|---|---|---|---|
| Modbus TCP | 主站 | 已实现 | `Uic.Driver.Modbus` | FR-COL-01 |
| Modbus RTU | 主站 | 已实现 | `Uic.Driver.Modbus` + `Uic.Transport` | FR-COL-02 |
| Modbus ASCII | 主站 | 已实现 | `Uic.Driver.Modbus` + `Uic.Transport` | FR-COL-02 |
| Modbus TCP | 从站 | 已实现（仅仿真） | `Uic.Sim.Modbus` | FR-COL-03 |
| Modbus RTU/ASCII | 从站 | 未实现 | — | FR-COL-03 的延伸 |
| 虚拟设备 | 主站侧 | 已实现 | `Uic.Driver.Virtual` | FR-COL-07 |
| OPC UA | 服务端 | 已实现（匿名 + None） | `Uic.OpcUa` | 北向 FR-NOR |
| OPC UA | 客户端 | **未实现** | — | FR-COL-05（M，首版缺口） |
| MQTT | 北向发布 | 已实现（3.1.1） | `Uic.Northbound` | FR-NOR-04 |
| MQTT | 北向下行命令 | 已实现（白名单） | `Uic.Northbound/MqttCommandSink.cs` | FR-OPS 远程命令 |
| MQTT | 南向订阅取数 | **未实现** | — | FR-COL-06（M，首版缺口） |
| REST + SSE | 北向 | 已实现 | `Uic.Northbound/RestApi*.cs` | FR-NOR-01..03 |
| S7comm（西门子） | 主站 | **未实现** | — | FR-COL-04（M，首版缺口） |
| MC 3E（三菱） | 主站 | 未实现 | — | Wave 2 |
| FINS（欧姆龙） | 主站 | 未实现 | — | Wave 2 |
| EtherNet/IP + CIP（罗克韦尔/AB） | 主站 | 未实现 | — | Wave 2 |
| BACnet/IP | 主站 | 未实现 | — | PRD §6.1 明确不做 |
| IEC 60870-5-104 | 主站 | 未实现 | — | PRD §6.1 明确不做 |
| IEC 61850 | 客户端 | 未实现 | — | Wave 3 |
| PROFINET IO | 主站/从站 | 不适合本架构 | — | 全局明确不做 |
| EtherCAT | 主站 | 不适合本架构 | — | 全局明确不做 |
| GB/T 28181 | 客户端 | 不适合本架构 | — | 全局明确不做 |

先给结论：

- **能用的只有 Modbus 三个变体（主站）+ Modbus TCP 从站 + 虚拟设备**，
  北向是 REST/SSE/MQTT/OPC UA 服务端。这条链路是完整的，带黄金帧和端到端用例。
- **S7 与 OPC UA 客户端是 PRD 里的 M 级需求，本版本没做**。这不是 PRD §6.1
  「不做」清单里的项，属于真实缺口（见 [requirements-traceability.md](requirements-traceability.md)）。
- **BACnet、IEC 104、CIP、MC、FINS 是 PRD §6.1 明确不做**，属于产品范围决定。
- **PROFINET、EtherCAT、GB/T 28181 不在协议路线图内**，原因是它们与轮询式采集模型
  冲突或需要专用硬件，不是「以后补个驱动」的事（见 §4）。

## 2. Modbus：唯一做透的协议族

### 2.1 主站（TCP / RTU / ASCII）

三个驱动共用同一份 PDU 编解码（`Uic.Codec.Modbus`），差异只在分帧：

| 能力 | 实现情况 |
|---|---|
| 功能码 | FC 1/2/3/4/5/6/15/16（读线圈、读离散输入、读保持/输入寄存器、写单线圈/单寄存器、写多线圈/多寄存器） |
| 区域 | 保持寄存器、输入寄存器、线圈、离散输入 |
| 位访问 | 支持位/字混读，按区域自动拆窗 |
| 批量窗口 | 读寄存器 ≤125、写寄存器 ≤123、读位 ≤2000、写位 ≤1968（协议上限），相邻地址自动合并，跨空洞不合并 |
| 异常码 | 01/02/03/04 映射到质量码与错误文本，不把异常响应当成功 |
| 字节序 | ABCD / CDAB / BADC / DCBA，点位级可覆盖设备级 |
| 数据类型 | bool、有/无符号 16/32/64、float32/64、string、BCD16/32、bytes、datetime（4 寄存器 int64 Unix 毫秒 UTC） |
| TCP | MBAP + 事务号校验；事务号不匹配直接丢弃（防止把上一次超时的迟到响应当成新结果） |
| RTU | CRC16；帧边界按功能码/字节数推导；发送前清空输入缓冲；3.5 字符帧间静默；同一串口半双工串行 |
| ASCII | `:` 起始、LRC 校验、CRLF 结束 |
| 串口参数 | 端口、300–921600 波特率、数据位、`none/odd/even/mark/space` 校验、`one/onePointFive/two` 停止位、读超时 |
| 超时与重试 | 设备级 `timeout` + `retries`（只重试临时性失败）；重试前断开重连 |
| 探活 | 连接建立时读一个保持寄存器，确认链路与会话都可用 |

帧间静默由 `ModbusSerialTiming` 计算：3.5 个字符时间（每字符 11 位），
9600 bps 约 4 ms；>19200 bps 按规范固定 1.75 ms。计时挂在物理串口上，
同一根 RS-485 上多台从站交替发请求时也共享这段静默；超时后同样重新计时，
避免迟到响应撞进下一次事务。帧边界仍由长度字段/功能码与 CRC 判定。

测试证据：`golden/*`（8 份 Modbus 黄金帧）、`codec/*`、`planner/*` 八条、
`serial/rtu_golden_frame_over_pty`、`serial/ascii_golden_frame_over_pty`、
`serial/half_duplex_does_not_cross_responses`、`serial/rtu_waits_between_frames`、`loopback/*`。

### 2.2 从站（仿真）

`Uic.Sim.Modbus` 是一个**只用于自测与替代设备**的 TCP 从站，`uic sim` 启动：

| 能力 | 实现情况 |
|---|---|
| 传输 | 仅 TCP（MBAP） |
| 功能码 | 1/2/3/4/5/6/15/16，其余返回异常码 01 |
| 地址空间 | 保持/输入寄存器各 65536 字；线圈/离散输入初始 8192 点，按需扩容 |
| 并发 | 单锁串行处理 PDU，避免半更新 |
| 单元号 | 按请求回显，不做多单元路由 |

从站的 PDU 处理器（`ModbusSlaveHandler`）与分帧无关，所以补 RTU/ASCII 从站
只需要加一层 framing，不需要重写语义。

## 3. 北向：只做「网关 → 上位机」，其中只有 OPC UA 是双向的

### 3.1 REST + SSE

已实现且是控制台唯一依赖的通道：设备/点位查询、批量与单点写入、配置
validate/apply/versions/diff/rollback/export/import、模板 CRUD 与批量实例化、
点表 CSV 导入、命令执行、指标/健康/日志/诊断包、SSE 事件流。
契约见 [northbound-api.md](northbound-api.md)。

### 3.2 MQTT

只实现 **MQTT 3.1.1 客户端**，角色是发布者（北向）：

| 能力 | 实现情况 |
|---|---|
| 版本 | 3.1.1（CONNECT/PUBLISH/SUBSCRIBE/PINGREQ 等），不支持 MQTT 5 属性 |
| QoS | 0 / 1；未实现 QoS2 |
| 保留消息 | `$birth`、`$state` 用 retained；`$death` 由 broker 代发遗嘱 |
| 主题 | 前缀 + `{site}` + `{device}` + `{point}` 模板；`$birth`/`$state`/`$death` 固定后缀 |
| 重连 | 固定退避（≥500 ms，可配）；重连后重发 `$birth` 与全部 `$state`，并重订阅 |
| 下行命令 | 请求/结果双主题，按 `id` 做 5 分钟进程内幂等；只放行 `driver.restart`、`device.selftest`、`outbox.flush`、`log.level`、`diag.bundle` |
| 传输 | 明文 TCP；TLS/WebSocket 未内置 |

「MQTT 南向订阅取数」（FR-COL-06）没有实现：北向 MQTT 的连接是「发出去 + 收命令」，
南向需要另一套点位映射、生命周期与质量传播，不能复用同一个客户端实例。

### 3.3 OPC UA 服务端

| 能力 | 实现情况 |
|---|---|
| 角色 | 服务端（地址空间提供者） |
| 传输 | `opc.tcp://`，UA TCP |
| 安全 | 仅匿名 + `SecurityPolicy.None`；配置成其它策略直接拒绝启动 |
| 地址空间 | `UIC/<site>/<device>/<point>`，设备节点带类型定义，点位是 `BaseDataVariableState` |
| 读 | 标准浏览 + 读；值来自 `PointCache` |
| 写 | 点位可写时开放 `CurrentWrite`；写入走 `Scheduler.WriteAsync`，与 REST 写入同一条路径 |
| 订阅 | 由标准服务端栈提供 MonitoredItem/Subscription |
| 审计 | 写入次数/失败次数计入服务端指标 |

**OPC UA 客户端**（FR-COL-05）没有实现，也没有半成品：会话建立、重连、
MonitoredItem 生命周期、证书信任链都要另写一套。北向服务端的地址空间模型
不能直接反用成客户端，所以不能靠「把服务端代码翻过来」省事。

## 4. 没实现的协议：分三类，不要混为一谈

### 4.1 首版缺口（PRD 要求 M，但本版本没做）

| 协议 | 缺什么 | 补的时候要动哪里 |
|---|---|---|
| **S7comm 主站** | 全部。TPKT/COTP 分帧、S7 PDU 协商（240/480/960）、DB/IB/QB/MB 区、位地址、S7-1200 的 PUT/GET 语义 | 新增 `Uic.Driver.S7`：`IDriver`/`IConn` + 自己的分帧；`Address` 已有 symbol/register 字段可用；按「加一个协议」五步接入 |
| **OPC UA 客户端** | 全部。会话、订阅、证书、重连、点位映射 | 新增 `Uic.Driver.OpcUa`，建议直接用 `Opc.Ua.Client` 而不是手写二进制栈；`Capabilities.Subscribe` 放开为 `Supported` |
| **MQTT 南向取数** | 全部。订阅主题 → 点位映射、断线重订阅、质量传播 | 新增 `Uic.Driver.Mqtt`；连接生命周期要与北向 MQTT 隔离，避免一个 broker 抖动同时打穿两侧 |

这三项是**真实的验收缺口**。当前版本能进现场做的只有 Modbus + OPC UA 服务端/北向，
不能说「PRD 的首版范围已经闭环」。

### 4.2 PRD §6.1 明确不做（产品范围决定）

| 协议 | 计划 | 依据 |
|---|---|---|
| BACnet/IP | Wave 2 | `docs/03-product/07-roadmap.md` |
| IEC 60870-5-104 | Wave 2 | 同上 |
| EtherNet/IP + CIP | Wave 2 | 同上 |
| MC 3E（三菱） | Wave 2 | 同上 |
| FINS（欧姆龙） | Wave 2 | 同上 |

这些协议的扩展点与 S7 相同：一个驱动包 + 黄金帧 + 一致性用例，不需要改调度、
北向或 UI。差别只在协议本身的复杂度（CIP 的标签/结构体、BACnet 的对象模型、
104 的遥测/遥信分类）。

### 4.3 不适合本架构（不是排期问题）

| 协议 | 为什么不适合 | 如果客户硬要 |
|---|---|---|
| **PROFINET IO** | 走以太网二层（EtherType 0x8892），需要原始套接字、实时优先级、GSDML 组态；IRT 还依赖专用网卡。本项目的模型是「IP 之上的请求/响应轮询」，套不进去 | 用支持 PROFINET 的网关/主站卡做协议转换，本产品只采转换后的 Modbus/OPC UA |
| **EtherCAT** | 主站需要实时网卡与分布时钟同步，周期是微秒级；它不是「读一个地址」的协议，而是过程数据镜像 | 同上：由专用主站把过程数据映射成 Modbus/OPC UA |
| **GB/T 28181** | 是视频监控的 SIP 信令 + RTP 媒体流，不是遥测轮询；要处理注册、心跳、目录查询、实时点播、录像回放，和「点位 + 质量码」的数据模型没有交集 | 单独的视频接入服务，本产品只做设备在线状态/告警的北向汇聚 |

把这三类硬塞进 `IDriver` 会逼着驱动接口长出「流」「订阅」「二层帧」等概念，
最后每个驱动都只能用其中一小部分——这正是本项目要避免的「宽接口」。

## 5. 加一个协议要做什么

以 S7 为例，最小闭环是五步（细节见 [implementation-notes.md](implementation-notes.md)）：

1. **地址表达式**：`Address` 已支持 register/symbol 两类字段，先确认目标协议的寻址能落在模型里；
   落不进去就先改模型，不要先写驱动。
2. **分帧层**：把「收一条完整报文」抽成独立类型（参考 `Uic.Codec.Modbus/Frames.cs`），
   驱动只面对完整 PDU。
3. **编解码 + 黄金帧**：每个功能码至少一份请求/响应黄金帧，异常码也要有；
   `testdata/golden/*.json` 是验收门槛，不是可选文档。
4. **`IDriver` / `IConn`**：`Meta` 声明角色、传输、地址类型与能力四态；
   `Probe` 负责探活，`Read`/`Write` 只做协议语义，不做调度。
5. **注册 + 用例**：在 `Program.cs` 注册驱动，补一致性用例（T1–T18 中适用的部分）
   与一条端到端回环（有仿真的协议优先用仿真）。

## 6. 怎么核对这份文档

- 代码层：`src/Uic.Driver.*`、`src/Uic.Sim.*`、`src/Uic.OpcUa`、`src/Uic.Northbound`；
  `Program.cs` 里 `drivers.Register(...)` 是驱动清单的唯一事实来源。
- 需求层：[requirements-traceability.md](requirements-traceability.md) 的 FR-COL 表逐条标注了
  「已实现 / 部分实现 / 刻意没做」及证据用例。
- 测试层：`dotnet run --project tests/Uic.Tests -- --list` 能看到全部用例名；
  `docs/testing.md` 有分组说明。
