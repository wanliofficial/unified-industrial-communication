// Package dlt645 提供多功能电能表（645 协议）的内存从站，用于自检与演示。
//
// 从站复用 codec/dlt645 的组帧/解帧与 DI 表，因此「主站解出来的值」与
// 「从站写进去的值」不会因为两边各写一套 BCD 而漂移——那类 bug 在真实
// 电表上无法复现，只能靠内置从站抓。
//
// 刻意只实现「一台表」的核心行为：
//
//   - 读数据（0x11 / 0x12）与读通信地址（0x13）；
//   - 写数据（0x14），带密码校验与可写性校验；
//   - 冻结（0x16）、广播校时（0x08）与拉合闸扩展（0x1C）。
//
// 不做负荷记录、事件记录、费率时段表。它们的数据结构随厂商差异极大，
// 内置从站的价值是验证帧层与常用数据项，不是模拟一台完整的费控表。
package dlt645

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	dlt "uic/internal/codec/dlt645"
	"uic/internal/transport"
)

// Value 是从站里一个数据项的内存表示。
type Value struct {
	// Raw 是**未加 0x33**、低字节在前的数据域真值。
	Raw []byte
	// Writable 表示允许写数据（0x14）。
	Writable bool
}

// Server 是一个内存电表从站。
type Server struct {
	mu     sync.RWMutex
	addr   [dlt.AddrLen]byte
	values map[dlt.DI]Value
	// password 为空表示不校验密码（PA/P0..P2 都接受）。
	password []byte
	// relay 记录最近一次拉合闸命令的动作，供自检读取。
	relay    byte
	relayAt  time.Time
	clock    time.Time
	writes   int
	requests int64

	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool
	// streams 是由 pty 等方式注入的单条链路（见 pty_linux.go）。
	stream transport.Stream
}

// Option 调整从站的初始状态。
type Option func(*Server)

// WithAddress 设置通信地址（12 位 BCD 文本）。
func WithAddress(s string) Option {
	return func(srv *Server) {
		if a, err := dlt.ParseAddr(s); err == nil {
			srv.addr = a
		}
	}
}

// WithPassword 要求写/控制命令携带指定密码（3 字节真值）。
func WithPassword(p [3]byte) Option {
	return func(srv *Server) { srv.password = p[:] }
}

// NewServer 创建并启动 TCP 从站；addr 为 "127.0.0.1:0" 时自动分配端口。
//
// TCP 承载不是标准的一部分，但它是唯一能在 CI 里跑、且不依赖 pty 的
// 承载方式；分帧逻辑与串口完全一致。
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
		addr:     [dlt.AddrLen]byte{0x42, 0, 0, 0, 0, 0}, // 000000000042
		values:   map[dlt.DI]Value{},
		conns:    map[net.Conn]struct{}{},
		clock:    time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local),
		password: nil,
	}
	for _, o := range opts {
		o(s)
	}
	s.seed()
	return s
}

