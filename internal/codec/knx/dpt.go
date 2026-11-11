package knx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"uic/internal/model"
)

// DPT 是 KNX 数据点类型（Datapoint Type）。
//
// KNX 把「一个对象的值怎么编码」定义成一族编号：`主类型.子类型`。主类型
// 决定线宽与编码方式（1 位、1 字节、2 字节浮点、4 字节 IEEE 浮点……），
// 子类型只影响单位与取值范围（9.001 是摄氏温度、9.004 是照度，编码完全
// 相同）。
//
// 为什么必须显式声明而不是从 dataType 反推：dataType=float32 既可能是
// 9.xxx（2 字节二进制浮点）也可能是 14.xxx（4 字节 IEEE），两者的线格式
// 毫无共同点。猜错的代价不是报错，而是把一个温度值解成 3.7e-41——没有
// 任何一层能发现它错了。
type DPT struct {
	// Main 是主类型号（1、5、9、14…）。
	Main int
	// Sub 是子类型号（1、4、7…）。0 表示「该主类型下的通用形式」。
	Sub int

	// Name 是给人看的名字，出现在错误信息里。
	Name string
	// Unit 是计量单位（°C、%、lx…）；无量纲时为空。
	Unit string

	// Bits 是线宽：1、2、4、8、16、32。
	Bits int
	// Kind 决定字节怎么解释。
	Kind DPTKind

	// Factor 只对 KindScaled 有意义：解码值 = 原始字节 * Factor。
	Factor float64
	// Min/Max 是解码后的取值范围（含端点）。用于编码前的范围检查，
	// 以及给前端生成配置提示。
	Min, Max float64
}

// DPTKind 是 DPT 的编码类别。
type DPTKind string

const (
	// KindBool 是 1 位布尔量，值直接放在 APCI 字节的低 6 位里。
	KindBool DPTKind = "bool"
	// KindUnsigned 是无符号整数（1/2/4 字节）。
	KindUnsigned DPTKind = "unsigned"
	// KindSigned 是有符号整数（1/2/4 字节，补码）。
	KindSigned DPTKind = "signed"
	// KindScaled 是「原始字节 * Factor」的线性映射（DPT 5.001 的百分比）。
	KindScaled DPTKind = "scaled"
	// KindFloat16 是 KNX 自己的 2 字节二进制浮点：1 位符号 + 4 位指数 +
	// 11 位补码尾数，值 = 尾数 * 2^指数 / 100。它不是 IEEE 半精度。
	KindFloat16 DPTKind = "float16"
	// KindFloat32 是 IEEE 754 单精度（DPT 14.xxx）。
	KindFloat32 DPTKind = "float32"
)

// Payload 是一次组值报文携带的载荷。
//
// 1 位 DPT 的值不在「数据域」里，而是编码在 APCI 字节的低 6 位：因此
// 分开表达比塞进一个 []byte 更不容易写错——把 1 位值当成 1 字节发出去，
// 设备会把它读成一个字节的 DPT，而不是开关。
type Payload struct {
	// Bit 在 DPT.Bits == 1 时有效。
	Bit bool
	// Data 在 DPT.Bits > 1 时有效，长度恒为 Bits/8。
	Data []byte
}

