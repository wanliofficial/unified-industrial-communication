// Package bacnet 是 BACnet/IP（Annex J，UDP 47808）的纯函数编解码层。
//
// 只做「报文进、报文出」：BVLC → NPDU → APDU 三层逐段解析与构造，加上 BACnet
// 标签（tag）与应用值编解码。没有 I/O、没有时钟、没有可变状态，因此可以直接
// 与真实设备对拍，也可以在测试里逐字节核对。
//
// 覆盖范围是刻意的：只实现 ReadProperty（0x0C）与 WriteProperty（0x0F）两类
// 服务，以及它们的应答（ComplexACK / SimpleACK / Error / Reject / Abort）。
// Who-Is/I-Am 广播发现不做（网关只需要点名读一台设备，广播发现在多网段场景
// 下不可控）。COV 订阅不做——它是另一套「设备主动推」的模型，与本平台
// 的调度式采集语义不符。
package bacnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ---------------------------------------------------------------------------
// 常量：BVLC / NPDU / APDU / 服务 / 对象类型 / 属性
// ---------------------------------------------------------------------------

const (
	// BVLC（BACnet Virtual Link Control）功能码。我们只处理两种：
	// Original-Unicast（0x0A，正常请求/应答）与 Forwarded-NPDU（0x0B，
	// BBMD 转发）。收到 Forwarded-NPDU 时它内部仍是一条 NPDU，只是外层
	// 多了 4 字节转发信息，需要跳过而不是报错。
	BVLCOriginalUnicast = 0x0A
	BVLCForwardedNPDU   = 0x0B

	// NPDU control 字段：bit7=网络层报文，bit5=有目的端，bit3=有源端，
	// bit2=期望应答，bit1=高优先级，bit0=保留。我们只发「期望应答、无
	// 路由」这一种，固定 0x04；应答帧用 0x00。
	NPDUControlExpectReply = 0x04
	NPDUControlNone        = 0x00

	// APDU 类型（首字节高 4 位，ASHRAE 135 §20.1）。
	//
	// 这四个 ACK 型取值不能「看起来差不多就够」：SimpleACK=0x20、
	// ComplexACK=0x30、SegmentACK=0x40 是标准固定值，错一位在回环里看
	// 不出来（两端用同一份常量），对任何标准设备/客户端第一笔交互就断。
	APDUConfirmedRequest   = 0x00 // 0000
	APDUUnconfirmedRequest = 0x10 // 0001
	APDUSimpleACK          = 0x20 // 0010
	APDUComplexACK         = 0x30 // 0011
	APDUSegmentACK         = 0x40 // 0100（不做分段，解析时明确报「不支持」）
	APDUError              = 0x50 // 0101
	APDUReject             = 0x60 // 0110
	APDUAbort              = 0x70 // 0111

	// Reject 原因（ASHRAE 135 §20.6）。与 Abort 是两张不同的表，不能复用：
	// 同一个数值在两边语义不同（Abort 的 2=invalid-APDU-in-this-state）。
	RejectReasonBufferOverflow           = 0x01
	RejectReasonInconsistentParameters   = 0x02
	RejectReasonInvalidParameterDatatype = 0x03
	RejectReasonInvalidTag               = 0x04
	RejectReasonTooManyArguments         = 0x05
	RejectReasonUndefinedEnumeration     = 0x06
	RejectReasonUnrecognizedService      = 0x08

	// Abort 原因（ASHRAE 135 §20.7）。
	AbortReasonBufferOverflow     = 0x01
	AbortReasonInvalidAPDUInState = 0x02
	AbortReasonServerTimeout      = 0x03
	AbortReasonSegmentationNotSup = 0x04
	AbortReasonSecurityError      = 0x05
	AbortReasonOther              = 0x06

	// 服务选择码（ConfirmedRequest 的 serviceChoice，以及应答里的 serviceChoice）。
	ServiceReadProperty  = 0x0C
	ServiceWriteProperty = 0x0F

	// DefaultPort 是 BACnet/IP 的著名端口。现场几乎不会改，因此只在
	// 地址缺端口时补上；显式写端口仍然尊重配置（用于跨 NAT 或调试）。
	DefaultPort = 47808
)

// 应用标签（BACnet application tag，即「类型 + 长度」编码里的 tag number）。
const (
	TagNull           = 0
	TagBoolean        = 1
	TagUnsigned       = 2
	TagSigned         = 3
	TagReal           = 4
	TagDouble         = 5
	TagOctetString    = 6
	TagCharacterStr   = 7
	TagBitString      = 8
	TagEnumerated     = 9
	TagDate           = 10
	TagTime           = 11
	TagObjectID       = 12
	TagReserved       = 13
	TagNullExplicit   = 13
	TagAny            = 14
	TagCharacterStrMB = 15
)

// 标签头低 3 位（LVT）的取值。同一个数值在 application 与 context 两种
// 标签里的含义并不相同，这是 BACnet 编码最容易写错的一处：
//
//	LVT   application tag            context tag
//	0-4   负载长度 0..4              负载长度 0..4
//	5     扩展长度，后续 1 字节       扩展长度，后续 1 字节
//	6     扩展长度，后续 2 字节       opening tag（无负载）
//	7     扩展长度，后续 4 字节       closing tag（无负载）
//
// 换句话说：application 标签永远用 LVT 表示长度；context 标签只有 0..5
// 表示长度，6/7 被 opening/closing 占用。
const (
	lvtExt1     = 5 // 后续 1 字节给出长度（两种标签通用）
	lvtExt2     = 6 // application：后续 2 字节长度；context：opening
	lvtExt4     = 7 // application：后续 4 字节长度；context：closing
	lvtCtxOpen  = 6 // context opening tag
	lvtCtxClose = 7 // context closing tag
)

// 对象类型（object type）。覆盖楼宇/工控最常见的模拟量、二进制、多状态量，
// 加上 device（用于 Probe 时读设备名）。
const (
	ObjectAnalogInput       = 0
	ObjectAnalogOutput      = 1
	ObjectAnalogValue       = 2
	ObjectBinaryInput       = 3
	ObjectBinaryOutput      = 4
	ObjectBinaryValue       = 5
	ObjectCalendar          = 6
	ObjectCommand           = 7
	ObjectDevice            = 8
	ObjectEventEnrollment   = 9
	ObjectFile              = 10
	ObjectGroup             = 11
	ObjectLoop              = 12
	ObjectMultiStateInput   = 13
	ObjectMultiStateValue   = 14
	ObjectNotificationClass = 15
	ObjectProgram           = 16
	ObjectSchedule          = 17
	ObjectMultiStateOutput  = 18
	ObjectTrend             = 20
	ObjectCharacterString   = 40
)

