package calc

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// idRe 与 model 包的点位/设备 ID 规则保持一致（lowercase + 数字 + . _ -）。
// 复制而不是引 model：表达式引擎要保持零内部依赖。
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// token 种类。
type tokKind int

const (
	tokEOF tokKind = iota
	tokNumber
	tokIdent  // 引用（device:point）或函数名，由 parser 按上下文区分
	tokOp     // 运算符
	tokLParen // (
	tokRParen // )
	tokComma  // ,
	// tokDuration 是时长字面量：500ms / 30s / 5m / 2h。
	//
	// 单独一种 token 而不是「数字 + 后缀标识符」：时长只能出现在窗口聚合
	// 的第二个参数位置，词法层直接识别出来，才能让 `avg(x, 5m)` 与
	// `avg(x, 5 * m)` 产生两种明确的结论（前者是窗口，后者是未知标识符）。
	tokDuration
)

type token struct {
	kind tokKind
	text string
	num  float64
	// dur 只在 tokDuration 时有意义。
	dur time.Duration
	// badNum 标记「看起来像数字但解析失败」的 token（如 `1.2.3`）。
	// 如果直接退回 0，表达式 `1.2.3 + x` 会算出一个毫无意义的和，
	// 而配置校验会一路绿灯——这类静默降级必须在编译期暴露。
	badNum bool
	pos    int // 字节偏移，用于错误信息定位
}

// isRefChar 判定「引用内部」允许出现的字符。它只在已经确定这是一个
// 引用/标识符时使用——绝不能拿它判断一个字符能否**开始**一个 token：
// `-` 与 `.` 都是引用内部字符，如果把它们当成起始字符，`10 - 3` 会被
// 切成 `10` `-`(标识符) `3`，`sqrt(-1)` 会被切成一个叫 `-1` 的标识符。
//
// 大写字母在词法层放行，是为了让 `Temp:value` 能走到 validateRef，
// 报出「ID 必须匹配 ^[a-z0-9][a-z0-9._-]{0,63}$」，而不是裂成几个
// token 后给一句看不懂的 unexpected。
func isRefChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-'
}

// isIdentStart 是标识符（引用/函数名）的起始字符。
func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// scanWord 扫描从 i 开始的最大「引用字符 + 冒号」连续段。
func scanWord(src string, i int) int {
	for i < len(src) && (isRefChar(src[i]) || src[i] == ':') {
		i++
	}
	return i
}

// lex 把表达式切成 token。它不做语义判断，只负责「切对」。
//
// 切分的核心歧义只有一个：设备 ID 与点位 ID 允许包含 `-` 与 `.`，
// 而它们同时也是运算符/小数点。消歧规则是**看这一段里有没有冒号**：
//
//   - 有冒号 → 一定是点位引用，整段吃下去（`line-a:power-1` 是引用
//     `line-a` 的 `power-1`，不是减法）。
//   - 没有冒号 → 不可能是引用。以字母开头的是函数名/true/false；
//     以数字开头的是数字，按严格的数字规则重新切，于 `10-3` 得到
//     `10 - 3`、`1.2.3` 得到 `1.2` 与一个非法字符。
//
// 由此产生一条使用约定：**减号两边要留空格**，否则 `a:1-2` 会被理解成
// 引用 `a:1-2`。这是 ID 允许 `-` 的必然代价，且失败方式是可读的
// （「引用不存在」），不会算出错误结果。
func lex(src string) []token {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isIdentStart(c) || c == ':' || isDigit(c) || c == '.' && i+1 < len(src) && isDigit(src[i+1]):
			start := i
			word := src[start:scanWord(src, start)]
			switch {
			case strings.Contains(word, ":"):
				// 引用：整段交给 validateRef 做格式校验。
				out = append(out, token{kind: tokIdent, text: word, pos: start})
				i = start + len(word)
			case isIdentStart(c):
				// 函数名 / true / false（也可能是不带冒号的错误引用，
				// 由 parser 报「unknown identifier」并给出格式提示）。
				out = append(out, token{kind: tokIdent, text: word, pos: start})
				i = start + len(word)
			default:
				// 以数字（或 `.5`）开头且不含冒号：按严格数字规则切。
				i = lexNumber(src, start, &out)
			}
		case c == '(':
			out = append(out, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			out = append(out, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == ',':
			out = append(out, token{kind: tokComma, text: ",", pos: i})
			i++
		default:
			// 双字符运算符优先。
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "==", "!=", "&&", "||":
				out = append(out, token{kind: tokOp, text: two, pos: i})
				i += 2
				continue
			}
			switch c {
			case '+', '-', '*', '/', '%', '<', '>', '!':
				out = append(out, token{kind: tokOp, text: string(c), pos: i})
				i++
			default:
				// 非法字符：造一个 op token，让 parser 报「unexpected」，
				// 避免 lexer 自己吞掉后产生更难懂的错位报错。
				out = append(out, token{kind: tokOp, text: string(c), pos: i})
				i++
			}
		}
	}
	out = append(out, token{kind: tokEOF, text: "", pos: len(src)})
	return out
}

