// Package iec101 提供串行总线非平衡式远动从站的内存实现（FT1.2 帧），
// 用 TCP 透传承载，供无硬件自检与自动化测试。
//
// 从站要复现的不是「一整套协议栈」，而是主站最容易搞错的五处：
//
//  1. **从站不能主动开口**。数据必须先被主站「点名」才送出去；把应答
//     直接写回链路，在真实 485 上就是两台设备抢总线。
//  2. **帧计数位（FCB）去重**。主站重传时 FCB 不变，从站必须识别成重复
//     帧：只回链路确认，**不再执行一次命令**。少了这一步，「确认丢失 →
//     主站重发 → 开关被拉合两次」。
//  3. **访问需求位（ACD）**。还有数据没送完时，每一帧都要置位；主站靠
//     它决定「继续取」还是「收工」。
//  4. **总召唤是一份快照 + 一条终止报文**。数据送完必须补一条终止，
//     否则主站只能靠超时判断结束。
//  5. **命令是两段确认**。链路确认只表示报文到了；应用层还要回一条
//     激活确认（成功）或否定确认（未知 IOA / 不可写）。
//
// 刻意不做的有：平衡式（从站可主动发送）、文件传输、时钟同步的实际生效、
// 带时标的命令、多级数据优先级队列。主站也只做这些，两边保持同一份边界。
package iec101

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"uic/internal/codec/iec101"
	"uic/internal/codec/iec60870"
	"uic/internal/transport"
)

// 从站默认的链路参数，与驱动的默认值一致：1 字节链路地址、1 字节公共
// 地址、2 字节信息对象地址、无源发地址。
const (
	DefaultLinkAddr     uint16 = 1
	DefaultLinkAddrSize        = 1
	DefaultCommonAddr   uint16 = 1
)

// InfoKind 是信息对象的类别，决定用哪种 ASDU 类型上送。
type InfoKind int

const (
	KindSingle  InfoKind = iota // 单点信息（bool）
	KindDouble                  // 双点信息（0..3）
	KindFloat                   // 短浮点测量值
	KindScaled                  // 标度化测量值（int16）
	KindCounter                 // 累计量（uint32）
)

// Info 是一个信息对象。
type Info struct {
	IOA     uint32
	Kind    InfoKind
	Bool    bool
	Num     float64
	Counter uint32
	Quality iec60870.Quality
	// Time 非 nil 时用带时标的类型上送。
	Time *time.Time

	// Writable 标记该对象接受命令。
	Writable bool

	// Command 表示这是一个**只在控制方向存在**的对象：它的 IOA 不参与
	// 总召唤，也不会自发上送。真实设备里遥控号与遥信号是两套地址，把
	// 两者混成一个标志，会让总召唤凭空多出一批「点位」。
	Command bool
}

// Server 是内存从站。
type Server struct {
	ln   net.Listener
	log  *slog.Logger
	addr string

	mu    sync.Mutex
	info  map[uint32]*Info
	ca    uint16
	link  uint16
	lsize int
	prof  iec60870.Profile
	// noDataFixed 决定「没有数据」时回固定帧（FC=9）还是单字节 E5。
	// 两种应答现场都见得到，做成开关是为了让两条解析路径都被走到。
	noDataFixed bool
	noDefaults  bool
	autoIncr    bool

	conns     map[*station]struct{}
	badFrames uint64
	writes    int64
	closed    chan struct{}
	closeOnce sync.Once
}

// Option 调整从站参数。
type Option func(*Server)

// WithCommonAddr 设置公共地址。
func WithCommonAddr(ca uint16) Option {
	return func(s *Server) { s.ca = ca }
}

// WithLinkAddress 设置链路地址与宽度（0..2 字节）。
func WithLinkAddress(addr uint16, size int) Option {
	return func(s *Server) { s.link, s.lsize = addr, size }
}

// WithProfile 设置 ASDU 头部宽度。
func WithProfile(p iec60870.Profile) Option {
	return func(s *Server) { s.prof = p }
}

