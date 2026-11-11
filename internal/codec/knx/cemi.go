package knx

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// cEMI（common External Message Interface）是 KNX 链路层数据的封装格式，
// 隧道报文里携带的就是它。
//
// 一条 L_Data.req 的布局：
//
//	0     消息码 0x11 = L_Data.req（另有 0x29 L_Data.ind、0x2E L_Data.con）
//	1     附加信息长度（我们恒为 0，但解析时必须跳过它给的长度）
//	2     控制字段 1
//	3     控制字段 2
//	4..5  源物理地址（大端 16 位）
//	6..7  目的地址（组地址或物理地址，大端 16 位）
//	8     TPDU 长度
//	9..   TPDU：TPCI 字节 + APCI/数据
//
// 控制字段 1（2 字节里的高字节）：
//
//	bit7 帧类型 1=标准帧
//	bit6 保留（0）
//	bit5 不重复
//	bit4 系统广播
//	bit3-2 优先级 00 系统 / 01 普通 / 10 紧急 / 11 低
//	bit1 需要确认（仅 L_Data.req 有效）
//	bit0 确认有错（仅 L_Data.con 有意义）
//
// 控制字段 2（低字节）：
//
//	bit7 地址类型 1=组地址 / 0=物理地址
//	bit6 保留（0）
//	bit5-4 跳数
//	bit3-0 扩展帧格式 0000=标准
const (
	// MsgLDataReq 是本端要往总线上发数据（隧道客户端 → 网关）。
	MsgLDataReq = 0x11
	// MsgLDataInd 是总线上的数据上报（网关 → 隧道客户端）。
	MsgLDataInd = 0x29
	// MsgLDataCon 是对本端 L_Data.req 的链路层确认。
	MsgLDataCon = 0x2E
)

// 控制字段 1 的位。
const (
	ctrl1StdFrame   = 0x80 // 标准帧
	ctrl1NoRepeat   = 0x20 // 不重复
	ctrl1SystemBcst = 0x10 // 系统广播
	ctrl1WantAck    = 0x02 // 请求链路层确认
	ctrl1Error      = 0x01 // 确认有错
)

// 控制字段 2 的位。
const (
	ctrl2GroupAddr = 0x80 // 目的地址是组地址
)

// 优先级取值（控制字段 1 的 bit3-2）。
const (
	PrioSystem = 0
	PrioNormal = 1
	PrioUrgent = 2
	PrioLow    = 3
)

// APCI 服务码。APCI 是 10 位：高 2 位在 TPCI 字节的低 2 位，低 8 位在
// 下一个字节。下面用「10 位值」表达，编码时再拆开。
const (
	APCIGroupValueRead     = 0x0000
	APCIGroupValueResponse = 0x0040
	APCIGroupValueWrite    = 0x0080
)

// maxAPDU 是标准帧能承载的最大 APDU 字节数（TPDU 长度字段是 4 位，
// 上限 15；再大就必须用扩展帧）。
const maxAPDU = 15

// LData 是一条链路层数据报文。
type LData struct {
	// MsgCode 是消息码：MsgLDataReq / MsgLDataInd / MsgLDataCon。
	MsgCode byte

	// Control1/Control2 是原始控制字段。编码时由 BuildLData 按字段填，
	// 解析时原样保留，便于诊断时回显。
	Control1 byte
	Control2 byte

	// Source 是源物理地址。
	Source IndividualAddress
	// Dest 是目的地址。IsGroup 决定它按组地址还是物理地址解释。
	Dest    uint16
	IsGroup bool

	// APCI 是应用层服务码（10 位）。
	APCI uint16
	// Data 是 APDU 的数据部分（不含 APCI 的两个字节）。
	Data []byte
	// Bit 是 1 位 DPT 的值（APCI 字节 bit0）。仅当 DPT 为 1 位时有效。
	Bit bool
}

