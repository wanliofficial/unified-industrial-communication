# 实现说明

这份文档说明**代码与设计文档的对应关系**、**首版取了什么舍了什么**，以及**怎么往下扩**。
它面向接手的人，不面向评审流程。

## 与设计文档的对应

实现依据仓库根目录 `docs/` 下的产品文档，不依赖任何外部代码。逐章对应：

| 文档 | 落到代码 |
|---|---|
| `03-product/02-architecture.md` | 分层与依赖方向 → `src/Uic.*` 的项目切分；装配点集中在 `Uic.Runtime` |
| `03-product/03-domain-model.md` | `Uic.Domain`：`Channel` / `Device` / `Point` / `Address` / `DataValue` / `Sample` / `Quality` |
| `03-product/04-driver-sdk-spec.md` §7 | 一致性用例 T1–T18 → `tests/Uic.Tests`（覆盖表见 [testing.md](testing.md)） |
| `03-product/04-driver-sdk-spec.md` §7.1 | 黄金帧格式 → `testdata/golden/*.json` |
| `03-product/04-driver-sdk-spec.md` §8 | 虚拟设备（波形 + 故障注入）→ `Uic.Driver.Virtual` |
| `03-product/05-northbound-api-spec.md` | REST / SSE / MQTT / OPC UA → `Uic.Northbound`、`Uic.OpcUa`（契约见 [northbound-api.md](northbound-api.md)） |
| `03-product/06-nfr.md` | 断网不丢、可回滚、可观测 → `Uic.Outbox`、`Uic.Config`、`Uic.Obs` |
| `02-synthesis/02-architecture-patterns.md` | 「配置即制品」「每设备 actor」「分区队列」三个模式 |
| `02-synthesis/03-gap-analysis.md` | 首版只做 Modbus，其余协议留作扩展点 |

### 借鉴了哪些方案的思路

这份实现是独立写的，没有移植任何代码。设计上的取舍来自对若干参考方案的阅读，
下文只保留编号（编号含义见 `docs/01-project-analysis/`）：

| 编号 | 借用的思路 | 落点 |
|---|---|---|
| PA-06 | 采集底座要「可治理」而不是「能连多少」 | 分层、配置版本、质量码贯穿 |
| PA-10 | 通道与设备两级模型、代次隔离 | `Channel` / `Device`、`DeviceActor.Generation` |
| PA-13 | 插件边界收窄到两个接口 | `IDriver` / `IConn` |
| PA-16 | 单二进制 + 内嵌前端 | `Uic.Host` 托管 WASM 静态目录 |
| PA-18 | 小而真的驱动包工程：黄金帧 + 一致性用例 | `testdata/golden`、T1–T18 |
| PA-22 | 编解码与传输分离 | `Uic.Codec.Modbus` / `Uic.Transport` |
| PA-34 | 浏览器形态的边缘控制台 | `Uic.Ui`（Uno WASM，只走 REST/SSE） |

反面教材同样重要：PA-02 / PA-12 那种「把每个协议都做成一个页面」的做法会让
控制台与协议实现耦合，所以这里的 UI 不认识任何驱动，只认北向契约。

## 首版范围

### 做了什么

- **南向**：Modbus TCP / RTU / ASCII 主站；S7comm 主站（S7-300/400/1200/1500）；
  OPC UA 客户端（匿名，SecurityPolicy None / Basic256Sha256）；MQTT 订阅方
  （主题过滤器 + JSON 字段取值，QoS0/1、TLS，只读）；Modbus TCP 从站
  （仅用于仿真与测试）；虚拟设备（正弦/斜坡/随机/常量 + 五类故障注入）。
- **北向**：REST（设备/点位/写入/配置/命令/诊断/模板/导入导出）、SSE、MQTT 发布 + 可选下行命令、OPC UA 服务端。
- **治理**：内容寻址配置 + diff + 回滚 + 热更新；旧配置迁移链；模板库与参数化批量实例化；
  点表 CSV 导入（地址表达式、按 id 合并、默认 dry-run）；配置导出/导入；
  数据类型含标量 13 种与定长数组（`count`，元素类型 + 元素个数正交表达）；
  持久化上报队列（分区分位点、三重上限）；设备状态机 + 熔断 + 探活恢复；
  点位级采集周期（`point.interval` 覆盖设备周期，按周期分桶合并读取）；
  `retries` 重试（只重试临时性失败，重试前断开避免迟到帧；地址错不参与熔断）；指标 + 环形日志 + 可选 JSON 行文件日志（按大小轮转） + 诊断包。
