package alarm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// fakeClock 让「连续满足 N 秒」这类测试不需要真的睡 N 秒。
//
// 时间只通过 Now() 注入，引擎内部从不直接调 time.Now()，因此评估结果完全
// 可重放——这也是要让引擎支持 Now 参数的原因，而不是为了测试方便。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// source 是 Lookup 的测试替身：一个手控的点位值表。
type source struct {
	mu      sync.RWMutex
	samples map[string]model.Sample
}

func newSource() *source { return &source{samples: map[string]model.Sample{}} }

func key(dev, pt string) string { return dev + "/" + pt }

func (s *source) set(dev, pt string, v float64, q model.Quality) {
	s.mu.Lock()
	s.samples[key(dev, pt)] = model.Sample{
		DeviceID: dev, PointID: pt,
		Value: model.NumValue(model.TypeFloat64, v), Quality: q,
	}
	s.mu.Unlock()
}

func (s *source) setText(dev, pt, v string, q model.Quality) {
	s.mu.Lock()
	s.samples[key(dev, pt)] = model.Sample{
		DeviceID: dev, PointID: pt,
		Value: model.TextValue(model.TypeString, v), Quality: q,
	}
	s.mu.Unlock()
}

func (s *source) drop(dev, pt string) {
	s.mu.Lock()
	delete(s.samples, key(dev, pt))
	s.mu.Unlock()
}

func (s *source) lookup(dev, pt string) (model.Sample, bool) {
	s.mu.RLock()
	sm, ok := s.samples[key(dev, pt)]
	s.mu.RUnlock()
	return sm, ok
}

func ptr(v float64) *float64 { return &v }

func ms(n int) model.Duration { return model.Duration(time.Duration(n) * time.Millisecond) }

// newTestEngine 构造一个用假时钟驱动的引擎。
func newTestEngine(t *testing.T, rules []Rule, look Lookup) (*Engine, *fakeClock, *source) {
	t.Helper()
	src := newSource()
	if look == nil {
		look = src.lookup
	}
	clk := newFakeClock()
	e, err := New(Options{
		Rules:  rules,
		Lookup: look,
		Log:    slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError})),
		Now:    clk.Now,
		Sweep:  time.Hour, // 测试里显式调用 Sweep，不让后台循环掺和
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, clk, src
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// stateOf 是断言用的小工具。
func stateOf(t *testing.T, e *Engine, id string) Status {
	t.Helper()
	st, ok := e.Get(id)
	if !ok {
		t.Fatalf("alarm %q not found", id)
	}
	return st
}

func mustState(t *testing.T, e *Engine, id string, want State) Status {
	t.Helper()
	st := stateOf(t, e, id)
	if st.State != want {
		t.Fatalf("alarm %q: state = %s, want %s (reason=%q)", id, st.State, want, st.Reason)
	}
	return st
}

// drainEvents 取出订阅通道里当前全部事件。
func drainEvents(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// 规则校验
// ---------------------------------------------------------------------------

func TestRuleValidate(t *testing.T) {
	base := Rule{ID: "r1", Severity: SeverityWarning, Point: "dev:p", High: ptr(80)}

	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{"ok threshold", base, ""},
		{"ok low only", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", Low: ptr(5)}, ""},
		{"ok dual side", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(80), Low: ptr(10)}, ""},
		{"ok dual side with hysteresis", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(80), Low: ptr(10), Hysteresis: 2}, ""},
		{"ok expr", Rule{ID: "r1", Severity: SeverityInfo, Expr: "dev:p > 1"}, ""},
		{"bad id upper", Rule{ID: "R1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1)}, "must match"},
		{"bad id space", Rule{ID: "r 1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1)}, "must match"},
		{"bad id leading dot", Rule{ID: ".r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1)}, "must match"},
		{"bad id empty", Rule{Severity: SeverityInfo, Point: "dev:p", High: ptr(1)}, "must match"},
		{"bad severity", Rule{ID: "r1", Severity: "fatal", Point: "dev:p", High: ptr(1)}, "severity"},
		{"no condition", Rule{ID: "r1", Severity: SeverityInfo}, "one of"},
		{"both conditions", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), Expr: "dev:p > 1"}, "mutually exclusive"},
		{"expr not bool", Rule{ID: "r1", Severity: SeverityInfo, Expr: "dev:p + 1"}, "boolean"},
		{"expr syntax", Rule{ID: "r1", Severity: SeverityInfo, Expr: "dev:p >"}, "expr"},
		{"expr too long", Rule{ID: "r1", Severity: SeverityInfo, Expr: "dev:p > 1 && " + strings.Repeat("dev:p > 0 && ", 200) + "dev:p > 0"}, "bytes exceeds"},
		{"point no colon", Rule{ID: "r1", Severity: SeverityInfo, Point: "p", High: ptr(1)}, "device:point"},
		{"point empty after colon", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:", High: ptr(1)}, "device:point"},
		{"point empty before colon", Rule{ID: "r1", Severity: SeverityInfo, Point: ":p", High: ptr(1)}, "device:point"},
		{"no threshold value", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", Hysteresis: 1}, "at least one threshold"},
		{"high nan", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: func() *float64 { v := math.NaN(); return &v }()}, "finite"},
		{"high inf", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: func() *float64 { v := math.Inf(1); return &v }()}, "finite"},
		{"low nan", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", Low: func() *float64 { v := math.NaN(); return &v }()}, "finite"},
		{"negative hysteresis", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), Hysteresis: -1}, "hysteresis"},
		{"high <= low", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(10), Low: ptr(10)}, "greater than low"},
		{"hysteresis too wide", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(10), Low: ptr(0), Hysteresis: 5}, "too large"},
		{"negative delay", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), OnDelay: model.Duration(-time.Second)}, ">= 0"},
		{"delay too big", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), OnDelay: model.Duration(25 * time.Hour)}, "must be <="},
		{"name too long", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), Name: strings.Repeat("x", MaxNameBytes+1)}, "exceeds"},
		{"message too long", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), Message: strings.Repeat("x", MaxMessageBytes+1)}, "exceeds"},
		{"control char in name", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), Name: "a\nb"}, "control characters"},
		{"control char in message", Rule{ID: "r1", Severity: SeverityInfo, Point: "dev:p", High: ptr(1), Message: "a\tb"}, "control characters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want contains %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeDefaults(t *testing.T) {
	r := Rule{ID: "r1", Point: "dev:p", High: ptr(1)}
	r.Normalize()
	if r.Severity != SeverityWarning {
		t.Fatalf("severity = %q, want warning", r.Severity)
	}
	// 显式值不被覆盖，且幂等。
	r.Severity = SeverityCritical
	r.Normalize()
	if r.Severity != SeverityCritical {
		t.Fatalf("severity overwritten: %q", r.Severity)
	}
}

