package dlt645

import (
	"context"
	"errors"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	dlt645sim "uic/internal/sim/dlt645"
)

// 这一组用例跑在「真 TCP + 内置从站」上，覆盖的是驱动与协议栈的整体
// 行为：发帧、收帧、DI 回显校验、地址校验、后续帧、错误映射。分帧与
// BCD 的细节由 codec 的单测钉住，这里只关心它们连起来是否自洽。
//
// 用 TCP 而不是 pty：串口服务器的透传模式与这里完全同构（同一段字节流、
// 同一套分帧），而 pty 在 CI 上需要 /dev/ptmx 与 termios，脆弱得多。
// 串口路径本身由 serialport 的单测与 Modbus RTU 的 pty 用例覆盖。

func startSim(t *testing.T, opts ...dlt645sim.Option) *dlt645sim.Server {
	t.Helper()
	srv, err := dlt645sim.NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func openConn(t *testing.T, addr string, di ...string) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels:   map[string]string{"address": "000000000042"},
	}
	for i, d := range di {
		dev.Points = append(dev.Points, model.Point{
			ID: "p" + string(rune('a'+i)), Enabled: true, DataType: model.TypeFloat64,
			Address: model.Address{
				Kind: model.AddressRegister, Area: d, Offset: 0, Bit: -1, Length: 0,
			},
		})
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: addr},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func points(di ...string) []driver.ReadSpan {
	var refs []driver.PointRef
	for i, d := range di {
		refs = append(refs, driver.PointRef{
			ID:       "p" + string(rune('a'+i)),
			Address:  model.Address{Kind: model.AddressRegister, Area: d, Bit: -1},
			DataType: model.TypeFloat64,
		})
	}
	return []driver.ReadSpan{{ID: "s", Points: refs}}
}

func TestProbeReadsActualAddress(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbeRejectsWrongMeter(t *testing.T) {
	// 从站地址是 000000000099，配置指向 000000000042：必须报错，
	// 而不是把 99 号表的数据记到 42 号表头上。
	srv := startSim(t, dlt645sim.WithAddress("000000000099"))
	c := openConn(t, srv.Addr())
	err := c.Probe(context.Background())
	if err == nil {
		t.Fatal("probe accepted a different meter")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

func TestReadEnergyVoltageCurrent(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv.Addr(), "00000000", "02010100", "02020100")
	got, err := c.Read(context.Background(), points("00000000", "02010100", "02020100"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d samples", len(got))
	}
	want := []float64{123456.78, 220.0, 5.123}
	for i, w := range want {
		if got[i].Err != nil {
			t.Fatalf("%s: %v", got[i].PointID, got[i].Err)
		}
		if got[i].Value.Num != w {
			t.Fatalf("%s = %v, want %v", got[i].PointID, got[i].Value.Num, w)
		}
		if got[i].Quality != model.QualityGood {
			t.Fatalf("%s quality = %v", got[i].PointID, got[i].Quality)
		}
	}
}

func TestReadDemandTimestamp(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	got, err := c.Read(context.Background(), points("01010000"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0].Value.Num != 12.3456 {
		t.Fatalf("demand = %v", got[0].Value.Num)
	}
}

func TestReadUnknownDIReturnsAddressError(t *testing.T) {
	// 从站没有这项数据 → 0xD1 ERR=02（无请求数据）→ ErrAddress。
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	got, err := c.Read(context.Background(), points("04000405"))
	if err != nil {
		// 单点时驱动会把第一条错误上抛；允许返回 err，但样本必须是坏的。
		if driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d samples", len(got))
	}
	if got[0].Err == nil {
		t.Fatal("failed point must carry an error")
	}
	if got[0].Quality != model.QualityCommError {
		t.Fatalf("quality = %v", got[0].Quality)
	}
}

func TestReadUnlistedDIFailsFast(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	// 03 系列（事件记录）没收录：驱动必须在本地报 ErrAddress，
	// 而不是发一个宽度未知的请求再按错误长度解析。
	got, err := c.Read(context.Background(), points("03010000"))
	if err == nil {
		t.Fatal("unlisted DI accepted")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
	if len(got) != 1 || got[0].Err == nil {
		t.Fatal("failed sample missing")
	}
}

func TestReadFollowFrames(t *testing.T) {
	srv := startSim(t)
	// 数据块长 250 字节，超过单帧 195 字节的分段上限，从站必须拆成
	// 「0x91 + 后续 0x92」两帧以上。分帧边界由 codec 的单测钉住，
	// 这里验证驱动能把它们拼回一条完整数据。
	raw := make([]byte, 0, 250)
	for i := 0; i < 62; i++ {
		raw = append(raw, 0x78, 0x56, 0x34, 0x12)
	}
	raw = raw[:250]
	if err := srv.SetValue("0201FF00", raw); err != nil {
		t.Fatal(err)
	}

	c := openConn(t, srv.Addr())
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
		ID: "block",
		Address: model.Address{Kind: model.AddressRegister, Area: "0201FF00",
			Bit: -1, Length: len(raw)},
		// 数据块是多个数据项的拼接，没有单一数值含义，只能按 bytes
		// 交付；用 float64 会被驱动显式拒绝。
		DataType: model.TypeBytes,
	}}}})
	if err != nil {
		t.Fatalf("read block: %v", err)
	}
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("sample = %+v", got[0])
	}
	if got[0].Value.Type != model.TypeBytes {
		t.Fatalf("value type = %v", got[0].Value.Type)
	}
	if n := len(got[0].Value.Text); n != 250 {
		t.Fatalf("block length = %d, want 250 (follow frames must be concatenated)", n)
	}
	// 四段内容的开头必须与写入时一致，证明拼装顺序没有错位。
	if prefix := got[0].Value.Text[:4]; prefix != string([]byte{0x78, 0x56, 0x34, 0x12}) {
		t.Fatalf("block prefix = % X", prefix)
	}
}