// seed 预置与 examples/dlt645.json 对应的数据项。
func (s *Server) seed() {
	set := func(di string, raw []byte, writable bool) {
		d, err := dlt.ParseDI(di)
		if err != nil {
			panic(err)
		}
		s.values[d] = Value{Raw: raw, Writable: writable}
	}
	// setBCD 按数据项的标准宽度写值：BCD 编码必须写满宽度，否则主站会
	// 因为「字节数不符」拒绝——这与真实电表的行为一致（它永远按标准
	// 宽度回，不会因为数值小而少回一个字节）。
	setBCD := func(di string, v uint64, width int, writable bool) {
		raw := bcdLEWidth(v, width)
		set(di, raw, writable)
	}
	// 组合有功总电能 123456.78 kWh：低字节在前。
	setBCD("00000000", 12345678, 4, false)
	setBCD("00010000", 12345678, 4, false)
	setBCD("00020000", 0, 4, false)
	setBCD("00030000", 4321, 4, false)
	// A/B/C 电压 220.0 / 219.8 / 220.3 V。
	setBCD("02010100", 2200, 2, false)
	setBCD("02010200", 2198, 2, false)
	setBCD("02010300", 2203, 2, false)
	// 电流 5.123 / 4.987 / 5.201 A（3 字节）。
	setBCD("02020100", 5123, 3, false)
	setBCD("02020200", 4987, 3, false)
	setBCD("02020300", 5201, 3, false)
	// 电压数据块：三相各 2 字节，共 6 字节。数据块在平台只按 bytes 交付，
	// 用来演示「读取一个块，由上层按项拆分」的用法。
	block := append(bcdLEWidth(2200, 2), bcdLEWidth(2198, 2)...)
	block = append(block, bcdLEWidth(2203, 2)...)
	set("0201FF00", block, false)
	// 瞬时总有功功率 1.2345 kW（3 字节、四位小数）。
	setBCD("02030000", 12345, 3, false)
	// 功率因数 0.985（2 字节、三位小数）。
	setBCD("02060000", 985, 2, false)
	// 电网频率 50.00 Hz。
	setBCD("02800002", 5000, 2, false)
	// 最大需量 12.3456 kW + 发生时间 11-07 12:00 —— 8 字节复合。
	// 需量按数值小端编码；发生时间按 MM DD hh mm 字段序（与日期数据项同形，
	// 不是小端整数），颠倒后会被读成「59 月 23 日」。
	demand := append(bcdLEWidth(123456, 4), []byte{0x11, 0x07, 0x12, 0x00}...)
	set("01010000", demand, false)
	// 日期及时间（6 字节 ss mm hh DD MM YY）。
	clockBytes := dlt.TimeBytes(s.clock)
	set("04000101", clockBytes[:], true)
	// 通信地址（6 字节 BCD）。
	set("04000401", s.addr[:], true)
	// 电表运行状态字。
	setBCD("04000402", 0, 2, false)
	// 第 1 结算日：4 字节 MMDDhhmm（没有年）。注意这不是小端 BCD：
	// 字节序是 MM DD hh mm，用「数值小端」编码会把月份与分钟对调。
	set("04000B01", []byte{0x11, 0x07, 0x12, 0x00}, true)
}

// Addr 返回监听地址（TCP 模式）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Address 返回通信地址文本。
func (s *Server) Address() string { return dlt.FormatAddr(s.addr) }

// SetValue 直接设置数据项（未加 0x33 的真值，低字节在前）。
func (s *Server) SetValue(di string, raw []byte) error {
	d, err := dlt.ParseDI(di)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.values[d]
	v.Raw = append([]byte(nil), raw...)
	s.values[d] = v
	return nil
}

// Value 读取数据项当前值（未加 0x33 的真值）。
func (s *Server) Value(di string) []byte {
	d, err := dlt.ParseDI(di)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.values[d].Raw...)
}

// Relay 返回最近一次拉合闸动作（0 表示从未收到）。
func (s *Server) Relay() (byte, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.relay, s.relayAt
}

// Writes 返回成功写入的次数，供测试断言「主站确实写进来了」。
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
	rd := dlt.NewReader(st)
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
		// 从站不应答时也把请求算作「收到过」，便于诊断「请求到了但被丢弃」。
		if err := st.Write(resp); err != nil {
			return err
		}
	}
}

// handle 处理一帧请求，返回应答帧（nil 表示不应答）。
func (s *Server) handle(fr dlt.Frame) ([]byte, bool) {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	// 广播地址只对特定的两类命令有效。
	broadcast := fr.Addr == dlt.Broadcast()
	if !broadcast && !dlt.MatchAddr(s.addr, fr.Addr) {
		// 总线上不是我的事：保持静默。替别人的地址应答是串扰。
		return nil, true
	}
	switch fr.Ctrl {
	case dlt.CtrlRead:
		return s.handleRead(fr)
	case dlt.CtrlReadNext:
		return s.handleReadNext(fr)
	case dlt.CtrlReadAddr:
		if broadcast {
			return nil, true
		}
		// 应答的地址域必须是**实际地址**，不能原样回显请求里的全 AA：
		// 主站靠它确认「应答我的是我点名的那块表」。
		return s.build(s.addr, dlt.CtrlReadAddr|dlt.BitReply, s.addr[:])
	case dlt.CtrlWrite:
		return s.handleWrite(fr)
	case dlt.CtrlFreeze:
		// 广播冻结：执行但不应答。
		if broadcast {
			return nil, true
		}
		return s.build(fr.Addr, dlt.CtrlFreeze|dlt.BitReply, nil)
	case dlt.CtrlBroadcastTime:
		s.handleBroadcastTime(fr)
		return nil, true
	case dlt.CtrlRelay:
		return s.handleRelay(fr)
	}
	return nil, true
}