func TestSplitRef(t *testing.T) {
	tests := []struct {
		in  string
		dev string
		pt  string
		ok  bool
	}{
		{"a:b", "a", "b", true},
		{"line-a:power", "line-a", "power", true},
		{"a:b:c", "a", "b:c", true},
		{"a:", "", "", false},
		{":b", "", "", false},
		{"ab", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		dev, pt, ok := SplitRef(tc.in)
		if ok != tc.ok || dev != tc.dev || pt != tc.pt {
			t.Fatalf("SplitRef(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, dev, pt, ok, tc.dev, tc.pt, tc.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// 阈值与滞回
// ---------------------------------------------------------------------------

func TestHighThresholdWithHysteresis(t *testing.T) {
	e, clk, src := newTestEngine(t, []Rule{{
		ID: "high", Severity: SeverityWarning, Point: "dev:power",
		High: ptr(80), Hysteresis: 5,
	}}, nil)

	// 没有采样：未知，不能报警。
	e.Sweep()
	st := mustState(t, e, "high", StateNormal)
	if st.ValueKnown {
		t.Fatal("value should be unknown before any sample")
	}
	if !strings.Contains(st.Reason, "no value") {
		t.Fatalf("reason = %q", st.Reason)
	}

	// 刚好等于阈值不触发：`>` 是严格的。
	src.set("dev", "power", 80, model.QualityGood)
	e.Sweep()
	mustState(t, e, "high", StateNormal)

	// 超过阈值：立刻 active（onDelay=0）。
	clk.Advance(time.Second)
	src.set("dev", "power", 80.1, model.QualityGood)
	e.Sweep()
	st = mustState(t, e, "high", StateActive)
	if st.Value == nil || *st.Value != 80.1 {
		t.Fatalf("value = %v", st.Value)
	}
	if st.Count != 1 {
		t.Fatalf("count = %d, want 1", st.Count)
	}
	if st.Since == nil || !st.Since.Equal(clk.Now()) {
		t.Fatalf("since = %v", st.Since)
	}

	// 落回滞回带（75~80）内：保持 active —— 这是滞回的全部意义。
	clk.Advance(time.Second)
	src.set("dev", "power", 78, model.QualityGood)
	e.Sweep()
	mustState(t, e, "high", StateActive)

	// 跌破 High-Hysteresis：清除（因未确认 → cleared）。
	clk.Advance(time.Second)
	src.set("dev", "power", 74.9, model.QualityGood)
	e.Sweep()
	st = mustState(t, e, "high", StateCleared)
	if st.ClearedAt == nil || !st.ClearedAt.Equal(clk.Now()) {
		t.Fatalf("clearedAt = %v", st.ClearedAt)
	}

	// cleared 期间条件再次成立：重新 active，并重置确认状态。
	clk.Advance(time.Second)
	src.set("dev", "power", 81, model.QualityGood)
	e.Sweep()
	st = mustState(t, e, "high", StateActive)
	if st.Count != 2 {
		t.Fatalf("count = %d, want 2", st.Count)
	}
	if st.Acknowledged {
		t.Fatal("a new raise must not inherit the previous ack")
	}
}

func TestLowThreshold(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "low", Severity: SeverityWarning, Point: "dev:tank", Low: ptr(10), Hysteresis: 2,
	}}, nil)

	src.set("dev", "tank", 50, model.QualityGood)
	e.Sweep()
	mustState(t, e, "low", StateNormal)

	src.set("dev", "tank", 9, model.QualityGood)
	e.Sweep()
	mustState(t, e, "low", StateActive)

	// 回到 10~12 的滞回带内保持。
	src.set("dev", "tank", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "low", StateActive)

	src.set("dev", "tank", 12.1, model.QualityGood)
	e.Sweep()
	mustState(t, e, "low", StateCleared)
}

func TestDualSideThreshold(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "band", Severity: SeverityWarning, Point: "dev:temp",
		Low: ptr(10), High: ptr(30), Hysteresis: 2,
	}}, nil)

	src.set("dev", "temp", 20, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateNormal)

	src.set("dev", "temp", 31, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateActive)

	// 带内：保持（28~32 之间都属于"仍在高压侧"）。
	src.set("dev", "temp", 29, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateActive)

	// 进入正常区：清除。
	src.set("dev", "temp", 27, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateCleared)

	// 低侧同样工作。
	src.set("dev", "temp", 8, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateActive)
	src.set("dev", "temp", 9, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateActive)
	src.set("dev", "temp", 12.5, model.QualityGood)
	e.Sweep()
	mustState(t, e, "band", StateCleared)
}

func TestZeroHysteresisChattersWithoutDamping(t *testing.T) {
	// 没有滞回 + 没有 offDelay 时，值在阈值附近抖动必然产生一串事件。
	// 这个测试是「为什么需要滞回」的可执行说明，而不是缺陷。
	e, _, src := newTestEngine(t, []Rule{{
		ID: "chatter", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)

	ch, cancel := e.Subscribe(128)
	defer cancel()

	for i, v := range []float64{11, 9, 11, 9} {
		src.set("dev", "v", v, model.QualityGood)
		e.Sweep()
		_ = i
	}
	evs := drainEvents(ch)
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4 (raised/cleared alternating)", len(evs))
	}
	want := []EventType{EventRaised, EventCleared, EventRaised, EventCleared}
	for i, ev := range evs {
		if ev.Type != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, ev.Type, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 延时
// ---------------------------------------------------------------------------

func TestOnDelayRequiresContinuousCondition(t *testing.T) {
	e, clk, src := newTestEngine(t, []Rule{{
		ID: "delay", Severity: SeverityWarning, Point: "dev:v",
		High: ptr(10), OnDelay: ms(1000),
	}}, nil)

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	st := mustState(t, e, "delay", StatePending)
	if !st.Pending || st.Active {
		t.Fatalf("pending=%v active=%v", st.Pending, st.Active)
	}

	// 800ms 还没到点。
	clk.Advance(800 * time.Millisecond)
	e.Sweep()
	mustState(t, e, "delay", StatePending)

	// 抖一下：计时重置。
	src.set("dev", "v", 5, model.QualityGood)
	e.Sweep()
	mustState(t, e, "delay", StateNormal)

	// 重新满足，但从头计时。
	clk.Advance(time.Second)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "delay", StatePending)

	clk.Advance(999 * time.Millisecond)
	e.Sweep()
	mustState(t, e, "delay", StatePending)

	clk.Advance(time.Millisecond)
	e.Sweep()
	mustState(t, e, "delay", StateActive)
}

func TestOnDelayZeroRaisesImmediately(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "now", Severity: SeverityInfo, Point: "dev:v", High: ptr(0),
	}}, nil)
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	mustState(t, e, "now", StateActive)
}

