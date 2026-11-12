package bacnetmstp

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
	simmstp "uic/internal/sim/bacnetmstp"
)

const testMAC = 128

func newTestConn(t *testing.T, labels map[string]string) (*simmstp.Server, driver.Conn) {
	t.Helper()
	srv, err := simmstp.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ch := model.Channel{
		ID: "ch-mstp", Protocol: "bacnet-mstp",
		Transport:   model.Transport{Kind: "tcp", Address: srv.Addr()},
		Concurrency: 1, Timeout: model.Duration(500 * time.Millisecond),
	}
	dev := model.Device{
		ID: "dev", ChannelID: ch.ID, Interval: model.Duration(time.Second), Enabled: true,
		Labels: labels,
	}
	if _, ok := dev.Labels["address"]; !ok {
		dev.UnitID = testMAC
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return srv, c
}

func objPoint(id, objType string, instance int, property string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id,
		Address: model.Address{
			Kind: model.AddressObject, ObjectType: objType,
			Instance: instance, Property: property,
		},
		DataType: dt,
	}
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "bacnet-mstp" || m.Name != "BACnet MS/TP" {
		t.Fatalf("meta = %+v", m)
	}
	if len(m.Transports) != 2 || m.Transports[0] != "serial" || m.Transports[1] != "tcp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if len(m.AddressKinds) != 1 || m.AddressKinds[0] != model.AddressObject {
		t.Fatalf("address kinds = %v", m.AddressKinds)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
	if m.Capabilities.Browse == driver.CapSupported || m.Capabilities.Discovery == driver.CapSupported {
		t.Fatalf("browse/discovery must be unsupported: %+v", m.Capabilities)
	}
}

func TestProbeAndRead(t *testing.T) {
	_, c := newTestConn(t, nil)
	ctx := context.Background()
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}

	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
		objPoint("name", "analog-input", 1, "object-name", model.TypeString),
		objPoint("units", "analog-input", 1, "units", model.TypeUint32),
		objPoint("run", "binary-input", 1, "present-value", model.TypeBool),
		objPoint("label", "character-string", 1, "present-value", model.TypeString),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 5 {
		t.Fatalf("samples = %d", len(samples))
	}
	if got := samples[0].Value.Num; got != 42.5 {
		t.Fatalf("temp = %v, want 42.5", got)
	}
	if got := samples[1].Value.Text; got != "AI-1" {
		t.Fatalf("name = %q", got)
	}
	if got := samples[2].Value.Num; got != 62 {
		t.Fatalf("units = %v, want 62", got)
	}
	if !samples[3].Value.Bool {
		t.Fatalf("run = %+v, want true", samples[3].Value)
	}
	if got := samples[4].Value.Text; got != "PUMP-01" {
		t.Fatalf("label = %q", got)
	}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %s", s.PointID, s.Quality)
		}
	}
}

func TestWriteAndReadBack(t *testing.T) {
	srv, c := newTestConn(t, nil)
	ctx := context.Background()
	pt := objPoint("sp", "analog-value", 2, "present-value", model.TypeFloat32)

	res, err := c.Write(ctx, []driver.WriteRequest{{
		PointID: "sp", Address: pt.Address, DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat32, 21.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("write result = %+v", res)
	}
	if v, ok := srv.Value("analog-value", 2, "present-value"); !ok || v != 21.5 {
		t.Fatalf("sim value = %v (%v), want 21.5", v, ok)
	}
	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{pt}}})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := samples[0].Value.Num; got != 21.5 {
		t.Fatalf("read back = %v", got)
	}

	// 字符串对象同样走 present-value，但类型由对象决定为 CHARACTER STRING。
	if _, err := c.Write(ctx, []driver.WriteRequest{{
		PointID: "label",
		Address: model.Address{Kind: model.AddressObject, ObjectType: "character-string",
			Instance: 1, Property: "present-value"},
		DataType: model.TypeString,
		Value:    model.TextValue(model.TypeString, "PUMP-02"),
	}}); err != nil {
		t.Fatalf("write string: %v", err)
	}
	if v, _ := srv.Value("character-string", 1, "present-value"); v != "PUMP-02" {
		t.Fatalf("string value = %v", v)
	}
}

func TestReadOnlyWriteMapsToPermission(t *testing.T) {
	_, c := newTestConn(t, nil)
	pt := objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", Address: pt.Address, DataType: pt.DataType,
		Value: model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil {
		t.Fatal("write to read-only property succeeded")
	}
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("kind = %s, want permission: %v", driver.KindOf(err), err)
	}
	if len(res) != 1 || res[0].OK || res[0].Quality != model.QualityBad {
		t.Fatalf("result = %+v", res)
	}
}

