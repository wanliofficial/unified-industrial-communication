// Package value 在原始字节与平台统一值之间转换。
//
// 这是协议无关的一层：所有工业协议最终都是「一段字节 + 一个类型 + 一种
// 字序」。把它抽出来不是为了复用几行代码，而是为了**保证语义唯一**——
// 如果 Modbus 与 S7 各写一份解码，某天一个修了负数符号扩展而另一个没修，
// 两个协议对同一个 0xFFFF 就会给出不同的值，而且极难发现。
//
// 约定：
//   - 数值一律按大端解释（网络序，也是 S7 的原生序）；
//   - 32/64 位值的 16 位字之间的排列由 ByteOrder 决定（Modbus 需要，
//     S7 只用 ABCD）；
//   - 字符串按 NUL 截断；bytes 原样返回。
package value

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"uic/internal/model"
)

// Reorder 按字节序重排 16 位字的顺序与字内字节顺序。
//
// ABCD 原样返回；CDAB 交换相邻字；BADC 交换字内字节；DCBA 整体倒序。
// 奇数长度时只处理完整的字对，尾字节保持原位——协议层不会产生奇数长度
// 的数值，这里只是不 panic。
func Reorder(b []byte, order model.ByteOrder) []byte {
	out := append([]byte(nil), b...)
	switch order {
	case "", model.OrderABCD:
		return out
	case model.OrderCDAB:
		for i := 0; i+3 < len(out); i += 4 {
			out[i], out[i+1], out[i+2], out[i+3] = out[i+2], out[i+3], out[i], out[i+1]
		}
	case model.OrderDCBA:
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	case model.OrderBADC:
		for i := 0; i+1 < len(out); i += 2 {
			out[i], out[i+1] = out[i+1], out[i]
		}
	}
	return out
}

// Decode 把原始字节解码为模型值。b 可以比类型宽度长（取前 N 字节），
// 但不能短——短了说明地址长度配错，必须报错而不是补零。
func Decode(b []byte, t model.DataType, order model.ByteOrder) (model.Value, error) {
	switch t {
	case model.TypeString:
		return model.TextValue(t, strings.TrimRight(string(b), "\x00")), nil
	case model.TypeBytes:
		return model.BytesValue(b), nil
	}
	if len(b) < t.Bytes() {
		return model.Value{}, fmt.Errorf("value: need %d bytes for %s, got %d", t.Bytes(), t, len(b))
	}
	raw := Reorder(b[:t.Bytes()], order)
	switch t {
	case model.TypeInt16:
		return model.NumValue(t, float64(int16(binary.BigEndian.Uint16(raw)))), nil
	case model.TypeUint16:
		return model.NumValue(t, float64(binary.BigEndian.Uint16(raw))), nil
	case model.TypeInt32:
		return model.NumValue(t, float64(int32(binary.BigEndian.Uint32(raw)))), nil
	case model.TypeUint32:
		return model.NumValue(t, float64(binary.BigEndian.Uint32(raw))), nil
	case model.TypeInt64:
		return model.NumValue(t, float64(int64(binary.BigEndian.Uint64(raw)))), nil
	case model.TypeUint64:
		return model.NumValue(t, float64(binary.BigEndian.Uint64(raw))), nil
	case model.TypeFloat32:
		return model.NumValue(t, float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))), nil
	case model.TypeFloat64:
		return model.NumValue(t, math.Float64frombits(binary.BigEndian.Uint64(raw))), nil
	case model.TypeBCD16:
		v, err := decodeBCD(raw)
		return model.NumValue(t, v), err
	case model.TypeBCD32:
		v, err := decodeBCD(raw)
		return model.NumValue(t, v), err
	}
	return model.Value{}, fmt.Errorf("value: unsupported dataType %q", t)
}

// ErrRange 表示数值无法以目标类型精确表示：整数越界、带小数、NaN/Inf、
// float32 溢出成 ±Inf 或下溢成 0、BCD 溢出或为负。
//
// 这不是「编码失败」，而是「编码会静默改变值」——70000 写 int16 会变成
// 4464、1e300 写 float32 会变成 +Inf、1.5 写 int16 会丢成 1。写入报「成功」
// 但设备上的值已不是调用方给的值，比写入失败更难发现（§3.4 规则 3）。
// 上层用 errors.Is(err, value.ErrRange) 把它归为点/配置错误（Addr/Unsupported），
// 不计入熔断、不重试。
var ErrRange = errors.New("value: number not representable in target type")