func TestOffDelayRequiresContinuousRecovery(t *testing.T) {
	e, clk, src := newTestEngine(t, []Rule{{
		ID: "off", Severity: SeverityWarning, Point: "dev:v",
		High: ptr(10), OffDelay: ms(1000),
	}}, nil)

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "off", StateActive)

	// 恢复但还没到 offDelay：保持 active。
	src.set("dev", "v", 5, model.QualityGood)
	e.Sweep()
	mustState(t, e, "off", StateActive)

	clk.Advance(999 * time.Millisecond)
	e.Sweep()
	mustState(t, e, "off", StateActive)

	// 还没到点又超阈值：恢复计时清零。
	src.set("dev", "v", 12, model.QualityGood)
	e.Sweep()
	mustState(t, e, "off", StateActive)
	clk.Advance(2 * time.Second)
	src.set("dev", "v", 5, model.QualityGood)
	e.Sweep()
	mustState(t, e, "off", StateActive)

	clk.Advance(time.Second)
	e.Sweep()
	mustState(t, e, "off", StateCleared)
}

// ---------------------------------------------------------------------------
// 质量门控
// ---------------------------------------------------------------------------

func TestBadQualityDoesNotClearActive(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "q", Severity: SeverityCritical, Point: "dev:v", High: ptr(10),
	}}, nil)

	src.set("dev", "v", 20, model.QualityGood)
	e.Sweep()
	mustState(t, e, "q", StateActive)

	// 数据断了：绝不能当成「好了」。
	src.set("dev", "v", 20, model.QualityTimeout)
	e.Sweep()
	st := mustState(t, e, "q", StateActive)
	if st.ValueKnown {
		t.Fatal("value should be reported as unknown")
	}
	if st.Value == nil || *st.Value != 20 {
		t.Fatalf("last known value must survive, got %v", st.Value)
	}
	if st.Reason == "" {
		t.Fatal("unknown input must carry a reason")
	}
	if c := e.Counts(); c.Unknown != 1 || c.Active != 1 {
		t.Fatalf("counts = %+v", c)
	}

	// 恢复可信后继续按值判定（值仍在阈值上方 → 保持 active）。
	src.set("dev", "v", 20, model.QualityGood)
	e.Sweep()
	mustState(t, e, "q", StateActive)

	// 数据恢复且值正常 → 清除。
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	mustState(t, e, "q", StateCleared)
}

func TestBadQualityCancelsPending(t *testing.T) {
	e, clk, src := newTestEngine(t, []Rule{{
		ID: "p", Severity: SeverityWarning, Point: "dev:v", High: ptr(10), OnDelay: ms(1000),
	}}, nil)

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "p", StatePending)

	// 未知时间不能被用来凑够 onDelay。
	clk.Advance(500 * time.Millisecond)
	src.set("dev", "v", 0, model.QualityCommError)
	e.Sweep()
	mustState(t, e, "p", StateNormal)

	// 恢复可信：重新计时，而不是接着之前的 500ms。
	clk.Advance(time.Second)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "p", StatePending)
	clk.Advance(600 * time.Millisecond)
	e.Sweep()
	mustState(t, e, "p", StatePending)
}

func TestUncertainQualityIsUsable(t *testing.T) {
	// uncertain 表示「值可信但可能不是最新」，仍应参与判定（与采集层
	// 的质量传播规则一致）。
	e, _, src := newTestEngine(t, []Rule{{
		ID: "u", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)
	src.set("dev", "v", 11, model.QualityUncertain)
	e.Sweep()
	st := mustState(t, e, "u", StateActive)
	if st.Quality != model.QualityUncertain {
		t.Fatalf("quality = %q", st.Quality)
	}
}

func TestStaleIsNotUsable(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "s", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)
	src.set("dev", "v", 11, model.QualityStale)
	e.Sweep()
	st := mustState(t, e, "s", StateNormal)
	if st.ValueKnown {
		t.Fatal("stale must not be treated as a valid input")
	}
}

func TestNonNumericValueIsUnknown(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "t", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)
	src.setText("dev", "v", "hot", model.QualityGood)
	e.Sweep()
	st := mustState(t, e, "t", StateNormal)
	if st.ValueKnown {
		t.Fatal("text value must be unknown")
	}
	if !strings.Contains(st.Reason, "not numeric") {
		t.Fatalf("reason = %q", st.Reason)
	}
}

func TestNilLookup(t *testing.T) {
	e, err := New(Options{
		Rules: []Rule{{ID: "x", Severity: SeverityInfo, Point: "dev:v", High: ptr(1)}},
		Now:   newFakeClock().Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Sweep()
	st := mustState(t, e, "x", StateNormal)
	if !strings.Contains(st.Reason, "no value source") {
		t.Fatalf("reason = %q", st.Reason)
	}
}

// ---------------------------------------------------------------------------
// 确认
// ---------------------------------------------------------------------------

func TestAckFlow(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "a", Severity: SeverityWarning, Point: "dev:v", High: ptr(10),
	}}, nil)

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "a", StateActive)

	// 从未报警时不可确认。
	other, _, _ := newTestEngine(t, []Rule{{ID: "b", Severity: SeverityInfo, Point: "dev:v", High: ptr(10)}}, nil)
	if _, err := other.Ack("b", "alice"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("ack normal: err = %v, want ErrNotActive", err)
	}

	st, err := e.Ack("a", "alice")
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if st.State != StateAcked || !st.Acknowledged {
		t.Fatalf("state = %s acked = %v", st.State, st.Acknowledged)
	}
	if st.AckBy != "alice" || st.AckAt == nil {
		t.Fatalf("ackBy = %q ackAt = %v", st.AckBy, st.AckAt)
	}
	if !st.Active {
		t.Fatal("acked alarm is still active (condition may still hold)")
	}

	// 幂等：重复确认不报错、不产生第二个事件。
	ch, cancel := e.Subscribe(16)
	defer cancel()
	st2, err := e.Ack("a", "bob")
	if err != nil {
		t.Fatalf("idempotent ack: %v", err)
	}
	if st2.AckBy != "alice" {
		t.Fatalf("idempotent ack overwrote ackBy: %q", st2.AckBy)
	}
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("idempotent ack emitted %d events", len(evs))
	}

	// 已确认后恢复：直接回 normal，不需要二次确认。
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	st = mustState(t, e, "a", StateNormal)
	if !st.Acknowledged {
		t.Fatal("recovered-and-acked alarm should stay acknowledged")
	}
}

