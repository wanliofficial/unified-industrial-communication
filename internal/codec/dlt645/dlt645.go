// Package dlt645 实现多功能电能表通信协议（2007 版）的纯函数编解码。
//
// 这个协议与 Modbus/S7 的差别不在「字段多少」，而在三件事：
//
//  1. **帧没有独立的长度前缀**。接收方只能先找 0x68，再从固定偏移读出
//     数据域长度。因此分帧只能靠「起始符 + 6 字节 BCD 地址 + 第二个
//     0x68」这三重校验；任何一处对不上就必须回退一个字节重新扫描，否则
//     会把一段噪声当成帧长几十字节的请求，把后面的正常应答全吃掉。
//  2. **数据域整体加 0x33**。目的是避开帧界字节（0x68/0x16）与容易误判
//     的 ASCII 控制码，代价是所有取值——DI、密码、时间、BCD 数值——都要
//     先减 0x33 才是真值。
//  3. **寻址是「数据标识」而不是寄存器偏移**。一次读只能要一个 DI（或一
//     个数据块），没有「从 offset 读 N 个寄存器」这种动作。因此驱动侧的
//     address.area 承载 8 位十六进制 DI，offset 恒为 0。
//
// 本包不建连接、不读时钟、不持有状态；串口主站、TCP 从站与自检命令共用
// 同一份组帧/解帧代码，避免两边对同一个字节的理解漂移。
//
// 帧结构（前导 FE 不计入校验和与长度）：
//
//	FE FE FE FE | 68 | A0 A1 A2 A3 A4 A5 | 68 | C | L | DATA(+0x33) | CS | 16
//	             └────────── CS 覆盖范围（模 256 求和）──────────┘
//
// 地址域 6 字节、每字节 2 位 BCD，**低字节先传**：地址 000000000042 在线
// 上是 42 00 00 00 00 00。缩位寻址时高位补 0xAA 通配，从站应答必须回实际
// 地址——主站不校验这一点就会在多表总线上把 A 表的数据记到 B 表头上。
package dlt645

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 链路层常量。
const (
	StartByte = 0x68
	EndByte   = 0x16
	Preamble  = 0xFE

	// PreambleLen 是标准要求的主站前导字节个数（唤醒接收方）。
	//
	// 它刻意做成可关闭：大量 USB 转 485 适配器与一部分电表反而在前导之后
	// 丢掉第一个字节，现场表现为「不发 FE 能通、发了就不通」。
	PreambleLen = 4

	AddrLen = 6
	// FixedLen 是「起始符(1) + 地址(6) + 起始符(1) + 控制码(1) + 长度(1)」。
	FixedLen = 1 + AddrLen + 1 + 2

	// MaxData 是数据域上限：读 200 字节。分帧时按这个上限收，写请求则按
	// MaxWriteData 拦（标准规定写 ≤50）。
	MaxData      = 200
	MaxWriteData = 50
	// MaxFrame 是一整帧的字节上限（不含前导）。
	MaxFrame = FixedLen + MaxData + 2

	// Wildcard 是缩位寻址的空缺字节。
	Wildcard = 0xAA
)

// 控制码：D7 方向、D6 异常、D5 后续帧、D4..D0 功能码。
const (
	CtrlBroadcastTime = 0x08 // 广播校时（地址全 99，从站不应答）
	CtrlRead          = 0x11 // 读数据
	CtrlReadNext      = 0x12 // 读后续数据
	CtrlReadAddr      = 0x13 // 读通信地址（仅点对点）
	CtrlWrite         = 0x14 // 写数据
	CtrlWriteAddr     = 0x15 // 写通信地址
	CtrlFreeze        = 0x16 // 冻结命令
	CtrlChangeBaud    = 0x17 // 更改通信速率
	CtrlChangePwd     = 0x18 // 修改密码
	CtrlDemandReset   = 0x19 // 最大需量清零
	CtrlMeterReset    = 0x1A // 电表清零
	CtrlEventReset    = 0x1B // 事件清零
	CtrlRelay         = 0x1C // 拉闸/合闸（备案扩展）
	CtrlTerminal      = 0x1D // 多功能端子输出控制（备案扩展）
)

