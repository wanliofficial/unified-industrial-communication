package dlt645

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

// 组帧/解帧是这个协议里最容易出错的一层：前导、±0x33、CS 覆盖范围、
// 地址方向、DI 方向，五处里任何一处错都会得到「看起来像帧但不通」的
// 报文。这里用一段真实抓包形状的字节钉死这五件事。

func TestBuildReadRequestGoldenBytes(t *testing.T) {
	addr, err := ParseAddr("000000000042")
	if err != nil {
		t.Fatal(err)
	}
	di, err := ParseDI("02010100") // A 相电压
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ReadRequest(addr, di)
	if err != nil {
		t.Fatal(err)
	}
	// 地址 42 00 00 00 00 00；DI 线上顺序 00 01 01 02 → +0x33 → 33 34 34 35。
	want := []byte{
		0xFE, 0xFE, 0xFE, 0xFE,
		0x68, 0x42, 0x00, 0x00, 0x00, 0x00, 0x00, 0x68,
		0x11, 0x04,
		0x33, 0x34, 0x34, 0x35,
	}
	var sum byte
	for _, b := range want[4:] {
		sum += b
	}
	want = append(want, sum, 0x16)

	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = % X\nwant  = % X", frame, want)
	}
	// 自己组的帧必须能被自己解开（组帧与解帧对 CS 覆盖范围的理解一致）。
	fr, consumed, err := Parse(frame)
	if err != nil {
		t.Fatalf("parse own frame: %v", err)
	}
	if consumed != len(frame) {
		t.Fatalf("consumed = %d, want %d", consumed, len(frame))
	}
	if fr.Ctrl != CtrlRead {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	if got, _ := DIFromBytes(fr.Data); got != di {
		t.Fatalf("di roundtrip = %s, want %s", got, di)
	}
}

func TestParseSkipsNoiseAndResyncs(t *testing.T) {
	addr, _ := ParseAddr("42")
	good, err := ReadRequest(addr, DI{DI3: 0x02, DI2: 0x01, DI1: 0x01, DI0: 0x00})
	if err != nil {
		t.Fatal(err)
	}
	// 前面塞噪声、中间塞一个「假的 68」：Reader 必须最终吐出那一帧好
	// 数据。Parse 单次可能因为假帧头返回 SyncError——这正是 Reader 存在
	// 的理由：它把「跳过几个字节再试」收敛到一个循环里。
	noise := []byte{0x00, 0x68, 0x11, 0x22, 0x33}
	buf := append(append([]byte{}, noise...), good...)
	rd := NewReader(&memStream{data: buf})
	fr, err := rd.Next()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if fr.Ctrl != CtrlRead {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	if _, err := rd.Next(); err == nil {
		t.Fatal("second Next should hit EOF")
	}
}

// memStream 是一段静态字节流，用于在不碰真实串口的前提下驱动 Reader。
type memStream struct {
	data []byte
	pos  int
}

func (m *memStream) Write(p []byte) error { return nil }

func (m *memStream) ReadFull(n int) ([]byte, error) {
	if m.pos >= len(m.data) {
		return nil, io.EOF
	}
	end := m.pos + n
	if end > len(m.data) {
		end = len(m.data)
	}
	out := make([]byte, end-m.pos)
	copy(out, m.data[m.pos:end])
	m.pos = end
	if len(out) < n {
		return out, io.ErrUnexpectedEOF
	}
	return out, nil
}

func (m *memStream) Close() error { return nil }

func TestParseRejectsBadChecksumAndEnd(t *testing.T) {
	addr, _ := ParseAddr("42")
	good, _ := ReadRequest(addr, DI{})

	bad := append([]byte(nil), good...)
	bad[len(bad)-2] ^= 0xFF // CS
	if _, _, err := Parse(bad); err == nil {
		t.Fatal("bad checksum accepted")
	} else {
		var ck *ChecksumError
		if !errors.As(err, &ck) {
			t.Fatalf("err = %T %v, want ChecksumError", err, err)
		}
	}

	badEnd := append([]byte(nil), good...)
	badEnd[len(badEnd)-1] = 0x00
	if _, _, err := Parse(badEnd); err == nil {
		t.Fatal("bad end byte accepted")
	}
}

func TestParseIncompleteKeepsStart(t *testing.T) {
	addr, _ := ParseAddr("42")
	good, _ := ReadRequest(addr, DI{})
	// 切到一半：consumed 必须是「起始符之前」的字节数，起始符本身留下
	// 等后续字节——否则 Reader 永远凑不齐一帧。
	partial := good[:10]
	_, consumed, err := Parse(partial)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
	// 前导 FE 不是帧的一部分，必须立即丢弃（4 个字节）；起始符 0x68
	// 必须留住，等后续字节把这一帧凑齐。
	if consumed != PreambleLen {
		t.Fatalf("consumed = %d, want %d (only preamble may be dropped)", consumed, PreambleLen)
	}
}

func TestAddressParseAndFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"000000000042", "000000000042"},
		{"0042", "AAAAAAAA0042"}, // 缩位：只给低 4 位，其余补 AA 通配
		{"42", "AAAAAAAAAA42"},
		{"001234567890", "001234567890"},
		{"AA0000000042", "AA0000000042"},
	}
	for _, c := range cases {
		a, err := ParseAddr(c.in)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", c.in, err)
		}
		if got := FormatAddr(a); got != c.want {
			t.Fatalf("ParseAddr(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "123", "12AB", "00112233445566"} {
		if _, err := ParseAddr(bad); err == nil {
			t.Fatalf("ParseAddr(%q) accepted", bad)
		}
	}
}

