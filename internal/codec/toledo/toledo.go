// Package toledo 实现 METTLER TOLEDO「连续输出（Continuous Output）」的纯函数编解码。
//
// 只做「字节 ↔ 语义」：不建连接、不读时钟、不持有状态。主站驱动与内置秤模拟
// 共用这里的解析与组帧，避免两边对同一帧格式各自理解而漂移。
//
// # 定位：秤是「主动推送」的一端，没有地址也没有请求
//
// TOLEDO 连续输出与仓库里其它协议有一个根本差别：衡器**自己不询问就持续往上送**
// 帧（典型 10~25 帧/秒），帧里也没有站号、寄存器、功能码这类可寻址的东西。因此
// 这里解析出来的不是「某个寄存器的值」，而是**一帧读数**：状态（稳定/运动/过载）、
// 毛净模式、带符号的重量、单位。驱动负责把这些读数映射到点表里的各个点位
// （weight / gross / net / status / unit / stable …）。
//
// # 实现哪种帧格式：ASCII 逗号三段式
//
// 「TOLEDO」在业界并不对应单一帧格式，而是一个家族。本包实现的是现场最常见、
// 也是各国产仪表标注「Toledo compatible」时普遍采用的**ASCII 逗号
// 三段式**，其规范形态（D39 手册逐字节给出的实例）为：
//
//	53 54 2C 47 53 2C 2B 30 30 31 34 39 2E 30 6B 67 0D 0A
//	即 ST,GS,+00149.0kg<CR><LF>
//
// 三段分别是：状态（ST/US/OL/UL）、毛净模式（GS/NT/TR）、`<符号><数值><单位>`。
// 选它作为唯一实现格式的理由在文档里逐条说明，见 §「明确不做」。
package toledo

// 本文件是「帧格式」这一层。连续输出的帧边界只有 CR/LF，没有长度字段也没有
// 二进制同步字——这让「怎么从一段字节流里认出一帧」变成一件必须写对、否则症状
// 极其罕见的事。典型故障：半双工总线上一次迟到的帧在下一轮被当成新一轮的首部。
// 因此 Reader（io.go）强制以「首字节是已知状态助记符或 STX」为帧的起点，其余
// 一切丢弃并计数。

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxFrame 是一帧的上限（字节）。
//
// 连续输出帧很短（典型 < 30 字节）。设上限是为了防止对端（或噪声）一直不回
// CR/LF 时无限吞字节把内存吃满；超过就丢弃整段并重新同步。
const MaxFrame = 64

// 帧内数值段的固定宽度（不含符号）。D39 的「WEIGHT(7)」即此宽度：5 位整数 + '.'
// + 1 位小数。组帧时按此宽度对齐，解析时不强制（现场有变体的整数位更多）。
const valueWidth = 7

// Status 是一帧里的称量状态（第一段）。
type Status string

const (
	StatusStable    Status = "ST" // 稳定
	StatusMotion    Status = "US" // 运动中（unstable）
	StatusOverload  Status = "OL" // 超量程（overload）
	StatusUnderload Status = "UL" // 欠量程（underload）
)

// Mode 是一帧里的毛/净/皮模式（第二段）。
type Mode string

const (
	ModeGross Mode = "GS" // 毛重
	ModeNet   Mode = "NT" // 净重
	ModeTare  Mode = "TR" // 皮重
)

// Unit 是帧里的计量单位（第三段数值后的字母串，已归一为小写）。
//
// 单位集合是开放的：仪表可自定义（kg/lb/g/t/oz/kN/tn 及磅盎司等）。因此这里
// 不做白名单校验，原样保留解析出的 token；驱动把它作为字符串点位暴露，由使用方
// 决定是否换算。lb_oz 是磅盎司双单位模式的标记，其数值语义（磅 + 盎司的字段排布）
// 与单一标量不同，单独标记出来，见 Reading.DualUnit。
type Unit string

const (
	UnitLbOz Unit = "lb_oz" // 磅盎司双单位（lb oz / lb:oz）
)

// Reading 是解析出的一帧读数。
//
// 它是「语义值」而不是「字节」：驱动拿它去填各个点位，模拟器和自检命令拿它去
// 打印人类可读的结果。Raw 保留原始文本，供排障时对照「我们解出来的」与「线上真
// 正的」。
type Reading struct {
	// Status 是称量状态。
	Status Status
	// Mode 是毛/净/皮模式。
	Mode Mode
	// Sign 是 +1 / -1。
	Sign int
	// Weight 是带符号、已按小数点还原的重量。Masked 为真时无意义（0）。
	Weight float64
	// Unit 是计量单位（小写 token；lb_oz 表示磅盎司双单位）。
	Unit Unit
	// Decimals 是数值里的小数位数。组帧时决定对齐宽度，解析时供诊断。
	Decimals int
	// Masked 为真表示数值段不是数字（仪表在过载/出错时用掩码字符占位，如
	// M-------L）。此时 Weight 无意义，但 Status 仍然有效——「超量程」恰恰是
	// 靠 Status 而非数值表达的。读取重量类点位应据此判坏。
	Masked bool
	// DualUnit 为真表示磅盎司双单位模式：Weight 只是按字面解析出的标量，
	// 不代表单一物理量（磅与盎司的排布方式各厂商不一），使用方应谨慎。
	DualUnit bool
	// Raw 是去掉行尾 CR/LF 后的原始帧文本。
	Raw string
}

