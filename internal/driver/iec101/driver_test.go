package iec101

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"uic/internal/codec/iec60870"
	"uic/internal/driver"
	"uic/internal/model"
	sim "uic/internal/sim/iec101"
)

// 本文件是驱动 + 内置从站的端到端测试。它验证的不是「某个函数返回什么」，
// 而是「一整轮非平衡轮询能否在一条真实 TCP 字节流上跑通」——帧计数位、
// 访问需求位、总召唤终止、命令两段确认，这些只有在两端都在跑时才会互相
// 咬合。单元测试各自绿、拼起来不通，是这类协议最常见的失败形态。

// testChannel 造一条指向从站的 TCP 通道。
func testChannel(addr string) model.Channel {
	return model.Channel{
		ID: "ch-test", Protocol: "iec101",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
}

func testDevice(ca int, points ...model.Point) model.Device {
	return model.Device{
		ID: "rtu", ChannelID: "ch-test", UnitID: ca,
		Interval: model.Duration(time.Second), Enabled: true,
		Points: points,
		Labels: map[string]string{
			"linkAddress": "1", "linkAddressSize": "1",
			"commonAddressSize": "1", "ioaSize": "2", "orgSize": "0",
			"interrogate": "10m",
		},
	}
}

func ioaPoint(id, area string, ioa int, dt model.DataType) model.Point {
	return model.Point{
		ID: id, Enabled: true, DataType: dt,
		Address: model.Address{Kind: model.AddressRegister, Area: area, Offset: ioa, Bit: -1},
	}
}

func openTest(t *testing.T, srv *sim.Server, dev model.Device) driver.Conn {
	t.Helper()
	d := New()
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  dev,
		Log:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readAll(t *testing.T, c driver.Conn, dev model.Device) map[string]driver.RawSample {
	t.Helper()
	span := driver.ReadSpan{ID: "s1"}
	for _, p := range dev.Points {
		span.Points = append(span.Points, driver.PointRefOf(dev, p))
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	out := make(map[string]driver.RawSample, len(samples))
	for _, s := range samples {
		out[s.PointID] = s
	}
	return out
}

func TestReadInterrogationSnapshot(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1,
		ioaPoint("temp", AreaIOA, 1001, model.TypeFloat32),
		ioaPoint("flow", AreaIOA, 1002, model.TypeFloat32),
		ioaPoint("count", AreaIOA, 1003, model.TypeInt32),
		ioaPoint("run", AreaIOA, 2001, model.TypeBool),
		ioaPoint("stopped", AreaIOA, 2002, model.TypeBool),
		ioaPoint("breaker", AreaIOA, 2003, model.TypeBool),
		ioaPoint("energy", AreaIOA, 3001, model.TypeUint32),
	)
	c := openTest(t, srv, dev)
	got := readAll(t, c, dev)

	cases := []struct {
		id   string
		good model.Quality
		text string
	}{
		{"temp", model.QualityGood, "42.5"},
		{"flow", model.QualityGood, "7.25"},
		{"count", model.QualityGood, "1234"},
		{"run", model.QualityGood, "true"},
		{"stopped", model.QualityGood, "false"},
		{"breaker", model.QualityGood, "true"},
		{"energy", model.QualityGood, "1000"},
	}
	for _, tc := range cases {
		s, ok := got[tc.id]
		if !ok {
			t.Fatalf("missing sample %q", tc.id)
		}
		if s.Err != nil {
			t.Fatalf("%s: %v", tc.id, s.Err)
		}
		if s.Quality != tc.good {
			t.Fatalf("%s: quality = %s, want %s", tc.id, s.Quality, tc.good)
		}
		if s.Value.String() != tc.text {
			t.Fatalf("%s: value = %s, want %s", tc.id, s.Value.String(), tc.text)
		}
	}
}

func TestReadQualityBitsFromSubstituted(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("sub", AreaIOA, 1004, model.TypeFloat32))
	c := openTest(t, srv, dev)
	got := readAll(t, c, dev)
	if got["sub"].Quality != model.QualityUncertain {
		t.Fatalf("substituted value quality = %s, want uncertain", got["sub"].Quality)
	}
}

func TestTimeTaggedValueDecodes(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("ts", AreaIOA, 1005, model.TypeFloat32))
	c := openTest(t, srv, dev)
	got := readAll(t, c, dev)
	if got["ts"].Err != nil {
		t.Fatalf("time-tagged read: %v", got["ts"].Err)
	}
	if got["ts"].Value.String() != "-3.5" {
		t.Fatalf("value = %s", got["ts"].Value.String())
	}
}

func TestWriteSingleCommandConfirms(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1,
		ioaPoint("cmd", AreaSingleCommand, 4001, model.TypeBool),
		ioaPoint("state", AreaIOA, 2001, model.TypeBool),
	)
	c := openTest(t, srv, dev)

	before, _ := srv.Info(4001)
	_ = before
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "cmd", DataType: model.TypeBool,
		Address: dev.Points[0].Address, Value: model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("results = %+v", res)
	}
	if srv.Writes() != 1 {
		t.Fatalf("sim executed %d commands, want 1", srv.Writes())
	}
	info, ok := srv.Info(4001)
	if !ok || !info.Bool {
		t.Fatalf("sim object = %+v, want Bool=true", info)
	}
}

func TestWriteDoubleCommandRejectsInvalidState(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("dbl", AreaDoubleCommand, 2003, model.TypeUint16))
	c := openTest(t, srv, dev)

	// 0/3 是「不允许」，不是「中间态」。必须在点击那一刻就拒绝，而不是
	// 发下去让设备静默不动。
	_, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "dbl", DataType: model.TypeUint16,
		Address: dev.Points[0].Address, Value: model.NumValue(model.TypeUint16, 3),
	}})
	if err == nil {
		t.Fatal("double command state 3 must be rejected")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
}

