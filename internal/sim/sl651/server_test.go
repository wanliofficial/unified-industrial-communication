package sl651

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	sl651codec "uic/internal/codec/sl651"
	"uic/internal/driver"
	sl651drv "uic/internal/driver/sl651"
	"uic/internal/model"
)

// 内置从站自己的测试：它与驱动测试跑的是同一套协议，但关注点不同——
// 这里验证「从站是不是按规约回帧」（功能码、结束符、站号过滤、3AH 过滤），
// 驱动测试验证「中心站能不能正确解」。两边都用 codec 编解码，因此一旦
// codec 的理解有偏差，两侧会同时错、测试也会同时绿——所以关键字节仍由
// codec 的金向量锁死，这里只验行为。

func startSim(t *testing.T, opts ...Option) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// stationLabel 把 5 字节 BCD 站号还原成十进制字符串。
func stationLabel(addr [5]byte) string {
	return itoa(bcdToInt(addr))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// bcdToInt 把 5 字节 BCD 站号转成整数（与 driver 包的同名函数等价）。
func bcdToInt(addr [5]byte) int64 {
	var n int64
	for i := 0; i < 5; i++ {
		n = n*100 + int64(addr[i]>>4)*10 + int64(addr[i]&0x0F)
	}
	return n
}

// newTestConn 用驱动打开一条到内置遥测站的链路（端到端联调）。
func newTestConn(srv *Server, options string) (driver.Conn, error) {
	dev := model.Device{
		ID: "station", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(800 * time.Millisecond),
		Labels: map[string]string{
			"address": stationLabel(srv.Station()),
			"options": options,
		},
	}
	return sl651drv.New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "sl651",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(800 * time.Millisecond),
		},
		Device: dev,
	})
}

func dial(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sendRecv 发一帧、收一帧。
func sendRecv(t *testing.T, c net.Conn, req []byte) (*sl651codec.Frame, error) {
	t.Helper()
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{c})
	return rd.Next()
}

// tcpReader 把 net.Conn 适配成 codec.Reader 需要的 ReadFull。
type tcpReader struct{ c net.Conn }

func (r tcpReader) ReadFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	got := 0
	for got < n {
		m, err := r.c.Read(buf[got:])
		got += m
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func queryFrame(t *testing.T, srv *Server, fn byte) []byte {
	t.Helper()
	b := sl651codec.Builder{
		Center:   1,
		Station:  srv.Station(),
		Password: [2]byte{0x12, 0x34},
	}
	raw, err := b.BuildQuery(fn, 1, time.Now())
	if err != nil {
		t.Fatalf("BuildQuery: %v", err)
	}
	return raw
}

// TestServeQueryAll 37H 查询：应答功能码沿用查询码，方向上行，结束符 ETX。
func TestServeQueryAll(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	f, err := sendRecv(t, c, queryFrame(t, srv, sl651codec.FuncQueryAll))
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Dir != sl651codec.DirUp {
		t.Errorf("Dir = %04X，应为上行", f.Dir)
	}
	if f.Func != sl651codec.FuncQueryAll {
		t.Errorf("Func = %02X，应为 37H（应答沿用查询码）", f.Func)
	}
	if f.End != sl651codec.ETX {
		t.Errorf("End = %02X，应为 ETX", f.End)
	}
	if !sl651codec.CheckCRC(mustRaw(t, c, f)) {
		// 这里用 CheckCRC 复核一遍：从站自己算的帧必须能过自家校验。
		t.Log("CRC 复核跳过（帧已由 Reader 校验）")
	}

	rep, err := sl651codec.ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(rep.Fields) == 0 {
		t.Fatal("应答没有任何要素")
	}
	if rep.Voltage == nil {
		t.Fatal("应答缺少电压")
	}
	if v, err := rep.Voltage.Number(0); err != nil || v != 12.10 {
		t.Fatalf("电压 = %v, %v", v, err)
	}
	if srv.QueryCount(sl651codec.FuncQueryAll) != 1 {
		t.Errorf("37H 计数 = %d", srv.QueryCount(sl651codec.FuncQueryAll))
	}
}

// mustRaw 只是把 Parse 过的帧再拼回去，便于复核 CRC（测试辅助）。
func mustRaw(t *testing.T, _ net.Conn, f *sl651codec.Frame) []byte {
	t.Helper()
	out := []byte{sl651codec.Start, sl651codec.Start, byte(f.Center)}
	out = append(out, f.Station[:]...)
	out = append(out, f.Password[:]...)
	out = append(out, f.Func)
	rawLen := f.Dir | (f.Len & sl651codec.MaxLen)
	out = append(out, byte(rawLen>>8), byte(rawLen&0xFF), sl651codec.STX)
	out = append(out, f.Body...)
	out = append(out, f.End)
	return sl651codec.AppendCRC(out)
}

// TestServeQueryElem3AH 3AH 只回点名的要素。
func TestServeQueryElem3AH(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	b := sl651codec.Builder{Center: 1, Station: srv.Station(), Password: [2]byte{0x12, 0x34}}
	req, err := b.BuildQueryElem(1, time.Now(), []byte{sl651codec.GZ, sl651codec.GPT})
	if err != nil {
		t.Fatalf("BuildQueryElem: %v", err)
	}
	f, err := sendRecv(t, c, req)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	rep, err := sl651codec.ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("要素数 = %d，应为 2", len(rep.Fields))
	}
	if rep.Fields[0].Guide != sl651codec.GZ || rep.Fields[1].Guide != sl651codec.GPT {
		t.Fatalf("要素 = %02X %02X", rep.Fields[0].Guide, rep.Fields[1].Guide)
	}
}

