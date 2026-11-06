package modbus

import (
	"errors"
	"os"
	"time"

	mb "uic/internal/codec/modbus"
	"uic/internal/model"
	"uic/internal/obs"
	"uic/internal/transport"
)

// 串口从站（Modbus RTU / ASCII）。
//
// 与 TCP 从站的差别不只是「换一种传输」，而是**分帧与地址语义都变了**：
//
//   - TCP 由 MBAP 长度字段分帧，串口 RTU 没有长度前缀，必须按功能码推导
//     帧长，推导不出来时只能靠 3.5 个字符的静默判断帧尾；
//   - TCP 上「不是我的单元」可以回 0x0B，串口上必须**保持静默**：一条
//     RS-485 总线上挂着多台从站，替别人的地址应答就是串扰，主站会看到
//     两台设备同时回话；
//   - TCP 的连接数上限在串口上不成立：物理端口只有一个，同一时刻只能
//     服务一个请求。因此 `maxConnections` 在串口端点上是配置错误，而
//     不是被忽略的字段。
//
// ASCII 帧以 ':' 起始、CRLF 结束，看起来「不需要推导长度」，但预知长度
// 能把「帧被截断」从字符超时提前成一次立刻可判的错误，代价只有几行。

// serialPort 是从站需要的串口能力。
//
// transport.Stream 只保证 Write / ReadFull / Close；从站还需要 SetDeadline
// （帧间静默检测、帧内字符超时）与 Drain（CRC/LRC 错之后丢弃残字节）。
type serialPort interface {
	transport.Stream
	SetDeadline(t time.Time) error
	Drain() error
}

// serialAdapter 把 transport.Stream 补齐成 serialPort。
//
// 用适配器而不是在 transport 里加宽接口：主站只用到 SetDeadline，Drain
// 是从站专属的「重新同步」动作，不该污染主站的字节流抽象。
type serialAdapter struct{ transport.Stream }

func (a serialAdapter) SetDeadline(t time.Time) error {
	if s, ok := a.Stream.(transport.DeadlineSetter); ok {
		return s.SetDeadline(t)
	}
	return nil
}

func (a serialAdapter) Drain() error {
	if d, ok := a.Stream.(interface{ Drain() error }); ok {
		return d.Drain()
	}
	return nil
}

func openSerial(cfg transport.SerialConfig) (serialPort, error) {
	st, err := transport.OpenSerial(cfg)
	if err != nil {
		return nil, err
	}
	return serialAdapter{st}, nil
}

// frameGap 返回 RTU 的帧间静默时间（3.5 个字符时间）。
//
// 只有两种情况用到它：未知功能码（长度无法推导）时判断帧尾；校验失败后
// 丢弃残字节重新同步。已知功能码一律按推导长度读满，不依赖静默——这是
// 有意的：3.5 个字符时间在 USB 转串口上并不总是可靠。
//
// 下限取 5ms：USB 适配器的转发延迟普遍在 1..16ms，严格按 9600 波特下的
// 3.6ms 切帧会把一条完整帧截断，症状是「偶发 CRC 错」。
func frameGap(s model.SlaveSerial) time.Duration {
	bits := 1 + s.DataBits + s.StopBits
	if s.Parity != "none" {
		bits++
	}
	charTime := time.Duration(float64(bits) / float64(s.Baud) * float64(time.Second))
	gap := 35 * charTime / 10
	if gap < 5*time.Millisecond {
		gap = 5 * time.Millisecond
	}
	if gap > 200*time.Millisecond {
		gap = 200 * time.Millisecond
	}
	return gap
}

// asciiCharTimeout 是 ASCII 帧内两个字符之间的最大间隔。
//
// 规范给的是 1s（超过即认为帧结束）。取规范值而不是更短：ASCII 设备
// 常常是慢速仪表，把字符间隔收紧只会制造「偶尔收不到完整帧」。
const asciiCharTimeout = time.Second

// serveSerial 是串口从站的主循环，带断线重开。
//
// 为什么自动重开：USB 转串口适配器会因为供电波动短暂消失（EIO），此时
// `/dev/ttyUSB0` 这个路径通常还在。让从站直接死掉意味着现场必须重启
// 进程；重开失败会持续记录日志，失败语义仍然是可见的。
func (s *Server) serveSerial() {
	defer s.wg.Done()

	backoff := time.Second
	for {
		err := s.serveSerialOnce()
		if err == nil || s.isClosed() {
			return
		}
		s.log.Warn("slave_serial_lost", "slave", s.id, "endpoint", s.addr,
			"error", err.Error(), "retry", backoff.String())
		if !s.sleep(backoff) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if err := s.reopenSerial(); err != nil {
			s.log.Error("slave_serial_reopen_failed", "slave", s.id,
				"endpoint", s.addr, "error", err.Error())
			continue
		}
		s.log.Info("slave_serial_reopened", "slave", s.id, "endpoint", s.addr)
		backoff = time.Second
	}
}

