package secscodec

import (
	"strings"
	"testing"
)

// 长度上限与畸形长度：这些是「对端（或噪声）声称一个巨大长度」时的护栏。
// 没有它们，一条 12 字节的畸形帧就能让进程申请几个 GiB。

func TestMaxPayloadGuard(t *testing.T) {
	head := make([]byte, FrameLen)
	// 长度前缀写 0xFFFFFFFF：必被上限拦住。
	putLen(head, 0xFFFFFFFF)
	if _, err := FrameDataLen(head); err == nil {
		t.Fatal("长度 0xFFFFFFFF 必须被上限拦住")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("错误信息应说明上限: %v", err)
	}

	// 边界：载荷正好等于上限放行（前缀 = MaxPayload + 10 字节头），超一拒绝。
	atMax := make([]byte, FrameLen)
	putLen(atMax, MaxPayload+headerLen)
	if n, err := FrameDataLen(atMax); err != nil || n != MaxPayload {
		t.Fatalf("恰好上限应放行: %d %v", n, err)
	}
	over := make([]byte, FrameLen)
	putLen(over, MaxPayload+headerLen+1)
	if _, err := FrameDataLen(over); err == nil {
		t.Fatal("超上限必须拒绝")
	}
}

func putLen(b []byte, n int) {
	b[0] = byte(n >> 24)
	b[1] = byte(n >> 16)
	b[2] = byte(n >> 8)
	b[3] = byte(n)
}

func TestEncodeRejectsOversizePayload(t *testing.T) {
	m := Message{Stream: 1, Function: 1, MsgType: MsgTypeDataMsg, WBit: true}
	m.Payload = make([]byte, MaxPayload+1)
	if _, err := Encode(m); err == nil {
		t.Fatal("超大载荷必须拒绝编码")
	}
}

func TestParseFrameRejectsOversize(t *testing.T) {
	b := make([]byte, FrameLen)
	b[0], b[1] = 0x81, 0x03
	putLen(b, MaxPayload+1)
	b[10], b[11] = MsgTypeDataMsg, 1
	if _, _, err := ParseFrame(b); err == nil {
		t.Fatal("ParseFrame 必须拒绝超大长度")
	}
}

// L 项声明一个天文数字的子项数：不能按它预分配。
func TestDecodeListAbsurdCount(t *testing.T) {
	// L, 3 字节长度 = 16777215，后面一个子项都没有。
	b := []byte{byte(FormatList)<<2 | 0x03, 0xFF, 0xFF, 0xFF}
	if _, _, err := DecodeItem(b); err == nil {
		t.Fatal("空列表声明 16M 个子项必须失败")
	}
}

// 长度字节数 00 与 01 都表示 1 字节（历史写法）。
func TestDecodeLenRawZeroTreatedAsOne(t *testing.T) {
	// U1 项：格式字节低 2 位写 00，长度仍是 1 字节。
	b := []byte{byte(FormatU1) << 2, 0x01, 0x2A}
	it, used, err := DecodeItem(b)
	if err != nil {
		t.Fatalf("raw=0 应视同 1 字节长度: %v", err)
	}
	if used != 3 {
		t.Fatalf("消费 %d 字节, want 3", used)
	}
	if v, _ := it.U1(); v != 42 {
		t.Fatalf("值 = %d, want 42", v)
	}
}

func TestDecodeLenInvalidCount(t *testing.T) {
	// 长度字节数只能是 1..3；这里造一个 raw=3 但内容被截断的项。
	b := []byte{byte(FormatU4)<<2 | 0x03, 0x00, 0x01}
	if _, _, err := DecodeItem(b); err == nil {
		t.Fatal("3 字节长度只给 2 字节必须失败")
	}
}

func TestDecodeItemEmpty(t *testing.T) {
	if _, _, err := DecodeItem(nil); err == nil {
		t.Fatal("空输入必须失败")
	}
}

