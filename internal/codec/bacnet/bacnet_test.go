package bacnet

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// BVLC / NPDU
// ---------------------------------------------------------------------------

func TestBuildAndParseMessage(t *testing.T) {
	apdu := []byte{APDUComplexACK, ServiceReadProperty, 1, 0xAA}
	msg := BuildMessage(1, NPDUControlExpectReply, apdu)
	if msg[0] != 0x81 || msg[1] != BVLCOriginalUnicast {
		t.Fatalf("BVLC header wrong: % X", msg[:2])
	}
	if got := int(binary.BigEndian.Uint16(msg[2:4])); got != len(apdu)+2 {
		t.Fatalf("BVLC length = %d, want NPDU+APDU = %d", got, len(apdu)+2)
	}
	got, ctrl, err := ParseMessage(msg)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if ctrl != NPDUControlExpectReply {
		t.Fatalf("control = 0x%02X", ctrl)
	}
	if !bytes.Equal(got, apdu) {
		t.Fatalf("apdu = % X, want % X", got, apdu)
	}
}

func TestParseMessageForwardedNPDU(t *testing.T) {
	apdu := []byte{APDUComplexACK, ServiceReadProperty, 1}
	// BVLC(4) + 转发信息(6)：原始 IP 192.168.1.10 + 原始端口 0xBAC0
	msg := []byte{0x81, BVLCForwardedNPDU, 0, 0, 192, 168, 1, 10, 0xBA, 0xC0, 1, NPDUControlExpectReply}
	msg = append(msg, apdu...)
	binary.BigEndian.PutUint16(msg[2:4], uint16(len(msg)-4))
	got, _, err := ParseMessage(msg)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if !bytes.Equal(got, apdu) {
		t.Fatalf("apdu = % X", got)
	}
}

func TestParseMessageErrors(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
	}{
		{"empty", nil},
		{"short bvlc", []byte{0x81}},
		{"bad bvlc type", []byte{0x82, BVLCOriginalUnicast, 0, 2, 1, 0x04}},
		{"length exceeds", []byte{0x81, BVLCOriginalUnicast, 0, 100, 1, 0x04}},
		{"unsupported function", []byte{0x81, 0x0C, 0, 2, 1, 0x04}},
		{"short forwarded", []byte{0x81, BVLCForwardedNPDU, 0, 2, 1, 0x04}},
		{"npdu too short", []byte{0x81, BVLCOriginalUnicast, 0, 1, 1}},
		{"bad npdu version", []byte{0x81, BVLCOriginalUnicast, 0, 2, 2, 0x04}},
		{"destination truncated", []byte{0x81, BVLCOriginalUnicast, 0, 3, 1, 0x20, 9}},
		{"destination address truncated", []byte{0x81, BVLCOriginalUnicast, 0, 4, 1, 0x20, 2, 5}},
		{"source route truncated", []byte{0x81, BVLCOriginalUnicast, 0, 3, 1, 0x08, 9}},
		{"source address truncated", []byte{0x81, BVLCOriginalUnicast, 0, 5, 1, 0x08, 2, 5, 0}},
		{"no apdu", []byte{0x81, BVLCOriginalUnicast, 0, 2, 1, 0x04}},
	}
	for _, tc := range cases {
		if _, _, err := ParseMessage(tc.msg); err == nil {
			t.Fatalf("%s: expected error, got nil (% X)", tc.name, tc.msg)
		}
	}
}

func TestParseMessageDestinationRoute(t *testing.T) {
	apdu := []byte{APDUComplexACK, ServiceReadProperty, 1}
	// version, control(bit5=有目的端), DNET(2 字节), DLEN, DADR(DLEN), Hop Count
	npdu := []byte{1, 0x20, 0x02, 0x01, 0x02, 0xAA, 0xBB, 0x20}
	body := append(npdu, apdu...)
	msg := []byte{0x81, BVLCOriginalUnicast, byte(len(body) >> 8), byte(len(body))}
	msg = append(msg, body...)
	got, _, err := ParseMessage(msg)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if !bytes.Equal(got, apdu) {
		t.Fatalf("apdu = % X", got)
	}
}

// control bit1（0x02）是网络优先级，不是「带目的地址」。按 0x02 去跳过
// DNET/DADR 会让经路由器回包的应答整体错位——这条用例把这个位锁住。
func TestParseNPDUPriorityBitIsNotRouting(t *testing.T) {
	apdu := []byte{APDUComplexACK, ServiceReadProperty, 1}
	got, ctrl, err := ParseNPDU(append([]byte{1, 0x02}, apdu...))
	if err != nil {
		t.Fatalf("ParseNPDU: %v", err)
	}
	if ctrl != 0x02 || !bytes.Equal(got, apdu) {
		t.Fatalf("ctrl = 0x%02X, apdu = % X", ctrl, got)
	}
}

