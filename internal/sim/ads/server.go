// Package ads 是内置的 ADS/AMS 仿真服务端（测试/演示用）。
//
// 它实现 AMS/TCP（默认 48898）上的七类命令：ReadDeviceInfo、Read、
// Write、ReadWrite、ReadState、WriteControl 与句柄释放，足以让主站驱动
// 走完「变量名 → 句柄 → 值」的完整链路。
//
// 与真实 PLC 刻意保留的差异：
//
//  1. 没有符号表上传（0xF004）与通知订阅（AddDeviceNotification）：
//     本驱动不实现它们，仿真也不假装支持，未知 IndexGroup 一律回
//     invalid group (0x0702)。
//  2. 变量表是扁平的、预先加载的，没有 PLC 的编译周期；每个数组元素
//     单独占一个符号名（`MAIN.arr[0]`），这与真实符号表一致。
//  3. 句柄全局编号而不是按连接隔离：测试要能一调用就「让所有句柄失效」，
//     模拟 PLC 重启。真实设备重启后旧句柄同样全部失效。
//
// 句柄失效的行为是：按句柄读/写返回 invalid offset (0x0703)；驱动据此
// 清缓存、重新按名字解析一次。这条路径不模拟的话，现场「PLC 重启后
// 所有点位红一轮」的问题在自检里永远复现不出来。
package ads

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strings"
	"sync"

	adscodec "uic/internal/codec/ads"
)

const (
	// DefaultNetID 是仿真设备默认的 AMS NetId。
	DefaultNetID = "127.0.0.1.1.1"
	// DefaultAMSPort 是默认的 AMS 端口（第一个 PLC 运行时）。
	DefaultAMSPort uint16 = 851
	// DefaultStrLen 是 STRING(80) 的字节数：80 个字符 + 结尾 NUL。
	DefaultStrLen = 81
)

// variable 是一个 PLC 符号的当前值。data 是线上原始字节（小端）。
type variable struct {
	name     string
	typ      string
	data     []byte
	writable bool
}

// Options 是可调参数。
type Options struct {
	// NetID 是设备 NetId，空值时用 DefaultNetID。
	NetID string
	// Port 是 AMS 端口，0 时用 DefaultAMSPort。
	Port uint16
	// Name 是 ReadDeviceInfo 返回的设备名。
	Name string
}

// Server 是内置 ADS 服务端。
type Server struct {
	ln    net.Listener
	netid adscodec.NetID
	port  uint16
	name  string

	mu       sync.Mutex
	vars     map[string]*variable
	order    []string
	handles  map[uint32]string
	nextHnd  uint32
	state    adscodec.State
	requests int
	writes   int
	closed   bool

	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewServer 在 addr（如 "127.0.0.1:0"）上监听 AMS/TCP。
func NewServer(addr string) (*Server, error) { return NewServerWithOptions(addr, Options{}) }

// NewServerWithOptions 带参数启动。
func NewServerWithOptions(addr string, opt Options) (*Server, error) {
	netid, err := adscodec.ParseNetID(opt.NetID)
	if opt.NetID == "" {
		netid = adscodec.MustParseNetID(DefaultNetID)
	} else if err != nil {
		return nil, fmt.Errorf("ads sim: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ads sim: listen %q: %w", addr, err)
	}
	port := opt.Port
	if port == 0 {
		port = DefaultAMSPort
	}
	name := opt.Name
	if name == "" {
		name = "UIC ADS sim"
	}
	s := &Server{
		ln:      ln,
		netid:   netid,
		port:    port,
		name:    name,
		vars:    map[string]*variable{},
		handles: map[uint32]string{},
		nextHnd: 0x00010001,
		state:   adscodec.StateRun,
		conns:   map[net.Conn]struct{}{},
	}
	s.loadDefaults()
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// NetID 返回设备 NetId。
func (s *Server) NetID() adscodec.NetID { return s.netid }

// Port 返回 AMS 端口。
func (s *Server) Port() uint16 { return s.port }

// Requests 返回收到的合法请求数。
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Writes 返回成功写入的变量次数。
func (s *Server) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// State 返回当前运行时状态。
func (s *Server) State() adscodec.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// InvalidateHandles 让所有已分配的句柄失效（模拟 PLC 重启）。
func (s *Server) InvalidateHandles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles = map[uint32]string{}
}

// Get 返回一个变量的原始字节。
func (s *Server) Get(name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vars[name]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v.data...), true
}

// SetValue 直接改一个变量的原始字节（测试夹具用；不检查可写标记）。
func (s *Server) SetValue(name string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vars[name]
	if !ok {
		return fmt.Errorf("ads sim: variable %q not found", name)
	}
	if len(raw) != len(v.data) {
		return fmt.Errorf("ads sim: variable %q is %d bytes, got %d", name, len(v.data), len(raw))
	}
	copy(v.data, raw)
	return nil
}

