package modbus

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

// pr 造一个跨段内的点位（4 参顺序与 modbus_test.go 的 ref 不同，避免混淆）。
func pr(id string, offset int, dt model.DataType, length int) driver.PointRef {
	return driver.PointRef{ID: id, DataType: dt, Length: length,
		Address: model.Address{Kind: model.AddressRegister, Offset: offset, Length: length, Bit: -1}}
}

// span 造一个单点/多点读跨段。
func span(area string, pts ...driver.PointRef) driver.ReadSpan {
	for i := range pts {
		pts[i].Address.Area = area
	}
	return driver.ReadSpan{ID: "s", Points: pts}
}

// 用读响应构造：给定寄存器值，按大端拼出 PDUs。
func readReqPDU(addr, qty int) []byte {
	pdu := make([]byte, 5)
	pdu[0] = FCReadHoldingRegs
	pdu[1] = byte(addr >> 8)
	pdu[2] = byte(addr)
	pdu[3] = byte(qty >> 8)
	pdu[4] = byte(qty)
	return pdu
}

func readResp(fc byte, regs ...uint16) []byte {
	out := []byte{fc, byte(len(regs) * 2)}
	for _, r := range regs {
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}

// 每种数值类型必须往返一致：decode(encode(x)) == x。
func TestValueRoundTripAllTypes(t *testing.T) {
	cases := []struct {
		t   model.DataType
		num float64
	}{
		{model.TypeInt16, -1234},
		{model.TypeUint16, 54321},
		{model.TypeInt32, -123456},
		{model.TypeUint32, 3456789012},
		{model.TypeInt64, -9007199254740992},
		{model.TypeUint64, 9007199254740992},
		{model.TypeFloat32, 1.5},
		{model.TypeFloat64, 3.141592653589793},
		{model.TypeBCD16, 1234},
		{model.TypeBCD32, 87654321},
	}
	for _, tc := range cases {
		for _, order := range []model.ByteOrder{"", model.OrderABCD, model.OrderCDAB, model.OrderBADC, model.OrderDCBA} {
			raw, err := encodeValue(model.NumValue(tc.t, tc.num), tc.t, order)
			if err != nil {
				t.Fatalf("%s/%q encode: %v", tc.t, order, err)
			}
			if len(raw) != tc.t.Bytes() {
				t.Fatalf("%s/%q encoded %d bytes, want %d", tc.t, order, len(raw), tc.t.Bytes())
			}
			v, err := decodeValue(raw, tc.t, order)
			if err != nil {
				t.Fatalf("%s/%q decode: %v", tc.t, order, err)
			}
			if math.Abs(v.Num-tc.num) > 1e-6*math.Max(1, math.Abs(tc.num)) {
				t.Fatalf("%s/%q round trip = %v, want %v", tc.t, order, v.Num, tc.num)
			}
			if v.Type != tc.t {
				t.Fatalf("%s: type = %s", tc.t, v.Type)
			}
		}
	}
}

// 符号位必须正确：int16 的 0xFFFF 是 -1，而不是 65535。
func TestSignedDecoding(t *testing.T) {
	v, err := decodeValue([]byte{0xFF, 0xFF}, model.TypeInt16, "")
	if err != nil || v.Num != -1 {
		t.Fatalf("int16 -1: %+v %v", v, err)
	}
	v, err = decodeValue([]byte{0xFF, 0xFF}, model.TypeUint16, "")
	if err != nil || v.Num != 65535 {
		t.Fatalf("uint16: %+v %v", v, err)
	}
	v, err = decodeValue([]byte{0xFF, 0xFF, 0xFF, 0xFE}, model.TypeInt32, "")
	if err != nil || v.Num != -2 {
		t.Fatalf("int32 -2: %+v %v", v, err)
	}
}

func TestStringAndBytes(t *testing.T) {
	v, err := decodeValue([]byte{'R', 'U', 'N', 0, 0}, model.TypeString, "")
	if err != nil || v.Text != "RUN" {
		t.Fatalf("string = %+v %v", v, err)
	}
	v, err = decodeValue([]byte{1, 2, 3}, model.TypeBytes, "")
	if err != nil || v.Type != model.TypeBytes || len(v.Text) != 3 {
		t.Fatalf("bytes = %+v %v", v, err)
	}
	// 短缓冲必须报错，而不是静默截断。
	if _, err := decodeValue([]byte{1}, model.TypeUint32, ""); err == nil {
		t.Fatal("short buffer must fail")
	}
	// 字符串不可写。
	if _, err := encodeValue(model.TextValue(model.TypeString, "x"), model.TypeString, ""); err == nil {
		t.Fatal("string write must fail")
	}
	// bool 不是寄存器可写类型。
	if _, err := encodeValue(model.BoolValue(true), model.TypeBool, ""); err == nil {
		t.Fatal("bool encodeValue must fail")
	}
}

func TestBCDDigits(t *testing.T) {
	// 0x12 0x34 → 1234
	v, err := decodeValue([]byte{0x12, 0x34}, model.TypeBCD16, "")
	if err != nil || v.Num != 1234 {
		t.Fatalf("bcd16 = %+v %v", v, err)
	}
	// 非法 BCD 数字（0x1A）必须报错，不能当成 1A 处理。
	if _, err := decodeValue([]byte{0x1A}, model.TypeBCD16, ""); err == nil {
		t.Fatal("invalid BCD digit must fail")
	}
}

// 混合跨段：一次请求覆盖多个点位，按相对偏移回填，且不串位。
func TestDecodeReadMultiPoint(t *testing.T) {
	c := NewCodec("dev")
	// 起始 10：u16=1, u16=2（空隙）, f32=1.5 位于 13..14
	s := span("holding",
		pr("a", 10, model.TypeUint16, 1),
		pr("b", 11, model.TypeUint16, 1),
		pr("f", 13, model.TypeFloat32, 2),
	)
	resp := readResp(FCReadHoldingRegs, 1, 2, 0xFFFF, 0x3FC0, 0x0000)
	got, err := c.DecodeRead(s, resp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("samples = %+v", got)
	}
	if got[0].Value.Num != 1 || got[1].Value.Num != 2 || got[2].Value.Num != 1.5 {
		t.Fatalf("values = %v %v %v", got[0].Value.Num, got[1].Value.Num, got[2].Value.Num)
	}
}

// 响应里的功能码与请求不一致必须是 protocol 错误。
func TestDecodeReadRejectsWrongFunction(t *testing.T) {
	c := NewCodec("dev")
	s := span("holding", pr("a", 0, model.TypeUint16, 1))
	_, err := c.DecodeRead(s, readResp(FCReadInputRegs, 1))
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %s err = %v", driver.KindOf(err), err)
	}
}

