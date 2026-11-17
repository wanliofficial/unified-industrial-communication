package main

import (
	"errors"
	"strings"
	"testing"

	"uic/internal/model"
)

// 自检命令的纯函数部分：只测「命令行字符串 → 平台值」。网络行为由
// driver 与 sim 的用例覆盖，这里不重复造一个 UDP 客户端。
func TestParseSNMPSetValue(t *testing.T) {
	ok := []struct {
		raw  string
		typ  string
		want model.Value
	}{
		{"1", "bool", model.BoolValue(true)},
		{"off", "bool", model.BoolValue(false)},
		{"开", "bool", model.BoolValue(true)},
		{"-42", "int16", model.NumValue(model.TypeInt16, -42)},
		{"0x2a", "uint32", model.NumValue(model.TypeUint32, 42)},
		{"4294967295", "uint32", model.NumValue(model.TypeUint32, 4294967295)},
		{"-9223372036854775808", "int64", model.NumValue(model.TypeInt64, -9223372036854775808)},
		{" hello ", "string", model.TextValue(model.TypeString, "hello")},
		{"raw", "bytes", model.TextValue(model.TypeBytes, "raw")},
	}
	for _, tc := range ok {
		got, err := parseSNMPSetValue(tc.raw, tc.typ)
		if err != nil {
			t.Fatalf("%s %q: %v", tc.typ, tc.raw, err)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("%s %q = %+v, want %+v", tc.typ, tc.raw, got, tc.want)
		}
	}

	bad := []struct{ raw, typ, hint string }{
		{"2", "bool", "不合法"},
		{"70000", "uint16", "越界"},
		{"-1", "uint32", "不合法"},
		{"1.5", "int32", "不合法"},
		{"abc", "int16", "不合法"},
		{"1.5", "float64", "浮点"},
		{"1", "bcd16", "BCD"},
		{"1", "nope", "未知写入类型"},
	}
	for _, tc := range bad {
		_, err := parseSNMPSetValue(tc.raw, tc.typ)
		if err == nil {
			t.Fatalf("%s %q must fail", tc.typ, tc.raw)
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Fatalf("error = %v, want hint %q", err, tc.hint)
		}
	}
}

func TestSNMPRowDisplay(t *testing.T) {
	r := snmpRow{OID: "1.3.6.1.2.1.1.1.0", Type: "OCTET STRING", Value: "hello"}
	if got := r.display(); got != "hello" {
		t.Fatalf("display = %q", got)
	}
	r.Hex = "68656c6c6f"
	if got := r.display(); got != "hello  (0x68656c6c6f)" {
		t.Fatalf("raw display = %q", got)
	}
	r.Error = "boom"
	if got := r.display(); got != "错误: boom" {
		t.Fatalf("error display = %q", got)
	}
}

// TestSNMPReadFailed 钉住自检读的退出码判据。
//
// 三档必须分清，混在一起会让脚本把两类完全不同的故障当成同一种：
//
//   - 全部失败（okCount==0）→ 非零：设备一个 OID 都不认，采集等于没做；
//   - 部分成功 + 链路错误 → 非零：后半段根本没读，这一轮不完整；
//   - 部分成功 + 无链路错误 → 零：§3.3.1 的部分失败语义。
//
// 「有错误就非零」是容易漏的那一档：只看 okCount 会把「读到 3 个点后链路
// 断开」判成成功，脚本随后拿这 3 个点当全量结果。
func TestSNMPReadFailed(t *testing.T) {
	linkErr := errors.New("dev1 read timeout")
	cases := []struct {
		name    string
		okCount int
		err     error
		want    bool
	}{
		{"all good", 3, nil, false},
		{"partial without link error", 1, nil, false},
		{"all failed", 0, nil, true},
		{"partial with link error", 2, linkErr, true},
		{"all failed with link error", 0, linkErr, true},
	}
	for _, tc := range cases {
		if got := snmpReadFailed(tc.okCount, tc.err); got != tc.want {
			t.Errorf("%s: snmpReadFailed(%d, %v) = %v, want %v",
				tc.name, tc.okCount, tc.err, got, tc.want)
		}
	}
}
