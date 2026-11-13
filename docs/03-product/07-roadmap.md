# 路线图

> **版本**：v0.1
> **上游**：`01-prd.md` §6、`06-nfr.md`
> **节奏假设**：3–5 人全职；每波 3 个月；不追求「一次性做完」，追求「每一波都能交付」。

---

## 1. 总体节奏

```
Wave 0（2 周）   骨架与地基
Wave 1（3 个月） MVP：12 个协议条目 + 4 通道 + 4 治理域
Wave 2（3 个月） 场景纵深：11 个协议 + 历史 + 报警 + RBAC + 远程配置
Wave 3（6 个月） 规模化：集中管理 + 进程外驱动 + 多站点 + 国际化
```

**每一波的出口都是「可交付」**：能装、能跑、能演示、能通过验收门槛。不做「半成品等下一波补完」。

---

## 2. Wave 0：骨架与地基（2 周）

### 2.1 目标

把架构决策变成可编译的代码骨架，验证「四类寻址 + 三层驱动 + 时间片调度 + Outbox」的最小闭环。

### 2.2 交付物

> 第 1–13 项是「两周骨架」的原始范围；第 14 项起是骨架落地后按 P0 顺序继续
> 补齐的纵深能力。仍然列在同一张表里，是因为每一项都保持了同一种可验证的
> 验收口径：**能用一个命令或一次回环证明它成立**。

