package sl651

import (
	"testing"
)

// 附录 C 表 C.1 共 133 行：14 项引导符类（F0H~FDH）+ 117 项常规要素
// （01H~75H）+ 1 行保留区（76H~EFH）+ 1 行自定义扩展（FFXXH）。
// 保留区与扩展区不是可解释的数值要素，因此表里应当正好 131 项。
func TestGuideTableSize(t *testing.T) {
	const want = 131
	if got := len(guides); got != want {
		t.Fatalf("len(guides) = %d, want %d", got, want)
	}
	if got := len(Guides()); got != want {
		t.Fatalf("len(Guides()) = %d, want %d", got, want)
	}
}

// Guides() 必须升序：自检命令直接拿它列表，顺序不稳会让同一台设备每次输出
// 不同（map 遍历无序）。
func TestGuidesSorted(t *testing.T) {
	got := Guides()
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("Guides()[%d]=%02X <= [%d]=%02X", i, got[i], i-1, got[i-1])
		}
	}
}

// ASCII 简称与中文名都不能重复，否则 LookupGuide 的精确匹配结果取决于
// map 遍历顺序。
func TestGuideAliasAndNameUnique(t *testing.T) {
	alias := map[string]byte{}
	name := map[string]byte{}
	for code, g := range guides {
		if g.Alias == "" {
			t.Errorf("guide %02X has empty alias", code)
		}
		if g.Name == "" {
			t.Errorf("guide %02X has empty name", code)
		}
		if prev, dup := alias[g.Alias]; dup {
			t.Errorf("alias %q used by %02X and %02X", g.Alias, prev, code)
		}
		alias[g.Alias] = code
		if prev, dup := name[g.Name]; dup {
			t.Errorf("name %q used by %02X and %02X", g.Name, prev, code)
		}
		name[g.Name] = code
	}
}

// 逐项锁定附录 C 的关键行：这些是「照数字顺序推算」会写错的地方，
// 必须由测试而不是由记忆保证。
func TestGuideAppendixCAnchors(t *testing.T) {
	cases := []struct {
		guide    byte
		alias    string
		unit     string
		digits   int
		decimals int
		width    int
		def      byte
	}{
		// 序号 15~18：断面面积到时间步长码。
		{GAC, "AC", "m²", 8, 2, 4, 0x22},
		{GAI, "AI", "℃", 3, 1, 2, 0x11},
		{GC, "C", "℃", 3, 1, 2, 0x11},
		// 时间步长码：表 C.3 规定 3 字节、定义字节固定 18H。
		{GDR, "DRnnn", "", 6, 0, 3, 0x18},
		// 序号 19：时段长是 HH.mm，4 位、无小数。
		{GDT, "DT", "h.min", 4, 0, 2, 0x10},
		// 序号 36：16H 是 M80，不是 M70——「按序号 +1 推」会在这里出错。
		{0x16, "M80", "%", 4, 1, 2, 0x11},
		{0x15, "M60", "%", 4, 1, 2, 0x11},
		{GM100, "M100", "%", 4, 1, 2, 0x11},
		// 序号 39：开机台数 N(2)，1 字节。
		{GNS, "NS", "台", 2, 0, 1, 0x08},
		// 序号 52：降水量累计值 N(6,1)，3 字节——比 P1 的 N(5,1) 宽。
		{GPT, "PT", "mm", 6, 1, 3, 0x19},
		{GP1, "P1", "mm", 5, 1, 3, 0x19},
		// 序号 53：流量 N(9,3)，5 字节。
		{GQ, "Q", "m³/s", 9, 3, 5, 0x2B},
		// 序号 64：输沙量 N(11,3)，6 字节。
		{GSW, "SW", "万t", 11, 3, 6, 0x33},
		// 序号 70：电压 N(4,2)。
		{GVT, "VT", "V", 4, 2, 2, 0x12},
		// 序号 71：水位 N(7,3)，4 字节。
		{GZ, "Z", "m", 7, 3, 4, 0x23},
		// 序号 83：状态字是 4 字节 HEX，不是 BCD。
		{GZT, "ZT", "", 0, 0, 4, 0x20},
		// 序号 86：电导率 N(5) 无小数。
		{GCOND, "COND", "μS/cm", 5, 0, 3, 0x18},
		// 序号 90：氨氮 N(6,2)，3 字节。
		{GNH4N, "NH4N", "mg/L", 6, 2, 3, 0x1A},
		// 序号 110：水表剩余水量 N(11,3)，6 字节。
		{GSYL1, "SYL1", "m³", 11, 3, 6, 0x33},
		// 序号 129：交流 A 相电流 N(4,1)。
		{GVIA, "VIA", "A", 4, 1, 2, 0x11},
	}
	for _, c := range cases {
		g, ok := GuideInfo(c.guide)
		if !ok {
			t.Errorf("guide %02X missing", c.guide)
			continue
		}
		if g.Alias != c.alias {
			t.Errorf("guide %02X alias = %q, want %q", c.guide, g.Alias, c.alias)
		}
		if g.Unit != c.unit {
			t.Errorf("guide %02X unit = %q, want %q", c.guide, g.Unit, c.unit)
		}
		if g.Digits != c.digits || g.Decimals != c.decimals {
			t.Errorf("guide %02X N(%d,%d), want N(%d,%d)", c.guide, g.Digits, g.Decimals, c.digits, c.decimals)
		}
		if got := g.Bytes(); got != c.width {
			t.Errorf("guide %02X Bytes() = %d, want %d", c.guide, got, c.width)
		}
		if got := g.DataDef(); got != c.def {
			t.Errorf("guide %02X DataDef() = %02X, want %02X", c.guide, got, c.def)
		}
	}
}

