package mbus

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// 本文件是编码侧：把「要上送哪些记录」组装成一条 RSP_UD 的数据域。
//
// 它只服务内置从站（仿真）与测试：真实主站不需要编码遥测数据。放在
// codec 里而不是 sim 里，是为了让**编解码共用同一份长度/量纲规则**——
// 如果仿真器自己算一遍 DIF 与 VIF，测试就只能证明"仿真器与它自己一致"，
// 无法发现主站侧的解析错误。金字节测试正是靠这一点才有意义：
// 同一份记录在编码后再解码，必须回到原值。

// RecordSpec 描述要编码的一条记录。
//
// Width 是 DIF 的低 4 位（长度编码）。留 0 时按 Value 选能装下的最窄
// 整数宽度；显式给出则严格按它编码——测试里需要构造"表声明 32 位但
// 只上送 0.001 精度"这类真实报文，自动选宽会把这种差异抹平。
type RecordSpec struct {
	VIF       byte
	VIFEs     []byte
	Function  byte
	Storage   uint32
	Tariff    uint16
	Subunit   byte
	Width     byte
	Value     int64
	Real      float32
	Text      string
	CustomVIF string
	Raw       []byte
}

// EncodeRecord 把一条 RecordSpec 编成 DIF 起始的字节串。
func EncodeRecord(r RecordSpec) ([]byte, error) {
	width := r.Width
	var data []byte

	switch {
	case r.CustomVIF != "":
		// 自定义 ASCII 量名：VIF=7Ch，紧跟长度与文本。
		if len(r.CustomVIF) > 0x7F {
			return nil, fmt.Errorf("自定义 VIF 名过长（%d 字节）", len(r.CustomVIF))
		}
	case r.Text != "" && width == 0:
		width = 0x0D
	case r.Text != "" && width != 0x0D:
		return nil, fmt.Errorf("文本只支持 DIF=0Dh（变长），得到 %02X", width)
	}

	switch {
	case len(r.Raw) > 0:
		if width == 0 {
			width = 0x0D
		}
		if width == 0x0D {
			// 调用方给的 Raw 已包含 LVAR；长度以 LVAR 为准再校验一次，
			// 免得内置从站上送一条自相矛盾的报文。
			if len(r.Raw) < 1 {
				return nil, fmt.Errorf("变长记录缺少 LVAR")
			}
			n, ok := lvarLen(r.Raw[0])
			if !ok || n != len(r.Raw)-1 {
				return nil, fmt.Errorf("LVAR %02X 与数据长度 %d 不一致", r.Raw[0], len(r.Raw)-1)
			}
			data = append([]byte(nil), r.Raw...)
		} else {
			data = append([]byte(nil), r.Raw...)
		}
	case r.Text != "":
		b := []byte(r.Text)
		if len(b) > 0xBF {
			return nil, fmt.Errorf("文本长度 %d 超过 191 字节", len(b))
		}
		data = append(data, byte(len(b)))
		data = append(data, b...)
	case width == 0x05:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(r.Real))
		data = buf[:]
	case isBCDWidth(width):
		data = encodeBCD(r.Value, bcdWidth(width))
	default:
		if width == 0 {
			width = autoIntWidth(r.Value)
		}
		n := intWidth(width)
		if n == 0 {
			return nil, fmt.Errorf("DIF 长度编码 %02X 不能用于整数", width)
		}
		data = encodeIntLE(r.Value, n)
	}

	nDIFE := neededDIFEs(r.Storage, r.Tariff, r.Subunit)
	dif := width | (r.Function&0x03)<<4 | byte((r.Storage&0x01)<<6)
	if nDIFE > 0 {
		dif |= 0x80
	}
	out := make([]byte, 0, 2+nDIFE+len(r.VIFEs)+len(data))
	out = append(out, dif)
	for i := 0; i < nDIFE; i++ {
		b := byte((r.Storage>>uint(1+4*i))&0x0F) |
			byte((r.Tariff>>uint(2*i))&0x03)<<4 |
			byte((r.Subunit>>uint(i))&0x01)<<6
		if i < nDIFE-1 {
			b |= 0x80
		}
		out = append(out, b)
	}

	vif := r.VIF
	if r.CustomVIF != "" {
		vif = 0x7C
	}
	if len(r.VIFEs) > 0 {
		vif |= 0x80
	}
	out = append(out, vif)
	if r.CustomVIF != "" {
		out = append(out, byte(len(r.CustomVIF)))
		out = append(out, r.CustomVIF...)
	}
	for i, vife := range r.VIFEs {
		b := vife
		if i < len(r.VIFEs)-1 {
			b |= 0x80
		}
		out = append(out, b)
	}
	out = append(out, data...)
	return out, nil
}

