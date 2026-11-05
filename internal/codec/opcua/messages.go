package opcua

import (
	"fmt"
	"time"
)

// 连接层（UACP）消息类型与分块类型。见 Part 6 §7.1.2。
const (
	MsgHello   = "HEL"
	MsgAck     = "ACK"
	MsgError   = "ERR"
	MsgOpen    = "OPN"
	MsgMessage = "MSG"
	MsgClose   = "CLO"
)

const (
	ChunkFinal        = 'F'
	ChunkIntermediate = 'C'
	ChunkAbort        = 'A'
)

// 命名空间 0 中常用的节点与引用类型。只收录本实现真正用到的。
const (
	NodeRootFolder    uint32 = 84
	NodeObjectsFolder uint32 = 85
	NodeTypesFolder   uint32 = 86
	NodeViewsFolder   uint32 = 87
	NodeServer        uint32 = 2253
	NodeServerStatus  uint32 = 2256
	NodeServerNSArray uint32 = 2255

	RefReferences             uint32 = 31
	RefHierarchical           uint32 = 33
	RefHierarchicalReferences uint32 = 33
	RefHasChild               uint32 = 34
	RefOrganizes              uint32 = 35
	RefHasComponent           uint32 = 47
	RefHasProperty            uint32 = 46
	RefHasTypeDef             uint32 = 40
)

// 服务标识使用「二进制编码」的 NodeId（Part 6 附录 A 的
// xxx_Encoding_DefaultBinary），而不是抽象的 xxx 标识：ExtensionObject
// 的 TypeId 按约定必须指向编码节点。
const (
	SvcServiceFaultResponse uint32 = 397

	SvcGetEndpointsRequest  uint32 = 428
	SvcGetEndpointsResponse uint32 = 431

	SvcOpenSecureChannelRequest  uint32 = 446
	SvcOpenSecureChannelResponse uint32 = 449

	SvcCloseSecureChannelRequest  uint32 = 452
	SvcCloseSecureChannelResponse uint32 = 455

	SvcCreateSessionRequest  uint32 = 461
	SvcCreateSessionResponse uint32 = 464

	SvcActivateSessionRequest  uint32 = 467
	SvcActivateSessionResponse uint32 = 470

	SvcCloseSessionRequest  uint32 = 473
	SvcCloseSessionResponse uint32 = 476

	SvcBrowseRequest  uint32 = 527
	SvcBrowseResponse uint32 = 530

	SvcReadRequest  uint32 = 631
	SvcReadResponse uint32 = 634

	SvcWriteRequest  uint32 = 673
	SvcWriteResponse uint32 = 676
)

const (
	SecurityModeNone uint32 = 1

	TokenRequestIssue uint32 = 0
	TokenRequestRenew uint32 = 1

	TimestampsSource  uint32 = 0
	TimestampsServer  uint32 = 1
	TimestampsBoth    uint32 = 2
	TimestampsNeither uint32 = 3

	AttrNodeID      uint32 = 1
	AttrNodeClass   uint32 = 2
	AttrBrowseName  uint32 = 3
	AttrDisplayName uint32 = 4
	AttrDescription uint32 = 5
	AttrValue       uint32 = 13
	AttrDataType    uint32 = 14

	NodeClassObject       uint32 = 1
	NodeClassVariable     uint32 = 2
	NodeClassMethod       uint32 = 4
	NodeClassObjectType   uint32 = 8
	NodeClassVariableType uint32 = 16
	NodeClassDataType     uint32 = 64
	NodeClassView         uint32 = 128

	BrowseForward uint32 = 0
	BrowseInverse uint32 = 1
	BrowseBoth    uint32 = 2

	// 浏览方向的可读别名：字段名 Direction 与常量名 BrowseForward 之间
	// 差一个词，现场写代码时最容易写错。
	BrowseDirectionForward uint32 = BrowseForward
	BrowseDirectionInverse uint32 = BrowseInverse
	BrowseDirectionBoth    uint32 = BrowseBoth

	// BrowseResultMask 的位组合。全选是最常见的用法（通用客户端都这么发）。
	BrowseResultMaskAll uint32 = 0x3F

	TokenTypeAnonymous uint32 = 0
	TokenTypeUserName  uint32 = 1
	TokenTypeCert      uint32 = 2
	TokenTypeIssued    uint32 = 3

	AppTypeServer uint32 = 0
	AppTypeClient uint32 = 1
)

// PolicyNone 是不加密、不签名的安全策略 URI。
const PolicyNone = "http://opcfoundation.org/UA/SecurityPolicy#None"

// TransportBinary 是 UA-TCP 传输协议 URI。
const TransportBinary = "http://opcfoundation.org/UA-Profile/Transport/uatcp-uasc-uabinary"

// ---------------------------------------------------------------------------
// 连接层报文
// ---------------------------------------------------------------------------

// ConnHeader 是 8 字节连接层报文头（HEL/ACK/ERR）。
type ConnHeader struct {
	Type  string
	Chunk byte
	Size  uint32
}

func DecodeConnHeader(b []byte) (ConnHeader, error) {
	if len(b) < 8 {
		return ConnHeader{}, ErrShort
	}
	h := ConnHeader{Type: string(b[:3]), Chunk: b[3], Size: u32(b[4:8])}
	if h.Size < 8 || h.Size > 64<<20 {
		return ConnHeader{}, fmt.Errorf("opcua: invalid message size %d", h.Size)
	}
	return h, nil
}

