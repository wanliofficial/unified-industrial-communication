package mqtt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client 是一个最小 MQTT 3.1.1 发布客户端。
//
// 并发模型：一个后台 goroutine 唯一负责从连接读报文，其他 goroutine 通过
// 通道等 PUBACK。这样避免了「多个 goroutine 同时读同一个 socket」——那会
// 让报文被随机切成两半，是最难查的一类串扰。
type Client struct {
	opt Options

	mu    sync.Mutex
	conn  net.Conn
	state atomic.Int32 // 0=idle 1=connecting 2=ready 3=closed

	writeMu sync.Mutex
	nextPID atomic.Uint32

	pendingMu sync.Mutex
	// pending 把 packet id 映射到等待者。用 chan error 而不是 chan struct{}：
	// 连接断开时必须让等待者**立刻失败**，而不是等满 AckTimeout，更不能
	// 因为通道被关闭而把零值误判成「已确认」。
	pending map[uint16]chan error

	// pingWait 是「等 PINGRESP」的等待者。PINGRESP 报文里没有 packet id，
	// 所以不能复用 pending：同时只能有一个在飞的 PINGREQ。
	//
	// 用 pingFlight（done 关闭 + 共享错误）而不是 chan error：一次探测只有
	// 一个结论，但等待者可能有多个（Read 的链路自检与 Probe 会同时探测）。
	// 向 channel 里塞值只能唤醒其中一个，其余会在健康链路上白等满
	// AckTimeout 并误判成断链；close 才能把同一个结论广播给所有等待者。
	//
	// 结论必须带 error 而不是裸 struct{}：PINGRESP 到达（成功）与 dropConn
	// （链路已死）都会结束这次探测，只关闭 channel 无法区分两者，后到的
	// Ping 会在死链路上误报成功（nil）。
	pingMu   sync.Mutex
	pingWait *pingFlight

	done     chan struct{}
	doneOnce sync.Once

	lastErrMu sync.Mutex
	lastErr   error

	// 统计：让运维能回答「到底发出去了没有」。
	sent      atomic.Uint64
	acked     atomic.Uint64
	recv      atomic.Uint64
	reconnect atomic.Uint64

	// epoch 每次 CONNECT/CONNACK 成功 +1。订阅方靠它判断「这是一条新会话」，
	// 从而在发送数据之前先补 BIRTH——OnConnect 是异步的，单靠回调无法保证
	// 第一次 Publish 发生在回调之后。
	epoch atomic.Uint64
}

// Options 是客户端参数。
type Options struct {
	// Addr 形如 "tcp://h:1883"、"h:1883"、":1883"。仅支持 tcp（不引 TLS/WS
	// 依赖：TLS 需要证书管理，是独立的一件事，不在这里偷偷降级）。
	Addr      string
	ClientID  string
	Username  string
	Password  string
	KeepAlive time.Duration
	// ConnectTimeout 限制单次 CONNECT 往返。
	ConnectTimeout time.Duration
	// AckTimeout 是等待 PUBACK 的上限。超时即视为投递失败，由 Outbox 重发。
	AckTimeout time.Duration
	// MaxPacket 限制入站报文大小。
	MaxPacket int
	// Will 返回本次 CONNECT 要登记的遗嘱；nil 表示不带遗嘱。
	//
	// 做成回调而不是常量字段，是因为遗嘱内容**每次都不同**：Sparkplug 要求
	// 每个新 CONNECT 的 bdSeq 比上一个 +1（255 后回绕 0），而 bdSeq 就写在
	// NDEATH 遗嘱载荷里。用常量字段会在第一次重连之后就让所有 NDEATH 带上
	// 同一个 bdSeq，Host 无法区分「这次掉线」与「上一次掉线」的旧消息。
	//
	// 回调在持有连接锁的情况下调用，必须快速返回且不能回调进客户端。
	Will func() *Will

	// OnConnect 在每次 CONNECT/CONNACK 成功后调用（含重连）。它由独立
	// goroutine 执行，因此可以安全地调用 Publish——但不要在这里做长阻塞。
	//
	// 用途：Broker 重启会清掉 retained 状态，消费方看到的设备状态就永远
	// 停在掉线前。重连后主动重发 birth/state 才能让这条链路自愈。
	OnConnect func()

	// OnMessage 在收到 PUBLISH 时调用。它在读循环里同步执行，必须快速
	// 返回——不要在这里做网络调用或磁盘写。南向订阅取数用它把消息塞进
	// 本地缓存，慢消费者应自己开队列。
	//
	// 建链的订阅窗口（SUBACK 之前的 retained/在途消息）也会调用它，此时
	// 连接锁仍被 Connect 持有：回调里只能碰自己的状态，不得再调客户端的
	// Connect / Publish / Ping / Close。
	OnMessage func(Publish)

	// Subscriptions 是「连接建立后要恢复的订阅」。CleanSession=true 时
	// Broker 不保留订阅，断线重连后必须重新 SUBSCRIBE——忘了这一步的
	// 典型现象是「重连成功但再也收不到数据」。
	Subscriptions []TopicFilter
}

