package secscodec_test

import (
	"bytes"
	"errors"
	"testing"

	secscodec "uic/internal/codec/secs"
)

// 补充覆盖：诊断用的名字表、长度字段的 2/3 字节分支、取值方法的错误分支，
// 以及 Text 的每个格式分支。这些分支不影响主链路，但它们是自检 CLI 的
// 全部输出，漏测会让「看起来能用」的假象留在人面前。

func TestAllTypeNames(t *testing.T) {
	types := []uint8{
		secscodec.MsgTypeDataMsg, secscodec.MsgTypeSelectReq, secscodec.MsgTypeSelectRsp,
		secscodec.MsgTypeDeselectReq, secscodec.MsgTypeDeselectRsp, secscodec.MsgTypeLinktestReq,
		secscodec.MsgTypeLinktestRsp, secscodec.MsgTypeSeparateReq, secscodec.MsgTypeNotSelectReq,
		secscodec.MsgTypeNotSelectRsp, secscodec.MsgTypeRejectReq, secscodec.MsgTypeRejectRsp,
		secscodec.MsgTypeDataDelimReq, secscodec.MsgTypeDataDelimAck, secscodec.MsgTypePrimaryEnq,
		secscodec.MsgTypePrimaryRecvd, secscodec.MsgTypeSecondaryEnq, secscodec.MsgTypeSecondaryRsp,
		secscodec.MsgTypeSecondaryRecvd, secscodec.MsgTypePingReq, secscodec.MsgTypePingRsp,
	}
	seen := map[string]bool{}
	for _, typ := range types {
		name := secscodec.TypeName(typ)
		if name == "" {
			t.Fatalf("类型 %#x 没有名字", typ)
		}
		if seen[name] {
			t.Fatalf("类型名重复: %s", name)
		}
		seen[name] = true
	}
	if got := secscodec.TypeName(0x7F); got == "" {
		t.Fatal("未知类型也要有名字")
	}
}

func TestAllStatusNames(t *testing.T) {
	codes := []uint16{
		secscodec.StatusOK, secscodec.StatusUnsupported, secscodec.StatusTooMuch,
		secscodec.StatusAlready, secscodec.StatusLinkNotActive, secscodec.StatusDuplicate,
		secscodec.StatusUnknown, secscodec.StatusIllegal, secscodec.StatusWait,
	}
	for _, c := range codes {
		if secscodec.StatusName(c) == "" {
			t.Fatalf("状态 %#x 没有名字", c)
		}
	}
	if got := secscodec.StatusName(0x7777); got == "" {
		t.Fatal("未知状态也要有名字")
	}
}

func TestAllFormatNames(t *testing.T) {
	formats := []secscodec.Format{
		secscodec.FormatList, secscodec.FormatBinary, secscodec.FormatBoolean,
		secscodec.FormatASCII, secscodec.FormatJIS, secscodec.FormatI8,
		secscodec.FormatI1, secscodec.FormatI2, secscodec.FormatI4,
		secscodec.FormatF8, secscodec.FormatF4, secscodec.FormatU8,
		secscodec.FormatU1, secscodec.FormatU2, secscodec.FormatU4,
	}
	for _, f := range formats {
		if secscodec.FormatName(f) == "" {
			t.Fatalf("格式 %d 没有名字", f)
		}
	}
	if got := secscodec.FormatName(secscodec.Format(63)); got == "" {
		t.Fatal("未知格式也要有名字")
	}
}

// TestItemLengthBoundaries 覆盖 255/256 与 65535/65536 四个边界：
// 它们是「长度字节数从 1 跳到 2、从 2 跳到 3」的位置。
func TestItemLengthBoundaries(t *testing.T) {
	for _, n := range []int{255, 256, 65535, 65536} {
		body := bytes.Repeat([]byte{0x7F}, n)
		raw, err := secscodec.EncodeItem(secscodec.Binary(body))
		if err != nil {
			t.Fatalf("n=%d: encode: %v", n, err)
		}
		got, used, err := secscodec.DecodeItem(raw)
		if err != nil {
			t.Fatalf("n=%d: decode: %v", n, err)
		}
		if used != len(raw) {
			t.Fatalf("n=%d: used=%d len=%d", n, used, len(raw))
		}
		b, err := got.Bytes()
		if err != nil || len(b) != n {
			t.Fatalf("n=%d: bytes=%d err=%v", n, len(b), err)
		}
	}
}