// Parse 解析一帧 ASCII 连续输出。
//
// 输入应是一行（可含或不含行尾 CR/LF）。解析失败返回带上下文的错误，驱动据此
// 把「收到了字节流但一帧都解不出」与「根本没收到」区分开——前者通常是波特率/
// 校验位配错或帧格式选错，后者才是链路断。
func Parse(line []byte) (Reading, error) {
	s := strings.TrimRight(string(line), "\r\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return Reading{}, fmt.Errorf("toledo: empty line")
	}

	// 二进制 TOLEDO 连续帧以 STX(0x02) 开头。它不吃 CR/LF 分帧、带状态字 A/B/C
	// 与校验和，语义与本包的 ASCII 三段式完全不同。这里明确识别并给出可执行
	// 的指引，而不是把它当 ASCII 硬解出垃圾值——那会是一种「值看起来合理、其实
	// 全错」的最坏故障。
	if len(s) > 0 && s[0] == 0x02 {
		return Reading{}, fmt.Errorf("toledo: 收到 TOLEDO 二进制连续帧（STX 开头，18 字节带状态字与校验和）；" +
			"本驱动只实现 ASCII 连续输出格式 ST,GS,+00149.0kg，请在仪表上切换到 ASCII/Toledo 连续输出格式")
	}

	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return Reading{}, fmt.Errorf("toledo: 帧应分为 3 段（状态,毛净,数值），实际 %d 段：%q", len(parts), s)
	}

	statusTok := strings.TrimSpace(parts[0])
	modeTok := strings.TrimSpace(parts[1])
	valTok := parts[2]

	status, ok := parseStatus(statusTok)
	if !ok {
		return Reading{}, fmt.Errorf("toledo: 未知状态助记符 %q（应为 ST/US/OL/UL）", statusTok)
	}
	mode, ok := parseMode(modeTok)
	if !ok {
		return Reading{}, fmt.Errorf("toledo: 未知毛净模式 %q（应为 GS/NT/TR）", modeTok)
	}

	weight, unit, decimals, masked, dual, err := parseValue(valTok)
	if err != nil {
		return Reading{}, fmt.Errorf("toledo: 数值段 %q：%w", valTok, err)
	}

	sign := signOf(valTok)
	// Weight 保存**带符号**的值：符号单独存 Sign 供诊断/显示，但驱动取数直接用
	// Weight，避免每处都要记得乘一次符号而漏乘。
	return Reading{
		Status:   status,
		Mode:     mode,
		Sign:     sign,
		Weight:   float64(sign) * weight,
		Unit:     unit,
		Decimals: decimals,
		Masked:   masked,
		DualUnit: dual,
		Raw:      s,
	}, nil
}

// parseStatus 识别状态助记符（大小写不敏感）。
func parseStatus(tok string) (Status, bool) {
	switch strings.ToUpper(tok) {
	case "ST":
		return StatusStable, true
	case "US":
		return StatusMotion, true
	case "OL":
		return StatusOverload, true
	case "UL":
		return StatusUnderload, true
	}
	return "", false
}

// parseMode 识别毛/净/皮模式（大小写不敏感）。
//
// GR 是 GS（毛重）的别名：部分仪表（拼接式帧）用 GR 表示 gross，逗号三段式普遍
// 用 GS。两者都收，归一为 ModeGross。
func parseMode(tok string) (Mode, bool) {
	switch strings.ToUpper(tok) {
	case "GS", "GR":
		return ModeGross, true
	case "NT":
		return ModeNet, true
	case "TR":
		return ModeTare, true
	}
	return "", false
}

