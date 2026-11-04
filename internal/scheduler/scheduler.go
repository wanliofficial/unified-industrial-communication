// Package scheduler 负责「什么时候、由谁、以多大并发」去采集一台设备。
//
// 并发模型（与 docs/03-product/02-architecture.md §5 一致）：
//
//	1 个 tick 协程（默认 50ms）负责到期判定与投递；
//	每设备 1 个容量为 1 的 slot：同一设备的采集任务永不并发，
//	    投递失败即跳过本轮并计数（uic_scheduler_skipped_total）；
//	每通道 1 个信号量（Gate）：限制在飞请求数，写请求优先于读请求；
//	每设备 1 个代次（generation）：重启/重配后旧任务的结果必须被丢弃。
//
// 调度器不认识协议，也不碰 cache/outbox：它只调用 Target。这样
// 「重试/熔断/代次」这套容易写错的逻辑可以脱离驱动单独测试。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/obs"
)

// State 是设备的运行态（不是配置态）。
type State string

const (
	StateConfigured State = "configured" // 已加载，未启动
	StateConnecting State = "connecting" // 正在建链/探活
	StateCollecting State = "collecting" // 正常采集
	StateDegraded   State = "degraded"   // 部分失败，仍在重试
	StateFaulted    State = "faulted"    // 熔断，等待探活
	StateDisabled   State = "disabled"   // 配置停用
)

// ErrDeviceNotFound 表示设备未注册到调度器。
var ErrDeviceNotFound = errors.New("scheduler: device not found")

// ErrStopped 表示调度器已停止。
var ErrStopped = errors.New("scheduler: stopped")

// Target 是一台设备在调度器眼中的全部行为。
//
// Collect 必须自行打开/复用连接，并在返回前把结果写入 cache/outbox；
// 调度器只负责调用时机、超时、互斥与失败分类。实现必须是串行的
// （同一设备同时只有一个 Collect），但 Close 可能来自其他协程，
// 因此实现需要自己保证 Close 与 Collect 的互斥。
type Target interface {
	Collect(ctx context.Context, lease *Lease) error
	Probe(ctx context.Context, lease *Lease) error
	Close() error
}

// Lease 是「本次采集所属的代次」。
//
// 采集可能很慢，期间设备被重启或删除。Target 在把结果写回缓存前调用
// Live()；返回 false 表示本次结果已经过期，必须丢弃。这是比加锁更轻的
// 正确性手段：不需要让重启等待在飞请求。
type Lease struct {
	Device     string
	Generation uint64
	live       func() bool
}

// Live 报告本次采集是否仍然有效。
func (l *Lease) Live() bool {
	return l != nil && l.live != nil && l.live()
}