// 引导符类的定义字节由注 a~f 写死，绝不能按宽度换算：
// 观测时间若按 N(5) 换算会得到 0x28（高 5 位 = 5 字节），
// 而规约要求必须写 F0H；F1H 同理。
//
// DRP/DRZ 是例外：表 C.1 给的是「12 字节 HEX」「24 字节 HEX」，宽度能放进
// 定义字节高 5 位，因此仍按 (字节数<<3)|小数位 换算（0x60 / 0xC0），
// 只有宽度本身由规约固定这一点需要查表。
func TestGuideFixedDef(t *testing.T) {
	cases := []struct {
		guide byte
		def   byte
		width int
	}{
		{GTT, 0xF0, 5},
		{GSI, 0xF1, 5},
		{GRGZS, 0xF2, 0}, // 长度由报文总长决定
		{GPIC, 0xF3, 0},
		{GDRP, 0x60, 12},
		{GDRZ1, 0xC0, 24},
		{GDRZ8, 0xC0, 24},
		{GDATA, 0xF6, 0},
		{GDR, 0x18, 3},
	}
	for _, c := range cases {
		g, ok := GuideInfo(c.guide)
		if !ok {
			t.Fatalf("guide %02X missing", c.guide)
		}
		if g.DataDef() != c.def {
			t.Errorf("guide %02X DataDef() = %02X, want %02X", c.guide, g.DataDef(), c.def)
		}
		if g.Bytes() != c.width {
			t.Errorf("guide %02X Bytes() = %d, want %d", c.guide, g.Bytes(), c.width)
		}
		if wantVar := c.width == 0; g.Variable() != wantVar {
			t.Errorf("guide %02X Variable() = %v, want %v", c.guide, g.Variable(), wantVar)
		}
	}
}

// DRP/DRZ/ZT 是 HEX 数据：数长度与量值都不是十进制。
func TestGuideHexCoded(t *testing.T) {
	for _, guide := range []byte{GDRP, GDRZ1, GDRZ8, GZT} {
		g, ok := GuideInfo(guide)
		if !ok {
			t.Fatalf("guide %02X missing", guide)
		}
		if !g.Hex {
			t.Errorf("guide %02X should be Hex-coded", guide)
		}
	}
	// ZT 是状态字，没有数值含义。
	if g, _ := GuideInfo(GZT); g.Scale != 0 {
		t.Errorf("ZT scale = %v, want 0", g.Scale)
	}
	if g, _ := GuideInfo(GDRP); g.Scale != 0.1 {
		t.Errorf("DRP scale = %v, want 0.1", g.Scale)
	}
	if g, _ := GuideInfo(GDRZ1); g.Scale != 0.01 {
		t.Errorf("DRZ1 scale = %v, want 0.01", g.Scale)
	}
}

