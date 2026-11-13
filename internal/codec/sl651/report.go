package sl651

import (
	"errors"
	"fmt"
	"time"
)

// Report 是一帧上行报文解析后的结构化结果。
//
// 字段按报文正文出现顺序排列，与表 32（定时报）、表 34（加报报）、
// 表 36（小时报）、表 43/45（查询应答）一致：
//
//	流水号2 | 发报时间6 | 地址标识符(F1H)+站址(5) | 分类码1
//	| 观测时间标识符(F0H)+观测时间(5) | 要素信息组 | 电压标识符+电压数据
//
// 注意「地址标识符 F1H」与「观测时间标识符 F0H」不是可变长字符串的前缀，
// 而是**引导符类要素**：F1H 后面固定 5 字节站号、F0H 后面固定 5 字节 BCD
// 时间（附录 C 注 a、b）。把它们当成「以 F0H 结尾的变长站名」来扫，是看
// 这类规约最容易犯的错——站号里恰好出现 F0 时当场解析错位。
type Report struct {
	// Frame 是原始帧（含链路层字段与校验码）。
	Frame *Frame
	// Serial 是流水号（2 字节 HEX，1..65535）。
	Serial [2]byte
	// ReportTime 是发报时间（6 字节 BCD，YYMMDDHHmmSS）。
	ReportTime time.Time
	// ReportTimeRaw 是发报时间原始 6 字节，供需要原样回写的场景使用。
	ReportTimeRaw [6]byte
	// Station 是遥测站地址（5 字节 BCD，规约 6.2.3.2）。
	Station [5]byte
	// ClassCode 是遥测站分类码（附录 A）。链路维持报为 0。
	ClassCode byte
	// ObsTime 是观测时间（5 字节 BCD，YYMMDDHHmm）。均匀时段报中它是
	// 「第一组数据的观测时间」（表 30 注 a）。链路维持报为零值。
	ObsTime time.Time
	// ObsTimeRaw 是观测时间原始 5 字节。
	ObsTimeRaw [5]byte
	// TimeStep 是均匀时段报（31H）的时间步长码（3 字节 BCD「日 时 分」），
	// 其他功能码为 nil。
	TimeStep *TimeStep
	// Fields 是要素信息组，按报文顺序排列。
	Fields []Field
	// Voltage 是电源电压（引导符 VT 38H），未编报时为 nil。
	Voltage *Field
	// Rest 是解析完所有要素后剩余的字节。正常情况下为空；非空说明报文
	// 尾部有多余字节（现场常见于厂商自行追加的私有字段），驱动会打日志
	// 但不会因此丢整帧。
	Rest []byte
}

// ErrBadReport 表示报文正文结构与功能码不符。
var ErrBadReport = errors.New("sl651: bad report")

// ParseReport 解析一帧上行报文的正文。
//
// 按功能码分派：
//
//	2FH 链路维持报  —— 只有流水号 + 发报时间
//	30H/32H/33H     —— 标准报文体（表 28/32/34）
//	31H             —— 均匀时段报（表 30，单要素多时段）
//	34H             —— 小时报（表 36，雨量/水位用固定宽度引导符承载 12 组）
//	37H/38H/3AH 应答 —— 与标准报文体同形（表 43/45/49）
//
// 其余功能码（配置表、控制命令、图片报等）返回 ErrUnsupported：它们是
// 写/配置类，本包只做只读解析（见包注释）。
func ParseReport(f *Frame) (*Report, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil frame", ErrBadReport)
	}
	if f.Dir != DirUp {
		return nil, fmt.Errorf("%w: not an uplink frame (dir=0x%04X)", ErrBadReport, f.Dir)
	}

	switch f.Func {
	case FuncLink:
		return parseLinkReport(f)
	case FuncEven:
		return parseEvenReport(f)
	case FuncTest, FuncTimed, FuncSurge, FuncHourly,
		FuncQueryAll, FuncQueryPer, FuncQueryElem:
		return parseStdReport(f)
	default:
		return nil, fmt.Errorf("%w: func 0x%02X", ErrUnsupported, f.Func)
	}
}

// reportHead 是标准报文体的前 23 字节固定部分。
type reportHead struct {
	serial     [2]byte
	reportTime [6]byte
	station    [5]byte
	classCode  byte
	obsTime    [5]byte
}

// headLen 是标准报文体固定前缀的字节数：流水号2 + 发报时间6 + F1H+站址5
// + 分类码1 + F0H+观测时间5 = 23。
const headLen = 23

