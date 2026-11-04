package s7

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"uic/internal/codec/s7"
)

// 从站自己的测试只关心「字节层面是否符合协议」：主站测试已经覆盖了
// 端到端语义，这里补的是从站独有的边界——协商值、越界返回码、只读区。

func dialSim(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func writeFrame(t *testing.T, c net.Conn, frame []byte) {
	t.Helper()
	if _, err := c.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFrame(t *testing.T, c net.Conn) []byte {
	t.Helper()
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := int(binary.BigEndian.Uint16(head[2:4]))
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return append(head, body...)
}

func handshake(t *testing.T, c net.Conn, want int) {
	t.Helper()
	writeFrame(t, c, s7.COTPConnect())
	cc := readFrame(t, c)
	if err := s7.ParseCOTPConnect(cc); err != nil {
		t.Fatalf("cc: %v", err)
	}
	setup := s7.WrapDT(s7.BuildPDU(s7.ROSCTRJob, 1, s7.SetupParams(960), nil))
	writeFrame(t, c, setup)
	resp, err := s7.Parse(readFrame(t, c))
	if err != nil {
		t.Fatalf("setup parse: %v", err)
	}
	n, err := s7.ParseSetupAck(resp)
	if err != nil {
		t.Fatalf("setup ack: %v", err)
	}
	if n != want {
		t.Fatalf("negotiated = %d, want %d", n, want)
	}
}

func TestHandshakeNegotiatesConfiguredPDU(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	handshake(t, dialSim(t, srv), 240)
}

func TestReadOutOfRangeAndMissingDB(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)
	handshake(t, c, 480)

	// 两个 item：一个越界、一个不存在的 DB。
	items := []s7.Item{
		{Area: s7.AreaDB, DB: 1, ByteAddr: 5000, Bit: -1, Bytes: 2},
		{Area: s7.AreaDB, DB: 99, ByteAddr: 0, Bit: -1, Bytes: 2},
	}
	pdu, _ := s7.BuildRead(2, items)
	writeFrame(t, c, s7.WrapDT(pdu))
	resp, err := s7.Parse(readFrame(t, c))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, errs, err := s7.ParseReadAck(resp, items)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := errs[0].(s7.ItemError).Code; got != s7.CodeInvalidAddr {
		t.Fatalf("oob code = %#x", got)
	}
	if got := errs[1].(s7.ItemError).Code; got != s7.CodeObjectNotFound {
		t.Fatalf("missing db code = %#x", got)
	}
}

func TestWriteReadOnlyArea(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)
	handshake(t, c, 480)

	items := []s7.Item{{Area: s7.AreaInput, ByteAddr: 0, Bit: -1, Bytes: 2}}
	pdu, err := s7.BuildWrite(3, items, [][]byte{{0x00, 0x01}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	writeFrame(t, c, s7.WrapDT(pdu))
	resp, perr := s7.Parse(readFrame(t, c))
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	errs, perr := s7.ParseWriteAck(resp, 1)
	if perr != nil {
		t.Fatalf("ack: %v", perr)
	}
	if errs[0] == nil || errs[0].(s7.ItemError).Code != s7.CodeAccessDenied {
		t.Fatalf("input write must be denied: %v", errs[0])
	}
}

func TestWriteBitPreservesNeighbours(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)
	handshake(t, c, 480)

	srv.SetBit(s7.AreaDB, 1, 0, 0, true)
	srv.SetBit(s7.AreaDB, 1, 0, 7, true)

	items := []s7.Item{{Area: s7.AreaDB, DB: 1, ByteAddr: 0, Bit: 3, BitAccess: true}}
	pdu, err := s7.BuildWrite(4, items, [][]byte{{1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	writeFrame(t, c, s7.WrapDT(pdu))
	resp, err := s7.Parse(readFrame(t, c))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errs, err := s7.ParseWriteAck(resp, 1); err != nil || errs[0] != nil {
		t.Fatalf("write ack: %v %v", err, errs)
	}
	if !srv.Bit(s7.AreaDB, 1, 0, 0) || !srv.Bit(s7.AreaDB, 1, 0, 3) || !srv.Bit(s7.AreaDB, 1, 0, 7) {
		t.Fatalf("bits = %08b", srv.Bytes(s7.AreaDB, 1, 0, 1)[0])
	}
}

func TestUnknownPDUClosesConnection(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)
	handshake(t, c, 480)

	// 0x99 不是本从站实现的业务（也永远不会是）。
	pdu := s7.BuildPDU(s7.ROSCTRJob, 9, []byte{0x99}, nil)
	writeFrame(t, c, s7.WrapDT(pdu))
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c, make([]byte, 1)); err == nil {
		t.Fatal("unknown PDU must close the connection")
	}
}
