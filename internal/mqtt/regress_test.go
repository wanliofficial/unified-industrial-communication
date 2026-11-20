package mqtt_test

import (
	"context"
	"errors"
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
