package modbus

import (
	"bytes"
	"encoding/binary"
	"errors"
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
	res, err := c.DecodeWrite(driver.WriteRequest{PointID: "sp",
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: -1}}, pdu)
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

// TestWordSetBit 保证读-改-写只碰目标位——这是整个功能唯一不能错的一行。
func TestWordSetBit(t *testing.T) {
	const base = uint16(0xA5A5) // 1010 0101 1010 0101
	if got := WordSetBit(base, 3, true); got != 0xA5AD {
		t.Fatalf("set bit3 = %#04x", got)
	}
	if got := WordSetBit(base, 0, false); got != 0xA5A4 {
		t.Fatalf("clear bit0 = %#04x", got)
	}
	if got := WordSetBit(base, 15, false); got != 0x25A5 {
		t.Fatalf("clear bit15 = %#04x", got)
	}
	// 越界位号不改动：调用方应该已经拒绝，但纯函数不该 panic 或静默改错位。
	for _, bit := range []int{-1, 16, 99} {
		if got := WordSetBit(base, bit, true); got != base {
			t.Fatalf("bit %d changed the word to %#04x", bit, got)
		}
	}
}

func TestEncodeReadRegistersGolden(t *testing.T) {
	c := NewCodec("dev-1")
	pdu, err := c.EncodeReadRegisters(5, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x03, 0x00, 0x05, 0x00, 0x01}
	if !bytes.Equal(pdu, want) {
		t.Fatalf("PDU = % x, want % x", pdu, want)
	}
	words, err := c.DecodeRegisters([]byte{0x03, 0x02, 0x12, 0x34}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 1 || words[0] != 0x1234 {
		t.Fatalf("words = %#v", words)
	}
}

func TestDecodeRegistersRejectsBadResponses(t *testing.T) {
	c := NewCodec("dev-1")
	cases := map[string][]byte{
		"empty":       {},
		"byte count":  {0x03, 0x04, 0x00, 0x01},
		"truncated":   {0x03, 0x02, 0x00},
		"wrong func":  {0x04, 0x02, 0x00, 0x01},
		"exception 2": {0x83, 0x02},
	}
	for name, resp := range cases {
		_, err := c.DecodeRegisters(resp, 1)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	if _, err := c.DecodeRegisters([]byte{0x83, 0x02}, 1); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("exception kind = %s", driver.KindOf(err))
	}
}

func TestEncodeWriteRegisterGolden(t *testing.T) {
	c := NewCodec("dev-1")
	pdu, err := c.EncodeWriteRegister(9, 0x8001)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x06, 0x00, 0x09, 0x80, 0x01}
	if !bytes.Equal(pdu, want) {
		t.Fatalf("PDU = % x, want % x", pdu, want)
	}
}

