package dlms

import (
	"encoding/binary"
	"fmt"
)

// 本文件实现 ACSE 关联（AARQ/AARE）与 Get 服务的 APDU。这三类 PDU 是
// "无安全 + LN 引用 + HDLC 承载"这条路径上**全部**需要的应用层结构。
//
// # 两层长度，别搞混
//
// APDU 内部嵌套了三层不同的长度语义，混一层就会全盘错位：
//
//	外层 BER 长度   AARQ 的 `60 1D`、AARE 的 `61 29`——按 ASN.1 BER 的短形式
//	                （<128 字节用单字节长度）
//	user-information 里的 `BE 10` 是 [30] 标签 + 长度，里面再套 `04 0E`
//	                （OCTET STRING + 长度）才是 14 字节的 xDLMS-Initiate
//	Conformance    `5F 1F 04 00 <3 字节位串>`：标签 [APPLICATION 31] 占两字节，
//	                长度 4 里第一个字节是"末尾未使用位数"，后 3 字节才是 24 位
//
// 第三层最容易写错：很多人把 `5F 1F 03` 当成"标签 + 3 字节位串"，于是丢掉
// 了"未使用位数"那个 0x00，导致后面所有字段整体前移一字节——症状是
// AARE 解出来的 max-PDU 是个天文数字或 0。
//
// # 权威金向量（IEC 62056-53 Annex C.3 / C.8）
//
// AARQ（LN，无安全）：
//
//	60 1D A1 09 06 07 60 85 74 05 08 01 01 BE 10 04 0E
//	01 00 00 00 06 5F 1F 04 00 00 7E 1F 04 B0
//
// AARE（LN，无安全，接受）：
//
//	61 29 A1 09 06 07 60 85 74 05 08 01 01 A2 03 02 01 00
//	A3 05 A1 03 02 01 00 BE 10 04 0E 08 00 06 5F 1F 04 00
//	00 50 1F 01 F4 00 07
//
// 这两条**逐字节**来自标准正文的编码示例，因此本文件的编解码以它们为
// 回归基准：任何改动若让这两条不再往返，就是改错了。

// ASN.1 / BER 标签。
const (
	TagAARQ     byte = 0x60 // [APPLICATION 0] AARQ
	TagAARE     byte = 0x61 // [APPLICATION 1] AARE
	TagRLRQ     byte = 0x62
	TagRLRE     byte = 0x63
	TagInitReq  byte = 0x01 // [1] InitiateRequest（APDU CHOICE 里）
	TagInitResp byte = 0x08 // [8] InitiateResponse

	// application-context-name [1]，内层是 OBJECT IDENTIFIER。
	TagCtxName byte = 0xA1
	// called-AP-title [2] / called-AE-qualifier [3] / called-AP-invocation-id [4]…
	TagResult       byte = 0xA2 // AARE 的 result [2]
	TagResultDiag   byte = 0xA3 // AARE 的 result-source-diagnostic [3]
	TagUSerInfo     byte = 0xBE // user-information [30]
	TagConformance  byte = 0x5F // Conformance 标签的第一字节
	TagConformance2 byte = 0x1F // Conformance 标签的第二字节（[APPLICATION 31]）

	// xDLMS APDU 标签（IEC 62056-53）。与 AARQ/AARE 的 BER 带长度不同，
	// Get/Set 这些 xDLMS APDU 走 A-XDR：标签后**没有长度字节**，紧跟类型
	// 字节与正文。normal/next/with-list 之争发生在标签之后的类型字节上，
	// 不是独立的 APDU 标签——0xC1 是 Set-Request、0xC5 是 Set-Response，
	// 把它们当「Get 的变体」解析会把 Set 应答读成数据块。
	TagGetRequest  byte = 0xC0 // get-Request
	TagSetRequest  byte = 0xC1 // set-Request（声明不做）
	TagGetResponse byte = 0xC4 // get-Response
	TagSetResponse byte = 0xC5 // set-Response（声明不做）

	// Get 请求/应答共用的类型字节（APDU 标签之后的第一个字节）。
	GetKindNormal byte = 0x01 // normal
	GetKindNext   byte = 0x02 // 取后续数据块（声明不做，解析侧明确报错）
	GetKindList   byte = 0x03 // 带清单（声明不做）

	// Get-Data-Result。
	TagData             byte = 0x00 // [0] Data
	TagDataAccessResult byte = 0x01 // [1] Data-Access-Result
)

