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
| OPC UA | 客户端 | 已实现（None / Basic256Sha256） | `Uic.Driver.OpcUa` | FR-COL-05 |
| MQTT | 北向发布 | 已实现（3.1.1） | `Uic.Northbound` | FR-NOR-04 |
| MQTT | 北向下行命令 | 已实现（白名单） | `Uic.Northbound/MqttCommandSink.cs` | FR-OPS 远程命令 |
| MQTT | 南向订阅取数 | 已实现（只读） | `Uic.Codec.Mqtt` + `Uic.Driver.Mqtt` | FR-COL-06 |
| REST + SSE | 北向 | 已实现 | `Uic.Northbound/RestApi*.cs` | FR-NOR-01..03 |
| S7comm（西门子） | 主站 | 已实现 | `Uic.Codec.S7` + `Uic.Driver.S7` | FR-COL-04 |
| S7comm | 从站 | 已实现（仅仿真） | `Uic.Sim.S7` | FR-COL-04 的自测 |
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

- **能用的是 Modbus 三个变体（主站）+ Modbus TCP 从站 + S7 主站 + OPC UA 客户端 +
  MQTT 订阅方 + 虚拟设备**，北向是 REST/SSE/MQTT/OPC UA 服务端。这条链路是完整的，
  带黄金帧和端到端用例。
- **MQTT 南向（FR-COL-06）已实现**：主题过滤器寻址、JSON 字段取值、QoS0/1、TLS、
  断线重订阅。采集主路径仍是调度器轮询，`ReadAsync` 读的是驱动内缓存（见 §3.5）。
- OPC UA 客户端有两处**已验证边界**，不要当成「全都能接」：订阅没有接入采集主路径
  （仍是轮询），Basic256Sha256 没有仓内端到端（见 §3.4）。
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
| 写回显 | FC05/06/16 的响应必须与请求逐字节一致（地址、值或数量）；不一致按协议错处理，不报「写入成功」 |
| 字节序 | ABCD / CDAB / BADC / DCBA，点位级可覆盖设备级；只影响 32/64 位数值，`string`/`bytes` 按原样字节流读写 |
| 数据类型 | bool、有/无符号 16/32/64、float32/64、string、BCD16/32、bytes、datetime（4 寄存器 int64 Unix 毫秒 UTC） |
| 数组 | 定宽数值类型可配 `count`（元素个数）成定长数组；逐元素套字节序与缩放，单点位上限 125 寄存器 |
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

测试证据：`golden/*`（20 份 Modbus 黄金帧，含 4 份数组帧）、`codec/*`、`planner/*` 八条、
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

## 2.3 S7comm 主站

`Uic.Codec.S7` 只做纯编解码（TPKT / COTP / S7 PDU），`Uic.Driver.S7` 管会话；
分层与 Modbus 一致，所以抓包排查的路径认得出来。

| 能力 | 实现情况 |
|---|---|
| 传输 | ISO-on-TCP（TPKT 4 字节头 + COTP DT），仅 TCP；端口默认 102 |
| 会话 | COTP 连接请求/确认 → Setup Communication 协商 PDU；两者都在连接建立时完成，之后每个 Job 带 PDU 引用号 |
| PDU 协商 | 请求 480，以 PLC 回应为准（240/480/960）。协商值决定**读请求自动切片**：请求项 12 字节 + 响应项 `4+data+偶对齐` 都计入预算。参数头里的项数字段只有**一字节**，因此编解码层另有一条硬上限 255 项（超过当场报 `protocol`，不截断） |
| 区域 | `db<N>` / `i`(`e`) / `q`(`a`) / `m`(`merker`) / `l` / `p`(`pe`) / `c` / `t`；DB 号嵌在区名里 |
| 寻址 | `address.offset` 是**字节偏移**（不是寄存器序号），上限 2 097 151（3 字节位偏移字段）；`address.bit` 限 0..7；bool 点位必须带 bit |
| 读写 | Read Var / Write Var，位访问用 BIT 传输尺寸；响应里的长度字段按传输尺寸解释（0x03–0x07 是**位**，0x09 是字节）；写输入区由 PLC 拒绝 |
| 返回码 | `0x05` 越界 / `0x0A` 对象不存在 → `address_error`（不参与熔断）；`0x03` → `disabled`；其余 `bad` |
| 数据类型 | 与 Modbus 共用 `Uic.Driver/ValueCodec.cs`：字节序四位、有/无符号 16/32/64、float32/64、bool、BCD16/32、string、bytes、定长数组 |
| 探活 | 建会话 + 读 `MB0` 一个字节；只判「TCP 通」会把握手失败当成可用 |
| 明确不支持 | 符号寻址（0xB2）、S7Plus、TPDU 分片重组、一条链路上的多连接并发；都明确报错，不静默降级 |

