// Package enip 提供 EtherNet/IP（CIP 显式报文）内存从站，用于无硬件自检。
//
// 从站只实现「符号标签 + Read/Write Tag + Multiple Service Packet」这一组
// 最少服务：它验证的是主站的会话、CPF、CIP 路径与错误映射，而不是一个
// 完整的标签仿真器。标签类型与元素宽度由 Set* 方法显式指定，避免从站
// 自己的猜测掩盖主站的类型校验问题。
package enip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	enipcodec "uic/internal/codec/enip"
)

// Server 是 EtherNet/IP 内存从站。
type Server struct {
	ln  net.Listener
	log *slog.Logger

	mu       sync.Mutex
	tags     map[string]*tag
	sessions map[uint32]bool
	// links 记录 Forward Open 建立的 Class 3 连接：连接 ID → 连接序列号。
	links    map[uint32]uint16
	nextID   uint32
	nextConn uint32
	closed   chan struct{}
	// msp 允许测试关掉 Multiple Service Packet，验证主站的退回路径。
	msp bool
	// fragSize 是单帧能带回的最大数据字节数。真机的连接尺寸有限，超过就回
	// 0x06（partial transfer）。测试可以把它调小，用很小的标签也能逼出分片。
	fragSize int
}

type tag struct {
	typ   byte
	count int
	data  []byte // 小端线上字节，count 个元素
}

// NewServer 启动从站。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		log:      slog.Default(),
		tags:     map[string]*tag{},
		sessions: map[uint32]bool{},
		links:    map[uint32]uint16{},
		nextID:   0x1000,
		nextConn: 0x20000000,
		closed:   make(chan struct{}),
		msp:      true,
		fragSize: 400,
	}
	s.installDefaults()
	go s.serve()
	go s.tick()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// SetMultipleSupported 控制是否响应 Multiple Service Packet（0x0A）。
// 关掉后主站必须退回逐条请求，而不是整轮采集失败。
func (s *Server) SetMultipleSupported(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msp = v
}

// SetFragmentSize 设置单帧最大数据字节数（默认 400）。
//
// 调小之后，普通的小标签也会触发 0x06（partial transfer），可以在没有大
// 标签的情况下验证主站的分片路径。
func (s *Server) SetFragmentSize(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fragSize = n
}

// Close 停止监听与自增计数器。
func (s *Server) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return s.ln.Close()
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

// tick 让 counter 每秒自增，便于观察 SSE 与轮询变化。
func (s *Server) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			s.mu.Lock()
			if tg, ok := s.tags["counter"]; ok && len(tg.data) >= 4 {
				v := binary.LittleEndian.Uint32(tg.data) + 1
				binary.LittleEndian.PutUint32(tg.data, v)
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) installDefaults() {
	_ = s.SetDINT("DINT_TAG", 1234)
	_ = s.SetINT("INT_TAG", 5678)
	_ = s.SetREAL("REAL_TAG", 48)
	_ = s.SetBool("BOOL_TAG", true)
	_ = s.SetString("STRING_TAG", "PUMP-01", 82)
	_ = s.SetDINT("COUNTER", 0)
	_ = s.SetArray("DINT_ARRAY", []int32{10, 20, 30, 40})
}

// ---------------------------------------------------------
// 标签写入（测试与自检用）
// ---------------------------------------------------------

// SetTag 直接写入一个标签；data 必须与类型宽度一致。
func (s *Server) SetTag(name string, typ byte, count int, data []byte) error {
	size := enipcodec.TypeSize(typ)
	if typ == enipcodec.TypeSTRING {
		if count != 1 || len(data) < 4 {
			return errors.New("enip sim: STRING tag needs count 1 and >= 4 bytes")
		}
	} else {
		if size <= 0 {
			return fmt.Errorf("enip sim: type %s not supported", enipcodec.TypeText(typ))
		}
		if count < 1 || len(data) < count*size {
			return fmt.Errorf("enip sim: %s tag needs %d bytes", enipcodec.TypeText(typ), count*size)
		}
		data = data[:count*size]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags[strings.ToLower(name)] = &tag{typ: typ, count: count, data: append([]byte(nil), data...)}
	return nil
}

// SetDINT 写入一个 DINT 标签。
func (s *Server) SetDINT(name string, v int32) error {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return s.SetTag(name, enipcodec.TypeDINT, 1, b)
}

// SetINT 写入一个 INT 标签。
func (s *Server) SetINT(name string, v int16) error {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, uint16(v))
	return s.SetTag(name, enipcodec.TypeINT, 1, b)
}