| # | 交付物 | 验收 |
|---|---|---|
| 1 | Go module `uic` + 目录结构 | `go build ./...` 通过 |
| 2 | 领域模型（Channel/Device/Point/Address/Sample/Quality） | 单元测试覆盖不变量 |
| 3 | Modbus TCP codec（纯函数） | 金报文测试通过 |
| 4 | Modbus TCP transport + client | 与内置从站回环通过 |
| 5 | Modbus TCP 从站（虚拟设备） | 第三方主站可读写 |
| 6 | 虚拟驱动（正弦/斜坡/故障） | 可注入 5 类故障 |
| 7 | 时间片调度 + 设备 actor + 通道闸门 | 100 设备无并发冲突 |
| 8 | 批读规划 | 100 点合并为 ≤ N 请求 |
| 9 | 快照缓存 + SSE | 浏览器可见实时值 |
| 10 | REST（health/devices/points/write/metrics） | curl 可验证 |
| 11 | 配置加载 + 校验 + 版本 + 回滚 | 注入非法配置自动回滚 |
| 12 | Outbox（文件 WAL + 确认位点） | 断网重放无重复 |
| 13 | 单二进制 + 交叉编译 | 四平台产物 |
| 14 | Modbus RTU 主站 + 串口 transport | pty 上半双工多从站并发无串扰 |
| 15 | MQTT sink（自写 3.1.1 客户端 + Outbox 接入） | 断 Broker 后积压、恢复后自动补投；retained 状态可读 |
| 16 | Modbus ASCII 主站 | pty 上与 RTU 并存不串帧；同串口混配两种分帧被拒绝 |
| 17 | S7（S7comm）主站 + 内置 S7 从站 | 跨 DB/I/Q/M 区批量读一次事务完成；位读写不破坏邻位；协商 240 时自动切块；写输入区返回权限错 |
| 18 | Modbus TCP 从站（点位反向暴露） | 第三方主站可读四区映射；写穿透到南向设备；未映射地址返回 0x02；`uic mbread` 可自检 |
| 19 | OPC UA 主站（None/None + 匿名）+ 内置服务端 | 三层握手跑通；`ns=2;s=` 与 `ns=2;i=` 都能采；越界判坏不截断；只读节点写入回 `BadNotWritable`；TokenId 到期前自动续订 |
| 20 | MQTT 南向（订阅取数）+ 内置 Broker | 推送型协议适配成 `Read` 拉取契约：`Read` 无网络交互；同主题多点位只订一次；`+`/`#` 通配符在配置期拒绝；SUBACK `0x80` 直接失败；断链重连自动重订；三档质量码（good/stale 保值/bad）；「一条消息都没来」不触发重连，安静超窗口且 PING 往返失败才判链；写入=发布且结果 `uncertain` |
| 21 | 计算点（零依赖表达式引擎 + `computed` 驱动） | 无物理链路、复用驱动契约：`address.kind: computed` + `expr`；同设备依赖在一次 `Read` 内按拓扑序闭合（`cost_with_tax / cost` 恒为税率），跨设备最终一致；环（含跨设备）配置期拒绝；输入不可用则不计算、质量取最差并指名输入；整数四舍五入 + 越界判坏不截断；计算点强制只读；`planner` 把计算点合成单个 `expr` span（漏组会表现为全坏且无日志） |
| 22 | 报警引擎（五态机 + 确认 + 多出口） | 有生命周期的对象而不是派生布尔：`normal/pending/active/acked/cleared` 五态；阈值（单侧/双侧 + 滞回）或布尔表达式（复用 `calc`）；`onDelay`/`offDelay` 是**连续**满足；输入质量不可信时**只更新可观测字段、不动状态机**（`pending` 取消，`active`/`acked` 保持）；`active` 与 `cleared` 都要人确认，`acked` 幂等，批量确认先整体校验再应用（混入不可确认项整体失败，`all:true` 静默跳过）；规则热更新按判定条件哈希决定保留或重置，改名字不重置，删除规则发 `removed` 清理 retained 幽灵；出口为 REST + SSE `/alarms/stream` + MQTT retained `uic/{site}/$alarm/{id}`（重连整体补发）+ `health.alarms` + 5 个指标 |
| 23 | 历史采样日志（有界分段 + 查询/聚合 API） | 不引入数据库与第三方依赖：事件驱动 + 心跳补点的采样策略（死区、最小间隔、质量变化、`disabled` 不记）；被 `minInterval` 挡下的变化进 pending，到期补写成最终值；按字节上限与保留期**整段**淘汰且永远保留一段；掉电留下的半行触发新段而不是把新记录拼进坏行；查询左闭右闭、结果按时间升序、桶起点对齐 `from + k*step`、桶质量取最差、非数值点位的 `min/max/avg` 为 `null`、桶数上限 5000；`health.history` + 6 个指标 + 诊断包 `history.json`；关掉时返回 `409 history.disabled` 而不是空结果 |
| 24 | OPC UA 订阅（服务端推送） | 通道级 `subscribe` 开关（默认关闭 = 轮询）；CreateSubscription + CreateMonitoredItems + Publish 长轮询跑通；走**独立第二条连接**，Publish 不阻塞读写；每个带序列号的通知都在下一次 Publish 中确认（不确认会让服务端重传队列填满，表现为「跑一阵就不动了」）；`QueueSize=1 + DiscardOldest` 只保留最新值；监视项建立后立刻收到一次初始样本；建订阅失败让 `Open` 失败（不静默退化成轮询）；运行期断开自动重建，期间 `Read` 回退真读；`Close` 靠关闭 socket 打断阻塞中的 Publish，不等 15 秒保活超时；内置服务端同步支持订阅/发布，`Set` 与写穿透都触发通知 |
| 25 | OPC UA 北向服务端（把点位表暴露给 SCADA） | 地址空间 `Root/Objects/{site}/{device}/{point}`，变量 NodeId `ns=2;s={site}/{device}/{point}`；只暴露 enabled 的设备与点位；质量码走 `DataValue.Status`（good→Good、uncertain/stale→Uncertain、timeout/comm_error/address_error/not_connected→对应 Bad*，Bad 不带值）；写入穿透到 `Runtime.Write`（5 秒超时），拒绝码映射回 `BadNotWritable`/`BadTypeMismatch`/`BadNodeIDUnknown`/`BadNoCommunication`；订阅推送复用服务端内核；只支持 None/None + 匿名，`policy`/`allowAnonymous` 在配置期强校验；配置按指纹热重建，端口被占用时启动失败、热更新抢不到端口回退上一份配置；`uic uaread` 可在不装第三方客户端时自检读写 |
| 26 | 多身份认证 + RBAC + 操作审计（含 `uic token`） | 三个角色九项权限按**动作**定义，`viewer ⊆ operator ⊆ admin`，未知角色/权限一律拒绝；Token 先 SHA-256 再定长比较，配置可只存 `tokenSha256`，无凭据时进入 open 模式并在启动横幅与日志里明说；凭据表放在 `atomic.Pointer` 里整体替换，`server.users` 改动 apply 后**下一条请求**即按新表判定（删掉的用户立刻 401）；401（`result=error`）与 403（`result=denied`）在审计里区分；路由级权限写死在 `routes()` 里，高危命令（`outbox.drop`/`config.rollback`/`log.level`）在 handler 内二次判定；审计为 NDJSON 分段 + SHA-256 哈希链（`seq` 连续、字段「长度:内容」编码、整段淘汰后仍可验、半行触发新段），`/api/v1/audit` 可按用户/动作/结果/时间/关键字查询，`/audit/verify` 指出第一条不一致记录；审计默认开启，写失败只计数不阻断业务；`uic token` 生成 ≥16 字符随机 Token 并输出可直接粘贴的 `server.users` 片段 |
| 27 | 历史窗口聚合（表达式里的 `avg(ref, 5m)`） | 计算点与报警规则共用同一套窗口语法：`avg/min/max/sum/count(ref, duration)`，窗口 1s..24h，单位 `ms/s/m/h`；只统计质量为 good 的数值样本（stale 与 bool/string 不计）；空窗口判失败而不是返回 0（计算点 bad、报警 unknown）；窗口引用不进 `Refs()`，因此不与当前值构成环；配置期校验历史已开启、引用存在且为数值、窗口不超过 `maxAge`；runtime 适配层带 TTL 缓存（`clamp(window/60, 1s, 30s)`，空窗口 1s，错误也缓存），让 200ms 报警兜底扫描不会打满磁盘 |
| 28 | S7 块服务（目录 / 属性 / 上传，含 `uic s7blocks`/`s7upload`） | 修正 S7 头长度：ACK/ACK_DATA 为 12 字节（多 error class/code），Job/UserData 为 10 字节；USER_DATA 组 0x03 实现 ListAll（数据区 `0A 00 0000`）、ListBlocksOfType、BlockInfo（78 字节属性，块号 5 位 ASCII）；Job 0x1D/0x1E/0x1F 实现三段式上传，分片以 `param[1]` 判最后一片、块长度取 5 位十进制 ASCII、失败也补 End 释放 PLC 会话；单块上限 16MB/65536 片；`Session` 一次性连接不进调度；内置从站同步支持，`uic s7blocks`/`s7upload` 可无第三方工具自检 |
| 29 | 设备模板库 + 实例化（含 `uic instantiate`） | 占位符 `{name}` 只替换字符串字面量、**不做算术**；整字面量是占位符时 int/float/bool 落成 JSON 数字/布尔；参数名允许 camelCase；未知字段/参数/占位符、键里的占位符、默认值类型不符一律在模板加载期报错；必填缺失一次列全；实例 `templateId` 记录来源；`examples/templates` 提供 11 份可校验模板，库测试逐份实例化并跑 `model.Device.Validate`；`uic instantiate` 只生成文件，不在运行期动态生成设备 |
| 30 | 点表 CSV 导入（含 `uic import`） | 列名大小写/空格/下划线不敏感，支持中英文别名，未知列报错；`point/address/type` 必填；行错误带行号列名并聚合（前 8 条 + 其余计数）；重复点位 ID 是错误、重复地址只是警告；`length` 列先于地址解析，字符串/字节类型能只写偏移；协议决定地址种类与宽度（Modbus 寄存器数 / S7 字节数 / 未知协议必须显式 length）；`#selector` 只允许 MQTT；合并动作为 new/unchanged/replaced/conflict，默认拒绝静默覆盖，`-dry-run` 与真写共用同一份结果 |
| 31 | 三菱 MC 3E 主站 + 内置从站（含 `uic mcread`） | 只做 3E 二进制帧 + TCP（端口 5000），ASCII/4E/UDP 形态配置期即拒；帧头 9 字节、起始设备号 3 字节小端、子命令 0x0000 字/0x0001 位；设备码表 D/W/R/ZR/SD/SW/TN/CN（字）+ M/X/Y/B/F/L/SM/SB/S（位），未收录区报错；位软元件按位打包（首点 bit0），X/Y 编号按十六进制理解（`X1F`→offset 31）；字软元件 bool 必须显式 `address.bit`（0..15），驱动做 RMW 不清零邻位；字序归一化（字内小端→大端）后按 `byteOrder` 解码，默认 CDAB；结束代码 0xC052/53/54/56→`address_error`；`internal/sim/mc` 内置从站 + `examples/mc.json` + `uic mcread` 自检；主站/从站/编解码三层测试 |
| 32 | 欧姆龙 FINS 主站 + 内置从站（含 `uic finsread`） | 只做 **FINS/TCP**（端口 9600），FINS/UDP 与 Host Link 配置期即拒；三层封装各自独立：「FINS」+ 长度（大端，仅含命令+错误码+负载）+ 命令（0 节点请求 / 1 节点响应 / 2 FINS 帧）+ 错误码；建链先做节点号协商（客户端发 0 → PLC 分配），`device.unitId > 0` 覆盖目标节点、0 用协商值；FINS 帧头 10 字节（ICF/RSV/GCT/DNA/DA1/DA2/SNA/SA1/SA2/SID），请求 ICF=0x80、响应 0xC0 并交换源/目的；读写 **MRC 同为 0x01、靠 SRC 区分**（0x01 读 / 0x02 写），参数为 `区码(1)+字地址(2 大端)+位号(1)+点数(2 大端)`；区码 CIO B0/30、WR B1/31、HR B2/32、AR B3/33、DM 82/02、EM 当前区 98（无位码，bool 走 RMW）、EM bank A0..AC/20..2C，支持 `d/w/h/a` 别名；`offset` 是**字地址**、`bit` 是字内位号；字访问 2 字节/字、位访问 **1 字节/位**；32 位低字在前（默认 CDAB）、64 位 DCBA；结束码 0x1101..1104→`address_error`、0x02xx/0x03xx→`comm_error`、其余→`bad`，TCP 错误码→`comm_error` 且可重试；每帧校验 SID；`internal/sim/fins` 位/字共用字内存表 + `examples/fins.json` + `uic finsread` 字/位读写自检；编解码/主站/从站三层测试（含真实 TCP 回环） |
| 33 | EtherNet/IP + CIP 主站 + 内置从站（含 `uic enipread`） | 只做 **CIP 显式报文 + TCP 44818 + 符号标签**：Read Tag 0x4C / Write Tag 0x4D，不做隐式 I/O（UDP 2222）、不做对象/属性寻址、不做结构体/数组自动展开（属性段 0x2C/0x30 编码有歧义，没真机不猜）；三层封装各自独立：封装头 24 字节（命令/长度只算负载/会话句柄/状态/发送方上下文/选项）→ CPF（接口句柄(4)+超时(2)+条目数(2)+条目，奇数长度补 1 字节对齐）→ CIP（服务+路径字数+路径+数据）；建链 RegisterSession(0x65) 协议版本必须 1、选项必须 0、会话句柄 0 视为失败，关闭发 UnRegisterSession(0x66)；CIP 无事务号，`conn.mu` 串行化整条会话，发送方上下文严格校验（只放行全零）；地址复用 `register`：`area`=标签名（大小写不敏感）、`offset`=数组起始元素、`length`=bytes 字节数/string 字符容量/数值固定 1、`bit`=-1 表示标签本身是 BOOL、0..63 表示从整数标签取位；**不能用 `PointRef.Length` 当元素数**（它按 16 位寄存器数推导，int32 会得 2）；CIP 数值线上小端、解码先整元素反转再走 `value.Decode(ABCD)`；同宽有符号/无符号可互换，浮点与非浮点/不同宽度判地址错，类型以**应答里的实际类型**为准；STRING 为「4 字节小端长度 + 字符容量（默认 82）」；bytes 两阶段读（先读 offset+1 个元素学宽度，再按覆盖区间重读切片），写入保留邻字节；MultiRead 走 Multiple Service Packet（路径 class 0x02/instance 0x01，450 字节预算拆批、单次读写 400 字节上限），设备回 0x08/0x14 自动关合包并退回逐条；位写 RMW 只回写目标元素（不清零邻位、不写整个数组）；CIP 状态 0x04/05/13/15/16→`address_error`、0x08/14→`bad`、0x0E/0F→`bad`、0x01/02/07/0C/10→`comm_error`、其余→`bad`，封装状态 0x64（会话失效）单独判**可重试**；`internal/sim/enip` 标签表（默认 DINT/INT/REAL/BOOL/STRING/DINT_ARRAY/自增 COUNTER）+ `examples/enip.json` + `uic enipread` 标签/数组/字符串/bytes/位读写自检；编解码/主站/从站三层测试（含真实 TCP 回环与错误路径脚本流） |
| 34 | BACnet/IP 主站 + 内置从站（含 `uic bacread`） | 只做 **ReadProperty（0x0C）/ WriteProperty（0x0F）**：不做 Who-Is/I-Am 广播发现、不做 COV 订阅、不做 ReadPropertyMultiple（0x0E）、不做分段传输；四层封装各自独立：BVLC（`0x81` + 功能 + 长度**只算 NPDU+APDU**）→ NPDU（版本 1 + 控制 `0x04`，Forwarded-NPDU `0x0B` 跳过 4 字节转发信息）→ APDU（高 4 位类型，服务码 + invoke ID）→ 应用值标签；**application 与 context 的 LVT 含义不同**（0–4 内联长度、5 单字节扩展、application 6/7 为 2/4 字节扩展、context 6/7 为 opening/closing），BOOLEAN 的 LVT 直接承载值；对象寻址 `kind=object`：`objectType`（名或编号）/`instance`（0..4194302）/`property`（名或编号）/`propIndex`（0=不带下标）；对象类型到值语义分组（模拟量 REAL、二进制 ENUMERATED、多状态 UNSIGNED、字符串 CHARACTER STRING），写 `present-value` 按**对象语义**选标签而非平台类型；`[0]对象ID [1]属性ID [2]数组下标(可选) [3]opening+应用值+closing [4]优先级(可选)`，读应答两种形态（构造包裹 / 老设备裸值）都接受；UDP 用 connected socket，8 位 invoke ID 配对、`conn.mu` 串行化整条会话、按 invoke 丢弃迟到应答、超时重发 2 次、断链不重试；错误映射 class 1→`address`、class 2 code 32/42/37/38→`address`、code 40/27→`permission`、class 3→`disconnected`（可重试）、class 4→`permission`、Reject/Abort→`protocol`；读数值类型宽松（REAL/DOUBLE/UNSIGNED/SIGNED/ENUMERATED 都收），bool/string 严格；`internal/sim/bacnet` 对象表（AI-1 温度 42.5 / AV-2 可写设定值与 16 级优先级数组 / AV-3 自增 / BI-1 active / 字符串 PUMP-01 / device-1）+ `examples/bacnet.json`（7 点位）+ `uic bacread` 属性读写自检；编解码/主站/从站三层测试（含真实 UDP 回环、超时、迟到报文、错误类映射） |
| 35 | IEC 60870-5-104 主站 + 内置从站（含 `uic iec104read`） | 只做**平衡式 TCP 主站**（端口 2404）+ STARTDT/STOPDT/TESTFR + 站总召唤 + 电能量召唤 + 单点/双点/设定值（归一化/标度化/短浮点）命令；不做 101 串口、文件传输、时钟同步写路径、冗余通道、带时标命令、组召唤；APCI 三种帧型独立解析：长度字段**只算控制域+ASDU**、U 帧功能码低两位必须 `11`、I 帧长度 4（无 ASDU）直接判错；`ParseFrame` 只解析已切好的整帧，不做流式重组；ASDU 头 6 字节（VSQ 最高位 SQ）、IOA 3 字节小端；元素宽度按类型固定，**SIQ/DIQ 品质位整体左移 3 位、QDS 低 5 位**，混用会让设备明说的 IV 显示成 good；CP56Time2a 七字节逐位掩保留位；`Read` **不做任何网络交互**，只查本地最新值表（与 MQTT 南向同一模式）；质量只来自 ASDU 品质位（IV→bad、NT→stale、SB/BL/OV→uncertain）与链路是否活着（TESTFR 往返），**刻意不做「多久没更新就降级」**——104 只在变化时上送，按时间降级会把整页刷成 stale；`Open` 只等 STARTDT 确认不等总召唤完成（快照由接收循环逐步填充）、召唤超时不拆链、电能量召唤**无条件发送**（主站无法从点表推断设备有无累计量，不支持时回否定确认按调试日志处理）、EndOfInit 触发重新召唤并 1 秒去重；点位 `kind=register`，`offset` 即 IOA，`area` 取 6 值决定命令类型（命令类型不由 dataType 推导——同一 float32 可能是短浮点也可能是放大后的标度化），`area=ioa` 的可写点按 dataType 推导（bool→单点、float→短浮点、其它→标度化），同一 IOA 可挂「遥信 + 遥控」两个点位共享缓存；双点命令只接受 1（分）/ 2（合），0 与 3 是标准里的「不允许」而非中间态，本地即拒（否则现场表现为「按钮点了没反应」）；写入成功后先填缓存并标 uncertain（避免读回确认读到上一轮旧值），且回填只补「没有」或「比命令更旧（`sentAt`）」的条目——突发上送常常紧跟激活确认甚至先于等待协程被唤醒，无条件覆盖会把这个推测值留成长期 stale，否定确认不污染缓存；否定确认分类 44/45→protocol、46/47→address、其它→permission；只读/不存在对象统一回「未知 IOA」；纯控制方向 IOA 初次读取给出明确说明而不是含糊的「还没收到数据」；keeper 用 k=12/t1=15s/t2=10s/空闲 20s 探测，TESTFR 无回应即判死；**旁路表 `Observed`** 记录设备实际送过的每个 IOA（原始类型/传送原因/未转换对象/映射点位），4096 条硬上限防异常从站打爆内存，让 `iec104read` 不依赖点表就能回答「这台设备送什么」；`internal/sim/iec104` 默认对象表（1001–1005 遥测/2001–2004 遥信含可遥控双点/3001 累计量自增/4001–4003 纯命令）+ `examples/iec104.json`（14 点位）+ `uic iec104read` 三层自检（`-probe` / 上送对象列表 / 五种命令下发）；编解码/主站/从站三层测试（含真实 TCP 回环与注入式假链路） |
| 36 | IEC 60870-5-101 主站 + 内置从站（含 `uic iec101read`） | 只做**非平衡式主站**（串口 / 串口服务器透传），应用层与 104 共用 `internal/codec/iec60870`，本项只新增 FT1.2 帧层与轮询语义；不做平衡式、FT1.1、文件传输、时钟同步写路径、带时标命令、选择-执行、分组召唤；三种帧各自独立：长度字段**只计「控制域 + 链路地址 + ASDU」**（整帧 = `L+6`）、校验和从控制域累加到 ASDU 末字节、单字节帧既无地址也无控制域（`E5` 肯定 / `A2` 否定不能混同）；链路地址宽度 0..2 字节且必须来自配置，不从报文猜；控制域按方向解释：主站侧 FCB/FCV（只有带用户数据的帧翻转），从站侧 ACD/DFC（ACD 只是提示，DFC 是「稍后再来」而非错误）；剖面 `Profile{OrgSize,CASize,IOASize}` 全部走设备标签（默认链路地址 1/1 字节、CA 1 字节、IOA 2 字节、ORG 0），非法组合与超宽地址配置期拒绝；**总召唤 / 电能量召唤 / 时钟同步 / 初始化结束四类报文没有信息对象地址**，编码解码共用 `HasIOA` 判断，并用黄金字节钉住（多写 1..3 个零字节会让对端整条丢弃）；`Read` **真实轮询总线**：每轮请求第 1 级（事件是串行总线的主要数据源）、按周期做站总召唤、无条件补一次电能量召唤（累计量不在站召唤范围内，只发站召唤的实现表现为「电度点位永远没值」）、`class2` 时再请求第 2 级；连续取数 32 条/轮上限（公平性，一台狂发事件的装置不能占满整轮）；终止只认类型 100/101 的激活终止与类型 70，**激活确认不能当结束**（否则总召唤只回来一个点），电能量召唤用它自己的类型 101 终止；总召唤**不放在 `Open`**（几百点在 9600 上要几秒，会和「连接超时」混淆），由第一次 `Read` 触发；重传**原样重发同一帧字节**（FCB 不变），重新组帧会把 FCB 翻转成一条新命令、开关被拉合两次，从站侧按「最近一次被接受的 FCB」去重并对重复帧照样回确认；复位链路是唯一的帧序号归零点，`Open` 第一件事就是复位；写入等**应用层激活确认**（`cause=7`）才成功，链路 `E5` 只说明报文到了，44..47 按 protocol/address 分类；写成功后回填缓存并标 uncertain，只补「没有」或「比命令更旧」的条目；**缓存按点位 ID 而不是 IOA**（同一 IOA 可同时是遥信与遥控，按 IOA 存会让后写入者覆盖前者，症状是「布尔遥信显示成 double 的 2」），这条修正同时改到了 104；点表层新增 `WidthNone`（远动地址不表达宽度，`length` 反而是误配），`PolicyFor("iec101")` 与 `PolicyFor("iec104")` 共用同一套对象类别白名单；半双工纪律：先 `Drain` 再设截止时间（反序会让每次写都超时），总线锁覆盖「排空 → 发送 → 收完帧」整段；错误分类：复位无应答/校验错/A2/DFC→protocol（可重试）、公共地址不符→protocol、IOA 不在点表与被拒命令→address、断链→disconnected；诊断接口 `Observed`/`BadFrames`/`Frames`/`Dropped` 让自检能区分「没在通信」「通信了但没数据」「帧通了内容不对」；`internal/sim/iec101` 默认对象表（与 104 对齐的 1001–1005 / 2001–2004 / 3001 自增 / 4001–4003 纯命令）+ `examples/iec101.json`（14 点位）+ `uic iec101read`（`-probe` / 对象清单 / 五种命令下发 / 剖面参数全覆盖）；编解码/主站/从站三层测试，含真实 TCP 回环、FCB 重传只执行一次、终止类型、ACD 边界、小地址剖面与无地址链路 |
| 37 | Mewtocol 主站 + 内置从站（含 `uic mewtocolread`） | 只做**计算机链接的 ASCII 裸帧**（串口 RS-485/RS-232 与串口服务器 TCP 透传，默认端口 2000）；不做 FP2-ET-LAN 的 16 字节封装、二进制变体、`SC`/`SD` 整块置位、扩展头 `<` 的发送、自动登录；**一帧里混着三种进制**，每一种读错都不报格式错、PLC 会回一份**别的地址**的数据（值有、质量好、对应关系全错）：站号两位**十进制**（16 号站写 `16` 而非 `10`）、数据区地址五位**十进制**（DT100 → `00100`）、接点编号是「三位十进制字 + 末位**十六进制**」（X1F → `001F`、X16.3 → `0163`）、T/C 接点四位**十进制**（T10 → `0010`）、数据内容每字节两位十六进制且**字内低字节在前**（H0063 → `6300`）；BCC 是**从帧头 `%` 起算、到最后一位正文字符止、不含 CR** 的异或，漏掉 `%` 时「大部分时候也对」、等它出错已在现场；因此点表里不让用户写 `X1F`，而是 `x:1.15`（字地址 + 位号两个十进制整数），把进制混排关在编解码层；另外四处必须按字节记住：读用**起止地址**而不是「起点 + 个数」（点数 1 时恰好等价，于是测试全过、现场最后一个点位恒坏）、写的正文**也带结束地址**（冗余，但与数据长度不自洽就回 41H，本实现由数据长度反推、两者不可能不一致）、应答回显的命令码**只有前两位**（`RCC`→`RC`、`WCS`→`WC`，按整条比会把每条接点命令判成错帧）、`RCC`/`WCC` 地址 4 位而 `RD`/`WD` 是 5 位、索引寄存器 IX/IY/ID 是「两个字符区码 + 9 个 0」（不是两段各 5 个）；容量上限要自己推而不是找手册常数：读应答 `9+4n`、写请求 `20+4n`，118 字符下读能到 27、写只能到 24，取较小者 **`MaxWords = 24`** 并分别在组帧前与拆 span 时各拦一次（只拦一处时另一端只会回一个说不清哪段太长的 41H）；接点按点读写 **`MaxContacts = 8`** 来自正文「个数」只有一位字符，拆帧绕不开、只能分批；`Read` **真实走线**且按区拆 span（不同区的命令码与编号体系都不同）、按 `[min,max]` 合并后每 24 字切一刀、逐点按 `address.bit` 取位、失败时**每个点位都出失败样本**（消失的点位会被当成「设备安静」）；三条刻意边界：**布尔量只走 `WCS` 原子单点写**（「读字→改位→写字」把一次写变成两次事务，之间别的客户端改了同字邻位就会被最后写入者一起覆盖）、**字区上的布尔点位可读不可写**（协议没有「数据寄存器第 n 位」的写命令，驱动返回 `unsupported` + 质量 `address_error` 并给出替代做法：改成 `uint16` 按整字写）、**T/C 接点不走按字读**（手册允许 `RCC` 读 T/C，但会按「每字 16 个接点」重新编号，与 T/C 自身的十进制编号是两套体系，只走 `RCS`/`RCP`；内置从站仍按真实语义实现按字读，避免「从站答应、真 PLC 不答应」）；错误码保留手册四类（链路系统 / 基本过程 / 处理 / PLC 应用）再映射到驱动分类：`60/61/66/67`→`address`（点表或型号配置问题，重试一万次也是同一结果）、`63/65`→`permission`、`40..43`/`53`→`protocol`（可重试），`23` 站号重号 / `24` 硬件错 / `26` 站号设置错**明确不可重试**（算进熔断重试只会让一台配错的设备把整条通道拖黑）；半双工纪律：帧间静默 10ms（`labels.options: "gap=20ms"` 覆盖）、**先 `Drain` 再 `SetDeadline`**（反序会把截止时间压成过去时刻：连得上但每次写都超时）、总线锁覆盖「排空 → 发送 → 收完一帧」、收帧只认 `%`/`<` 并把其余字节逐字节丢弃计数 `Dropped()`（迟到的应答被当成本轮首部的症状是「数据永远滞后一轮」，现场极难发现）；应答校验站号（重号时会把 A 的数据记到 B 头上且完全看不出异常）与回显前两位；超时文案带排查顺序（串口参数 → 接线 → 站号 → PLC 链接模式 → FP7 LAN 封装模式）；承载两种：串口出厂 9600/8/**奇**/1（改成无校验的典型症状是「能发不能收」）与透明 TCP，端口各厂不同（以太网单元 2000、网关 60001、FP Web-Server 9094、FP7 编程口 32769..32772），只发裸帧（封装模式下表现为「发出去没有任何应答」，正确处理是在 PLC 侧改回默认模式而不是给驱动加一个「猜封装」的开关）；点表层用 `WidthRegisters`（与 Modbus 同语义）+ 十四个区名白名单；新增 `AddressPolicy.BitlessAreas`（区名集合而非按协议名硬编码「T/C 例外」）让 T/C 的布尔量不带位号，写错的区名在 `uic import` 阶段就被拒而不是等到网关起来后每周期返回 `address_error`；字节序：原生「字内低字节在前 + 低字在前」两处交换叠加等价于按 16 位字整体倒序，`DecodeWords` 消掉字内交换后只剩字序 → 32 位默认 `CDAB`、64 位 `DCBA`（显式配置优先）；`internal/codec/mewtocol`（区域表与别名 `d`/`f`/`wx`/`set`/`k`、BCC、RD/WD/RCC/WCC/RCS/RCP/WCS/WCP/RS/WS/RK/WK 组帧、接点编号双向、流式收帧 `Reader`、`ParseRequest`/`BuildResponse` 主站与从站共用）+ `internal/sim/mewtocol`（站位图、两套接点编号、WCS/WCP、错误码、站号过滤：非本机站号的请求静默、广播也不应答）+ `examples/mewtocol.json`（8 点位：DT0/DT1、DT2..DT3 = 25.5 float32、X0.1/X0.5、可写 Y0.0、T10、SV5）+ `uic mewtocolread`（`-probe` 固定读 DT0 / 字区与接点读写 / `-json` / `-raw`）；编解码/主站/从站三层测试，含真实 TCP 回环、BCC 手册实例 `%01#RCSX0000`→`1D`、14 条黄金字节、T/C 双编号、WCP 多接点写、站号过滤与字区位写拒绝 |
| 38 | Host Link FINS 主站 + 内置从站（含 `uic hostlinkread`） | 只做**头码 `FA` 的 FINS 帧**（串口 RS-232/422/485 与串口服务器 TCP 透传），不做 C 模式（`RR`/`WR`/`RD`/`WD` 是另一套命令集与 BCD 寻址）、不做跨网长格式、不自动重试；选 FINS 帧而不是 C 模式，是为了与已有的 FINS/TCP **逐字节共用命令语义**——内存区码表、字/位寻址、读写语义、数值字节序完全一致，差的只是「怎么把一帧发到线上」；因此把命令语义抽进共享层 `internal/driver/finscore`（主站会话）与 `internal/sim/finsmem`（从站内存表），`codec/hostlink` 只做「FINS 帧 ↔ Host Link 信封」，同一份点表换个 `protocol`（`fins` ↔ `hostlink`）就能跑（不抽共享层的结果是两个驱动各自演化出「同一个结束码两种含义」）；信封六件必须按字节记住的事：**等待时间只有一位十六进制**（0..F，单位 10ms，写成两位会把第二位当 ICF 高位、整帧错位）、**请求与应答是两条独立布局**（应答在 `FA` 后多一格两位结束码、少一格等待时间，按请求布局解应答会让后面每个字段错两格）、**FCS 从 `@` 起异或到正文末**（不含 `*` 与 CR，漏掉 `@` 时本地回环永远通过、真机必挂）、**正文是一字节两字符的十六进制**（整帧纯可打印 ASCII）、**单元号是两位数十进制**（00..31，当十六进制会把 31 号站写成 `1F`）、**直连 ICF 是 `0x00`**（应答 `0x40`，照抄 FINS/TCP 的 `0x80` 会被当跨网帧、找不到路由）；容量上限不能照抄 FINS/TCP 的 999：FINS 命令不能在信封上分段，上限由整帧字符预算反推——读受应答帧长约束 **269 字 / 538 位**、写受命令帧长约束 **267 字 / 534 位**（读写差两个字是结构性的），超了稳定收到结束码 `0x18`，而这个错误在本地回环里永远不会出现（从站是自己写的、不校验整帧长度）；**串口出厂参数是 9600-7-E-2**，与 Mewtocol 的 9600-8-O-1 相反，7 位数据位够用是因为整帧是可打印 ASCII，「FINS 必须 8 位数据位」是错的，套用 8/N/1 的症状是「发得出去、收不到东西」且不报错；错误分两层：信封层结束码（`0x13` FCS 错 / `0x14` 格式错 / `0x15` 数据错 / `0x18` 帧长错，都是配置或组帧问题、修一次永久消失）与 FINS 层结束码（与 FINS/TCP 同一张表）；超时文案带排查顺序（单元号 → 串口参数 → 端口模式 → 波特率），因为单元号不匹配时真 PLC 完全沉默、与「线没接」现象一模一样；内置从站同样实现「单元号不匹配完全不出声」（从站对任何帧都回，主站里「单元号校验」这条路径就永远测不到）、FCS 错回 `0x13`、格式错回 `0x14`、认不出的头码回 `0x16`；不自动重试是刻意的：没有可靠事务号，重发一次写可能把同一个字写两遍，对「加计数」点位是真实数据错误，重连交给调度器；点表层 `PolicyFor("fins","hostlink")` 共用 `WidthRegisters` + `finsAreas`，`d/w/h/a` 别名归一化成 `dm/wr/hr/ar`；`internal/codec/hostlink`（`FromFINS`/`ToFINS`、FCS、收发两条布局、269/267 上限、结束码分层、流式收帧 `*`+CR 定界）+ `internal/driver/finscore`（区拆分、字/位读写、按 `Limits` 切块、结束码分类）+ `internal/sim/finsmem`（字视图 + 位视图、EM bank、`Respond`）+ `internal/driver/hostlink` + `internal/sim/hostlink` + `examples/hostlink.json`（6 点位：DM0/DM1、DM2..DM3 = 3.14 float32、CIO0.1 运行位、CIO0.5 可写故障位、DM10 可写计数器）+ `uic hostlinkread`（`-probe` / 字区与位读写 / `-limits` / `-json` / `-raw`）；编解码/主站/从站/共享层四层测试，含真实 TCP 回环、黄金字节逐字节、单元号过滤沉默、错误结束码、超时排查文案 |
| 39 | KNXnet/IP 隧道主站 + 内置网关（含 `uic knxread`） | 只做 **TUNNELING（UDP 单播隧道）+ 组地址读写**（GroupValueRead / GroupValueResponse / GroupValueWrite），不做 ROUTING 组播、不做设备管理连接（属性/内存下载）、不做 KNX Secure、不做 raw/busmon；**DPT 是必填字段、且是地址的一部分**（`1/2/3@9.001`）——组地址在总线上不携带类型，靠 `dataType` 反推得不到答案（同一个 float32 既可能是 2 字节的 9.xxx 也可能是 4 字节的 14.xxx），猜错不报错、只会把 21.5 读成 `3.7e-41`，因此把类型前移到点表地址里、由 `pointtable.PolicyFor("knx")` 用 `WidthNone + AllowSymbol + AllowDPT` 强制校验，且 `Address.Canonical()` 把 `@DPT` 拼回符号地址（否则 `1/2/3` 与 `1/2/3@9.001` 会被当成两个不同点位、重复地址检查漏判）；DPT 表用**切片而非 map**，因为 `9.001` 与 `9.1` 是同一个类型、字符串键需要补零归一化，切片上做两段匹配没有这个问题；**1 位 DPT 的值在 APCI 字节的 bit0、不在数据域**，因此 `knx.Payload` 分 `Bit` 与 `Data` 两个字段、`BuildLData` 把长度字段算成「TPDU 长度不含 TPCI 字节也不含 APCI 高 2 位」（当成整个 APDU 字节数会让网关把最后一个数据字节当成下一条报文，症状是「写成功了但值不对」），`ParseLData` 反过来必须把这一位从 APCI 里摘掉——不摘时一个「开」的 GroupValueWrite 解析成 `0x0081` 而不是 `0x0080`，调用方按服务码 switch 落空、报文被**静默丢弃**（已用 `TestParseLDataNormalizesOneBitAPCI` 钉死）；隧道序号是 **8 位、收到 TUNNELLING_ACK 之后才加一**，超时重传用**同一个序号**（换号会被当新帧、同一次写被执行两遍），序号错乱时唯一可靠的复位是**重建隧道**（`dropTunnel`）——通道号与序号是一体的，单独「调序号」在真网关上没有对应的服务；整条会话用一把 `sync.Mutex` 串行化（一条隧道只有一个通道号 + 一个 8 位序号，总线 9600 波特，并发没有收益只有乱序风险）；`Open` **只 dial 不建隧道**，CONNECT 延后到首次 `Probe`/`Read`/`Write`——否则「网关隧道槽位满」会表现成「驱动加载失败」、把一次可重试的拓扑问题说成配置错误；`Probe` 定义为「建隧道 + CONNECTIONSTATE 往返」，**不读任何组地址**（现场排障经常需要把「网关对不对」与「工程组地址配得对不对」拆开）；心跳 `CONNECTIONSTATE_REQUEST` 默认 60s（限制 1s..60s，网关侧超时通常 120s），`ackTimeout` 默认 1s 且必须 ≤ `device.timeout`（心跳窗口比设备超时还长时，一轮采集会被心跳拖长）；写路径的 `L_Data.con` 用一个 `confirmWindow=300ms` 的短窗口：**收不到 con 按成功**（部分网关不给隧道客户端回 con，硬等会把一半的写判成失败），收到且 error 位置位才判失败——而 error 位是**总线级故障**（总线忙、冲突、链路发送失败），**不是「这个组地址有没有设备在听」**：组地址是广播语义，写一个没人订阅的地址在链路层是成功的，把它判成故障会让「写了但没生效」与「网络断了」变成同一件事（内置网关因此默认 con 正常，只有 `FailNextConfirms(n)` 显式注入故障）；源地址优先级为 `labels.sourceAddress` > CONNECT_RESPONSE 的 CRD > `0.0.0`（交给网关替换），`sourceAddress` 拒绝 `0.0.0` 与非法四段；`disconnect` 在读写失败后 `c.link` 已被置空，必须持建链时的局部引用（否则最常见的「网关不回 DISCONNECT_RESPONSE」路径会变成对 nil 的 `SetDeadline` 崩掉）；`internal/codec/knx`（组地址 4/4/8 与物理地址 4/4/8、`group.go`；DPT 表与 `Accepts`/`Encode`/`Decode`、`dpt.go`；cEMI `BuildLData`/`BuildLDataCon`/`ParseLData`/`DecodeGroupValue`/`ConfirmationError`、`cemi.go`；KNXnet/IP 信封与全部服务报文、HPAI、`DefaultPort`/`HostPort`/`ParseDisconnectResponse`、`netip.go`）+ `internal/driver/knx`（`Dialer`/`New`/`NewWithDialer`、`Meta`、`parseSettings`、`buildPlans`/`planPoint`、`Probe`/`Close`/`ensure`/`redial`/`localHPAI`/`connect`/`establish`/`checkState`/`disconnect`/`dropTunnel`/`dropLink`、`writeFrame`/`readFrame`/`acceptInbound`/`answerStateRequest`/`answerServerDisconnect`、`exchange`/`sendLast`/`awaitACK`/`ackError`/`awaitValue`、`Read`/`readOne`/`Write`/`writeOne`）+ `internal/sim/knx`（`Options{MaxTunnels,DisableConfirm,DisableAutoIncrement}`、观测计数、激励 `DropNextACKs`/`FailNextSequence`/`FailNextConfirms`/`ExpireTunnels`、对象表 `SetPayload`/`Payload`/`SetValue`/`SetBool`/`SetNumber`/`Objects`、`DefaultObjects()` 单一来源）+ `examples/knx.json`（7 点位：`1/1/1`/`1/1/2` 开关、`2/1/1`/`2/1/2` 2 字节浮点、`3/1/1` 1 字节百分比、`4/1/1` 4 字节浮点每秒自增、`5/1/1` 2 字节有符号计数）+ `uic knxread`（`-addr`/`-group`/`-dpt`/`-type`/`-timeout`/`-heartbeat`/`-retries`/`-source`/`-probe`/`-write`/`-list`/`-json`/`-raw`，支持 `组地址@DPT` 内联写法）+ `uic sim -protocol knx`；编解码/主站/从站三层测试，含真实 UDP 回环、1 位 APCI 归一化回归、序号 8 位回绕后「新帧 vs 重传」边界、丢 ACK 重传、序号错乱后重建隧道、通道被网关回收、隧道槽位满判可重试、网关不回 con 时写仍成功、总线级 con 失败报 `protocol`、未知组地址读超时且写被转发但不落地 |
| 40 | IEC 61850（MMS）主站 + 内置 IED（含 `uic iec61850read`） | 只做 **ACSE 关联 + MMS 初始化 + 读/写变量 + getNameList 模型浏览**，不做 GOOSE/SV、报告控制块（BRCB/URCB）、文件传输、定值组、数据集读写、TLS——GOOSE/SV 是二层/三层组播、有独立生命周期与采样率，报告控制块是「服务端主动推」、与调度式采集是两种语义，混进采集驱动会让超时/重连/死区三套逻辑互相干扰；**封套五段且归属不对称**：TPKT（RFC1006）→ COTP → 会话层 → 表示层 → ACSE → MMS，COTP 的 CR/CC/DR **自带 TPKT 可直接发**，而会话的 CN/AC/DT/FIN 与所有 MMS 请求必须先 `WrapData`（会话 DT + 表示层）再 `BuildCOTPData`（补 TPKT + COTP 头）——少套或多套一层，对端只会断链、没有任何诊断信息，因此五段组帧全部用黄金字节钉住；**会话层 LI 与 BER 长度是两套编码**：`0x00..0xFE` 是单字节长度，`0xFF` 后跟**恰好两字节**大端长度，而 BER 长形式「后面跟几个字节」由低 7 位决定——两者在短长度上长得一模一样，用通用 BER 解析器读 LI 会「小响应一直正常、一读大结构就断链」（`TestSessionConnectUsesExtendedUserData` 钉住 `0xFF` 分支）；**FC 是名字的一段而不是元数据**：`GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal` 是两个不同对象，点表要么写全 FC、要么由 `labels.defaultFC` 补齐，驱动**绝不从数据属性名反推**（反推需要完整 SCL 模型，而驱动只有一份点表），FC 白名单严格限定 ST/MX/CO/SP/CF/DC/SG/SE（`MOD` 不是功能约束，写进引用会得到语法合法但语义错位的项名）；与 KNX 相反，**类型在装置侧**：一个对象是布尔、整型还是浮点由 IED 的 MMS 模型决定、读回来才知道，因此 `PolicyFor("iec61850")` 是 `WidthNone + AllowSymbol`、点表**不写 length**，`dataType` 只声明平台侧怎么存（把浮点标成 `int32` 不会读成整数，只会把浮点按整数入账），`-raw` 打印的是装置回的类型而不是 `-type`；**读写请求体里 `listOfVariable` 与 `listOfData` 都编码成 `[0]`（同为 `0xA0`）**，没有任何标签可区分，只能按出现顺序解析（第一个是变量名列表、第二个是数据列表）——把「按标签找」的习惯带进来会静默拿到错位的值；响应侧结果条数与请求不等时直接拒绝，不做「尽量配对」（错位配对会把 A 的错误报成 B 的错误，运维照着改就会改错点）；**invokeID 严格配对**：调用方 `c.invokeID++` 后把新值传给 Build 函数（编解码层不自己维护计数器），收到响应先校验、不一致就丢弃并继续读下一帧——不校验在「一切正常」时完全正确，只在出过一次超时后出错，迟到响应恰好被下一次请求读走，症状是「偶尔有个点位显示别的点位的值、刷新一次又好了」，这是最难查的一类缺陷（有专门的「先超时再正常」测试）；整条关联一把 `sync.Mutex` 串行化（请求/应答协议在一条 TCP 上没有并发收益、只有乱序风险）；**单项数据访问错误逐条分类**而不是整批失败：10/4/5/6→`address`（不计入熔断，重试一万次也一样）、3→`permission`、2/1→`protocol` 且可重试、其余→`protocol`——把「点位配错了」与「装置在重启」混成一类，运维只能两边都查；链路级失败（超时/断链）**中止本轮剩余点位**，地址类错误继续读下一个；错误消息里已带错误码与对象引用，因此 `driver.E` 的 cause 必须留空（非 nil 会拼出「... on X: ... on X」这种同一句话重复两遍的文案，这是端到端手测里真看到并修掉的缺陷）；**浏览能力位诚实声明 `unsupported`**（能力位描述采集路径，整棵模型枚举是运维动作），一次性枚举走包级 `Browse`（独立建链、不与采集抢锁也不搅乱 invokeID），分页靠字典序游标 `continueAfter`（MMS 没有偏移量），并处理两个边界：「说还有却回空页」直接返回、「256 页上限」防「永远说还有、游标不前进」的畸形对端；`Open` **只 dial 不做握手**（否则「装置重启中」会表现成驱动加载失败、把三十秒后自愈的状态判成坏设备），关联由首次 `Probe`/`Read`/`Write` 触发、失败可重试；`Probe` 读不到探针对象时**降级为「仅关联成功」而不是判离线**（对象名拼错不等于链路坏）；关联被拒分类为 `protocol` 且不可重试，且不计入成功关联；**内置 IED 只回项名不带宽前缀**：变量树内部键是 `simpleIO$GGIO1$ST$…`（域 + 项的扁平索引），而 MMS 里 identifier 在域内唯一、域由 `objectScope` 单独携带——整键回出去会让客户端拼成 `simpleIO/simpleIO.GGIO1.ST.Ind1.stVal`（端到端手测里真出现过这个输出）；内置 IED 还刻意复现：未知对象回 `object-non-existent` 不静默成功、写只落在已存在对象上、关联可被拒（验证「关联被拒」与「读超时」是两类事故）、`DropReads` 读超时后连接仍可用、会话 `FIN` → `RLRE` 有序释放不白等超时；`internal/codec/iec61850`（`ber.go`/`tpkt.go`/`cotp.go`/`session.go`/`presentation.go`/`acse.go`/`association.go`/`mms.go`/`data.go`/`address.go`）+ `internal/driver/iec61850`（`Dialer`/`New`/`NewWithDialer`、`Meta`、`parseSettings`、`buildPlans`/`planPoint`、`Open`、`Probe`/`Browse`/`listNames`/包级 `Browse`、`Close`、`ensure`/`redial`/`associate`/`awaitCOTPConfirm`/`awaitAssociation`/`release`、`sendRaw`/`sendSession`/`readFrame`/`roundTrip`、`Read`/`readObject`/`Write`/`writeOne`、`classifyMMS`/`dataError`）+ `internal/sim/iec61850`（`NewServer`/变量树/激励/观测、`DefaultObjects()` 单一来源）+ `examples/iec61850.json`（8 点位：LLN0 健康、两路状态、位置、相电压/电流、两个可写点）+ `uic iec61850read`（`-addr`/`-object`/`-type`/`-fc`/`-domain`/`-write`/`-probe`/`-browse`/`-list`/`-timeout`/`-json`/`-raw`）+ `uic sim -protocol iec61850`；四层测试（编解码 / 主站 / 从站 / 点表），含真实 TCP 回环、分页、关联被拒、读超时后恢复、有序释放、服务端侧构造与解析往返 |
| 41 | SNMP v3（USM 认证 + 加密）主站 + 内置代理（含 `uic snmpread -version 3`） | 复用 v1/v2c 的 OID 地址模型与批量预算，只换外层封套（`HeaderData` + `msgSecurityParameters` + ScopedPDU），v1/v2c 的 `Build`/`Parse` 明确拒绝 v3、不退回 v2c 碰运气；口令本地化（1 MiB 扩展 + `H(Ku‖engineID‖Ku)`）、HMAC 认证（MD5/SHA-1/224/256/384/512，摘要截断 = 摘要长度的一半，`subtle.ConstantTimeCompare` 比对，对「authParameters 清零后的原始字节」算摘要不重编码）、DES/AES-128/192/256 加密（CBC/CFB，DES 用 `engineBoots‖salt` 前置 IV，AES 用 `engineBoots‖engineTime‖salt` CFB 不补齐，解密按外层 SEQUENCE 长度 `trimBERSequence` 截断，`msgData` 加密时套一层 OCTET STRING）全部标准库实现、无 CGO；AES-192/256 必须配 SHA-256+（加密密钥从认证算法哈希派生）；口令 8..64 字节配置期拒绝、空用户名仅 noAuthNoPriv 允许；**发现只有一条路**：始终发空 engineID 无认证探测包（带 UserName 否则被静默丢弃）一次拿全 engineID/boots/time，固定 engineID 也照做（身份已知 ≠ 时间基准已知，跳过必卡 notInTimeWindow），engineID 写错同时报两个 engineID 并拒绝、绝不静默改用代理的；engineID 变更（代理重装）时缓存作废回空重发现、会话自愈，`notInTimeWindow` 只重学时间、`unknownEngineIDs` 才丢身份；密钥是 engineID 的函数，`setEngine` 统一打回 `keysReady`；request-id 配对在解密之后（加密应答 id 在密文里），读层只判「结构上是不是 v3」；6 类 Report 分类：wrongDigest/unknownUser/unsupportedSecLevel→permission，notInTimeWindow/unknownEngineID→自愈后重试，decryptionError→protocol；`internal/codec/snmp`（`usm.go`/`v3.go`）+ `internal/driver/snmp`（`v3session.go`）+ `internal/sim/snmp`（真做 HMAC/解密）+ `examples/snmp-v3.json` + `uic snmpread -version 3`；RFC 3414 A.3.1 派生向量逐字节通过 + 端到端发现/认证/加密/读/写/读回 + 4 条固定 engineID 回归 |
| 42 | MTConnect（HTTP + XML）主站 + 内置 Agent（含 `uic mtconnectread`） | 唯一「HTTP + XML、只读、一条请求覆盖全设备」的协议，没有事务也没有请求-应答的配对方程式，`/current` 一次 GET 就带回全设备所有数据项，因此 **MultiRead 声明 supported、点位数不影响请求数**（多一个点 vs 多一百个点成本相同），这是驱动里独一份的形态；**寻址只有 `symbol`、必须只有一个 `dataItemId`**，`pointtable.PolicyFor("mtconnect")` 用 `WidthNone + AllowSymbol`，`selector`/`index`/`length` 在导入期就拒绝（`dataItemId` 里的点号是 Agent 命名习惯不是数组下标）；**单点问题绝不升级为链路错误**是最核心的取舍：`/current` 一条请求扛全设备，若把「某点此刻 `UNAVAILABLE`」「点表里这 ID 根本不存在」当 `Read` 错误返回，上层会按错误分类丢掉整条连接，一个本来不可用的点会让整台设备每周期重连，因此 `Read` 只在「文档拿不到/解析失败/设备选不出」时返错，`UNAVAILABLE`→`bad`、ID 不存在或类型解析失败→`address_error`，**一律只写进该点**；条件是**两层结构**——外层元素是条件类型（`System`），内层 `Normal/Warning/Fault/Unavailable` 是级别，同时兼容「元素名即级别、`type` 属性承载类型」的平铺写法，归一到同一级别枚举；**条件点的值就是级别、级别即质量**：`Fault`→`bad`、`Warning`→`uncertain`、`Normal`→`good`，同一 ID 多条按 `FAULT>WARNING>UNAVAILABLE>NORMAL` 合并（不是后者覆盖）；**多设备 Agent 必须显式选设备**：`labels.deviceUuid` 优先、其次 `deviceName`，多台且未选报地址错误并列出可选设备（否则 A 机床的 `Xabs` 覆盖 B 机床的 `Xabs` 不报错、只会坐标互串）；**地址规范化与探测**：`host:port`/`http://`/`https://`、省略端口补 5000、支持反向代理基址子路径、拒绝 URL 内凭据/query/fragment；`net/http` 自动解 gzip 按魔数兜底、XML 按**本地名**匹配（带 `mt:` 前缀照样解析）、兼容 ISO-8859-1/latin-1/windows-1252 与 UTF-8 BOM、不支持的字符集显式报错；文档上限 32 MiB、`/sample` 的 `count` 上限 10000；只读是协议边界不是取舍：`Write` 声明 `unsupported` 逐项 `ErrUnsupported`，`Subscribe`/`Browse`/`Discovery` 一并 `unsupported`（能解析 `/sample` 不等于能订阅，`/probe` 是一次性诊断不是调度式浏览）；自检 `ProbeDevices` 的 version 优先 XML Header、无 `version` 才回落 `X-MTConnect-Version`；内置 Agent 刻意复现三件事：`toolOffset` 初始即 `UNAVAILABLE`（数据项存在但无值，真机最常见常态，用来验证显示坏质量而不是 0）、条件用规范两层写法渲染、`/sample` 按环形缓冲序号窗口返回（旧观测被顶掉）；故障注入七类（状态码/探针失败/畸形 XML/HTML/延迟/省略数据项/全 UNAVAILABLE/条件）各自独立、测试只开自己关心的那一个；`internal/codec/mtconnect` + `internal/driver/mtconnect` + `internal/sim/mtconnect` + `examples/mtconnect.json`（13 点）+ `uic mtconnectread`（probe/current/sample/id 三入口）；编解码覆盖本地名匹配忽略命名空间、Header 序号/`nextSequence` 补齐、Samples/Events/Conditions 三类观测、两层与平铺条件归一到同一级别并按严重度合并、`MTConnectError` 文档；驱动覆盖一条 `/current` 覆盖多点的 MultiRead、单点问题只降该点质量、条件级别即质量、多设备未选报地址错误并列可选设备、对接真实 sim 的集成测试；sim 三轴 Agent 复现 `toolOffset` 初始 UNAVAILABLE、两层条件渲染、`/sample` 序号窗口与七类故障注入各自独立 |
| 43 | BACnet MS/TP 主站 + 内置从站（含 `uic mstpread`） | 与 BACnet/IP 共用 `codec/bacnet` 的网络层与应用层，只换承载：NPDU 外再套 MS/TP 帧 `55 FF｜type｜dest｜src｜len(2,大端)｜hdrCRC｜data｜dataCRC(2,小端)`，头 CRC 用标准给出的**特殊移位展开**（有效帧残差 `0x55`）、数据 CRC 是 **CRC-16/X-25**（反射多项式 0x8408、初值 0xFFFF、结果取反，有效帧残差 `0xF0B8`），最大数据段 501；同一帧里长度大端、数据 CRC 小端是标准原文如此，也是二手实现最容易抄错的地方——头 CRC 的所有移位项必须由「异或数据后的原始值」一次算出，写成 `c ^= c<<1; c ^= c<<2` 会得到另一条 CRC，且只在单字节向量 `crcHeaderByte(0,1)=0xFE` 上暴露；**定位 sole-master 轮询从站**：本机占主站段 0..127（默认 0），被采集设备是 MAC 128..254 的从站（`labels.address`，回退 `unitId`），主站用 DataExpectingReply 点名问、从站回 DataNotExpectingReply，令牌不发生转移（从站不持有令牌，这正是单主站能成立的原因）；**刻意不做**多主站令牌环的建立与维护（Tslot 抢占、NextStation 探活、Tno_token 等毫秒级定时器在 TCP 透传/串口服务器上不可靠）、Who-Is 发现、COBS 扩展帧、波特率自适应、ReplyPostponed 后续上送（直接映射为协议错误让上层重试）、轮询主站设备，且**空闲时不监听总线**（sole-master 拓扑的明确前提，不是遗漏；现场确有第二个主站应换驱动而不是靠 `labels.nextStation` 维持令牌环）；等待应答窗口内**被动配合**：收到发给本机的 Token 就记住、事务结束后交还（优先 `labels.nextStation`，否则按 PollForMaster 从本机地址之后现场探测下一站），收到 PollForMaster/TestRequest 立即应答；每次发送前留 `labels.presilence`（默认 5ms）线路静默，给 RS-485 收发器与 `Tturnaround` 留时间（过短表现为「随机首字节 0x55 丢失→整帧当噪声→随机超时」）；**`Drain→SetDeadline→发送` 顺序固定**：`link.tcpLink.Drain` 会把 deadline 压到 `now+1ms` 且不恢复，反序会让每次写都立刻超时；分帧用**滚动缓冲逐字节重扫**，坏头只丢 1 字节而不是丢整段候选头（后者会把紧跟假 preamble 后的真帧一起吞掉，症状是随机丢一帧）；串口按物理路径共享、事务由总线锁串行、`concurrency` 必须为 1；超时/断链时剩余点一并失败（继续逐个等超时会把一轮采集拖成 N 倍时长）；`pointtable.PolicyFor("bacnet","bacnet-mstp")` 共用 `WidthProtocol + AllowObject`，对象地址允许带连字符与数字的对象类型（`analog-input:1.present-value`、`0:1.present-value`）；`internal/codec/bacnet/mstp.go` + `internal/driver/bacnetmstp` + `internal/sim/bacnetmstp` + `examples/bacnet-mstp.json`（7 点）+ `uic mstpread`（probe / 读写 / `-dtype` 覆盖）；编解码覆盖双 CRC 残差与单字节向量、端序、往返、坏帧、长度上限、地址分段；驱动覆盖真实 TCP 回环、超时重试、被动 Token 应答与交还、PollForMaster 现场探测、迟到 invoke 丢弃、垃圾字节跳过、ReplyPostponed→protocol、错误来源 MAC 忽略、错误类映射、写类型分支；从站覆盖镜像对象表、数组下标、写权限与类型收敛、每秒自增、垃圾帧重扫 |
| 44 | M-Bus（EN 13757-2/3）主站 + 内置表计（含 `uic mbusread`） | 只做**主地址 0..250 的单一主站轮询**，读方向是 `SND_NKE` 初始化 + `REQ_UD2` 请求整表（`RSP_UD`）。链路层四种帧必须都实现：`E5` 单字符 ACK（无 C 字段）、短帧 `10 C A CS 16`、长帧 `68 L L 68 C A CI DATA CS 16`、控制帧（`L=3`、无数据域）；校验和是 **`C+A+CI+DATA` 的模 256 和**、**不覆盖** L 与起止符——按整帧算会在每个长帧上固定差 3，这是这个协议被抄错最多的一处；长帧两个 `L` 必须相等、整帧长恒为 `L+6`，只信第一个 `L` 会在半双工丢字节时把半帧当整帧；`0x68` 既是长帧头也是控制帧头而 `E5` 只有 1 字节，串口服务器把 ACK 与紧随应答粘在一次 read 里返回时，扫描器必须能先把 `E5` 单独切出来，否则「链路正常」被判成「帧结构错」。**服务端决定数据布局**：CI=`72h` 是 12 字节固定头（表号 BCD 4 + 厂商 2 + 版本 + 介质 + 访问号 + 状态 + 签名 2）之后的**自描述记录串**，每条记录先用 DIF（+DIFE 链）说明长度编码、功能位（瞬时/最大/最小/故障）、存储号/费率/子单元，再用 VIF（+VIFE 链）说明物理量与 10 的幂，最后才是数据——没有「第 n 个寄存器」，偏移取决于前一条记录的长度，换一块表就整体错位，这是点表必须长出符号寻址的真实原因；VIF 表按区间 + 幂次生成而不是手抄 256 行（抄漏一行无法测出）；DIF 长度解整数 1/2/3/4/6/8 字节（不是二的幂）、BCD 2/4/6/8/12 位（最高半字节 `Fh` 是负号）、REAL 4 字节 IEEE754、变长（`0Dh`）带 LVAR（`≤BFh` 文本、`C0..EF` 编码、`EC+n*4`、`F5`=48、`F6`=64）；`2Fh` 是 16 字节对齐填充必须跳过，`0Fh`/`1Fh` 是厂商/更多记录标志；Type G 日期 2 字节、Type F/I 日期时间 4/6 字节按位域解，**时间无效位置位时报文本 `(时间无效)`**，不把 `0xFF` 解成 2159 年；Type F 的世纪号是 `1900+century*100+year`（写成 `century*100+year` 会得到 124 年、被年份校验判非法，症状是「所有时间字段都空」）。**两条核心语义**：① 一条 `REQ_UD2` 拿整表，点位多少都只发一次请求（`MultiRead` 声明 `supported`），同报文内点位天然同快照；② **同一轮重试保持 FCB 不变、跨轮才翻转**，从站据此回放上一次应答（访问号不重复累加、存储值不被重读），写错的表现是「偶尔读到上一次的值」；读取失败后补发一次 `SND_NKE` 复位从站 FCB 状态再试。`Probe` 用 `SND_NKE` 的单字节应答（不依赖任何数据结构），把「完全没字节回来」与「有字节但解析不了」分开；`Reader.BadFrames()` 只数「确实像帧但内容不对」，起始符之前的噪声不算，驱动在「有坏帧且无一好帧」时报协议错并提示查波特率/校验位（标准 2400 8E1）与终端电阻，而不是笼统超时。点表 `AddressPolicy{Width: WidthNone, AllowSymbol: true}`，`path` 是归一化物理量名 `q:energy` / `q:volume@1`（存储号）/ `q:power-max`（功能位后缀 `-max`/`-min`/`-error`）/ `q:energy/1`（同量名第 2 次出现），保留 `r:<index>` 逃生口给厂商自定义 VIF；单位与倍率由 VIF 表统一换算（`energy` 恒 Wh、`volume` 恒 m³）。**边界**：次级地址需 `SND_UD(CI=52h)` 选中且地址域变 8 字节，混合实现比不支持更难排查，编解码层**显式拒绝 `FDh`**（`ErrFrame`）；不做 `SND_UD` 写入（发数据 `51h` / 改地址 `5Eh` / 校时 `50h`——写错会改变计费参数、改地址/速率会让表从当前站号消失），`Write` 声明 `unsupported` 并逐项返回 `ErrUnsupported`；不做 2 类数据（ACD）上送与多主站仲裁；固定结构（`73h`）能解但按老式紧凑表处理、不推荐；`serial`（共享串口总线锁）与 `tcp`（串口服务器透传，每连接互斥）双承载，串口 `concurrency` 必须为 1；`internal/codec/mbus`（`mbus.go`/`io.go`/`vif.go`/`values.go`/`record.go`/`encode.go`/`response.go`）+ `internal/driver/mbus` + `internal/sim/mbus` + `examples/mbus.json`（11 点）+ `uic mbusread`（probe / 整表 / `-records` / `-raw` / `-json` / `-scan lo-hi`）；编解码覆盖金字节、四类帧、校验范围、双 `L`、截断与重同步、VIF 全表与幂次、BCD/REAL/LVAR、Type G/F/I 与无效位、`q:`/`r:` 选择、编码解码互逆；驱动覆盖真实 TCP 回环、FCB 跨轮翻转与同轮回放、失败后 NKE 复位、坏帧归因、状态字降质量、整表寻址；从站覆盖 FCB 重放不改访问号、访问号自增、地址不符静默、坏校验注入、后续报文标志、值可变 |
| 45 | DLMS/COSEM（IEC 62056）主站 + 内置电表（含 `uic dlmsread`） | 只做 **LN 逻辑名引用 + 无安全（Lowest Level Security）+ HDLC 三层承载**——这是 DLMS 能做到「离线写出点表」的唯一一条路径。**SN（short name）引用不做**：short name 是厂商在 `object_list` 里分配的，同一个 OBIS 在不同表上编号不同，要用它必须先读 `0.0.40.0.0.255` 的 attribute 2 建映射，属于另一套「先发现再寻址」的模型，与「点表直接写地址」的采集方式不兼容（`Browse` 因此诚实声明 `unsupported`）；**HLS/GMAC 不做**：需要密钥协商、帧计数器与 AES-GCM，各厂商密钥分发方式完全不同，做了也只能覆盖一部分现场，却会把「密码错」与「地址错」混成同一类建链失败；**TCP/UDP 的 COSEM wrapper 不做**（另一套 version + wrapper 头封装）；**Set/Action/EventNotification 不做**（写方向要处理 selective access 与分块写，方法调用要处理参数与返回，服务端主动上报是无编号帧——不是「GET 换个方向」，半成品比不做更难排查，`Write` 声明 `unsupported`）；**分块读不做**（表在应答超过 max-PDU 时必然走数据块，驱动能解出 `last-block`/`block-number` 并报 `ErrUnsupported` + 提示调大 `max-pdu`，但不去拼块——拼块要处理块号连续性、重传与超时重取，是一套独立状态机）。**四处 HDLC 线格式陷阱**：① 长度字段是**两标志之间的全部字节数**（含 2 字节格式字段与末尾 FCS，不含标志），整帧 = `len(body)+2`，11 位跨两字节且 bit11 是分段位——少算 2 字节会让接收方永远等不到结束标志，症状是「第一帧之后就超时」；② **HCS 覆盖 `format+dest+src+ctl`**，因此必须**先算长度再算 HCS**，先占位后回填会让头校验与格式字段不一致、接收方判定「头校验失败」并丢弃整帧；③ **类型 3 帧不做透明转义**，`0x7E` 可合法出现在 info 与 FCS 里，分帧必须按长度字段定界而不是「扫下一个标志」——按标志扫会在某些应答上把一帧切成两半且只有特定数值才触发（本仓库用 `1.0.13.7.0.255` 的 FCS `7E 64` 做回归，最初的 `ParseRaw` 正是因此修成「按长度定界 + 结束标志校验，失步时前移 1 字节」）；④ 地址域是 **7 位/字节 + 扩展位**（bit0=1 表示结束），1/2/4 字节，客户端恒 1 字节，4 字节按「高 7 位组 + 低 7 位组」各左移一位（`48 68 FE FF`）。CRC 是 **CRC-16/X-25**（初值 `0xFFFF`、反射多项式 `0x8408`、结果取反、小端），向量 `FCS(A0 07 03 21 93) = 0F 01`，好帧残余 `0xF0B8`——这个固定余数性质是分帧的基础。**应用层两处容易写反**：U 帧控制域是固定常量、**不携带可变的 P/F 位**（`UA=0x63` 的 bit4 为 0），要求往返保持 P/F 会让建链永远等不到应答（`SNRM` 金帧 `7E A0 07 03 21 93 0F 01 7E`、AARQ/AARE 逐字节向量在测试里锁死）；LLC 头 `E6 E6 00`（请求）/ `E6 E7 00`（应答）里的 `E7` 是「这一帧是应答」的唯一信号，缺了它说明收到的是服务端主动上报。**三个真实 bug 是自检/测试挖出来的**：`date_time` 解码整体错位一位（布局是 `年(2) 月 日 星期 时 分 秒 百分秒(2) 偏差(2) 状态`，**星期在日月之后、时分之前**，时区偏差是带符号分钟、`0x8000` 表示未指定）；`ApplyScaler` 逐次除 10 累积浮点误差（`5001/10/10` 得到 `50.010000000000005`，改为一次乘除 `10^|scaler|`）；`data-access-result` 编成 `01 01 xx`（标准是 `[1] IMPLICIT ENUMERATED`，定长 8 位无长度前缀，线上只有 `01 <值>` 两字节——「兼容两种形态」会让结果码为 `hardware-fault(1)` 的应答被读成「数据不完整」，于是所有硬件故障都报成超时）。**scaler 是 DLMS 最容易写错的一处**：Register 类（3/4/5）的 attribute 2 是原始整数、attribute 3 是 `structure {integer scaler, enum unit}`，工程值 = 原始值 × 10^scaler，**scaler 为负是除**（写反差 10000 倍且读数「看起来是个数」不报错）；驱动读完 attribute 2 再读一次 attribute 3 并应用、带进程内缓存（`c.scalers`，key = `class/obis`——scaler 是静态配置参数，每轮每点都多读一次会让往返数翻倍）；单位枚举只认标准表，**Wh = 29、VAh = 30、varh = 31**（sim 里三处电能点曾写成 30，数值 12345678 一切正常但量纲是视在电能，这类错误不报错只错口径）。点表 `path` 是 `<OBIS>[:<class>[:<attribute>]]`，`AddressPolicy{Width: WidthNone, AllowSymbol: true}`；只写 OBIS 时类号与属性号从内置 29 条常见表推导，**查不到就报 `ErrAddress` 而不是猜**（猜错的类号会回 `object-class-inconsistent(9)`，而「猜了一个看起来能读的类」会读到另一个对象的合法数字）；`SplitRef` 是**唯一**地址切分点（驱动与 `-obis` 都走它），因为标准正文写法 `1-0:1-8-0-255` 自带冒号，正确做法是枚举冒号位置、看前缀能否解析成 6 段 OBIS——按首个/末个冒号切都会分叉出「自检通过、采集失败」；OBIS 六段是**十进制**（`1.0.32.7.0.255` → `01 00 20 07 00 FF`，写成 `0x32` 是另一个对象）；`data-access-result` 全部归 `ErrAddress`（重试无用），坏帧归 `ErrProtocol` 并提示查波特率/校验位（物理层固定 **8N1**）与终端电阻，链路级失败记住 `linkErr` 不再逐点等超时。`MultiRead` 声明 `unsupported`：一条 `Get-Request` 只读**一个**属性，批量的 `get-request-with-list` 要求服务端声明 `multiple-references`（现场极少支持）——把「N 个点 = N 次往返」暴露给配置的人比假装合并更负责任（300 点挂 9600bps，一轮要十几秒）。`Probe` 打印**协商后**的 conformance 而非本端请求值（标准允许服务端砍掉一半能力，按请求值判断会得出错误结论）。`internal/codec/dlms`（`dlms.go`/`io.go`/`axdr.go`/`apdu.go`/`obis.go`/`objects.go`）+ `internal/driver/dlms` + `internal/sim/dlms` + `examples/dlms.json`（20 点，15 启用）+ `uic dlmsread`（`-probe` / 默认 7 点 / `-obis` / `-raw` / `-json` / `-scan` / `-list`）；编解码覆盖 SNRM 金帧、HCS/FCS 与固定余数、`0x7E` 出现在 FCS 时的定界回归、1/2/4 字节地址域、LLC 请求/应答区分、I/RR/RNR/U 控制域与序号回绕、AARQ/AARE 向量与 conformance 位值、A-XDR 全标签（空号必须报错）、`date_time`/`date`/`time` 布局、`data-access-result` 只认 2 字节形态、Get-Response 三分支、OBIS 与 `SplitRef`、`ApplyScaler` 精确性；驱动覆盖真实 TCP 回环（SNRM→AARQ→逐点 GET→DISC）、`Probe` 复用关联、拒绝关联/静默/坏校验三条故障路径、`linkErr` 缓存、object-undefined 与类号不一致归 `ErrAddress`、分块报 `ErrUnsupported`、五个 scaler 点逐位精确（`50.01`/`0.985`/`220.1`/`5.12`/`345.6`）、`Scan` 条数与内置表一致且带 scaler_unit、`Association` 采用服务端较小的 max-PDU；从站覆盖 40+ 项对象表与 `examples/dlms.json` 一一对应、未建关联读回 `read-write-denied`、两种结果码、地址/客户端地址过滤静默、三种故障开关、`SetValue`/`Value` 坏定位报错、TCP `acceptLoop`→`serveConn` 真实 socket 路径 |

