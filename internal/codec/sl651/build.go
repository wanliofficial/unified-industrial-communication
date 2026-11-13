package sl651

import (
	"errors"
	"fmt"
	"time"
)

// Builder 描述一帧的链路层字段（除长度与功能码外的全部固定字段）。
//
// 中心站地址、遥测站地址与密码是「每条链路固定、每条报文重复」的三项，
// 所以单独抽出来，避免每个构造函数都再传一遍。
type Builder struct {
	// Center 是中心站地址（1 字节，0 表示不指定）。
	Center int
	// Station 是遥测站地址（5 字节 BCD）。
	Station [5]byte
	// Password 是 2 字节密码。
	Password [2]byte
}

// frame 组装一帧：7E7E + 链路层 + 长度 + STX + 正文 + 结束符 + CRC。
//
// 长度字段回填的是「报文起始符之后、结束符之前」的字节数 = len(body)，
// 整帧 = 17 + len(body)；校验码覆盖 7E7E 到结束符，高位字节在前。
// 这三条都在 io.go 的注释里给了出处，build 与 parse 必须互为逆运算，
// 测试用 make → parse → 比较往返来锁死。
//
// 地址字段顺序随方向变化（表 11 上行 / 表 12 下行）：上行是「中心站1 + 遥测站5」，
// 下行是「遥测站5 + 中心站1」。这里与 Parse 必须同步，否则中心站发出的查询帧
// 会把自己的站号写进遥测站字段。
//
// end 由调用方给出而不是按方向猜：下行结束符有 ENQ/ACK/NAK/EOT/ESC 五种，
// 语义完全不同（查询、确认、拒绝重发、退出、保持在线）。统一写成 ETX 的帧
// 结构上能过 CRC，对端却会当成上行帧或直接丢弃。
func (b Builder) frame(fn byte, dir int, end byte, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrBadReport)
	}
	if len(body) > MaxLen {
		return nil, fmt.Errorf("%w: body %d bytes exceeds %d", ErrBadReport, len(body), MaxLen)
	}
	out := make([]byte, 0, FrameLen(len(body)))
	out = append(out, Start, Start)
	if dir == DirUp {
		out = append(out, byte(b.Center&0xFF))
		out = append(out, b.Station[:]...)
	} else {
		out = append(out, b.Station[:]...)
		out = append(out, byte(b.Center&0xFF))
	}
	out = append(out, b.Password[:]...)
	out = append(out, fn)

	rawLen := dir | (len(body) & MaxLen)
	out = append(out, byte(rawLen>>8), byte(rawLen&0xFF))
	out = append(out, STX)
	out = append(out, body...)
	out = append(out, end)
	return AppendCRC(out), nil
}

// BuildQuery 构造 37H（实时数据）/38H（时段数据）查询帧（表 42/44）。
//
// 这两类查询的下行正文只有流水号 + 发报时间，没有别的参数；要查哪些要素
// 由中心站与终端的事先约定决定。
func (b Builder) BuildQuery(fn byte, serial uint16, t time.Time) ([]byte, error) {
	if fn != FuncQueryAll && fn != FuncQueryPer {
		return nil, fmt.Errorf("%w: func 0x%02X is not 37H/38H", ErrUnsupported, fn)
	}
	body := make([]byte, 0, 8)
	body = append(body, byte(serial>>8), byte(serial&0xFF))
	rt := EncodeBCDTime(t)
	body = append(body, rt[:]...)
	return b.frame(fn, DirDown, ENQ, body)
}

// BuildQueryElem 构造 3AH（查询指定要素）查询帧。
//
// 正文 = 流水号 + 发报时间 + 要素标识符组。标识符组只写「引导符 + 数据定义
// 字节」，不写数据（中心站要的是数据，不是给数据）；数据定义字节取附录 C
// 的规约默认值，即 N(D,d) 换算出的字节数与小位数。
func (b Builder) BuildQueryElem(serial uint16, t time.Time, guideList []byte) ([]byte, error) {
	if len(guideList) == 0 {
		return nil, fmt.Errorf("%w: 3AH needs at least one guide", ErrBadReport)
	}
	body := make([]byte, 0, 8+2*len(guideList))
	body = append(body, byte(serial>>8), byte(serial&0xFF))
	rt := EncodeBCDTime(t)
	body = append(body, rt[:]...)
	for _, g := range guideList {
		info, ok := GuideInfo(g)
		if !ok {
			return nil, fmt.Errorf("%w: guide %02X", ErrUnknownGuide, g)
		}
		body = append(body, g, info.DataDef())
	}
	return b.frame(FuncQueryElem, DirDown, ENQ, body)
}

