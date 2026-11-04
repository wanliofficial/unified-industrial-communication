package modbus

import (
	"encoding/binary"
	"testing"
)

// RTU 分帧的难点不在 CRC，而在「没有长度字段」。这些用例锁定长度推导
// 规则：主站按它决定读多少字节，从站若返回不同长度必须先被识别再被
// 拒绝，否则会把下一帧的字节算进本帧。

func TestRTUExpectedResponseLenReadHolding(t *testing.T) {
	// FC3 读 2 个寄存器：地址 + FC + 字节数 + 4 字节数据 + CRC = 9
	req := []byte{FCReadHoldingRegs, 0, 0, 0, 2}
	got, err := RTUExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 9 {
		t.Fatalf("len = %d, want 9", got)
	}
}

func TestRTUExpectedResponseLenReadCoils(t *testing.T) {
	// FC1 读 9 个线圈：ceil(9/8)=2 字节数据 → 地址+FC+字节数+2+CRC = 7
	req := make([]byte, 5)
	req[0] = FCReadCoils
	binary.BigEndian.PutUint16(req[3:5], 9)
	got, err := RTUExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 7 {
		t.Fatalf("len = %d, want 7", got)
	}
}

func TestRTUExpectedResponseLenCoilBoundary(t *testing.T) {
	// 8 个线圈正好 1 字节；16 个线圈 2 字节。差一字节就会读错帧。
	cases := []struct {
		qty  uint16
		want int
	}{
		{1, 6}, {8, 6}, {9, 7}, {16, 7}, {17, 8},
	}
	for _, c := range cases {
		req := make([]byte, 5)
		req[0] = FCReadCoils
		binary.BigEndian.PutUint16(req[3:5], c.qty)
		got, err := RTUExpectedResponseLen(req)
		if err != nil {
			t.Fatalf("qty %d: %v", c.qty, err)
		}
		if got != c.want {
			t.Fatalf("qty %d: len = %d, want %d", c.qty, got, c.want)
		}
	}
}

func TestRTUExpectedResponseLenWrites(t *testing.T) {
	// 所有写响应都是 8 字节回显。
	reqs := [][]byte{
		{FCWriteSingleCoil, 0, 0, 0xFF, 0x00},
		{FCWriteSingleRegister, 0, 0, 0, 1},
		{FCWriteMultipleCoils, 0, 0, 0, 8, 1, 0xFF},
		{FCWriteMultipleRegs, 0, 0, 0, 1, 2, 0, 9},
	}
	for _, req := range reqs {
		got, err := RTUExpectedResponseLen(req)
		if err != nil {
			t.Fatalf("fc %#x: %v", req[0], err)
		}
		if got != 8 {
			t.Fatalf("fc %#x: len = %d, want 8", req[0], got)
		}
	}
}

func TestRTUExpectedResponseLenRejectsBadQuantity(t *testing.T) {
	// 数量为 0 或超过协议上限属于请求本身非法，应在发帧前拦住。
	cases := []uint16{0, MaxReadRegisters + 1}
	for _, qty := range cases {
		req := make([]byte, 5)
		req[0] = FCReadHoldingRegs
		binary.BigEndian.PutUint16(req[3:5], qty)
		if _, err := RTUExpectedResponseLen(req); err == nil {
			t.Fatalf("qty %d: expected error", qty)
		}
	}
}

func TestRTUExpectedResponseLenRejectsShortPDU(t *testing.T) {
	if _, err := RTUExpectedResponseLen(nil); err == nil {
		t.Fatal("empty pdu should fail")
	}
	if _, err := RTUExpectedResponseLen([]byte{FCReadHoldingRegs, 0, 0}); err == nil {
		t.Fatal("truncated pdu should fail")
	}
}

func TestRTUExpectedResponseLenRejectsUnknownFunction(t *testing.T) {
	if _, err := RTUExpectedResponseLen([]byte{0x41, 0, 0, 0, 1}); err == nil {
		t.Fatal("unknown function must fail, not guess")
	}
}

// 主站推导的长度必须与真实构帧长度一致；两边不匹配就是「永远读不满」。
func TestRTUExpectedResponseLenMatchesActualFrame(t *testing.T) {
	resp := BuildRTU(1, []byte{FCReadHoldingRegs, 4, 0x04, 0xD2, 0x00, 0x07})
	req := []byte{FCReadHoldingRegs, 0, 0, 0, 2}
	want, err := RTUExpectedResponseLen(req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want != len(resp) {
		t.Fatalf("predicted %d, actual frame %d", want, len(resp))
	}
}

func TestParseRTURejectsBadCRC(t *testing.T) {
	frame := BuildRTU(1, []byte{FCReadHoldingRegs, 2, 0x00, 0x01})
	frame[len(frame)-1] ^= 0xFF
	if _, _, err := ParseRTU(frame); err == nil {
		t.Fatal("corrupted CRC must be rejected")
	}
}

func TestParseRTURejectsShortFrame(t *testing.T) {
	if _, _, err := ParseRTU([]byte{1, 3, 0}); err == nil {
		t.Fatal("short frame must be rejected")
	}
}
