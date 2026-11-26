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

DB STRING 的 2 字节长度头：西门子 DB 里的 `STRING[n]` 占 `2+n` 字节（头两字节是
声明的最大长度与当前实际长度）。驱动 `string` 类型按这个语义读写：`address.length`
是**含头**的总宽，写入自动带 `[max, cur]` 头，读取剥头后按 `cur` 取字符——裸字节
读写（不带头）用 `bytes` 类型。内置从站的 DB 是裸内存，用 `bytes` 点位即可自检
任意字节布局。

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

寄存器位写支持 **FC22 掩码写**：Modbus 保持寄存器里「只改一位」如果按
读-改-写（FC3 + FC6）做，两步之间南向设备自己改过的其它位会被写回旧值覆盖。
FC22 让设备侧一次算出 `(当前 AND AND_Mask) OR (OR_Mask AND NOT AND_Mask)`，
不存在这个窗口。主站默认自动探测（设备回 `0x01` 就在本次连接内永久退回
读-改-写），用 `device.labels.options=maskwrite=on|off|auto` 锁定策略；
网关从站也实现了 FC22，掩码基于最近一次采样的缓存值计算，只作用于单寄存器
的可写映射（32 位点位仍拒绝 `0x02`）。

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
false（只有 STATE 是 retained）。序号是节点级的（节点消息与全部设备消息共用
一个 0..255 计数器，NBIRTH 为 0 并重置），NDEATH 不带序号；重连、NCMD
`Node Control/Rebirth`、设备故障恢复都会重发 BIRTH。质量非 good 用
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

EtherNet/IP 只做 **CIP 显式报文 + 符号标签**（Read/Write Tag + Fragmented），不做隐式 I/O（UDP 2222）、
不做对象/属性寻址、不做结构体/数组自动展开。`address.area` 是标签名（大小写不敏感）、
`offset` 是数组起始元素、`length` 是 bytes 字节数/string 字符容量、`bit` 是整数标签内的
位号（-1 表示标签本身是 BOOL）。数值线上小端；同宽有符号/无符号可互换，类型以应答里的
实际类型为准；bytes 两阶段读（先学宽度再按区间重读）；一轮多条读走 Multiple Service
Packet，设备不支持时自动退回逐条请求。单帧放不下的大标签（长数组、大 STRING、字节块）
经 Forward Open 建立的 Class 3 连接走 Read/Write Tag Fragmented（0x52/0x53）分片读写，
对调用方透明；总量上限 1 MiB，超过按地址错拒绝。

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

BACnet MS/TP 对象属性（RS-485 串行总线，单主站轮询从站）：

```bash
./uic sim -protocol bacnet-mstp -listen 127.0.0.1:24001 &   # 内置 MS/TP 从站（MAC 128，TCP 透传）
./uic -config examples/bacnet-mstp.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8103/api/v1/points
#   7 点：analog-input:1 的 present-value/object-name/units、binary-input:1、
#         analog-value:2 的 present-value、analog-value:3 每秒自增、character-string:1
#   自检（不需要网关）：
./uic mstpread -transport serial -port /dev/ttyUSB0 -baud 38400 -mac 128 -probe
./uic mstpread -addr 127.0.0.1:24001 -mac 128 -type analog-input -instance 1
./uic mstpread -addr 127.0.0.1:24001 -mac 128 -type analog-value -instance 2 -write 55.5
```

MS/TP 与 BACnet/IP 共用同一套 NPDU/APDU、对象属性寻址（`analog-input:1.present-value`）
与 ReadProperty（0x0C）/ WriteProperty（0x0F）语义，只换了承载层：IP 上是 UDP 数据报，
MS/TP 上是加了两条 CRC 的串行帧。分帧为
`55 FF | type | dest | src | len(2,大端) | hdrCRC | data | dataCRC(2,小端)`，
头 CRC 用标准给出的特殊移位展开（有效帧残差 `0x55`），数据 CRC 是 CRC-16/X-25
（残差 `0xF0B8`）；同一帧里长度大端、数据 CRC 小端，是标准原文如此，也是二手实现
最容易抄错的地方。

**定位是「单一主站轮询从站」**：本机占主站地址段 0..127（默认 0），被采集设备是
MAC 128..254 的从站（`labels.address`，回退 `device.unitId`）。主站用
DataExpectingReply 点名问、从站回 DataNotExpectingReply，令牌不发生转移——从站不持有
令牌。驱动**刻意不做**多主站令牌环的建立与维护（Tslot 抢占、NextStation 探活、
Tno_token 等毫秒级定时器在 TCP 透传/串口服务器上都不可靠）、不做 Who-Is 发现、不做
COBS 扩展帧、不做波特率自适应、不做 ReplyPostponed 的后续上送（直接映射为协议错误，
让上层重试）、不轮询主站设备。**空闲时不监听总线**是 sole-master 拓扑的明确前提，不是
遗漏；现场若确有第二个主站，应换驱动而不是靠 `labels.nextStation` 勉强维持令牌环。

在等待从站应答的窗口内，驱动会**被动配合**令牌环：收到发给本机的 Token 就记住并在事务
结束后交还（优先 `labels.nextStation`，否则按 PollForMaster 从本机地址之后现场探测下一站），
收到 PollForMaster/TestRequest 立即应答。每次发送前留 `labels.presilence`（默认 5ms）
线路静默，给 RS-485 收发器方向切换与 `Tturnaround` 留时间；过短会在长线上表现为
「随机首字节丢失 → 整帧被当噪声 → 随机超时」。串口按物理路径共享、事务由总线锁串行，
`concurrency` 必须为 1。

IEC 60870-5-104 调度主站（TCP 2404）：

```bash
./uic sim -protocol iec104 -listen 127.0.0.1:2404 &   # 内置 IEC 104 从站
./uic -config examples/iec104.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8091/api/v1/points
#   17 点：短浮点 42.5/7.25、标度化 1234、被取代值（uncertain）、带时标值、
#         单点 on/off、双点 1/2、累计量每秒自增、步位置/位串、
#         单点命令 + 短浮点/标度化设定值 + 双点遥控 + 调节步命令（可写）
#   自检（不需要网关）：./uic iec104read -addr 127.0.0.1:2404 -ca 1
#   只验链路层：./uic iec104read -addr 127.0.0.1:2404 -probe
#   下发命令：./uic iec104read -addr 127.0.0.1:2404 -area double-command -cmd-ioa 2003 -write off
```

IEC 104 与其它协议的根本差别是**数据由从站推上来**：建链（STARTDT）后做一次站总召唤
拿快照，之后设备按变化主动上送，没有「读第 1001 号点」这种动作。所以驱动的 `Read`
**不做任何网络交互**，只查本地最新值表（与 MQTT 南向同一模式）。累计量不在站召唤范围
内，`Open` 里固定再发一次电能量召唤；设备不支持时回否定确认，按调试日志处理。电能量
应答可能是 `M_IT_NA_1`（类型 15）或带时标的 `M_IT_TB_1`（类型 37，BCR + CP56Time2a），
两者都按累计量解码，时标只进旁路表、不参与点位映射。监视类型另支持步位置 `M_ST_NA_1`
（类型 5，档位 0..127 + 暂态位）与 32 位位串 `M_BO_NA_1`（类型 7，bit0 在最低字节、无品质）。

质量码只来自两处：ASDU 品质位（`IV`→bad、`NT`→stale、`SB`/`BL`/`OV`→uncertain）与
链路是否活着（TESTFR 往返）。**刻意不做「多久没更新就降级」**——104 只在变化时上送，
一个温度半小时不变是正常的，按时间降级会让整页看起来都不新鲜。

点位用 `kind=register` 寻址，`offset` 即 IOA（1..16777215），`area` 取 `ioa` /
`single-command` / `double-command` / `step-command` / `setpoint-float` /
`setpoint-scaled` / `setpoint-normalized`。命令类型由 `area` 声明，不由 `dataType`
推导：同一个 `float32` 可能是短浮点设定值，也可能是放大 100 倍的标度化整数。双点命令
只接受 1（分）/2（合），调节步命令只接受 1（降档）/2（升档），0 与 3 是标准里的
「不允许」，本地即拒。同一 IOA 可挂两个点位（读状态 + 写命令）。

