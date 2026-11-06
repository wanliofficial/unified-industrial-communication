package history

import (
	"errors"
	"testing"
	"time"

	"uic/internal/model"
)

// 窗口统计的三个过滤条件（点位、质量、类型）必须一次性钉住：少任何一个，
// 结果都会变成「看起来合理但完全错误」的数字。
func TestWindowStatsAggregatesGoodNumericSamples(t *testing.T) {
	s, clock := openTest(t, t.TempDir(), Options{})
	now := clock.Now()

	mustWrite(t, s, numRec(now.Add(-4*time.Minute), "press", "power", 10))
	mustWrite(t, s, numRec(now.Add(-3*time.Minute), "press", "power", 20))
	mustWrite(t, s, numRec(now.Add(-1*time.Minute), "press", "power", 30))
	// 窗口外：不能被算进来。
	mustWrite(t, s, numRec(now.Add(-10*time.Minute), "press", "power", 100))

	other := numRec(now.Add(-2*time.Minute), "press", "power", 999)
	other.Quality = model.QualityStale
	mustWrite(t, s, other)

	bad := numRec(now.Add(-2*time.Minute), "press", "power", 888)
	bad.Quality = model.QualityBad
	mustWrite(t, s, bad)

	uncertain := numRec(now.Add(-2*time.Minute), "press", "power", 777)
	uncertain.Quality = model.QualityUncertain
	mustWrite(t, s, uncertain)

	// 非数值类型（bool / string）不参与统计。
	mustWrite(t, s, Record{Time: now.Add(-time.Minute), Device: "press", Point: "power",
		Value: model.BoolValue(true), Quality: model.QualityGood})
	mustWrite(t, s, Record{Time: now.Add(-time.Minute), Device: "press", Point: "power",
		Value: model.TextValue(model.TypeString, "42"), Quality: model.QualityGood})

	// 其他点位/设备。
	mustWrite(t, s, numRec(now.Add(-time.Minute), "press", "temp", 70))
	mustWrite(t, s, numRec(now.Add(-time.Minute), "line", "power", 80))

	got, err := s.WindowStats("press", "power", 5*time.Minute)
	if err != nil {
		t.Fatalf("WindowStats: %v", err)
	}
	if got.Count != 3 || got.Sum != 60 || got.Min != 10 || got.Max != 30 {
		t.Fatalf("stats = %+v, want count=3 sum=60 min=10 max=30", got)
	}
	if avg := got.Avg(); avg != 20 {
		t.Fatalf("avg = %v, want 20", avg)
	}
}

func TestWindowStatsEmptyAndInvalid(t *testing.T) {
	s, clock := openTest(t, t.TempDir(), Options{})
	now := clock.Now()
	mustWrite(t, s, numRec(now.Add(-time.Hour), "a", "b", 1))

	got, err := s.WindowStats("a", "b", 5*time.Minute)
	if err != nil {
		t.Fatalf("empty window must not fail: %v", err)
	}
	if got.Count != 0 {
		t.Fatalf("stats = %+v, want empty", got)
	}
	// Count=0 时 Avg 必须是 0 而不是 NaN。
	if got.Avg() != 0 {
		t.Fatalf("empty Avg = %v", got.Avg())
	}

	if _, err := s.WindowStats("a", "b", 0); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("zero window: %v", err)
	}
	if _, err := s.WindowStats("", "b", time.Minute); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("empty device: %v", err)
	}
	if _, err := s.WindowStats("a", "", time.Minute); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("empty point: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.WindowStats("a", "b", time.Minute); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed store: %v", err)
	}
}

// 窗口是相对历史库自己的时钟，不是相对「最后一次写入」。假时钟推进后
// 旧样本必须自然退出窗口——这条用真实时钟只能靠 sleep 验证。
func TestWindowStatsFollowsClock(t *testing.T) {
	s, clock := openTest(t, t.TempDir(), Options{})
	mustWrite(t, s, numRec(clock.Now(), "a", "b", 5))

	clock.Advance(4 * time.Minute)
	if got, err := s.WindowStats("a", "b", 5*time.Minute); err != nil || got.Count != 1 {
		t.Fatalf("within window: %+v, %v", got, err)
	}

	clock.Advance(2 * time.Minute)
	got, err := s.WindowStats("a", "b", 5*time.Minute)
	if err != nil {
		t.Fatalf("after advance: %v", err)
	}
	if got.Count != 0 {
		t.Fatalf("stats after window = %+v, want empty", got)
	}
}

// 边界：from/to 都是闭区间。恰好落在 now-window 上的样本算在窗口内，
// 落到下一纳秒就算窗口外——这样「5m 窗口」在整分钟采样下能稳定包含 5 个点。
func TestWindowStatsBoundariesInclusive(t *testing.T) {
	s, clock := openTest(t, t.TempDir(), Options{})
	now := clock.Now()
	mustWrite(t, s, numRec(now.Add(-5*time.Minute), "a", "b", 1))
	mustWrite(t, s, numRec(now.Add(-5*time.Minute-time.Nanosecond), "a", "b", 2))
	mustWrite(t, s, numRec(now, "a", "b", 3))

	got, err := s.WindowStats("a", "b", 5*time.Minute)
	if err != nil {
		t.Fatalf("WindowStats: %v", err)
	}
	if got.Count != 2 || got.Min != 1 || got.Max != 3 {
		t.Fatalf("stats = %+v, want the two boundary samples", got)
	}
}
