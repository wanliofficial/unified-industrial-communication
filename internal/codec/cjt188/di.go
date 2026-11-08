package cjt188

import (
	"fmt"
	"strconv"
	"strings"
)

// DI 是 2 字节数据标识。
//
// 字段名刻意用 Hi/Lo 而不是 DI0/DI1：标准把线上先传的那个字节叫 DI0，
// 而人在手册上看到的写法是「901F」——把 0x90 放在前面。用 Hi/Lo 表示
// 「手册顺序」、用 Bytes() 表示「线上顺序」，就不会在同一段代码里把两个
// 方向混着用。实测最常见的错误是把 901F 按手册顺序发成 90 1F，表一声
// 不吭（帧本身合法，只是没有这个数据项）。
type DI struct {
	Hi byte // 手册顺序的高字节（901F 的 0x90）
	Lo byte // 手册顺序的低字节（901F 的 0x1F）
}

// Bytes 返回**传输顺序**的 2 字节（Lo 在前）。
//
// 标准原文「所有多字节数据域均先传送低位字节」；901F 在线上是 1F 90。
func (d DI) Bytes() [2]byte { return [2]byte{d.Lo, d.Hi} }

// BytesLiteral 返回按 Hi、Lo 的字面量顺序写出的两个字节。
//
// 只有读地址命令（0x03）的报文标识是例外：标准与所有实现都把它写成
// 字面量 81 0A，不遵守「低字节先传」的变换。把这条命令硬塞进 Bytes()
// 的规则里，发出去就成了 0A 81，表一声不吭（帧合法、只是没有这个命令）。
// 需要它的地方只有从站回显；主站侧 ReadAddrRequest 直接用常量组帧。
func (d DI) BytesLiteral() [2]byte { return [2]byte{d.Hi, d.Lo} }

// String 返回手册书写顺序的 4 位十六进制（Hi 在前）。
func (d DI) String() string { return fmt.Sprintf("%02X%02X", d.Hi, d.Lo) }

// ParseDI 解析 4 位十六进制数据标识（手册顺序，如 901F）。
func ParseDI(s string) (DI, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F') {
			return DI{}, fmt.Errorf("cjt188: 数据标识 %q 含非十六进制字符 %q", s, r)
		}
	}
	if len(s) != 4 {
		return DI{}, fmt.Errorf("cjt188: 数据标识 %q 必须是 4 位十六进制（如 901F）", s)
	}
	n, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return DI{}, fmt.Errorf("cjt188: 数据标识 %q 非法: %w", s, err)
	}
	return DI{Hi: byte(n >> 8), Lo: byte(n)}, nil
}

// DIFromBytes 从**传输顺序**的 2 字节（Lo 在前）还原数据标识。
func DIFromBytes(b []byte) (DI, error) {
	if len(b) < 2 {
		return DI{}, fmt.Errorf("cjt188: 数据标识需要 2 字节，实际 %d", len(b))
	}
	return DI{Hi: b[1], Lo: b[0]}, nil
}

// 标准里用到的几个特殊标识。
//
// 读地址（810A）与写地址（A018）、阀门（A017）不是「可读数据项」，而是
// 命令报文里的报文标识；它们不参与 901F 的字段解析，单独列出是为了让
// 驱动与从站不必各写一遍裸字节。
var (
	// DIReadData 是读计量数据 1：当前累积流量/热量、结算日量、时间、状态。
	DIReadData = DI{Hi: 0x90, Lo: 0x1F}
	// DIValve 是阀门控制（写数据 0x04 的报文标识）。
	DIValve = DI{Hi: 0xA0, Lo: 0x17}
	// DIWriteAddr 是写地址（0x15 的报文标识）。
	DIWriteAddr = DI{Hi: 0xA0, Lo: 0x18}
	// DIReadAddr 是读地址（0x03 的报文标识）；线上固定 81 0A。
	DIReadAddr = DI{Hi: 0x81, Lo: 0x0A}
)

// ---------------------------------------------------------------------------
// 仪表类型与表计家族
// ---------------------------------------------------------------------------

// Family 是仪表家族。同一 DI 在不同家族里字段布局不同——901F 在热量表上
// 是「热量/功率/温度」，在水表上却是「流量/时间」；按表类型分派 schema
// 不是可选项。判错的症状是「读到了数，但流量变成了温度」。
type Family int

const (
	// FamilyWater 是水表（T=10H..19H）。
	FamilyWater Family = iota
	// FamilyHeat 是热量表（T=20H..29H）。
	FamilyHeat
	// FamilyGas 是燃气表（T=30H..39H）。
	FamilyGas
	// FamilyOther 是其他仪表（T=40H..49H）及未知类型。
	FamilyOther
)

