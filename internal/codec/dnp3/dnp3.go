// Package dnp3 实现 DNP3（IEEE 1815）的编解码。
//
// DNP3 是**三层叠加**的协议，每一层都有自己的头、自己的长度字段和自己的
// 校验，而且三层的错误现象完全不同：
//
//	链路层  —— 0x0564 帧头 + CRC，错一个字节整帧丢弃；
//	传输层  —— 1 字节的 FIR/FIN/SEQ，负责把超长应用报文切片；
//	应用层  —— 功能码 + 对象头（组/变体/限定词）+ 对象值。
//
// 因此这里刻意按层拆分：链路帧、传输分片、应用报文、对象解析各自独立，
// 驱动可以只用到其中几层，测试也能逐层钉死。把所有东西塞进一个
// `Decode([]byte) (any, error)` 会让「CRC 错」和「对象组不认识」变成同一种
// 失败，而现场排障恰恰最需要区分这两件事。
//
// 字节序：DNP3 全链路小端（含 CRC 的存放顺序）。CRC 是 CRC-16/DNP，
// 多项式 0x3D65 的反射形式 0xA6BC，初值 0，结果取反；标准校验值
// CRC("123456789") == 0xEA82，单元测试以此为锚。
//
// 边界：只实现本平台用到的对象组（见 numericSpec 与 bitLayout 的注释），
// 不做文件传输、不做安全认证（Secure Authentication）、不做时间同步的
// 写路径、不做设备属性（G0）。遇到不认识的组时返回已解析的部分对象加
// UnsupportedError——「读懂了一半」比「整帧丢掉」更有用。
package dnp3

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// 链路帧的起止字节。DNP3 没有帧尾，靠长度字段与 CRC 定界。
const (
	Start1 byte = 0x05
	Start2 byte = 0x64
)

// 链路控制字节的位。
const (
	ControlDir = 0x80 // 1 = 主站 → 从站
	ControlPrm = 0x40 // 1 = 发起方（primary）
	ControlFCB = 0x20
	ControlFCV = 0x10
)

// 链路层功能码。低 4 位。
const (
	FuncResetLink         byte = 0
	FuncResetUserProcess  byte = 1
	FuncTestLink          byte = 2
	FuncConfirmedUserData byte = 3
	FuncUnconfirmedData   byte = 4
	FuncRequestLinkStatus byte = 9

	FuncAck          byte = 0
	FuncNack         byte = 1
	FuncLinkStatus   byte = 11
	FuncNotSupported byte = 15
	MaxUserData           = 250 // LEN 最大 255，减去 CTRL/DEST/SRC 五个字节
	MaxAppPerFrame        = MaxUserData - 1
	headerLen             = 10 // 0x05 0x64 LEN CTRL DEST(2) SRC(2) CRC(2)
)

// 应用层功能码。
const (
	AppConfirm            byte = 0x00
	AppRead               byte = 0x01
	AppWrite              byte = 0x02
	AppSelect             byte = 0x03
	AppOperate            byte = 0x04
	AppDirectOperate      byte = 0x05
	AppDirectOperateNoAck byte = 0x06
	AppEnableUnsolicited  byte = 0x14
	AppDisableUnsolicited byte = 0x15
	AppResponse           byte = 0x81
	AppUnsolicited        byte = 0x82
)

// IIN（内部指示）位。线格式是先 IIN1 后 IIN2，所以低字节在前。
const (
	IINAllStations    uint16 = 0x0001
	IINClass1Events   uint16 = 0x0002
	IINClass2Events   uint16 = 0x0004
	IINClass3Events   uint16 = 0x0008
	IINNeedTime       uint16 = 0x0010
	IINLocalControl   uint16 = 0x0020
	IINDeviceTrouble  uint16 = 0x0040
	IINDeviceRestart  uint16 = 0x0080
	IINFuncNotSupport uint16 = 0x0100
	IINObjectUnknown  uint16 = 0x0200
	IINParamError     uint16 = 0x0400
	IINEventOverflow  uint16 = 0x0800
	IINAlreadyExec    uint16 = 0x1000
	IINConfigCorrupt  uint16 = 0x2000
)

// 品质位。二进制量与模拟量共用低 6 位，bit7 在二进制量里是状态。
const (
	FlagOnline       byte = 0x01
	FlagRestart      byte = 0x02
	FlagCommLost     byte = 0x04
	FlagRemoteForced byte = 0x08
	FlagLocalForced  byte = 0x10
	FlagChatter      byte = 0x20 // 计数器为 ROLLOVER，模拟量为 OVER_RANGE
	FlagRefErr       byte = 0x40 // 模拟量为 REFERENCE_ERR
	FlagState        byte = 0x80 // 仅二进制量：当前状态
)

// 对象组。本实现认识的组都在这里列出来，README/驱动文档引用同一份常量。
const (
	GroupBinaryInput       byte = 1
	GroupBinaryEvent       byte = 2
	GroupDoubleBitInput    byte = 3
	GroupDoubleBitEvent    byte = 4
	GroupBinaryOutput      byte = 10
	GroupBinaryOutputEvent byte = 11
	GroupCROB              byte = 12
	GroupCounter           byte = 20
	GroupCounterEvent      byte = 22
	GroupAnalogInput       byte = 30
	GroupAnalogEvent       byte = 32
	GroupAnalogOutput      byte = 40
	GroupAnalogOutputBlock byte = 41
	GroupTime              byte = 50
	GroupTimeCTO           byte = 51
	GroupTimeDelay         byte = 52
	GroupClass             byte = 60
)

