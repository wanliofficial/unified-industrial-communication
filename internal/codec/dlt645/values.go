package dlt645

import (
	"fmt"
	"math"
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
		// 读路径的字节序是 MM DD hh mm，与写路径的 DD MM hh mm 相反（见
		// EncodeDate 的说明）。这里按字段逐个解释，不能走 decodeScaled：
		// 后者会把 4 字节当小端带符号整数（0x31 被当成符号位），既给错值
		// 又对越界字段静默放行。
		if err := validBCDDate4(raw[:4]); err != nil {
			return 0, err
		}
		mon := int(raw[0]>>4)*10 + int(raw[0]&0xF)
		day := int(raw[1]>>4)*10 + int(raw[1]&0xF)
		hour := int(raw[2]>>4)*10 + int(raw[2]&0xF)
		min := int(raw[3]>>4)*10 + int(raw[3]&0xF)
		if err := ValidDateParts(mon, day, hour, min, 0); err != nil {
			return 0, fmt.Errorf("dlt645: %s % X: %w", it.Name, raw[:4], err)
		}
		// 平台一个点位只能承载一个数值。这里给 MMDDhhmm 这种可排序的紧凑
		// 形式（12312359 表示 12 月 31 日 23:59），比 8 位日期少一位年——
		// 结算日数据项本来就没有年。写回时由 fromDateCompact 识别这个形状。
		return float64(mon)*1e6 + float64(day)*1e4 + float64(hour)*1e2 + float64(min), nil
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
		//
		// 后半段必须按 **MM DD hh mm** 字段序解释（与日期数据项同形），
		// 不能当小端整数：否则「最大需量发生时间」会被读成另一个日期，
		// 而 Format 的诊断行会把它打印成合理-looking 的时间。这里只做
		// 合法性校验（不产出数值），错误在 Format 里同样会被触发。
		if len(raw) < 8 {
			return 0, fmt.Errorf("dlt645: %s 需要 8 字节，实际 %d", it.Name, len(raw))
		}
		if err := validBCDDate4(raw[4:8]); err != nil {
			return 0, fmt.Errorf("dlt645: %s 发生时间: %w", it.Name, err)
		}
		mon := int(raw[4]>>4)*10 + int(raw[4]&0xF)
		day := int(raw[5]>>4)*10 + int(raw[5]&0xF)
		hour := int(raw[6]>>4)*10 + int(raw[6]&0xF)
		min := int(raw[7]>>4)*10 + int(raw[7]&0xF)
		if err := ValidDateParts(mon, day, hour, min, 0); err != nil {
			return 0, fmt.Errorf("dlt645: %s 发生时间: %w", it.Name, err)
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
		// 与 Decode 对齐：MM DD hh mm 字段序。早先用 decodeBCD（小端整数）
		// 解释，读出的月/日/时/分整体错位。
		if err := validBCDDate4(raw[:4]); err != nil {
			return hexBytes(raw)
		}
		mon := int(raw[0]>>4)*10 + int(raw[0]&0xF)
		day := int(raw[1]>>4)*10 + int(raw[1]&0xF)
		hour := int(raw[2]>>4)*10 + int(raw[2]&0xF)
		min := int(raw[3]>>4)*10 + int(raw[3]&0xF)
		if err := ValidDateParts(mon, day, hour, min, 0); err != nil {
			return hexBytes(raw)
		}
		return fmt.Sprintf("%02d-%02d %02d:%02d", mon, day, hour, min)
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
		// 发生时间按 MM DD hh mm 字段序，与 Decode 的 KindDate 分支一致。
		mon := int(raw[4]>>4)*10 + int(raw[4]&0xF)
		day := int(raw[5]>>4)*10 + int(raw[5]&0xF)
		hour := int(raw[6]>>4)*10 + int(raw[6]&0xF)
		min := int(raw[7]>>4)*10 + int(raw[7]&0xF)
		if err := ValidDateParts(mon, day, hour, min, 0); err != nil {
			return trimFloat(v)
		}
		return fmt.Sprintf("%s（发生于 %02d-%02d %02d:%02d）", trimFloat(v), mon, day, hour, min)
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
	case KindBits:
		// 位域是二进制，与 Decode 的 KindBits 对称。绝不能落到底部的
		// encodeScaled（BCD）——那会把 0x12 写成 0x18、读回 24。
		return encodeBits(v, it.Width)
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

// EncodeDate 编码 4 字节日期参数（结算日、时区日等）。
//
// 字节序必须与 Decode 对齐：**MM DD hh mm**。Decode 是权威解释（读路径的
// 应答就是按这个顺序来的），写路径必须产出相同的字节序，否则「把刚读到的
// 结算日原样写回去」会得到一个日期错乱的日期——而且不会报错，只会让表在
// 错误的日子结算。
//
// 早先它走 encodeScaled（小端：DD MM hh mm），与 Decode 相反，这是一个真实的
// 编解码不对称缺陷。6 字节时间（ss mm hh DD MM YY）才是低字节在前，日期
// 数据项不是。
func EncodeDate(t time.Time) ([]byte, error) {
	if err := ValidDateParts(int(t.Month()), t.Day(), t.Hour(), t.Minute(), 0); err != nil {
		return nil, fmt.Errorf("dlt645: 日期 %s: %w", t.Format("01-02 15:04"), err)
	}
	return []byte{bcdByte(int(t.Month())), bcdByte(t.Day()), bcdByte(t.Hour()), bcdByte(t.Minute())}, nil
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

// validBCDDate4 校验 4 字节日期（MMDDhhmm）的每个半字节都是合法 BCD 数字。
func validBCDDate4(b []byte) error {
	for i, x := range b {
		if x>>4 > 9 || x&0x0F > 9 {
			return fmt.Errorf("dlt645: 日期第 %d 字节 0x%02X 不是 BCD", i+1, x)
		}
	}
	return nil
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
//
// 取值范围必须按 **BCD** 校验，不能按二进制字节数校验：width 字节的 BCD
// 字段最多表示 10^(2·width)-1，比 2^(8·width) 小得多。早先用二进制上界
// （1 << (4*width*2)）比较，导致 1 字节的 100 被静默截成 0x00、2 字节的
// 10000 被截成 0x0000——帧本身合法，电表照单全收，于是「写入成功」却是
// 一个完全不同的值，这正是最难查的一类缺陷。
//
// 带符号项还要再让出最高位（符号位），有效数值位少一位：1 字节有符号
// 项最大到 79，85 会被编成 0x85、再读回来变成 -5。
//
// 校验放在编码**之后**：走完整数位循环若还有剩余位数，说明十进制位没装
// 完（溢出）；带符号项再看最高字节的最高位是否与符号位重叠。比先算一个
// 浮点上界更准——宽字段（8 字节）的 10^16 已超出 float64 的整数精度。
func encodeScaled(v float64, width, decimals int, signed bool) ([]byte, error) {
	if width < 1 || width > 8 {
		return nil, fmt.Errorf("dlt645: BCD 宽度 %d 字节不受支持（1..8）", width)
	}
	if decimals > 0 {
		v *= pow10(decimals)
	}
	// 先拦非有限值与大得离谱的值：把超出 uint64 的浮点数转成整数在 Go 里
	// 是未定义行为，不能让它走到下面的 uint64(rounded)。
	if math.IsNaN(v) || math.IsInf(v, 0) || v >= 1e19 {
		return nil, fmt.Errorf("dlt645: 值超出 %d 字节 BCD 可表示范围", width)
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
	n := uint64(rounded)
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		lo := byte(n % 10)
		n /= 10
		hi := byte(n % 10)
		n /= 10
		out[i] = hi<<4 | lo
	}
	// 十进制位没装完 = 值太大。此前这里只截断、不报错，是静默回绕的根源。
	if n != 0 {
		return nil, fmt.Errorf("dlt645: 值 %s 超出 %d 字节 BCD 可表示范围", trimFloat(rounded-0.5), width)
	}
	// 带符号项的最高位是符号位；最高半字节超过 7 会与它重叠。
	if signed && out[width-1]&0x80 != 0 {
		return nil, fmt.Errorf("dlt645: 值 %s 超出 %d 字节有符号 BCD 范围（最高位是符号位）",
			trimFloat(rounded-0.5), width)
	}
	if neg {
		out[width-1] |= 0x80
	}
	return out, nil
}

// encodeBits 把位域值编码为定宽二进制整数（低字节在前），与 Decode 的
// KindBits 对称。
//
// 位域是**二进制**，不是 BCD：按 BCD 编码会把 0x12（=18）写成 0x18、再读
// 回来是 24，且不报任何错。周休日特征字这类可写位域项必须走这条路径。
func encodeBits(v float64, width int) ([]byte, error) {
	if width < 1 || width > 8 {
		return nil, fmt.Errorf("dlt645: 位域宽度 %d 字节不受支持（1..8）", width)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v != math.Trunc(v) {
		return nil, fmt.Errorf("dlt645: 位域值 %v 必须是非负整数", v)
	}
	max := math.Pow(2, float64(8*width))
	if v >= max {
		return nil, fmt.Errorf("dlt645: 位域值 %s 超出 %d 字节范围（最大 %s）",
			trimFloat(v), width, trimFloat(max-1))
	}
	n := uint64(v)
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		out[i] = byte(n)
		n >>= 8
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
