package bacnetmstp

import (
	"context"
	"testing"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/model"
)

// fakeReplyBus 在每次 Write 后按帧类型注入应答，便于覆盖「被动令牌 + 现场
// 探测下一主站」这条路径（pollMaster 在正常从站轮询里不会被执行）。
type fakeReplyBus struct {
	*fakeBus
	// onData 返回给数据帧的应答；onPoll 返回给 PollForMaster 的应答。
	onData func(wire []byte) [][]byte
	onPoll func(wire []byte) [][]byte
}

func (f *fakeReplyBus) Write(p []byte) error {
	fakeFrame, _, err := bacnetcodec.DecodeMSTPFrame(p)
	if err != nil {
		return f.fakeBus.Write(p)
	}
	var frames [][]byte
	switch fakeFrame.Type {
	case bacnetcodec.FrameDataExpectingReply:
		if f.onData != nil {
			frames = f.onData(p)
		}
	case bacnetcodec.FramePollForMaster:
		if f.onPoll != nil {
			frames = f.onPoll(p)
		}
	}
	if err := f.fakeBus.Write(p); err != nil {
		return err
	}
	f.fakeBus.mu.Lock()
	for _, fr := range frames {
		f.fakeBus.buf = append(f.fakeBus.buf, fr...)
	}
	f.fakeBus.mu.Unlock()
	return nil
}

func mustFrame(t *testing.T, f bacnetcodec.MSTPFrame) []byte {
	t.Helper()
	out, err := bacnetcodec.BuildMSTPFrame(f)
	if err != nil {
		t.Fatalf("build frame: %v", err)
	}
	return out
}

// 被动收到 Token 且没有配置 nextStation 时，驱动应通过 PollForMaster 现场
// 探测到下一个主站，再把令牌交给它。
func TestPassTokenScansForNextMaster(t *testing.T) {
	app, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 1, nil, false)
	ack, _ := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, 1,
		bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, app)
	reply := mustFrame(t, bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataNotExpectingReply, Dest: 0, Src: 128,
		Data: bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlNone, ack),
	})
	token := mustFrame(t, bacnetcodec.MSTPFrame{Type: bacnetcodec.FrameToken, Dest: 0, Src: 128})

	bus := &fakeReplyBus{fakeBus: &fakeBus{}}
	bus.onData = func([]byte) [][]byte { return [][]byte{token, reply} }
	// 只有 MAC 3 会应答 PollForMaster。
	bus.onPoll = func(wire []byte) [][]byte {
		f, _, err := bacnetcodec.DecodeMSTPFrame(wire)
		if err != nil || f.Dest != 3 {
			return nil
		}
		return [][]byte{mustFrame(t, bacnetcodec.MSTPFrame{
			Type: bacnetcodec.FrameReplyToPollForMaster, Dest: f.Src, Src: f.Dest,
		})}
	}

	c := openFake(t, bus, map[string]string{"address": "128"})
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		objPoint("temp", "analog-input", 1, "present-value", model.TypeFloat32),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Num != 1 {
		t.Fatalf("value = %v", samples[0].Value.Num)
	}

	var passedTo3 bool
	bus.mu.Lock()
	for _, w := range bus.writes {
		f, _, err := bacnetcodec.DecodeMSTPFrame(w)
		if err == nil && f.Type == bacnetcodec.FrameToken && f.Dest == 3 {
			passedTo3 = true
		}
	}
	bus.mu.Unlock()
	if !passedTo3 {
		t.Fatal("driver did not pass the token to the master discovered by PollForMaster")
	}
}

// mapBacnetError 的每个错误类都必须落到一个有意义的平台错误种类。
func TestAPDUErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		class byte
		code  byte
		want  driver.ErrorKind
	}{
		{"unknown object", bacnetcodec.ErrorClassObject, bacnetcodec.ErrorCodeUnknownObject, driver.ErrAddress},
		{"unknown property", bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeUnknownProperty, driver.ErrAddress},
		{"invalid array index", bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidArrayIndex, driver.ErrAddress},
		{"write denied", bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeWriteAccessDenied, driver.ErrPermission},
		{"read denied", bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeReadAccessDenied, driver.ErrPermission},
		{"value out of range", bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeValueOutOfRange, driver.ErrAddress},
		{"invalid data type", bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidDataType, driver.ErrAddress},
		{"resources", bacnetcodec.ErrorClassResources, 0, driver.ErrDisconnected},
		{"security", bacnetcodec.ErrorClassSecurity, 0, driver.ErrPermission},
		{"unmapped", 99, 0, driver.ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apdu := bacnetcodec.BuildError(bacnetcodec.ServiceReadProperty, 1, tc.class, tc.code)
			c := &connState{cfg: driver.OpenConfig{Device: model.Device{ID: "dev"}}}
			plan := pointPlan{objType: bacnetcodec.ObjectAnalogInput, instance: 1,
				property: bacnetcodec.PropPresentValue}
			err := c.apduError("read", apdu, plan)
			if got := driver.KindOf(err); got != tc.want {
				t.Fatalf("kind = %s, want %s (%v)", got, tc.want, err)
			}
		})
	}
}

