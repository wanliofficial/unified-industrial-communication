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

// TestOIDFirstSubIDBoundary 盯住首个子标识符的 64 位承载能力。
//
// RFC 1155 规定 arc1=2 时 arc2 没有上界，于是首个子标识符
// (40*arc1+arc2) 最大能到 40*2+(2^32-1) = 2^32+79，**超出 uint32**。
//
// 这里曾经用 uint32 计算 40*arc1+arc2：{2, 4294967295} 会回绕成 79，编出来
// 与 {2,39} 完全相同的字节，解码回来变成一台不存在的设备 1.39。报文合法、
// 对端不报错、值落在别的 OID 上——这是最难查的一类现场故障，所以既要盯着
// 编码字节，也要盯着往返，还要盯着解码侧的上限是否与编码侧对称。
func TestOIDFirstSubIDBoundary(t *testing.T) {
	// 2.4294967295 → 首子标识符 4294967296+79，base-128 恰好 5 字节。
	// 16 是 2^32/128^4 的商，后三字节全 0，末字节 79。
	big := MustParseOID("2.4294967295")
	wantBig := hexBytes(t, "90 80 80 80 4f")
	if got := encodeOID(big); !bytes.Equal(got, wantBig) {
		t.Fatalf("%s = % x, want % x", big, got, wantBig)
	}
	// 2.39 的首子标识符是 40*2+39 = 119，只占一字节；与 2^32+79 绝不能撞车。
	small := MustParseOID("2.39")
	if got := encodeOID(small); !bytes.Equal(got, hexBytes(t, "77")) {
		t.Fatalf("%s = % x, want 77", small, got)
	}
	// 真正的碰撞对象是 1.39：它的首子标识符恰是 79，正是 uint32 回绕后的值。
	// 也就是说这个缺陷会让报文悄悄指向 1.39 那台「另一台设备」。
	collide := MustParseOID("1.39")
	if got := encodeOID(collide); !bytes.Equal(got, hexBytes(t, "4f")) {
		t.Fatalf("%s = % x, want 4f", collide, got)
	}
	if bytes.Equal(encodeOID(big), encodeOID(collide)) {
		t.Fatal("{2, 4294967295} must not encode like {1, 39}")
	}

	// 往返：解码侧必须接受首个子标识符 > 2^32-1，否则就成了写得出、读不回。
	for _, s := range []string{
		"2.4294967295", // 首子标识符上限 2^32+79
		"2.4294967216", // 首子标识符恰好 2^32，刚跨过 32 位边界
		"2.4294967215", // 首子标识符恰好 2^32-1，最后一个能塞进 uint32 的值
		"2.4294967214", // 首子标识符 2^32-2，跨边界的前一刻
		"2.39",
		"2.40", // arc2 刚过「首弧<2 时 ≤39」的限制，首弧=2 时合法
		"1.3",
		"0.0",
		"2.4294967295.4294967295", // 大首弧 + 32 位上限的普通子标识符
		"2.4294967295.0.1",
	} {
		o := MustParseOID(s)
		got, err := decodeOID(encodeOID(o))
		if err != nil {
			t.Fatalf("%s round trip: %v", s, err)
		}
		if got.Compare(o) != 0 {
			t.Fatalf("%s round trip = %s", s, got)
		}
		if got.String() != s {
			t.Fatalf("%s formatted as %s", s, got)
		}
	}

	// 首个子标识符越界：2^32+80（base-128 末字节 0x50）必须拒绝，
	// 而不是回绕或截断成 2.4294967295。
	if _, err := decodeOID(hexBytes(t, "90 80 80 80 50")); err == nil {
		t.Fatal("first sub-identifier 2^32+80 must be rejected")
	}
	// 普通子标识符仍然卡在 32 位：2^32（末字节 0x00）必须拒绝。
	if _, err := decodeOID(hexBytes(t, "2b 90 80 80 80 00")); err == nil {
		t.Fatal("non-first sub-identifier 2^32 must be rejected")
	}
	// 32 位上限本身必须放行（1.3.4294967295）。
	top := MustParseOID("1.3.4294967295")
	back, err := decodeOID(encodeOID(top))
	if err != nil || back.Compare(top) != 0 {
		t.Fatalf("1.3.4294967295 round trip = %s, %v", back, err)
	}
	// 超长续字节串：必须在累积成 uint64 回绕之前就报错。
	long := bytes.Repeat([]byte{0xFF}, 16)
	if _, err := decodeOID(long); err == nil {
		t.Fatal("overlong sub-identifier must be rejected")
	}
}

