package opcua

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"uic/internal/model"
)

// 这一层解决的是「协议类型 ⇄ 平台类型」的映射。放在 codec 而不是 driver，
// 是因为 sim（服务端）也要用同一套转换：如果主站按一套规则写、从站按另
// 一套规则读，自测就永远发现不了类型错误——自测夹具必须是同一份实现。

// VariantType 返回平台类型对应的 OPC UA 内置类型编号。
// BCD 没有对应的内置类型：BCD 是 Modbus/串口的编码方式，不是数据类型，
// 在 OPC UA 侧只能先解码成整数再上报，因此这里明确报错而不是硬映射。
func VariantType(t model.DataType) (byte, error) {
	switch t {
	case model.TypeBool:
		return TypeBoolean, nil
	case model.TypeInt16:
		return TypeInt16, nil
	case model.TypeUint16:
		return TypeUInt16, nil
	case model.TypeInt32:
		return TypeInt32, nil
	case model.TypeUint32:
		return TypeUInt32, nil
	case model.TypeInt64:
		return TypeInt64, nil
	case model.TypeUint64:
		return TypeUInt64, nil
	case model.TypeFloat32:
		return TypeFloat, nil
	case model.TypeFloat64:
		return TypeDouble, nil
	case model.TypeString:
		return TypeString, nil
	case model.TypeBytes:
		return TypeByteString, nil
	}
	return 0, fmt.Errorf("opcua: data type %q has no OPC UA equivalent", t)
}

// PlatformType 把内置类型映射回平台类型。
func PlatformType(t byte) (model.DataType, bool) {
	switch t {
	case TypeBoolean:
		return model.TypeBool, true
	case TypeSByte, TypeInt16:
		return model.TypeInt16, true
	case TypeByte, TypeUInt16:
		return model.TypeUint16, true
	case TypeInt32:
		return model.TypeInt32, true
	case TypeUInt32:
		return model.TypeUint32, true
	case TypeInt64:
		return model.TypeInt64, true
	case TypeUInt64:
		return model.TypeUint64, true
	case TypeFloat:
		return model.TypeFloat32, true
	case TypeDouble:
		return model.TypeFloat64, true
	case TypeString, TypeDateTime:
		return model.TypeString, true
	case TypeByteString:
		return model.TypeBytes, true
	}
	return "", false
}

// DecodeValue 把 Variant 转成平台值。want 为空表示按 Variant 自身类型。
//
// 数值型之间做范围检查而不是静默截断：一个 Int32 的 70000 写进 uint16
// 点位，截断成 4464 会在北向留下一条看不出错的错误数据。宁可在解码阶段
// 报 bad，也不要让错误值流进历史库。
func DecodeValue(v Variant, want model.DataType) (model.Value, error) {
	if v.Array {
		return model.Value{}, fmt.Errorf("opcua: array values are not supported")
	}
	if v.Type == 0 {
		return model.Value{}, fmt.Errorf("opcua: empty variant")
	}

	src, ok := PlatformType(v.Type)
	if !ok {
		return model.Value{}, fmt.Errorf("opcua: unsupported variant type %d", v.Type)
	}
	if want == "" {
		want = src
	}

	switch want {
	case model.TypeBool:
		if v.Type == TypeBoolean {
			return model.BoolValue(v.Bool), nil
		}
		n, err := variantNumber(v)
		if err != nil {
			return model.Value{}, err
		}
		return model.BoolValue(n != 0), nil
	case model.TypeString:
		if v.Type == TypeDateTime {
			if v.Time.IsZero() {
				return model.TextValue(model.TypeString, ""), nil
			}
			return model.TextValue(model.TypeString, v.Time.Format(time.RFC3339Nano)), nil
		}
		if v.Type == TypeString {
			return model.TextValue(model.TypeString, v.Str), nil
		}
		// 数值转字符串：保留原始精度（整数用整数格式）。
		if v.Int != 0 || v.Type == TypeInt16 || v.Type == TypeInt32 || v.Type == TypeInt64 || v.Type == TypeSByte {
			return model.TextValue(model.TypeString, strconv.FormatInt(v.Int, 10)), nil
		}
		if v.Uint != 0 || v.Type == TypeByte || v.Type == TypeUInt16 || v.Type == TypeUInt32 || v.Type == TypeUInt64 {
			return model.TextValue(model.TypeString, strconv.FormatUint(v.Uint, 10)), nil
		}
		return model.TextValue(model.TypeString, strconv.FormatFloat(v.Float, 'g', -1, 64)), nil
	case model.TypeBytes:
		if v.Type != TypeByteString {
			return model.Value{}, fmt.Errorf("opcua: cannot convert type %d to bytes", v.Type)
		}
		return model.BytesValue(v.Bytes), nil
	}

	n, err := variantNumber(v)
	if err != nil {
		return model.Value{}, err
	}
	if err := checkRange(want, n); err != nil {
		return model.Value{}, err
	}
	return model.NumValue(want, n), nil
}