// 限定词的低 4 位（范围码）。高 4 位是前缀码，见 prefixSize。
const (
	QualStartStop8  byte = 0x00
	QualStartStop16 byte = 0x01
	QualStartStop32 byte = 0x02
	QualAllObjects  byte = 0x06
	QualCount8      byte = 0x07
	QualCount16     byte = 0x08
)

// QualIndex8/QualIndex16 是最常见的两种「带索引前缀的事件」限定词。
const (
	QualIndex8  byte = 0x17
	QualIndex16 byte = 0x28
	QualIndex32 byte = 0x38
)

// CROB 控制码。
const (
	CROBPulseOn  byte = 0x01
	CROBPulseOff byte = 0x02
	CROBLatchOn  byte = 0x03
	CROBLatchOff byte = 0x04
)

// CRC 计算 CRC-16/DNP。
//
// 注意这是「反射」算法：右移而不是左移，多项式用 0xA6BC。写成左移版本
// 在空数据上碰巧也返回 0xFFFF，很容易骗过随手写的测试，所以这里用标准
// 校验值 CRC("123456789") == 0xEA82 做锚。
func CRC(b []byte) uint16 {
	var crc uint16
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA6BC
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

// Frame 是一条已解出的链路帧。
type Frame struct {
	Control byte
	Dest    uint16
	Source  uint16
	Data    []byte // 已剥掉每 16 字节一块的 CRC
}

// Primary 表示这是发起方（主站或主动上送的从站）发出的帧。
func (f Frame) Primary() bool { return f.Control&ControlPrm != 0 }

// Function 返回链路功能码。
func (f Frame) Function() byte { return f.Control & 0x0F }

// EncodeFrame 把一段用户数据封装成链路帧。
//
// 用户数据每 16 字节追加 2 字节 CRC；最后一块可以不足 16 字节。整个帧
// 最长 292 字节（10 字节头 + 250 字节用户数据 + 32 字节块 CRC）。
func EncodeFrame(control byte, dest, source uint16, data []byte) ([]byte, error) {
	if len(data) > MaxUserData {
		return nil, fmt.Errorf("dnp3: user data %d bytes exceeds %d", len(data), MaxUserData)
	}
	out := make([]byte, 0, headerLen+len(data)+2*((len(data)+15)/16))
	out = append(out, Start1, Start2, byte(5+len(data)), control,
		byte(dest), byte(dest>>8), byte(source), byte(source>>8))
	crc := CRC(out[2:8])
	out = append(out, byte(crc), byte(crc>>8))
	for i := 0; i < len(data); i += 16 {
		end := i + 16
		if end > len(data) {
			end = len(data)
		}
		block := data[i:end]
		out = append(out, block...)
		bc := CRC(block)
		out = append(out, byte(bc), byte(bc>>8))
	}
	return out, nil
}

// Decoder 从 TCP 字节流里切出链路帧。
//
// TCP 不保留消息边界，一次 Read 可能拿到半个帧、两个帧或两帧之间的噪声。
// 解码器在缓冲区里找 0x05 0x64，按长度字段等够整帧，再逐块验证 CRC。
// CRC 错误的帧整帧丢弃并返回错误——宁可丢一帧并让上层计数，也不要把
// 一个字节错位的数据当成合法值。
type Decoder struct {
	buf []byte
}

// Feed 追加收到的字节。
func (d *Decoder) Feed(p []byte) { d.buf = append(d.buf, p...) }

// Buffered 返回尚未消费的字节数。
func (d *Decoder) Buffered() int { return len(d.buf) }

// Next 返回下一条完整帧。第二个返回值为 false 表示数据还不够，调用方
// 应继续读 socket。
func (d *Decoder) Next() (Frame, bool, error) {
	for {
		idx := indexStart(d.buf)
		if idx < 0 {
			// 保留结尾的单个 0x05：它可能是下一帧的第一个字节。
			if n := len(d.buf); n > 0 && d.buf[n-1] == Start1 {
				d.buf = d.buf[n-1:]
			} else {
				d.buf = d.buf[:0]
			}
			return Frame{}, false, nil
		}
		if idx > 0 {
			d.buf = d.buf[idx:]
		}
		if len(d.buf) < headerLen {
			return Frame{}, false, nil
		}
		length := int(d.buf[2])
		if length < 5 {
			// 长度字段非法：跳过这两个起始字节，继续找下一帧。
			d.buf = d.buf[2:]
			return Frame{}, false, fmt.Errorf("dnp3: invalid link length %d", length)
		}
		userLen := length - 5
		total := headerLen + userLen + 2*((userLen+15)/16)
		if len(d.buf) < total {
			return Frame{}, false, nil
		}
		raw := d.buf[:total]
		d.buf = d.buf[total:]

		want := binary.LittleEndian.Uint16(raw[8:10])
		if got := CRC(raw[2:8]); got != want {
			return Frame{}, false, fmt.Errorf("dnp3: header CRC %04X, want %04X", got, want)
		}
		data := make([]byte, 0, userLen)
		pos := headerLen
		remain := userLen
		for remain > 0 {
			n := remain
			if n > 16 {
				n = 16
			}
			want := binary.LittleEndian.Uint16(raw[pos+n : pos+n+2])
			if got := CRC(raw[pos : pos+n]); got != want {
				return Frame{}, false, fmt.Errorf("dnp3: data block CRC %04X, want %04X", got, want)
			}
			data = append(data, raw[pos:pos+n]...)
			pos += n + 2
			remain -= n
		}
		return Frame{Control: raw[3], Dest: binary.LittleEndian.Uint16(raw[4:6]),
			Source: binary.LittleEndian.Uint16(raw[6:8]), Data: data}, true, nil
	}
}

func indexStart(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == Start1 && b[i+1] == Start2 {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 传输层
// ---------------------------------------------------------------------------

// 传输头位。
const (
	TransportFIR = 0x80
	TransportFIN = 0x40
	TransportSeq = 0x3F
)

// EncodeTransport 把应用报文切成带传输头的片段。
//
// seq 是第一个片段的序号（0..63），后续片段依次 +1 回绕。返回的每个片段
// 都已含 1 字节传输头，调用方直接塞进链路帧即可。
func EncodeTransport(payload []byte, seq uint8) [][]byte {
	if len(payload) == 0 {
		return [][]byte{{TransportFIR | TransportFIN | seq&TransportSeq}}
	}
	out := make([][]byte, 0, (len(payload)+MaxAppPerFrame-1)/MaxAppPerFrame)
	for i := 0; i < len(payload); i += MaxAppPerFrame {
		end := i + MaxAppPerFrame
		if end > len(payload) {
			end = len(payload)
		}
		h := seq & TransportSeq
		if i == 0 {
			h |= TransportFIR
		}
		if end == len(payload) {
			h |= TransportFIN
		}
		chunk := make([]byte, 0, 1+end-i)
		chunk = append(chunk, h)
		chunk = append(chunk, payload[i:end]...)
		out = append(out, chunk)
		seq++
	}
	return out
}

// Reassembler 把多帧传输拼回完整应用报文。
//
// 它只保证「同一时刻只有一条在重组」。DNP3 的传输序号是 6 位的，回绕很
// 快；如果对端交错发送两条多帧消息，重组必然错位。协议本身也不允许这种
// 交错，因此遇到序号不连续时直接丢弃本轮并返回错误，让上层重新发起读，
// 而不是把半条消息交给应用层去解释。
type Reassembler struct {
	buf    []byte
	next   uint8
	active bool
}

// Push 送进一个传输片段。第二个返回值为 true 时表示一条应用报文已完整。
func (r *Reassembler) Push(fir, fin bool, seq uint8, p []byte) ([]byte, bool, error) {
	seq &= TransportSeq
	if fir {
		r.buf = r.buf[:0]
		r.active = true
		r.next = seq
	}
	if !r.active {
		return nil, false, nil
	}
	if seq != r.next {
		r.active = false
		r.buf = r.buf[:0]
		return nil, false, fmt.Errorf("dnp3: transport sequence %d, want %d", seq, r.next)
	}
	r.buf = append(r.buf, p...)
	r.next = (seq + 1) & TransportSeq
	if !fin {
		return nil, false, nil
	}
	out := append([]byte(nil), r.buf...)
	r.active = false
	r.buf = r.buf[:0]
	return out, true, nil
}

// ---------------------------------------------------------------------------
// 应用层
// ---------------------------------------------------------------------------

// AppControl 是应用报文的首字节。
type AppControl struct {
	FIR bool
	FIN bool
	CON bool
	UNS bool
	Seq uint8
}

// Byte 编码应用控制字节。
func (a AppControl) Byte() byte {
	b := a.Seq & 0x0F
	if a.FIR {
		b |= 0x80
	}
	if a.FIN {
		b |= 0x40
	}
	if a.CON {
		b |= 0x20
	}
	if a.UNS {
		b |= 0x10
	}
	return b
}

// ParseAppControl 解析应用控制字节。
func ParseAppControl(b byte) AppControl {
	return AppControl{
		FIR: b&0x80 != 0, FIN: b&0x40 != 0, CON: b&0x20 != 0,
		UNS: b&0x10 != 0, Seq: b & 0x0F,
	}
}

// AppMessage 是一条完整的应用报文。
//
// 请求没有 IIN 字段，响应才有；解码时按功能码的最高位判断，而不是靠
// 调用方「记得传对」。
type AppMessage struct {
	Control  AppControl
	Function byte
	IIN      uint16
	Objects  []byte
}

// DecodeApp 解析应用报文。
func DecodeApp(b []byte) (AppMessage, error) {
	if len(b) < 2 {
		return AppMessage{}, errors.New("dnp3: application header truncated")
	}
	m := AppMessage{Control: ParseAppControl(b[0]), Function: b[1]}
	if m.Function >= 0x80 {
		if len(b) < 4 {
			return AppMessage{}, errors.New("dnp3: application response header truncated")
		}
		m.IIN = uint16(b[2]) | uint16(b[3])<<8
		m.Objects = b[4:]
	} else {
		m.Objects = b[2:]
	}
	return m, nil
}

// EncodeRequest 编码一条应用请求（无 IIN）。
func EncodeRequest(ctrl AppControl, function byte, objects []byte) []byte {
	out := make([]byte, 0, 2+len(objects))
	out = append(out, ctrl.Byte(), function)
	return append(out, objects...)
}

// EncodeResponse 编码一条应用响应（含 IIN）。
func EncodeResponse(ctrl AppControl, function byte, iin uint16, objects []byte) []byte {
	out := make([]byte, 0, 4+len(objects))
	out = append(out, ctrl.Byte(), function, byte(iin), byte(iin>>8))
	return append(out, objects...)
}

// ---------------------------------------------------------------------------
// 对象
// ---------------------------------------------------------------------------

// CROB 是控制继电器输出块（组 12 变体 1）。
type CROB struct {
	Code    byte
	Count   byte
	OnTime  uint32
	OffTime uint32
	Status  byte
}

// Success 表示从站接受了这次控制。
func (c CROB) Success() bool { return c.Status == 0 }

// Object 是一个已解析的对象值。
//
// Value 的具体类型由组/变体决定：二进制量是 bool，双点量是 uint64(0..3)，
// 计数器是 uint64，模拟量是 int64 或 float64，控制块是 CROB。调用方按
// 组别断言，不要按「猜的类型」断言——同一个字段在不同变体里宽度不同。
type Object struct {
	Group     byte
	Variation byte
	Index     uint32
	Flags     byte
	HasFlags  bool
	Value     any
	When      time.Time
	HasTime   bool
	Relative  uint16
	HasRel    bool
}

// Header 是只解到对象头与范围的请求头，供从站解析 READ 用。
type Header struct {
	Group     byte
	Variation byte
	Qualifier byte
	Start     uint32
	Stop      uint32
	Count     int // All 为 true 时是 -1
	All       bool
}

// UnsupportedError 表示遇到了本实现不认识的组/变体。
//
// 单独成类型是为了让调用方可以「保留已解析的对象 + 记一条日志」，而不是
// 因为一个无关的对象组把整条响应丢掉。
type UnsupportedError struct {
	Group     byte
	Variation byte
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("dnp3: unsupported object group %d variation %d", e.Group, e.Variation)
}

// ParseObjects 解析响应中的对象区。
//
// 返回的错误可能是 *UnsupportedError，此时 objects 里是出错前已经解析出的
// 部分结果，调用方可以照常使用。
func ParseObjects(p []byte) ([]Object, error) {
	var out []Object
	pos := 0
	for pos < len(p) {
		if pos+3 > len(p) {
			return out, fmt.Errorf("dnp3: object header truncated at byte %d", pos)
		}
		g, v, q := p[pos], p[pos+1], p[pos+2]
		pos += 3
		pref := prefixSize(q)
		if pref < 0 {
			return out, fmt.Errorf("dnp3: unsupported qualifier 0x%02X", q)
		}
		start, count, all, err := consumeRange(p, &pos, q)
		if err != nil {
			return out, err
		}
		switch g {
		case GroupBinaryInput, GroupDoubleBitInput, GroupBinaryOutput,
			GroupBinaryEvent, GroupDoubleBitEvent, GroupBinaryOutputEvent:
			if err := parseBits(p, &pos, g, v, pref, start, count, all, &out); err != nil {
				return out, err
			}
		case GroupTime, GroupTimeCTO, GroupTimeDelay:
			if err := skipFixed(p, &pos, g, v, count, all); err != nil {
				return out, err
			}
		default:
			if err := parseNumericGroup(p, &pos, g, v, pref, start, count, all, &out); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// ParseRequestHeaders 只解析「头 + 范围」，不解析对象体。
//
// READ 请求的报文只有头与范围（限定词 0x06 时连范围都没有），用
// ParseObjects 会把它当成空对象体而失败。
func ParseRequestHeaders(p []byte) ([]Header, error) {
	var out []Header
	pos := 0
	for pos < len(p) {
		if pos+3 > len(p) {
			return out, fmt.Errorf("dnp3: request header truncated at byte %d", pos)
		}
		h := Header{Group: p[pos], Variation: p[pos+1], Qualifier: p[pos+2]}
		pos += 3
		if prefixSize(h.Qualifier) < 0 {
			return out, fmt.Errorf("dnp3: unsupported qualifier 0x%02X", h.Qualifier)
		}
		start, count, all, err := consumeRange(p, &pos, h.Qualifier)
		if err != nil {
			return out, err
		}
		h.Start, h.Stop, h.Count, h.All = start, 0, count, all
		if !all && (h.Qualifier&0x0F) <= QualStartStop32 {
			h.Stop = start + uint32(count) - 1
		}
		out = append(out, h)
	}
	return out, nil
}

func prefixSize(q byte) int {
	switch q >> 4 {
	case 0:
		return 0
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return 4
	}
	return -1
}

func consumeRange(p []byte, pos *int, q byte) (uint32, int, bool, error) {
	switch q & 0x0F {
	case QualStartStop8:
		if *pos+2 > len(p) {
			return 0, 0, false, errors.New("dnp3: range truncated")
		}
		start, stop := uint32(p[*pos]), uint32(p[*pos+1])
		*pos += 2
		if stop < start {
			return 0, 0, false, fmt.Errorf("dnp3: invalid range %d..%d", start, stop)
		}
		return start, int(stop-start) + 1, false, nil
	case QualStartStop16:
		if *pos+4 > len(p) {
			return 0, 0, false, errors.New("dnp3: range truncated")
		}
		start := uint32(binary.LittleEndian.Uint16(p[*pos:]))
		stop := uint32(binary.LittleEndian.Uint16(p[*pos+2:]))
		*pos += 4
		if stop < start {
			return 0, 0, false, fmt.Errorf("dnp3: invalid range %d..%d", start, stop)
		}
		return start, int(stop-start) + 1, false, nil
	case QualStartStop32:
		if *pos+8 > len(p) {
			return 0, 0, false, errors.New("dnp3: range truncated")
		}
		start := binary.LittleEndian.Uint32(p[*pos:])
		stop := binary.LittleEndian.Uint32(p[*pos+4:])
		*pos += 8
		if stop < start {
			return 0, 0, false, fmt.Errorf("dnp3: invalid range %d..%d", start, stop)
		}
		return start, int(stop-start) + 1, false, nil
	case QualAllObjects:
		return 0, -1, true, nil
	case QualCount8:
		if *pos+1 > len(p) {
			return 0, 0, false, errors.New("dnp3: count truncated")
		}
		n := int(p[*pos])
		*pos++
		if n == 0 {
			return 0, 0, false, errors.New("dnp3: zero object count")
		}
		return 0, n, false, nil
	case QualCount16:
		if *pos+2 > len(p) {
			return 0, 0, false, errors.New("dnp3: count truncated")
		}
		n := int(binary.LittleEndian.Uint16(p[*pos:]))
		*pos += 2
		if n == 0 {
			return 0, 0, false, errors.New("dnp3: zero object count")
		}
		return 0, n, false, nil
	}
	return 0, 0, false, fmt.Errorf("dnp3: unsupported range code 0x%02X", q&0x0F)
}

func consumeIndex(p []byte, pos *int, size int) (uint32, error) {
	if *pos+size > len(p) {
		return 0, errors.New("dnp3: index prefix truncated")
	}
	var idx uint32
	switch size {
	case 1:
		idx = uint32(p[*pos])
	case 2:
		idx = uint32(binary.LittleEndian.Uint16(p[*pos:]))
	case 4:
		idx = binary.LittleEndian.Uint32(p[*pos:])
	default:
		return 0, fmt.Errorf("dnp3: unsupported index prefix size %d", size)
	}
	*pos += size
	return idx, nil
}

// bitLayout 返回该组/变体是否带品质位、是否带时标。
//
// 静态二进制量（组 1/3/10）的变体 1 是纯位打包；变体 2 是每个点一个
// 「状态 + 品质」字节，状态在 bit7（双点量在 bit6-7），没有独立的位图
// 字段。事件（组 2/4/11）的变体 1/2/3 分别对应无时标、绝对时标、相对
// 时标，且**只能带索引前缀逐点编码**——带时标的事件没有「打包位」这种
// 写法，因为每个点的时标不同。遇到无前缀的带时标事件直接报不支持，
// 而不是猜一个布局把时间读错。
func bitLayout(g, v byte) (flags bool, timeKind int, err error) {
	switch g {
	case GroupBinaryInput, GroupDoubleBitInput, GroupBinaryOutput:
		switch v {
		case 1:
			return false, 0, nil
		case 2:
			return true, 0, nil
		}
	case GroupBinaryEvent, GroupDoubleBitEvent:
		switch v {
		case 1:
			return true, 0, nil
		case 2:
			return true, 6, nil
		case 3:
			return true, 2, nil
		}
	case GroupBinaryOutputEvent:
		switch v {
		case 1:
			return true, 0, nil
		case 2:
			return true, 6, nil
		}
	}
	return false, 0, &UnsupportedError{Group: g, Variation: v}
}

func bitValue(group byte, flags byte) any {
	switch group {
	case GroupDoubleBitInput, GroupDoubleBitEvent:
		return uint64(flags >> 6)
	default:
		return flags&FlagState != 0
	}
}

func parseBits(p []byte, pos *int, g, v byte, pref int, start uint32, count int, all bool, out *[]Object) error {
	flags, timeKind, err := bitLayout(g, v)
	if err != nil {
		return err
	}
	if pref > 0 {
		// 事件对象一定是「索引 + flags(+ 时标)」逐点编码，没有位图字段。
		n := count
		for i := 0; n < 0 || i < n; i++ {
			if n < 0 && *pos >= len(p) {
				break
			}
			idx, err := consumeIndex(p, pos, pref)
			if err != nil {
				return err
			}
			if !flags {
				return fmt.Errorf("dnp3: group %d variation %d with index prefix is not supported", g, v)
			}
			if *pos+1 > len(p) {
				return errors.New("dnp3: flags truncated")
			}
			f := p[*pos]
			*pos++
			o := Object{Group: g, Variation: v, Index: idx, Flags: f, HasFlags: true, Value: bitValue(g, f)}
			switch timeKind {
			case 6:
				if *pos+6 > len(p) {
					return errors.New("dnp3: event time truncated")
				}
				o.When = decodeTime48(p[*pos:])
				o.HasTime = true
				*pos += 6
			case 2:
				if *pos+2 > len(p) {
					return errors.New("dnp3: event relative time truncated")
				}
				o.Relative = binary.LittleEndian.Uint16(p[*pos:])
				o.HasRel = true
				*pos += 2
			}
			*out = append(*out, o)
		}
		return nil
	}

	// 无前缀：静态量。变体 1 是纯位图；变体 2 是「状态 + 品质」打包在
	// 同一个字节里（bit7 是状态，双点量是 bit6-7），**没有**独立的位图
	// 字段。把这两种布局混为一谈会让对象体整体错位。
	if g == GroupBinaryEvent || g == GroupDoubleBitEvent || g == GroupBinaryOutputEvent {
		return fmt.Errorf("dnp3: packed event group %d variation %d is not supported", g, v)
	}
	if all {
		rem := len(p) - *pos
		if flags {
			count = rem
		} else {
			bitsPer := 1
			if g == GroupDoubleBitInput {
				bitsPer = 2
			}
			count = rem * 8 / bitsPer
		}
	}
	if count < 0 {
		return errors.New("dnp3: object count is unknown")
	}
	if flags {
		if *pos+count > len(p) {
			return errors.New("dnp3: flags truncated")
		}
		fbuf := p[*pos : *pos+count]
		*pos += count
		for i := 0; i < count; i++ {
			f := fbuf[i]
			*out = append(*out, Object{
				Group: g, Variation: v, Index: start + uint32(i),
				Flags: f, HasFlags: true, Value: bitValue(g, f),
			})
		}
		return nil
	}

	// 变体 1：只有位图。二进制/二进制输出是 1 位一点，双点量是 2 位一点。
	bitsPer := 1
	if g == GroupDoubleBitInput {
		bitsPer = 2
	}
	nbytes := (count*bitsPer + 7) / 8
	if *pos+nbytes > len(p) {
		return errors.New("dnp3: packed bits truncated")
	}
	bits := p[*pos : *pos+nbytes]
	*pos += nbytes
	for i := 0; i < count; i++ {
		var val any
		if bitsPer == 2 {
			val = uint64((bits[i/4] >> (2 * uint(i%4))) & 0x03)
		} else {
			val = bits[i/8]>>uint(i%8)&1 != 0
		}
		*out = append(*out, Object{
			Group: g, Variation: v, Index: start + uint32(i),
			Value: val,
		})
	}
	return nil
}

// numericSpec 描述一个数值对象的线格式。
//
// size 是含品质位与时标的完整宽度；kind 决定值的解释方式。
type numericSpec struct {
	size  int
	flags bool
	kind  byte // i=int32 I=int16 u=uint32 U=uint16 f=float32 d=float64 c=CROB
	time  bool
}

func numericSpecOf(g, v byte) (numericSpec, bool) {
	switch g {
	case GroupCounter:
		switch v {
		case 1:
			return numericSpec{size: 5, flags: true, kind: 'u'}, true
		case 2:
			return numericSpec{size: 3, flags: true, kind: 'U'}, true
		case 5:
			return numericSpec{size: 4, kind: 'u'}, true
		case 6:
			return numericSpec{size: 2, kind: 'U'}, true
		}
	case GroupCounterEvent:
		switch v {
		case 1:
			return numericSpec{size: 5, flags: true, kind: 'u'}, true
		case 2:
			return numericSpec{size: 3, flags: true, kind: 'U'}, true
		// 带时标的计数器事件（22v5/v6）：32/16 位值 + 品质 + 6 字节绝对时标，
		// 合计 11/9 字节。此前误用了组 20「无品质」的 4/2 字节布局，真实从站
		// 回这两条变体时对象体整体错位——第一个对象就把后续所有对象读歪。
		// 尺寸与 IEEE 1815 / DNP Users Group AN2013-004b 一致：组 32 带时标
		// 整型同样是 11/9 字节绝对时标（组 22 没有相对时标变体，只有绝对时标）。
		case 5:
			return numericSpec{size: 11, flags: true, kind: 'u', time: true}, true
		case 6:
			return numericSpec{size: 9, flags: true, kind: 'U', time: true}, true
		}
	case GroupAnalogInput:
		switch v {
		case 1:
			return numericSpec{size: 5, flags: true, kind: 'i'}, true
		case 2:
			return numericSpec{size: 3, flags: true, kind: 'I'}, true
		case 3:
			return numericSpec{size: 4, kind: 'i'}, true
		case 4:
			return numericSpec{size: 2, kind: 'I'}, true
		case 5:
			return numericSpec{size: 5, flags: true, kind: 'f'}, true
		case 6:
			return numericSpec{size: 9, flags: true, kind: 'd'}, true
		}
	case GroupAnalogOutput:
		switch v {
		case 1:
			return numericSpec{size: 5, flags: true, kind: 'i'}, true
		case 2:
			return numericSpec{size: 3, flags: true, kind: 'I'}, true
		case 3:
			return numericSpec{size: 5, flags: true, kind: 'f'}, true
		case 4:
			return numericSpec{size: 9, flags: true, kind: 'd'}, true
		}
	case GroupAnalogEvent:
		switch v {
		case 1:
			return numericSpec{size: 5, flags: true, kind: 'i'}, true
		case 2:
			return numericSpec{size: 3, flags: true, kind: 'I'}, true
		case 3:
			return numericSpec{size: 11, flags: true, kind: 'i', time: true}, true
		case 4:
			return numericSpec{size: 9, flags: true, kind: 'I', time: true}, true
		case 5:
			return numericSpec{size: 5, flags: true, kind: 'f'}, true
		case 6:
			return numericSpec{size: 9, flags: true, kind: 'd'}, true
		case 7:
			return numericSpec{size: 11, flags: true, kind: 'f', time: true}, true
		case 8:
			return numericSpec{size: 15, flags: true, kind: 'd', time: true}, true
		}
	case GroupAnalogOutputBlock:
		switch v {
		case 1:
			return numericSpec{size: 4, kind: 'i'}, true
		case 2:
			return numericSpec{size: 2, kind: 'I'}, true
		case 3:
			return numericSpec{size: 4, kind: 'f'}, true
		case 4:
			return numericSpec{size: 8, kind: 'd'}, true
		}
	case GroupCROB:
		if v == 1 {
			return numericSpec{size: 11, kind: 'c'}, true
		}
	}
	return numericSpec{}, false
}

func parseNumericGroup(p []byte, pos *int, g, v byte, pref int, start uint32, count int, all bool, out *[]Object) error {
	spec, ok := numericSpecOf(g, v)
	if !ok {
		return &UnsupportedError{Group: g, Variation: v}
	}
	if pref > 0 {
		for i := 0; count < 0 || i < count; i++ {
			if count < 0 && *pos >= len(p) {
				break
			}
			idx, err := consumeIndex(p, pos, pref)
			if err != nil {
				return err
			}
			o, err := readNumeric(p, pos, spec, g, v, idx)
			if err != nil {
				return err
			}
			*out = append(*out, o)
		}
		return nil
	}
	if all {
		if spec.size <= 0 {
			return fmt.Errorf("dnp3: cannot infer object count for group %d variation %d", g, v)
		}
		count = (len(p) - *pos) / spec.size
	}
	if count < 0 {
		return errors.New("dnp3: object count is unknown")
	}
	for i := 0; i < count; i++ {
		o, err := readNumeric(p, pos, spec, g, v, start+uint32(i))
		if err != nil {
			return err
		}
		*out = append(*out, o)
	}
	return nil
}

func readNumeric(p []byte, pos *int, spec numericSpec, g, v byte, idx uint32) (Object, error) {
	if *pos+spec.size > len(p) {
		return Object{}, fmt.Errorf("dnp3: group %d variation %d object truncated at byte %d", g, v, *pos)
	}
	b := p[*pos : *pos+spec.size]
	*pos += spec.size
	o := Object{Group: g, Variation: v, Index: idx}
	off := 0
	if spec.flags {
		o.Flags = b[0]
		o.HasFlags = true
		off = 1
	}
	switch spec.kind {
	case 'i':
		o.Value = int64(int32(binary.LittleEndian.Uint32(b[off:])))
	case 'I':
		o.Value = int64(int16(binary.LittleEndian.Uint16(b[off:])))
	case 'u':
		o.Value = uint64(binary.LittleEndian.Uint32(b[off:]))
	case 'U':
		o.Value = uint64(binary.LittleEndian.Uint16(b[off:]))
	case 'f':
		o.Value = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:])))
	case 'd':
		o.Value = math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
	case 'c':
		o.Value = CROB{
			Code: b[0], Count: b[1],
			OnTime:  binary.LittleEndian.Uint32(b[2:]),
			OffTime: binary.LittleEndian.Uint32(b[6:]),
			Status:  b[10],
		}
	}
	if spec.time {
		o.When = decodeTime48(b[off+valueWidth(spec.kind):])
		o.HasTime = true
	}
	return o, nil
}

func valueWidth(kind byte) int {
	switch kind {
	case 'I', 'U':
		return 2
	case 'i', 'u', 'f':
		return 4
	case 'd':
		return 8
	}
	return 0
}

func skipFixed(p []byte, pos *int, g, v byte, count int, all bool) error {
	size := 0
	switch g {
	case GroupTime, GroupTimeCTO:
		size = 6
	case GroupTimeDelay:
		size = 2
	}
	if size == 0 {
		return &UnsupportedError{Group: g, Variation: v}
	}
	if all {
		count = (len(p) - *pos) / size
	}
	if count < 0 {
		return errors.New("dnp3: object count is unknown")
	}
	if *pos+count*size > len(p) {
		return errors.New("dnp3: time object truncated")
	}
	*pos += count * size
	return nil
}

// decodeTime48 解析 DNP3 的 48 位毫秒时间戳（自 1970-01-01 UTC）。
func decodeTime48(b []byte) time.Time {
	if len(b) < 6 {
		return time.Time{}
	}
	ms := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 |
		uint64(b[3])<<24 | uint64(b[4])<<32 | uint64(b[5])<<40
	return time.UnixMilli(int64(ms)).UTC()
}

// EncodeTime48 把时间编码成 48 位毫秒。
func EncodeTime48(t time.Time) []byte {
	ms := uint64(t.UnixMilli())
	return []byte{byte(ms), byte(ms >> 8), byte(ms >> 16), byte(ms >> 24), byte(ms >> 32), byte(ms >> 40)}
}

// ---------------------------------------------------------------------------
// 构造辅助（主站与从站共用）
// ---------------------------------------------------------------------------

// EncodeHeader 生成对象头。
//
// 对计数型限定词（QualCount8/16），stop 参数承载对象个数而不是结束索引；
// 这样调用方不必为两种语义记两套参数名。对 QualAllObjects 两个参数都忽略。
func EncodeHeader(g, v, q byte, start, stop uint32) ([]byte, error) {
	out := []byte{g, v, q}
	switch q & 0x0F {
	case QualStartStop8:
		if start > 0xFF || stop > 0xFF {
			return nil, fmt.Errorf("dnp3: start/stop %d..%d exceeds 8-bit range", start, stop)
		}
		out = append(out, byte(start), byte(stop))
	case QualStartStop16:
		if start > 0xFFFF || stop > 0xFFFF {
			return nil, fmt.Errorf("dnp3: start/stop %d..%d exceeds 16-bit range", start, stop)
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(start))
		out = binary.LittleEndian.AppendUint16(out, uint16(stop))
	case QualStartStop32:
		out = binary.LittleEndian.AppendUint32(out, start)
		out = binary.LittleEndian.AppendUint32(out, stop)
	case QualAllObjects:
	case QualCount8:
		if stop > 0xFF {
			return nil, fmt.Errorf("dnp3: count %d exceeds 8-bit range", stop)
		}
		out = append(out, byte(stop))
	case QualCount16:
		if stop > 0xFFFF {
			return nil, fmt.Errorf("dnp3: count %d exceeds 16-bit range", stop)
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(stop))
	default:
		return nil, fmt.Errorf("dnp3: unsupported qualifier 0x%02X", q)
	}
	return out, nil
}

// ClassObjects 生成「读某一类数据」的对象区。
//
// 类 0 是静态值，类 1..3 是事件。一次 READ 可以把四类都带上，从站用一条
// 响应回全部——总召唤就是这么做的。
func ClassObjects(classes ...byte) ([]byte, error) {
	var out []byte
	for _, c := range classes {
		if c > 3 {
			return nil, fmt.Errorf("dnp3: class %d out of range", c)
		}
		out = append(out, GroupClass, c+1, QualAllObjects)
	}
	return out, nil
}

// CROBObjects 生成一个直接控制（组 12 变体 1）的对象区。
//
// 索引前缀按数值大小选 8/16/32 位：点表允许 24 位索引，>0xFFFF 时用
// QualIndex32——把 uint16 静默截断会把控制命令下发到错误的点上。
func CROBObjects(index uint32, code, count byte, onTime, offTime uint32) ([]byte, error) {
	q := indexQualifier(index)
	out, err := EncodeHeader(GroupCROB, 1, q, 0, 1)
	if err != nil {
		return nil, err
	}
	out = appendIndexPrefix(out, q, index)
	out = append(out, code, count)
	out = binary.LittleEndian.AppendUint32(out, onTime)
	out = binary.LittleEndian.AppendUint32(out, offTime)
	out = append(out, 0) // status：请求里恒为 0
	return out, nil
}

// AnalogOutputObjects 生成一个模拟量输出（组 41）的对象区。
//
// variation 必须是 1..4，分别对应 int32 / int16 / float32 / float64。
func AnalogOutputObjects(index uint32, variation byte, value float64) ([]byte, error) {
	if variation < 1 || variation > 4 {
		return nil, fmt.Errorf("dnp3: analog output variation %d out of range", variation)
	}
	q := indexQualifier(index)
	out, err := EncodeHeader(GroupAnalogOutputBlock, variation, q, 0, 1)
	if err != nil {
		return nil, err
	}
	out = appendIndexPrefix(out, q, index)
	switch variation {
	case 1:
		out = binary.LittleEndian.AppendUint32(out, uint32(int32(value)))
	case 2:
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(value)))
	case 3:
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(value)))
	case 4:
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(value))
	}
	return out, nil
}

// indexQualifier 按索引大小选带索引前缀的限定词（8/16/32 位）。
func indexQualifier(index uint32) byte {
	switch {
	case index > 0xFFFF:
		return QualIndex32
	case index > 0xFF:
		return QualIndex16
	}
	return QualIndex8
}

// appendIndexPrefix 按限定词把索引前缀写进对象区。
func appendIndexPrefix(out []byte, q byte, index uint32) []byte {
	switch q {
	case QualIndex8:
		return append(out, byte(index))
	case QualIndex16:
		return binary.LittleEndian.AppendUint16(out, uint16(index))
	}
	return binary.LittleEndian.AppendUint32(out, index)
}
