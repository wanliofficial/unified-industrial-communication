package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	mb "uic/internal/codec/modbus"
	"uic/internal/model"
	"uic/internal/transport"
)

// 串口从站的测试用 net.Pipe 代替真实串口：它同样提供有序字节流、
// SetDeadline 与「对端关闭后读返回错误」，足以验证分帧、地址过滤、
// 校验失败恢复与断线重开。真实 termios 行为由 transport 包自己的测试覆盖。

type fakeSerial struct{ conn net.Conn }

func (f *fakeSerial) Write(p []byte) error {
	_, err := f.conn.Write(p)
	return err
}

func (f *fakeSerial) ReadFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(f.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (f *fakeSerial) Close() error                  { return f.conn.Close() }
func (f *fakeSerial) SetDeadline(t time.Time) error { return f.conn.SetDeadline(t) }
func (f *fakeSerial) Drain() error                  { return nil }

// newSerialServer 建一个跑在管道上的串口从站，返回主站侧的连接。
func newSerialServer(t *testing.T, framing string, slave model.Slave,
	points map[string]model.Point, src Source, w Writer) (*Server, net.Conn) {
	t.Helper()
	slave.Listen = ""
	slave.Serial = &model.SlaveSerial{Port: "/dev/ttyTEST", Framing: framing}
	slaveEnd, master := net.Pipe()

	s, err := New(Options{
		Slave: slave, Points: points, Source: src, Writer: w,
		WriteTimeout: time.Second,
		SerialOpener: func(cfg transport.SerialConfig) (serialPort, error) {
			if cfg.Port != "/dev/ttyTEST" {
				return nil, fmt.Errorf("unexpected port %q", cfg.Port)
			}
			return &fakeSerial{conn: slaveEnd}, nil
		},
	})
	if err != nil {
		t.Fatalf("new serial server: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_ = master.Close()
	})
	return s, master
}

func serialSpans() []model.SlaveUnit {
	return []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}
}

func serialPoints() map[string]model.Point {
	return map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
}

func writeFrame(t *testing.T, c net.Conn, frame []byte) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFrame(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

// expectSilence 断言从站在 d 时间内没有任何字节发出。
func expectSilence(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err == nil {
		t.Fatalf("slave answered when it should stay silent: % x", buf[:n])
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read error = %v, want a timeout", err)
	}
}

func TestSerialRTUServesReads(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 4321)),
	}}
	s, master := newSerialServer(t, "rtu", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), src, &fakeWriter{})

	if got := s.Addr(); got != "/dev/ttyTEST" {
		t.Fatalf("addr = %q, want the serial path", got)
	}

	writeFrame(t, master, mb.BuildRTU(1, readHolding(0, 1)))
	frame := readFrame(t, master, 7) // 地址 1 + PDU 4 + CRC 2
	unit, pdu, err := mb.ParseRTU(frame)
	if err != nil {
		t.Fatalf("parse response % x: %v", frame, err)
	}
	if unit != 1 {
		t.Fatalf("unit = %d, want 1", unit)
	}
	expectRegs(t, pdu, mb.FCReadHoldingRegs, 4321)
}

func TestSerialASCIIRoundTrip(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 77)),
	}}
	_, master := newSerialServer(t, "ascii", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), src, &fakeWriter{})

	req := readHolding(0, 1)
	writeFrame(t, master, mb.BuildASCII(1, req))
	n, err := mb.ASCIIExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("cannot predict ASCII response length: %v", err)
	}
	frame := readFrame(t, master, n)
	unit, pdu, err := mb.ParseASCII(frame)
	if err != nil {
		t.Fatalf("parse response %q: %v", frame, err)
	}
	if unit != 1 {
		t.Fatalf("unit = %d, want 1", unit)
	}
	expectRegs(t, pdu, mb.FCReadHoldingRegs, 77)
}

