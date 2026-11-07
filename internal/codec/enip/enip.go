// Package enip 实现 EtherNet/IP 封装层与 CIP 显式报文的纯函数编解码。
//
// 三层结构与 FINS 类似，但每层的「长度」含义各不相同：
//
//	┌─ 封装头（24 字节）──────────────────────────────┐
//	│ 命令(2) 长度(2) 会话句柄(4) 状态(4)              │
//	│ 发送方上下文(8) 选项(4)                          │
//	├─ CPF（通用报文格式）────────────────────────────┤
//	│ 接口句柄(4) 超时(2) 条目数(2) + 条目...          │
//	├─ CIP 报文───────────────────────────────────────┤
//	│ 服务(1) 路径字数(1) 路径(2N) 请求数据            │
//	└─────────────────────────────────────────────────┘
//
// 本包只做编解码，不持有连接、不读时钟。会话建立、请求配对与重试在
// driver 层；内置从站复用同一份 CPF/CIP 代码，避免主站与从站对同一份
// 报文产生两种理解。
package enip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"uic/internal/driver"
)

// DefaultPort 是 EtherNet/IP 显式报文的默认 TCP 端口。
const DefaultPort = 44818

// HeaderLen 是封装头长度。
const HeaderLen = 24

// MaxPayload 是封装层 length 字段能表达的上限。
const MaxPayload = 65535

// 封装命令。
const (
	CmdNOP             uint16 = 0x0000
	CmdListServices    uint16 = 0x0004
	CmdListIdentity    uint16 = 0x0063
	CmdListInterfaces  uint16 = 0x0064
	CmdRegisterSession uint16 = 0x0065
	CmdUnregister      uint16 = 0x0066
	CmdSendRRData      uint16 = 0x006F
	CmdSendUnitData    uint16 = 0x0070
)

// CPF 条目类型。
const (
	ItemNullAddress       uint16 = 0x0000
	ItemConnectedAddress  uint16 = 0x00A1
	ItemConnectedData     uint16 = 0x00B1
	ItemUnconnectedData   uint16 = 0x00B2
	ItemListIdentityReply uint16 = 0x000C
	ItemSequencedAddress  uint16 = 0x8002
)

// CIP 服务码（只列本平台用到的）。
const (
	ServiceMultiple   byte = 0x0A
	ServiceGetAttrAll byte = 0x01
	ServiceGetAttr    byte = 0x0E
	ServiceSetAttr    byte = 0x10
	ServiceReadTag    byte = 0x4C
	ServiceWriteTag   byte = 0x4D
	ServiceReadFrag   byte = 0x52
	ServiceWriteFrag  byte = 0x53
	ServiceReplyMask  byte = 0x80
)

// CIP 数据类型码（标准原子类型）。
const (
	TypeBOOL     byte = 0xC1
	TypeSINT     byte = 0xC2
	TypeINT      byte = 0xC3
	TypeDINT     byte = 0xC4
	TypeLINT     byte = 0xC5
	TypeUSINT    byte = 0xC6
	TypeUINT     byte = 0xC7
	TypeUDINT    byte = 0xC8
	TypeULINT    byte = 0xC9
	TypeREAL     byte = 0xCA
	TypeLREAL    byte = 0xCB
	TypeSTIME    byte = 0xCC
	TypeDATE     byte = 0xCD
	TypeTOD      byte = 0xCE
	TypeDATETIME byte = 0xCF
	TypeSTRING   byte = 0xD0
	TypeBYTE     byte = 0xD1
	TypeWORD     byte = 0xD2
	TypeDWORD    byte = 0xD3
	TypeLWORD    byte = 0xD4
	TypeSTRING2  byte = 0xD5
	// TypeStruct/TypeArray 只用于识别「这不是原子类型」，解码时显式拒绝。
	TypeStruct byte = 0xA0
	TypeArray  byte = 0xA1
)

// ---------------------------------------------------------
// 封装头
// ---------------------------------------------------------