func TestDecodeReadTruncated(t *testing.T) {
	c := NewCodec("dev")
	s := span("holding", pr("a", 0, model.TypeUint16, 1))
	// 声称 2 字节，只给 1 字节。
	if _, err := c.DecodeRead(s, []byte{FCReadHoldingRegs, 2, 0x01}); err == nil {
		t.Fatal("truncated payload must fail")
	}
	// 空响应。
	if _, err := c.DecodeRead(s, nil); err == nil {
		t.Fatal("empty response must fail")
	}
	// 只有功能码。
	if _, err := c.DecodeRead(s, []byte{FCReadHoldingRegs}); err == nil {
		t.Fatal("missing byte count must fail")
	}
}

// 异常码映射：02 是地址错误（不可重试、不熔断），04 是设备故障（可重试）。
func TestExceptionMapping(t *testing.T) {
	cases := []struct {
		code      byte
		kind      driver.ErrorKind
		retryable bool
	}{
		{1, driver.ErrProtocol, false},
		{2, driver.ErrAddress, false},
		{3, driver.ErrProtocol, false},
		{4, driver.ErrProtocol, true},
		{6, driver.ErrProtocol, true},
		{0x63, driver.ErrProtocol, false},
	}
	c := NewCodec("dev")
	s := span("holding", pr("a", 0, model.TypeUint16, 1))
	for _, tc := range cases {
		_, err := c.DecodeRead(s, []byte{FCReadHoldingRegs | 0x80, tc.code})
		if err == nil {
			t.Fatalf("code %d: expected error", tc.code)
		}
		if got := driver.KindOf(err); got != tc.kind {
			t.Fatalf("code %d: kind = %s, want %s", tc.code, got, tc.kind)
		}
		if got := driver.IsRetryable(err); got != tc.retryable {
			t.Fatalf("code %d: retryable = %v", tc.code, got)
		}
	}
	// 只有 1 字节的异常响应是畸形帧。
	if _, err := c.DecodeRead(s, []byte{0x83}); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("short exception kind = %s", driver.KindOf(err))
	}
}