func TestParseMessageSourceRoute(t *testing.T) {
	apdu := []byte{APDUComplexACK, ServiceReadProperty, 1}
	// control bit3 = 源路由：SNET(2 字节) + SLEN(1) + SADR(SLEN)
	npdu := []byte{1, 0x08, 0x03, 0x00, 0x02, 0xAA, 0xBB}
	body := append(npdu, apdu...)
	msg := []byte{0x81, BVLCOriginalUnicast, byte(len(body) >> 8), byte(len(body))}
	msg = append(msg, body...)
	got, _, err := ParseMessage(msg)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if !bytes.Equal(got, apdu) {
		t.Fatalf("apdu = % X", got)
	}
}

// ---------------------------------------------------------------------------
// 标签头
// ---------------------------------------------------------------------------

func TestEncodeAppTagHeader(t *testing.T) {
	cases := []struct {
		length int
		want   []byte
	}{
		{0, []byte{0x40}},
		{1, []byte{0x41}},
		{3, []byte{0x43}},
		{4, []byte{0x44}},
		{5, []byte{0x45, 0x05}},
		{255, []byte{0x45, 0xFF}},
		{256, []byte{0x46, 0x01, 0x00}},
		{65535, []byte{0x46, 0xFF, 0xFF}},
		{65536, []byte{0x47, 0x00, 0x01, 0x00, 0x00}},
	}
	for _, tc := range cases {
		got := encodeAppTagHeader(TagReal, tc.length)
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("len %d: tag header = % X, want % X", tc.length, got, tc.want)
		}
	}
}

func TestEncodeCtxTagHeader(t *testing.T) {
	cases := []struct {
		length int
		want   []byte
		ok     bool
	}{
		{0, []byte{0x18}, true},
		{1, []byte{0x19}, true},
		{3, []byte{0x1B}, true},
		{4, []byte{0x1C}, true},
		{5, []byte{0x1D, 0x05}, true},
		{255, []byte{0x1D, 0xFF}, true},
		{256, nil, false},
	}
	for _, tc := range cases {
		got, ok := encodeCtxTagHeader(1, tc.length)
		if ok != tc.ok {
			t.Fatalf("len %d: ok = %v, want %v", tc.length, ok, tc.ok)
		}
		if ok && !bytes.Equal(got, tc.want) {
			t.Fatalf("len %d: header = % X, want % X", tc.length, got, tc.want)
		}
	}
}

func TestContextOpeningClosing(t *testing.T) {
	if got := contextOpening(3); !bytes.Equal(got, []byte{0x3E}) {
		t.Fatalf("opening tag 3 = % X, want 3E", got)
	}
	if got := contextClosing(3); !bytes.Equal(got, []byte{0x3F}) {
		t.Fatalf("closing tag 3 = % X, want 3F", got)
	}
	if got := contextOpening(0); !bytes.Equal(got, []byte{0x0E}) {
		t.Fatalf("opening tag 0 = % X, want 0E", got)
	}
	if got := contextClosing(0); !bytes.Equal(got, []byte{0x0F}) {
		t.Fatalf("closing tag 0 = % X, want 0F", got)
	}
}

func TestContextTag(t *testing.T) {
	got, err := contextTag(0, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("contextTag: %v", err)
	}
	if want := []byte{0x0C, 1, 2, 3, 4}; !bytes.Equal(got, want) {
		t.Fatalf("contextTag = % X, want % X", got, want)
	}
	got, err = contextTag(1, []byte{0x55})
	if err != nil {
		t.Fatalf("contextTag: %v", err)
	}
	if want := []byte{0x19, 0x55}; !bytes.Equal(got, want) {
		t.Fatalf("contextTag = % X, want % X", got, want)
	}
	if _, err := contextTag(2, make([]byte, 256)); err == nil {
		t.Fatal("256-byte context payload accepted")
	}
}

func TestParseTagHeader(t *testing.T) {
	cases := []struct {
		name    string
		buf     []byte
		tag     byte
		context bool
		length  int
		offset  int
		open    bool
		clos    bool
		boolVal bool
	}{
		{"app null", []byte{0x00}, 0, false, 0, 1, false, false, false},
		{"app real", []byte{0x44, 1, 2, 3, 4}, 4, false, 4, 1, false, false, false},
		{"app ext1", []byte{0x45, 5}, 4, false, 5, 2, false, false, false},
		{"app ext2", []byte{0x46, 1, 0}, 4, false, 256, 3, false, false, false},
		{"app ext4", []byte{0x47, 0, 1, 0, 0}, 4, false, 65536, 5, false, false, false},
		{"app bool true", []byte{0x11}, 1, false, 0, 1, false, false, true},
		{"app bool false", []byte{0x10}, 1, false, 0, 1, false, false, false},
		{"ctx len 0", []byte{0x08}, 0, true, 0, 1, false, false, false},
		{"ctx len 4", []byte{0x0C, 1, 2, 3, 4}, 0, true, 4, 1, false, false, false},
		{"ctx ext1", []byte{0x1D, 5}, 1, true, 5, 2, false, false, false},
		{"ctx opening", []byte{0x3E}, 3, true, 0, 1, true, false, false},
		{"ctx closing", []byte{0x3F}, 3, true, 0, 1, false, true, false},
	}
	for _, tc := range cases {
		h, off, err := parseTagHeader(tc.buf)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if h.tagNumber != tc.tag || h.context != tc.context || h.length != tc.length ||
			off != tc.offset || h.opening != tc.open || h.closing != tc.clos || h.boolValue != tc.boolVal {
			t.Fatalf("%s: got tag=%d ctx=%v len=%d off=%d open=%v close=%v bool=%v",
				tc.name, h.tagNumber, h.context, h.length, off, h.opening, h.closing, h.boolValue)
		}
	}
}

