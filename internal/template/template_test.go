package template

import (
	"strings"
	"testing"
)

const meterTemplate = `{
  "id": "modbus-meter",
  "name": "三相电表",
  "protocol": "modbus-tcp",
  "parameters": [
    {"name": "index", "type": "string", "required": true},
    {"name": "unitId", "type": "int", "default": 1},
    {"name": "base", "type": "int", "default": 0},
    {"name": "ratio", "type": "float", "default": 0.1},
    {"name": "enabled", "type": "bool", "default": true}
  ],
  "device": {
    "id": "meter-{index}",
    "name": "电表 {index}",
    "unitId": "{unitId}",
    "enabled": "{enabled}",
    "interval": "5s",
    "points": [
      {
        "id": "voltage",
        "name": "电压 {index}",
        "address": {"kind": "register", "area": "holding", "offset": "{base}", "length": 2, "bit": -1},
        "dataType": "float32",
        "byteOrder": "CDAB",
        "unit": "V",
        "scale": "{ratio}",
        "enabled": true
      }
    ]
  }
}`

func parseMeter(t *testing.T) *Definition {
	t.Helper()
	d, err := Parse([]byte(meterTemplate))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

func TestInstantiateWithDefaults(t *testing.T) {
	d := parseMeter(t)
	dev, err := d.Instantiate(map[string]string{"index": "1"})
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if dev.ID != "meter-1" || dev.Name != "电表 1" {
		t.Fatalf("device = %q / %q", dev.ID, dev.Name)
	}
	if dev.UnitID != 1 || !dev.Enabled {
		t.Fatalf("unitId=%d enabled=%v", dev.UnitID, dev.Enabled)
	}
	if dev.TemplateID != "modbus-meter" {
		t.Fatalf("templateId = %q", dev.TemplateID)
	}
	if len(dev.Points) != 1 {
		t.Fatalf("points = %d", len(dev.Points))
	}
	p := dev.Points[0]
	if p.Name != "电压 1" || p.Address.Offset != 0 || p.Address.Length != 2 {
		t.Fatalf("point = %+v", p)
	}
	if p.Address.Bit != -1 {
		t.Fatalf("bit = %d, want -1", p.Address.Bit)
	}
	// 数值/布尔参数必须落成 JSON 数字/布尔：如果变成字符串，反序列化
	// 阶段就会因为类型不符而失败，或者更糟——静默变成零值。
	if p.Scale != 0.1 {
		t.Fatalf("scale = %v", p.Scale)
	}

	dev.ChannelID = "ch-meter"
	if err := dev.Validate(); err != nil {
		t.Fatalf("instantiated device must validate: %v", err)
	}
}

func TestInstantiateOverridesDefaults(t *testing.T) {
	d := parseMeter(t)
	dev, err := d.Instantiate(map[string]string{
		"index": "07", "unitId": "9", "base": "100", "ratio": "2.5", "enabled": "false",
	})
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if dev.ID != "meter-07" || dev.UnitID != 9 || dev.Enabled {
		t.Fatalf("device = %+v", dev)
	}
	if dev.Points[0].Address.Offset != 100 || dev.Points[0].Scale != 2.5 {
		t.Fatalf("point = %+v", dev.Points[0])
	}
}

func TestInstantiateIsDeterministic(t *testing.T) {
	d := parseMeter(t)
	a, err := d.Instantiate(map[string]string{"index": "1"})
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	b, err := d.Instantiate(map[string]string{"index": "1"})
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if a.ID != b.ID || len(a.Points) != len(b.Points) ||
		a.Points[0].Address != b.Points[0].Address {
		t.Fatalf("two instantiations differ: %+v / %+v", a, b)
	}
}

func TestInstantiateReportsAllMissingParams(t *testing.T) {
	d, err := Parse([]byte(`{
      "id": "t",
      "parameters": [
        {"name": "a", "type": "string", "required": true},
        {"name": "b", "type": "int", "required": true}
      ],
      "device": {"id": "d-{a}-{b}"}
    }`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = d.Instantiate(nil)
	if err == nil {
		t.Fatal("missing params must fail")
	}
	// 一次列出全部缺失项：批量实例化时修一个跑一次很浪费时间。
	if !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstantiateRejectsUnknownParam(t *testing.T) {
	d := parseMeter(t)
	_, err := d.Instantiate(map[string]string{"index": "1", "basee": "2"})
	if err == nil || !strings.Contains(err.Error(), "unknown parameter") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstantiateRejectsBadValue(t *testing.T) {
	d := parseMeter(t)
	if _, err := d.Instantiate(map[string]string{"index": "1", "base": "x"}); err == nil {
		t.Fatal("non-integer base must fail")
	}
	if _, err := d.Instantiate(map[string]string{"index": "1", "enabled": "yes"}); err == nil {
		t.Fatal("bool must be true/false")
	}
	if _, err := d.Instantiate(map[string]string{"index": "1", "ratio": "abc"}); err == nil {
		t.Fatal("float must be numeric")
	}
}

// 参数名允许 camelCase（配置里 unitId 这种写法很常见），占位符必须与
// 声明逐字符一致。
func TestParameterNameCaseSensitive(t *testing.T) {
	d, err := Parse([]byte(`{
      "id": "t",
      "parameters": [{"name": "unitId", "type": "int", "required": true}],
      "device": {"id": "x", "unitId": "{unitId}"}
    }`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := d.Instantiate(map[string]string{"unitid": "1"}); err == nil {
		t.Fatal("placeholder names must be case-sensitive")
	}
}

// 参数值里的引号、反斜杠、换行必须正确转义，否则会生成一份坏 JSON，
// 而错误信息会指向模板本身，让人以为模板写错了。
func TestInstantiateEscapesStringValues(t *testing.T) {
	d, err := Parse([]byte(`{
      "id": "escape",
      "parameters": [{"name": "label", "type": "string", "required": true}],
      "device": {"id": "dev-1", "name": "{label}", "interval": "1s"}
    }`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	label := "a\"b\\c\nd"
	dev, err := d.Instantiate(map[string]string{"label": label})
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if dev.Name != label {
		t.Fatalf("name = %q, want %q", dev.Name, label)
	}
}

func TestTemplateValidate(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bad id", `{"id":"Bad ID","device":{"id":"x"}}`, "must match"},
		{"dup param", `{"id":"t","parameters":[{"name":"a","type":"string"},{"name":"a","type":"int"}],"device":{"id":"x"}}`, "duplicate parameter"},
		{"bad param name", `{"id":"t","parameters":[{"name":"a-b","type":"string"}],"device":{"id":"x"}}`, "must match"},
		{"bad type", `{"id":"t","parameters":[{"name":"a","type":"list"}],"device":{"id":"x"}}`, "unknown type"},
		{"default type", `{"id":"t","parameters":[{"name":"a","type":"int","default":"1"}],"device":{"id":"x"}}`, "default"},
		{"unknown placeholder", `{"id":"t","parameters":[],"device":{"id":"x-{nope}"}}`, "unknown parameter"},
		{"placeholder in key", `{"id":"t","parameters":[{"name":"a","type":"string"}],"device":{"{a}":"x"}}`, "placeholder in key"},
		{"no device", `{"id":"t","parameters":[]}`, "device is required"},
		{"device not object", `{"id":"t","device":[]}`, "must be a JSON object"},
		{"unknown field", `{"id":"t","paramters":[],"device":{"id":"x"}}`, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.raw))
			if err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestPlaceholders(t *testing.T) {
	d := parseMeter(t)
	names, err := d.Placeholders()
	if err != nil {
		t.Fatalf("placeholders: %v", err)
	}
	want := []string{"base", "enabled", "index", "ratio", "unitId"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestStringDefaultMustBeJSONString(t *testing.T) {
	_, err := Parse([]byte(`{
      "id": "t",
      "parameters": [{"name": "a", "type": "string", "default": 1}],
      "device": {"id": "x"}
    }`))
	if err == nil || !strings.Contains(err.Error(), "JSON string") {
		t.Fatalf("error = %v", err)
	}
}
