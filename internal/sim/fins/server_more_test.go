package fins

import (
	"encoding/binary"
	"io"
	"net"
	"testing"

	finscodec "uic/internal/codec/fins"
)

// 这些用例绕过主站驱动，直接往从站发原始帧：主站只会发「正确」的帧，
// 而现场真正会遇到的是报文错位、点数错、区码错。从站的错误应答必须
// 是一条明确的结束码，而不是静默或崩溃。

func dialRaw(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write(finscodec.BuildNodeRequest(0)); err != nil {
		t.Fatalf("handshake write: %v", err)
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		t.Fatalf("handshake header: %v", err)
	}
	if th.Length > 8 {
		if _, err := io.ReadFull(c, make([]byte, th.Length-8)); err != nil {
			t.Fatalf("handshake payload: %v", err)
		}
	}
	return c
}

func roundTripRaw(t *testing.T, c net.Conn, frame []byte) finscodec.Frame {
	t.Helper()
	pkt, err := finscodec.BuildTCPFrame(frame)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if _, err := c.Write(pkt); err != nil {
		t.Fatalf("write: %v", err)
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("read header: %v", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	body := make([]byte, th.Length-8)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp, err := finscodec.Parse(body)
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	return resp
}

func rawCommand(sid byte, mrc, src byte, data []byte) []byte {
	return finscodec.BuildCommand(finscodec.CommandHeader(1, 2, sid), mrc, src, data)
}

func TestRawErrorEndCodes(t *testing.T) {
	srv := newSim(t)
	c := dialRaw(t, srv)

	// 参数区布局与驱动一致：区码(1)+字(2)+位(1)+点数(2)。
	dm, _ := finscodec.Lookup("dm")
	cio, _ := finscodec.Lookup("cio")

	cases := []struct {
		name string
		f    []byte
		want uint16
	}{
		{
			name: "unknown area code",
			f:    rawCommand(1, 0x01, 0x01, []byte{0xEE, 0, 0, 0, 0, 1}),
			want: 0x1101,
		},
		{
			name: "count zero",
			f:    rawCommand(2, 0x01, 0x01, []byte{dm.Word, 0, 0, 0, 0, 0}),
			want: 0x1102,
		},
		{
			name: "word count over limit",
			f:    rawCommand(3, 0x01, 0x01, []byte{dm.Word, 0, 0, 0, 0x03, 0xE8}), // 1000
			want: 0x1102,
		},
		{
			name: "word range overflow",
			f:    rawCommand(4, 0x01, 0x01, []byte{dm.Word, 0xFF, 0xFF, 0, 0, 2}),
			want: 0x1103,
		},
		{
			name: "bit position out of range",
			f:    rawCommand(5, 0x01, 0x01, []byte{cio.Bit, 0, 0, 16, 0, 1}),
			want: 0x1103,
		},
		{
			name: "unknown command",
			f:    rawCommand(6, 0x02, 0x01, []byte{dm.Word, 0, 0, 0, 0, 1}),
			want: 0x0401,
		},
		{
			name: "command too short",
			f:    rawCommand(7, 0x01, 0x01, []byte{dm.Word, 0, 0}),
			want: 0x1002,
		},
		{
			name: "bit write data length mismatch",
			f:    rawCommand(8, 0x01, 0x02, []byte{cio.Bit, 0, 10, 0, 0, 2, 1}), // 声称 2 位只给 1 字节
			want: 0x1002,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := roundTripRaw(t, c, tc.f)
			if resp.EndCode != tc.want {
				t.Fatalf("end code = 0x%04X, want 0x%04X (%s)",
					resp.EndCode, tc.want, finscodec.EndCodeText(resp.EndCode))
			}
		})
	}
}

func TestRawBitReadSpansWords(t *testing.T) {
	srv := newSim(t)
	// 字 0 的 bit15 与字 1 的 bit0 相邻但不同字；一次读 2 位必须跨字正确取。
	srv.SetBit("cio", 0, 15, true)
	srv.SetBit("cio", 1, 0, true)
	c := dialRaw(t, srv)

	cio, _ := finscodec.Lookup("cio")
	resp := roundTripRaw(t, c, rawCommand(1, 0x01, 0x01, []byte{cio.Bit, 0, 0, 15, 0, 2}))
	if resp.EndCode != 0 {
		t.Fatalf("end code = 0x%04X", resp.EndCode)
	}
	if len(resp.Data) != 2 || resp.Data[0] != 1 || resp.Data[1] != 1 {
		t.Fatalf("bits = % x", resp.Data)
	}

	// 同一批数据按字读：CIO0 = 0x8000，CIO1 = 0x0001。
	resp = roundTripRaw(t, c, rawCommand(2, 0x01, 0x01, []byte{cio.Word, 0, 0, 0, 0, 2}))
	if len(resp.Data) != 4 {
		t.Fatalf("words = % x", resp.Data)
	}
	if binary.BigEndian.Uint16(resp.Data[0:]) != 0x8000 || binary.BigEndian.Uint16(resp.Data[2:]) != 0x0001 {
		t.Fatalf("words = % x", resp.Data)
	}
}

func TestHandshakeRejectsServerNode(t *testing.T) {
	srv := newSim(t)
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// 客户端指定节点 1，与从站自己的节点号冲突：必须回错误码 0x24。
	if _, err := c.Write(finscodec.BuildNodeRequest(1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("read: %v", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if th.Length != 8 || th.Error != 0x24 {
		t.Fatalf("header = %+v", th)
	}
}

func TestUnknownAreaWrite(t *testing.T) {
	srv := newSim(t)
	c := dialRaw(t, srv)
	resp := roundTripRaw(t, c, rawCommand(1, 0x01, 0x02, []byte{0xEE, 0, 0, 0, 0, 1, 0}))
	if resp.EndCode != 0x1101 {
		t.Fatalf("end code = 0x%04X", resp.EndCode)
	}
}
