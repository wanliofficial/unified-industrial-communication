package cjt188

import (
	"context"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	cjsim "uic/internal/sim/cjt188"
)

// 这一组用例跑在「真 TCP + 内置从站」上，覆盖驱动与协议栈的整体行为：
// 读地址、按 DI 缓存字段、单位换算、写阀门/地址的权限闸门、地址校验。
// 分帧与 BCD 的细节由 codec 的单测钉住。

func startSim(t *testing.T, opts ...cjsim.Option) *cjsim.Server {
	t.Helper()
	srv, err := cjsim.NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func openConn(t *testing.T, srv *cjsim.Server, options string) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(time.Second),
		Labels: map[string]string{
			"address": srv.Address(),
			"type":    "10",
			"options": options,
		},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(800 * time.Millisecond),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func refs(fields ...FieldRef) []driver.ReadSpan {
	var out []driver.PointRef
	for _, f := range fields {
		out = append(out, driver.PointRef{
			ID: f.ID,
			Address: model.Address{
				Kind: model.AddressRegister, Area: f.DI, Offset: f.Offset, Bit: -1,
			},
			DataType: f.Type,
		})
	}
	return []driver.ReadSpan{{ID: "s", Points: out}}
}

// FieldRef 是测试里描述一个点的简写。
type FieldRef struct {
	ID     string
	DI     string
	Offset int
	Type   model.DataType
}

func TestProbeReadsActualAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbeRejectsWrongMeter(t *testing.T) {
	srv := startSim(t, cjsim.WithAddress("00000805000099"))
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Labels: map[string]string{"address": "00000805000001", "type": "10"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Probe(context.Background())
	if err == nil {
		t.Fatal("probe accepted a different meter")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

// TestReadWaterFields 一次 Read 里读 901F 的四个字段，只应发一帧。
func TestReadWaterFields(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	spans := refs(
		FieldRef{"cur", "901F", 0, model.TypeFloat64},
		FieldRef{"settle", "901F", 5, model.TypeFloat64},
		FieldRef{"clock", "901F", 10, model.TypeFloat64},
		FieldRef{"status", "901F", 17, model.TypeUint16},
	)
	before := srv.Requests()
	got, err := c.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after := srv.Requests(); after-before != 1 {
		t.Fatalf("requests = %d, want 1 (fields on the same DI must share one transaction)", after-before)
	}
	want := map[string]float64{
		"cur":    1234.56,
		"settle": 1200,
		"clock":  20261107120000,
		"status": 0,
	}
	for _, s := range got {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Value.Num != want[s.PointID] {
			t.Fatalf("%s = %v, want %v", s.PointID, s.Value.Num, want[s.PointID])
		}
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %v", s.PointID, s.Quality)
		}
	}
	if got[3].Value.Type != model.TypeUint16 {
		t.Fatalf("status type = %s", got[3].Value.Type)
	}
}

// TestReadHistorical 上 1 月与上 12 月结算日量。
func TestReadHistorical(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, err := c.Read(context.Background(), refs(
		FieldRef{"m1", "D120", 0, model.TypeFloat64},
		FieldRef{"m12", "D12B", 0, model.TypeFloat64},
	))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0].Value.Num != 1190 || got[1].Value.Num != 1080 {
		t.Fatalf("history = %v, %v", got[0].Value.Num, got[1].Value.Num)
	}
}

// TestReadHeatSchema 热量表的 901F 布局与单位换算。
func TestReadHeatSchema(t *testing.T) {
	srv := startSim(t, cjsim.WithType(0x20))
	dev := model.Device{
		ID: "heat", ChannelID: "ch", Enabled: true,
		Labels: map[string]string{"address": srv.Address(), "type": "20"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	spans := refs(
		FieldRef{"settle-heat", "901F", 0, model.TypeFloat64},
		FieldRef{"cur-heat", "901F", 5, model.TypeFloat64},
		FieldRef{"power", "901F", 10, model.TypeFloat64},
		FieldRef{"flow", "901F", 15, model.TypeFloat64},
		FieldRef{"volume", "901F", 20, model.TypeFloat64},
		FieldRef{"supply", "901F", 25, model.TypeFloat64},
		FieldRef{"return", "901F", 28, model.TypeFloat64},
		FieldRef{"hours", "901F", 31, model.TypeFloat64},
		FieldRef{"clock", "901F", 34, model.TypeFloat64},
		FieldRef{"status", "901F", 41, model.TypeUint16},
	)
	got, err := c.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := map[string]float64{
		"settle-heat": 120, "cur-heat": 123.45, "power": 12.34, "flow": 1.2345,
		"volume": 456.78, "supply": 55, "return": 45, "hours": 1234,
		"clock": 20261107120000, "status": 0,
	}
	for _, s := range got {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Value.Num != want[s.PointID] {
			t.Fatalf("%s = %v, want %v", s.PointID, s.Value.Num, want[s.PointID])
		}
	}
}

func TestValveWriteGate(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "") // 没有 allow-control
	got, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "valve",
		Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrPermission {
		t.Fatalf("write without allow-control = %v", got[0].Err)
	}
	if srv.Writes() != 0 {
		t.Fatalf("sim writes = %d", srv.Writes())
	}
}

func TestValveWriteOpenClose(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-control")
	write := func(v float64) driver.WriteResult {
		got, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: "valve",
			Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
			Value:   model.NumValue(model.TypeFloat64, v),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return got[0]
	}
	if r := write(0); r.Err != nil {
		t.Fatalf("close: %v", r.Err)
	}
	if v, _ := srv.Valve(); v != 1 {
		t.Fatalf("valve = %d, want closed (1)", v)
	}
	if r := write(1); r.Err != nil {
		t.Fatalf("open: %v", r.Err)
	}
	if v, _ := srv.Valve(); v != 0 {
		t.Fatalf("valve = %d, want open (0)", v)
	}
	// 读状态字应反映阀门状态。
	got, err := c.Read(context.Background(), refs(FieldRef{"status", "901F", 17, model.TypeUint16}))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Value.Num != 0 {
		t.Fatalf("status = %v", got[0].Value.Num)
	}
	if srv.Writes() != 2 {
		t.Fatalf("writes = %d", srv.Writes())
	}
}

func TestWriteAddrUpdatesConnection(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-write")
	got, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "A018", Bit: -1},
		Value:   model.TextValue(model.TypeString, "00000805000099"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Err != nil {
		t.Fatalf("write addr: %v", got[0].Err)
	}
	if srv.Address() != "00000805000099" {
		t.Fatalf("sim address = %s", srv.Address())
	}
	// 驱动应已切到新地址，下一次读必须还能通。
	res, err := c.Read(context.Background(), refs(FieldRef{"cur", "901F", 0, model.TypeFloat64}))
	if err != nil {
		t.Fatalf("read after rename: %v", err)
	}
	if res[0].Err != nil || res[0].Value.Num != 1234.56 {
		t.Fatalf("read after rename = %+v", res[0])
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe after rename: %v", err)
	}
}

func TestWriteAddrRequiresPermissionAndFullAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "A018", Bit: -1},
		Value:   model.TextValue(model.TypeString, "00000805000099"),
	}})
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrPermission {
		t.Fatalf("write without allow-write = %v", got[0].Err)
	}

	c2 := openConn(t, srv, "allow-write")
	got, _ = c2.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "A018", Bit: -1},
		Value:   model.TextValue(model.TypeString, "99"),
	}})
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrUnsupported {
		t.Fatalf("short address = %v", got[0].Err)
	}
	if srv.Address() != "00000805000001" {
		t.Fatalf("sim address changed to %s", srv.Address())
	}
}