// 应用上下文 OID（LN 引用 + 无安全）。
//
// `60 85 74 05 08 01 01` 是 {joint-iso-ccitt(2) country(16) country-name(756)
// identified-organization(5) DLMS-UA(8) application-context(1) context_id(1)}；
// 末位 context_id=2 表示 SN 引用（本包不支持）。
var (
	// OIDLNNoSecurity 是 LN 引用 + 无安全的应用上下文。
	OIDLNNoSecurity = []byte{0x60, 0x85, 0x74, 0x05, 0x08, 0x01, 0x01}
	// OIDSNNoSecurity 是 SN 引用 + 无安全（仅用于识别与报错，不支持）。
	OIDSNNoSecurity = []byte{0x60, 0x85, 0x74, 0x05, 0x08, 0x01, 0x02}
)

// DataAccessResult 是 Get/SET/ACTION 的通用结果码（§8.6.1）。
type DataAccessResult int

const (
	DARSuccess                 DataAccessResult = 0
	DARHardwareFault           DataAccessResult = 1
	DARTemporaryFailure        DataAccessResult = 2
	DARReadWriteDenied         DataAccessResult = 3
	DARObjectUndefined         DataAccessResult = 4
	DARObjectClassInconsistent DataAccessResult = 9
	DARObjectUnavailable       DataAccessResult = 11
	DARTypeUnmatched           DataAccessResult = 12
	DARScopeOfAccessViolated   DataAccessResult = 13
	DARDataBlockUnavailable    DataAccessResult = 14
	DARLongGetAborted          DataAccessResult = 15
	DARNoLongGetInProgress     DataAccessResult = 16
	DARLongSetAborted          DataAccessResult = 17
	DARNoLongSetInProgress     DataAccessResult = 18
	DAROtherReason             DataAccessResult = 250
)

// String 输出中文说明。现场看到 `object-undefined` 与 `object-class-inconsistent`
// 的区别很重要：前者是 OBIS 写错，后者是类号（class id）写错。
func (r DataAccessResult) String() string {
	switch r {
	case DARSuccess:
		return "成功"
	case DARHardwareFault:
		return "硬件故障"
	case DARTemporaryFailure:
		return "暂时失败"
	case DARReadWriteDenied:
		return "拒绝读写"
	case DARObjectUndefined:
		return "对象未定义（OBIS 或类号不存在）"
	case DARObjectClassInconsistent:
		return "对象类不一致（类号与 OBIS 不匹配）"
	case DARObjectUnavailable:
		return "对象不可用"
	case DARTypeUnmatched:
		return "类型不匹配"
	case DARScopeOfAccessViolated:
		return "访问范围越界"
	case DARDataBlockUnavailable:
		return "数据块不可用"
	case DARLongGetAborted:
		return "长读被中止"
	case DARNoLongGetInProgress:
		return "没有进行中的长读"
	case DARLongSetAborted:
		return "长写被中止"
	case DARNoLongSetInProgress:
		return "没有进行中的长写"
	case DAROtherReason:
		return "其它原因"
	}
	return fmt.Sprintf("未知结果码 %d", int(r))
}

// OK 表示这个结果码算成功。
func (r DataAccessResult) OK() bool { return r == DARSuccess }

// Conformance 是 24 位一致性位块（§8.5）。
//
// 表示方式是**线上 3 字节的大端整数**（例如标准示例里的 `00 7E 1F` 就是
// 数值 0x007E1F）。这一点必须说清楚，因为 ASN.1 BIT STRING 的位号从最高位
// 数起：bit0 是第一个字节的最高位。因此「bit n」对应数值位 `23-n`。
//
// 直接把 bit n 写成 `1<<n` 是最常见的错误——那样 read(bit3) 会变成 0x08，
// 而线上它应该是 0x100000（第一字节的 bit4）。建链时声明错位不会立刻报错，
// 只会让表按另一套能力回话，症状是「某个属性永远读不到」。
type Conformance uint32

