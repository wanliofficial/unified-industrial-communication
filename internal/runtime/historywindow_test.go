package runtime

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"uic/internal/history"
	"uic/internal/model"
	"uic/internal/obs"
)

// hwClock 是可推进的假时钟。窗口缓存的所有判定（TTL、过期重查）都基于
// 时间，真实时钟只能靠 sleep 验证，既慢又不稳。
type hwClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hwClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *hwClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// hwSource 是可控的查询替身：记录调用次数，并返回预设结果。
type hwSource struct {
	mu    sync.Mutex
	calls int
	stats model.WindowStats
	err   error
}

func (s *hwSource) query(device, point string, window time.Duration) (model.WindowStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.stats, s.err
}

func (s *hwSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newTestWindow(t *testing.T, src *hwSource, clock *hwClock) *historyWindow {
	t.Helper()
	w := newHistoryWindow(src.query,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		obs.New())
	w.now = clock.Now
	return w
}

// TTL 内重复 lookup 只能真正查询一次；过期后必须重查。
// 这是窗口聚合能在报警 200ms 扫描下不打满磁盘的唯一保证。
func TestHistoryWindowCachesWithinTTL(t *testing.T) {
	clock := &hwClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	src := &hwSource{stats: model.WindowStats{Count: 2, Sum: 10, Min: 4, Max: 6}}
	w := newTestWindow(t, src, clock)

	st, ok := w.lookup("dev:pt", 5*time.Minute)
	if !ok || st.Count != 2 {
		t.Fatalf("lookup = %+v, %v", st, ok)
	}
	// 5m 窗口 TTL = 5s。
	clock.Advance(4 * time.Second)
	if _, ok := w.lookup("dev:pt", 5*time.Minute); !ok {
		t.Fatal("second lookup must succeed from cache")
	}
	if n := src.count(); n != 1 {
		t.Fatalf("queries = %d, want 1 (cache hit)", n)
	}

	// 跨过 TTL 后必须重查。
	clock.Advance(2 * time.Second)
	if _, ok := w.lookup("dev:pt", 5*time.Minute); !ok {
		t.Fatal("third lookup must succeed")
	}
	if n := src.count(); n != 2 {
		t.Fatalf("queries = %d, want 2 (expired)", n)
	}
}

// TTL 的上下界：1s 窗口不能一秒内反复扫盘，24h 窗口也不能缓存半小时。
func TestWindowTTLBounds(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{time.Second, time.Second},      // 下界
		{30 * time.Second, time.Second}, // 30s/60 = 500ms → 夹到 1s
		{5 * time.Minute, 5 * time.Second},
		{10 * time.Minute, 10 * time.Second},
		{time.Hour, 30 * time.Second}, // 上界
		{24 * time.Hour, 30 * time.Second},
	}
	for _, c := range cases {
		if got := ttlFor(c.window); got != c.want {
			t.Errorf("ttlFor(%s) = %s, want %s", c.window, got, c.want)
		}
	}
}

// 空窗口是「数据还没积累够」的暂态：TTL 压到 1s，否则刚启动的计算点
// 会白等一整个 TTL 才开始出值。
func TestHistoryWindowEmptyUsesShortTTL(t *testing.T) {
	clock := &hwClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	src := &hwSource{} // Count=0
	w := newTestWindow(t, src, clock)

	if st, ok := w.lookup("dev:pt", 5*time.Minute); !ok || st.Count != 0 {
		t.Fatalf("lookup = %+v, %v; empty window is a successful result", st, ok)
	}
	// 1s 内仍然命中缓存。
	clock.Advance(900 * time.Millisecond)
	if _, ok := w.lookup("dev:pt", 5*time.Minute); !ok {
		t.Fatal("lookup must succeed")
	}
	if n := src.count(); n != 1 {
		t.Fatalf("queries = %d, want 1", n)
	}
	// 1s 后重查，而不是等完整的 5s。
	clock.Advance(200 * time.Millisecond)
	if _, ok := w.lookup("dev:pt", 5*time.Minute); !ok {
		t.Fatal("lookup must succeed")
	}
	if n := src.count(); n != 2 {
		t.Fatalf("queries = %d, want 2 after 1s", n)
	}
}

