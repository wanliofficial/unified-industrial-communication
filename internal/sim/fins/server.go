// Package fins 实现欧姆龙 FINS/TCP 的内置从站（模拟 PLC）。
//
// 本包只做 FINS/TCP 的信封：监听、节点号协商、16 字节定长头、按长度字段
// 拆帧。内存区语义与应答码在 sim/finsmem，与 Host Link 从站共用同一份，
// 因此两个模拟器对同一个请求的应答逐字节相同——这正是自检时想要的：
// 「换个信封，行为不变」。
package fins

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	finscodec "uic/internal/codec/fins"
	"uic/internal/sim/finsmem"
)

// Server 是 FINS/TCP 从站。
type Server struct {
	ln  net.Listener
	mem *finsmem.Mem

	mu      sync.Mutex
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
		mem:     finsmem.New(),
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

// Mem 暴露内存表，便于预置初值与断言。
func (s *Server) Mem() *finsmem.Mem { return s.mem }

// SetWord 预置一个字的初值。
func (s *Server) SetWord(area string, index int, v uint16) { s.mem.SetWord(area, index, v) }

// GetWord 读取一个字（测试/演示用）。
func (s *Server) GetWord(area string, index int) uint16 { return s.mem.GetWord(area, index) }

// SetBit 预置一个位的初值（word 是字地址，bit 是 0..15）。
func (s *Server) SetBit(area string, word, bit int, v bool) { s.mem.SetBit(area, word, bit, v) }

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
		pkt, err := finscodec.BuildTCPFrame(s.mem.Respond(req))
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
