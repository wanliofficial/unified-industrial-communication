// Package snmp 是 SNMP v1/v2c 的纯函数编解码层。
//
// SNMP 的报文只有一层薄结构：`SEQUENCE { version, community, PDU }`，PDU 里
// 是 `request-id + error-status + error-index + varbind 列表`，每个 varbind 是
// `(OID, 值)`。真正的复杂度在 BER 与「值的类型由设备决定」这两件事上。
//
// 三条容易踩的规则，各有注释与用例钉住：
//
//  1. **BER 的整数必须是最短补码。** 0 编码成 `02 01 00`；128 编码成
//     `02 02 00 80`（多一个 0x00 是为了不让最高位被当成符号位）；-1 是
//     `02 01 FF`。多写或少写一个字节，设备侧解析器直接丢掉整条报文。
//  2. **无符号量（Counter32/Gauge32/TimeTicks）也按 INTEGER 编码**，只是
//     语义上不解释符号位；最高位为 1 时必须补前导 0x00。
//  3. **长度是 definite 形式**，超过 127 字节用 `0x80|n` 加 n 字节大端长度。
//     收到 indefinite（0x80）一律拒绝：SNMP 不允许它，接受它等于允许
//     一个永远等不到结束标记的解析器。
//
// 这一层不做任何 I/O、不读时钟、不持有可变状态；request-id 由驱动分配。
package snmp

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// DefaultPort 是 SNMP 代理的标准端口。
	DefaultPort = 161
	// MaxDatagram 是一条 SNMP 报文的大小上限。
	//
	// 取 65535（UDP 载荷上限）而不是 1472：有的设备用「大 PDU + IP 分片」
	// 返回整张表，拒收会让 GetBulk 在个别型号上永远失败。我们自己发请求时
	// 另有分片预算（见 driver 的 maxRequestBytes）。
	MaxDatagram = 65535
	// MaxVarbinds 是一帧内允许的 varbind 上限。
	//
	// 这不是协议限制，而是防御：一个畸形长度字段可以声明「几万个 varbind」，
	// 让解析器在一条报文上耗掉整轮采集的时间。
	MaxVarbinds = 2048
)

// Version 是 SNMP 版本。v3 不做（见 ParseVersion 的注释）。
type Version int

const (
	// V1 是 SNMPv1：没有 per-varbind 异常，错误靠 error-status/error-index。
	V1 Version = 0
	// V2c 是 SNMPv2c：有 GetBulk 与 noSuchObject/noSuchInstance/endOfMibView。
	V2c Version = 1
)

func (v Version) String() string {
	switch v {
	case V1:
		return "1"
	case V2c:
		return "2c"
	}
	return fmt.Sprintf("version(%d)", int(v))
}

// ParseVersion 解析配置里的版本写法（`1` / `v1` / `2c` / `v2c`）。
//
// v3 在这里显式拒绝而不是「当 v2c 试一下」：v3 的报文结构（msgGlobalData、
// 认证/加密参数）与 v1/v2c 完全不同，退回 v2c 只会让现场看到「发了报文但
// 没有应答」，把配置问题伪装成网络问题。
func ParseVersion(s string) (Version, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "2c", "v2c", "2", "v2":
		return V2c, nil
	case "1", "v1":
		return V1, nil
	case "3", "v3":
		return 0, errors.New("snmp: v3 (USM 认证/加密) is not implemented; use 1 or 2c")
	}
	return 0, fmt.Errorf("snmp: unknown version %q (want 1 or 2c)", s)
}

// PDUType 是 PDU 的上下文标签。
type PDUType byte

const (
	PDUGet      PDUType = 0xA0
	PDUGetNext  PDUType = 0xA1
	PDUResponse PDUType = 0xA2
	PDUSet      PDUType = 0xA3
	// PDUTrapV1 是 SNMPv1 的 Trap，结构与其它 PDU 不同；本层只做类型识别，
	// 不做解析（驱动是轮询主站，不接收 Trap）。
	PDUTrapV1  PDUType = 0xA4
	PDUGetBulk PDUType = 0xA5
	PDUInform  PDUType = 0xA6
	PDUTrapV2  PDUType = 0xA7
	PDUReport  PDUType = 0xA8
)

