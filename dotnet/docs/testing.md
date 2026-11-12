# 测试

## 怎么跑

测试是一个自包含控制台程序，不引 xUnit、不引 Test SDK —— 离网构建机上 `dotnet run` 就能跑，
不需要还原任何测试框架。

```bash
dotnet run --project tests/Uic.Tests                        # 全部用例
dotnet run --project tests/Uic.Tests -- --list              # 列出用例名
dotnet run --project tests/Uic.Tests -- --filter planner    # 子串过滤
dotnet run --project tests/Uic.Tests -- --opcua             # 额外跑 OPC UA 端到端验收
dotnet run --project tests/Uic.Tests -- --stop-on-fail      # 首个失败即停
```

正常输出：

```
310 passed, 0 failed, 3 skipped · 113.5s
```

加 `--opcua` 跑全量（含 3 条 OPC UA 端到端）：

```
313 passed, 0 failed, 0 skipped · 131.2s
```

耗时随机器负载波动较大：219 条时在同一台机器上空载量到 65.5 s，233 条在同时跑着
发布/容器时量到 114.2 s；239 条时空载量到 112.7 s，而机器有 20+ 后台负载时量到 1471 s；
269 条时空载量到 106.4 s；280 条时空载量到 92.0 s（加 `--opcua` 的 283 条是 112.5 s）；
同一份代码连跑四次落在 88.7–110.0 s 之间。310/313 条量到 113.5 s / 131.2 s，
其中较长的那次 `load average` 9–15（本机后台负载波动，不是用例本身变慢）。
NFR 的门槛是 120 s，压测用例、PBKDF2 派生与 TLS 握手是主要开销——**这个门槛只在空载机器上有意义**，
比对数字前先看 `uptime`。TLS 用例全部用进程内生成的自签证书跑真实握手，不依赖系统信任库，也不联网。

默认跳过的三条都是 OPC UA 验收（要绑端口、生成证书）：北向服务端 1 条，
南向客户端 2 条。
串口用例会自己开伪终端，Linux 上默认执行；其它平台报 `skip`，不算通过也不算失败。

用例与 PRD 需求的逐条对应见 [requirements-traceability.md](requirements-traceability.md)：
那里写明每条 FR 是「已实现 / 部分实现 / 刻意没做」，以及对应的用例名。

运行器的行为：

| 行为 | 说明 |
|---|---|
| 单个用例硬超时 180 s | 挂死的用例不会拖住整轮，失败信息里会写 `timed out after 180s` |
| 每个用例独立临时目录 | `TestContext.TempDir`，跑完不清理，失败时可以进去看现场 |
| `UIC_TESTDATA` | 覆盖黄金帧目录；不设时从输出目录向上找 `testdata/golden` |
| 环境不具备时跳过 | 用例抛 `SkipException`（例如非 Linux 没有 pty），输出 `skip` 与原因，不计入通过 |
| 退出码 | 0 = 全过，1 = 有失败，2 = 语料加载失败 |

## 用例分布

