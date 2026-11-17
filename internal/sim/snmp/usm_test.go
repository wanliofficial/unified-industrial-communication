package snmp

import (
	"bytes"
	"testing"
	"time"

	snmpcodec "uic/internal/codec/snmp"
)

// 本文件测 v3/USM 的代理侧行为。
//
// 测法与 server_test.go 相同：真实 UDP 回环 + 真实 codec。代理侧最容易
// 写错的地方是「以为在校验、实际只看了用户名」——那类实现在自测里能通过
// （因为客户端也只看用户名），在真机上表现为「怎么配都通」。只有让测试
// 主动构造错误摘要/错误密钥，才能逼出「其实没校验」的实现。

// v3Client 是 v3 测试用的最小客户端：不做发现缓存，每步都显式构造。
type v3Client struct {
	t       *testing.T
	srv     *Server
	engine  []byte
	boots   int
	etime   int
	msgID   int32
	auth    snmpcodec.AuthProtocol
	priv    snmpcodec.PrivProtocol
	authKey []byte
	privKey []byte
}

// newV3Client 用代理的 engineID 派生出一套密钥。口令与代理侧一致，
// 这样「正常路径」能通；测试要制造失败时直接改 c.authKey。
func newV3Client(t *testing.T, srv *Server, user string, auth snmpcodec.AuthProtocol, authPass string, priv snmpcodec.PrivProtocol, privPass string) *v3Client {
	t.Helper()
	c := &v3Client{t: t, srv: srv, engine: srv.EngineID(), boots: 1, etime: 100, auth: auth, priv: priv}
	var err error
	if auth != snmpcodec.AuthNone {
		ku, kerr := snmpcodec.PasswordToKey(auth, authPass)
		if kerr != nil {
			t.Fatalf("auth key: %v", kerr)
		}
		if c.authKey, err = snmpcodec.LocalizeKey(auth, ku, c.engine); err != nil {
			t.Fatalf("localize auth: %v", err)
		}
	}
	if priv != snmpcodec.PrivNone {
		ku, kerr := snmpcodec.PasswordToKey(auth, privPass)
		if kerr != nil {
			t.Fatalf("priv key: %v", kerr)
		}
		if c.privKey, err = snmpcodec.LocalizeKey(auth, ku, c.engine); err != nil {
			t.Fatalf("localize priv: %v", err)
		}
	}
	return c
}

// build 组一条 v3 请求报文。
func (c *v3Client) build(pdu snmpcodec.PDU) []byte {
	c.t.Helper()
	c.msgID++
	c.etime++ // 每条请求推进一秒，模拟真实主站
	scoped, err := snmpcodec.BuildScopedPDU(c.engine, "", pdu)
	if err != nil {
		c.t.Fatalf("scoped: %v", err)
	}
	flags := snmpcodec.FlagReportable
	if c.auth != snmpcodec.AuthNone {
		flags |= snmpcodec.FlagAuth
	}
	if c.priv != snmpcodec.PrivNone {
		flags |= snmpcodec.FlagPriv
	}
	hdr := snmpcodec.HeaderData{MsgID: c.msgID, MaxSize: 1400, Flags: flags, SecModel: snmpcodec.SecurityModelUSM}
	sec := snmpcodec.USMSecurityParameters{
		EngineID: c.engine, EngineBoots: c.boots, EngineTime: c.etime, UserName: "monitor",
	}
	var keys *snmpcodec.V3Keys
	if c.auth != snmpcodec.AuthNone {
		keys = &snmpcodec.V3Keys{Auth: c.auth, AuthKey: c.authKey, Priv: c.priv, PrivKey: c.privKey}
	}
	raw, err := snmpcodec.BuildV3(hdr, sec, scoped, keys)
	if err != nil {
		c.t.Fatalf("build: %v", err)
	}
	return raw
}