// parseValue 解析第三段 `<符号><数值><单位>`。
//
// 规则：
//   - 可选的符号 + 或 -（其余字符视为正号）；
//   - 数值段是 `[0-9]*\.?[0-9]*`，至少要有数字，否则视为掩码（Masked）；
//   - 数值之后是单位 token（字母），归一为小写；lb oz / lb:oz 归一为 lb_oz。
//
// 掩码（如 M-------L、----）是仪表在超量程/出错时对数值段的占位：没有数字。这
// 不是解析错误，而是「这一帧的重量不可用」的合法表达——交由 Status 说明原因。
func parseValue(tok string) (weight float64, unit Unit, decimals int, masked bool, dual bool, err error) {
	i := 0
	// 符号
	if i < len(tok) && (tok[i] == '+' || tok[i] == '-') {
		i++
	}
	// 数值前缀：连续的数字与至多一个小数点
	j := i
	seenDot := false
	seenDigit := false
	for j < len(tok) {
		c := tok[j]
		if c >= '0' && c <= '9' {
			seenDigit = true
			j++
			continue
		}
		if c == '.' && !seenDot {
			seenDot = true
			j++
			continue
		}
		break
	}
	numStr := tok[i:j]
	rest := strings.TrimSpace(tok[j:])

	// 无数字 → 掩码（超量程/出错占位）
	if !seenDigit {
		return 0, "", 0, true, false, nil
	}

	w, perr := strconv.ParseFloat(numStr, 64)
	if perr != nil {
		return 0, "", 0, false, false, fmt.Errorf("不是合法数值")
	}

	dec := 0
	if seenDot {
		dec = len(numStr) - strings.IndexByte(numStr, '.') - 1
	}

	u := normalizeUnit(rest)
	if u == UnitLbOz {
		dual = true
	}
	return w, u, dec, false, dual, nil
}

// normalizeUnit 把单位 token 归一为小写；磅ounces 双单位归一为 lb_oz。
func normalizeUnit(tok string) Unit {
	t := strings.ToLower(strings.TrimSpace(tok))
	switch t {
	case "lb oz", "lb:oz", "lboz", "lb-oz":
		return UnitLbOz
	}
	return Unit(t)
}

// signOf 返回第三段的符号（+1 / -1）。空或非符号字符视为 +1。
func signOf(tok string) int {
	for i := 0; i < len(tok); i++ {
		if tok[i] == '+' {
			return 1
		}
		if tok[i] == '-' {
			return -1
		}
		if (tok[i] >= '0' && tok[i] <= '9') || tok[i] == '.' {
			return 1
		}
	}
	return 1
}

// Format 把一帧读数组成为 ASCII 连续输出字节（含行尾 CR LF）。
//
// 供内置秤模拟与测试使用，使「模拟写进去的」与「驱动解出来的」共用同一套格式
// 规则，不会各写一套而产生理解漂移。decimals 决定数值段对齐宽度（0..5）。
//
// 规范宽度对齐：数值段共 valueWidth 个字符，其中小数点占 1 位，故整数位为
// valueWidth-1-decimals 位，左侧补零。decimals=1 → 5 位整数 + '.' + 1 位，
// 即「00149.0」，与 D39 实例一致。
//
// Weight 是带符号的值（见 Parse），符号决定段首的 + / -。
func Format(r Reading, decimals int) []byte {
	if decimals < 0 {
		decimals = 0
	}
	if decimals > valueWidth-1 {
		decimals = valueWidth - 1
	}
	intW := valueWidth - 1 - decimals

	intPart, fracPart, sign := splitSigned(r.Weight, decimals)

	var b strings.Builder
	// 符号
	if sign < 0 {
		b.WriteByte('-')
	} else {
		b.WriteByte('+')
	}
	// 整数部分（左侧补零到 intW）
	b.WriteString(zeroPad(intPart, intW))
	if decimals > 0 {
		b.WriteByte('.')
		b.WriteString(fracPart)
	}
	// 单位
	b.WriteString(string(r.Unit))

	body := b.String()
	return []byte(fmt.Sprintf("%s,%s,%s\r\n", string(r.Status), string(r.Mode), body))
}

// splitSigned 把带符号浮点按 decimals 位拆成整数串、小数串与符号（+1 / -1）。
//
// 走字符串而不是先数值取整再格式化：连续输出的整数位可能很宽（超大毛重），
// 而 float64 尾数只有 15~16 位有效十进制，先乘 10^n 取整会在大值上丢精度。用
// strconv.FormatFloat 以 -1 精度取最短可往返表示，再手动切分，稳且不丢位。
// 小数串强制补齐到 decimals 位（去尾的补零），保证帧宽固定。
func splitSigned(v float64, decimals int) (intPart, fracPart string, sign int) {
	// 防御负数：Format 会先把 decimals 钳到 [0, valueWidth-1]，但这个函数是
	// 「字符串拆分不丢精度」的核心，不该依赖调用方先钳。负位数按 0 处理，
	// 否则下面 `fracPart[:decimals]` 会切片越界直接 panic。
	if decimals < 0 {
		decimals = 0
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	sign = 1
	if strings.HasPrefix(s, "-") {
		sign = -1
		s = s[1:]
	}
	intPart = s
	fracPart = ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		fracPart = s[i+1:]
	}
	for len(fracPart) < decimals {
		fracPart += "0"
	}
	if len(fracPart) > decimals {
		fracPart = fracPart[:decimals]
	}
	return intPart, fracPart, sign
}

// zeroPad 左侧补零到宽度 w；不足补零，超出原样返回。
func zeroPad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat("0", w-len(s)) + s
}
