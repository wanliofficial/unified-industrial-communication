package ads

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	adscodec "uic/internal/codec/ads"
	"uic/internal/driver"
	"uic/internal/model"
	adssim "uic/internal/sim/ads"
)

// 驱动测试走真实 TCP 回环：AMS 帧、句柄、错误码都经过一次线格式，
// 这样“只在 mock 里对”的错误（字段顺序、长度、invoke）会被抓住。

func sym(path string, dt model.DataType) driver.PointRef {
	return driver.PointRef{ID: path, Address: model.Address{Kind: model.AddressSymbol, Path: path, Index: -1}, DataType: dt}
}

func symLen(path string, dt model.DataType, n int) driver.PointRef {
	p := sym(path, dt)
	p.Address.Length = n
	return p
}

func startSim(t *testing.T, opts ...adssim.Options) *adssim.Server {
	t.Helper()
	var o adssim.Options
	if len(opts) > 0 {
		o = opts[0]
	}
	srv, err := adssim.NewServerWithOptions("127.0.0.1:0", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func testConfig(srv *adssim.Server) driver.OpenConfig {
	return driver.OpenConfig{
		Channel: model.Channel{
			ID:       "ch-ads",
			Protocol: "ads",
			Transport: model.Transport{
				Kind:    "tcp",
				Address: srv.Addr(),
			},
			Timeout: model.Duration(2 * time.Second),
		},
		Device: model.Device{
			ID:        "plc",
			ChannelID: "ch-ads",
			Labels: map[string]string{
				"netid": srv.NetID().String(),
				"port":  strconv.Itoa(int(srv.Port())),
			},
		},
	}
}

func openTest(t *testing.T, srv *adssim.Server) *connState {
	t.Helper()
	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*connState)
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "ads" || m.SDKVersion != driver.SDKVersion {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("read/write caps = %+v", m.Capabilities)
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported || m.Capabilities.MultiRead != driver.CapUnsupported {
		t.Fatalf("subscribe/multiread caps = %+v", m.Capabilities)
	}
	if !m.Accepts(model.AddressSymbol) || m.Accepts(model.AddressRegister) {
		t.Fatal("address kinds")
	}
	if len(m.Transports) != 1 || m.Transports[0] != "tcp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	var hasNetID bool
	for _, f := range m.ConfigFields {
		if f.Name == "netid" && f.Required && f.Scope == "device" {
			hasNetID = true
		}
	}
	if !hasNetID {
		t.Fatal("netid config field is not required")
	}
}

func TestOpenValidation(t *testing.T) {
	srv := startSim(t)
	base := testConfig(srv)
	mutate := func(f func(*driver.OpenConfig)) driver.OpenConfig {
		cfg := base
		cfg.Device.Labels = map[string]string{}
		for k, v := range base.Device.Labels {
			cfg.Device.Labels[k] = v
		}
		f(&cfg)
		return cfg
	}
	cases := []struct {
		name string
		cfg  driver.OpenConfig
		want driver.ErrorKind
		msg  string
	}{
		{"udp transport", mutate(func(c *driver.OpenConfig) { c.Channel.Transport.Kind = "udp" }), driver.ErrUnsupported, "tcp transport"},
		{"empty address", mutate(func(c *driver.OpenConfig) { c.Channel.Transport.Address = " " }), driver.ErrUnsupported, "transport.address"},
		{"missing netid", mutate(func(c *driver.OpenConfig) { delete(c.Device.Labels, "netid") }), driver.ErrUnsupported, "labels.netid"},
		{"bad netid", mutate(func(c *driver.OpenConfig) { c.Device.Labels["netid"] = "1.2.3" }), driver.ErrUnsupported, "6 dot-separated"},
		{"bad port", mutate(func(c *driver.OpenConfig) { c.Device.Labels["port"] = "0" }), driver.ErrUnsupported, "labels.port"},
		{"bad local netid", mutate(func(c *driver.OpenConfig) { c.Device.Labels["localnetid"] = "x" }), driver.ErrUnsupported, "netid"},
		{"bad local port", mutate(func(c *driver.OpenConfig) { c.Device.Labels["localport"] = "70000" }), driver.ErrUnsupported, "localport"},
		{"retries low", mutate(func(c *driver.OpenConfig) { c.Device.Labels["options"] = "retries=-1" }), driver.ErrUnsupported, "0..10"},
		{"retries high", mutate(func(c *driver.OpenConfig) { c.Device.Labels["options"] = "retries=11" }), driver.ErrUnsupported, "0..10"},
		{"retries no value", mutate(func(c *driver.OpenConfig) { c.Device.Labels["options"] = "retries" }), driver.ErrUnsupported, "需要 =N"},
		{"strlen zero", mutate(func(c *driver.OpenConfig) { c.Device.Labels["options"] = "strlen=0" }), driver.ErrUnsupported, "1..65535"},
		{"unknown option", mutate(func(c *driver.OpenConfig) { c.Device.Labels["options"] = "foo=1" }), driver.ErrUnsupported, "未知的 options"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Open(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := driver.KindOf(err); got != tc.want {
				t.Fatalf("kind = %v, want %v (%v)", got, tc.want, err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("message %q does not contain %q", err, tc.msg)
			}
		})
	}
}

func TestProbeInfoState(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	ctx := context.Background()
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	info, err := c.DeviceInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "UIC ADS sim" || info.VersionString() != "3.1.4024" {
		t.Fatalf("info = %+v", info)
	}
	st, err := c.DeviceState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ADS != adscodec.StateRun {
		t.Fatalf("state = %v", st.ADS)
	}
	if err := c.WriteControl(ctx, adscodec.StateStop); err != nil {
		t.Fatal(err)
	}
	if srv.State() != adscodec.StateStop {
		t.Fatalf("sim state = %v", srv.State())
	}
	// 不支持的运行状态由设备拒绝，分类为 protocol。
	if err := c.WriteControl(ctx, adscodec.State(99)); err == nil ||
		driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("invalid control state: %v", err)
	}
}

func TestReadAllTypes(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	refs := []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
		sym("MAIN.nSetpoint", model.TypeInt16),
		sym("MAIN.fTemperature", model.TypeFloat32),
		sym("MAIN.fSpeed", model.TypeFloat64),
		sym("MAIN.bEnable", model.TypeBool),
		symLen("MAIN.sRecipe", model.TypeString, 81),
		sym("MAIN.arr[0]", model.TypeInt16),
		sym("MAIN.arr[2]", model.TypeInt16),
		symLen("MAIN.arrBytes", model.TypeBytes, 4),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(refs) {
		t.Fatalf("got %d samples", len(out))
	}
	want := map[string]float64{
		"MAIN.nCounter":     1234,
		"MAIN.nSetpoint":    500,
		"MAIN.fTemperature": 23.5,
		"MAIN.fSpeed":       1450.75,

		"MAIN.arr[0]": 10,
		"MAIN.arr[2]": 30,
	}
	for _, s := range out {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %s", s.PointID, s.Quality)
		}
		switch s.PointID {
		case "MAIN.sRecipe":
			if s.Value.Text != "batch-42" {
				t.Fatalf("recipe = %q", s.Value.Text)
			}
		case "MAIN.arrBytes":
			if s.Value.Text != string([]byte{1, 2, 3, 4}) {
				t.Fatalf("arrBytes = % x", []byte(s.Value.Text))
			}
		case "MAIN.bEnable":
			if !s.Value.Bool {
				t.Fatalf("bEnable = %+v", s.Value)
			}
		default:
			if s.Value.Num != want[s.PointID] {
				t.Fatalf("%s = %v, want %v", s.PointID, s.Value.Num, want[s.PointID])
			}
		}
	}
	if srv.Requests() == 0 {
		t.Fatal("sim saw no requests")
	}
}

func TestReadAddressErrorsDoNotFailRound(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	refs := []driver.PointRef{
		sym("MAIN.missing", model.TypeInt16),
		sym("MAIN.nCounter", model.TypeInt32), // 宽度不符 → invalid size
		sym("MAIN.nSetpoint", model.TypeInt16),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}})
	if err != nil {
		// 同一轮里既有地址错也有正常点：地址错不进入 firstErr。
		t.Fatalf("read returned %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("samples = %d", len(out))
	}
	if out[0].Err == nil || driver.KindOf(out[0].Err) != driver.ErrAddress {
		t.Fatalf("missing symbol err = %v", out[0].Err)
	}
	if out[1].Err == nil || driver.KindOf(out[1].Err) != driver.ErrAddress {
		t.Fatalf("size mismatch err = %v", out[1].Err)
	}
	if out[2].Err != nil || out[2].Value.Num != 500 {
		t.Fatalf("setpoint = %+v", out[2])
	}
}

