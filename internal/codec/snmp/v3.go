package snmp

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"
)

// 本文件是 SNMPv3 的报文层：msgGlobalData + msgSecurityParameters + ScopedPDU。
//
// v3 与 v1/v2c 的关系容易被误解成「v2c 加个密码」，实际只有最内层的 PDU 是
// 共用的，外面全是新的：
//
//	Message ::= SEQUENCE {
//	    msgVersion            INTEGER (3),
//	    msgGlobalData         HeaderData,
//	    msgSecurityParameters OCTET STRING,   -- USM 参数的 BER 编码，**再套一层**
//	    msgData               ScopedPDU | EncryptedPDU
//	}
//
// 两处「多套一层」是初学 USM 最常见的失败点：
//
//  1. msgSecurityParameters 的类型是 **OCTET STRING，内容是 BER**。直接把它
//     当 SEQUENCE 写出去，代理会把这个字节串当成密文，报「无法解析」。
//  2. 加密时 msgData 的内容是**原始密文字节**，不是 OCTET STRING 包一层。
//     多包一层，解密后得到 `04 82 …` 开头的东西，ASN.1 解析立刻失败。
//
// 还有一处只在出错时可见：msgFlags 的三位（auth / priv / reportable）必须
// 与「实际填了什么」一致。authFlag=1 但 authParameters 全 0，代理会算一遍
// 摘要再丢弃；privFlag=1 但 authFlag=0 是非法的（加密必须伴随认证），代理
// 回 usmStatsUnsupportedSecLevels 而不是解密失败——两种错误看起来都像「认证
// 配置错了」。

// USMSecurityParameters 是 USM 的安全参数。
//
// 字段顺序是线上的顺序，不能调换：USM 把它们编成一个 SEQUENCE 放进 OCTET
// STRING，双方按位置读，没有标签可依赖。
type USMSecurityParameters struct {
	EngineID       []byte
	EngineBoots    int
	EngineTime     int
	UserName       string
	AuthParameters []byte
	PrivParameters []byte
}

// MsgFlags 是 v3 头部的三位标志。
type MsgFlags byte

const (
	// FlagAuth 表示报文带认证摘要。
	FlagAuth MsgFlags = 0x01
	// FlagPriv 表示 payload 已加密。
	FlagPriv MsgFlags = 0x02
	// FlagReportable 表示出问题时希望对端回一个 Report PDU。
	//
	// 每一条请求都该置它：引擎发现（engineID 未知）与时间窗口错误
	// （notInTimeWindow）都只能靠 Report 得知。应答不能置它——对 Report
	// 再要求回 Report 是无意义的循环。
	FlagReportable MsgFlags = 0x04
)

func (f MsgFlags) String() string {
	s := ""
	if f&FlagAuth != 0 {
		s += "auth,"
	}
	if f&FlagPriv != 0 {
		s += "priv,"
	}
	if f&FlagReportable != 0 {
		s += "reportable,"
	}
	if s == "" {
		return "none"
	}
	return s[:len(s)-1]
}

// HasAuth / HasPriv / HasReportable 是三位标志的读取器。
//
// 用方法而不是让调用方写 `f&FlagAuth != 0`：位运算与「安全级别」是两个
// 概念，散落在各处的位运算迟早会有一处写成 `f&FlagPriv != 0` 判认证。
func (f MsgFlags) HasAuth() bool       { return f&FlagAuth != 0 }
func (f MsgFlags) HasPriv() bool       { return f&FlagPriv != 0 }
func (f MsgFlags) HasReportable() bool { return f&FlagReportable != 0 }

// SecurityLevel 返回人读的安全级别（noAuthNoPriv / authNoPriv / authPriv）。
func (f MsgFlags) SecurityLevel() string {
	switch {
	case f.HasPriv():
		return "authPriv"
	case f.HasAuth():
		return "authNoPriv"
	}
	return "noAuthNoPriv"
}

// HeaderData 是 msgGlobalData。
type HeaderData struct {
	MsgID    int32
	MaxSize  int
	Flags    MsgFlags
	SecModel int
}

// SecurityModelUSM 是 USM 的安全模型编号。别的编号（如 TSM）本实现不支持。
const SecurityModelUSM = 3

