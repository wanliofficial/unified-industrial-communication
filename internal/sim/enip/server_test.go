package enip

import (
	"encoding/binary"
	"io"
	"net"
	"testing"

	enipcodec "uic/internal/codec/enip"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// rawClient 直接说 EtherNet/IP，不经过驱动：驱动只会发「正确」的报文，
// 而现场会遇到会话失效、未知服务、类型不匹配等错误路径。
type rawClient struct {
	t    *testing.T
	conn net.Conn
	sid  uint32
	seq  uint64
}

func dialRaw(t *testing.T, srv *Server) *rawClient {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawClient{t: t, conn: c}
}

func (c *rawClient) send(cmd uint16, session uint32, payload []byte) (enipcodec.Header, []byte) {
	c.t.Helper()
	c.seq++
	var ctx [8]byte
	binary.LittleEndian.PutUint64(ctx[:], c.seq)
	pkt, err := enipcodec.BuildPacket(cmd, session, 0, ctx, payload)
	if err != nil {
		c.t.Fatalf("build: %v", err)
	}
	if _, err := c.conn.Write(pkt); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	head := make([]byte, enipcodec.HeaderLen)
	if _, err := io.ReadFull(c.conn, head); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	h, err := enipcodec.ParseHeader(head)
	if err != nil {
		c.t.Fatalf("parse header: %v", err)
	}
	body := make([]byte, h.Length)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return h, body
}

func (c *rawClient) register() {
	c.t.Helper()
	h, body := c.send(enipcodec.CmdRegisterSession, 0, enipcodec.BuildRegisterSession(1))
	if h.Status != 0 || h.Session == 0 {
		c.t.Fatalf("register: session=%d status=%d", h.Session, h.Status)
	}
	if v, _, err := enipcodec.ParseRegisterSession(body); err != nil || v != 1 {
		c.t.Fatalf("register payload: v=%d err=%v", v, err)
	}
	c.sid = h.Session
}

func (c *rawClient) cip(req []byte) (enipcodec.Reply, enipcodec.Header) {
	c.t.Helper()
	payload, err := enipcodec.BuildSendRRData(req)
	if err != nil {
		c.t.Fatalf("rrdata: %v", err)
	}
	h, body := c.send(enipcodec.CmdSendRRData, c.sid, payload)
	if h.Status != 0 {
		return enipcodec.Reply{}, h
	}
	raw, err := enipcodec.ParseSendRRData(body)
	if err != nil {
		c.t.Fatalf("parse rrdata: %v", err)
	}
	reply, err := enipcodec.ParseReply(raw)
	if err != nil {
		c.t.Fatalf("parse reply: %v", err)
	}
	return reply, h
}

func TestRegisterAndUnregister(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()
	// 注销后同一会话句柄必须被判无效（0x64）。
	pkt, _ := enipcodec.BuildPacket(enipcodec.CmdUnregister, c.sid, 0, [8]byte{}, nil)
	if _, err := c.conn.Write(pkt); err != nil {
		t.Fatal(err)
	}
	// 注销没有应答；紧接着发一个 SendRRData，设备应回 0x64。
	req, _ := enipcodec.BuildReadTag("DINT_TAG", 1)
	payload, _ := enipcodec.BuildSendRRData(req)
	h, _ := c.send(enipcodec.CmdSendRRData, c.sid, payload)
	if h.Status != 0x64 {
		t.Fatalf("status = %d, want 0x64", h.Status)
	}
}

func TestBadProtocolVersion(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	h, _ := c.send(enipcodec.CmdRegisterSession, 0, enipcodec.BuildRegisterSession(2))
	if h.Status != 0x69 {
		t.Fatalf("status = %d, want 0x69", h.Status)
	}
}

func TestUnknownCommand(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	h, _ := c.send(enipcodec.CmdListIdentity, 0, nil)
	if h.Status != 0x01 {
		t.Fatalf("status = %d, want 0x01", h.Status)
	}
}

func TestReadTagOverRaw(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()

	req, _ := enipcodec.BuildReadTag("dint_tag", 1)
	reply, _ := c.cip(req)
	typ, count, data, err := enipcodec.ParseReadTagReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	if typ != enipcodec.TypeDINT || count != 1 || binary.LittleEndian.Uint32(data) != 1234 {
		t.Fatalf("type/count/data = %02X/%d/% x", typ, count, data)
	}

	// 超出标签元素数：0x15（too much data），不是静默返回一部分。
	req, _ = enipcodec.BuildReadTag("DINT_TAG", 2)
	reply, _ = c.cip(req)
	if reply.Status != 0x15 {
		t.Fatalf("status = %d, want 0x15", reply.Status)
	}

	// 未知标签：0x05（path destination unknown）。
	req, _ = enipcodec.BuildReadTag("GHOST", 1)
	reply, _ = c.cip(req)
	if reply.Status != 0x05 {
		t.Fatalf("status = %d, want 0x05", reply.Status)
	}

	// 0 个元素：0x03（invalid parameter value）。
	req, _ = enipcodec.BuildReadTag("DINT_TAG", 1)
	req[len(req)-2] = 0
	req[len(req)-1] = 0
	reply, _ = c.cip(req)
	if reply.Status != 0x03 {
		t.Fatalf("status = %d, want 0x03", reply.Status)
	}
}

func TestWriteTagOverRaw(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()

	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 777)
	req, _ := enipcodec.BuildWriteTag("DINT_TAG", enipcodec.TypeDINT, 1, data)
	reply, _ := c.cip(req)
	if reply.Status != 0 {
		t.Fatalf("write status = %d", reply.Status)
	}
	if _, _, got, _ := srv.Tag("DINT_TAG"); binary.LittleEndian.Uint32(got) != 777 {
		t.Fatalf("tag = % x", got)
	}

	// 类型不匹配：0x03。
	req, _ = enipcodec.BuildWriteTag("DINT_TAG", enipcodec.TypeREAL, 1, data)
	reply, _ = c.cip(req)
	if reply.Status != 0x03 {
		t.Fatalf("type mismatch status = %d, want 0x03", reply.Status)
	}

	// 数据不足：0x13（not enough data）。
	req, _ = enipcodec.BuildWriteTag("DINT_TAG", enipcodec.TypeDINT, 1, data[:2])
	reply, _ = c.cip(req)
	if reply.Status != 0x13 {
		t.Fatalf("short data status = %d, want 0x13", reply.Status)
	}

	// 元素数超过标签容量：0x15。
	req, _ = enipcodec.BuildWriteTag("DINT_TAG", enipcodec.TypeDINT, 2, append(data, data...))
	reply, _ = c.cip(req)
	if reply.Status != 0x15 {
		t.Fatalf("too many status = %d, want 0x15", reply.Status)
	}
}

