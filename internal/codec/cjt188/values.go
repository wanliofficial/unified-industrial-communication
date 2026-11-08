package cjt188

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件只做一件事：把数据域里的若干字节解成「人可以看的数」。
//
// 与 DL/T 645 的三个关键差异，都在这里收口：
//
//  1. **数据域不加 0x33**。把 645 的 `- 0x33` 搬过来会让每个数值都小 51，
//     而 BCD 半字节校验多半还能通过——读出来是一组「合法但错」的数。
//  2. **单位是一个显式字节**，不是从小数位推出来的。同样 4 字节 BCD，
//     单位 0x2C 是 m³、0x29 是 L；按「4 字节就是两位小数」处理会把
//     1000 L 记成 1 m³ 之外的另一个数。
//  3. **时间是 7 字节**：秒/分/时/日/月/年低/年高，各字段是 BCD，
//     年份是两个 BCD 字节拼成的 4 位十进制。

// DecodeField 解码一个字段。
//
// raw 必须正好是 `field.Width()` 个字节（驱动从整段数据域里切出来）。
// 返回的 float64 已经换算到规范单位（m³ / m³/h / GJ / kW），**未**施加
// 点位 Scale/Bias——那一步由采集管线统一做。
func DecodeField(f Field, raw []byte) (float64, error) {
	if len(raw) < f.Width() {
		return 0, fmt.Errorf("cjt188: %s 需要 %d 字节，实际 %d", f.Name, f.Width(), len(raw))
	}
	switch f.Kind {
	case FieldTime:
		t, err := ParseTime(raw[:7])
		if err != nil {
			return 0, err
		}
		return CompactTime(t), nil
	case FieldStatus:
		return float64(uint16(raw[0]) | uint16(raw[1])<<8), nil
	case FieldNumber:
		v, err := DecodeBCD(raw[:f.Length])
		if err != nil {
			return 0, fmt.Errorf("%s：%w", f.Name, err)
		}
		if f.HasUnit() {
			code := raw[f.Length]
			factor, ok := UnitFactor(f.Qty, code)
			if !ok {
				return 0, fmt.Errorf("cjt188: %s 的单位代号 0x%02X 不是标准 %s 单位",
					f.Name, code, f.Qty.Unit())
			}
			v *= factor
		}
		if f.Decimals > 0 {
			v /= pow10(f.Decimals)
		}
		return v, nil
	}
	return 0, fmt.Errorf("cjt188: %s 的字段类型 %d 未实现", f.Name, f.Kind)
}

// FormatField 生成用于诊断/日志的人读文本。
func FormatField(f Field, raw []byte) string {
	switch f.Kind {
	case FieldTime:
		t, err := ParseTime(raw)
		if err != nil {
			return HexBytes(raw)
		}
		return t.Format("2006-01-02 15:04:05")
	case FieldStatus:
		if len(raw) < 2 {
			return HexBytes(raw)
		}
		v := uint16(raw[0]) | uint16(raw[1])<<8
		return fmt.Sprintf("0x%04X（%s）", v, StatusText(v))
	}
	v, err := DecodeField(f, raw)
	if err != nil {
		return HexBytes(raw)
	}
	unit := f.Qty.Unit()
	if unit == "" {
		switch f.Name {
		case "供水温度", "回水温度":
			unit = "℃"
		case "累积工作时间":
			unit = "h"
		}
	}
	if unit != "" {
		return TrimFloat(v) + " " + unit
	}
	return TrimFloat(v)
}

// DecodeBCD 按「低字节在前」解码压缩 BCD，返回整数值。
//
// 每个半字节都必须在 0..9。0xFF 在 BCD 里不是数字，而是「无效/未使用」；
// 把它当成 15 会让结算日流量变成一个天文数字，且半字节检查通不过的
// 字节往往正是表在说「这一项我没有」。
func DecodeBCD(raw []byte) (float64, error) {
	if len(raw) == 0 || len(raw) > 8 {
		return 0, fmt.Errorf("cjt188: BCD 宽度 %d 字节不受支持（1..8）", len(raw))
	}
	var v uint64
	for i := len(raw) - 1; i >= 0; i-- {
		b := raw[i]
		hi, lo := b>>4, b&0x0F
		if hi > 9 || lo > 9 {
			return 0, fmt.Errorf("cjt188: 字节 %d（0x%02X）不是合法 BCD", i, b)
		}
		v = v*100 + uint64(hi)*10 + uint64(lo)
	}
	return float64(v), nil
}