func TestAPDUErrorRejectAndAbort(t *testing.T) {
	c := &connState{cfg: driver.OpenConfig{Device: model.Device{ID: "dev"}}}
	plan := pointPlan{objType: bacnetcodec.ObjectAnalogInput, instance: 1,
		property: bacnetcodec.PropPresentValue}

	reject := bacnetcodec.BuildReject(1, bacnetcodec.RejectReasonInvalidAPDU)
	if err := c.apduError("read", reject, plan); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("reject kind = %s", driver.KindOf(err))
	}
	abort := bacnetcodec.BuildAbort(false, 1, 0)
	if err := c.apduError("read", abort, plan); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("abort kind = %s", driver.KindOf(err))
	}
	// 一个既不是 Error/Reject/Abort 的 APDU 也要给出协议错误，而不是 panic。
	if err := c.apduError("read", []byte{0xF0}, plan); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("unknown APDU kind = %s", driver.KindOf(err))
	}
}

// toValue 必须能覆盖模拟量、二进制、多状态、字符串、字节串与带符号数。
func TestToValueConversions(t *testing.T) {
	mk := func(dt model.DataType) pointPlan {
		return pointPlan{ref: driver.PointRef{ID: "p", DataType: dt}}
	}
	cases := []struct {
		name  string
		plan  pointPlan
		tag   byte
		u     uint64
		f     float64
		b     []byte
		bl    bool
		check func(t *testing.T, v model.Value)
	}{
		{"real->float32", mk(model.TypeFloat32), bacnetcodec.AppReal, 0, 3.5, nil, false,
			func(t *testing.T, v model.Value) {
				if v.Num != 3.5 {
					t.Fatalf("num=%v", v.Num)
				}
			}},
		{"double->float64", mk(model.TypeFloat64), bacnetcodec.AppDouble, 0, 2.25, nil, false,
			func(t *testing.T, v model.Value) {
				if v.Num != 2.25 {
					t.Fatalf("num=%v", v.Num)
				}
			}},
		{"unsigned->uint32", mk(model.TypeUint32), bacnetcodec.AppUnsigned, 7, 0, nil, false,
			func(t *testing.T, v model.Value) {
				if v.Num != 7 {
					t.Fatalf("num=%v", v.Num)
				}
			}},
		{"enumerated->bool true", mk(model.TypeBool), bacnetcodec.AppEnumerated, 2, 0, nil, false,
			func(t *testing.T, v model.Value) {
				if !v.Bool {
					t.Fatal("want true")
				}
			}},
		{"enumerated->bool false", mk(model.TypeBool), bacnetcodec.AppEnumerated, 0, 0, nil, false,
			func(t *testing.T, v model.Value) {
				if v.Bool {
					t.Fatal("want false")
				}
			}},
		{"boolean->bool", mk(model.TypeBool), bacnetcodec.AppBoolean, 0, 0, nil, true,
			func(t *testing.T, v model.Value) {
				if !v.Bool {
					t.Fatal("want true")
				}
			}},
		{"signed->float32", mk(model.TypeFloat32), bacnetcodec.AppSigned, ^uint64(3), 0, nil, false,
			func(t *testing.T, v model.Value) {
				if v.Num != -4 {
					t.Fatalf("num=%v", v.Num)
				}
			}},
		{"charstr->string", mk(model.TypeString), bacnetcodec.AppCharacterStr, 0, 0, []byte("AB"), false,
			func(t *testing.T, v model.Value) {
				if v.Text != "AB" {
					t.Fatalf("text=%q", v.Text)
				}
			}},
		{"octet->bytes", mk(model.TypeBytes), bacnetcodec.AppOctetString, 0, 0, []byte{1, 2}, false,
			func(t *testing.T, v model.Value) {
				if len(v.Text) != 2 {
					t.Fatalf("bytes=%q", v.Text)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := toValue(tc.plan, tc.tag, tc.u, tc.f, tc.b, tc.bl)
			if err != nil {
				t.Fatalf("toValue: %v", err)
			}
			tc.check(t, v)
		})
	}

	// 类型不匹配必须报地址错误，而不是把值硬塞进去。
	if _, err := toValue(mk(model.TypeString), bacnetcodec.AppReal, 0, 1, nil, false); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("string point + real tag -> %v", err)
	}
	if _, err := toValue(mk(model.TypeBool), bacnetcodec.AppCharacterStr, 0, 0, []byte("x"), false); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("bool point + charstr tag -> %v", err)
	}
	if _, err := toValue(mk(model.TypeBytes), bacnetcodec.AppReal, 0, 1, nil, false); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("bytes point + real tag -> %v", err)
	}
}

