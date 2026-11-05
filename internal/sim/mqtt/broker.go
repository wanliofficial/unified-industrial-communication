// Package mqtt 提供一个最小的 MQTT 3.1.1 Broker，用于演示与集成测试。
//
// 为什么自己写一个 Broker：本项目的演示链路要求「零外部依赖」——Modbus、S7、
// OPC UA 都有内置从站，MQTT 不应例外：跑 examples/mqtt-southbound.json 不该
// 先让人去装一个 mosquitto。
//
// 边界（明确不做，而不是做不到）：
//   - 不支持 QoS2（PUBREC/PUBREL/PUBCOMP），收到 QoS2 订阅申请按 QoS1 授予；
//   - 不做持久会话：CleanSession 一律当 true，断线即丢订阅；
//   - 不做 TLS / WebSocket / 认证 / ACL；
//   - retained 消息只存在内存里，进程退出即丢。
//
// 这些取舍与客户端侧一致：两侧都只实现同一个规范子集，才不会出现「客户端测
// 通过、换真 Broker 失败」的分歧。
package mqtt

import (
	"encoding/binary"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"

	// 客户端与 Broker 同名包，这里用别名区分：这个包是服务端，
	// mqttc 是报文编解码（两侧共用同一份，避免「编码错被解码错抵消」）。
	mqttc "uic/internal/mqtt"
)

// Message 是一条经过 Broker 的消息。
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// Broker 是一个最小 MQTT 3.1.1 Broker。
//
// 并发模型：一条连接一个读协程；写由每个连接的 writeMu 串行化。Broker 自身
// 的 mu 只保护「连接列表 + 订阅表 + 统计」这些簿记，不保护任何 I/O——把
// socket 写操作放进全局锁里，一个慢客户端就能拖死所有客户端。
type Broker struct {
	ln net.Listener

	// 以下开关必须在 Start 之前设置（Start 之后并发写会有数据竞争）。
	// 运行中要改请用对应的 setter，它们会加锁。
	connackCode     byte
	dropOnConnect   bool
	ackPublish      bool
	rejectSubscribe bool
	ignorePing      bool
	onMessage       func(Message)

	mu       sync.Mutex
	clients  []*conn
	connects int
	pings    int
	closes   int
	msgs     []Message
	last     map[string]Message
	subs     []string
	closed   bool
	wg       sync.WaitGroup
}

// conn 是一条已连接的客户端。
type conn struct {
	net.Conn
	broker *Broker

	writeMu sync.Mutex
	mu      sync.Mutex
	filters []mqttc.TopicFilter
}

func (c *conn) write(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.Write(b)
	return err
}

// grantedQoS 返回该连接对某主题授予的 QoS；255 表示没有匹配的订阅。
func (c *conn) grantedQoS(topic string) byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	best := byte(255)
	for _, f := range c.filters {
		if TopicMatches(f.Filter, topic) && f.QoS < best {
			best = f.QoS
		}
	}
	return best
}

// Options 是 Broker 的可配项。零值即「正常工作的 Broker」。
type Options struct {
	// ConnackCode 让 CONNECT 直接返回拒绝码，用来测客户端对拒绝的处理。
	ConnackCode byte
	// DropOnConnect 接受连接后立刻关闭，模拟 Broker 崩溃。
	DropOnConnect bool
	// AckPublish 默认应为 true；置 false 可测客户端的 PUBACK 超时路径。
	// 注意零值是 false，所以 New 会把它当成「要回 ACK」——见 New 的注释。
	NoAckPublish bool
	// RejectSubscribe 让 SUBACK 全回 0x80，模拟 ACL 拒绝。
	RejectSubscribe bool
	// IgnorePing 让 Broker 不回 PINGRESP，模拟「半死链路」。
	IgnorePing bool
	// OnMessage 在收到客户端 PUBLISH 并记录之后调用。
	//
	// 用途：演示 Broker 用它模拟「设备响应」——把收到设定值回显到同一主题，
	// 让网关的回读确认能走通。它运行在连接的读协程里，必须快速返回；要发布
	// 消息请用 Broker.Publish，不要在里面写大块逻辑。
	OnMessage func(Message)
}

// New 创建 Broker 并开始监听。调用方负责 Close。
//
// 先 Listen 再返回：调用方需要在启动客户端之前拿到真实端口（:0 的场景），
// 因此监听失败必须在构造期就暴露，而不是在一个后台协程里静默退出。
func New(addr string, opt Options) (*Broker, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	b := &Broker{
		ln:              ln,
		connackCode:     opt.ConnackCode,
		dropOnConnect:   opt.DropOnConnect,
		ackPublish:      !opt.NoAckPublish,
		rejectSubscribe: opt.RejectSubscribe,
		ignorePing:      opt.IgnorePing,
		onMessage:       opt.OnMessage,
		last:            map[string]Message{},
	}
	b.wg.Add(1)
	go b.acceptLoop()
	return b, nil
}

