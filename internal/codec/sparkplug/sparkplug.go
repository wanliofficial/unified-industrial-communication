// Package sparkplug 是 Sparkplug B 载荷的纯函数编解码。
//
// Sparkplug B 没有自己的帧：它把 Protobuf 编码的 Payload 放进 MQTT 消息，
// 生命周期完全由主题（NBIRTH/NDEATH/DBIRTH/DDEATH/DDATA）表达。因此这里
// 只做两件事：
//
//  1. 把 Payload/Metric 编成 Protobuf 的线格式（varint、长度前缀、定长）；
//  2. 反过来解析命令载荷（NCMD/DCMD）。
//
// 不引第三方 protobuf 运行时：整个仓库零依赖，而 Sparkplug 用到的 schema
// 很小，手写编码器比引入一套代码生成链路更可控。未知字段按 Protobuf 的
// 规则跳过，这样对端升级 schema 不会让解析直接失败。
//
// 没有实现 Metadata / DataSet / 模板：本平台只用 `engUnit` 与 `quality`
// 两个字符串属性。PropertySet 按线格式编码成 keys/values 两组平行数组，
// 解码时只取 string_value；其它属性类型跳过而不是报错。
package sparkplug

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"
)

// DataType 是 Sparkplug B 的 Metric 数据类型枚举。
//
// 枚举值属于线契约，不能改。平台类型到它的映射在 internal/sparkplug
// 里，codec 本身不认识 model.DataType。
type DataType uint32

const (
	TypeUnknown  DataType = 0
	TypeInt8     DataType = 1
	TypeInt16    DataType = 2
	TypeInt32    DataType = 3
	TypeInt64    DataType = 4
	TypeUInt8    DataType = 5
	TypeUInt16   DataType = 6
	TypeUInt32   DataType = 7
	TypeUInt64   DataType = 8
	TypeFloat    DataType = 9
	TypeDouble   DataType = 10
	TypeBoolean  DataType = 11
	TypeString   DataType = 12
	TypeDateTime DataType = 13
	TypeText     DataType = 14
	// 以下为「附加类型」。本平台只用到 Bytes；其余保留枚举名是为了在
	// 解码时能把对端的类型准确报出来，而不是显示成一个裸数字。
	TypeUUID     DataType = 15
	TypeDataSet  DataType = 16
	TypeBytes    DataType = 17
	TypeFile     DataType = 18
	TypeTemplate DataType = 19
)

func (d DataType) String() string {
	switch d {
	case TypeUnknown:
		return "Unknown"
	case TypeInt8:
		return "Int8"
	case TypeInt16:
		return "Int16"
	case TypeInt32:
		return "Int32"
	case TypeInt64:
		return "Int64"
	case TypeUInt8:
		return "UInt8"
	case TypeUInt16:
		return "UInt16"
	case TypeUInt32:
		return "UInt32"
	case TypeUInt64:
		return "UInt64"
	case TypeFloat:
		return "Float"
	case TypeDouble:
		return "Double"
	case TypeBoolean:
		return "Boolean"
	case TypeString:
		return "String"
	case TypeDateTime:
		return "DateTime"
	case TypeText:
		return "Text"
	case TypeUUID:
		return "UUID"
	case TypeDataSet:
		return "DataSet"
	case TypeBytes:
		return "Bytes"
	case TypeFile:
		return "File"
	case TypeTemplate:
		return "Template"
	}
	return fmt.Sprintf("DataType(%d)", uint32(d))
}

// Metric 是一个 Sparkplug 指标。
//
// Value 的具体类型由 DataType 决定：
//
//	Int8/Int16/Int32/Int64/DateTime → int64
//	UInt8/UInt16/UInt32/UInt64      → uint64
//	Float                          → float32
//	Double                         → float64
//	Boolean                        → bool
//	String/Text                    → string
//	bytes_value（字段 16）          → []byte
//
// IsNull 为 true 时 Value 应为 nil：Sparkplug 用它表示「这一轮没有可用
// 值」，与本平台的非 good 质量对应。
type Metric struct {
	Name       string
	Alias      uint64
	Timestamp  time.Time
	DataType   DataType
	IsNull     bool
	Value      any
	Properties map[string]string
}

// Payload 是 Sparkplug B 的一帧。
type Payload struct {
	Timestamp time.Time
	Seq       uint64
	Metrics   []Metric
}