// V3Message 是一条解析后的 v3 报文。
type V3Message struct {
	Header   HeaderData
	Security USMSecurityParameters

	// ScopedPDU 是解密后的 ScopedPDU 原文（不含任何外层标签）。
	// 加密报文在 ParseV3 阶段保持密文，由调用方在拿到密钥后再解。
	ScopedPDU []byte
	// Encrypted 表示 ScopedPDU 是不是密文。
	Encrypted bool
	// RawScoped 是 msgData 的原始内容（密文或明文），用于校验摘要。
	RawScoped []byte
	// PDU 是解出的 PDU。明文报文在 ParseV3 里就解析好；密文报文要等
	// DecryptScoped 之后才有值。
	PDU PDU
	// ContextEngineID / ContextName 是 ScopedPDU 的上下文（明文时可用）。
	ContextEngineID []byte
	ContextName     string
}

// ScopedPDU 是 v3 的内层：contextEngineID + contextName + PDU。
type ScopedPDU struct {
	ContextEngineID []byte
	ContextName     string
	PDU             PDU
}

// engineIDPrefix 是本端（管理器）自动生成的 engineID 前缀。
//
// USM 规定 engineID 至少 5 字节、最多 32 字节，且第 1 字节的高位决定格式。
// 0x80 开头表示「由企业自行定义」，后面 4 字节是企业号。管理器作为
// authoritative engine 只在「接收 Trap/Inform」时才会用到自己的 engineID；
// 本实现是轮询主站，本端 engineID 只用于 Report 的对称场景，取一个稳定的
// 构造即可（必须非空：空 engineID 在部分代理上会被判成畸形）。
var managerEngineID = []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x01}

// BuildV3 组一条 v3 报文。
//
// authKey / privKey 都是**本地化之后的**密钥（调用方先用 LocalizeKey 派生）。
// 传 nil 表示该层不启用。两个密钥与 Header.Flags 必须自洽：本函数会校验，
// 因为「flag 说加密但没给密钥」这类错误在线上只表现为对端静默丢弃。
func BuildV3(h HeaderData, sec USMSecurityParameters, scoped []byte, authPriv *V3Keys) ([]byte, error) {
	if h.SecModel != SecurityModelUSM {
		return nil, fmt.Errorf("snmp: security model %d is not supported (only USM=3)", h.SecModel)
	}
	if h.MsgID == 0 {
		// msgID=0 是合法的，但把它当「未设置」更好：0 会让某些代理把
		// 请求与「引擎发现」混在一起。请求方应从 1 开始递增。
		return nil, errors.New("snmp: msgID must not be zero")
	}
	if h.MaxSize < 484 {
		return nil, fmt.Errorf("snmp: msgMaxSize %d is below the 484-byte minimum", h.MaxSize)
	}

	encrypted := h.Flags&FlagPriv != 0
	authenticated := h.Flags&FlagAuth != 0
	// 空 engineID 只允许出现在**引擎发现**报文里：那时我们还不知道对端的
	// engineID，也就无从派生本地化密钥，因此必然不带认证。允许它在别处
	// 出现，等于允许一条「用空 engineID 算摘要」的报文——那种报文的摘要
	// 与任何代理都对不上，现场表现却是「认证失败」，方向完全错了。
	if len(sec.EngineID) == 0 {
		if authenticated || encrypted {
			return nil, errors.New("snmp: empty engine ID is only valid for unauthenticated discovery messages")
		}
	} else if len(sec.EngineID) < 5 || len(sec.EngineID) > 32 {
		return nil, fmt.Errorf("snmp: engine ID must be 5..32 bytes, got %d", len(sec.EngineID))
	}
	if encrypted && !authenticated {
		return nil, errors.New("snmp: privFlag requires authFlag (USM forbids privacy without authentication)")
	}
	if authenticated && (authPriv == nil || len(authPriv.AuthKey) == 0) {
		return nil, errors.New("snmp: authFlag is set but no authentication key was provided")
	}
	if encrypted && (authPriv == nil || len(authPriv.PrivKey) == 0) {
		return nil, errors.New("snmp: privFlag is set but no privacy key was provided")
	}

	msgData := scoped
	if encrypted {
		var privParams []byte
		var err error
		msgData, privParams, err = encryptScoped(authPriv.Priv, authPriv.PrivKey,
			sec.EngineBoots, sec.EngineTime, authPriv.Salt(), scoped)
		if err != nil {
			return nil, err
		}
		sec.PrivParameters = privParams
		// msgData 的 CHOICE 里 encryptedPDU 是 **OCTET STRING**，所以密文
		// 必须套一层 TLV。少这一层，对端会把第一个密文字节当成 tag 去解析
		// （大多数情况下立刻报畸形；偶尔恰好是 0x04 时能读进去，但长度也
		// 是密文的一部分，表现为「随机地解不开」）。
		msgData = appendTLV(nil, tagOctetString, msgData)
	}

	// authParameters 在算摘要时必须先占位成「摘要长度个 0」：摘要是对
	// **含这个占位**的整条报文算的，算完再原地替换。少这一步（把 auth 字段
	// 留空、或长度写错）会让摘要的输入与对端重建的输入不一致。
	if authenticated {
		sec.AuthParameters = make([]byte, authPriv.Auth.DigestLen())
	} else {
		sec.AuthParameters = nil
	}

	secBytes, err := encodeUSMParams(sec)
	if err != nil {
		return nil, err
	}

	build := func() ([]byte, error) {
		body := make([]byte, 0, 64+len(msgData))
		body = appendTLV(body, tagInteger, encodeInt(int64(V3)))
		header, err := encodeHeaderData(h)
		if err != nil {
			return nil, err
		}
		body = append(body, header...)
		// 注意：OCTET STRING 里装的是 secBytes 这段 **BER 编码**。
		body = appendTLV(body, tagOctetString, secBytes)
		body = append(body, msgData...)
		return appendTLV(nil, tagSequence, body), nil
	}

	raw, err := build()
	if err != nil {
		return nil, err
	}
	if !authenticated {
		return raw, nil
	}

	// 摘要必须对「authParameters 为零」的报文计算，所以先构造一次、算摘要、
	// 再填回、再构造一次。两次构造的字节长度完全相同（digest 长度固定），
	// 因此第二次不需要重新分配长度字段。
	digest, err := authDigest(authPriv.Auth, authPriv.AuthKey, raw)
	if err != nil {
		return nil, err
	}
	sec.AuthParameters = digest
	if secBytes, err = encodeUSMParams(sec); err != nil {
		return nil, err
	}
	return build()
}

