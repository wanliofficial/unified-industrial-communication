package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"testing"
	"time"

	mb "uic/internal/codec/modbus"
	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

type fakeSource struct {
	mu      map[string]model.Sample
	samples map[string]model.Sample
}

func (f *fakeSource) Sample(device, point string) (model.Sample, bool) {
	s, ok := f.samples[device+"/"+point]
	return s, ok
}

type fakeWriter struct {
	mu   chan struct{}
	got  []WriteRequest
	fail bool
}

func (f *fakeWriter) Write(_ context.Context, reqs []WriteRequest) []WriteResult {
	if f.mu != nil {
		f.mu <- struct{}{}
	}
	f.got = append(f.got, reqs...)
	out := make([]WriteResult, len(reqs))
	for i := range out {
		if f.fail {
			out[i] = WriteResult{OK: false, Err: errors.New("device refused")}
			continue
		}
		out[i] = WriteResult{OK: true}
	}
	return out
}

func goodSample(device, point string, v model.Value) model.Sample {
	return model.Sample{DeviceID: device, PointID: point, Value: v, Quality: model.QualityGood, CollectedAt: time.Now()}
}

func badSample(device, point string, v model.Value) model.Sample {
	return model.Sample{DeviceID: device, PointID: point, Value: v, Quality: model.QualityCommError, CollectedAt: time.Now()}
}

// pt 构造一个点位。regs 是它在寄存器区占用的寄存器个数（string 为
// 寄存器个数，不是字符数）——这正是映射宽度，也是配置里 address.length
// 的含义。
func pt(dt model.DataType, regs int, writable bool) model.Point {
	return model.Point{
		ID: "p", DataType: dt, Writable: writable, Enabled: true,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: regs, Bit: -1},
	}
}

func newServer(t *testing.T, slave model.Slave, points map[string]model.Point, src Source, w Writer) *Server {
	t.Helper()
	if slave.Listen == "" {
		slave.Listen = "127.0.0.1:0"
	}
	s, err := New(Options{Slave: slave, Points: points, Source: src, Writer: w, WriteTimeout: time.Second})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---------------------------------------------------------------------------
// 原始客户端
// ---------------------------------------------------------------------------

type client struct {
	conn net.Conn
	tx   uint16
}

func dial(t *testing.T, s *Server, unit byte) *client {
	t.Helper()
	c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{conn: c, tx: 100}
}

func (c *client) roundTrip(t *testing.T, unit byte, pdu []byte) []byte {
	t.Helper()
	c.tx++
	frame := mb.BuildTCP(mb.Frame{Transaction: c.tx, Unit: unit, PDU: pdu})
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, 7)
	if _, err := io.ReadFull(c.conn, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if got := binary.BigEndian.Uint16(head[0:2]); got != c.tx {
		t.Fatalf("transaction = %d, want %d", got, c.tx)
	}
	n := int(binary.BigEndian.Uint16(head[4:6]))
	body := make([]byte, n-1)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func readHolding(addr, qty int) []byte {
	pdu := []byte{mb.FCReadHoldingRegs, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	return pdu
}

func readCoils(addr, qty int) []byte {
	pdu := []byte{mb.FCReadCoils, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	return pdu
}

func writeOneRegister(addr int, v uint16) []byte {
	pdu := []byte{mb.FCWriteSingleRegister, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], v)
	return pdu
}

func writeRegisters(addr int, vals ...uint16) []byte {
	pdu := []byte{mb.FCWriteMultipleRegs, 0, 0, 0, 0, byte(len(vals) * 2)}
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(len(vals)))
	for _, v := range vals {
		pdu = append(pdu, byte(v>>8), byte(v))
	}
	return pdu
}

func expectException(t *testing.T, resp []byte, fc, code byte) {
	t.Helper()
	if len(resp) != 2 || resp[0] != fc|0x80 || resp[1] != code {
		t.Fatalf("response = % x, want exception fc=%#x code=%#x", resp, fc, code)
	}
}

func expectRegs(t *testing.T, resp []byte, fc byte, want ...uint16) {
	t.Helper()
	if len(resp) < 2 || resp[0] != fc {
		t.Fatalf("response = % x, want fc %#x", resp, fc)
	}
	if int(resp[1]) != len(want)*2 {
		t.Fatalf("byte count = %d, want %d", resp[1], len(want)*2)
	}
	for i, w := range want {
		if got := binary.BigEndian.Uint16(resp[2+i*2:]); got != w {
			t.Fatalf("reg[%d] = %#x, want %#x", i, got, w)
		}
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func TestReadHoldingAcrossMappings(t *testing.T) {
	// offset 0: float32 12.5 (2 regs)；offset 2: uint16 7；offset 3: int16 -3
	points := map[string]model.Point{
		"plc/temp":     pt(model.TypeFloat32, 2, false),
		"plc/count":    pt(model.TypeUint16, 1, false),
		"plc/negative": pt(model.TypeInt16, 1, false),
	}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/temp":     goodSample("plc", "temp", model.NumValue(model.TypeFloat32, 12.5)),
		"plc/count":    goodSample("plc", "count", model.NumValue(model.TypeUint16, 7)),
		"plc/negative": goodSample("plc", "negative", model.NumValue(model.TypeInt16, -3)),
	}}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID: 1,
		Mappings: []model.SlaveMapping{
			{Point: "plc/temp", Area: model.MappingHolding, Offset: 0},
			{Point: "plc/count", Area: model.MappingHolding, Offset: 2},
			{Point: "plc/negative", Area: model.MappingHolding, Offset: 3},
		},
	}}}, points, src, &fakeWriter{})

	c := dial(t, s, 1)
	resp := c.roundTrip(t, 1, readHolding(0, 4))
	// 12.5 = 0x41480000 → 0x4148、0x0000
	expectRegs(t, resp, mb.FCReadHoldingRegs, 0x4148, 0x0000, 7, 0xFFFD)
}