func (o Options) withDefaults() Options {
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 5 * time.Second
	}
	if o.AckTimeout <= 0 {
		o.AckTimeout = 5 * time.Second
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 30 * time.Second
	}
	if o.MaxPacket <= 0 {
		o.MaxPacket = 1 << 20 // 1 MiB
	}
	return o
}

// New 创建客户端但不连接。连接由 Connect 或首次 Publish 触发。
func New(opt Options) *Client {
	c := &Client{
		opt:     opt.withDefaults(),
		pending: map[uint16]chan error{},
		done:    make(chan struct{}),
	}
	return c
}

// Connect 建立连接并完成 CONNECT/CONNACK 握手。
func (c *Client) Connect(ctx context.Context) error {
	if c.state.Load() == 3 {
		return errors.New("mqtt: client closed")
	}
	if c.state.Load() == 2 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Load() == 2 {
		return nil
	}
	c.state.Store(1)
	defer func() {
		if c.state.Load() == 1 {
			c.state.Store(0)
		}
	}()

	addr, err := NormalizeAddr(c.opt.Addr)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: c.opt.ConnectTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mqtt: dial %s: %w", addr, err)
	}

	pkt, err := ConnectPacket(ConnectOptions{
		ClientID:     c.opt.ClientID,
		Username:     c.opt.Username,
		Password:     c.opt.Password,
		KeepAlive:    uint16(c.opt.KeepAlive / time.Second),
		CleanSession: true,
		Will:         c.pendingWill(),
	})
	if err != nil {
		_ = conn.Close()
		return err
	}

	deadline := time.Now().Add(c.opt.ConnectTimeout)
	_ = conn.SetDeadline(deadline)
	if err := writeAll(conn, pkt); err != nil {
		_ = conn.Close()
		return fmt.Errorf("mqtt: send connect: %w", err)
	}
	resp, err := ReadPacket(conn, c.opt.MaxPacket)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mqtt: read connack: %w", err)
	}
	if resp.Type != TypeConnack {
		_ = conn.Close()
		return fmt.Errorf("mqtt: expected CONNACK, got packet type %d", resp.Type)
	}
	_, code, err := ParseConnack(resp.Body)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if code != ConnAccepted {
		_ = conn.Close()
		return fmt.Errorf("mqtt: connection refused: %s", ConnackCode(code))
	}

	// 握手完成后取消 deadline：后续超时由单条消息的 AckTimeout 与
	// 后台读循环控制。留着握手 deadline 会让连接在 5 秒后整体失效。
	_ = conn.SetDeadline(time.Time{})
	c.conn = conn
	c.state.Store(2)
	c.epoch.Add(1)

	// 订阅必须在启动读循环之前完成，且由**当前 goroutine 自己读 socket**：
	// 两个 goroutine 同时读同一条 TCP 流会把报文切碎，是这类客户端最难
	// 排查的串扰。期间到达的 retained PUBLISH 在这里顺手分发（它们往往
	// 就是设备的初始值，丢掉会让点位在重启后一直空着）。
	if len(c.opt.Subscriptions) > 0 {
		if err := c.subscribeLocked(conn, c.opt.Subscriptions); err != nil {
			c.conn = nil
			c.state.Store(0)
			_ = conn.Close()
			return err
		}
	}

	go c.readLoop(conn)
	go c.keepaliveLoop(conn)
	if c.opt.OnConnect != nil {
		go c.opt.OnConnect()
	}
	return nil
}

