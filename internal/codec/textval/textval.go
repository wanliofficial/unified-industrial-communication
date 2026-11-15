// Package textval 把「文本 / JSON 标量」载荷解码成平台值，以及反向编码。
//
// 这一层服务的是一类协议：载荷本身就是设备给出的值的字符串表示（`"21.5"`、
// `"true"`、`"RUN"`），或者是一段 JSON、由点位配置里的 selector 指定取哪个
// 字段（`data.temperature`、`values[0].temp`）。MQTT 南向与 CoAP 都属于这一类，
// 区别只在「怎么把这段字节拿回来」，而不在「怎么把它变成一个数」。
//
// 因此这里只做纯函数：不读时钟、不做 I/O、不认识任何协议。把这一层单独拿出来
// 的收益是两处协议共享同一套类型规则——同一个 `"on"` 不会在 MQTT 上解析成
// true、在 CoAP 上解析失败。
package textval

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"uic/internal/model"
)

// Select 按点分路径取 JSON 字段，支持 `a.b` 与 `a[0]`。
//
// selector 必须在调用前保证非空：空 selector 在协议层的语义是「整段载荷就是
// 值」，通常不该按 JSON 解析（`123` 按文本是字符串 "123"，按 JSON 是数字）。
// 需要「整段 JSON 文档的标量值」时用 JSONValue。
func Select(payload []byte, selector string) ([]byte, error) {
	if selector == "" {
		return nil, fmt.Errorf("textval: empty selector")
	}
	doc, err := decodeJSON(payload)
	if err != nil {
		return nil, err
	}
	cur := doc
	for _, seg := range splitSelector(selector) {
		// 先按键名下钻，再按下标取元素。`values[0]` 必须两步都做：
		// 只做下标会把「文档本身」当成数组，于是报出一个很奇怪的类型错。
		if seg.name != "" {
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("selector %q: %q is not an object", selector, seg.name)
			}
			next, ok := obj[seg.name]
			if !ok {
				return nil, fmt.Errorf("selector %q: field %q not found", selector, seg.name)
			}
			cur = next
		}
		if seg.index != nil {
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("selector %q: %q is not an array", selector, seg.name)
			}
			i := *seg.index
			if i < 0 || i >= len(arr) {
				return nil, fmt.Errorf("selector %q: index %d out of range (%d)", selector, i, len(arr))
			}
			cur = arr[i]
		}
	}
	return leafText(cur, selector)
}

// JSONValue 取整段 JSON 的标量值。对象/数组会被压缩成紧凑 JSON 文本，交给
// string 类型接收；null 视为错误（没有值不等于空字符串）。
func JSONValue(payload []byte) ([]byte, error) {
	doc, err := decodeJSON(payload)
	if err != nil {
		return nil, err
	}
	return leafText(doc, "")
}

func decodeJSON(payload []byte) (any, error) {
	var doc any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("payload is not valid JSON: %w", err)
	}
	return doc, nil
}

// leafText 把 JSON 叶子转成不含引号的文本。
func leafText(v any, selector string) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		if selector == "" {
			return nil, fmt.Errorf("payload is JSON null")
		}
		return nil, fmt.Errorf("selector %q: value is null", selector)
	case string:
		return []byte(x), nil
	case bool:
		return []byte(strconv.FormatBool(x)), nil
	case json.Number:
		return []byte(x.String()), nil
	default:
		// 对象/数组作为叶子值：按紧凑 JSON 返回，由字符串类型接收。
		b, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
}

type selSeg struct {
	name  string
	index *int
}

// splitSelector 把 "a.b[0].c" 拆成 [{a} {b,0} {c}]。
func splitSelector(s string) []selSeg {
	var out []selSeg
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			continue
		}
		name := part
		var idx *int
		// 只有当方括号里是合法整数时才把它当成数组下标。`arr[x]` 不是
		// 下标语法，必须按完整字段名处理——否则会静默读成整个数组，
		// 把「取不到那个字段」伪装成「字段值就是一个数组」。
		if i := strings.IndexByte(part, '['); i >= 0 && strings.HasSuffix(part, "]") {
			if n, err := strconv.Atoi(part[i+1 : len(part)-1]); err == nil {
				name = part[:i]
				idx = &n
			}
		}
		if name == "" && idx == nil {
			continue
		}
		out = append(out, selSeg{name: name, index: idx})
	}
	return out
}

// Decode 把一段文本转成目标类型的平台值。
func Decode(raw []byte, dt model.DataType) (model.Value, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return model.Value{}, fmt.Errorf("empty payload")
	}
	switch dt {
	case model.TypeBool:
		switch strings.ToLower(s) {
		case "true", "1", "on", "yes":
			return model.BoolValue(true), nil
		case "false", "0", "off", "no":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("cannot parse %q as bool", s)
	case model.TypeString:
		return model.TextValue(model.TypeString, s), nil
	case "":
		// 未声明类型时按内容猜：整数 → int64，小数 → float64，其余 → string。
		return Guess(s)
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return model.Value{}, fmt.Errorf("cannot parse %q as %s", s, dt)
	}
	// 未知类型不能悄悄按数值解析：一个拼错的 `dataType` 会变成「能读到值，
	// 但所有比较/缩放都按错误的类型走」，比直接报错难排查得多。
	switch dt {
	case model.TypeInt16, model.TypeUint16, model.TypeInt32, model.TypeUint32,
		model.TypeInt64, model.TypeUint64, model.TypeFloat32, model.TypeFloat64,
		model.TypeBCD16, model.TypeBCD32:
	default:
		return model.Value{}, fmt.Errorf("unknown data type %q", dt)
	}
	// 越界判错而不是截断：与 OPC UA 侧保持同一条纪律。
	if err := CheckRange(dt, n); err != nil {
		return model.Value{}, err
	}
	return model.NumValue(dt, n), nil
}

