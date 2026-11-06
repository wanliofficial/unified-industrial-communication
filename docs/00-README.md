# 统一工业通信网关（UIC）｜文档与代码总索引

> **一句话定位**：一个用 Go 写的、可治理的工业数据采集底座——南向接协议，中间做建模与调度，北向出标准接口；重点不在「能连多少种设备」，而在「跑了三年、改过五十次之后，还能知道哪个点坏了、为什么坏、改坏了怎么退回去」。

| 项 | 数字 |
|---|---|
| 分析的参考方案 | 38 份（PA-01 … PA-38，其中 PA-06 合并了两个完全相同的仓库） |
| 文档总量 | 55 份 Markdown / 18,124 行（含 3 份 ADR） |
| 代码总量 | 141 个 Go 文件 / 57,907 行（其中测试 25,470 行） |
| Go 包 | 37 个（1 个入口 + 36 个内部包；35 个有测试） |
| 第三方依赖 | 0（仅标准库，零 CGO） |

---

## 1. 这个仓库是什么

三层交付物，自下而上：

```
docs/01-project-analysis/   38 份逐方案解剖（它们做对了什么、错在哪）
        ↓
docs/02-synthesis/          横向归纳（能力矩阵 / 架构原型 / 缺口 / 协议目录）
        ↓
docs/03-product/            产品定义（PRD / 架构 / 领域模型 / 驱动 SDK / 北向 API / NFR / 路线图）
        ↓
cmd/ + internal/            可运行的最小骨架（不是伪代码，能采数、能写入、能回滚、能补发）
```

### 1.1 它是什么

- 一份**证据驱动**的产品定义：每一条「必须有」的能力，都能在 38 份分析里找到「谁做到了、谁没做到、代价是什么」。
- 一个**能跑通**的骨架：`go build ./...` 之后可以用内置虚拟设备起服务，也可以用内置 Modbus 从站走真实编解码链路做回环验证。
- 一套**可验收的契约**：驱动接口有版本号，错误有分类，配置有内容哈希版本，北向有错误码表。

### 1.2 它不是什么

- 不是协议库大杂烩。当前实现了 8 个驱动（Modbus TCP / RTU / ASCII 主站、S7 主站、OPC UA 主站、
  MQTT 南向订阅、计算点、virtual），外加 Modbus TCP 从站、四个内置模拟从站/Broker、一个零依赖报警引擎与一个有界历史采样日志；其余协议在
  `04-protocol-catalog.md` 里按工作量排了三波，没有假装已经支持。
  注意 MQTT 出现了两次且**互不相干**：北向是「网关把数据发出去」（sink），南向是「网关订阅别人发来的数据」（driver）。
- 不是组态软件。不做画面、不做报表、不做自研时序库——理由见 `02-synthesis/01-capability-matrix.md` §6.4。
- 不是「参考项目的改写版」。全部产物经过脱敏，只以 PA 编号 + 技术画像指代，不含任何被分析对象的名称、地址、作者、商标或包名前缀。

---

## 2. 三分钟跑起来

```bash
# 1. 编译（无第三方依赖，无需联网）
go build -o uic ./cmd/uic

# 2. 只校验配置，不启动（CI 里可当 lint 用）
./uic -check -config examples/demo.json

# 3. 跑虚拟设备演示（无需任何硬件、无需网络）
./uic -config examples/demo.json
#    另开一个终端：
curl -s localhost:8080/api/v1/health | jq
curl -s localhost:8080/api/v1/devices | jq '.items[0]'
curl -sN localhost:8080/api/v1/stream | head -20        # SSE 实时流
```

真实协议链路回环（一个进程里同时起主站与从站）：

```bash
./uic sim -listen 127.0.0.1:15020 &      # 内置 Modbus TCP 从站
./uic -config examples/modbus-loopback.json
curl -s localhost:8081/api/v1/points | jq '.items[0]'
```

串口（Modbus RTU）链路：

```bash
# 有真实硬件时直接改 examples/modbus-rtu.json 里的 port / baud / 从站地址
./uic -check -config examples/modbus-rtu.json
./uic -config examples/modbus-rtu.json

# ASCII 帧（':' + 十六进制 + LRC + CRLF），老式仪表常见
./uic -check -config examples/modbus-ascii.json

# 没有硬件时可用伪终端模拟一条总线（Linux）
socat -d -d pty,raw,echo=0,link=/tmp/ttyV0 pty,raw,echo=0
#   然后在 /tmp/ttyV0 上挂从站模拟器，或把配置里的 port 指向它
```

S7（S7comm / ISO-on-TCP）链路：

```bash
./uic sim -protocol s7 -listen 127.0.0.1:15102 &   # 内置 S7 从站
./uic -config examples/s7.json
curl -s localhost:8085/api/v1/points | jq '.items[] | {point, value, quality}'
```

示例配置覆盖 7 个点位：DB1 的 REAL/INT16/DINT32/STRING、DB2 的 WORD、
输入区 IW0、位地址 M0.3。模拟从站每秒改 DB1.DBD0，可用来观察 SSE。
`-pdu 240|480|960` 可改协商值，用来验证切块（240 时 40 个 float 会被拆成多帧）。

OPC UA（二进制，SecurityPolicy None / 匿名）：

```bash
./uic sim -protocol opcua -listen 127.0.0.1:14840 &   # 内置 OPC UA 服务端
./uic -config examples/opcua.json
curl -s localhost:8086/api/v1/points | jq '.items[] | {point, value, quality}'
# 写穿透（服务端回读确认）：
curl -s -X POST -H 'Authorization: Bearer dev-token-0123456789' -H 'Content-Type: application/json' \
     -d '{"value":{"type":"float64","num":42.5},"confirm":true}' \
     localhost:8086/api/v1/points/ua1/setpoint/write
```

示例配置覆盖 6 个点位：`ns=2;s=Plant.Temperature`（double）、`Counter`（int32）、
`Running`（bool）、`Label`（string）、`Setpoint`（double，可写）、
以及标准节点 `ns=0;i=2258`（ServerStatus.CurrentTime）。

OPC UA 订阅（服务端推送；`Read` 不再逐点轮询）：

```bash
./uic sim -protocol opcua -listen 127.0.0.1:14840 &
./uic -config examples/opcua-subscribe.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8093/api/v1/points/ua1/counter
# channels[].subscribe / publishingInterval / samplingInterval
```
服务端每秒改 Counter 与 CurrentTime，可用来观察 SSE。

从站（把采集到的点位反向暴露给 SCADA / 第三方主站）：

```bash
./uic sim -listen 127.0.0.1:15020 &           # 上游设备
./uic -config examples/modbus-slave.json &    # REST :8086，Modbus 从站 :1502

./uic mbread -addr 127.0.0.1:1502 -unit 1 -area holding -offset 0 -qty 4
./uic mbread -addr 127.0.0.1:1502 -area holding -offset 3 -write 777
./uic mbread -addr 127.0.0.1:1502 -area coil -offset 0 -qty 1
```

`uic mbread` 是内置的极简主站，用来在不装任何第三方工具的情况下验证从站：
寄存器区打印 uint16 / int16 / 十六进制并按 ABCD 拼 float32，位区打印逐位状态。
读一个跨越未映射地址的区间会返回异常码 `0x02`——这是有意的，补零会让主站
把「没配映射」当成真实值 0。

从站映射的宽度来自点位类型（`Point.RegisterWidth()`），可写性必须同时满足
「区可写（holding/coil）」与「点位 `writable: true`」。写入会穿透到
`Runtime.Write`，与 REST 写入共用设备 actor 与校验，不存在第二条绕过治理的写路径。

从站同时实现诊断与设备识别（FC8 回显/清计数/六类计数、FC11 事件计数器、FC17
服务器 ID、FC43/MEI 0x0E 读设备标识并支持分页）。`slaves[].identity` 可配置
厂商/产品/型号/版本；版本缺省取运行进程的构建版本，上位机读到的版本与二进制一致。

MQTT 北向（需要一个 Broker，任意 MQTT 3.1.1 服务端均可）：

```bash
mosquitto -p 1883 &                          # 或用现场已有的 Broker
./uic -config examples/mqtt.json
mosquitto_sub -h 127.0.0.1 -t 'uic/mqtt-lab/#' -v
```

观察三个主题族：`.../{device}/{point}/value`（变化上报，QoS1）、
`.../{device}/$state`（设备状态，retained）、`.../$birth`（网关上线，retained）。
把 Broker 停掉再起来，可以看到数据先进 Outbox、恢复后按序补投，以及
`$birth`/`$state` 被重新发布。

MQTT 南向（订阅别人的数据，方向与上一节相反）：这是全项目**唯一一个推送型**
协议，驱动必须把「设备主动发」适配成「采集器来拉」。内置 Broker 让这条链路
同样零外部依赖：