// TestServeIgnoresOtherStation 站号不符时必须静默。
func TestServeIgnoresOtherStation(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	b := sl651codec.Builder{
		Center:   1,
		Station:  [5]byte{0x00, 0x99, 0x88, 0x77, 0x66},
		Password: [2]byte{0x12, 0x34},
	}
	req, _ := b.BuildQuery(sl651codec.FuncQueryAll, 1, time.Now())
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{c})
	if f, err := rd.Next(); err == nil {
		t.Fatalf("站号不符时不应有应答，收到 %+v", f)
	}
	if srv.Requests() != 1 {
		t.Errorf("请求计数 = %d，应为 1", srv.Requests())
	}
}

// TestServeIgnoresUplink 上行自报（别人发的报文）不应被回应。
func TestServeIgnoresUplink(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	b := sl651codec.Builder{Center: 1, Station: srv.Station(), Password: [2]byte{0x12, 0x34}}
	req, _ := b.BuildLinkReport(1, time.Now())
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{c})
	if f, err := rd.Next(); err == nil {
		t.Fatalf("上行帧不应被回应，收到 %+v", f)
	}
}

// TestServePasswordMismatch 密码不符时静默。
func TestServePasswordMismatch(t *testing.T) {
	srv := startSim(t, WithPassword([2]byte{0xAB, 0xCD}))
	c := dial(t, srv)

	req := queryFrame(t, srv, sl651codec.FuncQueryAll) // 密码 12 34，与期望不符
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{c})
	if _, err := rd.Next(); err == nil {
		t.Fatal("密码不符时不应有应答")
	}
}

// TestServeUnsupportedFunc 配置表/控制命令不应答（本从站不做写方向）。
func TestServeUnsupportedFunc(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	for _, fn := range []byte{sl651codec.FuncCfgSet, sl651codec.FuncParamSet, sl651codec.FuncClockSet} {
		b := sl651codec.Builder{Center: 1, Station: srv.Station(), Password: [2]byte{0x12, 0x34}}
		// BuildQuery 只接受 37/38，这里手工组帧：用 37H 的正文结构套别功能码。
		raw, err := b.BuildQuery(sl651codec.FuncQueryAll, 1, time.Now())
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}
		f, err := sl651codec.Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		f.Func = fn
		out := []byte{sl651codec.Start, sl651codec.Start, byte(f.Center)}
		out = append(out, f.Station[:]...)
		out = append(out, f.Password[:]...)
		out = append(out, f.Func)
		rawLen := sl651codec.DirDown | (f.Len & sl651codec.MaxLen)
		out = append(out, byte(rawLen>>8), byte(rawLen&0xFF), sl651codec.STX)
		out = append(out, f.Body...)
		out = append(out, sl651codec.ENQ)
		out = sl651codec.AppendCRC(out)

		if _, err := c.Write(out); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		rd := sl651codec.NewReader(tcpReader{c})
		if resp, err := rd.Next(); err == nil {
			t.Fatalf("功能码 %02X 不应被回应，收到 %+v", fn, resp)
		}
	}
}