func TestReadBCDRejected(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		sym("MAIN.nCounter", model.TypeBCD16),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
	if out[0].Err == nil || !strings.Contains(out[0].Err.Error(), "BCD") {
		t.Fatalf("sample err = %v", out[0].Err)
	}
}

func TestWrite(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	setpoint := sym("MAIN.nSetpoint", model.TypeInt16)
	speed := sym("MAIN.fSpeed", model.TypeFloat64)
	run := sym("MAIN.bRun", model.TypeBool)
	name := symLen("MAIN.sName", model.TypeString, 81)
	bare := symLen("MAIN.arrBytes", model.TypeBytes, 4)

	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "setpoint", Address: setpoint.Address, DataType: setpoint.DataType, Value: model.NumValue(model.TypeInt16, -321)},
		{PointID: "speed", Address: speed.Address, DataType: speed.DataType, Value: model.NumValue(model.TypeFloat64, 1234.5)},
		{PointID: "run", Address: run.Address, DataType: run.DataType, Value: model.BoolValue(true)},
		{PointID: "name", Address: name.Address, DataType: name.DataType, Value: model.TextValue(model.TypeString, "line-2")},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, r := range res {
		if !r.OK || r.Err != nil {
			t.Fatalf("%s: %+v", r.PointID, r)
		}
	}
	if raw, _ := srv.Get("MAIN.nSetpoint"); binary.LittleEndian.Uint16(raw) != uint16(0xFEBF) {
		t.Fatalf("setpoint raw = % x", raw)
	}
	if raw, _ := srv.Get("MAIN.fSpeed"); math.Float64frombits(binary.LittleEndian.Uint64(raw)) != 1234.5 {
		t.Fatalf("speed raw = % x", raw)
	}
	if raw, _ := srv.Get("MAIN.bRun"); raw[0] != 1 {
		t.Fatalf("run raw = % x", raw)
	}
	raw, _ := srv.Get("MAIN.sName")
	if got := strings.TrimRight(string(raw), "\x00"); got != "line-2" {
		t.Fatalf("name = %q", got)
	}

	// 只读 / 不存在 / 字节数组只读：逐点错误隔离，写入顺序与结果顺序一致。
	res, err = c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "counter", Address: sym("MAIN.nCounter", model.TypeInt16).Address, DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1)},
		{PointID: "ghost", Address: sym("MAIN.nope", model.TypeInt16).Address, DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1)},
		{PointID: "bare", Address: bare.Address, DataType: bare.DataType, Value: model.BytesValue([]byte{9, 8, 7, 6})},
		{PointID: "setpoint", Address: setpoint.Address, DataType: setpoint.DataType, Value: model.NumValue(model.TypeInt16, 42)},
	})
	if err != nil {
		t.Fatalf("write with per-point errors: %v", err)
	}
	wantKinds := []driver.ErrorKind{driver.ErrPermission, driver.ErrAddress, driver.ErrPermission, ""}
	for i, want := range wantKinds {
		if want == "" {
			if !res[i].OK {
				t.Fatalf("%s not ok: %+v", res[i].PointID, res[i])
			}
			continue
		}
		if res[i].OK || res[i].Err == nil || driver.KindOf(res[i].Err) != want {
			t.Fatalf("%s = %+v, want %v", res[i].PointID, res[i], want)
		}
	}
	if srv.Writes() != 5 {
		t.Fatalf("writes = %d, want 5", srv.Writes())
	}
}