**为什么 offset 是字节而不是寄存器**：S7 的地址本来就是 `DB1.DBW4` 这种字节+位，
`DBW3` 这种奇数字节偏移在现场很常见。按寄存器组织地址会在每个奇地址上做二次换算，
那正是把 Modbus 思维搬过来最容易错的地方。

从站 `Uic.Sim.S7` 与主站共用一个进程也能跑：`uic sim` 默认在 127.0.0.1:15102 起
S7 从站，示例配置里的 `s7-1` 设备直连它（DB1 放温度/压力/计数，M0.0 放运行状态）。

测试证据：`s7/*`（16 条黄金帧与地址解析 + 14 条主站↔从站回环，含 PDU 切片、
位写只清一位、越界落 `address_error`、沉默 PLC 超时、断线重连）。

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
| 设备下线 | 清 `$state` 的 retained（空载荷 + retain），并从重连补发集合中移除 |
| 下行命令 | 请求/结果双主题，按 `id` 做 5 分钟进程内幂等（按单调时钟计时，校时跳变不影响窗口）；只放行 `driver.restart`、`device.selftest`、`outbox.flush`、`log.level`、`diag.bundle` |
| 传输 | 明文 TCP 或 TLS（`northbound.mqtt.tls`，校验服务端证书链与主机名，支持自定义 CA）；WebSocket 未内置 |

MQTT 3.1.1 的报文实现放在协议层 `Uic.Codec.Mqtt`：北向发布与南向订阅各持有一个
客户端实例，生命周期互不相干（北向 sink 随配置重载重建，南向连接跟着设备走）。
南向取数见 §3.5；两侧共用同一份 CONNECT/PUBLISH/SUBSCRIBE 编码，行为不会漂移。

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

### 3.4 OPC UA 客户端

| 能力 | 实现情况 |
|---|---|
| 角色 | 客户端（网关去读 PLC/SCADA 的 UA 服务端） |
| 协议栈 | `OPCFoundation.NetStandard.Opc.Ua.Client`（官方栈），不手写二进制层 |
| 传输 | `opc.tcp://`，端点由通道 `host`/`port`（默认 4840）+ `labels.endpointPath` 拼出 |
| 寻址 | 符号寻址：`address.path` 写 NodeId 文本（`ns=2;s=Demo.Temp` / `ns=2;i=85`）；省略 `ns=` 时用 `address.index` |
| 安全 | `labels.securityPolicy` = `None` / `Basic256Sha256`，`labels.securityMode` = `None` / `Sign` / `SignAndEncrypt`；**端点按配置精确匹配，找不到就报错并列出服务端实际档位，绝不静默降级** |
| 身份 | 匿名（`UserIdentity` 空实例）。用户名/口令链路没有接进驱动，配置了也不会试图登录 |
| 读 | 一轮的点位合并成一次 Read 服务调用，按服务端 `MaxNodesPerRead` 分片；地址写错的点位单独落 `address_error`，不拖累同批其他点 |
| 写 | Write 服务，逐点裁决；越界/类型不符在**发到线上之前**就被拒绝 |
| 订阅 | MonitoredItem + Subscription，通过 `Uic.Driver` 的可选接口 `ISubscriptionConn` 暴露 |
| 证书 | PKI 默认落在 `<dataDir>/opcua-client-pki`（`labels.pkiDir` 可覆盖，相对路径按数据目录解析）；首次连接自动生成客户端自签证书 |

