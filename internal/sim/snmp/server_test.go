package snmp

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	snmpcodec "uic/internal/codec/snmp"
)

// 内置代理的测试用**真实 UDP 回环 + 真实 codec**：手写一条「看起来像
// 应答」的字节串再断言自己的解析器，只能证明解析器自洽，证明不了
// 「发出去的东西能被标准客户端读懂」。

type client struct {
	t    *testing.T
	conn net.Conn
}

func dialAgent(t *testing.T, srv *Server) *client {
	t.Helper()
	c, err := net.DialTimeout("udp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{t: t, conn: c}
}

// send 发一条请求并读回应答；timeout 为真时返回读错误。
func (c *client) send(raw []byte, wait time.Duration) (snmpcodec.Message, error) {
	c.t.Helper()
	if _, err := c.conn.Write(raw); err != nil {
		return snmpcodec.Message{}, err
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return snmpcodec.Message{}, err
	}
	buf := make([]byte, snmpcodec.MaxDatagram)
	n, err := c.conn.Read(buf)
	if err != nil {
		return snmpcodec.Message{}, err
	}
	return snmpcodec.Parse(buf[:n])
}

func (c *client) mustSend(raw []byte) snmpcodec.Message {
	c.t.Helper()
	m, err := c.send(raw, 2*time.Second)
	if err != nil {
		c.t.Fatalf("request: %v", err)
	}
	return m
}

func startAgent(t *testing.T, opt Options) *Server {
	t.Helper()
	srv, err := NewServerWithOptions("127.0.0.1:0", opt)
	if err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func getReq(t *testing.T, v snmpcodec.Version, community string, oids ...string) []byte {
	t.Helper()
	list := make([]snmpcodec.OID, len(oids))
	for i, s := range oids {
		list[i] = snmpcodec.MustParseOID(s)
	}
	raw, err := snmpcodec.GetRequest(v, community, 7, list)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return raw
}

func TestGetAndCounters(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	resp := c.mustSend(getReq(t, snmpcodec.V2c, DefaultCommunity,
		"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.3.0", "1.3.6.1.2.1.1.7.0"))
	if resp.PDU.Type != snmpcodec.PDUResponse || resp.PDU.RequestID != 7 || resp.PDU.ErrorStatus != 0 {
		t.Fatalf("response = %+v", resp.PDU)
	}
	if len(resp.PDU.Varbinds) != 3 {
		t.Fatalf("varbinds = %d", len(resp.PDU.Varbinds))
	}
	if got := resp.PDU.Varbinds[0].Value.String(); !strings.Contains(got, "simulated") {
		t.Fatalf("sysDescr = %q", got)
	}
	if resp.PDU.Varbinds[1].Value.Type != snmpcodec.TypeTimeTicks {
		t.Fatalf("sysUpTime type = %s", resp.PDU.Varbinds[1].Value.Type)
	}
	if resp.PDU.Varbinds[2].Value.Int != 72 {
		t.Fatalf("sysServices = %d", resp.PDU.Varbinds[2].Value.Int)
	}
	if srv.Requests() != 1 {
		t.Fatalf("requests = %d", srv.Requests())
	}
}

// TestConcurrentDispatchIsRaceFree 钉住 handle/handleV3 的锁边界：dispatch
// 读写 s.table / s.sorted / s.writes，必须与测试夹具 Get/Set 共用 s.mu。
// 原先这里只在 s.requests++ 后立刻 Unlock 再无锁调用 dispatch，导致 UDP
// 处理协程（写表）与调用方 Get（读表）在 map 上数据竞争，`go test -race`
// 稳定复现。修复后并发跑一批 SET + Get 必须无竞争。
func TestConcurrentDispatchIsRaceFree(t *testing.T) {
	srv := startAgent(t, Options{})
	const oid = "1.3.6.1.2.1.1.4.0" // sysContact：默认可写
	if err := srv.SetWritable(oid, true); err != nil {
		t.Fatal(err)
	}
	c := dialAgent(t, srv)

	const writers = 4
	const perWriter = 50
	done := make(chan struct{}, writers)
	for w := 0; w < writers; w++ {
		go func(seed int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perWriter; i++ {
				id := int32(seed*perWriter + i)
				pdu, err := snmpcodec.NewSetPDU(id, []snmpcodec.Varbind{
					{OID: snmpcodec.MustParseOID(oid), Value: snmpcodec.String("ops@x.invalid")},
				})
				if err != nil {
					t.Errorf("set pdu: %v", err)
					return
				}
				raw, err := snmpcodec.Build(snmpcodec.Message{
					Version: snmpcodec.V2c, Community: "public", PDU: pdu,
				})
				if err != nil {
					t.Errorf("build set: %v", err)
					return
				}
				if _, err := c.conn.Write(raw); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				// 读应答，避免请求在内核里无限堆积；忽略内容。
				buf := make([]byte, snmpcodec.MaxDatagram)
				_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, _ = c.conn.Read(buf)
			}
		}(w)
	}

	// 一边并发读表，逼出与写表的竞争。
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, ok := srv.Get(oid); !ok {
				t.Errorf("Get(%s) = missing during concurrent SET", oid)
				return
			}
		}
	}()

	for w := 0; w < writers; w++ {
		<-done
	}
	close(stop)
}

