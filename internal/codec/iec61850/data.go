package iec61850

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// 本文件实现 MMS 的 Data（值）与 TypeSpecification（类型）两种递归结构，
// 以及它们与平台 model.Value 之间的映射。
//
// 这两者是 MMS 里唯一必须递归的东西：一个 Data 可以是 structure / array，
// 里面再套 Data。IEC 61850 的「数据对象」到了线格式上就是一棵这样的树——
// 一个 `MMXU1$MX$TotW` 读回来通常是 `{mag:{f:...}, q:{...}, t:{...}}`。
// 驱动侧的点位要么声明取其中一个叶子（用 Component 路径），要么整棵树
// 按字符串呈现，绝不做「猜哪个字段是值」的事：猜错不会报错，只会让功率
// 点位读到品质位。

// MMS Data 的标签。
const (
	tagDataArray         = 0xA1
	tagDataStructure     = 0xA2
	tagDataBoolean       = 0x83
	tagDataBitString     = 0x84
	tagDataInteger       = 0x85
	tagDataUnsigned      = 0x86
	tagDataFloat         = 0x87
	tagDataOctetString   = 0x89
	tagDataVisibleString = 0x8A
	tagDataGeneralized   = 0x8B
	tagDataBinaryTime    = 0x8C
	tagDataBooleanArray  = 0x8E
	tagDataBCD           = 0x8D
	tagDataObjID         = 0x8F
	tagDataMMSString     = 0x90
	tagDataUTCTime       = 0x91
)

// MMS 数据访问错误的码值。
const (
	DataErrObjectInvalidated  = 0
	DataErrHardwareFault      = 1
	DataErrTemporarilyUnavail = 2
	DataErrAccessDenied       = 3
	DataErrObjectUndefined    = 4
	DataErrInvalidAddress     = 5
	DataErrTypeUnsupported    = 6
	DataErrTypeInconsistent   = 7
	DataErrAttrInconsistent   = 8
	DataErrAccessUnsupported  = 9
	DataErrObjectNonExistent  = 10
	DataErrObjectValueInvalid = 11
)

// DataErrorName 返回数据访问错误码的可读名。
func DataErrorName(code uint8) string {
	switch code {
	case DataErrObjectInvalidated:
		return "object-invalidated"
	case DataErrHardwareFault:
		return "hardware-fault"
	case DataErrTemporarilyUnavail:
		return "temporarily-unavailable"
	case DataErrAccessDenied:
		return "object-access-denied"
	case DataErrObjectUndefined:
		return "object-undefined"
	case DataErrInvalidAddress:
		return "invalid-address"
	case DataErrTypeUnsupported:
		return "type-unsupported"
	case DataErrTypeInconsistent:
		return "type-inconsistent"
	case DataErrAttrInconsistent:
		return "object-attribute-inconsistent"
	case DataErrAccessUnsupported:
		return "object-access-unsupported"
	case DataErrObjectNonExistent:
		return "object-non-existent"
	case DataErrObjectValueInvalid:
		return "object-value-invalid"
	}
	return fmt.Sprintf("code-%d", code)
}

// DataError 是 MMS 层返回的单项数据访问错误。
//
// 它实现 driver.Kinded：这些错误里有的是地址写错（对象不存在），有的是
// 权限问题（拒绝访问），有的是设备临时故障——把它们都压成一个「协议错误」
// 会让运维在「点位配错了」和「装置正在重启」之间无从分辨。
type DataError struct {
	Code uint8
	// Item 是出错的对象名，用于错误信息。
	Item string
}

func (e *DataError) Error() string {
	if e.Item != "" {
		return fmt.Sprintf("mms data access error %s (%d) on %s", DataErrorName(e.Code), e.Code, e.Item)
	}
	return fmt.Sprintf("mms data access error %s (%d)", DataErrorName(e.Code), e.Code)
}