func TestAddressErrors(t *testing.T) {
	_, c := newTestConn(t, nil)
	ctx := context.Background()

	cases := []struct {
		name string
		ref  driver.PointRef
		kind driver.ErrorKind
	}{
		{"unknown object", objPoint("x", "analog-input", 999, "present-value", model.TypeFloat32), driver.ErrAddress},
		{"unknown property", objPoint("x", "analog-input", 1, "no-such-prop", model.TypeFloat32), driver.ErrAddress},
		{"unknown object type", objPoint("x", "no-such-type", 1, "present-value", model.TypeFloat32), driver.ErrAddress},
		{"bad array index", objPoint("x", "analog-value", 2, "priority-array", model.TypeFloat32), driver.ErrAddress},
		{"non-object address", driver.PointRef{ID: "x",
			Address:  model.Address{Kind: model.AddressRegister, Offset: 0, Length: 1},
			DataType: model.TypeFloat32}, driver.ErrAddress},
	}
	// 数组下标越界应由从站回 Error(invalid-array-index)，驱动映射成地址错误；
	// 这里单独构造，避免依赖 propIndex 字段的零值语义。
	bad := objPoint("x", "analog-value", 2, "priority-array", model.TypeFloat32)
	bad.Address.PropIndex = 17
	cases[3].ref = bad

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{tc.ref}}})
			if err == nil {
				t.Fatalf("read succeeded: %+v", samples)
			}
			if got := driver.KindOf(err); got != tc.kind {
				t.Fatalf("kind = %s, want %s (%v)", got, tc.kind, err)
			}
		})
	}
}

func TestArrayElementRead(t *testing.T) {
	_, c := newTestConn(t, nil)
	pt := objPoint("prio1", "analog-value", 2, "priority-array", model.TypeFloat32)
	pt.Address.PropIndex = 1
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{pt}}})
	// 优先级数组默认全 NULL；驱动把 NULL 当数值类型解码失败，属于点位
	// 配置错误而不是链路错误。这里只断言没有把 NULL 解成 0。
	if err == nil {
		t.Fatalf("NULL element decoded as %+v", samples)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %s, want address", driver.KindOf(err))
	}
	if len(samples) != 1 || samples[0].Err == nil {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestOpenRejectsBadAddresses(t *testing.T) {
	newDriver := func() *Driver { return New() }
	base := model.Channel{ID: "ch", Protocol: "bacnet-mstp",
		Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"}}
	mk := func(labels map[string]string) driver.OpenConfig {
		return driver.OpenConfig{
			Channel: base,
			Device: model.Device{ID: "dev", ChannelID: "ch", Interval: model.Duration(time.Second),
				Enabled: true, Labels: labels},
		}
	}

	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"missing slave", map[string]string{}},
		{"slave in master range", map[string]string{"address": "5"}},
		{"slave too large", map[string]string{"address": "255"}},
		{"bad master", map[string]string{"address": "128", "master": "200"}},
		{"bad next station", map[string]string{"address": "128", "nextStation": "250"}},
		{"bad presilence", map[string]string{"address": "128", "presilence": "soon"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newDriver().Open(context.Background(), mk(tc.labels)); err == nil {
				t.Fatal("open succeeded")
			} else if driver.KindOf(err) != driver.ErrUnsupported {
				t.Fatalf("kind = %s, want unsupported: %v", driver.KindOf(err), err)
			}
		})
	}

	// unitId 作为 labels.address 的回退。
	c := mk(map[string]string{})
	c.Device.UnitID = 130
	if _, err := newDriver().Open(context.Background(), c); err == nil {
		t.Fatal("open against 127.0.0.1:1 unexpectedly succeeded")
	} // 这里失败在 dial，说明地址校验已通过；不能断言错误种类。
}

func TestTimeoutIsRetriedAndClassified(t *testing.T) {
	// 一个只 accept 不回包的监听端：驱动必须超时，而不是挂死。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	ch := model.Channel{ID: "ch", Protocol: "bacnet-mstp",
		Transport: model.Transport{Kind: "tcp", Address: ln.Addr().String()},
		Timeout:   model.Duration(80 * time.Millisecond)}
	dev := model.Device{ID: "dev", ChannelID: "ch", Interval: model.Duration(time.Second),
		Enabled: true, UnitID: 128}
	c, err := New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	start := time.Now()
	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err == nil {
		t.Fatal("read succeeded against a silent peer")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %s, want timeout: %v", driver.KindOf(err), err)
	}
	// 默认重试 2 次 → 约 3 个超时周期。给一个宽松上限，只防「重试逻辑
	// 退化成无限等待」。
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v, retries look unbounded", elapsed)
	}
}