// 每个区对应的功能码与位宽必须正确。
func TestAreasMapToCorrectFunction(t *testing.T) {
	cases := map[string]struct {
		fc   byte
		bits bool
	}{
		"coil":             {FCReadCoils, true},
		"coils":            {FCReadCoils, true},
		"discrete":         {FCReadDiscreteInputs, true},
		"discrete_input":   {FCReadDiscreteInputs, true},
		"holding":          {FCReadHoldingRegs, false},
		"holding_register": {FCReadHoldingRegs, false},
		"hr":               {FCReadHoldingRegs, false},
		"input":            {FCReadInputRegs, false},
		"input_register":   {FCReadInputRegs, false},
		"ir":               {FCReadInputRegs, false},
		"COIL":             {FCReadCoils, true},
	}
	c := NewCodec("dev")
	for area, want := range cases {
		pdu, err := c.EncodeRead(span(area, pr("a", 0, model.TypeUint16, 1)))
		if err != nil {
			t.Fatalf("%s: %v", area, err)
		}
		if pdu[0] != want.fc {
			t.Fatalf("%s: fc = %#x, want %#x", area, pdu[0], want.fc)
		}
	}
	if _, err := c.EncodeRead(span("nonsense", pr("a", 0, model.TypeUint16, 1))); err == nil {
		t.Fatal("unknown area must fail")
	}
}

// 超过协议上限（125 寄存器 / 2000 位）必须拒绝，而不是发出非法请求。
func TestEncodeReadEnforcesLimits(t *testing.T) {
	c := NewCodec("dev")
	over := pr("big", 0, model.TypeUint16, 126)
	if _, err := c.EncodeRead(span("holding", over)); err == nil {
		t.Fatal("126 registers must be rejected")
	}
	if _, err := c.EncodeRead(driver.ReadSpan{}); err == nil {
		t.Fatal("empty span must be rejected")
	}
	// 跨区混合必须拒绝。
	mixed := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		{ID: "a", DataType: model.TypeUint16, Length: 1,
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Bit: -1}},
		{ID: "b", DataType: model.TypeUint16, Length: 1,
			Address: model.Address{Kind: model.AddressRegister, Area: "input", Offset: 0, Bit: -1}},
	}}
	if _, err := c.EncodeRead(mixed); err == nil {
		t.Fatal("mixed areas must be rejected")
	}
}

// 非寄存器寻址（符号/对象）不能走 Modbus。
func TestEncodeReadRejectsNonRegisterAddress(t *testing.T) {
	c := NewCodec("dev")
	s := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "a", DataType: model.TypeUint16, Length: 1,
		Address: model.Address{Kind: model.AddressSymbol, Path: "DB1"},
	}}}
	if _, err := c.EncodeRead(s); err == nil {
		t.Fatal("symbol address must be rejected")
	}
}

// 多点写：<=1 寄存器用 FC6，>1 用 FC16，且字节数正确。
func TestEncodeWriteChoosesFunction(t *testing.T) {
	c := NewCodec("dev")
	single, err := c.EncodeWrite(driver.WriteRequest{
		PointID: "a", DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1),
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Bit: -1},
	})
	if err != nil || single[0] != FCWriteSingleRegister {
		t.Fatalf("single = % x %v", single, err)
	}
	multi, err := c.EncodeWrite(driver.WriteRequest{
		PointID: "b", DataType: model.TypeFloat32, Value: model.NumValue(model.TypeFloat32, 1.5),
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 4, Bit: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if multi[0] != FCWriteMultipleRegs {
		t.Fatalf("multi fc = %#x", multi[0])
	}
	if binary.BigEndian.Uint16(multi[3:5]) != 2 {
		t.Fatalf("quantity = %d", binary.BigEndian.Uint16(multi[3:5]))
	}
	if multi[5] != 4 {
		t.Fatalf("byte count = %d", multi[5])
	}
	if !bytes.Equal(multi[6:], []byte{0x3F, 0xC0, 0x00, 0x00}) {
		t.Fatalf("payload = % x", multi[6:])
	}
}

func TestEncodeWriteRejectsNonRegisterAndNonNumeric(t *testing.T) {
	c := NewCodec("dev")
	if _, err := c.EncodeWrite(driver.WriteRequest{
		PointID: "a", DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1),
		Address: model.Address{Kind: model.AddressSymbol, Path: "DB1"},
	}); driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("symbol kind = %s", driver.KindOf(err))
	}
}