// Header 是 EtherNet/IP 封装头。
type Header struct {
	Command uint16
	Length  uint16
	Session uint32
	Status  uint32
	Context [8]byte
	Options uint32
}

// ParseHeader 解析 24 字节封装头。
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderLen {
		return Header{}, fmt.Errorf("enip: header needs %d bytes, got %d", HeaderLen, len(b))
	}
	var h Header
	h.Command = binary.LittleEndian.Uint16(b[0:])
	h.Length = binary.LittleEndian.Uint16(b[2:])
	h.Session = binary.LittleEndian.Uint32(b[4:])
	h.Status = binary.LittleEndian.Uint32(b[8:])
	copy(h.Context[:], b[12:20])
	h.Options = binary.LittleEndian.Uint32(b[20:])
	return h, nil
}

// BuildHeader 序列化封装头。
func BuildHeader(h Header) []byte {
	b := make([]byte, HeaderLen)
	binary.LittleEndian.PutUint16(b[0:], h.Command)
	binary.LittleEndian.PutUint16(b[2:], h.Length)
	binary.LittleEndian.PutUint32(b[4:], h.Session)
	binary.LittleEndian.PutUint32(b[8:], h.Status)
	copy(b[12:20], h.Context[:])
	binary.LittleEndian.PutUint32(b[20:], h.Options)
	return b
}

// BuildPacket 生成「封装头 + 负载」。
func BuildPacket(command uint16, session, status uint32, ctx [8]byte, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("enip: payload %d exceeds %d", len(payload), MaxPayload)
	}
	h := Header{Command: command, Length: uint16(len(payload)), Session: session, Status: status, Context: ctx}
	return append(BuildHeader(h), payload...), nil
}

// EncapsulationStatusText 返回封装层状态码的可读文本。
func EncapsulationStatusText(code uint32) string {
	switch code {
	case 0x00:
		return "success"
	case 0x01:
		return "invalid command"
	case 0x02:
		return "insufficient memory"
	case 0x03:
		return "incorrect data"
	case 0x64:
		return "invalid session handle"
	case 0x65:
		return "invalid length"
	case 0x69:
		return "unsupported protocol revision"
	}
	return "unknown encapsulation status"
}

// ---------------------------------------------------------
// CPF
// ---------------------------------------------------------

// Item 是 CPF 的一个条目。
type Item struct {
	Type uint16
	Data []byte
}

// BuildCPF 序列化 CPF 结构。接口句柄固定 0，超时固定 0（显式报文不使用）。
func BuildCPF(items []Item) ([]byte, error) {
	if len(items) > 64 {
		return nil, fmt.Errorf("enip: too many CPF items: %d", len(items))
	}
	out := make([]byte, 8, 8+len(items)*4)
	binary.LittleEndian.PutUint16(out[6:], uint16(len(items)))
	for _, it := range items {
		if len(it.Data) > MaxPayload {
			return nil, fmt.Errorf("enip: CPF item too large: %d", len(it.Data))
		}
		var hdr [4]byte
		binary.LittleEndian.PutUint16(hdr[0:], it.Type)
		binary.LittleEndian.PutUint16(hdr[2:], uint16(len(it.Data)))
		out = append(out, hdr[:]...)
		out = append(out, it.Data...)
		if len(it.Data)%2 == 1 {
			// CPF 要求条目按 16 位边界对齐。少了这个补齐，后面的条目
			// 会从奇地址开始，解析端读到的类型/长度全是错的。
			out = append(out, 0)
		}
	}
	return out, nil
}

// ParseCPF 解析 CPF 结构，返回条目列表。
func ParseCPF(b []byte) ([]Item, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("enip: CPF needs at least 8 bytes, got %d", len(b))
	}
	count := int(binary.LittleEndian.Uint16(b[6:]))
	if count > 64 {
		return nil, fmt.Errorf("enip: too many CPF items: %d", count)
	}
	off := 8
	items := make([]Item, 0, count)
	for i := 0; i < count; i++ {
		if off+4 > len(b) {
			return nil, errors.New("enip: CPF truncated at item header")
		}
		typ := binary.LittleEndian.Uint16(b[off:])
		n := int(binary.LittleEndian.Uint16(b[off+2:]))
		off += 4
		if n < 0 || off+n > len(b) {
			return nil, fmt.Errorf("enip: CPF item %d length %d exceeds payload", i, n)
		}
		data := append([]byte(nil), b[off:off+n]...)
		off += n
		if n%2 == 1 {
			off++
		}
		items = append(items, Item{Type: typ, Data: data})
	}
	return items, nil
}

