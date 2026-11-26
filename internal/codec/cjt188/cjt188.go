// Package cjt188 实现户用计量仪表数据传输协议（2004 版）的纯函数编解码。
//
// 它和同目录之外的 DL/T 645 看起来很像——都是 0x68 起始、0x16 结束、BCD
// 地址——但**不能共用一份代码**：
//
//	┌──────────────┬──────────────────────┬──────────────────────┐
//	│              │ DL/T 645-2007        │ CJ/T 188-2004        │
//	├──────────────┼──────────────────────┼──────────────────────┤
//	│ 起始符       │ 两个 0x68            │ 一个 0x68            │
//	│ 仪表类型     │ 无                   │ 1 字节 T             │
//	│ 地址         │ 6 字节 BCD           │ 7 字节 BCD（含厂商码）│
//	│ 数据域偏移   │ 每字节 +0x33         │ 无                   │
//	│ 数据标识     │ 4 字节 DI            │ 2 字节 DI            │
//	│ 序列号       │ 仅后续帧             │ 每次读/写都带 SER    │
//	│ 一个 DI      │ 一个值（数据块除外） │ 一组字段             │
//	└──────────────┴──────────────────────┴──────────────────────┘
//
// 帧结构：
//
//	FE × n │ 68 │ T │ A0 A1 A2 A3 A4 A5 A6 │ C │ L │ DATA… │ CS │ 16
//
// 三条容易写错的规则，各有注释与用例钉住：
//
//  1. **只有一个 0x68**，且数据域不加 0x33。把 645 的经验直接搬过来会让
//     校验和永远对不上。
//  2. **地址低字节先传，但数据标识 DI 是高字节先传。** 地址 A0 是最低字节、
//     数值的最低位字节在前，都遵守 6.4.2 的「先低位」；唯独 DI 是命令/应答的
//     标识字段，2004 版按大端书写——点表里 901F 上线发 90 1F（DI0=0x90 在前）。
//     把「先低位」无套到 DI 上会发出 1F 90，表把它当成一个不存在的标识而不应答。
//     注意这是 2004 的规定；2018 版才把 DI 改成小端。
//  3. **CS 从 0x68 到数据域末字节**，前导 FE 不计入。把前导算进去会让所有
//     带前导的帧都校验失败，而现场的表几乎都要求前导。
package cjt188

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// Preamble 是前导字节（唤醒电池供电的表）。
	Preamble = 0xFE
	// PreambleLen 是标准建议的前导字节数（2..4）。
	PreambleLen = 2
	// StartByte 是帧起始符。
	StartByte = 0x68
	// EndByte 是帧结束符。
	EndByte = 0x16
	// AddrLen 是地址域字节数。
	AddrLen = 7

	// MaxData 是读数据时数据域的字节数上限（0x64）。
	MaxData = 0x64
	// MaxWriteData 是写数据时数据域的字节数上限（0x32）。
	MaxWriteData = 0x32

	// FixedLen 是除数据域与校验/结束符之外的固定长度：
	// 68 + T + 7 字节地址 + C + L。
	FixedLen = 1 + 1 + AddrLen + 1 + 1
	// MaxFrame 是一帧字节数上限。
	MaxFrame = FixedLen + MaxData + 2
)

// 控制码。D7 表示方向（从站应答），D6 表示异常。
const (
	CtrlRead      = 0x01 // 读数据
	CtrlReadAddr  = 0x03 // 读地址（表号），单机操作
	CtrlWrite     = 0x04 // 写数据
	CtrlWriteAddr = 0x15 // 写地址（表号）
	CtrlSync      = 0x16 // 写机电同步数（置表底数）

	BitReply = 0x80 // D7：1 = 从站应答
	BitError = 0x40 // D6：1 = 通讯异常
)

// 仪表类型。
const (
	TypeBroadcast = 0xAA // 读地址时使用：T 与地址域都为 AA
)