// ---------------------------------------------------------------------------
// 编码
// ---------------------------------------------------------------------------

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

type writer struct{ b []byte }

func (w *writer) varint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

func (w *writer) tag(field, wire int) { w.varint(uint64(field)<<3 | uint64(wire)) }

func (w *writer) uint(field int, v uint64) {
	if v == 0 {
		return
	}
	w.uintForce(field, v)
}

// uintForce 用于 oneof 里的值字段：0 是一个合法值，不能按「缺省字段」
// 省略。省略会让对端把 Boolean false / int 0 读成「没有值」。
func (w *writer) uintForce(field int, v uint64) {
	w.tag(field, wireVarint)
	w.varint(v)
}

func (w *writer) boolean(field int, v bool) {
	if !v {
		return
	}
	w.tag(field, wireVarint)
	w.varint(1)
}

func (w *writer) str(field int, s string) {
	if s == "" {
		return
	}
	w.strForce(field, s)
}

func (w *writer) strForce(field int, s string) {
	w.tag(field, wireBytes)
	w.varint(uint64(len(s)))
	w.b = append(w.b, s...)
}

func (w *writer) bytes(field int, b []byte) {
	w.tag(field, wireBytes)
	w.varint(uint64(len(b)))
	w.b = append(w.b, b...)
}

func (w *writer) fixed32(field int, v uint32) {
	w.tag(field, wireFixed32)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	w.b = append(w.b, buf[:]...)
}

func (w *writer) fixed64(field int, v uint64) {
	w.tag(field, wireFixed64)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	w.b = append(w.b, buf[:]...)
}

func (w *writer) message(field int, fn func(*writer)) {
	var sub writer
	fn(&sub)
	w.tag(field, wireBytes)
	w.varint(uint64(len(sub.b)))
	w.b = append(w.b, sub.b...)
}

// Encode 把 Payload 编码成 Protobuf 线格式。
func (p Payload) Encode() ([]byte, error) {
	var w writer
	if !p.Timestamp.IsZero() {
		w.uint(1, uint64(p.Timestamp.UnixMilli()))
	}
	for _, m := range p.Metrics {
		m := m
		if err := m.validate(); err != nil {
			return nil, err
		}
		w.message(2, func(sw *writer) { sw.metric(m) })
	}
	w.uint(3, p.Seq)
	return w.b, nil
}

func (m Metric) validate() error {
	if m.Name == "" && m.Alias == 0 {
		return fmt.Errorf("sparkplug: metric needs name or alias")
	}
	if m.IsNull {
		return nil
	}
	switch m.DataType {
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64, TypeDateTime:
		if _, ok := m.Value.(int64); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype %v needs int64, got %T", m.Name, m.DataType, m.Value)
		}
	case TypeUInt8, TypeUInt16, TypeUInt32, TypeUInt64:
		if _, ok := m.Value.(uint64); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype %v needs uint64, got %T", m.Name, m.DataType, m.Value)
		}
	case TypeFloat:
		if _, ok := m.Value.(float32); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype Float needs float32, got %T", m.Name, m.Value)
		}
	case TypeDouble:
		if _, ok := m.Value.(float64); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype Double needs float64, got %T", m.Name, m.Value)
		}
	case TypeBoolean:
		if _, ok := m.Value.(bool); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype Boolean needs bool, got %T", m.Name, m.Value)
		}
	case TypeString, TypeText:
		if _, ok := m.Value.(string); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype %v needs string, got %T", m.Name, m.DataType, m.Value)
		}
	case TypeBytes:
		if _, ok := m.Value.([]byte); !ok {
			return fmt.Errorf("sparkplug: metric %q datatype Bytes needs []byte, got %T", m.Name, m.Value)
		}
	default:
		switch m.Value.(type) {
		case nil, int64, uint64, float32, float64, bool, string, []byte:
		default:
			return fmt.Errorf("sparkplug: metric %q has unsupported value type %T", m.Name, m.Value)
		}
	}
	return nil
}