// community 不匹配必须静默丢弃：回一条错误会让代理变成口令探测的
// 预言机，真实设备不会这么做。
func TestWrongCommunityIsDropped(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	_, err := c.send(getReq(t, snmpcodec.V2c, "wrong", "1.3.6.1.2.1.1.1.0"), 200*time.Millisecond)
	if err == nil {
		t.Fatal("wrong community must not get a response")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("error = %v, want timeout", err)
	}
	if srv.Requests() != 0 {
		t.Fatalf("requests = %d; a dropped datagram must not count", srv.Requests())
	}

	// 同一端口上正确的 community 仍能工作（代理没有退出循环）。
	resp := c.mustSend(getReq(t, snmpcodec.V2c, DefaultCommunity, "1.3.6.1.2.1.1.1.0"))
	if len(resp.PDU.Varbinds) != 1 {
		t.Fatalf("varbinds = %d", len(resp.PDU.Varbinds))
	}
}

func TestMissingOIDDiffersBetweenVersions(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	v2 := c.mustSend(getReq(t, snmpcodec.V2c, DefaultCommunity, "1.3.6.1.2.1.1.99.0"))
	if v2.PDU.ErrorStatus != 0 {
		t.Fatalf("v2 status = %d", v2.PDU.ErrorStatus)
	}
	if v2.PDU.Varbinds[0].Value.Type != snmpcodec.TypeNoSuchObject {
		t.Fatalf("v2 value = %s", v2.PDU.Varbinds[0].Value.Type)
	}

	v1 := c.mustSend(getReq(t, snmpcodec.V1, DefaultCommunity, "1.3.6.1.2.1.1.99.0"))
	if v1.PDU.ErrorStatus != int(snmpcodec.ErrNoSuchName) || v1.PDU.ErrorIndex != 1 {
		t.Fatalf("v1 status/index = %d/%d", v1.PDU.ErrorStatus, v1.PDU.ErrorIndex)
	}

	// 同一批里第二个 OID 缺失：error-index 必须是 2。
	v1 = c.mustSend(getReq(t, snmpcodec.V1, DefaultCommunity,
		"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.99.0"))
	if v1.PDU.ErrorIndex != 2 {
		t.Fatalf("error-index = %d, want 2", v1.PDU.ErrorIndex)
	}
}

