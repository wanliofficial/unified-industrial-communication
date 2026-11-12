package mbus

import (
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件补的是「自检命令与日志会用到、但不参与采集主路径」的那部分表面：
// 帧访问器、地址校验、量名列表、记录描述、错误码翻译、Reader 的诊断计数。
//
// 它们看起来琐碎，却直接决定现场排障的体验：`uic mbusread -raw` 打出来的
// 就是 Describe 的格式，`-records q:xxx` 写错时看到的是 QuantityKeys 的
// 候选列表，而"有字节回来但不成帧"靠的是 BadFrames。

func TestFrameAccessors(t *testing.T) {
	// REQ_UD2 带 FCB：C=7Bh，PRM=1、FCB=1、FCV=1、功能码 B。
	req := Frame{Type: FrameShort, C: CtrlREQUD2FCB, A: 1}
	if !req.Master() {
		t.Fatal("主站请求的 PRM 位应为 1")
	}
	if req.Function() != FuncREQUD2 {
		t.Fatalf("功能码 = %X，应为 %X", req.Function(), FuncREQUD2)
	}
	if !req.FCB() || !req.FCV() {
		t.Fatalf("FCB/FCV = %v/%v，都应为真", req.FCB(), req.FCV())
	}

	// RSP_UD 带 ACD：C=28h，PRM=0、ACD=1、DFC=0、功能码 8。
	rsp := Frame{Type: FrameLong, C: CtrlRSPUD, A: 1}
	if rsp.Master() {
		t.Fatal("从站应答的 PRM 位应为 0")
	}
	if rsp.FCB() {
		t.Fatal("从站方向上 bit5 是 ACD，不该被当作 FCB")
	}
	// 从站方向 bit5 是 ACD（有 2 类数据待取），bit4 是 DFC（缓冲满）。
	// 干净应答两个位都是 0。
	if rsp.ACD() || rsp.DFC() {
		t.Fatalf("ACD/DFC = %v/%v，都应为假", rsp.ACD(), rsp.DFC())
	}
	rsp.C |= 0x20
	if !rsp.ACD() {
		t.Fatal("ACD 置位后应为真")
	}
	rsp.C |= 0x10
	if !rsp.DFC() {
		t.Fatal("DFC 置位后应为真")
	}
}

func TestFrameTypeString(t *testing.T) {
	want := map[FrameType]string{
		FrameAck: "ack", FrameShort: "short", FrameLong: "long",
		FrameControl: "control", FrameType(0): "unknown", FrameType(9): "unknown",
	}
	for ft, s := range want {
		if got := ft.String(); got != s {
			t.Errorf("FrameType(%d) = %q，应为 %q", ft, got, s)
		}
	}
}

func TestBuildControlFrame(t *testing.T) {
	// 控制帧 = L=3 的长帧：整帧 9 字节，数据域为空，校验和是 C+A+CI。
	fr := BuildControl(CtrlRSPUD, 7, CIAlarmStatus)
	if len(fr) != 9 {
		t.Fatalf("控制帧长 %d，应为 9", len(fr))
	}
	parsed, n, err := Parse(fr)
	if err != nil {
		t.Fatalf("解析控制帧: %v", err)
	}
	if n != len(fr) {
		t.Fatalf("消耗 %d 字节，应为 %d", n, len(fr))
	}
	if parsed.Type != FrameControl {
		t.Fatalf("帧类型 = %v，应为 control", parsed.Type)
	}
	if parsed.CIField != CIAlarmStatus || len(parsed.Data) != 0 {
		t.Fatalf("CI=%02X Data=%x", parsed.CIField, parsed.Data)
	}
	// FrameLen 对控制帧也要给出整帧长：Reader 靠它决定还差多少字节。
	if got, ok := FrameLen(fr); !ok || got != 9 {
		t.Fatalf("FrameLen = %d,%v，应为 9,true", got, ok)
	}
}

func TestValidatePrimaryAddress(t *testing.T) {
	if err := ValidatePrimaryAddress(0); err != nil {
		t.Fatalf("0 是合法主地址: %v", err)
	}
	if err := ValidatePrimaryAddress(MaxPrimaryAddr); err != nil {
		t.Fatalf("%d 是合法主地址: %v", MaxPrimaryAddr, err)
	}
	// 三个特殊地址的报错必须点名"它是什么"，否则现场只会以为是越界。
	cases := []struct {
		a    byte
		want string
	}{
		{AddrBroadcast, "广播"},
		{AddrSecondary, "次级"},
		{252, "不是主地址"},
	}
	for _, c := range cases {
		err := ValidatePrimaryAddress(c.a)
		if err == nil {
			t.Fatalf("地址 %d 应被拒绝", c.a)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("地址 %d 的报错应包含 %q，得到 %v", c.a, c.want, err)
		}
	}
	if !IsPrimaryAddress(0) || !IsPrimaryAddress(250) {
		t.Fatal("0 与 250 都是合法主地址")
	}
	if IsPrimaryAddress(AddrSecondary) || IsPrimaryAddress(AddrBroadcast) {
		t.Fatal("253/255 不是可用主地址")
	}
}

func TestReqUD2Control(t *testing.T) {
	if got := ReqUD2Control(true); got != CtrlREQUD2FCB {
		t.Fatalf("FCB=1 控制字 = %02X，应为 %02X", got, CtrlREQUD2FCB)
	}
	if got := ReqUD2Control(false); got != CtrlREQUD2NoFCB {
		t.Fatalf("FCB=0 控制字 = %02X，应为 %02X", got, CtrlREQUD2NoFCB)
	}
	// 两个字之间只有 FCB 位不同 —— 否则"翻转 FCB"会顺带改掉别的位。
	if CtrlREQUD2FCB^CtrlREQUD2NoFCB != 0x20 {
		t.Fatalf("FCB 两个控制字之差 = %02X，应只有 bit5", CtrlREQUD2FCB^CtrlREQUD2NoFCB)
	}
}

func TestQuantityKeysAreAddressable(t *testing.T) {
	keys := QuantityKeys()
	if len(keys) < 20 {
		t.Fatalf("物理量键只有 %d 个，VIF 表明显没铺开", len(keys))
	}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k == "" {
			t.Fatal("物理量键不能为空")
		}
		if k != NormalizeQuantity(k) {
			t.Errorf("%q 不是归一化形态（应为 %q）", k, NormalizeQuantity(k))
		}
		if seen[k] {
			t.Errorf("%q 重复", k)
		}
		seen[k] = true
	}
	// 排序是为了让报错时的候选列表稳定可比。
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("未排序：%q 在 %q 之后", keys[i-1], keys[i])
		}
	}
	for _, want := range []string{"energy", "volume", "flow-temperature", "power", "date"} {
		if !seen[want] {
			t.Errorf("缺少 %q", want)
		}
	}
}

