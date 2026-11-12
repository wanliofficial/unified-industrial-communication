package snmp

import (
	"bytes"
	"strings"
	"testing"
)

// testKeys 派生一对匹配某 engineID 的认证/加密密钥。
func testKeys(t *testing.T, auth AuthProtocol, priv PrivProtocol, engineID []byte) *V3Keys {
	t.Helper()
	k := &V3Keys{Auth: auth, Priv: priv}
	if auth != AuthNone {
		ku, err := PasswordToKey(auth, "authpass123")
		if err != nil {
			t.Fatal(err)
		}
		if k.AuthKey, err = LocalizeKey(auth, ku, engineID); err != nil {
			t.Fatal(err)
		}
	}
	if priv != PrivNone {
		ku, err := PasswordToKey(auth, "privpass123")
		if err != nil {
			t.Fatal(err)
		}
		if k.PrivKey, err = LocalizeKey(auth, ku, engineID); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

var testEngine = []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x02}

// TestBuildParseV3RoundTrip 覆盖三个安全级别的报文往返。
func TestBuildParseV3RoundTrip(t *testing.T) {
	oids := []OID{MustParseOID("1.3.6.1.2.1.1.1.0")}
	cases := []struct {
		name  string
		auth  AuthProtocol
		priv  PrivProtocol
		flags MsgFlags
	}{
		{"noAuthNoPriv", AuthNone, PrivNone, FlagReportable},
		{"authNoPriv", AuthSHA1, PrivNone, FlagAuth | FlagReportable},
		{"authPriv", AuthSHA256, PrivAES, FlagAuth | FlagPriv | FlagReportable},
		{"authPrivDES", AuthMD5, PrivDES, FlagAuth | FlagPriv | FlagReportable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys := testKeys(t, c.auth, c.priv, testEngine)
			pdu, err := NewRequestPDU(PDUGet, 42, oids)
			if err != nil {
				t.Fatal(err)
			}
			scoped, err := BuildScopedPDU(testEngine, "", pdu)
			if err != nil {
				t.Fatal(err)
			}
			hdr := HeaderData{MsgID: 7, MaxSize: 1400, Flags: c.flags, SecModel: SecurityModelUSM}
			sec := USMSecurityParameters{
				EngineID: testEngine, EngineBoots: 1, EngineTime: 500, UserName: "monitor",
			}
			var authPriv *V3Keys
			if c.auth != AuthNone {
				authPriv = keys
			}
			raw, err := BuildV3(hdr, sec, scoped, authPriv)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			m, err := ParseV3(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if m.Header.MsgID != 7 || m.Header.Flags != c.flags || m.Header.SecModel != SecurityModelUSM {
				t.Fatalf("header = %+v", m.Header)
			}
			if m.Security.EngineBoots != 1 || m.Security.EngineTime != 500 || m.Security.UserName != "monitor" {
				t.Fatalf("security = %+v", m.Security)
			}
			if c.auth != AuthNone {
				if err := VerifyV3(raw, m.Security, c.auth, keys.AuthKey); err != nil {
					t.Fatalf("verify: %v", err)
				}
			}
			if m.Encrypted {
				if err := m.DecryptScoped(c.priv, keys.PrivKey); err != nil {
					t.Fatalf("decrypt: %v", err)
				}
			}
			if m.PDU.Type != PDUGet || m.PDU.RequestID != 42 || len(m.PDU.Varbinds) != 1 {
				t.Fatalf("pdu = %+v", m.PDU)
			}
			if m.PDU.Varbinds[0].OID.Compare(oids[0]) != 0 {
				t.Fatalf("varbind oid = %v", m.PDU.Varbinds[0].OID)
			}
		})
	}
}

// TestVerifyV3RejectsTampered 钉住「摘要覆盖整条报文」。
//
// 篡改任意一个字节（包括头、security parameters、密文）都必须被检出。
// 只验 payload 的实现能通过前两条测试，但拦不住改 msgMaxSize 之类的攻击。
func TestVerifyV3RejectsTampered(t *testing.T) {
	keys := testKeys(t, AuthSHA1, PrivNone, testEngine)
	pdu, _ := NewRequestPDU(PDUGet, 9, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	scoped, _ := BuildScopedPDU(testEngine, "", pdu)
	hdr := HeaderData{MsgID: 3, MaxSize: 1400, Flags: FlagAuth | FlagReportable, SecModel: SecurityModelUSM}
	sec := USMSecurityParameters{EngineID: testEngine, EngineBoots: 1, EngineTime: 100, UserName: "u"}
	raw, err := BuildV3(hdr, sec, scoped, keys)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ParseV3(raw)
	if err := VerifyV3(raw, m.Security, AuthSHA1, keys.AuthKey); err != nil {
		t.Fatalf("baseline verify: %v", err)
	}
	// 逐字节翻转，每一处都必须失败。
	bad := append([]byte(nil), raw...)
	for i := range bad {
		orig := bad[i]
		bad[i] = orig ^ 0x01
		mm, perr := ParseV3(bad)
		if perr == nil {
			if err := VerifyV3(bad, mm.Security, AuthSHA1, keys.AuthKey); err == nil {
				t.Fatalf("tampered byte %d was not detected", i)
			}
		}
		bad[i] = orig
	}
}

// TestVerifyV3WrongKey：密钥不对必须失败，且不是「结构错误」。
func TestVerifyV3WrongKey(t *testing.T) {
	keys := testKeys(t, AuthSHA256, PrivNone, testEngine)
	other := testKeys(t, AuthSHA256, PrivNone, []byte{0x80, 0, 0x1f, 0x88, 0x81})
	pdu, _ := NewRequestPDU(PDUGet, 1, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	scoped, _ := BuildScopedPDU(testEngine, "", pdu)
	hdr := HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagAuth, SecModel: SecurityModelUSM}
	sec := USMSecurityParameters{EngineID: testEngine, UserName: "u"}
	raw, err := BuildV3(hdr, sec, scoped, keys)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ParseV3(raw)
	if err := VerifyV3(raw, m.Security, AuthSHA256, other.AuthKey); err == nil {
		t.Fatal("verify with the wrong key must fail")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error = %v, want a digest mismatch", err)
	}
}

// TestBuildV3Rejects 覆盖构造期的自洽校验。
func TestBuildV3Rejects(t *testing.T) {
	keys := testKeys(t, AuthSHA1, PrivNone, testEngine)
	scoped, _ := BuildScopedPDU(testEngine, "", PDU{Type: PDUGet, RequestID: 1,
		Varbinds: []Varbind{{OID: MustParseOID("1.3.6.1.2.1.1.1.0"), Value: Null()}}})
	base := HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagReportable, SecModel: SecurityModelUSM}
	good := USMSecurityParameters{EngineID: testEngine, UserName: "u"}

	cases := []struct {
		name string
		h    HeaderData
		s    USMSecurityParameters
		k    *V3Keys
		hint string
	}{
		{"msgID zero", HeaderData{MsgID: 0, MaxSize: 1400, SecModel: SecurityModelUSM}, good, nil, "msgID"},
		{"maxSize too small", HeaderData{MsgID: 1, MaxSize: 100, SecModel: SecurityModelUSM}, good, nil, "484"},
		{"wrong security model", HeaderData{MsgID: 1, MaxSize: 1400, SecModel: 2}, good, nil, "security model"},
		{"empty engine with auth", HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagAuth | FlagReportable, SecModel: SecurityModelUSM},
			USMSecurityParameters{UserName: "u"}, keys, "empty engine ID"},
		{"short engine ID", base, USMSecurityParameters{EngineID: []byte{1, 2, 3}, UserName: "u"}, nil, "5..32"},
		{"priv without auth", HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagPriv, SecModel: SecurityModelUSM},
			good, keys, "privFlag requires authFlag"},
		{"auth without key", HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagAuth, SecModel: SecurityModelUSM},
			good, nil, "no authentication key"},
		{"priv without key", HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagAuth | FlagPriv, SecModel: SecurityModelUSM},
			good, keys, "no privacy key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildV3(c.h, c.s, scoped, c.k)
			if err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(err.Error(), c.hint) {
				t.Fatalf("error = %v, want %q", err, c.hint)
			}
		})
	}
}

