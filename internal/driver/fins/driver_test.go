package fins

import (
	"context"
	"errors"
	"testing"
	"time"

	finscodec "uic/internal/codec/fins"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// memStream 把预置的响应帧按顺序喂给驱动，并记录每次写入的请求。
type memStream struct {
	requests [][]byte
	frames   [][]byte
	pos      []int
	idx      int
	closed   bool
}

func (m *memStream) Write(p []byte) error {
	m.requests = append(m.requests, append([]byte(nil), p...))
	return nil
}

func (m *memStream) ReadFull(n int) ([]byte, error) {
	if m.idx >= len(m.frames) {
		return nil, errors.New("eof")
	}
	frame := m.frames[m.idx]
	cur := m.pos[m.idx]
	if cur+n > len(frame) {
		return nil, errors.New("short")
	}
	out := frame[cur : cur+n]
	m.pos[m.idx] = cur + n
	if m.pos[m.idx] >= len(frame) {
		m.idx++
	}
	return out, nil
}

func (m *memStream) Close() error { m.closed = true; return nil }

func (m *memStream) add(b []byte) {
	m.frames = append(m.frames, b)
	m.pos = append(m.pos, 0)
}

// tcp 把内层 FINS 帧封装成 FINS/TCP 命令 2 响应。
func tcp(frame []byte) []byte {
	pkt, err := finscodec.BuildTCPFrame(frame)
	if err != nil {
		panic(err)
	}
	return pkt
}

func testChannel() model.Channel {
	return model.Channel{
		ID: "ch", Protocol: "fins",
		Transport: model.Transport{Kind: "tcp", Address: "mem"},
		Timeout:   model.Duration(time.Second),
	}
}

func openMem(t *testing.T, stream *memStream) driver.Conn {
	t.Helper()
	stream.add(finscodec.BuildNodeResponse(2, 1))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return stream, nil
	})
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// response 构造一个带指定 SID 的成功响应。
func response(sid byte, data []byte) []byte {
	out := make([]byte, 14+len(data))
	out[0] = 0xC0
	out[2] = 2
	out[7] = 1
	out[9] = sid
	out[10], out[11] = 1, 1
	copy(out[14:], data)
	return out
}

func errorResponse(sid byte, code uint16) []byte {
	out := response(sid, nil)
	out[12], out[13] = byte(code>>8), byte(code)
	return out
}

// sentFINS 取出第 n 个写入请求里的 FINS 帧（跳过 16 字节 TCP 头）。
func sentFINS(t *testing.T, m *memStream, n int) []byte {
	t.Helper()
	if n >= len(m.requests) {
		t.Fatalf("request %d missing (have %d)", n, len(m.requests))
	}
	pkt := m.requests[n]
	if len(pkt) < 28 || string(pkt[:4]) != "FINS" {
		t.Fatalf("request %d is not a FINS/TCP frame: % x", n, pkt)
	}
	return pkt[16:]
}

func TestOpenHandshake(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	defer c.Close()

	if len(stream.requests) != 1 {
		t.Fatalf("requests = %d", len(stream.requests))
	}
	req := stream.requests[0]
	if string(req[:4]) != "FINS" || len(req) != 20 {
		t.Fatalf("handshake request = % x", req)
	}
}

func TestReadWords(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(1, []byte{0x12, 0x34, 0x56, 0x78})))

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		{ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
		{ID: "dm1", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Length: 1, Bit: -1}, DataType: model.TypeUint16},
	}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d", len(samples))
	}
	if v, ok := samples[0].Value.AsFloat(); !ok || uint16(v) != 0x1234 {
		t.Fatalf("dm0 = %v", samples[0].Value)
	}
	if v, ok := samples[1].Value.AsFloat(); !ok || uint16(v) != 0x5678 {
		t.Fatalf("dm1 = %v", samples[1].Value)
	}

	f := sentFINS(t, stream, 1)
	if f[12] != 0x82 || f[13] != 0 || f[14] != 0 || f[15] != 0 || f[16] != 0 || f[17] != 2 {
		t.Fatalf("read frame = % x", f)
	}
}

func TestReadFloat32LowWordFirst(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	// 48.0 = 0x42400000；Omron 低字在前 → 线上 00 00 42 40。
	stream.add(tcp(response(1, []byte{0x00, 0x00, 0x42, 0x40})))

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "dm2", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 2, Length: 2, Bit: -1},
		DataType: model.TypeFloat32,
	}}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if v, ok := samples[0].Value.AsFloat(); !ok || v != 48 {
		t.Fatalf("dm2 = %v", samples[0].Value)
	}
}

func TestReadBits(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	data := make([]byte, 16)
	data[0] = 1
	data[3] = 1
	stream.add(tcp(response(1, data)))

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		{ID: "b0", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 0}, DataType: model.TypeBool},
		{ID: "b3", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 3}, DataType: model.TypeBool},
		{ID: "b4", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 4}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !samples[0].Value.Bool || !samples[1].Value.Bool || samples[2].Value.Bool {
		t.Fatalf("bits = %v %v %v", samples[0].Value.Bool, samples[1].Value.Bool, samples[2].Value.Bool)
	}

	f := sentFINS(t, stream, 1)
	if f[12] != 0x30 || f[13] != 0 || f[14] != 0 || f[15] != 0 || f[16] != 0 || f[17] != 16 {
		t.Fatalf("bit read frame = % x", f)
	}
}

func TestWriteBit(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(1, nil)))

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "fault",
		Address:  model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 10, Bit: 3},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("not ok: %+v", res[0])
	}
	f := sentFINS(t, stream, 1)
	// 01 02 | 30 | 000A | 03 | 0001 | 01
	want := []byte{0x01, 0x02, 0x30, 0x00, 0x0A, 0x03, 0x00, 0x01, 0x01}
	got := f[10:]
	if len(got) != len(want) {
		t.Fatalf("body = % x", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("body = % x, want % x", got, want)
		}
	}
}

func TestWriteBoolOnWordOnlyAreaDoesRMW(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(1, []byte{0x00, 0x00})))
	stream.add(tcp(response(2, nil)))

	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "em-bit",
		Address:  model.Address{Kind: model.AddressRegister, Area: "em", Offset: 5, Bit: 3},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	read := sentFINS(t, stream, 1)
	if read[12] != 0x98 || read[13] != 0 || read[14] != 5 || read[17] != 1 {
		t.Fatalf("rmw read frame = % x", read)
	}
	write := sentFINS(t, stream, 2)
	if write[12] != 0x98 || write[17] != 1 || write[18] != 0x00 || write[19] != 0x08 {
		t.Fatalf("rmw write frame = % x", write)
	}
}

func TestEndCodeAddressQuality(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(errorResponse(1, 0x1103)))

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
	}}}
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{span})
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("sample = %+v", samples[0])
	}
	if samples[0].Err == nil {
		t.Fatal("expected error")
	}
}

func TestSIDMismatch(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(99, []byte{0, 0})))

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
	}}}
	_, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatal("SID mismatch accepted")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	d := New()
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "fins", Transport: model.Transport{Kind: "udp", Address: "x"}},
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "fins" || !m.Accepts(model.AddressRegister) {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.BitAccess != driver.CapSupported {
		t.Fatalf("bit access = %q", m.Capabilities.BitAccess)
	}
}
