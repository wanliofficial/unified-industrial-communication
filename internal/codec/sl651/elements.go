package sl651

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Field 是报文正文里的一个要素字段：一个标识符 + 一到多个等宽数据。
//
// 为什么不是「一个标识符 + 一个数据」：规约 6.6.4.5 d) 规定「标识符规定的
// 数据长度定义适用于其每组数据，即每组数据长度应一致」。均匀时段报（31H）
// 与小时报（34H）在这种规则下写成「1 个标识符 + n 个同宽数据」，n 由报文
// 长度与时段数推出，而不是另有计数字段。Values 因此是二维的：
//
//	Values[0] 是单值报文的唯一数据；Values[1..] 是多时段报文的其余时段。
type Field struct {
	// Guide 是引导符（0x00..0xDF 规约定义，0xEF 之后为引导符类/扩展）。
	Guide byte
	// DataDef 是数据定义字节：高 5 位字节数、低 3 位小数位数。
	DataDef byte
	// Values 是各时段的数据，每个长度都等于 Width()。
	Values [][]byte
}

// Width 返回每个数据的字节数。
//
// 引导符类不能只看数据定义字节：F0H/F1H/F4H..FCH 的宽度由规约固定，而它们
// 的定义字节写的是标记值（F0H 的「高 5 位」是 30）。这里与 ParseField 用同
// 一套判断，避免「解析对了、回写错了」。
func (f Field) Width() int {
	if g, ok := guides[f.Guide]; ok {
		if g.FixedWidth > 0 {
			return g.FixedWidth
		}
	}
	return int(f.DataDef>>3) & 0x1F
}

// Decimals 返回小数位数（数据定义字节低 3 位）。
func (f Field) Decimals() int { return int(f.DataDef & 0x07) }

// Count 返回数据个数（多时段报文大于 1）。
func (f Field) Count() int { return len(f.Values) }

// Key 返回标识符的可读形式（引导符 + 数据定义字节）。
func (f Field) Key() string { return fmt.Sprintf("%02X%02X", f.Guide, f.DataDef) }

// Name 返回要素中文名。
func (f Field) Name() string { return GuideName(f.Guide) }

// Unit 返回要素默认单位。
func (f Field) Unit() string { return GuideUnit(f.Guide) }

// NoData 判断第 i 个数据是否为「缺测」。
//
// 规约 6.6.4.5 d) 规定：某个要素某个时间点没有数据时，HEX/BCD 报文在对应
// 位置填「与其他数据位数一样的 F」。若把它当数值解出来就是 9999…，会直接
// 污染数据库，因此必须单独识别。
func (f Field) NoData(i int) bool {
	if i < 0 || i >= len(f.Values) {
		return true
	}
	for _, b := range f.Values[i] {
		if b != 0xFF {
			return false
		}
	}
	return len(f.Values[i]) > 0
}

// Number 返回第 i 个数据的数值。
//
// 两种编码走两条路，不能混：
//   - 常规要素是 BCD 十进制，数值 = 无符号整数 / 10^小数位数；
//   - DRP/DRZ 类是 HEX 整数（附录 C 表 C.1 第 5、6 行写明「每组雨量占 1 字节
//     HEX」「每组水位占 2 字节 HEX，分辨率为厘米」），数值 = 无符号整数 × LSB。
//     把 0x01F4 按 BCD 解会得到 194（而不是 5.00m），这类「看着合理」的错值
//     比报错更危险，所以按引导符分派而不是按数据定义字节。
func (f Field) Number(i int) (float64, error) {
	if i < 0 || i >= len(f.Values) {
		return 0, fmt.Errorf("%w: index %d out of range", ErrShort, i)
	}
	if f.NoData(i) {
		return 0, ErrNoData
	}
	if g, ok := guides[f.Guide]; ok && g.Hex {
		if g.Scale == 0 {
			return 0, fmt.Errorf("%w: guide %02X carries no numeric value", ErrBadElement, f.Guide)
		}
		return float64(f.Hex(i)) * g.Scale, nil
	}
	raw, err := decodeBCD(f.Values[i])
	if err != nil {
		return 0, err
	}
	return float64(raw) / float64(pow10(f.Decimals())), nil
}

// Hex 返回第 i 个数据的原始 HEX 值（用于状态字、报警字这类非数值要素）。
func (f Field) Hex(i int) uint64 {
	if i < 0 || i >= len(f.Values) {
		return 0
	}
	var v uint64
	for _, b := range f.Values[i] {
		v = v<<8 | uint64(b)
	}
	return v
}

