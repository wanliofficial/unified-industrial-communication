package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	sim "uic/internal/sim/modbus"
	"uic/internal/transport"
)

func newSim(t *testing.T) *sim.Server {
	t.Helper()
	s, err := sim.NewServer("127.0.0.1:0", 512)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openConn(t *testing.T, s *sim.Server, unit int) driver.Conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch1", Protocol: "modbus-tcp",
			Transport: model.Transport{Kind: "tcp", Address: s.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
		Device: model.Device{ID: "dev1", ChannelID: "ch1", UnitID: unit,
			Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func regPoint(id, area string, offset int, dt model.DataType, length int) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: dt, Length: length,
		Address: model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Length: length, Bit: -1},
	}
}

func TestMetaSatisfiesRegistry(t *testing.T) {
	d := New()
	m := d.Meta()
	if m.ID != "modbus-tcp" {
		t.Fatalf("id = %q", m.ID)
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported || m.Capabilities.Discovery != driver.CapUnsupported {
		t.Fatal("modbus must not claim subscribe/discovery")
	}
	if err := driver.Register(d); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestOpenRejectsNonTCPTransport(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch1", Protocol: "modbus-tcp",
			Transport: model.Transport{Kind: "memory"}},
		Device: model.Device{ID: "dev1"},
	})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenDialFailureIsDisconnected(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch1", Protocol: "modbus-tcp",
			Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"},
			Timeout:   model.Duration(200 * time.Millisecond)},
		Device: model.Device{ID: "dev1"},
	})
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("err = %v kind = %s", err, driver.KindOf(err))
	}
	if !driver.IsRetryable(err) {
		t.Fatal("dial failure should be retryable")
	}
}

