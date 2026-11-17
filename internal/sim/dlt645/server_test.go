package dlt645

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	dlt "uic/internal/codec/dlt645"
)

// 从站的价值在于「能让主站对着一个行为正确的表跑起来」。因此这里的用例
// 全部通过真实字节流（loopback TCP）驱动，而不是直接调 handle()：
// 帧的组/解、地址匹配、异常应答的形状都必须和真表一致。

// dialSim 连上从站并返回一个客户端。
func dialSim(t *testing.T, srv *Server) (net.Conn, func()) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c, func() { _ = c.Close() }
}

// roundTrip 发一帧并读回一帧。broadcast 命令不应答，调用方自己判断。
func roundTrip(t *testing.T, c net.Conn, req []byte) (dlt.Frame, bool) {
	t.Helper()
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := c.Read(tmp)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return dlt.Frame{}, false
			}
			t.Fatalf("read: %v", err)
		}
		buf = append(buf, tmp[:n]...)
		fr, consumed, perr := dlt.Parse(buf)
		if perr == nil {
			_ = consumed
			return fr, true
		}
		if !errors.Is(perr, dlt.ErrIncomplete) && consumed > 0 {
			buf = buf[consumed:]
		}
	}
}

func simAddr(t *testing.T, s string) [dlt.AddrLen]byte {
	t.Helper()
	a, err := dlt.ParseAddr(s)
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	return a
}

