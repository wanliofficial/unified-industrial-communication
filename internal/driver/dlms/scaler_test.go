package dlms

import (
	"context"
	"testing"
	"time"

	dl "uic/internal/codec/dlms"
	"uic/internal/driver"
	"uic/internal/model"
	simdlms "uic/internal/sim/dlms"
)

// Register 家族的取值必须配 scaler：attribute 2 是原始整数、attribute 3 是
// scaler_unit，工程值 = 原始整数 × 10^scaler。取不到 scaler 时把原始整数当 good
// 交出去就是**静默回绕**——2201（真值 220.1 V，scaler=-1）会被当成 2201 上报，
// 整整差 10 倍且毫无征兆。这与 codec 层 scalerUnit「形态不对不默认 0」是同一条
// 既定原则（见 TestScalerUnitShape：默认 0 = 不缩放 = 真值可能差 1000 倍）。
//
// 这条用例锁两件事：
//  1. attribute 3 缺失时，该点必须**判坏**（有 quality、有 err），不能报一个
//     可能错 10^n 倍的 raw good 值；
//  2. 该读失败**不得被永久缓存**——随后补上 attribute 3，同一条连接上要能回到
//     正确缩放（220.1），而不是被首轮的「无 scaler」哨兵永久毒化。
func TestReadRegisterScalerUnavailableMarksBadAndRecovers(t *testing.T) {
	o, _ := dl.ParseOBIS("1.0.32.7.0.255")
	// Register：只有 attribute 2（value=2201）与 attribute 1（OBIS），
	// 故意不给 attribute 3（scaler_unit）。
	objs := []simdlms.Object{
		{ClassID: dl.ClassRegister, OBIS: o, Attribute: dl.AttrValue, Value: dl.U16(2201), Name: "A 相电压"},
		{ClassID: dl.ClassRegister, OBIS: o, Attribute: dl.AttrLogicalName, Value: dl.Octets(o[:]), Name: "ln"},
	}
	srv, err := simdlms.NewServer("127.0.0.1:0", simdlms.WithObjects(objs))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pt := driver.PointRef{ID: "v",
		Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.32.7.0.255:3:2"}}
	spans := []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{pt}}}

	// 第一轮：attribute 3 缺失 → 该点判坏，绝不能把 2201 当 good 交出去。
	samples, err := conn.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("第一轮 Read 返回整体错误 %v；应保留好样本语义（本例只有这一个点，允许 err!=nil，但样本必须标坏）", err)
	}
	if len(samples) != 1 {
		t.Fatalf("样本数 = %d, want 1", len(samples))
	}
	s0 := samples[0]
	if s0.Err == nil {
		t.Fatalf("attribute 3 缺失时该点报 good（val=%v）；这是静默回绕，必须判坏", s0.Value)
	}
	if s0.Quality == model.QualityGood {
		t.Fatalf("attribute 3 缺失时 quality = good；必须标坏")
	}
	t.Logf("第一轮（attr3 缺失）: q=%s err=%v", s0.Quality, s0.Err)

	// 补上 attribute 3（scaler=-1, unit=34=V）：2201 → 220.1。
	srv.AddObject(simdlms.Object{
		ClassID: dl.ClassRegister, OBIS: o, Attribute: dl.AttrScalerUnit,
		Value: dl.Structure(dl.I8(-1), dl.Enum(34)), Name: "A 相电压 scaler_unit",
	})

	// 第二轮：同一条连接，scaler 现在可读 → 必须回到正确缩放，且 quality=good。
	samples, err = conn.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("第二轮 Read: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("样本数 = %d, want 1", len(samples))
	}
	s1 := samples[0]
	if s1.Err != nil || s1.Quality != model.QualityGood {
		t.Fatalf("第二轮（attr3 已补）q=%s err=%v；应恢复成 good", s1.Quality, s1.Err)
	}
	got, ok := s1.Value.AsFloat()
	if !ok {
		t.Fatalf("第二轮值不是数值：%v", s1.Value)
	}
	if got < 220.0 || got > 220.2 {
		t.Fatalf("第二轮值 = %v，want 220.1（2201 × 10^-1）；若不是说明首轮失败被永久缓存", got)
	}
	t.Logf("第二轮（attr3 已补）: q=%s val=%v", s1.Quality, got)
}