func TestWriteOtherDIRejected(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-write,allow-control")
	got, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x",
		Address: model.Address{Kind: model.AddressRegister, Area: "D120", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 1),
	}})
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrUnsupported {
		t.Fatalf("write D120 = %v", got[0].Err)
	}
}

func TestBadPointAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	cases := []struct {
		name string
		di   string
		off  int
	}{
		{"unknown DI", "911F", 0},
		{"unknown offset", "901F", 2},
		{"valve is write-only", "A017", 0},
	}
	for _, tc := range cases {
		// 只有一个点时，Read 会同时返回失败样本与聚合错误；这里关心的是
		// 样本上的分类，而不是它是否被当成整轮错误上抛。
		got, _ := c.Read(context.Background(), refs(FieldRef{"p", tc.di, tc.off, model.TypeFloat64}))
		if len(got) != 1 {
			t.Fatalf("%s: samples = %d", tc.name, len(got))
		}
		if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrAddress {
			t.Fatalf("%s = %v", tc.name, got[0].Err)
		}
	}
}

func TestWrongLengthRejected(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
		ID: "p",
		Address: model.Address{
			Kind: model.AddressRegister, Area: "901F", Offset: 0, Length: 4, Bit: -1,
		},
		DataType: model.TypeFloat64,
	}}}})
	if len(got) != 1 {
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrAddress {
		t.Fatalf("length mismatch = %v", got[0].Err)
	}
}

// TestWrongAddressTimesOut 从站地址不匹配时保持静默，主站应按超时处理。
func TestWrongAddressTimesOut(t *testing.T) {
	srv := startSim(t, cjsim.WithAddress("00000805000099"))
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Labels: map[string]string{"address": "00000805000001", "type": "10"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(200 * time.Millisecond)},
		Device: dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Read(context.Background(), refs(FieldRef{"cur", "901F", 0, model.TypeFloat64}))
	if err == nil {
		t.Fatal("read from wrong address must fail")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
}

// TestPreambleOption 前导 0/4 都能通：从站按字节流扫描，不依赖前导。
func TestPreambleOption(t *testing.T) {
	for _, opt := range []string{"preamble=0", "preamble=4"} {
		srv := startSim(t)
		c := openConn(t, srv, opt)
		got, err := c.Read(context.Background(), refs(FieldRef{"cur", "901F", 0, model.TypeFloat64}))
		if err != nil {
			t.Fatalf("%s read: %v", opt, err)
		}
		if got[0].Value.Num != 1234.56 {
			t.Fatalf("%s value = %v", opt, got[0].Value.Num)
		}
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"missing address", map[string]string{"type": "10"}},
		{"short address", map[string]string{"address": "123", "type": "10"}},
		{"wildcard address", map[string]string{"address": "AAAA0805000001", "type": "10"}},
		{"bad type", map[string]string{"address": "00000805000001", "type": "AA"}},
	}
	for _, tc := range cases {
		_, err := New().Open(context.Background(), driver.OpenConfig{
			Channel: model.Channel{ID: "ch", Protocol: "cjt188",
				Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"},
				Timeout:   model.Duration(time.Second)},
			Device: model.Device{ID: "m", Labels: tc.labels},
		})
		if err == nil {
			t.Fatalf("%s: open must fail", tc.name)
		}
		if driver.KindOf(err) != driver.ErrUnsupported {
			t.Fatalf("%s kind = %v", tc.name, driver.KindOf(err))
		}
	}
}
