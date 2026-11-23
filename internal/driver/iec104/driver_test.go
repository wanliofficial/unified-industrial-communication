package iec104

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/codec/iec104"
	"uic/internal/driver"
	"uic/internal/model"
	sim "uic/internal/sim/iec104"
)

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

type h struct {
	t   *testing.T
	srv *sim.Server
	drv *Driver
	cfg driver.OpenConfig
}

func newH(t *testing.T) *h {
	t.Helper()
	srv, err := sim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	srv.SetAutoIncrement(false)
	ch := model.Channel{
		ID: "ch-104", Protocol: "iec104",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(800 * time.Millisecond),
	}
	dev := model.Device{
		ID: "rtu01", ChannelID: ch.ID, UnitID: 1,
		Interval: model.Duration(time.Second), Enabled: true,
		Points: []model.Point{
			pt("temp", AreaIOA, 1001, model.TypeFloat32, false),
			pt("flow", AreaIOA, 1002, model.TypeFloat32, false),
			pt("scaled", AreaIOA, 1003, model.TypeInt16, false),
			pt("subst", AreaIOA, 1004, model.TypeFloat32, false),
			pt("ts", AreaIOA, 1005, model.TypeFloat32, false),
			pt("run", AreaIOA, 2001, model.TypeBool, false),
			pt("stopped", AreaIOA, 2002, model.TypeBool, false),
			pt("breaker", AreaIOA, 2003, model.TypeBool, false),
			pt("counter", AreaIOA, 3001, model.TypeUint32, false),
			pt("cmd", AreaSingleCommand, 4001, model.TypeBool, true),
			pt("sp", AreaSetpointFloat, 4002, model.TypeFloat32, true),
			pt("sps", AreaSetpointScaled, 4003, model.TypeInt16, true),
			pt("step", AreaStepCommand, 4004, model.TypeUint16, true),
		},
	}
	hh := &h{t: t, srv: srv, drv: New(),
		cfg: driver.OpenConfig{Channel: ch, Device: dev}}
	t.Cleanup(func() { _ = srv.Close() })
	return hh
}

func pt(id, area string, ioa int, dt model.DataType, writable bool) model.Point {
	return model.Point{
		ID: id, Enabled: true, Writable: writable, DataType: dt,
		Address: model.Address{Kind: model.AddressRegister, Area: area, Offset: ioa, Bit: -1},
	}
}

