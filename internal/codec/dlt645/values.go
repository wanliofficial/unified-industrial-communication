package dlt645

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件只做一件事：把数据域里的若干字节解成「人可以看的数」。
//
// 有三个坑必须在同一处处理，否则会以三种不同的形式散落进驱动与从站：
//
//  1. **字节序是低字节在前**，与 Modbus/S7 的直觉相反。123456.78 kWh 在
//     线上是 78 56 34 12，不是 12 34 56 78。
//  2. **小数位不是从字节数推出来的**。同为 4 字节，电能是两位小数、功率
//     是四位；同为 2 字节，电压是一位、频率是两位。小数位来自 DI，不来自
//     长度——按长度猜必然把 220.0 V 读成 2200 V。
//  3. **符号位藏在最高字节的最高位**。所以 4 字节电能的有效位只有 7 位
//     BCD（最大 9999999.99），而不是 8 位。

// Decode 按 Item 解码数据域。
//
// raw 是减过 0x33 的数据域（不含 DI）。返回的 float64 已经按小数位换算过，
// **未**施加点位 Scale/Bias——那一步由采集管线统一做，驱动不得重复缩放。
func Decode(it Item, raw []byte) (float64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("dlt645: %s 数据域为空", it.Name)
	}
	switch it.Kind {
	case KindTime:
		t, err := ParseTime(raw)
		if err != nil {
			return 0, err
		}
		// 时间在平台侧没有专用类型，用「YYYYMMDDhhmmss」这种可排序的数字
		// 表达，比字符串更好比较：北向与历史库都能直接按数值排序。
		return float64(t.Year())*1e10 + float64(t.Month())*1e8 +
			float64(t.Day())*1e6 + float64(t.Hour())*1e4 +
			float64(t.Minute())*1e2 + float64(t.Second()), nil
	case KindDate:
		if len(raw) < 4 {
			return 0, fmt.Errorf("dlt645: %s 需要 4 字节，实际 %d", it.Name, len(raw))
		}
		v, err := decodeBCD(raw[:4], true)
		if err != nil {
			return 0, err
		}
		// MMDDhhmm：直接给数值便于与事件时间比对。
		return v, nil
	case KindAddr:
		if len(raw) < 6 {
			return 0, fmt.Errorf("dlt645: %s 需要 6 字节，实际 %d", it.Name, len(raw))
		}
		v, err := decodeBCD(raw[:6], false)
		if err != nil {
			return 0, err
		}
		return v, nil
	case KindDemand:
		// 8 字节 = 4 字节需量 + 4 字节发生时间。平台一个点位只能承载一个
		// 数，因此只返回需量；发生时间由 Format 输出到诊断行里。
		if len(raw) < 8 {
			return 0, fmt.Errorf("dlt645: %s 需要 8 字节，实际 %d", it.Name, len(raw))
		}
		return decodeScaled(raw[:4], it.Decimals, it.Signed)
	case KindBits:
		var v uint64
		for i := len(raw) - 1; i >= 0; i-- {
			v = v<<8 | uint64(raw[i])
		}
		return float64(v), nil
	case KindBCD:
		return decodeBCD(raw, it.Signed)
	}
	return decodeScaled(raw, it.Decimals, it.Signed)
}

// Format 生成用于诊断/日志的人读文本。
//
// 与 Decode 分开：Decode 要给采集管线一个纯数值，Format 要给人看。把它们
// 合成一个返回值只会让其中一个妥协——要么数值带上单位字符串，要么时间
// 只剩一个看不懂的 20261107123456。
func Format(it Item, raw []byte) string {
	switch it.Kind {
	case KindTime:
		t, err := ParseTime(raw)
		if err != nil {
			return hexBytes(raw)
		}
		return t.Format("2006-01-02 15:04:05")
	case KindDate:
		if len(raw) < 4 {
			return hexBytes(raw)
		}
		v, err := decodeBCD(raw[:4], false)
		if err != nil {
			return hexBytes(raw)
		}
		mm := int(v) / 1000000
		dd := int(v) / 10000 % 100
		hh := int(v) / 100 % 100
		mi := int(v) % 100
		return fmt.Sprintf("%02d-%02d %02d:%02d", mm, dd, hh, mi)
	case KindAddr:
		if len(raw) < 6 {
			return hexBytes(raw)
		}
		var b strings.Builder
		for i := 5; i >= 0; i-- {
			fmt.Fprintf(&b, "%02X", raw[i])
		}
		return b.String()
	case KindDemand:
		if len(raw) < 8 {
			return hexBytes(raw)
		}
		v, err := decodeScaled(raw[:4], it.Decimals, it.Signed)
		if err != nil {
			return hexBytes(raw)
		}
		tv, err := decodeBCD(raw[4:8], false)
		if err != nil {
			return trimFloat(v)
		}
		mm := int(tv) / 1000000
		dd := int(tv) / 10000 % 100
		hh := int(tv) / 100 % 100
		mi := int(tv) % 100
		return fmt.Sprintf("%s（发生于 %02d-%02d %02d:%02d）", trimFloat(v), mm, dd, hh, mi)
	case KindBits:
		v, err := Decode(it, raw)
		if err != nil {
			return hexBytes(raw)
		}
		return fmt.Sprintf("0x%X", uint64(v))
	}
	v, err := Decode(it, raw)
	if err != nil {
		return hexBytes(raw)
	}
	return trimFloat(v)
}

