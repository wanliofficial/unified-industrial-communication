// Package iec104 实现 IEC 60870-5-104 的 APCI 与 ASDU 纯函数编解码。
//
// 协议只有两层封装，但每层的「序号」语义完全不同：
//
//	┌─ APDU（最长 255 字节，首字节 0x68）─────────────────┐
//	│ 0x68  长度(1，只算其后的 4 字节控制域 + ASDU)          │
//	├─ APCI（4 字节）────────────────────────────────────┤
//	│ I 帧：发送序号 N(S) 15 位 | 接收序号 N(R) 15 位        │
//	│ S 帧：0x01 0x00         | 接收序号 N(R) 15 位          │
//	│ U 帧：功能码 1 字节 + 3 个 0x00                        │
//	├─ ASDU──────────────────────────────────────────────┤
//	│ 类型(1) 可变结构限定词(1) 传送原因(1) 源发地址(1)      │
//	│ 公共地址(2，小端) 信息对象...                         │
//	└────────────────────────────────────────────────────┘
//
// 与 Modbus/S7 不同，104 的「读」不是一个请求-应答动作：数据由从站按变化
// 主动上送，主站只在建立链路后做一次总召唤拿初始快照。因此本包的职责被
// 刻意压到最小——只负责把字节变成结构化 ASDU，以及把结构化 ASDU 变成
// 字节；序号回绕、窗口、超时与「迟到报文」全部留给 driver 层。
//
// 本包不持有连接、不读时钟、不引用任何第三方库。
package iec104

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
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
// ASDU 类型与传送原因
// ---------------------------------------------------------------------------

// ASDU 类型标识（只列本平台实现或明确拒绝的类型）。
const (
	TypeSinglePoint            byte = 1   // M_SP_NA_1
	TypeDoublePoint            byte = 3   // M_DP_NA_1
	TypeMeasuredNormalized     byte = 9   // M_ME_NA_1
	TypeMeasuredScaled         byte = 11  // M_ME_NB_1
	TypeMeasuredFloat          byte = 13  // M_ME_NC_1
	TypeIntegratedTotals       byte = 15  // M_IT_NA_1
	TypeSinglePointTime        byte = 30  // M_SP_TB_1
	TypeDoublePointTime        byte = 31  // M_DP_TB_1
	TypeMeasuredNormalizedTime byte = 34  // M_ME_TD_1
	TypeMeasuredScaledTime     byte = 35  // M_ME_TE_1
	TypeMeasuredFloatTime      byte = 36  // M_ME_TF_1
	TypeSingleCommand          byte = 45  // C_SC_NA_1
	TypeDoubleCommand          byte = 46  // C_DC_NA_1
	TypeSetpointNormalized     byte = 48  // C_SE_NA_1
	TypeSetpointScaled         byte = 49  // C_SE_NB_1
	TypeSetpointFloat          byte = 50  // C_SE_NC_1
	TypeEndOfInit              byte = 70  // M_EI_NA_1
	TypeInterrogation          byte = 100 // C_IC_NA_1
	TypeCounterInterrogation   byte = 101 // C_CI_NA_1
	TypeClockSync              byte = 103 // C_CS_NA_1
)

// 传送原因（低 6 位）。高两位分别是 P/N（否定确认）与 T（试验）。
const (
	CausePeriodic          byte = 1
	CauseBackground        byte = 2
	CauseSpontaneous       byte = 3
	CauseInitialized       byte = 4
	CauseRequest           byte = 5
	CauseActivation        byte = 6
	CauseActivationCon     byte = 7
	CauseDeactivation      byte = 8
	CauseDeactivationCon   byte = 9
	CauseActivationTerm    byte = 10
	CauseInterrogated      byte = 20
	CauseGroup1            byte = 21
	CauseUnknownType       byte = 44
	CauseUnknownCause      byte = 45
	CauseUnknownCommonAddr byte = 46
	CauseUnknownIOA        byte = 47
)

