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
- **配置态与运行态是两个字段，不能合并成一个 `state`。** 「运维主动停采」和「设备出故障」
  是两种完全不同的处境：前者不该报警、不该派人去查链路，后者必须。合成一个字段后，界面上
  两者长得一样，运维只能靠经验猜。因此 `configuredState`（来自配置的 `enabled`）与
  `runtimeState`（`configured/connecting/collecting/degraded/faulted/disabled`）各自透出，
  停用的设备仍然出现在设备列表里、点表也照旧可读——配置还在，只是不采。
  用例 `contract/disabled_device_shows_both_states` 用「一台停用 + 一台启用」的对照来钉：
  单看一台停用设备时两个字段都是 `disabled`，把一边抄成另一边照样能过。
- **路径前缀判定必须与路由一样忽略大小写。** ASP.NET Core 的路由匹配本身不区分大小写，
  `/API/v1/devices` 会命中同一个端点；而鉴权中间件原本按 `Ordinal` 判 `"/api/"` 前缀，
  于是大写变体被当成「非 /api/ 的静态资源」直接放行——一次大小写改写就是完整的鉴权绕过，
  而且不需要令牌。这不是理论问题：扫描器最先试探的就是路径大小写变体。
  限流用同一个前缀判定，同样的写法会让攻击者改个大小写就拿到无限配额。
  权限检查、强制改密的白名单也都走这一个判定，避免「改了一处漏了另一处」。
  用例 `contract/api_requires_bearer_token`（未认证 + 已认证两个方向）与
  `contract/rate_limit_returns_429_with_the_shared_code`。
- **驱动注册是「不匹配就拒绝」，不是「尽力兼容」。** `DriverRegistry.Register` 校验
  `DriverMeta.SdkVersion`：版本对不上意味着接口形状可能已经变了（宿主没实现的方法、
  驱动不认识的上下文），运行时才炸会变成「跑了几小时才在某个分支上出错」，
  启动时炸出来便宜得多。拒绝后不留半注册状态，重复 id 也不覆盖先注册的驱动。
  用例 `sdk/register_rejects_sdk_version_mismatch`、`sdk/register_rejects_duplicate_id`。
- **优雅退出的顺序本身是一条验收项，不是「DisposeAsync 不抛异常」。** 停机时
  队列里还没投走的记录必须被 flush 干净，且发生在发 `$death` 之前——否则 retained 的
  `$death` 一落地，订阅者就以为数据到此为止，而实际上还有一批排在盘上没送走。
  反向也要钉：flush 有 3 秒上限，对端一直不可用时停机不能无限等，未投完的记录留在盘上
  下次重放。用真实进程内 runtime + 可脚本化的 sink 验证顺序与副作用，而不是只断言「没抛异常」。
  用例 `shutdown/flush_runs_before_death_and_clears_the_queue`、
  `shutdown/flush_gives_up_after_the_deadline`。
- **订阅不进采集主路径。** `Capabilities.Subscribe = supported` 说的是「这个驱动能提供订阅」，
  不是「采集走订阅」。device actor 的看门狗、熔断、重试、死区全建立在「有周期」的前提上；
  推送驱动的采集需要另一套语义，半接进去比不接更危险（会出现「有时候收不到但没人知道」）。
- **PKI 落在 `<dataDir>` 下，不散落在当前目录。** 客户端证书是**每台网关一份**的资产：
  放数据目录才会跟着备份/迁移/容器卷一起走，换工作目录不会静默生成第二张证书导致服务端
  拒绝。`labels.pkiDir` 允许覆盖，是给「证书由外部挂载管理」的部署留的口子。
- **端点严格精确匹配，不做「最安全的那个」。** 服务端公布的端点集合是它**愿意**提供的档位；
  客户端配置是运维**要求**的档位。自动挑选会让配置形同虚设，而「签名不可用就退回明文」
  这类兜底更糟——日志里看起来是加密的，抓包是明文。选不到就失败并列出服务端实际档位。