// BuildLData 构造一条 L_Data 报文。
//
// 默认按「标准帧、普通优先级、请求确认、组地址、6 跳」编码——这正是
// 隧道客户端发 GroupValue 时网关期望的形态。跳数给 6 而不是 0：0 跳的
// 报文在部分老网关上会被当成「已经转发过」而丢弃。
func BuildLData(msgCode byte, src, dest uint16, isGroup bool, apci uint16, data []byte, bit bool) ([]byte, error) {
	if msgCode != MsgLDataReq && msgCode != MsgLDataInd && msgCode != MsgLDataCon {
		return nil, fmt.Errorf("knx: unsupported cEMI message code 0x%02X", msgCode)
	}
	if len(data) > maxAPDU-2 {
		return nil, fmt.Errorf("knx: APDU data %d bytes exceeds the standard frame limit (%d)", len(data), maxAPDU-2)
	}

	ctrl1 := byte(ctrl1StdFrame | PrioNormal<<2)
	if msgCode == MsgLDataReq {
		ctrl1 |= ctrl1WantAck
	}
	ctrl2 := byte(6 << 4) // 6 跳
	if isGroup {
		ctrl2 |= ctrl2GroupAddr
	}

	// TPDU = TPCI 字节 + APCI 高 2 位 | APCI 低 8 位 | 数据。
	//
	// 关键细节（KNX 03_03_04）：TPDU 长度字段**不包含 TPCI 字节本身**，
	// 也不包含 APCI 的高 2 位（那两位在 TPCI 字节里），只计「APCI 低 8 位
	// + 数据」。所以 1 位 DPT 的长度是 1（只有 APCI 低字节），2 字节 DPT
	// 的长度是 3。把它当成「整个 APDU 的字节数」会让网关把最后一个数据
	// 字节当成下一条报文的一部分，表现为「写入成功了，但值不对」。
	apciLow := byte(apci & 0xFF)
	apdu := make([]byte, 0, 2+len(data))
	apdu = append(apdu, byte(apci>>8)&0x03) // TPCI 字节的低 2 位 = APCI 高 2 位
	apdu = append(apdu, apciLow)
	apdu = append(apdu, data...)
	if bit && len(data) == 0 {
		// 1 位值放在 APCI 低字节的 bit0；其余 5 位保留为 0。
		apdu[1] = apciLow | 0x01
	}

	length := len(apdu) - 1
	if length < 1 || length > maxAPDU {
		return nil, fmt.Errorf("knx: TPDU length %d out of range 1..%d", length, maxAPDU)
	}

	out := make([]byte, 0, 2+6+1+len(apdu))
	out = append(out, msgCode, 0x00, ctrl1, ctrl2)
	out = binary.BigEndian.AppendUint16(out, src)
	out = binary.BigEndian.AppendUint16(out, dest)
	out = append(out, byte(length))
	out = append(out, apdu...)
	return out, nil
}

// BuildLDataCon 构造一条链路层确认（L_Data.con）。
//
// 与本端请求的差别只有控制字段 1 的 bit0：置位表示「确认有错」，即帧已经
// 发到总线但没有任何设备应答。单开一个函数而不是让调用方改字节：并发地
// 拼字节会把「第几个字节是控制字段」这个知识散到每个调用点，改一次格式就
// 要改全部（这个位置已经错过一次：附加信息段让控制字段从 2 变成了 2+n）。
func BuildLDataCon(src, dest uint16, isGroup bool, apci uint16, data []byte, bit, failed bool) ([]byte, error) {
	out, err := BuildLData(MsgLDataCon, src, dest, isGroup, apci, data, bit)
	if err != nil {
		return nil, err
	}
	if failed {
		// 0 = 消息码、1 = 附加信息长度（本函数只产生 0）、2 = 控制字段 1。
		out[2] |= ctrl1Error
	}
	return out, nil
}