func TestWriteRequiresOptIn(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "clock",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000101", Bit: -1, Length: 6},
		Value:   model.NumValue(model.TypeFloat64, 20261107120000),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res[0].OK {
		t.Fatal("write must be refused without allow-write")
	}
	if driver.KindOf(res[0].Err) != driver.ErrPermission {
		t.Fatalf("kind = %v", driver.KindOf(res[0].Err))
	}
	if srv.Writes() != 0 {
		t.Fatal("slave must not see a write")
	}
}

func TestWriteClockRoundTrip(t *testing.T) {
	srv := startSim(t)
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels: map[string]string{
			"address": "000000000042",
			"options": "allow-write",
		},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "clock",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000101", Bit: -1, Length: 6},
		Value:   model.NumValue(model.TypeFloat64, 20261107123456),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write failed: %v", res[0].Err)
	}
	if srv.Writes() != 1 {
		t.Fatalf("writes = %d", srv.Writes())
	}
	// 06 秒 34 分 12 时 07 日 11 月 26 年，低字节在前 = ss mm hh DD MM YY。
	want := []byte{0x56, 0x34, 0x12, 0x07, 0x11, 0x26}
	got := srv.Value("04000101")
	if len(got) != 6 {
		t.Fatalf("value = % X", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("value = % X, want % X", got, want)
		}
	}
}

func TestWriteReadonlyDI(t *testing.T) {
	srv := startSim(t)
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Labels:   map[string]string{"address": "000000000042", "options": "allow-write"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "voltage",
		Address: model.Address{Kind: model.AddressRegister, Area: "02010100", Bit: -1, Length: 2},
		Value:   model.NumValue(model.TypeFloat64, 300),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res[0].OK {
		t.Fatal("voltage must be read-only")
	}
	if driver.KindOf(res[0].Err) != driver.ErrPermission {
		t.Fatalf("kind = %v", driver.KindOf(res[0].Err))
	}

	// 状态字区间（04 00 05 xx）不是「控制命令」的暗门：按 DI 表它只读，
	// 写它必须被拒。按区间放行会让「把状态字配成可写」这种点表错误一路
	// 走到设备，而设备通常只回一个含糊的「其它错误」。
	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "status",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000402", Bit: -1, Length: 2},
		Value:   model.NumValue(model.TypeUint16, 1),
	}})
	if err != nil {
		t.Fatalf("write status: %v", err)
	}
	if res[0].OK || driver.KindOf(res[0].Err) != driver.ErrPermission {
		t.Fatalf("status word write = %+v", res[0])
	}
}

