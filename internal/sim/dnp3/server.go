// Package dnp3 提供了一个 DNP3（IEEE 1815）内存从站，用于无硬件自检与主站
// 驱动的单元测试。
//
// 从站复现的重点不是「完整的协议栈」，而是主站最容易搞错的几处：
//
//  1. **链路层有确认**。主站的复位/探活/确认用户数据都必须收到二级 ACK，
//     否则主站会在第一步就超时；从站主动上送的数据帧也应走确认用户数据，
//     让主站练习回 ACK。
//  2. **应用层也有确认**。带事件对象的响应要置 CON 位，主站必须回一条
//     application confirm；从站把未确认的事件留在 inflight 里，下一轮
//     重发。主站漏确认时，测试能直接看到 inflight 一直不清零。
//  3. **类 0 是整站快照**，类 1/2/3 是三个独立的事件队列。一次 READ 可以
//     同时请求四类，从站用一条响应回全部——总召唤就是这么做的。
//  4. **直接控制的响应会回显状态**。主站只写 socket 不检查回显时，拒绝
//     会被当成成功；从站对不存在的索引返回非零 status，正是为了抓这个。
//
// 与主站保持同一份边界：只做 TCP、类 0/1/2/3、CROB 与模拟量输出、直接
// 控制；不做串口、安全认证、文件传输、时间同步写路径、选择-执行（SBO）。
package dnp3

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	dnp3codec "uic/internal/codec/dnp3"
)

// Area 与主站 address.area 使用同一套名字，配置与自检输出不必再翻译一次。
type Area string

const (
	BinaryInput    Area = "binary-input"
	DoubleBitInput Area = "double-bit-input"
	BinaryOutput   Area = "binary-output"
	Counter        Area = "counter"
	AnalogInput    Area = "analog-input"
	AnalogOutput   Area = "analog-output"
)

var areaOrder = []Area{BinaryInput, DoubleBitInput, BinaryOutput, Counter, AnalogInput, AnalogOutput}

// Point 是一个从站数据点。
//
// 四个值字段按 Area 只使用其中一个：Bool 用于二进制量，Double 用于双点量
// （0..3），Counter 用于计数器，Num 用于模拟量。把它们放在同一个结构里
// 而不是 any，是为了让默认点表和测试的写法一眼能看出类型，不是为了省内存。
type Point struct {
	Area    Area
	Index   uint32
	Bool    bool
	Double  uint8
	Counter uint32
	Num     float64
	Flags   byte
	// Class 为 0 表示只参与类 0 静态值；1/2/3 表示值变化时进入对应事件队列。
	Class byte
}

// Event 是一条待上送的事件。
type Event struct {
	Area    Area
	Index   uint32
	Class   byte
	Flags   byte
	Bool    bool
	Double  uint8
	Counter uint32
	Num     float64
	At      time.Time
}

// DefaultLinkAddr 是从站默认的链路地址。
const DefaultLinkAddr uint16 = 1

// Server 是 DNP3 内存从站。
type Server struct {
	ln   net.Listener
	addr string
	log  *slog.Logger

	mu       sync.Mutex
	points   map[Area]map[uint32]*Point
	queue    []Event
	inflight map[byte][]Event
	conns    map[*station]bool
	auto     bool
	closed   chan struct{}
	once     sync.Once

	badFrames  uint64
	appReads   uint64
	appControl uint64
	confirms   uint64
}

// NewServer 启动从站。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dnp3 sim: listen %q: %w", addr, err)
	}
	s := &Server{
		ln:       ln,
		addr:     ln.Addr().String(),
		log:      slog.Default(),
		points:   map[Area]map[uint32]*Point{},
		inflight: map[byte][]Event{},
		conns:    map[*station]bool{},
		auto:     true,
		closed:   make(chan struct{}),
	}
	s.installDefaults()
	go s.accept()
	go s.tick()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.addr }

// Close 停止监听并断开所有连接。可重复调用。
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.closed)
		err = s.ln.Close()
		s.mu.Lock()
		for st := range s.conns {
			_ = st.nc.Close()
		}
		s.mu.Unlock()
	})
	return err
}

// SetAutoChange 控制默认点表是否每秒变化（二进制点 0 翻转、计数器 0 自增）。
func (s *Server) SetAutoChange(v bool) {
	s.mu.Lock()
	s.auto = v
	s.mu.Unlock()
}

// BadFrames 返回无法解析的链路帧数。
func (s *Server) BadFrames() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badFrames
}

