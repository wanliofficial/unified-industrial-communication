package clockmon

import (
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func rd(wall time.Time, mono time.Duration) Reading { return Reading{Wall: wall, Mono: mono} }

func TestOffset(t *testing.T) {
	cases := []struct {
		name string
		prev Reading
		now  Reading
		want time.Duration
	}{
		{"steady", rd(t0, 0), rd(t0.Add(10*time.Second), 10*time.Second), 0},
		{"forward step", rd(t0, 0), rd(t0.Add(10*time.Second+1500*time.Millisecond), 10*time.Second), 1500 * time.Millisecond},
		{"backward step", rd(t0, 0), rd(t0.Add(10*time.Second-750*time.Millisecond), 10*time.Second), -750 * time.Millisecond},
		{"wall frozen while mono advances", rd(t0, 0), rd(t0, 4*time.Second), -4 * time.Second},
		{"wall runs at double speed", rd(t0, 0), rd(t0.Add(10*time.Second), 5*time.Second), 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Offset(tc.prev, tc.now); got != tc.want {
				t.Fatalf("Offset = %v, want %v", got, tc.want)
			}
		})
	}
}

// 墙钟换时区不等于时间跳变：两个读数用不同 Location 表示同一时刻时，偏移必须
// 仍然是 0。时区只影响显示，不影响绝对时刻。
func TestOffsetAcrossLocations(t *testing.T) {
	shanghai := time.FixedZone("CST", 8*3600)
	prev := rd(t0.In(shanghai), 0)
	now := rd(t0.Add(30*time.Second).In(time.UTC), 30*time.Second)
	if got := Offset(prev, now); got != 0 {
		t.Fatalf("timezone change reported as offset: %v", got)
	}
}

func TestDetectThreshold(t *testing.T) {
	cases := []struct {
		name      string
		step      time.Duration
		threshold time.Duration
		want      bool
	}{
		{"below default", 999 * time.Millisecond, 0, false},
		{"exactly default", time.Second, 0, true},
		{"exactly default backward", -time.Second, 0, true},
		{"below default backward", -999 * time.Millisecond, 0, false},
		{"custom above", 101 * time.Millisecond, 100 * time.Millisecond, true},
		{"custom exactly", 100 * time.Millisecond, 100 * time.Millisecond, true},
		{"custom below", 99 * time.Millisecond, 100 * time.Millisecond, false},
		{"large", 2 * time.Hour, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := rd(t0, 0)
			now := rd(t0.Add(10*time.Second+tc.step), 10*time.Second)
			j, ok := Detect(prev, now, tc.threshold)
			if ok != tc.want {
				t.Fatalf("Detect = %v, want %v (step %v)", ok, tc.want, tc.step)
			}
			if ok && j.Step != tc.step {
				t.Fatalf("jump step = %v, want %v", j.Step, tc.step)
			}
			if ok && !j.At.Equal(now.Wall) {
				t.Fatalf("jump At = %v, want the post-jump wall clock %v", j.At, now.Wall)
			}
		})
	}
}

func TestJumpDirectionAndSeconds(t *testing.T) {
	fwd := Jump{Step: 1500 * time.Millisecond}
	back := Jump{Step: -2 * time.Second}
	zero := Jump{}
	if fwd.Direction() != DirectionForward || back.Direction() != DirectionBackward {
		t.Fatalf("directions: %q %q", fwd.Direction(), back.Direction())
	}
	if zero.Direction() != DirectionForward {
		t.Fatalf("zero step direction = %q, want forward", zero.Direction())
	}
	if fwd.Seconds() != 1.5 || back.Seconds() != -2 {
		t.Fatalf("seconds: %v %v", fwd.Seconds(), back.Seconds())
	}
}

func TestMonitorUnknownBeforeFirstSample(t *testing.T) {
	m := New(0, nil)
	s := m.Snapshot()
	if s.Samples != 0 {
		t.Fatalf("samples = %d", s.Samples)
	}
	if s.Verdict() != VerdictUnknown {
		t.Fatalf("verdict = %q", s.Verdict())
	}
	if !s.SampledAt.IsZero() || s.Offset != 0 || s.LastJump != nil {
		t.Fatalf("unexpected state before first sample: %+v", s)
	}
	if s.Threshold != DefaultStepThreshold {
		t.Fatalf("threshold = %v, want default", s.Threshold)
	}
}

func TestMonitorFirstSampleNeverJumps(t *testing.T) {
	m := New(0, nil)
	if _, ok := m.Sample(rd(t0, 0)); ok {
		t.Fatal("the first sample has nothing to compare against and must not report a jump")
	}
	if got := m.Snapshot().Verdict(); got != VerdictOK {
		t.Fatalf("verdict = %q", got)
	}
}

