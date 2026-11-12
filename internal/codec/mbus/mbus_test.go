package mbus

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"testing"
)

// 本文件是 codec 的金字节测试。它守四件事：
//
//  1. 组帧/解帧的字节产出一字节不差（校验和覆盖范围最容易写错）；
//  2. 真实报文的解析结果与手工核对的值一致（VIF 表 / BCD / 日期）；
//  3. 坏帧必须被识别成"坏帧"，而不是被当成好帧解出一堆鬼数；
//  4. `q:` 寻址在同名记录、历史记录、功能位后缀下都能选对。

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("测试向量不是合法十六进制：%v", err)
	}
	return b
}

func TestBuildShortAndAck(t *testing.T) {
	// SND_NKE 到地址 1：10 40 01 41 16。校验和是 C+A 的模 256 和。
	if got := hex.EncodeToString(BuildShort(CtrlSNDNKE, 1)); got != "1040014116" {
		t.Fatalf("SND_NKE 短帧 = %s，应为 1040014116", got)
	}
	// REQ_UD2 到地址 5：C=5B，A=05，CS=60。
	if got := hex.EncodeToString(BuildShort(CtrlREQUD2NoFCB, 5)); got != "105b056016" {
		t.Fatalf("REQ_UD2 短帧 = %s，应为 105b056016", got)
	}
	if got := hex.EncodeToString(BuildAck()); got != "e5" {
		t.Fatalf("ACK = %s，应为 e5", got)
	}
	// 校验和回绕：C+A = 0x5B+0x00 = 0x5B；换一个会溢出的组合。
	if got := hex.EncodeToString(BuildShort(0xFF, 0x02)); got != "10ff020116" {
		t.Fatalf("回绕校验和 = %s，应为 10ff020116（0xFF+0x02=0x01）", got)
	}
}

func TestBuildLongChecksumRange(t *testing.T) {
	// 校验和只覆盖 C/A/CI/数据，不含 68 L L 68。用 3 字节数据域手算
	// 校验和，锁住"覆盖范围"。
	payload := []byte{1, 2, 3}
	fr := BuildLong(CtrlRSPUD, 0x01, CIVariableData, payload)
	// C+A+CI+1+2+3 = 0x08+0x01+0x72+0x06 = 0x81
	want := []byte{0x68, 0x06, 0x06, 0x68, 0x08, 0x01, 0x72, 1, 2, 3, 0x81, 0x16}
	if !bytes.Equal(fr, want) {
		t.Fatalf("长帧 = %x，应为 %x", fr, want)
	}
	// 解回来必须一字节不差。
	got, n, err := Parse(fr)
	if err != nil {
		t.Fatalf("解析自己组的帧失败：%v", err)
	}
	if n != len(fr) || got.CIField != CIVariableData || !bytes.Equal(got.Data, payload) {
		t.Fatalf("回读不一致：%+v n=%d", got, n)
	}
}

// goldenResponse 是一条典型热表的 RSP_UD 报文（CI=72h）。
//
// 报文内容（与下面的断言一一对应）：
//
//	表号 12345678、厂商 3A63、版本 1、介质 04 热、访问号 0x15、状态 0
//	能量 1234567 Wh、累计量 800.0 m^3、流量温度 90.5 °C、回水温度 56.1 °C
//	瞬时流量 0.25 m^3/h、瞬时功率 24484 W、结算日期 2024-03-15
const goldenResponse = "68 30 30 68 08 01 72 " +
	"78 56 34 12 3A 63 01 04 15 00 00 00 " +
	"04 03 87 D6 12 00 " +
	"04 13 00 35 0C 00 " +
	"02 5A 89 03 " +
	"02 5E 31 02 " +
	"02 3B FA 00 " +
	"04 2B A4 5F 00 00 " +
	"02 6C 0F 33"

