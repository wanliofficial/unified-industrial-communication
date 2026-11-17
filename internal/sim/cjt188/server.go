// Package cjt188 提供户用计量仪表（CJ/T 188-2004）的内存从站，用于自检与演示。
//
// 从站复用 codec/cjt188 的组帧/解帧、DI schema 与 BCD 编码，因此「主站解
// 出来的值」与「从站写进去的值」不会因为两边各写一套 BCD 而漂移。
//
// 刻意只实现一台表的核心行为：
//
//   - 读计量数据（0x01），901F 与 D120..D12B；
//   - 读地址（0x03，T 与地址域全 AA）；
//   - 写阀门（0x04 + A017）；
//   - 写地址（0x15 + A018）。
//
// 不做价格、金额、密钥、机电同步。它们的数据结构随厂商差异极大，内置
// 从站的价值是验证帧层与常用数据项，不是模拟一台完整的收费表。
package cjt188

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	cj "uic/internal/codec/cjt188"
	"uic/internal/transport"
)

// Server 是一个内存表计从站。
type Server struct {
	mu     sync.RWMutex
	addr   cj.Addr
	mtype  byte
	values map[cj.DI][]byte
	clock  time.Time
	// valve 是阀门状态：0 开、1 关（与状态字 D0/D1 一致）。
	valve    byte
	valveAt  time.Time
	writes   int
	requests int64
	// dropAfter > 0 时，从站在成功应答这么多次后主动断开当前连接。
	dropAfter int
	// missing 是「表不支持」的数据标识：收到请求时回异常应答。
	missing []cj.DI

	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool
	stream transport.Stream
}

// Option 调整从站的初始状态。
type Option func(*Server)

// WithAddress 设置 14 位表地址。
func WithAddress(s string) Option {
	return func(srv *Server) {
		if a, err := cj.ParseAddr(s); err == nil {
			srv.addr = a
		}
	}
}

// WithType 设置仪表类型 T（10 冷水/20 热量/30 燃气…）。
func WithType(t byte) Option {
	return func(srv *Server) { srv.mtype = t }
}

// WithDropAfter 让从站在成功应答 n 次后主动断开当前连接（0 = 不断开）。
//
// 用于自检「一轮采集途中链路断开」的处置：驱动必须保留已读到的样本，
// 并把剩余点位显式标坏后立即返回，而不是逐个等到超时。
func WithDropAfter(n int) Option {
	return func(srv *Server) { srv.dropAfter = n }
}

// WithoutValue 让从站假装没有实现某个数据标识（收到请求时回异常应答）。
// 真实的表几乎都只实现标准的一个子集：点表里配了一个该表不支持的数据标识，
// 得到的是异常应答而不是超时。这正是「一个坏 DI 不该把整台表拖成每周期重连」
// 那条契约的现场来源，因此从站必须能复现它。
func WithoutValue(di string) Option {
	return func(srv *Server) {
		d, err := cj.ParseDI(di)
		if err != nil {
			panic("sim/cjt188: WithoutValue: " + err.Error())
		}
		srv.missing = append(srv.missing, d)
	}
}

// NewServer 创建并启动 TCP 从站；addr 为 "127.0.0.1:0" 时自动分配端口。
func NewServer(addr string, opts ...Option) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newServer(opts...)
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func newServer(opts ...Option) *Server {
	s := &Server{
		addr:    cj.Addr{0x01, 0x00, 0x00, 0x05, 0x08, 0x00, 0x00}, // 00000805000001
		mtype:   0x10,
		values:  map[cj.DI][]byte{},
		clock:   time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local),
		conns:   map[net.Conn]struct{}{},
		valve:   0,
		valveAt: time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local),
	}
	for _, o := range opts {
		o(s)
	}
	s.seed()
	return s
}