func (s *Server) serveSerialOnce() error {
	if s.framing == "ascii" {
		return s.asciiLoop()
	}
	return s.rtuLoop()
}

// st 返回当前串口句柄。串口只由本从站的循环使用；加锁是为了和 Close
// 以及重开竞争时有一致的视图。
func (s *Server) stream() serialPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// reopenSerial 关闭旧句柄并重新打开同一路径。
func (s *Server) reopenSerial() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("closed")
	}
	old := s.st
	s.st = nil
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	st, err := s.openSerial(s.serialCfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = st.Close()
		return errors.New("closed")
	}
	s.st = st
	s.mu.Unlock()
	return nil
}

// sleep 是可被 Close 打断的等待，返回 false 表示已经关闭。
func (s *Server) sleep(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.isClosed() {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !s.isClosed()
}

// ---------------------------------------------------------------------------
// RTU
// ---------------------------------------------------------------------------

func (s *Server) rtuLoop() error {
	st := s.stream()
	if st == nil {
		return errors.New("serial closed")
	}
	for {
		// 帧间等待用 1s 轮询而不是无期限：既能让 Close 生效，也让
		// 「读一直超时」这种半死状态有一个稳定的退出路径。
		_ = st.SetDeadline(time.Now().Add(time.Second))
		first, err := st.ReadFull(1)
		if err != nil {
			if s.isClosed() {
				return nil
			}
			if isTimeout(err) {
				continue
			}
			return err
		}

		frame, err := s.readRTUFrame(st, first)
		if err != nil {
			return err
		}
		if frame == nil {
			// 半帧：等静默后再重新同步，避免把下一帧的地址当成这帧的尾巴。
			s.noteBadFrame("short")
			s.silence(st)
			continue
		}
		unit, pdu, perr := mb.ParseRTU(frame)
		if perr != nil {
			s.crcErrors.Add(1)
			s.noteBadFrame("crc")
			s.log.Warn("slave_frame_rejected", "slave", s.id, "endpoint", s.addr,
				"reason", "crc", "bytes", len(frame))
			s.silence(st)
			continue
		}
		s.serveFrame(st, unit, pdu)
	}
}

// readRTUFrame 从已收到的帧首字节开始读满一帧。
//
// 返回 nil, nil 表示这一帧应当被丢弃（半帧或超长），链路本身正常；
// 返回非 nil error 表示链路错误，需要重开串口。
func (s *Server) readRTUFrame(st serialPort, first []byte) ([]byte, error) {
	frame := append([]byte(nil), first...)
	for {
		want, ok := mb.RTURequestLen(frame)
		if !ok {
			// 长度无法推导：读到静默为止。这是唯一依赖 3.5 字符时间的路径。
			b, err := s.readByte(st, s.gap)
			if err != nil {
				if isTimeout(err) {
					return frame, nil
				}
				return nil, err
			}
			frame = append(frame, b)
			if len(frame) > mb.RTUMaxFrame {
				return nil, nil
			}
			continue
		}
		if len(frame) >= want {
			return frame[:want], nil
		}
		if want > mb.RTUMaxFrame {
			// 字节数被写坏的请求（FC15/16）：不能照着它去读一段
			// 不存在的长帧，否则会把后续请求一起吞掉。
			return nil, nil
		}
		rest, err := s.readN(st, want-len(frame))
		if err != nil {
			if isTimeout(err) {
				return nil, nil // 半帧
			}
			return nil, err
		}
		return append(frame, rest...), nil
	}
}

func (s *Server) readN(st serialPort, n int) ([]byte, error) {
	// 一段帧内字节的等待上限按「整帧时间 + 余量」估算，而不是固定 1s：
	// 一条被截断的帧应该尽快失败，不该把下一条请求也压在后面。
	_ = st.SetDeadline(time.Now().Add(s.frameTimeout()))
	return st.ReadFull(n)
}

func (s *Server) readByte(st serialPort, d time.Duration) (byte, error) {
	_ = st.SetDeadline(time.Now().Add(d))
	b, err := st.ReadFull(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// frameTimeout 是一条最大帧在链路上传输所需时间的粗估（按 9600 波特
// 下 256 字节约 270ms 计），最少 1s。
func (s *Server) frameTimeout() time.Duration {
	t := 3 * s.gap
	if t < time.Second {
		t = time.Second
	}
	return t
}

// silence 读到链路静默（最多 4 个帧间隔），然后丢弃残字节。
func (s *Server) silence(st serialPort) {
	deadline := time.Now().Add(4 * s.gap)
	for time.Now().Before(deadline) {
		if _, err := s.readByte(st, s.gap); err != nil {
			if isTimeout(err) {
				break
			}
			return
		}
	}
	_ = st.Drain()
}

// ---------------------------------------------------------------------------
// ASCII
// ---------------------------------------------------------------------------

func (s *Server) asciiLoop() error {
	st := s.stream()
	if st == nil {
		return errors.New("serial closed")
	}
	for {
		_ = st.SetDeadline(time.Now().Add(time.Second))
		b, err := st.ReadFull(1)
		if err != nil {
			if s.isClosed() {
				return nil
			}
			if isTimeout(err) {
				continue
			}
			return err
		}
		if b[0] != ':' {
			// ASCII 帧以 ':' 起始；其余字节只能丢弃。线路上的噪声、
			// 半条上一帧的尾巴都走这条路径。
			s.noteBadFrame("noise")
			continue
		}

		frame, err := s.readASCIIFrame(st)
		if err != nil {
			return err
		}
		if frame == nil {
			s.noteBadFrame("short")
			continue
		}
		unit, pdu, perr := mb.ParseASCII(frame)
		if perr != nil {
			s.crcErrors.Add(1)
			s.noteBadFrame("lrc")
			s.log.Warn("slave_frame_rejected", "slave", s.id, "endpoint", s.addr,
				"reason", "lrc", "bytes", len(frame))
			continue
		}
		s.serveFrame(st, unit, pdu)
	}
}

// readASCIIFrame 从已读到的起始 ':' 开始读到 CR/LF 或长度上限。
func (s *Server) readASCIIFrame(st serialPort) ([]byte, error) {
	frame := []byte{':'}
	for {
		c, err := s.readByte(st, asciiCharTimeout)
		if err != nil {
			if isTimeout(err) || s.isClosed() {
				return nil, nil
			}
			return nil, err
		}
		frame = append(frame, c)
		if c == '\n' {
			return frame, nil
		}
		if len(frame) > mb.ASCIIMaxFrame {
			return nil, nil
		}
	}
}

// ---------------------------------------------------------------------------
// 请求分派（RTU / ASCII 共用）
// ---------------------------------------------------------------------------

// serveFrame 按串口地址语义处理一帧：非本机单元保持静默，广播只执行写。
func (s *Server) serveFrame(st serialPort, unit byte, pdu []byte) {
	if len(pdu) == 0 {
		return
	}
	if unit == 0 {
		// 广播：从站执行写，但**不得回应**——总线上所有从站会同时收到，
		// 谁应答都会造成串扰。广播读在规范里没有意义，直接忽略。
		s.requests.Add(1)
		s.noteRequest(int(unit), pdu[0], "broadcast")
		s.broadcast(pdu)
		return
	}
	if _, ok := s.units[int(unit)]; !ok {
		// 不是本机的单元号：必须静默。回 0x0B 会让同总线上的其它从站
		// 地址收到一个不属于它的响应。
		return
	}
	resp := s.respond(int(unit), pdu)
	out := mb.BuildRTU(unit, resp)
	if s.framing == "ascii" {
		out = mb.BuildASCII(unit, resp)
	}
	_ = st.SetDeadline(time.Now().Add(s.writeTo))
	if err := st.Write(out); err != nil {
		if !s.isClosed() {
			s.log.Warn("slave_serial_write_failed", "slave", s.id,
				"endpoint", s.addr, "error", err.Error())
		}
	}
}

// broadcast 把广播写应用到所有单元。
func (s *Server) broadcast(pdu []byte) {
	switch pdu[0] {
	case mb.FCWriteSingleCoil, mb.FCWriteSingleRegister,
		mb.FCWriteMultipleCoils, mb.FCWriteMultipleRegs:
	default:
		return
	}
	for _, u := range s.units {
		_ = s.handle(u, pdu)
	}
}

func (s *Server) noteBadFrame(reason string) {
	s.badFrames.Add(1)
	if s.met != nil {
		s.met.Inc("uic_slave_bad_frames_total", obs.Labels{
			"slave": s.id, "reason": reason,
		})
	}
}

func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}
