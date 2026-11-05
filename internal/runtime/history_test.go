package runtime

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/history"
	"uic/internal/model"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeHistorySample 造一个采样。CollectedAt 默认等于 at。
func fakeHistorySample(dev, pt string, at time.Time, v float64, q model.Quality) model.Sample {
	return model.Sample{
		DeviceID: dev, PointID: pt, Quality: q, CollectedAt: at,
		Value: model.NumValue(model.TypeFloat64, v),
	}
}

func TestHistoryWriterSampling(t *testing.T) {
	dir := t.TempDir()
	store, err := history.Open(history.Options{Dir: dir, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	w := newHistoryWriter(store)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := base
	w.now = func() time.Time { return now }
	p := historyPolicy{minInterval: time.Second, deadband: 0.5}

	write := func(off time.Duration, v float64, q model.Quality) bool {
		now = base.Add(off)
		return w.consider(fakeHistorySample("press", "power", now, v, q), p)
	}

	if !write(0, 10, model.QualityGood) {
		t.Fatal("the first sample must always be recorded")
	}
	// 同一时刻的重复值（缓存不会发，但 minInterval 到期后的 pending 会）。
	if write(0, 10, model.QualityGood) {
		t.Fatal("an unchanged value must not be recorded")
	}
	// 变化小于死区：曲线上的噪声不该进磁盘。
	if write(2*time.Second, 10.2, model.QualityGood) {
		t.Fatal("a change below the deadband must not be recorded")
	}
	// 超过死区。
	if !write(3*time.Second, 11, model.QualityGood) {
		t.Fatal("a change above the deadband must be recorded")
	}
	// 质量变化即使值没变也要记：坏数据的起点是最需要查的信息。
	if !write(5*time.Second, 11, model.QualityCommError) {
		t.Fatal("a quality change must be recorded")
	}
	// disabled 是显式关停，不是数据。
	if write(7*time.Second, 12, model.QualityDisabled) {
		t.Fatal("disabled points must not be recorded")
	}

	// minInterval 内的变化进 pending，tick 到期后落盘。上次落盘在 5s，
	// 所以 5.5s 的变化必须等，6s 才允许写。
	now = base.Add(5500 * time.Millisecond)
	if w.consider(fakeHistorySample("press", "power", now, 20, model.QualityGood), p) {
		t.Fatal("a change inside minInterval must wait")
	}
	if _, ok := w.pending["press/power"]; !ok {
		t.Fatal("the change must be pending, not dropped")
	}
	now = base.Add(5800 * time.Millisecond)
	if w.tick(p) {
		t.Fatal("pending must not be flushed before minInterval elapses")
	}
	now = base.Add(6 * time.Second)
	if !w.tick(p) {
		t.Fatal("pending must be flushed once minInterval elapses")
	}
	if _, ok := w.pending["press/power"]; ok {
		t.Fatal("pending entry must be cleared after a successful write")
	}

	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	res, err := store.Query(history.Query{Device: "press", Point: "power",
		From: base.Add(-time.Minute), To: base.Add(time.Minute), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 4 { // 10, 11, comm_error@11, 20
		var vals []float64
		for _, p := range res.Points {
			vals = append(vals, p.Value.Num)
		}
		t.Fatalf("count = %d values = %v", res.Count, vals)
	}
	// 时间戳必须单调（查询契约）。
	for i := 1; i < len(res.Points); i++ {
		if !res.Points[i].Time.After(res.Points[i-1].Time) {
			t.Fatalf("timestamps are not increasing: %+v", res.Points)
		}
	}
	if last := res.Points[len(res.Points)-1]; last.Value.Num != 20 {
		t.Fatalf("last point = %+v (the final value must not be lost)", last)
	}
}

func TestHistoryWriterHeartbeat(t *testing.T) {
	dir := t.TempDir()
	store, err := history.Open(history.Options{Dir: dir, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	w := newHistoryWriter(store)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := base
	w.now = func() time.Time { return now }
	p := historyPolicy{minInterval: time.Second, heartbeat: time.Minute}

	f := &config.File{Devices: []model.Device{{
		ID: "press", Enabled: true,
		Points: []model.Point{{ID: "power", Enabled: true}},
	}}}
	live := fakeHistorySample("press", "power", base, 42, model.QualityGood)
	get := func(dev, pt string) (model.Sample, bool) { return live, true }

	// 没有历史状态时，心跳直接把当前值记下来。
	if !w.heartbeat(f, p, now, get) {
		t.Fatal("the first heartbeat must record")
	}
	// 距上次写入还不到 heartbeat：不补点。
	now = base.Add(30 * time.Second)
	if w.heartbeat(f, p, now, get) {
		t.Fatal("heartbeat must respect its period")
	}
	// 到点补点。
	now = base.Add(61 * time.Second)
	live.CollectedAt = now
	if !w.heartbeat(f, p, now, get) {
		t.Fatal("heartbeat must record once the period elapses")
	}

	// 数据已经停了：CollectedAt 停在很久以前，补点等于伪造新鲜度。
	stale := live
	stale.CollectedAt = base
	get = func(dev, pt string) (model.Sample, bool) { return stale, true }
	now = base.Add(10 * time.Minute)
	if w.heartbeat(f, p, now, get) {
		t.Fatal("heartbeat must not invent points for a dead device")
	}

	// 停用的点位不补。
	f.Devices[0].Points[0].Enabled = false
	if w.heartbeat(f, p, now, get) {
		t.Fatal("disabled points must not be heartbeated")
	}
}

func TestHistoryRuntimeEndToEnd(t *testing.T) {
	h := newAlarmHarness(t, func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir":     cfg["storage"].(map[string]any)["dir"],
			"journal": false,
			"history": map[string]any{
				"enabled": true, "minInterval": "50ms", "heartbeat": "1s",
				"maxBytes": 4 << 20, "segmentBytes": 256 << 10,
			},
		}
	})

	h.waitPointGood("src", "power")
	h.writePoint("src", "power", 33)

	// 采样是后台进行的：断言要等到「至少两条记录」而不是「立刻能查到」。
	deadline := time.Now().Add(10 * time.Second)
	var res history.Result
	for {
		code, body := h.do(http.MethodGet,
			"/api/v1/history?device=src&point=power&from=-5m&limit=100", "")
		if code != 200 {
			t.Fatalf("history = %d %s", code, body)
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("history body: %v (%s)", err, body)
		}
		if res.Count >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if res.Count < 2 {
		t.Fatalf("history never recorded the value change: %+v", res)
	}
	if res.Device != "src" || res.Point != "power" || !res.From.Before(res.To) {
		t.Fatalf("result metadata = %+v", res)
	}

	// 聚合查询：1s 桶、取最后一个值。
	code, body := h.do(http.MethodGet,
		"/api/v1/history?device=src&point=power&from=-2m&step=1s&agg=last", "")
	if code != 200 {
		t.Fatalf("agg = %d %s", code, body)
	}
	var agg history.Result
	if err := json.Unmarshal(body, &agg); err != nil {
		t.Fatal(err)
	}
	if len(agg.Buckets) == 0 || agg.Step.Duration() != time.Second || agg.Agg != history.AggLast {
		t.Fatalf("buckets = %+v step = %s agg = %s", agg.Buckets, agg.Step, agg.Agg)
	}

	// 健康检查要能看出历史库在写。
	code, body = h.do(http.MethodGet, "/api/v1/health", "")
	if code != 200 {
		t.Fatalf("health = %d", code)
	}
	var health struct {
		History struct {
			Enabled  bool   `json:"enabled"`
			Records  uint64 `json:"records"`
			Segments int    `json:"segments"`
		} `json:"history"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if !health.History.Enabled || health.History.Records == 0 || health.History.Segments == 0 {
		t.Fatalf("health.history = %+v", health.History)
	}

	// 没有数据的点位返回空结果，而不是 404。
	code, body = h.do(http.MethodGet, "/api/v1/history?device=src&point=nope", "")
	if code != 200 {
		t.Fatalf("unknown point = %d %s", code, body)
	}

	// 桶数超限在参数层拒绝（否则一次查询会扫出几十万个桶）。
	code, body = h.do(http.MethodGet,
		"/api/v1/history?device=src&point=power&from=-24h&step=100ms", "")
	if code != 400 {
		t.Fatalf("too many buckets = %d %s", code, body)
	}
	if string(body) == "" || !containsAll(string(body), "history.invalid_query", "buckets") {
		t.Fatalf("body = %s", body)
	}

	// 配置说没开历史：409 而不是空结果——空结果会被误读成「这段时间没数据」。
	h2 := newAlarmHarness(t, nil)
	code, body = h2.do(http.MethodGet, "/api/v1/history?device=src&point=power", "")
	if code != 409 {
		t.Fatalf("disabled history = %d %s", code, body)
	}
	if !containsAll(string(body), "history.disabled") {
		t.Fatalf("body = %s", body)
	}
}

func TestHistoryRetentionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := clock
	opts := history.Options{Dir: dir, MaxBytes: 4096, SegmentBytes: 1024, Log: quietLog(),
		Now: func() time.Time { return now }}
	store, err := history.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		now = clock.Add(time.Duration(i) * time.Second)
		if err := store.Write(history.Record{Time: now, Device: "d", Point: "p",
			Value: model.NumValue(model.TypeFloat64, float64(i)), Quality: model.QualityGood}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启后：旧数据还在（受上限约束），新的写入能接上。
	store2, err := history.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if st := store2.Stats(); st.Bytes > 4096 || st.Segments == 0 {
		t.Fatalf("stats after restart = %+v", st)
	}
	now = clock.Add(time.Hour)
	if err := store2.Write(history.Record{Time: now, Device: "d", Point: "p",
		Value: model.NumValue(model.TypeFloat64, 999), Quality: model.QualityGood}); err != nil {
		t.Fatal(err)
	}
	// limit 必须足够大：保留策略淘汰的是最旧的段，最新一条一定在范围内，
	// 但默认 limit 会把它截断在执行窗口之外。
	res, err := store2.Query(history.Query{Device: "d", Point: "p",
		From: clock.Add(-time.Hour), To: now.Add(time.Minute), Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count == 0 || res.Points[len(res.Points)-1].Value.Num != 999 {
		t.Fatalf("points = %+v", res.Points)
	}
}

func TestHistoryFingerprint(t *testing.T) {
	base := config.History{Enabled: true, Dir: "/a", MaxBytes: 10, SegmentBytes: 5,
		MaxAge: model.Duration(time.Hour)}
	same := base
	if historyFingerprint(base) != historyFingerprint(same) {
		t.Fatal("identical policies must have identical fingerprints")
	}
	// 采样参数可以热改，不能进指纹（否则每次改死区都提示「需重启」）。
	same.MinInterval = model.Duration(time.Minute)
	same.Deadband = 3
	same.Heartbeat = nil
	if historyFingerprint(base) != historyFingerprint(same) {
		t.Fatal("sampling knobs must not affect the restart fingerprint")
	}
	for _, mutate := range []func(*config.History){
		func(h *config.History) { h.Dir = "/b" },
		func(h *config.History) { h.MaxBytes = 11 },
		func(h *config.History) { h.SegmentBytes = 6 },
		func(h *config.History) { h.MaxAge = model.Duration(2 * time.Hour) },
		func(h *config.History) { h.Enabled = false },
	} {
		h := base
		mutate(&h)
		if historyFingerprint(base) == historyFingerprint(h) {
			t.Fatalf("mutating %+v must change the fingerprint", h)
		}
	}
	if historyFingerprint(config.History{}) != "disabled" {
		t.Fatal("disabled policy must have a stable fingerprint")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
