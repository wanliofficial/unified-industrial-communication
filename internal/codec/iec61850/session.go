package iec61850

import (
	"errors"
	"fmt"
)

// 本文件实现 ISO 8327 会话层里 IEC 61850 用到的那一小块：CN（连接）、AC
// （接受）、DT（数据）、FN（有序释放）、AB（异常中止）与 RF（拒绝）。
//
// 会话层在 MMS 里看起来像一层没用的封装——数据帧的 SPDU 头恒为
// `01 00 01 00`。但它不是可选的：对端严格按 SPDU 解析，少一层就会把
// 表示层的第一个字节当成 SPDU 标识，报出的错误与真正的原因毫无关系。
//
// 一个必须记住的细节：CN/AC 的 LI（长度）**包含用户数据本身**，而 DT 的
// LI 不参与（DT 用的是固定四字节头）。把两者混为一谈是本层最常见的错误。

// SPDU 标识。
const (
	spduGiveTokens    = 0x01
	spduDataTransfer  = 0x01 // DT 的第二个字节也是 0x01（GT + DT 两段）
	spduNotFinished   = 0x08
	spduFinish        = 0x09
	spduDisconnect    = 0x0A
	spduRefuse        = 0x0C
	spduConnect       = 0x0D
	spduAccept        = 0x0E
	spduAbort         = 0x19
	spduDataOverflow  = 0x01
	spduConnectSecond = 0x0D
)

// SPDU 标识的导出别名（内置从站按它分流会话层：CN 关联、DT 数据、FN 释放）。
//
// 与上面的小写常量是同一组值；导出是为了让 sim 包不必硬编码 0x0D/0x01/0x09。
const (
	SPDUConnect      = spduConnect      // 0x0D 连接请求
	SPDUAccept       = spduAccept       // 0x0E 连接接受
	SPDUDataTransfer = spduDataTransfer // 0x01 数据传输
	SPDUFinish       = spduFinish       // 0x09 有序释放
	SPDUDisconnect   = spduDisconnect   // 0x0A 断开
	SPDURefuse       = spduRefuse       // 0x0C 拒绝
	SPDUAbort        = spduAbort        // 0x19 中止
)

// 会话层参数标识（PGI / PI）。
const (
	pgiConnectionIdentifier = 0x01 // 连接标识符
	pgiConnectAcceptItem    = 0x05 // Connect/Accept Item
	pgiSessionRequirement   = 0x14 // 会话要求
	pgiCallingSSSel         = 0x33 // 调用侧 SS-SEL
	pgiCalledSSSel          = 0x34 // 被调侧 SS-SEL
	pgiUserData             = 0xC1 // 用户数据
	piExtendedUserData      = 0xC2 // 扩展用户数据（>512 字节）
	piProtocolOptions       = 0x13 // 协议选项
	piVersionNumber         = 0x16 // 版本号
	piTransportDisconnect   = 0x11 // 传输连接释放
	piReasonCode            = 0x32 // 原因码
)

// 会话要求：双工功能单元。
const sessionReqDuplex = 0x0002

// SPDU LI 的边界。
const (
	// liShortMax 是 LI 短形式能表示的最大值。
	liShortMax = 254
	// liLongMax 是两字节长形式能表示的最大值。
	liLongMax = 65535
	// connectUserDataMax 是 CN 里 User Data PGI 能装的上限，超过它就改用
	// 扩展用户数据参数。这个阈值由 LI 短形式的上限（254）决定，不是策略选择。
	connectUserDataMax = liShortMax
)

// DataSPDUHeader 是 DT SPDU 的固定四字节头。
var DataSPDUHeader = [4]byte{spduGiveTokens, 0x00, spduDataTransfer, 0x00}

// SessionParams 是会话层的选择子。
type SessionParams struct {
	// CallingSSSel 是调用侧 SS-SEL，默认 00 01。
	CallingSSSel []byte
	// CalledSSSel 是被调侧 SS-SEL，默认 00 01。
	CalledSSSel []byte
}

// DefaultSessionParams 返回 IEC 61850 的默认选择子。
func DefaultSessionParams() SessionParams {
	return SessionParams{
		CallingSSSel: []byte{0x00, 0x01},
		CalledSSSel:  []byte{0x00, 0x01},
	}
}

// encodeUserData 写用户数据参数（标识 + LI + 内容）。

// userDataIDLen 是 userDataID 对应的开销：标识 + LI + 内容。
func userDataIDLen(payload int) int {
	if payload > liShortMax {
		return 1 + liSize(payload) + payload
	}
	return 1 + 1 + payload
}

