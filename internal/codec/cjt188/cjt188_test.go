package cjt188

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.ReplaceAll(s, " ", "")
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func mustAddr(t *testing.T, s string) Addr {
	t.Helper()
	a, err := ParseAddr(s)
	if err != nil {
		t.Fatalf("addr %q: %v", s, err)
	}
	return a
}

// TestReadRequestGolden 钉住整帧字节：单 68、无 0x33、DI 高字节先传、CS 不含前导。
//
// 金帧取自 CJ/T 188-2004 标准表 5 与现场水表的真实抓包：地址 00000805000001、
// 读计量数据 901F、SER=0。数据标识按 2004 的大端书写，线上 DI0=0x90、DI1=0x1F
// （`03 90 1F 00`），与 2018 版的小端（`1F 90`）正好相反——这是最容易写反的一处。
func TestReadRequestGolden(t *testing.T) {
	addr := mustAddr(t, "00000805000001")
	fr, err := ReadRequest(0x10, addr, DIReadData, 0x00)
	if err != nil {
		t.Fatal(err)
	}
	want := mustHex(t, "FEFE681001000005080000"+"0103"+"901F"+"00"+"3916")
	if !bytes.Equal(fr, want) {
		t.Fatalf("request = %X, want %X", fr, want)
	}
	// 前导不计入 CS：把前导去掉后重新算一次，应与帧内 CS 相同。
	body := fr[PreambleLen:]
	if got, want := body[len(body)-2], Sum(body[:len(body)-2]); got != want {
		t.Fatalf("CS = %02X, want %02X", got, want)
	}
}