func TestParseTagHeaderTruncated(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x45},       // 扩展长度 1 字节缺失
		{0x46, 0x01}, // 扩展长度 2 字节缺失
		{0x47, 0, 1}, // 扩展长度 4 字节缺失
	}
	for i, c := range cases {
		if _, _, err := parseTagHeader(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 应用值
// ---------------------------------------------------------------------------

func TestEncodeApplicationValueGolden(t *testing.T) {
	signed := func(v int64) uint64 { return uint64(v) }
	_ = signed
	cases := []struct {
		name  string
		build func() ([]byte, error)
		want  string
	}{
		{"null", func() ([]byte, error) {
			return EncodeApplicationValue(AppNull, 0, 0, nil, false)
		}, "00"},
		{"bool true", func() ([]byte, error) {
			return EncodeApplicationValue(AppBoolean, 0, 0, nil, true)
		}, "11"},
		{"bool false", func() ([]byte, error) {
			return EncodeApplicationValue(AppBoolean, 0, 0, nil, false)
		}, "10"},
		{"unsigned 0", func() ([]byte, error) {
			return EncodeApplicationValue(AppUnsigned, 0, 0, nil, false)
		}, "21 00"},
		{"unsigned 300", func() ([]byte, error) {
			return EncodeApplicationValue(AppUnsigned, 300, 0, nil, false)
		}, "22 01 2C"},
		{"enumerated 9", func() ([]byte, error) {
			return EncodeApplicationValue(AppEnumerated, 9, 0, nil, false)
		}, "91 09"},
		{"signed -1", func() ([]byte, error) {
			return EncodeApplicationValue(AppSigned, signed(-1), 0, nil, false)
		}, "31 FF"},
		{"signed 127", func() ([]byte, error) {
			return EncodeApplicationValue(AppSigned, 127, 0, nil, false)
		}, "31 7F"},
		{"signed 128", func() ([]byte, error) {
			return EncodeApplicationValue(AppSigned, 128, 0, nil, false)
		}, "32 00 80"},
		{"signed -128", func() ([]byte, error) {
			return EncodeApplicationValue(AppSigned, signed(-128), 0, nil, false)
		}, "31 80"},
		{"real 1.5", func() ([]byte, error) {
			return EncodeApplicationValue(AppReal, 0, 1.5, nil, false)
		}, "44 3F C0 00 00"},
		{"double 1.5", func() ([]byte, error) {
			return EncodeApplicationValue(AppDouble, 0, 1.5, nil, false)
		}, "55 08 3F F8 00 00 00 00 00 00"},
		{"charstring Hi", func() ([]byte, error) {
			return EncodeApplicationValue(AppCharacterStr, 0, 0, []byte("Hi"), false)
		}, "73 00 48 69"},
		{"octetstring", func() ([]byte, error) {
			return EncodeApplicationValue(AppOctetString, 0, 0, []byte{0xDE, 0xAD}, false)
		}, "62 DE AD"},
		{"objectid", func() ([]byte, error) {
			return EncodeApplicationValue(AppObjectID, uint64(ObjectAnalogInput)<<22|1, 0, nil, false)
		}, "C4 00 00 00 01"},
	}
	for _, tc := range cases {
		got, err := tc.build()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := mustHex(t, tc.want)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s = % X, want % X", tc.name, got, want)
		}
	}
}

func TestEncodeApplicationValueErrors(t *testing.T) {
	if _, err := EncodeApplicationValue(AppReal, 0, math.NaN(), nil, false); err == nil {
		t.Fatal("NaN accepted as REAL")
	}
	if _, err := EncodeApplicationValue(AppReal, 0, math.Inf(1), nil, false); err == nil {
		t.Fatal("Inf accepted as REAL")
	}
	if _, err := EncodeApplicationValue(AppDouble, 0, math.Inf(-1), nil, false); err == nil {
		t.Fatal("Inf accepted as DOUBLE")
	}
	if _, err := EncodeApplicationValue(AppObjectID, 1<<33, 0, nil, false); err == nil {
		t.Fatal("33-bit object id accepted")
	}
	if _, err := EncodeApplicationValue(0x0F, 0, 0, nil, false); err == nil {
		t.Fatal("unknown application tag accepted")
	}
}

func TestDecodeApplicationValueRoundTrip(t *testing.T) {
	// unsigned / enumerated / object id 共享整数解码路径
	for _, tag := range []byte{AppUnsigned, AppEnumerated, AppObjectID} {
		for _, v := range []uint64{0, 1, 255, 256, 65535, 65536, 1 << 31} {
			raw, err := EncodeApplicationValue(tag, v, 0, nil, false)
			if err != nil {
				t.Fatalf("tag %d encode %d: %v", tag, v, err)
			}
			got, u, _, _, _, err := DecodeApplicationValue(raw)
			if err != nil || got != tag || u != v {
				t.Fatalf("tag %d value %d: got tag=%d u=%d err=%v", tag, v, got, u, err)
			}
		}
	}
	for _, v := range []int64{0, 1, -1, 127, 128, -128, -129, 32767, -32768, 1 << 40, -(1 << 40)} {
		raw, err := EncodeApplicationValue(AppSigned, uint64(v), 0, nil, false)
		if err != nil {
			t.Fatalf("signed encode %d: %v", v, err)
		}
		_, u, _, _, _, err := DecodeApplicationValue(raw)
		if err != nil || int64(u) != v {
			t.Fatalf("signed %d: got %d err=%v", v, int64(u), err)
		}
	}
	for _, f := range []float64{0, 1.5, -2.25, 42.5, 1e10} {
		raw, _ := EncodeApplicationValue(AppReal, 0, f, nil, false)
		_, _, got, _, _, err := DecodeApplicationValue(raw)
		if err != nil || got != f {
			t.Fatalf("real %v: got %v err=%v", f, got, err)
		}
		raw, _ = EncodeApplicationValue(AppDouble, 0, f, nil, false)
		_, _, got, _, _, err = DecodeApplicationValue(raw)
		if err != nil || got != f {
			t.Fatalf("double %v: got %v err=%v", f, got, err)
		}
	}
	raw, _ := EncodeApplicationValue(AppCharacterStr, 0, 0, []byte("hello"), false)
	_, _, _, b, _, err := DecodeApplicationValue(raw)
	if err != nil || string(b) != "hello" {
		t.Fatalf("charstring: %q err=%v", b, err)
	}
	raw, _ = EncodeApplicationValue(AppBoolean, 0, 0, nil, true)
	_, _, _, _, bl, err := DecodeApplicationValue(raw)
	if err != nil || !bl {
		t.Fatalf("bool: %v err=%v", bl, err)
	}
	raw, _ = EncodeApplicationValue(AppNull, 0, 0, nil, false)
	if tag, _, _, _, _, err := DecodeApplicationValue(raw); err != nil || tag != AppNull {
		t.Fatalf("null: tag=%d err=%v", tag, err)
	}
}

func TestDecodeApplicationValueErrors(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"real truncated", []byte{0x44, 0x3F}},
		{"double truncated", []byte{0x55, 0x3F, 0xF8}},
		{"charstring missing charset", []byte{0x70}},
		{"date unsupported", []byte{0x8A, 0x00}},
		{"time unsupported", []byte{0x8B, 0x00}},
		{"context tag", []byte{0x0C, 1, 2, 3, 4}},
		{"opening tag", []byte{0x3E}},
	}
	for _, tc := range cases {
		if _, _, _, _, _, err := DecodeApplicationValue(tc.buf); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}

