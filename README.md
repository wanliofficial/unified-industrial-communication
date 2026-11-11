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

把 `format` 换成 `sparkplug-b` 就是 Sparkplug B（同一个 Broker、同一套凭据与
Outbox，生命周期由协议接管）：

```bash
./uic -check -config examples/sparkplug.json
./uic -config examples/sparkplug.json
#   任意 Sparkplug B 消费端订阅 spBv1.0/plant-a/#
#   STATE(online) → NBIRTH → DBIRTH(line-a) → DDATA → DBIRTH(line-b) → DDATA
```

主题由 `groupId/nodeId/设备名` 拼出，所以 `topic` 必须留空、`retain` 必须为
false（只有 STATE 是 retained）。NBIRTH 从 0 编号、DBIRTH 重置设备序号；重连、
NCMD `Node Control/Rebirth`、设备故障恢复都会重发 BIRTH。质量非 good 用
`is_null` 表达，单位走 `engUnit` 属性；`bytes` 用 Bytes 类型 + `bytes_value`
保留原始字节。DataSet、模板、别名与报警出口不做——半成品比不做更难排查。

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

组态（模板实例化 + 点表 CSV 导入）：

```bash
# 模板库：examples/templates/*.json
cat examples/templates/modbus-meter.json
./uic instantiate -template examples/templates/modbus-meter.json \
    -param index=1 -param unitId=3 -channel ch-modbus -out meter-1.json

# 点表 CSV：先预览，再写回整份配置
./uic import -csv points.csv -device press -channel ch-modbus \
    -config examples/modbus-loopback.json -dry-run
./uic import -csv points.csv -device press -channel ch-modbus \
    -config examples/modbus-loopback.json -out config.new.json -replace
```

模板只做**整值替换**，不做算术：`{base + 2}` 不是合法占位符。需要偏移的点位
各自声明参数。`int` / `float` / `bool` 参数在「整个字面量就是一个占位符」时落成
JSON 数字/布尔，其余情况按字符串转义。点表导入默认**不覆盖**已有点位：同 ID
内容不同即冲突，必须显式 `-replace`，避免「地址改了一半」把调好的缩放、死区、
单位一起冲掉。`-dry-run` 与真正写入走同一份合并结果。

三菱 MC（3E 二进制帧，TCP 5000）：

```bash
./uic sim -protocol mc -listen 127.0.0.1:5000 &   # 内置 MC 从站
./uic -config examples/mc.json
curl -s localhost:8087/api/v1/points
#   5 点：D0 uint16=1234、D1 int16=5678、D2 float32=48.0(CDAB)、M0 bool、M1 可写 bool
#   自检（不需要网关）：./uic mcread -addr 127.0.0.1:5000 -area d -offset 0 -qty 2
```

MC 3E 只做二进制帧 + TCP；ASCII/4E/UDP 形态配置期即拒。位软元件（M/X/Y/B）按位
打包、首点是 bit0；X/Y 编号按十六进制理解（`X1F` 填 31）；字软元件上的 bool 必须
显式 `address.bit`（0..15），驱动做读整字→改位→写整字的 RMW，不清零邻位。

欧姆龙 FINS（FINS/TCP 9600）：

```bash
./uic sim -protocol fins -listen 127.0.0.1:9600 &   # 内置 FINS 从站
./uic -config examples/fins.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8088/api/v1/points
#   6 点：DM0 uint16=1234、DM1 int16=5678、DM2 float32=48.0(低字在前)、
#         CIO0.0 run=true、CIO0.1 fault 可写 bool、DM10 counter 每秒自增
#   自检（不需要网关）：./uic finsread -addr 127.0.0.1:9600 -area dm -offset 0 -qty 2
#   位读写：./uic finsread -addr 127.0.0.1:9600 -area cio -offset 0 -bit 1 -write on
```

EtherNet/IP + CIP 显式报文（TCP 44818）：

```bash
./uic sim -protocol enip -listen 127.0.0.1:44818 &   # 内置 CIP 从站
./uic -config examples/enip.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8089/api/v1/points
#   7 点：DINT=1234、INT=5678、REAL=48.0 可写、BOOL=true、STRING="PUMP-01"、
#         DINT_TAG.bit1 可写 bool、COUNTER 每秒自增
#   自检（不需要网关）：./uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG
#   位读写：./uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG -type bool -bit 1 -write off
```

EtherNet/IP 只做 **CIP 显式报文 + 符号标签**（Read/Write Tag），不做隐式 I/O（UDP 2222）、
不做对象/属性寻址、不做结构体/数组自动展开。`address.area` 是标签名（大小写不敏感）、
`offset` 是数组起始元素、`length` 是 bytes 字节数/string 字符容量、`bit` 是整数标签内的
位号（-1 表示标签本身是 BOOL）。数值线上小端；同宽有符号/无符号可互换，类型以应答里的
实际类型为准；bytes 两阶段读（先学宽度再按区间重读）；一轮多条读走 Multiple Service
Packet，设备不支持时自动退回逐条请求。

BACnet/IP 对象属性（UDP 47808）：

```bash
./uic sim -protocol bacnet -listen 127.0.0.1:47808 &   # 内置 BACnet/IP 从站
./uic -config examples/bacnet.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8090/api/v1/points
#   7 点：温度 42.5、对象名 "AI-1"、单位 62、设定值可写、二进制状态、
#         标牌字符串可写、AV-3 每秒自增
#   自检（不需要网关）：./uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 1
#   写设定值：./uic bacread -addr 127.0.0.1:47808 -type analog-value -instance 2 -write 55.5
```

BACnet/IP 只做 ReadProperty（0x0C）/ WriteProperty（0x0F），不做 Who-Is 广播发现、
不做 COV 订阅、不做批量读、不做分段。点位用 `kind=object` 寻址：`objectType`（名字或编号）、
`instance`、`property`（`present-value` / `object-name` / `units` 等）、`propIndex`
（数组下标，0 表示不带）。`present-value` 的编码由对象语义决定（模拟量 REAL、
二进制 ENUMERATED、多状态 UNSIGNED、字符串 CHARACTER STRING），不是由平台 `dataType`
决定；可写性同样由对象决定——`analog-input` 的 `present-value` 永远只读。
UDP 无连接，驱动用 8 位 invoke ID 配对，并把整条会话串行化，迟到应答直接丢弃。

IEC 60870-5-104 调度主站（TCP 2404）：

```bash
./uic sim -protocol iec104 -listen 127.0.0.1:2404 &   # 内置 IEC 104 从站
./uic -config examples/iec104.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8091/api/v1/points
#   14 点：短浮点 42.5/7.25、标度化 1234、被取代值（uncertain）、带时标值、
#         单点 on/off、双点 1/2、累计量每秒自增、
#         单点命令 + 短浮点/标度化设定值 + 双点遥控（可写）
#   自检（不需要网关）：./uic iec104read -addr 127.0.0.1:2404 -ca 1
#   只验链路层：./uic iec104read -addr 127.0.0.1:2404 -probe
#   下发命令：./uic iec104read -addr 127.0.0.1:2404 -area double-command -cmd-ioa 2003 -write off
```

IEC 104 与其它协议的根本差别是**数据由从站推上来**：建链（STARTDT）后做一次站总召唤
拿快照，之后设备按变化主动上送，没有「读第 1001 号点」这种动作。所以驱动的 `Read`
**不做任何网络交互**，只查本地最新值表（与 MQTT 南向同一模式）。累计量不在站召唤范围
内，`Open` 里固定再发一次电能量召唤；设备不支持时回否定确认，按调试日志处理。