func TestGetNextWalk(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	raw, err := snmpcodec.GetNextRequest(snmpcodec.V2c, DefaultCommunity, 9,
		[]snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")})
	if err != nil {
		t.Fatal(err)
	}
	resp := c.mustSend(raw)
	if got := resp.PDU.Varbinds[0].OID.String(); got != "1.3.6.1.2.1.1.2.0" {
		t.Fatalf("successor of sysDescr = %s", got)
	}

	// 越过表尾：v2c 是 endOfMibView，v1 是 noSuchName。
	raw, _ = snmpcodec.GetNextRequest(snmpcodec.V2c, DefaultCommunity, 10,
		[]snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.9.9")})
	resp = c.mustSend(raw)
	if resp.PDU.Varbinds[0].Value.Type != snmpcodec.TypeEndOfMibView {
		t.Fatalf("end = %s", resp.PDU.Varbinds[0].Value.Type)
	}
	raw, _ = snmpcodec.GetNextRequest(snmpcodec.V1, DefaultCommunity, 11,
		[]snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.9.9")})
	resp = c.mustSend(raw)
	if resp.PDU.ErrorStatus != int(snmpcodec.ErrNoSuchName) {
		t.Fatalf("v1 end status = %d", resp.PDU.ErrorStatus)
	}
}

func TestGetBulk(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	raw, err := snmpcodec.GetBulkRequest(DefaultCommunity, 12,
		[]snmpcodec.OID{snmpcodec.MustParseOID("1.3.6.1.2.1.2.2.1.2")}, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	resp := c.mustSend(raw)
	if len(resp.PDU.Varbinds) != 3 {
		t.Fatalf("varbinds = %d, want 3", len(resp.PDU.Varbinds))
	}
	// 严格递增，且跨过 ifDescr.2 后进入下一列（ifType.1）。
	want := []string{
		"1.3.6.1.2.1.2.2.1.2.1",
		"1.3.6.1.2.1.2.2.1.2.2",
		"1.3.6.1.2.1.2.2.1.3.1",
	}
	for i, w := range want {
		if got := resp.PDU.Varbinds[i].OID.String(); got != w {
			t.Fatalf("varbind[%d] = %s, want %s", i, got, w)
		}
	}
}

func TestSetWritableAndReadBack(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	raw, err := snmpcodec.SetRequest(snmpcodec.V2c, DefaultCommunity, 13, []snmpcodec.Varbind{{
		OID: snmpcodec.MustParseOID("1.3.6.1.2.1.1.6.0"), Value: snmpcodec.String("building-7"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := c.mustSend(raw)
	if resp.PDU.ErrorStatus != 0 {
		t.Fatalf("set status = %d", resp.PDU.ErrorStatus)
	}
	if got := resp.PDU.Varbinds[0].Value.String(); got != "building-7" {
		t.Fatalf("echo = %q", got)
	}
	if v, ok := srv.Get("1.3.6.1.2.1.1.6.0"); !ok || v.String() != "building-7" {
		t.Fatalf("stored = %v %v", v, ok)
	}
	if srv.Writes() != 1 {
		t.Fatalf("writes = %d", srv.Writes())
	}
}

func TestSetErrors(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)
	oid := func(s string) snmpcodec.OID { return snmpcodec.MustParseOID(s) }

	// 只读对象：v2c 是 notWritable，v1 是 readOnly。
	raw, _ := snmpcodec.SetRequest(snmpcodec.V2c, DefaultCommunity, 1, []snmpcodec.Varbind{{
		OID: oid("1.3.6.1.2.1.1.1.0"), Value: snmpcodec.String("x"),
	}})
	if resp := c.mustSend(raw); resp.PDU.ErrorStatus != int(snmpcodec.ErrNotWritable) {
		t.Fatalf("v2 read-only status = %d", resp.PDU.ErrorStatus)
	}
	raw, _ = snmpcodec.SetRequest(snmpcodec.V1, DefaultCommunity, 2, []snmpcodec.Varbind{{
		OID: oid("1.3.6.1.2.1.1.1.0"), Value: snmpcodec.String("x"),
	}})
	if resp := c.mustSend(raw); resp.PDU.ErrorStatus != int(snmpcodec.ErrReadOnly) {
		t.Fatalf("v1 read-only status = %d", resp.PDU.ErrorStatus)
	}

	// 类型不匹配：v2c 是 wrongType，v1 是 badValue。
	raw, _ = snmpcodec.SetRequest(snmpcodec.V2c, DefaultCommunity, 3, []snmpcodec.Varbind{{
		OID: oid("1.3.6.1.2.1.1.6.0"), Value: snmpcodec.Integer(1),
	}})
	if resp := c.mustSend(raw); resp.PDU.ErrorStatus != int(snmpcodec.ErrWrongType) {
		t.Fatalf("wrong type status = %d", resp.PDU.ErrorStatus)
	}

	// 不存在的 OID。
	raw, _ = snmpcodec.SetRequest(snmpcodec.V2c, DefaultCommunity, 4, []snmpcodec.Varbind{{
		OID: oid("1.3.6.1.2.1.1.99.0"), Value: snmpcodec.Integer(1),
	}})
	if resp := c.mustSend(raw); resp.PDU.ErrorStatus != int(snmpcodec.ErrNoCreation) {
		t.Fatalf("missing OID status = %d", resp.PDU.ErrorStatus)
	}
}

// SET 是原子的：第二个 varbind 不合法时第一个也不能生效。
func TestSetIsAtomic(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	raw, err := snmpcodec.SetRequest(snmpcodec.V2c, DefaultCommunity, 5, []snmpcodec.Varbind{
		{OID: snmpcodec.MustParseOID("1.3.6.1.2.1.1.6.0"), Value: snmpcodec.String("new")},
		{OID: snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0"), Value: snmpcodec.String("nope")},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := c.mustSend(raw)
	if resp.PDU.ErrorStatus != int(snmpcodec.ErrNotWritable) || resp.PDU.ErrorIndex != 2 {
		t.Fatalf("status/index = %d/%d", resp.PDU.ErrorStatus, resp.PDU.ErrorIndex)
	}
	if v, _ := srv.Get("1.3.6.1.2.1.1.6.0"); v.String() != "lab" {
		t.Fatalf("first varbind was applied before the second failed: %q", v)
	}
}

func TestMalformedAndNonRequestAreIgnored(t *testing.T) {
	srv := startAgent(t, Options{})
	c := dialAgent(t, srv)

	if _, err := c.send([]byte{0x30, 0x03, 0x02, 0x01}, 100*time.Millisecond); err == nil {
		t.Fatal("malformed datagram must not be answered")
	}
	// 一条 Response（不是请求）也必须被丢弃：代理只回答请求。
	respMsg, err := snmpcodec.Build(snmpcodec.Message{
		Version: snmpcodec.V2c, Community: DefaultCommunity,
		PDU: snmpcodec.PDU{Type: snmpcodec.PDUResponse, RequestID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.send(respMsg, 100*time.Millisecond); err == nil {
		t.Fatal("response PDU must not be answered")
	}
	// 代理仍然活着。
	if resp := c.mustSend(getReq(t, snmpcodec.V2c, DefaultCommunity, "1.3.6.1.2.1.1.1.0")); len(resp.PDU.Varbinds) != 1 {
		t.Fatal("agent stopped serving after malformed datagrams")
	}
}

func TestAddAndWritableControls(t *testing.T) {
	srv := startAgent(t, Options{})
	if err := srv.Set("1.3.6.1.4.1.99999.7.0", snmpcodec.Integer(5)); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, ok := srv.Get("1.3.6.1.4.1.99999.7.0"); !ok || v.Int != 5 {
		t.Fatalf("get = %v %v", v, ok)
	}
	if err := srv.SetWritable("1.3.6.1.2.1.1.6.0", false); err != nil {
		t.Fatalf("writable: %v", err)
	}
	if err := srv.SetWritable("1.3.6.1.2.1.1.99.0", true); err == nil {
		t.Fatal("unknown OID must fail")
	}
	if err := srv.Set("bad", snmpcodec.Integer(1)); err == nil {
		t.Fatal("bad OID must fail")
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