// ---------------------------------------------------------------------------
// 对象标识符
// ---------------------------------------------------------------------------

func TestObjectIDRoundTrip(t *testing.T) {
	cases := []struct{ typ, inst int }{
		{ObjectAnalogInput, 1},
		{ObjectDevice, 4194303},
		{ObjectMultiStateValue, 100},
		{ObjectCharacterString, 0},
	}
	for _, tc := range cases {
		b, err := EncodeObjectID(tc.typ, tc.inst)
		if err != nil {
			t.Fatalf("encode (%d,%d): %v", tc.typ, tc.inst, err)
		}
		if len(b) != 4 {
			t.Fatalf("object id length = %d", len(b))
		}
		typ, inst, err := DecodeObjectID(b)
		if err != nil || typ != tc.typ || inst != tc.inst {
			t.Fatalf("roundtrip (%d,%d) -> (%d,%d) err=%v", tc.typ, tc.inst, typ, inst, err)
		}
	}
}

func TestObjectIDErrors(t *testing.T) {
	if _, err := EncodeObjectID(-1, 0); err == nil {
		t.Fatal("negative type accepted")
	}
	if _, err := EncodeObjectID(1024, 0); err == nil {
		t.Fatal("type > 1023 accepted")
	}
	if _, err := EncodeObjectID(0, -1); err == nil {
		t.Fatal("negative instance accepted")
	}
	if _, err := EncodeObjectID(0, 1<<22); err == nil {
		t.Fatal("instance > 4194303 accepted")
	}
	if _, _, err := DecodeObjectID([]byte{1, 2, 3}); err == nil {
		t.Fatal("short object id accepted")
	}
}

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

