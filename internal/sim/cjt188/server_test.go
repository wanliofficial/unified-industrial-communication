package cjt188

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net"
	"testing"
	"time"

	cj "uic/internal/codec/cjt188"
)

// 从站的价值在于「能让主站对着一个行为正确的表跑起来」。因此这里的用例
// 全部通过真实字节流（loopback TCP 与 net.Pipe）驱动，而不是直接调
// handle()：组帧、地址匹配、异常应答的形状都必须和真表一致。

func mustDI(t *testing.T, s string) cj.DI {
	t.Helper()
	di, err := cj.ParseDI(s)
	if err != nil {
		t.Fatalf("DI %s: %v", s, err)
	}
	return di
}

func mustAddr(t *testing.T, s string) cj.Addr {
	t.Helper()
	a, err := cj.ParseAddr(s)
	if err != nil {
		t.Fatalf("addr %s: %v", s, err)
	}
	return a
}

func dialSim(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// roundTrip 发一帧并读回一帧；从站静默时返回 ok=false。
func roundTrip(t *testing.T, c net.Conn, req []byte, wait time.Duration) (cj.Frame, bool) {
	t.Helper()
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := c.Read(tmp)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return cj.Frame{}, false
			}
			t.Fatalf("read: %v", err)
		}
		buf = append(buf, tmp[:n]...)
		fr, _, perr := cj.Parse(buf)
		if perr == nil {
			return fr, true
		}
	}
}

func readCurrent(t *testing.T, srv *Server) float64 {
	t.Helper()
	c := dialSim(t, srv)
	req, err := cj.ReadRequest(srv.Type(), mustAddr(t, srv.Address()), cj.DIReadData, 0x01)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.IsError() {
		t.Fatalf("exception frame (ctrl %02X)", fr.Ctrl)
	}
	f, err := cj.LookupField(fr.Type, cj.DIReadData, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := fr.Data[3:]
	v, err := cj.DecodeField(f, payload[f.Offset:f.End()])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestSimReadData(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if srv.Address() != "00000805000001" || srv.Type() != 0x10 {
		t.Fatalf("defaults: addr=%s type=%02X", srv.Address(), srv.Type())
	}

	c := dialSim(t, srv)
	req, err := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), cj.DIReadData, 0x07)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.Ctrl != cj.CtrlRead|cj.BitReply {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
	// 数据域 = DI(2) + SER(1) + 19 字节，水表 901F 必须写满宽度。
	if len(fr.Data) != 3+19 {
		t.Fatalf("data len = %d, want 22", len(fr.Data))
	}
	if fr.Data[2] != 0x07 {
		t.Fatalf("SER = %d, want 7", fr.Data[2])
	}
	payload := fr.Data[3:]
	cur, _ := cj.LookupField(0x10, cj.DIReadData, 0)
	if v, err := cj.DecodeField(cur, payload[cur.Offset:cur.End()]); err != nil || v != 1234.56 {
		t.Fatalf("current = %v, %v", v, err)
	}
	tm, _ := cj.LookupField(0x10, cj.DIReadData, 10)
	if v, err := cj.DecodeField(tm, payload[tm.Offset:tm.End()]); err != nil || v != 20261107120000 {
		t.Fatalf("clock = %v, %v", v, err)
	}
	st, _ := cj.LookupField(0x10, cj.DIReadData, 17)
	if v, err := cj.DecodeField(st, payload[st.Offset:st.End()]); err != nil || v != 0 {
		t.Fatalf("status = %v, %v", v, err)
	}
}

func TestSimIgnoresOtherAddresses(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	// 请求另一块表：RS-485 上必须保持静默，不能替别人应答。
	req, err := cj.ReadRequest(0x10, mustAddr(t, "00000805000099"), cj.DIReadData, 0x01)
	if err != nil {
		t.Fatal(err)
	}
	if fr, ok := roundTrip(t, c, req, 200*time.Millisecond); ok {
		t.Fatalf("slave answered for another address: ctrl=%02X addr=%s", fr.Ctrl, fr.Addr)
	}
	if srv.Requests() == 0 {
		t.Fatal("server did not see the request")
	}
}

func TestSimReadAddress(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithAddress("00000805000042"), WithType(0x20))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	req, err := cj.ReadAddrRequest(0x03)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.Ctrl != cj.CtrlReadAddr|cj.BitReply {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
	// 应答地址必须是真实地址，不能回显请求里的全 AA。
	if fr.Addr != mustAddr(t, "00000805000042") {
		t.Fatalf("addr = %s", fr.Addr)
	}
	if fr.Type != 0x20 {
		t.Fatalf("type = %02X", fr.Type)
	}
}

func TestSimValveWrite(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	req, err := cj.ValveRequest(0x10, mustAddr(t, srv.Address()), 0x04, false) // 关阀
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.Ctrl != cj.CtrlWrite|cj.BitReply {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
	if got := mustDI(t, "A017"); !bytes.Equal(fr.Data[:2], []byte{0xA0, 0x17}) || fr.Data[2] != 0x04 {
		t.Fatalf("echo = %X (DI %s)", fr.Data, got)
	}
	if len(fr.Data) != 5 || fr.Data[3] != 1 {
		t.Fatalf("status = % X, want 01 00", fr.Data[3:])
	}
	if v, _ := srv.Valve(); v != 1 {
		t.Fatalf("valve = %d, want closed (1)", v)
	}
	if srv.Writes() != 1 {
		t.Fatalf("writes = %d", srv.Writes())
	}
	// 状态字必须跟着阀门走，否则自检会看到「写成功但读到的还是旧值」。
	c2 := dialSim(t, srv)
	rreq, _ := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), cj.DIReadData, 0x01)
	rfr, ok := roundTrip(t, c2, rreq, 500*time.Millisecond)
	if !ok {
		t.Fatal("no read reply")
	}
	st, _ := cj.LookupField(0x10, cj.DIReadData, 17)
	payload := rfr.Data[3:]
	if v, err := cj.DecodeField(st, payload[st.Offset:st.End()]); err != nil || v != 1 {
		t.Fatalf("status = %v, %v", v, err)
	}
}

