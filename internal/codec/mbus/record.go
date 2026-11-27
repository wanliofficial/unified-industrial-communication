package mbus

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// 本文件解析 EN 13757-3 的两种数据结构：
//
//	CI=72h  可变数据结构（variable data structure）：12 字节固定头 + 自描述记录
//	CI=73h  固定数据结构（fixed data structure）：16 字节，计数器位置固定
//
// 可变结构是现在的主流；固定结构只在老式紧凑表（尤其是早期热量表）上遇到。
// 两种都必须能解，但**绝不能混用**：固定结构没有 DIF/VIF，把它的字节按
// 记录去解会得到一堆"看起来合理"的数字，而且不会有任何报错。

// Kind 是记录值的解释方式。
type Kind byte

const (
	// KindNone 表示该记录没有数据域（选择项/填充）。
	KindNone Kind = iota
	// KindNumeric 表示数值型记录，工程值在 Num。
	KindNumeric
	// KindText 表示 ASCII 文本或日期时间，文本在 Text。
	KindText
	// KindBinary 表示厂商特定的原始字节，字节在 Raw。
	KindBinary
)

func (k Kind) String() string {
	switch k {
	case KindNumeric:
		return "number"
	case KindText:
		return "text"
	case KindBinary:
		return "bytes"
	}
	return "none"
}

// Record 是一条解码后的数据记录。
//
// 字段的设计原则：**原始信息全部保留**。现场排查"为什么这个点读出来是
// 0.001 倍"时，能直接看到 DIF/VIF 与 Scale，比只看到一个换算后的浮点数
// 有用得多；而把 DIF/VIF 丢掉再想反推，需要拿报文回来重新解一遍。
type Record struct {
	// Index 是记录在报文中的位置（0 起），`r:<n>` 寻址用它。
	Index int
	// DIF 是数据信息块的首字节，DataField 是它的低 4 位（长度编码），
	// Function 是功能位（瞬时/最大/最小/故障）。
	DIF       byte
	DIFEs     []byte
	DataField byte
	Function  byte
	// VIF 是值信息块首字节，VIFEs 是后续扩展。
	VIF   byte
	VIFEs []byte
	// Storage/Tariff/Subunit 来自 DIFE：存储号（历史数据第几期）、
	// 费率号、子单元号。当前值通常是 0/0/0。
	Storage uint32
	Tariff  uint16
	Subunit byte

	// Quantity 是归一化物理量名（含功能后缀，如 power-max）。
	Quantity string
	// Unit 是工程单位；Scale 把 RawInt 换算成 Num（Num = RawInt * Scale）。
	Unit    string
	Scale   float64
	Special Special

	Kind   Kind
	Num    float64
	RawInt int64
	Text   string
	// Raw 是数据域原始字节，不含 DIF/VIF。
	Raw []byte
	// CustomVIF 是 VIF=7Ch 时表内自带的 ASCII 量名。
	CustomVIF string
}

// Header 是可变/固定数据结构共用的 12 字节固定头。
type Header struct {
	ID             uint32
	IDText         string
	Manufacturer   uint16
	ManufacturerID string
	Version        byte
	Medium         byte
	MediumName     string
	AccessNo       byte
	Status         byte
	Signature      uint16
}

// StatusError 判断状态字节是否报告了应用错误。
//
// 状态位（EN 13757-3 §6.6）：
//
//	bit 0-1  应用错误码（0 = 无错误）
//	bit 2    电源电压低
//	bit 3    永久错误
//	bit 4    暂时错误
//	bit 5-6  厂商特定
//	bit 7    厂商特定
//
// 只有 bit0-1 非零才是"这个值本身不可信"；电源低、永久错误说明表有
// 故障，但当前读数通常仍然有效。把两者混为一谈会让一大批电池快没电的
// 表整表变成坏质量。
func (h Header) StatusError() bool { return h.Status&0x03 != 0 }

