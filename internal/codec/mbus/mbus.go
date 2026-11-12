// Package mbus 实现 M-Bus（EN 13757-2 链路层 / EN 13757-3 应用层）的纯函数
// 编解码。
//
// 这个协议与仓库里其它表计协议（DL/T 645、CJ/T 188）最不一样的地方在于
// **它有两层"变长"**：
//
//  1. 链路层有四种帧：单字符 ACK（0xE5）、短帧（0x10）、长帧（0x68）与
//     控制帧。短帧没有数据域，长帧的数据域长度在 L 字段里，且 L 同时出现
//     两次（两个 L 必须相等，否则是残帧或噪声）。分帧必须同时校验起始符、
//     双重 L、校验和与结束符，任何一处对不上都要回退一个字节重新扫描。
//  2. 应用层的可变数据结构是一串**自描述记录**：每条记录先用 DIF（+DIFE…）
//     说明"值有多长、是哪一类值、第几个存储号/费率/子单元"，再用 VIF
//     （+VIFE…）说明"这个值是什么物理量、单位与小数位"，最后才是数据
//     本身。主站不可能"按偏移读第 n 个值"——偏移取决于前一条记录的长度。
//
// 因此本包只做三件事：组帧/解帧、解析记录、把记录里的原始整数换算成
// 工程单位。它不建连接、不持有地址状态、不重传——那些属于驱动；但主站
// 与内置从站共用同一份组帧/解帧与记录编码，避免两边对同一个字节的理解
// 漂移。
//
// 帧结构（EN 13757-2 §4）：
//
//	ACK       E5
//	短帧      10 C A CS 16
//	长帧      68 L L 68 C A CI DATA(L-3) CS 16
//	控制帧    68 03 03 68 C A CI CS 16      （L=3，无用户数据）
//
// 校验和是 C、A、CI 与用户数据的**模 256 算术和**，不参与 L、起始符与
// 结束符。这个"覆盖范围与长度字段不同"的细节是 M-Bus 分帧最容易写错的
// 一处：按帧长算校验和会在长帧上永远差 3。
//
// C 字段（控制域）：
//
//	bit7  保留（发送方置 0）
//	bit6  PRM：1 = 主站报文（请求），0 = 从站报文（应答）
//	bit5  主站方向为 FCB，从站方向为 ACD
//	bit4  主站方向为 FCV，从站方向为 DFC
//	bit3-0 功能码
//
// 主站请求：SND_NKE=0x40、SND_UD=0x53/0x73、REQ_UD2=0x5B/0x7B（FCB 翻转）。
// 从站应答：RSP_UD=0x08，可能叠加 DFC(0x10) 与 ACD(0x20)。
//
// 地址字段是**单字节主地址**（0..250）。251..255 是保留/次级地址/广播：
// 251 保留，252 保留，253(0xFD) 次级地址，254(0xFE) 网络层，255(0xFF) 广播。
// 次级地址会把地址字段扩成 8 字节并改变整帧的校验范围，本包**不支持**
// 次级地址寻址：需要先发 SND_UD(CI=0x52) 选中从站，之后每一帧的地址
// 字段都是 8 字节。半成品支持比不支持更难排查，所以这里显式拒绝。
package mbus

import (
	"errors"
	"fmt"
)

// 链路层固定字节。
const (
	// Ack 是单字符确认帧。SND_NKE 的正常应答就是这一个字节。
	Ack byte = 0xE5
	// ShortStart 是短帧起始符。
	ShortStart byte = 0x10
	// LongStart 既是长帧起始符也是控制帧起始符（两者只差 L 的值）。
	LongStart byte = 0x68
	// Stop 是所有非单字符帧的结束符。
	Stop byte = 0x16
)

// 主站请求的功能码（C 字段低 4 位）。
const (
	// FuncSNDNKE 是初始化/复位从站的 FCB 状态。
	FuncSNDNKE byte = 0x00
	// FuncSNDUD 是主站向从站发送数据（改时间、改地址、写参数）。
	FuncSNDUD byte = 0x04
	// FuncREQUD2 是请求从站上送 2 类数据（整份遥测报文）。
	FuncREQUD2 byte = 0x0B
	// FuncREQUD1 是请求从站上送 1 类数据（少见，本包只用于诊断展示）。
	FuncREQUD1 byte = 0x0A
	// FuncRSPUD 是从站上送数据。
	FuncRSPUD byte = 0x08
)