// 特殊报文标识（读地址 810A、写地址 A018、阀门 A017）与 901F 一样，按
// CJ/T 188-2004 的大端书写：DIReadAddr.Bytes() = 81 0A、DIWriteAddr.Bytes()
// = A0 18、DIValve.Bytes() = A0 17。构造请求时统一走 DI.Bytes()，不再维护
// 独立的线序字面量——那样会多一处可能写反的真相来源。
var (
	// ErrIncomplete 表示缓冲区里的字节还不足以构成一整帧。
	ErrIncomplete = errors.New("cjt188: incomplete frame")
	// ErrBadFrame 表示出现了无法解释的帧（长度/结束符/校验和）。
	ErrBadFrame = errors.New("cjt188: bad frame")
)

// Addr 是 7 字节 BCD 地址，线上低字节在前（A0 是最低字节）。
//
// 点表里写的是 14 位十进制数字（高位在前），ParseAddr 负责翻转。
type Addr [AddrLen]byte

// BroadcastAddr 是广播地址（全 AA）。只用于读地址命令。
var BroadcastAddr = Addr{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}

// ParseAddr 解析 14 位十进制 BCD 地址（高位在前，与点表写法一致）。
//
// 线上是 A0 先传（最低字节在前），所以字符串最后两位对应 Addr[0]。
// 只接受数字；`AA` 作为通配字节保留给广播地址，普通设备地址里出现 AA
// 会让「读数据」变成一次范围不确定的广播，因此在驱动里另行拒绝。
func ParseAddr(s string) (Addr, error) {
	var out Addr
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if s == "" {
		return out, errors.New("cjt188: 地址为空")
	}
	if len(s) != 2*AddrLen {
		return out, fmt.Errorf("cjt188: 地址 %q 必须是 14 位十进制数（含厂商码）", s)
	}
	for i := 0; i < AddrLen; i++ {
		pair := s[len(s)-2*(i+1) : len(s)-2*i]
		if pair == "AA" {
			out[i] = 0xAA
			continue
		}
		hi, lo := pair[0], pair[1]
		if hi < '0' || hi > '9' || lo < '0' || lo > '9' {
			return out, fmt.Errorf("cjt188: 地址 %q 第 %d 段 %q 不是十进制数字", s, i+1, pair)
		}
		out[i] = (hi-'0')<<4 | (lo - '0')
	}
	return out, nil
}

// FormatAddr 输出 14 位大写十六进制地址串（低字节在最后）。
func FormatAddr(a Addr) string {
	var b strings.Builder
	for i := AddrLen - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%02X", a[i])
	}
	return b.String()
}

// String 返回 14 位地址串。
func (a Addr) String() string { return FormatAddr(a) }

// IsBroadcast 判断是否为全 AA 广播地址。
func (a Addr) IsBroadcast() bool { return a == BroadcastAddr }

// HasWildcard 判断地址里是否含 AA 通配字节。
func (a Addr) HasWildcard() bool {
	for _, b := range a {
		if b == 0xAA {
			return true
		}
	}
	return false
}

// Frame 是一帧。
type Frame struct {
	Type byte
	Addr Addr
	Ctrl byte
	Data []byte
}

// IsReply 表示这是从站应答帧。
func (f Frame) IsReply() bool { return f.Ctrl&BitReply != 0 }

// IsError 表示这是异常应答。
func (f Frame) IsError() bool { return f.Ctrl&BitError != 0 }

// Sum 计算校验和：从 0x68 到数据域末字节的模 256 和。
//
// 传入的必须是**实际发送字节**（含 0x68、T、地址、控制码、长度、数据），
// 不含前导、CS 与 0x16。
func Sum(b []byte) byte {
	var s byte
	for _, x := range b {
		s += x
	}
	return s
}