// Stats 返回应用层计数，供测试断言「主站确实回了确认」。
type Stats struct {
	Reads    uint64
	Controls uint64
	Confirms uint64
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Reads: s.appReads, Controls: s.appControl, Confirms: s.confirms}
}

// Inflight 返回尚未被主站确认的事件条数。
func (s *Server) Inflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, evs := range s.inflight {
		n += len(evs)
	}
	return n
}

// Pending 返回尚未上送的事件条数。
func (s *Server) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *Server) installDefaults() {
	for _, p := range []Point{
		{Area: BinaryInput, Index: 0, Bool: true, Flags: dnp3codec.FlagOnline, Class: 1},
		{Area: BinaryInput, Index: 1, Bool: false, Flags: dnp3codec.FlagOnline, Class: 1},
		{Area: BinaryInput, Index: 2, Bool: true, Flags: dnp3codec.FlagOnline},
		{Area: BinaryInput, Index: 3, Bool: false, Flags: dnp3codec.FlagOnline},
		{Area: DoubleBitInput, Index: 0, Double: 1, Flags: dnp3codec.FlagOnline, Class: 1},
		{Area: DoubleBitInput, Index: 1, Double: 2, Flags: dnp3codec.FlagOnline},
		{Area: BinaryOutput, Index: 0, Bool: false, Flags: dnp3codec.FlagOnline, Class: 1},
		{Area: BinaryOutput, Index: 1, Bool: false, Flags: dnp3codec.FlagOnline, Class: 1},
		{Area: Counter, Index: 0, Counter: 1000, Flags: dnp3codec.FlagOnline, Class: 3},
		{Area: Counter, Index: 1, Counter: 42, Flags: dnp3codec.FlagOnline},
		{Area: AnalogInput, Index: 0, Num: 42.5, Flags: dnp3codec.FlagOnline, Class: 2},
		{Area: AnalogInput, Index: 1, Num: 7.25, Flags: dnp3codec.FlagOnline, Class: 2},
		{Area: AnalogInput, Index: 2, Num: -3.5, Flags: dnp3codec.FlagOnline, Class: 2},
		{Area: AnalogInput, Index: 3, Num: 1234, Flags: dnp3codec.FlagOnline},
		{Area: AnalogOutput, Index: 0, Num: 0, Flags: dnp3codec.FlagOnline},
	} {
		s.SetPoint(p)
	}
}

// SetPoint 新增或覆盖一个点。覆盖不会自动产生事件——事件只由 SetValue
// 与直接控制产生，避免「改点表」被误当成「设备值变化」。
func (s *Server) SetPoint(p Point) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.points[p.Area]
	if m == nil {
		m = map[uint32]*Point{}
		s.points[p.Area] = m
	}
	cp := p
	m[p.Index] = &cp
}

// SetValue 修改一个点的值；值发生变化且 Class>0 时入事件队列。
func (s *Server) SetValue(area Area, index uint32, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.points[area][index]
	if p == nil {
		return fmt.Errorf("dnp3 sim: %s[%d] not configured", area, index)
	}
	changed := false
	switch area {
	case BinaryInput, BinaryOutput:
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("dnp3 sim: %s[%d] needs bool, got %T", area, index, v)
		}
		changed = p.Bool != b
		p.Bool = b
	case DoubleBitInput:
		var d uint8
		switch x := v.(type) {
		case uint8:
			d = x
		case int:
			d = uint8(x)
		default:
			return fmt.Errorf("dnp3 sim: %s[%d] needs uint8/int, got %T", area, index, v)
		}
		d &= 0x03
		changed = p.Double != d
		p.Double = d
	case Counter:
		n, ok := numericToUint32(v)
		if !ok {
			return fmt.Errorf("dnp3 sim: %s[%d] needs an integer, got %T", area, index, v)
		}
		changed = p.Counter != n
		p.Counter = n
	case AnalogInput, AnalogOutput:
		n, ok := numericToFloat(v)
		if !ok {
			return fmt.Errorf("dnp3 sim: %s[%d] needs a number, got %T", area, index, v)
		}
		changed = p.Num != n
		p.Num = n
	default:
		return fmt.Errorf("dnp3 sim: unknown area %q", area)
	}
	if changed {
		s.pushEventLocked(*p)
	}
	return nil
}