// Data 是 MMS 的一个值。
//
// 用「标签 + 原始内容」而不是 Go 的 any 来承载：IEC 61850 里同一个数据对象
// 的线类型是固定的，驱动只需要把字节解释成平台值；一旦在解码时就折成 Go
// 类型，结构体的字段顺序、位串的填充位数这些信息就丢了，而写回时又必须
// 一模一样地还原。
type Data struct {
	// Tag 是线格式标签。
	Tag byte
	// Bool 用于布尔。
	Bool bool
	// Int 用于有符号整数。
	Int int64
	// Uint 用于无符号整数与位串数值。
	Uint uint64
	// Float 用于浮点。
	Float float64
	// FloatWidth 是浮点的位宽（32 或 64）。
	FloatWidth int
	// Bytes 用于位串、字节串、可见字符串与时间。
	Bytes []byte
	// Padding 是位串末尾的填充位数。
	Padding int
	// Elements 用于 structure 与 array。
	Elements []Data
	// Text 用于可见字符串与 MMS 字符串。
	Text string
}

// Encode 把 Data 编成 TLV。
func (d Data) Encode() ([]byte, error) {
	switch d.Tag {
	case tagDataBoolean:
		b := byte(0)
		if d.Bool {
			b = 0xFF
		}
		return appendTLV(nil, d.Tag, []byte{b}), nil
	case tagDataInteger:
		return appendTLV(nil, d.Tag, encodeInt(d.Int)), nil
	case tagDataUnsigned:
		return appendTLV(nil, d.Tag, encodeUnsigned(d.Uint)), nil
	case tagDataFloat:
		width := d.FloatWidth
		if width == 0 {
			width = 32
		}
		body, err := encodeFloat(d.Float, width)
		if err != nil {
			return nil, err
		}
		return appendTLV(nil, d.Tag, body), nil
	case tagDataBitString, tagDataBooleanArray, tagDataOctetString:
		if d.Tag == tagDataBitString || d.Tag == tagDataBooleanArray {
			return appendTLV(nil, d.Tag, encodeBitString(d.Bytes, d.Padding)), nil
		}
		return appendTLV(nil, d.Tag, d.Bytes), nil
	case tagDataVisibleString, tagDataMMSString, tagDataGeneralized:
		text := d.Text
		if text == "" && len(d.Bytes) > 0 {
			text = string(d.Bytes)
		}
		return appendTLV(nil, d.Tag, []byte(text)), nil
	case tagDataBinaryTime, tagDataUTCTime:
		return appendTLV(nil, d.Tag, d.Bytes), nil
	case tagDataArray, tagDataStructure:
		var inner []byte
		for _, e := range d.Elements {
			b, err := e.Encode()
			if err != nil {
				return nil, err
			}
			inner = append(inner, b...)
		}
		return appendTLV(nil, d.Tag, inner), nil
	}
	return nil, fmt.Errorf("iec61850: cannot encode mms data tag 0x%02X", d.Tag)
}

// DecodeData 从 b 的开头解析一个 Data，返回它与消耗的字节数。
func DecodeData(b []byte) (Data, int, error) {
	e, err := parseElement(b)
	if err != nil {
		return Data{}, 0, err
	}
	consumed := len(b) - len(e.rest)
	out, err := decodeDataElement(e)
	if err != nil {
		return Data{}, 0, err
	}
	return out, consumed, nil
}

func decodeDataElement(e element) (Data, error) {
	d := Data{Tag: e.tag}
	switch e.tag {
	case tagDataBoolean:
		if len(e.data) < 1 {
			return Data{}, errors.New("iec61850: empty boolean")
		}
		d.Bool = e.data[0] != 0
	case tagDataInteger:
		v, err := decodeInt(e.data)
		if err != nil {
			return Data{}, err
		}
		d.Int = v
	case tagDataUnsigned:
		v, err := decodeUnsigned(e.data)
		if err != nil {
			return Data{}, err
		}
		d.Uint = v
	case tagDataFloat:
		f, w, err := decodeFloat(e.data)
		if err != nil {
			return Data{}, err
		}
		d.Float, d.FloatWidth = f, w
	case tagDataBitString, tagDataBooleanArray:
		pad, bits, err := decodeBitString(e.data)
		if err != nil {
			return Data{}, err
		}
		d.Padding, d.Bytes = pad, append([]byte(nil), bits...)
	case tagDataOctetString, tagDataBinaryTime, tagDataUTCTime:
		d.Bytes = append([]byte(nil), e.data...)
	case tagDataVisibleString, tagDataMMSString, tagDataGeneralized:
		d.Text = string(e.data)
		d.Bytes = append([]byte(nil), e.data...)
	case tagDataBCD:
		d.Bytes = append([]byte(nil), e.data...)
	case tagDataObjID:
		d.Bytes = append([]byte(nil), e.data...)
	case tagDataArray, tagDataStructure:
		rest := e.data
		for len(rest) > 0 {
			child, n, err := DecodeData(rest)
			if err != nil {
				return Data{}, err
			}
			d.Elements = append(d.Elements, child)
			rest = rest[n:]
		}
	default:
		return Data{}, fmt.Errorf("iec61850: unsupported mms data tag 0x%02X", e.tag)
	}
	return d, nil
}