// 一致性位（§8.5）。数值 = 1<<(23-bit)。
const (
	ConfRead                Conformance = 1 << 20 // bit3
	ConfWrite               Conformance = 1 << 19 // bit4
	ConfUnconfirmedWrite    Conformance = 1 << 18 // bit5
	ConfAttribute0Set       Conformance = 1 << 15 // bit8
	ConfPriorityMgmt        Conformance = 1 << 14 // bit9
	ConfAttribute0Get       Conformance = 1 << 13 // bit10
	ConfBlockTransferGet    Conformance = 1 << 12 // bit11
	ConfBlockTransferSet    Conformance = 1 << 11 // bit12
	ConfBlockTransferAction Conformance = 1 << 10 // bit13
	ConfMultipleReferences  Conformance = 1 << 9  // bit14
	ConfInformationReport   Conformance = 1 << 8  // bit15
	ConfParameterizedAccess Conformance = 1 << 5  // bit18
	ConfGet                 Conformance = 1 << 4  // bit19
	ConfSet                 Conformance = 1 << 3  // bit20
	ConfSelectiveAccess     Conformance = 1 << 2  // bit21
	ConfEventNotification   Conformance = 1 << 1  // bit22
	ConfAction              Conformance = 1 << 0  // bit23
)

// Has 判断某一位是否置位。
func (c Conformance) Has(bit Conformance) bool { return c&bit != 0 }

// Bytes 输出 3 字节位串（大端，高位在前）。
func (c Conformance) Bytes() []byte {
	return []byte{byte(c >> 16), byte(c >> 8), byte(c)}
}

// String 输出已置位的服务名，便于 CLI 展示。
func (c Conformance) String() string {
	names := []struct {
		bit  Conformance
		name string
	}{
		{ConfRead, "read"}, {ConfWrite, "write"}, {ConfUnconfirmedWrite, "unconfirmed-write"},
		{ConfAttribute0Set, "attribute0-set"}, {ConfPriorityMgmt, "priority-mgmt"},
		{ConfAttribute0Get, "attribute0-get"}, {ConfBlockTransferGet, "block-transfer-get"},
		{ConfBlockTransferSet, "block-transfer-set"}, {ConfBlockTransferAction, "block-transfer-action"},
		{ConfMultipleReferences, "multiple-references"}, {ConfInformationReport, "information-report"},
		{ConfParameterizedAccess, "parameterized-access"}, {ConfGet, "get"}, {ConfSet, "set"},
		{ConfSelectiveAccess, "selective-access"}, {ConfEventNotification, "event-notification"},
		{ConfAction, "action"},
	}
	out := ""
	for _, n := range names {
		if c.Has(n.bit) {
			if out != "" {
				out += " "
			}
			out += n.name
		}
	}
	if out == "" {
		return "（空）"
	}
	return out
}

// InitiateRequest 是 xDLMS-Initiate.request 的参数（§8.4）。
type InitiateRequest struct {
	DLMSVersion   uint8
	Conformance   Conformance
	MaxReceivePDU uint16
}

// DefaultInitiateRequest 返回一个保守的建链参数。
//
// 只声明**本驱动真正会用到**的能力：GET 与 selective-access 之外一律不开。
// 多声明能力不会让表更配合，只会让表按"你支持 block transfer"来回一段
// 带数据块的应答，而驱动并不会拼块——那时症状是"值读出来是半截"。
func DefaultInitiateRequest() InitiateRequest {
	return InitiateRequest{
		DLMSVersion: 6,
		// 声明 GET 与 attribute0-get：前者用于普通读，后者用于读
		// object_list（attribute 0 是类的属性列表入口，只有声明了
		// attribute0-get，部分表才允许读它）。
		Conformance:   ConfGet | ConfAttribute0Get | ConfSelectiveAccess,
		MaxReceivePDU: 0x0200, // 512 字节，足够装最长的 AARE 与常规应答
	}
}