// Slots 把一个批量要素的数据按规约的时段宽度切成若干段，返回每段的字节。
//
// 只对附录 C 里定义了 Slot 的引导符（DRP 1 字节/段、DRZ 2 字节/段）有效；
// 其余要素返回 nil。为什么不复用 SplitBytes：SplitBytes 是「调用方自己
// 指定宽度」，而这里宽度是**规约事实**——把 DRZ 按 1 字节切会得到 24 个
// 半截水位，值域看着正常、数值全错。
func (f Field) Slots(i int) [][]byte {
	if i < 0 || i >= len(f.Values) {
		return nil
	}
	g, ok := guides[f.Guide]
	if !ok || g.Slot <= 0 {
		return nil
	}
	data := f.Values[i]
	if len(data)%g.Slot != 0 {
		return nil
	}
	out := make([][]byte, 0, len(data)/g.Slot)
	for len(data) >= g.Slot {
		out = append(out, append([]byte(nil), data[:g.Slot]...))
		data = data[g.Slot:]
	}
	return out
}

// SlotCount 返回批量要素的时段数；非批量要素返回 1（就是它自己）。
//
// 这个数与 Values 的长度**不是一回事**：DRP 在报文里是 1 个 12 字节数据，
// Values 长度是 1，但 SlotCount 是 12。自检命令与驱动按 SlotCount 输出，
// 才不会把 12 段雨量当成一个溢出整数。
func (f Field) SlotCount(i int) int {
	if s := f.Slots(i); s != nil {
		return len(s)
	}
	return 1
}

// SlotNoData 判断第 i 个数据的第 j 段是否缺测（该段全 F）。
func (f Field) SlotNoData(i, j int) bool {
	slot := f.slot(i, j)
	if slot == nil {
		return true
	}
	for _, b := range slot {
		if b != 0xFF {
			return false
		}
	}
	return true
}

// SlotNumber 返回第 i 个数据的第 j 段的数值。
//
// 与 Number 的区别只在「段」这一层：Number 把整个 Values[i] 当一个数，
// 对 DRP（12 段挤在 1 个数据里）会解出溢出值。这里先按 Slot 切开，再用
// 与 Number 完全相同的规则换算（HEX × Scale / BCD ÷ 10^d），保证两条路径
// 不会得到不同的数。
func (f Field) SlotNumber(i, j int) (float64, error) {
	slot := f.slot(i, j)
	if slot == nil {
		return 0, fmt.Errorf("%w: slot %d of value %d out of range", ErrShort, j, i)
	}
	if f.SlotNoData(i, j) {
		return 0, ErrNoData
	}
	if g, ok := guides[f.Guide]; ok && g.Hex {
		if g.Scale == 0 {
			return 0, fmt.Errorf("%w: guide %02X carries no numeric value", ErrBadElement, f.Guide)
		}
		return float64(hexVal(slot)) * g.Scale, nil
	}
	raw, err := decodeBCD(slot)
	if err != nil {
		return 0, err
	}
	return float64(raw) / float64(pow10(f.Decimals())), nil
}