func goldenFrame(t *testing.T) []byte {
	t.Helper()
	clean := bytes.ReplaceAll([]byte(goldenResponse), []byte(" "), nil)
	b := mustHex(t, string(clean))
	// 长度与校验和都按实际内容重算：手抄报文时最容易漏改这两个字节，
	// 让它们在测试里自动对齐，断言才能专注于"记录解出来对不对"。
	if len(b) < 8 {
		t.Fatal("向量过短")
	}
	b = append(b, 0, Stop) // 校验和与结束符占位，下面按内容填
	length := len(b) - 6
	b[1], b[2] = byte(length), byte(length)
	b[len(b)-2] = Checksum(b[4], b[5], b[6], b[7:4+length])
	return b
}

func TestParseGoldenVariable(t *testing.T) {
	fr, _, err := Parse(goldenFrame(t))
	if err != nil {
		t.Fatalf("解析典型热表报文失败：%v", err)
	}
	resp, err := ParseResponse(fr)
	if err != nil {
		t.Fatalf("解析应答失败：%v", err)
	}
	if resp.Kind != DataVariable {
		t.Fatalf("kind = %v，应为 variable", resp.Kind)
	}
	h := resp.Header
	if h.IDText != "12345678" {
		t.Errorf("表号 = %q，应为 12345678", h.IDText)
	}
	if h.ManufacturerID != "XYZ" {
		t.Errorf("厂商 = %q，应为 XYZ", h.ManufacturerID)
	}
	if h.Medium != MediumHeat || h.MediumName != "heat" {
		t.Errorf("介质 = %02X/%q，应为 04/heat", h.Medium, h.MediumName)
	}
	if h.AccessNo != 0x15 {
		t.Errorf("访问号 = %d，应为 21", h.AccessNo)
	}
	if h.StatusError() {
		t.Errorf("状态字 %02X 不该报应用错误", h.Status)
	}

	want := []struct {
		q     string
		unit  string
		value float64
	}{
		{"energy", "Wh", 1234567},
		{"volume", "m^3", 800},
		{"flow-temperature", "°C", 90.5},
		{"return-temperature", "°C", 56.1},
		{"volume-flow", "m^3/h", 0.25},
		{"power", "W", 24484},
	}
	if len(resp.Records) != len(want)+1 {
		t.Fatalf("记录数 = %d，应为 %d（%s）", len(resp.Records), len(want)+1, RecordNames(resp.Records))
	}
	for i, w := range want {
		r := resp.Records[i]
		if r.Quantity != w.q {
			t.Errorf("记录 %d 量名 = %q，应为 %q", i, r.Quantity, w.q)
			continue
		}
		if r.Unit != w.unit {
			t.Errorf("%s 单位 = %q，应为 %q", w.q, r.Unit, w.unit)
		}
		if math.Abs(r.Num-w.value) > 1e-6 {
			t.Errorf("%s 值 = %g，应为 %g", w.q, r.Num, w.value)
		}
		if r.Kind != KindNumeric {
			t.Errorf("%s kind = %v，应为 number", w.q, r.Kind)
		}
	}
	// 日期记录：Type G，两字节。
	if d := resp.Records[6]; d.Quantity != "date" || d.Text != "2024-03-15" {
		t.Errorf("日期记录 = %+v，应为 2024-03-15", d)
	}
}

