package opcua

import (
	"math"
	"testing"
	"time"

	"uic/internal/model"
)

func TestVariantTypeMapping(t *testing.T) {
	ok := map[model.DataType]byte{
		model.TypeBool:    TypeBoolean,
		model.TypeInt16:   TypeInt16,
		model.TypeUint16:  TypeUInt16,
		model.TypeInt32:   TypeInt32,
		model.TypeUint32:  TypeUInt32,
		model.TypeInt64:   TypeInt64,
		model.TypeUint64:  TypeUInt64,
		model.TypeFloat32: TypeFloat,
		model.TypeFloat64: TypeDouble,
		model.TypeString:  TypeString,
		model.TypeBytes:   TypeByteString,
	}
	for dt, want := range ok {
		got, err := VariantType(dt)
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", dt, got, err)
		}
	}
	// BCD 是编码方式而不是数据类型，没有对应的 OPC UA 内置类型。
	// 硬映射会在北向上产生一个语义错误的值，这里必须明确报错。
	if _, err := VariantType(model.TypeBCD16); err == nil {
		t.Fatal("bcd16 must not map to an OPC UA type")
	}
}

func TestDecodeValueScalars(t *testing.T) {
	cases := []struct {
		name string
		v    Variant
		want model.DataType
		text string
		num  float64
	}{
		{"bool", Variant{Type: TypeBoolean, Bool: true}, model.TypeBool, "true", 0},
		{"int16", Variant{Type: TypeInt16, Int: -5}, model.TypeInt16, "-5", -5},
		{"uint16", Variant{Type: TypeUInt16, Uint: 65535}, model.TypeUint16, "65535", 65535},
		{"float to float32", Variant{Type: TypeFloat, Float: 1.5}, model.TypeFloat32, "1.5", 1.5},
		{"double to float64", Variant{Type: TypeDouble, Float: 2.5}, model.TypeFloat64, "2.5", 2.5},
		{"string", Variant{Type: TypeString, Str: "abc"}, model.TypeString, "abc", 0},
		{"datetime as string", Variant{Type: TypeDateTime, Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
			model.TypeString, "2026-01-02T03:04:05Z", 0},
	}
	for _, c := range cases {
		got, err := DecodeValue(c.v, c.want)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Type != c.want {
			t.Fatalf("%s: type %s", c.name, got.Type)
		}
		if got.String() != c.text {
			t.Fatalf("%s: value %q want %q", c.name, got.String(), c.text)
		}
	}
}

// 越界不能截断：Int32 的 70000 写进 uint16 点位，静默截断成 4464 会在
// 北向留下一条看不出错的错误数据。
func TestDecodeValueRangeCheck(t *testing.T) {
	if _, err := DecodeValue(Variant{Type: TypeInt32, Int: 70000}, model.TypeUint16); err == nil {
		t.Fatal("int32 70000 must not fit uint16")
	}
	if _, err := DecodeValue(Variant{Type: TypeInt16, Int: -1}, model.TypeUint16); err == nil {
		t.Fatal("negative must not fit uint16")
	}
	if _, err := DecodeValue(Variant{Type: TypeDouble, Float: math.NaN()}, model.TypeFloat64); err == nil {
		t.Fatal("NaN must be rejected")
	}
	if _, err := DecodeValue(Variant{Type: TypeDouble, Float: math.Inf(1)}, model.TypeFloat64); err == nil {
		t.Fatal("Inf must be rejected")
	}
	// 未指定目标类型时按 Variant 自身类型，不做范围判断。
	if _, err := DecodeValue(Variant{Type: TypeInt32, Int: 70000}, ""); err != nil {
		t.Fatalf("no target type: %v", err)
	}
}

func TestDecodeValueRejectsUnsupportedShapes(t *testing.T) {
	if _, err := DecodeValue(Variant{Type: TypeInt32, Array: true}, model.TypeInt32); err == nil {
		t.Fatal("arrays must be rejected")
	}
	if _, err := DecodeValue(Variant{}, model.TypeInt32); err == nil {
		t.Fatal("empty variant must be rejected")
	}
	if _, err := DecodeValue(Variant{Type: TypeString, Str: "x"}, model.TypeFloat64); err == nil {
		t.Fatal("string to number must be rejected")
	}
	if _, err := DecodeValue(Variant{Type: TypeDouble, Float: 1}, model.TypeBytes); err == nil {
		t.Fatal("number to bytes must be rejected")
	}
}

func TestEncodeValueRoundTrip(t *testing.T) {
	cases := []struct {
		dt model.DataType
		v  model.Value
	}{
		{model.TypeBool, model.BoolValue(true)},
		{model.TypeInt16, model.NumValue(model.TypeInt16, -3)},
		{model.TypeUint16, model.NumValue(model.TypeUint16, 1000)},
		{model.TypeInt32, model.NumValue(model.TypeInt32, -100000)},
		{model.TypeUint32, model.NumValue(model.TypeUint32, 4000000000)},
		{model.TypeInt64, model.NumValue(model.TypeInt64, -1<<40)},
		{model.TypeUint64, model.NumValue(model.TypeUint64, 1<<50)},
		{model.TypeFloat32, model.NumValue(model.TypeFloat32, 1.25)},
		{model.TypeFloat64, model.NumValue(model.TypeFloat64, 2.5)},
		{model.TypeString, model.TextValue(model.TypeString, "pump")},
		{model.TypeBytes, model.BytesValue([]byte{1, 2, 3})},
	}
	for _, c := range cases {
		variant, err := EncodeValue(c.dt, c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.dt, err)
		}
		back, err := DecodeValue(variant, c.dt)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.dt, err)
		}
		if back.String() != c.v.String() {
			t.Fatalf("%s: %q → %+v → %q", c.dt, c.v.String(), variant, back.String())
		}
	}
}

