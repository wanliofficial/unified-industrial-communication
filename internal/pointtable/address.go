package pointtable

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"uic/internal/model"
)

// 点表里的地址是一行文本，而平台内部是结构化 model.Address。本文件只做
// 这一件事：把运维写得出来的地址表达式，解析成带完整校验的地址。
//
// 语法与 model.Address.Canonical 对齐（同一份地址在导入、日志、北向节点名
// 里应当是同一个字符串）：
//
//	register  holding:100[.<bit>][:<length>]
//	          db12:0[.<bit>][:<length>]        （S7：db 号直接跟在 db 后）
//	symbol    ns=2;s=Plant.Temp[#data.temp]
//	object    analogInput:1.presentValue[<index>]
//	file      file:1/0:0
//	computed  expr:line-a:power / line-a:rate
//
// length 省略时按协议推导：Modbus 是寄存器个数，S7 是字节数。字符串/字节
// 类型没有可推导的宽度，必须显式写——猜一个宽度只会让读回来的值错位。

// WidthMode 决定「没写 length 时怎么推导寄存器宽度」。
type WidthMode int

const (
	// WidthExplicit 表示必须显式给 length。未知协议走这条，宁可让人写清楚。
	WidthExplicit WidthMode = iota
	// WidthRegisters 是 Modbus 语义：一个 16 位寄存器算 1。
	WidthRegisters
	// WidthBytes 是 S7 语义：DB1.DBD0 的 length 是 4。
	WidthBytes
	// WidthNone 表示地址里**不表达宽度**：值有多宽由协议元素类型决定
	// （远动协议的 ASDU 类型标识一个对象就是固定几字节），写 length 反而
	// 是误配。它与 WidthProtocol 的区别是「点表这一层是否还需要宽度」：
	// WidthProtocol 仍要求驱动确认宽度，WidthNone 则连问都不问。
	WidthNone
	// WidthProtocol 表示宽度由协议层按数据项定义决定，地址里可以省略。
	//
	// 与 WidthExplicit 的区别不是「谁更宽松」，而是**长度的归属**：Modbus/S7
	// 的地址只表达位置，宽度是调用方的选择（同一个 holding:0 可以读成 2 个
	// 寄存器也可以读成 4 个）；而按数据标识寻址的协议里，宽度是数据项的
	// 固有属性，多写一个字节就取不到这个数据项。所以这里允许省略，由驱动
	// 按协议定义填——驱动也会在应答长度不符时报错，那是这一层做不到的事。
	WidthProtocol
)

// AddressPolicy 描述某协议接受的地址形态。
type AddressPolicy struct {
	Width WidthMode
	// RegisterAreas 为空表示不限制（未知协议）。
	RegisterAreas map[string]bool
	// BitlessAreas 是「布尔量不带位号」的区名集合。
	//
	// 绝大多数寄存器协议里，一个 bool 点位必须指明它是哪个字的第几位；
	// 但 Mewtocol 的定时器/计数器接点（T/C）按定时器号/计数器号编号，
	// 一个号就是一个接点，没有「字内第 n 位」。把它放进白名单而不是在
	// parseRegister 里按协议名硬编码，是为了让这条规则跟着地址策略走。
	BitlessAreas map[string]bool
	// AllowRegister/Symbol/Object/File/Computed 是地址种类白名单。
	AllowRegister bool
	AllowSymbol   bool
	AllowObject   bool
	AllowFile     bool
	AllowComputed bool
	// AllowSelector 表示符号地址可以带 `#selector`（MQTT 载荷字段）。
	// 其他协议带 selector 多半是把 MQTT 主题贴错了列，应当在导入时拦下。
	AllowSelector bool
	// DIBits 表示寄存器地址的第一段是「数据标识」而不是区域名：
	// 4（CJ/T 188）或 8（DL/T 645）位十六进制；0 表示不是 DI 型寻址。
	//
	// 单独一个字段而不是把它塞进 RegisterAreas：DI 是无限集合，
	// 没法穷举成白名单。位数是协议事实，不做成「几个都行」——多一位少
	// 一位都指向另一个数据项，宽松解析只会让人查很久。
	DIBits int
	// AllowSymbolLength 表示符号地址可以带长度：`NAME:81` 或点表的
	// length 列。它只对「值的宽度必须由点表给出」的符号协议有意义
	// （ADS 的 STRING/bytes 变量），寄存器型协议的 length 另有语义。
	AllowSymbolLength bool
	// SymbolIndexInPath 表示数组下标属于符号名本身（`MAIN.arr[2]`），
	// 而不是拆成 Address.Index。ADS 的变量名就是带下标的完整路径；
	// 拆开再拼回去会让「[0] 与未配置」变得无法区分。
	SymbolIndexInPath bool

	// DIField 表示 DI 后面允许写「字段偏移」而不是只允许 0。
	//
	// DL/T 645 的一个 DI 就是一个值，offset 只能为 0；CJ/T 188 的一个 DI
	// 是一组字段的容器（901F 里同时有累积流量、结算日流量、时间、状态），
	// 只能按字段定位。两种语义不能共用一个开关。
	DIField bool

	// AllowDPT 表示符号地址后面必须带数据点类型：`1/2/3@9.001`。
	//
	// 只有 KNX 走这条：组地址只回答「哪个对象」，不回答「值有多宽」，
	// 类型信息不在总线上，只在工程的组地址表里。因此点表的 DPT 列不是
	// 可选的元数据，而是地址的一部分——缺了它，驱动连该读几个字节都
	// 不知道。
	AllowDPT bool
}