// round 发一条 Get 并解析应答。
func (c *v3Client) round(oid string) (snmpcodec.V3Message, error) {
	c.t.Helper()
	pdu, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 11, []snmpcodec.OID{snmpcodec.MustParseOID(oid)})
	if err != nil {
		c.t.Fatalf("pdu: %v", err)
	}
	raw := c.build(pdu)
	cl := dialAgent(c.t, c.srv)
	if err := cl.conn.SetDeadline(time.Now().Add(1500 * time.Millisecond)); err != nil {
		c.t.Fatalf("deadline: %v", err)
	}
	if err := cl.conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond)); err != nil {
		c.t.Fatalf("read deadline: %v", err)
	}
	if _, err := cl.conn.Write(raw); err != nil {
		return snmpcodec.V3Message{}, err
	}
	buf := make([]byte, snmpcodec.MaxDatagram)
	n, err := cl.conn.Read(buf)
	if err != nil {
		return snmpcodec.V3Message{}, err
	}
	m, err := snmpcodec.ParseV3(buf[:n])
	if err != nil {
		c.t.Fatalf("parse reply: %v", err)
	}
	// 验签（如果应答带了认证）：这一步顺带证明代理用对了密钥。
	if m.Header.Flags.HasAuth() {
		if err := snmpcodec.VerifyV3(buf[:n], m.Security, c.auth, c.authKey); err != nil {
			c.t.Fatalf("reply signature: %v", err)
		}
	}
	if m.Encrypted {
		if err := m.DecryptScoped(c.priv, c.privKey); err != nil {
			c.t.Fatalf("decrypt reply: %v", err)
		}
	}
	return m, nil
}

// sendV3 发一条 v3 报文并解析 v3 应答（server_test.go 的 send 只解 v1/v2c）。
func sendV3(t *testing.T, srv *Server, raw []byte) snmpcodec.V3Message {
	t.Helper()
	cl := dialAgent(t, srv)
	if _, err := cl.conn.Write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := cl.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	buf := make([]byte, snmpcodec.MaxDatagram)
	n, err := cl.conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v (reports=%d dropped=%d)", err, srv.Reports(), srv.Dropped())
	}
	m, err := snmpcodec.ParseV3(buf[:n])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func v3Sim(t *testing.T, opt Options) *Server {
	t.Helper()
	if opt.V3 == nil {
		opt.V3 = &USMConfig{}
	}
	return startAgent(t, opt)
}

// TestV3DiscoveryReport：空 engineID 的请求必须换回一条带引擎身份的 Report。
//
// 这是 v3 的第一步，也是最容易被漏掉的一步：如果代理对空 engineID 直接
// 丢弃，主站就只能靠超时猜，而超时的原因有十几种。
func TestV3DiscoveryReport(t *testing.T) {
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "authpass123"},
	}}})

	pdu, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 3, []snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := snmpcodec.BuildScopedPDU(nil, "", pdu)
	if err != nil {
		t.Fatal(err)
	}
	hdr := snmpcodec.HeaderData{MsgID: 1, MaxSize: 1400, Flags: snmpcodec.FlagReportable, SecModel: snmpcodec.SecurityModelUSM}
	raw, err := snmpcodec.BuildV3(hdr, snmpcodec.USMSecurityParameters{UserName: "monitor"}, scoped, nil)
	if err != nil {
		t.Fatal(err)
	}

	m := sendV3(t, srv, raw)
	if m.PDU.Type != snmpcodec.PDUReport {
		t.Fatalf("pdu = %s, want Report", m.PDU.Type)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportUnknownEngineID {
		t.Fatalf("report kind = %v", got)
	}
	if !bytes.Equal(m.Security.EngineID, srv.EngineID()) {
		t.Fatalf("engine = %x, want %x", m.Security.EngineID, srv.EngineID())
	}
	if m.Security.EngineBoots != 1 || m.Security.EngineTime != 100 {
		t.Fatalf("boots/time = %d/%d", m.Security.EngineBoots, m.Security.EngineTime)
	}
	// 发现报文不带认证：主站此时没有密钥，签了也验不了。
	if m.Header.Flags.HasAuth() || m.Header.Flags.HasPriv() {
		t.Fatalf("discovery report flags = %v", m.Header.Flags)
	}
	if srv.Reports() != 1 {
		t.Fatalf("reports = %d", srv.Reports())
	}
}

