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

FINS 只做 FINS/TCP；FINS/UDP 与 Host Link 配置期即拒。建链先做节点号协商（命令 0
→ 1），`device.unitId>0` 覆盖目标节点、0 用协商值。`offset` 是**字地址**、`bit` 是
字内位号（`CIO0.3` → `offset=0, bit=3`）；字访问 2 字节/字，位访问**1 字节/位**；
32 位值低字在前（默认 CDAB）、64 位 DCBA；当前 EM 区无位访问码，bool 走 RMW。

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
| `cmd/uic/` | 单入口：`run` / `sim`（Modbus / S7 / OPC UA / MQTT / MC / FINS / EtherNet-IP / BACnet-IP / IEC 104 / 645 / 188 / SNMP）/ `mbread` / `mcread` / `finsread` / `enipread` / `bacread` / `iec104read` / `dlt645read` / `cjt188read` / `snmpread` / `uaread` / `s7blocks` / `s7upload` / `instantiate` / `import` / `token` / `-check` / `version` |
| `internal/model` | 通道 / 设备 / 点位 / 地址 / 采样 / 质量码 |
| `internal/driver` | 驱动契约（Codec / Transport / Driver）、错误分类、注册表、只读 `Lookup` 注入 |
| `internal/calc` | 零依赖表达式引擎（编译期类型检查、短路求值、引用 `device:point`） |
| `internal/codec/mc` | 三菱 MC 3E 纯函数编解码（帧头、设备码表、位打包、结束码、24 位设备号） |
| `internal/driver/mc` | MC 3E 主站驱动（字/位读写、字软元件 bool 的 RMW、字序归一化、结束码分类） |
| `internal/sim/mc` | MC 3E 内置从站（内存字/位表、按区读写、越界/未知设备结束码） |
| `internal/codec/fins` | 欧姆龙 FINS 纯函数编解码（FINS/TCP 封装、节点协商、内存区码表、字/位读写、结束码） |
| `internal/driver/fins` | FINS/TCP 主站驱动（节点号协商、字/位读写、RMW 位写、低字在前字序、结束码分类、SID 校验） |
| `internal/sim/fins` | FINS/TCP 内置从站（节点分配、字内存表同时承载字/位视图、结束码） |
| `internal/codec/enip` | EtherNet/IP 纯函数编解码（封装头 / CPF / SendRRData / CIP 请求应答 / 符号路径 / Read/Write Tag / 多服务合包 / 类型表） |
| `internal/driver/enip` | CIP 显式报文主站（标签读写、数组 offset、STRING、bytes 两阶段读、位 RMW、多服务合包与自动退回） |
| `internal/sim/enip` | EtherNet/IP 内置从站（RegisterSession / Read/Write Tag / 多服务合包 / 默认标签表） |
| `internal/codec/bacnet` | BACnet/IP 纯函数编解码（BVLC / NPDU / APDU / 标签 / 对象属性名表 / Read-WriteProperty） |
| `internal/driver/bacnet` | BACnet/IP 主站（对象属性读写、UDP invoke ID 配对、按对象语义选写标签、错误类映射） |
| `internal/sim/bacnet` | BACnet/IP 内置从站（对象表 / 属性读写 / 数组下标 / 写权限 / 类型收敛） |
| `internal/codec/iec104` | IEC 60870-5-104 纯函数编解码（APCI 三种帧型 / ASDU / 元素宽度 / CP56Time2a / 品质位 / 命令构造 / 类型与原因名表） |
| `internal/driver/iec104` | IEC 104 主站（STARTDT 建链、站总召唤、电能量召唤、最新值表、五种命令、否定确认分类、旁路表） |
| `internal/sim/iec104` | IEC 104 内置从站（对象表 / STARTDT 门控 / 召唤快照 / 命令三段式 / 突发上送） |
| `internal/codec/dlt645` | 645-2007 纯函数编解码（帧/校验和 / 数据标识 DI 表 / 压缩 BCD 与小数位 / 时间与地址 / 收帧扫描 / 后续帧序号） |
| `internal/driver/dlt645` | 645 主站（RS-485 与串口服务器双承载、总线锁、逐点读 + 后续帧、写权限四道闸门、错误分类） |
| `internal/sim/dlt645` | 645 内置从站（按标准宽度回数据 / 地址过滤 / 后续帧拆分 / 写权限与密码 / 广播校时静默生效 / 拉合闸） |
| `internal/codec/cjt188` | 188-2004 纯函数编解码（单 `68` / 7 字节 BCD 地址 / 无 `0x33` / 2 字节 DI / 按表类型分派的字段 schema / 单位代号 / 7 字节时间 / 收帧扫描） |
| `internal/driver/cjt188` | 188 主站（RS-485 与串口服务器双承载、总线锁、按 DI 缓存字段、阀门/写地址两道闸门、错误分类） |
| `internal/sim/cjt188` | 188 内置从站（水/热两套 901F / D120..D12B / 读地址 / 阀门写 / 写地址 / 地址过滤） |
| `internal/codec/snmp` | SNMP v1/v2c 纯函数编解码（BER TLV / 整数最短补码 / OID base-128 / 7 类 PDU / varbind / 异常标记 / error-status 分类） |
| `internal/driver/snmp` | SNMP v1/v2c 主站（批量 GET / v1 `noSuchName` 退化 / 单 OID SET / 类型映射与回显校验 / request-id 配对 / 自检 `GetRaw`、`Walk`） |
| `internal/sim/snmp` | SNMP v1/v2c 内置代理（system 组 + 两块网卡 / GET、GETNEXT、GETBULK、SET / community 静默丢弃 / v1 与 v2c 差异 / SET 原子性） |
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
| `internal/mqtt` | 自写 MQTT 3.1.1 客户端（北向发布 + 南向订阅共用；QoS0/1、retained、LWT、PING 往返探测） |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 |
| `internal/runtime` | 唯一编排层 + 看门狗 + 后端实现（含各 sink 装配） |

## 许可

待定（`docs/04-decisions/` 补充）。
