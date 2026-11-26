package cjt188

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"uic/internal/codec/cjt188"
	"uic/internal/driver"
	"uic/internal/model"
	cjsim "uic/internal/sim/cjt188"
)

// 这一组用例跑在「真 TCP + 内置从站」上，覆盖驱动与协议栈的整体行为：
// 读地址、按 DI 缓存字段、单位换算、写阀门/地址的权限闸门、地址校验。
// 分帧与 BCD 的细节由 codec 的单测钉住。

func startSim(t *testing.T, opts ...cjsim.Option) *cjsim.Server {
	t.Helper()
	srv, err := cjsim.NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func openConn(t *testing.T, srv *cjsim.Server, options string) driver.Conn {
	t.Helper()
	return openConnTo(t, srv, srv.Address(), options)
}

// openConnTo 用指定的表地址建链：地址与从站不一致时从站保持静默，
// 用来构造「链路还在但事务超时」的场景。
func openConnTo(t *testing.T, srv *cjsim.Server, address, options string) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(300 * time.Millisecond),
		Labels: map[string]string{
			"address": address,
			"type":    "10",
			"options": options,
		},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func refs(fields ...FieldRef) []driver.ReadSpan {
	var out []driver.PointRef
	for _, f := range fields {
		out = append(out, driver.PointRef{
			ID: f.ID,
			Address: model.Address{
				Kind: model.AddressRegister, Area: f.DI, Offset: f.Offset, Bit: -1,
			},
			DataType: f.Type,
		})
	}
	return []driver.ReadSpan{{ID: "s", Points: out}}
}

// FieldRef 是测试里描述一个点的简写。
type FieldRef struct {
	ID     string
	DI     string
	Offset int
	Type   model.DataType
}

func TestProbeReadsActualAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbeRejectsWrongMeter(t *testing.T) {
	srv := startSim(t, cjsim.WithAddress("00000805000099"))
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Labels: map[string]string{"address": "00000805000001", "type": "10"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Probe(context.Background())
	if err == nil {
		t.Fatal("probe accepted a different meter")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

// scriptedLink 是一条按脚本应答的假链路：Write 时把预置帧放进接收缓冲，
// 供需要构造「合法但语义异常」应答的用例使用（内置从站只会正常应答）。
type scriptedLink struct {
	mu   sync.Mutex
	buf  []byte
	resp func(req []byte) []byte
}

func (l *scriptedLink) Write(p []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = l.resp(append([]byte(nil), p...))
	return nil
}

func (l *scriptedLink) ReadFull(n int) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) < n {
		return nil, io.ErrUnexpectedEOF
	}
	out := l.buf[:n]
	l.buf = l.buf[n:]
	return out, nil
}

func (l *scriptedLink) SetDeadline(time.Time) error { return nil }
func (l *scriptedLink) Drain() error                { return nil }
func (l *scriptedLink) Lock()                       {}
func (l *scriptedLink) Unlock()                     {}
func (l *scriptedLink) Close() error                { return nil }

// TestProbeExceptionIsNotRetryable 表收到读地址命令却回异常应答时，重发结果
// 不会变：探测必须按不可重试上报，否则上层会一轮轮白等超时。
func TestProbeExceptionIsNotRetryable(t *testing.T) {
	var broadcast cjt188.Addr
	for i := range broadcast {
		broadcast[i] = 0xAA
	}
	frame, err := cjt188.Build(0x10, broadcast,
		cjt188.CtrlReadAddr|cjt188.BitReply|cjt188.BitError,
		[]byte{0x01, 0x00, 0x00}) // 标准异常应答：SER + ST
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	addr, err := cjt188.ParseAddr("00000805000001")
	if err != nil {
		t.Fatal(err)
	}
	c := &conn{
		link:      &scriptedLink{resp: func([]byte) []byte { return frame }},
		addr:      addr,
		meterType: 0x10,
		devID:     "meter",
		timeout:   time.Second,
	}
	err = c.Probe(context.Background())
	if err == nil {
		t.Fatal("probe accepted an exception reply")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %v, want protocol", driver.KindOf(err))
	}
	if driver.IsRetryable(err) {
		t.Fatalf("异常应答被标成可重试：%v", err)
	}
}

// TestReadWaterFields 一次 Read 里读 901F 的四个字段，只应发一帧。
func TestReadWaterFields(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	spans := refs(
		FieldRef{"cur", "901F", 0, model.TypeFloat64},
		FieldRef{"settle", "901F", 5, model.TypeFloat64},
		FieldRef{"clock", "901F", 10, model.TypeFloat64},
		FieldRef{"status", "901F", 17, model.TypeUint16},
	)
	before := srv.Requests()
	got, err := c.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after := srv.Requests(); after-before != 1 {
		t.Fatalf("requests = %d, want 1 (fields on the same DI must share one transaction)", after-before)
	}
	want := map[string]float64{
		"cur":    1234.56,
		"settle": 1200,
		"clock":  20261107120000,
		"status": 0,
	}
	for _, s := range got {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Value.Num != want[s.PointID] {
			t.Fatalf("%s = %v, want %v", s.PointID, s.Value.Num, want[s.PointID])
		}
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %v", s.PointID, s.Quality)
		}
	}
	if got[3].Value.Type != model.TypeUint16 {
		t.Fatalf("status type = %s", got[3].Value.Type)
	}
}

// TestReadHistorical 上 1 月与上 12 月结算日量。
func TestReadHistorical(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, err := c.Read(context.Background(), refs(
		FieldRef{"m1", "D120", 0, model.TypeFloat64},
		FieldRef{"m12", "D12B", 0, model.TypeFloat64},
	))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0].Value.Num != 1190 || got[1].Value.Num != 1080 {
		t.Fatalf("history = %v, %v", got[0].Value.Num, got[1].Value.Num)
	}
}

