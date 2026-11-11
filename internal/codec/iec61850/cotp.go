package iec61850

import (
	"encoding/binary"
	"fmt"
)

// 本文件实现 RFC 1006（TPKT）与 ISO 8073（COTP）里 IEC 61850 用到的那一小块。
//
// 与 codec/s7 中同名功能的关系：两者在链路上完全一样（都是 ISO-on-TCP，
// 端口 102），但 S7 的 COTP 只发「连接请求 + 数据」，而这里还要处理
// TPDU 大小协商与 TSAP——IEC 61850-8-1 规定 T-Selector 固定为 00 01，
// MMS 的对端会据此区分「这条连接是 MMS 而不是别的 ISO 协议」。因此不复用
// s7 的实现：那边把 CR 的参数写死成机架/槽号，改它会影响 S7 的现场兼容性。

// TPKT 的版本号，固定为 3。
const TPKTVersion = 0x03

// COTP 的 TPDU 类型（高 4 位）。
const (
	COTPCR = 0xE0 // 连接请求 Connection Request
	COTPCC = 0xD0 // 连接确认 Connection Confirm
	COTPDT = 0xF0 // 数据 Data
	COTPDR = 0x80 // 断开请求 Disconnect Request
	COTPDC = 0xC0 // 断开确认 Disconnect Confirm
	COTPER = 0x70 // 错误 Error
)

// COTP 参数代码。
const (
	cotpParamTPDUSize   = 0xC0
	cotpParamSrcTSAP    = 0xC1
	cotpParamDstTSAP    = 0xC2
	cotpParamChecksum   = 0xC3
	cotpParamVersion    = 0xC4
	cotpParamProtection = 0xC5
)

// DefaultTSAP 是 IEC 61850-8-1 规定的 T-Selector。
//
// 两段都为 00 01：调用侧与被调侧同名。真机（尤其带多个客户端口的装置）会
// 用 T-Selector 区分「这条连接属于哪个客户端」，但对 MMS 而言这个值只用于
// 建立连接时的匹配，不影响后续语义。
var DefaultTSAP = [2]byte{0x00, 0x01}

// COTP 可协商的 TPDU 大小（以 2 的幂表示，编码值是 log2）。
const (
	// minTPDUSize 是 ISO 8073 的最小值 128 字节：不带 TPDU 大小参数时对端
	// 按 128 处理，所以任何实现都必须能处理它。
	minTPDUSize = 128
	// maxTPDUSize 是本驱动主动提议的 8192 字节（编码 0x0D）。
	//
	// 不提议更大：COTP 的上限就是 8192，而 MMS PDU 一旦超过它就要在 COTP
	// 层分片。提议 8192 能让绝大多数读写在一次 TPDU 里走完。
	maxTPDUSize = 8192
)

// BuildTPKT 给载荷加上 4 字节 TPKT 头（版本 3 + 保留 + 总长度）。
func BuildTPKT(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	out[0] = TPKTVersion
	out[1] = 0
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	copy(out[4:], payload)
	return out
}

// ParseTPKT 校验并剥离 TPKT 头。
func ParseTPKT(b []byte) ([]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("iec61850: tpkt too short (%d)", len(b))
	}
	if b[0] != TPKTVersion {
		return nil, fmt.Errorf("iec61850: tpkt version %d, want 3", b[0])
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n < 4 {
		return nil, fmt.Errorf("iec61850: tpkt length %d is below the 4-byte header", n)
	}
	if n > len(b) {
		return nil, fmt.Errorf("iec61850: tpkt truncated: have %d, want %d", len(b), n)
	}
	// 多余的字节不报错：TCP 可能把两帧粘在一起，由调用方按长度切分。
	return b[4:n], nil
}

// TPKTLength 返回缓冲区开头那条 TPKT 声明的总长度。
//
// 用于「先读 4 字节头、再按长度读剩余」的分帧方式。返回 0 表示头部还不够。
func TPKTLength(head []byte) (int, error) {
	if len(head) < 4 {
		return 0, nil
	}
	if head[0] != TPKTVersion {
		return 0, fmt.Errorf("iec61850: tpkt version %d, want 3", head[0])
	}
	n := int(binary.BigEndian.Uint16(head[2:4]))
	if n < 4 {
		return 0, fmt.Errorf("iec61850: tpkt length %d is below the 4-byte header", n)
	}
	return n, nil
}

