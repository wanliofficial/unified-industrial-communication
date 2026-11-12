package bacnet

import (
	"bytes"
	"testing"
)

// 这两个残差是 MS/TP 的「算法是否正确」试金石：头字段（含 CRC）算下来
// 必须是 0x55，数据段（含两字节 CRC）算下来必须是 0xF0B8。任何抄错多项式
// 的实现都会在这里露馅。
func TestMSTPCRCResidues(t *testing.T) {
	head := []byte{FrameToken, 0x01, 0x02, 0x00, 0x00}
	crc := CRCHeader(head)
	if crc != 0x40 {
		t.Fatalf("CRCHeader(% X) = 0x%02X, want 0x40", head, crc)
	}
	if got := HeaderResidue(append(head, crc)); got != 0x55 {
		t.Fatalf("header residue = 0x%02X, want 0x55", got)
	}
	if !CheckHeaderCRC(append(head, crc)) {
		t.Fatal("CheckHeaderCRC rejected a valid header")
	}

	for _, data := range [][]byte{
		{0x01, 0x02, 0x03, 0x04},
		{0x00},
		{0x02, 0x00, 0x0C, 0x01, 0x02},
	} {
		sum := CRCData(data)
		wire := append(append([]byte(nil), data...), byte(sum), byte(sum>>8))
		if got := DataResidue(wire); got != 0xF0B8 {
			t.Fatalf("data residue(% X) = 0x%04X, want 0xF0B8", data, got)
		}
		if !CheckDataCRC(wire) {
			t.Fatalf("CheckDataCRC rejected valid data % X", data)
		}
	}
}

// 标准里的查表实现有 CRC_Calc_Header(0) = 0x00、CRC_Calc_Header(1) = 0xFE；
// 数据侧有 CRC_Calc_Data(0) = 0x0000、CRC_Calc_Data(1) = 0x1189。用单字节
// 输入对拍，能排除「整体残差碰巧对了但字节顺序/初值抄错」的情况。
func TestMSTPCRCByteVectors(t *testing.T) {
	if got := crcHeaderByte(0, 0); got != 0x00 {
		t.Fatalf("crcHeaderByte(0,0) = 0x%02X, want 0x00", got)
	}
	if got := crcHeaderByte(0, 1); got != 0xFE {
		t.Fatalf("crcHeaderByte(0,1) = 0x%02X, want 0xFE", got)
	}
	if got := crcDataByte(0, 0); got != 0x0000 {
		t.Fatalf("crcDataByte(0,0) = 0x%04X, want 0x0000", got)
	}
	if got := crcDataByte(0, 1); got != 0x1189 {
		t.Fatalf("crcDataByte(0,1) = 0x%04X, want 0x1189", got)
	}
}

func TestMSTPBuildTokenFrame(t *testing.T) {
	out, err := BuildMSTPFrame(MSTPFrame{Type: FrameToken, Dest: 0x05, Src: 0x00})
	if err != nil {
		t.Fatalf("BuildMSTPFrame: %v", err)
	}
	// 55 FF 00 05 00 00 00 <crc>
	if !bytes.Equal(out[:2], []byte{0x55, 0xFF}) {
		t.Fatalf("preamble = % X", out[:2])
	}
	if len(out) != MSTPHeaderLen+2 {
		t.Fatalf("token frame length = %d, want %d", len(out), MSTPHeaderLen+2)
	}
	if out[2] != FrameToken || out[3] != 0x05 || out[4] != 0x00 {
		t.Fatalf("header fields = % X", out[2:5])
	}
	if !CheckHeaderCRC(out[2:MSTPHeaderLen]) {
		t.Fatalf("header CRC invalid: % X", out[2:MSTPHeaderLen])
	}
	// 数据段为空，数据 CRC 是 CRCData(nil)。
	crc := CRCData(nil)
	if out[8] != byte(crc) || out[9] != byte(crc>>8) {
		t.Fatalf("data CRC bytes = %02X %02X, want %02X %02X",
			out[8], out[9], byte(crc), byte(crc>>8))
	}
}

