package bacnet

import (
	"context"
	"testing"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/model"
)

// writeReq 构造一次写入请求。
func writeReq(id, objType string, instance int, property string, dt model.DataType, val model.Value) driver.WriteRequest {
	return driver.WriteRequest{
		PointID:  id,
		Address:  model.Address{Kind: model.AddressObject, ObjectType: objType, Instance: instance, Property: property},
		DataType: dt,
		Value:    val,
	}
}

// ---------------------------------------------------------------------------
// 编码分支
// ---------------------------------------------------------------------------

func TestEncodeWriteValueDouble(t *testing.T) {
	// float64 + 非 present-value 属性 → DOUBLE（8 字节）应用值。
	// present-value 的对象语义会强制 REAL；DOUBLE 只在其它数值属性上出现。
	p := objRef("x", "analog-value", 2, "resolution", model.TypeFloat64)
	plan, err := planPoint(p)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got, err := encodeWriteValue(plan, writeReq("x", "analog-value", 2, "resolution", model.TypeFloat64,
		model.NumValue(model.TypeFloat64, 3.25)))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tag, _, f, _, _, err := bacnetcodec.DecodeApplicationValue(got)
	if err != nil || tag != bacnetcodec.AppDouble || f != 3.25 {
		t.Fatalf("double = tag %d f=%v err=%v", tag, f, err)
	}
}

func TestEncodeWriteValueOctetBytes(t *testing.T) {
	// bytes + 非 present-value 属性 → OCTET STRING。
	p := objRef("x", "character-string", 1, "description", model.TypeBytes)
	plan, _ := planPoint(p)
	got, err := encodeWriteValue(plan, writeReq("x", "character-string", 1, "description", model.TypeBytes,
		model.BytesValue([]byte{0xDE, 0xAD})))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tag, _, _, b, _, err := bacnetcodec.DecodeApplicationValue(got)
	if err != nil || tag != bacnetcodec.AppOctetString || string(b) != string([]byte{0xDE, 0xAD}) {
		t.Fatalf("octet = tag %d b=% X err=%v", tag, b, err)
	}
}

func TestEncodeWriteValueUnsignedMultiState(t *testing.T) {
	// multi-state-value 的 present-value 是 UNSIGNED。
	srv := newSim(t)
	if err := srv.SetValue("multi-state-value", 1, "present-value", uint64(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := openSim(t, srv)
	p := objRef("x", "multi-state-value", 1, "present-value", model.TypeUint32)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: p.Address, DataType: model.TypeUint32,
		Value: model.NumValue(model.TypeUint32, 2),
	}})
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	if v, ok := srv.Value("multi-state-value", 1, "present-value"); !ok || v.(uint64) != 2 {
		t.Fatalf("sim value = %v", v)
	}
}

func TestEncodeWriteValueBadNumeric(t *testing.T) {
	p := objRef("x", "analog-value", 2, "present-value", model.TypeFloat32)
	plan, _ := planPoint(p)
	// 把字符串当数值写：本地就拒绝，不发报文。
	_, err := encodeWriteValue(plan, writeReq("x", "analog-value", 2, "present-value", model.TypeString,
		model.TextValue(model.TypeString, "abc")))
	if err == nil {
		t.Fatal("string into numeric must fail")
	}
}

func TestEncodeWriteValueBadStringBytesIntoReal(t *testing.T) {
	// bytes 属性写进 REAL：拒绝。
	p := objRef("x", "analog-value", 2, "present-value", model.TypeBytes)
	plan, _ := planPoint(p)
	_, err := encodeWriteValue(plan, writeReq("x", "analog-value", 2, "present-value", model.TypeBytes,
		model.BytesValue([]byte("x"))))
	if err == nil {
		t.Fatal("bytes into real must fail")
	}
}

// ---------------------------------------------------------------------------
// 值解码分支
// ---------------------------------------------------------------------------