var (
	modbusAreas = map[string]bool{"holding": true, "input": true, "coil": true, "discrete": true}
	s7Areas     = map[string]bool{
		"db": true, "input": true, "output": true, "merker": true,
		"counter": true, "timer": true,
	}
	// iecAreas 是远动协议的「对象类别」：监视方向的 ioa，加上五种命令
	// 类别。它们长得像寄存器区域，实际是应用层的对象类别——放在
	// address.area 而不是另造字段，是为了让点表、北向、日志共用一种形状。
	iecAreas = map[string]bool{
		"ioa": true, "single-command": true, "double-command": true,
		"setpoint-float": true, "setpoint-scaled": true, "setpoint-normalized": true,
	}
	// mewAreas 是 Mewtocol 的区名：字区（dt/ld/fl）、接点区（x/y/r/l/t/c）、
	// 索引寄存器（ix/iy/id）、定时器/计数器值区（sv/ev）。
	//
	// 与 modbusAreas 一样是「固定的、可穷举的」集合，放进白名单而不是
	// 放行任意区名：写错的区名（例如把 dt 写成 d）应当导入期就被拒，
	// 而不是等到启动后每个周期都返回 address_error。
	mewAreas = map[string]bool{
		"x": true, "y": true, "r": true, "l": true, "t": true, "c": true,
		"dt": true, "ld": true, "fl": true,
		"ix": true, "iy": true, "id": true,
		"sv": true, "ev": true,
	}
	// mewBitless 是 mewtocol 里「布尔量不带位号」的区：T/C 接点按定时器/
	// 计数器编号，一个号就是一个接点。
	mewBitless = map[string]bool{"t": true, "c": true}
	// finsAreas 是 FINS 内存区的区名集合：CIO/WR/HR/AR 四个继电器区，
	// 数据存储器 DM，加上「当前 EM 区」（em）与 13 个 bank（em0..em12）。
	//
	// 与 modbusAreas 同样处理成可穷举的白名单：区名写错（例如把 dm 写成
	// dmm、把 em5 写成 em15）应当在点表导入期就报出来，而不是等到运行时
	// 每个周期都返回 address_error。别名 D/W/H/A 由 splitRegisterPrefix
	// 归一化成小写全名，所以这里只列规范名。
	finsAreas = func() map[string]bool {
		m := map[string]bool{
			"cio": true, "wr": true, "hr": true, "ar": true, "dm": true, "em": true,
		}
		for i := 0; i <= 12; i++ {
			m[fmt.Sprintf("em%d", i)] = true
		}
		return m
	}()
	// finsAliases 是现场口语里的单字母区名。它们只在 splitRegisterPrefix
	// 里归一化，不进入 finsAreas：白名单只放规范名，别名一旦归一化就
	// 不再是别名，写错的 `dmm` 也就不会因为「像 dm」而蒙混过去。
	finsAliases = map[string]string{
		"d": "dm", "w": "wr", "h": "hr", "a": "ar",
	}
)

