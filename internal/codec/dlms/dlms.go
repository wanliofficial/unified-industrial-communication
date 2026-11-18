// Package dlms 实现 IEC 62056 DLMS/COSEM（3 层面向连接的 HDLC 承载 + LN
// 逻辑名引用 + A-XDR 数据编码）的纯函数编解码。
//
// 这个协议与仓库里其它表计协议（DL/T 645、CJ/T 188、M-Bus）最不一样的地方
// 在于**它是真正分层的**：三层里每一层都有自己的封装、自己的地址、自己的
// 长度与自己的错误语义。
//
//	HDLC 帧    7E | A0 len | dest | src | ctl | HCS | LLC | APDU | FCS | 7E
//	LLC 头     E6 E6 00（请求）/ E6 E7 00（应答）
//	APDU       AARQ/AARE（建链）→ Get-Request/Get-Response（读）
//	A-XDR 数据 0x11 = 1 字节无符号 … 0x06 = 双长无符号 … 0x09 = OCTET STRING
//
// 四层的字节都容易被"看起来对"地写错，而错一处就整条链路不通，且症状往往
// 只是「超时」——所以本包把每一层都摊开成独立文件：
//
//	hdlc.go    帧格式类型 3、地址域（1/2/4 字节）、HCS/FCS、控制域
//	axdr.go    A-XDR 类型标签与值编码
//	apdu.go    AARQ/AARE、Get-Request/Get-Response、Data-Access-Result
//	obis.go    OBIS 码（6 字节逻辑名）的解析与格式化
//	objects.go 常用 COSEM 类/属性/单位/常见 OBIS 语义表
//	io.go      从字节流切帧的 Reader
//
// # 三条最容易写错的规则
//
//  1. **长度字段覆盖什么**。HDLC 的 length 是「格式字段之后、到 FCS 为止」
//     的字节数，即 dest+src+ctl+HCS+info+FCS，**不含两端的 7E**。有 info
//     时 FCS 在长度里；没有 info 时 HCS 就是唯一的校验序列，长度 = 7。
//     把长度写成"含标志位"会让每一帧都差 2，接收方永远等不到结束标志。
//  2. **地址域是 7 位/字节 + 扩展位**。最低位为 1 表示地址域结束，其余 7 位
//     是地址值。所以 1 字节地址 0x03 表示「逻辑设备 1」，2 字节地址
//     `21 A8` 表示「物理设备 1、逻辑设备 16」（高字节先传）。把地址当普通
//     字节读会把 0x03 读成 3 而不是 1。
//  3. **HCS 与 FCS 的范围不同**。HCS 只覆盖 `format+dest+src+ctl`，FCS 覆盖
//     「开头到 info 结束」。两者都是 CRC-16/X-25（初值 0xFFFF、反射多项式
//     0x8408、结果取反、小端发送），但**范围的起止点不同**，写成同一个范围
//     会让 HCS 永远对不上——而 HCS 错意味着「长度字段不可信」，接收方必须
//     丢弃后续所有字节，症状是「第一帧之后全乱」。
//
// # 定位
//
// 只做 **LN（logical name）引用 + 无安全（Lowest Level Security）+ HDLC
// 承载** 这一条最小可用路径：点表按 OBIS 码 + 类 + 属性寻址，一条
// Get-Request 读一个属性，Get-Response 的数据按 A-XDR 解码。
//
// 不做（且在文档里说明理由）：
//   - **SN（short name）引用**：SN 的 short name 是厂商在 object_list 里
//     分配的，同一个 OBIS 在不同表上 short name 不同，点表无法离线写出来；
//     必须先读 object_list 建立映射，属于另一套模型。
//   - **HLS/GMAC 认证与加密**（security suite 0/1/2）：需要密钥协商、帧计数器、
//     AES-GCM，且各厂商的密钥分发方式完全不同。
//   - **TCP/UDP 承载（COSEM wrapper）**：wrapper 是另一套封装（版本号 +
//     Wrapper 头），与 HDLC 分帧无关，属于另一个 profile。
//   - **Set/Action/EventNotification**：写方向与订阅各自是另一套状态机。
package dlms

import (
	"errors"
	"fmt"
)

