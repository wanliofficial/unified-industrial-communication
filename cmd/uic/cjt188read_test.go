package main

import (
	"testing"

	"uic/internal/codec/cjt188"
	"uic/internal/model"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：DI 写错一位、把
// 寄存器偏移当成字段偏移、给阀门塞一个字符串，都应在发出任何帧之前被拦住。

func TestParseMeterType(t *testing.T) {
	cases := map[string]byte{
		"10": 0x10, "0x20": 0x20, " 21 ": 0x21, "30": 0x30, "4a": 0x4A,
	}
	for in, want := range cases {
		got, err := parseMeterType(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q = %02X, want %02X", in, got, want)
		}
	}
	for _, bad := range []string{"", "AA", "0F", "50", "1", "ZZ"} {
		if _, err := parseMeterType(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestParseFieldList(t *testing.T) {
	got, err := parseFieldList("901F:0, 901F:5,901F:10,901F:17,901F:0", 0x10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 去重但保序：输出顺序要能对应命令行。
	if len(got) != 4 {
		t.Fatalf("got %d fields, want 4: %+v", len(got), got)
	}
	want := []struct {
		offset int
		name   string
		unit   string
		width  int
	}{
		{0, "当前累积流量", "m³", 5},
		{5, "结算日累积流量", "m³", 5},
		{10, "实时时间", "", 7},
		{17, "状态", "", 2},
	}
	for i, w := range want {
		if got[i].Offset != w.offset || got[i].Name != w.name ||
			got[i].Unit != w.unit || got[i].Length != w.width {
			t.Fatalf("field[%d] = %+v, want %+v", i, got[i], w)
		}
	}

	// 只给 DI 时默认取偏移 0。
	only, err := parseFieldList("901F", 0x10)
	if err != nil || len(only) != 1 || only[0].Offset != 0 {
		t.Fatalf("bare DI = %+v, %v", only, err)
	}

	// 热量表 901F 的偏移与水表不同：25 是供水温度。
	heat, err := parseFieldList("901F:25,901F:31", 0x20)
	if err != nil {
		t.Fatalf("heat: %v", err)
	}
	if heat[0].Name != "供水温度" || heat[0].Unit != "℃" {
		t.Fatalf("heat[0] = %+v", heat[0])
	}
	if heat[1].Name != "累积工作时间" || heat[1].Unit != "h" {
		t.Fatalf("heat[1] = %+v", heat[1])
	}

	// 上 1..12 月结算日量。
	hist, err := parseFieldList("D120:0,D12B:0", 0x10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if hist[0].Name != "上 1 月结算日累积流量" || hist[1].Name != "上 12 月结算日累积流量" {
		t.Fatalf("history = %+v", hist)
	}

	// 错误必须在发帧前暴露。
	bad := []string{
		"",           // 空
		"901",        // 不是 4 位
		"90G1",       // 非十六进制
		"901F:2",     // 把寄存器偏移的习惯带过来了
		"901F:0:4",   // 长度与 DI 定义（4 BCD + 单位 = 5）不符
		"901F:0:0",   // 非正长度
		"901F:x",     // 偏移不是整数
		"901F:0:5:6", // 字段过多
		"911F:0",     // 未收录的 DI
		"D12C:0",     // 只有 D120..D12B
		"A017:0",     // 阀门是写命令，没有可读字段
	}
	for _, s := range bad {
		if _, err := parseFieldList(s, 0x10); err == nil {
			t.Fatalf("%q must fail", s)
		}
	}
}

func TestCJT188PointRef(t *testing.T) {
	fields, err := parseFieldList("901F:17", 0x10)
	if err != nil {
		t.Fatal(err)
	}
	ref := cjt188PointRef(fields[0])
	if ref.DataType != model.TypeUint16 {
		t.Fatalf("status dataType = %s", ref.DataType)
	}
	if ref.Address.Area != "901F" || ref.Address.Offset != 17 || ref.Address.Bit != -1 {
		t.Fatalf("status address = %+v", ref.Address)
	}
	// 长度交给驱动按 schema 推导：点表里写死会让「同一 DI 换个字段」变成
	// 长度不符的假故障。
	if ref.Address.Length != 0 {
		t.Fatalf("length = %d, want 0", ref.Address.Length)
	}
	if ref.ID != "901F:17" {
		t.Fatalf("id = %q", ref.ID)
	}

	fields, _ = parseFieldList("901F:0", 0x10)
	ref = cjt188PointRef(fields[0])
	if ref.DataType != model.TypeFloat64 {
		t.Fatalf("volume dataType = %s", ref.DataType)
	}
}

func TestParseValve(t *testing.T) {
	for _, s := range []string{"open", "ON", "1", "开"} {
		v, err := parseValve(s)
		if err != nil || v != 1 {
			t.Fatalf("%q = %v, %v", s, v, err)
		}
	}
	for _, s := range []string{"close", "off", "0", "关"} {
		v, err := parseValve(s)
		if err != nil || v != 0 {
			t.Fatalf("%q = %v, %v", s, v, err)
		}
	}
	for _, bad := range []string{"", "2", "toggle", "-1"} {
		if _, err := parseValve(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestTrimNum(t *testing.T) {
	cases := map[float64]string{0: "0", 42: "42", 1234.56: "1234.56", 1.2345: "1.2345"}
	for in, want := range cases {
		if got := trimNum(in); got != want {
			t.Fatalf("%v = %q, want %q", in, got, want)
		}
	}
}

// TestCJT188FieldMetadata 确认命令行展示用的名称/单位与 codec 的 schema 一致，
// 避免 CLI 里另抄一份而漂移。
func TestCJT188FieldMetadata(t *testing.T) {
	di, _ := cjt188.ParseDI("901F")
	f, err := cjt188.LookupField(0x10, di, 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "当前累积流量" || f.Qty != cjt188.QtyVolume {
		t.Fatalf("field = %+v", f)
	}
}

// 写命令的报文标识与可读字段是两套：A017/A018 没有 schema，不能拿
// parseFieldList 去解析，否则会得到「未收录的数据标识」这种误导性错误。
func TestParseWriteDI(t *testing.T) {
	cases := map[string]cjt188.DI{
		"A017":   cjt188.DIValve,
		"a018":   cjt188.DIWriteAddr,
		"A017:0": cjt188.DIValve,
		" A018 ": cjt188.DIWriteAddr,
	}
	for in, want := range cases {
		got, err := parseWriteDI(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q = %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"A017,A018", "901F", "02010100", "A01", "ZZZZ", ""} {
		if _, err := parseWriteDI(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}
