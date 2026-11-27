// Package clockmon 监测墙钟相对单调钟的跳变。
//
// 背景：本进程里几乎所有「时间」都取自 time.Now()——样本采集时刻、Outbox 的
// TTL、历史保留窗口、报警去抖、审计时间戳。time.Now() 返回的时刻里有两个分量：
//
//   - **墙钟**（wall）：真实世界的日期时间，会被 NTP 步进、容器迁移、人工
//     `date -s`、虚拟机快照恢复改变；
//   - **单调钟**（monotonic）：进程内的流逝时间，只会前进，不受上述调整影响。
//
// 两个分量一旦分叉，上面那些逻辑不会报错，只会**安静地算错**：向前跳变让
// TTL 提前到期，Outbox 一次性丢掉一批还没送出去的数据；向后跳变让采集时间戳
// 倒流，按时间去重/排序的消费方出现错乱。因此「墙钟还是不是原来那个墙钟」必须
// 是一个可判定的状态，而不是只能靠事后翻日志的猜测。
//
// 本包只做判定，不碰系统时间、不起协程、不做 I/O：
//
//	offset := clockmon.Offset(prev, now)   // 纯函数：两次采样之间墙钟相对单调钟的偏移
//	jump, ok := clockmon.Detect(prev, now, threshold)
//
//	mon := clockmon.New(clockmon.DefaultStepThreshold, nil)
//	mon.Sample(clockmon.SystemSource()())  // 采样节奏由调用方决定
//	snap := mon.Snapshot()                 // 供 health / 指标读取
//
// 采样源可注入（runtime.Options.ClockSource），所以「墙钟被拨了 1.5 秒」这种
// 场景可以在测试里精确复现，而不必真的去改系统时间——那既不现实，也会污染
// 同一台机器上的所有其他进程。
package clockmon

import (
	"sync"
	"time"
)

// DefaultStepThreshold 是判定「跳变」的默认阈值。
//
// 1 秒的依据：NTP 正常工作时小于 1 秒的偏差是**缓慢调谐**（slew）掉的，不是
// 步进；而 1 秒以上的步进足以让按秒看趋势的数据出问题——Outbox 的 TTL 会
// 提前到期、同一设备的相邻样本可能出现时间错序、按时间去重的消费方会漏数。
// 阈值取得太小只会把正常的调度抖动和 NTP 调谐报成跳变，训练运维忽略这个指标，
// 那比不报更糟。
const DefaultStepThreshold = time.Second

// Reading 是一次时钟采样：墙钟 + 单调钟。
type Reading struct {
	// Wall 是墙钟时刻。SystemSource 会给出去掉单调读数的值；即便调用方传进来
	// 的读数带着单调分量，Offset/Detect 也会先 Round(0) 剥掉，所以不会静默
	// 变成「永远没有偏移」。
	Wall time.Time
	// Mono 是单调流逝的时间，零点由采样源自定（SystemSource 用自己创建的时刻）。
	// 只在同一采样源的两个读数之间比较；不同源之间混用没有意义。
	Mono time.Duration
}

// SystemSource 返回系统时钟采样源。
//
// 返回值是一个闭包而不是直接读 time.Now()，是因为单调读数需要一个固定零点：
// 零点在调用本函数时确定，之后每次采样给出「自那以来流逝的单调时间」。
//
// 注意 Linux 的 CLOCK_MONOTONIC 不包含挂起（suspend）时间。机器从休眠恢复后，
// 墙钟前进了、单调钟没前进，本包会把它报成一次向前跳变——这是有意的：对数据
// 时间戳和 TTL 来说，「休眠了 8 小时」与「时钟被向前拨了 8 小时」的后果完全
// 相同，区分二者需要 CLOCK_BOOTTIME，而那个能力没有任何标准库入口。
func SystemSource() func() Reading {
	origin := time.Now()
	return func() Reading {
		now := time.Now()
		// now.Sub(origin) 用的是单调分量（两个时刻都带单调读数）。
		// now.Round(0) 剥掉单调读数，得到纯墙钟，供跨采样做墙钟算术。
		return Reading{Wall: now.Round(0), Mono: now.Sub(origin)}
	}
}

// Offset 返回两次采样之间「墙钟流逝」与「单调流逝」的差。
//
// 正数表示墙钟比单调钟跑得快（向前跳变），负数表示回拨。两次采样之间只做了
// 匀速前进时结果约为 0（在微秒级抖动以内）。
func Offset(prev, now Reading) time.Duration {
	// Round(0) 是防御性的：Wall 若带着单调读数，Sub 会走单调分量，偏移将恒为 0，
	// 整个监测会静默失效。
	wall := now.Wall.Round(0).Sub(prev.Wall.Round(0))
	return wall - (now.Mono - prev.Mono)
}

// 跳变方向。
const (
	DirectionForward  = "forward"
	DirectionBackward = "backward"
)

// Jump 是一次被判定为跳变的偏移。
type Jump struct {
	// At 是检测到跳变时读到的墙钟时刻，也就是**跳变之后**的值。因此向后跳变
	// 时它会早于真正的检测时刻——这不是笔误，而是「此刻墙上钟显示几点」的
	// 忠实记录。
	At time.Time
	// Step 是有符号幅度：正数=墙钟向前跳，负数=回拨。
	Step time.Duration
}

// Direction 返回 forward 或 backward。
func (j Jump) Direction() string {
	if j.Step < 0 {
		return DirectionBackward
	}
	return DirectionForward
}