// HDLC 固定字节。
const (
	// Flag 是 HDLC 帧的起始与结束标志（ISO/IEC 13239 §4.2.2）。
	Flag byte = 0x7E

	// FormatType3 是帧格式字段的高 4 位（格式类型 3，IEC 62056-46 §6.3.1）。
	//
	// 格式字段是一个 16 位值：高 4 位格式类型 = 0xA，低 12 位是长度。
	// 因此本字节的高 4 位恒为 0xA。
	FormatType3 byte = 0xA0

	// FormatType3Mask 取格式字段的高 4 位，用于拒绝其它格式类型。
	FormatType3Mask byte = 0xF0

	// MaxFrameLength 是 11 位帧长子字段的上限（§6.3.3 NOTE）。
	MaxFrameLength = 0x7FF
)

// LLC 子层头（IEC 62056-46 §5.3.2）。COSEM 把 APDU 包在 8802-2 LLC 里。
const (
	// LLCCommand 是客户端→服务端的 LLC 头：目的 LSAP 0xE6、源 LSAP 0xE6、
	// 质量 0x00。
	LLCCommand = "\xe6\xe6\x00"
	// LLCResponse 是服务端→客户端的 LLC 头：源 LSAP 改为 0xE7。
	//
	// 这个字节是"这一帧是应答"的**唯一**信号——HDLC 控制域只表达
	// I/RR/UA 这类链路语义，不表达"这是对哪条 APDU 的应答"。少了它，
	// 解析器会把服务端的应答当成请求再处理一遍。
	LLCResponse = "\xe6\xe7\x00"
)

// 控制域（§6.4.3 Table 6）。这里是单向 NRM（normal response mode）轮询下
// 用得到的几个。
const (
	// CtrlSNRM 是置为正常响应模式（set normal response mode），请求侧 P=1。
	CtrlSNRM byte = 0x93
	// CtrlUA 是未编号确认（unnumbered acknowledgement），SNRM/DISC 的应答。
	//
	// UA 必须取 **F=1** 的形态 0x73：UA 是对 P=1 的 SNRM/DISC 的应答，
	// HDLC 规定应答把探询位翻成 F=1。0x63 是 F=0 变体，真表回的 0x73
	// 会落到 S 帧分支被误判成 RR——症状是「链路永远建不起来」。
	CtrlUA byte = 0x73
	// CtrlDM 是断开模式（disconnected mode）：服务端拒绝建链，F=1。
	CtrlDM byte = 0x1F
	// CtrlDISC 是断链（请求侧 P=1）。
	CtrlDISC byte = 0x53
	// CtrlFRMR 是帧拒绝（F=1）。
	CtrlFRMR byte = 0x97
	// CtrlUI 是未编号信息帧，用于事件上报（本包只解析，不主动发）。
	CtrlUI byte = 0x03

	// CtrlRR 的基值（N(R) 在低 5 位、bit4 是 P/F）。
	CtrlRRBase byte = 0x01
	// CtrlRNR 的基值（接收未就绪）。
	CtrlRNRBase byte = 0x05
	// CtrlIBase 是 I 帧的控制域基值（bit0 = 0 表示 I 帧）。
	CtrlIBase byte = 0x00
)

// 保留地址（§6.4.2，去掉扩展位后的 7 位值）。
const (
	// AddrNoStation 是 NO_STATION：客户端与管理逻辑设备都用它。
	AddrNoStation = 0x00
	// AddrClientManagement 是客户端管理进程。
	AddrClientManagement = 0x01
	// AddrPublicClient 是公共客户端，权限最低、永远可用。
	AddrPublicClient = 0x10
	// AddrManagementLD 是管理逻辑设备（每个物理设备都必须有）。
	AddrManagementLD = 0x01
	// AddrAllStation 是 1 字节地址域下的广播。
	AddrAllStation = 0x7F
	// AddrAllStation2 是 2/4 字节地址域下的广播。
	AddrAllStation2 = 0x3FFF
	// AddrCalling 是 CALLING 物理设备（客户端主动呼叫时用）。
	AddrCalling = 0x7E
)

// 错误分类。codec 层只区分「结构不对」与「数据不够」，具体归类交给驱动。
var (
	// ErrFrame 表示帧结构错误（格式类型、长度、地址域、控制域）。
	ErrFrame = errors.New("dlms: 帧结构错误")
	// ErrChecksum 表示 HCS 或 FCS 校验失败。
	ErrChecksum = errors.New("dlms: 校验失败")
	// ErrIncomplete 表示字节不够，需要继续读。它不是错误，是"还没到"。
	ErrIncomplete = errors.New("dlms: 数据不完整")
	// ErrAPDU 表示 APDU 层结构错误。
	ErrAPDU = errors.New("dlms: APDU 结构错误")
	// ErrAXDR 表示 A-XDR 数据编码错误。
	ErrAXDR = errors.New("dlms: A-XDR 数据编码错误")
)