// pendingWill 取本次 CONNECT 的遗嘱。回调可能为 nil（不发遗嘱）。
func (c *Client) pendingWill() *Will {
	if c.opt.Will == nil {
		return nil
	}
	return c.opt.Will()
}

// subscribeLocked 在已持有 c.mu 的情况下发 SUBSCRIBE 并等 SUBACK。
//
// 单独抽出来是因为重连路径也要用它：CleanSession=true 时 Broker 不保留
// 订阅，重连后不重新订阅就会出现「连上了但永远收不到数据」——这是
// 订阅型接入最常见的一个坑，而且从外部看与「设备没发数据」无法区分。
func (c *Client) subscribeLocked(conn net.Conn, filters []TopicFilter) error {
	if len(filters) == 0 {
		return nil
	}
	pid := c.allocPID()
	pkt, err := SubscribePacket(filters, pid)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(c.opt.AckTimeout)
	if err := c.writePacket(conn, deadline, pkt); err != nil {
		return fmt.Errorf("mqtt: send subscribe: %w", err)
	}

	// 等 SUBACK。期间可能夹着 retained 的 PUBLISH，必须边收边分发，
	// 不能丢弃——它们往往就是设备的初始值。
	_ = conn.SetReadDeadline(deadline)
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	for {
		resp, err := ReadPacket(conn, c.opt.MaxPacket)
		if err != nil {
			return fmt.Errorf("mqtt: wait suback: %w", err)
		}
		switch resp.Type {
		case TypeSuback:
			rpid, granted, perr := ParseSuback(resp.Body)
			if perr != nil {
				return perr
			}
			if rpid != pid {
				// 迟到的 SUBACK（上一次重连的）。继续等当前这个。
				continue
			}
			for i, g := range granted {
				if g == SubackFailure {
					name := ""
					if i < len(filters) {
						name = filters[i].Filter
					}
					// 被拒绝必须报错：静默继续会得到一个「显示在线但
					// 永远没数据」的设备，是最难排查的一类现场问题。
					return fmt.Errorf("mqtt: subscription %q rejected by broker (check ACL)", name)
				}
			}
			return nil
		case TypePublish:
			if err := c.dispatchPublish(conn, resp); err != nil {
				// 订阅窗口里的非法 PUBLISH 也必须让本次建链失败：
				// 带着错误语义继续订阅，等于把故障藏进「连上了但不对」。
				return fmt.Errorf("mqtt: subscribe window: %w", err)
			}
		default:
			// 其它报文忽略：这里不是读循环，不该处理 PUBACK 等语义。
		}
	}
}

