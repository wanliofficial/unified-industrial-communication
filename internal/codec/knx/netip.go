package knx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// KNXnet/IP 信封的固定部分。
//
//	0   结构长度 0x06
//	1   协议版本 0x10（1.0）
//	2..3 服务类型（大端）
//	4..5 总长度（含 6 字节头，大端）
//
// 总长度包含头部这一点很容易记错成「载荷长度」。写成后者时网关会把
// 头 6 字节也算进载荷，解析出的 cEMI 消息码变成 0x06，整条报文被丢弃。
const (
	HeaderLen = 6
	// ProtocolVersion 是当前唯一有效的版本：1.0。
	ProtocolVersion = 0x10
)

// 服务类型。
const (
	SvcSearchRequest       = 0x0201
	SvcSearchResponse      = 0x0202
	SvcDescriptionRequest  = 0x0203
	SvcDescriptionResponse = 0x0204
	SvcConnectRequest      = 0x0205
	SvcConnectResponse     = 0x0206
	SvcConnectionStateReq  = 0x0207
	SvcConnectionStateRes  = 0x0208
	SvcDisconnectRequest   = 0x0209
	SvcDisconnectResponse  = 0x020A
	SvcDeviceConfigRequest = 0x0310
	SvcDeviceConfigACK     = 0x0311
	SvcTunnellingRequest   = 0x0420
	SvcTunnellingACK       = 0x0421
	SvcRoutingIndication   = 0x0530
	SvcRoutingLostMessage  = 0x0531
	SvcRoutingBusy         = 0x0532
	SvcSecureWrapper       = 0x0950
	SvcSessionRequest      = 0x0951
	SvcSessionResponse     = 0x0952
	SvcSessionAuthenticate = 0x0953
	SvcSessionStatus       = 0x0954
	SvcTimerNotify         = 0x0955
)

// ServiceName 返回服务类型的中文名，用于日志与错误信息。
func ServiceName(svc uint16) string {
	switch svc {
	case SvcSearchRequest:
		return "SEARCH_REQUEST"
	case SvcSearchResponse:
		return "SEARCH_RESPONSE"
	case SvcConnectRequest:
		return "CONNECT_REQUEST"
	case SvcConnectResponse:
		return "CONNECT_RESPONSE"
	case SvcConnectionStateReq:
		return "CONNECTIONSTATE_REQUEST"
	case SvcConnectionStateRes:
		return "CONNECTIONSTATE_RESPONSE"
	case SvcDisconnectRequest:
		return "DISCONNECT_REQUEST"
	case SvcDisconnectResponse:
		return "DISCONNECT_RESPONSE"
	case SvcTunnellingRequest:
		return "TUNNELLING_REQUEST"
	case SvcTunnellingACK:
		return "TUNNELLING_ACK"
	case SvcRoutingIndication:
		return "ROUTING_INDICATION"
	case SvcSecureWrapper:
		return "SECURE_WRAPPER"
	}
	return fmt.Sprintf("0x%04X", svc)
}

// 连接类型与隧道层。
const (
	// ConnTypeTunnel 是隧道连接（采集与下发走的正是这一种）。
	ConnTypeTunnel = 0x04
	// ConnTypeDeviceMgmt 是设备管理连接（工程组态工具的下载通道），本包不做。
	ConnTypeDeviceMgmt = 0x03

	// LayerLink 是链路层隧道：收发 L_Data.*。
	LayerLink = 0x02
	// LayerRaw 是原始层隧道；LayerBusmon 是总线监视。
	LayerRaw    = 0x04
	LayerBusmon = 0x80
)

// 主机协议码（HPAI 里的 protocol 字段）。
const (
	ProtoUDP4 = 0x01
	ProtoTCP4 = 0x02
)

// 状态码（CONNECT_RESPONSE / TUNNELLING_ACK / CONNECTIONSTATE_RESPONSE 共用）。
const (
	StatusNoError                 = 0x00
	StatusHostProtocolType        = 0x01
	StatusVersionNotSupported     = 0x02
	StatusSequenceNumber          = 0x04
	StatusConnectionID            = 0x21
	StatusConnectionType          = 0x22
	StatusConnectionOption        = 0x23
	StatusNoMoreConnections       = 0x24
	StatusNoMoreUniqueConnections = 0x25
	StatusDataConnection          = 0x26
	StatusKNXConnection           = 0x27
	StatusTunnellingLayer         = 0x29
)

