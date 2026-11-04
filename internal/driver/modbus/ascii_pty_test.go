//go:build linux

package modbus

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
)

// ASCII 的端到端验证必须走真实 tty：ASCII 是文本协议，最常见的现场
// 故障恰恰是「内核把文本改写了」（OPOST/ICRNL 把 \n 变成 \r\n，或
// 把字符回显回来）。这些只能在真 pty 上暴露，内存 Stream 永远测不出来。

// asciiBus 在主端扮演 ASCII 从站集群。
type asciiBus struct {
	t      *testing.T
	master *os.File
	slave  string
	units  map[byte]map[uint16]uint16
	mu     sync.Mutex
	reqs   int
	// 故意在帧前塞噪声，模拟上一次事务残留。
	prefixNoise []byte
}

func newASCIIBus(t *testing.T, units map[byte]map[uint16]uint16) *asciiBus {
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
	b := &asciiBus{t: t, master: master, slave: fmt.Sprintf("/dev/pts/%d", ptn), units: units}
	t.Cleanup(func() { _ = master.Close() })
	go b.serve()
	return b
}

func (b *asciiBus) serve() {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := b.master.Read(tmp)
		if err != nil {
			return
		}
		buf = append(buf, tmp[:n]...)
		// ASCII 以 CRLF 分帧，按行切分即可。
		for {
			idx := strings.IndexByte(string(buf), '\n')
			if idx < 0 {
				break
			}
			frame := append([]byte(nil), buf[:idx+1]...)
			buf = buf[idx+1:]
			b.answer(frame)
		}
	}
}

func (b *asciiBus) answer(frame []byte) {
	unit, pdu, err := mbcodec.ParseASCII(frame)
	if err != nil || len(pdu) < 5 {
		return
	}
	b.mu.Lock()
	b.reqs++
	hold, known := b.units[unit]
	b.mu.Unlock()
	if !known {
		return
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
	resp := mbcodec.BuildASCII(unit, body)
	if len(b.prefixNoise) > 0 {
		resp = append(append([]byte(nil), b.prefixNoise...), resp...)
	}
	_, _ = b.master.Write(resp)
}

func (b *asciiBus) requestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reqs
}

func openASCIIDevice(t *testing.T, d *ASCII, path string, unit int) driver.Conn {
	t.Helper()
	// 故意配 8N1 而不是规范推荐的 7E1：现代设备的默认值，能验证
	// termios 不会因为「ASCII 就该是 7 位」而擅自改动。
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "bus", Protocol: "modbus-ascii",
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

func TestASCIIOverRealPTY(t *testing.T) {
	bus := newASCIIBus(t, map[byte]map[uint16]uint16{
		1: {0: 2201, 1: 512},
		2: {0: 2198, 1: 500},
	})
	d := NewASCII()
	c := openASCIIDevice(t, d, bus.slave, 1)
	defer func() { _ = c.Close() }()

	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("v", "holding", 0, model.TypeUint16, 1),
		regPoint("i", "holding", 1, model.TypeUint16, 1),
	}}})
	if err != nil {
		t.Fatalf("read over pty: %v", err)
	}
	if got[0].Value.Num != 2201 || got[1].Value.Num != 512 {
		t.Fatalf("values = %v/%v", got[0].Value.Num, got[1].Value.Num)
	}
	if bus.requestCount() == 0 {
		t.Fatal("slave never saw a request")
	}
}

// 写后读回：验证 ASCII 的写回显解析正确，而且值真的落到从站。
func TestASCIIWriteThenReadOverPTY(t *testing.T) {
	bus := newASCIIBus(t, map[byte]map[uint16]uint16{1: {5: 0}})
	d := NewASCII()
	c := openASCIIDevice(t, d, bus.slave, 1)
	defer func() { _ = c.Close() }()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "setpoint", Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: -1},
		DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 4242),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("write result = %+v", res)
	}

	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("sp", "holding", 5, model.TypeUint16, 1)}}})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got[0].Value.Num != 4242 {
		t.Fatalf("read back = %v, want 4242", got[0].Value.Num)
	}
}

// 总线上有两台从站时，只有被寻址的那台应答；另一台的响应不能串进来。
func TestASCIITwoUnitsOverPTY(t *testing.T) {
	bus := newASCIIBus(t, map[byte]map[uint16]uint16{
		1: {0: 1111},
		2: {0: 2222},
	})
	d := NewASCII()
	c1 := openASCIIDevice(t, d, bus.slave, 1)
	defer func() { _ = c1.Close() }()
	c2 := openASCIIDevice(t, d, bus.slave, 2)
	defer func() { _ = c2.Close() }()

	read := func(c driver.Conn) float64 {
		t.Helper()
		got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
			Points: []driver.PointRef{regPoint("v", "holding", 0, model.TypeUint16, 1)}}})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return got[0].Value.Num
	}
	if v := read(c1); v != 1111 {
		t.Fatalf("unit 1 = %v", v)
	}
	if v := read(c2); v != 2222 {
		t.Fatalf("unit 2 = %v", v)
	}
}

// 从站在帧前多发了杂散字节（上次事务残留）：主站必须跳过噪声找到 ':'，
// 而不是把第一条请求判成协议错误。这是现场「重启后第一帧必失败」的根因。
func TestASCIISkipsLeadingNoise(t *testing.T) {
	bus := newASCIIBus(t, map[byte]map[uint16]uint16{1: {0: 777}})
	bus.prefixNoise = []byte("garbage\r\n")
	d := NewASCII()
	c := openASCIIDevice(t, d, bus.slave, 1)
	defer func() { _ = c.Close() }()

	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("v", "holding", 0, model.TypeUint16, 1)}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0].Value.Num != 777 {
		t.Fatalf("value = %v", got[0].Value.Num)
	}
}

// 沉默的从站必须按 deadline 超时，不能挂死。
func TestASCIISilentUnitTimesOut(t *testing.T) {
	bus := newASCIIBus(t, map[byte]map[uint16]uint16{1: {0: 1}})
	d := NewASCII()
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "bus", Protocol: "modbus-ascii",
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

// 同一根线上既配 RTU 又配 ASCII 必须报错：两种分帧交错会产生随机
// LRC/CRC 错，日志里完全看不出根因。
func TestASCIIRejectsPortAlreadyUsedByRTU(t *testing.T) {
	bus := newASCIIBus(t, map[byte]map[uint16]uint16{1: {0: 1}})

	rtu := NewRTU()
	rc := openBusDevice(t, rtu, bus.slave, 1)
	defer func() { _ = rc.Close() }()

	ascii := NewASCII()
	_, err := ascii.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "bus", Protocol: "modbus-ascii",
			Transport: model.Transport{Kind: "serial", Port: bus.slave, Baud: 9600,
				DataBits: 8, Parity: "none", StopBits: 1},
			Timeout: model.Duration(time.Second)},
		Device: model.Device{ID: "d-rtu", ChannelID: "bus", UnitID: 1},
	})
	if err == nil {
		t.Fatal("ASCII on an RTU port must be rejected")
	}
	if !strings.Contains(err.Error(), "framing") {
		t.Fatalf("error should explain framing conflict, got: %v", err)
	}
}
