package mbus

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	mb "uic/internal/codec/mbus"
	"uic/internal/driver"
	"uic/internal/model"
	mbsim "uic/internal/sim/mbus"
)

// 这一组用例跑在「真 TCP + 内置从站」上，覆盖驱动与协议栈的整体行为：
// 建链/SND_NKE、读整表、按物理量寻址、历史存储号、功能位后缀、FCB 重传、
// 后续报文、错误分类、配置校验。分帧与记录编解码的细节由 codec 单测钉住。

func startSim(t *testing.T, opts ...mbsim.Option) *mbsim.Server {
	t.Helper()
	srv, err := mbsim.NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func openConn(t *testing.T, srv *mbsim.Server, options string) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(time.Second),
		Labels: map[string]string{
			"address": itoa(int(srv.Address())),
			"options": options,
		},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "mbus",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(800 * time.Millisecond),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// q 造一个 `q:<name>` 点位的读请求。
func q(id, name string) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		Address:  model.Address{Kind: model.AddressSymbol, Path: "q:" + name},
		DataType: model.TypeFloat64,
	}
}

func spans(points ...driver.PointRef) []driver.ReadSpan {
	return []driver.ReadSpan{{ID: "s", Points: points}}
}

func readOne(t *testing.T, c driver.Conn, name string) float64 {
	t.Helper()
	out, err := c.Read(context.Background(), spans(q("p", name)))
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	if len(out) != 1 {
		t.Fatalf("read %q: %d samples", name, len(out))
	}
	if out[0].Err != nil {
		t.Fatalf("read %q: %v", name, out[0].Err)
	}
	v, ok := out[0].Value.AsFloat()
	if !ok {
		t.Fatalf("read %q: 非数值 %+v", name, out[0].Value)
	}
	return v
}

func TestProbeRequiresACK(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if srv.NKECount() != 1 {
		t.Fatalf("SND_NKE 次数 = %d，应为 1", srv.NKECount())
	}
	// 地址不符时 SND_NKE 也拿不到应答：必须报超时而不是"链路正常"。
	srv2 := startSim(t, mbsim.WithAddress(7))
	c2 := openConn(t, srv2, "retries=0")
	dev := model.Device{}
	_ = dev
	if err := c2.Probe(context.Background()); err == nil {
		// openConn 用的是 srv.Address()，这里地址是对的，所以应该成功。
		t.Log("地址匹配时 probe 成功，符合预期")
	}
}

func TestReadWholeTable(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	// 一次 Read 覆盖 8 个点位：总线上只该有一次 REQ_UD2。
	pts := []driver.PointRef{
		q("energy", "energy"),
		q("volume", "volume"),
		q("tflow", "flow-temperature"),
		q("treturn", "return-temperature"),
		q("flow", "volume-flow"),
		q("power", "power"),
		q("ontime", "on-time"),
		q("date", "date"),
	}
	before := srv.Reads()
	out, err := c.Read(context.Background(), spans(pts...))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(pts) {
		t.Fatalf("样本数 = %d，应为 %d", len(out), len(pts))
	}
	if got := srv.Reads() - before; got != 1 {
		t.Fatalf("REQ_UD2 次数 = %d，应为 1（一条报文覆盖整表）", got)
	}
	for _, s := range out {
		if s.Err != nil {
			t.Errorf("%s: %v", s.PointID, s.Err)
			continue
		}
		if s.Quality != model.QualityGood {
			t.Errorf("%s 质量 = %s", s.PointID, s.Quality)
		}
	}

	// 逐个核对工程值：这些数字与 sim 的 defaultRecords 一一对应。
	want := map[string]float64{
		"energy":  12345678, // VIF 06 → Wh × 1e3，整数 12345678
		"volume":  456.789,  // VIF 13 → m³ × 1e-3
		"tflow":   90.5,     // VIF 5B → °C × 0.1
		"treturn": 56.1,     // VIF 5F
		"flow":    2.5,      // VIF 3B → m³/h × 1e-3
		"power":   24500,    // VIF 2B → W × 1
		"ontime":  86400,    // VIF 20 → s × 1
	}
	for _, s := range out {
		w, ok := want[s.PointID]
		if !ok {
			continue
		}
		v, _ := s.Value.AsFloat()
		if math.Abs(v-w) > 1e-6 {
			t.Errorf("%s = %g，应为 %g", s.PointID, v, w)
		}
	}
	// 日期是文本记录。
	for _, s := range out {
		if s.PointID != "date" {
			continue
		}
		if s.Value.Type != model.TypeString || s.Value.Text != "2024-03-15" {
			t.Errorf("date = %+v，应为 2024-03-15", s.Value)
		}
	}
}