// Build 组装一帧（含前导）。
func Build(t byte, addr Addr, ctrl byte, data []byte) ([]byte, error) {
	if len(data) > MaxData {
		return nil, fmt.Errorf("cjt188: 数据域 %d 字节超过上限 %d", len(data), MaxData)
	}
	out := make([]byte, 0, PreambleLen+FixedLen+len(data)+2)
	for i := 0; i < PreambleLen; i++ {
		out = append(out, Preamble)
	}
	body := make([]byte, 0, FixedLen+len(data)+2)
	body = append(body, StartByte, t)
	body = append(body, addr[:]...)
	body = append(body, ctrl, byte(len(data)))
	body = append(body, data...)
	body = append(body, Sum(body), EndByte)
	return append(out, body...), nil
}

// Parse 从字节流中找出一帧。
//
// 返回值 consumed 是「应当从缓冲区开头丢弃多少字节」，成功时等于
// start + total（起始符之前的噪声 + 整帧）；ErrIncomplete 时等于起始符
// 之前的噪声字节数（起始符本身留在缓冲区里等后续字节）；其它错误时至少
// 为 start+1。这个契约让调用方只写一个循环 `buf = buf[consumed:]`：若成功
// 时只返回 total，前导噪声会留在缓冲区头部、同时帧尾等长的真实字节被砍掉。
func Parse(buf []byte) (Frame, int, error) {
	start := -1
	for i, b := range buf {
		if b == StartByte {
			start = i
			break
		}
	}
	if start < 0 {
		// 全是噪声：除了最后一个字节（可能是被截断的起始符）之外都可以丢。
		drop := len(buf)
		if drop > 0 {
			drop--
		}
		return Frame{}, drop, ErrIncomplete
	}
	if len(buf)-start < FixedLen {
		return Frame{}, start, ErrIncomplete
	}
	l := int(buf[start+10])
	if l > MaxData {
		// 长度不可能这么大：这个 0x68 是数据里的偶然字节，跳过它继续找。
		return Frame{}, start + 1, fmt.Errorf("%w: L=%d 超过上限 %d", ErrBadFrame, l, MaxData)
	}
	total := FixedLen + l + 2
	if len(buf)-start < total {
		return Frame{}, start, ErrIncomplete
	}
	raw := buf[start : start+total]
	if raw[total-1] != EndByte {
		return Frame{}, start + 1, fmt.Errorf("%w: 缺少结束符 0x16", ErrBadFrame)
	}
	if want := Sum(raw[:FixedLen+l]); raw[total-2] != want {
		return Frame{}, start + 1, fmt.Errorf("%w: 校验和 %02X，应为 %02X", ErrBadFrame, raw[total-2], want)
	}
	var addr Addr
	copy(addr[:], raw[2:9])
	return Frame{
		Type: raw[1],
		Addr: addr,
		Ctrl: raw[9],
		Data: append([]byte(nil), raw[11:11+l]...),
	}, start + total, nil
}

// FrameLen 根据已收到的字节推算整帧长度；数据不足时 ok=false。
func FrameLen(buf []byte) (int, bool) {
	start := -1
	for i, b := range buf {
		if b == StartByte {
			start = i
			break
		}
	}
	if start < 0 || len(buf)-start < FixedLen {
		return 0, false
	}
	l := int(buf[start+10])
	if l > MaxData {
		return 0, false
	}
	return start + FixedLen + l + 2, true
}

// ---------------------------------------------------------------------------
// 命令构造
// ---------------------------------------------------------------------------

// ReadRequest 构造读数据请求（0x01）。
//
// 数据域 = DI0 DI1 SER，L=03。
func ReadRequest(t byte, addr Addr, di DI, ser byte) ([]byte, error) {
	d := di.Bytes()
	return Build(t, addr, CtrlRead, []byte{d[0], d[1], ser})
}

// ReadAddrRequest 构造读地址请求（0x03）。
//
// T 与地址域都是 AA（单机操作）；报文标识固定为 81 0A。
func ReadAddrRequest(ser byte) ([]byte, error) {
	wire := DIReadAddr.Bytes()
	return Build(TypeBroadcast, BroadcastAddr, CtrlReadAddr,
		[]byte{wire[0], wire[1], ser})
}

