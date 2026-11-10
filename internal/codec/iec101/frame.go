// Package iec101 实现串行总线上的非平衡式远动传输的链路层：FT1.2 帧、
// 控制域、链路地址与校验和。
//
// 三种帧长什么样（同一套链路层服务，三种长度）：
//
//	固定帧  10 C A...  CS 16     —— 无用户数据，只有链路层服务
//	可变帧  68 L L 68 C A... ASDU CS 16
//	单字节  E5                   —— 肯定确认；A2 是否定确认
//
// 三个字节的位置最容易被搞错，因此在这里写死并且只写一次：
//
//   - L 只计「控制域 + 链路地址 + ASDU」，**不含**两个起始字节、第二个
//     起始字节、校验和与结束符。整帧长度 = L + 6。
//   - 校验和从**控制域**开始累加到 ASDU 最后一个字节，不含起始字节。
//   - 固定帧的校验和同样从控制域开始，覆盖控制域与链路地址。
//
// 「差一个字节」是这一层最典型的故障：按以太网承载的习惯把 L 算成整帧
// 长度、或者把起始字节也累加进校验和，帧看起来完全合法，对端却一声不响
// 或者回一个否定确认。因此本包的所有判断都以**原始字节**为单位，并把
// 上面三条写成函数而不是散落在调用点。
//
// 与 iec60870（应用层）的分工：本包只管把一个字节流切成帧、解析控制域，
// 不理解 ASDU 的任何内容。要从帧里取应用数据，把 Frame.Payload 交给
// iec60870.ParseASDUProfile。
//
// 本包不持有连接、不读时钟、不引用任何第三方库。
package iec101

import (
	"errors"
	"fmt"
)

// FT1.2 帧的固定标识。
const (
	// StartFixed 是固定帧的起始字节（也是唯一的起始标识）。
	StartFixed byte = 0x10
	// StartVar 是可变帧的起始字节，帧内出现两次。
	StartVar byte = 0x68
	// EndByte 是每帧的结束字节。
	EndByte byte = 0x16
	// AckByte 是单字节肯定确认。
	AckByte byte = 0xE5
	// NackByte 是单字节否定确认。
	NackByte byte = 0xA2
)

// 帧类型。
type Kind uint8

const (
	// KindFixed 是固定帧：只有控制域与链路地址，承载链路层服务。
	KindFixed Kind = iota
	// KindVariable 是可变帧：带 ASDU，承载用户数据。
	KindVariable
	// KindSingle 是单字节帧：E5/A2，只有确认/否认，没有控制域。
	KindSingle
)

func (k Kind) String() string {
	switch k {
	case KindFixed:
		return "fixed"
	case KindVariable:
		return "variable"
	case KindSingle:
		return "single"
	}
	return "?"
}

// MinLen 与 MaxLen 分别是一帧的最小与最大长度（含首尾标识）。
//
// 最大长度由 L 字段的一个字节决定：L <= 255，整帧 = L + 6，因此上限
// 是 261。把上限设成 255 是一个常见错误——它恰好把「满载」的合法帧
// 判成非法，症状是偶发地丢最后一条报文。
const (
	MinLen     = 5 // 固定帧最短：10 C CS 16（链路地址 0 字节时是 4，见下文）
	MaxLen     = 261
	maxLinkLen = 2
)

// MaxPayload 是一个可变帧里 ASDU 的最大字节数。
//
// 它由 L 字段反推：L = 1（控制域）+ 链路地址（0..2）+ ASDU，L <= 255。
// 取最保守的 2 字节链路地址，得到 252。真正的上限由配置的链路地址宽度
// 决定，Framer 会按实际宽度再算一次。
const MaxPayload = 255 - 1 - maxLinkLen

// ---------------------------------------------------------------------------
// 控制域
// ---------------------------------------------------------------------------

// 控制域里 PRM 位与功能码的位布局：
//
//	bit 7  bit 6  bit 5   bit 4   bit 3..0
//	RES    PRM    FCB/ACD FCV/DFC  功能码
//
// PRM=1 表示报文由启动站（主站）发出，PRM=0 表示由从动站发出。同一个
// 功能码值在两侧含义完全不同：主站的 8 是「请求访问需求」，从站的 8 是
// 「用户数据」。因此控制域必须与控制方向一起解释，单独看 8 会得出相反
// 的结论。
const (
	ctrlPRM byte = 0x40
	ctrlFCB byte = 0x20 // PRM=1 时是帧计数位
	ctrlACD byte = 0x20 // PRM=0 时是访问需求位
	ctrlFCV byte = 0x10 // PRM=1 时是帧计数有效位
	ctrlDFC byte = 0x10 // PRM=0 时是数据流控制位
	ctrlFC  byte = 0x0F // 功能码低 4 位
)

