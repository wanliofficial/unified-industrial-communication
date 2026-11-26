package dlt645

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	dlt645codec "uic/internal/codec/dlt645"
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

// openConnTimeout 与 openConn 同构，但允许把单次事务超时压短：
// 「链路失败」类用例必须等满一个超时窗口，用默认 2s 会让用例变慢。
func openConnTimeout(t *testing.T, addr string, timeout time.Duration) driver.Conn {
	t.Helper()
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Labels:   map[string]string{"address": "000000000042"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: addr},
			Timeout:   model.Duration(timeout)},
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

// TestReadShortAddress 缩位地址（高位补 AA）端到端：从站应答必须带实际地
// 址（标准 §5.2.2），主站按「请求里的 AA 是通配」匹配，不能把实际地址误判成
// 「另一块表在应答」。
func TestReadShortAddress(t *testing.T) {
	srv := startSim(t)
	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels:   map[string]string{"address": "42"},
		Points: []model.Point{{
			ID: "v", Enabled: true, DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressRegister, Area: "02010100", Bit: -1},
		}},
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

	got, err := c.Read(context.Background(), points("02010100"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("sample = %+v", got)
	}
	if got[0].Value.Num != 220.0 {
		t.Fatalf("voltage = %v", got[0].Value.Num)
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
	// 质量码必须与错误种类一致：这是「地址错」（点表要改），不是「通信错」
	// （线路要查）。把地址错标成 comm_error 会让运维去查一根没问题的线。
	if driver.KindOf(got[0].Err) != driver.ErrAddress {
		t.Fatalf("kind = %v", driver.KindOf(got[0].Err))
	}
	if got[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %v, want address_error", got[0].Quality)
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

// TestWriteQualityFollowsErrorKind 钉住写失败的质量码同样由错误种类派生。
//
// 早先 writeOne 的每条失败路径都不设 Quality，序列化出去是空串——既不是
// good 也不是 bad，下游按「可用」判断会把它当成一次成功的写入。现在与读
// 路径共用 qualityOf：权限错 → bad（要改开关/点表），地址错 → address_error
// （要改 DI），两者都不能冒充 comm_error。
func TestWriteQualityFollowsErrorKind(t *testing.T) {
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

	cases := []struct {
		name    string
		area    string
		value   model.Value
		wantKnd driver.ErrorKind
		wantQ   model.Quality
	}{
		{
			name: "只读数据项", area: "02010100",
			value:   model.NumValue(model.TypeFloat64, 220.0),
			wantKnd: driver.ErrPermission, wantQ: model.QualityBad,
		},
		{
			name: "未收录的 DI", area: "03010000",
			value:   model.NumValue(model.TypeFloat64, 1),
			wantKnd: driver.ErrAddress, wantQ: model.QualityAddressError,
		},
	}
	for _, c1 := range cases {
		res, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: c1.name,
			Address: model.Address{Kind: model.AddressRegister, Area: c1.area, Bit: -1, Length: 2},
			Value:   c1.value,
		}})
		if err != nil {
			t.Fatalf("%s: write: %v", c1.name, err)
		}
		if res[0].OK {
			t.Fatalf("%s: 不应成功", c1.name)
		}
		if got := driver.KindOf(res[0].Err); got != c1.wantKnd {
			t.Fatalf("%s: kind = %v, want %v (%v)", c1.name, got, c1.wantKnd, res[0].Err)
		}
		if res[0].Quality != c1.wantQ {
			t.Fatalf("%s: quality = %q, want %q", c1.name, res[0].Quality, c1.wantQ)
		}
		if res[0].Quality == "" {
			t.Fatalf("%s: 质量码为空——序列化后既不是 good 也不是 bad", c1.name)
		}
	}
	if srv.Writes() != 0 {
		t.Fatal("被本地闸门拒绝的写入不得到达设备")
	}
}