func (hh *h) open() *conn {
	hh.t.Helper()
	c, err := hh.drv.Open(context.Background(), hh.cfg)
	if err != nil {
		hh.t.Fatalf("open: %v", err)
	}
	hh.t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// waitRead 轮询直到指定点位拿到可用质量，或超时。
//
// 不能把 Read 的错误当成硬失败：按 §3.3.1，只要这一轮一个点位都没成功，
// Read 就必须上抛错误（否则熔断器看不到异常）。启动初期「只配了点但还没
// 收到总召唤快照」正是这种情形，它是正常的过渡态，不是故障。
func (hh *h) waitRead(c *conn, want ...string) map[string]driver.RawSample {
	hh.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []driver.RawSample
	for time.Now().Before(deadline) {
		out, _ := c.Read(context.Background(), c.plan())
		last = out
		seen := map[string]bool{}
		for _, s := range out {
			if s.Quality.IsUsable() {
				seen[s.PointID] = true
			}
		}
		ok := true
		for _, id := range want {
			if !seen[id] {
				ok = false
				break
			}
		}
		if ok {
			m := map[string]driver.RawSample{}
			for _, s := range out {
				m[s.PointID] = s
			}
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	hh.t.Fatalf("points never became usable: %+v", last)
	return nil
}

// planPoints 返回 plan() 里全部点位的引用，用于校验样本数量。
func (c *conn) planPoints() []driver.PointRef {
	var out []driver.PointRef
	for _, span := range c.plan() {
		out = append(out, span.Points...)
	}
	return out
}

// plan 把设备点位按类别分组，模拟 planner 的输出。
func (c *conn) plan() []driver.ReadSpan {
	groups := map[string][]driver.PointRef{}
	order := []string{}
	for _, p := range c.points {
		key := p.area
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p.ref)
	}
	out := make([]driver.ReadSpan, 0, len(order))
	for _, k := range order {
		out = append(out, driver.ReadSpan{ID: k, Points: groups[k]})
	}
	return out
}

// ---------------------------------------------------------------------------
// 元数据
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "iec104" || m.Version == "" || m.SDKVersion != driver.SDKVersion {
		t.Fatalf("meta = %+v", m)
	}
	if !m.Accepts(model.AddressRegister) || m.Accepts(model.AddressObject) {
		t.Fatal("address kinds wrong")
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported ||
		m.Capabilities.Subscribe != driver.CapSupported || m.Capabilities.BitAccess != driver.CapUnsupported {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
	if len(m.ConfigFields) == 0 || len(m.PointFields) == 0 {
		t.Fatal("config fields missing")
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func TestReadAfterInterrogation(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	got := hh.waitRead(c, "temp", "flow", "scaled", "run", "stopped", "breaker", "counter")

	if v := got["temp"].Value; v.Type != model.TypeFloat32 || v.Num != 42.5 {
		t.Fatalf("temp = %+v", v)
	}
	if v := got["scaled"].Value; v.Num != 1234 {
		t.Fatalf("scaled = %+v", v)
	}
	if v := got["run"].Value; !v.Bool || v.Type != model.TypeBool {
		t.Fatalf("run = %+v", v)
	}
	if v := got["stopped"].Value; v.Bool {
		t.Fatalf("stopped = %+v", v)
	}
	// 双点 2 -> 合 -> true
	if v := got["breaker"].Value; !v.Bool {
		t.Fatalf("breaker = %+v", v)
	}
	if v := got["counter"].Value; v.Num != 1000 {
		t.Fatalf("counter = %+v", v)
	}
}

// 品质位必须传播：SB（取代）→ uncertain。
func TestReadQualityPropagates(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	got := hh.waitRead(c, "subst")
	if q := got["subst"].Quality; q != model.QualityUncertain {
		t.Fatalf("subst quality = %s", q)
	}
}

// 带时标的测量值仍然按数值读出来，时标不参与取值。
func TestReadTimeTaggedValue(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	got := hh.waitRead(c, "ts")
	if v := got["ts"].Value; v.Num != -3.5 {
		t.Fatalf("ts = %+v", v)
	}
}

// 未配置的点位必须报地址错，而不是空值。单点全失败要上抛（§3.3.1）。
func TestReadUnknownPoint(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	out, err := c.Read(context.Background(), []driver.ReadSpan{{Points: []driver.PointRef{{ID: "ghost"}}}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v / %v", err, driver.KindOf(err))
	}
	if len(out) != 1 || out[0].Quality != model.QualityAddressError {
		t.Fatalf("out = %+v", out)
	}
	if driver.KindOf(out[0].Err) != driver.ErrAddress {
		t.Fatalf("kind = %v", driver.KindOf(out[0].Err))
	}
}

// 从站不上送的点位：值是 bad 但 Read 不返回整体错误——「没收到数据」与
// 「链路断了」是两件事。
func TestReadMissingObjectIsPerPointError(t *testing.T) {
	hh := newH(t)
	hh.cfg.Device.Points = append(hh.cfg.Device.Points,
		pt("absent", AreaIOA, 9999, model.TypeFloat32, false))
	c := hh.open()
	hh.waitRead(c, "temp")
	out, err := c.Read(context.Background(), c.plan())
	if err != nil {
		t.Fatalf("read must not fail on a silent object: %v", err)
	}
	var found bool
	for _, s := range out {
		if s.PointID == "absent" {
			found = true
			if s.Quality != model.QualityBad {
				t.Fatalf("absent quality = %s", s.Quality)
			}
		}
	}
	if !found {
		t.Fatal("absent point missing from result")
	}
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func TestWriteSingleCommand(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "cmd", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 4001, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("res = %+v", res)
	}
	// 激活确认（原因 7）只说明设备「接受了命令」，实际改写发生在它之后：
	// sim 按标准顺序先回确认、再落值、最后回激活终止。这里必须轮询，否则
	// 测试会随「确认与落值谁先到达」抖动。
	waitFor(t, "sim to apply the command", func() bool {
		info, ok := hh.srv.Value(4001)
		return ok && info.Bool
	})
}

func TestWriteSetpointAndReadBack(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointFloat, Offset: 4002, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 55.5),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write = %+v %v", res, err)
	}
	// 驱动用请求值填充了缓存，因此立刻读回就是新值。
	out, _ := c.Read(context.Background(), c.plan())
	for _, s := range out {
		if s.PointID == "sp" {
			if s.Value.Num != 55.5 {
				t.Fatalf("sp = %+v", s.Value)
			}
			if s.Quality != model.QualityUncertain {
				t.Fatalf("sp quality = %s（写入后应由突发上送转 good，这里先看到 uncertain）", s.Quality)
			}
		}
	}
	info, _ := hh.srv.Value(4002)
	if info.Num != 55.5 {
		waitFor(t, "sim to apply the float setpoint", func() bool {
			i, ok := hh.srv.Value(4002)
			return ok && i.Num == 55.5
		})
	}
}

func TestWriteScaledSetpoint(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sps", DataType: model.TypeInt16,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointScaled, Offset: 4003, Bit: -1},
		Value:   model.NumValue(model.TypeInt16, 777),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write = %+v %v", res, err)
	}
	waitFor(t, "sim to apply the scaled setpoint", func() bool {
		info, ok := hh.srv.Value(4003)
		return ok && info.Num == 777
	})
}

// TestWriteStepCommand 调节步命令：bool true→升档(2)、false→降档(1)，
// sim 把方向记进 Num。这是步位置监视（M_ST_NA_1）的控制对偶——能看档位，
// 也能下升/降命令。
func TestWriteStepCommand(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "step", DataType: model.TypeUint16,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaStepCommand, Offset: 4004, Bit: -1},
		Value:   model.BoolValue(true), // true → 升档
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write step higher = %+v %v", res, err)
	}
	waitFor(t, "sim to record the higher step", func() bool {
		info, ok := hh.srv.Value(4004)
		return ok && info.Num == 2
	})

	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "step", DataType: model.TypeUint16,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaStepCommand, Offset: 4004, Bit: -1},
		Value:   model.BoolValue(false), // false → 降档
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write step lower = %+v %v", res, err)
	}
	waitFor(t, "sim to record the lower step", func() bool {
		info, ok := hh.srv.Value(4004)
		return ok && info.Num == 1
	})
}