// userDataID 是用户数据参数：内容不超过 254 字节时是 0xC1，超过时换成扩展
// 参数 0xC2（它的 LI 用三字节长形式）。
func userDataID(payload []byte) byte {
	if len(payload) > liShortMax {
		return piExtendedUserData
	}
	return pgiUserData
}

// encodeUserData 写用户数据参数（标识 + LI + 内容）。
func encodeUserData(payload []byte) []byte {
	out := []byte{userDataID(payload)}
	out = putLI(out, len(payload))
	return append(out, payload...)
}

// parseLI 解析会话层的长度字段（LI），返回长度值与它占用的字节数。
//
// 会话层的 LI 与 BER 的长度字段是两套独立编码，绝不能共用 parseLength：
//   - 0x00..0xFE：长度值就是这一个字节（所以 0x95 = 149，是合法长度）；
//   - 0xFF：后接**恰好两个**字节（big-endian）的长形式。
//
// BER 的 0x81/0x82/0x83 长形式在这里会被误读成内容长度的一部分；反过来会话
// 层的 0x95 会被 BER 解析器当成「不支持的长度形式」。两者必须分开。
//
// 注意长形式宽度是**固定的**，不是「1~3 个按内容猜」：ISO 8327-1 规定长形式
// 就是两字节。若按首字节去猜宽度，会把内容字节当成长度的一部分，凑出一个
// 超出 liLongMax 的假长度再报一句无关的失败。putLI/liSize 与这里的「恰好
// 两字节」必须保持一致。
func parseLI(b []byte) (n, hdr int, err error) {
	if len(b) == 0 {
		return 0, 0, ErrTruncated
	}
	first := b[0]
	if first < 0xFF {
		return int(first), 1, nil
	}
	// 0xFF：长形式，后接**恰好两个**字节（big-endian）。ISO 8327-1 规定长
	// 形式固定两字节（不是「1~3 个、按内容猜」）：猜宽会把内容字节当成长度的
	// 一部分，凑出一个超出 liLongMax 的假长度再报一句无关的失败。
	if len(b) < 3 {
		return 0, 0, ErrTruncated
	}
	v := int(b[1])<<8 | int(b[2])
	return v, 3, nil
}

// putLI 追加会话层长度字段。
//
// LI 是会话层专有的长度编码（ISO 8327-1 §8.2.5），与 BER 的长形式
// （0x81/0x82/0x83）是两套独立编码，混用会让对端按错误的字节数去读长度，
// 整条报文的边界随之错位。宽度规则与 parseLI 必须一致（都由 liSize 计算）：
//
//	长度 0..254     → 单字节，值即长度
//	长度 255..65535 → 三字节：0xFF + 恰好两个字节（big-endian）
//
// **255 走长形式，不是短形式。** 0xFF 是长形式标记，不能作为单字节 LI 的值；
// 所以长度 255 必须编成 `FF 00 FF`。曾有一个 `n <= 0xFF` 的中间分支把 255
// 编成 `FF FF`（只有一字节长度），既违反「长形式恰好两字节」，也无法被
// parseLI 还原（它会把标记后的两个字节都当长度，吃掉一个内容字节）。这个
// 分支已删除。
func putLI(dst []byte, n int) []byte {
	if n <= liShortMax {
		return append(dst, byte(n))
	}
	return append(dst, 0xFF, byte(n>>8), byte(n))
}

// liSize 返回 putLI 会写多少字节。
func liSize(n int) int {
	if n <= liShortMax {
		return 1
	}
	return 3
}

// encodeConnectAcceptItem 写 PGI 5：协议选项 + 版本号。
//
// 版本号恒为 2。写别的值不是「更新一点」，而是另一个不兼容的版本。
func encodeConnectAcceptItem(dst []byte) []byte {
	// PGI 5 的内容：协议选项（3 字节）+ 版本号（3 字节）。LI 是这两个参数
	// 的固定长度，与 SPDU 的 LI 是两套独立字段。
	dst = append(dst, pgiConnectAcceptItem, 0x06)
	dst = append(dst, piProtocolOptions, 0x01, 0x00)
	dst = append(dst, piVersionNumber, 0x01, 0x02)
	return dst
}