// PolicyFor 返回协议对应的地址策略。
//
// 这里刻意把「协议接受什么地址」写成显式表，而不是问驱动：点表导入发生在
// 驱动装配之前（CI 里甚至没有设备），一个静态错误应当在导入时就报，
// 而不是等到现场启动后每个周期都返回 address_error。
func PolicyFor(protocol string) AddressPolicy {
	switch protocol {
	case "modbus-tcp", "modbus-rtu", "modbus-ascii":
		return AddressPolicy{
			Width: WidthRegisters, RegisterAreas: modbusAreas, AllowRegister: true,
		}
	case "s7":
		return AddressPolicy{
			Width: WidthBytes, RegisterAreas: s7Areas, AllowRegister: true,
		}
	case "opcua":
		return AddressPolicy{AllowSymbol: true}
	case "ads":
		// ADS 的地址就是 PLC 符号名：`MAIN.nCounter`、`MAIN.arr[2]`、
		// 结构体逐级写全。下标属于名字本身（见 SymbolIndexInPath）；
		// STRING/bytes 的宽度必须由点表给出，所以允许 `NAME:81`
		// 或 length 列——PLC 变量的宽度是符号的属性，猜不出来。
		return AddressPolicy{
			AllowSymbol: true, AllowSymbolLength: true, SymbolIndexInPath: true,
		}
	case "secs":
		// SECS 的寻址是「对象类型 + 编号 + 属性」：`svid:1001.value`、
		// `ecid:200.value`。编号是协议原生的 32 位整数（不是寄存器偏移），
		// 所以用对象地址承载；没有区域、偏移或数组下标。
		//
		// 宽度不可推导：同一个编号在不同设备上可能是 U4、A 或 F8，
		// 由点表声明 dataType 决定如何解释。因此 Width 用 WidthProtocol，
		// 点表层不复制 SECS-II 的格式表。
		return AddressPolicy{Width: WidthProtocol, AllowObject: true}
	case "snmp":
		// SNMP 的地址就是一个点分十进制 OID。没有区域、没有偏移、没有
		// 数组下标（实例号写在 OID 里），也没有 MQTT 那种载荷字段选择器。
		//
		// 不在这一层校验 OID 的数值约束（首弧 0..2 等）：那是协议事实，
		// 由驱动/AST 校验能给出更准确的信息；点表层只负责拒绝把它写成
		// 寄存器或对象地址。
		return AddressPolicy{AllowSymbol: true}
	case "mqtt":
		return AddressPolicy{AllowSymbol: true, AllowSelector: true}
	case "computed":
		return AddressPolicy{AllowComputed: true}
	case "virtual":
		return AddressPolicy{
			Width: WidthBytes, AllowRegister: true, AllowSymbol: true,
			AllowComputed: true, AllowSelector: true,
		}
	case "iec101", "iec104":
		// 远动协议用「对象类别 + 信息对象地址」寻址，值有多宽由 ASDU 的
		// 类型标识决定，地址里写 length 只会让驱动拒绝这条点位。因此
		// Width 用 WidthNone：点表层不推导、也不要求宽度。
		//
		// 两个承载共用同一套地址形态：104 多出来的只是 TCP 与序号层，
		// 点表没有任何理由不同。
		return AddressPolicy{Width: WidthNone, AllowRegister: true, RegisterAreas: iecAreas}
	case "mewtocol":
		// 寄存器型寻址：`区名:字地址[.位号][:长度]`。
		//
		// 一个地址 = 一个 16 位字，因此 Width 用 WidthRegisters（与 Modbus
		// 同语义）：16 位类型长度 1，32 位类型长度 2。布尔量分两种——
		// 接点区（x/y/r/l）写 `x:1.15` 指「第 1 字第 15 位」；T/C 是
		// 「一个号一个接点」，写 `t:10` 不带位号（见 BitlessAreas）。
		//
		// 字区（dt/ld/fl）上的布尔量也是 `dt:0.3`：读可以（字内取位），
		// 写会被驱动拒绝——协议没有数据寄存器某一位的原子写命令。
		return AddressPolicy{
			Width: WidthRegisters, RegisterAreas: mewAreas,
			BitlessAreas: mewBitless, AllowRegister: true,
		}
	case "fins", "hostlink":
		// 寄存器型寻址，与 Mewtocol 同形：`区名:字地址[.位号][:长度]`。
		//
		// Host Link（头码 FA）与 FINS/TCP 在应用层是同一套命令：内存区、
		// 字地址、位号、读写语义完全一致，差别只在信封（ASCII 十六进制 +
		// FCS）与承载。因此点表没有理由分成两种写法——同一份点表换个
		// protocol 就能跑，是本项目把两者放在一起的核心理由。
		//
		// 宽度仍按寄存器推导（uint16 = 1，float32 = 2）。两者的单次上限
		// 不同（Host Link 读 269 字、写 267 字；FINS/TCP 读 480 字），
		// 那是驱动按信封算出来的，点表层不复制这些数字：同一个点表在
		// Host Link 上会被驱动切成更多次事务，而不是被拒绝。
		return AddressPolicy{
			Width: WidthRegisters, RegisterAreas: finsAreas, AllowRegister: true,
		}
	case "dlt645":
		// 数据标识型寻址：`<DI>:0[:length]`，DI 是手册顺序的 8 位十六进制。
		// offset 恒为 0（协议没有偏移概念），长度由 DI 表推导；只有数据块
		// （DI 含 FF）必须显式给长度，因为它的宽度由请求决定。
		//
		// Width 用 WidthProtocol：点表这一层不复制 DI 表，也不 import
		// 协议 codec；宽度由驱动按数据项定义决定（那里报错能说出数据项
		// 名字，比「长度不对」有用得多）。数据块仍然必须显式给长度，
		// 因为它的宽度本来就由请求决定。
		return AddressPolicy{Width: WidthProtocol, AllowRegister: true, DIBits: 8}
	case "cjt188":
		// 数据标识型寻址，但 DI 只有 2 字节（手册顺序的 4 位十六进制），
		// 且一个 DI 承载多个字段：`<DI>:<fieldOffset>[:length]`。
		//
		// 字段偏移是「DI 数据域内的字节偏移」，不是寄存器偏移。让它出现在
		// offset 位置而不是另造语法，是为了让点表、规划层、北向的地址展示
		// 保持同一种形状；字段含义由驱动按 (DI, 表类型) 的 schema 解析。
		return AddressPolicy{Width: WidthProtocol, AllowRegister: true, DIBits: 4, DIField: true}
	case "knx":
		// 组地址型寻址：`<组地址>@<DPT>`，例如 `1/2/3@9.001`。
		//
		// 为什么必须是符号地址而不是寄存器地址：KNX 的寻址单位不是「某个
		// 区里的第 n 个字」，而是一个 16 位组地址（主组/中组/子组各几位），
		// 组地址之间没有算术关系——`1/2/3` 与 `1/2/4` 不一定是相邻对象。
		// 用 offset 表达会诱导人写出 `1/2/3` + 长度 2 这种不存在的概念。
		//
		// 为什么必须带 DPT：组地址不携带类型。同一个 `1/2/3` 在 A 工程里
		// 是 1 位开关、在 B 工程里是 2 字节浮点温度，由 dataType 反推会
		// 得到「f32 到底是 9.xxx 还是 14.xxx」这种无法回答的问题——猜错
		// 不报错，只会把温度读成 3.7e-41。DPT 因此做成必填。
		//
		// Width 用 WidthNone：宽度由 DPT 决定（1 位在 APCI 字节里，2 字节
		// 浮点是 2 字节），点表层不该再要求一个 length。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true, AllowDPT: true}
	case "iec61850":
		// 对象引用型寻址：`LD/LN.FC.DO[.DA...]`，例如
		// `simpleIO/GGIO1.ST.Ind1.stVal`。
		//
		// 为什么是符号地址而不是对象地址：IEC 61850 的 MMS 层把一个变量
		// 寻址成「域 = 逻辑设备，项 = LN$FC$DO$DA」，逻辑设备与逻辑节点之间
		// 没有数字偏移关系——`simpleIO/GGIO1` 与 `simpleIO/GGIO2` 不是相邻
		// 对象。用区域:偏移表达会诱导人写出不存在的位置算术。
		//
		// 为什么 FC 必须出现（或提供 labels.defaultFC）：MMS 的项名里 FC 是
		// 名字的一段而不是元数据，缺了它 `GGIO1$Ind1$stVal` 与
		// `GGIO1$ST$Ind1$stVal` 是两个不同对象，只有一个存在。默认值由设备标签
		// 给出，不从数据属性名反推——反推需要完整 SCL 模型，而驱动手上只有
		// 一份点表。
		//
		// Width 用 WidthNone：值有多宽由数据对象的 MMS 类型决定（布尔、整型、
		// 浮点、结构体的某个叶子），点表里写 length 没有意义。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true}
	case "mbus":
		// 记录名型寻址：`q:<物理量>[@存储号][/第几条]` 或 `r:<记录序号>`。
		//
		// 为什么是符号地址而不是寄存器偏移：M-Bus 的可变数据结构是**自描述
		// 记录串**——每条记录先用 DIF/VIF 说明自己多长、是什么量，数据才跟在
		// 后面。没有"第 n 个寄存器"这种东西：偏移取决于前一条记录的长度，
		// 换一块表（甚至同一块表换了固件）就会整体错位。用 offset 表达会
		// 诱导人写出「寄存器 0 长 4 字节」这种在协议里不存在的概念，而读错
		// 位不会报错，只会把热量读成流量。
		//
		// Width 用 WidthNone：值有多宽由报文的 DIF 决定，点表里写 length
		// 只会与表实际声明的宽度冲突。selector/index 同样无意义——`@` 与
		// `/` 已经在 path 里表达了"第几条"，拆成符号下标反而表达不了
		// 「同一物理量的第二次出现」这种协议事实。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true}
	case "mtconnect":
		// 数据项 ID 寻址：`/probe` 给出的 `dataItemId`（如 `Xabs`、`avail`），
		// 由 `/current` 的 XML 文档按 id 查值。
		//
		// 为什么是纯符号地址：MTConnect 的点位是 Agent 侧的数据项定义，没有
		// 「基址 + 偏移」的线性结构——`Xabs` 与 `Yabs` 是两个独立 id，不是相邻
		// 位置。用区域:偏移表达会诱导人去猜不存在的地址算术。
		//
		// Width 用 WidthNone：值有多宽由数据项的文本与点表 dataType 决定，点表里
		// 写 length 没有意义。selector / index / length 也一律无意义：一条
		// `/current` 已经覆盖全设备，不需要按字段下钻，也不需要下标。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true}
	case "bacnet", "bacnet-mstp":
		// 对象属性寻址：`analog-input:1.present-value`、`analog-value:2.priority-array[3]`。
		//
		// BACnet 的两层地址（设备地址 + 对象属性）里，设备地址属于设备/链路
		// 配置（IP 与端口，或 MS/TP 的 MAC），点表只表达对象这一层。这与
		// Modbus 的「单元号在设备上、寄存器在点表上」是同一种分工。
		//
		// Width 用 WidthProtocol：值有多宽由对象的属性类型决定（REAL、
		// ENUMERATED、CHARACTER STRING），点表里写 length 只会诱导出
		// 「读 4 个字节的 present-value」这种说法。BACnet/IP 与 MS/TP 共用
		// 同一条策略，因为两者只差承载层，对象寻址完全一致。
		return AddressPolicy{Width: WidthProtocol, AllowObject: true}
	case "dlms":
		// 对象属性寻址：OBIS 逻辑名 + 类号 + 属性号。
		//
		// 为什么用符号地址而不是 object/register：DLMS 的三层寻址里没有任何
		// 一层是「线性偏移」。OBIS 是六个独立索引（介质/通道/物理量/费率/
		// 存储号），既不是进制位也不能相加；类号决定这个 OBIS 落在哪个
		// COSEM 对象上；属性号决定读对象的哪一项（value / scaler_unit /
		// logical_name）。用 register:offset:length 表达会诱导人写
		// 「OBIS 加 1 就是下一个费率」这种在协议里不存在的东西，而读错
		// 地址不会报错，只会把费率 2 的电量记到费率 1 上。
		//
		// 语法：`<OBIS>` 或 `<OBIS>:<class>:<attribute>`。
		//   `1.0.1.8.0.255`          类号与属性号从内置常见 OBIS 表推导
		//   `1.0.1.8.1.255:3:2`      显式指定 class 3、attribute 2
		//   `0-0-42-0-0-255:1:2`     标准连字符写法同样接受
		//
		// 为什么允许只写 OBIS：现场点表 90% 是标准对象，由驱动补全类号能
		// 少写两列；但厂商私有对象（不在表里）**必须**显式给类号，否则
		// 驱动报地址错误而不是猜一个类去读——猜错的表现是
		// object-class-inconsistent(9)，比直接说"这个 OBIS 我不认识"难查。
		//
		// Width 用 WidthNone：值有多宽由 Get-Response 里的 A-XDR 标签决定
		// （同一个 OBIS 可能返回 long-unsigned、double-long-unsigned，甚至
		// structure），点表写 length 只会与应答冲突。selector / index /
		// DPT 一律无意义：DLMS 没有「载荷字段」也没有「数据点类型」。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true}
	case "sl651":
		// 要素标识符寻址：附录 C 表 C.1 的引导符，写成 ASCII 简称（`Z`、
		// `PT`、`VT`）或两位十六进制（`39`、`26`、`38`）。
		//
		// 为什么不接受 register:offset：SL 651 的报文是**要素信息组**，
		// 每个要素自带「引导符 + 数据定义字节」说明自己是什么、多长，
		// 没有「第 n 个字节」这种线性结构。数据定义字节由要素自身携带，
		// 点表里写 length 只会与报文声明的宽度冲突；读错位不会报错，
		// 只会把降水量记到水位上。
		//
		// 为什么允许符号地址：引导符是规约定义的**常量**（39H 永远是
		// 瞬时河道水位），所以这里与 M-Bus/DLMS 同类——地址是名字，不是
		// 算术。规律性强的点位（多站同构点表）由 labels.address 表达站号。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true}
	case "toledo":
		// 读数字段寻址：weight / status / mode / unit / stable / motion /
		// decimals。每个点位是「当前一帧读数」的一个字段，不是某个寄存器。
		//
		// 为什么不接受 register:offset：连续输出帧是 `状态,毛净,数值` 三段式，
		// 没有「第 n 个寄存器」这种线性结构；数值段宽度由仪表的小数位数决定
		// （D39 的 WEIGHT(7) 是 5 位整数 + '.' + 1 位小数），点表里写 length 只会
		// 与帧里的实际宽度冲突。
		//
		// 为什么允许符号地址：字段名是衡器读数的**语义标签**（weight 永远是重量
		// 值），与 SL 651 的要素标识符同类——地址是名字，不是算术。字段名的
		// 合法性（weight/status/…）由驱动在 Open 时校验，这里只放开种类。
		return AddressPolicy{Width: WidthNone, AllowSymbol: true}
	}
	// 未知协议：不拦地址种类（驱动可能在外部注册），但 length 必须显式，
	// 避免用一个猜出来的宽度去读真实设备。
	return AddressPolicy{
		Width:         WidthExplicit,
		AllowRegister: true, AllowSymbol: true, AllowObject: true,
		AllowFile: true, AllowComputed: true, AllowSelector: true,
	}
}