// TestV3AuthPrivRoundTrip 是代理侧的主路径：三档安全级别都要能读。
func TestV3AuthPrivRoundTrip(t *testing.T) {
	const authPass, privPass = "authpass123", "privpass123"
	cases := []struct {
		name string
		auth snmpcodec.AuthProtocol
		priv snmpcodec.PrivProtocol
	}{
		{"noAuthNoPriv", snmpcodec.AuthNone, snmpcodec.PrivNone},
		{"authNoPriv-MD5", snmpcodec.AuthMD5, snmpcodec.PrivNone},
		{"authNoPriv-SHA512", snmpcodec.AuthSHA512, snmpcodec.PrivNone},
		{"authPriv-DES", snmpcodec.AuthSHA1, snmpcodec.PrivDES},
		{"authPriv-AES128", snmpcodec.AuthSHA1, snmpcodec.PrivAES},
		{"authPriv-AES192", snmpcodec.AuthSHA256, snmpcodec.PrivAES192},
		{"authPriv-AES256", snmpcodec.AuthSHA512, snmpcodec.PrivAES256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
				"monitor": {Name: "monitor", Auth: c.auth, AuthPassword: authPass, Priv: c.priv, PrivPassword: privPass},
			}}})
			cl := newV3Client(t, srv, "monitor", c.auth, authPass, c.priv, privPass)
			m, err := cl.round("1.3.6.1.2.1.1.1.0")
			if err != nil {
				t.Fatalf("round: %v", err)
			}
			if m.PDU.Type != snmpcodec.PDUResponse {
				t.Fatalf("pdu = %s (%v)", m.PDU.Type, snmpcodec.ClassifyReport(m.PDU))
			}
			if len(m.PDU.Varbinds) != 1 || len(m.PDU.Varbinds[0].Value.Bytes) == 0 {
				t.Fatalf("varbinds = %+v", m.PDU.Varbinds)
			}
			// 应答的安全级别必须与请求一致：降级应答会让主站把明文
			// 当密文解。
			if got, want := m.Header.Flags.HasPriv(), c.priv != snmpcodec.PrivNone; got != want {
				t.Fatalf("reply priv = %v, want %v", got, want)
			}
			if got, want := m.Header.Flags.HasAuth(), c.auth != snmpcodec.AuthNone; got != want {
				t.Fatalf("reply auth = %v, want %v", got, want)
			}
		})
	}
}

