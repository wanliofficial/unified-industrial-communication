package value

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"uic/internal/model"
)

// 这一层被所有协议共用，所以它的测试必须独立于任何协议：如果哪天有人
// 只改 Modbus 的调用点，protocol 测试仍会通过，而语义已经分叉。

func TestReorderIsInvolution(t *testing.T) {
	raw := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	for _, order := range []model.ByteOrder{"", model.OrderABCD, model.OrderCDAB, model.OrderBADC, model.OrderDCBA} {
		got := Reorder(raw, order)
		if !bytes.Equal(Reorder(got, order), raw) {
			t.Fatalf("%q is not an involution", order)
		}
		if &got[0] == &raw[0] {
			t.Fatalf("%q must not alias the input", order)
		}
	}
	if !bytes.Equal(Reorder(raw, model.OrderCDAB), []byte{3, 4, 1, 2, 7, 8, 5, 6}) {
		t.Fatalf("CDAB = % x", Reorder(raw, model.OrderCDAB))
	}
	if !bytes.Equal(Reorder(raw, model.OrderBADC), []byte{2, 1, 4, 3, 6, 5, 8, 7}) {
		t.Fatalf("BADC = % x", Reorder(raw, model.OrderBADC))
	}
	if !bytes.Equal(Reorder(raw, model.OrderDCBA), []byte{8, 7, 6, 5, 4, 3, 2, 1}) {
		t.Fatalf("DCBA = % x", Reorder(raw, model.OrderDCBA))
	}
}

func TestNumericRoundTrip(t *testing.T) {
	cases := []struct {
		typ model.DataType
		val float64
	}{
		{model.TypeInt16, -32768},
		{model.TypeUint16, 65535},
		{model.TypeInt32, -2147483648},
		{model.TypeUint32, 4294967295},
		{model.TypeInt64, -1 << 62},
		{model.TypeUint64, 1 << 63},
		{model.TypeFloat32, 1.5},
		{model.TypeFloat64, math.Pi},
		{model.TypeBCD16, 1234},
		{model.TypeBCD32, 987654},
	}
	for _, tc := range cases {
		for _, order := range []model.ByteOrder{"", model.OrderCDAB, model.OrderBADC, model.OrderDCBA} {
			raw, err := Encode(model.NumValue(tc.typ, tc.val), tc.typ, order)
			if err != nil {
				t.Fatalf("%s/%q encode: %v", tc.typ, order, err)
			}
			if len(raw) != tc.typ.Bytes() {
				t.Fatalf("%s width = %d, want %d", tc.typ, len(raw), tc.typ.Bytes())
			}
			got, err := Decode(raw, tc.typ, order)
			if err != nil {
				t.Fatalf("%s/%q decode: %v", tc.typ, order, err)
			}
			// float32 有精度损失，按 1e-6 比较。
			if math.Abs(got.Num-tc.val) > 1e-6 {
				t.Fatalf("%s/%q = %v, want %v", tc.typ, order, got.Num, tc.val)
			}
		}
	}
}

func TestDecodeRejectsShortInput(t *testing.T) {
	if _, err := Decode([]byte{1}, model.TypeUint32, ""); err == nil {
		t.Fatal("short input must be rejected instead of zero-padded")
	}
	if _, err := Decode(nil, model.TypeFloat64, ""); err == nil {
		t.Fatal("empty input must be rejected")
	}
}

func TestStringAndBytes(t *testing.T) {
	v, err := Decode([]byte("RUN\x00\x00"), model.TypeString, "")
	if err != nil || v.Text != "RUN" {
		t.Fatalf("string = %+v, %v", v, err)
	}
	v, err = Decode([]byte{0x01, 0x02}, model.TypeBytes, "")
	if err != nil || !bytes.Equal([]byte(v.Text), []byte{1, 2}) {
		t.Fatalf("bytes = %+v, %v", v, err)
	}
	// 宽度由地址决定，编码层不猜。
	if _, err := Encode(model.TextValue(model.TypeString, "RUN"), model.TypeString, ""); err == nil {
		t.Fatal("string encode without explicit width must fail")
	}
}

func TestInvalidBCDIsRejected(t *testing.T) {
	// 0x1A 的高半字节合法、低半字节非法；不能悄悄当成 10。
	if _, err := Decode([]byte{0x1A}, model.TypeBCD16, ""); err == nil {
		t.Fatal("invalid BCD digit must be rejected")
	}
}