- **两种时钟分开用，不互相顶替。** 采集节奏、停滞判定、Outbox 的 TTL、命令去重窗口、写入
  幂等窗口、登录锁定窗口、写后确认等待、变化率死区的 Δt、MQTT 南向的过期判定、限流桶的
  配额恢复与空闲清扫用单调时钟（`Stopwatch` / `TimeProvider.GetTimestamp`）；只有对外的时间戳
  （样本 `collectedAt`、日志行、审计条目）用墙钟。理由是同一次 NTP 校时能让墙钟往前跳一小时：用墙钟算节奏，
  调度器会瞬间补发一小时积压；用墙钟算停滞，所有健康设备会同时报假警；用墙钟算 TTL，
  整条积压会被判成过期清空；用墙钟算 uptime，`/health` 会凭空多出一小时
  甚至变成负数；用墙钟算登录锁定，一次校时就能让被锁的攻击者继续试口令、也能把合法用户永久
  锁死；用墙钟掐「写后确认」的等待，NTP 回拨一小时会让这一个写点位的请求挂住一小时；用墙钟算
  变化率死区的 Δt，一次前跳会把 1 秒的尖峰压成 1 小时、把该报的变化吞掉，后跳则凭空造峰；
  用墙钟算限流配额，前跳会让一个被打满的攻击者瞬间拿回全额配额（限流形同虚设），后跳则让
  配额永不恢复、正常客户端被永久 429；
  用墙钟算「多久没收到 MQTT 报文」，前跳会让所有南向点位瞬间误报 stale，后跳则让过期点位
  无限期报 Good——把旧数据当新鲜值，正是最危险的那种假象；用墙钟算 REST 写的幂等窗口，
  前跳会让客户端重投的同一个 `X-Idempotency-Key` 被当成新请求、对设备把同一个物理动作再执行
  一遍，后跳则让缓存永不过期、一次真正的新写入被当成重放而根本不执行。
  **停滞判定要求墙钟与单调时钟同时越过阈值**——只有这样才能把「校时」和「真的不动」
  分开，这也是 `fault/*` 用例注入可拨时钟的入口。
  单调时刻存进 `Sample.MonoAt` / `IdempotentResponse.MonoAt` 时统一归一化到 `TimeSpan.Ticks`：时间戳频率由注入的
  `TimeProvider` 决定，判定处若按系统频率换算会把 Δt 整体算错（这条边界踩过一次，
  由 `pipeline/deadband_rate_ignores_a_clock_jump` 钉住）。北向的幂等窗口直接取
  `Scheduler.Time`（与 uptime 同一个注入源），不必再给 `NorthboundContext` 添参数。
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
- **登录锁定窗口同样用单调时钟。** 失败尝试的 1 分钟窗口与 60 秒锁定都是防爆破的
  安全边界，原本按墙钟算。墙钟前跳会让窗口里所有旧尝试瞬间失效——被锁的攻击者只要
  等一次校时就能继续试口令；后跳则让 `now - t` 变负、窗口永远清不掉，合法用户被永久
  锁死。现在失败记录只存 `GetTimestamp()`，到期时刻按「还剩多久」换算回墙钟再对外
  返回（`retryAfterSeconds` 仍然是对客户端有意义的秒数）。用例
  `auth/lockout_survives_a_clock_jump` 只拨墙钟、不拨单调时钟，钉住这条边界。
- **字节序只作用在 32/64 位数值上，`string`/`bytes` 不重排。** `ValueCodec.Normalize`
  原本对任何类型都套字序变换，而 `byteOrder` 是**设备级**的、点位可覆盖：现场常见
  「为了某个 `float32` 把设备设成 `cdab`」，于是该设备上所有字符串点位跟着逐字符错位
  （`ABCD` 读成 `CDAB`）。这类错误不会报错——读回来仍然「像是有值」，只是内容是乱的，
  往往要到对账或人工巡检时才发现；写方向更危险，错位的字节会真的下发给设备。
  字序的语义本来就是「多个 16 位寄存器拼成一个数值时的排列」，字节流没有「字」可排。
  用例 `codec/byte_order_does_not_scramble_text` 把四种字序下字符串与 `bytes` 的读、写都钉住。
- **写入值要反算回原始值，写和读才是同一个口径。** 读方向做 `raw × scale + offset`，
  写方向若把北向传来的工程量原样下发，同一个点位就会「写 10、读回 1」——现场只会
  以为设备不接受设定值，于是反复下发、反复「不生效」。更隐蔽的是写后确认：回读比较
  发生在原始值域里，两次都错得一样，于是返回 `verified: true`，把一个错误的写入
  伪装成成功。反算放在 `DeviceActor`（`Pipeline.ToRaw`），不放进驱动——驱动不该
  知道点位表里的工程量口径；同时写后确认拿到的期望值也是同一份原始值，不必再换算。