func (s *Server) handleRead(fr dlt.Frame) ([]byte, bool) {
	if len(fr.Data) < 4 {
		return s.exception(fr, dlt.CtrlRead, dlt.ErrOther), false
	}
	di, err := dlt.DIFromBytes(fr.Data)
	if err != nil {
		return s.exception(fr, dlt.CtrlRead, dlt.ErrOther), false
	}
	s.mu.RLock()
	v, ok := s.values[di]
	clock := s.clock
	s.mu.RUnlock()
	if !ok {
		// 「无请求数据」——这是标准里该场景唯一正确的错误位。
		return s.exception(fr, dlt.CtrlRead, dlt.ErrNoData), false
	}
	raw := v.Raw
	// 时间是「活的值」：广播校时改的是 s.clock，读时钟数据项必须反映
	// 它，否则校时看上去生效了（写不报错）却读不回来。
	if it, known := dlt.Lookup(di); known && it.Kind == dlt.KindTime && len(raw) == 6 {
		tb := dlt.TimeBytes(clock)
		raw = tb[:]
	}
	ctrl := byte(dlt.CtrlRead | dlt.BitReply)
	// 超过单帧容量才拆帧。块大小固定为 followChunk：读后续应答还要多带
	// 一个帧序号字节，所以它比 MaxData-4 少 1，这样两种帧都在 L≤200 内。
	if len(raw) > followChunk {
		ctrl |= dlt.BitFollow
		return s.buildWith(fr.Addr, ctrl, di, raw[:followChunk], 0)
	}
	return s.buildWith(fr.Addr, ctrl, di, raw, 0)
}

func (s *Server) handleReadNext(fr dlt.Frame) ([]byte, bool) {
	if len(fr.Data) < 5 {
		return s.exception(fr, dlt.CtrlReadNext, dlt.ErrOther), false
	}
	di, err := dlt.DIFromBytes(fr.Data)
	if err != nil {
		return s.exception(fr, dlt.CtrlReadNext, dlt.ErrOther), false
	}
	seq := fr.Data[4]
	s.mu.RLock()
	v, ok := s.values[di]
	s.mu.RUnlock()
	if !ok {
		return s.exception(fr, dlt.CtrlReadNext, dlt.ErrNoData), false
	}
	// 从站把「已发出多少」编码进序号：序号 n 对应第 n 段之后的位置。
	// 这让无状态实现也能正确续传，且不会因主站丢帧而错位。
	off := int(seq) * followChunk
	if off >= len(v.Raw) {
		return s.exception(fr, dlt.CtrlReadNext, dlt.ErrNoData), false
	}
	rest := v.Raw[off:]
	ctrl := byte(dlt.CtrlReadNext | dlt.BitReply)
	if len(rest) > followChunk {
		rest = rest[:followChunk]
		ctrl |= dlt.BitFollow
	}
	return s.buildWith(fr.Addr, ctrl, di, rest, seq)
}

func (s *Server) handleWrite(fr dlt.Frame) ([]byte, bool) {
	// 数据域 = DI(4) + PA/P0P1P2(4) + C0..C3(4) + 参数。
	if len(fr.Data) < 12 {
		return s.exception(fr, dlt.CtrlWrite, dlt.ErrOther), false
	}
	di, err := dlt.DIFromBytes(fr.Data)
	if err != nil {
		return s.exception(fr, dlt.CtrlWrite, dlt.ErrOther), false
	}
	perm := fr.Data[4]
	pwd := fr.Data[5:8]
	payload := fr.Data[12:]
	if perm > 0x09 {
		// 密文/安全认证权限（如 98H）不在本从站范围内：明确拒绝，而不是
		// 假装成功——「写了但没生效」比「写了被拒绝」难查得多。
		return s.exception(fr, dlt.CtrlWrite, dlt.ErrPassword), false
	}
	if len(s.password) > 0 && string(pwd) != string(s.password) {
		return s.exception(fr, dlt.CtrlWrite, dlt.ErrPassword), false
	}
	s.mu.Lock()
	v, ok := s.values[di]
	if !ok {
		s.mu.Unlock()
		return s.exception(fr, dlt.CtrlWrite, dlt.ErrNoData), false
	}
	if !v.Writable {
		s.mu.Unlock()
		return s.exception(fr, dlt.CtrlWrite, dlt.ErrOther), false
	}
	if len(payload) > 0 {
		v.Raw = append([]byte(nil), payload...)
		// 写时钟同时推进内部时钟：读路径是从 s.clock 生成时间的，
		// 只改 v.Raw 会让「写成功但读回来还是旧时间」。
		if it, known := dlt.Lookup(di); known && it.Kind == dlt.KindTime && len(payload) == 6 {
			if t, terr := dlt.ParseTime(payload); terr == nil {
				s.clock = t
			}
		}
	}
	s.values[di] = v
	s.writes++
	s.mu.Unlock()
	return s.build(fr.Addr, dlt.CtrlWrite|dlt.BitReply, nil)
}