// StatusName 返回状态码的中文说明。
func StatusName(s uint8) string {
	switch s {
	case StatusNoError:
		return "成功"
	case StatusHostProtocolType:
		return "不支持的主机协议类型"
	case StatusVersionNotSupported:
		return "不支持的协议版本"
	case StatusSequenceNumber:
		return "序号错乱"
	case StatusConnectionID:
		return "通道号不存在"
	case StatusConnectionType:
		return "不支持的连接类型"
	case StatusConnectionOption:
		return "不支持的连接选项"
	case StatusNoMoreConnections:
		return "隧道连接数已满"
	case StatusNoMoreUniqueConnections:
		return "没有可分配的物理地址"
	case StatusDataConnection:
		return "数据连接错误"
	case StatusKNXConnection:
		return "KNX 总线连接错误"
	case StatusTunnellingLayer:
		return "不支持的隧道层"
	}
	return fmt.Sprintf("未知状态 0x%02X", s)
}

// DefaultPort 是 KNXnet/IP 的注册端口。
//
// 3671 是标准端口，但现场一定会有「同一个网关开多个隧道端点」或「一台
// 网关同时被组态工具与另一台采集机占用，于是把隧道挪到 3672」的情况，因此配置里
// 端口永远可覆盖。
const DefaultPort = 3671

// HostPort 把 `host[:port]` 归一成 `host:port`，缺端口时补 3671。
//
// 不做「顺便校验 IP 字面量」：现场常用主机名或 `/etc/hosts` 里的别名，
// 真正的解析交给 DialUDP。但 HPAI 需要 IP 字面量（它是协议字段，不是
// 名字），所以驱动在 CONNECT 阶段用本端套接字的实际地址，而不是配置值。
func HostPort(addr string) (string, error) {
	s := strings.TrimSpace(addr)
	if s == "" {
		return "", errors.New("knx: address is empty (want host:port, e.g. 192.168.1.10:3671)")
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s, nil
	}
	// 没有端口：只要不是 IPv6 字面量就直接补默认端口。IPv6 必须显式写
	// 方括号与端口，否则 `fe80::1` 与 `fe80` + 端口无法区分。
	if strings.Contains(s, ":") && !strings.HasPrefix(s, "[") {
		return "", fmt.Errorf("knx: address %q looks like an IPv6 literal; write [addr]:port", addr)
	}
	return net.JoinHostPort(strings.Trim(s, "[]"), strconv.Itoa(DefaultPort)), nil
}

// HPAI 是 Host Protocol Address Information：告诉对端「往哪个 IP:端口 回包」。
//
//	0     结构长度 0x08
//	1     主机协议 0x01=UDP / 0x02=TCP
//	2..5  IPv4 地址
//	6..7  端口（大端）
//
// 全零 IP + 0 端口是「route back」：表示「用这条报文的源地址回我」。
// 网关在 NAT 后面时这是唯一能用的写法，因此解析时不能把它当错误。
type HPAI struct {
	Protocol uint8
	IP       net.IP
	Port     uint16
}

// HPALen 是 HPAI 的固定长度。
const HPALen = 8

// NewHPAI 用「IP:端口」构造 HPAI。
func NewHPAI(addr string) (HPAI, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return HPAI{}, fmt.Errorf("knx: hpai address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return HPAI{}, fmt.Errorf("knx: hpai address %q: not an IP literal", host)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return HPAI{}, fmt.Errorf("knx: hpai address %q: only IPv4 is supported", host)
	}
	var port uint16
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return HPAI{}, fmt.Errorf("knx: hpai address %q: bad port: %w", addr, err)
	}
	return HPAI{Protocol: ProtoUDP4, IP: append(net.IP(nil), ip4...), Port: port}, nil
}

// RouteBack 表示「用报文源地址回送」。
func (h HPAI) RouteBack() bool {
	return (len(h.IP) == 0 || h.IP.IsUnspecified()) && h.Port == 0
}

// String 输出 `IP:端口`。
func (h HPAI) String() string {
	if h.RouteBack() {
		return "route-back"
	}
	return net.JoinHostPort(h.IP.String(), fmt.Sprint(h.Port))
}