func TestWriteValidation(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	cases := []struct {
		name string
		req  driver.WriteRequest
		msg  string
	}{
		{
			name: "out of range int16",
			req: driver.WriteRequest{PointID: "p", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
				DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 70000)},
			msg: "int16",
		},
		{
			name: "fractional int16",
			req: driver.WriteRequest{PointID: "p", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
				DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1.5)},
			msg: "整数",
		},
		{
			name: "string overflow",
			req: driver.WriteRequest{PointID: "p", Address: symLen("MAIN.sName", model.TypeString, 5).Address,
				DataType: model.TypeString, Value: model.TextValue(model.TypeString, "abcdef")},
			msg: "超过目标长度",
		},
		{
			name: "string no NUL room",
			req: driver.WriteRequest{PointID: "p", Address: symLen("MAIN.sName", model.TypeString, 5).Address,
				DataType: model.TypeString, Value: model.TextValue(model.TypeString, "abcde")},
			msg: "NUL",
		},
		{
			name: "bcd",
			req: driver.WriteRequest{PointID: "p", Address: sym("MAIN.nSetpoint", model.TypeBCD16).Address,
				DataType: model.TypeBCD16, Value: model.NumValue(model.TypeBCD16, 1)},
			msg: "BCD",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Write(context.Background(), []driver.WriteRequest{tc.req})
			if err != nil {
				t.Fatalf("unexpected link error: %v", err)
			}
			if res[0].OK || res[0].Err == nil {
				t.Fatalf("accepted: %+v", res[0])
			}
			if driver.KindOf(res[0].Err) != driver.ErrUnsupported {
				t.Fatalf("kind = %v", driver.KindOf(res[0].Err))
			}
			if !strings.Contains(res[0].Err.Error(), tc.msg) {
				t.Fatalf("message %q does not contain %q", res[0].Err, tc.msg)
			}
		})
	}
	// 不写进设备：所有拒绝都发生在发报文之前。
	if srv.Writes() != 0 {
		t.Fatalf("writes = %d", srv.Writes())
	}
}