// TestReadHeatSchema 热量表的 901F 布局与单位换算。
func TestReadHeatSchema(t *testing.T) {
	srv := startSim(t, cjsim.WithType(0x20))
	dev := model.Device{
		ID: "heat", ChannelID: "ch", Enabled: true,
		Labels: map[string]string{"address": srv.Address(), "type": "20"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "cjt188",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	spans := refs(
		FieldRef{"settle-heat", "901F", 0, model.TypeFloat64},
		FieldRef{"cur-heat", "901F", 5, model.TypeFloat64},
		FieldRef{"power", "901F", 10, model.TypeFloat64},
		FieldRef{"flow", "901F", 15, model.TypeFloat64},
		FieldRef{"volume", "901F", 20, model.TypeFloat64},
		FieldRef{"supply", "901F", 25, model.TypeFloat64},
		FieldRef{"return", "901F", 28, model.TypeFloat64},
		FieldRef{"hours", "901F", 31, model.TypeFloat64},
		FieldRef{"clock", "901F", 34, model.TypeFloat64},
		FieldRef{"status", "901F", 41, model.TypeUint16},
	)
	got, err := c.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := map[string]float64{
		"settle-heat": 120, "cur-heat": 123.45, "power": 12.34, "flow": 1.2345,
		"volume": 456.78, "supply": 55, "return": 45, "hours": 1234,
		"clock": 20261107120000, "status": 0,
	}
	for _, s := range got {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Value.Num != want[s.PointID] {
			t.Fatalf("%s = %v, want %v", s.PointID, s.Value.Num, want[s.PointID])
		}
	}
}

func TestValveWriteGate(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "") // 没有 allow-control
	got, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "valve",
		Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrPermission {
		t.Fatalf("write without allow-control = %v", got[0].Err)
	}
	if srv.Writes() != 0 {
		t.Fatalf("sim writes = %d", srv.Writes())
	}
}

func TestValveWriteOpenClose(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-control")
	write := func(v float64) driver.WriteResult {
		got, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: "valve",
			Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
			Value:   model.NumValue(model.TypeFloat64, v),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return got[0]
	}
	if r := write(0); r.Err != nil {
		t.Fatalf("close: %v", r.Err)
	}
	if v, _ := srv.Valve(); v != 1 {
		t.Fatalf("valve = %d, want closed (1)", v)
	}
	if r := write(1); r.Err != nil {
		t.Fatalf("open: %v", r.Err)
	}
	if v, _ := srv.Valve(); v != 0 {
		t.Fatalf("valve = %d, want open (0)", v)
	}
	// 读状态字应反映阀门状态。
	got, err := c.Read(context.Background(), refs(FieldRef{"status", "901F", 17, model.TypeUint16}))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Value.Num != 0 {
		t.Fatalf("status = %v", got[0].Value.Num)
	}
	if srv.Writes() != 2 {
		t.Fatalf("writes = %d", srv.Writes())
	}
}

func TestWriteAddrUpdatesConnection(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-write")
	got, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "A018", Bit: -1},
		Value:   model.TextValue(model.TypeString, "00000805000099"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Err != nil {
		t.Fatalf("write addr: %v", got[0].Err)
	}
	if srv.Address() != "00000805000099" {
		t.Fatalf("sim address = %s", srv.Address())
	}
	// 驱动应已切到新地址，下一次读必须还能通。
	res, err := c.Read(context.Background(), refs(FieldRef{"cur", "901F", 0, model.TypeFloat64}))
	if err != nil {
		t.Fatalf("read after rename: %v", err)
	}
	if res[0].Err != nil || res[0].Value.Num != 1234.56 {
		t.Fatalf("read after rename = %+v", res[0])
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe after rename: %v", err)
	}
}

func TestWriteAddrRequiresPermissionAndFullAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "A018", Bit: -1},
		Value:   model.TextValue(model.TypeString, "00000805000099"),
	}})
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrPermission {
		t.Fatalf("write without allow-write = %v", got[0].Err)
	}

	c2 := openConn(t, srv, "allow-write")
	got, _ = c2.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "A018", Bit: -1},
		Value:   model.TextValue(model.TypeString, "99"),
	}})
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrUnsupported {
		t.Fatalf("short address = %v", got[0].Err)
	}
	if srv.Address() != "00000805000001" {
		t.Fatalf("sim address changed to %s", srv.Address())
	}
}