```bash
./uic sim -protocol mqtt -listen 127.0.0.1:18830 &   # 内置 Broker，每秒发布演示点位
./uic -config examples/mqtt-southbound.json          # REST :8087
curl -s -H 'Authorization: Bearer dev-token-0123456789' \
     localhost:8087/api/v1/points | jq '.items[] | {point, value, quality}'
```

示例配置覆盖 6 个点位、两种载荷形态：纯文本标量（`plant/line1/temp` → `21.3`）
与 JSON 对象（`plant/line1/state` → 用 `address.selector` 取出 `running` / `mode` /
`cycle` 三个字段，且三者只订阅**一次**）。`setpoint` 可写，写入就是向同一主题
发一条 PUBLISH，Broker 采纳后立刻回显，于是回读确认能闭环。

两条语义必须记住：**质量码只反映新鲜度**（收到过且在 `staleAfter` 内 → good；
超时 → stale 但保留最后值；从未收到 → bad），以及 **一条消息都没来不等于断链**
（不会因此重连；只有「安静超过一个新鲜度窗口且 PING 往返失败」才判断链——
拔网线、对端断电这类故障 TCP 自己不会报错）。

OPC UA 北向（把点位表暴露成服务端地址空间）：

```bash
./uic -config examples/opcua-north.json        # REST :8094，OPC UA 服务端 :14841
./uic uaread -addr 127.0.0.1:14841 \
  -nodes 'ns=2;s=north-lab/line-a/temperature,ns=2;s=north-lab/line-a/setpoint' -type float32
./uic uaread -addr 127.0.0.1:14841 -nodes 'ns=2;s=north-lab/line-a/setpoint' \
  -type float32 -write 42.5
```

地址空间固定为 `Root/Objects/{site}/{device}/{point}`，变量 NodeId 是
`ns=2;s={site}/{device}/{point}`，质量码走 `DataValue.Status`。写入真的走南向
设备：只读点位回 `BadNotWritable`，未知点位回 `BadNodeIdUnknown`，设备没起来回
`BadNoCommunication`——不会出现「网关内存改了数、设备没收到」却报成功的情况。
`uic uaread` 与 `uic mbread` 一样，是不装第三方客户端即可自证链路的工具。

计算点（纯表达式，没有任何物理链路）：

```bash
./uic -config examples/computed.json          # REST :8089
curl -s localhost:8089/api/v1/points | jq '.items[] | {point, value, quality}'
curl -s -X POST localhost:8089/api/v1/points/line-a-kpi/write \
  -H 'Content-Type: application/json' \
  -d '{"writes":[{"point":"cost","value":{"type":"float64","num":1}}]}' | jq
# → {"ok":false,"code":"point.not_writable", ...}
```

示例里 `line-a` 是虚拟源（斜坡波形，3 个点位），`line-a-kpi` 是 6 个计算点：
`cost = power × rate`、`cost_with_tax = cost × 1.13`（**同设备依赖**，同一次 Read
内按拓扑序求值，因此 `cost_with_tax / cost` 恒为 1.13）、`avg` / `round` / `if` 的
用法、`high_load = power > 80`（布尔点位必须接布尔表达式）。写入被拒是设计：
计算点没有可写回的地址。

表达式直接复用驱动契约（`planner` 把计算点合成一个 `expr` span，驱动按编译期
依赖图求值），不引入第二套采集路径。引用写作 `device:point`；因为 ID 允许 `-`，
**减号两边要留空格**（`line-a:power-1` 是引用，`line-a:power - 1` 才是减法）。
质量沿依赖传播：任一输入不可用则不计算，输出取最差质量并指明是哪个输入。

报警（阈值 / 滞回 / 延时 / 确认）：

```bash
./uic -config examples/alarms.json            # REST :8091
curl -s localhost:8091/api/v1/alarms | jq '.counts'
curl -s 'localhost:8091/api/v1/alarms?state=active' | jq '.items[] | {id,state,value}'
curl -s -X POST localhost:8091/api/v1/alarms/high-power/ack -d '{"by":"operator-1"}' | jq
curl -sN 'localhost:8091/api/v1/alarms/stream?heartbeat=5s'    # 事件流
```

示例里压机功率是一条 10~90kW 的正弦波，5 条规则演示了：阈值 + 滞回 + `onDelay`
（功率过高）、低阈值（空转）、布尔表达式（功率与速率不匹配）、`autoAck`（尖峰）、
以及 `disabled`（维护窗口）。确认语义刻意区分「正在报警」和「已恢复但没看过」：
两者都需要人处理，确认后分别是 `acked` 与 `normal`。

历史（有界采样日志，不引入数据库与第三方依赖）：

```bash
./uic -config examples/history.json           # REST :8092
curl -s 'localhost:8092/api/v1/history?device=press&point=power&from=-5m&limit=5' | jq
curl -s 'localhost:8092/api/v1/history?device=press&point=power&from=-1h&step=1m&agg=avg' | jq '.buckets[0]'
curl -s localhost:8092/api/v1/health | jq '.history'
```

采样是「事件驱动 + 心跳补点」：值变化超过死区就记，没变化也至少每 `heartbeat`
记一条；被 `minInterval` 挡下的变化进 pending，到期补写成**最终值**（否则「快速
变化后立刻稳定」的末值会永久丢失）。三条边界必须记住：**它是旁路**（写失败只
计数，不反压采集）；**整段淘汰**（按字节上限与保留期淘汰最旧的段，永远保留一
段）；**心跳不撒谎**（只有样本本身仍新鲜才补点，设备停了就不再写「此刻仍然是
42」）。查询左闭右闭、结果按时间升序、桶质量取桶内最差，非数值点位的
`min/max/avg` 是 `null` 而不是 0。

写入与回滚（这是最能体现「可治理」的两条命令）：

```bash
curl -s -X POST localhost:8080/api/v1/points/line-a/write \
  -H 'Content-Type: application/json' \
  -d '{"writes":[{"point":"setpoint","value":{"type":"float32","num":42.5},"confirm":true}]}' | jq

curl -s localhost:8080/api/v1/config/versions | jq
curl -s -X POST localhost:8080/api/v1/commands \
  -d '{"action":"config.rollback","params":{"version":"sha256:...","confirm":true}}' | jq
```

> 命令一览：`uic [run] -config <file> [-listen] [-token] [-log-level] [-data]`、`uic -check -config <file>`、`uic sim [-listen]`、`uic version`。

---

## 3. 文档地图

```
docs/
├── 00-README.md                    ← 你在这里（索引 + 结论速查）
├── 01-project-analysis/            38 份逐方案解剖
│   ├── _CONVENTIONS.md             统一写作口径（术语、评分标准、脱敏规则）
│   ├── _TEMPLATE.md                六章模板
│   └── PA-01 … PA-38.md            每份：定位 / 功能 / 架构 / 需求 / 可借鉴与应规避 / 结论
├── 02-synthesis/                   四份横切分析
│   ├── 01-capability-matrix.md     38 × 12 能力域矩阵 + 六维评分 + P0/P1/P2/X 分级
│   ├── 02-architecture-patterns.md 三种架构原型 + 故障恢复/配置生命周期演化
│   ├── 03-gap-analysis.md          十二个行业缺口 + 优先级 + 反风险
│   └── 04-protocol-catalog.md      四层协议模型 + 四种寻址 + 难度分级 + 三波路线
├── 03-product/                     七份产品定义
│   ├── 01-prd.md                   定位、原则、功能需求、用户流程、版本范围
│   ├── 02-architecture.md          部署视图、模块依赖、数据流、并发、状态机、存储、接口
│   ├── 03-domain-model.md          地址/类型/点位/设备/通道/采样/批读/管线/配置/模板
│   ├── 04-driver-sdk-spec.md       三层契约、错误模型、能力四态、版本兼容
│   ├── 05-northbound-api-spec.md   REST + SSE + MQTT + OPC UA 映射、错误码、幂等
│   ├── 06-nfr.md                   性能、可用性、安全、可移植性指标
│   └── 07-roadmap.md               三波交付计划与验收门槛
└── 04-decisions/                   ADR
    └── 0001-language-selection.md  Go 主运行时 + C# 辅助进程（含加权评分与退出条件）
```

### 3.1 按角色选阅读路径

