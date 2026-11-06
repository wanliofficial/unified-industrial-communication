//go:build linux

package modbus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	mb "uic/internal/codec/modbus"
	"uic/internal/model"
)

// 真实 tty 路径的端到端验证。
//
// 管道测试能覆盖分帧与地址语义，但覆盖不了 termios：raw 模式、非阻塞
// poller、tcdrain、SetReadDeadline 在 tty 上的真实行为。pty 走的正是
// 同一套 tty 驱动，因此这里既是「从站能不能在串口上工作」的验证，也是
// 「transport 与从站循环拼起来是否自洽」的验证。

const (
	ioTIOCSPTLCK = 0x40045431 // TIOCSPTLCK
	ioTIOCGPTN   = 0x80045430 // TIOCGPTN
)

// openPTYForSlave 创建一对伪终端，返回主端与从端路径。
func openPTYForSlave(t *testing.T) (*os.File, string) {
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

// ptyReadFull 从主端读满 n 字节，带 deadline（pty 主端是普通 *os.File，
// 支持 SetReadDeadline）。
func ptyReadFull(t *testing.T, f *os.File, n int) []byte {
	t.Helper()
	_ = f.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatalf("pty read %d bytes: %v", n, err)
	}
	return buf
}

func TestSerialRTUOverPTY(t *testing.T) {
	master, path := openPTYForSlave(t)

	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 1234)),
	}}
	s, err := New(Options{
		Slave: model.Slave{ID: "ser", Serial: &model.SlaveSerial{Port: path, Framing: "rtu"},
			Units: serialSpans()},
		Points: serialPoints(), Source: src, Writer: &fakeWriter{},
		WriteTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Addr(); got != path {
		t.Fatalf("addr = %q, want %q", got, path)
	}

	// 主端 → 从端：一条标准 RTU 读请求。
	if _, err := master.Write(mb.BuildRTU(1, readHolding(0, 1))); err != nil {
		t.Fatalf("master write: %v", err)
	}
	frame := ptyReadFull(t, master, 7)
	unit, pdu, err := mb.ParseRTU(frame)
	if err != nil {
		t.Fatalf("parse % x: %v", frame, err)
	}
	if unit != 1 {
		t.Fatalf("unit = %d", unit)
	}
	expectRegs(t, pdu, mb.FCReadHoldingRegs, 1234)

	// 非本机地址必须静默：主端读超时即通过。
	_ = master.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := master.Write(mb.BuildRTU(7, readHolding(0, 1))); err != nil {
		t.Fatalf("master write: %v", err)
	}
	buf := make([]byte, 8)
	if n, err := master.Read(buf); err == nil {
		t.Fatalf("answered a foreign unit: % x", buf[:n])
	} else if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read error = %v, want timeout", err)
	}

	// 静默之后仍要能正常应答。
	if _, err := master.Write(mb.BuildRTU(1, readHolding(0, 1))); err != nil {
		t.Fatalf("master write: %v", err)
	}
	frame = ptyReadFull(t, master, 7)
	if _, pdu, err := mb.ParseRTU(frame); err != nil {
		t.Fatalf("parse after silence: %v", err)
	} else {
		expectRegs(t, pdu, mb.FCReadHoldingRegs, 1234)
	}
}
