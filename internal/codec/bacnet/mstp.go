package bacnet

// MS/TP（Master-Slave/Token-Passing，ASHRAE 135 clause 9）分帧。
//
// BACnet/IP 与 MS/TP 的区别不只是换了一条物理线：MS/TP 在 NPDU 之外又套了
// 一层自己的帧（preamble + 帧类型 + 目的/源 MAC + 长度 + 两条 CRC）。这一层
// 只负责「在一串字节里切出一帧、并证明它没有被 RS-485 上的干扰改坏」，APDU
// 与 NPDU 的语义仍由 bacnet.go 负责，因此 MS/TP 主站与 BACnet/IP 主站可以
// 共用同一套服务构造与应答解析。
//
// 帧布局（全部字段按出现顺序）：
//
//	55 FF | type(1) | dest(1) | src(1) | len(2, MSB first) | hdrCRC(1) | data(len) | dataCRC(2, LSB first)
//
// 三条容易踩的坑，写在这里免得以后重新踩：
//
//  1. 长度字段是 **2 字节大端**，而数据 CRC 是 **2 字节小端**。同一帧里两种
//     字节序并存，是标准原文如此，不是笔误。
//  2. 头 CRC 是 8 位、数据 CRC 是 16 位。早期一些实现（以及不少二手资料）
//     把头 CRC 的算法套到数据上，结果只能收到「长度 ≤ 4 字节」的 Token 帧，
//     一收数据帧就丢。
//  3. 头 CRC 的多项式不能用常见的 CRC-8/0x8C 反射式代替。标准给出的是一段
//     特殊的移位展开，校验有效帧（含 CRC 字节）时残差必须是 0x55；数据 CRC
//     则是 CRC-16/X-25（反射多项式 0x8408、初值 0xFFFF、结果取反），校验
//     有效帧（含两字节 CRC）时残差必须是 0xF0B8。这两条残差是现场判断
//     「算法抄对了没有」最快的办法，测试里也直接断言它们。

// MS/TP 的帧类型。数值是标准固定的，不能改。
const (
	FrameToken                 = 0 // 令牌：把发送权交给目的站
	FramePollForMaster         = 1 // 寻找下一个主站（只在主站地址段内轮询）
	FrameReplyToPollForMaster  = 2 // 对 PollForMaster 的应答，表示「我是主站且想要令牌」
	FrameTestRequest           = 3 // 链路测试请求
	FrameTestResponse          = 4 // 链路测试应答
	FrameDataExpectingReply    = 5 // 数据帧：发送方期望一个应答
	FrameDataNotExpectingReply = 6 // 数据帧：发送方不期望应答（广播或纯上送）
	FrameReplyPostponed        = 7 // 从站暂时无法应答，稍后主动上送
)

// MS/TP 帧头与地址空间。
const (
	// MSTPPreamble1 / MSTPPreamble2 是每帧开头固定的两个字节。0x55 的位模式
	// 在 RS-485 上产生方波，接收端据此做位同步；0xFF 是帧起始定界符。
	MSTPPreamble1 = 0x55
	MSTPPreamble2 = 0xFF

	// MSTPBroadcast 是广播地址。
	MSTPBroadcast = 0xFF

	// MS/TP 的 MAC 地址空间：0..127 是主站，128..254 是从站，255 是广播。
	// 从站不能持有令牌，因此主站只能把 Token / PollForMaster 发给 0..127；
	// 反过来，发给 128..254 的数据帧只能是「主站问从站」，从站的应答不会
	// 把令牌带走。这条区分决定了轮询从站时不需要考虑令牌轮转。
	MSTPMasterMin = 0
	MSTPMasterMax = 127
	MSTPSlaveMin  = 128
	MSTPSlaveMax  = 254

	// MSTPMaxData 是一帧数据段的最大长度。NPDU 头 2 字节 + APDU 最多 480
	// 字节（MS/TP 的最大 APDU），再留出一点余量，标准给的上限是 501。
	MSTPMaxData = 501

	// MSTPHeaderLen 是 preamble 到 header CRC 的总长度。
	MSTPHeaderLen = 8
)

// FrameTypeName 返回帧类型的中文/英文混合名，供日志与自检输出使用。
func FrameTypeName(t byte) string {
	switch t {
	case FrameToken:
		return "token"
	case FramePollForMaster:
		return "poll-for-master"
	case FrameReplyToPollForMaster:
		return "reply-to-poll-for-master"
	case FrameTestRequest:
		return "test-request"
	case FrameTestResponse:
		return "test-response"
	case FrameDataExpectingReply:
		return "data-expecting-reply"
	case FrameDataNotExpectingReply:
		return "data-not-expecting-reply"
	case FrameReplyPostponed:
		return "reply-postponed"
	}
	return "unknown"
}

