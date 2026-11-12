package snmp

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"uic/internal/driver"
)

// 这一层的价值在于「一条报文一个字节都不用猜」。因此金帧与逐字段断言
// 比「build 之后 parse 回来相等」更重要：后者只能证明自己跟自己一致。

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	// 金帧写成缩进的多行十六进制，缩进是制表符；统一按空白切开再拼接，
	// 比逐个替换空格/换行更不容易漏掉一种空白。
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad hex in test: %v", err)
	}
	return b
}

// GetRequest 的金帧（v2c，community=public，request-id=0x01020304，
// OID=1.3.6.1.2.1.1.1.0）。逐字节写出来是为了让「多一个 0x00」「长度
// 算错一个字节」这类错误在评审时肉眼可见。
func TestGetRequestGolden(t *testing.T) {
	req, err := GetRequest(V2c, "public", 0x01020304, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	if err != nil {
		t.Fatal(err)
	}
	want := hexBytes(t, `
		30 29
		  02 01 01
		  04 06 70 75 62 6c 69 63
		  a0 1c
		    02 04 01 02 03 04
		    02 01 00
		    02 01 00
		    30 0e
		      30 0c
		        06 08 2b 06 01 02 01 01 01 00
		        05 00`)
	if !bytes.Equal(req, want) {
		t.Fatalf("request = % x\nwant      = % x", req, want)
	}

	// 解析回来必须得到同样的结构（包括 community 的原样保留）。
	m, err := Parse(req)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Version != V2c || m.Community != "public" {
		t.Fatalf("header = %v %q", m.Version, m.Community)
	}
	if m.PDU.Type != PDUGet || m.PDU.RequestID != 0x01020304 {
		t.Fatalf("pdu = %s id=%d", m.PDU.Type, m.PDU.RequestID)
	}
	if len(m.PDU.Varbinds) != 1 || m.PDU.Varbinds[0].OID.String() != "1.3.6.1.2.1.1.1.0" {
		t.Fatalf("varbinds = %+v", m.PDU.Varbinds)
	}
	if m.PDU.Varbinds[0].Value.Type != TypeNull {
		t.Fatalf("request value = %s, want NULL", m.PDU.Varbinds[0].Value.Type)
	}
}

// 应答金帧：TimeTicks（最高位不进位）、Counter32 0x80000000（必须补前导
// 0x00）、OCTET STRING。三种写法混在一帧里，长度字段错一个字节就整帧失败。
func TestResponseGolden(t *testing.T) {
	raw := hexBytes(t, `
		30 51
		  02 01 01
		  04 06 70 75 62 6c 69 63
		  a2 44
		    02 01 07
		    02 01 00
		    02 01 00
		    30 39
		      30 0f
		        06 08 2b 06 01 02 01 01 03 00
		        43 03 01 e2 40
		      30 13
		        06 0a 2b 06 01 02 01 02 02 01 0a 01
		        41 05 00 80 00 00 00
		      30 11
		        06 08 2b 06 01 02 01 01 05 00
		        04 05 72 74 2d 30 31`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.PDU.Type != PDUResponse || m.PDU.RequestID != 7 {
		t.Fatalf("pdu = %s id=%d", m.PDU.Type, m.PDU.RequestID)
	}
	vbs := m.PDU.Varbinds
	if len(vbs) != 3 {
		t.Fatalf("varbinds = %d", len(vbs))
	}
	if vbs[0].OID.String() != "1.3.6.1.2.1.1.3.0" || vbs[0].Value.Type != TypeTimeTicks || vbs[0].Value.Uint != 123456 {
		t.Fatalf("varbind[0] = %s %s", vbs[0].OID, vbs[0].Value)
	}
	if vbs[1].OID.String() != "1.3.6.1.2.1.2.2.1.10.1" || vbs[1].Value.Uint != 0x80000000 {
		t.Fatalf("varbind[1] = %s %s", vbs[1].OID, vbs[1].Value)
	}
	if vbs[2].Value.Type != TypeOctetString || string(vbs[2].Value.Bytes) != "rt-01" {
		t.Fatalf("varbind[2] = %s", vbs[2].Value)
	}
}

func TestIntegerEncoding(t *testing.T) {
	cases := []struct {
		v    int64
		want string
	}{
		{0, "02 01 00"},
		{127, "02 01 7f"},
		{128, "02 02 00 80"},
		{-1, "02 01 ff"},
		{-128, "02 01 80"},
		{-129, "02 02 ff 7f"},
		{2147483647, "02 04 7f ff ff ff"},
		{-2147483648, "02 04 80 00 00 00"},
	}
	for _, tc := range cases {
		got := appendTLV(nil, tagInteger, encodeInt(tc.v))
		if !bytes.Equal(got, hexBytes(t, tc.want)) {
			t.Fatalf("%d = % x, want % x", tc.v, got, hexBytes(t, tc.want))
		}
		// 往返。
		back, err := decodeInt(got[2:])
		if err != nil || back != tc.v {
			t.Fatalf("%d round trip = %d, %v", tc.v, back, err)
		}
	}
}

func TestUnsignedEncodingNeedsLeadingZero(t *testing.T) {
	cases := []struct {
		v    uint64
		want string
	}{
		{0, "00"},
		{127, "7f"},
		{128, "00 80"},
		{0x7FFFFFFF, "7f ff ff ff"},
		{0x80000000, "00 80 00 00 00"},
		{0xFFFFFFFF, "00 ff ff ff ff"},
	}
	for _, tc := range cases {
		got := encodeUnsigned(tc.v)
		if !bytes.Equal(got, hexBytes(t, tc.want)) {
			t.Fatalf("%d = % x, want % x", tc.v, got, hexBytes(t, tc.want))
		}
		back, err := decodeUnsigned(got)
		if err != nil || back != tc.v {
			t.Fatalf("%d round trip = %d, %v", tc.v, back, err)
		}
	}
}

func TestOIDEncoding(t *testing.T) {
	cases := map[string]string{
		"1.3.6.1.2.1.1.1.0":      "2b 06 01 02 01 01 01 00",
		"0.0":                    "00",
		"2.39.1":                 "77 01",
		"1.3.6.1.4.1.9.9":        "2b 06 01 04 01 09 09",
		"1.3.6.1.2.1.2.2.1.10.1": "2b 06 01 02 01 02 02 01 0a 01",
	}
	for in, want := range cases {
		o := MustParseOID(in)
		if got := encodeOID(o); !bytes.Equal(got, hexBytes(t, want)) {
			t.Fatalf("%s = % x, want % x", in, got, hexBytes(t, want))
		}
		back, err := decodeOID(encodeOID(o))
		if err != nil || back.Compare(o) != 0 {
			t.Fatalf("%s round trip = %s, %v", in, back, err)
		}
		if o.String() != in {
			t.Fatalf("%s formatted as %s", in, o)
		}
	}
}

func TestOIDOrdering(t *testing.T) {
	// 字符串比较会把 .10 排在 .9 前面；字典序必须按弧比较。
	a := MustParseOID("1.3.6.1.2.1.2.2.1.9")
	b := MustParseOID("1.3.6.1.2.1.2.2.1.10")
	if a.Compare(b) >= 0 {
		t.Fatalf("%s should sort before %s", a, b)
	}
	if MustParseOID("1.3.6.1").Compare(MustParseOID("1.3.6.1.0")) >= 0 {
		t.Fatal("prefix must sort first")
	}
	if !MustParseOID("1.3.6.1").HasPrefix(MustParseOID("1.3.6.1.2.1")) {
		t.Fatal("HasPrefix should accept descendants")
	}
	if MustParseOID("1.3.6.1.2").HasPrefix(MustParseOID("1.3.6.1")) {
		t.Fatal("HasPrefix must not accept ancestors")
	}
}

func TestOIDRejects(t *testing.T) {
	bad := []string{
		"", "1", "1.", ".1.3.", "1.03.6", "3.1", "1.40", "1.3.-1", "1.3.6.x",
		"1.3.4294967296", "1.3. 6",
	}
	// 前后空格是被允许的（ReadOID 的调用方常从 YAML 抄），单独验证。
	if o, err := ParseOID(" 1.3.6.1 "); err != nil || o.String() != "1.3.6.1" {
		t.Fatalf("trim = %v, %v", o, err)
	}
	for _, s := range bad {
		if o, err := ParseOID(s); err == nil {
			t.Fatalf("%q must fail, got %s", s, o)
		}
	}
}

func TestOIDDecodeRejects(t *testing.T) {
	// 以 continuation 结尾：最后一个字节的高位还是 1。
	if _, err := decodeOID([]byte{0x2b, 0x86}); err == nil {
		t.Fatal("continuation at end must fail")
	}
	if _, err := decodeOID(nil); err == nil {
		t.Fatal("empty OID must fail")
	}
	// 首弧 > 2 时按 2.(subid-80) 处理，与主流实现的宽松解读一致。
	// 159 的 base-128 编码是 `81 1f`，不是单字节 `9f`（高位为 1 表示续字节）。
	o, err := decodeOID([]byte{0x81, 0x1f})
	if err != nil || o.String() != "2.79" {
		t.Fatalf("2.79 = %s, %v", o, err)
	}
}

func TestParseVersion(t *testing.T) {
	ok := map[string]Version{"": V2c, "2c": V2c, "v2c": V2c, "2": V2c, "1": V1, "v1": V1, "3": V3, "v3": V3}
	for in, want := range ok {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Fatalf("%q = %v, %v", in, got, err)
		}
	}
	if _, err := ParseVersion("4"); err == nil {
		t.Fatal("unknown version must fail")
	}
}

func TestBuildRejects(t *testing.T) {
	oid := MustParseOID("1.3.6.1.2.1.1.1.0")
	if _, err := Build(Message{Version: V1, Community: "", PDU: PDU{Type: PDUGet, Varbinds: []Varbind{{OID: oid, Value: Null()}}}}); err == nil {
		t.Fatal("empty community must fail")
	}
	if _, err := GetBulkRequest("public", 1, []OID{oid}, 0, 10); err != nil {
		t.Fatalf("v2c getbulk: %v", err)
	}
	// GetBulk 只能走 v2c。
	if _, err := Build(Message{Version: V1, Community: "public", PDU: PDU{
		Type: PDUGetBulk, RequestID: 1, Varbinds: []Varbind{{OID: oid, Value: Null()}},
	}}); err == nil {
		t.Fatal("v1 getbulk must fail")
	}
	// 异常标记不能被当成值编码。
	if _, err := SetRequest(V2c, "public", 1, []Varbind{{OID: oid, Value: Exception(TypeNoSuchObject)}}); err == nil {
		t.Fatal("encoding an exception must fail")
	}
	if _, err := SetRequest(V2c, "public", 1, []Varbind{{OID: oid, Value: IP([]byte{1, 2, 3})}}); err == nil {
		t.Fatal("3-byte IpAddress must fail")
	}
}

func TestParseRejects(t *testing.T) {
	good, err := GetRequest(V2c, "public", 1, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":                nil,
		"trailing bytes":       append(append([]byte(nil), good...), 0x00),
		"truncated":            good[:len(good)-1],
		"not a sequence":       append([]byte{0x02, 0x01, 0x01}, good[3:]...),
		"indefinite len":       {0x30, 0x80, 0x02, 0x01, 0x01, 0x00, 0x00},
		"non-minimal len":      {0x30, 0x81, 0x05, 0x02, 0x01, 0x01, 0x02, 0x01, 0x00, 0x00},
		"multi-byte tag":       {0x3f, 0x01, 0x00},
		"unknown wire version": hexBytes(t, "30 06 02 01 09 04 01 00"),
	}
	for name, b := range cases {
		if _, err := Parse(b); err == nil {
			t.Fatalf("%s must fail", name)
		}
	}
}

func TestParseExceptionVarbinds(t *testing.T) {
	// v2c 的三种「没有值」：错误码为 0，值位置是异常标记。
	oid := MustParseOID("1.3.6.1.2.1.1.9.0")
	msg := Message{Version: V2c, Community: "public", PDU: PDU{
		Type: PDUResponse, RequestID: 3, Varbinds: []Varbind{
			{OID: oid, Value: Exception(TypeNoSuchObject)},
			{OID: oid, Value: Exception(TypeNoSuchInstance)},
			{OID: oid, Value: Exception(TypeEndOfMibView)},
		},
	}}
	raw, err := Build(msg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for i, want := range []Type{TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView} {
		if back.PDU.Varbinds[i].Value.Type != want {
			t.Fatalf("varbind[%d] = %s, want %s", i, back.PDU.Varbinds[i].Value.Type, want)
		}
		if !back.PDU.Varbinds[i].Value.IsException() {
			t.Fatal("exception flag not set")
		}
	}
}

func TestGetBulkWire(t *testing.T) {
	raw, err := GetBulkRequest("public", 9, []OID{MustParseOID("1.3.6.1.2.1.2.2")}, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.PDU.Type != PDUGetBulk || m.PDU.NonRepeaters != 0 || m.PDU.MaxRepetitions != 8 {
		t.Fatalf("pdu = %s nr=%d mr=%d", m.PDU.Type, m.PDU.NonRepeaters, m.PDU.MaxRepetitions)
	}
	// GetBulk 的 non-repeaters / max-repetitions 占的是 error-status /
	// error-index 的两个 INTEGER 位置；解析后必须落在正确字段上。
	if m.PDU.ErrorStatus != 0 || m.PDU.ErrorIndex != 0 {
		t.Fatalf("error fields = %d/%d", m.PDU.ErrorStatus, m.PDU.ErrorIndex)
	}
}

func TestErrorStatusKinds(t *testing.T) {
	cases := []struct {
		st   ErrorStatus
		kind driver.ErrorKind
	}{
		{ErrTooBig, driver.ErrProtocol},
		{ErrNoSuchName, driver.ErrAddress},
		{ErrBadValue, driver.ErrAddress},
		{ErrReadOnly, driver.ErrPermission},
		{ErrGenErr, driver.ErrProtocol},
		{ErrNoAccess, driver.ErrPermission},
		{ErrWrongType, driver.ErrAddress},
		{ErrWrongLength, driver.ErrAddress},
		{ErrWrongEncoding, driver.ErrAddress},
		{ErrWrongValue, driver.ErrAddress},
		{ErrNoCreation, driver.ErrPermission},
		{ErrInconsistentValue, driver.ErrAddress},
		{ErrResourceUnavailable, driver.ErrPermission},
		{ErrCommitFailed, driver.ErrProtocol},
		{ErrUndoFailed, driver.ErrProtocol},
		{ErrAuthorizationError, driver.ErrPermission},
		{ErrNotWritable, driver.ErrPermission},
		{ErrInconsistentName, driver.ErrAddress},
	}
	for _, tc := range cases {
		if got := tc.st.Kind(); got != tc.kind {
			t.Fatalf("%s kind = %v, want %v", tc.st, got, tc.kind)
		}
	}
	// 值得重试的只有「代理暂时做不到」这一类，配置错误重试一万次也没用。
	for _, st := range []ErrorStatus{ErrTooBig, ErrGenErr, ErrCommitFailed, ErrResourceUnavailable} {
		if !st.Temporary() {
			t.Fatalf("%s should be temporary", st)
		}
	}
	for _, st := range []ErrorStatus{ErrNoSuchName, ErrReadOnly, ErrWrongType} {
		if st.Temporary() {
			t.Fatalf("%s must not be temporary", st)
		}
	}
}

func TestStringValues(t *testing.T) {
	if got := String("rt-01").String(); got != "rt-01" {
		t.Fatalf("text = %q", got)
	}
	if got := Octets([]byte{0x00, 0xFF}).String(); got != "0x00ff" {
		t.Fatalf("binary = %q", got)
	}
	if got := IP([]byte{192, 168, 1, 2}).String(); got != "192.168.1.2" {
		t.Fatalf("ip = %q", got)
	}
	if got := OIDValue(MustParseOID("1.3.6.1.4.1.9")).String(); got != "1.3.6.1.4.1.9" {
		t.Fatalf("oid = %q", got)
	}
	if got := Unsigned(TypeCounter64, 1234567890123).String(); got != "1234567890123" {
		t.Fatalf("counter64 = %q", got)
	}
	if got := Exception(TypeEndOfMibView).String(); got != "endOfMibView" {
		t.Fatalf("exception = %q", got)
	}
}

func TestParseOIDList(t *testing.T) {
	got, err := ParseOIDList("1.3.6.1.2.1.1.1.0, 1.3.6.1.2.1.1.3.0;1.3.6.1.2.1.1.5.0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d OIDs", len(got))
	}
	if _, err := ParseOIDList(" , "); err == nil {
		t.Fatal("empty list must fail")
	}
	if _, err := ParseOIDList("1.3.6.1,bad"); err == nil {
		t.Fatal("bad OID must fail")
	}
}

func TestTruncatedErrorsAreIdentifiable(t *testing.T) {
	_, err := Parse([]byte{0x30, 0x10, 0x02})
	if err == nil {
		t.Fatal("must fail")
	}
	if !errors.Is(err, errShort) {
		t.Fatalf("error = %v, want errShort", err)
	}
}