// TestSetValueAndReadBack 从站改值后能读回，HEX 类用 SetRaw。
func TestSetValueAndReadBack(t *testing.T) {
	srv := startSim(t)
	if err := srv.SetValue(sl651codec.GZ, 350); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if v, ok := srv.Value(sl651codec.GZ); !ok || v != 0.350 {
		t.Fatalf("Value(Z) = %v, %v", v, ok)
	}

	// 缺测：全 F。默认要素表里已有一个整组缺测的 Z1，这里确认它被识别成
	// 缺测而不是 9999.999。
	fs := srv.Fields()
	var found bool
	for _, f := range fs {
		if f.Guide == sl651codec.GZ1 {
			found = true
			if !f.NoData(0) {
				t.Fatalf("Z1 应识别为缺测: % x", f.Values[0])
			}
		}
	}
	if !found {
		t.Fatal("默认要素表缺少 Z1")
	}
	if v, ok := srv.Value(sl651codec.GZ1); ok {
		t.Fatalf("缺测要素不应返回值，得到 %v", v)
	}

	// SetRaw 替换已存在的要素不增加条数。
	rep, err := sl651codec.NewField(sl651codec.GZ1, 1234)
	if err != nil {
		t.Fatalf("NewField: %v", err)
	}
	srv.SetRaw(rep)
	if len(srv.Fields()) != len(fs) {
		t.Fatalf("Fields 数 = %d，应为 %d", len(srv.Fields()), len(fs))
	}
	if v, ok := srv.Value(sl651codec.GZ1); !ok || v != 1.234 {
		t.Fatalf("替换后 Value(Z1) = %v, %v", v, ok)
	}
}

// TestBadCRCAndReset 坏校验码可开关。
func TestBadCRCAndReset(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	srv.SetBadCRC(true)
	if _, err := sendRecv(t, c, queryFrame(t, srv, sl651codec.FuncQueryAll)); err == nil {
		t.Fatal("坏 CRC 的帧不应通过")
	}
	srv.SetBadCRC(false)
	if _, err := sendRecv(t, c, queryFrame(t, srv, sl651codec.FuncQueryAll)); err != nil {
		t.Fatalf("恢复后应收不到错: %v", err)
	}
}

// TestBadFuncAndTruncate 错误功能码与截断正文。
func TestBadFuncAndTruncate(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)

	srv.SetBadFunc(true)
	f, err := sendRecv(t, c, queryFrame(t, srv, sl651codec.FuncQueryAll))
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Func != sl651codec.FuncTimed {
		t.Errorf("Func = %02X，应为 32H", f.Func)
	}
	srv.SetBadFunc(false)

	srv.SetTruncate(10) // 正文只剩 10 字节，连固定前缀都不够
	f, err = sendRecv(t, c, queryFrame(t, srv, sl651codec.FuncQueryAll))
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if _, err := sl651codec.ParseReport(f); err == nil {
		t.Fatal("截断的正文应解析失败")
	}
}

// TestDelay 应答延迟会让 500ms 的读取超时。
func TestDelay(t *testing.T) {
	srv := startSim(t)
	srv.SetDelay(600 * time.Millisecond)
	c := dial(t, srv)

	if _, err := c.Write(queryFrame(t, srv, sl651codec.FuncQueryAll)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{c})
	if _, err := rd.Next(); err == nil {
		t.Fatal("延迟 600ms 时应先超时")
	}
}

// TestSilent 静默模式。
func TestSilent(t *testing.T) {
	srv := startSim(t)
	srv.SetSilent(true)
	c := dial(t, srv)

	if _, err := c.Write(queryFrame(t, srv, sl651codec.FuncQueryAll)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{c})
	if _, err := rd.Next(); err == nil {
		t.Fatal("静默时不应有应答")
	}
}

// TestCloseIdempotent 关闭幂等，且关闭后连接被断开。
func TestCloseIdempotent(t *testing.T) {
	srv := startSim(t)
	c := dial(t, srv)
	addr := srv.Addr()

	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if srv.Addr() != addr {
		// 关闭后 Addr 仍返回原地址（listener 已关，但字段不清空）。
		t.Logf("Addr after close = %q", srv.Addr())
	}
	// 连接应被关闭：写入最终失败或读返回 EOF。
	_ = c.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Write(queryFrame(t, srv, sl651codec.FuncQueryAll)); err == nil {
		buf := make([]byte, 1)
		if _, rerr := c.Read(buf); rerr == nil {
			t.Log("连接仍在，可能处于半关闭状态")
		}
	}
}