func (h ConnHeader) Encode() []byte {
	return h.appendTo(nil)
}

func (h ConnHeader) appendTo(dst []byte) []byte {
	dst = append(dst, h.Type...)
	dst = append(dst, h.Chunk)
	return appendU32(dst, h.Size)
}

// SecureHeader 是 OPN/MSG/CLO 的 12 字节头。
type SecureHeader struct {
	Type      string
	Chunk     byte
	Size      uint32
	ChannelID uint32
}

func DecodeSecureHeader(b []byte) (SecureHeader, error) {
	if len(b) < 12 {
		return SecureHeader{}, ErrShort
	}
	h := SecureHeader{
		Type:      string(b[:3]),
		Chunk:     b[3],
		Size:      u32(b[4:8]),
		ChannelID: u32(b[8:12]),
	}
	if h.Size < 12 || h.Size > 64<<20 {
		return SecureHeader{}, fmt.Errorf("opcua: invalid secure message size %d", h.Size)
	}
	return h, nil
}

// Hello 是连接层问候。
type Hello struct {
	Version        uint32
	ReceiveBufSize uint32
	SendBufSize    uint32
	MaxMessageSize uint32
	MaxChunkCount  uint32
	EndpointURL    string
}

// EncodeHello 生成完整 HEL 报文（含 8 字节头）。
func EncodeHello(h Hello) []byte {
	e := &encoder{}
	e.U32(h.Version)
	e.U32(h.ReceiveBufSize)
	e.U32(h.SendBufSize)
	e.U32(h.MaxMessageSize)
	e.U32(h.MaxChunkCount)
	e.String(h.EndpointURL)
	body := e.Bytes()
	dst := make([]byte, 0, 8+len(body))
	dst = ConnHeader{Type: MsgHello, Chunk: ChunkFinal, Size: uint32(8 + len(body))}.appendTo(dst)
	return append(dst, body...)
}

// DecodeHello 解析 HEL/ACK 的正文（不含头）。两者字段布局完全一致，
// 差别只在 ACK 没有 EndpointURL——多读一个字段会被长度检查拦住，
// 所以 ACK 单独用 DecodeAck。
func DecodeHello(body []byte) (Hello, error) {
	d := newDecoder(body)
	var h Hello
	h.Version = d.U32()
	h.ReceiveBufSize = d.U32()
	h.SendBufSize = d.U32()
	h.MaxMessageSize = d.U32()
	h.MaxChunkCount = d.U32()
	h.EndpointURL = d.String()
	return h, d.Err()
}

// Acknowledge 是连接层确认。
type Acknowledge struct {
	Version        uint32
	ReceiveBufSize uint32
	SendBufSize    uint32
	MaxMessageSize uint32
	MaxChunkCount  uint32
}

func EncodeAcknowledge(a Acknowledge) []byte {
	e := &encoder{}
	e.U32(a.Version)
	e.U32(a.ReceiveBufSize)
	e.U32(a.SendBufSize)
	e.U32(a.MaxMessageSize)
	e.U32(a.MaxChunkCount)
	body := e.Bytes()
	dst := make([]byte, 0, 8+len(body))
	dst = ConnHeader{Type: MsgAck, Chunk: ChunkFinal, Size: uint32(8 + len(body))}.appendTo(dst)
	return append(dst, body...)
}

func DecodeAcknowledge(body []byte) (Acknowledge, error) {
	d := newDecoder(body)
	var a Acknowledge
	a.Version = d.U32()
	a.ReceiveBufSize = d.U32()
	a.SendBufSize = d.U32()
	a.MaxMessageSize = d.U32()
	a.MaxChunkCount = d.U32()
	return a, d.Err()
}

// ErrorMessage 是连接层错误。
type ErrorMessage struct {
	Code   StatusCode
	Reason string
}

func EncodeErrorMessage(m ErrorMessage) []byte {
	e := &encoder{}
	e.U32(uint32(m.Code))
	e.String(m.Reason)
	body := e.Bytes()
	dst := make([]byte, 0, 8+len(body))
	dst = ConnHeader{Type: MsgError, Chunk: ChunkFinal, Size: uint32(8 + len(body))}.appendTo(dst)
	return append(dst, body...)
}

func DecodeErrorMessage(body []byte) (ErrorMessage, error) {
	d := newDecoder(body)
	m := ErrorMessage{Code: StatusCode(d.U32()), Reason: d.String()}
	return m, d.Err()
}

// ---------------------------------------------------------------------------
// 安全会话报文
// ---------------------------------------------------------------------------

// AsymmetricSecurityHeader 是 OPN 报文的头部（Policy=None 时证书与
// 指纹都为空）。字段仍然要写：把三个字段直接省略会让服务端解析错位。
type AsymmetricSecurityHeader struct {
	PolicyURI          string
	SenderCertificate  []byte
	ReceiverThumbprint []byte
}

func (h AsymmetricSecurityHeader) encode(e *encoder) {
	e.String(h.PolicyURI)
	e.ByteString(h.SenderCertificate)
	e.ByteString(h.ReceiverThumbprint)
}

func (d *decoder) AsymmetricSecurityHeader() AsymmetricSecurityHeader {
	var h AsymmetricSecurityHeader
	h.PolicyURI = d.String()
	h.SenderCertificate = d.ByteString()
	h.ReceiverThumbprint = d.ByteString()
	return h
}