// ParseAddress 把一行地址表达式解析为结构化地址。
func ParseAddress(expr string, dt model.DataType, protocol string) (model.Address, error) {
	return ParseAddressWithLength(expr, dt, protocol, 0)
}

// ParseAddressWithLength 与 ParseAddress 相同，但当地址本身省略长度时，
// 允许用点表的 length 列提供。字符串/字节类型没有可推导的宽度，CSV 里
// 常见的写法是地址只写到偏移、长度单独一列。
func ParseAddressWithLength(expr string, dt model.DataType, protocol string, length int) (model.Address, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return model.Address{}, fmt.Errorf("address is required")
	}
	if strings.ContainsAny(expr, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d") {
		return model.Address{}, fmt.Errorf("address %q contains control characters", expr)
	}
	pol := PolicyFor(protocol)

	switch {
	case strings.HasPrefix(expr, "expr:"):
		if !pol.AllowComputed {
			return model.Address{}, fmt.Errorf("protocol %q does not accept computed addresses", protocol)
		}
		e := strings.TrimSpace(strings.TrimPrefix(expr, "expr:"))
		if e == "" {
			return model.Address{}, fmt.Errorf("expr: expression is empty")
		}
		if length > 0 {
			return model.Address{}, fmt.Errorf("length is only valid for register addresses")
		}
		return model.Address{Kind: model.AddressComputed, Expr: e}, nil

	case strings.HasPrefix(expr, "file:"):
		if !pol.AllowFile {
			return model.Address{}, fmt.Errorf("protocol %q does not accept file addresses", protocol)
		}
		if length > 0 {
			return model.Address{}, fmt.Errorf("length is only valid for register addresses")
		}
		return parseFileAddress(expr)
	}

	// DI 型寻址的语法与 register 同形（`DI:offset:length`），因此必须先
	// 拦在前面：否则 `db1:0` 之类会落进 parseRegister 被当成 S7 数据块。
	if pol.DIBits > 0 {
		return parseDIAddress(expr, dt, pol, length)
	}

	if area, db, rest, ok := splitRegisterPrefix(expr); ok {
		if !pol.AllowRegister {
			return model.Address{}, fmt.Errorf("protocol %q does not accept register addresses", protocol)
		}
		if pol.RegisterAreas != nil && !pol.RegisterAreas[area] {
			return model.Address{}, fmt.Errorf("area %q is not valid for protocol %q (allowed: %s)",
				area, protocol, joinKeys(pol.RegisterAreas))
		}
		return parseRegister(area, db, rest, dt, pol, length)
	}

	if a, ok, err := parseObjectAddress(expr); ok {
		if err != nil {
			return model.Address{}, err
		}
		if !pol.AllowObject {
			return model.Address{}, fmt.Errorf("protocol %q does not accept object addresses", protocol)
		}
		if length > 0 {
			return model.Address{}, fmt.Errorf("length is only valid for register addresses")
		}
		return a, nil
	}

	if !pol.AllowSymbol {
		return model.Address{}, fmt.Errorf("protocol %q does not accept symbol addresses (got %q)", protocol, expr)
	}
	// 数据点类型：`1/2/3@9.001`。必须在符号解析之前切掉，否则 `@9.001`
	// 会变成路径的一部分——组地址表里不会有这个字符串，驱动查不到。
	dpt := ""
	if pol.AllowDPT {
		if strings.ContainsRune(expr, '#') {
			// 先把 selector 拦下来：`1/2/3@9.001#temp` 会被当成「DPT 是
			// 9.001#temp」，报出「不是数据点类型」——那是一条把人引向
			// DPT 写法的错误，而真正的问题是协议没有载荷字段这个概念。
			return model.Address{}, fmt.Errorf(
				"protocol %q does not accept selector addresses (got %q)", protocol, expr)
		}
		body, typ, err := splitDPT(expr)
		if err != nil {
			return model.Address{}, err
		}
		dpt, expr = typ, body
		if length > 0 {
			// 宽度由 DPT 决定，length 在这里没有含义：1 位 DPT 的值在
			// APCI 字节里，2 字节 DPT 就 2 字节。允许写 length 只会让人
			// 以为可以「多读几个字节」。
			return model.Address{}, fmt.Errorf(
				"length is not expressed by this protocol (the width comes from the datapoint type), got %d", length)
		}
	}
	// 符号地址的显式长度：`NAME:81`。只在协议允许时切分；否则冒号
	// 属于符号名（虽然极少见），不能擅自截断。
	if pol.AllowSymbolLength {
		if i := strings.LastIndexByte(expr, ':'); i > 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(expr[i+1:])); err == nil && n > 0 {
				if length > 0 && length != n {
					return model.Address{}, fmt.Errorf(
						"length is given twice with different values (%d in address, %d in length column)", n, length)
				}
				expr, length = expr[:i], n
			}
		}
	}
	a, err := parseSymbolAddress(expr, pol.SymbolIndexInPath, dpt)
	if err != nil {
		return model.Address{}, err
	}
	if a.Selector != "" && !pol.AllowSelector {
		return model.Address{}, fmt.Errorf("protocol %q does not accept selector addresses (got %q)", protocol, expr)
	}
	if length > 0 {
		if !pol.AllowSymbolLength {
			return model.Address{}, fmt.Errorf("length is only valid for register addresses")
		}
		if length > 65535 {
			return model.Address{}, fmt.Errorf("length %d exceeds 65535", length)
		}
		a.Length = length
	}
	return a, nil
}