func TestReadHalfOfFloatIsAllowed(t *testing.T) {
	// 主站先读 1 个寄存器试探是常见行为，拒绝会让它直接放弃。
	points := map[string]model.Point{"plc/temp": pt(model.TypeFloat32, 2, false)}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/temp": goodSample("plc", "temp", model.NumValue(model.TypeFloat32, 12.5)),
	}}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/temp", Area: model.MappingHolding, Offset: 10}},
	}}}, points, src, &fakeWriter{})

	resp := dial(t, s, 1).roundTrip(t, 1, readHolding(11, 1))
	expectRegs(t, resp, mb.FCReadHoldingRegs, 0x0000)
}

func TestReadHoleIsException(t *testing.T) {
	points := map[string]model.Point{
		"plc/a": pt(model.TypeUint16, 1, false),
		"plc/b": pt(model.TypeUint16, 1, false),
	}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 1)),
		"plc/b": goodSample("plc", "b", model.NumValue(model.TypeUint16, 2)),
	}}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID: 1,
		Mappings: []model.SlaveMapping{
			{Point: "plc/a", Area: model.MappingHolding, Offset: 0},
			// 故意在 1 处留一个空洞
			{Point: "plc/b", Area: model.MappingHolding, Offset: 2},
		},
	}}}, points, src, &fakeWriter{})

	resp := dial(t, s, 1).roundTrip(t, 1, readHolding(0, 3))
	expectException(t, resp, mb.FCReadHoldingRegs, 0x02)
}

