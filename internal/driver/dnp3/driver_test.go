package dnp3

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	dnp3codec "uic/internal/codec/dnp3"
	"uic/internal/driver"
	"uic/internal/model"
	sim "uic/internal/sim/dnp3"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

type harness struct {
	t   *testing.T
	srv *sim.Server
	drv *Driver
	cfg driver.OpenConfig
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv, err := sim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	// 默认点表每秒自变，会让「读到的静态值」不可断言；需要事件的测试
	// 自己调 SetValue，不要依赖时钟。
	srv.SetAutoChange(false)
	t.Cleanup(func() { _ = srv.Close() })

	ch := model.Channel{
		ID: "ch-dnp3", Protocol: "dnp3",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(800 * time.Millisecond),
	}
	dev := model.Device{
		ID: "rtu-dnp3", ChannelID: ch.ID, UnitID: 1, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(800 * time.Millisecond),
		Labels: map[string]string{
			"masterAddress": "1024",
			"eventPoll":     "150ms",
			"integrityPoll": "2s",
		},
		Points: []model.Point{
			dp("bi0", AreaBinaryInput, 0, model.TypeBool, false),
			dp("bi1", AreaBinaryInput, 1, model.TypeBool, false),
			dp("dbi0", AreaDoubleBitInput, 0, model.TypeBool, false),
			dp("dbi1", AreaDoubleBitInput, 1, model.TypeBool, false),
			dp("bo0", AreaBinaryOutput, 0, model.TypeBool, true),
			dp("bo1", AreaBinaryOutput, 1, model.TypeBool, true),
			dp("bo9", AreaBinaryOutput, 9, model.TypeBool, true), // 从站没有这个索引
			dp("ctr0", AreaCounter, 0, model.TypeUint32, false),
			dp("ctr1", AreaCounter, 1, model.TypeUint32, false),
			dp("ai0", AreaAnalogInput, 0, model.TypeFloat64, false),
			dp("ai1", AreaAnalogInput, 1, model.TypeFloat32, false),
			dp("ai2", AreaAnalogInput, 2, model.TypeFloat64, false),
			dp("ao0", AreaAnalogOutput, 0, model.TypeFloat64, true),
		},
	}
	return &harness{t: t, srv: srv, drv: New(),
		cfg: driver.OpenConfig{Channel: ch, Device: dev}}
}

func dp(id, area string, offset int, dt model.DataType, writable bool) model.Point {
	return model.Point{
		ID: id, Enabled: true, Writable: writable, DataType: dt,
		Address: model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Bit: -1},
	}
}

