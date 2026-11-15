package textval

import (
	"math"
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

// TestCheckRangeWideIntegers 钉住 64 位整型的边界。
//
// 这两个分支最容易漏：`int16` 的越界早就有测试，64 位整型却被当成「反正
// float64 装得下」。实际后果是 `1e30` 被接受，写回设备时 `int64(1e30)`
// 回绕成 `-9223372036854775808`——一个完全错误的数，而且没有任何告警。
func TestCheckRangeWideIntegers(t *testing.T) {
	ok := []struct {
		dt model.DataType
		n  float64
	}{
		{model.TypeInt64, -9223372036854775808}, // -2^63 是精确可表示的
		{model.TypeInt64, 0},
		{model.TypeInt64, 9007199254740992}, // 2^53
		{model.TypeUint64, 0},
		{model.TypeUint64, 18446744073709549568}, // 2^64 - 2048，float64 可精确表示
		{model.TypeFloat64, 1e300},
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
		{model.TypeInt64, 9223372036854775808}, // 2^63，已超出
		{model.TypeInt64, -1.8446744073709552e19},
		{model.TypeInt64, 1e30},
		{model.TypeUint64, -1},
		{model.TypeUint64, 18446744073709551616}, // 2^64
		{model.TypeUint64, 1e30},
	}
	for _, c := range bad {
		if err := CheckRange(c.dt, c.n); err == nil {
			t.Errorf("CheckRange(%s,%g): expected error", c.dt, c.n)
		}
	}
}

// TestCheckRangeRejectsNonFinite：NaN/±Inf 必须判坏。
//
// `strconv.ParseFloat("NaN")` / `("Inf")` 都不报错，所以这条只能靠
// CheckRange 挡住；放过去的话，JSON 编码会失败、比较永远为假。
func TestCheckRangeRejectsNonFinite(t *testing.T) {
	inf := math.Inf(1)
	for _, dt := range []model.DataType{
		model.TypeInt16, model.TypeUint32, model.TypeInt64, model.TypeUint64,
		model.TypeFloat32, model.TypeFloat64,
	} {
		for _, n := range []float64{math.NaN(), inf, -inf} {
			if err := CheckRange(dt, n); err == nil {
				t.Errorf("CheckRange(%s,%v): expected error", dt, n)
			}
		}
	}
	// 非数值类型没有范围可校验，保持通过。
	if err := CheckRange(model.TypeString, 0); err != nil {
		t.Errorf("CheckRange(string): %v", err)
	}
}

// TestEncodeRejectsUnrepresentable 钉住写路径不静默改值。
func TestEncodeRejectsUnrepresentable(t *testing.T) {
	bad := []struct {
		name string
		v    model.Value
		dt   model.DataType
	}{
		{"int16 over", model.NumValue(model.TypeInt16, 40000), model.TypeInt16},
		{"int16 under", model.NumValue(model.TypeInt16, -40000), model.TypeInt16},
		{"int32 over", model.NumValue(model.TypeInt32, 3e9), model.TypeInt32},
		{"int32 fraction", model.NumValue(model.TypeInt32, 1.5), model.TypeInt32},
		{"uint16 negative", model.NumValue(model.TypeUint16, -1), model.TypeUint16},
		{"int64 overflow", model.NumValue(model.TypeInt64, 1e30), model.TypeInt64},
		{"uint64 overflow", model.NumValue(model.TypeUint64, 1e30), model.TypeUint64},
		{"float32 over", model.NumValue(model.TypeFloat32, 1e39), model.TypeFloat32},
		{"float64 nan", model.NumValue(model.TypeFloat64, math.NaN()), model.TypeFloat64},
		{"unknown type", model.NumValue(model.TypeInt32, 1), model.DataType("float128")},
	}
	for _, c := range bad {
		if b, err := Encode(c.v, c.dt); err == nil {
			t.Errorf("%s: Encode = %q, want error", c.name, b)
		}
	}

	// 上界附近仍要正常输出，且 uint64 不能变成负数。
	if b, err := Encode(model.NumValue(model.TypeUint64, 18000000000000000000), model.TypeUint64); err != nil {
		t.Fatalf("uint64 high: %v", err)
	} else if string(b) != "18000000000000000000" {
		t.Fatalf("uint64 high = %q", b)
	}
	if b, err := Encode(model.NumValue(model.TypeInt64, -9223372036854775808), model.TypeInt64); err != nil {
		t.Fatalf("int64 low: %v", err)
	} else if string(b) != "-9223372036854775808" {
		t.Fatalf("int64 low = %q", b)
	}
}

// TestGuessKeepsInexactIntegers：推断不出精确数值时保留文本，不回绕。
func TestGuessKeepsInexactIntegers(t *testing.T) {
	for _, s := range []string{"9223372036854775807", "9007199254740993"} {
		v, err := Guess(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if v.Type != model.TypeString || v.Text != s {
			t.Errorf("Guess(%q) = %+v, want text", s, v)
		}
	}
	// 精确可表示的整数仍是数值。
	v, err := Guess("9007199254740992")
	if err != nil || v.Type != model.TypeInt64 || v.Num != 9007199254740992 {
		t.Fatalf("Guess(2^53) = %+v err=%v", v, err)
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