// seed 预置与 examples/cjt188.json 对应的数据。
func (s *Server) seed() {
	if cj.FamilyOf(s.mtype) == cj.FamilyHeat {
		s.seedHeat()
	} else {
		s.seedWater()
	}
	// 上 1..12 月结算日量：严格递增，方便断言「D12B 是上 12 月」。
	for n := 1; n <= 12; n++ {
		di := cj.DI{Hi: 0xD1, Lo: byte(0x20 + n - 1)}
		val := 1200.0 - float64(n)*10
		var b []byte
		var err error
		if cj.FamilyOf(s.mtype) == cj.FamilyHeat {
			b, err = cj.EncodeScaled(val, 4, 2, cj.QtyEnergy, 0x11) // GJ
		} else {
			b, err = cj.EncodeScaled(val, 4, 2, cj.QtyVolume, 0x2C) // m³
		}
		if err != nil {
			panic(err)
		}
		s.values[di] = b
	}
}

func (s *Server) seedWater() {
	var payload []byte
	add := func(v float64, unit byte) {
		b, err := cj.EncodeScaled(v, 4, 2, cj.QtyVolume, unit)
		if err != nil {
			panic(err)
		}
		payload = append(payload, b...)
	}
	add(1234.56, 0x2C) // 当前累积流量 1234.56 m³
	add(1200.00, 0x2C) // 结算日累积流量 1200.00 m³
	payload = append(payload, cj.TimeBytes(s.clock)...)
	payload = append(payload, cj.EncodeStatus(uint16(s.valve))...)
	s.values[cj.DIReadData] = payload
}

func (s *Server) seedHeat() {
	var payload []byte
	add := func(v float64, length, decimals int, qty cj.Qty, unit byte) {
		b, err := cj.EncodeScaled(v, length, decimals, qty, unit)
		if err != nil {
			panic(err)
		}
		payload = append(payload, b...)
	}
	add(120.00, 4, 2, cj.QtyEnergy, 0x11) // 结算日热量 GJ
	add(123.45, 4, 2, cj.QtyEnergy, 0x11) // 当前热量 GJ
	add(12.34, 4, 2, cj.QtyPower, 0x17)   // 热功率 kW
	add(1.2345, 4, 4, cj.QtyFlow, 0x35)   // 瞬时流量 m³/h
	add(456.78, 4, 2, cj.QtyVolume, 0x2C) // 累积流量 m³
	add(55.00, 3, 2, cj.QtyNone, 0)       // 供水温度
	add(45.00, 3, 2, cj.QtyNone, 0)       // 回水温度
	add(1234, 3, 0, cj.QtyNone, 0)        // 累积工作时间 h
	payload = append(payload, cj.TimeBytes(s.clock)...)
	payload = append(payload, cj.EncodeStatus(uint16(s.valve))...)
	s.values[cj.DIReadData] = payload
}

// Addr 返回监听地址（TCP 模式）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Address 返回 14 位表地址文本。
func (s *Server) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr.String()
}

// Type 返回仪表类型。
func (s *Server) Type() byte { return s.mtype }

// SetClock 推进从站时钟；901F 里的实时时间字段会随下一次读刷新。
func (s *Server) SetClock(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = t
}

// Clock 返回当前时钟。
func (s *Server) Clock() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clock
}

// SetValue 直接设置整个 DI 数据域（不含 DI 与 SER）。
func (s *Server) SetValue(di string, raw []byte) error {
	d, err := cj.ParseDI(di)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[d] = append([]byte(nil), raw...)
	return nil
}

// Value 读取 DI 数据域的快照。
func (s *Server) Value(di string) []byte {
	d, err := cj.ParseDI(di)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.values[d]...)
}

// Valve 返回最近一次阀门动作（0 开、1 关）与时间。
func (s *Server) Valve() (byte, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.valve, s.valveAt
}

// Requests 返回收到的请求帧数（含地址不匹配被静默丢弃的帧）。
func (s *Server) Requests() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.requests
}

// Writes 返回成功写入的次数。
func (s *Server) Writes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writes
}

// Close 停止从站；幂等。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[net.Conn]struct{}{}
	st := s.stream
	s.mu.Unlock()

	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	if st != nil {
		_ = st.Close()
	}
	s.wg.Wait()
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	st := &tcpStream{conn: c}
	_ = s.ServeStream(st)
}