// BuildSendRRData 生成 SendRRData 的 CPF 负载：空地址条目 + 未连接数据条目。
func BuildSendRRData(cip []byte) ([]byte, error) {
	return BuildCPF([]Item{
		{Type: ItemNullAddress},
		{Type: ItemUnconnectedData, Data: cip},
	})
}

// ParseSendRRData 从 SendRRData 的 CPF 负载里取出 CIP 报文。
//
// 响应通常用「未连接数据」条目；少数设备回「已连接数据」条目。两种都接受，
// 但必须恰好有一个数据条目——多于一个说明报文结构不是我们理解的样子。
func ParseSendRRData(payload []byte) ([]byte, error) {
	items, err := ParseCPF(payload)
	if err != nil {
		return nil, err
	}
	var data []byte
	for _, it := range items {
		switch it.Type {
		case ItemUnconnectedData, ItemConnectedData:
			if data != nil {
				return nil, errors.New("enip: multiple data items in SendRRData")
			}
			data = it.Data
		}
	}
	if data == nil {
		return nil, errors.New("enip: no data item in SendRRData")
	}
	return data, nil
}

// ---------------------------------------------------------
// 会话
// ---------------------------------------------------------

// BuildRegisterSession 生成 RegisterSession 负载：协议版本 + 选项。
func BuildRegisterSession(version uint16) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[0:], version)
	binary.LittleEndian.PutUint16(b[2:], 0)
	return b
}

// ParseRegisterSession 解析 RegisterSession 负载。
func ParseRegisterSession(b []byte) (version, options uint16, err error) {
	if len(b) < 4 {
		return 0, 0, fmt.Errorf("enip: RegisterSession payload needs 4 bytes, got %d", len(b))
	}
	return binary.LittleEndian.Uint16(b[0:]), binary.LittleEndian.Uint16(b[2:]), nil
}

// ---------------------------------------------------------
// CIP 报文
// ---------------------------------------------------------

// Request 是一条 CIP 请求。
type Request struct {
	Service byte
	Path    []byte
	Data    []byte
}

// Reply 是一条 CIP 应答。
type Reply struct {
	Service byte
	Status  byte
	Extra   []byte
	Data    []byte
}

// BuildRequest 序列化 CIP 请求：服务 + 路径字数 + 路径 + 数据。
func BuildRequest(service byte, path, data []byte) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("enip: empty CIP path")
	}
	if len(path)%2 != 0 {
		return nil, fmt.Errorf("enip: CIP path must be 16-bit aligned, got %d bytes", len(path))
	}
	words := len(path) / 2
	if words > 255 {
		return nil, fmt.Errorf("enip: CIP path too long: %d words", words)
	}
	out := make([]byte, 0, 2+len(path)+len(data))
	out = append(out, service, byte(words))
	out = append(out, path...)
	out = append(out, data...)
	return out, nil
}

// ParseRequest 解析 CIP 请求（内置从站使用）。
func ParseRequest(b []byte) (Request, error) {
	if len(b) < 2 {
		return Request{}, errors.New("enip: CIP request truncated")
	}
	words := int(b[1])
	off := 2 + words*2
	if off > len(b) {
		return Request{}, fmt.Errorf("enip: CIP request path %d words exceeds packet", words)
	}
	if off == 2 {
		return Request{}, errors.New("enip: empty CIP path")
	}
	return Request{
		Service: b[0],
		Path:    append([]byte(nil), b[2:off]...),
		Data:    append([]byte(nil), b[off:]...),
	}, nil
}