// SetWritable 调整变量的可写标记。
func (s *Server) SetWritable(name string, writable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vars[name]
	if !ok {
		return fmt.Errorf("ads sim: variable %q not found", name)
	}
	v.writable = writable
	return nil
}

// Variables 返回按名字排序的变量名与类型（自检输出用）。
func (s *Server) Variables() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.order))
	for _, name := range s.order {
		v := s.vars[name]
		out = append(out, fmt.Sprintf("%s %s", name, v.typ))
	}
	return out
}

// Close 停止服务并关闭所有连接。可重复调用。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for nc := range s.conns {
		conns = append(conns, nc)
	}
	s.mu.Unlock()

	_ = s.ln.Close()
	for _, nc := range conns {
		_ = nc.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(nc)
			s.mu.Lock()
			delete(s.conns, nc)
			s.mu.Unlock()
			_ = nc.Close()
		}()
	}
}

func (s *Server) serveConn(nc net.Conn) {
	for {
		f, err := readFrame(nc)
		if err != nil {
			return
		}
		resp, ok := s.handle(f)
		if !ok {
			continue
		}
		raw, err := resp.Encode()
		if err != nil {
			return
		}
		if _, err := nc.Write(raw); err != nil {
			return
		}
	}
}

func readFrame(r io.Reader) (adscodec.Frame, error) {
	var hdr [adscodec.TCPHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return adscodec.Frame{}, err
	}
	n, err := adscodec.FrameLen(hdr[:])
	if err != nil {
		return adscodec.Frame{}, err
	}
	buf := make([]byte, n)
	copy(buf, hdr[:])
	if _, err := io.ReadFull(r, buf[adscodec.TCPHeaderLen:]); err != nil {
		return adscodec.Frame{}, err
	}
	return adscodec.Decode(buf)
}

// handle 处理一条请求，返回应答帧。ok=false 表示不需要应答（通知类）。
func (s *Server) handle(req adscodec.Frame) (adscodec.Frame, bool) {
	if !req.IsResponse() && req.Command == adscodec.CmdDeviceNotification {
		return adscodec.Frame{}, false
	}
	resp := adscodec.Frame{
		Target:  req.Source,
		Source:  req.Target,
		Command: req.Command,
		Flags:   adscodec.FlagADS | adscodec.FlagResponse,
		Invoke:  req.Invoke,
	}
	if req.Target.NetID != s.netid {
		resp.Error = adscodec.CodeTargetMachineNotFound
		return resp, true
	}
	if req.Target.Port != s.port {
		resp.Error = adscodec.CodeTargetPortNotFound
		return resp, true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++

	switch req.Command {
	case adscodec.CmdReadDeviceInfo:
		resp.Data = s.deviceInfo()
	case adscodec.CmdReadState:
		resp.Data = s.readState()
	case adscodec.CmdWriteControl:
		resp.Data = s.writeControl(req.Data)
	case adscodec.CmdRead:
		resp.Data = s.read(req.Data)
	case adscodec.CmdWrite:
		resp.Data = s.write(req.Data)
	case adscodec.CmdReadWrite:
		resp.Data = s.readWrite(req.Data)
	default:
		resp.Error = adscodec.CodeUnknownCommandID
	}
	return resp, true
}

func (s *Server) deviceInfo() []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:4], 0)
	b[4], b[5] = 3, 1
	binary.LittleEndian.PutUint16(b[6:8], 4024)
	copy(b[8:24], s.name)
	return b
}

func (s *Server) readState() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[4:6], uint16(s.state))
	binary.LittleEndian.PutUint16(b[6:8], 0)
	return b
}

func (s *Server) writeControl(p []byte) []byte {
	if len(p) < 8 {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	state := adscodec.State(binary.LittleEndian.Uint16(p[0:2]))
	switch state {
	case adscodec.StateRun, adscodec.StateStop, adscodec.StateReset:
		s.state = state
		return resultOnly(adscodec.CodeOK)
	}
	return resultOnly(adscodec.CodeInvalidState)
}

func resultOnly(code adscodec.Code) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(code))
	return b
}

// read 处理 Read：IG=0xF005 按句柄读；其余 IndexGroup 一律 invalid group。
func (s *Server) read(p []byte) []byte {
	if len(p) < 12 {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	group := binary.LittleEndian.Uint32(p[0:4])
	offset := binary.LittleEndian.Uint32(p[4:8])
	length := binary.LittleEndian.Uint32(p[8:12])
	if group != adscodec.IGSymValueByHandle {
		return resultOnly(adscodec.CodeInvalidGroup)
	}
	name, ok := s.handles[offset]
	if !ok {
		return resultOnly(adscodec.CodeInvalidOffset)
	}
	v := s.vars[name]
	if v == nil || int(length) != len(v.data) {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	b := make([]byte, 8+len(v.data))
	binary.LittleEndian.PutUint32(b[0:4], 0)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(v.data)))
	copy(b[8:], v.data)
	return b
}