func TestFunctionSuffix(t *testing.T) {
	cases := map[byte]string{0: "", 1: "-max", 2: "-min", 3: "-error", 4: "", 7: "-error"}
	for fn, want := range cases {
		if got := FunctionSuffix(fn); got != want {
			t.Errorf("FunctionSuffix(%d) = %q，应为 %q", fn, got, want)
		}
	}
}

func TestNormalizeQuantityEdgeCases(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"  Energy  ":        "energy",
		"flow_temperature":  "flow-temperature",
		"Flow Temperature":  "flow-temperature",
		"a--b":              "a-b",
		"-energy-":          "energy",
		"already-normal-xx": "already-normal-xx",
	}
	for in, want := range cases {
		if got := NormalizeQuantity(in); got != want {
			t.Errorf("NormalizeQuantity(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestApplicationErrorText(t *testing.T) {
	// 这些码是标准枚举，现场最常见的问题全在这张表里。
	for code, want := range map[byte]string{
		0x00: "未指明",
		0x01: "不支持",
		0x02: "溢出",
		0x0C: "无此数据",
		0x11: "未被接受",
	} {
		if got := ApplicationError(code); !strings.Contains(got, want) {
			t.Errorf("ApplicationError(%02X) = %q，应包含 %q", code, got, want)
		}
	}
	// 未知码要说"未知"，不能返回空串（空串在表格里会被当成没有错误）。
	if got := ApplicationError(0x7E); !strings.Contains(got, "未知") {
		t.Fatalf("未知错误码 = %q，应包含「未知」", got)
	}
}

func TestResponseStringForms(t *testing.T) {
	if got := DataVariable.String(); got != "variable" {
		t.Fatalf("DataVariable = %q", got)
	}
	if got := DataFixed.String(); got != "fixed" {
		t.Fatalf("DataFixed = %q", got)
	}
	if got := DataNone.String(); got != "none" {
		t.Fatalf("DataNone = %q", got)
	}
	for k, want := range map[Kind]string{
		KindNone: "none", KindNumeric: "number", KindText: "text", KindBinary: "bytes",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d) = %q，应为 %q", k, got, want)
		}
	}
}