func boolBit(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func (w *writer) metric(m Metric) {
	w.str(1, m.Name)
	w.uint(2, m.Alias)
	if !m.Timestamp.IsZero() {
		w.uint(3, uint64(m.Timestamp.UnixMilli()))
	}
	w.uint(4, uint64(m.DataType))
	w.boolean(7, m.IsNull)
	if len(m.Properties) > 0 {
		w.message(9, func(pw *writer) {
			// PropertySet 是「平行数组」而不是 map：keys 与 values 按下标
			// 一一对应。map 的迭代顺序不稳定，必须排序，否则同一个指标
			// 每次编码出的字节不同，金帧测试与抓包 diff 都会变成噪声。
			keys := make([]string, 0, len(m.Properties))
			for k := range m.Properties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				pw.str(1, k)
			}
			for _, k := range keys {
				val := m.Properties[k]
				pw.message(2, func(v *writer) {
					v.uint(1, uint64(TypeString))
					v.str(8, val)
				})
			}
		})
	}
	if m.IsNull || m.Value == nil {
		return
	}
	switch v := m.Value.(type) {
	case int64:
		// Int8/16/32 走 int_value（uint32），Int64 走 long_value。
		// 负数按补码截断成无符号 varint，这是 Protobuf 的 int32 语义。
		if m.DataType == TypeInt64 || m.DataType == TypeDateTime {
			w.uintForce(11, uint64(v))
		} else {
			w.uintForce(10, uint64(uint32(v)))
		}
	case uint64:
		if m.DataType == TypeUInt64 {
			w.uintForce(11, v)
		} else {
			w.uintForce(10, v)
		}
	case float32:
		w.fixed32(12, math.Float32bits(v))
	case float64:
		w.fixed64(13, math.Float64bits(v))
	case bool:
		w.uintForce(14, boolBit(v))
	case string:
		w.strForce(15, v)
	case []byte:
		w.bytes(16, v)
	}
}

// ---------------------------------------------------------------------------
// 解码
// ---------------------------------------------------------------------------

type fieldValue struct {
	wire    int
	varint  uint64
	fixed32 uint32
	fixed64 uint64
	bytes   []byte
}

// parseFields 把一条 Protobuf 消息拆成字段表。只支持 Sparkplug 用到的
// 四种线类型；其它线类型（3/4 组）直接报错，而不是按「未知字段」跳过——
// 那会让一条损坏的报文被当成合法命令。
func parseFields(b []byte) (map[int][]fieldValue, error) {
	out := map[int][]fieldValue{}
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, fmt.Errorf("sparkplug: truncated field key")
		}
		b = b[n:]
		field, wire := int(key>>3), int(key&7)
		if field <= 0 {
			return nil, fmt.Errorf("sparkplug: invalid field number %d", field)
		}
		var fv fieldValue
		fv.wire = wire
		switch wire {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("sparkplug: truncated varint in field %d", field)
			}
			fv.varint = v
			b = b[n:]
		case wireFixed64:
			if len(b) < 8 {
				return nil, fmt.Errorf("sparkplug: truncated fixed64 in field %d", field)
			}
			fv.fixed64 = binary.LittleEndian.Uint64(b[:8])
			b = b[8:]
		case wireFixed32:
			if len(b) < 4 {
				return nil, fmt.Errorf("sparkplug: truncated fixed32 in field %d", field)
			}
			fv.fixed32 = binary.LittleEndian.Uint32(b[:4])
			b = b[4:]
		case wireBytes:
			l, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("sparkplug: truncated length in field %d", field)
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return nil, fmt.Errorf("sparkplug: field %d says %d bytes, only %d left", field, l, len(b))
			}
			fv.bytes = b[:l]
			b = b[l:]
		default:
			return nil, fmt.Errorf("sparkplug: unsupported wire type %d in field %d", wire, field)
		}
		out[field] = append(out[field], fv)
	}
	return out, nil
}

func last(fields map[int][]fieldValue, n int) (fieldValue, bool) {
	vs := fields[n]
	if len(vs) == 0 {
		return fieldValue{}, false
	}
	return vs[len(vs)-1], true
}

// Decode 解析一帧 Payload。
func Decode(b []byte) (Payload, error) {
	fields, err := parseFields(b)
	if err != nil {
		return Payload{}, err
	}
	var p Payload
	if v, ok := last(fields, 1); ok {
		p.Timestamp = time.UnixMilli(int64(v.varint)).UTC()
	}
	if v, ok := last(fields, 3); ok {
		p.Seq = v.varint
	}
	for _, mv := range fields[2] {
		m, err := decodeMetric(mv.bytes)
		if err != nil {
			return Payload{}, err
		}
		p.Metrics = append(p.Metrics, m)
	}
	return p, nil
}

