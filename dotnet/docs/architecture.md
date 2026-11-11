# 架构

## 一句话

一条单向的数据流水线：**协议字节 ↔ 驱动 → 采集调度 → 点位缓存 → 上报队列 → 北向通道**，
每一段都可以单独替换、单独测试，装配只发生在一个地方（`Uic.Runtime`）。

## 分层

```
                      ┌──────────────────────────────┐
                      │  Uic.Host（进程 / 命令行）    │
                      └───────────────┬──────────────┘
                                      │
                      ┌───────────────▼──────────────┐
                      │  Uic.Runtime（唯一装配点）    │
                      └───┬───────────┬───────────┬──┘
                          │           │           │
        ┌─────────────────▼──┐   ┌────▼─────┐  ┌──▼──────────────┐
        │ Uic.Northbound     │   │ Uic.Outbox│  │ Uic.OpcUa       │
        │ REST/SSE/MQTT      │   │ 分区队列  │  │ 地址空间/订阅   │
        └─────────┬──────────┘   └────▲─────┘  └──▲──────────────┘
                  │                   │           │
        ┌─────────▼───────────────────┴───────────┴──┐
        │  Uic.Engine（调度器 / 设备 actor / 缓存）   │
        └─────────┬──────────────────────────────────┘
                  │
        ┌─────────▼──────────┐   ┌───────────────────┐
        │ Uic.Driver         │◄──│ Uic.Codec.Modbus   │
        │ 驱动 SDK + 注册表   │   │ Uic.Codec.S7       │
        └─────────┬──────────┘   │ Uic.Transport      │
                  │              └───────────────────┘
        ┌─────────▼──────────┐   ┌───────────────────┐
        │ Uic.Driver.Modbus  │   │ Uic.Domain         │
        │ Uic.Driver.S7      │   │ 模型 + 线上契约     │
        │ Uic.Driver.OpcUa   │   └───────────────────┘
        │ Uic.Driver.Virtual │
        │ Uic.Sim.Modbus     │   ← 南向驱动（取数）
        │ Uic.Sim.S7         │
        └────────────────────┘
```

`Uic.Driver.OpcUa` 与 `Uic.OpcUa` 是两条相反的边：前者是南向**客户端**（网关去读别人的
地址空间），位于驱动层、只引用 `Uic.Domain` + `Uic.Driver`；后者是北向**服务端**
（网关把自己的点位暴露出去），位于北向侧、认识 `PointCache` 与调度器。
两者不互相引用——共享的只有同名的协议栈依赖。

`Uic.Codec.Mqtt` 同时服务两侧：北向 MQTT sink 用它发布，南向 `Uic.Driver.Mqtt`
用它订阅。报文实现只有一份，但各自持有独立的客户端实例与生命周期——一个 Broker
抖动不该同时打穿上报和采集。

依赖只向下：下层不认识上层。`Uic.Domain` 不引用任何框架包，
所以同一份模型既能在服务端序列化，也能编译进 WASM 前端。

## 各层的职责边界

