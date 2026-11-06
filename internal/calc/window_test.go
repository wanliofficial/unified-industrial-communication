package calc

import (
	"strings"
	"testing"
	"time"
)

// windowEnv 是测试用的窗口解析器：按 ref 返回固定摘要，并记录被问到的窗口。
type windowCall struct {
	ref    string
	window time.Duration
}

func windowEnv(stats map[string]WindowStats, calls *[]windowCall) WindowResolver {
	return func(ref string, window time.Duration) (WindowStats, bool) {
		if calls != nil {
			*calls = append(*calls, windowCall{ref, window})
		}
		st, ok := stats[ref]
		return st, ok
	}
}

// 时长字面量的词法决定了 `avg(x, 5m)` 到底是窗口还是「数字 5 乘未知
// 标识符 m」。后者会报错，但报错位置与原因完全不同，必须逐项钉住。
func TestDurationLexing(t *testing.T) {
	ok := []struct {
		src  string
		want time.Duration
	}{
		{"avg(a:x, 1s)", time.Second},
		{"avg(a:x, 30s)", 30 * time.Second},
		{"avg(a:x, 1500ms)", 1500 * time.Millisecond},
		{"avg(a:x, 5m)", 5 * time.Minute},
		{"avg(a:x, 1.5m)", 90 * time.Second},
		{"avg(a:x, 2h)", 2 * time.Hour},
		{"avg(a:x, 1e1s)", 10 * time.Second},
	}
	for _, c := range ok {
		p := mustCompile(t, c.src)
		wins := p.Windows()
		if len(wins) != 1 {
			t.Errorf("%q: windows = %v", c.src, wins)
			continue
		}
		if wins[0].Window != c.want {
			t.Errorf("%q: window = %v, want %v", c.src, wins[0].Window, c.want)
		}
	}

	bad := []struct{ src, want string }{
		// `5mph` 绝不能被静默拆成「5 分钟」+「ph」。
		{"avg(a:x, 5mph)", "invalid duration"},
		// 单位之间必须有数字。
		{"avg(a:x, m)", "unknown identifier"},
		// 大小写敏感：S/M/H 不是单位（写成大写时必须报错，而不是当成秒）。
		{"avg(a:x, 5M)", "invalid duration"},
		// 数字与单位之间不能有空格：`5 m` 是两个 token（报 expected ')'，
		// 因为窗口需要一个时长而不是引用）。
		{"avg(a:x, 5 m)", "expected ')'"},
	}
	for _, c := range bad {
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

func TestCompileWindowErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		// 窗口长度上下界。
		{"avg(a:x, 0s)", "out of range"},
		{"avg(a:x, 500ms)", "out of range"},
		{"avg(a:x, 25h)", "out of range"},
		{"avg(a:x, 24h)", ""}, // 上界本身合法
		{"avg(a:x, 1s)", ""},  // 下界本身合法
		// 第一参数必须是点位引用：`avg(a:x * 2, 5m)` 是常见的误写，
		// 它会让人以为「先算再平均」，实际语义完全不同。
		{"avg(a:x * 2, 5m)", "must be a point reference"},
		{"avg(1, 5m)", "must be a point reference"},
		// 不支持窗口的函数。
		{"abs(a:x, 5m)", "does not take a duration"},
		{"round(a:x, 5m)", "does not take a duration"},
		{"if(true, 5m, 1)", "only allowed as the second argument"},
		// count 只有窗口形式。
		{"count(a:x)", "requires a duration window"},
		{"count(a:x, 5)", "requires a duration window"},
		// 时长不是值。
		{"5m", "only allowed as the second argument"},
		{"5m + 1", "only allowed as the second argument"},
		{"a:x + 2h", "only allowed as the second argument"},
		{"max(1, 2, 5m)", "only allowed as the second argument"},
	}
	for _, c := range cases {
		p, err := Compile(c.src)
		if c.want == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.src, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q: expected error, got kind %s", c.src, p.Kind())
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %q does not contain %q", c.src, err, c.want)
		}
	}
}

func TestWindowEval(t *testing.T) {
	stats := map[string]WindowStats{
		"a:power": {Count: 4, Sum: 40, Min: 5, Max: 15},
	}
	cases := []struct {
		src  string
		want float64
	}{
		{"avg(a:power, 5m)", 10},
		{"min(a:power, 5m)", 5},
		{"max(a:power, 5m)", 15},
		{"sum(a:power, 5m)", 40},
		{"count(a:power, 5m)", 4},
		// 窗口结果参与普通算术。
		{"avg(a:power, 5m) * 2 + 1", 21},
		// 窗口与当前值可以混用：当前值 100 与历史均值 10。
		{"a:power / avg(a:power, 5m)", 10},
	}
	r := env(map[string]float64{"a:power": 100})
	w := windowEnv(stats, nil)
	for _, c := range cases {
		p := mustCompile(t, c.src)
		got, err := p.EvalWindow(r, w)
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q = %v, want %v", c.src, got, c.want)
		}
	}
}

