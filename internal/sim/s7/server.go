// Package s7（sim）实现一个最小 S7comm 从站，用于自测、演示与驱动一致性
// 测试。它复用 codec/s7 的编解码函数，避免「主站与从站各写一套」导致
// 的语义漂移——尤其是长度单位（位 / 字节）这种最容易分叉的地方。
//
// 边界：只实现连接、Setup、Read Var、Write Var 四种 PDU。不含报警、
// 块上传、符号表、时间同步。它存在的目的是让主站在没有真实 PLC 的机器
// 上也能跑通完整链路，而不是模拟一台真 CPU。
package s7

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"uic/internal/codec/s7"
)

// Server 是一个内存区域模型的 S7 从站。
type Server struct {
	mu       sync.RWMutex
	ln       net.Listener
	pduLen   int
	mem      map[memKey][]byte
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   bool
	reqCount int
}

type memKey struct {
	area byte
	db   int
}

// NewServer 创建从站；addr 为 "127.0.0.1:0" 时自动分配端口。
// pduLen 是 Setup 阶段返回给主站的协商值（常见 240 / 480 / 960）。
func NewServer(addr string, pduLen int) (*Server, error) {
	if pduLen <= 0 {
		pduLen = 480
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:     ln,
		pduLen: pduLen,
		mem:    map[memKey][]byte{},
		conns:  map[net.Conn]struct{}{},
	}
	// 默认给 M / I / Q / DB1 各 1KB，避免测试每次都要先扩容。
	s.Ensure(s7.AreaMerker, 0, 1024)
	s.Ensure(s7.AreaInput, 0, 1024)
	s.Ensure(s7.AreaOutput, 0, 1024)
	s.Ensure(s7.AreaDB, 1, 1024)

	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// PDULen 返回协商的 PDU 长度。
func (s *Server) PDULen() int { return s.pduLen }

// Close 停止监听并等待连接退出。
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
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// Ensure 把某个区域的长度至少扩到 size。
func (s *Server) Ensure(area byte, db, size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memKey{area: area, db: db}
	if len(s.mem[k]) >= size {
		return
	}
	buf := make([]byte, size)
	copy(buf, s.mem[k])
	s.mem[k] = buf
}

// SetBytes 写入一段字节（自动扩容）。
func (s *Server) SetBytes(area byte, db, offset int, data []byte) {
	if offset < 0 {
		return
	}
	s.Ensure(area, db, offset+len(data))
	s.mu.Lock()
	defer s.mu.Unlock()
	copy(s.mem[memKey{area: area, db: db}][offset:], data)
}

// Bytes 读出一段字节。
func (s *Server) Bytes(area byte, db, offset, n int) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.mem[memKey{area: area, db: db}]
	if offset < 0 || offset+n > len(buf) {
		return nil
	}
	return append([]byte(nil), buf[offset:offset+n]...)
}

// SetBit 设置某字节的某一位。
func (s *Server) SetBit(area byte, db, offset, bit int, on bool) {
	if bit < 0 || bit > 7 {
		return
	}
	s.Ensure(area, db, offset+1)
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.mem[memKey{area: area, db: db}]
	mask := byte(1) << uint(bit)
	if on {
		buf[offset] |= mask
	} else {
		buf[offset] &^= mask
	}
}

// Bit 读取某字节的某一位。
func (s *Server) Bit(area byte, db, offset, bit int) bool {
	b := s.Bytes(area, db, offset, 1)
	if len(b) == 0 || bit < 0 || bit > 7 {
		return false
	}
	return b[0]&(1<<uint(bit)) != 0
}

