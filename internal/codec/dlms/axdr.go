package dlms

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// 本文件实现 A-XDR（Adapted eXtended Data Representation，IEC 61334-6）的
// 数据编码。它是 DLMS/COSEM 的「类型系统」——每一个值都自带一个标签字节，
// 说明「后面这串字节是什么类型、有多长」。
//
// # 为什么需要它
//
// 与寄存器型协议（Modbus、DL/T 645）最大的差别：DLMS 的值是**自描述**的。
// 点表不需要声明「这个点读 2 字节有符号整数」——Get-Response 里直接写着
// `0x10 0x01 0x2C`（long，值 300）。同一个 OBIS 在不同表上可能返回
// long-unsigned、double-long-unsigned，甚至 structure（例如 scaler_unit
// 是 `{scalar, unit}` 两元素结构）。
//
// 代价是：解码器必须完整实现类型表，否则碰到未实现的标签就整条链路卡住。
// 因此本文件按 IEC 62056-53 §8.6.4 的 `Data ::= CHOICE` 逐项实现，**不猜标签**。
//
// # 标签表（标准原文，不要凭直觉改）
//
//	0x00 null-data        0x11 unsigned (u8)
//	0x01 array            0x12 long-unsigned (u16)
//	0x02 structure        0x13 compact-array
//	0x03 boolean          0x14 long64 (i64)
//	0x04 bit-string       0x15 long64-unsigned (u64)
//	0x05 double-long (i32) 0x16 enum
//	0x06 double-long-unsigned (u32) 0x17 float32
//	0x09 octet-string     0x18 float64
//	0x0A visible-string   0x19 date_time (12 字节)
//	0x0D bcd              0x1A date (5 字节)
//	0x0F integer (i8)     0x1B time (4 字节)
//	0x10 long (i16)       0xFF don't-care
//
// 注意 0x07/0x08/0x0B/0x0C/0x0E 是**空号**：它们不是「保留待用」，而是历史上
// 从未分配。把它们当 flt32 / int16 之类的常见错误会让解码在第一个真实值上
// 就偏一字节，然后全盘错位。

// A-XDR 类型标签。
const (
	TagNull           byte = 0x00
	TagArray          byte = 0x01
	TagStructure      byte = 0x02
	TagBoolean        byte = 0x03
	TagBitString      byte = 0x04
	TagDoubleLong     byte = 0x05 // int32
	TagDoubleLongUns  byte = 0x06 // uint32
	TagOctetString    byte = 0x09
	TagVisibleString  byte = 0x0A
	TagBCD            byte = 0x0D
	TagInteger        byte = 0x0F // int8
	TagLong           byte = 0x10 // int16
	TagUnsigned       byte = 0x11 // uint8
	TagLongUnsigned   byte = 0x12 // uint16
	TagCompactArray   byte = 0x13
	TagLong64         byte = 0x14 // int64
	TagLong64Unsigned byte = 0x15 // uint64
	TagEnum           byte = 0x16
	TagFloat32        byte = 0x17
	TagFloat64        byte = 0x18
	TagDateTime       byte = 0x19
	TagDate           byte = 0x1A
	TagTime           byte = 0x1B
	TagDontCare       byte = 0xFF
)

// tagName 给出标签的可读名，用于错误信息与 CLI 输出。
func tagName(t byte) string {
	switch t {
	case TagNull:
		return "null-data"
	case TagArray:
		return "array"
	case TagStructure:
		return "structure"
	case TagBoolean:
		return "boolean"
	case TagBitString:
		return "bit-string"
	case TagDoubleLong:
		return "double-long"
	case TagDoubleLongUns:
		return "double-long-unsigned"
	case TagOctetString:
		return "octet-string"
	case TagVisibleString:
		return "visible-string"
	case TagBCD:
		return "bcd"
	case TagInteger:
		return "integer"
	case TagLong:
		return "long"
	case TagUnsigned:
		return "unsigned"
	case TagLongUnsigned:
		return "long-unsigned"
	case TagCompactArray:
		return "compact-array"
	case TagLong64:
		return "long64"
	case TagLong64Unsigned:
		return "long64-unsigned"
	case TagEnum:
		return "enum"
	case TagFloat32:
		return "float32"
	case TagFloat64:
		return "float64"
	case TagDateTime:
		return "date_time"
	case TagDate:
		return "date"
	case TagTime:
		return "time"
	case TagDontCare:
		return "don't-care"
	}
	return fmt.Sprintf("未知标签 0x%02X", t)
}