// Address 是一个 HDLC 地址域。
//
// 地址域最多 4 字节，布局由 Size 决定（§6.4.2.2）：
//
//	Size=1  只有 Upper（逻辑设备地址）
//	Size=2  Upper（逻辑设备）+ Lower（物理设备），各 7 位
//	Size=4  Upper + Lower，各 14 位
//
// 客户端地址恒为 1 字节。
type Address struct {
	Upper int
	Lower int
	Size  int
}

// ClientAddress 返回 1 字节客户端地址。
func ClientAddress(a int) Address { return Address{Upper: a, Size: 1} }

// ServerAddress 返回服务端地址（逻辑设备 + 物理设备）。
func ServerAddress(upper, lower, size int) Address {
	return Address{Upper: upper, Lower: lower, Size: size}
}

// String 输出地址的十六进制写法，带 lower 时用 `upper/lower`。
func (a Address) String() string {
	if a.Size <= 1 {
		return fmt.Sprintf("%02X", a.Upper)
	}
	return fmt.Sprintf("%02X/%02X", a.Upper, a.Lower)
}

// Check 校验地址是否落在它的宽度内。
func (a Address) Check() error {
	switch a.Size {
	case 1:
		if a.Upper < 0 || a.Upper > 0x7F {
			return fmt.Errorf("%w: 1 字节地址 Upper=%d 超出 0..127", ErrFrame, a.Upper)
		}
		if a.Lower != 0 {
			return fmt.Errorf("%w: 1 字节地址不应有 Lower=%d", ErrFrame, a.Lower)
		}
	case 2:
		if a.Upper < 0 || a.Upper > 0x7F {
			return fmt.Errorf("%w: 2 字节地址 Upper=%d 超出 0..127", ErrFrame, a.Upper)
		}
		if a.Lower < 0 || a.Lower > 0x7F {
			return fmt.Errorf("%w: 2 字节地址 Lower=%d 超出 0..127", ErrFrame, a.Lower)
		}
	case 4:
		if a.Upper < 0 || a.Upper > 0x3FFF {
			return fmt.Errorf("%w: 4 字节地址 Upper=%d 超出 0..16383", ErrFrame, a.Upper)
		}
		if a.Lower < 0 || a.Lower > 0x3FFF {
			return fmt.Errorf("%w: 4 字节地址 Lower=%d 超出 0..16383", ErrFrame, a.Lower)
		}
	default:
		return fmt.Errorf("%w: 地址域只能是 1/2/4 字节，得到 %d", ErrFrame, a.Size)
	}
	return nil
}

// Encode 输出地址域字节。
//
// 每字节承载 7 位，最高位先传；除了最后一字节，其余字节的最低位为 0（继续），
// 最后一字节最低位为 1（结束）。这就是"地址 0x03 表示 1 号逻辑设备"的原因：
// 0x03 = 0b00000011，低 7 位是 1，bit0=1 表示结束。
func (a Address) Encode() ([]byte, error) {
	if err := a.Check(); err != nil {
		return nil, err
	}
	switch a.Size {
	case 1:
		return []byte{byte(a.Upper<<1) | 0x01}, nil
	case 2:
		return []byte{byte(a.Upper << 1), byte(a.Lower<<1) | 0x01}, nil
	default: // 4
		// 14 位拆成两个 7 位组：高组占第一个字节，低组占第二个字节。
		// 每字节承载 7 位地址值并左移一位腾出扩展位，只有整段最后一字节置 1。
		// 标准例子（§6.4.2.3）：Upper=0x1234、Lower=0x3FFF → 48 68 FE FF。
		//
		// 关键：一个 14 位值是「高 7 位 + 低 7 位」两个组，各自再左移一位
		// 放进字节。写成 `Upper>>6 & 0x7F` 会把高位组塞进低 6 位，既丢位又
		// 让 0x3FFF 编成 0x3FFE——两个不同的逻辑设备会收到同一帧。
		upHi := byte(a.Upper>>7&0x7F) << 1
		upLo := byte(a.Upper&0x7F) << 1
		loHi := byte(a.Lower>>7&0x7F) << 1
		loLo := byte(a.Lower&0x7F)<<1 | 0x01
		return []byte{upHi, upLo, loHi, loLo}, nil
	}
}