// EncodeValue 把工程值编码为数据域的参数字节（不含 DI/密码/操作者代码）。
//
// 宽度取 Item.Width：写的时候必须写满标准宽度，否则电表按 L 长度校验会
// 直接拒绝。数据块（Width=0）不允许写——不知道要写几位就不该猜。
func EncodeValue(it Item, v float64) ([]byte, error) {
	if it.Width <= 0 {
		return nil, fmt.Errorf("dlt645: %s 不支持按数值写入（宽度未定义）", it.Name)
	}
	switch it.Kind {
	case KindTime:
		return nil, fmt.Errorf("dlt645: %s 是时间类型，请用 EncodeTime 写入", it.Name)
	case KindDate:
		return nil, fmt.Errorf("dlt645: %s 是日期类型，请用 EncodeDate 写入", it.Name)
	case KindAddr:
		return nil, fmt.Errorf("dlt645: %s 是地址类型，请用 EncodeAddr 写入", it.Name)
	}
	if v < 0 && !it.Signed {
		return nil, fmt.Errorf("dlt645: %s 不接受负值 %s", it.Name, trimFloat(v))
	}
	return encodeScaled(v, it.Width, it.Decimals, it.Signed)
}

// EncodeTime 编码 6 字节时间参数（写日期及时间）。
func EncodeTime(t time.Time) []byte {
	b := TimeBytes(t)
	return b[:]
}

// EncodeDate 编码 4 字节 MMDDhhmm 参数。
func EncodeDate(t time.Time) ([]byte, error) {
	return encodeScaled(float64(t.Month())*1e6+float64(t.Day())*1e4+
		float64(t.Hour())*1e2+float64(t.Minute()), 4, 0, false)
}

// EncodeAddr 编码 6 字节通信地址参数（低字节在前）。
func EncodeAddr(s string) ([]byte, error) {
	a, err := ParseAddr(s)
	if err != nil {
		return nil, err
	}
	for i := 0; i < AddrLen; i++ {
		if a[i] == Wildcard {
			return nil, fmt.Errorf("dlt645: 写入地址 %q 含通配符 AA", s)
		}
	}
	return a[:], nil
}

// decodeScaled 按小数位解码 BCD 数。
func decodeScaled(raw []byte, decimals int, signed bool) (float64, error) {
	v, err := decodeBCD(raw, signed)
	if err != nil {
		return 0, err
	}
	if decimals <= 0 {
		return v, nil
	}
	return v / pow10(decimals), nil
}

// decodeBCD 按「低字节在前 + 最高位符号位」解码压缩 BCD。
//
// 符号位只对带符号的数据项生效：无符号项（电压、频率）的最高位是正常数值
// 位，把它当成符号会把 220.0 V 读成 -9.2 V，而且看起来完全合理。
func decodeBCD(raw []byte, signed bool) (float64, error) {
	if len(raw) == 0 || len(raw) > 8 {
		return 0, fmt.Errorf("dlt645: BCD 宽度 %d 字节不受支持（1..8）", len(raw))
	}
	neg := false
	top := len(raw) - 1
	if signed && raw[top]&0x80 != 0 {
		neg = true
	}
	var v uint64
	for i := top; i >= 0; i-- {
		b := raw[i]
		if i == top && signed {
			b &^= 0x80
		}
		hi, lo := b>>4, b&0x0F
		if hi > 9 || lo > 9 {
			return 0, fmt.Errorf("dlt645: 字节 %d（0x%02X）不是合法 BCD", i, raw[i])
		}
		v = v*100 + uint64(hi)*10 + uint64(lo)
	}
	f := float64(v)
	if neg {
		f = -f
	}
	return f, nil
}

// encodeScaled 把数值编码为定宽 BCD（低字节在前，可带符号位）。
func encodeScaled(v float64, width, decimals int, signed bool) ([]byte, error) {
	if width < 1 || width > 8 {
		return nil, fmt.Errorf("dlt645: BCD 宽度 %d 字节不受支持（1..8）", width)
	}
	if decimals > 0 {
		v *= pow10(decimals)
	}
	neg := v < 0
	if neg {
		if !signed {
			return nil, fmt.Errorf("dlt645: 该数据项不接受负值")
		}
		v = -v
	}
	// 四舍五入：现场的设定值常常是 0.1 的倍数乘以缩放，浮点误差会让
	// 229.9999999 在截断后变成 229.9 而不是 230.0。
	rounded := v + 0.5
	limit := float64(uint64(1) << (4 * uint(width) * 2))
	if rounded >= limit {
		return nil, fmt.Errorf("dlt645: 值 %s 超出 %d 字节 BCD 的取值范围", trimFloat(v), width)
	}
	n := uint64(rounded)
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		lo := byte(n % 10)
		n /= 10
		hi := byte(n % 10)
		n /= 10
		out[i] = hi<<4 | lo
	}
	if neg {
		out[width-1] |= 0x80
	}
	return out, nil
}

// ParseScaled 是命令行/北向用来把文本转成数值的辅助函数（支持带单位后缀
// 与前后空白）。它不是协议的一部分，放在这里只是因为宽度/小数位的知识
// 都在这个包里。
func ParseScaled(it Item, s string) (float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimSuffix(s, "kwh")
	s = strings.TrimSuffix(s, "kvarh")
	s = strings.TrimPrefix(s, "+")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("dlt645: %q 不是数字", s)
	}
	return v, nil
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func hexBytes(b []byte) string {
	var s strings.Builder
	for i := len(b) - 1; i >= 0; i-- {
		fmt.Fprintf(&s, "%02X", b[i])
	}
	return s.String()
}
