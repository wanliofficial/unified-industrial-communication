// Package secscodec 实现半导体设备与工厂之间最常用的两套协议：
//
//   - HSMS（High-Speed SECS Message Services）：TCP 5000 上的消息传输层，
//     只负责「把一条 SECS-II 消息送到对端并取回应答」，不含任何语义；
//   - SECS-II：消息本身。一条消息 = 4 字节消息头（Stream/Function +
//     W 位 + 系统字节）+ 一串数据项（item）。
//
// 包内只做纯函数：无 I/O、无时钟、无可变状态。
//
// 分层与 TCP 上的其它协议一样清楚。SEMI E37 的线上帧是：
//
//	[0..3]   长度（大端 4 字节）= 10 字节头 + 载荷，**不含长度前缀本身**
//	[4..13]  10 字节头：
//	  [4..5]  SessionID（数据消息=设备 ID；控制消息固定 0xFFFF）
//	  [6]     头字节 2：数据消息 = Stream(7 位) | W 位；控制消息 = 0
//	          （Reject.req 例外：这里放被拒消息的 SType）
//	  [7]     头字节 3：数据消息 = Function；Select/Deselect.rsp = 状态码；
//	          Reject.req = 拒绝原因；其余控制消息 = 0
//	  [8]     PType = 0（SECS-II 编码）
//	  [9]     SType = 消息类型（0=DATA、1/2=Select.req/rsp、3/4=Deselect、
//	          5/6=Linktest、7=Reject.req、9=Separate.req）
//	  [10..13] 系统字节（大端）
//	[14..]   载荷（仅 DATA 消息允许携带）
//
// 系统字节（SystemBytes）由发送方选择、接收方原样回显，它是「这一帧属于
// 哪一次请求」的唯一标识——与 ADS 的 invoke、CoAP 的 MID/Token 同类。
// 长度「含头」这一点最容易写错：写成只算载荷，任何标准对端都会把
// 4 字节前缀 + 头错位成另一条消息。
//
// SECS-II 的格式字节最容易写错：它是「6 位格式码 << 2 | 长度字节数」，长度
// 字节数取值 1..3（0 非法），不是「格式码 | 长度字节数」。写成后者时，
// 0x21 会被解成 Binary 而不是 Binary+1 字节长度，所有项都会错位一格。
package secscodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// DefaultPort 是 HSMS/TCP 的默认端口。
const DefaultPort = 5000

// FrameLen 是 HSMS 帧的固定开销：4 字节长度前缀 + 10 字节头 = 14 字节。
// 前缀里的长度值含这 10 字节头（SEMI E37 / Wireshark dissector 的
// 「length + 4 == 整帧长」判定）。
const FrameLen = 14

// headerLen 是长度前缀之后的固定头长。
const headerLen = 10

// MaxPayload 是一条消息数据区的上限（16 MiB）。
//
// 长度字段是 32 位的，理论上允许 4 GiB；读循环若照着它分配缓冲区，一条
// 畸形帧（或对端被噪声篡改的长度）就能让进程直接 OOM。SECS-II 的实际载荷
// 远小于此——最大的常见消息是整批状态值或报告定义，量级在几十 KB。
// 超限按协议错误处理，而不是「再等等看」。
const MaxPayload = 16 << 20

// 消息类型（SEMI E37 的 SType）。8 保留未用、10..127 为伴生标准保留；
// 「未选中」不靠独立消息表达，而是 Select.rsp 里的非零状态码。
const (
	MsgTypeDataMsg     uint8 = 0x00
	MsgTypeSelectReq   uint8 = 0x01
	MsgTypeSelectRsp   uint8 = 0x02
	MsgTypeDeselectReq uint8 = 0x03
	MsgTypeDeselectRsp uint8 = 0x04
	MsgTypeLinktestReq uint8 = 0x05
	MsgTypeLinktestRsp uint8 = 0x06
	MsgTypeRejectReq   uint8 = 0x07
	MsgTypeSeparateReq uint8 = 0x09
)

// HSMS 状态（Select/Deselect/Linktest/Reject 的应答里携带）。
const (
	StatusOK            uint16 = 0x0000
	StatusUnsupported   uint16 = 0x0001
	StatusTooMuch       uint16 = 0x0002
	StatusAlready       uint16 = 0x0003
	StatusLinkNotActive uint16 = 0x0004
	StatusDuplicate     uint16 = 0x0005
	StatusUnknown       uint16 = 0x0006
	StatusIllegal       uint16 = 0x0007
	StatusWait          uint16 = 0x0200
)