**两处必须说清的边界**：

1. **订阅没有接入采集主路径。** `ISubscriptionConn` 已实现并通过回环用例，
   但 `DeviceActor` 仍然按周期轮询——与 Modbus/S7 完全一致。把订阅接进去要另做
   「推送驱动的重试/熔断/代次隔离」，以及没有周期可依据时的看门狗语义，
   属于独立的调度改造。所以现在的 `Capabilities.Subscribe = supported` 说的是
   「这个驱动能提供订阅」，不是「采集走订阅」。
2. **Basic256Sha256 没有仓内端到端。** 内置服务端（`Uic.OpcUa`）只提供
   `SecurityPolicy.None`（配置校验就拒绝其它策略），因此无法在仓库内跑通一条
   加密会话。已覆盖的部分：策略名/模式校验、`SelectEndpoint` 的精确匹配与
   失败信息（`opcua/endpoint_selection_never_downgrades`），以及真连一次
   「配置要 Basic256Sha256、服务端只有 None」必须显式失败
   （`opcua/client_secure_policy_fails_without_match`）。**首次上真机时必须单独验一次
   证书信任链与加密会话**，别拿这套用例当加密已验收。

**明确不支持并显式报错**：`nsu=` / `svr=`（服务端作用域命名空间）、
Basic128Rsa15/Aes 系列策略、结构体/ExtensionObject 类型的值、BCD 类型。

### 3.5 MQTT 南向订阅（只读）

| 能力 | 实现情况 |
|---|---|
| 角色 | 订阅方（网关从 Broker 取现场数据） |
| 协议栈 | `Uic.Codec.Mqtt`（与北向发布共用一份 3.1.1 实现），驱动在 `Uic.Driver.Mqtt` |
| 传输 | 明文 TCP（默认 1883）或 TLS（`labels.tlsEnabled` / `tlsCaFile` / `tlsInsecureSkipVerify`） |
| 寻址 | 符号寻址：`address.path` 是主题过滤器（可含 `+` / `#`）；`address.property` 是载荷里的 JSON 字段路径 |
| 取值 | 字段名（`data.temp`）或 JSON 指针（`/data/temp`）；设备标签 `payloadField` 提供默认值（默认 `value`）；`address.property` 逐点覆盖 |
| 载荷 | JSON 标量/对象/数组；北向 Sample JSON 的 `{"type","num"}` 值信封会先解包再按本地点表类型转换，网关可级联 |
| QoS | 0 / 1；QoS2 不支持 |
| 重连 | 协议层自动重连并重发 SUBSCRIBE（clean session）；起始间隔可配（≥500 ms），指数退避上限 30 s |
| 质量 | 从未收到报文 / 超过 `labels.staleAfterSeconds` 未更新 → `stale`；非 JSON → `bad`；字段缺失或类型不符 → `address_error` |
| 写入 | 不支持（`Capabilities.Write = unsupported`）；南向命令通道不在本版范围 |

**边界必须说清**：

1. **采集路径是「订阅 + 缓存」，不是推送驱动。** 驱动在内部维持订阅，`ReadAsync`
   返回最近一条报文的解析结果；调度器仍按周期轮询。这样不需要改 `DeviceActor`，
   代价是缓存值会被反复采样（死区与 Outbox 的既有语义照常生效）。
