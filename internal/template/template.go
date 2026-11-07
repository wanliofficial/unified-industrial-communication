// Package template 实现「参数化设备模板」：一份 JSON 模板 + 一组参数值
// → 一台可校验的设备配置。
//
// 为什么需要它：集成商交付一栋楼时，同一型号的设备有几十台，差别只有
// 地址、单元号和编号。复制粘贴配置的后果不是「写得慢」，而是「每份都
// 有点不一样」——地址错一个寄存器、单位漏写、字节序忘了改。模板把
// 「相同」与「不同」显式分开，不同之处必须写成参数。
//
// 设计约束（与 docs/03-product/03-domain-model.md §13 一致）：
//
//  1. 替换只发生在 JSON 字符串字面量里，不做表达式求值。模板是数据，
//     不是程序；一旦允许 `{base + 2}`，模板就变成了一门需要调试的语言。
//  2. 数值/布尔参数在「整个字面量就是一个占位符」时替换成 JSON 数字/
//     布尔，而不是字符串。否则 `"offset": "{base}"` 会变成
//     `"offset": "0"`，解析时才炸。
//  3. 实例化结果必须能通过完整校验（等价于手工写的配置）；模板本身
//     在加载阶段就要校验，不能等到用的时候才发现占位符拼错。
//  4. 模板与实例分开存储：本包只负责「生成设备」，不负责把它写进配置。
//     这样模板变更不会悄悄改动已经上线的设备。
package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"uic/internal/model"
)

// ParameterType 是模板参数的类型。只保留配置里真正需要的四种：字符串
// 用于 ID/主机名，整数用于偏移/单元号，浮点用于系数，布尔用于开关。
// 不引入数组/对象：模板是「少量标量参数 + 固定结构」，复杂结构应当拆成
// 多个模板。
type ParameterType string

const (
	ParamString ParameterType = "string"
	ParamInt    ParameterType = "int"
	ParamFloat  ParameterType = "float"
	ParamBool   ParameterType = "bool"
)

func (t ParameterType) valid() bool {
	switch t {
	case ParamString, ParamInt, ParamFloat, ParamBool:
		return true
	}
	return false
}

// Parameter 是模板参数声明。
//
// Default 用 json.RawMessage 而不是 string：JSON 里写 `"default": 1`
// 与 `"default": "1"` 是两种不同的东西，前者才能进入 offset 这类数值
// 字段。保留原始字面量让我们能区分它们，并在类型不符时直接报错。
type Parameter struct {
	Name        string          `json:"name"`
	Type        ParameterType   `json:"type"`
	Required    bool            `json:"required,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Description string          `json:"description,omitempty"`
}

// Definition 是一份设备模板。
type Definition struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Protocol 只是提示（用于文档与 CLI 展示），实例化不依赖它：
	// 模板生成的是 model.Device，协议由设备所挂的通道决定。
	Protocol   string          `json:"protocol,omitempty"`
	Parameters []Parameter     `json:"parameters"`
	Device     json.RawMessage `json:"device"`
}

var (
	templateIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	paramNameRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
	placeholderRe = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_]{0,31})\}`)
)

