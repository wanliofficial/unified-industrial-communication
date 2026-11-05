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

- **南向**：Modbus TCP / RTU / ASCII 主站；Modbus TCP 从站（仅用于仿真与测试）；
  虚拟设备（正弦/斜坡/随机/常量 + 五类故障注入）。
- **北向**：REST（设备/点位/写入/配置/命令/诊断）、SSE、MQTT 发布侧、OPC UA 服务端。
- **治理**：内容寻址配置 + diff + 回滚 + 热更新；持久化上报队列（分区分位点、三重上限）；
  设备状态机 + 熔断 + 探活恢复；指标 + 环形日志 + 诊断包。
- **控制台**：Uno WASM 五个页面（设备 / 点位 / 监控 / 日志 / 配置）。
- **验收**：黄金帧 16 份；T1–T18 全部有对应用例，其中 T15–T18 跑在 libc 开出来的
  真实伪终端上（见 [testing.md](testing.md)）。

### 刻意没做

| 没做 | 原因 | 怎么补 |
|---|---|---|
| Modbus 从站的 RTU/ASCII 封装 | 仿真只需要一条 TCP 链路就能覆盖主站的全部路径 | `ModbusSlaveHandler` 与封装无关，加一层 framing 即可 |
| OPC UA 签名/加密 | 证书生命周期（签发、轮换、吊销、信任列表）是独立的运维负担 | 配置里已显式拒绝非 `None`，加实现时同步放开校验 |
| OPC UA 用户认证 | 没有用户后端，匿名读 + 可选匿名写足够覆盖调试场景 | 接 `IUserIdentityValidator` |
| MQTT QoS2 | 现场 QoS1 + 持久化队列已经够用，QoS2 的握手复杂度不划算 | 在 `MqttClient` 里加 PUBREL 状态机 |
| MQTT 订阅（下行命令） | 下行命令走 REST 更可控（有幂等键与审计） | 新增一个 `MqttCommandSink`，复用 `IGatewayControl` |
| 多协议（S7 / OPC DA / IEC 104 …） | 首版把一条链路的每一层都做透，比铺开十个半成品有用 | 见下「扩展点」 |

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
4. 加一份黄金帧（`testdata/golden/<proto>_*.json`）与一条回环用例；
5. 配置里 `channel.protocol` 写新驱动的 `Meta.Id`。

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
