package snmp

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"

	"uic/internal/driver"
)

// Type 是 varbind 值的 BER 标签。
//
// 前六个是通用 BER 类型；0x40 起是 SNMP 的「应用类型」，0x80 起是 v2c 的
// 异常标记。这些标签直接出现在线上，不能重排。
type Type byte

const (
	TypeInteger     Type = tagInteger
	TypeOctetString Type = tagOctetString
	TypeNull        Type = tagNull
	TypeOID         Type = tagOID

	TypeIPAddress Type = 0x40
	TypeCounter32 Type = 0x41
	// Gauge32 与 Unsigned32 在 SMIv2 里是同一个 ASN.1 类型
	// （`Unsigned32 ::= [APPLICATION 2] IMPLICIT INTEGER (0..4294967295)`，
	// RFC 2578 §7.1.7 明确写「indistinguishable from Gauge32」），
	// 共用线上标签 0x42。因此没有独立的 Unsigned32 标签：无符号 32 位一律
	// 用 TypeGauge32 承载。历史上这里曾出现过 0x47（APPLICATION 7）这个
	// 「UInteger32」标签，但标准 SNMP 从未分配过 0x47——保留它会让解析器
	// 把一个线上不存在的标签当成合法无符号值接受，也会诱导调用方构造出对
	// 标准代理无意义的报文。已按标准移除。
	TypeGauge32   Type = 0x42
	TypeTimeTicks Type = 0x43
	TypeOpaque    Type = 0x44
	TypeCounter64 Type = 0x46

	// v2c 的三种「这个 OID 没有值」标记。它们出现在 varbind 的值位置，
	// 代替真正的值；错误码仍是 0。
	TypeNoSuchObject   Type = 0x80
	TypeNoSuchInstance Type = 0x81
	TypeEndOfMibView   Type = 0x82
)

func (t Type) String() string {
	switch t {
	case TypeInteger:
		return "INTEGER"
	case TypeOctetString:
		return "OCTET STRING"
	case TypeNull:
		return "NULL"
	case TypeOID:
		return "OID"
	case TypeIPAddress:
		return "IpAddress"
	case TypeCounter32:
		return "Counter32"
	case TypeGauge32:
		return "Gauge32"
	case TypeTimeTicks:
		return "TimeTicks"
	case TypeOpaque:
		return "Opaque"
	case TypeCounter64:
		return "Counter64"
	case TypeNoSuchObject:
		return "noSuchObject"
	case TypeNoSuchInstance:
		return "noSuchInstance"
	case TypeEndOfMibView:
		return "endOfMibView"
	}
	return fmt.Sprintf("type(0x%02X)", byte(t))
}

// IsException 表示这个类型不是值，而是「该 OID 没有值」的说明。
func (t Type) IsException() bool {
	switch t {
	case TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView:
		return true
	}
	return false
}

// Value 是 varbind 的值。
//
// 用一个结构体承载所有类型而不是 interface{}：编码/解码只有一份实现，
// 错误信息能带上线类型，驱动也不必对每个 varbind 做一次类型断言。
type Value struct {
	Type  Type
	Int   int64
	Uint  uint64
	Bytes []byte
	OID   OID
}

// Integer 构造 INTEGER。
func Integer(v int64) Value { return Value{Type: TypeInteger, Int: v} }

// Counter32/Gauge32/TimeTicks 共用无符号载体。Gauge32 同时是 SMIv2 里
// Unsigned32 的线上表示（二者不可区分，见 TypeGauge32 的说明）。
func Unsigned(t Type, v uint64) Value { return Value{Type: t, Uint: v} }

// Octets 构造 OCTET STRING（含 Opaque）。
func Octets(b []byte) Value {
	return Value{Type: TypeOctetString, Bytes: append([]byte(nil), b...)}
}

// String 构造 UTF-8/ASCII 的 OCTET STRING。
func String(s string) Value { return Octets([]byte(s)) }

// OIDValue 构造 OID 值。
func OIDValue(o OID) Value { return Value{Type: TypeOID, OID: o.Clone()} }

// IP 构造 IpAddress（线上必须 4 字节；这里不做补齐，长度错在编码时暴露）。
func IP(b []byte) Value {
	return Value{Type: TypeIPAddress, Bytes: append([]byte(nil), b...)}
}

// Null 构造 NULL（请求里的值位置必须放它）。
func Null() Value { return Value{Type: TypeNull} }

// Exception 构造 v2c 的「没有值」标记。
func Exception(t Type) Value { return Value{Type: t} }

// IsException 判断这个值是不是「没有值」标记。
func (v Value) IsException() bool { return v.Type.IsException() }