// parseHead 解析固定前缀，并严格校验两个引导符类的标识符字节。
func parseHead(body []byte) (reportHead, error) {
	var h reportHead
	if len(body) < headLen {
		return h, fmt.Errorf("%w: need %d bytes for head, got %d", ErrBadReport, headLen, len(body))
	}
	copy(h.serial[:], body[0:2])
	copy(h.reportTime[:], body[2:8])

	// 地址标识符：F1H + 站址。附录 C 注 b 规定定义字节恒为 F1H。
	if body[8] != GSI || body[9] != 0xF1 {
		return h, fmt.Errorf("%w: expected address id F1 F1, got %02X %02X", ErrBadReport, body[8], body[9])
	}
	copy(h.station[:], body[10:15])
	h.classCode = body[15]

	// 观测时间标识符：F0H + 时间。附录 C 注 a 规定定义字节恒为 F0H。
	if body[16] != GTT || body[17] != 0xF0 {
		return h, fmt.Errorf("%w: expected time id F0 F0, got %02X %02X", ErrBadReport, body[16], body[17])
	}
	copy(h.obsTime[:], body[18:23])
	return h, nil
}

// parseStdReport 解析 30H/32H/33H/34H 与 37H/38H/3AH 应答的正文。
func parseStdReport(f *Frame) (*Report, error) {
	if len(f.Body) < headLen {
		return nil, fmt.Errorf("%w: body %d bytes, need >= %d", ErrBadReport, len(f.Body), headLen)
	}
	h, err := parseHead(f.Body)
	if err != nil {
		return nil, err
	}

	rt, err := ParseBCDTime(h.reportTime[:])
	if err != nil {
		return nil, fmt.Errorf("%w: report time: %v", ErrBadReport, err)
	}
	ot, err := ParseObsTime(h.obsTime[:])
	if err != nil {
		return nil, fmt.Errorf("%w: observation time: %v", ErrBadReport, err)
	}

	r := &Report{
		Frame:         f,
		Serial:        h.serial,
		ReportTime:    rt,
		ReportTimeRaw: h.reportTime,
		Station:       h.station,
		ClassCode:     h.classCode,
		ObsTime:       ot,
		ObsTimeRaw:    h.obsTime,
	}

	rest := f.Body[headLen:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			r.Rest = append([]byte(nil), rest...)
			break
		}
		fld, n, err := ParseField(rest)
		if err != nil {
			// 尾部残缺、未知扩展、长度不可推导的引导符类（F2H 人工置数、
			// F3H 图片、FDH 批量流速）：不丢已解析的要素，把剩余字节交给
			// 调用方。这三类在规约里「数据长度由报文总长决定」，一旦出现
			// 在要素组里，后面就没有可对齐的边界了。
			if errors.Is(err, ErrShort) || errors.Is(err, ErrUnknownGuide) || errors.Is(err, ErrUnsupported) {
				r.Rest = append([]byte(nil), rest...)
				break
			}
			return nil, err
		}
		rest = rest[n:]
		if fld.Guide == GVT {
			r.Voltage = &fld
			continue
		}
		r.Fields = append(r.Fields, fld)
	}
	return r, nil
}

// parseLinkReport 解析 2FH 链路维持报：正文只有流水号 + 发报时间。
func parseLinkReport(f *Frame) (*Report, error) {
	if len(f.Body) < 8 {
		return nil, fmt.Errorf("%w: link report needs 8 bytes, got %d", ErrBadReport, len(f.Body))
	}
	r := &Report{Frame: f}
	copy(r.Serial[:], f.Body[0:2])
	copy(r.ReportTimeRaw[:], f.Body[2:8])
	rt, err := ParseBCDTime(r.ReportTimeRaw[:])
	if err != nil {
		return nil, fmt.Errorf("%w: report time: %v", ErrBadReport, err)
	}
	r.ReportTime = rt
	if len(f.Body) > 8 {
		r.Rest = append([]byte(nil), f.Body[8:]...)
	}
	return r, nil
}

