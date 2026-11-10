package hostlink

import (
	"context"
	"errors"
	"testing"
	"time"

	finscodec "uic/internal/codec/fins"
	hlcodec "uic/internal/codec/hostlink"
	"uic/internal/driver"
	"uic/internal/driver/finscore"
	"uic/internal/model"
	"uic/internal/transport"
)

// memLink 是一条内存链路：把预置的应答按帧喂给收帧器，并记录发出去的字节。
//
// 用它而不是真 TCP，是为了精确构造「不该发生但现场会发生」的帧：FCS 错的、
// 单元号错的、SID 错位的、结束码非 0 的。这些路径在回环里永远跑不到。
type memLink struct {
	responses [][]byte
	pos       int
	wrote     [][]byte
	closed    bool
	readErr   error
}

func (m *memLink) Write(p []byte) error {
	m.wrote = append(m.wrote, append([]byte(nil), p...))
	return nil
}

func (m *memLink) ReadFull(n int) ([]byte, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	if m.pos >= len(m.responses) {
		return nil, errors.New("eof")
	}
	f := m.responses[m.pos]
	m.pos++
	if len(f) < n {
		return nil, errors.New("short")
	}
	return f[:n], nil
}

func (m *memLink) Close() error                  { m.closed = true; return nil }
func (m *memLink) SetDeadline(t time.Time) error { return nil }
func (m *memLink) Drain() error                  { return nil }
func (m *memLink) Lock()                         {}
func (m *memLink) Unlock()                       {}

// addResponse 按字节分次交付，模拟串口逐字节到达。
func (m *memLink) addFrame(f []byte) {
	for _, b := range f {
		m.responses = append(m.responses, []byte{b})
	}
}

func newTransactor(l *memLink) *transactor {
	return &transactor{link: l, unit: 0, gap: 0, devID: "plc", timeout: time.Second}
}