func decodeMetric(b []byte) (Metric, error) {
	fields, err := parseFields(b)
	if err != nil {
		return Metric{}, err
	}
	var m Metric
	if v, ok := last(fields, 1); ok {
		m.Name = string(v.bytes)
	}
	if v, ok := last(fields, 2); ok {
		m.Alias = v.varint
	}
	if v, ok := last(fields, 3); ok {
		m.Timestamp = time.UnixMilli(int64(v.varint)).UTC()
	}
	if v, ok := last(fields, 4); ok {
		m.DataType = DataType(v.varint)
	}
	if v, ok := last(fields, 7); ok {
		m.IsNull = v.varint != 0
	}
	if v, ok := last(fields, 9); ok {
		if props, err := decodeProperties(v.bytes); err != nil {
			return Metric{}, err
		} else if len(props) > 0 {
			m.Properties = props
		}
	}
	if m.IsNull {
		return m, nil
	}

	// oneof：最多一个 value 字段。按线类型取，而不是只信任 datatype——
	// 一条声明 Int16 却带 double_value 的报文是损坏的，必须报错。
	if v, ok := last(fields, 10); ok {
		m.Value = valueForInt(m.DataType, uint32(v.varint))
		return m, nil
	}
	if v, ok := last(fields, 11); ok {
		m.Value = valueForLong(m.DataType, v.varint)
		return m, nil
	}
	if v, ok := last(fields, 12); ok {
		if m.DataType != TypeFloat && m.DataType != TypeUnknown {
			return Metric{}, fmt.Errorf("sparkplug: metric %q declares %v but carries float_value", m.Name, m.DataType)
		}
		m.Value = math.Float32frombits(v.fixed32)
		return m, nil
	}
	if v, ok := last(fields, 13); ok {
		if m.DataType != TypeDouble && m.DataType != TypeUnknown {
			return Metric{}, fmt.Errorf("sparkplug: metric %q declares %v but carries double_value", m.Name, m.DataType)
		}
		m.Value = math.Float64frombits(v.fixed64)
		return m, nil
	}
	if v, ok := last(fields, 14); ok {
		m.Value = v.varint != 0
		return m, nil
	}
	if v, ok := last(fields, 15); ok {
		m.Value = string(v.bytes)
		return m, nil
	}
	if v, ok := last(fields, 16); ok {
		m.Value = append([]byte(nil), v.bytes...)
		return m, nil
	}
	return m, nil
}

// decodeProperties 解析 PropertySet：keys 是字符串数组，values 是按下标
// 对应的 PropertyValue 数组。只取 string_value；其它类型（嵌套属性集、
// 扩展）在本地没有使用场景，跳过即可。
func decodeProperties(b []byte) (map[string]string, error) {
	fields, err := parseFields(b)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(fields[1]))
	for _, v := range fields[1] {
		keys = append(keys, string(v.bytes))
	}
	vals := fields[2]
	out := map[string]string{}
	for i, k := range keys {
		if i >= len(vals) {
			break
		}
		vf, err := parseFields(vals[i].bytes)
		if err != nil {
			return nil, err
		}
		if sv, ok := last(vf, 8); ok {
			out[k] = string(sv.bytes)
		}
	}
	return out, nil
}

func valueForInt(dt DataType, v uint32) any {
	switch dt {
	case TypeInt8:
		return int64(int8(uint8(v)))
	case TypeInt16:
		return int64(int16(uint16(v)))
	case TypeInt32:
		return int64(int32(v))
	case TypeInt64, TypeDateTime:
		return int64(v)
	case TypeUInt8:
		return uint64(uint8(v))
	case TypeUInt16:
		return uint64(uint16(v))
	case TypeUInt32:
		return uint64(v)
	case TypeUInt64:
		return uint64(v)
	}
	return int64(v)
}

func valueForLong(dt DataType, v uint64) any {
	switch dt {
	case TypeInt64, TypeDateTime:
		return int64(v)
	case TypeUInt64:
		return v
	case TypeInt8, TypeInt16, TypeInt32:
		return int64(v)
	}
	return v
}