// TypeName 返回消息类型的可读名（诊断输出用）。
func TypeName(t uint8) string {
	switch t {
	case MsgTypeDataMsg:
		return "DATA"
	case MsgTypeSelectReq:
		return "SelectReq"
	case MsgTypeSelectRsp:
		return "SelectRsp"
	case MsgTypeDeselectReq:
		return "DeselectReq"
	case MsgTypeDeselectRsp:
		return "DeselectRsp"
	case MsgTypeLinktestReq:
		return "LinktestReq"
	case MsgTypeLinktestRsp:
		return "LinktestRsp"
	case MsgTypeSeparateReq:
		return "SeparateReq"
	case MsgTypeRejectReq:
		return "RejectReq"
	}
	return fmt.Sprintf("Unknown(0x%02x)", t)
}

// StatusName 返回 HSMS 状态码的中文名。
func StatusName(s uint16) string {
	switch s {
	case StatusOK:
		return "成功"
	case StatusUnsupported:
		return "不支持的消息类型"
	case StatusTooMuch:
		return "设备忙（请求被丢弃）"
	case StatusAlready:
		return "已处于该状态"
	case StatusLinkNotActive:
		return "链路未激活"
	case StatusDuplicate:
		return "重复的系统字节"
	case StatusUnknown:
		return "未定义的消息类型"
	case StatusIllegal:
		return "非法消息"
	case StatusWait:
		return "请稍候"
	}
	return fmt.Sprintf("未知状态(0x%04x)", s)
}

// Message 是一个 HSMS 帧：消息头（系统字节等）+ 消息类型 + 数据。
//
// SystemBytes 由发送方选择（通常是自增序号），接收方原样回显——它是
// 配对请求与应答的唯一依据。
type Message struct {
	SystemBytes uint32 // 系统字节：事务配对
	SessionID   uint16 // 数据消息 = 设备 ID；控制消息固定 0xFFFF（Encode 强制）
	Stream      uint8  // 1..127（Reject.req 时是被拒消息的 SType）
	Function    uint8  // 0..255（Select/Deselect.rsp 时是状态码；Reject.req 时是拒绝原因）
	WBit        bool   // 该 Primary 期待一条 Secondary 应答
	MsgType     uint8  // 消息类型（E37 SType）
	RspStatus   uint8  // Select.rsp / Deselect.rsp 在头字节 3 携带的状态码
	Payload     []byte // 仅 DATA 消息允许携带（E37：控制消息无载荷）
}

// IsData 报告这是不是一条数据消息（而非 Select/Deselect/Linktest 等控制消息）。
func (m Message) IsData() bool { return m.MsgType == MsgTypeDataMsg }

// String 给出可读摘要（诊断与日志用）。
func (m Message) String() string {
	if m.IsData() {
		w := ""
		if m.WBit {
			w = " W"
		}
		return fmt.Sprintf("S%dF%d%s sys=%d %dB", m.Stream, m.Function, w, m.SystemBytes, len(m.Payload))
	}
	return fmt.Sprintf("%s sys=%d status=%d(%s)",
		TypeName(m.MsgType), m.SystemBytes, m.Status(), StatusName(m.Status()))
}

// Status 取 Select.rsp / Deselect.rsp 头字节 3 里的状态码。
// 其它消息没有状态字节，返回 StatusUnknown。
func (m Message) Status() uint16 {
	if m.MsgType == MsgTypeSelectRsp || m.MsgType == MsgTypeDeselectRsp {
		return uint16(m.RspStatus)
	}
	return StatusUnknown
}

// ---------------------------------------------------------------------------
// HSMS 帧编解码
// ---------------------------------------------------------------------------