| 你是 | 建议顺序 | 大约耗时 |
|---|---|---|
| **只想判断这东西值不值得用** | 本文件 §5–§8 → `03-product/01-prd.md` → `03-product/07-roadmap.md` | 40 分钟 |
| **要接手写代码** | `03-product/02-architecture.md` → `03-product/03-domain-model.md` → 本文件 §9 代码地图 → `internal/driver/driver.go` | 3 小时 |
| **要写一个新驱动** | `03-product/04-driver-sdk-spec.md` → `internal/driver/driver.go` → `internal/driver/virtual/`（故障注入范例）→ `codec/modbus` 的纯函数分层 | 半天 |
| **要做技术选型评审** | `04-decisions/0001-language-selection.md` → `02-synthesis/02-architecture-patterns.md` → `03-product/06-nfr.md` | 2 小时 |
| **要理解「为什么不做 X」** | `02-synthesis/01-capability-matrix.md` §6.4 → `02-synthesis/03-gap-analysis.md` §6 | 1 小时 |
| **要做竞品/资料研究** | `02-synthesis/01-capability-matrix.md` §2–§4 → 按需翻对应的 `PA-xx.md` | 按需 |

---

## 4. 三十秒结论（如果只读一段）

1. **协议广度是入场券，不是护城河。** 38 个方案里协议最多的那个（50 个协议家族）六维总分排第 15；排名第一的是协议中等但工程完整的方案（25 种协议 / 28 分）。因为协议广度靠人力堆，架构撑不过第二个五年。
2. **可运维性是全行业最低分项**（均值 2.3 / 5，能到 4 分的只有 5 个）。这解释了现场工程师为什么默认「能跑就行，坏了重启」——因为过去十年没人把运维面当产品做。
3. **四个空白格决定产品形态**：持久化 Outbox 补发（完整实现 1 个）、配置版本化回滚（3 个）、协议一致性测试套件（2 个）、默认安全基线（0 个）。前两个决定「敢不敢远程运维」，后两个决定「敢不敢规模化交付」。
4. **产品的最小可信形态 = 12 个协议 + 4 个北向通道 + 4 个治理域**（调度 / 边缘自治 / 配置 / 可观测）。任何试图第一版覆盖 30 个协议的计划，都会重演「清单漂亮、一半是桩」的结局。
5. **本仓库选择 Go 作为主运行时**：单二进制交付、交叉编译到 ARM、goroutine 天然匹配「每设备一个 actor」。OPC DA 与厂商 SDK 这类必须 Windows COM 的能力，用 C# 辅助进程桥接——理由是「不为 2% 的能力把 98% 的部署绑死在 Windows 上」。完整论证与退出条件见 `04-decisions/0001-language-selection.md`。

---

## 5. 38 个参考方案总览

编号即索引，点开 `01-project-analysis/PA-xx-*.md` 看全文。**协议数按可核实条目计**，不采信自述；「成熟度」是工程完成度判断，不是商业成功度。

| 编号 | 形态 | 语言/运行时 | 可核实协议数 | 规模画像 | 成熟度 | 一句话价值 |
|---|---|---|---|---|---|---|
| PA-01 | 协议内核类库（含从站模拟） | C# 多目标 | 50 家族 / 120+ 端点 | 211 文件 / 63k 行 | 商业化 | 协议语义的百科全书写在代码里 |
| PA-02 | 桌面调试工作台 | C# WinForms | 218 个协议页 | 751 文件 / 205k 行 | 商业化 | 把「联调」做成了产品 |
| PA-03 | JVM 移植 + 演示 | Java 8 | 约 .NET 的 1/3 | 119 文件 / 21k 行 | 生产可用（滞后） | 跨平台移植的真实成本样本 |
| PA-04 | HMI 控件库 | C# WinForms/WPF | — | 150 公开类型 | 商业化 | 图元与属性的工业化沉淀 |
| PA-05 | 开源分支 | C# 多目标 | 约 29 设备类 | 211 文件 | 社区开源 | 同源代码的许可边界实验 |
| PA-06 | Java 多模块网关 | Java 17 / Spring Boot | 34 个协议代码 | 1087 文件 / 192k 行 | 生产可用 | **可治理采集底座的标杆** |
| PA-07 | Node 全栈采集器 | TypeScript | 19 可采集 / 12 栈 | 46 文件 / 8.3k 行 | 内部可用 | 小团队交付速度的上限 |
| PA-08 | WoT→OPC UA 网桥 | C# .NET 10 | 18 驱动 | 400 文件 / 79k 行 | 生产可用 | 用标准信息模型统一异构设备 |
| PA-09 | 协议仿真平台 | Python / FastAPI | 31 协议 id | 191 文件 / 67k 行 | 生产可用 | 把「测试设备」做成了平台 |
| PA-10 | Avalonia 边缘网关 | C# .NET 8 | 9 实现 | 133 文件 / 19k 行 | 原型可用 | 通道与代次模型很干净 |
| PA-11 | 电力协议工作站 | Python + Vue + Tauri | 8 类 | 398 文件 / 89k 行 | 内部可用偏生产 | 电力规约纵深最深 |
| PA-12 | WinForms 采集工作站 | C# .NET Framework | 40 类型 / 约 7 真实 | 91 文件 | 内部可用 | 功能密度的极限与代价 |
| PA-13 | 插件化 DAQ 框架 | C# .NET 10 | 由插件决定 | 106 文件 | 内部可用偏生产 | 插件隔离做得最彻底 |
| PA-14 | 语义化工作站 | C# .NET Framework | 39 驱动 / 约 25 真实 | 141 文件 / 58k 行 | 内部可用偏生产 | 语义/模板/计算编排 |
| PA-15 | Java 协议类库 | Java 8 | 5 协议族 | 474 文件 / 62k 行 | 生产可用 | S7 深度与批量算法 |
| PA-16 | Go IoT 中台 | Go | 3 描述符 | 196 文件 / 17.7k 行 | 原型 | 单二进制 + 嵌入式前端的交付形态 |
| PA-17 | Go DDD 平台 | Go | 十余种（多为桩） | 173 文件 / 49.5k 行 | 原型 | 分层与接口下沉最彻底 |
| PA-18 | .NET PLC 驱动包 | C# netstandard | 2 厂商 / 4 寻址 | 74 文件 / 7.9k 行 | 生产可用 | 小而真的驱动包工程 |
| PA-19 | 网关生成器 | Python / PyQt6 | 13 协议族 | 32 文件 / 4.7k 行 | 原型 | 「生成器」思路的失败与启示 |
| PA-20 | Go 教学监控 | Go / Gin | 0 | 22 文件 / 1.8k 行 | 教学演示 | 只有管理面，没有采集面 |
| PA-21 | Java 轻量网关 | Java 8 / Spring Boot | 2 可用 | 37 文件 / 1.8k 行 | 试运行 | 最小闭环（采集→队列→上报） |
| PA-22 | Go 编解码集合 | Go | 20+ 包 | 92 文件 / 10k 行 | 实验田 | 协议广度的廉价实现 |
| PA-23 | .NET 调试工具集 | C# .NET 8 | 4 类 | 331 文件 | 原型 | 新老两代工具并存的重构现场 |
| PA-24 | .NET 客户端库 | C# netstandard | 6 协议族 + 7 从站 | 76 文件 / 19k 行 | 生产可用 | 客户端与模拟器同源的范式 |
| PA-25 | Java/Netty 协议集合 | Java 8 / Netty | 2 | 88 文件 / 7.1k 行 | 原型（已停更） | 技术选型失败的一手记录 |
| PA-26 | 上位链路仿真器 | C# WinForms | 1 | 8 文件 / 1.6k 行 | 一次性原型 | 单协议仿真器的下限 |
| PA-27 | Go 协议封装 | Go | 4 可用 | 68 文件 / 7.7k 行 | 学习型原型 | Go 生态里罕见的完整 BACnet 栈 |
| PA-28 | Go 微型协议 | Go | 1（RTU 帧） | 12 文件 / 1.4k 行 | 早期原型 | 「能跑通」到「能上生产」的距离 |
| PA-29 | C# 插件平台 | C# .NET Framework | 0 | 258 文件 / 51k 行 | 骨架 + 停更 | 平台抽象过早的教科书案例 |
| PA-30 | C++ FINS 实现 | C++ MFC | 1 | 17 源文件 / 1.4k 行 | 一次性演示 | 协议语义知识大于代码价值 |
| PA-31 | WPF 多协议平台 | C# .NET 6 | 9 类 | 175 文件 / 21k 行 | 原型后期 | 桌面 + 服务端双形态 |
| PA-32 | 跨平台 C# 库 | C# netstandard | 10 PLC + 8 驱动 | 92 文件 / 17k 行 | 可用库 | 字节视图抽象的生产力 |
| PA-33 | 上位机控件框架 | C# .NET Framework | 8 类 PLC | 490 文件 / 192k 行 | 单机交付 | 「控件即绑定」的组态思想 |
| PA-34 | Web 边缘网关 | C# .NET 10 | 25 | 548 文件 / 113k 行 | 准生产 | **运维面与产品化程度的标杆** |
| PA-35 | WinForms 边缘网关 | C# .NET Framework | 12 内置 + 1 插件 | 228 文件 / 45k 行 | 内部可用 | **边缘三件套（Outbox/版本/远程）** |
| PA-36 | Avalonia 跨平台 | C# .NET 8 | 9 | 133 文件 / 19k 行 | 内部可用 | headless 优先的跨平台路径 |
| PA-37 | HTTP/JSON 中间件 | JS 前端 + 闭源内核 | 15 驱动 | 334 个 .js（多为第三方） | 产品化 | 「Web API 即工业接口」的产品学 |
| PA-38 | PHP 管理后台 | PHP / Workerman | 8 类接入 | 24 张表 | 学习演示 | 管理面与采集面必须分离 |