// 控制码的位掩码。
const (
	BitReply  = 0x80 // D7：1 = 从站应答
	BitError  = 0x40 // D6：1 = 异常应答
	BitFollow = 0x20 // D5：1 = 有后续数据帧
	FuncMask  = 0x1F // D4..D0 功能码
)

// 错误信息字（异常应答的 ERR 字节）的位定义。
//
// 一位一因，但现场常见多个位同时置位；因此调用方应当按「地址类/权限类/
// 协议类」做归类，而不是只认第一个命中的位。
const (
	ErrOther     = 1 << 0 // 其它错误
	ErrNoData    = 1 << 1 // 无请求数据
	ErrPassword  = 1 << 2 // 密码错 / 未授权
	ErrBaud      = 1 << 3 // 通信速率不能更改
	ErrYearZone  = 1 << 4 // 年时区数超
	ErrDayPeriod = 1 << 5 // 日时段数超
	ErrRateCount = 1 << 6 // 费率数超
	// 注意：没有 D7 位定义。标准把 D7 标为「保留」，ErrSegments 因此删除——
	// 它与 ErrRateCount 同值重复，只会让「按位分类」出现两个名字解释同一位。
	BroadcastAddr = 0x999999999999
)

// 拉合闸命令的控制类型（备案扩展）。
const (
	RelayTrip  = 0x1A // 跳闸（拉闸）
	RelayAllow = 0x1B // 合闸允许
)

var (
	// ErrIncomplete 表示缓冲区里的字节还不足以构成一整帧。调用方应当继续
	// 读，而不是把它当成故障——串口上按字节到达是常态。
	ErrIncomplete = errors.New("dlt645: incomplete frame")

	errNoFrame = errors.New("dlt645: no frame start in buffer")
)

// SyncError 表示在预期位置没有看到应有的字节。它是「这不是一帧」的信号，
// 通常是噪声、上一帧的尾巴，或者两个设备同时在应答（总线冲突）。
type SyncError struct {
	Offset int
	Want   byte
	Got    byte
	Why    string
}

func (e *SyncError) Error() string {
	if e.Why != "" {
		return fmt.Sprintf("dlt645: 帧同步失败（偏移 %d）：%s", e.Offset, e.Why)
	}
	return fmt.Sprintf("dlt645: 帧同步失败（偏移 %d）：期望 0x%02X，实际 0x%02X",
		e.Offset, e.Want, e.Got)
}

// ChecksumError 表示校验和不对。按标准，校验失败的帧必须整帧丢弃且不予
// 应答；主站侧则应当继续等下一个 0x68。
type ChecksumError struct {
	Addr [AddrLen]byte
	Ctrl byte
	Got  byte
	Want byte
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("dlt645: 校验和错误（地址 %s 控制码 0x%02X）：帧内 0x%02X，计算 0x%02X",
		FormatAddr(e.Addr), e.Ctrl, e.Got, e.Want)
}

// Exception 是从站异常应答里的错误信息字。
type Exception struct {
	Code byte
}

func (e *Exception) Error() string {
	return "dlt645: 从站异常应答 0x" + fmt.Sprintf("%02X", e.Code) + "（" + e.Text() + "）"
}

// Has 判断某个错误位是否置位。
func (e *Exception) Has(bit byte) bool { return e.Code&bit != 0 }

// Text 把置位的原因拼成一句人话。
func (e *Exception) Text() string {
	if e.Code == 0 {
		return "未说明原因"
	}
	names := make([]string, 0, 4)
	add := func(bit byte, s string) {
		if e.Has(bit) {
			names = append(names, s)
		}
	}
	add(ErrOther, "其它错误")
	add(ErrNoData, "无请求数据")
	add(ErrPassword, "密码错或未授权")
	add(ErrBaud, "通信速率不能更改")
	add(ErrYearZone, "年时区数超")
	add(ErrDayPeriod, "日时段数超")
	add(ErrRateCount, "费率数超")
	if len(names) == 0 {
		return fmt.Sprintf("未定义原因（0x%02X）", e.Code)
	}
	return strings.Join(names, "、")
}