// 总召唤限定词：20 表示站召唤，21..36 表示第 1..16 组。
const (
	QOIStation byte = 20
)

// 品质位（QDS 的位序）。单点/双点信息里的品质位整体左移 3 位，
// 因为低 4 位被状态值与保留位占用。
const (
	QOverflow    Quality = 1 << 0
	QBlocked     Quality = 1 << 1
	QSubstituted Quality = 1 << 2
	QNotTopical  Quality = 1 << 3
	QInvalid     Quality = 1 << 4

	// siqQualityMask 是单点/双点信息里真正存在的品质位。
	siqQualityMask = QBlocked | QSubstituted | QNotTopical | QInvalid
)

// Quality 是信息对象品质的归一化位集合。
type Quality uint8

// Has 判断某一位是否置位。
func (q Quality) Has(bit Quality) bool { return q&bit != 0 }

// String 返回品质位的可读形式，用于日志（空表示 good）。
func (q Quality) String() string {
	if q == 0 {
		return "good"
	}
	var parts []string
	for _, p := range []struct {
		bit  Quality
		name string
	}{
		{QOverflow, "overflow"},
		{QBlocked, "blocked"},
		{QSubstituted, "substituted"},
		{QNotTopical, "not-topical"},
		{QInvalid, "invalid"},
	} {
		if q.Has(p.bit) {
			parts = append(parts, p.name)
		}
	}
	return strings.Join(parts, ",")
}

// decodeSIQQuality 从 SIQ/DIQ 字节里取品质位。
func decodeSIQQuality(b byte) Quality {
	return Quality((b >> 3) & byte(siqQualityMask))
}

// encodeSIQQuality 把归一化品质位写进 SIQ/DIQ 字节的高 4 位。
func encodeSIQQuality(q Quality) byte {
	return byte(q&siqQualityMask) << 3
}

// ---------------------------------------------------------------------------
// 信息对象与 ASDU
// ---------------------------------------------------------------------------

// Object 是一个信息对象。字段按类型取用：
//
//	单点       Bool / Quality / Time
//	双点       Uint（0..3）/ Quality / Time
//	归一化     Num（-1..1）/ Quality / Time
//	标度化     Num（int16）/ Quality / Time
//	短浮点     Num / Quality / Time
//	累计量     Uint / Raw（序号字节）
//	命令       Bool 或 Uint 或 Num / Select / Qualifier
//	召唤/同步  Qualifier 或 Time
type Object struct {
	IOA       uint32
	Bool      bool
	Uint      uint32
	Num       float64
	Quality   Quality
	Time      *time.Time
	Select    bool
	Qualifier byte
	Raw       byte
}

// ASDU 是一条应用服务数据单元。
type ASDU struct {
	TypeID     byte
	SQ         bool // 可变结构限定词的最高位：后续对象 IOA 连续递增
	Cause      byte // 低 6 位
	Negative   bool // P/N：否定确认
	Test       bool // T：试验
	Originator byte
	CommonAddr uint16
	Objects    []Object
}

// String 返回便于日志阅读的摘要。
func (a ASDU) String() string {
	return fmt.Sprintf("%s cause=%d%s ca=%d objects=%d",
		TypeName(a.TypeID), a.Cause, map[bool]string{true: ",negative", false: ""}[a.Negative],
		a.CommonAddr, len(a.Objects))
}