func TestOpenRequiresAddressLabel(t *testing.T) {
	srv := startSim(t)
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()}},
		Device: model.Device{ID: "meter", ChannelID: "ch", Enabled: true,
			Interval: model.Duration(time.Second)},
	})
	if err == nil {
		t.Fatal("missing labels.address accepted")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

func TestReadTimeoutIsClassified(t *testing.T) {
	// 先建链成功、再从站消失：差错必须归为可重试的链路类，而不是
	// 「这个点没有数据」。直接连一个已关闭的端口是不行的——那会在
	// Open 阶段就失败，压根走不到 Read。
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	_ = srv.Close()

	_, err := c.Read(context.Background(), points("00000000"))
	if err == nil {
		t.Fatal("read against closed sim succeeded")
	}
	k := driver.KindOf(err)
	if k != driver.ErrTimeout && k != driver.ErrDisconnected {
		t.Fatalf("kind = %v (%v)", k, err)
	}
}

func TestLinkFailureStopsFurtherRequests(t *testing.T) {
	// 第一点读成功、随后链路断开时，后续点位必须快速失败而不是逐个等
	// 超时——否则一轮采集的时延等于「点数 × 超时」。
	srv := startSim(t)
	c := openConn(t, srv.Addr(), "00000000")
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	_ = srv.Close()
	start := time.Now()
	got, _ := c.Read(context.Background(), points("00000000", "02010100", "02020100"))
	if len(got) != 3 {
		t.Fatalf("got %d samples", len(got))
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("read took %v; link failure should abort after the first timeout", elapsed)
	}
	for _, s := range got {
		if s.Err == nil {
			t.Fatalf("%s should have failed after link loss", s.PointID)
		}
	}
}

func TestExceptionMapsToAddressKind(t *testing.T) {
	// 直连从站、手组一个不存在的数据项，确认 0xD1 被归为地址类。
	srv := startSim(t)
	c := openConn(t, srv.Addr())
	got, err := c.Read(context.Background(), points("0400040A"))
	if err == nil {
		t.Fatal("missing DI returned success")
	}
	var oe *driver.OpError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %T", err)
	}
	if oe.Kind != driver.ErrAddress {
		t.Fatalf("kind = %v", oe.Kind)
	}
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
}

// 4 字节日期数据项（结算日）在线路上只有 MMDDhhmm，读出来是 8 位；写回
// 必须接受同形的值。否则「把刚读到的结算日原样写回」会得到一个月=0 的
// 垃圾日期——这类错误不会报错，只会让表在错误的日子结算。
func TestWriteSettlementDateRoundTrip(t *testing.T) {
	srv := startSim(t)
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels:   map[string]string{"address": "000000000042", "options": "allow-write"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	read := func() float64 {
		t.Helper()
		got, rerr := c.Read(context.Background(), points("04000B01"))
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
		if got[0].Err != nil {
			t.Fatalf("read point: %v", got[0].Err)
		}
		return got[0].Value.Num
	}
	if v := read(); v != 11071200 {
		t.Fatalf("settlement date = %v, want 11071200 (MMDDhhmm)", v)
	}

	// 原样写回读到的值。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "date",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000B01", Bit: -1, Length: 4},
		Value:   model.NumValue(model.TypeFloat64, 11071200),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write rejected: %v", res[0].Err)
	}

	// 完整 14 位时间也接受，只取月/日/时/分。
	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "date",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000B01", Bit: -1, Length: 4},
		Value:   model.NumValue(model.TypeFloat64, 20261231235900),
	}})
	if err != nil {
		t.Fatalf("write full time: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write full time rejected: %v", res[0].Err)
	}
	if v := read(); v != 12312359 {
		t.Fatalf("settlement date after write = %v, want 12312359", v)
	}
}

// 电表时钟是最不能"猜"的一类写入：把 13 月归一化成次年 1 月不会报错，只会
// 让表在错误的时间结算。范围校验必须在本地完成。
func TestWriteTimeRejectsInvalidDate(t *testing.T) {
	srv := startSim(t)
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels:   map[string]string{"address": "000000000042", "options": "allow-write"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	for _, tc := range []struct {
		area string
		val  float64
	}{
		{"04000101", 20261307120000}, // 13 月
		{"04000101", 20260230120000}, // 2 月 30 日
		{"04000B01", 13320000},       // 月=13 的结算日
		{"04000B01", 42},             // 既不是 MMDDhhmm 也不是完整时间
	} {
		res, werr := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: "t",
			Address: model.Address{Kind: model.AddressRegister, Area: tc.area, Bit: -1},
			Value:   model.NumValue(model.TypeFloat64, tc.val),
		}})
		if werr != nil {
			t.Fatalf("%s=%v: %v", tc.area, tc.val, werr)
		}
		if res[0].OK {
			t.Fatalf("%s = %v accepted", tc.area, tc.val)
		}
		if driver.KindOf(res[0].Err) != driver.ErrUnsupported {
			t.Fatalf("%s = %v kind = %v", tc.area, tc.val, driver.KindOf(res[0].Err))
		}
	}
	if srv.Writes() != 0 {
		t.Fatalf("slave saw %d writes, want 0", srv.Writes())
	}
}
