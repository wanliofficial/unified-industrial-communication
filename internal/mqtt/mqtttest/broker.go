// Package mqtttest 是 internal/sim/mqtt 的测试封装。
//
// 为什么不再自己实现一遍 Broker：北向 MQTT 的验证点有两层——协议客户端本身
// （internal/mqtt）和 runtime 的 sink / 南向驱动装配。两层都需要一个能说
// MQTT 的对端。两份 Broker 会慢慢长成不同的行为，最终出现「客户端测试通过
// 但集成测试失败」这种最难查的分歧。所以真实实现只有一份（sim/mqtt），
// 这里只负责「绑定 *testing.T 的生命周期」与等待类断言。
package mqtttest

import (
	"testing"
	"time"

	broker "uic/internal/sim/mqtt"
)

// Message 是 Broker 收到（或推送）的一条消息。
type Message = broker.Message

// TopicMatches 判断主题是否匹配过滤器（支持 + 与 #）。
var TopicMatches = broker.TopicMatches

// Broker 是一个可编排的最小 Broker。
type Broker struct {
	*broker.Broker
	t *testing.T
}

// New 启动一个监听随机端口的 Broker。测试结束时自动关闭。
//
// setup 在监听之后、接收连接之前执行：开关（拒绝订阅、不回 PINGRESP 等）
// 必须在客户端连接前设定，否则测的是一个「半途改变行为」的场景——那种
// 场景另有 setter 覆盖。
func New(t *testing.T, setup func(*Broker)) *Broker {
	t.Helper()
	b, err := broker.New("127.0.0.1:0", broker.Options{})
	if err != nil {
		t.Fatalf("mqtttest: %v", err)
	}
	w := &Broker{Broker: b, t: t}
	if setup != nil {
		setup(w)
	}
	t.Cleanup(func() { _ = b.Close() })
	return w
}

// Publish 由 Broker 主动向订阅者推送一条消息，返回投递的连接数。
func (b *Broker) Publish(topic string, payload []byte, qos byte, retain bool) int {
	b.t.Helper()
	return b.Broker.Publish(topic, payload, qos, retain)
}

// PublishString 是 Publish 的字符串便捷形式。
func (b *Broker) PublishString(topic, payload string) {
	b.t.Helper()
	b.Broker.Publish(topic, []byte(payload), 1, false)
}

// WaitMessages 等到至少收到 n 条消息，超时则测试失败。
func (b *Broker) WaitMessages(t *testing.T, n int) []Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := b.Messages(); len(msgs) >= n {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("mqtttest: got %d messages, want %d", len(b.Messages()), n)
	return nil
}

// WaitConnects 等到累计 CONNECT 次数达到 n。
func (b *Broker) WaitConnects(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b.Connects() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("mqtttest: connects = %d, want >= %d", b.Connects(), n)
}

// WaitSubscriptions 等到收到至少 n 个不同的过滤器。
func (b *Broker) WaitSubscriptions(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if subs := b.Subscriptions(); len(subs) >= n {
			return subs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("mqtttest: subscriptions = %v, want >= %d", b.Subscriptions(), n)
	return nil
}

// 以下 setup 函数用于在客户端连接之前设定 Broker 行为。
//
// 之所以做成函数而不是直接赋字段：真实实现（sim/mqtt）里的开关都由互斥锁
// 保护，直接暴露字段会诱使调用方在运行中赋值，从而引入数据竞争。

// NoAck 让 Broker 不回 PUBACK（测客户端的 QoS1 超时路径）。
func NoAck(b *Broker) { b.SetAckPublish(false) }

// Rejecting 让 SUBACK 全部返回 0x80（模拟 ACL 拒绝）。
func Rejecting(b *Broker) { b.SetRejectSubscribe(true) }

// IgnoringPing 让 Broker 不回 PINGRESP（模拟半死链路）。
func IgnoringPing(b *Broker) { b.SetIgnorePing(true) }

// DropOnConnect 接受连接后立刻关闭（模拟 Broker 崩溃）。
func DropOnConnect(b *Broker) { b.SetDropOnConnect(true) }

// ConnackCode 指定 CONNECT 的返回码。
func ConnackCode(code byte) func(*Broker) {
	return func(b *Broker) { b.SetConnackCode(code) }
}