// EncodeBCD 把整数编码成定宽压缩 BCD（低字节在前）。
//
// 只给内置从站与测试用：主站写 CJ/T 188 的入口只有阀门与地址，没有
// 「按 BCD 写一个数」的命令。
func EncodeBCD(v uint64, width int) ([]byte, error) {
	if width < 1 || width > 8 {
		return nil, fmt.Errorf("cjt188: BCD 宽度 %d 字节不受支持（1..8）", width)
	}
	limit := uint64(1)
	for i := 0; i < width*2; i++ {
		limit *= 10
	}
	if v >= limit {
		return nil, fmt.Errorf("cjt188: 值 %d 超出 %d 字节 BCD 的范围", v, width)
	}
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		lo := byte(v % 10)
		v /= 10
		hi := byte(v % 10)
		v /= 10
		out[i] = hi<<4 | lo
	}
	return out, nil
}

// EncodeScaled 把工程值编码成「BCD + 单位代号」字段（模拟从站用）。
func EncodeScaled(v float64, length, decimals int, qty Qty, unit byte) ([]byte, error) {
	if v < 0 {
		return nil, fmt.Errorf("cjt188: 计量值不接受负值")
	}
	if decimals > 0 {
		v *= pow10(decimals)
	}
	n := uint64(v + 0.5)
	b, err := EncodeBCD(n, length)
	if err != nil {
		return nil, err
	}
	if qty != QtyNone {
		b = append(b, unit)
	}
	return b, nil
}

// ParseTime 解析 7 字节实时时间。
//
// 布局（低字节在前）：秒 分 时 日 月 年低 年高。每个日历年字段都是 BCD；
// 年份是两字节拼成的 4 位十进制（0x15 0x20 → 2015）。日期在本地校验，
// 2 月 30 日、13 月不会走到 time.Date 的归一化分支。
func ParseTime(raw []byte) (time.Time, error) {
	if len(raw) < 7 {
		return time.Time{}, fmt.Errorf("cjt188: 时间需要 7 字节，实际 %d", len(raw))
	}
	parts := make([]int, 7)
	for i := 0; i < 7; i++ {
		b := raw[i]
		hi, lo := b>>4, b&0x0F
		if hi > 9 || lo > 9 {
			return time.Time{}, fmt.Errorf("cjt188: 时间字节 %d（0x%02X）不是合法 BCD", i, b)
		}
		parts[i] = int(hi)*10 + int(lo)
	}
	sec, min, hour, day, month := parts[0], parts[1], parts[2], parts[3], parts[4]
	year := parts[6]*100 + parts[5]
	if year < 2000 || year > 2099 {
		return time.Time{}, fmt.Errorf("cjt188: 年份 %d 超出 2000..2099", year)
	}
	if err := CheckDateParts(month, day, hour, min, sec); err != nil {
		return time.Time{}, err
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	// 字段都合法但日期不存在（2 月 30 日）时 time.Date 会进到下个月。
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, fmt.Errorf("cjt188: %04d-%02d-%02d 不存在", year, month, day)
	}
	return t, nil
}

// TimeBytes 把时间编码成 7 字节（低字节在前）。
func TimeBytes(t time.Time) []byte {
	bcd := func(n int) byte { return byte(n/10)<<4 | byte(n%10) }
	return []byte{
		bcd(t.Second()), bcd(t.Minute()), bcd(t.Hour()),
		bcd(t.Day()), bcd(int(t.Month())),
		bcd(t.Year() % 100), bcd(t.Year() / 100),
	}
}

// CompactTime 把时间压成 YYYYMMDDhhmmss。
//
// 平台的 Value 没有时间类型；压成一个可排序的数值比字符串更好比较，
// 北向与历史库都能直接按数值排序。驱动写回时用 FromCompactTime 解回来。
func CompactTime(t time.Time) float64 {
	return float64(t.Year())*1e10 + float64(t.Month())*1e8 +
		float64(t.Day())*1e6 + float64(t.Hour())*1e4 +
		float64(t.Minute())*1e2 + float64(t.Second())
}

// FromCompactTime 把 YYYYMMDDhhmmss 还原成时间。
func FromCompactTime(v float64) (time.Time, error) {
	n := int64(v)
	sec := int(n % 100)
	n /= 100
	min := int(n % 100)
	n /= 100
	hour := int(n % 100)
	n /= 100
	day := int(n % 100)
	n /= 100
	month := int(n % 100)
	n /= 100
	year := int(n)
	if year < 100 {
		year += 2000
	}
	if year < 2000 || year > 2099 {
		return time.Time{}, fmt.Errorf("cjt188: 年份 %d 超出 2000..2099", year)
	}
	if err := CheckDateParts(month, day, hour, min, sec); err != nil {
		return time.Time{}, err
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, fmt.Errorf("cjt188: %04d-%02d-%02d 不存在", year, month, day)
	}
	return t, nil
}