// 只读对象写入必须得到地址错（设备回「未知 IOA」），而不是笼统的失败。
func TestWriteReadOnlyIsAddressError(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointFloat, Offset: 1001, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil {
		t.Fatal("write to read-only object must fail")
	}
	if res[0].OK || res[0].Quality != model.QualityAddressError {
		t.Fatalf("res = %+v", res)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
}

// 值类型不符在本地就拒绝，不发报文。
func TestWriteTypeMismatchRejectedLocally(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "cmd", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 4001, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if !strings.Contains(err.Error(), "single command requires bool") {
		t.Fatalf("err = %v", err)
	}
	info, _ := hh.srv.Value(4001)
	if info.Bool {
		t.Fatal("sim must not have been touched")
	}
}

// 归一化设定值超范围必须在本地拒绝。
func TestWriteNormalizedOutOfRange(t *testing.T) {
	hh := newH(t)
	hh.cfg.Device.Points = append(hh.cfg.Device.Points,
		pt("spn", AreaSetpointNormalized, 4002, model.TypeFloat32, true))
	c := hh.open()
	hh.waitRead(c, "temp")
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "spn", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointNormalized, Offset: 4002, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 2),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

// 未声明命令类型（area=ioa）的可写点：按 dataType 推导命令类型。
func TestWriteAreaIOADerivesCommand(t *testing.T) {
	hh := newH(t)
	hh.cfg.Device.Points = append(hh.cfg.Device.Points,
		pt("auto", AreaIOA, 4001, model.TypeBool, true))
	c := hh.open()
	hh.waitRead(c, "temp")
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "auto", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaIOA, Offset: 4001, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write = %+v %v", res, err)
	}
	waitFor(t, "sim to apply the derived command", func() bool {
		info, ok := hh.srv.Value(4001)
		return ok && info.Bool
	})
}

