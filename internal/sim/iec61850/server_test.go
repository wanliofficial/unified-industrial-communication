package iec61850

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	ieccodec "uic/internal/codec/iec61850"
)

// 这一组测试直接拿 codec 当客户端，对内置从站做**真实 TCP** 上的完整往返。
//
// 为什么不只在驱动层测：驱动测试会掩盖从站自身的协议错误——驱动宽容地跳过
// 坏帧、重试、重连，最后仍可能得到一个「看起来对」的结果。从站必须独立证明
// 自己产出的字节能被一个不懂内部结构、只按标准解码的客户端读懂。这里用的
// 客户端全部是 codec 的公开函数，与驱动无关。

// client 是一个最小 MMS 客户端，只做握手 + 单次请求/响应。
type client struct {
	t    *testing.T
	conn net.Conn
}

func dialSim(t *testing.T, srv *Server) *client {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial sim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &client{t: t, conn: conn}
}

func (cl *client) write(b []byte) {
	cl.t.Helper()
	_ = cl.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := cl.conn.Write(b); err != nil {
		cl.t.Fatalf("write: %v", err)
	}
}

// readFrame 读一个完整 TPKT 帧（含 4 字节头）。
func (cl *client) readFrame(timeout time.Duration) ([]byte, error) {
	_ = cl.conn.SetReadDeadline(time.Now().Add(timeout))
	head := make([]byte, 4)
	if _, err := readFull(cl.conn, head); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(head[2:4]))
	if n < 4 || n > 8192 {
		return nil, errors.New("client: bad tpkt length")
	}
	body := make([]byte, n-4)
	if _, err := readFull(cl.conn, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

// associate 走完 COTP CR/CC → 会话 CN/AC → 表示 CP/CPA → ACSE AARQ/AARE。
func (cl *client) associate() (ieccodec.AARE, error) {
	cl.t.Helper()
	cl.write(ieccodec.COTPConnect())
	frame, err := cl.readFrame(2 * time.Second)
	if err != nil {
		return ieccodec.AARE{}, err
	}
	if _, err := ieccodec.ParseCOTPConfirm(frame); err != nil {
		return ieccodec.AARE{}, err
	}

	cn, err := ieccodec.BuildConnectPDU(ieccodec.DefaultInitiateRequest(), nil)
	if err != nil {
		return ieccodec.AARE{}, err
	}
	cl.write(ieccodec.BuildCOTPData(cn))

	frame, err = cl.readFrame(2 * time.Second)
	if err != nil {
		return ieccodec.AARE{}, err
	}
	session, _, err := ieccodec.ParseCOTPData(frame)
	if err != nil {
		return ieccodec.AARE{}, err
	}
	aare, _, err := ieccodec.ParseConnectResponse(session)
	return aare, err
}

// request 发一个完整的 confirmed-request，返回 MMS 响应体。
func (cl *client) request(req []byte) ([]byte, error) {
	cl.t.Helper()
	dt, err := ieccodec.WrapData(req)
	if err != nil {
		return nil, err
	}
	cl.write(ieccodec.BuildCOTPData(dt))
	frame, err := cl.readFrame(2 * time.Second)
	if err != nil {
		return nil, err
	}
	session, _, err := ieccodec.ParseCOTPData(frame)
	if err != nil {
		return nil, err
	}
	return ieccodec.UnwrapData(session)
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// ---------------------------------------------------------------------------
// 变量树
// ---------------------------------------------------------------------------

// 默认变量树里的每个值都必须能被 codec 的值转换读懂——从站与驱动共用
// DataFromValue/ToValue 这一对函数，任何类型对不上都会在这里先暴露。
func TestDefaultObjectsAreReadable(t *testing.T) {
	objs := DefaultObjects()
	if len(objs) < 6 {
		t.Fatalf("default objects = %d, want at least 6", len(objs))
	}
	for name, data := range objs {
		raw, err := data.ToValue()
		if err != nil {
			t.Errorf("%s: ToValue: %v", name, err)
			continue
		}
		if raw == nil {
			t.Errorf("%s: nil value", name)
		}
	}
	// 探针对象与三类型的代表点必须存在。
	for _, want := range []string{
		"simpleIO$LLN0$ST$Health",
		"simpleIO$GGIO1$ST$Ind1$stVal",
		"simpleIO$GGIO1$MX$PhV$mag$f",
		"simpleIO$GGIO1$CO$SPCs$stVal",
	} {
		if _, ok := objs[want]; !ok {
			t.Errorf("default tree is missing %q", want)
		}
	}
}

func TestNamesAreSorted(t *testing.T) {
	srv := newTestServer(t)
	names := srv.Names()
	if len(names) < 2 {
		t.Fatalf("names = %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("names not sorted at %d: %q > %q", i, names[i-1], names[i])
		}
	}
}

func TestSetAndGet(t *testing.T) {
	srv := newTestServer(t)
	srv.SetBool("simpleIO$GGIO1$ST$Ind2$stVal", true)
	d, ok := srv.Get("simpleIO$GGIO1$ST$Ind2$stVal")
	if !ok || !d.Bool {
		t.Fatalf("Get = %+v ok=%v", d, ok)
	}
	if _, ok := srv.Get("simpleIO$nope"); ok {
		t.Fatal("Get returned a value for an unknown name")
	}
}

// ---------------------------------------------------------------------------
// 读 / 写 / 模型浏览
// ---------------------------------------------------------------------------

func TestReadOverTCP(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}

	names := []ieccodec.ObjectName{
		ieccodec.DomainObject("simpleIO", "GGIO1$ST$Ind1$stVal"),
		ieccodec.DomainObject("simpleIO", "GGIO1$MX$PhV$mag$f"),
	}
	req, err := ieccodec.BuildReadRequest(1, ieccodec.ReadRequest{Names: names})
	if err != nil {
		t.Fatalf("BuildReadRequest: %v", err)
	}
	mms, err := cl.request(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp, err := ieccodec.ParseReadResponse(mms)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	if !resp.Results[0].OK || !resp.Results[0].Data.Bool {
		t.Errorf("first = %+v", resp.Results[0])
	}
	if !resp.Results[1].OK || resp.Results[1].Data.Float != 230.5 {
		t.Errorf("second = %+v", resp.Results[1])
	}
	if srv.ReadCount() != 1 {
		t.Errorf("read count = %d, want 1", srv.ReadCount())
	}
}

func TestReadUnknownObjectIsAnItemError(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}
	req, err := ieccodec.BuildReadRequest(1, ieccodec.ReadRequest{Names: []ieccodec.ObjectName{
		ieccodec.DomainObject("simpleIO", "GGIO1$ST$Ghost$stVal"),
	}})
	if err != nil {
		t.Fatalf("BuildReadRequest: %v", err)
	}
	mms, err := cl.request(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp, err := ieccodec.ParseReadResponse(mms)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].OK {
		t.Fatalf("results = %+v", resp.Results)
	}
	if resp.Results[0].Error != ieccodec.DataErrObjectNonExistent {
		t.Errorf("error = %d, want object-non-existent", resp.Results[0].Error)
	}
}

func TestWriteOverTCP(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}

	const key = "simpleIO$GGIO1$CO$SPCs$stVal"
	val, err := ieccodec.DataFromValue(ieccodec.Value{Type: "bool", Bool: true})
	if err != nil {
		t.Fatalf("DataFromValue: %v", err)
	}
	req, err := ieccodec.BuildWriteRequest(2, ieccodec.WriteRequest{
		Names:  []ieccodec.ObjectName{ieccodec.DomainObject("simpleIO", "GGIO1$CO$SPCs$stVal")},
		Values: []ieccodec.Data{val},
	})
	if err != nil {
		t.Fatalf("BuildWriteRequest: %v", err)
	}
	mms, err := cl.request(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ieccodec.ParseWriteResponse(mms)
	if err != nil {
		t.Fatalf("ParseWriteResponse: %v", err)
	}
	if len(resp.Results) != 1 || !resp.Results[0].OK {
		t.Fatalf("results = %+v", resp.Results)
	}
	d, ok := srv.Get(key)
	if !ok || !d.Bool {
		t.Fatalf("sim value = %+v ok=%v, want true", d, ok)
	}
	if srv.WriteCount() != 1 {
		t.Errorf("write count = %d, want 1", srv.WriteCount())
	}
}

func TestWriteUnknownObjectFails(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}
	val, err := ieccodec.DataFromValue(ieccodec.Value{Type: "bool", Bool: true})
	if err != nil {
		t.Fatalf("DataFromValue: %v", err)
	}
	req, err := ieccodec.BuildWriteRequest(3, ieccodec.WriteRequest{
		Names:  []ieccodec.ObjectName{ieccodec.DomainObject("simpleIO", "GGIO1$CO$Ghost")},
		Values: []ieccodec.Data{val},
	})
	if err != nil {
		t.Fatalf("BuildWriteRequest: %v", err)
	}
	mms, err := cl.request(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ieccodec.ParseWriteResponse(mms)
	if err != nil {
		t.Fatalf("ParseWriteResponse: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].OK {
		t.Fatalf("results = %+v", resp.Results)
	}
	if resp.Results[0].Error != ieccodec.DataErrObjectNonExistent {
		t.Errorf("error = %d", resp.Results[0].Error)
	}
}

// 分页：每页 3 项，moreFollows 与 used ContinueAfter 必须能驱动客户端一页页读完。
func TestGetNameListPagination(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}

	want := itemNames(srv.Names(), "simpleIO")
	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		req := ieccodec.BuildGetNameListRequest(uint32(page+1), ieccodec.GetNameListRequest{
			Class: ieccodec.ObjectClassNamedVariable, Domain: "simpleIO", ContinueAfter: cursor,
		})
		mms, err := cl.request(req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		resp, err := ieccodec.ParseGetNameListResponse(mms)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(resp.Identifiers) == 0 {
			t.Fatalf("page %d: empty page before the end", page)
		}
		got = append(got, resp.Identifiers...)
		if !resp.MoreFollows {
			break
		}
		cursor = resp.Identifiers[len(resp.Identifiers)-1]
	}
	if len(got) != len(want) {
		t.Fatalf("collected %d names, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("name[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 故障注入
// ---------------------------------------------------------------------------

// 拒绝关联：AARE 的结果码必须是 1（永久拒绝），且不计入成功关联。
func TestRejectAssociation(t *testing.T) {
	srv := newTestServer(t)
	srv.RejectAssociation(true)
	cl := dialSim(t, srv)
	aare, err := cl.associate()
	if err == nil {
		t.Fatal("expected the association to be rejected")
	}
	if aare.Result != 1 {
		t.Errorf("aare.Result = %d, want 1 (permanent rejection)", aare.Result)
	}
	if srv.AssocCount() != 0 {
		t.Errorf("assoc count = %d, want 0", srv.AssocCount())
	}
}

// DropReads：从站刻意不回读响应，客户端必须靠自己的 deadline 超时。
func TestDropReadsTimesOut(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}
	srv.DropReads(true)

	req, err := ieccodec.BuildReadRequest(9, ieccodec.ReadRequest{Names: []ieccodec.ObjectName{
		ieccodec.DomainObject("simpleIO", "GGIO1$ST$Ind1$stVal"),
	}})
	if err != nil {
		t.Fatalf("BuildReadRequest: %v", err)
	}
	dt, err := ieccodec.WrapData(req)
	if err != nil {
		t.Fatalf("WrapData: %v", err)
	}
	cl.write(ieccodec.BuildCOTPData(dt))

	if _, err := cl.readFrame(300 * time.Millisecond); err == nil {
		t.Fatal("expected a read timeout while DropReads is on")
	} else if !isTimeoutErr(err) {
		t.Fatalf("error = %v, want a timeout", err)
	}
	// 超时之后连接仍然是可用的：从站没有关连接，恢复投递后能继续读。
	srv.DropReads(false)
	mms, err := cl.request(req)
	if err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	resp, err := ieccodec.ParseReadResponse(mms)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if !resp.Results[0].OK {
		t.Errorf("results = %+v", resp.Results)
	}
}

// 有序释放：会话 FN 必须回一个可解析的 RLRE，而不是把连接晾在那里。
func TestReleaseAssociation(t *testing.T) {
	srv := newTestServer(t)
	cl := dialSim(t, srv)
	if _, err := cl.associate(); err != nil {
		t.Fatalf("associate: %v", err)
	}
	rel, err := ieccodec.BuildReleasePDU()
	if err != nil {
		t.Fatalf("BuildReleasePDU: %v", err)
	}
	cl.write(ieccodec.BuildCOTPData(rel))
	frame, err := cl.readFrame(2 * time.Second)
	if err != nil {
		t.Fatalf("release response: %v", err)
	}
	session, _, err := ieccodec.ParseCOTPData(frame)
	if err != nil {
		t.Fatalf("ParseCOTPData: %v", err)
	}
	if got := ieccodec.ParseRelease(session); got != "rlre" && !strings.HasPrefix(got, "finish") {
		t.Errorf("release response = %q, want an RLRE/finish", got)
	}
}

func isTimeoutErr(err error) bool {
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// itemNames 把变量树的整键（域 + "$" + 项）折成 MMS 项名。
//
// getNameList 回的是域内的项名，域由 objectScope 单独携带；整键只是本包内部
// 的扁平索引形式，不能直接当作 wire 上的 identifier 断言。
func itemNames(keys []string, domain string) []string {
	prefix := domain + "$"
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k[len(prefix):])
		}
	}
	return out
}
