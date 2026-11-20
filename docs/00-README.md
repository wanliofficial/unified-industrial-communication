# 统一工业通信网关（UIC）｜文档与代码总索引

> **一句话定位**：一个用 Go 写的、可治理的工业数据采集底座——南向接协议，中间做建模与调度，北向出标准接口；重点不在「能连多少种设备」，而在「跑了三年、改过五十次之后，还能知道哪个点坏了、为什么坏、改坏了怎么退回去」。

| 项 | 数字 |
|---|---|
| 分析的参考方案 | 38 份（PA-01 … PA-38，其中 PA-06 合并了两个完全相同的仓库） |
| 文档总量 | 55 份 Markdown / 25,413 行（含 3 份 ADR） |
| 代码总量 | 387 个 Go 文件 / 171,057 行（其中测试 186 个文件 / 71,685 行 / 2,446 个测试函数） |
| Go 包 | 101 个（1 个入口 + 100 个内部包；98 个内部包有测试，`internal/mqtt/mqtttest` 与 `internal/sim/finsmem` 无独立测试文件） |
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

- 不是协议库大杂烩。当前实现了 31 个驱动（Modbus TCP / RTU / ASCII 主站、S7、OPC UA、MQTT 南向订阅、
  MC 3E、FINS、Host Link FINS、EtherNet/IP CIP、BACnet/IP、BACnet MS/TP、IEC 60870-5-104、IEC 60870-5-101、Mewtocol、DNP3、CoAP、KNXnet/IP、IEC 61850（MMS）、DL/T 645、
  CJ/T 188、M-Bus（EN 13757）、DLMS/COSEM（IEC 62056）、SL 651（水文监测数据通信规约）、Toledo（METTLER TOLEDO 连续输出）、SNMP v1/v2c/v3、ADS/AMS、SECS/GEM、MTConnect、计算点、virtual），外加
  Modbus 从站（TCP / RTU / ASCII）、二十六个内置模拟从站/Broker（Modbus TCP、S7、MC 3E、FINS、Host Link FINS、CIP、BACnet/IP、BACnet MS/TP、
  IEC 104、IEC 101、Mewtocol、DNP3、CoAP、KNXnet/IP、IEC 61850、DL/T 645、CJ/T 188、M-Bus、DLMS、SL 651、Toledo、SNMP v1/v2c、ADS/AMS、HSMS/SECS-II、MTConnect Agent、MQTT）与一个内置 OPC UA 服务端、一个零依赖报警引擎与一个有界历史采样日志；其余协议在
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
# 块服务（不需要起网关，直接连 PLC/模拟从站）：
./uic s7blocks -addr 127.0.0.1:15102                 # 七类块数量
./uic s7blocks -addr 127.0.0.1:15102 -type db        # DB 块号列表
./uic s7blocks -addr 127.0.0.1:15102 -type db -num 1 # DB1 属性（78 字节块信息）
./uic s7upload -addr 127.0.0.1:15102 -type db -num 1 -out db1.bin
```

示例配置覆盖 7 个点位：DB1 的 REAL/INT16/DINT32/STRING、DB2 的 WORD、
输入区 IW0、位地址 M0.3。模拟从站每秒改 DB1.DBD0，可用来观察 SSE。
`-pdu 240|480|960` 可改协商值，用来验证切块（240 时 40 个 float 会被拆成多帧）。

块服务（`uic s7blocks` / `uic s7upload`）刻意不进入采集调度：它用于备份、
比对与排障，一次可能传几 MB，与周期采集的延迟目标冲突。三条容易写错的协议
细节都已固化在 `codec/s7` 里：**ACK/ACK_DATA 是 12 字节头**（Job/UserData 是
10 字节，参数区从 12 而不是 10 开始）；**ListAll 的数据区是 `0A 00 0000`**，
不是其它查询用的 `FF 09`；**上传的长度是 5 位十进制 ASCII**，分片以 `param[1]`
判断是否最后一片。上传失败也会补一条 End——不释放 PLC 侧会话，反复重试几次
就可能把上传会话耗尽。

组态（模板实例化 + 点表 CSV 导入）：

```bash
# 模板库在 examples/templates/*.json（电表 / 变频器 / 温控器 / IO / 流量计 /
# 冷水机组 / S7 PLC / S7 产线 / OPC UA 机床 / MQTT 传感器 / 计算点 KPI）
./uic instantiate -template examples/templates/modbus-meter.json \
    -param index=1 -param unitId=3 -channel ch-modbus -out meter-1.json

# 点表 CSV：先预览，再写回整份配置
./uic import -csv points.csv -device press -channel ch-modbus \
    -config examples/modbus-loopback.json -dry-run
./uic import -csv points.csv -device press -config examples/modbus-loopback.json \
    -out config.new.json -replace
```

模板只做**整值替换**，不做算术：`{base + 2}` 不是合法占位符，需要偏移的点位
各自声明参数。`int`/`float`/`bool` 参数在「整个字面量就是一个占位符」时落成
JSON 数字/布尔。点表导入默认不覆盖已有点位：同 ID 内容不同即冲突，必须显式
`-replace`；`-dry-run` 与真写共用同一份合并结果。

三菱 MC（3E 二进制帧，TCP 5000）：

```bash
./uic sim -protocol mc -listen 127.0.0.1:5000 &   # 内置 MC 从站
./uic -config examples/mc.json
curl -s localhost:8087/api/v1/points
#   5 点：D0 uint16=1234、D1 int16=5678、D2 float32=48.0(CDAB)、M0 bool、M1 可写 bool
#   自检（不需要网关）：./uic mcread -addr 127.0.0.1:5000 -area d -offset 0 -qty 2
```

MC 3E 只做二进制帧 + TCP；ASCII/4E/UDP 形态配置期即拒。位软元件（M/X/Y/B…）按位
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

FINS 只做 FINS/TCP；FINS/UDP 与 Host Link 配置期即拒。建链先做**节点号协商**
（命令 0 → 1），`device.unitId>0` 覆盖目标节点、0 用协商值。`offset` 是**字地址**、
`bit` 是字内位号（`CIO0.3` → `offset=0, bit=3`）；字访问 2 字节/字，位访问
**1 字节/位**；32 位值低字在前（默认 CDAB）、64 位 DCBA；当前 EM 区无位访问码，
bool 走 RMW，要按位读就用 `em0`..`em12`。

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

BACnet/IP 只做 **ReadProperty（0x0C）/ WriteProperty（0x0F）**，不做广播发现、
不做 COV 订阅、不做批量读、不做分段。点位用 `kind=object` 寻址：
`objectType`（`analog-input` 或编号）、`instance`、`property`（`present-value` /
`object-name` / `units` 等）、`propIndex`（数组下标，0 表示不带）。
`present-value` 的编码由**对象语义**决定（模拟量 REAL、二进制 ENUMERATED、
多状态 UNSIGNED、字符串 CHARACTER STRING），不是由平台 `dataType` 决定；
可写性同样由对象决定——`analog-input` 的 `present-value` 永远只读。
UDP 无连接，驱动用 8 位 invoke ID 配对并把整条会话串行化，迟到应答直接丢弃。

IEC 60870-5-104（调度侧主站，TCP 2404）：

```bash
./uic sim -protocol iec104 -listen 127.0.0.1:2404 &   # 内置 IEC 104 从站
./uic -config examples/iec104.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8091/api/v1/points
#   14 点：短浮点 42.5/7.25、标度化 1234、被取代值（质量 uncertain）、
#         带时标值、单点 on/off、双点 1/2、累计量每秒自增、
#         单点命令 + 短浮点/标度化设定值 + 双点遥控（可写）
#   自检（不需要网关）：./uic iec104read -addr 127.0.0.1:2404 -ca 1
#   只验链路层：./uic iec104read -addr 127.0.0.1:2404 -probe
#   下发命令：./uic iec104read -addr 127.0.0.1:2404 -area double-command -cmd-ioa 2003 -write off
```

IEC 104 与前面所有协议的根本差别是**数据由从站推上来**：主站建链（STARTDT）后做一次
站总召唤拿快照，之后设备按变化主动上送；没有「读第 1001 号点」这种动作。因此驱动的
`Read` **不做任何网络交互**，只查本地最新值表（与 MQTT 南向同一模式），一次读多少点
的成本都一样。累计量不在站召唤范围内，`Open` 里固定再发一次电能量召唤；设备不支持时
它回否定确认，按调试日志处理，不影响连接。

质量码只来自两处：ASDU 里的品质位（`IV`→bad、`NT`→stale、`SB`/`BL`/`OV`→uncertain）
与链路是否活着（TESTFR 往返）。**刻意不做「多久没更新就降级」**——104 只在值变化时
上送，一个温度半小时不变是正常的，按时间降级会把整页刷成 stale。

点位用 `kind=register` 寻址，`offset` 即 IOA（1..16777215），`area` 取
`ioa` / `single-command` / `double-command` / `setpoint-float` / `setpoint-scaled` /
`setpoint-normalized` 之一。**命令类型由 `area` 声明而不是由 `dataType` 推导**：同一个
`float32` 可能是短浮点设定值，也可能是放大 100 倍的标度化整数。双点命令只接受 1（分）/
2（合），0 与 3 是标准里的「不允许」，本地即拒。同一个 IOA 可以挂两个点位
（`area=ioa` 读状态 + `area=double-command` 写命令），这是遥信/遥控共用编号的常见形态。

`iec104read` 不依赖点表：驱动维护一张 4096 条上限的**旁路表**，按 IOA 记录设备实际
送过的原始对象（类型标识、传送原因、是否带时标），所以初次接一台陌生设备就能先看清
「它送什么」再写点表。双点的 0/3 中间态原样显示成「不确定 (3)」，不硬转成布尔。

IEC 60870-5-101（串行远动主站，FT1.2，串口 / 串口服务器）：

```bash
./uic sim -protocol iec101 -listen 127.0.0.1:4001 &   # 内置 101 从站（FT1.2 over TCP 透传）
./uic -config examples/iec101.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8092/api/v1/points
#   14 点：短浮点 42.5/7.25、标度化 1234、被取代值（质量 uncertain）、
#         带时标值、单点 on/off、双点 1/2、累计量每秒自增（需电能量召唤）、
#         单点命令 + 短浮点/标度化设定值 + 双点遥控（可写）
#   自检（不需要网关）：./uic iec101read -addr 127.0.0.1:4001
#   只验链路层：./uic iec101read -transport serial -port /dev/ttyUSB0 -baud 9600 -probe
#   下发命令：./uic iec101read -addr 127.0.0.1:4001 -area double-command -cmd-ioa 2003 -write on
```

101 与 104 只在**应用层**相同：类型标识、传送原因、品质位、时标、命令格式是同一套，
本仓库把这一层抽成共享包 `internal/codec/iec60870`，两个驱动共用，避免「同一份点表走
串口和走网口行为不一样」。承载层是另一套物理事实：**只有主站可以发言**，从站被点名才
回答；没有会话，建链就是复位链路（固定帧 `FC=0`）；可靠性靠帧计数位 FCB + 链路确认 +
应用层激活确认，而不是 15 位收发序号。

因此 101 的 `Read` **真的去走线**——每轮请求第 1 级事件、按周期做站总召唤、再顺带请求
第 2 级，而不是像 104 那样只查本地表。总召唤不放在 `Open`（几百个点在 9600 上要好几秒，
塞进 `Open` 会让「连接超时」与「装置没数据」混在一起），由第一次 `Read` 触发。**电能量
召唤必须单独做一次**：累计量不在站召唤范围内，只发站召唤的实现现场表现为「电度点位永远
没有值」。终止报文有三种形态（`100`/`101` 的激活终止、`70` 初始化结束），但 `100` 的
**激活确认不能当结束**，否则总召唤只回来一个点。

FT1.2 有三个必须按字节记住的点：长度字段只计「控制域 + 链路地址 + ASDU」（整帧 = `L+6`）；
校验和从控制域累加到 ASDU 末字节；单字节帧没有地址也没有控制域（`E5` 肯定确认、`A2` 否定
确认，把 `A2` 当 `E5` 的症状是「装置拒绝了但主站报成功」）。链路地址宽度 0..2 字节，`0`
表示不带地址，此时固定帧只有 4 字节。

重传必须**原样重发同一帧字节**（含 FCB）而不是重新组帧：重新组帧等于发出另一条新命令，
从站会再执行一次，症状是「确认超时后开关被拉合了两次」。本实现保存整帧字节，超时后用
同一份重发一次；内置从站按「最近一次被接受的 FCB」去重，重复帧只回确认、不再执行。

写入有**两层确认**：链路层 `E5` 只说明报文到了，应用层激活确认（`cause=7`）才说明装置
收到并要执行。本实现等应用层确认才返回成功，`44..47`（未知类型/原因/公共地址/IOA）按
错误分类返回。写成功后用请求值回填本地缓存（质量 `uncertain`），只补「没有」或「比命令
更旧」的条目，避免抹掉一条更新的上送。

地址形态与 104 完全一致（`kind=register`、`offset` 即 IOA、`area` 取 `ioa` 与五种命令），
点表层用 `WidthNone`：远动协议的值宽度由类型标识决定，地址里写 `length` 反而是误配。
**同一个 IOA 可以同时挂遥信与遥控**（`ioa:2003` 布尔 + `double-command:2003` 可写），
两套语义、两种平台类型，所以缓存按**点位 ID** 而不是 IOA——按 IOA 存会让后写入的一方
覆盖另一方，症状是「布尔遥信显示成 double 的 2」。这条修正同时改到了 104。

剖面全部走设备标签：链路地址 1（1 字节）、CA 1（取 `unitId`）、IOA 2 字节、ORG 0 字节；
可用 `linkAddress` / `linkAddressSize` / `commonAddress` / `commonAddressSize` / `ioaSize` /
`orgSize` / `interrogate` / `class2` 覆盖。把某一组当成标准写死，是接不同厂家装置时最先
撞上的墙：帧能收发、校验也对，只是所有字段整体错位一到三个字节。四类报文（总召唤、电能
量召唤、时钟同步、初始化结束）**没有信息对象地址**，由共享层的 `HasIOA` 统一表达，编码
与解码两侧走同一个判断，并用黄金字节钉住。

`iec101read` 与 `iec104read` 一样不依赖点表，但诊断口径更贴串行现场：串口远动的失败模式
几乎全是「没有应答」，所以它把「卡在哪一层」直接打出来——帧计数、解析失败帧数、公共地址
不符帧数、每轮新增/更新数。

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
是那些「看起来是十进制」的位置。一帧里混着三种进制：站号两位**十进制**（16 号站写
`16`）、数据区地址五位**十进制**（DT100 → `00100`）、接点编号是「三位十进制字 + 末位
**十六进制**」（X1F → `001F`，X16.3 → `0163`）、定时器接点四位**十进制**（T10 →
`0010`）、数据内容每字节两位十六进制且**字内低字节在前**（H0063 → `6300`）、BCC 两位
十六进制且**包含帧头 `%`**（`%01#RCSX0000` → `1D`）。

任何一种读错都不会报格式错：PLC 会回一份别的地址的数据，值有、质量好、对应关系全错。
所以点表里不让人写 `X1F`，而是写 `x:1.15`（字地址 + 位号两个十进制整数），把进制混排
关在编解码层里。帧边界只有帧头的 `%` 与结尾 CR，半双工总线上一份迟到的应答如果被当成
下一轮首部，症状是「数据永远滞后一轮」，所以收帧层强制首字节是 `%`/`<`，其余逐字节
丢弃并在 `Dropped()` 里计数。

读写受**不同**的帧长限制：读请求定长 20 字节、应答 `9+4n`；写请求 `20+4n`。118 字符上限
下读能到 27 字、写只能到 24 字，取较小者 `MaxWords = 24`。接点按点读写（`RCP`/`WCP`）
的上限是 8，来自正文「个数」只有一位字符，不能靠拆帧绕开。

三条刻意的边界：**布尔量只走 `WCS` 单点写**（「读字→改位→写字」是非原子写，会把同字
邻位一起覆盖）；**字区上的布尔点位可读不可写**（协议没有位写命令，驱动返回
`unsupported` 并提示改成 `uint16` 按整字写）；**T/C 接点不走按字读**（`RCC` 会把它们按
「每字 16 个接点」重新编号，与 T/C 自身的十进制编号是两套体系，驱动只走 `RCS`/`RCP`，
内置从站仍按真实语义实现按字读）。

承载有两种：串口（出厂 9600/8/**奇**/1，改成无校验的典型症状是「能发不能收」）与串口
服务器 TCP 透传（端口各厂不同：2000、60001、9094、FP7 编程口 32769..32772，默认 2000）。
TCP 承载**只发裸帧**：FP7 的 LAN 口若设成「与 FP2-ET-LAN 连接」，帧外会多一层 16 字节
头，表现是「发出去没有任何应答」；正确做法是在 PLC 侧改回默认模式，而不是给驱动加一个
「猜封装」的开关。

错误码保留手册的四类（链路 / 基本过程 / 处理 / PLC 应用），再映射到驱动的错误分类：
`60/61/66/67` → `address`（点表或型号配置问题，重试无用）、`65` 写保护与 `63` 模式错 →
`permission`、`40..43`/`53` → `protocol`（可重试）、`23` 站号重号 / `24` 硬件错 / `26`
站号设置错不可重试。

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
读写语义、数值字节序与 FINS/TCP 逐字节相同，差别只在「怎么把一帧发到线上」。所以命令
语义抽进 `driver/finscore`（主站）与 `sim/finsmem`（从站内存表）与 FINS/TCP 共用，同一份
点表换个 `protocol`（`fins` ↔ `hostlink`）就能跑。

帧的形状要按字节记住六件事：等待时间只有**一位**十六进制（0..F，单位 10ms）；应答在
`FA` 后**多一格结束码**、少一格等待时间（收发两条独立布局）；FCS 从 `@` 起异或到正文
末尾、不含 `*` 与 CR；正文是「一字节两字符」的十六进制，整帧纯可打印 ASCII；单元号是
两位数**十进制**（00..31）；直连 ICF 是 `0x00`（不是 FINS/TCP 惯用的 `0x80`）。

**单次上限读 269 字、写 267 字**（不是 FINS/TCP 的 999）：命令不能在信封上分段，上限
由整帧字符预算反推——读看应答帧能带多少数据，写看命令帧能塞多少正文，读写差两个字是
结构性的。照抄 999 会稳定收到结束码 `0x18`（帧长错）。

**串口出厂参数是 9600-7-E-2**，与 Mewtocol 的 9600-8-O-1 正好相反。7 位数据位够用是
因为整帧是可打印 ASCII；把 FINS/TCP 用户的 8/N/1 套过来会「发得出去、收不到东西」，而且
不报错。错误分两层：信封层结束码（`0x13` FCS 错 / `0x14` 格式错 / `0x18` 帧长错，修一次
永久消失）与 FINS 层结束码（与 FINS/TCP 同一张表）。

两条刻意的边界：**只做 FINS 帧（`FA`），不做 C 模式**（那是另一套命令集与 BCD 寻址，
混进来每个字段都要先判断走哪套）；**不做跨网长格式，也不自动重试**（跨网多 12 个路由
字段，目标由单元号承载；自动重发一次写可能把同一个字写两遍，对「加计数」点位是真实
数据错误）。

DNP3（IEEE 1815 调度侧主站，TCP 20000）：

```bash
./uic sim -protocol dnp3 -listen 127.0.0.1:20000 &   # 内置 DNP3 从站（链路地址 1）
./uic -config examples/dnp3.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8098/api/v1/points
#   13 点：二进制输入（类 1，每秒翻转）、双点 1=分 / 2=合、计数器（类 3，每秒自增）、
#         模拟量 42.5/7.25/-3.5/1234（类 2）、
#         二进制输出（CROB）+ 模拟量输出（组 41）（可写）
#   自检（不需要网关）：./uic dnp3read -addr 127.0.0.1:20000
#   只验链路层：./uic dnp3read -addr 127.0.0.1:20000 -probe
#   直接控制：./uic dnp3read -addr 127.0.0.1:20000 -binary 0=on -analog 0=12.5
```

DNP3 是三层协议，且三层各有各的确认：链路层（`05 64` 起始、每 16 字节数据一块 CRC、
主站与从站各自有链路地址）、传输层（应用报文分片，首字节带 FIR/FIN/传输序号）、应用
层（功能码、对象头、限定词、IIN）。三层任何一层错位都表现为「TCP 连上了但读不到值」，
所以 `Open` 分三步：复位链路并等链路 ACK → TEST_LINK 探活 → 用一条 READ 同时请求类
0/1/2/3 完成首次总召唤。**每一步都有超时**，对端只接受连接不说话时会在有限时间内
失败并报 timeout，而不是挂死。

与 IEC 104 同样，数据由从站推上来：`Read` 只查本地最新值表，不做网络交互。总召唤
（默认 5 分钟）刷新静态值，事件轮询（默认 1 秒）拉类 1/2/3。**带 CON 位的应用响应必须
回一条 application confirm**，否则从站把事件留在待确认队列里反复重发，最终撑爆它的事件
缓冲区——现场「刚开始正常、几小时后全是旧值」大多是这个原因。驱动在收到带 CON 的响应
和未请求响应（UNSOLICITED）后都立即回确认。

质量映射只看低 6 位：`ONLINE=0` 或 `COMM_LOST=1` → bad；`RESTART`、强置、越限、参考
误差 → uncertain；其余 good。**二进制量的 bit7、双点量的 bit6-7 是过程状态而不是品质位**，
必须先屏蔽——否则所有处于「合」位的开关都会被判成 uncertain，变成「全站不可信」的假
故障。双点量按 bool 映射：1（分）→ false、2（合）→ true、0/3（中间态）→ false +
uncertain，绝不把「未知」显示成「已关闭」。

点位用 `kind=register` 寻址，`offset` 是对象索引，`area` 取 `binary-input` /
`double-bit-input` / `binary-output` / `counter` / `analog-input` / `analog-output`
（也接受 `bi`/`dbi`/`bo`/`ctr`/`ai`/`ao`）。二进制输出用 CROB（组 12，锁存开/关），
模拟量输出用组 41；两者都等从站的应用响应并检查回显状态——**只写 socket 不检查回显
会把拒绝当成成功**。不做选择-执行（SBO）两段式、不做安全认证、不做文件传输、不做
时间同步写路径、不做串口：这些都需要额外的会话状态或证书管理，做成半成品比不做更危险。