`iec104read` 不依赖点表：驱动旁路表按 IOA 记录设备实际送过的原始对象（类型标识、
传送原因、时标），初次接陌生设备时先看清「它送什么」再写点表。双点的 0/3 中间态
原样显示成「不确定 (3)」，不硬转成布尔。

IEC 60870-5-101 串行远动主站（FT1.2，串口 / 串口服务器）：

```bash
./uic sim -protocol iec101 -listen 127.0.0.1:4001 &   # 内置 101 从站（FT1.2 over TCP 透传）
./uic -config examples/iec101.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8092/api/v1/points
#   15 点：短浮点 42.5/7.25、标度化 1234、被取代值（uncertain）、带时标值、
#         单点 on/off、双点 1/2、累计量每秒自增（需电能量召唤）、
#         单点命令 + 短浮点/标度化设定值 + 双点遥控 + 调节步命令（可写）
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
#   14 点：二进制输入（类 1，每秒翻转）、双点 1/2、计数器（类 3，每秒自增）、
#          冻结计数器（组 21，只读）、
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
`double-bit-input` / `binary-output` / `counter` / `frozen-counter` / `analog-input` /
`analog-output`（也接受 `bi`/`dbi`/`bo`/`ctr`/`fctr`/`ai`/`ao` 短别名）。**`frozen-counter`
（组 21/23）与 `counter`（组 20/22）是两个独立的对象组**：同一个索引在两组里分别是
结算周期冻结值与瞬时值，各自寻址，不能互相顶替；冻结计数器只读。双点量按 bool 映射时：1（分）→
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
`text` 与 `octets` 的宽度规则与读取一致。不做 DTLS、不做 Block1 分块发送。

**Observe 订阅**（RFC 7641）：通道打开 `subscribe: true` 时，驱动用一条独立 UDP
套接字对每个资源发 `GET + Observe:0` 登记关注，把设备推送的通知沉淀进一张最新值表；
`Read` 在「订阅已确认 + 值未过保鲜期 + 类型一致」时直接用推送值、不再发 GET，否则
退回一次真实 GET（绝不拿陈旧推送冒充实时）。通知的 24 位序号按 RFC 7641 §3.4 判新鲜
（含 `2²⁴` 回绕与 128s 重排窗口），乱序/重复被丢弃；链路静默后自动退回轮询，重启
重连后自动重注册。分块通知（Block2 + Observe）不支持，此时该点位退回 GET。能力声明
`Subscribe` 是 `supported`——与 MQTT/OPC UA 同口径，指驱动内部订阅、上层仍是 `Read`
拉取契约。

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

IEC 61850（MMS over TCP，站控层读写）：

```bash
./uic sim -protocol iec61850 -listen 127.0.0.1:10202 &   # 内置 IED（逻辑设备 simpleIO）
./uic -config examples/iec61850.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8102/api/v1/points
#   8 点：LLN0.ST.Health、GGIO1 的两路状态指示与位置、相电压/电流（32 位浮点）、
#          GGIO1.CO.SPCs.stVal（可写）、GGIO1.SP.SETV.setMag.f（可写）
#   自检（不需要 IED）：./uic iec61850read -list
#   只验关联：./uic iec61850read -addr 127.0.0.1:10202 -probe
#   读点位：  ./uic iec61850read -addr 127.0.0.1:10202 \
#               -object simpleIO/GGIO1.MX.PhV.mag.f -type float32 -raw
#   写点位：  ./uic iec61850read -addr 127.0.0.1:10202 \
#               -object simpleIO/GGIO1.CO.SPCs.stVal -type bool -write true
#   浏览模型：./uic iec61850read -addr 127.0.0.1:10202 -browse -domain simpleIO
```

这一条链路比前面所有协议都长：TPKT（RFC1006）→ COTP → 会话层 → 表示层 → ACSE →
MMS，五段封套依次套上才算一次「读一个变量」。封套多不是难点，难点是**每层都有
自己的长度与编号**，其中至少两处必须分开处理：

- **会话层的 LI 与 BER 长度是两套编码。** `0x00..0xFE` 是单字节长度，`0xFF` 后面
  跟**恰好两字节**大端长度——它不是 BER 的长形式（后者后面跟几个字节由低 7 位
  决定）。拿通用的 BER 长度解析器去读 LI，短帧能过、长帧会读错位置。
- **封套的归属不对称。** COTP 的连接请求/确认自带 TPKT，可以直接发；而会话的
  CN/AC/DT/FIN 以及所有 MMS 请求都必须先套「会话 DT + 表示层」，再由 COTP 补上
  TPKT 与 COTP 头。少套一层或多套一层，对端只会断链，没有更具体的提示。

地址沿用 IEC 61850 的对象引用 `LD/LN.FC.DO[.DA]`（示例写作
`simpleIO/GGIO1.MX.PhV.mag.f`）。**FC（功能约束）是名字的一段，不是元数据**：
`GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal` 是两个不同的对象，只有一个存在。所以
点表要么把 FC 写全，要么由 `labels.defaultFC` 补默认值；驱动**不从数据属性名反推
FC**——反推需要完整的 SCL 模型，而驱动手上只有一份点表。

与 KNX 相反，这里的**类型在装置侧**：一个对象是布尔、整型还是浮点，由 IED 的 MMS
模型决定，读回来才知道。因此地址策略是 `WidthNone + AllowSymbol`，点表**不写
length**；`dataType` 只声明平台侧怎么存。把 `MX.PhV.mag.f` 标成 `int32` 不会变成
「读四个字节的整数」，只会把读到的浮点按整数入账——所以 `-raw` 打印的是装置回的
类型，而不是 `-type`。

一条关联上可以有多个在途请求，靠 **invokeID** 分回对应关系。驱动严格配对：迟到的
响应（上一次超时的那个）按 invokeID 丢弃，绝不当作当前请求的答案——不校验就会在
「A 超时、B 复用同一条链路」时把 A 的值写给 B。整条关联仍然用一把锁串行化：MMS 是
请求/应答协议，并发没有收益，只有乱序风险。

单项数据访问错误（`object-non-existent`、`access-denied` 等）不是网络错误，逐条映射：
对象不存在/未定义/地址无效/类型不支持 → `address`（改点表，不计入熔断），访问被拒 →
`permission`，暂时不可用/硬件故障 → `protocol` 且**可重试**。把「点位配错了」与
「装置在重启」混成一类，运维只能两边都查。

模型浏览（`getNameList`）协议支持，但驱动把 `Browse` 能力位声明为 `unsupported`：
枚举整棵 IED 模型是「一次性运维动作」，与调度式采集是两种语义，混进 `Read` 只会让
采集多一条难以解释的慢路径。自检命令走包级 `Browse`，按页收齐（每页 3 项，
continueAfter 用上一页最后一项做游标，上限 256 页防「永远说还有」的对端）。

边界：只做 ACSE + MMS 初始化 + 读/写变量 + 模型浏览。不做 GOOSE/SV（二层/三层
组播，另一套生命周期）、不做报告控制块（服务端主动推，与调度式采集冲突）、不做
文件传输、定值组、数据集读写、TLS。

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

写是单 OID SET（不合并 varbind，避免一条只读对象回滚整批）：`uint16/uint32`→Gauge32、
`int*`→INTEGER、`string/bytes`→OCTET STRING；float/BCD 没有标准线类型，显式拒绝。
应答同时校验错误码与回显值，设备「接受但改了值」会在写入当场暴露。Counter64 经
平台 `float64` 承载，超过 2^53 会丢精度；Counter32 会回绕，时间序列侧要做回绕检测。
团体名按凭据处理（界面掩码），默认 `public` 只是最少配置。

SNMP v3（USM 认证 + 加密，同一 UDP 通道，同一套 OID 地址模型）：

```bash
# 内置代理带 v3/USM：口令走环境变量，避免出现在进程列表与 shell 历史里
UIC_SNMP_AUTH_PASSWORD=authpass123 UIC_SNMP_PRIV_PASSWORD=privpass123 \
  ./uic sim -protocol snmp -v3 -listen 127.0.0.1:16161 \
            -user monitor -auth sha256 -priv aes