// BuildSessionConnect 生成 CN SPDU。
func BuildSessionConnect(p SessionParams, payload []byte) ([]byte, error) {
	calling := p.CallingSSSel
	if calling == nil {
		calling = DefaultSessionParams().CallingSSSel
	}
	called := p.CalledSSSel
	if called == nil {
		called = DefaultSessionParams().CalledSSSel
	}
	if len(calling) > 16 || len(called) > 16 {
		return nil, errors.New("iec61850: session selector longer than 16 bytes")
	}
	// CN 的 LI 包含用户数据本身；用户数据的 LI 在超过 512 字节时换成扩展
	// 参数（0xC2），因此算长度之前必须先确定用哪一种。
	userID := userDataID(payload)
	body := 8 + 4 + (2 + len(calling)) + (2 + len(called)) +
		1 + liSize(len(payload)) + len(payload)
	if body > liLongMax {
		return nil, fmt.Errorf("iec61850: session connect body %d exceeds %d", body, liLongMax)
	}

	out := make([]byte, 0, 1+liSize(body)+body)
	out = append(out, spduConnect)
	out = putLI(out, body)
	out = encodeConnectAcceptItem(out)

	out = append(out, pgiSessionRequirement, 0x02, 0x00, sessionReqDuplex)
	out = append(out, pgiCallingSSSel, byte(len(calling)))
	out = append(out, calling...)
	out = append(out, pgiCalledSSSel, byte(len(called)))
	out = append(out, called...)
	out = append(out, userID)
	out = putLI(out, len(payload))
	out = append(out, payload...)
	return out, nil
}

// BuildSessionAccept 生成 AC SPDU。
//
// AC 不带调用侧 SS-SEL：只有发起方需要宣告自己是谁。
func BuildSessionAccept(p SessionParams, payload []byte) ([]byte, error) {
	called := p.CalledSSSel
	if called == nil {
		called = DefaultSessionParams().CalledSSSel
	}
	body := 8 + 4 + (2 + len(called)) + 1 + userDataIDLen(len(payload))
	if body > liLongMax {
		return nil, fmt.Errorf("iec61850: session accept body %d exceeds %d", body, liLongMax)
	}
	out := make([]byte, 0, 1+liSize(body)+body)
	out = append(out, spduAccept)
	out = putLI(out, body)
	out = encodeConnectAcceptItem(out)
	out = append(out, pgiSessionRequirement, 0x02, 0x00, sessionReqDuplex)
	out = append(out, pgiCalledSSSel, byte(len(called)))
	out = append(out, called...)
	out = append(out, userDataID(payload))
	out = putLI(out, len(payload))
	out = append(out, payload...)
	return out, nil
}

// BuildSessionData 生成 DT SPDU：固定四字节头 + 载荷。
func BuildSessionData(payload []byte) []byte {
	out := make([]byte, 0, 4+len(payload))
	out = append(out, DataSPDUHeader[:]...)
	return append(out, payload...)
}

// BuildSessionFinish 生成 FN SPDU（有序释放）。
func BuildSessionFinish(payload []byte) ([]byte, error) {
	return buildUserDataSPDU(spduFinish, payload)
}

// BuildSessionDisconnect 生成 DN SPDU。
func BuildSessionDisconnect(payload []byte) ([]byte, error) {
	return buildUserDataSPDU(spduDisconnect, payload)
}

// buildUserDataSPDU 生成「只有一个用户数据参数」的 SPDU。
func buildUserDataSPDU(id byte, payload []byte) ([]byte, error) {
	body := 3 + 1 + userDataIDLen(len(payload))
	if body > liLongMax {
		return nil, fmt.Errorf("iec61850: session spdu body %d exceeds %d", body, liLongMax)
	}
	out := make([]byte, 0, 1+liSize(body)+body)
	out = append(out, id)
	out = putLI(out, body)
	out = append(out, userDataID(payload))
	out = putLI(out, len(payload))
	out = append(out, payload...)
	return out, nil
}

// BuildSessionAbort 生成 AB SPDU。
//
// 传输断开的取值 0x0B = 传输连接释放 | 用户中止 | 无原因，是 MMS 客户端
// 主动中止关联时的标准取值。
func BuildSessionAbort(payload []byte) ([]byte, error) {
	userID := userDataID(payload)
	body := 3 + 1 + userDataIDLen(len(payload))
	if body > liLongMax {
		return nil, fmt.Errorf("iec61850: session abort body %d exceeds %d", body, liLongMax)
	}
	out := make([]byte, 0, 1+liSize(body)+body)
	out = append(out, spduAbort)
	out = putLI(out, body)
	out = append(out, piTransportDisconnect, 0x01, 0x0B)
	out = append(out, userID)
	out = putLI(out, len(payload))
	return append(out, payload...), nil
}

