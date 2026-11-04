package modbus

import (
	"bytes"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

func ref(id string, off, length int, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: dt, Length: length,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: off, Length: length, Bit: -1},
	}
}

func TestCRC16Golden(t *testing.T) {
	// Modbus CRC16 的标准检验向量。
	if got := CRC16([]byte("123456789")); got != 0x4B37 {
		t.Fatalf("CRC16 = %#04x, want 0x4b37", got)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	adu := BuildTCP(Frame{Transaction: 0x1234, Unit: 7, PDU: []byte{0x03, 0x00, 0x00, 0x00, 0x01}})
	want := []byte{0x12, 0x34, 0x00, 0x00, 0x00, 0x06, 0x07, 0x03, 0x00, 0x00, 0x00, 0x01}
	if !bytes.Equal(adu, want) {
		t.Fatalf("ADU = % x, want % x", adu, want)
	}
	f, err := ParseTCP(adu)
	if err != nil {
		t.Fatal(err)
	}
	if f.Transaction != 0x1234 || f.Unit != 7 || !bytes.Equal(f.PDU, want[7:]) {
		t.Fatalf("parsed = %+v", f)
	}
	if _, err := ParseTCP(adu[:5]); err == nil {
		t.Fatal("short frame must fail")
	}
}

func TestReadHoldingGolden(t *testing.T) {
	c := NewCodec("dev-1")
	span := driver.ReadSpan{ID: "s1", Points: []driver.PointRef{
		ref("a", 0, 1, model.TypeUint16),
		ref("b", 1, 1, model.TypeUint16),
		ref("c", 2, 2, model.TypeUint32),
	}}
	pdu, err := c.EncodeRead(span)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x03, 0x00, 0x00, 0x00, 0x04}
	if !bytes.Equal(pdu, want) {
		t.Fatalf("PDU = % x, want % x", pdu, want)
	}
	// 1234, 5678, uint32 0x00010002
	resp := []byte{0x03, 0x08, 0x04, 0xD2, 0x16, 0x2E, 0x00, 0x01, 0x00, 0x02}
	samples, err := c.DecodeRead(span, resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d", len(samples))
	}
	for i, want := range []float64{1234, 5678, 65538} {
		if samples[i].Quality != model.QualityGood {
			t.Fatalf("%s quality = %s", samples[i].PointID, samples[i].Quality)
		}
		if samples[i].Value.Num != want {
			t.Fatalf("%s = %v, want %v", samples[i].PointID, samples[i].Value.Num, want)
		}
	}
}

func TestExceptionMapsToAddressError(t *testing.T) {
	c := NewCodec("dev-1")
	span := driver.ReadSpan{Points: []driver.PointRef{ref("a", 0, 1, model.TypeUint16)}}
	_, err := c.DecodeRead(span, ExceptionPDU(FCReadHoldingRegs, 2))
	if err == nil {
		t.Fatal("expected error")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %s, want address", driver.KindOf(err))
	}
	if driver.IsRetryable(err) {
		t.Fatal("address error must not be retryable")
	}
	if driver.CountsTowardBreaker(err) {
		t.Fatal("address error must not trip the breaker")
	}
}

func TestBitAccessInRegister(t *testing.T) {
	c := NewCodec("dev-1")
	p := ref("bit3", 0, 1, model.TypeBool)
	p.Address.Bit = 3
	span := driver.ReadSpan{Points: []driver.PointRef{p}}
	// data word = 0b1000_0000 -> bit7 set, bit3 clear
	samples, err := c.DecodeRead(span, []byte{0x03, 0x02, 0x00, 0x80})
	if err != nil {
		t.Fatal(err)
	}
	if samples[0].Value.Bool {
		t.Fatal("bit3 should be false")
	}
	samples, err = c.DecodeRead(span, []byte{0x03, 0x02, 0x00, 0x08})
	if err != nil {
		t.Fatal(err)
	}
	if !samples[0].Value.Bool {
		t.Fatal("bit3 should be true")
	}
}

func TestEncodeWriteGolden(t *testing.T) {
	c := NewCodec("dev-1")
	pdu, err := c.EncodeWrite(driver.WriteRequest{
		PointID:  "sp",
		Address:  model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
		Value:    model.NumValue(model.TypeUint16, 0x1234),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x06, 0x00, 0x05, 0x12, 0x34}
	if !bytes.Equal(pdu, want) {
		t.Fatalf("PDU = % x, want % x", pdu, want)
	}
	res, err := c.DecodeWrite(driver.WriteRequest{PointID: "sp"}, pdu)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.PointID != "sp" {
		t.Fatalf("result = %+v", res)
	}
}

// 写只读区必须被拒绝：放行会把值写到 holding 的同一偏移，形成「写成功但写错位置」。
func TestEncodeWriteRejectsReadOnlyArea(t *testing.T) {
	c := NewCodec("dev-1")
	for _, area := range []string{"input", "input_register", "ir", "discrete"} {
		_, err := c.EncodeWrite(driver.WriteRequest{
			PointID:  "sp",
			Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: 0, Length: 1, Bit: -1},
			DataType: model.TypeUint16,
			Value:    model.NumValue(model.TypeUint16, 1),
		})
		if err == nil {
			t.Fatalf("area %q should be rejected", area)
		}
		if kind := driver.KindOf(err); kind != driver.ErrPermission {
			t.Fatalf("area %q kind = %s", area, kind)
		}
	}
	// 布尔写只能落在线圈上。
	_, err := c.EncodeWrite(driver.WriteRequest{
		PointID:  "b",
		Address:  model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 1, Bit: 3},
		DataType: model.TypeBool,
		Value:    model.BoolValue(true),
	})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("bool write to register kind = %s (%v)", driver.KindOf(err), err)
	}
}

func TestByteOrder(t *testing.T) {
	raw := []byte{0x11, 0x22, 0x33, 0x44}
	cases := []struct {
		order model.ByteOrder
		want  []byte
	}{
		{model.OrderABCD, []byte{0x11, 0x22, 0x33, 0x44}},
		{model.OrderCDAB, []byte{0x33, 0x44, 0x11, 0x22}},
		{model.OrderBADC, []byte{0x22, 0x11, 0x44, 0x33}},
		{model.OrderDCBA, []byte{0x44, 0x33, 0x22, 0x11}},
	}
	for _, tc := range cases {
		got := Reorder(raw, tc.order)
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("%s = % x, want % x", tc.order, got, tc.want)
		}
		// 对称性：再变换一次应还原。
		if back := Reorder(got, tc.order); !bytes.Equal(back, raw) {
			t.Fatalf("%s not symmetric: % x", tc.order, back)
		}
	}
}