// Parse 解析并校验模板。未知字段会被拒绝：模板是给人写的，拼错的键
// （比如 `paramters`）静默忽略会让「为什么参数没生效」变成一场考古。
func Parse(raw []byte) (*Definition, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Definition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("template: parse: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// Load 从文件读取模板。
func Load(path string) (*Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("template: read %s: %w", path, err)
	}
	d, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// Validate 校验模板自身的完整性。
func (d *Definition) Validate() error {
	if !templateIDRe.MatchString(d.ID) {
		return fmt.Errorf("template.id %q: must match %s", d.ID, templateIDRe.String())
	}
	params := make(map[string]Parameter, len(d.Parameters))
	for _, p := range d.Parameters {
		if !paramNameRe.MatchString(p.Name) {
			return fmt.Errorf("template %q: parameter %q: must match %s", d.ID, p.Name, paramNameRe.String())
		}
		if !p.Type.valid() {
			return fmt.Errorf("template %q: parameter %q: unknown type %q (string|int|float|bool)",
				d.ID, p.Name, p.Type)
		}
		if _, dup := params[p.Name]; dup {
			return fmt.Errorf("template %q: duplicate parameter %q", d.ID, p.Name)
		}
		if len(p.Default) > 0 {
			if _, err := p.DefaultParam(); err != nil {
				return fmt.Errorf("template %q: %w", d.ID, err)
			}
		}
		params[p.Name] = p
	}

	if len(bytes.TrimSpace(d.Device)) == 0 {
		return fmt.Errorf("template %q: device is required", d.ID)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(d.Device), []byte("{")) {
		return fmt.Errorf("template %q: device must be a JSON object", d.ID)
	}
	names, err := placeholders(d.Device)
	if err != nil {
		return fmt.Errorf("template %q: %w", d.ID, err)
	}
	for _, name := range names {
		if _, ok := params[name]; !ok {
			return fmt.Errorf("template %q: device references unknown parameter %q", d.ID, name)
		}
	}
	return nil
}

// Placeholders 返回模板中引用到的参数名（排序后），用于 CLI 展示与测试。
func (d *Definition) Placeholders() ([]string, error) {
	names, err := placeholders(d.Device)
	if err != nil {
		return nil, err
	}
	return names, nil
}

// value 是一个已解析的参数值。text 是可以直接写进 JSON 的字面量
// （字符串参数则是未转义的原文，由调用方决定加不加引号）。
type value struct {
	typ  ParameterType
	text string
}

func parseValue(p Parameter, raw string) (value, error) {
	switch p.Type {
	case ParamString:
		return value{typ: ParamString, text: raw}, nil
	case ParamInt:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return value{}, fmt.Errorf("want an integer, got %q", raw)
		}
		return value{typ: ParamInt, text: strconv.FormatInt(n, 10)}, nil
	case ParamFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return value{}, fmt.Errorf("want a number, got %q", raw)
		}
		return value{typ: ParamFloat, text: strconv.FormatFloat(f, 'g', -1, 64)}, nil
	case ParamBool:
		// 只接受 JSON 的 true/false：`yes`/`1` 在配置文件里没有统一约定，
		// 收窄输入比猜用户意图更安全。
		switch strings.TrimSpace(strings.ToLower(raw)) {
		case "true":
			return value{typ: ParamBool, text: "true"}, nil
		case "false":
			return value{typ: ParamBool, text: "false"}, nil
		}
		return value{}, fmt.Errorf("want true or false, got %q", raw)
	}
	return value{}, fmt.Errorf("unknown type %q", p.Type)
}

// DefaultParam 解析参数的默认值；没有默认值时返回 nil。
//
// 字符串参数要求默认值是 JSON 字符串（`"abc"`），数值/布尔参数要求
// 默认值是 JSON 数字/布尔（`1`、`true`）。两种都接受会让
// `"default": "2"` 在整数参数上「看起来能用」，直到有人把它拼进 ID。
func (p Parameter) DefaultParam() (*string, error) {
	if len(p.Default) == 0 {
		return nil, nil
	}
	if p.Type == ParamString {
		var s string
		if err := json.Unmarshal(p.Default, &s); err != nil {
			return nil, fmt.Errorf("parameter %q: default must be a JSON string", p.Name)
		}
		if _, err := parseValue(p, s); err != nil {
			return nil, fmt.Errorf("parameter %q: default: %w", p.Name, err)
		}
		return &s, nil
	}
	s := strings.TrimSpace(string(p.Default))
	if _, err := parseValue(p, s); err != nil {
		return nil, fmt.Errorf("parameter %q: default: %w", p.Name, err)
	}
	return &s, nil
}