func TestReadPropertyRequest(t *testing.T) {
	req, err := ReadPropertyRequest(42, ObjectAnalogValue, 3, PropPresentValue, 0)
	if err != nil {
		t.Fatalf("ReadPropertyRequest: %v", err)
	}
	// 00 05 2A 0C | 0C 00 80 00 03 | 19 55（0x05 = 不分段 + 最大 APDU 1476）
	want := mustHex(t, "00 05 2A 0C 0C 00 80 00 03 19 55")
	if !bytes.Equal(req, want) {
		t.Fatalf("request = % X, want % X", req, want)
	}
	parsed, err := ParseConfirmedRequest(req)
	if err != nil || parsed.Service != ServiceReadProperty || parsed.InvokeID != 42 || parsed.Segmented {
		t.Fatalf("ParseConfirmedRequest: %+v err=%v", parsed, err)
	}
	typ, inst, prop, idx, err := ReadPropertyRequestParams(parsed.Params)
	if err != nil || typ != ObjectAnalogValue || inst != 3 || prop != PropPresentValue || idx != 0 {
		t.Fatalf("params = %d/%d/%d/%d err=%v", typ, inst, prop, idx, err)
	}
}

func TestReadPropertyRequestWithIndex(t *testing.T) {
	req, err := ReadPropertyRequest(1, ObjectDevice, 419441, PropPriorityArray, 5)
	if err != nil {
		t.Fatalf("ReadPropertyRequest: %v", err)
	}
	parsed, err := ParseConfirmedRequest(req)
	if err != nil {
		t.Fatalf("ParseConfirmedRequest: %v", err)
	}
	typ, inst, prop, idx, err := ReadPropertyRequestParams(parsed.Params)
	if err != nil || typ != ObjectDevice || inst != 419441 || prop != PropPriorityArray || idx != 5 {
		t.Fatalf("params = %d/%d/%d/%d err=%v", typ, inst, prop, idx, err)
	}
}

func TestWritePropertyRequest(t *testing.T) {
	val, _ := EncodeApplicationValue(AppReal, 0, 12.5, nil, false)
	req, err := WritePropertyRequest(7, ObjectAnalogValue, 1, PropPresentValue, 0, 16, val)
	if err != nil {
		t.Fatalf("WritePropertyRequest: %v", err)
	}
	// 00 05 07 0F | 0C 00 80 00 01 | 19 55 | 3E 44 41 48 00 00 3F | 49 10
	want := mustHex(t, "00 05 07 0F 0C 00 80 00 01 19 55 3E 44 41 48 00 00 3F 49 10")
	if !bytes.Equal(req, want) {
		t.Fatalf("request = % X, want % X", req, want)
	}
	parsed, err := ParseConfirmedRequest(req)
	if err != nil || parsed.Service != ServiceWriteProperty || parsed.InvokeID != 7 {
		t.Fatalf("ParseConfirmedRequest: %+v err=%v", parsed, err)
	}
	typ, inst, prop, prio, value, err := WritePropertyRequestParams(parsed.Params)
	if err != nil || typ != ObjectAnalogValue || inst != 1 || prop != PropPresentValue || prio != 16 {
		t.Fatalf("params = %d/%d/%d/%d err=%v", typ, inst, prop, prio, err)
	}
	if !bytes.Equal(value, val) {
		t.Fatalf("value = % X, want % X", value, val)
	}
}

func TestWritePropertyRequestWithoutPriority(t *testing.T) {
	val, _ := EncodeApplicationValue(AppUnsigned, 7, 0, nil, false)
	req, err := WritePropertyRequest(3, ObjectBinaryValue, 2, PropPresentValue, 0, 0, val)
	if err != nil {
		t.Fatalf("WritePropertyRequest: %v", err)
	}
	parsed, err := ParseConfirmedRequest(req)
	if err != nil {
		t.Fatalf("ParseConfirmedRequest: %v", err)
	}
	_, _, _, prio, _, err := WritePropertyRequestParams(parsed.Params)
	if err != nil || prio != 0 {
		t.Fatalf("priority = %d err=%v", prio, err)
	}
}

