// Package iec60870 实现远动通信「应用层」的纯函数编解码：ASDU 结构、
// 信息元素、品质位与时标。
//
// 存在的理由：同一套应用层被两种承载复用——面向以太网的平衡式传输，
// 与面向串行总线的非平衡式传输。两者的类型标识、传送原因、元素布局
// 逐字节相同，差别只在链路层（一种是 4 字节序号控制域，另一种是
// FT1.2 帧 + 链路地址 + 校验和），以及头部三个字段的宽度由工程配置
// 决定（源发地址 0/1 字节、公共地址 1/2 字节、信息对象地址 1..3 字节）。
//
// 把应用层写两遍的代价不是「多写代码」，而是**两份实现会漂移**：一处
// 修了归一化值的边界，另一处没修，症状是「同一台设备走串口读出来的
// 温度和走网口不一样」。因此这里只有一份，两个链路层都用它。
//
// 本包不持有连接、不读时钟、不引用任何第三方库。
package iec60870

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// ASDU 类型与传送原因
// ---------------------------------------------------------------------------

// MaxASDU 是一条 ASDU 的建议上限。链路层各有自己的帧长约束（网口是
// 253 字节 APDU，串口由 FT1.2 的 L 字段决定），这里只拦「明显不合理」
// 的构造请求，避免一次误配置就让对端收到几十 KB 的单帧。
const MaxASDU = 249

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
	TypeIntegratedTotalsTime   byte = 32  // M_IT_TB_1（带 CP56Time2a 时标的累计量）
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

// BCR（二进制计数器读数）第 5 字节里的标志位。低 5 位是序号，不是品质。
const (
	bcrSeqMask  = 0x1F
	bcrCarry    = 1 << 5 // CY：计数期间发生进位（已溢出）
	bcrAdjusted = 1 << 6 // CA：计数器被人工调整过
	bcrInvalid  = 1 << 7 // IV：读数无效
)

// decodeBCRQuality 把 BCR 的标志位折成本包的品质位集合。
//
// IV 是「这个读数不能信」，必须落成 QInvalid（→ bad）。CY/CA 不是坏值，
// 但它们说明这个计数与上一周期不可比：进位让增量算成了负数，人工调整让
// 差值为假。两者都落成 uncertain，绝不能悄悄算成 good——电能量结算按
// 增量计费，一个假的增量就是一笔错账。
func decodeBCRQuality(b byte) Quality {
	var q Quality
	if b&bcrCarry != 0 {
		q |= QOverflow
	}
	if b&bcrAdjusted != 0 {
		q |= QSubstituted
	}
	if b&bcrInvalid != 0 {
		q |= QInvalid
	}
	return q
}

// encodeBCRQuality 是 decodeBCRQuality 的逆运算，保留原始序号位。
func encodeBCRQuality(raw byte, q Quality) byte {
	out := raw & bcrSeqMask
	if q.Has(QOverflow) {
		out |= bcrCarry
	}
	if q.Has(QSubstituted) {
		out |= bcrAdjusted
	}
	if q.Has(QInvalid) {
		out |= bcrInvalid
	}
	return out
}

// encodeSIQQuality 把归一化品质位写进 SIQ/DIQ 字节的高 4 位。
func encodeSIQQuality(q Quality) byte {
	return byte(q&siqQualityMask) << 3
}

// decodeQDS 把测量值（M_ME_*）的 QDS 字节折品质位集合。
//
// QDS = [OV, 保留×3, BL, SB, NT, IV]：OV 在 bit0，bit1..3 是保留位，BL/SB/NT/IV
// 在 bit4..7（与 SIQ/DIQ 的品质位同位置）。OV 与 BL/SB/NT/IV 之间隔着三个
// 保留位，不连续，必须逐位映射——既不能整体平移，也不能裸 `Quality(b & 0x1F)`
// （那会把 IV 落在 bit7 整个漏掉、把 BL 落在 bit4 误读成 IV，并把保留位当品质）。
func decodeQDS(b byte) Quality {
	var q Quality
	if b&0x01 != 0 {
		q |= QOverflow // OV bit0
	}
	if b&(1<<4) != 0 {
		q |= QBlocked // BL bit4
	}
	if b&(1<<5) != 0 {
		q |= QSubstituted // SB bit5
	}
	if b&(1<<6) != 0 {
		q |= QNotTopical // NT bit6
	}
	if b&(1<<7) != 0 {
		q |= QInvalid // IV bit7
	}
	return q
}

