package calc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
)

type token struct {
	kind tokKind
	text string
	num  float64
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
	*out = append(*out, token{kind: tokNumber, text: text, num: v, pos: start})
	return i
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
	if _, err := lookupFunc(name.text); err != nil {
		return nil, fmt.Errorf("offset %d: %w", name.pos, err)
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
	return &callNode{name: name.text, args: args, pos: name.pos}, nil
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
