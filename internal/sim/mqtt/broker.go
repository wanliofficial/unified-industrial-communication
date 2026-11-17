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
	// infos 记录每条 CONNECT 的解析结果，wills 记录 Broker 代发的遗嘱。
	//
	// 两者都只服务于测试断言：客户端「本地看起来正确」不代表线上字节对，
	// 比如 CleanSession、遗嘱 QoS/retain、bdSeq 这些契约只有从对端看才
	// 能确认。badConnects 记解析失败的次数——一个坏 CONNECT 被当成「没
	// 遗嘱」静默接受，会让「遗嘱没送达」看起来像 Broker 的问题。
	infos       []mqttc.ConnectInfo
	wills       []Message
	badConnects int
	// retained 保存 retain=true 的消息，供新订阅方在 SUBSCRIBE 时补发。
	retained []Message
	wg       sync.WaitGroup
}

// conn 是一条已连接的客户端。
type conn struct {
	net.Conn
	broker *Broker

	writeMu sync.Mutex
	mu      sync.Mutex
	filters []mqttc.TopicFilter

	// will 是这条连接在 CONNECT 里登记的遗嘱；graceful 表示收到了
	// DISCONNECT（正常结束）——此时按 MQTT 规范**不得**代发遗嘱。
	will     *mqttc.Will
	graceful bool
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
		// 遗嘱只在「非正常断开」时代发：收到 DISCONNECT 才算正常结束。
		// 漏掉 graceful 判定，正常关机会被消费方看成一次掉线——本地测试
		// 全绿，现场却每次重启都报一次假告警。
		if c.will != nil && !c.graceful {
			b.publishWill(*c.will)
		}
	}()

	pkt, err := mqttc.ReadPacket(c, 1<<20)
	if err != nil || pkt.Type != mqttc.TypeConnect {
		return
	}
	// 解析 CONNECT 体以取得遗嘱与 CleanSession。解析失败说明对面发的不是
	// 我们能理解的 3.1.1 报文：计数后拒绝，而不是当成「没有遗嘱」继续，
	// 否则测试会静默通过。
	info, cerr := mqttc.ParseConnect(pkt.Body)
	b.mu.Lock()
	b.connects++
	drop := b.dropOnConnect
	code := b.connackCode
	if cerr != nil {
		b.badConnects++
	} else {
		b.infos = append(b.infos, info)
	}
	b.mu.Unlock()
	if cerr != nil {
		return
	}
	if info.HasWill {
		w := info.Will
		c.will = &w
	}
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
			// retained 必须在这里落账：真实 Broker 会保存它并投给后来的
			// 订阅方。漏掉这一步，测试就只能验证「本进程发了字节」，
			// 验证不了「消费方真的能读到」。
			//
			// 零长度载荷 + retain 是 MQTT 规范里**删除**保留消息的唯一
			// 手段。不处理它，被删除的设备/报警会永远留在 Broker 上，
			// 而发送方以为自己已经清理干净了。
			if msg.Retain {
				if len(msg.Payload) == 0 {
					b.dropRetainedLocked(msg.Topic)
				} else {
					b.setRetainedLocked(msg)
				}
			}
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
			c.graceful = true
			return
		}
	}
}

// setRetainedLocked 保存一条保留消息，覆盖同主题的旧值（MQTT 语义：一个
// 主题只有一条保留消息）。调用方必须持有 b.mu。
func (b *Broker) setRetainedLocked(m Message) {
	for i, old := range b.retained {
		if old.Topic == m.Topic {
			b.retained[i] = m
			return
		}
	}
	b.retained = append(b.retained, m)
}

// dropRetainedLocked 删除某个主题的保留消息。调用方必须持有 b.mu。
func (b *Broker) dropRetainedLocked(topic string) {
	for i, old := range b.retained {
		if old.Topic == topic {
			b.retained = append(b.retained[:i], b.retained[i+1:]...)
			return
		}
	}
}

// publishWill 按遗嘱的 QoS/retain 代发一条消息，并把它投给订阅者。
//
// 真正的 Broker 会把遗嘱投递到订阅方；这里同时记进 msgs/last，
// 因为测试关注的是「Broker 有没有代发、发的是什么」，订阅方投递可以
// 用返回的连接数单独断言。
func (b *Broker) publishWill(w mqttc.Will) {
	b.mu.Lock()
	m := Message{Topic: w.Topic, Payload: append([]byte(nil), w.Payload...), QoS: w.QoS, Retain: w.Retain}
	b.wills = append(b.wills, m)
	b.msgs = append(b.msgs, m)
	b.last[m.Topic] = m
	if w.Retain {
		// retained 遗嘱要留在内存里，供后续订阅方在 SUBSCRIBE 时取到。
		b.setRetainedLocked(m)
	}
	b.mu.Unlock()
	b.deliver(m)
}