质量码只来自两处：ASDU 品质位（`IV`→bad、`NT`→stale、`SB`/`BL`/`OV`→uncertain）与
链路是否活着（TESTFR 往返）。**刻意不做「多久没更新就降级」**——104 只在变化时上送，
一个温度半小时不变是正常的，按时间降级会让整页看起来都不新鲜。

点位用 `kind=register` 寻址，`offset` 即 IOA（1..16777215），`area` 取 `ioa` /
`single-command` / `double-command` / `setpoint-float` / `setpoint-scaled` /
`setpoint-normalized`。命令类型由 `area` 声明，不由 `dataType` 推导：同一个 `float32`
可能是短浮点设定值，也可能是放大 100 倍的标度化整数。双点命令只接受 1（分）/2（合），
0 与 3 是标准里的「不允许」，本地即拒。同一 IOA 可挂两个点位（读状态 + 写命令）。

`iec104read` 不依赖点表：驱动旁路表按 IOA 记录设备实际送过的原始对象（类型标识、
传送原因、时标），初次接陌生设备时先看清「它送什么」再写点表。双点的 0/3 中间态
原样显示成「不确定 (3)」，不硬转成布尔。

IEC 60870-5-101 串行远动主站（FT1.2，串口 / 串口服务器）：

```bash
./uic sim -protocol iec101 -listen 127.0.0.1:4001 &   # 内置 101 从站（FT1.2 over TCP 透传）
./uic -config examples/iec101.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8092/api/v1/points
#   14 点：短浮点 42.5/7.25、标度化 1234、被取代值（uncertain）、带时标值、
#         单点 on/off、双点 1/2、累计量每秒自增（需电能量召唤）、
#         单点命令 + 短浮点/标度化设定值 + 双点遥控（可写）
#   自检（不需要网关）：./uic iec101read -addr 127.0.0.1:4001
#   只验链路层：./uic iec101read -transport serial -port /dev/ttyUSB0 -baud 9600 -probe
#   下发命令：./uic iec101read -addr 127.0.0.1:4001 -area double-command -cmd-ioa 2003 -write on
```

101 与 104 只在**应用层**相同：类型标识、传送原因、品质位、时标、命令格式是同一套，
本仓库把这一层抽成共享的 `internal/codec/iec60870`，两个驱动共用。承载层则是另一套
物理事实，六条差异决定了驱动的形状：

| 事实 | 104 | 101 |
|---|---|---|
| 谁可以发言 | 双方都可以（序号 + k/w 流控） | **只有主站**；从站被点名才回答 |
| 数据怎么来 | 从站按变化主动上送 | 主站轮询第 1/2 级；不轮询就永远没有新值 |
| 可靠性 | 15 位收发序号 | 帧计数位 FCB + 链路确认 + 应用层激活确认 |
| 建链 | TCP + STARTDT | 复位链路（固定帧 `FC=0`），没有会话 |
| 地址宽度 | 固定 ORG 1 / CA 2 / IOA 3 | 每一项都是工程参数，按装置手册配 |
| 帧边界 | APCI 长度字段 | 起始符 + 长度字段 + 校验和 + 结束符 |

所以 101 的 `Read` **真的去走线**：每轮请求第 1 级事件、按周期做站总召唤、再顺带
请求第 2 级，而不是像 104 那样只查本地表。总召唤**不放在 `Open`**：一台几百个点的
装置在 9600 的总线上传完整站快照要好几秒，塞进 `Open` 会让「连接超时」与「装置没
数据」两件事混在一起。**电能量召唤必须单独做一次**——累计量不在站召唤范围内，只发站
召唤的实现现场表现为「电度点位永远没有值」。终止报文的形态有三种（`100`/`101` 的激活
终止、`70` 初始化结束），但 `100` 的**激活确认不能当结束**，否则总召唤只回来一个点。

FT1.2 有三个必须按字节记住的点：**长度字段只计「控制域 + 链路地址 + ASDU」**（整帧 =
`L + 6`，按含头写会多算 6 字节）；**校验和从控制域累加到 ASDU 末字节**；**单字节帧没有
地址也没有控制域**（`E5` 肯定确认、`A2` 否定确认，把 `A2` 当 `E5` 的症状是「设备拒绝
了但主站报成功」）。链路地址宽度是 0..2 字节，`0` 表示不带地址，此时固定帧只有 4 字节。

重传必须**原样重发同一帧字节**（含 FCB），而不是重新组帧：重新组帧会把 FCB 翻转成
另一条新命令，从站再执行一次，症状是「确认超时后开关被拉合了两次」。本实现的
`sendUser` 保存整帧字节，超时后用同一份重发一次；内置从站按「最近一次被接受的 FCB」
去重，重复帧只回确认、不再执行。复位链路是唯一的「帧序号归零」手段，因此 `Open`
第一件事就是复位链路。

写入有**两层确认**：链路层 `E5` 只说明报文到了，应用层激活确认（`cause=7`）才说明
装置收到并要执行。把第一层当成功是这类驱动最常见的一处谎报——装置完全可能因为
「未知 IOA」「不允许遥控」而拒绝。本实现等到应用层确认才返回，`44..47`（未知类型/
原因/公共地址/IOA）按错误分类返回。写成功后用请求值回填本地缓存（质量 `uncertain`），
只补「没有」或「比命令更旧」的条目，避免抹掉一条更新的上送。

地址形态用 `kind=register`，`offset` 即 IOA，`area` 取 `ioa` / `single-command` /
`double-command` / `setpoint-float` / `setpoint-scaled` / `setpoint-normalized`。
点表层用 `WidthNone`：远动协议的值宽度由类型标识决定，地址里写 `length` 反而是误配。
**同一个 IOA 可以同时挂遥信与遥控**（`ioa:2003` 布尔状态 + `double-command:2003`
可写命令），两套语义、两种平台类型，所以缓存按**点位 ID** 而不是 IOA——按 IOA 存会让
后写入的一方覆盖另一方，症状是「布尔遥信显示成 double 的 2」。这条修正同时改到了 104。

剖面全部走设备标签，默认取「一台装置一个公共地址、站内点数不超过 65535」：链路地址
1（1 字节）、CA 1（`unitId`）、IOA 2 字节、ORG 0 字节；可用 `linkAddress` /
`linkAddressSize` / `commonAddress` / `commonAddressSize` / `ioaSize` / `orgSize` /
`interrogate` / `class2` 覆盖。把某一组当成标准写死，是接不同厂家设备时最先撞上的墙：
帧能收发、校验也对，只是所有字段整体错位一到三个字节。

`iec101read` 与 `iec104read` 一样不依赖点表：先复位链路（证明物理层与链路层通），再
请求链路状态，然后做一轮完整采集，最后按 IOA 列出装置实际送过的对象。串口远动的失败
模式几乎全是「没有应答」，所以它把「卡在哪一层」直接打出来：帧计数、解析失败帧数、
公共地址不符帧数、每轮新增/更新数。

点表侧写 `ioa:1001`（`kind=register`，`offset` 即 IOA），**不写 `length`、布尔量也不写
位号**：远动协议的值宽度由 ASDU 类型标识决定，一个布尔量就是一个单点对象。这与
Modbus/S7 的习惯正好相反（那里 `float32` 要 2 个寄存器、`bool` 必须给 `.3`），所以点
表层用独立的 `WidthNone` 模式而不是复用寄存器那套推导——`ioa:1001:2` 与 `ioa:2001.3`
都会在 `uic import` 阶段就被拒绝，而不是等到网关起来后每个周期返回 `address_error`。

DNP3（IEEE 1815）调度主站（TCP 20000）：

