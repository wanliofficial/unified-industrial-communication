package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"uic/internal/heartbeat"
	"uic/internal/northbound/httpapi"
)

// 正常运行时 /api/v1/health 必须报告调度器在转：verdict=healthy，且带年龄与阈值。
// 这是「进程活着且采集中枢也在转」的正例；下面的 stalled 用例是反例。
func TestHealthReportsSchedulerHealthy(t *testing.T) {
	h := newHarness(t, nil)
	// 先确保调度器真的 tick 过（采集器跑起来才会有点位好值）。
	h.waitPointGood("sim01", "reg0")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body := h.do(http.MethodGet, "/api/v1/health", "")
		if code != 200 {
			t.Fatalf("health = %d %s", code, body)
		}
		var health httpapi.Health
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if health.Scheduler.Verdict == "healthy" {
			if health.Scheduler.StallAfterMs <= 0 {
				t.Fatalf("healthy 但未回显阈值: %+v", health.Scheduler)
			}
			if health.Status != "ok" && health.Status != "degraded" && health.Status != "empty" {
				t.Fatalf("健康调度器不应把 status 置为 %q", health.Status)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("scheduler 从未报告 healthy")
}

// 采集中枢卡死时，健康检查必须显式暴露并用最高级状态压过设备态的「看似正常」。
//
// 实现方式：不冻结真实协程（会拖垮整场测试），而是用一个「未来时刻」调用
// healthAt —— 等价于「已经过去 15 秒而没有 tick」。这是真实的卡死形态，只是
// 时间被快进。
func TestHealthReportsSchedulerStalled(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	future := time.Now().Add(2 * time.Minute)
	health := h.rt.healthAt(future)
	if health.Scheduler.Verdict != "stalled" {
		t.Fatalf("verdict = %q, want stalled (%+v)", health.Scheduler.Verdict, health.Scheduler)
	}
	if health.Status != "stalled" {
		t.Fatalf("status = %q, want stalled（卡死必须压过设备态）", health.Status)
	}
	var found bool
	for _, p := range health.Problems {
		if p.State == "stalled" {
			found = true
			if p.Reason == "" {
				t.Fatalf("stalled problem 无 reason: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("stalled 但 problems 里没有 state=stalled 的条目: %+v", health.Problems)
	}
}

// 卡死判定必须能被阈值裁剪：注入极短 StallAfter 后，正常运行的调度器也不该
// 被判 healthy。这条锁住「阈值真的被用上了」，而不是永远走默认 15s。
func TestHealthStallThresholdIsWired(t *testing.T) {
	h := newHarnessTuned(t, t.TempDir(), nil, func(o *Options) {
		// 1 纳秒阈值：任何 age>0 都算卡死。
		o.StallAfter = time.Nanosecond
	})
	h.waitPointGood("sim01", "reg0")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		health := h.rt.Health()
		if health.Scheduler.Verdict == "stalled" {
			if health.Scheduler.StallAfterMs != 0 {
				// 1ns 毫秒取整为 0，omitempty 会省掉；只验证 verdict 与 age 字段。
				t.Logf("stallAfterMs=%d", health.Scheduler.StallAfterMs)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("注入 1ns 阈值后仍未判 stalled：StallAfter 没有被接入判定")
}

// 心跳文件：运维不访问 HTTP 也能判断进程活着且中枢在转。
func TestHeartbeatFileWritten(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessTuned(t, dir, nil, func(o *Options) {
		o.HeartbeatInterval = 50 * time.Millisecond
	})
	h.waitPointGood("sim01", "reg0")

	path := heartbeat.Path(dir)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			var snap heartbeat.Snapshot
			if err := json.Unmarshal(b, &snap); err != nil {
				t.Fatalf("心跳文件不是合法 JSON: %v (%s)", err, b)
			}
			if snap.TS == 0 || snap.TSAt == "" {
				t.Fatalf("心跳缺少时间戳: %+v", snap)
			}
			if snap.PID != os.Getpid() {
				t.Fatalf("pid = %d, want %d", snap.PID, os.Getpid())
			}
			if snap.InstanceID == "" {
				t.Fatalf("心跳缺少 instanceId: %+v", snap)
			}
			if snap.Scheduler.Verdict == "" {
				t.Fatalf("心跳缺少 scheduler.verdict: %+v", snap)
			}
			if snap.Devices.Total == 0 {
				t.Fatalf("心跳缺少设备汇总: %+v", snap.Devices)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("心跳文件从未生成: %s", path)
}

// 心跳文件的 ts 必须前进：一个卡住不更新的心跳文件本身就是「进程疑似挂了」的
// 信号，写一次就再也不动等于假信号。
func TestHeartbeatTimestampAdvances(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessTuned(t, dir, nil, func(o *Options) {
		o.HeartbeatInterval = 30 * time.Millisecond
	})
	h.waitPointGood("sim01", "reg0")

	path := heartbeat.Path(dir)
	read := func() (heartbeat.Snapshot, bool) {
		b, err := os.ReadFile(path)
		if err != nil {
			return heartbeat.Snapshot{}, false
		}
		var s heartbeat.Snapshot
		if json.Unmarshal(b, &s) != nil {
			return heartbeat.Snapshot{}, false
		}
		return s, true
	}
	first, ok := read()
	if !ok {
		t.Fatal("心跳文件未生成")
	}
	// 配置周期 30ms，要求在 2 秒内至少推进 3 次：这既证明 ts 在前进，也证明
	// 「配置的周期真的被用上了」——把 ticker 硬编码成 5s 的变异体会在这里失败。
	deadline := time.Now().Add(2 * time.Second)
	lastTS := first.TSMs
	advances := 0
	for time.Now().Before(deadline) {
		if s, ok := read(); ok && s.TSMs > lastTS {
			lastTS = s.TSMs
			advances++
			if advances >= 3 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("心跳 ts 在 2 秒内只推进 %d 次（配置周期 30ms 应远多于此）", advances)
}

// HeartbeatInterval<0 表示显式禁用：此时不应生成心跳文件（而不是生成后不更新）。
func TestHeartbeatDisabled(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessTuned(t, dir, nil, func(o *Options) {
		o.HeartbeatInterval = -1
	})
	h.waitPointGood("sim01", "reg0")
	// 留足两个默认间隔的时间，确认确实没写。
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(heartbeat.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("禁用后仍生成了心跳文件: %v", err)
	}
}

// 卡死必须在指标与日志之外、也体现在 /api/v1/health 的 scheduler 块里被外部
// 读取；同时 /healthz 的既有契约不能被破坏（仍是无条件 200）。
func TestHealthzContractUnchanged(t *testing.T) {
	h := newHarness(t, nil)
	code, body := h.do(http.MethodGet, "/healthz", "")
	if code != 200 {
		t.Fatalf("healthz = %d %s", code, body)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("healthz 契约被改动: %s", body)
	}
}

// 看门狗必须主动记指标并「一次卡死只记一次」，而不是每秒刷一条：
// 计数器被高频刷会把「一次故障」放大成「上万次故障」，告警阈值全部失真。
func TestWatchdogRecordsStallOncePerEpisode(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	// 用未来时刻连续判定三次：模拟「持续卡死」的多个看门狗周期。
	r := h.rt
	future := time.Now().Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		r.checkSchedulerLiveness(future)
	}
	if got := r.metrics.Value("uic_scheduler_stalled_total", nil); got != 1 {
		t.Fatalf("stalled_total = %v, want 1（一次卡死只记一次）", got)
	}
	if !r.schedStalled.Load() {
		t.Fatal("schedStalled 未置位")
	}
	// 恢复后计数器不再增加，且标记清零。
	r.checkSchedulerLiveness(time.Now())
	if r.schedStalled.Load() {
		t.Fatal("恢复后 schedStalled 未清零")
	}
	if got := r.metrics.Value("uic_scheduler_stalled_total", nil); got != 1 {
		t.Fatalf("恢复正常不应增加计数器，got %v", got)
	}
	// 再次卡死：这是新的一次故障，必须再记一次。
	r.checkSchedulerLiveness(future)
	if got := r.metrics.Value("uic_scheduler_stalled_total", nil); got != 2 {
		t.Fatalf("第二次卡死应记 2，got %v", got)
	}
	// tick 年龄 gauge 必须被写（趋势告警依赖它）。
	if got := r.metrics.Value("uic_scheduler_tick_age_seconds", nil); got <= 0 {
		t.Fatalf("tick_age_seconds = %v, want >0", got)
	}
}

// 启动时必须立刻写首份心跳：若只在「间隔到点」才写，把间隔配成 5 分钟就意味着
// 进程启动后前 5 分钟对外完全隐形，supervisor 会以为它没起来。
func TestHeartbeatWrittenAtStartup(t *testing.T) {
	dir := t.TempDir()
	// 间隔 1 小时：测试窗口内不可能等到任何一次周期性写入。
	h := newHarnessTuned(t, dir, nil, func(o *Options) {
		o.HeartbeatInterval = time.Hour
	})
	h.waitPointGood("sim01", "reg0")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(heartbeat.Path(dir)); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("间隔 1h 时启动阶段从未写首份心跳")
}