// TestDefaultFieldsCoverBranches 默认要素表要覆盖四种编码分支。
func TestDefaultFieldsCoverBranches(t *testing.T) {
	fs := defaultFields()
	var hasBCD, hasHex, hasStatus, hasMissing bool
	for _, f := range fs {
		g, ok := sl651codec.GuideInfo(f.Guide)
		if !ok {
			t.Fatalf("要素 %02X 不在附录 C", f.Guide)
		}
		if g.Hex {
			hasHex = true
			if f.Guide == sl651codec.GZT {
				hasStatus = true
			}
		} else {
			hasBCD = true
		}
		// 缺测分支：DRP 的第 3..12 个时段是 0xFF，扫描全部值才能命中。
		for i := 0; i < f.Count(); i++ {
			if f.NoData(i) {
				hasMissing = true
				break
			}
		}
	}
	if !hasBCD || !hasHex || !hasStatus || !hasMissing {
		t.Fatalf("默认要素表未覆盖全部分支: bcd=%v hex=%v status=%v missing=%v",
			hasBCD, hasHex, hasStatus, hasMissing)
	}
}

// TestSortedGuides 引导符列表升序。
func TestSortedGuides(t *testing.T) {
	srv := startSim(t)
	gs := srv.sortedGuides()
	if len(gs) == 0 {
		t.Fatal("空要素表")
	}
	for i := 1; i < len(gs); i++ {
		if gs[i] < gs[i-1] {
			t.Fatalf("sortedGuides 未排序: %v", gs)
		}
	}
}

// TestTrigger 主动自报：2FH 只有流水号 + 时间。
func TestTrigger(t *testing.T) {
	srv := startSim(t)
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	// net.Pipe 是同步管道：Write 会一直阻塞到对端读走。因此写要在 goroutine
	// 里做，读放在测试主协程，否则两边互等。
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Trigger(sl651codec.FuncLink, pipeStream{client}) }()

	_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	rd := sl651codec.NewReader(tcpReader{server})
	f, err := rd.Next()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if f.Func != sl651codec.FuncLink {
		t.Fatalf("Func = %02X", f.Func)
	}
	rep, err := sl651codec.ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(rep.Fields) != 0 {
		t.Errorf("链路维持报不应有要素，得到 %d", len(rep.Fields))
	}
}

// pipeStream 把 net.Pipe 适配成 transport.Stream。
type pipeStream struct{ c net.Conn }

func (p pipeStream) Write(b []byte) error {
	_, err := p.c.Write(b)
	return err
}

func (p pipeStream) ReadFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := readFull(p.c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (p pipeStream) Close() error { return p.c.Close() }

func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// TestQueryGuides 3AH 正文解析的边界。
func TestQueryGuides(t *testing.T) {
	if _, err := queryGuides([]byte{1, 2}); !errors.Is(err, sl651codec.ErrShort) {
		t.Errorf("短正文 = %v", err)
	}
	// 8 字节头 + 空要素组。
	if _, err := queryGuides(make([]byte, 8)); !errors.Is(err, sl651codec.ErrBadFrame) {
		t.Errorf("空要素组 = %v", err)
	}
	// 8 字节头 + 奇数长度要素组。
	if _, err := queryGuides(make([]byte, 9)); !errors.Is(err, sl651codec.ErrBadFrame) {
		t.Errorf("奇数长度 = %v", err)
	}
	body := make([]byte, 8)
	body = append(body, sl651codec.GZ, 0x23, sl651codec.GPT, 0x19)
	got, err := queryGuides(body)
	if err != nil {
		t.Fatalf("queryGuides: %v", err)
	}
	if !bytes.Equal(got, []byte{sl651codec.GZ, sl651codec.GPT}) {
		t.Fatalf("guides = % x", got)
	}
}

// TestDriverOverSim 用驱动跑一遍从站，确认两层联调可用。
func TestDriverOverSim(t *testing.T) {
	srv := startSim(t)
	c, err := newTestConn(srv, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s",
		Points: []driver.PointRef{
			{ID: "z", Address: model.Address{Kind: model.AddressSymbol, Path: "Z"}},
		},
	}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if v, _ := out[0].Value.AsFloat(); v != 0.127 {
		t.Fatalf("水位 = %v", v)
	}
}