func TestMatchAddrWildcard(t *testing.T) {
	want, _ := ParseAddr("42") // 0000AAAA0042
	got, _ := ParseAddr("000000000042")
	if !MatchAddr(got, want) {
		t.Fatal("缩位地址必须匹配实际地址")
	}
	other, _ := ParseAddr("000000000043")
	if MatchAddr(other, want) {
		t.Fatal("不同表号的应答必须被拒绝")
	}
}

func TestDecodeLittleEndianBCD(t *testing.T) {
	// 123456.78 kWh → 12345678 → BCD 12 34 56 78 → 低字节在前 78 56 34 12。
	raw := []byte{0x78, 0x56, 0x34, 0x12}
	item, ok := Lookup(DI{DI3: 0x00, DI2: 0x00})
	if !ok {
		t.Fatal("energy DI must be known")
	}
	v, err := Decode(item, raw)
	if err != nil {
		t.Fatal(err)
	}
	if v != 123456.78 {
		t.Fatalf("energy = %v, want 123456.78", v)
	}
}

func TestDecodeSignBit(t *testing.T) {
	// 组合有功电能最高位是符号位：0x92 34 56 78 → 去掉符号位是
	// 12 34 56 78 = 12345678，两位小数 → -123456.78。
	raw := []byte{0x78, 0x56, 0x34, 0x92}
	item, _ := Lookup(DI{})
	v, err := Decode(item, raw)
	if err != nil {
		t.Fatal(err)
	}
	if v != -123456.78 {
		t.Fatalf("signed energy = %v", v)
	}

	// 电压无符号：2200 → BCD 22 00 → 低字节在前 00 22；最高字节 0x22
	// 的 bit7 是 0，但真正的关键是「即使它是 1 也不能当符号位」。
	volt, _ := Lookup(mustDI(t, "02010100"))
	v, err = Decode(volt, []byte{0x00, 0x22})
	if err != nil {
		t.Fatal(err)
	}
	if v != 220.0 {
		t.Fatalf("voltage = %v, want 220.0", v)
	}

	// 电流有符号、三位小数：-5.123 A → 5123 → 3 字节 BCD 00 51 23 →
	// 低字节在前 23 51 00，最高字节置符号位 → 23 51 80。
	cur, _ := Lookup(mustDI(t, "02020100"))
	v, err = Decode(cur, []byte{0x23, 0x51, 0x80})
	if err != nil {
		t.Fatal(err)
	}
	if v != -5.123 {
		t.Fatalf("current = %v, want -5.123", v)
	}
}

