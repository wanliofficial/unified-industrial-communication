package coap

// Observe（RFC 7641）集成测试。跑在真实 UDP 上，因为要验证的是「两条套接字、
// 一条同步事务、一条异步推送」的时序，内存链路会把阻塞与并发这两件最关键
// 的事抹掉。
//
// 断言原则与 OPC UA 的订阅测试一致：看推送必须查本地最新值表（obs.lookup），
// 不能只看 Read——Read 在表未命中时会真读，于是「服务端改了值」会被误当成
// 「推送已经到了」，测试假绿。

import (
	"context"
	"testing"
	"time"

	coapcodec "uic/internal/codec/coap"
	"uic/internal/driver"
	"uic/internal/model"
	simcoap "uic/internal/sim/coap"
)

// openSimSub 启动内置服务端并建立一个 Observe 订阅（Channel.Subscribe=true）。
func openSimSub(t *testing.T, path string, dt string) (driver.Conn, *simcoap.Server) {
	t.Helper()
	s, err := simcoap.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dev := baseDevice(pt("p", path, "", dt))
	cfg := baseConfig(s.Addr(), dev)
	cfg.Channel.Subscribe = true
	ch, err := New().Open(context.Background(), cfg)
	if err != nil {
		_ = s.Close()
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch, s
}

// waitForCache 等本地最新值表出现期望的数值（或超时）。
func waitForCache(t *testing.T, c *conn, pointID string, dt model.DataType, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.obs != nil {
			if sv, ok := c.obs.lookup(pointID, dt); ok {
				if sv.Quality == model.QualityGood && sv.Value.Num == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got string
	healthy := false
	if c.obs != nil {
		healthy = c.obs.healthy()
		if sv, ok := c.obs.lookup(pointID, dt); ok {
			got = sv.Value.String()
		}
	}
	t.Fatalf("observe cache never became %v (got %q, healthy=%v)", want, got, healthy)
}

// 订阅刚建立时还没有任何通知：第一轮 Read 必须从服务端真读一次，不能返回零值。
func TestObserveFirstReadFallsBackToServer(t *testing.T) {
	ch, _ := openSimSub(t, "/sensors/temp", "float64")
	smp := readOne(t, ch, pt("p", "/sensors/temp", "", "float64"))
	if smp.Err != nil {
		t.Fatalf("read: %v", smp.Err)
	}
	if smp.Quality != model.QualityGood || smp.Value.Num != 21.5 {
		t.Fatalf("sample = %+v", smp)
	}
}

// 设备主动推送的变化必须回流到 Read，且之后 Read 用推送值（不再真读）。
func TestObservePushesServerChanges(t *testing.T) {
	ch, s := openSimSub(t, "/sensors/temp", "float64")
	c := ch.(*conn)

	// 等初始样本进入推送表。
	waitForCache(t, c, "p", model.TypeFloat64, 21.5)

	// 服务端改值 → 推一条通知 → 表更新。
	if !s.SetValue("/sensors/temp", "33.5") {
		t.Fatal("SetValue failed")
	}
	waitForCache(t, c, "p", model.TypeFloat64, 33.5)

	// 推送到的值必须能被 Read 直接消费，且不再触发真读（通知计数不涨）。
	before := s.Requests()
	smp := readOne(t, ch, pt("p", "/sensors/temp", "", "float64"))
	if smp.Err != nil || smp.Value.Num != 33.5 {
		t.Fatalf("read after push = %+v", smp)
	}
	if after := s.Requests(); after != before {
		t.Fatalf("Read hit the server (%d new requests) instead of using the pushed value", after-before)
	}
}

// 通知的 Observe 序号乱序/重复时，旧值必须被丢弃，不能把值推回去。
func TestObserveDropsStaleNotification(t *testing.T) {
	ch, _ := openSimSub(t, "/sensors/temp", "float64")
	c := ch.(*conn)
	waitForCache(t, c, "p", model.TypeFloat64, 21.5)

	// 直接构造一条「序号比当前小 1」的通知（乱序到达），payload 是旧值。
	// 它应被 ObserveFresh 判为旧值丢弃。
	p := c.obs.points["p"]
	c.obs.mu.Lock()
	prevSeq := p.seq
	c.obs.mu.Unlock()
	if prevSeq == 0 {
		t.Skip("server did not assign a non-zero sequence")
	}
	stale := coapcodec.Message{
		Type: coapcodec.TypeNonConfirmable, Code: coapcodec.CodeContent, MID: 4242,
		Token:   append([]byte(nil), p.token...),
		Options: []coapcodec.Option{{Number: coapcodec.OptObserve, Value: coapcodec.EncodeUint(prevSeq - 1)}},
		Payload: []byte("9.9"),
	}
	c.obs.handleMessage(stale)

	if sv, ok := c.obs.lookup("p", model.TypeFloat64); ok && sv.Value.Num == 9.9 {
		t.Fatalf("stale notification overwrote fresh value: %+v", sv)
	}
}

// 空载荷通知（RFC 7641 允许服务端用空 2.04 表示「无变化」）绝不能把表里的
// 上一条成功读数清成零值——它只应刷新到达时刻、延长保鲜期。
func TestObserveEmptyNotificationKeepsLastValue(t *testing.T) {
	ch, _ := openSimSub(t, "/sensors/temp", "float64")
	c := ch.(*conn)
	waitForCache(t, c, "p", model.TypeFloat64, 21.5)

	p := c.obs.points["p"]
	c.obs.mu.Lock()
	nextSeq := p.seq + 1
	c.obs.mu.Unlock()

	empty := coapcodec.Message{
		Type: coapcodec.TypeNonConfirmable, Code: coapcodec.CodeContent, MID: 5252,
		Token:   append([]byte(nil), p.token...),
		Options: []coapcodec.Option{{Number: coapcodec.OptObserve, Value: coapcodec.EncodeUint(nextSeq)}},
		// Payload 故意留空：表示「无变化」。
	}
	c.obs.handleMessage(empty)

	sv, ok := c.obs.lookup("p", model.TypeFloat64)
	if !ok {
		t.Fatalf("empty notification evicted the last good value")
	}
	if sv.Quality != model.QualityGood || sv.Value.Num != 21.5 {
		t.Fatalf("empty notification corrupted the value: %+v", sv)
	}
}

// 订阅链路静默超过保鲜期时，Read 必须退回真实 GET——拿最后一次推送冒充
// 当前值是这类实现最危险的失效模式。
func TestObserveFallsBackWhenStale(t *testing.T) {
	ch, s := openSimSub(t, "/sensors/temp", "float64")
	c := ch.(*conn)
	waitForCache(t, c, "p", model.TypeFloat64, 21.5)

	// 强制把推送值标记为「很久以前到达」，模拟链路静默。
	c.obs.mu.Lock()
	if v, ok := c.obs.values["p"]; ok {
		v.at = time.Now().Add(-10 * time.Minute)
		c.obs.values["p"] = v
	}
	c.obs.mu.Unlock()

	// 服务端此刻的真实值已变，但推送表过期；Read 应真读拿到新值。
	if !s.SetValue("/sensors/temp", "42.0") {
		t.Fatal("SetValue failed")
	}
	smp := readOne(t, ch, pt("p", "/sensors/temp", "", "float64"))
	if smp.Err != nil || smp.Value.Num != 42.0 {
		t.Fatalf("stale fallback read = %+v, want 42.0", smp)
	}
}

// 未启用订阅（Channel.Subscribe=false）时，obs 为 nil，Read 全走真读。
func TestNoSubscribeUsesPolling(t *testing.T) {
	ch, _ := openSim(t, nil, pt("p", "/sensors/temp", "", "float64"))
	if c := ch.(*conn); c.obs != nil {
		t.Fatal("obs must be nil when Channel.Subscribe=false")
	}
	smp := readOne(t, ch, pt("p", "/sensors/temp", "", "float64"))
	if smp.Err != nil || smp.Value.Num != 21.5 {
		t.Fatalf("polling read = %+v", smp)
	}
}

// 空的观察状态（没有点位）应全部 miss，交给真读。
func TestObserveEmptyStateMisses(t *testing.T) {
	o := newObserveState("d", nil, nil, nil)
	if _, ok := o.lookup("none", model.TypeFloat64); ok {
		t.Fatal("empty observe state must miss")
	}
	if o.healthy() {
		t.Fatal("empty observe state must not be healthy")
	}
}

// 关闭后向服务端注销观察：观察者计数应回落到 0。
func TestObserveDeregistersOnClose(t *testing.T) {
	ch, s := openSimSub(t, "/sensors/temp", "float64")
	c := ch.(*conn)
	waitForCache(t, c, "p", model.TypeFloat64, 21.5)

	if n := s.Observers("/sensors/temp"); n != 1 {
		t.Fatalf("observers = %d, want 1", n)
	}
	_ = ch.Close()

	// 注销是异步的（GET+Observe:1 发出去后由服务端处理），给一点时间。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Observers("/sensors/temp") == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("observer not deregistered after close (still %d)", s.Observers("/sensors/temp"))
}