func (p PDUType) String() string {
	switch p {
	case PDUGet:
		return "GetRequest"
	case PDUGetNext:
		return "GetNextRequest"
	case PDUResponse:
		return "Response"
	case PDUSet:
		return "SetRequest"
	case PDUTrapV1:
		return "Trap(v1)"
	case PDUGetBulk:
		return "GetBulkRequest"
	case PDUInform:
		return "InformRequest"
	case PDUTrapV2:
		return "Trap(v2)"
	case PDUReport:
		return "Report"
	}
	return fmt.Sprintf("pdu(0x%02X)", byte(p))
}

// IsRequest 表示这是一个请求（而不是应答/通知）。
func (p PDUType) IsRequest() bool {
	switch p {
	case PDUGet, PDUGetNext, PDUSet, PDUGetBulk:
		return true
	}
	return false
}

// Message 是一条完整的 SNMP 报文。
type Message struct {
	Version   Version
	Community string
	PDU       PDU
}

// PDU 是一条协议数据单元。
//
// GetBulk 复用 ErrorStatus/ErrorIndex 两个字段承载 non-repeaters /
// max-repetitions：两者的线格式相同（连续两个 INTEGER），分成两套字段只会
// 让编码器写两遍同样的代码。
type PDU struct {
	Type           PDUType
	RequestID      int32
	ErrorStatus    int
	ErrorIndex     int
	NonRepeaters   int
	MaxRepetitions int
	Varbinds       []Varbind
}

// Varbind 是一个 (OID, 值) 对。
type Varbind struct {
	OID   OID
	Value Value
}

// Build 组一条完整报文。
func Build(m Message) ([]byte, error) {
	if m.Version != V1 && m.Version != V2c {
		return nil, fmt.Errorf("snmp: unsupported version %v", m.Version)
	}
	if m.Community == "" {
		return nil, errors.New("snmp: community is empty")
	}
	if m.PDU.Type == PDUGetBulk && m.Version != V2c {
		return nil, errors.New("snmp: GetBulk requires v2c")
	}
	if len(m.PDU.Varbinds) > MaxVarbinds {
		return nil, fmt.Errorf("snmp: %d varbinds exceeds the %d limit", len(m.PDU.Varbinds), MaxVarbinds)
	}

	vbs := make([]byte, 0, 64*len(m.PDU.Varbinds))
	for i, vb := range m.PDU.Varbinds {
		// 异常标记只属于应答：请求里出现 noSuchObject 之类，说明调用方把
		// 「读回来的结果」当成「要写的东西」传了进来；编码出去会让设备
		// 侧看到一条无意义的值。
		if m.PDU.Type.IsRequest() && vb.Value.IsException() {
			return nil, fmt.Errorf("snmp: varbind %d: %s is not valid in a request", i, vb.Value.Type)
		}
		enc, err := encodeVarbind(vb)
		if err != nil {
			return nil, fmt.Errorf("snmp: varbind %d: %w", i, err)
		}
		vbs = append(vbs, enc...)
	}
	vbList := appendTLV(nil, tagSequence, vbs)

	body := make([]byte, 0, 16+len(vbList))
	body = appendTLV(body, tagInteger, encodeInt(int64(m.PDU.RequestID)))
	switch m.PDU.Type {
	case PDUGetBulk:
		body = appendTLV(body, tagInteger, encodeInt(int64(m.PDU.NonRepeaters)))
		body = appendTLV(body, tagInteger, encodeInt(int64(m.PDU.MaxRepetitions)))
	default:
		body = appendTLV(body, tagInteger, encodeInt(int64(m.PDU.ErrorStatus)))
		body = appendTLV(body, tagInteger, encodeInt(int64(m.PDU.ErrorIndex)))
	}
	body = append(body, vbList...)
	pdu := appendTLV(nil, byte(m.PDU.Type), body)

	msg := make([]byte, 0, 32+len(pdu))
	msg = appendTLV(msg, tagInteger, encodeInt(int64(m.Version)))
	msg = appendTLV(msg, tagOctetString, []byte(m.Community))
	msg = append(msg, pdu...)
	return appendTLV(nil, tagSequence, msg), nil
}

