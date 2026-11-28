package computed

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

func point(id, expr string, dt model.DataType) model.Point {
	return model.Point{
		ID: id, DataType: dt, Enabled: true,
		Address: model.Address{Kind: model.AddressComputed, Expr: expr},
	}
}

func open(t *testing.T, dev model.Device, lookup func(string, string) (model.Sample, bool)) driver.Conn {
	t.Helper()
	return openWithHistory(t, dev, lookup, nil)
}

func openWithHistory(t *testing.T, dev model.Device, lookup func(string, string) (model.Sample, bool),
	history func(ref string, window time.Duration) (model.WindowStats, bool)) driver.Conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Device: dev, Lookup: lookup, History: history,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func span(ids ...string) []driver.ReadSpan {
	pts := make([]driver.PointRef, len(ids))
	for i, id := range ids {
		pts[i] = driver.PointRef{ID: id}
	}
	return []driver.ReadSpan{{ID: "expr", Points: pts}}
}

func read(t *testing.T, c driver.Conn, ids ...string) map[string]driver.RawSample {
	t.Helper()
	raws, err := c.Read(context.Background(), span(ids...))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := make(map[string]driver.RawSample, len(raws))
	for _, r := range raws {
		out[r.PointID] = r
	}
	return out
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "computed" {
		t.Fatalf("id = %q", m.ID)
	}
	if got := m.Capabilities.Write; got != driver.CapUnsupported {
		t.Fatalf("write capability = %s (computed points are read-only)", got)
	}
	if got := m.Capabilities.Read; got != driver.CapSupported {
		t.Fatalf("read capability = %s", got)
	}
	if len(m.AddressKinds) != 1 || m.AddressKinds[0] != model.AddressComputed {
		t.Fatalf("address kinds = %v", m.AddressKinds)
	}
	if !m.Accepts(model.AddressComputed) {
		t.Fatal("meta must accept computed addressing")
	}
}

// 同一设备内的依赖必须在**一次 Read 内**闭合：mid 用的是本次的 base，
// 而不是缓存里上一轮的 base。用不断变化的输入才能区分这两种实现。
func TestReadEvaluatesDependenciesInOrder(t *testing.T) {
	var call int
	lookup := func(dev, pt string) (model.Sample, bool) {
		if dev != "src" || pt != "x" {
			return model.Sample{}, false
		}
		call++
		// 每轮返回不同的值：如果依赖是按上一轮算的，链式结果就对不上。
		return model.Sample{DeviceID: dev, PointID: pt,
			Value: model.NumValue(model.TypeFloat64, float64(call)), Quality: model.QualityGood}, true
	}
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("base", "src:x * 2", model.TypeFloat64),
		point("mid", "kpi:base + 1", model.TypeFloat64),
		point("top", "kpi:mid * 10", model.TypeFloat64),
	}}
	c := open(t, dev, lookup)

	got := read(t, c, "base", "mid", "top")
	if got["base"].Value.Num != 2 || got["mid"].Value.Num != 3 || got["top"].Value.Num != 30 {
		t.Fatalf("chain = base:%v mid:%v top:%v, want 2/3/30",
			got["base"].Value.Num, got["mid"].Value.Num, got["top"].Value.Num)
	}
	for id, r := range got {
		if r.Quality != model.QualityGood {
			t.Fatalf("%s quality = %s", id, r.Quality)
		}
	}

	// 第二轮：输入变了，链式结果必须整体跟着变。
	got = read(t, c, "base", "mid", "top")
	if got["base"].Value.Num != 4 || got["mid"].Value.Num != 5 || got["top"].Value.Num != 50 {
		t.Fatalf("second round = %v/%v/%v", got["base"].Value.Num, got["mid"].Value.Num, got["top"].Value.Num)
	}
}

// 只请求一个点位时，它依赖的中间点位仍要被求值。
func TestReadEvaluatesUnrequestedDependencies(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("base", "2 + 3", model.TypeFloat64),
		point("top", "kpi:base * 2", model.TypeFloat64),
	}}
	c := open(t, dev, nil)
	got := read(t, c, "top")
	if len(got) != 1 {
		t.Fatalf("returned %d points, want only the requested one", len(got))
	}
	if got["top"].Value.Num != 10 {
		t.Fatalf("top = %v", got["top"].Value.Num)
	}
}