func TestWritePropertyRequestErrors(t *testing.T) {
	val, _ := EncodeApplicationValue(AppReal, 0, 1, nil, false)
	if _, err := WritePropertyRequest(1, ObjectAnalogValue, 1, PropPresentValue, 0, 0, nil); err == nil {
		t.Fatal("empty value accepted")
	}
	if _, err := WritePropertyRequest(1, ObjectAnalogValue, 1, PropPresentValue, 0, 17, val); err == nil {
		t.Fatal("priority 17 accepted")
	}
	if _, err := WritePropertyRequest(1, ObjectAnalogValue, 1, PropPresentValue, 0, -1, val); err == nil {
		t.Fatal("priority -1 accepted")
	}
	if _, err := WritePropertyRequest(1, ObjectAnalogValue, 1, PropPresentValue, -1, 0, val); err == nil {
		t.Fatal("negative array index accepted")
	}
}

// ---------------------------------------------------------------------------
// 应答解析
// ---------------------------------------------------------------------------

func TestClassifyAPDU(t *testing.T) {
	cases := []struct {
		first byte
		want  APDUKind
	}{
		{0x00, APDUKindConfirmedRequest},
		{0x08, APDUKindConfirmedRequest},
		{0x10, APDUKindUnconfirmed},
		{0x20, APDUKindSimpleACK},
		{0x40, APDUKindSegmentACK},
		{0x12, APDUKindUnconfirmed},
		{0x30, APDUKindComplexACK},
		{0x38, APDUKindComplexACK},
		{0x50, APDUKindError},
		{0x60, APDUKindReject},
		{0x70, APDUKindAbort},
		{0x71, APDUKindAbort},
		{0x80, APDUKind(0)},
		{0xC0, APDUKind(0)},
	}
	for _, tc := range cases {
		if got := ClassifyAPDU(tc.first); got != tc.want {
			t.Fatalf("ClassifyAPDU(0x%02X) = %v, want %v", tc.first, got, tc.want)
		}
	}
	if s := APDUKindComplexACK.String(); s != "complex-ack" {
		t.Fatalf("String() = %q", s)
	}
	if s := APDUKind(99).String(); s != "unknown" {
		t.Fatalf("String() = %q", s)
	}
}

func TestParseACK(t *testing.T) {
	kind, svc, invoke, payload, err := ParseACK([]byte{APDUComplexACK, 9, ServiceReadProperty, 0xAA})
	if err != nil {
		t.Fatalf("ParseACK: %v", err)
	}
	if kind != APDUKindComplexACK || svc != ServiceReadProperty || invoke != 9 || !bytes.Equal(payload, []byte{0xAA}) {
		t.Fatalf("ParseACK = %v/%d/%d/% X", kind, svc, invoke, payload)
	}
	kind, svc, invoke, payload, err = ParseACK([]byte{APDUSimpleACK, 9, ServiceWriteProperty})
	if err != nil {
		t.Fatalf("ParseACK simple: %v", err)
	}
	if kind != APDUKindSimpleACK || svc != ServiceWriteProperty || invoke != 9 || len(payload) != 0 {
		t.Fatalf("simple ack = %v/%d/%d/% X", kind, svc, invoke, payload)
	}
}