// 属性标识符（property identifier）。只收录 Read/Write 会用到的。
const (
	PropAckedTransitions               = 0
	PropAckRequired                    = 1
	PropAction                         = 2
	PropActionText                     = 3
	PropActiveText                     = 4
	PropActiveVtSessions               = 5
	PropAlarmValue                     = 6
	PropAlarmValues                    = 7
	PropAlterationsOnly                = 8
	PropAplicationPriority             = 9
	PropArchive                        = 10
	PropBias                           = 11
	PropChangeOfStateCount             = 12
	PropChangeOfStateTime              = 13
	PropNotificationClass              = 14
	PropControlledVariableReference    = 15
	PropControlledVariableUnits        = 16
	PropControlledVariableValue        = 17
	PropCovIncrement                   = 18
	PropDatelist                       = 19
	PropDaylightSavingsStatus          = 20
	PropDeadband                       = 21
	PropDerivativeConstant             = 22
	PropDerivativeConstantUnits        = 23
	PropDescription                    = 28
	PropDescriptionOfHalt              = 29
	PropDeviceAddressBinding           = 30
	PropDeviceType                     = 31
	PropEffectivePeriod                = 32
	PropElapsedActiveTime              = 33
	PropErrorLimit                     = 34
	PropEventEnable                    = 35
	PropEventState                     = 36
	PropEventType                      = 37
	PropExceptionSchedule              = 38
	PropFaultValues                    = 39
	PropFeedbackValue                  = 40
	PropFileAccessMethod               = 41
	PropFileSize                       = 42
	PropFileType                       = 43
	PropFirmwareRevision               = 44
	PropHighLimit                      = 45
	PropInactiveText                   = 46
	PropInProcess                      = 47
	PropInstanceOfAny                  = 48
	PropIntegralConstant               = 49
	PropIntegralConstantUnits          = 50
	PropLimitEnable                    = 51
	PropListOfObjectPropertyReferences = 52
	PropLocalDate                      = 53
	PropLocalTime                      = 54
	PropLocation                       = 55
	PropLowLimit                       = 56
	PropManipulatedVariableReference   = 57
	PropMaximumOutput                  = 58
	PropMaxInfoFrames                  = 59
	PropMaxMaster                      = 60
	PropMaxPresValue                   = 61
	PropMinimumOffTime                 = 62
	PropMinimumOnTime                  = 63
	PropMinimumOutput                  = 64
	PropModelName                      = 70
	PropModificationDate               = 71
	PropNotifyType                     = 72
	PropNumberOfApduRetries            = 73
	PropNumberOfStates                 = 74
	PropObjectIdentifier               = 75
	PropObjectName                     = 77
	PropObjectPropertyReference        = 78
	PropObjectList                     = 76
	PropObjectType                     = 79
	PropOptional                       = 80
	PropOutOfService                   = 81
	PropOutputUnits                    = 82
	PropPolarity                       = 84
	PropPresentValue                   = 85
	PropPriority                       = 86
	PropPriorityArray                  = 87
	PropProcessIdentifier              = 88
	PropProgramChange                  = 89
	PropProgramLocation                = 90
	PropProgramState                   = 91
	PropProportionalConstant           = 92
	PropProportionalConstantUnits      = 93
	PropProtocolVersion                = 98
	PropReadOnly                       = 101
	PropReliability                    = 103
	PropRelinquishDefault              = 104
	PropRequired                       = 105
	PropResolution                     = 106
	PropSegmentSupported               = 107
	PropSetpoint                       = 108
	PropSetpointReference              = 109
	PropStateText                      = 110
	PropStatusFlags                    = 111
	PropSystemStatus                   = 112
	PropTimeDelay                      = 113
	PropTimeOfActiveTimeReset          = 114
	PropTimeOfStateCountReset          = 115
	PropTimeSynchronizationInterval    = 116
	PropUnits                          = 117
	PropUpdateInterval                 = 118
	PropUtcOffset                      = 119
	PropVendorIdentifier               = 120
	PropVendorName                     = 121
)

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

// ErrTruncated 表示报文在声称的长度之前就结束了。
var ErrTruncated = errors.New("bacnet: truncated")

// ErrProtocol 表示报文结构不符合 BACnet/IP（类型未知、长度不符等）。
type ErrProtocol struct{ Msg string }

func (e *ErrProtocol) Error() string { return "bacnet: " + e.Msg }

