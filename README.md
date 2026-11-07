# 统一工业通信网关（UIC）

跨平台工业数据采集中间件：南向接协议，中间做建模与调度，北向出标准接口（REST / SSE / MQTT / OPC UA）。

设计重心不在「能连多少种设备」，而在**可治理**：断网不丢数据、配置改坏能退回、设备坏了能看出为什么坏、一个二进制能拷到 ARM 盒子上跑。

## 快速开始

```bash
go build -o uic ./cmd/uic
./uic -check -config examples/demo.json   # 只校验配置
./uic -config examples/demo.json          # 虚拟设备演示，无需硬件
curl -s localhost:8080/api/v1/health
```

真实协议回环（主站 + 内置从站，同一进程树内）：

```bash
./uic sim -listen 127.0.0.1:15020 &
./uic -config examples/modbus-loopback.json
curl -s localhost:8081/api/v1/points
```

串口 / Modbus RTU（有硬件改端口即可，无硬件可用 `socat` 造一对 pty）：

```bash
./uic -check -config examples/modbus-rtu.json
./uic -config examples/modbus-rtu.json
```

Modbus ASCII（`:` + 十六进制 + LRC + CRLF，老式仪表常见）：

```bash
./uic -check -config examples/modbus-ascii.json
```

S7（S7comm / ISO-on-TCP，TCP 102）：

```bash
./uic sim -protocol s7 -listen 127.0.0.1:15102 &   # 内置 S7 从站
./uic -config examples/s7.json
curl -s localhost:8085/api/v1/points
#   7 点：DB1 的 REAL/INT16/DINT32/STRING、DB2 WORD、IW0、M0.3
#   -pdu 240 可验证按协商 PDU 切块

# 块服务（不需要起网关，直接连 PLC 或内置从站）：目录 / 属性 / 上传
./uic s7blocks -addr 127.0.0.1:15102                  # 七类块数量
./uic s7blocks -addr 127.0.0.1:15102 -type db         # DB 块号列表
./uic s7blocks -addr 127.0.0.1:15102 -type db -num 1  # DB1 的 78 字节属性
./uic s7upload -addr 127.0.0.1:15102 -type db -num 1 -out db1.bin
```

块服务刻意不进入采集调度：它用于备份、比对与排障，一次可能传几 MB，与周期采集
的延迟目标冲突。三条最容易写错的协议细节已经固化在 `codec/s7` 里：**ACK/ACK_DATA
是 12 字节头**（Job/UserData 是 10 字节，参数区从 12 而不是 10 开始）；**ListAll
的数据区是 `0A 00 0000`**，不是其它查询用的 `FF 09`；**上传的块长度是 5 位十进制
ASCII**，分片以 `param[1]` 判断是否最后一片（0 才是最后一片）。上传失败也会补一条
End：不释放 PLC 侧会话，反复重试就可能把上传会话耗尽。

OPC UA（二进制，SecurityPolicy None / 匿名）：

```bash
./uic sim -protocol opcua -listen 127.0.0.1:14840 &   # 内置 OPC UA 服务端
./uic -config examples/opcua.json
curl -s localhost:8086/api/v1/points
#   6 点：double / int32 / bool / string / 可写 double / 标准节点 ns=0;i=2258
#   写穿透：curl -X POST .../points/ua1/setpoint/write
```

OPC UA 北向（把点位表暴露成服务端地址空间，供 SCADA 直接读写）：

```bash
./uic -config examples/opcua-north.json        # REST :8094，OPC UA 服务端 :14841
./uic uaread -addr 127.0.0.1:14841 \
  -nodes 'ns=2;s=north-lab/line-a/temperature,ns=2;s=north-lab/line-a/setpoint' -type float32
./uic uaread -addr 127.0.0.1:14841 -nodes 'ns=2;s=north-lab/line-a/setpoint' \
  -type float32 -write 42.5
#   NodeId 约定：ns=2;s={site}/{device}/{point}
#   写入真的过南向设备；只读点位回 BadNotWritable，未知点位回 BadNodeIdUnknown
```