`dnp3read` 也不依赖点表：驱动旁路表按「区 + 索引」记录设备实际送过的每个对象（组、
变体、原始品质位、时标），初次接陌生设备时先看清「它到底送哪些对象」再写点表。带
`-raw` 会把组/变体与原始 flags 一起打出来，便于和生产方对账。

CoAP（UDP 资源模型，端口 5683）：

```bash
./uic sim -protocol coap -listen 127.0.0.1:5683 &   # 内置 CoAP 服务端
./uic -config examples/coap.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8099/api/v1/points
#   12 点：纯文本温度/湿度、JSON 字段（temperature/ok）、可写阀门与设定值、
#          POST 触发器、bytes 资源、3000 字节大资源（Block2）、分离响应资源
#   自检（不需要网关）：./uic coapread -addr 127.0.0.1:5683 -discover
#   只验链路：./uic coapread -addr 127.0.0.1:5683 -probe
#   写设定值：./uic coapread -addr 127.0.0.1:5683 -path /actuator/setpoint -write 42
#   写 JSON 字段：./uic coapread -addr 127.0.0.1:5683 -path /config \
#                   -selector mode -type string -content-format json -write manual
```

CoAP 的数据模型是 **URI 资源**：点位用 `kind=symbol`，`path` 是资源路径，`selector`
是 JSON 文档里的字段（`a.b` / `a[0]`），`address.length` 限制 `string`/`bytes` 的
接收字节数。同一个路径可以挂多个点位，驱动逐点 GET，不做响应缓存。

读取按响应的 `Content-Format` 解码，不猜：纯文本走与 MQTT 共用的文本规则；JSON 按
selector 取字段后再按点位类型解码；`application/octet-stream` 只交给 `bytes`/`string`；
CBOR 等其它格式明确报「不支持」。没有 `Content-Format` 时按纯文本处理。大资源用
Block2：首个请求默认不带选项、由服务端决定块大小，驱动按「块号 × 块大小 == 已收字节
数」续读；服务端对 Block2 回 `4.02` 时退回不带选项再取一次。重组上限 8 MiB。

重传是稳定性的核心：`ackTimeout`（默认 2s）逐次翻倍、`maxRetransmit`（默认 4，0 表示
只发一次），等待窗口永远不越过设备级 `timeout`。空 ACK 表示「请求已接受、别重发」，
此时只等分离响应；分离响应是 CON 就回空 ACK。与本事务无关的 CON 回 RST 让对方停止
重传，垃圾报文只计数、不让当前请求失败。写入用 PUT（默认）或 POST；**PUT 是替换整个
资源**，带 selector 时只发 `{selector: value}` 子树，数组下标写入直接拒绝。不做 DTLS、
不做 Observe 订阅、不做 Block1 分块发送——`Subscribe` 在能力声明里是 `unsupported`。

KNX（楼宇总线，UDP 隧道 3671）：

```bash
./uic sim -protocol knx -listen 127.0.0.1:13671 &   # 内置 KNX 网关
./uic -config examples/knx.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8099/api/v1/points
#   7 点：两路开关、温度/湿度（2 字节浮点）、调光百分比、功率（4 字节浮点，
#         每秒 +10）、有符号计数
#   自检（不需要网关）：./uic knxread -list
#   只验隧道：./uic knxread -addr 127.0.0.1:13671 -probe
#   读温度：  ./uic knxread -addr 127.0.0.1:13671 -group 2/1/1 -dpt 9.001 -raw
#   开灯：    ./uic knxread -addr 127.0.0.1:13671 -group 1/1/1@1.001 -write on
```

KNX 的地址是 **组地址**（`主组/中间组/子组`），但组地址在总线上**不携带类型**：
同一个 `2/1/1` 在 A 工程里是 1 位开关、在 B 工程里是 2 字节浮点温度。因此点位地址
必须把数据类型（DPT）写进去——`"path": "2/1/1", "dpt": "9.001"`，命令行对应
`-group 2/1/1 -dpt 9.001` 或内联的 `-group 2/1/1@9.001`。

这不是形式主义：`dataType: float32` 反推不出 DPT（既可能是 2 字节的 9.xxx，也可能是
4 字节的 14.xxx，编码方式完全不同），猜错的代价不是报错，而是**一个合法的错值**
——21.5 会被解成 `3.7e-41`，质量还标着 good。缺 DPT 在 `uic import` 阶段就被拒。

驱动只做 **TUNNELING（UDP 单播隧道）+ 组地址读写**。隧道建在首次 `Probe`/读写时，
`Open` 只做 UDP dial——这样「网关隧道槽位满」这类几分钟后会自愈的状态不会被判成
配置错误。序号是 8 位，**收到网关确认才加一**，超时重传用同一个序号，序号错乱时
重建隧道（不猜）。写操作在 300ms 窗口内等链路层确认：**收不到按成功**（部分网关
不回），收到且标了故障才判失败——而那个故障位表示**总线级**发送失败，不是「这个
组地址有没有人在听」。

IEC 61850（MMS over TCP，站控层读写）：

```bash
./uic sim -protocol iec61850 -listen 127.0.0.1:10202 &   # 内置 IED（域 simpleIO）
./uic -config examples/iec61850.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8102/api/v1/points
#   8 点：LLN0.ST.Health、GGIO1.ST.Ind1/Ind2、GGIO1.ST.Str.Pos、
#          GGIO1.MX.PhV.mag.f / A.mag.f（32 位浮点）、
#          GGIO1.CO.SPCs.stVal（可写）、GGIO1.SP.SETV.setMag.f（可写）
#   自检（不需要 IED）：./uic iec61850read -list
#   只验关联：./uic iec61850read -addr 127.0.0.1:10202 -probe
#   读点位：  ./uic iec61850read -addr 127.0.0.1:10202 \
#               -object simpleIO/GGIO1.MX.PhV.mag.f -type float32 -raw
#   写点位：  ./uic iec61850read -addr 127.0.0.1:10202 \
#               -object simpleIO/GGIO1.CO.SPCs.stVal -type bool -write true
#   浏览模型：./uic iec61850read -addr 127.0.0.1:10202 -browse -domain simpleIO
```

这是本仓库里封套最长的一条链路：TPKT（RFC1006）→ COTP → 会话层 → 表示层 → ACSE →
MMS。两层细节必须分开处理，否则**短帧能过、长帧错位**：

- **会话层 LI 与 BER 长度是两套编码。** `0x00..0xFE` 是单字节长度；`0xFF` 后面跟
  **恰好两字节**大端长度，与 BER 长形式（后面跟几个字节由低 7 位决定）不是一回事。
- **封套归属不对称。** COTP 的 CR/CC 自带 TPKT，可以直接发；会话的 CN/AC/DT/FIN
  与所有 MMS 请求都要先套「会话 DT + 表示层」，再由 COTP 补 TPKT 与 COTP 头。

地址是对象引用 `LD/LN.FC.DO[.DA]`（`simpleIO/GGIO1.MX.PhV.mag.f`）。**FC 是名字的
一段而不是元数据**：`GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal` 是两个不同对象。
点表要么写全 FC，要么由 `labels.defaultFC` 补；驱动不从数据属性名反推 FC——那需要
完整 SCL 模型，而驱动手上只有一份点表。

类型在装置侧：一个对象是布尔、整型还是浮点由 IED 的 MMS 模型决定，读回来才知道。
所以地址策略是 `WidthNone + AllowSymbol`、点表**不写 length**，`dataType` 只声明
平台侧怎么存。把 `MX.PhV.mag.f` 标成 `int32` 不会读成整数，只会把浮点按整数入账。

一条关联上可以有多个在途请求，靠 invokeID 分回对应关系；驱动严格配对，迟到的响应
按 invokeID 丢弃，绝不当作当前请求的答案（不校验就会在「A 超时、B 复用链路」时把
A 的值写给 B）。整条关联一把锁串行化——MMS 是请求/应答协议，并发只有乱序风险。

单项数据访问错误不是网络错误：对象不存在/未定义/地址无效/类型不支持 → `address`，
访问被拒 → `permission`，暂时不可用/硬件故障 → `protocol` 且可重试。模型浏览
（`getNameList`）协议支持，但 `Browse` 能力位诚实声明为 `unsupported`：整棵模型
枚举是一次性运维动作，不是采集路径；自检命令走包级 `Browse` 按页收齐。

边界：只做 ACSE + MMS 初始化 + 读/写变量 + 模型浏览；不做 GOOSE/SV、报告控制块、
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
#   写结算日：... -di 04000B01 -write "11-07 12:00"（4 字节日期，MM DD hh mm 字段序，不是小端整数）
```

645 的地址是**数据标识（DI）**而不是寄存器偏移：`address.area` 写 8 位十六进制（手册
顺序 `DI3DI2DI1DI0`，`02010100` = A 相电压），`offset` 恒为 0，`length` 通常省略——
宽度（电压 2 字节、电流 3 字节、电能 4 字节）由 DI 表决定。点表层为此引入
`WidthProtocol`，不在点表里复制 DI 表，也让「长度与类型宽度不符」的告警不会在表计
协议上刷屏。数据块（DI 含 `FF`）只按 `bytes` 交付：把 15 个 4 字节项的串联硬解成
`float64`，会给出一个看起来像读数、实际是前 8 字节的假值。

协议一次请求只能带一个 DI，因此驱动**逐点串行**、`MultiRead` 诚实声明 `unsupported`。
一轮采集耗时 = 点数 × 单次事务；300 个点位挂在 2400bps 总线上要几十秒，这个事实必须
让配点表的人看见。链路级故障（超时/断开）后本轮剩余点快速失败，不逐点等超时。数据块
超长时在同一 `Read` 内跟完后续帧（上限 64 帧）：截断的电能读数会以「少了几位」的形式
入账，看不出任何异常。

写命令四道闸门：`allow-write`、`allow-control`（拉合闸/清零）、DI 表可写、设备密码。
它们是「与」关系。广播校时/广播冻结连入口都不提供——一条配错的广播会同时改掉总线上
所有表的时间。

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
起止符——按帧长算会在每个长帧上固定差 3，这是这个协议最容易写错的一处。四种帧共用两个
起始符（`0x68` 既是长帧头也是控制帧头），而 `E5` 只有 1 字节：串口服务器把 ACK 与紧随的
应答粘在一次 read 里返回时，扫描器必须能先把 `E5` 单独切出来，否则「链路正常」会被判成
「帧结构错」。

可变数据结构（CI=`72h`）是 12 字节固定头（表号 BCD + 厂商 + 版本 + 介质 + 访问号 +
状态 + 签名）之后的一串**自描述记录**：每条记录先有 DIF（+DIFE 链）说明长度、功能位
（瞬时/最大/最小/故障）与存储号/费率/子单元，再有 VIF（+VIFE 链）说明物理量与 10 的幂，
最后才是数据。没有「第 n 个寄存器」这种东西——偏移取决于前一条记录的长度，换一块表
就会错位。这是点表模型必须长出符号寻址的真实原因，而不是设计偏好。

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
变长（`DIF=0Dh`）带 LVAR：LVAR 同时编码长度与形态，`C0..CF`/`D0..DF` 是正/负 BCD 数
（符号由段给出，不是末字节 `Fh`）、`E0..EF` 是二进制整数、`F0..FA` 是浮点（标准标
"to be defined"，按原始字节暴露）。类型 G 日期（2 字节）与类型 F/I 日期时间（4/6 字节）按位域解，
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

边界：只做**主地址**寻址（0..250），251 保留、253 次级、254 网络层、255 广播都在配置期
拒绝。次级地址需要先 `SND_UD(CI=52h)` 选中且地址域变 8 字节、校验范围随之变化，混合
实现比不支持更难排查，因此编解码层显式拒绝 `FDh` 地址。不做 `SND_UD` 写入（CI=`51h`
发数据 / `5Eh` 改地址 / `50h` 校时）：写什么完全由表厂剖面决定，写错会改变计费参数，
改地址/速率还会让表从当前站号「消失」——`Write` 诚实声明 `unsupported`。固定数据结构
（CI=`73h`，16 字节、两个计数器）能解但不推荐使用，它是老式紧凑表的形态。

承载支持 `serial`（真实 RS-485，走共享串口层总线级互斥）与 `tcp`（串口服务器/DTU
透传，锁退化为每连接互斥）；两者分帧与命令序列完全相同，所以内置 TCP 从站能直接当
自检目标。串口协议 `concurrency` 必须为 1。

DLMS/COSEM 电表（IEC 62056-46/-53/-62，LN 逻辑名引用 + 无安全，HDLC 三层承载）：

```bash
./uic sim -protocol dlms -listen 127.0.0.1:4051 &   # 内置三相电表从站（逻辑设备 1，客户端 16）
./uic -config examples/dlms.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8095/api/v1/points
#   20 点（15 启用）：正向/反向有功电能、无功电能、三相电压/电流、总功率、功率因数、
#          频率、表号、逻辑设备名、固件版本、厂商、电表时钟（另 5 点是语法示例）
#   只验链路：./uic dlmsread -addr 127.0.0.1:4051 -probe
#   默认点表：./uic dlmsread -addr 127.0.0.1:4051
#   指定 OBIS：./uic dlmsread -addr 127.0.0.1:4051 -obis 1.0.1.8.0.255:3:2
#   试读全表：./uic dlmsread -addr 127.0.0.1:4051 -scan
#   内置 OBIS 表：./uic dlmsread -list
#   串口实机：./uic dlmsread -transport serial -port /dev/ttyUSB0 -baud 9600 -address 1
```

DLMS 是唯一「地址是标准固定的六段码」的协议：`1.0.1.8.0.255` 在任何厂商的表上都是
正向有功电能。这是点表能离线书写、跨表复用的原因，也是本驱动只做 **LN 引用**的理由
——SN（short name）引用要厂商在 `object_list` 里分配编号，同一个 OBIS 在不同表上编号
不同，必须先「发现再寻址」，与「点表直接写地址」的模型不兼容。

点表 `path` 是 `<OBIS>[:<class>[:<attribute>]]`。只写 OBIS 时类号与属性号从内置表推导；
**查不到就报 `ErrAddress` 而不是猜**——猜错的类号会让表回 `object-class-inconsistent(9)`，
而「猜了一个看起来能读的类」会读到另一个对象的合法数字，且数值一切正常。

| 写法 | 含义 |
|---|---|
| `1.0.1.8.0.255` | 按内置表推导（Register(3) / attribute 2） |
| `1.0.1.8.0.255:3` | 显式类号，属性号默认 2（value） |
| `1.0.1.8.0.255:3:3` | 显式类号与属性号（这里是 scaler_unit） |
| `1-0:1-8-0-255:3:2` | 标准正文写法：OBIS 内部自带冒号，必须能正确切分 |

HDLC 承载有四件事必须一次做对：**① 长度字段是两标志之间的全部字节数**（含 2 字节格式
字段与末尾 FCS，不含标志），整帧 = `len(body)+2`，11 位跨两字节且 bit11 是分段位；
**② HCS 覆盖 `format+dest+src+ctl`**，所以必须先算长度再算 HCS——先占位后回填会让头
校验与格式字段不一致，接收方判定「头校验失败」并丢弃整帧，症状是第一帧之后就超时；
**③ 类型 3 帧不做透明转义**，`0x7E` 可以合法出现在 info 与 FCS 里，分帧必须按长度字段
定界而不是扫标志——按标志扫会在某些应答上把一帧切成两半，而且只有特定数值才触发
（本仓库用 `1.0.13.7.0.255` 的 FCS `7E 64` 做回归）；**④ 地址域是 7 位/字节 + 扩展位**，
1/2/4 字节，客户端恒 1 字节，4 字节按「高 7 位组 + 低 7 位组」各左移一位。

应用层是 A-XDR（IEC 61334-6）编码的 APDU。建链 `SNRM → UA`、`AARQ → AARE`，读
`Get-Request-Normal → Get-Response-Normal`，断链 `DISC → UA`。`SNRM` 的金帧是
`7E A0 07 03 21 93 0F 01 7E`，AARQ/AARE 的逐字节向量在测试里锁死。**U 帧控制域是固定
常量、不携带可变的 P/F 位**（UA=0x63 的 bit4 为 0），要求往返保持 P/F 会让建链永远等
不到应答。LLC 头 `E6 E6 00`（请求）/ `E6 E7 00`（应答），`E7` 是「这一帧是应答」的唯一
信号；缺了它说明收到的是服务端主动上报（本驱动不做，报协议错）。

**scaler 是 DLMS 最容易写错的一处**：Register 类（3/4/5）的 attribute 2 是原始整数，
attribute 3 是 `structure {integer scaler, enum unit}`，工程值 = 原始值 × 10^scaler，
**scaler 为负是除**。驱动读完 attribute 2 再读一次 attribute 3 并应用，带进程内缓存
（scaler 是对象的静态配置参数，不会随测量值变化）。`ApplyScaler` 必须**一次**乘除
`10^|scaler|`：逐次除 10 会累积舍入误差，`5001 / 10 / 10` 得到 `50.010000000000005`，
屏幕上就是一串尾数且会污染聚合值。单位枚举同样只认标准表：**Wh = 29、VAh = 30、
varh = 31**——写错不会报错，只会把电量标成「视在电能」，数值一切正常，最难发现。

`data-access-result` 在 A-XDR 下是 `[1] IMPLICIT ENUMERATED`，定长 8 位、**无长度
前缀**，线上只有 `01 <值>` 两个字节。不要「兼容」`01 01 xx` 那种带长度的写法：结果码为
`hardware-fault(1)` 时两者前两字节完全相同，兼容会把它读成「数据不完整」，于是所有硬件
故障都被报成超时。宁可只认标准形态，让非标实现明确报错。

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
标准里的批量机制 `get-request-with-list` 要求服务端声明 `multiple-references`（现场极少
支持），因此 `MultiRead` 声明 `unsupported`——把「N 个点 = N 次往返」这个事实暴露给配置
的人，比假装合并更负责任。300 个点挂 9600bps 串口，一轮要十几秒。

`Probe` 打印的是**协商后**的 conformance，不是本端请求的那份：标准允许服务端砍掉一半
能力，按请求值判断会得出错误结论。物理层固定 **8N1**（IEC 62056-46 的 HDLC profile），
配成 8E1 会收到全错位的字节——若驱动发现「有字节回来但一帧都没成」，会报协议错误并提示
查波特率/校验位与终端电阻，而不是笼统报超时。

水文遥测站 SL 651（《水文监测数据通信规约》SL 651—2014，中心站侧查询 + 应答）：

```bash
./uic sim -protocol sl651 -listen 127.0.0.1:4001 &   # 内置遥测站（站号 31234567）
./uic -config examples/sl651.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8097/api/v1/points
#   9 点（5 启用）：瞬时水位 Z=0.127m、1h 时段雨量 P1=0.5mm、累计雨量 PT=123.4mm、
#          瞬时流量 Q=1234.567m³/s、电源电压 VT=12.1V
#   只验链路：./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -probe
#   整帧要素：./uic sl651read -addr 127.0.0.1:4001 -station 31234567
#   按点表读：./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -points Z,PT,VT
#   批量要素：./uic sl651read -addr 127.0.0.1:4001 -station 31234567 -points DRP,DRP[3]
#   要素表：  ./uic sl651read -list
```

SL 651 与其它表计协议（645 / 188 / M-Bus / DLMS）的差别不在字段多少，而在三条「凭印象
写就会当场错」的实现事实：

**① 分帧不能扫标志字节。** 帧以 `7E 7E` 开头，但正文与 CRC 里可以合法出现 `0x7E`——
本仓库金向量的第一帧正文首字节就是 `7E`。任何「读到 `0x7E` 就当帧尾」的写法都会把一帧
切成两半；只能先读长度字段再按长度收，整帧 = **17 + 长度**。

**② 长度字段数的是「报文起始符之后、结束符之前」的字节数**，不含 STX、不含 ETX/ETB、也
不含 `7E 7E`；高 4 位是上下行标识（0000 上行 / 1000 下行），低 12 位才是长度。M3 多包
时「包总数 + 包序号」那 3 字节**计入**长度（规约这么定，不是实现选择）。

**③ CRC 是 CRC-16/MODBUS 变体，但结果不取反。** 初值 `FFFF`、右移、多项式 `A001`、
**xorout 0000**，覆盖 `7E 7E` 起至结束符止，**高位字节在前**。仓库里其它协议多用 X-25
（结果取反）且低字节在前，照抄会得到 `3C51` 而不是 `C3AE`。链路维持报的完整金帧：

```
7E 7E 00 9876543210 1234 2F 0008 02 7E08221011100747 03 C3AE     (25 字节)
                                               ↑ 正文首字节就是 7E
