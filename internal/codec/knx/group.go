// Package knx 是 KNXnet/IP 的纯函数编解码层：KNXnet/IP 信封（服务类型、
// HPAI、连接与隧道报文）+ cEMI 链路层数据帧 + 组地址/物理地址 + 数据点类型
// （DPT）的取值编码。
//
// 只做「报文进、报文出」：不持有套接字、不读时钟、不做重传。序号配对、
// 通道保活、超时重连都在驱动里，因为它们依赖时钟与连接状态，而这一层必须
// 保持「同一份输入永远得到同一份输出」，否则金帧测试与主站/从站互测都会
// 变成偶发失败。
//
// 覆盖范围是刻意的：只做 TUNNELLING（UDP 单播隧道）+ 组地址读写。
//
//   - **不做 ROUTING（0x0530 组播）**：路由模式没有连接状态与序号，看起来
//     更简单，但它把「谁发的、发给谁」交给组播网段，多网段现场不可控；
//     而且路由模式收不到 L_Data.con，丢包无法与「设备没响应」区分。
//   - **不做 DEVICE_CONFIGURATION（0x0310）**：那是给 ETS 做设备下载用的
//     属性读写通道，与「采集点位」是两件事，混进来会把驱动变成半个配置工具。
//   - **不做 KNX Secure（0x0950 系列）**：会话密钥协商是独立子系统。
//
// 一个容易被忽略的事实：KNX 的组地址**不携带类型**。同一个 `1/2/3` 在
// A 工程里是 1 位开关，在 B 工程里是 2 字节浮点温度。类型只存在于工程的
// 组地址表里，因此本包要求调用方显式给出 DPT，而不是从 DATA 长度去猜——
// 猜错不会报错，只会把 21.5 读成 3.7e-41。
package knx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GroupAddress 是组地址（通信对象地址）。总线上是两个字节，工程里习惯
// 写成三级 `主组/中间组/子组`。
//
// 三级表示法把 16 位切成 5 + 3 + 8：
//
//	main   0..31   （5 位）
//	middle 0..7    （3 位）
//	sub    0..255  （8 位）
//
// 也接受两级表示法 `主组/子组`：把低 11 位当成一个整的子组号（0..2047）。
// 两种写法指向同一批地址，区别只在工程习惯，因此这里统一归一到 16 位原始
// 值，输出时一律用三级形式——同一个地址在日志、点表、北向节点名里必须是
// 同一个字符串。
type GroupAddress uint16

// MaxGroupAddress 是组地址的上限。
const MaxGroupAddress = 0xFFFF

// NewGroupAddress 用三级表示法构造组地址。
func NewGroupAddress(main, middle, sub int) GroupAddress {
	return GroupAddress(main&0x1F)<<11 | GroupAddress(middle&0x07)<<8 | GroupAddress(sub&0xFF)
}

// NewGroupAddress2 用两级表示法构造组地址。
func NewGroupAddress2(main, sub int) GroupAddress {
	return GroupAddress(main&0x1F)<<11 | GroupAddress(sub&0x7FF)
}

// ParseGroupAddress 解析组地址文本。
//
// 接受三种写法，与工程工具的输入习惯一致：
//
//	1/2/3     三级：主组 0..31、中间组 0..7、子组 0..255
//	1/300     两级：主组 0..31、子组 0..2047
//	2307      原始 16 位值（十进制），也接受 0x 前缀
//
// 0/0/0 被拒绝：它是「没有地址」，不是合法对象。把它当成合法地址会让
// 「点表里地址列写漏了」变成「一直在读某个不存在的对象」，而后者只在
// 超时日志里表现为一句没有信息量的 no response。
func ParseGroupAddress(s string) (GroupAddress, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("knx: group address is empty")
	}
	// 原始数值写法：单段，允许 0x 前缀。
	if !strings.ContainsRune(s, '/') {
		n, err := parseUint(s, 0)
		if err != nil {
			return 0, fmt.Errorf("knx: group address %q: %w", s, err)
		}
		if n == 0 {
			return 0, fmt.Errorf("knx: group address %q: 0 is not a valid group address", s)
		}
		if n > MaxGroupAddress {
			return 0, fmt.Errorf("knx: group address %q exceeds 16 bits (%d)", s, n)
		}
		return GroupAddress(n), nil
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		main, err := parseUint(parts[0], 10)
		if err != nil {
			return 0, fmt.Errorf("knx: group address %q: main group: %w", s, err)
		}
		sub, err := parseUint(parts[1], 10)
		if err != nil {
			return 0, fmt.Errorf("knx: group address %q: sub group: %w", s, err)
		}
		if main > 0x1F {
			return 0, fmt.Errorf("knx: group address %q: main group must be 0..31, got %d", s, main)
		}
		if sub > 0x7FF {
			return 0, fmt.Errorf("knx: group address %q: sub group must be 0..2047 in the two-level form, got %d", s, sub)
		}
		if main == 0 && sub == 0 {
			return 0, fmt.Errorf("knx: group address %q: 0/0 is not a valid group address", s)
		}
		return NewGroupAddress2(int(main), int(sub)), nil
	case 3:
		main, err := parseUint(parts[0], 10)
		if err != nil {
			return 0, fmt.Errorf("knx: group address %q: main group: %w", s, err)
		}
		middle, err := parseUint(parts[1], 10)
		if err != nil {
			return 0, fmt.Errorf("knx: group address %q: middle group: %w", s, err)
		}
		sub, err := parseUint(parts[2], 10)
		if err != nil {
			return 0, fmt.Errorf("knx: group address %q: sub group: %w", s, err)
		}
		if main > 0x1F {
			return 0, fmt.Errorf("knx: group address %q: main group must be 0..31, got %d", s, main)
		}
		if middle > 0x07 {
			return 0, fmt.Errorf("knx: group address %q: middle group must be 0..7, got %d", s, middle)
		}
		if sub > 0xFF {
			return 0, fmt.Errorf("knx: group address %q: sub group must be 0..255, got %d", s, sub)
		}
		if main == 0 && middle == 0 && sub == 0 {
			return 0, fmt.Errorf("knx: group address %q: 0/0/0 is not a valid group address", s)
		}
		return NewGroupAddress(int(main), int(middle), int(sub)), nil
	}
	return 0, fmt.Errorf("knx: group address %q: want 主组/中间组/子组, 主组/子组 or a raw 16-bit value", s)
}