// Encode 把一条消息编成 E37 HSMS 帧（4 字节长度前缀 + 10 字节头 + 载荷）。
//
// 长度前缀 = 10 + len(载荷)——含头。控制消息不携带载荷（E37 如此，
// Wireshark 对「控制消息带数据」直接判坏帧）；控制消息的 SessionID 固定
// 0xFFFF，头字节 2/3 的取值见表头注释。
func Encode(m Message) ([]byte, error) {
	if len(m.Payload) > MaxPayload {
		return nil, fmt.Errorf("secscodec: 数据长度 %d 超过上限 %d", len(m.Payload), MaxPayload)
	}
	if !m.IsData() && len(m.Payload) != 0 {
		return nil, fmt.Errorf("secscodec: 控制消息 %s 不携带载荷", TypeName(m.MsgType))
	}
	if m.IsData() && (m.Stream == 0 || m.Stream > 127) {
		return nil, fmt.Errorf("secscodec: Stream %d 必须在 1..127", m.Stream)
	}
	buf := make([]byte, FrameLen+len(m.Payload))
	// 长度前缀：10 字节头 + 载荷。
	binary.BigEndian.PutUint32(buf[0:4], uint32(headerLen+len(m.Payload)))
	// 头：SessionID。控制消息固定 0xFFFF（Wireshark/SEMI E37 均如此校验）。
	session := m.SessionID
	if !m.IsData() {
		session = 0xFFFF
	}
	binary.BigEndian.PutUint16(buf[4:6], session)
	// 头字节 2/3。
	switch {
	case m.IsData():
		b2 := m.Stream & 0x7f
		if m.WBit {
			b2 |= 0x80
		}
		buf[6] = b2
		buf[7] = m.Function
	case m.MsgType == MsgTypeSelectRsp || m.MsgType == MsgTypeDeselectRsp:
		buf[7] = m.RspStatus // 状态在头字节 3
	case m.MsgType == MsgTypeRejectReq:
		buf[6] = m.Stream   // 被拒消息的 SType
		buf[7] = m.Function // 拒绝原因
	}
	buf[8] = 0         // PType：SECS-II 编码
	buf[9] = m.MsgType // SType
	binary.BigEndian.PutUint32(buf[10:14], m.SystemBytes)
	copy(buf[FrameLen:], m.Payload)
	return buf, nil
}

// Decode 把一段完整帧解成消息（输入长度必须正好 = 4 + 长度前缀）。
func Decode(b []byte) (Message, error) {
	var m Message
	if len(b) < FrameLen {
		return m, fmt.Errorf("secscodec: frame %d bytes, need %d", len(b), FrameLen)
	}
	n := binary.BigEndian.Uint32(b[0:4])
	if int(n) != len(b)-4 {
		return m, fmt.Errorf("secscodec: 长度字段 %d 与实际数据 %d 不一致（长度含 10 字节头）",
			n, len(b)-4)
	}
	payloadLen := int(n) - headerLen
	if payloadLen < 0 || payloadLen > MaxPayload {
		return m, fmt.Errorf("secscodec: 数据长度 %d 超过上限 %d", payloadLen, MaxPayload)
	}
	m.SessionID = binary.BigEndian.Uint16(b[4:6])
	m.MsgType = b[9]
	if b[8] != 0 {
		return m, fmt.Errorf("secscodec: 不支持的 PType %d（本包只解 SECS-II = 0）", b[8])
	}
	if m.IsData() {
		m.Stream = b[6] & 0x7f
		m.WBit = b[6]&0x80 != 0
		m.Function = b[7]
	} else {
		switch m.MsgType {
		case MsgTypeSelectRsp, MsgTypeDeselectRsp:
			m.RspStatus = b[7] // 状态在头字节 3
		case MsgTypeRejectReq:
			m.Stream = b[6]   // 被拒消息的 SType
			m.Function = b[7] // 拒绝原因
		}
	}
	m.SystemBytes = binary.BigEndian.Uint32(b[10:14])
	if payloadLen > 0 {
		m.Payload = make([]byte, payloadLen)
		copy(m.Payload, b[FrameLen:])
	}
	return m, nil
}

// FrameDataLen 从帧头取载荷长度（读循环用它决定还要读多少字节）。
//
// 前缀的长度值含 10 字节头，因此载荷 = 前缀 - headerLen。
func FrameDataLen(b []byte) (uint32, error) {
	if len(b) < FrameLen {
		return 0, fmt.Errorf("secscodec: header %d bytes, need %d", len(b), FrameLen)
	}
	n := binary.BigEndian.Uint32(b[0:4])
	if n < headerLen {
		return 0, fmt.Errorf("secscodec: 长度字段 %d 小于 10 字节头", n)
	}
	plen := n - headerLen
	if plen > MaxPayload {
		return 0, fmt.Errorf("secscodec: 数据长度 %d 超过上限 %d", plen, MaxPayload)
	}
	return plen, nil
}

// ErrTruncated 表示「字节还不够」；读循环据此继续等待而不是报错。
var ErrTruncated = errors.New("secscodec: truncated frame")

