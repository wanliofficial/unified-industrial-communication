// Package knx 实现 KNXnet/IP 隧道（TUNNELING）内置从站，模拟一台接口网关
// 加上一条挂在它后面的总线。
//
// 与其它内置从站同理由：KNX 的失败模式几乎全在「链路建立」这一侧——通道
// 号被回收、序号错乱、网关满槽位、ACK 丢失、网关不回链路层确认。这些都不是
// 「报文格式对不对」的问题，靠编解码层的单元测试一个也覆盖不到。有一个能
// 按需制造这些故障的从站，才能把这些分支变成可重复的测试。
//
// 刻意做的四个行为（都对应真机会发生的事）：
//
//  1. **序号严格校验。** 期望值以外的序号一律 ACK 回 StatusSequenceNumber，
//     且不处理报文。真网关就是这么做的，而这一步恰好是隧道客户端最容易
//     写错的地方：把「发出后加一」当成「收到 ACK 后加一」，丢一次包之后
//     整条隧道就永久错位。
//
//  2. **重复序号只重发 ACK，不重复执行。** 客户端等不到 ACK 时用同一序号
//     重传，网关必须 ACK 但不能把那条 GroupValueWrite 再执行一次——否则
//     「一次丢包」会变成「开关被按了两次」。
//
//  3. **空的组地址不出声。** 读一个从站不知道的组地址时，唯一的真实行为
//     就是没有任何报文：总线上没有设备应答这个地址。若模拟器礼貌地回一个
//     错误，客户端就永远测不到「超时」这条最常走的路。
//
//  4. **链路层确认（L_Data.con）可关。** 一部分网关不给隧道客户端回 con。
//     驱动把「收不到 con」按成功处理，这条降级路径必须有地方验证。
package knx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	knxcodec "uic/internal/codec/knx"
	"uic/internal/model"
)

// DefaultMaxTunnels 是内置网关的隧道槽位数。
//
// 取 4 与常见接口设备一致：现场遇到的「连不上」有一大半是槽位被组态工具、
// 上一版驱动程序或同网段的另一台采集机占满。
const DefaultMaxTunnels = 4

// Options 是内置网关的可调参数。
type Options struct {
	// MaxTunnels 是允许同时存在的隧道数；0 使用 DefaultMaxTunnels。
	MaxTunnels int
	// DisableConfirm 模拟「不给隧道客户端回 L_Data.con」的网关。
	DisableConfirm bool
	// DisableAutoIncrement 关掉周期自增，便于断言固定值。
	DisableAutoIncrement bool
}

// tunnel 是一条已建立的隧道。
type tunnel struct {
	peer net.UDPAddr
	// rx 是期望收到的下一个 TUNNELLING_REQUEST 序号。
	rx uint8
	// lastRx 是最近一次成功处理的序号，用于识别重传。
	lastRx  uint8
	hasLast bool
	// tx 是本端发送 TUNNELLING_REQUEST 时使用的序号。
	tx uint8
}

// Server 是 KNXnet/IP 隧道从站。
type Server struct {
	pc         *net.UDPConn
	log        *slog.Logger
	maxTunnels int
	noConfirm  bool

	mu      sync.Mutex
	tunnels map[uint8]*tunnel
	nextCh  uint8
	// objects 是组地址对象表：组地址 → 线格式载荷。
	//
	// 刻意按「线格式」而不是「按 DPT 解码后的值」存：从站不知道点表声明的
	// DPT（那是客户端工程的属性），它只知道自己某个通信对象在总线上是多少
	// 字节。用 DPT 解码会强迫从站抄一份类型表，而那份表一旦与 codec 漂移，
	// 表现就是「读写都对，就是值不对」。
	objects map[knxcodec.GroupAddress]knxcodec.Payload

	autoIncrement bool
	closed        chan struct{}

	requests    int
	reads       int
	writes      int
	seqErrors   int
	retransmit  int
	badPackets  int
	connects    int
	refusals    int
	disconnects int
	// dropACKs 是「接下来要丢掉的 ACK 个数」，用于制造丢包。
	dropACKs int
	// failSeqOnce 让下一条 TUNNELLING_REQUEST 被回以序号错乱。
	failSeqOnce bool
	// failConfirms 让接下来 n 条 L_Data.con 带上「确认有错」。
	failConfirms int
}