// 整数项长度不是宽度整数倍：不是合法的 SECS-II 标量数组。
func TestItemIntegerLengthMismatch(t *testing.T) {
	it := &Item{Format: FormatU4, Raw: []byte{0, 0, 1}} // 3 字节，U4 要 4
	if _, err := it.U4(); err == nil {
		t.Fatal("长度不是 4 必须失败")
	}
	it8 := &Item{Format: FormatU8, Raw: []byte{0, 1, 2, 3}}
	if _, err := it8.U8(); err == nil {
		t.Fatal("U8 长度 4 必须失败")
	}
	f4 := &Item{Format: FormatF4, Raw: []byte{0, 1}}
	if _, err := f4.F4(); err == nil {
		t.Fatal("F4 长度 2 必须失败")
	}
	f8 := &Item{Format: FormatF8, Raw: []byte{0}}
	if _, err := f8.F8(); err == nil {
		t.Fatal("F8 长度 1 必须失败")
	}
}

// BOOLEAN 项宽度不是 1：标准规定 1 字节，多字节是畸形。
func TestItemBoolWrongWidth(t *testing.T) {
	it := &Item{Format: FormatBoolean, Raw: []byte{1, 0}}
	if _, err := it.Bool(); err == nil {
		t.Fatal("BOOLEAN 宽度 2 必须失败")
	}
	zero := &Item{Format: FormatBoolean, Raw: []byte{0}}
	if v, err := zero.Bool(); err != nil || v {
		t.Fatalf("0 应为 false: %v %v", v, err)
	}
	// 非 0 即 true（标准如此，不要求恰好 1）。
	nonzero := &Item{Format: FormatBoolean, Raw: []byte{0x80}}
	if v, err := nonzero.Bool(); err != nil || !v {
		t.Fatalf("0x80 应为 true: %v %v", v, err)
	}
}

func TestEncodeLenBoundaries(t *testing.T) {
	cases := []struct {
		n     int
		count byte
		len   int
	}{
		{0, 1, 1},
		{255, 1, 1},
		{256, 2, 2},
		{65535, 2, 2},
		{65536, 3, 3},
		{16777215, 3, 3},
	}
	for _, tc := range cases {
		count, body, err := encodeLen(tc.n)
		if err != nil {
			t.Fatalf("encodeLen(%d): %v", tc.n, err)
		}
		if count != tc.count || len(body) != tc.len {
			t.Fatalf("encodeLen(%d) = count %d len %d, want %d/%d", tc.n, count, len(body), tc.count, tc.len)
		}
	}
	if _, _, err := encodeLen(-1); err == nil {
		t.Fatal("负长度必须失败")
	}
	if _, _, err := encodeLen(16777216); err == nil {
		t.Fatal("超过 3 字节上限必须失败")
	}
}

// Int/Float 拿到非定长格式时返回空项（没有 Raw）：解码侧必须因此报格式不符，
// 而不是把「0 字节的 U4」当成 0 悄悄读出来。
func TestIntFloatNonScalarProducesEmptyItem(t *testing.T) {
	for _, f := range []Format{FormatList, FormatASCII, FormatBinary, FormatBoolean} {
		it := Int(f, 1)
		if len(it.Raw) != 0 {
			t.Fatalf("Int(%s) 不该产出 Raw", FormatName(f))
		}
		if _, err := it.U4(); err == nil {
			t.Fatalf("Int(%s) 的空项不该能解成整数", FormatName(f))
		}
		if it := Float(f, 1); len(it.Raw) != 0 {
			t.Fatalf("Float(%s) 不该产出 Raw", FormatName(f))
		}
	}
}

// ASCII/JIS 的超长内容：长度字段只到 3 字节，超过 16 MiB 要明确报错。
func TestEncodeItemTooLong(t *testing.T) {
	it := ASCII(strings.Repeat("x", 16777216))
	if _, err := EncodeItem(it); err == nil {
		t.Fatal("超过 3 字节长度的字符串必须失败")
	}
}

// 格式名兜底：未知格式码打印成 ?(n)，而不是空字符串。
func TestFormatNameUnknown(t *testing.T) {
	if got := FormatName(Format(15)); got != "?(15)" {
		t.Fatalf("FormatName(15) = %q", got)
	}
}