// ParseFrame 从一段可能的「半个帧」里解出消息。
//
// 它与 Decode 的分工是：Decode 要求输入正好是一整个帧；ParseFrame 允许
// 输入更长（缓冲区里可能排着下一条），只取第一帧，并在「还不够」时返回
// ErrTruncated。读循环因此可以先 read 一段、再反复调用它。
func ParseFrame(b []byte) (Message, int, error) {
	if len(b) < FrameLen {
		return Message{}, 0, ErrTruncated
	}
	n := binary.BigEndian.Uint32(b[0:4])
	if n < headerLen {
		return Message{}, 0, fmt.Errorf("secscodec: 长度字段 %d 小于 10 字节头", n)
	}
	plen := int(n) - headerLen
	if plen > MaxPayload {
		return Message{}, 0, fmt.Errorf("secscodec: 数据长度 %d 超过上限 %d", plen, MaxPayload)
	}
	total := 4 + int(n)
	if total < 4 {
		return Message{}, 0, fmt.Errorf("secscodec: 长度字段 %d 溢出", n)
	}
	if len(b) < total {
		return Message{}, 0, ErrTruncated
	}
	m, err := Decode(b[:total])
	if err != nil {
		return Message{}, 0, err
	}
	return m, total, nil
}

// ---------------------------------------------------------------------------
// 控制消息构造
// ---------------------------------------------------------------------------

// NewSelectReq 构造一条 Select.req：请求把链路切到「选中」状态，让随后
// 的 DATA 消息开始交换。这是 HSMS 通信的第一步。
func NewSelectReq(sys uint32) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeSelectReq}
}

// NewSelectRsp 构造 Select.rsp：status 为 0 表示选中成功，非零（如 1）
// 表示「未被选中」。E37 把状态放在头字节 3，控制消息不携带载荷。
func NewSelectRsp(sys uint32, status uint16) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeSelectRsp, RspStatus: byte(status)}
}

// NewDeselectReq 构造 Deselect.req：通信结束前请求回到未选中状态。
func NewDeselectReq(sys uint32) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeDeselectReq}
}

// NewDeselectRsp 构造 Deselect.rsp。
func NewDeselectRsp(sys uint32, status uint16) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeDeselectRsp, RspStatus: byte(status)}
}

// NewLinktestReq 构造 Linktest.req：探活。与 Select 不同，Linktest 不改变
// 链路状态，只是问一句「还在吗」，因此可以在选中状态下随时使用。
func NewLinktestReq(sys uint32) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeLinktestReq}
}

// NewLinktestRsp 构造 Linktest.rsp。E37 里 Linktest.rsp 没有状态字段
// （字节 2/3 必须为 0），参数仅为兼容保留、必须传 0。
func NewLinktestRsp(sys uint32, status uint16) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeLinktestRsp}
}

// NewSeparateReq 构造 Separate.req：告诉对端「我要走了」，随后关闭 TCP。
func NewSeparateReq(sys uint32) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeSeparateReq}
}

// NewRejectReq 构造 Reject.req：对端发现一条它无法处理的消息时回这条。
// 被拒消息的 SType 放头字节 2、拒绝原因放头字节 3，载荷为空；SessionID
// 由 Encode 强制为 0xFFFF。
func NewRejectReq(sys uint32, sType, sCode uint8) Message {
	return Message{SystemBytes: sys, MsgType: MsgTypeRejectReq, Stream: sType, Function: sCode}
}

// ---------------------------------------------------------------------------
// DATA 消息
// ---------------------------------------------------------------------------

// DataMessage 是一条 SECS-II DATA 消息。
type DataMessage struct {
	SystemBytes uint32
	Stream      uint8
	Function    uint8
	WBit        bool // 该 Primary 期待一条 Secondary 应答
	Body        []byte
}

// ToMessage 把 DATA 消息包装成 HSMS Message。
func (d DataMessage) ToMessage() Message {
	return Message{
		SystemBytes: d.SystemBytes,
		Stream:      d.Stream,
		Function:    d.Function,
		WBit:        d.WBit,
		MsgType:     MsgTypeDataMsg,
		Payload:     d.Body,
	}
}