// BuildVariableData 组装 CI=72h 的完整数据域（12 字节固定头 + 记录）。
func BuildVariableData(h Header, specs []RecordSpec) ([]byte, error) {
	head, err := encodeHeader(h)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 12+16*len(specs))
	out = append(out, head...)
	for i, s := range specs {
		b, err := EncodeRecord(s)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条记录：%w", i, err)
		}
		out = append(out, b...)
	}
	// 长帧的 L 字段是单字节，且要容纳 C/A/CI 三个字节：数据域最多 252。
	if len(out) > 249 {
		return nil, fmt.Errorf("数据域 %d 字节超过单帧上限 249（L 为单字节）", len(out))
	}
	return out, nil
}

// BuildFixedData 组装 CI=73h 的 16 字节固定结构。
//
// types 是计数器单位编码（见 fixedUnit），bcd 决定计数器的编码方式，
// stored 表示这是"上次读数时"的历史值。四个参数都是协议里真实存在的
// 独立开关，压成一个结构体反而看不出它们各自影响哪个字节。
func BuildFixedData(h Header, bcd, stored bool, types [2]byte, vals [2]int64) ([]byte, error) {
	head, err := encodeHeader(h)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	copy(out[0:4], head[0:4])
	out[4] = h.AccessNo
	out[5] = h.Status
	if bcd {
		out[5] &^= 0x80
	} else {
		out[5] |= 0x80
	}
	if stored {
		out[5] |= 0x40
	}
	out[6], out[7] = types[0], types[1]
	for i := 0; i < 2; i++ {
		var v []byte
		if bcd {
			v = encodeBCD(vals[i], 4)
		} else {
			v = encodeIntLE(vals[i], 4)
		}
		copy(out[8+4*i:12+4*i], v)
	}
	return out, nil
}

func encodeHeader(h Header) ([]byte, error) {
	id := h.ID
	if id == 0 && h.IDText != "" {
		v, err := BCDID(h.IDText)
		if err != nil {
			return nil, err
		}
		id = v
	}
	man := h.Manufacturer
	if man == 0 && h.ManufacturerID != "" {
		v, err := DecodeManufacturerCode(h.ManufacturerID)
		if err != nil {
			return nil, err
		}
		man = v
	}
	out := make([]byte, 12)
	binary.LittleEndian.PutUint32(out[0:4], id)
	binary.LittleEndian.PutUint16(out[4:6], man)
	out[6] = h.Version
	out[7] = h.Medium
	out[8] = h.AccessNo
	out[9] = h.Status
	binary.LittleEndian.PutUint16(out[10:12], h.Signature)
	return out, nil
}

// BCDID 把 8 位十进制表号编成 4 字节 BCD（低位在前）。
func BCDID(text string) (uint32, error) {
	text = strings.TrimSpace(text)
	if len(text) > 8 {
		return 0, fmt.Errorf("表号 %q 超过 8 位", text)
	}
	for len(text) < 8 {
		text = "0" + text
	}
	var b [4]byte
	for i := 0; i < 4; i++ {
		hi := text[6-2*i] - '0'
		lo := text[7-2*i] - '0'
		if hi > 9 || lo > 9 {
			return 0, fmt.Errorf("表号 %q 含非十进制字符", text)
		}
		b[i] = hi<<4 | lo
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func isBCDWidth(w byte) bool {
	return (w >= 0x09 && w <= 0x0C) || w == 0x0E
}

func bcdWidth(w byte) int {
	switch w {
	case 0x09:
		return 1
	case 0x0A:
		return 2
	case 0x0B:
		return 3
	case 0x0C:
		return 4
	case 0x0E:
		return 6
	}
	return 0
}

func encodeBCD(v int64, n int) []byte {
	neg := v < 0
	if neg {
		v = -v
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		lo := byte(v % 10)
		v /= 10
		hi := byte(v % 10)
		v /= 10
		out[i] = hi<<4 | lo
	}
	if neg {
		out[n-1] |= 0xF0
	}
	return out
}

func intWidth(w byte) int {
	switch w {
	case 0x01:
		return 1
	case 0x02:
		return 2
	case 0x03:
		return 3
	case 0x04, 0x05:
		return 4
	case 0x06:
		return 6
	case 0x07:
		return 8
	}
	return 0
}

func autoIntWidth(v int64) byte {
	switch {
	case v >= math.MinInt8 && v <= math.MaxInt8:
		return 0x01
	case v >= math.MinInt16 && v <= math.MaxInt16:
		return 0x02
	case v >= -(1<<23) && v <= (1<<23)-1:
		return 0x03
	case v >= math.MinInt32 && v <= math.MaxInt32:
		return 0x04
	}
	return 0x07
}

func encodeIntLE(v int64, n int) []byte {
	out := make([]byte, n)
	u := uint64(v)
	for i := 0; i < n; i++ {
		out[i] = byte(u >> uint(8*i))
	}
	return out
}

func neededDIFEs(storage uint32, tariff uint16, subunit byte) int {
	n := 0
	for {
		if storage>>uint(1+4*n) == 0 && tariff>>uint(2*n) == 0 && subunit>>uint(n) == 0 {
			return n
		}
		n++
		if n > 10 {
			return 10
		}
	}
}