// Encode 输出 xDLMS-Initiate.request（A-XDR）。
//
// 布局（Annex C.2）：
//
//	01       InitiateRequest 标签
//	00       dedicated-key 未使用
//	00       response-allowed 用默认值 TRUE
//	00       proposed-quality-of-service 未使用
//	06       proposed-dlms-version-number
//	5F 1F 04 00 <3 字节>  proposed-conformance
//	<2 字节> client-max-receive-pdu-size
//
// 前三个 00 是"可选字段未使用"的标志位，不是填充。少写一个就整体错位。
func (r InitiateRequest) Encode() []byte {
	out := []byte{TagInitReq, 0x00, 0x00, 0x00, r.DLMSVersion}
	out = append(out, TagConformance, TagConformance2, 0x04, 0x00)
	out = append(out, r.Conformance.Bytes()...)
	var pdu [2]byte
	binary.BigEndian.PutUint16(pdu[:], r.MaxReceivePDU)
	return append(out, pdu[:]...)
}

// InitiateResponse 是 xDLMS-Initiate.response 的参数。
type InitiateResponse struct {
	DLMSVersion   uint8
	Conformance   Conformance
	MaxReceivePDU uint16
	VAAName       uint16
}

// parseInitiate 解 xDLMS-Initiate.request/response。
//
// 两者布局近似但不完全相同：请求多三个"可选字段未使用"标志位，响应只有
// 一个（negotiated-quality-of-service），且响应末尾多 2 字节 vaa-name。
// 用 isRequest 区分，避免写两份近乎相同的解析。
func parseInitiate(b []byte, isRequest bool) (InitiateResponse, int, error) {
	var out InitiateResponse
	pos := 0
	wantTag := byte(TagInitResp)
	if isRequest {
		wantTag = TagInitReq
	}
	if len(b) < 1 {
		return out, 0, ErrIncomplete
	}
	if b[0] != wantTag {
		return out, 0, fmt.Errorf("%w: initiate 标签 %02X，期望 %02X", ErrAPDU, b[0], wantTag)
	}
	pos++
	// 可选字段标志位：请求有 3 个（dedicated-key / response-allowed /
	// quality-of-service），响应只有 1 个（quality-of-service）。
	flags := 1
	if isRequest {
		flags = 3
	}
	if len(b) < pos+flags+1 {
		return out, 0, ErrIncomplete
	}
	pos += flags
	out.DLMSVersion = b[pos]
	pos++
	// Conformance：标签 5F 1F（或兼容的单字节 5F），长度 4，首字节是未使用位数。
	if pos >= len(b) {
		return out, 0, ErrIncomplete
	}
	if b[pos] != TagConformance {
		return out, 0, fmt.Errorf("%w: conformance 标签 %02X", ErrAPDU, b[pos])
	}
	pos++
	// 标准允许单字节 5F（见 Annex C.2 IMPORTANT），两字节 5F 1F 是标准形态。
	if pos < len(b) && b[pos] == TagConformance2 {
		pos++
	}
	if pos+1 >= len(b) {
		return out, 0, ErrIncomplete
	}
	n := int(b[pos])
	pos++
	if n < 4 || pos+n > len(b) {
		return out, 0, fmt.Errorf("%w: conformance 长度 %d 非法", ErrAPDU, n)
	}
	// 首字节是"末尾未使用位数"，跳过。
	out.Conformance = Conformance(b[pos+1])<<16 | Conformance(b[pos+2])<<8 | Conformance(b[pos+3])
	pos += n
	if pos+2 > len(b) {
		return out, 0, ErrIncomplete
	}
	out.MaxReceivePDU = binary.BigEndian.Uint16(b[pos:])
	pos += 2
	if !isRequest {
		if pos+2 > len(b) {
			return out, 0, ErrIncomplete
		}
		out.VAAName = binary.BigEndian.Uint16(b[pos:])
		pos += 2
	}
	return out, pos, nil
}