// ParseASDU 解析一条完整 ASDU。
//
// 对未知类型返回错误：本平台没有该类型的元素长度，继续按「猜」解析会
// 把后续字段读成垃圾。调用方（driver）应当丢弃整帧——APDU 的长度字段
// 已经保证帧边界，丢弃一条不认识的 ASDU 不会破坏后续帧。
func ParseASDU(b []byte) (ASDU, error) {
	if len(b) < 6 {
		return ASDU{}, fmt.Errorf("asdu too short: %d bytes", len(b))
	}
	a := ASDU{
		TypeID:     b[0],
		SQ:         b[1]&0x80 != 0,
		Cause:      b[2] & 0x3F,
		Negative:   b[2]&0x40 != 0,
		Test:       b[2]&0x80 != 0,
		Originator: b[3],
		CommonAddr: binary.LittleEndian.Uint16(b[4:6]),
	}
	count := int(b[1] & 0x7F)
	rest := b[6:]
	if count == 0 {
		if len(rest) != 0 {
			return ASDU{}, fmt.Errorf("asdu %s: %d trailing bytes without information objects", TypeName(a.TypeID), len(rest))
		}
		return a, nil
	}
	if _, ok := ElementSize(a.TypeID); !ok {
		return ASDU{}, fmt.Errorf("asdu type %d (%s) is not implemented", a.TypeID, TypeName(a.TypeID))
	}

	var ioa uint32
	a.Objects = make([]Object, 0, count)
	for i := 0; i < count; i++ {
		if a.SQ && i > 0 {
			ioa++
		} else {
			if len(rest) < 3 {
				return ASDU{}, fmt.Errorf("asdu %s: truncated information object address", TypeName(a.TypeID))
			}
			ioa = uint32(rest[0]) | uint32(rest[1])<<8 | uint32(rest[2])<<16
			rest = rest[3:]
		}
		o, n, err := decodeElement(a.TypeID, rest)
		if err != nil {
			return ASDU{}, fmt.Errorf("asdu %s ioa %d: %w", TypeName(a.TypeID), ioa, err)
		}
		o.IOA = ioa
		a.Objects = append(a.Objects, o)
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return ASDU{}, fmt.Errorf("asdu %s: %d trailing bytes after %d objects", TypeName(a.TypeID), len(rest), count)
	}
	return a, nil
}

