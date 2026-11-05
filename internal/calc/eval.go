package calc

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// funcSpec 描述一个内置函数。函数表刻意保持很小：每多一个函数，就多一条
// 需要写测试、需要解释语义、需要在文档里维护的路径。表里的这些覆盖了工业
// 现场绝大多数派生点位（合计、温差、效率、限幅、条件选择）。
type funcSpec struct {
	name string
	min  int
	max  int // -1 表示可变参数
	impl func(args []float64) (float64, error)
	// arity 检查之外的类型约束由 checkCall 处理：目前只有 if 需要
	// 第一个参数是布尔，其余函数的所有参数都必须是数值。
}

var funcs = map[string]funcSpec{
	"abs": {name: "abs", min: 1, max: 1, impl: func(a []float64) (float64, error) {
		return math.Abs(a[0]), nil
	}},
	"min": {name: "min", min: 1, max: -1, impl: func(a []float64) (float64, error) {
		m := a[0]
		for _, v := range a[1:] {
			if v < m {
				m = v
			}
		}
		return m, nil
	}},
	"max": {name: "max", min: 1, max: -1, impl: func(a []float64) (float64, error) {
		m := a[0]
		for _, v := range a[1:] {
			if v > m {
				m = v
			}
		}
		return m, nil
	}},
	"sum": {name: "sum", min: 1, max: -1, impl: func(a []float64) (float64, error) {
		var s float64
		for _, v := range a {
			s += v
		}
		return s, nil
	}},
	"avg": {name: "avg", min: 1, max: -1, impl: func(a []float64) (float64, error) {
		var s float64
		for _, v := range a {
			s += v
		}
		return s / float64(len(a)), nil
	}},
	"round": {name: "round", min: 1, max: 1, impl: func(a []float64) (float64, error) {
		return math.Round(a[0]), nil
	}},
	"floor": {name: "floor", min: 1, max: 1, impl: func(a []float64) (float64, error) {
		return math.Floor(a[0]), nil
	}},
	"ceil": {name: "ceil", min: 1, max: 1, impl: func(a []float64) (float64, error) {
		return math.Ceil(a[0]), nil
	}},
	"sqrt": {name: "sqrt", min: 1, max: 1, impl: func(a []float64) (float64, error) {
		if a[0] < 0 {
			return 0, fmt.Errorf("sqrt of negative number %v", a[0])
		}
		return math.Sqrt(a[0]), nil
	}},
	"pow": {name: "pow", min: 2, max: 2, impl: func(a []float64) (float64, error) {
		v := math.Pow(a[0], a[1])
		if math.IsNaN(v) {
			// 例如 pow(-1, 0.5)：实数域无定义。返回 NaN 会被上层当成
			// 「一个数字」写进趋势图，必须显式失败。
			return 0, fmt.Errorf("pow(%v, %v) has no real result", a[0], a[1])
		}
		return v, nil
	}},
	"clamp": {name: "clamp", min: 3, max: 3, impl: func(a []float64) (float64, error) {
		if a[1] > a[2] {
			return 0, fmt.Errorf("clamp: lower bound %v > upper bound %v", a[1], a[2])
		}
		return math.Min(math.Max(a[0], a[1]), a[2]), nil
	}},
}

func lookupFunc(name string) (funcSpec, error) {
	if name == "if" {
		return funcSpec{name: "if", min: 3, max: 3}, nil
	}
	f, ok := funcs[name]
	if !ok {
		names := make([]string, 0, len(funcs)+1)
		for k := range funcs {
			names = append(names, k)
		}
		names = append(names, "if")
		// 稳定输出（map 顺序随机，报错信息每次都不同会让人以为换了错误）。
		sort.Strings(names)
		return funcSpec{}, fmt.Errorf("unknown function %q (available: %s)", name, strings.Join(names, ", "))
	}
	return f, nil
}

