package opcua

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/driver"
	"uic/internal/model"
	opcuaserver "uic/internal/opcuaserver"
)

// 订阅测试跑在真实 TCP 上。原因和 driver_test.go 一样：这里要验证的是
// 「两条连接、两套会话、一条被 Publish 长期占用」的状态机，内存流会把
// 阻塞与并发这两件最关键的事都抹掉。

func subChannel(addr string, interval time.Duration) model.Channel {
	ch := channel(addr)
	ch.Subscribe = true
	ch.PublishingInterval = model.Duration(interval)
	return ch
}

func openSub(t *testing.T, srv *opcuaserver.Server, interval time.Duration) (*conn, func()) {
	t.Helper()
	ch := subChannel(srv.Addr(), interval)
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "ua-sub", ChannelID: ch.ID,
			Interval: model.Duration(interval), Enabled: true,
			Points: []model.Point{{
				ID: "temp", Enabled: true, DataType: model.TypeFloat64,
				Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Temperature", Index: -1},
			}},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cc, ok := c.(*conn)
	if !ok {
		t.Fatalf("unexpected conn type %T", c)
	}
	return cc, func() { _ = c.Close() }
}

func tempSpan() []driver.ReadSpan {
	return []driver.ReadSpan{span("sym:Plant",
		nodePt("temp", "ns=2;s=Plant.Temperature", model.TypeFloat64),
	)}
}