// WriteRequest 构造写数据请求（0x04）。
//
// 数据域 = DI0 DI1 SER + 参数。
func WriteRequest(t byte, addr Addr, di DI, ser byte, payload []byte) ([]byte, error) {
	d := di.Bytes()
	data := []byte{d[0], d[1], ser}
	data = append(data, payload...)
	if len(data) > MaxWriteData {
		return nil, fmt.Errorf("cjt188: 写数据域 %d 字节超过上限 %d", len(data), MaxWriteData)
	}
	return Build(t, addr, CtrlWrite, data)
}

// WriteAddrRequest 构造写地址请求（0x15）。
//
// 数据域 = 报文标识 A0 18 + SER + 7 字节新地址，L=0x0A。
func WriteAddrRequest(t byte, addr Addr, ser byte, next Addr) ([]byte, error) {
	wire := DIWriteAddr.Bytes()
	data := []byte{wire[0], wire[1], ser}
	data = append(data, next[:]...)
	return Build(t, addr, CtrlWriteAddr, data)
}

// ValveRequest 构造阀门控制请求（0x04 + 报文标识 A017）。
//
// open=true 发 0x55 开阀，false 发 0x99 关阀。这是标准「写数据」在阀门
// 上的一种常见实现；不同厂商另有 0x2A 等自定义控制码，本驱动不猜。
func ValveRequest(t byte, addr Addr, ser byte, open bool) ([]byte, error) {
	ctr := byte(0x99)
	if open {
		ctr = 0x55
	}
	wire := DIValve.Bytes()
	data := []byte{wire[0], wire[1], ser, ctr}
	return Build(t, addr, CtrlWrite, data)
}

// ---------------------------------------------------------------------------
// 应答解析
// ---------------------------------------------------------------------------

// Reply 是读/写应答里与请求配对的部分。
type Reply struct {
	DI      DI
	SER     byte
	Payload []byte
}

// ParseReply 解析正常应答（0x81 / 0x84 / 0x95 …）。
//
// 数据域前三个字节是 DI0 DI1 SER；其余是数据。少于三个字节说明对方回的
// 不是这个协议，直接判错——把短帧当成「没有数据」会让上层以为设备正常。
func ParseReply(fr Frame) (Reply, error) {
	if fr.IsError() {
		// 返回带类型的 *Exception：驱动靠 errors.As 把它归为「表拒绝执行」
		// 的点级错误，而不是「帧坏了」的协议错。
		return Reply{}, ExceptionOf(fr)
	}
	if !fr.IsReply() {
		return Reply{}, fmt.Errorf("cjt188: 不是应答帧（控制码 %02X）", fr.Ctrl)
	}
	if len(fr.Data) < 3 {
		return Reply{}, fmt.Errorf("cjt188: 应答数据域只有 %d 字节，至少需要 3 字节（DI+SER）", len(fr.Data))
	}
	return Reply{
		DI:      DI{Hi: fr.Data[0], Lo: fr.Data[1]}, // 大端：DI0 是高字节
		SER:     fr.Data[2],
		Payload: append([]byte(nil), fr.Data[3:]...),
	}, nil
}

// ParseReadReply 解析读数据应答，并校验 DI 与序列号。
func ParseReadReply(fr Frame, want DI, ser byte) (Reply, error) {
	r, err := ParseReply(fr)
	if err != nil {
		return Reply{}, err
	}
	if r.DI != want {
		return Reply{}, fmt.Errorf("cjt188: 应答数据标识 %s 与请求 %s 不符", r.DI, want)
	}
	if r.SER != ser {
		return Reply{}, fmt.Errorf("cjt188: 应答序列号 %d 与请求 %d 不符", r.SER, ser)
	}
	return r, nil
}