// Frame 是一帧的结构化表达，Data 已经是减过 0x33 的真值。
type Frame struct {
	Addr [AddrLen]byte
	Ctrl byte
	Data []byte
}

// IsReply 表示这是从站发出的帧。
func (f Frame) IsReply() bool { return f.Ctrl&BitReply != 0 }

// IsError 表示这是从站异常应答。
func (f Frame) IsError() bool { return f.Ctrl&BitError != 0 }

// HasFollow 表示还有后续数据帧（读数据的 0xB1 / 读后续的 0xB2）。
func (f Frame) HasFollow() bool { return f.Ctrl&BitFollow != 0 }

// Func 返回功能码（D4..D0）。
func (f Frame) Func() byte { return f.Ctrl & FuncMask }

// AddrString 是地址域的 12 位十六进制文本（0xAA 通配原样保留）。
func (f Frame) AddrString() string { return FormatAddr(f.Addr) }

// Exception 在异常应答帧上返回错误信息字；正常帧返回 nil。
func (f Frame) Exception() *Exception {
	if !f.IsError() {
		return nil
	}
	var code byte
	if len(f.Data) > 0 {
		code = f.Data[0]
	}
	return &Exception{Code: code}
}

// Build 组一帧。data 是「真值」，函数内部统一做 +0x33。
//
// 前导字节也在这里产出：组帧与发送是同一个决定，把前导拆到调用方只会
// 让「有的命令带前导、有的不带」这种不对称悄悄出现。
func Build(addr [AddrLen]byte, ctrl byte, data []byte) ([]byte, error) {
	if len(data) > MaxData {
		return nil, fmt.Errorf("dlt645: 数据域 %d 字节超过上限 %d", len(data), MaxData)
	}
	out := make([]byte, 0, PreambleLen+FixedLen+len(data)+2)
	for i := 0; i < PreambleLen; i++ {
		out = append(out, Preamble)
	}
	body := make([]byte, 0, FixedLen+len(data)+2)
	body = append(body, StartByte)
	body = append(body, addr[:]...)
	body = append(body, StartByte, ctrl, byte(len(data)))
	for _, b := range data {
		body = append(body, b+0x33)
	}
	body = append(body, Sum(body), EndByte)
	return append(out, body...), nil
}

// Sum 计算帧校验和：从第一个 0x68 到数据域最后一个字节的模 256 和。
//
// 注意传进来的必须是**已经加过 0x33 的实际发送字节**，不能是减完 0x33 的
// 真值——这是实现时最容易搞错的一处，表现为「自己发的帧自己算不对」。
func Sum(b []byte) byte {
	var s byte
	for _, x := range b {
		s += x
	}
	return s
}

