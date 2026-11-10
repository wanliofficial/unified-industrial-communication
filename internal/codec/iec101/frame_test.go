package iec101

import (
	"bytes"
	"errors"
	"testing"
)

// 本文件的黄金字节来自公开的工程样本，用于锁住三处最容易写错的算术：
//
//  1. L 只算「控制域 + 链路地址 + ASDU」，整帧 = L + 6；
//  2. 校验和从控制域开始累加，不含起始字节；
//  3. 固定帧与可变帧共用同一套控制域位定义。

func TestParseGoldenFixedRequest(t *testing.T) {
	// 主站请求链路状态：10 49 0C 00 55 16
	// 链路地址 2 字节 = 12，控制域 0x49 = PRM=1、FC=9。
	raw := []byte{0x10, 0x49, 0x0C, 0x00, 0x55, 0x16}
	res, err := ParseAt(raw, 2)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	if res.Consumed != len(raw) {
		t.Fatalf("Consumed = %d, want %d", res.Consumed, len(raw))
	}
	fr := res.Frame
	if fr.Kind != KindFixed {
		t.Fatalf("Kind = %v", fr.Kind)
	}
	if !fr.Ctrl.PRM || fr.Ctrl.Func != FCRequestLinkStatus {
		t.Fatalf("control = %+v", fr.Ctrl)
	}
	if fr.Ctrl.FCV || fr.Ctrl.FCB {
		t.Fatalf("link-status request must not use FCB/FCV: %+v", fr.Ctrl)
	}
	if fr.LinkAddr != 12 || fr.LinkAddrSize != 2 {
		t.Fatalf("addr = %d/%d", fr.LinkAddr, fr.LinkAddrSize)
	}
	if len(fr.Payload) != 0 {
		t.Fatalf("fixed frame must carry no payload, got % X", fr.Payload)
	}
}

func TestParseGoldenFixedResponse(t *testing.T) {
	// 从站回链路状态：10 0B 0C 00 17 16，控制域 0x0B = PRM=0、FC=11。
	raw := []byte{0x10, 0x0B, 0x0C, 0x00, 0x17, 0x16}
	res, err := ParseAt(raw, 2)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	if res.Frame.Ctrl.PRM {
		t.Fatal("secondary frame must have PRM=0")
	}
	if res.Frame.Ctrl.Func != FCLinkStatus {
		t.Fatalf("fc = %d", res.Frame.Ctrl.Func)
	}
}

func TestParseGoldenVariable(t *testing.T) {
	// 从站上送一条电能量召唤确认：
	// 68 0B 0B 68 08 0C 00 65 01 0A 0C 00 00 00 05 95 16
	raw := []byte{
		0x68, 0x0B, 0x0B, 0x68, 0x08, 0x0C, 0x00,
		0x65, 0x01, 0x0A, 0x0C, 0x00, 0x00, 0x00, 0x05,
		0x95, 0x16,
	}
	res, err := ParseAt(raw, 2)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	if res.Consumed != len(raw) {
		t.Fatalf("Consumed = %d, want %d", res.Consumed, len(raw))
	}
	fr := res.Frame
	if fr.Kind != KindVariable {
		t.Fatalf("Kind = %v", fr.Kind)
	}
	if fr.Ctrl.PRM || fr.Ctrl.Func != FCUserData {
		t.Fatalf("control = %+v", fr.Ctrl)
	}
	if fr.LinkAddr != 12 {
		t.Fatalf("addr = %d", fr.LinkAddr)
	}
	// L=11 = 控制域 1 + 地址 2 + ASDU 8。
	if len(fr.Payload) != 8 {
		t.Fatalf("payload len = %d, want 8 (% X)", len(fr.Payload), fr.Payload)
	}
	if fr.Payload[0] != 0x65 {
		t.Fatalf("type id = 0x%02X, want 0x65", fr.Payload[0])
	}
}

