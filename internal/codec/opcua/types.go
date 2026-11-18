package opcua

import (
	"fmt"
	"time"
)

// 内置类型编号（OPC UA Part 6, 5.1.2）。
const (
	TypeBoolean      byte = 1
	TypeSByte        byte = 2
	TypeByte         byte = 3
	TypeInt16        byte = 4
	TypeUInt16       byte = 5
	TypeInt32        byte = 6
	TypeUInt32       byte = 7
	TypeInt64        byte = 8
	TypeUInt64       byte = 9
	TypeFloat        byte = 10
	TypeDouble       byte = 11
	TypeString       byte = 12
	TypeDateTime     byte = 13
	TypeGUID         byte = 14
	TypeByteString   byte = 15
	TypeXMLElement   byte = 16
	TypeNodeID       byte = 17
	TypeExpandedNode byte = 18
	TypeStatusCode   byte = 19
	TypeQualified    byte = 20
	TypeLocalized    byte = 21
	TypeExtension    byte = 22
	TypeDataValue    byte = 23
	TypeVariant      byte = 24
	TypeDiagnostic   byte = 25
)

// 常用状态码。完整表在规范 Part 4；这里只收录会影响错误分类的那些。
const (
	StatusGood                  StatusCode = 0x00000000
	StatusGoodClamped           StatusCode = 0x00300000
	StatusUncertain             StatusCode = 0x40000000
	StatusBadUnexpectedError    StatusCode = 0x80010000
	StatusBadInternalError      StatusCode = 0x80020000
	StatusBadCommunicationError StatusCode = 0x80050000
	StatusBadTimeout            StatusCode = 0x800A0000
	StatusBadServiceUnsupported StatusCode = 0x800B0000
	StatusBadSecurityChecks     StatusCode = 0x80130000
	StatusBadUserAccessDenied   StatusCode = 0x801F0000
	StatusBadSecureChannelID    StatusCode = 0x80220000
	StatusBadSessionIDInvalid   StatusCode = 0x80250000
	StatusBadSessionClosed      StatusCode = 0x80260000
	StatusBadNothingToDo        StatusCode = 0x800F0000
	StatusBadTooManyOperations  StatusCode = 0x80100000
	StatusBadNoCommunication    StatusCode = 0x80310000
	StatusBadWaitingForData     StatusCode = 0x80320000
	StatusBadNodeIDInvalid      StatusCode = 0x80330000
	StatusBadNodeIDUnknown      StatusCode = 0x80340000
	StatusBadAttributeInvalid   StatusCode = 0x80350000
	StatusBadNotReadable        StatusCode = 0x803A0000
	StatusBadNotWritable        StatusCode = 0x803B0000
	StatusBadOutOfRange         StatusCode = 0x803C0000
	StatusBadTypeMismatch       StatusCode = 0x80740000
	StatusBadNotConnected       StatusCode = 0x808A0000
	StatusBadNoData             StatusCode = 0x809B0000
)

func (s StatusCode) Error() string { return s.Text() }

// StatusCode 是 OPC UA 状态码。实现 error 以便直接作为错误返回。
type StatusCode uint32

// IsGood 表示状态码为 Good（含 Good 的子码）。
func (s StatusCode) IsGood() bool { return s&0xC0000000 == 0 }

// IsUncertain 表示 Uncertain 类别。
func (s StatusCode) IsUncertain() bool { return s&0xC0000000 == 0x40000000 }

// IsBad 表示 Bad 类别。
func (s StatusCode) IsBad() bool { return s&0x80000000 != 0 }

