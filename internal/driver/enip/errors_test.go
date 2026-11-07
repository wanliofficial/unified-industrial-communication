package enip

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	enipcodec "uic/internal/codec/enip"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// scriptStream 按顺序吐出预置的完整封装报文，并记录写出的请求。
//
// 用真实从站测「正常路径」，用这个流测「错误路径」：现场遇到的是
// 会话失效、命令错位、应答截断，而不是永远正确回包。
type scriptStream struct {
	frames   [][]byte
	pos      []int
	idx      int
	requests [][]byte
	// failAfter > 0 时，第 failAfter 次写入返回 errWrite。
	failAfter int
	writes    int
	reads     int
}

var errScript = errors.New("scripted stream failure")

func (s *scriptStream) Write(p []byte) error {
	s.writes++
	s.requests = append(s.requests, append([]byte(nil), p...))
	if s.failAfter > 0 && s.writes >= s.failAfter {
		return errScript
	}
	return nil
}

func (s *scriptStream) ReadFull(n int) ([]byte, error) {
	s.reads++
	if s.idx >= len(s.frames) {
		return nil, errScript
	}
	frame := s.frames[s.idx]
	cur := s.pos[s.idx]
	if cur+n > len(frame) {
		return nil, errScript
	}
	out := frame[cur : cur+n]
	s.pos[s.idx] = cur + n
	if s.pos[s.idx] >= len(frame) {
		s.idx++
	}
	return out, nil
}

func (s *scriptStream) Close() error { return nil }

func (s *scriptStream) add(frame []byte) *scriptStream {
	s.frames = append(s.frames, frame)
	s.pos = append(s.pos, 0)
	return s
}

func packet(cmd uint16, session, status uint32, ctx [8]byte, payload []byte) []byte {
	b, err := enipcodec.BuildPacket(cmd, session, status, ctx, payload)
	if err != nil {
		panic(err)
	}
	return b
}

func registerReply(session uint32, status uint32, version uint16) []byte {
	return packet(enipcodec.CmdRegisterSession, session, status, [8]byte{},
		enipcodec.BuildRegisterSession(version))
}

// rrReply 生成一条正常的 SendRRData 应答（CIP 状态由调用方给）。
func rrReply(session uint32, service, status byte, extra, data []byte) []byte {
	reply, err := enipcodec.BuildReply(service, status, extra, data)
	if err != nil {
		panic(err)
	}
	payload, err := enipcodec.BuildSendRRData(reply)
	if err != nil {
		panic(err)
	}
	return packet(enipcodec.CmdSendRRData, session, 0, [8]byte{}, payload)
}

// rrRaw 生成一条 CPF 负载已经准备好的 SendRRData 应答。
func rrRaw(session uint32, payload []byte) []byte {
	return packet(enipcodec.CmdSendRRData, session, 0, [8]byte{}, payload)
}

func openScript(t *testing.T, s *scriptStream) (driver.Conn, *scriptStream) {
	t.Helper()
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, s
}

func newScript() *scriptStream { return &scriptStream{} }

// ---------------------------------------------------------------------------
// 会话建立
// ---------------------------------------------------------------------------

func TestRegisterEncapStatusIsRetryable(t *testing.T) {
	s := newScript().add(registerReply(0, 0x64, 1))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("register with status 0x64 succeeded")
	}
	if driver.KindOf(err) != driver.ErrDisconnected || !driver.IsRetryable(err) {
		t.Fatalf("kind = %v retryable = %v (%v)", driver.KindOf(err), driver.IsRetryable(err), err)
	}
}

