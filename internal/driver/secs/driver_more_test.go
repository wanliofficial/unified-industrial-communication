package secs

import (
	"context"
	"testing"

	secscodec "uic/internal/codec/secs"
	"uic/internal/driver"
	"uic/internal/model"
)

// 覆盖配置解析、值编解码的边界分支，以及「只读 ECID」「写值无法编码」等
// 诚实失败路径。

func TestOpenBadLabels(t *testing.T) {
	srv := startSim(t)
	base := testConfig(srv, svidPoint("p", 1, model.TypeUint32))

	bad := base
	bad.Device.Labels = map[string]string{"retries": "99"}
	if _, err := New().Open(context.Background(), bad); driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("retries 越界应 ErrUnsupported，得到 %v", err)
	}
	bad = base
	bad.Device.Labels = map[string]string{"commTimeout": "0s"}
	if _, err := New().Open(context.Background(), bad); driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("commTimeout=0 应 ErrUnsupported，得到 %v", err)
	}
	bad = base
	bad.Device.Labels = map[string]string{"commTimeout": "notaduration"}
	if _, err := New().Open(context.Background(), bad); driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("非法 commTimeout 应 ErrUnsupported，得到 %v", err)
	}
}

func TestOpenMissingAddress(t *testing.T) {
	cfg := testConfig(startSim(t))
	cfg.Channel.Transport.Address = ""
	if _, err := New().Open(context.Background(), cfg); driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("缺地址应 ErrUnsupported，得到 %v", err)
	}
}

func TestPlanPointErrors(t *testing.T) {
	cases := []struct {
		name string
		ref  driver.PointRef
	}{
		{"register 寻址", driver.PointRef{ID: "p", Address: model.Address{Kind: model.AddressRegister}}},
		{"未知对象类型", driver.PointRef{ID: "p", Address: model.Address{Kind: model.AddressObject, ObjectType: "foo", Instance: 1, Property: "value"}, DataType: model.TypeUint32}},
		{"未知属性", driver.PointRef{ID: "p", Address: model.Address{Kind: model.AddressObject, ObjectType: "svid", Instance: 1, Property: "name"}, DataType: model.TypeUint32}},
		{"编号非正", driver.PointRef{ID: "p", Address: model.Address{Kind: model.AddressObject, ObjectType: "svid", Instance: 0, Property: "value"}, DataType: model.TypeUint32}},
	}
	for _, tc := range cases {
		if _, err := planPoint(tc.ref, false); err == nil {
			t.Fatalf("%s: 应报错", tc.name)
		}
	}
}

// TestReadStringNumber 覆盖 itemFloat 的「字符串里是数字」分支。
func TestReadStringNumber(t *testing.T) {
	srv := startSim(t)
	srv.SetVID(1, secscodec.ASCII("42"))
	cfg := testConfig(srv, svidPoint("p", 1, model.TypeInt32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if len(samples) != 1 || samples[0].Err != nil {
		t.Fatalf("字符串数字应能读成 int32: %+v", samples)
	}
	if samples[0].Value.Num != 42 {
		t.Fatalf("值 = %v, want 42", samples[0].Value.Num)
	}
}

func TestValueItemTypes(t *testing.T) {
	cases := []struct {
		dt   model.DataType
		val  model.Value
		want func(*secscodec.Item) bool
	}{
		{model.TypeFloat32, model.NumValue(model.TypeFloat32, 1.5), func(it *secscodec.Item) bool { v, err := it.F4(); return err == nil && v == 1.5 }},
		{model.TypeFloat64, model.NumValue(model.TypeFloat64, 2.5), func(it *secscodec.Item) bool { v, err := it.F8(); return err == nil && v == 2.5 }},
		{model.TypeInt64, model.NumValue(model.TypeInt64, 9), func(it *secscodec.Item) bool { v, err := it.I8(); return err == nil && v == 9 }},
		{model.TypeString, model.TextValue(model.TypeString, "x"), func(it *secscodec.Item) bool { s, err := it.String(); return err == nil && s == "x" }},
	}
	for _, tc := range cases {
		it, err := valueItem(tc.val, tc.dt)
		if err != nil {
			t.Fatalf("%s: %v", tc.dt, err)
		}
		if !tc.want(it) {
			t.Fatalf("%s: 编码结果不符 %s", tc.dt, it.Text())
		}
	}
}

// TestWriteReadOnlyECID 覆盖「ecid 但配置不可写」→ permission。
func TestWriteReadOnlyECID(t *testing.T) {
	srv := startSim(t)
	srv.SetECID(10, secscodec.Int(secscodec.FormatU4, 300))
	cfg := testConfig(srv, ecidPoint("limit", 10, model.TypeUint32, false))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "limit", Address: model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: 10, Property: "value"},
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 1),
	}})
	if len(res) != 1 || driver.KindOf(res[0].Err) != driver.ErrPermission {
		t.Fatalf("只读 ecid 应 permission: %+v", res)
	}
}

// TestWriteBadValue 覆盖「值无法按声明类型编码」→ unsupported。
func TestWriteBadValue(t *testing.T) {
	srv := startSim(t)
	// 点位声明 int32，但写入一个字符串值 → valueItem 无法把 string 转成整数项。
	cfg := testConfig(srv, ecidPoint("limit", 10, model.TypeInt32, true))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "limit", Address: model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: 10, Property: "value"},
		DataType: model.TypeInt32, Value: model.TextValue(model.TypeString, "abc"),
	}})
	if len(res) != 1 || driver.KindOf(res[0].Err) != driver.ErrUnsupported {
		t.Fatalf("无法编码的值应 unsupported: %+v", res)
	}
}

// TestUseAfterClose 覆盖 close 后的各操作 → disconnected。
func TestUseAfterClose(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("close 后 Probe 应 disconnected，得到 %v", err)
	}
	if _, err := c.Read(context.Background(), nil); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("close 后 Read 应 disconnected，得到 %v", err)
	}
	// 幂等 Close。
	if err := c.Close(); err != nil {
		t.Fatalf("二次 Close 应无错，得到 %v", err)
	}
}
