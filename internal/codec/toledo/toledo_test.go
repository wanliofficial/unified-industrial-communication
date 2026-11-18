package toledo

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// 这些测试钉死的是「帧格式」这一层。Toledo 兼容帧不是某一本手册独有的，而是
// 现场多本手册共同记载的一个家族；每一条断言都尽量挂到一条真实实例或一个明确
// 的规则上，而不是凭印象写「大概长这样」。解析错了不会报协议错，只会静默吐出
// 一个「看起来合理、其实全错」的读数——所以格式层的正确性值得逐字节钉死。

func TestParse_D39CanonicalInstance(t *testing.T) {
	// D39 手册逐字节给出的实例：
	//   53 54 2C 47 53 2C 2B 30 30 31 34 39 2E 30 6B 67 0D 0A
	//   即 ST,GS,+00149.0kg<CR><LF>
	// WEIGHT(7) = 5 位整数 + '.' + 1 位小数 = 00149.0，即 149.0 kg，毛重、稳定。
	line, _ := hex.DecodeString("53542C47532C2B30303134392E306B670D0A")
	r, err := Parse(line)
	if err != nil {
		t.Fatalf("parse D39 实例：%v", err)
	}
	if r.Status != StatusStable {
		t.Errorf("status = %q，应为 ST", r.Status)
	}
	if r.Mode != ModeGross {
		t.Errorf("mode = %q，应为 GS（毛重）", r.Mode)
	}
	if r.Weight != 149.0 {
		t.Errorf("weight = %v，应为 149.0", r.Weight)
	}
	if r.Unit != "kg" {
		t.Errorf("unit = %q，应为 kg", r.Unit)
	}
	if r.Sign != 1 {
		t.Errorf("sign = %d，应为 +1", r.Sign)
	}
	if r.Decimals != 1 {
		t.Errorf("decimals = %d，应为 1", r.Decimals)
	}
	if r.Masked {
		t.Errorf("D39 实例不应是掩码")
	}
	if r.DualUnit {
		t.Errorf("D39 实例不应是双单位")
	}
	if r.Raw != "ST,GS,+00149.0kg" {
		t.Errorf("raw = %q，应去掉行尾 CR/LF", r.Raw)
	}
}

func TestParse_StatusVariants(t *testing.T) {
	cases := []struct {
		line string
		want Status
	}{
		{"ST,GS,+00149.0kg", StatusStable},
		{"US,GS,+00149.0kg", StatusMotion}, // 运动中
		{"OL,GS,+99999.9kg", StatusOverload},
		{"UL,GS,+00000.0kg", StatusUnderload},
		{"st,gs,+00149.0kg", StatusStable}, // 小写也收
	}
	for _, c := range cases {
		r, err := Parse([]byte(c.line))
		if err != nil {
			t.Fatalf("parse %q：%v", c.line, err)
		}
		if r.Status != c.want {
			t.Errorf("%q: status = %q，应为 %q", c.line, r.Status, c.want)
		}
	}
}

func TestParse_ModeVariants(t *testing.T) {
	cases := []struct {
		line string
		want Mode
	}{
		{"ST,GS,+00149.0kg", ModeGross},
		{"ST,NT,+00149.0kg", ModeNet},
		{"ST,TR,+00149.0kg", ModeTare},
		{"ST,GR,+00149.0kg", ModeGross}, // GR 是 GS 的别名
		{"ST,nt,+00149.0kg", ModeNet},   // 小写也收
	}
	for _, c := range cases {
		r, err := Parse([]byte(c.line))
		if err != nil {
			t.Fatalf("parse %q：%v", c.line, err)
		}
		if r.Mode != c.want {
			t.Errorf("%q: mode = %q，应为 %q", c.line, r.Mode, c.want)
		}
	}
}

func TestParse_NegativeWeight(t *testing.T) {
	// 负号：净重通常为负（容器比内容重）。D39 值段以 + 或 - 开头。
	r, err := Parse([]byte("ST,NT,-00012.5kg"))
	if err != nil {
		t.Fatalf("parse：%v", err)
	}
	if r.Sign != -1 {
		t.Errorf("sign = %d，应为 -1", r.Sign)
	}
	if r.Weight != -12.5 {
		t.Errorf("weight = %v，应为 -12.5", r.Weight)
	}
}

func TestParse_ZeroAndIntegerVariants(t *testing.T) {
	// 不同仪表小数位不同：有的无小数点，有的整数位更多。解析不强求固定宽度，
	// 只在 Format 时按宽度对齐。
	cases := []struct {
		line     string
		weight   float64
		decimals int
	}{
		{"ST,GS,+00000.0kg", 0.0, 1},
		{"ST,GS,+00123kg", 123.0, 0},         // 无小数点
		{"ST,GS,+123456.78kg", 123456.78, 2}, // 变体：整数位更多
		{"ST,GS,+0.5kg", 0.5, 1},
	}
	for _, c := range cases {
		r, err := Parse([]byte(c.line))
		if err != nil {
			t.Fatalf("parse %q：%v", c.line, err)
		}
		if r.Weight != c.weight {
			t.Errorf("%q: weight = %v，应为 %v", c.line, r.Weight, c.weight)
		}
		if r.Decimals != c.decimals {
			t.Errorf("%q: decimals = %d，应为 %d", c.line, r.Decimals, c.decimals)
		}
	}
}