// StatusText 给出状态位的可读解释。
func (h Header) StatusText() string {
	var parts []string
	if h.Status&0x03 != 0 {
		parts = append(parts, fmt.Sprintf("application-error(%d)", h.Status&0x03))
	}
	if h.Status&0x04 != 0 {
		parts = append(parts, "power-low")
	}
	if h.Status&0x08 != 0 {
		parts = append(parts, "permanent-error")
	}
	if h.Status&0x10 != 0 {
		parts = append(parts, "temporary-error")
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, ",")
}

// VariableData 是一条 CI=72h 应答解出的内容。
type VariableData struct {
	Header  Header
	Records []Record
	// MoreRecordsFollow 为真表示从站还有后续报文（DIF=1Fh）。驱动应当
	// 继续用 REQ_UD1/REQ_UD2 取回剩余部分，否则会静默丢掉后半张表。
	MoreRecordsFollow bool
	// VendorData 是 DIF=0Fh/1Fh 之后无法按标准解释的字节。
	VendorData []byte
}

// parseHeader 解 12 字节固定头。
func parseHeader(b []byte) (Header, error) {
	if len(b) < 12 {
		return Header{}, fmt.Errorf("%w: 可变数据结构头只有 %d 字节（需要 12）", ErrFrame, len(b))
	}
	var h Header
	h.ID = binary.LittleEndian.Uint32(b[0:4])
	h.IDText = BCDString(b[0:4])
	h.Manufacturer = binary.LittleEndian.Uint16(b[4:6])
	h.ManufacturerID = ManufacturerCode(b[4:6])
	h.Version = b[6]
	h.Medium = b[7]
	h.MediumName = MediaName(b[7])
	h.AccessNo = b[8]
	h.Status = b[9]
	h.Signature = binary.LittleEndian.Uint16(b[10:12])
	return h, nil
}

// ParseVariableData 解析 CI=72h 的数据域（不含 CI 自身）。
func ParseVariableData(b []byte) (VariableData, error) {
	h, err := parseHeader(b)
	if err != nil {
		return VariableData{}, err
	}
	out := VariableData{Header: h}
	i := 12
	for i < len(b) {
		// 2Fh 是空闲填充：报文按 16 字节对齐时补的，不是记录。
		if b[i] == 0x2F {
			i++
			continue
		}
		rec, n, err := parseRecord(b[i:], len(out.Records))
		if err != nil {
			return out, err
		}
		if rec.Kind == KindBinary && rec.DIF == 0x0F {
			// 厂商特定记录：其后整段都属于厂商，按字节交出去。
			out.VendorData = rec.Raw
		}
		if rec.DIF == 0x1F {
			out.MoreRecordsFollow = true
			out.VendorData = rec.Raw
			out.Records = append(out.Records, rec)
			return out, nil
		}
		out.Records = append(out.Records, rec)
		i += n
	}
	return out, nil
}

