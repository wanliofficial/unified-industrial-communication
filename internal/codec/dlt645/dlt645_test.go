package dlt645

import (
	"bytes"
	"errors"
	"io"
	"strings"
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
	// 8 字节复合：需量按数值编码（低字节在前），发生时间按 **MM DD hh mm**
	// 字段序——与日期数据项同形，不是小端整数。
	raw := []byte{0x56, 0x34, 0x12, 0x00, 0x11, 0x07, 0x12, 0x00}
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
	// 发生时间段用字段序解释：MM DD hh mm。若实现把它当成小端整数，会把
	// 「12 月 31 日 23:59」读成「59 月 23 日…」，必须报错而不是归一化。
	bad := append([]byte{0x56, 0x34, 0x12, 0x00}, 0x59, 0x23, 0x31, 0x12)
	if _, err := Decode(it, bad); err == nil {
		t.Fatal("发生时间按错误的字节序被接受")
	}
	// 正确的「12 月 31 日 23:59」必须能读出来。
	good := append([]byte{0x56, 0x34, 0x12, 0x00}, 0x12, 0x31, 0x23, 0x59)
	if _, err := Decode(it, good); err != nil {
		t.Fatalf("valid demand timestamp rejected: %v", err)
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

// TestEncodeRejectsBCDBoundary 钉住「按 BCD 上界而非二进制上界校验」。
//
// 早先用 1<<(4*width*2)=256^width 做上界，2 字节项的界是 65536，而真正的
// BCD 上界只有 9999：6000..9999 之间的值全部「通过校验、被静默截断」。电压
// 是 2 字节 1 位小数，BCD 上界为 999.9；1000.0 必须报错而不是发出 00 00。
func TestEncodeRejectsBCDBoundary(t *testing.T) {
	volt, _ := Lookup(mustDI(t, "02010100"))
	// 边界内：999.9 → 9999，必须成功且能原样读回。
	if b, err := EncodeValue(volt, 999.9); err != nil {
		t.Fatalf("999.9 must encode: %v", err)
	} else if !bytes.Equal(b, []byte{0x99, 0x99}) {
		t.Fatalf("999.9 wire = % X, want 99 99", b)
	}
	// 刚过边界：1000.0 必须报错，不能发出 00 00。
	for _, v := range []float64{1000.0, 6500.0, 6000.0} {
		if b, err := EncodeValue(volt, v); err == nil {
			back, _ := Decode(volt, b)
			t.Fatalf("%v 必须被拒绝（BCD 上界 999.9），却编成 % X（读回 %v）", v, b, back)
		}
	}
	// 1 字节有符号项：符号位占最高位，有效位只到 79。85 会被编成 0x85、
	// 读回 -5；必须报错。
	oneByte := Item{Name: "1 字节有符号", Width: 1, Signed: true}
	if b, err := EncodeValue(oneByte, 79); err != nil {
		t.Fatalf("79 must encode: %v", err)
	} else if !bytes.Equal(b, []byte{0x79}) {
		t.Fatalf("79 wire = % X", b)
	}
	for _, v := range []float64{80, 85, 99} {
		if b, err := EncodeValue(oneByte, v); err == nil {
			back, _ := Decode(oneByte, b)
			t.Fatalf("有符号 1 字节值 %v 必须被拒绝（符号位占最高位），却编成 % X（读回 %v）", v, b, back)
		}
	}
	// 无符号 1 字节仍可到 99。
	oneByteU := Item{Name: "1 字节无符号", Width: 1}
	if b, err := EncodeValue(oneByteU, 99); err != nil || !bytes.Equal(b, []byte{0x99}) {
		t.Fatalf("无符号 99 = % X, %v", b, err)
	}
}

// TestEncodeBitsIsBinary 钉住位域写入是**二进制**、与 Decode 对称。
//
// 周休日特征字（04000701）是唯一可写的 KindBits 项。早先 EncodeValue 把它
// 落进 BCD 编码：18 写成 0x18、读回 24；100 写成 0x00、读回 0。全是「写
// 成功、值是错的」。位域必须按二进制字节写出。
func TestEncodeBitsIsBinary(t *testing.T) {
	weekday, ok := Lookup(mustDI(t, "04000701"))
	if !ok || weekday.Kind != KindBits || !weekday.Writable {
		t.Fatalf("周休日特征字 = %+v，期望可写的 KindBits", weekday)
	}
	for _, v := range []float64{0, 5, 18, 100, 255} {
		b, err := EncodeValue(weekday, v)
		if err != nil {
			t.Fatalf("encode %v: %v", v, err)
		}
		if !bytes.Equal(b, []byte{byte(v)}) {
			t.Fatalf("%v wire = % X，期望 [%02X]（二进制）", v, b, byte(v))
		}
		back, err := Decode(weekday, b)
		if err != nil {
			t.Fatalf("decode % X: %v", b, err)
		}
		if back != v {
			t.Fatalf("往返 %v → % X → %v", v, b, back)
		}
	}
	// 越界与非整数必须拒绝。
	for _, v := range []float64{256, -1, 1.5} {
		if _, err := EncodeValue(weekday, v); err == nil {
			t.Fatalf("位域值 %v 必须被拒绝", v)
		}
	}
}

// TestParseTimeRejectsOutOfRangeFields 钉住「读路径的时间不再被静默归一化」。
//
// 早先 ParseTime 只校验 BCD 半字节是否合法，然后交给 time.Date。结果是
// 一堆「半字节都合法、字段却越界」的垃圾被静默洗成合法时间：
//
//	72 65 30 32 19 26 → 2027-08-02 07:06:12（秒=72 变下一分钟、月=19 变次年 7 月）
//	00 00 00 00 13 26 → 2026-12-31 00:00:00（月=13 变 12 月，日=0 变 31 日）
//
// 电表时钟是最不能「猜」的数据项：一个假的 07:06 比读不到更危险，因为它会
// 被当成真实事件时间参与结算与统计。与写路径共用同一套范围规则。
func TestParseTimeRejectsOutOfRangeFields(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string // 错误里必须出现的字段名
	}{
		{"秒越界", []byte{0x72, 0x11, 0x11, 0x11, 0x11, 0x26}, "秒"},
		{"分越界", []byte{0x00, 0x65, 0x11, 0x11, 0x11, 0x26}, "分钟"},
		{"时越界", []byte{0x00, 0x00, 0x30, 0x11, 0x11, 0x26}, "小时"},
		{"日越界", []byte{0x00, 0x00, 0x00, 0x32, 0x11, 0x26}, "日"},
		{"月为 0", []byte{0x00, 0x00, 0x00, 0x11, 0x00, 0x26}, "月份"},
		{"月越界", []byte{0x00, 0x00, 0x00, 0x11, 0x19, 0x26}, "月份"},
	}
	for _, c := range cases {
		tm, err := ParseTime(c.raw)
		if err == nil {
			t.Errorf("%s: ParseTime(% X) = %s, want error", c.name, c.raw, tm.Format("2006-01-02 15:04:05"))
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}
	// 合法时间必须照常解析。
	tm, err := ParseTime([]byte{0x07, 0x30, 0x12, 0x11, 0x11, 0x26})
	if err != nil {
		t.Fatalf("valid time: %v", err)
	}
	if tm.Year() != 2026 || int(tm.Month()) != 11 || tm.Day() != 11 ||
		tm.Hour() != 12 || tm.Minute() != 30 || tm.Second() != 7 {
		t.Fatalf("valid time parsed wrong: %s", tm)
	}
}

// TestDecodeDateRejectsOutOfRangeFields 钉住 4 字节日期数据项（MMDDhhmm）。
//
// 早先 Decode 用 decodeBCD(raw, true) 把它当「小端带符号整数」，两个问题：
// 一是字段越界会被静默接受（月=19、日=0）；二是 0x31 会被当成符号位，
// 「结算日 = 12312359」这类值会被解成负数。现在按 MMDDhhmm 逐字段解析。
func TestDecodeDateRejectsOutOfRangeFields(t *testing.T) {
	it, _ := Lookup(mustDI(t, "04000B01"))
	cases := []struct {
		name string
		raw  []byte
		want float64
	}{
		{"11-07 12:00", []byte{0x11, 0x07, 0x12, 0x00}, 11071200},
		{"12-31 23:59", []byte{0x12, 0x31, 0x23, 0x59}, 12312359},
	}
	for _, c := range cases {
		v, err := Decode(it, c.raw)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if v != c.want {
			t.Errorf("%s: value = %v, want %v", c.name, v, c.want)
		}
		// 读出来的值必须能被写回去（原样写回是常见操作）。
		b, err := EncodeDate(time.Date(2026, time.Month(int(c.want/1e6)),
			int(c.want/1e4)%100, int(c.want/1e2)%100, int(c.want)%100, 0, 0, time.UTC))
		if err != nil {
			t.Errorf("%s: EncodeDate: %v", c.name, err)
			continue
		}
		if !bytes.Equal(b, c.raw) {
			t.Errorf("%s: round trip = % X, want % X", c.name, b, c.raw)
		}
		// 再解回来必须是同一个数值——这才是「原样写回」要成立的原因。
		back, err := Decode(it, b)
		if err != nil {
			t.Errorf("%s: decode(encode) : %v", c.name, err)
			continue
		}
		if back != c.want {
			t.Errorf("%s: decode(encode) = %v, want %v", c.name, back, c.want)
		}
	}
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"月为 19", []byte{0x19, 0x07, 0x12, 0x00}},
		{"日为 0", []byte{0x11, 0x00, 0x12, 0x00}},
		{"时为 30", []byte{0x11, 0x07, 0x30, 0x00}},
		{"分越界", []byte{0x11, 0x07, 0x12, 0x60}},
	} {
		if _, err := Decode(it, c.raw); err == nil {
			t.Errorf("%s: Decode(% X) 被接受，应为错误", c.name, c.raw)
		}
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

// TestRejectsRequestDirectionAsReply 钉住「D7 方向位必须校验」。
//
// D7=1 是从站应答、D7=0 是主站请求。部分 USB 转 485 / 两线半双工适配器会把
// 主站刚发出的字节回显给接收端，于是主站会收到自己发出的请求帧。若解析器
// 只看功能码而不看 D7：
//
//   - 写请求（0x14）被当成 0x94 应答 → 写操作「成功」，设备其实没收到；
//   - 读请求（0x11）被当成 0x91 应答 → 数据域是「DI 回显 + 空数据」，
//     DI 校验能过，最后得到一条 0 字节的数据项。
//
// 两个方向都必须拒绝。回显的跳过由驱动的事务层负责（见 exchange）。
func TestRejectsRequestDirectionAsReply(t *testing.T) {
	di := mustDI(t, "04000101")
	addr := [AddrLen]byte{0x42}

	writeReq, err := WriteRequest(addr, di, 0x02, [3]byte{}, [4]byte{},
		[]byte{0x12, 0x00, 0x11, 0x07, 0x26, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	wfr, _, err := Parse(writeReq[PreambleLen:])
	if err != nil {
		t.Fatal(err)
	}
	if wfr.IsReply() {
		t.Fatal("写请求帧不应带 D7")
	}
	if err := WriteAck(wfr); err == nil {
		t.Fatal("主站自己发出的写请求被当成写成功——适配器回显时会让写操作假成功")
	}

	readReq, err := ReadRequest(addr, di)
	if err != nil {
		t.Fatal(err)
	}
	rfr, _, err := Parse(readReq[PreambleLen:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReadReply(rfr, di); err == nil {
		t.Fatal("读请求帧被当成读应答（数据域是 DI 回显 + 空数据）")
	}
	if _, err := ParseAddrReply(rfr); err == nil {
		t.Fatal("请求帧被当成读地址应答")
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