// ErrorReply 是从站异常应答里能确定的内容。
//
// 2004 版异常应答的数据域是 **SER + 状态 ST（2 字节）**，不是「DI + SER」：
//
//	68H T A0..A6 CTR_2 03H SER ST0 ST1 CS 16H
//
// 标准 §8.1.3（读）与 §8.2.x（写）都写作 `CTR_2 L SER 状态ST`；现场抓包也
// 印证——汇中水表「写时间失败」的应答是 `C4 03 02 00 00`，数据域 `02 00 00`
// 即 SER=0x02、ST=0x0000。把这里当成 DI0 DI1 SER 会把 SER 读成 DI 高字节、
// 把 ST 高字节读成 SER，诊断文本整段错位。
//
// 少数厂商只回 1 字节 SER（L=01），同样接受；其 ST 视为 0。其余长度按协议
// 错误处理。
type ErrorReply struct {
	SER byte
	// ST 是 2 字节状态字（低字节先传）：第一字节定义见表 14，第二字节由
	// 厂商定义。L=01 的厂商变体没有状态字，ST 为 0。
	ST uint16
}

// Exception 是从站异常应答（控制码 D6=1）对应的错误。
//
// 单独成类型而不是一个 fmt.Errorf：驱动要靠类型把「表拒绝执行」与「帧本身
// 坏了」分成两类处置——前者是点级问题（多半是点表配了表不支持的数据标识），
// 不该重连、不该熔断；后者才需要看帧边界是否还可信。
type Exception struct {
	Ctrl byte
	SER  byte
	ST   uint16
}

// Error 输出便于对照请求的错误文本。
//
// 188 的异常应答没有错误码，因此文案只能说清「哪条命令被拒」——把 SER 与
// 状态字 ST 都带上，现场才查得到是点表里的哪一行。
func (e *Exception) Error() string {
	return fmt.Sprintf("cjt188: 从站拒绝执行（控制码 %02X，SER %d，状态 ST 0x%04X）", e.Ctrl, e.SER, e.ST)
}

// ParseErrorReply 解析异常应答（0xC1 / 0xC4 …）。
func ParseErrorReply(fr Frame) (ErrorReply, error) {
	if !fr.IsError() {
		return ErrorReply{}, fmt.Errorf("cjt188: 不是异常应答（控制码 %02X）", fr.Ctrl)
	}
	switch len(fr.Data) {
	case 3:
		// 数值低字节先传：ST0 是低字节、ST1 是高字节。
		return ErrorReply{SER: fr.Data[0], ST: uint16(fr.Data[1]) | uint16(fr.Data[2])<<8}, nil
	case 1:
		return ErrorReply{SER: fr.Data[0]}, nil
	default:
		return ErrorReply{}, fmt.Errorf("cjt188: 异常应答数据域 %d 字节，无法解释（标准为 SER+ST 共 3 字节）", len(fr.Data))
	}
}

// ExceptionOf 把异常应答转成 *Exception 错误；不是异常应答时返回 nil。
//
// 与 ParseErrorReply 的分工：后者是「能解析出什么」，本函数是「如何向上
// 报告」。驱动不该自己拼一条 fmt.Errorf——那样 errors.As 就拿不到类型，
// 分类只能靠字符串匹配。
func ExceptionOf(fr Frame) error {
	if !fr.IsError() {
		return nil
	}
	ex := &Exception{Ctrl: fr.Ctrl}
	if er, err := ParseErrorReply(fr); err == nil {
		ex.SER = er.SER
		ex.ST = er.ST
	}
	return ex
}

// ParseAddrReply 从读地址应答里取出仪表类型与地址。
//
// 地址来自**帧头**而不是数据域：请求用全 AA，从站用自己的真实地址应答，
// 这正是主站「问一台表你是谁」的全部机制。
func ParseAddrReply(fr Frame) (byte, Addr, error) {
	if fr.IsError() {
		return 0, Addr{}, ExceptionOf(fr)
	}
	if !fr.IsReply() {
		return 0, Addr{}, fmt.Errorf("cjt188: 不是应答帧（控制码 %02X）", fr.Ctrl)
	}
	if _, err := ParseReply(fr); err != nil {
		return 0, Addr{}, err
	}
	if fr.Addr == BroadcastAddr {
		return 0, Addr{}, errors.New("cjt188: 读地址应答仍是广播地址，说明对方没有填表号")
	}
	return fr.Type, fr.Addr, nil
}
