package watchdog

import (
	"strings"
	"testing"
	"time"
)

// Evaluate 的三态语义：healthy / stalled / unknown。
func TestEvaluateHealthy(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-2 * time.Second)
	st := Evaluate(now, last, true, Options{StallAfter: 15 * time.Second})
	if st.Verdict != VerdictHealthy {
		t.Fatalf("verdict = %q, want healthy", st.Verdict)
	}
	if st.Age != 2*time.Second {
		t.Fatalf("age = %v, want 2s", st.Age)
	}
	if st.StallAfter != 15*time.Second {
		t.Fatalf("stallAfter = %v, want 15s", st.StallAfter)
	}
}

func TestEvaluateStalled(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-20 * time.Second)
	st := Evaluate(now, last, true, Options{StallAfter: 15 * time.Second})
	if st.Verdict != VerdictStalled {
		t.Fatalf("verdict = %q, want stalled", st.Verdict)
	}
	if st.Age != 20*time.Second {
		t.Fatalf("age = %v, want 20s", st.Age)
	}
	if !strings.Contains(st.Detail, "overdue") {
		t.Fatalf("detail = %q, want mention overdue", st.Detail)
	}
}

// 边界：恰好等于阈值不算卡死（严格大于才判 stalled）。这条边界必须锁死，
// 否则「15s 阈值」在 15.000s 时到底是健康还是卡死会随实现漂移。
func TestEvaluateBoundary(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-15 * time.Second)
	st := Evaluate(now, last, true, Options{StallAfter: 15 * time.Second})
	if st.Verdict != VerdictHealthy {
		t.Fatalf("age == stallAfter 应判 healthy, got %q", st.Verdict)
	}
	// 超一毫秒就卡死。
	st = Evaluate(now, last.Add(-time.Millisecond), true, Options{StallAfter: 15 * time.Second})
	if st.Verdict != VerdictStalled {
		t.Fatalf("age > stallAfter 应判 stalled, got %q", st.Verdict)
	}
}

// 调度器尚未 tick：返回 unknown 而不是 stalled（刚启动不能误报卡死）。
func TestEvaluateUnknown(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := Evaluate(now, time.Time{}, false, Options{StallAfter: 15 * time.Second})
	if st.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want unknown", st.Verdict)
	}
	if st.StallAfter != 15*time.Second {
		t.Fatalf("stallAfter = %v, want echo of 15s", st.StallAfter)
	}
}

// 时钟回拨（lastTick 在未来）：保守当作 age=0，不因此误判卡死。
func TestEvaluateClockSkew(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(10 * time.Second) // 未来
	st := Evaluate(now, last, true, Options{StallAfter: 15 * time.Second})
	if st.Verdict != VerdictHealthy {
		t.Fatalf("负 age 应保守判 healthy, got %q", st.Verdict)
	}
	if st.Age != 0 {
		t.Fatalf("age = %v, want 0", st.Age)
	}
}

// 默认阈值生效：StallAfter<=0 用 DefaultStallAfter。
func TestEvaluateDefaultThreshold(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// 14s 在默认 15s 内 → healthy。
	st := Evaluate(now, now.Add(-14*time.Second), true, Options{})
	if st.StallAfter != DefaultStallAfter {
		t.Fatalf("stallAfter = %v, want default %v", st.StallAfter, DefaultStallAfter)
	}
	if st.Verdict != VerdictHealthy {
		t.Fatalf("14s 在默认阈值内应 healthy, got %q", st.Verdict)
	}
	// 16s 超默认阈值 → stalled。
	st = Evaluate(now, now.Add(-16*time.Second), true, Options{})
	if st.Verdict != VerdictStalled {
		t.Fatalf("16s 超默认阈值应 stalled, got %q", st.Verdict)
	}
}
