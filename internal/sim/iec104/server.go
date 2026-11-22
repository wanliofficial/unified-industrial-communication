// Package iec104 提供 IEC 60870-5-104 内存从站（TCP），用于无硬件自检。
//
// 从站要复现的不是「完整的 104 协议栈」，而是主站最容易搞错的那几处：
//
//  1. **STARTDT 之前不发数据**：104 的数据传输是显式启停的，把帧提前
//     塞过去会让只做时序校验的主站直接判错；
//  2. **总召唤是一次整站快照**，响应里 N 个对象共用一条 ASDU（SQ=1），
//     最后必须补一条「初始化结束」——主站靠它判断召唤完成；
//  3. **序号是双向的**：从站必须按自己的 N(S) 递增发送，并确认主站的
//     N(S)（S 帧或 I 帧捎带）；
//  4. **命令要走完激活 → 激活确认 → 激活终止**三段，否定确认要能复现。
//
// 刻意不做的有：101 串口、文件传输、时钟同步的实际生效、冗余通道切换、
// 带时标的命令。主站也只做这些，两边保持同一份边界。
package iec104

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

	"uic/internal/codec/iec104"
)

// 从站默认的公共地址。
const DefaultCommonAddr uint16 = 1

// InfoKind 是信息对象的类别，决定总召唤时用哪种 ASDU 类型上送。
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
	Quality iec104.Quality
	// Time 非 nil 时用带时标的类型上送。
	Time *time.Time

	// Writable 标记该对象接受命令。真实设备上「遥测」几乎不可写，
	// 「遥控/遥调」才可写；从站必须复现这个差别，否则主站的否定确认
	// 路径测不到。
	Writable bool

	// Command 表示这是一个**只在控制方向存在**的对象：它的 IOA 不参与
	// 监视方向，既不上送总召唤，也不会自发上送。
	//
	// 它与 Writable 是两个正交的概念：一个双点开关状态（遥信）既在监视
	// 方向被召唤，又接受遥控命令，此时 Writable=true、Command=false；
	// 而「遥控号 4001」这类纯命令对象在监视方向根本没有对应信息对象，
	// 此时 Command=true。把两者混成一个标志，会让总召唤里凭空多出一
	// 批「点位」，主站把它们当成真实状态显示出来。
	Command bool
}

// Server 是 IEC 104 内存从站。
type Server struct {
	ln   net.Listener
	log  *slog.Logger
	ca   uint16
	addr string

	mu    sync.Mutex
	info  map[uint32]*Info
	conns map[*station]bool
	// autoIncrement 让 3001 每秒自增，便于观察 SSE 与死区。
	autoIncrement bool
	closed        chan struct{}
	closeOnce     sync.Once

	// 诊断计数：解析失败的帧数，供测试断言「垃圾帧被静默丢弃」。
	badFrames uint64
}

// NewServer 启动从站。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("iec104 sim: listen %q: %w", addr, err)
	}
	s := &Server{
		ln:            ln,
		log:           slog.Default(),
		ca:            DefaultCommonAddr,
		addr:          ln.Addr().String(),
		info:          map[uint32]*Info{},
		conns:         map[*station]bool{},
		autoIncrement: true,
		closed:        make(chan struct{}),
	}
	s.installDefaults()
	go s.accept()
	go s.tick()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.addr }

// Close 停止监听并断开所有连接。
//
// 幂等：第二次调用返回 nil。上层（进程关闭流程、测试清理）经常会在
// 不同路径上各关一次，重复调用报「already closed」只会制造噪音。
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
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

// SetCommonAddr 设置本站的公共地址。
func (s *Server) SetCommonAddr(ca uint16) {
	s.mu.Lock()
	s.ca = ca
	s.mu.Unlock()
}

// BadFrames 返回解析失败的帧数。
func (s *Server) BadFrames() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badFrames
}