| 分组 | 数量 | 覆盖 |
|---|---|---|
| `golden/*` | 20 | 字节级：请求与响应完全匹配（T14），含 4 份数组黄金帧（float32 数组、uint16 数组、批量写数组、逐元素字节序） |
| `codec/*` | 18 | 校验和、字节序（含「字符串/字节流不参与重排」）、BCD、时间值、窗口、异常码、分帧容错、数组逐元素字节序与元素个数校验 |
| `pipeline/*` | 13 | 缩放/偏移、整数取整、质量传播、三种死区模式、数组逐元素缩放与「取最大变化」死区、**变化率死区的 Δt 用单调时钟**（校时跳变不伪造也不抹平尖峰） |
| `cache/*` | 5 | 序号单调、只在变化时通知、按点位死区判定、移除设备、订阅者故障隔离 |
| `planner/*` | 8 | 窗口合并、切分上限、区分离、非寄存器寻址 |
| `history/*` | 9 | journal 历史：时间/身份过滤、每桶降采样、重放去重、缺失文件、坏行跳过、跨读取缓冲区行、跨缓冲区胜出行回读、转义名回退、非法日历日期跳过 |
| `outbox/*` | 9 | 位点推进、重启重放、批次、分区隔离、三重上限、半行容错、**磁盘满（`/dev/full` 注入 ENOSPC）时不推进位点且恢复后补投** |
| `fault/*` | 4 | 环境级故障注入：**进程假死**（驱动卡在不可取消的 Task 上，看门狗报停滞并做 5 分钟去重）、**时钟跳变**不误判停滞、**时钟跳变**不清空 Outbox 积压（TTL 按单调时长）、数据目录不可写时样本被计数丢弃而不是静默消失 |
| `lifecycle/*` | 3 | 设备下线后的状态清理：**被删设备的观测状态一并遗忘**（含停滞去重窗口，删了又加回来能重新报警）、重新加回时重推 `$state`、**读挂死时下线不被阻塞**（`StopAsync` 有等待上限，否则配置应用与关机一起挂住） |
| `config/*` | 18 | 内容寻址、回滚、启动优先级、非法文档、diff、旧配置迁移、复合时长、死区/量程规则、数组维度规则（类型/125 寄存器上限）、仓库示例可校验、25 类非法配置逐条定位、脱敏占位符还原、控制台草稿拼装出的配置能被真的解析器接受、四类寻址各自必填/禁止字段与混填拒绝 |
| `json/*` | 11 | 时长、枚举、值形状、数组值形状、`count` 往返、UTC、往返稳定、前向兼容 |
| `logs/*` | 7 | JSON 行字段、轮转不丢行、归档裁剪、默认关闭、热更新开关、非法配置拒绝、环形缓冲顺序与过滤 |
| `resources/*` | 5 | 磁盘/内存进入与恢复回差、探针不可读不误报、降级时文件日志暂停、非法水位配置拒绝 |
| `engine/*` | 13 | 同连接永不并发、状态机与代次、临时失败重试、坏质量整批重试、deadline 超时重试、确定性错误不重试、重试次数有限、写后回读一致/不一致/不开启、数组写入形状校验、点位周期覆盖设备周期、快慢桶失败互不牵连 |
| `schedule/*` | 5 | 启动相位均匀铺开、单调时间换算与到期边界、调度器实际分配相位、相位按设备内最快点位周期、**uptime 用单调时钟**（校时跳变不会让它凭空多出一小时） |
| `loopback/*` | 12 | Modbus 主站 ↔ 内置从站的真实链路 |
| `s7/*` | 30 | S7comm：16 条手算黄金帧与地址解析（TPKT/COTP 握手、PDU 协商、读/写/位读写、长度单位是位、错误分类），14 条主站 ↔ 内置从站回环（探活、DB/I/Q/M 读、位读写、写输入区被拒、越界落 `address_error`、bool 缺 bit 被拒、数组、按 PDU 切片、单点装不下点名失败、沉默 PLC 超时、断线重连） |
| `serial/*` | 7 | 真实伪终端上的 RTU / ASCII、半双工、帧间静默、deadline、关闭打断读 |
| `mqtt/*` | 34 | 测试内 broker：CONNECT/SUBSCRIBE 字段、QoS0/1、主题渲染、重连补发、**下线设备清 retained `$state`**、**命令去重窗口用单调时钟（校时跳变不会重复执行命令）**、**过期判定用单调「多久没收到报文」（校时跳变不伪造也不放过过期）**、TLS 发布与不可信 CA 拒绝；南向驱动：主题通配匹配、JSON 字段/类型/信封取值、首次读数前 `stale`、QoS0/1 取数、点位字段覆盖、坏载荷与恢复、断线重订阅、订阅被 ACL 拒绝 |
| `template/*` | 4 | 50 实例批量生成、参数类型/缺失/未知参数拒绝、未声明占位符拒绝；**同一模板产出 Modbus 与 S7 两台设备并同时通过校验**（FR-CFG-01 验收口径） |
| `csv/*` | 5 | 地址表达式（参考号/区域偏移/分列）、按 id 合并与冲突行号、死区模式与量程列、点位周期列、数组元素个数列 |
| `contract/*` | 30 | 进程内真实 Kestrel 的 REST 契约（含驱动能力、模板、CSV、导入导出、单点幂等、**幂等窗口用单调时钟**（校时跳变既不重复执行写命令、也不会永不失效）、资源降级联动、脱敏配置往返、明文凭据被拒、journal 历史查询、审计写入/导出/拒绝留痕、点位有效周期、数组元素个数与值形态、HTTPS 监听、控制台新建设备的完整链路；认证的 5 条见 `contract/auth_*`） |
| `secret/*` | 9 | 凭据引用：环境变量 / 密钥文件解析、缺失变量硬失败（不能静默变成空令牌）、明文被拒、空值合法、`enc:v1` 封解与篡改拒绝、未知方案与版本、摘要不泄露明文 |
| `tls/*` | 3 | 证书必填且可读、PFX 口令必须是引用、`insecureSkipVerify` 依赖 `tls.enabled`；证书/CA 不接受凭据引用 |
| `audit/*` | 7 | 审计落盘：按动作/结果/身份查询、重启后序号单调且旧记录可查、按大小轮转与保留只删旧文件、禁用时不写盘、断电半行跳过、按文件名日期跳过无关归档、分页最新在前 |
| `auth/*` | 10 | 本地账号：PBKDF2 加盐且文件无明文、首次启动只建一次 admin、改密清除强制标记并删初始口令文件、会话 12 小时过期、5 次失败/分钟锁定与按 IP 隔离、锁定窗口不受系统校时跳变影响、角色权限矩阵、停用账号立即失效、最后一个管理员不可停用/降级、口令策略 |
| `opcua/*` | 14 | 北向服务端：标准 OPC UA 客户端浏览/读/写/订阅（`--opcua`）；南向客户端：NodeId 解析与拒绝形态、StatusCode→质量表、Variant 标量/数组/形状与范围校验、写入编码、时标、端点/策略/PKI 参数推导、端点精确匹配不降级（11 条纯函数），以及客户端↔内置服务端回环：批量读（坏地址单独失败）、写与回读、越界拒绝、订阅推送、要求 Basic256Sha256 而服务端只有 None 时显式失败（2 条 `--opcua`） |