func TestParseGoldenResyncAndChecksum(t *testing.T) {
	good := goldenFrame(t)
	// 前导噪声 + 好帧：必须跳到帧起点。
	noisy := append(mustHex(t, "0016FF02"), good...)
	if _, _, err := Parse(noisy); !errors.Is(err, ErrResync) {
		t.Fatalf("首个字节 00 应报 ErrResync，得到 %v", err)
	}
	r := NewReader(&fakeStream{data: noisy})
	got, err := r.Next()
	if err != nil {
		t.Fatalf("从噪声中恢复失败：%v", err)
	}
	if got.Type != FrameLong {
		t.Fatalf("恢复出的帧类型 = %v", got.Type)
	}

	// 改坏校验和：必须报 ErrChecksum，且不能返回部分数据。
	bad := append([]byte(nil), good...)
	length := int(bad[1])
	bad[4+length] ^= 0xFF
	if _, _, err := Parse(bad); !errors.Is(err, ErrChecksum) {
		t.Fatalf("坏校验和应报 ErrChecksum，得到 %v", err)
	}

	// 两个 L 不等：这是残帧最典型的样子。
	mismatch := append([]byte(nil), good...)
	mismatch[2] = bad[1] - 1
	if _, _, err := Parse(mismatch); !errors.Is(err, ErrFrame) {
		t.Fatalf("两个 L 不等应报 ErrFrame，得到 %v", err)
	}

	// 结束符错。
	stop := append([]byte(nil), good...)
	stop[len(stop)-1] = 0x17
	if _, _, err := Parse(stop); !errors.Is(err, ErrFrame) {
		t.Fatalf("结束符错应报 ErrFrame，得到 %v", err)
	}

	// 截断：必须报 ErrIncomplete（让 Reader 继续等，而不是丢掉）。
	if _, _, err := Parse(good[:len(good)-3]); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("截断帧应报 ErrIncomplete，得到 %v", err)
	}
}

func TestParseSecondaryAddressRejected(t *testing.T) {
	// 把地址换成 0xFD（次级地址标记）：本包必须明确拒绝，而不是把 8 字节
	// 次级地址当成数据再报"校验和错"。
	fr := BuildLong(CtrlRSPUD, AddrSecondary, CIVariableData, []byte{1, 2, 3})
	if _, _, err := Parse(fr); !errors.Is(err, ErrFrame) {
		t.Fatalf("次级地址应报 ErrFrame，得到 %v", err)
	}
}

func TestShortFrameParse(t *testing.T) {
	fr, n, err := Parse(mustHex(t, "1040014116"))
	if err != nil || n != 5 {
		t.Fatalf("短帧解析失败：%v n=%d", err, n)
	}
	if fr.Type != FrameShort || !fr.Master() || fr.Function() != FuncSNDNKE {
		t.Fatalf("短帧语义错：%+v master=%v func=%X", fr, fr.Master(), fr.Function())
	}
	if fr.A != 1 {
		t.Fatalf("地址 = %d", fr.A)
	}
	// ACK 只有 1 字节，且没有 C 字段。
	ack, n, err := Parse([]byte{Ack})
	if err != nil || n != 1 || ack.Type != FrameAck {
		t.Fatalf("ACK 解析失败：%+v n=%d err=%v", ack, n, err)
	}
}

func TestParseFixedData(t *testing.T) {
	// 16 字节固定结构：表号 12345678、访问号 5、状态 0（BCD、当前值）
	// 计数器 1 = 1234 l（type 0x2B → 体积 100 l 单位）
	// 计数器 2 = 5678 l
	data := mustHex(t, "78563412"+"05"+"00"+"2B"+"2B"+"34120000"+"78560000")
	fd, err := ParseFixedData(data)
	if err != nil {
		t.Fatalf("固定结构解析失败：%v", err)
	}
	if !fd.BCD || fd.Stored {
		t.Fatalf("BCD/Stored = %v/%v，应为 true/false", fd.BCD, fd.Stored)
	}
	if fd.Header.IDText != "12345678" {
		t.Errorf("表号 = %q", fd.Header.IDText)
	}
	// type 2Bh 是 100 l 单位，1234 个 100 l = 123.4 m^3。
	if got := fd.Counters[0].Num; math.Abs(got-123.4) > 1e-9 {
		t.Errorf("计数器 1 = %g，应为 123.4", got)
	}
	if got := fd.Counters[1].Num; math.Abs(got-567.8) > 1e-9 {
		t.Errorf("计数器 2 = %g，应为 567.8", got)
	}
	if fd.Counters[0].Quantity != "volume" || fd.Counters[1].Quantity != "volume-2" {
		t.Errorf("量名 = %q/%q，应为 volume/volume-2",
			fd.Counters[0].Quantity, fd.Counters[1].Quantity)
	}
	// 长度不是 16 必须报错：把固定结构当可变结构解是最危险的一类静默错误。
	if _, err := ParseFixedData(data[:15]); !errors.Is(err, ErrFrame) {
		t.Fatalf("15 字节应报 ErrFrame，得到 %v", err)
	}
}

