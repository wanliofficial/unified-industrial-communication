package mqtt

import (
	"context"
	"testing"
	"time"

	mqttc "uic/internal/mqtt"
)

// 这个 Broker 不是测试夹具，它是 `uic sim -protocol mqtt` 的运行时实现，
// 所以要有自己的测试。用例尽量走真客户端（internal/mqtt），而不是手搓
// 报文：Broker 的价值就在于「与真实客户端配合」，手搓报文测的是编解码。

func dialClient(t *testing.T, b *Broker, opt mqttc.Options) *mqttc.Client {
	t.Helper()
	opt.Addr = b.Addr()
	if opt.ClientID == "" {
		opt.ClientID = "t-" + t.Name()
	}
	if opt.AckTimeout <= 0 {
		opt.AckTimeout = 2 * time.Second
	}
	c := mqttc.New(opt)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestBrokerDeliversToSubscriber(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	got := make(chan mqttc.Publish, 4)
	dialClient(t, b, mqttc.Options{
		Subscriptions: []mqttc.TopicFilter{{Filter: "plant/+/temp", QoS: 1}},
		OnMessage:     func(p mqttc.Publish) { got <- p },
	})

	// 订阅是同步完成的（客户端在 Connect 里等 SUBACK），所以这里可以直接推。
	if n := b.Publish("plant/line1/temp", []byte("21"), 1, false); n != 1 {
		t.Fatalf("delivered to %d subscribers, want 1", n)
	}
	select {
	case p := <-got:
		if p.Topic != "plant/line1/temp" || string(p.Payload) != "21" {
			t.Fatalf("p = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("message not delivered")
	}

	// 不匹配的主题不能投递。
	if n := b.Publish("plant/line1/pressure", []byte("1"), 1, false); n != 0 {
		t.Fatalf("delivered to %d subscribers for non-matching topic", n)
	}
}

func TestBrokerRecordsClientPublishes(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	c := dialClient(t, b, mqttc.Options{})
	if err := c.Publish(context.Background(), mqttc.Message{
		Topic: "plant/setpoint", Payload: []byte("42.5"), QoS: 1,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	msgs := b.Messages()
	if len(msgs) != 1 || msgs[0].Topic != "plant/setpoint" || string(msgs[0].Payload) != "42.5" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if last, ok := b.LastMessage("plant/setpoint"); !ok || string(last.Payload) != "42.5" {
		t.Fatalf("last = %+v ok=%v", last, ok)
	}
	if _, ok := b.LastMessage("nope"); ok {
		t.Fatal("unknown topic must not have a last message")
	}
}

func TestBrokerQoS2IsDowngraded(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	// 授予 QoS2 会让客户端等 PUBREC；本 Broker 不做 QoS2，必须降级授予。
	got := make(chan mqttc.Publish, 1)
	dialClient(t, b, mqttc.Options{
		Subscriptions: []mqttc.TopicFilter{{Filter: "a", QoS: 1}},
		OnMessage:     func(p mqttc.Publish) { got <- p },
	})
	if n := b.Publish("a", []byte("x"), 1, false); n != 1 {
		t.Fatalf("delivered = %d", n)
	}
	select {
	case p := <-got:
		if p.QoS != 1 {
			t.Fatalf("qos = %d, want 1", p.QoS)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not delivered")
	}
}

func TestBrokerRejectSubscribe(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{RejectSubscribe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	c := mqttc.New(mqttc.Options{
		Addr: b.Addr(), ClientID: "rej", AckTimeout: 2 * time.Second,
		Subscriptions: []mqttc.TopicFilter{{Filter: "a", QoS: 1}},
	})
	if err := c.Connect(context.Background()); err == nil {
		_ = c.Close()
		t.Fatal("rejected subscription must fail connect")
	}
}

func TestBrokerConnackRefusal(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{ConnackCode: mqttc.ConnRefusedNotAuth})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	c := mqttc.New(mqttc.Options{Addr: b.Addr(), ClientID: "x", ConnectTimeout: time.Second})
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err == nil {
		t.Fatal("refused CONNACK must fail connect")
	}
	if b.Connects() != 1 {
		t.Fatalf("connects = %d", b.Connects())
	}
}

func TestBrokerPing(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	c := dialClient(t, b, mqttc.Options{})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if b.Pings() < 1 {
		t.Fatalf("pings = %d", b.Pings())
	}

	// 关掉 PINGRESP：Ping 必须失败，否则「半死链路」永远发现不了。
	b.SetIgnorePing(true)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("ping must fail when broker stays silent")
	}
}

func TestBrokerDropAllAndReconnect(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	got := make(chan mqttc.Publish, 4)
	c := dialClient(t, b, mqttc.Options{
		Subscriptions: []mqttc.TopicFilter{{Filter: "a", QoS: 1}},
		OnMessage:     func(p mqttc.Publish) { got <- p },
	})
	b.Publish("a", []byte("1"), 1, false)
	<-got

	b.DropAll()
	// 等客户端自己发现断链（读循环收到 EOF），否则 Connect 会因为「状态还是
	// 已连接」直接返回，测试就和读循环赛跑了。
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Connected && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// 客户端按需重连：下一次 Connect/Publish 才会重建，并重新订阅。
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if b.Connects() < 2 {
		t.Fatalf("connects = %d, want >= 2", b.Connects())
	}
	b.Publish("a", []byte("2"), 1, false)
	select {
	case p := <-got:
		if string(p.Payload) != "2" {
			t.Fatalf("payload = %q", p.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not delivered after reconnect")
	}
}

func TestBrokerCloseIsIdempotent(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := dialClient(t, b, mqttc.Options{})
	_ = c
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestTopicMatches(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"a/+", "a/b", true},
		{"a/+", "a/b/c", false},
		{"a/#", "a/b/c", true},
		{"a/#", "a", true},
		{"#", "anything/at/all", true},
		{"+/b", "a/b", true},
		{"a/b", "a", false},
		// 空层级是合法主题：a//b 有三个层级。
		{"a/+/b", "a//b", true},
		{"a/#/b", "a/x/b", false}, // # 只能出现在最后
	}
	for _, c := range cases {
		if got := TopicMatches(c.filter, c.topic); got != c.want {
			t.Errorf("TopicMatches(%q, %q) = %v, want %v", c.filter, c.topic, got, c.want)
		}
	}
}

func TestEncodeRemainingLen(t *testing.T) {
	cases := map[int][]byte{
		0:       {0x00},
		127:     {0x7f},
		128:     {0x80, 0x01},
		16383:   {0xff, 0x7f},
		16384:   {0x80, 0x80, 0x01},
		2097151: {0xff, 0xff, 0x7f},
	}
	for n, want := range cases {
		got := encodeRemainingLen(n)
		if len(got) != len(want) {
			t.Fatalf("%d: % x, want % x", n, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%d: % x, want % x", n, got, want)
			}
		}
	}
}

func TestBrokerURL(t *testing.T) {
	b, err := New("127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if b.URL() != "tcp://"+b.Addr() {
		t.Fatalf("url = %q addr = %q", b.URL(), b.Addr())
	}
}