// ServeStream 在一条字节流上跑从站循环。串口/pty 与 TCP 共用它。
func (s *Server) ServeStream(st transport.Stream) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	// 记录流以便 Close 能主动打断阻塞读（pty 场景）。
	s.stream = st
	s.mu.Unlock()

	rd := cj.NewReader(st)
	replies := 0
	for {
		fr, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || isClosed(err) {
				return nil
			}
			return err
		}
		resp, silent := s.handle(fr)
		if silent || len(resp) == 0 {
			continue
		}
		if err := st.Write(resp); err != nil {
			return err
		}
		replies++
		if s.dropAfter > 0 && replies >= s.dropAfter {
			// 模拟串口服务器/DTU 掉线：这一次应答已经发出，下一帧不再
			// 有对端。返回后由 serveConn 关闭底层连接。
			return nil
		}
	}
}

// handle 处理一帧请求，返回应答帧（nil 表示不应答）。
func (s *Server) handle(fr cj.Frame) ([]byte, bool) {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	s.mu.RLock()
	myAddr := s.addr
	s.mu.RUnlock()
	broadcast := fr.Addr.IsBroadcast()
	if !broadcast && fr.Addr != myAddr {
		// 总线上不是我的事：保持静默。替别人的地址应答是串扰。
		return nil, true
	}
	switch fr.Ctrl {
	case cj.CtrlRead:
		if broadcast {
			// 广播读在标准里没有定义；表不应答。
			return nil, true
		}
		return s.handleRead(fr)
	case cj.CtrlReadAddr:
		// 读地址必须单机操作：请求里 T 与地址全 AA，应答带真实地址。
		return s.handleReadAddr(fr)
	case cj.CtrlWrite:
		if broadcast {
			return nil, true
		}
		return s.handleWrite(fr)
	case cj.CtrlWriteAddr:
		if broadcast {
			return nil, true
		}
		return s.handleWriteAddr(fr)
	}
	// 未知/未实现的命令（含写机电同步 0x16）：明确回异常，让主站知道
	// 「表收到了但做不了」，而不是等到超时。
	return s.exception(fr), false
}

func (s *Server) handleRead(fr cj.Frame) ([]byte, bool) {
	if len(fr.Data) < 3 {
		return s.exception(fr), false
	}
	di, err := cj.DIFromBytes(fr.Data)
	if err != nil {
		return s.exception(fr), false
	}
	ser := fr.Data[2]
	s.mu.RLock()
	payload, ok := s.values[di]
	out := append([]byte(nil), payload...)
	clock := s.clock
	valve := s.valve
	missing := false
	for _, m := range s.missing {
		if m == di {
			missing = true
			break
		}
	}
	s.mu.RUnlock()
	if missing || !ok {
		return s.exception(fr), false
	}
	// 时间与状态是「活的值」：SetClock/写阀门之后必须能从 901F 读回来，
	// 否则自检会看到「写成功但读到的还是旧值」。
	if di == cj.DIReadData {
		if fields, ok := cj.Fields(s.mtype, di); ok {
			for _, f := range fields {
				switch f.Kind {
				case cj.FieldTime:
					copy(out[f.Offset:], cj.TimeBytes(clock))
				case cj.FieldStatus:
					copy(out[f.Offset:], cj.EncodeStatus(uint16(valve)))
				}
			}
		}
	}
	return s.reply(fr.Addr, cj.CtrlRead|cj.BitReply, di, ser, out), false
}

func (s *Server) handleReadAddr(fr cj.Frame) ([]byte, bool) {
	if len(fr.Data) < 3 {
		return s.exception(fr), false
	}
	// 读地址的报文标识在帧里是字面量 81 0A（大端 DI{0x81,0x0A} 的直接写出），
	// 这里逐字节比较即可；与 handleRead 一样从帧首两字节取标识，语义清晰。
	if fr.Data[0] != 0x81 || fr.Data[1] != 0x0A {
		return s.exception(fr), false
	}
	ser := fr.Data[2]
	// 应答的地址域必须是**实际地址**，不能原样回显请求里的全 AA。
	s.mu.RLock()
	myAddr := s.addr
	s.mu.RUnlock()
	return s.replyWire(myAddr, cj.CtrlReadAddr|cj.BitReply, [2]byte{0x81, 0x0A}, ser, nil), false
}