// 真实回环：读保持寄存器并按类型解码。
func TestReadHoldingRegisters(t *testing.T) {
	s := newSim(t)
	// 1234 = 0x04D2；float32 1.5 = 0x3FC0 0x0000
	s.SetHolding(0, 1234)
	s.SetHolding(2, 0x3FC0, 0x0000)
	s.SetHolding(10, 7)
	s.SetHolding(11, 0xBEEF)
	c := openConn(t, s, 1)

	got, err := c.Read(context.Background(), []driver.ReadSpan{
		{ID: "s1", Points: []driver.PointRef{
			regPoint("u16", "holding", 0, model.TypeUint16, 1),
			regPoint("f32", "holding", 2, model.TypeFloat32, 2),
		}},
		{ID: "s2", Points: []driver.PointRef{
			regPoint("u32", "holding", 10, model.TypeUint32, 2),
		}},
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("samples = %+v", got)
	}
	byID := map[string]driver.RawSample{}
	for _, sm := range got {
		byID[sm.PointID] = sm
	}
	if v := byID["u16"].Value.Num; v != 1234 {
		t.Fatalf("u16 = %v", v)
	}
	if v := byID["f32"].Value.Num; v != 1.5 {
		t.Fatalf("f32 = %v", v)
	}
	if v := byID["u32"].Value.Num; v != 0x0007BEEF {
		t.Fatalf("u32 = %#x", uint32(v))
	}
	for id, sm := range byID {
		if sm.Quality != model.QualityGood {
			t.Fatalf("%s quality = %s", id, sm.Quality)
		}
	}
}

// 字节序必须逐点生效：同一份寄存器用不同字节序解出不同值，且与 codec 的约定一致。
func TestReadByteOrder(t *testing.T) {
	s := newSim(t)
	s.SetHolding(0, 0x3FC0, 0x0000) // 大端 = 1.5
	c := openConn(t, s, 1)

	p := regPoint("f", "holding", 0, model.TypeFloat32, 2)
	p.ByteOrder = model.OrderCDAB // 字交换：0x0000 0x3FC0 = 1.08e-41
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value.Num == 1.5 {
		t.Fatalf("byte order not applied: %+v", got)
	}
	if got[0].Quality != model.QualityGood {
		t.Fatalf("quality = %s", got[0].Quality)
	}
}

func TestReadCoilsAndDiscrete(t *testing.T) {
	s := newSim(t)
	s.SetCoil(3, true)
	s.SetCoil(4, false)
	c := openConn(t, s, 1)

	coil := driver.PointRef{ID: "c3", DataType: model.TypeBool, Length: 1,
		Address: model.Address{Kind: model.AddressRegister, Area: "coil", Offset: 3, Bit: -1}}
	disc := driver.PointRef{ID: "d0", DataType: model.TypeBool, Length: 1,
		Address: model.Address{Kind: model.AddressRegister, Area: "discrete", Offset: 0, Bit: -1}}
	got, err := c.Read(context.Background(), []driver.ReadSpan{
		{ID: "s1", Points: []driver.PointRef{coil}},
		{ID: "s2", Points: []driver.PointRef{disc}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Value.Bool || got[1].Value.Bool {
		t.Fatalf("coils = %+v", got)
	}
}

// 位访问：bit >= 0 时从寄存器取位。
func TestReadRegisterBit(t *testing.T) {
	s := newSim(t)
	s.SetHolding(5, 0b1000)
	c := openConn(t, s, 1)
	bit3 := regPoint("b3", "holding", 5, model.TypeBool, 1)
	bit3.Address.Bit = 3
	bit0 := regPoint("b0", "holding", 5, model.TypeBool, 1)
	bit0.Address.Bit = 0
	got, err := c.Read(context.Background(), []driver.ReadSpan{
		{ID: "s", Points: []driver.PointRef{bit3, bit0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Value.Bool || got[1].Value.Bool {
		t.Fatalf("bits = %+v", got)
	}
}

// 越界读返回异常码 02 → address 错误，质量是 address_error 而不是 bad。
func TestOutOfRangeReadIsAddressError(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	// 从站容量 512，读 510..513 越界。
	p := regPoint("oob", "holding", 510, model.TypeUint32, 4)
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil {
		t.Fatal("expected error")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %s err = %v", driver.KindOf(err), err)
	}
	if len(got) != 1 || got[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", got)
	}
	if got[0].PointID != "oob" {
		t.Fatalf("point id lost: %+v", got[0])
	}
}

// 一个 span 失败不应吞掉另一个 span 的成功结果。
func TestPartialSpanFailureKeepsGoodSamples(t *testing.T) {
	s := newSim(t)
	s.SetHolding(0, 99)
	c := openConn(t, s, 1)
	bad := regPoint("bad", "holding", 5000, model.TypeUint16, 1)
	good := regPoint("good", "holding", 0, model.TypeUint16, 1)
	got, err := c.Read(context.Background(), []driver.ReadSpan{
		{ID: "s1", Points: []driver.PointRef{bad}},
		{ID: "s2", Points: []driver.PointRef{good}},
	})
	if err != nil {
		t.Fatalf("partial failure must not fail the whole read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %+v", got)
	}
	if got[0].Quality != model.QualityAddressError {
		t.Fatalf("bad quality = %s", got[0].Quality)
	}
	if got[1].Quality != model.QualityGood || got[1].Value.Num != 99 {
		t.Fatalf("good sample = %+v", got[1])
	}
}

func TestWriteSingleAndMultipleRegisters(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "w1", DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 4242),
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 7, Length: 1, Bit: -1},
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK || res[0].Quality != model.QualityGood {
		t.Fatalf("result = %+v", res)
	}
	if got := s.Holding(7); got != 4242 {
		t.Fatalf("holding[7] = %d", got)
	}

	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "w2", DataType: model.TypeFloat32, Value: model.NumValue(model.TypeFloat32, 2.5),
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 20, Length: 2, Bit: -1},
	}})
	if err != nil {
		t.Fatalf("write float: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	if a, b := s.Holding(20), s.Holding(21); a != 0x4020 || b != 0x0000 {
		t.Fatalf("float32 words = %#x %#x", a, b)
	}
}

func TestWriteCoil(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "c1", DataType: model.TypeBool, Value: model.BoolValue(true),
		Address: model.Address{Kind: model.AddressRegister, Area: "coil", Offset: 2, Length: 1, Bit: -1},
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
		ID: "c1", DataType: model.TypeBool, Length: 1,
		Address: model.Address{Kind: model.AddressRegister, Area: "coil", Offset: 2, Bit: -1},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Value.Bool {
		t.Fatalf("coil read = %+v", got)
	}
}

// TestWriteRegisterBitReadModifyWrite 验证「只改一位，其它位不动」。
func TestWriteRegisterBitReadModifyWrite(t *testing.T) {
	s := newSim(t)
	// bit15..bit12 = 0xF，bit3 = 1，bit0 = 1：其余位必须原样保留。
	s.SetHolding(5, 0xF009)
	c := openConn(t, s, 1)
	ctx := context.Background()

	bitReq := func(v bool) driver.WriteRequest {
		return driver.WriteRequest{
			PointID: "b3", DataType: model.TypeBool, Value: model.BoolValue(v),
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: 3},
		}
	}
	res, err := c.Write(ctx, []driver.WriteRequest{bitReq(false)})
	if err != nil {
		t.Fatalf("clear bit3: %v", err)
	}
	if len(res) != 1 || !res[0].OK || res[0].Quality != model.QualityGood {
		t.Fatalf("clear bit3 result = %+v", res)
	}
	if got := s.Holding(5); got != 0xF001 {
		t.Fatalf("after clear: holding[5] = %#04x, want 0xF001", got)
	}

	res, err = c.Write(ctx, []driver.WriteRequest{bitReq(true)})
	if err != nil || !res[0].OK {
		t.Fatalf("set bit3: %+v %v", res, err)
	}
	if got := s.Holding(5); got != 0xF009 {
		t.Fatalf("after set: holding[5] = %#04x, want 0xF009", got)
	}

	// 批量：位写与整字写混在一批里，互不影响。
	res, err = c.Write(ctx, []driver.WriteRequest{
		bitReq(false),
		{PointID: "w", DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 0x00FF),
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 9, Length: 1, Bit: -1}},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !res[0].OK || !res[1].OK {
		t.Fatalf("batch results = %+v", res)
	}
	if got := s.Holding(5); got != 0xF001 {
		t.Fatalf("batch bit write = %#04x", got)
	}
	if got := s.Holding(9); got != 0x00FF {
		t.Fatalf("batch word write = %#04x", got)
	}
}

// TestWriteRegisterBitRequiresBit 明确拒绝「往整字写真假」：0/1 都不是
// 用户想要的意思，静默选一个就会覆盖其它 15 位。
func TestWriteRegisterBitRequiresBit(t *testing.T) {
	s := newSim(t)
	s.SetHolding(5, 0xFF00)
	c := openConn(t, s, 1)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "b", DataType: model.TypeBool, Value: model.BoolValue(true),
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: -1},
	}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	if got := s.Holding(5); got != 0xFF00 {
		t.Fatalf("holding changed to %#04x", got)
	}
}

