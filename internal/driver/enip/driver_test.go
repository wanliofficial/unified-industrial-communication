package enip

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	enipsim "uic/internal/sim/enip"
	"uic/internal/transport"
)

func testChannel(addr string) model.Channel {
	return model.Channel{
		ID: "ch", Protocol: "enip",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
}

func openSim(t *testing.T, srv *enipsim.Server) driver.Conn {
	t.Helper()
	d := New()
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newSim(t *testing.T) *enipsim.Server {
	t.Helper()
	srv, err := enipsim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func ref(id, tag string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id,
		Address: model.Address{
			Kind: model.AddressRegister, Area: tag, Offset: 0, Length: 0, Bit: -1,
		},
		DataType: dt,
	}
}

func readOne(t *testing.T, c driver.Conn, p driver.PointRef) driver.RawSample {
	t.Helper()
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err != nil {
		t.Fatalf("read %s: %v", p.ID, err)
	}
	if len(samples) != 1 {
		t.Fatalf("read %s: %d samples", p.ID, len(samples))
	}
	return samples[0]
}

func TestReadAtomicTypes(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)

	cases := []struct {
		id   string
		tag  string
		dt   model.DataType
		want float64
	}{
		{"dint", "DINT_TAG", model.TypeInt32, 1234},
		{"uint32", "DINT_TAG", model.TypeUint32, 1234},
		{"int16", "INT_TAG", model.TypeInt16, 5678},
		{"real", "REAL_TAG", model.TypeFloat32, 48},
	}
	for _, tc := range cases {
		s := readOne(t, c, ref(tc.id, tc.tag, tc.dt))
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %q err=%v", tc.id, s.Quality, s.Err)
		}
		got, ok := s.Value.AsFloat()
		if !ok || got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.id, s.Value, tc.want)
		}
	}

	b := readOne(t, c, ref("flag", "BOOL_TAG", model.TypeBool))
	if b.Quality != model.QualityGood || !b.Value.Bool {
		t.Fatalf("bool = %+v", b)
	}

	s := readOne(t, c, ref("name", "STRING_TAG", model.TypeString))
	if s.Quality != model.QualityGood || s.Value.Text != "PUMP-01" {
		t.Fatalf("string = %+v", s)
	}
}

func TestReadTagIsCaseInsensitive(t *testing.T) {
	c := openSim(t, newSim(t))
	s := readOne(t, c, ref("x", "dint_tag", model.TypeInt32))
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %q err=%v", s.Quality, s.Err)
	}
}

func TestReadBytes(t *testing.T) {
	c := openSim(t, newSim(t))
	p := ref("arr", "DINT_ARRAY", model.TypeBytes)
	p.Address.Length = 8
	s := readOne(t, c, p)
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %q err=%v", s.Quality, s.Err)
	}
	want := []byte{10, 0, 0, 0, 20, 0, 0, 0}
	if s.Value.Text != string(want) {
		t.Fatalf("bytes = % x", []byte(s.Value.Text))
	}
}

func TestReadArrayOffset(t *testing.T) {
	c := openSim(t, newSim(t))
	p := ref("third", "DINT_ARRAY", model.TypeInt32)
	p.Address.Offset = 2
	s := readOne(t, c, p)
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %q err=%v", s.Quality, s.Err)
	}
	if got, _ := s.Value.AsFloat(); got != 30 {
		t.Fatalf("value = %v", s.Value)
	}
}

func TestReadBitFromInteger(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	// 1234 = 0b100_1101_0010：bit1/bit4/bit6/bit7/bit10 为 1。
	for _, tc := range []struct {
		bit  int
		want bool
	}{{0, false}, {1, true}, {4, true}, {7, true}, {5, false}} {
		p := ref("bit", "DINT_TAG", model.TypeBool)
		p.Address.Bit = tc.bit
		s := readOne(t, c, p)
		if s.Quality != model.QualityGood || s.Value.Bool != tc.want {
			t.Fatalf("bit %d = %+v, want %v", tc.bit, s, tc.want)
		}
	}
}