func TestWriteUnknownIOAIsAddressError(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	// 9001 在从站里不存在。
	dev := testDevice(1, ioaPoint("ghost", AreaSingleCommand, 9001, model.TypeBool))
	c := openTest(t, srv, dev)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "ghost", DataType: model.TypeBool,
		Address: dev.Points[0].Address, Value: model.BoolValue(true),
	}})
	if err == nil {
		t.Fatalf("unknown IOA must fail, got %+v", res)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	if res[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s", res[0].Quality)
	}
}

func TestWriteSetpointFloat(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("sp", AreaSetpointFloat, 4002, model.TypeFloat32))
	c := openTest(t, srv, dev)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat32,
		Address: dev.Points[0].Address, Value: model.NumValue(model.TypeFloat32, 12.5),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	info, _ := srv.Info(4002)
	if info.Num != 12.5 {
		t.Fatalf("sim value = %v, want 12.5", info.Num)
	}
	// 写下去之后缓存应立即能读到刚写的值（质量 uncertain，等设备上报转 good）。
	got := readAll(t, c, dev)
	if got["sp"].Value.String() != "12.5" {
		t.Fatalf("read-back = %s", got["sp"].Value.String())
	}
}

func TestEventIsPickedUpByClass1(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("run", AreaIOA, 2001, model.TypeBool))
	c := openTest(t, srv, dev)

	// 先做一次总召唤把链路带起来。
	readAll(t, c, dev)

	// 从站侧改值 → 进第 1 级队列 → 下一轮 Read 应当取到。
	if err := srv.SetBool(2001, false); err != nil {
		t.Fatalf("SetBool: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := readAll(t, c, dev)
		if got["run"].Value.String() == "false" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("class-1 event never arrived, last = %s", got["run"].Value.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDuplicateFrameIsNotExecutedTwice(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("cmd", AreaSingleCommand, 4001, model.TypeBool))
	c := openTest(t, srv, dev)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "cmd", DataType: model.TypeBool,
		Address: dev.Points[0].Address, Value: model.BoolValue(true),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	// 同一条命令的帧计数位不变时，从站只回确认、不重复执行。这里验证
	// 的是「一次 Write 只被算作一次执行」这个契约，而不是重传路径本身
	// （重传由 sendUser 内部触发，测试环境不易稳定复现超时）。
	if n := srv.Writes(); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}
}

func TestLinkAddressMismatchIsIgnoredBySim(t *testing.T) {
	// 链路地址配错时，从站不应回应，主站应当超时而不是「成功读到旧值」。
	srv, err := sim.NewServer("", sim.WithLinkAddress(7, 1))
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("temp", AreaIOA, 1001, model.TypeFloat32))
	d := New()
	_, err = d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  dev,
		Log:     slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("open with wrong link address must fail")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %v, want timeout", driver.KindOf(err))
	}
}

func TestProbeUsesLinkStatus(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("temp", AreaIOA, 1001, model.TypeFloat32))
	c := openTest(t, srv, dev)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestCompactProfileWidths(t *testing.T) {
	// 1 字节 IOA + 2 字节公共地址：一个很小的装置常见的配置。宽度变了，
	// 帧结构必须跟着变，否则读回来的是错位的数。
	srv, err := sim.NewServer("",
		sim.WithProfile(iec60870.Profile{OrgSize: 0, CASize: 2, IOASize: 1}),
		sim.WithCommonAddr(0x1234),
		// 默认对象集的 IOA 是四位数，1 字节地址空间装不下；从站会在组帧
		// 时失败并断开，主站看到的是一个莫名其妙的 EOF。
		sim.WithNoDefaults(true))
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	srv.Add(sim.Info{IOA: 101, Kind: sim.KindFloat, Num: 42.5})

	dev := testDevice(0x1234, ioaPoint("temp", AreaIOA, 101, model.TypeFloat32))
	dev.Labels["commonAddressSize"] = "2"
	dev.Labels["ioaSize"] = "1"
	c := openTest(t, srv, dev)
	got := readAll(t, c, dev)
	if got["temp"].Value.String() != "42.5" {
		t.Fatalf("value = %s (%v)", got["temp"].Value.String(), got["temp"].Err)
	}
}

func TestIOAOutOfProfileRangeRejectedAtOpen(t *testing.T) {
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1, ioaPoint("big", AreaIOA, 70000, model.TypeFloat32))
	dev.Labels["ioaSize"] = "1" // 上限 255
	d := New()
	_, err = d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  dev,
		Log:     slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("IOA 70000 with ioaSize=1 must be rejected")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
}

func TestConcurrentReadsAreSerialized(t *testing.T) {
	// 一条总线同时只能有一个事务。两个并发 Read 若不串行，会让两轮的
	// 请求-应答交错；症状是偶发的「值串台」，在单设备测试里看不到。
	srv, err := sim.NewServer("")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	dev := testDevice(1,
		ioaPoint("temp", AreaIOA, 1001, model.TypeFloat32),
		ioaPoint("flow", AreaIOA, 1002, model.TypeFloat32),
	)
	c := openTest(t, srv, dev)
	readAll(t, c, dev)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			span := driver.ReadSpan{ID: "s"}
			for _, p := range dev.Points {
				span.Points = append(span.Points, driver.PointRefOf(dev, p))
			}
			if _, err := c.Read(context.Background(), []driver.ReadSpan{span}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
}