## 故障注入矩阵 12 类

前 9 类靠协议级用例覆盖（真实回环、真实 broker、真实伪终端，或虚拟驱动的故障模式）；
后 3 类是环境级故障，各自有一个**真实**注入点，而不是用一个假实现把断言凑出来。

| # | 故障 | 注入方式 | 用例 |
|---|---|---|---|
| 1 | 连接拒绝 | 指向未监听的端口 | `loopback/*`、`s7/silent_plc_times_out` |
| 2 | 连接中断 | 回环中主动断链 | `loopback/*`、`mqtt/driver_resubscribes_after_broker_drop` |
| 3 | 读超时 | 从站沉默 | `loopback/probe_times_out`、`loopback/silent_slave_times_out` |
| 4 | 半包 | 响应分片到达 | `loopback/fragmented_response_is_reassembled` |
| 5 | 错 CRC | 篡改校验和 | `codec/framing_rejects_corruption` |
| 6 | 异常码 | 从站返回 01/02/03/04/06/11 | `codec/exception_codes_map_to_kinds` |
| 7 | 部分响应 | 少于请求的寄存器数 | `codec/decode_rejects_inconsistent_frames` |
| 8 | 慢响应 | 延迟应答 | `engine/deadline_is_retried_as_timeout`、`s7/silent_plc_times_out` |
| 9 | Broker 断线 | 测试内 broker 主动断开 | `mqtt/driver_resubscribes_after_broker_drop` |
| 10 | 磁盘满 | 目标文件软链到 `/dev/full`，内核写时返回 ENOSPC | `outbox/disk_full_keeps_records_pending` |
| 11 | 时钟跳变 | 注入可拨的 `TimeProvider`，墙钟与单调时钟分开推进 | `fault/clock_jump_does_not_fake_a_stall`、`fault/outbox_ttl_ignores_a_clock_jump` |
| 12 | 进程假死 | 驱动卡在一个不响应取消的 `Task` 上，actor 真的不再产出样本 | `fault/collection_stall_is_reported` |

两条与平台相关的跳过写在这里，免得看到 `skip` 时以为用例坏了：
第 10 类需要 `/dev/full`（非 Linux 没有），`fault/outbox_permission_error_is_counted`
需要 POSIX 文件权限（Windows 上跳过）。**跳过不计入通过数**。

## 规范用例 T1–T18 的对应关系

