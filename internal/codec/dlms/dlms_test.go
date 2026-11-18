package dlms

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// FCS 的 Annex A.1 金向量：`03 3F` 的校验序列是 `5B EC`（小端 0xEC5B），
// 好帧补上校验后重算留下 0xF0B8。这条向量同时锁定了「初值、多项式、反射、
// 取反、余数」五件事——任何一处写错都不会留下 F0B8。
func TestFCSAnnexA(t *testing.T) {
	if got := FCS([]byte{0x03, 0x3F}); got != 0xEC5B {
		t.Fatalf("FCS(03 3F) = %04X, want EC5B", got)
	}
	if !GoodFCS([]byte{0x03, 0x3F, 0x5B, 0xEC}) {
		t.Error("03 3F 5B EC 未留下好帧余数 F0B8")
	}
	if GoodFCS([]byte{0x03, 0x3E, 0x5B, 0xEC}) {
		t.Error("被篡改的帧通过了 FCS 校验")
	}
}

// 标准金向量：无信息域的 SNRM 帧 `7E A0 07 03 21 93 0F 01 7E`。
//
//	长度 A0 07 = 7（格式 2 + dest 1 + src 1 + ctl 1 + 校验 2）
//	dest 0x03 = 客户端地址 1（bit0=1 结束，低 7 位 = 1）
//	src 0x21 = 按 7 位地址解码是 16（客户端地址；示例取自标准 §6.4.2.3 的字节形态）
//	ctl 0x93 = SNRM
//	HCS 覆盖 A0 07 03 21 93 → 0F 01（无信息域时它就是唯一校验序列）
func TestSNRMGolden(t *testing.T) {
	fr := Frame{
		Dest:    ServerAddress(1, 0, 1), // 电表：管理逻辑设备 1
		Src:     ClientAddress(0x10),    // 客户端：公共客户端 16
		Control: Control{Kind: KindSNRM},
	}
	body, err := fr.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []byte{0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01}
	if !bytes.Equal(body, want) {
		t.Fatalf("SNRM body = % X, want % X", body, want)
	}

	// 端到端：补标志后能被 ParseRaw 解回，且 HCS 与 FCS 都自洽。
	raw := append([]byte{Flag}, body...)
	raw = append(raw, Flag)
	got, consumed, err := ParseRaw(raw)
	if err != nil {
		t.Fatalf("ParseRaw: %v", err)
	}
	if consumed != len(raw) {
		t.Fatalf("consumed = %d, want %d", consumed, len(raw))
	}
	if got.Kind() != KindSNRM {
		t.Fatalf("kind = %v, want SNRM", got.Kind())
	}
	if got.Dest.Upper != 1 || got.Src.Upper != 0x10 {
		t.Fatalf("addr = %+v", got)
	}
}

// 4 字节地址的金向量（§6.4.2.3）：Upper=0x1234、Lower=0x3FFF（全站）→
// `48 68 FE FF`；加上客户端地址 0x3A（1 字节 0x75）后应完整解出。
func TestAddressFourOctet(t *testing.T) {
	dest := ServerAddress(0x1234, AddrAllStation2, 4)
	d, err := dest.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []byte{0x48, 0x68, 0xFE, 0xFF}
	if !bytes.Equal(d, want) {
		t.Fatalf("4 字节地址 = % X, want % X", d, want)
	}

	// 往返：客户端 0x3A → 0x75。
	cli, _ := ClientAddress(0x3A).Encode()
	if cli[0] != 0x75 {
		t.Fatalf("client 0x3A = %02X, want 75", cli[0])
	}
}

// 2 字节地址往返：Upper/Lower 各 7 位，结束位在最后一字节。
func TestAddressRoundTrip(t *testing.T) {
	for _, a := range []Address{
		ClientAddress(0x10),
		ServerAddress(1, 0x11, 2),
		ServerAddress(0x1234, 0x3FFF, 4),
	} {
		b, err := a.Encode()
		if err != nil {
			t.Fatalf("Encode %+v: %v", a, err)
		}
		got, n, err := DecodeAddress(b)
		if err != nil || n != len(b) {
			t.Fatalf("DecodeAddress(% X) = %+v, %d, %v", b, got, n, err)
		}
		if got != a {
			t.Fatalf("往返 = %+v, want %+v", got, a)
		}
	}
}

// 有信息域的端到端：把标准 Annex C.9 的 LN AARE APDU（47 字节）装进一帧，
// 走一遍 Encode → ParseRaw。这条用例覆盖了「HCS 与 FCS 都在」的分支——
// 无信息域时 HCS 就是唯一校验序列，长度恒为 7，那条路径由 TestSNRMGolden 覆盖。
//
// 长度字段 A0 37 = 55 = 2(格式) + 1(dest) + 1(src) + 1(ctl) + 2(HCS) + 46(info) + 2(FCS)。
func TestAAREFrame(t *testing.T) {
	apdu, err := hex.DecodeString("6129A109060760857405080101A203020100A305A103020100BE1004" +
		"0E0800065F1F040000501F01F40007")
	if err != nil {
		t.Fatal(err)
	}
	info := append([]byte(LLCResponse), apdu...)
	fr := Frame{
		Dest:    ServerAddress(1, 0, 1),
		Src:     ClientAddress(0x10),
		Control: Control{Kind: KindI, NS: 0, NR: 0},
		Info:    info,
	}
	body, err := fr.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if body[1] != 0x37 {
		t.Fatalf("长度字段 = %02X, want 37", body[1])
	}
	if got := int(body[1]); got != len(body) {
		t.Fatalf("长度字段 %d != len(body) %d", got, len(body))
	}

	raw := append([]byte{Flag}, body...)
	raw = append(raw, Flag)
	got, n, err := ParseRaw(raw)
	if err != nil {
		t.Fatalf("ParseRaw: %v", err)
	}
	if n != len(raw) {
		t.Fatalf("consumed %d want %d", n, len(raw))
	}
	if len(got.Info) != len(info) {
		t.Fatalf("info len = %d, want %d", len(got.Info), len(info))
	}
	ap, ok := got.APDU()
	if !ok || !bytes.Equal(ap, apdu) {
		t.Fatalf("APDU = % X", ap)
	}
}

// 长度字段恒等式：长度字段值 = 两标志之间的字节数 = body（不含标志）的长度，
// 其中已包含末尾 FCS。SNRM 的 `A0 07` 与 AARE 的 `A0 37`（55）都印证这一点。
func TestFrameLen(t *testing.T) {
	body := []byte{0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01}
	if int(body[1]) != len(body) {
		t.Fatalf("长度恒等式不成立: b[1]=%d len(body)=%d", body[1], len(body))
	}
	n, ok := FrameLen(body)
	if !ok || n != len(body)+2 {
		t.Fatalf("FrameLen = %d, %v; want %d, true", n, ok, len(body)+2)
	}
}