// Kind 是 A-XDR 值的种类，供调用方在不关心具体数值时也能判断形状。
type Kind int

const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindUint
	KindFloat
	KindBytes  // octet-string
	KindString // visible-string
	KindArray  // array / structure 统一视为序列
	KindBitStr
	KindDateTime
	KindBCD
)

// Value 是一个解码后的 A-XDR 值。
//
// 用一个结构体而不是 interface{}：DLMS 的值最终都要落到 model.Value（数值 /
// 布尔 / 文本三选一），用具体字段比类型断言更省事，也更容易在诊断输出里
// 打印原始形态（例如 octet-string 的十六进制）。
type Value struct {
	Tag byte
	// Int / Uint / Float 三者按 Tag 取其一。
	Int   int64
	Uint  uint64
	Float float64
	Bool  bool
	// Bytes 是 octet-string 的原始字节（也用于 bit-string 的尾字节）。
	Bytes []byte
	// Text 是 visible-string 的内容。
	Text string
	// Elems 是 array / structure 的元素。
	Elems []Value
	// Raw 是未解释的原始字节（date_time / bcd / bit-string），保留给诊断。
	Raw []byte
}

// Kind 返回值的种类。
func (v Value) Kind() Kind {
	switch v.Tag {
	case TagNull, TagDontCare:
		return KindNull
	case TagBoolean:
		return KindBool
	case TagInteger, TagLong, TagDoubleLong, TagLong64, TagEnum:
		return KindInt
	case TagUnsigned, TagLongUnsigned, TagDoubleLongUns, TagLong64Unsigned:
		return KindUint
	case TagFloat32, TagFloat64:
		return KindFloat
	case TagOctetString:
		return KindBytes
	case TagVisibleString:
		return KindString
	case TagArray, TagStructure, TagCompactArray:
		return KindArray
	case TagBitString:
		return KindBitStr
	case TagDateTime, TagDate, TagTime:
		return KindDateTime
	case TagBCD:
		return KindBCD
	}
	return KindNull
}

// Number 把数值型值统一取出为 float64。非数值返回 false。
//
// 布尔按 0/1 处理：DLMS 里 boolean 常见于继电器状态，把它当数值上报比
// 「类型不支持」更可用。
func (v Value) Number() (float64, bool) {
	switch v.Kind() {
	case KindBool:
		if v.Bool {
			return 1, true
		}
		return 0, true
	case KindInt:
		return float64(v.Int), true
	case KindUint:
		return float64(v.Uint), true
	case KindFloat:
		return v.Float, true
	}
	return 0, false
}

// String 输出值的可读形式，用于 CLI 与日志。
//
// structure / array 用括号展开，octet-string 用十六进制，bit-string 用二进制
// 加长度——这些都是现场核对点表时最需要看到的形态。
func (v Value) String() string {
	switch v.Kind() {
	case KindNull:
		return "null"
	case KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KindInt:
		return fmt.Sprintf("%d", v.Int)
	case KindUint:
		return fmt.Sprintf("%d", v.Uint)
	case KindFloat:
		return fmt.Sprintf("%g", v.Float)
	case KindBytes:
		return fmt.Sprintf("% X", v.Bytes)
	case KindString:
		return v.Text
	case KindBitStr:
		return fmt.Sprintf("bits(%d)=%b", len(v.Bytes)*8, v.Bytes)
	case KindArray:
		var sb strings.Builder
		sb.WriteString("{")
		for i, e := range v.Elems {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(e.String())
		}
		sb.WriteString("}")
		return sb.String()
	case KindDateTime:
		if t, ok := v.DateTime(); ok {
			return t.Format("2006-01-02T15:04:05")
		}
		return fmt.Sprintf("% X", v.Raw)
	case KindBCD:
		return fmt.Sprintf("bcd(%d)", v.Int)
	}
	return fmt.Sprintf("% X", v.Raw)
}

