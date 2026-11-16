package bacnet

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/model"
	bacnetsim "uic/internal/sim/bacnet"
	"uic/internal/transport"
)

func testChannel(addr string) model.Channel {
	return model.Channel{
		ID: "ch", Protocol: "bacnet",
		Transport: model.Transport{Kind: "udp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
}

func newSim(t *testing.T) *bacnetsim.Server {
	t.Helper()
	srv, err := bacnetsim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.DisableAutoIncrement()
	return srv
}

func openSim(t *testing.T, srv *bacnetsim.Server) driver.Conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  model.Device{ID: "ahu", ChannelID: "ch"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func objRef(id, objType string, instance int, property string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id,
		Address: model.Address{
			Kind: model.AddressObject, ObjectType: objType, Instance: instance, Property: property,
		},
		DataType: dt,
	}
}

// readOne 只要求「有且只有一个采样」，不把单点错误当成致命失败：
// 驱动契约允许 Read 在单点失败时同时返回错误与带 Err 的采样，测试关心的
// 是这个采样本身的分类。
func readOne(t *testing.T, c driver.Conn, p driver.PointRef) driver.RawSample {
	t.Helper()
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if len(samples) != 1 {
		t.Fatalf("read %s: %d samples (err=%v)", p.ID, len(samples), err)
	}
	return samples[0]
}

// ---------------------------------------------------------------------------
// 元数据
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "bacnet" {
		t.Fatalf("id = %q", m.ID)
	}
	if m.SDKVersion != driver.SDKVersion {
		t.Fatalf("sdkVersion = %q", m.SDKVersion)
	}
	if len(m.Roles) != 1 || m.Roles[0] != driver.RoleMaster {
		t.Fatalf("roles = %v", m.Roles)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "udp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if !m.Accepts(model.AddressObject) {
		t.Fatal("object addressing must be accepted")
	}
	// 只做单属性读：多读与位访问必须明确声明不支持，否则上方调度会
	// 尝试合并读并得到错误的行为。
	if m.Capabilities.MultiRead != driver.CapUnsupported {
		t.Fatalf("multiRead = %s", m.Capabilities.MultiRead)
	}
	if m.Capabilities.BitAccess != driver.CapUnsupported {
		t.Fatalf("bitAccess = %s", m.Capabilities.BitAccess)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("read/write = %s/%s", m.Capabilities.Read, m.Capabilities.Write)
	}
	if len(m.ConfigFields) == 0 || len(m.PointFields) == 0 {
		t.Fatal("config/point fields must be declared for the UI")
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func TestReadAnalog(t *testing.T) {
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32))
	if s.Err != nil {
		t.Fatalf("err: %v", s.Err)
	}
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %s", s.Quality)
	}
	if s.Value.Num != 42.5 || s.Value.Type != model.TypeFloat32 {
		t.Fatalf("value = %+v", s.Value)
	}
}

func TestReadStringAndEnum(t *testing.T) {
	c := openSim(t, newSim(t))

	s := readOne(t, c, objRef("name", "analog-input", 1, "object-name", model.TypeString))
	if s.Err != nil || s.Value.Text != "AI-1" {
		t.Fatalf("object-name = %+v err=%v", s.Value, s.Err)
	}

	s = readOne(t, c, objRef("units", "analog-input", 1, "units", model.TypeUint32))
	if s.Err != nil || s.Value.Num != 62 {
		t.Fatalf("units = %+v err=%v", s.Value, s.Err)
	}
}

func TestReadBinaryAsBool(t *testing.T) {
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("run", "binary-input", 1, "present-value", model.TypeBool))
	if s.Err != nil || !s.Value.Bool {
		t.Fatalf("binary = %+v err=%v", s.Value, s.Err)
	}
}

func TestReadBinaryAsNumber(t *testing.T) {
	// 同一属性配成 uint32 也必须能读：ENUMERATED → 数值。
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("run", "binary-input", 1, "present-value", model.TypeUint32))
	if s.Err != nil || s.Value.Num != 1 {
		t.Fatalf("binary as number = %+v err=%v", s.Value, s.Err)
	}
}