func proto(format string, args ...any) error {
	return &ErrProtocol{Msg: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// BVLC + NPDU 层
// ---------------------------------------------------------------------------

// BuildNPDU 构造 NPDU：version + control + APDU。
//
// MS/TP 帧的数据段就是 NPDU，BACnet/IP 的 BVLC 负载也是 NPDU，两者只差
// 外层封装。把它抽出来是为了让 MS/TP 与 BACnet/IP 共用同一套网络层构造，
// 而不是在 MS/TP 里再手写一遍 version/control 两个字节。
func BuildNPDU(npduVersion byte, npduControl byte, apdu []byte) []byte {
	out := make([]byte, 2+len(apdu))
	out[0] = npduVersion
	out[1] = npduControl
	copy(out[2:], apdu)
	return out
}

// ParseNPDU 解析 NPDU，跳过可选路由字段后返回 APDU 与 control 字节。
//
// 路由字段的跳过逻辑与标准一致（ASHRAE 135 §6.2.2）：control bit5 (0x20)
// 表示带目的地址，格式为 DNET(2) + DLEN(1) + DADR(DLEN，DLEN>0 时) +
// Hop Count(1)；bit3 (0x08) 表示带源地址，格式为 SNET(2) + SLEN(1) +
// SADR(SLEN，SLEN>0 时)。本平台不生成路由报文，但真实
// 设备可能经过路由器回包，因此解析侧必须能跳过而不是报错。注意 bit1
// (0x02) 是网络优先级、不是目的地址标志——按 0x02 跳过 DNET/DADR 会让
// 经路由器回包的应答整体错位。
func ParseNPDU(npdu []byte) (apdu []byte, npduControl byte, err error) {
	if len(npdu) < 2 {
		return nil, 0, proto("NPDU too short")
	}
	if npdu[0] != 1 {
		return nil, 0, proto("unsupported NPDU version %d", npdu[0])
	}
	npduControl = npdu[1]
	// NPDU 头：version(1) + control(1)，之后才是 APDU。
	apduStart := 2
	// 网络层报文（bit7）：Message Type(1) + Vendor ID(1)。
	if npduControl&0x80 != 0 {
		if len(npdu) < apduStart+2 {
			return nil, 0, proto("NPDU with network layer message truncated")
		}
		apduStart += 2
	}
	// 跳过目的地址（bit5）：DNET(2) + DLEN(1) + DADR(DLEN) + Hop Count(1)。
	// hop count 是目的规格的一部分，只要 bit5 置位就必有。
	if npduControl&0x20 != 0 {
		if len(npdu) < apduStart+3 {
			return nil, 0, proto("NPDU with destination truncated")
		}
		dlen := int(npdu[apduStart+2])
		apduStart += 3
		if dlen > 0 {
			if len(npdu) < apduStart+dlen {
				return nil, 0, proto("NPDU destination address truncated")
			}
			apduStart += dlen
		}
		if len(npdu) < apduStart+1 {
			return nil, 0, proto("NPDU hop count truncated")
		}
		apduStart += 1
	}
	// 跳过源地址（bit3）：SNET(2) + SLEN(1) + SADR(SLEN)。
	if npduControl&0x08 != 0 {
		if len(npdu) < apduStart+3 {
			return nil, 0, proto("NPDU with source route truncated")
		}
		slen := int(npdu[apduStart+2])
		apduStart += 3
		if slen > 0 {
			if len(npdu) < apduStart+slen {
				return nil, 0, proto("NPDU source address truncated")
			}
			apduStart += slen
		}
	}
	if apduStart >= len(npdu) {
		return nil, 0, proto("NPDU consumed entire message, no APDU")
	}
	return npdu[apduStart:], npduControl, nil
}

// BuildMessage 构造一条完整的 BACnet/IP 报文：BVLC(4) + NPDU(2..) + APDU。
//
// BVLC 的长度字段（大端 2 字节）只算 NPDU + APDU，不含 BVLC 自身的 4 字节头。
// 这是 BACnet/IP 与很多其它协议「长度含头」习惯相反的一点，最容易写错。
func BuildMessage(npduVersion byte, npduControl byte, apdu []byte) []byte {
	body := BuildNPDU(npduVersion, npduControl, apdu)
	msg := make([]byte, 4+len(body))
	msg[0] = 0x81 // BVLC 类型
	msg[1] = BVLCOriginalUnicast
	binary.BigEndian.PutUint16(msg[2:4], uint16(len(body)))
	copy(msg[4:], body)
	return msg
}

// ParseMessage 剥离 BVLC 与 NPDU，返回 APDU 负载与 NPDU control 字节。
//
// 对 Forwarded-NPDU（0x0B，来自 BBMD 转发）会跳过额外的 6 字节转发信息，
// 因为内部仍是标准 NPDU；对未知功能码返回错误而不是静默丢弃。
func ParseMessage(msg []byte) (apdu []byte, npduControl byte, err error) {
	if len(msg) < 4 {
		return nil, 0, proto("BVLC header too short: %d bytes", len(msg))
	}
	if msg[0] != 0x81 {
		return nil, 0, proto("not a BACnet/IP BVLC message: type 0x%02X", msg[0])
	}
	length := int(binary.BigEndian.Uint16(msg[2:4]))
	if length+4 > len(msg) {
		return nil, 0, proto("BVLC length %d exceeds message %d", length, len(msg))
	}
	body := msg[4 : 4+length]
	switch msg[1] {
	case BVLCOriginalUnicast:
		// body 直接是 NPDU + APDU
	case BVLCForwardedNPDU:
		// 6 字节转发信息：原始终端 IP(4) + 原始终端端口(2)
		if len(body) < 6 {
			return nil, 0, proto("Forwarded-NPDU too short: %d", len(body))
		}
		body = body[6:]
	default:
		return nil, 0, proto("unsupported BVLC function 0x%02X", msg[1])
	}
	return ParseNPDU(body)
}

// ---------------------------------------------------------------------------
// 标签（tag）编解码
// ---------------------------------------------------------------------------

// tagHeader 描述一个 BACnet 标签的头部：tag number、class（application 还是
// context）、LVT，以及由 LVT 解析出来的长度。
type tagHeader struct {
	tagNumber byte
	context   bool // true = context-specific tag
	lvt       byte // length/value type（原始低 3 位）
	opening   bool // context opening tag（LVT=6）
	closing   bool // context closing tag（LVT=7）
	// 内联值（仅 application 布尔有意义）。
	boolValue bool
	length    int
}

// encodeAppTagHeader 编码一个 application 标签头（含扩展长度字节）。
//
// application 标签的 LVT 永远表示长度：0..4 内联，5/6/7 分别跟 1/2/4 字节
// 的扩展长度。布尔是唯一例外，它的「长度位」直接承载值（0/1），且没有负载。
func encodeAppTagHeader(tagNumber byte, length int) []byte {
	first := (tagNumber << 4) & 0xF0
	switch {
	case length >= 0 && length <= 4:
		return []byte{first | byte(length)}
	case length <= 0xFF:
		return []byte{first | lvtExt1, byte(length)}
	case length <= 0xFFFF:
		return []byte{first | lvtExt2, byte(length >> 8), byte(length)}
	default:
		b := []byte{first | lvtExt4, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(length))
		return b
	}
}

// encodeCtxTagHeader 编码一个 context 标签头。LVT=6/7 被 opening/closing
// 占用，因此长度只能走 0..4 内联或 LVT=5 的单字节扩展长度；超过 255 字节的
// context 负载在 BACnet 里必须分段，本实现不支持，返回 false 让调用方报错。
func encodeCtxTagHeader(tagNumber byte, length int) ([]byte, bool) {
	first := 0x08 | ((tagNumber << 4) & 0xF0)
	switch {
	case length >= 0 && length <= 4:
		return []byte{first | byte(length)}, true
	case length <= 0xFF:
		return []byte{first | lvtExt1, byte(length)}, true
	default:
		return nil, false
	}
}

// parseTagHeader 解析一个标签头部，返回头部与负载起始偏移（相对 buf）。
func parseTagHeader(buf []byte) (tagHeader, int, error) {
	if len(buf) < 1 {
		return tagHeader{}, 0, ErrTruncated
	}
	first := buf[0]
	h := tagHeader{
		tagNumber: (first >> 4) & 0x0F,
		context:   first&0x08 != 0,
		lvt:       first & 0x07,
	}
	off := 1

	if !h.context {
		// application 标签。
		if h.tagNumber == TagBoolean {
			h.boolValue = h.lvt == 1
			return h, off, nil
		}
		switch {
		case h.lvt <= 4:
			h.length = int(h.lvt)
		case h.lvt == lvtExt1:
			if len(buf) < off+1 {
				return tagHeader{}, 0, ErrTruncated
			}
			h.length = int(buf[off])
			off++
		case h.lvt == lvtExt2:
			if len(buf) < off+2 {
				return tagHeader{}, 0, ErrTruncated
			}
			h.length = int(binary.BigEndian.Uint16(buf[off : off+2]))
			off += 2
		case h.lvt == lvtExt4:
			if len(buf) < off+4 {
				return tagHeader{}, 0, ErrTruncated
			}
			h.length = int(binary.BigEndian.Uint32(buf[off : off+4]))
			off += 4
		}
		return h, off, nil
	}

	// context 标签：6/7 是 opening/closing。
	switch {
	case h.lvt == lvtCtxOpen:
		h.opening = true
	case h.lvt == lvtCtxClose:
		h.closing = true
	case h.lvt <= 4:
		h.length = int(h.lvt)
	case h.lvt == lvtExt1:
		if len(buf) < off+1 {
			return tagHeader{}, 0, ErrTruncated
		}
		h.length = int(buf[off])
		off++
	}
	return h, off, nil
}

// contextTag 构造一个带负载的 context 标签（对象标识符、属性标识符、
// 属性索引等参数）。负载超过 255 字节时返回错误：BACnet 的 context 扩展
// 长度只有单字节，再长必须分段，本实现不做。
func contextTag(tagNumber byte, payload []byte) ([]byte, error) {
	head, ok := encodeCtxTagHeader(tagNumber, len(payload))
	if !ok {
		return nil, proto("context tag %d payload %d bytes exceeds 255", tagNumber, len(payload))
	}
	return append(head, payload...), nil
}

// contextOpening 构造 context opening tag（`[n]` 的构造形式，无负载）。
func contextOpening(tagNumber byte) []byte {
	return []byte{0x08 | ((tagNumber << 4) & 0xF0) | lvtCtxOpen}
}

// contextClosing 构造 context closing tag。
func contextClosing(tagNumber byte) []byte {
	return []byte{0x08 | ((tagNumber << 4) & 0xF0) | lvtCtxClose}
}

// ---------------------------------------------------------------------------
// 应用值编解码
// ---------------------------------------------------------------------------

// 应用值（application value）的类型标记，用于把平台数据类型映射到 BACnet 标签。
const (
	AppNull         = TagNull
	AppBoolean      = TagBoolean
	AppUnsigned     = TagUnsigned
	AppSigned       = TagSigned
	AppReal         = TagReal
	AppDouble       = TagDouble
	AppOctetString  = TagOctetString
	AppCharacterStr = TagCharacterStr
	AppBitString    = TagBitString
	AppEnumerated   = TagEnumerated
	AppObjectID     = TagObjectID
)

// EncodeApplicationValue 把平台值编码为 BACnet 应用值字节（tag + 负载）。
//
// 平台侧传入 tag 决定用哪种 BACnet 类型编码，避免与平台数据类型耦合：
//   - AppNull：空负载
//   - AppBoolean：值内联在标签头，无负载
//   - AppUnsigned / AppEnumerated：大端无符号整数
//   - AppSigned：大端二进制补码有符号整数
//   - AppReal：4 字节大端 IEEE754
//   - AppDouble：8 字节大端 IEEE754
//   - AppOctetString：原始字节
//   - AppCharacterStr：1 字节字符集标识（0 = ASCII）+ 文本
//   - AppObjectID：4 字节（type<<22 | instance）
func EncodeApplicationValue(tag byte, value uint64, floatVal float64, bytes []byte, boolVal bool) ([]byte, error) {
	switch tag {
	case AppNull:
		return encodeAppTagHeader(TagNull, 0), nil
	case AppBoolean:
		first := byte(TagBoolean << 4)
		if boolVal {
			first |= 1
		}
		return []byte{first}, nil
	case AppUnsigned, AppEnumerated:
		b := encodeUint(value)
		return append(encodeAppTagHeader(tag, len(b)), b...), nil
	case AppSigned:
		b := encodeInt(int64(value))
		return append(encodeAppTagHeader(tag, len(b)), b...), nil
	case AppReal:
		if math.IsNaN(floatVal) || math.IsInf(floatVal, 0) {
			return nil, proto("cannot encode NaN/Inf as BACnet REAL")
		}
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, math.Float32bits(float32(floatVal)))
		return append(encodeAppTagHeader(tag, 4), b...), nil
	case AppDouble:
		if math.IsNaN(floatVal) || math.IsInf(floatVal, 0) {
			return nil, proto("cannot encode NaN/Inf as BACnet DOUBLE")
		}
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, math.Float64bits(floatVal))
		return append(encodeAppTagHeader(tag, 8), b...), nil
	case AppCharacterStr:
		b := append([]byte{0x00}, bytes...) // 0 = ANSI X3.4（ASCII）
		return append(encodeAppTagHeader(tag, len(b)), b...), nil
	case AppOctetString:
		return append(encodeAppTagHeader(tag, len(bytes)), bytes...), nil
	case AppBitString:
		// 1 字节 unused-bit 计数 + 位数据。
		b := bytes
		return append(encodeAppTagHeader(tag, len(b)), b...), nil
	case AppObjectID:
		// ObjectID = type<<22 | instance，共 32 位（10 位 type + 22 位 instance）。
		if value > 0xFFFFFFFF {
			return nil, proto("object identifier %d exceeds 32-bit range", value)
		}
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(value))
		return append(encodeAppTagHeader(tag, 4), b...), nil
	}
	return nil, proto("cannot encode application tag 0x%02X", tag)
}