// AppendHPAI 把 HPAI 编码到 out。
func AppendHPAI(out []byte, h HPAI) []byte {
	ip := h.IP.To4()
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	out = append(out, HPALen, h.Protocol)
	out = append(out, ip...)
	return binary.BigEndian.AppendUint16(out, h.Port)
}

// ParseHPAI 从 b 的开头解析一个 HPAI，返回 HPAI 与消耗的字节数。
func ParseHPAI(b []byte) (HPAI, int, error) {
	var h HPAI
	if len(b) < HPALen {
		return h, 0, fmt.Errorf("knx: HPAI truncated (%d bytes, want %d)", len(b), HPALen)
	}
	if b[0] != HPALen {
		return h, 0, fmt.Errorf("knx: HPAI structure length %d, want %d", b[0], HPALen)
	}
	h.Protocol = b[1]
	h.IP = append(net.IP(nil), b[2:6]...)
	h.Port = binary.BigEndian.Uint16(b[6:8])
	return h, HPALen, nil
}

// Frame 是一条完整的 KNXnet/IP 报文：头部 + 原始载荷。
//
// 载荷按服务类型各自解析，不在这里做联合体式的「解析成某一种结构」：
// 隧道场景里真正需要解析的只有 6 种服务，而一个 switch 比反射式的
// 通用解析更容易在错误信息里说清「哪一段不对」。
type Frame struct {
	Service uint16
	Payload []byte
}

// BuildFrame 组装一条报文。
func BuildFrame(service uint16, payload []byte) []byte {
	total := HeaderLen + len(payload)
	out := make([]byte, 0, total)
	out = append(out, HeaderLen, ProtocolVersion)
	out = binary.BigEndian.AppendUint16(out, service)
	out = binary.BigEndian.AppendUint16(out, uint16(total))
	return append(out, payload...)
}

// ParseFrame 解析报文头并切出载荷。
//
// 三重校验都是必要的：
//
//  1. 结构长度必须是 6——有些老设备会在这里写 0x0A（把 HPAI 也算进去）。
//  2. 协议版本必须是 0x10。
//  3. 总长度必须 >= 6 且不超过实际收到的字节数；小于 6 说明对端把
//     「载荷长度」写进了这个字段，按它切片会得到空的 cEMI。
func ParseFrame(b []byte) (Frame, error) {
	var f Frame
	if len(b) < HeaderLen {
		return f, fmt.Errorf("knx: frame too short (%d bytes, want >= %d)", len(b), HeaderLen)
	}
	if b[0] != HeaderLen {
		return f, fmt.Errorf("knx: header length %d, want %d", b[0], HeaderLen)
	}
	if b[1] != ProtocolVersion {
		return f, fmt.Errorf("knx: protocol version 0x%02X, want 0x%02X", b[1], ProtocolVersion)
	}
	f.Service = binary.BigEndian.Uint16(b[2:4])
	total := int(binary.BigEndian.Uint16(b[4:6]))
	if total < HeaderLen {
		return f, fmt.Errorf("knx: total length %d is smaller than the header (%d)", total, HeaderLen)
	}
	if total > len(b) {
		return f, fmt.Errorf("knx: total length %d exceeds the received %d bytes", total, len(b))
	}
	f.Payload = append([]byte(nil), b[HeaderLen:total]...)
	return f, nil
}

// ---------------------------------------------------------------------------
// 各服务的报文结构
// ---------------------------------------------------------------------------

// ConnectRequest 是 CONNECT_REQUEST 的载荷。
type ConnectRequest struct {
	Control HPAI
	Data    HPAI
	Layer   uint8
}

// Encode 组装请求载荷（隧道连接、链路层）。
func (c ConnectRequest) Encode() []byte {
	out := make([]byte, 0, 2*HPALen+4)
	out = AppendHPAI(out, c.Control)
	out = AppendHPAI(out, c.Data)
	// CRI：结构长度 4、连接类型 4（隧道）、隧道层、保留。
	layer := c.Layer
	if layer == 0 {
		layer = LayerLink
	}
	return append(out, 4, ConnTypeTunnel, layer, 0x00)
}