func TestMSTPBuildDataFrameEndianness(t *testing.T) {
	// 长度字段大端、数据 CRC 小端：用一段 0x0102 的数据把两者同时看出来。
	data := []byte{0x02, 0x00, 0x01, 0x02}
	out, err := BuildMSTPFrame(MSTPFrame{
		Type: FrameDataExpectingReply, Dest: 0x80, Src: 0x00, Data: data,
	})
	if err != nil {
		t.Fatalf("BuildMSTPFrame: %v", err)
	}
	if out[5] != 0x00 || out[6] != 0x04 {
		t.Fatalf("length field = %02X %02X, want 00 04 (big-endian)", out[5], out[6])
	}
	sum := CRCData(data)
	if out[8+len(data)] != byte(sum) || out[8+len(data)+1] != byte(sum>>8) {
		t.Fatalf("data CRC = %02X %02X, want %02X %02X (little-endian)",
			out[8+len(data)], out[8+len(data)+1], byte(sum), byte(sum>>8))
	}
}

func TestMSTPDecodeRoundTrip(t *testing.T) {
	cases := []MSTPFrame{
		{Type: FrameToken, Dest: 0x01, Src: 0x00},
		{Type: FramePollForMaster, Dest: 0x02, Src: 0x00},
		{Type: FrameReplyToPollForMaster, Dest: 0x00, Src: 0x02},
		{Type: FrameTestRequest, Dest: 0x80, Src: 0x00},
		{Type: FrameTestResponse, Dest: 0x00, Src: 0x80},
		{Type: FrameDataExpectingReply, Dest: 0x80, Src: 0x00,
			Data: []byte{0x01, 0x04, 0x00, 0x05, 0x0C}},
		{Type: FrameDataNotExpectingReply, Dest: 0x00, Src: 0x80,
			Data: []byte{0x01, 0x00, 0x20, 0x01, 0x0C}},
		{Type: FrameReplyPostponed, Dest: 0x00, Src: 0x80},
	}
	for _, want := range cases {
		wire, err := BuildMSTPFrame(want)
		if err != nil {
			t.Fatalf("%s: build: %v", FrameTypeName(want.Type), err)
		}
		// 末尾追加一个 pad 与半帧噪声，解码必须只消费本帧。
		wire = append(wire, 0xFF, 0x55, 0xFF, 0x00)
		got, n, err := DecodeMSTPFrame(wire)
		if err != nil {
			t.Fatalf("%s: decode: %v", FrameTypeName(want.Type), err)
		}
		if n != len(wire)-4 {
			t.Fatalf("%s: consumed %d, want %d", FrameTypeName(want.Type), n, len(wire)-4)
		}
		if got.Type != want.Type || got.Dest != want.Dest || got.Src != want.Src {
			t.Fatalf("%s: header mismatch: got %+v want %+v", FrameTypeName(want.Type), got, want)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("%s: data mismatch: got % X want % X", FrameTypeName(want.Type), got.Data, want.Data)
		}
	}
}