// KindName 返回 Data 的类型名，用于日志与自检输出。
func (d Data) KindName() string {
	switch d.Tag {
	case tagDataBoolean:
		return "boolean"
	case tagDataInteger:
		return "integer"
	case tagDataUnsigned:
		return "unsigned"
	case tagDataFloat:
		return fmt.Sprintf("float%d", d.FloatWidth)
	case tagDataBitString:
		return "bit-string"
	case tagDataBooleanArray:
		return "boolean-array"
	case tagDataOctetString:
		return "octet-string"
	case tagDataVisibleString:
		return "visible-string"
	case tagDataMMSString:
		return "mms-string"
	case tagDataGeneralized:
		return "generalized-time"
	case tagDataBinaryTime:
		return "binary-time"
	case tagDataUTCTime:
		return "utc-time"
	case tagDataArray:
		return "array"
	case tagDataStructure:
		return "structure"
	case tagDataBCD:
		return "bcd"
	case tagDataObjID:
		return "object-identifier"
	}
	return fmt.Sprintf("tag-0x%02X", d.Tag)
}

// String 把整棵树渲染成一行可读文本，例如
// `structure{float32=1.5, structure{boolean=true}}`。
func (d Data) String() string {
	var b strings.Builder
	d.writeString(&b)
	return b.String()
}

func (d Data) writeString(b *strings.Builder) {
	switch d.Tag {
	case tagDataArray, tagDataStructure:
		b.WriteString(d.KindName())
		b.WriteByte('{')
		for i, e := range d.Elements {
			if i > 0 {
				b.WriteString(", ")
			}
			e.writeString(b)
		}
		b.WriteByte('}')
	case tagDataFloat:
		b.WriteString(d.KindName())
		b.WriteByte('=')
		b.WriteString(strconv.FormatFloat(d.Float, 'g', -1, 64))
	case tagDataBoolean:
		b.WriteString(d.KindName())
		b.WriteByte('=')
		b.WriteString(strconv.FormatBool(d.Bool))
	case tagDataInteger:
		b.WriteString(d.KindName())
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(d.Int, 10))
	case tagDataUnsigned:
		b.WriteString(d.KindName())
		b.WriteByte('=')
		b.WriteString(strconv.FormatUint(d.Uint, 10))
	case tagDataVisibleString, tagDataMMSString, tagDataGeneralized:
		b.WriteString(d.KindName())
		b.WriteString("=\"")
		b.WriteString(d.Text)
		b.WriteByte('"')
	default:
		b.WriteString(d.KindName())
	}
}

// ---------------------------------------------------------------------------
// 与平台值的映射
// ---------------------------------------------------------------------------

// ToValue 把 Data 折成平台值。
//
// 只折「叶子」：布尔、整数、无符号、浮点、字符串、位串、时间。遇到
// structure/array 时按 todo 报错——折哪一层由调用方决定（Component 路径），
// 在这里默默取第一个元素是「看起来能跑、读出来的值却不对」的经典来源。
func (d Data) ToValue() (value any, err error) {
	switch d.Tag {
	case tagDataBoolean:
		return d.Bool, nil
	case tagDataInteger:
		return d.Int, nil
	case tagDataUnsigned:
		if d.Uint > math.MaxInt64 {
			// 平台侧没有 uint64 的精确承载（Value 用 float64），超出的值
			// 会丢精度。报错比悄悄截断安全。
			return nil, fmt.Errorf("iec61850: unsigned %d exceeds the 64-bit signed range", d.Uint)
		}
		return int64(d.Uint), nil
	case tagDataFloat:
		return d.Float, nil
	case tagDataVisibleString, tagDataMMSString:
		return d.Text, nil
	case tagDataBitString, tagDataBooleanArray:
		return bitStringValue(d), nil
	case tagDataOctetString:
		return d.Bytes, nil
	case tagDataBinaryTime:
		return binaryTimeValue(d.Bytes), nil
	case tagDataUTCTime:
		return utcTimeValue(d.Bytes), nil
	case tagDataStructure, tagDataArray:
		return nil, fmt.Errorf(
			"iec61850: the value is a %s with %d element(s); address one of its leaves (e.g. MMXU1$MX$TotW$mag$f)",
			d.KindName(), len(d.Elements))
	}
	return nil, fmt.Errorf("iec61850: unsupported mms data type %s", d.KindName())
}