- **凭据**：配置里不落明文。`northbound.rest.token` / `northbound.mqtt.password`
  只能是 `${env:NAME}`、`${file:PATH}` 或 `enc:v1:...`（AES-256-GCM，密钥来自
  `UIC_SECRET_KEY` / `UIC_SECRET_KEY_FILE`）；`uic secret keygen|seal` 负责生成密钥与密封。
  解析失败一律硬失败（空值只表示不提供静态令牌，匿名访问并不存在），明文只存在进程内存。
- **审计**：追加写 NDJSON（`data/audit/`），按天与按大小切分、按份数保留、逐条
  `Flush(flushToDisk: true)`；记录配置变更、逐条写入（旧值 + 新值）、REST/MQTT 命令、
  登录/登出/改密/越权、账号变更与审计导出；`GET /api/v1/audit` 查询、`/api/v1/audit/export` 导出。
  写失败只告警并暴露指标，不阻断业务（`AuditLog` 在 `Uic.Obs`，选项在 `Uic.Domain`）。
- **TLS**：REST 可在 Kestrel 上直接监听 HTTPS（PFX / PEM，PFX 口令只接受引用）；
  MQTT 客户端用 `SslStream` 包裹，校验服务端证书链与主机名，自签 Broker 通过
  `northbound.mqtt.tls.caFile` 走自定义信任根。证书与监听地址的变更需要重启。
- **账号与权限**：本地账号库 `data/security/users.json`（PBKDF2-SHA256 / 210k 迭代 /
  每账号随机盐 / 原子替换写盘）；首次启动生成随机 `admin` 口令并强制改密；会话 Token
  内存保存 12 小时；四个内置角色（`admin` / `operator` / `viewer` / `auditor`）与九项权限，
  端点 `.RequirePermission(...)` + 中间件默认拒绝；`uic user list|reset-password` 作为离线兜底。
- **控制台**：Uno WASM 八个页面（设备 / 驱动 / 点位 / 监控 / 日志 / 审计 / 配置 / 账号）；
  登录 + 强制改密 + 按权限隐藏导航与写操作；设备页可新建通道 + 设备（含首个点位），
  提交走 `config/validate` → `config/apply`。
- **验收**：黄金帧 20 份（含 4 份数组帧）；T1–T18 全部有对应用例，其中 T15–T18 跑在 libc 开出来的
  真实伪终端上；MQTT 先在测试内按规范独立实现的最小 broker 上做协议级验收（发布、
  订阅、入站 PUBACK、下行命令去重与权限、南向驱动的缓存/重连/过期），再用官方 mosquitto 容器跑一次互操作
  （`scripts/mqtt-interop.sh`：上线、重启重连、命令请求/结果、kill -9 后由 broker
  代发遗嘱）；WASM 控制台用 `scripts/wasm-smoke.sh` 发布后
  在 Playwright 里真跑一遍（Chromium 与 Firefox 各一遍：`index.html` / 引导脚本 /
  SPA 回退 / 登录与强制改密 / REST 首屏 / 在设备页真填表新建一台设备并等它出现在列表里）；容量与写延迟
  由 `scripts/capacity-bench.sh` 起真宿主按 PRD 档位（200 设备 / 10,000 点）量出来，
  不从代码推断。
  见 [testing.md](testing.md)。

### 刻意没做