func TestClearThenAck(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "c", Severity: SeverityWarning, Point: "dev:v", High: ptr(10),
	}}, nil)

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	mustState(t, e, "c", StateCleared)

	// cleared 也要人确认：恢复本身不代表运维看到了这次报警。
	st, err := e.Ack("c", "carol")
	if err != nil {
		t.Fatalf("Ack cleared: %v", err)
	}
	if st.State != StateNormal || !st.Acknowledged {
		t.Fatalf("state = %s acked = %v", st.State, st.Acknowledged)
	}

	// 已回到 normal 后再确认是「状态已过期」。
	if _, err := e.Ack("c", "carol"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("err = %v, want ErrNotActive", err)
	}
}

func TestAutoAck(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "auto", Severity: SeverityInfo, Point: "dev:v", High: ptr(10), AutoAck: true,
	}}, nil)

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "auto", StateActive)

	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	st := mustState(t, e, "auto", StateNormal)
	if !st.Acknowledged || st.AckBy != "auto" {
		t.Fatalf("acked=%v by=%q", st.Acknowledged, st.AckBy)
	}
}

func TestAckPendingRejected(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "p", Severity: SeverityInfo, Point: "dev:v", High: ptr(10), OnDelay: ms(1000),
	}}, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "p", StatePending)

	_, err := e.Ack("p", "alice")
	if !errors.Is(err, ErrNotActive) {
		t.Fatalf("err = %v, want ErrNotActive", err)
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Fatalf("error should explain the pending state: %v", err)
	}
	// 失败不能改变状态。
	mustState(t, e, "p", StatePending)
}

func TestAckNotFound(t *testing.T) {
	e, _, _ := newTestEngine(t, nil, nil)
	if _, err := e.Ack("nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := e.AckMany([]string{"nope"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AckMany err = %v, want ErrNotFound", err)
	}
}

func TestAckAllAndAckMany(t *testing.T) {
	rules := []Rule{
		{ID: "a1", Severity: SeverityCritical, Point: "dev:v", High: ptr(10)},
		{ID: "a2", Severity: SeverityWarning, Point: "dev:v", High: ptr(10)},
		{ID: "a3", Severity: SeverityInfo, Point: "dev:v", High: ptr(10)},
	}
	e, _, src := newTestEngine(t, rules, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()

	if c := e.Counts(); c.Active != 3 {
		t.Fatalf("active = %d", c.Active)
	}

	// 已确认的 ID 混在批量里是幂等的，不算失败。
	if _, err := e.AckMany([]string{"a1", "a2"}, "op"); err != nil {
		t.Fatalf("AckMany: %v", err)
	}
	if _, err := e.AckMany([]string{"a3", "a1"}, "op"); err != nil {
		t.Fatalf("AckMany with an already-acked id: %v", err)
	}

	// 但混入一条不存在的 / 不可确认的必须整体失败，不能半完成。
	normal, _, _ := newTestEngine(t, []Rule{
		{ID: "n1", Severity: SeverityInfo, Point: "dev:v", High: ptr(10)},
		{ID: "n2", Severity: SeverityInfo, Point: "dev:v", High: ptr(10)},
	}, nil)
	if _, err := normal.AckMany([]string{"n1"}, ""); !errors.Is(err, ErrNotActive) {
		t.Fatalf("AckMany on normal: err = %v", err)
	}
	if _, err := e.AckMany([]string{"a3", "missing"}, "op"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AckMany with unknown id: err = %v", err)
	}

	// 全部已确认 → AckAll 什么都不做。
	out, err := e.AckMany(nil, "op")
	if err != nil || len(out) != 0 {
		t.Fatalf("AckMany(nil) = %v, %v", out, err)
	}
	if got := e.AckAll("op"); len(got) != 0 {
		t.Fatalf("AckAll returned %d", len(got))
	}

	// 一轮恢复 + 复发之后，重复 ID 只处理一次。
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	if c := e.Counts(); c.Normal != 3 {
		t.Fatalf("counts after clear = %+v", c)
	}
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	out, err = e.AckMany([]string{"a3", "a3"}, "op")
	if err != nil {
		t.Fatalf("AckMany dup: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("returned %d statuses, want 1", len(out))
	}

	// 同一轮的剩下两条由 AckAll 清理。
	out = e.AckAll("op")
	if len(out) != 2 {
		t.Fatalf("AckAll returned %d, want 2", len(out))
	}
	for _, st := range out {
		if st.State != StateAcked {
			t.Fatalf("%s state = %s", st.ID, st.State)
		}
	}
}

// ---------------------------------------------------------------------------
// 表达式规则
// ---------------------------------------------------------------------------

func TestExprRule(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "eff", Severity: SeverityWarning,
		Expr: "dev:in > 0 && dev:out / dev:in < 0.5",
	}}, nil)

	// 缺输入 → 未知，不报警。
	src.set("dev", "in", 100, model.QualityGood)
	e.Sweep()
	st := mustState(t, e, "eff", StateNormal)
	if st.ValueKnown {
		t.Fatal("missing reference must be unknown")
	}
	if !strings.Contains(st.Reason, "dev:out") {
		t.Fatalf("reason should name the missing ref: %q", st.Reason)
	}

	src.set("dev", "out", 80, model.QualityGood)
	e.Sweep()
	st = mustState(t, e, "eff", StateNormal)
	if !st.ValueKnown || st.Value == nil || *st.Value != 0 {
		t.Fatalf("boolean expr should report 0/1, got %v", st.Value)
	}

	src.set("dev", "out", 30, model.QualityGood)
	e.Sweep()
	st = mustState(t, e, "eff", StateActive)
	if st.Value == nil || *st.Value != 1 {
		t.Fatalf("value = %v, want 1", st.Value)
	}
}