// AssociateRequest 是发起关联所需的参数。
type AssociateRequest struct {
	// ApplicationContext 默认 LN 无安全。
	ApplicationContext []byte
	// Initiate 是 xDLMS-Initiate 参数。
	Initiate InitiateRequest
}

// Encode 输出 AARQ APDU（不含 LLC 与 HDLC）。
//
// 标准金向量（Annex C.3）：
//
//	60 1D A1 09 06 07 60 85 74 05 08 01 01 BE 10 04 0E
//	01 00 00 00 06 5F 1F 04 00 00 7E 1F 04 B0
//
// 其中 60 1D = AARQ + 长度 29；A1 09 = [1] application-context-name + 长度 9；
// 06 07 = OID + 长度 7；BE 10 = [30] user-information + 长度 16；
// 04 0E = OCTET STRING + 长度 14（14 字节的 InitiateRequest）。
func (r AssociateRequest) Encode() ([]byte, error) {
	ctx := r.ApplicationContext
	if len(ctx) == 0 {
		ctx = OIDLNNoSecurity
	}
	init := r.Initiate.Encode()
	// user-information = [30] { OCTET STRING { initiate } }
	userInfo := []byte{TagUSerInfo, byte(2 + len(init)), 0x04, byte(len(init))}
	userInfo = append(userInfo, init...)
	// application-context-name = [1] { OID }
	ctxField := []byte{TagCtxName, byte(2 + len(ctx)), 0x06, byte(len(ctx))}
	ctxField = append(ctxField, ctx...)

	body := append(ctxField, userInfo...)
	if len(body) > 127 {
		return nil, fmt.Errorf("%w: AARQ 长度 %d 超过 BER 单字节上限", ErrAPDU, len(body))
	}
	out := append([]byte{TagAARQ, byte(len(body))}, body...)
	return out, nil
}

// AssociateResponse 是解析后的 AARE。
type AssociateResponse struct {
	// ApplicationOK 表示 result 字段为 accepted(0)。
	ApplicationOK bool
	// Result 是原始 result 值（0=accepted，1=rejected-permanent，2=rejected-transient）。
	Result int
	// Diagnostic 是 result-source-diagnostic 的值（ACSE 服务用户诊断）。
	Diagnostic int
	// Context 是服务端回的 application-context-name（可能与应用的不同）。
	Context []byte
	// Initiate 是服务端的协商结果。
	Initiate InitiateResponse
	// HasInitiate 表示 user-information 里确实带了 InitiateResponse。
	HasInitiate bool
}

// ParseAssociateResponse 解 AARE APDU。
//
// 字段顺序按 ASN.1 SEQUENCE 固定，但可选字段可能出现也可能不出现，因此
// 用标签驱动逐段前移，而不是按固定偏移读。result-source-diagnostic 可能是
// `A1 03 02 01 00`（acse-service-user 0）也可能直接是 ENUMERATED，两种都要认。
func ParseAssociateResponse(b []byte) (AssociateResponse, error) {
	var out AssociateResponse
	if len(b) < 2 {
		return out, ErrIncomplete
	}
	if b[0] != TagAARE {
		return out, fmt.Errorf("%w: 期望 AARE 标签 61，得到 %02X", ErrAPDU, b[0])
	}
	n := int(b[1])
	if len(b) < 2+n {
		return out, ErrIncomplete
	}
	body := b[2 : 2+n]
	for len(body) > 0 {
		tag := body[0]
		if len(body) < 2 {
			return out, ErrIncomplete
		}
		ln := int(body[1])
		if len(body) < 2+ln {
			return out, fmt.Errorf("%w: 字段 %02X 长度 %d 越界", ErrAPDU, tag, ln)
		}
		val := body[2 : 2+ln]
		switch tag {
		case TagCtxName:
			// A1 { OID }
			if len(val) >= 2 && val[0] == 0x06 {
				out.Context = append([]byte(nil), val[2:]...)
			}
		case TagResult:
			// A2 { 02 01 xx }（INTEGER）
			if len(val) >= 3 && val[0] == 0x02 {
				out.Result = int(val[2])
				out.ApplicationOK = out.Result == 0
			}
		case TagResultDiag:
			// A3 { A1 03 02 01 xx }（acse-service-user + INTEGER）或裸 ENUMERATED。
			if len(val) >= 5 && val[0] == 0xA1 {
				out.Diagnostic = int(val[4])
			} else if len(val) >= 3 && val[0] == 0x02 {
				out.Diagnostic = int(val[2])
			}
		case TagUSerInfo:
			// BE { 04 len <initiate> }
			if len(val) >= 2 && val[0] == 0x04 {
				init := val[2:]
				if resp, used, err := parseInitiate(init, false); err == nil {
					// 记录实际消耗的位置，避免把尾部填充当出错。
					_ = used
					out.Initiate = resp
					out.HasInitiate = true
				}
			}
		}
		body = body[2+ln:]
	}
	return out, nil
}

