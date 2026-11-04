//go:build linux

package transport

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Linux 伪终端常量（syscall 未全部导出）。
const (
	ioTIOCSPTLCK = 0x40045431 // TIOCSPTLCK
	ioTIOCGPTN   = 0x80045430 // TIOCGPTN
)

// openPTY 创建一对伪终端，返回 (主端, 从端路径)。
//
// 用 pty 而不是真串口，是因为它走完全相同的 termios 与 tty 驱动路径：
// 如果 deadline 或 raw 模式有问题，这里就会暴露；而它不需要任何硬件，
// 因此在 CI 上也能跑。这也是本项目唯一能自动化验证串口层的方式。
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("no /dev/ptmx")
		}
		t.Fatalf("open ptmx: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })

	c, err := master.SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	var ptn int
	var inner error
	if err := c.Control(func(fd uintptr) {
		// 解锁从端，否则打开 /dev/pts/N 会得到 EIO。
		unlock := 0
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioTIOCSPTLCK,
			uintptr(unsafe.Pointer(&unlock))); errno != 0 {
			inner = errno
			return
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioTIOCGPTN,
			uintptr(unsafe.Pointer(&ptn))); errno != 0 {
			inner = errno
		}
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if inner != nil {
		t.Fatalf("pty ioctl: %v", inner)
	}
	return master, fmt.Sprintf("/dev/pts/%d", ptn)
}

// serialStreamIface 是 pty 测试需要的能力集合。
//
// Stream 本身只有「写满/读满/关闭」，deadline 与 flush 是可选能力（内存链路
// 不需要它们）。测试里显式断言，也就顺带验证了串口实现确实提供了这两项。
type serialStreamIface interface {
	Stream
	DeadlineSetter
	Flusher
}

// openTestSerial 在 pty 从端上打开传输层。
func openTestSerial(t *testing.T, path string) serialStreamIface {
	t.Helper()
	s, err := OpenSerial(SerialConfig{Port: path, Baud: 9600})
	if err != nil {
		t.Fatalf("open serial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	si, ok := s.(serialStreamIface)
	if !ok {
		t.Fatalf("%T must implement DeadlineSetter and Flusher", s)
	}
	return si
}

func TestSerialPTYReadWrite(t *testing.T) {
	master, path := openPTY(t)
	s := openTestSerial(t, path)

	// 主端 → 从端：验证 raws 模式下不会改写字节。
	// 0x0D(CR)、0x11(XON)、0x13(XOFF) 是行规程最容易吃掉的值。
	payload := []byte{0x0D, 0x11, 0x13, 0x00, 0xFF, 0x7E}
	if _, err := master.Write(payload); err != nil {
		t.Fatalf("master write: %v", err)
	}
	got, err := s.ReadFull(len(payload))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("raw bytes mangled: got %x want %x", got, payload)
	}

	// 从端 → 主端：验证写路径与 tcdrain。
	if err := s.Write([]byte{0x01, 0x03, 0xAA}); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 3)
	_ = master.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := master.Read(buf)
	if err != nil {
		t.Fatalf("master read: %v", err)
	}
	if n != 3 || buf[2] != 0xAA {
		t.Fatalf("master got %x", buf[:n])
	}
}

// deadline 是串口层最容易写错的地方：一旦退化成阻塞 fd，超时静默失效，
// 采集 goroutine 会被永久钉住。这里必须能在截止时间附近返回。
func TestSerialPTYDeadline(t *testing.T) {
	_, path := openPTY(t)
	s := openTestSerial(t, path)

	if err := s.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	start := time.Now()
	_, err := s.ReadFull(4) // 线路上没有数据
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("read should time out")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("deadline not honored: waited %v", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned too early (%v), deadline may be ignored", elapsed)
	}
}

// Close 必须能打断正在阻塞的 ReadFull，否则关机会挂住。
func TestSerialPTYCloseUnblocksRead(t *testing.T) {
	_, path := openPTY(t)
	s, err := OpenSerial(SerialConfig{Port: path, Baud: 9600})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.ReadFull(1)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // 让读真正进入阻塞
	_ = s.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read after close should fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock pending read")
	}
}

func TestSerialPTYDoubleCloseIsSafe(t *testing.T) {
	_, path := openPTY(t)
	s, err := OpenSerial(SerialConfig{Port: path, Baud: 9600})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close must be a no-op, got %v", err)
	}
	if _, err := s.ReadFull(1); err == nil {
		t.Fatal("read after close must fail")
	}
	if err := s.Write([]byte{0}); err == nil {
		t.Fatal("write after close must fail")
	}
}

func TestSerialPTYDrainDiscardsStaleBytes(t *testing.T) {
	// 半双工链路上，上一轮迟到的响应必须能被清掉，否则会被当成本轮的帧头。
	master, path := openPTY(t)
	s := openTestSerial(t, path)

	if _, err := master.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF}); err != nil {
		t.Fatalf("master write: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // 让字节进入从端缓冲
	f, ok := s.(Flusher)
	if !ok {
		t.Fatal("serial stream must implement Flusher")
	}
	if err := f.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// 清空后立刻读应当超时，而不是读到旧字节。
	if err := s.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFull(1); err == nil {
		t.Fatalf("stale byte survived drain: %x", got)
	}
}

func TestOpenSerialMissingPort(t *testing.T) {
	_, err := OpenSerial(SerialConfig{Port: "/dev/definitely-not-a-tty"})
	if err == nil {
		t.Fatal("missing device must fail")
	}
}