// SetREAL 写入一个 REAL 标签。
func (s *Server) SetREAL(name string, v float32) error {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
	return s.SetTag(name, enipcodec.TypeREAL, 1, b)
}

// SetBool 写入一个 BOOL 标签。
func (s *Server) SetBool(name string, v bool) error {
	b := []byte{0}
	if v {
		b[0] = 1
	}
	return s.SetTag(name, enipcodec.TypeBOOL, 1, b)
}

// SetString 写入一个 STRING 标签。
func (s *Server) SetString(name, v string, capacity int) error {
	b, err := enipcodec.EncodeString(v, capacity)
	if err != nil {
		return err
	}
	return s.SetTag(name, enipcodec.TypeSTRING, 1, b)
}

// SetArray 写入一个 DINT 数组标签。
func (s *Server) SetArray(name string, values []int32) error {
	b := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(b[i*4:], uint32(v))
	}
	return s.SetTag(name, enipcodec.TypeDINT, len(values), b)
}

// Tag 返回标签副本。
func (s *Server) Tag(name string) (typ byte, count int, data []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tags[strings.ToLower(name)]
	if !ok {
		return 0, 0, nil, false
	}
	return t.typ, t.count, append([]byte(nil), t.data...), true
}

// ---------------------------------------------------------
// 连接处理
// ---------------------------------------------------------

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	for {
		head := make([]byte, enipcodec.HeaderLen)
		if _, err := io.ReadFull(c, head); err != nil {
			return
		}
		h, err := enipcodec.ParseHeader(head)
		if err != nil {
			return
		}
		body := make([]byte, 0)
		if h.Length > 0 {
			body = make([]byte, h.Length)
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
		}
		resp, ok := s.respond(h, body)
		if !ok {
			continue
		}
		if _, err := c.Write(resp); err != nil {
			return
		}
	}
}

func (s *Server) respond(h enipcodec.Header, body []byte) ([]byte, bool) {
	switch h.Command {
	case enipcodec.CmdRegisterSession:
		version, options, err := enipcodec.ParseRegisterSession(body)
		if err != nil || version != 1 {
			return mustPacket(enipcodec.CmdRegisterSession, 0, 0x69, h.Context, nil), true
		}
		s.mu.Lock()
		s.nextID++
		sid := s.nextID
		s.sessions[sid] = true
		s.mu.Unlock()
		_ = options
		return mustPacket(enipcodec.CmdRegisterSession, sid, 0, h.Context,
			enipcodec.BuildRegisterSession(1)), true

	case enipcodec.CmdUnregister:
		s.mu.Lock()
		delete(s.sessions, h.Session)
		s.mu.Unlock()
		return nil, false

	case enipcodec.CmdSendRRData:
		s.mu.Lock()
		valid := s.sessions[h.Session]
		s.mu.Unlock()
		if !valid {
			return mustPacket(enipcodec.CmdSendRRData, h.Session, 0x64, h.Context, nil), true
		}
		cip, err := enipcodec.ParseSendRRData(body)
		if err != nil {
			return mustPacket(enipcodec.CmdSendRRData, h.Session, 0x03, h.Context, nil), true
		}
		reply := s.dispatch(cip)
		payload, err := enipcodec.BuildSendRRData(reply)
		if err != nil {
			return mustPacket(enipcodec.CmdSendRRData, h.Session, 0x03, h.Context, nil), true
		}
		return mustPacket(enipcodec.CmdSendRRData, h.Session, 0, h.Context, payload), true

	case enipcodec.CmdSendUnitData:
		s.mu.Lock()
		valid := s.sessions[h.Session]
		s.mu.Unlock()
		if !valid {
			return mustPacket(enipcodec.CmdSendUnitData, h.Session, 0x64, h.Context, nil), true
		}
		connID, seq, cip, err := enipcodec.ParseSendUnitData(body)
		if err != nil {
			return mustPacket(enipcodec.CmdSendUnitData, h.Session, 0x03, h.Context, nil), true
		}
		s.mu.Lock()
		_, linked := s.links[connID]
		s.mu.Unlock()
		var reply []byte
		if !linked {
			// 连接不存在（从未建立或被 Forward Close 关掉）：回 CIP 0x07
			// （connection lost），让主站知道该重开连接。
			svc := byte(0)
			if req, perr := enipcodec.ParseRequest(cip); perr == nil {
				svc = req.Service
			}
			reply = mustReply(svc, 0x07, nil, nil)
		} else {
			reply = s.dispatch(cip)
		}
		payload, err := enipcodec.BuildSendUnitData(connID, seq, reply)
		if err != nil {
			return mustPacket(enipcodec.CmdSendUnitData, h.Session, 0x03, h.Context, nil), true
		}
		return mustPacket(enipcodec.CmdSendUnitData, h.Session, 0, h.Context, payload), true
	}
	return mustPacket(h.Command, h.Session, 0x01, h.Context, nil), true
}

