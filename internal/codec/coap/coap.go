// Package coap 实现 CoAP（RFC 7252）的纯函数编解码：报文头、选项的
// delta/length 扩展编码、请求/响应码、Block1/Block2、Observe 选项值，
// 以及资源发现用的 link-format 解析。
//
// 刻意不做的事：
//
//   - **不做传输**。重传、超时、消息 ID 配对、去重都在驱动里，因为它们
//     依赖时钟与套接字状态，而这一层必须保持「同一份输入永远得到同一份
//     输出」，否则金帧测试与从站/主站互测都会变成偶发。
//   - **不做 DTLS**。加密握手是独立子系统；本包只处理明文 CoAP。
//   - **不解释载荷**。Content-Format 只是一个整数，怎么把 CBOR/JSON/纯
//     文本变成平台值由驱动决定。
//
// 选项编码是 CoAP 最容易写错的地方：选项号与前一个选项号做差（delta），
// 长度也做压缩；13/14 是「再跟一个字节/两个字节」的转义，15 是保留值。
// 把 delta 当成绝对选项号，或者把 14 的附加值算成 `value-13`，都会让
// 解析器在第二个选项处开始读垃圾。本包的测试对每个分支都钉了金字节。
package coap

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Version1 是当前唯一的 CoAP 版本。
const Version1 = 1

// Type 是报文类型。
type Type uint8

const (
	TypeConfirmable    Type = 0 // CON：需要 ACK
	TypeNonConfirmable Type = 1 // NON：不需要 ACK
	TypeAck            Type = 2 // ACK：确认一条 CON
	TypeReset          Type = 3 // RST：拒绝一条 CON/NON
)