// splitRegisterPrefix 识别 `holding:...`、`db12:...` 这类前缀。
// db 号可以写成 `db12` 或 `db:12`：前者是 Canonical 的形式，后者是现场
// 更习惯的写法；两种都接受，但输出永远只有一种。
func splitRegisterPrefix(expr string) (area string, db int, rest string, ok bool) {
	colon := strings.IndexByte(expr, ':')
	if colon <= 0 {
		return "", 0, "", false
	}
	head := expr[:colon]
	lower := strings.ToLower(head)

	if strings.HasPrefix(lower, "db") {
		numPart := head[2:]
		if numPart == "" {
			// `db:12:0:4`：冒号后面第一个字段是 DB 号。
			parts := strings.Split(expr[colon+1:], ":")
			if len(parts) < 2 {
				return "", 0, "", false
			}
			n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				return "", 0, "", false
			}
			return "db", n, strings.Join(parts[1:], ":"), true
		}
		n, err := strconv.Atoi(numPart)
		if err != nil {
			return "", 0, "", false
		}
		return "db", n, expr[colon+1:], true
	}

	switch lower {
	case "holding", "input", "coil", "discrete", "output", "merker", "counter", "timer",
		"ioa", "single-command", "double-command",
		"setpoint-float", "setpoint-scaled", "setpoint-normalized",
		"x", "y", "r", "l", "t", "c", "dt", "ld", "fl",
		"ix", "iy", "id", "sv", "ev",
		// FINS 内存区（FINS/TCP 与 Host Link 共用）。别名 D/W/H/A 在这里
		// 归一化成规范名，这样点表里写 `d:0` 与 `dm:0` 产出同一个地址，
		// 与协议 codec 的 Lookup 行为一致。
		"cio", "wr", "hr", "ar", "dm", "em",
		"d", "w", "h", "a":
		if alias, ok := finsAliases[lower]; ok {
			lower = alias
		}
		return lower, 0, expr[colon+1:], true
	}
	// EM 区按 bank 编号写：em0..em12。
	if rest, ok := strings.CutPrefix(lower, "em"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 && n <= 12 {
			return lower, 0, expr[colon+1:], true
		}
	}
	return "", 0, "", false
}

