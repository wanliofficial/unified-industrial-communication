// Package mqtttest 提供一个最小 MQTT 3.1.1 Broker，仅供测试使用。
//
// 为什么独立成包而不是留在 _test.go 里：北向 MQTT 的验证点有两层——协议
// 客户端本身（internal/mqtt）和 runtime 的 sink 装配（internal/runtime）。
// 两层都需要一个能说 MQTT 的对端。复制一份假 Broker 到两个测试包里，
// 两边就会慢慢长成不同的行为，最终出现「客户端测试通过但集成测试失败」
// 这种最难查的分歧。
package mqtttest

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"uic/internal/mqtt"
)

// Message 是 Broker 收到的一条消息。
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// Broker 是一个可编排的最小 Broker。
type Broker struct {
	ln net.Listener

	// ConnackCode 控制 CONNACK 返回码（默认接受）。必须在构造时设定。
	ConnackCode byte
	// DropOnConnect 为 true 时接受连接后立刻关闭，模拟 Broker 崩溃。
	DropOnConnect bool
	// AckPublish 为 false 时不回 PUBACK，用于测超时与断线路径。
	AckPublish bool

	mu       sync.Mutex
	connects int
	msgs     []Message
	conns    []net.Conn
	pings    int
	closes   int
	wg       sync.WaitGroup
}

// New 启动一个监听随机端口的 Broker。测试结束时自动关闭。
func New(t *testing.T, setup func(*Broker)) *Broker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mqtttest: listen: %v", err)
	}
	b := &Broker{ln: ln, AckPublish: true}
	if setup != nil {
		setup(b)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		b.mu.Lock()
		for _, c := range b.conns {
			_ = c.Close()
		}
		b.mu.Unlock()
		b.wg.Wait()
	})
	b.wg.Add(1)
	go b.acceptLoop()
	return b
}

// Addr 返回 host:port。
func (b *Broker) Addr() string { return b.ln.Addr().String() }

// URL 返回 tcp://host:port。
func (b *Broker) URL() string { return "tcp://" + b.Addr() }

func (b *Broker) acceptLoop() {
	defer b.wg.Done()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.conns = append(b.conns, c)
		b.mu.Unlock()
		b.wg.Add(1)
		go b.serve(c)
	}
}

func (b *Broker) serve(c net.Conn) {
	defer b.wg.Done()
	defer c.Close()

	if b.DropOnConnect {
		return
	}

	pkt, err := mqtt.ReadPacket(c, 1<<20)
	if err != nil || pkt.Type != mqtt.TypeConnect {
		return
	}
	b.mu.Lock()
	b.connects++
	b.mu.Unlock()

	if _, err := c.Write([]byte{mqtt.TypeConnack << 4, 2, 0, b.ConnackCode}); err != nil {
		return
	}
	if b.ConnackCode != mqtt.ConnAccepted {
		return
	}

	for {
		pkt, err := mqtt.ReadPacket(c, 1<<20)
		if err != nil {
			return
		}
		switch pkt.Type {
		case mqtt.TypePublish:
			msg, pid, perr := parsePublish(pkt)
			if perr != nil {
				return
			}
			b.mu.Lock()
			b.msgs = append(b.msgs, msg)
			ack := b.AckPublish
			b.mu.Unlock()
			if msg.QoS > 0 && ack {
				if _, err := c.Write(mqtt.PubackPacket(pid)); err != nil {
					return
				}
			}
		case mqtt.TypePingreq:
			b.mu.Lock()
			b.pings++
			b.mu.Unlock()
			if _, err := c.Write([]byte{mqtt.TypePingresp << 4, 0}); err != nil {
				return
			}
		case mqtt.TypeDisconnect:
			b.mu.Lock()
			b.closes++
			b.mu.Unlock()
			return
		}
	}
}

func parsePublish(pkt mqtt.Packet) (Message, uint16, error) {
	body := pkt.Body
	if len(body) < 2 {
		return Message{}, 0, errors.New("mqtttest: short publish")
	}
	tl := int(binary.BigEndian.Uint16(body[:2]))
	if len(body) < 2+tl {
		return Message{}, 0, errors.New("mqtttest: short topic")
	}
	m := Message{
		Topic:  string(body[2 : 2+tl]),
		QoS:    (pkt.Flags >> 1) & 0x03,
		Retain: pkt.Flags&0x01 != 0,
	}
	rest := body[2+tl:]
	var pid uint16
	if m.QoS > 0 {
		if len(rest) < 2 {
			return Message{}, 0, errors.New("mqtttest: short packet id")
		}
		pid = binary.BigEndian.Uint16(rest[:2])
		rest = rest[2:]
	}
	m.Payload = append([]byte(nil), rest...)
	return m, pid, nil
}

// SetAckPublish 在运行中切换「是否回 PUBACK」。
//
// 必须走方法而不是直接改字段：serve 协程在另一个 goroutine 里读这个字段，
// 直接赋值在 -race 下会被判为数据竞争。
func (b *Broker) SetAckPublish(v bool) {
	b.mu.Lock()
	b.AckPublish = v
	b.mu.Unlock()
}

// Messages 返回已收到的全部消息（副本）。
func (b *Broker) Messages() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.msgs...)
}

// Connects 返回累计 CONNECT 次数（重连会累加）。
func (b *Broker) Connects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

// Pings 返回累计 PINGREQ 次数。
func (b *Broker) Pings() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pings
}

// Disconnects 返回累计正常 DISCONNECT 次数。
func (b *Broker) Disconnects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes
}

// DropAll 断开所有当前连接，模拟网络闪断。
func (b *Broker) DropAll() {
	b.mu.Lock()
	conns := b.conns
	b.conns = nil
	b.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
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
	t.Fatalf("mqtttest: waiter: got %d messages, want %d", len(b.Messages()), n)
	return nil
}