func TestExprShortCircuitSkipsBadInput(t *testing.T) {
	// `false && bad` 是 false，不是未知：没有求值的引用不参与质量判定。
	e, _, src := newTestEngine(t, []Rule{{
		ID: "sc", Severity: SeverityInfo, Expr: "dev:flag > 0 && dev:dead > 0",
	}}, nil)

	src.set("dev", "flag", 0, model.QualityGood)
	src.set("dev", "dead", 0, model.QualityTimeout)
	e.Sweep()
	st := mustState(t, e, "sc", StateNormal)
	if !st.ValueKnown {
		t.Fatalf("short-circuited expression should be known, reason=%q", st.Reason)
	}
}

func TestExprDivisionByZeroIsUnknownNotActive(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "div", Severity: SeverityInfo, Expr: "dev:a / dev:b > 1",
	}}, nil)
	src.set("dev", "a", 10, model.QualityGood)
	src.set("dev", "b", 0, model.QualityGood)
	e.Sweep()
	st := mustState(t, e, "div", StateNormal)
	if st.ValueKnown {
		t.Fatal("evaluation error must be unknown, not false")
	}
	if !strings.Contains(st.Reason, "expression") {
		t.Fatalf("reason = %q", st.Reason)
	}
}

func TestExprIncludesOnlyEvaluatedRefsInWorst(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "worst", Severity: SeverityInfo, Expr: "dev:a > 1 && dev:b > 1",
	}}, nil)
	src.set("dev", "a", 5, model.QualityUncertain)
	src.set("dev", "b", 5, model.QualityGood)
	e.Sweep()
	st := mustState(t, e, "worst", StateActive)
	if st.Quality != model.QualityUncertain {
		t.Fatalf("quality = %q, want uncertain", st.Quality)
	}
}

func TestReferences(t *testing.T) {
	r := Rule{ID: "t", Severity: SeverityInfo, Point: "dev:p", High: ptr(1)}
	refs, err := r.References()
	if err != nil || len(refs) != 1 || refs[0] != "dev:p" {
		t.Fatalf("refs = %v err = %v", refs, err)
	}

	r = Rule{ID: "e", Severity: SeverityInfo, Expr: "dev:a > dev:b"}
	refs, err = r.References()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != "dev:a" || refs[1] != "dev:b" {
		t.Fatalf("refs = %v", refs)
	}

	r = Rule{ID: "bad", Severity: SeverityInfo, Expr: "dev:a >"}
	if _, err := r.References(); err == nil {
		t.Fatal("want error for invalid expr")
	}
}

// ---------------------------------------------------------------------------
// 停用
// ---------------------------------------------------------------------------

func TestDisabledRuleIsNeverEvaluated(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "off", Severity: SeverityCritical, Point: "dev:v", High: ptr(1), Disabled: true,
	}}, nil)

	src.set("dev", "v", 100, model.QualityGood)
	e.Sweep()

	st := mustState(t, e, "off", StateNormal)
	if !st.Disabled {
		t.Fatal("status should report disabled")
	}
	if st.ValueKnown {
		t.Fatal("disabled rule must not be evaluated")
	}
	// 停用的规则不算 unknown：否则会伪造出「链路故障」。
	if c := e.Counts(); c.Unknown != 0 || c.Total != 1 || c.Normal != 1 {
		t.Fatalf("counts = %+v", c)
	}
	if _, err := e.Ack("off", ""); !errors.Is(err, ErrNotActive) {
		t.Fatalf("ack disabled: %v", err)
	}
}

func TestDisabledToEnabledStartsFresh(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "d", Severity: SeverityInfo, Point: "dev:v", High: ptr(1), Disabled: true,
	}}, nil)
	src.set("dev", "v", 100, model.QualityGood)
	e.Sweep()

	if err := e.Reload([]Rule{{
		ID: "d", Severity: SeverityInfo, Point: "dev:v", High: ptr(1),
	}}); err != nil {
		t.Fatal(err)
	}
	e.Sweep()
	mustState(t, e, "d", StateActive)
}

// ---------------------------------------------------------------------------
// Reload
// ---------------------------------------------------------------------------