// COTP CR/CC 的协商参数。
type COTPOptions struct {
	// TPDUSize 是本端提议（或确认）的最大 TPDU 尺寸，取值 128..8192。
	TPDUSize int
	// SrcTSAP 是调用侧（本端发起时）的 T-Selector。
	SrcTSAP []byte
	// DstTSAP 是被调侧（对端）的 T-Selector。
	DstTSAP []byte
}

// DefaultCOTPOptions 返回 IEC 61850 的默认参数。
func DefaultCOTPOptions() COTPOptions {
	return COTPOptions{
		TPDUSize: maxTPDUSize,
		SrcTSAP:  DefaultTSAP[:],
		DstTSAP:  DefaultTSAP[:],
	}
}

// encodeOptions 写可变部分。
//
// 顺序是固定的：先 TPDU 大小，再被调 TSAP，最后调用 TSAP。顺序本身不影响
// 语义，但真机的解析器普遍按顺序扫，乱序实现（尤其把 0xC2 与 0xC1 换位）
// 在个别装置上会得到「连接被拒」而不是任何有用的错误。
func (o COTPOptions) encodeOptions() []byte {
	size := o.TPDUSize
	if size <= 0 {
		size = maxTPDUSize
	}
	if size > maxTPDUSize {
		size = maxTPDUSize
	}
	if size < minTPDUSize {
		size = minTPDUSize
	}
	var out []byte
	out = append(out, cotpParamTPDUSize, 0x01, uint8(tpduSizeCode(size)))
	if len(o.DstTSAP) > 0 {
		out = append(out, cotpParamDstTSAP, byte(len(o.DstTSAP)))
		out = append(out, o.DstTSAP...)
	}
	if len(o.SrcTSAP) > 0 {
		out = append(out, cotpParamSrcTSAP, byte(len(o.SrcTSAP)))
		out = append(out, o.SrcTSAP...)
	}
	return out
}

// tpduSizeCode 把字节数换成编码值（log2）。
func tpduSizeCode(n int) int {
	code := 7 // 128
	v := minTPDUSize
	for v < n && code < 13 {
		v <<= 1
		code++
	}
	return code
}

// decodeTPDUSize 把编码值换回字节数。
func decodeTPDUSize(code byte) int {
	if code < 7 || code > 13 {
		return minTPDUSize
	}
	return minTPDUSize << (code - 7)
}

// BuildCOTPConnect 返回建立 ISO 连接的 COTP CR（已含 TPKT）。
func BuildCOTPConnect(opts COTPOptions, srcRef uint16) []byte {
	v := opts.encodeOptions()
	// LI 只数它自己之后的部分：类型(1) + DST-REF(2) + SRC-REF(2) + 类别(1) + 参数。
	pdu := make([]byte, 0, 7+len(v))
	pdu = append(pdu,
		byte(6+len(v)),
		COTPCR,
		0x00, 0x00, // DST-REF，发起方填 0
		byte(srcRef>>8), byte(srcRef),
		0x00, // 协议类别 0（TPKT over TCP 只用类别 0）
	)
	pdu = append(pdu, v...)
	return BuildTPKT(pdu)
}

// COTPConnect 返回使用默认参数的 CR。
func COTPConnect() []byte {
	return BuildCOTPConnect(DefaultCOTPOptions(), 1)
}

// COTPConfirm 是解析出来的 CC。
type COTPConfirm struct {
	// TPDUSize 是对端确认的 TPDU 大小。实际可用值是两端提议的较小者，
	// 由调用方按 min 取。
	TPDUSize int
	// SrcTSAP 是对端的调用侧 T-Selector。
	SrcTSAP []byte
	// DstTSAP 是对端的被调侧 T-Selector。
	DstTSAP []byte
	// Class 是协议类别。IEC 61850 只用类别 0。
	Class byte
}

// ParseCOTPConfirm 解析 CC。
func ParseCOTPConfirm(pdu []byte) (COTPConfirm, error) {
	payload, err := ParseTPKT(pdu)
	if err != nil {
		return COTPConfirm{}, err
	}
	if len(payload) < 7 {
		return COTPConfirm{}, fmt.Errorf("iec61850: cotp cc too short (%d)", len(payload))
	}
	if payload[1] != COTPCC {
		return COTPConfirm{}, fmt.Errorf("iec61850: cotp tpdu type 0x%02X, want CC (0x%02X)", payload[1], COTPCC)
	}
	out := COTPConfirm{TPDUSize: minTPDUSize, Class: payload[6]}
	li := int(payload[0])
	if li < 6 || 1+li > len(payload) {
		// LI 与实际长度不符时按「没有参数」处理比直接失败更宽容：一部分
		// 装置把 LI 写成 0，但其余字段仍然是对的。TPDU 大小退回 128，
		// 后续分片会因此更保守，不会出错。
		return out, nil
	}
	parseCOTPOptions(payload[7:1+li], &out)
	return out, nil
}