```

还有一条只看上行帧永远发现不了的差别：**上下行帧的地址字段顺序相反**（表 11 上行
2=中心站/3=遥测站，表 12 下行 2=遥测站/3=中心站），密码、功能码、长度字段的偏移不变。
只做被动收包的实现看不出问题，一旦主动发查询就会把站号写进中心站字段。

站号是 **5 字节 BCD，装 10 位十进制**。现场测站编码普遍是 8 位（如 `31234567`），所以
驱动按 `0..9999999999` 校验而不是按「5 位数」——按 5 位卡会把合法站号判成配置错误。

正文是**要素信息组**，不是固定偏移的寄存器：每个要素自带「引导符(1) + 数据定义字节(1)」，
数据定义字节 = `(字节数 << 3) | 小数位数`。报文自己说明每个值有多宽、有几位小数，接收方
不需要预先协商点表格式。附录 C 表 C.1 的 131 项可解释引导符（水位 Z、时段/累计雨量
P1/PT、流量 Q、电压 VT、闸位 GH、含水率 M10…）就是协议的数据模型本身，`tables.go` 把它
写死在代码里——同一台终端送来的 `39H` 在任何工程里都是「瞬时河道水位、潮位」，做成配置项
只会让同一份报文在不同站点解出不同含义。

例外是「引导符类」：观测时间（`F0H`）、测站编码（`F1H`）、人工置数（`F2H`）、图片
（`F3H`）、批量雨量/水位（`F4H..FCH`）的数据定义字节被规约**写死**成固定值，宽度**不能**
从高 5 位读出来——按高 5 位读会把「观测时间 5 字节」当成「30 字节」，当场错位。

**DRP / DRZ 是最容易解错的一类。** `F4H`（1h 内每 5min 时段雨量）在报文里是**一个 12
字节数据**，语义上却是 12 段 × 1 字节；`F5H..FCH`（相对水位）是 24 字节 = 12 段 × 2 字节。
把它们当一个 HEX 整数解会得到 `1844674407370955300` 这种溢出值——**不报错、值域看着像
大数**，是最危险的一类错。本仓库用 `Guide.Slot` 显式声明段宽（DRP=1、DRZ=2），点表地址
写成 `<要素>[<段号>]`（`DRP[0]`..`DRP[11]`，段号从 0 开始），段号只能用在批量要素上——
`Z[1]` 报地址错，而不是让 `SlotNumber` 返回「越界」。

DRP/DRZ/ZT 的数据是 **HEX 整数不是 BCD**：DRP 1 字节 × 0.1mm（`0x19` = 2.5mm），DRZ 2
字节 × 0.01m（`0x01F4` = 500cm = 5.00m）。按 BCD 解 `0x01F4` 得到 194，正好落在「看起来
合理的 194cm」上——这类错值比报错难查，所以换算按引导符分派而不是按数据定义字节。ZT
（遥测站状态及报警信息）是 4 字节 HEX 状态字，没有尺度，按原始 HEX 输出。

缺测的表示是「与数据位数一样的 `F`」，不是 9999。驱动把它降级为 `quality=bad` 而不是报
地址错误——规约允许某个时段没有数据；当成 9999 解出来会直接污染数据库。

查询侧：中心站发 `37H` / `38H` / `3AH`，**应答沿用查询的功能码**（不是另有一套应答码）。
这一点必须和自报帧分开判：`2F/30/31/32/33/34` 是遥测站主动上送，而 `37/38/3A` 既有下行
查询也有上行应答，方向只能从长度字段高 4 位看。读路径按「一次查询取整帧全部要素」设计
——37H 的应答与 32H 定时报同形，逐点发 37H 不但慢，还会让同一设备的点位落在不同快照上。

电压 `VT`（`38H`）在报文里**独立编报**，位于要素组之后，不是要素组的一项。codec 因此把
它放在 `Report.Voltage` 而不是 `Report.Fields`——少这一处兜底，`VT` 点位会「永远读不到」，
而 CRC 与其它要素都正常，极难发现。

边界：只做中心站侧查询与上行报文解析。不做 ASCII 字符编码帧（长度字段是 4 个 ASCII
字符、数值是文本，分帧/校验/宽度全不同，遇到返回 `ErrUnsupported`）；不做 M3 多包重组
（重组要会话状态机，本包只如实标出 ETB「后面还有包」）；不做配置表读写（`40H~43H`）、
控制命令（`4CH~4FH`）、人工置数（`35H`）、图片报（`36H`），`Write` 声明 `unsupported`；
不做 `FFXXH` 自定义扩展（`XX` 的含义只有设备方知道，报 `ErrUnknownGuide`）。

SNMP v1/v2c（交换机 / UPS / 服务器 / 带网管的仪表，UDP 161）：

```bash
./uic sim -protocol snmp -listen 127.0.0.1:16161 &   # 内置 SNMP v1/v2c 代理
./uic -config examples/snmp.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8095/api/v1/points
#   9 点：sysDescr/UpTime/Name/Services、可写 sysContact/sysLocation、
#          ifNumber、ifInOctets、ifAdminStatus
#   自检（不需要网关）：./uic snmpread -addr 127.0.0.1:16161 -probe
#   遍历子树：./uic snmpread -addr 127.0.0.1:16161 -walk 1.3.6.1.2.1.2.2.1
#   写 sysLocation：./uic snmpread -addr 127.0.0.1:16161 \
#       -oid 1.3.6.1.2.1.1.6.0 -set lab-2 -setType string
```

SNMP 是唯一「按对象名寻址」的协议：点位的 `address.kind` 是 `symbol`，`path` 是点分十进制
OID，实例号写在 OID 里（标量 `.0`、表项 `.10.1`）。读用一条 GET 带多个 varbind，
`MultiRead` 声明 `supported`；但一帧受 `maxOids`（默认 32）与 1400 字节编码预算双重约束——
超过 MTU 的报文会被分片，而现场大量设备与防火墙直接丢分片，症状是「OID 少时能读、点表
一多就超时」。v1 没有 per-varbind 异常，批量 GET 遇 `noSuchName` 时驱动逐点重发，否则一个
坏 OID 会让同批所有点位变成地址错误。

写是单 OID SET（不合并多 varbind，避免一条只读对象回滚整批），`uint16/uint32` 上线为
Gauge32、`int*` 为 INTEGER、`string/bytes` 为 OCTET STRING；float/BCD 显式拒绝（没有标准
线类型，私有编码互不兼容）。应答同时校验错误码与回显值——设备「接受但按自己的解释改了值」
会在写入当场暴露。Counter64 经平台 `float64` 承载，超过 2^53 会丢精度；Counter32 会回绕，
时间序列侧要做回绕检测。团体名按凭据处理（界面掩码），默认 `public` 只是最少配置。

SNMP v3（USM 认证 + 加密，同一 UDP 通道，同一套 OID 地址模型，v3 报文只是外层换成
`HeaderData + USMSecurityParameters + ScopedPDU`）：

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

v3 的三件事全部用标准库实现，没有 CGO 也没有第三方依赖：USM 口令本地化为密钥（1 MiB 扩展 +
`H(Ku||engineID||Ku)`）、HMAC 认证（MD5/SHA-1/SHA-224/256/384/512，截断长度 = 摘要长度的一半）、
DES/AES-128/192/256 加密（含 CBC 与 SNMPv3 特有的 salt-IV）。认证摘要对「authParameters 清零
后的原始字节」计算，比对用常量时间比较；加密时 `msgData` 外面还套一层 OCTET STRING，解密后
按外层 SEQUENCE 长度截断以抵消 CBC 补齐。

发现流程只有一条路：始终发空 engineID 的无认证探测包（带用户名，代理才会回 Report 而不是
静默丢弃），从 Report 一次拿全 engineID/boots/time。即便配置里固定写了 engineID 也照做——
「身份已知」不等于「时间基准已知」，而时间基准只能问出来；跳过它的症状是第一条请求必然吃
一个 `notInTimeWindow`，重试一轮还是 0/0，最后报「时间窗口不同步」。engineID 写错时不
静默改用代理报的值，而是把两个 engineID 都报出来并给出处置建议（删掉 engineID 让驱动自动
发现）——静默纠正会让「配置写错」变成「看起来一切正常」，还会掩盖那次拓扑变更。engineID
变更（代理重装/换主控板）时缓存身份作废、回到空 engineID 重发现，会话自愈；两种失效
Report 走不同起点：`notInTimeWindow` 只重学时间，`unknownEngineIDs` 才丢身份重发现。

加密口令缺省回退到认证口令（USM 允许两者不同，回退是显式的）。加密强度与认证算法绑定：
AES-192/256 要求 SHA-256 及以上，因为加密密钥是从认证算法的哈希派生的——这是协议本身的
要求，不是实现选择。USM 口令 8..64 字节，过短在配置期就拒绝，不让它伪装成认证失败。空
用户名只在 `noAuthNoPriv` 档位允许。

SNMP v3（USM 认证 + 加密，同一 UDP 通道，同一套 OID 地址模型）：

```bash
# 内置代理带 v3/USM：口令走环境变量，避免出现在进程列表与 shell 历史里
UIC_SNMP_AUTH_PASSWORD=authpass123 UIC_SNMP_PRIV_PASSWORD=privpass123 \
  ./uic sim -protocol snmp -v3 -listen 127.0.0.1:16161 \
            -user monitor -auth sha256 -priv aes
./uic -config examples/snmp-v3.json
#   9 点：sysDescr/UpTime/Name/Services、可写 sysContact/sysLocation、
#          ifNumber、ifInOctets、ifAdminStatus
#   自检 v3（不需要网关，一条命令跑完发现 → 认证 → 加密 → 读 → 写 → 读回）：
#     ./uic snmpread -addr 127.0.0.1:16161 -version 3 -user monitor \
#       -auth sha256 -authPassword authpass123 -priv aes -privPassword privpass123 -probe
```

v3 的三件事全部用标准库实现，没有 CGO 也没有第三方依赖：USM 口令本地化为密钥
（1 MiB 扩展 + `H(Ku‖engineID‖Ku)`）、HMAC 认证（MD5/SHA-1/SHA-224/256/384/512，
截断长度 = 摘要长度的一半）、DES/AES-128/192/256 加密（SNMPv3 特有的 salt-IV 构造：
DES 用 `engineBoots‖salt` 做 CBC 前置 IV，AES 用 `engineBoots‖engineTime‖salt` 做
CFB IV；CBC 补齐后按外层 SEQUENCE 长度截断）。认证摘要对「authParameters 清零后的原始
字节」计算，比对用 `subtle.ConstantTimeCompare`；加密时 `msgData` 外面还套一层 OCTET
STRING，解密后必须剥掉。

发现流程只有一条路：始终发空 engineID 的无认证探测包（**带用户名**——空 engineID + 空
用户名会被部分代理当垃圾包静默丢弃，连 Report 都不回），从 Report 一次拿全
engineID/boots/time。即便配置里固定写了 engineID 也照做：「身份已知」不等于「时间基准
已知」，而时间基准只能问出来；跳过它的症状是第一条请求必然吃一个 `notInTimeWindow`，
重试一轮还是 0/0，最后报「时间窗口不同步」。这条捷径（用已知 engineID 发无认证请求）
看似省一次往返，实则拿不到时间基准：配了 auth 的代理回 `unsupportedSecLevel`，配了
authPriv 的更常见的是直接丢弃。代价是建立会话时多一次往返，之后 boots/time 都缓存，
不重复付。engineID 写错时**不**静默改用代理的值，而是把两个 engineID 都报出来并给处置
建议（删掉 engineID 让驱动自动发现）——静默纠正会让「配置写错」变成「看起来一切正常」，
还会掩盖那次拓扑变更。engineID 变更（代理重装/换主控板）时缓存身份作废、回到空
engineID 重发现，会话自愈；两种失效 Report 走不同起点：`notInTimeWindow` 只重学时间，
`unknownEngineIDs` 才丢身份重发现。

加密口令缺省回退到认证口令（USM 允许两者不同，回退是显式的）。加密强度与认证算法绑定：
AES-192/256 要求 SHA-256 及以上，因为加密密钥从认证算法的哈希派生——这是协议本身的
要求，不是实现选择。USM 口令 8..64 字节，过短在配置期就拒绝，不让它伪装成认证失败。
空用户名只在 `noAuthNoPriv` 档位允许。

ADS/AMS（PLC 运行时，TCP 48898）：

```bash
./uic sim -protocol ads -listen 127.0.0.1:48898 &   # 内置 ADS/AMS 服务端
./uic -config examples/ads.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8096/api/v1/points
#   9 点：nCounter/fTemperature/bEnable/sRecipe/arr[2]（只读）、
#          setpoint/fSpeed/run/name（可写）
#   自检：./uic adsread -addr 127.0.0.1:48898 -probe
#   读点位：./uic adsread -addr 127.0.0.1:48898 \
#       -symbol MAIN.nCounter:int16,MAIN.sRecipe:string:81,MAIN.arr[2]:int16
#   写点位：./uic adsread -addr 127.0.0.1:48898 -symbol MAIN.nSetpoint:int16 -set 750
#   状态：  ./uic adsread -addr 127.0.0.1:48898 -state
```

ADS 是唯一「地址就是 PLC 工程变量名、且一次取值要两次往返」的协议：先把变量名换成句柄
（`ReadWrite` + IndexGroup `0xF003`），再用句柄读值（`Read` + `0xF005`）。句柄属于会话，
PLC 重启后全部失效；驱动遇到 `invalid offset` 时清掉缓存、重新解析一次，第二次仍失败就
按点位报错——继续重试只会把「PLC 一直重启」变成「驱动一直转圈」。

地址 `kind=symbol`，`path` 就是符号名；数组下标写在名字里（`MAIN.arr[2]`），不拆成平台
的 `index` 字段。string/bytes 的宽度由点表给出（`NAME:81` 或 length 列），因为 PLC 变量
的宽度是符号属性，驱动猜不出来。全线小端；写前做类型/范围检查，`70000` 写 INT16 会在
发报文之前被拒绝，而不是变成 `4464`。

TCP 48898 与 AMS 端口是两层：`labels.netid` 是 6 段 NetId，`labels.port` 是 851/801。
`Open` 立即做一次 `ReadDeviceInfo` 握手，把 NetId/端口配错从「一堆红点」提前到启动期的
一句 `target machine not found`。只做 AMS/TCP 上的符号读写：通知订阅、符号表浏览、跨
AMS 路由发现、多句柄合并读写都显式声明 `unsupported`——它们各自是另一套模型，半成品
比不做更难排查。

MTConnect（机床数据采集，HTTP + XML，默认端口 5000）：

```bash
./uic sim -protocol mtconnect -listen 127.0.0.1:15000 &   # 内置 Agent（三轴立式加工中心）
./uic -config examples/mtconnect.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8097/api/v1/points
#   13 点：avail/exec/program/mode（事件）、alm（报警条件）、Xabs/Yabs/Zabs、
#          Xload、Sspeed/Sload、temp（采样）、toolOffset（初始 UNAVAILABLE）
#   自检：./uic mtconnectread -addr 127.0.0.1:15000 -probe    # 设备模型与数据项 ID
#         ./uic mtconnectread -addr 127.0.0.1:15000 -current   # 整台当前快照
#         ./uic mtconnectread -addr 127.0.0.1:15000 -id Xabs,exec
#         ./uic mtconnectread -addr 127.0.0.1:15000 -sample -from 1 -count 20
```

MTConnect 是唯一「一条请求覆盖整台设备」的协议：`/current` 一次返回全部数据项的当前
值，所以点位数不影响请求数（`MultiRead` 声明 `supported`）。点位用 `kind=symbol`，
`path` 是 Agent 的 `dataItemId`（如 `Xabs`），不是显示名；寻址不带宽度、不带下标、不带
二级选择器——值有多宽由数据项文本与点表 `dataType` 决定。

**单点问题不升级为链路错误**是这套驱动的核心取舍：`/current` 一条请求覆盖全设备，若把
「某个点 `UNAVAILABLE`」「点表里的 ID 不存在」当成 `Read` 的错误返回，上层会按错误分类
丢掉整条 HTTP 连接——一个本来就不可用的点会让整台设备每周期重连一次。因此 `Read` 只在
「文档拿不到 / 解析不了 / 设备选不出来」时返回错误；`UNAVAILABLE` 折成 `bad` 质量、ID
不存在与文本按 `dataType` 解析失败都折成地址错误，且只写进该点。`toolOffset` 初始即
`UNAVAILABLE`，用来验证「数据项存在但此刻无值」显示为坏质量而不是 0。

值是文本，按 `dataType` 解释，`UNAVAILABLE` 是保留字（不是一个数）。条件是两层结构——
外层元素是条件类型（`System`/`Actuator`…），内层 `Normal/Warning/Fault/Unavailable` 是
级别——同时兼容「元素名即级别、`type` 属性是条件类型」的平铺写法。条件点的值就是级别，
级别即质量：`FAULT`→`bad`、`WARNING`→`uncertain`、`NORMAL`→`good`，同一 ID 多条条件按
`FAULT > WARNING > UNAVAILABLE > NORMAL` 合并。

多设备 Agent（一条产线多台机床）必须显式选设备：`labels.deviceUuid` 优先，其次
`labels.deviceName`；多台且未选就报地址错误并列出可选设备——否则「A 机床的 `Xabs` 覆盖
B 机床的 `Xabs`」不会报错，只会让两台设备的坐标互相串。`Probe` 先 GET `/probe`，遇
404/405 退回 `/current`（裁剪版 Agent 可能只实现后者）；`/sample?from=&count=` 按序号翻
历史窗口，`count` 上限 10000。

地址规范化接受 `host:port`、`http://`、`https://`，省略端口一律 5000；支持反向代理的
基址子路径（`http://host:5000/mtconnect`），拒绝 URL 内凭据、query、fragment。文档上限
32 MiB，gzip 由 `net/http` 自动解压并按魔数兜底；XML 按本地名匹配、忽略命名空间，
兼容 ISO-8859-1/latin-1/windows-1252 与 UTF-8 BOM，不校验 MTConnect 版本。只读协议：
`Write` 声明 `unsupported` 并对每项返回 `ErrUnsupported`（标准没有写数据项的服务）；
`Subscribe`/`Browse`/`Discovery` 都标 `unsupported`——能解析 `/sample` 不等于能订阅，
`/probe` 是一次性诊断而不是调度式浏览。

BACnet MS/TP（串行总线，RS-485，单主站轮询从站）：

```bash
./uic sim -protocol bacnet-mstp -listen 127.0.0.1:24001 &  # 内置从站（MAC 128，TCP 透传）
./uic -config examples/bacnet-mstp.json
curl -s -H 'Authorization: Bearer dev-token-0123456789' localhost:8103/api/v1/points
#   7 点：analog-input:1 的 present-value/object-name/units、binary-input:1、
#          analog-value:2 的 present-value、analog-value:3 每秒自增、character-string:1
#   自检：./uic mstpread -transport serial -port /dev/ttyUSB0 -baud 38400 -mac 128 -probe
#         ./uic mstpread -addr 127.0.0.1:24001 -mac 128 -type analog-input -instance 1
#         ./uic mstpread -addr 127.0.0.1:24001 -mac 128 -type analog-value -instance 2 -write 55.5
```

MS/TP 与 BACnet/IP 是同一套 NPDU/APDU/对象语义，只换了承载层：IP 上是 UDP 数据报，
MS/TP 上是加了两条 CRC 的串行帧。因此两者共用 `codec/bacnet`（`BuildNPDU`/`ParseNPDU`
被抽成公共层）、同一套对象属性寻址（`analog-input:1.present-value`）、同一套
ReadProperty（0x0C）/WriteProperty（0x0F）服务与错误类映射。区别在于分帧：
`55 FF | type | dest | src | len(2,大端) | hdrCRC | data | dataCRC(2,小端)`，头 CRC
用标准给出的特殊移位展开（有效帧残差 0x55），数据 CRC 是 CRC-16/X-25（残差 0xF0B8）。
同一帧里长度字段大端、数据 CRC 小端，是标准原文如此，也是二手实现最容易抄错的地方。

**定位是「单一主站轮询从站」**：本机占据主站地址段 0..127（默认 0），被采集设备是
MAC 128..254 的从站（`labels.address`，回退 `device.unitId`）。主站用
DataExpectingReply 点名问从站，从站回 DataNotExpectingReply，令牌不发生转移——从站
不持有令牌。驱动**刻意不做**多主站令牌环的建立与维护（Tslot 抢占、NextStation 探活、
Tno_token 等毫秒级定时器在 TCP 透传/串口服务器上都不可靠）、不做 Who-Is 发现、不做
COBS 扩展帧、不做波特率自适应、不做 ReplyPostponed 的后续上送（直接映射为协议错误，
让上层重试）、不轮询主站设备。**空闲时不监听总线**，这是 sole-master 拓扑的明确前提，
不是遗漏：若现场确有第二个主站，应更换驱动而不是指望 `labels.nextStation` 维持令牌环。

在等待从站应答的窗口内，驱动会**被动配合**令牌环：收到发给本机的 Token 就记住并在
事务结束后交还（优先 `labels.nextStation`，否则按 PollForMaster 从本机地址之后现场探测
下一站），收到 PollForMaster/TestRequest 立即应答。每次发送前留 `labels.presilence`
（默认 5ms）线路静默，给 RS-485 收发器与 `Tturnaround` 留时间；过短会在长线上表现为
「随机首字节丢失」→ 整帧被当噪声 → 随机超时。串口按物理路径共享、事务由总线锁串行，
`concurrency` 必须为 1。只做单属性读写，不做 ReadPropertyMultiple 与 COV 订阅（理由与
BACnet/IP 相同）。

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

串口从站（RS-485 半双工总线；`framing` 只接 `rtu` 或 `ascii`，不做自动探测）：

```bash
# 有硬件时把 port 换成 /dev/ttyUSB0；无硬件时可以用一对 pty 模拟总线
# （socat -d -d pty,raw,echo=0 pty,raw,echo=0，把两个路径分别填给下面两行）
./uic -config examples/modbus-slave-rtu.json &    # 从站打开 serial.port

# 另一端自检：读 2 个寄存器、写 1 个可写点位
./uic mbread -transport rtu -port /dev/pts/11 -unit 1 -area holding -offset 0 -qty 2
./uic mbread -transport rtu -port /dev/pts/11 -unit 1 -area holding -offset 2 -write 777

# ASCII 只需换分帧方式（设备侧配置也要一致）
./uic mbread -transport ascii -port /dev/pts/11 -unit 1 -area holding -offset 0 -qty 1
```

`uic mbread` 的串口参数与配置同名：`-baud` / `-dataBits` / `-parity` / `-stopBits`。
帧边界外的行为是刻意的：非本机单元号**静默**（总线上还有别的从站），广播（单元 0）
只执行写不回应，CRC/LRC 错后丢弃残字节重新同步。串口端口与主站通道、其他从站
按字面路径互斥，配置期就会拒绝重复占用。

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

同一个 Broker 也能跑 Sparkplug B：把载荷格式换成 `sparkplug-b`，主题与生命周期
就交给协议的契约管理：

```bash
./uic -check -config examples/sparkplug.json
./uic -config examples/sparkplug.json
#   任意 Sparkplug B 消费端订阅 spBv1.0/plant-a/#
#   顺序：NBIRTH(gw-line1) → DBIRTH(line-a) → DDATA…
#         → DBIRTH(line-b) → DDATA…
```

它和 json 格式**互斥**，不是「多带几个字段」：`format: "sparkplug-b"` 时主题由
`groupId/nodeId/设备名` 拼出，`topic` 必须留空（配了会启动报错），`retain` 必须
为 false、`qos` 必须省略（都配了会启动报错）——Sparkplug 对每类消息逐条规定了
QoS 与 retain，做成可配只会让人配出一个「本地正常、严格 Host 直接丢弃」的部署。
本平台只扮演**边缘节点**，不发布 `spBv1.0/STATE/{host_id}`（那是 Host 的出生/
死亡证书），在线语义由 NBIRTH/NDEATH 表达。

