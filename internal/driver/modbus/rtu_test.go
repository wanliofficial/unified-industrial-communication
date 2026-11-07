package modbus

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
	"uic/internal/transport/serialport"
)

// ---------------------------------------------------------------------------
// 内存串口夹具
//
// 真实串口无法在 CI 上稳定复现半包、字节粘连、CRC 错这类故障，而这正是
// RTU 最容易出问题的地方。这里用可控的 Stream 替代，把「物理层怪癖」
// 变成显式可编排的行为。
// ---------------------------------------------------------------------------

type fakeSerial struct {
	mu        sync.Mutex
	writes    [][]byte
	closed    bool
	deadline  time.Time
	drains    int
	handler   func(req []byte) ([][]byte, error) // 返回待交付的字节片段（可模拟半包）
	pending   []byte
	onWriteFn func(req []byte)
}

func (f *fakeSerial) Write(p []byte) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errors.New("closed")
	}
	f.writes = append(f.writes, append([]byte(nil), p...))
	h, hook := f.handler, f.onWriteFn
	f.mu.Unlock()

	if hook != nil {
		hook(p)
	}
	if h == nil {
		return nil
	}
	chunks, err := h(p)
	if err != nil {
		return err
	}
	f.mu.Lock()
	for _, c := range chunks {
		f.pending = append(f.pending, c...)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeSerial) ReadFull(n int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, io.EOF
	}
	if len(f.pending) < n {
		return nil, io.ErrUnexpectedEOF
	}
	out := append([]byte(nil), f.pending[:n]...)
	f.pending = f.pending[n:]
	return out, nil
}

func (f *fakeSerial) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSerial) SetDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline = t
	return nil
}

func (f *fakeSerial) Drain() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
	f.pending = nil
	return nil
}

// rtuSlave 解析主站请求并按 unit 过滤，模拟一台从站。
func rtuSlave(unit byte, holding map[uint16]uint16, coils map[uint16]bool) func([]byte) ([][]byte, error) {
	return func(req []byte) ([][]byte, error) {
		reqUnit, pdu, err := mbcodec.ParseRTU(req)
		if err != nil {
			return nil, err
		}
		if reqUnit != unit {
			return nil, nil // 不是我，保持静默
		}
		if len(pdu) < 5 {
			return nil, nil
		}
		fc := pdu[0]
		addr := uint16(pdu[1])<<8 | uint16(pdu[2])
		qty := uint16(pdu[3])<<8 | uint16(pdu[4])
		switch fc {
		case mbcodec.FCReadHoldingRegs:
			body := []byte{fc, byte(qty * 2)}
			for i := uint16(0); i < qty; i++ {
				v := holding[addr+i]
				body = append(body, byte(v>>8), byte(v))
			}
			return [][]byte{mbcodec.BuildRTU(unit, body)}, nil
		case mbcodec.FCReadCoils:
			n := (qty + 7) / 8
			body := []byte{fc, byte(n)}
			data := make([]byte, n)
			for i := uint16(0); i < qty; i++ {
				if coils[addr+i] {
					data[i/8] |= 1 << (i % 8)
				}
			}
			body = append(body, data...)
			return [][]byte{mbcodec.BuildRTU(unit, body)}, nil
		case mbcodec.FCWriteSingleRegister:
			v := uint16(pdu[3])<<8 | uint16(pdu[4])
			holding[addr] = v
			return [][]byte{mbcodec.BuildRTU(unit, pdu[:5])}, nil
		}
		// 不支持的功能码 → 异常响应 01
		return [][]byte{mbcodec.BuildRTU(unit, []byte{fc | 0x80, 0x01})}, nil
	}
}