// 总线上还有别的从站：不是本机地址的请求必须静默，否则两台设备会同时
// 回话，主站看到的是随机串扰。
func TestSerialIgnoresForeignUnits(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 5)),
	}}
	_, master := newSerialServer(t, "rtu", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), src, &fakeWriter{})

	writeFrame(t, master, mb.BuildRTU(9, readHolding(0, 1)))
	expectSilence(t, master, 150*time.Millisecond)

	// 静默之后本机地址仍然要能应答：过滤不能把读循环一起卡死。
	writeFrame(t, master, mb.BuildRTU(1, readHolding(0, 1)))
	frame := readFrame(t, master, 7)
	if _, pdu, err := mb.ParseRTU(frame); err != nil {
		t.Fatalf("parse: %v", err)
	} else {
		expectRegs(t, pdu, mb.FCReadHoldingRegs, 5)
	}
}

func TestSerialRejectsBadCRCAndRecovers(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 9)),
	}}
	s, master := newSerialServer(t, "rtu", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), src, &fakeWriter{})

	bad := mb.BuildRTU(1, readHolding(0, 1))
	bad[len(bad)-1] ^= 0xFF
	writeFrame(t, master, bad)
	expectSilence(t, master, 120*time.Millisecond)

	// 等从站读完静默窗口并清空残字节，再发一条正常请求。
	time.Sleep(60 * time.Millisecond)
	writeFrame(t, master, mb.BuildRTU(1, readHolding(0, 1)))
	frame := readFrame(t, master, 7)
	if _, pdu, err := mb.ParseRTU(frame); err != nil {
		t.Fatalf("parse after crc error: %v", err)
	} else {
		expectRegs(t, pdu, mb.FCReadHoldingRegs, 9)
	}
	if got := s.Stats().BadFrames; got == 0 {
		t.Fatal("bad frame was not counted")
	}

	// FC8 子功能 0x000C 在串口上必须如实报告 CRC 错误数：TCP 上它恒为 0
	// 是事实，串口上再恒为 0 就只是占位了。
	writeFrame(t, master, mb.BuildRTU(1, fc8(0x000C, 0)))
	resp := readFrame(t, master, 8)
	if _, pdu, err := mb.ParseRTU(resp); err != nil {
		t.Fatalf("parse fc8: %v", err)
	} else if got := binary.BigEndian.Uint16(pdu[3:5]); got == 0 {
		t.Fatal("bus communication error count must report the CRC error on serial")
	}
}

// 广播写必须执行但**不得回应**：总线上所有从站会同时收到广播，谁应答
// 都会造成串扰。
func TestSerialBroadcastWriteIsSilent(t *testing.T) {
	writer := &fakeWriter{mu: make(chan struct{}, 1)}
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, true)}
	units := []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0, Writable: true}},
	}}
	_, master := newSerialServer(t, "rtu", model.Slave{ID: "ser", Units: units},
		points, &fakeSource{}, writer)

	writeFrame(t, master, mb.BuildRTU(0, writeOneRegister(0, 777)))
	select {
	case <-writer.mu:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast write was not forwarded")
	}
	expectSilence(t, master, 120*time.Millisecond)

	if len(writer.got) != 1 {
		t.Fatalf("writes = %d, want 1", len(writer.got))
	}
	if got := writer.got[0]; got.Device != "plc" || got.Point != "a" {
		t.Fatalf("write target = %s/%s", got.Device, got.Point)
	}
}

// 未知功能码没有长度表可查，只能靠静默判断帧尾；判断完仍要给对端一个
// 明确的异常码，而不是让它超时。
func TestSerialUnknownFunctionUsesSilenceFraming(t *testing.T) {
	_, master := newSerialServer(t, "rtu", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), &fakeSource{}, &fakeWriter{})

	// 0x41 不是任何已实现的功能码；帧形如标准 8 字节请求。
	body := []byte{1, 0x41, 0x00, 0x00, 0x00, 0x00}
	crc := mb.CRC16(body)
	writeFrame(t, master, append(body, byte(crc), byte(crc>>8)))

	frame := readFrame(t, master, 5)
	_, pdu, err := mb.ParseRTU(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expectException(t, pdu, 0x41, 0x01)
}