// WithNoDataFixed 让「没有数据」的应答使用固定帧而不是单字节确认。
func WithNoDataFixed(b bool) Option {
	return func(s *Server) { s.noDataFixed = b }
}

// WithNoDefaults 不装入默认对象。
//
// 默认对象集的 IOA 是三位数以上；用小地址剖面（例如 1 字节 IOA）测试时
// 它们根本编码不出去，从站会在组帧时失败并断开连接——症状是主站看到
// 一个莫名其妙的 EOF。这类测试应当自己 Add 一批落在地址空间内的对象。
func WithNoDefaults(b bool) Option {
	return func(s *Server) { s.noDefaults = b }
}

// WithAutoIncrement 让累计量每秒自增并自发上送，便于观察长连接。
func WithAutoIncrement(b bool) Option {
	return func(s *Server) { s.autoIncr = b }
}

// NewServer 启动从站。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string, opts ...Option) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	s := &Server{
		log:    slog.Default(),
		ca:     DefaultCommonAddr,
		link:   DefaultLinkAddr,
		lsize:  DefaultLinkAddrSize,
		prof:   iec60870.Profile{OrgSize: 0, CASize: 1, IOASize: 2},
		info:   map[uint32]*Info{},
		conns:  map[*station]struct{}{},
		closed: make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if err := s.prof.Validate(); err != nil {
		return nil, fmt.Errorf("iec101 sim: %w", err)
	}
	if s.lsize < 0 || s.lsize > 2 {
		return nil, fmt.Errorf("iec101 sim: link address size %d must be 0..2", s.lsize)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("iec101 sim: listen %q: %w", addr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	if !s.noDefaults {
		s.installDefaults()
	}
	go s.accept()
	if s.autoIncr {
		go s.tick()
	}
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.addr }

// Close 停止监听并断开所有连接。幂等。
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.ln.Close()
		s.mu.Lock()
		for st := range s.conns {
			_ = st.st.Close()
		}
		s.mu.Unlock()
	})
	return err
}

// SetCommonAddr 设置本站的公共地址。
func (s *Server) SetCommonAddr(ca uint16) {
	s.mu.Lock()
	s.ca = ca
	s.mu.Unlock()
}

// BadFrames 返回帧解析失败或公共地址不符的次数。
func (s *Server) BadFrames() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badFrames
}

// Writes 返回实际执行的写命令条数（重复帧不计）。
func (s *Server) Writes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// installDefaults 装入一套覆盖各类型的对象，与 104 内置从站的 IOA 对齐，
// 便于对照两种承载的读数。
func (s *Server) installDefaults() {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	for _, i := range []*Info{
		{IOA: 1001, Kind: KindFloat, Num: 42.5},
		{IOA: 1002, Kind: KindFloat, Num: 7.25},
		{IOA: 1003, Kind: KindScaled, Num: 1234},
		{IOA: 1004, Kind: KindFloat, Num: 0.5, Quality: iec60870.QSubstituted},
		{IOA: 1005, Kind: KindFloat, Num: -3.5, Time: &ts},
		{IOA: 2001, Kind: KindSingle, Bool: true},
		{IOA: 2002, Kind: KindSingle, Bool: false},
		{IOA: 2003, Kind: KindDouble, Num: 2, Writable: true},
		{IOA: 2004, Kind: KindDouble, Num: 1},
		{IOA: 3001, Kind: KindCounter, Counter: 1000},
		{IOA: 4001, Kind: KindSingle, Bool: false, Writable: true, Command: true},
		{IOA: 4002, Kind: KindFloat, Num: 0, Writable: true, Command: true},
		{IOA: 4003, Kind: KindScaled, Num: 0, Writable: true, Command: true},
	} {
		s.info[i.IOA] = i
	}
}

