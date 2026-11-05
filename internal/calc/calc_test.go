package calc

import (
	"math"
	"strings"
	"testing"
)

// env 是测试用的简单 Resolver。
func env(m map[string]float64) Resolver {
	return func(ref string) (float64, bool) {
		v, ok := m[ref]
		return v, ok
	}
}

func mustCompile(t *testing.T, src string) *Program {
	t.Helper()
	p, err := Compile(src)
	if err != nil {
		t.Fatalf("Compile(%q) = %v", src, err)
	}
	return p
}

func evalNum(t *testing.T, src string, r Resolver) float64 {
	t.Helper()
	p := mustCompile(t, src)
	v, err := p.Eval(r)
	if err != nil {
		t.Fatalf("Eval(%q) = %v", src, err)
	}
	return v
}

// 优先级与结合性：这是表达式引擎唯一「错了也不会报错、只会算错数」的地方，
// 因此必须逐层验证。
func TestPrecedenceAndAssociativity(t *testing.T) {
	cases := []struct {
		src  string
		want float64
	}{
		{"1 + 2 * 3", 7},
		{"(1 + 2) * 3", 9},
		{"10 - 3 - 2", 5},             // 减法左结合
		{"100 / 10 / 2", 5},           // 除法左结合
		{"2 * 3 % 4", 2},              // 同级左结合
		{"-2 + 3", 1},                 // 一元负号
		{"-(-3)", 3},                  //
		{"!true", 0},                  //
		{"!false", 1},                 //
		{"1 + 2 < 4", 1},              // 比较低于加法
		{"1 < 2 == true", 1},          // 相等低于比较
		{"1 + 1 == 2 && 2 > 1", 1},    // && 低于相等
		{"false || true && false", 0}, // && 高于 ||
		{"2.5e2 + 0.5", 250.5},
		{"1e-2 * 100", 1},
	}
	for _, c := range cases {
		if got := evalNum(t, c.src, nil); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%q = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestReferences(t *testing.T) {
	r := env(map[string]float64{
		"line-a:power": 100,
		"line-a:rate":  4,
	})
	if got := evalNum(t, "line-a:power / line-a:rate", r); got != 25 {
		t.Fatalf("got %v", got)
	}
	// 引用可以出现在任意位置，包括函数参数。
	if got := evalNum(t, "max(line-a:power, 200)", r); got != 200 {
		t.Fatalf("got %v", got)
	}
	// 缺失引用必须是错误，而不是 0——0 会让「没数据」看起来像一个真实值。
	p := mustCompile(t, "line-a:missing + 1")
	if _, err := p.Eval(r); err == nil {
		t.Fatal("missing reference must fail")
	} else if !strings.Contains(err.Error(), "line-a:missing") {
		t.Fatalf("error should name the reference: %v", err)
	}
}

// 点号引用是最常见的写法错误（ID 本身允许点号，因此必须拒绝且有提示）。
func TestReferenceSyntaxErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{"line-a.power * 2", "冒号"},
		{"noColon", "unknown identifier"},
		{":point", "invalid reference"},
		{"device:", "invalid reference"},
		{"a:b:c", "invalid reference"},
		{"Line-a:power", "must match"},
		{"a b", "unknown identifier"},
	}
	for _, c := range cases {
		_, err := Compile(c.src)
		if err == nil {
			t.Errorf("%q: expected error", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %q does not contain %q", c.src, err, c.want)
		}
	}
}

func TestCompileRejectsBadInput(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"1 +",
		"(1 + 2",
		"1 + 2)",
		"()",
		"1.2.3",
		"sum(1,)",
		"1 && 2",    // 逻辑运算只接受布尔
		"!1",        // 取反只接受布尔
		"-true",     // 负号只接受数值
		"true == 1", // 类型不同不能比较
		"1 < true",  //
		"unknownFn(1)",
		"abs()",              // 参数太少
		"abs(1, 2)",          // 参数太多
		"pow(2)",             //
		"if(1, 2, 3)",        // 条件必须是布尔
		"if(true, 1, false)", // 分支类型不一致
		"abs(true)",          // 数值函数的参数必须是数值
	}
	for _, src := range cases {
		if p, err := Compile(src); err == nil {
			t.Errorf("Compile(%q) should fail, got %s", src, p.Kind())
		}
	}
}