// SetFlags 修改一个点的品质位。传 0 表示离线（ONLINE 位为 0）。
// 品质变化在真实设备上也是一次状态变化，会给带事件类的点补一条事件。
func (s *Server) SetFlags(area Area, index uint32, flags byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.points[area][index]
	if p == nil {
		return fmt.Errorf("dnp3 sim: %s[%d] not configured", area, index)
	}
	if p.Flags == flags {
		return nil
	}
	p.Flags = flags
	s.pushEventLocked(*p)
	return nil
}

// Value 返回点副本。
func (s *Server) Value(area Area, index uint32) (Point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.points[area][index]
	if p == nil {
		return Point{}, false
	}
	return *p, true
}

// Points 返回按区与索引排序的点表副本。
func (s *Server) Points() []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Point
	for _, a := range areaOrder {
		idx := make([]uint32, 0, len(s.points[a]))
		for i := range s.points[a] {
			idx = append(idx, i)
		}
		sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
		for _, i := range idx {
			out = append(out, *s.points[a][i])
		}
	}
	return out
}

func numericToUint32(v any) (uint32, bool) {
	switch x := v.(type) {
	case uint32:
		return x, true
	case uint16:
		return uint32(x), true
	case int:
		if x < 0 {
			return 0, false
		}
		return uint32(x), true
	case int64:
		if x < 0 {
			return 0, false
		}
		return uint32(x), true
	case float64:
		if x < 0 {
			return 0, false
		}
		return uint32(x), true
	}
	return 0, false
}

func numericToFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint16:
		return float64(x), true
	}
	return 0, false
}

// pushEventLocked 把当前值压入事件队列；状态位在 flags 的高位里。
func (s *Server) pushEventLocked(p Point) {
	if p.Class == 0 || p.Class > 3 {
		return
	}
	f := p.Flags
	switch p.Area {
	case BinaryInput, BinaryOutput:
		f &^= dnp3codec.FlagState
		if p.Bool {
			f |= dnp3codec.FlagState
		}
	case DoubleBitInput:
		f &^= 0xC0
		f |= (p.Double & 0x03) << 6
	}
	s.queue = append(s.queue, Event{
		Area: p.Area, Index: p.Index, Class: p.Class, Flags: f,
		Bool: p.Bool, Double: p.Double, Counter: p.Counter, Num: p.Num,
		At: time.Now().UTC(),
	})
	// 真实从站的事件缓冲区也是有限的；这里给一个足够大的上限，防止一个
	// 不轮询的主站把测试进程的内存吃光。
	if len(s.queue) > 8192 {
		s.queue = s.queue[len(s.queue)-8192:]
	}
}

func (s *Server) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			s.mu.Lock()
			if !s.auto {
				s.mu.Unlock()
				continue
			}
			if p := s.points[BinaryInput][0]; p != nil {
				p.Bool = !p.Bool
				s.pushEventLocked(*p)
			}
			if p := s.points[Counter][0]; p != nil {
				p.Counter++
				s.pushEventLocked(*p)
			}
			s.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type station struct {
	srv *Server
	nc  net.Conn

	// 由收到的帧更新：从站发出的帧把方向反过来。
	mu   sync.Mutex
	dest uint16
	src  uint16

	wmu  sync.Mutex
	tseq uint8
	re   dnp3codec.Reassembler
}

func (s *Server) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		st := &station{srv: s, nc: nc, dest: 0, src: DefaultLinkAddr}
		s.mu.Lock()
		s.conns[st] = true
		s.mu.Unlock()
		go st.serve()
	}
}

func (st *station) serve() {
	defer func() {
		_ = st.nc.Close()
		st.srv.mu.Lock()
		delete(st.srv.conns, st)
		st.srv.mu.Unlock()
	}()

	var dec dnp3codec.Decoder
	buf := make([]byte, 4096)
	for {
		n, err := st.nc.Read(buf)
		if n > 0 {
			dec.Feed(buf[:n])
			for {
				f, ok, derr := dec.Next()
				if derr != nil {
					st.srv.mu.Lock()
					st.srv.badFrames++
					st.srv.mu.Unlock()
					continue
				}
				if !ok {
					break
				}
				st.handleFrame(f)
			}
		}
		if err != nil {
			return
		}
	}
}

