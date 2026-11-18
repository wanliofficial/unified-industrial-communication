package secscodec_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	secscodec "uic/internal/codec/secs"
)

// 这一层只做纯函数：无 I/O、无时钟。测试以两条线钉住：
//
//  1. 自己编 → 自己解（对称性）；
//  2. 手算金字节（布局正确性）。
//
// 金字节尤其重要：SECS-II 的格式字节是「6 位格式码 << 2 | 长度字节数」，
// 写成「格式码 | 长度字节数」时前几项还能解出来，后面全部错位。金字节
// 是唯一能把这种错误钉死的东西。

// ---------------------------------------------------------------------------
// HSMS 帧
// ---------------------------------------------------------------------------

func TestHSMSFrameRoundTrip(t *testing.T) {
	cases := []secscodec.Message{
		{SystemBytes: 1, Stream: 1, Function: 13, WBit: true, MsgType: secscodec.MsgTypeDataMsg,
			Payload: []byte{0x01, 0x02, 0x03, 0x04}},
		{SystemBytes: 0x01020304, Stream: 10, Function: 1, MsgType: secscodec.MsgTypeSelectReq},
		{SystemBytes: 42, Stream: 15, Function: 255, MsgType: secscodec.MsgTypeDataMsg},
		{SystemBytes: 7, Stream: 1, Function: 1, WBit: true, MsgType: secscodec.MsgTypeDataMsg},
	}
	for i, m := range cases {
		raw, err := secscodec.Encode(m)
		if err != nil {
			t.Fatalf("case %d: encode: %v", i, err)
		}
		if len(raw) != secscodec.FrameLen+len(m.Payload) {
			t.Fatalf("case %d: frame %d bytes, want %d", i, len(raw), secscodec.FrameLen+len(m.Payload))
		}
		got, err := secscodec.Decode(raw)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		// 控制消息的 Stream/Function 在线上是 0xFF 填充，解码后不参与比较。
		if m.IsData() && (got.Stream != m.Stream || got.Function != m.Function || got.WBit != m.WBit) {
			t.Fatalf("case %d: got %+v, want %+v", i, got, m)
		}
		if got.SystemBytes != m.SystemBytes || got.MsgType != m.MsgType {
			t.Fatalf("case %d: got %+v, want %+v", i, got, m)
		}
		if !bytes.Equal(got.Payload, m.Payload) {
			t.Fatalf("case %d: payload %x, want %x", i, got.Payload, m.Payload)
		}
	}
}

// TestHSMSHeaderGolden 钉住 E37 的 14 字节固定区：4 字节长度前缀（含
// 10 字节头）+ SessionID + Stream/W + Function + PType + SType + SystemBytes。
func TestHSMSHeaderGolden(t *testing.T) {
	m := secscodec.Message{SystemBytes: 0x0A0B0C0D, SessionID: 0x0001, Stream: 2, Function: 14,
		MsgType: secscodec.MsgTypeDataMsg, Payload: []byte{0xAA, 0xBB}}
	raw, err := secscodec.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x0C, // 长度 = 10 字节头 + 2 字节载荷
		0x00, 0x01, // SessionID = 1（设备 ID）
		0x02,                   // Stream = 2（无 W 位）
		0x0E,                   // Function = 14
		0x00,                   // PType = 0（SECS-II）
		0x00,                   // SType = 0（DATA）
		0x0A, 0x0B, 0x0C, 0x0D, // SystemBytes
		0xAA, 0xBB, // 数据
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("frame = % x, want % x", raw, want)
	}
	// Select.req 的金字节：SessionID 固 0xFFFF、字节 2/3 全 0、SType=1。
	sel, err := secscodec.Encode(secscodec.NewSelectReq(0x11))
	if err != nil {
		t.Fatal(err)
	}
	wantSel := []byte{
		0x00, 0x00, 0x00, 0x0A, // 长度 = 10（只有头，无载荷）
		0xFF, 0xFF, // SessionID
		0x00, 0x00, // 头字节 2/3 = 0
		0x00,                   // PType
		0x01,                   // SType = Select.req
		0x00, 0x00, 0x00, 0x11, // SystemBytes
	}
	if !bytes.Equal(sel, wantSel) {
		t.Fatalf("select req = % x, want % x", sel, wantSel)
	}
	// Select.rsp 的状态在头字节 3。
	rsp, err := secscodec.Encode(secscodec.NewSelectRsp(0x11, secscodec.StatusTooMuch))
	if err != nil {
		t.Fatal(err)
	}
	if rsp[7] != byte(secscodec.StatusTooMuch) || rsp[9] != secscodec.MsgTypeSelectRsp {
		t.Fatalf("select rsp header = % x", rsp[4:14])
	}
	if len(rsp) != secscodec.FrameLen {
		t.Fatalf("控制消息不应有载荷：len=%d", len(rsp))
	}
}