// IsMasterMAC 判断地址是否落在主站段（0..127）。
func IsMasterMAC(mac byte) bool { return mac <= MSTPMasterMax }

// IsSlaveMAC 判断地址是否落在从站段（128..254）。
func IsSlaveMAC(mac byte) bool { return mac >= MSTPSlaveMin && mac <= MSTPSlaveMax }

// ---------------------------------------------------------------------------
// CRC
// ---------------------------------------------------------------------------

// CRCHeader 计算 8 位头校验并取反，返回值就是帧里要写的那个字节。
//
// 算法逐字对应标准：先把当前 CRC 与数据字节异或，再对它做 1..7 位的移位
// 折叠，最后把低 8 位（去掉最低位）与最高位的异或作为结果。它不是常规
// 的 CRC-8 移位寄存器形式，所以不能用「多项式 0x8C + 反射」来替代。
func CRCHeader(data []byte) byte {
	crc := byte(0xFF)
	for _, b := range data {
		crc = crcHeaderByte(crc, b)
	}
	return ^crc
}

func crcHeaderByte(crc, data byte) byte {
	c := uint16(crc) ^ uint16(data)
	// 所有移位项都必须由「异或数据后的原始值」一次算出。写成
	// `c ^= c<<1; c ^= c<<2` 会让后一项用到前一项的结果，得到的是一条
	// 完全不同的 CRC——这个错误在只看残差时不一定暴露，但单字节向量
	// （crcHeaderByte(0,1) 应为 0xFE）会立刻失败。
	c = c ^ c<<1 ^ c<<2 ^ c<<3 ^ c<<4 ^ c<<5 ^ c<<6 ^ c<<7
	return byte(c&0xFE) ^ (byte(c>>8) & 0x01)
}

// HeaderResidue 计算「头字段 + 收到的 CRC 字节」的校验残差。
// 有效帧必须等于 0x55；这是测试与抓包工具判断头 CRC 是否正确的依据。
func HeaderResidue(fieldsWithCRC []byte) byte {
	crc := byte(0xFF)
	for _, b := range fieldsWithCRC {
		crc = crcHeaderByte(crc, b)
	}
	return crc
}

// CheckHeaderCRC 校验头字段（type..len，不含 preamble）与随后的 CRC 字节。
func CheckHeaderCRC(fieldsWithCRC []byte) bool {
	return HeaderResidue(fieldsWithCRC) == 0x55
}

// CRCData 计算 16 位数据校验并取反，发送时低字节在前。
//
// 它等价于 CRC-16/X-25：反射多项式 0x8408（即 0x1021 的位反转）、初值
// 0xFFFF、结果取反。标准原文给的是展开式，这里照抄展开式而不是查表，
// 原因是表只有 256 项却会把算法藏在数据里；一帧最多 501 字节，逐字节
// 展开的代价在 9600～115200 的串口上可以忽略。
func CRCData(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc = crcDataByte(crc, b)
	}
	return ^crc
}

func crcDataByte(crc uint16, data byte) uint16 {
	low := (crc ^ uint16(data)) & 0xFF
	return (crc >> 8) ^
		(low << 8) ^
		(low << 3) ^
		(low << 12) ^
		(low >> 4) ^
		(low & 0x0F) ^
		((low & 0x0F) << 7)
}

// DataResidue 计算「数据 + 收到的两个 CRC 字节（低字节在前）」的残差。
// 有效帧必须等于 0xF0B8。
func DataResidue(dataWithCRC []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range dataWithCRC {
		crc = crcDataByte(crc, b)
	}
	return crc
}

// CheckDataCRC 校验数据段与其后两字节 CRC。
func CheckDataCRC(dataWithCRC []byte) bool {
	return DataResidue(dataWithCRC) == 0xF0B8
}

// ---------------------------------------------------------------------------
// 帧
// ---------------------------------------------------------------------------

// MSTPFrame 是一帧解析后的形式。Data 只对数据帧（类型 5/6）非空；
// 其余帧类型不允许携带数据，BuildMSTPFrame 会拒绝这种组合。
type MSTPFrame struct {
	Type byte
	Dest byte
	Src  byte
	Data []byte
}

