package dlms

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件处理 OBIS 码——DLMS 的「逻辑名」。
//
// # 它是什么
//
// 一个 COSEM 对象由三部分唯一确定：
//
//	类号（class id，1..~40）+ 逻辑名（OBIS，6 字节）+ 属性号（attribute id）
//
// 逻辑名在 LN 引用里是 **6 个字节**，书写形式是 `A-B-C-D-E-F`，每一段是
// 一个 0..255 的十进制数。例如正向有功电能是 `1-0-1-8-0-255`，编码为
// `01 00 01 08 00 FF`。
//
// # 六段的分工（IEC 62056-61）
//
//	A  介质/能量类型（0=抽象、1=电、2=热、3=气、4=水、5=热、6=热、7=气…）
//	B  通道号（同一物理表上的多路测量）
//	C  物理量（1=有功电能、31=电流、32=电压、33=功率因子…）
//	D  测量类型/费率（8=总量、7=瞬时值、1..6=费率 1..6）
//	E  统计类型（0=当前、1=累计、2=最大值…）
//	F  计费/存储号（255=当前值、0..N=历史数据块号）
//
// 这六段**不是**数学上的进制位，不能相加也不能按位运算。它们是六个独立
// 的索引，所以 `1-0-1-8-0-255` 与 `1-0-1-8-0-254` 是完全不同的两个对象
// （相邻两个历史块），而不是"相差 1"的同一个对象。
//
// # 为什么用字符串而不是 [6]byte
//
// 点表是人写的，`1.0.1.8.0.255` 比 `01 00 01 08 00 FF` 好核对；现场抄表
// 软件、Excel 点表、厂商手册都用点分十进制。因此对外接口接受/输出这个
// 字符串形式，内部再转 6 字节。**两种分隔符都接受**（`.` 与 `-`）：标准
// 文本用 `-`，而绝大多数厂商工具用 `.`，只认一种会让一半的点表直接报错。

// OBIS 是 6 字节逻辑名。
type OBIS [6]byte

// ParseOBIS 解析 `1.0.1.8.0.255` 或 `1-0-1-8-0-255` 或 `1-0:1-8-0-255`。
//
// 宽容但严格：
//   - 分隔符 `.`、`-`、`:` 与空格可以混用（见下面「为什么允许冒号」）；
//   - 每段允许前导零（`01` 与 `1` 等价）；
//   - 段数必须恰好 6，每段必须 0..255。
//
// 不做的事：不接受 `1.0.1.8.0`（5 段）或 `1.0.1.8.0.255.0`（7 段）然后
// "自动补 255" —— 补出来的对象可能真实存在但语义完全不同，把错误地址读成
// 一个"看起来合理"的值比直接报错危险得多。
//
// # 为什么允许冒号
//
// IEC 62056-61 的正文写法是 `A-B:C-D-E-F`：冒号是"介质/通道"组与"物理量/
// 费率"组之间的分隔符，而连字符是段之间的分隔符。例如 `1-0:1-8-0-255`。
// 这个写法在表厂商手册与校准报告里很常见，只认 `.` 会让这些点表直接报错。
//
// 冒号同时又是本包里 `<OBIS>:<class>:<attribute>` 的地址后缀分隔符，两者
// 靠**段数**区分：SplitRef 取"第一个能解析成 6 段的冒号前缀"。因为 6 段
// 恰好意味着 5 个分隔符，切点是被唯一确定的，不存在歧义。
func ParseOBIS(s string) (OBIS, error) {
	var o OBIS
	s = strings.TrimSpace(s)
	if s == "" {
		return o, fmt.Errorf("%w: 空 OBIS", ErrAXDR)
	}
	// 统一分隔符后切分。
	norm := strings.NewReplacer("-", ".", ":", ".", " ", ".").Replace(s)
	parts := strings.Split(norm, ".")
	if len(parts) != 6 {
		return o, fmt.Errorf("%w: OBIS %q 应为 6 段，得到 %d 段", ErrAXDR, s, len(parts))
	}
	for i, p := range parts {
		if p == "" {
			return o, fmt.Errorf("%w: OBIS %q 第 %d 段为空", ErrAXDR, s, i+1)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return o, fmt.Errorf("%w: OBIS %q 第 %d 段 %q 不是数字", ErrAXDR, s, i+1, p)
		}
		if n < 0 || n > 255 {
			return o, fmt.Errorf("%w: OBIS %q 第 %d 段 %d 超出 0..255", ErrAXDR, s, i+1, n)
		}
		o[i] = byte(n)
	}
	return o, nil
}