// 输入是 uncertain（写入后尚未回读确认）时，计算点可以算，但结果也只能是
// uncertain——算式没问题不代表输入已确认。
func TestQualityPropagatesUncertain(t *testing.T) {
	lookup := func(dev, pt string) (model.Sample, bool) {
		return model.Sample{DeviceID: dev, PointID: pt,
			Value: model.NumValue(model.TypeFloat64, 2), Quality: model.QualityUncertain}, true
	}
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("v", "src:x * 3", model.TypeFloat64),
	}}
	got := read(t, open(t, dev, lookup), "v")["v"]
	if got.Quality != model.QualityUncertain {
		t.Fatalf("quality = %s, want uncertain", got.Quality)
	}
	if got.Value.Num != 6 {
		t.Fatalf("value = %v", got.Value.Num)
	}
}

// 输入不可用时不计算：宁可没有值，也不要拿 0 算出一个「看起来正常」的数字。
func TestQualityPropagatesUnusable(t *testing.T) {
	for _, q := range []model.Quality{
		model.QualityStale, model.QualityTimeout, model.QualityCommError, model.QualityAddressError,
	} {
		lookup := func(dev, pt string) (model.Sample, bool) {
			return model.Sample{DeviceID: dev, PointID: pt,
				Value: model.NumValue(model.TypeFloat64, 2), Quality: q}, true
		}
		dev := model.Device{ID: "kpi", Points: []model.Point{
			point("v", "src:x * 3", model.TypeFloat64),
		}}
		got := read(t, open(t, dev, lookup), "v")["v"]
		if got.Quality != q {
			t.Fatalf("quality = %s, want %s", got.Quality, q)
		}
		if got.Value.Num != 0 && !got.Value.Type.IsNumeric() {
			t.Fatalf("unexpected value %+v", got.Value)
		}
		if got.Value.Num != 0 {
			t.Fatalf("value must be empty when inputs are unusable, got %v", got.Value.Num)
		}
		if got.Err == nil || !strings.Contains(got.Err.Error(), "src:x") {
			t.Fatalf("error must name the bad input: %v", got.Err)
		}
	}
}

func TestMissingInput(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("v", "src:nope + 1", model.TypeFloat64),
	}}
	// 完全没有 Lookup：必须按「没有值」处理，而不是 panic。
	got := read(t, open(t, dev, nil), "v")["v"]
	if got.Quality != model.QualityBad {
		t.Fatalf("quality = %s", got.Quality)
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), "no value") {
		t.Fatalf("error = %v", got.Err)
	}
}

// 环在 Open 期必须报错：否则会按 map 顺序求值，表现为「有时好有时坏」。
func TestCycleDetectedAtOpen(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("a", "kpi:b + 1", model.TypeFloat64),
		point("b", "kpi:a + 1", model.TypeFloat64),
	}}
	_, err := New().Open(context.Background(), driver.OpenConfig{Device: dev})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle must be rejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "a -> b -> a") && !strings.Contains(err.Error(), "b -> a -> b") {
		t.Fatalf("error should show the cycle path: %v", err)
	}
}

func TestSelfReferenceIsCycle(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("a", "kpi:a + 1", model.TypeFloat64),
	}}
	if _, err := New().Open(context.Background(), driver.OpenConfig{Device: dev}); err == nil {
		t.Fatal("self reference must be rejected")
	}
}

func TestBadExpressionAtOpen(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("a", "1 +", model.TypeFloat64),
	}}
	_, err := New().Open(context.Background(), driver.OpenConfig{Device: dev})
	if err == nil || !strings.Contains(err.Error(), `point "a"`) {
		t.Fatalf("bad expression must be rejected with the point id, got %v", err)
	}
}

func TestOpenWithoutComputedPoints(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{Device: model.Device{ID: "kpi"}})
	if err == nil {
		t.Fatal("a device without computed points must be rejected")
	}
}

func TestWriteIsRejected(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("v", "1 + 1", model.TypeFloat64),
	}}
	c := open(t, dev, nil)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{PointID: "v"}})
	if err != nil {
		t.Fatalf("write must fail per item, not as a call: %v", err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("write result = %+v", res)
	}
	if kind := driver.KindOf(res[0].Err); kind != driver.ErrPermission {
		t.Fatalf("error kind = %s, want permission", kind)
	}
}

func TestProbeAndClose(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{point("v", "1", model.TypeFloat64)}}
	c := open(t, dev, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// 已取消的 context 必须让 Read 立刻返回，而不是继续算。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Read(ctx, span("v")); err == nil {
		t.Fatal("cancelled context must abort Read")
	}
}