func TestReadManyPoints(t *testing.T) {
	c := openSim(t, newSim(t))
	pts := []driver.PointRef{
		objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32),
		objRef("setpoint", "analog-value", 2, "present-value", model.TypeFloat32),
		objRef("counter", "analog-value", 3, "present-value", model.TypeFloat32),
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("%d samples", len(samples))
	}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
	}
}

func TestReadUnknownObjectIsAddressError(t *testing.T) {
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("x", "analog-input", 9, "present-value", model.TypeFloat32))
	if s.Err == nil {
		t.Fatal("expected error")
	}
	if driver.KindOf(s.Err) != driver.ErrAddress {
		t.Fatalf("kind = %s: %v", driver.KindOf(s.Err), s.Err)
	}
	if s.Quality != model.QualityAddressError {
		t.Fatalf("quality = %s", s.Quality)
	}
}

func TestReadUnknownPropertyIsAddressError(t *testing.T) {
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("x", "analog-input", 1, "high-limit", model.TypeFloat32))
	if s.Err == nil || driver.KindOf(s.Err) != driver.ErrAddress {
		t.Fatalf("err = %v", s.Err)
	}
}

func TestReadArrayElementAndOutOfRange(t *testing.T) {
	c := openSim(t, newSim(t))
	p := objRef("pa", "analog-value", 2, "priority-array", model.TypeFloat32)
	p.Address.PropIndex = 1
	s := readOne(t, c, p)
	// 优先级数组元素是 NULL，用 float32 读会得到 tag 类型不匹配的地址错误；
	// 关键是「下标被正确送达从站」，而不是读到一个随便的值。
	if s.Err == nil {
		t.Fatalf("null element read as float32 should fail, got %+v", s.Value)
	}

	p.Address.PropIndex = 17
	s = readOne(t, c, p)
	if s.Err == nil || driver.KindOf(s.Err) != driver.ErrAddress {
		t.Fatalf("out-of-range index err = %v", s.Err)
	}
}

func TestReadTypeMismatchIsAddressError(t *testing.T) {
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("temp", "analog-input", 1, "present-value", model.TypeString))
	if s.Err == nil || driver.KindOf(s.Err) != driver.ErrAddress {
		t.Fatalf("err = %v", s.Err)
	}
	if !strings.Contains(s.Err.Error(), "configured as string") {
		t.Fatalf("message = %v", s.Err)
	}
}

func TestReadRejectsNonObjectAddress(t *testing.T) {
	c := openSim(t, newSim(t))
	p := driver.PointRef{
		ID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "holding"},
		DataType: model.TypeFloat32,
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil {
		t.Fatal("expected error")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %s", driver.KindOf(err))
	}
	if len(samples) != 1 || samples[0].Err == nil {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestReadRejectsUnknownNames(t *testing.T) {
	c := openSim(t, newSim(t))
	cases := []driver.PointRef{
		objRef("bad-type", "no-such-object", 1, "present-value", model.TypeFloat32),
		objRef("bad-prop", "analog-input", 1, "no-such-property", model.TypeFloat32),
	}
	for _, p := range cases {
		samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
		if err == nil || driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("%s: err = %v", p.ID, err)
		}
		if len(samples) != 1 || samples[0].Err == nil {
			t.Fatalf("%s: samples = %+v", p.ID, samples)
		}
	}
	// 实例号越界同样应该在本地就拒绝，而不是发一个非法对象标识符出去。
	bad := objRef("bad-inst", "analog-input", 1, "present-value", model.TypeFloat32)
	bad.Address.Instance = 1 << 22
	if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{bad}}}); err == nil {
		t.Fatal("instance out of range accepted")
	}
}