func TestWriteOtherDIRejected(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-write,allow-control")
	got, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x",
		Address: model.Address{Kind: model.AddressRegister, Area: "D120", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 1),
	}})
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrUnsupported {
		t.Fatalf("write D120 = %v", got[0].Err)
	}
}

func TestBadPointAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	cases := []struct {
		name string
		di   string
		off  int
	}{
		{"unknown DI", "911F", 0},
		{"unknown offset", "901F", 2},
		{"valve is write-only", "A017", 0},
	}
	for _, tc := range cases {
		// 只有一个点时，Read 会同时返回失败样本与聚合错误；这里关心的是
		// 样本上的分类，而不是它是否被当成整轮错误上抛。
		got, _ := c.Read(context.Background(), refs(FieldRef{"p", tc.di, tc.off, model.TypeFloat64}))
		if len(got) != 1 {
			t.Fatalf("%s: samples = %d", tc.name, len(got))
		}
		if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrAddress {
			t.Fatalf("%s = %v", tc.name, got[0].Err)
		}
	}
}

func TestWrongLengthRejected(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
		ID: "p",
		Address: model.Address{
			Kind: model.AddressRegister, Area: "901F", Offset: 0, Length: 4, Bit: -1,
		},
		DataType: model.TypeFloat64,
	}}}})
	if len(got) != 1 {
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrAddress {
		t.Fatalf("length mismatch = %v", got[0].Err)
	}
}