// checkType 做静态类型检查并返回结果类型。它同时承担「这个表达式能不能
// 求值」的全部判断，因此 eval 里不需要再判类型。
func checkType(n node) (Kind, error) {
	switch x := n.(type) {
	case *numLit:
		return KindNumber, nil
	case *boolLit:
		return KindBool, nil
	case *refNode:
		return KindNumber, nil
	case *unaryNode:
		k, err := checkType(x.x)
		if err != nil {
			return "", err
		}
		if x.op == "!" {
			if k != KindBool {
				return "", fmt.Errorf("operator ! needs a bool operand, got %s", k)
			}
			return KindBool, nil
		}
		if k != KindNumber {
			return "", fmt.Errorf("unary %s needs a number operand, got %s", x.op, k)
		}
		return KindNumber, nil
	case *binaryNode:
		lk, err := checkType(x.l)
		if err != nil {
			return "", err
		}
		rk, err := checkType(x.r)
		if err != nil {
			return "", err
		}
		switch x.op {
		case "+", "-", "*", "/", "%", "<", "<=", ">", ">=":
			if lk != KindNumber || rk != KindNumber {
				return "", fmt.Errorf("operator %s needs numbers, got %s and %s", x.op, lk, rk)
			}
		case "&&", "||":
			if lk != KindBool || rk != KindBool {
				return "", fmt.Errorf("operator %s needs bools, got %s and %s", x.op, lk, rk)
			}
		case "==", "!=":
			if lk != rk {
				return "", fmt.Errorf("operator %s compares %s with %s", x.op, lk, rk)
			}
		}
		switch x.op {
		case "<", "<=", ">", ">=", "==", "!=", "&&", "||":
			return KindBool, nil
		}
		return KindNumber, nil
	case *callNode:
		spec, err := lookupFunc(x.name)
		if err != nil {
			return "", err
		}
		if len(x.args) < spec.min || (spec.max >= 0 && len(x.args) > spec.max) {
			want := fmt.Sprintf("%d", spec.min)
			if spec.min != spec.max {
				want = fmt.Sprintf("%d..%d", spec.min, spec.max)
				if spec.max < 0 {
					want = fmt.Sprintf("at least %d", spec.min)
				}
			}
			return "", fmt.Errorf("function %s takes %s argument(s), got %d", x.name, want, len(x.args))
		}
		if x.name == "if" {
			ck, err := checkType(x.args[0])
			if err != nil {
				return "", err
			}
			if ck != KindBool {
				return "", fmt.Errorf("function if: first argument must be bool, got %s", ck)
			}
			ak, err := checkType(x.args[1])
			if err != nil {
				return "", err
			}
			bk, err := checkType(x.args[2])
			if err != nil {
				return "", err
			}
			if ak != bk {
				return "", fmt.Errorf("function if: branches differ (%s vs %s)", ak, bk)
			}
			return ak, nil
		}
		for _, a := range x.args {
			k, err := checkType(a)
			if err != nil {
				return "", err
			}
			if k != KindNumber {
				return "", fmt.Errorf("function %s: arguments must be numbers, got %s", x.name, k)
			}
		}
		return KindNumber, nil
	}
	return "", fmt.Errorf("internal: unknown node %T", n)
}

// collectRefs 按出现顺序收集去重后的引用。
func collectRefs(n node) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(node)
	walk = func(n node) {
		switch x := n.(type) {
		case *refNode:
			if !seen[x.text] {
				seen[x.text] = true
				out = append(out, x.text)
			}
		case *unaryNode:
			walk(x.x)
		case *binaryNode:
			walk(x.l)
			walk(x.r)
		case *callNode:
			for _, a := range x.args {
				walk(a)
			}
		}
	}
	walk(n)
	return out
}

func collectFuncs(n node) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(node)
	walk = func(n node) {
		switch x := n.(type) {
		case *unaryNode:
			walk(x.x)
		case *binaryNode:
			walk(x.l)
			walk(x.r)
		case *callNode:
			if !seen[x.name] {
				seen[x.name] = true
				out = append(out, x.name)
			}
			for _, a := range x.args {
				walk(a)
			}
		}
	}
	walk(n)
	return out
}

