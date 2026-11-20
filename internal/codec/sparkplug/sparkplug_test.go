package sparkplug

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"
	"time"
)

// 金帧钉住线格式：字段号、线类型、oneof 值字段都不能漂。Sparkplug 的
// 消费方（Ignition 一类）不会因为我们「字段号写错但自洽」而宽容。
func TestEncodeGolden(t *testing.T) {
	p := Payload{
		Timestamp: time.UnixMilli(1000).UTC(),
		Seq:       5,
		Metrics: []Metric{{
			Name:     "x",
			DataType: TypeInt16,
			Value:    int64(42),
		}},
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("08e80712070a01782002502a1805")
	if !bytes.Equal(raw, want) {
		t.Fatalf("payload = %x, want %x", raw, want)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 5 || !got.Timestamp.Equal(p.Timestamp) || len(got.Metrics) != 1 {
		t.Fatalf("decoded = %+v", got)
	}
	if got.Metrics[0].Name != "x" || got.Metrics[0].DataType != TypeInt16 || got.Metrics[0].Value != int64(42) {
		t.Fatalf("metric = %+v", got.Metrics[0])
	}
}

// 序号 0 必须**出现在线上**。
//
// 规范 TCK 项 [tck-id-payloads-sequence-num-always-included] 要求「除
// NDEATH 外每条消息都必须带序号」，而 NBIRTH/DBIRTH 的序号恰好是 0。
// protobuf 是 proto2（`optional uint64 seq = 3`），「字段缺失」与「字段为
// 0」在消费方是两件事：Tahu 生成的代码用 hasSeq() 区分。此前编码走的是
// 「值为 0 就省略」的通用路径，于是每一条 NBIRTH/DBIRTH 都没有 seq 字段。
func TestSeqZeroIsOnTheWire(t *testing.T) {
	raw, err := Payload{Seq: 0}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x18, 0x00} // field 3, wire type 0, value 0
	if !bytes.Equal(raw, want) {
		t.Fatalf("seq=0 payload = % x, want % x", raw, want)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasSeq || got.Seq != 0 {
		t.Fatalf("decoded = %+v, want present seq 0", got)
	}

	// NDEATH 相反：必须不带序号（[tck-id-topics-ndeath-seq]）。
	raw, err = Payload{Seq: 7, OmitSeq: true}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("OmitSeq payload = % x, want empty", raw)
	}
	got, err = Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasSeq {
		t.Fatalf("OmitSeq decode reports seq=%d present", got.Seq)
	}
}

func TestEncodeZeroValues(t *testing.T) {
	// 0 / false / 空串都是合法值：oneof 字段必须显式写出，否则对端会把
	// 它们读成「没有值」。这是 Protobuf 里最容易踩的一个坑。
	cases := []Metric{
		{Name: "b", DataType: TypeBoolean, Value: false},
		{Name: "i", DataType: TypeInt16, Value: int64(0)},
		{Name: "u", DataType: TypeUInt32, Value: uint64(0)},
		{Name: "l", DataType: TypeInt64, Value: int64(0)},
		{Name: "s", DataType: TypeString, Value: ""},
	}
	for _, m := range cases {
		raw, err := Payload{Metrics: []Metric{m}}.Encode()
		if err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		got, err := Decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if len(got.Metrics) != 1 || got.Metrics[0].Value == nil {
			t.Fatalf("%s decoded as no value: %+v", m.Name, got.Metrics)
		}
	}
}

func TestAllDataTypesRoundTrip(t *testing.T) {
	cases := []Metric{
		{Name: "i8", DataType: TypeInt8, Value: int64(-128)},
		{Name: "i16", DataType: TypeInt16, Value: int64(-32768)},
		{Name: "i32", DataType: TypeInt32, Value: int64(-2147483648)},
		{Name: "i64", DataType: TypeInt64, Value: int64(math.MinInt64)},
		{Name: "u8", DataType: TypeUInt8, Value: uint64(255)},
		{Name: "u16", DataType: TypeUInt16, Value: uint64(65535)},
		{Name: "u32", DataType: TypeUInt32, Value: uint64(4294967295)},
		{Name: "u64", DataType: TypeUInt64, Value: uint64(math.MaxUint64)},
		{Name: "f", DataType: TypeFloat, Value: float32(1.5)},
		{Name: "d", DataType: TypeDouble, Value: float64(-2.25)},
		{Name: "b", DataType: TypeBoolean, Value: true},
		{Name: "s", DataType: TypeString, Value: "hello"},
		{Name: "t", DataType: TypeText, Value: "文本"},
		{Name: "bytes", DataType: TypeBytes, Value: []byte{0x00, 0xFF}},
		{Name: "ts", DataType: TypeDateTime, Value: int64(1700000000000)},
		{Name: "null", DataType: TypeDouble, IsNull: true},
	}
	raw, err := Payload{Metrics: cases}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Metrics) != len(cases) {
		t.Fatalf("metrics = %d, want %d", len(got.Metrics), len(cases))
	}
	for i, want := range cases {
		m := got.Metrics[i]
		if m.Name != want.Name || m.DataType != want.DataType || m.IsNull != want.IsNull {
			t.Fatalf("%s = %+v, want %+v", want.Name, m, want)
		}
		if want.IsNull {
			if m.Value != nil {
				t.Fatalf("%s null = %v", want.Name, m.Value)
			}
			continue
		}
		if b, ok := want.Value.([]byte); ok {
			if !bytes.Equal(m.Value.([]byte), b) {
				t.Fatalf("%s = % x, want % x", want.Name, m.Value, b)
			}
			continue
		}
		if m.Value != want.Value {
			t.Fatalf("%s = %#v (%T), want %#v (%T)", want.Name, m.Value, m.Value, want.Value, want.Value)
		}
	}
}