func TestUnknownFunctionListsAvailable(t *testing.T) {
	_, err := Compile("no_such(1)")
	if err == nil {
		t.Fatal("expected error")
	}
	// 报错必须可执行：列出可用函数，并且顺序稳定。
	if !strings.Contains(err.Error(), "available:") || !strings.Contains(err.Error(), "clamp") {
		t.Fatalf("unhelpful error: %v", err)
	}
	first := err.Error()
	for i := 0; i < 10; i++ {
		_, e := Compile("no_such(1)")
		if e.Error() != first {
			t.Fatalf("error message is not stable:\n%s\n%s", first, e.Error())
		}
	}
}

// 短路求值不是性能优化，而是正确性：防护式写法必须能挡住除零。
func TestShortCircuit(t *testing.T) {
	r := env(map[string]float64{"m:rate": 0, "m:total": 100})
	p := mustCompile(t, "m:rate != 0 && m:total / m:rate > 10")
	if v, err := p.Eval(r); err != nil || v != 0 {
		t.Fatalf("guard failed: v=%v err=%v", v, err)
	}

	// 右侧本来就错（除零）也不能被求值。
	p = mustCompile(t, "true || (1 / 0 > 1)")
	if v, err := p.Eval(nil); err != nil || v != 1 {
		t.Fatalf("|| short-circuit failed: v=%v err=%v", v, err)
	}
	// && 的另一半：左边假，右边根本不该求值。
	p = mustCompile(t, "false && (1 / 0 > 1)")
	if v, err := p.Eval(nil); err != nil || v != 0 {
		t.Fatalf("&& short-circuit failed: v=%v err=%v", v, err)
	}

	// 反过来：没有短路保护时必须报错（证明上面的通过不是因为除零被容忍）。
	p = mustCompile(t, "m:total / m:rate > 10")
	if _, err := p.Eval(r); err == nil {
		t.Fatal("unguarded division by zero must fail")
	}
}