// 常用的完整 C 字段取值。把它们写成常量而不是让调用方拼位，是因为
// FCB/FCV 两个位的位置与语义在主/从方向上是相反的（主站是 FCB/FCV，
// 从站是 ACD/DFC），拼错一位在总线上表现为"从站不理你"。
const (
	// CtrlSNDNKE 是 SND_NKE 请求：PRM=1，功能码 0，FCB/FCV 均为 0。
	CtrlSNDNKE byte = 0x40
	// CtrlREQUD2NoFCB 是 REQ_UD2 且 FCB=0（FCV=1）。标准要求 REQ_UD2 置
	// FCV，FCB 用来让从站识别"这是新请求还是上一次的重传"。
	CtrlREQUD2NoFCB byte = 0x5B
	// CtrlREQUD2FCB 是 REQ_UD2 且 FCB=1。
	CtrlREQUD2FCB byte = 0x7B
	// CtrlRSPUD 是从站数据应答的基准控制码；实际值可能再叠加 DFC/ACD。
	CtrlRSPUD byte = 0x08
)

// 控制信息字段（CI）。只列本包会解码的取值。
const (
	// CIVariableData 是可变数据结构的应答（8 字节以上、自描述记录）。
	// 这是绝大多数表计上送遥测时用的格式。
	CIVariableData byte = 0x72
	// CIFixedData 是固定数据结构的应答（老式紧凑表，16 字节固定布局）。
	CIFixedData byte = 0x73
	// CIGeneralError 是应用层错误应答，数据域首字节是错误码。
	CIGeneralError byte = 0x70
	// CIAlarmStatus 是告警状态应答。
	CIAlarmStatus byte = 0x71
	// CISelectSlave 是次级地址选择命令（本包不实现，仅用于报错时指认）。
	CISelectSlave byte = 0x52
)

// 地址字段的特殊值。
const (
	// AddrReservedLow、AddrReservedHigh 是保留地址。
	AddrReservedLow byte = 0xFB
	// AddrSecondary 表示后续帧使用 8 字节次级地址。
	AddrSecondary byte = 0xFD
	// AddrNetwork 是网络层地址。
	AddrNetwork byte = 0xFE
	// AddrBroadcast 是广播地址：从站执行但不应答。
	AddrBroadcast byte = 0xFF
	// MaxPrimaryAddr 是最大的可用主地址（0..250）。
	MaxPrimaryAddr byte = 250
)

// 分帧错误。它们都可由 Reader 吞掉并继续扫描，只有调用方需要区分
// "帧还没收全"与"这帧是坏的"。
var (
	// ErrIncomplete 表示缓冲区里的字节还不够组成一帧。
	ErrIncomplete = errors.New("mbus: 帧不完整")
	// ErrChecksum 表示校验和不匹配。总线上出现它是干扰/终端电阻/地线的
	// 典型症状，与"从站没应答"是两类故障。
	ErrChecksum = errors.New("mbus: 校验和错误")
	// ErrFrame 表示帧结构不合法（起始符、长度、结束符对不上）。
	ErrFrame = errors.New("mbus: 帧结构不合法")
	// ErrResync 表示当前位置不是帧起点，跳过即可。
	ErrResync = errors.New("mbus: 非帧起点")
)

// FrameType 是 M-Bus 的四种帧类型。
type FrameType byte

const (
	FrameAck FrameType = iota + 1
	FrameShort
	FrameLong
	FrameControl
)

func (t FrameType) String() string {
	switch t {
	case FrameAck:
		return "ack"
	case FrameShort:
		return "short"
	case FrameLong:
		return "long"
	case FrameControl:
		return "control"
	}
	return "unknown"
}

// Frame 是一帧解析后的视图。
//
// Data 只对长帧有意义，且**不含 CI 本身**：CI 单独放在 CIField 里，因为
// 它是"这份数据是什么格式"的开关，而 Data 是格式内部的字节。把两者混在
// 一起会让每个调用方都自己切第一个字节。
type Frame struct {
	Type    FrameType
	C       byte
	A       byte
	CIField byte
	Data    []byte
}

// Master 表示这帧是否由主站发出（C 字段 PRM=1）。
func (f Frame) Master() bool { return f.C&0x40 != 0 }