// V3Keys 是一次会话里用到的全部本地化密钥与参数。
type V3Keys struct {
	Auth    AuthProtocol
	AuthKey []byte
	Priv    PrivProtocol
	PrivKey []byte

	// salt 是 privParameters 的计数器。AES 用 8 字节、DES 用 4 字节；
	// 每次加密必须不同——同一 (key, IV) 用两次，CFB 模式下两份密文的
	// 异或等于两份明文的异或，等于把明文泄露出去。
	salt [8]byte
}

// Salt 返回本次加密用的 salt 并推进计数器。
func (k *V3Keys) Salt() []byte {
	// 取当前值，然后 +1。初始全 0 就够了：IV 里还混了 engineBoots 与
	// engineTime，三者一起保证唯一。
	out := make([]byte, k.Priv.saltLen())
	if k.Priv == PrivDES {
		copy(out, k.salt[4:])
	} else {
		copy(out, k.salt[:])
	}
	for i := len(k.salt) - 1; i >= 0; i-- {
		k.salt[i]++
		if k.salt[i] != 0 {
			break
		}
	}
	return out
}

func encodeHeaderData(h HeaderData) ([]byte, error) {
	if h.MaxSize < 0 || h.MaxSize > 0x7FFFFFFF {
		return nil, fmt.Errorf("snmp: msgMaxSize %d out of range", h.MaxSize)
	}
	body := make([]byte, 0, 24)
	body = appendTLV(body, tagInteger, encodeInt(int64(h.MsgID)))
	body = appendTLV(body, tagInteger, encodeInt(int64(h.MaxSize)))
	body = appendTLV(body, tagOctetString, []byte{byte(h.Flags)})
	body = appendTLV(body, tagInteger, encodeInt(int64(h.SecModel)))
	return appendTLV(nil, tagSequence, body), nil
}

func encodeUSMParams(p USMSecurityParameters) ([]byte, error) {
	if p.EngineBoots < 0 || p.EngineTime < 0 {
		return nil, fmt.Errorf("snmp: negative engineBoots/time (%d/%d)", p.EngineBoots, p.EngineTime)
	}
	body := make([]byte, 0, 64+len(p.EngineID))
	body = appendTLV(body, tagOctetString, p.EngineID)
	body = appendTLV(body, tagInteger, encodeInt(int64(p.EngineBoots)))
	body = appendTLV(body, tagInteger, encodeInt(int64(p.EngineTime)))
	body = appendTLV(body, tagOctetString, []byte(p.UserName))
	body = appendTLV(body, tagOctetString, p.AuthParameters)
	body = appendTLV(body, tagOctetString, p.PrivParameters)
	return appendTLV(nil, tagSequence, body), nil
}