| 没做 | 原因 | 怎么补 |
|---|---|---|
| Modbus 从站的 RTU/ASCII 封装 | 仿真只需要一条 TCP 链路就能覆盖主站的全部路径 | `ModbusSlaveHandler` 与封装无关，加一层 framing 即可 |
| OPC UA **服务端**的签名/加密 | 证书生命周期（签发、轮换、吊销、信任列表）是独立的运维负担 | 配置里已显式拒绝非 `None`，加实现时同步放开校验。注意这是**北向服务端**的缺口：南向客户端已支持 `Basic256Sha256` |
| OPC UA 用户认证 | 没有用户后端，匿名读 + 可选匿名写足够覆盖调试场景 | 北向接 `IUserIdentityValidator`；南向在 `OpcUaClientDriver` 里换掉 `UserIdentity` |
| MQTT QoS2 | 现场 QoS1 + 持久化队列已经够用，QoS2 的握手复杂度不划算 | 在 `MqttClient` 里加 PUBREL 状态机 |
| MQTT WebSocket | 现场 Broker 走原生 TCP；浏览器不直连 MQTT，控制台只走 REST/SSE | 在 `MqttClient` 的连接建立处换成 WebSocket 传输 |
| MQTT 命令的 ACL 与跨重启幂等 | MQTT 命令这一版没有用户身份（TLS 只保护链路，不认证发布者），幂等窗口在进程内，重启即清空；命令本身已进审计 | 在 Broker 侧做主题 ACL；需要跨重启幂等时把幂等台账落到外部存储 |
| 多协议（OPC DA / IEC 104 / MC / FINS …） | 首版把一条链路的每一层都做透，比铺开十个半成品有用 | 见下「扩展点」 |
| S7 的符号寻址（0xB2）、S7Plus、TPDU 分片重组、多连接并发 | 前三项要么依赖 S7-1200/1500 的编程环境（符号表/证书），要么需要把「一个 PDU」重组成多帧状态机；多连接并发与调度器「一设备一通道串行」的模型冲突。**这些都明确报错，不静默降级** | 符号寻址需要先有符号表的导入路径（TIA 导出 XML/CSV），再在驱动里加功能码 0xB2；分片要在 COTP 层加重组缓冲 |
| object / file 寻址的驱动 | 四类寻址的模型、校验与模板参数化都已就绪（同一模板可产出 Modbus 与 S7 设备），但只有 register 与 symbol 有驱动消费。object 需要目标协议的对象模型（BACnet 的 analogInput、CIP 的类别/实例/属性），file 需要文件记录语义（Modbus FC20/21）——现在造一个只会是「为填满矩阵而写的假驱动」 | 接 BACnet/CIP 时按对象模型落地 `Address.Object`；file 寻址可随 Modbus FC20/21 一起做（内置从站同步补） |
| MQTT 南向的推送式采集 | 订阅本身已实现（`Uic.Driver.Mqtt` 内部维持订阅并缓存），但 `ReadAsync` 仍由调度器轮询——缓存值会被反复采样。改成推送驱动要重做看门狗、重试/熔断与代次隔离，属于独立的调度改造 | 在 `DeviceActor` 里加订阅分支；MQTT 驱动的订阅缓存已经就绪，接上回调即可 |
| OPC UA 客户端订阅接入采集主路径 | 订阅本身已实现（`ISubscriptionConn`），但调度器仍是轮询。接进去要重做「没有周期可依据」的看门狗、推送驱动的重试/熔断与代次隔离，属于独立改造 | 在 `DeviceActor` 里加一条订阅分支：首值到达前保持 `stale`，断线重订阅时按代次丢弃旧回调 |
| 报警引擎与专用时序库 | 报警需要独立的状态机（阈值、抑制、确认）；专用时序库需要保留策略与索引，塞进采集进程会把边缘设备压垮 | 历史查询已先用 journal NDJSON 做了「时间范围 + 降采样 + 分页」的轻量实现（FR-NOR-08）；报警与更高数据量的保留策略仍按 PRD 路线图 Wave 2 单独立项 |
| 计算点 / 订阅型协议（COV、事件、通知） | 计算点依赖「输入质量传播」的产品定义；订阅型协议在 Wave 2 | `Pipeline` 已留出质量传播点；驱动侧 `Capabilities.Subscribe` 已是四态声明，接入时按 `RuntimeDependent` 逐步放开 |
| Sparkplug B、进程外驱动 | PRD §6.1 明确列为不做；进程外驱动需要稳定的 IPC 契约与进程生命周期管理 | Sparkplug 可作为一个 MQTT sink；进程外驱动按 `IDriver` 的进程内形状定义 IPC 协议后再拆 |
| 浸泡测试（72h）、安全扫描、三平台 CI 矩阵 | 验收门槛里的工程活动，不是代码功能；本仓库只提供可自动化的部分 | 用 `scripts/` 起容器化长时间运行；安全扫描接入 CI；产物矩阵见路线图 |

能力四态（`Supported` / `Unsupported` / `RuntimeDependent` / `Experimental`）是这些「没做」
能诚实表达的前提：驱动不会因为「本版本没实现」而谎称支持，控制台据此渲染徽标。

## 几个「为什么这么做」

架构决策写在 [architecture.md](architecture.md)，这里只记录**看上去可以省、但实际不能省**的地方。