func (s *Server) installDefaults() {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	for _, i := range []*Info{
		{IOA: 1001, Kind: KindFloat, Num: 42.5},
		{IOA: 1002, Kind: KindFloat, Num: 7.25},
		{IOA: 1003, Kind: KindScaled, Num: 1234},
		{IOA: 1004, Kind: KindFloat, Num: 0.5, Quality: iec104.QSubstituted},
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

// SetValue 直接写入一个对象（测试与自检用）。
func (s *Server) SetValue(ioa uint32, v Info) error {
	s.mu.Lock()
	cur, ok := s.info[ioa]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("iec104 sim: IOA %d not configured", ioa)
	}
	next := v
	next.IOA = ioa
	next.Kind = cur.Kind
	next.Writable = cur.Writable
	next.Command = cur.Command
	s.info[ioa] = &next
	s.mu.Unlock()
	s.broadcast(ioa)
	return nil
}

// Value 返回对象副本。
func (s *Server) Value(ioa uint32) (Info, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.info[ioa]
	if !ok {
		return Info{}, false
	}
	return *i, true
}

// Objects 返回按 IOA 排序的对象列表。
func (s *Server) Objects() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Info, 0, len(s.info))
	for _, i := range s.info {
		out = append(out, *i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].IOA < out[b].IOA })
	return out
}

// tick 让计数器每秒自增并主动上送（模拟「突发传输」）。
func (s *Server) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			// autoIncrement 由 mu 保护（SetAutoIncrement 在测试里随时改），
			// 读它必须在锁内；放到锁外读是一次真实的数据竞争。
			s.mu.Lock()
			if !s.autoIncrement {
				s.mu.Unlock()
				continue
			}
			i, ok := s.info[3001]
			if ok {
				i.Counter++
			}
			s.mu.Unlock()
			if ok {
				s.broadcast(3001)
			}
		}
	}
}

// SetAutoIncrement 控制计数器是否自增。
func (s *Server) SetAutoIncrement(v bool) {
	s.mu.Lock()
	s.autoIncrement = v
	s.mu.Unlock()
}

// broadcast 把某个对象按突发原因发给所有已启动数据传输的连接。
func (s *Server) broadcast(ioa uint32) {
	s.mu.Lock()
	var targets []*station
	for st := range s.conns {
		// started 归 station.mu 保护，不是 server.mu。在 s.mu 里直接读它
		// 与 STARTDT/STOPDT 的写入构成竞争（-race 能抓到）。
		st.mu.Lock()
		started := st.started
		st.mu.Unlock()
		if started {
			targets = append(targets, st)
		}
	}
	s.mu.Unlock()
	for _, st := range targets {
		st.sendInfo(ioa, iec104.CauseSpontaneous)
	}
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

func (s *Server) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		st := &station{server: s, nc: nc}
		s.mu.Lock()
		s.conns[st] = true
		s.mu.Unlock()
		go st.serve()
	}
}

type station struct {
	server *Server
	nc     net.Conn

	// mu 保护序号与「已启动」标志。写帧必须串行：104 的帧之间没有事务
	// 号，两个写交错会让序号与实际到达顺序不一致。
	mu      sync.Mutex
	sendSeq uint16
	recvSeq uint16
	started bool
	closed  bool

	// wmu 只保护写调用本身，避免 mu 在阻塞写时被长期占住。
	wmu sync.Mutex
}

func (st *station) serve() {
	defer func() {
		st.mu.Lock()
		st.closed = true
		st.mu.Unlock()
		st.server.mu.Lock()
		delete(st.server.conns, st)
		st.server.mu.Unlock()
		_ = st.nc.Close()
	}()

	head := make([]byte, 2)
	for {
		if _, err := io.ReadFull(st.nc, head); err != nil {
			return
		}
		if head[0] != iec104.StartByte {
			st.bad()
			return
		}
		n := int(head[1])
		if n < 4 || n > iec104.MaxAPDU {
			st.bad()
			return
		}
		frame := make([]byte, 2+n)
		copy(frame, head)
		if _, err := io.ReadFull(st.nc, frame[2:]); err != nil {
			return
		}
		f, err := iec104.ParseFrame(frame)
		if err != nil {
			st.bad()
			return
		}
		if err := st.handle(f); err != nil {
			return
		}
	}
}