func TestMonitorCountsAndLastJump(t *testing.T) {
	m := New(0, nil)
	m.Sample(rd(t0, 0))
	m.Sample(rd(t0.Add(10*time.Second), 10*time.Second))                                     // 平稳
	m.Sample(rd(t0.Add(20*time.Second+1500*time.Millisecond), 20*time.Second))               // 向前 1.5s
	m.Sample(rd(t0.Add(30*time.Second+1500*time.Millisecond), 30*time.Second))               // 平稳
	m.Sample(rd(t0.Add(40*time.Second+1500*time.Millisecond-2*time.Second), 40*time.Second)) // 回拨 2s

	s := m.Snapshot()
	if s.Samples != 5 {
		t.Fatalf("samples = %d", s.Samples)
	}
	if s.Forward != 1 || s.Backward != 1 {
		t.Fatalf("jumps forward=%d backward=%d", s.Forward, s.Backward)
	}
	if s.Verdict() != VerdictJumped {
		t.Fatalf("verdict = %q", s.Verdict())
	}
	if s.LastJump == nil || s.LastJump.Step != -2*time.Second {
		t.Fatalf("last jump = %+v", s.LastJump)
	}
	if s.LastJump.Direction() != DirectionBackward {
		t.Fatalf("last jump direction = %q", s.LastJump.Direction())
	}
	// 累计偏移 = 向后 2s + 向前 1.5s = -0.5s。
	if s.Offset != -500*time.Millisecond {
		t.Fatalf("cumulative offset = %v", s.Offset)
	}
	// 采样时刻是最近一次采样的墙钟。
	if !s.SampledAt.Equal(t0.Add(40*time.Second + 1500*time.Millisecond - 2*time.Second)) {
		t.Fatalf("sampledAt = %v", s.SampledAt)
	}
}

// 没有任何一次跳变，但墙钟每次都比单调钟多跑一点：这是「缓慢调谐/晶振漂移」，
// 累计超过阈值必须报 drifted——否则「时间已经偏了 3 秒但从来没跳变」就成了
// 监测的盲区。
func TestMonitorDriftWithoutJump(t *testing.T) {
	m := New(0, nil) // 阈值 1s
	m.Sample(rd(t0, 0))
	drift := time.Duration(0)
	for i := 1; i <= 12; i++ {
		drift += 100 * time.Millisecond // 每次间隔内偏 100ms，单次不达阈值
		m.Sample(rd(t0.Add(time.Duration(i)*10*time.Second+drift), time.Duration(i)*10*time.Second))
	}
	s := m.Snapshot()
	if s.Forward != 0 || s.Backward != 0 {
		t.Fatalf("drift must not be counted as a jump: %+v", s)
	}
	if s.Offset != 1200*time.Millisecond {
		t.Fatalf("offset = %v", s.Offset)
	}
	if s.Verdict() != VerdictDrifted {
		t.Fatalf("verdict = %q, want drifted", s.Verdict())
	}
}

// 回拨方向的漂移同样必须报 drifted：判据若漏掉负向（只判 offset >= threshold），
// 「墙钟越走越慢、已经落后 1.2 秒但从未跳变」就会被静默放过——那是监测半边瞎。
func TestMonitorDriftNegativeWithoutJump(t *testing.T) {
	m := New(0, nil)
	m.Sample(rd(t0, 0))
	drift := time.Duration(0)
	for i := 1; i <= 12; i++ {
		drift -= 100 * time.Millisecond // 墙钟每次比单调钟少跑 100ms
		m.Sample(rd(t0.Add(time.Duration(i)*10*time.Second+drift), time.Duration(i)*10*time.Second))
	}
	s := m.Snapshot()
	if s.Forward != 0 || s.Backward != 0 {
		t.Fatalf("negative drift must not be counted as a jump: %+v", s)
	}
	if s.Offset != -1200*time.Millisecond {
		t.Fatalf("offset = %v", s.Offset)
	}
	if s.Verdict() != VerdictDrifted {
		t.Fatalf("verdict = %q, want drifted for negative offset", s.Verdict())
	}
}

// 累计偏移以「首次采样」为固定基准，而不是滑动地跟着上一次采样走。跳变把
// 累计值扳回阈值以内时，判定仍然是被一次跳变永久标记过的 jumped：事件发生过
// 就是发生过，与它后来有没有被抵消无关。
func TestMonitorJumpBeatsDrift(t *testing.T) {
	m := New(0, nil)
	m.Sample(rd(t0, 0))
	m.Sample(rd(t0.Add(10*time.Second+1500*time.Millisecond), 10*time.Second)) // 向前跳 1.5s
	// 单调时钟照常走到 20s；墙钟先停在跳后的 11.5s，再走 8.5s 到 20s，累计偏移归零。
	// 单调走到 20s 而墙钟走到 20s，等价于墙钟在中间回拨了 1.5s。
	m.Sample(rd(t0.Add(20*time.Second), 20*time.Second))

	s := m.Snapshot()
	if s.Offset != 0 {
		t.Fatalf("offset = %v, want 0", s.Offset)
	}
	if s.Verdict() != VerdictJumped {
		t.Fatalf("verdict = %q, want jumped", s.Verdict())
	}
}