// bitStringValue 把位串折成数值：最多 8 字节时按大端整数，否则按字节串。
//
// 位串在 IEC 61850 里主要用来承载品质（q）与各种按位定义的枚举。把它折成
// 整数能让「品质 != 0 就报警」这类规则写得出来；超过 64 位的位串没有数值
// 含义，退回字节串。
func bitStringValue(d Data) any {
	if len(d.Bytes) == 0 {
		return int64(0)
	}
	if len(d.Bytes) <= 8 {
		var v uint64
		for _, c := range d.Bytes {
			v = v<<8 | uint64(c)
		}
		// 末尾的填充位归零：不清掉会让品质值带上几位垃圾。
		if d.Padding > 0 && d.Padding < 8 {
			v >>= uint(d.Padding)
			v <<= uint(d.Padding)
		}
		return int64(v)
	}
	return d.Bytes
}

// binaryTimeValue 把 MMS 的二进制时间折成 Unix 毫秒。
//
// 4 字节形式是「1984-01-01 起的毫秒数」，6 字节形式在后面多两个字节的
// 天数。两者都要加 1984 这个纪元偏移。
func binaryTimeValue(b []byte) int64 {
	const epoch1984 = 441763200000 // 1984-01-01T00:00:00Z 的 Unix 毫秒
	if len(b) < 4 {
		return 0
	}
	ms := int64(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3])) & 0xFFFFFFFF
	if len(b) >= 6 {
		days := int64(uint16(b[4])<<8 | uint16(b[5]))
		ms += days * 86400000
	}
	return epoch1984 + ms
}

// utcTimeValue 把 8 字节的 UTC 时间折成 Unix 毫秒。
//
// 这不是简单的「自 1970 起的秒」：IEC 61850 的 UtcTime 是
// 4 字节秒 + 3 字节小数（2^-24 秒）+ 1 字节品质。品质字节在最后，
// 不同实现对它的定义不完全一致，这里只取时间部分。
func utcTimeValue(b []byte) int64 {
	if len(b) < 8 {
		return 0
	}
	secs := int64(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
	frac := int64(uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6]))
	// 小数部分共 24 位，换算成纳秒再降到毫秒。
	ms := secs*1000 + frac*1000/(1<<24)
	return ms
}

// ValueFromData 把 Data 转成平台值，并按 dt 归一到对应类型。
//
// 这一层刻意宽松：IEC 61850 的同一个数据对象在装置之间可能用 32 位或
// 64 位浮点、也可能把计数写成有符号或无符号整数，点表声明的 dataType 是
// 「平台侧存成什么」而不是「线上是什么」。只要数值装得下就接受。
func ValueFromData(d Data, dt string) (Value, error) {
	raw, err := d.ToValue()
	if err != nil {
		return Value{}, err
	}
	return coerceValue(raw, dt)
}

// Value 是平台侧的最小值载体（避免 codec 依赖 model 包）。
type Value struct {
	Type string
	Num  float64
	Text string
	Bool bool
}