func TestMSTPDecodeRejectsMalformed(t *testing.T) {
	valid, err := BuildMSTPFrame(MSTPFrame{
		Type: FrameDataExpectingReply, Dest: 0x80, Src: 0x00,
		Data: []byte{0x01, 0x04, 0x00, 0x0C},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("short", func(t *testing.T) {
		if _, _, err := DecodeMSTPFrame(valid[:MSTPHeaderLen-1]); err != ErrTruncated {
			t.Fatalf("err = %v, want ErrTruncated", err)
		}
	})
	t.Run("bad preamble", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		bad[0] = 0x54
		if _, _, err := DecodeMSTPFrame(bad); err != ErrMSTPBadPreamble {
			t.Fatalf("err = %v, want ErrMSTPBadPreamble", err)
		}
	})
	t.Run("bad header crc", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		bad[7] ^= 0xFF
		if _, _, err := DecodeMSTPFrame(bad); err != ErrMSTPBadCRC {
			t.Fatalf("err = %v, want ErrMSTPBadCRC", err)
		}
	})
	t.Run("bad data crc", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		bad[len(bad)-1] ^= 0xFF
		if _, _, err := DecodeMSTPFrame(bad); err != ErrMSTPBadCRC {
			t.Fatalf("err = %v, want ErrMSTPBadCRC", err)
		}
	})
	t.Run("length exceeds limit", func(t *testing.T) {
		// 直接改长度字段会先让头 CRC 失效，因此这里手工构造一条
		// 「长度 502、头 CRC 正确」的帧，专门测长度上限。
		head := []byte{FrameDataExpectingReply, 0x80, 0x00, 0x01, 0xF6}
		wire := append([]byte{0x55, 0xFF}, head...)
		wire = append(wire, CRCHeader(head))
		if _, _, err := DecodeMSTPFrame(wire); err == nil || err == ErrTruncated {
			t.Fatalf("err = %v, want length-limit error", err)
		}
	})
	t.Run("body truncated", func(t *testing.T) {
		if _, _, err := DecodeMSTPFrame(valid[:len(valid)-1]); err != ErrTruncated {
			t.Fatalf("err = %v, want ErrTruncated", err)
		}
	})
}

func TestMSTPBuildRejectsInvalid(t *testing.T) {
	if _, err := BuildMSTPFrame(MSTPFrame{Type: 8, Dest: 1, Src: 0}); err == nil {
		t.Fatal("unknown frame type accepted")
	}
	if _, err := BuildMSTPFrame(MSTPFrame{Type: FrameToken, Dest: 1, Src: 0, Data: []byte{1}}); err == nil {
		t.Fatal("token frame with payload accepted")
	}
	if _, err := BuildMSTPFrame(MSTPFrame{Type: FrameDataExpectingReply, Dest: 1, Src: 0}); err == nil {
		t.Fatal("data frame without NPDU accepted")
	}
	if _, err := BuildMSTPFrame(MSTPFrame{
		Type: FrameDataExpectingReply, Dest: 1, Src: 0,
		Data: make([]byte, MSTPMaxData+1),
	}); err == nil {
		t.Fatal("oversized data frame accepted")
	}
}

func TestMSTPAddressClasses(t *testing.T) {
	for mac := 0; mac <= 127; mac++ {
		if !IsMasterMAC(byte(mac)) || IsSlaveMAC(byte(mac)) {
			t.Fatalf("MAC %d classification wrong", mac)
		}
	}
	for mac := 128; mac <= 254; mac++ {
		if IsMasterMAC(byte(mac)) || !IsSlaveMAC(byte(mac)) {
			t.Fatalf("MAC %d classification wrong", mac)
		}
	}
	if IsMasterMAC(MSTPBroadcast) || IsSlaveMAC(MSTPBroadcast) {
		t.Fatal("broadcast must be neither master nor slave")
	}
}

func TestMSTPFrameTypeNames(t *testing.T) {
	want := map[byte]string{
		FrameToken:                 "token",
		FramePollForMaster:         "poll-for-master",
		FrameReplyToPollForMaster:  "reply-to-poll-for-master",
		FrameTestRequest:           "test-request",
		FrameTestResponse:          "test-response",
		FrameDataExpectingReply:    "data-expecting-reply",
		FrameDataNotExpectingReply: "data-not-expecting-reply",
		FrameReplyPostponed:        "reply-postponed",
	}
	for typ, name := range want {
		if got := FrameTypeName(typ); got != name {
			t.Fatalf("FrameTypeName(%d) = %q, want %q", typ, got, name)
		}
	}
	if got := FrameTypeName(99); got != "unknown" {
		t.Fatalf("FrameTypeName(99) = %q", got)
	}
}