func TestReadCoilsAndDiscrete(t *testing.T) {
	points := map[string]model.Point{
		"plc/run":  pt(model.TypeBool, 1, false),
		"plc/stop": pt(model.TypeBool, 1, false),
	}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/run":  goodSample("plc", "run", model.BoolValue(true)),
		"plc/stop": goodSample("plc", "stop", model.BoolValue(false)),
	}}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID: 1,
		Mappings: []model.SlaveMapping{
			{Point: "plc/run", Area: model.MappingCoil, Offset: 0},
			{Point: "plc/stop", Area: model.MappingCoil, Offset: 1},
			{Point: "plc/run", Area: model.MappingDiscrete, Offset: 0},
		},
	}}}, points, src, &fakeWriter{})

	c := dial(t, s, 1)
	resp := c.roundTrip(t, 1, readCoils(0, 2))
	if len(resp) != 3 || resp[0] != mb.FCReadCoils || resp[1] != 1 || resp[2] != 0x01 {
		t.Fatalf("coils = % x", resp)
	}
	// 离散输入区只读，读法与线圈一致。
	pdu := readCoils(0, 1)
	pdu[0] = mb.FCReadDiscreteInputs
	resp = c.roundTrip(t, 1, pdu)
	if len(resp) != 3 || resp[0] != mb.FCReadDiscreteInputs || resp[2] != 0x01 {
		t.Fatalf("discrete = % x", resp)
	}
}

func TestByteOrderIsApplied(t *testing.T) {
	// CDAB：12.5 = 4148 0000 → 0000 4148
	points := map[string]model.Point{"plc/temp": pt(model.TypeFloat32, 2, false)}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/temp": goodSample("plc", "temp", model.NumValue(model.TypeFloat32, 12.5)),
	}}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID: 1, ByteOrder: model.OrderCDAB,
		Mappings: []model.SlaveMapping{{Point: "plc/temp", Area: model.MappingHolding, Offset: 0}},
	}}}, points, src, &fakeWriter{})

	resp := dial(t, s, 1).roundTrip(t, 1, readHolding(0, 2))
	expectRegs(t, resp, mb.FCReadHoldingRegs, 0x0000, 0x4148)
}

func TestUnknownUnitReturnsGatewayError(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}}, points, &fakeSource{samples: map[string]model.Sample{}}, &fakeWriter{})

	resp := dial(t, s, 9).roundTrip(t, 9, readHolding(0, 1))
	expectException(t, resp, mb.FCReadHoldingRegs, 0x0B)
}

func TestNoSampleIsServerFailure(t *testing.T) {
	// 网关刚启动、还没采到值：0x04 比返回 0 诚实。
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}}, points, &fakeSource{samples: map[string]model.Sample{}}, &fakeWriter{})

	resp := dial(t, s, 1).roundTrip(t, 1, readHolding(0, 1))
	expectException(t, resp, mb.FCReadHoldingRegs, 0x04)
}

func TestBadQualityServesLastValueAndCounts(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": badSample("plc", "a", model.NumValue(model.TypeUint16, 42)),
	}}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}}, points, src, &fakeWriter{})

	resp := dial(t, s, 1).roundTrip(t, 1, readHolding(0, 1))
	expectRegs(t, resp, mb.FCReadHoldingRegs, 42)
	if got := s.Stats().Degraded; got != 1 {
		t.Fatalf("degraded = %d, want 1", got)
	}
}

func TestIllegalFunction(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}}, points, &fakeSource{}, &fakeWriter{})

	resp := dial(t, s, 1).roundTrip(t, 1, []byte{0x2B, 0x0E, 0x01, 0x00})
	expectException(t, resp, 0x2B, 0x01)
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func TestWriteSingleRegisterForwards(t *testing.T) {
	points := map[string]model.Point{"plc/setpoint": pt(model.TypeUint16, 1, true)}
	w := &fakeWriter{}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/setpoint", Area: model.MappingHolding, Offset: 5, Writable: true}},
	}}}, points, &fakeSource{}, w)

	resp := dial(t, s, 1).roundTrip(t, 1, writeOneRegister(5, 1234))
	if len(resp) != 5 || resp[0] != mb.FCWriteSingleRegister {
		t.Fatalf("echo = % x", resp)
	}
	if len(w.got) != 1 || w.got[0].Device != "plc" || w.got[0].Point != "setpoint" {
		t.Fatalf("forwarded = %+v", w.got)
	}
	if w.got[0].Value.Num != 1234 {
		t.Fatalf("value = %v", w.got[0].Value.Num)
	}
}