### 2.3 出口标准

```bash
go build ./... && go test -race ./... && go vet ./...
./uic -config examples/demo.json &
curl localhost:8080/api/v1/points/virtual-01/sine
curl -N localhost:8080/api/v1/stream?points=virtual-01/sine
```

全部通过，且 `docs/` 与代码一致（接口签名、配置字段、指标名）。

---

## 3. Wave 1：MVP（3 个月）

### 3.1 目标

交付一个**能进现场做 POC** 的版本：12 个协议条目、4 个北向通道、4 个治理域（调度/补发/配置/可观测）。

### 3.2 范围

| 域 | 内容 |
|---|---|
| 协议 | Modbus TCP/RTU/ASCII（主+从）、S7、OPC UA（客户端+服务端）、MQTT（北向上报 + 南向订阅）、虚拟设备 |
| 模型 | 四类寻址、8+ 数据类型、4 种字序、模板与点表导入 |
| 调度 | 时间片、actor、通道闸门、写优先、退避、熔断、代次 |
| 加工 | 缩放、死区、质量传播 |
| 北向 | REST（全量）、SSE、MQTT（含 Outbox）、OPC UA（地址空间 + 订阅） |
| 治理 | 指标、三级健康、结构化日志、配置版本/回滚、远程命令白名单 |
| 安全 | 认证、TLS、凭据加密、审计（基础） |
| 交付 | 单二进制（三平台）、systemd/Windows 服务、容器镜像 |
| 测试 | 金报文、T1–T14、12 类故障注入、72h 浸泡 |