// TestReadReplyGolden 用一台冷水表的应答（累积流量 556677.00 m³）检验解码。
//
// 数据标识按 CJ/T 188-2004 的大端书写，与请求一致（应答回显同一 DI）。
// 交换两个 DI 字节不改变校验和（仍是 0x13），但会改变语义：1F 90 会被当成
// 一个不存在的数据标识，正确写法是 90 1F。
func TestReadReplyGolden(t *testing.T) {
	// 前导 FE×4 + 帧。末尾 CS 是按标准重算的 0x13（原始博客写 0x08，
	// 逐字节相加得到的是 0x13；这里以可验证的算法为准）。数据域 DI 为大端 90 1F。
	raw := mustHex(t, "FEFEFEFE"+
		"6810443322110033788116"+
		"901F00"+
		"007766552C"+"007766552C"+
		"31012211051520"+
		"2184"+
		"1316")
	fr, consumed, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// consumed 是整帧长度，不含前导；前导只是被跳过的噪声。
	if consumed != 35 {
		t.Fatalf("consumed = %d, want 35", consumed)
	}
	if fr.Type != 0x10 {
		t.Fatalf("type = %02X", fr.Type)
	}
	// A0..A4 = 44 33 22 11 00，A5/A6 = 33 78（厂商码），故 14 位为 7833+0011223344。
	if got := fr.Addr.String(); got != "78330011223344" {
		t.Fatalf("addr = %s", got)
	}
	if !fr.IsReply() || fr.IsError() {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
	reply, err := ParseReadReply(fr, DIReadData, 0x00)
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(reply.Payload) != 19 {
		t.Fatalf("payload = %d bytes, want 19", len(reply.Payload))
	}
	fields, _ := Fields(fr.Type, DIReadData)
	cur, err := LookupField(fr.Type, DIReadData, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := DecodeField(cur, reply.Payload[cur.Offset:cur.End()])
	if err != nil {
		t.Fatalf("decode current: %v", err)
	}
	if v != 556677 {
		t.Fatalf("current = %v, want 556677", v)
	}
	if got := FormatField(cur, reply.Payload[cur.Offset:cur.End()]); got != "556677 m³" {
		t.Fatalf("format = %q", got)
	}
	// 结算日累积流量与当前流量在这帧里相同。
	settle, _ := LookupField(fr.Type, DIReadData, 5)
	v, err = DecodeField(settle, reply.Payload[settle.Offset:settle.End()])
	if err != nil || v != 556677 {
		t.Fatalf("settle = %v, %v", v, err)
	}
	// 实时时间：2015-05-11 22:01:31。
	tm, _ := LookupField(fr.Type, DIReadData, 10)
	tv, err := DecodeField(tm, reply.Payload[tm.Offset:tm.End()])
	if err != nil {
		t.Fatalf("time: %v", err)
	}
	if tv != 20150511220131 {
		t.Fatalf("time = %v", tv)
	}
	if got := FormatField(tm, reply.Payload[tm.Offset:tm.End()]); got != "2015-05-11 22:01:31" {
		t.Fatalf("time format = %q", got)
	}
	// 状态：低字节 0x21 = 阀门关。
	st, _ := LookupField(fr.Type, DIReadData, 17)
	sv, err := DecodeField(st, reply.Payload[st.Offset:st.End()])
	if err != nil || sv != 0x8421 {
		t.Fatalf("status = %v, %v", sv, err)
	}
	if got := StatusText(uint16(sv)); !strings.Contains(got, "阀门关") {
		t.Fatalf("status text = %q", got)
	}
	if len(fields) != 4 {
		t.Fatalf("fields = %d", len(fields))
	}
}

// TestHeatSchema 校验热量表的字段布局：温度/工作时间是 3 字节无单位，
// 总宽 43 字节。把水表的 19 字节布局套到热量表上会Everything错位。
func TestHeatSchema(t *testing.T) {
	if w, err := TotalWidth(0x20, DIReadData); err != nil || w != 43 {
		t.Fatalf("heat width = %d, %v", w, err)
	}
	if w, err := TotalWidth(0x10, DIReadData); err != nil || w != 19 {
		t.Fatalf("water width = %d, %v", w, err)
	}
	supply, err := LookupField(0x20, DIReadData, 25)
	if err != nil {
		t.Fatal(err)
	}
	if supply.Length != 3 || supply.HasUnit() || supply.Decimals != 2 || supply.Name != "供水温度" {
		t.Fatalf("supply = %+v", supply)
	}
	// 55.00 ℃ → 5500。
	b, _ := EncodeBCD(5500, 3)
	if v, err := DecodeField(supply, b); err != nil || v != 55 {
		t.Fatalf("supply decode = %v, %v", v, err)
	}
	work, _ := LookupField(0x20, DIReadData, 31)
	b, _ = EncodeBCD(123456, 3)
	if v, err := DecodeField(work, b); err != nil || v != 123456 {
		t.Fatalf("work decode = %v, %v", v, err)
	}
	if got := FormatField(supply, mustHex(t, "005500")); got != "55 ℃" {
		t.Fatalf("supply format = %q", got)
	}
}

// TestUnitFactor 校验单位换算；未知代号必须失败而不是默认成基准单位。
func TestUnitFactor(t *testing.T) {
	cases := []struct {
		q    Qty
		code byte
		want float64
	}{
		{QtyVolume, 0x2C, 1},
		{QtyVolume, 0x29, 0.001},
		{QtyFlow, 0x35, 1},
		{QtyFlow, 0x32, 0.001},
		{QtyEnergy, 0x11, 1},
		{QtyEnergy, 0x13, 0.01},
		{QtyEnergy, 0x05, 0.0036},
		{QtyEnergy, 0x08, 3.6},
		{QtyEnergy, 0x0A, 0.036},
		{QtyEnergy, 0x01, 1e-9},
		{QtyPower, 0x17, 1},
		{QtyPower, 0x14, 0.001},
		{QtyPower, 0x1A, 1000},
	}
	for _, c := range cases {
		got, ok := UnitFactor(c.q, c.code)
		if !ok || got != c.want {
			t.Fatalf("%s %02X = %v, %v; want %v", c.q.Unit(), c.code, got, ok, c.want)
		}
	}
	for _, code := range []byte{0x00, 0x2B, 0x99} {
		if _, ok := UnitFactor(QtyVolume, code); ok {
			t.Fatalf("unknown volume unit %02X must fail", code)
		}
	}
}

// TestDecodeBCDRejectsNibbles 钉住「FF 不是数字」。
func TestDecodeBCDRejectsNibbles(t *testing.T) {
	if _, err := DecodeBCD([]byte{0x00, 0xFF}); err == nil {
		t.Fatal("0xFF must not decode as BCD")
	}
	if _, err := DecodeBCD([]byte{0x1A}); err == nil {
		t.Fatal("nibble A must not decode as BCD")
	}
	v, err := DecodeBCD([]byte{0x00, 0x77, 0x66, 0x55})
	if err != nil || v != 55667700 {
		t.Fatalf("bcd = %v, %v", v, err)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	raw := mustHex(t, "31012211051520")
	tm, err := ParseTime(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := tm.Format("2006-01-02 15:04:05"); got != "2015-05-11 22:01:31" {
		t.Fatalf("time = %s", got)
	}
	if !bytes.Equal(TimeBytes(tm), raw) {
		t.Fatalf("round trip = %X", TimeBytes(tm))
	}
	if v := CompactTime(tm); v != 20150511220131 {
		t.Fatalf("compact = %v", v)
	}
	if back, err := FromCompactTime(20150511220131); err != nil || !back.Equal(tm) {
		t.Fatalf("from compact = %v, %v", back, err)
	}
	for _, bad := range []string{
		"00000001132020", // 13 月
		"00000030022020", // 2 月 30 日
		"00002401012020", // 24 时
		"6a000001012020", // 非 BCD
	} {
		if _, err := ParseTime(mustHex(t, bad)); err == nil {
			t.Fatalf("time %s must fail", bad)
		}
	}
}

// TestReadAddr 读地址请求/应答：T 与地址全 AA，报文标识固定 81 0A。
func TestReadAddr(t *testing.T) {
	req, err := ReadAddrRequest(0x00)
	if err != nil {
		t.Fatal(err)
	}
	want := mustHex(t, "FEFE68AAAAAAAAAAAAAAAA0303810A00"+"4916")
	if !bytes.Equal(req, want) {
		t.Fatalf("read addr request = %X, want %X", req, want)
	}
	// 从站用自己的真实地址应答。
	addr := mustAddr(t, "00000805000001")
	resp, _ := Build(0x10, addr, CtrlReadAddr|BitReply, []byte{0x81, 0x0A, 0x00})
	fr, _, err := Parse(resp)
	if err != nil {
		t.Fatal(err)
	}
	typ, got, err := ParseAddrReply(fr)
	if err != nil {
		t.Fatal(err)
	}
	if typ != 0x10 || got != addr {
		t.Fatalf("type = %02X addr = %s", typ, got)
	}
	if got.String() != "00000805000001" {
		t.Fatalf("addr string = %s", got)
	}
}

func TestParseErrorReply(t *testing.T) {
	addr := mustAddr(t, "00000805000001")
	resp, _ := Build(0x10, addr, CtrlRead|BitReply|BitError, []byte{0x90, 0x1F, 0x07})
	fr, _, err := Parse(resp)
	if err != nil {
		t.Fatal(err)
	}
	er, err := ParseErrorReply(fr)
	if err != nil {
		t.Fatal(err)
	}
	if er.DI != DIReadData || er.SER != 0x07 {
		t.Fatalf("error reply = %+v", er)
	}
	if _, err := ParseReply(fr); err == nil {
		t.Fatal("ParseReply must reject exception frames")
	}
}

func TestParseDI(t *testing.T) {
	di, err := ParseDI("901F")
	if err != nil {
		t.Fatal(err)
	}
	if di.String() != "901F" {
		t.Fatalf("string = %s", di.String())
	}
	if b := di.Bytes(); b[0] != 0x90 || b[1] != 0x1F {
		t.Fatalf("wire = %X (want 901F)", b)
	}
	if back, err := DIFromBytes([]byte{0x90, 0x1F}); err != nil || back != di {
		t.Fatalf("from bytes = %v, %v", back, err)
	}
	for _, bad := range []string{"", "90", "901F0", "90G1"} {
		if _, err := ParseDI(bad); err == nil {
			t.Fatalf("DI %q must fail", bad)
		}
	}
}

// TestDIBigEndian2004 钉住 CJ/T 188-2004 的数据标识字节序契约。
//
// 2004 版 DI 是**大端**：手册写 901F，线上先传高字节 0x90（`90 1F`）。这与
// 地址/数值的「低字节先传」相反，是 DI 独有的例外；发成小端 `1F 90` 时帧本身
// 合法，但表把它当成不存在的标识而不应答——现场最难查的一类「静默无响应」。
//
// 这里同时钉住四个特殊报文标识，确保它们与 901F 走同一套变换、没有被单独写成
// 反序的字面量（早先正是那样埋下过整段写反的隐患）。2018 版才改成小端并更名
// SER→SEQ；本实现面向 2004，若将来接 2018 的表必须按版本分派，不能只翻字节。
func TestDIBigEndian2004(t *testing.T) {
	cases := []struct {
		di   DI
		wire [2]byte
	}{
		{DIReadData, [2]byte{0x90, 0x1F}},
		{DIValve, [2]byte{0xA0, 0x17}},
		{DIWriteAddr, [2]byte{0xA0, 0x18}},
		{DIReadAddr, [2]byte{0x81, 0x0A}},
	}
	for _, c := range cases {
		got := c.di.Bytes()
		if got != c.wire {
			t.Fatalf("%s wire = %X, want %X", c.di, got, c.wire)
		}
		back, err := DIFromBytes(c.wire[:])
		if err != nil || back != c.di {
			t.Fatalf("%X decode = %s, %v; want %s", c.wire, back, err, c.di)
		}
	}
	// 明确断言「不是小端」：把这行删掉就等于放弃了 2004 契约。
	if b := DIReadData.Bytes(); b[0] == 0x1F {
		t.Fatalf("901F 被写成小端 %X；2004 要求大端 901F", b)
	}
}

// TestHistoricalDI 校验上 1..12 月结算日量的动态 schema。
func TestHistoricalDI(t *testing.T) {
	for n := 1; n <= 12; n++ {
		di := DI{Hi: 0xD1, Lo: byte(0x1F + n)}
		w, err := TotalWidth(0x10, di)
		if err != nil || w != 5 {
			t.Fatalf("D12%d width = %d, %v", n-1, w, err)
		}
		f, err := LookupField(0x10, di, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := "上 " + itoa(n) + " 月结算日累积流量"
		if f.Name != want {
			t.Fatalf("name = %q, want %q", f.Name, want)
		}
		if f.Qty != QtyVolume {
			t.Fatalf("qty = %v", f.Qty)
		}
		hf, _ := LookupField(0x20, di, 0)
		if hf.Qty != QtyEnergy {
			t.Fatalf("heat qty = %v", hf.Qty)
		}
	}
	// D12B 是上 12 月；D12C 不存在。
	if _, err := LookupField(0x10, DI{Hi: 0xD1, Lo: 0x2C}, 0); err == nil {
		t.Fatal("D12C must fail")
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestUnknownDI(t *testing.T) {
	if _, err := LookupField(0x10, DI{Hi: 0x91, Lo: 0x1F}, 0); err == nil {
		t.Fatal("911F is not implemented; must fail")
	}
	// 报错要把可用偏移列出来，方便现场改点表。
	_, err := LookupField(0x10, DIReadData, 2)
	if err == nil || !strings.Contains(err.Error(), "0（当前累积流量）") {
		t.Fatalf("error = %v", err)
	}
}

// TestPreambleOutsideChecksum 前导可变，但不影响 CS。
func TestPreambleOutsideChecksum(t *testing.T) {
	addr := mustAddr(t, "00000805000001")
	fr, _ := ReadRequest(0x10, addr, DIReadData, 0x00)
	for _, n := range []int{0, 2, 4} {
		buf := make([]byte, 0, n+len(fr))
		buf = append(buf, bytes.Repeat([]byte{Preamble}, n)...)
		buf = append(buf, fr[PreambleLen:]...)
		if _, _, err := Parse(buf); err != nil {
			t.Fatalf("preamble %d: %v", n, err)
		}
	}
}

// TestParseRecoversFromNoise 校验失败后只跳过 1 字节，后面那帧好数据
// 不能被一起丢掉。
func TestParseRecoversFromNoise(t *testing.T) {
	good, _ := Build(0x10, mustAddr(t, "00000805000001"), CtrlRead|BitReply,
		[]byte{0x90, 0x1F, 0x00, 0x00, 0x00, 0x00, 0x00})
	bad := append([]byte(nil), good...)
	bad[len(bad)-3] ^= 0xFF // 破坏 CS
	buf := append([]byte{0x12, 0x34}, bad...)
	buf = append(buf, good...)
	fr, _, err := Parse(buf)
	if err == nil {
		_ = fr
	}
	// 用 Reader 的循环跑一遍：它应当跳过坏帧并返回后面那帧。
	rd := &Reader{buf: buf, maxFrame: MaxFrame}
	fr, err = rd.Next()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if fr.Ctrl != CtrlRead|BitReply {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
}

func TestWriteRequests(t *testing.T) {
	addr := mustAddr(t, "00000805000001")
	// 阀门：DI A017（线上 A0 17）+ SER + 55/99。
	open, err := ValveRequest(0x10, addr, 0x01, true)
	if err != nil {
		t.Fatal(err)
	}
	fr, _, err := Parse(open)
	if err != nil {
		t.Fatal(err)
	}
	di, _ := DIFromBytes(fr.Data[:2])
	if di != DIValve || fr.Ctrl != CtrlWrite {
		t.Fatalf("valve frame = %X (DI %s ctrl %02X)", open, di, fr.Ctrl)
	}
	if !bytes.Equal(fr.Data, []byte{0xA0, 0x17, 0x01, 0x55}) {
		t.Fatalf("valve data = %X", fr.Data)
	}
	closeFr, _ := ValveRequest(0x10, addr, 0x01, false)
	fr, _, _ = Parse(closeFr)
	if fr.Data[3] != 0x99 {
		t.Fatalf("close data = %X", fr.Data)
	}

	// 写地址：DI A018（线上 A0 18）+ SER + 7 字节新地址。
	next := mustAddr(t, "00000805000099")
	wa, err := WriteAddrRequest(0x10, addr, 0x02, next)
	if err != nil {
		t.Fatal(err)
	}
	fr, _, err = Parse(wa)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Ctrl != CtrlWriteAddr || len(fr.Data) != 10 {
		t.Fatalf("write addr ctrl = %02X len = %d", fr.Ctrl, len(fr.Data))
	}
	if !bytes.Equal(fr.Data[:3], []byte{0xA0, 0x18, 0x02}) {
		t.Fatalf("write addr head = %X", fr.Data[:3])
	}
	if !bytes.Equal(fr.Data[3:], next[:]) {
		t.Fatalf("write addr payload = %X", fr.Data[3:])
	}
}

func TestAddrValidation(t *testing.T) {
	if _, err := ParseAddr("123"); err == nil {
		t.Fatal("short address must fail")
	}
	if _, err := ParseAddr("0000080500000A"); err == nil {
		t.Fatal("non-digit address must fail")
	}
	a := mustAddr(t, "00000805000001")
	if a.HasWildcard() || a.IsBroadcast() {
		t.Fatalf("addr %s", a)
	}
	if !BroadcastAddr.IsBroadcast() || !BroadcastAddr.HasWildcard() {
		t.Fatal("broadcast helpers")
	}
}