// BuildReport 构造一帧上行报文（30H/32H/33H/34H，以及 37H/38H/3AH 的应答）。
//
// 参数与表 32 的字段一一对应。voltage 传 nil 表示不编报电压；非 nil 时必须是
// 2 字节 BCD（规约要求保留 2 位小数，单位 V）。
func (b Builder) BuildReport(fn byte, serial uint16, reportTime time.Time,
	station [5]byte, classCode byte, obsTime time.Time,
	fields []Field, voltage []byte) ([]byte, error) {
	switch fn {
	case FuncTest, FuncTimed, FuncSurge, FuncHourly,
		FuncQueryAll, FuncQueryPer, FuncQueryElem:
	default:
		return nil, fmt.Errorf("%w: func 0x%02X", ErrUnsupported, fn)
	}

	body := make([]byte, 0, 128)
	body = append(body, byte(serial>>8), byte(serial&0xFF))
	rt := EncodeBCDTime(reportTime)
	body = append(body, rt[:]...)

	// 地址标识符：引导符 F1H + 固定定义字节 F1H + 5 字节站号。
	body = append(body, GSI, 0xF1)
	body = append(body, station[:]...)
	body = append(body, classCode)

	// 观测时间标识符：引导符 F0H + 固定定义字节 F0H + 5 字节 BCD 时间。
	body = append(body, GTT, 0xF0)
	ot := EncodeObsTime(obsTime)
	body = append(body, ot[:]...)

	for _, f := range fields {
		b, err := encodeField(f)
		if err != nil {
			return nil, err
		}
		body = append(body, b...)
	}

	if voltage != nil {
		if len(voltage) != 2 {
			return nil, fmt.Errorf("%w: voltage must be 2 BCD bytes, got %d", ErrBadReport, len(voltage))
		}
		info, ok := GuideInfo(GVT)
		if !ok {
			return nil, fmt.Errorf("%w: guide %02X", ErrUnknownGuide, GVT)
		}
		body = append(body, GVT, info.DataDef())
		body = append(body, voltage...)
	}

	return b.frame(fn, DirUp, ETX, body)
}

// BuildEvenReport 构造 31H 均匀时段报（表 30）。
//
// 正文 = 流水号 + 发报时间 + 地址 + 分类码 + 观测时间 + 时间步长码 + 1 个
// 要素标识符 + n 个等宽数据。字段的多组值直接写进 Field.Values。
func (b Builder) BuildEvenReport(serial uint16, reportTime time.Time,
	station [5]byte, classCode byte, obsTime time.Time,
	step TimeStep, field Field) ([]byte, error) {
	if _, ok := GuideInfo(field.Guide); !ok {
		return nil, fmt.Errorf("%w: guide %02X", ErrUnknownGuide, field.Guide)
	}
	if len(field.Values) == 0 {
		return nil, fmt.Errorf("%w: even report field has no values", ErrBadReport)
	}
	if _, ok := step.Minutes(); !ok {
		return nil, fmt.Errorf("%w: time step %02X%02X%02X is not a valid BCD step",
			ErrBadReport, step[0], step[1], step[2])
	}

	body := make([]byte, 0, 160)
	body = append(body, byte(serial>>8), byte(serial&0xFF))
	rt := EncodeBCDTime(reportTime)
	body = append(body, rt[:]...)
	body = append(body, GSI, 0xF1)
	body = append(body, station[:]...)
	body = append(body, classCode)
	body = append(body, GTT, 0xF0)
	ot := EncodeObsTime(obsTime)
	body = append(body, ot[:]...)

	// 时间步长码：定义字节固定 18H，数据 3 字节 BCD。
	body = append(body, GDR, 0x18)
	body = append(body, step[:]...)

	enc, err := encodeField(field)
	if err != nil {
		return nil, err
	}
	body = append(body, enc...)
	return b.frame(FuncEven, DirUp, ETX, body)
}

// BuildLinkReport 构造 2FH 链路维持报：正文只有流水号 + 发报时间。
func (b Builder) BuildLinkReport(serial uint16, reportTime time.Time) ([]byte, error) {
	body := make([]byte, 0, 8)
	body = append(body, byte(serial>>8), byte(serial&0xFF))
	rt := EncodeBCDTime(reportTime)
	body = append(body, rt[:]...)
	return b.frame(FuncLink, DirUp, ETX, body)
}

// encodeField 把字段编回字节（引导符 + 数据定义字节 + 各组数据）。
//
// 多组数据必须同宽：规约 6.6.4.5 d) 明确「标识符规定的数据长度定义适用于
// 其每组数据」，宽度不一致的报文是不合规的，这里直接拒绝而不是截断。
func encodeField(f Field) ([]byte, error) {
	w := f.Width()
	if w <= 0 {
		return nil, fmt.Errorf("%w: field %02X width %d", ErrWidth, f.Guide, w)
	}
	out := make([]byte, 0, 2+w*len(f.Values))
	out = append(out, f.Guide, f.DataDef)
	for i, v := range f.Values {
		if len(v) != w {
			return nil, fmt.Errorf("%w: field %02X value %d is %d bytes, want %d",
				ErrWidth, f.Guide, i, len(v), w)
		}
		out = append(out, v...)
	}
	return out, nil
}

