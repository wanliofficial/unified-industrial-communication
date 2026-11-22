// Package iec104 实现面向以太网的平衡式远动传输的 APCI 帧层。
//
//	┌─ APDU（最长 255 字节，首字节 0x68）─────────────────┐
//	│ 0x68  长度(1，只算其后的 4 字节控制域 + ASDU)          │
//	├─ APCI（4 字节）────────────────────────────────────┤
//	│ I 帧：发送序号 N(S) 15 位 | 接收序号 N(R) 15 位        │
//	│ S 帧：0x01 0x00         | 接收序号 N(R) 15 位          │
//	│ U 帧：功能码 1 字节 + 3 个 0x00                        │
//	├─ ASDU（见包 iec60870）────────────────────────────┤
//	│ 类型(1) 可变结构限定词(1) 传送原因(1) 源发地址(1)      │
//	│ 公共地址(2，小端) 信息对象...                         │
//	└────────────────────────────────────────────────────┘
//
// 与 Modbus/S7 不同，「读」不是一个请求-应答动作：数据由从站按变化主动
// 上送，主站只在建立链路后做一次总召唤拿初始快照。因此本包的职责被刻意
// 压到最小——只负责 APCI 的组帧/解帧与序号校验；序号回绕、窗口、超时与
// 「迟到报文」全部留给 driver 层。
//
// 应用层（ASDU、元素、品质、时标）在串行总线与以太网两种承载上逐字节
// 相同，因此被抽到 iec60870 包，只保留一份实现。本文件末尾用类型别名
// 与常量转发把它重新导出，既有调用点无需改动。
//
// 本包不持有连接、不读时钟、不引用任何第三方库。
package iec104

import (
	"encoding/binary"
	"errors"
	"fmt"

	"uic/internal/codec/iec60870"
)

// DefaultPort 是 IEC 60870-5-104 的默认 TCP 端口。
const DefaultPort = 2404

// StartByte 是每个 APDU 的首字节。
const StartByte = 0x68

// MaxAPDU 是长度字节能表达的最大值（4 字节控制域 + 249 字节 ASDU）。
const MaxAPDU = 253

// HeaderLen 是 APDU 固定头长度：起始字节 + 长度 + 4 字节控制域。
const HeaderLen = 6

// maxASDU 是一帧能承载的 ASDU 上限。
const maxASDU = MaxAPDU - 4

// 序号上限：15 位，计到 32767 之后回到 0。
const maxSeq = 0x7FFF

// ---------------------------------------------------------------------------
// APCI 帧
// ---------------------------------------------------------------------------

// Kind 是 APCI 帧类型。
type Kind uint8

const (
	// KindI 是信息帧，承载 ASDU，同时捎带接收序号。
	KindI Kind = iota
	// KindS 是监视帧，只确认收到的 I 帧，不承载 ASDU。
	KindS
	// KindU 是无编号控制帧，用于启停数据传输与链路测试。
	KindU
)

func (k Kind) String() string {
	switch k {
	case KindI:
		return "I"
	case KindS:
		return "S"
	case KindU:
		return "U"
	}
	return "?"
}

// U 帧功能码。注意这些值不能随手取：低两位必须是 11，否则会被当成
// I/S 帧；其余位两两配对（act / con）。
const (
	UStartAct byte = 0x07
	UStartCon byte = 0x0B
	UStopAct  byte = 0x13
	UStopCon  byte = 0x23
	UTestAct  byte = 0x43
	UTestCon  byte = 0x83
)

// UName 返回 U 帧功能码的可读名，用于日志与错误消息。
func UName(fn byte) string {
	switch fn {
	case UStartAct:
		return "STARTDT act"
	case UStartCon:
		return "STARTDT con"
	case UStopAct:
		return "STOPDT act"
	case UStopCon:
		return "STOPDT con"
	case UTestAct:
		return "TESTFR act"
	case UTestCon:
		return "TESTFR con"
	}
	return fmt.Sprintf("U(0x%02X)", fn)
}

// Frame 是解析后的 APCI 帧。
type Frame struct {
	Kind    Kind
	SendSeq uint16 // 仅 I 帧
	RecvSeq uint16 // I/S 帧
	UFunc   byte   // 仅 U 帧
	ASDU    []byte // 仅 I 帧
}