// dptTable 是支持的 DPT 集合。
//
// 只收录现场最常见的类型。刻意不做的有：3 位/4 位受控类型（调光控制，
// 值里混着「相对/绝对」与「步长」两个语义，平台模型表达不了）、
// 变长字符串（DPT 15/16/17/28）、复合结构（DPT 21/22/23/225 等）。
// 遇到未收录的类型时返回明确错误并列出支持的集合，而不是按位宽硬编——
// 按位宽硬编能「读出一个数」，但那个数的含义无人能验证。
//
// 用切片而不是 map：`9.001` 与 `9.1` 是同一个类型，字符串键必须做补零
// 归一化才查得到，而少归一化一处就会变成「表里明明有、就是查不到」。
// 数值键没有这个问题。
var dptTable = []DPT{
	{Main: 1, Sub: 1, Name: "开关", Bits: 1, Kind: KindBool},
	{Main: 1, Sub: 2, Name: "布尔", Bits: 1, Kind: KindBool},
	{Main: 1, Sub: 3, Name: "使能", Bits: 1, Kind: KindBool},
	{Main: 1, Sub: 9, Name: "开/关", Bits: 1, Kind: KindBool},

	{Main: 5, Sub: 1, Name: "百分比", Unit: "%", Bits: 8, Kind: KindScaled,
		Factor: 100.0 / 255.0, Min: 0, Max: 100},
	{Main: 5, Sub: 3, Name: "角度", Unit: "°", Bits: 8, Kind: KindScaled,
		Factor: 360.0 / 255.0, Min: 0, Max: 360},
	{Main: 5, Sub: 4, Name: "百分比(0..255)", Unit: "%", Bits: 8, Kind: KindScaled,
		Factor: 1, Min: 0, Max: 255},
	{Main: 5, Sub: 10, Name: "脉冲计数", Bits: 8, Kind: KindUnsigned, Min: 0, Max: 255},

	{Main: 6, Sub: 1, Name: "百分比(有符号)", Unit: "%", Bits: 8, Kind: KindSigned, Min: -128, Max: 127},
	{Main: 6, Sub: 10, Name: "计数(有符号)", Bits: 8, Kind: KindSigned, Min: -128, Max: 127},

	{Main: 7, Sub: 1, Name: "脉冲计数(16 位)", Bits: 16, Kind: KindUnsigned, Min: 0, Max: 65535},
	{Main: 7, Sub: 12, Name: "无符号计数", Bits: 16, Kind: KindUnsigned, Min: 0, Max: 65535},
	{Main: 8, Sub: 1, Name: "有符号计数(16 位)", Bits: 16, Kind: KindSigned, Min: -32768, Max: 32767},
	{Main: 8, Sub: 10, Name: "百分比(16 位有符号)", Unit: "%", Bits: 16, Kind: KindSigned, Min: -32768, Max: 32767},

	{Main: 9, Sub: 1, Name: "温度", Unit: "°C", Bits: 16, Kind: KindFloat16, Min: -273, Max: 670433.28},
	{Main: 9, Sub: 2, Name: "温差", Unit: "K", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 3, Name: "温度变化率", Unit: "K/h", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 4, Name: "照度", Unit: "lx", Bits: 16, Kind: KindFloat16, Min: 0, Max: 670760},
	{Main: 9, Sub: 5, Name: "风速", Unit: "m/s", Bits: 16, Kind: KindFloat16, Min: 0, Max: 670760},
	{Main: 9, Sub: 6, Name: "气压", Unit: "Pa", Bits: 16, Kind: KindFloat16, Min: 0, Max: 670760},
	{Main: 9, Sub: 7, Name: "湿度", Unit: "%", Bits: 16, Kind: KindFloat16, Min: 0, Max: 670760},
	{Main: 9, Sub: 8, Name: "空气品质", Unit: "ppm", Bits: 16, Kind: KindFloat16, Min: 0, Max: 670760},
	{Main: 9, Sub: 20, Name: "电压", Unit: "mV", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 21, Name: "电流", Unit: "mA", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 22, Name: "功率密度", Unit: "W/m²", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 24, Name: "功率", Unit: "kW", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 25, Name: "体积流量", Unit: "m³/h", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	{Main: 9, Sub: 28, Name: "风速", Unit: "km/h", Bits: 16, Kind: KindFloat16, Min: 0, Max: 670760},

	{Main: 12, Sub: 1, Name: "无符号计数(32 位)", Bits: 32, Kind: KindUnsigned, Min: 0, Max: 4294967295},
	{Main: 13, Sub: 1, Name: "有符号计数(32 位)", Bits: 32, Kind: KindSigned,
		Min: -2147483648, Max: 2147483647},
	{Main: 13, Sub: 10, Name: "有功电能", Unit: "Wh", Bits: 32, Kind: KindSigned,
		Min: -2147483648, Max: 2147483647},
	{Main: 13, Sub: 13, Name: "有功电能", Unit: "kWh", Bits: 32, Kind: KindSigned,
		Min: -2147483648, Max: 2147483647},

	{Main: 14, Sub: 1, Name: "加速度", Unit: "m/s²", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	{Main: 14, Sub: 7, Name: "角度", Unit: "°", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	{Main: 14, Sub: 19, Name: "电流", Unit: "A", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	{Main: 14, Sub: 27, Name: "电压", Unit: "V", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	{Main: 14, Sub: 31, Name: "有功功率", Unit: "kW", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	{Main: 14, Sub: 33, Name: "频率", Unit: "Hz", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	{Main: 14, Sub: 56, Name: "功率", Unit: "W", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},

	{Main: 20, Sub: 102, Name: "HVAC 模式", Bits: 8, Kind: KindUnsigned, Min: 0, Max: 255},
}

// dptFamily 是「只写主类型」时的默认形式。用于 `-dpt 9` 这种写法：现场
// 文档常只写主类型，具体子类型由工程决定，但线格式对同一主类型是一样的。
var dptFamily = map[int]DPT{
	1:  {Main: 1, Name: "1 位开关", Bits: 1, Kind: KindBool},
	5:  {Main: 5, Name: "8 位无符号", Bits: 8, Kind: KindUnsigned, Min: 0, Max: 255},
	6:  {Main: 6, Name: "8 位有符号", Bits: 8, Kind: KindSigned, Min: -128, Max: 127},
	7:  {Main: 7, Name: "16 位无符号", Bits: 16, Kind: KindUnsigned, Min: 0, Max: 65535},
	8:  {Main: 8, Name: "16 位有符号", Bits: 16, Kind: KindSigned, Min: -32768, Max: 32767},
	9:  {Main: 9, Name: "2 字节浮点", Bits: 16, Kind: KindFloat16, Min: -670760, Max: 670760},
	12: {Main: 12, Name: "32 位无符号", Bits: 32, Kind: KindUnsigned, Min: 0, Max: 4294967295},
	13: {Main: 13, Name: "32 位有符号", Bits: 32, Kind: KindSigned, Min: -2147483648, Max: 2147483647},
	14: {Main: 14, Name: "4 字节浮点", Bits: 32, Kind: KindFloat32,
		Min: -math.MaxFloat32, Max: math.MaxFloat32},
	20: {Main: 20, Name: "8 位枚举", Bits: 8, Kind: KindUnsigned, Min: 0, Max: 255},
}

// ParseDPT 解析 DPT 文本。
//
// 接受 `9.001`、`9`（主类型默认形式）、`DPT9.001`。不接受空串：把空值
// 当成「按 dataType 猜」正是本包要消除的那种猜测。
func ParseDPT(s string) (DPT, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return DPT{}, errors.New("knx: dpt is required (e.g. 9.001); KNX group addresses carry no type information")
	}
	body := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(raw), "DPT"))
	if body == "" {
		return DPT{}, fmt.Errorf("knx: dpt %q: missing type number", s)
	}
	mainPart, subPart, hasSub := strings.Cut(body, ".")
	main, err := strconv.Atoi(strings.TrimSpace(mainPart))
	if err != nil {
		return DPT{}, fmt.Errorf("knx: dpt %q: main type %q is not a number", s, mainPart)
	}
	sub := 0
	if hasSub {
		subStr := strings.TrimSpace(subPart)
		// `9.*` 与 `9.xxx` 是文档里的通配写法，等价于「只给主类型」。
		if subStr != "" && subStr != "*" && !strings.EqualFold(subStr, "xxx") {
			n, err := strconv.Atoi(subStr)
			if err != nil {
				return DPT{}, fmt.Errorf("knx: dpt %q: sub type %q is not a number", s, subPart)
			}
			sub = n
		}
	}
	if main <= 0 || sub < 0 {
		return DPT{}, fmt.Errorf("knx: dpt %q: main type must be positive and sub type non-negative", s)
	}
	for _, d := range dptTable {
		if d.Main == main && d.Sub == sub {
			return d, nil
		}
	}
	if sub == 0 {
		if d, ok := dptFamily[main]; ok {
			return d, nil
		}
	}
	return DPT{}, fmt.Errorf("knx: dpt %q is not supported (supported: %s)", s, strings.Join(DPTNames(), ", "))
}

// DPTNames 返回支持的具体 DPT 列表（排序后），用于错误信息与前端枚举。
func DPTNames() []string {
	out := make([]string, 0, len(dptTable))
	for _, d := range dptTable {
		out = append(out, d.String())
	}
	return out
}

// String 输出规范写法 `主类型.子类型`。
func (d DPT) String() string { return fmt.Sprintf("%d.%03d", d.Main, d.Sub) }

// Width 返回线宽（位）。
func (d DPT) Width() int { return d.Bits }

// Bytes 返回载荷字节数；1 位类型返回 0（值在 APCI 字节里）。
func (d DPT) Bytes() int {
	if d.Bits <= 1 {
		return 0
	}
	return d.Bits / 8
}

// Accepts 判断点位声明的主类型是否与 DPT 相容。
//
// 只校验「能否无损承载」，不强行一致：DPT 9.001 解出来是浮点，声明
// float32 或 float64 都可以；声明 bool 则必然是把线宽弄错了。
func (d DPT) Accepts(t model.DataType) error {
	switch d.Kind {
	case KindBool:
		if t != model.TypeBool {
			return fmt.Errorf("dpt %s (%s) is 1 bit, requires dataType bool, got %q", d, d.Name, t)
		}
		return nil
	}
	if d.Bits == 8 && d.Kind == KindUnsigned && t == model.TypeBool {
		return fmt.Errorf("dpt %s is %d-bit unsigned, not a bool", d, d.Bits)
	}
	switch t {
	case model.TypeBool:
		return fmt.Errorf("dpt %s (%s) is %d bits, requires a numeric dataType, got bool", d, d.Name, d.Bits)
	case model.TypeString, model.TypeBytes, model.TypeBCD16, model.TypeBCD32:
		return fmt.Errorf("dpt %s (%s) requires a numeric dataType, got %q", d, d.Name, t)
	}
	// 位宽超过目标类型容量时拒绝：把 32 位计数读进 uint16 会静默截断。
	if d.Bits > 0 && t.Bytes() > 0 && d.Bits > t.Bytes()*8 {
		return fmt.Errorf("dpt %s is %d bits, dataType %s holds only %d bits",
			d, d.Bits, t, t.Bytes()*8)
	}
	return nil
}

// Decode 把载荷解码成平台值。返回值类型由调用方声明的 dataType 决定，
// 但**取值范围与线宽由 DPT 决定**。
func (d DPT) Decode(p Payload, t model.DataType) (model.Value, error) {
	if err := d.Accepts(t); err != nil {
		return model.Value{}, err
	}
	switch d.Kind {
	case KindBool:
		return model.BoolValue(p.Bit), nil
	}
	data := p.Data
	if len(data) != d.Bytes() {
		return model.Value{}, fmt.Errorf("dpt %s: payload must be %d bytes, got %d", d, d.Bytes(), len(data))
	}
	switch d.Kind {
	case KindUnsigned:
		u := beUint(data)
		if u > d.Max {
			// 超出声明范围不是「设备坏了」，而是点表把 DPT 写窄了。报错比
			// 静默返回一个越界值更有用。
			return model.Value{}, fmt.Errorf("dpt %s: raw value %v exceeds declared maximum %v", d, u, d.Max)
		}
		return model.NumValue(t, u), nil
	case KindSigned:
		s := beInt(data)
		if s > d.Max || s < d.Min {
			return model.Value{}, fmt.Errorf("dpt %s: raw value %v outside %v..%v", d, s, d.Min, d.Max)
		}
		return model.NumValue(t, s), nil
	case KindScaled:
		raw := float64(beUint(data))
		return model.NumValue(t, raw*d.Factor), nil
	case KindFloat16:
		v, err := decodeFloat16(data)
		if err != nil {
			return model.Value{}, fmt.Errorf("dpt %s: %w", d, err)
		}
		return model.NumValue(t, v), nil
	case KindFloat32:
		bits := binary.BigEndian.Uint32(data)
		return model.NumValue(t, float64(math.Float32frombits(bits))), nil
	}
	return model.Value{}, fmt.Errorf("dpt %s: unsupported encoding %q", d, d.Kind)
}

// Encode 把平台值编码成载荷。
func (d DPT) Encode(v model.Value, t model.DataType) (Payload, error) {
	if err := d.Accepts(t); err != nil {
		return Payload{}, err
	}
	if d.Kind == KindBool {
		switch {
		case v.Type == model.TypeBool:
			return Payload{Bit: v.Bool}, nil
		case v.Type.IsNumeric():
			// 允许用 0/1 写开关：命令行与剧本里更自然。其他数值一律拒绝，
			// 否则 `2` 会被静默当成 true。
			switch v.Num {
			case 0:
				return Payload{Bit: false}, nil
			case 1:
				return Payload{Bit: true}, nil
			}
			return Payload{}, fmt.Errorf("dpt %s: bool accepts 0 or 1, got %v", d, v.Num)
		case v.Type == model.TypeString:
			switch strings.ToLower(strings.TrimSpace(v.Text)) {
			case "on", "true", "1":
				return Payload{Bit: true}, nil
			case "off", "false", "0":
				return Payload{Bit: false}, nil
			}
			return Payload{}, fmt.Errorf("dpt %s: bool accepts on/off, got %q", d, v.Text)
		}
		return Payload{}, fmt.Errorf("dpt %s: cannot encode %s as bool", d, v.Type)
	}

	num, ok := v.AsFloat()
	if !ok {
		return Payload{}, fmt.Errorf("dpt %s: %s is not numeric", d, v.Type)
	}
	if num < d.Min || num > d.Max {
		return Payload{}, fmt.Errorf("dpt %s (%s): value %v outside %v..%v%s",
			d, d.Name, num, d.Min, d.Max, unitSuffix(d.Unit))
	}
	switch d.Kind {
	case KindUnsigned:
		return Payload{Data: putUint(uint64(math.Round(num)), d.Bytes())}, nil
	case KindSigned:
		return Payload{Data: putInt(int64(math.Round(num)), d.Bytes())}, nil
	case KindScaled:
		raw := math.Round(num / d.Factor)
		if raw < 0 {
			raw = 0
		}
		if maxRaw := math.Pow(2, float64(d.Bits)) - 1; raw > maxRaw {
			raw = maxRaw
		}
		return Payload{Data: putUint(uint64(raw), d.Bytes())}, nil
	case KindFloat16:
		b, err := encodeFloat16(num)
		if err != nil {
			return Payload{}, fmt.Errorf("dpt %s: %w", d, err)
		}
		return Payload{Data: b}, nil
	case KindFloat32:
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, math.Float32bits(float32(num)))
		return Payload{Data: b}, nil
	}
	return Payload{}, fmt.Errorf("dpt %s: unsupported encoding %q", d, d.Kind)
}

func unitSuffix(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}

// beUint 把 1..4 字节按大端读成无符号数。
func beUint(b []byte) float64 {
	var u uint64
	for _, c := range b {
		u = u<<8 | uint64(c)
	}
	return float64(u)
}

// beInt 把 1..4 字节按大端补码读成有符号数。
func beInt(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var u uint64
	for _, c := range b {
		u = u<<8 | uint64(c)
	}
	bits := uint(len(b)) * 8
	if u&(1<<(bits-1)) != 0 {
		return float64(int64(u) - int64(1)<<bits)
	}
	return float64(u)
}

func putUint(u uint64, n int) []byte {
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte(u)
		u >>= 8
	}
	return out
}

func putInt(v int64, n int) []byte {
	out := make([]byte, n)
	u := uint64(v)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte(u)
		u >>= 8
	}
	return out
}

// decodeFloat16 解 KNX 的 2 字节浮点。
//
//	值 = (尾数) * 2^(指数) / 100
//
// 尾数是 11 位**补码**（不是符号-幅值），指数 4 位无符号。0x7FFF 被标准
// 保留为「无效数据」，遇到时必须报错而不是解成 0x7FF * 2^15 / 100。
func decodeFloat16(b []byte) (float64, error) {
	if len(b) != 2 {
		return 0, fmt.Errorf("2-byte float needs 2 bytes, got %d", len(b))
	}
	raw := binary.BigEndian.Uint16(b)
	if raw == 0x7FFF {
		return 0, errors.New("value is the reserved 0x7FFF (invalid data)")
	}
	exp := int((raw >> 11) & 0x0F)
	mant := int(raw & 0x7FF)
	if raw&0x8000 != 0 {
		mant -= 2048
	}
	return float64(mant) * math.Ldexp(1, exp) / 100, nil
}

// encodeFloat16 编 KNX 的 2 字节浮点。
//
// 归一化策略与工程工具一致：先把值乘 100，再右移除 2 直到落进 11 位补码
// 的 [-2048, 2047]；接近 0 的值（|v| < 0.005）统一编成 0x0000，避免
// `-0.005` 被编成 0x8000（那会被解码成 -20.48）。
func encodeFloat16(v float64) ([]byte, error) {
	knx := v * 100
	if math.Round(knx) == 0 {
		return []byte{0x00, 0x00}, nil
	}
	exp := 0
	for !(knx >= -2048 && knx <= 2047) {
		exp++
		knx /= 2
		if exp > 15 {
			return nil, fmt.Errorf("value %v is out of range for 2-byte float", v)
		}
	}
	mant := int(math.Round(knx)) & 0x7FF
	msb := byte((exp << 3) | (mant >> 8))
	if knx < 0 {
		msb |= 0x80
	}
	return []byte{msb, byte(mant & 0xFF)}, nil
}