func (st *station) handleFrame(f dnp3codec.Frame) {
	st.mu.Lock()
	st.dest, st.src = f.Source, f.Dest
	st.mu.Unlock()

	if !f.Primary() {
		// 主站对从站确认用户数据的链路 ACK。没有额外状态要维护。
		return
	}
	switch f.Function() {
	case dnp3codec.FuncResetLink:
		st.writeLink(dnp3codec.FuncAck, nil)
		st.re = dnp3codec.Reassembler{}
	case dnp3codec.FuncTestLink:
		st.writeLink(dnp3codec.FuncAck, nil)
	case dnp3codec.FuncRequestLinkStatus:
		st.writeLink(dnp3codec.FuncLinkStatus, nil)
	case dnp3codec.FuncConfirmedUserData:
		// 先确认链路帧，再处理应用报文：主站的链路层可能等这个 ACK
		// 才继续发下一帧。
		st.writeLink(dnp3codec.FuncAck, nil)
		st.handleApp(f.Data)
	case dnp3codec.FuncUnconfirmedData:
		st.handleApp(f.Data)
	}
}

func (st *station) writeLink(control byte, data []byte) {
	st.mu.Lock()
	dest, src := st.dest, st.src
	st.mu.Unlock()
	raw, err := dnp3codec.EncodeFrame(control, dest, src, data)
	if err != nil {
		return
	}
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_ = st.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = st.nc.Write(raw)
}

func (st *station) sendApp(payload []byte) {
	chunks := dnp3codec.EncodeTransport(payload, st.tseq)
	st.wmu.Lock()
	st.tseq = (st.tseq + uint8(len(chunks))) & dnp3codec.TransportSeq
	st.wmu.Unlock()
	for _, ch := range chunks {
		st.writeLink(dnp3codec.ControlPrm|dnp3codec.FuncConfirmedUserData, ch)
	}
}

func (st *station) handleApp(data []byte) {
	if len(data) < 1 {
		return
	}
	h := data[0]
	app, done, err := st.re.Push(h&dnp3codec.TransportFIR != 0, h&dnp3codec.TransportFIN != 0,
		h&dnp3codec.TransportSeq, data[1:])
	if err != nil || !done {
		return
	}
	m, err := dnp3codec.DecodeApp(app)
	if err != nil {
		return
	}
	switch m.Function {
	case dnp3codec.AppRead:
		st.srv.mu.Lock()
		st.srv.appReads++
		st.srv.mu.Unlock()
		st.handleRead(m)
	case dnp3codec.AppDirectOperate:
		st.srv.mu.Lock()
		st.srv.appControl++
		st.srv.mu.Unlock()
		st.handleDirectOperate(m)
	case dnp3codec.AppSelect, dnp3codec.AppOperate:
		st.respond(m, dnp3codec.IINFuncNotSupport, nil, false)
	case dnp3codec.AppConfirm:
		st.srv.mu.Lock()
		st.srv.confirms++
		st.srv.inflight = map[byte][]Event{}
		st.srv.mu.Unlock()
	default:
		st.respond(m, dnp3codec.IINFuncNotSupport, nil, false)
	}
}

func (st *station) handleRead(m dnp3codec.AppMessage) {
	headers, err := dnp3codec.ParseRequestHeaders(m.Objects)
	if err != nil {
		st.respond(m, dnp3codec.IINParamError, nil, false)
		return
	}
	st.srv.mu.Lock()
	objects, hasEvents := st.srv.buildRead(headers)
	st.srv.mu.Unlock()
	st.respond(m, 0, objects, hasEvents)
}

func (st *station) respond(req dnp3codec.AppMessage, iin uint16, objects []byte, con bool) {
	ctrl := dnp3codec.AppControl{FIR: true, FIN: true, CON: con, Seq: req.Control.Seq}
	st.sendApp(dnp3codec.EncodeResponse(ctrl, dnp3codec.AppResponse, iin, objects))
}

// buildRead 按请求头生成对象区；返回的第二个值表示是否带事件（需要 CON）。
func (s *Server) buildRead(headers []dnp3codec.Header) ([]byte, bool) {
	var out []byte
	hasEvents := false
	for _, h := range headers {
		switch h.Group {
		case dnp3codec.GroupClass:
			// 类对象的变体 = 类号 + 1（类 0 是变体 1）。
			class := h.Variation
			if class == 1 {
				out = append(out, s.encodeStaticAll()...)
				continue
			}
			if class < 2 || class > 4 {
				continue
			}
			if evs := s.takeEvents(class - 1); len(evs) > 0 {
				out = append(out, s.encodeEvents(evs)...)
				hasEvents = true
			}
		case dnp3codec.GroupBinaryInput, dnp3codec.GroupDoubleBitInput, dnp3codec.GroupBinaryOutput,
			dnp3codec.GroupCounter, dnp3codec.GroupAnalogInput, dnp3codec.GroupAnalogOutput:
			out = append(out, s.encodeStaticGroup(h.Group)...)
		}
	}
	return out, hasEvents
}

