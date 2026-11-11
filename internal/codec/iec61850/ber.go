package iec61850

import (
	"errors"
	"fmt"
	"math"
)

// 本文件是 IEC 61850 用的最小 BER 编解码器。
//
// 为什么自己写而不是复用 internal/codec/snmp 的那一份：那一份只覆盖 SNMP
// 需要的标签（INTEGER / OCTET STRING / NULL / OID / SEQUENCE），而这里要用
// 的是**上下文标签**（context class + constructed 位）与它们的不同编码规则
// ——ACSE 与 MMS 里同一个语义字段在请求侧是 EXPLICIT（外面套一层 0xA0 之类
// 的构造标签），在响应侧可能是 IMPLICIT（标签直接替换掉原类型标签）。把
// 这种差别塞进通用 TLV 会把「这一层到底套没套」藏起来，而它恰好是本协议
// 最容易写错、也最难从错误里反推的地方。
//
// 单标签解析仍然只做一件事：给一个 tag 与一个长度，取出内容。构造标签的
// 嵌套由各层自己解释——那边的语义（哪一段是 CHOICE、哪一段是 SEQUENCE OF）
// 只有各层知道。

// BER 标签的类别位。
const (
	tagClassUniversal   = 0x00
	tagClassApplication = 0x40
	tagClassContext     = 0x80
	tagClassPrivate     = 0xC0
)

// tagConstructed 是构造位：置位表示内容是若干 TLV 的串联。
const tagConstructed = 0x20

// 通用类的常用标签。
const (
	tagBool         = 0x01
	tagInt          = 0x02
	tagBitString    = 0x03
	tagOctetString  = 0x04
	tagNull         = 0x05
	tagOID          = 0x06
	tagSequence     = 0x30
	tagSet          = 0x31
	tagVisibleChars = 0x1a
)

// maxBERLength 是长度字段的上限。
//
// 取 0xFFFFFF（三字节长形式）：IEC 61850 的 MMS PDU 在传输层被 COTP 分成
// 128..8192 字节的 TPDU，单个 PDU 不会到 16 MB；允许更长的长度只会让一个
// 「前面某个字节写错」的报文被当成合法报文读完再报一句莫名其妙的解码失败。
const maxBERLength = 0xFFFFFF

// ErrTruncated 表示缓冲区不足以容纳声明的长度。
var ErrTruncated = errors.New("iec61850: truncated ber element")

// errIndefinite 表示遇到不定长（0x80）长度形式。
//
// BER 允许不定长（以两个 0x00 结束），但 IEC 61850 的规定里不使用它，而
// 支持它要求「找结束标记」的扫描，那在长度错误时会把整条流读歪。明确拒绝
// 比猜一个边界安全。
var errIndefinite = errors.New("iec61850: indefinite ber length is not used by this protocol")

// element 是一个解析出来的 TLV。
type element struct {
	tag  byte
	data []byte
	// rest 是同一个缓冲区里本元素之后的字节。用它做「顺序读若干字段」比
	// 维护一个游标更不容易漏掉推进。
	rest []byte
}

// parseElement 从 b 的开头取出一个 TLV。
func parseElement(b []byte) (element, error) {
	if len(b) < 2 {
		return element{}, ErrTruncated
	}
	tag := b[0]
	n, hdr, err := parseLength(b[1:])
	if err != nil {
		return element{}, err
	}
	end := 1 + hdr + n
	if end > len(b) {
		return element{}, fmt.Errorf("%w: tag 0x%02X declares %d bytes, have %d",
			ErrTruncated, tag, n, len(b)-1-hdr)
	}
	return element{tag: tag, data: b[1+hdr : end], rest: b[end:]}, nil
}