// Instantiate 用参数值生成设备。
//
// 未提供的参数取默认值；未知参数直接报错（静默忽略会让「我明明改了
// 参数」变成无法解释的行为）；必填参数缺失时错误信息里会列出全部缺失项，
// 而不是只报第一个——批量实例化时一次看全比修一个跑一次省时间。
func (d *Definition) Instantiate(params map[string]string) (model.Device, error) {
	known := make(map[string]bool, len(d.Parameters))
	vals := make(map[string]value, len(d.Parameters))
	var missing []string

	for _, p := range d.Parameters {
		known[p.Name] = true
		raw, ok := params[p.Name]
		if !ok {
			def, err := p.DefaultParam()
			if err != nil {
				return model.Device{}, fmt.Errorf("template %q: %w", d.ID, err)
			}
			if def == nil {
				if p.Required {
					missing = append(missing, p.Name)
				}
				continue
			}
			raw = *def
		}
		v, err := parseValue(p, raw)
		if err != nil {
			return model.Device{}, fmt.Errorf("template %q: parameter %q: %w", d.ID, p.Name, err)
		}
		vals[p.Name] = v
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return model.Device{}, fmt.Errorf("template %q: missing required parameter(s): %s",
			d.ID, strings.Join(missing, ", "))
	}

	var unknown []string
	for name := range params {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return model.Device{}, fmt.Errorf("template %q: unknown parameter(s): %s",
			d.ID, strings.Join(unknown, ", "))
	}

	expanded, err := expandJSON(d.Device, vals)
	if err != nil {
		return model.Device{}, fmt.Errorf("template %q: %w", d.ID, err)
	}
	var dev model.Device
	dec := json.NewDecoder(bytes.NewReader(expanded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dev); err != nil {
		return model.Device{}, fmt.Errorf("template %q: device after substitution: %w", d.ID, err)
	}
	dev.TemplateID = d.ID
	return dev, nil
}

// placeholders 返回 JSON 文档字符串值里引用到的参数名。
func placeholders(raw []byte) ([]string, error) {
	seen := map[string]bool{}
	err := walkStrings(raw, func(literal string, isKey bool) error {
		if isKey {
			if strings.Contains(literal, "{") {
				return fmt.Errorf("placeholder in key %s: keys must be literal", literal)
			}
			return nil
		}
		for _, m := range placeholderRe.FindAllStringSubmatch(literal, -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// expandJSON 只替换字符串字面量中的占位符，保持其余字节原样。
//
// 逐字节扫描而不是先反序列化再遍历：后者会丢掉键顺序与数字原始格式，
// 让「模板文件」与「实例文件」无谓地不同，也让 diff 变得难读；而且
// 反序列化后 offset 变成 float64，大整数会静默失真。
func expandJSON(raw []byte, vals map[string]value) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(raw) + 64)
	err := walkStringsWithOutput(raw, func(literal string, isKey bool, quote []byte) error {
		if isKey {
			out.Write(quote)
			return nil
		}
		expanded, err := expandLiteral(literal, vals)
		if err != nil {
			return err
		}
		out.Write(expanded)
		return nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// expandLiteral 处理一个字符串字面量（content 不含引号）。
func expandLiteral(content string, vals map[string]value) ([]byte, error) {
	matches := placeholderRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return []byte(`"` + content + `"`), nil
	}
	// 整个字面量恰好是一个占位符：数值/布尔要落成 JSON 数字/布尔。
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(content) {
		name := content[matches[0][2]:matches[0][3]]
		v, ok := vals[name]
		if !ok {
			return nil, fmt.Errorf("device references unknown parameter %q", name)
		}
		if v.typ == ParamString {
			return []byte(`"` + escapeJSONString(v.text) + `"`), nil
		}
		return []byte(v.text), nil
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		name := content[m[2]:m[3]]
		v, ok := vals[name]
		if !ok {
			return nil, fmt.Errorf("device references unknown parameter %q", name)
		}
		b.WriteString(content[last:m[0]])
		b.WriteString(escapeJSONString(v.text))
		last = m[1]
	}
	b.WriteString(content[last:])
	return []byte(`"` + b.String() + `"`), nil
}

// escapeJSONString 转义字符串，供嵌进 JSON 字面量。
func escapeJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// walkStrings 遍历 JSON 文档里的每个字符串字面量。
//
// literal 是引号之间的原始内容（可能含转义序列），isKey 表示该字符串是
// 对象的键。它不做完整的 JSON 语法校验——那件事交给 encoding/json；
// 这里只需要正确跳过转义，不能把 `\"` 当成字符串结束。
func walkStrings(raw []byte, fn func(literal string, isKey bool) error) error {
	return walkStringsWithOutput(raw, func(literal string, isKey bool, _ []byte) error {
		return fn(literal, isKey)
	}, nil)
}

func walkStringsWithOutput(raw []byte, fn func(literal string, isKey bool, quote []byte) error, out *bytes.Buffer) error {
	i := 0
	for i < len(raw) {
		if raw[i] != '"' {
			if out != nil {
				out.WriteByte(raw[i])
			}
			i++
			continue
		}
		start := i
		i++
		for i < len(raw) {
			if raw[i] == '\\' {
				i += 2
				continue
			}
			if raw[i] == '"' {
				break
			}
			i++
		}
		if i >= len(raw) {
			return fmt.Errorf("unterminated string literal")
		}
		i++ // 跳过结束引号
		literal := string(raw[start+1 : i-1])
		j := i
		for j < len(raw) {
			switch raw[j] {
			case ' ', '\t', '\r', '\n':
				j++
				continue
			}
			break
		}
		isKey := j < len(raw) && raw[j] == ':'
		if err := fn(literal, isKey, raw[start:i]); err != nil {
			return err
		}
	}
	return nil
}