// eval 递归求值。布尔值以 1/0 传播。
//
// `&&` 与 `||` **短路**：这不是性能优化，而是正确性的一部分——
// `rate != 0 && total / rate > 10` 在 rate 为 0 时必须得到 false，而不是
// 「除零错误」。现场写这种防护式表达式非常自然。
func eval(n node, r Resolver) (float64, error) {
	switch x := n.(type) {
	case *numLit:
		return x.v, nil
	case *boolLit:
		if x.v {
			return 1, nil
		}
		return 0, nil
	case *refNode:
		v, ok := r(x.text)
		if !ok {
			return 0, fmt.Errorf("reference %q has no value", x.text)
		}
		return v, nil
	case *unaryNode:
		v, err := eval(x.x, r)
		if err != nil {
			return 0, err
		}
		switch x.op {
		case "-":
			return -v, nil
		case "+":
			return v, nil
		case "!":
			if v == 0 {
				return 1, nil
			}
			return 0, nil
		}
		return 0, fmt.Errorf("internal: unknown unary %q", x.op)
	case *binaryNode:
		if x.op == "&&" || x.op == "||" {
			l, err := eval(x.l, r)
			if err != nil {
				return 0, err
			}
			if x.op == "&&" && l == 0 {
				return 0, nil
			}
			if x.op == "||" && l != 0 {
				return 1, nil
			}
			v, err := eval(x.r, r)
			if err != nil {
				return 0, err
			}
			if v != 0 {
				return 1, nil
			}
			return 0, nil
		}
		l, err := eval(x.l, r)
		if err != nil {
			return 0, err
		}
		rv, err := eval(x.r, r)
		if err != nil {
			return 0, err
		}
		v, err := applyBinary(x.op, l, rv)
		if err != nil {
			return 0, err
		}
		return finite(v, x.op)
	case *callNode:
		if x.name == "if" {
			c, err := eval(x.args[0], r)
			if err != nil {
				return 0, err
			}
			if c != 0 {
				return eval(x.args[1], r)
			}
			return eval(x.args[2], r)
		}
		spec, err := lookupFunc(x.name)
		if err != nil {
			return 0, err
		}
		args := make([]float64, len(x.args))
		for i, a := range x.args {
			v, err := eval(a, r)
			if err != nil {
				return 0, err
			}
			args[i] = v
		}
		v, err := spec.impl(args)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", x.name, err)
		}
		return finite(v, x.name)
	}
	return 0, fmt.Errorf("internal: unknown node %T", n)
}

func applyBinary(op string, l, r float64) (float64, error) {
	switch op {
	case "+":
		return l + r, nil
	case "-":
		return l - r, nil
	case "*":
		return l * r, nil
	case "/":
		if r == 0 {
			return 0, fmt.Errorf("division by zero")
		}
		return l / r, nil
	case "%":
		if r == 0 {
			return 0, fmt.Errorf("modulo by zero")
		}
		return math.Mod(l, r), nil
	case "<":
		return b2f(l < r), nil
	case "<=":
		return b2f(l <= r), nil
	case ">":
		return b2f(l > r), nil
	case ">=":
		return b2f(l >= r), nil
	case "==":
		return b2f(l == r), nil
	case "!=":
		return b2f(l != r), nil
	}
	return 0, fmt.Errorf("internal: unknown operator %q", op)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// finite 拒绝 NaN/Inf。
//
// 为什么不做「静默转成 0」或「截断到最大浮点」：这两种处理都会产生一个
// 看起来完全正常的数字，然后被写进趋势图、报表和报警阈值判断。宁可让这个
// 点位显示为坏质量，也不要给出一个能骗过所有人的假数字。
func finite(v float64, what string) (float64, error) {
	if math.IsNaN(v) {
		return 0, fmt.Errorf("%s: result is NaN", what)
	}
	if math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s: result overflows float64", what)
	}
	return v, nil
}