func (s *Server) handleBroadcastTime(fr dlt.Frame) {
	if len(fr.Data) < 6 {
		return
	}
	if t, err := dlt.ParseTime(fr.Data); err == nil {
		s.mu.Lock()
		s.clock = t
		s.mu.Unlock()
	}
}

func (s *Server) handleRelay(fr dlt.Frame) ([]byte, bool) {
	// 数据域 = PA/P0P1P2(4) + C0..C3(4) + 控制类型(1) + 保留(1) + 时间(6)。
	if len(fr.Data) < 12 {
		return s.exception(fr, dlt.CtrlRelay, dlt.ErrOther), false
	}
	perm := fr.Data[0]
	pwd := fr.Data[1:4]
	action := fr.Data[8]
	if perm > 0x09 {
		return s.exception(fr, dlt.CtrlRelay, dlt.ErrPassword), false
	}
	if len(s.password) > 0 && string(pwd) != string(s.password) {
		return s.exception(fr, dlt.CtrlRelay, dlt.ErrPassword), false
	}
	if action != dlt.RelayTrip && action != dlt.RelayAllow {
		return s.exception(fr, dlt.CtrlRelay, dlt.ErrOther), false
	}
	s.mu.Lock()
	s.relay = action
	s.relayAt = time.Now()
	s.mu.Unlock()
	return s.build(fr.Addr, dlt.CtrlRelay|dlt.BitReply, nil)
}

// ---------------------------------------------------------------------------
// 组帧
// ---------------------------------------------------------------------------

func (s *Server) build(addr [dlt.AddrLen]byte, ctrl byte, data []byte) ([]byte, bool) {
	fr, err := dlt.Build(addr, ctrl, data)
	if err != nil {
		return nil, true
	}
	return fr, false
}

// buildWith 组「DI 回显 + 数据（+ 序号）」应答。
func (s *Server) buildWith(addr [dlt.AddrLen]byte, ctrl byte, di dlt.DI, data []byte, seq byte) ([]byte, bool) {
	b := di.Bytes()
	out := make([]byte, 0, 4+len(data)+1)
	out = append(out, b[:]...)
	out = append(out, data...)
	if seq != 0 {
		out = append(out, seq)
	}
	return s.build(addr, ctrl, out)
}

func (s *Server) exception(fr dlt.Frame, ctrl, code byte) []byte {
	out, _ := dlt.Build(fr.Addr, ctrl|dlt.BitReply|dlt.BitError, []byte{code})
	return out
}

// followChunk 是分帧时每段的数据字节数。
//
// 读数据应答的 L 上限 200，减去 4 字节 DI 得 196；读后续应答还要多带
// 一个帧序号，因此统一取 195，两种帧都在上限内。
const followChunk = dlt.MaxData - 5

// bcdLEWidth 把非负整数编码为**指定宽度**的 BCD，低字节在前。
func bcdLEWidth(n uint64, width int) []byte {
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		lo := byte(n % 10)
		n /= 10
		hi := byte(n % 10)
		n /= 10
		out[i] = hi<<4 | lo
	}
	return out
}

// bcdLE 把非负整数编码为定长 BCD（最少字节），低字节在前。
//
// 长度取「够装下这个数的最少字节数」，至少 1。测试与预置数据用；
// 协议路径上的宽度一律由 DI 表决定，不由数据决定。
func bcdLE(n uint64) []byte {
	if n == 0 {
		return []byte{0}
	}
	var out []byte
	for n > 0 {
		lo := byte(n % 10)
		n /= 10
		hi := byte(n % 10)
		n /= 10
		out = append(out, hi<<4|lo)
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
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF)
}

var _ = fmt.Sprintf