func (f Family) String() string {
	switch f {
	case FamilyWater:
		return "水表"
	case FamilyHeat:
		return "热量表"
	case FamilyGas:
		return "燃气表"
	}
	return "其他仪表"
}

// FamilyOf 按仪表类型 T 的高 4 位判定家族。
//
// 标准把 T=30H..49H 与 T=10H..19H 归为同一套「累积流量」schema；因此
// FamilyGas/FamilyOther 在 901F 上走与水表相同的字段表，只有 FamilyHeat
// 单独一套。
func FamilyOf(t byte) Family {
	switch t >> 4 {
	case 0x1:
		return FamilyWater
	case 0x2:
		return FamilyHeat
	case 0x3:
		return FamilyGas
	}
	return FamilyOther
}

// TypeName 返回仪表类型的人读名称。
func TypeName(t byte) string {
	switch t {
	case 0x10:
		return "冷水水表"
	case 0x11:
		return "生活热水水表"
	case 0x12:
		return "直饮水水表"
	case 0x13:
		return "中水水表"
	case 0x20:
		return "热量表（计热量）"
	case 0x21:
		return "热量表（计冷量）"
	case 0x30:
		return "燃气表"
	case 0x40:
		return "电度表/其他仪表"
	}
	return fmt.Sprintf("未定义仪表类型 %02XH（按%s处理）", t, FamilyOf(t))
}

// ---------------------------------------------------------------------------
// 字段 schema
// ---------------------------------------------------------------------------

// Qty 是带单位字节的工程量种类。解码后统一换算到这些规范单位：
//
//	体积 m³ / 流量 m³/h / 能量 GJ / 功率 kW
//
// 不按「表上显示什么单位就返回什么单位」：同一批水表里 0x29（L）与 0x2C
// （m³）混装很常见，不归一化的话北向会把 1 m³ 和 1000 L 当成两个数量级。
type Qty int

const (
	// QtyNone 表示该字段没有单位字节（温度、工作时间等固定单位）。
	QtyNone Qty = iota
	// QtyVolume 是体积，规范单位 m³。
	QtyVolume
	// QtyFlow 是瞬时流量，规范单位 m³/h。
	QtyFlow
	// QtyEnergy 是热量/冷量，规范单位 GJ。
	QtyEnergy
	// QtyPower 是热功率，规范单位 kW。
	QtyPower
)

// Unit 返回规范单位符号。
func (q Qty) Unit() string {
	switch q {
	case QtyVolume:
		return "m³"
	case QtyFlow:
		return "m³/h"
	case QtyEnergy:
		return "GJ"
	case QtyPower:
		return "kW"
	}
	return ""
}

// FieldKind 决定一个字段怎么解码。
type FieldKind int

const (
	// FieldNumber 是压缩 BCD 数值，可能后跟 1 字节单位代号。
	FieldNumber FieldKind = iota
	// FieldTime 是 7 字节实时时间。
	FieldTime
	// FieldStatus 是 2 字节状态字。
	FieldStatus
)

// Field 是 DI 数据域里的一个字段。
//
// Offset 是**数据域内的字节偏移**（从 901F 应答里 DI+SER 之后的第一个
// 字节算起），不是寄存器偏移。点表里的 `901F:10` 就是指这个偏移。
type Field struct {
	Offset   int
	Length   int // BCD 字节数；FieldTime=7、FieldStatus=2
	Kind     FieldKind
	Decimals int // 仅 FieldNumber 有效
	Qty      Qty
	Name     string
}

// HasUnit 表示字段末尾跟一个单位代号字节。
func (f Field) HasUnit() bool { return f.Kind == FieldNumber && f.Qty != QtyNone }

// Width 是字段在数据域里占的总字节数（含单位字节）。
func (f Field) Width() int {
	if f.HasUnit() {
		return f.Length + 1
	}
	return f.Length
}

// End 是字段的结束偏移（不含）。
func (f Field) End() int { return f.Offset + f.Width() }

// String 返回用于诊断的「名称 @ 偏移」文本。
func (f Field) String() string { return fmt.Sprintf("%s@%d", f.Name, f.Offset) }

// waterSchema 是 T=10H..19H/30H..49H 的 901F 字段表。
//
//	0..4   当前累积流量   4 BCD + 单位
//	5..9   结算日累积流量 4 BCD + 单位
//	10..16 实时时间       7 BCD
//	17..18 状态           2 字节
var waterSchema = []Field{
	{Offset: 0, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: QtyVolume, Name: "当前累积流量"},
	{Offset: 5, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: QtyVolume, Name: "结算日累积流量"},
	{Offset: 10, Length: 7, Kind: FieldTime, Name: "实时时间"},
	{Offset: 17, Length: 2, Kind: FieldStatus, Name: "状态"},
}