`uic uaread` 与 `uic mbread` 是同一类工具：不装任何第三方软件即可自证链路。
北向写入复用 REST 的写入路径（设备 actor、类型校验、幂等），不存在第二条
绕过治理的写路径。质量码走 `DataValue.Status`（Good / Uncertain / Bad*），
不额外造一个 quality 变量。

OPC UA 订阅（服务端推送，而不是每轮 Read 轮询）：

```bash
./uic sim -protocol opcua -listen 127.0.0.1:14840 &
./uic -config examples/opcua-subscribe.json
curl -s localhost:8093/api/v1/points/ua1/counter   # 每秒自增，由服务端推送
#   channels[].subscribe=true；publishingInterval / samplingInterval 可调
```

从站（把网关采集到的点位反向暴露给 SCADA / 第三方主站）：

```bash
./uic sim -listen 127.0.0.1:15020 &           # 上游设备
./uic -config examples/modbus-slave.json &    # REST :8086，Modbus 从站 :1502
./uic mbread -addr 127.0.0.1:1502 -unit 1 -area holding -offset 0 -qty 4
./uic mbread -addr 127.0.0.1:1502 -area holding -offset 3 -write 777
```

`uic mbread` 是内置的极简主站：寄存器打印 uint16 / int16 / 十六进制并拼 float32，
位区逐位打印。写入会穿透到真实设备，不是改缓存。

串口从站（RS-485 半双工；`serial.framing` 必须显式写 `rtu` 或 `ascii`）：

```bash
# 有硬件时把 serial.port 改成 /dev/ttyUSB0；没有硬件可用一对 pty 模拟总线：
#   socat -d -d pty,raw,echo=0 pty,raw,echo=0
./uic -config examples/modbus-slave-rtu.json &     # 从站打开 serial.port
./uic mbread -transport rtu -port /dev/pts/11 -unit 1 -area holding -offset 0 -qty 2
./uic mbread -transport rtu -port /dev/pts/11 -unit 1 -area holding -offset 2 -write 777
# 串口参数：-baud / -dataBits / -parity / -stopBits（与配置同名）
```

串口上的地址语义与 TCP 不同，且都是刻意的：非本机单元号**静默**（同一条总线上
还有别的从站），广播（单元 0）只执行写不回应，CRC/LRC 错后丢弃残字节重新同步，
半帧直接丢弃而不是把下一帧的开头接上来。RTU 按功能码推导帧长，推不出来时（未知
功能码）才退回 3.5 字符静默；ASCII 以 `:` 起始、CRLF 结束。USB 适配器掉线
（EIO）会按 1s→2s→…→30s 退避重开，不需要重启进程。串口端口与主站通道、其他从站
按字面路径互斥，配置期就会拒绝重复占用。

从站还实现了诊断与设备识别：FC8（回显/清计数/六类计数）、FC11（事件计数器）、
FC17（服务器 ID）、FC43/MEI 0x0E（读设备标识，支持 basic/regular/extended 与按
对象读取，响应超长时自动分页）。`slaves[].identity` 可配置厂商、型号、版本等
信息；不配时 basic 三件套也有默认值，版本取运行进程的构建版本。

MQTT 北向（需要一个 Broker；`mosquitto` 或任意 MQTT 3.1.1 服务端均可）：

```bash
mosquitto -p 1883 &
./uic -config examples/mqtt.json &
mosquitto_sub -h 127.0.0.1 -t 'uic/mqtt-lab/#' -v
#   uic/mqtt-lab/$birth                       {"schemaVersion":1,"online":true,...}
#   uic/mqtt-lab/line-a/temperature/value     {"schemaVersion":1,"device":"line-a",...}
#   uic/mqtt-lab/line-a/$state                {"schemaVersion":1,"state":"collecting",...}
```

Broker 断开时数据进本地 Outbox（不丢），恢复后自动补投；`$state`/`$birth` 是
retained 的，消费方连上即可读到当前状态。

MQTT 南向（订阅别人发来的数据，方向相反）：