// encodeQDS 是 decodeQDS 的逆运算。
func encodeQDS(q Quality) byte {
	var b byte
	if q.Has(QOverflow) {
		b |= 0x01 // OV bit0
	}
	if q.Has(QBlocked) {
		b |= 1 << 4 // BL bit4
	}
	if q.Has(QSubstituted) {
		b |= 1 << 5 // SB bit5
	}
	if q.Has(QNotTopical) {
		b |= 1 << 6 // NT bit6
	}
	if q.Has(QInvalid) {
		b |= 1 << 7 // IV bit7
	}
	return b
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
//	累计量     Uint / Raw（BCR 序号，低 5 位）/ Quality（CY/CA/IV）
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

// Profile 是 ASDU 头部各字段的线上宽度。
//
// 应用层（ASDU 的类型、传送原因、元素与品质位）在 101 与 104 里完全相同，
// 差别只有头部几个字段的宽度——而它们由每一份「伴随标准」的工程配置决定，
// 不是协议常量：
//
//	字段            104        101 现场常见
//	源发地址 ORG     1 字节     0 或 1 字节
//	公共地址 CA      2 字节     1 或 2 字节
//	信息对象地址 IOA  3 字节     1、2 或 3 字节
//
// 把这些当成固定值，是「按 104 的宽度拼 101 帧」这类错误的来源：帧看起来
// 是合法的，每个字段也都「读出来了」，只是整体错位一到三个字节。
type Profile struct {
	// OrgSize 是源发地址字节数（0 或 1）。0 表示传送原因只有 1 字节。
	OrgSize int
	// CASize 是公共地址字节数（1 或 2）。
	CASize int
	// IOASize 是信息对象地址字节数（1..3）。
	IOASize int
}

// Profile104 是 104 固定使用的剖面：ORG 1、CA 2、IOA 3。
var Profile104 = Profile{OrgSize: 1, CASize: 2, IOASize: 3}

// HeaderLen 返回 ASDU 固定头长度（不含信息对象）。
func (p Profile) HeaderLen() int { return 3 + p.OrgSize + p.CASize }

// Validate 检查剖面是否落在标准允许的取值里。
func (p Profile) Validate() error {
	switch p.OrgSize {
	case 0, 1:
	default:
		return fmt.Errorf("originator address size %d must be 0 or 1", p.OrgSize)
	}
	switch p.CASize {
	case 1, 2:
	default:
		return fmt.Errorf("common address size %d must be 1 or 2", p.CASize)
	}
	switch p.IOASize {
	case 1, 2, 3:
	default:
		return fmt.Errorf("information object address size %d must be 1..3", p.IOASize)
	}
	return nil
}

func (p Profile) maxIOA() uint32 {
	switch p.IOASize {
	case 1:
		return 0xFF
	case 2:
		return 0xFFFF
	}
	return 0xFFFFFF
}

func (p Profile) maxCA() uint16 {
	if p.CASize == 1 {
		return 0xFF
	}
	return 0xFFFF
}

// ParseASDU 解析一条完整 ASDU（104 剖面）。
func ParseASDU(b []byte) (ASDU, error) { return ParseASDUProfile(b, Profile104) }

// ParseASDUProfile 按指定剖面解析一条完整 ASDU。
//
// 对未知类型返回错误：本平台没有该类型的元素长度，继续按「猜」解析会
// 把后续字段读成垃圾。调用方（driver）应当丢弃整帧——帧的长度字段
// 已经保证帧边界，丢弃一条不认识的 ASDU 不会破坏后续帧。
func ParseASDUProfile(b []byte, p Profile) (ASDU, error) {
	if err := p.Validate(); err != nil {
		return ASDU{}, err
	}
	if len(b) < p.HeaderLen() {
		return ASDU{}, fmt.Errorf("asdu too short: %d bytes, profile header is %d", len(b), p.HeaderLen())
	}
	a := ASDU{
		TypeID:   b[0],
		SQ:       b[1]&0x80 != 0,
		Cause:    b[2] & 0x3F,
		Negative: b[2]&0x40 != 0,
		Test:     b[2]&0x80 != 0,
	}
	off := 3
	if p.OrgSize == 1 {
		a.Originator = b[off]
		off++
	}
	if p.CASize == 1 {
		a.CommonAddr = uint16(b[off])
	} else {
		a.CommonAddr = binary.LittleEndian.Uint16(b[off : off+2])
	}
	off += p.CASize
	count := int(b[1] & 0x7F)
	rest := b[off:]
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
		// 召唤、时钟同步、初始化结束这几类 ASDU 的对象**没有**信息对象
		// 地址：限定词/时标紧跟在公共地址之后。按「每个对象都有 IOA」的
		// 通式去读，会多吞 1..3 个字节，随后所有字段整体错位——帧长度
		// 校验还能过，因为长度字段只描述外层长度，不代表内部分解正确。
		if HasIOA(a.TypeID) {
			if a.SQ && i > 0 {
				ioa++
				if ioa > p.maxIOA() {
					return ASDU{}, fmt.Errorf("asdu %s: sequential IOA overflows %d-byte address space", TypeName(a.TypeID), p.IOASize)
				}
			} else {
				if len(rest) < p.IOASize {
					return ASDU{}, fmt.Errorf("asdu %s: truncated information object address (%d bytes left, need %d)",
						TypeName(a.TypeID), len(rest), p.IOASize)
				}
				ioa = 0
				for k := 0; k < p.IOASize; k++ {
					ioa |= uint32(rest[k]) << (8 * k)
				}
				rest = rest[p.IOASize:]
			}
		} else {
			ioa = 0
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

// BuildASDU 编码一条 ASDU（104 剖面）。
func BuildASDU(a ASDU) ([]byte, error) { return BuildASDUProfile(a, Profile104) }

// BuildASDUProfile 按指定剖面编码一条 ASDU。
func BuildASDUProfile(a ASDU, p Profile) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if a.Cause > 0x3F {
		return nil, fmt.Errorf("cause %d exceeds 6 bits", a.Cause)
	}
	if len(a.Objects) > 127 {
		return nil, fmt.Errorf("too many information objects: %d (max 127)", len(a.Objects))
	}
	// 召唤/时钟同步/初始化结束一类的应用层报文只带一个对象，且该对象
	// 没有信息对象地址。多条会被从站按「不认识的报文」丢弃。
	if !HasIOA(a.TypeID) {
		if len(a.Objects) > 1 {
			return nil, fmt.Errorf("asdu %s carries at most one object, got %d",
				TypeName(a.TypeID), len(a.Objects))
		}
		for _, o := range a.Objects {
			if o.IOA != 0 {
				return nil, fmt.Errorf("asdu %s has no information object address, got IOA %d",
					TypeName(a.TypeID), o.IOA)
			}
		}
	}
	if _, ok := ElementSize(a.TypeID); !ok {
		return nil, fmt.Errorf("asdu type %d (%s) is not implemented", a.TypeID, TypeName(a.TypeID))
	}
	if a.Originator != 0 && p.OrgSize == 0 {
		return nil, fmt.Errorf("originator address %d set but profile has no ORG octet", a.Originator)
	}
	if uint32(a.CommonAddr) > uint32(p.maxCA()) {
		return nil, fmt.Errorf("common address %d exceeds %d-byte address space", a.CommonAddr, p.CASize)
	}
	if a.SQ {
		for i := 1; i < len(a.Objects); i++ {
			if a.Objects[i].IOA != a.Objects[0].IOA+uint32(i) {
				return nil, fmt.Errorf("SQ=1 requires consecutive IOAs: object %d has %d", i, a.Objects[i].IOA)
			}
		}
	}

	b := make([]byte, 0, p.HeaderLen()+len(a.Objects)*12)
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
	b = append(b, cot)
	if p.OrgSize == 1 {
		b = append(b, a.Originator)
	}
	if p.CASize == 1 {
		b = append(b, byte(a.CommonAddr))
	} else {
		b = binary.LittleEndian.AppendUint16(b, a.CommonAddr)
	}
	for i, o := range a.Objects {
		if o.IOA > p.maxIOA() {
			return nil, fmt.Errorf("information object address %d exceeds %d-byte address space", o.IOA, p.IOASize)
		}
		if HasIOA(a.TypeID) && (!a.SQ || i == 0) {
			for k := 0; k < p.IOASize; k++ {
				b = append(b, byte(o.IOA>>(8*k)))
			}
		}
		var err error
		b, err = appendElement(b, a.TypeID, o)
		if err != nil {
			return nil, err
		}
	}
	if len(b) > MaxASDU {
		return nil, fmt.Errorf("asdu too large: %d bytes (max %d)", len(b), MaxASDU)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// 元素编解码
// ---------------------------------------------------------------------------

// HasIOA 表示该类型标识的信息对象是否以「信息对象地址」开头。
//
// 四类例外：总召唤、电能量召唤（只有限定词）、时钟同步（只有 7 字节
// 时标）、初始化结束（只有 1 字节初始化原因）。它们**没有** IOA。
func HasIOA(typeID byte) bool {
	switch typeID {
	case TypeInterrogation, TypeCounterInterrogation, TypeClockSync, TypeEndOfInit:
		return false
	}
	return true
}

// ElementSize 返回一个信息对象的字节数（不含信息对象地址）。时间标签已计入。
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
	// M_IT_TB_1：BCR（5 字节）+ CP56Time2a（7 字节）。
	case TypeIntegratedTotalsTime:
		return 12, true
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
		TypeIntegratedTotals, TypeIntegratedTotalsTime,
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
		TypeMeasuredNormalizedTime, TypeMeasuredScaledTime, TypeMeasuredFloatTime,
		TypeIntegratedTotalsTime:
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
		return Object{Num: normalized(b[0:2]), Quality: decodeQDS(b[2])}, size, nil
	case TypeMeasuredScaled:
		return Object{Num: float64(int16(binary.LittleEndian.Uint16(b[0:2]))), Quality: decodeQDS(b[2])}, size, nil
	case TypeMeasuredFloat:
		return Object{Num: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4]))), Quality: decodeQDS(b[4])}, size, nil
	case TypeMeasuredNormalizedTime:
		return Object{
			Num: normalized(b[0:2]), Quality: decodeQDS(b[2]),
			Time: decodeCP56Time2a(b[3:10]),
		}, size, nil
	case TypeMeasuredScaledTime:
		return Object{
			Num: float64(int16(binary.LittleEndian.Uint16(b[0:2]))), Quality: decodeQDS(b[2]),
			Time: decodeCP56Time2a(b[3:10]),
		}, size, nil
	case TypeMeasuredFloatTime:
		return Object{
			Num: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4]))), Quality: decodeQDS(b[4]),
			Time: decodeCP56Time2a(b[5:12]),
		}, size, nil
	case TypeIntegratedTotals:
		// BCR（二进制计数器读数）第 5 字节：低 5 位是序号 SQ，bit5=CY
		// （进位）、bit6=CA（计数器被调整）、bit7=IV（值无效）。
		// 只取低 32 位而把这一字节整个丢掉，等于把设备明确标注的
		// 「这个累计量已经不可信」翻译成 good——电能量结算场景下这是
		// 最不该静默的一类错误（钱算错了没人会发现）。
		return Object{
			Uint: binary.LittleEndian.Uint32(b[0:4]),
			Raw:  b[4], Quality: decodeBCRQuality(b[4]),
		}, size, nil
	case TypeIntegratedTotalsTime:
		// 与 M_IT_NA_1 同一个 BCR 读法，只是末尾多 7 字节 CP56Time2a。
		// 时标对轮询主站不改变取值（值仍是累计量），但必须消费掉——
		// 否则后续信息对象的地址会偏移，整帧解析错位。
		return Object{
			Uint: binary.LittleEndian.Uint32(b[0:4]),
			Raw:  b[4], Quality: decodeBCRQuality(b[4]),
			Time: decodeCP56Time2a(b[5:12]),
		}, size, nil
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
		b = append(b, encodeQDS(o.Quality))
		if typeID == TypeMeasuredNormalizedTime {
			b = append(b, encodeCP56Time2a(o.Time)...)
		}
		return b, nil
	case TypeMeasuredScaled, TypeMeasuredScaledTime:
		if o.Num < math.MinInt16 || o.Num > math.MaxInt16 {
			return nil, fmt.Errorf("scaled value %v exceeds int16", o.Num)
		}
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(math.Round(o.Num))))
		b = append(b, encodeQDS(o.Quality))
		if typeID == TypeMeasuredScaledTime {
			b = append(b, encodeCP56Time2a(o.Time)...)
		}
		return b, nil
	case TypeMeasuredFloat, TypeMeasuredFloatTime:
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(o.Num)))
		b = append(b, encodeQDS(o.Quality))
		if typeID == TypeMeasuredFloatTime {
			b = append(b, encodeCP56Time2a(o.Time)...)
		}
		return b, nil
	case TypeIntegratedTotals:
		b = binary.LittleEndian.AppendUint32(b, o.Uint)
		return append(b, encodeBCRQuality(o.Raw, o.Quality)), nil
	case TypeIntegratedTotalsTime:
		b = binary.LittleEndian.AppendUint32(b, o.Uint)
		b = append(b, encodeBCRQuality(o.Raw, o.Quality))
		return append(b, encodeCP56Time2a(o.Time)...), nil
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

