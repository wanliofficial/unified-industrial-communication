// Package mbus 提供 M-Bus（EN 13757）的内存从站（虚拟表计），用于自检与演示。
//
// 从站复用 codec/mbus 的组帧/解帧与记录编码，因此「主站解出来的值」与
// 「从站写进去的值」不会因为两边各写一套 DIF/VIF 规则而漂移。这一点比
// 表格驱动更有价值：单测里同一份记录会先编码再解码，真出现理解不一致时
// 会立刻暴露。
//
// 刻意只实现一台表计的核心行为：
//
//   - SND_NKE（10 40 A CS 16）→ 单字符 E5，并清零 FCB 状态；
//   - REQ_UD2（10 5B/7B A CS 16）→ RSP_UD（68 L L 68 C A 72 …）；
//   - FCB 回放：同一 FCB 的重传不累加访问号，直接回放上一次应答；
//   - 次地址/广播/短帧 SND_UD 明确不应答（总线上不该由我来接）。
//
// 不做 SND_UD（写参数/改地址/校时），不做多表共存（一个进程一台表），
// 不做次级地址选择。这些都属于"一个内置从站不该假装能模拟"的范畴：
// 它们依赖表厂剖面，模拟出来只会制造"我们的实现没问题"的错觉。
package mbus

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	mb "uic/internal/codec/mbus"
	"uic/internal/transport"
)

// Server 是一台内存 M-Bus 从站。
type Server struct {
	mu sync.RWMutex
	// addr 是主地址（0..250）。
	addr byte
	// header 是 12 字节固定头的模板；AccessNo 每次应答后自增。
	header mb.Header
	// records 是报文里的记录顺序与内容。
	records []mb.RecordSpec
	// moreRecords 为真时在报文末尾追加 DIF=1Fh 标记"还有后续"。
	moreRecords bool
	// moreThenAck 为真时：第一段声明"还有后续"，第二段只回 E5。
	//
	// 这是标准允许的多段读取终止方式之一（EN 13757-3 §6.5：「直到应答仅为
	// E5 或不再有 DIF=1Fh」）。主站若把 E5 当帧错误，整轮采集会全灭。
	moreThenAck  bool
	moreDeclared int
	// ackOnly 为真时对所有 REQ_UD2 只回 E5（收到请求但无数据）。
	ackOnly bool

	// fcbSeen/fcbValue 实现 FCB 回放语义。
	fcbSeen  bool
	fcbValue bool
	// lastReply 是上一次成功应答的原始帧，用于 FCB 回放。
	lastReply []byte

	// badChecksum 为真时把应答的校验和改坏一个字节（用于验证主站的
	// 干扰处理路径：应报协议错误并重试，而不是把坏数据当真值）。
	badChecksum bool
	// badCI 非零时把应答的 CI 字段换成它（用于验证主站对「C/CI 不对」
	// 的处理路径：应报协议错误且**不重发**）。
	//
	// 注意这里改的是 CI 而不是地址：地址被改成 0xFDh 会让 codec 在**分帧**
	// 阶段就拒绝（Reader 跳过坏帧 → 最终表现成超时），走不到「帧成了但内容
	// 不对」那条分支。只有让帧完整、校验和正确、只是 CI 不受支持，才能构造
	// 出 spec §3.10.25.6 里「从站在正常应答，只是我们理解错了」的场景。
	badCI byte
	// silent 为真时对一切请求都不应答（模拟总线断开/站号错）。
	silent bool
	// statusError 为真时状态字报应用错误（bit0-1 非零）。
	statusError bool
	// errorCode 非零时对 REQ_UD2 回 CI=70h 通用错误应答（注入用）。
	errorCode byte

	requests int64
	nkeCount int64
	reads    int64
	// reqCount 是收到的 REQ_UD1/REQ_UD2 帧数（含 FCB 回放）。
	//
	// 它与 reads 的区别正是「重发」：主站重发同一 FCB 时从站回放旧应答，
	// reads 不变而 reqCount 会加一。测试「某类错误该不该重发」只能看它。
	reqCount int64

	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool
	stream transport.Stream
}

// Option 调整从站的初始状态。
type Option func(*Server)

// WithAddress 设置主地址。
func WithAddress(a byte) Option {
	return func(s *Server) {
		if mb.IsPrimaryAddress(a) {
			s.addr = a
		}
	}
}