- **写入值越界与超长文本直接报错，不静默取模/截断。** `(ushort)70000` 是 4464：
  设备收到一个用户从未输入过的值，API 却报 `ok`，现场只会表现为「设备不听话」，
  几乎不可能联想到写入越界。同理，5 个字符写进 2 个寄存器的点位若截断成 4 个字符，
  配方名、序列号会被悄悄改短，直到对账才发现。边界值本身要能写，所以检查是
  「不超过类型上/下限」，而不是「小于某个保守值」；double → float 的越界转换
  在 .NET 里不抛异常而是变成 Infinity，也一并拒绝。
- **客户端语法错误不能冒成 500。** 写请求体由调用方拼装，多一个逗号、少一个引号、
  被截断的报文都是常态。`JsonDocument.Parse` 遇到这些抛的是 `JsonException`，
  它不是 `ApiException`，若不在写接口就地翻译成 `400 request.invalid_json`，
  它会一路冒到最外层的兜底 catch 被当成「网关自身出了故障」返回 500——而 500
  在运维那里专指网关故障、会触发告警、进错误看板、被当成发布事故，把一次客户端
  格式错误误报成网关崩溃，既掩盖了调用方的 bug，也污染了对网关健康度的判断。
  配置导入早就把语法错误归成客户端错误，写接口必须一致：请求不成立就是 400，
  网关没坏。用例 `contract/malformed_write_body_is_a_client_error` 双向钉住这条。
- **限流桶必须能被清掉，否则限流器自己成了拒绝服务的手段。** 桶按来源 IP 建，
  全网扫描器每天能贡献几十万个不同地址；每来一个地址建一个永久桶，内存就是
  无上限增长。清扫按**时间**触发（至多每分钟一次），不是「每 N 次请求」——
  桶多的时候按请求触发会把遍历所有桶的成本摊到每次调用上，压力越大越慢。
  两条配套规则：空闲窗口不得短于「空桶回满」的时间（否则每次清扫都会丢掉一个
  本该仍受限的桶，等于替对方把配额立刻刷新）；清扫在桶锁外做，避免与
  `TryTake` 形成反向加锁顺序。用例 `limits/prune_drops_idle_buckets`、
  `limits/prune_keeps_active_buckets`、`limits/idle_window_cannot_undercut_a_refill`。
  空闲判定同样用单调时钟，校时跳变不会把活跃桶误判成空闲。
- **429 只有一个线上 code：`request.rate_limited`。** 三个来源（读/写配额、
  SSE 连接数上限、登录失败锁定）共用它，调用方写一套「我被限流了」的分支就够。
  这里曾经漂过：实现统一返回 `request.rate_limited`，`northbound-api.md`
  却写着 `request.too_many_requests`——文档是客户端错误表的来源，这种漂移会
  让客户端认不出被限流。用例 `limits/too_many_requests_uses_one_wire_code`
  与 `contract/rate_limit_returns_429_with_the_shared_code` 钉住这条口径。
- **幂等键绑定请求体，换了内容要报 409。** 只按 key 重放的话，调用方把同一个 key
  配上一个新值发过来，会拿到首次的 `ok: true` / `verified: true`——它以为新值已经
  写进设备，而那次写入从未发生。这类「静默丢写 + 假成功」在现场几乎不可能追查：
  日志、审计、接口返回值三者都说写成功了。键被复用到另一个请求上通常是调用方
  拼接 id 的 bug，选择报错而不是「最后一次为准」，是为了让它立刻暴露。
  已有的行为保持不变：同一份请求体重投仍然原样重放（那才是幂等键的用途）。
  幂等键绑定的不只是请求体，还包括**运行时实例**：台账里存的是「首次写入时刻的
  单调时刻」，这个数只在产出它的那个单调时钟上可解释。做成 `RestApi` 上的静态字段
  会让同一进程里新起的运行时读到上一个运行时的条目——两个时钟读数相减是负数，
  `elapsed < window` 永远成立，重投被判成重放，返回的是另一套配置、另一批设备的
  首次响应，而设备一次都没被写过。现场症状是「换完配置后点写按钮没反应」，
  而日志、审计、接口返回值三者全都正常，与上面那条 409 同属「假成功」家族。
  现在台账由 `NorthboundContext.Writes`（`IdempotencyCache`）持有，与它记的时钟同源。
  用例 `contract/idempotency_ledger_is_per_runtime`（让第二个运行时的单调时钟更小，
  还原成静态字典后 FAIL：`a key used against another runtime must not be treated as a
  replay: expected <"false">, got <"true">`）。