// normalized 把 16 位定点数还原成 -1..1 的浮点。
//
// 除数是 32768 而不是 32767：标准里归一化值的定义就是 N/32768，因此
// 取值范围是 -1 .. 1-2^-15，「+1」本身不可表示。用 32767 当分母会让
// 每一个值都比标准值大 0.003%，而满量程 32767 会「刚好」显示成 1.0，
// 看起来更漂亮，实际是把设备的定标悄悄改了。
func normalized(b []byte) float64 {
	return float64(int16(binary.LittleEndian.Uint16(b))) / 32768
}

// encodeNormalized 是 normalized 的逆运算：同样按 32768 定标。
//
// 编码与解码必须用同一个分母。用 32767 编码、32768 解码（一份很常见的
// 「对称看起来差不多」的写法）会让每一个读写往返都缩小 0.003%：读回
// 来的值再写下去、再读回来，数值会缓慢爬向 0。这点误差在单点上确实
// 看不出来，但斜坡给定、闭环调试这类场景会持续累积。
//
// 夹紧是必须的：round(1.0*32768) = 32768 已经溢出 int16，直接转 uint16
// 会回绕成 -32768，也就是把「满量程」写成「负满量程」。
func encodeNormalized(v float64) uint16 {
	if v > 1 {
		v = 1
	}
	if v < -1 {
		v = -1
	}
	n := int(math.Round(v * 32768))
	if n > math.MaxInt16 {
		n = math.MaxInt16
	}
	if n < math.MinInt16 {
		n = math.MinInt16
	}
	return uint16(int16(n))
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
	// 年字节只有低 7 位有定义，最高位是保留位。不掩掉它，一个把保留位
	// 置 1 的从站会让「2026 年」变成「2126 年」——而 2126 是一个语法上
	// 完全合法的时间，下游没有任何理由拒绝它。
	year := 2000 + int(b[6]&0x7F)
	if minute > 59 || hour > 23 || day < 1 || day > 31 || month < 1 || month > 12 {
		return nil
	}
	t := time.Date(year, time.Month(month), day, hour, minute,
		ms/1000, (ms%1000)*int(time.Millisecond), time.Local)
	// time.Date 会把越界日期**静默规范化**：2 月 31 日变成 3 月 3 日。
	// 那是把「设备给了一个非法时间」改写成「一个合法但错误的时间」，比
	// 直接判无效危险得多。回读一次，只接受原样保留的日期。
	if t.Year() != year || int(t.Month()) != month || t.Day() != day ||
		t.Hour() != hour || t.Minute() != minute {
		return nil
	}
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

// BuildInterrogation 构造总召唤（站召唤，以太网承载的固定剖面）。
func BuildInterrogation(ca uint16, qoi byte) ([]byte, error) {
	return BuildInterrogationProfile(ca, qoi, Profile104)
}

// BuildInterrogationProfile 按指定剖面构造总召唤。
//
// 101 的公共地址宽度由工程配置决定，不能沿用 104 的 2 字节；这是「同一份
// 应用层、不同剖面」最典型的例子。
func BuildInterrogationProfile(ca uint16, qoi byte, p Profile) ([]byte, error) {
	if qoi < 20 || qoi > 36 {
		return nil, fmt.Errorf("QOI %d out of range 20..36", qoi)
	}
	return BuildASDUProfile(ASDU{
		TypeID: TypeInterrogation, Cause: CauseActivation, CommonAddr: ca,
		Objects: []Object{{Qualifier: qoi}},
	}, p)
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