func TestRegisterZeroSessionRejected(t *testing.T) {
	s := newScript().add(registerReply(0, 0, 1))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterUnsupportedVersionRejected(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 2))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterCommandMismatch(t *testing.T) {
	// 设备回了别的命令：继续解析会把 CPF 当 RegisterSession 负载。
	s := newScript().add(packet(enipcodec.CmdSendRRData, 0, 0, [8]byte{}, nil))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterContextMismatch(t *testing.T) {
	// 非零但不匹配的发送方上下文：说明应答属于另一条请求（或残留报文）。
	s := newScript().add(packet(enipcodec.CmdRegisterSession, 0x11, 0, [8]byte{9, 9, 9, 9, 9, 9, 9, 9},
		enipcodec.BuildRegisterSession(1)))
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterTruncatedHeader(t *testing.T) {
	s := newScript().add([]byte{0x65, 0x00})
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return s, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("mem"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("truncated header accepted")
	}
}

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

func TestProbeToleratesUnsupported(t *testing.T) {
	for _, status := range []byte{0x00, 0x08, 0x14} {
		s := newScript().add(registerReply(0x11, 0, 1)).add(rrReply(0x11, enipcodec.ServiceGetAttrAll, status, nil, nil))
		c, _ := openScript(t, s)
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("status %02X: %v", status, err)
		}
	}
}

func TestProbeReportsRealError(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceGetAttrAll, 0x0F, nil, nil))
	c, _ := openScript(t, s)
	err := c.Probe(context.Background())
	if err == nil || driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("err = %v (%v)", err, driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 应答解析错误
// ---------------------------------------------------------------------------

func TestSendRejectsBrokenCPF(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrRaw(0x11, []byte{0, 0, 0, 0, 0, 0, 1, 0})) // 一个数据条目都没有
	c, _ := openScript(t, s)
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestSendRejectsShortReply(t *testing.T) {
	cip := []byte{0xCC}
	payload, _ := enipcodec.BuildSendRRData(cip)
	s := newScript().add(registerReply(0x11, 0, 1)).add(rrRaw(0x11, payload))
	c, _ := openScript(t, s)
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestSendPropagatesEncapStatus(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(packet(enipcodec.CmdSendRRData, 0x11, 0x64, [8]byte{}, nil))
	c, _ := openScript(t, s)
	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || !driver.IsRetryable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadContextAlreadyDone(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteNetworkFailure(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	s.failAfter = s.writes + 1
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "DINT_TAG", Bit: -1},
		DataType: model.TypeInt32, Value: model.NumValue(model.TypeInt32, 1),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("err = %v", err)
	}
	if len(res) != 1 || res[0].OK || res[0].Quality != model.QualityCommError {
		t.Fatalf("result = %+v", res)
	}
}

// ---------------------------------------------------------------------------
// 读解码错误
// ---------------------------------------------------------------------------

func TestReadBoolTagShortReply(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeBOOL, 0, 1, 0}))
	c, _ := openScript(t, s)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("flag", "BOOL_TAG", model.TypeBool),
	}}})
	if err == nil || len(samples) != 1 || samples[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestReadStringBrokenPayload(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeSTRING, 0, 1, 0}))
	c, _ := openScript(t, s)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("name", "STRING_TAG", model.TypeString),
	}}})
	if err == nil || len(samples) != 1 || samples[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestReadNumericShortReply(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeDINT, 0, 1, 0, 1, 2}))
	c, _ := openScript(t, s)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || len(samples) != 1 || samples[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestReadBytesOnStructTag(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeStruct, 0, 1, 0, 1, 2, 3, 4}))
	c, _ := openScript(t, s)
	p := ref("raw", "STRUCT_TAG", model.TypeBytes)
	p.Address.Length = 4
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil || len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestReadZeroElements(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeDINT, 0, 0, 0}))
	c, _ := openScript(t, s)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || len(samples) != 1 || samples[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestBuildReadRejectsBadAddress(t *testing.T) {
	cases := []driver.PointRef{
		{ID: "no-tag", Address: model.Address{Kind: model.AddressRegister, Bit: -1}, DataType: model.TypeInt32},
		{ID: "neg", Address: model.Address{Kind: model.AddressRegister, Area: "T", Offset: -1, Bit: -1}, DataType: model.TypeInt32},
		{ID: "str-offset", Address: model.Address{Kind: model.AddressRegister, Area: "T", Offset: 1, Bit: -1}, DataType: model.TypeString},
		{ID: "bytes", Address: model.Address{Kind: model.AddressRegister, Area: "T", Bit: -1}, DataType: model.TypeBytes},
		{ID: "bcd", Address: model.Address{Kind: model.AddressRegister, Area: "T", Bit: -1}, DataType: model.TypeBCD16},
	}
	for _, p := range cases {
		if _, _, err := buildRead(p); err == nil {
			t.Fatalf("%s accepted", p.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// 写路径错误
// ---------------------------------------------------------------------------

func TestWriteRejectsBadAddress(t *testing.T) {
	c, _ := openScript(t, newScript().add(registerReply(0x11, 0, 1)))
	cases := []driver.WriteRequest{
		{PointID: "no-tag", Address: model.Address{Kind: model.AddressRegister, Bit: -1}, DataType: model.TypeInt32},
		{PointID: "neg", Address: model.Address{Kind: model.AddressRegister, Area: "T", Offset: -1, Bit: -1}, DataType: model.TypeInt32},
		{PointID: "bcd", Address: model.Address{Kind: model.AddressRegister, Area: "T", Bit: -1}, DataType: model.TypeBCD16},
		{PointID: "multi", Address: model.Address{Kind: model.AddressRegister, Area: "T", Length: 2, Bit: -1}, DataType: model.TypeInt32},
		{PointID: "str-offset", Address: model.Address{Kind: model.AddressRegister, Area: "T", Offset: 1, Bit: -1}, DataType: model.TypeString},
		{PointID: "bytes", Address: model.Address{Kind: model.AddressRegister, Area: "T", Bit: -1}, DataType: model.TypeBytes},
	}
	for _, req := range cases {
		req.Value = model.NumValue(model.TypeInt32, 1)
		res, err := c.Write(context.Background(), []driver.WriteRequest{req})
		if err == nil {
			t.Fatalf("%s accepted", req.PointID)
		}
		if len(res) != 1 || res[0].OK {
			t.Fatalf("%s result = %+v", req.PointID, res)
		}
	}
}

func TestWriteStringDefaultCapacity(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceWriteTag, 0, nil, []byte{enipcodec.TypeSTRING, 0, 1, 0}))
	c, _ := openScript(t, s)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "name",
		Address:  model.Address{Kind: model.AddressRegister, Area: "STRING_TAG", Bit: -1},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, "X"),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// 请求里的字符串应为 4+82 字节（默认容量）。
	req := s.requests[len(s.requests)-1]
	if len(req) < 4+82 {
		t.Fatalf("request too short: %d", len(req))
	}
}

func TestWriteBytesTooLarge(t *testing.T) {
	big := make([]byte, 4+maxReadBytes+1)
	big[0] = enipcodec.TypeSINT
	binary.LittleEndian.PutUint16(big[2:], uint16(maxReadBytes+1))
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeSINT, 0, 1, 0, 0})).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, big))
	c, _ := openScript(t, s)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "raw",
		Address:  model.Address{Kind: model.AddressRegister, Area: "SINT_ARRAY", Bit: -1, Length: maxReadBytes + 1},
		DataType: model.TypeBytes, Value: model.BytesValue(make([]byte, maxReadBytes+1)),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteBytesOnStructTag(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeStruct, 0, 1, 0, 1, 2, 3, 4}))
	c, _ := openScript(t, s)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "raw",
		Address:  model.Address{Kind: model.AddressRegister, Area: "STRUCT_TAG", Bit: -1, Length: 4},
		DataType: model.TypeBytes, Value: model.BytesValue([]byte{1, 2, 3, 4}),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteOffsetTypeMismatch(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeINT, 0, 2, 0, 1, 0, 2, 0}))
	c, _ := openScript(t, s)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "INT_TAG", Offset: 1, Bit: -1},
		DataType: model.TypeInt32, Value: model.NumValue(model.TypeInt32, 7),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteBitOutsideElement(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeINT, 0, 1, 0, 0, 0}))
	c, _ := openScript(t, s)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "bit",
		Address:  model.Address{Kind: model.AddressRegister, Area: "INT_TAG", Bit: 15 + 1},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteNumericRejectsOverlongReply(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceWriteTag, 0x13, nil, nil))
	c, _ := openScript(t, s)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "DINT_TAG", Bit: -1},
		DataType: model.TypeInt32, Value: model.NumValue(model.TypeInt32, 1),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v (%v)", err, driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 多服务应答