func TestDecodePowerFactorThreeDecimals(t *testing.T) {
	pf, _ := Lookup(mustDI(t, "02060000"))
	v, err := Decode(pf, []byte{0x85, 0x09}) // 0985
	if err != nil {
		t.Fatal(err)
	}
	if v != 0.985 {
		t.Fatalf("power factor = %v", v)
	}
}

func TestDecodeTimeAndAddress(t *testing.T) {
	tm, _ := Lookup(mustDI(t, "04000101"))
	v, err := Decode(tm, []byte{0x07, 0x30, 0x12, 0x11, 0x11, 0x26})
	if err != nil {
		t.Fatal(err)
	}
	if v != 20261111123007 {
		t.Fatalf("time compact = %v", v)
	}

	addrItem, _ := Lookup(mustDI(t, "04000401"))
	v, err = Decode(addrItem, []byte{0x42, 0, 0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if v != 42 {
		t.Fatalf("address = %v", v)
	}
}

func TestDecodeDemand(t *testing.T) {
	// 12.3456 kW + 11-07 12:00
	it, _ := Lookup(mustDI(t, "01010000"))
	raw := []byte{0x56, 0x34, 0x12, 0x00, 0x00, 0x12, 0x07, 0x11}
	v, err := Decode(it, raw)
	if err != nil {
		t.Fatal(err)
	}
	if v != 12.3456 {
		t.Fatalf("demand = %v", v)
	}
	if s := Format(it, raw); s != "12.3456（发生于 11-07 12:00）" {
		t.Fatalf("format = %q", s)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	volt, _ := Lookup(mustDI(t, "02010100"))
	b, err := EncodeValue(volt, 228.7)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte{0x87, 0x22}) {
		t.Fatalf("encoded = % X, want 87 22", b)
	}
	v, err := Decode(volt, b)
	if err != nil {
		t.Fatal(err)
	}
	if v != 228.7 {
		t.Fatalf("roundtrip = %v", v)
	}
}

func TestEncodeRejectsOverflow(t *testing.T) {
	volt, _ := Lookup(mustDI(t, "02010100"))
	if _, err := EncodeValue(volt, 10000); err == nil {
		t.Fatal("2-byte BCD must reject 4-digit overflow")
	}
}

func TestTimeBytesOrder(t *testing.T) {
	tm := time.Date(2026, 11, 7, 12, 34, 56, 0, time.Local)
	b := TimeBytes(tm)
	want := []byte{0x56, 0x34, 0x12, 0x07, 0x11, 0x26}
	if !bytes.Equal(b[:], want) {
		t.Fatalf("time bytes = % X, want % X", b[:], want)
	}
	back, err := ParseTime(b[:])
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(tm) {
		t.Fatalf("roundtrip = %v", back)
	}
}

func TestExceptionClassification(t *testing.T) {
	cases := []struct {
		code byte
		kind ExceptionKind
	}{
		{ErrNoData, ExceptionAddress},
		{ErrPassword, ExceptionPermission},
		{ErrOther, ExceptionProtocol},
		{ErrBaud, ExceptionProtocol},
	}
	for _, c := range cases {
		e := &Exception{Code: c.code}
		if got := e.Kind(); got != c.kind {
			t.Fatalf("code %#x kind = %v, want %v", c.code, got, c.kind)
		}
	}
}

func TestParseReadReplyVerifiesDIAndSeq(t *testing.T) {
	di := mustDI(t, "02010100")
	diRaw := di.Bytes()
	raw, _ := Build([AddrLen]byte{0x42}, CtrlRead|BitReply|BitFollow,
		append(diRaw[:], 0x87, 0x22))
	fr, _, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseReadReply(fr, di)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Follow || len(r.Data) != 2 || !bytes.Equal(r.Data, []byte{0x87, 0x22}) {
		t.Fatalf("reply = %+v", r)
	}
	// DI 不匹配必须报错——否则迟到的应答会被按本次的小数位解释。
	if _, err := ParseReadReply(fr, mustDI(t, "02010200")); err == nil {
		t.Fatal("mismatched DI accepted")
	}

	// 后续帧带序号，且序号必须与请求一致。
	nextRaw, _ := Build([AddrLen]byte{0x42}, CtrlReadNext|BitReply,
		append(diRaw[:], 0x01, 0x02, 0x03))
	nfr, _, err := Parse(nextRaw)
	if err != nil {
		t.Fatal(err)
	}
	nr, err := ParseReadNextReply(nfr, di, 3)
	if err != nil {
		t.Fatal(err)
	}
	if nr.Seq != 3 || !bytes.Equal(nr.Data, []byte{0x01, 0x02}) {
		t.Fatalf("next reply = %+v", nr)
	}
	if _, err := ParseReadNextReply(nfr, di, 4); err == nil {
		t.Fatal("seq mismatch accepted")
	}
}

func TestReadNextRequestLayout(t *testing.T) {
	addr, _ := ParseAddr("42")
	frame, err := ReadNextRequest(addr, mustDI(t, "00000000"), 7)
	if err != nil {
		t.Fatal(err)
	}
	fr, _, err := Parse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Ctrl != CtrlReadNext || len(fr.Data) != 5 || fr.Data[4] != 7 {
		t.Fatalf("frame = %+v", fr)
	}
	if _, err := ReadNextRequest(addr, DI{}, 0); err == nil {
		t.Fatal("seq 0 accepted")
	}
}

func TestWriteRequestLayout(t *testing.T) {
	addr, _ := ParseAddr("42")
	di := mustDI(t, "04000101")
	payload := []byte{0x56, 0x34, 0x12, 0x07, 0x11, 0x26}
	frame, err := WriteRequest(addr, di, 0x02, [3]byte{0x11, 0x22, 0x33}, [4]byte{1, 2, 3, 4}, payload)
	if err != nil {
		t.Fatal(err)
	}
	fr, _, err := Parse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Ctrl != CtrlWrite {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	if len(fr.Data) != 12+len(payload) {
		t.Fatalf("len = %d", len(fr.Data))
	}
	if fr.Data[0] != di.DI0 || fr.Data[3] != di.DI3 {
		t.Fatalf("DI order wrong: % X", fr.Data[:4])
	}
	if fr.Data[4] != 0x02 || fr.Data[5] != 0x11 {
		t.Fatalf("password fields wrong: % X", fr.Data[4:8])
	}
	if !bytes.Equal(fr.Data[12:], payload) {
		t.Fatalf("payload = % X", fr.Data[12:])
	}
}

func TestBlockDIKnown(t *testing.T) {
	di := mustDI(t, "0201FF00")
	it, ok := Lookup(di)
	if !ok || it.Width != 0 {
		t.Fatalf("block item = %+v ok=%v", it, ok)
	}
	// 未知数据块也接受（宽度由响应决定）。
	if _, ok := Lookup(mustDI(t, "03FF0000")); !ok {
		t.Fatal("unknown block should be accepted with dynamic width")
	}
	// 未收录的非块 DI 必须拒绝——猜宽度会让电量错位入账。
	if _, ok := Lookup(mustDI(t, "03010000")); ok {
		t.Fatal("unknown non-block DI must be rejected")
	}
}

func mustDI(t *testing.T, s string) DI {
	t.Helper()
	di, err := ParseDI(s)
	if err != nil {
		t.Fatal(err)
	}
	return di
}