// SPDU 是解析出来的一个会话层报文。
type SPDU struct {
	// ID 是 SPDU 标识（0x0D 连接、0x0E 接受、0x01 数据…）。
	ID byte
	// Payload 是用户数据（DT 的载荷，或 CN/AC/FN/DN/AB 的用户数据参数）。
	Payload []byte
	// Reason 是 RF/AB 里的原因码；其它 SPDU 为 0。
	Reason byte
	// CallingSSSel / CalledSSSel 只在 CN 里有值。
	CallingSSSel []byte
	CalledSSSel  []byte
}

// ParseSPDU 解析一个会话层报文。
//
// DT 单独走一条路径：它的头是固定的四字节，没有 LI 语义。用通用解析器去读
// DT 会把第一个 0x00 当成 LI，得到一个长度为 0 的报文，随后表现为「关联
// 建立正常，但每次读都超时」。
func ParseSPDU(b []byte) (SPDU, error) {
	if len(b) < 1 {
		return SPDU{}, errors.New("iec61850: empty session message")
	}
	if isDataSPDU(b) {
		if len(b) < 4 {
			return SPDU{}, ErrTruncated
		}
		return SPDU{ID: spduDataTransfer, Payload: b[4:]}, nil
	}
	if len(b) < 2 {
		return SPDU{}, ErrTruncated
	}
	id := b[0]
	n, hdr, err := parseLI(b[1:])
	if err != nil {
		return SPDU{}, err
	}
	end := 1 + hdr + n
	if end > len(b) {
		// 参数区结束在报文边界之内时（多写了字节、或 LI 算错），容忍并把
		// 剩余部分交给后面的参数解析：会话层的 SPDU LI 与参数长度是两套
		// 独立计算的字段，一边对一边错时严格失败会让真正的原因（哪个
		// 参数对不上）被掩盖。
		end = len(b)
	}
	out := SPDU{ID: id}
	if err := parseSessionParams(b[1+hdr:end], &out); err != nil {
		return SPDU{}, err
	}
	return out, nil
}

// isDataSPDU 判断是否是 DT。
//
// 判据是前四字节恰好等于固定头。不能只看 b[0]==0x01：AB 之外的 SPDU 里
// 0x01 也可能出现，但作为**首字节**且后面跟 00 01 00 的只有 DT。
func isDataSPDU(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	return b[0] == spduGiveTokens && b[1] == 0x00 && b[2] == spduDataTransfer && b[3] == 0x00
}

// parseSessionParams 解析 CN/AC/FN/DN/AB 的参数段。
func parseSessionParams(b []byte, out *SPDU) error {
	for i := 0; i < len(b); {
		pgi := b[i]
		if i+1 >= len(b) {
			return ErrTruncated
		}
		// 0xC1/0xC2 用 LI 形式（可能是长形式）；其余参数用单字节长度。
		var n, hdr int
		var err error
		if pgi == pgiUserData || pgi == piExtendedUserData {
			n, hdr, err = parseLI(b[i+1:])
			if err != nil {
				return err
			}
		} else {
			n, hdr = int(b[i+1]), 1
		}
		start := i + 1 + hdr
		if start+n > len(b) {
			return fmt.Errorf("%w: session parameter 0x%02X declares %d bytes", ErrTruncated, pgi, n)
		}
		val := b[start : start+n]
		switch pgi {
		case pgiUserData, piExtendedUserData:
			out.Payload = val
		case pgiCallingSSSel:
			out.CallingSSSel = append([]byte(nil), val...)
		case pgiCalledSSSel:
			out.CalledSSSel = append([]byte(nil), val...)
		case piReasonCode:
			if n >= 1 {
				out.Reason = val[0]
			}
		}
		i = start + n
		continue
	}
	return nil
}

// SessionName 返回 SPDU 标识的可读名，用于错误信息与日志。
func SessionName(id byte) string {
	switch id {
	case spduConnect:
		return "CN(connect)"
	case spduAccept:
		return "AC(accept)"
	case spduDataTransfer:
		return "DT(data)"
	case spduFinish:
		return "FN(finish)"
	case spduDisconnect:
		return "DN(disconnect)"
	case spduAbort:
		return "AB(abort)"
	case spduRefuse:
		return "RF(refuse)"
	case spduNotFinished:
		return "NF(not-finished)"
	}
	return fmt.Sprintf("SPDU(0x%02X)", id)
}
