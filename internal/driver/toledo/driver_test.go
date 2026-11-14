package toledo

import (
	"context"
	"testing"
	"time"

	toledocodec "uic/internal/codec/toledo"
	"uic/internal/driver"
	"uic/internal/model"
	toledosim "uic/internal/sim/toledo"
)

// 这一组用例跑在「真 TCP + 内置衡器」上，覆盖驱动与协议栈的整体行为：建链、
// Probe、整帧取读数、按符号寻址、状态到质量码的降级、错误分类。分帧与解析的
// 细节由 codec 单测钉死，这里只验证「驱动拿到解析结果后怎么用」。

func startSim(t *testing.T, opts ...toledosim.Option) *toledosim.Server {
	t.Helper()
	srv, err := toledosim.NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func openConn(t *testing.T, srv *toledosim.Server, points ...driver.PointRef) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "scale", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(5 * time.Second),
		Timeout:  model.Duration(time.Second),
		Labels:   map[string]string{},
	}
	// 把点位塞进 device.Points（Open 从这里取）。
	for _, p := range points {
		dev.Points = append(dev.Points, model.Point{
			ID:       p.ID,
			Address:  p.Address,
			DataType: p.DataType,
			Enabled:  true,
		})
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "toledo",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func pt(id, path string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		Address:  model.Address{Kind: model.AddressSymbol, Path: path},
		DataType: dt,
	}
}

func spans(points ...driver.PointRef) []driver.ReadSpan {
	return []driver.ReadSpan{{ID: "s", Points: points}}
}

// sampleByID 从 Read 结果里按点位 ID 取样本。
func sampleByID(out []driver.RawSample, id string) (driver.RawSample, bool) {
	for _, s := range out {
		if s.PointID == id {
			return s, true
		}
	}
	return driver.RawSample{}, false
}

// TestProbeReceivesFrame 建链后 Probe 应能等到连续输出帧。
func TestProbeReceivesFrame(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(149.0, "kg", 1))
	c := openConn(t, srv, pt("w", "weight", model.TypeFloat64))
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// TestReadStableWeight 稳定重量帧 → weight 点位 good，值为设定值。
func TestReadStableWeight(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(149.0, "kg", 1))
	c := openConn(t, srv,
		pt("w", "weight", model.TypeFloat64),
		pt("st", "status", model.TypeString),
		pt("u", "unit", model.TypeString),
		pt("m", "mode", model.TypeString),
	)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}

	var out []driver.RawSample
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		out, err = c.Read(context.Background(), spans(
			pt("w", "weight", model.TypeFloat64),
			pt("st", "status", model.TypeString),
			pt("u", "unit", model.TypeString),
			pt("m", "mode", model.TypeString),
		))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if s, ok := sampleByID(out, "w"); ok && s.Quality == model.QualityGood {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	s, ok := sampleByID(out, "w")
	if !ok {
		t.Fatal("no weight sample")
	}
	if s.Quality != model.QualityGood {
		t.Fatalf("weight quality = %s, want good", s.Quality)
	}
	v, _ := s.Value.AsFloat()
	if v != 149.0 {
		t.Errorf("weight = %v, want 149.0", v)
	}
	if s, _ := sampleByID(out, "st"); s.Value.Text != "ST" {
		t.Errorf("status = %q, want ST", s.Value.Text)
	}
	if s, _ := sampleByID(out, "u"); s.Value.Text != "kg" {
		t.Errorf("unit = %q, want kg", s.Value.Text)
	}
	if s, _ := sampleByID(out, "m"); s.Value.Text != "GS" {
		t.Errorf("mode = %q, want GS", s.Value.Text)
	}
}