func (st *station) bad() {
	st.server.mu.Lock()
	st.server.badFrames++
	st.server.mu.Unlock()
}

func (st *station) handle(f iec104.Frame) error {
	switch f.Kind {
	case iec104.KindI:
		st.mu.Lock()
		want := st.recvSeq
		st.mu.Unlock()
		if f.SendSeq != want {
			return fmt.Errorf("iec104 sim: N(S)=%d, want %d", f.SendSeq, want)
		}
		st.mu.Lock()
		st.recvSeq = (st.recvSeq + 1) & 0x7FFF
		st.mu.Unlock()
		// 收到 I 帧即确认对端的发送窗口；不需要额外回 S 帧。
		if len(f.ASDU) == 0 {
			return nil
		}
		return st.apply(f.ASDU)
	case iec104.KindS:
		// 对端确认了我们的 N(S)；这里只需要不把它当成错误。
		return nil
	case iec104.KindU:
		switch f.UFunc {
		case iec104.UStartAct:
			st.mu.Lock()
			st.started = true
			st.mu.Unlock()
			return st.writeU(iec104.UStartCon)
		case iec104.UStopAct:
			st.mu.Lock()
			st.started = false
			st.mu.Unlock()
			return st.writeU(iec104.UStopCon)
		case iec104.UTestAct:
			return st.writeU(iec104.UTestCon)
		case iec104.UTestCon:
			return nil
		}
	}
	return nil
}

// apply 处理主站发来的 ASDU。
func (st *station) apply(raw []byte) error {
	a, err := iec104.ParseASDU(raw)
	if err != nil {
		st.bad()
		return nil
	}
	s := st.server
	s.mu.Lock()
	ca := s.ca
	s.mu.Unlock()
	if a.CommonAddr != ca {
		// 公共地址不匹配：按标准回「未知公共地址」，这是主站排查
		// 「地址配错」时唯一能拿到的线索。
		return st.negative(a, iec104.CauseUnknownCommonAddr)
	}
	switch a.TypeID {
	case iec104.TypeInterrogation:
		if len(a.Objects) == 0 {
			return nil
		}
		if a.Objects[0].Qualifier < iec104.QOIStation {
			return st.negative(a, iec104.CauseUnknownCause)
		}
		return st.interrogation(a)
	case iec104.TypeCounterInterrogation:
		return st.counterInterrogation(a)
	case iec104.TypeClockSync:
		return st.confirm(a)
	case iec104.TypeSingleCommand, iec104.TypeDoubleCommand,
		iec104.TypeSetpointNormalized, iec104.TypeSetpointScaled, iec104.TypeSetpointFloat:
		return st.command(a)
	}
	return st.negative(a, iec104.CauseUnknownType)
}

func (st *station) interrogation(req iec104.ASDU) error {
	s := st.server
	s.mu.Lock()
	objs := make([]Info, 0, len(s.info))
	ca := s.ca
	for _, i := range s.info {
		if i.Kind == KindCounter {
			continue // 累计量走电能量召唤
		}
		if i.Command {
			// 纯命令对象在监视方向不存在，站召唤不上送它。真实设备里
			// 遥控号与遥信号是两套独立地址，强行把命令对象塞进总召唤
			// 会让主站把「这个号我能写」误读成「这个号有当前状态」。
			continue
		}
		objs = append(objs, *i)
	}
	s.mu.Unlock()
	sort.Slice(objs, func(a, b int) bool { return objs[a].IOA < objs[b].IOA })

	// 按「ASDU 类型 + 是否带时标」分组，每组一条 SQ=1 的 ASDU（IOA 连续
	// 时才用 SQ）。真实站点通常是「一个类型一条报文」，逐对象一条报文也
	// 正确但报文数会翻十几倍。
	for _, g := range groupByType(objs) {
		if err := st.sendGroup(g, iec104.CauseInterrogated, ca); err != nil {
			return err
		}
	}
	// 初始化结束：主站靠它判断召唤完成。
	if err := st.writeASDU(iec104.ASDU{
		TypeID: iec104.TypeEndOfInit, Cause: iec104.CauseActivationCon, CommonAddr: ca,
		Objects: []iec104.Object{{Qualifier: 0}},
	}); err != nil {
		return err
	}
	return st.confirm(req)
}