// BuildASDU 编码一条 ASDU。
func BuildASDU(a ASDU) ([]byte, error) {
	if a.Cause > 0x3F {
		return nil, fmt.Errorf("cause %d exceeds 6 bits", a.Cause)
	}
	if len(a.Objects) > 127 {
		return nil, fmt.Errorf("too many information objects: %d (max 127)", len(a.Objects))
	}
	if _, ok := ElementSize(a.TypeID); !ok {
		return nil, fmt.Errorf("asdu type %d (%s) is not implemented", a.TypeID, TypeName(a.TypeID))
	}
	if a.SQ {
		for i := 1; i < len(a.Objects); i++ {
			if a.Objects[i].IOA != a.Objects[0].IOA+uint32(i) {
				return nil, fmt.Errorf("SQ=1 requires consecutive IOAs: object %d has %d", i, a.Objects[i].IOA)
			}
		}
	}

	b := make([]byte, 0, 6+len(a.Objects)*12)
	b = append(b, a.TypeID)
	vsq := byte(len(a.Objects))
	if a.SQ {
		vsq |= 0x80
	}
	b = append(b, vsq)
	cot := a.Cause
	if a.Negative {
		cot |= 0x40
	}
	if a.Test {
		cot |= 0x80
	}
	b = append(b, cot, a.Originator)
	b = binary.LittleEndian.AppendUint16(b, a.CommonAddr)
	for i, o := range a.Objects {
		if o.IOA > 0xFFFFFF {
			return nil, fmt.Errorf("information object address %d exceeds 24 bits", o.IOA)
		}
		if !a.SQ || i == 0 {
			b = append(b, byte(o.IOA), byte(o.IOA>>8), byte(o.IOA>>16))
		}
		var err error
		b, err = appendElement(b, a.TypeID, o)
		if err != nil {
			return nil, err
		}
	}
	if len(b) > maxASDU {
		return nil, fmt.Errorf("asdu too large: %d bytes (max %d)", len(b), maxASDU)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// 元素编解码
// ---------------------------------------------------------------------------

// ElementSize 返回一个信息对象的字节数（不含 3 字节 IOA）。时间标签已计入。
func ElementSize(typeID byte) (int, bool) {
	switch typeID {
	case TypeSinglePoint, TypeDoublePoint, TypeEndOfInit,
		TypeSingleCommand, TypeDoubleCommand,
		TypeInterrogation, TypeCounterInterrogation:
		return 1, true
	case TypeMeasuredNormalized, TypeMeasuredScaled, TypeSetpointNormalized, TypeSetpointScaled:
		return 3, true
	case TypeMeasuredFloat, TypeIntegratedTotals, TypeSetpointFloat:
		return 5, true
	case TypeSinglePointTime, TypeDoublePointTime:
		return 8, true
	case TypeClockSync:
		return 7, true
	case TypeMeasuredNormalizedTime, TypeMeasuredScaledTime:
		return 10, true
	case TypeMeasuredFloatTime:
		return 12, true
	}
	return 0, false
}

// IsCommandType 表示该 ASDU 是控制方向（主站 → 从站）。
func IsCommandType(typeID byte) bool {
	switch typeID {
	case TypeSingleCommand, TypeDoubleCommand,
		TypeSetpointNormalized, TypeSetpointScaled, TypeSetpointFloat,
		TypeInterrogation, TypeCounterInterrogation, TypeClockSync:
		return true
	}
	return false
}

// IsData 表示该 ASDU 携带过程值（从站 → 主站）。
func IsData(typeID byte) bool {
	switch typeID {
	case TypeSinglePoint, TypeDoublePoint,
		TypeMeasuredNormalized, TypeMeasuredScaled, TypeMeasuredFloat,
		TypeIntegratedTotals,
		TypeSinglePointTime, TypeDoublePointTime,
		TypeMeasuredNormalizedTime, TypeMeasuredScaledTime, TypeMeasuredFloatTime:
		return true
	}
	return false
}

// IsTimeTagged 表示该数据类型的元素自带 7 字节时标。
func IsTimeTagged(typeID byte) bool {
	switch typeID {
	case TypeSinglePointTime, TypeDoublePointTime,
		TypeMeasuredNormalizedTime, TypeMeasuredScaledTime, TypeMeasuredFloatTime:
		return true
	}
	return false
}

func decodeElement(typeID byte, b []byte) (Object, int, error) {
	size, ok := ElementSize(typeID)
	if !ok {
		return Object{}, 0, fmt.Errorf("unsupported type %d", typeID)
	}
	if len(b) < size {
		return Object{}, 0, fmt.Errorf("truncated element: %d bytes, want %d", len(b), size)
	}
	switch typeID {
	case TypeSinglePoint:
		return Object{Bool: b[0]&0x01 != 0, Quality: decodeSIQQuality(b[0])}, size, nil
	case TypeDoublePoint:
		return Object{Uint: uint32(b[0] & 0x03), Quality: decodeSIQQuality(b[0])}, size, nil
	case TypeSinglePointTime:
		return Object{
			Bool: b[0]&0x01 != 0, Quality: decodeSIQQuality(b[0]),
			Time: decodeCP56Time2a(b[1:8]),
		}, size, nil
	case TypeDoublePointTime:
		return Object{
			Uint: uint32(b[0] & 0x03), Quality: decodeSIQQuality(b[0]),
			Time: decodeCP56Time2a(b[1:8]),
		}, size, nil
	case TypeMeasuredNormalized:
		return Object{Num: normalized(b[0:2]), Quality: Quality(b[2] & 0x1F)}, size, nil
	case TypeMeasuredScaled:
		return Object{Num: float64(int16(binary.LittleEndian.Uint16(b[0:2]))), Quality: Quality(b[2] & 0x1F)}, size, nil
	case TypeMeasuredFloat:
		return Object{Num: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4]))), Quality: Quality(b[4] & 0x1F)}, size, nil
	case TypeMeasuredNormalizedTime:
		return Object{
			Num: normalized(b[0:2]), Quality: Quality(b[2] & 0x1F),
			Time: decodeCP56Time2a(b[3:10]),
		}, size, nil
	case TypeMeasuredScaledTime:
		return Object{
			Num: float64(int16(binary.LittleEndian.Uint16(b[0:2]))), Quality: Quality(b[2] & 0x1F),
			Time: decodeCP56Time2a(b[3:10]),
		}, size, nil
	case TypeMeasuredFloatTime:
		return Object{
			Num: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4]))), Quality: Quality(b[4] & 0x1F),
			Time: decodeCP56Time2a(b[5:12]),
		}, size, nil
	case TypeIntegratedTotals:
		return Object{Uint: binary.LittleEndian.Uint32(b[0:4]), Raw: b[4]}, size, nil
	case TypeSingleCommand:
		return Object{Bool: b[0]&0x01 != 0, Select: b[0]&0x80 != 0, Qualifier: (b[0] >> 2) & 0x1F}, size, nil
	case TypeDoubleCommand:
		return Object{Uint: uint32(b[0] & 0x03), Select: b[0]&0x80 != 0, Qualifier: (b[0] >> 2) & 0x1F}, size, nil
	case TypeSetpointNormalized:
		return Object{Num: normalized(b[0:2]), Select: b[2]&0x80 != 0, Qualifier: b[2] & 0x7F}, size, nil
	case TypeSetpointScaled:
		return Object{Num: float64(int16(binary.LittleEndian.Uint16(b[0:2]))), Select: b[2]&0x80 != 0, Qualifier: b[2] & 0x7F}, size, nil
	case TypeSetpointFloat:
		return Object{
			Num:       float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4]))),
			Select:    b[4]&0x80 != 0,
			Qualifier: b[4] & 0x7F,
		}, size, nil
	case TypeEndOfInit, TypeInterrogation, TypeCounterInterrogation:
		return Object{Qualifier: b[0]}, size, nil
	case TypeClockSync:
		return Object{Time: decodeCP56Time2a(b[0:7])}, size, nil
	}
	return Object{}, 0, fmt.Errorf("unsupported type %d", typeID)
}