func TestSimValveRejectsUnknownPayload(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	req, err := cj.WriteRequest(0x10, mustAddr(t, srv.Address()), cj.DIValve, 0x05, []byte{0x00})
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() || fr.Ctrl != cj.CtrlWrite|cj.BitReply|cj.BitError {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
	if v, _ := srv.Valve(); v != 0 {
		t.Fatalf("valve changed to %d", v)
	}
}

func TestSimWriteAddress(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	next := mustAddr(t, "00000805000042")
	req, err := cj.WriteAddrRequest(0x10, mustAddr(t, srv.Address()), 0x09, next)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.Ctrl != cj.CtrlWriteAddr|cj.BitReply {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
	if srv.Address() != "00000805000042" {
		t.Fatalf("address = %s", srv.Address())
	}
	// 新地址可读；旧地址必须静默。
	if v := readCurrent(t, srv); v != 1234.56 {
		t.Fatalf("current = %v", v)
	}
	old, err := cj.ReadRequest(0x10, mustAddr(t, "00000805000001"), cj.DIReadData, 0x01)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := roundTrip(t, c, old, 200*time.Millisecond); ok {
		t.Fatal("slave still answers on the old address")
	}
}

func TestSimRejectsWildcardWriteAddress(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	req, err := cj.WriteAddrRequest(0x10, mustAddr(t, srv.Address()), 0x01, cj.BroadcastAddr)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() {
		t.Fatalf("ctrl = %02X, want exception", fr.Ctrl)
	}
	if srv.Address() != "00000805000001" {
		t.Fatalf("address changed to %s", srv.Address())
	}
}

func TestSimUnknownCommandReturnsException(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	// 写机电同步（0x16）未实现：必须明确回异常，而不是让主站等到超时。
	req, err := cj.Build(0x10, mustAddr(t, srv.Address()), cj.CtrlSync, []byte{0x01, 0x02, 0x03})
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() || fr.Ctrl != cj.CtrlSync|cj.BitReply|cj.BitError {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}
}

// TestSimWithoutValueReturnsException 未实现的数据标识必须回异常应答，且只
// 影响那一个 DI：主站据此把它判成点级错误，而不是整台表都不可用。
func TestSimWithoutValueReturnsException(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithoutValue("D120"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	di := mustDI(t, "D120")
	req, err := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), di, 0x02)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() || fr.Ctrl != cj.CtrlRead|cj.BitReply|cj.BitError {
		t.Fatalf("ctrl = %02X, want exception", fr.Ctrl)
	}
	// 2004 标准：异常应答数据域 = SER + 状态 ST(2)，SER 回显请求。
	if len(fr.Data) != 3 || fr.Data[0] != 0x02 || fr.Data[1] != 0x00 || fr.Data[2] != 0x00 {
		t.Fatalf("exception data = %X, want SER(02) ST(00 00)", fr.Data)
	}

	// 同一块表的其它 DI 仍然正常：异常是点级事实，不是表级故障。
	ok2, _ := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), mustDI(t, "D121"), 0x03)
	fr2, ok := roundTrip(t, c, ok2, 500*time.Millisecond)
	if !ok || fr2.IsError() {
		t.Fatalf("D121 should still answer normally: ok=%v ctrl=%02X", ok, fr2.Ctrl)
	}
}