# 网关侧：examples/snmp-v3.json，9 个点（sysDescr/UpTime/Name/Services、
#   可写 sysContact/sysLocation、ifNumber、ifInOctets、ifAdminStatus）
./uic -config examples/snmp-v3.json

# 不启网关，直接自检 v3 全链路（发现 → 认证 → 加密 → 读 → 写 → 读回）
./uic snmpread -addr 127.0.0.1:16161 -version 3 -user monitor \
  -auth sha256 -authPassword authpass123 -priv aes -privPassword privpass123 -probe
```

v3 的三件事全部用标准库实现，没有 CGO 也没有第三方依赖：USM 口令本地化为密钥
（1 MiB 扩展 + `H(Ku||engineID||Ku)`）、HMAC 认证（MD5/SHA-1/SHA-224/256/384/512，
截断长度按两张固定表：MD5/SHA-1 是 RFC 3414 的 96 位 = 12 字节，SHA-2 系列是
RFC 7860 的 16/24/32/48 字节——**不是**「摘要长度的一半」）、DES/AES-128/192/256
加密（含 3DES 之外的 CFB/CBC 与 SNMPv3 特有的 salt-IV 构造）。认证摘要对「authParameters 清零后的原始字节」计算，
比对用 `subtle.ConstantTimeCompare`；加密时 `msgData` 外面还套一层 OCTET STRING，
解密后按外层 SEQUENCE 长度截断以抵消 CBC 补齐。

发现流程只有一条路：始终发空 engineID 的无认证探测包（带用户名，代理才会回 Report
而不是静默丢弃），从 Report 一次拿全 engineID/boots/time。即便配置里固定写了
engineID 也照做——「身份已知」不等于「时间基准已知」，而时间基准只能问出来；跳过
它的症状是第一条请求必然吃一个 `notInTimeWindow`，重试一轮还是 0/0，最后报「时间
窗口不同步」。engineID 写错时不静默改用代理的值，而是把两个 engineID 都报出来并
给出处置建议（删掉 engineID 让驱动自动发现）——静默纠正会让「配置写错」变成
「看起来一切正常」，还会掩盖那次拓扑变更。engineID 变更（代理重装/换主控板）时
缓存身份作废、回到空 engineID 重发现，会话自愈；两种失效 Report 走不同起点：
`notInTimeWindow` 只重学时间，`unknownEngineIDs` 才丢身份重发现。

加密口令缺省回退到认证口令（USM 允许两者不同，回退是显式的）。加密强度与认证算法
绑定：AES-192/256 要求 SHA-256 及以上，因为加密密钥是从认证算法的哈希派生的——
这是协议本身的要求，不是实现选择。USM 口令 8..64 字节，过短在配置期就拒绝，
不让它伪装成认证失败。空用户名只在 `noAuthNoPriv` 档位允许。

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

MTConnect（机床数据采集，HTTP + XML，默认端口 5000）：

```bash
./uic sim -protocol mtconnect -listen 127.0.0.1:15000 &   # 内置 Agent（三轴加工中心）
./uic -config examples/mtconnect.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8097/api/v1/points
#   13 点：avail/exec/program/mode（事件）、alm（报警条件）、
#          Xabs/Yabs/Zabs、Xload、Sspeed/Sload、temp（采样）、toolOffset（初始 UNAVAILABLE）
#   自检：./uic mtconnectread -addr 127.0.0.1:15000 -probe    # 列设备模型与数据项 ID
#         ./uic mtconnectread -addr 127.0.0.1:15000 -current   # 整台当前快照
#         ./uic mtconnectread -addr 127.0.0.1:15000 -id Xabs,exec
#         ./uic mtconnectread -addr 127.0.0.1:15000 -sample -from 1 -count 20
```

MTConnect 是唯一「一条请求覆盖整台设备」的协议：`/current` 一次返回所有数据项的
当前值，所以点位多少都不影响请求数（`MultiRead` 声明 `supported`）。点位用
`kind=symbol`，`path` 是 Agent 的 `dataItemId`（如 `Xabs`），不是显示名。寻址不带
宽度、不带下标、不带二级选择器——值有多宽由数据项文本与点表 `dataType` 决定。

**单点问题不升级为链路错误**是这套驱动的核心取舍：`/current` 一条请求覆盖全设备，
若把「某个点 `UNAVAILABLE`」「点表里的 ID 不存在」当成 `Read` 的错误返回，上层会按
错误分类丢掉整条 HTTP 连接——一个本来就不可用的点会让整台设备每周期重连一次。因此
`Read` 只在「文档拿不到 / 解析不了 / 设备选不出来」时返回错误；`UNAVAILABLE` 折成
`bad` 质量、ID 不存在折成地址错误、文本按 `dataType` 解析失败折成地址错误，都只写进
该点。`toolOffset` 初始即 `UNAVAILABLE`，用来验证「数据项存在但此刻无值」显示为坏
质量而不是 0。

值是文本，按 `dataType` 解释；`UNAVAILABLE` 是保留字（不是一个数）。条件（报警）是
两层结构——外层元素是条件类型（`System`/`Actuator`…），内层 `Normal/Warning/Fault/
Unavailable` 是级别——同时也兼容「元素名即级别、`type` 属性是条件类型」的平铺写法。
条件点的值就是级别，级别即质量：`FAULT`→`bad`、`WARNING`→`uncertain`、`NORMAL`→`good`，
同一 ID 多条条件按 `FAULT > WARNING > UNAVAILABLE > NORMAL` 合并。

多设备 Agent（一条产线多台机床）必须显式选设备：`labels.deviceUuid` 优先，其次
`labels.deviceName`；多台且未选就报地址错误并列出可选设备——否则「A 机床的 Xabs 覆盖
B 机床的 Xabs」不会报错，只会让两台设备的坐标互相串。`Probe` 先 GET `/probe`，遇到
404/405 退回 `/current`（裁剪版 Agent 可能只实现后者）；`/sample?from=&count=` 按序号
翻历史窗口，`count` 上限 10000。

地址规范化接受 `host:port`、`http://`、`https://`，省略端口一律 5000；支持反向代理的
基址子路径（`http://host:5000/mtconnect`），拒绝 URL 内凭据、query、fragment。文档上限
32 MiB，gzip 由 `net/http` 自动解压、按魔数兜底畸形响应；XML 按本地名匹配、忽略命名
空间，兼容 ISO-8859-1/latin-1/windows-1252 与 UTF-8 BOM，不校验 MTConnect 版本。只读
协议：`Write` 声明 `unsupported` 并对每项返回 `ErrUnsupported`（标准没有写数据项的
服务）；`Subscribe`/`Browse`/`Discovery` 都标 `unsupported`——能解析 `/sample` 不等于
能订阅，`/probe` 是一次性诊断而不是调度式浏览。

M-Bus 表计（EN 13757-2/3，热量表 / 水表 / 气表 / 电表，RS-485 或串口服务器）：

```bash
./uic sim -protocol mbus -listen 127.0.0.1:8401 -busAddr 1 &   # 内置热表从站（主地址 1）
./uic -config examples/mbus.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8095/api/v1/points
#   11 点：energy 12345678 Wh、volume 456.789 m³、供/回水温度 90.5/56.1 °C、
#          瞬时流量 2.5 m³/h、瞬时功率 24.5 kW、运行时间 86400 s、结算日期 2024-03-15
#          （另 3 点是 q:power-max / q:volume@1 / r:0 的语法示例，默认不采集）
#   只验链路：./uic mbusread -addr 127.0.0.1:8401 -address 1 -probe
#   读整表：  ./uic mbusread -addr 127.0.0.1:8401 -address 1
#   原始字节：./uic mbusread -addr 127.0.0.1:8401 -address 1 -raw
#   扫描总线：./uic mbusread -port /dev/ttyUSB0 -baud 2400 -scan 1-20
```

