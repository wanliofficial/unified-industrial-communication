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
)

// AddressPolicy 描述某协议接受的地址形态。
type AddressPolicy struct {
	Width WidthMode
	// RegisterAreas 为空表示不限制（未知协议）。
	RegisterAreas map[string]bool
	// AllowRegister/Symbol/Object/File/Computed 是地址种类白名单。
	AllowRegister bool
	AllowSymbol   bool
	AllowObject   bool
	AllowFile     bool
	AllowComputed bool
	// AllowSelector 表示符号地址可以带 `#selector`（MQTT 载荷字段）。
	// 其他协议带 selector 多半是把 MQTT 主题贴错了列，应当在导入时拦下。
	AllowSelector bool
}

var (
	modbusAreas = map[string]bool{"holding": true, "input": true, "coil": true, "discrete": true}
	s7Areas     = map[string]bool{
		"db": true, "input": true, "output": true, "merker": true,
		"counter": true, "timer": true,
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
	case "mqtt":
		return AddressPolicy{AllowSymbol: true, AllowSelector: true}
	case "computed":
		return AddressPolicy{AllowComputed: true}
	case "virtual":
		return AddressPolicy{
			Width: WidthBytes, AllowRegister: true, AllowSymbol: true,
			AllowComputed: true, AllowSelector: true,
		}
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
	a, err := parseSymbolAddress(expr)
	if err != nil {
		return model.Address{}, err
	}
	if a.Selector != "" && !pol.AllowSelector {
		return model.Address{}, fmt.Errorf("protocol %q does not accept selector addresses (got %q)", protocol, expr)
	}
	if length > 0 {
		return model.Address{}, fmt.Errorf("length is only valid for register addresses")
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
	case "holding", "input", "coil", "discrete", "output", "merker", "counter", "timer":
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
	if !explicit {
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
	if dt == model.TypeBool && area != "coil" && area != "discrete" && bit < 0 {
		return model.Address{}, fmt.Errorf("bool point needs a bit (e.g. %s:%d.0)", area, offset)
	}

	a := model.Address{Kind: model.AddressRegister, Area: area, DB: db, Offset: offset, Length: length, Bit: bit}
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
	if mode == WidthExplicit {
		return 0, fmt.Errorf("length is required for %s (protocol width is unknown)", dt)
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

func parseSymbolAddress(expr string) (model.Address, error) {
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
	if strings.HasSuffix(path, "]") {
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