func mustPacket(cmd uint16, session, status uint32, ctx [8]byte, payload []byte) []byte {
	b, err := enipcodec.BuildPacket(cmd, session, status, ctx, payload)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------
// CIP 分发
// ---------------------------------------------------------

func (s *Server) dispatch(cip []byte) []byte {
	req, err := enipcodec.ParseRequest(cip)
	if err != nil {
		return mustReply(0, 0x03, nil, nil)
	}
	switch req.Service {
	case enipcodec.ServiceReadTag:
		return s.readTag(req)
	case enipcodec.ServiceWriteTag:
		return s.writeTag(req)
	case enipcodec.ServiceReadFrag:
		return s.readFrag(req)
	case enipcodec.ServiceWriteFrag:
		return s.writeFrag(req)
	case enipcodec.ServiceForwardOpen, enipcodec.ServiceLargeForwardOpen:
		return s.forwardOpen(req)
	case enipcodec.ServiceForwardClose:
		return s.forwardClose(req)
	case enipcodec.ServiceMultiple:
		s.mu.Lock()
		ok := s.msp
		s.mu.Unlock()
		if !ok {
			return mustReply(req.Service, 0x08, nil, nil)
		}
		return s.multiple(req)
	}
	return mustReply(req.Service, 0x08, nil, nil)
}

func mustReply(service, status byte, extra, data []byte) []byte {
	b, err := enipcodec.BuildReply(service, status, extra, data)
	if err != nil {
		return nil
	}
	return b
}

func (s *Server) readTag(req enipcodec.Request) []byte {
	name, err := enipcodec.ParseSymbolicPath(req.Path)
	if err != nil {
		return mustReply(req.Service, 0x04, nil, nil)
	}
	if len(req.Data) < 2 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	count := int(binary.LittleEndian.Uint16(req.Data))
	if count < 1 {
		return mustReply(req.Service, 0x03, nil, nil)
	}

	s.mu.Lock()
	t, ok := s.tags[strings.ToLower(name)]
	if !ok {
		s.mu.Unlock()
		return mustReply(req.Service, 0x05, nil, nil)
	}
	if count > t.count {
		s.mu.Unlock()
		return mustReply(req.Service, 0x15, nil, nil)
	}
	data := append([]byte(nil), t.data...)
	typ := t.typ
	s.mu.Unlock()

	n := count
	if typ == enipcodec.TypeSTRING {
		// STRING 不是定长元素；按 1 个元素返回完整结构。
		n = 1
	} else {
		size := enipcodec.TypeSize(typ)
		if size <= 0 || len(data) < n*size {
			return mustReply(req.Service, 0x03, nil, nil)
		}
		data = data[:n*size]
	}

	// 单帧放不下就只回前 fragSize 字节并置 0x06，与真机的 partial transfer
	// 行为一致；主站随后会用 0x52 从偏移处继续读。
	limit := s.fragmentSize()
	status := byte(0)
	if len(data) > limit {
		data = data[:limit]
		status = 0x06
	}
	// 应答体是「类型 + 数据」，没有元素数字段。
	body := make([]byte, 2, 2+len(data))
	binary.LittleEndian.PutUint16(body[0:], uint16(typ))
	body = append(body, data...)
	return mustReply(req.Service, status, nil, body)
}

func (s *Server) fragmentSize() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fragSize
}

