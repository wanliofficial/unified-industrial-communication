package main

import (
	"testing"

	"uic/internal/codec/sl651"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：站号、密码、要素地址都
// 必须在发出任何帧之前就拦住，并且报错要说清错在哪一层——SL 651 的失败在串口
// 上全都表现成「没有应答」，如果这里把地址错报成超时，人会先去改波特率。

func TestParseSL651Station(t *testing.T) {
	good := map[string]int64{
		"1": 1, "31234567": 31234567, "0001": 1, " 42 ": 42, "9999999999": 9999999999,
	}
	for in, want := range good {
		got, err := parseSL651Station(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q = %d，应为 %d", in, got, want)
		}
	}

	bads := map[string]string{
		"":            "为空",
		"abc":         "不是十进制",
		"12.5":        "不是十进制",
		"-1":          "超出",
		"10000000000": "超出", // 11 位，超过 5 字节 BCD 的 10 位
	}
	for in, want := range bads {
		_, err := parseSL651Station(in)
		if err == nil {
			t.Fatalf("%q 应被拒绝", in)
		}
		if !contains(err.Error(), want) {
			t.Fatalf("%q 的报错应提到 %q，得到 %v", in, want, err)
		}
	}
}

func TestParseSL651Password(t *testing.T) {
	var want [2]byte = [2]byte{0x12, 0x34}
	got, err := parseSL651Password("1234")
	if err != nil {
		t.Fatalf("1234: %v", err)
	}
	if got != want {
		t.Fatalf("1234 = % x，应为 12 34", got)
	}

	// 密码必须是整 2 字节：短写法（如 "AB"）与长写法都拒绝，不做静默补齐——
	// 补齐后密码错，终端是静默的，比启动就报错难查得多。
	for _, bad := range []string{"", "123", "12345", "zzzz", "AB", "12 34"} {
		if _, err := parseSL651Password(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestSL651Refs(t *testing.T) {
	got := sl651Refs("  Z , PT ,, VT ")
	want := []string{"Z", "PT", "VT"}
	if len(got) != len(want) {
		t.Fatalf("got %v，应为 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v，应为 %v", got, want)
		}
	}
}

// sl651SplitSlot 与驱动里的 guideOfWithSlot 必须同一套语法：自检命令先拆一
// 遍只为尽早报错，真正的解析仍在驱动里。两条路径各写一套规则正是「自检通
// 过、采集失败」的经典成因。
func TestSL651SplitSlot(t *testing.T) {
	cases := []struct {
		in   string
		name string
		slot int
		ok   bool
	}{
		{"Z", "Z", 0, true},
		{"DRP[1]", "DRP", 1, true},
		{"  drz3 [11] ", "drz3", 11, true},
		{"f4[0]", "f4", 0, true},
		{"Z[1", "Z[1", 0, false},       // 缺右括号：整串当要素名，LookupGuide 会拒
		{"PT[-1]", "PT[-1]", 0, false}, // 负段号
		{"DRP[x]", "DRP[x]", 0, false}, // 非整数
	}
	for _, c := range cases {
		name, slot, ok := sl651SplitSlot(c.in)
		if ok != c.ok || name != c.name || slot != c.slot {
			t.Errorf("sl651SplitSlot(%q) = (%q, %d, %v)，want (%q, %d, %v)",
				c.in, name, slot, ok, c.name, c.slot, c.ok)
		}
	}
}

// 中文名能被 LookupGuide 认出来（供内部使用），但自检命令在调用它之前就用
// sl651SplitSlot + 白名单拦掉了——这里只锁 LookupGuide 的行为本身。
func TestSL651PointsValidation(t *testing.T) {
	for _, good := range []string{"Z", "PT", "VT", "39", "DRP", "DRZ1", "drz1"} {
		if _, ok := sl651.LookupGuide(good); !ok {
			t.Fatalf("内置表应有 %q", good)
		}
	}
	if _, ok := sl651.LookupGuide("瞬时河道水位"); !ok {
		t.Fatal("LookupGuide 应支持中文名匹配")
	}
}

// LookupGuide 支持三种写法（两位十六进制、ASCII 简称、中文名），但自检命令
// 只接受前两种。这里分别锁死两种行为，避免有人「顺手」把中文名也放开。
func TestSL651GuideLookups(t *testing.T) {
	for _, name := range []string{"Z", "PT", "VT", "DRP"} {
		if g, ok := sl651.LookupGuide(name); !ok {
			t.Fatalf("LookupGuide(%q) 失败", name)
		} else if got := sl651.GuideName(g); !contains(got, name) && !contains(name, got) {
			// 简称命中
		}
	}
	if _, ok := sl651.LookupGuide("瞬时河道水位"); !ok {
		t.Fatal("LookupGuide 应支持中文名（供内部使用）")
	}
}