// TestDecodeWriteChecksEchoAddress 防止串包被当成写成功。
func TestDecodeWriteChecksEchoAddress(t *testing.T) {
	c := NewCodec("dev-1")
	req := driver.WriteRequest{
		PointID:  "b3",
		Address:  model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: 3},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}
	if _, err := c.DecodeWrite(req, []byte{0x06, 0x00, 0x06, 0x00, 0x08}); err == nil {
		t.Fatal("mismatched echo address must be a protocol error")
	} else if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %s", driver.KindOf(err))
	}
	if res, err := c.DecodeWrite(req, []byte{0x06, 0x00, 0x05, 0x00, 0x08}); err != nil || !res.OK {
		t.Fatalf("matching echo: %+v %v", res, err)
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

// FC15 编解码：LSB-first 位序、数量/范围上限、回显校验。
func TestWriteMultipleCoils(t *testing.T) {
	c := NewCodec("dev1")
	pdu, err := c.EncodeWriteMultipleCoils(10, []bool{true, false, true, true})
	if err != nil {
		t.Fatal(err)
	}
	if pdu[0] != FCWriteMultipleCoils {
		t.Fatalf("fc = %#x", pdu[0])
	}
	if binary.BigEndian.Uint16(pdu[1:3]) != 10 || binary.BigEndian.Uint16(pdu[3:5]) != 4 {
		t.Fatalf("header = % X", pdu[:5])
	}
	if pdu[5] != 1 || pdu[6] != 0b00001101 {
		t.Fatalf("data = byteCount %d bits %08b, want 1 / 00001101 (LSB-first)", pdu[5], pdu[6])
	}
	// 从站侧解析共享同一语义（slave/sim 的 FC15 处理会再钉一遍）。

	// 回显：地址与数量一致才算成功。
	echo := append([]byte{FCWriteMultipleCoils}, pdu[1:5]...)
	if err := c.DecodeWriteMultipleCoils(10, 4, echo); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if err := c.DecodeWriteMultipleCoils(11, 4, echo); err == nil {
		t.Fatal("地址不一致应报错")
	}
	if err := c.DecodeWriteMultipleCoils(10, 5, echo); err == nil {
		t.Fatal("数量不一致应报错")
	}

	// 越界 / 超限 / 空。
	if _, err := c.EncodeWriteMultipleCoils(0xFFFF, []bool{true, false}); err == nil {
		t.Fatal("越出 0x10000 应报错")
	}
	if _, err := c.EncodeWriteMultipleCoils(0, make([]bool, MaxWriteCoils+1)); err == nil {
		t.Fatal("超过 MaxWriteCoils 应报错")
	}
	if _, err := c.EncodeWriteMultipleCoils(0, nil); err == nil {
		t.Fatal("空批次应报错")
	}
}

// ---------------------------------------------------------------------------
// FC22 掩码写
// ---------------------------------------------------------------------------

// TestMaskWriteWordSemantics 直接把 FC22 的公式钉在纯函数上：
// `结果 = (当前 AND AND_Mask) OR (OR_Mask AND NOT AND_Mask)`。
//
// 这里刻意的检查点是 **OR_Mask 里 AND_Mask 为 1 的位会被忽略**。若实现写成
// `(cur AND and) OR or`（少了 `AND NOT and`），只改一位时结果碰巧相同——
// 因为 or 只有那一位为 1、and 在那一位为 0——所以只有用「两个掩码都多位」
// 的向量才能把这个错误暴露出来。
func TestMaskWriteWordSemantics(t *testing.T) {
	cases := []struct {
		name        string
		bit         int
		v           bool
		and, or     uint16
		cur, expect uint16
	}{
		{"set bit0", 0, true, 0xFFFE, 0x0001, 0x0000, 0x0001},
		{"clear bit0", 0, false, 0xFFFE, 0x0000, 0xFFFF, 0xFFFE},
		{"set bit15 keeps others", 15, true, 0x7FFF, 0x8000, 0x700F, 0xF00F},
		{"clear bit15 keeps others", 15, false, 0x7FFF, 0x0000, 0xFFFF, 0x7FFF},
		// 每个用 MaskWriteWord 得到的掩码都满足 `or & and == 0`，所以
		// 这一条与「set bit15」覆盖的是不同的位位置，而不是不同公式。
		{"set bit3 keeps neighbours", 3, true, 0xFFF7, 0x0008, 0xABC5, 0xABCD},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			and, or, err := MaskWriteWord(tc.bit, tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if and != tc.and || or != tc.or {
				t.Fatalf("masks = and %#04x or %#04x, want and %#04x or %#04x",
					and, or, tc.and, tc.or)
			}
			got := ApplyMaskWrite(tc.cur, and, or)
			if got != tc.expect {
				t.Fatalf("%#04x -> %#04x, want %#04x", tc.cur, got, tc.expect)
			}
		})
	}
}

// TestApplyMaskWriteOverlappingMasks 锁定 ApplyMaskWrite 的「规范公式」而非
// 「朴素公式」。
//
// MaskWriteWord 生成的掩码永远满足 or & and == 0（AND_Mask 目标位为 0、OR_Mask
// 目标位为 1，互不重叠）。在掩码不重叠时，规范公式
//
//	(cur AND and) OR (or AND NOT and)
//
// 与朴素公式
//
//	(cur AND and) OR or
//
// 完全等价——这正是 TestMaskWriteWordSemantics 没能区分两者的原因，也是把
// ApplyMaskWrite「简化」成朴素写法后测试仍全绿的原因。
//
// 两者的差别只在 AND_Mask 与 OR_Mask 有重叠位时出现（见 ApplyMaskWrite 注释）。
// 取 and=0x000F、or=0x0003（位 0/1 同时出现在两个掩码里）、cur=0：
//
//	规范: (0 AND 0x000F) | (0x0003 AND NOT 0x000F) = 0 | 0 = 0x0000
//	朴素: (0 AND 0x000F) | 0x0003                      = 0x0003  ← 错
//
// 朴素写法会把位 0/1 错置 1，因为它没有排除 AND_Mask 已经「保留」的位。FC22
// 允许任意 AND/OR 掩码，规范公式必须在重叠掩码下也正确，这条向量就是为它兜底。
func TestApplyMaskWriteOverlappingMasks(t *testing.T) {
	const cur, and, or = 0x0000, 0x000F, 0x0003
	if got := ApplyMaskWrite(cur, and, or); got != 0x0000 {
		t.Fatalf("ApplyMaskWrite(%#04x, %#04x, %#04x) = %#04x, want 0x0000 "+
			"(重叠位应以 AND_Mask 为准，朴素公式会错置成 0x0003)", cur, and, or, got)
	}
	// 再给一例 cur 非零，确认「保留当前值」这条也被覆盖。
	// cur=0x00F0：位 0-3 由 AND_Mask 保留（=0），or 的位 0/1 被 NOT and 排除。
	// 规范: (0x00F0 AND 0x000F) | (0x0003 AND NOT 0x000F) = 0 | 0 = 0x0000
	if got := ApplyMaskWrite(0x00F0, and, or); got != 0x0000 {
		t.Fatalf("ApplyMaskWrite(0x00F0, %#04x, %#04x) = %#04x, want 0x0000", and, or, got)
	}
}