### 5.1 六维评分 Top 10

评分是对「作为参照物的价值」的评分，不是代码质量评分。1–5 分，满分 30。

| 排名 | 编号 | 协议广度 | 可演进性 | 跨平台 | 可运维 | 工程质量 | 许可安全 | 总分 |
|---|---|---|---|---|---|---|---|---|
| 1 | PA-34 | 5 | 4 | 5 | 5 | 4 | 5 | **28** |
| 2 | PA-06 | 5 | 4 | 4 | 5 | 4 | 5 | **27** |
| 2 | PA-08 | 5 | 4 | 4 | 4 | 5 | 5 | **27** |
| 2 | PA-09 | 5 | 4 | 5 | 4 | 4 | 5 | **27** |
| 5 | PA-13 | 3 | 5 | 3 | 4 | 4 | 5 | **24** |
| 6 | PA-15 | 4 | 3 | 5 | 2 | 4 | 5 | **23** |
| 6 | PA-32 | 4 | 4 | 5 | 2 | 3 | 5 | **23** |
| 8 | PA-17 | 3 | 4.5 | 4 | 3.5 | 2 | 4.5 | **21.5** |
| 9 | PA-24 | 4 | 3 | 4 | 2 | 3 | 5 | **21** |
| 10 | PA-11 | 5 | 3 | 4 | 3 | 3 | 2 | **20** |

**两处值得注意的错位**：

- PA-01（50 个协议家族、63k 行）总分 17，排第 15。协议广度 5 分，可演进性 2 分、许可安全 2 分——**广度和可持续性是负相关的**，因为广度靠堆人力，而人力堆出来的代码很难重构。
- PA-35（12 个协议、45k 行、仅 Windows）总分 18。它是唯一把「Outbox + 配置版本 + 远程命令」三件套都做出来的方案，但跨平台只有 1 分——**这正是本仓库要解决的问题**：把这套边缘自治能力搬到跨平台运行时上。

### 5.2 六维分布：最低的那一栏就是机会

| 维度 | 均值 | 分布特征 |
|---|---|---|
| 协议广度 | 3.0 | 两极分化：5 分 9 个，1–2 分 14 个 |
| 可演进性 | 2.9 | 4 分以上 9 个，2 分及以下 16 个 |
| 跨平台 | 2.9 | 5 分 8 个、1 分 11 个 |
| **可运维性** | **2.3** | **全卷最低；4 分以上仅 5 个** |
| 工程质量 | 2.8 | 有测试且能跑自动化的不到三分之一 |
| 许可安全 | 3.1 | 1 分 8 个（无 LICENSE / AGPL / GPL / 闭源） |

### 5.3 协议出现频次（38 份文档硬计数）

| 协议 | 出现份数 | | 协议 | 出现份数 |
|---|---|---|---|---|
| Modbus（TCP/RTU） | 33 | | BACnet | 11 |
| MQTT | 27 | | EtherNet/IP + CIP | 9 |
| S7（S7comm） | 24 | | OPC DA | 8 |
| OPC UA | 24 | | ADS（倍福） | 7 |
| 欧姆龙 FINS | 19 | | IEC 61850 | 6 |
| 三菱 MC（1E/3E/4E） | 17 | | DNP3 | 6 |
| | | | DL/T 645 | 6 |
| | | | IEC 60870-5-104 | 5 |

> 频次高 ≠ 质量高。Modbus 出现 33 次，但能正确处理异常码、批量切分、位访问、字节序四件事的不到一半。详见 `02-synthesis/04-protocol-catalog.md`。

---

## 6. 十二个能力域怎么读

`02-synthesis/01-capability-matrix.md` 把 38 个方案摊成 12 张能力表，每张给「代表实现 / 覆盖度 / 目标优先级」。这里只列各域的**关键判断**：

| 域 | 关键判断 |
|---|---|
| D1 南向接入 | Modbus TCP + S7 + OPC UA 是 P0 三件套；Modbus 从站被严重低估（只有 5 份做了），但它是自测与替代真机的唯一低成本手段 |
| D2 数据建模 | 四种寻址（寄存器 / 符号 / 对象 / 文件）必须统一到一个 `Address` 里，否则每加一个协议就要改一次上层 |
| D3 采集调度 | 分水岭不是「能不能并发」，而是「代次隔离」——设备重启后旧 goroutine 的写入必须被丢弃 |
| D4 数据加工 | 缩放与死区是现场刚需，表达式是伪刚需（38 份里做了表达式的都在维护地狱里） |
| D5 北向接口 | REST 用于配置与查询，SSE 用于实时观察，MQTT 用于上报，OPC UA 用于被 SCADA 集成——四个通道各有明确职责，不能合并 |
| D6 持久化 | 配置 / Outbox / 历史三个库必须分离，写频率与失败影响完全不同，混在一起必然互相拖累 |
| D7 边缘自治 | Outbox + 看门狗 + 熔断探活，三件事缺一不可；只有 1 份做全了 Outbox，只有 1 份做了看门狗 |
| D8 配置治理 | 内容哈希版本 + 快照 + 一键回滚；只有 3 份做了版本化，做到「回滚自动恢复失败配置」的是 0 份 |
| D9 可观测性 | 结构化日志 + 指标 + live/ready 分离；只有 1 份用了 OpenTelemetry |
| D10 安全 | SBOM 为 0，依赖许可清单为 0，凭据加密少见——这是全行业的免费午餐，也是最好建立的差异点 |
| D11 扩展性 | 驱动接口 + 虚拟设备 + 故障注入 + 契约版本；只有 2 份做了故障注入 |
| D12 交付形态 | 单二进制 + 交叉编译是最被低估的能力；它决定了能不能「拷一个文件到 ARM 盒子上就跑」 |

优先级分级见 `01-capability-matrix.md` §6：P0（第一版必须有，缺了现场无法验收）/ P1（第一年）/ P2（第二年）/ X（明确不做）。

---

## 7. 产品定义的四个抓手

来自 `03-product/01-prd.md`，这里只给骨架。

### 7.1 边缘三件套

| 能力 | 解决什么 | 代码落点 |
|---|---|---|
| **Outbox 持久补发** | 断网 24 小时，数据不丢；恢复后按序补投（at-least-once） | `internal/outbox/` + `internal/runtime/outbox.go` |
| **配置版本与回滚** | 任意一次变更可在 30 秒内退回；版本以内容哈希标识，不可篡改 | `internal/config/` |
| **远程运维命令** | 不登机器也能重启驱动、探活、刷队列、调日志级别、拉诊断包 | `internal/runtime/backends.go` + `POST /api/v1/commands` |

三件套的共同点：**都是「出事之后」才用得到的能力**，因此在演示里看不见、在招标里不加分、在现场决定生死。这正是 38 个方案集体欠账的原因。

### 7.2 状态必须分开返回

```json
{ "configuredState": "enabled", "runtimeState": "faulted", "lastError": "..." }
```

「配置里启用了」和「现在真的在跑」是两件事。把它们合成一个 `status` 字段，是现场排障最大的时间黑洞（见 `PA-06` / `PA-34` 的做法对比）。

### 7.3 四通道北向

| 通道 | 用途 | 当前实现 |
|---|---|---|
| REST | 配置、查询、写入、命令 | ✅ 完整 |
| SSE | 实时观察（浏览器/curl 友好） | ✅ 完整 |
| MQTT | 变化上报 + 设备状态 | ✅ 完整（QoS0/1、retained `$state`/`$birth`、LWT `$death`、断线补投；不支持 QoS2/TLS/WebSocket） |
| OPC UA | 被 SCADA 集成 | ✅ 服务端地址空间（`Root/Objects/{site}/{device}/{point}`，`ns=2;s=...`）；质量码走 `DataValue.Status`；写入穿透到南向设备；支持订阅推送；None/None + 匿名，不支持证书/历史/方法/事件 |

> 注意方向的对称性：MQTT 与 OPC UA 都**同时出现在南向和北向**，但这是两条独立的链路。
> 北向 MQTT 是「把采集到的值发出去」，南向 MQTT 是「订阅设备发来的值」；两者共用
> 报文编解码与连接管理（`internal/mqtt`），但驱动契约、配置项、质量语义完全不同——
> 混为一谈会得到「以为在订阅、其实在发布」这种最难查的错配。