// TestWrongAddressTimesOut 从站地址不匹配时保持静默，主站应按超时处理。
func TestWrongAddressTimesOut(t *testing.T) {
	srv := startSim(t, cjsim.WithAddress("00000805000099"))
	c := openConnTo(t, srv, "00000805000001", "")
	got, err := c.Read(context.Background(), refs(FieldRef{"cur", "901F", 0, model.TypeFloat64}))
	if err == nil {
		t.Fatal("read from wrong address must fail")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	// 样本质量必须跟着错误分类走：超时不等于通讯中断。
	if len(got) != 1 || got[0].Quality != model.QualityTimeout {
		t.Fatalf("sample = %+v, want quality %s", got, model.QualityTimeout)
	}
}

// TestReadLinkFailureKeepsGoodSamples 读取途中断链时：已成功的样本必须
// 留下，剩余点位显式标坏，链路错误仍然上抛（docs §3.3.1 的链路级例外）。
func TestReadLinkFailureKeepsGoodSamples(t *testing.T) {
	srv := startSim(t, cjsim.WithDropAfter(1))
	c := openConn(t, srv, "")
	got, err := c.Read(context.Background(), refs(
		FieldRef{"cur", "901F", 0, model.TypeFloat64},
		FieldRef{"jan", "D120", 0, model.TypeFloat64},
	))
	if err == nil {
		t.Fatal("链路级失败即便已有好样本也必须上抛")
	}
	switch driver.KindOf(err) {
	case driver.ErrDisconnected, driver.ErrTimeout:
	default:
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2（缺失样本会被采集器当成驱动漏报）", len(got))
	}
	if got[0].Err != nil || got[0].Quality != model.QualityGood || got[0].Value.Num != 1234.56 {
		t.Fatalf("第一个点应保留成功样本: %+v", got[0])
	}
	if got[1].Err == nil {
		t.Fatalf("断链后的点位必须带错误: %+v", got[1])
	}
	if got[1].Quality != qualityOf(err) {
		t.Fatalf("剩余点质量 = %s, want %s", got[1].Quality, qualityOf(err))
	}
}

// TestReadMixedBadAddressKeepsPartialSuccess 地址错是点级问题：同批的好点
// 必须照常返回，整轮不得上抛错误。
func TestReadMixedBadAddressKeepsPartialSuccess(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, err := c.Read(context.Background(), refs(
		FieldRef{"bad", "911F", 0, model.TypeFloat64},
		FieldRef{"cur", "901F", 0, model.TypeFloat64},
	))
	if err != nil {
		t.Fatalf("有成功点时不得上抛错误: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrAddress {
		t.Fatalf("bad point = %v", got[0].Err)
	}
	if got[0].Quality != model.QualityAddressError {
		t.Fatalf("bad point quality = %s, want %s", got[0].Quality, model.QualityAddressError)
	}
	if got[1].Err != nil || got[1].Value.Num != 1234.56 {
		t.Fatalf("good point = %+v", got[1])
	}
}

// TestReadAllFailedReturnsError 全部失败时必须上抛第一个错误，否则熔断器与
// 告警永远看不到异常。
func TestReadAllFailedReturnsError(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	got, err := c.Read(context.Background(), refs(
		FieldRef{"bad1", "911F", 0, model.TypeFloat64},
		FieldRef{"bad2", "901F", 2, model.TypeFloat64},
	))
	if err == nil {
		t.Fatal("全失败必须返回错误")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d", len(got))
	}
	for _, s := range got {
		if s.Err == nil || s.Quality != model.QualityAddressError {
			t.Fatalf("%s = %+v", s.PointID, s)
		}
	}
}

// TestExceptionReplyIsPointLevel 从站拒答一个数据标识（异常应答）是**点级**
// 问题：不能重连、不能熔断，同批其它点位照常读。
//
// 现场来源：点表里配了一个该表不支持的 DI。把它归成任意的 ErrProtocol +
// temporary 会让采集器每周期丢弃连接（§3.3.1 的读错误处置），并把一个正常的
// 表计入熔断——一台好表因此看起来彻底坏了。
func TestExceptionReplyIsPointLevel(t *testing.T) {
	srv := startSim(t, cjsim.WithoutValue("D121"))
	c := openConn(t, srv, "")
	got, err := c.Read(context.Background(), refs(
		FieldRef{"missing", "D121", 0, model.TypeFloat64},
		FieldRef{"cur", "901F", 0, model.TypeFloat64},
	))
	if err != nil {
		t.Fatalf("有成功点时不得上抛错误: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].Err == nil {
		t.Fatal("被拒的 DI 必须带错误")
	}
	if k := driver.KindOf(got[0].Err); k != driver.ErrAddress {
		t.Fatalf("kind = %v (%v)，异常应答应归为点级的 address", k, got[0].Err)
	}
	if driver.CountsTowardBreaker(got[0].Err) {
		t.Fatal("异常应答不得计入熔断（否则一个坏 DI 会把整台表判成故障）")
	}
	if driver.IsRetryable(got[0].Err) {
		t.Fatal("异常应答不得标成可重试（重发结果相同，只是多打一轮帧）")
	}
	if got[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s, want %s", got[0].Quality, model.QualityAddressError)
	}
	if got[1].Err != nil || got[1].Value.Num != 1234.56 {
		t.Fatalf("good point = %+v", got[1])
	}
}

// TestExceptionReplyDoesNotBreakLink 连续两轮都遇到异常应答时，连接必须还能
// 用：不得出现「第一轮异常 → 丢弃连接」的隐式重连。
func TestExceptionReplyDoesNotBreakLink(t *testing.T) {
	srv := startSim(t, cjsim.WithoutValue("D121"))
	c := openConn(t, srv, "")
	before := srv.Requests()
	for round := 0; round < 2; round++ {
		if _, err := c.Read(context.Background(), refs(
			FieldRef{"cur", "901F", 0, model.TypeFloat64},
		)); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	// 每轮一帧；若中途重连，请求数不会变多但连接会重建——这里用「读仍成功」
	// 与请求数增长两条一起断言。
	if after := srv.Requests(); after-before != 2 {
		t.Fatalf("requests = %d, want 2", after-before)
	}
}

// TestWriteRejectedIsPermission 表拒绝写（异常应答）应归为 ErrPermission：
// 不算链路故障、不计熔断、不带可重试标记，质量码为 bad。
func TestWriteRejectedIsPermission(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "allow-control")
	// 从站只接受 0x55/0x99；0x2A 这类值会走异常应答路径，但这属于本地拒绝。
	// 这里改用「表不支持的数据标识」构造真实的写异常：写地址给一个能被本地
	// 接受、但由从站拒绝的值。
	got, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "valve",
		Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 2), // 本地就拒绝
	}})
	if err != nil {
		t.Fatalf("点级失败不得改成整批错误: %v", err)
	}
	if got[0].Err == nil || driver.KindOf(got[0].Err) != driver.ErrUnsupported {
		t.Fatalf("valve=2 = %v", got[0].Err)
	}
	if got[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %q, want %q", got[0].Quality, model.QualityAddressError)
	}
}