// SetValue 直接写入一个对象，并把变化放进第 1 级队列（自发上送）。
func (s *Server) SetValue(ioa uint32, v Info) error {
	s.mu.Lock()
	cur, ok := s.info[ioa]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("iec101 sim: IOA %d not configured", ioa)
	}
	next := v
	if next.Quality == 0 {
		next.Quality = cur.Quality
	}
	if next.Kind == 0 && cur.Kind != 0 {
		next.Kind = cur.Kind
	}
	// Command / Writable 描述的是对象的**身份**，不是它当前的值：调用方
	// 用 SetValue 改一个值时，不该顺手把一个「只在控制方向存在」的对象
	// 变成监视对象。那样一来，遥控号会突然出现在总召唤里——主站看到的是
	// 一个凭空多出来的点位，而且它的值还是上一条命令的残值。
	if !next.Command {
		next.Command = cur.Command
	}
	if !next.Writable {
		next.Writable = cur.Writable
	}
	*s.info[ioa] = next
	snapshot := next
	ca := s.ca
	stations := make([]*station, 0, len(s.conns))
	for st := range s.conns {
		stations = append(stations, st)
	}
	s.mu.Unlock()

	// 纯命令对象在监视方向不存在，不能自发上送。
	if snapshot.Command {
		return nil
	}
	a := infoASDU(snapshot, iec60870.CauseSpontaneous, ca)
	for _, st := range stations {
		st.enqueue1(a)
	}
	return nil
}

// SetBool 设置一个单点/双点对象的布尔状态。
func (s *Server) SetBool(ioa uint32, v bool) error {
	s.mu.Lock()
	cur, ok := s.info[ioa]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("iec101 sim: IOA %d not configured", ioa)
	}
	next := *cur
	next.Bool = v
	if next.Kind == KindDouble {
		if v {
			next.Num = 2
		} else {
			next.Num = 1
		}
	}
	return s.SetValue(ioa, next)
}

// SetFloat 设置一个测量值。
func (s *Server) SetFloat(ioa uint32, v float64) error {
	s.mu.Lock()
	cur, ok := s.info[ioa]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("iec101 sim: IOA %d not configured", ioa)
	}
	next := *cur
	next.Num = v
	return s.SetValue(ioa, next)
}

// SetCounter 设置一个累计量。
func (s *Server) SetCounter(ioa uint32, v uint32) error {
	s.mu.Lock()
	cur, ok := s.info[ioa]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("iec101 sim: IOA %d not configured", ioa)
	}
	next := *cur
	next.Counter = v
	return s.SetValue(ioa, next)
}

// Add 增加（或替换）一个信息对象。
//
// 比 SetValue 更基础：SetValue 只能改已存在的对象，Add 用于测试里构造一套
// 尺寸受控的对象集。
func (s *Server) Add(i Info) {
	s.mu.Lock()
	cp := i
	s.info[i.IOA] = &cp
	s.mu.Unlock()
}

// Info 返回一个对象的当前值副本。
func (s *Server) Info(ioa uint32) (Info, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.info[ioa]
	if !ok {
		return Info{}, false
	}
	return *i, true
}

func (s *Server) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
		}
		s.mu.Lock()
		var targets []Info
		for _, i := range s.info {
			if i.Kind == KindCounter {
				i.Counter++
				targets = append(targets, *i)
			}
		}
		ca := s.ca
		stations := make([]*station, 0, len(s.conns))
		for st := range s.conns {
			stations = append(stations, st)
		}
		s.mu.Unlock()
		for _, i := range targets {
			a := infoASDU(i, iec60870.CauseSpontaneous, ca)
			for _, st := range stations {
				st.enqueue1(a)
			}
		}
	}
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		closed := s.isClosedLocked()
		s.mu.Unlock()
		if closed {
			_ = c.Close()
			return
		}
		go func() {
			// ServeStream 负责把这条连接登记成 station 并清理；accept
			// 这一层不再重复登记，否则「自发上送」会写到一个没人读的
			// station 对象上——症状是「总召唤有数据、事件永远不来」。
			defer c.Close()
			_ = s.ServeStream(&tcpStream{conn: c})
		}()
	}
}

