//go:build linux

package modbus

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// 端到端：真实 pty 上跑一条两从站总线。
//
// 这个用例的价值在于它同时覆盖了此前只能分别验证的三件事：termios 打开
// 是否真的 raw、主站推导的长度是否与真实帧一致、以及多设备共享一个物理
// 串口时事务是否被正确串行化。前面那些 fakeSerial 用例都绕过了内核 tty
// 层，只有这里会暴露「代码逻辑对但内核行为不同」的问题。

const (
	ioTIOCSPTLCK = 0x40045431
	ioTIOCGPTN   = 0x80045430
)

// ptyBus 用伪终端模拟 RS-485 总线：主端扮演从站，从端交给驱动。
type ptyBus struct {
	t      *testing.T
	master *os.File
	slave  string
	units  map[byte]map[uint16]uint16 // unit -> holding
	mu     sync.Mutex
	reqs   int
}

func newPTYBus(t *testing.T, units map[byte]map[uint16]uint16) *ptyBus {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
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
	b := &ptyBus{t: t, master: master, slave: fmt.Sprintf("/dev/pts/%d", ptn), units: units}
	t.Cleanup(func() { _ = master.Close() })
	go b.serve()
	return b
}

// serve 在主端上应答所有从站请求。它模拟的是「总线上有若干台从站」。
func (b *ptyBus) serve() {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := b.master.Read(tmp)
		if err != nil {
			return
		}
		buf = append(buf, tmp[:n]...)
		for {
			reqLen, ok := peekRequestLen(buf)
			if !ok || len(buf) < reqLen {
				break
			}
			frame := append([]byte(nil), buf[:reqLen]...)
			buf = buf[reqLen:]
			b.answer(frame)
		}
	}
}

// peekRequestLen 判断缓冲区开头是否已构成一条完整请求帧。
func peekRequestLen(buf []byte) (int, bool) {
	if len(buf) < 2 {
		return 0, false
	}
	switch buf[1] {
	case mbcodec.FCReadCoils, mbcodec.FCReadDiscreteInputs,
		mbcodec.FCReadHoldingRegs, mbcodec.FCReadInputRegs,
		mbcodec.FCWriteSingleCoil, mbcodec.FCWriteSingleRegister:
		return 8, len(buf) >= 8
	case mbcodec.FCWriteMultipleCoils, mbcodec.FCWriteMultipleRegs:
		if len(buf) < 7 {
			return 0, false
		}
		total := 9 + int(buf[6])
		return total, len(buf) >= total
	}
	return 0, false
}

func (b *ptyBus) answer(frame []byte) {
	unit, pdu, err := mbcodec.ParseRTU(frame)
	if err != nil || len(pdu) < 5 {
		return
	}
	b.mu.Lock()
	b.reqs++
	hold, known := b.units[unit]
	b.mu.Unlock()
	if !known {
		return // 总线上没有这个地址，保持静默
	}
	fc := pdu[0]
	addr := uint16(pdu[1])<<8 | uint16(pdu[2])
	qty := uint16(pdu[3])<<8 | uint16(pdu[4])

	var body []byte
	switch fc {
	case mbcodec.FCReadHoldingRegs, mbcodec.FCReadInputRegs:
		body = []byte{fc, byte(qty * 2)}
		for i := uint16(0); i < qty; i++ {
			v := hold[addr+i]
			body = append(body, byte(v>>8), byte(v))
		}
	case mbcodec.FCWriteSingleRegister:
		v := uint16(pdu[3])<<8 | uint16(pdu[4])
		b.mu.Lock()
		hold[addr] = v
		b.mu.Unlock()
		body = pdu[:5]
	default:
		body = []byte{fc | 0x80, 0x01}
	}
	_, _ = b.master.Write(mbcodec.BuildRTU(unit, body))
}

func (b *ptyBus) requestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reqs
}

func openBusDevice(t *testing.T, d *RTU, path string, unit int) driver.Conn {
	t.Helper()
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "bus", Protocol: "modbus-rtu",
			Transport: model.Transport{Kind: "serial", Port: path, Baud: 9600,
				DataBits: 8, Parity: "none", StopBits: 1},
			Timeout: model.Duration(2 * time.Second)},
		Device: model.Device{ID: fmt.Sprintf("dev%d", unit), ChannelID: "bus",
			UnitID: unit, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open unit %d: %v", unit, err)
	}
	return c
}

