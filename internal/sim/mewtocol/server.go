// Package mewtocol 实现 Mewtocol（计算机链接）的内置从站（模拟 PLC）。
//
// 它提供一块「字区 + 接点区 + 定时器值区」的内存表，按 Mewtocol 帧响应
// 读写请求。用途与其它内置从站一致：没有硬件时验证主站链路、CI 回归、
// 故障注入。
//
// 内存模型刻意简单，但**语义必须与主站完全对称**：
//
//   - 字区（dt/ld/fl）按字索引，读写单位是 16 位字；
//   - 接点区（x/y/r/l）按字索引再按位取，一个字号覆盖 16 个接点；
//     T/C 的接点没有「位」的概念，一个号就是一个接点；
//   - 索引寄存器（ix/iy/id）没有编号，整个区只有一个值（id 是两个）；
//   - 定时器值区（sv/ev）按号索引。
//
// 为什么从站不另写一套帧解析：主站与从站对「RD 的地址是 5 位、RCC 的是
// 4 位、接点号末位是十六进制」这些细节各自实现，迟早会漂移。漂移的表现
// 是「主站自检通过、对接真 PLC 全错」——最坏的一种假绿。
package mewtocol

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	mewcodec "uic/internal/codec/mewtocol"
)

// Server 是 Mewtocol 从站。
type Server struct {
	ln net.Listener

	// station 是本从站应答的站号。收到其它站号的请求一律不应答——真实
	// 的 PLC 也是这样：总线上每个单元只听自己的号。
	station mewcodec.Station

	mu   sync.Mutex
	word map[string]map[int]uint16 // 字区与定时器值区：区名 → 号 → 值
	bit  map[string]map[int]bool   // 接点区：区名 → 接点号 → 值
}

// NewServer 在 addr 上监听，站号取 1。addr 为 "127.0.0.1:0" 时由系统
// 自动分配端口。
func NewServer(addr string) (*Server, error) {
	return NewServerStation(addr, 1)
}