// SequenceHeader 是每个安全报文都带的序号与请求号。
type SequenceHeader struct {
	Number    uint32
	RequestID uint32
}

func (s SequenceHeader) encode(e *encoder) {
	e.U32(s.Number)
	e.U32(s.RequestID)
}

func (d *decoder) SequenceHeader() SequenceHeader {
	return SequenceHeader{Number: d.U32(), RequestID: d.U32()}
}

// EncodeOpenMessage 组装一条 OPN 报文。
func EncodeOpenMessage(channelID uint32, sec AsymmetricSecurityHeader, seq SequenceHeader, serviceID uint32, body []byte) []byte {
	e := &encoder{}
	sec.encode(e)
	seq.encode(e)
	writeExtensionObject(e, serviceID, body)
	payload := e.Bytes()

	dst := make([]byte, 0, 12+len(payload))
	dst = append(dst, MsgOpen...)
	dst = append(dst, ChunkFinal)
	dst = appendU32(dst, uint32(12+len(payload)))
	dst = appendU32(dst, channelID)
	return append(dst, payload...)
}

// EncodeSecureMessage 组装 MSG/CLO 报文。
func EncodeSecureMessage(typ string, channelID, tokenID uint32, seq SequenceHeader, serviceID uint32, body []byte) []byte {
	e := &encoder{}
	e.U32(tokenID)
	seq.encode(e)
	writeExtensionObject(e, serviceID, body)
	payload := e.Bytes()

	dst := make([]byte, 0, 12+len(payload))
	dst = append(dst, typ...)
	dst = append(dst, ChunkFinal)
	dst = appendU32(dst, uint32(12+len(payload)))
	dst = appendU32(dst, channelID)
	return append(dst, payload...)
}

// SecureMessage 是解码后的一条 OPN/MSG/CLO 报文。
type SecureMessage struct {
	Type      string
	ChannelID uint32
	TokenID   uint32 // 仅 MSG/CLO
	Seq       SequenceHeader
	Security  AsymmetricSecurityHeader // 仅 OPN
	ServiceID uint32
	Body      []byte // 服务正文，不含 ResponseHeader 之外的包装
}

// DecodeSecureMessage 解析一条完整的 OPN/MSG/CLO 报文。
func DecodeSecureMessage(raw []byte) (SecureMessage, error) {
	h, err := DecodeSecureHeader(raw)
	if err != nil {
		return SecureMessage{}, err
	}
	if int(h.Size) > len(raw) {
		return SecureMessage{}, ErrShort
	}
	raw = raw[:h.Size]
	if h.Chunk != ChunkFinal {
		// 分块响应需要按 Chunk 拼装；本实现的单次请求都远小于缓冲区，
		// 见到分块说明对端配置异常，直接报错比拼错更强。
		return SecureMessage{}, fmt.Errorf("opcua: chunked messages are not supported (chunk=%q)", string(h.Chunk))
	}

	m := SecureMessage{Type: h.Type, ChannelID: h.ChannelID}
	d := newDecoder(raw[12:])
	switch h.Type {
	case MsgOpen:
		m.Security = d.AsymmetricSecurityHeader()
	case MsgMessage, MsgClose:
		m.TokenID = d.U32()
	default:
		return SecureMessage{}, fmt.Errorf("opcua: unexpected message type %q", h.Type)
	}
	m.Seq = d.SequenceHeader()
	x := d.ExtensionObject()
	m.ServiceID = x.TypeID.ID
	m.Body = x.Body
	if err := d.Err(); err != nil {
		return SecureMessage{}, err
	}
	return m, nil
}

// writeExtensionObject 写服务体包装。
func writeExtensionObject(e *encoder, serviceID uint32, body []byte) {
	Numeric(0, serviceID).Encode(e)
	e.Byte(0x01)
	e.I32(int32(len(body)))
	e.b = append(e.b, body...)
}

// EncodedService 返回不带报文头、只含 ExtensionObject 的服务体，
// 供测试与 sim 复用。
func EncodedService(serviceID uint32, body []byte) []byte {
	e := &encoder{}
	writeExtensionObject(e, serviceID, body)
	return e.Bytes()
}

// ---------------------------------------------------------------------------
// 请求/响应头
// ---------------------------------------------------------------------------

// RequestHeader 是每个服务请求的公共头。
type RequestHeader struct {
	AuthenticationToken NodeID
	Timestamp           time.Time
	RequestHandle       uint32
	ReturnDiagnostics   uint32
	AuditEntryID        string
	TimeoutHint         uint32
}

func (h RequestHeader) encode(e *encoder) {
	h.AuthenticationToken.Encode(e)
	e.Time(h.Timestamp)
	e.U32(h.RequestHandle)
	e.U32(h.ReturnDiagnostics)
	e.String(h.AuditEntryID)
	e.U32(h.TimeoutHint)
	ExtensionObject{}.Encode(e) // additionalHeader：无
}

func (d *decoder) RequestHeader() RequestHeader {
	var h RequestHeader
	h.AuthenticationToken = d.NodeID()
	h.Timestamp = d.Time()
	h.RequestHandle = d.U32()
	h.ReturnDiagnostics = d.U32()
	h.AuditEntryID = d.String()
	h.TimeoutHint = d.U32()
	_ = d.ExtensionObject()
	return h
}

// ResponseHeader 是每个服务响应的公共头。
type ResponseHeader struct {
	Timestamp     time.Time
	RequestHandle uint32
	ServiceResult StatusCode
	StringTable   []string
}

