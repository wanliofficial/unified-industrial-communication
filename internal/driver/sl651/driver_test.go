package sl651

import (
	"context"
	"testing"
	"time"

	sl651codec "uic/internal/codec/sl651"
	"uic/internal/driver"
	"uic/internal/model"
	sl651sim "uic/internal/sim/sl651"
)

// 这一组用例跑在「真 TCP + 内置遥测站」上，覆盖驱动与协议栈的整体行为：
// 建链/查询、整帧取要素、按要素寻址、错误分类、配置校验。分帧与要素
// 编解码的细节由 codec 单测钉住。

func startSim(t *testing.T, opts ...sl651sim.Option) *sl651sim.Server {
	t.Helper()
	srv, err := sl651sim.NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

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

func openConn(t *testing.T, srv *sl651sim.Server, options string) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "station", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(time.Second),
		Labels: map[string]string{
			"address": stationLabel(srv.Station()),
			"options": options,
		},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "sl651",
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

// pts 造一组按要素简称寻址的点位。
func pt(id, path string) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		Address:  model.Address{Kind: model.AddressSymbol, Path: path},
		DataType: model.TypeFloat64,
	}
}

func spans(points ...driver.PointRef) []driver.ReadSpan {
	return []driver.ReadSpan{{ID: "s", Points: points}}
}

func readOne(t *testing.T, c driver.Conn, path string) float64 {
	t.Helper()
	out, err := c.Read(context.Background(), spans(pt("p", path)))
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if len(out) != 1 {
		t.Fatalf("read %q: %d samples", path, len(out))
	}
	if out[0].Err != nil {
		t.Fatalf("read %q: %v", path, out[0].Err)
	}
	v, ok := out[0].Value.AsFloat()
	if !ok {
		t.Fatalf("read %q: 非数值 %+v", path, out[0].Value)
	}
	return v
}

// TestProbeAndQuery 建链后应能取到应答，且总线上只发一次 37H。
func TestProbeAndQuery(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := srv.QueryCount(sl651codec.FuncQueryAll); got != 1 {
		t.Fatalf("37H 次数 = %d，应为 1", got)
	}
}