// parseLength 解析长度字段，返回长度值与它占用的字节数。
//
// 短形式（<0x80）与「0xFF + 1~3 字节」长形式都接受。0x80（不定长）与
// 0x81~0x84（BER 长形式）一律拒绝：IEC 61850 不使用它们，而接受它们会
// 让一个前面某个字节写错的报文被当成合法报文读完再报一句无关的解码失败。
func parseLength(b []byte) (n, hdr int, err error) {
	if len(b) == 0 {
		return 0, 0, ErrTruncated
	}
	first := b[0]
	if first < 0x80 {
		// 短形式：内容长度就是这一个字节。
		return int(first), 1, nil
	}
	if first == 0xFF {
		count := 2 // 长形式固定两字节：0xFF <hi> <lo>。
		if len(b) < 1+count {
			return 0, 0, ErrTruncated
		}
		v := int(b[1])<<8 | int(b[2])
		if v > maxBERLength {
			return 0, 0, fmt.Errorf("iec61850: ber length %d exceeds the %d limit", v, maxBERLength)
		}
		return v, 1 + count, nil
	}
	if first == 0x80 {
		return 0, 0, errIndefinite
	}
	return 0, 0, fmt.Errorf("iec61850: unsupported ber length form 0x%02X", first)
}

// appendTLV 追加一个 TLV。
func appendTLV(dst []byte, tag byte, content []byte) []byte {
	dst = append(dst, tag)
	dst = appendLength(dst, len(content))
	return append(dst, content...)
}

// appendLength 追加 BER 长度字段。
//
// 短形式与「0xFF + 1~3 字节」长形式：与会话层的 LI 是同一套编码。写错宽
// 度会让对端把后续字节误读为长度的一部分。
func appendLength(dst []byte, n int) []byte {
	if n < 0x80 {
		return append(dst, byte(n))
	}
	if n <= 0xFF {
		return append(dst, 0xFF, byte(n))
	}
	if n <= 0xFFFF {
		return append(dst, 0xFF, byte(n>>8), byte(n))
	}
	return append(dst, 0xFF, byte(n>>16), byte(n>>8), byte(n))
}

// lengthSize 返回 appendLength 会写多少字节。构造外层长度时需要先知道它。
func lengthSize(n int) int {
	switch {
	case n < 0x80:
		return 1
	case n <= 0xFF:
		return 2
	case n <= 0xFFFF:
		return 3
	}
	return 4
}

// tlvSize 返回一个 TLV 的总字节数。
func tlvSize(n int) int { return 1 + lengthSize(n) + n }

// ---------------------------------------------------------------------------
// 整数
// ---------------------------------------------------------------------------

// encodeInt 把有符号整数编成 BER INTEGER 的内容字节（最短补码表示）。
//
// 「最短」不是可选项：0 编成 00 00，或 128 编成 00 80，都会让严格的对端
// 判定为非最短编码而拒收整条 PDU。
func encodeInt(v int64) []byte {
	var raw [8]byte
	for i := 7; i >= 0; i-- {
		raw[i] = byte(v)
		v >>= 8
	}
	start := 0
	for start < 7 {
		cur, next := raw[start], raw[start+1]
		redundant := (cur == 0x00 && next&0x80 == 0) || (cur == 0xFF && next&0x80 != 0)
		if !redundant {
			break
		}
		start++
	}
	return append([]byte(nil), raw[start:]...)
}

// encodeUnsigned 把无符号整数编成内容字节（最短表示，必要时补前导 0x00）。
//
// BER 的 INTEGER 是有符号的，最高位为 1 时必须补一个 0x00，否则对端会把它
// 读成负数——一个 invokeID 大于 127 的请求会因此永远等不到响应。
func encodeUnsigned(v uint64) []byte {
	var raw [8]byte
	for i := 7; i >= 0; i-- {
		raw[i] = byte(v)
		v >>= 8
	}
	start := 0
	for start < 7 && raw[start] == 0x00 {
		start++
	}
	out := append([]byte(nil), raw[start:]...)
	if out[0]&0x80 != 0 {
		out = append([]byte{0x00}, out...)
	}
	return out
}

// decodeInt 解析有符号整数的内容字节。
func decodeInt(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, errors.New("iec61850: empty integer")
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("iec61850: integer of %d octets is out of range", len(b))
	}
	v := int64(int8(b[0]))
	for _, c := range b[1:] {
		v = v<<8 | int64(c)
	}
	return v, nil
}