// TestV3WrongDigestReport：摘要错必须回 wrongDigests，且 **不签名**。
//
// 不签名的理由：我们与对方密钥不一致，签出来的东西对方也验不过。
// 更重要的是，主站看到 wrongDigests 就知道去查口令，而不是查网络。
func TestV3WrongDigestReport(t *testing.T) {
	const authPass = "authpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: authPass},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA1, authPass, snmpcodec.PrivNone, "")
	// 换一个字节让摘要对不上（但结构仍然合法）。
	cl.authKey = append([]byte(nil), cl.authKey...)
	cl.authKey[0] ^= 0xFF

	m, err := cl.round("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if m.PDU.Type != snmpcodec.PDUReport {
		t.Fatalf("pdu = %s, want Report", m.PDU.Type)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportWrongDigest {
		t.Fatalf("report = %v", got)
	}
	if m.Header.Flags.HasAuth() {
		t.Fatal("wrongDigest report must not be signed (keys do not match)")
	}
}

// TestV3FaultInjection 覆盖 4 个故障开关。
func TestV3FaultInjection(t *testing.T) {
	const authPass, privPass = "authpass123", "privpass123"
	cases := []struct {
		name   string
		faults USMFaults
		want   snmpcodec.ReportKind
	}{
		{"WrongDigest", USMFaults{WrongDigest: true}, snmpcodec.ReportWrongDigest},
		{"NotInTimeWindow", USMFaults{NotInTimeWindow: true}, snmpcodec.ReportNotInTimeWindow},
		{"UnsupportedSecLevel", USMFaults{UnsupportedSecLevel: true}, snmpcodec.ReportUnsupportedSecLevel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := v3Sim(t, Options{V3: &USMConfig{
				Users: map[string]USMUser{
					"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: authPass,
						Priv: snmpcodec.PrivAES, PrivPassword: privPass},
				},
				Faults: c.faults,
			}})
			cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA256, authPass, snmpcodec.PrivAES, privPass)
			m, err := cl.round("1.3.6.1.2.1.1.1.0")
			if err != nil {
				t.Fatalf("round: %v", err)
			}
			if m.PDU.Type != snmpcodec.PDUReport {
				t.Fatalf("pdu = %s (%+v)", m.PDU.Type, m.PDU)
			}
			if got := snmpcodec.ClassifyReport(m.PDU); got != c.want {
				t.Fatalf("report = %v, want %v", got, c.want)
			}
		})
	}
}

// TestV3WrongPrivKeyFails：加密口令错时主站拿不到数据。
//
// 这里不能断言「一定收到 decryptionError Report」：AES-CFB 没有完整性校验，
// 错密钥解出来的是一段长度正确的垃圾，代理只有在解析 ScopedPDU 时才发现，
// 那时它已经无从判断「是密钥错还是报文错」，真机通常直接丢弃。DES-CBC
// 同理（填充值未规定，读出垃圾不报错）。
//
// 所以这里断言的是**可观测事实**：没有成功应答，且代理要么静默丢弃、
// 要么回了 decryptionError。两种都算正确实现。
func TestV3WrongPrivKeyFails(t *testing.T) {
	const authPass, privPass = "authpass123", "privpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: authPass,
			Priv: snmpcodec.PrivAES, PrivPassword: privPass},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA256, authPass, snmpcodec.PrivAES, privPass)
	// 认证密钥对（能过验签），加密密钥错（解出来是垃圾）。
	cl.privKey = append([]byte(nil), cl.privKey...)
	cl.privKey[0] ^= 0xFF

	pdu, _ := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 1, []snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	raw := cl.build(pdu)
	cm := dialAgent(t, srv)
	if err := cm.conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := cm.conn.Write(raw); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, snmpcodec.MaxDatagram)
	n, err := cm.conn.Read(buf)
	if err != nil {
		// 静默丢弃：这在真机上是常见行为，可以接受。
		if srv.Dropped() == 0 {
			t.Fatal("no response and nothing was dropped: the agent swallowed the request silently")
		}
		return
	}
	m, perr := snmpcodec.ParseV3(buf[:n])
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	if m.PDU.Type != snmpcodec.PDUReport {
		t.Fatalf("got %s for a wrong priv key", m.PDU.Type)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportDecryptionError {
		t.Fatalf("report = %v", got)
	}
}