```bash
./uic sim -protocol dnp3 -listen 127.0.0.1:20000 &   # 内置 DNP3 从站（链路地址 1）
./uic -config examples/dnp3.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8098/api/v1/points
#   13 点：二进制输入（类 1，每秒翻转）、双点 1/2、计数器（类 3，每秒自增）、
#          模拟量 42.5/7.25/-3.5/1234（类 2）、
#          二进制输出（CROB）+ 模拟量输出（组 41）（可写）
#   自检（不需要网关）：./uic dnp3read -addr 127.0.0.1:20000
#   只验链路层：./uic dnp3read -addr 127.0.0.1:20000 -probe
#   直接控制：./uic dnp3read -addr 127.0.0.1:20000 -binary 0=on -analog 0=12.5
```

DNP3 是三层协议：链路层（`05 64` 起始、每 16 字节一块 CRC、主站/从站各有自己的
链路地址）、传输层（应用报文分片，首尾字节带 FIR/FIN/序号）、应用层（功能码、对象头、
限定词、IIN）。三层任何一层错位都表现为「TCP 连上了但读不到值」，所以 `Open` 阶段
分三步走：复位链路（等链路 ACK）→ 探活（TEST_LINK）→ 用一条 READ 同时请求类
0/1/2/3 做首次总召唤。

与 IEC 104 一样，**数据由从站推上来**：`Read` 只查本地最新值表，不做网络交互。总召唤
（默认 5 分钟）刷新静态值，事件轮询（默认 1 秒）拉类 1/2/3。事件响应的应用层 CON 位
必须回一条确认，否则从站把事件留在待确认队列里反复重发，最终撑爆它的事件缓冲区——
这是现场「刚开始正常、几小时后突然全是旧值」最常见的原因。驱动在收到带 CON 的响应后
立即回确认，并在收到**非请求响应**（UNSOLICITED）时同样回确认。

质量映射只看低 6 位：`ONLINE=0` 或 `COMM_LOST=1` → bad；`RESTART`、强置、越限、
参考误差 → uncertain；其余 good。**二进制量的 bit7、双点量的 bit6-7 是过程状态而不是
品质位**，必须先屏蔽——否则所有处于「合」位的开关都会被判成 uncertain。

点位用 `kind=register` 寻址，`offset` 是对象索引，`area` 取 `binary-input` /
`double-bit-input` / `binary-output` / `counter` / `analog-input` / `analog-output`
（也接受 `bi`/`dbi`/`bo`/`ctr`/`ai`/`ao` 短别名）。双点量按 bool 映射时：1（分）→
false、2（合）→ true、0/3（中间态/不确定）→ false + uncertain，绝不把「未知」显示成
「已关闭」。写二进制输出用 CROB（组 12，锁存开/关），写模拟量输出用组 41；两者都等
从站的应用响应并检查回显状态，**只写 socket 不检查回显会把拒绝当成成功**。不做选择-
执行（SBO）两段式、不做安全认证、不做文件传输、不做时间同步写路径、不做串口。

`dnp3read` 同样不依赖点表：驱动旁路表按「区 + 索引」记录设备实际送过的每个对象
（组、变体、原始品质位、时标），初次接陌生设备时先看清「它到底送哪些对象」，再写点表。
带 `-raw` 时把组/变体与原始 flags 一起打出来，便于和生产方对账。

CoAP（UDP 资源模型，端口 5683）：

```bash
./uic sim -protocol coap -listen 127.0.0.1:5683 &   # 内置 CoAP 服务端
./uic -config examples/coap.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8099/api/v1/points
#   12 点：纯文本温度/湿度、JSON 文档字段（temperature/ok）、
#          可 PUT 的阀门与设定值、POST 触发器、bytes 资源、
#          3000 字节大资源（Block2 续读）、分离响应资源
#   自检（不需要网关）：./uic coapread -addr 127.0.0.1:5683 -discover
#   只验链路：./uic coapread -addr 127.0.0.1:5683 -probe
#   写设定值：./uic coapread -addr 127.0.0.1:5683 -path /actuator/setpoint -write 42
#   写 JSON 字段：./uic coapread -addr 127.0.0.1:5683 -path /config \
#                   -selector mode -type string -content-format json -write manual
```

CoAP 的数据模型是 **URI 资源**，不是寄存器：点位用 `kind=symbol` 寻址，`path` 是资源
路径（`/sensors/env`），`selector` 是 JSON 文档里的字段（`temperature`、`a.b`、
`a[0]`），`address.length` 限制 `string`/`bytes` 的接收字节数。同一个路径可以挂多个
点位（读不同字段），驱动按点位逐次 GET，不做响应缓存。

读取按响应里的 `Content-Format` 解码，不猜：`text/plain` 走与 MQTT 共用的文本规则，
`application/json` 按 selector 取字段后再按点位类型解码，`application/octet-stream`
只交给 `bytes`/`string`，CBOR 等其它格式明确报「不支持」。没有 `Content-Format`
时按纯文本处理。大资源用 Block2 分块：首个请求默认不带选项、由服务端决定块大小，
驱动按偏移逐块续读并校验「块号 × 块大小 == 已收字节数」；服务端对 Block2 回 `4.02`
时退回不带选项再取一次。重组上限 8 MiB，块数也有上限。

重传是 CoAP 的稳定性核心：`ackTimeout`（默认 2s）逐次翻倍，`maxRetransmit`（默认 4，
0 表示只发一次），且等待窗口永远不越过设备级 `timeout`。收到空 ACK 表示「请求已接受、
别重发」，此时只等分离响应；分离响应若是 CON 则回空 ACK。与本事务无关的 CON（迟到
响应或通知）回 RST 让对方停止重传，垃圾报文只计数不失败。

写入用 PUT（默认）或 POST：PUT 是**替换整个资源**，带 `selector` 时只发
`{selector: value}` 这棵子树；驱动刻意不做隐藏的读-改-写，数组下标写入直接拒绝
（否则会把其余元素抹成 null）。载荷格式由 `contentFormat` 选 text/json/octets，
`text` 与 `octets` 的宽度规则与读取一致。不做 DTLS、不做 Observe 订阅、不做 Block1
分块发送——能力声明里 `Subscribe` 是 `unsupported`，不把「一次性 GET」伪装成推送。

KNXnet/IP 隧道主站 + 内置网关（楼宇自控，UDP 3671）：

```bash
./uic sim -protocol knx -listen 127.0.0.1:13671 &   # 内置 KNX 网关
./uic -config examples/knx.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8099/api/v1/points
#   7 点：1/1/1 与 1/1/2 开关、2/1/1 温度 21.5、2/1/2 湿度 45、
#         3/1/1 调光 50.196、4/1/1 功率（每秒 +10）、5/1/1 有符号计数 8888
#   自检（不需要网关）：./uic knxread -list
#   只验隧道：./uic knxread -addr 127.0.0.1:13671 -probe
#   读温度：  ./uic knxread -addr 127.0.0.1:13671 -group 2/1/1 -dpt 9.001 -raw
#   开灯：    ./uic knxread -addr 127.0.0.1:13671 -group 1/1/1@1.001 -write on
```

KNX 与其它协议的根本差别是**组地址不携带类型**。同一个 `2/1/1` 在工程 A 里是 1 位
开关、在工程 B 里是 2 字节浮点温度、在工程 C 里是 4 字节浮点功率。所以驱动不做
「按平台类型猜 DPT」：点位地址必须显式声明 DPT（`"path": "2/1/1", "dpt": "9.001"`），
`Address.Canonical()` 把它拼进符号地址参与去重，`PolicyFor("knx")` 缺了它直接在配置
期拒绝（`uic import` 阶段就报，不是等网关起来后每个周期返回 `address_error`）。