// NewServer 在 addr 上监听 UDP。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string) (*Server, error) {
	return NewServerWithOptions(addr, Options{})
}

// NewServerWithOptions 带参数启动从站。
func NewServerWithOptions(addr string, opt Options) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("knx sim: resolve %q: %w", addr, err)
	}
	pc, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		return nil, fmt.Errorf("knx sim: listen %q: %w", addr, err)
	}
	max := opt.MaxTunnels
	if max <= 0 {
		max = DefaultMaxTunnels
	}
	s := &Server{
		pc:            pc,
		log:           slog.Default(),
		maxTunnels:    max,
		noConfirm:     opt.DisableConfirm,
		tunnels:       map[uint8]*tunnel{},
		nextCh:        1,
		objects:       map[knxcodec.GroupAddress]knxcodec.Payload{},
		autoIncrement: !opt.DisableAutoIncrement,
		closed:        make(chan struct{}),
	}
	s.installDefaults()
	go s.serve()
	if s.autoIncrement {
		go s.tick()
	}
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.pc.LocalAddr().String() }

// Close 停止监听。
func (s *Server) Close() error {
	select {
	case <-s.closed:
		// 已经关过：返回 nil 而不是 net.ErrClosed。关闭路径是「尽力而为」
		// 的善后，重复关闭应当幂等，否则 defer 里的一次 Close 会盖掉真正
		// 的错误。
		return nil
	default:
	}
	close(s.closed)
	err := s.pc.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// 观测与激励（测试 / 自检用）
// ---------------------------------------------------------------------------

// Requests 返回收到的 KNXnet/IP 报文总数。
func (s *Server) Requests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }

// Reads 返回处理的 GroupValueRead 数。
func (s *Server) Reads() int { s.mu.Lock(); defer s.mu.Unlock(); return s.reads }

// Writes 返回处理的 GroupValueWrite 数。
func (s *Server) Writes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.writes }

// Connects 返回成功建立的隧道数。
func (s *Server) Connects() int { s.mu.Lock(); defer s.mu.Unlock(); return s.connects }

// Refusals 返回因槽位已满而被拒绝的 CONNECT 数。
func (s *Server) Refusals() int { s.mu.Lock(); defer s.mu.Unlock(); return s.refusals }

// SeqErrors 返回因序号错乱被拒绝的隧道报文数。
func (s *Server) SeqErrors() int { s.mu.Lock(); defer s.mu.Unlock(); return s.seqErrors }

// Retransmits 返回识别出的重传（序号与上一条相同）数。
func (s *Server) Retransmits() int { s.mu.Lock(); defer s.mu.Unlock(); return s.retransmit }

// BadPackets 返回无法解析的报文数。
func (s *Server) BadPackets() int { s.mu.Lock(); defer s.mu.Unlock(); return s.badPackets }

// Tunnels 返回当前存活的隧道数。
func (s *Server) Tunnels() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.tunnels) }

// DropNextACKs 让接下来 n 条 TUNNELLING_ACK 被丢掉，用于测试重传。
func (s *Server) DropNextACKs(n int) { s.mu.Lock(); defer s.mu.Unlock(); s.dropACKs = n }

// FailNextSequence 让下一条 TUNNELLING_REQUEST 被回以序号错乱。
func (s *Server) FailNextSequence() { s.mu.Lock(); defer s.mu.Unlock(); s.failSeqOnce = true }

// FailNextConfirms 让接下来 n 条 L_Data.con 带上「确认有错」位，模拟总线级
// 发送失败（总线忙、冲突、执行器离线）。
func (s *Server) FailNextConfirms(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failConfirms = n
}

// ExpireTunnels 把所有隧道标记为不存在，模拟网关在空闲超时后回收通道号。
func (s *Server) ExpireTunnels() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tunnels = map[uint8]*tunnel{}
}

