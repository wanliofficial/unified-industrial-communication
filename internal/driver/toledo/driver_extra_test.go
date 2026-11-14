package toledo

import (
	"context"
	"net"
	"testing"
	"time"

	toledocodec "uic/internal/codec/toledo"
	"uic/internal/driver"
	"uic/internal/model"
	toledosim "uic/internal/sim/toledo"
)

// 本文件补齐驱动层「配置校正」与「故障区分」两类用例。
//
// 为什么单独放一个文件：driver_test.go 覆盖的是「连上真 TCP 后读数对不对」，
// 而这里覆盖的是不靠正常读数才能暴露的行为——配置夹取的不变式、以及「没收到
// 字节」与「收到字节但解不出」必须分成两个错误类（这是规格点名的一条）。

// TestReadTimeoutAlwaysBelowStaleAfter 读超时校正后必须严格小于陈旧阈值。
//
// 原实现先对半再抬 1 秒下限，当 staleAfter ≤ 2s 时会让两者相等，把「必须小于」
// 这条不变式破坏掉。这里对边界值逐个钉住。
func TestReadTimeoutAlwaysBelowStaleAfter(t *testing.T) {
	cases := []struct {
		staleAfter string
	}{
		{"100ms"},
		{"1s"},
		{"1500ms"},
		{"2s"},
		{"2500ms"},
		{"10s"},
		{"30s"},
	}
	for _, c := range cases {
		t.Run(c.staleAfter, func(t *testing.T) {
			srv := startSim(t)
			dev := model.Device{
				ID: "scale", ChannelID: "ch", Enabled: true,
				Interval: model.Duration(5 * time.Second),
				Timeout:  model.Duration(time.Second),
				// readTimeout 故意配得比 staleAfter 大，触发校正。
				Labels: map[string]string{
					"options": "staleAfter=" + c.staleAfter + ",readTimeout=1h",
				},
				Points: []model.Point{{
					ID:      "w",
					Address: model.Address{Kind: model.AddressSymbol, Path: "weight"},
					Enabled: true,
				}},
			}
			cc, err := openRaw(context.Background(), srv, dev)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer cc.Close()

			inner := cc.(*conn)
			if inner.readTimeout <= 0 {
				t.Fatalf("readTimeout = %v，必须为正", inner.readTimeout)
			}
			if inner.readTimeout >= inner.staleAfter {
				t.Fatalf("readTimeout(%v) 必须小于 staleAfter(%v)：否则断链会被 stale 掩盖",
					inner.readTimeout, inner.staleAfter)
			}
		})
	}
}

// TestParseOptionsIgnoresBadValues 越界/非法值一律忽略并保留默认，不阻止建链。
func TestParseOptionsIgnoresBadValues(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  Options
		noval bool
	}{
		{name: "正常两项", raw: "staleAfter=45s,readTimeout=5s",
			want: Options{StaleAfter: 45 * time.Second, ReadTimeout: 5 * time.Second}},
		{name: "未知键忽略", raw: "foo=bar,staleAfter=20s",
			want: Options{StaleAfter: 20 * time.Second}},
		{name: "非时长忽略", raw: "staleAfter=abc,readTimeout=xyz", noval: true},
		{name: "零与负值忽略", raw: "staleAfter=0s,readTimeout=-3s", noval: true},
		{name: "缺等号忽略", raw: "staleAfter,readTimeout", noval: true},
		{name: "空串", raw: "", noval: true},
		{name: "键大小写不敏感", raw: "STALEAFTER=15s, ReadTimeout = 4s",
			want: Options{StaleAfter: 15 * time.Second, ReadTimeout: 4 * time.Second}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseOptions(model.Device{Labels: map[string]string{"options": c.raw}})
			if c.noval {
				if got.StaleAfter != 0 || got.ReadTimeout != 0 {
					t.Fatalf("非法值应全部忽略，得到 %+v", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("parseOptions = %+v，期望 %+v", got, c.want)
			}
		})
	}
}

// TestReadUnderloadDegradesToBad 欠量程（UL）与超量程同款：重量 bad、描述性字段 good。
func TestReadUnderloadDegradesToBad(t *testing.T) {
	srv := startSim(t, toledosim.WithReading(toledocodec.Reading{
		Status: toledocodec.StatusUnderload,
		Mode:   toledocodec.ModeGross,
		Sign:   1,
		Weight: -2.0,
		Unit:   "kg",
	}))
	c := openConn(t, srv,
		pt("w", "weight", model.TypeFloat64),
		pt("status", "status", model.TypeString),
	)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, _ := c.Read(context.Background(), spans(
		pt("w", "weight", model.TypeFloat64),
		pt("status", "status", model.TypeString),
	))
	if s, ok := sampleByID(out, "w"); !ok || s.Quality != model.QualityBad {
		t.Errorf("UL 时重量应为 bad，得到 %+v", s)
	}
	if s, ok := sampleByID(out, "status"); !ok || s.Quality != model.QualityGood {
		t.Errorf("UL 时 status 应仍为 good（描述性字段不降级），得到 %+v", s)
	}
}

