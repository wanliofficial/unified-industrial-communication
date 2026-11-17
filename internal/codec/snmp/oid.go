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
//   - 每弧 ≤ 2^32-1：子标识符用 32 位 base-128 编码，超出就无法落线；
//   - 不超过 128 个弧：RFC 2578 §3.5 的 SMIv2 上限（校验在 Validate 里）。
//
// 注意「每弧 ≤ 2^32-1」限制的是弧，不是子标识符：RFC 2578 §3.5 同样要求
// 每个子标识符 ≤ 2^32-1，但首个子标识符是前两弧的合成值 40*arc1+arc2，
// arc1=2 时为 2^32+79，本来就合法地超过 32 位，靠 5 字节 base-128 落线，
// 见 encodeOID / decodeOID 的两档上限。
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
//
// 除前两弧的规则外，还卡住 RFC 2578 §3.5 的「一个 OBJECT IDENTIFIER 最多
// 128 个子标识符」。这条上限不是理论洁癖：超长 OID 只会出现在「把整张表
// 的一行拼错」或自动生成器出 bug 的时候，线上发出去既浪费报文预算，也
// 只会换来代理的 genErr / tooBig——在本地就拒绝，错误信息才能指到那个点。
func (o OID) Validate() error {
	if len(o) < 2 {
		return fmt.Errorf("snmp: OID %s needs at least two arcs", o)
	}
	if len(o) > 128 {
		return fmt.Errorf("snmp: OID has %d arcs, RFC 2578 allows at most 128", len(o))
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
	// 前两个弧压进第一个子标识符：值 = 40*arc1 + arc2。
	//
	// 这里**必须**用 uint64 计算：RFC 1155 规定 arc1=2 时 arc2 没有上界，
	// 于是首子标识符可达 40*2 + (2^32-1) = 2^32+79，**超出 uint32**。若按
	// uint32 计算（o[0]*40+o[1]），这个值会回绕成一个小数——例如
	// {2, 4294967295} 会回绕成 79，与 {1,39} 撞车，编出来的报文解码回来是
	// 一台完全不同的设备。静默回绕属于最坏的一类缺陷：不报错、不截断，只是
	// 悄悄指向别的对象。
	//
	// 用 appendSubID64 承载（最多 5 字节），普通情形（arc1<2 时 arc2<=39）
	// 仍是单字节，编码结果不变。
	out = appendSubID64(out, uint64(o[0])*40+uint64(o[1]))
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

// appendSubID64 追加一个可能超过 32 位的 base-128 子标识符。
//
// 只有首子标识符（40*arc1+arc2）会走到这里，且上限是 2^32+79，5 字节足够；
// 留 10 字节只是为了让「以后别再有人改回 uint32」这件事有个显式的余量。
func appendSubID64(dst []byte, v uint64) []byte {
	var b [10]byte
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
	// 上限分两档，必须与编码侧严格对称：
	//   首个子标识符 = 40*arc1+arc2，arc1=2 时 arc2 无上界
	//                  → 最大 40*2+(2^32-1) = 2^32+79；
	//   其余子标识符是 32 位 → 最大 2^32-1。
	//
	// 若这里统一按 32 位限死，编码侧刚修好的 {2, 大弧} 会在解回时被误判为
	// 溢出——那是「写得出、读不回」的单向不兼容。
	const maxFirstSubID = uint64(0xFFFFFFFF) + 80 // 2^32+79
	limit := maxFirstSubID
	for _, x := range b {
		started = true
		// 逐字节检查，而不是等凑齐再查：一旦越界立刻返回，cur 就不会累积
		// 到能溢出 uint64 的量级（否则超长续字节会让 cur 静默回绕，又变成
		// 一次静默损坏）。
		cur = cur<<7 | uint64(x&0x7F)
		if cur > limit {
			if out == nil {
				return nil, fmt.Errorf("snmp: OID first sub-identifier exceeds 40*2+2^32-1 (%d)", cur)
			}
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
			limit = 0xFFFFFFFF
		} else {
			// RFC 2578 §3.5：最多 128 个子标识符。线上超过这个数只可能是
			// 生成器出错或报文被拼接，继续解析下去没有意义。
			if len(out) >= 128 {
				return nil, errors.New("snmp: OID has more than 128 arcs")
			}
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