// TestParseV3Rejects 覆盖解析期的结构校验。
func TestParseV3Rejects(t *testing.T) {
	keys := testKeys(t, AuthSHA1, PrivNone, testEngine)
	scoped, _ := BuildScopedPDU(testEngine, "", PDU{Type: PDUGet, RequestID: 1,
		Varbinds: []Varbind{{OID: MustParseOID("1.3.6.1.2.1.1.1.0"), Value: Null()}}})
	hdr := HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagAuth | FlagReportable, SecModel: SecurityModelUSM}
	sec := USMSecurityParameters{EngineID: testEngine, EngineBoots: 1, EngineTime: 5, UserName: "u"}
	good, err := BuildV3(hdr, sec, scoped, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseV3(good); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := ParseV3(nil); err == nil {
		t.Fatal("empty datagram must fail")
	}
	// 截断
	for _, n := range []int{1, 5, len(good) / 2, len(good) - 1} {
		if _, err := ParseV3(good[:n]); err == nil {
			t.Fatalf("truncated to %d bytes must fail", n)
		}
	}
	// 追加垃圾
	if _, err := ParseV3(append(append([]byte(nil), good...), 0x00)); err == nil {
		t.Fatal("trailing byte must fail")
	}
	// v2c 报文走 v3 解析器必须被拒
	v2, _ := GetRequest(V2c, "public", 1, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	if _, err := ParseV3(v2); err == nil {
		t.Fatal("v2c datagram must not parse as v3")
	}
	if _, err := Parse(good); err == nil {
		t.Fatal("v3 datagram must not parse as v2c")
	}
	// v3 报文走 v2c 解析器
	if _, err := Parse(good); err == nil {
		t.Fatal("v3 must not parse with Parse")
	}
}

// TestPeekVersion 钉住「按版本选解析器」。
func TestPeekVersion(t *testing.T) {
	v1, _ := GetRequest(V1, "public", 1, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	if v, err := PeekVersion(v1); err != nil || v != V1 {
		t.Fatalf("v1 = %v, %v", v, err)
	}
	v2, _ := GetRequest(V2c, "public", 1, []OID{MustParseOID("1.3.6.1.2.1.1.1.0")})
	if v, err := PeekVersion(v2); err != nil || v != V2c {
		t.Fatalf("v2c = %v, %v", v, err)
	}
	keys := testKeys(t, AuthSHA1, PrivNone, testEngine)
	scoped, _ := BuildScopedPDU(testEngine, "", PDU{Type: PDUGet, RequestID: 1,
		Varbinds: []Varbind{{OID: MustParseOID("1.3.6.1.2.1.1.1.0"), Value: Null()}}})
	raw, _ := BuildV3(HeaderData{MsgID: 1, MaxSize: 1400, Flags: FlagAuth, SecModel: SecurityModelUSM},
		USMSecurityParameters{EngineID: testEngine, UserName: "u"}, scoped, keys)
	if v, err := PeekVersion(raw); err != nil || v != V3 {
		t.Fatalf("v3 = %v, %v", v, err)
	}
	if _, err := PeekVersion([]byte{0x30, 0x03, 0x02, 0x01, 0x09}); err == nil {
		t.Fatal("version 9 must fail")
	}
	if _, err := PeekVersion(nil); err == nil {
		t.Fatal("empty must fail")
	}
}

// TestClassifyReport 覆盖 6 个 USM 计数器的分类。
func TestClassifyReport(t *testing.T) {
	cases := []struct {
		oid  OID
		want ReportKind
	}{
		{OIDUsmStatsUnsupportedSecLevels, ReportUnsupportedSecLevel},
		{OIDUsmStatsNotInTimeWindows, ReportNotInTimeWindow},
		{OIDUsmStatsUnknownUserNames, ReportUnknownUserName},
		{OIDUsmStatsUnknownEngineIDs, ReportUnknownEngineID},
		{OIDUsmStatsWrongDigests, ReportWrongDigest},
		{OIDUsmStatsDecryptionErrors, ReportDecryptionError},
	}
	for _, c := range cases {
		p := PDU{Type: PDUReport, Varbinds: []Varbind{{
			OID: c.oid, Value: Unsigned(TypeCounter32, 1),
		}}}
		if got := ClassifyReport(p); got != c.want {
			t.Errorf("%v = %v, want %v", c.oid, got, c.want)
		}
		if c.want.Hint() == "" {
			t.Errorf("%v has no hint", c.want)
		}
	}
	// 非 Report 一律 unknown。
	if got := ClassifyReport(PDU{Type: PDUResponse}); got != ReportUnknown {
		t.Errorf("response = %v", got)
	}
	// 未知 varbind 也是 unknown（不能把业务 OID 误当诊断）。
	if got := ClassifyReport(PDU{Type: PDUReport, Varbinds: []Varbind{{
		OID: MustParseOID("1.3.6.1.2.1.1.1.0"), Value: Integer(1),
	}}}); got != ReportUnknown {
		t.Errorf("business OID = %v", got)
	}
}

// TestCheckTimeWindow 覆盖窗口判定。
func TestCheckTimeWindow(t *testing.T) {
	if reason, ok := CheckTimeWindow(5, 100, 5, 100); !ok {
		t.Fatalf("same time rejected: %s", reason)
	}
	if reason, ok := CheckTimeWindow(5, 200, 5, 100); !ok {
		t.Fatalf("%ds drift rejected: %s", TimeWindow, reason)
	}
	if _, ok := CheckTimeWindow(5, 100+TimeWindow+1, 5, 100); ok {
		t.Fatal("drift beyond the window accepted")
	}
	if reason, ok := CheckTimeWindow(6, 100, 5, 100); ok || !strings.Contains(reason, "engineBoots") {
		t.Fatalf("boots mismatch = %q, %v", reason, ok)
	}
}

// TestScopedPDURoundTrip 覆盖 ScopedPDU 的编解码，含 contextName。
func TestScopedPDURoundTrip(t *testing.T) {
	pdu := PDU{Type: PDUGet, RequestID: -7, Varbinds: []Varbind{
		{OID: MustParseOID("1.3.6.1.2.1.1.1.0"), Value: Null()},
	}}
	raw, err := BuildScopedPDU(testEngine, "ctxA", pdu)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ParseScopedPDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ContextName != "ctxA" || !bytes.Equal(sc.ContextEngineID, testEngine) {
		t.Fatalf("context = %q %x", sc.ContextName, sc.ContextEngineID)
	}
	if sc.PDU.RequestID != -7 || len(sc.PDU.Varbinds) != 1 {
		t.Fatalf("pdu = %+v", sc.PDU)
	}
	// 追加垃圾必须被拒
	if _, err := ParseScopedPDU(append(append([]byte(nil), raw...), 0x00)); err == nil {
		t.Fatal("trailing byte must fail")
	}
}

// TestOIDUsmStatsAreValid 钉住计数器的 OID 拼写。
//
// 这几个 OID 手抄容易错一位，而错一位的表现是「Report 收到了，但
// 分类成 unknown」，比完全没有 Report 更难查。
func TestOIDUsmStatsAreValid(t *testing.T) {
	want := map[string]OID{
		"1.3.6.1.6.3.15.1.1.1.0": OIDUsmStatsUnsupportedSecLevels,
		"1.3.6.1.6.3.15.1.1.2.0": OIDUsmStatsNotInTimeWindows,
		"1.3.6.1.6.3.15.1.1.3.0": OIDUsmStatsUnknownUserNames,
		"1.3.6.1.6.3.15.1.1.4.0": OIDUsmStatsUnknownEngineIDs,
		"1.3.6.1.6.3.15.1.1.5.0": OIDUsmStatsWrongDigests,
		"1.3.6.1.6.3.15.1.1.6.0": OIDUsmStatsDecryptionErrors,
	}
	for s, oid := range want {
		if oid.String() != s {
			t.Errorf("%s = %s", s, oid)
		}
		if err := oid.Validate(); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

// TestMsgFlags 覆盖三位的读取与安全级别命名。
func TestMsgFlags(t *testing.T) {
	if got := (FlagAuth | FlagPriv | FlagReportable).SecurityLevel(); got != "authPriv" {
		t.Errorf("authPriv = %q", got)
	}
	if got := FlagAuth.SecurityLevel(); got != "authNoPriv" {
		t.Errorf("authNoPriv = %q", got)
	}
	if got := MsgFlags(0).SecurityLevel(); got != "noAuthNoPriv" {
		t.Errorf("noAuthNoPriv = %q", got)
	}
	if (FlagAuth | FlagPriv | FlagReportable).String() != "auth,priv,reportable" {
		t.Errorf("String = %q", (FlagAuth | FlagPriv | FlagReportable).String())
	}
}
