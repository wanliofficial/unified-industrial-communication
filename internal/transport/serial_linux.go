//go:build linux

package transport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Linux termios 常量。syscall 包并未导出全部，且这些值在所有 Linux 架构上
// 相同（0x54xx 系列），因此在此显式声明而不是依赖构建标签矩阵。
const (
	ioTCGETS  = 0x5401 // TCGETS
	ioTCSETS  = 0x5402 // TCSETS
	ioTCSBRK  = 0x5409 // TCSBRK（参数 1 表示 tcdrain）
	ioTCFLSH  = 0x540B // TCFLSH
	tcIFLUSH  = 0      // TCIFLUSH
	baud46080 = 0x1004 // B460800
	baud92160 = 0x1005 // B921600
)

// defaultReadTimeout 是没有显式 deadline 时的兜底读超时。
//
// 没有它，一次「从站没应答 + 上层忘了设 deadline」就会把采集 goroutine
// 永久钉在 read 上，而且因为持有内部锁，连 Close 都进不来。
const defaultReadTimeout = 3 * time.Second

// serialStream 是一个配好 termios 的串口。
//
// 关键设计：以 O_NONBLOCK 打开，os.File 因此进入 Go 运行时的 poller，
// SetReadDeadline 可以直接用——不需要自己写 select/poll 循环，也不需要
// 引入任何第三方串口库。
//
// 注意：正因为如此，这里**绝不能用 File.Fd()**。Fd() 会把 fd 切回阻塞
// 模式并让它脱离 poller，此后所有 deadline 都会静默失效。需要 fd 时一律
// 走 SyscallConn().Control()。
//
// 并发模型：mu 只保护 f 指针的读/写，**不覆盖阻塞 I/O**。浏览器式地
// 长时间持锁会让 Close 无法打断在读的那一方——os.File 的 poller 本身
// 支持并发 Read/Write/SetDeadline/Close，因此不需要在外层再加一把大锁。
type serialStream struct {
	mu        sync.Mutex
	f         *os.File
	path      string
	readDL    time.Time // 最近一次设置的读超时，供兜底计算
	writeDL   time.Time
	closeOnce sync.Once
	closeErr  error
	// writeMu 串行化写：一条帧必须整体上线，不能被并发写切碎。
	writeMu sync.Mutex
}

// OpenSerial 打开串口并应用参数。
func OpenSerial(cfg SerialConfig) (Stream, error) {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(cfg.Port, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("serial: open %s: %w", cfg.Port, err)
	}
	s := &serialStream{f: f, path: cfg.Port}
	if err := s.apply(cfg); err != nil {
		_ = f.Close()
		return nil, err
	}
	// 没有可轮询的 fd 就没有超时能力；这种情况下建链必须失败，
	// 否则一次掉线会把采集 goroutine 永久挂在 read 上。
	if err := f.SetReadDeadline(time.Now()); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		_ = f.Close()
		return nil, fmt.Errorf("serial: %s does not support deadlines: %w", cfg.Port, err)
	}
	_ = f.SetReadDeadline(time.Time{})
	return s, nil
}

// file 取当前 fd 句柄。仅此一步加锁：之后的阻塞调用不再持锁，
// 这样 Close 可以随时打断在飞的 ReadFull。
func (s *serialStream) file() (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil, errors.New("serial: closed")
	}
	return s.f, nil
}

// control 在持有 fd 引用期间执行一次 ioctl，避免 Fd() 的副作用。
func control(f *os.File, fn func(fd uintptr) error) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if cerr := raw.Control(func(fd uintptr) { inner = fn(fd) }); cerr != nil {
		return cerr
	}
	return inner
}