### 7.4 明确不做

组态画面 / 通用 BI 报表 / 自研时序数据库 / 云端 SaaS / 桌面 GUI 主形态 / 全协议一次性覆盖。每条都有对应的失败案例，见 `01-capability-matrix.md` §6.4。

---

## 8. 架构一页纸

```
                    ┌──────────────────────────────────────┐
   SCADA / MES ────▶│  northbound: REST · SSE · MQTT · OPC UA │
                    └──────────────┬───────────────────────┘
                                   │ 读快照 / 订阅变化
                    ┌──────────────▼───────────────┐
                    │  cache（当前值的唯一事实源）    │
                    └──────────────▲───────────────┘
                                   │ Sample（值 + 质量 + 时标 + 序号）
                    ┌──────────────┴───────────────┐
                    │  pipeline（缩放/质量/死区）     │
                    └──────────────▲───────────────┘
                                   │ 原始值
   ┌───────────────┐   ┌───────────┴───────────┐   ┌──────────────┐
   │ planner 批读  │──▶│ scheduler（tick + actor）│──▶│ driver 三层  │──▶ PLC
   └───────────────┘   └───────────────────────┘   │ Codec/Transport/Driver │
                                                   └──────────────┘
                    ┌───────────────────────────────┐
                    │ outbox（持久队列 + 分区确认）   │──▶ mqtt / webhook / journal
                    └───────────────────────────────┘
```

依赖方向是单向的，`runtime` 是唯一「知道所有模块」的地方：

```
cmd/uic → runtime → { config, scheduler, cache, pipeline, outbox, northbound, obs }
                  → driver（只通过注册表，不 import 具体驱动）
driver/modbus → codec/modbus → transport
model ← 所有包（不依赖任何内部包）
obs  ← 所有包（不依赖任何内部包）
```

完整版见 `03-product/02-architecture.md` §3。

### 8.1 三个关键的并发设计

| 设计 | 做法 | 为什么 |
|---|---|---|
| **每设备一个 actor** | 50ms tick + 容量 1 的 mailbox，同一设备永不并发采集 | 协议会话有状态（事务号、半双工、PDU 协商），并发采集是最常见的「偶发错乱」来源 |
| **每通道信号量（写优先）** | 通道级并发上限；写请求插队到读之前 | 写入是人的操作，延迟可感知；读是后台任务，晚 100ms 无所谓 |
| **代次隔离** | 每次重启设备递增 generation，旧 lease 的写入一律丢弃 | 解决「设备已删除，旧 goroutine 还在往 cache 写值」这类幽灵数据 |

实现见 `internal/scheduler/`，测试见 `internal/scheduler/*_test.go`（覆盖率 87.7%）。

---

## 9. 代码地图

### 9.1 包一览

| 包 | 职责 | 关键不变量 | 覆盖率 |
|---|---|---|---|
| `internal/model` | 通道/设备/点位/地址/采样/质量码/从站映射与设备标识 | 不依赖任何内部包；不变量集中在 `Validate`；DB 号与 area 联动校验；计算点强制只读、4 类地址各有规范形式；订阅周期只在 `subscribe=true` 时允许出现；从站标识限长并禁控制字符 | 85.6% |
| `internal/driver` | 驱动契约（Codec/Transport/Driver）、错误分类、注册表、只读 `Lookup` 注入 | 能力四态；`Conn` 不保证并发安全，由调用方串行；协议错误可用 `Kinded` 自带分类 | 87.0% |
| `internal/codec/modbus` | Modbus 纯函数编解码（TCP/RTU/ASCII，主站与从站共用） | 无 I/O、无时钟、无可变状态 | 88.1% |
| `internal/codec/value` | 协议无关值编解码（重排、数值/字符串/BCD） | 与协议无关；`Encode` 对 string/bytes 不猜宽度 | 95.0% |
| `internal/codec/s7` | S7 纯函数编解码（TPKT/COTP/S7 PDU、长度单位换算） | 请求按字节、响应按位/字节的差异收敛在一处 | 80.3% |
| `internal/codec/opcua` | OPC UA 纯函数编解码（UACP HEL/ACK、安全报文、NodeId、Variant、读/写/浏览 + 订阅/发布/确认） | 分块响应显式报错；数组长度上界必查；BCD 等无对应类型显式报错；未知通知类型跳过而不是判故障 | 86.1%（含 driver/sim 交叉覆盖） |
| `internal/transport` | 字节流抽象 + TCP + 串口（termios） | 写满、读满或超时、关闭幂等；协议层 deadline 不得被兜底值覆盖 | 80.2% |
| `internal/driver/modbus` | Modbus TCP + RTU + ASCII 主站 | 事务号递增；异常码分类；部分失败不吞好样本；同串口事务串行；分帧类型参与端口冲突判定 | 83.9% |
| `internal/driver/s7` | S7（S7comm）主站 | COTP/Setup 握手不可省；按协商 PDU 与 20 项上限切块；逐项返回码映射质量码 | 79.9% |
| `internal/driver/opcua` | OPC UA 二进制主站（None/None + 匿名）；可选订阅推送 | 三层状态（TCP/通道/会话）分开判断；按 RequestId 过滤迟到响应；节点越界判坏不截断；订阅走独立第二条连接，Read 优先本地表、失效即回退真读 | 76.2% |
| `internal/driver/mqtt` | MQTT 南向（订阅取数）：把推送型协议适配成拉取契约 | `Read` 无网络交互，只读本地最新值表；质量码只反映新鲜度（good/stale/bad 三档）；一条消息都没有不算断链，安静到一个窗口且 PING 失败才判链 | 77.4% |
| `internal/driver/computed` | 计算点：表达式驱动，无物理链路 | 同设备依赖在一次 `Read` 内按拓扑序闭合；输入不可用则不计算、质量取最差并指名输入；整数越界判坏不截断 | 92.4% |
| `internal/driver/virtual` | 虚拟设备（波形 + 故障注入） | 故障按「第 n 次请求」精确注入一次 | 98.6% |
| `internal/sim/modbus` | 内置 Modbus TCP 从站 | 复用同一份 codec，避免主从语义漂移 | 87.2% |
| `internal/sim/s7` | 内置 S7 从站（内存 DB/M/I/Q 区） | 与主站共用 codec/s7；输入区只读；位写保邻位 | 71.9% |
| `internal/opcuaserver` | 可复用的 OPC UA 服务端内核（地址空间、订阅/发布、标准节点） | 与主站共用 codec/opcua；未激活会话拒绝读写；Variant 类型必须与节点 DataType 一致；Publish 在独立 goroutine，响应按 RequestId 乱序返回；`NewSimulator` 提供演示地址空间 | 82.2% |
| `internal/northbound/opcua` | OPC UA 北向：点位表 → 地址空间 | 一点一变量；质量码映射到 `DataValue.Status`；写入过 `Runtime.Write`（设备 actor + 幂等）；配置变更按指纹重建，抢不到端口回退上一份配置 | 82.0% |
| `internal/sim/mqtt` | 内置 MQTT 3.1.1 Broker，兼 `uic sim -protocol mqtt` 运行时 | 全项目唯一一份 Broker 实现；不做 QoS2/持久会话/TLS；QoS2 授予时降级为 1 | 79.3% |
| `internal/slave/modbus` | Modbus TCP 从站（把点位暴露给外部主站） | 读走 cache、写走 Runtime.Write；未映射地址报错不补零；坏质量保留最后值并计数；支持诊断与设备识别（FC8/11/17/43） | 80.0% |
| `internal/alarm` | 报警引擎：五态机、阈值/滞回/延时、表达式规则、确认、订阅 | 输入不可信时绝不改状态机；延时是「连续满足」；`cleared` 与 `active` 都要确认；状态只在内存 | 94.4% |
| `internal/history` | 有界采样日志：NDJSON 分段、整段淘汰、原始查询与定步长聚合 | 一条记录不跨段；永远保留至少一段；末尾半行触发新段；查询升序、桶对齐、桶质量取最差；非数值点位不产出 `min/max/avg` | 83.5% |
| `internal/calc` | 零依赖表达式引擎（编译期类型检查、短路求值） | 无 `eval`；引用是 `device:point`；除零/NaN/Inf/溢出都报错而不是降级 | 91.2% |
| `internal/planner` | 点位 → 批量读请求（ReadSpan） | 协议无关；按连续性/上限合并；按 `area:db` 分组；计算点合成单个 `expr` span（漏组会表现为全坏且无日志） | 92.6% |
| `internal/scheduler` | tick、actor、信号量、退避、熔断、代次 | 同设备不并发；写优先；旧代次丢弃；状态回调不占采集路径 | 87.8% |
| `internal/pipeline` | 缩放、质量判定、死区过滤 | 质量不可用时不参与死区比较 | 92.7% |
| `internal/cache` | 当前值唯一事实源 + 变更订阅 | 读写都要过这里，不允许模块自留副本 | 96.7% |
| `internal/outbox` | 持久队列：分区、WAL、确认位点、三重上限 | at-least-once；整批或前缀成功才推进位点 | 77.4% |
| `internal/config` | 加载、校验、规范化、版本化、回滚 | 幂等 Normalize；原子写；失败自动恢复；北向参数、从站映射、计算点（编译/引用/类型/环）、报警（条件/引用/类型）与历史保留策略静态校验 | 86.8% |
| `internal/obs` | 指标、结构化日志、日志环形缓冲 | 指标标签必须归一化（不能含原始 URL） | 88.2% |
| `internal/rbac` | 角色/权限矩阵（纯函数） | 未知角色/未知权限一律拒绝；`viewer ⊆ operator ⊆ admin`；高危命令按动作映射到 `command.danger` | 100% |
| `internal/auth` | 凭据表：Token → 身份；`Reloadable` 支持热替换 | Token 先 SHA-256 再定长比较；无凭据进入 open 模式而不是造默认口令；拒绝弱口令；枚举永不返回 Token | 96.7% |
| `internal/audit` | 审计存储：NDJSON 分段 + SHA-256 哈希链 + 查询/校验 | 序号连续；字段边界用「长度:内容」编码；整段淘汰后链仍可校验；尾部半行触发新段 | 79.5% |
| `internal/northbound/httpapi` | REST + SSE + 认证/授权 + 错误码 | 错误码稳定；响应不泄露凭据；未知角色全拒；401 与 403 在审计里区分；报警/历史查询的未知参数取值必须报错而不是静默忽略 | 82.8% |
| `internal/mqtt` | MQTT 3.1.1 客户端（北向发布 + 南向订阅共用，纯 Go 零依赖） | 单 goroutine 读 socket；QoS1 必须等到 PUBACK；`Ping` 必须等到 PINGRESP（写成功不算活着）；断线立即失败而非超时 | 70.3% |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 | 不另写 Broker（两份实现必然长歪）；开关一律走 setup 函数而非直接赋字段 | — |
| `internal/runtime` | 唯一编排层 + 后端实现 + 看门狗 + 审计埋点 | 启动/关闭顺序（失败逆序收口）；`configuredState` 与 `runtimeState` 分离；协议/传输匹配在装配期校验；只向驱动与报警引擎注入只读 `cache.Get`；北向 OPC UA 启停与热重建；凭据表热替换后旧用户立即失效 | 80.8% |
| `cmd/uic` | 单入口：run / sim / mbread / uaread / check / version | 关闭预算 10 秒；`uaread` 与 `mbread` 是不装第三方客户端即可自证链路的自检工具 | — |