// WithHeader 设置固定头里的身份信息（表号/厂商/介质/版本）。
func WithHeader(h mb.Header) Option {
	return func(s *Server) {
		if h.ID != 0 || h.IDText != "" {
			s.header.ID = h.ID
			s.header.IDText = h.IDText
		}
		if h.Manufacturer != 0 || h.ManufacturerID != "" {
			s.header.Manufacturer = h.Manufacturer
			s.header.ManufacturerID = h.ManufacturerID
		}
		s.header.Version = h.Version
		s.header.Medium = h.Medium
		s.header.Signature = h.Signature
	}
}

// WithRecords 替换整张记录表。
func WithRecords(specs ...mb.RecordSpec) Option {
	return func(s *Server) { s.records = append([]mb.RecordSpec(nil), specs...) }
}

// WithMoreRecords 让从站在报文末尾声明"还有后续"。
//
// 它只用于测试驱动的跟读逻辑：真实表只有在整表放不进一帧时才这样做。
func WithMoreRecords(on bool) Option {
	return func(s *Server) { s.moreRecords = on }
}

// WithMoreRecordsThenAck 让第一段声明"还有后续"，跟随请求只回 E5。
//
// 标准允许从站用单字符 E5 结束多段读取（此时没有更多数据），驱动必须把它
// 当成正常终止而不是帧错误。
func WithMoreRecordsThenAck() Option {
	return func(s *Server) { s.moreThenAck = true }
}

// WithAckOnly 让从站对所有 REQ_UD2 只回 E5（收到请求但没有数据）。
func WithAckOnly() Option {
	return func(s *Server) { s.ackOnly = true }
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
		addr: 1,
		header: mb.Header{
			IDText:         "12345678",
			ManufacturerID: "XYZ",
			Version:        0x01,
			Medium:         mb.MediumHeat,
		},
		conns: map[net.Conn]struct{}{},
	}
	for _, o := range opts {
		o(s)
	}
	// 身份信息既可以用文本给，也可以直接用整数给（解码侧两者都要能解）。
	if s.header.ID == 0 && s.header.IDText != "" {
		if id, err := mb.BCDID(s.header.IDText); err == nil {
			s.header.ID = id
		}
	}
	if s.header.Manufacturer == 0 && s.header.ManufacturerID != "" {
		if m, err := mb.DecodeManufacturerCode(s.header.ManufacturerID); err == nil {
			s.header.Manufacturer = m
		}
	}
	if len(s.records) == 0 {
		s.records = defaultRecords()
	}
	return s
}

// defaultRecords 是与 examples/mbus.json 对应的一台典型热表。
//
// 值的选取刻意覆盖了四种长度编码（1/2/4 字节整数 + 2 字节 BCD）与三种
// 单位族（Wh、m^3、°C、W），这样自检一次就能验证编解码的主要分支。
func defaultRecords() []mb.RecordSpec {
	return []mb.RecordSpec{
		{VIF: 0x03, Width: 0x04, Value: 12345678},         // 能量 12345678 Wh（= 12345.678 kWh）
		{VIF: 0x13, Width: 0x04, Value: 456789},           // 累计量 456.789 m³（m³ × 1e-3）
		{VIF: 0x5A, Width: 0x02, Value: 905},              // 供水温度 90.5 °C（°C × 1e-1）
		{VIF: 0x5E, Width: 0x02, Value: 561},              // 回水温度 56.1 °C
		{VIF: 0x3B, Width: 0x04, Value: 2500},             // 瞬时流量 2.500 m³/h
		{VIF: 0x2B, Width: 0x04, Value: 24500},            // 瞬时功率 24.5 kW
		{VIF: 0x20, Width: 0x04, Value: 86400},            // 运行时间 86400 s
		{VIF: 0x6C, Width: 0x02, Raw: []byte{0x0F, 0x33}}, // 结算日期 2024-03-15
	}
}

// Addr 返回监听地址（TCP 模式）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Address 返回主地址。
func (s *Server) Address() byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr
}

// Requests 返回收到的请求帧数（含因地址不符被静默丢弃的帧）。
func (s *Server) Requests() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.requests
}

// Reads 返回成功应答的 REQ_UD2 次数（不含 FCB 回放）。
func (s *Server) Reads() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reads
}