// readFrame 构造一条 DM 读 1 字的请求并走一次事务。
func readFrame(t *testing.T, tr *transactor) (finscodec.Frame, error) {
	t.Helper()
	h := tr.Header()
	frame, err := finscodec.BuildMemoryRead(h, finscodec.WordAccess(finscodec.Area{Key: "dm", Word: 0x82}), 0, 0, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tr.RoundTrip(context.Background(), frame)
}

// goodReply 生成一条「DM0 = v」的合法应答（FCS 正确）。
func goodReply(sid byte, v uint16) []byte {
	resp := hlcodec.Response{
		Unit: 0, EndCode: 0, ICF: 0x40, MRC: 1, SRC: 1, FinsEndCode: 0,
		Data: []byte{byte(v >> 8), byte(v)},
	}
	resp.SID = sid
	b, err := hlcodec.BuildResponse(resp)
	if err != nil {
		panic(err)
	}
	return b
}

func TestRoundTripSendsHostLinkFrame(t *testing.T) {
	l := &memLink{}
	l.addFrame(goodReply(1, 0xBEEF))
	tr := newTransactor(l)

	resp, err := readFrame(t, tr)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(resp.Data) != 2 || resp.Data[0] != 0xBE || resp.Data[1] != 0xEF {
		t.Fatalf("data = % x", resp.Data)
	}
	if len(l.wrote) != 1 {
		t.Fatalf("wrote %d frames", len(l.wrote))
	}
	// 发出去的必须是 Host Link 帧：`@` 开头、`*`+CR 结尾、FCS 正确。
	sent := l.wrote[0]
	req, err := hlcodec.ParseRequest(sent)
	if err != nil {
		t.Fatalf("the driver did not send a parseable Host Link frame: %v (%q)", err, sent)
	}
	if req.Unit != 0 {
		t.Fatalf("unit = %d", req.Unit)
	}
	// 直连模式 ICF 必须是 00，不是 FINS/TCP 惯用的 80。
	if req.ICF != 0x00 {
		t.Fatalf("ICF = 0x%02X, want 0x00 for a direct Host Link connection", req.ICF)
	}
	if req.Wait != 0 {
		t.Fatalf("wait = %d", req.Wait)
	}
	if req.MRC != 1 || req.SRC != 1 {
		t.Fatalf("command = %02X%02X", req.MRC, req.SRC)
	}
	if len(req.Text) != 6 || req.Text[0] != 0x82 {
		t.Fatalf("text = % x", req.Text)
	}
}

func TestRoundTripAssignsAndChecksSID(t *testing.T) {
	l := &memLink{}
	l.addFrame(goodReply(7, 1)) // SID 不匹配（本层第一次分配的是 1）
	tr := newTransactor(l)
	if _, err := readFrame(t, tr); err == nil {
		t.Fatal("SID mismatch accepted")
	} else {
		var mm *hlcodec.SIDMismatch
		if !errors.As(err, &mm) {
			t.Logf("err = %v (kind %q)", err, driver.KindOf(err))
		}
		if finscore.QualityOf(err) != model.QualityCommError {
			t.Fatalf("quality = %q, want comm error", finscore.QualityOf(err))
		}
	}
}

func TestRoundTripRejectsWrongUnit(t *testing.T) {
	l := &memLink{}
	resp := hlcodec.Response{Unit: 5, EndCode: 0, ICF: 0x40, MRC: 1, SRC: 1}
	resp.SID = 1
	b, _ := hlcodec.BuildResponse(resp)
	l.addFrame(b)
	tr := newTransactor(l)
	_, err := readFrame(t, tr)
	if err == nil {
		t.Fatal("a response from another unit was accepted")
	}
	if finscore.QualityOf(err) != model.QualityCommError {
		t.Fatalf("quality = %q (%v)", finscore.QualityOf(err), err)
	}
}

func TestRoundTripSurfacesEndCodeAsFormatError(t *testing.T) {
	l := &memLink{}
	resp := hlcodec.Response{Unit: 0, EndCode: 0x14, ICF: 0x40, MRC: 1, SRC: 1}
	resp.SID = 1
	b, _ := hlcodec.BuildResponse(resp)
	l.addFrame(b)
	tr := newTransactor(l)
	_, err := readFrame(t, tr)
	if err == nil {
		t.Fatal("end code 0x14 accepted")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %q", driver.KindOf(err))
	}
	// 格式错重发没用，不该标成可重试。
	if driver.IsRetryable(err) {
		t.Fatal("a framing error must not be retryable")
	}
}

func TestRoundTripSurfacesWriteProtectAsPermission(t *testing.T) {
	l := &memLink{}
	resp := hlcodec.Response{Unit: 0, EndCode: 0x03, ICF: 0x40, MRC: 1, SRC: 2}
	resp.SID = 1
	b, _ := hlcodec.BuildResponse(resp)
	l.addFrame(b)
	tr := newTransactor(l)
	_, err := readFrame(t, tr)
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
}

func TestRoundTripSurfacesFinsEndCode(t *testing.T) {
	l := &memLink{}
	resp := hlcodec.Response{Unit: 0, EndCode: 0, ICF: 0x40, MRC: 1, SRC: 1, FinsEndCode: 0x1103}
	resp.SID = 1
	b, _ := hlcodec.BuildResponse(resp)
	l.addFrame(b)
	tr := newTransactor(l)
	_, err := readFrame(t, tr)
	if err == nil {
		t.Fatal("FINS end code 0x1103 accepted")
	}
	if finscore.QualityOf(err) != model.QualityAddressError {
		t.Fatalf("quality = %q (%v)", finscore.QualityOf(err), err)
	}
}

func TestRoundTripRejectsCommandEchoMismatch(t *testing.T) {
	l := &memLink{}
	resp := hlcodec.Response{Unit: 0, EndCode: 0, ICF: 0x40, MRC: 1, SRC: 2} // 回的是写命令
	resp.SID = 1
	b, _ := hlcodec.BuildResponse(resp)
	l.addFrame(b)
	tr := newTransactor(l)
	if _, err := readFrame(t, tr); err == nil {
		t.Fatal("a response echoing another command was accepted")
	}
}

func TestRoundTripRejectsBadFCS(t *testing.T) {
	l := &memLink{}
	b := goodReply(1, 1)
	// 把 FCS 的最后一个字符改掉。
	b[len(b)-3] ^= 0x01
	l.addFrame(b)
	tr := newTransactor(l)
	_, err := readFrame(t, tr)
	if err == nil {
		t.Fatal("bad FCS accepted")
	}
	if finscore.QualityOf(err) != model.QualityCommError {
		t.Fatalf("quality = %q (%v)", finscore.QualityOf(err), err)
	}
}

func TestRoundTripTimeoutIsRetryable(t *testing.T) {
	l := &memLink{readErr: timeoutErr{}}
	tr := newTransactor(l)
	_, err := readFrame(t, tr)
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("timeout must be retryable")
	}
	if finscore.QualityOf(err) != model.QualityTimeout {
		t.Fatalf("quality = %q", finscore.QualityOf(err))
	}
}