```bash
./uic sim -protocol mqtt -listen 127.0.0.1:18830 &   # 内置 Broker，每秒发布演示点位
./uic -config examples/mqtt-southbound.json          # REST :8087
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8087/api/v1/points
#   6 点：纯文本标量 temp / counter，JSON 字段 running / mode / cycle，可写 setpoint
#   写穿透：POST .../points/edge01/write  → 发布到 plant/line1/setpoint
```

MQTT 是推送型协议，而驱动契约是拉取型，所以驱动在连接时订阅、把消息存进本地
最新值表，`Read` 只查表。由此两条语义必须记住：**质量码只反映消息新鲜度**
（超时 → stale 但保留最后值），以及**一条消息都没来不等于断链**（只有安静超过
一个新鲜度窗口且 PING 往返失败才判断链）。写入成功也只代表「已发出」，
结果质量码是 `uncertain`，回读一致才转 good。

计算点（纯表达式，没有任何物理链路）：

```bash
./uic -config examples/computed.json          # REST :8089
curl -s localhost:8089/api/v1/points | jq '.items[] | {point, value, quality}'
#   9 点：line-a 三个源点 + line-a-kpi 六个计算点
#   cost = power × rate；cost_with_tax = cost × 1.13（同一轮内按拓扑序求值）
#   写入计算点返回 point.not_writable —— 它没有可写回的地址
```

`protocol: "computed"` + `address.kind: "computed"` + `address.expr`。表达式引擎
零依赖、编译期做类型检查、短路求值，除零 / NaN / Inf / 整数越界都判坏而不是
算出一个错数字。引用写作 `device:point`；因为 ID 允许 `-`，**减号两边要留空格**
（`line-a:power-1` 是引用，`line-a:power - 1` 才是减法）。质量沿依赖传播：
任一输入不可用则不计算，输出取最差质量并指明是哪个输入。

报警（阈值 / 滞回 / 延时 / 确认）：

```bash
./uic -config examples/alarms.json            # REST :8091
curl -s localhost:8091/api/v1/alarms | jq '.counts'
curl -s 'localhost:8091/api/v1/alarms?state=active' | jq '.items[] | {id,state,value}'
curl -s -X POST localhost:8091/api/v1/alarms/high-power/ack -d '{"by":"operator-1"}' | jq
curl -sN 'localhost:8091/api/v1/alarms/stream?heartbeat=5s'   # 事件流
```

示例里压机功率是一条 10~90kW 的正弦波，5 条规则演示了阈值 + 滞回 + `onDelay`
（功率过高）、低阈值（空转）、布尔表达式（功率与速率不匹配）、`autoAck`（尖峰）
与 `disabled`（维护窗口）。三条语义必须记住：**延时是「连续满足」**，中途抖动
清零；**输入质量不可信时不动状态机**——数据断了既不当成「正常」也不当成「报警」；
**`active` 与 `cleared` 都需要人确认**，前者转 `acked`，后者转 `normal`，另外
`pending` 不可确认（否则等于允许提前消音）。

历史（有界采样日志，不引入数据库）：

```bash
./uic -config examples/history.json           # REST :8092
curl -s 'localhost:8092/api/v1/history?device=press&point=power&from=-5m&limit=5' | jq
curl -s 'localhost:8092/api/v1/history?device=press&point=power&from=-1h&step=1m&agg=avg' | jq '.buckets[0]'
curl -s localhost:8092/api/v1/health | jq '.history'
```

示例把采样策略配成「1 秒最小间隔 + 30 秒心跳 + 0.5 死区」。三条语义必须记住：
**它是旁路**——写失败只计数，绝不反压采集；**整段淘汰**——按字节上限与保留期
淘汰最旧的段，永远保留一段；**心跳不撒谎**——只有样本本身仍新鲜才补点，设备停
了就不再写「此刻仍然是 42」。查询左闭右闭、结果按时间升序、桶质量取桶内最差，
非数值点位的 `min/max/avg` 是 `null` 而不是 0。

历史窗口聚合（在计算点/报警表达式里直接引用历史）：