// Encode 绝不能「静默改变值」：越界/带小数/NaN/Inf/BCD 溢出都必须在编码前
// 拒绝，而不是用强制转换落地后让设备收到另一个数。这是所有协议共用的
// 收口点，漏一处就会有一条写入路径静默损坏数据（Modbus 与 Mewtocol 曾
// 因各自漏掉 checkRange 而中招）。
func TestEncodeRejectsValuesThatWouldChangeOnTheWire(t *testing.T) {
	bad := []struct {
		typ model.DataType
		val float64
		why string
	}{
		{model.TypeInt16, 40000, "int16 上溢"},
		{model.TypeInt16, -40000, "int16 下溢"},
		{model.TypeInt16, 1.5, "int16 带小数"},
		{model.TypeUint16, 70000, "uint16 上溢"},
		{model.TypeUint16, -1, "uint16 负数"},
		{model.TypeUint16, 3.7, "uint16 带小数"},
		{model.TypeInt32, 3e9, "int32 上溢"},
		{model.TypeUint32, 5e9, "uint32 上溢"},
		{model.TypeUint32, -2, "uint32 负数"},
		{model.TypeInt64, 1e19, "int64 上溢"},
		{model.TypeUint64, 2e19, "uint64 上溢"},
		{model.TypeUint64, -1, "uint64 负数"},
		{model.TypeFloat32, 1e300, "float32 溢出成 +Inf"},
		{model.TypeFloat32, -1e300, "float32 溢出成 -Inf"},
		{model.TypeFloat32, 1e-50, "float32 下溢成 0"},
		{model.TypeFloat32, math.NaN(), "float32 NaN"},
		{model.TypeFloat32, math.Inf(1), "float32 +Inf"},
		{model.TypeFloat64, math.NaN(), "float64 NaN"},
		{model.TypeFloat64, math.Inf(-1), "float64 -Inf"},
		{model.TypeBCD16, 12345, "BCD16 超过 4 位"},
		{model.TypeBCD16, -1, "BCD16 负数"},
		{model.TypeBCD16, 12.5, "BCD16 带小数"},
		{model.TypeBCD32, 123456789, "BCD32 超过 8 位"},
		{model.TypeBCD32, -5, "BCD32 负数"},
	}
	for _, tc := range bad {
		_, err := Encode(model.NumValue(tc.typ, tc.val), tc.typ, "")
		if err == nil {
			t.Fatalf("%s: Encode(%v) 必须拒绝（会静默改变值）", tc.why, tc.val)
		}
		if !errors.Is(err, ErrRange) {
			t.Fatalf("%s: err = %v, 期望 errors.Is(err, ErrRange)", tc.why, err)
		}
	}
}

// 边界值必须放行且往返一致：拒绝不能「宁枉勿纵」地把合法极值也挡掉。
func TestEncodeAcceptsBoundaryValues(t *testing.T) {
	ok := []struct {
		typ model.DataType
		val float64
	}{
		{model.TypeInt16, math.MinInt16},
		{model.TypeInt16, math.MaxInt16},
		{model.TypeUint16, 0},
		{model.TypeUint16, math.MaxUint16},
		{model.TypeInt32, math.MinInt32},
		{model.TypeInt32, math.MaxInt32},
		{model.TypeUint32, math.MaxUint32},
		{model.TypeFloat32, 0},
		{model.TypeFloat32, 3.5},
		{model.TypeFloat64, 0},
		{model.TypeBCD16, 0},
		{model.TypeBCD16, 9999},
		{model.TypeBCD32, 0},
		{model.TypeBCD32, 99999999},
	}
	for _, tc := range ok {
		raw, err := Encode(model.NumValue(tc.typ, tc.val), tc.typ, "")
		if err != nil {
			t.Fatalf("%s=%v 应放行: %v", tc.typ, tc.val, err)
		}
		back, err := Decode(raw, tc.typ, "")
		if err != nil {
			t.Fatalf("%s=%v decode: %v", tc.typ, tc.val, err)
		}
		if back.Num != tc.val {
			t.Fatalf("%s 往返 = %v, want %v", tc.typ, back.Num, tc.val)
		}
	}
}
