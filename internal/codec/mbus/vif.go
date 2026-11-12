package mbus

import (
	"fmt"
	"sort"
	"strings"
)

// 本文件把 VIF/VIFE 翻译成「物理量名 + 单位 + 换算系数」。
//
// M-Bus 的 VIF 是**编码后的量纲**：同一族物理量共享一段连续的编码，用低
// 3 位表示 10 的幂。例如 0x00..0x07 都是"能量，单位 Wh"，最低位给出
// 1e-3、1e-2 … 1e4 的倍率。因此手写 128 行表既冗长又容易抄错——这里用
// 区间 + 幂次生成，再补上不成规律的几个（日期、字符串、HC A 等）。
//
// 换算系数 Scale 的含义：**工程值 = 原始整数 × Scale**。驱动始终按这个
// 规则把值归一到"表里写的单位"，与记录在报文中用了 0.001 Wh 还是 10000 Wh
// 无关——这样同一个点表 `q:energy` 能跨表厂通用，不会因为换了一块表
// 就从"1234.5 kWh"变成"1234500 Wh"。
//
// Quantity 是点表里 `q:` 后面那个名字，必须稳定、ASCII、无空格：
// `energy`、`volume-flow`、`flow-temperature`……

// Special 描述"这条记录的值不是普通整数"的几种例外。
type Special byte

const (
	// SpecialNone 是普通数值记录。
	SpecialNone Special = iota
	// SpecialDate 是 Type G 日期（2 字节）。
	SpecialDate
	// SpecialDateTime 是 Type F/I 日期时间（4 或 6 字节）。
	SpecialDateTime
	// SpecialCustomVIF 表示 VIF=7Ch/FCh：VIF 之后紧跟长度 + ASCII 量名。
	SpecialCustomVIF
	// SpecialManufacturer 表示厂商自定义 VIF（7Fh/FFh）：语义不可知，
	// 值按原始字节暴露。
	SpecialManufacturer
)

// VIFInfo 是一条记录的物理量语义。
type VIFInfo struct {
	// Quantity 是规范化物理量键，点表用它寻址（`q:energy`）。
	Quantity string
	// Unit 是工程单位。注释里的写法尽量贴近 I 列单位：m^3、°C、J/h。
	Unit string
	// Scale 把原始整数换算成 Unit 表示的值。
	Scale float64
	// Special 标记日期/字符串/厂商特定等非普通数值。
	Special Special
}

// Pow10 返回 10 的 n 次方。负指数按浮点算（1e-6 等）。
func Pow10(n int) float64 {
	v := 1.0
	if n >= 0 {
		for i := 0; i < n; i++ {
			v *= 10
		}
		return v
	}
	for i := 0; i > n; i-- {
		v /= 10
	}
	return v
}

// 时长类 VIF 的倍率：秒、分、时、天。
var durationScale = [4]float64{1, 60, 3600, 86400}

// LookupVIF 返回 VIF 的物理量语义。
//
// vif 是原始字节（扩展位 0x80 保留）；vifes 是紧随其后的 VIFE 序列。
// 只有当 VIF 为 0xFD（"主 VIFE 表"）时 VIFE 才改变物理量本身；其余情况
// VIFE 只是修饰（例如"最大值""上次读数以来"），物理量仍由 VIF 决定。
func LookupVIF(vif byte, vifes []byte) VIFInfo {
	base := vif & 0x7F

	// 0x7C/0xFC：VIF 是 ASCII 字符串，由解析器另行读取。
	if base == 0x7C {
		return VIFInfo{Quantity: "custom", Scale: 1, Special: SpecialCustomVIF}
	}
	// 0x7D/0xFD：真正的物理量在 VIFE 主表里。
	if base == 0x7D && len(vifes) > 0 {
		return lookupVIFE(vifes[0])
	}
	if info, ok := primaryVIF(base); ok {
		return info
	}
	if base == 0x7F || vif == 0xFF {
		return VIFInfo{Quantity: "manufacturer-specific", Scale: 1, Special: SpecialManufacturer}
	}
	return VIFInfo{Quantity: fmt.Sprintf("vif-%02x", base), Scale: 1}
}

