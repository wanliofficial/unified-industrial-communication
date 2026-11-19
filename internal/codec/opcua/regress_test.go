package opcua

import (
	"math"
	"testing"

	"uic/internal/model"
)

// 本文件是 OPC UA codec 层（Variant 编解码 + 范围校验）的回归测试。
//
// 钉住的都是「静默改写」这一类缺陷：编码/解码时把一个不能精确表示的值
// 悄悄改写成另一个合法值却仍返回成功。工业现场这是最坏的错误——值看起来
// 完全正常，北向留下的是一条错误数据，而日志与配置都看不出问题。

func TestEncodeValueRejectsFractionalIntoInteger(t *testing.T) {
	if _, err := EncodeValue(model.TypeInt16, model.NumValue(model.TypeInt16, 1.5)); err == nil {
		t.Fatal("1.5 into int16 accepted (fraction silently truncated)")
	}
}

func TestEncodeValueRejectsFloat32Overflow(t *testing.T) {
	if _, err := EncodeValue(model.TypeFloat32, model.NumValue(model.TypeFloat32, 1e300)); err == nil {
		t.Fatal("1e300 into float32 accepted (becomes +Inf)")
	}
}

func TestEncodeValueRejectsFloat32Underflow(t *testing.T) {
	// 1e-300 在 float32 下溢成 0：不是调用方给的值，必须拒绝。
	if _, err := EncodeValue(model.TypeFloat32, model.NumValue(model.TypeFloat32, 1e-300)); err == nil {
		t.Fatal("1e-300 into float32 accepted (becomes 0)")
	}
}

func TestEncodeValueRejectsNonFiniteFloat64(t *testing.T) {
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := EncodeValue(model.TypeFloat64, model.NumValue(model.TypeFloat64, x)); err == nil {
			t.Fatalf("%v into float64 accepted", x)
		}
	}
}

func TestEncodeValueInt64BoundaryIsExact(t *testing.T) {
	// 上界必须严格小于 2^63：9223372036854775807.0 这个字面量在 float64 里
	// 会舍入成 2^63，于是 2^63 本身也能通过校验再 int64(n) 溢出成负数。
	if _, err := EncodeValue(model.TypeInt64, model.NumValue(model.TypeInt64, 9223372036854775808.0)); err == nil {
		t.Fatal("2^63 into int64 accepted (overflows to negative)")
	}
	// 边界内（2^63-1 经 float64 舍入后恰为 2^63，无法精确传入，用 2^62 验证
	// 正常大整数仍能通过）必须成功。
	if _, err := EncodeValue(model.TypeInt64, model.NumValue(model.TypeInt64, 1<<62)); err != nil {
		t.Fatalf("2^62 into int64 rejected: %v", err)
	}
}

func TestEncodeValueUint64BoundaryIsExact(t *testing.T) {
	if _, err := EncodeValue(model.TypeUint64, model.NumValue(model.TypeUint64, 18446744073709551616.0)); err == nil {
		t.Fatal("2^64 into uint64 accepted (wraps)")
	}
}

func TestDecodeValueRejectsFractionalIntoInteger(t *testing.T) {
	if _, err := DecodeValue(Variant{Type: TypeDouble, Float: 1.5}, model.TypeInt16); err == nil {
		t.Fatal("double 1.5 decoded into int16 (truncated)")
	}
}

func TestDecodeValueRejectsFloat32Overflow(t *testing.T) {
	// double 1e300 解到 float32 会变成 +Inf，必须报错而不是让 Inf 流进历史库。
	if _, err := DecodeValue(Variant{Type: TypeDouble, Float: 1e300}, model.TypeFloat32); err == nil {
		t.Fatal("double 1e300 decoded into float32 (becomes +Inf)")
	}
}

func TestDecodeValueRejectsFloat32Underflow(t *testing.T) {
	if _, err := DecodeValue(Variant{Type: TypeDouble, Float: 1e-300}, model.TypeFloat32); err == nil {
		t.Fatal("double 1e-300 decoded into float32 (becomes 0)")
	}
}

func TestDecodeValueInRangeFloat32Succeeds(t *testing.T) {
	// 反向：合法范围内的 float32→float64 必须成功，不能误杀。
	v, err := DecodeValue(Variant{Type: TypeFloat, Float: 21.5}, model.TypeFloat32)
	if err != nil {
		t.Fatalf("21.5 into float32 rejected: %v", err)
	}
	if f, _ := v.AsFloat(); f != 21.5 {
		t.Fatalf("got %v", f)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		dt model.DataType
		v  model.Value
	}{
		{model.TypeBool, model.BoolValue(true)},
		{model.TypeInt16, model.NumValue(model.TypeInt16, -1234)},
		{model.TypeUint16, model.NumValue(model.TypeUint16, 60000)},
		{model.TypeInt32, model.NumValue(model.TypeInt32, 100000)},
		{model.TypeUint32, model.NumValue(model.TypeUint32, 4000000000)},
		{model.TypeInt64, model.NumValue(model.TypeInt64, 1<<40)},
		{model.TypeUint64, model.NumValue(model.TypeUint64, 1<<40)},
		{model.TypeFloat32, model.NumValue(model.TypeFloat32, 3.5)},
		{model.TypeFloat64, model.NumValue(model.TypeFloat64, 12345.6789)},
		{model.TypeString, model.TextValue(model.TypeString, "SIM-01")},
	}
	for _, c := range cases {
		variant, err := EncodeValue(c.dt, c.v)
		if err != nil {
			t.Fatalf("encode %s: %v", c.dt, err)
		}
		got, err := DecodeValue(variant, c.dt)
		if err != nil {
			t.Fatalf("decode %s: %v", c.dt, err)
		}
		gf, _ := got.AsFloat()
		wf, _ := c.v.AsFloat()
		if gf != wf {
			t.Fatalf("%s round-trip: got %v want %v", c.dt, gf, wf)
		}
	}
}