func TestHSMSBoundsAndWBit(t *testing.T) {
	if _, err := secscodec.Encode(secscodec.Message{Stream: 0, MsgType: secscodec.MsgTypeDataMsg}); err == nil {
		t.Error("Stream=0 必须失败")
	}
	if _, err := secscodec.Encode(secscodec.Message{Stream: 128, MsgType: secscodec.MsgTypeDataMsg}); err == nil {
		t.Error("Stream=128 必须失败")
	}
	// Stream 的最高位是 W 位：Stream=15 + W=1 时头字节 2 是 0x8F。
	raw, err := secscodec.Encode(secscodec.Message{Stream: 15, WBit: true, MsgType: secscodec.MsgTypeDataMsg})
	if err != nil {
		t.Fatal(err)
	}
	if raw[6] != 0x8F {
		t.Fatalf("W-bit stream byte = %#x, want 0x8F", raw[6])
	}
	if raw[6]&0x7f != 15 {
		t.Fatalf("Stream 解码回 %d, want 15", raw[6]&0x7f)
	}
}

func TestHSMSByteOrderGuard(t *testing.T) {
	raw, err := secscodec.Encode(secscodec.Message{Stream: 1, Function: 1, MsgType: secscodec.MsgTypeDataMsg})
	if err != nil {
		t.Fatal(err)
	}
	raw[8] = 2 // PType：本包只支持 SECS-II(0)
	if _, err := secscodec.Decode(raw); err == nil {
		t.Fatal("PType 2 必须失败")
	}
}