// ParseFrame 解析一整帧 APDU（含 0x68 与长度字节）。
//
// 调用方必须先按长度字段切好帧：本函数不做流式重组，也不会从尾部多余的
// 字节里「猜」下一帧。把分帧与解析分开，是为了让「长度字段被写错」这类
// 错误在 driver 层就变成明确的协议错，而不是在 ASDU 里静默错位。
func ParseFrame(b []byte) (Frame, error) {
	if len(b) < HeaderLen {
		return Frame{}, fmt.Errorf("apdu too short: %d bytes", len(b))
	}
	if b[0] != StartByte {
		return Frame{}, fmt.Errorf("apdu start byte: want 0x68, got 0x%02X", b[0])
	}
	n := int(b[1])
	if n < 4 {
		return Frame{}, fmt.Errorf("apdu length field %d is smaller than the 4-byte control field", n)
	}
	if n > MaxAPDU {
		return Frame{}, fmt.Errorf("apdu length field %d exceeds %d", n, MaxAPDU)
	}
	if len(b) != 2+n {
		return Frame{}, fmt.Errorf("apdu length mismatch: field says %d, got %d payload bytes", n, len(b)-2)
	}

	ctrl := b[2:6]
	var f Frame
	switch {
	case ctrl[0]&0x01 == 0:
		// I 帧：两个序号都占 15 位，最低位是帧类型标志。
		f.Kind = KindI
		f.SendSeq = (uint16(ctrl[0]) | uint16(ctrl[1])<<8) >> 1
		f.RecvSeq = (uint16(ctrl[2]) | uint16(ctrl[3])<<8) >> 1
		f.ASDU = b[6:]
		// I 帧必须承载 ASDU。长度字段正好 4 说明控制域之后没有内容，
		// 这既不是 S 帧（最低位对不上）也不是合法的 I 帧，只能判错。
		if len(f.ASDU) == 0 {
			return Frame{}, errors.New("I frame carries no ASDU")
		}
	case ctrl[0] == 0x01 && ctrl[1] == 0x00 && ctrl[2]&0x01 == 0:
		f.Kind = KindS
		f.RecvSeq = (uint16(ctrl[2]) | uint16(ctrl[3])<<8) >> 1
	case ctrl[0]&0x03 == 0x03 && ctrl[1] == 0 && ctrl[2] == 0 && ctrl[3] == 0:
		f.Kind = KindU
		f.UFunc = ctrl[0]
	default:
		return Frame{}, fmt.Errorf("unrecognized control field % X", ctrl)
	}
	return f, nil
}

// BuildIFrame 生成 I 帧。send/recv 超过 15 位是编程错误，直接报错而不是
// 截断——截断会让序号在无提示的情况下错位，表现为「偶尔丢一帧」。
func BuildIFrame(send, recv uint16, asdu []byte) ([]byte, error) {
	if send > maxSeq || recv > maxSeq {
		return nil, fmt.Errorf("sequence out of range: send=%d recv=%d (max %d)", send, recv, maxSeq)
	}
	if len(asdu) == 0 {
		return nil, errors.New("I frame requires an ASDU")
	}
	if len(asdu) > maxASDU {
		return nil, fmt.Errorf("ASDU too large: %d bytes (max %d)", len(asdu), maxASDU)
	}
	b := make([]byte, HeaderLen+len(asdu))
	b[0] = StartByte
	b[1] = byte(4 + len(asdu))
	binary.LittleEndian.PutUint16(b[2:4], send<<1)
	binary.LittleEndian.PutUint16(b[4:6], recv<<1)
	copy(b[6:], asdu)
	return b, nil
}

// BuildSFrame 生成 S 帧。
func BuildSFrame(recv uint16) ([]byte, error) {
	if recv > maxSeq {
		return nil, fmt.Errorf("receive sequence out of range: %d", recv)
	}
	b := make([]byte, HeaderLen)
	b[0] = StartByte
	b[1] = 4
	b[2] = 0x01
	b[4] = byte(recv << 1)
	b[5] = byte(recv >> 7)
	return b, nil
}

// BuildUFrame 生成 U 帧。
func BuildUFrame(fn byte) ([]byte, error) {
	switch fn {
	case UStartAct, UStartCon, UStopAct, UStopCon, UTestAct, UTestCon:
	default:
		return nil, fmt.Errorf("unknown U-frame function 0x%02X", fn)
	}
	b := make([]byte, HeaderLen)
	b[0] = StartByte
	b[1] = 4
	b[2] = fn
	return b, nil
}

// ---------------------------------------------------------------------------
// 应用层转发（实现见 iec60870）
// ---------------------------------------------------------------------------
//
// 这里全部是别名与一行转发：104 与 101 的应用层只有一份实现（iec60870），
// 但驱动、内置从站、自检命令都按「当前承载的包」引用符号。用别名而不是
// 让它们各自 import 两个包，是为了让「这条 ASDU 属于哪种承载」在调用点
// 一目了然，同时避免有人误以为两边的类型不兼容、写出一段多余的转换。