// ParseData 从一条 HSMS 消息里取出 SECS-II DATA 消息。
func ParseData(m Message) (DataMessage, error) {
	if !m.IsData() {
		return DataMessage{}, fmt.Errorf("secscodec: 不是 DATA 消息（%s）", TypeName(m.MsgType))
	}
	return DataMessage{
		SystemBytes: m.SystemBytes,
		Stream:      m.Stream,
		Function:    m.Function,
		WBit:        m.WBit,
		Body:        m.Payload,
	}, nil
}

// ---------------------------------------------------------------------------
// SECS-II 数据项
// ---------------------------------------------------------------------------

// Format 是 SECS-II 数据项的 6 位格式码。
//
// 线上格式字节 = byte(Format)<<2 | 长度字节数（1..3）。这一条必须记牢：
// 把 0x21 当成「Binary | 1」而不是「Binary 的 6 位码 8 << 2 | 1」，会让
// 每个项都错位一格，而且前几项「看起来还能解出来」。
type Format uint8

const (
	FormatList    Format = 0  // L：长度字段是子项个数
	FormatBinary  Format = 8  // B：原始字节
	FormatBoolean Format = 9  // BOOLEAN：1 字节，非 0 即 true
	FormatASCII   Format = 16 // A：ASCII 字符串
	FormatJIS     Format = 17 // J：JIS-8 字符串
	FormatI8      Format = 24 // I8：有符号 64 位（大端）
	FormatI1      Format = 25 // I1：有符号 8 位
	FormatI2      Format = 26 // I2：有符号 16 位（大端）
	FormatI4      Format = 28 // I4：有符号 32 位（大端）
	FormatF8      Format = 32 // F8：双精度浮点（大端）
	FormatF4      Format = 36 // F4：单精度浮点（大端）
	FormatU8      Format = 40 // U8：无符号 64 位（大端）
	FormatU1      Format = 41 // U1：无符号 8 位
	FormatU2      Format = 42 // U2：无符号 16 位（大端）
	FormatU4      Format = 44 // U4：无符号 32 位（大端）
)

// FormatName 返回格式码的可读名。
func FormatName(f Format) string {
	switch f {
	case FormatList:
		return "L"
	case FormatBinary:
		return "B"
	case FormatBoolean:
		return "BOOLEAN"
	case FormatASCII:
		return "A"
	case FormatJIS:
		return "J"
	case FormatI8:
		return "I8"
	case FormatI1:
		return "I1"
	case FormatI2:
		return "I2"
	case FormatI4:
		return "I4"
	case FormatF8:
		return "F8"
	case FormatF4:
		return "F4"
	case FormatU8:
		return "U8"
	case FormatU1:
		return "U1"
	case FormatU2:
		return "U2"
	case FormatU4:
		return "U4"
	}
	return fmt.Sprintf("?(%d)", uint8(f))
}

// fixedSize 返回定长格式的单值宽度；变长（B/BOOLEAN/A/J）与 L 返回 0。
func (f Format) fixedSize() int {
	switch f {
	case FormatBoolean, FormatI1, FormatU1:
		return 1
	case FormatI2, FormatU2:
		return 2
	case FormatI4, FormatU4, FormatF4:
		return 4
	case FormatI8, FormatU8, FormatF8:
		return 8
	}
	return 0
}

// Item 是一个 SECS-II 数据项：格式 + 值。
//
// 标量项持有 Raw（大端原始字节）或 Str；List 项持有 Children。取值方法
// 只在格式匹配时返回数据，格式不符时报错而不是悄悄转换——这与文本协议
// 「按声明解码、不猜」是同一条纪律。
type Item struct {
	Format   Format
	Raw      []byte  // 标量的原始字节（大端）
	Str      string  // A/J 项的字符串
	Children []*Item // L 项的子项
}

// List 构造一个 L 项。
func List(items ...*Item) *Item {
	return &Item{Format: FormatList, Children: items}
}

// Binary 构造一个 B 项。
func Binary(b []byte) *Item {
	out := make([]byte, len(b))
	copy(out, b)
	return &Item{Format: FormatBinary, Raw: out}
}

// ASCII 构造一个 A 项。
func ASCII(s string) *Item { return &Item{Format: FormatASCII, Str: s} }

// Bool 构造一个 BOOLEAN 项。
func Bool(v bool) *Item {
	b := byte(0)
	if v {
		b = 1
	}
	return &Item{Format: FormatBoolean, Raw: []byte{b}}
}