// encodeWriteValue 的分支覆盖：present-value 由对象决定，其它属性由 dataType
// 推断，且类型不符时必须拒绝。
func TestEncodeWriteValue(t *testing.T) {
	cs := func(id, objType, prop string, inst int, dt model.DataType) pointPlan {
		plan, err := planPoint(driver.PointRef{ID: id, DataType: dt, Address: model.Address{
			Kind: model.AddressObject, ObjectType: objType, Instance: inst, Property: prop,
		}})
		if err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
		return plan
	}

	cases := []struct {
		name string
		plan pointPlan
		req  driver.WriteRequest
		want byte
	}{
		{"analog present-value -> real",
			cs("p", "analog-value", "present-value", 1, model.TypeFloat32),
			driver.WriteRequest{PointID: "p", DataType: model.TypeFloat32, Value: model.NumValue(model.TypeFloat32, 1.5)},
			bacnetcodec.AppReal},
		{"binary present-value -> enumerated",
			cs("p", "binary-value", "present-value", 1, model.TypeBool),
			driver.WriteRequest{PointID: "p", DataType: model.TypeBool, Value: model.BoolValue(true)},
			bacnetcodec.AppEnumerated},
		{"multistate present-value -> unsigned",
			cs("p", "multi-state-value", "present-value", 1, model.TypeUint32),
			driver.WriteRequest{PointID: "p", DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 3)},
			bacnetcodec.AppUnsigned},
		{"string present-value -> charstr",
			cs("p", "character-string", "present-value", 1, model.TypeString),
			driver.WriteRequest{PointID: "p", DataType: model.TypeString, Value: model.TextValue(model.TypeString, "hi")},
			bacnetcodec.AppCharacterStr},
		{"bool property -> boolean",
			cs("p", "analog-value", "object-name", 1, model.TypeBool),
			driver.WriteRequest{PointID: "p", DataType: model.TypeBool, Value: model.BoolValue(false)},
			bacnetcodec.AppBoolean},
		{"string property -> charstr",
			cs("p", "analog-value", "object-name", 1, model.TypeString),
			driver.WriteRequest{PointID: "p", DataType: model.TypeString, Value: model.TextValue(model.TypeString, "n")},
			bacnetcodec.AppCharacterStr},
		{"bytes property -> octet",
			cs("p", "analog-value", "object-name", 1, model.TypeBytes),
			driver.WriteRequest{PointID: "p", DataType: model.TypeBytes, Value: model.BytesValue([]byte{1})},
			bacnetcodec.AppOctetString},
		{"double property -> double",
			cs("p", "analog-value", "object-name", 1, model.TypeFloat64),
			driver.WriteRequest{PointID: "p", DataType: model.TypeFloat64, Value: model.NumValue(model.TypeFloat64, 2)},
			bacnetcodec.AppDouble},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := encodeWriteValue(tc.plan, tc.req)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(out) == 0 || out[0]>>4 != tc.want {
				t.Fatalf("tag = 0x%02X, want tag %d", out[0]>>4, tc.want)
			}
		})
	}

	// 类型不符必须报错。
	bad := []struct {
		name string
		plan pointPlan
		req  driver.WriteRequest
	}{
		{"string to real", cs("p", "analog-value", "present-value", 1, model.TypeString),
			driver.WriteRequest{PointID: "p", DataType: model.TypeString, Value: model.TextValue(model.TypeString, "x")}},
		{"negative to unsigned", cs("p", "multi-state-value", "present-value", 1, model.TypeUint32),
			driver.WriteRequest{PointID: "p", DataType: model.TypeUint32, Value: model.NumValue(model.TypeFloat64, -1)}},
	}
	for _, tc := range bad {
		t.Run("reject "+tc.name, func(t *testing.T) {
			if _, err := encodeWriteValue(tc.plan, tc.req); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