- **OPC UA 客户端用官方协议栈（`Opc.Ua.Client`），不手写二进制层。** UA 的握手不是「一段帧」：
  Hello/Ack → OpenSecureChannel → CreateSession → ActivateSession → CreateSubscription → Publish，
  每一步都有 Nonce、序列号、续期与错误重协商。手写出来的实现通常「连自家的服务端能通」，
  遇到第三方栈就卡在证书链或 Nonce 复用上。这与 S7 的选择不矛盾：S7 的报文是结构化的
  TPKT/COTP/PDU，可以用黄金帧逐字节钉住，而且没有加密握手；UA 的安全通道没有黄金帧可钉。
- **South 与 North 不共用 `Uic.OpcUa`。** 客户端驱动只引用 `Uic.Domain` + `Uic.Driver`。
  一旦让它认识 `PointCache`，驱动层就会反向依赖采集侧，「一个驱动能被单独测」这条边界就没了。
  同名协议栈依赖是两回事：一边用 `.Server`，一边用 `.Client`。
- **订阅不进采集主路径。** `Capabilities.Subscribe = supported` 说的是「这个驱动能提供订阅」，
  不是「采集走订阅」。device actor 的看门狗、熔断、重试、死区全建立在「有周期」的前提上；
  推送驱动的采集需要另一套语义，半接进去比不接更危险（会出现「有时候收不到但没人知道」）。
- **PKI 落在 `<dataDir>` 下，不散落在当前目录。** 客户端证书是**每台网关一份**的资产：
  放数据目录才会跟着备份/迁移/容器卷一起走，换工作目录不会静默生成第二张证书导致服务端
  拒绝。`labels.pkiDir` 允许覆盖，是给「证书由外部挂载管理」的部署留的口子。
- **端点严格精确匹配，不做「最安全的那个」。** 服务端公布的端点集合是它**愿意**提供的档位；
  客户端配置是运维**要求**的档位。自动挑选会让配置形同虚设，而「签名不可用就退回明文」
  这类兜底更糟——日志里看起来是加密的，抓包是明文。选不到就失败并列出服务端实际档位。
- **两种时钟分开用，不互相顶替。** 采集节奏、停滞判定、Outbox 的 TTL 用单调时钟
  （`Stopwatch` / `TimeProvider.GetTimestamp`）；只有对外的时间戳（样本 `collectedAt`、
  日志行、审计条目）用墙钟。理由是同一次 NTP 校时能让墙钟往前跳一小时：用墙钟算节奏，
  调度器会瞬间补发一小时积压；用墙钟算停滞，所有健康设备会同时报假警；用墙钟算 TTL，
  整条积压会被判成过期清空；用墙钟算 uptime，`/health` 会凭空多出一小时甚至变成负数。
  **停滞判定要求墙钟与单调时钟同时越过阈值**——只有这样才能把「校时」和「真的不动」
  分开，这也是 `fault/*` 用例注入可拨时钟的入口。
- **设备下线要把「关于它的记忆」一起删掉。** 热更新移除一台设备时，除了停止 actor，
  还有三份副本要处理：看门狗里补发用的上一状态与停滞去重窗口、`MqttSink` 里重连补发用的
  状态副本、以及 broker 上 retained 的 `$state`。前两份不清会随反复改配置无界增长，
  而且让「删了又加回来」的设备既不重推状态、也吞掉第一轮停滞告警；第三份让新订阅者
  读到一台已经不存在的设备。清 retained 的协议动作是往同一主题发空载荷并置 retain。
- **停止采集循环有等待上限。** 驱动可能卡在一条不响应取消的读里（协议栈死锁、串口假死），
  而 `StopAsync` 原本是无限等循环退出。那会让「改一台设备」变成无限等待、关机也卡住，
  正好毁掉「配置应用 ≤ 2 s」这条验收。超时后放弃等待是安全的：代次已经递增，
  飞行中的读结果会被丢弃，循环回来后会看到取消信号而退出。
- **命令去重窗口用单调时钟。** `_cache` 原本按墙钟判断 5 分钟窗口。一次 NTP 前跳
  会让所有在途条目瞬间「过期」，同一个命令 id 再来一次就会**真的再执行一遍**——
  对一个写点位的命令，那是重复的物理动作；后跳则让窗口永不失效、缓存只增不减。
  审计与对外结果里的 `at` 仍然是墙钟，两者必须分开。

## 与文档的偏差

