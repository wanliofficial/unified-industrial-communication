package bacnetmstp

import (
	"math"
	"testing"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/model"
)

// 与 BACnet/IP 驱动（internal/driver/bacnet/regress_test.go）同源的写值
// 数值边界回归：REAL 的 float32 溢出 / 非有限值，UNSIGNED/ENUMERATED 的
// 非整数。两类问题都违背「写入失败不得静默」——调用方拿到成功，设备收到
// 的是另一个值。MS/TP 与 IP 两个驱动共用 codec，防护必须同强度。
func mstpPlan(t *testing.T, objType, prop string, dt model.DataType) pointPlan {
	t.Helper()
	plan, err := planPoint(driver.PointRef{ID: "x", DataType: dt, Address: model.Address{
		Kind: model.AddressObject, ObjectType: objType, Instance: 2, Property: prop,
	}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

func TestWriteRejectsRealOverflowAndNonIntegerEnum(t *testing.T) {
	realCases := []struct {
		name string
		val  float64
	}{
		{"float64 1e300 溢出 float32", 1e300},
		{"float64 NaN", math.NaN()},
		{"float64 +Inf", math.Inf(1)},
		{"float64 下溢 1e-300", 1e-300},
	}
	for _, tc := range realCases {
		plan := mstpPlan(t, "analog-value", "present-value", model.TypeFloat32)
		out, err := encodeWriteValue(plan, driver.WriteRequest{
			PointID: "x", DataType: model.TypeFloat32,
			Value: model.NumValue(model.TypeFloat64, tc.val),
		})
		if err == nil {
			t.Fatalf("%s: must reject, got encoded % X", tc.name, out)
		}
		if driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("%s: wrong kind %v (err=%v)", tc.name, driver.KindOf(err), err)
		}
	}

	enumCases := []struct {
		name string
		obj  string
		val  float64
	}{
		{"1.5 写 UNSIGNED(multi-state)", "multi-state-value", 1.5},
		{"2.7 写 ENUMERATED(binary)", "binary-value", 2.7},
	}
	for _, tc := range enumCases {
		plan := mstpPlan(t, tc.obj, "present-value", model.TypeUint32)
		out, err := encodeWriteValue(plan, driver.WriteRequest{
			PointID: "x", DataType: model.TypeUint32,
			Value: model.NumValue(model.TypeUint32, tc.val),
		})
		if err == nil {
			t.Fatalf("%s: must reject, got encoded % X", tc.name, out)
		}
		if driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("%s: wrong kind %v (err=%v)", tc.name, driver.KindOf(err), err)
		}
	}
}

// 正常值不受影响：合法 REAL 与合法整数仍照常编码。
func TestWriteAcceptsValidRealAndInteger(t *testing.T) {
	plan := mstpPlan(t, "analog-value", "present-value", model.TypeFloat32)
	out, err := encodeWriteValue(plan, driver.WriteRequest{
		PointID: "x", DataType: model.TypeFloat32,
		Value: model.NumValue(model.TypeFloat64, 9.5),
	})
	if err != nil {
		t.Fatalf("9.5 must encode: %v", err)
	}
	tag, _, f, _, _, derr := bacnetcodec.DecodeApplicationValue(out)
	if derr != nil || tag != bacnetcodec.AppReal || f != 9.5 {
		t.Fatalf("encoded = tag %d f %v (% X) derr %v", tag, f, out, derr)
	}

	plan2 := mstpPlan(t, "multi-state-value", "present-value", model.TypeUint32)
	out2, err := encodeWriteValue(plan2, driver.WriteRequest{
		PointID: "x", DataType: model.TypeUint32,
		Value: model.NumValue(model.TypeUint32, 2),
	})
	if err != nil {
		t.Fatalf("integer 2 must encode: %v", err)
	}
	tag2, u, _, _, _, derr2 := bacnetcodec.DecodeApplicationValue(out2)
	if derr2 != nil || tag2 != bacnetcodec.AppUnsigned || u != 2 {
		t.Fatalf("encoded = tag %d u %d (% X) derr %v", tag2, u, out2, derr2)
	}
}

// NULL 写入编码成 AppNull，与 BACnet/IP 驱动同一入口（relinquish 语义）。
func TestWriteNullEncodesAsAppNull(t *testing.T) {
	plan := mstpPlan(t, "analog-value", "present-value", model.TypeFloat32)
	out, err := encodeWriteValue(plan, driver.WriteRequest{
		PointID: "x", DataType: model.TypeFloat32,
		Value: model.TextValue(model.TypeString, "null"),
	})
	if err != nil {
		t.Fatalf("null encode: %v", err)
	}
	tag, _, _, _, _, derr := bacnetcodec.DecodeApplicationValue(out)
	if derr != nil || tag != bacnetcodec.AppNull {
		t.Fatalf("tag = %d derr %v, want AppNull", tag, derr)
	}
}
