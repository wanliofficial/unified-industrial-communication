package mbus

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// 本文件只做「把一段字节按某种编码读成一个数」。
//
// 之所以把这些函数单独摊开：M-Bus 的数据域有 12 种长度编码，其中整数有
// 1/2/3/4/6/8 字节六种（不是二的幂！），BCD 有 2/4/6/8/12 位五种。用
// 统一循环 + 位宽参数处理整数是对的；但 BCD 的符号位、REAL 的 IEEE754
// 布局、日期的时间有效性位各有一套规则，混在一个大 switch 里最容易出现
// "某一类表读出来差 100 倍"这种只在个别型号上复现的问题。

// decodeIntLE 按小端读一个有符号整数，宽度 1..8 字节。
func decodeIntLE(b []byte) (int64, bool) {
	switch len(b) {
	case 1:
		return int64(int8(b[0])), true
	case 2:
		return int64(int16(binary.LittleEndian.Uint16(b))), true
	case 3:
		v := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16
		if b[2]&0x80 != 0 {
			v |= ^int64(0xFFFFFF) // 符号扩展
		}
		return v, true
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(b))), true
	case 6:
		v := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16 |
			int64(b[3])<<24 | int64(b[4])<<32 | int64(b[5])<<40
		if b[5]&0x80 != 0 {
			v |= ^int64(0xFFFFFFFFFFFF)
		}
		return v, true
	case 8:
		return int64(binary.LittleEndian.Uint64(b)), true
	}
	return 0, false
}

// decodeReal 读 4 字节 IEEE 754 单精度（M-Bus 的 REAL 就是 float32，
// 小端）。用它而不是自己拼位，是因为 NaN/Inf 在表计上确实会出现
// （传感器断线时很多表直接上送 0x7FC00000）。
func decodeReal(b []byte) (float64, bool) {
	if len(b) != 4 {
		return 0, false
	}
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), true
}

// decodeBCD 解 BCD 编码。
//
// 规则（EN 13757-3 §6.4）：每字节两位十进制，**低位在前**；最高字节的
// 高半字节为 Fh 表示负数（该半字节不是数字）。因此 8 位 BCD 的
// `78 56 34 12` 是 12345678，而 `78 56 34 F2` 是 -1234567。
//
// 非 BCD 数字（A..F 出现在数字位上）返回 ok=false：这种情况在总线上
// 通常是记录边界错位，继续按数字解释只会得到"看起来像真值"的鬼数。
func decodeBCD(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 8 {
		return 0, false
	}
	var v int64
	neg := false
	mult := int64(1)
	for i := 0; i < len(b); i++ {
		lo := b[i] & 0x0F
		hi := b[i] >> 4
		if i == len(b)-1 && hi == 0x0F {
			// 最高字节的高半字节是符号位，不是数字。
			neg = true
			hi = 0
			if lo > 9 {
				return 0, false
			}
			v += int64(lo) * mult
			mult *= 10
			continue
		}
		if lo > 9 || hi > 9 {
			return 0, false
		}
		v += int64(lo) * mult
		mult *= 10
		v += int64(hi) * mult
		mult *= 10
	}
	if neg {
		return -v, true
	}
	return v, true
}

// decodeBCDDigits 解**纯数字 BCD**：每字节两位十进制、低位在前，但不接受
// 末字节高半字节的 Fh 符号位。
//
// 用于变长记录（LVAR = C0h..D9h）：那里符号由 LVAR 的 C/D 段给出，数据字节
// 必须是纯数字。若复用 decodeBCD，一条末字节高半字节为 F 的畸形记录会被它
// 当成负号，再被 D 段取负，负负得正、静默得出一个"看起来合理"的正数。
// 这里遇到任何非十进制半字节都返回 ok=false，交由调用方按原始字节暴露。
//
// 上限 9 字节（18 位十进制）：EN 13757-3 Table 5 的 BCD 段到 C9h/D9h 为止，
// 18 位最大 999999999999999999 仍在 int64 内，不能按 8 字节截断——那会把
// 合法的 18 位记录降级成原始字节。
func decodeBCDDigits(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 9 {
		return 0, false
	}
	var v int64
	mult := int64(1)
	for i := 0; i < len(b); i++ {
		lo := b[i] & 0x0F
		hi := b[i] >> 4
		if lo > 9 || hi > 9 {
			return 0, false
		}
		v += int64(lo) * mult
		mult *= 10
		v += int64(hi) * mult
		mult *= 10
	}
	return v, true
}

// decodeDate 解 Type G 日期（2 字节）。
//
//	bit 0-4  日
//	bit 5-7  年 bit 8-10
//	bit 8-11 月
//	bit 12-15 年 bit 4-7
func decodeDate(b []byte) (time.Time, bool) {
	if len(b) != 2 {
		return time.Time{}, false
	}
	day := int(b[0] & 0x1F)
	month := int(b[1] & 0x0F)
	year := 2000 + int((b[0]&0xE0)>>5) + int((b[1]&0xF0)>>1)
	if day == 0 || month == 0 || month > 12 {
		return time.Time{}, false
	}
	// 走 makeTime 而不是直接 time.Date：后者会把 2 月 31 日静默归一化成
	// 3 月 2 日，于是"日期字段错位"这块表就会稳定地报出一个不存在的日期。
	// 类型 G 没有时间部分，时分秒固定为 0。
	return makeTime(year, month, day, 0, 0, 0)
}