func TestParseGeneralError(t *testing.T) {
	// CI=70h，错误码 02h（缓冲区溢出）。
	data := mustHex(t, "02")
	fr := BuildLong(CtrlRSPUD, 3, CIGeneralError, data)
	f, _, err := Parse(fr)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	resp, err := ParseResponse(f)
	if err != nil {
		t.Fatalf("解析应答失败：%v", err)
	}
	if resp.ErrorCode != 0x02 || resp.ErrorText == "" {
		t.Fatalf("错误码/说明 = %02X/%q", resp.ErrorCode, resp.ErrorText)
	}
	if resp.Kind != DataNone {
		t.Fatalf("kind = %v，应为 none", resp.Kind)
	}
}

func TestParseResponseRejectsMasterFrame(t *testing.T) {
	// 把主站请求当应答解析：必须报错，否则驱动会把回环的请求当成数据。
	req := BuildShort(CtrlREQUD2NoFCB, 1)
	fr, _, err := Parse(req)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if err := CheckResponse(fr); err == nil {
		t.Fatal("主站短帧不该被当成应答")
	}
}

func TestBCDRoundTrip(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"3412", 1234, true}, // 低位在前：字节 34 12 是 1234
		{"78563412", 12345678, true},
		{"12F0", -12, true}, // 最高半字节 Fh 是负号
		{"0000000000", 0, true},
		{"1A", 0, false}, // A 不是 BCD 数字
		{"FF", 0, false}, // 全 F 也不该被当成 -15
	}
	for _, c := range cases {
		got, ok := decodeBCD(mustHex(t, c.raw))
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("decodeBCD(%s) = %d,%v，应为 %d,%v", c.raw, got, ok, c.want, c.ok)
		}
	}
	// 编码是解码的逆运算（负数带符号半字节）。
	for _, v := range []int64{0, 7, 1234, 12345678, -12, -123456} {
		raw := encodeBCD(v, 4)
		got, ok := decodeBCD(raw)
		if !ok || got != v {
			t.Errorf("encodeBCD(%d) → %x → %d,%v", v, raw, got, ok)
		}
	}
}

func TestIntAndRealDecode(t *testing.T) {
	// 3 字节与 6 字节整数的符号扩展：只用 1/2/4/8 字节实现的解析器
	// 在这两个宽度上会算错，而 M-Bus 恰好用到了它们。
	if v, _ := decodeIntLE(mustHex(t, "FFFFFF")); v != -1 {
		t.Errorf("3 字节 -1 = %d", v)
	}
	if v, _ := decodeIntLE(mustHex(t, "000080")); v != -8388608 {
		t.Errorf("3 字节最小负数 = %d", v)
	}
	if v, _ := decodeIntLE(mustHex(t, "FFFFFFFFFFFF")); v != -1 {
		t.Errorf("6 字节 -1 = %d", v)
	}
	// REAL：小端 float32。
	if v, _ := decodeReal(mustHex(t, "0000C03F")); math.Abs(v-1.5) > 1e-6 {
		t.Errorf("REAL 1.5 = %g", v)
	}
	if v, _ := decodeReal(mustHex(t, "0000C07F")); !math.IsNaN(v) {
		t.Errorf("REAL NaN = %g", v)
	}
	// 宽度不对必须报不合法，而不是读出一半。
	if _, ok := decodeIntLE(make([]byte, 5)); ok {
		t.Error("5 字节整数不该被接受")
	}
}