func TestWriteMultipleRegistersSplitsPerPoint(t *testing.T) {
	points := map[string]model.Point{
		"plc/a": pt(model.TypeUint16, 1, true),
		"plc/b": pt(model.TypeFloat32, 2, true),
	}
	w := &fakeWriter{}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID: 1,
		Mappings: []model.SlaveMapping{
			{Point: "plc/a", Area: model.MappingHolding, Offset: 0, Writable: true},
			{Point: "plc/b", Area: model.MappingHolding, Offset: 1, Writable: true},
		},
	}}}, points, &fakeSource{}, w)

	resp := dial(t, s, 1).roundTrip(t, 1, writeRegisters(0, 9, 0x4148, 0x0000))
	if len(resp) != 5 || resp[0] != mb.FCWriteMultipleRegs {
		t.Fatalf("echo = % x", resp)
	}
	if len(w.got) != 2 {
		t.Fatalf("forwarded %d writes, want 2 (one per point): %+v", len(w.got), w.got)
	}
	if w.got[0].Value.Num != 9 {
		t.Fatalf("a = %v", w.got[0].Value.Num)
	}
	if math.Abs(w.got[1].Value.Num-12.5) > 1e-6 {
		t.Fatalf("b = %v", w.got[1].Value.Num)
	}
}

func TestWritePartialPointIsRejected(t *testing.T) {
	points := map[string]model.Point{"plc/b": pt(model.TypeFloat32, 2, true)}
	w := &fakeWriter{}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/b", Area: model.MappingHolding, Offset: 0, Writable: true}},
	}}}, points, &fakeSource{}, w)

	// 只写 float32 的第一个寄存器：会让设备收到撕裂值。
	resp := dial(t, s, 1).roundTrip(t, 1, writeOneRegister(0, 0x4148))
	expectException(t, resp, mb.FCWriteSingleRegister, 0x02)

	resp = dial(t, s, 1).roundTrip(t, 1, writeRegisters(0, 0x4148))
	expectException(t, resp, mb.FCWriteMultipleRegs, 0x02)

	if len(w.got) != 0 {
		t.Fatalf("nothing may be forwarded, got %+v", w.got)
	}
}

func TestWriteToReadOnlyMappingIsRejected(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	w := &fakeWriter{}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}}, points, &fakeSource{}, w)

	resp := dial(t, s, 1).roundTrip(t, 1, writeOneRegister(0, 1))
	expectException(t, resp, mb.FCWriteSingleRegister, 0x02)
	if len(w.got) != 0 {
		t.Fatalf("write must not be forwarded: %+v", w.got)
	}
}

func TestWriteMultipleCoils(t *testing.T) {
	points := map[string]model.Point{
		"plc/a": pt(model.TypeBool, 1, true),
		"plc/b": pt(model.TypeBool, 1, true),
		"plc/c": pt(model.TypeBool, 1, true),
	}
	w := &fakeWriter{}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID: 1,
		Mappings: []model.SlaveMapping{
			{Point: "plc/a", Area: model.MappingCoil, Offset: 0, Writable: true},
			{Point: "plc/b", Area: model.MappingCoil, Offset: 1, Writable: true},
			{Point: "plc/c", Area: model.MappingCoil, Offset: 2, Writable: true},
		},
	}}}, points, &fakeSource{}, w)

	// 0b101：a=on、b=off、c=on
	pdu := []byte{mb.FCWriteMultipleCoils, 0, 0, 0, 3, 1, 0b101}
	resp := dial(t, s, 1).roundTrip(t, 1, pdu)
	if len(resp) != 5 || resp[0] != mb.FCWriteMultipleCoils {
		t.Fatalf("echo = % x", resp)
	}
	if len(w.got) != 3 {
		t.Fatalf("forwarded %d, want 3: %+v", len(w.got), w.got)
	}
	want := []bool{true, false, true}
	for i, b := range want {
		if w.got[i].Value.Bool != b {
			t.Fatalf("coil %d = %v, want %v", i, w.got[i].Value.Bool, b)
		}
	}
}