func TestWriteAtomicAndReadBack(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)

	write := func(id string, addr model.Address, dt model.DataType, v model.Value) {
		t.Helper()
		res, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: id, Address: addr, DataType: dt, Value: v,
		}})
		if err != nil || !res[0].OK {
			t.Fatalf("write %s: %v %+v", id, err, res)
		}
	}
	addr := model.Address{Kind: model.AddressRegister, Area: "DINT_TAG", Bit: -1}
	write("dint", addr, model.TypeInt32, model.NumValue(model.TypeInt32, 4321))
	if _, _, data, _ := srv.Tag("DINT_TAG"); len(data) != 4 || binary.LittleEndian.Uint32(data) != 4321 {
		t.Fatalf("tag data = % x", data)
	}
	s := readOne(t, c, ref("dint", "DINT_TAG", model.TypeInt32))
	if got, _ := s.Value.AsFloat(); got != 4321 {
		t.Fatalf("read back = %v", s.Value)
	}

	write("real", model.Address{Kind: model.AddressRegister, Area: "REAL_TAG", Bit: -1},
		model.TypeFloat32, model.NumValue(model.TypeFloat32, 12.5))
	s = readOne(t, c, ref("real", "REAL_TAG", model.TypeFloat32))
	if got, _ := s.Value.AsFloat(); got != 12.5 {
		t.Fatalf("real = %v", s.Value)
	}

	write("flag", model.Address{Kind: model.AddressRegister, Area: "BOOL_TAG", Bit: -1},
		model.TypeBool, model.BoolValue(false))
	s = readOne(t, c, ref("flag", "BOOL_TAG", model.TypeBool))
	if s.Value.Bool {
		t.Fatal("bool write did not stick")
	}

	write("name", model.Address{Kind: model.AddressRegister, Area: "STRING_TAG", Bit: -1, Length: 16},
		model.TypeString, model.TextValue(model.TypeString, "MOTOR-7"))
	s = readOne(t, c, ref("name", "STRING_TAG", model.TypeString))
	if s.Value.Text != "MOTOR-7" {
		t.Fatalf("string = %q", s.Value.Text)
	}
}

func TestWriteBitPreservesNeighbours(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	// 1234 的 bit1 置 0：1234 &^ 2 = 1232。其它位必须原样保留。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "bit1",
		Address:  model.Address{Kind: model.AddressRegister, Area: "DINT_TAG", Bit: 1},
		DataType: model.TypeBool, Value: model.BoolValue(false),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write bit: %v %+v", err, res)
	}
	s := readOne(t, c, ref("dint", "DINT_TAG", model.TypeInt32))
	if got, _ := s.Value.AsFloat(); got != 1232 {
		t.Fatalf("after clear = %v, want 1232", s.Value)
	}

	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "bit3",
		Address:  model.Address{Kind: model.AddressRegister, Area: "DINT_TAG", Bit: 3},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write bit: %v %+v", err, res)
	}
	s = readOne(t, c, ref("dint", "DINT_TAG", model.TypeInt32))
	if got, _ := s.Value.AsFloat(); got != 1240 {
		t.Fatalf("after set = %v, want 1240", s.Value)
	}
}

func TestWriteArrayOffsetPreservesPrefix(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	p := model.Address{Kind: model.AddressRegister, Area: "DINT_ARRAY", Offset: 2, Bit: -1}
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "third", Address: p, DataType: model.TypeInt32,
		Value: model.NumValue(model.TypeInt32, 99),
	}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for i, want := range []int32{10, 20, 99, 40} {
		r := ref("v", "DINT_ARRAY", model.TypeInt32)
		r.Address.Offset = i
		s := readOne(t, c, r)
		if got, _ := s.Value.AsFloat(); got != float64(want) {
			t.Fatalf("element %d = %v, want %d", i, s.Value, want)
		}
	}
}