func TestParseGoldenVariableSQ(t *testing.T) {
	// 7 个对象的标度化测量值，验证 L 只算一遍地址、每对象带自己的地址。
	//
	// 注意：公开样本里这一帧印出的校验和是 0x85，但对它自己列出的字节
	// 求和得到 0x86。其余四帧的校验和都与算术一致，因此这里以算术为准
	// ——抄样本而不验算，正是一次「差一」故障的典型来源。
	raw := []byte{
		0x68, 0x2B, 0x2B, 0x68, 0x08, 0x0C, 0x00,
		0x0B, 0x07, 0x03, 0x0C, 0x00,
		0x10, 0x30, 0xBE, 0x09, 0x00,
		0x11, 0x30, 0x90, 0x09, 0x00,
		0x0E, 0x30, 0x75, 0x00, 0x00,
		0x28, 0x30, 0x25, 0x09, 0x00,
		0x29, 0x30, 0x75, 0x00, 0x00,
		0x0F, 0x30, 0x0F, 0x0A, 0x00,
		0x2E, 0x30, 0xAE, 0x05, 0x00,
		0x86, 0x16,
	}
	res, err := ParseAt(raw, 2)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	if got := len(res.Frame.Payload); got != 40 {
		t.Fatalf("payload len = %d, want 40", got)
	}
}

func TestParseSingleChar(t *testing.T) {
	for _, b := range []byte{AckByte, NackByte} {
		res, err := ParseAt([]byte{b}, 1)
		if err != nil {
			t.Fatalf("ParseAt(0x%02X): %v", b, err)
		}
		if res.Frame.Kind != KindSingle {
			t.Fatalf("Kind = %v", res.Frame.Kind)
		}
		if res.Consumed != 1 {
			t.Fatalf("Consumed = %d", res.Consumed)
		}
	}
}

func TestParseIncomplete(t *testing.T) {
	full := []byte{0x10, 0x49, 0x0C, 0x00, 0x55, 0x16}
	for i := 0; i < len(full); i++ {
		if _, err := ParseAt(full[:i], 2); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("prefix %d: err = %v, want ErrIncomplete", i, err)
		}
	}
	varFull := []byte{0x68, 0x0B, 0x0B, 0x68, 0x08, 0x0C, 0x00, 0x65, 0x01, 0x0A, 0x0C, 0x00, 0x00, 0x00, 0x05, 0x95, 0x16}
	for i := 0; i < len(varFull); i++ {
		if _, err := ParseAt(varFull[:i], 2); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("variable prefix %d: err = %v, want ErrIncomplete", i, err)
		}
	}
}

func TestParseRejectsCorrupt(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"bad checksum", []byte{0x10, 0x49, 0x0C, 0x00, 0x54, 0x16}},
		{"bad end byte", []byte{0x10, 0x49, 0x0C, 0x00, 0x55, 0x17}},
		{"length mismatch", []byte{0x68, 0x0B, 0x0C, 0x68, 0x08, 0x0C, 0x00, 0x65, 0x01, 0x0A, 0x0C, 0x00, 0x00, 0x00, 0x05, 0x95, 0x16}},
		{"bad second start", []byte{0x68, 0x0B, 0x0B, 0x69, 0x08, 0x0C, 0x00, 0x65, 0x01, 0x0A, 0x0C, 0x00, 0x00, 0x00, 0x05, 0x95, 0x16}},
		{"length too small", []byte{0x68, 0x01, 0x01, 0x68, 0x08, 0x16}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := ParseAt(c.raw, 2)
			if err == nil {
				t.Fatalf("expected error, got %+v", res)
			}
			if errors.Is(err, ErrIncomplete) {
				t.Fatalf("corrupt frame must not be reported as incomplete")
			}
			if res.Consumed <= 0 {
				t.Fatalf("Consumed = %d, must advance", res.Consumed)
			}
		})
	}
}