// 错误必须缓存：磁盘故障期间每 200ms 重试一次只会让故障更严重。
// 同时 ok=false 是对外语义（调用方据此把点位置 bad），不能吞掉。
func TestHistoryWindowCachesErrors(t *testing.T) {
	clock := &hwClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	src := &hwSource{err: errors.New("disk on fire")}
	w := newTestWindow(t, src, clock)

	if _, ok := w.lookup("dev:pt", 5*time.Minute); ok {
		t.Fatal("failed query must report ok=false")
	}
	if _, ok := w.lookup("dev:pt", 5*time.Minute); ok {
		t.Fatal("cached failure must report ok=false")
	}
	if n := src.count(); n != 1 {
		t.Fatalf("queries = %d, want 1 (errors are cached)", n)
	}
	// 故障恢复后，TTL 到期必须重新尝试并返回成功。
	src.mu.Lock()
	src.err = nil
	src.stats = model.WindowStats{Count: 1, Sum: 3, Min: 3, Max: 3}
	src.mu.Unlock()
	clock.Advance(6 * time.Second)
	st, ok := w.lookup("dev:pt", 5*time.Minute)
	if !ok || st.Count != 1 {
		t.Fatalf("after recovery = %+v, %v", st, ok)
	}
}

// 非法引用在配置期就被挡住了；直接调用时不能去查历史（那会白扫一遍盘），
// 也不能 panic。
func TestHistoryWindowInvalidRefDoesNotQuery(t *testing.T) {
	clock := &hwClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	src := &hwSource{stats: model.WindowStats{Count: 1}}
	w := newTestWindow(t, src, clock)

	for _, ref := range []string{"", "nocolon", ":pt", "dev:"} {
		if _, ok := w.lookup(ref, time.Minute); ok {
			t.Errorf("lookup(%q) must fail", ref)
		}
	}
	if n := src.count(); n != 0 {
		t.Fatalf("queries = %d, want 0 for invalid refs", n)
	}
}

// 不同窗口是不同缓存键：同一测点取 1m 与 5m 不能互相污染。
func TestHistoryWindowSeparatesWindows(t *testing.T) {
	clock := &hwClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	src := &hwSource{stats: model.WindowStats{Count: 1, Sum: 1, Min: 1, Max: 1}}
	w := newTestWindow(t, src, clock)

	if _, ok := w.lookup("dev:pt", time.Minute); !ok {
		t.Fatal("1m lookup failed")
	}
	if _, ok := w.lookup("dev:pt", 5*time.Minute); !ok {
		t.Fatal("5m lookup failed")
	}
	if n := src.count(); n != 2 {
		t.Fatalf("queries = %d, want 2 (distinct windows)", n)
	}
}

// 启动顺序：openAlarms 早于 openHistory。适配器必须在每次查询时读取
// r.hist，而不是构造时捕获——否则历史打开后窗口仍永久不可用。
func TestHistoryWindowSourceSeesLateStore(t *testing.T) {
	dir := t.TempDir()
	rt := &Runtime{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics: obs.New(),
	}
	rt.win = rt.newHistoryWindowSource()

	if _, ok := rt.historyWindow("dev:pt", time.Minute); ok {
		t.Fatal("without a store the adapter must report ok=false")
	}

	store, err := history.Open(history.Options{Dir: dir, Log: rt.log})
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Write(history.Record{
		Time: time.Now(), Device: "dev", Point: "pt",
		Value: model.NumValue(model.TypeFloat64, 7), Quality: model.QualityGood,
	}); err != nil {
		t.Fatalf("write history: %v", err)
	}
	rt.hist = store

	st, ok := rt.historyWindow("dev:pt", time.Minute)
	if !ok {
		t.Fatal("adapter must see the store opened after construction")
	}
	if st.Count != 1 || st.Sum != 7 {
		t.Fatalf("stats = %+v", st)
	}
	// calc 适配器只是转换类型，语义一致。
	cst, ok := rt.calcWindow("dev:pt", time.Minute)
	if !ok || cst.Count != 1 || cst.Sum != 7 {
		t.Fatalf("calcWindow = %+v, %v", cst, ok)
	}
}
