package mbus

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件补的是编码侧与「应答分派」这两块：
//
//   - 编码侧只服务内置从站与测试，但它是金帧可复现的前提。它一旦与解码
//     侧的长度/量纲规则不一致，测试就只能证明"仿真器与它自己一致"。
//   - 应答分派把"这帧是数据还是错误"这一判断收在一处。驱动不该自己判断
//     CI 是 72h 还是 73h、固定结构要不要再包一层；分派写错的表现是"某类
//     表永远读不出来"，而报文本身完全正常。

func TestBuildFixedDataRoundTrip(t *testing.T) {
	// CI=73h 固定结构：BCD 当前值、两个体积计数器。
	h := Header{IDText: "12345678", AccessNo: 5, Status: 0, Medium: 0x07}
	data, err := BuildFixedData(h, true, false, [2]byte{0x2B, 0x2B}, [2]int64{1234, 5678})
	if err != nil {
		t.Fatalf("组装固定结构: %v", err)
	}
	if len(data) != 16 {
		t.Fatalf("长度 %d，应为 16", len(data))
	}
	fd, err := ParseFixedData(data)
	if err != nil {
		t.Fatalf("解析固定结构: %v", err)
	}
	if !fd.BCD || fd.Stored {
		t.Fatalf("BCD/Stored = %v/%v，应为 true/false", fd.BCD, fd.Stored)
	}
	if fd.Header.AccessNo != 5 {
		t.Errorf("访问号 = %d，应为 5", fd.Header.AccessNo)
	}
	if fd.Header.IDText != "12345678" {
		t.Errorf("表号 = %q", fd.Header.IDText)
	}
	if fd.Counters[0].RawInt != 1234 || fd.Counters[1].RawInt != 5678 {
		t.Errorf("计数器 = %d/%d，应为 1234/5678",
			fd.Counters[0].RawInt, fd.Counters[1].RawInt)
	}
	// 0x2B 是 100 l 单位，工程值是 m³：1234 × 0.1 = 123.4。
	if got := fd.Counters[0].Num; math.Abs(got-123.4) > 1e-9 {
		t.Errorf("计数器 1 = %g，应为 123.4", got)
	}

	// 二进制（非 BCD）+ 历史值：状态字节的 bit7/bit6 都要能原样回来。
	data, err = BuildFixedData(h, false, true, [2]byte{0x14, 0x2B}, [2]int64{-7, 3})
	if err != nil {
		t.Fatalf("组装固定结构: %v", err)
	}
	fd, err = ParseFixedData(data)
	if err != nil {
		t.Fatalf("解析固定结构: %v", err)
	}
	if fd.BCD {
		t.Error("bit7 置位表示整数编码，BCD 应为假")
	}
	if !fd.Stored {
		t.Error("bit6 置位表示历史值，Stored 应为真")
	}
	// 二进制带符号：负数必须原样回来（这就是 bit7 存在的意义）。
	if fd.Counters[0].RawInt != -7 {
		t.Errorf("计数器 1 = %d，应为 -7", fd.Counters[0].RawInt)
	}
}

func TestBuildFixedDataRejectsBadHeader(t *testing.T) {
	// 表号含非十进制字符：宁可组帧失败，也不能拼出一条表号乱码的报文。
	if _, err := BuildFixedData(Header{IDText: "12AB5678"}, true, false, [2]byte{0x2B, 0x2B}, [2]int64{1, 2}); err == nil {
		t.Fatal("非法表号应报错")
	}
	if _, err := BuildFixedData(Header{ManufacturerID: "X1Z"}, true, false, [2]byte{0x2B, 0x2B}, [2]int64{1, 2}); err == nil {
		t.Fatal("非法厂商码应报错")
	}
}