// decodeDateTime 解 Type F（4 字节）与 Type I（6 字节）日期时间。
//
// Type F 没有秒（只有分/时/日/月/年 + 世纪位），Type I 有秒。两种都
// 有一个"时间无效"位：传感器没接时钟的表会把它置位，此时应当明确报
// 不可用，而不是把 0xFF 解成 2159 年。
func decodeDateTime(b []byte) (time.Time, bool) {
	switch len(b) {
	case 4:
		if b[0]&0x80 != 0 {
			return time.Time{}, false
		}
		minute := int(b[0] & 0x3F)
		hour := int(b[1] & 0x1F)
		day := int(b[2] & 0x1F)
		month := int(b[3] & 0x0F)
		year := int((b[2]&0xE0)>>5) | int((b[3]&0xF0)>>1)
		century := int((b[1] & 0x60) >> 5)
		if century == 0 && year <= 80 {
			// 老表只用两位年：00..80 归到 2000 年代。
			century = 1
		}
		// century 是世纪号（0→19xx、1→20xx、2→21xx），不是年的前缀：
		// 直接写 `century*100+year` 会得到 124 年，被 makeTime 判为非法
		// 年份，于是每条 Type F 日期时间都变成"不可用"。这类错误不会报
		// 错，只会让一屏时间字段全是空的。
		return makeTime(1900+century*100+year, month, day, hour, minute, 0)
	case 6:
		if b[1]&0x80 != 0 {
			return time.Time{}, false
		}
		sec := int(b[0] & 0x3F)
		minute := int(b[1] & 0x3F)
		hour := int(b[2] & 0x1F)
		day := int(b[3] & 0x1F)
		month := int(b[4] & 0x0F)
		year := 2000 + int((b[3]&0xE0)>>5) + int((b[4]&0xF0)>>1)
		return makeTime(year, month, day, hour, minute, sec)
	}
	return time.Time{}, false
}

func makeTime(year, month, day, hour, minute, sec int) (time.Time, bool) {
	if month < 1 || month > 12 || day < 1 || day > 31 ||
		hour > 23 || minute > 59 || sec > 59 || year < 1900 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, 0, time.UTC)
	// time.Date 会把 2 月 30 日"归一化"成 3 月 2 日；日期字段错位时
	// 这种静默归一化会产出一个不存在的日期。回读校验一次代价极低。
	if t.Day() != day || int(t.Month()) != month {
		return time.Time{}, false
	}
	return t, true
}

// ManufacturerCode 把 2 字节厂商代码解成三位字母。
//
// 编码方式（EN 13757-3 §6.3）：16 位整数拆成三个 5 位字段，每段加 64
// 得到 'A'..'Z'。这个值在报文里是小端存放，但拆位从高位开始——顺序
// 搞反会得到"厂商字母倒着写"，而倒着的字母看起来同样像合法代码，
// 因此只能靠测试向量锁住。
func ManufacturerCode(b []byte) string {
	if len(b) != 2 {
		return ""
	}
	v := int(binary.LittleEndian.Uint16(b))
	out := make([]byte, 3)
	for i := 0; i < 3; i++ {
		c := byte(((v >> uint(10-5*i)) & 0x1F) + 64)
		if c < 'A' || c > 'Z' {
			return ""
		}
		out[i] = c
	}
	return string(out)
}

// DecodeManufacturerCode 是 ManufacturerCode 的逆运算，供内置从站造报文。
func DecodeManufacturerCode(code string) (uint16, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 3 {
		return 0, fmt.Errorf("厂商标识必须是 3 个字母，得到 %q", code)
	}
	var v uint16
	for i := 0; i < 3; i++ {
		c := code[i]
		if c < 'A' || c > 'Z' {
			return 0, fmt.Errorf("厂商标识只能含 A..Z，得到 %q", code)
		}
		v |= uint16(c-64) << uint(10-5*i)
	}
	return v, nil
}

// MediaName 返回介质名。编码见 EN 13757-3 表 5。
func MediaName(m byte) string {
	names := map[byte]string{
		0x00: "other", 0x01: "oil", 0x02: "electricity", 0x03: "gas",
		0x04: "heat", 0x05: "steam", 0x06: "hot water", 0x07: "water",
		0x08: "hca", 0x09: "compressed air",
		0x0A: "cooling load (return)", 0x0B: "cooling load (flow)",
		0x0C: "heat (flow)", 0x0D: "heat/cooling load", 0x0E: "bus/system",
		0x0F: "unknown", 0x10: "irrigation water", 0x11: "water data logger",
		0x12: "gas data logger", 0x13: "gas converter", 0x14: "heat value",
		0x15: "hot water >=90C", 0x16: "cold water", 0x17: "dual water",
		0x18: "pressure", 0x19: "a/d converter", 0x1A: "smoke detector",
		0x1B: "room sensor", 0x1C: "gas detector",
		0x20: "breaker", 0x21: "valve", 0x25: "customer unit",
		0x28: "waste water", 0x29: "waste",
	}
	if n, ok := names[m]; ok {
		return n
	}
	return fmt.Sprintf("medium-%02x", m)
}

// MediumHeat 是热表/冷表最常用的 medium 编码，供示例与自检使用。
const MediumHeat byte = 0x04

// BCDString 把 4 字节表号按 BCD 解成 8 位十进制字符串（高位补零）。
//
// 表号在报文里是低位在前；`12345678` 的字节序是 78 56 34 12。直接按
// 整数打印会丢掉前导零（表号 `00001234` 变成 `1234`），而现场核对表号
// 时前导零是有意义的。
func BCDString(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for i := len(b) - 1; i >= 0; i-- {
		sb.WriteByte('0' + (b[i] >> 4))
		sb.WriteByte('0' + (b[i] & 0x0F))
	}
	return sb.String()
}