// ---------------------------------------------------------------------------
// 配置校验
// ---------------------------------------------------------------------------

func TestOpenRejectsBadConfig(t *testing.T) {
	hh := newH(t)
	cases := map[string]func(*driver.OpenConfig){
		"not tcp":    func(c *driver.OpenConfig) { c.Channel.Transport.Kind = "udp" },
		"no address": func(c *driver.OpenConfig) { c.Channel.Transport.Address = "" },
		"ca too big": func(c *driver.OpenConfig) { c.Device.UnitID = 70000 },
		"no points":  func(c *driver.OpenConfig) { c.Device.Points = nil },
		"bad kind": func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{{
				ID: "x", Enabled: true, DataType: model.TypeFloat32,
				Address: model.Address{Kind: model.AddressSymbol, Path: "x"},
			}}
		},
		"bad area": func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{pt("x", "registers", 1, model.TypeFloat32, false)}
		},
		"ioa zero": func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{pt("x", AreaIOA, 0, model.TypeFloat32, false)}
		},
		"ioa too big": func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{pt("x", AreaIOA, 0x1000000, model.TypeFloat32, false)}
		},
		"length set": func(c *driver.OpenConfig) {
			p := pt("x", AreaIOA, 1, model.TypeFloat32, false)
			p.Address.Length = 2
			c.Device.Points = []model.Point{p}
		},
		"bad dataType": func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{pt("x", AreaIOA, 1, model.TypeString, false)}
		},
	}
	for name, mut := range cases {
		cfg := hh.cfg
		mut(&cfg)
		if _, err := hh.drv.Open(context.Background(), cfg); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

// ca=0 应该被当作默认值 1，而不是错误。
func TestOpenDefaultCA(t *testing.T) {
	hh := newH(t)
	hh.cfg.Device.UnitID = 0
	c, err := hh.drv.Open(context.Background(), hh.cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	if c.(*conn).ca != 1 {
		t.Fatalf("ca = %d", c.(*conn).ca)
	}
}

// 省略端口时自动补 2404：拨号地址必须是 host:2404（端口不可达，Open 失败
// 也算通过——我们要验的是地址归一化，不是连通性）。
func TestOpenDefaultPort(t *testing.T) {
	var dialed string
	drv := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (link, error) {
		dialed = addr
		return nil, io.EOF
	})
	hh := newH(t)
	hh.cfg.Channel.Transport.Address = "127.0.0.1"
	if _, err := drv.Open(context.Background(), hh.cfg); err == nil {
		t.Fatal("expected dial failure")
	}
	if dialed != "127.0.0.1:2404" {
		t.Fatalf("dialed = %q", dialed)
	}
}

// ---------------------------------------------------------------------------
// 链路故障
// ---------------------------------------------------------------------------

// 没有 STARTDT con 时 Open 必须超时失败，而不是挂住。
func TestOpenTimesOutWithoutStartCon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		<-time.After(2 * time.Second)
	}()
	hh := newH(t)
	hh.cfg.Channel.Transport.Address = ln.Addr().String()
	hh.cfg.Channel.Timeout = model.Duration(150 * time.Millisecond)
	start := time.Now()
	if _, err := hh.drv.Open(context.Background(), hh.cfg); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("open took too long: %v", time.Since(start))
	}
}