// hexVal 把字节串按 HEX 拼成无符号整数（高位在前）。
func hexVal(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// SlotHex 返回第 i 个数据的第 j 段的原始 HEX 值（状态字/报警字用）。
func (f Field) SlotHex(i, j int) uint64 {
	return hexVal(f.slot(i, j))
}

// SlotBytes 返回第 i 个数据的第 j 段的原始字节（副本）。
func (f Field) SlotBytes(i, j int) []byte {
	slot := f.slot(i, j)
	if slot == nil {
		return nil
	}
	return append([]byte(nil), slot...)
}

// slot 取第 i 个数据的第 j 段；非批量要素只有 j==0 这一段的全部字节。
func (f Field) slot(i, j int) []byte {
	if i < 0 || i >= len(f.Values) {
		return nil
	}
	slots := f.Slots(i)
	if slots == nil {
		if j != 0 {
			return nil
		}
		return f.Values[i]
	}
	if j < 0 || j >= len(slots) {
		return nil
	}
	return slots[j]
}

// ErrNoData 表示该时段缺测（数据位全 F）。
var ErrNoData = errors.New("sl651: no data (all F)")

// ErrBadElement 表示要素标识符/数据长度不合法。
var ErrBadElement = errors.New("sl651: bad element")

// ErrUnknownGuide 表示引导符不在规约表内（含 FFXXH 自定义扩展）。
var ErrUnknownGuide = errors.New("sl651: unknown guide")

// ParseField 解析「引导符 + 数据定义字节 + 1 个数据」，返回字段与被消费字节数。
//
// 数据宽度有三个来源，必须按引导符先判：
//   - 引导符类中规约固定了宽度的（F0H/F1H/F4H..FCH/45H）：宽度查附录 C 注 a~f；
//   - 引导符类中长度由报文总长决定的（F2H 人工置数、F3H 图片、FDH 批量流速）：
//     本包无法确定边界，返回 ErrUnsupported，绝不按高 5 位猜；
//   - 常规要素：宽度 = 数据定义字节高 5 位（报文自描述）。
//
// 若把 F0H 按高 5 位算，会被当成 30 字节，整条报文立即错位——这是本协议
// 最容易踩的坑，所以判断顺序是「先查表，再读定义字节」。
func ParseField(buf []byte) (Field, int, error) {
	if len(buf) < 2 {
		return Field{}, 0, ErrShort
	}
	guide, def := buf[0], buf[1]
	if guide == 0xFF {
		// FFXXH：后面还跟一个自定义字节，含义只有设备方知道。猜出来的值不会
		// 报错、只会污染点位，所以显式拒绝。
		return Field{}, 0, fmt.Errorf("%w: FFXX custom extension", ErrUnknownGuide)
	}
	w := 0
	if g, ok := guides[guide]; ok {
		switch {
		case g.Variable():
			return Field{}, 0, fmt.Errorf("%w: guide %02X has no implicit length", ErrUnsupported, guide)
		case g.FixedWidth > 0:
			w = g.FixedWidth
		default:
			w = int(def>>3) & 0x1F
		}
	} else {
		w = int(def>>3) & 0x1F
	}
	if w == 0 {
		return Field{}, 0, fmt.Errorf("%w: zero width for guide %02X", ErrBadElement, guide)
	}
	if len(buf) < 2+w {
		return Field{}, 0, fmt.Errorf("%w: guide %02X needs %d bytes", ErrShort, guide, 2+w)
	}
	data := make([]byte, w)
	copy(data, buf[2:2+w])
	return Field{Guide: guide, DataDef: def, Values: [][]byte{data}}, 2 + w, nil
}

// decodeBCD 把 BCD 字节解为整数（高字节在前，每字节两位十进制）。
func decodeBCD(data []byte) (uint64, error) {
	var v uint64
	for _, b := range data {
		hi, lo := b>>4, b&0x0F
		if hi > 9 || lo > 9 {
			return 0, fmt.Errorf("%w: non-BCD byte 0x%02X", ErrBadElement, b)
		}
		v = v*100 + uint64(hi)*10 + uint64(lo)
	}
	return v, nil
}

// BCD 把 BCD 字节解为浮点整数，忽略非法半字节（供展示用，正式解码走 Number）。
func BCD(data []byte) float64 {
	var v float64
	for _, b := range data {
		v = v*100 + float64(b>>4)*10 + float64(b&0x0F)
	}
	return v
}

// pow10 返回 10 的 n 次方（n ≤ 7）。
func pow10(n int) int {
	r := 1
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

// ParseBCDTime 解析发报时间（6 字节 BCD，YYMMDDHHmmSS）。
func ParseBCDTime(data []byte) (time.Time, error) {
	if len(data) < 6 {
		return time.Time{}, ErrShort
	}
	v, err := decodeBCD(data[:6])
	if err != nil {
		return time.Time{}, err
	}
	return timeFromYYMMDDHHmmSS(v), nil
}

// ParseObsTime 解析观测时间（5 字节 BCD，YYMMDDHHmm）。
func ParseObsTime(data []byte) (time.Time, error) {
	if len(data) < 5 {
		return time.Time{}, ErrShort
	}
	v, err := decodeBCD(data[:5])
	if err != nil {
		return time.Time{}, err
	}
	return timeFromYYMMDDHHmm(v), nil
}

func timeFromYYMMDDHHmmSS(v uint64) time.Time {
	sec := int(v % 100)
	v /= 100
	min := int(v % 100)
	v /= 100
	hour := int(v % 100)
	v /= 100
	day := int(v % 100)
	v /= 100
	month := int(v % 100)
	year := int(v/100) + 2000
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
}

func timeFromYYMMDDHHmm(v uint64) time.Time {
	min := int(v % 100)
	v /= 100
	hour := int(v % 100)
	v /= 100
	day := int(v % 100)
	v /= 100
	month := int(v % 100)
	year := int(v/100) + 2000
	return time.Date(year, time.Month(month), day, hour, min, 0, 0, time.UTC)
}

// TimeStep 是均匀时段报（31H）的时间步长码：3 字节 BCD「日 时 分」。
//
// 表 C.3 规定 HEX/BCD 编码下数据结构是 3 字节、数据定义字节固定 18H：
//
//	d0000  单位为日，d 取 01~31
//	00hh00 单位为时，h 取 01~23
//	0000mm 单位为分，m 取 01~59
//	dhm    特定含义：全 0 表示其后必跟 DRP/DRZ 类 1h 时段组合数据（注 g）
//
// 三个字节里「一般只能有一个字节不为 00」，所以它不是「一个整数」而是一个
// 带单位的量。用 struct 而不是 int 承载，取值时就不会把 30min 与 30h 混淆。
type TimeStep [3]byte

// StepMin 返回以分钟为单位的步长（0 分钟表示「1h 时段组合」的特定含义）。
func StepMin(n int) TimeStep { return TimeStep{0x00, 0x00, byte(toBCD(n))} }

// StepHour 返回以小时为单位的步长。
func StepHour(n int) TimeStep { return TimeStep{0x00, byte(toBCD(n)), 0x00} }

// StepDay 返回以日为单位的步长。
func StepDay(n int) TimeStep { return TimeStep{byte(toBCD(n)), 0x00, 0x00} }

// StepHourlyCombo 是全 0 的时间步长码：其后必须跟 DRP/DRZ 类 1h 时段组合数据。
func StepHourlyCombo() TimeStep { return TimeStep{0x00, 0x00, 0x00} }

// Minutes 把时间步长码换算成分钟数；无法换算（非法 BCD 或多个非零字节）时
// 返回 false。全 0 返回 (0, true)，表示「1h 时段组合」这一特定含义。
func (s TimeStep) Minutes() (int, bool) {
	nonzero := 0
	for _, b := range s {
		hi, lo := b>>4, b&0x0F
		if hi > 9 || lo > 9 {
			return 0, false
		}
		if b != 0 {
			nonzero++
		}
	}
	if nonzero > 1 {
		return 0, false
	}
	v := func(b byte) int { return int(b>>4)*10 + int(b&0x0F) }
	return v(s[0])*24*60 + v(s[1])*60 + v(s[2]), true
}

// String 返回人能读的步长（如 "30min"、"1h"、"1d"、"1h 时段组合"）。
func (s TimeStep) String() string {
	if m, ok := s.Minutes(); ok {
		switch {
		case m == 0:
			return "1h 时段组合"
		case m%1440 == 0:
			return itoa(m/1440) + "d"
		case m%60 == 0:
			return itoa(m/60) + "h"
		default:
			return itoa(m) + "min"
		}
	}
	return fmt.Sprintf("时间步长码 %02X%02X%02X", s[0], s[1], s[2])
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }

// LookupGuide 按名字反查引导符：支持两位十六进制、ASCII 简称、中文名精确
// 与中文名包含三种写法。
//
// 现场点表里没人会写 `0x39`，写「水位」「Z」「39」都是常态，所以三种都收。
// 反查只在 ASCII 简称与中文名上做，绝不按「引导符数值 ±1」推算：附录 C 的
// 编码顺序与物理含义无关（0x16 是 M80 不是 M70），跳着写是常态。
func LookupGuide(s string) (byte, bool) {
	q := strings.ToLower(strings.TrimSpace(s))
	if q == "" {
		return 0, false
	}
	// 两位十六进制（表内的才认，避免把 "99" 当成引导符 0x99）。
	if len(q) == 2 {
		if v, ok := parseHex2(q); ok {
			if _, known := guides[v]; known {
				return v, true
			}
		}
	}
	// ASCII 简称：先精确、再前缀。精确优先，避免 "P1" 命中 "P12"。
	for code, g := range guides {
		if strings.EqualFold(g.Alias, q) {
			return code, true
		}
	}
	// 中文名：先精确、再包含。
	for code, g := range guides {
		if strings.ToLower(g.Name) == q {
			return code, true
		}
	}
	var hit byte
	found := false
	for code, g := range guides {
		if strings.Contains(strings.ToLower(g.Name), q) {
			// 多个命中时取编码最小的，保证结果稳定（map 遍历无序）。
			if !found || code < hit {
				hit, found = code, true
			}
		}
	}
	return hit, found
}

func parseHex2(s string) (byte, bool) {
	var v byte
	for i := 0; i < 2; i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}