// Stats 是一台设备的运行统计。字段名与北向接口一致。
type Stats struct {
	ReadsTotal          uint64  `json:"readsTotal"`
	ReadsFailed         uint64  `json:"readsFailed"`
	Skipped             uint64  `json:"skipped"`
	Probes              uint64  `json:"probes"`
	SuccessRate         float64 `json:"successRate"`
	AvgDurationMs       float64 `json:"avgDurationMs"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
	LastError           string  `json:"lastError,omitempty"`
	LastErrorKind       string  `json:"lastErrorKind,omitempty"`
	LastSuccess         string  `json:"lastSuccess,omitempty"`
	LastAttempt         string  `json:"lastAttempt,omitempty"`
}

// DeviceStatus 是设备运行态的对外视图。
type DeviceStatus struct {
	ID              string    `json:"id"`
	Name            string    `json:"name,omitempty"`
	Channel         string    `json:"channel"`
	Protocol        string    `json:"protocol"`
	ConfiguredState string    `json:"configuredState"`
	RuntimeState    State     `json:"runtimeState"`
	Since           time.Time `json:"since"`
	Generation      uint64    `json:"generation"`
	Interval        string    `json:"interval"`
	NextDue         string    `json:"nextDue,omitempty"`
	Stats           Stats     `json:"stats"`
}

// StateEvent 是一次设备运行态迁移。
type StateEvent struct {
	DeviceID string    `json:"device"`
	From     State     `json:"previous"`
	To       State     `json:"state"`
	At       time.Time `json:"at"`
	// Error 是触发迁移的错误摘要（进入好状态时为空）。
	Error string `json:"error,omitempty"`
}

// Options 是调度参数。零值可用。
type Options struct {
	Tick             time.Duration // 默认 50ms
	BreakerThreshold int           // 连续失败多少次进入熔断，默认 5
	BreakerCooldown  time.Duration // 熔断后探活间隔，默认 30s
	MaxBackoff       time.Duration // 退避上限，默认 1m
	Jitter           float64       // 抖动比例 0..1，默认 0.2
	Now              func() time.Time

	// OnState 在设备运行态迁移时被调用。
	//
	// 它由调度器内部的独立 goroutine 串行调用，绝不占用采集协程：北向投递
	// 可能因 Broker 慢而阻塞数秒，若在采集路径上同步调用，一台设备的网络
	// 抖动会拖慢所有设备。回调本身也不得阻塞太久（会积压事件）。
	OnState func(StateEvent)
}

func (o Options) withDefaults() Options {
	if o.Tick <= 0 {
		o.Tick = 50 * time.Millisecond
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = 5
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 30 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.Jitter < 0 {
		o.Jitter = 0
	}
	if o.Jitter == 0 {
		o.Jitter = 0.2
	}
	if o.Jitter > 1 {
		o.Jitter = 1
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Metrics 是调度器需要的指标子集；*obs.Metrics 满足该接口。
type Metrics interface {
	Inc(name string, l obs.Labels)
	Set(name string, v float64, l obs.Labels)
	Observe(name string, v float64, l obs.Labels)
}

// Scheduler 是唯一的调度入口。创建后必须调用 Start。
type Scheduler struct {
	opt Options
	log *slog.Logger
	met Metrics

	mu      sync.RWMutex
	devices map[string]*device
	order   []*device
	gates   map[string]*gate

	events chan StateEvent // 状态迁移事件队列（有界，满则丢弃并计数）

	started atomic.Bool
	stopped atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup // tick 协程 + 状态派发协程
	work    sync.WaitGroup // 在飞采集
}

type device struct {
	id     string
	dev    model.Device
	ch     model.Channel
	target Target
	slot   chan struct{}
	gen    atomic.Uint64
	wr     atomic.Int32 // 等待中的写请求数（用于让读让行）

	mu          sync.Mutex
	state       State
	since       time.Time
	nextDue     time.Time
	consecutive int
	probeNext   bool
	skipped     uint64
	readsTotal  uint64
	readsFailed uint64
	probes      uint64
	durSum      float64
	durCount    uint64
	lastErr     string
	lastErrKind string
	lastSuccess time.Time
	lastAttempt time.Time
}

// New 创建调度器。met 与 log 可为 nil。
func New(opt Options, log *slog.Logger, met Metrics) *Scheduler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Scheduler{
		opt:     opt.withDefaults(),
		log:     log,
		met:     met,
		devices: map[string]*device{},
		gates:   map[string]*gate{},
		events:  make(chan StateEvent, 256),
	}
}

// Start 启动 tick 协程。重复调用无副作用。
func (s *Scheduler) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go s.loop()
	if s.opt.OnState != nil {
		s.wg.Add(1)
		go s.dispatchStates(s.ctx)
	}
}

func (s *Scheduler) loop() {
	defer s.wg.Done()
	t := time.NewTicker(s.opt.Tick)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.dispatch(now)
		}
	}
}

func (s *Scheduler) now() time.Time { return s.opt.Now() }

func (s *Scheduler) dispatch(now time.Time) {
	s.mu.RLock()
	list := make([]*device, len(s.order))
	copy(list, s.order)
	s.mu.RUnlock()

	for _, d := range list {
		d.mu.Lock()
		due, st, probe := d.nextDue, d.state, d.probeNext
		d.mu.Unlock()

		if st == StateDisabled || now.Before(due) {
			continue
		}
		select {
		case d.slot <- struct{}{}:
			s.work.Add(1)
			go func(d *device, probe bool) {
				defer s.work.Done()
				defer func() { <-d.slot }()
				s.runCycle(d, probe)
			}(d, probe)
		default:
			// 设备 actor 在飞：本轮直接跳过，绝不排队。
			interval := d.dev.Interval.Duration()
			if interval <= 0 {
				interval = time.Second
			}
			d.mu.Lock()
			d.skipped++
			d.nextDue = now.Add(interval)
			d.mu.Unlock()
			s.inc("uic_scheduler_skipped_total", obs.Labels{"device": d.id, "reason": "busy"})
		}
	}
}

func (s *Scheduler) runCycle(d *device, probe bool) {
	if s.ctx.Err() != nil {
		return
	}
	gen := d.gen.Load()
	lease := &Lease{
		Device:     d.id,
		Generation: gen,
		live: func() bool {
			return d.gen.Load() == gen && s.ctx.Err() == nil
		},
	}

	// 整轮超时：单次请求超时之外留出批量读的余量，但不超过采集周期太多。
	cycle := d.dev.TimeoutOrDefault() + time.Second
	if iv := d.dev.Interval.Duration(); iv > cycle {
		cycle = iv
	}
	ctx, cancel := context.WithTimeout(s.ctx, cycle)
	defer cancel()

	g := s.gateFor(d.ch)
	if err := g.acquire(ctx, false); err != nil {
		s.finish(d, gen, 0, false, err)
		return
	}
	defer g.release()

	start := s.now()
	d.mu.Lock()
	d.lastAttempt = start
	if probe {
		d.probes++
	}
	d.mu.Unlock()

	var err error
	if probe {
		err = d.target.Probe(ctx, lease)
	} else {
		err = d.target.Collect(ctx, lease)
	}
	s.finish(d, gen, s.now().Sub(start), !probe, err)
}

func (s *Scheduler) finish(d *device, gen uint64, dur time.Duration, collect bool, err error) {
	now := s.now()

	// 代次不匹配：设备已被重启/删除，本次结果只允许记账，不允许改状态。
	if d.gen.Load() != gen {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if collect {
		d.readsTotal++
		d.durSum += dur.Seconds()
		d.durCount++
	}
	d.lastAttempt = now

	if err == nil {
		old := d.state
		if collect {
			d.state = StateCollecting
			d.lastSuccess = now
		} else {
			d.state = StateConnecting
		}
		d.consecutive = 0
		d.probeNext = false
		interval := d.dev.Interval.Duration()
		if interval <= 0 {
			interval = time.Second
		}
		d.nextDue = now.Add(interval)
		s.stateChanged(d, old)
		if collect {
			s.inc("uic_collect_total", obs.Labels{
				"device": d.id, "protocol": d.ch.Protocol, "status": "ok",
			})
			s.observe("uic_collect_duration_seconds", dur.Seconds(), obs.Labels{
				"device": d.id, "protocol": d.ch.Protocol,
			})
			if d.durCount > 0 {
				s.set("uic_collect_avg_ms", d.durSum/float64(d.durCount)*1000, obs.Labels{
					"device": d.id, "protocol": d.ch.Protocol,
				})
			}
		}
		return
	}

	old := d.state
	d.consecutive++
	d.lastErr = err.Error()
	d.lastErrKind = string(driver.KindOf(err))
	if collect {
		d.readsFailed++
	}

	if driver.CountsTowardBreaker(err) && d.consecutive >= s.opt.BreakerThreshold {
		d.state = StateFaulted
		d.probeNext = true
		d.nextDue = now.Add(s.jitter(s.opt.BreakerCooldown))
	} else {
		d.state = StateDegraded
		d.nextDue = now.Add(s.jitter(s.backoff(d)))
	}

	s.inc("uic_collect_total", obs.Labels{
		"device": d.id, "protocol": d.ch.Protocol, "status": "error",
	})
	s.inc("uic_collect_errors_total", obs.Labels{
		"device": d.id, "protocol": d.ch.Protocol, "kind": string(driver.KindOf(err)),
	})
	s.stateChanged(d, old)

	if d.state == StateFaulted && old != StateFaulted {
		s.log.Warn("device_faulted", "device", d.id, "channel", d.ch.Protocol,
			"kind", d.lastErrKind, "failures", d.consecutive, "error", d.lastErr)
	}
}

// backoff 计算指数退避基数：interval * 2^(n-1)，上限 MaxBackoff。
func (s *Scheduler) backoff(d *device) time.Duration {
	base := d.dev.Interval.Duration()
	if base < 200*time.Millisecond {
		base = 200 * time.Millisecond
	}
	n := d.consecutive
	if n > 10 {
		n = 10
	}
	v := float64(base) * math.Pow(2, float64(n-1))
	if v > float64(s.opt.MaxBackoff) {
		v = float64(s.opt.MaxBackoff)
	}
	return time.Duration(v)
}

// jitter 给延迟叠加 ±Jitter 的随机抖动，避免多设备同时重连造成惊群。
func (s *Scheduler) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Millisecond
	}
	f := 1 + s.opt.Jitter*(2*rand.Float64()-1)
	return time.Duration(float64(d) * f)
}

func (s *Scheduler) stateChanged(d *device, old State) {
	if old == d.state {
		return
	}
	if old != "" {
		s.set("uic_device_state", 0, obs.Labels{"device": d.id, "state": string(old)})
	}
	s.set("uic_device_state", 1, obs.Labels{"device": d.id, "state": string(d.state)})
	d.since = s.now()
	s.log.Info("device_state", "device", d.id, "state", string(d.state),
		"previous", string(old), "generation", d.gen.Load())

	if s.opt.OnState != nil {
		ev := StateEvent{DeviceID: d.id, From: old, To: d.state, At: d.since, Error: d.lastErr}
		select {
		case s.events <- ev:
		default:
			// 队列满说明下游长期阻塞。丢弃事件比阻塞采集安全：当前态仍然
			// 可以通过 REST /api/v1/devices 读到，而采集停摆是不可接受的。
			s.inc("uic_state_event_dropped_total", obs.Labels{"device": d.id})
		}
	}
}

// dispatchStates 串行调用 OnState 回调。
func (s *Scheduler) dispatchStates(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.events:
			s.opt.OnState(ev)
		}
	}
}

func (s *Scheduler) inc(name string, l obs.Labels) {
	if s.met != nil {
		s.met.Inc(name, l)
	}
}

func (s *Scheduler) set(name string, v float64, l obs.Labels) {
	if s.met != nil {
		s.met.Set(name, v, l)
	}
}

func (s *Scheduler) observe(name string, v float64, l obs.Labels) {
	if s.met != nil {
		s.met.Observe(name, v, l)
	}
}

// ---------------------------------------------------------------------------
// 注册与生命周期
// ---------------------------------------------------------------------------

// Add 注册一台设备。重复 ID 返回错误；配置停用的设备只登记不调度。
func (s *Scheduler) Add(dev model.Device, ch model.Channel, target Target) error {
	if dev.ID == "" || target == nil {
		return errors.New("scheduler: device and target required")
	}
	now := s.now()
	d := &device{
		id:      dev.ID,
		dev:     dev,
		ch:      ch,
		target:  target,
		slot:    make(chan struct{}, 1),
		state:   StateCollecting,
		since:   now,
		nextDue: now,
	}
	if !dev.Enabled {
		d.state = StateDisabled
	}
	s.mu.Lock()
	if _, exists := s.devices[dev.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("scheduler: device %q already registered", dev.ID)
	}
	s.devices[dev.ID] = d
	s.order = append(s.order, d)
	sort.Slice(s.order, func(i, j int) bool { return s.order[i].id < s.order[j].id })
	if _, ok := s.gates[ch.ID]; !ok {
		s.gates[ch.ID] = newGate(ch.EffectiveConcurrency())
	}
	s.mu.Unlock()

	d.mu.Lock()
	st := d.state
	d.mu.Unlock()
	s.set("uic_device_state", 1, obs.Labels{"device": dev.ID, "state": string(st)})
	return nil
}

// Remove 注销设备并关闭其连接。在飞任务的结果会因代次失效被丢弃。
func (s *Scheduler) Remove(id string) bool {
	s.mu.Lock()
	d, ok := s.devices[id]
	if ok {
		delete(s.devices, id)
		for i, x := range s.order {
			if x == d {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	d.gen.Add(1)
	for _, st := range []State{StateConfigured, StateConnecting, StateCollecting, StateDegraded, StateFaulted, StateDisabled} {
		s.set("uic_device_state", 0, obs.Labels{"device": id, "state": string(st)})
	}
	// Close 可能等待在飞请求结束，不阻塞调用方（可能是 HTTP handler）。
	go func() { _ = d.target.Close() }()
	return true
}

// Restart 让一台设备重新开始：代次递增、状态回到 connecting、立即到期。
// 旧连接的关闭由 Target 在发现代次变化后自行完成（避免与在飞读竞争）。
func (s *Scheduler) Restart(id string) error {
	d, ok := s.get(id)
	if !ok {
		return ErrDeviceNotFound
	}
	now := s.now()
	d.mu.Lock()
	old := d.state
	d.gen.Add(1)
	d.state = StateConnecting
	d.consecutive = 0
	d.probeNext = false
	d.nextDue = now
	s.stateChanged(d, old)
	d.mu.Unlock()

	s.inc("uic_device_restart_total", obs.Labels{"device": id})
	return nil
}

func (s *Scheduler) get(id string) (*device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[id]
	return d, ok
}

// Generation 返回设备当前代次；供 Target 的调试与断言使用。
func (s *Scheduler) Generation(id string) (uint64, bool) {
	d, ok := s.get(id)
	if !ok {
		return 0, false
	}
	return d.gen.Load(), true
}

// SetEnabled 在运行期启用/停用一台设备（不改变配置）。
func (s *Scheduler) SetEnabled(id string, enabled bool) error {
	d, ok := s.get(id)
	if !ok {
		return ErrDeviceNotFound
	}
	now := s.now()
	d.mu.Lock()
	old := d.state
	if enabled {
		d.state = StateConnecting
		d.nextDue = now
		d.consecutive = 0
		d.probeNext = false
	} else {
		d.state = StateDisabled
	}
	s.stateChanged(d, old)
	d.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 写路径
// ---------------------------------------------------------------------------

// SubmitWrite 在设备 actor 内执行一次写操作，并让同通道的读请求让行。
//
// 写操作和采集用同一个 slot：同一设备上写与读永不并发（驱动 Conn 的契约）。
// 与采集不同的是，写请求会等待 slot，而不是被跳过——写是用户动作，不能静默丢弃。
func (s *Scheduler) SubmitWrite(ctx context.Context, id string, fn func(ctx context.Context) error) error {
	d, ok := s.get(id)
	if !ok {
		return ErrDeviceNotFound
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return ErrStopped
	}

	// 先登记「有写等待」，让同通道其他设备的读请求让行。
	d.wr.Add(1)
	defer d.wr.Add(-1)

	select {
	case d.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done():
		return ErrStopped
	}
	defer func() { <-d.slot }()

	timeout := d.dev.TimeoutOrDefault()
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			timeout = rem
		}
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	g := s.gateFor(d.ch)
	if err := g.acquire(wctx, true); err != nil {
		return err
	}
	defer g.release()

	d.mu.Lock()
	d.lastAttempt = s.now()
	d.mu.Unlock()

	err := fn(wctx)
	if err == nil {
		s.inc("uic_write_total", obs.Labels{"device": id, "status": "ok"})
	} else {
		s.inc("uic_write_total", obs.Labels{"device": id, "status": "error"})
	}
	return err
}

func (s *Scheduler) done() <-chan struct{} {
	if s.ctx == nil {
		ch := make(chan struct{})
		return ch
	}
	return s.ctx.Done()
}

// ---------------------------------------------------------------------------
// 状态查询
// ---------------------------------------------------------------------------

// Status 返回全部设备状态（按 ID 排序）。
func (s *Scheduler) Status() []DeviceStatus {
	s.mu.RLock()
	list := make([]*device, len(s.order))
	copy(list, s.order)
	s.mu.RUnlock()

	out := make([]DeviceStatus, 0, len(list))
	for _, d := range list {
		out = append(out, d.status())
	}
	return out
}

// StatusOf 返回单台设备状态。
func (s *Scheduler) StatusOf(id string) (DeviceStatus, bool) {
	d, ok := s.get(id)
	if !ok {
		return DeviceStatus{}, false
	}
	return d.status(), true
}

func (d *device) status() DeviceStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	configured := "disabled"
	if d.dev.Enabled {
		configured = "enabled"
	}
	st := Stats{
		ReadsTotal: d.readsTotal, ReadsFailed: d.readsFailed, Skipped: d.skipped,
		Probes: d.probes, ConsecutiveFailures: d.consecutive,
		LastError: d.lastErr, LastErrorKind: d.lastErrKind,
	}
	if d.readsTotal > 0 {
		st.SuccessRate = float64(d.readsTotal-d.readsFailed) / float64(d.readsTotal)
	}
	if d.durCount > 0 {
		st.AvgDurationMs = d.durSum / float64(d.durCount) * 1000
	}
	if !d.lastSuccess.IsZero() {
		st.LastSuccess = d.lastSuccess.UTC().Format(time.RFC3339Nano)
	}
	if !d.lastAttempt.IsZero() {
		st.LastAttempt = d.lastAttempt.UTC().Format(time.RFC3339Nano)
	}
	out := DeviceStatus{
		ID: d.id, Name: d.dev.Name, Channel: d.dev.ChannelID, Protocol: d.ch.Protocol,
		ConfiguredState: configured, RuntimeState: d.state, Since: d.since.UTC(),
		Generation: d.gen.Load(), Interval: d.dev.Interval.String(), Stats: st,
	}
	if !d.nextDue.IsZero() {
		out.NextDue = d.nextDue.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// Close 停止调度并关闭全部设备连接。它不等待慢驱动超过 timeout。
func (s *Scheduler) Close(timeout time.Duration) error {
	if !s.stopped.CompareAndSwap(false, true) {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()

	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}

	s.mu.Lock()
	list := make([]*device, len(s.order))
	copy(list, s.order)
	s.mu.Unlock()
	for _, d := range list {
		_ = d.target.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// 通道闸门
// ---------------------------------------------------------------------------

type gate struct {
	sem     chan struct{}
	pending atomic.Int32
}

func newGate(n int) *gate {
	if n <= 0 {
		n = 4
	}
	return &gate{sem: make(chan struct{}, n)}
}

// acquire 获取一个通道槽位。write=true 时登记写等待，读请求在存在写等待时让行。
func (g *gate) acquire(ctx context.Context, write bool) error {
	if write {
		g.pending.Add(1)
		defer g.pending.Add(-1)
		select {
		case g.sem <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		if g.pending.Load() > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
			continue
		}
		select {
		case g.sem <- struct{}{}:
			// 取到槽位后二次确认：如果期间出现了写请求，让出并重试。
			if g.pending.Load() > 0 {
				<-g.sem
				continue
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gate) release() {
	select {
	case <-g.sem:
	default:
	}
}

// Inflight 返回通道当前在飞请求数（用于指标与测试）。
func (g *gate) inflight() int { return len(g.sem) }

func (s *Scheduler) gateFor(ch model.Channel) *gate {
	s.mu.RLock()
	g, ok := s.gates[ch.ID]
	s.mu.RUnlock()
	if ok {
		return g
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.gates[ch.ID]; ok {
		return g
	}
	g = newGate(ch.EffectiveConcurrency())
	s.gates[ch.ID] = g
	return g
}

// ChannelInflight 返回通道在飞请求数。
func (s *Scheduler) ChannelInflight(id string) int {
	s.mu.RLock()
	g, ok := s.gates[id]
	s.mu.RUnlock()
	if !ok {
		return 0
	}
	return g.inflight()
}