// 主站（PRM=1）功能码。
const (
	// FCResetLink 复位远方链路。
	FCResetLink byte = 0
	// FCResetProcess 复位用户进程。
	FCResetProcess byte = 1
	// FCSendConfirm 发送/确认，带用户数据（需要 FCB/FCV 交替）。
	FCSendConfirm byte = 3
	// FCSendNoReply 发送/无回答，带用户数据。
	FCSendNoReply byte = 4
	// FCRequestAccess 请求访问需求。
	FCRequestAccess byte = 8
	// FCRequestLinkStatus 请求链路状态。
	FCRequestLinkStatus byte = 9
	// FCRequestClass1 请求第 1 级数据。
	FCRequestClass1 byte = 10
	// FCRequestClass2 请求第 2 级数据。
	FCRequestClass2 byte = 11
)

// 从站（PRM=0）功能码。
const (
	// FCAck 肯定确认。
	FCAck byte = 0
	// FCNack 否定确认（链路忙）。
	FCNack byte = 1
	// FCUserData 用户数据。
	FCUserData byte = 8
	// FCNoData 请求的数据不可用。
	FCNoData byte = 9
	// FCLinkStatus 链路状态。
	FCLinkStatus byte = 11
)

// Control 是解析后的控制域。
type Control struct {
	// PRM 为 true 表示来自启动站（主站）。
	PRM bool
	// FCB 是帧计数位（仅 PRM=1 且 FCV=1 时有意义）。
	FCB bool
	// FCV 是帧计数有效位（仅 PRM=1 有意义）。
	FCV bool
	// ACD 是访问需求位（仅 PRM=0 有意义）：从站还有第 1 级数据待送。
	ACD bool
	// DFC 是数据流控制位（仅 PRM=0 有意义）：从站缓冲已满。
	DFC bool
	// Func 是功能码（低 4 位）。
	Func byte
	// Raw 是原始字节，便于日志与诊断。
	Raw byte
}

// ParseControl 解析控制域字节。
func ParseControl(b byte) Control {
	c := Control{
		PRM:  b&ctrlPRM != 0,
		Func: b & ctrlFC,
		Raw:  b,
	}
	if c.PRM {
		c.FCB = b&ctrlFCB != 0
		c.FCV = b&ctrlFCV != 0
	} else {
		c.ACD = b&ctrlACD != 0
		c.DFC = b&ctrlDFC != 0
	}
	return c
}

// Encode 把控制域写回一个字节。
func (c Control) Encode() byte {
	b := c.Func & ctrlFC
	if c.PRM {
		b |= ctrlPRM
		if c.FCB {
			b |= ctrlFCB
		}
		if c.FCV {
			b |= ctrlFCV
		}
	} else {
		if c.ACD {
			b |= ctrlACD
		}
		if c.DFC {
			b |= ctrlDFC
		}
	}
	return b
}

// String 返回控制域摘要，用于日志。
func (c Control) String() string {
	dir := "sec"
	if c.PRM {
		dir = "pri"
	}
	if c.PRM {
		return fmt.Sprintf("%s fc=%s fcb=%t fcv=%t", dir, FuncName(true, c.Func), c.FCB, c.FCV)
	}
	return fmt.Sprintf("%s fc=%s acd=%t dfc=%t", dir, FuncName(false, c.Func), c.ACD, c.DFC)
}

// FuncName 返回功能码的可读名。prm 决定按哪一侧的表解释。
func FuncName(prm bool, fc byte) string {
	if prm {
		switch fc {
		case FCResetLink:
			return "reset-link"
		case FCResetProcess:
			return "reset-process"
		case FCSendConfirm:
			return "send-confirm"
		case FCSendNoReply:
			return "send"
		case FCRequestAccess:
			return "request-access"
		case FCRequestLinkStatus:
			return "link-status"
		case FCRequestClass1:
			return "req-class1"
		case FCRequestClass2:
			return "req-class2"
		}
		return fmt.Sprintf("primary-%d", fc)
	}
	switch fc {
	case FCAck:
		return "ack"
	case FCNack:
		return "nack"
	case FCUserData:
		return "user-data"
	case FCNoData:
		return "no-data"
	case FCLinkStatus:
		return "link-status"
	}
	return fmt.Sprintf("secondary-%d", fc)
}

// ---------------------------------------------------------------------------
// 帧
// ---------------------------------------------------------------------------

// Frame 是一帧解析后的结果。
type Frame struct {
	Kind Kind
	// Ctrl 是控制域。单字节帧没有控制域，此字段为零值。
	Ctrl Control
	// LinkAddr 是链路地址（0 表示未配置，见 LinkAddrSize）。
	LinkAddr uint16
	// LinkAddrSize 是这一帧里链路地址占用的字节数（0..2）。
	LinkAddrSize int
	// Payload 是 ASDU 字节（仅可变帧非空）。
	Payload []byte
	// Byte 是单字节帧的原始字节（E5 肯定确认 / A2 否定确认）。
	//
	// 单字节帧没有控制域，Kind 无法表达「肯定还是否定」。把这个字节留下
	// 来，调用方才不用再去猜——猜错的表现是「设备回了 NACK，主站当成
	// 成功」，一条命令没执行却报了 OK。
	Byte byte
}