// TestWriteRegisterBitReadFails 验证读回失败时不会继续写：宁可这次失败，
// 也不能在「不知道其它位」的情况下覆盖整字。
func TestWriteRegisterBitReadFails(t *testing.T) {
	s := newSim(t)
	s.SetHolding(5, 0xFFFF)
	c := openConn(t, s, 1)
	_ = s.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "b", DataType: model.TypeBool, Value: model.BoolValue(false),
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 5, Length: 1, Bit: 3},
	}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if res[0].OK || res[0].Quality == model.QualityGood {
		t.Fatalf("result = %+v", res)
	}
}

// TestWriteRegisterBitRejectsReadOnlyArea 保证 input/discrete 不会被当成
// holding 的同一偏移写进去。
func TestWriteRegisterBitRejectsReadOnlyArea(t *testing.T) {
	s := newSim(t)
	s.SetHolding(5, 0xFF00)
	c := openConn(t, s, 1)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "b", DataType: model.TypeBool, Value: model.BoolValue(true),
		Address: model.Address{Kind: model.AddressRegister, Area: "input", Offset: 5, Length: 1, Bit: 3},
	}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	if got := s.Holding(5); got != 0xFF00 {
		t.Fatalf("holding[5] changed to %#04x", got)
	}
}

// 写只读区（input）必须逐项失败，且不影响同批次其他项。
func TestWriteOneFailureDoesNotAbortBatch(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "bad", DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1),
			Address: model.Address{Kind: model.AddressRegister, Area: "input", Offset: 0, Length: 1, Bit: -1}},
		{PointID: "good", DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 5),
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 1, Bit: -1}},
	})
	if err == nil {
		t.Fatal("expected an error for the failed item")
	}
	if len(res) != 2 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].OK || res[0].Quality == model.QualityGood {
		t.Fatalf("bad item = %+v", res[0])
	}
	if !res[1].OK {
		t.Fatalf("good item = %+v", res[1])
	}
	if got := s.Holding(0); got != 5 {
		t.Fatalf("holding[0] = %d", got)
	}
}

func TestProbe(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// 关闭从站后探活必须失败并被归类为可重试。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := c.Probe(context.Background())
	if err == nil {
		t.Fatal("probe on closed server should fail")
	}
	if !driver.IsRetryable(err) && driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestReadHonorsCanceledContext(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("p", "holding", 0, model.TypeUint16, 1),
	}}})
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v kind = %s", err, driver.KindOf(err))
	}
}

func TestEmptyReadIsNoop(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	got, err := c.Read(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty read = %+v %v", got, err)
	}
	res, err := c.Write(context.Background(), nil)
	if err != nil || len(res) != 0 {
		t.Fatalf("empty write = %+v %v", res, err)
	}
}

// ---------------------------------------------------------------------------
// 用假流测试分帧与网络错误分类（真实 TCP 难以稳定复现半包/断流）
// ---------------------------------------------------------------------------