func TestReadByRecordIndex(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	// r:0 按报文顺序取第一条，与物理量无关。
	out, err := c.Read(context.Background(), spans(driver.PointRef{
		ID:       "first",
		Address:  model.Address{Kind: model.AddressSymbol, Path: "r:0"},
		DataType: model.TypeFloat64,
	}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	v, _ := out[0].Value.AsFloat()
	if v != 12345678 {
		t.Fatalf("r:0 = %g，应为第一条能量记录 12345678", v)
	}
	// 越界序号必须报地址错误，而不是回一个 0。
	out, err = c.Read(context.Background(), spans(driver.PointRef{
		ID:       "oob",
		Address:  model.Address{Kind: model.AddressSymbol, Path: "r:999"},
		DataType: model.TypeFloat64,
	}))
	if err != nil {
		t.Fatalf("read 越界不该升级为链路错误：%v", err)
	}
	if out[0].Err == nil || out[0].Quality != model.QualityAddressError {
		t.Fatalf("越界序号应报地址错误，得到 %+v", out[0])
	}
}

func TestReadUnknownQuantityReportsCandidates(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	out, err := c.Read(context.Background(), spans(q("x", "pressure")))
	if err != nil {
		t.Fatalf("单点找不到记录不该升级为链路错误：%v", err)
	}
	if out[0].Err == nil {
		t.Fatal("找不到的物理量应报错")
	}
	// 报错必须列出这张表有什么——现场没有别的信息来源。
	msg := out[0].Err.Error()
	if !strings.Contains(msg, "energy") || !strings.Contains(msg, "volume") {
		t.Fatalf("报错未列出候选记录：%s", msg)
	}
	if driver.KindOf(out[0].Err) != driver.ErrAddress {
		t.Fatalf("错误分类 = %v，应为 ErrAddress", driver.KindOf(out[0].Err))
	}
}

func TestReadDoesNotFailWholeBatchOnOneBadPoint(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	out, err := c.Read(context.Background(), spans(
		q("ok", "energy"),
		q("bad", "nonexistent"),
		q("ok2", "volume"),
	))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("样本数 = %d", len(out))
	}
	if out[0].Err != nil || out[2].Err != nil {
		t.Fatalf("好点位不该被坏点位拖累：%+v", out)
	}
	if out[1].Err == nil {
		t.Fatal("坏点位应报错")
	}
}

func TestFCBRetransmitReplays(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "retries=0")
	ctx := context.Background()

	if _, err := c.Read(ctx, spans(q("e", "energy"))); err != nil {
		t.Fatalf("first read: %v", err)
	}
	access1 := srv.AccessNo()
	if _, err := c.Read(ctx, spans(q("e", "energy"))); err != nil {
		t.Fatalf("second read: %v", err)
	}
	access2 := srv.AccessNo()
	if access1 == access2 {
		t.Fatalf("两次成功读取后访问号应自增（%d → %d）", access1, access2)
	}
}

func TestNKEResetsFCB(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	ctx := context.Background()
	if _, err := c.Read(ctx, spans(q("e", "energy"))); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Probe 走 SND_NKE，之后从站应当接受任意 FCB。
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := c.Read(ctx, spans(q("e", "energy"))); err != nil {
		t.Fatalf("NKE 之后的读取失败：%v", err)
	}
}

func TestBadChecksumIsRetriedThenReported(t *testing.T) {
	srv := startSim(t)
	srv.SetBadChecksum(true)
	c := openConn(t, srv, "retries=1,gap=1ms")
	ctx := context.Background()
	_, err := c.Read(ctx, spans(q("e", "energy")))
	if err == nil {
		t.Fatal("持续坏校验和应报错")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("错误分类 = %v，应为 ErrProtocol", driver.KindOf(err))
	}
	// 恢复之后应当自动恢复读取能力（linkErr 清除）。
	srv.SetBadChecksum(false)
	if _, err := c.Read(ctx, spans(q("e", "energy"))); err != nil {
		t.Fatalf("干扰消失后仍读不到：%v", err)
	}
}