func TestRecordDescribe(t *testing.T) {
	// Describe 的输出是 `-raw` 的主体，必须同时带值、单位与原始编码，
	// 否则现场没法拿它去对照手册。
	numeric := Record{
		Index: 3, Quantity: "energy", Unit: "Wh", Kind: KindNumeric,
		Num: 1234.5, DIF: 0x04, VIF: 0x03,
	}
	s := numeric.Describe()
	for _, want := range []string{"[3]", "energy", "1234.5", "Wh", "DIF=04", "VIF=03"} {
		if !strings.Contains(s, want) {
			t.Errorf("Describe() = %q，缺少 %q", s, want)
		}
	}
	// DIFE/VIFE 存在时必须出现在同一行：存储号与功能位是判读的关键。
	full := Record{
		Index: 0, Quantity: "volume", Unit: "m^3", Kind: KindNumeric, Num: 1,
		DIF: 0x84, DIFEs: []byte{0x01}, VIF: 0x93, VIFEs: []byte{0x01},
		Storage: 2, Tariff: 3, Subunit: 1,
	}
	s = full.Describe()
	for _, want := range []string{"DIFE=01", "VIFE=01", "storage=2", "tariff=3", "subunit=1"} {
		if !strings.Contains(s, want) {
			t.Errorf("Describe() = %q，缺少 %q", s, want)
		}
	}
	// 文本与二进制各自有独立的格式：文本带引号，二进制按十六进制。
	if s := (Record{Kind: KindText, Text: "abc"}).Describe(); !strings.Contains(s, `"abc"`) {
		t.Errorf("文本记录 = %q，应带引号", s)
	}
	if s := (Record{Kind: KindBinary, Raw: []byte{0xDE, 0xAD}}).Describe(); !strings.Contains(s, "dead") {
		t.Errorf("二进制记录 = %q，应按十六进制", s)
	}
	if s := (Record{Kind: KindNone}).Describe(); !strings.Contains(s, "-") {
		t.Errorf("空记录 = %q，应有一个占位符", s)
	}
}

func TestTrimFloat(t *testing.T) {
	cases := map[float64]string{
		0: "0", 1: "1", 1.5: "1.5", 1234.5678: "1234.5678",
		-1.25: "-1.25", 0.001: "0.001", 1e-9: "0", 100: "100",
	}
	for in, want := range cases {
		if got := trimFloat(in); got != want {
			t.Errorf("trimFloat(%v) = %q，应为 %q", in, got, want)
		}
	}
}

func TestStatusText(t *testing.T) {
	cases := map[byte]string{
		0x00:        "ok",
		0x01:        "application-error(1)",
		0x04:        "power-low",
		0x08:        "permanent-error",
		0x10:        "temporary-error",
		0x04 | 0x08: "power-low,permanent-error",
	}
	for status, want := range cases {
		if got := (Header{Status: status}).StatusText(); got != want {
			t.Errorf("StatusText(%02X) = %q，应为 %q", status, got, want)
		}
	}
	// StatusError 只看 bit0-1：电池低电量时读数通常仍然有效，不能降质量。
	if (Header{Status: 0x04}).StatusError() {
		t.Fatal("只有电源低不算应用错误")
	}
	if !(Header{Status: 0x02}).StatusError() {
		t.Fatal("bit0-1 非零应算应用错误")
	}
}