// ---------------------------------------------------------------------------

func TestSendMultipleWrongReplyCount(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	// 两条请求，但设备只回一条：必须报协议错，不能把第一条的结果当成两条。
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1)
	r0, _ := enipcodec.BuildReply(enipcodec.ServiceReadTag, 0, nil, []byte{enipcodec.TypeDINT, 0, 1, 0, 1, 0, 0, 0})
	body = append(body, 0, 0)
	body = append(body, r0...)
	s.add(rrReply(0x11, enipcodec.ServiceMultiple, 0, nil, body))
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("a", "A", model.TypeInt32), ref("b", "B", model.TypeInt32),
	}}})
	if err == nil {
		t.Fatalf("mismatched reply count accepted: %+v", samples)
	}
}

func TestSendMultipleBrokenReply(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceMultiple, 0, nil, []byte{1, 0, 0xFF, 0xFF}))
	c, _ := openScript(t, s)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("a", "A", model.TypeInt32), ref("b", "B", model.TypeInt32),
	}}})
	if err == nil || len(samples) != 2 {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestSendMultipleEmbeddedCapError(t *testing.T) {
	// 500 字节的单条嵌入请求超过 32 条上限前会先拆批；这里用 33 条小请求
	// 直接打 BuildMultiple 的上限分支。
	reqs := make([][]byte, 33)
	for i := range reqs {
		r, _ := enipcodec.BuildReadTag("T", 1)
		reqs[i] = r
	}
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	points := make([]driver.PointRef, 33)
	for i := range points {
		points[i] = ref("p", "T", model.TypeInt32)
	}
	// 330 字节 < 450，会一次性进入 BuildMultiple 并触发条数上限。
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: points}})
	if err == nil {
		t.Fatalf("33 embedded requests accepted (%d samples)", len(samples))
	}
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func TestCompatibleTable(t *testing.T) {
	cases := []struct {
		a, b byte
		want bool
	}{
		{enipcodec.TypeDINT, enipcodec.TypeUDINT, true},
		{enipcodec.TypeDINT, enipcodec.TypeDWORD, true},
		{enipcodec.TypeINT, enipcodec.TypeUINT, true},
		{enipcodec.TypeREAL, enipcodec.TypeLREAL, false},
		{enipcodec.TypeDINT, enipcodec.TypeREAL, false},
		{enipcodec.TypeLINT, enipcodec.TypeDINT, false},
		{enipcodec.TypeBOOL, enipcodec.TypeBOOL, true},
		{enipcodec.TypeBOOL, enipcodec.TypeSINT, false},
		{enipcodec.TypeStruct, enipcodec.TypeStruct, false},
	}
	for _, c := range cases {
		if got := compatible(c.a, c.b); got != c.want {
			t.Fatalf("compatible(%02X,%02X) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestIsIntegerAndBoolByte(t *testing.T) {
	for _, typ := range []byte{enipcodec.TypeSINT, enipcodec.TypeINT, enipcodec.TypeDINT, enipcodec.TypeLINT,
		enipcodec.TypeUSINT, enipcodec.TypeUINT, enipcodec.TypeUDINT, enipcodec.TypeULINT} {
		if !isInteger(typ) {
			t.Fatalf("type %02X not integer", typ)
		}
	}
	for _, typ := range []byte{enipcodec.TypeREAL, enipcodec.TypeSTRING, enipcodec.TypeBOOL} {
		if isInteger(typ) {
			t.Fatalf("type %02X reported integer", typ)
		}
	}
	if string(boolByte(true)) != "\x01" || string(boolByte(false)) != "\x00" {
		t.Fatal("boolByte mismatch")
	}
}

func TestQualityMapping(t *testing.T) {
	cases := []struct {
		err  error
		want model.Quality
	}{
		{driver.E(driver.ErrTimeout, "x", "d", "", true, nil), model.QualityTimeout},
		{driver.E(driver.ErrAddress, "x", "d", "", false, nil), model.QualityAddressError},
		{driver.E(driver.ErrDisconnected, "x", "d", "", true, nil), model.QualityCommError},
		{driver.E(driver.ErrProtocol, "x", "d", "", false, nil), model.QualityBad},
		{nil, model.QualityBad},
	}
	for _, c := range cases {
		if got := qualityOf(c.err); got != c.want {
			t.Fatalf("qualityOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestClassifyNetErr(t *testing.T) {
	if got := classifyNetErr("x", context.DeadlineExceeded); driver.KindOf(got) != driver.ErrTimeout {
		t.Fatalf("deadline kind = %v", driver.KindOf(got))
	}
	if got := classifyNetErr("x", errScript); driver.KindOf(got) != driver.ErrDisconnected {
		t.Fatalf("generic kind = %v", driver.KindOf(got))
	}
	if classifyNetErr("x", nil) != nil {
		t.Fatal("nil error became non-nil")
	}
}

func TestStatusErrorIsRetryableFalse(t *testing.T) {
	// CIP 路径错误不应被调度器当成可重试；否则会对着配置错误反复重连。
	err := &enipcodec.StatusError{Service: enipcodec.ServiceReadTag, Code: 0x05}
	if driver.IsRetryable(err) {
		t.Fatal("status error marked retryable")
	}
}

func TestNeedMoreError(t *testing.T) {
	e := &needMore{count: 3}
	if e.Error() == "" {
		t.Fatal("empty message")
	}
}

func TestReadBytesExceedingLimit(t *testing.T) {
	// 学到实际宽度后才知道 401 字节超过上限；必须在驱动侧拒绝，
	// 而不是让设备回 0x06（partial transfer）。
	c := openSim(t, newSim(t))
	p := ref("big", "DINT_ARRAY", model.TypeBytes)
	p.Address.Length = maxReadBytes + 1
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil {
		t.Fatal("oversized bytes read accepted")
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v err=%v", samples, err)
	}
}

func TestReadBatchesMultipleServices(t *testing.T) {
	// 33 条读请求超过 450 字节预算，必须拆成两批；两批都成功。
	c := openSim(t, newSim(t))
	points := make([]driver.PointRef, 33)
	for i := range points {
		points[i] = ref("p", "DINT_TAG", model.TypeInt32)
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: points}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 33 {
		t.Fatalf("samples = %d", len(samples))
	}
	for _, s := range samples {
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %q err=%v", s.PointID, s.Quality, s.Err)
		}
	}
}

func TestCIPTypeMapping(t *testing.T) {
	cases := map[model.DataType]byte{
		model.TypeBool:    enipcodec.TypeBOOL,
		model.TypeInt16:   enipcodec.TypeINT,
		model.TypeUint16:  enipcodec.TypeUINT,
		model.TypeInt32:   enipcodec.TypeDINT,
		model.TypeUint32:  enipcodec.TypeUDINT,
		model.TypeInt64:   enipcodec.TypeLINT,
		model.TypeUint64:  enipcodec.TypeULINT,
		model.TypeFloat32: enipcodec.TypeREAL,
		model.TypeFloat64: enipcodec.TypeLREAL,
	}
	for dt, want := range cases {
		got, ok := cipTypeOf(dt)
		if !ok || got != want {
			t.Fatalf("cipTypeOf(%q) = %02X/%v, want %02X", dt, got, ok, want)
		}
	}
	for _, dt := range []model.DataType{model.TypeString, model.TypeBytes, model.TypeBCD16, model.TypeBCD32} {
		if _, ok := cipTypeOf(dt); ok {
			t.Fatalf("cipTypeOf(%q) unexpectedly mapped", dt)
		}
	}
}

func TestOpenDialFailure(t *testing.T) {
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return nil, errScript
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("10.0.0.5:44818"),
		Device:  model.Device{ID: "plc", ChannelID: "ch"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected || !driver.IsRetryable(err) {
		t.Fatalf("err = %v kind=%v", err, driver.KindOf(err))
	}
}

func TestProbePathEncodeFailure(t *testing.T) {
	// LogicalPath 不会失败（参数固定合法），这里验证的是它确实可用；
	// 保留一条断言防止以后改成「可失败的路径构造」时忘了处理。
	if _, err := enipcodec.LogicalPath(0x01, 0x01); err != nil {
		t.Fatal(err)
	}
}

func TestReadByteArrayOffsetPreservesPrefix(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	// DINT_ARRAY = [10,20,30,40]；从第 1 个元素起写 8 字节。
	p := model.Address{Kind: model.AddressRegister, Area: "DINT_ARRAY", Offset: 1, Bit: -1, Length: 8}
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "chunk", Address: p, DataType: model.TypeBytes,
		Value: model.BytesValue([]byte{0xAA, 0, 0, 0, 0xBB, 0, 0, 0}),
	}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	vals := []int32{10, 0xAA, 0xBB, 40}
	for i, want := range vals {
		r := ref("v", "DINT_ARRAY", model.TypeInt32)
		r.Address.Offset = i
		s := readOne(t, c, r)
		if got, _ := s.Value.AsFloat(); got != float64(want) {
			t.Fatalf("element %d = %v, want %d", i, s.Value, want)
		}
	}
}