func (st *station) counterInterrogation(req iec104.ASDU) error {
	s := st.server
	s.mu.Lock()
	var objs []Info
	for _, i := range s.info {
		if i.Kind == KindCounter && !i.Command {
			objs = append(objs, *i)
		}
	}
	ca := s.ca
	s.mu.Unlock()
	sort.Slice(objs, func(a, b int) bool { return objs[a].IOA < objs[b].IOA })
	for _, g := range groupByType(objs) {
		if err := st.sendGroup(g, iec104.CauseInterrogated, ca); err != nil {
			return err
		}
	}
	if err := st.writeASDU(iec104.ASDU{
		TypeID: iec104.TypeEndOfInit, Cause: iec104.CauseActivationCon, CommonAddr: ca,
		Objects: []iec104.Object{{Qualifier: 0}},
	}); err != nil {
		return err
	}
	return st.confirm(req)
}

func (st *station) command(req iec104.ASDU) error {
	if len(req.Objects) == 0 {
		return st.negative(req, iec104.CauseUnknownIOA)
	}
	ioao := req.Objects[0]
	s := st.server
	s.mu.Lock()
	cur, ok := s.info[ioao.IOA]
	if ok && !cur.Writable {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		// 对象不存在或不可写：标准里这两者没有区分，统一回「未知 IOA」。
		// 主站据此把点位标成地址错——比「写入失败」更可操作。
		return st.negative(req, iec104.CauseUnknownIOA)
	}

	// 激活确认（P/N=0）。
	if err := st.ackCommand(req, false); err != nil {
		return err
	}

	s.mu.Lock()
	monitored := false
	if i, ok := s.info[ioao.IOA]; ok {
		switch req.TypeID {
		case iec104.TypeSingleCommand:
			i.Bool = ioao.Bool
		case iec104.TypeDoubleCommand:
			i.Num = float64(ioao.Uint)
		default:
			i.Num = ioao.Num
		}
		monitored = !i.Command
	}
	s.mu.Unlock()

	// 只有「同时存在监视方向」的对象才自发上送（例如双点开关状态被遥控
	// 改变）。纯命令对象没有监视信息，上送它等于凭空造一条遥信。
	if monitored {
		st.sendInfo(ioao.IOA, iec104.CauseSpontaneous)
	}

	// 激活终止（P/N=0，cause=10）：表示「已执行完」。
	term := req
	term.Cause = iec104.CauseActivationTerm
	return st.writeASDU(term)
}

func (st *station) confirm(req iec104.ASDU) error {
	out := req
	out.Cause = iec104.CauseActivationCon
	return st.writeASDU(out)
}

func (st *station) negative(req iec104.ASDU, cause byte) error {
	out := req
	out.Cause = cause
	out.Negative = true
	return st.writeASDU(out)
}

func (st *station) ackCommand(req iec104.ASDU, neg bool) error {
	out := req
	out.Cause = iec104.CauseActivationCon
	out.Negative = neg
	return st.writeASDU(out)
}

func (st *station) sendInfo(ioa uint32, cause byte) {
	s := st.server
	s.mu.Lock()
	i, ok := s.info[ioa]
	var copy Info
	if ok {
		copy = *i
	}
	ca := s.ca
	st.mu.Lock()
	started := st.started
	st.mu.Unlock()
	s.mu.Unlock()
	if !ok || !started {
		return
	}
	_ = st.writeASDU(infoASDU(copy, cause, ca))
}