// dispatchPublish 把一条 PUBLISH 交给 OnMessage，并按 QoS 回 PUBACK。
//
// conn 由调用方显式传入，而不是在这里从 c.conn 取：Connect 在订阅窗口里
// 持有连接锁，若这里再 c.mu.Lock() 就是不可重入的自锁（SUBACK 之前到达的
// QoS1 保留消息会让 Connect 永久卡死）。调用方本来就知道自己读的是哪条
// 连接，加锁只会引入这条死路。
func (c *Client) dispatchPublish(conn net.Conn, pkt Packet) error {
	p, err := ParsePublish(pkt)
	if err != nil {
		return err
	}
	if p.QoS == 1 {
		// 写失败不在这里判死链路：readLoop 下一次读会得到同一个错误，
		// 由它统一走 dropConn。
		_ = c.writePacket(conn, time.Now().Add(c.opt.AckTimeout), PubackPacket(p.PID))
	}
	c.recv.Add(1)
	if c.opt.OnMessage != nil {
		c.opt.OnMessage(p)
	}
	return nil
}

// Epoch 返回当前连接代数：每成功建立一次会话 +1。
//
// 订阅型接入需要它来区分「同一条连接上的第 N 条消息」与「重连后的第一条
// 消息」。后者必须先发 BIRTH，否则消费方会把数据挂在一个未出生的节点上。
func (c *Client) Epoch() uint64 { return c.epoch.Load() }

// Publish 发布一条消息。QoS0 发完即返回；QoS1 等到 PUBACK 才返回成功。
//
// 这里刻意**不做内部重试**：重试策略属于 Outbox（它知道位点、TTL 与上限）。
// 在客户端里静默重试会把「投递失败」变成「投递变慢」，掩盖真实故障。
func (c *Client) Publish(ctx context.Context, m Message) error {
	if m.QoS > 1 {
		return fmt.Errorf("mqtt: qos %d not supported by publisher (0 or 1)", m.QoS)
	}
	if err := c.Connect(ctx); err != nil {
		return err
	}

	var pid uint16
	var ackCh chan error
	if m.QoS > 0 {
		ackCh = make(chan error, 1)
		// 选一个当前没有在飞的 packet id。直接用 allocPID 的单调计数器一旦
		// 回绕到某个仍在 pending 的 pid，新的 ackCh 会覆盖旧的——旧发布者永远
		// 收不到自己的 PUBACK，要白等满 AckTimeout 才报错，被 Outbox 误判为
		// 投递失败而重发（at-least-once 下不算丢数据，但是虚假失败 + 无谓重发）。
		pid = c.allocFreePID(ackCh)
		defer func() {
			c.pendingMu.Lock()
			delete(c.pending, pid)
			c.pendingMu.Unlock()
		}()
	}
	pkt, err := PublishPacket(m, pid)
	if err != nil {
		return err
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("mqtt: not connected")
	}
	if err = c.writePacket(conn, c.writeDeadline(ctx), pkt); err != nil {
		// 写失败意味着连接已不可用；主动断开，让下一次 Publish 重连，
		// 而不是把一个半死连接留在那儿。
		c.dropConn(conn)
		return fmt.Errorf("mqtt: publish: %w", err)
	}
	c.sent.Add(1)

	if m.QoS == 0 {
		return nil
	}

	timer := time.NewTimer(c.opt.AckTimeout)
	defer timer.Stop()
	select {
	case aerr := <-ackCh:
		if aerr != nil {
			return aerr
		}
		c.acked.Add(1)
		return nil
	case <-timer.C:
		return fmt.Errorf("mqtt: no PUBACK for pid %d within %s", pid, c.opt.AckTimeout)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("mqtt: client closed")
	}
}

// errPingConnLost 是链路在 PINGREQ 在飞期间断开时，发给所有等 PINGRESP
// 等待者的错误。用它而不是裸 errors.New 是为了让等待者能识别「这不是超时、
// 是连接已断」。
var errPingConnLost = errors.New("mqtt: connection lost before PINGRESP")

// pingFlight 是一次在飞的 PINGREQ。done 关闭时广播结论（err 在关闭前写入，
// 关闭后只读）。多个等待者读同一个结论，正好对应「PINGRESP 里没有 packet id
// 可以区分是在回谁」这一事实。
type pingFlight struct {
	done chan struct{}
	err  error
}