// Guess 在没有声明类型时按内容推断。
func Guess(s string) (model.Value, error) {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		// 超过 2^53 的整数在 float64 里不再精确：直接转数值会静默变成
		// 相邻的一个数（`MaxInt64` 甚至回绕成 `-2^63`）。这种「看着像
		// 数字、存下去却变了」的输入按文本保留，至少上层看到的是原样。
		if f := float64(i); int64(f) == i {
			return model.NumValue(model.TypeInt64, f), nil
		}
		return model.TextValue(model.TypeString, s), nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return model.NumValue(model.TypeFloat64, f), nil
	}
	return model.TextValue(model.TypeString, s), nil
}

// CheckRange 校验数值能否被目标类型无损承载。
//
// 非数值类型（string/bool/bytes）没有数值范围可校验，直接通过——调用方
// 负责先按类型分派。
func CheckRange(dt model.DataType, n float64) error {
	// NaN/±Inf 一旦进入模型，下游没有一条路能正确处理：JSON 不允许它们，
	// 转 int64 是未定义行为，比较永远为假。`strconv.ParseFloat` 接受
	// "NaN"/"Inf" 字面量，所以这里必须显式挡住。
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("value %v is not finite for %s", n, dt)
	}
	bad := func(lo, hi float64) error {
		if n < lo || n > hi {
			return fmt.Errorf("value %g out of range for %s", n, dt)
		}
		return nil
	}
	// 整数类型不接受小数：`1.5` 存成一个 TypeInt32 的值会让下游的编码器
	// 在 int32 载荷里写出 1.5。越界判错、小数也判错，一律不截断。
	switch dt {
	case model.TypeInt16, model.TypeUint16, model.TypeInt32, model.TypeUint32,
		model.TypeInt64, model.TypeUint64, model.TypeBCD16, model.TypeBCD32:
		if math.Trunc(n) != n {
			return fmt.Errorf("value %g is not an integer for %s", n, dt)
		}
	}
	switch dt {
	case model.TypeInt16:
		return bad(-32768, 32767)
	case model.TypeUint16, model.TypeBCD16:
		return bad(0, 65535)
	case model.TypeInt32:
		return bad(-2147483648, 2147483647)
	case model.TypeUint32, model.TypeBCD32:
		return bad(0, 4294967295)
	case model.TypeInt64:
		// 上界是开区间：float64 表示不了 MaxInt64，2^63 本身已经超出。
		if n < -9223372036854775808 || n >= 9223372036854775808 {
			return fmt.Errorf("value %g out of range for %s", n, dt)
		}
	case model.TypeUint64:
		if n < 0 || n >= 18446744073709551616 {
			return fmt.Errorf("value %g out of range for %s", n, dt)
		}
	case model.TypeFloat32:
		return bad(-3.4028234663852886e38, 3.4028234663852886e38)
	}
	return nil
}

// FormatNumber 按声明类型校验数值并格式化成十进制文本。
//
// 文本载荷与 JSON 数字字面量是同一种写法，两条写路径因此共用它：范围、
// 整数性、有限性都在这里一次判清，绝不把越界值交给 `int64(n)` 去回绕。
func FormatNumber(n float64, dt model.DataType) ([]byte, error) {
	if err := CheckRange(dt, n); err != nil {
		return nil, err
	}
	switch dt {
	case model.TypeFloat32, model.TypeFloat64:
		return []byte(strconv.FormatFloat(n, 'g', -1, 64)), nil
	case model.TypeUint64:
		// uint64 的上半段转 int64 会变成负数，必须用无符号格式化。
		return []byte(strconv.FormatUint(uint64(n), 10)), nil
	case model.TypeInt16, model.TypeUint16, model.TypeInt32, model.TypeUint32,
		model.TypeInt64, model.TypeBCD16, model.TypeBCD32:
		return []byte(strconv.FormatInt(int64(n), 10)), nil
	}
	return nil, fmt.Errorf("unknown data type %q", dt)
}

// Encode 把平台值编成文本载荷。
//
// 整数刻意不输出 `42.000000`：很多设备侧的解析器只认整数写法。
func Encode(v model.Value, dt model.DataType) ([]byte, error) {
	t := dt
	if t == "" {
		t = v.Type
	}
	switch t {
	case model.TypeBool:
		return []byte(strconv.FormatBool(v.Bool)), nil
	case model.TypeString:
		return []byte(v.Text), nil
	}
	n, ok := v.AsFloat()
	if !ok {
		return nil, fmt.Errorf("cannot encode %s value as %s", v.Type, t)
	}
	return FormatNumber(n, t)
}