// Text 返回简短说明。不追求覆盖全部状态码，只保证日志里能看懂。
func (s StatusCode) Text() string {
	switch s {
	case StatusGood:
		return "Good"
	case StatusGoodClamped:
		return "GoodClamped"
	case StatusUncertain:
		return "Uncertain"
	case StatusBadUnexpectedError:
		return "BadUnexpectedError"
	case StatusBadInternalError:
		return "BadInternalError"
	case StatusBadCommunicationError:
		return "BadCommunicationError"
	case StatusBadTimeout:
		return "BadTimeout"
	case StatusBadServiceUnsupported:
		return "BadServiceUnsupported"
	case StatusBadSecurityChecks:
		return "BadSecurityChecksFailed"
	case StatusBadUserAccessDenied:
		return "BadUserAccessDenied"
	case StatusBadSecureChannelID:
		return "BadSecureChannelIdInvalid"
	case StatusBadSessionIDInvalid:
		return "BadSessionIdInvalid"
	case StatusBadSessionClosed:
		return "BadSessionClosed"
	case StatusBadNothingToDo:
		return "BadNothingToDo"
	case StatusBadTooManyOperations:
		return "BadTooManyOperations"
	case StatusBadNoCommunication:
		return "BadNoCommunication"
	case StatusBadWaitingForData:
		return "BadWaitingForInitialData"
	case StatusBadNodeIDInvalid:
		return "BadNodeIdInvalid"
	case StatusBadNodeIDUnknown:
		return "BadNodeIdUnknown"
	case StatusBadAttributeInvalid:
		return "BadAttributeIdInvalid"
	case StatusBadNotReadable:
		return "BadNotReadable"
	case StatusBadNotWritable:
		return "BadNotWritable"
	case StatusBadOutOfRange:
		return "BadOutOfRange"
	case StatusBadTypeMismatch:
		return "BadTypeMismatch"
	case StatusBadNotConnected:
		return "BadNotConnected"
	case StatusBadNoData:
		return "BadNoData"
	case StatusBadNoSubscription:
		return "BadNoSubscription"
	case StatusBadSequenceNumberUnknown:
		return "BadSequenceNumberUnknown"
	case StatusBadSubscriptionIDInvalid:
		return "BadSubscriptionIdInvalid"
	case StatusBadMonitoredItemIDInvalid:
		return "BadMonitoredItemIdInvalid"
	}
	return fmt.Sprintf("StatusCode(0x%08X)", uint32(s))
}

// ---------------------------------------------------------------------------
// 基础结构
// ---------------------------------------------------------------------------

// LocalizedText 是本地化文本。
type LocalizedText struct {
	Locale string
	Text   string
}

func (l LocalizedText) Encode(e *encoder) {
	mask := byte(0)
	if l.Locale != "" {
		mask |= 0x01
	}
	if l.Text != "" {
		mask |= 0x02
	}
	e.Byte(mask)
	if mask&0x01 != 0 {
		e.String(l.Locale)
	}
	if mask&0x02 != 0 {
		e.String(l.Text)
	}
}

func (d *decoder) LocalizedText() LocalizedText {
	mask := d.Byte()
	var l LocalizedText
	if mask&0x01 != 0 {
		l.Locale = d.String()
	}
	if mask&0x02 != 0 {
		l.Text = d.String()
	}
	return l
}

// QualifiedName 是带命名空间的名称。
type QualifiedName struct {
	Namespace uint16
	Name      string
}

func (q QualifiedName) Encode(e *encoder) {
	e.U16(q.Namespace)
	e.String(q.Name)
}

func (d *decoder) QualifiedName() QualifiedName {
	return QualifiedName{Namespace: d.U16(), Name: d.String()}
}

// ExtensionObject 是带类型标识的任意结构体。
type ExtensionObject struct {
	TypeID   NodeID
	Encoding byte
	Body     []byte
}

// NewExtensionObject 构造二进制编码的扩展对象。
func NewExtensionObject(typeID uint32, body []byte) ExtensionObject {
	return ExtensionObject{TypeID: Numeric(0, typeID), Encoding: 0x01, Body: body}
}

func (x ExtensionObject) Encode(e *encoder) {
	x.TypeID.Encode(e)
	e.Byte(x.Encoding)
	switch x.Encoding {
	case 0x01:
		e.I32(int32(len(x.Body)))
		e.b = append(e.b, x.Body...)
	case 0x02:
		e.String(string(x.Body))
	default:
		// 0x00：无体，合法。
	}
}

func (d *decoder) ExtensionObject() ExtensionObject {
	var x ExtensionObject
	x.TypeID = d.NodeID()
	x.Encoding = d.Byte()
	switch x.Encoding {
	case 0x01:
		n := d.I32()
		if n < 0 {
			return x
		}
		// 上界不是「合理大小」，而是防止坏报文触发巨量分配；
		// 16MB 已经远大于本实现所有的读响应。
		if n > 16<<20 {
			d.fail(fmt.Errorf("opcua: extension object body too large (%d)", n))
			return x
		}
		b := d.take(int(n))
		x.Body = append([]byte(nil), b...)
	case 0x02:
		x.Body = []byte(d.String())
	}
	return x
}

