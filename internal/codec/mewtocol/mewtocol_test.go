package mewtocol

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// 这一层是整个协议最容易出错的地方：ASCII 帧里混着两种进制（站号与地址
// 十进制、数据与 BCC 十六进制）、地址宽度随命令码变化（RD 5 位 / RCC 4 位）、
// 字内低字节在前。每条断言都对应手册帧图上的一个格子。

// ---------------------------------------------------------------------------
// BCC
// ---------------------------------------------------------------------------

// 手册的 BCC 实例：`%01#RCSX0000` 的异或结果是 1D。
//
// 这条测试的价值不在算法（异或只有一行），而在**异或范围的边界**：
// 包含头部 `%`、不含 BCC 与 CR。范围错一位，BCC 就永远对不上，而所有
// 别的测试都可能因为用同一个错函数而一起通过。
func TestBCCMatchesManualExample(t *testing.T) {
	got := BCC([]byte("%01#RCSX0000"))
	if got != 0x1D {
		t.Fatalf("BCC(%q) = %02XH, want 1DH", "%01#RCSX0000", got)
	}
	out := AppendBCC([]byte("%01#RCSX0000"))
	if string(out) != "%01#RCSX00001D\r" {
		t.Fatalf("AppendBCC = %q, want %q", out, "%01#RCSX00001D\r")
	}
}

func TestBCCLowNibblePadding(t *testing.T) {
	// BCC 的低半字节必须补零成两位：0x0A 写成 "0A" 而不是 "A"。
	frame := []byte{'%', '0', '1', '#', 'R', 'D', 'D', '0', '0', '0', '0', '0', '0', '0', '0', '0'}
	b := BCC(frame)
	out := AppendBCC(frame)
	if len(out) != len(frame)+3 {
		t.Fatalf("frame length %d, want %d", len(out), len(frame)+3)
	}
	want := "0123456789ABCDEF"[b>>4]
	if out[len(out)-3] != want {
		t.Fatalf("high nibble = %q, want %q", out[len(out)-3], want)
	}
	if out[len(out)-2] != "0123456789ABCDEF"[b&0x0f] {
		t.Fatalf("low nibble = %q", out[len(out)-2])
	}
}

// ---------------------------------------------------------------------------
// 站号
// ---------------------------------------------------------------------------

func TestStationIsDecimal(t *testing.T) {
	// 站号 16 写 "16"，不是 "10"。这是整个协议里最容易被十六进制习惯
	// 带偏的地方。
	cases := []struct {
		st   Station
		want string
	}{
		{1, "01"}, {9, "09"}, {16, "16"}, {63, "63"}, {99, "99"}, {Broadcast, "FF"},
	}
	for _, c := range cases {
		if got := c.st.Format(); got != c.want {
			t.Errorf("Station(%d).Format() = %q, want %q", int(c.st), got, c.want)
		}
	}
}

func TestStationRange(t *testing.T) {
	for _, st := range []Station{1, 2, 63, 64, 99, Broadcast} {
		if err := st.Validate(); err != nil {
			t.Errorf("Station(%d) should be valid: %v", int(st), err)
		}
	}
	// 0 在协议里没有含义；100 超出两位十进制；负数显然是配置错。
	// （255 不在此列：它正好等于广播站号 0xFF。）
	for _, st := range []Station{0, -1, 100} {
		if err := st.Validate(); err == nil {
			t.Errorf("Station(%d) should be rejected", int(st))
		}
	}
}

func TestParseStation(t *testing.T) {
	for _, s := range []string{"01", "16", "99", "FF", "ff"} {
		if _, err := ParseStation(s); err != nil {
			t.Errorf("ParseStation(%q) failed: %v", s, err)
		}
	}
	st, _ := ParseStation("16")
	if st != 16 {
		t.Fatalf("ParseStation(\"16\") = %d, want 16 (decimal, not hex)", int(st))
	}
	for _, s := range []string{"", "1", "001", "00", "0A", "1A", " 1"} {
		if _, err := ParseStation(s); err == nil {
			t.Errorf("ParseStation(%q) should fail", s)
		}
	}
}

// ---------------------------------------------------------------------------
// 读帧
// ---------------------------------------------------------------------------

func dt(t *testing.T) Area {
	t.Helper()
	a, ok := Lookup("dt")
	if !ok {
		t.Fatal("area dt not found")
	}
	return a
}

func mustArea(t *testing.T, name string) Area {
	t.Helper()
	a, ok := Lookup(name)
	if !ok {
		t.Fatalf("area %q not found", name)
	}
	return a
}