// variantNumber 把任意数值 Variant 提成 float64。
func variantNumber(v Variant) (float64, error) {
	switch v.Type {
	case TypeSByte, TypeInt16, TypeInt32, TypeInt64:
		return float64(v.Int), nil
	case TypeByte, TypeUInt16, TypeUInt32, TypeUInt64:
		return float64(v.Uint), nil
	case TypeFloat, TypeDouble:
		return v.Float, nil
	case TypeBoolean:
		if v.Bool {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("opcua: variant type %d is not numeric", v.Type)
}

// checkRange 校验数值是否能被目标类型无损承载。
func checkRange(t model.DataType, n float64) error {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("opcua: non-finite value for %s", t)
	}
	bad := func(lo, hi float64) error {
		if n < lo || n > hi {
			return fmt.Errorf("opcua: value %g out of range for %s", n, t)
		}
		return nil
	}
	switch t {
	case model.TypeInt16:
		return bad(math.MinInt16, math.MaxInt16)
	case model.TypeUint16, model.TypeBCD16:
		return bad(0, math.MaxUint16)
	case model.TypeInt32:
		return bad(math.MinInt32, math.MaxInt32)
	case model.TypeUint32, model.TypeBCD32:
		return bad(0, math.MaxUint32)
	case model.TypeInt64:
		return bad(-9.223372036854776e18, 9.223372036854776e18)
	case model.TypeUint64:
		return bad(0, 1.8446744073709552e19)
	}
	return nil
}

// EncodeValue 把平台值转成写入用的 Variant。
func EncodeValue(t model.DataType, v model.Value) (Variant, error) {
	vt, err := VariantType(t)
	if err != nil {
		return Variant{}, err
	}
	switch t {
	case model.TypeBool:
		b := v.Bool
		if v.Type != model.TypeBool {
			n, ok := v.AsFloat()
			if !ok {
				// 非数值也允许写 false，但更可能是配置错误；
				// 这里只在无法判定时报错，避免把 string "true" 静默写成 false。
				return Variant{}, fmt.Errorf("opcua: cannot write %s value to bool node", v.Type)
			}
			b = n != 0
		}
		return Variant{Type: vt, Bool: b}, nil
	case model.TypeString:
		return Variant{Type: vt, Str: v.Text}, nil
	case model.TypeBytes:
		return Variant{Type: vt, Bytes: []byte(v.Text)}, nil
	}

	n, ok := v.AsFloat()
	if !ok {
		return Variant{}, fmt.Errorf("opcua: cannot write %s value to %s node", v.Type, t)
	}
	if err := checkRange(t, n); err != nil {
		return Variant{}, err
	}
	switch t {
	case model.TypeInt16:
		return Variant{Type: vt, Int: int64(int16(n))}, nil
	case model.TypeInt32:
		return Variant{Type: vt, Int: int64(int32(n))}, nil
	case model.TypeInt64:
		return Variant{Type: vt, Int: int64(n)}, nil
	case model.TypeUint16:
		return Variant{Type: vt, Uint: uint64(uint16(n))}, nil
	case model.TypeUint32:
		return Variant{Type: vt, Uint: uint64(uint32(n))}, nil
	case model.TypeUint64:
		return Variant{Type: vt, Uint: uint64(n)}, nil
	case model.TypeFloat32:
		return Variant{Type: vt, Float: float64(float32(n))}, nil
	case model.TypeFloat64:
		return Variant{Type: vt, Float: n}, nil
	}
	return Variant{}, fmt.Errorf("opcua: cannot encode %s", t)
}

// QualityOf 把 OPC UA 状态码映射到平台质量码。
//
// 这里的取舍是「宁可报坏也不要报好」：Uncertain 在 OPC UA 里表示
// 值可用但不可全信（例如传感器自检失败），映射为 uncertain；只有 Good
// 及其子码才是 good。把 Bad 映射成 bad 而不是 comm_error，是因为状态码
// 已经给出了更精确的原因，通信层错误由驱动另外判定。
func QualityOf(s StatusCode) model.Quality {
	if s.IsGood() {
		return model.QualityGood
	}
	if s.IsUncertain() {
		return model.QualityUncertain
	}
	switch s {
	case StatusBadWaitingForData:
		return model.QualityStale
	case StatusBadNoCommunication, StatusBadNotConnected, StatusBadCommunicationError:
		return model.QualityCommError
	case StatusBadNodeIDUnknown, StatusBadNodeIDInvalid, StatusBadAttributeInvalid:
		return model.QualityAddressError
	}
	return model.QualityBad
}