func TestDecodeWriteCases(t *testing.T) {
	c := NewCodec("dev")
	req := driver.WriteRequest{PointID: "a"}

	// 正常回显。
	if res, err := c.DecodeWrite(req, []byte{FCWriteSingleRegister, 0, 0, 0, 1}); err != nil || !res.OK {
		t.Fatalf("echo = %+v %v", res, err)
	}
	// 空响应 → protocol（可重试）。
	if _, err := c.DecodeWrite(req, nil); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("empty kind = %s", driver.KindOf(err))
	}
	// 异常 → address。
	if _, err := c.DecodeWrite(req, []byte{FCWriteSingleRegister | 0x80, 2}); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("exception kind = %s", driver.KindOf(err))
	}
	// 功能码不符 → protocol。
	if _, err := c.DecodeWrite(req, []byte{FCReadHoldingRegs, 2, 0, 1}); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("wrong fc kind = %s", driver.KindOf(err))
	}
	// 过短 → protocol。
	if _, err := c.DecodeWrite(req, []byte{FCWriteSingleRegister, 0}); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("short kind = %s", driver.KindOf(err))
	}
}

func TestParseTCP(t *testing.T) {
	f := Frame{Transaction: 0x1234, Unit: 7, PDU: []byte{0x03, 0, 0, 0, 1}}
	raw := BuildTCP(f)
	got, err := ParseTCP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transaction != 0x1234 || got.Unit != 7 || !bytes.Equal(got.PDU, f.PDU) {
		t.Fatalf("parsed = %+v", got)
	}

	if _, err := ParseTCP(make([]byte, 7)); err == nil {
		t.Fatal("too short must fail")
	}
	bad := append([]byte(nil), raw...)
	bad[3] = 1 // protocol id 非 0
	if _, err := ParseTCP(bad); err == nil {
		t.Fatal("non-zero protocol id must fail")
	}
	bad = append([]byte(nil), raw...)
	binary.BigEndian.PutUint16(bad[4:6], 1) // length 非法
	if _, err := ParseTCP(bad); err == nil {
		t.Fatal("bad length must fail")
	}
	// 长度声明超过实际字节数。
	bad = append([]byte(nil), raw...)
	binary.BigEndian.PutUint16(bad[4:6], 200)
	if _, err := ParseTCP(bad); err == nil {
		t.Fatal("length beyond buffer must fail")
	}
}

func TestBuildTCPLength(t *testing.T) {
	f := Frame{Transaction: 1, Unit: 1, PDU: []byte{0x03, 0, 0, 0, 1}}
	raw := BuildTCP(f)
	if len(raw) != 12 {
		t.Fatalf("len = %d", len(raw))
	}
	// length 字段 = PDU 长度 + 1（unit id）。
	if got := binary.BigEndian.Uint16(raw[4:6]); got != 6 {
		t.Fatalf("length = %d", got)
	}
}

// RTU 帧：CRC 正确时能解析，改一个字节就必须拒绝。
func TestRTURoundTrip(t *testing.T) {
	pdu := []byte{0x03, 0x00, 0x00, 0x00, 0x01}
	raw := BuildRTU(1, pdu)
	unit, gotPDU, err := ParseRTU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if unit != 1 || !bytes.Equal(gotPDU, pdu) {
		t.Fatalf("unit = %d pdu = % x", unit, gotPDU)
	}

	corrupt := append([]byte(nil), raw...)
	corrupt[2] ^= 0xFF
	if _, _, err := ParseRTU(corrupt); err == nil {
		t.Fatal("crc mismatch must fail")
	}
	if _, _, err := ParseRTU([]byte{1, 2}); err == nil {
		t.Fatal("short frame must fail")
	}
}