func (h *harness) open() *conn {
	h.t.Helper()
	c, err := h.drv.Open(context.Background(), h.cfg)
	if err != nil {
		h.t.Fatalf("open: %v", err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

func (h *harness) spans() []driver.ReadSpan {
	var pts []driver.PointRef
	for _, p := range h.cfg.Device.Points {
		pts = append(pts, driver.PointRefOf(h.cfg.Device, p))
	}
	return []driver.ReadSpan{{ID: "all", Points: pts}}
}

func (h *harness) readAll(c *conn) map[string]driver.RawSample {
	h.t.Helper()
	out, err := c.Read(context.Background(), h.spans())
	if err != nil {
		h.t.Fatalf("read: %v", err)
	}
	m := make(map[string]driver.RawSample, len(out))
	for _, s := range out {
		m[s.PointID] = s
	}
	return m
}

// waitFor 轮询直到条件成立。用轮询而不是固定 sleep：事件轮询的周期在
// 测试里被调到 150ms，固定 sleep 要么慢要么偶发失败。
func (h *harness) waitFor(c *conn, what string, cond func(map[string]driver.RawSample) bool) map[string]driver.RawSample {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got map[string]driver.RawSample
	for time.Now().Before(deadline) {
		got = h.readAll(c)
		if cond(got) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timeout waiting for %s; last = %+v", what, got)
	return nil
}

func num(t *testing.T, s driver.RawSample) float64 {
	t.Helper()
	if s.Value.Type == model.TypeBool {
		if s.Value.Bool {
			return 1
		}
		return 0
	}
	return s.Value.Num
}

// ---------------------------------------------------------------------------
// 读路径
// ---------------------------------------------------------------------------

func TestOpenAndReadStatic(t *testing.T) {
	h := newHarness(t)
	c := h.open()
	got := h.readAll(c)

	cases := []struct {
		id     string
		want   float64
		qual   model.Quality
		isBool bool
	}{
		{"bi0", 1, model.QualityGood, true},
		{"bi1", 0, model.QualityGood, true},
		// 双点 1 = 分、2 = 合；两者都是确定状态。
		{"dbi0", 0, model.QualityGood, true},
		{"dbi1", 1, model.QualityGood, true},
		{"bo0", 0, model.QualityGood, true},
		{"ctr0", 1000, model.QualityGood, false},
		{"ctr1", 42, model.QualityGood, false},
		{"ai0", 42.5, model.QualityGood, false},
		{"ai1", 7.25, model.QualityGood, false},
		{"ai2", -3.5, model.QualityGood, false},
		{"ao0", 0, model.QualityGood, false},
	}
	for _, tc := range cases {
		s, ok := got[tc.id]
		if !ok {
			t.Fatalf("%s missing", tc.id)
		}
		if s.Quality != tc.qual {
			t.Fatalf("%s quality = %s, want %s (err=%v)", tc.id, s.Quality, tc.qual, s.Err)
		}
		if tc.isBool {
			if s.Value.Type != model.TypeBool || s.Value.Bool != (tc.want == 1) {
				t.Fatalf("%s = %+v, want bool %v", tc.id, s.Value, tc.want == 1)
			}
			continue
		}
		if v := num(t, s); v != tc.want {
			t.Fatalf("%s = %v, want %v", tc.id, v, tc.want)
		}
	}

	// 诊断视图：从站实际送过的对象要能在不依赖点表的情况下列出来。
	obs := c.Observed()
	if len(obs) == 0 {
		t.Fatal("Observed is empty")
	}
	var sawBI, sawAI bool
	for _, o := range obs {
		if o.Area == AreaBinaryInput && o.Index == 0 {
			sawBI = true
		}
		if o.Area == AreaAnalogInput && o.Index == 0 {
			sawAI = true
		}
	}
	if !sawBI || !sawAI {
		t.Fatalf("Observed missing binary/analog input: %+v", obs)
	}
}

// 双点量的中间状态（0/3）不能当成 false good。这是「阀门未知显示成已关闭」
// 那类事故的源头。
func TestDoubleBitMiddleStateIsUncertain(t *testing.T) {
	h := newHarness(t)
	c := h.open()
	if err := h.srv.SetValue(sim.DoubleBitInput, 0, uint8(0)); err != nil {
		t.Fatal(err)
	}
	got := h.waitFor(c, "double-bit middle state", func(m map[string]driver.RawSample) bool {
		return m["dbi0"].Quality == model.QualityUncertain
	})
	if got["dbi0"].Value.Type != model.TypeBool || got["dbi0"].Value.Bool {
		t.Fatalf("middle state = %+v, want bool false", got["dbi0"].Value)
	}
}

func TestEventPollUpdatesValueAndConfirms(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	if err := h.srv.SetValue(sim.BinaryInput, 0, false); err != nil {
		t.Fatal(err)
	}
	got := h.waitFor(c, "binary input event", func(m map[string]driver.RawSample) bool {
		return m["bi0"].Value.Type == model.TypeBool && !m["bi0"].Value.Bool
	})
	if got["bi0"].Quality != model.QualityGood {
		t.Fatalf("quality = %s", got["bi0"].Quality)
	}
	// 主站必须回应用层确认；否则从站会把 inflight 一直留着重发。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.srv.Inflight() == 0 && h.srv.Stats().Confirms > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("event never confirmed: inflight=%d stats=%+v", h.srv.Inflight(), h.srv.Stats())
}

func TestQualityMapping(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	// ONLINE=0 -> bad。
	if err := h.srv.SetFlags(sim.AnalogInput, 1, 0); err != nil {
		t.Fatal(err)
	}
	h.waitFor(c, "COMM_LOST/offline", func(m map[string]driver.RawSample) bool {
		return m["ai1"].Quality == model.QualityBad
	})

	// RESTART -> uncertain。值仍然可用，但不能当正常值参与联锁。
	if err := h.srv.SetFlags(sim.AnalogInput, 1, dnp3codec.FlagOnline|dnp3codec.FlagRestart); err != nil {
		t.Fatal(err)
	}
	h.waitFor(c, "restart", func(m map[string]driver.RawSample) bool {
		return m["ai1"].Quality == model.QualityUncertain
	})

	// 恢复 -> good。
	if err := h.srv.SetFlags(sim.AnalogInput, 1, dnp3codec.FlagOnline); err != nil {
		t.Fatal(err)
	}
	h.waitFor(c, "online", func(m map[string]driver.RawSample) bool {
		return m["ai1"].Quality == model.QualityGood
	})
}

// ---------------------------------------------------------------------------
// 写路径
// ---------------------------------------------------------------------------

func TestWriteBinaryOutput(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "bo0", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaBinaryOutput, Offset: 0, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	p, _ := h.srv.Value(sim.BinaryOutput, 0)
	if !p.Bool {
		t.Fatal("outstation binary output not set")
	}

	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "bo0", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaBinaryOutput, Offset: 0, Bit: -1},
		Value:   model.BoolValue(false),
	}}); err != nil {
		t.Fatalf("write off: %v", err)
	}
	p, _ = h.srv.Value(sim.BinaryOutput, 0)
	if p.Bool {
		t.Fatal("outstation binary output not cleared")
	}
}