// takeEvents 取某一类的事件。未确认的事件优先重发；主站确认后 inflight 被
// 清空，下一轮才取新事件。
func (s *Server) takeEvents(class byte) []Event {
	if evs := s.inflight[class]; len(evs) > 0 {
		return evs
	}
	var evs, keep []Event
	for _, e := range s.queue {
		if e.Class == class {
			evs = append(evs, e)
		} else {
			keep = append(keep, e)
		}
	}
	s.queue = keep
	if len(evs) > 0 {
		s.inflight[class] = evs
	}
	return evs
}

func (s *Server) encodeStaticAll() []byte {
	var out []byte
	for _, a := range areaOrder {
		out = append(out, s.encodeArea(a)...)
	}
	return out
}

func (s *Server) encodeStaticGroup(g byte) []byte {
	switch g {
	case dnp3codec.GroupBinaryInput:
		return s.encodeArea(BinaryInput)
	case dnp3codec.GroupDoubleBitInput:
		return s.encodeArea(DoubleBitInput)
	case dnp3codec.GroupBinaryOutput:
		return s.encodeArea(BinaryOutput)
	case dnp3codec.GroupCounter:
		return s.encodeArea(Counter)
	case dnp3codec.GroupAnalogInput:
		return s.encodeArea(AnalogInput)
	case dnp3codec.GroupAnalogOutput:
		return s.encodeArea(AnalogOutput)
	}
	return nil
}

func (s *Server) encodeArea(a Area) []byte {
	m := s.points[a]
	if len(m) == 0 {
		return nil
	}
	var max uint32
	for idx := range m {
		if idx > max {
			max = idx
		}
	}
	var g, v byte
	switch a {
	case BinaryInput:
		g, v = dnp3codec.GroupBinaryInput, 2
	case DoubleBitInput:
		g, v = dnp3codec.GroupDoubleBitInput, 2
	case BinaryOutput:
		g, v = dnp3codec.GroupBinaryOutput, 2
	case Counter:
		g, v = dnp3codec.GroupCounter, 1
	case AnalogInput:
		g, v = dnp3codec.GroupAnalogInput, 5
	case AnalogOutput:
		g, v = dnp3codec.GroupAnalogOutput, 3
	default:
		return nil
	}
	q := byte(dnp3codec.QualStartStop8)
	if max > 0xFF {
		q = dnp3codec.QualStartStop16
	}
	hdr, err := dnp3codec.EncodeHeader(g, v, q, 0, max)
	if err != nil {
		return nil
	}
	out := append([]byte(nil), hdr...)
	if v == 2 {
		// 变体 2：一个点一个「状态 + 品质」字节，没有独立的位图。状态位
		// 必须在 flags 里，否则主站读到的是「离线/未知」而不是真实状态。
		for i := uint32(0); i <= max; i++ {
			var f byte
			if p := m[i]; p != nil {
				f = p.Flags
				switch a {
				case BinaryInput, BinaryOutput:
					f &^= dnp3codec.FlagState
					if p.Bool {
						f |= dnp3codec.FlagState
					}
				case DoubleBitInput:
					f &^= 0xC0
					f |= (p.Double & 0x03) << 6
				}
			}
			out = append(out, f)
		}
		return out
	}
	switch a {
	case BinaryInput, BinaryOutput:
		// 变体 1：纯位图，没有 flags 字节。
		bits := make([]byte, (int(max)+1+7)/8)
		for i := uint32(0); i <= max; i++ {
			if p := m[i]; p != nil && p.Bool {
				bits[i/8] |= 1 << (i % 8)
			}
		}
		out = append(out, bits...)
	case DoubleBitInput:
		bits := make([]byte, ((int(max)+1)*2+7)/8)
		for i := uint32(0); i <= max; i++ {
			var d uint8
			if p := m[i]; p != nil {
				d = p.Double & 0x03
			}
			bits[i/4] |= d << (2 * (i % 4))
		}
		out = append(out, bits...)
	case Counter:
		// 计数器与模拟量是「每个对象一个 flags 字节 + 值」，不是先把
		// 所有 flags 排完再排所有值。两种写法只差一个字节的错位，解析
		// 出来的数字却仍然「像」合法值，极难靠肉眼发现。
		for i := uint32(0); i <= max; i++ {
			var f byte
			var n uint32
			if p := m[i]; p != nil {
				f, n = p.Flags, p.Counter
			}
			out = append(out, f)
			out = binary.LittleEndian.AppendUint32(out, n)
		}
	case AnalogInput, AnalogOutput:
		for i := uint32(0); i <= max; i++ {
			var f byte
			var n float64
			if p := m[i]; p != nil {
				f, n = p.Flags, p.Num
			}
			out = append(out, f)
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(n)))
		}
	}
	return out
}