func TestReloadKeepsStateWhenConditionUnchanged(t *testing.T) {
	rule := Rule{ID: "r", Severity: SeverityWarning, Point: "dev:v", High: ptr(10), Name: "old"}
	e, _, src := newTestEngine(t, []Rule{rule}, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	if _, err := e.Ack("r", "alice"); err != nil {
		t.Fatal(err)
	}
	mustState(t, e, "r", StateAcked)

	ch, cancel := e.Subscribe(16)
	defer cancel()

	// 只改展示信息：状态必须保留。
	renamed := rule
	renamed.Name = "new"
	renamed.Message = "peak demand"
	if err := e.Reload([]Rule{renamed}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, e, "r", StateAcked)
	if st.Name != "new" || st.Message != "peak demand" {
		t.Fatalf("metadata not refreshed: %+v", st)
	}
	if st.Count != 1 || !st.Acknowledged {
		t.Fatalf("state lost on cosmetic reload: %+v", st)
	}
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("cosmetic reload emitted %d events", len(evs))
	}

	// 改判定条件：状态重置，并发出 reset 事件告诉消费方。
	changed := renamed
	changed.High = ptr(20)
	if err := e.Reload([]Rule{changed}); err != nil {
		t.Fatal(err)
	}
	st = mustState(t, e, "r", StateNormal)
	if st.Acknowledged {
		t.Fatal("reset must clear the ack flag")
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Type != EventReset {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].From != StateAcked {
		t.Fatalf("from = %s", evs[0].From)
	}
}

func TestReloadClearedRuleEmitsReset(t *testing.T) {
	// cleared 是一个真实存在、等待确认的状态，retained 主题上就是它。
	// 规则被改掉后这个待确认项已不存在，必须让消费方收到收尾。
	rule := Rule{ID: "c", Severity: SeverityWarning, Point: "dev:v", High: ptr(10)}
	e, _, src := newTestEngine(t, []Rule{rule}, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	mustState(t, e, "c", StateCleared)

	ch, cancel := e.Subscribe(16)
	defer cancel()

	changed := rule
	changed.High = ptr(20)
	if err := e.Reload([]Rule{changed}); err != nil {
		t.Fatal(err)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Type != EventReset || evs[0].From != StateCleared {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Status.State != StateNormal || evs[0].Status.Acknowledged {
		t.Fatalf("reset status = %+v", evs[0].Status)
	}
}

func TestReloadDisableActiveRuleClearsState(t *testing.T) {
	// 停用一条正在报警的规则：状态必须归位并发 reset，否则 retained 主题
	// 会永远挂着一个没人能确认、也不会恢复的 active 报警。
	rule := Rule{ID: "d", Severity: SeverityCritical, Point: "dev:v", High: ptr(10)}
	e, _, src := newTestEngine(t, []Rule{rule}, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "d", StateActive)

	ch, cancel := e.Subscribe(16)
	defer cancel()

	off := rule
	off.Disabled = true
	if err := e.Reload([]Rule{off}); err != nil {
		t.Fatal(err)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Type != EventReset || evs[0].From != StateActive {
		t.Fatalf("events = %+v", evs)
	}
	st := mustState(t, e, "d", StateNormal)
	if !st.Disabled || st.Active {
		t.Fatalf("status = %+v", st)
	}
	if c := e.Counts(); c.Active != 0 || c.Unknown != 0 || c.Normal != 1 {
		t.Fatalf("counts = %+v", c)
	}

	// 再启用：从头开始评估（不是接续被停用前的 active）。
	if err := e.Reload([]Rule{rule}); err != nil {
		t.Fatal(err)
	}
	e.Sweep()
	mustState(t, e, "d", StateActive)
}

func TestReloadDisabledToDisabled(t *testing.T) {
	// 停用的规则被 reload 时保留 disabled 语义（而不是「重置后启用」）。
	rule := Rule{ID: "r", Severity: SeverityInfo, Point: "dev:v", High: ptr(1), Disabled: true}
	e, _, _ := newTestEngine(t, []Rule{rule}, nil)
	if err := e.Reload([]Rule{rule}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, e, "r", StateNormal)
	if !st.Disabled {
		t.Fatal("rule should remain disabled")
	}
}

func TestReloadRemovedRuleEmitsEvent(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "gone", Severity: SeverityCritical, Point: "dev:v", High: ptr(1),
	}}, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "gone", StateActive)

	ch, cancel := e.Subscribe(16)
	defer cancel()

	if err := e.Reload(nil); err != nil {
		t.Fatal(err)
	}
	evs := drainEvents(ch)
	if len(evs) != 1 || evs[0].Type != EventRemoved {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Status.ID != "gone" || evs[0].From != StateActive {
		t.Fatalf("event = %+v", evs[0])
	}
	if _, ok := e.Get("gone"); ok {
		t.Fatal("removed rule still queryable")
	}
	if c := e.Counts(); c.Total != 0 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestReloadInvalidRuleKeepsOldSet(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "keep", Severity: SeverityInfo, Point: "dev:v", High: ptr(1),
	}}, nil)
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	mustState(t, e, "keep", StateActive)

	// 校验失败的 reload 必须是原子的：旧规则集完全不受影响。
	if err := e.Reload([]Rule{{ID: "bad", Severity: SeverityInfo}}); err == nil {
		t.Fatal("want validation error")
	}
	mustState(t, e, "keep", StateActive)
	if c := e.Counts(); c.Total != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestNewRejectsDuplicateID(t *testing.T) {
	_, err := New(Options{Rules: []Rule{
		{ID: "dup", Severity: SeverityInfo, Point: "dev:v", High: ptr(1)},
		{ID: "dup", Severity: SeverityInfo, Point: "dev:v", High: ptr(2)},
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func TestListFilterAndSort(t *testing.T) {
	rules := []Rule{
		{ID: "b-info", Severity: SeverityInfo, Point: "dev:v", High: ptr(100)},
		{ID: "a-crit", Severity: SeverityCritical, Point: "dev:v", High: ptr(10)},
		{ID: "c-warn", Severity: SeverityWarning, Point: "dev:v", High: ptr(20)},
	}
	e, _, src := newTestEngine(t, rules, nil)
	src.set("dev", "v", 50, model.QualityGood)
	e.Sweep()

	// 无条件时返回全部规则（不只是正在报警的）：列表页要能看到「一切正常」，
	// 否则运维无法区分「没有报警」和「报警规则没加载」。
	all := e.List(Filter{})
	got := make([]string, len(all))
	for i, st := range all {
		got[i] = st.ID
	}
	want := []string{"a-crit", "c-warn", "b-info"} // 严重度降序
	if len(got) != len(want) {
		t.Fatalf("list = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("list = %v, want %v", got, want)
		}
	}
	if all[2].State != StateNormal {
		t.Fatalf("b-info state = %s", all[2].State)
	}

	// 按状态过滤。
	if l := e.List(Filter{States: []State{StateActive}}); len(l) != 2 {
		t.Fatalf("active list = %d", len(l))
	}
	if l := e.List(Filter{States: []State{StateNormal}}); len(l) != 1 || l[0].ID != "b-info" {
		t.Fatalf("normal list = %+v", l)
	}

	// 按严重度过滤。
	if l := e.List(Filter{Severities: []Severity{SeverityCritical}}); len(l) != 1 || l[0].ID != "a-crit" {
		t.Fatalf("crit list = %+v", l)
	}

	// 搜索匹配 ID / Name / Message，不区分大小写。
	if l := e.List(Filter{Q: "CRIT"}); len(l) != 1 || l[0].ID != "a-crit" {
		t.Fatalf("q=CRIT list = %+v", l)
	}
	if l := e.List(Filter{Q: "nothing"}); len(l) != 0 {
		t.Fatalf("q=nothing list = %+v", l)
	}

	// Limit。
	if l := e.List(Filter{Limit: 1}); len(l) != 1 || l[0].ID != "a-crit" {
		t.Fatalf("limit list = %+v", l)
	}
}

func TestListSortsSameSeverityBySince(t *testing.T) {
	rules := []Rule{
		{ID: "later", Severity: SeverityWarning, Point: "dev:late", High: ptr(1)},
		{ID: "earlier", Severity: SeverityWarning, Point: "dev:early", High: ptr(1)},
	}
	e, clk, src := newTestEngine(t, rules, nil)

	src.set("dev", "early", 2, model.QualityGood)
	e.Sweep()
	clk.Advance(time.Minute)
	src.set("dev", "late", 2, model.QualityGood)
	e.Sweep()

	l := e.List(Filter{})
	if len(l) != 2 || l[0].ID != "earlier" || l[1].ID != "later" {
		t.Fatalf("list = %+v", l)
	}
}

func TestRulesSnapshot(t *testing.T) {
	rules := []Rule{{ID: "r", Severity: SeverityInfo, Point: "dev:v", High: ptr(1)}}
	e, _, _ := newTestEngine(t, rules, nil)
	got := e.Rules()
	if len(got) != 1 || got[0].ID != "r" {
		t.Fatalf("rules = %v", got)
	}
	// 修改返回值不能影响引擎。
	got[0].ID = "mutated"
	if _, ok := e.Get("r"); !ok {
		t.Fatal("engine state mutated through Rules()")
	}
}

func TestCountsAllStates(t *testing.T) {
	rules := []Rule{
		{ID: "normal", Severity: SeverityInfo, Point: "dev:v", High: ptr(1000)},
		{ID: "pending", Severity: SeverityInfo, Point: "dev:v", High: ptr(1), OnDelay: ms(1000)},
		{ID: "active", Severity: SeverityCritical, Point: "dev:v", High: ptr(2)},
		{ID: "cleared", Severity: SeverityWarning, Point: "dev:x", High: ptr(5)},
		{ID: "unknown", Severity: SeverityInfo, Point: "dev:missing", High: ptr(1)},
	}
	e, _, src := newTestEngine(t, rules, nil)
	src.set("dev", "v", 5, model.QualityGood)
	src.set("dev", "x", 6, model.QualityGood)
	e.Sweep()
	mustState(t, e, "cleared", StateActive)

	// cleared 需要先 active 再恢复。
	src.set("dev", "x", 1, model.QualityGood)
	e.Sweep()
	mustState(t, e, "cleared", StateCleared)

	c := e.Counts()
	if c.Total != 5 || c.Normal != 2 || c.Pending != 1 || c.Active != 1 || c.Cleared != 1 || c.Unknown != 1 {
		t.Fatalf("counts = %+v", c)
	}
	if c.Acked != 0 {
		t.Fatalf("acked = %d", c.Acked)
	}
}

// ---------------------------------------------------------------------------
// 订阅
// ---------------------------------------------------------------------------

func TestSubscribeDropOldest(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "s", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)
	ch, cancel := e.Subscribe(1)
	defer cancel()

	// 三个事件依次产生，缓冲区只有 1：保留最新的一条。
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	src.set("dev", "v", 1, model.QualityGood)
	e.Sweep()
	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()

	evs := drainEvents(ch)
	if len(evs) != 1 {
		t.Fatalf("buffered %d events, want 1", len(evs))
	}
	if evs[0].Type != EventRaised {
		t.Fatalf("kept %s, want the newest (%s)", evs[0].Type, EventRaised)
	}
}

func TestSubscribeDefaultBufferAndCancel(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "s", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)
	ch, cancel := e.Subscribe(0)
	cancel()
	cancel() // 重复调用安全

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("cancelled subscriber received %d events", len(evs))
	}
}

func TestMultipleSubscribersAllNotified(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "s", Severity: SeverityInfo, Point: "dev:v", High: ptr(10),
	}}, nil)
	ch1, c1 := e.Subscribe(4)
	defer c1()
	ch2, c2 := e.Subscribe(4)
	defer c2()

	src.set("dev", "v", 11, model.QualityGood)
	e.Sweep()
	// 只有依赖该点位的规则才会被 OnSample 评估；此处用 Sweep 保证执行。
	for _, ch := range []<-chan Event{ch1, ch2} {
		if evs := drainEvents(ch); len(evs) != 1 {
			t.Fatalf("subscriber got %d events", len(evs))
		}
	}
}