// coerceValue 按平台类型折叠一个 Go 值。
func coerceValue(raw any, dt string) (Value, error) {
	switch dt {
	case "bool":
		switch v := raw.(type) {
		case bool:
			return Value{Type: "bool", Bool: v}, nil
		case int64:
			return Value{Type: "bool", Bool: v != 0}, nil
		}
		return Value{}, fmt.Errorf("iec61850: cannot read %T as bool", raw)
	case "string":
		switch v := raw.(type) {
		case string:
			return Value{Type: "string", Text: v}, nil
		case []byte:
			return Value{Type: "string", Text: string(v)}, nil
		case int64:
			return Value{Type: "string", Text: strconv.FormatInt(v, 10)}, nil
		case float64:
			return Value{Type: "string", Text: strconv.FormatFloat(v, 'g', -1, 64)}, nil
		case bool:
			return Value{Type: "string", Text: strconv.FormatBool(v)}, nil
		}
		return Value{}, fmt.Errorf("iec61850: cannot read %T as string", raw)
	case "bytes":
		switch v := raw.(type) {
		case []byte:
			return Value{Type: "bytes", Text: string(v)}, nil
		case string:
			return Value{Type: "bytes", Text: v}, nil
		}
		return Value{}, fmt.Errorf("iec61850: cannot read %T as bytes", raw)
	}
	// 其余都按数值处理。bool 转 0/1，字符串尝试解析——这两条在
	// 「点表把 string 写成了 float32」时能给出可用的值，而不是整点失败。
	var f float64
	switch v := raw.(type) {
	case int64:
		f = float64(v)
	case float64:
		f = v
	case bool:
		if v {
			f = 1
		}
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return Value{}, fmt.Errorf("iec61850: cannot read string %q as %s", v, dt)
		}
		f = parsed
	case []byte:
		return Value{}, fmt.Errorf("iec61850: cannot read %d bytes as %s", len(v), dt)
	default:
		return Value{}, fmt.Errorf("iec61850: cannot read %T as %s", raw, dt)
	}
	if err := checkRange(f, dt); err != nil {
		return Value{}, err
	}
	return Value{Type: dt, Num: f}, nil
}

// checkRange 检查数值是否落在平台类型能表示的范围内。
//
// 不静默截断：把一个超出 int16 的计数塞进 int16，现象是「读数在某个值上
// 突然变负」，那种问题现场能查一整天。
func checkRange(f float64, dt string) error {
	switch dt {
	case "int16":
		if f < math.MinInt16 || f > math.MaxInt16 {
			return fmt.Errorf("iec61850: value %v is out of the int16 range", f)
		}
	case "uint16":
		if f < 0 || f > math.MaxUint16 {
			return fmt.Errorf("iec61850: value %v is out of the uint16 range", f)
		}
	case "int32":
		if f < math.MinInt32 || f > math.MaxInt32 {
			return fmt.Errorf("iec61850: value %v is out of the int32 range", f)
		}
	case "uint32":
		if f < 0 || f > math.MaxUint32 {
			return fmt.Errorf("iec61850: value %v is out of the uint32 range", f)
		}
	case "int64":
		if f < math.MinInt64 || f > math.MaxInt64 {
			return fmt.Errorf("iec61850: value %v is out of the int64 range", f)
		}
	case "uint64":
		if f < 0 {
			return fmt.Errorf("iec61850: value %v is out of the uint64 range", f)
		}
	case "bcd16":
		if f < 0 || f > 9999 {
			return fmt.Errorf("iec61850: value %v is out of the bcd16 range", f)
		}
	case "bcd32":
		if f < 0 || f > 99999999 {
			return fmt.Errorf("iec61850: value %v is out of the bcd32 range", f)
		}
	}
	return nil
}

// DataFromValue 按目标类型把一个平台值编成 Data。
//
// 选择哪个 MMS 类型的规则：
//   - bool → boolean
//   - 整数类型 → integer（有符号）或 unsigned（无符号）
//   - 浮点 → 浮点，位宽按目标类型（float32 → 32 位，float64 → 64 位）
//   - 字符串 → visible-string
//   - 字节 → octet-string
//
// 不按「服务端声明的类型」反推：写一个已存在的对象时，服务端会用自己声明的
// 类型校验我们的值，类型不符会明确回 type-inconsistent。由客户端猜类型反而
// 容易在「float32 对象写了 64 位浮点」这种地方失败。
func DataFromValue(v Value) (Data, error) {
	switch v.Type {
	case "bool":
		return Data{Tag: tagDataBoolean, Bool: v.Bool}, nil
	case "string":
		return Data{Tag: tagDataVisibleString, Text: v.Text}, nil
	case "bytes":
		return Data{Tag: tagDataOctetString, Bytes: []byte(v.Text)}, nil
	case "int16", "int32", "int64":
		return Data{Tag: tagDataInteger, Int: int64(v.Num)}, nil
	case "uint16", "uint32", "uint64", "bcd16", "bcd32":
		return Data{Tag: tagDataUnsigned, Uint: uint64(v.Num)}, nil
	case "float32":
		return Data{Tag: tagDataFloat, Float: v.Num, FloatWidth: 32}, nil
	case "float64":
		return Data{Tag: tagDataFloat, Float: v.Num, FloatWidth: 64}, nil
	}
	return Data{}, fmt.Errorf("iec61850: unsupported platform type %q", v.Type)
}