// String 生成人读的单行表示，用于日志与自检输出。
func (v Value) String() string {
	switch v.Type {
	case TypeInteger:
		return strconv.FormatInt(v.Int, 10)
	case TypeCounter32, TypeGauge32, TypeTimeTicks, TypeCounter64:
		return strconv.FormatUint(v.Uint, 10)
	case TypeOctetString, TypeOpaque:
		if printable(v.Bytes) {
			return string(v.Bytes)
		}
		return "0x" + hex.EncodeToString(v.Bytes)
	case TypeOID:
		return v.OID.String()
	case TypeIPAddress:
		return net.IP(v.Bytes).String()
	case TypeNull:
		return ""
	case TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView:
		return v.Type.String()
	}
	return "0x" + hex.EncodeToString(v.Bytes)
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// encodeValue 把值编成 TLV。
func encodeValue(v Value) ([]byte, error) {
	switch v.Type {
	case TypeInteger:
		return appendTLV(nil, tagInteger, encodeInt(v.Int)), nil
	case TypeOctetString, TypeOpaque:
		return appendTLV(nil, byte(v.Type), v.Bytes), nil
	case TypeNull:
		return []byte{tagNull, 0x00}, nil
	case TypeOID:
		if err := v.OID.Validate(); err != nil {
			return nil, err
		}
		return appendTLV(nil, tagOID, encodeOID(v.OID)), nil
	case TypeIPAddress:
		if len(v.Bytes) != 4 {
			return nil, fmt.Errorf("snmp: IpAddress must be 4 bytes, got %d", len(v.Bytes))
		}
		return appendTLV(nil, byte(TypeIPAddress), v.Bytes), nil
	case TypeCounter32, TypeGauge32, TypeTimeTicks:
		if v.Uint > 0xFFFFFFFF {
			return nil, fmt.Errorf("snmp: %s value %d overflows 32 bits", v.Type, v.Uint)
		}
		return appendTLV(nil, byte(v.Type), encodeUnsigned(v.Uint)), nil
	case TypeCounter64:
		return appendTLV(nil, byte(TypeCounter64), encodeUnsigned(v.Uint)), nil
	case TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView:
		// 异常标记是**应答**里的合法内容（内置代理要能发出去）；但它们是
		// 「值位置上的说明」而不是值，写请求里出现就是错误的——Build 会
		// 对请求 PDU 单独拦下（见 Build）。
		return appendTLV(nil, byte(v.Type), nil), nil
	}
	return nil, fmt.Errorf("snmp: cannot encode value type 0x%02X", byte(v.Type))
}

// parseValue 解析 varbind 值的 TLV 内容。
func parseValue(tag byte, b []byte) (Value, error) {
	t := Type(tag)
	switch t {
	case TypeInteger:
		n, err := decodeInt(b)
		if err != nil {
			return Value{}, err
		}
		return Integer(n), nil
	case TypeOctetString, TypeOpaque:
		return Value{Type: t, Bytes: append([]byte(nil), b...)}, nil
	case TypeNull:
		if len(b) != 0 {
			return Value{}, fmt.Errorf("snmp: NULL with %d content bytes", len(b))
		}
		return Null(), nil
	case TypeOID:
		o, err := decodeOID(b)
		if err != nil {
			return Value{}, err
		}
		return OIDValue(o), nil
	case TypeIPAddress:
		if len(b) != 4 {
			return Value{}, fmt.Errorf("snmp: IpAddress is %d bytes, want 4", len(b))
		}
		return IP(b), nil
	case TypeCounter32, TypeGauge32, TypeTimeTicks:
		n, err := decodeUnsigned(b)
		if err != nil {
			return Value{}, err
		}
		if n > 0xFFFFFFFF {
			return Value{}, fmt.Errorf("snmp: %s value %d overflows 32 bits", t, n)
		}
		return Unsigned(t, n), nil
	case TypeCounter64:
		n, err := decodeUnsigned(b)
		if err != nil {
			return Value{}, err
		}
		return Unsigned(TypeCounter64, n), nil
	case TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView:
		if len(b) != 0 {
			return Value{}, fmt.Errorf("snmp: exception %s with %d content bytes", t, len(b))
		}
		return Exception(t), nil
	}
	return Value{}, fmt.Errorf("snmp: unsupported value type 0x%02X", tag)
}

// ErrorStatus 是 PDU 的 error-status。
//
// v1 与 v2c 的前 6 个码相同，v2c 追加了 6..18。状态码不是「错误文本」而是
// 「下一步该做什么」：noSuchName 要改点表，tooBig 要减 OID 数量，readOnly
// 要开权限——把它们统一成一句「设备返回错误」会让现场无从下手。
type ErrorStatus int

const (
	ErrNoError ErrorStatus = 0
	// v1 / v2c 共有
	ErrTooBig     ErrorStatus = 1
	ErrNoSuchName ErrorStatus = 2
	ErrBadValue   ErrorStatus = 3
	ErrReadOnly   ErrorStatus = 4
	ErrGenErr     ErrorStatus = 5
	// 仅 v2c
	ErrNoAccess            ErrorStatus = 6
	ErrWrongType           ErrorStatus = 7
	ErrWrongLength         ErrorStatus = 8
	ErrWrongEncoding       ErrorStatus = 9
	ErrWrongValue          ErrorStatus = 10
	ErrNoCreation          ErrorStatus = 11
	ErrInconsistentValue   ErrorStatus = 12
	ErrResourceUnavailable ErrorStatus = 13
	ErrCommitFailed        ErrorStatus = 14
	ErrUndoFailed          ErrorStatus = 15
	ErrAuthorizationError  ErrorStatus = 16
	ErrNotWritable         ErrorStatus = 17
	ErrInconsistentName    ErrorStatus = 18
)

func (e ErrorStatus) String() string {
	switch e {
	case ErrNoError:
		return "noError"
	case ErrTooBig:
		return "tooBig"
	case ErrNoSuchName:
		return "noSuchName"
	case ErrBadValue:
		return "badValue"
	case ErrReadOnly:
		return "readOnly"
	case ErrGenErr:
		return "genErr"
	case ErrNoAccess:
		return "noAccess"
	case ErrWrongType:
		return "wrongType"
	case ErrWrongLength:
		return "wrongLength"
	case ErrWrongEncoding:
		return "wrongEncoding"
	case ErrWrongValue:
		return "wrongValue"
	case ErrNoCreation:
		return "noCreation"
	case ErrInconsistentValue:
		return "inconsistentValue"
	case ErrResourceUnavailable:
		return "resourceUnavailable"
	case ErrCommitFailed:
		return "commitFailed"
	case ErrUndoFailed:
		return "undoFailed"
	case ErrAuthorizationError:
		return "authorizationError"
	case ErrNotWritable:
		return "notWritable"
	case ErrInconsistentName:
		return "inconsistentName"
	}
	return "status(" + strconv.Itoa(int(e)) + ")"
}

// Kind 把 error-status 映射成驱动的错误分类。
//
// 分类决定「要不要重试/要不要熔断」：地址与权限问题不该让整台设备被熔断，
// 而 genErr/commitFailed 这类代理内部故障值得重试一次。
func (e ErrorStatus) Kind() driver.ErrorKind {
	switch e {
	case ErrNoError:
		return ""
	case ErrTooBig, ErrGenErr, ErrCommitFailed, ErrUndoFailed:
		return driver.ErrProtocol
	case ErrNoSuchName, ErrBadValue, ErrWrongType, ErrWrongLength, ErrWrongEncoding,
		ErrWrongValue, ErrInconsistentValue, ErrInconsistentName:
		return driver.ErrAddress
	case ErrReadOnly, ErrNoAccess, ErrNoCreation, ErrAuthorizationError, ErrNotWritable:
		return driver.ErrPermission
	case ErrResourceUnavailable:
		return driver.ErrPermission
	}
	return driver.ErrProtocol
}

// Temporary 表示该状态码值得重试。
func (e ErrorStatus) Temporary() bool {
	switch e {
	case ErrTooBig, ErrGenErr, ErrCommitFailed, ErrResourceUnavailable:
		return true
	}
	return false
}

// ExceptionKind 把 v2c 的异常标记映射成错误分类：三种都表示「这个 OID 在
// 这台设备上不存在」，属于点表问题，不是链路问题。
func (t Type) ExceptionKind() driver.ErrorKind { return driver.ErrAddress }

// ParseErrorStatus 把线序数字转成枚举，便于日志。
func ParseErrorStatus(n int) ErrorStatus { return ErrorStatus(n) }

// IsZeroOID 判断是否是 0.0（GetBulk 的占位 OID）。
func (o OID) IsZero() bool { return len(o) == 2 && o[0] == 0 && o[1] == 0 }

// ParseOIDList 解析逗号/空白分隔的多个 OID，供 CLI 与测试使用。
func ParseOIDList(s string) ([]OID, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]OID, 0, len(fields))
	for _, f := range fields {
		o, err := ParseOID(f)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("snmp: no OID given")
	}
	return out, nil
}