// Parse 解析一条完整报文。
//
// 严格拒绝「外层 SEQUENCE 之后还有字节」的报文：SNMP 是面向报文的传输，
// 多出来的字节只能是发错了或被人拼了包，猜着用会让 request-id 配错。
func Parse(b []byte) (Message, error) {
	if len(b) == 0 {
		return Message{}, errors.New("snmp: empty datagram")
	}
	if len(b) > MaxDatagram {
		return Message{}, fmt.Errorf("snmp: datagram %d bytes exceeds %d", len(b), MaxDatagram)
	}
	r := &reader{buf: b}
	tag, content, err := r.tlv()
	if err != nil {
		return Message{}, err
	}
	if tag != tagSequence {
		return Message{}, fmt.Errorf("snmp: top-level tag 0x%02X, want SEQUENCE", tag)
	}
	if r.off != len(b) {
		return Message{}, fmt.Errorf("snmp: %d trailing bytes after message", len(b)-r.off)
	}

	inner := &reader{buf: content}
	vtag, vcontent, err := inner.tlv()
	if err != nil {
		return Message{}, err
	}
	if vtag != tagInteger {
		return Message{}, fmt.Errorf("snmp: version tag 0x%02X, want INTEGER", vtag)
	}
	ver, err := decodeInt(vcontent)
	if err != nil {
		return Message{}, fmt.Errorf("snmp: version: %w", err)
	}
	if ver != int64(V1) && ver != int64(V2c) {
		return Message{}, fmt.Errorf("snmp: unsupported wire version %d", ver)
	}
	ctag, ccontent, err := inner.tlv()
	if err != nil {
		return Message{}, err
	}
	if ctag != tagOctetString {
		return Message{}, fmt.Errorf("snmp: community tag 0x%02X, want OCTET STRING", ctag)
	}
	// community 原样保留：它是字节串而不是 UTF-8 文本，做编码转换只会让
	// 少数用二进制 community 的设备对不上。
	community := string(ccontent)

	ptag, pcontent, err := inner.tlv()
	if err != nil {
		return Message{}, err
	}
	if inner.off != len(content) {
		return Message{}, fmt.Errorf("snmp: %d trailing bytes after PDU", len(content)-inner.off)
	}

	pdu, err := parsePDU(PDUType(ptag), pcontent)
	if err != nil {
		return Message{}, err
	}
	return Message{Version: Version(ver), Community: community, PDU: pdu}, nil
}

func parsePDU(typ PDUType, content []byte) (PDU, error) {
	switch typ {
	case PDUGet, PDUGetNext, PDUResponse, PDUSet, PDUGetBulk, PDUInform, PDUTrapV2, PDUReport:
	case PDUTrapV1:
		return PDU{}, errors.New("snmp: v1 Trap PDU is not supported (no trap receiver)")
	default:
		return PDU{}, fmt.Errorf("snmp: unknown PDU tag 0x%02X", byte(typ))
	}

	r := &reader{buf: content}
	p := PDU{Type: typ}
	readIntField := func(what string) (int, error) {
		tag, b, err := r.tlv()
		if err != nil {
			return 0, fmt.Errorf("snmp: %s: %w", what, err)
		}
		if tag != tagInteger {
			return 0, fmt.Errorf("snmp: %s tag 0x%02X, want INTEGER", what, tag)
		}
		v, err := decodeInt(b)
		if err != nil {
			return 0, fmt.Errorf("snmp: %s: %w", what, err)
		}
		if v < -2147483648 || v > 2147483647 {
			return 0, fmt.Errorf("snmp: %s %d out of 32-bit range", what, v)
		}
		return int(v), nil
	}

	id, err := readIntField("request-id")
	if err != nil {
		return PDU{}, err
	}
	p.RequestID = int32(id)
	if typ == PDUGetBulk {
		nr, err := readIntField("non-repeaters")
		if err != nil {
			return PDU{}, err
		}
		mr, err := readIntField("max-repetitions")
		if err != nil {
			return PDU{}, err
		}
		if nr < 0 || mr < 0 {
			return PDU{}, fmt.Errorf("snmp: negative non-repeaters/max-repetitions (%d/%d)", nr, mr)
		}
		p.NonRepeaters, p.MaxRepetitions = nr, mr
	} else {
		st, err := readIntField("error-status")
		if err != nil {
			return PDU{}, err
		}
		ix, err := readIntField("error-index")
		if err != nil {
			return PDU{}, err
		}
		if st < 0 || ix < 0 {
			return PDU{}, fmt.Errorf("snmp: negative error-status/index (%d/%d)", st, ix)
		}
		p.ErrorStatus, p.ErrorIndex = st, ix
	}

	tag, list, err := r.tlv()
	if err != nil {
		return PDU{}, err
	}
	if tag != tagSequence {
		return PDU{}, fmt.Errorf("snmp: varbind list tag 0x%02X, want SEQUENCE", tag)
	}
	if r.off != len(content) {
		return PDU{}, fmt.Errorf("snmp: %d trailing bytes after varbind list", len(content)-r.off)
	}
	lr := &reader{buf: list}
	for lr.off < len(list) {
		if len(p.Varbinds) >= MaxVarbinds {
			return PDU{}, fmt.Errorf("snmp: more than %d varbinds", MaxVarbinds)
		}
		tag, vb, err := lr.tlv()
		if err != nil {
			return PDU{}, err
		}
		if tag != tagSequence {
			return PDU{}, fmt.Errorf("snmp: varbind tag 0x%02X, want SEQUENCE", tag)
		}
		one, err := parseVarbind(vb)
		if err != nil {
			return PDU{}, err
		}
		p.Varbinds = append(p.Varbinds, one)
	}
	return p, nil
}