func TestParseSkipsNoise(t *testing.T) {
	// 噪声 + 好帧：第一个字节不是任何起始标识，应只消费 1 字节。
	buf := append([]byte{0x00, 0xFF}, []byte{0x10, 0x49, 0x0C, 0x00, 0x55, 0x16}...)
	res, err := ParseAt(buf, 2)
	if err == nil || res.Consumed != 1 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	res, err = ParseAt(buf[1:], 2)
	if err == nil || res.Consumed != 1 {
		t.Fatalf("second noise byte: res = %+v err = %v", res, err)
	}
	res, err = ParseAt(buf[2:], 2)
	if err != nil || res.Frame.Kind != KindFixed {
		t.Fatalf("good frame after noise: res = %+v err = %v", res, err)
	}
}

func TestBuildRoundTrip(t *testing.T) {
	for _, addrSize := range []int{0, 1, 2} {
		addr := uint16(0)
		if addrSize == 1 {
			addr = 5
		} else if addrSize == 2 {
			addr = 0x1234
		}

		fixed, err := BuildFixed(Control{PRM: true, Func: FCResetLink}, addr, addrSize)
		if err != nil {
			t.Fatalf("BuildFixed(%d): %v", addrSize, err)
		}
		res, err := ParseAt(fixed, addrSize)
		if err != nil {
			t.Fatalf("ParseAt fixed(%d): %v (% X)", addrSize, err, fixed)
		}
		if res.Frame.LinkAddr != addr {
			t.Fatalf("fixed addr = %d, want %d", res.Frame.LinkAddr, addr)
		}
		if res.Frame.Ctrl.Func != FCResetLink || !res.Frame.Ctrl.PRM {
			t.Fatalf("fixed control = %+v", res.Frame.Ctrl)
		}

		asdu := []byte{0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x14}
		v, err := BuildVariable(Control{PRM: true, FCB: true, FCV: true, Func: FCSendConfirm}, addr, addrSize, asdu)
		if err != nil {
			t.Fatalf("BuildVariable(%d): %v", addrSize, err)
		}
		res, err = ParseAt(v, addrSize)
		if err != nil {
			t.Fatalf("ParseAt variable(%d): %v (% X)", addrSize, err, v)
		}
		if !bytes.Equal(res.Frame.Payload, asdu) {
			t.Fatalf("payload = % X, want % X", res.Frame.Payload, asdu)
		}
		if !res.Frame.Ctrl.FCB || !res.Frame.Ctrl.FCV {
			t.Fatalf("FCB/FCV lost: %+v", res.Frame.Ctrl)
		}
		if res.Frame.LinkAddr != addr {
			t.Fatalf("variable addr = %d, want %d", res.Frame.LinkAddr, addr)
		}
	}
}

func TestBuildOverflowChecks(t *testing.T) {
	if _, err := BuildFixed(Control{PRM: true}, 1, 0); err == nil {
		t.Fatal("non-zero address with 0-byte width must fail")
	}
	if _, err := BuildFixed(Control{PRM: true}, 256, 1); err == nil {
		t.Fatal("address 256 must not fit in 1 byte")
	}
	if _, err := BuildVariable(Control{PRM: true}, 0, 3, nil); err == nil {
		t.Fatal("link address size 3 must be rejected")
	}
	if _, err := ParseAt([]byte{0x10}, 3); err == nil {
		t.Fatal("ParseAt with addr size 3 must fail")
	}
	// L 只有一个字节，body 超过 255 必须报错而不是截断。
	big := make([]byte, 256)
	if _, err := BuildVariable(Control{PRM: true}, 0, 0, big); err == nil {
		t.Fatal("body > 255 must be rejected")
	}
}