// waitForValue 轮询 Read 直到点位变成 want（或超时）。
//
// 用轮询而不是「等一个信号」：订阅是异步推送，测试要么等外部事件、
// 要么自己采样；采样能同时验证「推送最终会反映到 Read 上」这件事。
func waitForValue(t *testing.T, c driver.Conn, want float64) time.Duration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	start := time.Now()
	for time.Now().Before(deadline) {
		samples, err := c.Read(context.Background(), tempSpan())
		if err == nil && len(samples) == 1 && samples[0].Quality == model.QualityGood {
			if samples[0].Value.Type == model.TypeFloat64 && samples[0].Value.Num == want {
				return time.Since(start)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("temperature never became %v", want)
	return 0
}

// waitForCache 等本地最新值表出现期望值。断言推送必须看缓存而不能只看
// Read：Read 在缓存未命中时会真读，于是「服务端已经改了值」会被误当成
// 「推送已经到了」——测试会假绿。
func waitForCache(t *testing.T, c *conn, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sv, ok := c.sub.lookup("temp", model.TypeFloat64); ok {
			if sv.Quality == model.QualityGood && sv.Value.Type == model.TypeFloat64 && sv.Value.Num == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	sv, ok := c.sub.lookup("temp", model.TypeFloat64)
	t.Fatalf("subscription cache never became %v (got %+v, ok=%v, healthy=%v)",
		want, sv, ok, c.sub.isHealthy())
}

// 订阅刚建立时还没有任何通知，第一轮 Read 必须从服务端真读一次，
// 不能返回零值。
func TestSubscribeFirstReadFallsBackToServer(t *testing.T) {
	srv := newSim(t)
	c, closeFn := openSub(t, srv, 200*time.Millisecond)
	defer closeFn()

	samples, err := c.Read(context.Background(), tempSpan())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d", len(samples))
	}
	s := samples[0]
	if s.Quality != model.QualityGood || s.Value.Type != model.TypeFloat64 || s.Value.Num != 21.5 {
		t.Fatalf("sample = %+v", s)
	}
}

// 服务端变化必须通过推送回流到 Read，且不再依赖每轮真读。
func TestSubscribePushesServerChanges(t *testing.T) {
	srv := newSim(t)
	c, closeFn := openSub(t, srv, 60*time.Millisecond)
	defer closeFn()

	if _, err := c.Read(context.Background(), tempSpan()); err != nil {
		t.Fatalf("read: %v", err)
	}
	// 先等初始样本进缓存，再改服务端值，才能确定第二次变化是推送来的。
	waitForCache(t, c, 21.5)

	srv.Set("ns=2;s=Plant.Temperature", ua.Variant{Type: ua.TypeDouble, Float: 33.5})
	waitForCache(t, c, 33.5)

	// 推送到的值必须能被 Read 直接消费。
	samples, err := c.Read(context.Background(), tempSpan())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Num != 33.5 || samples[0].Quality != model.QualityGood {
		t.Fatalf("sample = %+v", samples[0])
	}
}

// 订阅连接断开时，Read 必须回退到真实读——拿最后一次成功的读数冒充
// 当前值，是这类实现最危险的失效模式。
func TestSubscribeFallsBackToPollingWhenDown(t *testing.T) {
	srv := newSim(t)
	c, closeFn := openSub(t, srv, 60*time.Millisecond)
	defer closeFn()

	if _, err := c.Read(context.Background(), tempSpan()); err != nil {
		t.Fatalf("read: %v", err)
	}
	waitForCache(t, c, 21.5)

	// 彻底停掉发布循环，而不是只翻一下 healthy 标志：否则下一轮保活
	// 会把它重新置为健康，测试就变成了看调度运气。
	c.subCancel()
	if !c.sub.waitDone(2 * time.Second) {
		t.Fatal("publisher did not stop")
	}
	if c.sub.isHealthy() {
		t.Fatal("subscription still reported healthy")
	}

	// 本地表里还留着 21.5，但必须不再被信任。
	srv.Set("ns=2;s=Plant.Temperature", ua.Variant{Type: ua.TypeDouble, Float: 19.25})

	samples, err := c.Read(context.Background(), tempSpan())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityGood {
		t.Fatalf("sample = %+v", samples)
	}
	if samples[0].Value.Num != 19.25 {
		t.Fatalf("read returned stale value %v", samples[0].Value.Num)
	}
}

// 订阅模式下的写入必须能立刻读回：写走主连接，推送走订阅连接，
// 两条链路的状态不能互相看不见。
func TestSubscribeWriteThenReadBack(t *testing.T) {
	srv := newSim(t)
	c, closeFn := openSub(t, srv, 60*time.Millisecond)
	defer closeFn()

	ctx := context.Background()
	results, err := c.Write(ctx, []driver.WriteRequest{{
		PointID:  "sp",
		DataType: model.TypeFloat64,
		Address:  model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Setpoint", Index: -1},
		Value:    model.Value{Type: model.TypeFloat64, Num: 44},
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v", results)
	}

	samples, err := c.Read(ctx, []driver.ReadSpan{span("s",
		nodePt("sp", "ns=2;s=Plant.Setpoint", model.TypeFloat64),
	)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Num != 44 {
		t.Fatalf("setpoint = %v", samples[0].Value.Num)
	}
}

// 配置了订阅但节点 ID 非法：Open 直接失败。静默退化成轮询会让
// 「配置写着订阅、实际在轮询」永远没人发现。
func TestSubscribeRejectsInvalidNodeID(t *testing.T) {
	srv := newSim(t)
	ch := subChannel(srv.Addr(), 100*time.Millisecond)
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "ua-bad", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true,
			Points: []model.Point{{
				ID: "p1", Enabled: true, DataType: model.TypeFloat64,
				Address: model.Address{Kind: model.AddressSymbol, Path: "ns=x;i=1", Index: -1},
			}},
		},
	})
	if err == nil {
		t.Fatal("open must fail")
	}
	if k := driver.KindOf(err); k != driver.ErrAddress {
		t.Fatalf("kind = %s (%v)", k, err)
	}
}

// Close 不能被长轮询拖住：Publish 的等待上限是 15 秒起，
// 关闭必须在一秒内完成。
func TestCloseDoesNotWaitForPublishTimeout(t *testing.T) {
	srv := newSim(t)
	c, _ := openSub(t, srv, 200*time.Millisecond)
	if _, err := c.Read(context.Background(), tempSpan()); err != nil {
		t.Fatalf("read: %v", err)
	}

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("close took %v", d)
	}
	// 幂等：第二次关闭不能 panic 也不能重新建链。
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// 没有启用的点位时不建立订阅，也不该失败：一台全停用的设备仍然要能
// 启动并被健康检查看到。
func TestSubscribeWithNoEnabledPointsUsesPolling(t *testing.T) {
	srv := newSim(t)
	ch := subChannel(srv.Addr(), 100*time.Millisecond)
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "ua-idle", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true,
			Points: []model.Point{{
				ID: "p1", Enabled: false, DataType: model.TypeFloat64,
				Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Temperature", Index: -1},
			}},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()
	if cc := c.(*conn); cc.sub != nil {
		t.Fatal("no subscription expected when every point is disabled")
	}
}

// subState 的退化判定是三条独立条件：不健康、从未收到、超过保鲜期。
// 单测它比端到端更可靠——要构造「收过值但保鲜期已过」需要假时钟。
func TestSubLookupDegrades(t *testing.T) {
	points := []subPoint{{id: "p", node: ua.StringNode(2, "P"), dt: model.TypeFloat64}}
	s := newSubState("dev", points, 100*time.Millisecond, 100*time.Millisecond, nil, nil, func(string, ...any) {})

	if _, ok := s.lookup("p", model.TypeFloat64); ok {
		t.Fatal("no value yet: lookup must miss")
	}

	s.mu.Lock()
	s.values["p"] = subValue{val: model.Value{Type: model.TypeFloat64, Num: 1}, quality: model.QualityGood, at: time.Now()}
	s.healthy = true
	s.lastAt = time.Now()
	s.mu.Unlock()

	if v, ok := s.lookup("p", model.TypeFloat64); !ok || v.Value.Num != 1 {
		t.Fatalf("lookup = %+v %v", v, ok)
	}
	// 类型变了：旧值不可比，必须交给真实读。
	if _, ok := s.lookup("p", model.TypeString); ok {
		t.Fatal("type change must miss")
	}
	// 超过保鲜期。
	s.mu.Lock()
	s.lastAt = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if _, ok := s.lookup("p", model.TypeFloat64); ok {
		t.Fatal("stale subscription must miss")
	}
	// 不健康。
	s.mu.Lock()
	s.lastAt = time.Now()
	s.healthy = false
	s.mu.Unlock()
	if _, ok := s.lookup("p", model.TypeFloat64); ok {
		t.Fatal("unhealthy subscription must miss")
	}
}

// 保活超时必须用服务端修订后的参数计算，并且有下限：把 100ms 的发布
// 周期算成 1 秒就判死，会让健康的订阅在正常的网络抖动下反复重建。
func TestKeepAliveTimeoutFloor(t *testing.T) {
	points := []subPoint{{id: "p", node: ua.StringNode(2, "P"), dt: model.TypeFloat64}}
	s := newSubState("dev", points, 100*time.Millisecond, 100*time.Millisecond, nil, nil, func(string, ...any) {})
	if got := s.keepAliveTimeout(); got != minKeepAliveTimeout {
		t.Fatalf("timeout = %v", got)
	}
	s.pubEvery = 2 * time.Second
	s.keepAlive = 10
	if got := s.keepAliveTimeout(); got != 40*time.Second {
		t.Fatalf("timeout = %v", got)
	}
	if s.staleAfter() <= s.keepAliveTimeout() {
		t.Fatal("stale window must be wider than the keepalive timeout")
	}
}

// 序列号为 0 的响应不产生确认。规范里 0 表示「这条消息不占用序列号」
// （比如某些服务端的空响应），确认它会被判为未知序列号。
//
// 带序列号的保活消息仍然要确认：服务端按序列号推进重传队列，保活也占用
// 一个号，漏确认同样会让队列填满。
func TestAckOfSkipsZeroSequence(t *testing.T) {
	if acks := ackOf(7, ua.PublishResponse{}); len(acks) != 0 {
		t.Fatalf("acks = %+v", acks)
	}
	r := ua.PublishResponse{NotificationMessage: ua.NotificationMessage{SequenceNumber: 3}}
	acks := ackOf(7, r)
	if len(acks) != 1 || acks[0].SubscriptionID != 7 || acks[0].SequenceNumber != 3 {
		t.Fatalf("acks = %+v", acks)
	}
}

var errTest = errors.New("boom")

// fakeMetrics 只累加计数，用来断言「推送确实发生了」。
type fakeMetrics struct {
	mu     sync.Mutex
	counts map[string]float64
}

func (f *fakeMetrics) Counter(name string, _ map[string]string) func(float64) {
	return func(d float64) {
		f.mu.Lock()
		if f.counts == nil {
			f.counts = map[string]float64{}
		}
		f.counts[name] += d
		f.mu.Unlock()
	}
}

func (f *fakeMetrics) Observe(string, float64, map[string]string) {}

func (f *fakeMetrics) get(name string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[name]
}

func TestSubscribeCountsNotifications(t *testing.T) {
	srv := newSim(t)
	m := &fakeMetrics{}
	ch := subChannel(srv.Addr(), 60*time.Millisecond)
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "ua-metrics", ChannelID: ch.ID,
			Interval: model.Duration(60 * time.Millisecond), Enabled: true,
			Points: []model.Point{{
				ID: "temp", Enabled: true, DataType: model.TypeFloat64,
				Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Temperature", Index: -1},
			}},
		},
		Metrics: m,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()
	cc := c.(*conn)

	waitForCache(t, cc, 21.5)
	srv.Set("ns=2;s=Plant.Temperature", ua.Variant{Type: ua.TypeDouble, Float: 25})
	waitForCache(t, cc, 25)

	if got := m.get("uic_driver_notifications_total"); got < 2 {
		t.Fatalf("notifications = %v", got)
	}
}

// markDown 要把错误留下并立刻让 Read 不再信任本地表；重复调用只在
// 「从健康变为不健康」时打一条日志、计一次错。
func TestSubMarkDown(t *testing.T) {
	points := []subPoint{{id: "p", node: ua.StringNode(2, "P"), dt: model.TypeFloat64}}
	m := &fakeMetrics{}
	var logs int
	s := newSubState("dev", points, time.Second, time.Second, nil, m,
		func(string, ...any) { logs++ })

	s.mu.Lock()
	s.healthy = true
	s.mu.Unlock()

	s.markDown(errTest)
	if s.isHealthy() {
		t.Fatal("must be unhealthy after markDown")
	}
	s.mu.RLock()
	last := s.lastErr
	s.mu.RUnlock()
	if last != "boom" {
		t.Fatalf("lastErr = %q", last)
	}
	if logs != 1 {
		t.Fatalf("logs = %d", logs)
	}
	// 已经是不健康状态：不重复记日志，但错误计数照记——这些失败代表
	// 重连在持续失败，正是需要被看到的信号。
	s.markDown(errTest)
	if logs != 1 {
		t.Fatalf("logs after second markDown = %d", logs)
	}
	if got := m.get("uic_driver_subscription_errors_total"); got != 2 {
		t.Fatalf("error counter = %v", got)
	}
}

func TestSleepCtxHonoursCancellation(t *testing.T) {
	start := time.Now()
	if sleepCtx(context.Background(), 10*time.Millisecond) != true {
		t.Fatal("sleep should complete")
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("sleep returned early")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleep should be interrupted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not interrupt sleep")
	}
}

func TestStatusCodeErrorsKeepTheirKind(t *testing.T) {
	if k := statusKindOf(ua.StatusBadNodeIDUnknown); k != driver.ErrAddress {
		t.Fatalf("node id unknown kind = %s", k)
	}
	if k := statusKindOf(ua.StatusBadNoCommunication); k != driver.ErrDisconnected {
		t.Fatalf("no communication kind = %s", k)
	}
	if k := statusKindOf(errTest); k != driver.ErrInternal {
		t.Fatalf("plain error kind = %s", k)
	}
}
