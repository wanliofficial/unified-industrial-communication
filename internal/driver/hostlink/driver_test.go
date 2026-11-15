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

// replyWithEndCode 构造一条 FINS 结束码非 0 的合法应答（FCS 正确、单元号与
// SID 与请求对上），只是把结束码改成 code。
//
// 走 BuildResponse 而不是手工改字节：FCS 覆盖整帧正文，手改结束码会先被
// FCS 校验拦下，测到的是「校验和错」而不是「结束码分类」。
func replyWithEndCode(code uint16) []byte {
	resp := hlcodec.Response{
		Unit: 0, EndCode: 0, ICF: 0x40, MRC: 1, SRC: 1,
		FinsEndCode: code, Data: nil,
	}
	resp.SID = 1
	b, err := hlcodec.BuildResponse(resp)
	if err != nil {
		panic(err)
	}
	return b
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

// 结束码按语义分类，不能一律当协议错且可重试。
//
// 早先 transactor.RoundTrip 把所有非零 FINS 结束码都包成
// driver.E(driver.ErrProtocol, …, temporary=true)，后果分两层：
//
//   - 地址类（0x1101..0x1104）被当成「可重试」。调度器会为一个填错地址的
//     点重发，结果完全相同；而重试次数还会把这一轮的采集时延拖长。
//   - 更严重的是 CountsTowardBreaker：ErrProtocol 计入熔断，于是**一个写错
//     地址的点数会把整台 PLC 标成坏设备**。设备本身是好的。
//
// 现在按同一张结束码表分类（与 FINS/TCP 共用 finscodec.EndCodeError）：
//
//   - 地址类 → ErrAddress（不计入熔断、不重试）；
//   - 链路类（节点不在网 / 令牌超时 / 路由表错）→ ErrProtocol + 可重试；
//   - 其余 → ErrProtocol，不重试。
func TestRoundTripEndCodeIsClassified(t *testing.T) {
	// 结束码在应答里紧跟精简头（ICF/DA2/SA2/SID 之后），FCS 已由
	// replyWithEndCode 正确计算。
	reply := replyWithEndCode

	cases := []struct {
		code    uint16
		kind    driver.ErrorKind
		retry   bool
		breaker bool
		quality model.Quality
	}{
		// 地址越界：点表问题，不重试、不计熔断、质量 address_error。
		{0x1103, driver.ErrAddress, false, false, model.QualityAddressError},
		// 区码不认识：同样是地址类。
		{0x1101, driver.ErrAddress, false, false, model.QualityAddressError},
		// 令牌超时：链路类，重发有意义。
		{0x0202, driver.ErrProtocol, true, true, model.QualityCommError},
		// 目标单元缺失：链路类。
		{0x0302, driver.ErrProtocol, true, true, model.QualityCommError},
		// 帧长错：配置/组帧问题，修一次永久消失。
		{0x0018, driver.ErrProtocol, false, true, model.QualityBad},
		// 模式不允许：PLC 状态错，重试只会再错一次。
		{0x0001, driver.ErrProtocol, false, true, model.QualityBad},
	}
	for _, tc := range cases {
		l := &memLink{}
		l.addFrame(reply(tc.code))
		tr := newTransactor(l)
		h := tr.Header()
		frame, _ := finscodec.BuildMemoryRead(h, finscodec.WordAccess(finscodec.Area{Key: "dm", Word: 0x82}), 0, 0, 1)
		_, err := tr.RoundTrip(t.Context(), frame)
		if err == nil {
			t.Fatalf("结束码 %04X 应报错", tc.code)
		}
		if got := driver.KindOf(err); got != tc.kind {
			t.Errorf("结束码 %04X：kind = %q，要 %q（%v）", tc.code, got, tc.kind, err)
		}
		if got := driver.IsRetryable(err); got != tc.retry {
			t.Errorf("结束码 %04X：retryable = %v，要 %v（%v）", tc.code, got, tc.retry, err)
		}
		if got := driver.CountsTowardBreaker(err); got != tc.breaker {
			t.Errorf("结束码 %04X：计入熔断 = %v，要 %v（%v）", tc.code, got, tc.breaker, err)
		}
		if got := finscore.QualityOf(err); got != tc.quality {
			t.Errorf("结束码 %04X：质量 = %q，要 %q（%v）", tc.code, got, tc.quality, err)
		}
	}
}

// 结束码错误必须原样透传在 OpError.Err 里：QualityOf 靠它把地址类结束码
// 映射成 address_error（而不是只看外层 Kind 得到 bad）。若哪次重构把
// Err 丢了，这条会先红。
func TestRoundTripEndCodeKeepsInnerError(t *testing.T) {
	b := replyWithEndCode(0x1103)
	l := &memLink{}
	l.addFrame(b)
	tr := newTransactor(l)
	h := tr.Header()
	frame, _ := finscodec.BuildMemoryRead(h, finscodec.WordAccess(finscodec.Area{Key: "dm", Word: 0x82}), 0, 0, 1)
	_, err := tr.RoundTrip(t.Context(), frame)

	var ec *finscodec.EndCodeError
	if !errors.As(err, &ec) || ec.Code != 0x1103 {
		t.Fatalf("err = %v，应包含 *EndCodeError{0x1103}", err)
	}
	var oe *driver.OpError
	if !errors.As(err, &oe) || oe.Err == nil {
		t.Fatalf("err = %v，应保留内层错误", err)
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

// TestSerialDefaultsPartialKeepsGenericFallback 钉住「部分配置」这一条：
// 只要用户填了任一项（最常见的场景是只改波特率），其余三项必须保持零值，
// 由 transport.Normalize 补通用缺省 8/N/1——而不是被强行改成 Host Link 的
// 7 位数据位 + 偶校验 + 2 停止位。
//
// 为什么这条重要：一台配成 19200-8-N-1 的 PLC，如果点表只写了
// `transport.baud: 19200`，逐字段补缺省会把链路悄悄改成 19200-7-E-2，
// 结果是「帧发得出去、应答收不回来」，而且不报错——串口现场最典型也最难
// 定位的假故障。规范 §3.10.19.8 与本函数的注释都写明「只补完全没配」，
// 这条测试把该行为固定下来。
func TestSerialDefaultsPartialKeepsGenericFallback(t *testing.T) {
	// 只改波特率：其余三项保持零，交给 Normalize 补 8/N/1。
	got := serialDefaults(model.Transport{Kind: "serial", Port: "/dev/ttyUSB0", Baud: 19200})
	if got.Baud != 19200 {
		t.Fatalf("explicit baud overridden: %+v", got)
	}
	if got.DataBits != 0 || got.Parity != "" || got.StopBits != 0 {
		t.Fatalf("partial config must keep the untouched fields empty (so Normalize applies 8/N/1), got %+v", got)
	}

	// 只填校验：同样不应触发 Host Link 出厂值。
	got = serialDefaults(model.Transport{Kind: "serial", Port: "/dev/ttyUSB0", Parity: "odd"})
	if got.Parity != "odd" || got.Baud != 0 || got.DataBits != 0 || got.StopBits != 0 {
		t.Fatalf("partial config with parity only was coerced: %+v", got)
	}

	// 反向：完全没配时仍必须补 9600-7-E-2（否则就丢了出厂缺省这条事实）。
	got = serialDefaults(model.Transport{Kind: "serial", Port: "/dev/ttyUSB0"})
	if got.Baud != 9600 || got.DataBits != 7 || got.Parity != "even" || got.StopBits != 2 {
		t.Fatalf("empty config must fill the Host Link factory defaults 9600-7-E-2, got %+v", got)
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