| # | 规范用例 | 本仓库用例 | 备注 |
|---|---|---|---|
| T1 | `Probe` 成功 | `loopback/probe_succeeds` | |
| T2 | `Probe` 超时 | `loopback/probe_times_out` | 断言 `ErrorKind.Timeout` 且 `Temporary=true` |
| T3 | 单点读 | `loopback/read_returns_values_in_order` + `codec/read_limits` | |
| T4 | 批量读 | `loopback/read_returns_values_in_order`、`loopback/planner_windows_round_trip` | 结果数量与顺序都断言 |
| T5 | 空批量读 | `loopback/empty_batch_is_a_no_op` | 读与写都返回空，且不发帧 |
| T6 | 地址越界 | `loopback/read_maps_exception_to_quality`、`engine/permanent_failure_is_not_retried` | 越界在编码阶段就被拦，质量码 `address_error`；上层既不重试，也不把它计入熔断 |
| T7 | 写入回读 | `loopback/write_then_read_back` | |
| T8 | 写入只读点 | `loopback/write_to_missing_register_is_rejected`、`codec/write_rejects_readonly_and_bit_mismatch` | |
| T9 | 取消上下文 | `loopback/cancellation_returns_promptly` | 断言 500 ms 内返回 |
| T10 | `Close` 幂等 | `loopback/close_is_idempotent` | 连续 3 次 |
| T11 | 并发保护 | `engine/conn_is_never_called_concurrently` | 用会计数的假驱动，读写都不许重叠 |
| T12 | 半包响应 | `loopback/fragmented_response_is_reassembled`、`codec/framing_rejects_corruption` | 响应分三片、带间隔到达 |
| T13 | 异常码映射 | `codec/exception_codes_map_to_kinds` | 01/02/03/04/06/11 全覆盖 |
| T14 | 金报文 | `golden/*`（20 份） | TCP / RTU / ASCII 三种封装 |
| T15 | 半双工并发 | `serial/half_duplex_does_not_cross_responses` | 两台从站共用一条 pty：值不许串，响应未发出前总线上不许出现新请求 |
| T16 | 沉默从站 | `loopback/silent_slave_times_out`、`serial/silent_slave_times_out` | TCP 与串口各一条，都是 300 ms 超时且不挂死 |
| T17 | 串口 deadline | `serial/read_returns_near_deadline` | 返回时间落在 deadline 附近，既不是瞬间返回也不是 N 倍 |
| T18 | 关闭打断读 | `serial/close_interrupts_blocked_read` | 阻塞中的读在关闭后 2 s 内返回 |

T1–T18 只约束驱动本身。重试次数、重试只针对临时性失败、重试耗尽后的降级
这些属于调度层的决定，由 `DeviceActor` 的用例覆盖（见上表 `engine/*`）。

### 串口用例怎么跑

T15–T18 是串口链路专属：termios、内核 tty 的 deadline、半双工总线上的响应交错。
这些行为不在内存流里 —— 内存流没有 tty 层，也没有真实的 poller，用内存流伪造出来的
「通过」只会掩盖缺陷。所以 `serial/*` 跑在**真实伪终端**上：

```
posix_openpt → grantpt → unlockpt → ptsname
主站：System.IO.Ports 打开 pts 从设备路径
测试端：用同一个 master fd 扮演总线上的一台或多台从站
```

只用 libc 的四个函数，不引第三方包，也不依赖 `socat` 之类的外部进程；
非 Linux 上 `posix_openpt` 不存在，用例报 `skip` 并打印原因。

从站这一端不是「照着期望字节回包」：它跑的是仓库里的 `ModbusSlaveHandler`
（与内置 TCP 从站同一份 PDU 逻辑），所以主站发出的帧必须真的能被解析、CRC 必须对。
`serial/rtu_golden_frame_over_pty` 与 `serial/ascii_golden_frame_over_pty` 还会把
总线上抓到的请求字节与 `testdata/golden/` 里的黄金帧逐字节比对 —— 这样「黄金帧」
和「真实 tty」之间不再是两套互不相干的验证。

## 黄金帧格式

```json
{
  "name": "read_holding_2_regs",
  "request": "0001 0000 0006 01 03 0064 0002",
  "response": "0001 0000 0007 01 03 04 000A 000B",
  "expect": [
    { "point": "p1", "type": "uint16", "num": 10 },
    { "point": "p2", "type": "uint16", "num": 11 }
  ]
}
```

| 字段 | 必需 | 说明 |
|---|---|---|
| `name` | 是 | 用例名，也是文件名 |
| `request` / `response` | 是 | 十六进制字节，空格仅为可读性 |
| `expect[]` | 是 | 解码后的期望值 |
| `kind` | 否 | `read`（默认）/ `write` |
| `frame` | 否 | `tcp`（默认）/ `rtu` / `ascii` |
| `area` | 否 | 写入用例的寄存器区 |
| `byteOrder` | 否 | 默认 `abcd` |
| `error` | 否 | 期望抛出的错误分类：`timeout`/`connection`/`protocol`/`address`/`permission`/`unsupported` |
| `expect[].offset` | 否 | 绝对地址偏移；缺省按前一个点位的宽度顺延 |
| `expect[].length` | 否 | `string`/`bytes` 的寄存器数 |
| `expect[].array` | 否 | 数组期望值；写了它就是数组点位，寄存器数按 `元素数 × 类型宽度` 推导 |
| `expect[].quality` | 否 | 期望质量码，默认 `good` |

`request` 与 `response` 是**整帧**：TCP 含 MBAP 头，RTU 含 CRC，ASCII 含 `:` 前缀与 `\r\n` 后缀。
测试会做三件事：编码出请求字节并与 `request` 逐字节比较、解码 `response` 并与 `expect` 比较、
再验证解码结果的类型与质量码。

### 现有 20 份