// RD 的正文是 `区码 + 起始(5) + 结束(5)`：5 位十进制，不是「起点 + 个数」。
func TestBuildReadWords(t *testing.T) {
	frame, err := BuildReadWords(1, dt(t), 1105, 1107)
	if err != nil {
		t.Fatal(err)
	}
	// 起始 01105、结束 01107，各 5 位。
	if got, want := string(frame), "%01#RDD0110501107"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
	if !bytes.HasSuffix(frame, []byte("\r")) {
		t.Fatalf("frame must end with CR: %q", frame)
	}
	// BCC 必须与帧内容自洽。
	body := frame[:len(frame)-3]
	if got, want := BCC(body), parseBCCForTest(t, frame); got != want {
		t.Fatalf("BCC = %02X, want %02X", got, want)
	}
}

func parseBCCForTest(t *testing.T, frame []byte) byte {
	t.Helper()
	if len(frame) < 4 {
		t.Fatalf("frame too short: %q", frame)
	}
	var v byte
	for _, c := range frame[len(frame)-3 : len(frame)-1] {
		var n byte
		switch {
		case c >= '0' && c <= '9':
			n = c - '0'
		case c >= 'A' && c <= 'F':
			n = c - 'A' + 10
		default:
			t.Fatalf("bad BCC digit %q in %q", c, frame)
		}
		v = v<<4 | n
	}
	return v
}