这不是形式主义的严格，而是**猜错的代价是「合法但错误」的值**：2 字节半精度被当 4 字节
单精度解，得到 `3.7e-41` 这样的 float32——不报错、质量还是 good、画在曲线上是一条贴着
零轴的噪声。明确报「不支持 99.9」反而安全得多。同理 `DPT.Accepts` 在 `Open` 就拒掉
`9.001` 配 `bool`：把 21.5 塞进布尔不会报错，只会变成 `true`。

隧道语义有三处必须记住的细节。其一，**1 位 DPT 的值在 APCI 字节的 bit0、不在数据域**：
`0x0080` 表示 1.001 = 关、`0x0081` 表示 1.001 = 开；解析时若把这一位当成数据保留，
调用方按服务码 `switch` 会落空，报文被**静默丢弃**（症状是「写返回成功，设备没动」，
日志里什么都没有）。其二，**TPDU 长度不含 TPCI 字节与 APCI 高 2 位**：当成整个 APDU
的字节数，网关会把最后一个数据字节当成下一条报文的开头，症状同样是「写成功了但值不对」。
其三，序号是 8 位、每通道一个：**收到 TUNNELLING_ACK 之后才加一**；超时重传必须用
**同一个序号**（换号会被当新帧，同一次写执行两遍）；序号错乱时唯一可靠的复位是
**重建隧道**（通道号与序号是一体的，任何「+1 试试」都在猜）。因此整条会话用一把锁串行化
——一条隧道只有一个通道号 + 一个序号，总线 9600 波特，并发没有收益只有乱序风险。

写路径的 `L_Data.con` 有一个容易读反的语义：它的 error 位表示**总线级**故障（总线忙、
冲突、链路发送失败），**不是**「这个组地址有没有设备在听」。组地址是广播语义，写一个
没人订阅的地址在链路层是**成功**的。所以驱动在 300ms 短窗口里等 con：**收不到按成功**
（部分网关不给隧道客户端回 con，硬等会把一半的写判成失败），收到且 error 位置位才判
`protocol`。把这两件事混在一起，「写了但没生效」与「网络断了」就会变成同一个错误。

承载是 UDP 单播（TUNNELING 请求 + 组地址读写）。`Open` 只做 `net.Dial`，CONNECT 延迟
到首次 `Probe`/`Read`/`Write`——这样「网关隧道槽位满」这个几分钟后会自愈的状态不会被
判成驱动加载失败。`Probe` 定义为「建隧道 + CONNECTIONSTATE 往返」，**不读任何组地址**：
排障时经常需要把「网关连得上吗」与「工程组地址配得对吗」拆开看。心跳默认 60s
（限制 1s..60s），`ackTimeout` 默认 1s 且必须 ≤ `device.timeout`。

内置网关的对象表**只更新已存在的组地址**；读未知组地址时只回 ACK 与 `L_Data.con`、
不回 `GroupValueResponse`，让客户端正常超时——那个超时正是它该报告给运维的事实（给未知
地址凭空造对象，会让「点表地址写错」这个最常见的故障从测试里消失）。它刻意复现四件真机
会发生的事：序号严格校验（宽松一点，驱动里的重传路径永远测不到）、8 位回绕后「新帧 vs
重传」的边界、槽位满时回明确的 `StatusNoMoreConnections` 而不是静默丢弃、以及
`FailNextConfirms(n)` 显式注入总线级故障（默认 con 正常）。

DL/T 645-2007 多功能电能表（RS-485 / 串口服务器）：

```bash
./uic sim -protocol dlt645 -listen 127.0.0.1:14001 &   # 内置电能表从站（表地址 000000000042）
./uic -config examples/dlt645.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8093/api/v1/points
#   13 点：电能 123456.78 kWh、三相电压/电流、总有功、功率因数、频率、
#          最大需量、时钟（可写）、状态字、电压数据块（bytes）、
#          第 1 结算日（MMDDhhmm，可写）
#   自检（不需要网关）：./uic dlt645read -addr 127.0.0.1:14001 -address 42 -di 00000000,02010100
#   只验物理层：./uic dlt645read -port /dev/ttyUSB0 -baud 2400 -address 0042 -probe
#   写时钟：./uic dlt645read -addr 127.0.0.1:14001 -address 42 -di 04000101 -write "2026-11-07 12:00:00"
```

645 的地址是**数据标识（DI）**而不是寄存器偏移：`address.area` 写 8 位十六进制
（手册顺序 `DI3DI2DI1DI0`，如 `02010100` = A 相电压），`offset` 恒为 0，`length`
通常省略——宽度（电压 2 字节、电流 3 字节、电能 4 字节）是数据项的固有属性，由 DI 表
决定。点表层为此引入 `WidthProtocol`（宽度由协议定义，不由 `dataType` 推导）：
不在点表里复制一张 DI 表，也不让「长度与类型宽度不符」的告警在表计协议上刷屏。
数据块（DI 含 `FF`）只按 `bytes` 交付——把 15 个 4 字节项的串联硬解成 `float64`，
会给出一个看起来像读数、实际是前 8 字节的假值。

协议一次请求只能带一个 DI，所以驱动**逐点串行**、不做合并读：`MultiRead` 诚实地声明
`unsupported`。一轮采集的耗时等于「点数 × 单次事务」，300 个点位挂在 2400bps 总线上
要几十秒——这个事实必须让配点表的人看见，而不是被驱动藏起来。读到链路级故障（超时/
断开）后本轮剩余点快速失败，不再逐点等超时。数据块超长时驱动在同一次 `Read` 内跟完
后续帧（`0x12`，SEQ 1..255，上限 64 帧）：截断的电能读数会以「少了几位」的形式入账，
看不出任何异常。

写命令有四道闸门：`options` 含 `allow-write`（普通写）、`allow-control`（拉合闸/清零）、
DI 表标注可写、设备密码。它们是「与」关系——一个能读的驱动不应该顺手能把闸拉了。

CJ/T 188-2004 户用计量仪表（冷水/热水/燃气/热量表，RS-485 / 串口服务器）：

```bash
./uic sim -protocol cjt188 -listen 127.0.0.1:14001 &   # 内置冷水表从站（表地址 00000805000001）
./uic -config examples/cjt188.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8094/api/v1/points
#   8 点：当前累积流量 1234.56 m³、结算日流量、实时时间、状态字、
#          上 1 月/上 12 月结算日流量、阀门与表地址（后两个是写命令，默认不采集）
#   自检（不需要网关）：./uic cjt188read -addr 127.0.0.1:14001 \
#       -address 00000805000001 -type 10 -di 901F:0,901F:5,901F:10,901F:17
#   只验物理层：./uic cjt188read -port /dev/ttyUSB0 -baud 2400 -address 00000805000001 -probe
#   写阀门：./uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 \
#       -di A017 -write close -allow-control
```

188 与 645 都是「数据标识」寻址，但帧层完全不同：只有一个 `0x68`、数据域**不加
`0x33`**、DI 只有 2 字节（`area` 写 4 位十六进制，如 `901F`），**一个 DI 是一组
字段**。因此 `offset` 在这里是「DI 数据域内的字节偏移」而不是恒 0：水表 `901F`
的 0/5/10/17 分别是当前累积流量、结算日累积流量、实时时间、状态；热量表同一
`901F` 的布局完全不同（0/5/10/15/20/25/28/31/34/41），由 `labels.type`（10 水 /
20 热 / 30 气 / 40 其他）决定用哪套 schema。类型配错的症状是「读到了但量纲不对」，
所以驱动不猜类型、也不做跨 DI 合并。