// ParseConnectRequest 解析 CONNECT_REQUEST。
func ParseConnectRequest(b []byte) (ConnectRequest, error) {
	var c ConnectRequest
	ctl, n, err := ParseHPAI(b)
	if err != nil {
		return c, fmt.Errorf("knx: CONNECT_REQUEST control endpoint: %w", err)
	}
	c.Control = ctl
	data, m, err := ParseHPAI(b[n:])
	if err != nil {
		return c, fmt.Errorf("knx: CONNECT_REQUEST data endpoint: %w", err)
	}
	c.Data = data
	rest := b[n+m:]
	if len(rest) < 2 {
		return c, errors.New("knx: CONNECT_REQUEST is missing the connection request information block")
	}
	length := int(rest[0])
	if length < 2 || len(rest) < length {
		return c, fmt.Errorf("knx: CONNECT_REQUEST CRI length %d is invalid", length)
	}
	connType := rest[1]
	if connType != ConnTypeTunnel {
		return c, fmt.Errorf("knx: connection type 0x%02X is not tunnelling (0x%02X)", connType, ConnTypeTunnel)
	}
	if length >= 3 {
		c.Layer = rest[2]
	}
	return c, nil
}

// ConnectResponse 是 CONNECT_RESPONSE 的载荷。
type ConnectResponse struct {
	Channel uint8
	Status  uint8
	// Data 是网关回给客户端的数据端点（route-back 表示「用源地址」）。
	Data HPAI
	// Assigned 是网关分配给这条隧道的物理地址（CRD 里给出）。
	Assigned IndividualAddress
	// HasCRD 表示响应里带了连接响应数据块。
	HasCRD bool
}

// Encode 组装响应载荷。
func (c ConnectResponse) Encode() []byte {
	out := make([]byte, 0, 2+HPALen+4)
	out = append(out, c.Channel, c.Status)
	if c.Status != StatusNoError {
		// 失败响应只有 2 字节：带 HPAI 会让部分客户端读越界。
		return out
	}
	out = AppendHPAI(out, c.Data)
	if !c.HasCRD {
		return out
	}
	out = append(out, 4, ConnTypeTunnel)
	return append(out, byte(c.Assigned>>8), byte(c.Assigned))
}

// ParseConnectResponse 解析 CONNECT_RESPONSE。
func ParseConnectResponse(b []byte) (ConnectResponse, error) {
	var c ConnectResponse
	if len(b) < 2 {
		return c, errors.New("knx: CONNECT_RESPONSE too short")
	}
	c.Channel = b[0]
	c.Status = b[1]
	if c.Status != StatusNoError {
		return c, nil
	}
	data, n, err := ParseHPAI(b[2:])
	if err != nil {
		return c, fmt.Errorf("knx: CONNECT_RESPONSE data endpoint: %w", err)
	}
	c.Data = data
	rest := b[2+n:]
	if len(rest) >= 2 {
		length := int(rest[0])
		if length >= 4 && len(rest) >= length && rest[1] == ConnTypeTunnel {
			c.HasCRD = true
			c.Assigned = IndividualAddress(binary.BigEndian.Uint16(rest[2:4]))
		}
	}
	return c, nil
}

// TunnellingRequest 是 TUNNELLING_REQUEST 的载荷：4 字节连接头 + cEMI。
//
//	0     结构长度 4
//	1     通道号
//	2     序号（0..255 循环）
//	3     保留 0
//	4..   cEMI
type TunnellingRequest struct {
	Channel uint8
	Seq     uint8
	CEMI    []byte
}

// Encode 组装载荷。
func (t TunnellingRequest) Encode() []byte {
	out := make([]byte, 0, 4+len(t.CEMI))
	out = append(out, 4, t.Channel, t.Seq, 0x00)
	return append(out, t.CEMI...)
}

// ParseTunnellingRequest 解析 TUNNELLING_REQUEST。
func ParseTunnellingRequest(b []byte) (TunnellingRequest, error) {
	var t TunnellingRequest
	if len(b) < 4 {
		return t, fmt.Errorf("knx: TUNNELLING_REQUEST too short (%d bytes)", len(b))
	}
	if b[0] != 4 {
		return t, fmt.Errorf("knx: TUNNELLING_REQUEST connection header length %d, want 4", b[0])
	}
	t.Channel = b[1]
	t.Seq = b[2]
	t.CEMI = append([]byte(nil), b[4:]...)
	return t, nil
}

// TunnellingACK 是 TUNNELLING_ACK 的载荷（4 字节定长）。
type TunnellingACK struct {
	Channel uint8
	Seq     uint8
	Status  uint8
}