func parseRegister(area string, db int, rest string, dt model.DataType, pol AddressPolicy, hint int) (model.Address, error) {
	if rest == "" {
		return model.Address{}, fmt.Errorf("register address misses offset")
	}
	parts := strings.Split(rest, ":")
	if len(parts) > 2 {
		return model.Address{}, fmt.Errorf("register address has too many fields: %q", rest)
	}
	offPart := strings.TrimSpace(parts[0])
	offset := 0
	bit := -1
	if i := strings.IndexByte(offPart, '.'); i >= 0 {
		n, err := strconv.Atoi(offPart[:i])
		if err != nil || n < 0 {
			return model.Address{}, fmt.Errorf("offset %q is not a non-negative integer", offPart[:i])
		}
		b, err := strconv.Atoi(offPart[i+1:])
		if err != nil || b < 0 {
			return model.Address{}, fmt.Errorf("bit %q is not a non-negative integer", offPart[i+1:])
		}
		offset, bit = n, b
		if dt != model.TypeBool {
			return model.Address{}, fmt.Errorf("address.bit is only valid for bool points, got %s", dt)
		}
		if area == "db" && b > 7 {
			return model.Address{}, fmt.Errorf("S7 bit must be 0..7, got %d", b)
		}
		if b > 15 {
			return model.Address{}, fmt.Errorf("bit must be 0..15, got %d", b)
		}
	} else {
		n, err := strconv.Atoi(offPart)
		if err != nil || n < 0 {
			return model.Address{}, fmt.Errorf("offset %q is not a non-negative integer", offPart)
		}
		offset = n
	}

	length := 0
	explicit := false
	if len(parts) == 2 {
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || n <= 0 {
			return model.Address{}, fmt.Errorf("length %q is not a positive integer", parts[1])
		}
		if hint > 0 && hint != n {
			return model.Address{}, fmt.Errorf("length is given twice with different values (%d in address, %d in length column)", n, hint)
		}
		length, explicit = n, true
	} else if hint > 0 {
		length, explicit = hint, true
	}
	if explicit && pol.Width == WidthNone {
		// 宽度由协议元素决定时，地址里写 length 不表达任何东西：同一个
		// `ioa:1001` 读成 float32 还是 int16 由点位的 dataType 与 ASDU 类型
		// 决定，多写一个 2 只会让驱动在 Open 阶段拒绝整条点位。宁可在导入
		// 期说清楚「这里不该有长度」，也不要留一条启动后才炸的点表。
		return model.Address{}, fmt.Errorf(
			"%s: length is not expressed by this protocol (the value width comes from the data type), got %d",
			area, length)
	}
	if pol.Width == WidthNone && bit >= 0 {
		// 远动协议没有「寄存器里的第 n 位」：一个布尔量就是一个单点对象，
		// 位号写在这里没有含义。驱动会拒，但在导入期拒可以说清原因。
		return model.Address{}, fmt.Errorf(
			"%s: bit addressing is not used by this protocol (a bool point is a single-point object), got bit %d",
			area, bit)
	}
	if !explicit && pol.Width != WidthNone {
		// 位访问的宽度恒为 1 字节/1 个线圈：bool 没有「类型宽度」，
		// 如果走通用推导会得到「长度必须显式给出」这种莫名其妙的错误。
		if dt == model.TypeBool && (area == "coil" || area == "discrete" || bit >= 0) {
			length = 1
		} else {
			n, err := deriveLength(area, dt, pol.Width)
			if err != nil {
				return model.Address{}, err
			}
			length = n
		}
	}
	if length > 125 {
		return model.Address{}, fmt.Errorf("length %d must be <= 125", length)
	}
	if dt == model.TypeBool && area != "coil" && area != "discrete" && bit < 0 &&
		pol.Width != WidthNone && !pol.BitlessAreas[area] {
		return model.Address{}, fmt.Errorf("bool point needs a bit (e.g. %s:%d.0)", area, offset)
	}

	a := model.Address{Kind: model.AddressRegister, Area: area, DB: db, Offset: offset, Length: length, Bit: bit}
	if err := a.Validate(); err != nil {
		return model.Address{}, err
	}
	return a, nil
}