// ---------------------------------------------------------------------------
// TypeSpecification
// ---------------------------------------------------------------------------

// TypeSpec 是 MMS 的类型声明。
//
// 只保留驱动实际用得到的区别：整数位宽、浮点格式、位串长度、结构和数组的
// 形状。抽象类型名（typeName）也支持，但只是记下来：驱动不会去解析装置私有
// 的类型定义。
type TypeSpec struct {
	// Kind 是 "boolean" / "bit-string" / "integer" / "unsigned" /
	// "float" / "octet-string" / "visible-string" / "mms-string" /
	// "utc-time" / "binary-time" / "array" / "structure" / "named"。
	Kind string
	// Bits 是整数/位串的位宽。负数表示可变长度。
	Bits int
	// FloatWidth 是浮点总位宽（32/64）。
	FloatWidth int
	// ExponentWidth 是浮点的指数位宽（8/11）。
	ExponentWidth int
	// Count 是数组元素个数。
	Count int
	// Elem 是数组元素类型。
	Elem *TypeSpec
	// Components 是结构体的成员类型。
	Components []TypeSpec
	// Name 是引用抽象类型时的类型名。
	Name string
}

// DecodeTypeSpec 解析一个 TypeSpecification。
func DecodeTypeSpec(b []byte) (TypeSpec, int, error) {
	e, err := parseElement(b)
	if err != nil {
		return TypeSpec{}, 0, err
	}
	consumed := len(b) - len(e.rest)
	t, err := decodeTypeSpecElement(e)
	if err != nil {
		return TypeSpec{}, 0, err
	}
	return t, consumed, nil
}