### 9.2 测试与验证命令

```bash
go build ./...                                  # 编译全部
go vet ./...                                    # 静态检查
gofmt -l internal cmd                           # 格式检查（应无输出）
go test -race -count=1 ./...                    # 竞态 + 全量测试
go test -coverprofile=c.out ./... && go tool cover -func=c.out | tail -1
```

当前状态：`go build` / `go vet` / `gofmt` 全部干净，`go test -race ./...` 35 个测试包全部通过
（另有 `cmd/uic` 与 `internal/mqtt/mqtttest` 两个无测试文件的包）。

### 9.3 源码阅读建议顺序

```
1. internal/model/model.go            领域语言（30 分钟）
2. internal/driver/driver.go          契约与错误分类（40 分钟）
3. internal/codec/modbus/modbus.go    纯函数编解码的写法（40 分钟）
4. internal/scheduler/scheduler.go    actor + 信号量 + 代次（1 小时）
5. internal/runtime/runtime.go        装配与生命周期（40 分钟）
6. internal/outbox/outbox.go          持久队列的分区与确认（40 分钟）
7. internal/runtime/backends.go       命令白名单与配置端点（30 分钟）
8. internal/mqtt/codec.go + client.go 报文字节与 QoS1 确认（30 分钟）
9. internal/driver/s7/driver.go       「一次事务多项」与 PDU 预算（40 分钟）
10. internal/slave/modbus/server.go    从站：映射、跨映射读、写穿透（40 分钟）
11. internal/driver/opcua/driver.go    三层握手与状态机（50 分钟）
12. internal/driver/mqtt/driver.go    推送→拉取适配与新鲜度语义（40 分钟）
13. internal/calc/parse.go            表达式词法/语法：引用与减号的消歧（30 分钟）
14. internal/driver/computed/driver.go 拓扑求值、质量传播、无 IO 的驱动形态（30 分钟）
15. internal/alarm/engine.go           五态机、滞回/延时、质量门控、按依赖索引（50 分钟）
16. internal/history/store.go + query.go 分段追加、整段淘汰、扫描聚合（50 分钟）
17. internal/opcuaserver/server.go     服务端内核：两阶段写、标准节点、订阅分发（50 分钟）
18. internal/northbound/opcua/northbound.go 点位表→地址空间、质量映射、写穿透（40 分钟）
```

---

## 10. 北向接口速查

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/healthz` / `/readyz` | 存活 / 就绪（分离；免认证） |
| GET | `/api/v1/health` | 汇总健康（设备/队列/降级统计） |
| GET | `/api/v1/auth/me` | 我是谁（免认证；返回角色/权限/是否 open 模式） |
| GET | `/api/v1/auth/users` | 账号清单（需 `audit.read`；只返回用户名与角色） |
| GET | `/api/v1/version` | 版本、提交、构建时间、驱动 SDK 版本 |
| GET | `/api/v1/metrics` | Prometheus 文本格式 |
| GET | `/api/v1/devices` | 设备列表（含 `configuredState` / `runtimeState`） |
| GET | `/api/v1/devices/{device}` | 单设备详情 |
| GET | `/api/v1/devices/{device}/points` | 设备下点位 |
| GET | `/api/v1/points` | 点位查询（按设备/质量/仅变化过滤） |
| GET | `/api/v1/points/{device}/{point}` | 单点当前值 |
| POST | `/api/v1/points/{device}/{point}/write` | 写单点 |
| POST | `/api/v1/points/{device}/write` | 批量写（逐项结果） |
| GET | `/api/v1/config` | 当前配置（凭据脱敏） |
| POST | `/api/v1/config/validate` | 只校验不应用 |
| POST | `/api/v1/config/apply` | 应用并热更新 |
| GET | `/api/v1/config/versions` | 版本列表（含 current 标记） |
| GET | `/api/v1/config/versions/{id}` | 指定版本内容 |
| GET | `/api/v1/config/diff?from=&to=` | 版本差异 |
| POST | `/api/v1/config/rollback` | 回滚（需 `confirm`） |
| POST | `/api/v1/commands` | 白名单命令 |
| GET | `/api/v1/logs` | 结构化日志（按设备/级别/条数） |
| GET | `/api/v1/stream` | SSE 实时流 |
| GET | `/api/v1/history?device=&point=&from=&to=&step=&agg=&limit=` | 历史查询（原始点 / 定步长聚合） |
| GET | `/api/v1/diagnostics/bundle` | 诊断 zip（配置脱敏） |
| GET | `/api/v1/audit?user=&role=&action=&result=&q=&from=&to=&limit=&cursor=` | 审计查询（倒序 + 游标） |
| GET | `/api/v1/audit/verify` | 校验哈希链，指出第一条不一致记录 |
| GET | `/api/v1/audit/stats` | 审计水位（条数/段数/字节/最后序号） |

OPC UA 服务端（同一份点位表的另一个出口）：

| 项 | 约定 |
|---|---|
| 端点 | `opc.tcp://{northbound.opcua.listen}`（默认 `:4840`） |
| 地址空间 | `Root/Objects/{site}/{device}` 目录 + `{point}` 变量 |
| NodeId | 变量 `ns=2;s={site}/{device}/{point}`；目录同理少一级 |
| 读写 | Read/Write/Browse/GetEndpoints + 订阅（CreateSubscription / MonitoredItems / Publish） |
| 质量 | `DataValue.Status`：Good / Uncertain / Bad*；Bad 不带值 |
| 写入 | 过 `Runtime.Write`，5 秒超时；只读点位 `BadNotWritable`，类型不符 `BadTypeMismatch` |

**权限**：角色 `viewer` / `operator` / `admin`，权限按动作划分（`read`、`point.write`、`alarm.ack`、`command`、`command.danger`、`config.read`、`config.write`、`audit.read`、`diagnostics`），未知角色/权限一律拒绝。

**命令白名单**：`driver.restart`、`device.selftest`、`outbox.flush`、`diag.bundle`（`command`）；`outbox.drop`（需 confirm）、`config.rollback`（需 confirm）、`log.level`（`command.danger`，仅 admin）。未知动作一律 400，不做猜测。