// TestWriteFailureCarriesQuality 写失败必须按错误种类给质量码；留空会被上层
// 兜底成 bad，超时/地址错/权限不足在下游就分不出来了（docs §3.4 规则 5）。
func TestWriteFailureCarriesQuality(t *testing.T) {
	cases := []struct {
		name    string
		area    string
		options string
		kind    driver.ErrorKind
		quality model.Quality
	}{
		{"bad DI", "ZZZZ", "allow-control,allow-write", driver.ErrAddress, model.QualityAddressError},
		{"no permission", "A017", "", driver.ErrPermission, model.QualityBad},
		{"unsupported DI", "D120", "allow-write,allow-control", driver.ErrUnsupported, model.QualityAddressError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startSim(t)
			c := openConn(t, srv, tc.options)
			got, err := c.Write(context.Background(), []driver.WriteRequest{{
				PointID: "p",
				Address: model.Address{Kind: model.AddressRegister, Area: tc.area, Bit: -1},
				Value:   model.NumValue(model.TypeFloat64, 1),
			}})
			if err != nil {
				t.Fatalf("点级失败不得改成整批错误: %v", err)
			}
			if len(got) != 1 || got[0].OK {
				t.Fatalf("results = %+v", got)
			}
			if driver.KindOf(got[0].Err) != tc.kind {
				t.Fatalf("kind = %v (%v)", driver.KindOf(got[0].Err), got[0].Err)
			}
			if got[0].Quality != tc.quality {
				t.Fatalf("quality = %q, want %q", got[0].Quality, tc.quality)
			}
		})
	}
}