func TestRTUOverRealPTY(t *testing.T) {
	bus := newPTYBus(t, map[byte]map[uint16]uint16{
		1: {0: 2201, 1: 512},
		2: {0: 2198, 1: 500},
	})

	d := NewRTU()
	c := openBusDevice(t, d, bus.slave, 1)
	defer func() { _ = c.Close() }()

	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("v", "holding", 0, model.TypeUint16, 1),
		regPoint("i", "holding", 1, model.TypeUint16, 1),
	}}})
	if err != nil {
		t.Fatalf("read over pty: %v", err)
	}
	if got[0].Value.Num != 2201 {
		t.Fatalf("voltage = %v", got[0].Value)
	}
	if got[1].Value.Num != 512 {
		t.Fatalf("current = %v", got[1].Value)
	}
	if bus.requestCount() == 0 {
		t.Fatal("slave never saw a request")
	}
}

// 两台从站共享同一物理串口：必须只打开一次，且各答各的。
func TestRTUSharedPortTwoUnitsOverPTY(t *testing.T) {
	bus := newPTYBus(t, map[byte]map[uint16]uint16{
		1: {0: 2201},
		2: {0: 2198},
	})
	// 用一个计数包装器统计真实打开次数。
	opens := 0
	var openMu sync.Mutex
	d := &RTU{Driver: NewWithSerial(func(cfg transport.SerialConfig) (transport.Stream, error) {
		openMu.Lock()
		opens++
		openMu.Unlock()
		return transport.OpenSerial(cfg)
	})}

	c1 := openBusDevice(t, d, bus.slave, 1)
	defer func() { _ = c1.Close() }()
	c2 := openBusDevice(t, d, bus.slave, 2)
	defer func() { _ = c2.Close() }()

	openMu.Lock()
	if opens != 1 {
		openMu.Unlock()
		t.Fatalf("opened serial %d times, want 1 (must share one fd)", opens)
	}
	openMu.Unlock()

	read := func(c driver.Conn) float64 {
		t.Helper()
		got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
			Points: []driver.PointRef{regPoint("v", "holding", 0, model.TypeUint16, 1)}}})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return got[0].Value.Num
	}
	if v := read(c1); v != 2201 {
		t.Fatalf("unit 1 = %v", v)
	}
	if v := read(c2); v != 2198 {
		t.Fatalf("unit 2 = %v", v)
	}
}

// 并发读同一总线：事务必须串行化，响应不能被张冠李戴。
func TestRTUConcurrentDevicesDoNotInterleave(t *testing.T) {
	bus := newPTYBus(t, map[byte]map[uint16]uint16{
		1: {0: 1111},
		2: {0: 2222},
		3: {0: 3333},
	})
	d := NewRTU()
	conns := map[int]driver.Conn{}
	expect := map[int]float64{1: 1111, 2: 2222, 3: 3333}
	for unit := range expect {
		c := openBusDevice(t, d, bus.slave, unit)
		defer func() { _ = c.Close() }()
		conns[unit] = c
	}

	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for round := 0; round < 10; round++ {
		for unit, c := range conns {
			wg.Add(1)
			go func(unit int, c driver.Conn) {
				defer wg.Done()
				got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
					Points: []driver.PointRef{regPoint("v", "holding", 0, model.TypeUint16, 1)}}})
				if err != nil {
					errs <- err
					return
				}
				if got[0].Value.Num != expect[unit] {
					errs <- fmt.Errorf("unit %d got %v, want %v", unit, got[0].Value.Num, expect[unit])
				}
			}(unit, c)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
}

// 沉默的从站（地址不存在）必须超时，而不是永久挂住。
func TestRTUSilentUnitTimesOut(t *testing.T) {
	bus := newPTYBus(t, map[byte]map[uint16]uint16{1: {0: 1}})
	d := NewRTU()
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "bus", Protocol: "modbus-rtu",
			Transport: model.Transport{Kind: "serial", Port: bus.slave, Baud: 9600},
			Timeout:   model.Duration(300 * time.Millisecond)},
		Device: model.Device{ID: "ghost", ChannelID: "bus", UnitID: 99},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Read(ctx, []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("v", "holding", 0, model.TypeUint16, 1)}}})
	if err == nil {
		t.Fatal("read from silent unit must fail")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout not honored: %v", elapsed)
	}
}
