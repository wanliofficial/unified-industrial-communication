package modbus

import (
	"strings"
	"testing"
)

// ASCII 分帧的测试重点与 RTU 不同：RTU 要证明「长度推导」正确，ASCII
// 要证明「文本转换 + LRC」在每一个字节边界上都可逆。金报文取自规范
// 示例，避免用同一份实现自证。

func TestLRCKnownVector(t *testing.T) {
	// 地址 0x01 + FC 0x03 + 00 00 00 01 → 和 0x05 → LRC 0xFB。
	// 这是规范附录里最常见的读 1 个保持寄存器的请求。
	body := []byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x01}
	if got := LRC(body); got != 0xFB {
		t.Fatalf("LRC = %#02x, want 0xFB", got)
	}
	// 加上 LRC 后整帧字节和必须为 0，这是 LRC 的定义性性质。
	var sum byte
	for _, b := range append(body, LRC(body)) {
		sum += b
	}
	if sum != 0 {
		t.Fatalf("body+LRC sum = %#02x, want 0", sum)
	}
}

func TestBuildASCIIGolden(t *testing.T) {
	// 读 1 个保持寄存器的完整 ASCII 帧（规范示例）。
	pdu := []byte{FCReadHoldingRegs, 0x00, 0x00, 0x00, 0x01}
	got := string(BuildASCII(0x01, pdu))
	want := ":010300000001FB\r\n"
	if got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

func TestParseASCIIGolden(t *testing.T) {
	unit, pdu, err := ParseASCII([]byte(":010300000001FB\r\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if unit != 0x01 {
		t.Fatalf("unit = %#x", unit)
	}
	if len(pdu) != 5 || pdu[0] != FCReadHoldingRegs {
		t.Fatalf("pdu = % x", pdu)
	}
}

// 大小写、仅 LF 终止符都要接受：现场串口服务器会做各种「无损」改写。
func TestParseASCIITolerant(t *testing.T) {
	cases := []string{
		":010300000001FB\r\n",
		":010300000001fb\r\n",
		":010300000001FB\n",
	}
	for _, in := range cases {
		if _, _, err := ParseASCII([]byte(in)); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

func TestParseASCIIRejects(t *testing.T) {
	cases := []struct{ name, in string }{
		{"no colon", "010300000001FB\r\n"},
		{"no terminator", ":010300000001FB"},
		{"odd hex", ":010300000001F\r\n"},
		{"non hex", ":01030000ZZ01FB\r\n"},
		{"bad lrc", ":010300000001FC\r\n"},
		{"too short", ":01FB\r\n"},
		{"empty", ":\r\n"},
	}
	for _, c := range cases {
		if _, _, err := ParseASCII([]byte(c.in)); err == nil {
			t.Fatalf("%s: %q must be rejected", c.name, c.in)
		}
	}
}

// BuildASCII 与 ParseASCII 必须在各种长度上互为逆运算。这里刻意跨过
// 奇偶字节边界与最大 PDU，避免「只在 8 字节帧上碰巧正确」。
func TestASCIIRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 7, 8, 9, 100, 253} {
		pdu := make([]byte, n)
		for i := range pdu {
			pdu[i] = byte(i * 37)
		}
		frame := BuildASCII(0xA5, pdu)
		unit, got, err := ParseASCII(frame)
		if err != nil {
			t.Fatalf("len %d: %v", n, err)
		}
		if unit != 0xA5 {
			t.Fatalf("len %d: unit %#x", n, unit)
		}
		if string(got) != string(pdu) {
			t.Fatalf("len %d: pdu mismatch", n)
		}
	}
}

// 帧长推导：ASCII 字符数 = 1 + 2×(地址+PDU+LRC) + 2。
func TestASCIIExpectedResponseLen(t *testing.T) {
	req := []byte{FCReadHoldingRegs, 0, 0, 0, 2}
	got, err := ASCIIExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// 响应 PDU = FC(1)+字节数(1)+数据(4)=6 字节；
	// ASCII: 1 + 2×(地址1+PDU6+LRC1) + 2 = 19
	if got != 19 {
		t.Fatalf("len = %d, want 19", got)
	}

	// 必须与实际生成的响应帧长度一致，否则「预读校验」会误判。
	respPDU := []byte{FCReadHoldingRegs, 4, 0x08, 0xAE, 0x02, 0x00}
	frame := BuildASCII(1, respPDU)
	if len(frame) != got {
		t.Fatalf("actual frame %d chars, predicted %d", len(frame), got)
	}
	// 只差终止符与起始符之外的字符数必须为偶数。
	if (len(frame)-3)%2 != 0 {
		t.Fatalf("hex payload has odd length: %q", frame)
	}
}

func TestASCIIExpectedResponseLenWriteEcho(t *testing.T) {
	// 写单寄存器响应是 5 字节 PDU 的回显：地址+5+LRC=7 字节 → 1+14+2=17
	req := []byte{FCWriteSingleRegister, 0, 1, 0, 0x2A}
	got, err := ASCIIExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 17 {
		t.Fatalf("len = %d, want 17", got)
	}
	frame := BuildASCII(1, req) // 回显 PDU 与请求同形
	if len(frame) != got {
		t.Fatalf("actual %d, predicted %d", len(frame), got)
	}
}

// 帧长上限必须能容纳最大合法 PDU（253 字节），否则长报文会被误判。
func TestASCIIMaxFrameCoversMaxPDU(t *testing.T) {
	pdu := make([]byte, 253)
	for i := range pdu {
		pdu[i] = byte(i)
	}
	frame := BuildASCII(1, pdu)
	if len(frame) > ASCIIMaxFrame {
		t.Fatalf("frame %d exceeds max %d", len(frame), ASCIIMaxFrame)
	}
	if !strings.HasSuffix(string(frame), "\r\n") {
		t.Fatalf("frame does not end with CRLF")
	}
}