func (s *Server) handleWrite(fr cj.Frame) ([]byte, bool) {
	if len(fr.Data) < 3 {
		return s.exception(fr), false
	}
	di, err := cj.DIFromBytes(fr.Data)
	if err != nil {
		return s.exception(fr), false
	}
	ser := fr.Data[2]
	payload := fr.Data[3:]
	if di != cj.DIValve {
		// 只支持写阀门。写其它 DI（价格、密钥…）明确拒绝。
		return s.exception(fr), false
	}
	if len(payload) < 1 {
		return s.exception(fr), false
	}
	var valve byte
	switch payload[0] {
	case 0x55:
		valve = 0
	case 0x99:
		valve = 1
	default:
		return s.exception(fr), false
	}
	s.mu.Lock()
	s.valve = valve
	s.valveAt = time.Now()
	s.writes++
	s.mu.Unlock()
	// 写阀应答带 2 字节状态（标准表 9），第一字节 D0/D1 是阀门状态。
	return s.reply(fr.Addr, cj.CtrlWrite|cj.BitReply, cj.DIValve, ser,
		[]byte{valve, 0x00}), false
}

func (s *Server) handleWriteAddr(fr cj.Frame) ([]byte, bool) {
	// 数据域 = DI(2) + SER(1) + 7 字节新地址。
	if len(fr.Data) != 10 {
		return s.exception(fr), false
	}
	di, err := cj.DIFromBytes(fr.Data)
	if err != nil || di != cj.DIWriteAddr {
		return s.exception(fr), false
	}
	ser := fr.Data[2]
	var next cj.Addr
	copy(next[:], fr.Data[3:10])
	if next.HasWildcard() {
		return s.exception(fr), false
	}
	s.mu.Lock()
	s.addr = next
	s.writes++
	s.mu.Unlock()
	return s.reply(fr.Addr, cj.CtrlWriteAddr|cj.BitReply, cj.DIWriteAddr, ser, nil), false
}

// reply 组「DI + SER + 数据」应答（DI 按 2004 的大端变换，901F → 90 1F）。
func (s *Server) reply(addr cj.Addr, ctrl byte, di cj.DI, ser byte, payload []byte) []byte {
	b := di.Bytes()
	return s.replyWire(addr, ctrl, b, ser, payload)
}

// replyWire 用给定的线序两字节组应答，供读地址这类字面量报文标识使用。
func (s *Server) replyWire(addr cj.Addr, ctrl byte, diWire [2]byte, ser byte, payload []byte) []byte {
	data := make([]byte, 0, 3+len(payload))
	data = append(data, diWire[0], diWire[1], ser)
	data = append(data, payload...)
	fr, err := cj.Build(s.mtype, addr, ctrl, data)
	if err != nil {
		return nil
	}
	return fr
}

// exception 组异常应答：控制码 = 请求 + 0x80 + 0x40，数据域 = DI + SER。
func (s *Server) exception(fr cj.Frame) []byte {
	var data []byte
	if len(fr.Data) >= 3 {
		data = append(data, fr.Data[0], fr.Data[1], fr.Data[2])
	}
	out, err := cj.Build(s.mtype, fr.Addr, fr.Ctrl|cj.BitReply|cj.BitError, data)
	if err != nil {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// 传输
// ---------------------------------------------------------------------------

// tcpStream 把 net.Conn 适配成 transport.Stream。串口侧用 transport.OpenSerial，
// 它本身已经是 Stream，不需要适配。
type tcpStream struct {
	conn net.Conn
}

func (t *tcpStream) Write(p []byte) error {
	_, err := t.conn.Write(p)
	return err
}

func (t *tcpStream) ReadFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(t.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (t *tcpStream) Close() error { return t.conn.Close() }

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
