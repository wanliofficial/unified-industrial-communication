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