// 被截断的帧（对端写到一半掉线）必须丢弃，而不是把下一帧的开头接上去。
func TestSerialDiscardsTruncatedFrame(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 3)),
	}}
	s, master := newSerialServer(t, "rtu", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), src, &fakeWriter{})

	// 只发 4 字节，正常请求是 8 字节。
	writeFrame(t, master, mb.BuildRTU(1, readHolding(0, 1))[:4])
	// 等从站帧内超时（1s）并清空残字节。
	time.Sleep(1200 * time.Millisecond)
	if got := s.Stats().BadFrames; got != 1 {
		t.Fatalf("bad frames = %d, want 1", got)
	}

	writeFrame(t, master, mb.BuildRTU(1, readHolding(0, 1)))
	frame := readFrame(t, master, 7)
	if _, pdu, err := mb.ParseRTU(frame); err != nil {
		t.Fatalf("parse after truncated frame: %v", err)
	} else {
		expectRegs(t, pdu, mb.FCReadHoldingRegs, 3)
	}
}

// 串口掉线后自动重开：USB 适配器短暂消失时不该要求重启整个进程。
func TestSerialReopensAfterLinkLoss(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 11)),
	}}
	masters := make(chan net.Conn, 2)
	s, err := New(Options{
		Slave: model.Slave{ID: "ser", Serial: &model.SlaveSerial{Port: "/dev/ttyTEST", Framing: "rtu"},
			Units: serialSpans()},
		Points: serialPoints(), Source: src, Writer: &fakeWriter{},
		WriteTimeout: time.Second,
		SerialOpener: func(transport.SerialConfig) (serialPort, error) {
			slaveEnd, master := net.Pipe()
			masters <- master
			return &fakeSerial{conn: slaveEnd}, nil
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	first := <-masters
	writeFrame(t, first, mb.BuildRTU(1, readHolding(0, 1)))
	if _, pdu, err := mb.ParseRTU(readFrame(t, first, 7)); err != nil {
		t.Fatalf("parse: %v", err)
	} else {
		expectRegs(t, pdu, mb.FCReadHoldingRegs, 11)
	}

	// 模拟线缆被拔掉：对端关闭后从站读返回错误，应进入重开流程。
	_ = first.Close()

	select {
	case second := <-masters:
		t.Cleanup(func() { _ = second.Close() })
		writeFrame(t, second, mb.BuildRTU(1, readHolding(0, 1)))
		if _, pdu, err := mb.ParseRTU(readFrame(t, second, 7)); err != nil {
			t.Fatalf("parse after reopen: %v", err)
		} else {
			expectRegs(t, pdu, mb.FCReadHoldingRegs, 11)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slave did not reopen the serial link")
	}
}

func TestSerialOpenFailureIsStartupError(t *testing.T) {
	_, err := New(Options{
		Slave: model.Slave{ID: "ser", Serial: &model.SlaveSerial{Port: "/dev/does-not-exist", Framing: "rtu"},
			Units: serialSpans()},
		Points: serialPoints(), Source: &fakeSource{}, Writer: &fakeWriter{},
		SerialOpener: func(transport.SerialConfig) (serialPort, error) {
			return nil, errors.New("device busy")
		},
	})
	if err == nil {
		t.Fatal("expected startup error")
	}
}

// ASCII 的容错分三件事：帧外噪声、LRC 错、截断帧。三者都必须「丢弃这一帧
// 并继续听下一帧」，而不是让从站退出循环——串口上噪声是常态，不是故障。
func TestSerialASCIINoiseAndBadLRC(t *testing.T) {
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 21)),
	}}
	s, master := newSerialServer(t, "ascii", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), src, &fakeWriter{})

	// 帧外噪声：不是 ':' 起始的字节只能逐字节丢弃。
	writeFrame(t, master, []byte("noise!"))

	// LRC 错：长度合法、起始符合法，只有校验和不符。
	bad := mb.BuildASCII(1, readHolding(0, 1))
	if bad[14] == '0' {
		bad[14] = '1'
	} else {
		bad[14] = '0'
	}
	writeFrame(t, master, bad)

	req := readHolding(0, 1)
	writeFrame(t, master, mb.BuildASCII(1, req))
	n, err := mb.ASCIIExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("predict length: %v", err)
	}
	frame := readFrame(t, master, n)
	if _, pdu, err := mb.ParseASCII(frame); err != nil {
		t.Fatalf("parse after noise: %v", err)
	} else {
		expectRegs(t, pdu, mb.FCReadHoldingRegs, 21)
	}
	if got := s.Stats().BadFrames; got < 2 {
		t.Fatalf("bad frames = %d, want at least 2 (noise + lrc)", got)
	}
}

