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
108 passed, 0 failed, 1 skipped · 29.7s
```

跳过的一条是 OPC UA 验收（要绑端口、生成证书，默认不跑）。
串口用例会自己开伪终端，Linux 上默认执行；其它平台报 `skip`，不算通过也不算失败。

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
| `pipeline/*` | 8 | 缩放/偏移、整数取整、质量传播、死区 |
| `cache/*` | 4 | 序号单调、只在变化时通知、移除设备、订阅者故障隔离 |
| `planner/*` | 8 | 窗口合并、切分上限、区分离、非寄存器寻址 |
| `outbox/*` | 8 | 位点推进、重启重放、批次、分区隔离、三重上限、半行容错 |
| `config/*` | 8 | 内容寻址、回滚、启动优先级、非法文档、diff |
| `json/*` | 9 | 时长、枚举、值形状、UTC、往返稳定、前向兼容 |
| `engine/*` | 2 | 同连接永不并发、状态机与代次 |
| `loopback/*` | 12 | 主站 ↔ 内置从站的真实链路 |
| `serial/*` | 6 | 真实伪终端上的 RTU / ASCII、半双工、deadline、关闭打断读 |
| `mqtt/*` | 8 | 测试内 broker：CONNECT 字段、QoS0/1、主题渲染、断线重连补发 |
| `contract/*` | 7 | 进程内真实 Kestrel 的 REST 契约 |
| `opcua/*` | 1 | 标准 OPC UA 客户端：浏览、读、写、订阅 |

## 规范用例 T1–T18 的对应关系

| # | 规范用例 | 本仓库用例 | 备注 |
|---|---|---|---|
| T1 | `Probe` 成功 | `loopback/probe_succeeds` | |
| T2 | `Probe` 超时 | `loopback/probe_times_out` | 断言 `ErrorKind.Timeout` 且 `Temporary=true` |
| T3 | 单点读 | `loopback/read_returns_values_in_order` + `codec/read_limits` | |
| T4 | 批量读 | `loopback/read_returns_values_in_order`、`loopback/planner_windows_round_trip` | 结果数量与顺序都断言 |
| T5 | 空批量读 | `loopback/empty_batch_is_a_no_op` | 读与写都返回空，且不发帧 |
| T6 | 地址越界 | `loopback/read_maps_exception_to_quality` | 越界在编码阶段就被拦，质量码 `address_error` |
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
- 反例：CONNACK 非 0 时不报「已连接」但继续重试；入站 PUBLISH 不会被当成 PUBACK。

这套用例抓到过三个真实缺陷，都属于「代码评审看不出来、上线才出问题」的类型：
`ReadPacketAsync` 把剩余长度字节当成报文头（所有应答都被丢弃）、
`PublishAsync` 在写完才登记等待者（PUBACK 抢跑即丢）、
读循环在 `OnConnected` 之后才启动（`$birth`/`$state` 每次重连都要等满 3 s 超时）。
另外补上了原先根本没实现的心跳：CONNECT 声明了 keepalive 却从不发 PINGREQ。

## OPC UA 验收

`--opcua` 会：

1. 取一个空闲端口起网关（含 OPC UA 服务端，自签证书建在临时目录）；
2. 用标准客户端（`OPCFoundation.NetStandard.Opc.Ua.Client`）匿名连接；
3. 浏览 `Objects/UIC/{site}/{device}`，断言命名空间索引为 2、节点 NodeId 为 `site.device`；
4. 读点位并校验值，写入 77.5 后回读；
5. 建订阅、等一次 DataChange 通知。

用第三方栈而不是自己拼二进制，是为了证明「真的兼容 OPC UA」而不是「看起来像」。
握手（Hello/Ack、OpenSecureChannel、CreateSession、CreateSubscription）一步都不能省。

## 已知缺口

| 缺口 | 影响 | 建议 |
|---|---|---|
| MQTT 未连真实 broker | 发布侧已在测试内 broker 上做协议级验收，但没和 mosquitto 等实现互操作过 | 上线前用 mosquitto 手工验收一次：联通、断线重连、`$birth`/`$death`/`$state` |
| WASM 控制台 | 自动化验收放在独立脚本里（Playwright 驱动），不在本运行器内 | 见仓库外部的浏览器冒烟脚本 |
| 真实硬件 | 全部用例都跑在内置从站 / 虚拟设备 / 伪终端上 | 现场接入时先用 `uic check` 与 `/api/v1/commands` 的 `device.selftest` |

串口用例虽然跑在伪终端上，走的是与真串口完全相同的内核 tty 路径（termios、
行规程、poller），所以 T15–T18 在 pty 上通过是有意义的；但它仍然不是
「某型号 USB 转 485 适配器」的验收——那一步只能在现场做。

这些缺口是**明确留白**而不是遗漏：每一条都写清了「为什么现在不做」与「上线前怎么补」。