### 3.3 里程碑

| 周 | 里程碑 | 判定 |
|---|---|---|
| W1–2 | 骨架 + Modbus 闭环 | 回环读写通过 |
| W3–4 | S7 + OPC UA 客户端 | 真机/仿真通过 |
| W5–6 | 调度 + 批读 + 质量码 | 200 设备压测通过 |
| W7–8 | 北向四通道 + Outbox | 断网 24h 补发通过 |
| W9–10 | 配置版本/回滚 + 指标 + 健康 | 注入非法配置自动回滚 |
| W11 | Web 控制台（6 页） | 首次开通 ≤ 15 分钟 |
| W12 | 浸泡 + 安全 + 文档 | 验收门槛 6 条全过 |

### 3.4 出口标准（与 PRD §6.2 一致）

| # | 门槛 | 判定方式 |
|---|---|---|
| 1 | 12 个协议条目有金报文与虚拟设备 | 自动化测试 |
| 2 | 断网 24h 补发无空洞 | 脚本 + 核对 |
| 3 | 配置改错自动回滚 | 注入非法配置 |
| 4 | 1000 点 / 50 设备 72h 稳定 | 浸泡测试 |
| 5 | 安全扫描零高危 | 内网扫描 |
| 6 | 三平台产物通过 `/readyz` | CI 矩阵 |