func decodeUSMParams(b []byte) (USMSecurityParameters, error) {
	var p USMSecurityParameters
	r := &reader{buf: b}
	tag, content, err := r.tlv()
	if err != nil {
		return p, err
	}
	if tag != tagSequence {
		return p, fmt.Errorf("snmp: usm parameters tag 0x%02X, want SEQUENCE", tag)
	}
	if r.off != len(b) {
		return p, fmt.Errorf("snmp: %d trailing bytes after usm parameters", len(b)-r.off)
	}
	inner := &reader{buf: content}
	octets := func(what string) ([]byte, error) {
		t, c, err := inner.tlv()
		if err != nil {
			return nil, fmt.Errorf("snmp: usm %s: %w", what, err)
		}
		if t != tagOctetString {
			return nil, fmt.Errorf("snmp: usm %s tag 0x%02X, want OCTET STRING", what, t)
		}
		return c, nil
	}
	intField := func(what string) (int, error) {
		t, c, err := inner.tlv()
		if err != nil {
			return 0, fmt.Errorf("snmp: usm %s: %w", what, err)
		}
		if t != tagInteger {
			return 0, fmt.Errorf("snmp: usm %s tag 0x%02X, want INTEGER", what, t)
		}
		v, err := decodeInt(c)
		if err != nil {
			return 0, fmt.Errorf("snmp: usm %s: %w", what, err)
		}
		if v < 0 || v > 0x7FFFFFFF {
			return 0, fmt.Errorf("snmp: usm %s %d out of range", what, v)
		}
		return int(v), nil
	}

	if p.EngineID, err = octets("engineID"); err != nil {
		return p, err
	}
	if p.EngineBoots, err = intField("engineBoots"); err != nil {
		return p, err
	}
	if p.EngineTime, err = intField("engineTime"); err != nil {
		return p, err
	}
	name, err := octets("userName")
	if err != nil {
		return p, err
	}
	p.UserName = string(name)
	if p.AuthParameters, err = octets("authParameters"); err != nil {
		return p, err
	}
	if p.PrivParameters, err = octets("privParameters"); err != nil {
		return p, err
	}
	if inner.off != len(content) {
		return p, fmt.Errorf("snmp: %d trailing bytes in usm parameters", len(content)-inner.off)
	}
	return p, nil
}

// ParseV3 解析一条 v3 报文。加密的 payload 保持密文（需要密钥才能解）。
//
// 返回的错误只覆盖「结构」层面：认证失败与时间窗口错误由调用方在
// VerifyV3 里判定。这个分工很重要——结构错误是不可恢复的，认证错误则可能
// 只是密钥配错或时钟漂移，两者的处理方式完全不同。
func ParseV3(b []byte) (V3Message, error) {
	var m V3Message
	if len(b) == 0 {
		return m, errors.New("snmp: empty datagram")
	}
	if len(b) > MaxDatagram {
		return m, fmt.Errorf("snmp: datagram %d bytes exceeds %d", len(b), MaxDatagram)
	}
	r := &reader{buf: b}
	tag, content, err := r.tlv()
	if err != nil {
		return m, err
	}
	if tag != tagSequence {
		return m, fmt.Errorf("snmp: top-level tag 0x%02X, want SEQUENCE", tag)
	}
	if r.off != len(b) {
		return m, fmt.Errorf("snmp: %d trailing bytes after message", len(b)-r.off)
	}

	inner := &reader{buf: content}
	// 1. msgVersion
	tag, c, err := inner.tlv()
	if err != nil {
		return m, err
	}
	if tag != tagInteger {
		return m, fmt.Errorf("snmp: version tag 0x%02X, want INTEGER", tag)
	}
	ver, err := decodeInt(c)
	if err != nil {
		return m, fmt.Errorf("snmp: version: %w", err)
	}
	if Version(ver) != V3 {
		return m, fmt.Errorf("snmp: wire version %d is not v3 (use Parse for v1/v2c)", ver)
	}

	// 2. msgGlobalData
	tag, c, err = inner.tlv()
	if err != nil {
		return m, err
	}
	if tag != tagSequence {
		return m, fmt.Errorf("snmp: msgGlobalData tag 0x%02X, want SEQUENCE", tag)
	}
	if m.Header, err = decodeHeaderData(c); err != nil {
		return m, err
	}

	// 3. msgSecurityParameters：OCTET STRING，内容是 BER
	tag, c, err = inner.tlv()
	if err != nil {
		return m, err
	}
	if tag != tagOctetString {
		return m, fmt.Errorf("snmp: msgSecurityParameters tag 0x%02X, want OCTET STRING", tag)
	}
	if m.Security, err = decodeUSMParams(c); err != nil {
		return m, err
	}

	// 4. msgData（ScopedPDU 或密文）
	tag, c, err = inner.tlv()
	if err != nil {
		return m, err
	}
	if inner.off != len(content) {
		return m, fmt.Errorf("snmp: %d trailing bytes after msgData", len(content)-inner.off)
	}
	m.RawScoped = c
	m.Encrypted = m.Header.Flags&FlagPriv != 0
	if m.Encrypted {
		// 加密时 msgData 是 **OCTET STRING**（密文字节），所以上面那个 TLV
		// 已经把密文取出来了。明文时它是 SEQUENCE（ScopedPDU 自己）。
		if tag != tagOctetString {
			return m, fmt.Errorf("snmp: encrypted msgData tag 0x%02X, want OCTET STRING", tag)
		}
		m.ScopedPDU = c
	} else {
		if tag != tagSequence {
			return m, fmt.Errorf("snmp: plaintext msgData tag 0x%02X, want SEQUENCE", tag)
		}
		// 保留原始 TLV：VerifyV3 要按「线上原样」算摘要。
		m.ScopedPDU = appendTLV(nil, tag, c)
		// 明文报文顺手解析出 PDU。这一步不能省：Report（引擎发现、时间
		// 窗口）永远不加密，而调用方要按 PDU 类型与 request-id 配对；
		// 留到调用方解析会让「所有 Report 都被当成不匹配的报文丢掉」，
		// 症状正好是「发现阶段永远超时」。
		sc, err := ParseScopedPDU(m.ScopedPDU)
		if err != nil {
			return m, err
		}
		m.PDU = sc.PDU
		m.ContextEngineID = sc.ContextEngineID
		m.ContextName = sc.ContextName
	}
	return m, nil
}