func TestWriteBytes(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	p := model.Address{Kind: model.AddressRegister, Area: "DINT_ARRAY", Offset: 0, Bit: -1, Length: 4}
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "raw", Address: p, DataType: model.TypeBytes,
		Value: model.BytesValue([]byte{1, 2, 3, 4}),
	}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, data, _ := srv.Tag("DINT_ARRAY"); len(data) < 4 || data[0] != 1 || data[1] != 2 || data[2] != 3 || data[3] != 4 {
		t.Fatalf("tag data = % x", data)
	}
}

func TestReadManyUsesMultipleService(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		ref("dint", "DINT_TAG", model.TypeInt32),
		ref("int", "INT_TAG", model.TypeInt16),
		ref("real", "REAL_TAG", model.TypeFloat32),
		ref("flag", "BOOL_TAG", model.TypeBool),
		ref("name", "STRING_TAG", model.TypeString),
	}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 5 {
		t.Fatalf("samples = %d", len(samples))
	}
	for _, s := range samples {
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %q err=%v", s.PointID, s.Quality, s.Err)
		}
	}
}

func TestMultipleServiceUnsupportedFallsBack(t *testing.T) {
	srv := newSim(t)
	srv.SetMultipleSupported(false)
	c := openSim(t, srv)
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		ref("dint", "DINT_TAG", model.TypeInt32),
		ref("real", "REAL_TAG", model.TypeFloat32),
	}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	for _, s := range samples {
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %q err=%v", s.PointID, s.Quality, s.Err)
		}
	}
	// 第二轮应直接走逐条请求（c.msp 已关闭），结果仍然正确。
	if samples, err = c.Read(context.Background(), []driver.ReadSpan{span}); err != nil {
		t.Fatalf("second read: %v", err)
	}
	for _, s := range samples {
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %q err=%v", s.PointID, s.Quality, s.Err)
		}
	}
}

func TestUnknownTagIsAddressError(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("ghost", "NO_SUCH_TAG", model.TypeInt32),
	}}})
	if err == nil {
		t.Fatal("unknown tag succeeded")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestTypeMismatchIsAddressError(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	// REAL_TAG 是 REAL（4 字节浮点），按 int32 读必须报配置错，而不是
	// 把位模式当整数返回。
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("real", "REAL_TAG", model.TypeInt32),
	}}})
	if err == nil {
		t.Fatal("type mismatch accepted")
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError || samples[0].Err == nil {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestNumericLengthOverOneRejected(t *testing.T) {
	c := openSim(t, newSim(t))
	p := ref("bad", "DINT_TAG", model.TypeInt32)
	p.Address.Length = 2
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil {
		t.Fatal("numeric length 2 accepted")
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestBytesRequiresLength(t *testing.T) {
	c := openSim(t, newSim(t))
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("raw", "DINT_ARRAY", model.TypeBytes),
	}}})
	if err == nil {
		t.Fatal("bytes without length accepted")
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestProbe(t *testing.T) {
	c := openSim(t, newSim(t))
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	d := New()
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "enip", Transport: model.Transport{Kind: "udp"}},
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenRequiresAddress(t *testing.T) {
	d := New()
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "enip", Transport: model.Transport{Kind: "tcp"}},
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
		Channel: model.Channel{ID: "ch", Protocol: "enip",
			Transport: model.Transport{Kind: "tcp", Address: "10.0.0.5"}},
		Device: model.Device{ID: "plc", ChannelID: "ch"},
	})
	if got != "10.0.0.5:44818" {
		t.Fatalf("dial address = %q", got)
	}
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "enip" || !m.Accepts(model.AddressRegister) || m.Accepts(model.AddressSymbol) {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.MultiRead != driver.CapSupported || m.Capabilities.BitAccess != driver.CapSupported {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
}

func TestWriteBitNeedsIntegerTag(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "bad",
		Address:  model.Address{Kind: model.AddressRegister, Area: "STRING_TAG", Bit: 0},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err == nil {
		t.Fatal("bit write on STRING accepted")
	}
	if len(res) != 1 || res[0].OK || res[0].Quality != model.QualityAddressError {
		t.Fatalf("result = %+v", res)
	}
}

func TestBitOutsideElement(t *testing.T) {
	c := openSim(t, newSim(t))
	// INT 只有 16 位：bit16 必须报错，不能悄悄读到下一个元素。
	p := ref("bad", "INT_TAG", model.TypeBool)
	p.Address.Bit = 16
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil {
		t.Fatal("bit 16 accepted")
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}
}

// TestReadPartialFailureKeepsGoodSamples 钉住部分失败契约（docs §3.7.4 /
// §3.9.5）：一个标签名配错（ErrAddress）时，只要还有别的点读成功，Read 就
// 不向上返回错误。否则 collector 会丢弃会话、整台 PLC 每周期重连。
func TestReadPartialFailureKeepsGoodSamples(t *testing.T) {
	c := openSim(t, newSim(t))
	good := ref("dint", "DINT_TAG", model.TypeInt32)
	bad := ref("bad", "NO_SUCH_TAG", model.TypeInt32)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{good, bad},
	}})
	if err != nil {
		t.Fatalf("有成功点时 Read 不该返回错误，得到 %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].Err != nil || samples[0].Quality != model.QualityGood || samples[0].Value.Num != 1234 {
		t.Fatalf("好点不该被连累: %+v", samples[0])
	}
	if samples[1].Err == nil {
		t.Fatalf("坏点应带错误: %+v", samples[1])
	}
}

