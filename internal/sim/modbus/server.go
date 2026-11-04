// Package modbus（sim）实现一个最小 Modbus TCP 从站，用于自测、演示与
// 驱动一致性测试。它复用 codec/modbus 的帧层与 CRC 实现，避免「主站与
// 从站各写一套」导致的语义漂移。
package modbus

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	mbcodec "uic/internal/codec/modbus"
)

// Server 是一个内存寄存器模型的 Modbus TCP 从站。
type Server struct {
	mu       sync.RWMutex
	ln       net.Listener
	holding  []uint16
	input    []uint16
	coils    []bool
	discrete []bool
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   bool
}

// NewServer 创建从站；addr 为 "127.0.0.1:0" 时自动分配端口。
func NewServer(addr string, size int) (*Server, error) {
	if size <= 0 {
		size = 10000
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		holding:  make([]uint16, size),
		input:    make([]uint16, size),
		coils:    make([]bool, size),
		discrete: make([]bool, size),
		conns:    map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

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
	// 关闭在飞连接，否则对端会一直阻塞到读超时（默认 60s），Close 变成慢操作。
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// SetHolding 预置保持寄存器。
func (s *Server) SetHolding(offset int, values ...uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range values {
		if offset+i >= 0 && offset+i < len(s.holding) {
			s.holding[offset+i] = v
		}
	}
}

// Holding 读取保持寄存器。
func (s *Server) Holding(offset int) uint16 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if offset < 0 || offset >= len(s.holding) {
		return 0
	}
	return s.holding[offset]
}

// SetCoil 预置线圈。
func (s *Server) SetCoil(offset int, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset >= 0 && offset < len(s.coils) {
		s.coils[offset] = v
	}
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
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			s.serve(c)
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	for {
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		head := make([]byte, 7)
		if _, err := io.ReadFull(c, head); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint16(head[4:6]))
		if length < 2 || length > 260 {
			return
		}
		body := make([]byte, length-1)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		resp := s.handle(body)
		out := make([]byte, 7+len(resp))
		copy(out[0:2], head[0:2])
		binary.BigEndian.PutUint16(out[2:4], 0)
		binary.BigEndian.PutUint16(out[4:6], uint16(len(resp)+1))
		out[6] = head[6]
		copy(out[7:], resp)
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

func (s *Server) handle(pdu []byte) []byte {
	fc, addr, qty, values, err := mbcodec.ParseRequestPDU(pdu)
	if err != nil {
		if len(pdu) > 0 {
			return mbcodec.ExceptionPDU(pdu[0], 1)
		}
		return mbcodec.ExceptionPDU(0, 1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	switch fc {
	case mbcodec.FCReadHoldingRegs:
		return s.readRegs(fc, s.holding, addr, qty)
	case mbcodec.FCReadInputRegs:
		return s.readRegs(fc, s.input, addr, qty)
	case mbcodec.FCReadCoils:
		return s.readBits(fc, s.coils, addr, qty)
	case mbcodec.FCReadDiscreteInputs:
		return s.readBits(fc, s.discrete, addr, qty)
	case mbcodec.FCWriteSingleCoil:
		if addr < 0 || addr >= len(s.coils) || len(values) < 2 {
			return mbcodec.ExceptionPDU(fc, 2)
		}
		s.coils[addr] = binary.BigEndian.Uint16(values) == 0xFF00
		return []byte{fc, pdu[1], pdu[2], pdu[3], pdu[4]}
	case mbcodec.FCWriteSingleRegister:
		if addr < 0 || addr >= len(s.holding) || len(values) < 2 {
			return mbcodec.ExceptionPDU(fc, 2)
		}
		s.holding[addr] = binary.BigEndian.Uint16(values)
		return []byte{fc, pdu[1], pdu[2], pdu[3], pdu[4]}
	case mbcodec.FCWriteMultipleRegs:
		if addr < 0 || addr+qty > len(s.holding) || len(values) < qty*2 {
			return mbcodec.ExceptionPDU(fc, 2)
		}
		for i := 0; i < qty; i++ {
			s.holding[addr+i] = binary.BigEndian.Uint16(values[i*2:])
		}
		return []byte{fc, pdu[1], pdu[2], pdu[3], pdu[4]}
	case mbcodec.FCWriteMultipleCoils:
		if addr < 0 || addr+qty > len(s.coils) {
			return mbcodec.ExceptionPDU(fc, 2)
		}
		for i := 0; i < qty; i++ {
			byteIdx := i / 8
			if byteIdx >= len(values) {
				return mbcodec.ExceptionPDU(fc, 3)
			}
			s.coils[addr+i] = values[byteIdx]&(1<<uint(i%8)) != 0
		}
		return []byte{fc, pdu[1], pdu[2], pdu[3], pdu[4]}
	}
	return mbcodec.ExceptionPDU(fc, 1)
}

func (s *Server) readRegs(fc byte, regs []uint16, addr, qty int) []byte {
	if qty <= 0 || qty > 125 || addr < 0 || addr+qty > len(regs) {
		return mbcodec.ExceptionPDU(fc, 2)
	}
	out := make([]byte, 2+qty*2)
	out[0] = fc
	out[1] = byte(qty * 2)
	for i := 0; i < qty; i++ {
		binary.BigEndian.PutUint16(out[2+i*2:], regs[addr+i])
	}
	return out
}

func (s *Server) readBits(fc byte, bits []bool, addr, qty int) []byte {
	if qty <= 0 || qty > 2000 || addr < 0 || addr+qty > len(bits) {
		return mbcodec.ExceptionPDU(fc, 2)
	}
	n := (qty + 7) / 8
	out := make([]byte, 2+n)
	out[0] = fc
	out[1] = byte(n)
	for i := 0; i < qty; i++ {
		if bits[addr+i] {
			out[2+i/8] |= 1 << uint(i%8)
		}
	}
	return out
}