// 基准必须是「首次采样」而不是「上一次采样」：若把上一次采样当基准，累计偏移
// 就退化成只看最近一个间隔，长期累积的漂移会在平稳的一次采样里被清零，drifted
// 永远报不出来。
func TestCumulativeOffsetIsAnchoredToFirstSample(t *testing.T) {
	m := New(0, nil)
	m.Sample(rd(t0, 0))
	for i := 1; i <= 10; i++ {
		// 每次墙钟比单调钟多跑 200ms，从不触发跳变（< 1s），但单调递增。
		m.Sample(rd(t0.Add(time.Duration(i)*10*time.Second+time.Duration(i)*200*time.Millisecond),
			time.Duration(i)*10*time.Second))
	}
	s := m.Snapshot()
	if s.Forward != 0 || s.Backward != 0 {
		t.Fatalf("no jump may be recorded: %+v", s)
	}
	if s.Offset != 2*time.Second {
		t.Fatalf("offset = %v, want 2s accumulated from the first sample", s.Offset)
	}
	if s.Verdict() != VerdictDrifted {
		t.Fatalf("verdict = %q, want drifted", s.Verdict())
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	m := New(0, nil)
	m.Sample(rd(t0, 0))
	m.Sample(rd(t0.Add(10*time.Second+2*time.Second), 10*time.Second))

	s := m.Snapshot()
	if s.LastJump == nil {
		t.Fatal("missing last jump")
	}
	s.LastJump.Step = time.Hour // 篡改快照
	s.Forward = 99
	if got := m.Snapshot(); got.LastJump.Step != 2*time.Second || got.Forward != 1 {
		t.Fatalf("snapshot aliases monitor state: %+v", got)
	}
}

// 回调在锁外执行：里面调用 Snapshot 不能自锁。
func TestOnJumpRunsOutsideTheLock(t *testing.T) {
	type result struct {
		samples uint64
		step    time.Duration
	}
	ch := make(chan result, 1)
	var m *Monitor
	m = New(0, func(j Jump) {
		// 若 Sample 持有锁时回调，这里会永久阻塞。
		s := m.Snapshot()
		ch <- result{samples: s.Samples, step: j.Step}
	})
	m.Sample(rd(t0, 0))
	m.Sample(rd(t0.Add(10*time.Second), 10*time.Second)) // 无跳变：不应回调
	select {
	case r := <-ch:
		t.Fatalf("callback fired without a jump: %+v", r)
	default:
	}
	m.Sample(rd(t0.Add(20*time.Second+3*time.Second), 20*time.Second))
	select {
	case r := <-ch:
		if r.step != 3*time.Second {
			t.Fatalf("callback step = %v", r.step)
		}
		// 回调发生在计数更新之后：它看到的是「已计入这一次」的快照。
		if r.samples != 3 {
			t.Fatalf("callback saw samples = %d, want 3", r.samples)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback never fired (deadlock between Sample and Snapshot?)")
	}
}

func TestMonitorConcurrentSamplingAndSnapshot(t *testing.T) {
	m := New(10*time.Millisecond, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				// 一半的采样带跳变，一半平稳，制造计数与快照的并发窗口。
				step := time.Duration(0)
				if n%2 == 0 {
					step = 50 * time.Millisecond
				}
				m.Sample(rd(t0.Add(time.Duration(n)*time.Second+step), time.Duration(n)*time.Second))
				_ = m.Snapshot().Verdict()
			}
		}(i)
	}
	wg.Wait()
	if s := m.Snapshot(); s.Samples != 1600 {
		t.Fatalf("samples = %d", s.Samples)
	}
}

// SystemSource 的两个不变量：墙钟不带单调读数（否则偏移恒为 0），单调读数
// 单调不减且与墙钟流逝一致。
func TestSystemSource(t *testing.T) {
	src := SystemSource()
	a := src()
	if strings.Contains(a.Wall.String(), "m=") {
		t.Fatalf("wall clock still carries a monotonic reading: %v", a.Wall)
	}
	time.Sleep(2 * time.Millisecond)
	b := src()
	if b.Mono <= a.Mono {
		t.Fatalf("monotonic reading did not advance: %v -> %v", a.Mono, b.Mono)
	}
	// 两次真实采样之间不该出现超过阈值的偏移（真实系统上 2ms 内不会跳变）。
	if off := Offset(a, b); off > 100*time.Millisecond || off < -100*time.Millisecond {
		t.Fatalf("nonsensical offset between two real samples: %v", off)
	}
}