func TestDateDecode(t *testing.T) {
	// Type G：2024-03-15。
	got, ok := decodeDate(mustHex(t, "0F33"))
	if !ok || got.Format("2006-01-02") != "2024-03-15" {
		t.Fatalf("日期 = %v,%v", got, ok)
	}
	// 日为 0 说明时钟未设置：必须判为不可用，而不是解出"上月最后一天"。
	if _, ok := decodeDate(mustHex(t, "0033")); ok {
		t.Error("日为 0 的日期应判为不可用")
	}
}

func TestDateTimeDecode(t *testing.T) {
	// Type I（6 字节）：秒 30、分 20、时 10、日 15、月 3、年 24。
	// b[0]=30=0x1E；b[1]=20=0x14；b[2]=10=0x0A；b[3]=15|(24&7)<<5=0x0F；
	// b[4]=3|(24>>3)<<4=0x33。
	got, ok := decodeDateTime(mustHex(t, "1E140A0F3300"))
	if !ok {
		t.Fatal("6 字节日期时间解析失败")
	}
	if s := got.Format("2006-01-02 15:04:05"); s != "2024-03-15 10:20:30" {
		t.Fatalf("日期时间 = %s", s)
	}
	// 时间无效位（b[1] bit7）置位：判为不可用。
	if _, ok := decodeDateTime(mustHex(t, "1E940A0F3300")); ok {
		t.Error("时间无效位应判为不可用")
	}
}

func TestManufacturerCode(t *testing.T) {
	code, err := DecodeManufacturerCode("XYZ")
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if got := ManufacturerCode(encodeU16(code)); got != "XYZ" {
		t.Fatalf("往返 = %q", got)
	}
	if _, err := DecodeManufacturerCode("XY"); err == nil {
		t.Error("两位厂商码应报错")
	}
	if _, err := DecodeManufacturerCode("X1Z"); err == nil {
		t.Error("含数字的厂商码应报错")
	}
}

func TestVIFTable(t *testing.T) {
	cases := []struct {
		vif   byte
		vifes []byte
		q     string
		unit  string
		scale float64
	}{
		{0x00, nil, "energy", "Wh", 1e-3},
		{0x03, nil, "energy", "Wh", 1},
		{0x07, nil, "energy", "Wh", 1e4},
		{0x08, nil, "energy", "J", 1},
		{0x10, nil, "volume", "m^3", 1e-6},
		{0x13, nil, "volume", "m^3", 1e-3},
		{0x16, nil, "volume", "m^3", 1},
		{0x1B, nil, "mass", "kg", 1},
		{0x20, nil, "on-time", "s", 1},
		{0x21, nil, "on-time", "s", 60},
		{0x23, nil, "on-time", "s", 86400},
		{0x2B, nil, "power", "W", 1},
		{0x3B, nil, "volume-flow", "m^3/h", 1e-3},
		{0x5A, nil, "flow-temperature", "°C", 0.1},
		{0x5E, nil, "return-temperature", "°C", 0.1},
		{0x62, nil, "temperature-difference", "K", 0.1},
		{0x6C, nil, "date", "", 1},
		{0x6D, nil, "date-time", "", 1},
		// VIF=FDh 时物理量来自 VIFE 主表。
		{0xFD, []byte{0x08}, "access-number", "", 1},
		{0xFD, []byte{0x1B}, "digital-input", "", 1},
		{0xFD, []byte{0x17}, "error-flags", "", 1},
	}
	for _, c := range cases {
		info := LookupVIF(c.vif, c.vifes)
		if info.Quantity != c.q || info.Unit != c.unit || math.Abs(info.Scale-c.scale) > 1e-15 {
			t.Errorf("VIF %02X %x → %q/%q/%g，应为 %q/%q/%g",
				c.vif, c.vifes, info.Quantity, info.Unit, info.Scale, c.q, c.unit, c.scale)
		}
	}
	// 未知 VIF 不该 panic，也不该静默变成某个已知量。
	if info := LookupVIF(0x7B, nil); info.Quantity == "" {
		t.Error("未收录的 VIF 仍应给出编码名")
	}
}

