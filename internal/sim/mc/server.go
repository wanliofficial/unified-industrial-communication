// Package mc 实现三菱 MC 3E 帧的内置从站（模拟 PLC）。
//
// 它提供一块「字软元件 + 位软元件」的内存表，按 3E 帧响应读写请求。用途与
// 其它内置从站一致：没有硬件时验证主站链路、CI 回归、故障注入。
//
// 内存模型刻意简单：每个区一个 map，键是起始编号。位区按位寻址，字区按
// 字寻址。访问未配置的编号返回 0（真实 PLC 多数区也如此，读未定义寄存器
// 得到 0 而不是错误），写则落表。越界（超出区容量）返回 0xC054。
package mc

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	mccodec "uic/internal/codec/mc"
)

// Server 是 MC 3E 从站。
type Server struct {
	ln net.Listener

	mu   sync.Mutex
	word map[string][]uint16 // 字区：按起始编号索引的字
	bit  map[string][]bool   // 位区：按起始编号索引的位
}

// NewServer 在 addr 上监听。addr 为 "127.0.0.1:0" 时由系统自动分配端口。
func NewServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:   ln,
		word: map[string][]uint16{},
		bit:  map[string][]bool{},
	}
	go s.serve()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 关闭监听。已建立的连接会在下一次读写时因对端关闭而退出。
func (s *Server) Close() error { return s.ln.Close() }

// SetWord 预置一个字软元件的值（测试与演示用）。
func (s *Server) SetWord(area string, index int, v uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := ensureWord(s.word, normalizeArea(area), index+1)
	buf[index] = v
}

// SetBit 预置一个位软元件。
func (s *Server) SetBit(area string, index int, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := ensureBit(s.bit, normalizeArea(area), index+1)
	buf[index] = v
}

func normalizeArea(area string) string { return strings.ToLower(area) }

func ensureWord(m map[string][]uint16, area string, n int) []uint16 {
	buf, ok := m[area]
	if !ok {
		buf = make([]uint16, n)
		m[area] = buf
		return buf
	}
	if len(buf) < n {
		grown := make([]uint16, n)
		copy(grown, buf)
		m[area] = grown
		buf = grown
	}
	return buf
}

func ensureBit(m map[string][]bool, area string, n int) []bool {
	buf, ok := m[area]
	if !ok {
		buf = make([]bool, n)
		m[area] = buf
		return buf
	}
	if len(buf) < n {
		grown := make([]bool, n)
		copy(grown, buf)
		m[area] = grown
		buf = grown
	}
	return buf
}

// serve 接受连接并各自服务。MC 3E 是「一问一答」的半双工协议，但 TCP 允许多
// 连接；每个连接独立缓冲。
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
	for {
		req, err := readFrame(conn)
		if err != nil {
			if err != io.EOF {
				logger.Debug("mc sim read", "err", err)
			}
			return
		}
		resp := s.respond(req)
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// readFrame 读满一个 3E 帧：先读 9 字节头，按数据长度读剩余部分。
func readFrame(conn net.Conn) ([]byte, error) {
	head := make([]byte, 9)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	dataLen := int(binary.LittleEndian.Uint16(head[7:]))
	if dataLen < 2 || dataLen > 4096 {
		return nil, fmt.Errorf("mc sim: invalid data length %d", dataLen)
	}
	body := make([]byte, dataLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

// respond 处理一个请求帧并返回响应帧。
func (s *Server) respond(req []byte) []byte {
	if len(req) < 21 {
		return mccodec.BuildResponse(0xC059, nil) // 命令格式错误
	}
	command := binary.LittleEndian.Uint16(req[11:])
	sub := binary.LittleEndian.Uint16(req[13:])
	code := req[15]
	head := mccodec.Uint24(req[16:])
	points := int(binary.LittleEndian.Uint16(req[19:]))
	data := req[21:]

	dev, ok := mccodec.ByCode(code)
	if !ok {
		return mccodec.BuildResponse(0xC056, nil) // 设备不支持
	}

	if command == mccodec.CommandRead {
		return s.handleRead(dev, head, points, sub)
	}
	if command == mccodec.CommandWrite {
		return s.handleWrite(dev, head, points, sub, data)
	}
	return mccodec.BuildResponse(0xC050, nil) // 命令不支持
}

func (s *Server) handleRead(dev mccodec.Device, head, points int, sub uint16) []byte {
	limit := 7168
	if !dev.Bit {
		limit = 960
	}
	if head < 0 || points <= 0 || head+points > limit {
		return mccodec.BuildResponse(0xC054, nil) // 起始+点数越界
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dev.Bit {
		buf := ensureBit(s.bit, dev.AreaKey(), head+points)
		bits := buf[head : head+points]
		return mccodec.BuildResponse(0x0000, mccodec.PackBits(bits))
	}
	buf := ensureWord(s.word, dev.AreaKey(), head+points)
	words := buf[head : head+points]
	out := make([]byte, points*2)
	for i, w := range words {
		mccodec.PutWord(out[i*2:], w)
	}
	return mccodec.BuildResponse(0x0000, out)
}

func (s *Server) handleWrite(dev mccodec.Device, head, points int, sub uint16, data []byte) []byte {
	limit := 7168
	if !dev.Bit {
		limit = 960
	}
	if head < 0 || points <= 0 || head+points > limit {
		return mccodec.BuildResponse(0xC054, nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dev.Bit {
		if sub != mccodec.SubcommandBit || len(data) != (points+7)/8 {
			return mccodec.BuildResponse(0xC058, nil)
		}
		buf := ensureBit(s.bit, dev.AreaKey(), head+points)
		bits := mccodec.UnpackBits(data, points)
		copy(buf[head:head+points], bits)
		return mccodec.BuildResponse(0x0000, nil)
	}
	if sub != mccodec.SubcommandWord || len(data) != points*2 {
		return mccodec.BuildResponse(0xC058, nil)
	}
	buf := ensureWord(s.word, dev.AreaKey(), head+points)
	for i := 0; i < points; i++ {
		buf[head+i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return mccodec.BuildResponse(0x0000, nil)
}