// 拨号失败必须是 disconnected（可重试）。
func TestOpenDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	hh := newH(t)
	hh.cfg.Channel.Transport.Address = addr
	_, err = hh.drv.Open(context.Background(), hh.cfg)
	if err == nil {
		t.Fatal("expected dial error")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

// 链路断开后 Read 返回整体错误，让上层把点位标成 comm_error。
func TestReadReportsBrokenLink(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp", "flow", "scaled", "subst", "ts", "run", "stopped", "breaker", "counter")
	_ = hh.srv.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.Read(context.Background(), c.plan())
		if err != nil {
			if driver.KindOf(err) != driver.ErrDisconnected {
				t.Fatalf("kind = %v", driver.KindOf(err))
			}
			// 链路级失败必须给每个点位一条坏样本，不能只回一个 error。
			if len(out) != len(c.planPoints()) {
				t.Fatalf("samples = %d, want %d", len(out), len(c.planPoints()))
			}
			for _, s := range out {
				if s.Quality != model.QualityCommError || s.Err == nil {
					t.Fatalf("sample %s = %+v", s.PointID, s)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("read never noticed the broken link")
}

// Probe 用 TESTFR 往返：链路正常时必须成功。
func TestProbe(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// Probe 在对端静默时必须超时，而不是永远等下去。
func TestProbeTimesOutWhenPeerSilent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		<-time.After(2 * time.Second)
		c.Close()
	}()
	hh := newH(t)
	hh.cfg.Channel.Transport.Address = ln.Addr().String()
	hh.cfg.Channel.Timeout = model.Duration(120 * time.Millisecond)
	_, err = hh.drv.Open(context.Background(), hh.cfg)
	if err == nil {
		t.Fatal("open must fail without STARTDT con")
	}
}

// Close 必须幂等。
func TestCloseIsIdempotent(t *testing.T) {
	hh := newH(t)
	c, err := hh.drv.Open(context.Background(), hh.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := c.Read(context.Background(), c.(*conn).plan()); err == nil {
		t.Fatal("read after close must fail")
	}
	if err := c.Probe(context.Background()); err == nil {
		t.Fatal("probe after close must fail")
	}
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "cmd", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 4001, Bit: -1},
		Value:   model.BoolValue(true),
	}}); err == nil {
		t.Fatal("write after close must fail")
	}
}

// ctx 已取消时 Read 立刻返回超时。
func TestReadHonoursCancelledContext(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Read(ctx, c.plan()); err == nil {
		t.Fatal("expected error")
	} else if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 序号与帧校验（注入假链路）
// ---------------------------------------------------------------------------

// fakeLink 允许测试精确控制从对端读到的字节。
type fakeLink struct {
	mu      sync.Mutex
	read    chan []byte
	written [][]byte
	closed  chan struct{}
	once    sync.Once
}

func newFakeLink() *fakeLink {
	return &fakeLink{read: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakeLink) Read(p []byte) (int, error) {
	select {
	case b, ok := <-f.read:
		if !ok {
			return 0, io.EOF
		}
		// 一次只喂一段，模拟 TCP 分片。
		n := copy(p, b)
		if n < len(b) {
			f.read <- b[n:]
		}
		return n, nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeLink) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.written = append(f.written, append([]byte(nil), p...))
	f.mu.Unlock()
	return len(p), nil
}

func (f *fakeLink) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeLink) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeLink) SetWriteDeadline(time.Time) error { return nil }

func (f *fakeLink) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.written) == 0 {
		return nil
	}
	return f.written[len(f.written)-1]
}

func (f *fakeLink) all() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.written...)
}

func (f *fakeLink) feed(b []byte) { f.read <- append([]byte(nil), b...) }

// buildFloatASDU 构造一条突发上送的短浮点测量值。
func buildFloatASDU(ioa uint32, v float64) ([]byte, error) {
	return iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: ioa, Num: v}},
	})
}

