// Package mc 实现三菱 MC 协议（3E 帧，二进制）的纯函数编解码。
//
// 只做「字节 ↔ 结构」：不建连接、不读时钟、不持有状态。主站与内置从站
// 共用这里的设备码表，避免两边对同一个区名的理解漂移。
//
// 3E 二进制帧的布局（请求）：
//
//	50 00 | 网络号 | PC 号 | 目标模块 IO(2) | 目标站号 | 数据长度(2) |
//	监视定时器(2) | 命令(2) | 子命令(2) | 设备码(1) | 起始设备号(3) |
//	点数(2) | [写入数据]
//
// 所有多字节字段都是小端。响应把「命令之后」换成「结束代码(2) + 数据」。
//
// 最常见的三个坑：
//  1. 起始设备号是 3 字节小端（不是 4 字节，也不是大端）；
//  2. 位软元件按位打包（首点在第一个字节的 bit0），不是一点一字节；
//  3. X/Y/B/W 的编号按十六进制理解。本实现把 offset 当作「线上数值」的
//     十进制表示：X1F 写 31，而不是让配置里出现无法用整数表达的字母。
package mc

import (
	"encoding/binary"
	"fmt"
	"strings"

	"uic/internal/driver"
)

// 帧常量。
const (
	// 副头部线上字节序是 50 00 / D0 00（首字节 0x50/0xD0）。MC 3E 二进制帧
	// 的其余多字节字段（命令、点数、软元件号）确实都是小端，唯独副头部
	// 不是——三菱 SLMP 规范的参考帧以 `50 00 00 FF FF 03 00 …` 开头，照
	// 「全帧小端」的直觉写常量会编出 00 50，真 PLC 会因副头部不识别而
	// 拒收。下面的数值按「小端编码后得到 50 00」来取，PutUint16 直接用。
	SubheaderRequest  = 0x0050 // LE 编码 → 线上 50 00
	SubheaderResponse = 0x00D0 // LE 编码 → 线上 D0 00

	CommandRead  = 0x0401
	CommandWrite = 0x1401

	SubcommandWord = 0x0000
	SubcommandBit  = 0x0001

	// DefaultTimer 是监视定时器，单位 250ms（0x0010 = 4 秒）。
	DefaultTimer = 0x0010

	maxWordPoints = 960
	maxBitPoints  = 7168
	maxHeadDevice = 0xFFFFFF
)

// Device 描述一个软元件区。
type Device struct {
	Code byte
	// Bit 表示按位寻址（M/X/Y/B/F/L/SM/SB 等）。位软元件一次读写按位
	//打包；字软元件按 16 位字读写。
	Bit  bool
	Name string
	// areaKey 是内部使用的 ASCII 键（小写区名），仅供从站内存表使用。
	areaKey string
}

func (d Device) AreaKey() string {
	if d.areaKey != "" {
		return d.areaKey
	}
	for k, dev := range devices {
		if dev.Code == d.Code {
			return k
		}
	}
	return ""
}

// devices 是支持的软元件表。键是配置里使用的区名（小写）。
//
// 只收录常用区：D/W/R/ZR 与 M/X/Y/B/F/L/SM/SD/TN/CN/SW/SB。未收录的区
// 在配置期就报错，而不是发到 PLC 上等一个 0xC056。
var devices = map[string]Device{
	"d":  {Code: 0xA8, Name: "数据寄存器 D"},
	"w":  {Code: 0xB4, Name: "链接寄存器 W"},
	"r":  {Code: 0xAF, Name: "文件寄存器 R"},
	"zr": {Code: 0xB0, Name: "文件寄存器 ZR"},
	"sd": {Code: 0xA9, Name: "特殊数据寄存器 SD"},
	"sw": {Code: 0xB5, Name: "链接特殊寄存器 SW"},
	"tn": {Code: 0xC2, Name: "定时器当前值 TN"},
	"cn": {Code: 0xC5, Name: "计数器当前值 CN"},

	"m":  {Code: 0x90, Bit: true, Name: "内部继电器 M"},
	"x":  {Code: 0x9C, Bit: true, Name: "输入 X"},
	"y":  {Code: 0x9D, Bit: true, Name: "输出 Y"},
	"b":  {Code: 0xA0, Bit: true, Name: "链接继电器 B"},
	"f":  {Code: 0x93, Bit: true, Name: "报警器 F"},
	"l":  {Code: 0x92, Bit: true, Name: "锁存继电器 L"},
	"sm": {Code: 0x91, Bit: true, Name: "特殊继电器 SM"},
	"sb": {Code: 0xA1, Bit: true, Name: "链接特殊继电器 SB"},
	"s":  {Code: 0x98, Bit: true, Name: "步进继电器 S"},
}