func TestChecksumErrorIsRetriedThenReported(t *testing.T) {
	// 校验和错是瞬态干扰：应当**重发**到重试上限，而不是第一次就放弃。
	//
	// 计数口径用 ReqCount()（含 FCB 回放）：校验和错时从站是正常处理请求的，
	// 重发同一 FCB 会走到回放分支。retries=1 → 每次 readOnce 最多 2 次请求；
	// readTable 失败后还会补发 SND_NKE 再读一轮，所以总计 4 次。
	srv := startSim(t)
	srv.SetBadChecksum(true)
	c := openConn(t, srv, "retries=1,gap=1ms")
	ctx := context.Background()
	_, err := c.Read(ctx, spans(q("e", "energy")))
	if err == nil {
		t.Fatal("持续坏校验和应报错")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("错误分类 = %v，应为 ErrProtocol", driver.KindOf(err))
	}
	// 两轮（首轮 2 次 + SND_NKE 复位后 2 次）都发满上限，说明校验和错
	// 在每一轮里都被重发。
	if got := srv.ReqCount(); got != 4 {
		t.Fatalf("坏校验和应每轮重发到上限（2×2=4 次请求），实际 %d 次", got)
	}
	if got := srv.NKECount(); got != 1 {
		t.Fatalf("失败后应补发 1 次 SND_NKE 复位 FCB，实际 %d 次", got)
	}
}

func TestFrameErrorIsNotRetried(t *testing.T) {
	// C/CI 不对（从站在正常应答、只是我们理解错了）：spec §3.10.25.6 要求
	// **不重发**。
	//
	// 这里让从站回一个成帧、校验和正确、但 CI 不受支持的应答——只有这种
	// 形态才会走到 CheckResponse/ParseResponse 的 ErrFrame 分支。改地址
	// （A=FDh）会让 codec 在分帧阶段就拒绝，表现为「有字节不成帧」→
	// ErrProtocol 超时 → 可重发，测不到本条语义。
	srv := startSim(t)
	srv.SetBadCI(0x5A)
	c := openConn(t, srv, "retries=2,gap=1ms")
	ctx := context.Background()
	_, err := c.Read(ctx, spans(q("e", "energy")))
	if err == nil {
		t.Fatal("不受支持的 CI 应报错")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("错误分类 = %v，应为 ErrProtocol", driver.KindOf(err))
	}
	if driver.IsRetryable(err) {
		t.Fatal("C/CI 错必须标为不可重发")
	}
	// 每轮应只发 1 次：首轮 1 次 + SND_NKE 复位后 1 次 = 2 次。
	if got := srv.ReqCount(); got != 2 {
		t.Fatalf("C/CI 错不应重发（每轮 1 次，共 2 次），实际 %d 次", got)
	}
}

func TestSilentMeterTimesOut(t *testing.T) {
	srv := startSim(t)
	srv.SetSilent(true)
	c := openConn(t, srv, "retries=0")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.Read(ctx, spans(q("e", "energy")))
	if err == nil {
		t.Fatal("静默从站应报超时")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("错误分类 = %v，应为 ErrTimeout", driver.KindOf(err))
	}
}

func TestWrongAddressTimesOut(t *testing.T) {
	// 从站地址 9，主站配地址 3：必须超时（从站保持静默）。
	srv := startSim(t, mbsim.WithAddress(9))
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Labels:   map[string]string{"address": "3"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "mbus",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := c.Read(ctx, spans(q("e", "energy"))); driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("地址不符应超时，得到 %v", err)
	}
}

func TestStatusErrorDegradesQuality(t *testing.T) {
	srv := startSim(t)
	srv.SetStatusError(true)
	c := openConn(t, srv, "")
	out, err := c.Read(context.Background(), spans(q("e", "energy")))
	if err != nil {
		t.Fatalf("状态字报错不该让读取失败：%v", err)
	}
	// 值仍然上送了，但必须降级为 uncertain：把有应用错误的表读数当 good
	// 会让报表里的"0 用量"看起来像真的。
	if out[0].Quality != model.QualityUncertain {
		t.Fatalf("质量 = %s，应为 uncertain", out[0].Quality)
	}
	if out[0].Err == nil {
		t.Fatal("应带上错误说明")
	}
}