// String 返回帧摘要，用于日志与错误消息。
func (f Frame) String() string {
	if f.Kind == KindSingle {
		if f.Byte == AckByte {
			return "single ack"
		}
		if f.Byte == NackByte {
			return "single nack"
		}
		return "single"
	}
	return fmt.Sprintf("%s %s addr=%d(%dB) payload=%d",
		f.Kind, f.Ctrl, f.LinkAddr, f.LinkAddrSize, len(f.Payload))
}

// ---------------------------------------------------------------------------
// 组帧
// ---------------------------------------------------------------------------

// Checksum 计算并累加校验和。
//
// 从**控制域**开始，不含起始字节，不含长度字段，不含结束符。这是本协议
// 里最容易写错的一处算术，因此独立成函数并配上黄金字节测试。
func Checksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return sum
}

// appendLinkAddr 按宽度追加链路地址（低字节在前）。
func appendLinkAddr(b []byte, addr uint16, size int) ([]byte, error) {
	switch size {
	case 0:
		if addr != 0 {
			return nil, fmt.Errorf("link address %d cannot be encoded with 0 bytes", addr)
		}
		return b, nil
	case 1:
		if addr > 0xFF {
			return nil, fmt.Errorf("link address %d exceeds 1 byte", addr)
		}
		return append(b, byte(addr)), nil
	case 2:
		return append(b, byte(addr), byte(addr>>8)), nil
	}
	return nil, fmt.Errorf("link address size %d must be 0..2", size)
}

// parseLinkAddr 按宽度读出链路地址。
func parseLinkAddr(b []byte, size int) (uint16, error) {
	switch size {
	case 0:
		return 0, nil
	case 1:
		if len(b) < 1 {
			return 0, errors.New("truncated link address")
		}
		return uint16(b[0]), nil
	case 2:
		if len(b) < 2 {
			return 0, errors.New("truncated link address")
		}
		return uint16(b[0]) | uint16(b[1])<<8, nil
	}
	return 0, fmt.Errorf("link address size %d must be 0..2", size)
}

// BuildFixed 组一个固定帧。
func BuildFixed(ctrl Control, addr uint16, addrSize int) ([]byte, error) {
	body, err := appendLinkAddr([]byte{ctrl.Encode()}, addr, addrSize)
	if err != nil {
		return nil, err
	}
	fr := make([]byte, 0, len(body)+4)
	fr = append(fr, StartFixed)
	fr = append(fr, body...)
	fr = append(fr, Checksum(body), EndByte)
	return fr, nil
}

// BuildVariable 组一个可变帧。
func BuildVariable(ctrl Control, addr uint16, addrSize int, asdu []byte) ([]byte, error) {
	body := []byte{ctrl.Encode()}
	var err error
	body, err = appendLinkAddr(body, addr, addrSize)
	if err != nil {
		return nil, err
	}
	body = append(body, asdu...)
	if len(body) > 255 {
		return nil, fmt.Errorf("frame body %d bytes exceeds the 1-byte length field (max 255)", len(body))
	}
	fr := make([]byte, 0, len(body)+6)
	fr = append(fr, StartVar, byte(len(body)), byte(len(body)), StartVar)
	fr = append(fr, body...)
	fr = append(fr, Checksum(body), EndByte)
	return fr, nil
}

// BuildSingle 返回单字节确认帧。
func BuildSingle(positive bool) []byte {
	if positive {
		return []byte{AckByte}
	}
	return []byte{NackByte}
}

// ---------------------------------------------------------------------------
// 解帧
// ---------------------------------------------------------------------------

// ErrIncomplete 表示缓冲区里还没有一整帧，需要继续读。
var ErrIncomplete = errors.New("iec101: incomplete frame")

// ParseResult 是一次解析的结论。
type ParseResult struct {
	Frame Frame
	// Consumed 是本次从缓冲区头部消费掉的字节数。
	//
	// 无论成功与否都必须前进，否则调用方会在一个坏字节上死循环。成功时
	// 它等于整帧长度；失败时它至少是 1（跳过起始标识或一个噪声字节）。
	Consumed int
}