// SplitRef 把点表地址 `<OBIS>[:<class>[:<attribute>]]` 切开。
//
// 返回的 class / attribute 为 -1 表示地址里没写，需要调用方从内置表推导。
// 这是整个仓库里**唯一**该做这件事的地方：驱动与自检命令都必须走它，否则
// 两边对同一个地址字符串的解析会分叉——而"自检通过、采集失败"正是最难查
// 的一类问题。
func SplitRef(s string) (obisPart string, classID, attrID int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", -1, -1, fmt.Errorf("%w: 地址为空", ErrAXDR)
	}
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		head, tail := s[:i], s[i+1:]
		if _, e := ParseOBIS(head); e != nil {
			// 这个冒号属于 OBIS 内部（`1-0:1-8-0-255`），继续往后找。
			continue
		}
		c, a, e := parseRefSuffix(tail)
		if e != nil {
			// 前缀是合法 OBIS 但后缀不合法：说明这个冒号仍然是 OBIS
			// 内部的分隔符（例如 `1-0:1-8-0-255` 整体就是一个 OBIS）。
			continue
		}
		return strings.TrimSpace(head), c, a, nil
	}
	if _, e := ParseOBIS(s); e != nil {
		return "", -1, -1, e
	}
	return s, -1, -1, nil
}

// parseRefSuffix 解析 `:<class>[:<attribute>]` 的冒号后部分。
func parseRefSuffix(s string) (int, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) > 2 {
		return -1, -1, fmt.Errorf("后缀 %q 最多两段（class:attribute）", s)
	}
	classID, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || classID < 0 || classID > 0xFFFF {
		return -1, -1, fmt.Errorf("类号 %q 非法（0..65535）", parts[0])
	}
	attrID := -1
	if len(parts) == 2 {
		attrID, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || attrID < 0 || attrID > 127 {
			return -1, -1, fmt.Errorf("属性号 %q 非法（0..127）", parts[1])
		}
	}
	return classID, attrID, nil
}

// String 输出点分十进制形式（厂商工具通用写法）。
func (o OBIS) String() string {
	return fmt.Sprintf("%d.%d.%d.%d.%d.%d", o[0], o[1], o[2], o[3], o[4], o[5])
}

// Dash 输出带连字符的形式（标准正文写法）。
func (o OBIS) Dash() string {
	return fmt.Sprintf("%d-%d-%d-%d-%d-%d", o[0], o[1], o[2], o[3], o[4], o[5])
}

// Hex 输出 6 字节十六进制，用于诊断帧内容。
func (o OBIS) Hex() string { return fmt.Sprintf("% X", o[:]) }

// Group 返回 OBIS 的 A 段（介质/能量类型）。
func (o OBIS) Group() byte { return o[0] }

// Channel 返回 B 段。
func (o OBIS) Channel() byte { return o[1] }

// Quantity 返回 C 段。
func (o OBIS) Quantity() byte { return o[2] }

// Rate 返回 D 段（费率/测量类型）。
func (o OBIS) Rate() byte { return o[3] }

// Tariff 返回 E 段。
func (o OBIS) Tariff() byte { return o[4] }

// Storage 返回 F 段（存储号，255 表示当前值）。
func (o OBIS) Storage() byte { return o[5] }

// IsCurrent 表示这是「当前值」而不是历史块。
func (o OBIS) IsCurrent() bool { return o[5] == 255 }