// finishPingLocked 给出一次在飞探测的结论并广播。调用方必须持有 pingMu。
func (c *Client) finishPingLocked(err error) {
	f := c.pingWait
	if f == nil {
		return
	}
	c.pingWait = nil
	f.err = err
	close(f.done)
}

// Ping 做一次真正的往返探测：发 PINGREQ 并等 PINGRESP。
//
// 只「写成功」不算活着——写进内核缓冲就返回成功，对端早就没了也看不出来。
// 订阅型驱动没有请求-响应可探，PINGRESP 是唯一能证明对端还在说话的信号。
// 超时即判定链路已死并主动断开，让下一次 Connect 重建。
func (c *Client) Ping(ctx context.Context) error {
	if err := c.Connect(ctx); err != nil {
		return err
	}
	c.pingMu.Lock()
	if prev := c.pingWait; prev != nil {
		// 已有一次在飞的 PINGREQ：等它的结论就行，不必叠加。
		c.pingMu.Unlock()
		select {
		case <-prev.done:
			// 结论由结束方给出：PINGRESP 是 nil，dropConn 是连接已断；
			// 不能把后者当成成功返回。
			return prev.err
		case <-time.After(c.opt.AckTimeout):
			return fmt.Errorf("mqtt: no PINGRESP within %s", c.opt.AckTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("mqtt: client closed")
		}
	}
	f := &pingFlight{done: make(chan struct{})}
	c.pingWait = f
	c.pingMu.Unlock()
	defer func() {
		// 超时/取消返回时把这次探测摘掉，下一次 Ping 重新发 PINGREQ，而不是
		// 挂在一个永远不会有人给出结论的飞行体上。
		c.pingMu.Lock()
		if c.pingWait == f {
			c.pingWait = nil
		}
		c.pingMu.Unlock()
	}()

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("mqtt: not connected")
	}
	if err := c.writePacket(conn, c.writeDeadline(ctx), PingreqPacket()); err != nil {
		c.dropConn(conn)
		return fmt.Errorf("mqtt: ping: %w", err)
	}

	timer := time.NewTimer(c.opt.AckTimeout)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.err
	case <-timer.C:
		c.dropConn(conn)
		return fmt.Errorf("mqtt: no PINGRESP within %s", c.opt.AckTimeout)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("mqtt: client closed")
	}
}

// Close 发送 DISCONNECT 并关闭连接。幂等。
func (c *Client) Close() error {
	c.doneOnce.Do(func() { close(c.done) })
	c.state.Store(3)

	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	// 正常关闭：发 DISCONNECT。Broker 因此不会触发遗嘱消息。
	_ = c.writePacket(conn, time.Now().Add(time.Second), DisconnectPacket())
	return conn.Close()
}

// Stats 返回投递统计，供指标与诊断使用。
type Stats struct {
	Sent      uint64
	Acked     uint64
	Recv      uint64
	Reconnect uint64
	Connected bool
	LastError string
}

func (c *Client) Stats() Stats {
	s := Stats{
		Sent:      c.sent.Load(),
		Acked:     c.acked.Load(),
		Recv:      c.recv.Load(),
		Reconnect: c.reconnect.Load(),
		Connected: c.state.Load() == 2,
	}
	c.lastErrMu.Lock()
	if c.lastErr != nil {
		s.LastError = c.lastErr.Error()
	}
	c.lastErrMu.Unlock()
	return s
}

func (c *Client) setErr(err error) {
	c.lastErrMu.Lock()
	c.lastErr = err
	c.lastErrMu.Unlock()
}

func (c *Client) allocPID() uint16 {
	for {
		v := c.nextPID.Add(1)
		if pid := uint16(v); pid != 0 {
			return pid
		}
	}
}

