// Package fins 实现欧姆龙 FINS/TCP 的内置从站（模拟 PLC）。
//
// 它提供一块按内存区索引的字表，同时承载字视图与位视图：位读写落在
// 对应的字上，因此「写 CIO0.0 后读 CIO0 字」能看到最低位变化，与真实
// PLC 的行为一致。
//
// 内存模型刻意简单：每个内存区一个 map，键是字地址；未配置地址读返回
// 0（真实 PLC 多数区也如此），写则落表。地址越界返回 0x1103（address
// out of range），区码不认识返回 0x1101（area type mismatch）。
package fins

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	finscodec "uic/internal/codec/fins"
)

// Server 是 FINS/TCP 从站。
type Server struct {
	ln net.Listener

	mu      sync.Mutex
	mem     map[byte]map[uint16]uint16 // 按字访问码索引
	nextID  byte
	serverN byte
}

// NewServer 在 addr 上监听。addr 为 "127.0.0.1:0" 时由系统自动分配端口。
func NewServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:      ln,
		mem:     map[byte]map[uint16]uint16{},
		nextID:  2,
		serverN: 1,
	}
	go s.serve()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 关闭监听。已建立的连接会在下一次读写时因对端关闭而退出。
func (s *Server) Close() error { return s.ln.Close() }

// SetWord 预置一个字的初值。
func (s *Server) SetWord(area string, index int, v uint16) {
	a, ok := finscodec.Lookup(area)
	if !ok || index < 0 || index > 0xFFFF {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setWord(a.Word, uint16(index), v)
}

// SetBit 预置一个位的初值（word 是字地址，bit 是 0..15）。
func (s *Server) SetBit(area string, word, bit int, v bool) {
	a, ok := finscodec.Lookup(area)
	if !ok || word < 0 || word > 0xFFFF || bit < 0 || bit > 15 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.word(a.Word, uint16(word))
	if v {
		w |= 1 << uint(bit)
	} else {
		w &^= 1 << uint(bit)
	}
	s.setWord(a.Word, uint16(word), w)
}

// GetWord 读取一个字（测试/演示用）。
func (s *Server) GetWord(area string, index int) uint16 {
	a, ok := finscodec.Lookup(area)
	if !ok || index < 0 || index > 0xFFFF {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.word(a.Word, uint16(index))
}

func (s *Server) word(code byte, addr uint16) uint16 {
	m := s.mem[code]
	if m == nil {
		return 0
	}
	return m[addr]
}

func (s *Server) setWord(code byte, addr uint16, v uint16) {
	m := s.mem[code]
	if m == nil {
		m = map[uint16]uint16{}
		s.mem[code] = m
	}
	m[addr] = v
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
	if err := s.handshake(conn); err != nil {
		if err != io.EOF {
			logger.Debug("fins sim handshake", "err", err)
		}
		return
	}
	for {
		req, err := readTCPFrame(conn)
		if err != nil {
			if err != io.EOF {
				logger.Debug("fins sim read", "err", err)
			}
			return
		}
		resp := s.respond(req)
		pkt, err := finscodec.BuildTCPFrame(resp)
		if err != nil {
			logger.Debug("fins sim build", "err", err)
			return
		}
		if _, err := conn.Write(pkt); err != nil {
			return
		}
	}
}

// handshake 处理节点号协商（命令 0 → 1）。
func (s *Server) handshake(conn net.Conn) error {
	head := make([]byte, 16)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		return err
	}
	if th.Command != finscodec.TCPCommandNodeRequest {
		return fmt.Errorf("fins sim: expected node request, got command %d", th.Command)
	}
	payload := make([]byte, th.Length-8)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return err
	}
	if len(payload) < 4 {
		return fmt.Errorf("fins sim: node request too short")
	}
	clientNode := payload[3]

	s.mu.Lock()
	switch {
	case clientNode == 0:
		clientNode = s.nextID
		s.nextID++
		if s.nextID == 0 || s.nextID == s.serverN {
			s.nextID++
		}
	case clientNode == s.serverN:
		s.mu.Unlock()
		_, _ = conn.Write(finscodec.BuildNodeError(0x24))
		return fmt.Errorf("fins sim: client attempted to use server node %d", clientNode)
	}
	serverNode := s.serverN
	s.mu.Unlock()

	_, err = conn.Write(finscodec.BuildNodeResponse(clientNode, serverNode))
	return err
}

// readTCPFrame 读取一个命令 2 的 FINS/TCP 帧并解析内层 FINS 帧。
func readTCPFrame(conn net.Conn) (finscodec.Frame, error) {
	head := make([]byte, 16)
	if _, err := io.ReadFull(conn, head); err != nil {
		return finscodec.Frame{}, err
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		return finscodec.Frame{}, err
	}
	if th.Command != finscodec.TCPCommandFrame {
		return finscodec.Frame{}, fmt.Errorf("fins sim: unexpected command %d", th.Command)
	}
	if th.Error != 0 {
		return finscodec.Frame{}, &finscodec.TCPError{Code: th.Error}
	}
	body := make([]byte, th.Length-8)
	if _, err := io.ReadFull(conn, body); err != nil {
		return finscodec.Frame{}, err
	}
	return finscodec.Parse(body)
}