// ErrWidth 表示字段宽度与数据长度不符（构造报文时校验）。
var ErrWidth = errors.New("sl651: field width mismatch")

// EncodeBCD 把整数编码成 n 字节 BCD（高字节在前）。
func EncodeBCD(v uint64, n int) []byte {
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte((v%100)/10)<<4 | byte(v%10)
		v /= 100
	}
	return out
}

// EncodeBCDTime 把时间编码成 6 字节 BCD（YYMMDDHHmmSS）。
func EncodeBCDTime(t time.Time) [6]byte {
	var out [6]byte
	copy(out[:], []byte{
		toBCD(t.Year() % 100), toBCD(int(t.Month())), toBCD(t.Day()),
		toBCD(t.Hour()), toBCD(t.Minute()), toBCD(t.Second()),
	})
	return out
}

// EncodeObsTime 把时间编码成 5 字节 BCD（YYMMDDHHmm）。
func EncodeObsTime(t time.Time) [5]byte {
	var out [5]byte
	copy(out[:], []byte{
		toBCD(t.Year() % 100), toBCD(int(t.Month())), toBCD(t.Day()),
		toBCD(t.Hour()), toBCD(t.Minute()),
	})
	return out
}

func toBCD(v int) byte { return byte((v/10)%10)<<4 | byte(v%10) }

// NewField 构造一个按附录 C 默认宽度/小数的单值字段。
//
// 数值按 BCD 编码、宽度取 N(D,d) 的字节数、小数位数取 d，因此
// NewField(GZ, 127) 得到的就是「39 23 00 00 01 27」（水位 0.127 m）。
// 数值超出字段宽度时返回错误——宁可构造失败，也不要送出被截断的报文。
//
// HEX 类要素（DRP/DRZ/ZT）不接受整数，因为它们的字节序与量值不是十进制：
// 用 NewFieldRaw 直接给原始字节，避免把 500cm 写成 BCD 的 0500。
func NewField(guide byte, value uint64) (Field, error) {
	info, ok := GuideInfo(guide)
	if !ok {
		return Field{}, fmt.Errorf("%w: guide %02X", ErrUnknownGuide, guide)
	}
	if info.Hex {
		return Field{}, fmt.Errorf("%w: guide %02X is HEX-coded, use NewFieldRaw", ErrBadElement, guide)
	}
	w := info.Bytes()
	if w <= 0 {
		return Field{}, fmt.Errorf("%w: guide %02X has no default width", ErrBadElement, guide)
	}
	if !fitsBCD(value, w) {
		return Field{}, fmt.Errorf("%w: value %d does not fit %d BCD bytes", ErrBadElement, value, w)
	}
	return Field{
		Guide:   guide,
		DataDef: info.DataDef(),
		Values:  [][]byte{EncodeBCD(value, w)},
	}, nil
}

// NewFieldRaw 构造一个按附录 C 默认宽度、数据原样给出的字段（状态字、报警
// 字、雨量/水位批量数据用）。
func NewFieldRaw(guide byte, data []byte) (Field, error) {
	info, ok := GuideInfo(guide)
	if !ok {
		return Field{}, fmt.Errorf("%w: guide %02X", ErrUnknownGuide, guide)
	}
	w := info.Bytes()
	if w <= 0 {
		return Field{}, fmt.Errorf("%w: guide %02X has no default width", ErrBadElement, guide)
	}
	if len(data) != w {
		return Field{}, fmt.Errorf("%w: guide %02X wants %d bytes, got %d", ErrBadElement, guide, w, len(data))
	}
	return Field{Guide: guide, DataDef: info.DataDef(), Values: [][]byte{append([]byte(nil), data...)}}, nil
}

// NewFieldSeries 构造「1 个标识符 + n 个等宽数据」的字段（均匀时段报、小时报
// 批量雨量/水位用）。与 NewFieldRaw 的差别只是 values 有多个。
func NewFieldSeries(guide byte, values ...[]byte) (Field, error) {
	info, ok := GuideInfo(guide)
	if !ok {
		return Field{}, fmt.Errorf("%w: guide %02X", ErrUnknownGuide, guide)
	}
	w := info.Bytes()
	if w <= 0 {
		return Field{}, fmt.Errorf("%w: guide %02X has no default width", ErrBadElement, guide)
	}
	if len(values) == 0 {
		return Field{}, fmt.Errorf("%w: guide %02X needs at least one value", ErrBadElement, guide)
	}
	out := Field{Guide: guide, DataDef: info.DataDef(), Values: make([][]byte, 0, len(values))}
	for i, v := range values {
		if len(v) != w {
			return Field{}, fmt.Errorf("%w: guide %02X value %d is %d bytes, want %d",
				ErrBadElement, guide, i, len(v), w)
		}
		out.Values = append(out.Values, append([]byte(nil), v...))
	}
	return out, nil
}

func fitsBCD(v uint64, n int) bool {
	limit := uint64(1)
	for i := 0; i < 2*n; i++ {
		limit *= 10
	}
	return v < limit
}