func (t Type) String() string {
	switch t {
	case TypeConfirmable:
		return "CON"
	case TypeNonConfirmable:
		return "NON"
	case TypeAck:
		return "ACK"
	case TypeReset:
		return "RST"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Code 是「类.明细」编码：高 3 位是类，低 5 位是明细。
type Code uint8

// 请求方法。
const (
	CodeEmpty  Code = 0x00 // 0.00：仅用于 ACK/RST 或 ping
	CodeGET    Code = 0x01 // 0.01
	CodePOST   Code = 0x02 // 0.02
	CodePUT    Code = 0x03 // 0.03
	CodeDELETE Code = 0x04 // 0.04
)

// 2.xx 成功响应。
const (
	CodeCreated  Code = 0x41 // 2.01
	CodeDeleted  Code = 0x42 // 2.02
	CodeValid    Code = 0x43 // 2.03
	CodeChanged  Code = 0x44 // 2.04
	CodeContent  Code = 0x45 // 2.05
	CodeContinue Code = 0x5F // 2.31（Block1 中间响应）
)

// 4.xx 客户端错误。
const (
	CodeBadRequest            Code = 0x80 // 4.00
	CodeUnauthorized          Code = 0x81 // 4.01
	CodeBadOption             Code = 0x82 // 4.02
	CodeForbidden             Code = 0x83 // 4.03
	CodeNotFound              Code = 0x84 // 4.04
	CodeMethodNotAllowed      Code = 0x85 // 4.05
	CodeNotAcceptable         Code = 0x86 // 4.06
	CodePreconditionFailed    Code = 0x8C // 4.12
	CodeRequestEntityTooLarge Code = 0x8D // 4.13
	CodeUnsupportedFormat     Code = 0x8F // 4.15
)

// 5.xx 服务端错误。
const (
	CodeInternalError  Code = 0xA0 // 5.00
	CodeNotImplemented Code = 0xA1 // 5.01
	CodeBadGateway     Code = 0xA2 // 5.02
	CodeUnavailable    Code = 0xA3 // 5.03
	CodeGatewayTimeout Code = 0xA4 // 5.04
	CodeProxyingNotSup Code = 0xA5 // 5.05
)

// Class 返回 0..7 的类。
func (c Code) Class() uint8 { return uint8(c) >> 5 }

// Detail 返回低 5 位。
func (c Code) Detail() uint8 { return uint8(c) & 0x1F }

// String 输出 "0.01 GET" 这种便于日志阅读的形式。
func (c Code) String() string {
	return fmt.Sprintf("%d.%02d %s", c.Class(), c.Detail(), CodeName(c))
}

// IsRequest 表示这是一个请求方法。
func (c Code) IsRequest() bool { return c >= CodeGET && c <= CodeDELETE }

// IsResponse 表示这是一个 2.xx..5.xx 响应。
func (c Code) IsResponse() bool { return c.Class() >= 2 }

// Success 表示 2.xx。
func (c Code) Success() bool { return c.Class() == 2 }

// CodeName 返回常见码的名字；未知码返回空串。
func CodeName(c Code) string {
	switch c {
	case CodeEmpty:
		return "EMPTY"
	case CodeGET:
		return "GET"
	case CodePOST:
		return "POST"
	case CodePUT:
		return "PUT"
	case CodeDELETE:
		return "DELETE"
	case CodeCreated:
		return "Created"
	case CodeDeleted:
		return "Deleted"
	case CodeValid:
		return "Valid"
	case CodeChanged:
		return "Changed"
	case CodeContent:
		return "Content"
	case CodeContinue:
		return "Continue"
	case CodeBadRequest:
		return "Bad Request"
	case CodeUnauthorized:
		return "Unauthorized"
	case CodeBadOption:
		return "Bad Option"
	case CodeForbidden:
		return "Forbidden"
	case CodeNotFound:
		return "Not Found"
	case CodeMethodNotAllowed:
		return "Method Not Allowed"
	case CodeNotAcceptable:
		return "Not Acceptable"
	case CodePreconditionFailed:
		return "Precondition Failed"
	case CodeRequestEntityTooLarge:
		return "Request Entity Too Large"
	case CodeUnsupportedFormat:
		return "Unsupported Content-Format"
	case CodeInternalError:
		return "Internal Server Error"
	case CodeNotImplemented:
		return "Not Implemented"
	case CodeBadGateway:
		return "Bad Gateway"
	case CodeUnavailable:
		return "Service Unavailable"
	case CodeGatewayTimeout:
		return "Gateway Timeout"
	case CodeProxyingNotSup:
		return "Proxying Not Supported"
	}
	return ""
}

// 选项编号。只列出本实现会用到或需要识别的那些。
const (
	OptIfMatch       uint16 = 1
	OptURIHost       uint16 = 3
	OptETag          uint16 = 4
	OptIfNoneMatch   uint16 = 5
	OptObserve       uint16 = 6
	OptURIPort       uint16 = 7
	OptLocationPath  uint16 = 8
	OptURIPath       uint16 = 11
	OptContentFormat uint16 = 12
	OptMaxAge        uint16 = 14
	OptURIQuery      uint16 = 15
	OptAccept        uint16 = 17
	OptLocationQuery uint16 = 20
	OptBlock2        uint16 = 23
	OptBlock1        uint16 = 27
	OptSize2         uint16 = 28
	OptProxyURI      uint16 = 35
	OptProxyScheme   uint16 = 39
	OptSize1         uint16 = 60
)

// 常用 Content-Format。
const (
	FormatTextPlain uint16 = 0
	FormatJSON      uint16 = 50
	FormatCBOR      uint16 = 60
	FormatOctets    uint16 = 42
	FormatLink      uint16 = 40 // application/link-format
)

// ContentFormatName 返回常见内容格式的名字。
func ContentFormatName(v uint16) string {
	switch v {
	case FormatTextPlain:
		return "text/plain"
	case FormatJSON:
		return "application/json"
	case FormatCBOR:
		return "application/cbor"
	case FormatOctets:
		return "application/octet-stream"
	case FormatLink:
		return "application/link-format"
	}
	return fmt.Sprintf("content-format(%d)", v)
}

// Option 是一个 CoAP 选项。Value 的语义由选项号决定：整数选项是
// 最短大端编码（0 编码成零长度），字符串选项是原始字节。
type Option struct {
	Number uint16
	Value  []byte
}

// Message 是一条 CoAP 报文。
type Message struct {
	Type    Type
	Code    Code
	MID     uint16
	Token   []byte
	Options []Option
	Payload []byte
}

// Get 返回某个选项的第一份值。
func (m Message) Get(num uint16) ([]byte, bool) {
	for _, o := range m.Options {
		if o.Number == num {
			return o.Value, true
		}
	}
	return nil, false
}

// GetAll 返回某个选项的全部值（Uri-Path / Uri-Query 会重复出现）。
func (m Message) GetAll(num uint16) [][]byte {
	var out [][]byte
	for _, o := range m.Options {
		if o.Number == num {
			out = append(out, o.Value)
		}
	}
	return out
}

// SetOption 追加一个选项。选项顺序在编码时统一排序，调用方不需要关心。
func (m *Message) SetOption(num uint16, v []byte) {
	m.Options = append(m.Options, Option{Number: num, Value: append([]byte(nil), v...)})
}

// SetUintOption 追加一个整数选项。
func (m *Message) SetUintOption(num uint16, v uint32) {
	m.Options = append(m.Options, Option{Number: num, Value: EncodeUint(v)})
}

// UintOption 读取整数选项。
func (m Message) UintOption(num uint16) (uint32, bool) {
	v, ok := m.Get(num)
	if !ok {
		return 0, false
	}
	return DecodeUint(v), true
}

// Path 返回 Uri-Path 段拼成的路径，带前导 `/`。
func (m Message) Path() string {
	segs := m.GetAll(OptURIPath)
	if len(segs) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		b.Write(s)
	}
	return b.String()
}

// Query 返回 Uri-Query 段用 `&` 拼成的查询串（不含 `?`）。
func (m Message) Query() string {
	segs := m.GetAll(OptURIQuery)
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, "&")
}