func TestRoundTripContextCancelled(t *testing.T) {
	l := &memLink{readErr: timeoutErr{}}
	tr := newTransactor(l)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := tr.Header()
	frame, _ := finscodec.BuildMemoryRead(h, finscodec.WordAccess(finscodec.Area{Key: "dm", Word: 0x82}), 0, 0, 1)
	_, err := tr.RoundTrip(ctx, frame)
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
}

func TestRoundTripAfterClose(t *testing.T) {
	l := &memLink{}
	tr := newTransactor(l)
	_ = tr.Close()
	_, err := readFrame(t, tr)
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("closed connection must be retryable")
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestDefaultSerialParams(t *testing.T) {
	got := serialDefaults(model.Transport{Kind: "serial", Port: "/dev/ttyUSB0"})
	if got.Baud != 9600 || got.DataBits != 7 || got.Parity != "even" || got.StopBits != 2 {
		t.Fatalf("defaults = %+v", got)
	}
	got = serialDefaults(model.Transport{Kind: "serial", Port: "/dev/ttyUSB0",
		Baud: 19200, DataBits: 8, Parity: "none", StopBits: 1})
	if got.Baud != 19200 || got.DataBits != 8 || got.Parity != "none" || got.StopBits != 1 {
		t.Fatalf("explicit params overridden: %+v", got)
	}
	if got := serialDefaults(model.Transport{Kind: "tcp", Address: "x"}); got.Baud != 0 {
		t.Fatalf("tcp transport was touched: %+v", got)
	}
}

func TestUnitOf(t *testing.T) {
	for _, n := range []int{-1, 32, 99} {
		_, err := unitOf(model.Device{ID: "d", UnitID: n})
		if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
			t.Fatalf("unit %d: err = %v", n, err)
		}
	}
	for _, n := range []int{0, 1, 31} {
		got, err := unitOf(model.Device{ID: "d", UnitID: n})
		if err != nil || int(got) != n {
			t.Fatalf("unit %d: got %d err %v", n, got, err)
		}
	}
}

func TestParseOptions(t *testing.T) {
	opt := parseOptions(model.Device{Labels: map[string]string{"options": "gap=20ms,wait=5"}})
	if opt.Gap != 20*time.Millisecond || opt.Wait != 5 {
		t.Fatalf("opt = %+v", opt)
	}
	// 非法的值必须静默忽略并保留缺省：这是可选项，不该让设备起不来。
	opt = parseOptions(model.Device{Labels: map[string]string{"options": "gap=99s,wait=99,junk"}})
	if opt.Gap != defaultGap || opt.Wait != defaultWait {
		t.Fatalf("opt = %+v", opt)
	}
	opt = parseOptions(model.Device{})
	if opt.Gap != defaultGap || opt.Wait != defaultWait {
		t.Fatalf("opt = %+v", opt)
	}
}

func TestHostLinkLimitsAreFrameDerived(t *testing.T) {
	l := hostLinkLimits()
	if l.WordsPerRead != hlcodec.MaxReadWords || l.WordsPerWrite != hlcodec.MaxWriteWords {
		t.Fatalf("limits = %+v", l)
	}
	// 写上限必须小于读上限：写要带上数据，命令帧比应答帧能装的更少。
	if l.WordsPerWrite >= l.WordsPerRead {
		t.Fatalf("write limit %d should be smaller than read limit %d", l.WordsPerWrite, l.WordsPerRead)
	}
	if l.WordsPerRead >= 999 {
		t.Fatalf("Host Link must not reuse the FINS/TCP 999-word limit")
	}
}

func TestHeaderIsDirectConnection(t *testing.T) {
	tr := newTransactor(&memLink{})
	h := tr.Header()
	if h.ICF != 0x00 {
		t.Fatalf("ICF = 0x%02X, want 0x00", h.ICF)
	}
	if h.DA1 != 0 || h.SA1 != 0 || h.DNA != 0 || h.SNA != 0 {
		t.Fatalf("Host Link has no node addresses, got %+v", h)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ transport.Stream = (*memLink)(nil)