// 迟到/乱序的 I 帧必须被判为协议错，而不是把值错位地写进缓存。
func TestOutOfOrderIFrameFailsLink(t *testing.T) {
	asdu, _ := buildFloatASDU(1001, 1.5)
	f := newFakeLink()
	drv := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (link, error) {
		return f, nil
	})
	_ = drv
	// 先手工建一个 conn，绕开 Open 的 STARTDT 等待。
	c := &conn{
		link: f, deviceID: "x", ca: 1, timeout: 100 * time.Millisecond,
		points: map[string]*pointPlan{
			"t": {ref: driver.PointRef{ID: "t", DataType: model.TypeFloat32},
				ioa: 1001, area: AreaIOA, dataType: model.TypeFloat32},
		},
		byIOA: map[uint32][]*pointPlan{
			1001: {{ref: driver.PointRef{ID: "t", DataType: model.TypeFloat32},
				ioa: 1001, area: AreaIOA, dataType: model.TypeFloat32}},
		},
		values:   map[string]cachedValue{},
		done:     make(chan struct{}),
		ackCh:    make(chan struct{}, 1),
		started:  make(chan struct{}),
		pending:  map[uint32]chan iec104.ASDU{},
		testIdle: time.Hour,
	}
	c.lastRecv.Store(time.Now().UnixNano())
	go c.readLoop()

	// N(S)=5，但期望 0。
	bad, err := iec104.BuildIFrame(5, 0, asdu)
	if err != nil {
		t.Fatal(err)
	}
	f.feed(bad)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		err := c.linkErr
		c.mu.Unlock()
		if err != nil {
			if !strings.Contains(err.Error(), "out of order") {
				t.Fatalf("link error = %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("out-of-order frame was not detected")
}

// 垃圾帧（起始字节错）必须断链。
func TestGarbageByteFailsLink(t *testing.T) {
	f := newFakeLink()
	c := &conn{
		link: f, deviceID: "x", ca: 1, timeout: 100 * time.Millisecond,
		points: map[string]*pointPlan{}, byIOA: map[uint32][]*pointPlan{},
		values: map[string]cachedValue{}, done: make(chan struct{}),
		ackCh: make(chan struct{}, 1), started: make(chan struct{}),
		pending: map[uint32]chan iec104.ASDU{}, testIdle: time.Hour,
	}
	c.lastRecv.Store(time.Now().UnixNano())
	go c.readLoop()
	f.feed([]byte{0x00, 0x04})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		err := c.linkErr
		c.mu.Unlock()
		if err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("garbage frame was not detected")
}

// writeFrame 必须带写超时；这里只验证调用不发生死锁并正确写出。
func TestSendFrameWrites(t *testing.T) {
	f := newFakeLink()
	c := &conn{link: f, timeout: 100 * time.Millisecond, done: make(chan struct{}),
		ackCh: make(chan struct{}, 1), pending: map[uint32]chan iec104.ASDU{},
		values: map[string]cachedValue{}, byIOA: map[uint32][]*pointPlan{},
		points: map[string]*pointPlan{}}
	if err := c.sendU(iec104.UStartAct); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	if len(got) != 6 || got[2] != iec104.UStartAct {
		t.Fatalf("frame = % X", got)
	}
	if err := c.sendS(); err != nil {
		t.Fatal(err)
	}
	if len(f.last()) != 6 || f.last()[2] != 0x01 {
		t.Fatalf("S frame = % X", f.last())
	}
}

// 累计量的 IV 位必须一路走到点位质量：设备说「这个读数无效」，驱动不能
// 把它的值当 good 报上去（电能量结算按增量计费，一个假增量就是一笔错账）。
// 这条覆盖 codec → 驱动缓存 → Read 的整条链路，而不只是 codec 的单元。
func TestReadCounterInvalidQualityIsBad(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "counter")

	if err := hh.srv.SetValue(3001, sim.Info{Counter: 4321, Quality: iec104.QInvalid}); err != nil {
		t.Fatalf("set value: %v", err)
	}
	// 计数变化会触发一次突发上送；等到质量位到达。
	deadline := time.Now().Add(3 * time.Second)
	var last driver.RawSample
	for time.Now().Before(deadline) {
		out, _ := c.Read(context.Background(), []driver.ReadSpan{{
			Points: []driver.PointRef{{ID: "counter"}},
		}})
		last = out[0]
		if last.Quality == model.QualityBad {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last.Quality != model.QualityBad {
		t.Fatalf("counter quality = %s (want bad: the station flagged IV)", last.Quality)
	}
	if last.Err == nil {
		t.Fatal("a bad counter must carry a diagnostic error")
	}
}