func TestLVARLengthTable(t *testing.T) {
	// LVAR <= BFh 是 ASCII 文本的直接长度；更大的编码是固定长度族。
	cases := []struct {
		lvar byte
		n    int
		ok   bool
	}{
		{0x00, 0, true}, {0x05, 5, true}, {0xBF, 191, true},
		{0xC0, 0, true}, {0xC9, 9, true},
		{0xD0, 0, true}, {0xD9, 9, true},
		{0xE0, 0, true}, {0xEF, 15, true},
		{0xF0, 16, true}, {0xF4, 32, true},
		{0xF5, 48, true}, {0xF6, 64, true},
		// CA..CF / D6..DF(部分) / F7..FF 不是合法 LVAR 编码。
		{0xCA, 0, false}, {0xCF, 0, false}, {0xF7, 0, false}, {0xFF, 0, false},
	}
	for _, c := range cases {
		n, ok := lvarLen(c.lvar)
		if ok != c.ok || (ok && n != c.n) {
			t.Errorf("lvarLen(%02X) = %d,%v，应为 %d,%v", c.lvar, n, ok, c.n, c.ok)
		}
	}
}

func TestFixedUnitTable(t *testing.T) {
	// 固定结构的单位编码里 0x0B..0x13 是 J 族，指数 1e3..1e11 单调递增：
	// 照「有跳变」推（以为 0Dh 是 MJ）会给出错 100 倍的能量值，而错值
	// 看起来同样合理。这里锁住单调性。
	cases := []struct {
		code    byte
		qty     string
		unit    string
		scale   float64
		special Special
	}{
		{0x00, "time", "s", 1, SpecialNone},
		{0x01, "date", "", 1, SpecialDate},
		{0x02, "energy", "Wh", 1, SpecialNone},
		{0x03, "energy", "Wh", 10, SpecialNone},
		{0x0B, "energy", "J", 1e3, SpecialNone},
		{0x0D, "energy", "J", 1e5, SpecialNone},
		{0x13, "energy", "J", 1e11, SpecialNone},
		{0x14, "power", "W", 1, SpecialNone},
		{0x26, "volume", "m^3", 1e-6, SpecialNone},
		{0x2F, "volume-flow", "m^3/h", 1e-6, SpecialNone},
		{0x38, "flow-temperature", "°C", 1e-3, SpecialNone},
		{0x39, "hca", "H.C.A.", 1, SpecialNone},
		{0x3F, "counter", "", 1, SpecialNone},
	}
	for _, c := range cases {
		qty, unit, scale, special := fixedUnit(c.code)
		if qty != c.qty || unit != c.unit {
			t.Errorf("fixedUnit(%02X) = %q,%q，应为 %q,%q", c.code, qty, unit, c.qty, c.unit)
		}
		// Pow10 是浮点幂，1e-6 之类的值会有末位差，按相对误差比。
		if c.scale == 0 || math.Abs(scale-c.scale)/math.Abs(c.scale) > 1e-9 {
			t.Errorf("fixedUnit(%02X) scale = %g，应为 %g", c.code, scale, c.scale)
		}
		if special != c.special {
			t.Errorf("fixedUnit(%02X) special = %v，应为 %v", c.code, special, c.special)
		}
	}
	// 未知单位码要给出可辨识的名字，而不是空串（空串会变成"无名记录"）。
	if qty, _, _, _ := fixedUnit(0x3E); !strings.HasPrefix(qty, "counter-") {
		t.Fatalf("未知单位码 = %q，应以 counter- 开头", qty)
	}
}

func TestDecodeDateRejects(t *testing.T) {
	cases := [][]byte{
		nil, {0x01}, // 长度不对
		{0x00, 0x01},       // 日 = 0
		{0x01, 0x00},       // 月 = 0
		{0x01, 0x0D},       // 月 = 13
		{0x0F, 0x0F, 0x00}, // 过长
		{0x1F, 0x02},       // 2 月 31 日：不存在的日期必须拒绝，不能归一化成 3 月 2 日
	}
	for _, b := range cases {
		if _, ok := decodeDate(b); ok {
			t.Errorf("decodeDate(%x) 应当失败", b)
		}
	}
}