// DecodeApplicationValue 解析一个 BACnet 应用值，返回 tag 与解码结果。
//
// 返回（tag, uint64, float64, bytes, bool, error）：不同 tag 只填其中一部分。
// 例如 AppBoolean 填 bool，AppReal 填 float64，AppCharacterStr 填 bytes。
func DecodeApplicationValue(buf []byte) (tag byte, u uint64, f float64, b []byte, bl bool, err error) {
	h, off, err := parseTagHeader(buf)
	if err != nil {
		return 0, 0, 0, nil, false, err
	}
	if h.context || h.opening || h.closing {
		return 0, 0, 0, nil, false, proto("expected application value, got context tag %d", h.tagNumber)
	}
	tag = h.tagNumber
	payload := buf[off:]
	if len(payload) < h.length {
		return 0, 0, 0, nil, false, ErrTruncated
	}
	payload = payload[:h.length]
	switch tag {
	case TagNull:
		return tag, 0, 0, nil, false, nil
	case TagBoolean:
		return tag, 0, 0, nil, h.boolValue, nil
	case TagUnsigned, TagEnumerated, TagObjectID:
		u = decodeUint(payload)
		return tag, u, 0, nil, false, nil
	case TagSigned:
		u = uint64(decodeInt(payload))
		return tag, u, 0, nil, false, nil
	case TagReal:
		if len(payload) != 4 {
			return tag, 0, 0, nil, false, ErrTruncated
		}
		f = float64(math.Float32frombits(binary.BigEndian.Uint32(payload)))
		return tag, 0, f, nil, false, nil
	case TagDouble:
		if len(payload) != 8 {
			return tag, 0, 0, nil, false, ErrTruncated
		}
		f = math.Float64frombits(binary.BigEndian.Uint64(payload))
		return tag, 0, f, nil, false, nil
	case TagCharacterStr:
		if len(payload) < 1 {
			return tag, 0, 0, nil, false, ErrTruncated
		}
		b = payload[1:] // 跳过 1 字节字符集标识
		return tag, 0, 0, b, false, nil
	case TagOctetString:
		return tag, 0, 0, payload, false, nil
	case TagBitString:
		// BitString：1 字节 unused-bit 计数 + 位数据。原样返回。
		return tag, 0, 0, payload, false, nil
	case TagDate, TagTime:
		// Date/Time 不在过程量采集范围内（它们用于日程）。
		return tag, 0, 0, nil, false, proto("DATE/TIME application tags are not supported for point values")
	}
	return tag, 0, 0, nil, false, proto("unsupported application tag 0x%02X", tag)
}