// BuildReply 序列化 CIP 应答：服务|0x80 + 保留 + 状态 + 附加状态字数 + 附加状态 + 数据。
func BuildReply(service, status byte, extra, data []byte) ([]byte, error) {
	if len(extra)%2 != 0 {
		return nil, fmt.Errorf("enip: additional status must be 16-bit aligned, got %d bytes", len(extra))
	}
	out := make([]byte, 0, 4+len(extra)+len(data))
	out = append(out, service|ServiceReplyMask, 0, status, byte(len(extra)/2))
	out = append(out, extra...)
	out = append(out, data...)
	return out, nil
}

// ParseReply 解析 CIP 应答。
func ParseReply(b []byte) (Reply, error) {
	if len(b) < 4 {
		return Reply{}, errors.New("enip: CIP reply truncated")
	}
	extraWords := int(b[3])
	off := 4 + extraWords*2
	if off > len(b) {
		return Reply{}, fmt.Errorf("enip: CIP reply additional status %d words exceeds packet", extraWords)
	}
	return Reply{
		Service: b[0],
		Status:  b[2],
		Extra:   append([]byte(nil), b[4:off]...),
		Data:    append([]byte(nil), b[off:]...),
	}, nil
}

// StatusError 是 CIP 通用状态错误。
//
// 分类很重要：路径错误说明配置的标签名/长度不对，重试没有意义；资源不足、
// 连接丢失重连后可能自愈。把它交给 driver.KindOf 统一判定，避免每个调用点
// 各写一套 switch。
type StatusError struct {
	Service byte
	Code    byte
	Extra   []byte
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("CIP service 0x%02X failed: status 0x%02X (%s)",
		e.Service, e.Code, StatusText(e.Code))
	if len(e.Extra) > 0 {
		msg += fmt.Sprintf(" extra=% x", e.Extra)
	}
	return msg
}

// Kind 实现 driver.Kinded。
func (e *StatusError) Kind() driver.ErrorKind { return statusKind(e.Code) }

// statusKind 把 CIP 通用状态码映射到平台错误分类。
func statusKind(code byte) driver.ErrorKind {
	switch code {
	case 0x04, 0x05, 0x13, 0x15, 0x16:
		// 路径段错误 / 目标不存在 / 数据不够 / 数据过多 / 对象不存在：
		// 都是配置问题，重试不会变好。
		return driver.ErrAddress
	case 0x08, 0x14:
		return driver.ErrUnsupported
	case 0x0E, 0x0F:
		return driver.ErrPermission
	case 0x01, 0x02, 0x07, 0x0C, 0x10:
		// 连接失败 / 资源不足 / 连接丢失 / 状态冲突 / 设备状态冲突：
		// 链路层面可能自愈。
		return driver.ErrDisconnected
	}
	return driver.ErrProtocol
}

// StatusText 返回 CIP 通用状态码的可读文本。
func StatusText(code byte) string {
	switch code {
	case 0x00:
		return "success"
	case 0x01:
		return "connection failure"
	case 0x02:
		return "resource unavailable"
	case 0x03:
		return "invalid parameter value"
	case 0x04:
		return "path segment error"
	case 0x05:
		return "path destination unknown"
	case 0x06:
		return "partial transfer"
	case 0x07:
		return "connection lost"
	case 0x08:
		return "service not supported"
	case 0x09:
		return "invalid attribute value"
	case 0x0A:
		return "attribute list error"
	case 0x0B:
		return "already in requested mode"
	case 0x0C:
		return "object state conflict"
	case 0x0D:
		return "object already exists"
	case 0x0E:
		return "attribute not settable"
	case 0x0F:
		return "privilege violation"
	case 0x10:
		return "device state conflict"
	case 0x11:
		return "reply data too large"
	case 0x12:
		return "fragmentation of a primitive value"
	case 0x13:
		return "not enough data"
	case 0x14:
		return "attribute not supported"
	case 0x15:
		return "too much data"
	case 0x16:
		return "object does not exist"
	case 0x17:
		return "service fragmentation sequence not in progress"
	case 0x18:
		return "no stored attribute data"
	case 0x19:
		return "store operation failure"
	case 0x1A:
		return "routing failure: request packet too large"
	case 0x1B:
		return "routing failure: response packet too large"
	case 0x1C:
		return "missing attribute list entry data"
	case 0x1D:
		return "invalid attribute value list"
	case 0x1E:
		return "embedded service error"
	case 0x1F:
		return "vendor specific error"
	case 0x20:
		return "invalid parameter"
	case 0x21:
		return "write-once value already written"
	case 0x22:
		return "invalid reply received"
	case 0x23:
		return "buffer overflow"
	case 0x24:
		return "invalid message size"
	case 0x25:
		return "key segment error"
	case 0x26:
		return "invalid I/O connection"
	case 0x27:
		return "cannot execute requested service"
	}
	return "unknown CIP status"
}