// TestSilentScaleReportsCommError 连着但不推帧 → comm_error，而不是看起来正常。
func TestSilentScaleReportsCommError(t *testing.T) {
	srv := startSim(t, toledosim.WithSilent())
	c := openConn(t, srv, pt("w", "weight", model.TypeFloat64))

	out, err := c.Read(context.Background(), spans(pt("w", "weight", model.TypeFloat64)))
	if err != nil {
		t.Fatalf("从未收到帧时 Read 不应直接报错（应逐点降级）：%v", err)
	}
	s, ok := sampleByID(out, "w")
	if !ok {
		t.Fatal("缺少 weight 样本")
	}
	if s.Quality != model.QualityCommError {
		t.Errorf("静默衡器的质量码 = %s，应为 comm_error", s.Quality)
	}
}

// TestBinaryFramesReportProtocolError 收到字节但一帧都解不出 → ErrProtocol。
//
// 这是规格点名的一条：仪表被设成二进制 TOLEDO（STX 开头 18 字节）时，若报成
// 超时，现场会先去改波特率而不是查仪表的帧格式设置。这里用一台只吐二进制帧的
// 假秤把这条判据钉死。
func TestBinaryFramesReportProtocolError(t *testing.T) {
	addr := startBinaryScale(t)
	c := openConnAt(t, addr, pt("w", "weight", model.TypeFloat64))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Read 本身不做网络交互（推送模型），因此第一轮可能还没读到字节——那时
	// 报 comm_error 是对的。要断言的是「字节已经进来、却一帧都解不出」之后
	// 的分类，所以这里等到坏帧计数上来再看。
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err = c.Read(ctx, spans(pt("w", "weight", model.TypeFloat64)))
		if err != nil {
			break
		}
		if c.(*conn).Stats().BadFrames > 0 {
			_, err = c.Read(ctx, spans(pt("w", "weight", model.TypeFloat64)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("只收到二进制帧时应报错，而不是把点位当成「还没收到」")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("错误分类 = %s，应为 protocol（提示改帧格式而不是改波特率）", driver.KindOf(err))
	}
	if msg := err.Error(); !contains(msg, "ASCII") {
		t.Errorf("错误消息应给出「切到 ASCII 格式」的可执行指引，得到：%s", msg)
	}
}

// TestStatsReportsDiagnostics Stats 要能把「字节数/坏帧/原始帧」报出来。
func TestStatsReportsDiagnostics(t *testing.T) {
	srv := startSim(t, toledosim.WithWeight(149.0, "kg", 1))
	c := openConn(t, srv, pt("w", "weight", model.TypeFloat64))
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	st := c.(*conn).Stats()
	if !st.HasFrame {
		t.Fatal("收到帧后 HasFrame 应为真")
	}
	if st.BytesSeen <= 0 {
		t.Errorf("BytesSeen = %d，应记录读到的字节数", st.BytesSeen)
	}
	if st.BadFrames != 0 {
		t.Errorf("正常帧不应计入坏帧，BadFrames = %d", st.BadFrames)
	}
	if !contains(st.Raw, "kg") {
		t.Errorf("Stats.Raw 应是最近一帧原文，得到 %q", st.Raw)
	}
	if st.FrameAge < 0 {
		t.Errorf("FrameAge = %v", st.FrameAge)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// openRaw 用完整的 device 配置建链（需要自定义 labels.options 时用）。
func openRaw(ctx context.Context, srv *toledosim.Server, dev model.Device) (driver.Conn, error) {
	return New().Open(ctx, driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "toledo",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second),
		},
		Device: dev,
	})
}

// openConnAt 连到任意地址（用于自建假秤）。
func openConnAt(t *testing.T, addr string, points ...driver.PointRef) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "scale", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(5 * time.Second),
		Timeout:  model.Duration(time.Second),
		Labels:   map[string]string{},
	}
	for _, p := range points {
		dev.Points = append(dev.Points, model.Point{
			ID: p.ID, Address: p.Address, DataType: p.DataType, Enabled: true,
		})
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "toledo",
			Transport: model.Transport{Kind: "tcp", Address: addr},
			Timeout:   model.Duration(time.Second),
		},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// startBinaryScale 起一台只会吐「二进制 TOLEDO 帧」的假秤。
//
// 它模拟的是现场最常见的一种误配：仪表被设成 STX 开头、18 字节、带状态字与
// 校验和的二进制连续格式，而驱动只实现 ASCII 三段式。字节流里**有数据**，
// 但一帧都解析不出来。
func startBinaryScale(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// STX + 状态字 A/B/C + 6 位重量 + 6 位皮重 + CR + 校验和。
				frame := []byte{0x02, 'S', 'T', ' ', '0', '0', '1', '4', '9', '0', '0', '0', '0', '0', '0', '\r', 0x00}
				for {
					if _, err := c.Write(frame); err != nil {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// contains 是 strings.Contains 的本地别名，避免为一条断言引入 import。
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