// AttributeDescriptor 是 COSEM 属性描述符（class-id + instance-id + attribute-id）。
type AttributeDescriptor struct {
	ClassID    uint16
	InstanceID [6]byte // OBIS 码
	Attribute  int8
}

// Encode 输出 `class(2) instance(6) attribute(1)`，共 9 字节。
func (a AttributeDescriptor) Encode() []byte {
	out := make([]byte, 9)
	binary.BigEndian.PutUint16(out, a.ClassID)
	copy(out[2:], a.InstanceID[:])
	out[8] = byte(a.Attribute)
	return out
}

// DecodeAttributeDescriptor 从 b 解一个属性描述符。
func DecodeAttributeDescriptor(b []byte) (AttributeDescriptor, int, error) {
	var a AttributeDescriptor
	if len(b) < 9 {
		return a, 0, ErrIncomplete
	}
	a.ClassID = binary.BigEndian.Uint16(b)
	copy(a.InstanceID[:], b[2:8])
	a.Attribute = int8(b[8])
	return a, 9, nil
}

// InvokeID 是 Invoke-Id-And-Priority（BIT STRING SIZE(8)）。
//
// bit0-3 = invoke_id，bit6 = service_class，bit7 = priority。
// HDLC 承载下 bit6 无关（服务类型由 HDLC 帧类型表达），因此常用 0x81
// （confirmed + normal priority + id 1）或 0xC1（含 high priority）。
//
// 本包固定用 0x81：一条 Get 一次往返，id 不需要滚动，priority 常态为
// normal。收到应答时**不校验** invoke_id——部分表会回 0xC1 而不是 0x81，
// 校验它只会让本来能通的链路报错。
const InvokeID = 0x81

// GetRequest 是一次 Get-Request-Normal。
type GetRequest struct {
	Attribute AttributeDescriptor
}

// Encode 输出 Get-Request-Normal APDU：
//
//	C0 01 81 00 03 01 00 01 08 00 FF 02
//	│  │  │  │  │  │  │  │  │  │  │  └ attribute-id = 2（value）
//	│  │  │  │  │  │  │  │  │  │  └─── OBIS 1.0.1.8.0.255 的末字节
//	│  │  │  │  │  │  │  │  │  └───── OBIS 1.0.1.8.0.255 的 8
//	│  │  │  │  │  │  │  │  └──────── OBIS 1.0.1.8.0.255 的 1
//	│  │  │  │  │  │  └────────────── OBIS 1.0.1.8.0.255 的 C
//	│  │  │  │  │  └───────────────── class-id 3（Register）低位
//	│  │  │  │  └──────────────────── class-id 3（Register）高位
//	│  │  └────────────────── invoke-id-and-priority
//	│  └────────────── 请求类型 = 1（normal；02=取后续块、03=带清单）
//	└─────────────── get-Request APDU 标签
//
// 与 AARQ/AARE 的 BER 编码不同，xDLMS APDU 走 A-XDR：标签后没有长度
// 字节（Gurux 等实机栈组帧亦如此，见 IEC 62056-53 的 Get-Request 定义）。
// 在这里写一个长度字节，真表会把长度值当成请求类型——0x0A 不在 1/2/3
// 里，请求被静默丢弃，症状是「链路正常、读表无应答」。
func (g GetRequest) Encode() []byte {
	desc := g.Attribute.Encode()
	out := []byte{TagGetRequest, GetKindNormal, InvokeID}
	return append(out, desc...)
}