// DiagnosticInfo 只做跳过。本实现从不请求诊断信息，但服务端可能主动
// 返回；不跳过会让后续字段全部错位——这类错误表现为「响应解析失败」，
// 而真正的原因（服务端附了一条诊断）会完全看不出来。
func (d *decoder) diagnosticInfo(depth int) {
	if depth > 8 {
		d.fail(fmt.Errorf("opcua: diagnostic info nested too deep"))
		return
	}
	mask := d.Byte()
	if mask&0x01 != 0 {
		d.I32()
	}
	if mask&0x02 != 0 {
		d.I32()
	}
	if mask&0x04 != 0 {
		d.I32()
	}
	if mask&0x08 != 0 {
		d.I32()
	}
	if mask&0x10 != 0 {
		_ = d.String()
	}
	if mask&0x20 != 0 {
		d.U32()
	}
	if mask&0x40 != 0 {
		d.diagnosticInfo(depth + 1)
	}
}

// ---------------------------------------------------------------------------
// Variant / DataValue
// ---------------------------------------------------------------------------

// Variant 是 OPC UA 的自描述值。
//
// 数值统一放在 Int/Uint/Float 三个字段里，而不是一个大 float64：
// UInt64 超过 2^53 后放进 float64 会静默丢精度，工业现场的累计量
// （电量、流量）正好经常超过这个量级。
type Variant struct {
	Type   byte
	Array  bool
	Bool   bool
	Int    int64
	Uint   uint64
	Float  float64
	Str    string
	Bytes  []byte
	Time   time.Time
	NodeID NodeID
	Values []Variant
}

// Scalar 构造标量 Variant。
func Scalar(t byte) Variant { return Variant{Type: t} }

func (v Variant) Encode(e *encoder) {
	mask := v.Type
	if v.Array {
		mask |= 0x80
	}
	e.Byte(mask)
	if v.Array {
		// 数组的元素按内置类型**裸编码**（Part 6 §5.2.2.16）：
		// Int32 数组 = 长度 + n 个 4 字节，没有逐元素的 EncodingByte。
		// 每个元素再套一层 Variant 头是自创格式，真实服务器解出来是垃圾。
		e.ArrayLength(len(v.Values))
		for _, item := range v.Values {
			encodeBare(e, item)
		}
		return
	}
	encodeBare(e, v)
}

func encodeBare(e *encoder, v Variant) {
	switch v.Type {
	case TypeBoolean:
		e.Bool(v.Bool)
	case TypeSByte:
		e.Byte(byte(int8(v.Int)))
	case TypeByte:
		e.Byte(byte(v.Uint))
	case TypeInt16:
		e.I16(int16(v.Int))
	case TypeUInt16:
		e.U16(uint16(v.Uint))
	case TypeInt32:
		e.I32(int32(v.Int))
	case TypeUInt32:
		e.U32(uint32(v.Uint))
	case TypeInt64:
		e.I64(v.Int)
	case TypeUInt64:
		e.U64(v.Uint)
	case TypeFloat:
		e.F32(float32(v.Float))
	case TypeDouble:
		e.F64(v.Float)
	case TypeString:
		e.String(v.Str)
	case TypeDateTime:
		e.Time(v.Time)
	case TypeByteString, TypeXMLElement:
		e.ByteString(v.Bytes)
	case TypeNodeID:
		v.NodeID.Encode(e)
	case TypeStatusCode:
		e.U32(uint32(v.Uint))
	case TypeQualified:
		e.U16(uint16(v.Uint >> 16))
		e.String(v.Str)
	case TypeLocalized:
		LocalizedText{Text: v.Str}.Encode(e)
	case TypeExtension:
		// 仅支持「无体」扩展对象：写请求里不会出现，读响应里也极少见。
		NullNodeID.Encode(e)
		e.Byte(0)
	default:
		// 未知类型：写空字符串会在服务端产生类型错，但比 panic 好。
		e.String("")
	}
}