// ---------------------------------------------------------
// EPATH
// ---------------------------------------------------------

// SymbolicPath 生成「ANSI 扩展符号段」路径：0x91 + 长度 + 字符 + 偶对齐。
//
// 符号标签访问全部走这一段。路径里不再拼接数组下标或成员：CIP 的
// Read Tag 没有偏移参数，元素偏移由请求里的 element count 与驱动侧的
// 切片完成，避免猜错 Member/Element 段的编码。
func SymbolicPath(tag string) ([]byte, error) {
	if tag == "" {
		return nil, errors.New("enip: empty tag name")
	}
	if len(tag) > 255 {
		return nil, fmt.Errorf("enip: tag name %d bytes exceeds 255", len(tag))
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] < 0x20 || tag[i] == 0x7F {
			return nil, fmt.Errorf("enip: tag name contains control character at %d", i)
		}
	}
	out := make([]byte, 2, 2+len(tag)+1)
	out[0] = 0x91
	out[1] = byte(len(tag))
	out = append(out, tag...)
	if len(tag)%2 == 1 {
		out = append(out, 0)
	}
	return out, nil
}

// ParseSymbolicPath 取出符号段里的标签名。
func ParseSymbolicPath(path []byte) (string, error) {
	if len(path) < 2 || path[0] != 0x91 {
		return "", errors.New("enip: path is not a symbolic segment")
	}
	n := int(path[1])
	if 2+n > len(path) {
		return "", errors.New("enip: symbolic segment truncated")
	}
	return string(path[2 : 2+n]), nil
}

// LogicalPath 生成「类 + 实例」路径。
//
// 不使用属性段（Attribute Segment）：本驱动只做符号标签访问，对象寻址
// 留给后续版本。属性段的类型码在不同厂商文档里有 0x2C/0x30 两种说法，
// 没有真机验证前不猜——猜错会读到别的对象的属性，比报错危险得多。
func LogicalPath(class, instance int) ([]byte, error) {
	if class < 0 || class > 0xFFFF || instance < 0 || instance > 0xFFFF {
		return nil, errors.New("enip: class/instance must be 0..65535")
	}
	var out []byte
	add := func(base byte, v int) {
		switch {
		case v <= 0xFF:
			out = append(out, base, byte(v))
		case v <= 0xFFFF:
			out = append(out, base+1, byte(v), byte(v>>8))
		default:
			out = append(out, base+2, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		}
	}
	add(0x20, class)
	add(0x24, instance)
	return out, nil
}

// ---------------------------------------------------------
// 标签服务
// ---------------------------------------------------------

// BuildReadTag 生成 Read Tag 请求（服务 0x4C）。
func BuildReadTag(tag string, count uint16) ([]byte, error) {
	if count == 0 {
		return nil, errors.New("enip: read element count must be >= 1")
	}
	path, err := SymbolicPath(tag)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, count)
	return BuildRequest(ServiceReadTag, path, data)
}