func encodeUint(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var b [8]byte
	n := 0
	for v > 0 {
		b[n] = byte(v)
		v >>= 8
		n++
	}
	// 大端：反转低 n 字节。
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = b[n-1-i]
	}
	return out
}

func decodeUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func encodeInt(v int64) []byte {
	// 最短二进制补码：从 8 字节大端里剥掉「与符号位重复」的前导字节。
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	for i := 0; i < 7; i++ {
		if v >= 0 && buf[i] == 0x00 && buf[i+1]&0x80 == 0 {
			continue
		}
		if v < 0 && buf[i] == 0xFF && buf[i+1]&0x80 != 0 {
			continue
		}
		return append([]byte(nil), buf[i:]...)
	}
	// 前 7 字节都可以剥掉（v == 0 或 v == -1），只剩最后 1 字节。
	return []byte{buf[7]}
}

func decodeInt(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	// 检查符号位。
	neg := b[0]&0x80 != 0
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	if neg {
		// 符号扩展。
		shift := uint(64 - len(b)*8)
		return int64(v<<shift) >> shift
	}
	return int64(v)
}

// ---------------------------------------------------------------------------
// 对象标识符
// ---------------------------------------------------------------------------

// EncodeObjectID 把对象类型与实例号打包为 4 字节大端（type<<22 | instance）。
func EncodeObjectID(objType, instance int) ([]byte, error) {
	if objType < 0 || objType > 0x3FF {
		return nil, proto("object type %d out of range 0..1023", objType)
	}
	if instance < 0 || instance > 0x3FFFFF {
		return nil, proto("object instance %d out of range 0..4194303", instance)
	}
	v := uint64(objType)<<22 | uint64(instance)
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b, nil
}

// DecodeObjectID 解析 4 字节对象标识符，返回类型与实例号。
func DecodeObjectID(b []byte) (objType, instance int, err error) {
	if len(b) != 4 {
		return 0, 0, ErrTruncated
	}
	v := binary.BigEndian.Uint32(b)
	objType = int(v >> 22)
	instance = int(v & 0x3FFFFF)
	return objType, instance, nil
}

// ---------------------------------------------------------------------------
// 服务请求构造
// ---------------------------------------------------------------------------

// confirmedRequestHeader 组装 ConfirmedRequest 的 APDU 头。
//
//	[0] 0x00（ConfirmedRequest，不分段）
//	[1] MAX-SEGMENTS-ACCEPTED(高 4 位) + MAX-APDU-SIZE-ACCEPTED(低 4 位) = 0x05
//	    高 4 位 0 = 不接受分段；低 4 位 5 = 最大 APDU 1476 字节。标准编码表
//	    里 0 是「最多 50 字节」——按 0x00 发送，设备回一个长一点的
//	    object-name 就会 Abort(buffer-overflow)。
//	[2] invoke ID
//	[3] 服务选择码
//
// 设备若非要分段应答只能回 Reject/Abort，驱动会把它映射成协议错误——
// 比悄悄拼出一个不完整的值安全。
func confirmedRequestHeader(service, invokeID byte) []byte {
	return []byte{APDUConfirmedRequest, 0x05, invokeID, service}
}

// ReadPropertyRequest 构造 ReadProperty 的 ConfirmedRequest APDU。
//
//	ReadProperty-Request ::= SEQUENCE {
//	  objectIdentifier        [0] BACnetObjectIdentifier,
//	  propertyIdentifier      [1] BACnetPropertyIdentifier,
//	  propertyArrayIndex      [2] Unsigned OPTIONAL
//	}
func ReadPropertyRequest(invokeID byte, objType, instance int, property int, propIndex int) ([]byte, error) {
	objBytes, err := EncodeObjectID(objType, instance)
	if err != nil {
		return nil, err
	}
	tag0, err := contextTag(0, objBytes)
	if err != nil {
		return nil, err
	}
	tag1, err := contextTag(1, encodeUint(uint64(property)))
	if err != nil {
		return nil, err
	}
	apdu := append(confirmedRequestHeader(ServiceReadProperty, invokeID), tag0...)
	apdu = append(apdu, tag1...)
	if propIndex > 0 {
		tag2, err := contextTag(2, encodeUint(uint64(propIndex)))
		if err != nil {
			return nil, err
		}
		apdu = append(apdu, tag2...)
	}
	return apdu, nil
}