func TestWriteAnalogOutput(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "ao0", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaAnalogOutput, Offset: 0, Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 12.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	p, _ := h.srv.Value(sim.AnalogOutput, 0)
	if p.Num != 12.5 {
		t.Fatalf("outstation analog output = %v, want 12.5", p.Num)
	}
}

// 从站回显非零 CROB status 时，主站必须报错而不是报成功。
func TestWriteUnknownOutputRejected(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "bo9", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaBinaryOutput, Offset: 9, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err == nil {
		t.Fatal("write to unknown index succeeded")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %s, want protocol", driver.KindOf(err))
	}
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteRejectsReadOnlyArea(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "ai0", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaAnalogInput, Offset: 0, Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 1),
	}})
	if err == nil {
		t.Fatal("write to analog input succeeded")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("kind = %s, want unsupported", driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 链路与配置
// ---------------------------------------------------------------------------

func TestProbe(t *testing.T) {
	h := newHarness(t)
	c := h.open()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestCloseThenRead(t *testing.T) {
	h := newHarness(t)
	c := h.open()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(context.Background(), h.spans()); err == nil {
		t.Fatal("read after close succeeded")
	} else if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %s, want disconnected", driver.KindOf(err))
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	mutate := func(f func(*driver.OpenConfig)) driver.OpenConfig {
		h := newHarness(t)
		cfg := h.cfg
		cfg.Device.Labels = map[string]string{}
		for k, v := range h.cfg.Device.Labels {
			cfg.Device.Labels[k] = v
		}
		f(&cfg)
		return cfg
	}
	cases := []struct {
		name   string
		cfg    driver.OpenConfig
		expect string
	}{
		{"unitId out of range", mutate(func(c *driver.OpenConfig) { c.Device.UnitID = 70000 }), "1..65519"},
		{"masterAddress out of range", mutate(func(c *driver.OpenConfig) {
			c.Device.Labels["masterAddress"] = "0"
		}), "1..65519"},
		{"same link addresses", mutate(func(c *driver.OpenConfig) {
			c.Device.UnitID = 1024
		}), "distinct"},
		{"bad event poll", mutate(func(c *driver.OpenConfig) { c.Device.Labels["eventPoll"] = "soon" }), "eventPoll"},
		{"event poll too small", mutate(func(c *driver.OpenConfig) { c.Device.Labels["eventPoll"] = "10ms" }), ">= 100ms"},
		{"integrity smaller than event", mutate(func(c *driver.OpenConfig) {
			c.Device.Labels["eventPoll"] = "2s"
			c.Device.Labels["integrityPoll"] = "1s"
		}), ">= eventPoll"},
		{"wrong transport", mutate(func(c *driver.OpenConfig) {
			c.Channel.Transport.Kind = "serial"
		}), "requires tcp"},
		{"unknown area", mutate(func(c *driver.OpenConfig) {
			c.Device.Points[0].Address.Area = "coil"
		}), "unknown area"},
		{"bad datatype", mutate(func(c *driver.OpenConfig) {
			c.Device.Points[0].DataType = model.TypeString
		}), "binary objects need"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Open(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("open succeeded")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("error = %v, want substring %q", err, tc.expect)
			}
			kind := driver.KindOf(err)
			if kind != driver.ErrUnsupported && kind != driver.ErrAddress {
				t.Fatalf("kind = %s", kind)
			}
		})
	}
}

// 链路复位没有回应时，Open 必须在有限时间内失败，而不是挂死。
func TestOpenTimesOutWithoutLinkAck(t *testing.T) {
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
			// 收下字节但不回任何帧。
			go func() {
				buf := make([]byte, 256)
				for {
					if _, err := c.Read(buf); err != nil {
						_ = c.Close()
						return
					}
				}
			}()
		}
	}()

	dev := model.Device{
		ID: "rtu-silent", ChannelID: "ch", UnitID: 1, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(200 * time.Millisecond),
		Points:   []model.Point{dp("bi", AreaBinaryInput, 0, model.TypeBool, false)},
	}
	ch := model.Channel{ID: "ch", Protocol: "dnp3",
		Transport: model.Transport{Kind: "tcp", Address: ln.Addr().String()}}

	start := time.Now()
	_, err = New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err == nil {
		t.Fatal("open succeeded against a silent peer")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("open took %s", d)
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %s, want timeout (%v)", driver.KindOf(err), err)
	}
}

// 同一个 Conn 不会被并发调用是契约；但后台轮询与 Read 是并发的，这个
// 用例用 -race 跑一遍，确认值表与链路状态没有数据竞争。
func TestConcurrentReadAndPolling(t *testing.T) {
	h := newHarness(t)
	c := h.open()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := c.Read(context.Background(), h.spans()); err != nil && !errors.Is(err, context.Canceled) {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if err := h.srv.SetValue(sim.Counter, 0, uint32(1000+i)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	wg.Wait()
}