// Parse 从字节流中找出一帧。
//
// 返回值 consumed 是「应当从缓冲区开头丢弃多少字节」：
//   - 成功时等于整帧长度；
//   - ErrIncomplete 时等于帧起始符之前跳过的噪声字节数（起始符本身要留
//     在缓冲区里等后续字节）；
//   - 其它错误时至少为 1，表示同步失败的位置已经被跳过，调用方可以继续
//     扫描而不必清空整个缓冲区。
//
// 这个「总是告诉调用方丢多少」的契约，是为了让调用方只写一个循环：
//
//	buf = buf[consumed:]
//	if err == ErrIncomplete { buf = append(buf, readMore()...) }
func Parse(buf []byte) (Frame, int, error) {
	start := -1
	for i, b := range buf {
		if b == StartByte {
			start = i
			break
		}
	}
	if start < 0 {
		return Frame{}, len(buf), ErrIncomplete
	}
	if start+FixedLen > len(buf) {
		return Frame{}, start, ErrIncomplete
	}
	head := buf[start:]
	// 第二个起始符是「这真的是帧头」的最强证据：噪声里出现两个相距 7 字节
	// 的 0x68、且中间 6 个字节都是合法 BCD，概率可以忽略。
	if head[7] != StartByte {
		return Frame{}, start + 1, &SyncError{Offset: start + 7, Want: StartByte, Got: head[7],
			Why: "地址域之后不是帧起始符"}
	}
	for i := 1; i <= AddrLen; i++ {
		if !validAddrByte(head[i]) {
			return Frame{}, start + 1, &SyncError{Offset: start + i, Got: head[i],
				Why: fmt.Sprintf("地址域第 %d 字节 0x%02X 既不是 BCD 也不是 0xAA 通配符", i, head[i])}
		}
	}
	length := int(head[9])
	if length > MaxData {
		return Frame{}, start + 1, &SyncError{Offset: start + 9, Got: head[9],
			Why: fmt.Sprintf("数据域长度 %d 超过上限 %d", length, MaxData)}
	}
	total := FixedLen + length + 2
	if start+total > len(buf) {
		return Frame{}, start, ErrIncomplete
	}
	raw := buf[start : start+total]
	want := Sum(raw[:total-2])
	if got := raw[total-2]; got != want {
		var addr [AddrLen]byte
		copy(addr[:], raw[1:7])
		return Frame{}, start + 1, &ChecksumError{Addr: addr, Ctrl: raw[8], Got: got, Want: want}
	}
	if raw[total-1] != EndByte {
		return Frame{}, start + 1, &SyncError{Offset: start + total - 1, Want: EndByte,
			Got: raw[total-1], Why: "结束符不对"}
	}
	fr := Frame{Ctrl: raw[8], Data: make([]byte, length)}
	copy(fr.Addr[:], raw[1:7])
	for i := 0; i < length; i++ {
		fr.Data[i] = raw[FixedLen+i] - 0x33
	}
	return fr, start + total, nil
}

// validAddrByte 判断一个地址字节是否合法：两位 BCD，或 0xAA 通配符。
func validAddrByte(b byte) bool {
	if b == Wildcard {
		return true
	}
	return b>>4 <= 9 && b&0x0F <= 9
}

// ---------------------------------------------------------------------------
// 地址
// ---------------------------------------------------------------------------

// ParseAddr 解析地址串。
//
// 接受两种写法：
//
//	12 位完整地址   000000000042 → A0=0x42，其余 0x00
//	2/4/..10 位缩位  0042         → A0=0x42, A1=0x00, A2..A5=0xAA
//
// 12 位里的 "AA" 视为通配符原样保留（用于广播/通配读），其余必须是十进制
// 数字：地址是 BCD，写成十六进制字母只会让人误以为可以填 0x3C。
func ParseAddr(s string) ([AddrLen]byte, error) {
	var out [AddrLen]byte
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if s == "" {
		return out, errors.New("dlt645: 地址为空")
	}
	if len(s)%2 != 0 || len(s) > 2*AddrLen {
		return out, fmt.Errorf("dlt645: 地址 %q 必须是偶数位、最多 12 位", s)
	}
	pairs := len(s) / 2
	if pairs < AddrLen {
		// 缩位寻址：高位补 AA。
		for i := pairs; i < AddrLen; i++ {
			out[i] = Wildcard
		}
	}
	for i := 0; i < pairs; i++ {
		pair := s[len(s)-2*(i+1) : len(s)-2*i]
		if pair == "AA" {
			out[i] = Wildcard
			continue
		}
		hi, lo := pair[0], pair[1]
		if hi < '0' || hi > '9' || lo < '0' || lo > '9' {
			return out, fmt.Errorf("dlt645: 地址 %q 第 %d 段 %q 不是十进制数字", s, i+1, pair)
		}
		out[i] = (hi-'0')<<4 | (lo - '0')
	}
	return out, nil
}

// FormatAddr 输出 12 位大写十六进制地址串（低字节在最后）。
func FormatAddr(a [AddrLen]byte) string {
	var b strings.Builder
	for i := AddrLen - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%02X", a[i])
	}
	return b.String()
}

// WildcardAddr 是缩位寻址/读地址用的全 AA 地址。
func WildcardAddr() [AddrLen]byte {
	return [AddrLen]byte{Wildcard, Wildcard, Wildcard, Wildcard, Wildcard, Wildcard}
}

// Broadcast 是全 99 的广播地址。
func Broadcast() [AddrLen]byte {
	return [AddrLen]byte{0x99, 0x99, 0x99, 0x99, 0x99, 0x99}
}