// checkRange 拒绝一切「编码后会变值」的输入。
//
// 放在 Encode 内部而不是各驱动里，是因为这是唯一的语义收口点：只要有一个
// 驱动忘了调自己的 checkRange，那条写入路径就会静默损坏数据。历史上
// Modbus 与 Mewtocol 就漏掉了，而同一个 checkRange 在其它五个驱动里被
// 复制了五份——复制得越多，越会有一个驱动漏掉。
func checkRange(t model.DataType, v float64) error {
	switch t {
	case model.TypeInt16:
		if v != math.Trunc(v) || v < math.MinInt16 || v > math.MaxInt16 {
			return fmt.Errorf("%w: %v 超出 int16 范围或不是整数", ErrRange, v)
		}
	case model.TypeUint16:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint16 {
			return fmt.Errorf("%w: %v 超出 uint16 范围或不是整数", ErrRange, v)
		}
	case model.TypeInt32:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return fmt.Errorf("%w: %v 超出 int32 范围或不是整数", ErrRange, v)
		}
	case model.TypeUint32:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint32 {
			return fmt.Errorf("%w: %v 超出 uint32 范围或不是整数", ErrRange, v)
		}
	case model.TypeInt64:
		// float64 无法精确表示 int64 全范围：2^63 以上一律拒绝，边界值
		// 本身（-2^63）可表示，原样放行。
		if v != math.Trunc(v) || v < math.MinInt64 || v >= 9223372036854775808.0 {
			return fmt.Errorf("%w: %v 超出 int64 范围或不是整数", ErrRange, v)
		}
	case model.TypeUint64:
		if v != math.Trunc(v) || v < 0 || v >= 18446744073709551616.0 {
			return fmt.Errorf("%w: %v 超出 uint64 范围或不是整数", ErrRange, v)
		}
	case model.TypeFloat32:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: %v 不是合法的 float32 值", ErrRange, v)
		}
		if v != 0 {
			f := float32(v)
			if math.IsInf(float64(f), 0) {
				return fmt.Errorf("%w: %v 超出 float32 可表示范围（会溢出成 Inf）", ErrRange, v)
			}
			if f == 0 {
				return fmt.Errorf("%w: %v 低于 float32 最小可表示值（会静默变成 0）", ErrRange, v)
			}
		}
	case model.TypeFloat64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: %v 不是合法的 float64 值", ErrRange, v)
		}
	case model.TypeBCD16, model.TypeBCD32:
		// BCD 每字节两位十进制，超出位数或为负都会丢位（encodeBCD 逐位取模）。
		max := 9999.0
		if t == model.TypeBCD32 {
			max = 99999999.0
		}
		if v != math.Trunc(v) || v < 0 || v > max {
			return fmt.Errorf("%w: %v 超出 %s 的 BCD 范围或不是非负整数", ErrRange, v, t)
		}
	}
	return nil
}

// Encode 把模型值编码为原始字节（大端 + 字序）。
//
// 字符串与 bytes 不在此处编码：它们的宽度由目标地址决定，调用方必须先
// 按点位长度补齐或截断。把「长度」这件事留在这里会逼出一个永远猜不对的
// 默认值。
func Encode(v model.Value, t model.DataType, order model.ByteOrder) ([]byte, error) {
	n := t.Bytes()
	if n <= 0 {
		return nil, fmt.Errorf("value: unsupported write type %q", t)
	}
	if err := checkRange(t, v.Num); err != nil {
		return nil, err
	}
	raw := make([]byte, n)
	switch t {
	case model.TypeInt16:
		binary.BigEndian.PutUint16(raw, uint16(int16(v.Num)))
	case model.TypeUint16:
		binary.BigEndian.PutUint16(raw, uint16(v.Num))
	case model.TypeInt32:
		binary.BigEndian.PutUint32(raw, uint32(int32(v.Num)))
	case model.TypeUint32:
		binary.BigEndian.PutUint32(raw, uint32(v.Num))
	case model.TypeInt64:
		binary.BigEndian.PutUint64(raw, uint64(int64(v.Num)))
	case model.TypeUint64:
		binary.BigEndian.PutUint64(raw, uint64(v.Num))
	case model.TypeFloat32:
		binary.BigEndian.PutUint32(raw, math.Float32bits(float32(v.Num)))
	case model.TypeFloat64:
		binary.BigEndian.PutUint64(raw, math.Float64bits(v.Num))
	case model.TypeBCD16, model.TypeBCD32:
		encodeBCD(raw, v.Num)
	default:
		return nil, fmt.Errorf("value: unsupported write type %q", t)
	}
	return Reorder(raw, order), nil
}

func decodeBCD(b []byte) (float64, error) {
	var out float64
	for _, x := range b {
		hi, lo := x>>4, x&0x0F
		if hi > 9 || lo > 9 {
			return 0, errors.New("value: invalid BCD digit")
		}
		out = out*100 + float64(hi)*10 + float64(lo)
	}
	return out, nil
}

func encodeBCD(b []byte, v float64) {
	n := int64(math.Round(v))
	for i := len(b) - 1; i >= 0; i-- {
		lo := byte(n % 10)
		n /= 10
		hi := byte(n % 10)
		n /= 10
		b[i] = hi<<4 | lo
	}
}
