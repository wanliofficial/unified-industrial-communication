package knx

import (
	"bytes"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"testing"

	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// 组地址
// ---------------------------------------------------------------------------

func TestParseGroupAddress(t *testing.T) {
	cases := []struct {
		in   string
		raw  uint16
		want string
	}{
		{"1/1/45", 0x092D, "1/1/45"},
		{"0/0/1", 0x0001, "0/0/1"},
		{"31/7/255", 0xFFFF, "31/7/255"},
		{"1/300", 1<<11 | 300, "1/1/44"},
		{"0x092D", 0x092D, "1/1/45"},
		{"2350", 2350, "1/1/46"},
		{"  1 / 2 / 3  ", 0x0A03, "1/2/3"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			g, err := ParseGroupAddress(tc.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if uint16(g) != tc.raw {
				t.Fatalf("raw = 0x%04X, want 0x%04X", uint16(g), tc.raw)
			}
			if got := g.String(); got != tc.want {
				t.Fatalf("String = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseGroupAddressRejects(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "empty"},
		{"0/0/0", "not a valid group address"},
		{"0/0", "not a valid group address"},
		{"32/0/1", "main group must be 0..31"},
		{"1/8/1", "middle group must be 0..7"},
		{"1/0/256", "sub group must be 0..255"},
		{"1/2048", "sub group must be 0..2047"},
		{"1/2/3/4", "want 主组"},
		{"a/b/c", "main group"},
		{"-1/2/3", "negative"},
		{"65536", "exceeds 16 bits"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseGroupAddress(tc.in)
			if err == nil {
				t.Fatalf("expected an error for %q", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestGroupAddressComponents(t *testing.T) {
	g := NewGroupAddress(5, 3, 17)
	if g.Main() != 5 || g.Middle() != 3 || g.Sub() != 17 {
		t.Fatalf("components = %d/%d/%d", g.Main(), g.Middle(), g.Sub())
	}
	if got := g.String(); got != "5/3/17" {
		t.Fatalf("String = %q", got)
	}
	// 两级形式与三级形式指向同一批低 11 位。
	if NewGroupAddress2(5, 3<<8|17) != g {
		t.Fatal("two-level and three-level forms must agree")
	}
}

func TestParseIndividualAddress(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1.0.250", "1.0.250"},
		{"15.15.255", "15.15.255"},
		{"1.1", "0.1.1"},
		{"0.0.1", "0.0.1"},
		{"0x10FA", "1.0.250"},
	}
	for _, tc := range cases {
		a, err := ParseIndividualAddress(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got := a.String(); got != tc.want {
			t.Fatalf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "16.0.1", "1.16.1", "1.0.256", "1.2.3.4"} {
		if _, err := ParseIndividualAddress(bad); err == nil {
			t.Fatalf("expected an error for %q", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// DPT：金字节
// ---------------------------------------------------------------------------

func TestDPTParse(t *testing.T) {
	d, err := ParseDPT("9.001")
	if err != nil {
		t.Fatal(err)
	}
	if d.Main != 9 || d.Sub != 1 || d.Kind != KindFloat16 || d.Bits != 16 {
		t.Fatalf("dpt = %+v", d)
	}
	if got := d.String(); got != "9.001" {
		t.Fatalf("String = %q", got)
	}
	// 大小写与前缀都接受。
	for _, in := range []string{"DPT9.001", "dpt9.001", " 9.001 ", "9.001 "} {
		if _, err := ParseDPT(in); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
	// 只写主类型时走通用形式。
	generic, err := ParseDPT("9")
	if err != nil {
		t.Fatal(err)
	}
	if generic.Kind != KindFloat16 || generic.Bits != 16 {
		t.Fatalf("generic dpt 9 = %+v", generic)
	}
	// 通配写法等价于主类型。
	for _, in := range []string{"9.*", "9.xxx"} {
		if g, err := ParseDPT(in); err != nil || g.Kind != KindFloat16 {
			t.Fatalf("%q: %+v %v", in, g, err)
		}
	}
}

func TestDPTParseRejects(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "dpt is required"},
		{"DPT", "missing type number"},
		{"abc", "not a number"},
		{"9.abc", "not a number"},
		{"0.001", "must be positive"},
		{"99.999", "not supported"},
		{"2.001", "not supported"},
	}
	for _, tc := range cases {
		_, err := ParseDPT(tc.in)
		if err == nil {
			t.Fatalf("expected an error for %q", tc.in)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: error = %v, want substring %q", tc.in, err, tc.want)
		}
	}
}

// TestDPTFloat16Golden 钉住 2 字节浮点的线格式。
//
// 这不是 IEEE 半精度：1 位符号 + 4 位指数 + 11 位**补码**尾数，
// 值 = 尾数 * 2^指数 / 100。用错误的解码会把 21.5 读成一个完全无关的数。
//
// 期望值逐个手算过：
//
//	21.5,   *100 = 2150 > 2047 → 指数 1、尾数 1075 → 0x0C33
//	20.48,  *100 = 2048 > 2047 → 指数 1、尾数 1024 → 0x0C00
//	-20.48, *100 = -2048       → 指数 0、尾数 -2048 → 0x8000
//	-1,     *100 = -100        → 指数 0、尾数 -100  → 0x879C
//	0                          → 0x0000
func TestDPTFloat16Golden(t *testing.T) {
	d, err := ParseDPT("9.001")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		value float64
		hex   string
	}{
		{21.5, "0c33"},
		{20.48, "0c00"},
		{-20.48, "8000"},
		{1, "0064"},
		{-1, "879c"},
		{0, "0000"},
		// 上界 2046 * 2^15 / 100 = 670433.28，编码 0x7FFE——0x7FFF 是保留的
		// 无效数据，因此这个界必须落在 0x7FFE 而不是 0x7FFF。
		{670433.28, "7ffe"},
	}
	for _, tc := range cases {
		t.Run(tc.hex, func(t *testing.T) {
			p, err := d.Encode(model.NumValue(model.TypeFloat32, tc.value), model.TypeFloat32)
			if err != nil {
				t.Fatalf("encode %v: %v", tc.value, err)
			}
			if got := hex.EncodeToString(p.Data); got != tc.hex {
				t.Fatalf("encode %v = %s, want %s", tc.value, got, tc.hex)
			}
			v, err := d.Decode(p, model.TypeFloat32)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if math.Abs(v.Num-tc.value) > 0.005 {
				t.Fatalf("round trip %v -> %v", tc.value, v.Num)
			}
		})
	}
}

// 0x7FFF 是标准保留的「无效数据」。把它解成 3276.75 是一类经典事故。
func TestDPTFloat16Invalid(t *testing.T) {
	d, _ := ParseDPT("9.001")
	_, err := d.Decode(Payload{Data: []byte{0x7F, 0xFF}}, model.TypeFloat32)
	if err == nil {
		t.Fatal("0x7FFF must be rejected as invalid data")
	}
	if !strings.Contains(err.Error(), "invalid data") {
		t.Fatalf("error = %v", err)
	}
}

func TestDPTBool(t *testing.T) {
	d, _ := ParseDPT("1.001")
	p, err := d.Encode(model.BoolValue(true), model.TypeBool)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Bit || len(p.Data) != 0 {
		t.Fatalf("payload = %+v", p)
	}
	// 0/1 也接受：点位声明为 bool、而命令行给了 1 是常见写法。
	// 注意「声明类型」仍是 bool——DPT 1.001 的线宽决定了这一点，
	// 数值 1 只是同一个值换了种写法。
	if p, err := d.Encode(model.NumValue(model.TypeUint16, 1), model.TypeBool); err != nil || !p.Bit {
		t.Fatalf("numeric 1: %+v %v", p, err)
	}
	// 2 必须被拒绝，而不是静默当 true。
	if _, err := d.Encode(model.NumValue(model.TypeUint16, 2), model.TypeBool); err == nil {
		t.Fatal("2 must not be accepted as bool")
	}
	// 把 1 位 DPT 声明成数值类型必须拒绝：线宽对不上，无论值是什么。
	if _, err := d.Encode(model.NumValue(model.TypeUint16, 1), model.TypeUint16); err == nil {
		t.Fatal("dpt 1 must not accept dataType uint16")
	}
	// 用 bool 类型声明 9.001 必须拒绝：线宽是 2 字节，不是 1 位。
	d9, _ := ParseDPT("9.001")
	if _, err := d9.Encode(model.NumValue(model.TypeFloat32, 1), model.TypeBool); err == nil {
		t.Fatal("dpt 9 must not accept dataType bool")
	}
}

func TestDPTScaled(t *testing.T) {
	d, _ := ParseDPT("5.001") // 百分比 0..100 -> 0..255
	p, err := d.Encode(model.NumValue(model.TypeFloat32, 50), model.TypeFloat32)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p.Data); got != "80" {
		// 50 / (100/255) = 127.5 -> 128 = 0x80
		t.Fatalf("encode 50%% = %s, want 80", got)
	}
	v, err := d.Decode(p, model.TypeFloat32)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v.Num-50.196) > 0.01 {
		t.Fatalf("decode = %v", v.Num)
	}
	// 越界必须拒绝。
	if _, err := d.Encode(model.NumValue(model.TypeFloat32, 101), model.TypeFloat32); err == nil {
		t.Fatal("101%% must be rejected for dpt 5.001")
	}
}

func TestDPTSignedUnsigned(t *testing.T) {
	s, _ := ParseDPT("8.001")
	p, _ := s.Encode(model.NumValue(model.TypeInt16, -2), model.TypeInt16)
	if got := hex.EncodeToString(p.Data); got != "fffe" {
		t.Fatalf("dpt 8.001 -2 = %s", got)
	}
	v, err := s.Decode(Payload{Data: []byte{0xFF, 0xFE}}, model.TypeInt16)
	if err != nil || v.Num != -2 {
		t.Fatalf("decode = %v %v", v, err)
	}

	u, _ := ParseDPT("7.001")
	p, _ = u.Encode(model.NumValue(model.TypeUint16, 65535), model.TypeUint16)
	if got := hex.EncodeToString(p.Data); got != "ffff" {
		t.Fatalf("dpt 7.001 65535 = %s", got)
	}
	// 32 位类型不能塞进 16 位目标类型，也不能反过来把 32 位数读进 uint16。
	if err := u.Accepts(model.TypeUint16); err != nil {
		t.Fatalf("uint16 must accept dpt 7: %v", err)
	}
	big, _ := ParseDPT("12.001")
	if err := big.Accepts(model.TypeUint16); err == nil {
		t.Fatal("dpt 12 must not accept uint16")
	}
}

func TestDPTFloat32(t *testing.T) {
	d, _ := ParseDPT("14.056")
	p, err := d.Encode(model.NumValue(model.TypeFloat32, 1234.5), model.TypeFloat32)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p.Data); got != "449a5000" {
		t.Fatalf("encode 1234.5 = %s", got)
	}
	v, err := d.Decode(p, model.TypeFloat32)
	if err != nil || math.Abs(v.Num-1234.5) > 1e-6 {
		t.Fatalf("decode = %v %v", v, err)
	}
}

func TestDPTRejectsNonNumeric(t *testing.T) {
	d, _ := ParseDPT("9.001")
	for _, dt := range []model.DataType{model.TypeString, model.TypeBytes, model.TypeBCD16} {
		if _, err := d.Encode(model.TextValue(dt, "x"), dt); err == nil {
			t.Fatalf("dpt 9 must reject %s", dt)
		}
	}
}

func TestDPTNamesSorted(t *testing.T) {
	names := DPTNames()
	if len(names) < 20 {
		t.Fatalf("too few DPTs: %d", len(names))
	}
	prev := 0
	for _, n := range names {
		main, _, _ := strings.Cut(n, ".")
		m, err := strconv.Atoi(main)
		if err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		if m < prev {
			t.Fatalf("%v is not sorted by main type", names)
		}
		prev = m
	}
}

func TestDPTBytes(t *testing.T) {
	cases := map[string]int{
		"1.001": 0, "5.001": 1, "9.001": 2, "13.010": 4, "14.056": 4,
	}
	for dp, want := range cases {
		d, err := ParseDPT(dp)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Bytes(); got != want {
			t.Fatalf("%s Bytes() = %d, want %d", dp, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// cEMI：真实捕获帧
// ---------------------------------------------------------------------------

// TestParseLDataRealCapture 使用一条真实的 Wireshark 捕获帧：
//
//	06 10 04 20 00 15 | 04 02 51 00 | 29 00 bc e0 10 fa 09 2d 01 00 80
//
// 它是网关上报的 L_Data.ind：源 1.0.250、目的组地址 1/1/45、
// GroupValueWrite 1 位值 false。这条帧同时钉住了四件事：
//
//  1. TPDU 长度字段是 01，而 APDU 实际有 2 个字节——长度**不含** TPCI 字节。
//  2. 控制字段 2 的 0xE0 里，bit7=组地址、bit6-4=6 跳（0xE0>>4&7 == 6）。
//  3. 1 位值在 APCI 字节 bit0，没有数据域。
//  4. HPAI 之外的载荷长度必须按总长度字段切。
func TestParseLDataRealCapture(t *testing.T) {
	raw, err := hex.DecodeString("06100420" + "0015" + "0402" + "5100" +
		"2900bce010fa092d010080")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}
	if f.Service != SvcTunnellingRequest {
		t.Fatalf("service = %04X", f.Service)
	}
	if !bytes.Equal(f.Payload, mustHex("04025100"+"2900bce010fa092d010080")) {
		t.Fatalf("payload = %X", f.Payload)
	}

	req, err := ParseTunnellingRequest(f.Payload)
	if err != nil {
		t.Fatalf("ParseTunnellingRequest: %v", err)
	}
	if req.Channel != 0x02 || req.Seq != 0x51 {
		t.Fatalf("channel/seq = %d/%d", req.Channel, req.Seq)
	}

	m, err := ParseLData(req.CEMI)
	if err != nil {
		t.Fatalf("ParseLData: %v", err)
	}
	if m.MsgCode != MsgLDataInd {
		t.Fatalf("msg = %02X", m.MsgCode)
	}
	if m.Control1 != 0xBC || m.Control2 != 0xE0 {
		t.Fatalf("control = %02X %02X", m.Control1, m.Control2)
	}
	if !m.IsGroup {
		t.Fatal("must be a group-addressed frame")
	}
	if m.Source.String() != "1.0.250" {
		t.Fatalf("source = %s", m.Source)
	}
	g := GroupAddress(m.Dest)
	if g.String() != "1/1/45" {
		t.Fatalf("dest = %s", g)
	}
	if m.APCI != APCIGroupValueWrite {
		t.Fatalf("apci = %04X", m.APCI)
	}
	if m.Bit {
		t.Fatal("captured value is false")
	}
	if len(m.Data) != 0 {
		t.Fatalf("1-bit frame must carry no data, got %X", m.Data)
	}
}

// TestBuildLDataRoundTrip 钉住 1 位与 2 字节 DPT 的 TPDU 长度字段。
func TestBuildLDataRoundTrip(t *testing.T) {
	src, _ := ParseIndividualAddress("1.1.250")
	g, _ := ParseGroupAddress("1/2/3")

	// 1 位：TPDU = TPCI(00) + APCI(80|01) = 2 字节，长度字段 = 1。
	raw, err := BuildLData(MsgLDataReq, uint16(src), uint16(g), true, APCIGroupValueWrite, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	want := mustHex("1100" + "86e0" + "11fa" + "0a03" + "01" + "0081")
	if !bytes.Equal(raw, want) {
		t.Fatalf("bool frame = %X, want %X", raw, want)
	}

	// 2 字节：TPDU = TPCI(00) + APCI(80) + 2 数据字节，长度字段 = 3。
	raw, err = BuildLData(MsgLDataReq, uint16(src), uint16(g), true, APCIGroupValueWrite,
		[]byte{0x0C, 0x33}, false)
	if err != nil {
		t.Fatal(err)
	}
	want = mustHex("1100" + "86e0" + "11fa" + "0a03" + "03" + "0080" + "0c33")
	if !bytes.Equal(raw, want) {
		t.Fatalf("2-byte frame = %X, want %X", raw, want)
	}

	m, err := ParseLData(raw)
	if err != nil {
		t.Fatalf("ParseLData: %v", err)
	}
	if m.APCI != APCIGroupValueWrite || !bytes.Equal(m.Data, []byte{0x0C, 0x33}) {
		t.Fatalf("round trip = %+v", m)
	}

	// GroupValueRead 没有值：长度字段 = 1，APCI 低字节全零。
	raw, err = BuildLData(MsgLDataReq, uint16(src), uint16(g), true, APCIGroupValueRead, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(raw[len(raw)-3:]); got != "0100"+"00" {
		t.Fatalf("read tail = %s, want 0100 00", got)
	}
}

func TestParseLDataRejects(t *testing.T) {
	cases := []struct {
		name string
		hex  string
		want string
	}{
		{"too short", "2900", "truncated"},
		{"bad msg code", "3000bce0" + "11010a03" + "010080", "unexpected cEMI message code"},
		{"tpdu truncated", "2900bce0" + "11010a03" + "03" + "0080", "TPDU truncated"},
		{"zero length", "2900bce0" + "11010a03" + "00" + "0080", "TPDU length 0 is invalid"},
		{"control tpdu", "2900bce0" + "11010a03" + "01" + "8000", "control TPDU"},
		{"short with info", "2905", "truncated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseLData(mustHex(tc.hex))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// cEMI 的附加信息段必须被跳过，而不是当成控制字段。
func TestParseLDataSkipsInfo(t *testing.T) {
	// 附加信息 2 字节（ff ff），其余与上面的 1 位写帧相同。
	raw := mustHex("2902" + "ffff" + "86e0" + "1101" + "0a03" + "01" + "0081")
	m, err := ParseLData(raw)
	if err != nil {
		t.Fatalf("ParseLData: %v", err)
	}
	if m.Control1 != 0x86 || m.Dest != 0x0A03 || !m.Bit {
		t.Fatalf("payload = %+v", m)
	}
}

func TestDecodeGroupValue(t *testing.T) {
	d9, _ := ParseDPT("9.001")
	frame := LData{APCI: APCIGroupValueResponse, Data: []byte{0x0C, 0x33}}
	p, err := DecodeGroupValue(frame, d9)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d9.Decode(p, model.TypeFloat32); err != nil || math.Abs(v.Num-21.5) > 0.005 {
		t.Fatalf("value = %v %v", v, err)
	}

	// GroupValueRead 没有值。
	if _, err := DecodeGroupValue(LData{APCI: APCIGroupValueRead}, d9); err == nil {
		t.Fatal("GroupValueRead must not yield a value")
	}
	// 位宽不符要拒绝：数据域 2 字节但 DPT 是 1 位。
	d1, _ := ParseDPT("1.001")
	if _, err := DecodeGroupValue(LData{APCI: APCIGroupValueWrite, Data: []byte{1, 2}}, d1); err == nil {
		t.Fatal("1-bit DPT with a data field must be rejected")
	}
	// 反之亦然。
	if _, err := DecodeGroupValue(LData{APCI: APCIGroupValueWrite, Data: []byte{1}}, d9); err == nil {
		t.Fatal("9.001 with 1 data byte must be rejected")
	}
}

// ---------------------------------------------------------------------------
// KNXnet/IP 信封
// ---------------------------------------------------------------------------

func TestFrameRoundTrip(t *testing.T) {
	payload := mustHex("0402" + "5100" + "2900bce010fa092d010080")
	raw := BuildFrame(SvcTunnellingRequest, payload)
	// 总长度字段含 6 字节头部：4(连接头) + 11(cEMI) + 6 = 21 = 0x0015。
	if got := hex.EncodeToString(raw[:6]); got != "061004200015" {
		t.Fatalf("header = %s", got)
	}
	f, err := ParseFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Service != SvcTunnellingRequest || !bytes.Equal(f.Payload, payload) {
		t.Fatalf("frame = %+v", f)
	}
}

func TestParseFrameRejects(t *testing.T) {
	cases := []struct {
		name string
		hex  string
		want string
	}{
		{"short", "06100420", "too short"},
		{"bad header len", "0a1004200015", "header length"},
		{"bad version", "061104200015", "protocol version"},
		{"total < header", "061004200005", "smaller than the header"},
		{"truncated", "061004200015" + "0402", "exceeds the received"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFrame(mustHex(tc.hex))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConnectRequestRoundTrip(t *testing.T) {
	h, err := NewHPAI("192.168.1.10:50000")
	if err != nil {
		t.Fatal(err)
	}
	req := ConnectRequest{Control: h, Data: h}
	body := req.Encode()
	if len(body) != 2*HPALen+4 {
		t.Fatalf("body length = %d", len(body))
	}
	// CRI 固定为 04 04 02 00：长度 4、隧道连接、链路层、保留。
	if got := hex.EncodeToString(body[2*HPALen:]); got != "04040200" {
		t.Fatalf("CRI = %s", got)
	}
	got, err := ParseConnectRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Control.String() != "192.168.1.10:50000" || got.Layer != LayerLink {
		t.Fatalf("parsed = %+v", got)
	}

	// 非隧道连接类型必须拒绝，而不是当成隧道继续跑。
	bad := append(append([]byte(nil), body[:2*HPALen]...), 4, ConnTypeDeviceMgmt, LayerLink, 0)
	if _, err := ParseConnectRequest(bad); err == nil {
		t.Fatal("device management connection must be rejected")
	}
}

func TestConnectResponseRoundTrip(t *testing.T) {
	h, _ := NewHPAI("192.168.1.10:50000")
	resp := ConnectResponse{Channel: 7, Data: h, Assigned: 0x11FA, HasCRD: true}
	body := resp.Encode()
	got, err := ParseConnectResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel != 7 || got.Status != StatusNoError || !got.HasCRD {
		t.Fatalf("parsed = %+v", got)
	}
	if got.Assigned.String() != "1.1.250" {
		t.Fatalf("assigned = %s", got.Assigned)
	}

	// 失败响应只有 2 字节，不能带 HPAI。
	fail := ConnectResponse{Channel: 0, Status: StatusNoMoreConnections}
	if body := fail.Encode(); len(body) != 2 {
		t.Fatalf("failure body = %X", body)
	}
}

func TestTunnellingACK(t *testing.T) {
	ack := TunnellingACK{Channel: 2, Seq: 0x51}
	body := ack.Encode()
	if got := hex.EncodeToString(body); got != "04025"+"100" {
		t.Fatalf("ack = %s", got)
	}
	got, err := ParseTunnellingACK(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel != 2 || got.Seq != 0x51 || got.Status != StatusNoError {
		t.Fatalf("parsed = %+v", got)
	}
	if _, err := ParseTunnellingACK([]byte{3, 0, 0, 0}); err == nil {
		t.Fatal("structure length other than 4 must be rejected")
	}
}

func TestConnectionStateAndDisconnect(t *testing.T) {
	h, _ := NewHPAI("127.0.0.1:3671")

	state := ConnectionStateRequest{Channel: 3, Control: h}
	got, err := ParseConnectionStateRequest(state.Encode())
	if err != nil || got.Channel != 3 || got.Control.Port != 3671 {
		t.Fatalf("state = %+v %v", got, err)
	}

	res := ConnectionStateResponse{Channel: 3}
	if b := res.Encode(); len(b) != 2 || b[1] != StatusNoError {
		t.Fatalf("state response = %X", b)
	}

	disc := DisconnectRequest{Channel: 3, Control: h}
	gd, err := ParseDisconnectRequest(disc.Encode())
	if err != nil || gd.Channel != 3 {
		t.Fatalf("disconnect = %+v %v", gd, err)
	}
	if b := (DisconnectResponse{Channel: 3}).Encode(); len(b) != 2 {
		t.Fatalf("disconnect response = %X", b)
	}
}

// route-back 是 NAT 场景下唯一能用的 HPAI 写法：全零 IP + 0 端口。
func TestHPAIRouteBack(t *testing.T) {
	h, _, err := ParseHPAI([]byte{8, ProtoUDP4, 0, 0, 0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if !h.RouteBack() {
		t.Fatalf("expected route-back, got %+v", h)
	}
	if got := h.String(); got != "route-back" {
		t.Fatalf("String = %q", got)
	}
	// 编码回去仍然是全零。
	if got := hex.EncodeToString(AppendHPAI(nil, h)); got != "0801000000000000" {
		t.Fatalf("encoded = %s", got)
	}
}

func TestHPAIRejects(t *testing.T) {
	if _, err := NewHPAI("not-an-ip:3671"); err == nil {
		t.Fatal("bad IP must be rejected")
	}
	if _, err := NewHPAI("192.168.1.1"); err == nil {
		t.Fatal("missing port must be rejected")
	}
	if _, _, err := ParseHPAI([]byte{7, 1, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("wrong structure length must be rejected")
	}
}

func TestStatusName(t *testing.T) {
	if got := StatusName(StatusNoMoreConnections); !strings.Contains(got, "隧道连接数已满") {
		t.Fatalf("status name = %q", got)
	}
	if got := StatusName(0xEE); !strings.Contains(got, "0xEE") {
		t.Fatalf("unknown status = %q", got)
	}
	if got := ServiceName(SvcTunnellingRequest); got != "TUNNELLING_REQUEST" {
		t.Fatalf("service name = %q", got)
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
