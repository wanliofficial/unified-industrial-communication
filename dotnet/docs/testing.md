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
162 passed, 0 failed, 1 skipped · 36.8s
```

跳过的一条是 OPC UA 验收（要绑端口、生成证书，默认不跑）。
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
| `golden/*` | 16 | 字节级：请求与响应完全匹配（T14） |
| `codec/*` | 12 | 校验和、字节序、BCD、窗口、异常码、分帧容错 |
| `pipeline/*` | 10 | 缩放/偏移、整数取整、质量传播、三种死区模式 |
| `cache/*` | 5 | 序号单调、只在变化时通知、按点位死区判定、移除设备、订阅者故障隔离 |
| `planner/*` | 8 | 窗口合并、切分上限、区分离、非寄存器寻址 |
| `outbox/*` | 8 | 位点推进、重启重放、批次、分区隔离、三重上限、半行容错 |
| `config/*` | 14 | 内容寻址、回滚、启动优先级、非法文档、diff、旧配置迁移、复合时长、死区/量程规则、仓库示例可校验、24 类非法配置逐条定位 |
| `json/*` | 9 | 时长、枚举、值形状、UTC、往返稳定、前向兼容 |
| `logs/*` | 7 | JSON 行字段、轮转不丢行、归档裁剪、默认关闭、热更新开关、非法配置拒绝、环形缓冲顺序与过滤 |
| `resources/*` | 5 | 磁盘/内存进入与恢复回差、探针不可读不误报、降级时文件日志暂停、非法水位配置拒绝 |
| `engine/*` | 10 | 同连接永不并发、状态机与代次、临时失败重试、坏质量整批重试、deadline 超时重试、确定性错误不重试、重试次数有限、写后回读一致/不一致/不开启 |
| `schedule/*` | 3 | 启动相位均匀铺开、单调时间换算与到期边界、调度器实际分配相位 |
| `loopback/*` | 12 | 主站 ↔ 内置从站的真实链路 |
| `serial/*` | 6 | 真实伪终端上的 RTU / ASCII、半双工、deadline、关闭打断读 |
| `mqtt/*` | 17 | 测试内 broker：CONNECT/SUBSCRIBE 字段、QoS0/1、主题渲染、重连补发、命令去重与权限 |
| `template/*` | 3 | 50 实例批量生成、参数类型/缺失/未知参数拒绝、未声明占位符拒绝 |
| `csv/*` | 3 | 地址表达式（参考号/区域偏移/分列）、按 id 合并与冲突行号、死区模式与量程列 |
| `contract/*` | 14 | 进程内真实 Kestrel 的 REST 契约（含驱动能力、模板、CSV、导入导出、单点幂等、资源降级联动） |
| `opcua/*` | 1 | 标准 OPC UA 客户端：浏览、读、写、订阅 |

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
| T14 | 金报文 | `golden/*`（16 份） | TCP / RTU / ASCII 三种封装 |
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
| `expect[].quality` | 否 | 期望质量码，默认 `good` |

`request` 与 `response` 是**整帧**：TCP 含 MBAP 头，RTU 含 CRC，ASCII 含 `:` 前缀与 `\r\n` 后缀。
测试会做三件事：编码出请求字节并与 `request` 逐字节比较、解码 `response` 并与 `expect` 比较、
再验证解码结果的类型与质量码。

### 现有 16 份

| 文件 | 覆盖 |
|---|---|
| `read_holding_2_regs` | 基础读，两寄存器 |
| `read_holding_with_gap` | 点位间有空隙（100 与 102），窗口不能跨空隙 |
| `read_input_int16_negative` | 输入寄存器 + 有符号负数（`0xFFFE` = -2） |
| `read_coils_10` | 线圈按位打包（`05 02` 一个字节装 8 位） |
| `read_discrete_inputs_3` | 离散输入，起始地址非 0 |
| `read_holding_float32_abcd` | 32 位大端 |
| `read_holding_float32_cdab` | 32 位字交换（现场最常见的非标准排布） |
| `read_holding_uint32` | 32 位无符号整数 |
| `read_holding_bcd16` | BCD 编码（电表常用） |
| `read_exception_illegal_address` | 异常响应 `0x83 0x02` → `address_error` |
| `write_single_register` | FC06 |
| `write_single_coil_on` | FC05，`0xFF00` |
| `write_multiple_regs_float32` | FC16，写两个寄存器 |
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
2. **运行层**（Playwright + Chromium）：真开页面，等 WASM 启动，等设备列表从 REST
   渲染出来；同时断言没有未捕获异常、失败请求、4xx/5xx，启动遮罩在挂载后移除。

运行层是重点：裁剪后的 WASM 最容易坏在运行时（源生成序列化漏类型、启动脚本与
`index.html` 的结构约定、同源地址解析写死端口），这些 curl 全看不出来。本机实测
首次启动约 12 s。

没装 playwright 时运行层报 `skip`，静态层仍会执行并给出结论；`--no-browser` 可以
显式只跑静态层，`--web <dir>` 复用已有发布目录，`--keep` 保留发布产物、日志与截图。

## 已知缺口

| 缺口 | 影响 | 建议 |
|---|---|---|
| MQTT 只与 mosquitto 互操作过 | 认证、ACL、TLS、其它 broker 的差异未覆盖（客户端本身只做匿名明文）；命令通道同样依赖 Broker 侧隔离 | 接入现场时用目标 broker 重跑 `scripts/mqtt-interop.sh`，并单独验 TLS 终结方案与命令主题的 ACL |
| MQTT 命令去重只在进程内 | 幂等窗口 5 分钟、重启即清空；没有持久审计台账 | 需要跨重启幂等时用 REST 的 `X-Idempotency-Key`，或把命令记录落到外部存储 |
| WASM 控制台只在 Chromium 上验过 | 未覆盖 Firefox / Safari、移动端与真实触屏 | 上线前在目标浏览器各跑一次同一套检查（换 Playwright 的 browser 参数） |
| 真实硬件 | 全部用例都跑在内置从站 / 虚拟设备 / 伪终端上 | 现场接入时先用 `uic check` 与 `/api/v1/commands` 的 `device.selftest` |
| 模板只支持 string/int/bool 三种参数 | 「按站点切换整段点位」这类复杂参数化做不到 | 这是刻意的：更强的模板等于一门配置语言，先把三种类型用透 |
| CSV 导入一次只写一台设备 | 上千台设备的批量开通需要逐台调用 | 调用方循环；若需要「一个 CSV 覆盖多设备」，在文件里加 `device` 列再扩展 `PointCsv` |

串口用例虽然跑在伪终端上，走的是与真串口完全相同的内核 tty 路径（termios、
行规程、poller），所以 T15–T18 在 pty 上通过是有意义的；但它仍然不是
「某型号 USB 转 485 适配器」的验收——那一步只能在现场做。

这些缺口是**明确留白**而不是遗漏：每一条都写清了「为什么现在不做」与「上线前怎么补」。