func TestHSMSLengthMismatch(t *testing.T) {
	raw, err := secscodec.Encode(secscodec.Message{Stream: 1, Function: 1,
		MsgType: secscodec.MsgTypeDataMsg, Payload: []byte{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 0x09 // 篡改长度前缀首字节
	if _, err := secscodec.Decode(raw); err == nil {
		t.Fatal("长度字段与实际数据不一致必须失败")
	}
	if _, err := secscodec.FrameDataLen(raw[:5]); err == nil {
		t.Fatal("头不足必须失败")
	}
}

func TestParseFrame(t *testing.T) {
	first, _ := secscodec.Encode(secscodec.Message{Stream: 1, Function: 1,
		MsgType: secscodec.MsgTypeDataMsg, Payload: make([]byte, 20)})
	second, _ := secscodec.Encode(secscodec.Message{Stream: 2, Function: 2,
		MsgType: secscodec.MsgTypeDataMsg, Payload: []byte{9}})
	buf := append(append([]byte{}, first...), second...)

	if _, _, err := secscodec.ParseFrame(buf[:5]); !errors.Is(err, secscodec.ErrTruncated) {
		t.Fatalf("半个头应返回 ErrTruncated，得到 %v", err)
	}
	if _, _, err := secscodec.ParseFrame(buf[:secscodec.FrameLen+3]); !errors.Is(err, secscodec.ErrTruncated) {
		t.Fatalf("半个体应返回 ErrTruncated，得到 %v", err)
	}
	m, used, err := secscodec.ParseFrame(buf)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(first) {
		t.Fatalf("used = %d, want %d", used, len(first))
	}
	if m.Stream != 1 || len(m.Payload) != 20 {
		t.Fatalf("第一帧解错: %+v", m)
	}
	// 继续解第二帧
	m2, used2, err := secscodec.ParseFrame(buf[used:])
	if err != nil {
		t.Fatal(err)
	}
	if m2.Stream != 2 || used2 != len(second) {
		t.Fatalf("第二帧: %+v used=%d", m2, used2)
	}
}

// ---------------------------------------------------------------------------
// 控制消息
// ---------------------------------------------------------------------------

func TestControlMessages(t *testing.T) {
	cases := []struct {
		name string
		m    secscodec.Message
		typ  uint8
	}{
		{"select", secscodec.NewSelectReq(11), secscodec.MsgTypeSelectReq},
		{"deselect", secscodec.NewDeselectReq(12), secscodec.MsgTypeDeselectReq},
		{"linktest", secscodec.NewLinktestReq(13), secscodec.MsgTypeLinktestReq},
		{"separate", secscodec.NewSeparateReq(14), secscodec.MsgTypeSeparateReq},
		{"selectRsp", secscodec.NewSelectRsp(11, secscodec.StatusOK), secscodec.MsgTypeSelectRsp},
		{"deselectRsp", secscodec.NewDeselectRsp(12, secscodec.StatusLinkNotActive), secscodec.MsgTypeDeselectRsp},
		{"linktestRsp", secscodec.NewLinktestRsp(13, secscodec.StatusOK), secscodec.MsgTypeLinktestRsp},
		{"reject", secscodec.NewRejectReq(15, secscodec.MsgTypeDataMsg, 0x01), secscodec.MsgTypeRejectReq},
	}
	for _, tc := range cases {
		raw, err := secscodec.Encode(tc.m)
		if err != nil {
			t.Fatalf("%s: encode: %v", tc.name, err)
		}
		got, err := secscodec.Decode(raw)
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if got.MsgType != tc.typ || got.SystemBytes != tc.m.SystemBytes {
			t.Fatalf("%s: got %+v, want type %#x sys %d", tc.name, got, tc.typ, tc.m.SystemBytes)
		}
		if got.String() == "" {
			t.Fatalf("%s: String() 为空", tc.name)
		}
	}
}

func TestSelectRspStatus(t *testing.T) {
	m := secscodec.NewSelectRsp(1, secscodec.StatusTooMuch)
	if got := m.Status(); got != secscodec.StatusTooMuch {
		t.Fatalf("status = %#x, want %#x", got, secscodec.StatusTooMuch)
	}
	// 空载荷的控制消息：Status 退化为 Unknown 而不是 panic。
	if got := (secscodec.Message{}).Status(); got != secscodec.StatusUnknown {
		t.Fatalf("空载荷 status = %#x, want Unknown", got)
	}
}

func TestTypeAndStatusNames(t *testing.T) {
	if got := secscodec.TypeName(secscodec.MsgTypeLinktestReq); got != "LinktestReq" {
		t.Fatalf("TypeName = %q", got)
	}
	if got := secscodec.TypeName(0xEE); got == "" {
		t.Fatal("未知类型也要渲染")
	}
	if secscodec.StatusName(secscodec.StatusOK) == "" || secscodec.StatusName(0x9999) == "" {
		t.Fatal("StatusName 必须始终可渲染")
	}
}

// ---------------------------------------------------------------------------
// SECS-II 数据项：金字节
// ---------------------------------------------------------------------------

func TestItemGolden(t *testing.T) {
	cases := []struct {
		name string
		item *secscodec.Item
		want []byte
	}{
		{"U1 7", secscodec.Int(secscodec.FormatU1, 7), []byte{0xA5, 0x01, 0x07}},
		{"U2 60000", secscodec.Int(secscodec.FormatU2, 60000), []byte{0xA9, 0x02, 0xEA, 0x60}},
		{"I2 -2", secscodec.Int(secscodec.FormatI2, -2), []byte{0x69, 0x02, 0xFF, 0xFE}},
		{"A3 abc", secscodec.ASCII("abc"), []byte{0x41, 0x03, 'a', 'b', 'c'}},
		{"B2", secscodec.Binary([]byte{0x00, 0xFF}), []byte{0x21, 0x02, 0x00, 0xFF}},
		{"BOOL true", secscodec.Bool(true), []byte{0x25, 0x01, 0x01}},
		{"空 L", secscodec.List(), []byte{0x01, 0x00}},
		{"L2", secscodec.List(secscodec.ASCII("x"), secscodec.Int(secscodec.FormatU1, 7)),
			[]byte{
				0x01, 0x02, // L, 2 项
				0x41, 0x01, 'x',
				0xA5, 0x01, 0x07,
			}},
	}
	for _, tc := range cases {
		raw, err := secscodec.EncodeItem(tc.item)
		if err != nil {
			t.Fatalf("%s: encode: %v", tc.name, err)
		}
		if !bytes.Equal(raw, tc.want) {
			t.Fatalf("%s = % x, want % x", tc.name, raw, tc.want)
		}
		got, used, err := secscodec.DecodeItem(raw)
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if used != len(raw) {
			t.Fatalf("%s: used %d, len %d", tc.name, used, len(raw))
		}
		if got.Format != tc.item.Format {
			t.Fatalf("%s: format %d vs %d", tc.name, got.Format, tc.item.Format)
		}
	}
}

func TestItemLengthBytes(t *testing.T) {
	// 300 字节 → 2 字节长度；格式字节低 2 位 = 2。
	big := bytes.Repeat([]byte("Z"), 300)
	raw, err := secscodec.EncodeItem(secscodec.ASCII(string(big)))
	if err != nil {
		t.Fatal(err)
	}
	if raw[0] != 0x42 {
		t.Fatalf("300 字节格式字节 = %#x, want 0x42（A + 2 字节长度）", raw[0])
	}
	if len(raw) != 1+2+300 {
		t.Fatalf("len = %d, want %d", len(raw), 1+2+300)
	}
	got, used, err := secscodec.DecodeItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(raw) {
		t.Fatalf("used = %d, want %d", used, len(raw))
	}
	if s, err := got.String(); err != nil || s != string(big) {
		t.Fatalf("往返失败: %v", err)
	}

	// 70000 字节 → 3 字节长度；格式字节低 2 位 = 3。
	huge := bytes.Repeat([]byte{0x5A}, 70000)
	raw, err = secscodec.EncodeItem(secscodec.Binary(huge))
	if err != nil {
		t.Fatal(err)
	}
	if raw[0] != 0x23 {
		t.Fatalf("70000 字节格式字节 = %#x, want 0x23（B + 3 字节长度）", raw[0])
	}
	got, _, err = secscodec.DecodeItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := got.Bytes(); err != nil || !bytes.Equal(b, huge) {
		t.Fatal("3 字节长度项往返失败")
	}

	// 空的 1 字节长度是合法的（例如空 ASCII）。
	raw, err = secscodec.EncodeItem(secscodec.ASCII(""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, []byte{0x41, 0x00}) {
		t.Fatalf("空 A 项 = % x", raw)
	}
}

func TestItemNestedList(t *testing.T) {
	nested := secscodec.List(secscodec.List(secscodec.Int(secscodec.FormatU1, 1), secscodec.ASCII("hi")))
	raw, err := secscodec.EncodeItem(nested)
	if err != nil {
		t.Fatal(err)
	}
	got, used, err := secscodec.DecodeItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(raw) {
		t.Fatalf("used = %d, want %d", used, len(raw))
	}
	if got.ChildrenCount() != 1 {
		t.Fatalf("外层子项 = %d", got.ChildrenCount())
	}
	inner, err := got.Child(0)
	if err != nil {
		t.Fatal(err)
	}
	if inner.ChildrenCount() != 2 {
		t.Fatalf("内层子项 = %d", inner.ChildrenCount())
	}
	v, _ := inner.Child(0)
	if n, err := v.U1(); err != nil || n != 1 {
		t.Fatalf("U1 = %d err=%v", n, err)
	}
	s, _ := inner.Child(1)
	if str, err := s.String(); err != nil || str != "hi" {
		t.Fatalf("A = %q err=%v", str, err)
	}
}

func TestDecodeAll(t *testing.T) {
	a, _ := secscodec.EncodeItem(secscodec.Int(secscodec.FormatU1, 1))
	b, _ := secscodec.EncodeItem(secscodec.ASCII("two"))
	items, err := secscodec.DecodeAll(append(a, b...))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Format != secscodec.FormatU1 || items[1].Format != secscodec.FormatASCII {
		t.Fatalf("格式错: %v %v", items[0].Format, items[1].Format)
	}
	if _, err := secscodec.DecodeAll([]byte{0x41, 0x05, 'a'}); err == nil {
		t.Fatal("截断必须失败")
	}
}

// ---------------------------------------------------------------------------
// 取值
// ---------------------------------------------------------------------------

func TestItemGetters(t *testing.T) {
	it, _, err := secscodec.DecodeItem([]byte{0xA5, 0x01, 0x07}) // U1 7
	if err != nil {
		t.Fatal(err)
	}
	if n, err := it.U1(); err != nil || n != 7 {
		t.Fatalf("U1 = %d err=%v", n, err)
	}
	if _, err := it.U2(); !errors.Is(err, secscodec.ErrFormatMismatch) {
		t.Fatalf("U1.U2() 应报 ErrFormatMismatch，得到 %v", err)
	}
	if _, err := it.String(); err == nil {
		t.Fatal("U1.String() 必须失败")
	}
	if _, err := it.Bytes(); err == nil {
		t.Fatal("U1.Bytes() 必须失败")
	}
	if _, err := it.Bool(); err == nil {
		t.Fatal("U1.Bool() 必须失败")
	}
}

func TestItemSignedAndUnsigned(t *testing.T) {
	cases := []struct {
		item *secscodec.Item
		want int64
		read func(*secscodec.Item) (int64, error)
	}{
		{secscodec.Int(secscodec.FormatI1, -128), -128, func(i *secscodec.Item) (int64, error) {
			v, err := i.I1()
			return int64(v), err
		}},
		{secscodec.Int(secscodec.FormatI2, -32768), -32768, func(i *secscodec.Item) (int64, error) {
			v, err := i.I2()
			return int64(v), err
		}},
		{secscodec.Int(secscodec.FormatI4, -2000000000), -2000000000, func(i *secscodec.Item) (int64, error) {
			v, err := i.I4()
			return int64(v), err
		}},
		{secscodec.Int(secscodec.FormatI8, math.MinInt64), math.MinInt64, func(i *secscodec.Item) (int64, error) {
			return i.I8()
		}},
		{secscodec.Int(secscodec.FormatU1, 255), 255, func(i *secscodec.Item) (int64, error) {
			v, err := i.U1()
			return int64(v), err
		}},
		{secscodec.Int(secscodec.FormatU2, 65535), 65535, func(i *secscodec.Item) (int64, error) {
			v, err := i.U2()
			return int64(v), err
		}},
		{secscodec.Int(secscodec.FormatU4, 4294967295), 4294967295, func(i *secscodec.Item) (int64, error) {
			v, err := i.U4()
			return int64(v), err
		}},
	}
	for _, tc := range cases {
		raw, err := secscodec.EncodeItem(tc.item)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := secscodec.DecodeItem(raw)
		if err != nil {
			t.Fatal(err)
		}
		v, err := tc.read(got)
		if err != nil || v != tc.want {
			t.Fatalf("%s = %d err=%v, want %d", secscodec.FormatName(got.Format), v, err, tc.want)
		}
	}
}

func TestItemFloatAndBool(t *testing.T) {
	raw, _ := secscodec.EncodeItem(secscodec.Float(secscodec.FormatF4, 1.5))
	got, _, err := secscodec.DecodeItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := got.F4(); err != nil || math.Abs(float64(v-1.5)) > 1e-6 {
		t.Fatalf("F4 = %v err=%v", v, err)
	}
	if _, err := got.F8(); err == nil {
		t.Fatal("F4.F8() 必须失败")
	}

	raw, _ = secscodec.EncodeItem(secscodec.Float(secscodec.FormatF8, 2.5))
	got, _, _ = secscodec.DecodeItem(raw)
	if v, err := got.F8(); err != nil || v != 2.5 {
		t.Fatalf("F8 = %v err=%v", v, err)
	}

	for _, want := range []bool{true, false} {
		raw, _ := secscodec.EncodeItem(secscodec.Bool(want))
		got, _, err := secscodec.DecodeItem(raw)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := got.Bool(); err != nil || v != want {
			t.Fatalf("BOOL = %v err=%v, want %v", v, err, want)
		}
	}
}

func TestItemChildBounds(t *testing.T) {
	it := secscodec.List(secscodec.Int(secscodec.FormatU1, 1))
	if _, err := it.Child(1); err == nil {
		t.Fatal("越界子项必须失败")
	}
	scalar, _, _ := secscodec.DecodeItem([]byte{0xA5, 0x01, 0x01})
	if _, err := scalar.Child(0); err == nil {
		t.Fatal("标量取子项必须失败")
	}
	if scalar.ChildrenCount() != 0 {
		t.Fatal("标量子项数必须为 0")
	}
}

func TestDecodeItemErrors(t *testing.T) {
	if _, _, err := secscodec.DecodeItem(nil); err == nil {
		t.Fatal("空输入必须失败")
	}
	// 长度字节数 00 表示 1 字节长度，因此 `A4 00` 是合法的「空 BOOLEAN」；
	// 真正非法的只有「声明的长度超过实际数据」。
	// 声明长度超过实际数据
	if _, _, err := secscodec.DecodeItem([]byte{0x41, 0x05, 'a'}); err == nil {
		t.Fatal("截断 A 项必须失败")
	}
	// L 声明 3 项但只有 1 项
	if _, _, err := secscodec.DecodeItem([]byte{0x01, 0x03, 0xA5, 0x01, 0x01}); err == nil {
		t.Fatal("L 子项缺失必须失败")
	}
	// 未知格式码：构造一个带 1 字节长度的项应能解出，但取值会失败
	unknown := &secscodec.Item{Format: secscodec.Format(2)}
	if _, err := secscodec.EncodeItem(unknown); err == nil {
		t.Fatal("未知格式必须编码失败")
	}
}

func TestItemText(t *testing.T) {
	cases := map[string]*secscodec.Item{
		"L[2]":         secscodec.List(secscodec.Int(secscodec.FormatU1, 1), secscodec.ASCII("a")),
		`A "hi"`:       secscodec.ASCII("hi"),
		"BOOLEAN true": secscodec.Bool(true),
		"U1 7":         secscodec.Int(secscodec.FormatU1, 7),
		"I2 -2":        secscodec.Int(secscodec.FormatI2, -2),
		"F8 2.5":       secscodec.Float(secscodec.FormatF8, 2.5),
	}
	for want, it := range cases {
		if got := it.Text(); got != want {
			t.Errorf("Text() = %q, want %q", got, want)
		}
	}
	b := secscodec.Binary([]byte{0xDE, 0xAD})
	if got := b.Text(); got != "B dead" {
		t.Errorf("Binary Text() = %q", got)
	}
}

// ---------------------------------------------------------------------------
// DATA 消息
// ---------------------------------------------------------------------------

func TestDataMessageWrap(t *testing.T) {
	body, _ := secscodec.EncodeItem(secscodec.Int(secscodec.FormatU4, 12345))
	dm := secscodec.DataMessage{SystemBytes: 99, Stream: 1, Function: 13, WBit: true, Body: body}
	m := dm.ToMessage()
	if !m.IsData() || m.MsgType != secscodec.MsgTypeDataMsg {
		t.Fatal("包装失败")
	}
	got, err := secscodec.ParseData(m)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stream != 1 || got.Function != 13 || !got.WBit || got.SystemBytes != 99 {
		t.Fatalf("解包 = %+v", got)
	}
	if !bytes.Equal(got.Body, body) {
		t.Fatal("body 不一致")
	}
	if _, err := secscodec.ParseData(secscodec.NewSelectReq(1)); err == nil {
		t.Fatal("控制消息不能被 ParseData 当成 DATA")
	}
	if m.String() == "" {
		t.Fatal("DATA String() 为空")
	}
}

// TestFullMessageRoundTrip 走一遍「SECS-II item → DATA → HSMS 帧 → 解码」。
func TestFullMessageRoundTrip(t *testing.T) {
	root := secscodec.List(
		secscodec.ASCII("uic"),
		secscodec.Int(secscodec.FormatU4, 1),
		secscodec.List(
			secscodec.Int(secscodec.FormatU1, 2),
			secscodec.ASCII("SVID-1001"),
			secscodec.Float(secscodec.FormatF8, 23.5),
		),
	)
	body, err := secscodec.EncodeItem(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := secscodec.Encode(secscodec.DataMessage{
		SystemBytes: 0x11223344, Stream: 6, Function: 12, WBit: true, Body: body,
	}.ToMessage())
	if err != nil {
		t.Fatal(err)
	}
	frame, used, err := secscodec.ParseFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(raw) {
		t.Fatalf("used %d, want %d", used, len(raw))
	}
	data, err := secscodec.ParseData(frame)
	if err != nil {
		t.Fatal(err)
	}
	if data.Stream != 6 || data.Function != 12 || !data.WBit {
		t.Fatalf("头解错: %+v", data)
	}
	items, err := secscodec.DecodeAll(data.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("顶层项 = %d, want 1", len(items))
	}
	root2 := items[0]
	if root2.ChildrenCount() != 3 {
		t.Fatalf("子项 = %d, want 3", root2.ChildrenCount())
	}
	name, _ := root2.Child(0)
	if s, err := name.String(); err != nil || s != "uic" {
		t.Fatalf("第 1 项 = %q err=%v", s, err)
	}
	third, _ := root2.Child(2)
	if third.ChildrenCount() != 3 {
		t.Fatalf("嵌套 L 子项 = %d", third.ChildrenCount())
	}
	val, _ := third.Child(2)
	if v, err := val.F8(); err != nil || v != 23.5 {
		t.Fatalf("嵌套 F8 = %v err=%v", v, err)
	}
}

func TestFloatGolden(t *testing.T) {
	// F4 1.5 → 0x91(A? no: F4) 头 + 4 字节 3F C0 00 00
	raw, _ := secscodec.EncodeItem(secscodec.Float(secscodec.FormatF4, 1.5))
	want := []byte{0x91, 0x04, 0x3F, 0xC0, 0x00, 0x00}
	if !bytes.Equal(raw, want) {
		t.Fatalf("F4 1.5 = % x, want % x", raw, want)
	}
	var b8 [8]byte
	binary.BigEndian.PutUint64(b8[:], math.Float64bits(2.5))
	raw, _ = secscodec.EncodeItem(secscodec.Float(secscodec.FormatF8, 2.5))
	want = append([]byte{0x81, 0x08}, b8[:]...)
	if !bytes.Equal(raw, want) {
		t.Fatalf("F8 2.5 = % x, want % x", raw, want)
	}
}