func TestStaleHandleReResolves(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	ref := sym("MAIN.nCounter", model.TypeInt16)
	read := func() {
		t.Helper()
		out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{ref}}})
		if err != nil || out[0].Err != nil || out[0].Value.Num != 1234 {
			t.Fatalf("read: %v %+v", err, out[0])
		}
	}
	read() // 建立句柄缓存
	srv.InvalidateHandles()
	read() // 第一次按句柄读失败 → 重新解析 → 成功
}

func TestWrongPortAndNetID(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	cfg.Device.Labels["port"] = "852"
	_, err := New().Open(context.Background(), cfg)
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("wrong port: %v", err)
	}
	if !strings.Contains(err.Error(), "labels.port") {
		t.Fatalf("wrong port message = %v", err)
	}

	cfg = testConfig(srv)
	cfg.Device.Labels["netid"] = "127.0.0.2.1.1"
	_, err = New().Open(context.Background(), cfg)
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("wrong netid: %v", err)
	}
	if !strings.Contains(err.Error(), "target machine not found") {
		t.Fatalf("wrong netid message = %v", err)
	}
}

func TestLinkFailureStopsRound(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	_ = srv.Close()
	refs := []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
		sym("MAIN.nSetpoint", model.TypeInt16),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}})
	if err == nil {
		t.Fatal("read after close succeeded")
	}
	if len(out) != 2 || out[0].Err == nil || out[1].Err == nil {
		t.Fatalf("samples = %+v", out)
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err == nil {
			// 保持连接但不回任何报文，制造握手超时。
			time.Sleep(2 * time.Second)
			_ = nc.Close()
		}
	}()
	cfg := driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "ads",
			Transport: model.Transport{Kind: "tcp", Address: ln.Addr().String()},
			Timeout:   model.Duration(250 * time.Millisecond),
		},
		Device: model.Device{ID: "plc", ChannelID: "ch", Labels: map[string]string{
			"netid": "127.0.0.1.1.1", "options": "retries=0",
		}},
	}
	start := time.Now()
	_, err = New().Open(context.Background(), cfg)
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "AMS 握手超时") {
		t.Fatalf("message = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
}

func TestCloseAndReadAfterClose(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := c.Probe(context.Background()); err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("probe after close: %v", err)
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("read after close: %v", err)
	}
	if len(out) != 1 || out[0].Err == nil {
		t.Fatalf("samples = %+v", out)
	}
	if _, err := c.DeviceState(context.Background()); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("state after close: %v", err)
	}
	if err := c.WriteControl(context.Background(), adscodec.StateRun); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("control after close: %v", err)
	}
}

func TestSymbolNameRejects(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	bad := []driver.PointRef{
		{ID: "reg", Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 1}, DataType: model.TypeInt16},
		{ID: "sel", Address: model.Address{Kind: model.AddressSymbol, Path: "MAIN.x", Selector: "a"}, DataType: model.TypeInt16},
		{ID: "empty", Address: model.Address{Kind: model.AddressSymbol, Path: " "}, DataType: model.TypeInt16},
		{ID: "index", Address: model.Address{Kind: model.AddressSymbol, Path: "MAIN.arr", Index: 2}, DataType: model.TypeInt16},
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: bad}})
	if err != nil {
		t.Fatalf("address errors should not fail the round: %v", err)
	}
	for _, s := range out {
		if s.Err == nil {
			t.Fatalf("%s accepted", s.PointID)
		}
		if driver.KindOf(s.Err) != driver.ErrAddress && driver.KindOf(s.Err) != driver.ErrUnsupported {
			t.Fatalf("%s kind = %v", s.PointID, driver.KindOf(s.Err))
		}
	}
}
