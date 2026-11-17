package main

import (
	"errors"
	"flag"
	"testing"

	dlt645codec "uic/internal/codec/dlt645"
	"uic/internal/model"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：写错一个 DI、把时间
// 写成 2026-13-01、或者给数值项塞一个 hex 串，都应当在发出任何帧之前就被
// 拦住。这些用例只覆盖纯函数，不碰网络。

func TestParseDIList(t *testing.T) {
	got, err := parseDIList("02010100, 00000000,02010100,0201ff00")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 去重但保序：输出顺序要能对应命令行，否则第一次看到「为什么少了一行」
	// 的人会以为是读取失败。
	want := []string{"02010100", "00000000", "0201FF00"}
	if len(got) != len(want) {
		t.Fatalf("got %d DI, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("DI[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	if _, err := parseDIList("  ,  "); err == nil {
		t.Fatal("empty list must fail")
	}
	if _, err := parseDIList("020101"); err == nil {
		t.Fatal("short DI must fail")
	}
	if _, err := parseDIList("0201010Z"); err == nil {
		t.Fatal("non-hex DI must fail")
	}
	if _, err := parseDIList("FFFFFFFF"); err == nil {
		t.Fatal("all-FF DI must fail")
	}
}

func TestParseCompactTime(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"20261107123456", 20261107123456},
		{"2026-11-07 12:34:56", 20261107123456},
		{"2026-11-07T12:34:56", 20261107123456},
		{"2026-11-07 12:34", 20261107123400},
		{"2026-11-07", 20261107000000},
	}
	for _, tc := range cases {
		got, err := parseCompactTime(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "2026/11/07 12:00:00", "20261307120000", "12:00"} {
		if _, err := parseCompactTime(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestDLT645Value(t *testing.T) {
	timeItem, _ := dlt645codec.Lookup(mustDIQuiet("04000101"))
	v, err := dlt645Value(timeItem, "2026-11-07 12:00:00")
	if err != nil {
		t.Fatalf("time: %v", err)
	}
	if n, ok := v.AsFloat(); !ok || n != 20261107120000 {
		t.Fatalf("time value = %+v", v)
	}

	// 地址类数据项接受缩位写法，但落到平台上必须是完整的 12 位地址——
	// 否则写进电表的会是一个「看起来短了」的地址。
	addrItem, _ := dlt645codec.Lookup(mustDIQuiet("04000401"))
	v, err = dlt645Value(addrItem, "000000000042")
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	if v.Type != model.TypeString || v.Text != "000000000042" {
		t.Fatalf("addr value = %+v", v)
	}
	// 写地址不接受缩位：`42` 展开成 AAAAAAAAAA42，写进去就再也点不到这块表。
	if _, err := dlt645Value(addrItem, "42"); err == nil {
		t.Fatal("short address must not be writable")
	}

	volt, _ := dlt645codec.Lookup(mustDIQuiet("02010100"))
	v, err = dlt645Value(volt, "220.5")
	if err != nil {
		t.Fatalf("number: %v", err)
	}
	if n, ok := v.AsFloat(); !ok || n != 220.5 {
		t.Fatalf("number value = %+v", v)
	}
	if _, err := dlt645Value(volt, "abc"); err == nil {
		t.Fatal("garbage number must fail")
	}

	// 4 字节日期数据项没有年：读出来是 8 位 MMDDhhmm，写回必须接受同形值，
	// 否则「把刚读到的结算日原样写回」会得到一个月 = 0 的垃圾日期。
	dateItem, _ := dlt645codec.Lookup(mustDIQuiet("04000B01"))
	v, err = dlt645Value(dateItem, "11071200")
	if err != nil {
		t.Fatalf("date: %v", err)
	}
	if n, ok := v.AsFloat(); !ok || n != 11071200 {
		t.Fatalf("date value = %+v", v)
	}
	v, err = dlt645Value(dateItem, "2026-12-31 23:59")
	if err != nil {
		t.Fatalf("full date: %v", err)
	}
	if n, ok := v.AsFloat(); !ok || n != 20261231235900 {
		t.Fatalf("full date value = %+v", v)
	}
	for _, bad := range []string{"13320000", "11320000", "11072400"} {
		if _, err := dlt645Value(dateItem, bad); err == nil {
			t.Fatalf("date %q must fail", bad)
		}
	}
}

func TestDLT645PointRef(t *testing.T) {
	volt := dlt645PointRef(mustDIQuiet("02010100"))
	if volt.DataType != model.TypeFloat64 {
		t.Fatalf("voltage dataType = %s", volt.DataType)
	}
	if volt.Address.Area != "02010100" || volt.Address.Offset != 0 || volt.Address.Bit != -1 {
		t.Fatalf("voltage address = %+v", volt.Address)
	}
	if volt.Address.Length != 2 {
		t.Fatalf("voltage length = %d, want 2", volt.Address.Length)
	}

	// 数据块没有单一数值含义，必须用 bytes 承载；硬解成 float64 会给出
	// 一个看起来像读数、实际是前 8 个字节的假值。
	block := dlt645PointRef(mustDIQuiet("0201FF00"))
	if block.DataType != model.TypeBytes {
		t.Fatalf("block dataType = %s", block.DataType)
	}
	if block.Address.Length != 0 {
		t.Fatalf("block length = %d, want 0 (由应答决定)", block.Address.Length)
	}
}

// 传输方式按「用户给了哪组参数」推断：只给 -addr 时必须是 tcp，否则自检
// 会先撞一次 /dev/ttyUSB0: no such file，看起来像适配器没插。
func TestDLT645TransportInference(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-addr", "127.0.0.1:14001"}, "tcp"},
		{[]string{"-port", "/dev/ttyUSB0"}, "serial"},
		{nil, "serial"},
		{[]string{"-addr", "127.0.0.1:14001", "-transport", "serial"}, "serial"},
		{[]string{"-transport", "tcp"}, "tcp"},
	}
	for _, tc := range cases {
		fs := flag.NewFlagSet("dlt645read", flag.ContinueOnError)
		tr := fs.String("transport", "serial", "")
		fs.String("addr", "127.0.0.1:4001", "")
		fs.String("port", "/dev/ttyUSB0", "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		got := inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
		if got != tc.want {
			t.Fatalf("%v -> %q, want %q", tc.args, got, tc.want)
		}
	}
}

// dlt645ReadFailed 决定自检读的退出码：全部失败或出现链路级错误必须非零，
// 部分成功仍是 0（docs §3.3.1）。全失败返回 0 时脚本会把「表不认这些 DI」
// 当成成功。
func TestDLT645ReadFailed(t *testing.T) {
	linkErr := errors.New("read timeout")
	cases := []struct {
		ok   int
		err  error
		want bool
	}{
		{0, nil, true},
		{0, linkErr, true},
		{2, linkErr, true},
		{1, nil, false},
		{3, nil, false},
	}
	for _, tc := range cases {
		if got := dlt645ReadFailed(tc.ok, tc.err); got != tc.want {
			t.Fatalf("ok=%d err=%v -> %v, want %v", tc.ok, tc.err, got, tc.want)
		}
	}
}