// 三种失败必须可区分：没注入历史 / 历史不可用 / 窗口里没有样本。
// 它们对应的处置动作完全不同，合并成一句「求值失败」等于没有信息。
func TestWindowEvalFailureModes(t *testing.T) {
	p := mustCompile(t, "avg(a:power, 5m)")

	if _, err := p.Eval(nil); err == nil || !strings.Contains(err.Error(), "history is not available") {
		t.Fatalf("nil resolver: %v", err)
	}

	w := func(string, time.Duration) (WindowStats, bool) { return WindowStats{}, false }
	if _, err := p.EvalWindow(nil, w); err == nil || !strings.Contains(err.Error(), "history is unavailable") {
		t.Fatalf("ok=false: %v", err)
	}

	w = func(string, time.Duration) (WindowStats, bool) { return WindowStats{}, true }
	if _, err := p.EvalWindow(nil, w); err == nil || !strings.Contains(err.Error(), "no good samples") {
		t.Fatalf("empty window: %v", err)
	}
}

func TestWindowEvalBool(t *testing.T) {
	p := mustCompile(t, "avg(a:temp, 5m) > 80")
	if p.Kind() != KindBool {
		t.Fatalf("kind = %s", p.Kind())
	}
	w := windowEnv(map[string]WindowStats{"a:temp": {Count: 2, Sum: 200, Min: 90, Max: 110}}, nil)
	ok, err := p.EvalBoolWindow(nil, w)
	if err != nil || !ok {
		t.Fatalf("EvalBoolWindow = %v, %v", ok, err)
	}
	// EvalBool 不带窗口时必须失败（而不是悄悄当成 nil 之后算出一个值）。
	if _, err := p.EvalBool(nil); err == nil {
		t.Fatal("EvalBool on a window expression must fail")
	}
}

// Refs() 不含窗口引用：窗口读的是历史，不是当前值，把它算进依赖会让
// 「A 依赖 B 的过去」被判成环。Windows() 单独给出窗口清单。
func TestWindowRefsAndWindows(t *testing.T) {
	src := "avg(a:x, 5m) + avg(b:y, 5m) + avg(a:x, 5m) + max(c:z, 1h) + d:now"
	p := mustCompile(t, src)

	wantRefs := []string{"d:now"}
	if got := p.Refs(); len(got) != 1 || got[0] != wantRefs[0] {
		t.Fatalf("refs = %v, want %v (window refs must be excluded)", got, wantRefs)
	}
	want := []WindowRef{
		{Name: "avg", Ref: "a:x", Window: 5 * time.Minute},
		{Name: "avg", Ref: "b:y", Window: 5 * time.Minute},
		{Name: "max", Ref: "c:z", Window: time.Hour},
	}
	got := p.Windows()
	if len(got) != len(want) {
		t.Fatalf("windows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("windows = %v, want %v", got, want)
		}
	}
	if !p.UsesWindow() {
		t.Fatal("UsesWindow must be true")
	}
	// 返回副本。
	got[0].Ref = "mutated"
	if p.Windows()[0].Ref != "a:x" {
		t.Fatal("Windows must return a copy")
	}
	// 独立的函数名表包含窗口函数（用于诊断展示）。
	if funcs := p.Funcs(); len(funcs) != 2 || funcs[0] != "avg" || funcs[1] != "max" {
		t.Fatalf("funcs = %v, want [avg max]", funcs)
	}
	if mustCompile(t, "a:x + 1").UsesWindow() {
		t.Fatal("UsesWindow must be false without windows")
	}
}

// 同一个点位取不同窗口是两次独立查询，不能被去重掉。
func TestWindowDedupKeepsDifferentWindows(t *testing.T) {
	p := mustCompile(t, "avg(a:x, 5m) - avg(a:x, 1h)")
	if n := len(p.Windows()); n != 2 {
		t.Fatalf("windows = %v, want 2", p.Windows())
	}
	var calls []windowCall
	w := windowEnv(map[string]WindowStats{
		"a:x": {Count: 1, Sum: 1, Min: 1, Max: 1},
	}, &calls)
	if _, err := p.EvalWindow(nil, w); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if len(calls) != 2 || calls[0].window != 5*time.Minute || calls[1].window != time.Hour {
		t.Fatalf("calls = %v", calls)
	}
}