func TestControlBits(t *testing.T) {
	cases := []struct {
		raw  byte
		want Control
	}{
		// 0x49：PRM=1 FCV=0 FCB=0 FC=9
		{0x49, Control{PRM: true, Func: 9, Raw: 0x49}},
		// 0x53：PRM=1 FCB=0 FCV=1 FC=3
		{0x53, Control{PRM: true, FCV: true, Func: 3, Raw: 0x53}},
		// 0x73：PRM=1 FCB=1 FCV=1 FC=3
		{0x73, Control{PRM: true, FCB: true, FCV: true, Func: 3, Raw: 0x73}},
		// 0x0B：PRM=0 ACD=0 DFC=0 FC=11
		{0x0B, Control{Func: 11, Raw: 0x0B}},
		// 0x29：PRM=0 ACD=1 DFC=0 FC=9
		{0x29, Control{ACD: true, Func: 9, Raw: 0x29}},
		// 0x19：PRM=0 ACD=0 DFC=1 FC=9
		{0x19, Control{DFC: true, Func: 9, Raw: 0x19}},
	}
	for _, c := range cases {
		got := ParseControl(c.raw)
		if got != c.want {
			t.Fatalf("ParseControl(0x%02X) = %+v, want %+v", c.raw, got, c.want)
		}
		if got.Encode() != c.raw {
			t.Fatalf("Encode(%+v) = 0x%02X, want 0x%02X", got, got.Encode(), c.raw)
		}
	}
}

func TestControlMutexBits(t *testing.T) {
	// PRM=1 时 FCB/FCV 有意义，ACD/DFC 必须被忽略；反之亦然。
	c := ParseControl(0x70) // PRM=1, FCB=1, FCV=1
	if !c.PRM || !c.FCB || !c.FCV {
		t.Fatalf("primary bits = %+v", c)
	}
	c = ParseControl(0x30) // PRM=0, ACD=1, DFC=1
	if c.PRM || !c.ACD || !c.DFC {
		t.Fatalf("secondary bits = %+v", c)
	}
}

func TestChecksum(t *testing.T) {
	// 黄金样本：0x49 + 0x0C + 0x00 = 0x55
	if got := Checksum([]byte{0x49, 0x0C, 0x00}); got != 0x55 {
		t.Fatalf("Checksum = 0x%02X, want 0x55", got)
	}
	// 回绕：0xFF + 0x02 = 0x01
	if got := Checksum([]byte{0xFF, 0x02}); got != 0x01 {
		t.Fatalf("wrap = 0x%02X", got)
	}
	if got := Checksum(nil); got != 0 {
		t.Fatalf("empty = 0x%02X", got)
	}
}

func TestFrameLen(t *testing.T) {
	cases := []struct {
		buf  []byte
		size int
		want int
		ok   bool
	}{
		{[]byte{0xE5}, 1, 1, true},
		{[]byte{0x10, 0x49}, 2, 6, true},
		{[]byte{0x10, 0x49}, 1, 5, true},
		{[]byte{0x10, 0x49}, 0, 4, true},
		{[]byte{0x68, 0x0B}, 2, 17, true},
		{[]byte{0x68}, 2, 0, false},
		{nil, 2, 0, false},
		{[]byte{0x00}, 2, 1, true},
	}
	for _, c := range cases {
		got, ok := FrameLen(c.buf, c.size)
		if got != c.want || ok != c.ok {
			t.Fatalf("FrameLen(% X, %d) = %d,%t want %d,%t", c.buf, c.size, got, ok, c.want, c.ok)
		}
	}
}

func TestFrameStrings(t *testing.T) {
	fr := Frame{Kind: KindVariable, Ctrl: Control{PRM: true, Func: 3, FCV: true}, LinkAddr: 12, LinkAddrSize: 2, Payload: make([]byte, 8)}
	if s := fr.String(); s == "" {
		t.Fatal("Frame.String empty")
	}
	if s := (Frame{Kind: KindSingle}).String(); s == "" {
		t.Fatal("single String empty")
	}
	if KindFixed.String() != "fixed" || KindVariable.String() != "variable" || KindSingle.String() != "single" {
		t.Fatal("Kind.String")
	}
	if Kind(9).String() != "?" {
		t.Fatal("unknown Kind.String")
	}
	if ParseControl(0x49).String() == "" {
		t.Fatal("Control.String")
	}
	if ParseControl(0x0B).String() == "" {
		t.Fatal("secondary Control.String")
	}
	if FuncName(true, 99) == "" || FuncName(false, 99) == "" {
		t.Fatal("FuncName unknown")
	}
}
