package mc

import (
	"encoding/binary"
	"strings"
	"testing"
)

// 金帧测试：3E 请求的固定头 9 字节 + 命令/子命令/设备码/3 字节起始号/2 字节点数。
// 这些字段错了 PLC 直接拒绝连接或返回 0xC050，因此逐字节钉死。

func hex(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0F]
	}
	return string(out)
}

func TestBuildReadGolden(t *testing.T) {
	// 读 D100 起 2 个字：设备码 A8、起始号 3 字节小端（64 00 00）、点数 02 00。
	frame, err := BuildRead(0xA8, 100, 2, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := "500000FFFF03000C00100001040000A86400000200"
	if got := hex(frame); got != want {
		t.Fatalf("read frame = %s, want %s", got, want)
	}
	if len(frame) != 9+12 {
		t.Fatalf("frame len = %d, want 21", len(frame))
	}
}

func TestBuildReadBitGolden(t *testing.T) {
	// 读 M0 起 16 个位：子命令 01 00（位），点数 10 00（16）。
	frame, err := BuildRead(0x90, 0, 16, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := "500000FFFF03000C00100001040100900000001000"
	if got := hex(frame); got != want {
		t.Fatalf("bit read frame = %s, want %s", got, want)
	}
}

func TestBuildWriteGolden(t *testing.T) {
	// 写 D0 一个字，值 0x1234：字单位 2 字节（34 12 小端）。
	data := []byte{0x34, 0x12}
	frame, err := BuildWrite(0xA8, 0, 1, false, data)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := "500000FFFF03000E00100001140000A800000001003412"
	if got := hex(frame); got != want {
		t.Fatalf("write frame = %s, want %s", got, want)
	}
}

func TestBuildWriteBitPacks(t *testing.T) {
	// 位写：M0=1, M1=0, M2=1 → 第一个字节 0x05（bit0 与 bit2）。
	raw := PackBits([]bool{true, false, true})
	frame, err := BuildWrite(0x90, 0, 3, true, raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 命令 1401、子命令 0100（位）、数据 05。
	if frame[len(frame)-1] != 0x05 {
		t.Fatalf("packed byte = %#x, want 0x05", frame[len(frame)-1])
	}
	if _, err := BuildWrite(0xA8, 0, 2, false, []byte{1, 2, 3}); err == nil {
		t.Fatal("word write with wrong data length must fail")
	}
}

func TestBuildValidation(t *testing.T) {
	cases := []struct {
		name    string
		build   func() ([]byte, error)
		wantSub string
	}{
		{"negative head", func() ([]byte, error) { return BuildRead(0xA8, -1, 1, false) }, "head device"},
		{"head overflow", func() ([]byte, error) { return BuildRead(0xA8, 1<<24, 1, false) }, "out of range"},
		{"zero points", func() ([]byte, error) { return BuildRead(0xA8, 0, 0, false) }, "points"},
		{"too many words", func() ([]byte, error) { return BuildRead(0xA8, 0, maxWordPoints+1, false) }, "points"},
		{"too many bits", func() ([]byte, error) { return BuildRead(0x90, 0, maxBitPoints+1, true) }, "points"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.build()
			if err == nil {
				t.Fatalf("want error containing %q", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error = %q, want containing %q", err, c.wantSub)
			}
		})
	}
}

func TestParseResponse(t *testing.T) {
	// 正常响应：D0 一个字 0x1234，结束码 0000。
	frame := BuildResponse(0x0000, []byte{0x34, 0x12})
	got, err := ParseResponse(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != 0x34 || got[1] != 0x12 {
		t.Fatalf("data = % x", got)
	}
}

func TestParseResponseEndCode(t *testing.T) {
	frame := BuildResponse(0xC052, nil)
	_, err := ParseResponse(frame)
	if err == nil {
		t.Fatal("want end code error")
	}
	ec, ok := err.(*EndCodeError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if ec.Code != 0xC052 {
		t.Fatalf("code = %#x", ec.Code)
	}
	if !strings.Contains(ec.Error(), "head device invalid") {
		t.Fatalf("error text = %q", ec.Error())
	}
}

func TestParseResponseErrors(t *testing.T) {
	cases := map[string][]byte{
		"too short": {0x50, 0x00},
		"bad header": func() []byte {
			b := BuildResponse(0, nil)
			b[0] = 0x51
			return b
		}(),
		"length mismatch": func() []byte {
			b := BuildResponse(0, []byte{1, 2, 3})
			binary.LittleEndian.PutUint16(b[7:], 2) // 声明 2 字节但后面有 3
			return b
		}(),
		"zero length": func() []byte {
			b := BuildResponse(0, nil)
			binary.LittleEndian.PutUint16(b[7:], 1)
			return b
		}(),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseResponse(raw); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestPackBits(t *testing.T) {
	got := PackBits([]bool{true, false, true, true})
	if len(got) != 1 || got[0] != 0b00001101 {
		t.Fatalf("packed = % x, want 0d", got)
	}
	out := UnpackBits([]byte{0b10110000}, 4)
	// 低 4 位是 0000，高 4 位是 1011。
	want := []bool{false, false, false, false}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("bit %d = %v, want %v", i, out[i], want[i])
		}
	}
}

func TestUint24(t *testing.T) {
	b := []byte{0x64, 0x00, 0x00} // 3 字节小端 = 100
	if Uint24(b) != 100 {
		t.Fatalf("Uint24 = %d, want 100", Uint24(b))
	}
	b2 := []byte{0xFF, 0xFF, 0x00} // = 65535
	if Uint24(b2) != 65535 {
		t.Fatalf("Uint24 = %d, want 65535", Uint24(b2))
	}
}

func TestDeviceTable(t *testing.T) {
	if !IsBitArea("M") || IsBitArea("D") {
		t.Fatal("bit area classification wrong")
	}
	// Lookup 大小写不敏感：驱动统一传小写，这里验证表本身。
	for _, area := range []string{"M", "m", "X", "x", "Y", "y", "B", "b"} {
		if !IsBitArea(area) {
			t.Fatalf("%q should be a bit area", area)
		}
	}
	for _, area := range []string{"D", "d", "W", "w", "R", "r", "ZR", "zr"} {
		if IsBitArea(area) {
			t.Fatalf("%q should be a word area", area)
		}
	}
	for _, area := range Areas() {
		if _, ok := Lookup(area); !ok {
			t.Fatalf("area %q has no device entry", area)
		}
	}
	if _, ok := Lookup("zz"); ok {
		t.Fatal("unknown area must not resolve")
	}
	// X 的编号按十六进制理解：X1F → offset 31。
	if d, ok := Lookup("x"); !ok || !d.Bit || d.Code != 0x9C {
		t.Fatalf("X device = %+v ok=%v", d, ok)
	}
}