M-Bus 的链路层有四种帧：单字符 ACK（`E5`）、短帧（`10 C A CS 16`）、长帧
（`68 L L 68 C A CI DATA CS 16`）与控制帧（`L=3`、无数据域）。分帧要同时校验起始符、
**两个相等的 L**、校验和与结束符；校验和是 `C+A+CI+DATA` 的模 256 和，**不覆盖** L 与
起止符——按帧长算会在每个长帧上固定差 3，这是这个协议最容易写错的一处。可变数据结构
（CI=`72h`）是 12 字节固定头（表号 BCD + 厂商 + 版本 + 介质 + 访问号 + 状态 + 签名）
之后的一串**自描述记录**：每条记录先有 DIF（+DIFE 链）说明长度、功能位（瞬时/最大/
最小/故障）与存储号/费率/子单元，再有 VIF（+VIFE 链）说明物理量与 10 的幂，最后才是
数据。没有「第 n 个寄存器」这种东西——偏移取决于前一条记录的长度，换一块表就会错位。

因此点表用 `kind=symbol`，`path` 是归一化物理量名：

| 写法 | 含义 |
|---|---|
| `q:energy` | 第 1 条 `energy` 记录（VIF `03h` → Wh；`07h` 也是 Wh，但倍率 1e4） |
| `q:volume@1` | 存储号 1 的 `volume`（DIFE 表达的上一期历史值） |
| `q:power-max` | 功能位=1 的记录：同一物理量的「最大值」加 `-max` 后缀（`-min`/`-error` 同理） |
| `q:energy/1` | 同一量名第 2 次出现（多费率表常见），`/0` 可省略 |
| `r:0` | 报文里第 0 条记录，语义无关——厂商自定义 VIF 不认识时的逃生口 |

单位与倍率由 VIF 表统一换算到工程值（`energy` 恒为 Wh、`volume` 恒为 m³、温度按 °C），
点表不需要知道这块表用了 0.001 Wh 还是 10000 Wh 的刻度。数值宽度按 DIF 解：整数
1/2/3/4/6/8 字节、BCD 2/4/6/8/12 位（最高半字节 `Fh` 是负号）、REAL 4 字节 IEEE754、
变长（`DIF=0Dh`）带 LVAR。类型 G 日期（2 字节）与类型 F/I 日期时间（4/6 字节）按位域解，
**时间无效位**置位时明确报文本 `(时间无效)` 而不是把 `0xFF` 解成 2159 年。

一条 `REQ_UD2` 让从站把**当前整张表**打包上送，所以点位多少都只发一次请求，
`MultiRead` 声明 `supported`；同一条报文里的点位天然一致（同一次快照），这是它与
「每点一次读」的驱动在语义上的关键差别。`FCB/FCV` 是重传语义的核心：**同一轮重试保持
FCB 不变、跨轮才翻转**，从站据此回放上一次应答（访问号不重复累加、存储值不被重读）。
写错的表现是「偶尔读到上一次的值」，极难发现，因此测试直接锁住「同 FCB 重放的访问号
不变」。读取失败后驱动发一次 `SND_NKE` 复位从站的 FCB 状态再重试——长期运行的电池表
忘记 FCB 是现场最常见的「隔几轮才读通一次」。

`Probe` 用 `SND_NKE` 的**单字节应答**，它不依赖任何数据结构，因此能把「完全没字节
回来」（物理层/波特率/站号错）与「有字节但解析不了」（参数错）分开。反过来，若驱动
发现「收到字节流但一帧都没成」，会明确报协议错误并提示查波特率/校验位（标准
2400 8E1）与终端电阻，而不是笼统报超时——这两种故障的处置方式完全不同。

边界：只做**主地址**寻址（0..250）。次级地址需要先 `SND_UD(CI=52h)` 选中且地址域变
8 字节、校验范围随之变化，混合实现比不支持更难排查，因此编解码层显式拒绝 `FDh` 地址。
不做 `SND_UD` 写入（CI=`51h` 发数据 / `5Eh` 改地址 / `50h` 校时）：写什么完全由表厂
剖面决定，写错会改变计费参数，改地址/速率还会让表从当前站号「消失」——`Write` 诚实
声明 `unsupported`。固定数据结构（CI=`73h`，16 字节、两个计数器）能解但不推荐使用，
它是老式紧凑表的形态。

承载支持 `serial`（真实 RS-485，走共享串口层总线级互斥）与 `tcp`（串口服务器/DTU
透传，锁退化为每连接互斥）；两者分帧与命令序列完全相同，所以内置 TCP 从站能直接当
自检目标。串口协议 `concurrency` 必须为 1。

DLMS/COSEM 电表（IEC 62056-46/-53/-62，LN 逻辑名引用 + 无安全，HDLC 三层承载）：

```bash
./uic sim -protocol dlms -listen 127.0.0.1:4051 &   # 内置三相电表从站（逻辑设备 1，客户端 16）
./uic -config examples/dlms.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8095/api/v1/points
#   20 点（15 启用）：正向/反向有功电能、无功电能、三相电压/电流、总功率、
#          功率因数 0.985、频率 50.01 Hz、表号 12345678、逻辑设备名、固件版本、
#          厂商、电表时钟（另有 5 点是语法示例，默认不采集）
#   只验链路：./uic dlmsread -addr 127.0.0.1:4051 -probe
#   默认点表：./uic dlmsread -addr 127.0.0.1:4051
#   指定 OBIS：./uic dlmsread -addr 127.0.0.1:4051 -obis 1.0.1.8.0.255:3:2
#   试读全表：./uic dlmsread -addr 127.0.0.1:4051 -scan
#   内置 OBIS 表：./uic dlmsread -list
#   串口实机：./uic dlmsread -transport serial -port /dev/ttyUSB0 -baud 9600 -address 1
```

DLMS 是唯一「地址是标准固定的六段码」的协议：`1.0.1.8.0.255` 在任何厂商的表上都
是正向有功电能。这是点表能离线书写、跨表复用的原因，也是本驱动只做 **LN 引用**的
理由——SN（short name）引用要厂商在 `object_list` 里分配编号，同一个 OBIS 在不同表
上编号不同，必须先「发现再寻址」，与「点表直接写地址」的模型不兼容。

点表 `path` 是 `<OBIS>[:<class>[:<attribute>]]`。只写 OBIS 时类号与属性号从内置表
推导；**查不到就报 `ErrAddress` 而不是猜**——猜错的类号会让表回
`object-class-inconsistent(9)`，而「猜了一个看起来能读的类」会读到另一个对象的合法数字。
三个写法：

| 写法 | 含义 |
|---|---|
| `1.0.1.8.0.255` | 按内置表推导（Register(3) / attribute 2） |
| `1.0.1.8.0.255:3` | 显式类号，属性号默认 2（value） |
| `1.0.1.8.0.255:3:3` | 显式类号与属性号（这里是 scaler_unit） |
| `1-0:1-8-0-255:3:2` | 标准正文写法：OBIS 内部自带冒号，必须能正确切分 |

HDLC 三层承载有四件事必须一次做对：**① 长度字段是两标志之间的全部字节数**（含 2 字节
格式字段与末尾 FCS，不含标志），整帧 = `len(body)+2`，11 位跨两字节且 bit11 是分段位；
**② HCS 覆盖 `format+dest+src+ctl`**，所以必须先算长度再算 HCS——先占位后回填会让头
校验与格式字段不一致，接收方判定「头校验失败」并丢弃整帧，症状是第一帧之后就超时；
**③ 类型 3 帧不做透明转义**，`0x7E` 可以合法出现在 info 与 FCS 里，分帧必须按长度字段
定界而不是扫标志——按标志扫会在某些应答上把一帧切成两半，而且只有特定数值才触发
（本仓库用 `1.0.13.7.0.255` 的 FCS `7E 64` 做回归）；**④ 地址域是 7 位/字节 + 扩展位**，
1/2/4 字节，客户端恒 1 字节，4 字节按「高 7 位组 + 低 7 位组」各左移一位。