一个 DI 内的多字段在一次 `Read` 里复用一次事务（四个字段只发一帧），跨 DI 仍然逐点
串行，`MultiRead` 诚实声明 `unsupported`。单位是数据域里的显式字节，解码后统一到
m³ / m³/h / GJ / kW；未知单位代号直接报错，不默认成基准单位（否则 L 与 m³ 的
1000 倍偏差会被静默掉）。写入口只有阀门（`A017`，`allow-control`）与表地址
（`A018`，`allow-write` + 完整 14 位）；写地址成功后驱动自动切到新地址，配置仍需
人工同步。188 的 `AA` 只属于读地址命令，因此表地址不支持缩位——带通配的读数据会
变成一次多表广播。
广播校时/广播冻结连入口都不提供：一条配错的广播会同时改掉总线上所有表的时间。

承载支持 `serial`（真实 RS-485，走共享串口层的总线级互斥）与 `tcp`（串口服务器/DTU
透传，锁退化为每连接互斥）。两种承载的分帧与命令序列完全相同，所以内置 TCP 从站能
直接当自检目标。

SNMP v1/v2c（交换机 / UPS / 服务器 / 带网管的仪表，UDP 161）：

```bash
./uic sim -protocol snmp -listen 127.0.0.1:16161 &   # 内置 SNMP v1/v2c 代理
./uic -config examples/snmp.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8095/api/v1/points
#   9 点：sysDescr/UpTime/Name/Services、可写 sysContact/sysLocation、
#          ifNumber、ifInOctets、ifAdminStatus
#   自检：./uic snmpread -addr 127.0.0.1:16161 -probe
#   遍历：./uic snmpread -addr 127.0.0.1:16161 -walk 1.3.6.1.2.1.2.2.1
#   写：  ./uic snmpread -addr 127.0.0.1:16161 -oid 1.3.6.1.2.1.1.6.0 \
#           -set lab-2 -setType string
```

SNMP 是唯一「按对象名寻址」的协议：点位用 `kind=symbol`，`path` 是点分十进制 OID，
实例号写在 OID 里（标量 `.0`、表项 `.10.1`）。读用一条 GET 带多个 varbind，
`MultiRead` 声明 `supported`；但一帧受 `maxOids`（默认 32）与 1400 字节编码预算双重
约束——超 MTU 会被分片，而现场设备/防火墙常直接丢分片。v1 没有 per-varbind 异常，
批量 GET 遇 `noSuchName` 时驱动逐点重发，否则一个坏 OID 会让同批所有点位都变地址错误。

只做 v1/v2c：v3 的 USM 认证加密是另一套子系统，配置期显式拒绝，不退回 v2c 碰运气。
写是单 OID SET（不合并 varbind，避免一条只读对象回滚整批）：`uint16/uint32`→Gauge32、
`int*`→INTEGER、`string/bytes`→OCTET STRING；float/BCD 没有标准线类型，显式拒绝。
应答同时校验错误码与回显值，设备「接受但改了值」会在写入当场暴露。Counter64 经
平台 `float64` 承载，超过 2^53 会丢精度；Counter32 会回绕，时间序列侧要做回绕检测。
团体名按凭据处理（界面掩码），默认 `public` 只是最少配置。

ADS/AMS（PLC 运行时，TCP 48898）：

```bash
./uic sim -protocol ads -listen 127.0.0.1:48898 &   # 内置 ADS/AMS 服务端
./uic -config examples/ads.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8096/api/v1/points
#   9 点：nCounter/fTemperature/bEnable/sRecipe/arr[2]（只读）、
#          setpoint/fSpeed/run/name（可写）
#   自检：./uic adsread -addr 127.0.0.1:48898 -probe
#   读：  ./uic adsread -addr 127.0.0.1:48898 \
#           -symbol MAIN.nCounter:int16,MAIN.sRecipe:string:81
#   写：  ./uic adsread -addr 127.0.0.1:48898 -symbol MAIN.nSetpoint:int16 -set 750
```

ADS 是唯一「地址就是 PLC 工程变量名、且一次取值要两次往返」的协议：先按名字取句柄
（`ReadWrite` + `0xF003`），再用句柄读值（`Read` + `0xF005`）。句柄属于会话，PLC 重启后
全部失效；驱动遇 `invalid offset` 清缓存重解析一次，第二次仍失败就按点位报错。地址
`kind=symbol`，数组下标写在名字里（`MAIN.arr[2]`）；string/bytes 的宽度由点表给出
（`NAME:81` 或 length 列）。全线小端，写前做类型/范围检查。

TCP 48898 与 AMS 端口是两层：`labels.netid` 是 6 段 NetId，`labels.port` 是 851/801；
`Open` 立即做 `ReadDeviceInfo` 握手。只做 AMS/TCP 符号读写：通知订阅、符号表浏览、跨
AMS 路由发现、多句柄合并读写都显式 `unsupported`，不做半成品。

SECS/GEM（半导体设备，HSMS/TCP 5000）：

```bash
./uic sim -protocol secs -listen 127.0.0.1:5000 &   # 内置 HSMS 设备
./uic -config examples/secs.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8100/api/v1/points
#   5 点：svid 1/2/3（U4 25、A "RUN"、F4 21.5，只读）、ecid 10/11（U4 300、A "AUTO"，可写）
#   自检：./uic secsread -addr 127.0.0.1:5000 -probe
#   看结构：./uic secsread -addr 127.0.0.1:5000 -describe -id 1,2,3
#   看原始项：./uic secsread -addr 127.0.0.1:5000 -raw -id 1,2,3
#   读值：  ./uic secsread -addr 127.0.0.1:5000 -id 3 -type float32
#   写常数：./uic secsread -addr 127.0.0.1:5000 -object ecid -id 10 -write 350
```

SECS/GEM 分两层：HSMS 管会话（Select 握手、SystemBytes 事务配对、Linktest 探活），
SECS-II 管消息体（格式字节 + 长度 + 内容的项序列）。点位用**对象地址**承载协议原生编号：
`svid:1001.value`（状态变量，S1F3 批量读，只读）、`ecid:200.value`（设备常数，
S2F13/S2F14 读、S2F15/S2F16 写，`ACKC7=0` 才算接受）。编号在标准里就是 32 位整数，
没有可寻址的符号名——名称要用 S1F11 单独查，那是给人看的。

读按对象类型分组：一个 span 里的 3 个 svid 点合成**一次** S1F3，svid + ecid 混排是
**两次**事务。写只认点表里声明过 `writable` 的 ecid，按地址临时构造的写请求一律
`permission`（读允许按地址解析，两者刻意不对称）。写 SVID 直接拒绝：状态变量没有写语义，
把 S1F3 的编号塞进 S2F15 轻则被拒、重则写到同号的另一个常数上。

线格式有四处会静默错位：控制消息的 Stream/Function 必须填 `0xFF`；格式字节低 2 位是
**长度字节数**（1..3，`00` 视同 1），不是长度值；L 项的长度是**子项个数**；整数定宽大端，
3 字节的 U4 是畸形而不是「补个 0」。12 字节帧头之外，数据长度字段设 16 MiB 上限
（`MaxPayload`）——TCP 上第一条读到的可能是别的服务回的垃圾，`0xFFFFFFFF` 照单分配就是
一次 OOM。应答配对要同时满足「同 SystemBytes + 同 Stream + Function=请求+1 + W 位清零」，
不匹配的消息（例如设备异步推的 S6F11）丢弃继续读，而不是让整次采集失败。