// primaryVIF 处理 0x00..0x7F 的主表。
func primaryVIF(v byte) (VIFInfo, bool) {
	switch {
	case v <= 0x07:
		return VIFInfo{Quantity: "energy", Unit: "Wh", Scale: 1e-3 * Pow10(int(v&0x07))}, true
	case v <= 0x0F:
		return VIFInfo{Quantity: "energy", Unit: "J", Scale: Pow10(int(v & 0x07))}, true
	case v <= 0x17:
		return VIFInfo{Quantity: "volume", Unit: "m^3", Scale: 1e-6 * Pow10(int(v&0x07))}, true
	case v <= 0x1F:
		return VIFInfo{Quantity: "mass", Unit: "kg", Scale: 1e-3 * Pow10(int(v&0x07))}, true
	case v <= 0x23:
		return VIFInfo{Quantity: "on-time", Unit: "s", Scale: durationScale[v&0x03]}, true
	case v <= 0x27:
		return VIFInfo{Quantity: "operating-time", Unit: "s", Scale: durationScale[v&0x03]}, true
	case v <= 0x2F:
		return VIFInfo{Quantity: "power", Unit: "W", Scale: 1e-3 * Pow10(int(v&0x07))}, true
	case v <= 0x37:
		return VIFInfo{Quantity: "power", Unit: "J/h", Scale: Pow10(int(v & 0x07))}, true
	case v <= 0x3F:
		return VIFInfo{Quantity: "volume-flow", Unit: "m^3/h", Scale: 1e-6 * Pow10(int(v&0x07))}, true
	case v <= 0x47:
		return VIFInfo{Quantity: "volume-flow", Unit: "m^3/min", Scale: 1e-7 * Pow10(int(v&0x07))}, true
	case v <= 0x4F:
		return VIFInfo{Quantity: "volume-flow", Unit: "m^3/s", Scale: 1e-9 * Pow10(int(v&0x07))}, true
	case v <= 0x57:
		return VIFInfo{Quantity: "mass-flow", Unit: "kg/h", Scale: 1e-3 * Pow10(int(v&0x07))}, true
	case v <= 0x5B:
		return VIFInfo{Quantity: "flow-temperature", Unit: "°C", Scale: 1e-3 * Pow10(int(v&0x03))}, true
	case v <= 0x5F:
		return VIFInfo{Quantity: "return-temperature", Unit: "°C", Scale: 1e-3 * Pow10(int(v&0x03))}, true
	case v <= 0x63:
		return VIFInfo{Quantity: "temperature-difference", Unit: "K", Scale: 1e-3 * Pow10(int(v&0x03))}, true
	case v <= 0x67:
		return VIFInfo{Quantity: "external-temperature", Unit: "°C", Scale: 1e-3 * Pow10(int(v&0x03))}, true
	case v <= 0x6B:
		return VIFInfo{Quantity: "pressure", Unit: "bar", Scale: 1e-3 * Pow10(int(v&0x03))}, true
	}

	// 以下是表里不成规律的几个。顺序无关，用的是精确匹配。
	switch v {
	case 0x6C:
		return VIFInfo{Quantity: "date", Unit: "", Scale: 1, Special: SpecialDate}, true
	case 0x6D:
		return VIFInfo{Quantity: "date-time", Unit: "", Scale: 1, Special: SpecialDateTime}, true
	case 0x6E:
		return VIFInfo{Quantity: "hca", Unit: "H.C.A.", Scale: 1}, true
	case 0x6F:
		return VIFInfo{Quantity: "reserved-6f", Scale: 1}, true
	case 0x70, 0x71, 0x72, 0x73:
		return VIFInfo{Quantity: "averaging-duration", Unit: "s", Scale: durationScale[v&0x03]}, true
	case 0x74, 0x75, 0x76, 0x77:
		return VIFInfo{Quantity: "actuality-duration", Unit: "s", Scale: durationScale[v&0x03]}, true
	case 0x78:
		// 出厂编号：通常按 BCD/整数暴露，没有工程单位。
		return VIFInfo{Quantity: "fabrication-no", Scale: 1}, true
	case 0x79:
		return VIFInfo{Quantity: "identification", Scale: 1}, true
	case 0x7A:
		return VIFInfo{Quantity: "bus-address", Scale: 1}, true
	case 0x7B, 0x7C, 0x7D, 0x7E:
		// 0x7B/0x7C/0x7D 由调用方先拦掉；0x7E 是"任意 VIF"。
		return VIFInfo{Quantity: "any-vif", Scale: 1}, true
	case 0x7F:
		return VIFInfo{Quantity: "manufacturer-specific", Scale: 1, Special: SpecialManufacturer}, true
	}
	return VIFInfo{}, false
}

