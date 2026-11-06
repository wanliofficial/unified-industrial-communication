package modbus

import "testing"

// 串口从站必须**自己**判断一帧有多长：RTU 没有长度前缀，ASCII 虽然有
// CRLF，但预知长度能让截断立刻失败，而不是等到字符超时。
//
// 这张表的每一条都对应一个真实主站的报文形状；推导错了的后果不是「读
// 失败」，而是把下一帧的开头当成这一帧的尾巴——现场表现为偶发 CRC 错，
// 极难复现。

func TestRTURequestLen(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want int
		ok   bool
	}{
		{"only address", []byte{1}, 0, false},
		{"read coils", []byte{1, FCReadCoils}, 8, true},
		{"read discrete inputs", []byte{1, FCReadDiscreteInputs}, 8, true},
		{"read holding", []byte{1, FCReadHoldingRegs}, 8, true},
		{"read input", []byte{1, FCReadInputRegs}, 8, true},
		{"write single coil", []byte{1, FCWriteSingleCoil}, 8, true},
		{"write single register", []byte{1, FCWriteSingleRegister}, 8, true},
		{"diagnostics", []byte{1, FC08}, 8, true},
		{"event counter", []byte{1, FC11}, 4, true},
		{"report server id", []byte{1, FC17}, 4, true},
		// FC15/16：字节数字段在帧内偏移 6，没收到它就无法推导。
		{"write multiple without byte count", []byte{1, FCWriteMultipleRegs, 0, 0, 0, 0}, 0, false},
		{"write multiple registers", []byte{1, FCWriteMultipleRegs, 0, 0, 0, 1, 4}, 13, true},
		{"write multiple coils", []byte{1, FCWriteMultipleCoils, 0, 0, 0, 9, 2}, 11, true},
		{"device id needs mei", []byte{1, FC43}, 0, false},
		{"device id", []byte{1, FC43, 0x0E}, 7, true},
		{"device id unknown mei", []byte{1, FC43, 0x0D, 1, 2}, 0, false},
		{"unknown fc", []byte{1, 0x41, 0, 0}, 0, false},
	}
	for _, c := range cases {
		got, ok := RTURequestLen(c.head)
		if ok != c.ok || got != c.want {
			t.Fatalf("%s: RTURequestLen(% x) = %d,%v want %d,%v",
				c.name, c.head, got, ok, c.want, c.ok)
		}
	}
}

func TestASCIIRequestLen(t *testing.T) {
	// 8 字节 RTU 请求（地址 1 + PDU 5 + CRC 2）→ ':' + 2×7 + CRLF = 17 字符。
	if got, ok := ASCIIRequestLen([]byte{1, FCReadHoldingRegs}); !ok || got != 17 {
		t.Fatalf("ASCIIRequestLen(read holding) = %d,%v want 17,true", got, ok)
	}
	// 4 字节短请求 → ':' + 2×3 + CRLF = 9 字符。
	if got, ok := ASCIIRequestLen([]byte{1, FC11}); !ok || got != 9 {
		t.Fatalf("ASCIIRequestLen(fc11) = %d,%v want 9,true", got, ok)
	}
	if _, ok := ASCIIRequestLen([]byte{1, 0x41}); ok {
		t.Fatal("unknown function must not be predicted")
	}
}

// 推导出的长度必须与真实编码长度一致：这是一条自洽性检查，防止两张表
// 各自漂移。BuildRTU 是主站侧唯一的编码入口，用它做对照最有意义。
func TestRTURequestLenMatchesEncoding(t *testing.T) {
	reqs := [][]byte{
		{FCReadHoldingRegs, 0, 0, 0, 1},
		{FCReadCoils, 0, 0, 0, 8},
		{FCWriteSingleRegister, 0, 3, 0, 9},
		{FCWriteMultipleRegs, 0, 0, 0, 2, 4, 0, 1, 0, 2},
		{FC08, 0, 0, 0xAB, 0xCD},
		{FC11},
		{FC17},
		{FC43, 0x0E, 1, 0},
	}
	for _, pdu := range reqs {
		frame := BuildRTU(1, pdu)
		head := frame
		if len(head) > 7 {
			head = head[:7]
		}
		got, ok := RTURequestLen(head)
		if !ok || got != len(frame) {
			t.Fatalf("fc %#x: predicted %d,%v, encoded %d", pdu[0], got, ok, len(frame))
		}
	}
}