type scriptedStream struct {
	mu      sync.Mutex
	writes  [][]byte
	respond func(req []byte) ([]byte, error) // 返回完整 ADU（含 7 字节 MBAP）
	pending []byte
	closed  bool
}

func (s *scriptedStream) Write(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("closed")
	}
	s.writes = append(s.writes, append([]byte(nil), p...))
	adu, err := s.respond(p)
	if err != nil {
		return err
	}
	s.pending = adu
	return nil
}

// ReadFull 按 MBAP 分帧需要逐段交付：先 7 字节头，再 body。
func (s *scriptedStream) ReadFull(n int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, io.EOF
	}
	if len(s.pending) < n {
		return nil, io.ErrUnexpectedEOF
	}
	out := append([]byte(nil), s.pending[:n]...)
	s.pending = s.pending[n:]
	return out, nil
}

func (s *scriptedStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func openScripted(t *testing.T, respond func(req []byte) ([]byte, error)) driver.Conn {
	t.Helper()
	d := NewWithDialer(func(context.Context, string, time.Duration) (transport.Stream, error) {
		return &scriptedStream{respond: respond}, nil
	})
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch1", Protocol: "modbus-tcp",
			Transport: model.Transport{Kind: "tcp", Address: "fake:502"}},
		Device: model.Device{ID: "dev1", UnitID: 1},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return c
}

// adu 把 PDU 包装成合法 MBAP 帧，回显事务号。
func adu(req []byte, pdu []byte) []byte {
	out := make([]byte, 7+len(pdu))
	copy(out[0:2], req[0:2])
	out[2], out[3] = 0, 0
	binary.BigEndian.PutUint16(out[4:6], uint16(len(pdu)+1))
	out[6] = req[6]
	copy(out[7:], pdu)
	return out
}

// MBAP 长度字段异常必须是 protocol 错误（不可重试），而不是默默读下一帧。
func TestInvalidMBAPLengthIsProtocolError(t *testing.T) {
	c := openScripted(t, func(req []byte) ([]byte, error) {
		// 7 字节头，length=1（非法，最小 2），后面没有 body。
		return []byte{req[0], req[1], 0, 0, 0, 1, 1}, nil
	})
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("p", "holding", 0, model.TypeUint16, 1),
	}}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v kind = %s", err, driver.KindOf(err))
	}
	if driver.IsRetryable(err) {
		t.Fatal("protocol error must not be retryable")
	}
}

// 读头部时 EOF → disconnected（可重试）。
func TestEOFMapsToDisconnected(t *testing.T) {
	c := openScripted(t, func([]byte) ([]byte, error) {
		return nil, nil // pending 为空 → ReadFull 返回 EOF
	})
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("p", "holding", 0, model.TypeUint16, 1),
	}}})
	if driver.KindOf(err) != driver.ErrDisconnected || !driver.IsRetryable(err) {
		t.Fatalf("err = %v kind = %s retryable=%v", err, driver.KindOf(err), driver.IsRetryable(err))
	}
}

// 事务号必须逐次递增，否则某些从站会丢弃重复请求。
func TestTransactionIDIncrements(t *testing.T) {
	var mu sync.Mutex
	var txs []uint16
	c := openScripted(t, func(req []byte) ([]byte, error) {
		fc := req[7]
		mu.Lock()
		txs = append(txs, binary.BigEndian.Uint16(req[0:2]))
		mu.Unlock()
		return adu(req, []byte{fc | 0x80, 2}), nil
	})
	p := regPoint("p", "holding", 0, model.TypeUint16, 1)
	for i := 0; i < 3; i++ {
		_, _ = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(txs) != 3 {
		t.Fatalf("writes = %d", len(txs))
	}
	for i := 1; i < len(txs); i++ {
		if txs[i] == txs[i-1] {
			t.Fatalf("transaction id reused: %v", txs)
		}
	}
}

// 从站返回异常码 01（非法功能）→ protocol，质量 bad。
func TestUnknownFunctionExceptionIsBadQuality(t *testing.T) {
	c := openScripted(t, func(req []byte) ([]byte, error) {
		return adu(req, []byte{req[7] | 0x80, 1}), nil
	})
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		regPoint("p", "holding", 0, model.TypeUint16, 1),
	}}})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(got) != 1 || got[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v", got)
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %s", driver.KindOf(err))
	}
}

// Close 必须能重复调用（重连路径会先 Close 再 Open）。
func TestCloseIsIdempotent(t *testing.T) {
	s := newSim(t)
	c := openConn(t, s, 1)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
