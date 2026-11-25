package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

// fakeTarget 记录调用次数，并允许测试注入错误与阻塞。
type fakeTarget struct {
	collects atomic.Int64
	probes   atomic.Int64
	closes   atomic.Int64

	inflight    atomic.Int64
	maxInflight atomic.Int64

	mu         sync.Mutex
	collectErr error
	probeErr   error

	block   chan struct{} // 非 nil 时 Collect 阻塞直到收到信号
	entered chan struct{} // 进入 Collect 时通知

	lastLease atomic.Value // *Lease
}

func (f *fakeTarget) Collect(ctx context.Context, lease *Lease) error {
	n := f.inflight.Add(1)
	for {
		max := f.maxInflight.Load()
		if n <= max || f.maxInflight.CompareAndSwap(max, n) {
			break
		}
	}
	defer f.inflight.Add(-1)
	f.collects.Add(1)
	f.lastLease.Store(lease)
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	err := f.collectErr
	f.mu.Unlock()
	return err
}

func (f *fakeTarget) setCollectErr(err error) {
	f.mu.Lock()
	f.collectErr = err
	f.mu.Unlock()
}

func (f *fakeTarget) setProbeErr(err error) {
	f.mu.Lock()
	f.probeErr = err
	f.mu.Unlock()
}

func (f *fakeTarget) Probe(ctx context.Context, lease *Lease) error {
	f.probes.Add(1)
	f.lastLease.Store(lease)
	f.mu.Lock()
	err := f.probeErr
	f.mu.Unlock()
	return err
}

func (f *fakeTarget) Close() error { f.closes.Add(1); return nil }

func dev(id string, interval time.Duration, enabled bool) model.Device {
	return model.Device{
		ID: id, ChannelID: "ch1", Interval: model.Duration(interval),
		Timeout: model.Duration(500 * time.Millisecond), Enabled: enabled,
	}
}

func chan1(concurrency int) model.Channel {
	return model.Channel{
		ID: "ch1", Protocol: "virtual", Transport: model.Transport{Kind: "memory"},
		Concurrency: concurrency, Timeout: model.Duration(500 * time.Millisecond),
	}
}