// SetPath 把 `/a/b/c` 拆成多个 Uri-Path 选项。空段被跳过。
func (m *Message) SetPath(path string) {
	for _, s := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if s == "" {
			continue
		}
		m.SetOption(OptURIPath, []byte(s))
	}
}

// SetQuery 把 `a=1&b=2` 拆成多个 Uri-Query 选项。空段被跳过。
func (m *Message) SetQuery(query string) {
	for _, s := range strings.Split(query, "&") {
		if s == "" {
			continue
		}
		m.SetOption(OptURIQuery, []byte(s))
	}
}

// EncodeUint 把整数编码成最短大端字节。
//
// CoAP 的整数选项（Content-Format、Max-Age、Observe、Block 等）规定「最短
// 形式」：值 0 编码成零长度，不做「补 0x00」。某些设备会接受非最短形式，
// 但编码端一旦补零，对端按差值解出的语义就可能整体偏移，所以这里统一规范化。
func EncodeUint(v uint32) []byte {
	if v == 0 {
		return nil
	}
	var b [4]byte
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
	start := 0
	for start < 3 && b[start] == 0 {
		start++
	}
	return append([]byte(nil), b[start:]...)
}

// DecodeUint 解析大端整数。
//
// 长度上限由调用方校验：Block 选项最多 3 字节，其它常见整数选项 ≤ 4 字节。
// 这里对超长输入不报错而是继续左移，是因为它只可能出现在畸形报文里，而
// 报文长度已经在 Decode 里被逐字段校验过；即便如此，调用方仍应先看长度。
func DecodeUint(b []byte) uint32 {
	var v uint32
	for _, c := range b {
		v = v<<8 | uint32(c)
	}
	return v
}

// maxOptionValue 是选项长度编码能表达的上限（14 + 65535）。
const maxOptionValue = 14 + 0xFFFF

// Encode 把报文编码成字节串。
//
// 选项按选项号升序排列（同一选项号保持插入顺序）——CoAP 要求严格非降序，
// 乱序的选项会让对端直接 4.02。调用方不需要自己排序。
func Encode(m Message) ([]byte, error) {
	if len(m.Token) > 8 {
		return nil, fmt.Errorf("coap: token length %d exceeds 8", len(m.Token))
	}
	if m.Code == CodeEmpty && (len(m.Payload) > 0 || len(m.Options) > 0) {
		return nil, errors.New("coap: empty code cannot carry options or payload")
	}
	out := make([]byte, 0, 4+len(m.Token)+len(m.Options)*4+len(m.Payload))
	out = append(out, byte(Version1<<6)|byte(m.Type<<4)|byte(len(m.Token)))
	out = append(out, byte(m.Code))
	out = append(out, byte(m.MID>>8), byte(m.MID))
	out = append(out, m.Token...)

	opts := make([]Option, len(m.Options))
	copy(opts, m.Options)
	sort.SliceStable(opts, func(i, j int) bool { return opts[i].Number < opts[j].Number })

	prev := uint16(0)
	for _, o := range opts {
		if len(o.Value) > maxOptionValue {
			return nil, fmt.Errorf("coap: option %d value too long (%d)", o.Number, len(o.Value))
		}
		dn, d := encodeOptionPart(uint32(o.Number - prev))
		ln, l := encodeOptionPart(uint32(len(o.Value)))
		out = append(out, byte(dn<<4|ln))
		out = append(out, d...)
		out = append(out, l...)
		out = append(out, o.Value...)
		prev = o.Number
	}

	if len(m.Payload) > 0 {
		out = append(out, 0xFF)
		out = append(out, m.Payload...)
	}
	return out, nil
}