// write 处理 Write：0xF005 写值，0xF006 释放句柄。
func (s *Server) write(p []byte) []byte {
	if len(p) < 12 {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	group := binary.LittleEndian.Uint32(p[0:4])
	offset := binary.LittleEndian.Uint32(p[4:8])
	length := int(binary.LittleEndian.Uint32(p[8:12]))
	if length < 0 || 12+length > len(p) {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	data := p[12 : 12+length]

	switch group {
	case adscodec.IGSymReleaseHandle:
		if length != 4 {
			return resultOnly(adscodec.CodeInvalidSize)
		}
		// 释放不存在的句柄是幂等的：真实设备也可能在会话重建后收到
		// 上一会话的释放请求。
		delete(s.handles, binary.LittleEndian.Uint32(data))
		return resultOnly(adscodec.CodeOK)
	case adscodec.IGSymValueByHandle:
		name, ok := s.handles[offset]
		if !ok {
			return resultOnly(adscodec.CodeInvalidOffset)
		}
		v := s.vars[name]
		if v == nil {
			return resultOnly(adscodec.CodeInvalidOffset)
		}
		if !v.writable {
			return resultOnly(adscodec.CodeInvalidAccess)
		}
		if len(data) != len(v.data) {
			return resultOnly(adscodec.CodeInvalidSize)
		}
		copy(v.data, data)
		s.writes++
		return resultOnly(adscodec.CodeOK)
	}
	return resultOnly(adscodec.CodeInvalidGroup)
}

// readWrite 处理 ReadWrite：本实现只支持「按名字取句柄」。
func (s *Server) readWrite(p []byte) []byte {
	if len(p) < 16 {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	group := binary.LittleEndian.Uint32(p[0:4])
	readLen := binary.LittleEndian.Uint32(p[8:12])
	writeLen := int(binary.LittleEndian.Uint32(p[12:16]))
	if writeLen < 0 || 16+writeLen > len(p) {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	if group != adscodec.IGSymHandleByName {
		return resultOnly(adscodec.CodeInvalidGroup)
	}
	name := strings.TrimRight(string(p[16:16+writeLen]), "\x00")
	if _, ok := s.vars[name]; !ok {
		return resultOnly(adscodec.CodeSymbolNotFound)
	}
	if readLen != 4 {
		return resultOnly(adscodec.CodeInvalidSize)
	}
	h := s.nextHnd
	s.nextHnd++
	if s.nextHnd == 0 {
		s.nextHnd = 1
	}
	s.handles[h] = name

	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], 0)
	binary.LittleEndian.PutUint32(b[4:8], 4)
	b = append(b, byte(h), byte(h>>8), byte(h>>16), byte(h>>24))
	return b
}

// ---------------------------------------------------------------------------
// 默认变量表
// ---------------------------------------------------------------------------

func (s *Server) loadDefaults() {
	add := func(name, typ string, data []byte, writable bool) {
		s.vars[name] = &variable{name: name, typ: typ, data: data, writable: writable}
		s.order = append(s.order, name)
	}
	int16b := func(v int16) []byte {
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(v))
		return b
	}
	f32b := func(v float32) []byte {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, math.Float32bits(v))
		return b
	}
	f64b := func(v float64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, math.Float64bits(v))
		return b
	}
	strb := func(v string) []byte {
		b := make([]byte, DefaultStrLen)
		copy(b, v)
		return b
	}

	add("MAIN.nCounter", "INT16", int16b(1234), false)
	add("MAIN.nSetpoint", "INT16", int16b(500), true)
	add("MAIN.fTemperature", "REAL", f32b(23.5), false)
	add("MAIN.fSpeed", "LREAL", f64b(1450.75), true)
	add("MAIN.bEnable", "BOOL", []byte{1}, false)
	add("MAIN.bRun", "BOOL", []byte{0}, true)
	add("MAIN.sRecipe", "STRING(80)", strb("batch-42"), false)
	add("MAIN.sName", "STRING(80)", strb("uic-sim"), true)
	for i, v := range []int16{10, 20, 30, 40} {
		add(fmt.Sprintf("MAIN.arr[%d]", i), "INT16", int16b(v), i == 2)
	}
	add("MAIN.arrBytes", "BYTE[4]", []byte{1, 2, 3, 4}, false)
	sort.Strings(s.order)
}