---

## 4. Wave 2：场景纵深（3 个月）

### 4.1 目标

从「能用」到「好用」：覆盖四类典型场景，补齐运维面。

### 4.2 范围

| 类别 | 内容 | 对应场景 |
|---|---|---|
| 协议 | MC 3E（已落地）、FINS（已落地）、EtherNet/IP + CIP（已落地）、BACnet/IP（已落地）、IEC 60870-5-104（已落地）、DL/T 645（已落地）、CJ/T 188（已落地）、M-Bus（已落地）、DLMS/COSEM（已落地，LN 引用 + 无安全 + HDLC，IEC 62056）、SNMP v1/v2c（已落地）、ADS/AMS（已落地）、OPC DA 桥接（独立 Windows 桥接进程）、Sparkplug B（已落地） | 汽车 / 3C / 水务 / 楼宇 |
| 数据 | 有界采样历史库（NDJSON 分段，已落地）+ 降采样 + 保留策略 + 历史查询 API + 窗口聚合（`avg(ref, 5m)`，已落地） | 全部 |
| 语义 | 单位/量程、计算点、报警判定（阈值/延时/组合） | 全部 |
| 运维 | RBAC（3 角色）、审计查询/导出、诊断包、网络诊断、远程配置下发 | 无人值守 |
| 组态 | 设备模板库（≥ 10 个，已落地）、点表 CSV 导入（已落地）、批量实例化（`uic instantiate` 已落地） | 集成商 |
| 安全 | 证书管理、API Token 管理、审计完整性 | 政企 |
| 测试 | 负载测试（1000 设备）、驱动一致性扩展 | 全部 |