// LookupGuide 三种写法都要认，且必须稳定。
func TestLookupGuide(t *testing.T) {
	cases := []struct {
		in    string
		guide byte
		ok    bool
	}{
		{"39", GZ, true},  // 两位十六进制
		{"39H", 0, false}, // 带 H 后缀不算十六进制写法，也不能当简称
		{"Z", GZ, true},   // ASCII 简称
		{"z", GZ, true},   // 大小写不敏感
		{"PT", GPT, true}, // 简称精确匹配
		{"P1", GP1, true}, // "P1" 不能命中 "P12"
		{"P12", GP12, true},
		{"水位", GZ, true}, // 中文包含匹配，多命中取编码最小者（39 < 3C）
		{"瞬时河道水位", GZ, true},
		{"降水量累计值", GPT, true},
		{"  Z  ", GZ, true}, // 去空白
		{"", 0, false},
		{"99", 0, false}, // 表内没有 0x99
		{"不存在的要素", 0, false},
	}
	for _, c := range cases {
		got, ok := LookupGuide(c.in)
		if ok != c.ok {
			t.Errorf("LookupGuide(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.guide {
			t.Errorf("LookupGuide(%q) = %02X, want %02X", c.in, got, c.guide)
		}
	}
}

// 反查必须稳定：同一个查询跑多遍结果一致（map 遍历是无序的）。
func TestLookupGuideStable(t *testing.T) {
	first, ok := LookupGuide("水位")
	if !ok {
		t.Fatal("LookupGuide(水位) failed")
	}
	for i := 0; i < 50; i++ {
		got, _ := LookupGuide("水位")
		if got != first {
			t.Fatalf("iter %d: %02X != %02X", i, got, first)
		}
	}
}

// GuideName/GuideUnit/GuideAlias 对表外引导符要给出可读结果而不是空串。
func TestGuideAccessorsUnknown(t *testing.T) {
	const unknown = 0x80
	if got := GuideName(unknown); got != "引导符 80" {
		t.Errorf("GuideName = %q", got)
	}
	if got := GuideUnit(unknown); got != "" {
		t.Errorf("GuideUnit = %q, want empty", got)
	}
	if got := GuideAlias(unknown); got != "" {
		t.Errorf("GuideAlias = %q, want empty", got)
	}
	if _, ok := GuideInfo(unknown); ok {
		t.Error("GuideInfo(0x80) should be false")
	}
}

// TimeStep：表 C.3 的结构是 3 字节 BCD「日 时 分」。
func TestTimeStep(t *testing.T) {
	cases := []struct {
		step TimeStep
		min  int
		ok   bool
		str  string
	}{
		{StepHourlyCombo(), 0, true, "1h 时段组合"},
		{StepMin(5), 5, true, "5min"},
		{StepMin(30), 30, true, "30min"},
		{StepMin(59), 59, true, "59min"},
		{StepHour(1), 60, true, "1h"},
		{StepHour(23), 1380, true, "23h"},
		{StepDay(1), 1440, true, "1d"},
		{TimeStep{0x00, 0x01, 0x00}, 60, true, "1h"},
		// 非法：两个字节非零（表 C.3 要求一般只能有一个非零）。
		{TimeStep{0x00, 0x01, 0x30}, 0, false, "时间步长码 000130"},
		// 非法 BCD。
		{TimeStep{0x00, 0x00, 0x1A}, 0, false, "时间步长码 00001A"},
	}
	for _, c := range cases {
		got, ok := c.step.Minutes()
		if ok != c.ok {
			t.Errorf("%v Minutes() ok = %v, want %v", c.step, ok, c.ok)
			continue
		}
		if ok && got != c.min {
			t.Errorf("%v Minutes() = %d, want %d", c.step, got, c.min)
		}
		if s := c.step.String(); s != c.str {
			t.Errorf("%v String() = %q, want %q", c.step, s, c.str)
		}
	}
}