// Seconds 返回有符号幅度（秒），便于直接写进 JSON 与指标。
func (j Jump) Seconds() float64 { return j.Step.Seconds() }

// Detect 判定一次采样间隔内是否发生了跳变。
//
// 阈值是**闭区间**：偏移达到 ±threshold 即判定为跳变。恰好等于阈值算跳变，
// 是为了让「阈值 1s」有一个确定的含义——偏移 1.000s 时报，0.999s 时不报，而
// 不是留一个「大于还是不小于」的模糊地带。
//
// 采样是离散的，所以「在一次间隔内跳出去又跳回来」看不见。这是采样的固有
// 局限，不是漏判：这种抖动对下游没有累积影响。
func Detect(prev, now Reading, threshold time.Duration) (Jump, bool) {
	if threshold <= 0 {
		threshold = DefaultStepThreshold
	}
	step := Offset(prev, now)
	if step >= threshold || step <= -threshold {
		return Jump{At: now.Wall, Step: step}, true
	}
	return Jump{}, false
}

// Monitor 累计跳变事实。
//
// 它不是零值可用的：必须用 New 构造。所有方法都可以并发调用（采样协程写、
// HTTP 处理协程读）。
type Monitor struct {
	threshold time.Duration
	onJump    func(Jump) // 由 Sample 在解锁之后调用，回调里可以安全地 Snapshot

	mu       sync.Mutex
	base     Reading // 首次采样，累计偏移的基准
	last     Reading
	samples  uint64
	forward  uint64
	backward uint64
	lastJump *Jump
}

// New 构造一个监测器。threshold <= 0 时用 DefaultStepThreshold。
//
// onJump 在每次检测到跳变时被调用（可为 nil）。它在锁外执行，因此回调里调用
// Snapshot 不会自锁；也正因如此，回调不应该做耗时操作——它跑在采样调用方的
// 协程上。
func New(threshold time.Duration, onJump func(Jump)) *Monitor {
	if threshold <= 0 {
		threshold = DefaultStepThreshold
	}
	return &Monitor{threshold: threshold, onJump: onJump}
}

// Sample 记录一次采样，返回本次间隔内判定出的跳变。
//
// 第一次采样只建立基准，不判定（没有「之前」可比）。返回的 Jump 在 ok=false
// 时是零值。
func (m *Monitor) Sample(r Reading) (jump Jump, ok bool) {
	m.mu.Lock()
	if m.samples == 0 {
		m.base = r
	} else {
		if j, hit := Detect(m.last, r, m.threshold); hit {
			jump, ok = j, true
			if j.Step < 0 {
				m.backward++
			} else {
				m.forward++
			}
			cp := j
			m.lastJump = &cp
		}
	}
	m.last = r
	m.samples++
	fn := m.onJump
	m.mu.Unlock()
	// 回调必须在锁外执行：它可能调用 Snapshot，持锁回调会自锁。
	if ok && fn != nil {
		fn(jump)
	}
	return jump, ok
}

// 判定结果，供健康检查直接映射。
const (
	// VerdictUnknown：还没有采到样，无从判断。
	VerdictUnknown = "unknown"
	// VerdictOK：没见过跳变，累计偏移也在阈值以内。
	VerdictOK = "ok"
	// VerdictDrifted：没见过跳变，但墙钟相对单调钟已累计偏移超过阈值——典型
	// 成因是长时间没有对时（晶振漂移）或 NTP 缓慢调谐的累积。
	VerdictDrifted = "drifted"
	// VerdictJumped：见过至少一次跳变。这是累计事实，不会自己恢复成 ok：
	// 「曾经被拨过」是一个客观发生过的事件，把它忘掉才是隐瞒。
	VerdictJumped = "jumped"
)

// Snapshot 是某一时刻的监测状态。它是一份值拷贝，可以安全地在协程之间传递。
type Snapshot struct {
	// SampledAt 是最近一次采样的墙钟时刻；零值表示还没采过样。
	SampledAt time.Time
	// Samples 是累计采样次数。
	Samples uint64
	// Forward / Backward 是按方向拆分的跳变次数。
	Forward  uint64
	Backward uint64
	// LastJump 是最近一次跳变；nil 表示从未发生。
	LastJump *Jump
	// Offset 是累计偏移：当前墙钟减去「首次采样的墙钟 + 单调流逝」。
	Offset time.Duration
	// Threshold 是判定跳变所用的阈值。
	Threshold time.Duration
}

// Verdict 给出四态判定。优先级：unknown > jumped > drifted > ok。
//
// 它回答的是**事实**问题（这个进程见过什么），不是「现在几点对不对」——后者
// 只能通过与外部时间源比对来回答。运维要对跳变告警就看计数与 lastJump，不要
// 指望这个字段自己恢复。
func (s Snapshot) Verdict() string {
	switch {
	case s.Samples == 0:
		return VerdictUnknown
	case s.Forward+s.Backward > 0:
		return VerdictJumped
	case s.Offset >= s.Threshold || s.Offset <= -s.Threshold:
		return VerdictDrifted
	}
	return VerdictOK
}

// Snapshot 返回当前状态的拷贝。
func (m *Monitor) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		Samples:   m.samples,
		Forward:   m.forward,
		Backward:  m.backward,
		Threshold: m.threshold,
	}
	if m.samples == 0 {
		return s
	}
	s.SampledAt = m.last.Wall
	s.Offset = Offset(m.base, m.last)
	if m.lastJump != nil {
		cp := *m.lastJump
		s.LastJump = &cp
	}
	return s
}