func (h ResponseHeader) encode(e *encoder) {
	e.Time(h.Timestamp)
	e.U32(h.RequestHandle)
	e.U32(uint32(h.ServiceResult))
	e.Byte(0) // serviceDiagnostics：空 DiagnosticInfo
	e.ArrayLength(len(h.StringTable))
	for _, s := range h.StringTable {
		e.String(s)
	}
	ExtensionObject{}.Encode(e)
}

func (d *decoder) ResponseHeader() ResponseHeader {
	var h ResponseHeader
	h.Timestamp = d.Time()
	h.RequestHandle = d.U32()
	h.ServiceResult = StatusCode(d.U32())
	d.diagnosticInfo(0)
	n := d.decodeArrayLen(1024)
	if n > 0 {
		h.StringTable = make([]string, 0, n)
		for i := 0; i < n; i++ {
			h.StringTable = append(h.StringTable, d.String())
		}
	}
	_ = d.ExtensionObject()
	return h
}

// ---------------------------------------------------------------------------
// 安全通道 / 会话
// ---------------------------------------------------------------------------

// OpenSecureChannelRequest 打开安全通道。
type OpenSecureChannelRequest struct {
	Header                RequestHeader
	ClientProtocolVersion uint32
	RequestType           uint32
	SecurityMode          uint32
	ClientNonce           []byte
	RequestedLifetime     uint32
}

func (r OpenSecureChannelRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.ClientProtocolVersion)
	e.U32(r.RequestType)
	e.U32(r.SecurityMode)
	e.ByteString(r.ClientNonce)
	e.U32(r.RequestedLifetime)
	return e.Bytes()
}

func DecodeOpenSecureChannelRequest(b []byte) (OpenSecureChannelRequest, error) {
	d := newDecoder(b)
	var r OpenSecureChannelRequest
	r.Header = d.RequestHeader()
	r.ClientProtocolVersion = d.U32()
	r.RequestType = d.U32()
	r.SecurityMode = d.U32()
	r.ClientNonce = d.ByteString()
	r.RequestedLifetime = d.U32()
	return r, d.Err()
}

// ChannelSecurityToken 是服务端下发的通道令牌。
type ChannelSecurityToken struct {
	ChannelID       uint32
	TokenID         uint32
	CreatedAt       time.Time
	RevisedLifetime uint32
}

// OpenSecureChannelResponse 是打开通道的响应。
type OpenSecureChannelResponse struct {
	Header                ResponseHeader
	ServerProtocolVersion uint32
	Token                 ChannelSecurityToken
	ServerNonce           []byte
}

func (r OpenSecureChannelResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.ServerProtocolVersion)
	e.U32(r.Token.ChannelID)
	e.U32(r.Token.TokenID)
	e.Time(r.Token.CreatedAt)
	e.U32(r.Token.RevisedLifetime)
	e.ByteString(r.ServerNonce)
	return e.Bytes()
}

func DecodeOpenSecureChannelResponse(b []byte) (OpenSecureChannelResponse, error) {
	d := newDecoder(b)
	var r OpenSecureChannelResponse
	r.Header = d.ResponseHeader()
	r.ServerProtocolVersion = d.U32()
	r.Token.ChannelID = d.U32()
	r.Token.TokenID = d.U32()
	r.Token.CreatedAt = d.Time()
	r.Token.RevisedLifetime = d.U32()
	r.ServerNonce = d.ByteString()
	return r, d.Err()
}

// ApplicationDescription 描述一个应用（客户端或服务端）。
type ApplicationDescription struct {
	ApplicationURI      string
	ProductURI          string
	ApplicationName     LocalizedText
	ApplicationType     uint32
	GatewayServerURI    string
	DiscoveryProfileURI string
	DiscoveryURLs       []string
}

func (a ApplicationDescription) encode(e *encoder) {
	e.String(a.ApplicationURI)
	e.String(a.ProductURI)
	a.ApplicationName.Encode(e)
	e.U32(a.ApplicationType)
	e.String(a.GatewayServerURI)
	e.String(a.DiscoveryProfileURI)
	e.ArrayLength(len(a.DiscoveryURLs))
	for _, u := range a.DiscoveryURLs {
		e.String(u)
	}
}

func (d *decoder) ApplicationDescription() ApplicationDescription {
	var a ApplicationDescription
	a.ApplicationURI = d.String()
	a.ProductURI = d.String()
	a.ApplicationName = d.LocalizedText()
	a.ApplicationType = d.U32()
	a.GatewayServerURI = d.String()
	a.DiscoveryProfileURI = d.String()
	n := d.decodeArrayLen(1024)
	for i := 0; i < n; i++ {
		a.DiscoveryURLs = append(a.DiscoveryURLs, d.String())
	}
	return a
}

// UserTokenPolicy 描述一种用户身份令牌。
type UserTokenPolicy struct {
	PolicyID          string
	TokenType         uint32
	IssuedTokenType   string
	IssuerEndpointURL string
	SecurityPolicyURI string
}

func (p UserTokenPolicy) encode(e *encoder) {
	e.String(p.PolicyID)
	e.U32(p.TokenType)
	e.String(p.IssuedTokenType)
	e.String(p.IssuerEndpointURL)
	e.String(p.SecurityPolicyURI)
}