func TestMaskWriteWordRejectsBadBit(t *testing.T) {
	for _, bit := range []int{-1, 16, 99} {
		if _, _, err := MaskWriteWord(bit, true); err == nil {
			t.Fatalf("bit %d must be rejected", bit)
		}
	}
}

func TestEncodeMaskWriteRegisterGolden(t *testing.T) {
	c := NewCodec("dev-1")
	pdu, err := c.EncodeMaskWriteRegister(0x1234, 0xFFFE, 0x0001)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x16, 0x12, 0x34, 0xFF, 0xFE, 0x00, 0x01}
	if !bytes.Equal(pdu, want) {
		t.Fatalf("PDU = % x, want % x", pdu, want)
	}
	// 回显必须逐字段校验：地址对但掩码顺序反了，写的是别的位。
	if err := c.DecodeMaskWriteRegister(0x1234, 0xFFFE, 0x0001, want); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	bad := append([]byte(nil), want...)
	bad[3], bad[5] = bad[5], bad[3]
	if err := c.DecodeMaskWriteRegister(0x1234, 0xFFFE, 0x0001, bad); err == nil {
		t.Fatal("swapped masks must be rejected")
	}
	bad = append([]byte(nil), want...)
	bad[1], bad[2] = 0x00, 0x01
	if err := c.DecodeMaskWriteRegister(0x1234, 0xFFFE, 0x0001, bad); err == nil {
		t.Fatal("address mismatch must be rejected")
	}
	exc := []byte{0x96, 0x01}
	err = c.DecodeMaskWriteRegister(0x1234, 0xFFFE, 0x0001, exc)
	if !errors.Is(err, ErrIllegalFunction) {
		t.Fatalf("exception 01 must map to ErrIllegalFunction, got %v", err)
	}
}

func TestParseRequestPDUMaskWrite(t *testing.T) {
	fc, addr, qty, vals, err := ParseRequestPDU([]byte{0x16, 0x00, 0x05, 0xFF, 0xF7, 0x00, 0x08})
	if err != nil {
		t.Fatal(err)
	}
	if fc != FCMaskWriteRegister || addr != 5 || qty != 1 {
		t.Fatalf("fc=%#x addr=%d qty=%d", fc, addr, qty)
	}
	if !bytes.Equal(vals, []byte{0xFF, 0xF7, 0x00, 0x08}) {
		t.Fatalf("values = % x", vals)
	}
	if _, _, _, _, err := ParseRequestPDU([]byte{0x16, 0x00, 0x05, 0xFF}); err == nil {
		t.Fatal("short FC22 pdu must fail")
	}
}

func TestRTULengthsForMaskWrite(t *testing.T) {
	// 请求整帧 = 地址 + PDU(7) + CRC(2) = 10；响应同为 10。
	if n, ok := RTURequestLen([]byte{0x01, 0x16}); !ok || n != 10 {
		t.Fatalf("RTURequestLen = %d ok=%v, want 10 true", n, ok)
	}
	if n, err := RTUExpectedResponseLen([]byte{0x16, 0x00, 0x05, 0xFF, 0xF7, 0x00, 0x08}); err != nil || n != 10 {
		t.Fatalf("RTUExpectedResponseLen = %d %v, want 10 nil", n, err)
	}
}