func decodeHeaderData(b []byte) (HeaderData, error) {
	var h HeaderData
	r := &reader{buf: b}
	intField := func(what string) (int64, error) {
		t, c, err := r.tlv()
		if err != nil {
			return 0, fmt.Errorf("snmp: %s: %w", what, err)
		}
		if t != tagInteger {
			return 0, fmt.Errorf("snmp: %s tag 0x%02X, want INTEGER", what, t)
		}
		return decodeInt(c)
	}
	id, err := intField("msgID")
	if err != nil {
		return h, err
	}
	if id < -2147483648 || id > 2147483647 {
		return h, fmt.Errorf("snmp: msgID %d out of 32-bit range", id)
	}
	h.MsgID = int32(id)
	size, err := intField("msgMaxSize")
	if err != nil {
		return h, err
	}
	if size < 484 || size > 0x7FFFFFFF {
		return h, fmt.Errorf("snmp: msgMaxSize %d out of range (min 484)", size)
	}
	h.MaxSize = int(size)
	t, c, err := r.tlv()
	if err != nil {
		return h, err
	}
	if t != tagOctetString {
		return h, fmt.Errorf("snmp: msgFlags tag 0x%02X, want OCTET STRING", t)
	}
	if len(c) != 1 {
		return h, fmt.Errorf("snmp: msgFlags is %d bytes, want 1", len(c))
	}
	h.Flags = MsgFlags(c[0])
	if h.Flags&^(FlagAuth|FlagPriv|FlagReportable) != 0 {
		return h, fmt.Errorf("snmp: msgFlags 0x%02X has reserved bits set", c[0])
	}
	if h.Flags&FlagPriv != 0 && h.Flags&FlagAuth == 0 {
		return h, errors.New("snmp: privFlag is set without authFlag (USM forbids it)")
	}
	model, err := intField("msgSecurityModel")
	if err != nil {
		return h, err
	}
	if model < 1 || model > 0x7FFFFFFF {
		return h, fmt.Errorf("snmp: msgSecurityModel %d out of range", model)
	}
	h.SecModel = int(model)
	if r.off != len(b) {
		return h, fmt.Errorf("snmp: %d trailing bytes in msgGlobalData", len(b)-r.off)
	}
	return h, nil
}