// TestReadWholeFrame 一次 Read 覆盖多个点位：总线上只该有一次 37H。
func TestReadWholeFrame(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	paths := []string{"Z", "P1", "PT", "Q", "VT"}
	refs := make([]driver.PointRef, 0, len(paths))
	for _, p := range paths {
		refs = append(refs, pt(p, p))
	}
	before := srv.QueryCount(sl651codec.FuncQueryAll)
	out, err := c.Read(context.Background(), spans(refs...))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(refs) {
		t.Fatalf("样本数 = %d，应为 %d", len(out), len(refs))
	}
	if got := srv.QueryCount(sl651codec.FuncQueryAll) - before; got != 1 {
		t.Fatalf("37H 次数 = %d，应为 1（一条应答覆盖整帧）", got)
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

	// 工程值要与 sim 的 defaultFields 一一对应。
	want := map[string]float64{
		"Z":  0.127,
		"P1": 0.5,
		"PT": 123.4,
		"Q":  1234.567,
		"VT": 12.10,
	}
	for _, s := range out {
		v, _ := s.Value.AsFloat()
		if v != want[s.PointID] {
			t.Errorf("%s = %v，应为 %v", s.PointID, v, want[s.PointID])
		}
	}
}

// TestReadByHexPath 十六进制写法与简称等价。
func TestReadByHexPath(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	byName := readOne(t, c, "Z")
	byHex := readOne(t, c, "39")
	if byName != byHex {
		t.Fatalf("Z = %v，39 = %v，应相同", byName, byHex)
	}
}

// TestReadValuedAfterSimChange 从站改值后，下一次读要看到新值。
func TestReadValuedAfterSimChange(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	if got := readOne(t, c, "Z"); got != 0.127 {
		t.Fatalf("初始水位 = %v", got)
	}
	if err := srv.SetValue(sl651codec.GZ, 350); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if got := readOne(t, c, "Z"); got != 0.350 {
		t.Fatalf("改值后水位 = %v，应为 0.350", got)
	}
}

// TestReadSlot 批量要素的段号寻址：DRP 前 2 段有值、后 10 段缺测。
// 不给段号只能取第 0 段，或者把 12 段当一个 HEX 整数解出溢出值；这两种
// 都不是现场想要的「最近 1h 每 5min 雨量」。
func TestReadSlot(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	if got := readOne(t, c, "DRP"); got != 2.5 {
		t.Fatalf("DRP 第 0 段 = %v，want 2.5", got)
	}
	if got := readOne(t, c, "DRP[0]"); got != 2.5 {
		t.Fatalf("DRP[0] = %v，want 2.5", got)
	}
	if got := readOne(t, c, "DRP[1]"); got != 5 {
		t.Fatalf("DRP[1] = %v，want 5", got)
	}

	// 缺测段按 quality=bad 降级，不报地址错——规约允许该时段无数据。
	out, err := c.Read(context.Background(), spans(pt("drp11", "DRP[11]")))
	if err != nil {
		t.Fatalf("read DRP[11]: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("样本数 = %d", len(out))
	}
	if out[0].Quality != model.QualityBad {
		t.Errorf("DRP[11] 质量 = %s，应为 bad（缺测）", out[0].Quality)
	}

	// 越界段号：报地址错，不 panic。
	out, _ = c.Read(context.Background(), spans(pt("drp99", "DRP[99]")))
	if out[0].Quality != model.QualityAddressError {
		t.Errorf("DRP[99] 质量 = %s，应为 addressError", out[0].Quality)
	}
}

// TestReadSlotOnNonBatch 段号只对批量要素有意义，提前报地址错而不是让
// SlotNumber 返回「越界」。
func TestReadSlotOnNonBatch(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	out, err := c.Read(context.Background(), spans(pt("z1", "Z[1]")))
	if err != nil {
		t.Fatalf("read Z[1]: %v", err)
	}
	if out[0].Quality != model.QualityAddressError {
		t.Errorf("Z[1] 质量 = %s，应为 addressError", out[0].Quality)
	}
}

// TestReadUnknownGuidePoint 报里没有的要素：单点报地址错，不升级为链路错。
func TestReadUnknownGuidePoint(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	out, err := c.Read(context.Background(), spans(
		pt("z", "Z"),
		pt("nh4", "NH4N"), // sim 不上送这个要素
	))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("样本数 = %d", len(out))
	}
	if out[0].Quality != model.QualityGood {
		t.Errorf("Z 质量 = %s", out[0].Quality)
	}
	if out[1].Quality != model.QualityAddressError {
		t.Errorf("NH4N 质量 = %s，应为 addressError", out[1].Quality)
	}
}

// TestReadBadPath 非法要素名在解码阶段报地址错误。
func TestReadBadPath(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	out, err := c.Read(context.Background(), spans(pt("x", "不存在的要素")))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 1 || out[0].Quality != model.QualityAddressError {
		t.Fatalf("out = %+v", out)
	}
}

// TestWrongStationTimesOut 站号不符：从站静默，驱动必须报超时。
func TestWrongStationTimesOut(t *testing.T) {
	srv := startSim(t, sl651sim.WithStation([5]byte{0x00, 0x99, 0x88, 0x77, 0x66}))
	dev := model.Device{
		ID: "station", ChannelID: "ch", Enabled: true,
		Timeout: model.Duration(300 * time.Millisecond),
		Labels: map[string]string{
			"address": "31234567", // 与 sim 站号不同
			"options": "retries=0",
		},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "sl651",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	out, err := c.Read(context.Background(), spans(pt("z", "Z")))
	if err == nil {
		t.Fatal("站号不符时应超时")
	}
	if len(out) != 1 || out[0].Quality != model.QualityTimeout {
		t.Fatalf("out = %+v，应为 timeout", out)
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("错误分类 = %v，应为 timeout", driver.KindOf(err))
	}
}

// TestPasswordMismatchIsSilent 密码不符时从站静默（现场最常见的一种「连不上」）。
func TestPasswordMismatchIsSilent(t *testing.T) {
	srv := startSim(t, sl651sim.WithPassword([2]byte{0xAB, 0xCD}))
	c := openConn(t, srv, "retries=0,password=1234")

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := c.Probe(ctx); err == nil {
		t.Fatal("密码不符时应超时")
	}
}

// TestPasswordMatch 密码正确时应答正常。
func TestPasswordMatch(t *testing.T) {
	srv := startSim(t, sl651sim.WithPassword([2]byte{0xAB, 0xCD}))
	c := openConn(t, srv, "password=ABCD")

	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// TestBadCRCRetriedAndReported 校验码坏：可重试，最终报协议错误。
func TestBadCRCRetriedAndReported(t *testing.T) {
	srv := startSim(t)
	srv.SetBadCRC(true)
	c := openConn(t, srv, "retries=1,gap=1ms")

	_, err := c.Read(context.Background(), spans(pt("z", "Z")))
	if err == nil {
		t.Fatal("CRC 坏时应报错")
	}
	if k := driver.KindOf(err); k != driver.ErrProtocol && k != driver.ErrTimeout {
		t.Fatalf("错误分类 = %v", k)
	}
	// retries=1 意味着共发 2 次 37H。
	if got := srv.QueryCount(sl651codec.FuncQueryAll); got < 2 {
		t.Fatalf("37H 次数 = %d，应 >= 2（发生了重发）", got)
	}
}

// TestBadReportIsNotRetried 帧成了但报文解不开（正文被截断）：这属于「终端在
// 正常应答、只是我们理解错了」，规范要求**不重发**——重发同一请求只会拿到同样
// 的报文。它与 CRC 错（可重发）在错误种类上同为 ErrProtocol，若重发循环只看
// KindOf 就会一起重试到上限，白等 retries × 超时。
func TestBadReportIsNotRetried(t *testing.T) {
	srv := startSim(t)
	srv.SetTruncate(10) // 正文只剩 10 字节，连固定前缀都不够
	c := openConn(t, srv, "retries=2,gap=1ms")

	_, err := c.Read(context.Background(), spans(pt("z", "Z")))
	if err == nil {
		t.Fatal("正文截断应报错")
	}
	if k := driver.KindOf(err); k != driver.ErrProtocol {
		t.Fatalf("错误分类 = %v，应为 ErrProtocol", k)
	}
	if driver.IsRetryable(err) {
		t.Fatal("报文解不开必须标为不可重发")
	}
	// retries=2 时若误重发会发 3 次；正确行为是只发 1 次。
	if got := srv.QueryCount(sl651codec.FuncQueryAll); got != 1 {
		t.Fatalf("报文解不开不应重发（只发 1 次 37H），实际 %d 次", got)
	}
}

// TestSilentStationTimesOut 静默终端：报超时并按 qualityOf 映射质量。
func TestSilentStationTimesOut(t *testing.T) {
	srv := startSim(t)
	srv.SetSilent(true)
	c := openConn(t, srv, "retries=0")

	out, err := c.Read(context.Background(), spans(pt("z", "Z")))
	if err == nil {
		t.Fatal("静默时应超时")
	}
	if len(out) != 1 || out[0].Quality != model.QualityTimeout {
		t.Fatalf("out = %+v", out)
	}
}

// TestLinkErrCached 链路错误后，同一连接不再重试而是直接复用缓存的错误。
func TestLinkErrCached(t *testing.T) {
	srv := startSim(t)
	srv.SetSilent(true)
	c := openConn(t, srv, "retries=0")

	conn := c.(*conn)
	if _, err := c.Read(context.Background(), spans(pt("z", "Z"))); err == nil {
		t.Fatal("第一次读应失败")
	}
	if conn.linkErr == nil {
		t.Fatal("linkErr 应被缓存")
	}
	before := srv.Requests()
	if _, err := c.Read(context.Background(), spans(pt("z", "Z"))); err == nil {
		t.Fatal("第二次读应直接返回缓存错误")
	}
	if got := srv.Requests() - before; got != 0 {
		t.Fatalf("第二次读不应再发请求，实际发了 %d 次", got)
	}
}

// TestWriteUnsupported 写方向明确不支持。
func TestWriteUnsupported(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	out, err := c.Write(context.Background(), []driver.WriteRequest{{PointID: "z"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("结果数 = %d", len(out))
	}
	if out[0].Quality != model.QualityBad {
		t.Errorf("质量 = %s", out[0].Quality)
	}
	if driver.KindOf(out[0].Err) != driver.ErrUnsupported {
		t.Errorf("错误分类 = %v", driver.KindOf(out[0].Err))
	}
}

// TestProbeAfterClose 关闭后 Probe/Read 报断链。
func TestProbeAfterClose(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("probe after close = %v", err)
	}
	if _, err := c.Read(context.Background(), spans(pt("z", "Z"))); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("read after close = %v", err)
	}
}

// TestReadEmptySpans 没有点位时不发请求。
func TestReadEmptySpans(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv, "")

	before := srv.Requests()
	out, err := c.Read(context.Background(), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %d", len(out))
	}
	if got := srv.Requests() - before; got != 0 {
		t.Fatalf("不应发请求，实际 %d 次", got)
	}
}

// TestFlattenRefsDeduplicates 同一 ID 出现在多个 span 里只保留一次。
func TestFlattenRefsDeduplicates(t *testing.T) {
	a := pt("z", "Z")
	b := pt("p", "PT")
	got := flattenRefs([]driver.ReadSpan{
		{ID: "1", Points: []driver.PointRef{a, b}},
		{ID: "2", Points: []driver.PointRef{a}},
		{ID: "3", Points: []driver.PointRef{{ID: ""}}},
	})
	if len(got) != 2 {
		t.Fatalf("flattenRefs = %d，应为 2", len(got))
	}
}

// TestOpenRejectsBadConfig 打开时的配置校验。
func TestOpenRejectsBadConfig(t *testing.T) {
	base := model.Channel{
		ID: "ch", Protocol: "sl651",
		Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"},
		Timeout:   model.Duration(50 * time.Millisecond),
	}
	cases := []struct {
		name string
		dev  model.Device
	}{
		{"no address", model.Device{ID: "d"}},
		{"bad address", model.Device{ID: "d", Labels: map[string]string{"address": "abc"}}},
		{"too large", model.Device{ID: "d", Labels: map[string]string{"address": "99999999999"}}},
	}
	for _, c := range cases {
		if _, err := New().Open(context.Background(), driver.OpenConfig{
			Channel: base, Device: c.dev,
		}); err == nil {
			t.Errorf("%s: 应失败", c.name)
		}
	}
}

// TestOpenFallsBackToUnitID unitId 也能作为站号。
func TestOpenFallsBackToUnitID(t *testing.T) {
	dev := model.Device{ID: "d", UnitID: 31234567}
	addr, err := addressOf(dev)
	if err != nil {
		t.Fatalf("addressOf: %v", err)
	}
	if got := stationLabel(addr); got != "31234567" {
		t.Fatalf("站号 = %s", got)
	}
}

// TestMetaContract 能力声明与契约字段。
func TestMetaContract(t *testing.T) {
	m := New().Meta()
	if m.ID != "sl651" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.SDKVersion != driver.SDKVersion {
		t.Errorf("SDKVersion = %q", m.SDKVersion)
	}
	if len(m.Roles) != 1 || m.Roles[0] != driver.RoleMaster {
		t.Errorf("Roles = %v", m.Roles)
	}
	if m.Capabilities.Read != driver.CapSupported {
		t.Errorf("Read = %s", m.Capabilities.Read)
	}
	if m.Capabilities.Write != driver.CapUnsupported {
		t.Errorf("Write = %s", m.Capabilities.Write)
	}
	if m.Capabilities.MultiRead != driver.CapSupported {
		t.Errorf("MultiRead = %s", m.Capabilities.MultiRead)
	}
	if m.Capabilities.Browse != driver.CapUnsupported {
		t.Errorf("Browse = %s", m.Capabilities.Browse)
	}
	// 8 个能力位必须全部落在能力四态内，不能是空串（CapState 零值陷阱）。
	// 显式断言全字段，防止漏写 Discovery/BitAccess 后序列化成 ""。
	caps := map[string]driver.CapState{
		"Read": m.Capabilities.Read, "Write": m.Capabilities.Write,
		"Subscribe": m.Capabilities.Subscribe, "Browse": m.Capabilities.Browse,
		"Discovery": m.Capabilities.Discovery, "MultiRead": m.Capabilities.MultiRead,
		"BitAccess": m.Capabilities.BitAccess, "StructTypes": m.Capabilities.StructTypes,
	}
	for name, st := range caps {
		switch st {
		case driver.CapSupported, driver.CapUnsupported,
			driver.CapRuntimeDependent, driver.CapExperimental:
		default:
			t.Errorf("Capabilities.%s = %q，不是能力四态之一（漏写会序列化成空串）", name, st)
		}
	}
	if m.Description == "" {
		t.Error("Description 为空")
	}
}

// TestParseOptions 配置解析。
func TestParseOptions(t *testing.T) {
	dev := model.Device{Labels: map[string]string{
		"options": "retries=3,gap=50ms,center=5,password=ABCD",
	}}
	opt := parseOptions(dev)
	if opt.Retries != 3 {
		t.Errorf("Retries = %d", opt.Retries)
	}
	if opt.Gap != 50*time.Millisecond {
		t.Errorf("Gap = %v", opt.Gap)
	}
	if opt.CenterAddr != 5 {
		t.Errorf("CenterAddr = %d", opt.CenterAddr)
	}
	if opt.Password != [2]byte{0xAB, 0xCD} {
		t.Errorf("Password = % x", opt.Password)
	}

	// 非法值被忽略，保留默认。
	def := parseOptions(model.Device{})
	if def.Retries != defaultRetries || def.Gap != defaultGap {
		t.Errorf("默认值 = %+v", def)
	}
	if got := parseOptions(model.Device{Labels: map[string]string{"options": "retries=99,gap=9s"}}); got.Retries != defaultRetries || got.Gap != defaultGap {
		t.Errorf("越界值应被忽略，得到 %+v", got)
	}
}

// TestAddressOf BCD 站号编解码。
func TestAddressOf(t *testing.T) {
	cases := []struct {
		raw  string
		want [5]byte
	}{
		{"31234567", [5]byte{0x00, 0x31, 0x23, 0x45, 0x67}},
		{"1", [5]byte{0x00, 0x00, 0x00, 0x00, 0x01}},
		{"9999999999", [5]byte{0x99, 0x99, 0x99, 0x99, 0x99}},
		// 前导零不影响 BCD：地址按整数解析，00123456 就是 123456，
		// 左侧补零到 10 位后编码成 00 00 12 34 56。
		{"00123456", [5]byte{0x00, 0x00, 0x12, 0x34, 0x56}},
	}
	for _, c := range cases {
		got, err := addressOf(model.Device{ID: "d", Labels: map[string]string{"address": c.raw}})
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q = % x，应为 % x", c.raw, got, c.want)
		}
		// 反解必须回到原值（去掉前导零）。
		if back := bcdToInt(got); itoa(back) != itoa(mustAtoi(t, c.raw)) {
			t.Errorf("%q 反解 = %d", c.raw, back)
		}
	}
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("%q 不是数字", s)
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// TestQualityMapping 错误分类到质量的映射。
func TestQualityMapping(t *testing.T) {
	cases := []struct {
		kind driver.ErrorKind
		want model.Quality
	}{
		{driver.ErrTimeout, model.QualityTimeout},
		{driver.ErrDisconnected, model.QualityCommError},
		{driver.ErrAddress, model.QualityAddressError},
		{driver.ErrUnsupported, model.QualityBad},
		{driver.ErrPermission, model.QualityBad},
		{driver.ErrProtocol, model.QualityBad},
		{driver.ErrInternal, model.QualityCommError},
	}
	for _, c := range cases {
		err := driver.E(c.kind, "read", "d", "test", false, nil)
		if got := qualityOf(err); got != c.want {
			t.Errorf("qualityOf(%s) = %s，应为 %s", c.kind, got, c.want)
		}
	}
}

// TestGuideOf 点位地址解析（含批量要素段号）。
func TestGuideOf(t *testing.T) {
	cases := []struct {
		path  string
		guide byte
		slot  int
		ok    bool
	}{
		{"Z", sl651codec.GZ, 0, true},
		{"PT", sl651codec.GPT, 0, true},
		{"39", sl651codec.GZ, 0, true},
		{"1A", sl651codec.GP1, 0, true},
		{"DRP", sl651codec.GDRP, 0, true},
		{"DRP[3]", sl651codec.GDRP, 3, true},
		{"DRZ1[11]", 0xF5, 11, true},
		{"f4[0]", sl651codec.GDRP, 0, true},
		{"不存在", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		got, slot, err := guideOfWithSlot(model.Address{Kind: model.AddressSymbol, Path: c.path})
		if c.ok {
			if err != nil || got != c.guide || slot != c.slot {
				t.Errorf("guideOfWithSlot(%q) = (%02X, %d), %v，want (%02X, %d)",
					c.path, got, slot, err, c.guide, c.slot)
			}
		} else if err == nil {
			t.Errorf("guideOfWithSlot(%q) 应失败", c.path)
		}
	}
	// 段号写法错误与非符号地址。
	for _, bad := range []string{"Z[1]", "PT[-1]", "DRP[x]", "DRP[1", "Z[]"} {
		if _, _, err := guideOfWithSlot(model.Address{Kind: model.AddressSymbol, Path: bad}); err == nil {
			t.Errorf("guideOfWithSlot(%q) 应失败", bad)
		}
	}
	if _, _, err := guideOfWithSlot(model.Address{Kind: model.AddressRegister, Path: "Z"}); err == nil {
		t.Error("非符号地址应失败")
	}
}