// TestAdapterEchoIsSkipped 钉住「适配器回显要被跳过、而不是当成应答」。
//
// 部分 USB 转 485 / 两线半双工适配器会把主站刚发出的字节原样回显给接收端，
// 于是主站先收到自己的请求帧、然后才是真正的应答。请求帧的 D7=0，任何从站
// 应答的 D7=1，因此这一帧是可无条件识别的。
//
// 两种错误处理都会造成现场事故：把回显当应答 → 写完报成功但设备没收到；
// 把回显当协议错 → 这类适配器完全不可用。正确行为是丢掉回显、继续等。
//
// 这里用一个「先回显、再转发」的 TCP 代理模拟这类适配器，跑真实驱动。
func TestAdapterEchoIsSkipped(t *testing.T) {
	srv := startSim(t)

	// 代理：把客户端发来的每个字节先原样回给客户端，再转给从站；
	// 从站的应答原样转回客户端。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(client net.Conn) {
				defer client.Close()
				up, derr := net.Dial("tcp", srv.Addr())
				if derr != nil {
					return
				}
				defer up.Close()
				// 客户端 → 从站：先把字节回显给客户端，再转发。
				go func() {
					buf := make([]byte, 512)
					for {
						n, rerr := client.Read(buf)
						if n > 0 {
							_, _ = client.Write(buf[:n]) // 回显
							_, _ = up.Write(buf[:n])
						}
						if rerr != nil {
							return
						}
					}
				}()
				// 从站 → 客户端。
				buf := make([]byte, 512)
				for {
					n, rerr := up.Read(buf)
					if n > 0 {
						_, _ = client.Write(buf[:n])
					}
					if rerr != nil {
						return
					}
				}
			}(client)
		}
	}()

	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels:   map[string]string{"address": "000000000042", "options": "allow-write,gap=1ms"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: ln.Addr().String()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 读：必须拿到真实电压 220.0，而不是「回显 DI + 0 字节」。
	got, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
		ID: "v", Address: model.Address{Kind: model.AddressRegister, Area: "02010100", Bit: -1},
		DataType: model.TypeFloat64,
	}}}})
	if err != nil {
		t.Fatalf("read through echoing adapter: %v", err)
	}
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("read sample = %+v", got)
	}
	if f, ok := got[0].Value.AsFloat(); !ok || f != 220.0 {
		t.Fatalf("value = %v (want 220.0)", got[0].Value)
	}

	// 写：必须真的写到设备（Writes()==1），而不是把回显当成功。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "clock",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000101", Bit: -1, Length: 6},
		Value:   model.NumValue(model.TypeFloat64, 20261107120000),
	}})
	if err != nil {
		t.Fatalf("write through echoing adapter: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write = %+v", res[0])
	}
	if srv.Writes() != 1 {
		t.Fatalf("设备收到 %d 次写；回显被当成应答时会是 0", srv.Writes())
	}
}