// GetResponse 是解析后的 Get-Response。
type GetResponse struct {
	// Kind 是应答类型：1=normal、2=with-datablock、3=with-list。
	Kind int
	// Data 是 normal 分支的数据（Kind=1 且 Result 成功时有效）。
	Data Value
	// HasData 表示 Data 是否有效。
	HasData bool
	// Result 是 Data-Access-Result（失败时有效）。
	Result DataAccessResult
	// IsResult 表示应答是"数据访问结果"而不是"数据"。
	IsResult bool
	// Block 是 with-datablock 分支的数据块信息。
	Block DataBlock
	// WithList 是 with-list 分支的各元素。
	WithList []GetDataResult
}

// GetDataResult 是 Get-Data-Result（data 或 data-access-result 二选一）。
type GetDataResult struct {
	Data     Value
	HasData  bool
	Result   DataAccessResult
	IsResult bool
}

// DataBlock 是 DataBlock-G。
type DataBlock struct {
	LastBlock bool
	BlockNo   uint32
	// Raw 是 raw-data 分支的内容。
	Raw []byte
	// Result 是 data-access-result 分支的结果。
	Result  DataAccessResult
	IsError bool
}

// ParseGetResponse 解 Get-Response APDU。
//
// 三个分支都认：
//
//	[1] normal       序列 { invoke, Get-Data-Result }
//	[2] with-datablock 序列 { invoke, DataBlock-G }
//	[3] with-list   序列 { invoke, SEQUENCE OF Get-Data-Result }
//
// 之所以不把 datablock 当"不支持就报错"：表在应答超过 max-receive-PDU 时
// **必然**走数据块，而 we 无法阻止它——建链时我们声明了 512 字节，但表可能
// 更小。这时如果直接报"不支持"，现场只会看到超时。正确做法是解出块号与
// 内容，由驱动决定是拼块还是明确报告"需要缩小 PDU 或改用分块读"。
func ParseGetResponse(b []byte) (GetResponse, error) {
	var out GetResponse
	// 线上 = C4 <类型> <invoke> <正文>。0xC5 是 Set-Response、0xC6 是
	// DataNotification——把它们当 Get 变体解析会读出垃圾，必须明确报错。
	if len(b) < 3 {
		return out, ErrIncomplete
	}
	if b[0] != TagGetResponse {
		return out, fmt.Errorf("%w: 不是 Get-Response 标签 %02X", ErrAPDU, b[0])
	}
	switch b[1] {
	case GetKindNormal:
		out.Kind = 1
	case GetKindNext:
		out.Kind = 2
	case GetKindList:
		out.Kind = 3
	default:
		return out, fmt.Errorf("%w: Get-Response 类型 %02X", ErrAPDU, b[1])
	}
	// 跳过 invoke-id-and-priority。不校验取值（见 InvokeID 注释）。
	body := b[3:]
	switch out.Kind {
	case 1:
		dr, used, err := parseGetDataResult(body)
		if err != nil {
			return out, err
		}
		_ = used
		out.Data = dr.Data
		out.HasData = dr.HasData
		out.Result = dr.Result
		out.IsResult = dr.IsResult
	case 2:
		blk, _, err := parseDataBlock(body)
		if err != nil {
			return out, err
		}
		out.Block = blk
	case 3:
		if len(body) < 1 {
			return out, ErrIncomplete
		}
		n := int(body[0])
		pos := 1
		for i := 0; i < n; i++ {
			dr, used, err := parseGetDataResult(body[pos:])
			if err != nil {
				return out, err
			}
			out.WithList = append(out.WithList, dr)
			pos += used
		}
	}
	return out, nil
}