func openRTU(t *testing.T, f *fakeSerial, unit int) driver.Conn {
	t.Helper()
	d := NewRTUWithSerial(func(transport.SerialConfig) (transport.Stream, error) { return f, nil })
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "bus1", Protocol: "modbus-rtu",
			Transport: model.Transport{Kind: "serial", Port: "/dev/fake0", Baud: 9600},
			Timeout:   model.Duration(2 * time.Second)},
		Device: model.Device{ID: "dev1", ChannelID: "bus1", UnitID: unit,
			Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRTUMetaDeclaresSerialOnly(t *testing.T) {
	m := NewRTU().Meta()
	if m.ID != "modbus-rtu" {
		t.Fatalf("id = %q", m.ID)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "serial" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if err := driver.Register(NewRTU()); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestRTURejectsTCPTransport(t *testing.T) {
	_, err := NewRTU().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch1", Protocol: "modbus-rtu",
			Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:502"}},
		Device: model.Device{ID: "dev1"},
	})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestRTURejectsBadSerialParams(t *testing.T) {
	d := NewRTUWithSerial(func(transport.SerialConfig) (transport.Stream, error) {
		t.Fatal("opener must not be called for invalid config")
		return nil, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch1", Protocol: "modbus-rtu",
			Transport: model.Transport{Kind: "serial", Port: "/dev/fake0", Baud: 12345}},
		Device: model.Device{ID: "dev1"},
	})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

// 主链路：读保持寄存器，验证编码 → 帧 → 解码全通。
func TestRTUReadHolding(t *testing.T) {
	holding := map[uint16]uint16{0: 1234, 1: 7}
	f := &fakeSerial{handler: rtuSlave(1, holding, nil)}
	c := openRTU(t, f, 1)

	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("a", "holding", 0, model.TypeUint16, 1),
		regPoint("b", "holding", 1, model.TypeUint16, 1),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples", len(got))
	}
	if got[0].Value.Num != 1234 {
		t.Fatalf("a = %v", got[0].Value)
	}
	if got[1].Value.Num != 7 {
		t.Fatalf("b = %v", got[1].Value)
	}

	// 写出去的必须是一条完整 RTU 帧（地址 + PDU + CRC），不是裸 PDU。
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 1 {
		t.Fatalf("writes = %d", len(f.writes))
	}
	if _, _, err := mbcodec.ParseRTU(f.writes[0]); err != nil {
		t.Fatalf("request frame invalid: %v", err)
	}
	if f.drains == 0 {
		t.Fatal("half-duplex: must drain before each request")
	}
}

// 半包：从站把一帧拆成 1 字节一段交付。逐字节边界最容易读错帧长。
func TestRTUHandlesFragmentedResponse(t *testing.T) {
	slave := rtuSlave(1, map[uint16]uint16{0: 0xBEEF}, nil)
	f := &fakeSerial{handler: func(req []byte) ([][]byte, error) {
		chunks, err := slave(req)
		if err != nil || len(chunks) == 0 {
			return chunks, err
		}
		full := chunks[0]
		out := make([][]byte, 0, len(full))
		for i := range full {
			out = append(out, full[i:i+1]) // 每次只到 1 字节
		}
		return out, nil
	}}
	c := openRTU(t, f, 1)

	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 1)}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0].Value.Num != 0xBEEF {
		t.Fatalf("value = %v", got[0].Value)
	}
}

// 粘包：从站一次交付两帧。主站必须只消费第一帧、把余量留到下次，
// 而不是把两帧拼起来当一帧解析。
func TestRTURejectsTrailingBytesInsteadOfMisparsing(t *testing.T) {
	f := &fakeSerial{handler: func(req []byte) ([][]byte, error) {
		_, pdu, err := mbcodec.ParseRTU(req)
		if err != nil {
			return nil, err
		}
		body := []byte{pdu[0], 2, 0x00, 0x01}
		frame := mbcodec.BuildRTU(1, body)
		return [][]byte{frame, frame}, nil // 同一响应交付两次
	}}
	c := openRTU(t, f, 1)
	res, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 1)}}})
	// 第一次读应成功；多余字节在缓冲区里，下轮 Drain 掉。关键是不能串帧。
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res[0].Value.Num != 1 {
		t.Fatalf("value = %v", res[0].Value)
	}
}

func TestRTUDetectsCRCCorruption(t *testing.T) {
	f := &fakeSerial{handler: func(req []byte) ([][]byte, error) {
		_, pdu, err := mbcodec.ParseRTU(req)
		if err != nil {
			return nil, err
		}
		frame := mbcodec.BuildRTU(1, []byte{pdu[0], 2, 0x12, 0x34})
		frame[len(frame)-1] ^= 0xFF // 破坏 CRC
		return [][]byte{frame}, nil
	}}
	c := openRTU(t, f, 1)
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 1)}}})
	if err == nil {
		t.Fatal("CRC corruption must surface as error")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %s", driver.KindOf(err))
	}
	if len(got) != 1 || got[0].Quality != model.QualityBad {
		t.Fatalf("sample = %+v", got)
	}
}

func TestRTUDetectsUnitMismatch(t *testing.T) {
	// 从站回的错误地址：主站必须拒绝，不能当成自己的数据。
	f := &fakeSerial{handler: func(req []byte) ([][]byte, error) {
		return [][]byte{mbcodec.BuildRTU(9, []byte{mbcodec.FCReadHoldingRegs, 2, 0x00, 0x01})}, nil
	}}
	c := openRTU(t, f, 1)
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 1)}}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v kind = %s", err, driver.KindOf(err))
	}
}

func TestRTUExceptionFrameMapsToAddressError(t *testing.T) {
	// 异常帧只有 5 字节，与正常帧长度不同，这条路径必须能正确分帧。
	f := &fakeSerial{handler: func(req []byte) ([][]byte, error) {
		_, pdu, _ := mbcodec.ParseRTU(req)
		return [][]byte{mbcodec.BuildRTU(1, []byte{pdu[0] | 0x80, 0x02})}, nil
	}}
	c := openRTU(t, f, 1)
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 1)}}})
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v kind = %s", err, driver.KindOf(err))
	}
}

func TestRTUFunctionEchoMismatch(t *testing.T) {
	// 功能码不回显说明线上错位（另一台从站在答、或上一轮残留），必须立刻失败。
	f := &fakeSerial{handler: func(req []byte) ([][]byte, error) {
		return [][]byte{mbcodec.BuildRTU(1, []byte{mbcodec.FCReadInputRegs, 2, 0x00, 0x01})}, nil
	}}
	c := openRTU(t, f, 1)
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 1)}}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v kind = %s", err, driver.KindOf(err))
	}
}