// deliver 把一条已落账的消息推给所有匹配的订阅者。
func (b *Broker) deliver(m Message) { _ = b.deliverCount(m) }

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
	// 新订阅方要立刻拿到 retained 消息，否则「连上就看不到当前状态」。
	// 这也是测试里验证「保留消息确实存在」的唯一途径。
	var replay []Message
	for _, m := range b.retained {
		for _, f := range filters {
			if TopicMatches(f.Filter, m.Topic) {
				replay = append(replay, m)
				break
			}
		}
	}
	b.mu.Unlock()

	body := []byte{byte(pid >> 8), byte(pid)}
	body = append(body, granted...)
	out := []byte{mqttc.TypeSuback << 4}
	out = append(out, encodeRemainingLen(len(body))...)
	out = append(out, body...)
	if err := c.write(out); err != nil {
		return false
	}
	// retained 必须在 SUBACK 之后发：客户端在等 SUBACK 时也会分发 PUBLISH，
	// 但先发 SUBACK 能让「订阅已建立」与「收到历史值」的因果顺序确定。
	for _, m := range replay {
		q := m.QoS
		var rpid uint16
		if q > 0 {
			rpid = 1
		}
		pkt, err := mqttc.PublishPacket(mqttc.Message{
			Topic: m.Topic, Payload: m.Payload, QoS: q, Retain: true,
		}, rpid)
		if err != nil {
			continue
		}
		if err := c.write(pkt); err != nil {
			return false
		}
	}
	return true
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
	m := Message{Topic: topic, Payload: append([]byte(nil), payload...), QoS: qos, Retain: retain}
	b.mu.Lock()
	if retain {
		if len(payload) == 0 {
			b.dropRetainedLocked(topic)
		} else {
			b.setRetainedLocked(m)
		}
	}
	b.mu.Unlock()
	return b.deliverCount(m)
}

// deliverCount 与 deliver 相同，但返回投递成功的连接数。
func (b *Broker) deliverCount(m Message) int {
	b.mu.Lock()
	clients := append([]*conn(nil), b.clients...)
	b.mu.Unlock()

	sent := 0
	for _, c := range clients {
		granted := c.grantedQoS(m.Topic)
		if granted == 255 {
			continue
		}
		q := m.QoS
		if granted < q {
			q = granted
		}
		var pid uint16
		if q > 0 {
			pid = 1
		}
		pkt, err := mqttc.PublishPacket(mqttc.Message{
			Topic: m.Topic, Payload: m.Payload, QoS: q, Retain: m.Retain,
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

// ConnectInfos 返回每条成功解析的 CONNECT（按到达顺序）。
//
// 测试用它断言「客户端线上到底承诺了什么」：CleanSession 是否为 true、
// 遗嘱 QoS 是否 1、retain 是否 false、Will 载荷里的 bdSeq 是否每次递增。
// 这些契约只看本地状态无法确认——本地对象可以是对的，字节是错的。
func (b *Broker) ConnectInfos() []mqttc.ConnectInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mqttc.ConnectInfo(nil), b.infos...)
}

// BadConnects 返回解析失败的 CONNECT 数量。
func (b *Broker) BadConnects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.badConnects
}

// Wills 返回 Broker 代发的遗嘱（按代发顺序）。
func (b *Broker) Wills() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.wills...)
}

// Retained 返回当前保存在 Broker 上的保留消息（按主题排序）。
//
// 这是验证「消费方到底能读到什么」的唯一可靠途径：客户端发了 retain=true
// 的零长度载荷只说明它想删除，Broker 是否真的删掉了要看这里。设备/报警
// 被删除后仍留在 retained 表里，本地测试全绿、现场看板却显示一台不存在的
// 设备——这类缺陷只有从这个角度才看得见。
func (b *Broker) Retained() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]Message(nil), b.retained...)
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// RetainedTopics 返回当前保留消息的主题列表（按主题排序）。
func (b *Broker) RetainedTopics() []string {
	out := make([]string, 0, 8)
	for _, m := range b.Retained() {
		out = append(out, m.Topic)
	}
	return out
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