// Addr 返回实际监听地址（:0 时是真实端口）。
func (b *Broker) Addr() string { return b.ln.Addr().String() }

// URL 返回 tcp://host:port，可直接喂给客户端配置。
func (b *Broker) URL() string { return "tcp://" + b.Addr() }

// Close 关闭监听并断开所有客户端连接。幂等。
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	clients := b.clients
	b.clients = nil
	b.mu.Unlock()

	err := b.ln.Close()
	for _, c := range clients {
		_ = c.Close()
	}
	b.wg.Wait()
	return err
}

func (b *Broker) acceptLoop() {
	defer b.wg.Done()
	for {
		raw, err := b.ln.Accept()
		if err != nil {
			return
		}
		c := &conn{Conn: raw, broker: b}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = raw.Close()
			return
		}
		b.clients = append(b.clients, c)
		b.mu.Unlock()
		b.wg.Add(1)
		go b.serve(c)
	}
}

func (b *Broker) serve(c *conn) {
	defer b.wg.Done()
	defer func() {
		b.mu.Lock()
		for i, x := range b.clients {
			if x == c {
				b.clients = append(b.clients[:i], b.clients[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		_ = c.Close()
	}()

	pkt, err := mqttc.ReadPacket(c, 1<<20)
	if err != nil || pkt.Type != mqttc.TypeConnect {
		return
	}
	b.mu.Lock()
	b.connects++
	drop := b.dropOnConnect
	code := b.connackCode
	b.mu.Unlock()
	if drop {
		return
	}
	if err := c.write([]byte{mqttc.TypeConnack << 4, 2, 0, code}); err != nil {
		return
	}
	if code != mqttc.ConnAccepted {
		return
	}

	for {
		pkt, err := mqttc.ReadPacket(c, 1<<20)
		if err != nil {
			return
		}
		switch pkt.Type {
		case mqttc.TypeSubscribe:
			if !b.handleSubscribe(c, pkt) {
				return
			}
		case mqttc.TypePublish:
			msg, pid, perr := parseIncoming(pkt)
			if perr != nil {
				return
			}
			b.mu.Lock()
			b.msgs = append(b.msgs, msg)
			b.last[msg.Topic] = msg
			ack := b.ackPublish
			onMsg := b.onMessage
			b.mu.Unlock()
			if msg.QoS > 0 && ack {
				if err := c.write(mqttc.PubackPacket(pid)); err != nil {
					return
				}
			}
			// 回调放在 PUBACK 之后：先让发布方收到确认，再做后续动作，
			// 否则一个慢回调会把 QoS1 的往返时间拖长。
			if onMsg != nil {
				onMsg(msg)
			}
		case mqttc.TypePingreq:
			b.mu.Lock()
			b.pings++
			ignore := b.ignorePing
			b.mu.Unlock()
			if ignore {
				continue
			}
			if err := c.write([]byte{mqttc.TypePingresp << 4, 0}); err != nil {
				return
			}
		case mqttc.TypeDisconnect:
			b.mu.Lock()
			b.closes++
			b.mu.Unlock()
			return
		}
	}
}

// parseIncoming 解析客户端发来的 PUBLISH。
func parseIncoming(pkt mqttc.Packet) (Message, uint16, error) {
	p, err := mqttc.ParsePublish(pkt)
	if err != nil {
		return Message{}, 0, err
	}
	return Message{Topic: p.Topic, Payload: p.Payload, QoS: p.QoS, Retain: p.Retain}, p.PID, nil
}

// handleSubscribe 记录过滤器并回 SUBACK。
func (b *Broker) handleSubscribe(c *conn, pkt mqttc.Packet) bool {
	pid, filters, err := parseSubscribe(pkt)
	if err != nil {
		return false
	}
	b.mu.Lock()
	reject := b.rejectSubscribe
	b.mu.Unlock()

	granted := make([]byte, len(filters))
	c.mu.Lock()
	for i, f := range filters {
		if reject {
			granted[i] = mqttc.SubackFailure
			continue
		}
		// QoS2 在这里降级授予 1：本 Broker 不实现 QoS2 的两段确认，
		// 授予 2 会让客户端等一个永远不来的 PUBREC。
		if f.QoS > 1 {
			f.QoS = 1
		}
		granted[i] = f.QoS
		c.filters = append(c.filters, f)
	}
	c.mu.Unlock()

	b.mu.Lock()
	for _, f := range filters {
		b.subs = append(b.subs, f.Filter)
	}
	b.mu.Unlock()

	body := []byte{byte(pid >> 8), byte(pid)}
	body = append(body, granted...)
	out := []byte{mqttc.TypeSuback << 4}
	out = append(out, encodeRemainingLen(len(body))...)
	out = append(out, body...)
	return c.write(out) == nil
}

// encodeRemainingLen 编码 MQTT 变长剩余长度。
func encodeRemainingLen(n int) []byte {
	var out []byte
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			return out
		}
	}
}

// parseSubscribe 拆出 packet id 与过滤器列表。
func parseSubscribe(pkt mqttc.Packet) (uint16, []mqttc.TopicFilter, error) {
	body := pkt.Body
	if len(body) < 2 {
		return 0, nil, errors.New("mqtt broker: short subscribe")
	}
	pid := binary.BigEndian.Uint16(body[:2])
	rest := body[2:]
	var out []mqttc.TopicFilter
	for len(rest) > 0 {
		if len(rest) < 2 {
			return 0, nil, errors.New("mqtt broker: short filter length")
		}
		n := int(binary.BigEndian.Uint16(rest[:2]))
		if len(rest) < 2+n+1 {
			return 0, nil, errors.New("mqtt broker: short filter")
		}
		out = append(out, mqttc.TopicFilter{Filter: string(rest[2 : 2+n]), QoS: rest[2+n]})
		rest = rest[2+n+1:]
	}
	if len(out) == 0 {
		return 0, nil, errors.New("mqtt broker: empty subscribe")
	}
	return pid, out, nil
}

// TopicMatches 判断主题是否匹配过滤器（支持 + 与 #）。
//
// 单独导出是因为客户端侧也要用同一条规则：两侧规则不一致会让「明明发了
// 消息但点位没值」成为无解问题。
func TopicMatches(filter, topic string) bool {
	if filter == topic {
		return true
	}
	fp := strings.Split(filter, "/")
	tp := strings.Split(topic, "/")
	for i, f := range fp {
		if f == "#" {
			// # 必须在最后一段；多级通配匹配剩余部分（含空）。
			return i == len(fp)-1
		}
		if i >= len(tp) {
			return false
		}
		if f == "+" {
			continue
		}
		if f != tp[i] {
			return false
		}
	}
	return len(fp) == len(tp)
}

// Publish 向所有匹配的订阅者推送一条消息，返回投递的连接数。
func (b *Broker) Publish(topic string, payload []byte, qos byte, retain bool) int {
	b.mu.Lock()
	clients := append([]*conn(nil), b.clients...)
	b.mu.Unlock()

	sent := 0
	for _, c := range clients {
		granted := c.grantedQoS(topic)
		if granted == 255 {
			continue
		}
		q := qos
		if granted < q {
			q = granted
		}
		var pid uint16
		if q > 0 {
			pid = 1
		}
		pkt, err := mqttc.PublishPacket(mqttc.Message{
			Topic: topic, Payload: payload, QoS: q, Retain: retain,
		}, pid)
		if err != nil {
			continue
		}
		if c.write(pkt) == nil {
			sent++
		}
	}
	return sent
}

// Messages 返回收到的全部消息（副本）。
func (b *Broker) Messages() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.msgs...)
}

