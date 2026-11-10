// Package hostlink 实现 Host Link FINS 的内置从站（模拟 PLC）。
//
// 与 sim/fins 同理由：现场把一条 RS-485/RS-232 线原样透传到以太网，因此用
// TCP 承载不是「另造一个协议」——分帧、FCS、单元号过滤、结束码与真实串口
// 完全一致。也正因为分帧完全一致，这个从站可以直接当 `uic hostlinkread`
// 的自检目标，不需要任何真机。
//
// 三个刻意的行为，都对应真机会发生的事：
//
//  1. **单元号不匹配就完全不出声。** 真 PLC 在共享总线上只回应点名到自己
//     的帧。如果模拟器对任何单元号都应答，主站「单元号配错」这条最常见
//     的故障就永远测不出来。
//
//  2. **FCS 错时回结束码 0x13，而不是静默丢弃。** 手册定义了这类结束码，
//     真机是否回取决于机型与设置；回一个明确的码比静默无言更有教学价值，
//     而且它让「FCS 算错了」和「根本没收到」在自检时能分开。
//
//  3. **格式错回 0x14、认不出的头码回 0x16。** 前者是字段错位（比如把响应
//     等待时间写成两位），后者是拿 C 模式帧当 FINS 发过来——两者在真机上
//     的表现就是 0x14 与 0x16，自检时能立刻定位。
package hostlink

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	finscodec "uic/internal/codec/fins"
	hlcodec "uic/internal/codec/hostlink"
	"uic/internal/sim/finsmem"
)

// Server 是 Host Link FINS 从站。
type Server struct {
	ln   net.Listener
	mem  *finsmem.Mem
	unit byte

	mu     sync.Mutex
	closed bool
}

// NewServer 在 addr 上监听，单元号取 unit（0..31）。
//
// addr 为 "127.0.0.1:0" 时由系统自动分配端口，便于测试与自检。
func NewServer(addr string, unit byte) (*Server, error) {
	if unit > hlcodec.MaxUnit {
		return nil, fmt.Errorf("hostlink sim: unit number %d out of range 0..%d", unit, hlcodec.MaxUnit)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, mem: finsmem.New(), unit: unit}
	go s.serve()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 关闭监听。
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.ln.Close()
}

// Mem 暴露内存表，便于预置初值与断言。
func (s *Server) Mem() *finsmem.Mem { return s.mem }

// SetWord 预置一个字的初值。
func (s *Server) SetWord(area string, index int, v uint16) { s.mem.SetWord(area, index, v) }

// GetWord 读取一个字。
func (s *Server) GetWord(area string, index int) uint16 { return s.mem.GetWord(area, index) }

// SetBit 预置一个位的初值。
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
	// 从站的收帧循环不能复用 codec.Reader 的「丢弃非 @ 前缀」策略：
	// 那种策略适合主站（它只需要认出自己的应答），而从站需要看到「有人发了
	// 一帧格式不对的东西」并回结束码。所以这里自己扫字节，只按 `*`+CR 定界。
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(conn, one); err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Debug("hostlink sim read", "err", err)
			}
			return
		}
		buf = append(buf, one[0])
		// 丢掉 `@` 之前的一切：噪声、别的主站残留、以及上一帧 `*CR` 之后
		// 的 CRLF。不丢的话，一个噪声字节就会让后面每一帧都解不出来。
		if buf[0] != hlcodec.Start {
			if i := firstStart(buf); i >= 0 {
				buf = append(buf[:0], buf[i:]...)
			} else {
				buf = buf[:0]
				continue
			}
		}
		if one[0] != hlcodec.CR {
			continue
		}
		n, ok := hlcodec.FrameLen(buf)
		if !ok {
			// 裸 CR 或还没到 `*`+CR：继续等。超长就清空重来。
			if len(buf) > hlcodec.MaxFrame {
				buf = buf[:0]
			}
			continue
		}
		frame := append([]byte(nil), buf[:n]...)
		buf = append(buf[:0], buf[n:]...)

		resp, silent := s.respond(frame)
		if silent {
			continue
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// firstStart 返回 buf 中第一个 `@` 的下标。
func firstStart(buf []byte) int {
	for i, c := range buf {
		if c == hlcodec.Start {
			return i
		}
	}
	return -1
}

// respond 处理一帧请求，返回要写回的字节。
//
// silent 为 true 表示「按协议什么都不该回」（单元号不是本机）。
func (s *Server) respond(raw []byte) (out []byte, silent bool) {
	req, err := hlcodec.ParseRequest(raw)
	if err != nil {
		// FCS 错、字段错位、头码不对：这些都能从原始字节里认出单元号，
		// 于是可以回一个带结束码的应答，而不是让主站干等超时。
		return s.errorReply(raw, err), false
	}
	if req.Unit != s.unit {
		return nil, true
	}
	finsReq := finscodec.Frame{
		Header: finscodec.Header{
			ICF: req.ICF, RSV: 0, GCT: 2,
			DNA: 0, DA1: 0, DA2: req.DA2,
			SNA: 0, SA1: 0, SA2: req.SA2, SID: req.SID,
		},
		MRC:  req.MRC,
		SRC:  req.SRC,
		Data: req.Text,
	}
	finsResp := s.mem.Respond(finsReq)
	parsed, err := finscodec.Parse(finsResp)
	if err != nil {
		// 内存层不应该产出非法帧；真出现就是模拟器自己的 bug，回格式错。
		return s.errorReply(raw, err), false
	}
	hr := hlcodec.Reply(req, parsed.EndCode, parsed.Data)
	out, err = hlcodec.BuildResponse(hr)
	if err != nil {
		return s.errorReply(raw, err), false
	}
	return out, false
}

// errorReply 尽力从一帧有问题的帧里取出单元号，回一个带结束码的应答。
//
// 取不到单元号时用本机单元号：主站至少能看见「有设备回了 0x13/0x14」，
// 比完全静默更有诊断价值。
func (s *Server) errorReply(raw []byte, cause error) []byte {
	code := byte(0x14) // format error
	var fe *hlcodec.FCSMismatch
	if errors.As(cause, &fe) {
		code = 0x13
	} else if len(raw) >= 5 && string(raw[3:5]) != hlcodec.HeaderCode {
		code = 0x16 // command not supported（拿 C 模式帧当 FINS）
	}
	unit := s.unit
	if u, err := hlcodec.PeekUnit(raw); err == nil {
		unit = u
	}
	// ICF/DA2/SA2/SID 都填 0：请求本身没解出来，回显无从谈起。
	out, err := hlcodec.BuildResponse(hlcodec.Response{Unit: unit, EndCode: code})
	if err != nil {
		return nil
	}
	return out
}