不做 S2F17/S2F31（时间）与 S2F33~S2F45（报告定义/链接）：那些改设备侧配置，配错会让
设备停止上报，属于调试工具。`Subscribe` 是 `unsupported`——能解析 S6F11 不等于能订阅。

FINS 只做 FINS/TCP；FINS/UDP 与 Host Link 配置期即拒。建链先做节点号协商（命令 0
→ 1），`device.unitId>0` 覆盖目标节点、0 用协商值。`offset` 是**字地址**、`bit` 是
字内位号（`CIO0.3` → `offset=0, bit=3`）；字访问 2 字节/字，位访问**1 字节/位**；
32 位值低字在前（默认 CDAB）、64 位 DCBA；当前 EM 区无位访问码，bool 走 RMW。

Mewtocol（松下 FP 系列，计算机链接 ASCII 帧，串口 / 串口服务器 TCP 2000）：

```bash
./uic sim -protocol mewtocol -listen 127.0.0.1:2000 &   # 内置 FP 从站（站号 1）
./uic -config examples/mewtocol.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8089/api/v1/points
#   8 点：DT0=0x0063、DT1=int16、DT2..DT3=25.5（float32，CDAB）、
#         X0.1/X0.5（输入接点）、Y0.0（可写，WCS 单点写）、
#         T10（定时器接点，不带位号）、SV5（定时器设定值）
#   自检（不需要网关）：./uic mewtocolread -addr 127.0.0.1:2000 -area dt -offset 0 -qty 3
#   只验链路：./uic mewtocolread -transport serial -port /dev/ttyUSB0 -baud 9600 -parity odd -probe
#   写接点：./uic mewtocolread -addr 127.0.0.1:2000 -area y -offset 0 -bit 0 -write on
```

Mewtocol 全 ASCII、没有二进制字段，抓包肉眼可读，看起来比 Modbus RTU 简单——真正难的
是那些「看起来是十进制」的位置。一帧里混着三种进制：

| 位置 | 进制 | 例子 |
|---|---|---|
| 站号 | 两位**十进制** | 16 号站写 `16`，不是 `10` |
| 数据区地址 | 五位**十进制** | DT100 → `00100` |
| 接点编号 | 三位十进制字 + 末位**十六进制** | X1F → `001F`；X16.3 → `0163` |
| 定时器/计数器接点 | 四位**十进制** | T10 → `0010` |
| 数据内容 | 每字节两位十六进制、**字内低字节在前** | H0063 → `6300` |
| BCC | 两位十六进制，**含帧头 `%`** | `%01#RCSX0000` → `1D` |

任何一种读错都不会报格式错：PLC 会回一份别的地址的数据，值有、质量好、对应关系全错。
所以点表里不让人写 `X1F`，而是写 `x:1.15`（字地址 + 位号两个十进制整数），把进制混排
关在编解码层里。

帧边界只有帧头的 `%` 与结尾 CR。半双工总线上一份迟到的应答如果被当成下一轮首部，
症状是「数据永远滞后一轮」，所以收帧层强制首字节是 `%`/`<`，其余字节逐字节丢弃并
在 `Dropped()` 里计数。

**读写受不同的帧长限制**：读请求定长 20 字节、应答 `9+4n`；写请求 `20+4n`。118 字符
上限下，读能到 27 字、写只能到 24 字，取较小者 `MaxWords = 24`。接点按点读写
（`RCP`/`WCP`）的上限是 8，来自正文「个数」只有一位字符，不能靠拆帧绕开。

三条刻意的边界：

- **布尔量只走 `WCS` 单点写。** 「读字 → 改位 → 写字」把一次写变成两次事务，中间别
  的客户端改了同字邻位，最后写入者会把它一起覆盖。协议给了原子单点写，就不该用非原子
  方式替代。
- **字区上的布尔点位可读不可写。** 协议没有「数据寄存器第 n 位」的写命令，驱动直接返
  `unsupported`，并给出替代做法（把点位改成 `uint16` 按整字写）。读是可以的：读字后本
  地取位。
- **T/C 接点不走按字读。** 手册允许 `RCC` 读 T/C，但会按「每字 16 个接点」重新编号
  （0000 表示 T0..T15），与 T/C 自身的十进制编号是两套体系；驱动只走 `RCS`/`RCP`。内
  置从站仍按真实语义实现按字读，避免「从站答应、真 PLC 不答应」。

写路径按对象分流：接点布尔量 `WCS`/`WCP`，接点区按字 `WCC`，字区 `WD`，定时器设定值
`WS`，经过值 `WK`。`WD` 的正文也带结束地址（冗余，但必须与数据长度自洽，否则回 41H），
本实现由数据长度反推结束地址，两者不可能不一致。

承载有两种：串口（出厂 9600/8/**奇**/1，改成无校验的典型症状是「能发不能收」）与串口服
务器 TCP 透传（端口各厂不同：2000、60001、9094、FP7 编程口 32769..32772，默认 2000）。
TCP 承载**只发裸帧**：FP7 的 LAN 口若设成「与 FP2-ET-LAN 连接」，帧外会多一层 16 字节
头，表现是「发出去没有任何应答」；正确做法是在 PLC 侧改回默认模式，而不是给驱动加一
个「猜封装」的开关。

错误码保留手册的四类（链路 / 基本过程 / 处理 / PLC 应用），再映射到驱动的错误分类：
`60/61/66/67` → `address`（点表或型号配置问题，重试无用）、`65` 写保护与 `63` 模式错 →
`permission`、`40..43`/`53` → `protocol`（可重试）、`23` 站号重号 / `24` 硬件错 / `26`
站号设置错不可重试。质量码只由错误分类与链路状态决定，不做「多久没更新就降级」。

Host Link FINS（欧姆龙上位链接，头码 `FA`，串口 / 串口服务器 TCP 2000）：

```bash
./uic sim -protocol hostlink -listen 127.0.0.1:2000 &  # 内置上位链接从站（单元号 0）
./uic -config examples/hostlink.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8090/api/v1/points
#   6 点：DM0/DM1 字、DM2..DM3=3.14（float32，CDAB）、CIO0.1 运行位、
#         CIO0.5 可写故障位、DM10 可写计数器
#   自检（不需要网关）：./uic hostlinkread -addr 127.0.0.1:2000 -area dm -offset 0 -qty 4
#   只验链路：./uic hostlinkread -transport serial -port /dev/ttyUSB0 \
#             -baud 9600 -dataBits 7 -parity even -stopBits 2 -probe
#   打印单次上限：./uic hostlinkread -limits
```

Host Link 不是另一种协议，而是 **FINS 命令换了一层 ASCII 信封**：内存区、字/位寻址、
读写语义、数值字节序与 FINS/TCP 逐字节相同，差别只在「怎么把一帧发到线上」。所以
`codec/hostlink` 只做「FINS 帧 ↔ Host Link 信封」的转换，命令语义由 `driver/finscore`
与 `sim/finsmem` 与 FINS/TCP 共用——同一份点表换个 `protocol`（`fins` ↔ `hostlink`）就能跑。

帧的形状要按字节记住六件事：

| 细节 | 正确 | 记错的症状 |
|---|---|---|
| 等待时间宽度 | `FA` 后**一位**十六进制（0..F，单位 10ms） | 写成两位，PLC 把第二位当 ICF 高位，整帧错位 |
| 收发对称性 | 应答 `FA` 后**多一格结束码**、少一格等待时间 | 按请求布局解应答，后面每个字段错位两格 |
| FCS 范围 | 从 `@` 起异或到正文最后一个字符 | 少算 `@`：本地回环永远通过，真机必挂 |
| 正文编码 | 一字节两字符的十六进制（全区可打印 ASCII） | 当成二进制发，PLC 回 0x14 |
| 单元号进制 | 两位**十进制**（00..31） | 当成十六进制，31 号站写成 `1F` |
| 直连 ICF | `0x00`（应答 `0x40`） | 照抄 FINS/TCP 的 `0x80`，PLC 按跨网解、找不到路由 |