应用层是 A-XDR（IEC 61334-6）编码的 APDU。建链 `SNRM → UA`、`AARQ → AARE`，读
`Get-Request-Normal → Get-Response-Normal`，断链 `DISC → UA`。`SNRM` 的金帧是
`7E A0 07 03 21 93 0F 01 7E`，AARQ/AARE 的逐字节向量在测试里锁死。**U 帧控制域是固定
常量，不携带可变的 P/F 位**（UA=0x63 的 bit4 为 0），要求往返保持 P/F 会让建链永远等
不到应答。LLC 头 `E6 E6 00`（请求）/ `E6 E7 00`（应答），`E7` 是「这一帧是应答」的
唯一信号；缺了它说明收到的是服务端主动上报（本驱动不做，报协议错）。

**scaler 是 DLMS 最容易写错的一处**：Register 类（3/4/5）的 attribute 2 是原始整数，
attribute 3 是 `structure {integer scaler, enum unit}`，工程值 = 原始值 × 10^scaler，
**scaler 为负是除**。驱动读完 attribute 2 再读一次 attribute 3 并应用，带进程内缓存
（scaler 是对象的静态配置参数，不会随测量值变化）。`ApplyScaler` 必须**一次**乘除
`10^|scaler|`：逐次除 10 会累积舍入误差，`5001 / 10 / 10` 得到 `50.010000000000005`，
屏幕上就是一串尾数且会污染聚合值。单位枚举同样只认标准表：**Wh = 29、VAh = 30、
varh = 31**——写错不会报错，只会把电量标成「视在电能」，数值一切正常，最难发现。

`data-access-result` 在 A-XDR 下是 `[1] IMPLICIT ENUMERATED`，定长 8 位、**无长度
前缀**，线上只有 `01 <值>` 两个字节。不要「兼容」`01 01 xx` 那种带长度的写法：结果码
为 `hardware-fault(1)` 时两者前两字节完全相同，兼容会把它读成「数据不完整」，于是所有
硬件故障都被报成超时。宁可只认标准形态，让非标实现明确报错。

时间类型按 IEC 62056-62 §4.1.6.1 布局：`date_time` 12 字节是
`年(2) 月 日 星期 时 分 秒 百分秒(2) 偏差(2) 状态`——**星期在日月之后、时分之前**，
错一位会把日当时、星期当分。时区偏差是带符号分钟，`0x8000` 表示未指定。

边界：只做 LN + 无安全（Lowest Level Security）+ HDLC 承载。不做 SN 引用；不做
HLS/GMAC（密钥协商、帧计数器、AES-GCM 各厂商分发方式都不同，做了也只能覆盖一部分
现场，却会把「密码错」与「地址错」混成同一类超时）；不做 TCP/UDP 的 COSEM wrapper
（另一套 version + wrapper 头封装）；不做 Set/Action/EventNotification（写方向要处理
selective access 与分块写，半成品比不做更难排查，`Write` 诚实声明 `unsupported`）；
不做分块读——表在应答超过 max-PDU 时**必然**走数据块，驱动能识别并报 `ErrUnsupported`
并提示调大 `max-pdu`，但不去拼块（拼块要处理块号连续性、重传与超时重取）。

一条 `Get-Request` 只读**一个**属性，这是协议硬约束，所以一轮采集的往返数是点位数。
标准里的批量机制 `get-request-with-list` 要求服务端声明 `multiple-references`（现场
极少支持），因此 `MultiRead` 声明 `unsupported`——把「N 个点 = N 次往返」这个事实暴露
给配置的人，比假装合并更负责任。300 个点挂 9600bps 串口，一轮要十几秒。

`Probe` 打印的是**协商后**的 conformance，不是本端请求的那份：标准允许服务端砍掉一半
能力，按请求值判断会得出错误结论。物理层固定 **8N1**（IEC 62056-46 的 HDLC profile），
配成 8E1 会收到全错位的字节——若驱动发现「有字节回来但一帧都没成」，会报协议错误并
提示查波特率/校验位与终端电阻，而不是笼统报超时。

水文遥测站 SL 651（《水文监测数据通信规约》SL 651—2014，中心站侧查询 + 应答）：

```bash
./uic sim -protocol sl651 -listen 127.0.0.1:4001 &   # 内置遥测站（站号 31234567）
./uic -config examples/sl651.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8097/api/v1/points
#   9 点（5 启用）：瞬时水位 Z=0.127m、1h 时段雨量 P1=0.5mm、累计雨量 PT=123.4mm、
#          瞬时流量 Q=1234.567m³/s、电源电压 VT=12.1V
#          （另有 4 点是地址语法/缺测/地址错误的演示，默认不采集）
#   只验链路：./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -probe
#   整帧要素：./uic sl651read -addr 127.0.0.1:4001 -station 31234567
#   按点表读：./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -points Z,PT,VT
#   批量要素：./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -points DRP,DRP[3]
#   要素表：  ./uic sl651read -list
#   串口实机：./uic sl651read -transport serial -port /dev/ttyUSB0 -baud 2400 -station 31234567
```

SL 651 与仓库里其它表计协议（645 / 188 / M-Bus / DLMS）的差别不在字段多少，而在三条
「凭印象写就会当场错」的实现事实：

**① 分帧不能扫标志字节。** 帧以 `7E 7E` 开头，但正文与 CRC 里可以合法出现 `0x7E`——
本仓库的金向量第一帧正文首字节就是 `7E`。任何「读到 `0x7E` 就当帧尾」的写法都会把
一帧切成两半。只能先读长度字段再按长度收，整帧 = **17 + 长度**。

**② 长度字段数的是「报文起始符之后、结束符之前」的字节数**，不含 STX、不含 ETX/ETB、
也不含 `7E 7E`；高 4 位是上下行标识（0000 上行 / 1000 下行），低 12 位才是长度。
M3 多包时「包总数 + 包序号」那 3 字节**计入**长度（规约这么定，不是实现选择）。

**③ CRC 是 CRC-16/MODBUS 变体，但结果不取反。** 初值 `FFFF`、右移、多项式 `A001`、
**xorout 0000**，覆盖 `7E 7E` 起至结束符止，**高位字节在前**。仓库里其它协议多用
X-25（结果取反）且低字节在前，照抄会得到 `3C51` 而不是 `C3AE`。链路维持报的完整金帧：

```
7E 7E 00 9876543210 1234 2F 0008 02 7E08221011100747 03 C3AE     (25 字节)
                                               ↑ 正文首字节就是 7E
```

还有一条只看上行帧永远发现不了的差别：**上下行帧的地址字段顺序相反**（表 11 上行
2=中心站/3=遥测站，表 12 下行 2=遥测站/3=中心站），而密码、功能码、长度字段的偏移
不变。只做被动收包的实现看不出问题，一旦主动发查询就会把站号写进中心站字段。

站号是 **5 字节 BCD，装 10 位十进制**。现场用的测站编码普遍是 8 位（如 `31234567`），
所以驱动按 `0..9999999999` 校验而不是按「5 位数」——按 5 位卡会把合法站号判成配置错误。

正文是**要素信息组**，不是固定偏移的寄存器：每个要素自带「引导符(1) + 数据定义字节(1)」，
数据定义字节 = `(字节数 << 3) | 小数位数`。也就是说报文自己说明每个值有多宽、有几位
小数，接收方不需要预先协商点表格式。附录 C 表 C.1 的 131 项可解释引导符（水位 Z、
时段/累计雨量 P1/PT、流量 Q、电压 VT、闸位 GH、含水率 M10…）就是协议的数据模型本身，
`internal/codec/sl651/tables.go` 把它写死在代码里——同一台终端送来的 `39H` 在任何工程
里都是「瞬时河道水位、潮位」，做成配置项只会让同一份报文在不同站点解出不同含义。

例外是「引导符类」：观测时间（`F0H`）、测站编码（`F1H`）、人工置数（`F2H`）、图片
（`F3H`）、批量雨量/水位（`F4H..FCH`）的数据定义字节被规约**写死**成固定值，因此它们的
宽度**不能**从高 5 位读出来——按高 5 位读会把「观测时间 5 字节」当成「30 字节」，
当场错位。规约原文就是这么定的，不是可推导量。

