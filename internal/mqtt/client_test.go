package mqtt_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"uic/internal/mqtt"
	"uic/internal/mqtt/mqtttest"
)

// 客户端测试统一走真实 TCP 与最小 Broker：MQTT 的问题大多出在帧同步、
// 半关闭、部分写这类只有真 socket 才暴露的地方。

func newClient(b *mqtttest.Broker, mod func(*mqtt.Options)) *mqtt.Client {
	opt := mqtt.Options{
		Addr:           b.Addr(),
		ClientID:       "uic-test",
		KeepAlive:      30 * time.Second,
		ConnectTimeout: 2 * time.Second,
		AckTimeout:     2 * time.Second,
	}
	if mod != nil {
		mod(&opt)
	}
	return mqtt.New(opt)
}

func TestClientConnectAndPublishQoS0(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, func(o *mqtt.Options) { o.KeepAlive = 30 * time.Second })
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Publish(ctx, mqtt.Message{Topic: "uic/site/d/p/value", Payload: []byte("42")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msgs := b.WaitMessages(t, 1)
	if msgs[0].Topic != "uic/site/d/p/value" || string(msgs[0].Payload) != "42" {
		t.Fatalf("message = %+v", msgs[0])
	}
	if msgs[0].QoS != 0 {
		t.Fatalf("qos = %d", msgs[0].QoS)
	}
}

// QoS1 必须等到 PUBACK 才算成功：否则 Outbox 会推进位点，数据永久丢失。
func TestClientPublishQoS1WaitsForPuback(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Publish(ctx, mqtt.Message{Topic: "t", Payload: []byte("x"), QoS: 1, Retain: true}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msgs := b.WaitMessages(t, 1)
	if msgs[0].QoS != 1 || !msgs[0].Retain {
		t.Fatalf("message = %+v", msgs[0])
	}
	st := c.Stats()
	if st.Sent != 1 || st.Acked != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestClientPublishQoS1TimesOutWithoutPuback(t *testing.T) {
	b := mqtttest.New(t, func(b *mqtttest.Broker) { b.AckPublish = false })
	c := newClient(b, func(o *mqtt.Options) { o.AckTimeout = 200 * time.Millisecond })
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Publish(ctx, mqtt.Message{Topic: "t", Payload: []byte("x"), QoS: 1})
	if err == nil {
		t.Fatal("missing PUBACK must be an error")
	}
	if !strings.Contains(err.Error(), "PUBACK") {
		t.Fatalf("err = %v", err)
	}
	if acked := c.Stats().Acked; acked != 0 {
		t.Fatalf("acked = %d, must stay 0", acked)
	}
}

func TestClientRejectsQoS2(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, nil)
	defer c.Close()
	err := c.Publish(context.Background(), mqtt.Message{Topic: "t", QoS: 2})
	if err == nil {
		t.Fatal("qos2 must be rejected rather than silently downgraded")
	}
}

func TestClientConnackRefusal(t *testing.T) {
	cases := []struct {
		code byte
		want string
	}{
		{mqtt.ConnRefusedBadAuth, "bad username"},
		{mqtt.ConnRefusedNotAuth, "not authorized"},
		{mqtt.ConnRefusedIdentifier, "client id"},
		{mqtt.ConnRefusedUnavailable, "unavailable"},
		{mqtt.ConnRefusedProtocol, "protocol"},
	}
	for _, tc := range cases {
		code := tc.code
		b := mqtttest.New(t, func(b *mqtttest.Broker) { b.ConnackCode = code })
		c := newClient(b, nil)
		err := c.Connect(context.Background())
		if err == nil {
			t.Fatalf("code %d: must fail", code)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("code %d: err = %v, want mention of %q", code, err, tc.want)
		}
		_ = c.Close()
	}
}

func TestClientConnectDialFailure(t *testing.T) {
	c := mqtt.New(mqtt.Options{Addr: "127.0.0.1:1", ClientID: "x",
		ConnectTimeout: 300 * time.Millisecond})
	if err := c.Connect(context.Background()); err == nil {
		t.Fatal("dial to closed port must fail")
	}
}

func TestClientReconnectsAfterDrop(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Publish(ctx, mqtt.Message{Topic: "t", Payload: []byte("1"), QoS: 1}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	b.DropAll()
	deadline := time.Now().Add(3 * time.Second)
	for c.Stats().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Stats().Connected {
		t.Fatal("client did not notice dropped connection")
	}

	if err := c.Publish(ctx, mqtt.Message{Topic: "t", Payload: []byte("2"), QoS: 1}); err != nil {
		t.Fatalf("publish after reconnect: %v", err)
	}
	if n := b.Connects(); n < 2 {
		t.Fatalf("connects = %d, want >= 2", n)
	}
}

// 连接在等 PUBACK 期间断开：必须立刻失败，不能等到 AckTimeout 才失败，
// 更不能被误判为成功。
func TestClientFailsFastWhenConnectionDropsMidPublish(t *testing.T) {
	b := mqtttest.New(t, func(b *mqtttest.Broker) { b.AckPublish = false })
	c := newClient(b, func(o *mqtt.Options) { o.AckTimeout = 10 * time.Second })
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Publish(ctx, mqtt.Message{Topic: "t", Payload: []byte("x"), QoS: 1})
	}()
	b.WaitMessages(t, 1)

	start := time.Now()
	b.DropAll()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("publish must fail when connection drops")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("failed after %v, should be immediate (AckTimeout=10s)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish hung after connection loss")
	}
}

func TestClientKeepaliveSendsPing(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, func(o *mqtt.Options) { o.KeepAlive = 2 * time.Second })
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// keepalive/2 = 1s 一次 ping。
	deadline := time.Now().Add(3 * time.Second)
	for b.Pings() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if b.Pings() == 0 {
		t.Fatal("no PINGREQ sent: keepalive broken")
	}
	if !c.Stats().Connected {
		t.Fatal("connection dropped despite keepalive")
	}
}

// 遗嘱必须出现在 CONNECT 里，否则 Broker 无法在掉线时代发 $death。
func TestClientWillIsSentInConnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := mqtt.New(mqtt.Options{
		Addr: b.Addr(), ClientID: "gw",
		WillTopic: "uic/site/$death", WillPayload: []byte(`{"online":false}`),
		WillQoS: 1, WillRetain: true, ConnectTimeout: 2 * time.Second,
	})
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if b.Connects() != 1 {
		t.Fatalf("connects = %d", b.Connects())
	}
}

func TestClientCloseSendsDisconnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 正常关闭必须发 DISCONNECT，否则 Broker 会触发遗嘱消息，
	// 消费方会看到一次假的「网关掉线」。
	deadline := time.Now().Add(2 * time.Second)
	for b.Disconnects() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Disconnects() == 0 {
		t.Fatal("DISCONNECT not received")
	}
}

func TestClientCloseIsIdempotent(t *testing.T) {
	b := mqtttest.New(t, nil)
	c := newClient(b, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := c.Publish(context.Background(), mqtt.Message{Topic: "t"}); err == nil {
		t.Fatal("publish after close must fail")
	}
}

func TestClientPublishAfterBrokerDies(t *testing.T) {
	b := mqtttest.New(t, func(b *mqtttest.Broker) { b.DropOnConnect = true })
	c := newClient(b, func(o *mqtt.Options) { o.ConnectTimeout = 500 * time.Millisecond })
	defer c.Close()
	if err := c.Publish(context.Background(), mqtt.Message{Topic: "t", Payload: []byte("x")}); err == nil {
		t.Fatal("publish against dead broker must fail")
	}
}

var _ = errors.Is