| 项目 | 负责 | 明确不负责 |
|---|---|---|
| `Uic.Domain` | 配置模型、地址模型、数据类型、质量码、序列化契约 | 任何 I/O |
| `Uic.Driver` | `IDriver` / `IConn` 接口、驱动元数据、错误分类、协议无关的 `ValueCodec`（字节序 / BCD / 数组） | 具体协议字节 |
| `Uic.Codec.Modbus` | PDU 编解码、CRC/LRC、批量窗口计算 | socket、重试 |
| `Uic.Codec.S7` | TPKT/COTP 分帧、S7 PDU 编解码、区名与字节偏移解析 | socket、会话状态 |
| `Uic.Codec.Mqtt` | MQTT 3.1.1 报文编解码、订阅登记与重发、主题匹配、TLS 装载 | 采集节奏、点位映射 |
| `Uic.Transport` | 字节流（TCP / 串口 / 内存）、超时、分帧 | 业务语义 |
| `Uic.Driver.Modbus` | 主站：连接、探活、读写、异常码映射 | 采集节奏 |
| `Uic.Driver.S7` | 主站：COTP 会话、PDU 协商、按 PDU 切片、位读写 | 采集节奏 |
| `Uic.Driver.OpcUa` | 主站（客户端）：NodeId 寻址、批量读、写、订阅、证书/PKI | 采集节奏、地址空间暴露 |
| `Uic.Driver.Mqtt` | 订阅方：主题寻址、JSON 字段取值、订阅缓存、质量/过期判定 | 采集节奏、南向命令通道、写点位 |
| `Uic.Sim.Modbus` | 从站：寄存器模型、异常注入 | 北向 |
| `Uic.Sim.S7` | 从站：DB/I/Q/M 内存模型、返回码 | 北向 |
| `Uic.Driver.Virtual` | 波形生成、故障注入 | 网络 |
| `Uic.Engine` | 每设备 actor、读规划、加工、死区、缓存 | 序列化、网络出口 |
| `Uic.Outbox` | 落盘队列、分区确认、三重上限、重放 | 认识 MQTT |
| `Uic.Config` | 内容寻址版本库、校验、diff、回滚 | 运行态 |
| `Uic.Obs` | 指标、环形日志、JSON 行文件日志 | 输出格式 |
| `Uic.Northbound` | HTTP 路由、鉴权、限流、SSE、MQTT 客户端 | 采集 |
| `Uic.OpcUa` | 地址空间、订阅推送、写回调度器 | 采集 |
| `Uic.Runtime` | 装配、热更新、优雅停止 | 具体业务逻辑 |

## 数据流

### 南向：一次采集

1. `DeviceActor` 按点位有效周期分桶（点位 `interval` 覆盖设备周期）后唤醒：每轮只读到期的桶，到期的多桶合并成一次读取（调度 tick 50 ms；启动相位按设备内最快点位周期把设备均匀铺开；到期时间用单调时钟，NTP 校时不会打乱节奏）；
2. `ReadPlanner` 把每个周期桶内的点位合并成若干「寄存器窗口」——连续地址合并、按区分离、按协议上限切分。
   符号寻址（OPC UA、MQTT）没有窗口概念，一个点一个 span，由驱动在一轮里合并成一次
   Read 服务调用；
3. `IConn.ReadAsync(spans)` 一次往返拿回整窗；
4. `ModbusCodec` / S7 编解码 / OPC UA `Variant` / MQTT JSON 字段按点位类型与字节序解出 `RawSample`；
   MQTT 没有请求/应答，`ReadAsync` 返回的是驱动内订阅缓存的最近一条报文；
5. `Pipeline.Process` 做缩放/偏移、质量传播、整数类型的取整；
6. `PointCache.PublishConfigured` 按点位的死区模式（绝对值 / 量程百分比 / 变化率）判断是否越过阈值，只有「确实变化」才通知订阅者；
7. `SampleFanout` 把变化序列化成一行 JSON，追加到每个 sink 的分区。

失败路径同样是数据：驱动异常被折成质量码（`timeout` / `comm_error` / `address_error` …），
整轮采集不中断；连续失败达到阈值后设备转 `degraded`，再失败转 `faulted` 并停止采集，
之后按 `probeInterval` 探活，恢复后自动回到 `collecting`。

### 北向：一次上报

队列里存的已经是序列化好的 JSON 行，投递时不再碰模型对象：

```
PointCache ──变化──► SampleFanout ──Enqueue──► Outbox[mqtt]  ──► MqttSink
                                             └─► Outbox[journal] ──► JournalSink（可选）
```

- **整批成功才推进位点**，部分成功不推进：宁可重复，不可丢；
- 每个 sink 独立分区，一个通道挂掉不拖住别的通道；
- 三重上限（条数 / 字节 / TTL）超限时丢最旧的并计数，磁盘不会无限增长；
- TTL 用单调时钟度量「在盘上放了多久」，不用墙钟：一次校时往前跳不会把积压判成过期；
- 掉电只可能丢最后半行，加载时跳过即可；
- 落盘失败（磁盘满 / 目录不可写）只丢这一条并计数，位点不推进，磁盘恢复后补投。

OPC UA 不走队列：它是「读当前值 + 订阅变化」的拉模型，
节点值直接读 `PointCache`，变化由缓存订阅推送。

## 关键设计决策

### 1. 每设备一个 actor，永不并发采集