// parseRecord 解析一条记录，返回记录与消耗的字节数。
func parseRecord(b []byte, index int) (Record, int, error) {
	if len(b) == 0 {
		return Record{}, 0, ErrIncomplete
	}
	rec := Record{Index: index, DIF: b[0]}
	rec.DataField = b[0] & 0x0F
	rec.Function = (b[0] & 0x30) >> 4

	// 厂商特定 / 更多记录：其后字节没有标准结构，交给调用方。
	if b[0] == 0x0F || b[0] == 0x1F {
		rec.Kind = KindBinary
		rec.Quantity = "vendor-specific"
		rec.Scale = 1
		rec.Raw = append([]byte(nil), b[1:]...)
		return rec, len(b), nil
	}

	i := 1
	// DIFE 链：每字节 bit7 表示"后面还有"。DIFE 只有出现在 DIF 的
	// 扩展位为 1 时才有，不能靠"下一个字节长得像 DIFE"去猜。
	if b[0]&0x80 != 0 {
		for {
			if i >= len(b) {
				return rec, 0, fmt.Errorf("%w: DIF %02X 声明有 DIFE，但报文到头了", ErrFrame, b[0])
			}
			dife := b[i]
			i++
			rec.DIFEs = append(rec.DIFEs, dife)
			if dife&0x80 == 0 {
				break
			}
			if len(rec.DIFEs) > 10 {
				return rec, 0, fmt.Errorf("%w: DIFE 超过 10 个", ErrFrame)
			}
		}
	}
	rec.Storage = uint32((b[0] & 0x40) >> 6)
	for n, dife := range rec.DIFEs {
		rec.Storage |= uint32(dife&0x0F) << uint(1+4*n)
		rec.Tariff |= uint16((dife&0x30)>>4) << uint(2*n)
		rec.Subunit |= ((dife & 0x40) >> 6) << uint(n)
	}

	if i >= len(b) {
		return rec, 0, fmt.Errorf("%w: 记录在 VIF 之前结束", ErrFrame)
	}
	rec.VIF = b[i]
	i++

	base := rec.VIF & 0x7F
	if base == 0x7C {
		// 自定义 ASCII 量名：长度 + 文本。
		if i >= len(b) {
			return rec, 0, fmt.Errorf("%w: 自定义 VIF 缺少长度字节", ErrFrame)
		}
		n := int(b[i])
		i++
		if i+n > len(b) {
			return rec, 0, fmt.Errorf("%w: 自定义 VIF 声明 %d 字节，超出报文", ErrFrame, n)
		}
		rec.CustomVIF = string(b[i : i+n])
		i += n
	}

	// VIFE 链。
	if rec.VIF&0x80 != 0 {
		for {
			if i >= len(b) {
				return rec, 0, fmt.Errorf("%w: VIF %02X 声明有 VIFE，但报文到头了", ErrFrame, rec.VIF)
			}
			vife := b[i]
			i++
			rec.VIFEs = append(rec.VIFEs, vife)
			if vife&0x80 == 0 {
				break
			}
			if len(rec.VIFEs) > 10 {
				return rec, 0, fmt.Errorf("%w: VIFE 超过 10 个", ErrFrame)
			}
		}
	}

	info := LookupVIF(rec.VIF, rec.VIFEs)
	rec.Quantity = info.Quantity
	rec.Unit = info.Unit
	rec.Scale = info.Scale
	rec.Special = info.Special
	if rec.CustomVIF != "" {
		// 自定义量名优先：它比 "custom" 更能说明这是什么。
		rec.Quantity = NormalizeQuantity(rec.CustomVIF)
	}
	rec.Quantity += FunctionSuffix(rec.Function)

	// 数据域长度。
	dataLen, err := difDataLen(rec.DataField)
	if err != nil {
		return rec, 0, err
	}
	if i+dataLen > len(b) {
		return rec, 0, fmt.Errorf("%w: DIF %02X 声明 %d 字节数据，只剩 %d 字节",
			ErrFrame, rec.DIF, dataLen, len(b)-i)
	}
	rec.Raw = append([]byte(nil), b[i:i+dataLen]...)
	i += dataLen

	// 变长记录（DIF 低 4 位 = Dh）：数据域首字节是 LVAR。
	//
	// difDataLen 对 Dh 返回 0，所以上面切出来的 Raw 是空的、i 仍停在
	// LVAR 上。LVAR 本身保留在 Raw 里：它是这条记录数据域的一部分，
	// `r:` 原始字节导出时不该把它吞掉。
	if rec.DataField == 0x0D {
		if i >= len(b) {
			return rec, 0, fmt.Errorf("%w: 变长记录缺少 LVAR 字节", ErrFrame)
		}
		lvar := b[i]
		n, ok := lvarLen(lvar)
		if !ok {
			return rec, 0, fmt.Errorf("%w: 无法识别的 LVAR %02X", ErrFrame, lvar)
		}
		if i+1+n > len(b) {
			return rec, 0, fmt.Errorf("%w: LVAR %02X 声明 %d 字节，只剩 %d 字节",
				ErrFrame, lvar, n, len(b)-i-1)
		}
		// Raw 含 LVAR（供 `r:` 导出）；payload 是去掉 LVAR 后的数据域。
		payload := append([]byte(nil), b[i+1:i+1+n]...)
		rec.Raw = append([]byte(nil), b[i:i+1+n]...)
		i += 1 + n
		// LVAR 的形态决定数据怎么解释（EN 13757-3:2018/2025 Table 5）：
		//   ≤BFh    ASCII 文本
		//   C0..C9  正 BCD 数   D0..D9  负 BCD 数
		//   E0..EF  二进制数    F0..F6  16..64 字节二进制数（超出 int64，按原始字节暴露）
		rec.Kind, rec.Num, rec.RawInt, rec.Text = decodeVariableValue(rec, lvar, payload)
		return rec, i, nil
	}

	rec.Kind, rec.Num, rec.RawInt, rec.Text = decodeRecordValue(rec)
	return rec, i, nil
}

