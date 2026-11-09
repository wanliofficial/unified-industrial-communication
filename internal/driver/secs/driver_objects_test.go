package secs

import (
	"context"
	"testing"

	secscodec "uic/internal/codec/secs"
	"uic/internal/driver"
	"uic/internal/model"
)

// 这一组测试钉住三件「只会在真实连线上暴露」的事：
//
//  1. SVID 与 ECID 是两套编号空间，同号互不影响；
//  2. 自检扩展（RawRead / Describe）走的是同一条事务层，不另起一套握手；
//  3. 写未知 ECID 会被设备拒绝，而不是被静默接受。

// inspector 取驱动的自检扩展。用类型断言而不是改 Open 的返回类型：真实调用方
// （调度、北向）只看 driver.Conn，多出来的方法只有诊断路径才需要。
func inspector(t *testing.T, c driver.Conn) Inspector {
	t.Helper()
	in, ok := c.(Inspector)
	if !ok {
		t.Fatalf("驱动没有实现 Inspector: %T", c)
	}
	return in
}

func TestSVIDAndECIDSeparateNamespaces(t *testing.T) {
	srv := startSim(t)
	srv.SetVID(5, secscodec.Int(secscodec.FormatU4, 111))
	srv.SetECID(5, secscodec.Int(secscodec.FormatU4, 222))

	cfg := testConfig(srv,
		svidPoint("v", 5, model.TypeUint32),
		ecidPoint("c", 5, model.TypeUint32, false),
	)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
		driver.PointRefOf(cfg.Device, cfg.Device.Points[1]),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := map[string]float64{}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		got[s.PointID] = s.Value.Num
	}
	if got["v"] != 111 || got["c"] != 222 {
		t.Fatalf("同名编号读串了：svid=%.0f ecid=%.0f", got["v"], got["c"])
	}
}

func TestRawReadShowsDeviceFormat(t *testing.T) {
	srv := startSim(t)
	srv.SetVID(1, secscodec.ASCII("RUN"))

	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	it, err := inspector(t, c).RawRead(context.Background(), "svid", 1)
	if err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if it.Format != secscodec.FormatASCII {
		t.Fatalf("格式 = %s, want A", secscodec.FormatName(it.Format))
	}
	if s, _ := it.String(); s != "RUN" {
		t.Fatalf("值 = %q", s)
	}
}

func TestRawReadMissingID(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	in := inspector(t, c)
	if _, err := in.RawRead(context.Background(), "svid", 9999); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("不存在的编号应 ErrAddress，得到 %v", err)
	}
	if _, err := in.RawRead(context.Background(), "bogus", 1); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("未知对象类型应 ErrAddress，得到 %v", err)
	}
}

func TestDescribeNames(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	vars, err := inspector(t, c).Describe(context.Background(), []uint32{1, 2})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(vars) != 2 {
		t.Fatalf("变量数 = %d, want 2", len(vars))
	}
	if vars[0].ID != 1 || vars[0].Name != "Temperature" || vars[0].Units != "C" {
		t.Fatalf("第 1 项 = %+v", vars[0])
	}
	// 设备未声明的编号不出现在应答里（标准行为），驱动不应凭空补一行。
	if vars[1].ID != 2 || vars[1].Name != "State" {
		t.Fatalf("第 2 项 = %+v", vars[1])
	}
}

func TestWriteUnknownECIDRejected(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv, ecidPoint("x", 999, model.TypeUint32, true))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: 999, Property: "value"},
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 1),
	}})
	if len(res) != 1 || res[0].OK {
		t.Fatalf("未知 ECID 必须被拒: %+v", res)
	}
	if driver.KindOf(res[0].Err) != driver.ErrUnsupported {
		t.Fatalf("拒绝原因应为 ErrUnsupported（设备侧拒绝），得到 %v", res[0].Err)
	}
	if err == nil {
		t.Fatal("整批被拒时也应返回错误")
	}
}

// 一个 span 里同时有 svid 与 ecid 时，必须是两次事务：先 S1F3 再 S2F13。
func TestReadSpanWithBothObjects(t *testing.T) {
	srv := startSim(t)
	before := srv.Requests()
	cfg := testConfig(srv,
		svidPoint("v1", 1, model.TypeUint32),
		svidPoint("v2", 2, model.TypeString),
		ecidPoint("c1", 10, model.TypeUint32, false),
	)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	refs := make([]driver.PointRef, 0, 3)
	for _, p := range cfg.Device.Points {
		refs = append(refs, driver.PointRefOf(cfg.Device, p))
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("样本数 = %d", len(samples))
	}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
	}
	// 3 个点、两组 → 2 次 DATA 请求（不是 3 次）。
	if got := srv.Requests() - before; got != 2 {
		t.Fatalf("DATA 请求数 = %d, want 2（svid 一组、ecid 一组）", got)
	}
}