// 全部点位失败时必须返回错误，让熔断器看到「设备全坏」。
func TestReadAllFailedStillReturnsError(t *testing.T) {
	c := openSim(t, newSim(t))
	bad1 := ref("b1", "NO_SUCH_TAG_A", model.TypeInt32)
	bad2 := ref("b2", "NO_SUCH_TAG_B", model.TypeInt32)

	if _, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{bad1, bad2},
	}}); err == nil {
		t.Fatal("全部点位失败时必须返回错误")
	}
}

// TestWriteFailureKeepsEarlierSuccessesIntact：批量写里后面的条目失败，不能把
// **已经成功**的条目也标上错误（docs §3.4「逐项返回结果」）。
//
// 早先这里是「把 firstErr 回填进所有 Err==nil 的结果」，后果是调用方会去
// 重发一个已经写下去的点位——对计数类点位就是重复累加。
func TestWriteFailureKeepsEarlierSuccessesIntact(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)

	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{
			PointID:  "ok",
			Address:  model.Address{Kind: model.AddressRegister, Area: "DINT_TAG", Bit: -1},
			DataType: model.TypeInt32,
			Value:    model.NumValue(model.TypeInt32, 7),
		},
		{
			PointID:  "bad",
			Address:  model.Address{Kind: model.AddressRegister, Area: "NO_SUCH_TAG", Bit: -1},
			DataType: model.TypeInt32,
			Value:    model.NumValue(model.TypeInt32, 1),
		},
	})
	if err == nil {
		t.Fatal("a batch with a failing item must return an error")
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if !res[0].OK || res[0].Err != nil || res[0].Quality != model.QualityGood {
		t.Fatalf("first item succeeded but was reported as %+v", res[0])
	}
	if res[1].OK || res[1].Err == nil {
		t.Fatalf("second item failed but was reported as %+v", res[1])
	}
	if res[1].Quality == "" {
		t.Fatal("a failed write must carry a quality, not the empty string")
	}
	// 成功的条目确实落到了从站上：逐项语义不能只是「报得好看」。
	if _, _, data, ok := srv.Tag("DINT_TAG"); !ok || binary.LittleEndian.Uint32(data) != 7 {
		t.Fatalf("tag data = % x", data)
	}
}