func (s *Server) isClosedLocked() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// ServeStream 在一条字节流上跑从站循环。串口/pty 与 TCP 共用它。
func (s *Server) ServeStream(stream transport.Stream) error {
	st := &station{server: s, st: stream}
	s.mu.Lock()
	s.conns[st] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, st)
		s.mu.Unlock()
	}()

	rd := iec101.NewReader(stream, s.lsize)
	for {
		fr, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || isClosed(err) {
				return nil
			}
			return err
		}
		reply, err := st.handle(fr)
		if err != nil {
			return err
		}
		for _, b := range reply {
			if len(b) == 0 {
				continue
			}
			if err := stream.Write(b); err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 单条连接上的从站状态
// ---------------------------------------------------------------------------

type station struct {
	server *Server
	st     transport.Stream

	mu sync.Mutex
	// q1 / q2 是待上送的数据，分别对应主站的「请求第 1 级 / 第 2 级」。
	q1 []iec60870.ASDU
	q2 []iec60870.ASDU

	// lastFCB 记住最近一次被接受的主站帧计数位；nil 表示还没收到过。
	lastFCB *bool
	// dups 统计被识别为重复帧的次数，供测试断言「重传没有重复执行」。
	dups int64
}

func (st *station) enqueue1(a iec60870.ASDU) {
	st.mu.Lock()
	st.q1 = append(st.q1, a)
	st.mu.Unlock()
}

func (st *station) pending1() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.q1) > 0
}

// handle 处理一个来自主站的帧，返回要写回链路的字节（可能为空）。
func (st *station) handle(fr iec101.Frame) ([][]byte, error) {
	switch fr.Kind {
	case iec101.KindVariable:
		return st.handleUser(fr)
	case iec101.KindFixed:
		return st.handleFixed(fr)
	}
	// 单字节帧只应从从站发出；主站发来的一律丢弃。
	return nil, nil
}

func (st *station) handleFixed(fr iec101.Frame) ([][]byte, error) {
	if !fr.Ctrl.PRM {
		return nil, nil // 从动站不应发出固定帧请求
	}
	switch fr.Ctrl.Func {
	case iec101.FCResetLink:
		st.mu.Lock()
		st.q1 = nil
		st.q2 = nil
		st.lastFCB = nil
		st.mu.Unlock()
		return st.fixedReply(iec101.FCAck, false)
	case iec101.FCResetProcess:
		return st.fixedReply(iec101.FCAck, false)
	case iec101.FCRequestLinkStatus:
		return st.fixedReply(iec101.FCLinkStatus, false)
	case iec101.FCRequestAccess:
		return st.fixedReply(iec101.FCAck, st.pending1())
	case iec101.FCRequestClass1:
		return st.pop(1)
	case iec101.FCRequestClass2:
		return st.pop(2)
	}
	// 不认识的功能码：回否定确认，让主站立刻看到「设备不支持这个动作」。
	return st.fixedReply(iec101.FCNack, st.pending1())
}

func (st *station) handleUser(fr iec101.Frame) ([][]byte, error) {
	if !fr.Ctrl.PRM {
		return nil, nil
	}
	if fr.Ctrl.Func != iec101.FCSendConfirm && fr.Ctrl.Func != iec101.FCSendNoReply {
		return st.fixedReply(iec101.FCNack, st.pending1())
	}

	dup := false
	if fr.Ctrl.FCV {
		st.mu.Lock()
		if st.lastFCB != nil && *st.lastFCB == fr.Ctrl.FCB {
			dup = true
			st.dups++
		} else {
			fcb := fr.Ctrl.FCB
			st.lastFCB = &fcb
		}
		st.mu.Unlock()
	}
	if !dup {
		if err := st.apply(fr.Payload); err != nil {
			return nil, err
		}
	}
	// 重复帧同样要回确认：主站重传的原因正是「没收到确认」。回别的东西
	// 会让它以为这一帧被拒，从而放弃重传。
	if fr.Ctrl.Func == iec101.FCSendNoReply {
		return nil, nil
	}
	return st.fixedReply(iec101.FCAck, st.pending1())
}