// DateTime 尝试把 date_time / date 解成 time.Time。
//
// # date_time 的 12 字节布局（IEC 62056-62 §4.1.6.1）
//
//	octet 1..2  年（大端；0xFFFF = 未指定）
//	octet 3     月（0xFF = 未指定）
//	octet 4     日
//	octet 5     星期（1=周一 … 7=周日；0xFF = 未指定）
//	octet 6     时
//	octet 7     分
//	octet 8     秒
//	octet 9     百分秒
//	octet 10..11 与 UTC 的偏差（带符号分钟，0x8000 = 未指定）
//	octet 12    时钟状态
//
// **星期在日月之后、时分之前**，这个位置特别容易记错：按"年 月 日 时 分 秒"
// 的直觉写，会把日当成月、把星期当成小时。症状是时钟要么解不出来（月份
// 越界），要么显示成一个月后的某天。
//
// 未指定的字段（0xFF）按标准的语义处理：缺年/月/日就无法构成一个时间点，
// 返回 false 让调用方保留原始字节。
func (v Value) DateTime() (time.Time, bool) {
	switch v.Tag {
	case TagDateTime:
		if len(v.Raw) != 12 {
			return time.Time{}, false
		}
		b := v.Raw
		if b[0] == 0xFF || b[1] == 0xFF || b[2] == 0xFF || b[3] == 0xFF {
			return time.Time{}, false // 年/月/日未指定，构不成时间点
		}
		year := int(binary.BigEndian.Uint16(b[0:2]))
		month := int(b[2])
		day := int(b[3])
		hour := int(b[5])
		min := int(b[6])
		sec := int(b[7])
		if year < 1 || month < 1 || month > 12 || day < 1 || day > 31 {
			return time.Time{}, false
		}
		loc := time.UTC
		// 时区偏移是带符号的分钟数；0x8000 表示未指定。
		if off := int16(binary.BigEndian.Uint16(b[9:11])); off != -32768 && off >= -720 && off <= 720 {
			loc = time.FixedZone("", int(off)*60)
		}
		return time.Date(year, time.Month(month), day, hour, min, sec, 0, loc), true
	case TagDate:
		if len(v.Raw) != 5 {
			return time.Time{}, false
		}
		b := v.Raw
		if b[0] == 0xFF || b[1] == 0xFF || b[2] == 0xFF || b[3] == 0xFF {
			return time.Time{}, false
		}
		year := int(binary.BigEndian.Uint16(b[0:2]))
		month := int(b[2])
		day := int(b[3])
		if month < 1 || month > 12 || day < 1 || day > 31 {
			return time.Time{}, false
		}
		return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), true
	case TagTime:
		// time 是 4 字节：时 分 秒 百分秒，没有日期，构不成 time.Time。
		return time.Time{}, false
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// 解码
// ---------------------------------------------------------------------------

// Decode 解一个 A-XDR 值，返回值与消耗的字节数。
//
// 递归实现——array / structure 内部又是 Data，所以「一层的长度」必须由
// 元素自描述推出，不需要外层长度前缀。这也是 compact-array 与普通 array
// 的差别：前者把类型描述与内容分开，后者每个元素自带标签。
func Decode(b []byte) (Value, int, error) {
	if len(b) == 0 {
		return Value{}, 0, ErrIncomplete
	}
	tag := b[0]
	rest := b[1:]
	switch tag {
	case TagNull, TagDontCare:
		return Value{Tag: tag}, 1, nil

	case TagBoolean:
		if len(rest) < 1 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Bool: rest[0] != 0}, 2, nil

	case TagInteger, TagEnum, TagBCD:
		if len(rest) < 1 {
			return Value{}, 0, ErrIncomplete
		}
		// enum / bcd 都是 1 字节整数的隐式编码（Data ::= CHOICE 里
		// enum [22] IMPLICIT ENUMERATED，bcd [13] IMPLICIT Integer8）。
		v := Value{Tag: tag, Int: int64(int8(rest[0]))}
		if tag == TagBCD {
			v.Raw = []byte{rest[0]}
		}
		return v, 2, nil

	case TagUnsigned:
		if len(rest) < 1 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Uint: uint64(rest[0])}, 2, nil

	case TagLong:
		if len(rest) < 2 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Int: int64(int16(binary.BigEndian.Uint16(rest)))}, 3, nil

	case TagLongUnsigned:
		if len(rest) < 2 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Uint: uint64(binary.BigEndian.Uint16(rest))}, 3, nil

	case TagDoubleLong:
		if len(rest) < 4 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Int: int64(int32(binary.BigEndian.Uint32(rest)))}, 5, nil

	case TagDoubleLongUns:
		if len(rest) < 4 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Uint: uint64(binary.BigEndian.Uint32(rest))}, 5, nil

	case TagLong64:
		if len(rest) < 8 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Int: int64(binary.BigEndian.Uint64(rest))}, 9, nil

	case TagLong64Unsigned:
		if len(rest) < 8 {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Uint: binary.BigEndian.Uint64(rest)}, 9, nil

	case TagFloat32:
		if len(rest) < 4 {
			return Value{}, 0, ErrIncomplete
		}
		bits := binary.BigEndian.Uint32(rest)
		return Value{Tag: tag, Float: float64(math.Float32frombits(bits))}, 5, nil

	case TagFloat64:
		if len(rest) < 8 {
			return Value{}, 0, ErrIncomplete
		}
		bits := binary.BigEndian.Uint64(rest)
		return Value{Tag: tag, Float: math.Float64frombits(bits)}, 9, nil

	case TagOctetString, TagVisibleString:
		if len(rest) < 1 {
			return Value{}, 0, ErrIncomplete
		}
		n := int(rest[0])
		// A-XDR 的长度是**单字节 0..255**（IEC 61334-6；octet-string 在 Data
		// CHOICE 里没有 SIZE 约束，因此 0x80..0xFF 是合法长度 128..255，**不是**
		// BER 的长形式前缀——A-XDR 根本没有 0x81/0x82 这种长度编码，见
		// docs/03-product/04-driver-sdk-spec.md §3.10.26.4）。所以这里
		// 不能对 rest[0]>=0x80 报错：那会把一台真机发来的 200 字节字符串当非法。
		if 1+n > len(rest) {
			// 长度字节声称的字节数超过了当前缓冲里实际剩下的。顶层的完整帧里
			// 这确实是「没收全」，返回 ErrIncomplete 让 Reader 继续等；但在递归
			// （array/structure 元素、Get-Data 值）里，子切片拿不到「还会来更多
			// 字节」的信息，这个超额主张其实意味着**帧被串位了**（OBIS/类号/编码
			// 与点表不符，或线路噪声）。两种成因都该显式失败——绝不把半截串当
			// 合法值返回——这里用 ErrAXDR 把「长度超过可用字节」与「字节不够读」
			// 区分开，让现场诊断直接指向串位而不是「等更多字节」。
			return Value{}, 0, fmt.Errorf("%w: %s 长度 %d 超过可用字节 %d（疑似帧串位）", ErrAXDR, tagName(tag), n, len(rest)-1)
		}
		body := rest[1 : 1+n]
		if tag == TagOctetString {
			return Value{Tag: tag, Bytes: append([]byte(nil), body...)}, 2 + n, nil
		}
		return Value{Tag: tag, Text: string(body)}, 2 + n, nil

	case TagBitString:
		// 长度以 bit 计，后面才是字节。末字节可能有多余的填充位。
		if len(rest) < 1 {
			return Value{}, 0, ErrIncomplete
		}
		bits := int(rest[0])
		n := (bits + 7) / 8
		if 1+n > len(rest) {
			// 与 octet/visible-string 同理：bit-string 的长度主张超过可用字节时，
			// 在递归语境下是帧串位而非「没收全」，用 ErrAXDR 显式区分。
			return Value{}, 0, fmt.Errorf("%w: bit-string 长度 %d 位(%d 字节)超过可用字节 %d（疑似帧串位）", ErrAXDR, bits, n, len(rest)-1)
		}
		return Value{Tag: tag, Bytes: append([]byte(nil), rest[1:1+n]...), Int: int64(bits)}, 2 + n, nil

	case TagDateTime, TagDate, TagTime:
		size := map[byte]int{TagDateTime: 12, TagDate: 5, TagTime: 4}[tag]
		if len(rest) < size {
			return Value{}, 0, ErrIncomplete
		}
		return Value{Tag: tag, Raw: append([]byte(nil), rest[:size]...)}, 1 + size, nil

	case TagArray, TagStructure:
		// array / structure 后面是元素个数（1 字节），再逐个元素（各自带标签）。
		if len(rest) < 1 {
			return Value{}, 0, ErrIncomplete
		}
		n := int(rest[0])
		pos := 1
		elems := make([]Value, 0, n)
		for i := 0; i < n; i++ {
			ev, used, err := Decode(rest[pos:])
			if err != nil {
				return Value{}, 0, err
			}
			elems = append(elems, ev)
			pos += used
		}
		return Value{Tag: tag, Elems: elems}, 1 + pos, nil

	case TagCompactArray:
		// compact-array 是「类型描述 + 原始字节串」，本实现只读内容不解类型
		// 描述（类型描述本身是 TypeDescription CHOICE，属于另一套递归结构）。
		// 遇到时保留原始字节，让调用方决定怎么解释，而不是猜。
		if len(rest) < 2 {
			return Value{}, 0, ErrIncomplete
		}
		// 跳过类型描述：这里只处理最常见的「0x12 long-unsigned」单元素描述，
		// 其余形态原样保留。真正需要 compact-array 的场景（大块历史数据）
		// 不在本驱动范围内。
		return Value{Tag: tag, Raw: append([]byte(nil), rest...)}, 1 + len(rest), nil
	}
	return Value{}, 0, fmt.Errorf("%w: %s", ErrAXDR, tagName(tag))
}