// Lookup 按区名查设备定义。大小写不敏感：MC 的区名在文档与配置里大小写
// 随意（M/m、X/x），驱动内部统一转小写，但这里仍兜一层，避免把「配了个大
// 写 M」当成未知区处理。
func Lookup(area string) (Device, bool) {
	key := strings.ToLower(area)
	d, ok := devices[key]
	if ok {
		d.areaKey = key
	}
	return d, ok
}

// ByCode 按设备码反查区定义（从站解析请求时用）。
func ByCode(code byte) (Device, bool) {
	for _, d := range devices {
		if d.Code == code {
			return d, true
		}
	}
	return Device{}, false
}

// Areas 返回支持的区名（固定顺序，便于错误信息与文档）。
func Areas() []string {
	out := []string{
		"d", "w", "r", "zr", "sd", "sw", "tn", "cn",
		"m", "x", "y", "b", "f", "l", "sm", "sb", "s",
	}
	return out
}

// IsBitArea 返回区名是否是位软元件。
func IsBitArea(area string) bool {
	d, ok := Lookup(area)
	return ok && d.Bit
}

// BuildRead 构造批量读请求。
func BuildRead(code byte, head, points int, bit bool) ([]byte, error) {
	return build(code, CommandRead, head, points, bit, nil)
}

// BuildWrite 构造批量写请求。data 是已按协议排列好的原始数据：
// 字单位每点 2 字节（小端），位单位按位打包。
func BuildWrite(code byte, head, points int, bit bool, data []byte) ([]byte, error) {
	if bit {
		want := (points + 7) / 8
		if len(data) != want {
			return nil, fmt.Errorf("mc: bit write needs %d bytes for %d points, got %d", want, points, len(data))
		}
	} else if len(data) != points*2 {
		return nil, fmt.Errorf("mc: word write needs %d bytes for %d points, got %d", points*2, points, len(data))
	}
	return build(code, CommandWrite, head, points, bit, data)
}

func build(code byte, command, head, points int, bit bool, data []byte) ([]byte, error) {
	if head < 0 || head > maxHeadDevice {
		return nil, fmt.Errorf("mc: head device %d out of range 0..%#x", head, maxHeadDevice)
	}
	limit := maxWordPoints
	sub := SubcommandWord
	if bit {
		limit = maxBitPoints
		sub = SubcommandBit
	}
	if points <= 0 || points > limit {
		return nil, fmt.Errorf("mc: points %d out of range 1..%d", points, limit)
	}

	payload := 2 + 2 + 2 + 1 + 3 + 2 + len(data)
	out := make([]byte, 9+payload)
	binary.LittleEndian.PutUint16(out[0:], SubheaderRequest)
	out[2] = 0x00 // 网络号
	out[3] = 0xFF // PC 号
	out[4] = 0xFF // 目标模块 IO 号（低）
	out[5] = 0x03 // 目标模块 IO 号（高）
	out[6] = 0x00 // 目标站号
	binary.LittleEndian.PutUint16(out[7:], uint16(payload))
	binary.LittleEndian.PutUint16(out[9:], DefaultTimer)
	binary.LittleEndian.PutUint16(out[11:], uint16(command))
	binary.LittleEndian.PutUint16(out[13:], uint16(sub))
	out[15] = code
	putUint24(out[16:], head)
	binary.LittleEndian.PutUint16(out[19:], uint16(points))
	copy(out[21:], data)
	return out, nil
}

// ParseResponse 校验响应帧并返回数据区。结束代码非零时返回 *EndCodeError。
func ParseResponse(raw []byte) ([]byte, error) {
	if len(raw) < 11 {
		return nil, fmt.Errorf("mc: response too short (%d bytes)", len(raw))
	}
	if binary.LittleEndian.Uint16(raw[0:]) != SubheaderResponse {
		return nil, fmt.Errorf("mc: bad response subheader % x", raw[0:2])
	}
	dataLen := int(binary.LittleEndian.Uint16(raw[7:]))
	if dataLen < 2 {
		return nil, fmt.Errorf("mc: response data length %d < 2", dataLen)
	}
	if want := 9 + dataLen; len(raw) != want {
		return nil, fmt.Errorf("mc: response length %d, header says %d", len(raw), want)
	}
	code := binary.LittleEndian.Uint16(raw[9:])
	if code != 0 {
		return nil, &EndCodeError{Code: code}
	}
	return raw[11:], nil
}

