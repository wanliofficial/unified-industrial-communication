package bacnet

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// talk 向从站发一条请求并读回整包。
func talk(t *testing.T, srv *Server, apdu []byte) []byte {
	t.Helper()
	pc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portOf(t, srv.Addr())})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pc.Close()
	if _, err := pc.Write(bacnetcodec.BuildMessage(1, bacnetcodec.NPDUControlExpectReply, apdu)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := pc.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := pc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		t.Fatalf("scan port: %v", err)
	}
	return port
}

func readAck(t *testing.T, srv *Server, apdu []byte) (bacnetcodec.APDUKind, byte, byte, []byte) {
	t.Helper()
	datagram := talk(t, srv, apdu)
	got, _, err := bacnetcodec.ParseMessage(datagram)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	kind, _, invoke, payload, err := bacnetcodec.ParseACK(got)
	if err != nil {
		t.Fatalf("parse ack: %v", err)
	}
	return kind, invoke, got[2], payload
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func TestServerReadsDefaultProperties(t *testing.T) {
	s := newServer(t)

	req, err := bacnetcodec.ReadPropertyRequest(7, bacnetcodec.ObjectAnalogInput, 1,
		bacnetcodec.PropPresentValue, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	kind, invoke, svc, payload := readAck(t, s, req)
	if kind != bacnetcodec.APDUKindComplexACK || invoke != 7 || svc != bacnetcodec.ServiceReadProperty {
		t.Fatalf("ack = %v/%d/%d", kind, invoke, svc)
	}
	appValue, has, err := bacnetcodec.ReadPropertyACK(payload)
	if err != nil || !has {
		t.Fatalf("ReadPropertyACK: %v %v", has, err)
	}
	tag, _, f, _, _, err := bacnetcodec.DecodeApplicationValue(appValue)
	if err != nil || tag != bacnetcodec.AppReal || f != 42.5 {
		t.Fatalf("value = tag %d f=%v err=%v", tag, f, err)
	}
}

func TestServerReadsStringProperty(t *testing.T) {
	s := newServer(t)
	req, _ := bacnetcodec.ReadPropertyRequest(1, bacnetcodec.ObjectAnalogInput, 1,
		bacnetcodec.PropObjectName, 0)
	_, _, _, payload := readAck(t, s, req)
	appValue, _, err := bacnetcodec.ReadPropertyACK(payload)
	if err != nil {
		t.Fatalf("ReadPropertyACK: %v", err)
	}
	tag, _, _, b, _, err := bacnetcodec.DecodeApplicationValue(appValue)
	if err != nil || tag != bacnetcodec.AppCharacterStr || string(b) != "AI-1" {
		t.Fatalf("object-name = %q err=%v", b, err)
	}
}

func TestServerUnknownObjectReturnsError(t *testing.T) {
	s := newServer(t)
	req, _ := bacnetcodec.ReadPropertyRequest(3, bacnetcodec.ObjectAnalogInput, 9,
		bacnetcodec.PropPresentValue, 0)
	datagram := talk(t, s, req)
	apdu, _, err := bacnetcodec.ParseMessage(datagram)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindError {
		t.Fatalf("APDU = % X", apdu)
	}
	_, class, code, err := bacnetcodec.ParseError(apdu)
	if err != nil {
		t.Fatalf("ParseError: %v", err)
	}
	if class != bacnetcodec.ErrorClassObject || code != bacnetcodec.ErrorCodeUnknownObject {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerUnknownPropertyReturnsError(t *testing.T) {
	s := newServer(t)
	req, _ := bacnetcodec.ReadPropertyRequest(3, bacnetcodec.ObjectAnalogInput, 1,
		12345, 0)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	_, class, code, _ := bacnetcodec.ParseError(apdu)
	if class != bacnetcodec.ErrorClassProperty || code != bacnetcodec.ErrorCodeUnknownProperty {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerArrayElementRead(t *testing.T) {
	s := newServer(t)
	req, _ := bacnetcodec.ReadPropertyRequest(5, bacnetcodec.ObjectAnalogValue, 2,
		bacnetcodec.PropPriorityArray, 1)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindComplexACK {
		t.Fatalf("APDU = % X", apdu)
	}
}

func TestServerArrayIndexOutOfRange(t *testing.T) {
	s := newServer(t)
	req, _ := bacnetcodec.ReadPropertyRequest(5, bacnetcodec.ObjectAnalogValue, 2,
		bacnetcodec.PropPriorityArray, 99)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	_, class, code, _ := bacnetcodec.ParseError(apdu)
	if class != bacnetcodec.ErrorClassProperty || code != bacnetcodec.ErrorCodeInvalidArrayIndex {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerArrayIndexOnScalar(t *testing.T) {
	s := newServer(t)
	// present-value 不是数组属性：带下标读取必须回 invalid-array-index，
	// 而不是静默忽略下标返回整个属性。
	req, _ := bacnetcodec.ReadPropertyRequest(5, bacnetcodec.ObjectAnalogInput, 1,
		bacnetcodec.PropPresentValue, 3)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	_, class, code, _ := bacnetcodec.ParseError(apdu)
	if class != bacnetcodec.ErrorClassProperty || code != bacnetcodec.ErrorCodeInvalidArrayIndex {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerArrayIndexZeroReturnsLength(t *testing.T) {
	s := newServer(t)
	req, _ := bacnetcodec.ReadPropertyRequest(6, bacnetcodec.ObjectAnalogValue, 2,
		bacnetcodec.PropPriorityArray, 0)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindComplexACK {
		t.Fatalf("APDU = % X", apdu)
	}
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func TestServerWriteAnalog(t *testing.T) {
	s := newServer(t)
	val, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 12.5, nil, false)
	req, err := bacnetcodec.WritePropertyRequest(4, bacnetcodec.ObjectAnalogValue, 2,
		bacnetcodec.PropPresentValue, 0, 0, val)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindSimpleACK {
		t.Fatalf("APDU = % X", apdu)
	}
	if v, ok := s.Value("analog-value", 2, "present-value"); !ok || v.(float64) != 12.5 {
		t.Fatalf("stored = %v", v)
	}
}

func TestServerWriteReadOnlyObject(t *testing.T) {
	s := newServer(t)
	val, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppReal, 0, 1, nil, false)
	req, _ := bacnetcodec.WritePropertyRequest(4, bacnetcodec.ObjectAnalogInput, 1,
		bacnetcodec.PropPresentValue, 0, 0, val)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	_, class, code, _ := bacnetcodec.ParseError(apdu)
	if class != bacnetcodec.ErrorClassProperty || code != bacnetcodec.ErrorCodeWriteAccessDenied {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerWriteInvalidDataType(t *testing.T) {
	s := newServer(t)
	// 往 REAL 属性写字符串：回 invalid-data-type，而不是存一个读不回来的值。
	val, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppCharacterStr, 0, 0, []byte("abc"), false)
	req, _ := bacnetcodec.WritePropertyRequest(4, bacnetcodec.ObjectAnalogValue, 2,
		bacnetcodec.PropPresentValue, 0, 0, val)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	_, class, code, _ := bacnetcodec.ParseError(apdu)
	if class != bacnetcodec.ErrorClassProperty || code != bacnetcodec.ErrorCodeInvalidDataType {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerWriteArrayElement(t *testing.T) {
	s := newServer(t)
	val, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppNull, 0, 0, nil, false)
	req, _ := bacnetcodec.WritePropertyRequest(4, bacnetcodec.ObjectAnalogValue, 2,
		bacnetcodec.PropPriorityArray, 3, 0, val)
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindSimpleACK {
		t.Fatalf("APDU = % X", apdu)
	}
}

// ---------------------------------------------------------------------------
// 不支持的报文
// ---------------------------------------------------------------------------

func TestServerRejectsSegmentedRequest(t *testing.T) {
	s := newServer(t)
	// 首字节 bit3 = 分段：ParseConfirmedRequest 报分段，驱动会回 Reject。
	req, _ := bacnetcodec.ReadPropertyRequest(9, bacnetcodec.ObjectAnalogInput, 1,
		bacnetcodec.PropPresentValue, 0)
	req[0] |= 0x08
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindAbort {
		t.Fatalf("APDU = % X", apdu)
	}
	_, reason, _ := bacnetcodec.ParseAbort(apdu)
	if reason != bacnetcodec.AbortReasonSegmentationNotSup {
		t.Fatalf("reason = 0x%02X", reason)
	}
}

func TestServerRejectsUnknownService(t *testing.T) {
	s := newServer(t)
	// 0x0E = ReadPropertyMultiple，从站明确不实现。
	req := []byte{bacnetcodec.APDUConfirmedRequest, 0x00, 9, 0x0E}
	datagram := talk(t, s, req)
	apdu, _, _ := bacnetcodec.ParseMessage(datagram)
	if bacnetcodec.ClassifyAPDU(apdu[0]) != bacnetcodec.APDUKindError {
		t.Fatalf("APDU = % X", apdu)
	}
	_, class, code, _ := bacnetcodec.ParseError(apdu)
	if class != bacnetcodec.ErrorClassServices || code != bacnetcodec.ErrorCodeOther {
		t.Fatalf("error = class %d code %d", class, code)
	}
}

func TestServerIgnoresUnparseableDatagram(t *testing.T) {
	s := newServer(t)
	pc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portOf(t, s.Addr())})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer pc.Close()
	if _, err := pc.Write([]byte{0x99, 0x01}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 256)
	if _, err := pc.Read(buf); err == nil {
		t.Fatal("no response expected for garbage")
	}
}

// ---------------------------------------------------------------------------
// Set / Value / Objects
// ---------------------------------------------------------------------------

func TestSetValueAndValue(t *testing.T) {
	s := newServer(t)
	if err := s.SetValue("analog-input", 1, "present-value", 1.5); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, ok := s.Value("analog-input", 1, "present-value"); !ok || v.(float64) != 1.5 {
		t.Fatalf("Value = %v ok=%v", v, ok)
	}
	if err := s.SetValue("binary-input", 2, "present-value", true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, ok := s.Value("binary-input", 2, "present-value"); !ok || !v.(bool) {
		t.Fatalf("Value = %v ok=%v", v, ok)
	}
	if err := s.SetValue("character-string", 2, "present-value", "X"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, ok := s.Value("character-string", 2, "present-value"); !ok || v.(string) != "X" {
		t.Fatalf("Value = %v ok=%v", v, ok)
	}
}

func TestSetValueErrors(t *testing.T) {
	s := newServer(t)
	if err := s.SetValue("no-such", 1, "present-value", 1.0); err == nil {
		t.Fatal("unknown object accepted")
	}
	if err := s.SetValue("analog-input", 1, "no-such", 1.0); err == nil {
		t.Fatal("unknown property accepted")
	}
	if err := s.SetValue("analog-input", 1, "present-value", struct{}{}); err == nil {
		t.Fatal("unsupported type accepted")
	}
}

func TestValueMisses(t *testing.T) {
	s := newServer(t)
	if _, ok := s.Value("analog-input", 9, "present-value"); ok {
		t.Fatal("unknown object reported as present")
	}
	if _, ok := s.Value("analog-input", 1, "no-such"); ok {
		t.Fatal("unknown property reported as present")
	}
}

func TestAutoIncrement(t *testing.T) {
	s := newServer(t)
	before, ok := s.Value("analog-value", 3, "present-value")
	if !ok {
		t.Fatal("missing default")
	}
	// 等一秒触发自增。
	time.Sleep(1100 * time.Millisecond)
	after, _ := s.Value("analog-value", 3, "present-value")
	if after.(float64) <= before.(float64) {
		t.Fatalf("counter did not increase: %v -> %v", before, after)
	}
	s.DisableAutoIncrement()
	time.Sleep(1100 * time.Millisecond)
	fixed, _ := s.Value("analog-value", 3, "present-value")
	time.Sleep(1100 * time.Millisecond)
	stable, _ := s.Value("analog-value", 3, "present-value")
	if fixed.(float64) != stable.(float64) {
		t.Fatalf("DisableAutoIncrement did not stop: %v -> %v", fixed, stable)
	}
}

func TestObjectsLists(t *testing.T) {
	s := newServer(t)
	objs := s.Objects()
	if len(objs) < 6 {
		t.Fatalf("%d objects", len(objs))
	}
	for i := 1; i < len(objs); i++ {
		if strings.Compare(objs[i-1], objs[i]) > 0 {
			t.Fatalf("not sorted: %v", objs)
		}
	}
}

func TestPropEncodeErrors(t *testing.T) {
	s := newServer(t)
	// NULL 槽位（priority-array 的空位）接受任意值写入——写完后槽位 tag
	// 变成值的类型，这正是「写 7.5 到 priority-array[1]」的标准形态；
	// NULL 写入保持 NULL（释放语义）。
	if err := s.setProp(0, 0, 0, &prop{tag: bacnetcodec.AppNull}); err != nil {
		t.Fatalf("set null: %v", err)
	}
	p := &prop{tag: bacnetcodec.AppNull}
	out, err := coerce(p, bacnetcodec.AppReal, 0, 1, nil, false)
	if err != nil {
		t.Fatalf("real value into empty (null) slot must be accepted: %v", err)
	}
	if out.tag != bacnetcodec.AppReal {
		t.Fatalf("slot tag = %d, want real after numeric write", out.tag)
	}
	if _, err := coerce(&prop{tag: 0x0F}, bacnetcodec.AppReal, 0, 1, nil, false); err == nil {
		t.Fatal("unknown property tag accepted")
	}
}

func TestPropCloneIsDeep(t *testing.T) {
	p := &prop{tag: bacnetcodec.AppOctetString, bytes: []byte{1, 2, 3}}
	c := p.clone()
	c.bytes[0] = 9
	if p.bytes[0] == 9 {
		t.Fatal("clone shares the byte slice")
	}
}

func TestSetAnalogBinaryString(t *testing.T) {
	s := newServer(t)
	if err := s.SetAnalog(1, 3.5); err != nil {
		t.Fatalf("SetAnalog: %v", err)
	}
	if v, _ := s.Value("analog-input", 1, "present-value"); v.(float64) != 3.5 {
		t.Fatalf("SetAnalog = %v", v)
	}
	if err := s.SetAnalogValue(2, 7.5); err != nil {
		t.Fatalf("SetAnalogValue: %v", err)
	}
	if v, _ := s.Value("analog-value", 2, "present-value"); v.(float64) != 7.5 {
		t.Fatalf("SetAnalogValue = %v", v)
	}
	if err := s.SetBinary(1, true); err != nil {
		t.Fatalf("SetBinary: %v", err)
	}
	if v, _ := s.Value("binary-input", 1, "present-value"); v.(uint64) != 1 {
		t.Fatalf("SetBinary = %v", v)
	}
	if err := s.SetString(1, "PUMP-09"); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	if v, _ := s.Value("character-string", 1, "present-value"); v.(string) != "PUMP-09" {
		t.Fatalf("SetString = %v", v)
	}
}

func TestSetValueInt32AndBytes(t *testing.T) {
	s := newServer(t)
	if err := s.SetValue("analog-value", 7, "present-value", int32(-3)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// SIGNED 存储：Value 回读 uint64(int64(-3))。
	neg := int64(-3)
	want := uint64(neg)
	if v, _ := s.Value("analog-value", 7, "present-value"); v.(uint64) != want {
		t.Fatalf("int32 = %v", v)
	}
	if err := s.SetValue("character-string", 1, "present-value", []byte{0x01, 0x02}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := s.Value("character-string", 1, "present-value"); string(v.([]byte)) != string([]byte{0x01, 0x02}) {
		t.Fatalf("bytes = %v", v)
	}
}

func TestCoerceBooleanAndSigned(t *testing.T) {
	// 写 BOOLEAN 属性：布尔值/枚举值都要收敛。
	s := newServer(t)
	if err := s.SetValue("binary-value", 2, "out-of-service", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	val, _ := bacnetcodec.EncodeApplicationValue(bacnetcodec.AppBoolean, 0, 0, nil, true)
	got, err := coerce(&prop{tag: bacnetcodec.AppBoolean}, bacnetcodec.AppBoolean, 0, 0, nil, true)
	if err != nil || !got.boolean {
		t.Fatalf("coerce bool = %+v err=%v", got, err)
	}
	_ = val
	if _, err := coerce(&prop{tag: bacnetcodec.AppBoolean}, bacnetcodec.AppUnsigned, 5, 0, nil, false); err != nil {
		t.Fatalf("coerce enum->bool: %v", err)
	}
	if _, err := coerce(&prop{tag: bacnetcodec.AppSigned}, bacnetcodec.AppReal, 0, 2.5, nil, false); err == nil {
		t.Fatalf("real->signed must be rejected")
	}
	if _, err := coerce(&prop{tag: bacnetcodec.AppSigned}, bacnetcodec.AppSigned, 0, 0, nil, false); err != nil {
		t.Fatalf("coerce signed->signed: %v", err)
	}
	if _, err := coerce(&prop{tag: bacnetcodec.AppCharacterStr}, bacnetcodec.AppOctetString, 0, 0, []byte("x"), false); err == nil {
		t.Fatalf("octet->string must be rejected")
	}
	if _, err := coerce(&prop{tag: bacnetcodec.AppOctetString}, bacnetcodec.AppCharacterStr, 0, 0, []byte("x"), false); err != nil {
		t.Fatalf("coerce string->octet: %v", err)
	}
	if _, err := coerce(&prop{tag: bacnetcodec.AppUnsigned}, bacnetcodec.AppBoolean, 0, 0, nil, true); err != nil {
		t.Fatalf("coerce bool->unsigned: %v", err)
	}
}

func TestPropEncodeVariants(t *testing.T) {
	cases := []struct {
		tag byte
		p   *prop
	}{
		{bacnetcodec.AppNull, &prop{tag: bacnetcodec.AppNull}},
		{bacnetcodec.AppBoolean, &prop{tag: bacnetcodec.AppBoolean, boolean: true}},
		{bacnetcodec.AppReal, &prop{tag: bacnetcodec.AppReal, f: 1}},
		{bacnetcodec.AppDouble, &prop{tag: bacnetcodec.AppDouble, f: 2}},
		{bacnetcodec.AppCharacterStr, &prop{tag: bacnetcodec.AppCharacterStr, text: "hi"}},
		{bacnetcodec.AppOctetString, &prop{tag: bacnetcodec.AppOctetString, bytes: []byte{1}}},
		{bacnetcodec.AppBitString, &prop{tag: bacnetcodec.AppBitString, bytes: []byte{0x80}}},
		{bacnetcodec.AppEnumerated, &prop{tag: bacnetcodec.AppEnumerated, u: 3}},
		{bacnetcodec.AppUnsigned, &prop{tag: bacnetcodec.AppUnsigned, u: 3}},
	}
	for _, tc := range cases {
		if _, err := tc.p.encode(); err != nil {
			t.Fatalf("encode tag %d: %v", tc.tag, err)
		}
	}
}

func TestHandleIgnoresNonRequest(t *testing.T) {
	s := newServer(t)
	if got := s.handle([]byte{0x99, 0x01}); got != nil {
		t.Fatalf("expected nil, got % X", got)
	}
	if got := s.handle(nil); got != nil {
		t.Fatalf("expected nil for empty, got % X", got)
	}
}