// IsData 判断是否为承载 NPDU 的数据帧。
func (f MSTPFrame) IsData() bool {
	return f.Type == FrameDataExpectingReply || f.Type == FrameDataNotExpectingReply
}

// BuildMSTPFrame 把一帧序列化为线上字节（含两条 CRC）。
//
// 只做结构校验，不判断「这个帧类型是否该由当前角色发送」：那属于驱动/
// 从站的状态机；编解码层越权判断会让它无法用于抓包分析。
func BuildMSTPFrame(f MSTPFrame) ([]byte, error) {
	if f.Type > FrameReplyPostponed {
		return nil, proto("unknown MS/TP frame type %d", f.Type)
	}
	if len(f.Data) > MSTPMaxData {
		return nil, proto("MS/TP data length %d exceeds %d", len(f.Data), MSTPMaxData)
	}
	switch f.Type {
	case FrameToken, FramePollForMaster, FrameReplyToPollForMaster,
		FrameTestRequest, FrameTestResponse, FrameReplyPostponed:
		if len(f.Data) != 0 {
			return nil, proto("MS/TP %s frame must not carry data (%d bytes)",
				FrameTypeName(f.Type), len(f.Data))
		}
	case FrameDataExpectingReply, FrameDataNotExpectingReply:
		if len(f.Data) < 2 {
			// 数据段至少要有 NPDU 的 version + control 两个字节。
			return nil, proto("MS/TP %s frame carries no NPDU", FrameTypeName(f.Type))
		}
	}

	head := make([]byte, 0, MSTPHeaderLen)
	head = append(head,
		MSTPPreamble1, MSTPPreamble2,
		f.Type, f.Dest, f.Src,
		byte(len(f.Data)>>8), byte(len(f.Data)))
	head = append(head, CRCHeader(head[2:]))

	out := make([]byte, 0, MSTPHeaderLen+len(f.Data)+2)
	out = append(out, head...)
	out = append(out, f.Data...)
	crc := CRCData(f.Data)
	// 数据 CRC 低字节先发——与长度字段的大端相反。
	out = append(out, byte(crc), byte(crc>>8))
	return out, nil
}

// ErrMSTPBadPreamble 表示缓冲区不是以 0x55 0xFF 开头。
var ErrMSTPBadPreamble = proto("MS/TP frame does not start with 0x55 0xFF")

// ErrMSTPBadCRC 表示头校验或数据校验不通过。
var ErrMSTPBadCRC = proto("MS/TP frame CRC mismatch")

// DecodeMSTPFrame 从 buf 起始处解析一帧，返回帧与消耗的字节数。
//
// 要求 buf 恰好从 preamble 开始；不负责在噪声里寻找帧边界——那是流式
// 读取器的事（驱动与内置从站各自持有一个逐字节扫描的读取循环）。这样
// 分工是为了让本函数保持纯函数：输入一段已知起点的字节，输出一帧或
// 一条明确的错误。
//
// 末尾的 pad 字节（0xFF，用于让帧之间至少隔一个字符时间）不属于帧本身，
// 不会被计入 consumed；调用方可以自行跳过。
func DecodeMSTPFrame(buf []byte) (MSTPFrame, int, error) {
	if len(buf) < MSTPHeaderLen {
		return MSTPFrame{}, 0, ErrTruncated
	}
	if buf[0] != MSTPPreamble1 || buf[1] != MSTPPreamble2 {
		return MSTPFrame{}, 0, ErrMSTPBadPreamble
	}
	typ, dest, src := buf[2], buf[3], buf[4]
	length := int(buf[5])<<8 | int(buf[6])
	if length > MSTPMaxData {
		return MSTPFrame{}, 0, proto("MS/TP data length %d exceeds %d", length, MSTPMaxData)
	}
	if !CheckHeaderCRC(buf[2:MSTPHeaderLen]) {
		return MSTPFrame{}, 0, ErrMSTPBadCRC
	}
	need := MSTPHeaderLen + length + 2
	if len(buf) < need {
		return MSTPFrame{}, 0, ErrTruncated
	}
	body := buf[MSTPHeaderLen:need]
	if !CheckDataCRC(body) {
		return MSTPFrame{}, 0, ErrMSTPBadCRC
	}
	// 复制数据段：调用方的缓冲区通常会被下一次读取复用，返回一个别名
	// 会让「帧在队列里等着处理」变成极难复现的脏数据 bug。
	data := append([]byte(nil), body[:length]...)
	return MSTPFrame{Type: typ, Dest: dest, Src: src, Data: data}, need, nil
}