func appendElement(b []byte, typeID byte, o Object) ([]byte, error) {
	switch typeID {
	case TypeSinglePoint:
		var siq byte
		if o.Bool {
			siq = 1
		}
		return append(b, siq|encodeSIQQuality(o.Quality)), nil
	case TypeSinglePointTime:
		var siq byte
		if o.Bool {
			siq = 1
		}
		b = append(b, siq|encodeSIQQuality(o.Quality))
		return append(b, encodeCP56Time2a(o.Time)...), nil
	case TypeDoublePoint:
		if o.Uint > 3 {
			return nil, fmt.Errorf("double point value %d exceeds 2 bits", o.Uint)
		}
		return append(b, byte(o.Uint)|encodeSIQQuality(o.Quality)), nil
	case TypeDoublePointTime:
		if o.Uint > 3 {
			return nil, fmt.Errorf("double point value %d exceeds 2 bits", o.Uint)
		}
		b = append(b, byte(o.Uint)|encodeSIQQuality(o.Quality))
		return append(b, encodeCP56Time2a(o.Time)...), nil
	case TypeMeasuredNormalized, TypeMeasuredNormalizedTime:
		b = binary.LittleEndian.AppendUint16(b, encodeNormalized(o.Num))
		b = append(b, byte(o.Quality&0x1F))
		if typeID == TypeMeasuredNormalizedTime {
			b = append(b, encodeCP56Time2a(o.Time)...)
		}
		return b, nil
	case TypeMeasuredScaled, TypeMeasuredScaledTime:
		if o.Num < math.MinInt16 || o.Num > math.MaxInt16 {
			return nil, fmt.Errorf("scaled value %v exceeds int16", o.Num)
		}
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(math.Round(o.Num))))
		b = append(b, byte(o.Quality&0x1F))
		if typeID == TypeMeasuredScaledTime {
			b = append(b, encodeCP56Time2a(o.Time)...)
		}
		return b, nil
	case TypeMeasuredFloat, TypeMeasuredFloatTime:
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(o.Num)))
		b = append(b, byte(o.Quality&0x1F))
		if typeID == TypeMeasuredFloatTime {
			b = append(b, encodeCP56Time2a(o.Time)...)
		}
		return b, nil
	case TypeIntegratedTotals:
		b = binary.LittleEndian.AppendUint32(b, o.Uint)
		return append(b, o.Raw), nil
	case TypeSingleCommand:
		var v byte
		if o.Bool {
			v = 1
		}
		if o.Select {
			v |= 0x80
		}
		return append(b, v|(o.Qualifier&0x1F)<<2), nil
	case TypeDoubleCommand:
		if o.Uint > 3 {
			return nil, fmt.Errorf("double command value %d exceeds 2 bits", o.Uint)
		}
		v := byte(o.Uint) | (o.Qualifier&0x1F)<<2
		if o.Select {
			v |= 0x80
		}
		return append(b, v), nil
	case TypeSetpointNormalized, TypeSetpointScaled:
		if typeID == TypeSetpointNormalized {
			if o.Num < -1 || o.Num > 1 {
				return nil, fmt.Errorf("normalized setpoint %v out of range -1..1", o.Num)
			}
			b = binary.LittleEndian.AppendUint16(b, encodeNormalized(o.Num))
		} else {
			if o.Num < math.MinInt16 || o.Num > math.MaxInt16 {
				return nil, fmt.Errorf("scaled setpoint %v exceeds int16", o.Num)
			}
			b = binary.LittleEndian.AppendUint16(b, uint16(int16(math.Round(o.Num))))
		}
		v := o.Qualifier & 0x7F
		if o.Select {
			v |= 0x80
		}
		return append(b, v), nil
	case TypeSetpointFloat:
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(o.Num)))
		v := o.Qualifier & 0x7F
		if o.Select {
			v |= 0x80
		}
		return append(b, v), nil
	case TypeEndOfInit, TypeInterrogation, TypeCounterInterrogation:
		return append(b, o.Qualifier), nil
	case TypeClockSync:
		return append(b, encodeCP56Time2a(o.Time)...), nil
	}
	return nil, fmt.Errorf("unsupported type %d", typeID)
}