func parseVarbind(b []byte) (Varbind, error) {
	r := &reader{buf: b}
	tag, name, err := r.tlv()
	if err != nil {
		return Varbind{}, err
	}
	if tag != tagOID {
		return Varbind{}, fmt.Errorf("snmp: varbind name tag 0x%02X, want OID", tag)
	}
	oid, err := decodeOID(name)
	if err != nil {
		return Varbind{}, fmt.Errorf("snmp: varbind name: %w", err)
	}
	tag, raw, err := r.tlv()
	if err != nil {
		return Varbind{}, err
	}
	if r.off != len(b) {
		return Varbind{}, fmt.Errorf("snmp: %d trailing bytes in varbind", len(b)-r.off)
	}
	v, err := parseValue(tag, raw)
	if err != nil {
		return Varbind{}, fmt.Errorf("snmp: varbind %s: %w", oid, err)
	}
	return Varbind{OID: oid, Value: v}, nil
}

func encodeVarbind(vb Varbind) ([]byte, error) {
	if err := vb.OID.Validate(); err != nil {
		return nil, err
	}
	name := appendTLV(nil, tagOID, encodeOID(vb.OID))
	val, err := encodeValue(vb.Value)
	if err != nil {
		return nil, err
	}
	return appendTLV(nil, tagSequence, append(name, val...)), nil
}

// GetRequest 组一条 GetRequest。
func GetRequest(v Version, community string, requestID int32, oids []OID) ([]byte, error) {
	return requestWith(PDUGet, v, community, requestID, oids)
}

// GetNextRequest 组一条 GetNextRequest（走 MIB 树用）。
func GetNextRequest(v Version, community string, requestID int32, oids []OID) ([]byte, error) {
	return requestWith(PDUGetNext, v, community, requestID, oids)
}

func requestWith(t PDUType, v Version, community string, requestID int32, oids []OID) ([]byte, error) {
	if len(oids) == 0 {
		return nil, errors.New("snmp: at least one OID is required")
	}
	vbs := make([]Varbind, len(oids))
	for i, o := range oids {
		vbs[i] = Varbind{OID: o, Value: Null()}
	}
	return Build(Message{Version: v, Community: community, PDU: PDU{
		Type: t, RequestID: requestID, Varbinds: vbs,
	}})
}

// SetRequest 组一条 SetRequest。
func SetRequest(v Version, community string, requestID int32, vbs []Varbind) ([]byte, error) {
	if len(vbs) == 0 {
		return nil, errors.New("snmp: at least one varbind is required")
	}
	return Build(Message{Version: v, Community: community, PDU: PDU{
		Type: PDUSet, RequestID: requestID, Varbinds: vbs,
	}})
}

// GetBulkRequest 组一条 GetBulkRequest（仅 v2c）。
func GetBulkRequest(community string, requestID int32, oids []OID, nonRepeaters, maxRepetitions int) ([]byte, error) {
	if len(oids) == 0 {
		return nil, errors.New("snmp: at least one OID is required")
	}
	vbs := make([]Varbind, len(oids))
	for i, o := range oids {
		vbs[i] = Varbind{OID: o, Value: Null()}
	}
	return Build(Message{Version: V2c, Community: community, PDU: PDU{
		Type: PDUGetBulk, RequestID: requestID,
		NonRepeaters: nonRepeaters, MaxRepetitions: maxRepetitions, Varbinds: vbs,
	}})
}