// TestAllowControlIsRejected 钉住「死掉的高危开关要被显式拒绝」。
//
// allow-control 曾被文档写成「拉合闸/清零的闸门」，但驱动从来没有控制入口，
// 也没有任何代码读这个字段。它因此是一个死开关：运维写了会以为已经授权拉闸，
// 实际什么也没打开；而将来若有人补上控制入口，一个陈旧配置会**默认**已经放行。
//
// 正确行为是 Open 直接报错，明确告诉配置者「这条路本驱动不通」，而不是静默忽略。
func TestAllowControlIsRejected(t *testing.T) {
	srv := startSim(t)
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: model.Device{ID: "meter", ChannelID: "ch", Enabled: true,
			Interval: model.Duration(time.Second),
			Labels: map[string]string{"address": "000000000042",
				"options": "allow-write,allow-control"}},
	})
	if err == nil {
		t.Fatal("allow-control 被静默接受——用户会以为拉闸已授权")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "allow-control") {
		t.Fatalf("错误信息必须点名 allow-control：%v", err)
	}
	if !strings.Contains(err.Error(), "厂商工具") {
		t.Fatalf("错误信息必须给出出路：%v", err)
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

	// 原样写回读到的值。Decode 现在返回 MMDDhhmm（11 月 7 日 12:00 → 11071200），
	// 不是旧实现的小端带符号整数。
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

	// 读到的非法时钟必须报错，而不是被归一化成另一个合法日期。写路径早已
	// 拒绝这类值（TestWriteTimeRejectsInvalidDate），读路径若静默接受，运维
	// 就会看到一个「看起来正常、实际是 time.Date 猜出来的」结算日。
	if err := srv.SetValue("04000B01", []byte{0x19, 0x07, 0x12, 0x00}); err != nil {
		t.Fatalf("set bad settlement date: %v", err)
	}
	got, rerr := c.Read(context.Background(), points("04000B01"))
	if rerr == nil {
		t.Fatalf("月=19 的结算日被静默接受：%+v", got)
	}
	if !strings.Contains(rerr.Error(), "月份") {
		t.Fatalf("错误信息应指出是月份越界：%v", rerr)
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
		{"04000B01", 2301200},        // 2 月 30 日 12:00（MMDDhhmm）
		{"04000B01", 4310000},        // 4 月 31 日 00:00（4 月只有 30 天）
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

// allowWrite 在测试里打开写闸门。真实的权限配置来自设备标签 options；
// 这几条用例关心的是失败时的质量码与批级上抛，不是闸门本身（后者由
// TestWriteRequiresOptIn 覆盖）。
func allowWrite(t *testing.T, c driver.Conn) {
	t.Helper()
	cc, ok := c.(*conn)
	if !ok {
		t.Fatalf("unexpected conn type %T", c)
	}
	cc.opt.AllowWrite = true
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

// TestProbeExceptionIsNotRetryable 表收到读通信地址命令却回异常应答时，重发
// 结果不会变：探测必须按不可重试上报，否则上层会一轮轮白等超时。
func TestProbeExceptionIsNotRetryable(t *testing.T) {
	addr, err := dlt645codec.ParseAddr("000000000042")
	if err != nil {
		t.Fatal(err)
	}
	// 异常应答：控制码 = 0x13|0x80|0x40，数据域 1 字节错误信息字。
	frame, err := dlt645codec.Build(addr, dlt645codec.CtrlReadAddr|dlt645codec.BitReply|dlt645codec.BitError,
		[]byte{dlt645codec.ErrNoData})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := &conn{
		link:    &scriptedLink{resp: func([]byte) []byte { return frame }},
		addr:    addr,
		devID:   "meter",
		timeout: time.Second,
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

// TestReadBadDIDoesNotBreakRound 一个表不支持的数据项只应让那一个点位变坏：
// 同轮其它数据项照常读、不重连、不熔断。
func TestReadBadDIDoesNotBreakRound(t *testing.T) {
	srv := startSim(t, dlt645sim.WithoutValue("02010100"))
	c := openConn(t, srv.Addr(), "00000000", "02010100", "02020100")
	got, err := c.Read(context.Background(), points("00000000", "02010100", "02020100"))
	if err != nil {
		t.Fatalf("partial success must not return an error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3", len(got))
	}
	if got[0].Quality != model.QualityGood || got[2].Quality != model.QualityGood {
		t.Fatalf("好点被连坐: %s=%s %s=%s", got[0].PointID, got[0].Quality, got[2].PointID, got[2].Quality)
	}
	if got[1].Quality != model.QualityAddressError {
		t.Fatalf("坏 DI 质量 = %s, want address_error", got[1].Quality)
	}
	if driver.KindOf(got[1].Err) != driver.ErrAddress {
		t.Fatalf("坏 DI 类别 = %v", driver.KindOf(got[1].Err))
	}
}

// TestReadAllFailedReturnsError 全轮都失败时必须上抛第一个错误：否则调度器
// 看不见故障，设备会一直以「成功但有坏点」的状态挂着。
func TestReadAllFailedReturnsError(t *testing.T) {
	srv := startSim(t, dlt645sim.WithoutValue("02010100"), dlt645sim.WithoutValue("02020100"))
	c := openConn(t, srv.Addr(), "02010100", "02020100")
	got, err := c.Read(context.Background(), points("02010100", "02020100"))
	if err == nil {
		t.Fatal("all-failed round returned success")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	for _, s := range got {
		if s.Err == nil || s.Quality != model.QualityAddressError {
			t.Fatalf("%s: err=%v quality=%s", s.PointID, s.Err, s.Quality)
		}
	}
}

// TestReadLinkFailureKeepsGoodSamples 一轮读途中对端掉线时，已读到的样本必须
// 保留、剩余点位显式标坏，并且立即返回链路错误（有成功点也返回）。
func TestReadLinkFailureKeepsGoodSamples(t *testing.T) {
	srv := startSim(t, dlt645sim.WithDropAfter(1))
	// 从站在第一条应答发出后立即断开：读的第一个 DI 必然成功，第二个开始
	// 只能看到 EOF/超时——这正是「一轮采集中途断链」的现场形状。
	c := openConnTimeout(t, srv.Addr(), 300*time.Millisecond)
	start := time.Now()
	got, err := c.Read(context.Background(), points("00000000", "02010100", "02020100"))
	if err == nil {
		t.Fatal("link failure did not surface")
	}
	if !driver.IsLinkFailure(err) {
		t.Fatalf("kind = %v, want link failure", driver.KindOf(err))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("read took %v; link failure should abort the round", elapsed)
	}
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3", len(got))
	}
	if got[0].Quality != model.QualityGood {
		t.Fatalf("第一个成功点被丢了: quality=%s", got[0].Quality)
	}
	for _, s := range got[1:] {
		if s.Err == nil || s.Quality == model.QualityGood {
			t.Fatalf("%s: err=%v quality=%s", s.PointID, s.Err, s.Quality)
		}
		if driver.KindOf(s.Err) != driver.KindOf(err) {
			t.Fatalf("%s kind = %v, batch err = %v", s.PointID, driver.KindOf(s.Err), driver.KindOf(err))
		}
	}
}

// TestReadCanceledContextMarksRemaining ctx 已到期时不该发任何帧，但每个点
// 都要有结果——少给样本会被采集器当成驱动漏报。
func TestReadCanceledContextMarksRemaining(t *testing.T) {
	srv := startSim(t)
	c := openConn(t, srv.Addr(), "00000000", "02010100")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := c.Read(ctx, points("00000000", "02010100"))
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v, want timeout", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	for _, s := range got {
		if s.Quality != model.QualityTimeout || s.Err == nil {
			t.Fatalf("%s: quality=%s err=%v", s.PointID, s.Quality, s.Err)
		}
	}
	if srv.Requests() != 0 {
		t.Fatalf("driver sent %d requests after ctx was done", srv.Requests())
	}
}

// TestWriteLinkFailureStopsRemaining 写链路中断时剩余写命令不再发送，已成功
// 的那条保持 OK，批级错误经第二个返回值上抛。
func TestWriteLinkFailureStopsRemaining(t *testing.T) {
	srv := startSim(t, dlt645sim.WithDropAfter(1))
	c := openConnTimeout(t, srv.Addr(), 300*time.Millisecond)
	reqs := []driver.WriteRequest{
		{PointID: "clock", Address: model.Address{Kind: model.AddressRegister, Area: "04000101", Bit: -1},
			Value: model.NumValue(model.TypeFloat64, 20270102030405)},
		{PointID: "date", Address: model.Address{Kind: model.AddressRegister, Area: "04000B01", Bit: -1},
			Value: model.NumValue(model.TypeFloat64, 12312359)},
	}
	allowWrite(t, c)
	res, err := c.Write(context.Background(), reqs)
	if err == nil {
		t.Fatal("link failure did not surface")
	}
	if !driver.IsLinkFailure(err) {
		t.Fatalf("kind = %v, want link failure", driver.KindOf(err))
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if !res[0].OK {
		t.Fatalf("first write should be kept: %v", res[0].Err)
	}
	if res[1].OK || res[1].Err == nil || res[1].Quality == model.QualityGood {
		t.Fatalf("remaining write not marked bad: %+v", res[1])
	}
	if srv.Writes() != 1 {
		t.Fatalf("slave saw %d writes, want 1", srv.Writes())
	}
}

// TestWriteTimeoutCarriesQuality 写超时时必须带 timeout 质量码，而不是让
// 上层兜底成 bad——「设备慢」与「参数不对」在下游要能区分。
func TestWriteTimeoutCarriesQuality(t *testing.T) {
	srv := startSim(t, dlt645sim.WithAddress("000000000099"))
	c := openConnTimeout(t, srv.Addr(), 300*time.Millisecond)
	allowWrite(t, c)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "clock",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000101", Bit: -1},
		Value:   model.NumValue(model.TypeFloat64, 20270102030405),
	}})
	if err == nil {
		t.Fatal("write to another meter succeeded")
	}
	if len(res) != 1 {
		t.Fatalf("got %d results", len(res))
	}
	if res[0].Quality != model.QualityTimeout {
		t.Fatalf("quality = %s, want timeout", res[0].Quality)
	}
	if driver.KindOf(res[0].Err) != driver.ErrTimeout {
		t.Fatalf("kind = %v", driver.KindOf(res[0].Err))
	}
}

// 写通信地址成功后必须回填连接内的地址：表已经改名，后续事务还发旧地址
// 会整连接超时（回归：曾表现为「写成功、下一轮读全超时」）。回执带新地址
// （表先改名再应答）也要接受，不能误判成「总线上有另一台表」。
func TestWriteCommAddressBackfillsConnAddr(t *testing.T) {
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

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "addr",
		Address: model.Address{Kind: model.AddressRegister, Area: "04000401", Bit: -1, Length: 6},
		Value:   model.TextValue(model.TypeString, "000000000099"),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write failed: %v", res[0].Err)
	}
	// 连接内地址必须跟着表走（线上序：低字节在前）。
	mc, ok := c.(*conn)
	if !ok {
		t.Fatalf("conn 类型 = %T", c)
	}
	want, aerr := dlt645codec.ParseAddr("000000000099")
	if aerr != nil {
		t.Fatalf("ParseAddr: %v", aerr)
	}
	if mc.addr != want {
		t.Fatalf("回填地址 = %s, want %s",
			dlt645codec.FormatAddr(mc.addr), dlt645codec.FormatAddr(want))
	}
	// 后续事务用新地址：读通信地址应该命中从站（旧地址会让从站静默超时）。
	samples, rerr := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s",
		Points: []driver.PointRef{{
			ID:       "addr",
			Address:  model.Address{Kind: model.AddressRegister, Area: "04000401", Bit: -1, Length: 6},
			DataType: model.TypeFloat64,
		}},
	}})
	if rerr != nil {
		t.Fatalf("read after rename: %v", rerr)
	}
	if samples[0].Quality != model.QualityGood {
		t.Fatalf("read quality = %s, err = %v", samples[0].Quality, samples[0].Err)
	}
}