- **分页游标不合法时报 400，不「当成从头开始」。** 游标对调用方是不透明串，
  服务端只认自己发出的那个形式。静默回落成 `offset=0` 会重新发一遍第一页，
  而 HTTP 仍是 200：客户端要么看到「整页重复」，要么按自己的去重逻辑跳过一页，
  两种都表现为「数据少了/多了」，日志里一片正常。校验方式是「重新编码后与原串
  逐字符相同」，数字固定用不变量格式（避免换进程区域设置后同一串游标解出不同
  结果）；补零、夹空白、自行重编的游标一律 400，而不是替调用方猜一个偏移。
  用例 `contract/pagination_cursor_is_validated` 把正常翻页、三种非数字内容
  与改写形式都钉住。
- **S7 请求的项数字段只有一字节，超了必须当场失败。** `param[1]` 是 8 位项数，
  而编解码层原本只拦「参数长度溢出 16 位」（5000 项）。256 项会写成 0——PLC 一项
  都不读，但整帧仍是合法请求、返回码正常；257 项写成 1——只读第一项，其余点位
  拿到错位的数据。两种都表现为「部分点位值不对」，帧本身毫无报错迹象。
  驱动侧的 PDU 预算在 240 时只放得下不到 20 项，所以现场很难踩到，但
  `S7Read.BuildRequest` 是公开的编解码 API，不能依赖调用方一定切过片；
  这类「越界就截断、截断后看起来仍然成功」的写死上限是同一类缺陷
  （地址字段超过 3 字节时也一样）。用例 `s7/item_count_fits_in_one_byte`。
- **凭据清单只能有一份，读侧脱敏与写侧还原必须共用它。** `RestApi.Redact` 原本按
  key 名精确匹配 `password`/`token`/`secret`，而 `RedactedSecrets` 里有另一份同样的
  清单。PFX 口令的 key 是组合词 `certificatePassword`，于是两侧同时漏掉：读接口把它
  原样返回（运维投屏、诊断包、浏览器缓存都带着它，而文档写的是「读接口统一显示 `***`」），
  客户端再原样提交时也不会命中「占位符还原」分支，明文被直接写进配置版本。
  两份清单同时漏掉是一种特别难发现的形态——没有任何一处会报错，两侧还互相「印证」
  了对方是对的。现在清单收在 `RedactedSecrets.SecretKeys`，脱敏侧通过
  `IsSecretKey` 复用；`IsSecretPath`/`MaskIfSecret`（diff 用）本来就吃这份清单，一起修好。
  用例 `contract/every_credential_field_is_redacted`（读、diff、apply 响应三处都验，
  修复前 FAIL：`expected <"***">, got <"${env:UIC_CONTRACT_PFX_PASSWORD}">`）与
  扩充后的 `config/redacted_secrets_are_restored`（还原不得把 `***` 写成字面量）。
- **压缩之后发号器不能回退，否则重启会把新数据当旧数据丢掉。** `Partition.Load`
  原本只从 `records.ndjson` 里的记录推进 `_nextId`，完全不看 `acked.id`。而
  `CompactIfEmpty` 会在队列清空且文件超阈值时把数据文件重写成空文件（位点文件保留）。
  两者一叠：压缩后重启，发号器从 1 重新开始；重启加载时这些 id 都 ≤ `_acked`，
  被 `rec.Id > _acked` 判成「早已投递」而跳过。更糟的是 `Enqueue` 是成功的——
  记录写进了磁盘、调用方收到受理、`Pending` 也是对的，只有重启才静默归零，
  既没有错误也没有丢弃计数。因此发号下界必须同时取「数据文件里的最大 id」与
  「确认位点 + 1」。用例 `outbox/compaction_does_not_reuse_ids`（修复前 FAIL：
  `expected <1>, got <0>`）。