func TestEncodeRecordRejectsBadWidth(t *testing.T) {
	// 文本只能走 DIF=0Dh（变长）。显式给了别的宽度必须报错：按定宽编码
	// 会把字符串截断成一个"看起来正常"的数字。
	if _, err := EncodeRecord(RecordSpec{VIF: 0x13, Width: 0x04, Text: "abc"}); err == nil {
		t.Fatal("文本配定宽应报错")
	}
	// 变长记录却给了非变长宽度。
	if _, err := EncodeRecord(RecordSpec{VIF: 0x13, Width: 0x04, Raw: []byte{0x02, 0xAA, 0xBB}}); err != nil {
		t.Fatalf("定宽 Raw 应被接受: %v", err)
	}
	// LVAR 与实际数据长度不一致：内置从站不能上送自相矛盾的报文。
	if _, err := EncodeRecord(RecordSpec{VIF: 0x13, Width: 0x0D, Raw: []byte{0x05, 0xAA}}); err == nil {
		t.Fatal("LVAR 与数据长度不符应报错")
	}
	// 文本超过 191 字节：LVAR 的单字节上限。
	if _, err := EncodeRecord(RecordSpec{VIF: 0x7D, Text: strings.Repeat("x", 200)}); err == nil {
		t.Fatal("超长文本应报错")
	}
	// 自定义 VIF 名过长。
	if _, err := EncodeRecord(RecordSpec{CustomVIF: strings.Repeat("v", 200), Width: 0x01, Value: 1}); err == nil {
		t.Fatal("超长自定义 VIF 应报错")
	}
}

func TestAutoIntWidth(t *testing.T) {
	cases := []struct {
		v    int64
		want byte
	}{
		{0, 0x01}, {127, 0x01}, {-128, 0x01},
		{128, 0x02}, {-129, 0x02}, {32767, 0x02},
		{32768, 0x03}, {-(1 << 23), 0x03}, {(1 << 23) - 1, 0x03},
		{1 << 23, 0x04}, {math.MinInt32, 0x04}, {math.MaxInt32, 0x04},
		{math.MaxInt32 + 1, 0x07},
	}
	for _, c := range cases {
		if got := autoIntWidth(c.v); got != c.want {
			t.Errorf("autoIntWidth(%d) = %02X，应为 %02X", c.v, got, c.want)
		}
	}
}

func TestBCDWidthTable(t *testing.T) {
	// BCD 的宽度编码不是"字节数"：09h 是 2 位、0Eh 是 12 位。
	cases := map[byte]int{0x09: 1, 0x0A: 2, 0x0B: 3, 0x0C: 4, 0x0E: 6}
	for w, want := range cases {
		if !isBCDWidth(w) {
			t.Errorf("%02X 应被判为 BCD 宽度", w)
		}
		if got := bcdWidth(w); got != want {
			t.Errorf("bcdWidth(%02X) = %d，应为 %d", w, got, want)
		}
	}
	// 01h..08h 与 0Dh 是整数/变长，不是 BCD。
	for _, w := range []byte{0x00, 0x01, 0x04, 0x05, 0x08, 0x0D, 0x0F} {
		if isBCDWidth(w) {
			t.Errorf("%02X 不是 BCD 宽度", w)
		}
		if got := bcdWidth(w); got != 0 {
			t.Errorf("bcdWidth(%02X) = %d，应为 0", w, got)
		}
	}
}

func TestApplicationErrorFullTable(t *testing.T) {
	// 全部标准码都要有中文说明：现场靠这句定位问题，返回空串会让表格
	// 里那一格看起来"什么都没有"。
	for code := 0x00; code <= 0x11; code++ {
		got := ApplicationError(byte(code))
		if got == "" || strings.HasPrefix(got, "未知错误码") {
			t.Errorf("标准错误码 %02X 没有说明：%q", code, got)
		}
	}
	if got := ApplicationError(0x20); !strings.Contains(got, "20") {
		t.Errorf("未知码应带上码值：%q", got)
	}
}