func TestDIFEStorageAndTariff(t *testing.T) {
	// 历史记录：storage=1、tariff=0。DIF 的 bit6 是存储号最低位。
	spec := RecordSpec{VIF: 0x13, Width: 0x04, Value: 12345, Storage: 1}
	raw, err := EncodeRecord(spec)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if raw[0]&0x40 == 0 {
		t.Fatalf("DIF=%02X 未置存储位", raw[0])
	}
	// 解除 DIF 后按记录解析，storage 必须回到 1。
	rec, _, err := parseRecord(raw, 0)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if rec.Storage != 1 {
		t.Errorf("storage = %d，应为 1", rec.Storage)
	}
	if math.Abs(rec.Num-12.345) > 1e-9 {
		t.Errorf("值 = %g，应为 12.345", rec.Num)
	}

	// 大存储号必须走 DIFE 链：storage=0x21（33）。
	spec = RecordSpec{VIF: 0x13, Width: 0x04, Value: 1, Storage: 33, Tariff: 2}
	raw, err = EncodeRecord(spec)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	rec, _, err = parseRecord(raw, 0)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if rec.Storage != 33 || rec.Tariff != 2 {
		t.Errorf("storage/tariff = %d/%d，应为 33/2", rec.Storage, rec.Tariff)
	}
}

func TestRecordSelection(t *testing.T) {
	recs := []Record{
		{Index: 0, Quantity: "energy", Unit: "Wh", Storage: 0, Kind: KindNumeric},
		{Index: 1, Quantity: "volume", Unit: "m^3", Storage: 0, Kind: KindNumeric},
		{Index: 2, Quantity: "energy", Unit: "Wh", Storage: 1, Kind: KindNumeric},
		{Index: 3, Quantity: "power-max", Unit: "W", Storage: 0, Kind: KindNumeric},
	}
	cases := []struct {
		expr string
		idx  int
		ok   bool
	}{
		{"energy", 0, true},
		{"q:energy", 0, false}, // `q:` 前缀由驱动剥掉；codec 只认量名
		{"volume", 1, true},
		{"energy@1", 2, true},
		{"energy/1", 2, true}, // 第 2 条 energy（不区分存储号时）
		{"power-max", 3, true},
		{"r:1", 1, true},
		{"r:99", 0, false},
		{"energy@9", 0, false},
		{"nope", 0, false},
	}
	for _, c := range cases {
		got, ok, err := FindRecord(recs, c.expr)
		if ok != c.ok {
			t.Errorf("FindRecord(%q) ok=%v err=%v，应为 ok=%v", c.expr, ok, err, c.ok)
			continue
		}
		if ok && got.Index != c.idx {
			t.Errorf("FindRecord(%q) → %d，应为 %d", c.expr, got.Index, c.idx)
		}
	}
	// 找不到时的报错必须列出候选，否则现场只能靠猜。
	_, _, err := FindRecord(recs, "pressure")
	if err == nil {
		t.Fatal("找不到应报错")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("energy")) {
		t.Errorf("报错未列出候选：%v", err)
	}
}