// DecodeAddress 从 b 里读一个地址域，返回地址与消耗的字节数。
//
// 只接受 1/2/4 字节（§6.4.2.2）。3 字节或更长的地址域直接拒绝：多字节地址
// 的每个字节都可能是"继续"，长度本身由最后的结束位决定，接受任意长度会让
// 一个被截断的帧被当成合法地址读下去。
func DecodeAddress(b []byte) (Address, int, error) {
	if len(b) == 0 {
		return Address{}, 0, ErrIncomplete
	}
	// 先数出地址域有几个字节：第一个 bit0=1 的字节就是末尾。
	n := 0
	for n < len(b) {
		n++
		if b[n-1]&0x01 != 0 {
			break
		}
		if n > 4 {
			return Address{}, 0, fmt.Errorf("%w: 地址域超过 4 字节", ErrFrame)
		}
	}
	if len(b) < n {
		return Address{}, 0, ErrIncomplete
	}
	if b[n-1]&0x01 == 0 {
		return Address{}, 0, fmt.Errorf("%w: 地址域没有结束位（可能被截断）", ErrFrame)
	}
	switch n {
	case 1:
		return Address{Upper: int(b[0] >> 1), Size: 1}, 1, nil
	case 2:
		return Address{Upper: int(b[0] >> 1), Lower: int(b[1] >> 1), Size: 2}, 2, nil
	case 4:
		// 与 Encode 对称：每字节低 7 位（>>1 后再取 7 位），高组在高字节。
		up := int(b[0]>>1&0x7F)<<7 | int(b[1]>>1&0x7F)
		lo := int(b[2]>>1&0x7F)<<7 | int(b[3]>>1&0x7F)
		return Address{Upper: up, Lower: lo, Size: 4}, 4, nil
	}
	return Address{}, 0, fmt.Errorf("%w: 地址域 %d 字节不是 1/2/4", ErrFrame, n)
}

// Broadcast 表示这个地址是否命中广播（上/下部件任一为全站地址）。
func (a Address) Broadcast() bool {
	all := AddrAllStation
	if a.Size == 4 {
		all = AddrAllStation2
	}
	return a.Upper == all || a.Lower == all
}

// FrameKind 是 HDLC 帧类型（UNC 指令集 + UI）。
type FrameKind int

const (
	// KindUnknown 表示控制域无法识别。
	KindUnknown FrameKind = iota
	// KindI 是信息帧（携带 APDU）。
	KindI
	// KindRR 是接收就绪。
	KindRR
	// KindRNR 是接收未就绪。
	KindRNR
	// KindSNRM 是置为正常响应模式。
	KindSNRM
	// KindDISC 是断链。
	KindDISC
	// KindUA 是未编号确认。
	KindUA
	// KindDM 是断开模式。
	KindDM
	// KindFRMR 是帧拒绝。
	KindFRMR
	// KindUI 是未编号信息帧（事件上报）。
	KindUI
)

// String 输出短名，便于日志与 CLI。
func (k FrameKind) String() string {
	switch k {
	case KindI:
		return "I"
	case KindRR:
		return "RR"
	case KindRNR:
		return "RNR"
	case KindSNRM:
		return "SNRM"
	case KindDISC:
		return "DISC"
	case KindUA:
		return "UA"
	case KindDM:
		return "DM"
	case KindFRMR:
		return "FRMR"
	case KindUI:
		return "UI"
	}
	return "unknown"
}

// Control 是解码后的控制域。
type Control struct {
	Kind FrameKind
	NS   int // I 帧的发送序号（modulo 8）
	NR   int // I/RR/RNR 的接收序号
	PF   bool
}