- **热更新必须先做无副作用的引用预检，再动任何 actor。** `Scheduler.ApplyAsync` 的顺序是
  「先卸旧设备、再建新设备」（同一串口不能被新旧两个 actor 同时持有），而「驱动是否已注册」
  只有 `CreateActor` 才检查——配置校验只要求 `protocol` 非空，它拿不到驱动注册表。
  于是 `channel.protocol` 拼错时：第一台设备已经被 Dispose、新的建不出来、`_config` 还停在
  旧值，接口只回一个 500。运行态与配置从此永久错位——一台仍在配置里的设备永远不出数，
  再 apply 一次还可能按设备顺序把另一台也拆掉。现在卸载之前先把「设备 → 通道 → 驱动」
  全量解析一遍（`Preflight`），有问题整份抛 `ConfigException`（422 + `channels[x].protocol`），
  一个副作用都不产生；启动路径共用同一预检，把「驱动没注册」从进程崩溃（退出码 1）
  变成配置错误（退出码 2，并一次列出全部未解析引用）。
  预检本身抽成 `Scheduler.CheckReferences`，`uic check` 也用同一份、同一组驱动，
  否则静态检查会放过一份启动必然失败的配置——那正是「check 通过、启动退出码 2」
  这种最难查的不一致。
  用例 `lifecycle/failed_apply_leaves_running_devices_alone`（修复前 FAIL：
  `a refused apply does not dispose the device that was already running`）、
  `contract/apply_refuses_unregistered_driver`（修复前 FAIL：
  `expected <UnprocessableEntity>, got <InternalServerError>`）、
  `sdk/check_command_rejects_unregistered_protocol`（去掉 `check` 里的引用检查后 FAIL：
  `expected <2>, got <0>`）。
- **「版本没落盘」也必须回滚引擎，否则三份状态各说各话。** `ApplyConfigAsync` 的顺序是
  「先让调度器收敛、再提交版本」，这么做是为了避免「版本已切换但设备没重启」的假成功。
  但提交本身会失败——磁盘满、只读挂载、配额打满。此时接口回 500（运维据此认为改动没生效），
  设备跑的却是新配置，磁盘上仍是旧版本（下次重启悄悄退回）。三处各说各话，排障时无从判断
  现场到底是哪一份。现在提交失败会把设备退回上一份配置再抛原异常：磁盘写不进去是运维问题，
  但「apply 失败 = 什么都没变」必须成立。回滚本身失败只能记 critical——那时磁盘已经出问题，
  再抛第二个异常只会掩盖真正的原因。
  用例 `lifecycle/unpersisted_apply_rolls_the_devices_back`（去掉回滚后 FAIL：
  `the devices were rolled back to the persisted configuration: expected <"42">, got <"7">`），
  注入点是真实的文件系统权限（把 `config/versions` 改成只读），不是假实现。
- **北向 sink 的装配失败也必须在副作用之前。** `ConfigureSinks` 的顺序是「先摘旧 sink、
  再建新的」：等它发现 MQTT 的自签 CA 读不出来时，旧连接已经断开、版本也已经提交，
  一次报 422 的 apply 实际把 MQTT 上报停了。现在 CA 装载与凭据解析一起前移到无副作用阶段。
  顺带修掉一个更隐蔽的问题：`X509Certificate2Collection.ImportFromPemFile` 对「内容不是 PEM」
  的文件**不抛异常**，只是导入 0 张证书，而空集合在本仓库的含义是「没有自定义 CA，用系统
  信任库」——配错的自签 CA 会静默退回系统信任库，直到握手时才以「证书链不受信任」出现。
  现在非空却解析不出证书的文件按配置错误拒绝；空文件仍然是合法的「显式不配 CA」。
  用例 `lifecycle/bad_mqtt_ca_fails_before_anything_changes`（去掉前置检查后 FAIL：
  `a config refused before its side effects does not commit a version`；去掉内容检查后 FAIL：
  `an unreadable MQTT CA is reported as a configuration error`）、
  `tls/mqtt_ca_file_without_pem_is_rejected`。
- **通道并发上限要跟着热更新换闸门。** `ChannelGate` 底层是 `SemaphoreSlim`，容量建好就改不了，
  而 `ApplyAsync` 用的是 `_gates.GetOrAdd(channel.Id, ...)`：闸门已存在就原样返回，
  新上限被静默丢弃。于是 `maxConcurrency` 从 4 改到 1 只在重启进程后才生效——配置说一套、
  跑的是另一套，两边都不报错，现场只能靠抓包或偶发错乱去猜。现在上限变了就换一个新闸门，
  旧的在设备重建完成后统一释放（当场释放会撞上在飞读里的 `Release`，抛 `ObjectDisposedException`）。
  用例 `lifecycle/channel_concurrency_follows_hot_update`（保留旧的 `GetOrAdd` 后 FAIL：
  `the new concurrency limit takes effect without a process restart: expected <1>, got <2>`），
  前半段先证明「上限 2 时两台设备确实会重叠」，否则后半段的断言测不到东西。