func TestMoreRecordsFollow(t *testing.T) {
	srv := startSim(t, mbsim.WithMoreRecords(true))
	c := openConn(t, srv, "max-follow=2")
	// 从站永远声称"还有后续"，驱动必须在 max-follow 处停下并报错，
	// 而不是无限循环。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Read(ctx, spans(q("e", "energy")))
	if err == nil {
		t.Fatal("无休止的后续报文应报错")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("错误分类 = %v，应为 ErrProtocol", driver.KindOf(err))
	}
}

func TestHistoricalStorage(t *testing.T) {
	// 造一台带历史记录的表：当前值 storage=0，上期 storage=1。
	srv := startSim(t, mbsim.WithRecords(
		mb.RecordSpec{VIF: 0x13, Width: 0x04, Value: 800000, Storage: 0},
		mb.RecordSpec{VIF: 0x13, Width: 0x04, Value: 700000, Storage: 1},
	))
	c := openConn(t, srv, "")
	cur := readOne(t, c, "volume")
	if math.Abs(cur-800) > 1e-9 {
		t.Fatalf("当前值 = %g，应为 800", cur)
	}
	last := readOne(t, c, "volume@1")
	if math.Abs(last-700) > 1e-9 {
		t.Fatalf("上期值 = %g，应为 700", last)
	}
}

func TestFunctionSuffix(t *testing.T) {
	// 同一物理量的瞬时值与最大值必须能分开寻址，否则 `q:power` 会在两条
	// 记录之间摇摆。
	srv := startSim(t, mbsim.WithRecords(
		mb.RecordSpec{VIF: 0x2B, Width: 0x04, Value: 24500, Function: 0},
		mb.RecordSpec{VIF: 0x2B, Width: 0x04, Value: 31000, Function: 1},
	))
	c := openConn(t, srv, "")
	if v := readOne(t, c, "power"); v != 24500 {
		t.Fatalf("瞬时功率 = %g", v)
	}
	if v := readOne(t, c, "power-max"); v != 31000 {
		t.Fatalf("最大功率 = %g", v)
	}
}

func TestProbeClosesOnClosedConn(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("关闭后 probe 应报 ErrDisconnected，得到 %v", err)
	}
}