func (d *decoder) UserTokenPolicy() UserTokenPolicy {
	var p UserTokenPolicy
	p.PolicyID = d.String()
	p.TokenType = d.U32()
	p.IssuedTokenType = d.String()
	p.IssuerEndpointURL = d.String()
	p.SecurityPolicyURI = d.String()
	return p
}

// EndpointDescription 描述一个端点。
type EndpointDescription struct {
	EndpointURL         string
	Server              ApplicationDescription
	ServerCertificate   []byte
	SecurityMode        uint32
	SecurityPolicyURI   string
	UserIdentityTokens  []UserTokenPolicy
	TransportProfileURI string
	SecurityLevel       byte
}

func (ep EndpointDescription) encode(e *encoder) {
	e.String(ep.EndpointURL)
	ep.Server.encode(e)
	e.ByteString(ep.ServerCertificate)
	e.U32(ep.SecurityMode)
	e.String(ep.SecurityPolicyURI)
	e.ArrayLength(len(ep.UserIdentityTokens))
	for _, t := range ep.UserIdentityTokens {
		t.encode(e)
	}
	e.String(ep.TransportProfileURI)
	e.Byte(ep.SecurityLevel)
}

func (d *decoder) EndpointDescription() EndpointDescription {
	var ep EndpointDescription
	ep.EndpointURL = d.String()
	ep.Server = d.ApplicationDescription()
	ep.ServerCertificate = d.ByteString()
	ep.SecurityMode = d.U32()
	ep.SecurityPolicyURI = d.String()
	n := d.decodeArrayLen(256)
	for i := 0; i < n; i++ {
		ep.UserIdentityTokens = append(ep.UserIdentityTokens, d.UserTokenPolicy())
	}
	ep.TransportProfileURI = d.String()
	ep.SecurityLevel = d.Byte()
	return ep
}

// SignedSoftwareCertificate 是会话请求里可为空的软件证书。
type SignedSoftwareCertificate struct {
	CertificateData []byte
	Signature       []byte
}

// SignatureData 是签名容器；Policy=None 下两个字段都为空。
type SignatureData struct {
	Algorithm string
	Signature []byte
}

func (s SignatureData) encode(e *encoder) {
	e.String(s.Algorithm)
	e.ByteString(s.Signature)
}

func (d *decoder) SignatureData() SignatureData {
	return SignatureData{Algorithm: d.String(), Signature: d.ByteString()}
}

// CreateSessionRequest 建立会话。
type CreateSessionRequest struct {
	Header                  RequestHeader
	ClientDescription       ApplicationDescription
	ServerURI               string
	EndpointURL             string
	SessionName             string
	ClientNonce             []byte
	ClientCertificate       []byte
	RequestedSessionTimeout float64
	MaxResponseMessageSize  uint32
}

func (r CreateSessionRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	r.ClientDescription.encode(e)
	e.String(r.ServerURI)
	e.String(r.EndpointURL)
	e.String(r.SessionName)
	e.ByteString(r.ClientNonce)
	e.ByteString(r.ClientCertificate)
	e.F64(r.RequestedSessionTimeout)
	e.U32(r.MaxResponseMessageSize)
	return e.Bytes()
}

func DecodeCreateSessionRequest(b []byte) (CreateSessionRequest, error) {
	d := newDecoder(b)
	var r CreateSessionRequest
	r.Header = d.RequestHeader()
	r.ClientDescription = d.ApplicationDescription()
	r.ServerURI = d.String()
	r.EndpointURL = d.String()
	r.SessionName = d.String()
	r.ClientNonce = d.ByteString()
	r.ClientCertificate = d.ByteString()
	r.RequestedSessionTimeout = d.F64()
	r.MaxResponseMessageSize = d.U32()
	return r, d.Err()
}

// CreateSessionResponse 是建立会话的响应。
type CreateSessionResponse struct {
	Header                     ResponseHeader
	SessionID                  NodeID
	AuthenticationToken        NodeID
	RevisedSessionTimeout      float64
	ServerNonce                []byte
	ServerCertificate          []byte
	ServerEndpoints            []EndpointDescription
	ServerSoftwareCertificates []SignedSoftwareCertificate
	ServerSignature            SignatureData
	MaxRequestMessageSize      uint32
}

func (r CreateSessionResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	r.SessionID.Encode(e)
	r.AuthenticationToken.Encode(e)
	e.F64(r.RevisedSessionTimeout)
	e.ByteString(r.ServerNonce)
	e.ByteString(r.ServerCertificate)
	e.ArrayLength(len(r.ServerEndpoints))
	for _, ep := range r.ServerEndpoints {
		ep.encode(e)
	}
	e.ArrayLength(len(r.ServerSoftwareCertificates))
	for _, c := range r.ServerSoftwareCertificates {
		e.ByteString(c.CertificateData)
		e.ByteString(c.Signature)
	}
	r.ServerSignature.encode(e)
	e.U32(r.MaxRequestMessageSize)
	return e.Bytes()
}

func DecodeCreateSessionResponse(b []byte) (CreateSessionResponse, error) {
	d := newDecoder(b)
	var r CreateSessionResponse
	r.Header = d.ResponseHeader()
	r.SessionID = d.NodeID()
	r.AuthenticationToken = d.NodeID()
	r.RevisedSessionTimeout = d.F64()
	r.ServerNonce = d.ByteString()
	r.ServerCertificate = d.ByteString()
	n := d.decodeArrayLen(64)
	for i := 0; i < n; i++ {
		r.ServerEndpoints = append(r.ServerEndpoints, d.EndpointDescription())
	}
	m := d.decodeArrayLen(64)
	for i := 0; i < m; i++ {
		r.ServerSoftwareCertificates = append(r.ServerSoftwareCertificates,
			SignedSoftwareCertificate{CertificateData: d.ByteString(), Signature: d.ByteString()})
	}
	r.ServerSignature = d.SignatureData()
	r.MaxRequestMessageSize = d.U32()
	return r, d.Err()
}