func TestInvalidCoilValueIsIllegalDataValue(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeBool, 1, true)}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingCoil, Offset: 0, Writable: true}},
	}}}, points, &fakeSource{}, &fakeWriter{})

	pdu := []byte{mb.FCWriteSingleCoil, 0, 0, 0x12, 0x34}
	resp := dial(t, s, 1).roundTrip(t, 1, pdu)
	expectException(t, resp, mb.FCWriteSingleCoil, 0x03)
}

func TestWriteFailureIsServerFailure(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, true)}
	w := &fakeWriter{fail: true}
	s := newServer(t, model.Slave{ID: "s", Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0, Writable: true}},
	}}}, points, &fakeSource{}, w)

	resp := dial(t, s, 1).roundTrip(t, 1, writeOneRegister(0, 1))
	expectException(t, resp, mb.FCWriteSingleRegister, 0x04)
	if s.Stats().WriteErrs != 1 {
		t.Fatalf("writeErrs = %d", s.Stats().WriteErrs)
	}
}

// ---------------------------------------------------------------------------
// 服务边界
// ---------------------------------------------------------------------------

func TestMaxConnectionsRejectsExtraClients(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	src := &fakeSource{samples: map[string]model.Sample{
		"plc/a": goodSample("plc", "a", model.NumValue(model.TypeUint16, 1)),
	}}
	s := newServer(t, model.Slave{ID: "s", MaxConnections: 1, Units: []model.SlaveUnit{{
		UnitID:   1,
		Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
	}}}, points, src, &fakeWriter{})

	c1 := dial(t, s, 1)
	// 先发一个请求确保连接已被 accept 并占用槽位。
	expectRegs(t, c1.roundTrip(t, 1, readHolding(0, 1)), mb.FCReadHoldingRegs, 1)

	c2, err := net.DialTimeout("tcp", s.Addr(), time.Second)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(time.Second))
	// 被拒的连接会被服务端直接关闭：读到 EOF 或 reset。
	if _, err := c2.Write(mb.BuildTCP(mb.Frame{Transaction: 1, Unit: 1, PDU: readHolding(0, 1)})); err == nil {
		if _, err := io.ReadFull(c2, make([]byte, 7)); err == nil {
			t.Fatal("second connection should have been rejected")
		}
	}
}

func TestCloseIsIdempotentAndStopsListener(t *testing.T) {
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	s, err := New(Options{
		Slave: model.Slave{ID: "s", Listen: "127.0.0.1:0", Units: []model.SlaveUnit{{
			UnitID:   1,
			Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
		}}},
		Points: points, Source: &fakeSource{}, Writer: &fakeWriter{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	addr := s.Addr()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after close")
	}
}

func TestNewRejectsInvalidMapping(t *testing.T) {
	points := map[string]model.Point{
		"plc/a": pt(model.TypeUint16, 1, false),
		"plc/b": pt(model.TypeUint16, 1, false),
	}
	// 重叠映射必须在构造期失败，而不是等到两个点位抢同一个地址。
	_, err := New(Options{
		Slave: model.Slave{ID: "s", Listen: "127.0.0.1:0", Units: []model.SlaveUnit{{
			UnitID: 1,
			Mappings: []model.SlaveMapping{
				{Point: "plc/a", Area: model.MappingHolding, Offset: 7},
				{Point: "plc/b", Area: model.MappingHolding, Offset: 7},
			},
		}}},
		Points: points, Source: &fakeSource{}, Writer: &fakeWriter{},
	})
	if err == nil {
		t.Fatal("overlapping mappings must be rejected")
	}
}