// ---------------------------------------------------------------------------
// 被动令牌处理
// ---------------------------------------------------------------------------

// fakeBus 是一个可编排的 MS/TP 字节流：Write 时把预设的帧塞进接收缓冲，
// ReadFull 从缓冲里取字节，空缓冲返回 deadline 错误。
type fakeBus struct {
	mu     sync.Mutex
	buf    []byte
	writes [][]byte
	// respond 在每次 Write 后调用，返回要追加到接收缓冲的帧。
	respond func(wire []byte) [][]byte
}

func (f *fakeBus) Write(p []byte) error {
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		for _, fr := range respond(p) {
			f.mu.Lock()
			f.buf = append(f.buf, fr...)
			f.mu.Unlock()
		}
	}
	return nil
}

func (f *fakeBus) ReadFull(n int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buf) < n {
		return nil, os.ErrDeadlineExceeded
	}
	out := append([]byte(nil), f.buf[:n]...)
	f.buf = f.buf[n:]
	return out, nil
}

func (f *fakeBus) Close() error                { return nil }
func (f *fakeBus) SetDeadline(time.Time) error { return nil }
func (f *fakeBus) Drain() error                { return nil }
func (f *fakeBus) Lock()                       {}
func (f *fakeBus) Unlock()                     {}
func (f *fakeBus) lastWrite() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return nil
	}
	return f.writes[len(f.writes)-1]
}

var _ link.Link = (*fakeBus)(nil)

func openFake(t *testing.T, bus link.Link, labels map[string]string) driver.Conn {
	t.Helper()
	ch := model.Channel{ID: "ch", Protocol: "bacnet-mstp",
		Transport: model.Transport{Kind: "tcp", Address: "fake:1"},
		Timeout:   model.Duration(100 * time.Millisecond)}
	dev := model.Device{ID: "dev", ChannelID: "ch", Interval: model.Duration(time.Second),
		Enabled: true, Labels: labels}
	d := NewWithDialer(func(context.Context, model.Transport, time.Duration, string) (link.Link, error) {
		return bus, nil
	})
	c, err := d.Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open fake: %v", err)
	}
	return c
}

func TestPassiveTokenIsAnsweredAndPassed(t *testing.T) {
	// 准备一条合法的 ComplexACK：读 analog-input:1 present-value。
	app, err := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 42.5, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, 1,
		bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, app)
	if err != nil {
		t.Fatal(err)
	}
	npdu := bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlNone, ack)

	mustFrame := func(f bacnetcodec.MSTPFrame) []byte {
		out, err := bacnetcodec.BuildMSTPFrame(f)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	token := mustFrame(bacnetcodec.MSTPFrame{Type: bacnetcodec.FrameToken, Dest: 0, Src: 5})
	poll := mustFrame(bacnetcodec.MSTPFrame{Type: bacnetcodec.FramePollForMaster, Dest: 0, Src: 5})
	testReq := mustFrame(bacnetcodec.MSTPFrame{Type: bacnetcodec.FrameTestRequest, Dest: 0, Src: 6})
	reply := mustFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 128, Data: npdu,
	})

	bus := &fakeBus{}
	bus.respond = func(wire []byte) [][]byte {
		// 只在收到数据请求时注入「主站间的握手帧 + 从站应答」。
		if len(wire) < 3 || wire[2] != bacnetcodec.FrameDataExpectingReply {
			return nil
		}
		return [][]byte{token, poll, testReq, reply}
	}
	c := openFake(t, bus, map[string]string{"address": "128", "nextStation": "5"})
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := samples[0].Value.Num; got != 42.5 {
		t.Fatalf("value = %v", got)
	}

	var sawReplyPoll, sawTestResp, sawToken bool
	bus.mu.Lock()
	for _, w := range bus.writes {
		f, _, err := bacnetcodec.DecodeMSTPFrame(w)
		if err != nil {
			t.Fatalf("driver wrote an invalid frame: %v", err)
		}
		switch {
		case f.Type == bacnetcodec.FrameReplyToPollForMaster && f.Dest == 5:
			sawReplyPoll = true
		case f.Type == bacnetcodec.FrameTestResponse && f.Dest == 6:
			sawTestResp = true
		case f.Type == bacnetcodec.FrameToken && f.Dest == 5:
			sawToken = true
		}
	}
	bus.mu.Unlock()
	if !sawReplyPoll {
		t.Error("driver did not answer PollForMaster")
	}
	if !sawTestResp {
		t.Error("driver did not answer TestRequest")
	}
	if !sawToken {
		t.Error("driver did not pass the token to the configured next station")
	}
}

