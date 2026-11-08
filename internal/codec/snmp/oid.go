package snmp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// OID 是对象标识符，按点分十进制书写（`1.3.6.1.2.1.1.1.0`）。
//
// 内部用 []uint32 而不是字符串：GETNEXT 需要按字典序比较，字符串比较会
// 把 `1.3.6.1.2.1.10` 排在 `1.3.6.1.2.1.9` 前面（按字符比 '1' < '9'），
// 于是表的遍历会跳过整段。
type OID []uint32

// ParseOID 解析点分十进制写法。
//
// 规则比「按点切开再转数字」严格得多，每一条都对应一种现场错误：
//   - 至少两个弧（`1.3`）：SNMP 的 OID 必须有对象与分支，单个数字不是 OID；
//   - 首弧 0..2，且首弧小于 2 时次弧 ≤ 39：这是 BER 第一字节的编码前提；
//   - 不带前导零（`1.03`）：前导零是八进制写法的残留，转成 3 还是 03 全靠猜；
//   - 每弧 ≤ 2^32-1：子标识符用 32 位 base-128 编码，超出就无法落线。
func ParseOID(s string) (OID, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil, errors.New("snmp: empty OID")
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("snmp: OID %q needs at least two arcs", s)
	}
	out := make(OID, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("snmp: OID %q has an empty arc", s)
		}
		if len(p) > 1 && p[0] == '0' {
			return nil, fmt.Errorf("snmp: OID %q has a leading zero in arc %q", s, p)
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("snmp: OID %q: arc %q is not a 32-bit unsigned integer", s, p)
		}
		out[i] = uint32(n)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// MustParseOID 只用于常量与测试。
func MustParseOID(s string) OID {
	o, err := ParseOID(s)
	if err != nil {
		panic(err)
	}
	return o
}

// Validate 校验 OID 的编码前提。
func (o OID) Validate() error {
	if len(o) < 2 {
		return fmt.Errorf("snmp: OID %s needs at least two arcs", o)
	}
	if o[0] > 2 {
		return fmt.Errorf("snmp: OID %s: first arc must be 0..2, got %d", o, o[0])
	}
	if o[0] < 2 && o[1] > 39 {
		return fmt.Errorf("snmp: OID %s: second arc must be 0..39 when the first is %d", o, o[0])
	}
	return nil
}

func (o OID) String() string {
	var b strings.Builder
	for i, v := range o {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.FormatUint(uint64(v), 10))
	}
	return b.String()
}

// Compare 按 OID 字典序比较：先逐弧比大小，前缀短的在前。
func (o OID) Compare(other OID) int {
	n := len(o)
	if len(other) < n {
		n = len(other)
	}
	for i := 0; i < n; i++ {
		switch {
		case o[i] < other[i]:
			return -1
		case o[i] > other[i]:
			return 1
		}
	}
	switch {
	case len(o) < len(other):
		return -1
	case len(o) > len(other):
		return 1
	}
	return 0
}

// HasPrefix 判断 other 是否落在 o 这棵子树里（含相等）。
func (o OID) HasPrefix(other OID) bool {
	if len(other) < len(o) {
		return false
	}
	for i := range o {
		if o[i] != other[i] {
			return false
		}
	}
	return true
}

// Clone 返回一份独立副本。
func (o OID) Clone() OID {
	if o == nil {
		return nil
	}
	out := make(OID, len(o))
	copy(out, o)
	return out
}

// encodeOID 把 OID 编成 BER 内容字节（不含 tag/length）。
func encodeOID(o OID) []byte {
	if len(o) < 2 {
		return nil
	}
	out := make([]byte, 0, 2*len(o))
	// 前两个弧压进第一个子标识符。
	out = appendSubID(out, o[0]*40+o[1])
	for _, v := range o[2:] {
		out = appendSubID(out, v)
	}
	return out
}

// appendSubID 追加一个 base-128 子标识符（高位置 1 表示还有后续字节）。
func appendSubID(dst []byte, v uint32) []byte {
	var b [5]byte
	i := len(b) - 1
	b[i] = byte(v & 0x7F)
	for v >>= 7; v > 0; v >>= 7 {
		i--
		b[i] = byte(v&0x7F) | 0x80
	}
	return append(dst, b[i:]...)
}

// decodeOID 解析 BER 内容字节。
func decodeOID(b []byte) (OID, error) {
	if len(b) == 0 {
		return nil, errors.New("snmp: empty OID")
	}
	var out OID
	var cur uint64
	started := false
	for _, x := range b {
		if !started {
			started = true
		}
		cur = cur<<7 | uint64(x&0x7F)
		if cur > 0xFFFFFFFF {
			return nil, errors.New("snmp: OID sub-identifier overflows 32 bits")
		}
		if x&0x80 != 0 {
			continue
		}
		if out == nil {
			// 第一个子标识符承载前两弧。
			first, second := cur/40, cur%40
			if first > 2 {
				first, second = 2, cur-80
			}
			out = append(out, uint32(first), uint32(second))
		} else {
			out = append(out, uint32(cur))
		}
		cur = 0
		started = false
	}
	if started {
		return nil, errors.New("snmp: OID ends with a continuation byte")
	}
	if len(out) < 2 {
		return nil, errors.New("snmp: OID has fewer than two arcs")
	}
	return out, nil
}