// Int 按 SECS-II 的整数格式构造一个标量项。负数按二进制补码截断到目标宽度。
func Int(f Format, v int64) *Item {
	b := make([]byte, f.fixedSize())
	switch f {
	case FormatI1, FormatU1:
		b[0] = byte(v)
	case FormatI2, FormatU2:
		binary.BigEndian.PutUint16(b, uint16(v))
	case FormatI4, FormatU4:
		binary.BigEndian.PutUint32(b, uint32(v))
	case FormatI8, FormatU8:
		binary.BigEndian.PutUint64(b, uint64(v))
	default:
		return &Item{Format: f}
	}
	return &Item{Format: f, Raw: b}
}

// Float 按 F4/F8 构造一个浮点项。
func Float(f Format, v float64) *Item {
	switch f {
	case FormatF4:
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, math.Float32bits(float32(v)))
		return &Item{Format: f, Raw: b}
	case FormatF8:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, math.Float64bits(v))
		return &Item{Format: f, Raw: b}
	}
	return &Item{Format: f}
}

// ---------------------------------------------------------------------------
// 长度字段
// ---------------------------------------------------------------------------

// encodeLen 返回「长度字节数」与长度内容。SECS-II 的长度字段可取 1..3 字节。
func encodeLen(n int) (count byte, body []byte, err error) {
	switch {
	case n < 0:
		return 0, nil, errors.New("secscodec: 负数长度")
	case n < 256:
		return 1, []byte{byte(n)}, nil
	case n < 65536:
		return 2, []byte{byte(n >> 8), byte(n)}, nil
	case n < 16777216:
		return 3, []byte{byte(n >> 16), byte(n >> 8), byte(n)}, nil
	default:
		return 0, nil, errors.New("secscodec: 长度超过 3 字节上限")
	}
}

// decodeLen 解析长度字段。raw 是格式字节低 2 位的原值：
// 00 与 01 都表示 1 字节长度（01 是现在通行的写法，00 是历史写法），
// 10 表示 2 字节，11 表示 3 字节。
//
// 这一处最容易写错：长度字节数写在**格式字节的低 2 位**里，不在长度区里
// 重复出现。把它当成「长度区的第一个字节」，会让每个项都从错误的位置
// 开始算，而且前几个项往往「看起来还能解出来」。
func decodeLen(b []byte, raw int) (n int, err error) {
	count := raw
	if count == 0 {
		count = 1
	}
	switch count {
	case 1:
		if len(b) < 1 {
			return 0, errors.New("secscodec: 长度字段不完整")
		}
		return int(b[0]), nil
	case 2:
		if len(b) < 2 {
			return 0, errors.New("secscodec: 长度字段不完整")
		}
		return int(binary.BigEndian.Uint16(b[:2])), nil
	case 3:
		if len(b) < 3 {
			return 0, errors.New("secscodec: 长度字段不完整")
		}
		return int(b[0])<<16 | int(b[1])<<8 | int(b[2]), nil
	}
	return 0, fmt.Errorf("secscodec: 长度字节数 %d 非法", raw)
}

// EncodeItem 把一个数据项编成 SECS-II 字节（格式字节 + 长度 + 内容）。
//
// 注意 L 项的长度是「子项个数」而不是字节数：先算长度值，再编长度字段，
// 不能拿「拼好后的 body 字节数」去编长度——那会把 L2 写成 L5。
func EncodeItem(it *Item) ([]byte, error) {
	if it == nil {
		return nil, errors.New("secscodec: nil item")
	}
	var length int
	var body []byte
	switch it.Format {
	case FormatList:
		length = len(it.Children)
		for i, c := range it.Children {
			cb, err := EncodeItem(c)
			if err != nil {
				return nil, fmt.Errorf("secscodec: L 项第 %d 个子项: %w", i+1, err)
			}
			body = append(body, cb...)
		}
	case FormatBinary, FormatBoolean:
		length = len(it.Raw)
		body = append([]byte(nil), it.Raw...)
	case FormatASCII, FormatJIS:
		length = len(it.Str)
		body = append([]byte(nil), it.Str...)
	default:
		n := it.Format.fixedSize()
		if n == 0 {
			return nil, fmt.Errorf("secscodec: 未知格式 %d（%s）", uint8(it.Format), FormatName(it.Format))
		}
		length = n
		body = make([]byte, n)
		copy(body, it.Raw)
	}
	count, hdr, err := encodeLen(length)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(hdr)+len(body))
	out = append(out, byte(it.Format)<<2|count)
	out = append(out, hdr...)
	out = append(out, body...)
	return out, nil
}