### 4.3 出口标准

| # | 门槛 |
|---|---|
| 1 | 四类场景各有一个端到端演示（真实设备或高保真仿真） |
| 2 | 1000 设备 / 10,000 点稳定 24h，内存 ≤ 300 MB |
| 3 | 1000 点工程从 CSV 到运行 ≤ 30 分钟 |
| 4 | 远程下发错误配置 30 秒内自动回滚 |
| 5 | RBAC + 审计通过越权测试 |

---

## 5. Wave 3：规模化（6 个月）

### 5.1 目标

从「单站点工具」到「多站点产品」：集中管理、生态扩展、国际化。

### 5.2 范围

| 类别 | 内容 |
|---|---|
| 协议 | IEC 61850（已落地，MMS 主站 + 内置 IED + `iec61850read` + 示例，只做 ACSE+MMS 初始化+读写变量+getNameList）、DNP3（已落地，主站 + 内置从站 + `dnp3read` + 示例）、SECS/GEM（已落地，主站 + 内置设备 + `secsread` + 示例）、KNX（已落地，隧道主站 + 内置网关 + `knxread` + 示例，只做 TUNNELING + 组地址读写 + DPT 显式声明）、CoAP（已落地，主站 + 内置服务端 + `coapread` + 示例）、Mewtocol（已落地，计算机链接主站 + 内置 FP 从站 + `mewtocolread` + 示例，串口/串口服务器双承载）、Host Link（已落地，FINS 帧主站 + 内置从站 + `hostlinkread` + 示例，命令语义与 FINS/TCP 共用）、MTConnect、IEC 60870-5-101（已落地，非平衡轮询主站 + 内置从站 + `iec101read` + 示例，应用层与 104 共享）、BACnet MS/TP（已落地，单主站轮询从站 + 内置从站 + `mstpread` + 示例，被动配合令牌环、串口/TCP 双承载） |
| 架构 | 进程外驱动（gRPC）、驱动市场（签名/版本/兼容性）、多实例协调 |
| 集中管理 | 中心聚合器（只读镜像 + 配置下发 + 批量升级 + 站点分组） |
| 灰度 | 配置分批下发、观察窗口、自动暂停 |
| 数据 | 数据契约 v2（双发过渡）、Sparkplug B 的 DataSet/模板（按客户现场需要） |
| 运维 | 远程日志拉取、离线升级包、看门狗自愈策略 |
| 国际化 | 中/英双语、时区、单位制 |
| 合规 | SBOM、签名发布、等保对照表 |