协议会话有状态：Modbus 事务号、串口半双工、PDU 协商。并发采集是「偶发错乱」的最常见来源。
同一设备的所有请求串行；同一通道的并发上限由 `channel.maxConcurrency` 在 `ChannelGate` 里统一控制。
写请求插队到读之前——现场按「立即执行」比按「先来后到」更符合直觉。

### 2. 配置即制品

配置是内容寻址的（`v<12 位摘要>`），每次提交落成新版本，历史只增不改：

- 改坏了 → 回滚到旧版本，回滚本身也是一次提交；
- 审阅改动 → `diff` 只比较语义内容，不比较格式；
- 重启 → `current` 指针优先于命令行传入的文件，避免「运维回滚过之后又捡回旧文件」；
- 热更新 → 按设备粒度 diff，只重启受影响的设备；
  被删设备的观测状态（上一状态、停滞去重窗口、broker 上的 retained `$state`）一并清理，
  否则「删了又加回来」的设备既不重推状态、也会吞掉第一轮停滞告警。

### 3. 质量码贯穿全链路

`Quality` 区分 `bad`（值不可信）、`timeout`、`comm_error`、`address_error`、`not_connected`、`disabled`。
「配置了但从没采到」返回 `not_connected` 并附 0 占位值，而不是编一个看起来正常的 0。
死区比较只在两侧都是 `good` 时按数值判断；质量跳变本身永远上报。
阈值可以是绝对值、量程百分比或变化率（单位/秒），由点位配置决定，语义见
[configuration.md](configuration.md#死区)。

### 4. 线上契约只有一份

`Uic.Domain.JsonDefaults` 是唯一权威：

- 时长写成 `"500ms"` / `"2m"` / `"1h30m"`，也接受纯数字（毫秒）与 `01:00:00`；
- 枚举一律小写线上名（`uint16` / `comm_error`），数值枚举被明确拒绝；
- 时间统一 UTC + 毫秒 + `Z`；
- 未知字段忽略（前向兼容），未知数据类型名降级为 `uint16`（旧客户端不该被新类型炸掉）。

前端复用同一份模型与选项，所以两端不会出现「各自自测通过、互相读不懂」。

### 5. 驱动 SDK 收窄到能测

`IDriver` 只有 `Meta` 和 `Open`；`IConn` 只有 `Probe` / `Read` / `Write` / `Dispose`。
错误用 `DriverException(Kind, Temporary, …)` 表达，上层据此决定重试还是熔断：
`Temporary` 的（超时、连接断开）按 `devices[].retries` 重试；`address` 类错误既不重试、
也不计入熔断——点表配错不该让整台设备被判定离线。
新增协议只需实现这两个接口并在 `DriverRegistry` 注册，不需要改调度、北向或 UI。

### 6. 一个进程，两种形态

`uic run` 是生产形态；`uic sim` 在同一进程里额外起 Modbus 从站与 S7 从站，
于是「主站 → TCP → 从站」的完整链路可以无硬件跑通——这条路径同时被测试套件复用。
两种从站各有一套内存模型：Modbus 是寄存器数组、S7 是字节数组 + 位地址，
合成一个会让两边的点位互相迁就。

## 运行时状态机

```
Configured ──start──► Connecting ──probe ok──► Collecting
                          │  ▲                    │
                    probe fail│             连续失败
                          ▼  │                    ▼
                       Faulted ◄────────────── Degraded
```

- `Configured` / `Disabled`：配置态与运行态分离，「配置了但没采」能被看见；
- `Faulted` 只探活不采集，恢复后自动回到 `Collecting`；
- 进 `Degraded` 的门槛是「连续通信失败」；`address` 类错误（点表地址不存在）
  不计入这个计数，设备保持 `Collecting`，错误只体现在点位质量上；
- 每次重启设备 actor 递增 `generation`，旧 actor 的迟到回调按代次丢弃，
  避免「旧连接的超时把新连接的状态改坏」。

## 观测

- `/api/v1/metrics` 输出 Prometheus 文本：采集次数、失败按错误分类、样本发布、队列积压与丢弃；
- `/api/v1/logs` 读环形缓冲（默认 2000 条），支持按级别、分类、设备过滤；
- `/api/v1/diagnostics/bundle` 打包配置、日志、指标与环境信息，便于贴工单；
- 启动顺序固定：**先读配置（失败就别启动）→ 起采集 → 起北向**，
  「配置说 enabled 却没监听」比直接退出更难排查。
