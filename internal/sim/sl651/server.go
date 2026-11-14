// Package sl651 提供《水文监测数据通信规约》SL 651—2014 的内存遥测站
// （虚拟水文终端），用于自检与演示。
//
// 从站复用 codec/sl651 的组帧/解帧与要素编码，因此「中心站解出来的值」与
// 「遥测站写进去的值」不会因为两边各写一套 BCD/定义字节规则而漂移。这一点
// 比表格驱动更有价值：单测里同一份要素会先编码再解码，真出现理解不一致时
// 会立刻暴露。
//
// 刻意只实现一台遥测站的核心行为：
//
//   - 37H（查询实时数据）→ 32H 同形的标准报文，含全部要素；
//   - 38H（查询时段数据）→ 同形应答（本从站不区分时段范围，回当前值）；
//   - 3AH（查询指定要素）→ 只回请求里点名的要素；
//   - 2FH（链路维持）/ 32H（定时报）/ 34H（小时报）自报：由 Trigger 主动发，
//     用于验证驱动的分帧与解析（不在驱动的主动路径上）。
//
// 不做配置表读写（40H~43H）、控制命令（4CH~4FH）、图片报（36H）、M3 多包，
// 与驱动侧的「明确不做」保持一致：一个内置从站不该假装能模拟它无法正确
// 实现的东西，否则只会制造「我们的实现没问题」的错觉。
package sl651

import (
	"errors"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	sl651codec "uic/internal/codec/sl651"
	"uic/internal/transport"
)

// Server 是一台内存遥测站。
type Server struct {
	mu sync.RWMutex
	// center 是中心站地址（应答里回填）。0 表示不校验。
	center int
	// station 是 5 字节 BCD 站号。
	station [5]byte
	// password 是期望的 2 字节密码。zero 表示不校验。
	password [2]byte
	// checkPassword 为真时校验密码。
	checkPassword bool
	// classCode 是应答里的分类码（附录 A）。
	classCode byte
	// fields 是要上送的要素（顺序即报文顺序）。
	fields []sl651codec.Field
	// obsTime 是观测时间偏移：应答里的观测时间 = 收到请求的时刻 - 此偏移。
	// 0 表示用当前时刻。
	obsOffset time.Duration
	// clock 提供时间源，测试可注入。
	clock func() time.Time
	// serial 是流水号计数器，每次应答 +1。
	serial uint16

	// silent 为真时对一切请求都不应答（模拟站号错/总线断开）。
	silent bool
	// badCRC 为真时把应答的校验码改坏一个字节（验证中心站的干扰处理）。
	badCRC bool
	// badFunc 为真时用错误的功能码应答（验证功能码校验）。
	badFunc bool
	// truncate 非 0 时把应答正文截断成这么多字节（验证长度/短帧处理）。
	truncate int
	// delay 是应答前的人为延迟（验证超时分类）。
	delay time.Duration

	// 统计
	requests int64
	queries  map[byte]int64

	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool
	stream transport.Stream
}

// Option 调整从站的初始状态。
type Option func(*Server)

// WithStation 设置遥测站 5 字节 BCD 站号。
func WithStation(addr [5]byte) Option {
	return func(s *Server) { s.station = addr }
}

// WithCenter 设置中心站地址。
func WithCenter(c int) Option {
	return func(s *Server) { s.center = c }
}

// WithPassword 设置并启用密码校验。
func WithPassword(p [2]byte) Option {
	return func(s *Server) { s.password = p; s.checkPassword = true }
}

// WithClass 设置分类码（附录 A）。
func WithClass(code byte) Option {
	return func(s *Server) { s.classCode = code }
}

// WithFields 替换要上送的要素表。
//
// 传 nil 表示「不编报要素」（只剩电压/状态字），用于验证驱动对空要素的处理。
func WithFields(fields ...sl651codec.Field) Option {
	return func(s *Server) { s.fields = append([]sl651codec.Field(nil), fields...) }
}