```bash
./uic -config examples/history-window.json    # REST :8095
curl -s localhost:8095/api/v1/points | jq '.items[] | select(.device=="kpi") | {point,value}'
#   avg1m = avg(press:power, 1m)    1 分钟平均功率
#   peak5m = max(press:power, 5m)   5 分钟峰值
#   samples5m = count(press:power, 5m)
#   load_factor = press:power / avg(press:power, 1m)
```

`avg/min/max/sum/count(ref, 5m)`：第二个参数是时长时，聚合对象从「当前值」变成
「最近一个窗口的历史样本」，窗口 1s..24h。永久有效的三条语义：**只统计 good 的
数值样本**（stale 与开关量不计）；**空窗口是失败而不是 0**（计算点 bad、报警
unknown）；**窗口引用不算当前值依赖**，所以 `a = avg(b, 5m)` 与 `b = a + 1`
可以共存。历史只记变化 + 心跳，窗口里可能只有 1 个样本——想知道够不够，用
`count()` 而不是猜。

认证、授权与审计（三角色 + 哈希链）：

```bash
./uic token -name ops -role operator     # 明文 Token + tokenSha256 + 可粘贴片段
./uic -config examples/rbac.json         # REST :8093，三个演示用户
TOKEN=Bnmcwphg0lxA1XTng-SuZJyfXqvYU8bR   # examples/rbac.md 里的 ops
curl -s -H "Authorization: Bearer $TOKEN" localhost:8093/api/v1/points          # 200
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
     localhost:8093/api/v1/commands -d '{"action":"outbox.drop","confirm":true}'  # 403
curl -s -H "Authorization: Bearer KilpNTTr9xyEEaJi3uaJucEuNvobBtSH" \
     'localhost:8093/api/v1/audit?result=denied' | jq
```

三个角色按**动作**划权限，且 `viewer ⊆ operator ⊆ admin`：`viewer` 只看；
`operator` 能写点位、确认报警、跑普通命令、导诊断包；`admin` 多出配置、审计与
高危命令（`outbox.drop` / `config.rollback` / `log.level`）。权限写在路由旁边，
高危命令在 handler 里再判一次——新增命令时忘改路由也不会把危险动作漏给 operator。

Token 在服务端先 SHA-256 再定长比较，配置可以只存 `tokenSha256`，泄露配置文件
不等于泄露凭据。凭据表本身是可整体替换的，`server.users` 改动 apply 后**下一条
请求**就按新表判定：删掉的用户立刻 401，而不是等重启。没配任何凭据时进入开放
模式，启动横幅与 `auth_open_mode` WARN 会明说，而不是偷偷生成一个默认口令。

审计是一份带 SHA-256 哈希链的 NDJSON：`seq` 连续、`prev`/`hash` 相连，删除或
篡改任何一条都会被 `/api/v1/audit/verify` 指出来。写入、命令、报警确认、配置
变更、越权尝试都记，且写入是**逐点位**记的——批量写一半失败时，能回答「到底
哪个点没写进去」。审计默认开启；写失败只计数（`uic_audit_write_errors_total`）
并记 ERROR，绝不让磁盘满导致产线停摆。

## 文档

- **[docs/00-README.md](docs/00-README.md)** — 总索引：阅读路径、38 个参考方案总览、结论速查、代码地图、接口速查、实现边界
- `docs/01-project-analysis/` — 38 份逐方案六章分析（PA-01 … PA-38）
- `docs/02-synthesis/` — 能力矩阵 / 架构原型 / 缺口分析 / 协议目录
- `docs/03-product/` — PRD / 架构 / 领域模型 / 驱动 SDK / 北向 API / NFR / 路线图
- `docs/04-decisions/` — ADR（语言选型、历史库选型）

## 开发

```bash
go build ./... && go vet ./... && gofmt -l internal cmd
go test -race -count=1 ./...
```

零第三方依赖、零 CGO，仅标准库。

## 目录

