package main

import (
	"strings"
	"testing"

	adscodec "uic/internal/codec/ads"
	"uic/internal/driver"
	"uic/internal/model"
)

// 与 snmpread 的自检用例一致：这里只测「命令行字符串 → 结构化声明/平台值」。
// 网络与句柄行为由 driver/ads 与 sim/ads 的用例覆盖，避免在 CLI 层再造
// 一个 AMS 客户端。

func TestParseADSSymbols(t *testing.T) {
	// 省略类型与长度：全部继承 -type。
	got, err := parseADSSymbols(" MAIN.nCounter , MAIN.nSetpoint ", "int16")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "MAIN.nCounter" || got[1].Name != "MAIN.nSetpoint" {
		t.Fatalf("symbols = %+v", got)
	}
	for _, s := range got {
		if s.Type != model.TypeInt16 || s.Length != 0 {
			t.Fatalf("%s = %+v", s.Name, s)
		}
	}

	// 逐个声明类型；string 带显式长度。
	got, err = parseADSSymbols("MAIN.nCounter:int16,MAIN.fTemperature:float32,MAIN.sRecipe:string:81,MAIN.raw:bytes:4", "uint16")
	if err != nil {
		t.Fatal(err)
	}
	want := []adsSymbol{
		{Name: "MAIN.nCounter", Type: model.TypeInt16},
		{Name: "MAIN.fTemperature", Type: model.TypeFloat32},
		{Name: "MAIN.sRecipe", Type: model.TypeString, Length: 81},
		{Name: "MAIN.raw", Type: model.TypeBytes, Length: 4},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("symbol[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// 空列表：只在使用读/写模式时才由调用方报错。
	if got, err := parseADSSymbols("  ", "int16"); err != nil || got != nil {
		t.Fatalf("empty = %+v %v", got, err)
	}

	bad := []struct{ raw, def, hint string }{
		{"MAIN.x", "nope", "未知默认类型"},
		{"MAIN.x:nope", "int16", "不合法"},
		{"MAIN.x:string:0", "int16", "1..65535"},
		{"MAIN.x:string:70000", "int16", "1..65535"},
		{"MAIN.x:int16:4", "int16", "只有 string/bytes"},
		{"MAIN.x:int16:4:5", "int16", "写法不合法"},
		{":int16", "int16", "不能为空"},
		{",", "int16", "没有有效符号"},
	}
	for _, tc := range bad {
		_, err := parseADSSymbols(tc.raw, tc.def)
		if err == nil {
			t.Fatalf("%q must fail", tc.raw)
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Fatalf("%q error = %v, want hint %q", tc.raw, err, tc.hint)
		}
	}
}

func TestParseADSControl(t *testing.T) {
	for raw, want := range map[string]adscodec.State{
		"run": adscodec.StateRun, " RUN ": adscodec.StateRun,
		"stop": adscodec.StateStop, "reset": adscodec.StateReset,
	} {
		got, err := parseADSControl(raw)
		if err != nil || got != want {
			t.Fatalf("parseADSControl(%q) = %v %v", raw, got, err)
		}
	}
	if _, err := parseADSControl("start"); err == nil || !strings.Contains(err.Error(), "run | stop | reset") {
		t.Fatalf("start: %v", err)
	}
}

func TestParseADSSetValue(t *testing.T) {
	ok := []struct {
		raw  string
		typ  model.DataType
		want model.Value
	}{
		{"1", model.TypeBool, model.BoolValue(true)},
		{"off", model.TypeBool, model.BoolValue(false)},
		{"开", model.TypeBool, model.BoolValue(true)},
		{"-42", model.TypeInt16, model.NumValue(model.TypeInt16, -42)},
		{"0x2a", model.TypeUint32, model.NumValue(model.TypeUint32, 42)},
		{"4294967295", model.TypeUint32, model.NumValue(model.TypeUint32, 4294967295)},
		{"-9223372036854775808", model.TypeInt64, model.NumValue(model.TypeInt64, -9223372036854775808)},
		{"23.5", model.TypeFloat32, model.NumValue(model.TypeFloat32, 23.5)},
		{"1450.75", model.TypeFloat64, model.NumValue(model.TypeFloat64, 1450.75)},
		{" hello ", model.TypeString, model.TextValue(model.TypeString, "hello")},
		{"raw", model.TypeBytes, model.TextValue(model.TypeBytes, "raw")},
	}
	for _, tc := range ok {
		got, err := parseADSSetValue(tc.raw, tc.typ)
		if err != nil {
			t.Fatalf("%s %q: %v", tc.typ, tc.raw, err)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("%s %q = %+v, want %+v", tc.typ, tc.raw, got, tc.want)
		}
	}

	bad := []struct {
		raw  string
		typ  model.DataType
		hint string
	}{
		{"2", model.TypeBool, "不合法"},
		{"70000", model.TypeUint16, "越界"},
		{"-1", model.TypeUint32, "不合法"},
		{"1.5", model.TypeInt32, "不合法"},
		{"abc", model.TypeInt16, "不合法"},
		{"abc", model.TypeFloat64, "不合法"},
		{"1", model.TypeBCD16, "BCD"},
		{"1", model.DataType("nope"), "未知写入类型"},
	}
	for _, tc := range bad {
		_, err := parseADSSetValue(tc.raw, tc.typ)
		if err == nil {
			t.Fatalf("%s %q must fail", tc.typ, tc.raw)
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Fatalf("%s %q error = %v, want hint %q", tc.typ, tc.raw, err, tc.hint)
		}
	}
}

func TestADSRow(t *testing.T) {
	r := adsRow{Symbol: "MAIN.nCounter", Type: "int16", Value: "1234", Quality: "good"}
	if got := r.display(); got != "1234" {
		t.Fatalf("display = %q", got)
	}
	r.Error = "boom"
	if got := r.display(); got != "错误: boom" {
		t.Fatalf("error display = %q", got)
	}

	sample := driver.RawSample{
		PointID: "MAIN.nCounter",
		Value:   model.NumValue(model.TypeInt16, 1234),
		Quality: model.QualityGood,
	}
	row := adsRowOf(sample)
	if row.Symbol != "MAIN.nCounter" || row.Type != "int16" || row.Value != "1234" || row.Quality != "good" {
		t.Fatalf("row = %+v", row)
	}

	// 错误样本：质量按错误分类，不沿用 RawSample 的 bad。
	cases := []struct {
		kind driver.ErrorKind
		want model.Quality
	}{
		{driver.ErrTimeout, model.QualityTimeout},
		{driver.ErrAddress, model.QualityAddressError},
		{driver.ErrDisconnected, model.QualityCommError},
		{driver.ErrPermission, model.QualityBad},
		{driver.ErrInternal, model.QualityBad},
	}
	for _, tc := range cases {
		err := driver.E(tc.kind, "read", "plc", "x", false, nil)
		row := adsRowOf(driver.RawSample{PointID: "p", Quality: model.QualityBad, Err: err})
		if row.Quality != string(tc.want) || row.Error == "" || row.Type != "" || row.Value != "" {
			t.Fatalf("%v row = %+v", tc.kind, row)
		}
		if got := adsQuality(err); got != tc.want {
			t.Fatalf("%v quality = %v", tc.kind, got)
		}
	}
}
