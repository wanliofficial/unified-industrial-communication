package textval

import (
	"reflect"
	"testing"

	"uic/internal/model"
)

func TestSelect(t *testing.T) {
	doc := []byte(`{"a":{"b":[10,20,{"c":"deep"}]},"flag":true,"n":3.5,"s":"hi","nul":null}`)
	cases := []struct {
		sel  string
		want string
	}{
		{"a.b[0]", "10"},
		{"a.b[2].c", "deep"},
		{"flag", "true"},
		{"n", "3.5"},
		{"s", "hi"},
	}
	for _, c := range cases {
		got, err := Select(doc, c.sel)
		if err != nil {
			t.Errorf("%q: %v", c.sel, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%q = %q, want %q", c.sel, got, c.want)
		}
	}

	// 对象作为叶子值：紧凑 JSON。
	got, err := Select(doc, "a.b[2]")
	if err != nil {
		t.Fatalf("object leaf: %v", err)
	}
	if string(got) != `{"c":"deep"}` {
		t.Errorf("object leaf = %q", got)
	}

	for _, bad := range []struct{ name, sel string }{
		{"empty selector", ""},
		{"missing field", "nope"},
		{"not object", "n.x"},
		{"index out of range", "a.b[9]"},
		{"not array", "a.b.c"},
		{"null leaf", "nul"},
	} {
		if _, err := Select(doc, bad.sel); err == nil {
			t.Errorf("%s: expected error", bad.name)
		}
	}
	if _, err := Select([]byte(`not json`), "a"); err == nil {
		t.Error("invalid JSON: expected error")
	}
}

func TestJSONValue(t *testing.T) {
	cases := map[string]string{
		`42`:      "42",
		`"text"`:  "text",
		`true`:    "true",
		`{"a":1}`: `{"a":1}`,
		`[1,2]`:   `[1,2]`,
		`3.5e2`:   "3.5e2",
	}
	for in, want := range cases {
		got, err := JSONValue([]byte(in))
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	if _, err := JSONValue([]byte(`null`)); err == nil {
		t.Error("null: expected error")
	}
	if _, err := JSONValue([]byte(`{`)); err == nil {
		t.Error("invalid JSON: expected error")
	}
}

func TestDecode(t *testing.T) {
	boolCases := map[string]bool{
		"true": true, "1": true, "on": true, "YES": true,
		"false": false, "0": false, "off": false, "No": false,
	}
	for in, want := range boolCases {
		v, err := Decode([]byte(in), model.TypeBool)
		if err != nil || v.Type != model.TypeBool || v.Bool != want {
			t.Errorf("Decode(%q,bool) = %+v err=%v", in, v, err)
		}
	}
	if _, err := Decode([]byte("maybe"), model.TypeBool); err == nil {
		t.Error("bool 'maybe': expected error")
	}

	v, err := Decode([]byte(" 42.5 "), model.TypeFloat32)
	if err != nil || v.Num != 42.5 || v.Type != model.TypeFloat32 {
		t.Fatalf("float32: %+v err=%v", v, err)
	}
	if _, err := Decode([]byte("70000"), model.TypeUint16); err == nil {
		t.Error("uint16 overflow: expected error")
	}
	if _, err := Decode([]byte(""), model.TypeFloat64); err == nil {
		t.Error("empty: expected error")
	}
	if _, err := Decode([]byte("abc"), model.TypeFloat64); err == nil {
		t.Error("non-numeric: expected error")
	}
	// 整数类型不接受小数，但 `42.0` 这类整值写法要能通过。
	if _, err := Decode([]byte("1.5"), model.TypeInt32); err == nil {
		t.Error("fractional int32: expected error")
	}
	if v, err := Decode([]byte("42.0"), model.TypeInt32); err != nil || v.Num != 42 {
		t.Errorf("42.0 int32 = %+v err=%v", v, err)
	}

	v, err = Decode([]byte(" RUN "), model.TypeString)
	if err != nil || v.Text != "RUN" || v.Type != model.TypeString {
		t.Fatalf("string: %+v err=%v", v, err)
	}

	// 未声明类型：按内容推断。
	for in, wantType := range map[string]model.DataType{
		"123": model.TypeInt64, "1.5": model.TypeFloat64, "xyz": model.TypeString,
	} {
		v, err := Decode([]byte(in), "")
		if err != nil || v.Type != wantType {
			t.Errorf("guess %q = %+v err=%v", in, v, err)
		}
	}
}

func TestEncode(t *testing.T) {
	cases := []struct {
		v    model.Value
		dt   model.DataType
		want string
	}{
		{model.BoolValue(true), model.TypeBool, "true"},
		{model.TextValue(model.TypeString, "RUN"), model.TypeString, "RUN"},
		{model.NumValue(model.TypeInt16, 42), model.TypeInt16, "42"},
		{model.NumValue(model.TypeFloat64, 42.5), model.TypeFloat64, "42.5"},
		{model.NumValue(model.TypeFloat32, 1e6), model.TypeFloat32, "1e+06"},
		{model.NumValue(model.TypeUint32, 7), "", "7"},
	}
	for _, c := range cases {
		got, err := Encode(c.v, c.dt)
		if err != nil {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Encode(%+v) = %q, want %q", c, got, c.want)
		}
	}
	if _, err := Encode(model.TextValue(model.TypeString, "x"), model.TypeFloat64); err == nil {
		t.Error("string as float: expected error")
	}
}

func TestCheckRange(t *testing.T) {
	ok := []struct {
		dt model.DataType
		n  float64
	}{
		{model.TypeInt16, -32768}, {model.TypeInt16, 32767},
		{model.TypeUint16, 65535}, {model.TypeInt32, -2147483648},
		{model.TypeUint32, 4294967295}, {model.TypeFloat32, 1e38},
		{model.TypeFloat64, 1e300}, {model.TypeString, 0},
	}
	for _, c := range ok {
		if err := CheckRange(c.dt, c.n); err != nil {
			t.Errorf("CheckRange(%s,%g) unexpected: %v", c.dt, c.n, err)
		}
	}
	bad := []struct {
		dt model.DataType
		n  float64
	}{
		{model.TypeInt16, 32768}, {model.TypeUint16, -1},
		{model.TypeInt32, 2147483648}, {model.TypeUint32, -1},
		{model.TypeFloat32, 1e39},
		{model.TypeInt32, 1.5}, {model.TypeUint64, 2.25},
		{model.TypeBCD16, 0.1},
	}
	for _, c := range bad {
		if err := CheckRange(c.dt, c.n); err == nil {
			t.Errorf("CheckRange(%s,%g): expected error", c.dt, c.n)
		}
	}
}

// TestSplitSelector 间接覆盖（splitSelector 未导出）：
// 用带空段与非法下标的 selector 检查语义。
func TestSplitSelector(t *testing.T) {
	doc := []byte(`{"a":{"b":["weird"]},"arr":[1,2]}`)
	// 空段被跳过：`a..b` 等价于 `a.b`。
	if got, err := Select(doc, "a..b[0]"); err != nil || !reflect.DeepEqual(string(got), "weird") {
		t.Fatalf("empty segment: %q err=%v", got, err)
	}
	// 下标不是数字时不当作下标，键名保留方括号（字段名允许方括号）。
	if _, err := Select(doc, "arr[x]"); err == nil {
		t.Error("arr[x]: expected error (not an array index, field missing)")
	}
}
