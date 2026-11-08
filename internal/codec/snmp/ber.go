package snmp

import (
	"errors"
	"fmt"
)

// BER 基础类型标签。SNMP 只用到其中很少一部分，且**只允许单字节标签**：
// 完整的 BER 支持多字节标签（0x1F 起），但 SNMP 的对象语法里没有这种类型，
// 支持它只会增加一个「解析到什么算什么」的入口。
const (
	tagInteger     = 0x02
	tagOctetString = 0x04
	tagNull        = 0x05
	tagOID         = 0x06
	tagSequence    = 0x30
)

// errShort 表示报文在中途断掉；调用方据此区分「格式错」与「没收全」。
var errShort = errors.New("snmp: truncated BER element")

// reader 是只前进的 BER 读取器。
type reader struct {
	buf []byte
	off int
}

// tlv 读取一个 tag-length-value。
func (r *reader) tlv() (byte, []byte, error) {
	if r.off >= len(r.buf) {
		return 0, nil, errShort
	}
	tag := r.buf[r.off]
	r.off++
	if tag&0x1F == 0x1F {
		return 0, nil, fmt.Errorf("snmp: multi-byte tag 0x%02X is not valid here", tag)
	}
	n, err := r.length()
	if err != nil {
		return 0, nil, err
	}
	if n > len(r.buf)-r.off {
		return 0, nil, fmt.Errorf("%w: need %d bytes, have %d", errShort, n, len(r.buf)-r.off)
	}
	content := r.buf[r.off : r.off+n]
	r.off += n
	return tag, content, nil
}

// length 读取 definite 形式的长度。
func (r *reader) length() (int, error) {
	if r.off >= len(r.buf) {
		return 0, errShort
	}
	b := r.buf[r.off]
	r.off++
	if b < 0x80 {
		return int(b), nil
	}
	if b == 0x80 {
		// indefinite length：SNMP 不允许，且接收方无法知道何时结束。
		return 0, errors.New("snmp: indefinite BER length is not allowed")
	}
	n := int(b & 0x7F)
	if n > 4 {
		// 超过 4 字节的长度字段一定是畸形报文：UDP 载荷本身也只有 64KB。
		return 0, fmt.Errorf("snmp: BER length uses %d bytes", n)
	}
	if len(r.buf)-r.off < n {
		return 0, errShort
	}
	v := 0
	for i := 0; i < n; i++ {
		v = v<<8 | int(r.buf[r.off+i])
	}
	r.off += n
	if v < 0x80 {
		// 非最短形式的长度：协议要求最短，接受它等于纵容两套编码器。
		return 0, fmt.Errorf("snmp: non-minimal BER length (0x%02X for %d)", b, v)
	}
	return v, nil
}

// appendTLV 追加一个 tag-length-value。
func appendTLV(dst []byte, tag byte, content []byte) []byte {
	dst = append(dst, tag)
	dst = appendLength(dst, len(content))
	return append(dst, content...)
}

func appendLength(dst []byte, n int) []byte {
	if n < 0x80 {
		return append(dst, byte(n))
	}
	var b [8]byte
	i := len(b)
	for v := n; v > 0; v >>= 8 {
		i--
		b[i] = byte(v)
	}
	dst = append(dst, 0x80|byte(len(b)-i))
	return append(dst, b[i:]...)
}

// encodeInt 用最短补码编码一个 64 位有符号整数。
func encodeInt(v int64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	i := 0
	// 去掉「符号位相同」的前导字节，但至少留一个。
	for i < 7 {
		next := b[i+1]
		if b[i] == 0x00 && next&0x80 == 0 {
			i++
			continue
		}
		if b[i] == 0xFF && next&0x80 != 0 {
			i++
			continue
		}
		break
	}
	return append([]byte(nil), b[i:]...)
}

// encodeUnsigned 把无符号量编码成 INTEGER 的补码形式。
//
// 最高位为 1 时补一个前导 0x00：否则接收方会把它读成负数。Counter32 的
// 2^31 与 TimeTicks 的 2^31 都会走到这条分支，少了这个字节，设备侧看到的
// 是一个负数。
func encodeUnsigned(v uint64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	i := 0
	for i < 7 && b[i] == 0x00 {
		i++
	}
	out := append([]byte(nil), b[i:]...)
	if out[0]&0x80 != 0 {
		out = append([]byte{0x00}, out...)
	}
	return out
}

// decodeInt 解析补码整数（1..8 字节）。
func decodeInt(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, errors.New("snmp: empty INTEGER")
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("snmp: INTEGER is %d bytes (max 8)", len(b))
	}
	var v int64
	if b[0]&0x80 != 0 {
		v = -1
	}
	for _, x := range b {
		v = v<<8 | int64(x)
	}
	return v, nil
}

// decodeUnsigned 解析无符号量（1..9 字节，允许一个前导 0x00）。
func decodeUnsigned(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, errors.New("snmp: empty unsigned value")
	}
	if len(b) > 9 {
		return 0, fmt.Errorf("snmp: unsigned value is %d bytes (max 9)", len(b))
	}
	if len(b) == 9 && b[0] != 0x00 {
		return 0, errors.New("snmp: 9-byte unsigned value without leading zero")
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v, nil
}