func (s *Server) respond(req finscodec.Frame) []byte {
	// 读和写的 MRC 都是 0x01，真正区分动作的是 SRC（0x01 读 / 0x02 写）。
	if req.MRC != finscodec.MRCRead || (req.SRC != finscodec.SRCRead && req.SRC != finscodec.SRCWrite) {
		return finscodec.BuildResponse(req, 0x0401, nil) // 未定义命令
	}
	if len(req.Data) < 6 {
		return finscodec.BuildResponse(req, 0x1002, nil) // 命令太短
	}
	code := req.Data[0]
	word := binary.BigEndian.Uint16(req.Data[1:3])
	bit := req.Data[3]
	count := binary.BigEndian.Uint16(req.Data[4:6])

	wordArea, isWord := finscodec.ByWordCode(code)
	_, isBit := finscodec.ByBitCode(code)
	if !isWord && !isBit {
		return finscodec.BuildResponse(req, 0x1101, nil)
	}
	if count == 0 {
		return finscodec.BuildResponse(req, 0x1102, nil)
	}

	// 位访问与字访问共用内存表：位码先映射到字码。
	memCode := code
	bitAccess := isBit
	if bitAccess {
		memCode, _ = finscodec.BitToWordCode(code)
	} else {
		memCode = wordArea.Word
	}
	if bitAccess && bit > 15 {
		return finscodec.BuildResponse(req, 0x1103, nil)
	}

	if req.SRC == finscodec.SRCRead {
		return s.handleRead(req, memCode, bitAccess, word, bit, count)
	}
	return s.handleWrite(req, memCode, bitAccess, word, bit, count)
}

func (s *Server) handleRead(req finscodec.Frame, code byte, bitAccess bool, word uint16, bit byte, count uint16) []byte {
	if int(count) > finscodec.MaxBits || (!bitAccess && int(count) > finscodec.MaxWords) {
		return finscodec.BuildResponse(req, 0x1102, nil)
	}
	if bitAccess {
		if int(word)+(int(bit)+int(count)-1)/16 > 0xFFFF {
			return finscodec.BuildResponse(req, 0x1103, nil)
		}
	} else if int(word)+int(count)-1 > 0xFFFF {
		return finscodec.BuildResponse(req, 0x1103, nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if bitAccess {
		out := make([]byte, count)
		for i := 0; i < int(count); i++ {
			pos := int(bit) + i
			w := s.word(code, word+uint16(pos/16))
			if w&(1<<uint(pos%16)) != 0 {
				out[i] = 1
			}
		}
		return finscodec.BuildResponse(req, 0, out)
	}
	out := make([]byte, int(count)*2)
	for i := 0; i < int(count); i++ {
		binary.BigEndian.PutUint16(out[i*2:], s.word(code, word+uint16(i)))
	}
	return finscodec.BuildResponse(req, 0, out)
}

func (s *Server) handleWrite(req finscodec.Frame, code byte, bitAccess bool, word uint16, bit byte, count uint16) []byte {
	if len(req.Data) < 6 {
		return finscodec.BuildResponse(req, 0x1002, nil)
	}
	data := req.Data[6:]
	if bitAccess {
		if len(data) != int(count) {
			return finscodec.BuildResponse(req, 0x1002, nil)
		}
	} else if len(data) != int(count)*2 {
		return finscodec.BuildResponse(req, 0x1002, nil)
	}
	if int(count) > finscodec.MaxBits || (!bitAccess && int(count) > finscodec.MaxWords) {
		return finscodec.BuildResponse(req, 0x1102, nil)
	}
	if bitAccess {
		if int(word)+(int(bit)+int(count)-1)/16 > 0xFFFF {
			return finscodec.BuildResponse(req, 0x1103, nil)
		}
	} else if int(word)+int(count)-1 > 0xFFFF {
		return finscodec.BuildResponse(req, 0x1103, nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if bitAccess {
		for i := 0; i < int(count); i++ {
			pos := int(bit) + i
			addr := word + uint16(pos/16)
			w := s.word(code, addr)
			if data[i] != 0 {
				w |= 1 << uint(pos%16)
			} else {
				w &^= 1 << uint(pos%16)
			}
			s.setWord(code, addr, w)
		}
		return finscodec.BuildResponse(req, 0, nil)
	}
	for i := 0; i < int(count); i++ {
		s.setWord(code, word+uint16(i), binary.BigEndian.Uint16(data[i*2:]))
	}
	return finscodec.BuildResponse(req, 0, nil)
}