// VerifyV3 校验报文的认证摘要。
//
// 实现方式是把报文中**authParameters 字段原地清零**再算 HMAC：这是唯一正确
// 的算法——摘要覆盖整条报文（含头、含 security parameters、含 msgData），
// 唯独不覆盖 auth 字段自身的值。
//
// 用 subtle.ConstantTimeCompare 而不是 bytes.Equal：比较结果若在第一个不同
// 字节就返回，攻击者可以通过计时差逐字节猜出正确摘要。这在「每次重连都验
// 一次」的场景里不现实，但不该由实现者替攻击者省事。
func VerifyV3(raw []byte, sec USMSecurityParameters, auth AuthProtocol, key []byte) error {
	if auth == AuthNone {
		return nil
	}
	if want := auth.DigestLen(); len(sec.AuthParameters) != want {
		return fmt.Errorf("snmp: auth parameters are %d bytes, %v needs %d", len(sec.AuthParameters), auth, want)
	}
	zeroed, err := zeroAuthField(raw, sec.AuthParameters)
	if err != nil {
		return err
	}
	sum, err := authDigest(auth, key, zeroed)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(sum, sec.AuthParameters) != 1 {
		return errors.New("snmp: authentication digest mismatch (wrong auth password or protocol)")
	}
	return nil
}

// zeroAuthField 把报文里的 authParameters 内容替换成等长的 0。
//
// 直接在原始字节里定位而不是重新编码：重新编码会把「解析器认为的」字节
// 序列算一遍摘要，一旦解析与编码对某个可选字段的处理不同（比如长度形式），
// 摘要就对不上，而这类差异极难从现象定位。
func zeroAuthField(raw []byte, authParams []byte) ([]byte, error) {
	if len(authParams) == 0 {
		return raw, nil
	}
	idx := indexOfBytes(raw, authParams)
	if idx < 0 {
		return nil, errors.New("snmp: auth parameters not found in the raw message")
	}
	out := append([]byte(nil), raw...)
	// 清零**最后一处**出现的位置更稳妥，但这里要求唯一：authParameters
	// 是随机摘要，正常不会与报文里别的字节串重复。若真出现多次，说明报文
	// 本身可疑，直接报错而不是猜。
	if again := indexOfBytes(raw[idx+1:], authParams); again >= 0 {
		return nil, errors.New("snmp: auth parameters occur more than once in the message")
	}
	for i := idx; i < idx+len(authParams); i++ {
		out[i] = 0
	}
	return out, nil
}

// indexOfBytes 返回子串首次出现的下标（-1 表示没有）。
func indexOfBytes(haystack, needle []byte) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	last := len(haystack) - len(needle)
	for i := 0; i <= last; i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// DecryptScopedBytes 用本地化密钥解密一段密文（供代理侧使用）。
//
// 与 V3Message.DecryptScoped 的差别：这里不涉及解析，只做解密。代理侧
// 要在「解析出 PDU」之前就判断解密是否成功，失败了要回 decryptionError
// 而不是丢弃。
func DecryptScopedBytes(priv PrivProtocol, key []byte, engineBoots, engineTime int, privParams, ciphertext []byte) ([]byte, error) {
	plain, err := decryptScoped(priv, key, engineBoots, engineTime, privParams, ciphertext)
	if err != nil {
		return nil, err
	}
	// DES 的 CBC 填充在这里一并剥掉：调用方拿到的应当是「可以直接解析的
	// ScopedPDU」，而不是「带 1..8 个填充字节的块」。
	if trimmed, ok := trimBERSequence(plain); ok {
		plain = trimmed
	}
	return plain, nil
}

// DecryptScoped 用本地化密钥解开 ScopedPDU，并解析出 PDU。
func (m *V3Message) DecryptScoped(priv PrivProtocol, key []byte) error {
	if !m.Encrypted {
		return m.ParseScopedPDU()
	}
	plain, err := decryptScoped(priv, key, m.Security.EngineBoots, m.Security.EngineTime,
		m.Security.PrivParameters, m.RawScoped)
	if err != nil {
		return err
	}
	// DES-CBC 必须补齐到 8 字节整数倍，明文尾部因此可能有 1..8 个填充字节
	// （值未规定，多数实现填 0，也有填随机数的）。AES-CFB 不补齐，所以这
	// 一步对它无害（外层 SEQUENCE 的长度正好等于整块明文）。
	//
	// 不能"看到多余字节就当真报错"：那会让 DES 永远解不开。也不能直接丢掉
	// 尾部——被截断的报文会伪装成合法报文。正确做法是按 BER 外层 SEQUENCE
	// 声明的长度取，长度之外的字节全部视为填充；如果 SEQUENCE 本身短于
	// 密文，多出的部分就是填充，反之说明报文被截断，交给 ParseScopedPDU
	// 报错。
	if trimmed, ok := trimBERSequence(plain); ok {
		plain = trimmed
	}
	m.ScopedPDU = plain
	m.Encrypted = false
	return m.ParseScopedPDU()
}