// AnonymousIdentityToken 构造匿名身份令牌的 ExtensionObject。
func AnonymousIdentityToken(policyID string) ExtensionObject {
	e := &encoder{}
	e.String(policyID)
	return NewExtensionObject(321, e.Bytes())
}

// DecodeAnonymousIdentityToken 读出匿名令牌里的 PolicyID。
func DecodeAnonymousIdentityToken(x ExtensionObject) (string, error) {
	d := newDecoder(x.Body)
	s := d.String()
	return s, d.Err()
}

// ActivateSessionRequest 激活会话并提交身份。
type ActivateSessionRequest struct {
	Header                     RequestHeader
	ClientSignature            SignatureData
	ClientSoftwareCertificates []SignedSoftwareCertificate
	LocaleIDs                  []string
	UserIdentityToken          ExtensionObject
	UserTokenSignature         SignatureData
}

func (r ActivateSessionRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	r.ClientSignature.encode(e)
	e.ArrayLength(len(r.ClientSoftwareCertificates))
	for _, c := range r.ClientSoftwareCertificates {
		e.ByteString(c.CertificateData)
		e.ByteString(c.Signature)
	}
	e.ArrayLength(len(r.LocaleIDs))
	for _, s := range r.LocaleIDs {
		e.String(s)
	}
	r.UserIdentityToken.Encode(e)
	r.UserTokenSignature.encode(e)
	return e.Bytes()
}

func DecodeActivateSessionRequest(b []byte) (ActivateSessionRequest, error) {
	d := newDecoder(b)
	var r ActivateSessionRequest
	r.Header = d.RequestHeader()
	r.ClientSignature = d.SignatureData()
	n := d.decodeArrayLen(64)
	for i := 0; i < n; i++ {
		r.ClientSoftwareCertificates = append(r.ClientSoftwareCertificates,
			SignedSoftwareCertificate{CertificateData: d.ByteString(), Signature: d.ByteString()})
	}
	m := d.decodeArrayLen(64)
	for i := 0; i < m; i++ {
		r.LocaleIDs = append(r.LocaleIDs, d.String())
	}
	r.UserIdentityToken = d.ExtensionObject()
	r.UserTokenSignature = d.SignatureData()
	return r, d.Err()
}

// ActivateSessionResponse 是激活会话的响应。
type ActivateSessionResponse struct {
	Header      ResponseHeader
	ServerNonce []byte
	Results     []StatusCode
}

func (r ActivateSessionResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ByteString(r.ServerNonce)
	e.ArrayLength(len(r.Results))
	for _, s := range r.Results {
		e.U32(uint32(s))
	}
	e.ArrayLength(0) // diagnosticInfos
	return e.Bytes()
}

func DecodeActivateSessionResponse(b []byte) (ActivateSessionResponse, error) {
	d := newDecoder(b)
	var r ActivateSessionResponse
	r.Header = d.ResponseHeader()
	r.ServerNonce = d.ByteString()
	n := d.decodeArrayLen(256)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, StatusCode(d.U32()))
	}
	m := d.decodeArrayLen(256)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// CloseSessionRequest 关闭会话。
type CloseSessionRequest struct {
	Header              RequestHeader
	DeleteSubscriptions bool
}

func (r CloseSessionRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.Bool(r.DeleteSubscriptions)
	return e.Bytes()
}

func DecodeCloseSessionRequest(b []byte) (CloseSessionRequest, error) {
	d := newDecoder(b)
	r := CloseSessionRequest{Header: d.RequestHeader(), DeleteSubscriptions: d.Bool()}
	return r, d.Err()
}

// CloseSessionResponse 是关闭会话的响应。
type CloseSessionResponse struct {
	Header ResponseHeader
}

func (r CloseSessionResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	return e.Bytes()
}

func DecodeCloseSessionResponse(b []byte) (CloseSessionResponse, error) {
	d := newDecoder(b)
	r := CloseSessionResponse{Header: d.ResponseHeader()}
	return r, d.Err()
}

// CloseSecureChannelRequest 是 CLO 报文里唯一允许的服务体。
type CloseSecureChannelRequest struct {
	Header RequestHeader
}

func (r CloseSecureChannelRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	return e.Bytes()
}

func DecodeCloseSecureChannelRequest(b []byte) (CloseSecureChannelRequest, error) {
	d := newDecoder(b)
	r := CloseSecureChannelRequest{Header: d.RequestHeader()}
	return r, d.Err()
}

// ---------------------------------------------------------------------------
// Read / Write
// ---------------------------------------------------------------------------

// ReadValueID 指定要读的一个节点属性。
type ReadValueID struct {
	NodeID       NodeID
	AttributeID  uint32
	IndexRange   string
	DataEncoding QualifiedName
}

func (v ReadValueID) encode(e *encoder) {
	v.NodeID.Encode(e)
	e.U32(v.AttributeID)
	e.String(v.IndexRange)
	v.DataEncoding.Encode(e)
}