func TestReadWithoutSpans(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{point("v", "1", model.TypeFloat64)}}
	c := open(t, dev, nil)
	raws, err := c.Read(context.Background(), nil)
	if err != nil || len(raws) != 0 {
		t.Fatalf("empty read = %v, %v", raws, err)
	}
	// span 里出现不属于本设备的点位（配置错误/热更新竞态）：忽略即可，
	// 采集层会把「没返回的点位」显式降级。
	raws, err = c.Read(context.Background(), span("nope"))
	if err != nil || len(raws) != 0 {
		t.Fatalf("unknown point read = %v, %v", raws, err)
	}
}

func TestEncode(t *testing.T) {
	cases := []struct {
		dt    model.DataType
		v     float64
		want  float64
		wantB bool
		isB   bool
		bad   bool
	}{
		{dt: model.TypeFloat64, v: 1.5, want: 1.5},
		{dt: model.TypeFloat32, v: 1.5, want: 1.5},
		{dt: model.TypeInt16, v: 2.7, want: 3},   // 四舍五入
		{dt: model.TypeInt16, v: -2.7, want: -3}, // 远离零
		{dt: model.TypeUint16, v: 65535, want: 65535},
		{dt: model.TypeUint16, v: 70000, bad: true}, // 越界判坏，不截断
		{dt: model.TypeUint16, v: -1, bad: true},
		{dt: model.TypeInt32, v: 2147483648, bad: true},
		// 64 位上界是 float64 的取舍边界：MaxInt64/MaxUint64 无法精确表示，
		// 会进位到 2^63/2^64。恰为该值的输入必须判坏，否则下游 int64(v.Num)
		// 会把它裁剪成 MinInt64（见回归用例与 Value.String 的同款严判）。
		{dt: model.TypeInt64, v: 9223372036854775808, bad: true},                   // 2^63
		{dt: model.TypeUint64, v: 18446744073709551616, bad: true},                 // 2^64
		{dt: model.TypeInt64, v: -9223372036854775808, want: -9223372036854775808}, // MinInt64 合法
		{dt: model.TypeBool, v: 0, isB: true, wantB: false},
		{dt: model.TypeBool, v: 0.1, isB: true, wantB: true},
	}
	for _, c := range cases {
		got, err := encode(c.dt, c.v)
		if c.bad {
			if err == nil {
				t.Errorf("encode(%s, %v) should fail", c.dt, c.v)
			}
			continue
		}
		if err != nil {
			t.Errorf("encode(%s, %v) = %v", c.dt, c.v, err)
			continue
		}
		if got.Type != c.dt {
			t.Errorf("encode(%s, %v) type = %s", c.dt, c.v, got.Type)
		}
		if c.isB {
			if got.Bool != c.wantB {
				t.Errorf("encode(%s, %v) = %v", c.dt, c.v, got.Bool)
			}
			continue
		}
		if math.Abs(got.Num-c.want) > 1e-9 {
			t.Errorf("encode(%s, %v) = %v, want %v", c.dt, c.v, got.Num, c.want)
		}
	}

	if _, err := encode(model.TypeFloat64, math.Inf(1)); err == nil {
		t.Error("inf must be rejected")
	}
	if _, err := encode(model.TypeString, 1); err == nil {
		t.Error("string data type must be rejected")
	}
	if _, err := encode(model.TypeFloat32, 1e300); err == nil {
		t.Error("float32 overflow must be rejected")
	}
}

// 溢出的计算结果必须判坏：把 Inf 当正常值传下去，报表里会出现「无穷大」。
func TestOverflowBecomesBad(t *testing.T) {
	lookup := func(dev, pt string) (model.Sample, bool) {
		return model.Sample{DeviceID: dev, PointID: pt,
			Value: model.NumValue(model.TypeFloat64, 1e308), Quality: model.QualityGood}, true
	}
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("v", "src:x * 10", model.TypeFloat64),
	}}
	got := read(t, open(t, dev, lookup), "v")["v"]
	if got.Quality != model.QualityBad {
		t.Fatalf("quality = %s", got.Quality)
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), "overflow") {
		t.Fatalf("error = %v", got.Err)
	}
}

func TestDivisionByZeroBecomesBad(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("v", "1 / 0", model.TypeFloat64),
	}}
	got := read(t, open(t, dev, nil), "v")["v"]
	if got.Quality != model.QualityBad {
		t.Fatalf("quality = %s", got.Quality)
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), "division by zero") {
		t.Fatalf("error = %v", got.Err)
	}
}