// encodeOptionPart 把 delta/length 编码成 4 位头 + 0..2 字节扩展。
func encodeOptionPart(v uint32) (byte, []byte) {
	switch {
	case v < 13:
		return byte(v), nil
	case v < 269:
		return 13, []byte{byte(v - 13)}
	default:
		x := v - 269
		return 14, []byte{byte(x >> 8), byte(x)}
	}
}

// Decode 解析一条完整报文。多一个字节、少一个字节都判错——UDP 的报文
// 边界由内核保证，这里再做一次长度校验能把「对端把两条报文粘在一起」
// 这类不可能事件变成明确错误，而不是悄悄截断。
func Decode(b []byte) (Message, error) {
	var m Message
	if len(b) < 4 {
		return m, fmt.Errorf("coap: message too short (%d bytes)", len(b))
	}
	ver := b[0] >> 6
	if ver != Version1 {
		return m, fmt.Errorf("coap: unsupported version %d", ver)
	}
	m.Type = Type(b[0]>>4) & 0x03
	tkl := int(b[0] & 0x0F)
	if tkl > 8 {
		return m, fmt.Errorf("coap: token length %d exceeds 8", tkl)
	}
	m.Code = Code(b[1])
	m.MID = uint16(b[2])<<8 | uint16(b[3])
	pos := 4
	if pos+tkl > len(b) {
		return m, errors.New("coap: token truncated")
	}
	m.Token = append([]byte(nil), b[pos:pos+tkl]...)
	pos += tkl

	prev := uint16(0)
	for pos < len(b) {
		if b[pos] == 0xFF {
			pos++
			if pos == len(b) {
				return m, errors.New("coap: payload marker without payload")
			}
			m.Payload = append([]byte(nil), b[pos:]...)
			return m, nil
		}
		head := b[pos]
		pos++
		delta, next, err := decodeExt(head>>4, b, pos)
		if err != nil {
			return m, err
		}
		pos = next
		length, next, err := decodeExt(head&0x0F, b, pos)
		if err != nil {
			return m, err
		}
		pos = next
		num := int(prev) + int(delta)
		if num > 0xFFFF {
			return m, fmt.Errorf("coap: option number %d exceeds 65535", num)
		}
		if pos+int(length) > len(b) {
			return m, fmt.Errorf("coap: option %d value truncated", num)
		}
		m.Options = append(m.Options, Option{
			Number: uint16(num),
			Value:  append([]byte(nil), b[pos:pos+int(length)]...),
		})
		pos += int(length)
		prev = uint16(num)
	}
	return m, nil
}

// decodeExt 解出一个选项头的 4 位字段，并消费它的扩展字节。
//
// **扩展字节的顺序是「先 delta 后 length」**：一个选项头里的两个 4 位字段
// 各自可能带 0/1/2 个扩展字节，它们不是一个整体，而是按 delta、length
// 依次排列，最后才是选项值。把两个扩展字节当成同一块读，会在「delta 与
// length 都 ≥ 13」的选项上把 delta 算成 length 的一部分（或反之），
// 从这条选项开始后面全部错位。
func decodeExt(nib byte, b []byte, pos int) (uint32, int, error) {
	switch {
	case nib < 13:
		return uint32(nib), pos, nil
	case nib == 13:
		if pos+1 > len(b) {
			return 0, pos, errors.New("coap: option delta/length extension truncated")
		}
		return 13 + uint32(b[pos]), pos + 1, nil
	case nib == 14:
		if pos+2 > len(b) {
			return 0, pos, errors.New("coap: option delta/length extension truncated")
		}
		return 269 + uint32(b[pos])<<8 + uint32(b[pos+1]), pos + 2, nil
	}
	return 0, pos, errors.New("coap: option nibble 15 is reserved")
}

// Block 是 Block1/Block2 选项的结构。
type Block struct {
	Num  uint32 // 块号
	More bool   // M
	Size uint32 // SZX 解码后的字节数（16..1024）
}

// DecodeBlock 解析 Block 选项值。
func DecodeBlock(v []byte) (Block, error) {
	if len(v) > 3 {
		return Block{}, fmt.Errorf("coap: block option length %d exceeds 3", len(v))
	}
	raw := DecodeUint(v)
	szx := raw & 0x07
	if szx > 6 {
		return Block{}, fmt.Errorf("coap: block SZX %d exceeds 6 (1024)", szx)
	}
	return Block{
		Num:  raw >> 4,
		More: raw&0x08 != 0,
		Size: 1 << (szx + 4),
	}, nil
}