func (d *decoder) ReadValueID() ReadValueID {
	var v ReadValueID
	v.NodeID = d.NodeID()
	v.AttributeID = d.U32()
	v.IndexRange = d.String()
	v.DataEncoding = d.QualifiedName()
	return v
}

// ReadRequest 批量读取。
type ReadRequest struct {
	Header             RequestHeader
	MaxAge             float64
	TimestampsToReturn uint32
	Nodes              []ReadValueID
}

func (r ReadRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.F64(r.MaxAge)
	e.U32(r.TimestampsToReturn)
	e.ArrayLength(len(r.Nodes))
	for _, n := range r.Nodes {
		n.encode(e)
	}
	return e.Bytes()
}

func DecodeReadRequest(b []byte) (ReadRequest, error) {
	d := newDecoder(b)
	var r ReadRequest
	r.Header = d.RequestHeader()
	r.MaxAge = d.F64()
	r.TimestampsToReturn = d.U32()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.Nodes = append(r.Nodes, d.ReadValueID())
	}
	return r, d.Err()
}

// ReadResponse 批量读取的结果。结果与请求逐项对应，错误放在
// DataValue.Status 里而不是整条响应失败——这是 OPC UA 与 Modbus
// 很重要的一处语义差别：部分失败是正常结果，不是通信错误。
type ReadResponse struct {
	Header ResponseHeader
	Values []DataValue
}

func (r ReadResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Values))
	for _, v := range r.Values {
		v.Encode(e)
	}
	e.ArrayLength(0) // diagnosticInfos
	return e.Bytes()
}

func DecodeReadResponse(b []byte) (ReadResponse, error) {
	d := newDecoder(b)
	var r ReadResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.Values = append(r.Values, d.DataValue())
	}
	m := d.decodeArrayLen(4096)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// WriteValue 指定一次写入。
type WriteValue struct {
	NodeID      NodeID
	AttributeID uint32
	IndexRange  string
	Value       DataValue
}

func (v WriteValue) encode(e *encoder) {
	v.NodeID.Encode(e)
	e.U32(v.AttributeID)
	e.String(v.IndexRange)
	v.Value.Encode(e)
}

func (d *decoder) WriteValue() WriteValue {
	var v WriteValue
	v.NodeID = d.NodeID()
	v.AttributeID = d.U32()
	v.IndexRange = d.String()
	v.Value = d.DataValue()
	return v
}

// WriteRequest 批量写入。
type WriteRequest struct {
	Header RequestHeader
	Nodes  []WriteValue
}

func (r WriteRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Nodes))
	for _, n := range r.Nodes {
		n.encode(e)
	}
	return e.Bytes()
}

func DecodeWriteRequest(b []byte) (WriteRequest, error) {
	d := newDecoder(b)
	var r WriteRequest
	r.Header = d.RequestHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.Nodes = append(r.Nodes, d.WriteValue())
	}
	return r, d.Err()
}

// WriteResponse 每个写入项一个状态码。
type WriteResponse struct {
	Header  ResponseHeader
	Results []StatusCode
}

func (r WriteResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Results))
	for _, s := range r.Results {
		e.U32(uint32(s))
	}
	e.ArrayLength(0)
	return e.Bytes()
}

func DecodeWriteResponse(b []byte) (WriteResponse, error) {
	d := newDecoder(b)
	var r WriteResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, StatusCode(d.U32()))
	}
	m := d.decodeArrayLen(4096)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// ---------------------------------------------------------------------------
// Browse / GetEndpoints / ServiceFault
// ---------------------------------------------------------------------------

// ViewDescription 指定浏览视图；空视图表示整个地址空间。
type ViewDescription struct {
	ViewID      NodeID
	Timestamp   time.Time
	ViewVersion uint32
}

func (v ViewDescription) encode(e *encoder) {
	v.ViewID.Encode(e)
	e.Time(v.Timestamp)
	e.U32(v.ViewVersion)
}

func (d *decoder) ViewDescription() ViewDescription {
	return ViewDescription{ViewID: d.NodeID(), Timestamp: d.Time(), ViewVersion: d.U32()}
}

// BrowseDescription 描述一次浏览。
type BrowseDescription struct {
	NodeID          NodeID
	Direction       uint32
	ReferenceTypeID NodeID
	IncludeSubtypes bool
	NodeClassMask   uint32
	ResultMask      uint32
}

func (b BrowseDescription) encode(e *encoder) {
	b.NodeID.Encode(e)
	e.U32(b.Direction)
	b.ReferenceTypeID.Encode(e)
	e.Bool(b.IncludeSubtypes)
	e.U32(b.NodeClassMask)
	e.U32(b.ResultMask)
}

func (d *decoder) BrowseDescription() BrowseDescription {
	var b BrowseDescription
	b.NodeID = d.NodeID()
	b.Direction = d.U32()
	b.ReferenceTypeID = d.NodeID()
	b.IncludeSubtypes = d.Bool()
	b.NodeClassMask = d.U32()
	b.ResultMask = d.U32()
	return b
}

// ReferenceDescription 是浏览结果中的一条引用。
type ReferenceDescription struct {
	ReferenceTypeID NodeID
	IsForward       bool
	Target          ExpandedNodeID
	BrowseName      QualifiedName
	DisplayName     LocalizedText
	NodeClass       uint32
	TypeDefinition  ExpandedNodeID
}