// DecodeItem 从 b 的开头解出一个数据项，返回项与消费的字节数。
func DecodeItem(b []byte) (*Item, int, error) {
	if len(b) < 1 {
		return nil, 0, errors.New("secscodec: 空的 item")
	}
	format := Format(b[0] >> 2)
	rawCount := int(b[0] & 0x03)
	n, err := decodeLen(b[1:], rawCount)
	if err != nil {
		return nil, 0, err
	}
	off := 1 + rawCount
	if rawCount == 0 {
		off = 2
	}
	if format == FormatList {
		// 每个子项至少占 2 字节（格式字节 + 长度字节）。声明数超过剩余字节
		// 数就一定是畸形长度——按它预分配会让一条 40 字节的畸形帧申请
		// 128 MiB 的指针数组。
		if n > len(b)-off {
			return nil, 0, fmt.Errorf("secscodec: L 项声明 %d 个子项，剩余字节只够更少", n)
		}
		items := make([]*Item, 0, n)
		for i := 0; i < n; i++ {
			if off >= len(b) {
				return nil, 0, fmt.Errorf("secscodec: L 项声明 %d 个子项，第 %d 个缺失", n, i+1)
			}
			child, used, err := DecodeItem(b[off:])
			if err != nil {
				return nil, 0, fmt.Errorf("secscodec: L 项第 %d 个子项: %w", i+1, err)
			}
			items = append(items, child)
			off += used
		}
		return &Item{Format: format, Children: items}, off, nil
	}
	if len(b) < off+n {
		return nil, 0, fmt.Errorf("secscodec: %s 项需要 %d 字节，实际 %d",
			FormatName(format), n, len(b)-off)
	}
	it := &Item{Format: format}
	switch format {
	case FormatASCII, FormatJIS:
		it.Str = string(b[off : off+n])
	default:
		it.Raw = make([]byte, n)
		copy(it.Raw, b[off:off+n])
	}
	return it, off + n, nil
}

