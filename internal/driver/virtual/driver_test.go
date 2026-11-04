package virtual

import (
	"context"
	"math"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

func openCfg(dev model.Device) driver.OpenConfig {
	return driver.OpenConfig{
		Channel: model.Channel{ID: "ch1", Protocol: "virtual", Transport: model.Transport{Kind: "memory"}},
		Device:  dev,
	}
}

func deviceWith(labels map[string]string) model.Device {
	return model.Device{ID: "vd1", ChannelID: "ch1", Labels: labels,
		Interval: model.Duration(time.Second), Enabled: true}
}

func floatPoint(id string) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: model.TypeFloat32, Length: 2,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Bit: -1},
	}
}

func open(t *testing.T, labels map[string]string) driver.Conn {
	t.Helper()
	c, err := New().Open(context.Background(), openCfg(deviceWith(labels)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readOne(t *testing.T, c driver.Conn, p driver.PointRef) driver.RawSample {
	t.Helper()
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].PointID != p.ID {
		t.Fatalf("samples = %+v", got)
	}
	return got[0]
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "virtual" || m.SDKVersion != driver.SDKVersion {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.BitAccess != driver.CapSupported {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
	if !m.Accepts(model.AddressRegister) || !m.Accepts(model.AddressSymbol) {
		t.Fatal("address kinds")
	}
	if err := driver.Register(New()); err != nil {
		t.Fatalf("meta must satisfy registry invariants: %v", err)
	}
}

// 虚拟驱动只支持 memory 传输：配错应由驱动自己拒绝，而不是静默假成功。
func TestOpenRejectsNonMemoryTransport(t *testing.T) {
	cfg := openCfg(deviceWith(nil))
	cfg.Channel.Transport = model.Transport{Kind: "tcp", Address: "127.0.0.1:502"}
	_, err := New().Open(context.Background(), cfg)
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v, kind = %s", err, driver.KindOf(err))
	}
}

func TestWaveforms(t *testing.T) {
	p := floatPoint("p1")

	// const 必须恒定。
	c := open(t, map[string]string{"mode": "const"})
	for i := 0; i < 3; i++ {
		if v := readOne(t, c, p).Value.Num; v != 42 {
			t.Fatalf("const = %v", v)
		}
	}

	// sine 在 10..90 之间波动。
	c = open(t, map[string]string{"mode": "sine"})
	for i := 0; i < 5; i++ {
		if v := readOne(t, c, p).Value.Num; v < 10 || v > 90 {
			t.Fatalf("sine out of range: %v", v)
		}
	}

	// ramp 落在 0..100，random 落在 0..100。
	for _, mode := range []string{"ramp", "random"} {
		c := open(t, map[string]string{"mode": mode})
		for i := 0; i < 5; i++ {
			v := readOne(t, c, p).Value.Num
			if v < 0 || v > 100 || math.IsNaN(v) {
				t.Fatalf("%s out of range: %v", mode, v)
			}
		}
	}
}

// 同一个点位在不同设备上相位不同，否则多设备演示会完全重合。
func TestPhaseDiffersPerPoint(t *testing.T) {
	c := open(t, map[string]string{"mode": "sine"})
	a := readOne(t, c, floatPoint("a")).Value.Num
	b := readOne(t, c, floatPoint("b")).Value.Num
	if a == b {
		t.Fatalf("identical phase for different points: %v", a)
	}
}

func TestBoolAndTextTypes(t *testing.T) {
	c := open(t, nil)
	got := readOne(t, c, driver.PointRef{ID: "b1", DataType: model.TypeBool, Length: 1,
		Address: model.Address{Kind: model.AddressRegister, Area: "coil", Bit: -1}})
	if got.Value.Type != model.TypeBool {
		t.Fatalf("bool point = %+v", got.Value)
	}
	got = readOne(t, c, driver.PointRef{ID: "s1", DataType: model.TypeString, Length: 1,
		Address: model.Address{Kind: model.AddressSymbol, Path: "State"}})
	if got.Value.Type != model.TypeString || got.Value.Text == "" {
		t.Fatalf("string point = %+v", got.Value)
	}
}

func TestWriteEchoesBackOnRead(t *testing.T) {
	c := open(t, map[string]string{"mode": "const"})
	p := floatPoint("p1")
	if v := readOne(t, c, p).Value.Num; v != 42 {
		t.Fatalf("pre-write = %v", v)
	}
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "p1", Address: p.Address, DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat32, 7.5),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].OK || res[0].PointID != "p1" {
		t.Fatalf("write result = %+v", res)
	}
	if v := readOne(t, c, p).Value.Num; v != 7.5 {
		t.Fatalf("written value not echoed: %v", v)
	}
}

// 故障注入是稀缺能力：必须能按「第 n 次请求」精确触发一次，之后恢复。
func TestFaultInjection(t *testing.T) {
	p := floatPoint("p1")
	cases := map[string]driver.ErrorKind{
		"drop":       driver.ErrTimeout,
		"timeout":    driver.ErrTimeout,
		"bad":        driver.ErrProtocol,
		"address":    driver.ErrAddress,
		"disconnect": driver.ErrDisconnected,
	}
	for fault, want := range cases {
		c := open(t, map[string]string{"fault": fault, "faultAt": "2"})
		if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}}); err != nil {
			t.Fatalf("%s: first read should succeed, got %v", fault, err)
		}
		_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
		if err == nil {
			t.Fatalf("%s: expected injected error", fault)
		}
		if got := driver.KindOf(err); got != want {
			t.Fatalf("%s: kind = %s, want %s", fault, got, want)
		}
		if !driver.IsRetryable(err) && fault != "bad" && fault != "address" {
			t.Fatalf("%s: transient fault should be retryable: %v", fault, err)
		}
		// 只注入一次。
		if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}}); err != nil {
			t.Fatalf("%s: fault should clear after one shot: %v", fault, err)
		}
	}
}

func TestDisconnectAffectsProbeAndWrite(t *testing.T) {
	c := open(t, map[string]string{"fault": "disconnect"})
	if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("probe err = %v", err)
	}
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{PointID: "p1"}}); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("write err = %v", err)
	}
}

// 没有故障时 Probe 必须成功（否则调度器永远进不了采集态）。
func TestProbeHealthy(t *testing.T) {
	c := open(t, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe = %v", err)
	}
	// 边界：faultAt 为非数字时视为 0，不注入。
	c = open(t, map[string]string{"fault": "timeout", "faultAt": "not-a-number"})
	for i := 0; i < 3; i++ {
		if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{floatPoint("p1")}}}); err != nil {
			t.Fatalf("bad faultAt should disable injection: %v", err)
		}
	}
}

func TestReadHonorsCanceledContext(t *testing.T) {
	c := open(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{floatPoint("p1")}}})
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
}

// 批量读必须按 span 顺序展开所有点位，不能丢点。
func TestReadExpandsAllSpans(t *testing.T) {
	c := open(t, nil)
	got, err := c.Read(context.Background(), []driver.ReadSpan{
		{ID: "s1", Points: []driver.PointRef{floatPoint("a"), floatPoint("b")}},
		{ID: "s2", Points: []driver.PointRef{floatPoint("c")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("samples = %d", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].PointID != want {
			t.Fatalf("sample[%d] = %q, want %q", i, got[i].PointID, want)
		}
		if got[i].Quality != model.QualityGood {
			t.Fatalf("quality = %s", got[i].Quality)
		}
	}
}