**错误码**：`request.invalid`、`request.confirm_required`、`request.unknown_action`、`device.not_found`、`point.not_found`、`config.apply_failed`、`config.version_not_found`（404）、`outbox.empty`（409）…完整表见 `05-northbound-api-spec.md` §4.3。

---

## 11. 实现状态与已知边界

**诚实清单**——这些是当前骨架的真实边界，不是 TODO 列表的修辞：

| 项 | 状态 | 说明 |
|---|---|---|
| Modbus TCP 主站 | ✅ | 批量读、单/多寄存器写、线圈写、位访问、异常码分类、字节序（ABCD/CDAB/BADC/DCBA） |
| Modbus 寄存器位写 | ✅ | 布尔点落在保持寄存器且给出 `address.bit`（0..15）时，驱动做「读整字 → 只改目标位 → 写整字」；读失败则不写、缺位号明确拒绝；同设备写入串行（`conn.wmu`），不会覆盖同寄存器的其它位；`coil` 仍走 FC5，`input`/`discrete` 在发帧前拒绝 |
| Modbus RTU | ✅ | 串口 transport（termios raw、非阻塞 + deadline、tcdrain/tcflush）、3.5 字符帧间静默、按请求推导帧长、半包/CRC 错/unit 错/异常帧分类、同串口多从站共享 fd 并串行化事务 |
| S7 主站 | ✅ | TPKT/COTP/S7comm 分层；COTP 目的 TSAP 按 rack/slot 计算；Setup 协商 PDU 后按「20 项 / PDU 预算」切块；DB/I/Q/M/C/T 区读写与位访问；逐项返回码分类（越界/缺 DB→地址错、写输入区→权限错）；部分失败保留好样本 |
| S7 符号表 / 块上传 / 报警 | ❌ | 需要 S7 浏览与报警服务；点位必须手写地址 |
| Modbus TCP 从站 | ✅ | `slaves[]` 存在即启用；四区（holding/input/coil/discrete）映射；跨映射与半值读；写穿透到 `Runtime.Write`；坏质量保留最后值并计入 `uic_slave_degraded_reads_total`；映射变化按指纹重建监听；`uic mbread` 可自检 |
| Modbus RTU/ASCII 从站 | ❌ | 只做 TCP；串口从站需要与主站角色互斥，语义与治理规则都不同 |
| Modbus 诊断与识别（FC8/FC11/FC17/FC43） | ✅ | FC8 子功能 0x0000 回显、0x000A 清计数、0x000B~0x0012 各类计数（TCP 上 CRC/字符层错误恒为 0）；FC11 事件计数器（自身不计数、异常不计数）；FC17 产品代码 + 运行指示 0xFF；FC43/MEI 0x0E 读设备标识（basic/regular/extended 与按对象 id 读取，响应超 253 字节时按 `more`/`nextObjectID` 分页）；身份标识由 `slave.identity` 配置，缺省也保证 basic 三件套非空；版本号取构建信息 |
| Modbus ASCII | ✅ | `:` + 十六进制 + LRC + CRLF 分帧；逐字符读到终止符并跳过 `:` 前杂散字节；与 RTU 共用 conn/codec；同一串口不允许混跑两种分帧（配置期报错） |
| 虚拟驱动 | ✅ | 正弦/斜坡/随机/常量波形；丢包/超时/坏帧/地址错/断连五种故障注入 |
| 调度与熔断 | ✅ | tick、actor、写优先、指数退避、熔断后转探活、代次隔离 |
| Outbox | ✅ | 文件 WAL + 分区确认位点 + 三重上限 + 定期压缩；默认批量 flush |
| Webhook 上报 | ✅ | 批量 POST、自定义头、失败不推进位点、恢复后自动补投 |
| journal 本地归档 | ✅ | `storage.journal=true` → `<dir>/journal.ndjson` |
| MQTT 上报 | ✅ | 自写 MQTT 3.1.1 客户端（零依赖）：QoS1 等 PUBACK 才推进位点、keepalive、重连后补发 `$birth` 与 retained `$state`、LWT `$death`；显式拒绝 QoS2 与 TLS/WS |
| MQTT 南向（订阅取数） | ✅ | 连接时按点位主题批量 SUBSCRIBE（同主题只订一次），消息进本地最新值表，`Read` 只读表不发网络请求；载荷支持纯文本标量与 JSON（`address.selector`，`a.b` / `a[0].b`）；写入 = 向同一主题 PUBLISH（QoS1）；SUBACK 0x80 视为配置错误直接失败而非「在线但没数据」；静默且 PING 往返失败才判断链 |
| MQTT 南向的语义边界 | ⚠️ | 质量码只反映「消息新鲜度」（`staleAfter = max(3×周期, 5s)`，兜底 30s）：收到过且未超时 → good；超时 → stale 且保留最后值；从未收到 → bad。写入成功只代表「已发出」不代表「设备已执行」，因此回读不一致时结果留在 uncertain |
| MQTT 南向不支持项 | ❌ | 点位的 `path` 只能是具体主题（禁止 `+` / `#` 通配符，否则一个点位对应 N 个主题、值无法归属）；不做持久会话 / ACL / TLS / WebSocket / QoS2；不做二进制载荷解码 |
| MQTT 内置 Broker | ✅ | `uic sim -protocol mqtt`：CONNECT/SUBSCRIBE/PUBLISH/PING 全套、retained 内存表、每秒发布演示点位并回显 setpoint 写入；同时是集成测试的对端（全项目唯一一份 Broker 实现） |
| OPC UA 主站 | ✅ | UACP HEL/ACK + OPN 安全通道 + CreateSession/ActivateSession 会话三层；批量读（≤100 节点/请求）、写、Browse、GetEndpoints；SecurityPolicy=None / SecurityMode=None / 匿名；NodeId 支持 `ns=2;s=` / `ns=2;i=` / `i=` / 裸字符串；TokenId 寿命 3/4 时自动 OPN 续订；数值越界判坏不截断 |
| OPC UA 订阅 | ✅ | 通道级 `subscribe` 开关（默认关闭 = 每轮 Read 轮询）；CreateSubscription + CreateMonitoredItems + Publish 长轮询 + 逐条确认；走**独立第二条连接**（Publish 不阻塞读写）；`QueueSize=1` + `DiscardOldest` 只保留最新值；监视项建立后立刻收到一次初始样本；建订阅失败让 `Open` 失败（不静默退化成轮询）；运行期断开自动重建并暂时回退真读 |
| OPC UA 订阅的边界 | ⚠️ | 不做同一连接上的多请求复用（那需要一套完整的多路复用器）；不实现 Republish 补取（跳号时靠下一条通知）；采样周期只发配置值，不做服务端采样率协商；`publishingInterval` / `samplingInterval` 必须配合 `subscribe=true` |
| OPC UA 历史 / 方法 / 事件 / 证书 | ❌ | 证书体系是另一套运维域；方法与事件需要完整地址空间与权限模型；历史由自建有界采样日志承担（ADR-0002） |
| OPC UA 服务端（内置演示） | ✅ | `uic sim -protocol opcua`：Root/Objects/Server/ServerStatus/CurrentTime + ns=2 Plant.{Counter,Temperature,Running,Label,Setpoint}；只读节点写入回 `BadNotWritable`；未知节点回 `BadNodeIdUnknown`；未实现的服务（方法调用/历史读取）回 `BadServiceUnsupported`；订阅按值变化推送，空闲按 KeepAlive 周期发空通知 |
| OPC UA 北向（对外暴露为服务端） | ✅ | 地址空间 `Root/Objects/{site}/{device}/{point}`（变量 `ns=2;s={site}/{device}/{point}`）；只暴露 enabled 的设备与点位；质量码走 `DataValue.Status`（good→Good，uncertain/stale→Uncertain，timeout/comm_error/address_error/not_connected→对应 Bad*）；写入穿透到 `Runtime.Write`（5 秒超时），拒绝码映射回 `BadNotWritable`/`BadTypeMismatch`/`BadNodeIdUnknown`/`BadNoCommunication`；订阅推送直接复用服务端内核；配置变更按指纹热重建，端口被占用时启动失败、热更新失败则回退上一份配置 |
| OPC UA 北向的边界 | ⚠️ | 只做 None/None + 匿名（`allowAnonymous` 必须显式为 true）；不做历史读取、方法调用、事件、别名、分块；地址空间是平铺的两级目录（站点/设备/点位），不映射设备层级结构，也不暴露设备状态变量（状态看 REST `/api/v1/devices` 或 MQTT `$state`）；每次配置变更都会重建服务端，SCADA 会经历一次重连 |
| 计算点（纯表达式） | ✅ | `protocol: "computed"` + `address.kind: "computed"` + `address.expr`；零依赖表达式引擎（编译期类型检查、短路求值、除零/NaN/Inf 报错）；同设备依赖在一次 `Read` 内按拓扑序闭合，跨设备最终一致；质量取最差且指名坏输入；整数四舍五入 + 越界判坏 |
| 计算点的边界 | ⚠️ | 只读（写入返回 `point.not_writable`）；只支持数值与布尔（字符串/Bytes/BCD 拒绝）；引用写作 `device:point`，因 ID 允许 `-`，**减号两边要留空格**（`line-a:power-1` 是引用）；跨设备引用慢一拍 |
| 计算点的环检测 | ✅ | 自引用、两点互引、**跨设备互引**都在配置期拒绝，报错打印完整路径（`a -> b -> a`） |
| 报警引擎 | ✅ | 五态机（normal/pending/active/acked/cleared）；阈值（单侧/双侧 + 滞回）或布尔表达式（复用 `calc`）；`onDelay`/`offDelay` 是「连续满足」；输入不可信时不动状态机；确认幂等、批量原子；规则热更新按判定条件哈希决定保留或重置 |
| 报警的出口 | ✅ | REST（列表/单条/确认）、SSE `/alarms/stream`、MQTT retained `uic/{site}/$alarm/{id}`（重连整体补发、删除规则发空载荷清理）、`health.alarms`、5 个指标 |
| 报警的边界 | ⚠️ | 状态只在内存（重启后仍成立的条件经 `onDelay` 重新报警，不恢复 `ackBy`）；不做报警抑制/依赖（父报警压子报警）、不做排班与升级通知；`autoAck` 只适合纯提示类规则 |
| 历史采样日志 | ✅ | `storage.history.enabled=true`：事件驱动 + 心跳补点（死区 / 最小间隔 / 质量变化 / `disabled` 不记）；NDJSON 分段、按字节上限与保留期整段淘汰且永远保留一段；掉电半行触发新段；查询左闭右闭、结果升序、桶起点对齐、桶质量取最差、桶数上限 5000 |
| 历史查询 API | ✅ | `GET /api/v1/history`：原始点 + 定步长聚合（last/first/avg/min/max/count）；`from`/`to` 支持 RFC3339、`now` 与相对写法（`-1h`）；关掉时返回 `409 history.disabled` 而不是空结果 |
| 历史的边界 | ⚠️ | 单点位查询（不做跨点 join / 多点多列）；不做样本级删除（只按段淘汰）；采样策略全局一套（需要区别对待时用死区与停用点位）；`dir`/`maxBytes`/`segmentBytes`/`maxAge` 改动需重启，`minInterval`/`heartbeat`/`deadband` 可热改 |
| 历史窗口聚合（`avg(近 5 分钟)`） | ⏳ | 历史库已可查；「在报警/计算表达式里引用历史窗口」尚未接入（属 Wave 2 的表达式扩展） |
| 配置版本/回滚/diff | ✅ | 内容哈希版本、快照、原子写、失败自动恢复、回滚缺版本返回 404 |
| 认证与授权 | ✅ | 多身份静态 Token（`server.users`，`token` 或 `tokenSha256`）；Token 先 SHA-256 再定长比较；三角色九权限按动作划分，`viewer ⊆ operator ⊆ admin`，未知角色/权限全拒；高危命令 handler 内二次判定；凭据表整体热替换，删掉的用户下一条请求即 401 |
| 认证的边界 | ⚠️ | 不做会话/口令/过期/自助轮换（见 ADR-0003）；不做账户锁定与失败次数限制；未配置凭据时进入 open 模式并告警，不生成默认口令 |
| 审计 | ✅ | 写入/命令/报警确认/配置变更/越权尝试逐条落盘（写入按点位逐项）；NDJSON 分段 + SHA-256 哈希链（`seq`/`prev`/`hash`）；`/audit` 倒序游标查询、`/audit/verify` 指认第一条不一致记录、`/audit/stats` 水位；默认开启，写失败只计数不阻断业务 |
| 审计的边界 | ⚠️ | 不加密（能读文件即可看内容，但不含凭据）；哈希链不能自证「从第一段开始整体重写」（需外部锚点，Wave 2）；`dir`/`maxBytes`/`segmentBytes`/`maxAge` 改动需重启，其余即时生效 |
| TLS | ✅ | `server.tls.enabled` + `certFile`/`keyFile`；开启但缺证书时回退明文并打 WARN（自签证书自动生成仍属 Wave 2） |
| `uic token` | ✅ | 生成 ≥16 字符 base64url Token + `tokenSha256` + 可直接粘贴的 `server.users` 片段（`-count` / `-json`） |
| `uic -check` | ✅ | 纯校验，不写快照/状态文件（可在 CI 里安全运行） |