// difDataLen 返回 DIF 低 4 位对应的数据域字节数。
//
// 0x0D（变长）在这里返回 0：它的长度在数据域首字节里，必须先读到
// LVAR 才能继续，不能在这一层算。
func difDataLen(field byte) (int, error) {
	switch field {
	case 0x00, 0x08: // 无数据 / 选择下一记录
		return 0, nil
	case 0x01, 0x09:
		return 1, nil
	case 0x02, 0x0A:
		return 2, nil
	case 0x03, 0x0B:
		return 3, nil
	case 0x04, 0x05, 0x0C:
		return 4, nil
	case 0x06, 0x0E:
		return 6, nil
	case 0x07:
		return 8, nil
	case 0x0D:
		return 0, nil
	case 0x0F:
		return 0, nil
	}
	return 0, fmt.Errorf("%w: DIF 长度编码 %X 无效", ErrFrame, field)
}

// lvarLen 解 LVAR 的长度编码（EN 13757-3:2018/2025 §6.3.3 Table 5）。
//
//	00h..BFh : 8 位文本（ISO/IEC 8859-1），长度 = LVAR 字节
//	C0h..C9h : 正 BCD 数，(LVAR-C0h)*2 位十进制 → 字节数 = LVAR-C0h
//	D0h..D9h : 负 BCD 数，(LVAR-D0h)*2 位十进制 → 字节数 = LVAR-D0h
//	E0h..EFh : 二进制数，字节数 = LVAR-E0h（0..15 字节）
//	F0h..F4h : 二进制数，字节数 = 4*(LVAR-ECh)（16/20/24/28/32 字节）
//	F5h      : 二进制数，48 字节
//	F6h      : 二进制数，64 字节
//	其余     : 保留
//
// 注意 F 段不是「浮点」：2018/2025 版把它们统称 Binary number，只是宽度
// 超过 int64。早期 1990 年代文档写过 F0..FA 是 (LVAR-F0) 字节的浮点
// "to be defined"，但标准化后已改成 4 的倍数 / 48 / 64；按旧文档解会让
// 合法的 16..64 字节记录被截断、把保留编码 F7h..FAh 当合法长度。
func lvarLen(lvar byte) (int, bool) {
	switch {
	case lvar <= 0xBF:
		return int(lvar), true
	case lvar >= 0xC0 && lvar <= 0xC9:
		return int(lvar - 0xC0), true
	case lvar >= 0xD0 && lvar <= 0xD9:
		return int(lvar - 0xD0), true
	case lvar >= 0xE0 && lvar <= 0xEF:
		return int(lvar - 0xE0), true
	case lvar >= 0xF0 && lvar <= 0xF4:
		return 4 * int(lvar-0xEC), true
	case lvar == 0xF5:
		return 48, true
	case lvar == 0xF6:
		return 64, true
	}
	return 0, false
}