// trimBERSequence 返回 b 中第一个完整 BER TLV 的字节范围。
//
// 用途只有一个：剥掉 DES 的 CBC 填充。ok=false 表示 b 不是一个完整的 TLV
// （长度字段说还有更多字节），此时原样交给解析器去报错，不在这里吞掉。
func trimBERSequence(b []byte) ([]byte, bool) {
	r := &reader{buf: b}
	if _, _, err := r.tlv(); err != nil {
		return b, false
	}
	if r.off > len(b) {
		return b, false
	}
	return b[:r.off], true
}

// ParseScopedPDU 解析明文 ScopedPDU。
func (m *V3Message) ParseScopedPDU() error {
	sc, err := ParseScopedPDU(m.ScopedPDU)
	if err != nil {
		return err
	}
	m.PDU = sc.PDU
	return nil
}

// ParseScopedPDU 解析 ScopedPDU 的字节。
//
// ScopedPDU 里的 contextEngineID 通常与 msgAuthoritativeEngineID 相同，但
// 协议不要求：代理可以代理到另一个上下文（如虚拟路由器）。驱动按「回什么
// 就用什么」处理，不做相等断言。
func ParseScopedPDU(b []byte) (ScopedPDU, error) {
	var sc ScopedPDU
	r := &reader{buf: b}
	tag, content, err := r.tlv()
	if err != nil {
		return sc, err
	}
	if tag != tagSequence {
		return sc, fmt.Errorf("snmp: scoped PDU tag 0x%02X, want SEQUENCE", tag)
	}
	if r.off != len(b) {
		return sc, fmt.Errorf("snmp: %d trailing bytes after scoped PDU", len(b)-r.off)
	}
	inner := &reader{buf: content}
	for _, what := range []string{"contextEngineID", "contextName"} {
		t, c, err := inner.tlv()
		if err != nil {
			return sc, fmt.Errorf("snmp: %s: %w", what, err)
		}
		if t != tagOctetString {
			return sc, fmt.Errorf("snmp: %s tag 0x%02X, want OCTET STRING", what, t)
		}
		if what == "contextEngineID" {
			sc.ContextEngineID = c
		} else {
			sc.ContextName = string(c)
		}
	}
	t, c, err := inner.tlv()
	if err != nil {
		return sc, err
	}
	if inner.off != len(content) {
		return sc, fmt.Errorf("snmp: %d trailing bytes after scoped PDU fields", len(content)-inner.off)
	}
	if sc.PDU, err = parsePDU(PDUType(t), c); err != nil {
		return sc, err
	}
	return sc, nil
}

// BuildScopedPDU 组一个 ScopedPDU（明文）。
func BuildScopedPDU(contextEngineID []byte, contextName string, pdu PDU) ([]byte, error) {
	enc, err := encodePDU(pdu)
	if err != nil {
		return nil, err
	}
	body := make([]byte, 0, 32+len(enc))
	body = appendTLV(body, tagOctetString, contextEngineID)
	body = appendTLV(body, tagOctetString, []byte(contextName))
	body = append(body, enc...)
	return appendTLV(nil, tagSequence, body), nil
}

// USM 的统计计数器 OID（Report PDU 里用来告知「为什么拒绝」）。
//
// 这几个 OID 是排障的关键：代理不明说「你密钥错了」，只说
// usmStatsWrongDigests.0 = N。把 N 读出来，才知道该去改口令还是改算法。
var (
	OIDUsmStatsUnsupportedSecLevels = MustParseOID("1.3.6.1.6.3.15.1.1.1.0")
	OIDUsmStatsNotInTimeWindows     = MustParseOID("1.3.6.1.6.3.15.1.1.2.0")
	OIDUsmStatsUnknownUserNames     = MustParseOID("1.3.6.1.6.3.15.1.1.3.0")
	OIDUsmStatsUnknownEngineIDs     = MustParseOID("1.3.6.1.6.3.15.1.1.4.0")
	OIDUsmStatsWrongDigests         = MustParseOID("1.3.6.1.6.3.15.1.1.5.0")
	OIDUsmStatsDecryptionErrors     = MustParseOID("1.3.6.1.6.3.15.1.1.6.0")
)

