package mqtt_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"uic/internal/mqtt"
	"uic/internal/mqtt/mqtttest"
)

// 回归：半死链路（Broker 不回 PINGRESP）下两个并发的 Ping 都必须报告失败。
//
// 历史缺陷：pingWait 是 chan struct{}，被 PINGRESP（成功）与 dropConn（链路
// 已死）用同一个 close() 关闭。后到的 Ping 等 prev 一被关闭就返回 nil——
// 无法区分「链路还活着」与「链路刚断」，于是在死链路上误报探测成功。
// 修复：pingWait 改为 chan error，PINGRESP 送 nil、dropConn 送连接已断。
func TestConcurrentPingDuringDeadLinkBothFail(t *testing.T) {
	b := mqtttest.New(t, mqtttest.IgnoringPing)
	c := newClient(b, func(o *mqtt.Options) {
		o.KeepAlive = 30 * time.Second // keepaliveLoop 不抢先发 ping
		o.AckTimeout = 1 * time.Second
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	err1 := make(chan error, 1)
	go func() { err1 <- c.Ping(ctx) }()
	time.Sleep(100 * time.Millisecond) // 让第一个 Ping 先占住 pingWait
	err2 := c.Ping(ctx)                // 等 prev
	e1 := <-err1

	for _, e := range []error{e1, err2} {
		if e == nil {
			t.Fatal("死链路上 Ping 不应返回成功（nil）")
		}
	}
	if !errors.Is(err2, context.DeadlineExceeded) && err2 == nil {
		t.Fatalf("err2 = %v", err2)
	}
	t.Logf("first=%v second=%v", e1, err2)
}

// 回归：链路健康时，并发的两个 Ping 都必须返回成功（nil），证明修复没有
// 把「真活着」也误判成断链。
func TestConcurrentPingOnHealthyLinkBothSucceed(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, func(o *mqtt.Options) {
		o.KeepAlive = 30 * time.Second
		o.AckTimeout = 2 * time.Second
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	err1 := make(chan error, 1)
	go func() { err1 <- c.Ping(ctx) }()
	time.Sleep(50 * time.Millisecond)
	err2 := c.Ping(ctx)
	if err1 := <-err1; err1 != nil {
		t.Fatalf("first ping on healthy link: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("second ping on healthy link: %v", err2)
	}
}

// scriptedBroker 起一个最小 MQTT 对端，把已接受的连接交给 script。
//
// 用途是构造真实 Broker（含 sim/mqtt）不刻意模拟、但规范允许的报文顺序：
// SUBACK 之前的保留消息、延迟的 PINGRESP。顺序本身是协议的一部分，测试
// 不能只依赖「本仓库 Broker 恰好这么发」。
func scriptedBroker(t *testing.T, script func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}()
	return ln.Addr().String()
}

// 回归：SUBACK 之前到达的 QoS1 保留消息不得让 Connect 自锁。
//
// 历史缺陷：Connect 全程持有 c.mu（连接状态机依赖它串行化），而订阅窗口里
// 分发的 PUBLISH 走 dispatchPublish，后者为了取 conn 再次 c.mu.Lock()。
// sync.Mutex 不可重入，于是收到 QoS1 保留消息的那一刻 Connect 永久卡死，
// 并且 Close/下一次 Publish 也一起卡在 c.mu 上。
//
// 规范允许 Broker 在 SUBACK 之前或之后发保留消息（§3.8.4 只要求确认订阅），
// 真实部署中其他发布者也可能在这条窗口里推来一条普通 PUBLISH，因此这不是
// 只存在于理论中的顺序。
func TestConnectDoesNotDeadlockOnQoS1PublishDuringSubscribe(t *testing.T) {
	addr := scriptedBroker(t, func(conn net.Conn) {
		if _, err := mqtt.ReadPacket(conn, 1<<20); err != nil { // CONNECT
			return
		}
		_, _ = conn.Write([]byte{mqtt.TypeConnack << 4, 2, 0, 0})
		pkt, err := mqtt.ReadPacket(conn, 1<<20) // SUBSCRIBE
		if err != nil {
			return
		}
		pid := binary.BigEndian.Uint16(pkt.Body[:2])
		pub, err := mqtt.PublishPacket(mqtt.Message{
			Topic: "uic/dev/state", Payload: []byte("1"), QoS: 1, Retain: true,
		}, 7)
		if err != nil {
			return
		}
		_, _ = conn.Write(pub) // 保留消息先于 SUBACK
		_, _ = conn.Write([]byte{mqtt.TypeSuback << 4, 3, byte(pid >> 8), byte(pid), 1})
		_, _ = io.Copy(io.Discard, conn) // 保持连接，等客户端关闭
	})

	got := make(chan mqtt.Publish, 4)
	c := mqtt.New(mqtt.Options{
		Addr:           addr,
		ClientID:       "uic-test",
		KeepAlive:      30 * time.Second,
		ConnectTimeout: 2 * time.Second,
		AckTimeout:     2 * time.Second,
		Subscriptions:  []mqtt.TopicFilter{{Filter: "uic/dev/#", QoS: 1}},
		OnMessage:      func(p mqtt.Publish) { got <- p },
	})

	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
	case <-time.After(3 * time.Second):
		// 卡死时不能再调用 c.Close()：它也要拿同一把锁。
		t.Fatal("Connect 自锁：订阅窗口内收到 QoS1 PUBLISH 时 dispatchPublish 二次加锁 c.mu")
	}
	defer c.Close()

	select {
	case p := <-got:
		if p.Topic != "uic/dev/state" || p.QoS != 1 {
			t.Fatalf("published message = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("订阅窗口内的保留消息没有分发给 OnMessage")
	}
}

// 回归：Publish 设置的写超时不得残留到连接上。
//
// 历史缺陷：Publish 在写之前 SetWriteDeadline(now+AckTimeout)，写完没有清除。
// Go 的写超时是粘在连接上的：一旦那一刻过去，之后所有写（keepalive 的
// PINGREQ、入站 QoS1 的 PUBACK、DISCONNECT）都立刻返回 i/o timeout。
// 表现是「发布间隔大于 AckTimeout 的连接会在下一次保活时被自己判死」，
// 于是网关周期性地掉线重连（Sparkplug 还会因此反复重建会话）。
func TestPublishDoesNotLeaveStickyWriteDeadline(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, func(o *mqtt.Options) {
		o.AckTimeout = 100 * time.Millisecond
		o.KeepAlive = 30 * time.Second // 让 keepaliveLoop 不参与本用例
	})
	defer c.Close()

	// 不带 deadline 的 ctx 走 AckTimeout 分支（onState 这类回调里的写法）。
	if err := c.Publish(context.Background(), mqtt.Message{Topic: "uic/t", Payload: []byte("x")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 等到 Publish 设的写截止时间已经过去，再走一次与保活相同的写路径。
	time.Sleep(300 * time.Millisecond)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("空闲后 Ping 失败：上一次 Publish 的写超时没有被清除: %v", err)
	}
}

// 回归：一次在飞的 PINGREQ 只能有一个结论，但结论必须广播给所有等待者。
//
// 历史缺陷：pingWait 是 chan error 且容量 1。PINGRESP 到达时只喂给一个
// 接收者，另一个并发 Ping 会一直等自己的 AckTimeout，在健康链路上误报
// 「无 PINGRESP」。driver 的 Read 自检与 Probe 会同时调用 Ping，因此这条
// 误报会直接变成一次不必要的断链重连。
func TestConcurrentPingBothWakeOnOnePingresp(t *testing.T) {
	addr := scriptedBroker(t, func(conn net.Conn) {
		if _, err := mqtt.ReadPacket(conn, 1<<20); err != nil { // CONNECT
			return
		}
		_, _ = conn.Write([]byte{mqtt.TypeConnack << 4, 2, 0, 0})
		if _, err := mqtt.ReadPacket(conn, 1<<20); err != nil { // PINGREQ
			return
		}
		// 延迟回应，确保第二个 Ping 已经挂到同一次在飞的探测上。
		time.Sleep(300 * time.Millisecond)
		_, _ = conn.Write([]byte{mqtt.TypePingresp << 4, 0})
		_, _ = io.Copy(io.Discard, conn)
	})

	c := mqtt.New(mqtt.Options{
		Addr:           addr,
		ClientID:       "uic-test",
		KeepAlive:      30 * time.Second,
		ConnectTimeout: 2 * time.Second,
		AckTimeout:     2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	p1 := make(chan error, 1)
	go func() { p1 <- c.Ping(ctx) }()
	time.Sleep(50 * time.Millisecond) // 让第一个 Ping 先占住 pingWait
	p2 := make(chan error, 1)
	go func() { p2 <- c.Ping(ctx) }()

	e1, e2 := <-p1, <-p2
	if e1 != nil || e2 != nil {
		t.Fatalf("健康链路上并发 Ping 必须都成功: first=%v second=%v", e1, e2)
	}
}

// 回归：入站 PUBLISH 的三处协议违约必须被拒，而不是被当成一条普通消息。
//
// 历史缺陷：ParsePublish 只校验了长度，QoS=3（保留值）、空主题、主题里带
// 通配符这三条规范 MUST 全部放行。它们在本地看起来只是「一条消息」：
// QoS3 会被当成「不是 1，所以不用回 PUBACK」，空主题的消息无处可投，
// 带 # 的主题在消费方看来像一个订阅。全部属于「宁可判坏」的输入。
func TestParsePublishRejectsProtocolViolations(t *testing.T) {
	// publish 构造一条 PUBLISH：flags 直接给，便于构造非法 QoS。
	// QoS>0 时补上 2 字节 packet id（报文体校验会要求它存在）。
	publish := func(flags byte, topic string, payload string) mqtt.Packet {
		qos := (flags >> 1) & 0x03
		body := []byte{byte(len(topic) >> 8), byte(len(topic))}
		body = append(body, topic...)
		if qos > 0 {
			body = append(body, 0x00, 0x07)
		}
		body = append(body, payload...)
		return mqtt.Packet{Type: mqtt.TypePublish, Flags: flags, Body: body}
	}

	cases := []struct {
		name  string
		flags byte
		topic string
	}{
		{"qos3", 0x06, "uic/dev/value"},    // QoS 位 = 11
		{"empty topic", 0x00, ""},          // §4.7.3 主题长度必须 ≥1
		{"wildcard hash", 0x00, "uic/#"},   // §3.3.2 发布主题不得含通配符
		{"wildcard plus", 0x00, "uic/+/v"}, // 同上
	}
	for _, tc := range cases {
		if _, err := mqtt.ParsePublish(publish(tc.flags, tc.topic, "x")); err == nil {
			t.Errorf("%s: ParsePublish 接受了非法报文（topic=%q flags=%#x）", tc.name, tc.topic, tc.flags)
		}
	}

	// 合法报文必须仍然通过，避免把守卫写成「全都拒」。
	ok := []struct {
		flags byte
		topic string
	}{
		{0x00, "uic/dev/value"},
		{0x02, "uic/dev/value"}, // QoS1
		{0x01, "uic/dev/state"}, // retain
		{0x0A, "a/b"},           // QoS1 + duplicate
	}
	for _, tc := range ok {
		if _, err := mqtt.ParsePublish(publish(tc.flags, tc.topic, "x")); err != nil {
			t.Errorf("合法报文被拒（topic=%q flags=%#x）: %v", tc.topic, tc.flags, err)
		}
	}
}

// 回归：Disconnect 必须优雅断开（发 DISCONNECT，Broker 不代发遗嘱），且断开后
// 客户端仍可再次建链。集群里非 leader 副本降主时靠它停掉站点级北向会话——若
// 直接掐 TCP，Broker 会广播 $death retained，把「整站离线」误报给所有消费方。
func TestDisconnectSuppressesWillAndAllowsReconnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, func(o *mqtt.Options) {
		o.Will = func() *mqtt.Will {
			return &mqtt.Will{Topic: "uic/site/$death", Payload: []byte(`{"online":false}`), QoS: 1, Retain: true}
		}
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Publish(ctx, mqtt.Message{Topic: "uic/site/d/p", Payload: []byte("1")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := c.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	b.WaitDisconnects(t, 1)
	if wills := b.Wills(); len(wills) != 0 {
		t.Fatalf("graceful Disconnect must not trigger the will, got %d: %+v", len(wills), wills)
	}

	// 断开可重连：下一次 Publish 自动建链并送达。
	if err := c.Publish(ctx, mqtt.Message{Topic: "uic/site/d/p", Payload: []byte("2")}); err != nil {
		t.Fatalf("publish after disconnect: %v", err)
	}
	msgs := b.WaitMessages(t, 2)
	if string(msgs[len(msgs)-1].Payload) != "2" {
		t.Fatalf("second publish payload = %q, want 2", msgs[len(msgs)-1].Payload)
	}
	if wills := b.Wills(); len(wills) != 0 {
		t.Fatalf("reconnect must not trigger the will, got %d", len(wills))
	}
}