// allocFreePID 返回当前不在 pending 里的 packet id，并把 ackCh 登记进去。
//
// 与 allocPID 的差别：allocPID 是纯单调计数器，回绕时会撞上某个仍在 pending 的
// 老发布者，导致「新 ackCh 覆盖旧的」。这里在 pendingMu 下选一个确实空闲的 id，
// 消除那条竞态。pending 里在飞的数量远小于 65536，循环通常一两步就命中；只有在
// 全部 id 都占满时才会一直转（那等于已经积压了 6.5 万条未确认的 QoS1 发布）。
func (c *Client) allocFreePID(ackCh chan error) uint16 {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for {
		v := c.nextPID.Add(1)
		if pid := uint16(v); pid != 0 {
			if _, busy := c.pending[pid]; !busy {
				c.pending[pid] = ackCh
				return pid
			}
		}
	}
}

// dropConn 关闭当前连接并清空状态，使下一次 Publish 重新建链。
func (c *Client) dropConn(conn net.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	if c.state.Load() != 3 {
		c.state.Store(0)
		c.reconnect.Add(1)
	}
	_ = conn.Close()
	// 唤醒所有等待 PUBACK 的发布者：连接已经没了，继续等只是白白拖长
	// Outbox 的失败判定。
	c.failPending(errors.New("mqtt: connection lost before PUBACK"))
	// 唤醒等 PINGRESP 的等待者，避免保活循环白等满 AckTimeout。链路已经
	// 断了，必须显式告知「失败」而不是只关闭 channel——否则并发的 Ping
	// 会把这次关闭误当成 PINGRESP 成功。
	c.pingMu.Lock()
	c.finishPingLocked(errPingConnLost)
	c.pingMu.Unlock()
}

// failPending 让所有在飞的 QoS1 发布立即失败。
func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	chans := make([]chan error, 0, len(c.pending))
	for _, ch := range c.pending {
		chans = append(chans, ch)
	}
	c.pendingMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- err:
		default:
		}
	}
}

// readLoop 是唯一读 socket 的 goroutine。
func (c *Client) readLoop(conn net.Conn) {
	for {
		pkt, err := ReadPacket(conn, c.opt.MaxPacket)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
			}
			c.setErr(err)
			c.dropConn(conn)
			return
		}
		switch pkt.Type {
		case TypePublish:
			// 订阅侧的入站消息。QoS1 必须回 PUBACK，否则 Broker 会一直
			// 重投，最终把连接判定为无响应。
			if err := c.dispatchPublish(conn, pkt); err != nil {
				// 非法 PUBLISH（QoS 3、空主题、主题带通配符）是协议
				// 违约，规范要求关闭网络连接。继续读下去只会让后续报文
				// 在一个已经不可信的主题空间上被解释。
				c.setErr(err)
				c.dropConn(conn)
				return
			}
		case TypePuback:
			pid, perr := ParsePuback(pkt.Body)
			if perr != nil {
				c.setErr(perr)
				continue
			}
			c.pendingMu.Lock()
			ch := c.pending[pid]
			c.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- nil:
				default:
				}
			}
		case TypePingresp:
			// 保活得到回应，说明链路仍然活着（不只是「写成功」）。
			c.pingMu.Lock()
			c.finishPingLocked(nil)
			c.pingMu.Unlock()
		default:
			// 订阅类报文本客户端不使用；忽略而不报错，避免 Broker 推送
			// 非预期报文时把连接打掉。
		}
	}
}