func TestToValueBytesFromOctet(t *testing.T) {
	srv := newSim(t)
	if err := srv.SetValue("character-string", 1, "present-value", "abc"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := openSim(t, srv)
	s := readOne(t, c, objRef("bytes", "character-string", 1, "present-value", model.TypeBytes))
	if s.Err != nil {
		t.Fatalf("err: %v", s.Err)
	}
	if string(s.Value.Text) != "abc" {
		t.Fatalf("value = %q", s.Value.Text)
	}
}

func TestToValueSigned(t *testing.T) {
	srv := newSim(t)
	// 用一个 SIGNED 属性：multi-state 不行（ UNSIGNED）。用 analog-value 写 DOUBLE。
	if err := srv.SetValue("analog-value", 5, "present-value", float64(-7.5)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := openSim(t, srv)
	s := readOne(t, c, objRef("x", "analog-value", 5, "present-value", model.TypeFloat32))
	if s.Err != nil || s.Value.Num != -7.5 {
		t.Fatalf("signed = %+v err=%v", s.Value, s.Err)
	}
}

func TestToValueFromDoubleTag(t *testing.T) {
	srv := newSim(t)
	if err := srv.SetValue("analog-value", 6, "present-value", float64(9.5)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := openSim(t, srv)
	s := readOne(t, c, objRef("x", "analog-value", 6, "present-value", model.TypeFloat64))
	if s.Err != nil || s.Value.Num != 9.5 {
		t.Fatalf("double = %+v err=%v", s.Value, s.Err)
	}
}

func TestToValueStringTagIntoBool(t *testing.T) {
	// 字符串属性配成 bool：标签不匹配 → 地址错误。
	c := openSim(t, newSim(t))
	s := readOne(t, c, objRef("x", "analog-input", 1, "object-name", model.TypeBool))
	if s.Err == nil || driver.KindOf(s.Err) != driver.ErrAddress {
		t.Fatalf("err = %v", s.Err)
	}
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func TestMapBacnetErrorClasses(t *testing.T) {
	plan, err := planPoint(objRef("x", "analog-input", 1, "present-value", model.TypeFloat32))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cases := []struct {
		class, code byte
		want        driver.ErrorKind
	}{
		{bacnetcodec.ErrorClassObject, bacnetcodec.ErrorCodeUnknownObject, driver.ErrAddress},
		{bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeUnknownProperty, driver.ErrAddress},
		{bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeWriteAccessDenied, driver.ErrPermission},
		{bacnetcodec.ErrorClassSecurity, bacnetcodec.ErrorCodeOther, driver.ErrPermission},
		{bacnetcodec.ErrorClassResources, bacnetcodec.ErrorCodeOther, driver.ErrDisconnected},
		{bacnetcodec.ErrorClassVT, bacnetcodec.ErrorCodeOther, driver.ErrProtocol},
	}
	for _, tc := range cases {
		err := mapBacnetError("read", "dev", plan, tc.class, tc.code)
		if driver.KindOf(err) != tc.want {
			t.Fatalf("class %d code %d: kind = %s, want %s", tc.class, tc.code, driver.KindOf(err), tc.want)
		}
	}
}

func TestApduErrorRejectsAndAborts(t *testing.T) {
	c := &connState{cfg: driver.OpenConfig{Device: model.Device{ID: "dev"}}}
	plan, _ := planPoint(objRef("x", "analog-input", 1, "present-value", model.TypeFloat32))

	reject := bacnetcodec.BuildReject(1, bacnetcodec.RejectReasonBufferOverflow)
	if err := c.apduError("read", reject, plan); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("reject kind = %s", driver.KindOf(err))
	}
	abort := bacnetcodec.BuildAbort(true, 2, 0)
	if err := c.apduError("read", abort, plan); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("abort kind = %s", driver.KindOf(err))
	}
}

func TestApduErrorUnexpected(t *testing.T) {
	c := &connState{cfg: driver.OpenConfig{Device: model.Device{ID: "dev"}}}
	plan, _ := planPoint(objRef("x", "analog-input", 1, "present-value", model.TypeFloat32))
	// SimpleACK 在读路径里算「意外 APDU」。
	simple := bacnetcodec.BuildSimpleACK(bacnetcodec.ServiceReadProperty, 1)
	err := c.apduError("read", simple, plan)
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// 会话细节
// ---------------------------------------------------------------------------

func TestInvokeIDWrapsAround(t *testing.T) {
	c := &connState{}
	seen := map[byte]bool{}
	for i := 0; i < 300; i++ {
		id := c.nextInvoke()
		if id == 0 {
			t.Fatalf("invoke ID wrapped to 0")
		}
		if seen[id] {
			t.Fatalf("invoke ID %d repeated before full cycle", id)
		}
		seen[id] = true
		if len(seen) == 255 {
			seen = map[byte]bool{}
		}
	}
}

func TestAttemptDeadlineRespectsContext(t *testing.T) {
	c := &connState{cfg: driver.OpenConfig{}}
	// 通道超时 10 秒，但 context 只剩 1 毫秒 → 取更小者。
	c.cfg.Channel.Timeout = model.Duration(10 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(3 * time.Millisecond)
	if d := c.attemptDeadline(ctx); d.After(time.Now().Add(time.Second)) {
		t.Fatalf("deadline ignored context: %v", d)
	}
}

func TestReadErrorContextDone(t *testing.T) {
	c := &connState{cfg: driver.OpenConfig{Device: model.Device{ID: "dev"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 用超时错误模拟一次读失败；readError 应识别 context 已取消。
	err := c.readError("read", timeoutErr(), ctx)
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %s", driver.KindOf(err))
	}
}

// timeoutErr 返回一个模拟网络超时的 error。
func timeoutErr() error {
	return &timeoutError{}
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