实现过程中发现文档有歧义或不可行的地方，按下面处理。**每一条都是有意为之**，
不是漏改：

1. **配置里的 `dataType` 与值里的 `type` 统一为小写线上名**（`uint16`）。
   文档两处写法不一致；统一到后者，并让解析大小写不敏感，旧文件里的 `uInt16` 仍能读。
2. **`/api/v1/config/validate` 对语法错误返回 200 + `valid:false`**，而不是 422。
   校验端点的产出就是「问题清单」；只有请求体整体不是 JSON 时才 422。
   `apply` / `rollback` 仍然一律 422 —— 它们要求「任何一条不满足就不生效」。
3. **SSE 不重放历史**。事件带 `id` 供客户端去重，但服务端不维护回放缓冲。
   断线重连由客户端先拉一次快照再吃增量，比在服务端留一份历史更省内存也更简单。
4. **`outbox.drop` 返回未实现**。丢弃待发数据必须有保留策略，否则就是「一个按钮删掉现场数据」。
   接口先占位并明确失败，好过悄悄做成「清空队列」。
5. **写入不区分「权限」与「不支持」的质量码**，两者都映射到 `disabled`。
   UI 只需要区分「这个点不能写」与「写了没成功」，多一个枚举值只会让前端多一个分支。

## 扩展点

### 加一个协议

1. 实现 `IDriver` / `IConn`（`src/Uic.Driver/Contracts.cs`）；
2. 在 `DriverMeta` 里声明支持的传输、寻址范式与能力；
3. 在 `Uic.Host/Program.cs` 与 `Uic.Runtime` 的注册处 `Register(...)`；
4. 加黄金帧与一条回环用例：能用静态字节描述的协议放 `testdata/golden/<proto>_*.json`，
   需要在运行时构造字节的（S7 的 TPKT/COTP 头带长度与引用号）写在测试代码里，
   但同样是手算十六进制逐字节比对；
5. 配置里 `channel.protocol` 写新驱动的 `Meta.Id`。

分层照 S7 的三段走最省事：`Uic.Codec.<proto>`（纯编解码，能被黄金帧直接调用）+
`Uic.Driver.<proto>`（会话与驱动）+ `Uic.Sim.<proto>`（进程内从站，供回环与
`uic sim`）。值编解码（字节序/BCD/数组）直接复用 `Uic.Driver/ValueCodec.cs`，
不要再写第二份——两份实现在奇数字节偏移或数组边界上迟早分叉。

不需要改：调度器、缓存、北向、UI。这就是把驱动边界收窄到两个接口的目的。

### 加一个北向通道

1. 在 `Uic.Northbound`（或新项目）实现 `IOutboxSink`，`DeliverAsync` 失败必须抛；
2. 在 `GatewayRuntime.ConfigureSinks` 里按配置注册，并把 sink 名加进 `SampleFanout` 的
   `Targets` —— 队列按 sink 名分区，名字就是契约；
3. 如果它是「拉模型」（像 OPC UA），不要走队列，直接读 `PointCache` 并订阅变化。

### 加一个指标

`MetricsRegistry.Counter/Gauge` 是唯一入口，命名遵循 Prometheus 约定：
`uic_<名词>_total`、`uic_<名词>_seconds`。不要在中途引入第二种指标库。

## 脱敏约定

这个仓库里的所有文本（代码注释、文档、示例、测试数据）都遵守同一套约定：

- 不出现任何参考项目的名称、仓库地址、作者、商标或包前缀；
- 需要引用设计来源时只用编号（`PA-xx`），编号与内容的对应关系留在根目录 `docs/` 里；
- 示例里的站点、设备、点位一律用中性名字（`demo-site`、`meter-1`、`holding`）；
- 第三方库只写包名与版本，不写「某某产品同款」之类的措辞。

## 代码风格

- 注释写**为什么**，不写**是什么**。`// 递增计数` 这种注释没有价值，
  `// 整批成功才推进位点：宁可重复，不可丢` 才是要留下的东西。
- 不用 `#region`，不用 `TODO`。没做的事写进文档的「刻意没做」，不留在代码里当噪声。
- 错误信息面向现场工程师：说清「哪个设备、哪个点位、什么原因」，
  而不是把内部异常原样抛给 UI。
- 捕获异常必须要么处理、要么记录、要么重新抛出。空 `catch` 只允许出现在
  「关闭期清理」与「测试收尾」这两处，并且要有注释说明。