func TestUnexpectedInvokeIsIgnored(t *testing.T) {
	app, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 7, nil, false)
	stale, _ := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, 9,
		bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, app)
	fresh, _ := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, 1,
		bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, app)
	mk := func(apdu []byte) []byte {
		return bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlNone, apdu)
	}
	mustFrame := func(data []byte) []byte {
		out, err := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
			Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 128, Data: data,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	bus := &fakeBus{}
	bus.respond = func(wire []byte) [][]byte {
		if len(wire) < 3 || wire[2] != bacnetcodec.FrameDataExpectingReply {
			return nil
		}
		// 先来一个 invoke=9 的迟到应答，再来 invoke=1 的正确应答。
		return [][]byte{mustFrame(mk(stale)), mustFrame(mk(fresh))}
	}
	c := openFake(t, bus, map[string]string{"address": "128"})
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := samples[0].Value.Num; got != 7 {
		t.Fatalf("value = %v, want the fresh reply", got)
	}
}

func TestJunkBeforeFrameIsSkipped(t *testing.T) {
	app, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 3.5, nil, false)
	ack, _ := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, 1,
		bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, app)
	frame, err := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 128,
		Data: bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlNone, ack),
	})
	if err != nil {
		t.Fatal(err)
	}
	junk := []byte{0x00, 0x55, 0x12, 0x55, 0x55, 0xFF, 0x00, 0x00}
	bus := &fakeBus{}
	bus.respond = func(wire []byte) [][]byte {
		if len(wire) < 3 || wire[2] != bacnetcodec.FrameDataExpectingReply {
			return nil
		}
		return [][]byte{append(append([]byte(nil), junk...), frame...)}
	}
	c := openFake(t, bus, map[string]string{"address": "128"})
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := samples[0].Value.Num; got != 3.5 {
		t.Fatalf("value = %v", got)
	}
}

func TestReplyPostponedIsProtocolError(t *testing.T) {
	postponed, err := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameReplyPostponed, Dest: 0, Src: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	bus := &fakeBus{}
	bus.respond = func(wire []byte) [][]byte {
		if len(wire) < 3 || wire[2] != bacnetcodec.FrameDataExpectingReply {
			return nil
		}
		return [][]byte{postponed}
	}
	c := openFake(t, bus, map[string]string{"address": "128"})
	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v (kind %s), want protocol", err, driver.KindOf(err))
	}
}

func TestWrongSlaveIsIgnored(t *testing.T) {
	app, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 9, nil, false)
	ack, _ := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, 1,
		bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, app)
	// 来自 129 的应答不该被当成 128 的应答；随后给出正确来源的应答。
	wrong, _ := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 129,
		Data: bacnetcodec.BuildNPDU(1, 0, ack),
	})
	right, _ := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 128,
		Data: bacnetcodec.BuildNPDU(1, 0, ack),
	})
	bus := &fakeBus{}
	bus.respond = func(wire []byte) [][]byte {
		if len(wire) < 3 || wire[2] != bacnetcodec.FrameDataExpectingReply {
			return nil
		}
		return [][]byte{wrong, right}
	}
	c := openFake(t, bus, map[string]string{"address": "128"})
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Num != 9 {
		t.Fatalf("value = %v", samples[0].Value.Num)
	}
}

func TestProbeErrorReplyCountsAsReachable(t *testing.T) {
	// device:1 不存在时从站回 Error(unknown-object)，Probe 仍应认为链路可用。
	errAPDU := bacnetcodec.BuildError(bacnetcodec.ServiceReadProperty, 1,
		bacnetcodec.ErrorClassObject, bacnetcodec.ErrorCodeUnknownObject)
	frame, _ := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 128,
		Data: bacnetcodec.BuildNPDU(1, 0, errAPDU),
	})
	bus := &fakeBus{}
	bus.respond = func(wire []byte) [][]byte {
		if len(wire) < 3 || wire[2] != bacnetcodec.FrameDataExpectingReply {
			return nil
		}
		return [][]byte{frame}
	}
	c := openFake(t, bus, map[string]string{"address": "128"})
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}
