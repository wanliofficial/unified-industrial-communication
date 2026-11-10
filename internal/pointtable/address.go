package pointtable

import (
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
	a, err := parseSymbolAddress(expr, pol.SymbolIndexInPath)
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
		"ix", "iy", "id", "sv", "ev":
		return lower, 0, expr[colon+1:], true
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

func parseSymbolAddress(expr string, keepIndexInPath bool) (model.Address, error) {
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
	a := model.Address{Kind: model.AddressSymbol, Path: path, Index: index, Selector: selector}
	if err := a.Validate(); err != nil {
		return model.Address{}, err
	}
	return a, nil
}

func parseObjectAddress(expr string) (model.Address, bool, error) {
	colon := strings.IndexByte(expr, ':')
	if colon <= 0 {
		return model.Address{}, false, nil
	}
	typ := expr[:colon]
	if !isIdent(typ) {
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

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
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