// BuildWriteTag 生成 Write Tag 请求（服务 0x4D）。
func BuildWriteTag(tag string, dataType byte, count uint16, data []byte) ([]byte, error) {
	if count == 0 {
		return nil, errors.New("enip: write element count must be >= 1")
	}
	path, err := SymbolicPath(tag)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 4, 4+len(data))
	binary.LittleEndian.PutUint16(payload[0:], uint16(dataType))
	binary.LittleEndian.PutUint16(payload[2:], count)
	payload = append(payload, data...)
	return BuildRequest(ServiceWriteTag, path, payload)
}

// ParseReadTagReply 解析 Read Tag 应答：类型 + 元素数 + 原始数据。
func ParseReadTagReply(reply Reply) (dataType byte, count uint16, data []byte, err error) {
	if reply.Status != 0 {
		return 0, 0, nil, &StatusError{Service: ServiceReadTag, Code: reply.Status, Extra: reply.Extra}
	}
	if len(reply.Data) < 4 {
		return 0, 0, nil, errors.New("enip: Read Tag reply shorter than type/count header")
	}
	dataType = reply.Data[0]
	count = binary.LittleEndian.Uint16(reply.Data[2:])
	return dataType, count, reply.Data[4:], nil
}

// ParseWriteTagReply 解析 Write Tag 应答：类型 + 元素数。
func ParseWriteTagReply(reply Reply) (dataType byte, count uint16, err error) {
	if reply.Status != 0 {
		return 0, 0, &StatusError{Service: ServiceWriteTag, Code: reply.Status, Extra: reply.Extra}
	}
	if len(reply.Data) < 4 {
		return 0, 0, errors.New("enip: Write Tag reply shorter than type/count header")
	}
	return reply.Data[0], binary.LittleEndian.Uint16(reply.Data[2:]), nil
}

// BuildMultiple 生成 Multiple Service Packet 请求（服务 0x0A）。
//
// 路径固定指向 Message Router（class 0x02 / instance 0x01）。请求数据是
// 「条数 + 每条相对偏移 + 嵌入报文」，偏移相对嵌入区起点（即偏移表之后）。
func BuildMultiple(requests [][]byte) ([]byte, error) {
	if len(requests) == 0 {
		return nil, errors.New("enip: multiple service packet needs at least one request")
	}
	if len(requests) > 32 {
		return nil, fmt.Errorf("enip: too many embedded requests: %d", len(requests))
	}
	path, err := LogicalPath(0x02, 0x01)
	if err != nil {
		return nil, err
	}
	bodyLen := 2 + 2*len(requests)
	for _, r := range requests {
		bodyLen += len(r)
	}
	body := make([]byte, 2+2*len(requests), bodyLen)
	binary.LittleEndian.PutUint16(body, uint16(len(requests)))
	off := 0
	for i, r := range requests {
		if off > 0xFFFF {
			return nil, errors.New("enip: embedded request offset overflow")
		}
		binary.LittleEndian.PutUint16(body[2+2*i:], uint16(off))
		off += len(r)
	}
	for _, r := range requests {
		body = append(body, r...)
	}
	return BuildRequest(ServiceMultiple, path, body)
}

// ParseMultipleReply 解析 Multiple Service Packet 应答。
func ParseMultipleReply(reply Reply) ([][]byte, error) {
	if reply.Status != 0 {
		return nil, &StatusError{Service: ServiceMultiple, Code: reply.Status, Extra: reply.Extra}
	}
	if len(reply.Data) < 2 {
		return nil, errors.New("enip: multiple service reply truncated")
	}
	count := int(binary.LittleEndian.Uint16(reply.Data))
	if count == 0 || count > 32 {
		return nil, fmt.Errorf("enip: invalid embedded reply count %d", count)
	}
	tableEnd := 2 + 2*count
	if tableEnd > len(reply.Data) {
		return nil, errors.New("enip: multiple service offset table truncated")
	}
	base := tableEnd
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		start := int(binary.LittleEndian.Uint16(reply.Data[2+2*i:]))
		if start < 0 || base+start > len(reply.Data) {
			return nil, fmt.Errorf("enip: embedded reply %d offset %d out of range", i, start)
		}
		end := len(reply.Data)
		if i+1 < count {
			next := int(binary.LittleEndian.Uint16(reply.Data[2+2*(i+1):]))
			if next < start || base+next > len(reply.Data) {
				return nil, fmt.Errorf("enip: embedded reply offsets not monotonic at %d", i)
			}
			end = base + next
		}
		out = append(out, append([]byte(nil), reply.Data[base+start:end]...))
	}
	return out, nil
}