| 文件 | 覆盖 |
|---|---|
| `read_holding_2_regs` | 基础读，两寄存器 |
| `read_holding_with_gap` | 点位间有空隙（100 与 102），窗口不能跨空隙 |
| `read_input_int16_negative` | 输入寄存器 + 有符号负数（`0xFFFE` = -2） |
| `read_coils_10` | 线圈按位打包（`05 02` 一个字节装 8 位） |
| `read_discrete_inputs_3` | 离散输入，起始地址非 0 |
| `read_holding_float32_abcd` | 32 位大端 |
| `read_holding_float32_cdab` | 32 位字交换（现场最常见的非标准排布） |
| `read_holding_float32_array` | 2 元素 `float32` 数组，4 个寄存器 |
| `read_holding_float32_array_cdab` | 同一数组按 `cdab`：**逐元素**字交换，元素顺序不变 |
| `read_holding_uint16_array` | 3 元素 `uint16` 数组 |
| `read_holding_uint32` | 32 位无符号整数 |
| `read_holding_bcd16` | BCD 编码（电表常用） |
| `read_exception_illegal_address` | 异常响应 `0x83 0x02` → `address_error` |
| `write_single_register` | FC06 |
| `write_single_coil_on` | FC05，`0xFF00` |
| `write_multiple_regs_float32` | FC16，写两个寄存器 |
| `write_multiple_regs_uint16_array` | FC16，一次写 3 个元素的 `uint16` 数组 |
| `write_exception_illegal_value` | 写异常 `0x86 0x03` → `protocol` |
| `rtu_read_holding_2_regs` | RTU 封装 + CRC |
| `ascii_read_holding_2_regs` | ASCII 封装 + LRC |

## 契约测试怎么起服务

`contract/*` 在进程内起一个真实的 Kestrel（不是 `TestServer`）：

```
WebApplication.CreateBuilder → UseUrls("http://127.0.0.1:0")
  → app.UseNorthbound(runtime.CreateNorthboundContext()) → StartAsync
  → 地址从 IServer + IServerAddressesFeature 取
```

这样走的是真实的中间件顺序、真实的 HTTP 解析、真实的错误信封 ——
用 `TestServer` 或直接调 handler 都会漏掉中间件顺序这类问题。

配置里的 `northbound.rest.listen` 填一个合法假值（`127.0.0.1:18080`），
因为校验器不接受 0 号端口，而真实绑定地址由 `UseUrls` 决定。

## MQTT 用例怎么跑

`mqtt/*` 连的是测试进程内的最小 broker（`tests/Uic.Tests/Mqtt/FakeMqttBroker.cs`），
它按 3.1.1 规范独立拆包、拼包，**不复用客户端任何代码** —— 否则「客户端拼错了」
和「测试也照着错」会互相掩护，跑通了也说明不了什么。覆盖：

- CONNECT：协议名与级别 4、clean session、遗嘱（主题/载荷/QoS/retain）、用户名口令；
- PUBLISH：QoS1 必须等到 PUBACK（broker 故意延迟 150 ms，断言发布确实被阻塞）、
  QoS0 不带 packet id、retain 跟随配置；
- 主题与载荷：`{site}/{device}/{point}` 渲染与斜杠清洗、`schemaVersion` 注入、
  载荷原样透传；
- 重连：broker 主动断链后客户端自行重连，并补发 `$birth` 与全部已知 `$state`；
- 心跳：空闲连接在 keepalive 的一半内发 PINGREQ（不发就会被 broker 按 1.5×keepalive 踢掉）；
- 订阅：SUBSCRIBE 带 packet id 与 QoS，SUBACK 被消费；入站 QoS1 要回 PUBACK
  （漏了这条 broker 会一直重投同一条命令）；
- 反例：CONNACK 非 0 时不报「已连接」但继续重试；入站 PUBLISH 不会被当成 PUBACK。

南向驱动（`MqttClientDriver`）的 12 条用例同样跑在这个 broker 上：先 `ProbeAsync`
确认订阅被接受，再用 `PublishToClientAsync` 往客户端推报文，断言 `ReadAsync` 从缓存里
读到的值、质量与过期行为。它证明的是「订阅登记 → 收到 → 解析 → 缓存」这条链路，
不代替真实 Broker 的 ACL/保留消息语义。

命令通道（`MqttCommandSink`）另有 8 条端到端用例，用 stub `IGatewayControl` 计数
来证明「执行了/没执行」而不是只看结果 JSON：