func (d *decoder) Variant() Variant {
	mask := d.Byte()
	v := Variant{Type: mask & 0x3F, Array: mask&0x80 != 0}
	if v.Type == 0 {
		return v
	}
	if v.Array {
		n := d.decodeArrayLen(65535)
		v.Values = make([]Variant, 0, n)
		for i := 0; i < n && d.err == nil; i++ {
			item := decodeBare(d, v.Type)
			v.Values = append(v.Values, item)
		}
		if mask&0x40 != 0 {
			dim := d.decodeArrayLen(64)
			for i := 0; i < dim; i++ {
				d.I32()
			}
		}
		return v
	}
	return decodeBare(d, v.Type)
}

// decodeBare 按内置类型读一个值（标量 Variant 的正文，或数组的元素）。
func decodeBare(d *decoder, typ byte) Variant {
	v := Variant{Type: typ}
	switch typ {
	case TypeBoolean:
		v.Bool = d.Bool()
	case TypeSByte:
		v.Int = int64(int8(d.Byte()))
	case TypeByte:
		v.Uint = uint64(d.Byte())
	case TypeInt16:
		v.Int = int64(d.I16())
	case TypeUInt16:
		v.Uint = uint64(d.U16())
	case TypeInt32:
		v.Int = int64(d.I32())
	case TypeUInt32:
		v.Uint = uint64(d.U32())
	case TypeInt64:
		v.Int = d.I64()
	case TypeUInt64:
		v.Uint = d.U64()
	case TypeFloat:
		v.Float = float64(d.F32())
	case TypeDouble:
		v.Float = d.F64()
	case TypeString, TypeXMLElement:
		v.Str = d.String()
	case TypeDateTime:
		v.Time = d.Time()
	case TypeByteString:
		v.Bytes = d.ByteString()
	case TypeNodeID:
		v.NodeID = d.NodeID()
	case TypeStatusCode:
		v.Uint = uint64(d.U32())
	case TypeQualified:
		q := d.QualifiedName()
		v.Uint = uint64(q.Namespace) << 16
		v.Str = q.Name
	case TypeLocalized:
		v.Str = d.LocalizedText().Text
	case TypeExtension:
		_ = d.ExtensionObject()
	case TypeDataValue:
		d.DataValue()
	case TypeVariant:
		v = d.Variant()
	case TypeGUID:
		d.Skip(16)
	case TypeExpandedNode:
		d.ExpandedNodeID()
	case TypeDiagnostic:
		d.diagnosticInfo(0)
	default:
		d.fail(fmt.Errorf("opcua: unsupported variant type %d", typ))
	}
	return v
}

// DataValue 是「值 + 质量 + 时间戳」的组合。
type DataValue struct {
	Value           *Variant
	Status          StatusCode
	SourceTimestamp time.Time
	ServerTimestamp time.Time
}

const (
	dvValue      = 0x01
	dvStatus     = 0x02
	dvSourceTime = 0x04
	dvServerTime = 0x08
	dvSourcePico = 0x10
	dvServerPico = 0x20
)

func (v DataValue) Encode(e *encoder) {
	mask := byte(0)
	if v.Value != nil {
		mask |= dvValue
	}
	if v.Status != 0 {
		mask |= dvStatus
	}
	if !v.SourceTimestamp.IsZero() {
		mask |= dvSourceTime
	}
	if !v.ServerTimestamp.IsZero() {
		mask |= dvServerTime
	}
	e.Byte(mask)
	if v.Value != nil {
		v.Value.Encode(e)
	}
	if mask&dvStatus != 0 {
		e.U32(uint32(v.Status))
	}
	if mask&dvSourceTime != 0 {
		e.Time(v.SourceTimestamp)
	}
	if mask&dvServerTime != 0 {
		e.Time(v.ServerTimestamp)
	}
}

func (d *decoder) DataValue() DataValue {
	mask := d.Byte()
	var v DataValue
	if mask&dvValue != 0 {
		val := d.Variant()
		v.Value = &val
	}
	if mask&dvStatus != 0 {
		v.Status = StatusCode(d.U32())
	}
	if mask&dvSourceTime != 0 {
		v.SourceTimestamp = d.Time()
	}
	if mask&dvSourcePico != 0 {
		d.U16()
	}
	if mask&dvServerTime != 0 {
		v.ServerTimestamp = d.Time()
	}
	if mask&dvServerPico != 0 {
		d.U16()
	}
	return v
}