// WithClock 注入时间源（测试用）。
func WithClock(fn func() time.Time) Option {
	return func(s *Server) { s.clock = fn }
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
		center:    1,
		station:   [5]byte{0x00, 0x31, 0x23, 0x45, 0x67},
		classCode: sl651codec.ClassRiver,
		clock:     func() time.Time { return time.Now().UTC() },
		queries:   map[byte]int64{},
		conns:     map[net.Conn]struct{}{},
	}
	for _, o := range opts {
		o(s)
	}
	if s.fields == nil {
		s.fields = defaultFields()
	}
	return s
}

// defaultFields 是与 examples/sl651.json 对应的一台典型河道站的要素表。
//
// 值的选取刻意覆盖了四种编码分支：常规 BCD（水位/雨量/流量）、HEX 批量数据
// （DRP/DRZ）、状态字（ZT）、缺测填充（全 F 的要素）。这样自检一次就能验证
// 编解码的主要路径。
func defaultFields() []sl651codec.Field {
	z, _ := sl651codec.NewField(sl651codec.GZ, 127)     // 0.127 m
	p1, _ := sl651codec.NewField(sl651codec.GP1, 5)     // 0.5 mm
	pt, _ := sl651codec.NewField(sl651codec.GPT, 1234)  // 123.4 mm
	q, _ := sl651codec.NewField(sl651codec.GQ, 1234567) // 1234.567 m³/s

	zt, _ := sl651codec.NewFieldRaw(sl651codec.GZT, []byte{0x00, 0x00, 0x00, 0x01})

	drpData := make([]byte, 12)
	drpData[0] = 0x19 // 2.5 mm
	drpData[1] = 0x32 // 5.0 mm
	for i := 2; i < 12; i++ {
		drpData[i] = 0xFF // 缺测
	}
	drp, _ := sl651codec.NewFieldRaw(sl651codec.GDRP, drpData)

	// 取（排）水口水位 1 全 F：规约 6.6.4.5 d) 用「与数据位数一样的 F」表示
	// 该时段无数据。这里刻意放一个整组缺测的要素，让自检能走到 Field.NoData
	// 与驱动侧的缺测降级路径（DRP 里那些 0xFF 是同一数据内的分段缺测，
	// 不经过 NoData）。
	z1, _ := sl651codec.NewFieldRaw(sl651codec.GZ1, []byte{0xFF, 0xFF, 0xFF, 0xFF})

	return []sl651codec.Field{z, p1, pt, q, zt, drp, z1}
}

// Addr 返回监听地址（"127.0.0.1:port"）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Station 返回 5 字节 BCD 站号。
func (s *Server) Station() [5]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.station
}

// Requests 返回收到的请求总数。
func (s *Server) Requests() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.requests
}

// QueryCount 返回某功能码的查询次数。
func (s *Server) QueryCount(fn byte) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.queries[fn]
}

// Fields 返回当前要素表（副本）。
func (s *Server) Fields() []sl651codec.Field {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]sl651codec.Field, len(s.fields))
	copy(out, s.fields)
	return out
}

// SetValue 修改某个要素的值（按引导符定位）。
//
// 只支持常规 BCD 要素：HEX 类（DRP/DRZ/ZT）的值不是十进制，用 SetRaw 更清楚。
func (s *Server) SetValue(guide byte, value uint64) error {
	f, err := sl651codec.NewField(guide, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.fields {
		if s.fields[i].Guide == guide {
			s.fields[i] = f
			return nil
		}
	}
	s.fields = append(s.fields, f)
	return nil
}

// SetRaw 用一个已有的字段替换/追加要素。
func (s *Server) SetRaw(f sl651codec.Field) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.fields {
		if s.fields[i].Guide == f.Guide {
			s.fields[i] = f
			return
		}
	}
	s.fields = append(s.fields, f)
}

