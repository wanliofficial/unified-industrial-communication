package secs

import (
	"context"
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	"uic/internal/driver"
	"uic/internal/model"
	secssim "uic/internal/sim/secs"
)

// 驱动测试走真实 TCP 回环：HSMS 帧、Select 握手、系统字节配对都经过一次线格式，
// 这样「只在内存里对」的错误（帧头长度、W 位、应答 Stream/Function）会被抓住。

func startSim(t *testing.T) *secssim.Server {
	t.Helper()
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func svidPoint(id string, vid uint32, dt model.DataType) model.Point {
	return model.Point{
		ID: id, Address: model.Address{Kind: model.AddressObject, ObjectType: "svid", Instance: int(vid), Property: "value"},
		DataType: dt, Enabled: true,
	}
}

func ecidPoint(id string, eid uint32, dt model.DataType, writable bool) model.Point {
	return model.Point{
		ID: id, Address: model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: int(eid), Property: "value"},
		DataType: dt, Enabled: true, Writable: writable,
	}
}

func testConfig(srv *secssim.Server, pts ...model.Point) driver.OpenConfig {
	return driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "secs",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
		Device: model.Device{ID: "dev", ChannelID: "ch", Timeout: model.Duration(2 * time.Second), Points: pts},
	}
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "secs" || m.Capabilities.Read != driver.CapSupported {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported || m.Capabilities.Browse != driver.CapUnsupported {
		t.Fatalf("SECS 不做订阅/浏览: %+v", m.Capabilities)
	}
	if !m.Accepts(model.AddressObject) || m.Accepts(model.AddressSymbol) {
		t.Fatalf("SECS 只接受对象寻址")
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	cfg.Channel.Transport.Kind = "udp"
	if _, err := New().Open(context.Background(), cfg); driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("非 tcp 应 ErrUnsupported，得到 %v", err)
	}
}

func TestReadSVIDBatch(t *testing.T) {
	srv := startSim(t)
	srv.SetVID(1, secscodec.Int(secscodec.FormatU4, 25))
	srv.SetVID(2, secscodec.ASCII("RUN"))
	srv.SetVID(3, secscodec.Float(secscodec.FormatF4, 21.5))

	cfg := testConfig(srv,
		svidPoint("temp", 1, model.TypeUint32),
		svidPoint("state", 2, model.TypeString),
		svidPoint("f", 3, model.TypeFloat32),
	)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s1", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
		driver.PointRefOf(cfg.Device, cfg.Device.Points[1]),
		driver.PointRefOf(cfg.Device, cfg.Device.Points[2]),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("样本数 = %d", len(samples))
	}
	got := map[string]driver.RawSample{}
	for _, s := range samples {
		got[s.PointID] = s
	}
	if v := got["temp"].Value; v.Num != 25 {
		t.Fatalf("temp = %+v", v)
	}
	if v := got["state"].Value; v.Text != "RUN" {
		t.Fatalf("state = %+v", v)
	}
	if got["temp"].Quality != model.QualityGood {
		t.Fatalf("质量 = %s", got["temp"].Quality)
	}
	if srv.Requests() < 1 {
		t.Fatal("sim 未收到请求")
	}
}

func TestReadMissingSVID(t *testing.T) {
	srv := startSim(t)
	// 表里只有 1，请求 999 → 应答缺该编号 → address_error。
	cfg := testConfig(srv, svidPoint("p", 999, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if len(samples) != 1 || driver.KindOf(samples[0].Err) != driver.ErrAddress {
		t.Fatalf("缺失 SVID 应 address_error: %+v", samples)
	}
}

func TestWriteECID(t *testing.T) {
	srv := startSim(t)
	srv.SetECID(10, secscodec.Int(secscodec.FormatU4, 300))
	cfg := testConfig(srv, ecidPoint("limit", 10, model.TypeUint32, true))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "limit", Address: model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: 10, Property: "value"},
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 350),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK || res[0].Quality != model.QualityGood {
		t.Fatalf("写结果 = %+v", res[0])
	}
	v, ok := srv.ValueOf(secscodec.ObjectECID, 10)
	if !ok {
		t.Fatal("sim 未更新 ECID 10")
	}
	if n, _ := v.U4(); n != 350 {
		t.Fatalf("sim ECID = %d, want 350", n)
	}
}