// SetPayload 直接设置一个组地址的线格式载荷。
func (s *Server) SetPayload(expr string, p knxcodec.Payload) error {
	ga, err := knxcodec.ParseGroupAddress(expr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[ga] = clonePayload(p)
	return nil
}

// Payload 读回一个组地址的线格式载荷。
func (s *Server) Payload(expr string) (knxcodec.Payload, bool) {
	ga, err := knxcodec.ParseGroupAddress(expr)
	if err != nil {
		return knxcodec.Payload{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.objects[ga]
	return clonePayload(p), ok
}

// SetValue 按 DPT 编码一个值写进组地址对象表。
//
// 从站在运行期并不需要 DPT，但默认对象表必须自己造出「线上是什么字节」——
// 用 codec 的编码器比在模拟器里手写 float16 位运算可靠得多。
func (s *Server) SetValue(expr, dpt string, v model.Value, t model.DataType) error {
	d, err := knxcodec.ParseDPT(dpt)
	if err != nil {
		return err
	}
	p, err := d.Encode(v, t)
	if err != nil {
		return err
	}
	return s.SetPayload(expr, p)
}

// SetBool 是 SetValue 的 1 位快捷方式。
func (s *Server) SetBool(expr string, on bool) error {
	return s.SetPayload(expr, knxcodec.Payload{Bit: on})
}

// SetNumber 按 DPT 写入一个数值。
func (s *Server) SetNumber(expr, dpt string, v float64, t model.DataType) error {
	return s.SetValue(expr, dpt, model.NumValue(t, v), t)
}

// Objects 返回对象表摘要（`1/2/3=2 字节`），供自检命令打印。
func (s *Server) Objects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for ga, p := range s.objects {
		if len(p.Data) == 0 {
			out = append(out, fmt.Sprintf("%s=%v(1 位)", ga, p.Bit))
			continue
		}
		out = append(out, fmt.Sprintf("%s=%d 字节", ga, len(p.Data)))
	}
	sort.Strings(out)
	return out
}

// DefaultObject 描述内置网关的一个默认组地址对象。
type DefaultObject struct {
	Group string  // 组地址，三级表示法
	DPT   string  // 数据点类型
	Value float64 // 初始值（1 位对象用 0/1）
	Bool  bool    // 该对象是否为 1 位
	Label string  // 一句话说明，供自检命令打印
}

// defaultObjects 是内置网关的对象表。
//
// 这些地址是自造的（不来自任何厂商工程的组地址分配），目的是让自检命令
// 一启动就有东西可读。表只有这一份：安装初始值与打印清单都从它读，避免
// 「文档里写的地址」与「实际上是配的地址」各说一套。
var defaultObjects = []DefaultObject{
	{Group: "1/1/1", DPT: "1.001", Bool: true, Label: "照明开关"},
	{Group: "1/1/2", DPT: "1.001", Bool: true, Value: 1, Label: "另一路开关（初始为开）"},
	{Group: "2/1/1", DPT: "9.001", Value: 21.5, Label: "室内温度 °C"},
	{Group: "2/1/2", DPT: "9.007", Value: 45, Label: "相对湿度 %"},
	{Group: "3/1/1", DPT: "5.001", Value: 50, Label: "调光/阀门位置 %"},
	{Group: "4/1/1", DPT: "14.056", Value: 1234.5, Label: "有功功率 W（每秒 +10）"},
	{Group: "5/1/1", DPT: "13.010", Value: 8888, Label: "有符号计数"},
}

// DefaultObjects 返回内置网关默认对象表的副本。
func DefaultObjects() []DefaultObject {
	out := make([]DefaultObject, len(defaultObjects))
	copy(out, defaultObjects)
	return out
}

// installDefaults 建立默认对象表：一组开关、模拟量与计数器。
func (s *Server) installDefaults() {
	for _, o := range defaultObjects {
		if o.Bool {
			if err := s.SetBool(o.Group, o.Value != 0); err != nil {
				panic("knx sim: default object " + o.Group + ": " + err.Error())
			}
			continue
		}
		if err := s.SetNumber(o.Group, o.DPT, o.Value, model.TypeFloat32); err != nil {
			panic("knx sim: default object " + o.Group + ": " + err.Error())
		}
	}
}

// tick 每秒把 4/1/1 的功率加 10 W，便于观察 SSE、死区与变化上报。
func (s *Server) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	ga := knxcodec.NewGroupAddress(4, 1, 1)
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			s.mu.Lock()
			if p, ok := s.objects[ga]; ok && len(p.Data) == 4 {
				f := math.Float32frombits(binary.BigEndian.Uint32(p.Data))
				p.Data = make([]byte, 4)
				binary.BigEndian.PutUint32(p.Data, math.Float32bits(f+10))
				s.objects[ga] = p
			}
			s.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// 收报文
// ---------------------------------------------------------------------------

func (s *Server) serve() {
	buf := make([]byte, 65535)
	for {
		n, raddr, err := s.pc.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		req := append([]byte(nil), buf[:n]...)
		for _, out := range s.handle(req, raddr) {
			if _, err := s.pc.WriteToUDP(out, raddr); err != nil {
				select {
				case <-s.closed:
					return
				default:
				}
			}
		}
	}
}

// handle 处理一条报文，返回要按顺序回送的报文列表。
//
// 顺序是真实的：先 ACK，再链路层确认，最后总线指示。客户端如果按「先等
// ACK、再等值」实现，这里正好按它的顺序走；如果它把响应先收到也没关系，
// 驱动侧的 inbox 能兜住。
func (s *Server) handle(datagram []byte, from *net.UDPAddr) [][]byte {
	f, err := knxcodec.ParseFrame(datagram)
	if err != nil {
		s.mu.Lock()
		s.badPackets++
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	switch f.Service {
	case knxcodec.SvcConnectRequest:
		return s.onConnect(f.Payload, from)
	case knxcodec.SvcConnectionStateReq:
		return s.onState(f.Payload)
	case knxcodec.SvcDisconnectRequest:
		return s.onDisconnect(f.Payload)
	case knxcodec.SvcTunnellingRequest:
		return s.onTunnel(f.Payload)
	case knxcodec.SvcTunnellingACK:
		// 客户端对我们发出的指示帧的确认。本从站不等它（不等也能正确
		// 工作），但收到就说明客户端在正常 ACK，不做处理。
		return nil
	}
	return nil
}

func (s *Server) onConnect(payload []byte, from *net.UDPAddr) [][]byte {
	if _, err := knxcodec.ParseConnectRequest(payload); err != nil {
		// CRI 不是隧道（例如设备管理连接）：明确拒绝，而不是静默丢弃。
		// 静默会让客户端等满一个超时，还以为是网络问题。
		s.mu.Lock()
		s.badPackets++
		s.mu.Unlock()
		return [][]byte{knxcodec.BuildFrame(knxcodec.SvcConnectResponse,
			knxcodec.ConnectResponse{Status: knxcodec.StatusConnectionType}.Encode())}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tunnels) >= s.maxTunnels {
		s.refusals++
		return [][]byte{knxcodec.BuildFrame(knxcodec.SvcConnectResponse,
			knxcodec.ConnectResponse{Status: knxcodec.StatusNoMoreConnections}.Encode())}
	}
	ch := s.nextCh
	for s.tunnels[ch] != nil {
		ch++
		if ch == 0 {
			ch = 1
		}
	}
	s.nextCh = ch + 1
	if s.nextCh == 0 {
		s.nextCh = 1
	}
	peer := *from
	s.tunnels[ch] = &tunnel{peer: peer}
	s.connects++

	// 数据端点：告诉客户端「往哪儿发隧道报文」，也就是本端监听地址。
	// 网关若处在 NAT 后面，这里要回它自己看到的公网地址；本地测试直接
	// 用监听地址即可。
	local, _ := knxcodec.NewHPAI(s.pc.LocalAddr().String())
	resp := knxcodec.ConnectResponse{
		Channel: ch,
		Status:  knxcodec.StatusNoError,
		Data:    local,
		// 分配的物理地址：区域 15、线路 15、设备号 241..244（+通道号）。
		//
		// 隧道客户端几乎从不校验这个值，但它会出现在日志与总线上，所以取
		// 一段「不可能是真实设备」的号：15.15.254 留给自己（模拟执行器），
		// 240 以下留给现场设备。
		Assigned: knxcodec.IndividualAddress(0xFF00 | 0xF0 | uint16(ch)),
		HasCRD:   true,
	}
	return [][]byte{knxcodec.BuildFrame(knxcodec.SvcConnectResponse, resp.Encode())}
}

func (s *Server) onState(payload []byte) [][]byte {
	req, err := knxcodec.ParseConnectionStateRequest(payload)
	if err != nil {
		s.mu.Lock()
		s.badPackets++
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	_, ok := s.tunnels[req.Channel]
	s.mu.Unlock()
	status := byte(knxcodec.StatusNoError)
	if !ok {
		status = knxcodec.StatusConnectionID
	}
	resp := knxcodec.ConnectionStateResponse{Channel: req.Channel, Status: status}
	return [][]byte{knxcodec.BuildFrame(knxcodec.SvcConnectionStateRes, resp.Encode())}
}

func (s *Server) onDisconnect(payload []byte) [][]byte {
	req, err := knxcodec.ParseDisconnectRequest(payload)
	if err != nil {
		s.mu.Lock()
		s.badPackets++
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	_, ok := s.tunnels[req.Channel]
	if ok {
		delete(s.tunnels, req.Channel)
		s.disconnects++
	}
	s.mu.Unlock()
	status := byte(knxcodec.StatusNoError)
	if !ok {
		status = knxcodec.StatusConnectionID
	}
	resp := knxcodec.DisconnectResponse{Channel: req.Channel, Status: status}
	return [][]byte{knxcodec.BuildFrame(knxcodec.SvcDisconnectResponse, resp.Encode())}
}

// onTunnel 处理 TUNNELLING_REQUEST：校验通道与序号、ACK、再处理 cEMI。
func (s *Server) onTunnel(payload []byte) [][]byte {
	req, err := knxcodec.ParseTunnellingRequest(payload)
	if err != nil {
		s.mu.Lock()
		s.badPackets++
		s.mu.Unlock()
		return nil
	}

	s.mu.Lock()
	t, ok := s.tunnels[req.Channel]
	if !ok {
		s.mu.Unlock()
		// 通道号不存在：ACK 里带 StatusConnectionID，让客户端立刻知道
		// 隧道已经没了，而不是继续往一个不存在的通道里发报文。
		ack := knxcodec.TunnellingACK{Channel: req.Channel, Seq: req.Seq, Status: knxcodec.StatusConnectionID}
		return [][]byte{knxcodec.BuildFrame(knxcodec.SvcTunnellingACK, ack.Encode())}
	}
	// 序号校验。三种情况必须分开：
	//   与期望相同     → 正常处理，期望值加一
	//   与上一条相同   → 客户端重传（它没收到 ACK），只重发 ACK
	//   其它           → 真的错位，回 StatusSequenceNumber
	if t.hasLast && req.Seq == t.lastRx {
		s.retransmit++
		s.mu.Unlock()
		ack := knxcodec.TunnellingACK{Channel: req.Channel, Seq: req.Seq, Status: knxcodec.StatusNoError}
		return s.maybeDropACK(knxcodec.BuildFrame(knxcodec.SvcTunnellingACK, ack.Encode()))
	}
	if req.Seq != t.rx || s.failSeqOnce {
		s.seqErrors++
		s.failSeqOnce = false
		s.mu.Unlock()
		ack := knxcodec.TunnellingACK{Channel: req.Channel, Seq: req.Seq, Status: knxcodec.StatusSequenceNumber}
		return [][]byte{knxcodec.BuildFrame(knxcodec.SvcTunnellingACK, ack.Encode())}
	}
	t.rx++
	t.lastRx = req.Seq
	t.hasLast = true
	s.mu.Unlock()

	out := [][]byte{}
	ack := knxcodec.TunnellingACK{Channel: req.Channel, Seq: req.Seq, Status: knxcodec.StatusNoError}
	out = append(out, s.maybeDropACK(knxcodec.BuildFrame(knxcodec.SvcTunnellingACK, ack.Encode()))...)

	// 链路层确认：客户端把一帧 L_Data.req 交给网关，网关确认它上了总线。
	// 这一帧必须带 MsgLDataCon，且控制字段的「确认有错」位表示总线上有没有
	// 设备应答。
	con := s.linkConfirm(req.CEMI)
	if con != nil && !s.noConfirm {
		out = append(out, s.tunnelFrame(req.Channel, con)...)
	}
	out = append(out, s.handleCEMI(req.Channel, req.CEMI)...)
	return out
}

// maybeDropACK 实现「丢掉接下来 n 个 ACK」。
func (s *Server) maybeDropACK(frame []byte) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropACKs > 0 {
		s.dropACKs--
		return nil
	}
	return [][]byte{frame}
}

// linkConfirm 构造 L_Data.con（对本端 L_Data.req 的确认）。
func (s *Server) linkConfirm(cemi []byte) []byte {
	m, err := knxcodec.ParseLData(cemi)
	if err != nil || m.MsgCode != knxcodec.MsgLDataReq {
		return nil
	}
	// 「确认有错」位表示**总线级**的问题（无人应答、总线忙、冲突），而不是
	// 「这个组地址上有没有设备在听」。
	//
	// 这两件事经常被混为一谈，但它们在协议里是不同的层：组地址是广播语义，
	// 写一个没人订阅的组地址在链路层是「成功」的——没有任何设备需要为它
	// 负责。因此内置网关默认把 con 报成正常；只有通过 FailNextConfirms 显式
	// 注入总线故障时，才把这一位置起来。
	//
	// 这一点对驱动的写路径很关键：驱动不能假设「没收到 con 就是失败」，
	// 也不能假设「con 正常就是有人在听」。
	s.mu.Lock()
	fail := s.failConfirms > 0
	if fail {
		s.failConfirms--
	}
	s.mu.Unlock()

	out, err := knxcodec.BuildLDataCon(uint16(m.Source), m.Dest, m.IsGroup, m.APCI, nil, false, fail)
	if err != nil {
		return nil
	}
	return out
}

// handleCEMI 处理隧道里送来的 L_Data.req。
func (s *Server) handleCEMI(channel uint8, cemi []byte) [][]byte {
	m, err := knxcodec.ParseLData(cemi)
	if err != nil || m.MsgCode != knxcodec.MsgLDataReq || !m.IsGroup {
		return nil
	}
	ga := knxcodec.GroupAddress(m.Dest)

	switch m.APCI {
	case knxcodec.APCIGroupValueRead:
		s.mu.Lock()
		p, ok := s.objects[ga]
		if ok {
			s.reads++
		}
		s.mu.Unlock()
		if !ok {
			// 总线上没有设备拥有这个组地址：什么都不回。客户端会超时，
			// 而那个超时正是它应该报告给运维的事实。
			return nil
		}
		ind, err := knxcodec.BuildLData(knxcodec.MsgLDataInd, uint16(s.ownAddress()), uint16(ga), true,
			knxcodec.APCIGroupValueResponse, p.Data, p.Bit)
		if err != nil {
			return nil
		}
		return s.tunnelFrame(channel, ind)
	case knxcodec.APCIGroupValueWrite:
		p := knxcodec.Payload{Bit: m.Bit}
		if len(m.Data) > 0 {
			p.Data = append([]byte(nil), m.Data...)
		}
		s.mu.Lock()
		// 只更新已有的对象：真实网关不存组对象，写报文只是被转发到总线，
		// 由「拥有该组地址的执行器」接收。给未知组地址凭空造一个对象，会
		// 让「写一个不存在的地址」在测试里变成可见的成功。
		if _, ok := s.objects[ga]; !ok {
			s.mu.Unlock()
			// 总线上没有执行器认领这个组地址：不发指示，也没有任何设备
			// 会因此改变状态。链路层确认仍然是正常的（见 linkConfirm）。
			return nil
		}
		s.objects[ga] = p
		s.writes++
		s.mu.Unlock()

		// 写入也要向总线广播一条指示（这正是「写完之后其它设备才知道」
		// 的机制）。
		ind, err := knxcodec.BuildLData(knxcodec.MsgLDataInd, uint16(s.ownAddress()), uint16(ga), true,
			knxcodec.APCIGroupValueWrite, p.Data, p.Bit)
		if err != nil {
			return nil
		}
		return s.tunnelFrame(channel, ind)
	}
	return nil
}

// ownAddress 是从站在总线上使用的物理地址（区域 15 线路 15 设备 254）。
func (s *Server) ownAddress() knxcodec.IndividualAddress {
	return knxcodec.IndividualAddress(0x0FFE)
}

// tunnelFrame 把一条 cEMI 包进 TUNNELLING_REQUEST。
func (s *Server) tunnelFrame(channel uint8, cemi []byte) [][]byte {
	s.mu.Lock()
	t, ok := s.tunnels[channel]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	seq := t.tx
	t.tx++
	s.mu.Unlock()
	req := knxcodec.TunnellingRequest{Channel: channel, Seq: seq, CEMI: cemi}
	return [][]byte{knxcodec.BuildFrame(knxcodec.SvcTunnellingRequest, req.Encode())}
}

func clonePayload(p knxcodec.Payload) knxcodec.Payload {
	if len(p.Data) > 0 {
		p.Data = append([]byte(nil), p.Data...)
	}
	return p
}