// TestV3DecryptErrorReportIsSigned 单独钉住 Report 的签名策略。
//
// 为什么要单测这个构造器：解密失败的 Report 若不带签名，任何人无需密钥
// 就能伪造它，让主站长期误报「加密口令错」。而「认证前的 Report 不签名」
// 是另一种情况（我们与对方密钥都不一致，签了也验不了）。两种策略必须
// 分开，用构造器直接断言比走网络更直白。
func TestV3DecryptErrorReportIsSigned(t *testing.T) {
	const authPass = "authpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: authPass},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA256, authPass, snmpcodec.PrivNone, "")

	req, err := snmpcodec.ParseV3(cl.build(snmpcodec.PDU{Type: snmpcodec.PDUGet, RequestID: 5,
		Varbinds: []snmpcodec.Varbind{{OID: snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0"), Value: snmpcodec.Null()}}}))
	if err != nil {
		t.Fatal(err)
	}
	// 带 authKey：必须签名。
	raw := srv.buildReport(req, srv.EngineID(), 1, 100, snmpcodec.OIDUsmStatsDecryptionErrors, 1,
		snmpcodec.AuthSHA256, cl.authKey)
	m, err := snmpcodec.ParseV3(raw)
	if err != nil {
		t.Fatalf("parse signed report: %v", err)
	}
	if !m.Header.Flags.HasAuth() {
		t.Fatal("a Report built with an auth key must be signed")
	}
	if err := snmpcodec.VerifyV3(raw, m.Security, snmpcodec.AuthSHA256, cl.authKey); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportDecryptionError {
		t.Fatalf("report = %v", got)
	}
	// 不带 authKey：必须不签名（认证前的 Report）。
	raw = srv.buildReport(req, srv.EngineID(), 1, 100, snmpcodec.OIDUsmStatsUnknownEngineIDs, 1,
		snmpcodec.AuthNone, nil)
	m, err = snmpcodec.ParseV3(raw)
	if err != nil {
		t.Fatalf("parse unsigned report: %v", err)
	}
	if m.Header.Flags.HasAuth() {
		t.Fatal("a Report built without a key must not be signed")
	}
}

// TestV3UnknownEngineID：拿着过期的 engineID 来，必须回 unknownEngineIDs
// 并带上当前身份，主站据此刷新。
func TestV3UnknownEngineID(t *testing.T) {
	const authPass = "authpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: authPass},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA1, authPass, snmpcodec.PrivNone, "")
	// 一个合法但不是本代理的 engineID。
	stale := []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x99}
	cl.engine = stale
	ku, _ := snmpcodec.PasswordToKey(snmpcodec.AuthSHA1, authPass)
	cl.authKey, _ = snmpcodec.LocalizeKey(snmpcodec.AuthSHA1, ku, stale)

	m, err := cl.round("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportUnknownEngineID {
		t.Fatalf("report = %v", got)
	}
	if !bytes.Equal(m.Security.EngineID, srv.EngineID()) {
		t.Fatalf("report engine = %x, want the agent's %x", m.Security.EngineID, srv.EngineID())
	}
}

// TestV3UnknownUser / TestV3SecLevelMismatch：两条「配置错」路径。
func TestV3UnknownUser(t *testing.T) {
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "authpass123"},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA1, "authpass123", snmpcodec.PrivNone, "")
	// 用未知用户名重发：这里直接改报文里的用户名，绕过 build 的固定值。
	pdu, _ := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 1, []snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	scoped, _ := snmpcodec.BuildScopedPDU(cl.engine, "", pdu)
	hdr := snmpcodec.HeaderData{MsgID: 1, MaxSize: 1400, Flags: snmpcodec.FlagAuth | snmpcodec.FlagReportable,
		SecModel: snmpcodec.SecurityModelUSM}
	sec := snmpcodec.USMSecurityParameters{EngineID: cl.engine, EngineBoots: 1, EngineTime: 100, UserName: "ghost"}
	raw, err := snmpcodec.BuildV3(hdr, sec, scoped, &snmpcodec.V3Keys{Auth: snmpcodec.AuthSHA1, AuthKey: cl.authKey})
	if err != nil {
		t.Fatal(err)
	}
	m := sendV3(t, srv, raw)
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportUnknownUserName {
		t.Fatalf("report = %v", got)
	}
}

func TestV3SecLevelMismatch(t *testing.T) {
	const authPass, privPass = "authpass123", "privpass123"
	// 代理侧用户只配 authNoPriv。
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: authPass},
	}}})
	// 客户端却配 authPriv。
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA256, authPass, snmpcodec.PrivAES, privPass)
	m, err := cl.round("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportUnsupportedSecLevel {
		t.Fatalf("report = %v", got)
	}
}