// decodeRecordValue 按 DIF 长度编码解释数据域。
func decodeRecordValue(rec Record) (Kind, float64, int64, string) {
	if rec.Special == SpecialDate {
		if t, ok := decodeDate(rec.Raw); ok {
			return KindText, 0, 0, t.Format("2006-01-02")
		}
		return KindText, 0, 0, ""
	}
	if rec.Special == SpecialDateTime {
		if t, ok := decodeDateTime(rec.Raw); ok {
			return KindText, 0, 0, t.Format(time.RFC3339)
		}
		return KindText, 0, 0, ""
	}
	if rec.Special == SpecialManufacturer {
		return KindBinary, 0, 0, ""
	}

	switch rec.DataField {
	case 0x00, 0x08:
		return KindNone, 0, 0, ""
	case 0x01, 0x02, 0x03, 0x04, 0x06, 0x07:
		v, ok := decodeIntLE(rec.Raw)
		if !ok {
			return KindBinary, 0, 0, ""
		}
		return KindNumeric, float64(v) * rec.Scale, v, ""
	case 0x05:
		v, ok := decodeReal(rec.Raw)
		if !ok {
			return KindBinary, 0, 0, ""
		}
		// REAL 已经是工程值，不再乘 Scale。M-Bus 里 REAL 只用于
		// 极少数场景，而标准也规定它不带 10 的幂。
		return KindNumeric, v, 0, ""
	case 0x09, 0x0A, 0x0B, 0x0C, 0x0E:
		v, ok := decodeBCD(rec.Raw)
		if !ok {
			return KindBinary, 0, 0, ""
		}
		return KindNumeric, float64(v) * rec.Scale, v, ""
	case 0x0F:
		return KindBinary, 0, 0, ""
	}
	return KindBinary, 0, 0, ""
}

// decodeVariableValue 按 LVAR 的形态解释变长记录的数据域（payload 已去掉 LVAR 字节）。
//
// EN 13757-3:2018/2025 §6.3.3 用 LVAR 同时编码长度与形态：≤BFh 是 8 位文本，
// C/D 段是正/负 BCD 数，E 段是 0..15 字节二进制数，F 段是 16/20/24/28/32/48/64
// 字节二进制数。后两段的宽度超过 int64，按原始字节暴露、不静默截断。
//
// 符号约定与固定宽度 BCD 不同：固定宽度（DIF 1001..1110）的符号在末字节高半
// 字节的 Fh；变长的符号由 C/D 段给出，数据字节本身必须是纯数字。因此这里用
// decodeBCDDigits（不接受 Fh 符号位）而不是 decodeBCD——否则一条畸形 D 段记录
// 会被 decodeBCD 当成"末字节带负号"再被 D 段取负，负负得正、静默出错。
func decodeVariableValue(rec Record, lvar byte, payload []byte) (Kind, float64, int64, string) {
	switch {
	case lvar <= 0xBF:
		return KindText, 0, 0, strings.TrimRight(string(payload), "\x00")
	case lvar >= 0xC0 && lvar <= 0xC9:
		v, ok := decodeBCDDigits(payload)
		if !ok {
			return KindBinary, 0, 0, ""
		}
		return KindNumeric, float64(v) * rec.Scale, v, ""
	case lvar >= 0xD0 && lvar <= 0xD9:
		v, ok := decodeBCDDigits(payload)
		if !ok {
			return KindBinary, 0, 0, ""
		}
		return KindNumeric, float64(-v) * rec.Scale, -v, ""
	case lvar >= 0xE0 && lvar <= 0xEF:
		v, ok := decodeIntLE(payload)
		if !ok {
			// 宽度装不进 int64（5/7/9..15 字节）：按原始字节暴露，不做静默截断。
			return KindBinary, 0, 0, ""
		}
		return KindNumeric, float64(v) * rec.Scale, v, ""
	default: // 0xF0..0xF6：16..64 字节二进制数，必然超出 int64，保持原始字节
		return KindBinary, 0, 0, ""
	}
}

// ---------------------------------------------------------------------------
// 固定数据结构
// ---------------------------------------------------------------------------

