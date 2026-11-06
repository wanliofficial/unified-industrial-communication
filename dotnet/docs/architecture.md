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
        │ 驱动 SDK + 注册表   │   │ Uic.Transport      │
        └─────────┬──────────┘   └───────────────────┘
                  │
        ┌─────────▼──────────┐   ┌───────────────────┐
        │ Uic.Driver.Modbus  │   │ Uic.Domain         │
        │ Uic.Driver.Virtual │   │ 模型 + 线上契约     │
        │ Uic.Sim.Modbus     │   └───────────────────┘
        └────────────────────┘
```

依赖只向下：下层不认识上层。`Uic.Domain` 不引用任何框架包，
所以同一份模型既能在服务端序列化，也能编译进 WASM 前端。

## 各层的职责边界

| 项目 | 负责 | 明确不负责 |
|---|---|---|
| `Uic.Domain` | 配置模型、地址模型、数据类型、质量码、序列化契约 | 任何 I/O |
| `Uic.Driver` | `IDriver` / `IConn` 接口、驱动元数据、错误分类 | 具体协议字节 |
| `Uic.Codec.Modbus` | PDU 编解码、CRC/LRC、字节序、批量窗口计算 | socket、重试 |
| `Uic.Transport` | 字节流（TCP / 串口 / 内存）、超时、分帧 | 业务语义 |
| `Uic.Driver.Modbus` | 主站：连接、探活、读写、异常码映射 | 采集节奏 |
| `Uic.Sim.Modbus` | 从站：寄存器模型、异常注入 | 北向 |
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

1. `DeviceActor` 按 `device.interval` 唤醒（调度 tick 50 ms；启动相位把同周期设备均匀铺开；到期时间用单调时钟，NTP 校时不会打乱节奏）；
2. `ReadPlanner` 把点位合并成若干「寄存器窗口」——连续地址合并、按区分离、按协议上限切分；
3. `IConn.ReadAsync(spans)` 一次往返拿回整窗；
4. `ModbusCodec` 按点位类型与字节序解出 `RawSample`；
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
- 掉电只可能丢最后半行，加载时跳过即可。

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
- 热更新 → 按设备粒度 diff，只重启受影响的设备。

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

`uic run` 是生产形态；`uic sim` 在同一进程里额外起一个 Modbus 从站，
于是「主站 → TCP → 从站」的完整链路可以无硬件跑通——这条路径同时被测试套件复用。

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