// TestListItemCountBoundary 覆盖 L 项「子项个数」本身跨过 255 的分支：
// 这里是长度字段的第二档，与字节长度共用同一套编码。
func TestListItemCountBoundary(t *testing.T) {
	items := make([]*secscodec.Item, 260)
	for i := range items {
		items[i] = secscodec.Int(secscodec.FormatU1, 1)
	}
	raw, err := secscodec.EncodeItem(secscodec.List(items...))
	if err != nil {
		t.Fatal(err)
	}
	got, used, err := secscodec.DecodeItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(raw) {
		t.Fatalf("used=%d len=%d", used, len(raw))
	}
	if got.ChildrenCount() != 260 {
		t.Fatalf("子项 = %d, want 260", got.ChildrenCount())
	}
}

func TestItemU8AndI8(t *testing.T) {
	it := secscodec.Int(secscodec.FormatU8, 1<<40)
	raw, err := secscodec.EncodeItem(it)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := secscodec.DecodeItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	v, err := got.U8()
	if err != nil || v != 1<<40 {
		t.Fatalf("U8 = %d err=%v", v, err)
	}
	if _, err := got.I8(); !errors.Is(err, secscodec.ErrFormatMismatch) {
		t.Fatalf("U8.I8() 应报 ErrFormatMismatch，得到 %v", err)
	}
}

// TestItemRawLengthMismatch 构造一个「格式说 U2、但只带 1 字节」的项，
// 断言取值时报错而不是越界读。
func TestItemRawLengthMismatch(t *testing.T) {
	it := &secscodec.Item{Format: secscodec.FormatU2, Raw: []byte{0x01}}
	if _, err := it.U2(); err == nil {
		t.Fatal("宽度不符必须失败")
	}
	empty := &secscodec.Item{Format: secscodec.FormatU1}
	if _, err := empty.U1(); err == nil {
		t.Fatal("Raw 为空必须失败")
	}
}

func TestItemTextAllFormats(t *testing.T) {
	cases := []struct {
		item *secscodec.Item
		want string
	}{
		{secscodec.Int(secscodec.FormatI1, -1), "I1 -1"},
		{secscodec.Int(secscodec.FormatI4, -100000), "I4 -100000"},
		{secscodec.Int(secscodec.FormatI8, -1), "I8 -1"},
		{secscodec.Int(secscodec.FormatU2, 65535), "U2 65535"},
		{secscodec.Int(secscodec.FormatU4, 7), "U4 7"},
		{secscodec.Int(secscodec.FormatU8, 9), "U8 9"},
		{secscodec.Float(secscodec.FormatF4, 1.5), "F4 1.5"},
		{secscodec.Binary([]byte{0x01}), "B 01"},
		{secscodec.ASCII("x"), `A "x"`},
	}
	for _, tc := range cases {
		if got := tc.item.Text(); got != tc.want {
			t.Errorf("Text() = %q, want %q", got, tc.want)
		}
	}
	// JIS 与 A 同名渲染
	j := &secscodec.Item{Format: secscodec.FormatJIS, Str: "jp"}
	if got := j.Text(); got != `J "jp"` {
		t.Errorf("JIS Text() = %q", got)
	}
	// 未知格式
	u := &secscodec.Item{Format: secscodec.Format(63)}
	if got := u.Text(); got == "" {
		t.Error("未知格式也要渲染")
	}
}

func TestDecodeFirstItemLengthOverflow(t *testing.T) {
	// 伪造一个长度字段极大、实际数据很少的项
	if _, _, err := secscodec.DecodeItem([]byte{0x23, 0xFF, 0xFF, 0xFF}); err == nil {
		t.Fatal("超大长度必须失败")
	}
	// 长度字段自身被截断
	if _, _, err := secscodec.DecodeItem([]byte{0x22, 0xFF}); err == nil {
		t.Fatal("2 字节长度字段截断必须失败")
	}
	if _, _, err := secscodec.DecodeItem([]byte{0x23, 0xFF, 0xFF}); err == nil {
		t.Fatal("3 字节长度字段截断必须失败")
	}
}

func TestFrameDataLenShortHeader(t *testing.T) {
	if _, err := secscodec.FrameDataLen(make([]byte, 5)); err == nil {
		t.Fatal("短头必须失败")
	}
}