// ---------------------------------------------------------
// 类型与字符串
// ---------------------------------------------------------

// TypeSize 返回固定宽度类型的字节数；变长类型返回 -1。
func TypeSize(t byte) int {
	switch t {
	case TypeBOOL, TypeSINT, TypeUSINT, TypeBYTE:
		return 1
	case TypeINT, TypeUINT, TypeWORD:
		return 2
	case TypeDINT, TypeUDINT, TypeREAL, TypeDWORD:
		return 4
	case TypeLINT, TypeULINT, TypeLREAL, TypeLWORD:
		return 8
	}
	return -1
}

// TypeText 返回类型码的可读名。
func TypeText(t byte) string {
	switch t {
	case TypeBOOL:
		return "BOOL"
	case TypeSINT:
		return "SINT"
	case TypeINT:
		return "INT"
	case TypeDINT:
		return "DINT"
	case TypeLINT:
		return "LINT"
	case TypeUSINT:
		return "USINT"
	case TypeUINT:
		return "UINT"
	case TypeUDINT:
		return "UDINT"
	case TypeULINT:
		return "ULINT"
	case TypeREAL:
		return "REAL"
	case TypeLREAL:
		return "LREAL"
	case TypeSTRING:
		return "STRING"
	case TypeBYTE:
		return "BYTE"
	case TypeWORD:
		return "WORD"
	case TypeDWORD:
		return "DWORD"
	case TypeLWORD:
		return "LWORD"
	case TypeStruct:
		return "STRUCT"
	case TypeArray:
		return "ARRAY"
	}
	return fmt.Sprintf("0x%02X", t)
}

// ParseTypeName 把配置里的类型名映射成 CIP 类型码（大小写不敏感）。
func ParseTypeName(s string) (byte, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bool", "boolean":
		return TypeBOOL, true
	case "sint", "int8":
		return TypeSINT, true
	case "int", "int16":
		return TypeINT, true
	case "dint", "int32":
		return TypeDINT, true
	case "lint", "int64":
		return TypeLINT, true
	case "usint", "uint8":
		return TypeUSINT, true
	case "uint", "uint16":
		return TypeUINT, true
	case "udint", "uint32":
		return TypeUDINT, true
	case "ulint", "uint64":
		return TypeULINT, true
	case "real", "float32":
		return TypeREAL, true
	case "lreal", "float64":
		return TypeLREAL, true
	case "string":
		return TypeSTRING, true
	case "byte", "bytes":
		return TypeSINT, true
	}
	return 0, false
}

// EncodeString 生成 STRING 的线上表示：4 字节小端长度 + 字符 + 补零。
//
// capacity 是字符容量（不含 4 字节长度）。常见实现的默认容量是 82 字符，
// 总长 86 字节；调用方必须显式给出容量，不猜。
func EncodeString(s string, capacity int) ([]byte, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("enip: string capacity must be >= 1, got %d", capacity)
	}
	raw := []byte(s)
	n := len(raw)
	if n > capacity {
		n = capacity
	}
	out := make([]byte, 4+capacity)
	binary.LittleEndian.PutUint32(out, uint32(n))
	copy(out[4:], raw[:n])
	return out, nil
}

// DecodeString 解析 STRING：4 字节小端长度 + 字符。
func DecodeString(b []byte) (string, error) {
	if len(b) < 4 {
		return "", errors.New("enip: STRING shorter than length header")
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n < 0 || n > len(b)-4 {
		n = len(b) - 4
	}
	return strings.TrimRight(string(b[4:4+n]), "\x00"), nil
}