// parseEvenReport 解析 31H 均匀时段报。
//
// 与其它功能码的关键差别（表 30 注 a/b + 6.6.4.4 b）：
//
//   - 只有一个要素标识符，后面跟 n 个**等宽**数据；
//   - 观测时间是第一组数据的观测时间；
//   - 采用 HEX/BCD 编码时**只编列 1 个要素**（多要素是 ASCII 帧才允许的），
//     因此 n 完全由「剩余字节数 ÷ 数据宽度」确定，不需要也不允许猜。
//
// 报文中没有「时段数」字段，也不该有：长度字段已经把它隐式给出了。
func parseEvenReport(f *Frame) (*Report, error) {
	if len(f.Body) < headLen+1 {
		return nil, fmt.Errorf("%w: even report body %d bytes", ErrBadReport, len(f.Body))
	}
	h, err := parseHead(f.Body)
	if err != nil {
		return nil, err
	}
	rt, err := ParseBCDTime(h.reportTime[:])
	if err != nil {
		return nil, fmt.Errorf("%w: report time: %v", ErrBadReport, err)
	}
	ot, err := ParseObsTime(h.obsTime[:])
	if err != nil {
		return nil, fmt.Errorf("%w: observation time: %v", ErrBadReport, err)
	}

	r := &Report{
		Frame:         f,
		Serial:        h.serial,
		ReportTime:    rt,
		ReportTimeRaw: h.reportTime,
		Station:       h.station,
		ClassCode:     h.classCode,
		ObsTime:       ot,
		ObsTimeRaw:    h.obsTime,
	}

	rest := f.Body[headLen:]

	// 时间步长码（引导符 04H，数据定义字节固定 18H，3 字节 BCD「日 时 分」）。
	// 表 30 序号 6 只写了「标识符」，但标识符后面必然跟它的数据（表 C.3）。
	step, n, err := ParseField(rest)
	if err != nil {
		return nil, fmt.Errorf("%w: time step: %v", ErrBadReport, err)
	}
	if step.Guide != GDR {
		return nil, fmt.Errorf("%w: expected time step guide %02X, got %02X", ErrBadReport, GDR, step.Guide)
	}
	if step.Width() != 3 {
		return nil, fmt.Errorf("%w: time step width %d, want 3", ErrBadReport, step.Width())
	}
	ts := TimeStep{step.Values[0][0], step.Values[0][1], step.Values[0][2]}
	r.TimeStep = &ts
	rest = rest[n:]

	// 唯一的要素标识符 + n 个等宽数据。
	if len(rest) < 2 {
		return nil, fmt.Errorf("%w: even report has no element", ErrBadReport)
	}
	tmpl, n, err := ParseField(rest)
	if err != nil {
		return nil, fmt.Errorf("%w: element: %v", ErrBadReport, err)
	}
	w := tmpl.Width()
	if w <= 0 {
		return nil, fmt.Errorf("%w: element width %d", ErrBadReport, w)
	}
	rest = rest[n:]

	if len(rest)%w != 0 {
		return nil, fmt.Errorf("%w: %d data bytes not a multiple of width %d", ErrBadReport, len(rest), w)
	}
	fld := Field{Guide: tmpl.Guide, DataDef: tmpl.DataDef, Values: make([][]byte, 0, 1+len(rest)/w)}
	fld.Values = append(fld.Values, tmpl.Values[0])
	for len(rest) >= w {
		v := make([]byte, w)
		copy(v, rest[:w])
		fld.Values = append(fld.Values, v)
		rest = rest[w:]
	}
	r.Fields = append(r.Fields, fld)
	if len(rest) > 0 {
		r.Rest = append([]byte(nil), rest...)
	}
	return r, nil
}

// Elems 返回报文里某个引导符的全部数据（多时段报文会有多个）。
//
// 同一条报文里同一引导符可能出现多次（例如加报报同时上送瞬时水位与均值
// 水位），因此返回切片而不是单值。
func (r *Report) Elems(guide byte) []Field {
	var out []Field
	for _, f := range r.Fields {
		if f.Guide == guide {
			out = append(out, f)
		}
	}
	return out
}

// First 返回报文里第一个指定引导符的字段。
func (r *Report) First(guide byte) (Field, bool) {
	for _, f := range r.Fields {
		if f.Guide == guide {
			return f, true
		}
	}
	return Field{}, false
}

// SplitBytes 把一个「固定宽度引导符类」的多组数据拆成等宽小段。
//
// 小时报（34H）的 1h 内每 5min 时段降水量（DRP F4H，12 字节）与相对水位
// （DRZ F5H..FCH，24 字节）在附录 C 里被定义成**一个定长数据**，规约表 36
// 里写的「12 组」就是这 12/24 字节的语义，不存在独立的组数字段。这里提供
// 按字节切分的辅助，让驱动能按 5min 时段逐段落点。
func SplitBytes(v []byte, width int) [][]byte {
	if width <= 0 || len(v)%width != 0 {
		return nil
	}
	out := make([][]byte, 0, len(v)/width)
	for len(v) >= width {
		out = append(out, append([]byte(nil), v[:width]...))
		v = v[width:]
	}
	return out
}