2. **首次读到值之前是 `stale`。** `ProbeAsync` 只验证「连上 Broker + 订阅未被 ACL
   拒绝」，不等数据——发布方可能本来就没上线。没有数据时点位报 `stale` 而不是
   `good` 的旧值。连续多轮 `stale` 会计入设备熔断（与其它坏质量一致），这是刻意的：
   一台「连着但永远没数据」的设备应当进入降级而不是看起来正常。
3. **不支持用户名/口令。** 本版本的凭据链路只覆盖北向，南向设备标签是明文落盘的；
   配了 `labels.username` / `labels.password` 会显式报 `unsupported`，而不是静默忽略。
   需要认证时用 TLS + Broker 侧网络 ACL（与 OPC UA 的匿名身份策略一致）。
4. **不支持 QoS2、持久会话、客户端证书、WebSocket。** 需要这些语义的部署应当把
   Broker 的能力边界写清楚，而不是期待驱动「顺手」兼容。

## 4. 没实现的协议：分三类，不要混为一谈

### 4.1 剩余延期项（不是 M 级采集缺口）

**PRD 中 M 级的采集协议缺口已全部补齐**：Modbus、S7、OPC UA 客户端与 MQTT 南向
订阅都已落地并有可自动跑的用例。剩余延期项（非 M 级采集缺口，逐条记录在
[requirements-traceability.md](requirements-traceability.md)）：

| 项 | 状态 | 说明 |
|---|---|---|
| object / file 寻址的驱动 | 模型已就绪，驱动未做 | 四类寻址的模型与校验已闭环（`config/four_address_kinds_validate`），symbol 由 OPC UA 与 MQTT 消费；object 要等 BACnet/CIP 的对象模型，file 要等带文件记录语义的协议 |

故障注入矩阵的 12 类已全部有可自动跑的用例（磁盘满、时钟跳变、进程假死用真实注入点
补齐，见 [requirements-traceability.md](requirements-traceability.md) §7），因此不再列在此表。

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

以已落地的 S7 为参照，最小闭环是五步（细节见 [implementation-notes.md](implementation-notes.md)）：

1. **地址表达式**：`Address` 已支持 register/symbol 两类字段，先确认目标协议的寻址能落在模型里；
   落不进去就先改模型，不要先写驱动。S7 复用了 `register`（`area` 写 `db1`），没有新增字段。
2. **分帧层**：把「收一条完整报文」抽成独立类型（参考 `Uic.Codec.S7/S7Frames.cs`、`Uic.Codec.Modbus/Frames.cs`），
   驱动只面对完整 PDU。
3. **编解码 + 黄金帧**：每个功能码至少一份请求/响应黄金帧，异常码也要有；
   `testdata/golden/*.json` 是验收门槛，不是可选文档。S7 的黄金帧是手算十六进制写在
   `tests/Uic.Tests/S7/S7CodecTests.cs` 里的（它依赖运行时构造的字节，不适合静态 JSON）。
4. **`IDriver` / `IConn`**：`Meta` 声明角色、传输、地址类型与能力四态；
   `Probe` 负责探活，`Read`/`Write` 只做协议语义，不做调度。
5. **注册 + 用例**：在 `Program.cs` 注册驱动，补一致性用例（T1–T18 中适用的部分）
   与一条端到端回环（有仿真的协议优先用仿真）。S7 的四步是
   `Uic.Codec.S7` + `Uic.Driver.S7` + `Uic.Sim.S7` + `s7/*` 用例。

## 6. 怎么核对这份文档

- 代码层：`src/Uic.Driver.*`、`src/Uic.Sim.*`、`src/Uic.OpcUa`、`src/Uic.Northbound`；
  `Program.cs` 里 `drivers.Register(...)` 是驱动清单的唯一事实来源。
- 需求层：[requirements-traceability.md](requirements-traceability.md) 的 FR-COL 表逐条标注了
  「已实现 / 部分实现 / 刻意没做」及证据用例。
- 测试层：`dotnet run --project tests/Uic.Tests -- --list` 能看到全部用例名；
  `docs/testing.md` 有分组说明。
