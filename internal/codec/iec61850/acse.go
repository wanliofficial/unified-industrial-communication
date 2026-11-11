package iec61850

import (
	"errors"
	"fmt"
)

// 本文件实现 ISO 8650（ACSE）里 MMS 用到的关联控制：AARQ（关联请求）、
// AARE（关联响应）、RLRQ/RLRE（有序释放）与 ABRT（异常中止）。
//
// ACSE 在 MMS 里只承担三件事：说明「我想用 MMS」（application-context-name）、
// 说明「我是谁」（calling AP-title / AE-qualifier）以及携带 MMS 的 Initiate。
// 其余字段都是可选的，本实现只发必要的那些——多发的字段在真机上不会被
// 拒绝，但会让报文难读，也让「为什么某个装置不接受」更难判。

// ACSE 的固定字节。
var (
	// appContextMMS 是 MMS 的应用上下文 OID 1.0.9506.2.3。
	appContextMMS = []byte{0x28, 0xca, 0x22, 0x02, 0x03}
	// authMechPassword 是口令认证机制 OID 2.3.1。
	authMechPassword = []byte{0x52, 0x03, 0x01}
)

// AARQ / AARE 的字段标签（上下文类编号）。
const (
	acseTagCalledAPTitle      = 2
	acseTagCalledAEQualifier  = 3
	acseTagCallingAPTitle     = 6
	acseTagCallingAEQualifier = 7
	acseTagUserInformation    = 30
)

// AARQ 的应用上下文标签是 [1] EXPLICIT。
const acseTagApplicationContext = 1

// ACSE 结果码。
const (
	// ACSEResultAccepted 表示关联被接受。
	ACSEResultAccepted = 0
	// ACSEResultRejectedPermanent 表示永久拒绝（配置/权限问题）。
	ACSEResultRejectedPermanent = 1
	// ACSEResultRejectedTransient 表示临时拒绝（资源不足，可重试）。
	ACSEResultRejectedTransient = 2
)

// acseIndirectRef 是 AARQ 里 user-information 的间接引用编号。
//
// 这个值必须与表示上下文编号一致（MMS = 3），否则对端会用上下文 1 的语法
// 去解析 Initiate。它看起来是个无关紧要的整数，但写错就会得到「AARE 结果
// 是接受，可 Initiate 解不开」这种最难查的现象。
const acseIndirectRef = MMSContextID

// 认证字段的 sender-requirements-authentication。
var senderRequirementsAuth = []byte{0x8A, 0x02, 0x04, 0x80}

// ACSEAuth 是可选的口令认证。
//
// IEC 61850 的关联认证在标准里是可选的，现场绝大多数装置不开。做成可选而
// 不是默认发送：没开认证的装置收到认证字段会直接拒绝关联。
type ACSEAuth struct {
	// Password 是口令明文。空表示不发送认证字段。
	Password string
}

// BuildAARQ 生成 AARQ PDU。
//
// payload 是 MMS 的 Initiate 请求。字段顺序与 ASN.1 定义一致：应用上下文、
// 各种标题、认证、用户信息。
func BuildAARQ(payload []byte, auth *ACSEAuth) ([]byte, error) {
	var body []byte
	// application-context-name [1] EXPLICIT { OID }
	body = appendTLV(body, contextTag(acseTagApplicationContext, true), appendTLV(nil, tagOID, appContextMMS))

	if auth != nil && auth.Password != "" {
		// 认证值是一个 SEQUENCE，里面是 [1] 机制 OID 与 [2] 认证值。
		var inner []byte
		inner = appendTLV(inner, 0xA1, appendTLV(nil, tagOID, authMechPassword))
		inner = appendTLV(inner, 0xA2, appendTLV(nil, tagVisibleChars, []byte(auth.Password)))
		body = appendTLV(body, contextTag(12, true), appendTLV(nil, tagSequence, inner))
		body = append(body, senderRequirementsAuth...)
	}

	body = append(body, buildUserInformation(payload)...)
	return appendTLV(nil, 0x60, body), nil
}

// buildUserInformation 写 [30] EXPLICIT 的用户信息。
//
// 结构：be { 28 { 02 01 <indirect-ref>, a0 { payload } } }
func buildUserInformation(payload []byte) []byte {
	assoc := appendTLV(nil, tagInt, []byte{acseIndirectRef})
	assoc = appendTLV(assoc, contextTag(0, true), payload)
	return appendTLV(nil, contextTag(acseTagUserInformation, true), appendTLV(nil, tagSequence, assoc))
}

// AARE 是解析出来的关联响应。
type AARE struct {
	// Result 是结果码：0 接受、1 永久拒绝、2 临时拒绝。
	Result int
	// Diagnostic 是 result-source-diagnostic 里的 acse-service-user 码。
	Diagnostic int
	// UserData 是 AARE 里携带的 MMS 数据（通常是 Initiate 响应）。
	UserData []byte
	// HasContext 表示对端回了 application-context-name。
	HasContext bool
}

// ParseACSE 解析一个 ACSE 报文。
//
// 返回的 kind："aarq" / "aare" / "rlrq" / "rlre" / "abrt"。
func ParseACSE(b []byte) (kind string, aare AARE, err error) {
	e, err := parseElement(b)
	if err != nil {
		return "", aare, err
	}
	switch e.tag {
	case 0x60:
		return "aarq", AARE{}, nil
	case 0x61:
		aare, err = parseAARE(e.data)
		return "aare", aare, err
	case 0x62:
		return "rlrq", AARE{}, nil
	case 0x63:
		return "rlre", AARE{}, nil
	case 0x64:
		return "abrt", AARE{}, nil
	}
	return "", aare, fmt.Errorf("iec61850: unknown acse tag 0x%02X", e.tag)
}