| 用例 | 断言 |
|---|---|
| `command_executes_and_publishes_result` | 请求参数透传、结果回到约定主题、`schemaVersion`/`replayed` 正确 |
| `command_unknown_action_returns_error` | `request.unknown_action`，错误信息包含被拒动作 |
| `command_duplicate_id_executes_once` | 同一 id 两次只调用一次控制面，第二次 `replayed:true` |
| `command_privileged_action_is_forbidden` | `config.rollback` 返回 `command.forbidden`，控制面零调用 |
| `command_missing_id_is_rejected` | 无 id 返回 `request.invalid`，一次也不执行 |
| `command_resubscribes_after_reconnect` | broker 断链后重新订阅，命令在新会话上仍可执行 |
| `command_outbox_flush_forwards_sink` | `outbox.flush` 的 `sink` 参数透传到控制面，而不是被静默忽略 |
| `command_diag_bundle_is_whitelisted` | `diag.bundle` 在白名单内，且返回落盘路径 |

这套用例抓到过三个真实缺陷，都属于「代码评审看不出来、上线才出问题」的类型：
`ReadPacketAsync` 把剩余长度字节当成报文头（所有应答都被丢弃）、
`PublishAsync` 在写完才登记等待者（PUBACK 抢跑即丢）、
读循环在 `OnConnected` 之后才启动（`$birth`/`$state` 每次重连都要等满 3 s 超时）。
另外补上了原先根本没实现的心跳：CONNECT 声明了 keepalive 却从不发 PINGREQ。

### 与真实 broker 的互操作

测试内 broker 证明「客户端拼的字节自洽」，但证明不了「别的实现也认」。`scripts/mqtt-interop.sh`
起一个官方 mosquitto 容器跑真链路，覆盖四件事：

1. 上线：`$birth` 与设备 `$state` 被 retained，点位值按 QoS1 持续发布；
   新起的订阅者一条命令就能拿到 retained 状态；
2. 重连：`docker rm -f` broker 再用同一端口起回来（retained 随进程消失），
   客户端在 `reconnectDelayMs` 后自己回去并补发 `$birth`/`$state`，数据继续流动；
3. 命令：用 `mosquitto_pub` 往 `$command` 发 `driver.restart`，结果回到
   `$command/result`；同一个 id 重投只重放结果（`replayed:true`）；
   `config.rollback` 被拒绝（`command.forbidden`）；
4. 遗嘱：`kill -9` 网关，broker 检测到没有 DISCONNECT，代发 retained 的 `$death`。

脚本需要 docker（镜像不存在时自动拉取 `eclipse-mosquitto:2`），不在 `tests/Uic.Tests`
里跑是因为它依赖外部容器，而那个运行器要保证离网可跑。`--keep` 保留配置、日志与抓包。

## OPC UA 验收

`--opcua` 会：

1. 取一个空闲端口起网关（含 OPC UA 服务端，自签证书建在临时目录）；
2. 用标准客户端（`OPCFoundation.NetStandard.Opc.Ua.Client`）匿名连接；
3. 浏览 `Objects/UIC/{site}/{device}`，断言命名空间索引为 2、节点 NodeId 为 `site.device`；
4. 读点位并校验值，写入 77.5 后回读；
5. 建订阅、等一次 DataChange 通知。

用第三方栈而不是自己拼二进制，是为了证明「真的兼容 OPC UA」而不是「看起来像」。
握手（Hello/Ack、OpenSecureChannel、CreateSession、CreateSubscription）一步都不能省。

## WASM 控制台验收

`scripts/wasm-smoke.sh` 分两层：

1. **静态层**（curl）：`/` 返回启动页、引导脚本可下载、`/api/v1/health` 同源可用、
   深链接回退到 `index.html`、缺失的框架资源返回 404（不能被 SPA 回退吞成 HTML）；
2. **运行层**（Playwright）：真开页面，用首次启动生成的初始口令登录，走完「强制改密」，
   等设备列表从 REST 渲染出来；再在设备页点「新建设备」，填一张表（新建通道 + 一台设备 +
   首个点位）并提交，等页面报「已创建设备」且列表里出现它；同时断言没有未捕获异常、
   失败请求、4xx/5xx，启动遮罩在挂载后移除，顶栏显示当前账号。

第二项是「读得到」与「写得进」的分界：它真的跨过登录 → 会话 Token → 强制改密 →
`config/validate` → `config/apply` → 配置版本 → 设备启动 → 列表刷新，是整条首开流程里
唯一无法用 curl 代替的部分。初始口令由脚本从 `data/security/initial-admin-password.txt`
读出，只通过环境变量传给 Playwright，不打印。新建的设备指向 `127.0.0.1:502`
（本机没有从站），所以它会停在 `faulted`；验收的是提交链路，不是连通性。

运行层是重点：裁剪后的 WASM 最容易坏在运行时（源生成序列化漏类型、启动脚本与
`index.html` 的结构约定、同源地址解析写死端口），这些 curl 全看不出来。本机实测
首次启动（含登录与强制改密）Chromium 约 12 s、Firefox 约 38 s。