// keepaliveLoop 定期发 PINGREQ 并等 PINGRESP。
//
// 必须等回应：只发不等的话，对端断电、网线被拔、NAT 表项过期这几种情况
// 都不会让本地 socket 报错——写进内核缓冲就返回成功。表现为「连接标志一直
// 是已连接，但一条数据都收不到」，这是订阅型接入最典型的静默故障。
func (c *Client) keepaliveLoop(conn net.Conn) {
	interval := c.opt.KeepAlive / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.mu.Lock()
			alive := c.conn == conn
			c.mu.Unlock()
			if !alive {
				return
			}

			wait := &pingFlight{done: make(chan struct{})}
			c.pingMu.Lock()
			if c.pingWait != nil {
				// 已经有一次 PINGREQ 在飞（比如 Probe 刚发过）。不要在飞
				// 两条：PINGRESP 里没有 packet id，无法区分是回哪一条的。
				c.pingMu.Unlock()
				continue
			}
			c.pingWait = wait
			c.pingMu.Unlock()

			err := c.writePacket(conn, time.Now().Add(c.opt.AckTimeout), PingreqPacket())
			if err != nil {
				c.setErr(err)
				c.dropConn(conn)
				return
			}

			timer := time.NewTimer(c.opt.AckTimeout)
			select {
			case <-wait.done:
				// nil = PINGRESP 到达（链路活）；非 nil = 链路已断，退出保活。
				timer.Stop()
				if wait.err != nil {
					c.setErr(wait.err)
					return
				}
			case <-timer.C:
				c.setErr(errors.New("mqtt: keepalive timeout (no PINGRESP)"))
				c.dropConn(conn)
				return
			case <-c.done:
				timer.Stop()
				return
			}
		}
	}
}

// writeDeadline 返回一条报文写入的上界：AckTimeout 与调用方 ctx 的 deadline
// 取较小者。ctx 的 deadline 可能远在 AckTimeout 之后，直接用它会让一条
// 卡死的写把探测拖得比设计的超时还长。
func (c *Client) writeDeadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.opt.AckTimeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

// writePacket 写一条报文：写之前设上界，写完清除。
//
// 两件事都必须做，而且是同一个 bug 的两面：
//   - 不设上界：对端停止读取（半死链路、接收窗口占满）时 Write 会永久阻塞，
//     Ping/保活根本走不到自己的超时判断，链路状态被永久冻结；
//   - 设了不清除：写超时是粘在 net.Conn 上的。Publish 用 now+AckTimeout 作
//     写截止时间，这一次写完之后若不清除，等那一刻过去，后续所有写——保活
//     PINGREQ、入站 QoS1 的 PUBACK、DISCONNECT——都会立刻返回 i/o timeout。
//     表现是「发布间隔大于 AckTimeout 的连接会在下一次保活时被自己判死」。
//
// 设置与清除都放在 writeMu 里：并发的两次写各自设自己的上界，谁也不能把
// 对方的截止时间清掉。
func (c *Client) writePacket(conn net.Conn, deadline time.Time, p []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(deadline)
	err := writeAll(conn, p)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// writeAll 写满整个报文：net.Conn.Write 允许部分写，MQTT 帧被截断
// 会让对端立刻失去帧同步。
func writeAll(w interface{ Write([]byte) (int, error) }, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return errors.New("mqtt: short write")
		}
		p = p[n:]
	}
	return nil
}

// NormalizeAddr 把 broker 地址规范成 host:port。
//
// 同时导出给 config 包做装配期校验：`uic -check` 必须能拦住 ssl:// 这类
// 本实现不支持的 scheme，否则「CI 通过、启动才发现」的错误要等到部署后
// 才暴露。校验规则只有这一处，避免两处漂移。
//
// 显式拒绝 tls:// 与 ws://：本项目未实现 TLS 与 WebSocket 传输，静默当成
// tcp 连接会在生产里变成「连上了但不加密」，这是比直接失败严重得多的错误。
func NormalizeAddr(addr string) (string, error) {
	if addr == "" {
		return "", errors.New("mqtt: broker address required")
	}
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		return strings.TrimPrefix(addr, "tcp://"), nil
	case strings.HasPrefix(addr, "ssl://"), strings.HasPrefix(addr, "tls://"),
		strings.HasPrefix(addr, "wss://"), strings.HasPrefix(addr, "ws://"):
		return "", fmt.Errorf("mqtt: scheme in %q not supported (tcp only); TLS/WebSocket need separate implementation", addr)
	}
	return addr, nil
}