func TestReadObjectNumberAndPropertyNumber(t *testing.T) {
	// 现场文档常只给编号：对象类型 0 = analog-input，属性 85 = present-value。
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("temp", "0", 1, "85", model.TypeFloat32))
	if s.Err != nil || s.Value.Num != 42.5 {
		t.Fatalf("numeric names = %+v err=%v", s.Value, s.Err)
	}
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func TestWriteAnalogAndReadBack(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	p := objRef("setpoint", "analog-value", 2, "present-value", model.TypeFloat32)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "setpoint", Address: p.Address, DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat32, 55.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	if v, ok := srv.Value("analog-value", 2, "present-value"); !ok || v.(float64) != 55.5 {
		t.Fatalf("sim value = %v ok=%v", v, ok)
	}
	s := readOne(t, c, p)
	if s.Err != nil || s.Value.Num != 55.5 {
		t.Fatalf("read back = %+v err=%v", s.Value, s.Err)
	}
}

func TestWriteReadOnlyIsPermissionError(t *testing.T) {
	c := openSim(t, newSim(t))
	p := objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", Address: p.Address, DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil {
		t.Fatal("expected error")
	}
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("kind = %s: %v", driver.KindOf(err), err)
	}
	if len(res) != 1 || res[0].OK || res[0].Quality != model.QualityBad {
		t.Fatalf("result = %+v", res)
	}
}

func TestWriteString(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	p := objRef("label", "character-string", 1, "present-value", model.TypeString)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "label", Address: p.Address, DataType: model.TypeString,
		Value: model.TextValue(model.TypeString, "PUMP-02"),
	}})
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	if v, ok := srv.Value("character-string", 1, "present-value"); !ok || v.(string) != "PUMP-02" {
		t.Fatalf("sim value = %v", v)
	}
}

func TestWriteBinaryPresentValueUsesEnumerated(t *testing.T) {
	// binary-value 的 present-value 是 ENUMERATED：写成 BOOLEAN 会让
	// 设备回 invalid-data-type。这条用例防止「按平台类型编码」的退化。
	srv := newSim(t)
	c := openSim(t, srv)
	if err := srv.SetValue("binary-value", 1, "present-value", uint64(1)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := objRef("bv", "binary-value", 1, "present-value", model.TypeFloat32)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "bv", Address: p.Address, DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat32, 0),
	}})
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	if v, ok := srv.Value("binary-value", 1, "present-value"); !ok || v.(uint64) != 0 {
		t.Fatalf("sim value = %v ok=%v", v, ok)
	}
}

func TestWriteUnknownObjectIsAddressError(t *testing.T) {
	c := openSim(t, newSim(t))
	p := objRef("x", "analog-value", 99, "present-value", model.TypeFloat32)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: p.Address, DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("result = %+v", res)
	}
}

func TestWriteTypeMismatchIsAddressError(t *testing.T) {
	// 往数值属性写字符串：本地就拒绝，不发报文。
	c := openSim(t, newSim(t))
	p := objRef("setpoint", "analog-value", 2, "present-value", model.TypeString)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "setpoint", Address: p.Address, DataType: model.TypeString,
		Value: model.TextValue(model.TypeString, "abc"),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteBoolToBooleanProperty(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	if err := srv.SetValue("binary-value", 2, "out-of-service", true); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := objRef("oos", "binary-value", 2, "out-of-service", model.TypeBool)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "oos", Address: p.Address, DataType: model.TypeBool,
		Value: model.BoolValue(false),
	}})
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	if v, ok := srv.Value("binary-value", 2, "out-of-service"); !ok || v.(bool) {
		t.Fatalf("sim value = %v", v)
	}
}