// apply 用 TCGETS/TCSETS 配置 termios：raw 模式、8N1、无流控。
func (s *serialStream) apply(cfg SerialConfig) error {
	speed, err := baudConst(cfg.Baud)
	if err != nil {
		return err
	}
	f, err := s.file()
	if err != nil {
		return err
	}
	return control(f, func(fd uintptr) error {
		var t syscall.Termios
		if err := ioctl(fd, ioTCGETS, unsafe.Pointer(&t)); err != nil {
			return fmt.Errorf("serial: %s: TCGETS: %w", s.path, err)
		}

		// raw 模式：关闭一切行内处理，否则 0x0D/0x11/0x13 会被内核改写，
		// 二进制协议必然损坏。
		t.Iflag = syscall.IGNPAR
		if cfg.Parity != "none" {
			t.Iflag |= syscall.INPCK
		}
		t.Oflag = 0
		t.Lflag = 0
		t.Cflag = speed | syscall.CLOCAL | syscall.CREAD

		switch cfg.DataBits {
		case 5:
			t.Cflag |= syscall.CS5
		case 6:
			t.Cflag |= syscall.CS6
		case 7:
			t.Cflag |= syscall.CS7
		default:
			t.Cflag |= syscall.CS8
		}
		switch cfg.Parity {
		case "even":
			t.Cflag |= syscall.PARENB
		case "odd":
			t.Cflag |= syscall.PARENB | syscall.PARODD
		}
		if cfg.StopBits == 2 {
			t.Cflag |= syscall.CSTOPB
		}

		// VMIN=1/VTIME=0：有数据就返回，没数据交给运行时的 deadline 机制。
		// 若写成 VMIN=0，非阻塞读会立刻返回 0 字节，io.ReadFull 会空转。
		t.Cc[syscall.VMIN] = 1
		t.Cc[syscall.VTIME] = 0
		t.Ispeed = speed
		t.Ospeed = speed

		if err := ioctl(fd, ioTCSETS, unsafe.Pointer(&t)); err != nil {
			return fmt.Errorf("serial: %s: TCSETS: %w", s.path, err)
		}
		// 丢弃上电或上一次会话遗留的字节。
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioTCFLSH, tcIFLUSH)
		if errno != 0 {
			return fmt.Errorf("serial: %s: tcflush: %w", s.path, errno)
		}
		return nil
	})
}

func (s *serialStream) Write(p []byte) error {
	f, err := s.file()
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	dl := s.writeDL
	s.mu.Unlock()
	if dl.IsZero() {
		_ = f.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err := f.Write(p); err != nil {
		return err
	}
	// tcdrain：等字节真正离开移位寄存器。半双工 RS-485 上这一步不能省，
	// 否则紧接着切换方向会把自家发送的数据读回来当响应。
	return control(f, func(fd uintptr) error {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioTCSBRK, 1)
		if errno != 0 {
			return fmt.Errorf("serial: tcdrain: %w", errno)
		}
		return nil
	})
}

func (s *serialStream) ReadFull(n int) ([]byte, error) {
	f, err := s.file()
	if err != nil {
		return nil, err
	}
	// 兜底超时：上层没设 deadline 时也不能无限期挂着。
	s.mu.Lock()
	dl := s.readDL
	s.mu.Unlock()
	if dl.IsZero() {
		_ = f.SetReadDeadline(time.Now().Add(defaultReadTimeout))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// SetDeadline 由协议层按设备超时调用。
func (s *serialStream) SetDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDL, s.writeDL = t, t
	f := s.f
	s.mu.Unlock()
	if f == nil {
		return errors.New("serial: closed")
	}
	if err := f.SetWriteDeadline(t); err != nil {
		return err
	}
	return f.SetReadDeadline(t)
}

// Drain 丢弃接收缓冲中的残留字节。
func (s *serialStream) Drain() error {
	f, err := s.file()
	if err != nil {
		return err
	}
	return control(f, func(fd uintptr) error {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioTCFLSH, tcIFLUSH)
		if errno != 0 {
			return fmt.Errorf("serial: tcflush: %w", errno)
		}
		return nil
	})
}

// Close 关闭串口并唤醒任何阻塞中的读写。幂等。
func (s *serialStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		f := s.f
		s.f = nil
		s.mu.Unlock()
		if f != nil {
			// 关 fd 会让 poller 上的阻塞读立刻返回 ErrClosed。
			s.closeErr = f.Close()
		}
	})
	return s.closeErr
}

// Path 返回串口路径，供日志与诊断使用。
func (s *serialStream) Path() string { return s.path }

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func baudConst(baud int) (uint32, error) {
	switch baud {
	case 300:
		return syscall.B300, nil
	case 600:
		return syscall.B600, nil
	case 1200:
		return syscall.B1200, nil
	case 2400:
		return syscall.B2400, nil
	case 4800:
		return syscall.B4800, nil
	case 9600:
		return syscall.B9600, nil
	case 19200:
		return syscall.B19200, nil
	case 38400:
		return syscall.B38400, nil
	case 57600:
		return syscall.B57600, nil
	case 115200:
		return syscall.B115200, nil
	case 230400:
		return syscall.B230400, nil
	case 460800:
		return baud46080, nil
	case 921600:
		return baud92160, nil
	}
	return 0, fmt.Errorf("serial: baud %d unsupported", baud)
}