// LastMessage 返回某个主题最后收到的消息。
func (b *Broker) LastMessage(topic string) (Message, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.last[topic]
	return m, ok
}

// Subscriptions 返回累计收到的过滤器（去重后按字典序）。
func (b *Broker) Subscriptions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, s := range b.subs {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
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
	clients := b.clients
	b.clients = nil
	b.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}

// SetAckPublish 运行中切换「是否回 PUBACK」。
//
// 必须走方法而不是直接改字段：serve 协程在另一个 goroutine 里读这个字段。
func (b *Broker) SetAckPublish(v bool) {
	b.mu.Lock()
	b.ackPublish = v
	b.mu.Unlock()
}

// SetIgnorePing 运行中切换「是否回 PINGRESP」。
func (b *Broker) SetIgnorePing(v bool) {
	b.mu.Lock()
	b.ignorePing = v
	b.mu.Unlock()
}

// SetRejectSubscribe 运行中切换「是否拒绝订阅」。
func (b *Broker) SetRejectSubscribe(v bool) {
	b.mu.Lock()
	b.rejectSubscribe = v
	b.mu.Unlock()
}

// SetIgnorePing 已在上面定义；这里补齐其余运行期开关。

// SetDropOnConnect 运行中切换「接受连接后立刻关闭」。
func (b *Broker) SetDropOnConnect(v bool) {
	b.mu.Lock()
	b.dropOnConnect = v
	b.mu.Unlock()
}

// SetConnackCode 运行中切换 CONNECT 返回码。
func (b *Broker) SetConnackCode(v byte) {
	b.mu.Lock()
	b.connackCode = v
	b.mu.Unlock()
}