// ---------------------------------------------------------------------------
// OnSample / Start / Stop
// ---------------------------------------------------------------------------

func TestOnSampleOnlyEvaluatesDependents(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{
		{ID: "a", Severity: SeverityInfo, Point: "dev:a", High: ptr(1)},
		{ID: "b", Severity: SeverityInfo, Point: "dev:b", High: ptr(1)},
	}, nil)

	src.set("dev", "a", 5, model.QualityGood)
	sm, _ := src.lookup("dev", "a")
	e.OnSample(sm)
	mustState(t, e, "a", StateActive)
	// b 没有被评估：仍然是未知，而不是「正常」。
	st := stateOf(t, e, "b")
	if st.ValueKnown {
		t.Fatal("unrelated rule must not be evaluated by OnSample")
	}

	// 表达式规则的索引要覆盖每个引用。
	e2, _, src2 := newTestEngine(t, []Rule{{ID: "x", Severity: SeverityInfo, Expr: "dev:p > 0 && dev:q > 0"}}, nil)
	src2.set("dev", "p", 1, model.QualityGood)
	src2.set("dev", "q", 1, model.QualityGood)
	sm, _ = src2.lookup("dev", "q")
	e2.OnSample(sm)
	mustState(t, e2, "x", StateActive)
}

func TestOnSampleUnknownPointIsNoop(t *testing.T) {
	e, _, _ := newTestEngine(t, []Rule{{ID: "a", Severity: SeverityInfo, Point: "dev:a", High: ptr(1)}}, nil)
	e.OnSample(model.Sample{DeviceID: "other", PointID: "x", Value: model.NumValue(model.TypeFloat64, 9), Quality: model.QualityGood})
	mustState(t, e, "a", StateNormal)
}