黄金字节（实现直接打印，单元号 0、等待 0）：

```
读 DM0..DM1   @00FA00000000001018200000000027F*\r       (34)
读应答         @00FA004000000001010000123456784B*\r       (35)
写 DM0=1234   @00FA000000000010282000000000112347B*\r     (38)
```

**单次上限是读 269 字、写 267 字**，不是 FINS/TCP 的 999。Host Link 的 FINS 命令不能
在信封上分段，所以上限由整帧字符预算反推：读受应答帧长约束（数据 1076 字符 / 4），写
受命令帧长约束（正文 1080 字符减 12 字符参数再 / 4）。读写相差两个字是结构性的，不是
随便统一的数字；照抄 999 会稳定收到结束码 0x18（帧长错）。

**串口出厂参数是 9600-7-E-2**，与 Mewtocol 的 9600-8-O-1 正好相反。整帧是可打印
ASCII，7 位数据位完全够用——「FINS 必须 8 位数据位」是错的。把 FINS/TCP 用户的 8/N/1
套过来会「发得出去、收不到东西」，而且不报错。

两条刻意的边界：

- **只做 FINS 帧（`FA`），不做 C 模式。** C 模式（`RR`/`RD`/`WD`…）是另一套命令集与
  寻址（BCD「通道号 + 位号」），没有区码表也没有 FINS 结束码，混进来每个字段都要先
  判断「现在走哪套」。
- **不做跨网长格式，也不自动重试。** 跨网格式多 12 个路由字段，目标由单元号承载；
  自动重发一次写可能把同一个字写两遍（没有可靠事务号排出迟到应答的歧义），对「加
  计数」点位是真实数据错误。重连交给调度器。