func TestDecodeDateTimeInvalidBit(t *testing.T) {
	// "时间无效"位置位时必须明确失败：把 FFh 解成 2159 年比不给出值更糟。
	if _, ok := decodeDateTime([]byte{0x80, 0x00, 0x01, 0x01}); ok {
		t.Fatal("Type F 的无效位应被识别")
	}
	if _, ok := decodeDateTime([]byte{0x00, 0x80, 0x00, 0x01, 0x01, 0x01}); ok {
		t.Fatal("Type I 的无效位应被识别")
	}
	if _, ok := decodeDateTime([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); ok {
		t.Fatal("全零日期应被拒绝")
	}
	// Type F：2000 年以前的老表用两位年，00..80 归到 2000 年代。
	if tm, ok := decodeDateTime([]byte{0x30, 0x12, 0x0F, 0x33}); !ok || tm.Year() != 2024 {
		t.Fatalf("Type F 解析 = %v,%v（年应为 2024）", tm, ok)
	}
}

func TestMakeTimeRejectsImpossibleDate(t *testing.T) {
	// time.Date 会把 2 月 30 日"归一化"成 3 月 2 日；这种静默归一化会
	// 产出一个不存在的日期，所以必须回读校验。
	if _, ok := makeTime(2024, 2, 30, 0, 0, 0); ok {
		t.Fatal("2 月 30 日应被拒绝")
	}
	if _, ok := makeTime(2024, 1, 1, 24, 0, 0); ok {
		t.Fatal("24 点应被拒绝")
	}
	if _, ok := makeTime(1899, 1, 1, 0, 0, 0); ok {
		t.Fatal("1900 年前的年份应被拒绝")
	}
	if tm, ok := makeTime(2024, 2, 29, 23, 59, 59); !ok || tm.Day() != 29 {
		t.Fatalf("闰日应通过: %v,%v", tm, ok)
	}
}

func TestWriteFrameRejectsEmpty(t *testing.T) {
	st := &fakeStream{}
	if err := WriteFrame(st, nil); err == nil {
		t.Fatal("空帧应被拒绝")
	}
	if err := WriteFrame(st, BuildShort(CtrlSNDNKE, 1)); err != nil {
		t.Fatalf("正常帧: %v", err)
	}
}

func TestReaderBadFramesAndBuffered(t *testing.T) {
	// 一帧坏校验 + 一帧好帧：Reader 应跳过坏的、返回好的，并把坏帧计入
	// BadFrames。驱动靠这个计数把「有字节但不成帧」与「完全没动静」分开。
	good := BuildLong(CtrlRSPUD, 1, CIVariableData, []byte{0x01, 0x02})
	bad := append([]byte(nil), good...)
	bad[len(bad)-2] ^= 0xFF // 破坏校验和，结构与长度都保持完好
	stream := &fakeStream{data: append(append([]byte{0x00, 0xFF}, bad...), good...)}
	r := NewReader(stream)
	if r.Buffered() != 0 {
		t.Fatalf("初始 Buffered = %d", r.Buffered())
	}
	fr, err := r.Next()
	if err != nil {
		t.Fatalf("应跳过坏帧拿到好帧: %v", err)
	}
	if fr.CIField != CIVariableData {
		t.Fatalf("CI = %02X", fr.CIField)
	}
	if r.BadFrames() != 1 {
		t.Fatalf("BadFrames = %d，应为 1（只数内容不对的帧）", r.BadFrames())
	}
	if r.Buffered() != 0 {
		t.Fatalf("取走整帧后 Buffered = %d，应为 0", r.Buffered())
	}
}

func TestReaderCountsOnlyRealBadFrames(t *testing.T) {
	// 起始符之前的噪声不是坏帧：总线上电瞬态本来就会有几字节垃圾，
	// 把它计入 BadFrames 会让驱动误报"线路质量差"。
	good := BuildShort(CtrlREQUD2NoFCB, 3)
	r := NewReader(&fakeStream{data: append([]byte{0x12, 0x34, 0x56}, good...)})
	if _, err := r.Next(); err != nil {
		t.Fatalf("应跨过噪声拿到帧: %v", err)
	}
	if n := r.BadFrames(); n != 0 {
		t.Fatalf("BadFrames = %d，重同步不该计数", n)
	}
}

func TestFindRecordErrors(t *testing.T) {
	recs := []Record{
		{Index: 0, Quantity: "energy", Unit: "Wh", Kind: KindNumeric, Num: 1},
		{Index: 1, Quantity: "volume", Unit: "m^3", Kind: KindNumeric, Num: 2},
		{Index: 2, Quantity: "energy", Storage: 1, Unit: "Wh", Kind: KindNumeric, Num: 3},
	}
	// r: 是语义无关的逃生口。
	if rec, ok, err := FindRecord(recs, "r:1"); !ok || rec.Quantity != "volume" || err != nil {
		t.Fatalf("r:1 = %+v,%v,%v", rec, ok, err)
	}
	if _, _, err := FindRecord(recs, "r:9"); err == nil {
		t.Fatal("r:9 越界应报错")
	}
	// 不存在的量名：报错里必须列出候选，否则现场不知道能填什么。
	_, ok, err := FindRecord(recs, "q:pressure")
	if ok || err == nil {
		t.Fatal("不存在的物理量应报错")
	}
	if !strings.Contains(err.Error(), "energy") || !strings.Contains(err.Error(), "volume") {
		t.Fatalf("报错应列出候选，得到 %v", err)
	}
	// 只写了 storage 但该组不存在。
	if _, ok, err := FindRecord(recs, "q:energy@9"); ok || err == nil {
		t.Fatal("不存在的存储号应报错")
	}
	// 空表达式是配置错误，要单独说清楚。
	if _, _, err := FindRecord(recs, ""); err == nil {
		t.Fatal("空表达式应报错")
	}
	if _, _, err := FindRecord(nil, "q:energy"); err == nil {
		t.Fatal("空记录集应报错")
	}
}

func TestRecordNamesList(t *testing.T) {
	recs := []Record{
		{Quantity: "energy", Unit: "Wh"},
		{Quantity: "volume", Unit: "m^3"},
		{Quantity: "energy", Unit: "Wh", Storage: 1},
	}
	s := RecordNames(recs)
	for _, want := range []string{"q:energy", "q:volume", "q:energy@1"} {
		if !strings.Contains(s, want) {
			t.Errorf("RecordNames = %q，缺少 %q", s, want)
		}
	}
	if RecordNames(nil) == "" {
		t.Fatal("没有记录时应给出明确提示而不是空串")
	}
}

func TestParseRejectsTruncatedLongFrame(t *testing.T) {
	fr := BuildLong(CtrlRSPUD, 1, CIVariableData, []byte{0xAA, 0xBB})
	for i := 0; i < len(fr)-1; i++ {
		if _, _, err := Parse(fr[:i]); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("截断到 %d 字节应报 ErrIncomplete，得到 %v", i, err)
		}
	}
	// 结束符错：整帧结构就不成立。
	bad := append([]byte(nil), fr...)
	bad[len(bad)-1] = 0x00
	if _, _, err := Parse(bad); err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("结束符错应报结构错，得到 %v", err)
	}
}