// readFrag 处理 Read Tag Fragmented（0x52）。
func (s *Server) readFrag(req enipcodec.Request) []byte {
	name, err := enipcodec.ParseSymbolicPath(req.Path)
	if err != nil {
		return mustReply(req.Service, 0x04, nil, nil)
	}
	if len(req.Data) < 6 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	count := int(binary.LittleEndian.Uint16(req.Data[0:]))
	offset := int(binary.LittleEndian.Uint32(req.Data[2:]))
	if count < 1 || offset < 0 {
		return mustReply(req.Service, 0x03, nil, nil)
	}

	s.mu.Lock()
	t, ok := s.tags[strings.ToLower(name)]
	if !ok {
		s.mu.Unlock()
		return mustReply(req.Service, 0x05, nil, nil)
	}
	typ := t.typ
	if typ == enipcodec.TypeSTRING {
		if count != 1 {
			s.mu.Unlock()
			return mustReply(req.Service, 0x15, nil, nil)
		}
	} else if count > t.count {
		s.mu.Unlock()
		return mustReply(req.Service, 0x15, nil, nil)
	}
	data := append([]byte(nil), t.data...)
	if typ != enipcodec.TypeSTRING {
		size := enipcodec.TypeSize(typ)
		if size <= 0 {
			s.mu.Unlock()
			return mustReply(req.Service, 0x03, nil, nil)
		}
		data = data[:count*size]
	}
	limit := s.fragSize
	s.mu.Unlock()

	if offset > len(data) {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	part := data[offset:]
	status := byte(0)
	if len(part) > limit {
		part = part[:limit]
		status = 0x06
	}
	body := make([]byte, 2, 2+len(part))
	binary.LittleEndian.PutUint16(body[0:], uint16(typ))
	body = append(body, part...)
	return mustReply(req.Service, status, nil, body)
}

// writeFrag 处理 Write Tag Fragmented（0x53）。
//
// count 是整个标签的总元素数，data 只带本片字节；写应答没有数据区。
func (s *Server) writeFrag(req enipcodec.Request) []byte {
	name, err := enipcodec.ParseSymbolicPath(req.Path)
	if err != nil {
		return mustReply(req.Service, 0x04, nil, nil)
	}
	if len(req.Data) < 8 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	typ := byte(binary.LittleEndian.Uint16(req.Data[0:]))
	count := int(binary.LittleEndian.Uint16(req.Data[2:]))
	offset := int(binary.LittleEndian.Uint32(req.Data[4:]))
	payload := req.Data[8:]
	if count < 1 || offset < 0 || len(payload) == 0 {
		return mustReply(req.Service, 0x03, nil, nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tags[strings.ToLower(name)]
	if !ok {
		return mustReply(req.Service, 0x05, nil, nil)
	}
	if typ != t.typ {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	if typ == enipcodec.TypeSTRING {
		if count != 1 {
			return mustReply(req.Service, 0x15, nil, nil)
		}
	} else if count > t.count {
		return mustReply(req.Service, 0x15, nil, nil)
	}
	if offset+len(payload) > len(t.data) {
		return mustReply(req.Service, 0x13, nil, nil)
	}
	copy(t.data[offset:], payload)
	// 与 readFrag 一致：非最后一片回 0x06（partial transfer），真机只有末片
	// 回 0x00。这样内置从站也走真实的分片应答路径，不会用「恒回 0」掩盖
	// 驱动把 0x06 当失败的缺陷。
	status := byte(0x06)
	if offset+len(payload) == len(t.data) {
		status = 0
	}
	return mustReply(req.Service, status, nil, nil)
}

// forwardOpen 处理 Forward Open / Large Forward Open，登记一条 Class 3 连接。
func (s *Server) forwardOpen(req enipcodec.Request) []byte {
	if len(req.Data) < 36 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	ot := binary.LittleEndian.Uint32(req.Data[2:])
	to := binary.LittleEndian.Uint32(req.Data[6:])
	serial := binary.LittleEndian.Uint16(req.Data[10:])
	vendor := binary.LittleEndian.Uint16(req.Data[12:])
	orig := binary.LittleEndian.Uint32(req.Data[14:])

	s.mu.Lock()
	if ot == 0 {
		ot = s.newConnID()
	}
	if to == 0 || to == 0xFFFFFFFF {
		to = s.newConnID()
	}
	s.links[ot] = serial
	s.mu.Unlock()

	body := make([]byte, 26)
	binary.LittleEndian.PutUint32(body[0:], ot)
	binary.LittleEndian.PutUint32(body[4:], to)
	binary.LittleEndian.PutUint16(body[8:], serial)
	binary.LittleEndian.PutUint16(body[10:], vendor)
	binary.LittleEndian.PutUint32(body[12:], orig)
	return mustReply(req.Service, 0, nil, body)
}

// forwardClose 处理 Forward Close，按连接序列号释放连接。
func (s *Server) forwardClose(req enipcodec.Request) []byte {
	if len(req.Data) < 10 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	serial := binary.LittleEndian.Uint16(req.Data[2:])
	s.mu.Lock()
	for id, sn := range s.links {
		if sn == serial {
			delete(s.links, id)
		}
	}
	s.mu.Unlock()
	return mustReply(req.Service, 0, nil, nil)
}

// newConnID 分配连接 ID。调用方必须持有 s.mu。
func (s *Server) newConnID() uint32 {
	s.nextConn++
	return s.nextConn
}

func (s *Server) writeTag(req enipcodec.Request) []byte {
	name, err := enipcodec.ParseSymbolicPath(req.Path)
	if err != nil {
		return mustReply(req.Service, 0x04, nil, nil)
	}
	if len(req.Data) < 4 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	typ := byte(binary.LittleEndian.Uint16(req.Data[0:]))
	count := int(binary.LittleEndian.Uint16(req.Data[2:]))
	data := req.Data[4:]
	if count < 1 {
		return mustReply(req.Service, 0x03, nil, nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tags[strings.ToLower(name)]
	if !ok {
		return mustReply(req.Service, 0x05, nil, nil)
	}
	if typ != t.typ {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	if count > t.count {
		return mustReply(req.Service, 0x15, nil, nil)
	}
	if typ == enipcodec.TypeSTRING {
		if count != 1 || len(data) < 4 || len(data) > len(t.data) {
			return mustReply(req.Service, 0x13, nil, nil)
		}
		copy(t.data, data)
	} else {
		size := enipcodec.TypeSize(typ)
		if size <= 0 || len(data) < count*size {
			return mustReply(req.Service, 0x13, nil, nil)
		}
		copy(t.data, data[:count*size])
	}
	// 写应答没有数据区，只有状态。
	return mustReply(req.Service, 0, nil, nil)
}

func (s *Server) multiple(req enipcodec.Request) []byte {
	if len(req.Data) < 2 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	count := int(binary.LittleEndian.Uint16(req.Data))
	if count < 1 || count > 32 {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	tableEnd := 2 + 2*count
	if tableEnd > len(req.Data) {
		return mustReply(req.Service, 0x03, nil, nil)
	}
	// 偏移相对数据区起点（条数字段在偏移 0），第一条至少是 2+2n。
	// 这是 1756-PM020/pycomm3 的约定，与 codec 的 BuildMultiple 一致。
	offsets := make([]int, count)
	for i := 0; i < count; i++ {
		offsets[i] = int(binary.LittleEndian.Uint16(req.Data[2+2*i:]))
		if offsets[i] < tableEnd || offsets[i] > len(req.Data) {
			return mustReply(req.Service, 0x03, nil, nil)
		}
		if i > 0 && offsets[i] < offsets[i-1] {
			return mustReply(req.Service, 0x03, nil, nil)
		}
	}
	replies := make([][]byte, count)
	for i := 0; i < count; i++ {
		start := offsets[i]
		end := len(req.Data)
		if i+1 < count {
			end = offsets[i+1]
		}
		if end < start || end > len(req.Data) {
			return mustReply(req.Service, 0x03, nil, nil)
		}
		replies[i] = s.dispatch(req.Data[start:end])
	}
	bodyLen := 2 + 2*count
	for _, r := range replies {
		bodyLen += len(r)
	}
	body := make([]byte, 2+2*count, bodyLen)
	binary.LittleEndian.PutUint16(body, uint16(count))
	off := 2 + 2*count
	for i, r := range replies {
		binary.LittleEndian.PutUint16(body[2+2*i:], uint16(off))
		off += len(r)
	}
	for _, r := range replies {
		body = append(body, r...)
	}
	return mustReply(req.Service, 0, nil, body)
}