func decodeTypeSpecElement(e element) (TypeSpec, error) {
	switch e.tag {
	case tagDataBoolean:
		return TypeSpec{Kind: "boolean"}, nil
	case tagDataUTCTime:
		return TypeSpec{Kind: "utc-time"}, nil
	case tagDataBitString:
		bits, err := typeSpecInt(e.data)
		if err != nil {
			return TypeSpec{}, err
		}
		return TypeSpec{Kind: "bit-string", Bits: bits}, nil
	case tagDataInteger:
		bits, err := typeSpecInt(e.data)
		if err != nil {
			return TypeSpec{}, err
		}
		return TypeSpec{Kind: "integer", Bits: bits}, nil
	case tagDataUnsigned:
		bits, err := typeSpecInt(e.data)
		if err != nil {
			return TypeSpec{}, err
		}
		return TypeSpec{Kind: "unsigned", Bits: bits}, nil
	case 0xA7: // floatingpoint：格式位宽 + 指数位宽
		var formatWidth, expWidth int
		rest := e.data
		for len(rest) > 0 {
			f, err := parseElement(rest)
			if err != nil {
				return TypeSpec{}, err
			}
			rest = f.rest
			if f.tag != tagInt {
				continue
			}
			v, err := decodeInt(f.data)
			if err != nil {
				return TypeSpec{}, err
			}
			if formatWidth == 0 {
				formatWidth = int(v)
			} else {
				expWidth = int(v)
			}
		}
		return TypeSpec{Kind: "float", FloatWidth: formatWidth, ExponentWidth: expWidth}, nil
	case tagDataOctetString:
		n, err := typeSpecInt(e.data)
		if err != nil {
			return TypeSpec{}, err
		}
		return TypeSpec{Kind: "octet-string", Bits: n}, nil
	case tagDataVisibleString:
		n, err := typeSpecInt(e.data)
		if err != nil {
			return TypeSpec{}, err
		}
		return TypeSpec{Kind: "visible-string", Bits: n}, nil
	case tagDataMMSString:
		n, err := typeSpecInt(e.data)
		if err != nil {
			return TypeSpec{}, err
		}
		return TypeSpec{Kind: "mms-string", Bits: n}, nil
	case tagDataBinaryTime:
		return TypeSpec{Kind: "binary-time"}, nil
	case tagDataArray:
		out := TypeSpec{Kind: "array"}
		rest := e.data
		var elemBytes []byte
		for len(rest) > 0 {
			f, err := parseElement(rest)
			if err != nil {
				return TypeSpec{}, err
			}
			rest = f.rest
			switch f.tag {
			case 0x81: // numberOfElements [1] IMPLICIT INTEGER
				v, err := decodeUnsigned(f.data)
				if err != nil {
					return TypeSpec{}, err
				}
				out.Count = int(v)
			case 0xA2: // elementType [2] EXPLICIT
				elemBytes = f.data
			}
		}
		if len(elemBytes) > 0 {
			elem, _, err := DecodeTypeSpec(elemBytes)
			if err != nil {
				return TypeSpec{}, err
			}
			out.Elem = &elem
		}
		return out, nil
	case tagDataStructure:
		out := TypeSpec{Kind: "structure"}
		rest := e.data
		for len(rest) > 0 {
			f, err := parseElement(rest)
			if err != nil {
				return TypeSpec{}, err
			}
			rest = f.rest
			if f.tag != 0xA1 { // components [1] IMPLICIT SEQUENCE OF
				continue
			}
			items := f.data
			for len(items) > 0 {
				c, err := parseElement(items)
				if err != nil {
					return TypeSpec{}, err
				}
				items = c.rest
				if c.tag != tagSequence {
					continue
				}
				for _, part := range sequenceParts(c.data) {
					if part.tag != 0xA1 { // type [1] EXPLICIT
						continue
					}
					ts, _, err := DecodeTypeSpec(part.data)
					if err != nil {
						return TypeSpec{}, err
					}
					out.Components = append(out.Components, ts)
				}
			}
		}
		return out, nil
	case contextTag(0, true): // typeName [0] EXPLICIT ObjectName
		name := ""
		if obj, err := DecodeObjectName(e.data); err == nil {
			name = obj.String()
		}
		return TypeSpec{Kind: "named", Name: name}, nil
	}
	return TypeSpec{}, fmt.Errorf("iec61850: unsupported type specification tag 0x%02X", e.tag)
}

// sequenceParts 按顺序取出 SEQUENCE 里的各个元素。
func sequenceParts(b []byte) []element {
	var out []element
	for len(b) > 0 {
		e, err := parseElement(b)
		if err != nil {
			return out
		}
		out = append(out, e)
		b = e.rest
	}
	return out
}

// typeSpecInt 读一个「只有一个 INTEGER」的类型参数。
func typeSpecInt(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	v, err := decodeInt(b)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// String 返回类型的可读描述，用于 `-list` 与错误信息。
func (t TypeSpec) String() string {
	switch t.Kind {
	case "integer":
		return fmt.Sprintf("integer%d", t.Bits)
	case "unsigned":
		return fmt.Sprintf("unsigned%d", t.Bits)
	case "bit-string":
		if t.Bits < 0 {
			return "bit-string[*]"
		}
		return fmt.Sprintf("bit-string[%d]", t.Bits)
	case "float":
		return fmt.Sprintf("float%d(e%d)", t.FloatWidth, t.ExponentWidth)
	case "octet-string":
		return fmt.Sprintf("octet-string[%d]", t.Bits)
	case "visible-string":
		return fmt.Sprintf("visible-string[%d]", t.Bits)
	case "mms-string":
		return fmt.Sprintf("mms-string[%d]", t.Bits)
	case "array":
		elem := "?"
		if t.Elem != nil {
			elem = t.Elem.String()
		}
		return fmt.Sprintf("array[%d] of %s", t.Count, elem)
	case "structure":
		parts := make([]string, 0, len(t.Components))
		for _, c := range t.Components {
			parts = append(parts, c.String())
		}
		return "structure{" + strings.Join(parts, ", ") + "}"
	case "named":
		return "type:" + t.Name
	}
	return t.Kind
}

// FormatUTC 把一个 UtcTime 字节串渲染成可读时间，用于自检输出。
func FormatUTC(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	ms := utcTimeValue(b)
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}