// TestOIDArcLimit 钉住 RFC 2578 §3.5 的「最多 128 个子标识符」。
//
// 这条上限只在两种场合被触发：自动生成的 OID 拼接出错，或报文被人为拼接。
// 两种都不该被「读到多少算多少」地接受——多出来的弧会把点位的字典序位置
// 挪到别处，让 GETNEXT 的遍历结果看起来「跳了一段」。
func TestOIDArcLimit(t *testing.T) {
	mk := func(n int) string {
		parts := make([]string, n)
		parts[0], parts[1] = "1", "3"
		for i := 2; i < n; i++ {
			parts[i] = "1"
		}
		return strings.Join(parts, ".")
	}
	// 128 个弧是合法的，且必须能往返。
	ok := MustParseOID(mk(128))
	back, err := decodeOID(encodeOID(ok))
	if err != nil || back.Compare(ok) != 0 {
		t.Fatalf("128-arc OID round trip: %v", err)
	}
	if _, err := ParseOID(mk(129)); err == nil {
		t.Fatal("129 arcs must be rejected")
	}

	// 解码侧同一把尺子：0x2b 是首子标识符（展开成 1.3 两弧），后面每个
	// 0x00 再加一弧。126 个 0x00 正好 128 弧，127 个就越界。
	atLimit := append([]byte{0x2b}, bytes.Repeat([]byte{0x00}, 126)...)
	o, err := decodeOID(atLimit)
	if err != nil || len(o) != 128 {
		t.Fatalf("128 arcs: len=%d, err=%v", len(o), err)
	}
	over := append([]byte{0x2b}, bytes.Repeat([]byte{0x00}, 127)...)
	if _, err := decodeOID(over); err == nil {
		t.Fatal("decoding 129 arcs must be rejected")
	}
}

// TestGetBulkRejectsNegativeCounters：GetBulk 的两个计数字段语义上不可为负，
// 编码侧必须与解析侧一样拒绝，而不是把负数写成 INTEGER 发出去。
func TestGetBulkRejectsNegativeCounters(t *testing.T) {
	oids := []OID{MustParseOID("1.3.6.1.2.1.1")}
	if _, err := GetBulkRequest("public", 1, oids, -1, 10); err == nil {
		t.Fatal("negative non-repeaters must be rejected")
	}
	if _, err := GetBulkRequest("public", 1, oids, 0, -1); err == nil {
		t.Fatal("negative max-repetitions must be rejected")
	}
	raw, err := GetBulkRequest("public", 1, oids, 0, 10)
	if err != nil {
		t.Fatalf("valid getbulk: %v", err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("round trip: %v", err)
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

// Unsigned32 与 Gauge32 在 SMIv2 里是不可区分的同一个 ASN.1 类型，共用
// 线上标签 0x42（RFC 2578 §7.1.7）。历史上这里曾存在一个 0x47 的
// 「UInteger32」标签，但标准 SNMP 从未分配过 APPLICATION 7。这条用例钉住：
//   - Unsigned32 语义的值必须编成 0x42（不是 0x47）；
//   - 0x47 作为线上标签出现时，解析必须明确拒绝，而不是当成合法无符号值。
func TestUnsigned32SharesGauge32Tag(t *testing.T) {
	// Unsigned32 语义用 TypeGauge32 承载：编码必须是 42 01 2a（最短形式）。
	raw, err := encodeValue(Unsigned(TypeGauge32, 42))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x42, 0x01, 0x2a}; !bytes.Equal(raw, want) {
		t.Fatalf("unsigned32 = % x, want % x", raw, want)
	}
	// 回读必须得到 TypeGauge32（Unsigned32 没有独立标签）。
	v, err := parseValue(0x42, []byte{0x2a})
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TypeGauge32 || v.Uint != 42 {
		t.Fatalf("decoded = %+v", v)
	}
	// 0x47 不是标准 SNMP 标签：必须被拒绝。
	if _, err := parseValue(0x47, []byte{0x2a}); err == nil {
		t.Fatal("tag 0x47 must be rejected: it is not a valid SNMP application tag")
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