序号是节点级的（节点消息与全部设备消息共用一个 0..255 计数器，NBIRTH 为 0 并
重置），NDEATH 不带序号；`bdSeq` 是另一套计数，**每个新 MQTT CONNECT 前进一格**
并同时出现在遗嘱与 NBIRTH 里，Host 靠它配对「生—死」。重连、
NCMD `Node Control/Rebirth`、设备从故障或配置删除后恢复都会重发 BIRTH；设备被
从配置里删掉时先发 DDEATH 并忘掉点位，否则重连的 DBIRTH 会把不存在的设备重新
宣布为在线。质量非 good 用 `is_null` 表达——把断线前的旧值当新值发出去，比不发
更危险。

点位值映射按 Sparkplug DataType 走，`bytes` 用 Bytes (17) + `bytes_value`
字段保留原始字节（声明成 String 会被 UTF-8 改写）；BCD 按 Double 发。DataSet、模板、别名、
报警出口都不做：它们各自是另一套状态机，半成品比不做更难排查。

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
| MQTT · Sparkplug B | 与 SCADA/IIoT 平台的标准化生命周期 | ✅ `format: "sparkplug-b"`（只扮演 Edge Node）：NBIRTH/NDEATH/DBIRTH/DDEATH/DDATA、序号与 bdSeq、NCMD/DCMD Rebirth、重连补 BIRTH、删除点位发 DDEATH；手写 Protobuf；不发 Host 的 `spBv1.0/STATE/{host_id}`；不做 DataSet/模板/别名 |
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

实现见 `internal/scheduler/`，测试见 `internal/scheduler/*_test.go`（覆盖率 88.1%）。

---

## 9. 代码地图

### 9.1 包一览