// EncodeBlock 编码 Block 选项值；Size 必须是 16..1024 的 2 的幂。
func EncodeBlock(b Block) ([]byte, error) {
	szx := uint32(0)
	for szx <= 6 && (1<<(szx+4)) != b.Size {
		szx++
	}
	if szx > 6 {
		return nil, fmt.Errorf("coap: block size %d is not 16..1024 power of two", b.Size)
	}
	v := b.Num<<4 | szx
	if b.More {
		v |= 0x08
	}
	return EncodeUint(v), nil
}

// SplitURI 把 coap://host[:port]/path?query 拆开。允许省略 scheme。
func SplitURI(raw string) (hostport, path, query string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", "", errors.New("coap: empty uri")
	}
	if strings.Contains(s, "://") {
		scheme, rest, ok := strings.Cut(s, "://")
		if !ok || !strings.EqualFold(scheme, "coap") {
			return "", "", "", fmt.Errorf("coap: unsupported scheme %q", scheme)
		}
		s = rest
	}
	// 去掉 fragment；CoAP 的 Uri 不使用 fragment。
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		query = s[i+1:]
		s = s[:i]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		path = s[i:]
		s = s[:i]
	} else {
		path = "/"
	}
	hostport = s
	if hostport == "" {
		return "", "", "", errors.New("coap: uri has no host")
	}
	// 带冒号但没端口（IPv6 字面量）时直接给 net.SplitHostPort 判断，
	// 失败就补默认端口。
	if _, _, e := net.SplitHostPort(hostport); e != nil {
		if strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[") {
			return "", "", "", fmt.Errorf("coap: bad host %q", hostport)
		}
		hostport = net.JoinHostPort(strings.Trim(hostport, "[]"), "5683")
	}
	return hostport, path, query, nil
}

// Link 是 link-format（RFC 6690）里的一条资源描述。
type Link struct {
	Path   string            // 带前导 `/`
	Params map[string]string // 无值参数的值是空串
}

// ParseLinkFormat 解析 `</a>;rt="temp";if="sensor",</b>` 这种格式。
//
// 只做资源发现需要的那部分：URI-Reference 与 `;name=value` 参数。嵌套的
// 引号转义按 RFC 处理；格式错误直接报错，因为发现结果不可信比发现失败
// 更危险——它会让运维以为设备只有那么几个资源。
func ParseLinkFormat(b []byte) ([]Link, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil, nil
	}
	var links []Link
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\r\n,")
		if s == "" {
			break
		}
		if s[0] != '<' {
			return nil, fmt.Errorf("coap: link-format entry does not start with '<': %q", s)
		}
		end := strings.IndexByte(s, '>')
		if end < 0 {
			return nil, errors.New("coap: link-format entry missing '>'")
		}
		l := Link{Path: s[1:end], Params: map[string]string{}}
		s = s[end+1:]
		for strings.HasPrefix(s, ";") {
			s = s[1:]
			eq := strings.IndexAny(s, "=;,")
			if eq < 0 {
				l.Params[strings.TrimSpace(s)] = ""
				s = ""
				break
			}
			name := strings.TrimSpace(s[:eq])
			if s[eq] != '=' {
				l.Params[name] = ""
				s = s[eq:]
				continue
			}
			rest := s[eq+1:]
			var val string
			if strings.HasPrefix(rest, "\"") {
				var err error
				val, rest, err = parseQuoted(rest)
				if err != nil {
					return nil, err
				}
			} else {
				cut := strings.IndexAny(rest, ";,")
				if cut < 0 {
					val, rest = rest, ""
				} else {
					val, rest = rest[:cut], rest[cut:]
				}
				val = strings.TrimSpace(val)
			}
			l.Params[name] = val
			s = rest
		}
		links = append(links, l)
	}
	return links, nil
}

// parseQuoted 解析一个带引号的参数值，返回 (值, 剩余串, err)。
func parseQuoted(s string) (string, string, error) {
	if !strings.HasPrefix(s, "\"") {
		return "", s, errors.New("coap: expected quoted string")
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", "", errors.New("coap: dangling escape in quoted string")
			}
			i++
			b.WriteByte(s[i])
		case '"':
			return b.String(), s[i+1:], nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", errors.New("coap: unterminated quoted string")
}

// DefaultPort 是 CoAP 的注册端口。
const DefaultPort = 5683

// HostPort 把配置里的地址补上默认端口。
func HostPort(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("coap: empty address")
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, nil
	}
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		return "", fmt.Errorf("coap: bad address %q (want host[:port])", addr)
	}
	host := strings.Trim(addr, "[]")
	if host == "" {
		return "", errors.New("coap: empty host")
	}
	return net.JoinHostPort(host, strconv.Itoa(DefaultPort)), nil
}