func TestSimReadReturnsStandardWidth(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()

	req, err := dlt.ReadRequest(simAddr(t, "000000000042"), mustDI(t, "02010100"))
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.IsError() {
		t.Fatalf("exception: %v", fr.Exception())
	}
	if fr.Ctrl != dlt.CtrlRead|dlt.BitReply {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	// 电压固定 2 字节：数值小也必须写满宽度，否则主站按长度不符拒绝。
	if len(fr.Data) != 4+2 {
		t.Fatalf("data len = %d, want 6", len(fr.Data))
	}
	if !bytes.Equal(fr.Data[4:], []byte{0x00, 0x22}) {
		t.Fatalf("voltage bytes = % X, want 00 22", fr.Data[4:])
	}
}

func TestSimIgnoresOtherAddresses(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()

	// 请求另一块表：RS-485 上必须保持静默，不能替别人应答。
	req, err := dlt.ReadRequest(simAddr(t, "000000000099"), mustDI(t, "00000000"))
	if err != nil {
		t.Fatal(err)
	}
	if fr, ok := roundTrip(t, c, req); ok {
		t.Fatalf("slave answered for another address: ctrl=%#x addr=%s", fr.Ctrl, fr.AddrString())
	}
}

func TestSimUnknownDataReturnsNoDataBit(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()

	req, err := dlt.ReadRequest(simAddr(t, "000000000042"), mustDI(t, "0400040A"))
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() {
		t.Fatal("unknown DI must be an exception reply")
	}
	ex := fr.Exception()
	if !ex.Has(dlt.ErrNoData) {
		t.Fatalf("exception = %v, want 无请求数据位", ex)
	}
}

func TestSimWriteHonoursWritabilityAndPassword(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithPassword([3]byte{0x11, 0x22, 0x33}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()
	addr := simAddr(t, "000000000042")
	di := mustDI(t, "04000101")
	payload := []byte{0x56, 0x34, 0x12, 0x07, 0x11, 0x26}

	// 密码错：必须拒绝，且不落盘。
	bad, err := dlt.WriteRequest(addr, di, 0x02, [3]byte{0, 0, 0}, [4]byte{}, payload)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, bad)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() || !fr.Exception().Has(dlt.ErrPassword) {
		t.Fatalf("wrong password reply = %+v", fr)
	}
	if got := srv.Value("04000101"); bytes.Equal(got, payload) {
		t.Fatal("value written despite wrong password")
	}

	// 密码对：成功且可回读。
	good, err := dlt.WriteRequest(addr, di, 0x02, [3]byte{0x11, 0x22, 0x33}, [4]byte{}, payload)
	if err != nil {
		t.Fatal(err)
	}
	fr, ok = roundTrip(t, c, good)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.IsError() {
		t.Fatalf("write rejected: %v", fr.Exception())
	}
	if fr.Ctrl != dlt.CtrlWrite|dlt.BitReply {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	if got := srv.Value("04000101"); !bytes.Equal(got, payload) {
		t.Fatalf("value = % X, want % X", got, payload)
	}
	if srv.Writes() != 1 {
		t.Fatalf("writes = %d", srv.Writes())
	}

	// 只读数据项（电压）：拒绝，且计数不增加。
	ro, err := dlt.WriteRequest(addr, mustDI(t, "02010100"), 0x02, [3]byte{0x11, 0x22, 0x33},
		[4]byte{}, []byte{0x00, 0x22})
	if err != nil {
		t.Fatal(err)
	}
	fr, ok = roundTrip(t, c, ro)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() {
		t.Fatal("read-only DI accepted a write")
	}
	if srv.Writes() != 1 {
		t.Fatalf("writes = %d after read-only write attempt", srv.Writes())
	}
}

func TestSimFollowFramesSplit(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()
	addr := simAddr(t, "000000000042")

	raw := make([]byte, 0, 500)
	for i := 0; i < 500; i++ {
		raw = append(raw, byte(i%256))
	}
	if err := srv.SetValue("0201FF00", raw); err != nil {
		t.Fatal(err)
	}

	req, err := dlt.ReadRequest(addr, mustDI(t, "0201FF00"))
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.HasFollow() {
		t.Fatal("500-byte block must be split")
	}
	// 首帧数据域 = 4 字节 DI + 分段数据；整帧 L 不得超过 200。
	if len(fr.Data) > dlt.MaxData {
		t.Fatalf("first frame data = %d bytes, exceeds %d", len(fr.Data), dlt.MaxData)
	}
	got := append([]byte(nil), fr.Data[4:]...)

	seq := byte(1)
	for fr.HasFollow() {
		if seq > 10 {
			t.Fatal("too many follow frames")
		}
		nreq, err := dlt.ReadNextRequest(addr, mustDI(t, "0201FF00"), seq)
		if err != nil {
			t.Fatal(err)
		}
		nfr, ok := roundTrip(t, c, nreq)
		if !ok {
			t.Fatalf("no reply for seq %d", seq)
		}
		if nfr.IsError() {
			t.Fatalf("seq %d exception: %v", seq, nfr.Exception())
		}
		// 读后续应答：DI(4) + 数据 + SEQ(1)。
		if len(nfr.Data) < 6 {
			t.Fatalf("seq %d data too short: %d", seq, len(nfr.Data))
		}
		if last := nfr.Data[len(nfr.Data)-1]; last != seq {
			t.Fatalf("seq echo = %d, want %d", last, seq)
		}
		chunk := nfr.Data[4 : len(nfr.Data)-1]
		if len(nfr.Data) > dlt.MaxData {
			t.Fatalf("seq %d frame data = %d, exceeds %d", seq, len(nfr.Data), dlt.MaxData)
		}
		got = append(got, chunk...)
		fr = nfr
		seq++
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("reassembled %d bytes, want %d (content mismatch: %v)", len(got), len(raw), bytes.Equal(got, raw))
	}
}

func TestSimReadAddress(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()

	req, err := dlt.ReadAddrRequest()
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.Ctrl != dlt.CtrlReadAddr|dlt.BitReply {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	if got := fr.AddrString(); got != "000000000042" {
		t.Fatalf("addr = %s", got)
	}
	if !bytes.Equal(fr.Data, []byte{0x42, 0, 0, 0, 0, 0}) {
		t.Fatalf("addr data = % X", fr.Data)
	}
}

func TestSimBroadcastTimeIsSilentButApplied(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()

	want := time.Date(2027, 3, 4, 5, 6, 7, 0, time.Local)
	req, err := dlt.BroadcastTimeRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := roundTrip(t, c, req); ok {
		t.Fatal("broadcast must not be answered")
	}
	// 校时应已生效：读时钟数据项验证。
	rreq, err := dlt.ReadRequest(simAddr(t, "000000000042"), mustDI(t, "04000101"))
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, rreq)
	if !ok {
		t.Fatal("no reply")
	}
	got, err := dlt.ParseTime(fr.Data[4:])
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("clock = %v, want %v", got, want)
	}
}

func TestSimRelayCommand(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()
	addr := simAddr(t, "000000000042")

	req, err := dlt.RelayRequest(addr, 0x02, [3]byte{}, [4]byte{},
		dlt.RelayTrip, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req)
	if !ok {
		t.Fatal("no reply")
	}
	if fr.IsError() {
		t.Fatalf("relay rejected: %v", fr.Exception())
	}
	if fr.Ctrl != dlt.CtrlRelay|dlt.BitReply {
		t.Fatalf("ctrl = %#x", fr.Ctrl)
	}
	if action, at := srv.Relay(); action != dlt.RelayTrip || at.IsZero() {
		t.Fatalf("relay = %#x at %v", action, at)
	}

	// 非法控制类型必须被拒绝，而不是被当成「最近一次命令」记下来。
	bad := append([]byte(nil), req...)
	// 数据域在帧内偏移 4(前导)+10(帧头)；控制类型在数据域偏移 8。
	bad[4+10+8] = 0x7F
	bad[len(bad)-2] = dlt.Sum(append([]byte(nil), bad[4:len(bad)-2]...))
	if fr, ok := roundTrip(t, c, bad); ok && !fr.IsError() {
		t.Fatal("illegal relay action accepted")
	}
	if action, _ := srv.Relay(); action != dlt.RelayTrip {
		t.Fatalf("relay overwritten by illegal command: %#x", action)
	}
}

// TestSimWithoutValueReturnsException 未实现的数据标识必须回异常应答，且只
// 影响那一个 DI：主站据此把它判成点级错误，而不是整台表都不可用。
func TestSimWithoutValueReturnsException(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithoutValue("02010100"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c, done := dialSim(t, srv)
	defer done()

	addr := simAddr(t, "000000000042")
	req, err := dlt.ReadRequest(addr, mustDI(t, "02010100"))
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := roundTrip(t, c, req)
	if !ok {
		t.Fatal("no reply")
	}
	if !fr.IsError() {
		t.Fatalf("ctrl = %02X, want exception", fr.Ctrl)
	}
	if !fr.Exception().Has(dlt.ErrNoData) {
		t.Fatalf("exception = %02X, want 无请求数据位", fr.Exception().Code)
	}

	// 同一块表的其它数据项仍然正常：异常是点级事实，不是表级故障。
	ok2, _ := dlt.ReadRequest(addr, mustDI(t, "02020100"))
	fr2, ok := roundTrip(t, c, ok2)
	if !ok || fr2.IsError() {
		t.Fatalf("C 相电压应正常应答: ok=%v ctrl=%02X", ok, fr2.Ctrl)
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
	c, done := dialSim(t, srv)
	defer done()

	addr := simAddr(t, "000000000042")
	first, _ := dlt.ReadRequest(addr, mustDI(t, "00000000"))
	if _, ok := roundTrip(t, c, first); !ok {
		t.Fatal("first transaction got no reply")
	}
	// 第一次应答已经发出，连接随后被从站关闭：第二次事务必须失败。
	second, _ := dlt.ReadRequest(addr, mustDI(t, "02010100"))
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

func TestSimCloseIsIdempotent(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestSimServeStreamOverNetPipe(t *testing.T) {
	// ServeStream 是串口路径的入口：用 net.Pipe 代替 pty，验证它不依赖
	// net.Listener 也能跑通一读一写。
	srv := newServer()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() { _ = srv.ServeStream(&tcpStream{conn: server}) }()

	req, err := dlt.ReadRequest(simAddr(t, "000000000042"), mustDI(t, "02010100"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan dlt.Frame, 1)
	go func() {
		fr, _ := roundTripPipe(client, req)
		done <- fr
	}()
	select {
	case fr := <-done:
		if fr.Ctrl == 0 {
			t.Fatal("no reply over pipe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout over pipe")
	}
}

func roundTripPipe(c net.Conn, req []byte) (dlt.Frame, error) {
	if _, err := c.Write(req); err != nil {
		return dlt.Frame{}, err
	}
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := c.Read(tmp)
		if err != nil {
			return dlt.Frame{}, err
		}
		buf = append(buf, tmp[:n]...)
		fr, _, perr := dlt.Parse(buf)
		if perr == nil {
			return fr, nil
		}
		if !errors.Is(perr, dlt.ErrIncomplete) {
			return dlt.Frame{}, perr
		}
	}
}

var _ = io.EOF

func mustDI(t *testing.T, s string) dlt.DI {
	t.Helper()
	di, err := dlt.ParseDI(s)
	if err != nil {
		t.Fatalf("DI %q: %v", s, err)
	}
	return di
}