// Encode 组装载荷。
func (t TunnellingACK) Encode() []byte {
	return []byte{4, t.Channel, t.Seq, t.Status}
}

// ParseTunnellingACK 解析 TUNNELLING_ACK。
func ParseTunnellingACK(b []byte) (TunnellingACK, error) {
	var t TunnellingACK
	if len(b) < 4 {
		return t, fmt.Errorf("knx: TUNNELLING_ACK too short (%d bytes)", len(b))
	}
	if b[0] != 4 {
		return t, fmt.Errorf("knx: TUNNELLING_ACK structure length %d, want 4", b[0])
	}
	t.Channel = b[1]
	t.Seq = b[2]
	t.Status = b[3]
	return t, nil
}

// ConnectionStateRequest 是 CONNECTIONSTATE_REQUEST 的载荷。
type ConnectionStateRequest struct {
	Channel uint8
	Control HPAI
}

// Encode 组装载荷。
func (c ConnectionStateRequest) Encode() []byte {
	out := []byte{c.Channel, 0x00}
	return AppendHPAI(out, c.Control)
}

// ParseConnectionStateRequest 解析 CONNECTIONSTATE_REQUEST。
func ParseConnectionStateRequest(b []byte) (ConnectionStateRequest, error) {
	var c ConnectionStateRequest
	if len(b) < 2 {
		return c, errors.New("knx: CONNECTIONSTATE_REQUEST too short")
	}
	c.Channel = b[0]
	ctl, _, err := ParseHPAI(b[2:])
	if err != nil {
		return c, fmt.Errorf("knx: CONNECTIONSTATE_REQUEST control endpoint: %w", err)
	}
	c.Control = ctl
	return c, nil
}

// ConnectionStateResponse 是 CONNECTIONSTATE_RESPONSE 的载荷。
type ConnectionStateResponse struct {
	Channel uint8
	Status  uint8
}

// Encode 组装载荷。
func (c ConnectionStateResponse) Encode() []byte {
	return []byte{c.Channel, c.Status}
}

// ParseConnectionStateResponse 解析 CONNECTIONSTATE_RESPONSE。
func ParseConnectionStateResponse(b []byte) (ConnectionStateResponse, error) {
	var c ConnectionStateResponse
	if len(b) < 2 {
		return c, errors.New("knx: CONNECTIONSTATE_RESPONSE too short")
	}
	c.Channel = b[0]
	c.Status = b[1]
	return c, nil
}

// DisconnectRequest 是 DISCONNECT_REQUEST 的载荷。
type DisconnectRequest struct {
	Channel uint8
	Control HPAI
}

// Encode 组装载荷。
func (d DisconnectRequest) Encode() []byte {
	out := []byte{d.Channel, 0x00}
	return AppendHPAI(out, d.Control)
}

// ParseDisconnectRequest 解析 DISCONNECT_REQUEST。
func ParseDisconnectRequest(b []byte) (DisconnectRequest, error) {
	var d DisconnectRequest
	if len(b) < 2 {
		return d, errors.New("knx: DISCONNECT_REQUEST too short")
	}
	d.Channel = b[0]
	ctl, _, err := ParseHPAI(b[2:])
	if err != nil {
		return d, fmt.Errorf("knx: DISCONNECT_REQUEST control endpoint: %w", err)
	}
	d.Control = ctl
	return d, nil
}

// DisconnectResponse 是 DISCONNECT_RESPONSE 的载荷。
type DisconnectResponse struct {
	Channel uint8
	Status  uint8
}

// Encode 组装载荷。
func (d DisconnectResponse) Encode() []byte {
	return []byte{d.Channel, d.Status}
}

// ParseDisconnectResponse 解析 DISCONNECT_RESPONSE。
//
// 与其它响应一样是 2 字节定长，没有结构长度字段：DISCONNECT_RESPONSE 的
// 载荷就是「通道号 + 状态」，多写一个长度字节会让网关把状态当成别的东西。
func ParseDisconnectResponse(b []byte) (DisconnectResponse, error) {
	var d DisconnectResponse
	if len(b) < 2 {
		return d, fmt.Errorf("knx: DISCONNECT_RESPONSE too short (%d bytes)", len(b))
	}
	d.Channel, d.Status = b[0], b[1]
	return d, nil
}