// EndCodeError 是 PLC 返回的非零结束代码。
type EndCodeError struct {
	Code uint16
}

func (e *EndCodeError) Error() string {
	return fmt.Sprintf("mc: end code %#04x: %s", e.Code, e.CodeText())
}

// Kind 把结束码映射成驱动错误种类。
//
// 没有这一层，`driver.KindOf` 会落到兜底的 ErrInternal，于是：
//   - `CountsTowardBreaker` 为 true —— 一个点位名写错（0xC052/0xC056）会让
//     整台设备被熔断；
//   - `IsLinkFailure` 为 false 但质量码只能靠驱动自己再查一遍结束码表，
//     API 层与采集层对同一条错误给出不同结论。
//
// 分类依据是「重发一次结果会不会变」：
//   - 地址类（软元件不存在、起始/点数越界、数据长度不符）→ ErrAddress；
//   - 命令/子命令不支持 → ErrUnsupported（配置错，不是链路问题）；
//   - 写保护 → ErrPermission；
//   - 其余（网络号/PC 号/站号/监视定时器错、请求无法处理）→ ErrProtocol。
//
// 这些码都不带 Temporary：MC 结束码是设备对「这条请求」的确定答复，
// 重发同样的请求只会得到同样的码。
func (e *EndCodeError) Kind() driver.ErrorKind {
	switch e.Code {
	case 0xC052, // head device invalid
		0xC053, // points invalid
		0xC054, // head device + points out of range
		0xC056, // device not supported
		0xC058, // data length mismatch
		0xC05B: // write data invalid
		return driver.ErrAddress
	case 0xC050, // command not supported
		0xC051: // subcommand not supported
		return driver.ErrUnsupported
	case 0xC06F: // device memory write protected
		return driver.ErrPermission
	}
	return driver.ErrProtocol
}

// CodeText 给出结束代码的人话说明。未收录的代码保留编号，不猜。
func (e *EndCodeError) CodeText() string {
	if s, ok := endCodes[e.Code]; ok {
		return s
	}
	return "unknown end code"
}

var endCodes = map[uint16]string{
	0xC050: "command not supported",
	0xC051: "subcommand not supported",
	0xC052: "head device invalid",
	0xC053: "points invalid",
	0xC054: "head device + points out of range",
	0xC056: "device not supported",
	0xC058: "data length mismatch",
	0xC059: "command format error",
	0xC05B: "write data invalid",
	0xC05C: "request data length invalid",
	0xC05F: "request cannot be processed",
	0xC060: "network number invalid",
	0xC061: "PC number invalid",
	0xC062: "module IO number invalid",
	0xC063: "station number invalid",
	0xC064: "monitoring timer invalid",
	0xC06F: "device memory write protected",
}

// BuildResponse 构造响应帧（供内置从站使用）。
func BuildResponse(endCode uint16, data []byte) []byte {
	out := make([]byte, 11+len(data))
	binary.LittleEndian.PutUint16(out[0:], SubheaderResponse)
	out[2] = 0x00
	out[3] = 0xFF
	out[4] = 0xFF
	out[5] = 0x03
	out[6] = 0x00
	binary.LittleEndian.PutUint16(out[7:], uint16(2+len(data)))
	binary.LittleEndian.PutUint16(out[9:], endCode)
	copy(out[11:], data)
	return out
}

// PackBits 把布尔序列按位打包（首点 = 第一个字节的 bit0）。
func PackBits(bits []bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b {
			out[i/8] |= 1 << uint(i%8)
		}
	}
	return out
}

// UnpackBits 是 PackBits 的逆操作；bits 的长度由调用方决定。
func UnpackBits(data []byte, n int) []bool {
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		if i/8 < len(data) {
			out[i] = data[i/8]&(1<<uint(i%8)) != 0
		}
	}
	return out
}

// PutWord/PutUint24/GetUint24 是驱动与从站共用的小工具。
func PutWord(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }

func putUint24(b []byte, v int) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
}

// Uint24 读取 3 字节小端整数。
func Uint24(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}