// 金向量：CRC16/MODBUS 的标准测试值。
func TestCRC16KnownValue(t *testing.T) {
	// "123456789" 的 CRC-16/MODBUS = 0x4B37。
	if got := CRC16([]byte("123456789")); got != 0x4B37 {
		t.Fatalf("crc = %#04x, want 0x4b37", got)
	}
	// 01 03 00 00 00 01 → 0x0A84（常见金向量）。
	frame := []byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x01}
	if got := CRC16(frame); got != 0x0A84 {
		t.Fatalf("crc = %#04x, want 0x0a84", got)
	}
}

func TestParseRequestPDU(t *testing.T) {
	fc, addr, qty, vals, err := ParseRequestPDU(readReqPDU(10, 5))
	if err != nil || fc != FCReadHoldingRegs || addr != 10 || qty != 5 || vals != nil {
		t.Fatalf("read: %d %d %d %v %v", fc, addr, qty, vals, err)
	}

	fc, addr, qty, vals, err = ParseRequestPDU([]byte{FCWriteSingleRegister, 0, 3, 0x00, 0x09})
	if err != nil || fc != FCWriteSingleRegister || addr != 3 || qty != 1 || !bytes.Equal(vals, []byte{0, 9}) {
		t.Fatalf("write: %d %d %d % x %v", fc, addr, qty, vals, err)
	}

	fc, addr, qty, vals, err = ParseRequestPDU([]byte{FCWriteMultipleRegs, 0, 0, 0, 2, 4, 0, 1, 0, 2})
	if err != nil || addr != 0 || qty != 2 || !bytes.Equal(vals, []byte{0, 1, 0, 2}) {
		t.Fatalf("multi: %d %d % x %v", addr, qty, vals, err)
	}

	if _, _, _, _, err := ParseRequestPDU(nil); err == nil {
		t.Fatal("empty pdu must fail")
	}
	if _, _, _, _, err := ParseRequestPDU([]byte{0x42}); err == nil {
		t.Fatal("unsupported function must fail")
	}
	if _, _, _, _, err := ParseRequestPDU([]byte{FCReadHoldingRegs, 0}); err == nil {
		t.Fatal("short read pdu must fail")
	}
	if _, _, _, _, err := ParseRequestPDU([]byte{FCWriteMultipleRegs, 0, 0, 0, 2, 9, 0}); err == nil {
		t.Fatal("truncated multi-write must fail")
	}
}

// Reorder 对奇数字节不应越界（BADC 分支逐字节对）。
func TestReorderOddLength(t *testing.T) {
	in := []byte{1, 2, 3}
	out := Reorder(in, model.OrderBADC)
	if !bytes.Equal(out, []byte{2, 1, 3}) {
		t.Fatalf("badc odd = % x", out)
	}
	// 输入不得被修改（调用方可能复用缓冲区）。
	if in[0] != 1 || in[1] != 2 || in[2] != 3 {
		t.Fatalf("input mutated: % x", in)
	}
}

// 单点解码失败不应让整个跨段失败：好点照常回调，坏点带质量码与分类错误。
func TestDecodeReadPerPointFailures(t *testing.T) {
	c := NewCodec("dev")

	// 应答只带了 1 个寄存器，但有个点需要 2 个 → 窗口越界，质量 address_error。
	s := span("holding",
		pr("ok", 0, model.TypeUint16, 1),
		pr("short", 1, model.TypeUint32, 2),
	)
	got, err := c.DecodeRead(s, readResp(FCReadHoldingRegs, 7))
	if err != nil {
		t.Fatalf("span must not fail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %+v", got)
	}
	if got[0].Quality != model.QualityGood || got[0].Value.Num != 7 {
		t.Fatalf("ok point = %+v", got[0])
	}
	if got[1].Quality != model.QualityAddressError {
		t.Fatalf("short point quality = %s", got[1].Quality)
	}
	if driver.KindOf(got[1].Err) != driver.ErrAddress {
		t.Fatalf("short point kind = %s", driver.KindOf(got[1].Err))
	}

	// BCD 位非法 → bad 质量，分类是 protocol。
	s = span("holding", pr("bcd", 0, model.TypeBCD16, 1))
	got, err = c.DecodeRead(s, readResp(FCReadHoldingRegs, 0x1A2B))
	if err != nil {
		t.Fatalf("bcd span must not fail: %v", err)
	}
	if got[0].Quality != model.QualityBad {
		t.Fatalf("bcd quality = %s", got[0].Quality)
	}
}
