package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"uic/internal/clockmon"
)

// 时钟跳变检测（G10）：墙钟相对单调钟的一次性步进，对上层只是「数据时间戳
// 是否可信」变成可判定状态。这里用 Options.ClockSource 注入一个「真实系统时钟 +
// 固定偏移」的源，把向前跳变 / 回拨两种场景精确复现出来，而不必真的去改系统时间。
func TestClockJumpDetectionEndToEnd(t *testing.T) {
	var stepNanos atomic.Int64
	step := stepNanos.Load

	base := clockmon.SystemSource()
	// 注入源：每次采样都调真实系统时钟（拿到单调分量），再叠加一个可变的偏移。
	// step 为 0 时与系统时钟完全等价，所以「没注入跳变」这一路测的是真实路径
	// 而不是另造一条假路径。
	src := func() clockmon.Reading {
		r := base()
		r.Wall = r.Wall.Add(time.Duration(step()))
		return r
	}

	h := newHarnessTuned(t, t.TempDir(), nil, func(o *Options) {
		o.ClockSource = src
	})

	// 启动后 1 秒内必须播种过：verdict 不能是 unknown，ageMs 不能是负数。
	// 这一条同时锁死「不播种就开看门狗」这个空窗——空窗期 health 全是 unknown，
	// 接进告警系统会先产生一波假阳性。
	var health struct {
		Status string `json:"status"`
		Clock  struct {
			Verdict         string  `json:"verdict"`
			OffsetSeconds   float64 `json:"offsetSeconds"`
			StepThresholdMs int64   `json:"stepThresholdMs"`
			AgeMs           int64   `json:"ageMs"`
			Jumps           struct {
				Forward  uint64 `json:"forward"`
				Backward uint64 `json:"backward"`
			} `json:"jumps"`
			LastJump *struct {
				At        string  `json:"at"`
				Seconds   float64 `json:"seconds"`
				Direction string  `json:"direction"`
			} `json:"lastJump"`
		} `json:"clock"`
	}

	code, body := h.do(http.MethodGet, "/api/v1/health", "")
	if code != 200 {
		t.Fatalf("health = %d %s", code, body)
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if health.Clock.Verdict != "ok" {
		t.Fatalf("clock.verdict = %q after seeding, want ok: %s", health.Clock.Verdict, body)
	}
	if health.Clock.StepThresholdMs != 1000 {
		t.Fatalf("stepThresholdMs = %d, want 1000", health.Clock.StepThresholdMs)
	}
	if health.Clock.AgeMs < 0 {
		t.Fatalf("ageMs must never be negative, got %d", health.Clock.AgeMs)
	}

	// 向前跳变 1.5s（> 1s 阈值）：必须报 jumped，方向 forward，幅度约 1.5s。
	stepNanos.Store(int64(1500 * time.Millisecond))
	var sawJump bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := h.do(http.MethodGet, "/api/v1/health", "")
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if health.Clock.Verdict == "jumped" && health.Clock.Jumps.Forward >= 1 {
			sawJump = true
			if health.Clock.OffsetSeconds < 1.4 || health.Clock.OffsetSeconds > 1.6 {
				t.Fatalf("offsetSeconds = %v, want ~1.5", health.Clock.OffsetSeconds)
			}
			if health.Clock.LastJump == nil || health.Clock.LastJump.Direction != "forward" {
				t.Fatalf("lastJump = %+v, want forward", health.Clock.LastJump)
			}
			if health.Clock.LastJump.Seconds < 1.4 || health.Clock.LastJump.Seconds > 1.6 {
				t.Fatalf("lastJump.seconds = %v, want ~1.5", health.Clock.LastJump.Seconds)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawJump {
		t.Fatalf("forward jump never observed: %s", body)
	}

	// 跳变必须写进日志：运维靠这条 Warn 定位「什么时候、往哪个方向、跳了多久」。
	// 断言日志而不是只断言指标，是因为日志带方向、幅度与跳变后的墙钟时刻，是
	// 排障时唯一能直接读出「发生了什么」的地方。
	var logged bool
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.rt.Logs("", "warn", 200) {
			if e.Message == "clock_jump" {
				if _, ok := e.Attrs["direction"]; !ok {
					t.Fatalf("clock_jump log missing direction attr: %+v", e.Attrs)
				}
				logged = true
				break
			}
		}
		if logged {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !logged {
		t.Fatal("clock jump was detected but never written to the warn log")
	}

	// 回拨 2s：单调照常，墙钟倒退 2s。累计偏移从 +1.5s 变成 -0.5s，方向 backward。
	stepNanos.Store(int64(-500 * time.Millisecond))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := h.do(http.MethodGet, "/api/v1/health", "")
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if health.Clock.Jumps.Backward >= 1 && health.Clock.LastJump.Direction == "backward" {
			if health.Clock.AgeMs < 0 {
				t.Fatalf("ageMs = %d after backward jump", health.Clock.AgeMs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backward jump never observed: %s", body)
}

// 时钟跳变必须出现在指标里，且按方向打标签——两个方向的后果是反的（向前跳变
// 让样本时间戳倒退，向后跳变让 TTL 提前到期丢掉数据），只看总数无法区分。
func TestClockJumpMetricsExposed(t *testing.T) {
	var stepNanos atomic.Int64
	base := clockmon.SystemSource()
	src := func() clockmon.Reading {
		r := base()
		r.Wall = r.Wall.Add(time.Duration(stepNanos.Load()))
		return r
	}
	h := newHarnessTuned(t, t.TempDir(), nil, func(o *Options) {
		o.ClockSource = src
	})

	stepNanos.Store(int64(2500 * time.Millisecond))

	var metrics string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := h.do(http.MethodGet, "/api/v1/metrics", "")
		metrics = string(body)
		if strings.Contains(metrics, `uic_clock_jumps_total{dir="forward"} 1`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 计数器：精确值断言（Inc 是整数累加，没有浮点问题）。
	if !strings.Contains(metrics, `uic_clock_jumps_total{dir="forward"} 1`) {
		t.Fatalf("forward counter missing or wrong value:\n%s", metrics)
	}
	// 声明（HELP 行）必须存在：只靠在业务代码里 Inc 也能「碰巧」产出指标行，
	// 但那样 /metrics 里就没有 HELP/TYPE 元数据，监控平台无法正确解析类型。断言
	// HELP 行等价于断言 declareMetrics 真的声明了它们。
	for _, want := range []string{
		"# HELP uic_clock_jumps_total ",
		"# HELP uic_clock_offset_seconds ",
		"# HELP uic_clock_last_jump_seconds ",
		"# TYPE uic_clock_jumps_total counter",
		"# TYPE uic_clock_offset_seconds gauge",
		"# TYPE uic_clock_last_jump_seconds gauge",
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing declaration %q:\n%s", want, metrics)
		}
	}
	// gauge 必须真的被刷新成非零值：只断言键存在，会在「gauge 从不 Set、恒为 0」
	// 时也通过——那正是「抓取时不刷新」这个缺陷的样子。跳了 2.5s，offset 与
	// last-jump 都应约等于 2.5（浮点往返会有微小误差，只断言量级与符号）。
	off, ok := metricValue(metrics, "uic_clock_offset_seconds")
	if !ok || off < 2.4 || off > 2.6 {
		t.Fatalf("uic_clock_offset_seconds = %v (found=%v), want ~2.5", off, ok)
	}
	lj, ok := metricValue(metrics, "uic_clock_last_jump_seconds")
	if !ok || lj < 2.4 || lj > 2.6 {
		t.Fatalf("uic_clock_last_jump_seconds = %v (found=%v), want ~2.5", lj, ok)
	}
}

// metricValue 从 Prometheus 文本里取一个无标签 gauge 的值。
func metricValue(text, name string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// healthAt 的 clock.ageMs 必须用**注入的 now** 计算，且把负值夹到 0。
//
// 为什么要单独测：HTTP 路径上 now 就是 time.Now()，与 time.Since(SampledAt) 同源，
// 区分不出「用了注入时钟」还是「用了真实时钟」。只有直接以受控时刻调 healthAt，
// 才能锁死这两个不变量——而它们都有真实后果：
//   - 用真实时钟算 age，watchdog 类测试注入的未来时刻会被旁路，age 永远接近 0，
//     「采样协程卡住」这个信号就永远报不出来；
//   - 不夹负，health 在「采样之后、下一次 tick 之前」被调用时会返回负的 age，
//     下游按数值做阈值判断会把它当成「刚采过」——比不报还误导。
func TestClockHealthAgeUsesInjectedTimeAndClampsNegative(t *testing.T) {
	h := newHarnessTuned(t, t.TempDir(), nil, nil)

	// 用一个明显未来的 now（1 小时后）：age 必须约等于 1 小时。真实时钟算不出这个
	// 值（真实 elapsed 只有毫秒级），所以这条能区分「注入 now」与「time.Since」。
	future := time.Now().Add(time.Hour)
	got := h.rt.healthAt(future)
	if got.Clock.AgeMs < 3500_000 { // 1h ≈ 3.6e6 ms；留 100s 余量给采样漂移
		t.Fatalf("AgeMs = %d with injected now +1h, want ~3.6e6 (was the injected clock bypassed?)",
			got.Clock.AgeMs)
	}

	// 用一个明显过去的 now（1 小时前）：无论如何采样都在「未来」，age 必须夹到 0。
	past := time.Now().Add(-time.Hour)
	got = h.rt.healthAt(past)
	if got.Clock.AgeMs != 0 {
		t.Fatalf("AgeMs = %d with injected now -1h, want 0 (negative age must be clamped)",
			got.Clock.AgeMs)
	}
}

// 还没采到样（New 之后、Start 之前）时，clock 块必须是「诚实的三元组」：
// verdict=unknown（合法枚举，不是空串）、ageMs=0（SampledAt 是零值，去算 age 会
// 得到一个一万年级的毫秒数）、stepThresholdMs 仍如实报出阈值。生产上 Start 会先
// 播种再对外服务，所以这个窗口在第 0 秒就关闭了；但契约不能依赖「调用顺序恰好
// 正确」——直接把 New 出来的实例拿来查 health 也必须看到合法的 unknown，而不是
// 一个空串或天文数字。
func TestClockHealthBeforeFirstSampleIsUnknown(t *testing.T) {
	dir := t.TempDir()
	cfg := map[string]any{
		"schemaVersion": 1,
		"site":          "clock-pre-seed",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": dir, "journal": true},
		"logging":       map[string]any{"level": "info"},
		"channels": []any{map[string]any{
			"id": "ch", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": "127.0.0.1:1"},
			"concurrency": 1, "timeout": "100ms",
		}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(time.Second) })

	got := rt.healthAt(time.Now())
	if got.Clock.Verdict != "unknown" {
		t.Fatalf("verdict before first sample = %q, want unknown", got.Clock.Verdict)
	}
	if got.Clock.AgeMs != 0 {
		t.Fatalf("ageMs before first sample = %d, want 0 (zero SampledAt must not be subtracted)", got.Clock.AgeMs)
	}
	if got.Clock.StepThresholdMs != 1000 {
		t.Fatalf("stepThresholdMs before first sample = %d, want 1000", got.Clock.StepThresholdMs)
	}
	if got.Clock.Jumps.Forward != 0 || got.Clock.Jumps.Backward != 0 || got.Clock.LastJump != nil {
		t.Fatalf("unexpected jump state before first sample: %+v", got.Clock)
	}
}