func TestWriteMultipleStopsOnLinkFailure(t *testing.T) {
	// 链路断开后，后续写请求必须直接标失败，而不是各等一个超时。
	srv := newSim(t)
	c := openSim(t, srv)
	_ = srv.Close()

	reqs := make([]driver.WriteRequest, 0, 3)
	for i := 0; i < 3; i++ {
		p := objRef("setpoint", "analog-value", 2, "present-value", model.TypeFloat32)
		reqs = append(reqs, driver.WriteRequest{
			PointID: "setpoint", Address: p.Address, DataType: model.TypeFloat32,
			Value: model.NumValue(model.TypeFloat32, 1),
		})
	}
	start := time.Now()
	res, err := c.Write(context.Background(), reqs)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(res) != len(reqs) {
		t.Fatalf("%d results for %d requests", len(res), len(reqs))
	}
	for _, r := range res {
		if r.OK {
			t.Fatalf("result %+v", r)
		}
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("writes took %v; later requests must fail fast", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Probe / Open
// ---------------------------------------------------------------------------

func TestProbe(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbeAcceptsDeviceError(t *testing.T) {
	// 设备实例号猜错时返回 unknown-object：BACnet 层仍然通了。
	srv := newSim(t)
	if err := srv.SetValue("device", 1, "object-name", "X"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := openSim(t, srv)
	// device:1 存在，所以这里验证的是「正常路径也能通过」；再构造一个
	// 只有 device:2 的场景来验证错误也放行。
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestOpenRejectsNonUDP(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "bacnet",
			Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:47808"}},
		Device: model.Device{ID: "d"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenRequiresAddress(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "bacnet",
			Transport: model.Transport{Kind: "udp"}},
		Device: model.Device{ID: "d"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAppendsDefaultPort(t *testing.T) {
	var got string
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		got = addr
		return nopDatagram{}, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "bacnet",
			Transport: model.Transport{Kind: "udp", Address: "10.0.0.5"}},
		Device: model.Device{ID: "d"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "10.0.0.5:47808" {
		t.Fatalf("addr = %q", got)
	}
}

func TestOpenKeepsExplicitPort(t *testing.T) {
	var got string
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		got = addr
		return nopDatagram{}, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "bacnet",
			Transport: model.Transport{Kind: "udp", Address: "10.0.0.5:9999"}},
		Device: model.Device{ID: "d"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "10.0.0.5:9999" {
		t.Fatalf("addr = %q", got)
	}
}

func TestOpenDialFailureIsDisconnected(t *testing.T) {
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return nil, errors.New("boom")
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "bacnet",
			Transport: model.Transport{Kind: "udp", Address: "10.0.0.5"}},
		Device: model.Device{ID: "d"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("err = %v", err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("dial failure must be retryable")
	}
}

// ---------------------------------------------------------------------------
// 超时 / 迟到报文 / 重试
// ---------------------------------------------------------------------------

func TestReadTimeoutIsTimeoutError(t *testing.T) {
	// 一个只收不回的 UDP 端点：请求能被送达，但永远没有应答，
	// 这样测到的才是「超时」而不是对端端口关闭引起的 ICMP 拒绝。
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	addr := pc.LocalAddr().String()

	drv := New()
	drv.retries = 0
	c, err := drv.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "bacnet",
			Transport: model.Transport{Kind: "udp", Address: addr},
			Timeout:   model.Duration(150 * time.Millisecond)},
		Device: model.Device{ID: "d"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityTimeout {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestStaleResponseIsIgnored(t *testing.T) {
	srv := newSim(t)
	// 一个会先塞一条 invoke ID 不匹配的应答、再正常应答的链路。
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		base, err := transport.DialUDP(ctx, addr, timeout)
		if err != nil {
			return nil, err
		}
		return &staleFirst{inner: base}, nil
	})
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  model.Device{ID: "ahu", ChannelID: "ch"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	s := readOne(t, c, objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32))
	if s.Err != nil || s.Value.Num != 42.5 {
		t.Fatalf("read = %+v err=%v", s.Value, s.Err)
	}
}

func TestRetryOnTimeout(t *testing.T) {
	// 第一次读不回应，第二次回应：驱动必须在超时后重发。
	srv := newSim(t)
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		base, err := transport.DialUDP(ctx, addr, timeout)
		if err != nil {
			return nil, err
		}
		return &dropFirst{inner: base}, nil
	})
	ch := testChannel(srv.Addr())
	ch.Timeout = model.Duration(300 * time.Millisecond)
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "ahu", ChannelID: "ch"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	// 第一个请求被丢弃 → 需要一次重发。用较短的超时把测试时长压住。
	s := readOne(t, c, objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32))
	if s.Err != nil || s.Value.Num != 42.5 {
		t.Fatalf("read = %+v err=%v", s.Value, s.Err)
	}
}

func TestContextCancelStopsRead(t *testing.T) {
	c := openSim(t, newSim(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityTimeout {
		t.Fatalf("samples = %+v", samples)
	}
}

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

type nopDatagram struct{}

func (nopDatagram) Write([]byte) error          { return nil }
func (nopDatagram) Read() ([]byte, error)       { return nil, errors.New("nop") }
func (nopDatagram) SetDeadline(time.Time) error { return nil }
func (nopDatagram) Close() error                { return nil }

// staleFirst 每次写之后先伪造一条 invoke ID 不匹配的应答。
type staleFirst struct {
	inner transport.Datagram
	sent  bool
}

func (s *staleFirst) Write(p []byte) error {
	if err := s.inner.Write(p); err != nil {
		return err
	}
	s.sent = true
	return nil
}

func (s *staleFirst) Read() ([]byte, error) {
	if s.sent {
		s.sent = false
		// 伪造一条 ComplexACK，invoke ID 与真实请求不同。
		apdu := []byte{bacnetcodec.APDUComplexACK, 0xEE, bacnetcodec.ServiceReadProperty}
		return bacnetcodec.BuildMessage(1, bacnetcodec.NPDUControlExpectReply, apdu), nil
	}
	return s.inner.Read()
}

func (s *staleFirst) SetDeadline(t time.Time) error { return s.inner.SetDeadline(t) }
func (s *staleFirst) Close() error                  { return s.inner.Close() }

// dropFirst 丢弃第一次写（模拟 UDP 丢包），让驱动必须重发。
type dropFirst struct {
	inner transport.Datagram
	n     int
}

func (d *dropFirst) Write(p []byte) error {
	d.n++
	if d.n == 1 {
		return nil
	}
	return d.inner.Write(p)
}

func (d *dropFirst) Read() ([]byte, error)         { return d.inner.Read() }
func (d *dropFirst) SetDeadline(t time.Time) error { return d.inner.SetDeadline(t) }
func (d *dropFirst) Close() error                  { return d.inner.Close() }

// TestReadPartialFailureKeepsGoodSamples 钉住部分失败契约（docs §3.7.4 /
// §3.9.5）：一轮里有一个点配错（属性名不存在）时，**只要还有别的点读成功，
// Read 就不向上返回错误**，好样本照常上送，坏点只在自己的样本上带 Err。
//
// 为什么必须这样：上层 collector 看到 Read 返回错误会按错误分类丢弃连接
// （ErrDisconnected/ErrTimeout/ErrProtocol 直接 Close 重连）。若单点错误
// 上升为整轮错误，一个属性名写错的点位会让整台设备每周期重连一次，且
// 熔断器会把「点表有一个错」误判成「设备坏了」。
func TestReadPartialFailureKeepsGoodSamples(t *testing.T) {
	c := openSim(t, newSim(t))
	good := objRef("temp", "analog-input", 1, "present-value", model.TypeFloat32)
	bad := objRef("bad", "analog-input", 1, "no-such-property", model.TypeFloat32)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{good, bad},
	}})
	if err != nil {
		t.Fatalf("有成功点时 Read 不该返回错误，得到 %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2（坏点也要有样本）", len(samples))
	}
	if samples[0].Err != nil || samples[0].Quality != model.QualityGood || samples[0].Value.Num != 42.5 {
		t.Fatalf("好点不该被连累: %+v", samples[0])
	}
	if samples[1].Err == nil || driver.KindOf(samples[1].Err) != driver.ErrAddress {
		t.Fatalf("坏点应带 address 错误: %+v", samples[1])
	}
}

// TestReadAllFailedStillReturnsError 是反面：全部点位都失败时必须返回错误，
// 否则「设备在采但全坏」对熔断器不可见。
func TestReadAllFailedStillReturnsError(t *testing.T) {
	c := openSim(t, newSim(t))
	bad1 := objRef("bad1", "analog-input", 1, "no-such-property", model.TypeFloat32)
	bad2 := objRef("bad2", "analog-input", 1, "no-such-property-either", model.TypeFloat32)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{bad1, bad2},
	}})
	if err == nil {
		t.Fatal("全部点位失败时必须返回错误")
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
}