// parseGetDataResult 解 Get-Data-Result。
//
// 标签 0x00 是 Data（后面跟一个完整 A-XDR 值），0x01 是 Data-Access-Result
// （后面跟一个 ENUMERATED 字节）。这两个标签恰好与 A-XDR 的 null-data 相同，
// 因此不能先尝试 A-XDR 解码——那会把"成功"读成 null。
func parseGetDataResult(b []byte) (GetDataResult, int, error) {
	var out GetDataResult
	if len(b) < 1 {
		return out, 0, ErrIncomplete
	}
	switch b[0] {
	case TagData:
		v, used, err := Decode(b[1:])
		if err != nil {
			return out, 0, err
		}
		out.Data = v
		out.HasData = true
		return out, 1 + used, nil
	case TagDataAccessResult:
		if len(b) < 2 {
			return out, 0, ErrIncomplete
		}
		// CHOICE [1] IMPLICIT Data-Access-Result：A-XDR 下 ENUMERATED 是
		// 定长 8 位，**没有长度前缀**，所以线上就是 `01 <值>` 两个字节。
		//
		// 千万不要"兼容" `01 01 xx`（带长度）那种写法：当结果码恰好是
		// hardware-fault(1) 时，标准形态 `01 01` 与带长度形态的前两字节
		// 完全相同，兼容会把它读成"还差一个字节"，于是所有 hardware-fault
		// 都被报成超时/不完整。宁可只认标准形态，让非标实现明确报错。
		out.Result = DataAccessResult(b[1])
		out.IsResult = true
		return out, 2, nil
	}
	return out, 0, fmt.Errorf("%w: Get-Data-Result 标签 %02X", ErrAPDU, b[0])
}

// parseDataBlock 解 DataBlock-G：last-block(1) block-number(4) result(CHOICE)。
func parseDataBlock(b []byte) (DataBlock, int, error) {
	var out DataBlock
	if len(b) < 6 {
		return out, 0, ErrIncomplete
	}
	out.LastBlock = b[0] != 0
	out.BlockNo = binary.BigEndian.Uint32(b[1:5])
	switch b[5] {
	case 0x00: // raw-data [0] IMPLICIT OCTET STRING
		if len(b) < 7 {
			return out, 0, ErrIncomplete
		}
		n := int(b[6])
		if len(b) < 7+n {
			return out, 0, ErrIncomplete
		}
		out.Raw = append([]byte(nil), b[7:7+n]...)
		return out, 7 + n, nil
	case 0x01: // data-access-result [1] IMPLICIT
		if len(b) < 7 {
			return out, 0, ErrIncomplete
		}
		out.Result = DataAccessResult(b[6])
		out.IsError = true
		return out, 7, nil
	}
	return out, 0, fmt.Errorf("%w: DataBlock result 标签 %02X", ErrAPDU, b[5])
}

// EncodeGetResponseNormal 组一个 Get-Response-Normal。
//
// 线上 = `C4 01 <invoke> 00 <A-XDR Data>`：结果字节 0 表示 data 分支，
// 其后是不带任何额外标签的 A-XDR 值。invoke 原样回显请求里的字节。
//
// 本包用它实现内置从站；主站侧只需要解析，不需要组应答。
func EncodeGetResponseNormal(invoke byte, data Value) ([]byte, error) {
	enc, err := data.Encode()
	if err != nil {
		return nil, err
	}
	out := []byte{TagGetResponse, GetKindNormal, invoke, TagData}
	return append(out, enc...), nil
}

// EncodeGetResponseResult 组一个带失败结果码的 Get-Response-Normal。
//
// 线上形态是 `C4 01 <invoke> 01 <结果码>`：Get-Data-Result 走
// `data-access-result [1] IMPLICIT` 分支，A-XDR 下 ENUMERATED 定长 8 位、
// 无长度前缀，因此分支标签后紧跟一个字节的结果码。
func EncodeGetResponseResult(invoke byte, r DataAccessResult) []byte {
	return []byte{TagGetResponse, GetKindNormal, invoke, TagDataAccessResult, byte(r)}
}