// DecodeAll 把一段字节解成若干个顶层数据项（一条消息的 Body 常常是 L 项，
// 但也可能并列多项）。全部消费完才算成功。
func DecodeAll(b []byte) ([]*Item, error) {
	var out []*Item
	for off := 0; off < len(b); {
		it, used, err := DecodeItem(b[off:])
		if err != nil {
			return nil, err
		}
		out = append(out, it)
		off += used
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 取值：只在格式匹配时返回数据，否则报错。
// ---------------------------------------------------------------------------

// ErrFormatMismatch 是格式不符时的哨兵错误。
var ErrFormatMismatch = errors.New("secscodec: 数据项格式不符")

func formatErr(want, got Format) error {
	return fmt.Errorf("%w：需要 %s，实际 %s", ErrFormatMismatch, FormatName(want), FormatName(got))
}

// U1/U2/U4/U8 按各自的格式取无符号整数。
func (it *Item) U1() (uint8, error) {
	v, err := it.uint(FormatU1, 1)
	return uint8(v), err
}

func (it *Item) U2() (uint16, error) {
	v, err := it.uint(FormatU2, 2)
	return uint16(v), err
}

func (it *Item) U4() (uint32, error) {
	v, err := it.uint(FormatU4, 4)
	return uint32(v), err
}

func (it *Item) U8() (uint64, error) { return it.uint(FormatU8, 8) }

// I1/I2/I4/I8 按各自的格式取有符号整数（按符号位扩展）。
func (it *Item) I1() (int8, error) {
	v, err := it.int(FormatI1, 1)
	return int8(v), err
}

func (it *Item) I2() (int16, error) {
	v, err := it.int(FormatI2, 2)
	return int16(v), err
}

func (it *Item) I4() (int32, error) {
	v, err := it.int(FormatI4, 4)
	return int32(v), err
}

func (it *Item) I8() (int64, error) { return it.int(FormatI8, 8) }

func (it *Item) uint(want Format, n int) (uint64, error) {
	if it.Format != want {
		return 0, formatErr(want, it.Format)
	}
	if len(it.Raw) != n {
		return 0, fmt.Errorf("secscodec: %s 项需要 %d 字节，实际 %d", FormatName(want), n, len(it.Raw))
	}
	var v uint64
	for _, b := range it.Raw {
		v = v<<8 | uint64(b)
	}
	return v, nil
}

func (it *Item) int(want Format, n int) (int64, error) {
	v, err := it.uint(want, n)
	if err != nil {
		return 0, err
	}
	if it.Raw[0]&0x80 != 0 {
		v -= 1 << (8 * n)
	}
	return int64(v), nil
}

// F4/F8 按 IEEE-754 单/双精度取值（大端）。
func (it *Item) F4() (float32, error) {
	if it.Format != FormatF4 {
		return 0, formatErr(FormatF4, it.Format)
	}
	if len(it.Raw) != 4 {
		return 0, fmt.Errorf("secscodec: F4 项需要 4 字节，实际 %d", len(it.Raw))
	}
	return math.Float32frombits(binary.BigEndian.Uint32(it.Raw)), nil
}

func (it *Item) F8() (float64, error) {
	if it.Format != FormatF8 {
		return 0, formatErr(FormatF8, it.Format)
	}
	if len(it.Raw) != 8 {
		return 0, fmt.Errorf("secscodec: F8 项需要 8 字节，实际 %d", len(it.Raw))
	}
	return math.Float64frombits(binary.BigEndian.Uint64(it.Raw)), nil
}

// Bool 取布尔项：SECS-II 的布尔是「1 字节，非 0 即 true」。
func (it *Item) Bool() (bool, error) {
	if it.Format != FormatBoolean {
		return false, formatErr(FormatBoolean, it.Format)
	}
	if len(it.Raw) != 1 {
		return false, fmt.Errorf("secscodec: BOOLEAN 项需要 1 字节，实际 %d", len(it.Raw))
	}
	return it.Raw[0] != 0, nil
}

// String 取字符串项；非 A/J 项报错。
func (it *Item) String() (string, error) {
	switch it.Format {
	case FormatASCII, FormatJIS:
		return it.Str, nil
	}
	return "", formatErr(FormatASCII, it.Format)
}

// Bytes 取原始字节项；非 B 项报错。
func (it *Item) Bytes() ([]byte, error) {
	if it.Format != FormatBinary {
		return nil, formatErr(FormatBinary, it.Format)
	}
	out := make([]byte, len(it.Raw))
	copy(out, it.Raw)
	return out, nil
}

// Child 取 L 项的第 i 个子项；不存在时报错。
func (it *Item) Child(i int) (*Item, error) {
	if it.Format != FormatList {
		return nil, formatErr(FormatList, it.Format)
	}
	if i < 0 || i >= len(it.Children) {
		return nil, fmt.Errorf("secscodec: L 项只有 %d 个子项，下标 %d 越界", len(it.Children), i)
	}
	return it.Children[i], nil
}

// ChildrenCount 返回 L 项的子项个数；非 L 项返回 0。
func (it *Item) ChildrenCount() int {
	if it.Format != FormatList {
		return 0
	}
	return len(it.Children)
}

// Text 把任意项渲染成一行可读文本（自检 CLI 的默认输出）。
func (it *Item) Text() string {
	switch it.Format {
	case FormatList:
		return fmt.Sprintf("L[%d]", len(it.Children))
	case FormatASCII, FormatJIS:
		return fmt.Sprintf("%s %q", FormatName(it.Format), it.Str)
	case FormatBinary:
		return fmt.Sprintf("B %x", it.Raw)
	case FormatBoolean:
		v := len(it.Raw) == 1 && it.Raw[0] != 0
		return fmt.Sprintf("BOOLEAN %v", v)
	case FormatI1:
		v, _ := it.I1()
		return fmt.Sprintf("I1 %d", v)
	case FormatI2:
		v, _ := it.I2()
		return fmt.Sprintf("I2 %d", v)
	case FormatI4:
		v, _ := it.I4()
		return fmt.Sprintf("I4 %d", v)
	case FormatI8:
		v, _ := it.I8()
		return fmt.Sprintf("I8 %d", v)
	case FormatU1:
		v, _ := it.U1()
		return fmt.Sprintf("U1 %d", v)
	case FormatU2:
		v, _ := it.U2()
		return fmt.Sprintf("U2 %d", v)
	case FormatU4:
		v, _ := it.U4()
		return fmt.Sprintf("U4 %d", v)
	case FormatU8:
		v, _ := it.U8()
		return fmt.Sprintf("U8 %d", v)
	case FormatF4:
		v, _ := it.F4()
		return fmt.Sprintf("F4 %g", v)
	case FormatF8:
		v, _ := it.F8()
		return fmt.Sprintf("F8 %g", v)
	}
	return fmt.Sprintf("%s ?", FormatName(it.Format))
}