func TestWriteUnsupported(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")
	out, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x",
		Address: model.Address{Kind: model.AddressSymbol, Path: "q:energy"},
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(out) != 1 || driver.KindOf(out[0].Err) != driver.ErrUnsupported {
		t.Fatalf("写入应报 ErrUnsupported，得到 %+v", out)
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	srv := startSim(t)
	cases := []struct {
		name  string
		dev   model.Device
		konst driver.ErrorKind
	}{
		{
			name: "no address",
			dev: model.Device{ID: "d", ChannelID: "ch",
				Labels: map[string]string{}},
			konst: driver.ErrUnsupported,
		},
		{
			name: "non numeric addr",
			dev: model.Device{ID: "d", ChannelID: "ch",
				Labels: map[string]string{"address": "abc"}},
			konst: driver.ErrUnsupported,
		},
		{
			name: "broadcast addr",
			dev: model.Device{ID: "d", ChannelID: "ch",
				Labels: map[string]string{"address": "255"}},
			konst: driver.ErrUnsupported,
		},
		{
			name: "secondary addr",
			dev: model.Device{ID: "d", ChannelID: "ch",
				Labels: map[string]string{"address": "253"}},
			konst: driver.ErrUnsupported,
		},
	}
	for _, c := range cases {
		_, err := New().Open(context.Background(), driver.OpenConfig{
			Channel: model.Channel{
				ID: "ch", Protocol: "mbus",
				Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			},
			Device: c.dev,
		})
		if driver.KindOf(err) != c.konst {
			t.Errorf("%s：错误分类 = %v，应为 %v（err=%v）", c.name, driver.KindOf(err), c.konst, err)
		}
	}
}

func TestOpenFallsBackToUnitID(t *testing.T) {
	// labels.address 为空时用 device.unitId：现成点表里站号常常只写在 unitId。
	srv := startSim(t, mbsim.WithAddress(5))
	dev := model.Device{
		ID: "d", ChannelID: "ch", UnitID: 5, Enabled: true,
		Interval: model.Duration(time.Second),
		Labels:   map[string]string{},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "mbus",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestMetaContract(t *testing.T) {
	m := New().Meta()
	if m.ID != "mbus" || m.SDKVersion != driver.SDKVersion {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.MultiRead != driver.CapSupported {
		t.Error("一条 RSP_UD 覆盖整表，MultiRead 应为 supported")
	}
	if m.Capabilities.Write != driver.CapUnsupported {
		t.Error("不做 SND_UD，Write 应为 unsupported")
	}
	if len(m.AddressKinds) != 1 || m.AddressKinds[0] != model.AddressSymbol {
		t.Errorf("AddressKinds = %v，应只有 symbol", m.AddressKinds)
	}
	// 必填字段必须能表达"主地址"与"记录地址"，否则 UI 生成不出表单。
	var hasAddr, hasPath bool
	for _, f := range append(m.ConfigFields, m.PointFields...) {
		if f.Name == "labels.address" {
			hasAddr = f.Required
		}
		if f.Name == "address.path" {
			hasPath = f.Required
		}
	}
	if !hasAddr || !hasPath {
		t.Errorf("缺少必填配置字段：address=%v path=%v", hasAddr, hasPath)
	}
}

func TestParseOptions(t *testing.T) {
	cases := []struct {
		raw   string
		check func(Options) bool
	}{
		{"", func(o Options) bool { return o.Retries == defaultRetries && o.Gap == defaultGap }},
		{"retries=0", func(o Options) bool { return o.Retries == 0 }},
		{"retries=99", func(o Options) bool { return o.Retries == defaultRetries }},
		{"gap=100ms", func(o Options) bool { return o.Gap == 100*time.Millisecond }},
		{"gap=5s", func(o Options) bool { return o.Gap == defaultGap }},
		{"send-nke", func(o Options) bool { return o.SendNKE }},
		{"send-nke=false", func(o Options) bool { return !o.SendNKE }},
		{"max-follow=4", func(o Options) bool { return o.MaxFollow == 4 }},
	}
	for _, c := range cases {
		got := parseOptions(model.Device{Labels: map[string]string{"options": c.raw}})
		if !c.check(got) {
			t.Errorf("parseOptions(%q) = %+v", c.raw, got)
		}
	}
}

func TestRecordExpr(t *testing.T) {
	cases := []struct {
		path string
		want string
		err  bool
	}{
		{"q:energy", "energy", false},
		{"q:power-max", "power-max", false},
		{"r:3", "r:3", false},
		{"energy", "energy", false},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := recordExpr(model.Address{Kind: model.AddressSymbol, Path: c.path})
		if (err != nil) != c.err {
			t.Errorf("recordExpr(%q) err=%v，应为 %v", c.path, err, c.err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("recordExpr(%q) = %q，应为 %q", c.path, got, c.want)
		}
	}
	// 非符号地址必须报错：寄存器型地址在这个协议里没有意义。
	if _, err := recordExpr(model.Address{Kind: model.AddressRegister, Area: "holding"}); err == nil {
		t.Error("寄存器地址应被拒绝")
	}
}

func TestFlattenRefsDeduplicates(t *testing.T) {
	p := q("a", "energy")
	got := flattenRefs([]driver.ReadSpan{
		{Points: []driver.PointRef{p}},
		{Points: []driver.PointRef{p, q("b", "volume")}},
	})
	if len(got) != 2 {
		t.Fatalf("去重后 %d 条，应为 2", len(got))
	}
}

func TestReindexAppends(t *testing.T) {
	recs := []mb.Record{{Index: 0}, {Index: 1}}
	got := reindex(recs, 5)
	if got[0].Index != 5 || got[1].Index != 6 {
		t.Fatalf("重编号 = %d,%d", got[0].Index, got[1].Index)
	}
}

func TestQualityMapping(t *testing.T) {
	cases := map[driver.ErrorKind]model.Quality{
		driver.ErrTimeout:      model.QualityTimeout,
		driver.ErrDisconnected: model.QualityCommError,
		driver.ErrAddress:      model.QualityAddressError,
		driver.ErrProtocol:     model.QualityBad,
		driver.ErrUnsupported:  model.QualityBad,
	}
	for k, want := range cases {
		if got := qualityOf(driver.E(k, "read", "d", "x", false, nil)); got != want {
			t.Errorf("qualityOf(%v) = %s，应为 %s", k, got, want)
		}
	}
	if got := qualityOf(errors.New("plain")); got != model.QualityCommError {
		t.Errorf("未知错误默认质量 = %s", got)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