// ReportKind 把 Report PDU 里的 varbind 归成可处理的原因。
//
// 为什么需要这个映射：USM 的失败诊断**只**通过 Report 传达，而 Report 的
// 内容是一个「计数器 OID + 当前值」。直接把计数器冒充成业务数据是错的，
// 直接把 Report 当「超时」也是错的——前者会污染点表，后者会让运维去查
// 网络，而真正的问题是「口令错了」。
type ReportKind int

const (
	ReportUnknown ReportKind = iota
	ReportUnknownEngineID
	ReportNotInTimeWindow
	ReportUnknownUserName
	ReportWrongDigest
	ReportDecryptionError
	ReportUnsupportedSecLevel
)

func (r ReportKind) String() string {
	switch r {
	case ReportUnknownEngineID:
		return "unknownEngineID"
	case ReportNotInTimeWindow:
		return "notInTimeWindow"
	case ReportUnknownUserName:
		return "unknownUserName"
	case ReportWrongDigest:
		return "wrongDigest"
	case ReportDecryptionError:
		return "decryptionError"
	case ReportUnsupportedSecLevel:
		return "unsupportedSecurityLevel"
	}
	return "unknown"
}

// Hint 给出针对性的排错提示。
func (r ReportKind) Hint() string {
	switch r {
	case ReportUnknownEngineID:
		return "代理不认识我们的 engine ID：通常是首次发现未完成或代理刚重启，重试即可"
	case ReportNotInTimeWindow:
		return "时间窗口不同步：代理的 engineBoots/engineTime 已变化（多为代理重启），重新发现即可"
	case ReportUnknownUserName:
		return "代理上没有这个 USM 用户：检查 labels.username 与代理侧的用户配置"
	case ReportWrongDigest:
		return "摘要不匹配：认证口令或认证算法与代理不一致"
	case ReportDecryptionError:
		return "解密失败：加密口令或加密算法与代理不一致"
	case ReportUnsupportedSecLevel:
		return "代理不接受这个安全级别：可能代理只允许 authNoPriv 而配置要求 authPriv"
	}
	return "未识别的 USM Report"
}

// ClassifyReport 从 Report PDU 的 varbind 里提取原因。
func ClassifyReport(p PDU) ReportKind {
	if p.Type != PDUReport {
		return ReportUnknown
	}
	for _, vb := range p.Varbinds {
		switch {
		case vb.OID.HasPrefix(OIDUsmStatsUnknownEngineIDs):
			return ReportUnknownEngineID
		case vb.OID.HasPrefix(OIDUsmStatsNotInTimeWindows):
			return ReportNotInTimeWindow
		case vb.OID.HasPrefix(OIDUsmStatsUnknownUserNames):
			return ReportUnknownUserName
		case vb.OID.HasPrefix(OIDUsmStatsWrongDigests):
			return ReportWrongDigest
		case vb.OID.HasPrefix(OIDUsmStatsDecryptionErrors):
			return ReportDecryptionError
		case vb.OID.HasPrefix(OIDUsmStatsUnsupportedSecLevels):
			return ReportUnsupportedSecLevel
		}
	}
	return ReportUnknown
}

// TimeWindow 判断对端时间是否在可接受窗口内。
//
// RFC 3414 §3.2 规定窗口是 150 秒，且 engineBoots 必须相等（否则视为代理
// 重启，必须重新发现）。这里用 150 秒而不是「大致差不多」：窗口放宽会让
// 重放攻击的可行窗口变大，收紧则会在网络抖动时误判。
const TimeWindow = 150

// CheckTimeWindow 校验 engineBoots/engineTime。
//
// 返回的 reason 为空表示通过；否则是可读的原因，用于日志与错误消息。
func CheckTimeWindow(boots, tm, localBoots, localTime int) (string, bool) {
	if boots != localBoots {
		return fmt.Sprintf("agent engineBoots %d != cached %d (agent likely restarted)", boots, localBoots), false
	}
	diff := tm - localTime
	if diff < 0 {
		diff = -diff
	}
	// 容忍本地计时与代理的轻微漂移：代理的 engineTime 只按秒递增，
	// 而我们的缓存值是从上一次应答里学的，两者最多差一个往返。
	if diff > TimeWindow {
		return fmt.Sprintf("agent engineTime %d differs from local estimate %d by more than %ds", tm, localTime, TimeWindow), false
	}
	return "", true
}

// nowUnix 便于测试替换。
var nowUnix = func() int64 { return time.Now().Unix() }