// ParseControl 解码 1 字节控制域（modulo 8）。
//
// 判定顺序很关键：I 帧的 bit0=0，其余都是 bit0=1 的 U/S 帧。先判 bit0 再
// 判具体值，否则 0x00（I 帧 NS=0）会被误认为某个未编号帧。
//
// U 帧（SNRM/UA/DISC/DM/FRMR/UI）的控制域是**固定值**，bit4 承载 P/F——
// SNRM 请求取 P=1（0x93），UA/DM/FRMR 应答取 F=1（0x73/0x1F/0x97）。
// 因此 P/F 按位取出，U 帧按含 P/F 的完整字节精确匹配。
// S 帧（RR/RNR）的 bit0-1 是 01/11 两态，用 `c&0x0F` 而不是 `c&0x03` 判定，
// 否则 0x05（RNR + NR=0）会被 0x03 掩码读成 RR。
func ParseControl(c byte) Control {
	if c&0x01 == 0 {
		return Control{Kind: KindI, NS: int(c>>1) & 0x07, NR: int(c>>5) & 0x07, PF: c&0x10 != 0}
	}
	switch c {
	case CtrlSNRM:
		return Control{Kind: KindSNRM, PF: c&0x10 != 0}
	case CtrlUA:
		return Control{Kind: KindUA, PF: c&0x10 != 0}
	case CtrlDM:
		return Control{Kind: KindDM, PF: c&0x10 != 0}
	case CtrlDISC:
		return Control{Kind: KindDISC, PF: c&0x10 != 0}
	case CtrlFRMR:
		return Control{Kind: KindFRMR, PF: c&0x10 != 0}
	case CtrlUI:
		return Control{Kind: KindUI, PF: c&0x10 != 0}
	}
	// S 帧：bit0-1 = 01(RR) / 11(RNR)，bit4 = P/F，bit5-7 = N(R)。
	// 注意 RNR 的基值是 0x05（bit0-1 = 01、bit2 = 1），必须先用 bit2 区分：
	// 只按 `c&0x03` 判定会把 0x05 读成 RR。
	if c&0x04 != 0 {
		return Control{Kind: KindRNR, NR: int(c>>5) & 0x07, PF: c&0x10 != 0}
	}
	return Control{Kind: KindRR, NR: int(c>>5) & 0x07, PF: c&0x10 != 0}
}

// EncodeControl 把控制域编回 1 字节。
func EncodeControl(c Control) byte {
	switch c.Kind {
	case KindI:
		b := byte(c.NS<<1) | byte(c.NR<<5)
		if c.PF {
			b |= 0x10
		}
		return b
	case KindRR:
		b := CtrlRRBase | byte(c.NR<<5)
		if c.PF {
			b |= 0x10
		}
		return b
	case KindRNR:
		b := CtrlRNRBase | byte(c.NR<<5)
		if c.PF {
			b |= 0x10
		}
		return b
	case KindSNRM:
		return CtrlSNRM
	case KindUA:
		return CtrlUA
	case KindDM:
		return CtrlDM
	case KindDISC:
		return CtrlDISC
	case KindFRMR:
		return CtrlFRMR
	case KindUI:
		return CtrlUI
	}
	return CtrlDM
}

// Frame 是一个 HDLC 帧（格式类型 3）。
type Frame struct {
	// Segmented 是分段位（格式字段的 bit11）：还有后续帧。
	Segmented bool
	// Length 是帧长字段的原始值（低 11 位）。
	Length  int
	Dest    Address
	Src     Address
	Control Control
	// HCS 是头校验序列的原始两字节（小端存放时不还原，只留作诊断）。
	HCS []byte
	// Info 是信息域（LLC + APDU），不含 FCS。为 nil 表示无信息域。
	Info []byte
	// FCS 是帧校验序列的原始两字节。
	FCS []byte
}

// Kind 返回帧类型。
func (f Frame) Kind() FrameKind { return f.Control.Kind }

// APDU 剥掉 LLC 头后返回 APDU。没有 LLC 头时返回整个 Info。
//
// 之所以允许"没有 LLC 头"：部分表在纯点对点串口上省略 LLC 子层，直接用
// APDU 填信息域。本包解码时容忍，编码时**总是**带 LLC（这是标准形态）。
func (f Frame) APDU() ([]byte, bool) {
	if len(f.Info) >= 3 {
		switch string(f.Info[:3]) {
		case LLCCommand:
			return f.Info[3:], true
		case LLCResponse:
			return f.Info[3:], true
		}
	}
	return f.Info, false
}

// IsResponse 判断信息域的 LLC 头是否表明这是应答。
//
// 这个字节是「这一帧是应答」的唯一信号：HDLC 控制域只表达 I/RR/UA 这类
// 链路语义，不表达「这是对哪条 APDU 的应答」。驱动用它挡掉服务端主动
// 上报的 I 帧（EventNotification），否则会把一条通知当成 Get-Response 解。
func (f Frame) IsResponse() bool {
	return len(f.Info) >= 3 && string(f.Info[:3]) == LLCResponse
}