// 索引寄存器没有编号：区码两字符 + 9 个 0，起始/结束两段被整体取代。
func TestBuildReadIndexRegister(t *testing.T) {
	frame, err := BuildReadWords(1, mustArea(t, "ix"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#RDIX000000000"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
	// 长度：%, 01, #, RD, IX, 9 个 0, BCC 2, CR = 20
	if len(frame) != 20 {
		t.Fatalf("frame length = %d, want 20 (%q)", len(frame), frame)
	}
}

func TestBuildReadWordsRejects(t *testing.T) {
	cases := []struct {
		name string
		area Area
		s, e int
	}{
		{"end before start", dt(t), 10, 9},
		{"start negative", dt(t), -1, 0},
		{"end too large", dt(t), 0, 100000},
		{"over word limit", dt(t), 0, MaxWords}, // MaxWords+1 个字
		{"contact area", mustArea(t, "x"), 0, 1},
	}
	for _, c := range cases {
		if _, err := BuildReadWords(1, c.area, c.s, c.e); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

// RCC 的地址是 4 位，一个地址 = 16 个接点。比 RD 少一位。
func TestBuildReadContactWords(t *testing.T) {
	area := mustArea(t, "x")
	frame, err := BuildReadContactWords(1, area, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#RCCX00000002"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

// RS/RK 的正文是裸的 4 位起止地址，没有区域码。
func TestBuildReadTimerValues(t *testing.T) {
	sv := mustArea(t, "sv")
	frame, err := BuildReadTimerValues(1, sv, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#RS00000002"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}

	ev := mustArea(t, "ev")
	frame, err = BuildReadTimerValues(1, ev, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#RK00100010"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 写帧
// ---------------------------------------------------------------------------

// WD 的正文也带结束地址：`区码 + 起始(5) + 结束(5) + 数据`。
//
// 这一条最容易写错——很多二手资料只写「起始地址 + 数据」。漏掉结束地址
// 时帧是「格式合法但语义错位」的，PLC 回 41H，而错误信息不会告诉你少了
// 哪一段。
func TestBuildWriteWords(t *testing.T) {
	data := []byte{0x00, 0x63, 0x33, 0x44, 0x00, 0x0A}
	frame, err := BuildWriteWords(1, dt(t), 1105, data)
	if err != nil {
		t.Fatal(err)
	}
	// 三个字：1105..1107；数据低字节在前 → 6300 4433 0A00。
	want := "%01#WDD0110501107630044330A00"
	if got := string(frame); !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

// 索引寄存器写：区码两字符 + 9 个 0 + 数据。
func TestBuildWriteIndexRegister(t *testing.T) {
	frame, err := BuildWriteWords(1, mustArea(t, "id"), 0, []byte{0x00, 0x02, 0x00, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#WDID00000000002000100"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

func TestBuildWriteContactWords(t *testing.T) {
	area := mustArea(t, "y")
	// 值 H0063（大端字节 00 63）→ 线上 6300。
	frame, err := BuildWriteContactWords(1, area, 0, []byte{0x00, 0x63})
	if err != nil {
		t.Fatal(err)
	}
	// 一个字：起始 0000、结束 0000，数据 6300。地址各 4 位。
	want := "%01#WCCY000000006300"
	if got := string(frame); !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

// WCS 是布尔点位唯一的写路径：`S` + 区码 + 4 位接点号 + 1/0。
func TestBuildWriteContact(t *testing.T) {
	cases := []struct {
		area string
		word int
		bit  int
		val  bool
		want string
	}{
		{"x", 1, 15, true, "%01#WCSX001F1"},
		{"x", 1, 15, false, "%01#WCSX001F0"},
		{"r", 16, 3, true, "%01#WCSR01631"},
		{"y", 0, 0, true, "%01#WCSY00001"},
		{"t", 10, 0, true, "%01#WCST00101"},
	}
	for _, c := range cases {
		frame, err := BuildWriteContact(1, mustArea(t, c.area), c.word, c.bit, c.val)
		if err != nil {
			t.Fatalf("%s%d.%d: %v", c.area, c.word, c.bit, err)
		}
		if got := string(frame); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s%d.%d=%v: frame = %q, want prefix %q", c.area, c.word, c.bit, c.val, got, c.want)
		}
	}
}

func TestBuildWriteTimerValues(t *testing.T) {
	frame, err := BuildWriteTimerValues(1, mustArea(t, "sv"), 5, []byte{0x00, 0x64})
	if err != nil {
		t.Fatal(err)
	}
	// 起始 0005、结束 0005、数据 6400（H0064 低字节在前）。
	want := "%01#WS000500056400"
	if got := string(frame); !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}

	frame, err = BuildWriteTimerValues(1, mustArea(t, "ev"), 5, []byte{0x00, 0x64})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#WK000500056400"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

func TestBuildWriteRejects(t *testing.T) {
	// 奇数字节、空数据、超字数、越界。
	if _, err := BuildWriteWords(1, dt(t), 0, []byte{0x01}); err == nil {
		t.Error("odd byte count should be rejected")
	}
	if _, err := BuildWriteWords(1, dt(t), 0, nil); err == nil {
		t.Error("empty data should be rejected")
	}
	if _, err := BuildWriteWords(1, dt(t), 0, make([]byte, (MaxWords+1)*2)); err == nil {
		t.Error("over word limit should be rejected")
	}
	if _, err := BuildWriteWords(1, dt(t), 99999, make([]byte, 4)); err == nil {
		t.Error("write past the address space should be rejected")
	}
	if _, err := BuildWriteContact(1, dt(t), 0, 0, true); err == nil {
		t.Error("writing a contact on a word area should be rejected")
	}
	if _, err := BuildWriteContact(1, mustArea(t, "x"), 0, 16, true); err == nil {
		t.Error("bit 16 should be rejected")
	}
}

// ---------------------------------------------------------------------------
// 接点编号
// ---------------------------------------------------------------------------

// X/Y/R/L 的编号是「前面十进制、末位十六进制」；T/C 全十进制。
func TestContactNumber(t *testing.T) {
	x := mustArea(t, "x")
	cases := []struct {
		area Area
		word int
		bit  int
		want string
	}{
		{x, 0, 0, "0000"},
		{x, 1, 15, "001F"},
		{x, 16, 3, "0163"},
		{x, 100, 10, "100A"},
		{mustArea(t, "t"), 10, 0, "0010"},
		{mustArea(t, "c"), 999, 0, "0999"},
	}
	for _, c := range cases {
		got, err := ContactNumber(c.area, c.word, c.bit)
		if err != nil {
			t.Fatalf("%s%d.%d: %v", c.area.Key, c.word, c.bit, err)
		}
		if got != c.want {
			t.Errorf("%s%d.%d = %q, want %q", c.area.Key, c.word, c.bit, got, c.want)
		}
	}
}

// 正反两个方向必须互逆，否则主站与从站会各算一套。
func TestContactNumberRoundTrip(t *testing.T) {
	for _, name := range []string{"x", "y", "r", "l", "t", "c"} {
		area := mustArea(t, name)
		for _, word := range []int{0, 1, 9, 10, 15, 16, 99, 100, 999, 1000, 9999} {
			maxBit := 15
			if !area.Hex {
				maxBit = 0
			}
			if area.Hex && word > 999 {
				continue
			}
			for bit := 0; bit <= maxBit; bit++ {
				text, err := ContactNumber(area, word, bit)
				if err != nil {
					t.Fatalf("%s%d.%d: %v", name, word, bit, err)
				}
				gotWord, gotBit, err := ParseContactNumber(area, text)
				if err != nil {
					t.Fatalf("%s%d.%d → %q: %v", name, word, bit, text, err)
				}
				if gotWord != word || gotBit != bit {
					t.Fatalf("%s%d.%d → %q → %d.%d", name, word, bit, text, gotWord, gotBit)
				}
			}
		}
	}
}

func TestContactNumberRejects(t *testing.T) {
	if _, err := ContactNumber(mustArea(t, "x"), 1000, 0); err == nil {
		t.Error("X1000 out of range")
	}
	if _, err := ContactNumber(mustArea(t, "x"), 0, 16); err == nil {
		t.Error("bit 16 out of range")
	}
	if _, err := ContactNumber(mustArea(t, "dt"), 0, 0); err == nil {
		t.Error("word area has no contact numbering")
	}
	if _, err := ContactNumber(mustArea(t, "sv"), 0, 0); err == nil {
		t.Error("sv has no contact numbering")
	}
	if _, err := ContactNumber(mustArea(t, "t"), 10000, 0); err == nil {
		t.Error("T10000 out of range")
	}
}

// ---------------------------------------------------------------------------
// 数据编解码
// ---------------------------------------------------------------------------

// 手册实例：寄存器值 H0063，线上是 "6300"。
func TestDecodeWordsLowByteFirst(t *testing.T) {
	got, err := DecodeWords("6300")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0x00, 0x63}) {
		t.Fatalf("DecodeWords(\"6300\") = % X, want 00 63", got)
	}
	// 三个字：H0063 H3344 H000A → 6300 4433 0A00
	got, err = DecodeWords("630044330A00")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0x00, 0x63, 0x33, 0x44, 0x00, 0x0A}) {
		t.Fatalf("DecodeWords = % X", got)
	}
}

func TestEncodeWordsInvertsDecode(t *testing.T) {
	for _, s := range []string{"6300", "4433", "000A", "FFFF", "0000", "1234ABCD"} {
		b, err := DecodeWords(s)
		if err != nil {
			t.Fatalf("DecodeWords(%q): %v", s, err)
		}
		back, err := EncodeWords(b)
		if err != nil {
			t.Fatalf("EncodeWords: %v", err)
		}
		if back != s {
			t.Fatalf("%q → % X → %q", s, b, back)
		}
	}
}

func TestDecodeWordsRejects(t *testing.T) {
	for _, s := range []string{"630", "63000", "63G0", "6"} {
		if _, err := DecodeWords(s); err == nil {
			t.Errorf("DecodeWords(%q) should fail", s)
		}
	}
	if _, err := EncodeWords([]byte{1}); err == nil {
		t.Error("EncodeWords with odd length should fail")
	}
}

func TestContactsCodec(t *testing.T) {
	if got := EncodeContacts([]bool{true, false, true}); got != "101" {
		t.Fatalf("EncodeContacts = %q, want \"101\"", got)
	}
	got, err := DecodeContacts("101")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Fatalf("DecodeContacts = %v", got)
	}
	if _, err := DecodeContacts("102"); err == nil {
		t.Error("contact data \"2\" should be rejected")
	}
}

// ---------------------------------------------------------------------------
// 应答解析
// ---------------------------------------------------------------------------

func resp(t *testing.T, body string) []byte {
	t.Helper()
	frame := append([]byte("%01$"), body...)
	return AppendBCC(frame)
}

func TestParseResponseNormal(t *testing.T) {
	frame := resp(t, "RD00634433000A")
	r, err := ParseResponse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("expected OK, got ErrCode %02X", r.ErrCode)
	}
	if r.Station != 1 {
		t.Fatalf("station = %d, want 1", int(r.Station))
	}
	if r.Code != "RD" {
		t.Fatalf("code = %q, want RD", r.Code)
	}
	if r.Data != "00634433000A" {
		t.Fatalf("data = %q", r.Data)
	}
	if !bytes.Equal(r.Raw, frame) {
		t.Fatalf("Raw not preserved")
	}
}

// 接点读的应答回显码只有两个字符：RCC → RC。
func TestParseResponseEchoIsTwoChars(t *testing.T) {
	frame := resp(t, "RC006300")
	r, err := ParseResponse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != "RC" {
		t.Fatalf("code = %q, want RC", r.Code)
	}
	if r.Data != "006300" {
		t.Fatalf("data = %q", r.Data)
	}
	if EchoCode(CmdReadContactWord) != "RC" {
		t.Fatalf("EchoCode(RCC) = %q, want RC", EchoCode(CmdReadContactWord))
	}
	if EchoCode(CmdWriteContactOne) != "WC" {
		t.Fatalf("EchoCode(WCS) = %q, want WC", EchoCode(CmdWriteContactOne))
	}
}

func TestParseResponseError(t *testing.T) {
	frame := []byte("%01!41")
	frame = AppendBCC(frame)
	r, err := ParseResponse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() {
		t.Fatal("error response must not report OK")
	}
	if r.ErrCode != 0x41 {
		t.Fatalf("ErrCode = %02X, want 41", r.ErrCode)
	}
	var ec *ErrorCode
	if !errors.As(r.Err(), &ec) {
		t.Fatalf("Err() = %v, want *ErrorCode", r.Err())
	}
	if ec.Kind() != ClassProcedure {
		t.Fatalf("kind = %v, want procedure", ec.Kind())
	}
}

func TestParseResponseBCCWildcard(t *testing.T) {
	// 对端用 `**` 表示不校验；我们按「不校验」接受，但 Raw 保留原样。
	frame := []byte("%01$RD0063**\r")
	r, err := ParseResponse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if r.Data != "0063" {
		t.Fatalf("data = %q", r.Data)
	}
}

func TestParseResponseBadBCC(t *testing.T) {
	frame := resp(t, "RD0063")
	// 把 BCC 两个字符改坏。
	frame[len(frame)-3] = '0'
	frame[len(frame)-2] = '0'
	_, err := ParseResponse(frame)
	var ce *ChecksumError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ChecksumError", err)
	}
}

func TestParseResponseRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":          nil,
		"no CR":          []byte("%01$RD006300"),
		"bad header":     append([]byte("X01$RD0063"), '\r'),
		"too short":      []byte("%01\r"),
		"bad station":    append([]byte("%0X$RD0063"), '\r'),
		"bad symbol":     append([]byte("%01%RD0063"), '\r'),
		"truncated body": []byte("%01$R\r"),
	}
	for name, frame := range cases {
		if _, err := ParseResponse(frame); err == nil {
			t.Errorf("%s: expected error for %q", name, frame)
		}
	}
}

// ---------------------------------------------------------------------------
// 错误码
// ---------------------------------------------------------------------------

func TestErrorCodeClassification(t *testing.T) {
	cases := []struct {
		code  byte
		class ErrorClass
		retry bool
	}{
		{0x22, ClassLink, true},         // 接收缓冲溢出
		{0x23, ClassLink, false},        // 站号冲突：配置问题
		{0x24, ClassLink, false},        // 硬件错
		{0x26, ClassLink, false},        // 站号设置错：配置问题
		{0x28, ClassLink, true},         // 无应答：可重试
		{0x40, ClassProcedure, true},    // BCC 错：重发一次有意义
		{0x41, ClassProcedure, true},    // 格式错
		{0x42, ClassProcedure, true},    // 命令不支持
		{0x50, ClassProcessing, true},   // 路由错
		{0x53, ClassProcessing, true},   // 忙
		{0x60, ClassApplication, false}, // 参数错
		{0x65, ClassApplication, false}, // 写保护
		{0x66, ClassApplication, false}, // 地址错
		{0x72, ClassApplication, true},  // 应用层超时
		{0x74, ClassApplication, true},  // 应用层超时
		{0x99, ClassUnknown, false},     // 未收录
	}
	for _, c := range cases {
		ec := &ErrorCode{Code: c.code}
		if got := ec.Kind(); got != c.class {
			t.Errorf("%02XH class = %v, want %v", c.code, got, c.class)
		}
		if got := ec.Retryable(); got != c.retry {
			t.Errorf("%02XH retryable = %v, want %v", c.code, got, c.retry)
		}
	}
}

func TestErrorCodeTextIsNotEmpty(t *testing.T) {
	for _, code := range []byte{0x22, 0x28, 0x40, 0x41, 0x53, 0x60, 0x66, 0x74} {
		ec := &ErrorCode{Code: code}
		if ec.Text() == "" || strings.Contains(ec.Text(), "unknown") {
			t.Errorf("%02XH has no description", code)
		}
		if !strings.Contains(ec.Error(), "H") {
			t.Errorf("%02XH error text should show the hex code: %q", code, ec.Error())
		}
	}
	if (&ErrorCode{Code: 0xEE}).Text() != "unknown error code" {
		t.Error("unknown code should say so")
	}
}

// ---------------------------------------------------------------------------
// 请求解析（内置从站用）
// ---------------------------------------------------------------------------

func req(t *testing.T, s string) Request {
	t.Helper()
	frame := append([]byte(nil), s...)
	frame = AppendBCC(frame)
	r, err := ParseRequest(frame)
	if err != nil {
		t.Fatalf("ParseRequest(%q): %v", s, err)
	}
	return r
}

func TestParseRequestRD(t *testing.T) {
	r := req(t, "%01#RDD0110501107")
	if r.Cmd != CmdReadData {
		t.Fatalf("cmd = %q", r.Cmd)
	}
	if r.Start != 1105 || r.End != 1107 {
		t.Fatalf("range = %d..%d, want 1105..1107", r.Start, r.End)
	}
	if r.Words() != 3 {
		t.Fatalf("Words() = %d, want 3", r.Words())
	}
	if r.Area.Key != "dt" {
		t.Fatalf("area = %q, want dt", r.Area.Key)
	}
}

func TestParseRequestIndexRegister(t *testing.T) {
	r := req(t, "%01#RDIX000000000")
	if r.Area.Key != "ix" {
		t.Fatalf("area = %q, want ix", r.Area.Key)
	}
	if r.Start != 0 || r.End != 0 {
		t.Fatalf("range = %d..%d, want 0..0", r.Start, r.End)
	}

	// ID 是 IX+IY 合成的 32 位：两个寄存器。
	r = req(t, "%01#RDID000000000")
	if r.Area.Key != "id" {
		t.Fatalf("area = %q, want id", r.Area.Key)
	}
	if r.Start != 0 || r.End != 1 {
		t.Fatalf("ID range = %d..%d, want 0..1", r.Start, r.End)
	}
}

func TestParseRequestWD(t *testing.T) {
	r := req(t, "%01#WDD0110501107630044330A00")
	if r.Start != 1105 || r.End != 1107 {
		t.Fatalf("range = %d..%d", r.Start, r.End)
	}
	want := []byte{0x00, 0x63, 0x33, 0x44, 0x00, 0x0A}
	if !bytes.Equal(r.Data, want) {
		t.Fatalf("data = % X, want % X", r.Data, want)
	}
}

// 结束地址与数据长度必须自洽：这是 WD/WCC/WS/WK 的正文里唯一的一致性
// 约束，收请求时就要拦，否则会写坏一段不属于这次请求的寄存器。
func TestParseRequestWriteLengthMismatch(t *testing.T) {
	frame := AppendBCC([]byte("%01#WDD0000000026300"))
	if _, err := ParseRequest(frame); err == nil {
		t.Fatal("end address 2 with one word of data should be rejected")
	}
}

func TestParseRequestRCC(t *testing.T) {
	r := req(t, "%01#RCCX00000002")
	if r.Cmd != CmdReadContactWord {
		t.Fatalf("cmd = %q", r.Cmd)
	}
	if r.Area.Key != "x" {
		t.Fatalf("area = %q", r.Area.Key)
	}
	if r.Start != 0 || r.End != 2 {
		t.Fatalf("range = %d..%d, want 0..2", r.Start, r.End)
	}
}

func TestParseRequestWCS(t *testing.T) {
	r := req(t, "%01#WCSX001F1")
	if r.Cmd != "WCS" {
		t.Fatalf("cmd = %q", r.Cmd)
	}
	if len(r.Contacts) != 1 {
		t.Fatalf("contacts = %d", len(r.Contacts))
	}
	c := r.Contacts[0]
	if c.Word != 1 || c.Bit != 15 || !c.Value || !c.HasValue {
		t.Fatalf("contact = %+v, want X1.15 = true", c)
	}
}

func TestParseRequestWCP(t *testing.T) {
	r := req(t, "%01#WCP2X00001R00101")
	if r.Cmd != "WCP" {
		t.Fatalf("cmd = %q", r.Cmd)
	}
	if len(r.Contacts) != 2 {
		t.Fatalf("contacts = %d, want 2", len(r.Contacts))
	}
	if r.Contacts[0].Area.Key != "x" || !r.Contacts[0].Value {
		t.Fatalf("first contact = %+v", r.Contacts[0])
	}
	if r.Contacts[1].Area.Key != "r" || !r.Contacts[1].Value {
		t.Fatalf("second contact = %+v", r.Contacts[1])
	}
}

func TestParseRequestTimerValues(t *testing.T) {
	r := req(t, "%01#RS00000002")
	if r.Area.Key != "sv" {
		t.Fatalf("area = %q, want sv", r.Area.Key)
	}
	if r.Start != 0 || r.End != 2 {
		t.Fatalf("range = %d..%d", r.Start, r.End)
	}

	r = req(t, "%01#WK000500056400")
	if r.Area.Key != "ev" {
		t.Fatalf("area = %q, want ev", r.Area.Key)
	}
	if !bytes.Equal(r.Data, []byte{0x00, 0x64}) {
		t.Fatalf("data = % X", r.Data)
	}
}

// 三字符命令必须先于两字符匹配：`RC` 是 `RCS`/`RCP`/`RCC` 的前缀。
func TestParseRequestPrefersThreeCharCommand(t *testing.T) {
	r := req(t, "%01#RCSX0000")
	if r.Cmd != "RCS" {
		t.Fatalf("cmd = %q, want RCS (not RC)", r.Cmd)
	}
}

// 地址必须是十进制。写成十六进制的帧要被明确拒绝，而不是被当成另一个地址。
func TestParseRequestRejectsHexAddress(t *testing.T) {
	frame := AppendBCC([]byte("%01#RDD0110A00002"))
	if _, err := ParseRequest(frame); err == nil {
		t.Fatal("hex digit in the address should be rejected")
	}
}

func TestParseRequestRejectsUnknownCommand(t *testing.T) {
	frame := AppendBCC([]byte("%01#ZZ0000"))
	if _, err := ParseRequest(frame); err == nil {
		t.Fatal("unknown command should be rejected")
	}
}

func TestParseRequestRejectsBadSymbol(t *testing.T) {
	// 应答符号出现在请求位置：说明这是「自己发的应答回来了」或者
	// 对端站号配重，必须拒绝。
	frame := AppendBCC([]byte("%01$RD0063"))
	if _, err := ParseRequest(frame); err == nil {
		t.Fatal("response symbol in a request should be rejected")
	}
}

// ---------------------------------------------------------------------------
// 应答构造
// ---------------------------------------------------------------------------

func TestBuildResponseEchoesTwoChars(t *testing.T) {
	frame, err := BuildResponse(1, CmdReadContactWord, "63004433")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01$RC63004433"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
	if _, err := ParseResponse(frame); err != nil {
		t.Fatalf("our own response does not parse: %v", err)
	}
}

func TestBuildErrorResponse(t *testing.T) {
	frame, err := BuildErrorResponse(1, 0x41)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01!41"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
	if len(frame) != 9 {
		t.Fatalf("length = %d, want 9 (%q)", len(frame), frame)
	}
	r, err := ParseResponse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if r.ErrCode != 0x41 {
		t.Fatalf("ErrCode = %02X", r.ErrCode)
	}
}

// 请求 → 应答 → 解析 的闭环：三个函数各自都对但拼起来的语义也可能错，
// 这条把三方钉在一起。
func TestRequestResponseRoundTrip(t *testing.T) {
	read, err := BuildReadContactWords(1, mustArea(t, "r"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRequest(read)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := BuildResponse(r.Station, r.Cmd, "001F")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != EchoCode(r.Cmd) {
		t.Fatalf("echo = %q, want %q", got.Code, EchoCode(r.Cmd))
	}
	if got.Data != "001F" {
		t.Fatalf("data = %q", got.Data)
	}
}

// ---------------------------------------------------------------------------
// 区域表
// ---------------------------------------------------------------------------

func TestLookupAliasesAndCodes(t *testing.T) {
	cases := map[string]string{
		"d": "dt", "D": "dt", "dt": "dt", "DT": "dt",
		"f": "fl", "fl": "fl", "F": "fl",
		"x": "x", "X": "x", "wx": "x", "WX": "x",
		"y": "y", "wy": "y",
		"r": "r", "wr": "r",
		"l": "l", "wl": "l",
		"ix": "ix", "IX": "ix", "i0": "ix",
		"iy": "iy", "i1": "iy",
		"id": "id", "ID": "id",
		"sv": "sv", "set": "sv",
		"ev": "ev", "k": "ev",
	}
	for in, want := range cases {
		a, ok := Lookup(in)
		if !ok {
			t.Errorf("Lookup(%q) failed", in)
			continue
		}
		if a.Key != want {
			t.Errorf("Lookup(%q).Key = %q, want %q", in, a.Key, want)
		}
	}
	if _, ok := Lookup("zz"); ok {
		t.Error("Lookup(\"zz\") should fail")
	}
}

// 规范键必须唯一：两个区共用一个 Key 会让规划层把两组点位塞进同一个 span。
func TestKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range Areas() {
		if seen[a.Key] {
			t.Errorf("duplicate area key %q", a.Key)
		}
		seen[a.Key] = true
	}
}

// DT 与 LD 的区码是 D 与 L，而 L 又是接点区「链接继电器」。同一个字母
// 在两类命令里含义不同，靠 Kind 区分——这条测试把这个歧义钉住。
func TestAreaCodeCollision(t *testing.T) {
	l, _ := Lookup("l")
	ld, _ := Lookup("ld")
	if l.Code != "L" || ld.Code != "L" {
		t.Fatalf("both should use code L: %q %q", l.Code, ld.Code)
	}
	if l.Kind != KindContact || ld.Kind != KindWord {
		t.Fatalf("kinds should differ: %v %v", l.Kind, ld.Kind)
	}
}

// T/C 接点必须走 RCS/RCP：按字读会把它们按每字 16 点重新编号，与 T/C 自身
// 的十进制编号是两套体系。
func TestBuildReadContact(t *testing.T) {
	frame, err := BuildReadContact(1, mustArea(t, "t"), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#RCST0010"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}

	// X1.15 → 接点号 001F（前十进制 + 末位十六进制）。
	frame, err = BuildReadContact(1, mustArea(t, "x"), 1, 15)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(frame), "%01#RCSX001F"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
}

func TestBuildReadContacts(t *testing.T) {
	contacts := []Contact{
		{Area: mustArea(t, "x"), Word: 0, Bit: 1},
		{Area: mustArea(t, "t"), Word: 10, Bit: 0},
	}
	frame, err := BuildReadContacts(1, contacts)
	if err != nil {
		t.Fatal(err)
	}
	// n=2，然后两组「区码 + 4 位接点号」：X0001、T0010。
	if got, want := string(frame), "%01#RCP2X0001T0010"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}

	// 超过 8 个应当被拒（命令正文的个数位只有一位）。
	many := make([]Contact, MaxContacts+1)
	for i := range many {
		many[i] = Contact{Area: mustArea(t, "x"), Word: i, Bit: 0}
	}
	if _, err := BuildReadContacts(1, many); err == nil {
		t.Fatal("more than 8 contacts in one RCP should be rejected")
	}
	if _, err := BuildReadContacts(1, nil); err == nil {
		t.Fatal("empty RCP should be rejected")
	}
}

// WCP：一次写多个接点，正文是「个数 + 每点(区码+接点号+数据位)」。
// 它不原子，所以驱动逐点走 WCS；但编解码必须对，否则内置从站的 WCP
// 分支就是一段没人能测的死代码。
func TestBuildWriteContacts(t *testing.T) {
	contacts := []Contact{
		{Area: mustArea(t, "y"), Word: 0, Bit: 0, Value: true, HasValue: true},
		{Area: mustArea(t, "r"), Word: 2, Bit: 3, Value: false, HasValue: true},
	}
	frame, err := BuildWriteContacts(1, contacts)
	if err != nil {
		t.Fatal(err)
	}
	// n=2，然后 Y0000 + 1、R0023 + 0。
	if got, want := string(frame), "%01#WCP2Y00001R00230"; !strings.HasPrefix(got, want) {
		t.Fatalf("frame = %q, want prefix %q", got, want)
	}
	// 往返：从站解析出来的接点必须与写进去的一致。
	req, err := ParseRequest(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Contacts) != 2 {
		t.Fatalf("contacts = %d, want 2", len(req.Contacts))
	}
	if !req.Contacts[0].Value || req.Contacts[1].Value {
		t.Fatalf("values = %v/%v, want true/false", req.Contacts[0].Value, req.Contacts[1].Value)
	}

	// 空与超限都拒绝（个数位只有一位字符）。
	if _, err := BuildWriteContacts(1, nil); err == nil {
		t.Fatal("empty WCP should be rejected")
	}
	many := make([]Contact, MaxContacts+1)
	for i := range many {
		many[i] = Contact{Area: mustArea(t, "y"), Word: i, Bit: 0}
	}
	if _, err := BuildWriteContacts(1, many); err == nil {
		t.Fatal("more than 8 contacts in one WCP should be rejected")
	}
}