// TestReadMotionDegradesToUncertain 运动中 → weight 点位 uncertain，status 仍 good。
func TestReadMotionDegradesToUncertain(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(50.0, "kg", 1))
	srv.SetStatus(toledocodec.StatusMotion)
	c := openConn(t, srv,
		pt("w", "weight", model.TypeFloat64),
		pt("st", "status", model.TypeString),
	)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, _ := c.Read(context.Background(), spans(
		pt("w", "weight", model.TypeFloat64),
		pt("st", "status", model.TypeString),
	))
	if s, _ := sampleByID(out, "w"); s.Quality != model.QualityUncertain {
		t.Errorf("motion weight quality = %s, want uncertain", s.Quality)
	}
	if s, _ := sampleByID(out, "st"); s.Quality != model.QualityGood {
		t.Errorf("motion status quality = %s, want good (status always valid)", s.Quality)
	}
}

// TestReadOverloadDegradesToBad 超量程 → weight 点位 bad，status 仍 good。
func TestReadOverloadDegradesToBad(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(0, "kg", 1))
	srv.SetStatus(toledocodec.StatusOverload)
	c := openConn(t, srv,
		pt("w", "weight", model.TypeFloat64),
		pt("st", "status", model.TypeString),
	)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, _ := c.Read(context.Background(), spans(
		pt("w", "weight", model.TypeFloat64),
		pt("st", "status", model.TypeString),
	))
	if s, _ := sampleByID(out, "w"); s.Quality != model.QualityBad {
		t.Errorf("overload weight quality = %s, want bad", s.Quality)
	}
	if s, _ := sampleByID(out, "st"); s.Quality != model.QualityGood {
		t.Errorf("overload status quality = %s, want good", s.Quality)
	}
}

// TestStableMotionBoolPoints stable/motion 布尔点位。
func TestStableMotionBoolPoints(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(10.0, "kg", 1))
	c := openConn(t, srv,
		pt("stable", "stable", model.TypeBool),
		pt("motion", "motion", model.TypeBool),
	)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, _ := c.Read(context.Background(), spans(
		pt("stable", "stable", model.TypeBool),
		pt("motion", "motion", model.TypeBool),
	))
	if s, ok := sampleByID(out, "stable"); !ok || !s.Value.Bool {
		t.Errorf("stable point should be true for ST frame, got %+v", s)
	}
	if s, ok := sampleByID(out, "motion"); !ok || s.Value.Bool {
		t.Errorf("motion point should be false for ST frame, got %+v", s)
	}
}

// TestWriteUnsupported 写操作返回 ErrUnsupported。
func TestWriteUnsupported(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(10.0, "kg", 1))
	c := openConn(t, srv, pt("w", "weight", model.TypeFloat64))
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "w",
	}})
	if err == nil {
		t.Fatal("write should be unsupported")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Errorf("error kind = %s, want unsupported", driver.KindOf(err))
	}
}

// TestOpenRejectsRegisterAddress 点位必须是 symbol 地址，register 地址被拒。
func TestOpenRejectsRegisterAddress(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(10.0, "kg", 1))
	dev := model.Device{
		ID: "scale", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(5 * time.Second),
		Labels:   map[string]string{},
		Points: []model.Point{{
			ID:      "w",
			Address: model.Address{Kind: model.AddressRegister, Path: "0", Length: 2},
			Enabled: true,
		}},
	}
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "toledo",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second),
		},
		Device: dev,
	})
	if err == nil {
		t.Fatal("register address should be rejected")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Errorf("error kind = %s, want unsupported", driver.KindOf(err))
	}
}

// TestOpenRejectsUnknownSymbol 未知读数字段在 Open 时即被拒（fail fast）。
func TestOpenRejectsUnknownSymbol(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(10.0, "kg", 1))
	dev := model.Device{
		ID: "scale", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(5 * time.Second),
		Labels:   map[string]string{},
		Points: []model.Point{{
			ID:      "w",
			Address: model.Address{Kind: model.AddressSymbol, Path: "temperature"},
			Enabled: true,
		}},
	}
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "toledo",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second),
		},
		Device: dev,
	})
	if err == nil {
		t.Fatal("unknown symbol should be rejected")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Errorf("error kind = %s, want address", driver.KindOf(err))
	}
}