// ---------------------------------------------------------------------------
// 校验序列
//
// HCS 与 FCS 是同一个算法（CRC-16/CCITT 的 X.25 变体），差别只在覆盖范围：
//
//	HCS  覆盖 format + dest + src + control（不含 HCS 自己）
//	FCS  覆盖 format + dest + src + control + HCS + info（不含 FCS 自己）
//
// 算法四要素：初值 0xFFFF、反射多项式 0x8408（等价于正写 0x1021）、
// 结果按位取反、以小端（低字节在前）发送。
// ---------------------------------------------------------------------------

// crc 是 X.25 CRC 的核心循环。
func crc(data []byte, sum uint16) uint16 {
	for _, b := range data {
		sum ^= uint16(b)
		for i := 0; i < 8; i++ {
			if sum&1 == 1 {
				sum = (sum >> 1) ^ 0x8408
			} else {
				sum >>= 1
			}
		}
	}
	return sum
}

// FCS 按标准算法计算校验序列。传入的字节是「校验序列自身之前的全部字节」。
//
// 返回值要先取反再小端发送，因此调用方拿到的不是可直接 append 的字节；
// 需要字节时用 appendFCS，需要比对时用 GoodFCS。
//
// 标准 Annex A.1 的金向量：FCS(03 3F) = EC5B，以小端发送为 `5B EC`；
// 好的帧在末尾补上 `5B EC` 后，对整段重算应留下 0xF0B8。
func FCS(data []byte) uint16 { return ^crc(data, 0xFFFF) }

// GoodFCS 校验一段「已含校验序列」的字节是否自洽。
//
// 这是分帧时判断「这里是一个完整帧尾」的核心手段：X.25 CRC 的性质决定了
// 一段合法帧（内容 + 校验序列）重算后必然留下固定余数 0xF0B8。因此本函数
// 不关心帧长字段，只回答「从某处到此的字节是不是一致」。
func GoodFCS(data []byte) bool { return crc(data, 0xFFFF) == 0xF0B8 }

// appendFCS 把小端的两字节校验序列追加到 dst 末尾。
func appendFCS(dst []byte) []byte {
	sum := FCS(dst)
	return append(dst, byte(sum), byte(sum>>8))
}

// ---------------------------------------------------------------------------
// 组帧 / 解帧
// ---------------------------------------------------------------------------

// header 组装「格式字段 + 地址 + 控制域」，即 HCS 覆盖的范围。
//
// 格式字段是 16 位：高 4 位是格式类型 3（0xA），bit11 是分段位，低 11 位是
// 长度。长度在地址与控制域长度确定前算不出来，但 **HCS 覆盖格式字段**，
// 所以这里必须由调用方（Encode）先把长度算好传进来，不能先占位后回填——
// 回填会让 HCS 与格式字段不一致，接收方判定「头校验失败」并丢弃整帧。
func header(dest, src []byte, ctl byte, seg bool, length int) []byte {
	hi := byte(FormatType3)
	if seg {
		hi |= 0x08
	}
	hi |= byte(length>>8) & 0x07
	out := make([]byte, 0, 2+len(dest)+len(src)+1)
	out = append(out, hi, byte(length))
	out = append(out, dest...)
	out = append(out, src...)
	out = append(out, ctl)
	return out
}