浏览器用 `--browser` 换（默认 `chromium`，也可 `firefox`；装了 webkit 同理）。
Uno WASM 的坑有一半是引擎相关的——同一份产物在 Gecko 上可能因为启动脚本或渲染层
差异而失败，只验一个引擎等于只验一种命：

```bash
scripts/wasm-smoke.sh --browser firefox
```

没装 playwright（或没装对应浏览器）时运行层报 `skip`，静态层仍会执行并给出结论；
`--no-browser` 可以显式只跑静态层，`--web <dir>` 复用已有发布目录，
`--keep` 保留发布产物、日志与截图。

## TLS 外部验收

自包含用例里的 HTTPS 是进程内 Kestrel + 钉死证书的客户端，证明不了「真实的
`curl` 也认」。`scripts/tls-smoke.sh` 换成真进程与真工具：

1. `openssl` 现生成自签证书（`CA:TRUE` + SAN），导出 PFX，口令只放环境变量；
2. `uic check` 只看配置就报出 `rest(https://...)`、`mqtts(...)` 与口令来源；
3. 真宿主起 HTTPS：`curl --cacert` 通过校验、错误 CA 被拒、同端口明文 HTTP 连不上、
   `openssl s_client` 看到 `CN=localhost`；
4. 失败路径逐个确认退出码 2 且端口没被占用：证书路径为空、文件不存在、
   PFX 口令不对；明文口令在 `uic check` 阶段就被拒；
5. 全流程不联网、不依赖 docker，装了 `openssl` + `curl` 就能跑。

```bash
scripts/tls-smoke.sh          # 跑一遍并清理
scripts/tls-smoke.sh --keep   # 保留证书与 host.log 便于排查
```

MQTT 的 TLS 客户端只有进程内用例（`mqtt/tls_publishes_over_custom_ca`、
`mqtt/tls_rejects_untrusted_broker`），与真实 Broker 的 TLS 互操作仍未覆盖，
接入现场时用目标 Broker 的 CA 再验一次（见「已知未覆盖」）。

## 历史查询性能基准

`scripts/history-bench.sh` 生成指定行数的 journal 并对四类查询各取 3 次最快值：

```bash
scripts/history-bench.sh                 # 100 万行 / 约 152 MB
scripts/history-bench.sh --lines 200000  # 换数据量
```

默认输出形如（本机 16 核 / 14 GB，Release）：

```
  全范围 + 5m 降采样            返回   66800 条  最快  1267 ms
  单设备 + 1m 降采样            返回    6668 条  最快   447 ms
  最近 30 分钟 + 1m 降采样      返回    6200 条  最快   333 ms
  全范围不降采样                返回 1000000 条  最快  7625 ms
```

这是耗时验收而不是单元测试：数字依赖机器与磁盘，放进自包含运行器会让
「通过/失败」随硬件漂移。同一台机器上不同轮次也会有一两成波动，看数量级即可。
PRD 的目标是「100 万点降采样后 < 2 s」，第一行就是它。

## 容量与写延迟基准

`scripts/capacity-bench.sh` 起一个真的宿主进程，按 PRD 的容量档生成配置
（默认 200 台设备 × 50 点 = 10,000 点，周期 1s），等全部点位转 `good` 后取
15 s 稳态窗口，再在持续读负载下按 5 次/秒发 100 次单点写：

```bash
scripts/capacity-bench.sh                  # 200 设备 × 50 点 / 1s
scripts/capacity-bench.sh --devices 50 --points 20   # 小规模先看一眼
scripts/capacity-bench.sh --confirm        # 写后回读（设备改用常量波形）
scripts/capacity-bench.sh --keep           # 保留配置、数据目录与 host.log
```

默认输出形如（本机 16 核 / 14 GB，Debug 宿主，另有两个开发容器在跑）：

```
  全部点位转 good 用时 1.5 s（200 台设备，10000 个点位）

  读请求               203.3 /s（200 台设备，每台每周期一个合并窗口：约    10163 点/秒）
  点位                 10000 / 10000 good（stale 0，bad 0）
  设备                   200 / 200 collecting（faulted 0）
  CPU                    114 %（单核 = 100 %，15 s 窗口平均）
  RSS                  260.6 MB（工作集，含运行时）
  健康                    ok

  写命令  100 次（5 次/秒，confirm=false，只测写入路径；预热 3 次不计入）
    P50     2.1 ms   P95     5.8 ms   P99    12.1 ms   最大    18.3 ms
```

