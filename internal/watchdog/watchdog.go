// Package watchdog 提供「采集中枢是否卡死」的纯判定逻辑。
//
// 背景：调度器是一个单协程 tick 循环。若它因死锁或长时间阻塞而停摆，所有设备
// 都不再被调度，但进程仍活着、HTTP 层照常响应、/healthz 照常 200——这是最难
// 发现的一类静默失效：外部 supervisor 只会看「进程在不在」，而「进程在、但什么
// 都没在采」对它们是隐形的。
//
// 本包不启动协程、不做 I/O，只把一个「最近一次 tick 的时刻」翻译成可判定的
// 结论。这样判定规则可以脱离时钟与调度器单独测试，也便于在不同调用点复用
// （运行时看门狗、健康检查、心跳文件）。
package watchdog

import "time"

// Verdict 是对「采集中枢活性」的判定。
type Verdict string

const (
	// VerdictUnknown：无法判定（调度器尚未 tick，例如刚启动）。
	VerdictUnknown Verdict = "unknown"
	// VerdictHealthy：最近一次 tick 在阈值之内，中枢在转。
	VerdictHealthy Verdict = "healthy"
	// VerdictStalled：最近一次 tick 距今超过阈值，中枢疑似卡死。
	VerdictStalled Verdict = "stalled"
)

// Options 是判定参数。
type Options struct {
	// StallAfter 是「多久没 tick 就判定卡死」的阈值。<=0 时用默认值。
	StallAfter time.Duration
}

// DefaultStallAfter 是默认的卡死阈值。
//
// 取值理由：调度器默认 tick 间隔 50ms，正常情况下每秒会 tick 二十次。取 15s
// 给了「一次偶发的长阻塞（如大配置热更新的短暂停顿）」足够余量，又远小于人工
// 巡检周期——真卡死时 15 秒内必被判定。过小会把正常的 GC/调度抖动误报成卡死。
const DefaultStallAfter = 15 * time.Second

func (o Options) withDefaults() Options {
	if o.StallAfter <= 0 {
		o.StallAfter = DefaultStallAfter
	}
	return o
}

// Status 是一次判定的结果，携带足够的上下文让调用方写健康/日志而不必再算。
type Status struct {
	Verdict Verdict
	// Age 是最近一次 tick 距今的时长；Unknown 时为零。
	Age time.Duration
	// StallAfter 是本次判定采用的阈值（便于在响应里回显）。
	StallAfter time.Duration
	// Detail 是人类可读的原因。
	Detail string
}

// Evaluate 根据「最近一次 tick 的时刻」判定中枢活性。
//
// lastTick 的第二个返回值来自 scheduler.LastTick()：false 表示调度器尚未启动，
// 此时返回 VerdictUnknown 而不是 VerdictStalled——刚启动的一瞬间不能误报卡死。
// now 由调用方注入，便于测试与在整轮健康汇总里复用同一个时刻。
func Evaluate(now, lastTick time.Time, tickKnown bool, opt Options) Status {
	opt = opt.withDefaults()
	if !tickKnown {
		return Status{
			Verdict:    VerdictUnknown,
			StallAfter: opt.StallAfter,
			Detail:     "scheduler has not ticked yet",
		}
	}
	age := now.Sub(lastTick)
	if age < 0 {
		// 时钟回拨或注入错误：保守地当作 0，不因此误判卡死。
		age = 0
	}
	if age > opt.StallAfter {
		return Status{
			Verdict:    VerdictStalled,
			Age:        age,
			StallAfter: opt.StallAfter,
			Detail:     "scheduler tick overdue: " + age.Truncate(time.Millisecond).String() + " since last tick",
		}
	}
	return Status{
		Verdict:    VerdictHealthy,
		Age:        age,
		StallAfter: opt.StallAfter,
		Detail:     "scheduler ticking",
	}
}