// 串口地址语义在 ASCII 上完全一致：非本机静默、广播只写不回。
func TestSerialASCIIAddressSemantics(t *testing.T) {
	writer := &fakeWriter{mu: make(chan struct{}, 1)}
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, true)}
	units := []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0, Writable: true}},
	}}
	_, master := newSerialServer(t, "ascii", model.Slave{ID: "ser", Units: units},
		points, &fakeSource{}, writer)

	writeFrame(t, master, mb.BuildASCII(8, readHolding(0, 1)))
	expectSilence(t, master, 150*time.Millisecond)

	writeFrame(t, master, mb.BuildASCII(0, writeOneRegister(0, 55)))
	select {
	case <-writer.mu:
	case <-time.After(2 * time.Second):
		t.Fatal("ASCII broadcast write was not forwarded")
	}
	expectSilence(t, master, 120*time.Millisecond)
	if len(writer.got) != 1 {
		t.Fatalf("writes = %d, want 1", len(writer.got))
	}
}

// 未知功能码在 ASCII 上没有长度表，读到 CRLF 即为一帧；仍要回异常码。
func TestSerialASCIIUnknownFunction(t *testing.T) {
	_, master := newSerialServer(t, "ascii", model.Slave{ID: "ser", Units: serialSpans()},
		serialPoints(), &fakeSource{}, &fakeWriter{})

	writeFrame(t, master, mb.BuildASCII(1, []byte{0x41, 0, 0, 0, 0}))
	// 异常响应 PDU 恒 2 字节：':' + 2×(地址+PDU+LRC) + CRLF = 11 字符。
	frame := readFrame(t, master, 11)
	_, pdu, err := mb.ParseASCII(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expectException(t, pdu, 0x41, 0x01)
}

// 帧间静默的上下限都是必需的：下限对抗 USB 转串口的转发延迟，上限防止
// 低波特率下「一条帧的尾巴」被当成静默。
func TestFrameGapClamps(t *testing.T) {
	fast := frameGap(model.SlaveSerial{Baud: 921600, DataBits: 8, Parity: "none", StopBits: 1})
	if fast != 5*time.Millisecond {
		t.Fatalf("fast baud gap = %v, want the 5ms floor", fast)
	}
	slow := frameGap(model.SlaveSerial{Baud: 50, DataBits: 8, Parity: "even", StopBits: 2})
	if slow != 200*time.Millisecond {
		t.Fatalf("slow baud gap = %v, want the 200ms ceiling", slow)
	}
	// 1200 波特下 3.5 个字符约 29ms，在两个钳位之间，必须原样返回。
	mid := frameGap(model.SlaveSerial{Baud: 1200, DataBits: 8, Parity: "none", StopBits: 1})
	if mid < 25*time.Millisecond || mid > 35*time.Millisecond {
		t.Fatalf("1200 baud gap = %v, want ~29ms", mid)
	}
}

// 只有 transport.Stream 三个方法的最小实现也必须能当串口用：SetDeadline
// 与 Drain 是可选能力，缺失时适配器退化成空操作，而不是 panic。
type plainStream struct{ closed bool }

func (p *plainStream) Write(b []byte) error           { return nil }
func (p *plainStream) ReadFull(n int) ([]byte, error) { return make([]byte, n), nil }
func (p *plainStream) Close() error                   { p.closed = true; return nil }

func TestSerialAdapterOptionalCapabilities(t *testing.T) {
	a := serialAdapter{&plainStream{}}
	if err := a.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetDeadline fallback: %v", err)
	}
	if err := a.Drain(); err != nil {
		t.Fatalf("Drain fallback: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