// apply 处理主站发来的 ASDU。
func (st *station) apply(payload []byte) error {
	s := st.server
	s.mu.Lock()
	a, err := iec60870.ParseASDUProfile(payload, s.prof)
	ca := s.ca
	s.mu.Unlock()
	if err != nil {
		s.mu.Lock()
		s.badFrames++
		s.mu.Unlock()
		return nil // 坏报文静默丢弃：真实装置不会为一条解析不了的帧回话
	}
	if a.CommonAddr != ca {
		s.mu.Lock()
		s.badFrames++
		s.mu.Unlock()
		return nil
	}

	switch a.TypeID {
	case iec60870.TypeInterrogation:
		return st.interrogation(a, false)
	case iec60870.TypeCounterInterrogation:
		return st.interrogation(a, true)
	case iec60870.TypeClockSync:
		out := a
		out.Cause = iec60870.CauseActivationCon
		st.enqueue1(out)
		return nil
	case iec60870.TypeSingleCommand, iec60870.TypeDoubleCommand,
		iec60870.TypeSetpointNormalized, iec60870.TypeSetpointScaled, iec60870.TypeSetpointFloat:
		return st.command(a)
	}
	return st.negative(a, iec60870.CauseUnknownType)
}

// interrogation 把整站快照压进第 1 级队列，末尾补一条终止报文。
func (st *station) interrogation(req iec60870.ASDU, countersOnly bool) error {
	s := st.server
	s.mu.Lock()
	objs := make([]Info, 0, len(s.info))
	ca := s.ca
	for _, i := range s.info {
		if i.Command || (i.Kind == KindCounter) != countersOnly {
			continue
		}
		objs = append(objs, *i)
	}
	s.mu.Unlock()
	if len(req.Objects) > 0 && countersOnly {
		// 电能量召唤的限定词（QCC）只在本平台回站快照，不细分。
		_ = req
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].IOA < objs[j].IOA })

	// 一对象一条 ASDU：真实装置常把同类型的连续 IOA 合成一条 SQ=1 的
	// 报文，但「一对象一条」同样合法，而且能让主站的逐个对象解析路径
	// 被完整走到。内置从站的价值是覆盖主站分支，不是优化带宽。
	for _, o := range objs {
		st.enqueue1(infoASDU(o, iec60870.CauseInterrogated, ca))
	}
	// 终止报文：与请求同类型 + 激活终止。没有它，主站只能靠「没有更多
	// 数据」加超时来判断结束，一个几百点的站会让每次总召唤都白等到
	// 超时。电能量召唤的终止必须用类型 101，用类型 100 会让主站误以为
	// 站总召唤还没结束。
	termType := byte(iec60870.TypeInterrogation)
	if countersOnly {
		termType = iec60870.TypeCounterInterrogation
	}
	st.enqueue1(iec60870.ASDU{
		TypeID: termType, Cause: iec60870.CauseActivationTerm,
		CommonAddr: ca, Objects: []iec60870.Object{{Qualifier: iec60870.QOIStation}},
	})
	return nil
}

func (st *station) command(a iec60870.ASDU) error {
	if len(a.Objects) == 0 {
		return st.negative(a, iec60870.CauseUnknownIOA)
	}
	o := a.Objects[0]
	s := st.server
	s.mu.Lock()
	cur, ok := s.info[o.IOA]
	if ok && !cur.Writable {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		return st.negative(a, iec60870.CauseUnknownIOA)
	}

	// 激活确认：先告诉主站「命令收到了、要执行了」。
	conf := a
	conf.Cause = iec60870.CauseActivationCon
	st.enqueue1(conf)

	s.mu.Lock()
	var snapshot Info
	monitored := false
	if i, ok := s.info[o.IOA]; ok {
		switch a.TypeID {
		case iec60870.TypeSingleCommand:
			i.Bool = o.Bool
		case iec60870.TypeDoubleCommand:
			i.Num = float64(o.Uint)
		default:
			i.Num = o.Num
		}
		snapshot = *i
		monitored = !i.Command
		s.writes++
	}
	ca := s.ca
	s.mu.Unlock()

	// 同时存在监视方向的对象才自发上送（例如遥控改变的双点状态）。
	if monitored {
		st.enqueue1(infoASDU(snapshot, iec60870.CauseSpontaneous, ca))
	}

	term := a
	term.Cause = iec60870.CauseActivationTerm
	st.enqueue1(term)
	return nil
}