// TestWriteACKsButValueUnchanged 钉住一条重要的边界：S2F16 的 ACKC7=0 只证明
// 「设备接受了请求」，不证明「值已经生效」。驱动层的 Write 只负责把 ACKC7 翻译成
// OK/失败——这是协议层语义，不是缺陷。真正防「ACK 通过但值没变」的是自检命令
// 的写后重读确认（cmd/uic/secsread.go），那条路径由 RejectWrites 仿真覆盖。
// 这一条测试保证驱动层的这个语义边界不会被后续改动悄悄抹平（例如有人「好心」
// 在驱动里加一次隐式重读，把「写成功但值没变」也变成驱动层的成功）。
func TestWriteACKsButValueUnchanged(t *testing.T) {
	srv, err := secssim.NewServerWithOptions("127.0.0.1:0", secssim.Options{RejectWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	srv.SetECID(10, secscodec.Int(secscodec.FormatU4, 300))
	cfg := testConfig(srv, ecidPoint("limit", 10, model.TypeUint32, true))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "limit", Address: model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: 10, Property: "value"},
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 350),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// 驱动层的成功判据就是 ACKC7=0：这一条必须是 OK，因为驱动不能替协议
	// 承诺它没承诺过的事（值生效）。真正校验生效与否由自检命令的
	// verify 路径负责。
	if !res[0].OK || res[0].Quality != model.QualityGood {
		t.Fatalf("驱动层 ACKC7=0 应翻译为 OK，得到 %+v", res[0])
	}
	v, ok := srv.ValueOf(secscodec.ObjectECID, 10)
	if !ok {
		t.Fatal("sim 应保留 ECID 10")
	}
	if n, _ := v.U4(); n != 300 {
		t.Fatalf("RejectWrites 设备应保留旧值，得到 %d（驱动不应隐式重读校验生效）", n)
	}
}

func TestWriteSVIDRejected(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv, svidPoint("temp", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", Address: model.Address{Kind: model.AddressObject, ObjectType: "svid", Instance: 1, Property: "value"},
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 1),
	}})
	if len(res) != 1 || driver.KindOf(res[0].Err) != driver.ErrPermission {
		t.Fatalf("写 SVID 应 permission: %+v", res)
	}
}

func TestProbe(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// TestReadIgnoresAsyncPush 验证主站收到设备主动推送（S6F11/S5F1）时能正确
// 忽略、继续等自己的应答。没有这条，异步推送会打断事务。
func TestReadIgnoresAsyncPush(t *testing.T) {
	srv := startSim(t)
	srv.SetVID(1, secscodec.Int(secscodec.FormatU4, 42))
	cfg := testConfig(srv, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 先推两条异步消息，再发起读：读请求发出后 sim 会先收到推送吗？不会——
	// 推送由测试主动触发，这里在读之前推送，验证它们不残留影响下一次读。
	srv.PushEvent(7, secscodec.Report{ReportID: 1, Values: []secscodec.StatusValue{{ID: 1, Value: secscodec.Int(secscodec.FormatU4, 1)}}})
	srv.PushAlarm(secscodec.Alarm{IsSet: true, Kind: 3, ALID: 5, ALTX: "HOT"})
	time.Sleep(50 * time.Millisecond)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if err != nil {
		t.Fatalf("read after push: %v", err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityGood {
		t.Fatalf("异步推送不应影响读: %+v", samples)
	}
	if v := samples[0].Value; v.Num != 42 {
		t.Fatalf("读到 %+v，want 42", v)
	}
}

func TestReadDataTypeMismatch(t *testing.T) {
	srv := startSim(t)
	// sim 里 VID 1 是字符串，点位声明 int32 → 转换应失败（bad quality，不 panic）。
	srv.SetVID(1, secscodec.ASCII("RUN"))
	cfg := testConfig(srv, svidPoint("p", 1, model.TypeInt32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if len(samples) != 1 || samples[0].Err == nil {
		t.Fatalf("类型不符应报错: %+v", samples)
	}
}

func TestReadRangeCheck(t *testing.T) {
	srv := startSim(t)
	// VID 1 = 70000（超出 uint16），点位声明 uint16 → 范围校验失败。
	srv.SetVID(1, secscodec.Int(secscodec.FormatU4, 70000))
	cfg := testConfig(srv, svidPoint("p", 1, model.TypeUint16))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if len(samples) != 1 || driver.KindOf(samples[0].Err) != driver.ErrProtocol {
		t.Fatalf("越界应 ErrProtocol: %+v", samples)
	}
}