func TestWorse(t *testing.T) {
	if got := worse(model.QualityGood, model.QualityUncertain); got != model.QualityUncertain {
		t.Fatalf("worse(good, uncertain) = %s", got)
	}
	if got := worse(model.QualityTimeout, model.QualityStale); got != model.QualityTimeout {
		t.Fatalf("worse(timeout, stale) = %s", got)
	}
	if got := worse(model.QualityBad, model.QualityCommError); got != model.QualityBad {
		t.Fatalf("worse(bad, comm_error) = %s", got)
	}
}

func TestSplitRef(t *testing.T) {
	cases := []struct {
		in      string
		dev, pt string
		ok      bool
	}{
		{"a:b", "a", "b", true},
		{"line-a:power-1", "line-a", "power-1", true},
		{"nocolon", "", "", false},
		{":b", "", "", false},
		{"a:", "", "", false},
	}
	for _, c := range cases {
		dev, pt, ok := splitRef(c.in)
		if dev != c.dev || pt != c.pt || ok != c.ok {
			t.Errorf("splitRef(%q) = %q,%q,%v", c.in, dev, pt, ok)
		}
	}
}

// 窗口聚合的输入来自历史，不走 cache；质量规则也与普通引用不同：
// 历史不可用时整个点位是 bad（而不是「某个输入坏了」）。
func TestWindowAggregate(t *testing.T) {
	var gotRef string
	var gotWindow time.Duration
	history := func(ref string, window time.Duration) (model.WindowStats, bool) {
		gotRef, gotWindow = ref, window
		return model.WindowStats{Count: 2, Sum: 10, Min: 4, Max: 6}, true
	}
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("avg5", "avg(src:x, 5m)", model.TypeFloat64),
		point("count5", "count(src:x, 5m)", model.TypeFloat64),
		point("mixed", "src:x - avg(src:x, 5m)", model.TypeFloat64),
	}}
	lookup := func(dev, pt string) (model.Sample, bool) {
		return model.Sample{DeviceID: dev, PointID: pt,
			Value: model.NumValue(model.TypeFloat64, 20), Quality: model.QualityGood}, true
	}
	c := openWithHistory(t, dev, lookup, history)
	got := read(t, c, "avg5", "count5", "mixed")
	if got["avg5"].Value.Num != 5 {
		t.Fatalf("avg5 = %v", got["avg5"].Value.Num)
	}
	if got["count5"].Value.Num != 2 {
		t.Fatalf("count5 = %v", got["count5"].Value.Num)
	}
	if got["mixed"].Value.Num != 15 {
		t.Fatalf("mixed = %v", got["mixed"].Value.Num)
	}
	for id, r := range got {
		if r.Quality != model.QualityGood {
			t.Fatalf("%s quality = %s (%v)", id, r.Quality, r.Err)
		}
	}
	if gotRef != "src:x" || gotWindow != 5*time.Minute {
		t.Fatalf("history called with %q/%v", gotRef, gotWindow)
	}
}

func TestWindowAggregateFailures(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("w", "avg(src:x, 5m)", model.TypeFloat64),
	}}
	cases := []struct {
		name    string
		history func(string, time.Duration) (model.WindowStats, bool)
		want    string
	}{
		{"no history source", nil, "history is not available"},
		{"history unavailable", func(string, time.Duration) (model.WindowStats, bool) {
			return model.WindowStats{}, false
		}, "history is unavailable"},
		{"empty window", func(string, time.Duration) (model.WindowStats, bool) {
			return model.WindowStats{}, true
		}, "no good samples"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := openWithHistory(t, dev, nil, tc.history)
			got := read(t, c, "w")
			s := got["w"]
			if s.Quality != model.QualityBad {
				t.Fatalf("quality = %s, want bad", s.Quality)
			}
			if s.Err == nil || !strings.Contains(s.Err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", s.Err, tc.want)
			}
		})
	}
}

// 窗口引用不算「当前值依赖」：base 读 mid 的过去，mid 读 base 的现在，
// 不是环。如果实现把它们当依赖，Open 会以 dependency cycle 失败。
func TestWindowReferenceIsNotACurrentValueDependency(t *testing.T) {
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("base", "avg(kpi:mid, 5m)", model.TypeFloat64),
		point("mid", "kpi:base + 1", model.TypeFloat64),
	}}
	history := func(string, time.Duration) (model.WindowStats, bool) {
		return model.WindowStats{Count: 1, Sum: 4, Min: 4, Max: 4}, true
	}
	c := openWithHistory(t, dev, nil, history)
	got := read(t, c, "base", "mid")
	if got["base"].Value.Num != 4 || got["mid"].Value.Num != 5 {
		t.Fatalf("base=%v mid=%v", got["base"].Value.Num, got["mid"].Value.Num)
	}
}