错误信息分两层：信封层结束码（`0x13` FCS 错 / `0x14` 格式错 / `0x15` 数据错 / `0x18`
帧长错，修一次永久消失）与 FINS 层结束码（越界、写保护等，与 FINS/TCP 同一张表）。

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
| `cmd/uic/` | 单入口：`run` / `sim`（Modbus / S7 / OPC UA / MQTT / MC / FINS / Host Link / KNX / EtherNet-IP / BACnet-IP / IEC 104 / IEC 101 / Mewtocol / DNP3 / CoAP / 645 / 188 / SNMP / ADS / SECS）/ `mbread` / `mcread` / `finsread` / `hostlinkread` / `enipread` / `bacread` / `iec104read` / `iec101read` / `mewtocolread` / `dnp3read` / `coapread` / `knxread` / `dlt645read` / `cjt188read` / `snmpread` / `adsread` / `secsread` / `uaread` / `s7blocks` / `s7upload` / `instantiate` / `import` / `token` / `-check` / `version` |
| `internal/model` | 通道 / 设备 / 点位 / 地址 / 采样 / 质量码 |
| `internal/driver` | 驱动契约（Codec / Transport / Driver）、错误分类、注册表、只读 `Lookup` 注入 |
| `internal/calc` | 零依赖表达式引擎（编译期类型检查、短路求值、引用 `device:point`） |
| `internal/codec/mc` | 三菱 MC 3E 纯函数编解码（帧头、设备码表、位打包、结束码、24 位设备号） |
| `internal/driver/mc` | MC 3E 主站驱动（字/位读写、字软元件 bool 的 RMW、字序归一化、结束码分类） |
| `internal/sim/mc` | MC 3E 内置从站（内存字/位表、按区读写、越界/未知设备结束码） |
| `internal/codec/fins` | 欧姆龙 FINS 纯函数编解码（FINS/TCP 封装、节点协商、内存区码表、字/位读写、结束码） |
| `internal/driver/fins` | FINS/TCP 主站驱动（节点号协商、字/位读写、RMW 位写、低字在前字序、结束码分类、SID 校验） |
| `internal/sim/fins` | FINS/TCP 内置从站（节点分配、字内存表同时承载字/位视图、结束码） |
| `internal/driver/finscore` | 与 FINS/TCP 共享的主站会话（区拆分、字/位读、按 `Limits` 切块、结束码分类；Host Link 通过它复用，不复制） |
| `internal/sim/finsmem` | 与 FINS/TCP 共享的从站内存表（字视图 + 位视图、EM bank、`Respond`） |
| `internal/codec/hostlink` | Host Link（上位链接 FINS）纯函数编解码（ASCII 信封 `FA` / FCS 异或含 `@` / 收发两条布局 / 单元号十进制 / 单次上限 269·267 / 结束码分层 / 流式收帧） |
| `internal/driver/hostlink` | Host Link 主站驱动（`FromFINS` 套信封、单元号·等待时间·ICF 收校验、结束码分层、串口 7-E-2 缺省、超时排查顺序、不自动重试） |
| `internal/sim/hostlink` | Host Link 内置从站（逐字节扫帧、`*`+CR 定界、单元号过滤沉默、FCS 错 0x13 / 格式错 0x14 / 头码错 0x16） |
| `internal/codec/enip` | EtherNet/IP 纯函数编解码（封装头 / CPF / SendRRData / CIP 请求应答 / 符号路径 / Read/Write Tag / 多服务合包 / 类型表） |
| `internal/driver/enip` | CIP 显式报文主站（标签读写、数组 offset、STRING、bytes 两阶段读、位 RMW、多服务合包与自动退回） |
| `internal/sim/enip` | EtherNet/IP 内置从站（RegisterSession / Read/Write Tag / 多服务合包 / 默认标签表） |
| `internal/codec/bacnet` | BACnet/IP 纯函数编解码（BVLC / NPDU / APDU / 标签 / 对象属性名表 / Read-WriteProperty） |
| `internal/driver/bacnet` | BACnet/IP 主站（对象属性读写、UDP invoke ID 配对、按对象语义选写标签、错误类映射） |
| `internal/sim/bacnet` | BACnet/IP 内置从站（对象表 / 属性读写 / 数组下标 / 写权限 / 类型收敛） |
| `internal/codec/iec60870` | IEC 60870-5 共享应用层（ASDU 头与元素 / 剖面 `Profile` / `HasIOA` / CP56Time2a / 品质位 / 请求构造 / 点位语义：对象类别、值转换、命令构造 / 类型与原因名表） |
| `internal/codec/iec104` | IEC 60870-5-104 纯函数编解码（APCI 三种帧型、序号与 k/w）+ 对共享应用层的别名转发 |
| `internal/driver/iec104` | IEC 104 主站（STARTDT 建链、站总召唤、电能量召唤、按点位 ID 的最新值表、五种命令、否定确认分类、旁路表） |
| `internal/sim/iec104` | IEC 104 内置从站（对象表 / STARTDT 门控 / 召唤快照 / 命令三段式 / 突发上送） |
| `internal/codec/iec101` | IEC 60870-5-101 纯函数帧层（FT1.2 固定帧 / 可变帧 / 单字节帧、控制域 FCB/FCV/ACD/DFC、校验和、流式收帧 `Reader`） |
| `internal/driver/iec101` | IEC 101 非平衡轮询主站（复位链路、第 1/2 级轮询、站总召唤 + 电能量召唤、FCB 原帧重传、命令两段确认、双承载、旁路表） |
| `internal/sim/iec101` | IEC 101 内置从站（FT1.2 over TCP 透传、FCB 去重、ACD、两级队列、召唤快照与终止、命令两段确认） |
| `internal/codec/dnp3` | DNP3 纯函数编解码（链路帧与 CRC / 流式收帧 / 传输分片重组 / 应用层请求响应 / 对象解析 / 请求头 / CROB 与组 41 构造） |
| `internal/driver/dnp3` | DNP3 主站（复位链路 + TEST_LINK、类 0/1/2/3 轮询、未请求响应、应用层确认、CROB 与模拟量直接控制、旁路表） |
| `internal/sim/dnp3` | DNP3 内置从站（链路 ACK / 类 0 静态快照 / 三类事件队列与 inflight / CROB 与组 41 直接控制回显） |
| `internal/codec/coap` | CoAP 纯函数编解码（4 字节头 / 选项 delta-len 扩展 / uint 与 Block 编解码 / URI 拆分 / link-format 解析） |
| `internal/codec/textval` | 文本/JSON 标量 → 平台值（MQTT 与 CoAP 共用的类型规则；selector 取字段） |
| `internal/driver/coap` | CoAP 主站（GET/PUT/POST、Block2 续读与降级、分离响应、指数重传、资源发现、坏包跳过） |
| `internal/sim/coap` | CoAP 内置服务端（CON/NON / MID 去重 / Block2 / 分离响应 / RST / link-format 发现） |
| `internal/codec/knx` | KNXnet/IP 纯函数编解码（组地址 4/4/8 与物理地址 4/4/8 / DPT 切片表与编解码 / cEMI `L_Data.req/ind/con` / 信封与全部服务报文 / HPAI） |
| `internal/driver/knx` | KNXnet/IP 隧道主站（建隧道/心跳/断链重建、8 位序号与重传、组地址读写、`L_Data.con` 短窗口确认、源地址三级优先） |
| `internal/sim/knx` | KNXnet/IP 内置网关（隧道分配与回收、序号严格校验与 8 位回绕、组地址对象表、`L_Data.con`、四种故障注入） |
| `internal/codec/dlt645` | 645-2007 纯函数编解码（帧/校验和 / 数据标识 DI 表 / 压缩 BCD 与小数位 / 时间与地址 / 收帧扫描 / 后续帧序号） |
| `internal/driver/dlt645` | 645 主站（RS-485 与串口服务器双承载、总线锁、逐点读 + 后续帧、写权限四道闸门、错误分类） |
| `internal/sim/dlt645` | 645 内置从站（按标准宽度回数据 / 地址过滤 / 后续帧拆分 / 写权限与密码 / 广播校时静默生效 / 拉合闸） |
| `internal/codec/cjt188` | 188-2004 纯函数编解码（单 `68` / 7 字节 BCD 地址 / 无 `0x33` / 2 字节 DI / 按表类型分派的字段 schema / 单位代号 / 7 字节时间 / 收帧扫描） |
| `internal/driver/cjt188` | 188 主站（RS-485 与串口服务器双承载、总线锁、按 DI 缓存字段、阀门/写地址两道闸门、错误分类） |
| `internal/sim/cjt188` | 188 内置从站（水/热两套 901F / D120..D12B / 读地址 / 阀门写 / 写地址 / 地址过滤） |
| `internal/codec/snmp` | SNMP v1/v2c 纯函数编解码（BER TLV / 整数最短补码 / OID base-128 / 7 类 PDU / varbind / 异常标记 / error-status 分类） |
| `internal/driver/snmp` | SNMP v1/v2c 主站（批量 GET / v1 `noSuchName` 退化 / 单 OID SET / 类型映射与回显校验 / request-id 配对 / 自检 `GetRaw`、`Walk`） |
| `internal/sim/snmp` | SNMP v1/v2c 内置代理（system 组 + 两块网卡 / GET、GETNEXT、GETBULK、SET / community 静默丢弃 / v1 与 v2c 差异 / SET 原子性） |
| `internal/codec/ads` | ADS/AMS 纯函数编解码（6 字节 AMS/TCP 头 / 32 字节 AMS 头 / 7 类命令 / 两层长度一致性 / NetId 解析 / 错误码分类） |
| `internal/driver/ads` | ADS/AMS 主站（符号名→句柄→值 / 句柄缓存与失效重解析 / 逐点事务 / 类型范围检查 / AMS 与 ADS 错误分类 / 自检 `DeviceInfo`、`DeviceState`、`WriteControl`） |
| `internal/sim/ads` | ADS/AMS 内置服务端（默认变量表 / 按名分配句柄 / 只读标记 / 宽度校验 / 状态控制 / 句柄失效模拟） |
| `internal/codec/secs` | SECS 纯函数编解码（12 字节 HSMS 帧头 / 控制消息 / SECS-II 项编解码 / `MaxPayload` 上限 / SxFy 消息体语义） |
| `internal/driver/secs` | HSMS 主站（Select 握手、SystemBytes 配对、按对象分组批量读、S2F15 写常数、Linktest 探活、自检 `Describe`/`RawRead`） |
| `internal/sim/secs` | HSMS 内置设备（SVID/ECID 双编号空间 / Select 门控 / 常数写拒绝未知编号 / 事件与报警推送） |
| `internal/codec/mewtocol` | Mewtocol 纯函数编解码（ASCII 帧 / BCC 含 `%` / 站号十进制 / 接点编号「十进制字 + 十六进制位」/ RD/WD/RCC/WCC/RCS/RCP/WCS/WCP/RS/WS/RK/WK 组帧 / 字内低字节在前 / 错误码四类 / 流式收帧） |
| `internal/driver/mewtocol` | Mewtocol 主站（区拆分与 24 字切分、T/C 按点读 RCS/RCP、布尔量 WCS 单点写、字区布尔量拒绝、站号与回显校验、串口/TCP 双承载） |
| `internal/sim/mewtocol` | Mewtocol 内置从站（站位图 / 两套接点编号 / RCC 按字与 RCS/RCP 按点 / WCS/WCP / 错误码 / 站号过滤） |
| `internal/template` | 设备模板：参数校验 / 字符串字面量替换 / 实例化后完整校验 |
| `internal/pointtable` | 点表 CSV：列别名 / 地址表达式 / 按协议推导宽度 / 合并动作 |
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
| `internal/config` | 配置校验 / 内容哈希版本 / 快照 / 回滚 / 原子写 |
| `internal/obs` | 指标 / 结构化日志 / 日志环形缓冲 |
| `internal/rbac` | 角色 / 权限矩阵（viewer / operator / admin，按动作授权） |
| `internal/auth` | 凭据表：Token → 身份；`Reloadable` 支持热替换；无凭据时 open 模式 |
| `internal/audit` | 审计存储：NDJSON 分段 + SHA-256 哈希链 + 查询 / 校验 |
| `internal/northbound/httpapi` | REST + SSE + 认证授权 + 错误码 |
| `internal/northbound/opcua` | OPC UA 北向：点位表 → 地址空间，质量码映射，写穿透到南向 |
| `internal/codec/sparkplug` | Sparkplug B 载荷的手写 Protobuf 编解码（Metric oneof / PropertySet / 未知字段跳过 / 确定性编码） |
| `internal/sparkplug` | Sparkplug B 生命周期状态机（主题 / BIRTH 与 DEATH / 序号作用域与回绕 / bdSeq / Rebirth 命令 / 指标映射） |
| `internal/mqtt` | 自写 MQTT 3.1.1 客户端（北向发布 + 南向订阅共用；QoS0/1、retained、LWT、PING 往返探测、连接代数 `Epoch`） |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 |
| `internal/runtime` | 唯一编排层 + 看门狗 + 后端实现（含各 sink 装配） |

## 许可

待定（`docs/04-decisions/` 补充）。