// TestSimDropAfterClosesConnection 掉线开关必须在第 n 次应答之后才断开，
// 这样驱动侧的用例才能精确构造「读到一半链路没了」。
func TestSimDropAfterClosesConnection(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithDropAfter(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	first, _ := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), cj.DIReadData, 0x01)
	if _, ok := roundTrip(t, c, first, 500*time.Millisecond); !ok {
		t.Fatal("first transaction got no reply")
	}
	// 第一次应答已经发出，连接随后被从站关闭：第二次事务必须失败。
	second, _ := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), cj.DIReadData, 0x02)
	if _, err := c.Write(second); err != nil {
		return // 写直接失败也算断开
	}
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 64)
	for {
		if _, err := c.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("slave kept the connection open after dropAfter=1")
			}
			return
		}
	}
}

func TestSimHistoricalAndHeatSchema(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithType(0x20))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := dialSim(t, srv)

	di := mustDI(t, "D12B")
	req, err := cj.ReadRequest(0x20, mustAddr(t, srv.Address()), di, 0x02)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	if len(fr.Data) != 3+5 {
		t.Fatalf("data len = %d, want 8", len(fr.Data))
	}
	f, _ := cj.LookupField(0x20, di, 0)
	payload := fr.Data[3:]
	v, err := cj.DecodeField(f, payload[f.Offset:f.End()])
	if err != nil {
		t.Fatal(err)
	}
	// seed 里第 n 个月是 1200 - n*10。
	if math.Abs(v-1080) > 1e-9 {
		t.Fatalf("D12B = %v, want 1080", v)
	}

	// 热量表 901F 是 43 字节，不是水表的 19。
	hreq, _ := cj.ReadRequest(0x20, mustAddr(t, srv.Address()), cj.DIReadData, 0x03)
	hfr, ok := roundTrip(t, c, hreq, 500*time.Millisecond)
	if !ok {
		t.Fatal("no heat reply")
	}
	if len(hfr.Data) != 3+43 {
		t.Fatalf("heat data len = %d, want 46", len(hfr.Data))
	}
	supply, _ := cj.LookupField(0x20, cj.DIReadData, 25)
	hp := hfr.Data[3:]
	if v, err := cj.DecodeField(supply, hp[supply.Offset:supply.End()]); err != nil || v != 55 {
		t.Fatalf("supply = %v, %v", v, err)
	}
}

func TestSimSetClockRefreshesTime(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	want := time.Date(2027, 1, 2, 3, 4, 5, 0, time.Local)
	srv.SetClock(want)
	if !srv.Clock().Equal(want) {
		t.Fatalf("clock = %v", srv.Clock())
	}

	c := dialSim(t, srv)
	req, _ := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), cj.DIReadData, 0x01)
	fr, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("no reply")
	}
	tm, _ := cj.LookupField(0x10, cj.DIReadData, 10)
	payload := fr.Data[3:]
	v, err := cj.DecodeField(tm, payload[tm.Offset:tm.End()])
	if err != nil || v != 20270102030405 {
		t.Fatalf("clock = %v, %v", v, err)
	}
}

func TestSimSetValue(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	// 用 codec 的编码器造一个「当前累积流量 = 42.00 m³」的整段数据域。
	raw, err := cj.EncodeScaled(42, 4, 2, cj.QtyVolume, 0x2C)
	if err != nil {
		t.Fatal(err)
	}
	full := make([]byte, 19) // 结算日量、时间、状态用零字节占位
	copy(full, raw)
	if err := srv.SetValue("901F", full); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(srv.Value("901F")[:5], raw) {
		t.Fatalf("value = %X", srv.Value("901F"))
	}
	if v := readCurrent(t, srv); v != 42 {
		t.Fatalf("current = %v", v)
	}
	if srv.Value("90GG") != nil {
		t.Fatal("invalid DI must yield nil")
	}
}

// pipeStream 把 net.Pipe 适配成 transport.Stream（net.Conn 的 Write 返回
// (int, error)，而 transport.Stream 只返回 error；也没有 ReadFull）。
type pipeStream struct{ net.Conn }

func (p pipeStream) Write(b []byte) error {
	_, err := p.Conn.Write(b)
	return err
}

func (p pipeStream) ReadFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.Conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func TestSimServeStream(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan error, 1)
	go func() { done <- srv.ServeStream(pipeStream{server}) }()

	req, _ := cj.ReadRequest(0x10, mustAddr(t, srv.Address()), cj.DIReadData, 0x01)
	if _, err := client.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 512)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fr, _, err := cj.Parse(buf[:n])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fr.Ctrl != cj.CtrlRead|cj.BitReply {
		t.Fatalf("ctrl = %02X", fr.Ctrl)
	}

	// 关掉客户端：ServeStream 应以 nil 退出（对端断开不是错误）。
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeStream = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStream did not return after peer close")
	}
}

func TestSimCloseIdempotent(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr()
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepts connections")
	}
}

func TestNewServerBadAddress(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithAddress("123"))
	if err != nil {
		t.Fatalf("bad WithAddress must be ignored, got %v", err)
	}
	defer func() { _ = srv.Close() }()
	if srv.Address() != "00000805000001" {
		t.Fatalf("address = %s", srv.Address())
	}
}
