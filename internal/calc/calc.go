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
// 时间窗口聚合（`avg(line-a:power, 5m)`）是 call 的一种特殊形式：第二个
// 参数是时长字面量（ms/s/m/h）时，聚合对象从「当前值」变成「历史样本」。
// 窗口长度限定 1s..24h，且要求 `storage.history.enabled = true`；只统计
// 质量为 good 的数值样本。
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
	"time"
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
	src     string
	root    node
	refs    []string
	windows []WindowRef
	kind    Kind
	// used 是表达式里出现过的函数名，供诊断与文档展示。
	used []string
}

// WindowRef 是表达式里出现的一个时间窗口聚合："对 ref 最近 Window 内的样本
// 做 name 聚合"。
//
// 它单独成表而不是并进 Refs()：窗口读取的是**历史**，不是当前值。把它当
// 依赖会让「A = avg(B, 5m)、B = A + 1」被判成环，而它们实际上可以正常
// 求值——A 读的是 B 的过去，不要求 B 的当前值。
type WindowRef struct {
	Name   string
	Ref    string
	Window time.Duration
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

// Windows 返回表达式里的时间窗口聚合（按出现顺序去重）。
//
// 配置层用它在加载期做三件事：检查引用的点位存在、确认历史已开启、
// 提示窗口长度是否超过保留期。把这三件事推到运行期，表现就是「报警永远
// 不响」或「趋势算出来全是未知」，而没有任何一条明确的错误。
func (p *Program) Windows() []WindowRef { return append([]WindowRef(nil), p.windows...) }

// UsesWindow 报告表达式是否引用了历史窗口。
func (p *Program) UsesWindow() bool { return len(p.windows) > 0 }

// MinWindow 与 MaxWindow 限定历史窗口的长度。
//
// 下界 1s：再短的窗口在「只记变化 + 心跳」的历史库里通常只有 0~1 个样本，
// 平均值没有统计意义；上界 24h：更长的窗口意味着每次求值都要扫几乎整个
// 保留期，而保留期默认 7 天——那已经不是「聚合」，是报表。
const (
	MinWindow = time.Second
	MaxWindow = 24 * time.Hour
)

// Resolver 把引用解析成一个数值。found 为 false 表示点位不存在或还没有值。
//
// 布尔值以 1/0 返回；调用方负责把 model.Value 转换过来。
type Resolver func(ref string) (v float64, found bool)

// WindowStats 是一个时间窗口内某个点位的样本摘要。
//
// 为什么传摘要而不是传 []float64：一个 5 分钟窗口在一台高频设备上可能是几万
// 条样本，把它们全部搬进内存再聚合，代价与窗口长度成正比；而聚合结果只有
// 四个数。历史库本来就是顺序扫描，在扫描过程中累加是最便宜的做法。
//
// 只统计**质量为 good 的数值样本**：把 stale（断线时保留的最后值）算进平均，
// 等于让一段没有数据的时间也参与计算，这是最隐蔽的一类假数据。
type WindowStats struct {
	Count int
	Sum   float64
	Min   float64
	Max   float64
}

// WindowResolver 返回 ref 在最近 window 内的样本摘要。
//
// ok=false 表示**无法提供**窗口数据（历史未开启、查询失败），与「窗口内
// 没有可用样本」（ok=true、Count=0）是两件不同的事：前者是配置/系统问题，
// 后者是数据还没积累够，运维要采取的动作完全不同。
type WindowResolver func(ref string, window time.Duration) (WindowStats, bool)

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
	prog.windows = collectWindows(root)
	prog.used = collectFuncs(root)
	return prog, nil
}

// Eval 求值。只有数值异常（除零、负数开方等）会返回错误；引用缺失由
// Resolver 报告，调用方通常把它转成「质量 bad」而不是整体失败——
// 一个点位没数据不该让整台设备的其他计算点全部失败。
//
// 表达式里没有时间窗口时等价于 EvalWindow(r, nil)。
func (p *Program) Eval(r Resolver) (float64, error) {
	return p.EvalWindow(r, nil)
}

// EvalWindow 求值，并允许表达式使用历史窗口聚合。
//
// w 为 nil 时窗口聚合会报错（而不是悄悄返回 0）：调用方没有注入历史却写
// 了 `avg(x, 5m)`，必须让它变成一条可读的错误，而不是一个看起来正常的值。
func (p *Program) EvalWindow(r Resolver, w WindowResolver) (float64, error) {
	return eval(p.root, r, w)
}

// EvalBool 求值并转成布尔。非布尔表达式返回错误（配置期已拦，这里是
// 防调用方用错）。
func (p *Program) EvalBool(r Resolver) (bool, error) {
	return p.EvalBoolWindow(r, nil)
}

// EvalBoolWindow 是带历史窗口的布尔求值。
func (p *Program) EvalBoolWindow(r Resolver, w WindowResolver) (bool, error) {
	if p.kind != KindBool {
		return false, fmt.Errorf("calc: %s: expression is %s, not bool", p.src, p.kind)
	}
	v, err := p.EvalWindow(r, w)
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

// durNode 是时长字面量。它只允许出现在窗口聚合的第二个参数位置；
// 出现在别处会在 checkType / eval 里得到明确报错，而不是被当成数字。
type durNode struct{ d time.Duration }

// windowNode 是 `avg(ref, 5m)` 这样的历史窗口聚合。
//
// 与 callNode 分开而不是塞一个 field：窗口求值需要**另一个**解析器
// （历史库），它和普通函数（纯内存计算）在失败语义上完全不同——
// 普通函数失败是表达式错，窗口失败更可能是「历史没开」或「样本不够」。
type windowNode struct {
	name   string
	ref    string
	window time.Duration
	pos    int
}

func (*numLit) isNode()     {}
func (*boolLit) isNode()    {}
func (*refNode) isNode()    {}
func (*unaryNode) isNode()  {}
func (*binaryNode) isNode() {}
func (*callNode) isNode()   {}
func (*durNode) isNode()    {}
func (*windowNode) isNode() {}