func TestWriteStringOverRaw(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()

	data, _ := enipcodec.EncodeString("NEW-NAME", 82)
	req, _ := enipcodec.BuildWriteTag("STRING_TAG", enipcodec.TypeSTRING, 1, data)
	reply, _ := c.cip(req)
	if reply.Status != 0 {
		t.Fatalf("status = %d", reply.Status)
	}
	_, _, raw, _ := srv.Tag("STRING_TAG")
	if got, _ := enipcodec.DecodeString(raw); got != "NEW-NAME" {
		t.Fatalf("string = %q", got)
	}
}

func TestMultipleServiceOverRaw(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()

	a, _ := enipcodec.BuildReadTag("DINT_TAG", 1)
	b, _ := enipcodec.BuildReadTag("GHOST", 1)
	pkt, err := enipcodec.BuildMultiple([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := c.cip(pkt)
	if reply.Status != 0 {
		t.Fatalf("multiple status = %d", reply.Status)
	}
	replies, err := enipcodec.ParseMultipleReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 2 {
		t.Fatalf("replies = %d", len(replies))
	}
	r0, _ := enipcodec.ParseReply(replies[0])
	if r0.Status != 0 {
		t.Fatalf("first status = %d", r0.Status)
	}
	// 嵌入错误必须按条目返回，而不是让整个多服务请求失败。
	r1, _ := enipcodec.ParseReply(replies[1])
	if r1.Status != 0x05 {
		t.Fatalf("second status = %d, want 0x05", r1.Status)
	}
}

func TestMultipleDisabled(t *testing.T) {
	srv := newTestServer(t)
	srv.SetMultipleSupported(false)
	c := dialRaw(t, srv)
	c.register()

	a, _ := enipcodec.BuildReadTag("DINT_TAG", 1)
	pkt, _ := enipcodec.BuildMultiple([][]byte{a})
	reply, _ := c.cip(pkt)
	if reply.Status != 0x08 {
		t.Fatalf("status = %d, want 0x08", reply.Status)
	}
}

func TestUnknownSymbolicService(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()

	path, _ := enipcodec.SymbolicPath("DINT_TAG")
	req, _ := enipcodec.BuildRequest(0x33, path, nil)
	reply, _ := c.cip(req)
	if reply.Status != 0x08 {
		t.Fatalf("status = %d, want 0x08", reply.Status)
	}
}

func TestNonSymbolicPathRejected(t *testing.T) {
	srv := newTestServer(t)
	c := dialRaw(t, srv)
	c.register()

	path, _ := enipcodec.LogicalPath(1, 1)
	req, _ := enipcodec.BuildRequest(enipcodec.ServiceReadTag, path, []byte{1, 0})
	reply, _ := c.cip(req)
	if reply.Status != 0x04 {
		t.Fatalf("status = %d, want 0x04", reply.Status)
	}
}

func TestSetTagValidation(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.SetTag("X", enipcodec.TypeDINT, 1, []byte{1}); err == nil {
		t.Fatal("short DINT accepted")
	}
	if err := srv.SetTag("S", enipcodec.TypeSTRING, 2, make([]byte, 8)); err == nil {
		t.Fatal("multi-element STRING accepted")
	}
	if err := srv.SetTag("S", enipcodec.TypeSTRING, 1, []byte{1}); err == nil {
		t.Fatal("short STRING accepted")
	}
	if err := srv.SetTag("B", enipcodec.TypeStruct, 1, make([]byte, 4)); err == nil {
		t.Fatal("struct tag accepted")
	}
}

func TestDefaultsAndCounter(t *testing.T) {
	srv := newTestServer(t)
	for name, want := range map[string]byte{
		"dint_tag": enipcodec.TypeDINT, "int_tag": enipcodec.TypeINT,
		"real_tag": enipcodec.TypeREAL, "bool_tag": enipcodec.TypeBOOL,
		"string_tag": enipcodec.TypeSTRING, "counter": enipcodec.TypeDINT,
	} {
		typ, _, _, ok := srv.Tag(name)
		if !ok || typ != want {
			t.Fatalf("%s type = %02X ok=%v", name, typ, ok)
		}
	}
	if _, count, data, _ := srv.Tag("DINT_ARRAY"); count != 4 || len(data) != 16 {
		t.Fatalf("array = count %d len %d", count, len(data))
	}
}