// groupByType 按「ASDU 类型」把对象分组，保持 IOA 升序。
func groupByType(objs []Info) [][]Info {
	order := []byte{}
	groups := map[byte][]Info{}
	for _, o := range objs {
		t := typeOf(o)
		if _, ok := groups[t]; !ok {
			order = append(order, t)
		}
		groups[t] = append(groups[t], o)
	}
	out := make([][]Info, 0, len(order))
	for _, t := range order {
		out = append(out, groups[t])
	}
	return out
}

func typeOf(i Info) byte {
	switch i.Kind {
	case KindSingle:
		if i.Time != nil {
			return iec104.TypeSinglePointTime
		}
		return iec104.TypeSinglePoint
	case KindDouble:
		if i.Time != nil {
			return iec104.TypeDoublePointTime
		}
		return iec104.TypeDoublePoint
	case KindScaled:
		if i.Time != nil {
			return iec104.TypeMeasuredScaledTime
		}
		return iec104.TypeMeasuredScaled
	case KindCounter:
		// 带 Time 的累计量按 M_IT_TB_1 上送：CRT 的序号位不能替代时标，
		// 「某个时刻的读数」是电能量结算的必需信息。
		if i.Time != nil {
			return iec104.TypeIntegratedTotalsTime
		}
		return iec104.TypeIntegratedTotals
	}
	if i.Time != nil {
		return iec104.TypeMeasuredFloatTime
	}
	return iec104.TypeMeasuredFloat
}

func infoASDU(i Info, cause byte, ca uint16) iec104.ASDU {
	return iec104.ASDU{
		TypeID: typeOf(i), Cause: cause, CommonAddr: ca,
		Objects: []iec104.Object{infoObject(i)},
	}
}

func infoObject(i Info) iec104.Object {
	o := iec104.Object{IOA: i.IOA, Quality: i.Quality, Time: i.Time}
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

// sendGroup 把一组同类型对象放进一条 ASDU。IOA 连续时用 SQ=1。
func (st *station) sendGroup(objs []Info, cause byte, ca uint16) error {
	if len(objs) == 0 {
		return nil
	}
	// 先判断连续性，再组装对象：把「判断」和「构建」写进同一个循环，
	// 一旦发现不连续就 break，会把这个组静默截断成前两个对象——报文
	// 本身完全合法，只是少了数据，主站那边表现为「有些点位永远没有值」。
	consecutive := len(objs) > 1
	for i := 1; i < len(objs) && consecutive; i++ {
		if objs[i].IOA != objs[0].IOA+uint32(i) {
			consecutive = false
		}
	}
	a := iec104.ASDU{TypeID: typeOf(objs[0]), Cause: cause, CommonAddr: ca, SQ: consecutive}
	for _, o := range objs {
		a.Objects = append(a.Objects, infoObject(o))
	}
	return st.writeASDU(a)
}

// writeASDU 分配 N(S) 并写出一帧。
func (st *station) writeASDU(a iec104.ASDU) error {
	raw, err := iec104.BuildASDU(a)
	if err != nil {
		return err
	}
	return st.writeIFrame(raw)
}

func (st *station) writeIFrame(asdu []byte) error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return errors.New("iec104 sim: closed")
	}
	seq := st.sendSeq
	ack := st.recvSeq
	st.sendSeq = (st.sendSeq + 1) & 0x7FFF
	st.mu.Unlock()

	frame, err := iec104.BuildIFrame(seq, ack, asdu)
	if err != nil {
		return err
	}
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_, err = st.nc.Write(frame)
	return err
}

func (st *station) writeU(fn byte) error {
	frame, err := iec104.BuildUFrame(fn)
	if err != nil {
		return err
	}
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_, err = st.nc.Write(frame)
	return err
}