// WritePropertyRequest 构造 WriteProperty 的 ConfirmedRequest APDU。
//
//	WriteProperty-Request ::= SEQUENCE {
//	  objectIdentifier        [0] BACnetObjectIdentifier,
//	  propertyIdentifier      [1] BACnetPropertyIdentifier,
//	  propertyArrayIndex      [2] Unsigned OPTIONAL,
//	  propertyValue           [3] ABSTRACT-SYNTAX.&Type,
//	  priority                [4] Unsigned (1..16) OPTIONAL
//	}
//
// propertyValue 用 opening/closing 包裹应用值：它的内容是「自带标签的类型」，
// 只能用构造形式表达，不能塞进 context 标签的原始负载里。
// priority 为 0 表示不携带（设备按 relinquish-default 处理）。
func WritePropertyRequest(invokeID byte, objType, instance int, property, propIndex, priority int, appValue []byte) ([]byte, error) {
	if len(appValue) == 0 {
		return nil, proto("write property requires a non-empty application value")
	}
	if priority < 0 || priority > 16 {
		return nil, proto("write priority %d out of range 0..16", priority)
	}
	if propIndex < 0 {
		return nil, proto("write property array index %d must be >= 0", propIndex)
	}
	objBytes, err := EncodeObjectID(objType, instance)
	if err != nil {
		return nil, err
	}
	tag0, err := contextTag(0, objBytes)
	if err != nil {
		return nil, err
	}
	tag1, err := contextTag(1, encodeUint(uint64(property)))
	if err != nil {
		return nil, err
	}
	value := make([]byte, 0, len(appValue)+2)
	value = append(value, contextOpening(3)...)
	value = append(value, appValue...)
	value = append(value, contextClosing(3)...)

	apdu := append(confirmedRequestHeader(ServiceWriteProperty, invokeID), tag0...)
	apdu = append(apdu, tag1...)
	if propIndex > 0 {
		tag2, err := contextTag(2, encodeUint(uint64(propIndex)))
		if err != nil {
			return nil, err
		}
		apdu = append(apdu, tag2...)
	}
	apdu = append(apdu, value...)
	if priority > 0 {
		tag4, err := contextTag(4, encodeUint(uint64(priority)))
		if err != nil {
			return nil, err
		}
		apdu = append(apdu, tag4...)
	}
	return apdu, nil
}

// ---------------------------------------------------------------------------
// 应答解析
// ---------------------------------------------------------------------------

// APDUKind 是应答 APDU 的类型。
type APDUKind int

const (
	APDUKindComplexACK APDUKind = iota
	APDUKindSimpleACK
	APDUKindError
	APDUKindReject
	APDUKindAbort
	APDUKindUnconfirmed
	APDUKindConfirmedRequest
	APDUKindSegmentACK
)

func (k APDUKind) String() string {
	switch k {
	case APDUKindComplexACK:
		return "complex-ack"
	case APDUKindSimpleACK:
		return "simple-ack"
	case APDUKindSegmentACK:
		return "segment-ack"
	case APDUKindError:
		return "error"
	case APDUKindReject:
		return "reject"
	case APDUKindAbort:
		return "abort"
	case APDUKindUnconfirmed:
		return "unconfirmed-request"
	case APDUKindConfirmedRequest:
		return "confirmed-request"
	}
	return "unknown"
}

// ClassifyAPDU 只按首字节判断 APDU 类型。
//
// 首字节高 4 位是 PDU 类型；低 4 位是分段/更多标志。这里只看高 4 位，
// 因为分段位由驱动在解析具体报文时单独检查。
func ClassifyAPDU(first byte) APDUKind {
	switch first & 0xF0 {
	case 0x00:
		return APDUKindConfirmedRequest
	case 0x10:
		return APDUKindUnconfirmed
	case 0x20:
		return APDUKindSimpleACK
	case 0x30:
		return APDUKindComplexACK
	case 0x40:
		return APDUKindSegmentACK
	case 0x50:
		return APDUKindError
	case 0x60:
		return APDUKindReject
	case 0x70:
		return APDUKindAbort
	}
	return APDUKind(0)
}

// ParseACK 解析 ComplexACK / SimpleACK 的公共头。
//
//	ComplexACK: [0]=0x3X [1]=invokeID [2]=service [3..]=应答数据
//	SimpleACK:  [0]=0x20 [1]=invokeID [2]=service
//
// 分段标志（ComplexACK 首字节 bit3、invoke ID 后的 sequence number 等）
// 不在支持范围内，命中直接报错。
func ParseACK(apdu []byte) (kind APDUKind, service byte, invokeID byte, payload []byte, err error) {
	if len(apdu) < 3 {
		return 0, 0, 0, nil, ErrTruncated
	}
	kind = ClassifyAPDU(apdu[0])
	switch kind {
	case APDUKindComplexACK:
		if apdu[0]&0x08 != 0 {
			return 0, 0, 0, nil, proto("segmented ComplexACK is not supported")
		}
	case APDUKindSimpleACK:
		// SimpleACK 首字节固定 0x20，低 4 位必须全 0。
		if apdu[0]&0x0F != 0 {
			return 0, 0, 0, nil, proto("unexpected SimpleACK header 0x%02X", apdu[0])
		}
	case APDUKindSegmentACK:
		return 0, 0, 0, nil, proto("segmented responses are not supported (SegmentACK 0x%02X)", apdu[0])
	default:
		return 0, 0, 0, nil, proto("unexpected ACK type 0x%02X (%s)", apdu[0], kind)
	}
	return kind, apdu[2], apdu[1], apdu[3:], nil
}

// ParseError 解析 Error APDU，返回 invoke ID、错误类与错误码。
//
//	[0]=0x50 [1]=invokeID [2..]=errorClass（Enumerated）[..]=errorCode（Enumerated）
func ParseError(apdu []byte) (invokeID, errClass, errCode byte, err error) {
	if len(apdu) < 3 {
		return 0, 0, 0, ErrTruncated
	}
	if ClassifyAPDU(apdu[0]) != APDUKindError {
		return 0, 0, 0, proto("not an Error APDU: 0x%02X", apdu[0])
	}
	invokeID = apdu[1]
	h, off, e := parseTagHeader(apdu[2:])
	if e != nil {
		return invokeID, 0, 0, e
	}
	if h.context || h.opening || h.closing {
		return invokeID, 0, 0, proto("error class is not an application value")
	}
	body := apdu[2:]
	if len(body) < off+h.length {
		return invokeID, 0, 0, ErrTruncated
	}
	errClass = byte(decodeUint(body[off : off+h.length]))
	rest := body[off+h.length:]
	h, off, e = parseTagHeader(rest)
	if e != nil {
		return invokeID, 0, 0, e
	}
	if h.context || h.opening || h.closing {
		return invokeID, 0, 0, proto("error code is not an application value")
	}
	if len(rest) < off+h.length {
		return invokeID, 0, 0, ErrTruncated
	}
	errCode = byte(decodeUint(rest[off : off+h.length]))
	return invokeID, errClass, errCode, nil
}