func TestParseACKErrors(t *testing.T) {
	cases := [][]byte{
		nil,
		{APDUComplexACK, ServiceReadProperty},
		{0x28, ServiceReadProperty, 1},  // 分段 ComplexACK 不支持
		{0x11, ServiceWriteProperty, 1}, // SimpleACK 低位非 0
		{APDUError, 1, 2},               // 不是 ACK
	}
	for i, c := range cases {
		if _, _, _, _, err := ParseACK(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestBuildAndParseComplexACK(t *testing.T) {
	val, _ := EncodeApplicationValue(AppReal, 0, 42.5, nil, false)
	ack, err := BuildComplexACK(ServiceReadProperty, 9, ObjectAnalogInput, 1, PropPresentValue, val)
	if err != nil {
		t.Fatalf("BuildComplexACK: %v", err)
	}
	// 30 09 0C | 0C 00 00 00 01 | 19 55 | 3E 44 42 2A 00 00 3F
	want := mustHex(t, "30 09 0C 0C 00 00 00 01 19 55 3E 44 42 2A 00 00 3F")
	if !bytes.Equal(ack, want) {
		t.Fatalf("ack = % X, want % X", ack, want)
	}
	kind, svc, invoke, payload, err := ParseACK(ack)
	if err != nil || kind != APDUKindComplexACK || svc != ServiceReadProperty || invoke != 9 {
		t.Fatalf("ParseACK: %v/%d/%d err=%v", kind, svc, invoke, err)
	}
	appVal, has, err := ReadPropertyACK(payload)
	if err != nil || !has {
		t.Fatalf("ReadPropertyACK: has=%v err=%v", has, err)
	}
	tag, _, f, _, _, err := DecodeApplicationValue(appVal)
	if err != nil || tag != AppReal || f != 42.5 {
		t.Fatalf("value = %v tag=%d err=%v", f, tag, err)
	}
}

func TestBuildComplexACKWithoutValue(t *testing.T) {
	ack, err := BuildComplexACK(ServiceReadProperty, 1, ObjectAnalogInput, 1, PropPresentValue, nil)
	if err != nil {
		t.Fatalf("BuildComplexACK: %v", err)
	}
	_, _, _, payload, err := ParseACK(ack)
	if err != nil {
		t.Fatalf("ParseACK: %v", err)
	}
	appVal, has, err := ReadPropertyACK(payload)
	if err != nil || has || appVal != nil {
		t.Fatalf("ReadPropertyACK: appVal=% X has=%v err=%v", appVal, has, err)
	}
}

// 部分老设备把应用值直接塞进 [3] 的原始负载，不带 opening/closing。
// 解析器两种都要接受，否则现场会出现「能连上但读不出值」。
func TestReadPropertyACKRawForm(t *testing.T) {
	val, _ := EncodeApplicationValue(AppUnsigned, 7, 0, nil, false)
	payload := mustHex(t, "0C 00 00 00 01 19 55")
	head, ok := encodeCtxTagHeader(3, len(val))
	if !ok {
		t.Fatal("encodeCtxTagHeader failed")
	}
	payload = append(payload, head...)
	payload = append(payload, val...)
	appVal, has, err := ReadPropertyACK(payload)
	if err != nil || !has {
		t.Fatalf("ReadPropertyACK: has=%v err=%v", has, err)
	}
	if _, u, _, _, _, err := DecodeApplicationValue(appVal); err != nil || u != 7 {
		t.Fatalf("value = %d err=%v", u, err)
	}
}

func TestReadPropertyACKNested(t *testing.T) {
	// 构造值里再套一个构造值（如 priority-array 的元素），只取最外层。
	payload := mustHex(t, "0C 00 00 00 01 19 57")
	payload = append(payload, contextOpening(3)...)
	payload = append(payload, contextOpening(0)...)
	payload = append(payload, mustHex(t, "44 42 2A 00 00")...)
	payload = append(payload, contextClosing(0)...)
	payload = append(payload, contextClosing(3)...)
	appVal, has, err := ReadPropertyACK(payload)
	if err != nil || !has {
		t.Fatalf("ReadPropertyACK: has=%v err=%v", has, err)
	}
	if !bytes.Contains(appVal, mustHex(t, "42 2A 00 00")) {
		t.Fatalf("inner value = % X", appVal)
	}
}

func TestReadPropertyACKErrors(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"truncated value", mustHex(t, "0C 00 00 00 01 19 55 3E 44 42")},
		{"missing closing", mustHex(t, "0C 00 00 00 01 19 55 3E 44 42 2A 00 00")},
		{"unexpected closing", mustHex(t, "3F")},
		{"bad opening number", mustHex(t, "1E 44 42 2A 00 00 1F")},
		{"truncated header", mustHex(t, "0C 00")},
	}
	for _, tc := range cases {
		if _, _, err := ReadPropertyACK(tc.payload); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}

func TestBuildSimpleACK(t *testing.T) {
	ack := BuildSimpleACK(ServiceWriteProperty, 9)
	if want := mustHex(t, "20 09 0F"); !bytes.Equal(ack, want) {
		t.Fatalf("SimpleACK = % X, want % X", ack, want)
	}
	kind, svc, invoke, payload, err := ParseACK(ack)
	if err != nil || kind != APDUKindSimpleACK || svc != ServiceWriteProperty || invoke != 9 || len(payload) != 0 {
		t.Fatalf("ParseACK = %v/%d/%d/% X err=%v", kind, svc, invoke, payload, err)
	}
}

func TestBuildAndParseError(t *testing.T) {
	apdu := BuildError(ServiceWriteProperty, 9, ErrorClassProperty, ErrorCodeWriteAccessDenied)
	if want := mustHex(t, "50 09 91 02 91 28"); !bytes.Equal(apdu, want) {
		t.Fatalf("Error = % X, want % X", apdu, want)
	}
	invoke, ec, ek, err := ParseError(apdu)
	if err != nil || invoke != 9 || ec != ErrorClassProperty || ek != ErrorCodeWriteAccessDenied {
		t.Fatalf("ParseError = %d/%d/%d err=%v", invoke, ec, ek, err)
	}
}

func TestParseErrorErrors(t *testing.T) {
	cases := [][]byte{
		nil,
		{APDUError, 5},
		{APDUComplexACK, 5, 0x91, 0x02},
		{APDUError, 5, 0x1C, 1, 2, 3, 4}, // 错误类是 context 标签
		{APDUError, 5, 0x91},             // 缺错误码
	}
	for i, c := range cases {
		if _, _, _, err := ParseError(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestParseRejectAndAbort(t *testing.T) {
	invoke, reason, err := ParseReject(BuildReject(3, RejectReasonInvalidTag))
	if err != nil || invoke != 3 || reason != RejectReasonInvalidTag {
		t.Fatalf("reject = %d/%d err=%v", invoke, reason, err)
	}
	invoke, reason, err = ParseAbort(BuildAbort(true, 4, RejectReasonBufferOverflow))
	if err != nil || invoke != 4 || reason != RejectReasonBufferOverflow {
		t.Fatalf("abort = %d/%d err=%v", invoke, reason, err)
	}
	if _, _, err := ParseReject([]byte{APDUAbort, 1, 2}); err == nil {
		t.Fatal("abort accepted as reject")
	}
	if _, _, err := ParseAbort([]byte{APDUReject, 1, 2}); err == nil {
		t.Fatal("reject accepted as abort")
	}
	if _, _, err := ParseReject([]byte{APDUReject}); err == nil {
		t.Fatal("short reject accepted")
	}
	if _, _, err := ParseAbort([]byte{APDUAbort}); err == nil {
		t.Fatal("short abort accepted")
	}
}

func TestAcknowledgeInvoke(t *testing.T) {
	cases := []struct {
		apdu []byte
		id   byte
		ok   bool
	}{
		{[]byte{APDUComplexACK, 7}, 7, true},
		{[]byte{APDUSimpleACK, 8}, 8, true},
		{[]byte{APDUError, 9, 0}, 9, true},
		{[]byte{APDUReject, 10, 0}, 10, true},
		{[]byte{APDUAbort, 11, 0}, 11, true},
		{[]byte{APDUConfirmedRequest, 12}, 0, false},
		{[]byte{APDUComplexACK}, 0, false},
	}
	for _, tc := range cases {
		id, ok := AcknowledgeInvoke(tc.apdu)
		if ok != tc.ok || id != tc.id {
			t.Fatalf("% X: got %d/%v, want %d/%v", tc.apdu, id, ok, tc.id, tc.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// 请求解析（从站侧）
// ---------------------------------------------------------------------------

func TestParseConfirmedRequestErrors(t *testing.T) {
	if _, err := ParseConfirmedRequest(nil); err == nil {
		t.Fatal("empty accepted")
	}
	if _, err := ParseConfirmedRequest([]byte{APDUComplexACK, 1, 2}); err == nil {
		t.Fatal("complex ack accepted")
	}
	req, err := ParseConfirmedRequest([]byte{0x08, 0x00, 1, 0x0C, 0xAA})
	if err != nil {
		t.Fatalf("segmented request rejected: %v", err)
	}
	if !req.Segmented || req.InvokeID != 1 || req.Service != ServiceReadProperty {
		t.Fatalf("segmented request = %+v", req)
	}
}

func TestParseContextParamsErrors(t *testing.T) {
	cases := []struct {
		name   string
		params []byte
		n      int
	}{
		{"zero count", mustHex(t, "0C 00 00 00 01"), 0},
		{"application tag", mustHex(t, "44 01 02 03 04"), 3},
		{"tag out of range", mustHex(t, "5C 00 00 00 01"), 3},
		{"duplicate tag", mustHex(t, "0C 00 00 00 01 0C 00 00 00 02"), 3},
		{"unexpected closing", mustHex(t, "0F"), 3},
		{"truncated", mustHex(t, "0C 00 00"), 3},
	}
	for _, tc := range cases {
		if _, err := parseContextParams(tc.params, tc.n); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}

func TestReadPropertyRequestParamsErrors(t *testing.T) {
	// 缺属性标识
	if _, _, _, _, err := ReadPropertyRequestParams(mustHex(t, "0C 00 00 00 01")); err == nil {
		t.Fatal("missing property accepted")
	}
	// 对象标识长度不足
	if _, _, _, _, err := ReadPropertyRequestParams(mustHex(t, "0C 00 00 00 19 01")); err == nil {
		t.Fatal("short object id accepted")
	}
}

func TestSplitWritePropertyParamsErrors(t *testing.T) {
	// 缺属性值
	if _, _, _, _, _, err := SplitWritePropertyParams(mustHex(t, "0C 00 00 00 01 19 55")); err == nil {
		t.Fatal("missing value accepted")
	}
	// 对象标识长度不足
	if _, _, _, _, _, err := SplitWritePropertyParams(
		mustHex(t, "0C 00 00 00 19 55 3E 44 00 00 00 00 3F")); err == nil {
		t.Fatal("short object id accepted")
	}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s)/2)
	var cur int
	have := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		case c == ' ' || c == '\t' || c == '\n':
			continue
		default:
			t.Fatalf("bad hex string %q", s)
		}
		if !have {
			cur = v
			have = true
		} else {
			out = append(out, byte(cur<<4|v))
			have = false
		}
	}
	if have {
		t.Fatalf("odd hex string %q", s)
	}
	return out
}