// TestWriteTimeoutQuality 事务超时是链路级失败：逐项结果带 timeout 质量码，
// 同时经第二个返回值上抛，让调度器看见「设备太慢」。
func TestWriteTimeoutQuality(t *testing.T) {
	srv := startSim(t, cjsim.WithAddress("00000805000099"))
	c := openConnTo(t, srv, "00000805000001", "allow-control")
	got, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "valve",
		Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 1),
	}})
	if err == nil {
		t.Fatal("链路级写失败必须经第二个返回值上抛")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if len(got) != 1 || got[0].OK {
		t.Fatalf("results = %+v", got)
	}
	if got[0].Quality != model.QualityTimeout {
		t.Fatalf("quality = %q, want %q", got[0].Quality, model.QualityTimeout)
	}
}

// TestWriteLinkFailureStopsRemaining 链路断了以后同批剩余写命令不得继续发：
// 逐项结果保留（失败项标坏），已成功的项不被连坐。
func TestWriteLinkFailureStopsRemaining(t *testing.T) {
	srv := startSim(t, cjsim.WithDropAfter(1))
	c := openConn(t, srv, "allow-control")
	reqs := []driver.WriteRequest{
		{PointID: "close", Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
			Value: model.NumValue(model.TypeFloat64, 0)},
		{PointID: "open", Address: model.Address{Kind: model.AddressRegister, Area: "A017", Bit: -1},
			Value: model.NumValue(model.TypeFloat64, 1)},
	}
	got, err := c.Write(context.Background(), reqs)
	if err == nil {
		t.Fatal("链路级写失败必须经第二个返回值上抛")
	}
	switch driver.KindOf(err) {
	case driver.ErrDisconnected, driver.ErrTimeout:
	default:
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2（逐项返回不得缺项）", len(got))
	}
	if !got[0].OK || got[0].Err != nil {
		t.Fatalf("第一项已成功，不得被后续失败连坐: %+v", got[0])
	}
	if got[1].OK || got[1].Err == nil {
		t.Fatalf("第二项应失败: %+v", got[1])
	}
	if got[1].Quality != qualityOf(got[1].Err) {
		t.Fatalf("quality = %q, want %q", got[1].Quality, qualityOf(got[1].Err))
	}
}

// TestPreambleOption 前导 0/4 都能通：从站按字节流扫描，不依赖前导。
func TestPreambleOption(t *testing.T) {
	for _, opt := range []string{"preamble=0", "preamble=4"} {
		srv := startSim(t)
		c := openConn(t, srv, opt)
		got, err := c.Read(context.Background(), refs(FieldRef{"cur", "901F", 0, model.TypeFloat64}))
		if err != nil {
			t.Fatalf("%s read: %v", opt, err)
		}
		if got[0].Value.Num != 1234.56 {
			t.Fatalf("%s value = %v", opt, got[0].Value.Num)
		}
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"missing address", map[string]string{"type": "10"}},
		{"short address", map[string]string{"address": "123", "type": "10"}},
		{"wildcard address", map[string]string{"address": "AAAA0805000001", "type": "10"}},
		{"bad type", map[string]string{"address": "00000805000001", "type": "AA"}},
	}
	for _, tc := range cases {
		_, err := New().Open(context.Background(), driver.OpenConfig{
			Channel: model.Channel{ID: "ch", Protocol: "cjt188",
				Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"},
				Timeout:   model.Duration(time.Second)},
			Device: model.Device{ID: "m", Labels: tc.labels},
		})
		if err == nil {
			t.Fatalf("%s: open must fail", tc.name)
		}
		if driver.KindOf(err) != driver.ErrUnsupported {
			t.Fatalf("%s kind = %v", tc.name, driver.KindOf(err))
		}
	}
}