- **历史去重键必须带采集时间，不能只用序号。** 去重的本意是「Outbox 至少一次投递，
  同一行会被重放一次」。而 `seq` 是 `PointCache` 的内存计数器：进程重启、或设备被热更新
  重建（`RemoveDevice` 清掉 `_seq`）之后从 1 重新开始——`cache/remove_device_clears_state`
  正是把「重新加回来时 seq 从 1 开始」当作正确行为钉住的。journal 却跨重启追加。
  两者一叠：只按 `(device, point, seq)` 去重会把重启后「序号恰好相同」的另一条真实样本
  判成重放吞掉，表现为**设备重启一次，之后的历史就再也查不出来**，而且接口 200、
  没有错误、没有计数。重放写出的是字节相同的同一行（连 `collectedAt` 也一样），
  所以键里带上采集时间既能去掉真正的重复，也不会误杀重启后的新数据。
  用例 `history/keeps_samples_after_seq_restarts`（修复前 FAIL：
  `a seq restart must not swallow the newer samples: expected <4>, got <2>`）。
- **「整批成功才推进位点」对逐条发送的 sink 太粗，必须允许它回报部分送达。** Outbox 的
  语义是 at-least-once：位点只在确认后前进，宁可重复不可丢——这条没错。但「批」指的是队列
  切给 sink 的那一段，不是 sink 内部的循环。`MqttSink` 是一条条 `PublishAsync`：发到一半
  broker 断连时，已发到对端的那几条会被整批回退、下一轮从头重发，订阅者收重复、retained
  被覆盖一遍。修复是让 sink 能报出「本批实际送达到最后一条」（正常返回末条 id，中途失败
  抛 `PartialDeliveryException` 携带送达点），队列只确认送达点之前、对未送达部分重试。
  用例 `outbox/a_failed_delivery_does_not_redeliver_earlier_records`：前 3 条成功、第 4 条
  失败，还原成「无视送达点、整批不推进」后 FAIL：
  `already-delivered records are not retried: expected <2>, got <5>`（5 条全部重发，前 3 条被发两遍）。
- **SSE 连接数是 hub 的属性，指标必须在 hub 的所有者那里注册。** `GatewayRuntime.RegisterGauges`
  里写的是 `() => 0`——`UseNorthbound` 内部才创建 `SseHub`，运行时根本拿不到它。
  结果 `/api/v1/metrics` 永远报 `uic_sse_connections 0`：运维看到的「没有订阅者」与
  「已接满 50 条、新连接开始被 429」完全一样，而这恰是判断「该扩容还是该排障」的依据。
  现在指标在 `UseNorthbound` 里随 hub 一起注册；运行时那行删掉并留注释，避免下次
  有人在那边再补一个常量。用例 `contract/metrics_report_live_sse_connections`
  （修复前 FAIL：`the gauge counts the live stream: expected <1>, got <0>`），
  持的是真实 Kestrel 上的真流，不是假 hub。
- **SSE 的订阅分隔符是 `/`，文档曾经写成 `.`。** 设备与点位 id 都允许含点
  （`^[a-z0-9][a-z0-9._-]{0,63}$`），用点分隔时 `meter-1.v2` 这种设备名与
  「设备 + 点位」的边界无法区分，实现只能整串当设备名去比。文档写成
  `meter-1.voltage` 的那个示例照着调会拿到 200、`hello` 照常、但**永远没有样本**——
  静默失败，客户端没有任何线索。产品规格与 UI 用的都是 `/`，实现也一直是 `/`，
  所以这里是文档单方面漂移。用例 `contract/stream_filters_by_point_pattern`
  （把实现改回 `.` 后 FAIL：`the filtered stream delivers at least one sample`），
  同时钉住「匹配不到任何东西的模式仍然建流成功但不出样本」这条容易误判的行为。