func TestRecordNamesAreAddressable(t *testing.T) {
	// RecordNames 的输出必须能被 FindRecord 直接吃回去——自检命令会把它
	// 打印给用户抄成点表地址，抄不回去就是设计缺陷。
	hdrs := Header{IDText: "12345678", ManufacturerID: "XYZ", Medium: MediumHeat}
	data, err := BuildVariableData(hdrs, []RecordSpec{
		{VIF: 0x06, Width: 0x04, Value: 1234567},
		{VIF: 0x13, Width: 0x04, Value: 800000},
		{VIF: 0x13, Width: 0x04, Value: 700000, Storage: 1},
	})
	if err != nil {
		t.Fatalf("组帧失败：%v", err)
	}
	vd, err := ParseVariableData(data)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	names := RecordNames(vd.Records)
	if names == "" {
		t.Fatal("RecordNames 为空")
	}
	for _, r := range vd.Records {
		// 逐个用 `q:` 形式反查，必须命中同一条记录。
		ref := "q:" + NormalizeQuantity(r.Quantity)
		if r.Storage > 0 {
			ref = "q:" + NormalizeQuantity(r.Quantity) + "@" + itoa(int(r.Storage))
		}
		name := ref[2:]
		got, ok, err := FindRecord(vd.Records, name)
		if !ok {
			t.Errorf("反查 %q 失败：%v（候选：%s）", name, err, names)
			continue
		}
		if got.Index != r.Index {
			t.Errorf("反查 %q 命中 %d，应为 %d", name, got.Index, r.Index)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// 编码 → 解码必须回到原值。这一条守着"主站与从站共用同一份规则"。
	cases := []RecordSpec{
		{VIF: 0x13, Width: 0x01, Value: 7},
		{VIF: 0x13, Width: 0x02, Value: -300},
		{VIF: 0x13, Width: 0x03, Value: 123456},
		{VIF: 0x13, Width: 0x04, Value: 12345678},
		{VIF: 0x13, Width: 0x06, Value: -1234567890},
		{VIF: 0x13, Width: 0x07, Value: 1 << 40},
		{VIF: 0x13, Width: 0x0C, Value: 123456},       // 8 位 BCD
		{VIF: 0x13, Width: 0x09, Value: -5},           // 2 位 BCD
		{VIF: 0x13, Width: 0x0E, Value: 999999999999}, // 12 位 BCD
		{VIF: 0x5B, Width: 0x02, Value: 905},          // 90.5 °C
		{VIF: 0x2B, Width: 0x04, Value: 24500},        // 24.5 kW
	}
	for _, c := range cases {
		raw, err := EncodeRecord(c)
		if err != nil {
			t.Fatalf("编码 %+v 失败：%v", c, err)
		}
		rec, n, err := parseRecord(raw, 0)
		if err != nil {
			t.Fatalf("解码 %x 失败：%v", raw, err)
		}
		if n != len(raw) {
			t.Errorf("消耗字节 %d，应为 %d（%x）", n, len(raw), raw)
		}
		if rec.RawInt != c.Value {
			t.Errorf("DIF 宽度 %02X：RawInt = %d，应为 %d（%x）",
				c.Width, rec.RawInt, c.Value, raw)
		}
	}
}

func TestEncodeTextAndReal(t *testing.T) {
	// 文本记录（DIF=0Dh）：LVAR = 长度，其后 ASCII。
	spec := RecordSpec{VIF: 0x7D, VIFEs: []byte{0x11}, Text: "ACME-01"}
	raw, err := EncodeRecord(spec)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	rec, _, err := parseRecord(raw, 0)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if rec.Kind != KindText || rec.Text != "ACME-01" {
		t.Fatalf("文本 = %q kind=%v", rec.Text, rec.Kind)
	}
	if rec.Quantity != "customer" {
		t.Errorf("量名 = %q，应为 customer", rec.Quantity)
	}

	// REAL（DIF 低 4 位 = 05h）：值是 IEEE754，不再乘 VIF 的 10 的幂。
	spec = RecordSpec{VIF: 0x13, Width: 0x05, Real: 1234.5}
	raw, err = EncodeRecord(spec)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	rec, _, err = parseRecord(raw, 0)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if math.Abs(rec.Num-1234.5) > 1e-3 {
		t.Fatalf("REAL = %g，应为 1234.5", rec.Num)
	}
}

func TestCustomVIFRecord(t *testing.T) {
	spec := RecordSpec{VIF: 0x13, CustomVIF: "Pressure_psi", Width: 0x02, Value: 123}
	raw, err := EncodeRecord(spec)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if raw[0] != 0x02 || raw[1] != 0x7C {
		t.Fatalf("DIF/VIF = %02X/%02X，应为 02/7C", raw[0], raw[1])
	}
	rec, _, err := parseRecord(raw, 0)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if rec.Quantity != "pressure-psi" {
		t.Errorf("量名 = %q，应为 pressure-psi", rec.Quantity)
	}
	if rec.Num != 123 {
		t.Errorf("值 = %g", rec.Num)
	}
}

func TestMoreRecordsFollow(t *testing.T) {
	// DIF=1Fh 表示还有后续报文。驱动必须能看到这个标志，否则会静默丢半张表。
	hdrs := Header{IDText: "12345678", ManufacturerID: "XYZ", Medium: MediumHeat}
	rec, err := EncodeRecord(RecordSpec{VIF: 0x13, Width: 0x04, Value: 1})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	data, err := BuildVariableData(hdrs, nil)
	if err != nil {
		t.Fatalf("组帧失败：%v", err)
	}
	data = append(data, rec...)
	data = append(data, 0x1F, 0xAA, 0xBB)
	vd, err := ParseVariableData(data)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !vd.MoreRecordsFollow {
		t.Error("未识别 DIF=1F 的后续报文标志")
	}
}

func TestIdleFillerSkipped(t *testing.T) {
	// 2Fh 是按 16 字节对齐时的填充，不是记录；把它当记录会读出长度 0
	// 且量名乱码的一条，后面整段记录随之错位。
	hdrs := Header{IDText: "00000001", ManufacturerID: "XYZ", Medium: MediumHeat}
	data, err := BuildVariableData(hdrs, []RecordSpec{{VIF: 0x13, Width: 0x04, Value: 42}})
	if err != nil {
		t.Fatalf("组帧失败：%v", err)
	}
	data = append(data, 0x2F, 0x2F)
	vd, err := ParseVariableData(data)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(vd.Records) != 1 || vd.Records[0].RawInt != 42 {
		t.Fatalf("记录 = %+v", vd.Records)
	}
}

func TestTruncatedRecord(t *testing.T) {
	// 记录声明 4 字节但只剩 2 字节：必须报错，而不是补零凑一个值。
	hdrs := Header{IDText: "00000001", ManufacturerID: "XYZ", Medium: MediumHeat}
	data, err := BuildVariableData(hdrs, []RecordSpec{{VIF: 0x13, Width: 0x04, Value: 42}})
	if err != nil {
		t.Fatalf("组帧失败：%v", err)
	}
	if _, err := ParseVariableData(data[:len(data)-2]); !errors.Is(err, ErrFrame) {
		t.Fatalf("截断记录应报 ErrFrame，得到 %v", err)
	}
}

func TestFrameLenAgreesWithParse(t *testing.T) {
	frames := [][]byte{
		BuildAck(),
		BuildShort(CtrlREQUD2NoFCB, 9),
		BuildLong(CtrlRSPUD, 2, CIVariableData, bytes.Repeat([]byte{0xAA}, 40)),
		goldenFrame(t),
	}
	for _, fr := range frames {
		n, ok := FrameLen(fr)
		if !ok || n != len(fr) {
			t.Errorf("FrameLen(%x) = %d,%v，应为 %d", fr, n, ok, len(fr))
		}
	}
}

// fakeStream 是只用于 Reader 测试的内存流。
type fakeStream struct {
	data []byte
	pos  int
}

func (s *fakeStream) Write(p []byte) error { return nil }

func (s *fakeStream) ReadFull(n int) ([]byte, error) {
	if s.pos >= len(s.data) {
		return nil, errEOF
	}
	end := s.pos + n
	if end > len(s.data) {
		end = len(s.data)
	}
	out := s.data[s.pos:end]
	s.pos = end
	return out, nil
}

func (s *fakeStream) Close() error { return nil }

var errEOF = errors.New("eof")

func encodeU16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