// Value 读回某个要素的当前值。
func (s *Server) Value(guide byte) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, f := range s.fields {
		if f.Guide == guide {
			v, err := f.Number(0)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// SetSilent 让从站对一切请求静默（模拟站号错/总线断开）。
func (s *Server) SetSilent(on bool) {
	s.mu.Lock()
	s.silent = on
	s.mu.Unlock()
}

// SetBadCRC 让从站应答的校验码错一个字节。
func (s *Server) SetBadCRC(on bool) {
	s.mu.Lock()
	s.badCRC = on
	s.mu.Unlock()
}

// SetBadFunc 让从站用另一个**合法**的上行功能码（32H 定时报）应答。
//
// 注意它换的不是"非法功能码"：32H 本身是合法上行报文，ParseReport 照样能
// 解析成功。它验证的是"终端回的功能码与我们问的不一致"这一现象，而不是
// 驱动对解析失败的处理。要构造"帧成了但报文解不开"，用 SetTruncate。
func (s *Server) SetBadFunc(on bool) {
	s.mu.Lock()
	s.badFunc = on
	s.mu.Unlock()
}

// SetTruncate 让从站的应答正文被截断（验证长度字段与短帧处理）。
func (s *Server) SetTruncate(n int) {
	s.mu.Lock()
	s.truncate = n
	s.mu.Unlock()
}

// SetDelay 让从站在应答前等待（验证超时分类）。
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// Clock 返回从站使用的时间源。
func (s *Server) Clock() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clock()
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
	_ = s.ServeStream(&tcpStream{conn: c})
}

// ServeStream 在一条字节流上跑从站循环。串口/pty 与 TCP 共用它。
func (s *Server) ServeStream(st transport.Stream) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.stream = st
	s.mu.Unlock()

	rd := sl651codec.NewReader(st)
	for {
		f, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || isClosed(err) {
				return nil
			}
			return err
		}
		resp, send, err := s.handle(f)
		if err != nil || !send {
			continue
		}
		if err := st.Write(resp); err != nil {
			return err
		}
	}
}