func (st *station) negative(req iec60870.ASDU, cause byte) error {
	out := req
	out.Cause = cause
	out.Negative = true
	st.enqueue1(out)
	return nil
}

// pop 回应一次取数请求：有数据就送一条，没有就回单字节确认/无数据。
func (st *station) pop(class int) ([][]byte, error) {
	st.mu.Lock()
	var a iec60870.ASDU
	var ok bool
	if class == 1 && len(st.q1) > 0 {
		a, st.q1 = st.q1[0], st.q1[1:]
		ok = true
	} else if class == 2 && len(st.q2) > 0 {
		a, st.q2 = st.q2[0], st.q2[1:]
		ok = true
	}
	more := false
	if class == 1 {
		more = len(st.q1) > 0
	} else {
		more = len(st.q2) > 0
	}
	st.mu.Unlock()

	if !ok {
		if st.server.noDataFixed {
			return st.fixedReply(iec101.FCNoData, false)
		}
		return [][]byte{iec101.BuildSingle(true)}, nil
	}
	frame, err := st.dataFrame(a, more)
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

// dataFrame 组一条带用户数据的从站帧。ACD 表示「还有数据」。
func (st *station) dataFrame(a iec60870.ASDU, more bool) ([]byte, error) {
	s := st.server
	s.mu.Lock()
	prof := s.prof
	addr := s.link
	size := s.lsize
	s.mu.Unlock()

	raw, err := iec60870.BuildASDUProfile(a, prof)
	if err != nil {
		return nil, err
	}
	return iec101.BuildVariable(iec101.Control{
		PRM: false, ACD: more, Func: iec101.FCUserData,
	}, addr, size, raw)
}

func (st *station) fixedReply(fc byte, acd bool) ([][]byte, error) {
	s := st.server
	s.mu.Lock()
	addr := s.link
	size := s.lsize
	s.mu.Unlock()
	fr, err := iec101.BuildFixed(iec101.Control{PRM: false, ACD: acd, Func: fc}, addr, size)
	if err != nil {
		return nil, err
	}
	return [][]byte{fr}, nil
}

func infoASDU(i Info, cause byte, ca uint16) iec60870.ASDU {
	return iec60870.ASDU{
		TypeID: typeOf(i), Cause: cause, CommonAddr: ca,
		Objects: []iec60870.Object{infoObject(i)},
	}
}

func typeOf(i Info) byte {
	switch i.Kind {
	case KindSingle:
		if i.Time != nil {
			return iec60870.TypeSinglePointTime
		}
		return iec60870.TypeSinglePoint
	case KindDouble:
		if i.Time != nil {
			return iec60870.TypeDoublePointTime
		}
		return iec60870.TypeDoublePoint
	case KindScaled:
		if i.Time != nil {
			return iec60870.TypeMeasuredScaledTime
		}
		return iec60870.TypeMeasuredScaled
	case KindCounter:
		if i.Time != nil {
			return iec60870.TypeIntegratedTotalsTime
		}
		return iec60870.TypeIntegratedTotals
	}
	if i.Time != nil {
		return iec60870.TypeMeasuredFloatTime
	}
	return iec60870.TypeMeasuredFloat
}

func infoObject(i Info) iec60870.Object {
	o := iec60870.Object{IOA: i.IOA, Quality: i.Quality, Time: i.Time}
	switch i.Kind {
	case KindSingle:
		o.Bool = i.Bool
	case KindDouble:
		o.Uint = uint32(math.Round(i.Num))
	case KindCounter:
		o.Uint = i.Counter
	default:
		o.Num = i.Num
	}
	return o
}

// ---------------------------------------------------------------------------
// TCP 承载
// ---------------------------------------------------------------------------

// tcpStream 把 net.Conn 适配成 transport.Stream。
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
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
