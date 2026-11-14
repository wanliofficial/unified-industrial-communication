// Package toledo 提供一个内存称重仪表（虚拟衡器），用于自检与演示。
//
// 衡器复用 codec/toledo 的组帧（Format），因此「驱动解出来的值」与「衡器写进去
// 的值」不会因两边各写一套帧格式规则而漂移——这一点比表格驱动更有价值：单测里
// 同一个读数先由衡器 Format 再由驱动 Parse，真出现理解不一致时立刻暴露。
//
// 衡器是主动推送的一端：连上后按设定频率持续往上送 ASCII 连续输出帧，不发命令、
// 不应答。本从站只模拟这一种行为，不做二进制 TOLEDO 格式、不做 SICS 命令应答，
// 与驱动侧的「明确不做」保持一致——一个内置衡器不该假装能模拟它无法正确实现的
// 东西，否则只会制造「我们的实现没问题」的错觉。
package toledo

import (
	"net"
	"sync"
	"time"

	toledocodec "uic/internal/codec/toledo"
)

// Server 是一台内存称重仪表。
type Server struct {
	mu sync.Mutex
	// scale 是当前读数。测试可通过 Set* 改变，衡器据此持续推帧。
	scale toledocodec.Reading
	// decimals 是数值段小数位数（决定帧宽）。
	decimals int
	// interval 是推帧间隔。0 表示用 defaultInterval。
	interval time.Duration
	// silent 为真时不推任何帧（模拟衡器掉电/未连接）。
	silent bool

	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool
}

// defaultInterval 是默认推帧间隔。取 50ms ≈ 20 帧/秒，落在现场常见的
// 10~25 帧/秒区间内，又不会给测试回环带来压力。
const defaultInterval = 50 * time.Millisecond

// Option 调整衡器的初始状态。
type Option func(*Server)

// WithReading 设置初始读数（状态/模式/符号/重量/单位）。
func WithReading(r toledocodec.Reading) Option {
	return func(s *Server) { s.scale = r }
}

// WithWeight 是一个便捷选项：设一个稳定毛重视为读数。
func WithWeight(v float64, unit string, decimals int) Option {
	return func(s *Server) {
		s.scale = toledocodec.Reading{
			Status: toledocodec.StatusStable,
			Mode:   toledocodec.ModeGross,
			Sign:   signOfFloat(v),
			Weight: v,
			Unit:   toledocodec.Unit(unit),
		}
		s.decimals = decimals
	}
}

// WithDecimals 设置小数位数。
func WithDecimals(n int) Option {
	return func(s *Server) { s.decimals = n }
}

// WithInterval 设置推帧间隔。
func WithInterval(d time.Duration) Option {
	return func(s *Server) { s.interval = d }
}

// WithSilent 让衡器不推任何帧。
func WithSilent() Option {
	return func(s *Server) { s.silent = true }
}

// NewServer 在 addr 上启动一台衡器。
func NewServer(addr string, opts ...Option) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		scale: toledocodec.Reading{
			Status: toledocodec.StatusStable,
			Mode:   toledocodec.ModeGross,
			Sign:   1,
			Weight: 149.0,
			Unit:   "kg",
		},
		decimals: 1,
		interval: defaultInterval,
		conns:    map[net.Conn]struct{}{},
	}
	s.ln = ln
	for _, o := range opts {
		o(s)
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回监听地址（"127.0.0.1:port"）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// SetReading 整体替换当前读数。
func (s *Server) SetReading(r toledocodec.Reading) {
	s.mu.Lock()
	s.scale = r
	s.mu.Unlock()
}

// SetWeight 改重量（保持状态/模式/单位不变）。
func (s *Server) SetWeight(v float64) {
	s.mu.Lock()
	s.scale.Weight = v
	if v < 0 {
		s.scale.Sign = -1
	} else {
		s.scale.Sign = 1
	}
	s.mu.Unlock()
}

// SetStatus 改称量状态（ST/US/OL/UL）。
func (s *Server) SetStatus(st toledocodec.Status) {
	s.mu.Lock()
	s.scale.Status = st
	s.mu.Unlock()
}

// SetMode 改毛净模式（GS/NT/TR）。
func (s *Server) SetMode(m toledocodec.Mode) {
	s.mu.Lock()
	s.scale.Mode = m
	s.mu.Unlock()
}

// Close 停止衡器并关闭所有连接。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return nil
}

// acceptLoop 接受连接并把每个连接交给一个推帧 goroutine。
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// 监听被 Close 关掉是正常的退出路径。
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.pushLoop(conn)
		}()
	}
}

// pushLoop 对单个连接持续推帧。
func (s *Server) pushLoop(conn net.Conn) {
	// 静默衡器：收下连接但不推帧，用于验证驱动「从未收到帧 → comm_error」。
	s.mu.Lock()
	silent := s.silent
	interval := s.interval
	s.mu.Unlock()
	if silent {
		// 阻塞到连接关闭，保持连接存在（不立即 EOF），让驱动能观察到「连着但没数据」。
		buf := make([]byte, 64)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		<-ticker.C
		s.mu.Lock()
		frame := toledocodec.Format(s.scale, s.decimals)
		s.mu.Unlock()
		if _, err := conn.Write(frame); err != nil {
			// 连接被 Close 关闭是正常的退出路径。
			return
		}
	}
}

// signOfFloat 返回非负浮点数的符号位（+1 / -1）。
func signOfFloat(v float64) int {
	if v < 0 {
		return -1
	}
	return 1
}