// decodeUnsigned 解析无符号整数的内容字节。
//
// 接受前导 0x00（对端把「最高位为 1 的无符号数」按 BER 规则补了零），但
// 拒绝前导 0xFF：那样写出来的是负数，按无符号读会把 -1 读成 2^64-1。
func decodeUnsigned(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, errors.New("iec61850: empty unsigned")
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("iec61850: unsigned of %d octets is out of range", len(b))
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 位串与浮点
// ---------------------------------------------------------------------------

// encodeBitString 编码 BIT STRING 的内容字节：首字节是「末尾有几个填充位」。
func encodeBitString(bits []byte, padding int) []byte {
	out := make([]byte, 0, 1+len(bits))
	out = append(out, byte(padding))
	return append(out, bits...)
}

// decodeBitString 解析 BIT STRING 的内容字节。
func decodeBitString(b []byte) (padding int, bits []byte, err error) {
	if len(b) == 0 {
		return 0, nil, errors.New("iec61850: empty bit string")
	}
	padding = int(b[0])
	if padding > 7 {
		return 0, nil, fmt.Errorf("iec61850: bit string padding %d is out of range", padding)
	}
	if len(b) == 1 && padding != 0 {
		return 0, nil, errors.New("iec61850: empty bit string declares padding")
	}
	return padding, b[1:], nil
}

// encodeFloat 编码 MMS 的浮点数内容：首字节是指数宽度，随后是大端 IEEE 754。
//
// 这个前导字节是 MMS 特有的：它让同一个 tag 承载 32 位与 64 位两种浮点。
// 32 位写 8，64 位写 11。
func encodeFloat(v float64, width int) ([]byte, error) {
	switch width {
	case 32:
		bits := math.Float32bits(float32(v))
		return []byte{8, byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}, nil
	case 64:
		bits := math.Float64bits(v)
		out := make([]byte, 9)
		out[0] = 11
		for i := 0; i < 8; i++ {
			out[1+i] = byte(bits >> (56 - 8*i))
		}
		return out, nil
	}
	return nil, fmt.Errorf("iec61850: unsupported float width %d", width)
}

// decodeFloat 解析 MMS 浮点数内容，返回值与位宽。
func decodeFloat(b []byte) (float64, int, error) {
	if len(b) == 0 {
		return 0, 0, errors.New("iec61850: empty floating point")
	}
	exp := int(b[0])
	body := b[1:]
	switch len(body) {
	case 4:
		var bits uint32
		for _, c := range body {
			bits = bits<<8 | uint32(c)
		}
		return float64(math.Float32frombits(bits)), 32, nil
	case 8:
		var bits uint64
		for _, c := range body {
			bits = bits<<8 | uint64(c)
		}
		return math.Float64frombits(bits), 64, nil
	}
	return 0, 0, fmt.Errorf("iec61850: floating point of %d octets with exponent width %d is unsupported",
		len(body), exp)
}

// ---------------------------------------------------------------------------
// 上下文标签
// ---------------------------------------------------------------------------

// contextTag 返回一个上下文类标签。
//
// constructed 决定是否置构造位：IMPLICIT 的原始类型必须是 primitive，
// EXPLICIT 的包装必须是 constructed——两者混淆会让对端把内容当成一个 TLV
// 继续解析，报出的错误离真正的原因很远。
func contextTag(num byte, constructed bool) byte {
	t := byte(tagClassContext) | (num & 0x1F)
	if constructed {
		t |= tagConstructed
	}
	return t
}

// isContext 判断标签是否属于上下文类，并返回编号与构造位。
func isContext(tag byte) (num byte, constructed bool, ok bool) {
	if tag&tagClassContext != tagClassContext {
		return 0, false, false
	}
	return tag & 0x1F, tag&tagConstructed != 0, true
}

// expectContext 取出一个指定编号的上下文标签，并返回它的内容。
func expectContext(e element, num byte, constructed bool) ([]byte, error) {
	n, c, ok := isContext(e.tag)
	if !ok || n != num || c != constructed {
		want := contextTag(num, constructed)
		return nil, fmt.Errorf("iec61850: expected context tag 0x%02X, got 0x%02X", want, e.tag)
	}
	return e.data, nil
}