`--confirm`（写后回读）在同一负载下是 P50 2.0 ms / P95 2.4 ms / P99 3.2 ms：
回读由驱动当场给出结论，不需要等下一轮采集。它要求「写进去的值下一轮读还在」，
而虚拟设备默认是正弦波形、每轮重新生成读数，所以这个模式会把设备切成常量波形——
此时值不再变化，缓存与扇出的成本随之下降，两种模式的数字不要混着比较。

两个说明：

- **写速率是 5 次/秒**，不是压到极限：REST 的写配额是每 IP 10 次/秒（`RateLimiter`），
  再快会拿到 429。那是限流而不是延迟，混在一起会让 P95 失去意义；脚本遇到 429
  会单独计数并在结果里提示。
- **预热 3 次不计入**：第一次写会撞上 JIT 与首次连接的冷路径（本机量到约 70 ms），
  算进 P95 只会掩盖稳态表现。

还有一点要说清楚：这套数字只覆盖**引擎与 REST 的成本**。虚拟设备没有链路 I/O，
journal、outbox、MQTT 都按默认关闭——它们各自的成本由 `history-bench.sh` 与
`mqtt-interop.sh` 分别衡量。现场接入前应当用真实链路、打开实际会用的 sink 再跑一遍。

与历史基准一样，这是耗时验收而不是用例：真的起 200 台设备跑十几秒，数字随机器漂，
通过/失败由人对着 PRD 判断。PRD 的两条目标是「200 设备 / 10,000 点」与
「写命令 P95 ≤ 300 ms（读满载下）」，上面的输出就是这两条的现场证据。

## 已知缺口

| 缺口 | 影响 | 建议 |
|---|---|---|
| MQTT 只与 mosquitto 互操作过 | 用户认证、ACL 与其它 broker 的差异未覆盖（TLS 只有自签证书的进程内用例，没有与真实 Broker 的 TLS 互操作）；命令通道同样依赖 Broker 侧隔离 | 接入现场时用目标 broker 重跑 `scripts/mqtt-interop.sh`，用现场 CA 再跑一次 TLS 连接，并验命令主题的 ACL |
| MQTT 南向只与测试内 broker 对过 | 订阅被 ACL 拒绝、保留消息、Broker 重启后的 `$SYS` 主题等真实差异没有端到端；驱动不实现持久会话，Broker 停机期间的消息会丢 | 现场接入前用目标 Broker 的 ACL 验证一次 `ProbeAsync`（被拒应报 `permission`），并用真实发布方确认断线窗口内可接受丢数 |
| MQTT 命令去重只在进程内 | 幂等窗口 5 分钟、重启即清空；命令本身已落审计，但幂等状态不跨重启 | 需要跨重启幂等时用 REST 的 `X-Idempotency-Key`，或把幂等台账落到外部存储 |
| WASM 控制台只验过 Chromium 与 Firefox | Safari / WebKit、移动端与真实触屏未覆盖；冒烟脚本已覆盖「登录 → 强制改密 → 首屏 → 新建设备」，但只验证 admin 一个角色 | 上线前在目标浏览器各跑一次同一套检查：`scripts/wasm-smoke.sh --browser <name>`（装了 webkit 后可直接跑） |
| 真实硬件 | 全部用例都跑在内置从站 / 虚拟设备 / 伪终端上 | 现场接入时先用 `uic check` 与 `/api/v1/commands` 的 `device.selftest` |
| S7 只在 S7-1200/1500 的通用语义上验过 | 符号寻址（0xB2）、S7Plus、TPDU 分片、多连接并发都明确不支持（配置或运行期直接报错，不静默降级）；S7-300/400 的机架/槽位换算与老型号的 PUT/GET 差异没有实机对照 | 现场先用 `uic sim` 的内置从站验通链路，再用真实 CPU 跑一次 `device.selftest`；机架/槽位按型号手册填 `labels.rack` / `labels.slot` |
| 模板只支持 string/int/bool 三种参数 | 「按站点切换整段点位」这类复杂参数化做不到 | 这是刻意的：更强的模板等于一门配置语言，先把三种类型用透 |
| CSV 导入一次只写一台设备 | 上千台设备的批量开通需要逐台调用 | 调用方循环；若需要「一个 CSV 覆盖多设备」，在文件里加 `device` 列再扩展 `PointCsv` |

串口用例虽然跑在伪终端上，走的是与真串口完全相同的内核 tty 路径（termios、
行规程、poller），所以 T15–T18 在 pty 上通过是有意义的；但它仍然不是
「某型号 USB 转 485 适配器」的验收——那一步只能在现场做。

这些缺口是**明确留白**而不是遗漏：每一条都写清了「为什么现在不做」与「上线前怎么补」。