// lexNumber 从 start 开始按「十进制 + 可选小数 + 可选指数」切一个数字，
// 返回下一个扫描位置。
func lexNumber(src string, start int, out *[]token) int {
	i := start
	digits := 0
	for i < len(src) && isDigit(src[i]) {
		i++
		digits++
	}
	if i < len(src) && src[i] == '.' {
		i++
		for i < len(src) && isDigit(src[i]) {
			i++
			digits++
		}
	}
	if digits == 0 {
		// 理论上到不了这里（调用前已确认 `.` 后跟数字）；保守返回一个
		// 非法 token，避免死循环。
		*out = append(*out, token{kind: tokOp, text: string(src[start]), pos: start})
		return start + 1
	}
	// 科学计数法：1e3 / 2.5E-4。只在确实跟着数字时才吃 e，
	// 否则 `2e` 会被误判成一个数字。
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		j := i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			i = j
			for i < len(src) && isDigit(src[i]) {
				i++
			}
		}
	}
	text := src[start:i]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// 语法错由 parser 统一报告（这里保留原文 + 标记，让报错可读）。
		*out = append(*out, token{kind: tokNumber, text: text, badNum: true, pos: start})
		return i
	}
	// 时长后缀。只认单后缀（ms/s/m/h），且后缀后面不能再跟标识符字符——
	// 否则 `5mph` 会被切成「5 分钟」+「ph」，那种静默拆分比直接报错危险
	// 得多。窗口长度有上下界（1s..24h），由 parser 在构造窗口节点时校验：
	// 词法层只管「这是一个时长」，不管它是不是合理的窗口。
	if unit, w := durationUnit(src, i); w > 0 {
		end := i + w
		if end < len(src) && (isRefChar(src[end]) || src[end] == ':') {
			*out = append(*out, token{kind: tokDuration, text: src[start:scanWord(src, start)], badNum: true, pos: start})
			return scanWord(src, start)
		}
		secs := v * float64(unit)
		if math.IsInf(secs, 0) || math.IsNaN(secs) || secs > float64(math.MaxInt64) {
			*out = append(*out, token{kind: tokDuration, text: src[start:end], badNum: true, pos: start})
			return end
		}
		*out = append(*out, token{kind: tokDuration, text: src[start:end],
			dur: time.Duration(secs), pos: start})
		return end
	}
	// 大写单位（5S / 5M / 5H）明确报「非法时长」，而不是让它裂成
	// 「数字 5」+「未知标识符 M」——后者会让人以为问题出在标识符上。
	if w := upperDurationSuffix(src, i); w > 0 {
		*out = append(*out, token{kind: tokDuration, text: src[start : i+w], badNum: true, pos: start})
		return i + w
	}
	*out = append(*out, token{kind: tokNumber, text: text, num: v, pos: start})
	return i
}