// parseAARE 解析 AARE 的内容。
func parseAARE(body []byte) (AARE, error) {
	out := AARE{Result: -1}
	for len(body) > 0 {
		e, err := parseElement(body)
		if err != nil {
			return out, err
		}
		body = e.rest
		num, _, ok := isContext(e.tag)
		if !ok {
			continue
		}
		switch num {
		case acseTagApplicationContext:
			out.HasContext = true
		case 2: // result [2] EXPLICIT INTEGER
			out.Result = intFromExplicit(e.data)
		case 3: // result-source-diagnostic [3] EXPLICIT SEQUENCE
			out.Diagnostic = parseDiagnostic(e.data)
		case acseTagUserInformation:
			out.UserData = parseUserInformation(e.data)
		}
	}
	return out, nil
}

// intFromExplicit 从 EXPLICIT INTEGER 里取值。
func intFromExplicit(b []byte) int {
	e, err := parseElement(b)
	if err != nil || e.tag != tagInt {
		return -1
	}
	v, err := decodeInt(e.data)
	if err != nil {
		return -1
	}
	return int(v)
}

// parseDiagnostic 从 result-source-diagnostic 里取 acse-service-user 码。
func parseDiagnostic(b []byte) int {
	e, err := parseElement(b)
	if err != nil || e.tag != tagSequence {
		return 0
	}
	inner := e.data
	for len(inner) > 0 {
		f, err := parseElement(inner)
		if err != nil {
			return 0
		}
		inner = f.rest
		if num, _, ok := isContext(f.tag); ok && num == 1 {
			return intFromExplicit(f.data)
		}
	}
	return 0
}

// parseUserInformation 从 [30] 里取出用户数据。
func parseUserInformation(b []byte) []byte {
	e, err := parseElement(b)
	if err != nil || e.tag != tagSequence {
		return nil
	}
	inner := e.data
	for len(inner) > 0 {
		f, err := parseElement(inner)
		if err != nil {
			return nil
		}
		inner = f.rest
		if f.tag == contextTag(0, true) {
			return f.data
		}
	}
	return nil
}

// BuildAARE 生成 AARE PDU（内置从站用）。
func BuildAARE(result int, diagnostic int, payload []byte) []byte {
	var body []byte
	body = appendTLV(body, contextTag(acseTagApplicationContext, true), appendTLV(nil, tagOID, appContextMMS))
	body = appendTLV(body, contextTag(2, true), appendTLV(nil, tagInt, []byte{byte(result)}))
	// result-source-diagnostic [3] EXPLICIT { [1] EXPLICIT INTEGER }
	diag := appendTLV(nil, contextTag(1, true), appendTLV(nil, tagInt, []byte{byte(diagnostic)}))
	body = appendTLV(body, contextTag(3, true), appendTLV(nil, tagSequence, diag))
	if len(payload) > 0 {
		body = append(body, buildUserInformation(payload)...)
	}
	return appendTLV(nil, 0x61, body)
}

// BuildRLRQ 返回有序释放请求，固定 5 字节。
func BuildRLRQ() []byte {
	return []byte{0x62, 0x03, 0x80, 0x01, 0x00}
}

// BuildRLRE 返回有序释放响应，固定 2 字节。
func BuildRLRE() []byte {
	return []byte{0x63, 0x00}
}

// BuildABRT 返回异常中止，固定 5 字节。
//
// provider 为 true 表示「提供方中止」（对端协议错误），false 表示「用户中止」。
func BuildABRT(provider bool) []byte {
	src := byte(0)
	if provider {
		src = 1
	}
	return []byte{0x64, 0x03, 0x80, 0x01, src}
}

// ACSEReason 把 AARE 的结果与诊断码翻成一句话。
func ACSEReason(result, diagnostic int) string {
	var r string
	switch result {
	case ACSEResultAccepted:
		return ""
	case ACSEResultRejectedPermanent:
		r = "permanent rejection"
	case ACSEResultRejectedTransient:
		r = "transient rejection"
	default:
		r = fmt.Sprintf("unknown result %d", result)
	}
	switch diagnostic {
	case 1:
		r += ": no reason given"
	case 2:
		r += ": application context not supported"
	case 3:
		r += ": calling AP title not recognized"
	case 4:
		r += ": calling AE qualifier not recognized"
	case 5:
		r += ": calling AP invocation identifier not recognized"
	case 6:
		r += ": calling AE invocation identifier not recognized"
	case 7:
		r += ": called AP title not recognized"
	case 8:
		r += ": called AE qualifier not recognized"
	case 9:
		r += ": called AP invocation identifier not recognized"
	case 10:
		r += ": called AE invocation identifier not recognized"
	case 11:
		r += ": authentication mechanism name not recognized"
	case 12:
		r += ": authentication mechanism name required"
	case 13:
		r += ": authentication failure"
	case 14:
		r += ": authentication required"
	}
	return r
}

// errACSEAborted 表示对端中止了关联。
var errACSEAborted = errors.New("iec61850: association aborted by peer")