### 5.3 出口标准

| # | 门槛 |
|---|---|
| 1 | 第三方按 SDK 文档在 2 周内交付一个驱动并通过一致性套件 |
| 2 | 中心管理 50 个站点，批量下发 500 台设备配置，失败率 < 1% |
| 3 | 灰度下发可暂停、可回滚，影响面可控 |
| 4 | 进程外驱动故障不影响主进程 |
| 5 | 英文界面完整，文档双语 |

---

## 6. 资源与分工

| 角色 | 人数 | 职责 | Wave 1 投入 |
|---|---|---|---|
| 架构/核心 | 1 | 模型、调度、配置、代码评审 | 100% |
| 协议驱动 | 1–2 | 编解码、虚拟设备、金报文 | 100% |
| 平台/北向 | 1 | REST/SSE/MQTT/UA、Outbox、存储 | 100% |
| 前端/控制台 | 0.5–1 | Web 控制台（6 页） | 80% |
| 测试/运维 | 0.5 | 故障注入、浸泡、发布 | 60% |

**关键约束**：协议驱动与平台开发必须并行，且**共用同一套模型与测试夹具**；否则会在 Wave 1 末尾出现「驱动写完但接不进运行时」。

---

## 7. 依赖与关键路径

```
模型/地址设计 ──→ Codec ──→ 传输 ──→ 驱动 ──→ 一致性套件
     │                                                │
     └──→ 调度/规划 ──→ 运行时 ──→ 北向 ──→ 端到端演示
                          │
                          └──→ 配置版本/回滚 ──→ 远程运维
```