// Function 返回 C 字段的功能码（低 4 位）。
func (f Frame) Function() byte { return f.C & 0x0F }

// FCB 返回主站方向上的帧计数位。
func (f Frame) FCB() bool { return f.C&0x20 != 0 }

// FCV 返回主站方向上的帧计数有效位。
func (f Frame) FCV() bool { return f.C&0x10 != 0 }

// ACD 返回从站方向上的"有 2 类数据待取"位。
func (f Frame) ACD() bool { return f.C&0x20 != 0 }

// DFC 返回从站方向上的"数据流控制"位：置位表示从站缓冲满，主站应把
// 后续报文缩短或稍后再来。
func (f Frame) DFC() bool { return f.C&0x10 != 0 }

// Checksum 计算 C、A、CI 与数据域的模 256 和。
func Checksum(c, a, ci byte, data []byte) byte {
	sum := c + a + ci
	for _, b := range data {
		sum += b
	}
	return sum
}

// BuildAck 返回单字符确认帧。
func BuildAck() []byte { return []byte{Ack} }

// BuildShort 组短帧。短帧只能承载 C、A 两个字节的信息，用于 SND_NKE、
// REQ_UD2 这类没有用户数据的请求。
func BuildShort(c, a byte) []byte {
	cs := c + a
	return []byte{ShortStart, c, a, cs, Stop}
}

// BuildLong 组长帧。data 是 CI 之后的用户数据。
func BuildLong(c, a, ci byte, data []byte) []byte {
	length := 3 + len(data)
	out := make([]byte, 0, length+6)
	out = append(out, LongStart, byte(length), byte(length), LongStart, c, a, ci)
	out = append(out, data...)
	out = append(out, Checksum(c, a, ci, data), Stop)
	return out
}

// BuildControl 组控制帧（L=3、无用户数据的长帧）。
//
// 控制帧与长帧只差长度字段，但不少从站对"L=3 的长帧"与"控制帧"的处理
// 并不一致；SND_UD 需要带数据，真正用到空控制帧的场合极少。保留它是因为
// 解析侧必须能把它与长帧区分开，否则会把 CI 之后本该有的数据当成缺失。
func BuildControl(c, a, ci byte) []byte {
	return []byte{LongStart, 3, 3, LongStart, c, a, ci, Checksum(c, a, ci, nil), Stop}
}

// Parse 从 buf 头部解析一帧，返回帧与消耗的字节数。
//
// 返回 ErrIncomplete 时 consumed 是**起始符之前**的噪声字节数，调用方必须
// 保留起始符继续等数据；返回 ErrChecksum/ErrFrame 时 consumed 是建议跳过
// 的字节数（至少 1）。这套语义与 DL/T 645 的 Reader 一致，驱动与内置从站
// 才能共用同一份扫描循环。
func Parse(buf []byte) (Frame, int, error) {
	if len(buf) == 0 {
		return Frame{}, 0, ErrIncomplete
	}
	switch buf[0] {
	case Ack:
		// 单字符 ACK 没有 C 字段；这里留 0，调用方必须先判 Type 再读 C。
		return Frame{Type: FrameAck}, 1, nil
	case ShortStart:
		return parseShort(buf)
	case LongStart:
		return parseLong(buf)
	}
	return Frame{}, 1, ErrResync
}

func parseShort(buf []byte) (Frame, int, error) {
	if len(buf) < 5 {
		return Frame{}, 0, ErrIncomplete
	}
	c, a, cs := buf[1], buf[2], buf[3]
	if buf[4] != Stop {
		return Frame{}, 1, fmt.Errorf("%w: 短帧结束符是 %02X，应为 16", ErrFrame, buf[4])
	}
	if got := c + a; got != cs {
		return Frame{}, 1, fmt.Errorf("%w: 短帧校验和 %02X，按 C+A 应为 %02X", ErrChecksum, cs, got)
	}
	return Frame{Type: FrameShort, C: c, A: a}, 5, nil
}