// ParseAt 从缓冲区头部解析一帧。
//
// addrSize 是配置的链路地址字节数（0..2）。它必须来自工程配置而不是
// 从报文里猜：链路地址是帧结构的一部分，猜错就整体错位。
//
// 返回 (res, nil) 表示解析出一帧；(res, ErrIncomplete) 表示数据还不够；
// 其他错误表示这段字节不是合法帧，调用方应按 res.Consumed 跳过。
func ParseAt(buf []byte, addrSize int) (ParseResult, error) {
	if addrSize < 0 || addrSize > maxLinkLen {
		return ParseResult{Consumed: 1}, fmt.Errorf("link address size %d must be 0..2", addrSize)
	}
	if len(buf) == 0 {
		return ParseResult{}, ErrIncomplete
	}
	switch buf[0] {
	case AckByte, NackByte:
		return ParseResult{
			Frame:    Frame{Kind: KindSingle, Byte: buf[0]},
			Consumed: 1,
		}, nil
	case StartFixed:
		return parseFixed(buf, addrSize)
	case StartVar:
		return parseVariable(buf, addrSize)
	}
	// 噪声：丢掉一个字节继续扫描。整段丢弃会把紧随其后的一帧好数据
	// 一起丢掉，而串口线上出现噪声是常态而不是异常。
	return ParseResult{Consumed: 1}, fmt.Errorf("iec101: unexpected byte 0x%02X", buf[0])
}

// fixedLen 返回固定帧的总长：起始 + 控制 + 链路地址 + 校验 + 结束。
func fixedLen(addrSize int) int { return 4 + addrSize }

// FrameLen 在长度已知时返回整帧字节数，用于一次读满。
//
// 第二个返回值为 false 表示「还不能确定长度」（字节不够，或首字节不是
// 起始标识）。它是收帧循环避免逐字节系统调用的关键。
func FrameLen(buf []byte, addrSize int) (int, bool) {
	if len(buf) == 0 {
		return 0, false
	}
	switch buf[0] {
	case AckByte, NackByte:
		return 1, true
	case StartFixed:
		return fixedLen(addrSize), true
	case StartVar:
		// 需要 L 两个字节。两者必须相同，但先按第一个算长度，
		// 相等性在 ParseAt 里校验——这里只是给读循环一个下界。
		if len(buf) < 2 {
			return 0, false
		}
		return int(buf[1]) + 6, true
	}
	return 1, true
}

func parseFixed(buf []byte, addrSize int) (ParseResult, error) {
	need := fixedLen(addrSize)
	if len(buf) < need {
		return ParseResult{}, ErrIncomplete
	}
	fr := buf[:need]
	if fr[need-1] != EndByte {
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: fixed frame missing end byte 0x16, got 0x%02X", fr[need-1])
	}
	body := fr[1 : need-2]
	want := Checksum(body)
	if got := fr[need-2]; got != want {
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: checksum 0x%02X, want 0x%02X", got, want)
	}
	addr, err := parseLinkAddr(body[1:], addrSize)
	if err != nil {
		return ParseResult{Consumed: 1}, err
	}
	return ParseResult{
		Frame: Frame{
			Kind:         KindFixed,
			Ctrl:         ParseControl(body[0]),
			LinkAddr:     addr,
			LinkAddrSize: addrSize,
		},
		Consumed: need,
	}, nil
}

func parseVariable(buf []byte, addrSize int) (ParseResult, error) {
	if len(buf) < 4 {
		return ParseResult{}, ErrIncomplete
	}
	l1, l2 := buf[1], buf[2]
	if l1 != l2 {
		// 两个长度字节不一致：帧内长度字段被破坏。整帧不可信，只跳过
		// 首字节重新寻找起始标识，让后面的好帧还有机会被认出来。
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: length bytes differ (%d vs %d)", l1, l2)
	}
	if buf[3] != StartVar {
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: second start byte 0x%02X, want 0x68", buf[3])
	}
	// L 最少要容纳控制域 1 字节；上限由字节本身保证（<=255）。
	if l1 < 1+byte(addrSize) {
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: length %d too small for control + %d-byte link address", l1, addrSize)
	}
	need := int(l1) + 6
	if len(buf) < need {
		return ParseResult{}, ErrIncomplete
	}
	fr := buf[:need]
	if fr[need-1] != EndByte {
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: variable frame missing end byte 0x16, got 0x%02X", fr[need-1])
	}
	body := fr[4 : need-2]
	want := Checksum(body)
	if got := fr[need-2]; got != want {
		return ParseResult{Consumed: 1}, fmt.Errorf("iec101: checksum 0x%02X, want 0x%02X", got, want)
	}
	addr, err := parseLinkAddr(body[1:], addrSize)
	if err != nil {
		return ParseResult{Consumed: 1}, err
	}
	payload := body[1+addrSize:]
	out := make([]byte, len(payload))
	copy(out, payload)
	return ParseResult{
		Frame: Frame{
			Kind:         KindVariable,
			Ctrl:         ParseControl(body[0]),
			LinkAddr:     addr,
			LinkAddrSize: addrSize,
			Payload:      out,
		},
		Consumed: need,
	}, nil
}