| 包 | 职责 | 关键不变量 | 覆盖率 |
|---|---|---|---|
| `internal/model` | 通道/设备/点位/地址/采样/质量码/从站映射与设备标识/窗口摘要 | 不依赖任何内部包；不变量集中在 `Validate`；DB 号与 area 联动校验；计算点强制只读、4 类地址各有规范形式；订阅周期只在 `subscribe=true` 时允许出现；从站标识限长并禁控制字符；`WindowStats.Avg` 在 `Count=0` 时返回 0 而不是 NaN | 87.1% |
| `internal/driver` | 驱动契约（Codec/Transport/Driver）、错误分类、注册表、只读 `Lookup` 注入、**部分失败契约的两个统一谓词**（`IsLinkFailure`：超时/断连才算链路级；`PartialReadError`：有成功点则吞错、全失败才上抛） | 能力四态；`Conn` 不保证并发安全，由调用方串行；协议错误可用 `Kinded` 自带分类；部分失败规则见 `docs/03-product/04-driver-sdk-spec.md` §3.3.1，禁止各驱动自写闸门（漏一处就会出现「一个坏点让整台设备每周期重连」） | 87% |
| `internal/codec/modbus` | Modbus 纯函数编解码（TCP/RTU/ASCII，主站与从站共用） | 无 I/O、无时钟、无可变状态 | 88.5% |
| `internal/codec/value` | 协议无关值编解码（重排、数值/字符串/BCD） | 与协议无关；`Encode` 对 string/bytes 不猜宽度 | 95% |
| `internal/codec/s7` | S7 纯函数编解码（TPKT/COTP/S7 PDU、块目录/属性/上传、长度单位换算） | 请求按字节、响应按位/字节的差异收敛在一处；ACK/ACK_DATA 为 12 字节头、Job/UserData 为 10 字节；ListAll 的数据区是 `0A 00 0000`；块长度是 5 位十进制 ASCII | 85.4% |
| `internal/codec/opcua` | OPC UA 纯函数编解码（UACP HEL/ACK、安全报文、NodeId、Variant、读/写/浏览 + 订阅/发布/确认） | 分块响应显式报错；数组长度上界必查；BCD 等无对应类型显式报错；未知通知类型跳过而不是判故障 | 86.1%（含 driver/opcua 与 opcuaserver 交叉覆盖） |
| `internal/transport` | 字节流抽象 + TCP + 串口（termios） | 写满、读满或超时、关闭幂等；协议层 deadline 不得被兜底值覆盖 | 80.3% |
| `internal/driver/modbus` | Modbus TCP + RTU + ASCII 主站 | 事务号递增；异常码分类（05 Acknowledge / 06 Busy 归可重试的临时故障，与 04 设备故障同档；01/02/03/08 不可重试）；部分失败不吞好样本；同串口事务串行；分帧类型参与端口冲突判定 | 82.7% |
| `internal/driver/s7` | S7（S7comm）主站；块服务走独立一次性 `Session` | COTP/Setup 握手不可省；按协商 PDU 与 20 项上限切块；逐项返回码映射质量码；顶层 ACK 错误直接判协议错；块上传以 `param[1]` 判最后一片，失败也补 End 释放 PLC 会话 | 78.1% |
| `internal/driver/opcua` | OPC UA 二进制主站（None/None + 匿名）；可选订阅推送 | 三层状态（TCP/通道/会话）分开判断；按 RequestId 过滤迟到响应；节点越界判坏不截断；订阅走独立第二条连接，Read 优先本地表、失效即回退真读 | 76% |
| `internal/driver/mqtt` | MQTT 南向（订阅取数）：把推送型协议适配成拉取契约 | `Read` 无网络交互，只读本地最新值表；质量码只反映新鲜度（good/stale/bad 三档）；一条消息都没有不算断链，安静到一个窗口且 PING 失败才判链 | 77.4% |
| `internal/driver/computed` | 计算点：表达式驱动，无物理链路 | 同设备依赖在一次 `Read` 内按拓扑序闭合；输入不可用则不计算、质量取最差并指名输入；整数越界判坏不截断；窗口聚合的数据来自只读 `OpenConfig.History`，缺失时显式失败；求值失败的错误归 `ErrProtocol` 而非 `ErrInternal`（计算点无物理链路，bad 永远来自表达式/输入，不是内部 bug） | 92.7% |
| `internal/driver/virtual` | 虚拟设备（波形 + 故障注入） | 故障按「第 n 次请求」精确注入一次；Probe/Read/Write 三个入口共享同一请求序列，`disconnect` 也遵守 `faultAt`（`faultAt<=0` 时从不断连，`>0` 时第 n 次请求起断），避免 `disconnect+faultAt>0` 成为永远建不起链、跑不到恢复的死配置 | 98.6% |
| `internal/sim/modbus` | 内置 Modbus TCP 从站 | 复用同一份 codec，避免主从语义漂移 | 87.2% |
| `internal/sim/s7` | 内置 S7 从站（内存 DB/M/I/Q 区；块目录/属性/上传） | 与主站共用 codec/s7；输入区只读；位写保邻位；DB 块内容直接从内存区域反向暴露，与读点位看到同一份数据；上传会话按连接隔离，End 后立即失效 | 80% |
| `internal/opcuaserver` | 可复用的 OPC UA 服务端内核（地址空间、订阅/发布、标准节点） | 与主站共用 codec/opcua；未激活会话拒绝读写；Variant 类型必须与节点 DataType 一致；Publish 在独立 goroutine，响应按 RequestId 乱序返回；`NewSimulator` 提供演示地址空间 | 82.2% |
| `internal/northbound/opcua` | OPC UA 北向：点位表 → 地址空间 | 一点一变量；质量码映射到 `DataValue.Status`；写入过 `Runtime.Write`（设备 actor + 幂等）；配置变更按指纹重建，抢不到端口回退上一份配置 | 82.4% |
| `internal/sim/mqtt` | 内置 MQTT 3.1.1 Broker，兼 `uic sim -protocol mqtt` 运行时 | 全项目唯一一份 Broker 实现；不做 QoS2/持久会话/TLS；QoS2 授予时降级为 1；解析 CONNECT 并记录 `ConnectInfo`，**在非正常断开时代发遗嘱**（收到 DISCONNECT 则不代发），retained 消息在 SUBSCRIBE 后补发 | 79.3% |
| `internal/slave/modbus` | Modbus 从站（TCP / RTU / ASCII，把点位暴露给外部主站） | 读走 cache、写走 Runtime.Write；未映射地址报错不补零；坏质量保留最后值并计数；RTU 按功能码推导帧长、ASCII 以 CRLF 分帧；非本机地址静默、广播只写不回；诊断与设备识别（FC8/11/17/43） | 81.4% |
| `internal/alarm` | 报警引擎：五态机、阈值/滞回/延时、表达式规则（含历史窗口）、确认、订阅 | 输入不可信时绝不改状态机；延时是「连续满足」；`cleared` 与 `active` 都要确认；状态只在内存；窗口无样本时判 unknown 而不是「不满足」 | 93.8% |
| `internal/history` | 有界采样日志：NDJSON 分段、整段淘汰、原始查询、定步长聚合与窗口摘要 | 一条记录不跨段；永远保留至少一段；末尾半行触发新段；查询升序、桶对齐、桶质量取最差；非数值点位不产出 `min/max/avg`；`WindowStats` 只统计 good 的数值样本，空窗口返回 `Count=0` 而非错误 | 84% |
| `internal/calc` | 零依赖表达式引擎（编译期类型检查、短路求值、历史窗口聚合） | 无 `eval`；引用是 `device:point`；除零/NaN/Inf/溢出都报错而不是降级；窗口只允许 1s..24h，且只在第二个参数是时长时生效；窗口引用不进 `Refs()` | 92.7% |
| `internal/template` | 设备模板：参数校验、字符串字面量替换、实例化后完整校验 | 占位符只替换字符串字面量且不做算术；整字面量是占位符时 int/float/bool 落成 JSON 数字/布尔；未知字段/参数/占位符与键里的占位符一律报错；必填缺失一次列全 | 92.5% |
| `internal/pointtable` | 点表 CSV：列别名、地址表达式、按协议推导宽度、合并动作 | 列名大小写/空格/下划线不敏感；未知列报错；行错误带行号列名并聚合；重复 ID 是错误、重复地址是警告；`length` 先于地址解析；`#selector` 只允许 MQTT；SNMP 只允许 OID；远动用 `WidthNone`（不表达长度、布尔量无位号，`length` 列与 `ioa:1.3` 在导入期拒绝）；MTConnect 同样用 `WidthNone`（值宽度由数据项文本与 `dataType` 决定，`dataItemId` 里的点号是 Agent 命名习惯而不是下标，`length`/selector/index 在导入期拒绝）；BACnet/BACnet MS/TP 用 `WidthProtocol` + 对象地址（`analog-input:1.present-value`，允许带连字符与数字的对象类型）；默认拒绝静默覆盖 | 92.1% |
| `internal/codec/mc` | 三菱 MC 3E 纯函数编解码（帧头/设备码表/位打包/结束码/24 位设备号） | 无 I/O、无时钟、无可变状态；起始设备号 3 字节小端；位软元件按位打包（首点 bit0）；`Lookup`/`ByCode` 大小写不敏感；结束代码表 `0xC052/53/54/56`→地址错 | 86.5% |
| `internal/driver/mc` | MC 3E 主站驱动（字/位读写、字软元件 bool 的 RMW、字序归一化、结束码分类） | 只做 3E 二进制帧 + TCP；字软元件 bool 必须显式 `address.bit`，驱动读整字→改位→写整字不清零邻位；字序归一化后按 `byteOrder` 解码默认 CDAB；结束码映射质量码 | 56.9% |
| `internal/sim/mc` | MC 3E 内置从站（内存字/位表、按区读写、越界/未知设备结束码） | 一问一答；未配置编号读返回 0、写落表；越界返回 0xC054；设备码反查 | 89.8% |
| `internal/codec/fins` | 欧姆龙 FINS 纯函数编解码（FINS/TCP 封装、节点协商、内存区码表、字/位读写命令、结束码） | 无 I/O、无时钟、无可变状态；TCP 长度只含「命令+错误码+负载」；读/写 MRC 同为 0x01、靠 SRC 区分；位访问一位一字节（不是按位打包）；当前 EM 区无位访问码 | 96.6% |
| `internal/driver/fins` | FINS/TCP 主站驱动（节点号协商、字/位读写、当前 EM 区 bool 的 RMW、低字在前字序、结束码分类、SID 校验） | 只做 FINS/TCP；`offset` 是字地址、`bit` 是字内位号；`unitId>0` 覆盖目标节点；字/位混合 span 拆两次事务再按原序合并；节点协商失败归类为可重试连接错 | 81.4% |
| `internal/sim/fins` | FINS/TCP 内置从站（节点分配、字内存表同时承载字/位视图、结束码） | 未配置地址读 0、写落表；位写只改目标位；越界 0x1103、未知区码 0x1101、点数 0/超限 0x1102；节点冲突回 `0x24` | 78.9%（抽出 `finsmem` 后） |
| `internal/driver/finscore` | 与 FINS/TCP 共享的主站会话（区拆分、字/位读写、按 `Limits` 切块、结束码分类、错误分类、内存区码表） | FINS/TCP 与 Host Link 复用同一实现：FINS/TCP 用 `fins.MaxWords=999` 切块，Host Link 用 `Limits`（读 269 字 / 538 位、写 267 字 / 534 位）切块；位写走内存区原生位命令（不需要 RMW）；结束码分类 0x11xx→address、0x03xx 写保护→permission、0x02xx→comm 可重试 | 82.4%（跨包） |
| `internal/sim/finsmem` | 与 FINS/TCP 共享的从站内存表（字视图 + 位视图、EM bank 0..12、区码表、`Respond`） | 同一张内存表被 `sim/fins`（FINS/TCP）与 `sim/hostlink`（Host Link）复用，从站不再各写一套；未配置地址读 0、写落表；位写只改目标位；越界 0x1103、未知区码 0x1101、点数 0/超限 0x1102 | 0%（无独立测试，跨包覆盖） |
| `internal/codec/enip` | EtherNet/IP 纯函数编解码（封装头/CPF/SendRRData/RegisterSession、CIP 请求与应答、符号路径与逻辑路径、Read/Write Tag、多服务合包、类型表、STRING） | 无 I/O、无时钟、无可变状态；封装长度只算负载（不含 24 字节头）；CPF 奇数条目补 1 字节对齐；发送方上下文原样回显；属性段不做（0x2C/0x30 编码有歧义）；CIP 状态码在 codec 层统一分类 | 97% |
| `internal/driver/enip` | CIP 显式报文主站（RegisterSession、符号标签读/写、数组 offset、STRING、bytes 两阶段读、位 RMW、多服务合包与自动退回） | 只做显式报文 + TCP；`address.length` 按类型解释（不能用 `PointRef.Length` 当元素数）；数值线上小端，解码先整元素反转再走 `value.Decode(ABCD)`；类型以应答里的实际类型为准；offset 写读整段再改目标元素；设备回 0x08/0x14 自动关合包；封装状态 0x64 单独判可重试；一轮里只有点级失败（address/permission/unsupported）时 `Read` 返回 `nil`，链路级失败（`driver.IsLinkFailure`：超时/断连）才立即上抛并标坏剩余点 | 87.4% |
| `internal/sim/enip` | EtherNet/IP 内置从站（RegisterSession/UnRegister、Read/Write Tag、多服务合包、默认标签表） | 与主站共用 codec/enip；标签表显式指定类型与元素数；未配置标签回 0x05、越界 0x15、类型不匹配 0x03、数据不足 0x13；`SetMultipleSupported(false)` 验证主站退回路径 | 87.4% |
| `internal/codec/bacnet` | BACnet/IP 与 MS/TP 共用纯函数编解码（NPDU/APDU、application 与 context 标签、对象属性请求与应答、Error/Reject/Abort、对象类型与属性名表；MS/TP 分帧与双 CRC） | 无 I/O、无时钟、无可变状态；BVLC 长度只算 NPDU+APDU；Forwarded-NPDU 跳过 4 字节转发信息；`BuildNPDU`/`ParseNPDU` 抽成公共层供 MS/TP 复用；application 与 context 的 LVT 含义分开实现；BOOLEAN 的 LVT 直接承载值；数值用最短字节数；编号解析只接受纯十进制（`-1` 不会被吃掉减号）；MS/TP 头 CRC 用标准的特殊移位展开（残差 0x55）、数据 CRC 用 CRC-16/X-25（残差 0xF0B8），长度大端、数据 CRC 小端 | 91.8% |
| `internal/driver/bacnet` | BACnet/IP 主站（对象属性读写、UDP invoke ID 配对、按对象语义选写标签、错误类映射） | 只做 ReadProperty/WriteProperty；UDP 用 connected socket，整条会话串行化；按 invoke 丢弃迟到应答；超时重发 2 次、断链不重试；写 `present-value` 的标签由对象语义决定；读数值宽松、bool/string 严格；点级错误（address/permission/protocol）只标该点，有成功点则 `Read` 返回 `nil`——一个属性名拼错不该让整台设备每周期重连；`resources`→`disconnected` 属链路级，立即收尾 | 87.3% |
| `internal/sim/bacnet` | BACnet/IP 内置从站（对象表、属性读写、数组下标、写权限、类型收敛） | 与主站共用 codec/bacnet；AI 的 present-value 只读、AV 可写；数组下标越界与「对标量带下标」都回 invalid-array-index；写入按目标属性类型收敛，不收的类型回 invalid-data-type；垃圾报文静默丢弃 | 85.2% |
| `internal/driver/bacnetmstp` | BACnet MS/TP 主站（串行总线单主站轮询从站、滚动字节扫描切帧、被动配合令牌环、PollForMaster 现场探测下一主站、`presilence` 静默） | 只做 ReadProperty/WriteProperty；从站 MAC 必须 128..254（0..127 是主站段，配置期拒绝）、本机主站默认 0；不做多主站令牌环维护/Who-Is/COBS 扩展帧/波特率自适应/ReplyPostponed 上送，空闲时不监听总线；`Drain→SetDeadline→发送` 顺序固定（TCP 透传的 Drain 会把 deadline 压到 1ms 且不恢复）；重试 2 次；收到 Token 事务结束后交还、PollForMaster/TestRequest 立即应答；部分失败遵循 §3.7.4/§3.9.5（有成功点则不返回整体错误、全部失败才返回、超时/断链立即返回并把剩余点标坏），不拖成 N 倍时长 | 82.6% |
| `internal/sim/bacnetmstp` | BACnet MS/TP 内置从站（MAC 128、镜像 sim/bacnet 默认对象、属性读写与数组下标、写权限与类型收敛、每秒自增、垃圾帧重扫） | 只模拟从站：不产生令牌、不参与令牌环、不应答 PollForMaster，只应答发给本机的数据帧与 TestRequest；对象表与 sim/bacnet 一致，切换承载不必改点表；从站 MAC 越界在启动期拒绝；帧扫描逐字节找 preamble、坏帧重扫 | 81.0% |
| `internal/codec/iec60870` | IEC 60870-5 **共享应用层**（ASDU 头与信息对象、元素宽度表、归一化/标度化/短浮点、CP56Time2a、品质位、`Profile{OrgSize,CASize,IOASize}`、`HasIOA`、请求构造；点位语义：对象类别、`PlanPoint`、值转换、命令构造、类型/原因名表） | 无 I/O、无时钟、无可变状态；剖面不合法在编码前拒绝（超宽地址不静默截断）；总召唤/电能量召唤/时钟同步/初始化结束四类**没有 IOA**，编码解码共用 `HasIOA` 判断并拒绝带地址与多对象；SIQ/DIQ 品质位整体左移 3 位、QDS 低 5 位，两者不混用；CP56Time2a 逐位掩保留位；8 种剖面组合回环 + 四类无地址报文黄金字节 | 73.9% |
| `internal/codec/iec104` | IEC 60870-5-104 纯函数编解码（APCI 三种帧型、15 位收发序号、k/w 流控）+ 对共享应用层的别名转发 | 无 I/O、无时钟、无可变状态；长度字段只算控制域+ASDU；U 帧功能码低两位必须 `11`；I 帧无 ASDU 直接判错；`ParseFrame` 只解析已切好的整帧，不做流式重组；本包不重复实现 ASDU，转发到 `codec/iec60870`，既有调用点零改动 | 88.2% |
| `internal/driver/iec104` | IEC 60870-5-104 主站（STARTDT/STOPDT/TESTFR、站总召唤、电能量召唤、按点位 ID 的最新值表、五种命令、否定确认分类、旁路表） | 只做平衡式 TCP；`Read` 不产生网络交互；质量只来自 ASDU 品质位与链路活性，不按「多久没更新」降级；召唤超时不拆链、EndOfInit 重新召唤并 1 秒去重；命令类型由 `area` 声明；双点 0/3 本地即拒；缓存按点位 ID（同一 IOA 的遥信与遥控互不覆盖）；写成功先填缓存并标 uncertain，但只补「没有」或「比命令更旧（`sentAt`）」的条目（避免覆盖紧跟确认的突发上送）；否定确认 44/45→protocol、46/47→address、其它→permission；旁路表 4096 条上限 | 92.0% |
| `internal/sim/iec104` | IEC 60870-5-104 内置从站（对象表、STARTDT 门控、站总召唤快照、电能量召唤、命令三段式、突发上送） | 与主站共用 codec/iec104；STARTDT 之前不发数据；站召唤跳过纯命令对象；SQ=1 分组不跨类型；累计量只走电能量召唤且每秒自增；只读对象回否定确认「未知 IOA」；乱序 I 帧立即断链；`Close` 幂等 | 95.7% |
| `internal/codec/iec101` | IEC 60870-5-101 纯函数帧层（FT1.2 固定帧/可变帧/单字节帧、控制域 FCB/FCV/ACD/DFC、校验和、`ParseAt`/`FrameLen`、流式收帧 `Reader`） | 无 I/O；长度字段只计「控制域+链路地址+ASDU」（整帧 = `L+6`）；校验和从控制域累加到 ASDU 末字节；链路地址宽度 0..2 字节且必须来自配置（不从报文猜）；噪声逐字节跳过而不是整段丢弃；单字节帧保留原始字节（`E5`/`A2` 不混同）；黄金字节钉住四种帧 | 73.0% |
| `internal/driver/iec101` | IEC 60870-5-101 非平衡轮询主站（复位链路、第 1/2 级轮询、站总召唤 + 电能量召唤、FCB 原帧重传、命令两段确认、串口/透传双承载、旁路表） | `Read` **真的走总线**（与 104 的纯缓存不同）；总召唤不放在 `Open`（几百点在 9600 上要几秒，会和「连接超时」混淆）；每轮无条件请求第 1 级，连续取数上限 32 条/轮（公平性）；终止只认类型 100/101 的激活终止与 70，激活确认不当结束；重传复用同一帧字节（FCB 不变），依赖从站去重避免命令重复执行；写等到应用层激活确认才成功，44..47 分类返回；缓存按点位 ID，写后只补「没有或更旧」的条目；半双工纪律：先 `Drain` 再设截止时间，总线锁覆盖整段事务 | 72.7% |
| `internal/sim/iec101` | IEC 60870-5-101 内置从站（FT1.2 over TCP 透传、FCB 去重、ACD、两级队列、召唤快照与终止、命令两段确认、可配置剖面） | 从站不主动开口；重复帧只确认不重执行；`ACD` 只表示「还有数据」；站召唤排除纯命令对象与累计量，电能量召唤用类型 101 终止；`SetValue` 保留 `Command`/`Writable` 身份位（改值不会让遥控号变成监视点）；默认对象 IOA 是四位数，小地址剖面必须 `WithNoDefaults` 自己 `Add` | 82.9% |
| `internal/codec/mewtocol` | Mewtocol 纯函数编解码（ASCII 帧 / BCC 含 `%` / 站号十进制 / 接点编号「十进制字 + 十六进制位」/ RD/WD/RCC/WCC/RCS/RCP/WCS/WCP/RS/WS/RK/WK 组帧 / 字内低字节在前 / 错误码四类 / 流式收帧） | 无 I/O、无时钟；BCC 从 `%` 到最后一位正文字符，不含 CR；站号 1..99 两位十进制，广播 `FF`；RD/WD 地址 5 位、RCC/WCC 4 位、索引寄存器「区码 + 9 个 0」；读用起止地址而非「起点 + 个数」；写的结束地址由数据长度反推；应答只回显前两位命令码（`RCC`→`RC`、`WCS`→`WC`）；`MaxWords=24`（写侧 `20+4n` 比读侧 `9+4n` 更紧）、`MaxContacts=8`（个数位一位）；T/C 接点全十进制且无位号；黄金字节含手册 BCC 实例 | 77.6% |
| `internal/driver/mewtocol` | Mewtocol 主站（区拆分与 24 字切分、T/C 按点读 RCS/RCP、布尔量 WCS 单点写、字区布尔量拒绝、站号与回显校验、串口/TCP 双承载） | `Read` 真实走线：span 必须同区、按 `[min,max]` 合并后每 24 字切一刀、逐点按 `address.bit` 取位；T/C 不走 `RCC`（会按每字 16 点重编号），只走 `RCS`/`RCP`；布尔量只走 `WCS`，字区布尔量直接 `unsupported`（协议没有原子位写）；写的结束地址由数据长度反推；帧间静默 10ms 可配，`Drain` 必须早于 `SetDeadline`；应答校验站号 + 回显前两位；错误分类 `60/61/66/67`→address、`63/65`→permission、`40..43`/`53`→protocol 可重试，`23/24/26` 不可重试；不做 FP2-ET-LAN 16 字节封装 | 62.7%（跨包） |
| `internal/sim/mewtocol` | Mewtocol 内置从站（站位图、两套接点编号、RCC 按字与 RCS/RCP 按点、WCS/WCP、错误码、站号过滤） | 与主站共用区域表与帧构造（`ParseRequest`/`BuildResponse`）；非本机站号的请求静默（广播也不应答）；`RCC` 对 T/C 按「每字 16 点」真实重编号，避免主站误以为按字读 T/C 安全；字区位写回 61H/66H 而不是「当字写成功」；未知区 60H、格式错 41H、命令不支持 42H、越界 66H | 64.9% |
| `internal/codec/hostlink` | Host Link FINS 纯函数编解码（ASCII 信封 `FA` / FCS 含 `@` / 收发两条独立布局 / 单元号两位十进制 / 等待时间一位十六进制 / 读 269 字、写 267 字上限 / 结束码分层 / 流式收帧 `*`+CR 定界） | 无 I/O、无时钟；请求与应答是两条独立布局（应答 `FA` 后多一格结束码、少一格等待时间）；FCS 从 `@` 起异或到正文末、不含 `*`/CR；单元号 0..31 两位十进制、等待时间 0..F 一位十六进制、ICF 直连 0x00；`FromFINS` 丢弃六个跨网路由字段；单次上限读 269 字/538 位、写 267 字/534 位（读写差两个字是字符预算反推的结果）；黄金字节逐字节钉住 | 73.1% |
| `internal/driver/hostlink` | Host Link FINS 主站（`FA` 信封组帧与收发校验 / 单元号与等待时间配置 / 串口出厂 9600-7-E-2 / 帧间静默 / 超时排查顺序 / 不自动重试） | `RoundTrip` 先 `Drain` 再设截止时间、总线锁覆盖整段事务；校验应答单元号、MRC/SRC 回显、FCS；超时文案带排查顺序（单元号→串口参数→端口模式→波特率）；不自动重试（无可靠事务号，重发写可能是脏写），重连交给调度器；串口缺省只在用户完全没配时补 9600-7-E-2 | 76.7%（跨包） |
| `internal/sim/hostlink` | Host Link FINS 内置从站（TCP 透传 / 逐字节扫帧 / `*`+CR 定界 / 单元号不匹配完全沉默 / FCS 错 0x13 / 格式错 0x14 / 头码错 0x16） | 与主站共用 `codec/fins` 与 `codec/hostlink`；单元号不匹配就完全不出声（真 PLC 只回应点名到自己的帧）；FCS 错回 0x13、格式错 0x14、认不出的头码 0x16；预置初值与 `examples/hostlink.json` 对应（DM0=1234/DM1=5678/DM2..3=float32 3.14/CIO0.1=on/CIO0.5=on） | 85.5% |
| `internal/codec/dnp3` | DNP3（IEEE 1815）纯函数编解码（CRC-16/DNP、链路帧、流式收帧、传输分片重组、应用层请求/响应、对象解析、请求头、CROB 与组 41 构造） | 无 I/O、无时钟、无可变状态；CRC 按 16 字节分块且块尾插校验；链路帧 LEN 只算用户数据；双点静态量变体 1 是 2 位/点；变体 2 是每点一个「状态+品质」字节、无独立位图；计数器/模拟量是每对象 `flags+值` 交错，不是 flags 块 + 值块；带时标事件（组 22 v5/v6）是「值+品质+6 字节绝对时标」=11/9 字节，不是组 20 无品质变体的 4/2 字节；未知组/变体直接报错不猜布局 | 71.9% |
| `internal/driver/dnp3` | DNP3 主站（复位链路 + TEST_LINK、类 0/1/2/3 轮询、未请求响应、应用层确认、CROB 与组 41 直接控制、旁路表） | 只做 TCP 主站；`Read` 只查本地表不产生网络交互；总召唤默认 5m、事件轮询默认 1s 且必须 < 总召唤；`masterAddress` 与 `unitId` 必须不同且 1..65519；带 CON 的响应与未请求响应立即回确认；质量只看低 6 位（状态位必须先屏蔽）；双点 0/3 → false + uncertain；写操作检查从站回显状态，非零 status 按 protocol 错误上报；模拟量输出值在发报文前校验能装进选定线格式（越界/带小数/`NaN`/`±Inf` 拒绝、设备零写入）；不做 SBO、安全认证、文件传输、时间同步写路径、串口 | 75.0% |
| `internal/sim/dnp3` | DNP3 内置从站（链路 ACK、类 0 静态快照、类 1/2/3 事件队列与 inflight、CROB 与组 41 直接控制回显） | 与主站共用 codec/dnp3；链路复位/探活/TEST_LINK 都回 ACK；确认用户数据先回链路 ACK 再处理应用报文；事件响应置 CON 且未确认前留在 inflight 重发；一次 READ 可同时请求类 0/1/2/3；未配置的点按 flags=0（离线）编码而不是 good；未知输出索引回 CROB not-supported；静态编码 → 主站解析有往返自洽测试 | 22.7% |
| `internal/codec/coap` | CoAP 纯函数编解码（4 字节头与 Token、选项 delta/length 扩展、uint 与 Block 编解码、URI 拆分、link-format 解析） | 无 I/O、无时钟、无可变状态；选项 delta 是相对值、扩展字节先 delta 后 length；`15` 是保留值，出现在 length 位置即畸形；Block 值 `NUM<<4\|M<<3\|SZX`，SZX 是块大小的对数下标；link-format 只做发现所需的一层解析 | 95.5% |
| `internal/codec/textval` | 文本/JSON 标量 → 平台值（MQTT 与 CoAP 共用；selector 支持 `a.b` 与 `a[0]`） | 无 I/O、无时钟；布尔接受 `true/false/on/off/yes/no/1/0`；整数类型拒绝小数也拒绝越界（含 `int64`/`uint64` 边界），不截断；`NaN`/`±Inf` 判坏；写路径与读路径共用同一条范围校验（`FormatNumber`，`uint64` 走无符号格式化）；未知类型报错而不是猜；selector 里只有合法整数才当下标，否则按完整字段名处理 | 96.7% |
| `internal/driver/coap` | CoAP 主站（GET/PUT/POST、Block2 续读与 `4.02` 降级、分离响应、指数重传、资源发现、坏包跳过与无关 CON 回 RST） | 只做明文 UDP 主站；首请求默认不带 Block2、由服务端决定块大小；收到空 ACK 后不再重传、只等分离响应；等待窗口不越过设备级 timeout；`Read` 逐点串行、链路级失败后剩余点短路并上抛；点级失败（`4.04`/类型不符）只标该点、有成功点则 `Read` 返回 `nil`、全失败才上抛；PUT 是替换整个资源，JSON 带 selector 只发子树、数组下标写入拒绝、越界/带小数/`NaN` 值在发报文前拒绝；不做 DTLS/Observe/Block1 发送 | 89.3% |
| `internal/sim/coap` | CoAP 内置服务端（CON/NON、MID 去重、Block2、分离响应、RST、link-format 发现、`DisableBlock2` 降级模式） | 与主站共用 codec/coap；同 MID 重发只重发缓存响应、不重复执行写；Block2 越界回 `4.02`；Block1 明确拒绝；空载荷写回 `4.00`；发现清单自动续块且不列出自己；GET 响应统一带 Content-Format | 91.5% |
| `internal/codec/knx` | KNX 纯函数编解码（组地址 4/4/8 与物理地址 4/4/8、DPT 切片表与 `Encode`/`Decode`、cEMI `L_Data.req/ind/con`、KNXnet/IP 信封与全部服务报文、HPAI） | 无 I/O、无时钟、无可变状态；1 位 DPT 的值在 APCI 字节 bit0，`ParseLData` 必须把它摘掉（不摘则「开」的写帧解析成 `0x0081`、调用方 switch 落空、报文静默丢弃）；TPDU 长度不含 TPCI 字节与 APCI 高 2 位；DPT 表用切片而不是 map（`9.1` ≡ `9.001`，字符串键要先补零）；`HostPort` 缺端口时补 3671 | 77.5% |
| `internal/driver/knx` | KNXnet/IP 隧道主站（建隧道/心跳/断链重建、8 位序号与重传、组地址读写、`L_Data.con` 短窗口确认、源地址三级优先） | 只做 TUNNELING + 组地址读写；整条会话一把锁串行（一条隧道只有一个通道号 + 一个 8 位序号）；序号**收到 ACK 之后才加一**，超时重传用同一序号，序号错乱唯一可靠的复位是重建隧道；`Open` 只 dial 不建隧道（否则「槽位满」变成驱动加载失败），`Probe` = 建隧道 + CONNECTIONSTATE、不读组地址；写路径 300ms 窗口等 con，收不到按成功、收到且 error 位置位才判 `protocol`——error 位是总线级故障，不是「有没有人在听」；`Address.DPT` 必填且与 `dataType` 静态匹配；点级失败（组地址不存在/DPT 不符）只标该点，有成功点则 `Read` 返回 `nil`，链路级失败（con 超时/隧道断）立即上抛并标坏剩余组地址 | 67.7% |
| `internal/sim/knx` | KNXnet/IP 内置网关（隧道分配与回收、序号严格校验与 8 位回绕、组地址对象表、`L_Data.con`、四种故障注入） | 与主站共用 codec/knx；重传只重发 ACK 不重复执行；**只更新已有的组地址对象**（凭空造对象会让「点表地址写错」从测试里消失）；读未知地址只回 ACK + con、不回 `GroupValueResponse`（让客户端正常超时）；槽位满回明确状态码而不是静默丢弃；`DefaultObjects()` 是对象表的唯一事实来源 | 85.2% |
| `internal/codec/iec61850` | IEC 61850 纯函数编解码（TPKT / COTP / 会话 LI 与 SPDU / 表示层 / ACSE / MMS Initiate 与 Read/Write/getNameList / 数据值与访问结果 / 对象引用与 FC） | 无 I/O、无时钟；**会话层 LI 与 BER 长度是两套编码**（`0xFF` 后恰好两字节大端，不是 BER 长形式）；封套归属不对称（COTP CR/CC 自带 TPKT，其余必须先 `WrapData` 再 `BuildCOTPData`）；读写请求体里 `listOfVariable` 与 `listOfData` 都编码成 `[0]`（同为 `0xA0`），只能按出现顺序区分；FC 严格限定 ST/MX/CO/SP/CF/DC/SG/SE（`MOD` 不是标准 FC，会被拒） | 70.0% |
| `internal/driver/iec61850` | IEC 61850（MMS）主站（五段关联握手、invokeID 严格配对、读写变量、FC 补全、单项错误分类、`Browse` 分页枚举） | 只做 ACSE + MMS 初始化 + 读/写变量 + 浏览；`Open` 只 dial（关联延迟到首次读写，否则「装置重启中」会表现成加载失败）；FC 必须显式或由 `labels.defaultFC` 补齐，不从属性名反推；整条关联一把锁串行化；迟到响应按 invokeID 丢弃；数据访问错误逐条映射（10/4/5/6→address、3→permission、2/1→protocol 可重试）；`Browse` 能力位 unsupported，一次性枚举走包级入口；点级失败（数据访问错误）只标该点，有成功点则 `Read` 返回 `nil`，链路级失败立即上抛并标坏剩余点 | 63.2% |
| `internal/sim/iec61850` | IEC 61850 内置 IED（变量树 / 关联可拒 / 读写 / getNameList 分页 / 有序释放 / 读超时注入） | 与主站共用 codec/iec61850；`getNameList` 只回**项名**（剥掉域前缀，域由 objectScope 携带，重复带域会让客户端拼成 `LD/LD.…`）；未知对象回 `object-non-existent` 而不是默认值；读未知对象逐项报错、写只落在已存在对象上；关联被拒不计入成功关联 | 83.3% |
| `internal/codec/dlt645` | 645-2007 纯函数编解码（前导/双 68 定界/地址 BCD 低字节先传/数据域 +0x33/校验和/数据标识 DI 表/压缩 BCD 符号位/时间与地址/收帧扫描/后续帧序号） | 无 I/O、无时钟、无可变状态；前导不计入长度与校验和，CS 从第一个 `0x68` 起到 DATA 末；地址域低字节先传、缩位补 `AA` 通配；DI 按手册顺序 `DI3DI2DI1DI0`，数据块（任一字节 FF）宽度为 0 由应答决定；读数据域上限 200、写上限 50（含 12 字节头）；解码只做「按 DI 小数位换算工程值」，不施加点位 Scale/Bias | 60.3% |
| `internal/driver/dlt645` | 645 主站（serial/tcp 双承载、总线锁、探针 0x13、逐点读 + 后续帧重组、写权限闸门、错误分类） | 协议一次只能读一个 DI，`MultiRead` 声明 unsupported；同一 DI 的多个点位共用一次事务；链路级故障后本轮剩余点快速失败；非数据块长度必须等于 DI 宽度；数据块只按 bytes 交付；`length` 由 DI 表推导、点表写错在驱动层报错；表地址取 `labels.address` 而不是 `unitId`；写命令受 `allow-write`/DI 可写/设备密码三道闸门约束，`allow-control` 被显式拒绝；适配器回显按 D7 跳过、codec 层拒绝 D7=0；后续帧上限按帧数 + 字节数双重约束；广播命令不提供入口 | 67.3% |
| `internal/sim/dlt645` | 645 内置从站（TCP + `ServeStream`、按标准宽度回数据、地址过滤、后续帧拆分、写权限与密码、广播校时静默生效、拉合闸） | 与主站共用 codec；地址不匹配静默（RS-485 上不能替别人应答）；读地址应答用实际地址而非回显全 `AA`；读时钟从内部时钟现场生成，写时钟同时推进内部时钟；数据块超过单帧容量按 `followChunk` 拆分，序号编码偏移让无状态续传正确 | 82.7% |
| `internal/codec/cjt188` | 188-2004 纯函数编解码（单 `68` 定界/地址 BCD 低字节先传/无 `0x33`/校验和/2 字节 DI 大端/按表类型分派的字段 schema/压缩 BCD/7 字节时间/单位代号/收帧扫描） | 无 I/O、无时钟、无可变状态；前导不计入长度与校验和，CS 从 `0x68` 到 DATA 末；帧总长恒为 `13+L`；数据标识 DI 按 2004 大端传输（`901F`→`90 1F`，DI0 高字节在前），与地址/数值的「低字节先传」相反，2018 版才改小端；901F 水/气/其他与热量表两套布局（19/43 字节），D120..D12B 为 5 字节；字段 offset 是 DI 数据域内字节偏移；单位是显式字节并统一到 m³/m³/h/GJ/kW，未知代号报错；时间 7 字节、年份 2000..2099，日期本地校验；读地址报文标识 `81 0A` | 64.5% |
| `internal/driver/cjt188` | 188 主站（serial/tcp 双承载、总线锁、探针 0x03、按 DI 缓存字段、阀门/写地址两道闸门、错误分类） | 同一 DI 的多个字段在一次 Read 内复用一次事务，跨 DI 不合并、`MultiRead` 声明 unsupported；resolved 字段必须与 schema 宽度一致；应答地址/DI/SER/长度四项校验；链路级故障后本轮剩余点快速失败；表地址取 `labels.address`（完整 14 位，含 AA 通配即拒绝）、类型取 `labels.type`；写阀门需 `allow-control`，写地址需 `allow-write` 且成功后自动切到新地址；其余 DI 写本地拒绝；不提供广播 | 77.6% |
| `internal/sim/cjt188` | 188 内置从站（TCP + `ServeStream`、水/热两套 901F、D120..D12B、读地址、阀门写、写地址、时钟刷新、地址过滤） | 与主站共用 codec/schema；地址不匹配静默；读地址应答用实际地址而非回显全 `AA`；时间与状态字每次读都从内部时钟/阀门状态现场生成；写地址后新地址可读、旧地址静默；未知命令回异常应答而不是让主站等超时；`Close` 幂等 | 88.2% |
| `internal/codec/mbus` | M-Bus（EN 13757-2/3）纯函数编解码（`E5` 单字符 / 短帧 / 长帧 / 控制帧四类分帧、`C+A+CI+DATA` 模 256 校验、双 `L` 一致性与整帧长 `L+6`、显式拒绝 `FDh` 次级地址；DIF/DIFE 与 VIF/VIFE 记录解析、整数/BCD/REAL/变长 LVAR、Type G/F/I 日期时间与无效位、固定数据结构、统一 VIF 表与幂次换算、`q:`/`r:` 反查与记录编码） | 无 I/O、无时钟、无可变状态；校验和**只覆盖** `C+A+CI+DATA`，不覆盖 L 与起止符（按整帧算会固定差 3）；长帧两个 `L` 必须相等、整帧长恒为 `L+6`；`0x68` 既是长帧头也是控制帧头，`E5` 只有 1 字节，扫描器必须能把它从粘包中单独切出；`FDh` 次级地址在编解码层直接拒绝（`ErrFrame`）；可变结构固定头恒 12 字节，少读一字节会让全部 VIF 错位；VIF 表按区间 + 幂次生成而不是手抄；时间无效位 → 文本 `(时间无效)`，不把 `0xFF` 解成 2159 年；`DIF=1Fh` 后续报文标志必须暴露 | 93.2% |
| `internal/driver/mbus` | M-Bus 主站（SND_NKE 初始化、REQ_UD2 整表上送、FCB 跨轮翻转与同轮重放、失败后 NKE 复位再试、后续报文跟随上限、状态字降质量、`q:` 物理量寻址 + `r:` 逃生口、坏帧计数区分「无字节」与「有字节不成帧」；自检 `Table`） | 只做主地址 0..250 的单一主站轮询，251/253/254/255 配置期拒绝；`MultiRead` 声明 supported（一条 RSP_UD 覆盖整表，同报文内点位天然同快照）；同一轮重试**保持 FCB 不变**、跨轮才翻转（从站据此回放上次应答，访问号不累加）；读取失败后补发一次 `SND_NKE` 复位再重试；`Write`（SND_UD）声明 unsupported 并逐项返回 `ErrUnsupported`；状态字只有 bit0-1 非零才降质量为 uncertain（电源低/永久错误不影响读数可信度）；点表 `AddressPolicy{Width: WidthNone, AllowSymbol: true}`，`q:<物理量>[@存储号][/第几条]` 与 `r:<index>` 才算合法；`Reader.BadFrames()` 非零而好帧为零时报协议错并提示查波特率/校验位/终端电阻，而不是笼统超时；串口 `concurrency` 必须为 1 | 86.7% |
| `internal/sim/mbus` | M-Bus 内置表计（虚拟热表 8 条记录 / FCB 重放不改访问号 / 访问号自增 / 地址过滤静默 / 校时与坏校验注入 / 静默模式 / 状态位与后续报文标志 / 值可变） | 与主站共用 codec/mbus；同一 FCB 重放回放上一次应答且**不增加访问号**（这是「同轮重传不重复读数」唯一可测的方式）；地址不符完全沉默（替别人应答会让主站张冠李戴）；坏校验只改校验和、结构保持完好（验证主站真的靠校验和发现问题）；`DIF=1Fh` 后续报文标志用于验证驱动的 follow 上限；`defaultRecords()` 与 `examples/mbus.json` 一一对应，`RecordNames` 的输出能被 `FindRecord` 直接吃回去 | 80.6% |
| `internal/codec/dlms` | DLMS/COSEM（IEC 62056-46/-53/-62）纯函数编解码（HDLC 类型 3 帧按长度字段定界、HCS/FCS 两段 CRC-16/X-25、1/2/4 字节 7 位地址域、LLC 头请求/应答区分、I/RR/RNR/U 帧控制域；A-XDR 全标签编解码含 `date_time` 12 字节布局与时区偏差；AARQ/AARE 的 BER 逐段解析、xDLMS-Initiate 协商、Get-Request-Normal / Get-Response 三分支、`data-access-result` 2 字节形态；OBIS 六段解析与 `SplitRef` 地址切分、内置常见对象表、COSEM 类号/单位枚举、`ApplyScaler` 单次乘除） | 无 I/O、无时钟、无可变状态；长度字段 = 两标志之间全部字节（含 2 字节格式字段与末尾 FCS，不含标志），整帧 = `len(body)+2`，11 位跨两字节且 bit11 是分段位；**HCS 覆盖 `format+dest+src+ctl`**，必须先算长度再算 HCS，占位回填会让接收方判定头校验失败；**类型 3 帧不做透明转义**，`0x7E` 可合法出现在 info 与 FCS，分帧必须按长度字段定界（用 `1.0.13.7.0.255` 的 FCS `7E 64` 做回归）；U 帧控制域是固定常量、不携带可变 P/F；`E7` 是「这一帧是应答」的唯一信号；`date_time` 是 `年(2) 月 日 星期 时 分 秒 百分秒(2) 偏差(2) 状态`，**星期在日月之后、时分之前**；`data-access-result` 是 `[1] IMPLICIT ENUMERATED`，线上只有 `01 <值>` 两字节，**不兼容** `01 01 xx`（结果码为 `hardware-fault(1)` 时前两字节相同，兼容会把它读成数据不完整）；`ApplyScaler` 一次乘除 `10^|scaler|`，逐次除 10 会得到 `50.010000000000005`；单位枚举 Wh=29 / VAh=30 / varh=31，写错不报错只标错量纲 | 92.4% |
| `internal/driver/dlms` | DLMS/COSEM 主站（LN 引用 + 无安全 + HDLC；SNRM→AARQ→GET→DISC 建链与断链、I 帧序号维护、`<OBIS>[:class[:attr]]` 地址推导、scaler 自动应用与进程内缓存、`data-access-result` 分类、坏帧归 `ErrProtocol`、链路错误缓存；自检 `Scan`/`Association`） | LN 引用 + Lowest Level Security + HDLC 三层承载，点表 `AddressPolicy{Width: WidthNone, AllowSymbol: true}`；只写 OBIS 时类号/属性号从内置表推导，**查不到报 `ErrAddress` 而不猜**（猜的类号会读到另一个对象的合法数字）；`SplitRef` 是唯一地址切分点（标准写法 `1-0:1-8-0-255` 自带冒号，靠「前缀必须解析成 6 段 OBIS」确定切点）；scaler 读 attribute 3 后按 `10^scaler` 应用并缓存（scaler 是静态配置参数，**只缓存确定结论**——类级「无 scaler_unit」与读成功，Register 家族读失败不缓存、交回判坏而非报 raw good 静默回绕）；`data-access-result` 归 `ErrAddress`（object-undefined / object-class-inconsistent 都指向点表写错，重试无用）；分块读报 `ErrUnsupported` 并提示调大 `max-pdu`；`Write`/`MultiRead`/`Browse` 声明 unsupported（一条 Get 只读一个属性，`get-request-with-list` 需服务端声明 multiple-references）；坏帧报协议错并提示查波特率/校验位（HDLC profile 8N1）与终端电阻；链路级失败记住 `linkErr`，不再逐点等超时；`Probe` 打印协商后的 conformance 而非请求值 | 87.8% |
| `internal/sim/dlms` | DLMS 内置电表（三相表 40+ 项，值带 scaler_unit；SNRM/DISC 应答、AARQ 接受或拒绝、Get-Request 分派、未建关联读回 `read-write-denied`、对象未定义 / 类号不一致两种结果码、静默 / 坏校验 / 拒绝关联三种故障注入） | 与主站共用 codec/dlms，主站解出的值与从站放进去的值不会因为两边各写一套字节规则而漂移；地址不符完全沉默（总线串扰时替别人应答会让主站张冠李戴）；坏校验只改 FCS 前一字节、结构保持完好（验证主站真的靠校验序列发现问题，而不是靠内容）；未建关联就读回 `read-write-denied` 而不是 DM，自检能拿到明确原因；时钟点是固定 12 字节（`2026-11-13T08:30:00`），不用它断言「当前时间」；默认对象表与 `examples/dlms.json` 一一对应 | 91.7% |
| `internal/codec/sl651` | SL 651（水文监测数据通信规约）纯函数编解码（按长度字段分帧而非扫 `7E`、长度字段语义与整帧 `17+长度`、CRC-16/MODBUS 变体不取反且高位在前、上下行地址字段反序、附录 C 表 C.1 131 项引导符全表、引导符类固定宽度与固定定义字节、数据定义字节 `(字节数<<3)|小数位`、批量要素 `Slot` 段宽与 `DRP[11]` 段寻址、DRP/DRZ/ZT 按 HEX 解、缺测全 F 识别、均匀时段报 31H 与小时报 34H、查询帧 37H/38H/3AH 构造、ASCII 帧与 M3 多包显式拒绝） | 无 I/O、无时钟、无可变状态；**分帧不能扫 `7E`**——正文与 CRC 里可合法出现 `0x7E`（金向量正文首字节就是 `7E`），只能先读长度字段；长度字段低 12 位是「STX 之后、结束符之前」的字节数，不含 STX/ETX，整帧恒为 `17+长度`；M3 的「包总数+包序号」3 字节**计入**长度；CRC 初值 `FFFF`/右移/多项式 `A001`/**xorout 0000**、高位在前（照抄 X-25 会得 `3C51` 而非 `C3AE`）；**上下行帧的地址字段顺序相反**（表 11 vs 表 12），密码/功能码/长度偏移不变；站号是 5 字节 BCD 装 10 位十进制（`0..9999999999`，按 5 位卡会误判 8 位测站编码）；引导符类（`F0H`/`F1H`/`F4H..FCH` 等）的定义字节被规约写死，宽度**不能**按高 5 位推导（否则 5 字节时间被当 30 字节）；DRP 是 1 个 12 字节数据 = 12 段 × 1 字节、DRZ 是 24 字节 = 12 段 × 2 字节，整块当 HEX 整数会解出 `1.8e18` 溢出值且不报错；DRP/DRZ 是 HEX × LSB 而非 BCD（`0x01F4` 是 5.00m 不是 194）；缺测是与数据位数相同的 `F` 而不是 9999；`FFXXH` 自定义扩展报 `ErrUnknownGuide` | 95.2% |
| `internal/codec/toledo` | METTLER TOLEDO 连续输出纯函数编解码（按 CR/LF 分帧而非扫 STX、行首必须是已知状态助记符或 STX、三段式 `ST,GS,+00149.0kg` 状态/毛净/符号+数值+单位解析与组帧、`splitSigned` 字符串拆分保大值精度、lb oz 双单位归一化、二进制 TOLEDO 帧 STX 识别并给出「切 ASCII」指引） | 无 I/O、无时钟、无可变状态；**分帧不能扫 STX**——STX(0x02) 是合法帧首（二进制 18 字节格式），行尾只有 CR/LF，所以强制「首字节 ∈ {大写 S/T/U/O/L, STX}」为帧起点，其余字节计 `Dropped`；帧体含 `.` 不被误判为行尾；数值段固定 6 位宽（WEIGHT(7)=5 位整数+`.`+1 位小数），越界报 `ErrBadFrame` 而非硬解；`splitSigned` 走字符串拆分而非先乘 10^n 取整（float64 尾数 15~16 位，工业大毛重加小数易越界）；单位集合开放不校验白名单；`Parse` 遇 STX 开头返回带「ASCII」指引的可执行错误而非静默吞掉（否则仪表被设成二进制格式时那句提示永远发不出去）；`decimals<0` 已在 `splitSigned` 内钳 0（否则切片越界 panic） | 92.4% |
| `internal/driver/sl651` | SL 651 中心站驱动（一次 37H 取整帧全部要素、应答沿用查询功能码、`<要素>` 与 `<要素>[<段号>]` 地址、`VT` 走 `Report.Voltage` 兜底、缺测降 `quality=bad`、坏帧计数区分「无字节」与「有字节不成帧」、断连与超时都缓存 `linkErr` 后续 `Read` 短路；自检 `Report`） | 只做中心站侧查询（37H/38H/3AH）与上行解析；`MultiRead` 声明 supported（一条 37H 应答覆盖整帧，同报文内点位天然同快照）；逐点发 37H 不但慢，还会让同一设备的点位落在不同快照上；应答**沿用查询功能码**，因此判「上行可解析报文」必须用 `IsResponse` 而不是只认自报类 `IsUplinkReport`；`VT`（`38H`）独立编报、不在要素组内，必须单独兜底查 `Report.Voltage`（否则 VT 点位永远读不到而其它一切正常）；缺测时段按 `quality=bad` 交付而不是报地址错（规约允许该时段无数据）；段号只对批量要素有效，`Z[1]` 提前报地址错；`Write` 声明 unsupported（配置表/控制命令/人工置数各有语义与应答格式，写错会改终端参数或控制闸门水泵）；坏帧非零而好帧为零时报协议错并提示查波特率/校验位；**断连（写失败/读 EOF）与超时都缓存进 `linkErr`**——断连走早退路径，原先漏了缓存会让终端掉线后每轮 `Read` 仍往坏链路再写一遍 37H，与静默超时缓存对称（同 cjt188）；串口 `concurrency` 必须为 1 | 88.4% |
| `internal/driver/toledo` | METTLER TOLEDO 连续输出只读驱动（推送模型收帧、symbol 字段寻址、状态到质量码降级、坏帧计数区分「无字节」与「有字节不成帧」、收帧统计 Stats、自检链路诊断） | 衡器主动推帧无站号/无命令，驱动只做客户端（C）+只读：`Open` 起后台 `pump()` 收帧存最新 `Reading`，`Read` 从内存取（无网络交互，耗时与点数无关，链路安静不超时）；**一帧没收到时 `Read` 不抛顶层错误、逐点降级为 comm_error 返回 nil**——本驱动与 MQTT 同为推送型（§3.10.6），衡器此刻链路是好的，把 `ErrProtocol`（有字节但解不出）/`ErrTimeout`（没字节）当 `Read` 顶层错误上抛会被 collector 按 §3.3.1 Close 拆链+consecutive 熔断，让正常设备每周期重连；两种故障的区分改由 `Stats()` 承载（`bytesSeen==0` 查接线/供电，`bytesSeen>0 且坏帧>0 而好帧==0` 查帧格式/波特率），只有 `Probe` 路径才把它们作为顶层错误抛出让运维失败退出；质量码：从未收帧=comm_error、超 staleAfter=stale、US 运动=uncertain（仅重量）、OL/UL=bad（仅重量）、掩码=bad（仅重量），status/unit/mode/decimals/stable/motion 描述性字段恒 good；`readTimeout` 校正后必须**严格小于** staleAfter（先对半再设 1s 下限，顺序不能反，否则 staleAfter≤2s 时两者相等掩盖断链）；`Write` 逐项返回 `ErrUnsupported`（顶层错误会被采集层摊成「全部点写失败+comm_error」像链路故障）；`Capabilities` 全字段显式声明（含 `Discovery`/`BitAccess`=unsupported，留空会序列化成空串非四态），`Subscribe`=supported（协议本身推送型，推送在包内消化）；点位用 symbol 地址（weight/status/unit/mode/stable/motion/decimals），`KnownSymbol` 在 Open 校验；`concurrency` 必须为 1 | 89.1% |
| `internal/sim/sl651` | SL 651 内置遥测站（7 要素覆盖 BCD/HEX/状态字/整组缺测四分支、37H/38H/3AH 分派、自报帧 2F/30/32/34、密码与站号过滤静默、坏校验注入、值可变、掉线注入） | 与主站共用 codec/sl651；站号/密码不符完全静默（密码错时终端本来就静默，不做口令预言机）；`defaultFields()` 刻意覆盖四种编码分支（`Z`/`P1`/`PT`/`Q` 常规 BCD、`ZT` 状态字、`DRP` 批量 HEX 含分段缺测、`Z1` 整组缺测），自检一次就能走到 `Field.NoData` 与驱动的缺测降级；电压字面量必须是合法 BCD（`0x1210` = 12.10V，用 `0x0A` 会造出非法 BCD 而让自检「成功但值荒谬」）；坏校验只改 CRC、结构保持完好（验证主站真的靠校验序列发现问题）；`Value` 对缺测要素返回 `ok=false` 而不是 9999；**`SetDropOnQuery` 收到下行查询即断开、不回应，模拟终端掉线/停电，中心站读拿到 EOF 归断连（区别于 `SetSilent` 的超时）**；`Trigger` 在 `net.Pipe` 上必须配 goroutine（同步管道写会一直阻塞到对端读走） | 82.6% |
| `internal/sim/toledo` | METTLER TOLEDO 内置衡器（ASCII 连续输出 ~20 帧/秒、可配小数位/推帧间隔/初始值/单位、ST/US/OL/UL 全状态覆盖、静默模式、Set 热变值） | 与主站共用 codec/toledo；帧格式有 D39 手册逐字节实例支撑（`53 54 2C 47 53 2C 2B 30 30 31 34 39 2E 30 6B 67`=`ST,GS,+00149.0kg`）；默认 50ms/帧≈20 帧/秒落在现场常见 10~25 帧/秒；`WithSilent` 不推任何帧（模拟衡器掉电，验证驱动 comm_error 而非看起来正常）；`WithReading` 直接给整帧读数（含 US/OL/UL 状态与 lb oz 单位）；小数位决定数值段帧宽，自检一次能覆盖稳定/运动/超量程/欠量程四分支到质量码降级 | 91.7% |
| `internal/codec/snmp` | SNMP v1/v2c/v3 纯函数编解码（BER TLV / 整数最短补码 / OID base-128 / 7 类 PDU / varbind / 异常标记 / error-status 分类；v3 另有 USM 安全参数、`BuildV3`/`ParseV3`、口令本地化、HMAC 摘要、DES/AES 加解密、Report 分类、时间窗口判定） | 无 I/O、无时钟、无可变状态；长度短/长形式必须按内容长度选；无符号值最高位为 1 必须补前导 `00`；OID 首两弧合并、`2.39.1`→`77 01`；异常标记只允许出现在应答，请求里出现即拒绝；v1 Trap PDU 识别后显式拒绝；v3 空 engineID 只允许出现在发现报文，带 auth/priv 时空 engineID 拒绝；摘要截断长度 = 摘要长度的一半 | 77.7% |
| `internal/driver/snmp` | SNMP 主站（v1/v2c 批量 GET、v1 `noSuchName` 退化、单 OID SET、类型映射与回显校验、错误分类、request-id 配对、自检 `GetRaw`/`Walk`；v3 引擎发现、时间同步、密钥派生与缓存、Report 驱动的自愈重试） | 只做 v1/v2c 的读/写路径不变；通道必须是 UDP，地址省略端口补 161；读按 `maxOids` 与 1400 字节双预算分批，顺序不变；v1 批量 `noSuchName` 逐点重发；写不合并 varbind，逐点错误隔离；只接受与请求 request-id 相同的 Response；float/BCD 与 >2^32-1 的无符号写本地拒绝；点级错误（地址/线类型不符）只标该点、有成功点则 `Read` 返回 `nil`、全失败才上抛；链路级失败（超时/断连）立即返回并标坏剩余批次；v3 加密口令缺省回退认证口令、USM 口令 8..64 字节、AES-192/256 需 SHA-256+；固定 engineID 与代理不一致时同时报出两个 engineID 并拒绝连接 | 73.0% |
| `internal/sim/snmp` | SNMP 内置代理（system 组 + 两块网卡、GET/GETNEXT/GETBULK/SET、community 静默丢弃、v1/v2 差异、SET 原子性；v3 真做 HMAC 验签与 DES/AES 解密、6 类 Report、引擎身份可切换、故障注入） | 与主站共用 codec/snmp；community 不匹配不回任何报文（不做口令预言机）；v1 缺 OID 回 `noSuchName` 带 `error-index`，v2c 回 `noSuchObject`；SET 先全量校验再落地；`sysUpTime` 固定值（测试要稳定）；`Close` 幂等；v3 对 USM 失败默认回 Report，`Faults.DropPercent` 模拟真机静默丢弃 | 85.1% | 85.1% | 85.1% |
| `internal/codec/ads` | ADS/AMS 纯函数编解码（6 字节 AMS/TCP 头、32 字节 AMS 头、7 类命令、两层长度一致性、NetId 解析、错误码分类） | 无 I/O、无时钟、无可变状态；TCP 长度含 AMS 头、AMS 长度只含载荷，少写一层会让对端把载荷开头当长度；flags 低位是应答位、`0x0004` 是 ADS 位；invoke 0 保留；错误码分 AMS 与 ADS 两段，只有临时性错误可重试 | 86.7% |
| `internal/driver/ads` | ADS/AMS 主站（符号名→句柄→值、句柄缓存与失效重解析、逐点事务、类型范围检查、AMS/ADS 错误分类、自检 `DeviceInfo`/`DeviceState`/`WriteControl`） | 地址必须是 `symbol`，`index>0` 直接报地址错；读写在返回 `invalid offset/group` 时只重解析一次；写前拒绝越界/小数/BCD，字符串刚好占满整段也拒绝；只重试超时与临时性协议错，断链与地址/权限错不重试；点级错误（地址/权限/不支持/宽度不符）只标该点、有成功点则 `Read` 返回 `nil`，全失败才上抛；链路级失败立即标坏剩余点 | 92.8% |
| `internal/sim/ads` | ADS/AMS 内置服务端（默认变量表、按名分配句柄、只读标记、宽度校验、状态控制、句柄失效模拟） | 与主站共用 codec/ads；变量表扁平、每个数组元素单独占一个符号名（与真实符号表一致）；句柄全局编号以便一次调用让所有句柄失效；未知 IndexGroup 一律 `invalid group`，通知帧不应答 | 96.9% |
| `internal/codec/secs` | SECS 纯函数编解码（SEMI E37 HSMS 帧：4 字节长度前缀（含 10 字节头）+ SessionID/PType/SType，控制消息固定 SessionID=0xFFFF 且不带载荷、Select.rsp 状态在头字节 3；SECS-II 项编解码、`MaxPayload` 上限、SxFy 消息体语义） | 无 I/O、无时钟、无可变状态；格式字节低 2 位是长度字节数（`00` 视同 1）不是长度值；L 项长度是子项个数；整数定宽大端，宽度不符即报错而不是补零；BOOLEAN 非 0 即 true；数据长度上限 16 MiB，读、解、编三处都校验；L 项声明数超过剩余字节数直接报错，不按声明数预分配 | 94.6% |
| `internal/driver/secs` | HSMS 主站（Select 握手、SystemBytes + Stream + Function+1 + W 位清零四条件配对、按对象分组批量读、S2F15 写常数、Linktest 探活、`Describe`/`RawRead` 自检扩展） | 只做 TCP 主站；Object 地址 `svid:<id>.value`（只读）/ `ecid:<id>.value`（可写）；读按对象类型分组，svid+ecid 混排是两次事务；未匹配的消息（异步 S6F11、迟到应答）丢弃继续读而不是判失败；写只认点表内的点，按地址临时构造的写请求一律 `permission`（读允许按地址解析）；ACKC7≠0 表示整批被拒；`attemptDeadline` 取设备超时与 ctx 的更早者；组内单点失败（缺编号/格式错）不拖垮同组其它点，有成功点则 `Read` 返回 `nil`，全失败才上抛，链路级失败短路后续组 | 89.1% |
| `internal/sim/secs` | HSMS 内置设备（SVID/ECID 双编号空间、Select 门控、常数写拒绝未知编号、事件与报警推送） | 与主站共用 codec/secs；SVID 与 ECID 用 `(对象类型, 编号)` 作键，同号互不干扰；未选中时 DATA 回 Reject；写未知 ECID 回 ACKC7≠0（不新建）；S1F11 空列表=全部 SVID；设备侧只设写截止时间（`SetDeadline` 会让空闲连接被判超时） | 95.1% |
| `internal/codec/mtconnect` | MTConnect 纯函数编解码（按本地名匹配、忽略命名空间；Header 序号与 `nextSequence` 补齐；Samples/Events/Conditions 三类观测；两层与平铺条件写法及按严重度合并；`/probe` 设备模型；`MTConnectError` 文档；ISO-8859-1/latin-1/windows-1252 与 UTF-8 BOM） | 无 I/O、无时钟、无可变状态；只按本地名匹配（带 `mt:` 前缀的文档照样解析）；两层条件的外层元素名是条件类型、内层是级别，平铺写法用 `type` 属性承载类型，两种写法必须归一到同一个 `ConditionLevel`；同一 ID 多条条件按 `FAULT > WARNING > UNAVAILABLE > NORMAL` 合并而不是「后者覆盖」，**未知级别（读不出严重度）排最高、且按 `Category` 而非 `IsCondition()` 判定是否参与合并**（否则一条级别读不出的报警会被同 ID 的 NORMAL 顶掉、静默变 good）；**数值属性（`sequence`/`bufferSize`/`instanceId`）用容错解析，空或非数字降级为 0 而不使整份文档失败**；`UNAVAILABLE` 是保留字不是数值；不支持的字符集显式报错，不静默产出乱码 | 80.3% |
| `internal/driver/mtconnect` | MTConnect 主站（一条 `/current` 覆盖全设备、单点问题只降该点质量、`/probe` 失败退回 `/current`、多设备按 uuid/name 选择、基址子路径与 gzip、只读边界；自检 `ProbeDevices`/`Current`/`Sample`） | 只做 HTTP + XML 只读；地址必须是 `symbol` 且只含一个 `dataItemId`（拒绝 selector/index/length）；`Read` 只在「文档拿不到 / 解析不了 / 设备选不出来」时返回错误，`UNAVAILABLE`→bad、ID 不存在或类型解析失败→address_error，一律只写进该点；条件点级别即质量（FAULT→bad、WARNING→uncertain）；多设备未选时报地址错误并列可选设备；省略端口补 5000，拒绝 URL 内凭据/query/fragment；文档上限 32 MiB | 81.3% |
| `internal/sim/mtconnect` | MTConnect 内置 Agent（三轴加工中心、`/probe`/`/current`/`/sample` 序号窗口、两层条件渲染、`toolOffset` 初始 UNAVAILABLE、gzip 与子路径、七类故障注入） | 与主站共用 codec/mtconnect；`toolOffset` 初始 UNAVAILABLE（真实机床最常见的「数据项存在但无值」）；条件用规范两层写法渲染；`/sample` 按环形缓冲的序号窗口返回，旧观测被顶掉；`/probe` 只列组件下的数据项，不重复平铺；故障注入（状态码/探针失败/畸形 XML/HTML/延迟/省略数据项/全 UNAVAILABLE/条件）各自独立，测试只打开自己关心的那一个 | 92.9% |
| `internal/driver/link` | 表计协议的承载层：串口与串口服务器（TCP 透传）统一成一条带总线锁的链路 | 锁覆盖「排空 → 发送 → 收完帧」整段；TCP 上 Drain 退化为读掉残留字节（分帧同步仍由 codec 扫描兜底）；`host` 省略端口时补默认端口；参数错误 `ErrUnsupported`、打开/连接失败 `ErrDisconnected`；`Drain` 必须早于 `SetDeadline` | 91.2% |

| `internal/planner` | 点位 → 批量读请求（ReadSpan） | 协议无关；按连续性/上限合并；按 `area:db` 分组；计算点合成单个 `expr` span（漏组会表现为全坏且无日志） | 92.6% |
| `internal/scheduler` | tick、actor、信号量、退避、熔断、代次 | 同设备不并发；写优先；旧代次丢弃；状态回调不占采集路径 | 87.8% |
| `internal/pipeline` | 缩放、质量判定、死区过滤 | 质量不可用时不参与死区比较 | 92.7% |
| `internal/cache` | 当前值唯一事实源 + 变更订阅 | 读写都要过这里，不允许模块自留副本 | 96.7% |
| `internal/outbox` | 持久队列：分区、WAL、确认位点、三重上限 | at-least-once；整批或前缀成功才推进位点 | 77.4% |
| `internal/config` | 加载、校验、规范化、版本化、回滚 | 幂等 Normalize；原子写；失败自动恢复；北向参数（含 Sparkplug 组/节点名与格式互斥）、从站映射、计算点（编译/引用/类型/环/窗口）、报警（条件/引用/类型/窗口）与历史保留策略静态校验 | 83.9% |
| `internal/obs` | 指标、结构化日志、日志环形缓冲 | 指标标签必须归一化（不能含原始 URL） | 88.2% |
| `internal/rbac` | 角色/权限矩阵（纯函数） | 未知角色/未知权限一律拒绝；`viewer ⊆ operator ⊆ admin`；高危命令按动作映射到 `command.danger` | 100% |
| `internal/auth` | 凭据表：Token → 身份；`Reloadable` 支持热替换 | Token 先 SHA-256 再定长比较；无凭据进入 open 模式而不是造默认口令；拒绝弱口令；枚举永不返回 Token | 96.7% |
| `internal/audit` | 审计存储：NDJSON 分段 + SHA-256 哈希链 + 查询/校验 | 序号连续；字段边界用「长度:内容」编码；整段淘汰后链仍可校验；尾部半行触发新段 | 80.7% |
| `internal/northbound/httpapi` | REST + SSE + 认证/授权 + 错误码 | 错误码稳定；响应不泄露凭据；未知角色全拒；401 与 403 在审计里区分；报警/历史查询的未知参数取值必须报错而不是静默忽略 | 83.4% |
| `internal/codec/sparkplug` | Sparkplug B 载荷的纯函数 Protobuf 编解码（varint/长度前缀/定长、Metric oneof、PropertySet、未知字段跳过） | 无第三方 protobuf 运行时；未知字段按规则跳过（对端升级 schema 不应让解析失败）；oneof 值字段即使为 0/false 也必须显式编码；`seq` 同样不能按「0 即缺省」省略（NBIRTH/DBIRTH 的序号就是 0，proto2 里「字段缺失」≠「值为 0」），NDEATH 则必须不带序号；声明类型与承载字段冲突时报错；PropertySet 是平行数组，键必须排序才能确定性编码 | 86.3% |
| `internal/sparkplug` | Sparkplug B **Edge Node** 生命周期状态机（主题、NBIRTH/NDEATH/DBIRTH/DDEATH/DDATA、序号与回绕、bdSeq、NCMD/DCMD 解析、指标映射） | 只扮演 Edge Node，**不发布 Host 的 STATE 主题**（在线语义由 NBIRTH/NDEATH 表达）；序号是**边缘节点级**的（节点消息与全部设备消息共用 0..255，只有 NBIRTH 重置为 0）；NDEATH 不带也不消耗序号；bdSeq **每个 CONNECT +1**（`BeginSession`）并同时用于遗嘱与 NBIRTH；QoS/retain 按消息类型定死（数据/出生 0、NDEATH 1、订阅 1），不是配置项；非 good 质量用 `is_null` + `quality` 属性；bytes 走 `bytes_value`；命令只认 Rebirth | 96.4% |
| `internal/mqtt` | MQTT 3.1.1 客户端（北向发布 + 南向订阅共用，纯 Go 零依赖） | 单 goroutine 读 socket；QoS1 必须等到 PUBACK；`Ping` 必须等到 PINGRESP（写成功不算活着）；断线立即失败而非超时；`Epoch` 暴露连接代数，供订阅方区分「新会话的第一条消息」；`CleanSession` 恒为 true；遗嘱是 `Options.Will func() *Will`，**每次 CONNECT 重新求值**（bdSeq 必须递增），`ParseConnect` 让 Broker 侧能断言线上承诺 | 70.3% |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 | 不另写 Broker（两份实现必然长歪）；开关一律走 setup 函数而非直接赋字段；`WaitConnections`/`WaitWills` 让测试能断言「线上到底承诺了什么」（CleanSession、遗嘱 QoS/retain、bdSeq 递增） | — |
| `internal/runtime` | 唯一编排层 + 后端实现 + 看门狗 + 审计埋点 + 窗口缓存 | 启动/关闭顺序（失败逆序收口）；`configuredState` 与 `runtimeState` 分离；协议/传输匹配在装配期校验；只向驱动与报警引擎注入只读 `cache.Get` 与窗口数据源；北向 OPC UA 启停与热重建；凭据表热替换后旧用户立即失效；MQTT 北向以 `northboundSink` 接口隔离 json 与 sparkplug-b | 81.1% |
| `cmd/uic` | 单入口：run / sim / mbread / mcread / finsread / hostlinkread / enipread / bacread / mstpread / iec104read / iec101read / mewtocolread / dnp3read / coapread / knxread / iec61850read / uaread / dlt645read / cjt188read / mbusread / dlmsread / sl651read / toledoread / snmpread / adsread / secsread / mtconnectread / s7blocks / s7upload / instantiate / import / token / check / version | 关闭预算 10 秒；`uaread`、`mbread`、`mcread`、`finsread`、`enipread`、`bacread`、`mstpread`、`iec104read`、`iec101read`、`mewtocolread`、`dnp3read`、`coapread`、`knxread`、`iec61850read`、`dlt645read`、`cjt188read`、`mbusread`、`dlmsread`、`sl651read`、`toledoread`、`snmpread`、`adsread`、`secsread`、`mtconnectread` 是不装第三方客户端即可自证链路的自检工具；`toledoread -raw` 额外打印收帧统计（读到字节/噪声/坏帧/最近帧龄/最近一帧原文）——Toledo 只有推送没有命令可发，线上字节是唯一可观测面；块服务放在 CLI 而不是北向 API（动作重、数据大、归操作系统账号管）；模板实例化与点表导入只产出文件，不在运行期动态生成设备 | — |

覆盖率一列是 `go test -cover ./<包>/` 的语句覆盖率（只统计包内测试，不做跨包交叉覆盖），
随测试增删会有 1% 级的漂移；要看当前值请按 §9.2 的命令重跑。

### 9.2 测试与验证命令

```bash
go build ./...                                  # 编译全部
go vet ./...                                    # 静态检查
gofmt -l internal cmd                           # 格式检查（应无输出）
go test -race -count=1 ./...                    # 竞态 + 全量测试
go test -coverprofile=c.out ./... && go tool cover -func=c.out | tail -1
```

当前状态：`go build` / `go vet` / `gofmt` 全部干净，`go test ./...` 覆盖 112 个包，其中 109 个
有测试文件且全部通过（`internal/mqtt/mqtttest` 是纯测试辅助包；`internal/sim/finsmem` 由
`sim/fins` 与 `sim/hostlink` 跨包覆盖）。

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
19. internal/codec/s7/block.go + driver/s7/session.go 有状态块服务：三段式上传与会话释放（40 分钟）
20. internal/template/template.go 字符串字面量替换与「不做算术」的边界（30 分钟）
21. internal/pointtable/address.go + csv.go 协议感知的地址解析与行错误聚合（40 分钟）
22. internal/codec/mc + driver/mc + sim/mc 三菱 MC 3E 三层：帧、RMW 位写、内置从站（60 分钟）
23. internal/codec/fins + driver/fins + sim/fins 欧姆龙 FINS 三层：套娃封装、节点协商、一位一字节（70 分钟）
24. internal/codec/enip + driver/enip + sim/enip EtherNet/IP 三层：三层封装、标签寻址、多服务合包（80 分钟）
25. internal/codec/snmp + driver/snmp + sim/snmp SNMP 三层：BER、OID 编码、双预算分批、v1 退化（80 分钟）
26. internal/codec/snmp/v3.go + usm.go + driver/snmp/v3session.go + sim/snmp/usm.go：USM v3 认证加密、引擎发现与时间同步、密钥派生与自愈重试、真做 HMAC/解密的仿真代理（180 分钟）
27. internal/codec/mbus + driver/mbus + sim/mbus M-Bus 三层：四类帧与校验范围、DIF/VIF 自描述记录、FCB 重传语义与整表上送（100 分钟）
28. internal/codec/dlms + driver/dlms + sim/dlms DLMS/COSEM 三层：HDLC 长度定界与两段 CRC、A-XDR 全标签、AARQ/AARE 协商、OBIS 寻址与 scaler、内置电表（150 分钟）
29. internal/codec/sl651 + driver/sl651 + sim/sl651 SL 651 三层：长度字段分帧与不取反 CRC、131 项引导符表、批量要素段宽与段寻址、查询-应答与整帧取全部、内置遥测站（130 分钟）
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
| S7 块服务（目录 / 属性 / 上传） | ✅ | USER_DATA 组 0x03：列七类块数量、列某类型块号、读单块 78 字节属性；Job 0x1D/0x1E/0x1F 三段式上传（块长度是 5 位十进制 ASCII，分片以 `param[1]` 判最后一片，失败也补 End 释放 PLC 会话）；`uic s7blocks` / `uic s7upload` 可自检；单块上限 16MB |
| S7 符号表 / 报警订阅 | ❌ | 符号寻址需要 S7 浏览服务，报警需要独立 PDU 通道；点位仍需手写地址 |
| 设备模板库 / 实例化 | ✅ | `internal/template` + `uic instantiate` + `examples/templates/` 11 份模板；只做整值替换（无算术），实例化结果必须通过完整校验；模板库测试逐份实例化并跑 `model.Device.Validate` |
| 点表 CSV 导入 | ✅ | `internal/pointtable` + `uic import`；列别名、协议感知的地址解析与长度推导、行错误聚合、new/unchanged/replaced/conflict 合并；默认拒绝静默覆盖 |
| 三菱 MC 3E 主站 + 内置从站 | ✅ | `internal/codec/mc` + `internal/driver/mc` + `internal/sim/mc` + `uic mcread` + `examples/mc.json`；只做 3E 二进制帧 + TCP；位软元件按位打包、X/Y 按十六进制、字软元件 bool 走 RMW 不清零邻位；结束码 0xC052/53/54/56→address_error；主站/从站/编解码三层测试 |
| 欧姆龙 FINS 主站 + 内置从站 | ✅ | `internal/codec/fins` + `internal/driver/fins` + `internal/sim/fins` + `uic finsread` + `examples/fins.json`；只做 FINS/TCP（FINS/UDP 与 Host Link 配置期即拒）；三层封装（FINS/TCP 头 / 10 字节 FINS 帧头 / 命令）各自独立；建链先做节点号协商，`unitId>0` 覆盖目标节点；读写 MRC 同为 0x01、靠 SRC 区分；`offset` 是字地址、`bit` 是字内位号；位访问一位一字节，32 位低字在前（CDAB）、64 位 DCBA；当前 EM 区无位码，bool 走 RMW；结束码 0x11xx→address_error、0x02xx/0x03xx→comm_error；每帧校验 SID；三层测试含真实 TCP 回环 |
| 欧姆龙 Host Link（上位链接 FINS）主站 + 内置从站 | ✅ | `internal/codec/hostlink` + `internal/driver/hostlink` + `internal/sim/hostlink` + `internal/driver/finscore` + `internal/sim/finsmem` + `uic hostlinkread` + `examples/hostlink.json`；只做头码 `FA` 的 FINS 帧（C 模式与跨网长格式配置期即拒）；**命令语义由 `driver/finscore` + `sim/finsmem` 与 FINS/TCP 共用，同一份点表换个 `protocol`（`fins` ↔ `hostlink`）就能跑**；ASCII 信封（请求 `@`+单元号两位十进制+`FA`+等待时间一位十六进制+精简 FINS 头+命令正文+FCS 两位十六进制+`*`+CR，应答多一格结束码、少一格等待时间）；FCS 从 `@` 起异或到正文末；单次上限读 269 字/538 位、写 267 字/534 位（读写差两个字是字符预算反推的结果，照抄 FINS/TCP 的 999 会收到结束码 0x18）；串口出厂 9600-7-E-2（与 Mewtocol 的 9600-8-O-1 相反，只发不收的典型症状）；CIO/WR/HR/AR 走 FINS 原生位写、DM/EM 走字写，无 RMW；信封层结束码 0x13/0x14/0x15/0x18→格式错（修一次永久消失）、0x03/0x23→permission、其余 protocol 可重试；FINS 层结束码与 FINS/TCP 同一张表；不自动重试（无可靠事务号，重发写可能是脏写），重连交给调度器；内置从站单元号不匹配完全沉默、FCS 错回 0x13、头码错回 0x16；三层测试含真实 TCP 回环 |
| EtherNet/IP + CIP 主站 + 内置从站 | ✅ | `internal/codec/enip` + `internal/driver/enip` + `internal/sim/enip` + `uic enipread` + `examples/enip.json`；只做 CIP 显式报文 + TCP 44818 + 符号标签（隐式 I/O、对象/属性寻址、结构体展开配置期即拒）；三层封装（24 字节封装头 / CPF / CIP）、封装长度只算负载、CPF 奇数条目对齐、发送方上下文严格校验；CIP 无事务号，整条会话串行化；地址字段按 CIP 重解释（标签名/元素 offset/按类型解释的 length/位号）；数值线上小端，整元素反转后走统一解码；类型以设备应答为准；STRING 为「4 字节长度 + 容量」；bytes 两阶段读；多服务合包（450 字节拆批、单次 400 字节上限）且设备不支持时自动退回；位写 RMW 只回写目标元素；状态码统一分类，封装 0x64 判可重试；三层测试含真实 TCP 回环与错误路径脚本流 |
| BACnet/IP 主站 + 内置从站 | ✅ | `internal/codec/bacnet` + `internal/driver/bacnet` + `internal/sim/bacnet` + `uic bacread` + `examples/bacnet.json`；只做 ReadProperty（0x0C）/ WriteProperty（0x0F）（不做 Who-Is/I-Am、COV、批量读、分段）；四层封装（BVLC/NPDU/APDU/标签），BVLC 长度只算 NPDU+APDU，Forwarded-NPDU 跳过 4 字节转发头；application 与 context 的 LVT 含义分开实现（context 6/7 是 opening/closing），BOOLEAN 的 LVT 直接承载值；点位用 `kind=object` 寻址（对象类型/实例/属性/数组下标，都支持名字或编号）；写 `present-value` 的标签由对象语义决定（REAL/ENUMERATED/UNSIGNED/CHARACTER STRING）；UDP 用 connected socket、8 位 invoke ID 配对、整条会话串行化、迟到应答丢弃、超时重发 2 次；错误类 1/2→address、40/27→permission、3→disconnected（可重试）、4→permission；三层测试含真实 UDP 回环 |
| BACnet MS/TP 主站 + 内置从站 | ✅ | `internal/codec/bacnet`（复用 NPDU/APDU）+ `internal/codec/bacnet/mstp.go` + `internal/driver/bacnetmstp` + `internal/sim/bacnetmstp` + `uic mstpread` + `examples/bacnet-mstp.json`；MS/TP 分帧 `55 FF\|type\|dest\|src\|len(2,大端)\|hdrCRC\|data\|dataCRC(2,小端)`，头 CRC 用标准特殊移位展开（残差 0x55）、数据 CRC 用 CRC-16/X-25（残差 0xF0B8），最大数据段 501；**定位 sole-master 轮询从站**：本机主站 0..127、从站 128..254，令牌不发生转移；不做多主站令牌环维护/Who-Is/COBS 扩展帧/波特率自适应/ReplyPostponed 上送，空闲不监听总线；被动收到 Token 事务后交还（`nextStation` 或 PollForMaster 现场探测）、PollForMaster/TestRequest 立即应答；串口按物理路径共享、总线锁串行、`concurrency=1`；`presilence`（默认 5ms）留方向切换时间；地址 `kind=object`、从站 MAC 走 `labels.address` 或 `unitId`；串口/TCP 透传两种承载；三层测试含真实 TCP 回环与被动令牌脚本流 |
| BACnet/IP 的边界 | ⚠️ | 不做广播发现、COV 订阅、ReadPropertyMultiple、分段传输、对象浏览；优先级数组可读但不暴露写优先级（避免「写了一直不生效、其实是更高优先级压住」）；实例号必须写在配置里，不能靠发现补齐 |
| IEC 60870-5-104 主站 + 内置从站 | ✅ | `internal/codec/iec104` + `internal/driver/iec104` + `internal/sim/iec104` + `uic iec104read` + `examples/iec104.json`；只做平衡式 TCP + STARTDT/STOPDT/TESTFR + 站总召唤 + 电能量召唤 + 单点/双点/设定值命令（不做文件传输、时钟同步写路径、冗余通道、带时标命令、组召唤；非平衡轮询是独立的 101 驱动）；APCI 长度只算控制域+ASDU、U 帧功能码低两位必须 `11`、I 帧空载荷判错；ASDU 层由 `codec/iec60870` 共享，本包只保留 APCI 与别名转发；`Read` 只查本地最新值表不产生网络交互；质量只来自 ASDU 品质位与 TESTFR 活性（**不做按时间降级**——104 只在变化时上送）；`Open` 只等 STARTDT、召唤超时不拆链、电能量召唤无条件发送、EndOfInit 触发重新召唤并去重；命令类型由 `area` 声明，双点只接受 1/2（0/3 本地即拒），写成功先填缓存标 uncertain；缓存按点位 ID（同一 IOA 的遥信与遥控互不覆盖）；旁路表 `Observed` 让 CLI 不依赖点表就能列出「设备送什么」；三层测试含真实 TCP 回环与注入式假链路 |
| IEC 60870-5-101 主站 + 内置从站 | ✅ | `internal/codec/iec101`（FT1.2 帧）+ `internal/codec/iec60870`（共享应用层）+ `internal/driver/iec101` + `internal/sim/iec101` + `uic iec101read` + `examples/iec101.json`；只做非平衡式主站 + 串口/串口服务器双承载（不做平衡式、FT1.1、文件传输、时钟同步写路径、带时标命令、选择-执行、分组召唤）；`Read` 真实轮询：每轮请求第 1 级、按周期站总召唤 + 电能量召唤、可选第 2 级；长度只计「控制域+链路地址+ASDU」、校验和从控制域起、单字节帧无地址无控制域；剖面 ORG/CA/IOA 宽度全部走设备标签，非法组合配置期拒绝；总召唤不放在 `Open`，终止只认激活终止与初始化结束（激活确认不算结束）；重传复用同一帧字节避免命令重复执行；写入等应用层激活确认，44..47 按 protocol/address 分类；缓存按点位 ID；点表层用 `WidthNone`（远动地址不表达宽度，布尔量不需要位号，`length` 与位号在导入期拒绝）；内置从站覆盖 FCB 去重、ACD、两级队列、召唤快照与终止类型、命令两段确认 |
| Mewtocol 主站 + 内置从站 | ✅ | `internal/codec/mewtocol` + `internal/driver/mewtocol` + `internal/sim/mewtocol` + `uic mewtocolread` + `examples/mewtocol.json`；只做计算机链接 ASCII 裸帧 + 串口/串口服务器双承载（不做 FP2-ET-LAN 16 字节封装、二进制变体、`SC`/`SD` 整块置位、扩展头发送）；一帧三种进制：站号两位十进制、数据区地址五位十进制、接点编号「三位十进制字 + 末位十六进制」、T/C 四位十进制；BCC 从 `%` 起算且不含 CR；读用起止地址（不是起点+个数）、写也带结束地址并由数据长度反推；应答只回显前两位命令码；`MaxWords=24`（写侧比读侧更紧）、`MaxContacts=8`（个数位一位）；`Read` 按区拆 span、按 `[min,max]` 合并后每 24 字切一刀、逐点取位；T/C 只走 `RCS`/`RCP`（按字读会按每字 16 点重编号）；布尔量只走 `WCS` 原子单点写，字区布尔量本地拒绝（协议没有位写）；站号 1..99、广播 `FF`，应答校验站号与回显；错误码四类映射到 address/permission/protocol 并区分可重试；三层测试含真实 TCP 回环与两套接点编号 |
| Host Link FINS 主站 + 内置从站 | ✅ | `internal/codec/hostlink` + `internal/driver/hostlink` + `internal/sim/hostlink` + `internal/driver/finscore` + `internal/sim/finsmem` + `uic hostlinkread` + `examples/hostlink.json`；只做 FINS 帧（头码 `FA`）+ 串口/串口服务器双承载（不做 C 模式、跨网长格式）；命令语义抽进 `finscore`/`finsmem` 与 FINS/TCP 共用，同一份点表换个 `protocol`（`fins`↔`hostlink`）就能跑；帧全可打印 ASCII，六件按字节记的事——等待时间一位十六进制、应答 `FA` 后多一格结束码、FCS 含 `@`、正文一字节两字符、单元号两位十进制、直连 ICF=0x00；单次上限读 269 字 / 538 位、写 267 字 / 534 位（读写差两字是字符预算反推，不是随便统一的）；串口出厂 9600-7-E-2（与 Mewtocol 的 8-O-1 相反）；不自动重试（无可靠事务号，重发写可能是脏写），重连交给调度器；错误分信封层结束码（`0x13/0x14/0x18` 修一次永久消失）与 FINS 层结束码（同 FINS/TCP 一张表）；三层测试含真实 TCP 回环、单元号不匹配沉默、FCS 错 0x13 / 格式错 0x14 / 头码错 0x16 |
| Host Link FINS 的边界 | ⚠️ | 只做 FINS 帧（`FA`），不做 C 模式（`RR`/`RD`/`WD`… 是另一套命令集与 BCD 寻址）；不做跨网长格式（多 12 个路由字段，目标由单元号承载）；不做自动重试（没有可靠事务号，重发一次写可能把同一字写两遍，对「加计数」点位是真实数据错误）；不做多主站 / 令牌传递（单主站模型） |
| DNP3（IEEE 1815）主站 + 内置从站 | ✅ | `internal/codec/dnp3` + `internal/driver/dnp3` + `internal/sim/dnp3` + `uic dnp3read` + `examples/dnp3.json`；只做 TCP 主站 + 类 0/1/2/3 + CROB + 组 41 直接控制（不做串口、安全认证、文件传输、时间同步写路径、SBO 两段式）；三层各自的确认都必须处理——链路 ACK、传输分片、应用层 CON；带 CON 响应与未请求响应立即回确认，否则从站事件缓冲区会被重发撑爆；`Read` 只查本地最新值表；质量只看低 6 位，二进制 bit7 / 双点 bit6-7 是状态不是品质；双点 0/3 → false + uncertain；写操作检查从站回显状态；模拟量输出值在发报文前校验能装进选定线格式（越界/带小数/`NaN` 拒绝）；带时标计数器事件（组 22 v5/v6）按 11/9 字节「值+品质+时标」解析；静态编码 → 解析有往返自洽测试，双点 2 位/点与「变体 2 无独立位图」两个易错点各钉了一个用例 |
| KNXnet/IP 隧道主站 + 内置网关 | ✅ | `internal/codec/knx`（`group.go` 组地址/物理地址 4/4+8、`dpt.go` DPT 切片表、`cemi.go` cEMI、`netip.go` 信封与服务报文）+ `internal/driver/knx` + `internal/sim/knx` + `uic knxread` + `examples/knx.json`；只做 **TUNNELING（UDP 单播隧道 3671）+ 组地址读写**（不做 ROUTING 组播、设备管理连接、KNX Secure、raw/busmon）；**DPT 是必填字段且是地址的一部分**（`1/2/3@9.001`）——组地址不携带类型，由 `dataType` 反推得不到答案（`float32` 既可能是 2 字节 9.xxx 也可能是 4 字节 14.xxx），猜错不报错、只会得到「合法的错值」（21.5 → `3.7e-41`，质量还是 good）；因此 `PolicyFor("knx")` 用 `WidthNone + AllowSymbol + AllowDPT` 强制要求，`Address.Canonical()` 把 `@DPT` 拼回去（否则重复地址检查漏判、改 DPT 不被视为冲突）；DPT 表用切片而非 map（`9.1` ≡ `9.001`，字符串键要先补零，而补零要在三处各写一遍）；**1 位 DPT 的值在 APCI 字节 bit0、不在数据域**，解析时必须摘掉（不摘则「开」的 `GroupValueWrite` 变成 `0x0081`、调用方 switch 落空、报文静默丢弃，现场症状是「写成功但设备没动」），编码侧 TPDU 长度不含 TPCI 字节与 APCI 高位；序号 8 位、**收到 ACK 之后才加一**、超时重传用**同一序号**、序号错乱时唯一可靠的复位是**重建隧道**（通道号与序号是一体的）；整条会话一把锁串行化（一条隧道只有一个通道号 + 一个 8 位序号，总线 9600 波特）；`Open` 只 dial 不建隧道（CONNECT 延后，否则「隧道槽位满」会表现成驱动加载失败），`Probe` = 建隧道 + CONNECTIONSTATE 往返、不读组地址；写路径 300ms 短窗口等 `L_Data.con`，**收不到按成功**、收到且 error 位置位才判 `protocol`——而 error 位是**总线级故障**，不是「这个地址有没有人在听」（组地址是广播语义，写没人订阅的地址在链路层是成功的）；源地址三级优先 `labels.sourceAddress` > CRD > `0.0.0`；心跳 60s（1s..60s）、`ackTimeout` 1s 且必须 ≤ `device.timeout`；三层测试含真实 UDP 回环、1 位 APCI 归一化回归、序号 8 位回绕边界、丢 ACK 重传、序号错乱后重建、通道被回收、槽位满判可重试、网关不回 con 时写仍成功 |
| KNXnet/IP 的边界 | ⚠️ | 不做 ROUTING 组播（需要网络侧组播配合与去重策略）、设备管理连接（属性/内存下载，是另一套服务与权限模型）、KNX Secure（证书与密钥分发是独立运维域）、raw/busmon（抓总线报文不是采集语义）；不做订阅——KNX 隧道客户端只能「读一次」，`Subscribe` 诚实声明 unsupported，不把周期性 `GroupValueRead` 伪装成推送；不做写完自动回读（多花一个事务是调用方的决定）；不支持「任意 DPT 自动推导」，几百个 DPT 编号里猜错的代价是一条合法但错误的读数，未收录的 DPT 明确报「不支持」；一轮采集耗时约等于「点位数 × 单次事务」，KNX 没有批量读的标准手段，这个代价是协议本身的 |
| IEC 61850（MMS）主站 + 内置 IED | ✅ | `internal/codec/iec61850`（`tpkt.go`/`cotp.go`/`session.go`/`presentation.go`/`acse.go`/`mms.go`/`data.go`/`address.go`）+ `internal/driver/iec61850` + `internal/sim/iec61850` + `uic iec61850read` + `examples/iec61850.json`；只做 **ACSE + MMS 初始化 + 读/写变量 + getNameList 浏览**（不做 GOOSE/SV、报告控制块、文件传输、定值组、数据集读写、TLS）；封套五段，**会话层 LI 与 BER 长度是两套编码**（`0xFF` 后跟恰好两字节大端长度，不是 BER 长形式——用通用 BER 解析器读 LI，短帧能过、长帧错位），且**封套归属不对称**（COTP CR/CC 自带 TPKT 可直接发，会话 CN/AC/DT/FIN 与 MMS 请求必须先 `WrapData` 再 `BuildCOTPData`）；**FC 是名字的一段**（`LD/LN.FC.DO[.DA]`）：`GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal` 是两个对象，点表要么写全、要么由 `labels.defaultFC` 补，驱动不从属性名反推（那需要完整 SCL 模型）；FC 白名单 ST/MX/CO/SP/CF/DC/SG/SE（`MOD` 不是标准 FC）；**类型在装置侧**，所以 `PolicyFor("iec61850")` 是 `WidthNone + AllowSymbol`、点表不写 length，`dataType` 只决定平台侧怎么存（`-raw` 打印装置回的类型）；读写请求体里 `listOfVariable` 与 `listOfData` 都编码成 `[0]`（同为 `0xA0`），只能按出现顺序区分；invokeID 严格配对、迟到响应丢弃（不校验会把 A 的值写给 B）；整条关联一把锁串行（MMS 是请求/应答协议，并发只有乱序风险）；单项数据访问错误逐条映射（10/4/5/6→`address`、3→`permission`、2/1→`protocol` 可重试）；`Browse` 能力位 unsupported（整棵模型枚举是运维动作，不是采集路径），一次性枚举走包级 `Browse` 按页收齐（continueAfter 用上一页最后一项做游标，256 页上限）；内置 IED 的 getNameList **只回项名**（剥掉域前缀，域由 objectScope 携带——整键回出去会让客户端拼成 `LD/LD.…`）；三层测试含真实 TCP 回环、分页、关联被拒、读超时后恢复、有序释放 |
| IEC 61850 的边界 | ⚠️ | 不做 GOOSE/SV（二层/三层组播，独立生命周期与采样率）、报告控制块（服务端主动推，与调度式采集语义冲突）、文件传输、定值组、数据集读写、TLS（证书与密钥分发是独立运维域）；不做 SCL 模型解析（需要完整工程文件与命名空间处理，而驱动只有点表）；`Probe` 读不到探针对象时降级为「仅关联成功」而不是判离线——对象名拼错不等于链路坏；一轮采集 = 点位数 × 单次事务，MMS 虽然有批量读但跨逻辑设备合并会让一个坏对象拖成整批失败，本实现按点位逐项、错误逐项返回；浏览结果不缓存，模型变更后需重新枚举；端口默认 102 是特权端口，内置 IED 因此用 10202 |
| CoAP 主站 + 内置服务端 | ✅ | `internal/codec/coap` + `internal/codec/textval` + `internal/driver/coap` + `internal/sim/coap` + `uic coapread` + `examples/coap.json`；只做明文 UDP 主站 + GET/PUT/POST + Block2 分块接收 + `/.well-known/core` 资源发现（不做 DTLS、Observe 订阅、Block1 分块发送、CBOR）；点位 `kind=symbol`，`path` 是资源、`selector` 取 JSON 字段、`length` 限接收字节；读取严格按响应 `Content-Format` 解码，不猜；Block2 首请求默认不带选项、由服务端决定块大小，`4.02` 时退回一次；`ackTimeout` 指数翻倍且不越过设备级 timeout，空 ACK 后只等分离响应，分离响应的 CON 回空 ACK，无关 CON 回 RST；PUT 是替换整个资源，JSON 带 selector 只发子树、数组下标写入拒绝；写成功标 good；不做响应缓存与隐藏读-改-写 |
| CoAP 的边界 | ⚠️ | 不做 DTLS、Observe 订阅、Block1 分块发送、DELETE/FETCH/iPATCH、CBOR/SenML、组播发现；`Subscribe` 诚实声明 unsupported——一次性 GET 不伪装成推送；CoAP 没有批量读，一轮采集耗时约等于「点位数 × 单次事务」，同路径多点位也不合并；JSON 写入只支持「替换整个资源或字段子树」，不做读-改-写，数组元素写入直接拒绝 |
| DL/T 645-2007 主站 + 内置从站 | ✅ | `internal/codec/dlt645` + `internal/driver/dlt645` + `internal/sim/dlt645` + `uic dlt645read` + `examples/dlt645.json`；`serial`（共享串口层总线锁）与 `tcp`（串口服务器/DTU 透传，每连接互斥）双承载；逐点读 + 后续帧重组（上限 64 帧 + 约 12.8KB 字节数）、链路故障短路、非数据块长度校验、数据块按 `bytes` 交付；写权限闸门（`allow-write` / DI 可写 / 设备密码），`allow-control` 被显式拒绝，只读 DI 本地拒绝，广播命令不提供入口；时间/日期字段做范围校验（不静默归一化）、读写字节序各自正确；适配器回显按 D7 跳过；点表 `WidthProtocol` + `DIBits` 让「宽度由 DI 表决定」与「长度必须写清楚」各自归位；三层测试含真实 TCP 回环 |
| DL/T 645 的边界 | ⚠️ | 不做 97 版、拉合闸/清零、广播校时/广播冻结、厂商扩展的主动上报、合并读（一次一 DI）、位寻址（状态字按 uint16 整体读）、DI 自动发现、表地址扫描；`MultiRead` 诚实声明 unsupported——一轮采集耗时等于「点数 × 单次事务」，300 点挂 2400bps 总线要几十秒，这个代价必须让配点表的人看见 |
| CJ/T 188-2004 主站 + 内置从站 | ✅ | `internal/codec/cjt188` + `internal/driver/cjt188` + `internal/sim/cjt188` + `uic cjt188read` + `examples/cjt188.json`；`serial`（共享串口层总线锁）与 `tcp`（串口服务器/DTU 透传，每连接互斥）双承载；单 `68`、数据域无 `0x33`、7 字节 BCD 地址；一个 DI 一组字段，按 `labels.type`（10 水/20 热/30 气/40 其他）分派 schema，同一 DI 多字段复用一次事务；单位显式字节统一到规范单位、未知代号报错；阀门（`allow-control`）与表地址（`allow-write` + 完整 14 位）两个写入口，写地址后驱动自动切换；不提供广播与缩位地址；三层测试含真实 TCP 回环 |
| CJ/T 188 的边界 | ⚠️ | 只做 2004 版（2018 版配置期显式拒绝）；只收录 901F 与 D120..D12B，价格/金额/密钥/机电同步等未收录 DI 直接报错；不做位寻址、DI 自动发现、跨 DI 合并读、表地址扫描；`MultiRead` 诚实声明 unsupported——一轮采集耗时约等于「DI 数 × 单次事务」，这个代价必须让配点表的人看见 |
| M-Bus（EN 13757）主站 + 内置从站 | ✅ | `internal/codec/mbus` + `internal/driver/mbus` + `internal/sim/mbus` + `uic mbusread` + `examples/mbus.json`；`serial`（共享串口总线锁）与 `tcp`（串口服务器/DTU 透传，每连接互斥）双承载；四种帧（`E5` / 短帧 / 长帧 / `L=3` 控制帧）、校验只覆盖 `C+A+CI+DATA`、双 `L` 与 `L+6` 整帧长；可变结构 12 字节固定头 + DIF/DIFE/VIF/VIFE 自描述记录（整数/BCD/REAL/变长 LVAR/Type G-F-I 时间）；点表 `WidthNone` + 符号地址 `q:<物理量>[@存储号][/第几条]` 与 `r:<index>`；一条 `REQ_UD2` 覆盖整表（`MultiRead` supported）、同报文内点位同快照；FCB 同轮重放 / 跨轮翻转、失败后 `SND_NKE` 复位再试；`BadFrames` 把「无字节」与「有字节不成帧」分开报；三层测试含真实 TCP 回环 |
| M-Bus 的边界 | ⚠️ | 只做**主地址**（0..250）单主站轮询：次级地址需 `SND_UD(CI=52h)` 选中、地址域变 8 字节，编解码层显式拒绝 `FDh`；不做 `SND_UD` 写入（发数据 `51h` / 改地址 `5Eh` / 校时 `50h`——写错会改变计费参数、改地址会让表从站号上消失），`Write` 声明 unsupported；不做 2 类数据（ACD）上送与多主站仲裁；固定数据结构（`73h`）能解但按老式紧凑表处理，不推荐；不做多表并行（一条总线一次只能有一个主站在说话）|
| DLMS/COSEM（IEC 62056）主站 + 内置从站 | ✅ | `internal/codec/dlms` + `internal/driver/dlms` + `internal/sim/dlms` + `uic dlmsread` + `examples/dlms.json`；`serial`（共享串口层总线锁）与 `tcp`（串口服务器/DTU 透传，每连接互斥）双承载；**LN 逻辑名引用 + Lowest Level Security + HDLC 三层承载**；HDLC 类型 3 帧按长度字段定界（`0x7E` 可出现在 info 与 FCS 里）、HCS 覆盖 `format+dest+src+ctl`、FCS 用 CRC-16/X-25、1/2/4 字节 7 位地址域；A-XDR 全标签编解码（含 `date_time` 12 字节布局与时区偏差）；SNRM→AARQ→GET→DISC 四步建链、I 帧序号维护；点表 `WidthNone` + 符号地址 `<OBIS>[:class[:attr]]`，只写 OBIS 时从内置表推导、查不到报 `ErrAddress`；scaler（attribute 3）自动应用并缓存；数据块报 `ErrUnsupported` 并提示调大 `max-pdu`；三层测试含真实 TCP 回环 |
| DLMS 的边界 | ⚠️ | 只做 LN 引用 + 无安全 + HDLC 承载：不做 SN 引用（short name 是厂商在 `object_list` 里分配的，同一 OBIS 在不同表上编号不同，必须先发现再寻址）；不做 HLS/GMAC（密钥协商、帧计数器、AES-GCM 各厂商分发方式不同，做了也只能覆盖一部分现场，还会把「密码错」与「地址错」混成同一类超时）；不做 TCP/UDP 的 COSEM wrapper（另一套 version + wrapper 头封装）；不做 Set/Action/EventNotification（写方向要处理 selective access 与分块写，半成品比不做更难排查）；不做分块读（表在应答超过 max-PDU 时必然走数据块，驱动能识别并报错但不拼块）；不做 SN 的 `get-request-with-list` 批量（需服务端声明 `multiple-references`，现场极少支持），`MultiRead` 诚实声明 unsupported——一条 Get 只读一个属性，一轮往返数 = 点位数，300 点挂 9600bps 要十几秒，这个代价必须让配点表的人看见 |
| SL 651（水文监测数据通信规约）主站 + 内置从站 | ✅ | `internal/codec/sl651` + `internal/driver/sl651` + `internal/sim/sl651` + `uic sl651read` + `examples/sl651.json`；`serial`（共享串口层总线锁）与 `tcp`（串口服务器/DTU 透传）双承载；**中心站侧查询-应答**：37H（实时）/ 38H（时段）/ 3AH（指定要素），应答沿用查询功能码；按长度字段分帧（`0x7E` 可出现在正文与 CRC 里）、整帧恒 `17+长度`、CRC-16/MODBUS 变体**不取反**且高位在前、上下行地址字段反序；附录 C 表 C.1 131 项引导符全表；要素标识符 = 引导符 + 数据定义字节（`(字节数<<3)|小数位`）；引导符类固定宽度/固定定义字节；批量要素（DRP/DRZ）用 `Guide.Slot` 声明段宽、点表 `<要素>[<段号>]` 逐段落点；DRP/DRZ/ZT 按 HEX × LSB 解、缺测全 F 降 `quality=bad`；`VT` 走 `Report.Voltage` 兜底；三层测试含真实 TCP 回环 |
| SL 651 的边界 | ⚠️ | 只做中心站侧查询（37H/38H/3AH）与上行报文解析：不做 ASCII 字符编码帧（长度字段是 4 个 ASCII 字符、数值是文本，分帧/校验/宽度全不同，遇到返回 `ErrUnsupported`）；不做 M3 多包重组（要会话状态机，只如实标出 ETB「后面还有包」）；不做配置表读写（40H~43H）、控制命令（4CH~4FH）、人工置数（35H）、图片报（36H）——写/控制类各有参数与应答格式，写错会改终端采集参数或控制闸门水泵，`Write` 声明 unsupported；不做 `FFXXH` 自定义扩展（`XX` 的含义只有设备方知道，猜出来的值不报错只污染点位）。自报帧（2F/30/31/32/33/34）codec 能解，但驱动不主动发起——中心站不发自报帧，那是终端的行为 |
| 共享串口层 | ✅ | `internal/transport/serialport`：`Acquire` 按路径引用计数复用、同路径不同参数显式报错（不静默复用）、`Ref.Lock` 总线级互斥覆盖「排空 → 发送 → 收完帧」、`Drain` 必须早于 `SetDeadline`（反序会让每次写都 i/o timeout）；`modbus-rtu` / `modbus-ascii` / `dlt645` / `cjt188` / `iec101` / `mewtocol` 共用 |
| IEC 104 的边界 | ⚠️ | 不做非平衡式（101 串口）、文件传输、驱动侧时钟同步、冗余通道切换、带时标命令、组召唤；没有「列举 IOA」这种协议动作，点表发现只能靠驱动旁路表（被动观察），设备不发就永远看不到；`Subscribe` 声明 supported 但对外仍是 `Read` 拉取契约 |
| DNP3 的边界 | ⚠️ | 不做串口、Secure Authentication、文件传输（组 70）、时间同步写路径、SBO 两段式控制、事件缓冲区重配（组 50 写）、类 0 单组召唤、主动触发非请求响应；没有「列举点索引」这种协议动作，点表发现同样只能靠旁路表（被动观察）；总召唤与事件轮询都是整类扫描，点位数的增加不会增加单次事务数，但会增加响应体积 |
| Modbus TCP 从站 | ✅ | `slaves[]` 存在即启用；四区（holding/input/coil/discrete）映射；跨映射与半值读；写穿透到 `Runtime.Write`；坏质量保留最后值并计入 `uic_slave_degraded_reads_total`；映射变化按指纹重建监听；`uic mbread` 可自检 |
| Modbus RTU/ASCII 从站 | ✅ | `serial.port` + `serial.framing`（rtu/ascii）；RTU 按功能码推导帧长、推不出来时读到 3.5 字符静默（下限 5ms 对抗 USB 转发延迟）；ASCII 以 `:` 起始、CRLF 结束；非本机单元号静默、广播只写不回、CRC/LRC 错后 `tcflush` 重同步、半帧丢弃；USB 适配器掉线按 1s→2s→…→30s 退避重开；端口与主站通道及其他从站按字面路径互斥（配置期拒绝） |
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
| 历史窗口聚合（`avg(近 5 分钟)`） | ✅ | 表达式里写 `avg/min/max/sum/count(ref, 5m)`（窗口 1s..24h，单位 `ms`/`s`/`m`/`h`）；计算点与报警规则都能用；只统计质量为 good 的**数值**样本（stale/bool/string 不计入）；空窗口求值失败 → 计算点 bad、报警 unknown（不是 0，也不是「不满足」）；配置期强制 `storage.history.enabled=true`、引用存在且为数值类型、窗口不超过 `history.maxAge` |
| 历史窗口聚合的代价与缓存 | ⚠️ | 每次求值都要扫历史段，因此 runtime 侧带短 TTL 缓存（`clamp(window/60, 1s, 30s)`，空窗口压到 1s）；历史只记「变化 + 心跳」，窗口内样本可能很少（`avg` 可能只基于 1 个样本），需要样本数时用 `count()`；窗口引用**不计入**计算点依赖图（读的是过去，不是当前值，因此不构成环） |
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
