package main

import (
	"strings"
	"testing"

	"uic/internal/model"
)

// 与其它 *read 的自检用例一致：这里只测「命令行字符串 → 平台值 / 校验」。
// 网络行为由 driver/coap 与 sim/coap 的用例覆盖，不在 CLI 层再造一个客户端。

func TestCoAPCLIValue(t *testing.T) {
	ok := []struct {
		raw  string
		typ  string
		want model.Value
	}{
		{"on", "bool", model.BoolValue(true)},
		{"OFF", "bool", model.BoolValue(false)},
		{"-42", "int16", model.NumValue(model.TypeInt16, -42)},
		{"42", "uint16", model.NumValue(model.TypeUint16, 42)},
		{"23.5", "float32", model.NumValue(model.TypeFloat32, 23.5)},
		{"1450.75", "float64", model.NumValue(model.TypeFloat64, 1450.75)},
		{" manual ", "string", model.TextValue(model.TypeString, "manual")},
		// bytes 按字面字节，不经过文本解析：\x00 就是一个字节。
		{"\x00\x01abc", "bytes", model.BytesValue([]byte("\x00\x01abc"))},
	}
	for _, tc := range ok {
		got, err := coapCLIValue(tc.raw, tc.typ)
		if err != nil {
			t.Fatalf("%s %q: %v", tc.typ, tc.raw, err)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("%s %q = %+v, want %+v", tc.typ, tc.raw, got, tc.want)
		}
	}

	bad := []struct {
		raw  string
		typ  string
		hint string
	}{
		{"maybe", "bool", "cannot parse"},
		{"70000", "uint16", "out of range"},
		{"-1", "uint32", "out of range"},
		{"1.5", "int32", "not an integer"},
		{"abc", "float64", "cannot parse"},
		{"1", "nope", "unknown data type"},
	}
	for _, tc := range bad {
		_, err := coapCLIValue(tc.raw, tc.typ)
		if err == nil {
			t.Fatalf("%s %q must fail", tc.typ, tc.raw)
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Fatalf("%s %q error = %v, want hint %q", tc.typ, tc.raw, err, tc.hint)
		}
	}
}

func TestValidCoAPType(t *testing.T) {
	for _, typ := range []string{"bool", "int16", "uint16", "int32", "uint32",
		"float32", "float64", "string", "bytes"} {
		if !validCoAPType(typ) {
			t.Errorf("%s: want valid", typ)
		}
	}
	for _, typ := range []string{"", "int64", "bcd16", "nope"} {
		if validCoAPType(typ) {
			t.Errorf("%s: want invalid", typ)
		}
	}
}

func TestValidCoAPBlock(t *testing.T) {
	for _, n := range []int{16, 32, 64, 128, 256, 512, 1024} {
		if !validCoAPBlock(n) {
			t.Errorf("%d: want valid", n)
		}
	}
	for _, n := range []int{0, 8, 15, 100, 2048, -16} {
		if validCoAPBlock(n) {
			t.Errorf("%d: want invalid", n)
		}
	}
}

func TestNormalizeCoAPFormat(t *testing.T) {
	cases := map[string]string{
		"text":                     "text",
		"TEXT/PLAIN":               "text",
		"json":                     "json",
		" Application/JSON ":       "json",
		"octets":                   "octets",
		"binary":                   "octets",
		"application/octet-stream": "octets",
		"xml":                      "text",
		"":                         "text",
	}
	for raw, want := range cases {
		if got := normalizeCoAPFormat(raw); got != want {
			t.Errorf("normalizeCoAPFormat(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCoAPValueText(t *testing.T) {
	cases := []struct {
		v    model.Value
		raw  bool
		want string
	}{
		{model.BoolValue(true), false, "true"},
		{model.TextValue(model.TypeString, "manual"), false, "manual"},
		{model.BytesValue([]byte{0x00, 0x01, 0xff}), false, `"\x00\x01\xff"`},
		{model.BytesValue([]byte{0x00, 0x01, 0xff}), true, "0001ff"},
		{model.NumValue(model.TypeInt16, -42), false, "-42"},
		{model.NumValue(model.TypeFloat64, 21.5), false, "21.5"},
	}
	for _, tc := range cases {
		if got := coapValueText(tc.v, tc.raw); got != tc.want {
			t.Errorf("%+v raw=%v = %q, want %q", tc.v, tc.raw, got, tc.want)
		}
	}
}