| 路径 | 内容 |
|---|---|
| `cmd/uic/` | 单入口：`run` / `sim`（Modbus / S7 / OPC UA / MQTT）/ `mbread` / `uaread` / `s7blocks` / `s7upload` / `token` / `-check` / `version` |
| `internal/model` | 通道 / 设备 / 点位 / 地址 / 采样 / 质量码 |
| `internal/driver` | 驱动契约（Codec / Transport / Driver）、错误分类、注册表、只读 `Lookup` 注入 |
| `internal/calc` | 零依赖表达式引擎（编译期类型检查、短路求值、引用 `device:point`） |
| `internal/alarm` | 报警引擎：五态机 / 滞回 / 延时 / 表达式规则 / 确认 / 订阅 |
| `internal/history` | 有界采样日志：NDJSON 分段 / 整段淘汰 / 原始查询与定步长聚合 |
| `internal/codec/modbus` | Modbus 纯函数编解码（主站与从站共用） |
| `internal/codec/value` | 协议无关值编解码（重排 / 数值 / 字符串 / BCD） |
| `internal/codec/s7` | S7 纯函数编解码（TPKT / COTP / S7 PDU / 块目录与块上传） |
| `internal/codec/opcua` | OPC UA 纯函数编解码（UACP、安全报文、NodeId、Variant、九类服务） |
| `internal/driver/modbus` | Modbus TCP + RTU + ASCII 主站 |
| `internal/driver/s7` | S7（S7comm）主站；块服务走独立一次性 `Session`（目录 / 属性 / 上传） |
| `internal/driver/opcua` | OPC UA 主站（None/None + 匿名，三层状态机）；可选订阅推送（独立第二条连接） |
| `internal/driver/mqtt` | MQTT 南向（订阅取数）：推送适配成拉取，三档新鲜度质量码 |
| `internal/driver/computed` | 计算点：表达式驱动，同设备依赖在一次 Read 内按拓扑序闭合 |
| `internal/driver/virtual` | 虚拟设备（波形 + 故障注入） |
| `internal/sim/modbus` | 内置 Modbus TCP 从站 |
| `internal/sim/s7` | 内置 S7 从站（内存 DB/M/I/Q 区；支持块目录 / 属性 / 上传） |
| `internal/opcuaserver` | 可复用 OPC UA 服务端内核（地址空间 + 订阅/发布 + 标准节点） |
| `internal/sim/mqtt` | 内置 MQTT 3.1.1 Broker（兼 `uic sim -protocol mqtt` 运行时） |
| `internal/slave/modbus` | Modbus TCP 从站（把点位映射给外部主站，写穿透到南向） |
| `internal/planner` | 点位 → 批量读请求 |
| `internal/scheduler` | tick / actor / 信号量 / 退避 / 熔断 / 代次隔离 |
| `internal/pipeline` | 缩放 / 质量 / 死区 |
| `internal/cache` | 当前值唯一事实源 + 变更订阅 |
| `internal/outbox` | 持久上报队列（分区 / WAL / 确认位点 / 三重上限） |
| `internal/config` | 配置校验 / 内容哈希版本 / 快照 / 回滚 |
| `internal/obs` | 指标 / 结构化日志 / 日志环形缓冲 |
| `internal/rbac` | 角色 / 权限矩阵（viewer / operator / admin，按动作授权） |
| `internal/auth` | 凭据表：Token → 身份；`Reloadable` 支持热替换；无凭据时 open 模式 |
| `internal/audit` | 审计存储：NDJSON 分段 + SHA-256 哈希链 + 查询 / 校验 |
| `internal/northbound/httpapi` | REST + SSE + 认证授权 + 错误码 |
| `internal/northbound/opcua` | OPC UA 北向：点位表 → 地址空间，质量码映射，写穿透到南向 |
| `internal/mqtt` | 自写 MQTT 3.1.1 客户端（北向发布 + 南向订阅共用；QoS0/1、retained、LWT、PING 往返探测） |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 |
| `internal/runtime` | 唯一编排层 + 看门狗 + 后端实现（含各 sink 装配） |

## 许可

待定（`docs/04-decisions/` 补充）。