func startSched(t *testing.T, opt Options) (*Scheduler, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(opt, nil, nil)
	s.Start(ctx)
	t.Cleanup(func() { cancel(); s.Close(2 * time.Second) })
	return s, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// 同一设备的采集永不并发：慢驱动 + 短周期只会产生 skipped，不会堆协程。
func TestDeviceActorSerializesCollects(t *testing.T) {
	tgt := &fakeTarget{block: make(chan struct{})}
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	if err := s.Add(dev("d1", 5*time.Millisecond, true), chan1(4), tgt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first collect", func() bool { return tgt.collects.Load() >= 1 })
	time.Sleep(60 * time.Millisecond)
	close(tgt.block)

	if got := tgt.maxInflight.Load(); got != 1 {
		t.Fatalf("max inflight = %d, want 1", got)
	}
	st, ok := s.StatusOf("d1")
	if !ok {
		t.Fatal("missing status")
	}
	if st.Stats.Skipped == 0 {
		t.Fatalf("expected skipped ticks while busy, got %+v", st.Stats)
	}
	// 注册时是 connecting，第一次成功采集之后才转 collecting；这里要等
	// 那一次迁移真的发生，而不是依赖「Add 时就已经是 collecting」。
	waitFor(t, "collecting after unblock", func() bool {
		cur, ok := s.StatusOf("d1")
		return ok && (cur.RuntimeState == StateCollecting || cur.RuntimeState == StateDegraded)
	})
}

func TestFailureDegradesThenBreaks(t *testing.T) {
	tgt := &fakeTarget{}
	tgt.setCollectErr(driver.E(driver.ErrTimeout, "read", "d1", "boom", true, nil))
	s, _ := startSched(t, Options{
		Tick: 2 * time.Millisecond, BreakerThreshold: 2,
		BreakerCooldown: 50 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		Jitter: 0.001,
	})
	if err := s.Add(dev("d1", 5*time.Millisecond, true), chan1(1), tgt); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "degraded", func() bool {
		st, _ := s.StatusOf("d1")
		return st.RuntimeState == StateDegraded || st.RuntimeState == StateFaulted
	})
	waitFor(t, "faulted", func() bool {
		st, _ := s.StatusOf("d1")
		return st.RuntimeState == StateFaulted
	})
	st, _ := s.StatusOf("d1")
	if st.Stats.ConsecutiveFailures < 2 {
		t.Fatalf("failures = %d", st.Stats.ConsecutiveFailures)
	}
	if st.Stats.LastErrorKind != string(driver.ErrTimeout) {
		t.Fatalf("lastErrorKind = %q", st.Stats.LastErrorKind)
	}
	// 熔断后转为探活而不是继续读：探活失败仍然要能恢复采集路径。
	tgt.setProbeErr(nil)
	waitFor(t, "probe", func() bool { return tgt.probes.Load() >= 1 })
}

// 地址错误不计入熔断：这是配置错误，重试没有意义，但也不该把设备关掉。
func TestAddressErrorDoesNotTripBreaker(t *testing.T) {
	tgt := &fakeTarget{}
	tgt.setCollectErr(driver.E(driver.ErrAddress, "read", "d1", "bad register", false, nil))
	s, _ := startSched(t, Options{
		Tick: 2 * time.Millisecond, BreakerThreshold: 2,
		MaxBackoff: 10 * time.Millisecond, Jitter: 0.001,
	})
	if err := s.Add(dev("d1", 5*time.Millisecond, true), chan1(1), tgt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "several failures", func() bool {
		st, _ := s.StatusOf("d1")
		return st.Stats.ConsecutiveFailures >= 4
	})
	st, _ := s.StatusOf("d1")
	if st.RuntimeState == StateFaulted {
		t.Fatalf("address error must not fault the device: %+v", st.Stats)
	}
}

// Restart 之后旧租约必须失效，在飞结果不允许写回。
func TestRestartInvalidatesLease(t *testing.T) {
	tgt := &fakeTarget{block: make(chan struct{}), entered: make(chan struct{}, 8)}
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	if err := s.Add(dev("d1", 10*time.Millisecond, true), chan1(1), tgt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tgt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("collect never entered")
	}
	lease, _ := tgt.lastLease.Load().(*Lease)
	if lease == nil || !lease.Live() {
		t.Fatal("lease should be live before restart")
	}
	genBefore, _ := s.Generation("d1")
	if err := s.Restart("d1"); err != nil {
		t.Fatal(err)
	}
	genAfter, _ := s.Generation("d1")
	if genAfter <= genBefore {
		t.Fatalf("generation did not advance: %d -> %d", genBefore, genAfter)
	}
	if lease.Live() {
		t.Fatal("lease from old generation must not be live")
	}
	close(tgt.block)
	waitFor(t, "collect after restart", func() bool { return tgt.collects.Load() >= 2 })
}

func TestRemoveStopsScheduling(t *testing.T) {
	tgt := &fakeTarget{}
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	if err := s.Add(dev("d1", 5*time.Millisecond, true), chan1(1), tgt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "collects", func() bool { return tgt.collects.Load() >= 1 })
	if !s.Remove("d1") {
		t.Fatal("remove returned false")
	}
	if s.Remove("d1") {
		t.Fatal("double remove must return false")
	}
	waitFor(t, "close", func() bool { return tgt.closes.Load() >= 1 })
	n := tgt.collects.Load()
	time.Sleep(40 * time.Millisecond)
	if got := tgt.collects.Load(); got > n+1 {
		t.Fatalf("collects continued after remove: %d -> %d", n, got)
	}
	if _, ok := s.StatusOf("d1"); ok {
		t.Fatal("status should be gone")
	}
}

func TestDisabledDeviceIsNotScheduled(t *testing.T) {
	tgt := &fakeTarget{}
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	if err := s.Add(dev("d1", 5*time.Millisecond, false), chan1(1), tgt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if n := tgt.collects.Load(); n != 0 {
		t.Fatalf("disabled device collected %d times", n)
	}
	st, _ := s.StatusOf("d1")
	if st.RuntimeState != StateDisabled || st.ConfiguredState != "disabled" {
		t.Fatalf("status = %+v", st)
	}

	// 运行期启用后应立刻开始采集（配置态仍保持 disabled）。
	if err := s.SetEnabled("d1", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "collect after enable", func() bool { return tgt.collects.Load() >= 1 })
	st, _ = s.StatusOf("d1")
	if st.ConfiguredState != "disabled" {
		t.Fatal("configuredState must reflect config, not runtime")
	}
	if err := s.SetEnabled("d1", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disabled state", func() bool {
		st, _ := s.StatusOf("d1")
		return st.RuntimeState == StateDisabled
	})
}

func TestSubmitWriteSerializesWithCollect(t *testing.T) {
	tgt := &fakeTarget{block: make(chan struct{})}
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	if err := s.Add(dev("d1", 10*time.Millisecond, true), chan1(2), tgt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "collect", func() bool { return tgt.collects.Load() >= 1 })

	var wrote atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- s.SubmitWrite(context.Background(), "d1", func(ctx context.Context) error {
			if tgt.inflight.Load() != 0 {
				t.Error("write ran while a collect was in flight")
			}
			wrote.Store(true)
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	if wrote.Load() {
		t.Fatal("write must wait for the device slot")
	}
	close(tgt.block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write never completed")
	}
	if !wrote.Load() {
		t.Fatal("write did not run")
	}
}

func TestSubmitWriteUnknownDevice(t *testing.T) {
	s, _ := startSched(t, Options{})
	if err := s.SubmitWrite(context.Background(), "ghost", func(context.Context) error { return nil }); err != ErrDeviceNotFound {
		t.Fatalf("err = %v", err)
	}
	if err := s.Restart("ghost"); err != ErrDeviceNotFound {
		t.Fatalf("restart err = %v", err)
	}
	if err := s.SetEnabled("ghost", true); err != ErrDeviceNotFound {
		t.Fatalf("setenabled err = %v", err)
	}
}

func TestAddRejectsDuplicates(t *testing.T) {
	s, _ := startSched(t, Options{})
	tgt := &fakeTarget{}
	if err := s.Add(dev("d1", time.Second, true), chan1(1), tgt); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(dev("d1", time.Second, true), chan1(1), &fakeTarget{}); err == nil {
		t.Fatal("expected duplicate error")
	}
	if err := s.Add(model.Device{}, chan1(1), tgt); err == nil {
		t.Fatal("expected missing id error")
	}
	if err := s.Add(dev("d2", time.Second, true), chan1(1), nil); err == nil {
		t.Fatal("expected missing target error")
	}
}

// 通道闸门：写请求优先，且并发不超过通道上限。
func TestGateWritePriority(t *testing.T) {
	g := newGate(1)
	ctx := context.Background()
	if err := g.acquire(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := g.inflight(); got != 1 {
		t.Fatalf("inflight = %d", got)
	}

	readDone := make(chan struct{})
	go func() {
		// 读请求会先等写请求让行。
		time.Sleep(10 * time.Millisecond)
		if err := g.acquire(ctx, false); err == nil {
			g.release()
		}
		close(readDone)
	}()
	writeDone := make(chan struct{})
	go func() {
		if err := g.acquire(ctx, true); err == nil {
			g.release()
		}
		close(writeDone)
	}()
	time.Sleep(20 * time.Millisecond)
	g.release()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("write never acquired the gate")
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read never acquired the gate")
	}
}

func TestGateHonorsContext(t *testing.T) {
	g := newGate(1)
	if err := g.acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentDeviceGathers(t *testing.T) {
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		tgt := &fakeTarget{}
		id := "d" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := s.Add(dev(id, 5*time.Millisecond, true), chan1(8), tgt); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	wg.Wait()
	waitFor(t, "statuses", func() bool { return len(s.Status()) == 50 })
	for _, st := range s.Status() {
		if st.Stats.ReadsTotal == 0 {
			continue
		}
		if st.Stats.SuccessRate < 0 || st.Stats.SuccessRate > 1 {
			t.Fatalf("bad success rate: %+v", st.Stats)
		}
	}
}

// 状态迁移必须通过独立 goroutine 回调，且回调变慢时不能拖住采集。
func TestStateEventsDoNotBlockCollection(t *testing.T) {
	release := make(chan struct{})
	got := make(chan StateEvent, 8)

	tgt := &fakeTarget{}
	s, _ := startSched(t, Options{
		Tick: 2 * time.Millisecond,
		OnState: func(ev StateEvent) {
			// 模拟慢消费者（比如 Broker 卡住）：第一个事件就把回调钉住，
			// 后续事件应入队等待，而不是阻塞采集协程。
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
			got <- ev
		},
	})
	if err := s.Add(dev("d1", 10*time.Millisecond, true), chan1(2), tgt); err != nil {
		t.Fatalf("add: %v", err)
	}

	// 回调被钉住期间，采集仍必须继续推进。
	waitFor(t, "collecting", func() bool { return tgt.collects.Load() >= 3 })

	// 制造一次失败 → degraded，验证事件内容。
	tgt.setCollectErr(errors.New("boom"))
	waitFor(t, "collect attempts during stall", func() bool { return tgt.collects.Load() >= 6 })

	close(release)
	select {
	case ev := <-got:
		if ev.DeviceID != "d1" || ev.To == "" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("state event never delivered")
	}
}

// 没有 OnState 时不得启动派发协程，也不得因 nil 回调 panic。
func TestStateEventsWithoutHook(t *testing.T) {
	tgt := &fakeTarget{}
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	if err := s.Add(dev("d1", 10*time.Millisecond, true), chan1(1), tgt); err != nil {
		t.Fatalf("add: %v", err)
	}
	waitFor(t, "collecting", func() bool { return tgt.collects.Load() >= 2 })
	if n := len(s.events); n != 0 {
		t.Fatalf("events queued without hook: %d", n)
	}
}

// 注册设备时必须发布一次初始状态事件。
//
// 这不是「锦上添花」：北向的 `.../$state` 是 retained 的当前事实，消费方
// 第一次连上时要能读到每台设备是否在线。若注册时静默把状态设为 collecting
// 且不发事件，一台从未失败过的设备就永远不会有状态消息——看板上它看起来
// 像「网关根本没在采」，而实际采集一切正常。
func TestAddPublishesInitialState(t *testing.T) {
	got := make(chan StateEvent, 8)
	s, _ := startSched(t, Options{
		Tick:    2 * time.Millisecond,
		OnState: func(ev StateEvent) { got <- ev },
	})
	if err := s.Add(dev("d1", 20*time.Millisecond, true), chan1(1), &fakeTarget{}); err != nil {
		t.Fatalf("add: %v", err)
	}
	select {
	case ev := <-got:
		if ev.DeviceID != "d1" {
			t.Fatalf("event = %+v", ev)
		}
		if ev.From != "" {
			t.Fatalf("initial event From = %q, want empty", ev.From)
		}
		// 注册时设备还没读过一次，不能声称「正在正常采集」。
		if ev.To != StateConnecting {
			t.Fatalf("initial state = %q, want %q", ev.To, StateConnecting)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial state event on Add: retained $state would never exist")
	}
}

// 停用的设备注册时也必须发一次，且直接是 disabled：
// 消费方要能区分「没这台设备」与「这台设备被停用了」。
func TestAddDisabledDevicePublishesDisabled(t *testing.T) {
	got := make(chan StateEvent, 8)
	s, _ := startSched(t, Options{
		Tick:    2 * time.Millisecond,
		OnState: func(ev StateEvent) { got <- ev },
	})
	if err := s.Add(dev("d1", 20*time.Millisecond, false), chan1(1), &fakeTarget{}); err != nil {
		t.Fatalf("add: %v", err)
	}
	select {
	case ev := <-got:
		if ev.To != StateDisabled {
			t.Fatalf("state = %q, want disabled", ev.To)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial state event for a disabled device")
	}
}

// LastTick 在 Start 之前必须报告 unknown（false）：调用方据此区分「刚启动」
// 与「真的卡死」，绝不能把「还没开始」误报成停摆。
func TestLastTickUnknownBeforeStart(t *testing.T) {
	s := New(Options{Tick: time.Millisecond}, nil, nil)
	if _, ok := s.LastTick(); ok {
		t.Fatal("未 Start 就声称有 tick 时刻")
	}
}

// Start 之后 tick 时间戳必须持续推进：这是「进程活着但采集中枢卡死」检测
// 的唯一信号源。若这里不动，运行时看门狗就永远判不了卡死。
func TestLastTickAdvances(t *testing.T) {
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	first, ok := s.LastTick()
	if !ok {
		t.Fatal("Start 后 LastTick 应已知")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cur, ok := s.LastTick(); ok && cur.After(first) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("tick 时间戳从不前进：卡死检测将永远失效")
}

// 停摆检测测的是「循环活性」而非「有没有设备要采」：没有设备时循环仍在转，
// 时间戳照常前进，不能被误判成卡死。
func TestLastTickAdvancesWithNoDevices(t *testing.T) {
	s, _ := startSched(t, Options{Tick: 2 * time.Millisecond})
	first, ok := s.LastTick()
	if !ok {
		t.Fatal("Start 后 LastTick 应已知")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cur, ok := s.LastTick(); ok && cur.After(first) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("空设备表的调度器不再推进 tick 时间戳")
}

// Start 必须在启动线程内同步种子 tick 时刻：看门狗若在 Start 之后、首个 tick
// 之前读 LastTick，必须得到「已知」而不是 unknown。否则刚启动的一小段时间会
// 落入「无法判定」，并让「启动即卡死」这一场景在头一个 tick 间隔内不可见。
func TestLastTickKnownImmediatelyAfterStart(t *testing.T) {
	s := New(Options{Tick: time.Hour}, nil, nil) // 周期极长：loop 不可能已经 tick
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Close(time.Second)

	if _, ok := s.LastTick(); !ok {
		t.Fatal("Start 后、首个 tick 前 LastTick 应已知（Start 必须种子时刻）")
	}
}