// NewServerStation 指定站号。
func NewServerStation(addr string, station int) (*Server, error) {
	st := mewcodec.Station(station)
	if err := st.Validate(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:      ln,
		station: st,
		word:    map[string]map[int]uint16{},
		bit:     map[string]map[int]bool{},
	}
	go s.serve()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 关闭监听。
func (s *Server) Close() error { return s.ln.Close() }

// SetWord 预置一个字区/定时器值区的值。
func (s *Server) SetWord(area string, addr int, v uint16) {
	a, ok := mewcodec.Lookup(area)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.word[a.Key]
	if m == nil {
		m = map[int]uint16{}
		s.word[a.Key] = m
	}
	m[addr] = v
}

// Word 读取一个字区/定时器值区的值。
func (s *Server) Word(area string, addr int) uint16 {
	a, ok := mewcodec.Lookup(area)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.word[a.Key][addr]
}

// SetBit 预置一个接点。
func (s *Server) SetBit(area string, contact int, v bool) {
	a, ok := mewcodec.Lookup(area)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.bit[a.Key]
	if m == nil {
		m = map[int]bool{}
		s.bit[a.Key] = m
	}
	m[contact] = v
}

// Bit 读取一个接点。contact 是「字地址×16 + 位号」；T/C 直接用号。
func (s *Server) Bit(area string, contact int) bool {
	a, ok := mewcodec.Lookup(area)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bit[a.Key][contact]
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	logger := slog.Default()
	rd := mewcodec.NewReader(&tcpStream{conn: conn})
	for {
		raw, err := rd.Next()
		if err != nil {
			if err != io.EOF {
				logger.Debug("mewtocol sim read", "err", err)
			}
			return
		}
		resp, ok := s.respond(raw)
		if !ok {
			// 站号不是本机的请求不应答。返回 false 而不是空切片，是为了
			// 让调用点明确「不是错误，是设计如此」。
			continue
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// tcpStream 把 net.Conn 适配成 transport.Stream，供 mewcodec.Reader 使用。
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

// respond 处理一条请求帧。第二个返回值为 false 表示「这条请求不是给本
// 从站的」或「广播，不应答」。
func (s *Server) respond(raw []byte) ([]byte, bool) {
	req, err := mewcodec.ParseRequest(raw)
	if err != nil {
		// 帧不可解析时尽量回一个格式错，但前提是能取出站号——取不出就
		// 只能沉默（真实 PLC 对无法识别的帧也不应答）。
		st, perr := parseStationOf(raw)
		if perr != nil {
			return nil, false
		}
		if st != s.station {
			return nil, false
		}
		out, berr := mewcodec.BuildErrorResponse(st, 0x41)
		if berr != nil {
			return nil, false
		}
		return out, true
	}
	if req.Station != s.station {
		// 站号不等于本机：不应答。广播（FF）也不应答。
		return nil, false
	}

	s.mu.Lock()
	data, code := s.execute(req)
	s.mu.Unlock()

	var (
		out    []byte
		buildE error
	)
	if code != 0 {
		out, buildE = mewcodec.BuildErrorResponse(req.Station, code)
	} else {
		out, buildE = mewcodec.BuildResponse(req.Station, req.Cmd, data)
	}
	if buildE != nil {
		return nil, false
	}
	return out, true
}

// execute 执行一条已解析的请求，返回应答数据段与错误码（0 表示成功）。
//
// 调用方持有 s.mu，因此这里可以直接读写内存表。
func (s *Server) execute(req mewcodec.Request) (string, byte) {
	switch req.Cmd {
	case mewcodec.CmdReadData, mewcodec.CmdReadContactWord,
		mewcodec.CmdReadSetValue, mewcodec.CmdReadElapsed:
		vals, ok := s.readRange(req)
		if !ok {
			return "", 0x66 // 地址越界
		}
		out, err := mewcodec.EncodeWords(vals)
		if err != nil {
			return "", 0x61
		}
		return out, 0

	case mewcodec.CmdWriteData, mewcodec.CmdWriteContactWord,
		mewcodec.CmdWriteSetValue, mewcodec.CmdWriteElapsed:
		if !s.writeRange(req) {
			return "", 0x66
		}
		return "", 0

	case mewcodec.CmdWriteContactOne:
		if len(req.Contacts) != 1 {
			return "", 0x61
		}
		c := req.Contacts[0]
		if !s.setContact(c) {
			return "", 0x66
		}
		return "", 0

	case "RCS":
		if len(req.Contacts) != 1 {
			return "", 0x61
		}
		v := s.contactValue(req.Contacts[0])
		return mewcodec.EncodeContacts([]bool{v}), 0

	case "RCP":
		vals := make([]bool, 0, len(req.Contacts))
		for _, c := range req.Contacts {
			vals = append(vals, s.contactValue(c))
		}
		return mewcodec.EncodeContacts(vals), 0

	case "WCP":
		for _, c := range req.Contacts {
			if !s.setContact(c) {
				return "", 0x66
			}
		}
		return "", 0
	}
	return "", 0x42 // 命令不支持
}

// readRange 读一段连续的字（含接点区按字读），返回大端规范化的字节。
func (s *Server) readRange(req mewcodec.Request) ([]byte, bool) {
	n := req.Words()
	if n <= 0 || n > mewcodec.MaxWords {
		return nil, false
	}
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		addr := req.Start + i
		v, ok := s.wordAt(req.Area, addr)
		if !ok {
			return nil, false
		}
		out = append(out, byte(v>>8), byte(v))
	}
	return out, true
}

// writeRange 写一段连续的字。
func (s *Server) writeRange(req mewcodec.Request) bool {
	n := req.Words()
	if n <= 0 || len(req.Data) != n*2 {
		return false
	}
	for i := 0; i < n; i++ {
		if !s.setWordAt(req.Area, req.Start+i, uint16(req.Data[i*2])<<8|uint16(req.Data[i*2+1])) {
			return false
		}
	}
	return true
}

// wordAt 按区种类取出一个「字」。
//
// 接点区按字读时，一个字就是 16 个接点打包成的位图：第 n 位对应接点号
// word*16+n。T/C 不是位图，一个号就是一个接点，此时「按字读」实际是
// 逐号读。
func (s *Server) wordAt(area mewcodec.Area, addr int) (uint16, bool) {
	if addr < 0 {
		return 0, false
	}
	switch area.Kind {
	case mewcodec.KindIndex:
		// 索引寄存器没有编号：地址恒为 0（id 是 0..1）。
		if addr > 1 {
			return 0, false
		}
		return s.word[area.Key][addr], true
	case mewcodec.KindContact:
		// 不论 X/Y/R/L 还是 T/C，按字读都是「一个字 16 个接点」的位图。
		// 对 T/C 手册也允许按字读，但会按这种每字 16 点的重新编号，
		// 与 T/C 自身的十进制编号是两套体系——驱动因此不用这条路，
		// 但内置从站仍按真实语义实现，避免「从站答应、真 PLC 不答应」。
		var v uint16
		for b := 0; b < 16; b++ {
			if s.bit[area.Key][addr*16+b] {
				v |= 1 << uint(b)
			}
		}
		return v, true
	}
	return s.word[area.Key][addr], true
}

func (s *Server) setWordAt(area mewcodec.Area, addr int, v uint16) bool {
	if addr < 0 {
		return false
	}
	switch area.Kind {
	case mewcodec.KindIndex:
		if addr > 1 {
			return false
		}
		s.ensureWord(area.Key)
		s.word[area.Key][addr] = v
		return true
	case mewcodec.KindContact:
		s.ensureBit(area.Key)
		for b := 0; b < 16; b++ {
			s.bit[area.Key][addr*16+b] = v&(1<<uint(b)) != 0
		}
		return true
	}
	s.ensureWord(area.Key)
	s.word[area.Key][addr] = v
	return true
}

func (s *Server) contactValue(c mewcodec.Contact) bool {
	if c.Area.Code == "T" || c.Area.Code == "C" {
		return s.bit[c.Area.Key][c.Word]
	}
	return s.bit[c.Area.Key][c.Word*16+c.Bit]
}

func (s *Server) setContact(c mewcodec.Contact) bool {
	if c.Word < 0 {
		return false
	}
	s.ensureBit(c.Area.Key)
	if c.Area.Code == "T" || c.Area.Code == "C" {
		s.bit[c.Area.Key][c.Word] = c.Value
		return true
	}
	if c.Bit < 0 || c.Bit > 15 {
		return false
	}
	s.bit[c.Area.Key][c.Word*16+c.Bit] = c.Value
	return true
}

func (s *Server) ensureWord(area string) {
	if s.word[area] == nil {
		s.word[area] = map[int]uint16{}
	}
}

func (s *Server) ensureBit(area string) {
	if s.bit[area] == nil {
		s.bit[area] = map[int]bool{}
	}
}

// parseStationOf 从原始帧里尽量取出站号（用于「请求解析失败但还想过一
// 遍站号过滤」）。取不出就返回错误。
func parseStationOf(raw []byte) (mewcodec.Station, error) {
	if len(raw) < 3 {
		return 0, fmt.Errorf("frame too short")
	}
	return mewcodec.ParseStation(strings.TrimSpace(string(raw[1:3])))
}