// durationUnit 报告 i 处的时长后缀及其字节宽度。0 表示没有后缀。
//
// "ms" 必须先于 "m" 判断，否则 `500ms` 会被读成 500 分钟 + 一个多余的 s。
func durationUnit(src string, i int) (unit time.Duration, width int) {
	if i >= len(src) {
		return 0, 0
	}
	if strings.HasPrefix(src[i:], "ms") {
		return time.Millisecond, 2
	}
	switch src[i] {
	case 's':
		return time.Second, 1
	case 'm':
		return time.Minute, 1
	case 'h':
		return time.Hour, 1
	}
	return 0, 0
}

// upperDurationSuffix 识别大写单位，只为了给出准确错误。
func upperDurationSuffix(src string, i int) int {
	if i >= len(src) {
		return 0
	}
	if strings.HasPrefix(src[i:], "MS") {
		return 2
	}
	switch src[i] {
	case 'S', 'M', 'H':
		return 1
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// parser 是递归下降解析器。
type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) acceptOp(ops ...string) (string, bool) {
	t := p.peek()
	if t.kind != tokOp {
		return "", false
	}
	for _, op := range ops {
		if t.text == op {
			p.next()
			return op, true
		}
	}
	return "", false
}

func (p *parser) expect(kind tokKind, want string) error {
	t := p.peek()
	if t.kind != kind {
		return fmt.Errorf("expected %s at offset %d, got %q", want, t.pos, t.text)
	}
	p.next()
	return nil
}

func (p *parser) parseExpr() (node, error) { return p.parseOr() }

func (p *parser) parseOr() (node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.acceptOp("||")
		if !ok {
			return l, nil
		}
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseAnd() (node, error) {
	l, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.acceptOp("&&")
		if !ok {
			return l, nil
		}
		r, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseEquality() (node, error) {
	l, err := p.parseRelational()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.acceptOp("==", "!=")
		if !ok {
			return l, nil
		}
		r, err := p.parseRelational()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseRelational() (node, error) {
	l, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.acceptOp("<", "<=", ">", ">=")
		if !ok {
			return l, nil
		}
		r, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseAdditive() (node, error) {
	l, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.acceptOp("+", "-")
		if !ok {
			return l, nil
		}
		r, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseMultiplicative() (node, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.acceptOp("*", "/", "%")
		if !ok {
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseUnary() (node, error) {
	if op, ok := p.acceptOp("-", "+", "!"); ok {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: op, x: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	t := p.peek()
	switch t.kind {
	case tokNumber:
		p.next()
		if t.badNum {
			return nil, fmt.Errorf("invalid number %q at offset %d", t.text, t.pos)
		}
		return &numLit{v: t.num}, nil
	case tokDuration:
		p.next()
		if t.badNum {
			return nil, fmt.Errorf("invalid duration %q at offset %d (use one of ms, s, m, h; e.g. 30s or 5m)",
				t.text, t.pos)
		}
		return &durNode{d: t.dur}, nil
	case tokLParen:
		p.next()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tokRParen, "')'"); err != nil {
			return nil, err
		}
		return inner, nil
	case tokIdent:
		p.next()
		// 跟着 '(' 是函数调用，否则是点位引用。
		if p.peek().kind == tokLParen {
			return p.parseCall(t)
		}
		switch t.text {
		case "true":
			return &boolLit{v: true}, nil
		case "false":
			return &boolLit{v: false}, nil
		}
		if err := validateRef(t); err != nil {
			return nil, err
		}
		return &refNode{text: t.text}, nil
	case tokEOF:
		return nil, fmt.Errorf("unexpected end of expression")
	}
	return nil, fmt.Errorf("unexpected %q at offset %d", t.text, t.pos)
}

func (p *parser) parseCall(name token) (node, error) {
	// 先判断函数是否存在，但窗口聚合里的 `count` 不是普通函数（它只有
	// 「窗口内计数」这一种形式），因此对它放行到最后再判。
	if _, err := lookupFunc(name.text); err != nil {
		if !isWindowFunc(name.text) {
			return nil, fmt.Errorf("offset %d: %w", name.pos, err)
		}
	}
	// 吃掉 '('
	p.next()
	var args []node
	if p.peek().kind != tokRParen {
		for {
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if p.peek().kind == tokComma {
				p.next()
				continue
			}
			break
		}
	}
	if err := p.expect(tokRParen, "')'"); err != nil {
		return nil, err
	}
	// 两个参数且第二个是时长字面量 → 时间窗口聚合。
	//
	// 之所以用「第二个参数是不是时长」而不是给窗口函数改名（如 avgw）：
	// 现场的表达式是手写的，`avg(line-a:power, 5m)` 读起来就是它算的东西。
	// 歧义也不存在——`avg` 的普通形式接受任意多个数值参数，而时长不是值。
	if len(args) == 2 {
		if d, ok := args[1].(*durNode); ok {
			return buildWindow(name, args[0], d)
		}
	}
	if name.text == "count" {
		return nil, fmt.Errorf("offset %d: function count requires a duration window, e.g. count(%s, 5m)",
			name.pos, "device:point")
	}
	return &callNode{name: name.text, args: args, pos: name.pos}, nil
}

// windowFuncs 是支持「时间窗口聚合」的函数名集合。
//
// 与普通聚合同名是刻意的：avg/min/max/sum 在不带窗口时是对**当前值**的
// 聚合，带上窗口时是对**历史样本**的聚合，两者在语义上是同一件事的不同
// 数据源。count 只在窗口形式下有定义（当前值的个数恒为 1，没有意义）。
var windowFuncs = map[string]bool{
	"avg": true, "min": true, "max": true, "sum": true, "count": true,
}

func isWindowFunc(name string) bool { return windowFuncs[name] }

// buildWindow 把 `name(ref, duration)` 变成窗口节点，并在这里做完所有
// 静态校验：函数名、第一参数必须是引用、窗口长度区间。
func buildWindow(name token, first node, d *durNode) (node, error) {
	if !isWindowFunc(name.text) {
		return nil, fmt.Errorf("offset %d: function %s does not take a duration argument", name.pos, name.text)
	}
	ref, ok := first.(*refNode)
	if !ok {
		return nil, fmt.Errorf("offset %d: function %s: first argument of a window aggregate must be a point reference (device:point)",
			name.pos, name.text)
	}
	if d.d < MinWindow || d.d > MaxWindow {
		return nil, fmt.Errorf("offset %d: function %s: window %s out of range %s..%s",
			name.pos, name.text, d.d, MinWindow, MaxWindow)
	}
	return &windowNode{name: name.text, ref: ref.text, window: d.d, pos: name.pos}, nil
}

// validateRef 在编译期检查引用写法。绝大多数写错都发生在这一步：
// 用了点号分隔、忘了设备前缀、引用了不合法的 ID。
func validateRef(t token) error {
	text := t.text
	if !strings.Contains(text, ":") {
		hint := ""
		if strings.Contains(text, ".") {
			hint = "（引用格式是 device:point，用冒号而不是点号）"
		}
		return fmt.Errorf("unknown identifier %q at offset %d%s", text, t.pos, hint)
	}
	parts := strings.SplitN(text, ":", 2)
	dev, pt := parts[0], parts[1]
	if dev == "" || pt == "" {
		return fmt.Errorf("invalid reference %q at offset %d: expected device:point", text, t.pos)
	}
	for _, seg := range []string{dev, pt} {
		// 与 model 的 idRe 保持一致。放在这里而不是引 model 包，是为了让
		// 表达式引擎保持零内部依赖——它要能被从站、报警、历史三条链路复用，
		// 依赖越少越不容易被拖成一个巨大的包。
		if !idRe.MatchString(seg) {
			return fmt.Errorf("invalid reference %q at offset %d: %q must match %s",
				text, t.pos, seg, idRe.String())
		}
	}
	return nil
}