**DRP / DRZ 是最容易解错的一类。** `F4H`（1h 内每 5min 时段雨量）在报文里是**一个
12 字节数据**，语义上却是 12 段 × 1 字节 5min 值；`F5H..FCH`（相对水位）是 24 字节 =
12 段 × 2 字节。把它们当一个 HEX 整数解，会得到 `1844674407370955300` 这种溢出值——
**不报错、值域看着像大数**，是最危险的一类错。本仓库用 `Guide.Slot` 显式声明段宽
（DRP=1、DRZ=2），点表地址写成 `<要素>[<段号>]`（`DRP[0]`..`DRP[11]`，段号从 0 开始），
段号只能用在批量要素上——`Z[1]` 会报地址错而不是让 `SlotNumber` 返回「越界」。

DRP/DRZ/ZT 的数据是 **HEX 整数不是 BCD**：DRP 1 字节 × 0.1mm（`0x19` = 2.5mm），DRZ
2 字节 × 0.01m（`0x01F4` = 500cm = 5.00m）。按 BCD 解 `0x01F4` 会得到 194，正好落在
「看起来合理的 194cm」上——这类错值比报错难查得多，所以换算按引导符分派而不是按数据
定义字节。ZT（遥测站状态及报警信息）是 4 字节 HEX 状态字，没有尺度，按原始 HEX 输出。

缺测的表示是「与数据位数一样的 `F`」，不是 9999。驱动把它降级为 `quality=bad` 而不是
报地址错误——规约允许某个时段没有数据；若当成 9999 解出来会直接污染数据库。

查询侧：中心站发 `37H`（查询实时数据）/ `38H`（查询时段数据）/ `3AH`（查询指定
要素），**应答沿用查询的功能码**（不是另有一套应答码）。这一点必须和「自报帧」分开判：
`2F/30/31/32/33/34` 是遥测站主动上送，而 `37/38/3A` 既有下行查询也有上行应答，方向只能
从长度字段高 4 位看。驱动的 read 路径按「一次查询取整帧全部要素」设计——37H 的应答与
32H 定时报同形，逐点发 37H 不但慢，还会让同一设备的点位落在不同快照上。

电压 `VT`（`38H`）在报文里**独立编报**，位于要素组之后，不是要素组的一项。codec 因此
把它放在 `Report.Voltage` 而不是 `Report.Fields`——少这一处兜底，`VT` 点位会「永远读不
到」，而 CRC 与其它要素都正常，极难发现。

边界：只做中心站侧查询（`37H` / `38H` / `3AH`）与上行报文解析。不做 ASCII 字符编码帧
（长度字段是 4 个 ASCII 字符、数值是文本，分帧/校验/宽度全不同，遇到返回
`ErrUnsupported`）；不做 M3 多包重组（重组要会话状态机，本包只如实标出 ETB「后面还有
包」）；不做配置表读写（`40H~43H`）、控制命令（`4CH~4FH`）、人工置数（`35H`）、图片报
（`36H`）——这些是写/控制类，`Write` 声明 `unsupported`；不做 `FFXXH` 自定义扩展（`XX`
的含义只有设备方知道，报 `ErrUnknownGuide`）。

称重仪表 Toledo（METTLER TOLEDO「连续输出 / Continuous Output」，ASCII 逗号三段式，只读推送）：

```bash
./uic sim -protocol toledo -listen 127.0.0.1:4001 &   # 内置衡器（~20 帧/秒 ST,GS,+00149.0kg）
./uic -config examples/toledo.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8098/api/v1/points
#   8 点（7 启用）：重量 weight=149、状态 status=ST、毛净 mode=GS、单位 unit=kg、
#                   稳定 stable=true、运动中 motion=false、小数位 decimals=1
#   只验链路：./uic toledoread -addr 127.0.0.1:4001 -probe
#   最新一帧：./uic toledoread -addr 127.0.0.1:4001
#   按点表读：./uic toledoread -addr 127.0.0.1:4001 -points weight,status,stable
#   读数字段：./uic toledoread -list
#   串口实机：./uic toledoread -transport serial -port /dev/ttyUSB0 -baud 9600
```

Toledo 与其它表计的差别不在字段多少，而在**方向反了**：衡器上电后**自己不询问就持续
吐帧**（典型 10~25 帧/秒），帧里没有站号、寄存器、功能码。所以本驱动是**后台收帧
循环 + `Read` 取最新一帧**，`Read` 里**没有任何网络交互**（耗时与点位数量无关、链路
安静也不会超时，与 MQTT 驱动同源）；真正断链（读到底层错误）才标记重连，而不是每次
`Read` 去「探一下」——那会把安静的正常设备误判为断链。

**「TOLEDO」不是单一帧格式，而是一个家族。** 本仓库只实现现场最通用、各品牌标注「Toledo
compatible」时普遍采用的 **ASCII 逗号三段式**，其规范形态（D39 手册逐字节实例）：

```
53 54 2C 47 53 2C 2B 30 30 31 34 39 2E 30 6B 67 0D 0A   即  ST,GS,+00149.0kg<CR><LF>
└┬┘  └┬┘  └────┬────┘
状态   毛净    <符号><数值><单位>
```

**二进制 TOLEDO 格式**（`STX` 开头 18 字节：状态字 A/B/C + 6 位重量 + 6 位皮重 + `CR` +
校验和）在手册里有完整记载，但本版**不实现**：`Parse` 识别到 `STX` 后**返回带可执行
指引的错误**（提示切到 ASCII 格式），而不是当 ASCII 硬解出垃圾值——那会是一种「值看着
合理、其实全错」的最坏故障。

帧边界只有 **CR/LF，没有长度字段也没有同步字**。收帧因此强制以「**大写**状态首字母
`S`/`T`/`U`/`O`/`L` 或 `STX`」为起点，其余字节丢弃并计 `Dropped()`。只认大写是刻意的：
小写 `l`/`o`/`s`/`t`/`u` 在 ASCII 噪声（上电横幅、其它设备输出）里太常见，纳入等于
没做过滤；`Parse` 仍接受小写状态（为健壮），但收帧层从严。

**状态→质量码映射**是核心：从未收帧 → `comm_error`、超 `staleAfter` → `stale`、`US`
运动中 → 重量点位 `uncertain`、`OL`/`UL` 超/欠量程 → `bad`、数值段掩码占位（无数字，
如 `M-------L`）→ `bad`。注意**只有重量类点位随称量状态降级**，`status`/`unit`/`mode`/
`decimals` 这些描述性字段恒 `good`（它们本身始终有效信息）。掩码不是解析错误：衡器
用占位符表达「出错」，此时 `Weight` 无意义但 `Status` 仍有效——「超量程」恰恰靠 `Status`
而非数值表达。

数值精度走**字符串拆分**而不是先在 `float64` 上取整：连续输出的整数位可能很宽，而
`float64` 尾数只有 15~16 位有效十进制，先乘 10^n 取整会在大值上丢精度。`Weight` 存
带符号值（符号单独存 `Sign` 供诊断，驱动取数直接用，避免每处都乘一次符号而漏乘）。

边界：只做**只读推送采集**。不做二进制 TOLEDO 格式、不做 SICS 命令应答（`I0`/`IP`/
`SIR` 等交互式命令，与「被动收连续输出」是两种工作模式）、不做任何写操作（衡器只读，
`Write` 恒 `ErrUnsupported`）、不做单位换算（单位集合开放 `kg`/`lb`/`g`/`t`/`oz`/
`kN`/`lb_oz`，`GR` 是 `GS` 毛重别名，`lb oz` 归一为 `lb_oz` 磅盎司双单位）。

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

零第三方依赖、零 CGO，仅标准库。构建 / 发布 / 供应链验收已固化进 `Makefile`（平台无关，CI 入口是 `make check`）：

```bash
make check    # build + vet + gofmt + go test -race
make load     # 负载测试：1000 点 / 50 设备，报告吞吐/常驻内存/协程（ADR C4/C5）
make release  # 交叉编译 linux/amd64·arm64·windows/amd64 单二进制到 dist/
make sbom     # 从产物内嵌 buildinfo 生成 CycloneDX SBOM（零第三方工具）
```

## 目录