func parseLong(buf []byte) (Frame, int, error) {
	// 头 4 字节：68 L L 68。两个 L 必须相等；第二个 0x68 是防止把
	// 一段噪声里的单个 0x68 当成帧头。
	if len(buf) < 4 {
		return Frame{}, 0, ErrIncomplete
	}
	length := buf[1]
	if buf[2] != length {
		return Frame{}, 1, fmt.Errorf("%w: 长帧两个长度字段不等（%d/%d）", ErrFrame, buf[1], buf[2])
	}
	if buf[3] != LongStart {
		return Frame{}, 1, fmt.Errorf("%w: 长帧头第 4 字节是 %02X，应为 68", ErrFrame, buf[3])
	}
	if length < 3 {
		return Frame{}, 1, fmt.Errorf("%w: 长帧长度 %d 小于 3（至少含 C/A/CI）", ErrFrame, length)
	}
	// 整帧长度 = 4（头）+ length（C…数据）+ 1（校验和）+ 1（结束符）。
	total := int(length) + 6
	if len(buf) < total {
		return Frame{}, 0, ErrIncomplete
	}
	if buf[total-1] != Stop {
		return Frame{}, 1, fmt.Errorf("%w: 长帧结束符是 %02X，应为 16", ErrFrame, buf[total-1])
	}
	c, a, ci := buf[4], buf[5], buf[6]
	// 次级地址帧的地址字段是 8 字节，本包不解析。明确报错，避免把
	// 后半段地址当成数据、再用错误的校验和判定"干扰"。
	if a == AddrSecondary || a == AddrNetwork {
		return Frame{}, total, fmt.Errorf(
			"%w: 地址 %02X 表示次级地址寻址，本包只支持单字节主地址", ErrFrame, a)
	}
	data := buf[7 : 4+int(length)]
	cs := buf[4+int(length)]
	if got := Checksum(c, a, ci, data); got != cs {
		// 校验和错时**跳过整个帧**而不是 1 个字节：这里的长度字段
		// （两个相等的 L）与结束符都已经对上了，帧边界是可信的，只有
		// 内容坏了。逐字节重扫会进到数据域里，一旦某个字节恰好是 0x68
		// 就会解析出一条"幽灵帧"，把一次干扰报成两次坏帧——而
		// BadFrames 正是驱动用来判断"线路噪声"的依据，虚高会误导排查。
		return Frame{}, total, fmt.Errorf("%w: 长帧校验和 %02X，应为 %02X", ErrChecksum, cs, got)
	}
	typ := FrameLong
	if length == 3 {
		typ = FrameControl
	}
	return Frame{Type: typ, C: c, A: a, CIField: ci, Data: data}, total, nil
}

// FrameLen 在已知起始符时返回整帧长度；未知时返回 ok=false。
//
// 它只用于让 Reader 一次读满剩余字节，减少逐字节系统调用；读多读少都是
// 安全的，真正的边界仍由 Parse 判定。
func FrameLen(buf []byte) (n int, ok bool) {
	if len(buf) == 0 {
		return 0, false
	}
	switch buf[0] {
	case Ack:
		return 1, true
	case ShortStart:
		return 5, true
	case LongStart:
		if len(buf) < 2 {
			return 0, false
		}
		return int(buf[1]) + 6, true
	}
	return 0, false
}

// IsPrimaryAddress 判断地址是否是可用的单字节主地址。
func IsPrimaryAddress(a byte) bool { return a <= MaxPrimaryAddr }

// ValidatePrimaryAddress 把"能不能拿这个地址去发请求"讲清楚。
func ValidatePrimaryAddress(a byte) error {
	switch {
	case a <= MaxPrimaryAddr:
		return nil
	case a == AddrBroadcast:
		return fmt.Errorf("地址 %d 是广播：从站会执行但不应答，不能用于读表", a)
	case a == AddrSecondary:
		return fmt.Errorf("地址 %d 是次级地址标记：本包不支持 8 字节次级地址寻址", a)
	default:
		return fmt.Errorf("地址 %d 不是主地址（0..%d）", a, MaxPrimaryAddr)
	}
}

// ReqUD2Control 返回带指定 FCB 的 REQ_UD2 控制字节。
//
// FCB 只在 FCV=1 时有意义；本实现始终置 FCV，因为要让从站区分"重传"
// 与"新请求"——超时重发时 FCB 不变，从站会重发上一次的应答而不是把
// 存储值再读一遍（也避免访问号被重复累加）。
func ReqUD2Control(fcb bool) byte {
	if fcb {
		return CtrlREQUD2FCB
	}
	return CtrlREQUD2NoFCB
}