func TestStartStopIdempotent(t *testing.T) {
	e, _, src := newTestEngine(t, []Rule{{
		ID: "t", Severity: SeverityInfo, Point: "dev:v", High: ptr(1), OnDelay: model.Duration(5 * time.Millisecond),
	}}, nil)
	// 用真实时钟跑：验证兜底循环确实能让「延时到点」在无新采样时生效。
	e.now = time.Now
	src.set("dev", "v", 11, model.QualityGood)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.switchSweep(5 * time.Millisecond)
	e.Start(ctx)
	e.Start(ctx) // 重复启动安全

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := e.Get("t"); ok && st.State == StateActive {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	mustState(t, e, "t", StateActive)

	e.Stop()
	e.Stop() // 重复停止安全
}

// switchSweep 只在测试里改兜底周期（字段没有对外暴露，是因为生产代码不需要
// 在运行期调整它）。
func (e *Engine) switchSweep(d time.Duration) {
	e.mu.Lock()
	e.sweep = d
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func TestConcurrentEvaluationAndQueries(t *testing.T) {
	rules := []Rule{
		{ID: "a", Severity: SeverityInfo, Point: "dev:v", High: ptr(10)},
		{ID: "b", Severity: SeverityWarning, Expr: "dev:v > 5 && dev:v < 100"},
	}
	e, _, src := newTestEngine(t, rules, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 一个写者：不断改值并评估。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			src.set("dev", "v", float64(i%40), model.QualityGood)
			sm, _ := src.lookup("dev", "v")
			e.OnSample(sm)
		}
	}()
	// 多个读者：查询 / 计数 / 订阅。
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = e.List(Filter{})
				_ = e.Counts()
				_, _ = e.Get("a")
				_, _ = e.Ack("a", "op")
				_, _ = e.AckMany([]string{"a"}, "op")
				_ = e.Rules()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = e.Reload(rules)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestEventJSONShape(t *testing.T) {
	// 事件是北向契约的一部分，字段名一旦定下就不该随意改。
	ev := Event{
		Type: EventRaised, At: time.Unix(0, 0).UTC(),
		From:   StateNormal,
		Status: Status{ID: "r", Severity: SeverityCritical, State: StateActive, Active: true, UpdatedAt: time.Unix(0, 0).UTC()},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"raised"`, `"from":"normal"`, `"state":"active"`, `"severity":"critical"`, `"active":true`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("json %s missing %s", b, want)
		}
	}
}

func TestQualityRankOrder(t *testing.T) {
	// worse 必须与采集层一致：good < uncertain < stale < timeout < not_connected < comm_error < address_error。
	ord := []model.Quality{
		model.QualityGood, model.QualityUncertain, model.QualityStale,
		model.QualityTimeout, model.QualityNotConnected, model.QualityCommError,
		model.QualityAddressError,
	}
	for i := 0; i < len(ord); i++ {
		for j := 0; j < len(ord); j++ {
			want := ord[maxInt(i, j)]
			if got := worse(ord[i], ord[j]); got != want {
				t.Fatalf("worse(%s,%s) = %s, want %s", ord[i], ord[j], got, want)
			}
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestConditionHashIgnoresPresentation(t *testing.T) {
	base := Rule{ID: "r", Severity: SeverityWarning, Point: "dev:v", High: ptr(10), Hysteresis: 1}
	h1 := conditionHash(base)

	cosmetic := base
	cosmetic.Name = "renamed"
	cosmetic.Message = "hello"
	if conditionHash(cosmetic) != h1 {
		t.Fatal("name/message must not affect the condition hash")
	}

	for name, mut := range map[string]func(Rule) Rule{
		"severity": func(r Rule) Rule { r.Severity = SeverityCritical; return r },
		"high":     func(r Rule) Rule { r.High = ptr(20); return r },
		"hyster":   func(r Rule) Rule { r.Hysteresis = 2; return r },
		"point":    func(r Rule) Rule { r.Point = "dev:other"; return r },
		"delay":    func(r Rule) Rule { r.OnDelay = ms(100); return r },
		"offdelay": func(r Rule) Rule { r.OffDelay = ms(100); return r },
		"autoack":  func(r Rule) Rule { r.AutoAck = true; return r },
		"disabled": func(r Rule) Rule { r.Disabled = true; return r },
	} {
		if conditionHash(mut(base)) == h1 {
			t.Fatalf("%s change did not alter the condition hash", name)
		}
	}
}

func TestCompiledTriggerAndHoldsTable(t *testing.T) {
	tests := []struct {
		name          string
		rule          Rule
		v             float64
		trigger, hold bool
	}{
		{"high under", Rule{Point: "d:p", High: ptr(10), Hysteresis: 2}, 9, false, true},
		{"high at", Rule{Point: "d:p", High: ptr(10), Hysteresis: 2}, 10, false, true},
		{"high over", Rule{Point: "d:p", High: ptr(10), Hysteresis: 2}, 11, true, true},
		{"high in band", Rule{Point: "d:p", High: ptr(10), Hysteresis: 2}, 8.5, false, true},
		{"high below band", Rule{Point: "d:p", High: ptr(10), Hysteresis: 2}, 8, false, false},
		{"low over", Rule{Point: "d:p", Low: ptr(10), Hysteresis: 2}, 11, false, true},
		{"low at", Rule{Point: "d:p", Low: ptr(10), Hysteresis: 2}, 10, false, true},
		{"low under", Rule{Point: "d:p", Low: ptr(10), Hysteresis: 2}, 9, true, true},
		{"low in band", Rule{Point: "d:p", Low: ptr(10), Hysteresis: 2}, 11.5, false, true},
		{"low above band", Rule{Point: "d:p", Low: ptr(10), Hysteresis: 2}, 12, false, false},
		{"dual inside", Rule{Point: "d:p", Low: ptr(10), High: ptr(20), Hysteresis: 2}, 15, false, false},
		{"dual high in band", Rule{Point: "d:p", Low: ptr(10), High: ptr(20), Hysteresis: 2}, 19, false, true},
		{"dual low in band", Rule{Point: "d:p", Low: ptr(10), High: ptr(20), Hysteresis: 2}, 11, false, true},
		{"dual high out", Rule{Point: "d:p", Low: ptr(10), High: ptr(20), Hysteresis: 2}, 21, true, true},
		{"dual low out", Rule{Point: "d:p", Low: ptr(10), High: ptr(20), Hysteresis: 2}, 9, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.rule
			r.ID, r.Severity = "r", SeverityInfo
			c, err := compile(r)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.trigger(tc.v); got != tc.trigger {
				t.Fatalf("trigger(%v) = %v, want %v", tc.v, got, tc.trigger)
			}
			if got := c.holds(tc.v); got != tc.hold {
				t.Fatalf("holds(%v) = %v, want %v", tc.v, got, tc.hold)
			}
		})
	}
}

func TestStatusSnapshotPointersAreCopies(t *testing.T) {
	// 快照里的时间/值必须是指针副本；返回内部地址会让调用方在无锁情况下
	// 看到并发修改。
	e, _, src := newTestEngine(t, []Rule{{
		ID: "r", Severity: SeverityInfo, Point: "dev:v", High: ptr(1),
	}}, nil)
	src.set("dev", "v", 5, model.QualityGood)
	e.Sweep()
	st := stateOf(t, e, "r")
	if st.Value == nil || st.Since == nil {
		t.Fatal("expected value and since")
	}
	*st.Value = -1
	*st.Since = time.Time{}
	st2 := stateOf(t, e, "r")
	if st2.Value == nil || *st2.Value != 5 {
		t.Fatalf("status aliased internal value: %v", st2.Value)
	}
	if st2.Since == nil || st2.Since.IsZero() {
		t.Fatal("status aliased internal time")
	}
}

// jsonMarshal 避免在测试文件里再 import encoding/json（其它断言不需要它）。
