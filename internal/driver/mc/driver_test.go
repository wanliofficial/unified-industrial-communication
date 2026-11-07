package mc

import (
	"context"
	"errors"
	"testing"
	"time"

	mccodec "uic/internal/codec/mc"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// memStream 是一个最小内存流：写入的请求原样回显为「成功响应」，
// 用来在不开 socket 的情况下驱动 framing 与错误分类逻辑。
type memStream struct {
	requests [][]byte
	frames   [][]byte // 每个完整响应帧
	pos      []int    // 每个帧当前的读取游标
	idx      int      // 当前帧索引
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

func testChannel() model.Channel {
	return model.Channel{
		ID: "ch", Protocol: "mc",
		Transport: model.Transport{Kind: "tcp", Address: "mem"},
		Timeout:   model.Duration(time.Second),
	}
}

func openMem(t *testing.T, stream transport.Stream) driver.Conn {
	t.Helper()
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

// respFor 把请求帧变成「成功读响应」。
func respFor(req []byte) []byte {
	code := req[15]
	dev, ok := mccodec.ByCode(code)
	if !ok {
		return mccodec.BuildResponse(0xC056, nil)
	}
	points := int(req[19]) | int(req[20])<<8
	var data []byte
	if dev.Bit {
		data = mccodec.PackBits(make([]bool, points))
	} else {
		data = make([]byte, points*2)
	}
	return mccodec.BuildResponse(0, data)
}

func TestReadWordsViaMem(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)

	// 预填响应：读 D0 2 个字（4 字节数据）。
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, make([]byte, 4)))
	stream.pos = append(stream.pos, 0)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID:       "d0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 2, Bit: -1},
		DataType: model.TypeUint16,
	}}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d", len(samples))
	}
	if len(stream.requests) != 1 {
		t.Fatalf("requests = %d", len(stream.requests))
	}
}

func TestEndCodeQuality(t *testing.T) {
	stream := &memStream{}
	// 用真实从站路径不方便（内存流不会跑 handleRead），这里直接构造
	// 一个返回 0xC052 的响应，验证驱动把它映射成 address_error。
	stream.frames = append(stream.frames, mccodec.BuildResponse(0xC052, nil))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID:       "d0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
	}}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatal("want error for end code")
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d", len(samples))
	}
	if samples[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s, want address_error", samples[0].Quality)
	}
}

func TestWriteWordBitNeedsBit(t *testing.T) {
	stream := &memStream{}
	// 写字软元件 bool 但没给 bit → 应地址错，且不发请求。
	// 不填任何响应：字软元件 bool 无 bit 应在发请求前就失败
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "d0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Bit: -1},
		DataType: model.TypeBool,
		Value:    model.BoolValue(true),
	}})
	if err == nil {
		t.Fatal("want error for word bool without bit")
	}
	if res[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s", res[0].Quality)
	}
	if len(stream.requests) != 0 {
		t.Fatalf("should not send request, got %d", len(stream.requests))
	}
}

func TestWriteWordsSuccess(t *testing.T) {
	stream := &memStream{}
	// 写 D0 一个字，响应空数据。
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, nil))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "d0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
		Value:    model.NumValue(model.TypeUint16, 0x1234),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("not ok: %v", res[0].Err)
	}
	if len(stream.requests) != 1 {
		t.Fatalf("requests = %d", len(stream.requests))
	}
	// 验证写入帧里的数据是 34 12（小端）。
	req := stream.requests[0]
	if req[len(req)-2] != 0x34 || req[len(req)-1] != 0x12 {
		t.Fatalf("write data = % x", req[len(req)-2:])
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	d := New()
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "mc",
			Transport: model.Transport{Kind: "rtu", Address: "/dev/ttyUSB0"}},
		Device: model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("want error for rtu transport")
	}
}

func TestMetaCapabilities(t *testing.T) {
	m := New().Meta()
	if m.ID != "mc" {
		t.Fatalf("meta id = %q", m.ID)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "tcp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if m.Capabilities.MultiRead != driver.CapSupported {
		t.Fatalf("multiRead = %s", m.Capabilities.MultiRead)
	}
}