func TestBytesValueAndProperties(t *testing.T) {
	// Bytes 有独立枚举（17），值走 bytes_value 字段，保持原始字节不被
	// UTF-8 改写。属性是平行数组，必须按下标一一对应。
	m := Metric{
		Name: "raw", DataType: TypeBytes, Value: []byte{0x00, 0xFF, 0x7F},
		Properties: map[string]string{"engUnit": "m³", "quality": "stale"},
	}
	raw, err := Payload{Metrics: []Metric{m}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Metrics[0].Value.([]byte), []byte{0x00, 0xFF, 0x7F}) {
		t.Fatalf("bytes = % x", got.Metrics[0].Value)
	}
	if got.Metrics[0].DataType != TypeBytes {
		t.Fatalf("datatype = %v", got.Metrics[0].DataType)
	}
	wantProps := map[string]string{"engUnit": "m³", "quality": "stale"}
	for k, v := range wantProps {
		if got.Metrics[0].Properties[k] != v {
			t.Fatalf("property %q = %q, want %q", k, got.Metrics[0].Properties[k], v)
		}
	}
	// 属性顺序必须稳定（map 遍历顺序是随机的）。
	raw2, err := Payload{Metrics: []Metric{m}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Fatal("property encoding is not deterministic")
	}
}

// 属性集是平行数组（keys=1、values=2），PropertyValue 的字符串是字段 8
// 而不是 6。这两个字段号写错时，本地解码仍然自洽，但对端会静默丢掉属性。
func TestPropertySetGolden(t *testing.T) {
	m := Metric{
		Name: "m", DataType: TypeInt16, Value: int64(1),
		Properties: map[string]string{"u": "V"},
	}
	raw, err := Payload{Metrics: []Metric{m}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// 末尾的 1800 是 seq=0：每条非 NDEATH 消息都必须带序号。
	want, _ := hex.DecodeString("12130a016d20024a0a0a01751205080c42015650011800")
	if !bytes.Equal(raw, want) {
		t.Fatalf("payload = %x, want %x", raw, want)
	}
}

func TestDecodeErrors(t *testing.T) {
	bad := map[string][]byte{
		"empty key":        {0x00},
		"truncated key":    {0x80},
		"truncated varint": {0x08, 0x80},
		"truncated bytes":  {0x12, 0x05, 0x0A},
		"group wire type":  {0x0B},
		"fixed32 short":    {0x65, 0x01},
	}
	for name, raw := range bad {
		if _, err := Decode(raw); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}

	// 声明 Int16 却带 double_value：线类型与 datatype 冲突，必须拒绝。
	raw, err := Payload{Metrics: []Metric{{Name: "x", DataType: TypeDouble, Value: float64(1)}}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// 把 datatype 从 Double(10) 改成 Int16(2)。datatype 字段是 0x20 0x0A。
	i := bytes.Index(raw, []byte{0x20, 0x0A})
	if i < 0 {
		t.Fatalf("datatype field not found in % x", raw)
	}
	raw[i+1] = 0x02
	if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "declares Int16") {
		t.Fatalf("conflicting value type: %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := []Metric{
		{DataType: TypeInt16, Value: int64(1)},
		{Name: "x", DataType: TypeInt16, Value: int32(1)},
		{Name: "x", DataType: TypeDouble, Value: float32(1)},
		{Name: "x", DataType: TypeString, Value: 1},
		{Name: "x", DataType: TypeBytes, Value: "not bytes"},
		{Name: "x", DataType: TypeBoolean, Value: int64(1)},
		{Name: "x", DataType: TypeUnknown, Value: struct{}{}},
	}
	for _, m := range bad {
		if _, err := (Payload{Metrics: []Metric{m}}).Encode(); err == nil {
			t.Fatalf("%+v accepted", m)
		}
	}
}

func TestUnknownFieldsSkipped(t *testing.T) {
	// 对端升级 schema 后多出来的字段不能让我们整帧解析失败。
	raw, err := Payload{Metrics: []Metric{{Name: "x", DataType: TypeInt16, Value: int64(1)}}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// 在末尾加字段 99、varint 7。
	raw = append(raw, 0x98, 0x06, 0x07)
	if _, err := Decode(raw); err != nil {
		t.Fatalf("unknown field rejected: %v", err)
	}
}

func TestDataTypeString(t *testing.T) {
	if TypeInt16.String() != "Int16" || TypeUnknown.String() != "Unknown" || TypeBytes.String() != "Bytes" {
		t.Fatal("datatype names")
	}
	if got := DataType(99).String(); !strings.Contains(got, "99") {
		t.Fatalf("unknown datatype = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Metric 解码加严
// ---------------------------------------------------------------------------

// pbMetric 手工拼一条 Metric 子消息，让测试能写出「编码器永远不会产生、
// 但对端可能发来」的畸形报文。
//
// 直接用 Encode 造不出这些用例：编码器（validate）已经把非法组合挡掉了，
// 拿它生成夹具只能证明「自己发的自己能读」，证明不了「别人的坏报文会被
// 拒绝」。所以这里逐字节构造。
func pbMetric(fields ...[]byte) []byte {
	var body []byte
	for _, f := range fields {
		body = append(body, f...)
	}
	return body
}

// pbUvarint 编码一个 protobuf varint。field 号大于 15 时标签本身就要占
// 两个字节（16<<3|0 = 128），所以标签也必须走同一条编码，不能直接转 byte。
func pbUvarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func pbTag(field, wire int) []byte {
	return pbUvarint(uint64(field)<<3 | uint64(wire))
}

func pbVarint(field int, v uint64) []byte {
	out := pbTag(field, wireVarint)
	return append(out, pbUvarint(v)...)
}

func pbFixed32(field int, bits uint32) []byte {
	out := pbTag(field, wireFixed32)
	return append(out, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
}

func pbFixed64(field int, bits uint64) []byte {
	out := pbTag(field, wireFixed64)
	for i := 0; i < 8; i++ {
		out = append(out, byte(bits>>(8*uint(i))))
	}
	return out
}

func pbBytes(field int, b []byte) []byte {
	out := pbTag(field, wireBytes)
	out = append(out, pbUvarint(uint64(len(b)))...)
	return append(out, b...)
}

func pbName(name string) []byte { return pbBytes(1, []byte(name)) }

// datatype 在 Sparkplug 的 Metric 里是字段 4（1=name 2=alias 3=timestamp
// 4=datatype ... 10..16=值）。
func pbType(dt DataType) []byte { return pbVarint(4, uint64(dt)) }

// pbPayload 把 Metric 子消息包进 Payload（字段 2 = metrics）。
func pbPayload(metrics ...[]byte) []byte {
	var out []byte
	for _, m := range metrics {
		out = append(out, pbBytes(2, m)...)
	}
	return out
}

// oneof 只能有一个成员。同时带两个值时，protobuf 语义是「后者覆盖前者」，
// 而覆盖顺序是解析器实现细节——两个实现可能读到不同的值。必须拒绝。
func TestDecodeMetricRejectsMultipleValueFields(t *testing.T) {
	raw := pbPayload(pbMetric(
		pbName("x"),
		pbType(TypeInt32),
		pbVarint(10, 5),      // int_value
		pbFixed64(13, 1<<62), // double_value（同一个 oneof）
	))
	if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("two value fields accepted: %v", err)
	}
}

// 每个承载字段的线类型必须与字段号匹配（12=fixed32、13=fixed64、
// 其余 varint/bytes）。线类型错位说明对端的 schema 与我们对不上。
func TestDecodeMetricRejectsWireTypeMismatch(t *testing.T) {
	cases := map[string][]byte{
		// float_value 必须是 fixed32，这里用 varint。
		"float as varint": pbPayload(pbMetric(pbName("x"), pbType(TypeFloat), pbVarint(12, 1))),
		// double_value 必须是 fixed64，这里用 fixed32。
		"double as fixed32": pbPayload(pbMetric(pbName("x"), pbType(TypeDouble), pbFixed32(13, 1))),
		// int_value 必须是 varint，这里用 bytes。
		"int as bytes": pbPayload(pbMetric(pbName("x"), pbType(TypeInt32), pbBytes(10, []byte{1}))),
		// boolean_value 必须是 varint。
		"bool as bytes": pbPayload(pbMetric(pbName("x"), pbType(TypeBoolean), pbBytes(14, []byte{1}))),
		// string_value 必须是 bytes。
		"string as varint": pbPayload(pbMetric(pbName("x"), pbType(TypeString), pbVarint(15, 1))),
		// bytes_value 必须是 bytes。
		"bytes as varint": pbPayload(pbMetric(pbName("x"), pbType(TypeBytes), pbVarint(16, 1))),
		// long_value 必须是 varint。
		"long as fixed64": pbPayload(pbMetric(pbName("x"), pbType(TypeInt64), pbFixed64(11, 1))),
	}
	for name, raw := range cases {
		if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "wire type") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// 声明的 datatype 必须与承载字段一致。
//
// `int_value` 是 int32 语义、`long_value` 是 int64 语义。声明 Int64 却用
// int_value 会让超过 32 位的值被静默截断——编码侧 validate 已经拒绝这种
// 组合，解码侧不校验就成了「自己发不出去，别人发过来照收」。
func TestDecodeMetricRejectsTypeFieldMismatch(t *testing.T) {
	cases := map[string][]byte{
		"Int64 in int_value":    pbPayload(pbMetric(pbName("x"), pbType(TypeInt64), pbVarint(10, 1))),
		"Int16 in long_value":   pbPayload(pbMetric(pbName("x"), pbType(TypeInt16), pbVarint(11, 1))),
		"Int32 in float_value":  pbPayload(pbMetric(pbName("x"), pbType(TypeInt32), pbFixed32(12, 0x3f800000))),
		"Double in int_value":   pbPayload(pbMetric(pbName("x"), pbType(TypeDouble), pbVarint(10, 1))),
		"String in bytes_value": pbPayload(pbMetric(pbName("x"), pbType(TypeString), pbBytes(16, []byte("a")))),
		"Bytes in string_value": pbPayload(pbMetric(pbName("x"), pbType(TypeBytes), pbBytes(15, []byte("a")))),
		"Boolean in int_value":  pbPayload(pbMetric(pbName("x"), pbType(TypeBoolean), pbVarint(10, 1))),
	}
	for name, raw := range cases {
		if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "declares") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// protobuf 的 bool 只允许 0/1。别的值说明生产者写坏了——按 true 处理
// 等于替对方猜，而布尔恰恰是最不能猜的类型。
func TestDecodeMetricRejectsInvalidBoolean(t *testing.T) {
	for _, v := range []uint64{2, 7, 255} {
		raw := pbPayload(pbMetric(pbName("x"), pbType(TypeBoolean), pbVarint(14, v)))
		if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "0 or 1") {
			t.Fatalf("boolean_value=%d: %v", v, err)
		}
	}
	// 0 与 1 必须正常解出 false/true。
	for _, c := range []struct {
		v    uint64
		want bool
	}{{0, false}, {1, true}} {
		raw := pbPayload(pbMetric(pbName("x"), pbType(TypeBoolean), pbVarint(14, c.v)))
		p, err := Decode(raw)
		if err != nil {
			t.Fatalf("boolean_value=%d: %v", c.v, err)
		}
		if len(p.Metrics) != 1 || p.Metrics[0].Value != c.want {
			t.Fatalf("boolean_value=%d = %+v", c.v, p.Metrics)
		}
	}
}

// 声明了标量类型却一个承载字段都没有：消费方会把这条指标当成「无值」，
// 而 NBIRTH/DBIRTH 要求每条指标都必须带当前值。
func TestDecodeMetricRejectsMissingValueField(t *testing.T) {
	for _, dt := range []DataType{TypeInt16, TypeUInt32, TypeInt64, TypeFloat,
		TypeDouble, TypeBoolean, TypeString, TypeDateTime, TypeBytes} {
		raw := pbPayload(pbMetric(pbName("x"), pbType(dt)))
		if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "no value field") {
			t.Fatalf("%v without value: %v", dt, err)
		}
	}
	// TypeUnknown 没有承诺类型，不带值是合法的（is_null 的指标就是这样）。
	raw := pbPayload(pbMetric(pbName("x")))
	p, err := Decode(raw)
	if err != nil {
		t.Fatalf("unknown type without value: %v", err)
	}
	if len(p.Metrics) != 1 {
		t.Fatalf("metrics = %+v", p.Metrics)
	}
}

// Int8/16/32、UInt8/16/32 在线上走 int_value（uint32 承载），编码时按补码
// 静默截断：Int8 收 300 发出 44、Int32 收 2^40 发出 0。截断后的值是「看起来
// 合法的另一个值」，消费方无从察觉，比直接报错危险得多。编码侧必须像解码侧
// 的 declaresInt32 一样在越界时拒绝，而不是回绕。
func TestEncodeRejectsIntValueOverflow(t *testing.T) {
	cases := []struct {
		dt   DataType
		bad  int64
		lo   int64
		hi   int64
		badU uint64
	}{
		{TypeInt8, 300, math.MinInt8, math.MaxInt8, 0},
		{TypeInt16, 100000, math.MinInt16, math.MaxInt16, 0},
		{TypeInt32, 1 << 40, math.MinInt32, math.MaxInt32, 0},
		{TypeUInt8, 0, 0, math.MaxUint8, 256},
		{TypeUInt16, 0, 0, math.MaxUint16, 1 << 16},
		{TypeUInt32, 0, 0, math.MaxUint32, 1 << 32},
	}
	for _, c := range cases {
		// 越界的 int64 值必须被拒绝。
		if c.bad > c.hi || c.bad < c.lo {
			m := Metric{Name: "x", DataType: c.dt, Value: c.bad}
			raw, err := Payload{Metrics: []Metric{m}}.Encode()
			if err == nil {
				t.Fatalf("%v: value %d overflowed but Encode accepted it (bytes %x)", c.dt, c.bad, raw)
			}
		}
		// 越界的 uint64 值必须被拒绝。
		if c.badU > uint64(c.hi) {
			m := Metric{Name: "x", DataType: c.dt, Value: c.badU}
			raw, err := Payload{Metrics: []Metric{m}}.Encode()
			if err == nil {
				t.Fatalf("%v: value %d overflowed but Encode accepted it (bytes %x)", c.dt, c.badU, raw)
			}
		}
	}
}

// 边界值（恰好等于 Min/Max）与 Int64/DateTime（走 long_value，不做位宽收敛）
// 必须能正常编码并往返，证明收敛检查没有误伤合法值。
func TestEncodeAcceptsIntBoundariesAndWideTypes(t *testing.T) {
	cases := []struct {
		dt   DataType
		val  int64
		want int64
	}{
		{TypeInt8, math.MinInt8, math.MinInt8},
		{TypeInt8, math.MaxInt8, math.MaxInt8},
		{TypeInt16, math.MinInt16, math.MinInt16},
		{TypeInt16, math.MaxInt16, math.MaxInt16},
		{TypeInt32, math.MinInt32, math.MinInt32},
		{TypeInt32, math.MaxInt32, math.MaxInt32},
		// Int64/DateTime 走 long_value，全域可表达，不做截断。
		{TypeInt64, 1 << 40, 1 << 40},
		{TypeDateTime, 1 << 40, 1 << 40},
	}
	for _, c := range cases {
		raw, err := Payload{Metrics: []Metric{{Name: "x", DataType: c.dt, Value: c.val}}}.Encode()
		if err != nil {
			t.Fatalf("%v: %v", c.dt, err)
		}
		p, err := Decode(raw)
		if err != nil {
			t.Fatalf("decode %v: %v", c.dt, err)
		}
		got, ok := p.Metrics[0].Value.(int64)
		if !ok || got != c.want {
			t.Fatalf("%v: decoded %v(%T), want %d", c.dt, p.Metrics[0].Value, p.Metrics[0].Value, c.want)
		}
	}
}

// PropertySet 是 keys/values 平行数组。空键会被 str() 当缺省字段丢掉、其值却
// 照写，导致 keys 比 values 短一格、后续键值整体错位。空键在 Sparkplug 里没有
// 意义，编码侧必须在 validate 阶段拒绝，而不是发出静默错位的集合。
func TestEncodeRejectsEmptyPropertyKey(t *testing.T) {
	m := Metric{Name: "x", DataType: TypeString, Value: "v",
		Properties: map[string]string{"": "blank", "engUnit": "V"}}
	raw, err := Payload{Metrics: []Metric{m}}.Encode()
	if err == nil {
		t.Fatalf("empty property key accepted; Encode must reject it (bytes %x)", raw)
	}
}

// 正常的多属性必须按键排序后对齐编解码，证明拒绝空键的同时没有破坏合法的
// PropertySet 对齐。
func TestEncodePropertySetRoundTrip(t *testing.T) {
	m := Metric{Name: "x", DataType: TypeString, Value: "v",
		Properties: map[string]string{"engUnit": "V", "quality": "good"}}
	raw, err := Payload{Metrics: []Metric{m}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Metrics[0].Properties
	if got["engUnit"] != "V" || got["quality"] != "good" || len(got) != 2 {
		t.Fatalf("properties misaligned: %+v", got)
	}
}