// ---------------------------------------------------------------------------
// 编码
// ---------------------------------------------------------------------------

// Encode 输出一个 A-XDR 值。
func (v Value) Encode() ([]byte, error) {
	switch v.Tag {
	case TagNull, TagDontCare:
		return []byte{v.Tag}, nil
	case TagBoolean:
		b := byte(0)
		if v.Bool {
			b = 1
		}
		return []byte{TagBoolean, b}, nil
	case TagInteger, TagEnum, TagBCD:
		return []byte{v.Tag, byte(int8(v.Int))}, nil
	case TagUnsigned:
		return []byte{TagUnsigned, byte(v.Uint)}, nil
	case TagLong:
		out := []byte{TagLong, 0, 0}
		binary.BigEndian.PutUint16(out[1:], uint16(int16(v.Int)))
		return out, nil
	case TagLongUnsigned:
		out := []byte{TagLongUnsigned, 0, 0}
		binary.BigEndian.PutUint16(out[1:], uint16(v.Uint))
		return out, nil
	case TagDoubleLong:
		out := []byte{TagDoubleLong, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], uint32(int32(v.Int)))
		return out, nil
	case TagDoubleLongUns:
		out := []byte{TagDoubleLongUns, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], uint32(v.Uint))
		return out, nil
	case TagLong64:
		out := []byte{TagLong64, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(out[1:], uint64(v.Int))
		return out, nil
	case TagLong64Unsigned:
		out := []byte{TagLong64Unsigned, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(out[1:], v.Uint)
		return out, nil
	case TagFloat32:
		out := []byte{TagFloat32, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], math.Float32bits(float32(v.Float)))
		return out, nil
	case TagFloat64:
		out := []byte{TagFloat64, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(out[1:], math.Float64bits(v.Float))
		return out, nil
	case TagOctetString:
		if len(v.Bytes) > 255 {
			return nil, fmt.Errorf("%w: octet-string 长度 %d 超过 255", ErrAXDR, len(v.Bytes))
		}
		out := append([]byte{TagOctetString, byte(len(v.Bytes))}, v.Bytes...)
		return out, nil
	case TagVisibleString:
		if len(v.Text) > 255 {
			return nil, fmt.Errorf("%w: visible-string 长度 %d 超过 255", ErrAXDR, len(v.Text))
		}
		out := append([]byte{TagVisibleString, byte(len(v.Text))}, v.Text...)
		return out, nil
	case TagBitString:
		bits := int(v.Int)
		if bits == 0 {
			bits = len(v.Bytes) * 8
		}
		out := append([]byte{TagBitString, byte(bits)}, v.Bytes...)
		return out, nil
	case TagDateTime, TagDate, TagTime:
		return append([]byte{v.Tag}, v.Raw...), nil
	case TagArray, TagStructure:
		if len(v.Elems) > 255 {
			return nil, fmt.Errorf("%w: 序列元素 %d 超过 255", ErrAXDR, len(v.Elems))
		}
		out := []byte{v.Tag, byte(len(v.Elems))}
		for _, e := range v.Elems {
			b, err := e.Encode()
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
		return out, nil
	case TagCompactArray:
		return append([]byte{TagCompactArray}, v.Raw...), nil
	}
	return nil, fmt.Errorf("%w: %s", ErrAXDR, tagName(v.Tag))
}

// ---------------------------------------------------------------------------
// 便捷构造：内置从站与测试用
// ---------------------------------------------------------------------------

func U8(v uint8) Value               { return Value{Tag: TagUnsigned, Uint: uint64(v)} }
func U16(v uint16) Value             { return Value{Tag: TagLongUnsigned, Uint: uint64(v)} }
func U32(v uint32) Value             { return Value{Tag: TagDoubleLongUns, Uint: uint64(v)} }
func U64(v uint64) Value             { return Value{Tag: TagLong64Unsigned, Uint: v} }
func I8(v int8) Value                { return Value{Tag: TagInteger, Int: int64(v)} }
func I16(v int16) Value              { return Value{Tag: TagLong, Int: int64(v)} }
func I32(v int32) Value              { return Value{Tag: TagDoubleLong, Int: int64(v)} }
func I64(v int64) Value              { return Value{Tag: TagLong64, Int: v} }
func Bool(v bool) Value              { return Value{Tag: TagBoolean, Bool: v} }
func Float32(v float32) Value        { return Value{Tag: TagFloat32, Float: float64(v)} }
func Float64(v float64) Value        { return Value{Tag: TagFloat64, Float: v} }
func Octets(v []byte) Value          { return Value{Tag: TagOctetString, Bytes: v} }
func Text(v string) Value            { return Value{Tag: TagVisibleString, Text: v} }
func Enum(v uint8) Value             { return Value{Tag: TagEnum, Int: int64(v)} }
func Structure(elems ...Value) Value { return Value{Tag: TagStructure, Elems: elems} }
func Array(elems ...Value) Value     { return Value{Tag: TagArray, Elems: elems} }