func TestRTUWrittenRequestMatchesExpectedLength(t *testing.T) {
	// 主站推导的响应长度必须与实际从站返回长度一致，否则永远读不满。
	holding := map[uint16]uint16{0: 1, 1: 2, 2: 3}
	f := &fakeSerial{handler: rtuSlave(1, holding, nil)}
	c := openRTU(t, f, 1)
	// 一次读 3 个寄存器 → 响应 5 + 6 = 11 字节
	if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{regPoint("a", "holding", 0, model.TypeUint16, 3)}}}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestRTUWriteSingleRegister(t *testing.T) {
	holding := map[uint16]uint16{5: 0}
	f := &fakeSerial{handler: rtuSlave(1, holding, nil)}
	c := openRTU(t, f, 1)

	req := driver.WriteRequest{PointID: "w", Address: model.Address{
		Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: -1},
		DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 4242)}
	res, err := c.Write(context.Background(), []driver.WriteRequest{req})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || res[0].Quality != model.QualityGood {
		t.Fatalf("result = %+v", res)
	}
	if holding[5] != 4242 {
		t.Fatalf("slave value = %d", holding[5])
	}
}

func TestRTUCloseReleasesPort(t *testing.T) {
	// 两个设备共享同一串口，仅当两者都关闭后才真正关闭 fd。
	f := &fakeSerial{handler: rtuSlave(1, map[uint16]uint16{0: 1}, nil)}
	d := NewRTUWithSerial(func(transport.SerialConfig) (transport.Stream, error) { return f, nil })

	open := func(devID string) driver.Conn {
		c, err := d.Open(context.Background(), driver.OpenConfig{
			Channel: model.Channel{ID: "bus1", Protocol: "modbus-rtu",
				Transport: model.Transport{Kind: "serial", Port: "/dev/fake1", Baud: 9600}},
			Device: model.Device{ID: devID, ChannelID: "bus1", UnitID: 1},
		})
		if err != nil {
			t.Fatalf("open %s: %v", devID, err)
		}
		return c
	}
	c1 := open("dev1")
	c2 := open("dev2")

	if n := serialport.Count(); n != 1 {
		t.Fatalf("shared port count = %d, want 1", n)
	}
	_ = c1.Close()
	f.mu.Lock()
	closedEarly := f.closed
	f.mu.Unlock()
	if closedEarly {
		t.Fatal("port closed while a device still holds it")
	}
	_ = c2.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		t.Fatal("port must close after last release")
	}
}

// 同一路径用不同参数打开必须报错：现场配错波特率是常见事故。
func TestAcquirePortRejectsParamConflict(t *testing.T) {
	f := &fakeSerial{}
	opener := func(transport.SerialConfig) (transport.Stream, error) { return f, nil }

	ref, err := serialport.Acquire("rtu", transport.SerialConfig{Port: "/dev/fake2", Baud: 9600}, opener)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = ref.Close() }()

	_, err = serialport.Acquire("rtu", transport.SerialConfig{Port: "/dev/fake2", Baud: 19200}, opener)
	if err == nil {
		t.Fatal("conflicting baud must be rejected")
	}
}

func TestAcquirePortReusesSameConfig(t *testing.T) {
	f := &fakeSerial{}
	opens := 0
	opener := func(transport.SerialConfig) (transport.Stream, error) {
		opens++
		return f, nil
	}
	cfg := transport.SerialConfig{Port: "/dev/fake3", Baud: 9600}
	r1, err := serialport.Acquire("rtu", cfg, opener)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := serialport.Acquire("rtu", cfg, opener)
	if err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("opened %d times, want 1", opens)
	}
	_ = r1.Close()
	_ = r2.Close()
	if n := serialport.Count(); n != 0 {
		t.Fatalf("leaked %d ports", n)
	}
}

func TestEffectiveFrameGap(t *testing.T) {
	// 9600 8N1：10 位/字符 × 3.5 / 9600 ≈ 3.65ms，再加 200μs 余量
	slow := effectiveFrameGap(model.Transport{Baud: 9600, DataBits: 8, StopBits: 1, Parity: "none"})
	if slow < 3*time.Millisecond || slow > 5*time.Millisecond {
		t.Fatalf("9600 gap = %v", slow)
	}
	// 高波特率固定 1750μs
	fast := effectiveFrameGap(model.Transport{Baud: 115200, DataBits: 8, StopBits: 1})
	if fast != 1750*time.Microsecond {
		t.Fatalf("115200 gap = %v", fast)
	}
	// 零值波特率按 9600 处理，不能算出 0 或负数
	if effectiveFrameGap(model.Transport{}) <= 0 {
		t.Fatal("zero baud must fall back to a positive gap")
	}
	// 校验位增加 1 位，间隔应变长
	even := effectiveFrameGap(model.Transport{Baud: 9600, DataBits: 8, StopBits: 1, Parity: "even"})
	if even <= slow {
		t.Fatalf("even parity gap %v should exceed none %v", even, slow)
	}
}