| 关键路径 | 风险 | 缓解 |
|---|---|---|
| 模型 → Codec | 地址设计反复 | Wave 0 先用两个协议验证（Modbus + 虚拟） |
| 调度 → 运行时 | 并发 bug 难复现 | `-race` + 故障注入 + 100 设备压测前置 |
| 北向 → 演示 | Outbox 语义复杂 | 先做文件 WAL，SQLite 后置 |
| 配置 → 远程 | 回滚边界复杂 | 先做本地版本，再开放远程 |

---

## 8. 风险与应对（路线图视角）

| 风险 | 触发信号 | 应对 |
|---|---|---|
| 协议开发超期 | W4 时 S7 未闭环 | 砍到「只读 + 单区」，写与符号后置 |
| 性能不达标 | 200 设备压测失败 | 降低 Wave 1 设备目标到 100，架构不变 |
| 控制台膨胀 | W8 时页面 > 10 | 冻结为 6 页，其余走 API |
| 虚拟设备被砍 | 进度压力 | 它是 M 级需求，砍它等于砍测试 |
| 许可问题 | 依赖扫描告警 | 立即替换；SBOM 进 CI |
| 单点依赖（1 人） | 关键人请假 | 文档 + 结对 + 代码评审强制 |

---

## 9. 不做什么（路线图层面的克制）

| 不做 | 时间点 | 理由 |
|---|---|---|
| 组态/HMI 绘图 | 永久 | 独立产品 |
| 自研时序数据库 | 永久 | 用 SQLite/现成引擎 |
| 桌面 GUI 主形态 | 永久 | headless 优先 |
| 云 SaaS | Wave 3 前 | 先做边缘 |
| 全协议覆盖 | 永久 | 按场景选协议 |
| 硬实时 | 永久 | 定位不同 |
| 多租户（单机） | Wave 3 前 | 集中管理才需要 |

---

## 10. 结论

1. **路线图的每一波都以「可交付」为界，而不是以「功能完成」为界。** Wave 1 结束时必须能进现场做 POC，哪怕协议只有 12 个；这比「做完 30 个协议但一个都不能验收」有价值得多。

2. **关键路径不是协议，是「模型 → 调度 → 北向」这条主干。** 协议可以后补（每个 1–4 周），主干错了要重构（3–6 个月）。因此 Wave 0 必须用两个差异最大的协议（Modbus 寄存器型 + OPC UA 符号型）来验证模型。

3. **克制清单与功能清单同等重要。** 明确「不做组态、不做时序库、不做桌面壳」，才不会被单客户的定制需求拖成项目制公司——这正是 38 个方案里大多数没能产品化的原因。