// ParseCOTPConnectRequest 解析 CR（内置从站用）。
func ParseCOTPConnectRequest(pdu []byte) (COTPConfirm, uint16, error) {
	payload, err := ParseTPKT(pdu)
	if err != nil {
		return COTPConfirm{}, 0, err
	}
	if len(payload) < 7 {
		return COTPConfirm{}, 0, fmt.Errorf("iec61850: cotp cr too short (%d)", len(payload))
	}
	if payload[1] != COTPCR {
		return COTPConfirm{}, 0, fmt.Errorf("iec61850: cotp tpdu type 0x%02X, want CR (0x%02X)", payload[1], COTPCR)
	}
	srcRef := binary.BigEndian.Uint16(payload[4:6])
	out := COTPConfirm{TPDUSize: minTPDUSize, Class: payload[6]}
	li := int(payload[0])
	if li >= 6 && 1+li <= len(payload) {
		parseCOTPOptions(payload[7:1+li], &out)
	}
	return out, srcRef, nil
}

// parseCOTPOptions 解析 CR/CC 的参数段。
func parseCOTPOptions(b []byte, out *COTPConfirm) {
	for i := 0; i+1 < len(b); {
		code := b[i]
		n := int(b[i+1])
		if i+2+n > len(b) {
			return
		}
		val := b[i+2 : i+2+n]
		switch code {
		case cotpParamTPDUSize:
			if n == 1 {
				out.TPDUSize = decodeTPDUSize(val[0])
			}
		case cotpParamSrcTSAP:
			out.SrcTSAP = append([]byte(nil), val...)
		case cotpParamDstTSAP:
			out.DstTSAP = append([]byte(nil), val...)
		}
		i += 2 + n
	}
}

// BuildCOTPConfirm 返回 CC（已含 TPKT）。
func BuildCOTPConfirm(opts COTPOptions, srcRef, dstRef uint16) []byte {
	v := opts.encodeOptions()
	pdu := make([]byte, 0, 7+len(v))
	pdu = append(pdu,
		byte(6+len(v)),
		COTPCC,
		byte(dstRef>>8), byte(dstRef), // DST-REF 回填对端的 SRC-REF
		byte(srcRef>>8), byte(srcRef),
		0x00,
	)
	pdu = append(pdu, v...)
	return BuildTPKT(pdu)
}

// BuildCOTPData 把一段用户数据包成 DT（已含 TPKT）。
//
// 末尾分片位置 1（EOT=1）：本实现不在 COTP 层做分片，一个 TPDU 就是一整段
// 用户数据。MMS 的 PDU 若超过协商的 TPDU 大小，由上层在编码前就拒绝——
// 那样报出的是「PDU 太大」而不是在 COTP 层莫名其妙地丢半段。
func BuildCOTPData(payload []byte) []byte {
	pdu := make([]byte, 0, 3+len(payload))
	pdu = append(pdu, 0x02, COTPDT, 0x80)
	pdu = append(pdu, payload...)
	return BuildTPKT(pdu)
}

// ParseCOTPData 从 DT 里取出用户数据。
func ParseCOTPData(pdu []byte) ([]byte, bool, error) {
	payload, err := ParseTPKT(pdu)
	if err != nil {
		return nil, false, err
	}
	if len(payload) < 3 {
		return nil, false, fmt.Errorf("iec61850: cotp dt too short (%d)", len(payload))
	}
	if payload[1] != COTPDT {
		return nil, false, fmt.Errorf("iec61850: cotp tpdu type 0x%02X, want DT (0x%02X)", payload[1], COTPDT)
	}
	return payload[3:], payload[2]&0x80 != 0, nil
}

// IsCOTPDisconnect 判断一个 TPKT 载荷是否是断开请求。
func IsCOTPDisconnect(payload []byte) bool {
	return len(payload) >= 2 && payload[1] == COTPDR
}

// BuildCOTPDisconnect 返回 DR（已含 TPKT）。
func BuildCOTPDisconnect(srcRef, dstRef uint16) []byte {
	pdu := []byte{
		0x06, COTPDR,
		byte(dstRef >> 8), byte(dstRef),
		byte(srcRef >> 8), byte(srcRef),
		0x00, // 原因：正常断开
	}
	return BuildTPKT(pdu)
}
