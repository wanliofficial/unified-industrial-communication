package s7

import (
	"bytes"
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

// roundTrip 发一条 PDU 并把应答解析成 s7.PDU。
func roundTrip(t *testing.T, c net.Conn, pdu []byte) s7.PDU {
	t.Helper()
	writeFrame(t, c, s7.WrapDT(pdu))
	resp, err := s7.Parse(readFrame(t, c))
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp
}

func userData(t *testing.T, pdu s7.PDU) s7.UserData {
	t.Helper()
	ud, err := s7.ParseUserData(pdu)
	if err != nil {
		t.Fatalf("userdata: %v", err)
	}
	return ud
}

// 从站必须自己也知道「块目录」的三条格式差异：ListAll 的数据区是
// `0A 00 0000`、块号列表每项 4 字节、块属性是 78 字节。主站测试会掩盖
// 从站单方面的错误（两边一起错就永远对不上真实 PLC），所以这里从字节
// 层面独立断言一次。
func TestBlockDirectory(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	srv.SetBytes(s7.AreaDB, 2, 0, []byte{1, 2, 3})
	srv.SetBlock(s7.BlockFB, 3, []byte{9})

	c := dialSim(t, srv)
	handshake(t, c, 480)

	ud := userData(t, roundTrip(t, c, s7.BuildListBlocks(2)))
	if ud.Group != s7.UserDataGroupBlocks || ud.Subfunc != s7.UserDataListAll {
		t.Fatalf("header = %+v", ud)
	}
	if len(ud.Payload) != 28 {
		t.Fatalf("count payload = %d bytes, want 28", len(ud.Payload))
	}
	counts, err := s7.ParseListBlocks(ud.Payload)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	got := map[byte]int{}
	for _, c := range counts {
		got[c.Type] = c.Count
	}
	// 默认 OB1 + DB1，加上手工注册的 DB2 与 FB3。
	if got[s7.BlockOB] != 1 || got[s7.BlockDB] != 2 || got[s7.BlockFB] != 1 {
		t.Fatalf("counts = %v", got)
	}

	ud = userData(t, roundTrip(t, c, s7.BuildListBlocksOfType(3, s7.BlockDB)))
	nums, err := s7.ParseListBlocksOfType(ud.Payload)
	if err != nil {
		t.Fatalf("db list: %v", err)
	}
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Fatalf("db numbers = %v, want [1 2]", nums)
	}

	req, err := s7.BuildBlockInfo(4, s7.BlockDB, 2)
	if err != nil {
		t.Fatalf("build info: %v", err)
	}
	info, err := s7.ParseBlockInfo(userData(t, roundTrip(t, c, req)).Payload)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Type != s7.BlockDB || info.Number != 2 || info.LoadSize != 3 {
		t.Fatalf("info = %+v", info)
	}

	// 未知子功能不崩连接，返回同组同子功能的空负载。
	ud = userData(t, roundTrip(t, c, s7.BuildUserData(5, s7.UserDataGroupBlocks, 0x09, nil)))
	if ud.Subfunc != 0x09 || len(ud.Payload) != 0 {
		t.Fatalf("unknown subfunc = %+v", ud)
	}
}

// 上传必须真的按协商 PDU 分片：240 的 PDU 每片最多 222 字节。
// 只测单片的实现会让「永远只发第一片」的 bug 全绿。
func TestBlockUploadFragmentsAndEnd(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	want := make([]byte, 1000)
	for i := range want {
		want[i] = byte(i*13 + 1)
	}
	srv.SetBlock(s7.BlockDB, 5, want)

	c := dialSim(t, srv)
	handshake(t, c, 240)

	req, err := s7.BuildStartUpload(2, s7.BlockDB, 5)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	start, err := s7.ParseStartUpload(roundTrip(t, c, req))
	if err != nil {
		t.Fatalf("start ack: %v", err)
	}
	if start.Length != len(want) {
		t.Fatalf("length = %d, want %d", start.Length, len(want))
	}

	var got []byte
	for i := 0; i < 100; i++ {
		frag, err := s7.ParseUpload(roundTrip(t, c, s7.BuildUpload(3, start.ID)))
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		got = append(got, frag.Data...)
		if frag.Last {
			break
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("uploaded %d bytes, mismatch", len(got))
	}

	if err := s7.ParseEndUpload(roundTrip(t, c, s7.BuildEndUpload(4, start.ID))); err != nil {
		t.Fatalf("end: %v", err)
	}

	// End 之后这个会话必须真的不存在了：再取一片会被当成协议错误断链，
	// 而不是悄悄重放上一份数据。
	writeFrame(t, c, s7.WrapDT(s7.BuildUpload(5, start.ID)))
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c, make([]byte, 1)); err == nil {
		t.Fatal("upload after end must close the connection")
	}
}

// 未知块在模拟器里是「存在但为空」：目标是让主站的完整流程可跑，
// 而不是复刻真实 PLC 的「对象不存在」错误码。
func TestBlockUploadUnknownIsEmpty(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)
	handshake(t, c, 480)

	req, _ := s7.BuildStartUpload(2, s7.BlockFC, 99)
	start, err := s7.ParseStartUpload(roundTrip(t, c, req))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if start.Length != 0 {
		t.Fatalf("length = %d, want 0", start.Length)
	}
	frag, err := s7.ParseUpload(roundTrip(t, c, s7.BuildUpload(3, start.ID)))
	if err != nil {
		t.Fatalf("fragment: %v", err)
	}
	if !frag.Last || len(frag.Data) != 0 {
		t.Fatalf("fragment = %+v", frag)
	}
	if err := s7.ParseEndUpload(roundTrip(t, c, s7.BuildEndUpload(4, start.ID))); err != nil {
		t.Fatalf("end: %v", err)
	}
}

func TestBlockRequestsRejectBadParams(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	cases := []struct {
		name string
		pdu  []byte
	}{
		// 开始上传的参数区必须 18 字节且地址长度 9。
		{"start short", s7.BuildPDU(s7.ROSCTRJob, 2, []byte{s7.FuncStartUpload}, nil)},
		// 地址开头不是 `_0`。
		{"start bad address", s7.BuildPDU(s7.ROSCTRJob, 2,
			append([]byte{s7.FuncStartUpload, 0, 0, 0, 0, 0, 0, 0, 9}, []byte("X0A00001A")...), nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dialSim(t, srv)
			handshake(t, c, 480)
			writeFrame(t, c, s7.WrapDT(tc.pdu))
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(c, make([]byte, 1)); err == nil {
				t.Fatal("bad block request must close the connection")
			}
		})
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