func normalized(b []byte) float64 {
	return float64(int16(binary.LittleEndian.Uint16(b))) / 32768
}

func encodeNormalized(v float64) uint16 {
	if v > 1 {
		v = 1
	}
	if v < -1 {
		v = -1
	}
	return uint16(int16(math.Round(v * 32767)))
}

// ---------------------------------------------------------------------------
// 时标
// ---------------------------------------------------------------------------

// EncodeCP56Time2a 编码 7 字节时标（毫秒小端 + 分 + 时 + 日 + 月 + 年）。
func EncodeCP56Time2a(t time.Time) []byte {
	b := make([]byte, 7)
	ms := t.Second()*1000 + t.Nanosecond()/int(time.Millisecond)
	b[0] = byte(ms)
	b[1] = byte(ms >> 8)
	b[2] = byte(t.Minute())
	b[3] = byte(t.Hour())
	dow := int(t.Weekday())
	if dow == 0 {
		dow = 7 // Go 的周日是 0，标准里是 7
	}
	b[4] = byte(t.Day()) | byte(dow<<5)
	b[5] = byte(t.Month())
	b[6] = byte(t.Year() % 100)
	return b
}

// DecodeCP56Time2a 解码时标。第二个返回值为 false 表示时标无效（IV 位置位），
// 此时时间值不可信，调用方应当把该对象当作「没有时标」而不是 1970 年。
func DecodeCP56Time2a(b []byte) (time.Time, bool) {
	t := decodeCP56Time2a(b)
	if t == nil {
		return time.Time{}, false
	}
	return *t, true
}