func (s *Server) encodeEvents(evs []Event) []byte {
	byArea := map[Area][]Event{}
	for _, e := range evs {
		byArea[e.Area] = append(byArea[e.Area], e)
	}
	var out []byte
	for _, a := range areaOrder {
		list := byArea[a]
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
		if len(list) > 255 {
			list = list[:255]
		}
		var g, v byte
		switch a {
		case BinaryInput:
			g, v = dnp3codec.GroupBinaryEvent, 2
		case DoubleBitInput:
			g, v = dnp3codec.GroupDoubleBitEvent, 2
		case BinaryOutput:
			g, v = dnp3codec.GroupBinaryOutputEvent, 2
		case Counter:
			g, v = dnp3codec.GroupCounterEvent, 1
		case AnalogInput:
			g, v = dnp3codec.GroupAnalogEvent, 5
		default:
			continue // 模拟量输出事件（组 42）本实现不编码，走静态刷新
		}
		hdr, err := dnp3codec.EncodeHeader(g, v, dnp3codec.QualIndex8, 0, uint32(len(list)))
		if err != nil {
			continue
		}
		out = append(out, hdr...)
		for _, e := range list {
			out = append(out, byte(e.Index), e.Flags)
			switch a {
			case Counter:
				out = binary.LittleEndian.AppendUint32(out, e.Counter)
			case AnalogInput:
				out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(e.Num)))
			}
			if v == 2 {
				out = append(out, dnp3codec.EncodeTime48(e.At)...)
			}
		}
	}
	return out
}

// handleDirectOperate 应用控制并回显结果。
func (st *station) handleDirectOperate(m dnp3codec.AppMessage) {
	objs, err := dnp3codec.ParseObjects(m.Objects)
	if err != nil {
		st.respond(m, dnp3codec.IINParamError, nil, false)
		return
	}
	var echo []byte
	for _, o := range objs {
		switch o.Group {
		case dnp3codec.GroupCROB:
			c, ok := o.Value.(dnp3codec.CROB)
			if !ok {
				continue
			}
			status := st.srv.applyCROB(o.Index, c)
			raw, err := dnp3codec.CROBObjects(o.Index, c.Code, c.Count, c.OnTime, c.OffTime)
			if err != nil {
				continue
			}
			raw[len(raw)-1] = status
			echo = append(echo, raw...)
		case dnp3codec.GroupAnalogOutputBlock:
			n, ok := o.Value.(float64)
			if !ok {
				if iv, ok2 := o.Value.(int64); ok2 {
					n, ok = float64(iv), true
				}
			}
			if !ok {
				continue
			}
			variation := o.Variation
			if variation < 1 || variation > 4 {
				variation = 1
			}
			st.srv.applyAnalog(o.Index, n)
			raw, err := dnp3codec.AnalogOutputObjects(o.Index, variation, n)
			if err != nil {
				continue
			}
			echo = append(echo, raw...)
		}
	}
	st.respond(m, 0, echo, false)
}

// CROB 的返回状态码，沿用标准定义。
const (
	CROBSuccess       byte = 0
	CROBTimeout       byte = 1
	CROBNoSelect      byte = 2
	CROBFormatError   byte = 3
	CROBNotSupported  byte = 4
	CROBAlreadyActive byte = 5
	CROBHardwareError byte = 6
	CROBLocalControl  byte = 7
	CROBTooManyOps    byte = 8
	CROBNotAuthorized byte = 9
)

func (s *Server) applyCROB(index uint32, c dnp3codec.CROB) byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.points[BinaryOutput][index]
	if p == nil {
		return CROBNotSupported
	}
	switch c.Code {
	case dnp3codec.CROBPulseOn, dnp3codec.CROBLatchOn:
		p.Bool = true
	case dnp3codec.CROBPulseOff, dnp3codec.CROBLatchOff:
		p.Bool = false
	case 0x00: // NUL：只回状态，不改变输出
		return CROBSuccess
	default:
		return CROBFormatError
	}
	s.pushEventLocked(*p)
	return CROBSuccess
}

func (s *Server) applyAnalog(index uint32, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.points[AnalogOutput][index]
	if p == nil {
		return
	}
	p.Num = v
}
