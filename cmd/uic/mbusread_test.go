package main

import (
	"testing"

	mb "uic/internal/codec/mbus"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：主地址写成 253
// （次级地址）、范围倒着写、记录地址里带空格，都应在发出任何帧之前被拦住。

func TestParsePrimaryAddr(t *testing.T) {
	cases := map[string]byte{
		"0": 0, "1": 1, "250": 250, " 42 ": 42, "0x0A": 0x0A, "0xfa": 250,
	}
	for in, want := range cases {
		got, err := parsePrimaryAddr(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q = %d，应为 %d", in, got, want)
		}
	}
	// 251..255 都是保留/特殊地址，必须明确拒绝并说明原因。
	bads := []string{"", "abc", "251", "252", "253", "254", "255", "256", "-1", "1.5"}
	for _, in := range bads {
		if _, err := parsePrimaryAddr(in); err == nil {
			t.Fatalf("%q 应被拒绝", in)
		}
	}
	// 错误信息必须说清「这是次级地址/广播」，而不是只说"越界"。
	if _, err := parsePrimaryAddr("253"); err == nil {
		t.Fatal("次级地址应被拒绝")
	} else if want := "次级"; !contains(err.Error(), want) {
		t.Fatalf("次级地址的报错应提到 %q，得到 %v", want, err)
	}
	if _, err := parsePrimaryAddr("255"); err == nil {
		t.Fatal("广播地址应被拒绝")
	} else if want := "广播"; !contains(err.Error(), want) {
		t.Fatalf("广播地址的报错应提到 %q，得到 %v", want, err)
	}
}

func TestParseScanRange(t *testing.T) {
	lo, hi, err := parseScanRange("1-20")
	if err != nil || lo != 1 || hi != 20 {
		t.Fatalf("1-20 → %d..%d,%v", lo, hi, err)
	}
	lo, hi, err = parseScanRange("5")
	if err != nil || lo != 5 || hi != 5 {
		t.Fatalf("5 → %d..%d,%v", lo, hi, err)
	}
	lo, hi, err = parseScanRange(" 0 - 250 ")
	if err != nil || lo != 0 || hi != 250 {
		t.Fatalf("0-250 → %d..%d,%v", lo, hi, err)
	}
	for _, bad := range []string{"20-1", "1-253", "", "a-b", "1-", "-5"} {
		if _, _, err := parseScanRange(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestMBusRefs(t *testing.T) {
	got := mbusRefs("q:energy, q:volume@1 ,,r:0")
	want := []string{"q:energy", "q:volume@1", "r:0"}
	if len(got) != len(want) {
		t.Fatalf("got %v，应为 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v，应为 %v", got, want)
		}
	}
	if len(mbusRefs("  ,  ")) != 0 {
		t.Fatal("只有空白时应返回空列表")
	}
}

func TestRowOfMakesCopyableAddress(t *testing.T) {
	base := map[string]int{}
	rec := mb.Record{
		Index: 0, Quantity: "energy", Unit: "Wh", Kind: mb.KindNumeric,
		Num: 1234.5, RawInt: 12345, Scale: 1e-1, DIF: 0x04, VIF: 0x03,
	}
	row := rowOf(rec, base)
	if row.Ref != "q:energy" {
		t.Fatalf("ref = %q", row.Ref)
	}
	if row.Value != "1234.5 Wh" {
		t.Fatalf("value = %q", row.Value)
	}
	// 同名记录第二次出现要带 /1，否则两条点位指向同一条记录。
	rec.Storage = 2
	row = rowOf(rec, base)
	if row.Ref != "q:energy@2/1" {
		t.Fatalf("ref = %q，应为 q:energy@2/1", row.Ref)
	}
	// 文本记录：时间无效时给出明确说明而不是空单元格。
	rec = mb.Record{Quantity: "date-time", Kind: mb.KindText}
	row = rowOf(rec, map[string]int{})
	if row.Value != "(时间无效)" {
		t.Fatalf("无效时间的 value = %q", row.Value)
	}
	// 字节记录：按十六进制导出，供 r: 逃生口核对。
	rec = mb.Record{Quantity: "vendor-specific", Kind: mb.KindBinary, Raw: []byte{0xAA, 0xBB}}
	row = rowOf(rec, map[string]int{})
	if row.Value != "AA BB" {
		t.Fatalf("二进制 value = %q", row.Value)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