// TestV3TimeWindowRejectsStaleBoots：engineBoots 变了（代理重启）必须拒绝。
func TestV3TimeWindowRejectsStaleBoots(t *testing.T) {
	const authPass = "authpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: authPass},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA1, authPass, snmpcodec.PrivNone, "")
	cl.boots = 7 // 代理是 1
	m, err := cl.round("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportNotInTimeWindow {
		t.Fatalf("report = %v", got)
	}
	// 窗口外的 engineTime 同样拒绝。
	cl.boots = 1
	cl.etime = 100 + snmpcodec.TimeWindow + 5
	m, err = cl.round("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := snmpcodec.ClassifyReport(m.PDU); got != snmpcodec.ReportNotInTimeWindow {
		t.Fatalf("stale time report = %v", got)
	}
}

// TestV3DropIsSilent：DropPercent=100 时一条也不回（模拟真机对坏摘要的处理）。
func TestV3DropIsSilent(t *testing.T) {
	const authPass = "authpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{
		Users: map[string]USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: authPass},
		},
		Faults: USMFaults{DropPercent: 100},
	}})
	cl := dialAgent(t, srv)
	pdu, _ := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 1, []snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	scoped, _ := snmpcodec.BuildScopedPDU(nil, "", pdu)
	raw, _ := snmpcodec.BuildV3(
		snmpcodec.HeaderData{MsgID: 1, MaxSize: 1400, Flags: snmpcodec.FlagReportable, SecModel: snmpcodec.SecurityModelUSM},
		snmpcodec.USMSecurityParameters{UserName: "monitor"}, scoped, nil)
	if _, err := cl.send(raw, 300*time.Millisecond); err == nil {
		t.Fatal("expected a read timeout on a dropped datagram")
	}
	if srv.Dropped() == 0 {
		t.Fatal("dropped counter did not move")
	}
}

// TestV3WithoutUSMIsDropped：没开 v3 的代理对 v3 报文一律静默丢弃。
func TestV3WithoutUSMIsDropped(t *testing.T) {
	srv := startAgent(t, Options{})
	pdu, _ := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 1, []snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	scoped, _ := snmpcodec.BuildScopedPDU(nil, "", pdu)
	raw, _ := snmpcodec.BuildV3(
		snmpcodec.HeaderData{MsgID: 1, MaxSize: 1400, Flags: snmpcodec.FlagReportable, SecModel: snmpcodec.SecurityModelUSM},
		snmpcodec.USMSecurityParameters{UserName: "monitor"}, scoped, nil)
	if _, err := dialAgent(t, srv).send(raw, 300*time.Millisecond); err == nil {
		t.Fatal("v3 datagram must be dropped when USM is not configured")
	}
}

// TestV3CoexistsWithCommunity：开了 v3 的代理仍然接受 v1/v2c（真机常见默认）。
func TestV3CoexistsWithCommunity(t *testing.T) {
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "authpass123"},
	}}})
	m := dialAgent(t, srv).mustSend(getReq(t, snmpcodec.V2c, DefaultCommunity, "1.3.6.1.2.1.1.1.0"))
	if m.PDU.Type != snmpcodec.PDUResponse || len(m.PDU.Varbinds) != 1 {
		t.Fatalf("v2c on a v3 agent = %+v", m.PDU)
	}
}