| 路径 | 内容 |
|---|---|
| `cmd/uic/` | 单入口：`run` / `sim`（Modbus / S7 / OPC UA / MQTT / MC / FINS / Host Link / KNX / IEC 61850 / EtherNet-IP / BACnet-IP / BACnet MS/TP / IEC 104 / IEC 101 / Mewtocol / DNP3 / CoAP / 645 / 188 / M-Bus / SNMP / ADS / SECS / MTConnect / DLMS / SL 651 / Toledo）/ `mbread` / `mcread` / `finsread` / `hostlinkread` / `enipread` / `bacread` / `mstpread` / `iec104read` / `iec101read` / `mewtocolread` / `dnp3read` / `coapread` / `knxread` / `iec61850read` / `dlt645read` / `cjt188read` / `mbusread` / `snmpread` / `adsread` / `secsread` / `mtconnectread` / `dlmsread` / `sl651read` / `toledoread` / `uaread` / `s7blocks` / `s7upload` / `instantiate` / `import` / `token` / `-check` / `version` |
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
| `internal/codec/enip` | EtherNet/IP 纯函数编解码（封装头 / CPF / SendRRData / CIP 请求应答 / 符号路径 / Read/Write Tag / Forward Open·Forward Close / SendUnitData / Read·Write Tag Fragmented / 多服务合包 / 类型表） |
| `internal/driver/enip` | CIP 显式报文主站（标签读写、数组 offset、STRING、bytes 两阶段读、位 RMW、多服务合包与自动退回） |
| `internal/sim/enip` | EtherNet/IP 内置从站（RegisterSession / Read/Write Tag / Forward Open·Close / SendUnitData / Read·Write Tag Fragmented / 多服务合包 / 默认标签表） |
| `internal/codec/bacnet` | BACnet/IP 与 MS/TP 共享纯函数编解码（BVLC / NPDU / APDU / 标签 / 对象属性名表 / Read-WriteProperty；MS/TP 分帧与双 CRC） |
| `internal/driver/bacnet` | BACnet/IP 主站（对象属性读写、UDP invoke ID 配对、按对象语义选写标签、错误类映射） |
| `internal/sim/bacnet` | BACnet/IP 内置从站（对象表 / 属性读写 / 数组下标 / 写权限 / 类型收敛） |
| `internal/driver/bacnetmstp` | BACnet MS/TP 主站（单主站轮询从站、滚动字节扫描切帧、被动配合令牌环与 PollForMaster 现场探测、`presilence` 静默、串口/串口服务器 TCP 双承载） |
| `internal/sim/bacnetmstp` | BACnet MS/TP 内置从站（MAC 128 / 镜像 sim/bacnet 对象表 / 属性读写与数组下标 / 写权限与类型收敛 / 每秒自增 / 垃圾帧重扫） |
| `internal/codec/iec60870` | IEC 60870-5 共享应用层（ASDU 头与元素 / 剖面 `Profile` / `HasIOA` / CP56Time2a / 品质位 / 请求构造 / 点位语义：对象类别、值转换、命令构造 / 类型与原因名表） |
| `internal/codec/iec104` | IEC 60870-5-104 纯函数编解码（APCI 三种帧型、序号与 k/w）+ 对共享应用层的别名转发 |
| `internal/driver/iec104` | IEC 104 主站（STARTDT 建链、站总召唤、电能量召唤、按点位 ID 的最新值表、五种命令、否定确认分类、旁路表） |
| `internal/sim/iec104` | IEC 104 内置从站（对象表 / STARTDT 门控 / 召唤快照 / 命令三段式 / 突发上送） |
| `internal/codec/iec101` | IEC 60870-5-101 纯函数帧层（FT1.2 固定帧 / 可变帧 / 单字节帧、控制域 FCB/FCV/ACD/DFC、校验和、流式收帧 `Reader`） |
| `internal/driver/iec101` | IEC 101 非平衡轮询主站（复位链路、第 1/2 级轮询、站总召唤 + 电能量召唤、FCB 原帧重传、命令两段确认、双承载、旁路表） |
| `internal/sim/iec101` | IEC 101 内置从站（FT1.2 over TCP 透传、FCB 去重、ACD、两级队列、召唤快照与终止、命令两段确认） |
| `internal/codec/dnp3` | DNP3 纯函数编解码（链路帧与 CRC / 流式收帧 / 传输分片重组 / 应用层请求响应 / 对象解析（含组 21/23 冻结计数器）/ 请求头 / CROB 与组 41 构造） |
| `internal/driver/dnp3` | DNP3 主站（复位链路 + TEST_LINK、类 0/1/2/3 轮询、未请求响应、应用层确认、CROB 与模拟量直接控制、旁路表） |
| `internal/sim/dnp3` | DNP3 内置从站（链路 ACK / 类 0 静态快照 / 三类事件队列与 inflight / 冻结计数器组 21/23 / CROB 与组 41 直接控制回显） |
| `internal/codec/coap` | CoAP 纯函数编解码（4 字节头 / 选项 delta-len 扩展 / uint 与 Block 编解码 / URI 拆分 / link-format 解析 / Observe 新鲜度判定） |
| `internal/codec/textval` | 文本/JSON 标量 → 平台值（MQTT 与 CoAP 共用的类型规则；selector 取字段） |
| `internal/driver/coap` | CoAP 主站（GET/PUT/POST、Block2 续读与降级、分离响应、指数重传、资源发现、坏包跳过、Observe 订阅与退回 GET） |
| `internal/sim/coap` | CoAP 内置服务端（CON/NON / MID 去重 / Block2 / 分离响应 / RST / link-format 发现 / Observe 推送） |
| `internal/codec/knx` | KNXnet/IP 纯函数编解码（组地址 4/4/8 与物理地址 4/4/8 / DPT 切片表与编解码 / cEMI `L_Data.req/ind/con` / 信封与全部服务报文 / HPAI） |
| `internal/driver/knx` | KNXnet/IP 隧道主站（建隧道/心跳/断链重建、8 位序号与重传、组地址读写、`L_Data.con` 短窗口确认、源地址三级优先） |
| `internal/sim/knx` | KNXnet/IP 内置网关（隧道分配与回收、序号严格校验与 8 位回绕、组地址对象表、`L_Data.con`、四种故障注入） |
| `internal/codec/iec61850` | IEC 61850 纯函数编解码（TPKT / COTP / 会话 LI 与 SPDU / 表示层 CP-CPA-CP-Type / ACSE AARQ-AARE / MMS Initiate 与 Read/Write/getNameList / 数据值与访问结果 / 对象引用与 FC） |
| `internal/driver/iec61850` | IEC 61850（MMS）主站（五段关联握手、invokeID 严格配对、读写变量、对象引用 FC 补全、单项数据访问错误分类、`Browse` 分页枚举） |
| `internal/sim/iec61850` | IEC 61850 内置 IED（变量树 / 关联可拒 / 读写 / getNameList 分页 / 有序释放 / 读超时注入） |
| `internal/codec/dlt645` | 645-2007 纯函数编解码（帧/校验和 / 数据标识 DI 表 / 压缩 BCD 与小数位 / 时间与地址 / 收帧扫描 / 后续帧序号） |
| `internal/driver/dlt645` | 645 主站（RS-485 与串口服务器双承载、总线锁、逐点读 + 后续帧、写权限四道闸门、错误分类） |
| `internal/sim/dlt645` | 645 内置从站（按标准宽度回数据 / 地址过滤 / 后续帧拆分 / 写权限与密码 / 广播校时静默生效 / 拉合闸） |
| `internal/codec/cjt188` | 188-2004 纯函数编解码（单 `68` / 7 字节 BCD 地址 / 无 `0x33` / 2 字节 DI / 按表类型分派的字段 schema / 单位代号 / 7 字节时间 / 收帧扫描） |
| `internal/driver/cjt188` | 188 主站（RS-485 与串口服务器双承载、总线锁、按 DI 缓存字段、阀门/写地址两道闸门、错误分类） |
| `internal/sim/cjt188` | 188 内置从站（水/热两套 901F / D120..D12B / 读地址 / 阀门写 / 写地址 / 地址过滤） |
| `internal/codec/mbus` | M-Bus 纯函数编解码（`E5` 单字符 / 短帧 / 长帧 / 控制帧四类分帧、`C+A+CI+DATA` 模 256 校验、双 `L` 一致性与整帧长 `L+6`、显式拒绝 `FDh` 次级地址；DIF/DIFE 与 VIF/VIFE 记录解析、整数/BCD/REAL/变长 LVAR、Type G/F/I 日期时间与无效位、固定数据结构、统一 VIF 表与幂次换算、`q:`/`r:` 反查与记录编码） |
| `internal/driver/mbus` | M-Bus 主站（SND_NKE 初始化、REQ_UD2 整表上送、FCB 跨轮翻转与同轮重放、失败后 NKE 复位再试、后续报文跟随上限、状态字降质量、`q:` 物理量寻址 + `r:` 逃生口、坏帧计数区分「无字节」与「有字节不成帧」；自检 `Table`） |
| `internal/sim/mbus` | M-Bus 内置表计（虚拟热表 8 条记录 / FCB 重放不改访问号 / 访问号自增 / 地址过滤静默 / 校时与坏校验注入 / 静默模式 / 状态位与后续报文标志 / 值可变） |
| `internal/codec/snmp` | SNMP v1/v2c/v3 纯函数编解码（BER TLV / 整数最短补码 / OID base-128 / 7 类 PDU / varbind / 异常标记 / error-status 分类；v3 另有 USM 安全参数、`BuildV3`/`ParseV3`、HMAC 摘要、DES/AES CBC-CFB 加解密、口令本地化、Report 分类、时间窗口判定） |
| `internal/driver/snmp` | SNMP 主站（v1/v2c 批量 GET / v1 `noSuchName` 退化 / 单 OID SET / 类型映射与回显校验 / request-id 配对；v3 引擎发现、时间同步、密钥派生与缓存、Report 驱动的自愈重试；自检 `GetRaw`、`Walk`） |
| `internal/sim/snmp` | SNMP 内置代理（system 组 + 两块网卡 / GET、GETNEXT、GETBULK、SET / community 静默丢弃 / v1 与 v2c 差异 / SET 原子性；v3 真做 HMAC 验签与解密、6 类 Report、引擎身份可切换、故障注入） |
| `internal/codec/ads` | ADS/AMS 纯函数编解码（6 字节 AMS/TCP 头 / 32 字节 AMS 头 / 7 类命令 / 两层长度一致性 / NetId 解析 / 错误码分类） |
| `internal/driver/ads` | ADS/AMS 主站（符号名→句柄→值 / 句柄缓存与失效重解析 / 逐点事务 / 类型范围检查 / AMS 与 ADS 错误分类 / 自检 `DeviceInfo`、`DeviceState`、`WriteControl`） |
| `internal/sim/ads` | ADS/AMS 内置服务端（默认变量表 / 按名分配句柄 / 只读标记 / 宽度校验 / 状态控制 / 句柄失效模拟） |
| `internal/codec/dlms` | DLMS/COSEM 纯函数编解码（HDLC 类型 3 帧按长度字段定界、HCS/FCS 两段 CRC-16/X-25、1/2/4 字节 7 位地址域、LLC 头请求/应答区分、I/RR/RNR/U 帧控制域；A-XDR 全标签编解码含 `date_time` 12 字节布局与时区偏差；AARQ/AARE 的 BER 逐段解析、xDLMS-Initiate 协商、`Get-Request-Normal`/`Get-Response`（normal/datablock/list 三分支）、`data-access-result` 2 字节形态；OBIS 六段解析与 `SplitRef` 地址切分、内置常见对象表、COSEM 类号/单位枚举、`ApplyScaler` 单次乘除） |
| `internal/driver/dlms` | DLMS/COSEM 主站（LN 引用 + 无安全 + HDLC；SNRM→AARQ→GET→DISC 四步建链与断链、I 帧序号维护、`<OBIS>[:class[:attr]]` 地址推导（查不到报 `ErrAddress` 不猜）、scaler 自动应用与进程内缓存、`data-access-result` 分类到 `ErrAddress`、坏帧归 `ErrProtocol` 并提示查波特率、链路错误缓存避免逐点等超时；自检 `Scan`、`Association`） |
| `internal/sim/dlms` | DLMS 内置电表（三相表 40+ 项：电能/费率/瞬时量/表号/固件/厂商/时钟，值带 scaler_unit；SNRM/DISC 应答、AARQ 接受或拒绝、Get-Request 分派、未建关联读回 `read-write-denied`、对象未定义与类号不一致两种结果码、静默/坏校验/拒绝关联三种故障注入） |
| `internal/codec/sl651` | SL 651 纯函数编解码（按长度字段分帧而非扫 `7E` / 长度字段语义与 `17+长度` / CRC-16/MODBUS 变体不取反且高位在前 / 上下行地址字段反序 / 附录 C 131 项引导符全表 / 引导符类固定宽度与固定定义字节 / 数据定义字节 `(字节数<<3)|小数位` / 批量要素 `Slot` 段宽与 `DRP[11]` 段寻址 / DRP·DRZ·ZT 按 HEX 解并填缺测而非 9999 / 均匀时段报 31H 与小时报 34H / 查询帧 37H·38H·3AH 构造 / ASCII 帧与 M3 多包显式拒绝） |
| `internal/driver/sl651` | SL 651 中心站驱动（一次 37H 取整帧全部要素、应答沿用查询功能码、`<要素>` 与 `<要素>[<段号>]` 地址、`VT` 走 `Report.Voltage` 兜底、缺测降 `quality=bad`、坏帧计数区分「无字节」与「有字节不成帧」、链路错误缓存；自检 `Report`） |
| `internal/sim/sl651` | SL 651 内置遥测站（7 要素覆盖 BCD/HEX/状态字/整组缺测四分支、37H·38H·3AH 分派、自报帧 2F·30·32·34、密码与站号过滤静默、坏校验注入、值可变） |
| `internal/codec/secs` | SECS 纯函数编解码（SEMI E37 HSMS 帧：4 字节长度前缀（含 10 字节头）+ SessionID/PType/SType / 控制消息 / SECS-II 项编解码 / `MaxPayload` 上限 / SxFy 消息体语义） |
| `internal/driver/secs` | HSMS 主站（Select 握手、SystemBytes 配对、按对象分组批量读、S2F15 写常数、Linktest 探活、自检 `Describe`/`RawRead`） |
| `internal/sim/secs` | HSMS 内置设备（SVID/ECID 双编号空间 / Select 门控 / 常数写拒绝未知编号 / 事件与报警推送） |
| `internal/codec/mtconnect` | MTConnect 纯函数编解码（按本地名匹配、忽略命名空间 / Header 序号与 nextSequence 补齐 / Samples、Events、Conditions 三类观测 / 两层与平铺条件写法与按严重度合并 / `/probe` 设备模型 / MTConnectError 文档 / ISO-8859-1、latin-1、windows-1252 与 UTF-8 BOM 字符集） |
| `internal/driver/mtconnect` | MTConnect 主站（一条 `/current` 覆盖全设备、单点问题只降该点质量、`/probe` 失败退回 `/current`、多设备按 uuid/name 选择、基址子路径与 gzip、只读边界；自检 `ProbeDevices`、`Current`、`Sample`） |
| `internal/sim/mtconnect` | MTConnect 内置 Agent（三轴加工中心 / `/probe`、`/current`、`/sample` 序号窗口 / 两层条件渲染 / `toolOffset` 初始 UNAVAILABLE / gzip 与子路径 / 七类故障注入） |
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
| `internal/sparkplug` | Sparkplug B 生命周期状态机（主题 / BIRTH 与 DEATH / 节点级序号与回绕 / bdSeq / Rebirth 命令 / 指标映射） |
| `internal/mqtt` | 自写 MQTT 3.1.1 客户端（北向发布 + 南向订阅共用；QoS0/1、retained、LWT、PING 往返探测·单飞广播、连接代数 `Epoch`、每报文写超时） |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 |
| `internal/runtime` | 唯一编排层 + 看门狗 + 后端实现（含各 sink 装配） |

## 许可

待定（`docs/04-decisions/` 补充）。
