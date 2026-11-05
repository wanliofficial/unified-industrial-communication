// Package calc 是计算点位的表达式引擎：把一段文本表达式编译成可反复求值的
// 程序，用于「由其他点位算出新点位」——能耗合计、温差、效率、简单判定。
//
// 设计约束（按重要性排序）：
//
//  1. **绝不用 eval**。没有第三方依赖，没有反射，没有动态代码生成。表达式
//     来自配置文件，而配置文件会经 REST 热更新、会被备份恢复、会被复制到
//     别的现场——它必须被当作不可信输入处理。一个表达式引擎能读写文件或
//     调用系统命令，等于把网关变成一个远程 shell。
//  2. **编译期拒绝，而不是求值期才炸**。括号不配对、类型不兼容（`1 && 2`）、
//     参数个数错、未知函数，全部在 Compile 阶段报错。现场最糟糕的情况是
//     「配置通过、跑了两周才在某条分支上失败」。
//  3. **求值必须确定且无副作用**。同一组输入永远得到同一结果，不读时钟、
//     不读随机数、不访问网络。这样它可以在测试里被穷举，也能被从站、报警、
//     历史三条链路安全复用。
//  4. **数值异常就是错误**。除零、负数开方、溢出成 Inf。工业场景里把 Inf
//     当正常值传下去，会让报表出现「产量无穷大」这种没人敢信的结论。
//
// 语法（优先级从低到高）：
//
//	or         := and ('||' and)*
//	and        := equality ('&&' equality)*
//	equality   := relational (('==' | '!=') relational)*
//	relational := additive (('<' | '<=' | '>' | '>=') additive)*
//	additive   := multiplicative (('+' | '-') multiplicative)*
//	multiplicative := unary (('*' | '/' | '%') unary)*
//	unary      := ('-' | '+' | '!') unary | primary
//	primary    := number | ref | call | '(' or ')' | 'true' | 'false'
//
// 引用写法是 `device:point`（例：`line-a:power`）。**不是** `device.point`：
// 设备 ID 与点位 ID 都允许包含点号（`[a-z0-9._-]`），用点号分隔会产生
// "a.b.c" 到底指 `a` 的 `b.c` 还是 `a.b` 的 `c` 的歧义；冒号不在 ID 的
// 合法字符集里，因此永远只有一个解释。
//
// 布尔值在内部用 1/0 表示：工业表达式里「位状态参与算术」非常常见
// （例如 `running * 100`），强行区分两套值类型只会让使用者到处写转换。
// 但**类型检查仍然是静态的**：逻辑运算符只接受布尔操作数，比较运算符只
// 接受数值操作数。
package calc

import (
	"fmt"
	"strings"
)

// Kind 是表达式的静态结果类型。
type Kind string

const (
	// KindNumber 表示结果是数值。
	KindNumber Kind = "number"
	// KindBool 表示结果是布尔（比较、逻辑运算、if 的布尔分支）。
	KindBool Kind = "bool"
)

// Program 是编译后的表达式。它是只读的，可被并发求值。
type Program struct {
	src  string
	root node
	refs []string
	kind Kind
	// used 是表达式里出现过的函数名，供诊断与文档展示。
	used []string
}

// String 返回原始表达式文本。
func (p *Program) String() string { return p.src }

// Kind 返回静态结果类型。
func (p *Program) Kind() Kind { return p.kind }

// Refs 返回按出现顺序去重的点位引用（`device:point`）。
//
// 它的用途不止是文档：驱动按它做依赖排序，配置校验按它做「引用是否存在」
// 与「有没有环」，质量聚合按它决定输出质量码。因此顺序必须稳定——map
// 遍历顺序不稳定会让同一份配置在两次启动里产生不同的依赖顺序。
func (p *Program) Refs() []string { return append([]string(nil), p.refs...) }

// Funcs 返回表达式用到的函数名（去重、按出现顺序）。
func (p *Program) Funcs() []string { return append([]string(nil), p.used...) }

// Resolver 把引用解析成一个数值。found 为 false 表示点位不存在或还没有值。
//
// 布尔值以 1/0 返回；调用方负责把 model.Value 转换过来。
type Resolver func(ref string) (v float64, found bool)

// Compile 解析并校验表达式。
func Compile(src string) (*Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("calc: empty expression")
	}
	p := &parser{toks: lex(src)}
	root, err := p.parseExpr()
	if err != nil {
		return nil, fmt.Errorf("calc: %s: %w", src, err)
	}
	if tok := p.peek(); tok.kind != tokEOF {
		return nil, fmt.Errorf("calc: %s: unexpected %q at offset %d", src, tok.text, tok.pos)
	}
	kind, err := checkType(root)
	if err != nil {
		return nil, fmt.Errorf("calc: %s: %w", src, err)
	}
	prog := &Program{src: src, root: root, kind: kind}
	prog.refs = collectRefs(root)
	prog.used = collectFuncs(root)
	return prog, nil
}

// Eval 求值。只有数值异常（除零、负数开方等）会返回错误；引用缺失由
// Resolver 报告，调用方通常把它转成「质量 bad」而不是整体失败——
// 一个点位没数据不该让整台设备的其他计算点全部失败。
func (p *Program) Eval(r Resolver) (float64, error) {
	return eval(p.root, r)
}

// EvalBool 求值并转成布尔。非布尔表达式返回错误（配置期已拦，这里是
// 防调用方用错）。
func (p *Program) EvalBool(r Resolver) (bool, error) {
	if p.kind != KindBool {
		return false, fmt.Errorf("calc: %s: expression is %s, not bool", p.src, p.kind)
	}
	v, err := p.Eval(r)
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

type node interface{ isNode() }

type numLit struct{ v float64 }

type boolLit struct{ v bool }

// refNode 是一个点位引用，text 形如 `device:point`。
type refNode struct{ text string }

type unaryNode struct {
	op string
	x  node
}

type binaryNode struct {
	op   string
	l, r node
}

type callNode struct {
	name string
	args []node
	pos  int
}

func (*numLit) isNode()     {}
func (*boolLit) isNode()    {}
func (*refNode) isNode()    {}
func (*unaryNode) isNode()  {}
func (*binaryNode) isNode() {}
func (*callNode) isNode()   {}
