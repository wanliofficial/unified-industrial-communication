package bacnet

import (
	"context"
	"math"
	"testing"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/model"
)

// 探针缺陷 1：ParseConfirmedRequest 的长度守卫 off-by-one（codec 包，见
// internal/codec/bacnet/regress_test.go）。

// 探针缺陷 2/3：encodeWriteValue 写路径的静默溢出 / 静默截断。
//
// 两个场景都违背「写入失败不得静默」：调用方拿到成功，设备实际收到的是
// 另一个值。
//
//   - analog 的 present-value 走 REAL(float32)。dataType=float64 的点写 1e300
//     在 textval.CheckRange 里是合法的 float64，但强转 float32 变 +Inf，原来
//     会原样编码发送——设备收到 +Inf。
//   - multi-state / binary 的 present-value 走 UNSIGNED/ENUMERATED（整数）。
//     写 1.5 原来强转成 uint64 静默变 1。
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
		plan, err := planPoint(objRef("x", "analog-value", 2, "present-value", model.TypeFloat64))
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		out, err := encodeWriteValue(plan, writeReq("x", "analog-value", 2, "present-value", model.TypeFloat64, model.NumValue(model.TypeFloat64, tc.val)))
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
		plan, err := planPoint(objRef("x", tc.obj, 2, "present-value", model.TypeUint32))
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		out, err := encodeWriteValue(plan, writeReq("x", tc.obj, 2, "present-value", model.TypeUint32, model.NumValue(model.TypeUint32, tc.val)))
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
	plan, err := planPoint(objRef("x", "analog-value", 2, "present-value", model.TypeFloat64))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	out, err := encodeWriteValue(plan, writeReq("x", "analog-value", 2, "present-value", model.TypeFloat64, model.NumValue(model.TypeFloat64, 9.5)))
	if err != nil {
		t.Fatalf("9.5 must encode: %v", err)
	}
	tag, _, f, _, _, derr := bacnetcodec.DecodeApplicationValue(out)
	if derr != nil || tag != bacnetcodec.AppReal || f != 9.5 {
		t.Fatalf("encoded = tag %d f %v (% X) derr %v", tag, f, out, derr)
	}

	plan2, err := planPoint(objRef("x", "multi-state-value", 2, "present-value", model.TypeUint32))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	out2, err := encodeWriteValue(plan2, writeReq("x", "multi-state-value", 2, "present-value", model.TypeUint32, model.NumValue(model.TypeUint32, 2)))
	if err != nil {
		t.Fatalf("integer 2 must encode: %v", err)
	}
	tag2, u, _, _, _, derr2 := bacnetcodec.DecodeApplicationValue(out2)
	if derr2 != nil || tag2 != bacnetcodec.AppUnsigned || u != 2 {
		t.Fatalf("encoded = tag %d u %d (% X) derr %v", tag2, u, out2, derr2)
	}
}

// 端到端：经驱动写路径（含 exchange），越界值必须整体失败、设备值不变。
func TestWriteOverflowRejectedEndToEnd(t *testing.T) {
	srv := newSim(t)
	if err := srv.SetValue("analog-value", 2, "present-value", float64(9.5)); err != nil {
		t.Fatalf("set: %v", err)
	}
	c := openSim(t, srv)
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		writeReq("x", "analog-value", 2, "present-value", model.TypeFloat64, model.NumValue(model.TypeFloat64, 1e300)),
	})
	if err == nil {
		t.Fatal("overflow write must return error")
	}
	if len(res) == 1 && res[0].OK {
		t.Fatalf("overflow write reported OK: %+v", res[0])
	}
	// 设备值应保持原样。
	s := readOne(t, c, objRef("x", "analog-value", 2, "present-value", model.TypeFloat64))
	if s.Err != nil || s.Value.Num != 9.5 {
		t.Fatalf("device value changed: %+v err=%v", s.Value, s.Err)
	}
}