// CheckDateParts 校验时间各字段的范围。
func CheckDateParts(month, day, hour, min, sec int) error {
	switch {
	case month < 1 || month > 12:
		return fmt.Errorf("cjt188: 月份 %d 必须在 1..12", month)
	case day < 1 || day > 31:
		return fmt.Errorf("cjt188: 日 %d 必须在 1..31", day)
	case hour < 0 || hour > 23:
		return fmt.Errorf("cjt188: 小时 %d 必须在 0..23", hour)
	case min < 0 || min > 59:
		return fmt.Errorf("cjt188: 分钟 %d 必须在 0..59", min)
	case sec < 0 || sec > 59:
		return fmt.Errorf("cjt188: 秒 %d 必须在 0..59", sec)
	}
	return nil
}

// EncodeStatus 把状态字编码成 2 字节（低字节在前）。
func EncodeStatus(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

// StatusText 把状态字翻成人读文本。
//
// 标准只定义了第一字节的低 3 位：D0/D1 阀门状态，D2 电池欠压；第二字节
// 厂商自定义。厂商手册常把 D2 写成「读数故障」，所以这里对 D2 用中性
// 表述，避免把「欠压」误报成「读数故障」。
func StatusText(v uint16) string {
	parts := make([]string, 0, 4)
	switch v & 0x03 {
	case 0:
		parts = append(parts, "阀门开")
	case 1:
		parts = append(parts, "阀门关")
	case 3:
		parts = append(parts, "阀门异常")
	}
	if v&0x04 != 0 {
		parts = append(parts, "电池欠压/读数异常")
	}
	if v>>8 != 0 {
		parts = append(parts, fmt.Sprintf("厂商状态 0x%02X", byte(v>>8)))
	}
	if len(parts) == 0 {
		return "正常"
	}
	return strings.Join(parts, "，")
}

// UnitFactor 把单位代号换算到规范单位。
//
// 只收录标准表 13 里的代号；未知代号一律报错。现场见过把 0x00 当
// 「m³」的表，也见过把 L 写成 0x2C 的批次；默认成基准单位等于把错误
// 静默成 1000 倍偏差，那比读不到更难发现。
func UnitFactor(q Qty, code byte) (float64, bool) {
	switch q {
	case QtyVolume:
		switch code {
		case 0x2C: // m³
			return 1, true
		case 0x29: // L
			return 0.001, true
		}
	case QtyFlow:
		switch code {
		case 0x35: // m³/h
			return 1, true
		case 0x32: // L/h
			return 0.001, true
		}
	case QtyEnergy:
		switch code {
		case 0x11: // GJ
			return 1, true
		case 0x13: // GJ×100（分辨率 0.01 GJ）
			return 0.01, true
		case 0x0E: // MJ
			return 0.001, true
		case 0x0B: // kJ
			return 1e-6, true
		case 0x01: // J
			return 1e-9, true
		case 0x05: // kWh
			return 0.0036, true
		case 0x08: // MWh
			return 3.6, true
		case 0x02: // Wh
			return 3.6e-6, true
		case 0x0A: // MWh×100（分辨率 0.01 MWh）
			return 0.036, true
		}
	case QtyPower:
		switch code {
		case 0x17: // kW
			return 1, true
		case 0x14: // W
			return 0.001, true
		case 0x1A: // MW
			return 1000, true
		}
	}
	return 0, false
}

// UnitTable 返回某类工程量的单位代号表，用于错误提示与文档。
func UnitTable(q Qty) string {
	var pairs []string
	for _, code := range []byte{0x01, 0x02, 0x05, 0x08, 0x0A, 0x0B, 0x0E, 0x11, 0x13, 0x14, 0x17, 0x1A, 0x29, 0x2C, 0x32, 0x35} {
		if _, ok := UnitFactor(q, code); ok {
			pairs = append(pairs, fmt.Sprintf("%02X", code))
		}
	}
	return strings.Join(pairs, " ")
}

// HexBytes 返回低字节在前的高亮字节串（与线上顺序一致）。
func HexBytes(b []byte) string {
	var s strings.Builder
	for i := len(b) - 1; i >= 0; i-- {
		fmt.Fprintf(&s, "%02X", b[i])
	}
	return s.String()
}

// TrimFloat 去掉浮点数的多余小数位。
func TrimFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}