// ParseLData 解析一条 cEMI L_Data 报文。
func ParseLData(b []byte) (LData, error) {
	var out LData
	if len(b) < 2 {
		return out, errors.New("knx: cEMI frame too short")
	}
	out.MsgCode = b[0]
	switch out.MsgCode {
	case MsgLDataReq, MsgLDataInd, MsgLDataCon:
	default:
		return out, fmt.Errorf("knx: unexpected cEMI message code 0x%02X (want L_Data.req/ind/con)", out.MsgCode)
	}

	// 附加信息段：长度字节 + 该长度的内容。隧道报文里通常是 0，
	// 但 L_Data.ind 可能带时间戳/相对时间，必须按声明跳过去。
	infoLen := int(b[1])
	pos := 2 + infoLen
	if len(b) < pos+7 {
		return out, fmt.Errorf("knx: cEMI frame truncated (info length %d)", infoLen)
	}
	out.Control1 = b[pos]
	out.Control2 = b[pos+1]
	out.Source = IndividualAddress(binary.BigEndian.Uint16(b[pos+2:]))
	out.Dest = binary.BigEndian.Uint16(b[pos+4:])
	out.IsGroup = out.Control2&ctrl2GroupAddr != 0
	length := int(b[pos+6])
	tpdu := b[pos+7:]

	// 长度字段 = APCI 低字节 + 数据，所以 TPdu 至少有 1（长度字段本身
	// 对应）+ 1（TPCI 字节）个字节。
	if length < 1 {
		return out, fmt.Errorf("knx: TPDU length %d is invalid", length)
	}
	if len(tpdu) < 1+length {
		return out, fmt.Errorf("knx: TPDU truncated: declared %d data bytes, got %d", length, len(tpdu)-1)
	}
	tpci := tpdu[0]
	if tpci&0x80 != 0 {
		// 控制 TPDU（T_Connect / T_Disconnect / T_ACK / T_NACK）：没有 APCI。
		return out, fmt.Errorf("knx: control TPDU 0x%02X is not an application frame", tpci)
	}
	out.APCI = uint16(tpci&0x03)<<8 | uint16(tpdu[1])
	if length == 1 {
		// 只有 APCI 低字节，没有数据域：这是 1 位 DPT，值就放在 APCI 低
		// 字节的 bit0。
		//
		// 因此 APCI 必须把这一位摘掉再报给调用方。不摘的话，一个「开」
		// 的 GroupValueWrite 会解析成 0x0081 而不是 0x0080，调用方按服务
		// 码 switch 时落到 default 分支——报文被静默丢弃，看上去像「写入
		// 成功但设备没反应」。
		out.Bit = tpdu[1]&0x01 != 0
		out.APCI &^= 0x01
	} else {
		out.Data = append([]byte(nil), tpdu[2:1+length]...)
	}
	return out, nil
}

// ConfirmationError 判断一条 L_Data.con 的「确认有错」位。
//
// 这一位是隧道客户端唯一能拿到的总线级反馈：帧已经发到总线，但没有任何
// 设备应答（组地址上没有执行器、设备掉线、地址写错）。它与「网关没收到
// 我们的帧」是两件事——后者由 TUNNELLING_ACK 反映，前者由这里反映。
// 把两者混成一句「写入失败」，现场就只能靠拔线复现。
//
// 对 L_Data.ind/req 这一位没有意义，返回 false。
func (m LData) ConfirmationError() bool {
	if m.MsgCode != MsgLDataCon {
		return false
	}
	return m.Control1&ctrl1Error != 0
}

// DecodeGroupValue 从一条 L_Data 中取出组值载荷，并按 DPT 解释。
func DecodeGroupValue(m LData, d DPT) (Payload, error) {
	switch m.APCI {
	case APCIGroupValueWrite, APCIGroupValueResponse:
	case APCIGroupValueRead:
		return Payload{}, errors.New("knx: GroupValueRead carries no value")
	default:
		return Payload{}, fmt.Errorf("knx: APCI 0x%03X is not a group value service", m.APCI)
	}
	if d.Bits == 1 {
		// 1 位 DPT 的值必须在 APCI 字节里，不能带数据域。带了数据说明
		// 对端把它当成更宽的类型发了，此时按 1 位读会得到错的值。
		if len(m.Data) != 0 {
			return Payload{}, fmt.Errorf("knx: dpt %s is 1 bit but the frame carries %d data bytes", d, len(m.Data))
		}
		return Payload{Bit: m.Bit}, nil
	}
	if len(m.Data) != d.Bytes() {
		return Payload{}, fmt.Errorf("knx: dpt %s needs %d data bytes, frame carries %d", d, d.Bytes(), len(m.Data))
	}
	return Payload{Data: m.Data}, nil
}