// ReqCount 返回收到的 REQ_UD1/REQ_UD2 帧数（含 FCB 回放）。
//
// 它与 Reads() 的差值就是重发次数：主站重发同一 FCB 时从站回放旧应答，
// Reads 不增而 ReqCount 增。判断「某类错误该不该重发」只能看它。
func (s *Server) ReqCount() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reqCount
}

// NKECount 返回收到的 SND_NKE 次数。
func (s *Server) NKECount() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nkeCount
}

// AccessNo 返回当前访问号（下一次应答会自增）。
func (s *Server) AccessNo() byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.header.AccessNo
}

// SetValue 按物理量名替换一条记录的值（保持记录顺序与 VIF 不变）。
//
// 按名字而不是按下标，是因为自检命令与测试都按 `q:` 名字描述点位；用下标
// 会让测试在记录表调整顺序后静默失效。
func (s *Server) SetValue(quantity string, value int64) error {
	q := mb.NormalizeQuantity(quantity)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		info := mb.LookupVIF(s.records[i].VIF, s.records[i].VIFEs)
		if mb.NormalizeQuantity(info.Quantity) == q {
			s.records[i].Value = value
			return nil
		}
	}
	return errors.New("从站里没有记录 " + quantity)
}

// Value 返回一条记录的原始整数值。
func (s *Server) Value(quantity string) (int64, bool) {
	q := mb.NormalizeQuantity(quantity)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		info := mb.LookupVIF(r.VIF, r.VIFEs)
		if mb.NormalizeQuantity(info.Quantity) == q {
			return r.Value, true
		}
	}
	return 0, false
}

// Records 返回当前记录表的副本（供自检命令打印"这台表有什么"）。
func (s *Server) Records() []mb.RecordSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]mb.RecordSpec(nil), s.records...)
}

// SetBadChecksum 开关"应答校验和坏一个字节"。
func (s *Server) SetBadChecksum(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badChecksum = on
}

// SetBadCI 让从站用一个"不受支持的 CI"应答。
//
// 它只用于验证主站的重发语义：spec §3.10.25.6 规定校验和错要重发（瞬态
// 干扰），而 C/CI 不对**不重发**（从站在正常应答，重试无用）。非零值才会
// 生效；0 表示恢复正常应答。
func (s *Server) SetBadCI(ci byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badCI = ci
}

// SetSilent 开关"完全不应答"。
func (s *Server) SetSilent(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = on
}

// SetStatusError 开关"状态字报应用错误"。
func (s *Server) SetStatusError(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusError = on
}

// SetErrorResponse 让从站对后续 REQ_UD2 回 CI=70h 通用错误应答（code 为
// EN 13757-3 的应用错误码）。非零 code 结束注入，恢复数据应答。用于验证
// 主站把「表在报错」如实上抛，而不是降级成逐点 address_error。
func (s *Server) SetErrorResponse(code byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errorCode = code
}

// SetAccessNo 直接设置访问号（用于构造特定向量）。
func (s *Server) SetAccessNo(n byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header.AccessNo = n
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

	rd := mb.NewReader(st)
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
	}
}

// handle 处理一帧请求，返回应答（nil 表示不应答）。
func (s *Server) handle(fr mb.Frame) ([]byte, bool) {
	s.mu.Lock()
	s.requests++
	addr := s.addr
	silent := s.silent
	s.mu.Unlock()

	if silent {
		return nil, true
	}
	// 地址过滤：不是我的事就保持静默。替别人的地址应答是总线串扰，
	// 在多表总线上会让主站张冠李戴。
	if fr.Type != mb.FrameAck && fr.A != addr {
		return nil, true
	}
	// 只处理主站报文；从站报文（别人发的应答）不该由我回应。
	if !fr.Master() && fr.Type != mb.FrameAck {
		return nil, true
	}

	switch fr.Type {
	case mb.FrameShort:
		switch fr.Function() {
		case mb.FuncSNDNKE:
			return s.handleNKE()
		case mb.FuncREQUD2, mb.FuncREQUD1:
			return s.handleREQ(fr)
		}
		// 其它短帧（如 SND_UD 的错误用法）不应答：标准里没有对它们的
		// 定义，回一个 E5 会让主站以为写成功了。
		return nil, true
	case mb.FrameLong, mb.FrameControl:
		// 长帧请求只有 SND_UD 一类；本从站不实现写，明确不应答。
		return nil, true
	}
	return nil, true
}