// Main 返回主组（5 位）。
func (g GroupAddress) Main() int { return int(g>>11) & 0x1F }

// Middle 返回中间组（3 位）。
func (g GroupAddress) Middle() int { return int(g>>8) & 0x07 }

// Sub 返回子组（8 位）。
func (g GroupAddress) Sub() int { return int(g) & 0xFF }

// String 输出三级表示法。
func (g GroupAddress) String() string {
	return fmt.Sprintf("%d/%d/%d", g.Main(), g.Middle(), g.Sub())
}

// IndividualAddress 是物理地址，用来标识总线上的某一台设备。
//
// 三级表示法把 16 位切成 4 + 4 + 8：区域 0..15、线路 0..15、设备 0..255。
// 与组地址不同，物理地址在隧道模式下由网关分配（客户端自己声明一个「期望
// 值」，网关可以改），因此驱动只需能解析它来做日志与源地址过滤。
type IndividualAddress uint16

// ParseIndividualAddress 解析物理地址文本：`区域.线路.设备`、`区域.线路`
// 或原始 16 位值（十进制 / 0x 前缀）。
func ParseIndividualAddress(s string) (IndividualAddress, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("knx: individual address is empty")
	}
	if !strings.ContainsRune(s, '.') {
		n, err := parseUint(s, 0)
		if err != nil {
			return 0, fmt.Errorf("knx: individual address %q: %w", s, err)
		}
		if n > MaxGroupAddress {
			return 0, fmt.Errorf("knx: individual address %q exceeds 16 bits (%d)", s, n)
		}
		return IndividualAddress(n), nil
	}
	parts := strings.Split(s, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := parseUint(p, 10)
		if err != nil {
			return 0, fmt.Errorf("knx: individual address %q: %w", s, err)
		}
		nums = append(nums, int(n))
	}
	switch len(nums) {
	case 2:
		if nums[0] > 0xFF || nums[1] > 0xFF {
			return 0, fmt.Errorf("knx: individual address %q: both fields must be 0..255", s)
		}
		return IndividualAddress(nums[0]<<8 | nums[1]), nil
	case 3:
		if nums[0] > 0x0F || nums[1] > 0x0F || nums[2] > 0xFF {
			return 0, fmt.Errorf("knx: individual address %q: want 区域 0..15 . 线路 0..15 . 设备 0..255", s)
		}
		return IndividualAddress(nums[0]<<12 | nums[1]<<8 | nums[2]), nil
	}
	return 0, fmt.Errorf("knx: individual address %q: want 区域.线路.设备, 区域.线路 or a raw 16-bit value", s)
}

// String 输出三级表示法。
func (a IndividualAddress) String() string {
	return fmt.Sprintf("%d.%d.%d", int(a>>12)&0x0F, int(a>>8)&0x0F, int(a)&0xFF)
}

// parseUint 解析无符号整数。
//
// 进制规则刻意分成两种调用场景：
//
//   - base=10：纯十进制。工程里的 `010` 几乎总是 10，而不是八进制 8。
//   - base=0：自动——`0x` 前缀走十六进制，其余走十进制。用于「原始 16 位
//     地址」这种既可能写 2350 也可能写 0x092D 的场合。
//
// 不用 strconv.ParseUint 的 base=0 处理十进制场景：它会把 `010` 读成 8。
func parseUint(s string, base int) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty number")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("%q is negative", s)
	}
	if base == 10 {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a decimal number", s)
		}
		return v, nil
	}
	// 自动进制：只认 0x / 0X 前缀为十六进制。
	if len(s) > 2 && (strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X")) {
		v, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a hex number", s)
		}
		return v, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a decimal number (use 0x for hex)", s)
	}
	return v, nil
}