// RequestCount 返回累计收到的 S7 业务请求数（不含握手）。
// 用于断言「多个点位被合并进一次事务」。
func (s *Server) RequestCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reqCount
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
		go func() {
			defer s.wg.Done()
			s.serve(c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			_ = c.Close()
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
	for {
		frame, err := readTPKT(c)
		if err != nil {
			return
		}
		payload, err := s7.ParseTPKT(frame)
		if err != nil || len(payload) < 2 {
			return
		}
		switch payload[1] {
		case s7.COTPCR:
			if _, err := c.Write(ccFrame()); err != nil {
				return
			}
		case s7.COTPDT:
			if len(payload) < 3 {
				return
			}
			resp, ok := s.handlePDU(payload[3:])
			if !ok {
				return
			}
			if _, err := c.Write(s7.WrapDT(resp)); err != nil {
				return
			}
		default:
			return
		}
	}
}

func readTPKT(c net.Conn) ([]byte, error) {
	head := make([]byte, 4)
	if _, err := readFull(c, head); err != nil {
		return nil, err
	}
	if head[0] != s7.TPKTVersion {
		return nil, fmt.Errorf("sim-s7: bad tpkt version %d", head[0])
	}
	n := int(binary.BigEndian.Uint16(head[2:4]))
	if n < 4 || n > 8192 {
		return nil, fmt.Errorf("sim-s7: bad tpkt length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		if err != nil {
			return got, err
		}
		got += n
	}
	return got, nil
}

// ccFrame 是连接确认。refs 与实际连接无关，只要类型是 CC 主站就会接受。
func ccFrame() []byte {
	payload := []byte{
		0x12, s7.COTPCC,
		0x00, 0x01, 0x00, 0x01, // refs
		0x00,
		0xC0, 0x02, 0x01, 0x02,
		0xC1, 0x02, 0x01, 0x00,
		0xC2, 0x02, 0x01, 0x00,
	}
	return s7.TPKT(payload)
}

func (s *Server) handlePDU(raw []byte) ([]byte, bool) {
	if len(raw) < 10 || raw[0] != s7.S7ProtoID {
		return nil, false
	}
	ref := binary.BigEndian.Uint16(raw[4:6])
	paramLen := int(binary.BigEndian.Uint16(raw[6:8]))
	dataLen := int(binary.BigEndian.Uint16(raw[8:10]))
	if len(raw) < 10+paramLen+dataLen {
		return nil, false
	}
	param := raw[10 : 10+paramLen]
	data := raw[10+paramLen : 10+paramLen+dataLen]
	if len(param) < 1 {
		return nil, false
	}

	switch param[0] {
	case s7.FuncSetup:
		// 回显对端请求的长度，但不超过本机配置：这正是协商的含义。
		want := s.pduLen
		if len(param) >= 8 {
			if n := int(binary.BigEndian.Uint16(param[6:8])); n < want {
				want = n
			}
		}
		p := []byte{s7.FuncSetup, 0x00, 0x00, 0x01, 0x00, 0x01,
			byte(want >> 8), byte(want)}
		return s7.BuildPDU(s7.ROSCTRAckData, ref, p, nil), true

	case s7.FuncRead:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleRead(ref, param), true

	case s7.FuncWrite:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleWrite(ref, param, data), true
	}
	return nil, false
}

func (s *Server) handleRead(ref uint16, param []byte) []byte {
	if len(param) < 2 {
		return s7.BuildPDU(s7.ROSCTRAckData, ref, []byte{s7.FuncRead, 0}, nil)
	}
	count := int(param[1])
	outParam := []byte{s7.FuncRead, byte(count)}
	var outData []byte
	for i := 0; i < count; i++ {
		off := 2 + i*12
		if off+12 > len(param) {
			outData = append(outData, 0x05, 0x00, 0x00, 0x00)
			continue
		}
		it, err := parseItem(param[off : off+12])
		if err != nil {
			outData = append(outData, 0x05, 0x00, 0x00, 0x00)
			continue
		}
		buf, code := s.readItem(it)
		if code != s7.CodeOK {
			outData = append(outData, code, 0x00, 0x00, 0x00)
			continue
		}
		ts := byte(s7.RSByteWord)
		length := len(buf) * 8
		if it.BitAccess {
			ts = s7.RSBit
			length = 1
		}
		outData = append(outData, s7.CodeOK, ts,
			byte(length>>8), byte(length))
		outData = append(outData, buf...)
		if len(buf)%2 == 1 {
			outData = append(outData, 0x00)
		}
	}
	return s7.BuildPDU(s7.ROSCTRAckData, ref, outParam, outData)
}

func (s *Server) handleWrite(ref uint16, param, data []byte) []byte {
	if len(param) < 2 {
		return s7.BuildPDU(s7.ROSCTRAckData, ref, []byte{s7.FuncWrite, 0}, nil)
	}
	count := int(param[1])
	outParam := []byte{s7.FuncWrite, byte(count)}
	var outData []byte
	off := 0
	for i := 0; i < count; i++ {
		p := 2 + i*12
		d := off
		if p+12 > len(param) || d+4 > len(data) {
			outData = append(outData, 0x05)
			continue
		}
		it, err := parseItem(param[p : p+12])
		if err != nil {
			outData = append(outData, 0x05)
			continue
		}
		ts := data[d+1]
		length := int(binary.BigEndian.Uint16(data[d+2 : d+4]))
		payload := payloadBytes(ts, length)
		d += 4
		if d+payload > len(data) {
			outData = append(outData, 0x05)
			continue
		}
		code := s.writeItem(it, data[d:d+payload])
		outData = append(outData, code)
		off = d + payload
		if payload%2 == 1 {
			off++
		}
	}
	return s7.BuildPDU(s7.ROSCTRAckData, ref, outParam, outData)
}

// parseItem 解析 S7ANY 变量说明。主站只会发 0x12 0x0A 0x10 这一种。
func parseItem(b []byte) (s7.Item, error) {
	if len(b) < 12 || b[0] != 0x12 || b[2] != 0x10 {
		return s7.Item{}, fmt.Errorf("sim-s7: unsupported item spec % x", b)
	}
	ts := b[3]
	length := int(binary.BigEndian.Uint16(b[4:6]))
	db := int(binary.BigEndian.Uint16(b[6:8]))
	area := b[8]
	addr := int(b[9])<<16 | int(b[10])<<8 | int(b[11])
	it := s7.Item{Area: area, DB: db, ByteAddr: addr / 8, Bit: -1}
	if ts == s7.TSBit {
		it.BitAccess = true
		it.Bit = addr % 8
	} else {
		it.Bytes = length
	}
	return it, nil
}

// payloadBytes 与 codec 侧保持一致：BIT/BYTE/WORD/DWORD 按位，其余按字节。
func payloadBytes(ts byte, length int) int {
	switch ts {
	case s7.RSBit, s7.RSByteWord:
		return (length + 7) / 8
	}
	return length
}

func (s *Server) readItem(it s7.Item) ([]byte, byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf, ok := s.mem[memKey{area: it.Area, db: it.DB}]
	if !ok {
		return nil, s7.CodeObjectNotFound
	}
	if it.ByteAddr < 0 || it.ByteAddr >= len(buf) {
		return nil, s7.CodeInvalidAddr
	}
	if it.BitAccess {
		if it.Bit < 0 || it.Bit > 7 {
			return nil, s7.CodeInvalidAddr
		}
		return []byte{buf[it.ByteAddr] >> uint(it.Bit) & 1}, s7.CodeOK
	}
	if it.Bytes <= 0 {
		return nil, s7.CodeInvalidAddr
	}
	if it.ByteAddr+it.Bytes > len(buf) {
		return nil, s7.CodeInvalidAddr
	}
	return append([]byte(nil), buf[it.ByteAddr:it.ByteAddr+it.Bytes]...), s7.CodeOK
}

func (s *Server) writeItem(it s7.Item, v []byte) byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memKey{area: it.Area, db: it.DB}
	// 输入区是只读的：允许写会让主站误以为写成功了，实际 PLC 上永远
	// 不会生效，是最难查的一类「静默失败」。
	if it.Area == s7.AreaInput {
		return s7.CodeAccessDenied
	}
	buf, ok := s.mem[k]
	if !ok {
		return s7.CodeObjectNotFound
	}
	if it.ByteAddr < 0 || it.ByteAddr >= len(buf) {
		return s7.CodeInvalidAddr
	}
	if it.BitAccess {
		if it.Bit < 0 || it.Bit > 7 || len(v) != 1 {
			return s7.CodeInvalidAddr
		}
		mask := byte(1) << uint(it.Bit)
		if v[0]&1 != 0 {
			buf[it.ByteAddr] |= mask
		} else {
			buf[it.ByteAddr] &^= mask
		}
		return s7.CodeOK
	}
	if it.Bytes <= 0 || len(v) != it.Bytes {
		return s7.CodeInvalidAddr
	}
	if it.ByteAddr+it.Bytes > len(buf) {
		return s7.CodeInvalidAddr
	}
	copy(buf[it.ByteAddr:], v)
	return s7.CodeOK
}