- **未知质量码必须在建流之前拒绝，不能建一条永远空的流。** `quality` 拼错（`gud`）
  时旧实现会给 200、发 `hello`、按心跳一直跳，但一帧样本都没有——客户端看到的是
  「设备没数据」，而配置、设备、采集都可能完全正常，排查方向被彻底带偏。
  现在按契约里的九个质量码校验，未知值 400 `request.invalid_parameter` 并列出合法值。
  不匹配的 `points` 模式仍然建流成功（那是合法用法：等一台还没上线的设备），
  但质量码不是「模式」，写错只能是笔误。用例仍是
  `contract/stream_filters_by_point_pattern`（去掉校验后 FAIL：
  `an unknown quality code is rejected instead of silently filtering everything out: expected <BadRequest>, got <OK>`）。
- **列表端点的 `quality` 过滤同样不能「静默过滤到空」。** `/api/v1/points` 与
  `/api/v1/devices/{id}/points` 的 `quality` 写错时旧行为返回 200 + `items: []`，
  与「这台设备没有点位」「点位全被停用」一模一样——调用方只能去翻配置才知道是自己
  拼错了。现在两处共用 `ReadQualityFilter`：未知值 `400 request.invalid_parameter`
  并列出合法值。用例 `contract/devices_and_points_are_listed`（去掉校验后 FAIL：
  `an unknown quality code is rejected on /api/v1/points?quality=gud: expected <BadRequest>, got <OK>`），
  同时保留「合法质量码仍按原样过滤」的正向断言，避免把过滤本身改坏却没人发现。
- **健康接口的「最近一小时丢弃数」必须真的是窗口，不能拿累计值冒充。** 字段名与产品
  规格都写 `droppedLastHour`，实现传的却是 `OutboxStats.DroppedTotal`——进程启动以来的
  累计丢弃。累计值只会变大：一次磁盘打满导致丢包之后，接口会永远报同一个数，
  运维无法回答它唯一想问的问题「现在还在丢吗」，只能靠重启进程把计数器清零来「试探」。
  现在 `Outbox` 按分钟分桶维护一个 60 桶的滑动窗口，桶号从构造时刻起按**单调时钟**算：
  墙钟前跳不会把窗口瞬间清空、后跳也不会让桶号停住导致一小时前的桶永远算在窗口内。
  60 个 `long` 与「逐条记时间戳」相比也避免了持续过载下每小时几十万条的内存增长。
  累计值仍保留在 `OutboxStats.DroppedTotal`（指标口径），健康接口只报窗口。
  为了让这条路径可测，`RuntimeOptions.Clock` 把可拨时钟交给 Outbox——否则在真实
  Kestrel 上根本没法让一小时过去。用例 `outbox/dropped_last_hour_falls_out_of_the_window`
  （把窗口换回累计值后 FAIL：`the window forgets drops older than an hour: expected <0>, got <2>`）
  与 `contract/health_reports_drops_in_the_last_hour`（`BuildHealth` 仍传累计值时 FAIL：
  `drops older than an hour leave the window: expected <0>, got <2>`），后者走真实的
  小上限 Outbox + 真实 HTTP 端点，钉住「装配没接回累计值」这类只在接口层才看得见的漂移。
- **已知边界：位置型游标挡不住「截断到另一个合法位置」。** 游标内部就是偏移量，
  没有签名；`MTAwMA==`（1000）截成 `MTAw`（100）后是另一页的**合法**游标，
  服务端无法区分「客户端截断了」与「客户端本来就在第 100 条」。要根治得给游标
  加 HMAC 或改成键集分页（按 `seq`/时间翻页），那是接口形状变更，超出本轮范围；
  当前先保证「解不开就报错」，不静默重来。
- **已知边界：审计去重窗口仍按墙钟。** `RestApi.Audit` 里认证失败与 403 的 30 秒
  按来源去重（`AuthFailureSeen` / `DenialSeen`）没改成单调时钟。它影响的是「同一来源
  短时间内写几行审计」，校时跳变只会让行数变多或变少，**不会漏记事件、也不会伪造
  事件**，不构成安全边界；而这两处状态需要在审计写入路径上尽量轻，留墙钟是有意的取舍。

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

1. 在 `Uic.Northbound`（或新项目）实现 `IOutboxSink`：整批一起送达的实现 `DeliverAsync` 抛异常即可；
   若是逐条发送（一条失败不该连累整批），实现 `DeliverPartialAsync` 返回送达点、或在中途失败时
   抛 `PartialDeliveryException` 携带已送达的最后一条 id，让队列只对未送达部分重试；
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