func TestParse_MaskedValue(t *testing.T) {
	// 超量程/出错时数值段是掩码占位（如 M-------L、----），没有数字。
	// 这不是解析错误：Weight 无意义，但 Status 仍然有效（OL 恰恰靠 Status 表达）。
	cases := []string{
		"OL,GS,M-------Lkg", // 掩码 + 单位
		"OL,GS,--------",    // 纯掩码无单位
		"US,NT,------kg",    // 运动中掩码
	}
	for _, line := range cases {
		r, err := Parse([]byte(line))
		if err != nil {
			t.Fatalf("parse %q：%v", line, err)
		}
		if !r.Masked {
			t.Errorf("%q: 应判为 Masked", line)
		}
		if r.Weight != 0 {
			t.Errorf("%q: 掩码时 weight 应为 0，得到 %v", line, r.Weight)
		}
	}
}

func TestParse_LbOzDualUnit(t *testing.T) {
	// 磅盎司双单位：lb oz / lb:oz 归一为 lb_oz，标记 DualUnit。
	for _, line := range []string{
		"ST,GS,+00149.0lb oz",
		"ST,GS,+00149.0lb:oz",
	} {
		r, err := Parse([]byte(line))
		if err != nil {
			t.Fatalf("parse %q：%v", line, err)
		}
		if r.Unit != UnitLbOz {
			t.Errorf("%q: unit = %q，应为 lb_oz", line, r.Unit)
		}
		if !r.DualUnit {
			t.Errorf("%q: 应标记 DualUnit", line)
		}
	}
}

func TestParse_BinarySTXRejected(t *testing.T) {
	// 二进制 TOLEDO 连续帧以 STX(0x02) 开头。这里识别并给出可执行指引，而不是
	// 当 ASCII 硬解出垃圾值。
	line, _ := hex.DecodeString("0255430300303030303030303030300D00")
	_, err := Parse(line)
	if err == nil {
		t.Fatal("二进制 STX 帧应被拒绝")
	}
	if !strings.Contains(err.Error(), "二进制") {
		t.Errorf("报错应提到「二进制」，得到 %v", err)
	}
}

func TestParse_Errors(t *testing.T) {
	// 真正报错的是段数/状态/模式。注意「ST,GS,notanumber」会被判为掩码（无数字），
	// 不是错误——仪表用占位符表达出错是合法的。
	cases := map[string]string{
		"":               "empty",
		"ST,GS":          "3 段", // 只有两段
		"ST,GS,+1.0kg,x": "3 段", // 四段
		"XX,GS,+1.0kg":   "未知状态",
		"ST,ZZ,+1.0kg":   "未知毛净",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil {
			t.Errorf("%q 应报错", in)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q 的报错应提到 %q，得到 %v", in, want, err)
		}
	}
}

func TestParse_NumericOnlyValueBecomesMasked(t *testing.T) {
	// 数值段完全没有数字（哪怕带符号/单位）→ 判为掩码，不报错。
	r, err := Parse([]byte("OL,GS,-kg"))
	if err != nil {
		t.Fatalf("parse：%v", err)
	}
	if !r.Masked {
		t.Errorf("无数字应判为 Masked")
	}
}

func TestFormat_RoundTrip(t *testing.T) {
	// Format → Parse 应还原出等价读数。decimals=1 时按 D39 的 7 字符宽度对齐。
	cases := []struct {
		r        Reading
		decimals int
		want     string
	}{
		{Reading{Status: StatusStable, Mode: ModeGross, Sign: 1, Weight: 149.0, Unit: "kg"}, 1, "ST,GS,+00149.0kg"},
		{Reading{Status: StatusStable, Mode: ModeNet, Sign: -1, Weight: -12.5, Unit: "kg"}, 1, "ST,NT,-00012.5kg"},
		{Reading{Status: StatusMotion, Mode: ModeGross, Sign: 1, Weight: 0.0, Unit: "kg"}, 1, "US,GS,+00000.0kg"},
	}
	for _, c := range cases {
		got := string(bytes.TrimRight(Format(c.r, c.decimals), "\r\n"))
		if got != c.want {
			t.Errorf("Format = %q，应为 %q", got, c.want)
			continue
		}
		// 往返：解回来应与原读数一致（除 Raw/DualUnit）。
		r, err := Parse([]byte(got))
		if err != nil {
			t.Fatalf("round-trip parse %q：%v", got, err)
		}
		if r.Status != c.r.Status || r.Mode != c.r.Mode || r.Sign != c.r.Sign ||
			r.Weight != c.r.Weight || r.Unit != Unit(c.r.Unit) {
			t.Errorf("round-trip 不一致：%+v vs 原 %+v", r, c.r)
		}
	}
}