> 判断标准：**没实现的能力必须显式失败或显式告警，不允许静默降级。** 这是从 38 份分析里提炼出的第一条工程纪律（见 `02-synthesis/03-gap-analysis.md` §6「反风险」）。

---

## 12. 约定

### 12.1 脱敏

所有文档与代码**不得出现**：参考项目的名称、仓库地址、作者名、公司名、产品商标、包名/命名空间前缀。统一用 `PA-xx` + 技术特征描述指代。

注意一个自指陷阱：**关键词表本身也不能入库**，否则「用来防泄露的清单」会变成泄露源。因此本仓库只保留方法论，不保留名单：

1. 关键词表由维护者保存在仓库外（本地文件或密码管理器），评审时手工比对。
2. CI 只做可自动化的部分：扫描 `docs/` 与 `cmd/` 中的外部链接，白名单是 `localhost` / `127.0.0.1` / `0.0.0.0` 与占位符形式（如 `<host>`、`<ip>`）。命中白名单之外的链接即失败——这一条能拦住绝大多数误粘贴。
3. 每份新文档合入前，由第二个人按「名称 / 地址 / 作者 / 商标 / 包名」五类逐条过一遍。

`PA-xx` 编号与技术画像的对应关系只存在于 `01-project-analysis/` 的文件名与首段，不进入代码、配置或 API。

### 12.2 命名

| 对象 | 规则 | 示例 |
|---|---|---|
| Site | `^[a-z][a-z0-9-]{1,31}$` | `plant-a` |
| Channel | `^[a-z0-9][a-z0-9-]{0,31}$` | `ch-plc` |
| Device / Point | `^[a-z0-9][a-z0-9._-]{0,63}$` | `plc-01`、`motor.speed` |
| 指标 | `uic_<subsystem>_<name>` | `uic_outbox_pending` |
| 日志事件 | 小写下划线 | `outbox_deliver_failed` |
| 错误码 | `<domain>.<reason>` | `config.version_not_found` |

### 12.3 目录

| 目录 | 放什么 | 不放什么 |
|---|---|---|
| `docs/01-project-analysis/` | 逐方案六章分析 | 产品决策 |
| `docs/02-synthesis/` | 横向归纳、矩阵、缺口 | 单个方案的细节 |
| `docs/03-product/` | 产品定义、契约、指标 | 实现细节（放代码注释） |
| `docs/04-decisions/` | ADR（一个决策一个文件，不可修改，只能新增） | 讨论过程 |
| `internal/<pkg>/` | 单一职责包 + 同包测试 | 跨包工具函数（放 `internal/xxx` 的公共包） |
| `examples/` | 可直接跑的配置 | 运行时产物（`snapshots/`、`*.state.json` 不应提交） |

---

## 13. 下一步去哪

| 想做的事 | 看这里 |
|---|---|
| 判断该不该做这个产品 | `02-synthesis/03-gap-analysis.md` §5「目标产品的差异化定位」 |
| 排第一版的开发计划 | `03-product/07-roadmap.md`（三波 + 验收门槛） |
| 加一个新协议 | `03-product/04-protocol-catalog.md`（难度分级 + 工作量估算）+ `04-driver-sdk-spec.md` |
| 定性能/可用性指标 | `03-product/06-nfr.md` |
| 评审语言与运行时选型 | `04-decisions/0001-language-selection.md`（含加权评分、敏感性分析、退出条件） |
| 理解某个具体方案 | `01-project-analysis/PA-xx-*.md` |

---

## 附：文档写作口径

- **数字必须可核实。** 协议数量按代码里的实现条目计，不采信 README 自述；规模按文件行数计；评分按统一标准（见 `_CONVENTIONS.md`）。
- **区分「文档提及」与「可核实实现」。** 例如「补发」在 11 份文档里出现，真正实现持久化 Outbox 的只有 1 份。
- **每条「应规避」都要有代价描述。** 不说「代码质量差」，而说「插件抽象在只有 0 个真实插件时就定型，导致 258 个文件里 60% 是空壳」。
- **结论章必须给三条关键结论。** 不允许「综上所述，值得借鉴」这种无信息量的收尾。
