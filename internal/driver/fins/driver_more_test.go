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

// errStream 让读操作返回指定错误，用来验证超时与断链的分类。
type errStream struct {
	err    error
	writes int
}

func (e *errStream) Write(p []byte) error { e.writes++; return nil }
func (e *errStream) ReadFull(n int) ([]byte, error) {
	return nil, e.err
}
func (e *errStream) Close() error { return nil }

// timeoutErr 实现 net.Error 的 Timeout()，但不依赖真实 socket。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestProbe(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(1, []byte{0, 0})))
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	f := sentFINS(t, stream, 1)
	if f[12] != 0x82 || f[17] != 1 {
		t.Fatalf("probe frame = % x", f)
	}
}

func TestWriteFloat32LowWordFirst(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(1, nil)))

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "temp",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 2, Length: 2, Bit: -1},
		DataType: model.TypeFloat32, Value: model.NumValue(model.TypeFloat32, 48),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	f := sentFINS(t, stream, 1)
	// 01 02 | 82 | 0002 | 00 | 0002 | 0000 4240
	data := f[18:]
	want := []byte{0x00, 0x00, 0x42, 0x40}
	if len(data) != len(want) {
		t.Fatalf("data = % x", data)
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("data = % x, want % x", data, want)
		}
	}
	if f[17] != 2 {
		t.Fatalf("count = %d", f[17])
	}
}

func TestWriteStringPadsAndTruncates(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(response(1, nil)))

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "name",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 100, Length: 4, Bit: -1},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, "PUMP-01"),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	f := sentFINS(t, stream, 1)
	if len(f[18:]) != 8 {
		t.Fatalf("payload = % x", f[18:])
	}
	if string(f[18:24]) != "PUMP-0" {
		t.Fatalf("payload = %q", string(f[18:]))
	}
}

func TestWriteBoolNeedsBit(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "bad",
		Address:  model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: -1},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
	if res[0].Quality != model.QualityAddressError || res[0].OK {
		t.Fatalf("result = %+v", res[0])
	}
	if len(stream.requests) != 1 {
		t.Fatalf("unexpected requests: %d", len(stream.requests))
	}
}

func TestWriteUnknownArea(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "zz", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1),
	}})
	if err == nil || res[0].Quality != model.QualityAddressError {
		t.Fatalf("err = %v result = %+v", err, res[0])
	}
}

func TestReadSpanMixesAreasRejected(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		{ID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
		{ID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
	}}
	if _, err := c.Read(context.Background(), []driver.ReadSpan{span}); err == nil {
		t.Fatal("mixed-area span accepted")
	}
}

func TestReadUnknownArea(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "zz", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16,
	}}}
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{span})
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestReadCommErrorQuality(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(errorResponse(1, 0x0301)))
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16,
	}}}
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{span})
	if len(samples) != 1 || samples[0].Quality != model.QualityCommError {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestTimeoutQuality(t *testing.T) {
	stream := &errStream{err: timeoutErr{}}
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return stream, nil
	})
	// 握手第一步就会读到超时：Open 必须判成可重试的超时/连接错，而不是协议错。
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("open succeeded on timeout")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("timeout not retryable")
	}
}

func TestHandshakeTCPError(t *testing.T) {
	stream := &memStream{}
	stream.add(finscodec.BuildNodeError(0x21))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return stream, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("open succeeded with node error")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("node conflict should be retryable")
	}
}

func TestHandshakeRejectsNoNodes(t *testing.T) {
	stream := &memStream{}
	stream.add(finscodec.BuildNodeResponse(0, 0))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return stream, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("handshake accepted two zero nodes")
	}
}

func TestOpenRequiresAddress(t *testing.T) {
	d := New()
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "fins", Transport: model.Transport{Kind: "tcp"}},
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAppendsDefaultPort(t *testing.T) {
	var got string
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		got = addr
		return nil, errors.New("stop here")
	})
	_, _ = d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "fins", Transport: model.Transport{Kind: "tcp", Address: "10.0.0.5"}},
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if got != "10.0.0.5:9600" {
		t.Fatalf("dial address = %q", got)
	}
}

func TestWriteClosesOverFirstError(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	stream.add(tcp(errorResponse(1, 0x1103)))

	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1)},
		{PointID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 2)},
	})
	if err == nil {
		t.Fatal("write succeeded")
	}
	if len(res) != 2 || res[0].OK || res[1].OK {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %q", res[0].Quality)
	}
	if res[1].Err == nil {
		t.Fatal("second result lost the error")
	}
}

func TestTCPLevelErrorIsRetryable(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	// 命令 2、长度 8、错误码 0x21（节点已被占用）：没有内层 FINS 帧。
	pkt := make([]byte, 16)
	copy(pkt, "FINS")
	pkt[7] = 8
	pkt[11] = 2
	pkt[15] = 0x21
	stream.add(pkt)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16,
	}}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatal("transport error ignored")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("FINS/TCP transport error should be retryable")
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityCommError {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestOrderFor(t *testing.T) {
	if orderFor(2, "") != model.OrderCDAB || orderFor(8, "") != model.OrderDCBA {
		t.Fatalf("defaults = %q/%q", orderFor(2, ""), orderFor(8, ""))
	}
	if orderFor(2, model.OrderABCD) != model.OrderABCD {
		t.Fatal("explicit order ignored")
	}
}