func decodeCP56Time2a(b []byte) *time.Time {
	if len(b) < 7 {
		return nil
	}
	if b[2]&0x80 != 0 { // 分钟字节的 IV 位
		return nil
	}
	ms := int(b[0]) | int(b[1])<<8
	if ms > 59999 {
		return nil
	}
	minute := int(b[2] & 0x3F)
	hour := int(b[3] & 0x1F)
	day := int(b[4] & 0x1F)
	month := int(b[5] & 0x0F)
	year := 2000 + int(b[6])
	if minute > 59 || hour > 23 || day < 1 || day > 31 || month < 1 || month > 12 {
		return nil
	}
	t := time.Date(year, time.Month(month), day, hour, minute,
		ms/1000, (ms%1000)*int(time.Millisecond), time.Local)
	return &t
}

func encodeCP56Time2a(t *time.Time) []byte {
	if t == nil {
		b := make([]byte, 7)
		b[2] = 0x80 // IV
		return b
	}
	return EncodeCP56Time2a(*t)
}

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// BuildInterrogation 构造总召唤（站召唤）。
func BuildInterrogation(ca uint16, qoi byte) ([]byte, error) {
	if qoi < 20 || qoi > 36 {
		return nil, fmt.Errorf("QOI %d out of range 20..36", qoi)
	}
	return BuildASDU(ASDU{
		TypeID: TypeInterrogation, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{Qualifier: qoi}},
	})
}

// BuildCounterInterrogation 构造电能量召唤。
func BuildCounterInterrogation(ca uint16, qcc byte) ([]byte, error) {
	return BuildASDU(ASDU{
		TypeID: TypeCounterInterrogation, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{Qualifier: qcc}},
	})
}

// BuildClockSync 构造时钟同步。
func BuildClockSync(ca uint16, t time.Time) ([]byte, error) {
	return BuildASDU(ASDU{
		TypeID: TypeClockSync, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{Time: &t}},
	})
}

// BuildSingleCommand 构造单点命令。select=true 时只选择不执行。
func BuildSingleCommand(ca uint16, ioa uint32, on, selectFlag bool) ([]byte, error) {
	return BuildASDU(ASDU{
		TypeID: TypeSingleCommand, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{IOA: ioa, Bool: on, Select: selectFlag}},
	})
}

// BuildDoubleCommand 构造双点命令。state 取 1（分）或 2（合）。
func BuildDoubleCommand(ca uint16, ioa uint32, state byte, selectFlag bool) ([]byte, error) {
	if state != 1 && state != 2 {
		return nil, fmt.Errorf("double command state must be 1 (off) or 2 (on), got %d", state)
	}
	return BuildASDU(ASDU{
		TypeID: TypeDoubleCommand, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{IOA: ioa, Uint: uint32(state), Select: selectFlag}},
	})
}

// BuildSetpointNormalized 构造归一化设定值命令。
func BuildSetpointNormalized(ca uint16, ioa uint32, v float64, selectFlag bool) ([]byte, error) {
	if v < -1 || v > 1 {
		return nil, fmt.Errorf("normalized setpoint %v out of range -1..1", v)
	}
	return BuildASDU(ASDU{
		TypeID: TypeSetpointNormalized, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{IOA: ioa, Num: v, Select: selectFlag}},
	})
}

// BuildSetpointScaled 构造标度化设定值命令。
func BuildSetpointScaled(ca uint16, ioa uint32, v int16, selectFlag bool) ([]byte, error) {
	return BuildASDU(ASDU{
		TypeID: TypeSetpointScaled, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{IOA: ioa, Num: float64(v), Select: selectFlag}},
	})
}

// BuildSetpointFloat 构造短浮点设定值命令。
func BuildSetpointFloat(ca uint16, ioa uint32, v float32, selectFlag bool) ([]byte, error) {
	return BuildASDU(ASDU{
		TypeID: TypeSetpointFloat, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{IOA: ioa, Num: float64(v), Select: selectFlag}},
	})
}