// MatchAddr 判断应答帧的地址是否可能是发往 want 的应答。
//
// want 可以带 0xAA 通配（缩位寻址），此时只比较真实的低位。从站必须回
// 实际地址，主站必须校验——否则多表总线上收到别的表的应答时，值会张冠
// 李戴，而且错得很安静。
func MatchAddr(got, want [AddrLen]byte) bool {
	for i := 0; i < AddrLen; i++ {
		if want[i] == Wildcard {
			continue
		}
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 报文组装
// ---------------------------------------------------------------------------

// ReadRequest 组「读数据」请求：数据域只有 4 字节 DI（线上 DI0 在前）。
func ReadRequest(addr [AddrLen]byte, di DI) ([]byte, error) {
	d := di.Bytes()
	return Build(addr, CtrlRead, d[:])
}

// ReadNextRequest 组「读后续数据」请求：DI + 帧序号（1..255）。
func ReadNextRequest(addr [AddrLen]byte, di DI, seq byte) ([]byte, error) {
	if seq == 0 {
		return nil, errors.New("dlt645: 帧序号从 1 开始")
	}
	d := di.Bytes()
	return Build(addr, CtrlReadNext, []byte{d[0], d[1], d[2], d[3], seq})
}

// ReadAddrRequest 组「读通信地址」请求：地址域全 AA，无数据域。
func ReadAddrRequest() ([]byte, error) {
	return Build(WildcardAddr(), CtrlReadAddr, nil)
}

// WriteRequest 组「写数据」请求。
//
// 数据域 = DI(4) + 权限(1) + 密码(3) + 操作者代码(4) + 参数值(m)，
// 因此 L = 12 + m，标准限制写 ≤ 50 字节。
//
// perm/pwd/op 传的是**真值字节**（BCD 已压好），本函数统一做 +0x33。
func WriteRequest(addr [AddrLen]byte, di DI, perm byte, pwd [3]byte, op [4]byte, payload []byte) ([]byte, error) {
	if len(payload)+12 > MaxWriteData {
		return nil, fmt.Errorf("dlt645: 写数据域 %d 字节超过上限 %d", len(payload)+12, MaxWriteData)
	}
	d := di.Bytes()
	data := make([]byte, 0, 12+len(payload))
	data = append(data, d[:]...)
	data = append(data, perm, pwd[0], pwd[1], pwd[2])
	data = append(data, op[:]...)
	data = append(data, payload...)
	return Build(addr, CtrlWrite, data)
}

// RelayRequest 组「拉闸/合闸」请求（备案扩展，控制码 0x1C）。
//
// 数据域 = DI 无 + 权限/密码(4) + 操作者代码(4) + 控制类型(1) + 保留(1) +
// 命令有效截止时间(6)。超过截止时间的命令电表必须拒绝——这是防止主站
// 在链路中断后重放一条「昨天的跳闸」的唯一手段，所以不能省略。
func RelayRequest(addr [AddrLen]byte, perm byte, pwd [3]byte, op [4]byte, action byte, deadline time.Time) ([]byte, error) {
	if action != RelayTrip && action != RelayAllow {
		return nil, fmt.Errorf("dlt645: 未知拉合闸控制类型 0x%02X", action)
	}
	data := make([]byte, 0, 12)
	data = append(data, perm, pwd[0], pwd[1], pwd[2])
	data = append(data, op[:]...)
	data = append(data, action, 0x00)
	tb := TimeBytes(deadline)
	data = append(data, tb[:]...)
	return Build(addr, CtrlRelay, data)
}

// FreezeRequest 组「冻结」请求：数据域是 4 字节冻结类型字。
//
// 类型字是压缩 BCD 的「掩码」：99DDhhmm 表示按月冻结、9999hhmm 按日、
// 999999mm 按小时、99999999 瞬时冻结。用 9 而不是 0 表示「这一位不参与
// 比较」，正是这些 9 让它能表达四种周期。
func FreezeRequest(addr [AddrLen]byte, kind [4]byte) ([]byte, error) {
	return Build(addr, CtrlFreeze, kind[:])
}

// BroadcastTimeRequest 组广播校时（控制码 0x08，地址全 99）。
//
// 广播命令从站不应答。数据域是 6 字节 BCD：秒 分 时 日 月 年——秒在最前，
// 与大多数人的直觉（年月日在前）相反。
func BroadcastTimeRequest(t time.Time) ([]byte, error) {
	b := TimeBytes(t)
	return Build(Broadcast(), CtrlBroadcastTime, b[:])
}

// ---------------------------------------------------------------------------
// 时间
// ---------------------------------------------------------------------------

// TimeBytes 把时间编码为 6 字节 BCD：ss mm hh DD MM YY。
func TimeBytes(t time.Time) [6]byte {
	var b [6]byte
	b[0] = bcdByte(t.Second())
	b[1] = bcdByte(t.Minute())
	b[2] = bcdByte(t.Hour())
	b[3] = bcdByte(t.Day())
	b[4] = bcdByte(int(t.Month()))
	b[5] = bcdByte(t.Year() % 100)
	return b
}

// ValidDateParts 校验时间各字段的取值范围。
//
// 只有范围校验、没有日历校验：2 月 30 日这类「字段都合法但日期不存在」要
// 交给调用方按语义决定（写路径必须拒绝，读路径可以选择如实上报），codec
// 层不做日历运算，避免与 time.Time 的时区行为耦合。
func ValidDateParts(mon, day, hour, min, sec int) error {
	switch {
	case mon < 1 || mon > 12:
		return fmt.Errorf("月份 %d 必须在 1..12", mon)
	case day < 1 || day > 31:
		return fmt.Errorf("日 %d 必须在 1..31", day)
	case hour < 0 || hour > 23:
		return fmt.Errorf("小时 %d 必须在 0..23", hour)
	case min < 0 || min > 59:
		return fmt.Errorf("分钟 %d 必须在 0..59", min)
	case sec < 0 || sec > 59:
		return fmt.Errorf("秒 %d 必须在 0..59", sec)
	}
	return nil
}

// ParseTime 解码 6 字节 BCD 时间（ss mm hh DD MM YY）。年份按 2000..2099
// 解释——协议只有两位年，这个窗口是行业默认；跨世纪问题不在本驱动的责任
// 范围内。
//
// 除了 BCD 半字节合法性，还会校验各字段取值范围。为什么必须做：time.Date
// 会把「秒=72」这类垃圾静默归一化（变成下一分钟 12 秒），月=19 会变成
// 次年 7 月，2 月 30 日会变成 3 月 2 日，且都不报错。电表时钟是最不能「猜」
// 的数据项之一——读到一个假的 07:06 比读不到更危险，因为它会被当成真实事件
// 时间参与结算与统计。
func ParseTime(b []byte) (time.Time, error) {
	// 必须精确 6 字节：多给的字节属于下一个数据项。只检查下界会让
	// 「DI + 时间」这种整段误传的调用悄悄成功（例如广播校时帧多带一字节）。
	if len(b) != 6 {
		return time.Time{}, fmt.Errorf("dlt645: 时间需要 6 字节，实际 %d", len(b))
	}
	v := make([]int, 6)
	for i := 0; i < 6; i++ {
		hi, lo := b[i]>>4, b[i]&0x0F
		if hi > 9 || lo > 9 {
			return time.Time{}, fmt.Errorf("dlt645: 时间第 %d 字节 0x%02X 不是 BCD", i+1, b[i])
		}
		v[i] = int(hi)*10 + int(lo)
	}
	if err := ValidDateParts(v[4], v[3], v[2], v[1], v[0]); err != nil {
		return time.Time{}, fmt.Errorf("dlt645: % X: %w", b, err)
	}
	if yy := v[5]; yy > 99 {
		return time.Time{}, fmt.Errorf("dlt645: 年份 20%02d 超出 2000..2099", yy)
	}
	return time.Date(2000+v[5], time.Month(v[4]), v[3], v[2], v[1], v[0], 0, time.Local), nil
}

// bcdByte 把 0..99 编码为 BCD 字节。
func bcdByte(n int) byte { return byte(n/10)<<4 | byte(n%10) }