// handle 处理一帧请求，返回应答与「是否发送」。
func (s *Server) handle(f *sl651codec.Frame) ([]byte, bool, error) {
	s.mu.Lock()
	s.requests++
	s.queries[f.Func]++
	silent, delay := s.silent, s.delay
	s.mu.Unlock()

	if silent {
		return nil, false, nil
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	// 只回应下行查询；上行自报（别人发的）不该由我回应。
	if f.Dir != sl651codec.DirDown {
		return nil, false, nil
	}
	// 站号过滤：不是我的事就保持静默。替别人的地址应答是总线串扰，
	// 在多站总线上会让中心站张冠李戴。
	s.mu.RLock()
	mine := s.station
	center := s.center
	checkPwd := s.checkPassword
	pwd := s.password
	s.mu.RUnlock()
	if f.Station != mine {
		return nil, false, nil
	}
	if checkPwd && f.Password != pwd {
		return nil, false, nil
	}

	switch f.Func {
	case sl651codec.FuncQueryAll, sl651codec.FuncQueryPer:
		return s.reply(f, center, nil)
	case sl651codec.FuncQueryElem:
		guides, err := queryGuides(f.Body)
		if err != nil || len(guides) == 0 {
			return nil, false, nil
		}
		return s.reply(f, center, guides)
	}
	// 其它功能码（配置表/控制命令/时钟）不应答：本从站不实现写方向。
	return nil, false, nil
}

// queryGuides 从 3AH 正文里取出要素引导符列表。
//
// 3AH 正文 = 流水号 2 + 发报时间 6 + n 组「引导符 1 + 数据定义字节 1」。
func queryGuides(body []byte) ([]byte, error) {
	if len(body) < 8 {
		return nil, sl651codec.ErrShort
	}
	rest := body[8:]
	if len(rest) == 0 || len(rest)%2 != 0 {
		return nil, sl651codec.ErrBadFrame
	}
	out := make([]byte, 0, len(rest)/2)
	for i := 0; i < len(rest); i += 2 {
		out = append(out, rest[i])
	}
	return out, nil
}

// reply 组一帧应答报文。
//
// 应答与定时报（32H）同形：这是表 43/45/49 的规定，也是驱动能复用
// ParseReport 的原因。
func (s *Server) reply(req *sl651codec.Frame, center int, only []byte) ([]byte, bool, error) {
	s.mu.Lock()
	fields := make([]sl651codec.Field, len(s.fields))
	copy(fields, s.fields)
	classCode := s.classCode
	station := s.station
	badCRC, badFunc := s.badCRC, s.badFunc
	truncate := s.truncate
	now := s.clock()
	obsOffset := s.obsOffset
	s.serial++
	serial := s.serial
	s.mu.Unlock()

	// 3AH：只回点名的要素。
	if only != nil {
		want := make(map[byte]bool, len(only))
		for _, g := range only {
			want[g] = true
		}
		picked := make([]sl651codec.Field, 0, len(only))
		for _, f := range fields {
			if want[f.Guide] {
				picked = append(picked, f)
				delete(want, f.Guide)
			}
		}
		fields = picked
	}

	b := sl651codec.Builder{
		Center:   center,
		Station:  station,
		Password: req.Password,
	}
	fn := req.Func // 应答功能码与查询一致（表 43/45/49）
	if badFunc {
		fn = sl651codec.FuncTimed
	}
	// 电压单独编报（引导符 VT），与要素组分开：这是表 32 的结构。
	volt := []byte{0x12, 0x10} // 12.10 V（BCD 两字节，低字节 0x10 不是 0x0A）
	raw, err := b.BuildReport(fn, serial, now, station, classCode,
		now.Add(-obsOffset), fields, volt)
	if err != nil {
		return nil, false, err
	}
	if truncate > 0 {
		raw, err = retruncate(raw, truncate)
		if err != nil {
			return nil, false, err
		}
	}
	if badCRC && len(raw) >= 2 {
		raw[len(raw)-1] ^= 0xFF
	}
	return raw, true, nil
}

// retruncate 把报文正文截短到 n 字节，并重算长度与校验码。
//
// 直接截断整帧会得到「长度字段说 40 字节、实际只有 20 字节」的帧，Reader
// 会等不到足够的字节而超时——那不是要验证的路径。这里改成「正文少 n 字节
// 但长度字段与 CRC 都对」，模拟终端编报时漏了尾部要素的形态。
func retruncate(raw []byte, n int) ([]byte, error) {
	f, err := sl651codec.Parse(raw)
	if err != nil {
		return nil, err
	}
	body := f.Body
	if n < len(body) {
		body = body[:n]
	}
	out := make([]byte, 0, len(raw))
	out = append(out, sl651codec.Start, sl651codec.Start)
	// 上行帧：中心站 1 + 站号 5。
	out = append(out, byte(f.Center&0xFF))
	out = append(out, f.Station[:]...)
	out = append(out, f.Password[:]...)
	out = append(out, f.Func)
	rawLen := sl651codec.DirUp | (len(body) & sl651codec.MaxLen)
	out = append(out, byte(rawLen>>8), byte(rawLen&0xFF))
	out = append(out, sl651codec.STX)
	out = append(out, body...)
	out = append(out, sl651codec.ETX)
	return sl651codec.AppendCRC(out), nil
}

// Trigger 主动发一帧自报（2FH 链路维持 / 32H 定时 / 34H 小时报）。
//
// 它只用于「验证驱动的分帧与解析」与演示：中心站驱动不会主动索取自报，
// 真实终端是定时自己发。
func (s *Server) Trigger(kind byte, st transport.Stream) error {
	s.mu.Lock()
	fields := make([]sl651codec.Field, len(s.fields))
	copy(fields, s.fields)
	classCode := s.classCode
	station := s.station
	center := s.center
	now := s.clock()
	s.serial++
	serial := s.serial
	s.mu.Unlock()

	b := sl651codec.Builder{Center: center, Station: station}
	var raw []byte
	var err error
	switch kind {
	case sl651codec.FuncLink:
		raw, err = b.BuildLinkReport(serial, now)
	case sl651codec.FuncHourly:
		raw, err = b.BuildReport(kind, serial, now, station, classCode, now, fields, []byte{0x12, 0x10})
	default:
		raw, err = b.BuildReport(sl651codec.FuncTimed, serial, now, station, classCode, now, fields, []byte{0x12, 0x10})
	}
	if err != nil {
		return err
	}
	return st.Write(raw)
}

// Now 返回从站时钟。
func (s *Server) Now() time.Time { return s.Clock() }

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	return false
}

type tcpStream struct{ conn net.Conn }

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

// sortedGuides 返回要素表里出现过的引导符（升序），供自检输出。
func (s *Server) sortedGuides() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]byte, 0, len(s.fields))
	for _, f := range s.fields {
		out = append(out, f.Guide)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