func TestEncodeValueRejectsOutOfRange(t *testing.T) {
	if _, err := EncodeValue(model.TypeUint16, model.NumValue(model.TypeUint16, 70000)); err == nil {
		t.Fatal("70000 must not encode as uint16")
	}
	if _, err := EncodeValue(model.TypeFloat32, model.TextValue(model.TypeString, "x")); err == nil {
		t.Fatal("string must not encode as float32")
	}
	// 非 bool 的数值写入 bool 点位：按 0/1 语义接受。
	v, err := EncodeValue(model.TypeBool, model.NumValue(model.TypeUint16, 1))
	if err != nil || !v.Bool {
		t.Fatalf("numeric to bool: %+v %v", v, err)
	}
}

// float32 必须真的降精度：写 0.1 到 Float 节点，回读是 0.1 的 float32
// 近似值，而不是 float64 的 0.1——否则北向看到的数值与设备里存的
// 不是同一个数。
func TestEncodeFloat32TruncatesPrecision(t *testing.T) {
	v, err := EncodeValue(model.TypeFloat32, model.NumValue(model.TypeFloat32, 0.1))
	if err != nil {
		t.Fatal(err)
	}
	if v.Float == 0.1 {
		t.Fatal("float32 must be rounded through float32")
	}
	if float32(v.Float) != float32(0.1) {
		t.Fatalf("got %v", v.Float)
	}
}

func TestPlatformTypeCoversBuiltins(t *testing.T) {
	for _, vt := range []byte{TypeBoolean, TypeSByte, TypeByte, TypeInt16, TypeUInt16,
		TypeInt32, TypeUInt32, TypeInt64, TypeUInt64, TypeFloat, TypeDouble,
		TypeString, TypeDateTime, TypeByteString} {
		if _, ok := PlatformType(vt); !ok {
			t.Fatalf("builtin type %d unmapped", vt)
		}
	}
	if _, ok := PlatformType(TypeGUID); ok {
		t.Fatal("GUID should not map to a platform type")
	}
}