// Encode 输出一帧（不含两端的 7E 标志）。
//
// 长度字段 = 两个标志之间的全部字节数，含 2 字节格式字段、地址、控制域、
// HCS、信息域与末尾 FCS，但不含标志位。有信息域时 HCS 与 FCS 都在；
// 没有信息域时 HCS 就是唯一的校验序列（此时长度恒为 7）。
//
// 标准金向量（无信息域）：
//
//	7E A0 07 03 21 93 0F 01 7E
//
// 其中 A0 07 = 长度 7；dest=0x03（客户端地址 1）、src=0x21（按 7 位解码是客户端 16；此示例字节取自 IEC 62056-46 §6.4.2.3）、
// control=0x93（SNRM），HCS 覆盖 A0 07 03 21 93 得 `0F 01`。
func (f Frame) Encode() ([]byte, error) {
	dest, err := f.Dest.Encode()
	if err != nil {
		return nil, err
	}
	src, err := f.Src.Encode()
	if err != nil {
		return nil, err
	}
	ctl := EncodeControl(f.Control)
	// 先算长度：格式 2 + dest + src + ctl 1 + HCS 2 + info +（有 info 时 FCS 2）。
	length := 2 + len(dest) + len(src) + 1 + 2 + len(f.Info)
	if len(f.Info) > 0 {
		length += 2
	}
	// 上限 0x7FF（11 位）。超过不是「一帧装不下」，而是调用方把一段超过
	// 最大信息长度的 APDU 塞进了一帧；静默截断会让长度字段与实际字节数
	// 不符，接收方永远等不到结束标志，症状是「第一帧之后就超时」。
	if length > MaxFrameLength {
		return nil, fmt.Errorf("%w: 帧长 %d 超过 11 位上限 %d", ErrFrame, length, MaxFrameLength)
	}
	buf := header(dest, src, ctl, f.Segmented, length)
	// HCS：覆盖 format+dest+src+ctl。无信息域时它就是唯一的校验序列。
	buf = appendFCS(buf)
	if len(f.Info) > 0 {
		buf = append(buf, f.Info...)
		buf = appendFCS(buf)
	}
	return buf, nil
}

// FrameLen 推算整帧字节数（**含两端标志**）。
//
// 两种输入都接受：
//   - 以 7E 起始标志开头的流缓冲（Reader 的用法）；
//   - 不含标志的 body（组帧/测试的视角）。
//
// 只用于决定"还该读多少字节"，不作合法性判定（那由 Parse 负责）。
func FrameLen(b []byte) (int, bool) {
	off := 0
	if len(b) > 0 && b[0] == Flag {
		off = 1
	}
	if len(b) < off+2 || b[off]&FormatType3Mask != FormatType3 {
		return 0, false
	}
	n := int(b[off]&0x07)<<8 | int(b[off+1])
	if n < 7 || n > MaxFrameLength {
		return 0, false
	}
	// 整帧 = 起始标志 + n 字节正文 + 结束标志。
	return n + 2, true
}

// ParseRaw 从含首尾标志的字节 b（`7E ... 7E`）解出一帧。
//
// 与 Parse 的差别只在两端：ParseRaw 负责剥掉前导与末尾的 7E，并把两标志之间
// 的内容交给 Parse。之所以单开一个入口：流上天然带着标志，而组帧/测试用的是
// 不带标志的 body——两个视角各有一个函数，比在每个调用点都记得剥标志安全。
//
// # 为什么必须信长度字段，而不能"扫下一个 7E"
//
// 这是本文件里最容易写错的一处，而且错法很隐蔽：类型 3 帧格式**不做透明
// 转义**（既没有比特填充，也没有字节转义），这正是它带 11 位长度字段的理由。
// 因此 info 域与 FCS 里都可以合法出现 0x7E。功率因数 0.985 那一帧的 FCS
// 就是 `7E 64`。按"扫到下一个 7E 就收尾"去分帧，会把这一帧从中间切断，
// 症状是**只有某些点位超时**——换一台表、换一天数据就"自己好了"，极难复现。
//
// 所以帧边界 = 起始标志 + 长度字段声明的字节数 + 结束标志。长度字段与结束
// 标志不一致时判为失步，丢弃一个字节重扫（真实总线上噪声之后就是这么恢复的）。
func ParseRaw(b []byte) (Frame, int, error) {
	// 跳过前导标志（帧间可能插多个 7E）。
	start := 0
	for start < len(b) && b[start] == Flag {
		start++
	}
	if start >= len(b) {
		return Frame{}, start, ErrIncomplete // 全是标志，等更多字节
	}
	body := b[start:]
	if len(body) < 2 {
		return Frame{}, start, ErrIncomplete // 长度字段还没收全
	}
	if body[0]&FormatType3Mask != FormatType3 {
		// 不是帧的起点：丢掉这个字节重扫，不要把它当成帧内容吞下去。
		return Frame{}, start + 1, fmt.Errorf("%w: 格式类型 %02X 不是类型 3", ErrFrame, body[0]&FormatType3Mask)
	}
	n := int(body[0]&0x07)<<8 | int(body[1])
	if n < 7 || n > MaxFrameLength {
		return Frame{}, start + 1, fmt.Errorf("%w: 长度字段 %d 越界", ErrFrame, n)
	}
	// 需要 n 字节正文 + 1 字节结束标志。
	if len(body) < n+1 {
		return Frame{}, start, ErrIncomplete
	}
	if body[n] != Flag {
		// 结束标志不在长度字段指定的位置：失步，丢一个字节重扫。
		return Frame{}, start + 1, fmt.Errorf("%w: 第 %d 字节是 %02X 而不是结束标志 %02X",
			ErrFrame, n+1, body[n], Flag)
	}
	fr, _, err := Parse(body[:n])
	if err != nil {
		// 这一帧坏了，但边界是可信的：整段跳过，不在里面重扫——真正的
		// info 里可能有 7E，重扫会连环错位。
		return Frame{}, start + n + 1, err
	}
	return fr, start + n + 1, nil
}