// parseDIAddress 解析「数据标识型」寄存器地址：`DI[:offset[:length]]`。
//
// 三种写法都接受，且都产出同一个结构化地址：
//
//	02010100          A 相电压（宽度由数据项定义决定）
//	02010100:0        offset 显式写 0（有的点表模板省不掉这一列）
//	0201FF00:0:20     电压数据块，显式给出期望长度
//
// offset 的含义由 AddressPolicy.DIField 决定：645 的一个 DI 就是一个值，
// offset 只能是 0；188 的一个 DI 是一组字段，offset 是数据域内的字节偏移。
func parseDIAddress(expr string, dt model.DataType, pol AddressPolicy, hint int) (model.Address, error) {
	parts := strings.Split(expr, ":")
	if len(parts) > 3 {
		return model.Address{}, fmt.Errorf("DI address has too many fields: %q (want DI[:offset[:length]])", expr)
	}
	di := strings.ToUpper(strings.TrimSpace(parts[0]))
	if len(di) != pol.DIBits {
		return model.Address{}, fmt.Errorf("DI %q must be %d hex digits", di, pol.DIBits)
	}
	for _, r := range di {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F') {
			return model.Address{}, fmt.Errorf("DI %q contains non-hex character %q", di, r)
		}
	}
	if di == strings.Repeat("F", pol.DIBits) {
		// 全 FF 既不是合法数据项，也不是任何标准数据块；它通常来自
		// 「地址还没填」的模板。位数随协议变化（645 是 8 位，188 是 4 位）。
		return model.Address{}, fmt.Errorf("DI %q is not a valid data identifier", di)
	}

	offset := 0
	if len(parts) >= 2 {
		off := strings.TrimSpace(parts[1])
		if off != "" {
			n, err := strconv.Atoi(off)
			if err != nil || n < 0 {
				return model.Address{}, fmt.Errorf("DI address offset %q is not a non-negative integer", off)
			}
			if !pol.DIField && n != 0 {
				// 645 的一个 DI 就是一个值：写别的偏移一定是把寄存器型
				// 协议的地址抄过来了。
				return model.Address{}, fmt.Errorf("DI address offset must be 0, got %q", off)
			}
			offset = n
		}
	}
	length := 0
	if len(parts) == 3 {
		n, err := strconv.Atoi(strings.TrimSpace(parts[2]))
		if err != nil || n <= 0 {
			return model.Address{}, fmt.Errorf("length %q is not a positive integer", parts[2])
		}
		if hint > 0 && hint != n {
			return model.Address{}, fmt.Errorf("length is given twice with different values (%d in address, %d in length column)", n, hint)
		}
		length = n
	} else if hint > 0 {
		length = hint
	}
	if length > 125 {
		// 这是模型的统一上限（Span 合并、北向展示都按它假设）。数据块
		// 常超过它——那种情况省略 length，宽度由应答决定。
		return model.Address{}, fmt.Errorf("length %d must be <= 125 (omit it for large data blocks)", length)
	}
	if dt == model.TypeBool {
		return model.Address{}, fmt.Errorf("protocol does not support bit addressing; read the status word as uint16")
	}
	if pol.Width != WidthProtocol && length == 0 {
		return model.Address{}, fmt.Errorf("length is required for %s (protocol width is unknown)", dt)
	}

	a := model.Address{Kind: model.AddressRegister, Area: di, Offset: offset, Length: length, Bit: -1}
	if err := a.Validate(); err != nil {
		return model.Address{}, err
	}
	return a, nil
}

func deriveLength(area string, dt model.DataType, mode WidthMode) (int, error) {
	if area == "coil" || area == "discrete" {
		return 1, nil
	}
	// bool 没有类型宽度，但寄存器里的一个位地址仍然占一个寄存器（Modbus）
	// 或一个字节（S7）。缺 bit 的错误由调用方给出，比「length is required
	// for bool」更好懂。
	if dt == model.TypeBool {
		return 1, nil
	}
	if mode == WidthNone {
		// 协议元素自带宽度，地址里没有位置放它，也不需要放。
		return 0, nil
	}
	if mode == WidthExplicit {
		return 0, fmt.Errorf("length is required for %s (protocol width is unknown)", dt)
	}
	if mode == WidthProtocol {
		return 0, fmt.Errorf("width of %s is defined by the protocol, not by the data type", dt)
	}
	width := dt.Bytes()
	if width <= 0 {
		return 0, fmt.Errorf("length is required for %s", dt)
	}
	if mode == WidthRegisters {
		if width%2 != 0 {
			return 0, fmt.Errorf("cannot derive register count for %s", dt)
		}
		return width / 2, nil
	}
	if dt == model.TypeBool {
		return 1, nil
	}
	return width, nil
}