// ParseReject 解析 Reject APDU，返回 invoke ID 与拒绝原因。
func ParseReject(apdu []byte) (invokeID, reason byte, err error) {
	if len(apdu) < 3 {
		return 0, 0, ErrTruncated
	}
	if ClassifyAPDU(apdu[0]) != APDUKindReject {
		return 0, 0, proto("not a Reject APDU: 0x%02X", apdu[0])
	}
	return apdu[1], apdu[2], nil
}

// ParseAbort 解析 Abort APDU，返回 invoke ID 与中止原因。
//
// Abort 的 bit0 是「服务端发起」标志，原因在高 4 位。
func ParseAbort(apdu []byte) (invokeID, reason byte, err error) {
	if len(apdu) < 3 {
		return 0, 0, ErrTruncated
	}
	if ClassifyAPDU(apdu[0]) != APDUKindAbort {
		return 0, 0, proto("not an Abort APDU: 0x%02X", apdu[0])
	}
	return apdu[1], apdu[2], nil
}

// AcknowledgeInvoke 返回应答/错误报文里的 invoke ID；无法识别时 ok=false。
//
// UDP 是无连接的：同一个套接字上可能先后收到上一条请求迟到的应答。
// 驱动必须先用它比对 invoke ID，再决定是否消费这条报文。
func AcknowledgeInvoke(apdu []byte) (invokeID byte, ok bool) {
	if len(apdu) < 2 {
		return 0, false
	}
	switch ClassifyAPDU(apdu[0]) {
	case APDUKindComplexACK, APDUKindSimpleACK, APDUKindError, APDUKindReject, APDUKindAbort:
		return apdu[1], true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 应答负载解析
// ---------------------------------------------------------------------------

// ReadPropertyACK 从 ComplexACK 负载里取出属性值。
//
//	ReadProperty-ACK ::= SEQUENCE {
//	  objectIdentifier [0], propertyIdentifier [1],
//	  propertyArrayIndex [2] OPTIONAL, propertyValue [3]
//	}
//
// propertyValue 有两种在用的编码：构造形式（[3] opening + 应用值 + [3] closing）
// 与原始形式（[3] 直接携带应用值字节）。两种都接受——前者是本实现发出的，
// 后者见于部分老设备。
func ReadPropertyACK(payload []byte) (appValue []byte, hasValue bool, err error) {
	off := 0
	for off < len(payload) {
		h, n, e := parseTagHeader(payload[off:])
		if e != nil {
			return nil, false, e
		}
		body := payload[off+n:]
		if h.opening {
			// 构造值：收集到同号 closing tag 为止。
			if h.tagNumber != 3 {
				return nil, false, proto("unexpected opening tag %d in ReadProperty-ACK", h.tagNumber)
			}
			inner, _, e := collectConstructed(body, h.tagNumber)
			if e != nil {
				return nil, false, e
			}
			if len(inner) == 0 {
				return nil, true, nil
			}
			return inner, true, nil
		}
		if h.closing {
			return nil, false, proto("unexpected closing tag %d in ReadProperty-ACK", h.tagNumber)
		}
		if len(body) < h.length {
			return nil, false, ErrTruncated
		}
		if h.tagNumber == 3 {
			return body[:h.length], true, nil
		}
		off += n + h.length
	}
	return nil, false, nil
}

// collectConstructed 从 opening tag 之后的内容里取出构造值本体，
// 返回本体与「包含 closing tag」的消耗长度。
func collectConstructed(buf []byte, tagNumber byte) (inner []byte, consumed int, err error) {
	off := 0
	for off < len(buf) {
		h, n, e := parseTagHeader(buf[off:])
		if e != nil {
			return nil, 0, e
		}
		if h.closing && h.tagNumber == tagNumber {
			return buf[:off], off + n, nil
		}
		body := buf[off+n:]
		if h.opening {
			_, sub, e := collectConstructed(body, h.tagNumber)
			if e != nil {
				return nil, 0, e
			}
			off += n + sub
			continue
		}
		if len(body) < h.length {
			return nil, 0, ErrTruncated
		}
		off += n + h.length
	}
	return nil, 0, proto("constructed value missing closing tag %d", tagNumber)
}

// ---------------------------------------------------------------------------
// 应答构造（内置从站使用）
// ---------------------------------------------------------------------------

// BuildComplexACK 构造 ReadProperty 的 ComplexACK APDU。
//
// appValue 是已编码好的应用值字节（tag + 负载），会被放进 [3] 的
// opening/closing 构造形式里。
func BuildComplexACK(service, invokeID byte, objType, instance, property int, appValue []byte) ([]byte, error) {
	objBytes, err := EncodeObjectID(objType, instance)
	if err != nil {
		return nil, err
	}
	tag0, err := contextTag(0, objBytes)
	if err != nil {
		return nil, err
	}
	tag1, err := contextTag(1, encodeUint(uint64(property)))
	if err != nil {
		return nil, err
	}
	apdu := []byte{APDUComplexACK, invokeID, service}
	apdu = append(apdu, tag0...)
	apdu = append(apdu, tag1...)
	if appValue != nil {
		apdu = append(apdu, contextOpening(3)...)
		apdu = append(apdu, appValue...)
		apdu = append(apdu, contextClosing(3)...)
	}
	return apdu, nil
}

// BuildSimpleACK 构造 SimpleACK APDU（WriteProperty 成功应答）。
func BuildSimpleACK(service, invokeID byte) []byte {
	return []byte{APDUSimpleACK, invokeID, service}
}

// ErrorClass / ErrorCode：只收录本网关会判定语义的那些。
const (
	ErrorClassDevice    = 0
	ErrorClassObject    = 1
	ErrorClassProperty  = 2
	ErrorClassResources = 3
	ErrorClassSecurity  = 4
	ErrorClassServices  = 5
	ErrorClassVT        = 6
	ErrorClassProtocol  = 7
	ErrorClassRT        = 8

	ErrorCodeOther                    = 0
	ErrorCodeDeviceBusy               = 3
	ErrorCodeInvalidDataType          = 9
	ErrorCodeInvalidParameterDataType = 13
	ErrorCodeMissingRequiredParameter = 16
	ErrorCodeOperationFailed          = 25
	ErrorCodeReadAccessDenied         = 27
	ErrorCodeUnknownObject            = 31
	ErrorCodeUnknownProperty          = 32
	ErrorCodeValueOutOfRange          = 37
	ErrorCodeWriteAccessDenied        = 40
	ErrorCodeInvalidArrayIndex        = 42
	ErrorCodePropertyIsNotAList       = 50
)

// BuildError 构造 Error APDU。
func BuildError(service, invokeID, errClass, errCode byte) []byte {
	classValue, _ := EncodeApplicationValue(AppEnumerated, uint64(errClass), 0, nil, false)
	codeValue, _ := EncodeApplicationValue(AppEnumerated, uint64(errCode), 0, nil, false)
	apdu := []byte{APDUError, invokeID}
	apdu = append(apdu, classValue...)
	apdu = append(apdu, codeValue...)
	return apdu
}

// BuildReject 构造 Reject APDU。
func BuildReject(invokeID, reason byte) []byte {
	return []byte{APDUReject, invokeID, reason}
}

// BuildAbort 构造 Abort APDU（server=true 表示由服务端发起）。
func BuildAbort(server bool, invokeID, reason byte) []byte {
	first := byte(APDUAbort)
	if server {
		first |= 0x01
	}
	return []byte{first, invokeID, reason}
}

// ---------------------------------------------------------------------------
// 服务请求解析（内置从站使用）
// ---------------------------------------------------------------------------

// ConfirmedRequest 是解析后的 ConfirmedRequest 报文。
type ConfirmedRequest struct {
	Service  byte
	InvokeID byte
	// Segmented 为 true 时后续字段无意义：本实现不支持分段请求。
	Segmented bool
	Params    []byte
}

// ParseConfirmedRequest 解析 ConfirmedRequest 的公共头。
func ParseConfirmedRequest(apdu []byte) (ConfirmedRequest, error) {
	if len(apdu) < 3 {
		return ConfirmedRequest{}, ErrTruncated
	}
	if ClassifyAPDU(apdu[0]) != APDUKindConfirmedRequest {
		return ConfirmedRequest{}, proto("not a ConfirmedRequest: 0x%02X", apdu[0])
	}
	req := ConfirmedRequest{
		Segmented: apdu[0]&0x08 != 0,
		InvokeID:  apdu[2],
		Service:   apdu[3],
		Params:    apdu[4:],
	}
	return req, nil
}

// ReadPropertyRequestParams 解析 ReadProperty 请求参数。
func ReadPropertyRequestParams(params []byte) (objType, instance, property, propIndex int, err error) {
	tags, err := parseContextParams(params, 3)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if tags[0] == nil || tags[1] == nil {
		return 0, 0, 0, 0, proto("read property missing object identifier or property identifier")
	}
	objType, instance, err = DecodeObjectID(tags[0])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	property = int(decodeUint(tags[1]))
	if tags[2] != nil {
		propIndex = int(decodeUint(tags[2]))
	}
	return objType, instance, property, propIndex, nil
}

// WritePropertyRequestParams 解析 WriteProperty 请求参数。
func WritePropertyRequestParams(params []byte) (objType, instance, property, priority int, appValue []byte, err error) {
	objID, propID, _, value, prio, err := SplitWritePropertyParams(params)
	if err != nil {
		return 0, 0, 0, 0, nil, err
	}
	objType, instance, err = DecodeObjectID(objID)
	if err != nil {
		return 0, 0, 0, 0, nil, err
	}
	property = int(decodeUint(propID))
	if prio != nil {
		priority = int(decodeUint(prio))
	}
	return objType, instance, property, priority, value, nil
}

// SplitWritePropertyParams 把 WriteProperty 参数拆成原始段：
// [0] 对象标识、[1] 属性标识、[2] 数组索引、[3] 属性值（应用值字节）、
// [4] 优先级。索引/优先级可能为 nil。
func SplitWritePropertyParams(params []byte) (objID, propID, arrayIndex, appValue, priority []byte, err error) {
	tags, err := parseContextParams(params, 5)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if tags[0] == nil || tags[1] == nil || tags[3] == nil {
		return nil, nil, nil, nil, nil, proto("write property missing object identifier, property identifier or value")
	}
	return tags[0], tags[1], tags[2], tags[3], tags[4], nil
}

// parseContextParams 把参数区拆成「context 标签号 → 负载」。
//
// n 是期望覆盖的最大标签号（含）。标签号必须在 [0,n) 内，重复标签号报错，
// 这样畸形报文不会悄悄覆盖掉前面的参数。
func parseContextParams(params []byte, n int) ([][]byte, error) {
	if n <= 0 {
		return nil, proto("invalid context param count %d", n)
	}
	out := make([][]byte, n)
	off := 0
	for off < len(params) {
		h, adv, err := parseTagHeader(params[off:])
		if err != nil {
			return nil, err
		}
		if !h.context {
			return nil, proto("application tag %d where context tag expected", h.tagNumber)
		}
		if h.opening {
			inner, consumed, err := collectConstructed(params[off+adv:], h.tagNumber)
			if err != nil {
				return nil, err
			}
			if int(h.tagNumber) >= n {
				return nil, proto("context tag %d out of range 0..%d", h.tagNumber, n-1)
			}
			if out[h.tagNumber] != nil {
				return nil, proto("duplicate context tag %d", h.tagNumber)
			}
			out[h.tagNumber] = inner
			off += adv + consumed
			continue
		}
		if h.closing {
			return nil, proto("unexpected closing tag %d", h.tagNumber)
		}
		if len(params[off+adv:]) < h.length {
			return nil, ErrTruncated
		}
		if int(h.tagNumber) >= n {
			return nil, proto("context tag %d out of range 0..%d", h.tagNumber, n-1)
		}
		if out[h.tagNumber] != nil {
			return nil, proto("duplicate context tag %d", h.tagNumber)
		}
		out[h.tagNumber] = params[off+adv : off+adv+h.length]
		off += adv + h.length
	}
	return out, nil
}