func (r ReferenceDescription) encode(e *encoder) {
	r.ReferenceTypeID.Encode(e)
	e.Bool(r.IsForward)
	r.Target.Encode(e)
	r.BrowseName.Encode(e)
	r.DisplayName.Encode(e)
	e.U32(r.NodeClass)
	r.TypeDefinition.Encode(e)
}

func (d *decoder) ReferenceDescription() ReferenceDescription {
	var r ReferenceDescription
	r.ReferenceTypeID = d.NodeID()
	r.IsForward = d.Bool()
	r.Target = d.ExpandedNodeID()
	r.BrowseName = d.QualifiedName()
	r.DisplayName = d.LocalizedText()
	r.NodeClass = d.U32()
	r.TypeDefinition = d.ExpandedNodeID()
	return r
}

// BrowseResult 是单个节点的浏览结果。
type BrowseResult struct {
	Status            StatusCode
	ContinuationPoint []byte
	References        []ReferenceDescription
}

func (r BrowseResult) encode(e *encoder) {
	e.U32(uint32(r.Status))
	e.ByteString(r.ContinuationPoint)
	e.ArrayLength(len(r.References))
	for _, ref := range r.References {
		ref.encode(e)
	}
}

func (d *decoder) BrowseResult() BrowseResult {
	var r BrowseResult
	r.Status = StatusCode(d.U32())
	r.ContinuationPoint = d.ByteString()
	n := d.decodeArrayLen(65535)
	for i := 0; i < n; i++ {
		r.References = append(r.References, d.ReferenceDescription())
	}
	return r
}

// BrowseRequest 浏览地址空间。
type BrowseRequest struct {
	Header         RequestHeader
	View           ViewDescription
	MaxRefsPerNode uint32
	Nodes          []BrowseDescription
}

func (r BrowseRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	r.View.encode(e)
	e.U32(r.MaxRefsPerNode)
	e.ArrayLength(len(r.Nodes))
	for _, n := range r.Nodes {
		n.encode(e)
	}
	return e.Bytes()
}

func DecodeBrowseRequest(b []byte) (BrowseRequest, error) {
	d := newDecoder(b)
	var r BrowseRequest
	r.Header = d.RequestHeader()
	r.View = d.ViewDescription()
	r.MaxRefsPerNode = d.U32()
	n := d.decodeArrayLen(256)
	for i := 0; i < n; i++ {
		r.Nodes = append(r.Nodes, d.BrowseDescription())
	}
	return r, d.Err()
}

// BrowseResponse 是浏览响应。
type BrowseResponse struct {
	Header  ResponseHeader
	Results []BrowseResult
}

func (r BrowseResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Results))
	for _, res := range r.Results {
		res.encode(e)
	}
	e.ArrayLength(0)
	return e.Bytes()
}

func DecodeBrowseResponse(b []byte) (BrowseResponse, error) {
	d := newDecoder(b)
	var r BrowseResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(256)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, d.BrowseResult())
	}
	m := d.decodeArrayLen(256)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// GetEndpointsRequest 查询服务端端点。
type GetEndpointsRequest struct {
	Header      RequestHeader
	EndpointURL string
	LocaleIDs   []string
	ProfileURIs []string
}

func (r GetEndpointsRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.String(r.EndpointURL)
	e.ArrayLength(len(r.LocaleIDs))
	for _, s := range r.LocaleIDs {
		e.String(s)
	}
	e.ArrayLength(len(r.ProfileURIs))
	for _, s := range r.ProfileURIs {
		e.String(s)
	}
	return e.Bytes()
}

func DecodeGetEndpointsRequest(b []byte) (GetEndpointsRequest, error) {
	d := newDecoder(b)
	var r GetEndpointsRequest
	r.Header = d.RequestHeader()
	r.EndpointURL = d.String()
	n := d.decodeArrayLen(64)
	for i := 0; i < n; i++ {
		r.LocaleIDs = append(r.LocaleIDs, d.String())
	}
	m := d.decodeArrayLen(64)
	for i := 0; i < m; i++ {
		r.ProfileURIs = append(r.ProfileURIs, d.String())
	}
	return r, d.Err()
}

// GetEndpointsResponse 返回服务端端点列表。
type GetEndpointsResponse struct {
	Header    ResponseHeader
	Endpoints []EndpointDescription
}

func (r GetEndpointsResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Endpoints))
	for _, ep := range r.Endpoints {
		ep.encode(e)
	}
	return e.Bytes()
}

func DecodeGetEndpointsResponse(b []byte) (GetEndpointsResponse, error) {
	d := newDecoder(b)
	var r GetEndpointsResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(64)
	for i := 0; i < n; i++ {
		r.Endpoints = append(r.Endpoints, d.EndpointDescription())
	}
	return r, d.Err()
}

// ServiceFault 是服务级错误。
type ServiceFault struct {
	Header ResponseHeader
}

func (f ServiceFault) Encode() []byte {
	e := &encoder{}
	f.Header.encode(e)
	return e.Bytes()
}

func DecodeServiceFault(b []byte) (ServiceFault, error) {
	d := newDecoder(b)
	f := ServiceFault{Header: d.ResponseHeader()}
	return f, d.Err()
}

// DecodeResponseHeader 从任意服务正文里取出响应头，用于错误诊断：
// 当服务体解析失败时，至少能告诉运维「服务端返回了 BadXXX」。
func DecodeResponseHeader(b []byte) ResponseHeader {
	return newDecoder(b).ResponseHeader()
}