func (s *Server) handleNKE() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nkeCount++
	// SND_NKE 的语义就是"把 FCB 状态清零"。
	s.fcbSeen = false
	s.lastReply = nil
	// 多段序列也从第一段重新开始。
	s.moreDeclared = 0
	return mb.BuildAck(), false
}

func (s *Server) handleREQ(fr mb.Frame) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqCount++

	// FCB 回放：FCV=1 且 FCB 与上一次相同 → 主站在重传，回放上一次应答。
	//
	// 这一步是 M-Bus 重传语义的核心。如果不回放而是重新生成一份，访问号
	// 会重复累加、存储值会被重读，在某些表上还会把"当前值"推进到下一个
	// 计费周期。
	if fr.FCV() && s.fcbSeen && fr.FCB() == s.fcbValue && len(s.lastReply) > 0 {
		return append([]byte(nil), s.lastReply...), false
	}
	s.fcbSeen = true
	s.fcbValue = fr.FCB()

	// 错误应答注入：CI=70h 通用错误（带 EN 13757-3 应用错误码）。
	// 同样纳入 FCB 回放与访问号自增，与正常应答的语义保持一致。
	if s.errorCode != 0 {
		frame := mb.BuildLong(mb.CtrlRSPUD, s.addr, mb.CIGeneralError, []byte{s.errorCode})
		s.lastReply = append([]byte(nil), frame...)
		s.header.AccessNo++
		return frame, false
	}

	if s.ackOnly {
		// 收到请求但没有数据：只回单字符 E5（不是错误帧）。
		// 仍写入 lastReply，保证同一 FCB 的重传回放的是这条 E5。
		s.lastReply = append([]byte(nil), mb.BuildAck()...)
		return mb.BuildAck(), false
	}

	if s.moreThenAck && s.moreDeclared > 0 {
		// 后续段没有更多记录：标准允许只回 E5（收到请求、无数据）。
		// 仍写入 lastReply，保证同一 FCB 的重传回放的是这条 E5。
		s.lastReply = append([]byte(nil), mb.BuildAck()...)
		return mb.BuildAck(), false
	}

	data, err := s.buildData()
	if err != nil {
		// 组帧失败说明记录表配置有误（例如数据域超过单帧上限）。
		// 沉默比回一个错的帧更安全：主站会超时并报告链路问题。
		return nil, true
	}
	if s.moreThenAck {
		s.moreDeclared++
	}
	frame := mb.BuildLong(mb.CtrlRSPUD, s.addr, mb.CIVariableData, data)
	if s.badCI != 0 {
		// 换 CI 之后必须重算校验和：否则失败原因会退化成"校验和错"，
		// 而校验和错是可重发的，测不到"CI 错不重发"这条语义。
		length := int(frame[1])
		frame[6] = s.badCI
		frame[4+length] = mb.Checksum(frame[4], frame[5], frame[6], frame[7:4+length])
	}
	if s.badChecksum {
		// 改坏校验和而不是改坏数据：这是总线上真实会发生的形态，
		// 而且主站必须靠校验和（而不是内容）发现它。
		length := int(frame[1])
		frame[4+length] ^= 0xFF
	}
	s.header.AccessNo++
	s.reads++
	s.lastReply = append([]byte(nil), frame...)
	return frame, false
}

// buildData 组装 12 字节固定头 + 记录。
func (s *Server) buildData() ([]byte, error) {
	h := s.header
	if s.statusError {
		h.Status |= 0x01 // 应用错误码 1：非零即"值不可信"
	}
	data, err := mb.BuildVariableData(h, s.records)
	if err != nil {
		return nil, err
	}
	if s.moreRecords || s.moreThenAck {
		// DIF=1Fh（更多记录跟随）+ 两个厂商字节。真实表会在这里放
		// 厂商特定数据；这里放可预测的常量，方便测试断言。
		data = append(data, 0x1F, 0xAA, 0xBB)
	}
	return data, nil
}

// tcpStream 把 net.Conn 适配成 transport.Stream 的最小实现。
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
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return false
}

// Clock 是给自检命令用的"从站当前时间"，仅供展示。
func (s *Server) Clock() time.Time { return time.Now().UTC() }