func TestNumericFailures(t *testing.T) {
	cases := []struct{ src, want string }{
		{"1 / 0", "division by zero"},
		{"1 % 0", "modulo by zero"},
		{"sqrt(-1)", "negative"},
		{"pow(-1, 0.5)", "no real result"},
		{"1e308 * 10", "overflows"},
		{"clamp(1, 10, 0)", "lower bound"},
	}
	for _, c := range cases {
		p := mustCompile(t, c.src)
		_, err := p.Eval(nil)
		if err == nil {
			t.Errorf("%q: expected error", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %q does not contain %q", c.src, err, c.want)
		}
	}
}

func TestFunctions(t *testing.T) {
	cases := []struct {
		src  string
		want float64
	}{
		{"abs(-3.5)", 3.5},
		{"min(3, 1, 2)", 1},
		{"max(3, 1, 2)", 3},
		{"sum(1, 2, 3, 4)", 10},
		{"avg(2, 4, 6)", 4},
		{"round(2.5)", 3},
		{"floor(2.9)", 2},
		{"ceil(2.1)", 3},
		{"sqrt(9)", 3},
		{"pow(2, 10)", 1024},
		{"clamp(15, 0, 10)", 10},
		{"clamp(-5, 0, 10)", 0},
		{"if(1 > 0, 10, 20)", 10},
		{"if(1 < 0, 10, 20)", 20},
		// 嵌套与布尔结果。
		{"if(true && !false, sum(1, 2), 0)", 3},
		{"max(if(true, 5, 6), 4)", 5},
	}
	for _, c := range cases {
		if got := evalNum(t, c.src, nil); got != c.want {
			t.Errorf("%q = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestBooleanResult(t *testing.T) {
	p := mustCompile(t, "x:temp > 80")
	if p.Kind() != KindBool {
		t.Fatalf("kind = %s", p.Kind())
	}
	r := env(map[string]float64{"x:temp": 90})
	ok, err := p.EvalBool(r)
	if err != nil || !ok {
		t.Fatalf("EvalBool = %v, %v", ok, err)
	}

	// 非布尔表达式调用 EvalBool 必须有明确错误（防止调用方用错）。
	p = mustCompile(t, "x:temp + 1")
	if _, err := p.EvalBool(r); err == nil {
		t.Fatal("EvalBool on a number expression must fail")
	}
	if p.Kind() != KindNumber {
		t.Fatalf("kind = %s", p.Kind())
	}

	// if 的两个分支都是布尔时，整体是布尔。
	p = mustCompile(t, "if(x:temp > 0, true, false)")
	if p.Kind() != KindBool {
		t.Fatalf("if-bool kind = %s", p.Kind())
	}
}

// Refs 的顺序必须稳定：驱动按它做依赖排序，map 随机序会让同一份配置
// 在两次启动里得到不同的拓扑顺序。
func TestRefsDedupAndOrder(t *testing.T) {
	p := mustCompile(t, "a:x + b:y * a:x + max(c:z, b:y)")
	want := []string{"a:x", "b:y", "c:z"}
	got := p.Refs()
	if len(got) != len(want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("refs = %v, want %v", got, want)
		}
	}
	// 返回的是副本：调用方改动不得影响下次读取。
	got[0] = "mutated"
	if p.Refs()[0] != "a:x" {
		t.Fatal("Refs must return a copy")
	}

	if n := len(p.Funcs()); n != 1 || p.Funcs()[0] != "max" {
		t.Fatalf("funcs = %v", p.Funcs())
	}

	empty := mustCompile(t, "1 + 2")
	if len(empty.Refs()) != 0 || len(empty.Funcs()) != 0 {
		t.Fatalf("literal refs=%v funcs=%v", empty.Refs(), empty.Funcs())
	}
}

func TestFuncsDedup(t *testing.T) {
	p := mustCompile(t, "abs(-1) + abs(2) + abs(3)")
	if got := p.Funcs(); len(got) != 1 || got[0] != "abs" {
		t.Fatalf("funcs = %v", got)
	}
}

func TestStringReturnsSource(t *testing.T) {
	src := "a:x * 2"
	if got := mustCompile(t, src).String(); got != src {
		t.Fatalf("String = %q", got)
	}
}

// 布尔值在算术语境里是 1/0（`running * 100` 是现场最常见的写法）。
func TestBoolAsNumber(t *testing.T) {
	r := env(map[string]float64{"m:running": 1})
	if got := evalNum(t, "m:running * 100", r); got != 100 {
		t.Fatalf("got %v", got)
	}
	// 但布尔**字面量**不参与算术：`true + true` 是写错了，不是 2。
	// 引用没有静态类型信息（都是 number），字面量有——既然能查出来就查。
	if _, err := Compile("true + true"); err == nil {
		t.Fatal("bool literals must not be used as numbers")
	}
}

// 表达式必须可并发求值：Program 编译后是只读的。
func TestConcurrentEval(t *testing.T) {
	p := mustCompile(t, "a:x * 2 + 1")
	r := env(map[string]float64{"a:x": 10})
	done := make(chan float64, 16)
	for i := 0; i < cap(done); i++ {
		go func() {
			v, err := p.Eval(r)
			if err != nil {
				done <- -1
				return
			}
			done <- v
		}()
	}
	for i := 0; i < cap(done); i++ {
		if v := <-done; v != 21 {
			t.Fatalf("concurrent eval = %v", v)
		}
	}
}