// TestV3OnlyDropsCommunity：只开 v3 的代理对 v1/v2c 一律静默丢弃，
// 而 v3 仍然正常。
//
// 这条开关不是「为了完整」：真机上「USM 配了、community 映射没配」很常见，
// 此时 v1/v2c 客户端拿到的是超时，运维最容易归因到网络/端口。测试要能
// 复现这种设备，驱动那边的诊断文案才有意义。
func TestV3OnlyDropsCommunity(t *testing.T) {
	srv := v3Sim(t, Options{V3Only: true, V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "authpass123"},
	}}})
	if _, err := dialAgent(t, srv).send(getReq(t, snmpcodec.V2c, DefaultCommunity, "1.3.6.1.2.1.1.1.0"), 300*time.Millisecond); err == nil {
		t.Fatal("v2c must be dropped on a v3-only agent")
	}
	if srv.Dropped() == 0 {
		t.Fatal("dropped counter did not move")
	}

	// v3 不受影响：空 engineID 的发现请求仍然换回 Report。
	pdu, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 3, []snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := snmpcodec.BuildScopedPDU(nil, "", pdu)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := snmpcodec.BuildV3(
		snmpcodec.HeaderData{MsgID: 1, MaxSize: 1400, Flags: snmpcodec.FlagReportable, SecModel: snmpcodec.SecurityModelUSM},
		snmpcodec.USMSecurityParameters{UserName: "monitor"}, scoped, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := sendV3(t, srv, raw); m.PDU.Type != snmpcodec.PDUReport {
		t.Fatalf("v3 on a v3-only agent = %s", m.PDU.Type)
	}
}

// TestV3EngineIDOverride：配置里给了 engineID 就用它。
func TestV3EngineIDOverride(t *testing.T) {
	custom := []byte{0x80, 0x00, 0x1F, 0x88, 0x04, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	srv := v3Sim(t, Options{V3: &USMConfig{
		EngineID: custom, EngineBoots: 3, EngineTime: 42,
		Users: map[string]USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "authpass123"},
		},
	}})
	if !bytes.Equal(srv.EngineID(), custom) {
		t.Fatalf("engine = %x", srv.EngineID())
	}
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA1, "authpass123", snmpcodec.PrivNone, "")
	cl.boots, cl.etime = 3, 42
	m, err := cl.round("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if m.PDU.Type != snmpcodec.PDUResponse {
		t.Fatalf("pdu = %s (%v)", m.PDU.Type, snmpcodec.ClassifyReport(m.PDU))
	}
	if m.Security.EngineBoots != 3 || m.Security.EngineTime < 42 {
		t.Fatalf("boots/time = %d/%d", m.Security.EngineBoots, m.Security.EngineTime)
	}
}

// TestV3SetThroughUSM：写操作走同一条安全通道，且真的落到表里。
func TestV3SetThroughUSM(t *testing.T) {
	const authPass, privPass = "authpass123", "privpass123"
	srv := v3Sim(t, Options{V3: &USMConfig{Users: map[string]USMUser{
		"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: authPass,
			Priv: snmpcodec.PrivAES, PrivPassword: privPass},
	}}})
	cl := newV3Client(t, srv, "monitor", snmpcodec.AuthSHA256, authPass, snmpcodec.PrivAES, privPass)

	oid := snmpcodec.MustParseOID("1.3.6.1.2.1.1.4.0")
	pdu, err := snmpcodec.NewSetPDU(21, []snmpcodec.Varbind{{OID: oid, Value: snmpcodec.String("noc@example.invalid")}})
	if err != nil {
		t.Fatal(err)
	}
	raw := cl.build(pdu)
	m := sendV3(t, srv, raw)
	if err := m.DecryptScoped(cl.priv, cl.privKey); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if m.PDU.Type != snmpcodec.PDUResponse || m.PDU.ErrorStatus != 0 {
		t.Fatalf("set response = %+v", m.PDU)
	}
	v, ok := srv.Get("1.3.6.1.2.1.1.4.0")
	if !ok || string(v.Bytes) != "noc@example.invalid" {
		t.Fatalf("stored value = %+v", v)
	}
	if srv.Writes() != 1 {
		t.Fatalf("writes = %d", srv.Writes())
	}
}