// Parse 从 b（不含首尾标志）解出一帧。返回帧与消耗的字节数。
//
// 分帧依据是**长度字段 + 两道校验序列**，任何一处不满足都拒绝：
//
//  1. 格式类型必须是 3（高 4 位 0xA）；
//  2. 长度字段必须等于实际字节数（长度覆盖到 FCS 为止）；
//  3. 地址域与控制域必须按扩展位解得开；
//  4. HCS 覆盖 `format+dest+src+ctl`，必须自洽；
//  5. 有 info 时 FCS 覆盖 `format..info`，必须自洽。
//
// 两道校验都验，才能避免「按错误长度切帧后把 HCS 当 info」。校验序列是
// 分帧的唯一可靠依据：长度字段写错时 FCS 会兜住，FCS 写错时长度字段会兜住。
func Parse(b []byte) (Frame, int, error) {
	var f Frame
	if len(b) < 2 {
		return f, 0, ErrIncomplete
	}
	if b[0]&FormatType3Mask != FormatType3 {
		return f, 0, fmt.Errorf("%w: 格式类型 %02X 不是类型 3", ErrFrame, b[0]&FormatType3Mask)
	}
	n := int(b[0]&0x07)<<8 | int(b[1])
	if n < 7 || n > MaxFrameLength {
		return f, 0, fmt.Errorf("%w: 长度字段 %d 越界", ErrFrame, n)
	}
	if len(b) < n {
		return f, 0, ErrIncomplete
	}
	frame := b[:n]

	// 解 dest / src / ctl，定位 HCS。
	pos := 2
	dest, dn, err := DecodeAddress(frame[pos:])
	if err != nil {
		return f, 0, err
	}
	pos += dn
	f.Dest = dest
	src, sn, err := DecodeAddress(frame[pos:])
	if err != nil {
		return f, 0, err
	}
	pos += sn
	f.Src = src
	if pos >= len(frame) {
		return f, 0, fmt.Errorf("%w: 缺少控制域", ErrFrame)
	}
	f.Control = ParseControl(frame[pos])
	pos++

	// HCS 紧跟在控制域之后，覆盖 format+dest+src+ctl+HCS。
	if pos+2 > len(frame) {
		return f, 0, fmt.Errorf("%w: 缺少 HCS", ErrFrame)
	}
	if !GoodFCS(frame[:pos+2]) {
		return f, 0, fmt.Errorf("%w: HCS 不自洽", ErrChecksum)
	}
	f.HCS = append([]byte(nil), frame[pos:pos+2]...)
	pos += 2

	rest := frame[pos:]
	if len(rest) == 0 {
		// 无 info：HCS 就是唯一的校验序列，长度恒为 7。
		if n != 7 {
			return f, 0, fmt.Errorf("%w: 无信息域但长度为 %d（应为 7）", ErrFrame, n)
		}
		f.Length = n
		f.Segmented = b[0]&0x08 != 0
		return f, n, nil
	}
	// 有 info：末尾两字节是 FCS，FCS 覆盖整帧。
	if len(rest) < 2 {
		return f, 0, fmt.Errorf("%w: 缺少 FCS", ErrFrame)
	}
	if !GoodFCS(frame) {
		return f, 0, fmt.Errorf("%w: FCS 不自洽", ErrChecksum)
	}
	f.FCS = append([]byte(nil), rest[len(rest)-2:]...)
	f.Info = append([]byte(nil), rest[:len(rest)-2]...)
	f.Length = n
	f.Segmented = b[0]&0x08 != 0
	return f, n, nil
}