// heatSchema 是 T=20H..29H 的 901F 字段表。
//
// 温度与累积工作时间是 3 字节 BCD、没有单位字节（单位固定为 ℃ 与 h）。
var heatSchema = []Field{
	{Offset: 0, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: QtyEnergy, Name: "结算日热量"},
	{Offset: 5, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: QtyEnergy, Name: "当前热量"},
	{Offset: 10, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: QtyPower, Name: "热功率"},
	{Offset: 15, Length: 4, Kind: FieldNumber, Decimals: 4, Qty: QtyFlow, Name: "瞬时流量"},
	{Offset: 20, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: QtyVolume, Name: "累积流量"},
	{Offset: 25, Length: 3, Kind: FieldNumber, Decimals: 2, Name: "供水温度"},
	{Offset: 28, Length: 3, Kind: FieldNumber, Decimals: 2, Name: "回水温度"},
	{Offset: 31, Length: 3, Kind: FieldNumber, Decimals: 0, Name: "累积工作时间"},
	{Offset: 34, Length: 7, Kind: FieldTime, Name: "实时时间"},
	{Offset: 41, Length: 2, Kind: FieldStatus, Name: "状态"},
}

// Fields 返回 (表类型, DI) 对应的字段表。
//
// 只实现 2004 版数据标识里真正会被点表用到的两类：
//
//   - 901F 当前计量数据（waterSchema / heatSchema）
//   - D120..D12B 上 1..12 月结算日量（动态生成）
//
// 其它 DI（911F、价格、金额、密钥等）不猜：未收录的 DI 让驱动直接报
// 「不认识这个数据标识」，比按 901F 的布局硬解出一组看似合理的值安全。
func Fields(t byte, di DI) ([]Field, bool) {
	fam := FamilyOf(t)
	switch {
	case di == DIReadData:
		if fam == FamilyHeat {
			return heatSchema, true
		}
		return waterSchema, true
	case di.Hi == 0xD1 && di.Lo >= 0x20 && di.Lo <= 0x2B:
		n := int(di.Lo-0x20) + 1
		qty := QtyVolume
		noun := "结算日累积流量"
		if fam == FamilyHeat {
			qty = QtyEnergy
			noun = "结算日热量"
		}
		return []Field{{
			Offset: 0, Length: 4, Kind: FieldNumber, Decimals: 2, Qty: qty,
			Name: fmt.Sprintf("上 %d 月%s", n, noun),
		}}, true
	}
	return nil, false
}

// LookupField 按偏移查找字段。
//
// 报错时把可用偏移一起列出来：现场最常见的问题是点表里写了 `901F:2`
// （把寄存器偏移的习惯带过来），而正确值是 0/5/10/17 —— 只回一句
// 「没有这个字段」等于让人回去翻标准。
func LookupField(t byte, di DI, offset int) (Field, error) {
	fields, ok := Fields(t, di)
	if !ok {
		return Field{}, fmt.Errorf("cjt188: 未收录的数据标识 %s（%s）；当前实现支持 901F 与 D120..D12B",
			di, TypeName(t))
	}
	for _, f := range fields {
		if f.Offset == offset {
			return f, nil
		}
	}
	offs := make([]string, 0, len(fields))
	for _, f := range fields {
		offs = append(offs, fmt.Sprintf("%d（%s）", f.Offset, f.Name))
	}
	return Field{}, fmt.Errorf("cjt188: 数据标识 %s 没有偏移 %d 的字段；可用偏移：%s",
		di, offset, strings.Join(offs, "、"))
}

// TotalWidth 返回整个 DI 数据域的字节数（不含 DI 与 SER）。
func TotalWidth(t byte, di DI) (int, error) {
	fields, ok := Fields(t, di)
	if !ok {
		return 0, fmt.Errorf("cjt188: 未收录的数据标识 %s", di)
	}
	if len(fields) == 0 {
		return 0, fmt.Errorf("cjt188: 数据标识 %s 没有字段定义", di)
	}
	return fields[len(fields)-1].End(), nil
}

// FieldList 返回用于帮助文本的字段清单。
func FieldList(t byte, di DI) string {
	fields, ok := Fields(t, di)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		unit := f.Qty.Unit()
		if f.Kind == FieldTime {
			unit = "时间"
		} else if f.Kind == FieldStatus {
			unit = "状态字"
		}
		if unit != "" {
			parts = append(parts, fmt.Sprintf("%d=%s(%s)", f.Offset, f.Name, unit))
		} else {
			parts = append(parts, fmt.Sprintf("%d=%s", f.Offset, f.Name))
		}
	}
	return strings.Join(parts, "，")
}