// vifeNames 是主 VIFE 表（VIF=FDh 时使用，编码范围 0x100..0x1FF）。
//
// 这里只收录"有明确语义、现场会用到"的那部分：计费（credit/debit）、
// 设备信息（介质/厂商/版本）、错误标志、数字量、通信参数、存储周期。
// 未收录的编码按 `vife-xx` 暴露，值本身仍然可读——不知道名字不应该
// 妨碍把数取上来，只是不能按名字寻址。
var vifeNames = map[byte]VIFInfo{
	0x08: {Quantity: "access-number", Scale: 1},
	0x09: {Quantity: "medium", Scale: 1},
	0x0A: {Quantity: "manufacturer", Scale: 1},
	0x0B: {Quantity: "parameter-set", Scale: 1},
	0x0C: {Quantity: "model-version", Scale: 1},
	0x0D: {Quantity: "hardware-version", Scale: 1},
	0x0E: {Quantity: "firmware-version", Scale: 1},
	0x0F: {Quantity: "software-version", Scale: 1},
	0x10: {Quantity: "customer-location", Scale: 1},
	0x11: {Quantity: "customer", Scale: 1},
	0x12: {Quantity: "access-code-user", Scale: 1},
	0x13: {Quantity: "access-code-operator", Scale: 1},
	0x14: {Quantity: "access-code-system", Scale: 1},
	0x15: {Quantity: "access-code-developer", Scale: 1},
	0x16: {Quantity: "password", Scale: 1},
	0x17: {Quantity: "error-flags", Scale: 1},
	0x18: {Quantity: "error-mask", Scale: 1},
	0x1A: {Quantity: "digital-output", Scale: 1},
	0x1B: {Quantity: "digital-input", Scale: 1},
	0x1C: {Quantity: "baudrate", Unit: "Baud", Scale: 1},
	0x1D: {Quantity: "response-delay", Unit: "bittimes", Scale: 1},
	0x1E: {Quantity: "retry", Scale: 1},
	0x20: {Quantity: "first-storage", Scale: 1},
	0x21: {Quantity: "last-storage", Scale: 1},
	0x22: {Quantity: "storage-block-size", Scale: 1},
	0x23: {Quantity: "reserved-23", Scale: 1},
	0x24: {Quantity: "storage-interval", Unit: "s", Scale: durationScale[0]},
	0x25: {Quantity: "storage-interval", Unit: "s", Scale: durationScale[1]},
	0x26: {Quantity: "storage-interval", Unit: "s", Scale: durationScale[2]},
	0x27: {Quantity: "storage-interval", Unit: "s", Scale: durationScale[3]},
	0x28: {Quantity: "storage-interval-month", Unit: "month", Scale: 1},
	0x29: {Quantity: "storage-interval-year", Unit: "year", Scale: 1},
	0x2C: {Quantity: "duration-since-last-readout", Unit: "s", Scale: durationScale[0]},
	0x2D: {Quantity: "duration-since-last-readout", Unit: "s", Scale: durationScale[1]},
	0x2E: {Quantity: "duration-since-last-readout", Unit: "s", Scale: durationScale[2]},
	0x2F: {Quantity: "duration-since-last-readout", Unit: "s", Scale: durationScale[3]},
}

// lookupVIFE 解主 VIFE 表。0x00..0x07 是"计费单位"，按 10 的幂编码；
// 其余查表，查不到就退化成编码名。
func lookupVIFE(vife byte) VIFInfo {
	v := vife & 0x7F
	if v <= 0x03 {
		return VIFInfo{Quantity: "credit", Unit: "currency", Scale: 1e-3 * Pow10(int(v&0x03))}
	}
	if v <= 0x07 {
		return VIFInfo{Quantity: "debit", Unit: "currency", Scale: 1e-3 * Pow10(int(v&0x03))}
	}
	if info, ok := vifeNames[v]; ok {
		return info
	}
	return VIFInfo{Quantity: fmt.Sprintf("vife-%02x", v), Scale: 1}
}

// QuantityKeys 返回全部可寻址的物理量键（去重、排序），供帮助信息与
// 报错时列出候选：地址写错时列出"能填什么"比只说"找不到"有用。
func QuantityKeys() []string {
	set := make(map[string]bool)
	for v := 0; v <= 0x7F; v++ {
		info := LookupVIF(byte(v), nil)
		if info.Quantity != "" {
			set[info.Quantity] = true
		}
	}
	for _, info := range vifeNames {
		set[info.Quantity] = true
	}
	set["credit"] = true
	set["debit"] = true
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FunctionSuffix 把 DIF 的功能位翻译成量名后缀。
//
// 功能位区分「瞬时值 / 最大值 / 最小值 / 故障时的值」。热表常同时上送
// 瞬时功率与"最大功率"，两者物理量都是 power；如果量名不加后缀，
// `q:power` 就会在两条记录之间摇摆。加后缀是唯一稳定的做法：
//
//	0 瞬时  → 无后缀        q:power
//	1 最大值 → -max          q:power-max
//	2 最小值 → -min          q:power-min
//	3 故障值 → -error        q:power-error
func FunctionSuffix(function byte) string {
	switch function & 0x03 {
	case 1:
		return "-max"
	case 2:
		return "-min"
	case 3:
		return "-error"
	}
	return ""
}

// NormalizeQuantity 把用户写的量名归一成小写、下划线转连字符。
//
// 点表里写 `q:Flow_Temperature` 与 `q:flow-temperature` 指的是同一条记录；
// 大小写与下划线只是输入习惯，不应该变成"地址不存在"。
func NormalizeQuantity(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