// 类型标识。
const (
	TypeSinglePoint            = iec60870.TypeSinglePoint
	TypeDoublePoint            = iec60870.TypeDoublePoint
	TypeMeasuredNormalized     = iec60870.TypeMeasuredNormalized
	TypeMeasuredScaled         = iec60870.TypeMeasuredScaled
	TypeMeasuredFloat          = iec60870.TypeMeasuredFloat
	TypeIntegratedTotals       = iec60870.TypeIntegratedTotals
	TypeIntegratedTotalsTime   = iec60870.TypeIntegratedTotalsTime
	TypeSinglePointTime        = iec60870.TypeSinglePointTime
	TypeDoublePointTime        = iec60870.TypeDoublePointTime
	TypeMeasuredNormalizedTime = iec60870.TypeMeasuredNormalizedTime
	TypeMeasuredScaledTime     = iec60870.TypeMeasuredScaledTime
	TypeMeasuredFloatTime      = iec60870.TypeMeasuredFloatTime
	TypeSingleCommand          = iec60870.TypeSingleCommand
	TypeDoubleCommand          = iec60870.TypeDoubleCommand
	TypeSetpointNormalized     = iec60870.TypeSetpointNormalized
	TypeSetpointScaled         = iec60870.TypeSetpointScaled
	TypeSetpointFloat          = iec60870.TypeSetpointFloat
	TypeEndOfInit              = iec60870.TypeEndOfInit
	TypeInterrogation          = iec60870.TypeInterrogation
	TypeCounterInterrogation   = iec60870.TypeCounterInterrogation
	TypeClockSync              = iec60870.TypeClockSync
)

// 传送原因。
const (
	CausePeriodic          = iec60870.CausePeriodic
	CauseBackground        = iec60870.CauseBackground
	CauseSpontaneous       = iec60870.CauseSpontaneous
	CauseInitialized       = iec60870.CauseInitialized
	CauseRequest           = iec60870.CauseRequest
	CauseActivation        = iec60870.CauseActivation
	CauseActivationCon     = iec60870.CauseActivationCon
	CauseDeactivation      = iec60870.CauseDeactivation
	CauseDeactivationCon   = iec60870.CauseDeactivationCon
	CauseActivationTerm    = iec60870.CauseActivationTerm
	CauseInterrogated      = iec60870.CauseInterrogated
	CauseGroup1            = iec60870.CauseGroup1
	CauseUnknownType       = iec60870.CauseUnknownType
	CauseUnknownCause      = iec60870.CauseUnknownCause
	CauseUnknownCommonAddr = iec60870.CauseUnknownCommonAddr
	CauseUnknownIOA        = iec60870.CauseUnknownIOA
)

// QOIStation 是站总召唤限定词。
const QOIStation = iec60870.QOIStation

// 品质位。
const (
	QOverflow    = iec60870.QOverflow
	QBlocked     = iec60870.QBlocked
	QSubstituted = iec60870.QSubstituted
	QNotTopical  = iec60870.QNotTopical
	QInvalid     = iec60870.QInvalid
)

// 应用层数据类型。
type (
	Quality = iec60870.Quality
	Object  = iec60870.Object
	ASDU    = iec60870.ASDU
	Profile = iec60870.Profile
)

// Profile104 是 104 固定使用的剖面（ORG 1 / CA 2 / IOA 3）。
var Profile104 = iec60870.Profile104

// 应用层编解码。
var (
	ParseASDU        = iec60870.ParseASDU
	ParseASDUProfile = iec60870.ParseASDUProfile
	BuildASDU        = iec60870.BuildASDU
	BuildASDUProfile = iec60870.BuildASDUProfile
	ElementSize      = iec60870.ElementSize
	IsCommandType    = iec60870.IsCommandType
	IsData           = iec60870.IsData
	IsTimeTagged     = iec60870.IsTimeTagged
	EncodeCP56Time2a = iec60870.EncodeCP56Time2a
	DecodeCP56Time2a = iec60870.DecodeCP56Time2a
)

// 应用层请求构造。
var (
	BuildInterrogation        = iec60870.BuildInterrogation
	BuildCounterInterrogation = iec60870.BuildCounterInterrogation
	BuildClockSync            = iec60870.BuildClockSync
	BuildSingleCommand        = iec60870.BuildSingleCommand
	BuildDoubleCommand        = iec60870.BuildDoubleCommand
	BuildSetpointNormalized   = iec60870.BuildSetpointNormalized
	BuildSetpointScaled       = iec60870.BuildSetpointScaled
	BuildSetpointFloat        = iec60870.BuildSetpointFloat
)

// 名称查询。
var (
	TypeName     = iec60870.TypeName
	TypeFromName = iec60870.TypeFromName
	TypeNames    = iec60870.TypeNames
	CauseName    = iec60870.CauseName
)