func TestParseResponseDispatch(t *testing.T) {
	hdrs := Header{IDText: "00000001", ManufacturerID: "XYZ", Medium: MediumHeat}

	// 可变结构（72h）。
	data, err := BuildVariableData(hdrs, []RecordSpec{{VIF: 0x13, Width: 0x04, Value: 4200}})
	if err != nil {
		t.Fatalf("组帧: %v", err)
	}
	fr := frameLong(CtrlRSPUD, 1, CIVariableData, data)
	resp, err := ParseResponse(fr)
	if err != nil {
		t.Fatalf("解析可变结构: %v", err)
	}
	if resp.Kind != DataVariable || len(resp.Records) != 1 {
		t.Fatalf("Kind=%v Records=%d", resp.Kind, len(resp.Records))
	}
	// VIF 13h 是可变量纲里的「体积，1e-3 m³」——不是能量。
	if resp.Records[0].Quantity != "volume" {
		t.Errorf("量名 = %q，应为 volume", resp.Records[0].Quantity)
	}

	// 固定结构（73h）。
	fixed, err := BuildFixedData(hdrs, true, false, [2]byte{0x2B, 0x2B}, [2]int64{1, 2})
	if err != nil {
		t.Fatalf("组固定结构: %v", err)
	}
	resp, err = ParseResponse(frameLong(CtrlRSPUD, 1, CIFixedData, fixed))
	if err != nil {
		t.Fatalf("解析固定结构: %v", err)
	}
	if resp.Kind != DataFixed || len(resp.Records) != 2 {
		t.Fatalf("Kind=%v Records=%d", resp.Kind, len(resp.Records))
	}

	// 通用错误（70h）。
	resp, err = ParseResponse(frameLong(CtrlRSPUD, 1, CIGeneralError, []byte{0x02}))
	if err != nil {
		t.Fatalf("解析通用错误: %v", err)
	}
	if resp.Kind != DataNone || resp.ErrorCode != 0x02 {
		t.Fatalf("Kind=%v Code=%02X", resp.Kind, resp.ErrorCode)
	}
	if !strings.Contains(resp.ErrorText, "溢出") {
		t.Errorf("错误说明 = %q", resp.ErrorText)
	}
	// 没有错误码的通用错误：也要给出可读说明，而不是空串。
	resp, err = ParseResponse(frameLong(CtrlRSPUD, 1, CIGeneralError, nil))
	if err != nil {
		t.Fatalf("解析空通用错误: %v", err)
	}
	if resp.ErrorText == "" {
		t.Error("无码通用错误也应有说明")
	}

	// 告警状态（71h）：本实现不解析内容，但必须明确说"没解析"，
	// 而不是当成空数据让上层以为表里没值。
	resp, err = ParseResponse(frameLong(CtrlRSPUD, 1, CIAlarmStatus, []byte{0x01}))
	if err != nil {
		t.Fatalf("解析告警状态: %v", err)
	}
	if resp.Kind != DataNone || !strings.Contains(resp.ErrorText, "告警") {
		t.Errorf("Kind=%v Text=%q", resp.Kind, resp.ErrorText)
	}

	// 不支持的 CI：报错要列出支持范围。
	_, err = ParseResponse(frameLong(CtrlRSPUD, 1, 0x99, []byte{0x01}))
	if !errors.Is(err, ErrFrame) {
		t.Fatalf("未知 CI 应报 ErrFrame，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("报错应带上 CI 值：%v", err)
	}
}

func TestParseResponseMode2CIs(t *testing.T) {
	// Mode 2（老式、MSB 置位）用 76h/77h。漏掉它们会让一整批老表被报成
	// "不支持的 CI"，而报文内容其实是好的。
	hdrs := Header{IDText: "00000001", ManufacturerID: "XYZ"}
	data, err := BuildVariableData(hdrs, []RecordSpec{{VIF: 0x13, Width: 0x02, Value: 99}})
	if err != nil {
		t.Fatalf("组帧: %v", err)
	}
	resp, err := ParseResponse(frameLong(CtrlRSPUD, 1, 0x76, data))
	if err != nil {
		t.Fatalf("Mode 2 可变结构: %v", err)
	}
	if resp.Kind != DataVariable || len(resp.Records) != 1 || resp.Records[0].RawInt != 99 {
		t.Fatalf("解出的记录 = %+v", resp.Records)
	}

	fixed, err := BuildFixedData(hdrs, true, false, [2]byte{0x2B, 0x2B}, [2]int64{1, 2})
	if err != nil {
		t.Fatalf("组固定结构: %v", err)
	}
	resp, err = ParseResponse(frameLong(CtrlRSPUD, 1, 0x77, fixed))
	if err != nil {
		t.Fatalf("Mode 2 固定结构: %v", err)
	}
	if resp.Kind != DataFixed {
		t.Fatalf("Kind = %v，应为 fixed", resp.Kind)
	}
}

func TestParseResponseStatusError(t *testing.T) {
	// 状态字 bit0-1 非零：数据仍然可解析，但必须把"表自己报了应用错误"
	// 这件事带出去，让上层降质量。
	hdrs := Header{IDText: "00000001", ManufacturerID: "XYZ", Status: 0x01}
	data, err := BuildVariableData(hdrs, []RecordSpec{{VIF: 0x13, Width: 0x02, Value: 5}})
	if err != nil {
		t.Fatalf("组帧: %v", err)
	}
	resp, err := ParseResponse(frameLong(CtrlRSPUD, 1, CIVariableData, data))
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if resp.ErrorText == "" {
		t.Error("应用错误应体现在 ErrorText 里")
	}
	if len(resp.Records) != 1 {
		t.Errorf("出错状态不该丢掉记录，得到 %d 条", len(resp.Records))
	}

	// 只有电源低（bit2）：读数通常仍然有效，不应该被当作应用错误。
	hdrs.Status = 0x04
	data, err = BuildVariableData(hdrs, []RecordSpec{{VIF: 0x13, Width: 0x02, Value: 5}})
	if err != nil {
		t.Fatalf("组帧: %v", err)
	}
	resp, err = ParseResponse(frameLong(CtrlRSPUD, 1, CIVariableData, data))
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if resp.ErrorText != "" {
		t.Errorf("只有电源低不该算应用错误，得到 %q", resp.ErrorText)
	}
}

func TestCheckResponseRejectsMasterAndShort(t *testing.T) {
	// 主站帧不是应答：这类错误在现场的典型成因是把 A 线 B 线接反后
	// 收到自己的回波。
	if err := CheckResponse(frameShort(CtrlREQUD2NoFCB, 1)); err == nil {
		t.Fatal("主站帧应被拒绝")
	}
	// 短帧不含数据域，不能当作读表应答。
	if err := CheckResponse(frameShort(CtrlRSPUD, 1)); err == nil {
		t.Fatal("短帧应答应被拒绝")
	}
	// ACK 单字符帧同样不是数据应答。
	if err := CheckResponse(Frame{Type: FrameAck}); err == nil {
		t.Fatal("ACK 帧应被拒绝")
	}
	// 功能码不是 RSP_UD。
	bad := frameLong(CtrlRSPUD, 1, CIVariableData, nil)
	bad.C = 0x0B
	if err := CheckResponse(bad); err == nil {
		t.Fatal("非 RSP_UD 功能码应被拒绝")
	}
	if err := CheckResponse(frameLong(CtrlRSPUD, 1, CIVariableData, nil)); err != nil {
		t.Fatalf("正常应答不该报错: %v", err)
	}
}

func TestLookupVIFFallbacks(t *testing.T) {
	// 不认识的 VIF 不能返回空量名：空名会让 `q:` 寻址无法表达，
	// 也会让 -records 输出出现一整列空白。
	for _, vif := range []byte{0x7E, 0x6F} {
		info := LookupVIF(vif, nil)
		if info.Quantity == "" {
			t.Errorf("VIF %02X 量名为空", vif)
		}
		if info.Scale != 1 {
			t.Errorf("VIF %02X 未知量纲的倍数应为 1，得到 %g", vif, info.Scale)
		}
	}
	// 厂商特定 VIF（7Fh/FFh）：语义不可知，必须打上标记而不是猜一个名字。
	for _, vif := range []byte{0x7F, 0xFF} {
		if info := LookupVIF(vif, nil); info.Special != SpecialManufacturer {
			t.Errorf("VIF %02X 应标记为厂商特定，得到 %v", vif, info.Special)
		}
	}
	// VIFE 不认识时退化成编码名，同样不能是空串。
	if info := LookupVIF(0xFD, []byte{0x7E}); info.Quantity == "" {
		t.Error("未知 VIFE 量名为空")
	}
	// 计费类 VIFE（00h..07h）按 10 的幂分 credit/debit。
	if info := LookupVIF(0xFD, []byte{0x01}); info.Quantity != "credit" {
		t.Errorf("VIFE 01 量名 = %q，应为 credit", info.Quantity)
	}
	if info := LookupVIF(0xFD, []byte{0x05}); info.Quantity != "debit" {
		t.Errorf("VIFE 05 量名 = %q，应为 debit", info.Quantity)
	}
}

func TestDecodeRealAndIntWidths(t *testing.T) {
	// REAL 是 4 字节 IEEE754；长度不对必须失败而不是补零。
	if v, ok := decodeReal([]byte{0x00, 0x00, 0x80, 0x3F}); !ok || math.Abs(v-1) > 1e-6 {
		t.Errorf("REAL 1.0 = %g,%v", v, ok)
	}
	if _, ok := decodeReal([]byte{0x00, 0x00, 0x80}); ok {
		t.Error("3 字节 REAL 应失败")
	}
	// 3 字节与 6 字节整数是符号扩展的重点：最高位为 1 时必须变成负数。
	if v, ok := decodeIntLE([]byte{0xFF, 0xFF, 0xFF}); !ok || v != -1 {
		t.Errorf("3 字节 -1 = %d,%v", v, ok)
	}
	if v, ok := decodeIntLE([]byte{0x00, 0x00, 0x80}); !ok || v != -8388608 {
		t.Errorf("3 字节最小值 = %d,%v", v, ok)
	}
	if v, ok := decodeIntLE([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}); !ok || v != -1 {
		t.Errorf("6 字节 -1 = %d,%v", v, ok)
	}
	// 不支持的宽度：返回 false，绝不能返回 0 冒充成功。
	if _, ok := decodeIntLE([]byte{0x01, 0x02, 0x03, 0x04, 0x05}); ok {
		t.Error("5 字节整数应失败")
	}
}

func TestDifDataLenRejects(t *testing.T) {
	// DIF 长度编码里有一批是"保留"值；把它们当整数解会读出鬼数。
	for _, w := range []byte{0x08, 0x0F} {
		if _, err := difDataLen(w); err != nil {
			t.Errorf("DIF %02X 不该报错: %v", w, err)
		}
	}
	// 变长（0Dh）的宽度由 LVAR 决定，函数本身返回 0。
	if n, err := difDataLen(0x0D); err != nil || n != 0 {
		t.Errorf("变长 DIF = %d,%v", n, err)
	}
	// 整数与 BCD 宽度。
	cases := map[byte]int{0x01: 1, 0x02: 2, 0x03: 3, 0x04: 4, 0x05: 4, 0x06: 6, 0x07: 8,
		0x09: 1, 0x0A: 2, 0x0B: 3, 0x0C: 4, 0x0E: 6}
	for w, want := range cases {
		if n, err := difDataLen(w); err != nil || n != want {
			t.Errorf("difDataLen(%02X) = %d,%v，应为 %d", w, n, err, want)
		}
	}
}

func TestParseNonNeg(t *testing.T) {
	if n, err := parseNonNeg("0"); err != nil || n != 0 {
		t.Errorf("parseNonNeg(0) = %d,%v", n, err)
	}
	if n, err := parseNonNeg("42"); err != nil || n != 42 {
		t.Errorf("parseNonNeg(42) = %d,%v", n, err)
	}
	// 空串、负数、非数字都必须是错误：`r:-1` 静默变成 r:0 会让
	// 点表写错的表现变成"读到了另一个点的值"。
	for _, s := range []string{"", "-1", "abc", " 1"} {
		if _, err := parseNonNeg(s); err == nil {
			t.Errorf("parseNonNeg(%q) 应报错", s)
		}
	}
}

// frameLong 组一帧长帧，仅用于构造应答。
func frameLong(c, a, ci byte, data []byte) Frame {
	return Frame{Type: FrameLong, C: c, A: a, CIField: ci, Data: data}
}

// frameShort 组一帧短帧。
func frameShort(c, a byte) Frame {
	return Frame{Type: FrameShort, C: c, A: a}
}

func TestDecodeRecordValueBranches(t *testing.T) {
	// 这个函数是「DIF 长度编码 → 值」的唯一分派点。正常路径已由金帧与
	// encode/decode 往返覆盖，这里补的是**坏数据**路径：结构对、内容坏时
	// 必须落到 KindBinary 而不是硬解出一个数——错值比没值更难发现。
	cases := []struct {
		name string
		rec  Record
		kind Kind
	}{
		// 日期/时间字段的长度与类型不符：显式标记为不可用，文本为空。
		{"日期长度错", Record{Special: SpecialDate, Raw: []byte{0x01}}, KindText},
		{"日期非法", Record{Special: SpecialDate, Raw: []byte{0x1F, 0x02}}, KindText},
		{"时间长度错", Record{Special: SpecialDateTime, Raw: []byte{0x01, 0x02}}, KindText},
		{"时间无效位", Record{Special: SpecialDateTime, Raw: []byte{0x80, 0, 1, 1}}, KindText},
		// 厂商特定 VIF：语义不可知，只能按字节暴露。
		{"厂商特定", Record{Special: SpecialManufacturer, Raw: []byte{0xAA}}, KindBinary},
		// 无数据域（选择项 / 填充）。
		{"空记录", Record{DataField: 0x00}, KindNone},
		{"填充", Record{DataField: 0x08}, KindNone},
		// 整数宽度不在 1..8：结构已经错了。
		{"整数宽度错", Record{DataField: 0x04, Raw: []byte{1, 2, 3, 4, 5}}, KindBinary},
		// REAL 必须是 4 字节。
		{"REAL 长度错", Record{DataField: 0x05, Raw: []byte{1, 2, 3}}, KindBinary},
		// BCD 里出现 A..F（符号位之外的数字位）：记录边界错位的典型症状。
		{"BCD 含非数字", Record{DataField: 0x0C, Raw: []byte{0xAB, 0x00, 0x00, 0x00}}, KindBinary},
		// 变长/保留编码：按原始字节暴露。
		{"变长", Record{DataField: 0x0F, Raw: []byte{0x01}}, KindBinary},
		{"未知编码", Record{DataField: 0x10}, KindBinary},
	}
	for _, c := range cases {
		kind, num, rawInt, text := decodeRecordValue(c.rec)
		if kind != c.kind {
			t.Errorf("%s：Kind = %v，应为 %v", c.name, kind, c.kind)
		}
		// 不是数值时不能悄悄留下一个看起来有效的 0。
		if kind != KindNumeric && (num != 0 || rawInt != 0) {
			t.Errorf("%s：非数值却带出了值 %g/%d", c.name, num, rawInt)
		}
		if kind == KindText && text != "" {
			t.Errorf("%s：坏时间不该产出文本，得到 %q", c.name, text)
		}
	}
	// 正常数值路径要乘上 Scale：这是「工程值 = 原始整数 × Scale」的落点。
	kind, num, rawInt, _ := decodeRecordValue(Record{DataField: 0x04, Raw: []byte{0x10, 0x27, 0, 0}, Scale: 1e-3})
	if kind != KindNumeric || rawInt != 10000 {
		t.Fatalf("整数记录 = %v/%d", kind, rawInt)
	}
	if math.Abs(num-10) > 1e-9 {
		t.Errorf("工程值 = %g，应为 10", num)
	}
	// REAL 不乘 Scale：标准规定它本身就是工程值。
	_, num, _, _ = decodeRecordValue(Record{DataField: 0x05, Raw: []byte{0x00, 0x00, 0x80, 0x3F}, Scale: 1e-3})
	if math.Abs(num-1) > 1e-6 {
		t.Errorf("REAL 工程值 = %g，应为 1（不该乘 Scale）", num)
	}
}

func TestLookupVIFCustomAndMaster(t *testing.T) {
	// 7Ch/FCCh：VIF 是 ASCII 字符串，物理量名由调用方从文本里取。
	for _, vif := range []byte{0x7C, 0xFC} {
		if info := LookupVIF(vif, nil); info.Special != SpecialCustomVIF {
			t.Errorf("VIF %02X 应标记为自定义，得到 %v", vif, info.Special)
		}
	}
	// 7Dh/FDh：物理量在 VIFE 主表里；没有 VIFE 时退回主表。
	if info := LookupVIF(0xFD, nil); info.Quantity == "" {
		t.Error("0xFD 无 VIFE 时也该给出量名")
	}
	if info := LookupVIF(0xFD, []byte{0x0A}); info.Quantity != "manufacturer" {
		t.Errorf("VIFE 0A 量名 = %q，应为 manufacturer", info.Quantity)
	}
	// FFh 与 7Fh 都是厂商特定。
	if info := LookupVIF(0xFF, nil); info.Special != SpecialManufacturer {
		t.Error("FFh 应标记为厂商特定")
	}
	// 量纲倍率是 10 的幂：VIF 03h（体积 1e-3 m³）与 13h 差 10 倍。
	lo := LookupVIF(0x13, nil)
	hi := LookupVIF(0x14, nil)
	if hi.Scale/lo.Scale < 9.9 || hi.Scale/lo.Scale > 10.1 {
		t.Errorf("相邻编码的倍率比 = %g，应约为 10", hi.Scale/lo.Scale)
	}
}

func TestMediaNameUnknown(t *testing.T) {
	// 未知介质要给可辨识的名字，不能返回空串——空串在表格里看起来像
	// "报文里没有这个字段"。
	if got := MediaName(0xEE); !strings.HasPrefix(got, "medium-") {
		t.Errorf("未知介质 = %q，应以 medium- 开头", got)
	}
	if got := MediaName(0x04); got != "heat" {
		t.Errorf("介质 04 = %q，应为 heat", got)
	}
}