func parseSymbolAddress(expr string, keepIndexInPath bool, dpt string) (model.Address, error) {
	path, selector := expr, ""
	if i := strings.IndexByte(expr, '#'); i >= 0 {
		path, selector = expr[:i], expr[i+1:]
		if path == "" {
			return model.Address{}, fmt.Errorf("symbol path before '#' is empty")
		}
		if selector == "" {
			return model.Address{}, fmt.Errorf("selector after '#' is empty")
		}
	}
	index := -1
	if !keepIndexInPath && strings.HasSuffix(path, "]") {
		if i := strings.LastIndexByte(path, '['); i >= 0 {
			n, err := strconv.Atoi(path[i+1 : len(path)-1])
			if err != nil || n < 0 {
				return model.Address{}, fmt.Errorf("symbol index %q is not a non-negative integer", path[i:])
			}
			path, index = path[:i], n
		}
	}
	a := model.Address{Kind: model.AddressSymbol, Path: path, Index: index, Selector: selector, DPT: dpt}
	if err := a.Validate(); err != nil {
		return model.Address{}, err
	}
	return a, nil
}

// splitDPT 从符号地址里切出 `@<DPT>`。
//
// DPT 必填：没有它，驱动不知道这条点位该读几个字节。空组地址、空 DPT
// 都在这里给出明确错误，而不是留到驱动 Open 阶段才炸出「不支持的地址」。
//
// 组地址文本里的空白会被去掉：`1 / 2 / 3` 与 `1/2/3` 是同一个组地址，
// 协议 codec 也按这个规则解析。留着空白不会让地址更清楚，只会让两处
// 相同的地址在点表里显示成两个不同的字符串。
func splitDPT(expr string) (body, dpt string, err error) {
	i := strings.LastIndexByte(expr, '@')
	if i < 0 {
		return "", "", errors.New(
			"dpt is required (e.g. 1/2/3@9.001): group addresses carry no type information")
	}
	body = strings.Join(strings.Fields(expr[:i]), "")
	dpt = strings.TrimSpace(expr[i+1:])
	if body == "" {
		return "", "", fmt.Errorf("group address before '@' is empty (got %q)", expr)
	}
	if dpt == "" {
		return "", "", fmt.Errorf("dpt after '@' is empty (got %q)", expr)
	}
	if strings.ContainsRune(dpt, ':') {
		// `1/2/3@9.001:2`：长度在这里没有含义，宽度由 DPT 决定。单独报
		// 这一条，否则会被当成「DPT 写错了」，让人去改一个本来对的类型。
		return "", "", fmt.Errorf(
			"length is not expressed by this protocol (the width comes from the datapoint type): %q", expr)
	}
	return body, dpt, nil
}

func parseObjectAddress(expr string) (model.Address, bool, error) {
	colon := strings.IndexByte(expr, ':')
	if colon <= 0 {
		return model.Address{}, false, nil
	}
	typ := expr[:colon]
	if !isObjectIdent(typ) {
		return model.Address{}, false, nil
	}
	rest := expr[colon+1:]
	dot := strings.IndexByte(rest, '.')
	if dot <= 0 {
		return model.Address{}, false, nil
	}
	inst, err := strconv.Atoi(rest[:dot])
	if err != nil || inst < 0 {
		return model.Address{}, false, nil
	}
	prop := rest[dot+1:]
	idx := 0
	if strings.HasSuffix(prop, "]") {
		if i := strings.LastIndexByte(prop, '['); i >= 0 {
			n, err := strconv.Atoi(prop[i+1 : len(prop)-1])
			if err != nil || n < 0 {
				return model.Address{}, true, fmt.Errorf("property index %q is not a non-negative integer", prop[i:])
			}
			prop, idx = prop[:i], n
		}
	}
	if prop == "" {
		return model.Address{}, true, fmt.Errorf("object property is empty")
	}
	a := model.Address{Kind: model.AddressObject, ObjectType: typ, Instance: inst, Property: prop, PropIndex: idx}
	if err := a.Validate(); err != nil {
		return model.Address{}, true, err
	}
	return a, true, nil
}

func parseFileAddress(expr string) (model.Address, error) {
	// file:<fileId>/<recordId>:<offset>
	body := strings.TrimPrefix(expr, "file:")
	slash := strings.IndexByte(body, '/')
	colon := strings.LastIndexByte(body, ':')
	if slash < 0 || colon <= slash {
		return model.Address{}, fmt.Errorf("file address must be file:<fileId>/<recordId>:<offset>")
	}
	fileID, err1 := strconv.Atoi(body[:slash])
	recordID, err2 := strconv.Atoi(body[slash+1 : colon])
	offset, err3 := strconv.Atoi(body[colon+1:])
	if err1 != nil || err2 != nil || err3 != nil || fileID < 0 || recordID < 0 || offset < 0 {
		return model.Address{}, fmt.Errorf("file address %q has a non-numeric field", expr)
	}
	a := model.Address{Kind: model.AddressFile, FileID: fileID, RecordID: recordID, Offset: offset}
	if err := a.Validate(); err != nil {
		return model.Address{}, err
	}
	return a, nil
}

// isObjectIdent 校验对象类型那一段：允许字母、数字、下划线与连字符，且允许以数字开头。
//
// 为什么不用 isIdent（禁止首字符为数字、禁止连字符）：BACnet 的对象类型
// 有 `analog-input` 这种带连字符的规范名，也允许直接写类型编号（`0` 就是
// Analog Input）。真正的合法性（这个名字到底对应哪个类型编号、编号是否在
// 0..4194303 内）由 codec/bacnet 的 ObjectTypeFromName 在驱动层判定；这里只
// 保证点表文本结构能被拆出来，不越权替协议做语义校验。
func isObjectIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func joinKeys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 固定顺序，错误信息才可测试。
	sort.Strings(out)
	return strings.Join(out, "|")
}