func TestFormat_D39WidthAlignment(t *testing.T) {
	// 钉死 7 字符数值段宽度：整数部分左补零到 5 位（decimals=1）。
	got := string(bytes.TrimRight(Format(Reading{Status: StatusStable, Mode: ModeGross, Sign: 1, Weight: 149.0, Unit: "kg"}, 1), "\r\n"))
	if got != "ST,GS,+00149.0kg" {
		t.Errorf("宽度对齐错误：%q（D39 实例 ST,GS,+00149.0kg）", got)
	}
}

func TestFormat_ClampsDecimals(t *testing.T) {
	// decimals 越界被夹到 [0, valueWidth-1]：99 夹到 6 位（应含小数点），
	// -3 夹到 0 位（不应含小数点）。
	big := Format(Reading{Status: StatusStable, Mode: ModeGross, Sign: 1, Weight: 1.0, Unit: "kg"}, 99)
	if !strings.Contains(string(big), ".") {
		t.Errorf("decimals 夹到上限后应仍有小数点：%q", big)
	}
	zero := Format(Reading{Status: StatusStable, Mode: ModeGross, Sign: 1, Weight: 1.0, Unit: "kg"}, -3)
	if strings.Contains(string(zero), ".") {
		t.Errorf("decimals=0 时不应有小数点：%q", zero)
	}
}

// ---------------------------------------------------------------------------
// 精度：字符串拆分，不在 float64 上取整
// ---------------------------------------------------------------------------

// splitSigned 必须走字符串拆分：先「乘 10^n 取整」再格式化会在大值上丢精度。
//
// float64 尾数只有 15~16 位有效十进制；工业衡器的大毛重（几十吨、以 g 为
// 单位）加上小数位很容易越过这个边界。这类错误的表现是「末几位莫名其妙变了
// 一位」，现场极难排查，所以这里把边界值逐字节钉死。
func TestSplitSignedKeepsPrecision(t *testing.T) {
	cases := []struct {
		name     string
		v        float64
		decimals int
		intPart  string
		fracPart string
		sign     int
	}{
		{name: "普通值", v: 149.0, decimals: 1, intPart: "149", fracPart: "0", sign: 1},
		{name: "负值", v: -12.5, decimals: 1, intPart: "12", fracPart: "5", sign: -1},
		// 15 位整数部分：仍在 float64 可精确表示范围内，必须逐位保留。
		{name: "十五位整数", v: 123456789012345.0, decimals: 1,
			intPart: "123456789012345", fracPart: "0", sign: 1},
		// 大值 + 小数：整数与小数都要完整。
		{name: "大值带小数", v: 999999999.5, decimals: 1,
			intPart: "999999999", fracPart: "5", sign: 1},
		// 小数位不足时右侧补零（不是截断）。
		{name: "小数补零", v: 0.1, decimals: 2, intPart: "0", fracPart: "10", sign: 1},
		// 小数位多于实际时按 decimals 截断。
		{name: "小数截断", v: 1.23456, decimals: 2, intPart: "1", fracPart: "23", sign: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, fp, sg := splitSigned(c.v, c.decimals)
			if ip != c.intPart || fp != c.fracPart || sg != c.sign {
				t.Fatalf("splitSigned(%v, %d) = (%q, %q, %d)，期望 (%q, %q, %d)",
					c.v, c.decimals, ip, fp, sg, c.intPart, c.fracPart, c.sign)
			}
		})
	}
}

// decimals 为负时不得 panic：Format 会钳到 0，但 splitSigned 作为核心函数
// 也必须自带防御（原实现会在 fracPart[:decimals] 处切片越界 panic）。
func TestSplitSignedRejectsNegativeDecimals(t *testing.T) {
	ip, fp, sg := splitSigned(1.0, -3)
	if ip != "1" || fp != "" || sg != 1 {
		t.Fatalf("splitSigned(1.0, -3) = (%q, %q, %d)", ip, fp, sg)
	}
}

// 大值组帧后必须能被 Parse 原样解回来（往返一致）。
func TestFormatParseRoundTripLargeValue(t *testing.T) {
	for _, v := range []float64{0, 0.1, 149.0, -12.5, 999999999.5, 123456789012345} {
		sign := 1
		if v < 0 {
			sign = -1
		}
		frame := Format(Reading{
			Status: StatusStable, Mode: ModeGross,
			Sign: sign, Weight: v, Unit: "kg",
		}, 1)
		r, err := Parse(frame)
		if err != nil {
			t.Fatalf("Parse(%q): %v", frame, err)
		}
		if r.Weight != v {
			t.Fatalf("往返后 Weight = %v，期望 %v（帧 %q）", r.Weight, v, frame)
		}
	}
}