func TestParseLongFrameLengthMismatch(t *testing.T) {
	// 两个 L 不等：只有第一个 L 被信任时，解析器会从错误的偏移读 CI，
	// 整条记录都会错位。必须显式拒绝。
	fr := BuildLong(CtrlRSPUD, 1, CIVariableData, []byte{0x01})
	fr[2] ^= 0x01
	if _, _, err := Parse(fr); !errors.Is(err, ErrFrame) {
		t.Fatalf("双 L 不等应报 ErrFrame，得到 %v", err)
	}
}

func TestGoldenFrameRoundTrip(t *testing.T) {
	// 金帧 → 解析 → 重新组帧，字节必须完全一致。这是"编码与解码共用
	// 同一份长度/量纲规则"的直接证据。
	fr := goldenFrame(t)
	parsed, n, err := Parse(fr)
	if err != nil {
		t.Fatalf("解析金帧: %v", err)
	}
	if n != len(fr) {
		t.Fatalf("消耗 %d 字节，应为 %d", n, len(fr))
	}
	rebuilt := BuildLong(parsed.C, parsed.A, parsed.CIField, parsed.Data)
	if hex.EncodeToString(rebuilt) != hex.EncodeToString(fr) {
		t.Fatalf("重组帧不一致:\n got %x\nwant %x", rebuilt, fr)
	}
}