// FixedData 是一条 CI=73h 应答解出的内容。
//
// 固定结构没有 DIF/VIF：两个计数器的单位写在各自的「type」字节里，
// 值的格式（BCD 还是二进制）写在状态字节的最高位。因此它天然只能表达
// 「一两个累计量」，没有瞬时量、没有历史记录——这正是它被淘汰的原因。
type FixedData struct {
	Header Header
	// Counters 长度恒为 2。无法解释的计数器 Kind 为 KindBinary。
	Counters [2]Record
	// Stored 为真表示计数器是"上次读数时"的历史值（状态 bit6）。
	Stored bool
	// BCD 为真表示计数器按 BCD 编码（状态 bit7 = 0）。
	BCD bool
}

// ParseFixedData 解析 CI=73h 的 16 字节数据域。
func ParseFixedData(b []byte) (FixedData, error) {
	if len(b) != 16 {
		return FixedData{}, fmt.Errorf("%w: 固定数据结构长度是 %d，应为 16", ErrFrame, len(b))
	}
	var out FixedData
	out.Header.ID = binary.LittleEndian.Uint32(b[0:4])
	out.Header.IDText = BCDString(b[0:4])
	out.Header.AccessNo = b[4]
	out.Header.Status = b[5]
	out.BCD = b[5]&0x80 == 0
	out.Stored = b[5]&0x40 != 0

	types := [2]byte{b[6], b[7]}
	vals := [2][]byte{b[8:12], b[12:16]}
	for i := 0; i < 2; i++ {
		rec := Record{Index: i, DataField: 0x04, Raw: append([]byte(nil), vals[i]...)}
		quantity, unit, scale, special := fixedUnit(types[i])
		rec.Quantity = quantity
		rec.Unit = unit
		rec.Scale = scale
		rec.Special = special
		if out.BCD {
			v, ok := decodeBCD(rec.Raw)
			if !ok {
				rec.Kind = KindBinary
			} else {
				rec.Kind = KindNumeric
				rec.RawInt = v
				rec.Num = float64(v) * scale
			}
		} else {
			v, _ := decodeIntLE(rec.Raw)
			rec.Kind = KindNumeric
			rec.RawInt = v
			rec.Num = float64(v) * scale
		}
		out.Counters[i] = rec
	}
	// 两个计数器的物理量同名时给第二个加后缀，保证 `q:` 寻址不歧义。
	if out.Counters[0].Quantity == out.Counters[1].Quantity {
		out.Counters[1].Quantity += "-2"
	}
	return out, nil
}

// fixedUnit 解固定结构的 type 字节：低 6 位是单位编码，bit6 表示历史值，
// bit7 表示按整数而非 BCD 编码（后者在上层处理）。
func fixedUnit(t byte) (quantity, unit string, scale float64, special Special) {
	code := t & 0x3F
	switch {
	case code == 0x00:
		return "time", "s", 1, SpecialNone
	case code == 0x01:
		return "date", "", 1, SpecialDate
	case code >= 0x02 && code <= 0x0A:
		return "energy", "Wh", Pow10(int(code - 0x02)), SpecialNone
	case code >= 0x0B && code <= 0x13:
		// 0x0B..0x13 是 J，指数 1e3..1e11 单调递增。
		//
		// 这一段容易被记成「有跳变」（比如以为 0Dh 是 MJ）：实际上标准
		// 与现场报文都是单调的，按 10 的幂推即可。写错一级就是错 10 倍，
		// 而 1e5 与 1e6 的差别在一块热表上就是「差了一个月用量」。
		return "energy", "J", Pow10(int(code-0x0B) + 3), SpecialNone
	case code >= 0x14 && code <= 0x1C:
		return "power", "W", Pow10(int(code - 0x14)), SpecialNone
	case code >= 0x1D && code <= 0x25:
		return "power", "J/h", Pow10(int(code-0x1D) + 3), SpecialNone
	case code >= 0x26 && code <= 0x2E:
		return "volume", "m^3", Pow10(int(code-0x26) - 6), SpecialNone
	case code >= 0x2F && code <= 0x37:
		return "volume-flow", "m^3/h", Pow10(int(code-0x2F) - 6), SpecialNone
	case code == 0x38:
		return "flow-temperature", "°C", 1e-3, SpecialNone
	case code == 0x39:
		return "hca", "H.C.A.", 1, SpecialNone
	case code == 0x3F:
		return "counter", "", 1, SpecialNone
	}
	return fmt.Sprintf("counter-%02x", code), "", 1, SpecialNone
}

// FindRecord 按 `q:` 地址在记录里选一条。
//
// 规则（按优先级，命中即返回）：
//
//	q:volume               第 1 条 quantity=volume、storage=0 的记录
//	q:volume/2             第 2 条同类记录（同一物理量常见于多费率/多存储号）
//	q:volume@3             storage=3 的 volume（历史第 3 期）
//	q:volume@3/2           上面这组里的第 2 条
//
// 返回 ok=false 时，err 里会给出候选列表——地址写错时最需要知道的是
// "这张表到底有什么"，而不是一句"找不到"。
func FindRecord(recs []Record, expr string) (Record, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Record{}, false, fmt.Errorf("空记录地址")
	}
	// `r:<n>`：按报文顺序取第 n 条，语义无关。给厂商自定义 VIF 留的
	// 逃生口——名字不认识时仍然要能取数。
	if strings.HasPrefix(expr, "r:") {
		n, err := parseNonNeg(expr[2:])
		if err != nil {
			return Record{}, false, fmt.Errorf("记录序号 %q 不是非负整数", expr[2:])
		}
		if n >= len(recs) {
			return Record{}, false, fmt.Errorf("记录序号 %d 越界（共 %d 条）", n, len(recs))
		}
		return recs[n], true, nil
	}

	name, storage, nth := splitQuantityExpr(expr)
	name = NormalizeQuantity(name)
	if name == "" {
		return Record{}, false, fmt.Errorf("记录地址 %q 缺少物理量名", expr)
	}
	seen := 0
	for _, r := range recs {
		if NormalizeQuantity(r.Quantity) != name {
			continue
		}
		if storage >= 0 && int(r.Storage) != storage {
			continue
		}
		if seen == nth {
			return r, true, nil
		}
		seen++
	}
	return Record{}, false, fmt.Errorf("报文里没有 %q（有：%s）", expr, RecordNames(recs))
}

// splitQuantityExpr 把 `name[@storage][/nth]` 拆成三部分。
// storage 返回 -1 表示"不限存储号"，nth 返回 0 表示"同组第一条"。
func splitQuantityExpr(expr string) (name string, storage, nth int) {
	storage, nth = -1, 0
	if i := strings.LastIndexByte(expr, '/'); i >= 0 {
		if n, err := parseNonNeg(expr[i+1:]); err == nil {
			nth = n
			expr = expr[:i]
		}
	}
	if i := strings.LastIndexByte(expr, '@'); i >= 0 {
		if n, err := parseNonNeg(expr[i+1:]); err == nil {
			storage = n
			expr = expr[:i]
		}
	}
	return expr, storage, nth
}

func parseNonNeg(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("空")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(r-'0')
		if n > 1<<20 {
			return 0, fmt.Errorf("过大")
		}
	}
	return n, nil
}

// RecordNames 汇总报文中出现的全部 `q:` 地址形态，供报错与自检输出。
func RecordNames(recs []Record) string {
	if len(recs) == 0 {
		return "无记录"
	}
	// 同一 quantity 出现多次时逐个编号，让用户能直接抄地址。
	count := map[string]int{}
	parts := make([]string, 0, len(recs))
	for _, r := range recs {
		key := NormalizeQuantity(r.Quantity)
		n := count[key]
		count[key] = n + 1
		ref := "q:" + key
		if r.Storage > 0 {
			ref += fmt.Sprintf("@%d", r.Storage)
		}
		if n > 0 {
			ref += fmt.Sprintf("/%d", n)
		}
		if r.Unit != "" {
			ref += "(" + r.Unit + ")"
		}
		parts = append(parts, ref)
	}
	return strings.Join(parts, ", ")
}
