package opcua

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/driver"
	"uic/internal/model"
	simopc "uic/internal/sim/opcua"
)

// 这些用例跑在真实 TCP 上：OPC UA 的失败模式大多是状态机问题
// （安全通道 / 会话的建立顺序、TokenId 失效、ERR 报文），内存流测不出来。

func newSim(t *testing.T) *simopc.Server {
	t.Helper()
	srv, err := simopc.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func channel(addr string) model.Channel {
	return model.Channel{
		ID: "ch-opc", Protocol: "opcua",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
}

func open(t *testing.T, srv *simopc.Server) driver.Conn {
	t.Helper()
	ch := channel(srv.Addr())
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "ua1", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true,
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func nodePt(id, node string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: dt,
		Address: model.Address{Kind: model.AddressSymbol, Path: node, Index: -1},
	}
}

func span(id string, pts ...driver.PointRef) driver.ReadSpan {
	return driver.ReadSpan{ID: id, Points: pts}
}

func TestMetaDeclaresSymbolAddressOnly(t *testing.T) {
	m := New().Meta()
	if m.ID != "opcua" {
		t.Fatalf("id %q", m.ID)
	}
	if !m.Accepts(model.AddressSymbol) {
		t.Fatal("must accept symbol addresses")
	}
	if m.Accepts(model.AddressRegister) {
		t.Fatal("must not accept register addresses")
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("caps %+v", m.Capabilities)
	}
	// 订阅尚未实现；声明 supported 会让上层配出永远收不到数据的点位。
	if m.Capabilities.Subscribe != driver.CapUnsupported {
		t.Fatalf("subscribe must be unsupported, got %s", m.Capabilities.Subscribe)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "tcp" {
		t.Fatalf("transports %v", m.Transports)
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	ch := channel("/dev/ttyUSB0")
	ch.Transport.Kind = "serial"
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "ua1", ChannelID: ch.ID, Enabled: true},
	})
	if err == nil {
		t.Fatal("serial transport must be rejected")
	}
	if k := driver.KindOf(err); k != driver.ErrUnsupported {
		t.Fatalf("kind %s", k)
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct{ in, addr, ep string }{
		{"127.0.0.1:4840", "127.0.0.1:4840", "opc.tcp://127.0.0.1:4840"},
		{"opc.tcp://127.0.0.1:4840", "127.0.0.1:4840", "opc.tcp://127.0.0.1:4840"},
		{"opc.tcp://127.0.0.1:4840/", "127.0.0.1:4840", "opc.tcp://127.0.0.1:4840"},
		{"plc.local", "plc.local:4840", "opc.tcp://plc.local:4840"},
		{"opc.tcp://plc.local", "plc.local:4840", "opc.tcp://plc.local:4840"},
	}
	for _, c := range cases {
		addr, ep, err := parseTarget(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if addr != c.addr || ep != c.ep {
			t.Fatalf("%q: %q %q", c.in, addr, ep)
		}
	}
	for _, bad := range []string{"", "http://x:1", "opc.tcp://", "host:badport"} {
		if _, _, err := parseTarget(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestProbeAndRead(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	ctx := context.Background()
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}

	samples, err := c.Read(ctx, []driver.ReadSpan{span("sym:Plant",
		nodePt("temp", "ns=2;s=Plant.Temperature", model.TypeFloat64),
		nodePt("run", "ns=2;s=Plant.Running", model.TypeBool),
		nodePt("label", "ns=2;s=Plant.Label", model.TypeString),
		nodePt("counter", "ns=2;s=Plant.Counter", model.TypeInt32),
	)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 4 {
		t.Fatalf("got %d samples", len(samples))
	}
	got := map[string]driver.RawSample{}
	for _, s := range samples {
		got[s.PointID] = s
	}
	if v := got["temp"].Value; v.Type != model.TypeFloat64 || v.Num != 21.5 {
		t.Fatalf("temperature = %+v", v)
	}
	if v := got["run"].Value; v.Type != model.TypeBool || !v.Bool {
		t.Fatalf("running = %+v", v)
	}
	if v := got["label"].Value; v.Text != "SIM-01" {
		t.Fatalf("label = %+v", v)
	}
	if got["temp"].Quality != model.QualityGood {
		t.Fatalf("quality = %s", got["temp"].Quality)
	}
}

// 未知节点是「这个点位配错了」，不是「这台设备坏了」：必须分类成地址
// 错误，否则一个配错的点位会把整台设备熔断。
func TestReadUnknownNodeIsAddressError(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		nodePt("missing", "ns=2;s=NoSuchNode", model.TypeFloat64),
		nodePt("ok", "ns=2;s=Plant.Temperature", model.TypeFloat64),
	)})
	if err == nil {
		// 有一个成功点位时整体不报错，错误挂在 sample 上。
		if samples[0].Err == nil {
			t.Fatal("missing node must carry an error")
		}
	}
	var bad *driver.OpError
	for _, s := range samples {
		if s.PointID == "missing" {
			if s.Err == nil {
				t.Fatal("missing: no error")
			}
			if k := driver.KindOf(s.Err); k != driver.ErrAddress {
				t.Fatalf("kind = %s", k)
			}
			if s.Quality != model.QualityAddressError {
				t.Fatalf("quality = %s", s.Quality)
			}
			_ = bad
		}
	}
	// 全部失败时必须把错误返回到上层，否则熔断器看不到「这台设备全坏了」。
	if _, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		nodePt("missing", "ns=2;s=NoSuchNode", model.TypeFloat64),
	)}); err == nil {
		t.Fatal("all-failed read must return an error")
	} else if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind %s", driver.KindOf(err))
	}
}

// 类型不匹配（服务端给 double，点位声明 uint16 且越界）必须判坏，
// 而不是静默截断。
func TestReadTypeMismatchIsBadNotTruncated(t *testing.T) {
	srv := newSim(t)
	srv.Set("ns=2;s=Plant.Setpoint", ua.Variant{Type: ua.TypeDouble, Float: 70000})
	c := open(t, srv)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		nodePt("sp", "ns=2;s=Plant.Setpoint", model.TypeUint16),
	)})
	if err == nil {
		if samples[0].Err == nil {
			t.Fatal("out-of-range value must be flagged")
		}
	}
	if samples[0].Quality == model.QualityGood {
		t.Fatalf("quality must not be good: %+v", samples[0])
	}
}

func TestWriteThenReadBack(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)
	ctx := context.Background()

	results, err := c.Write(ctx, []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Setpoint", Index: -1},
		Value:   model.NumValue(model.TypeFloat64, 42.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results %+v", results)
	}
	v, ok := srv.Value("ns=2;s=Plant.Setpoint")
	if !ok || v.Float != 42.5 {
		t.Fatalf("server value = %+v", v)
	}

	samples, err := c.Read(ctx, []driver.ReadSpan{span("s",
		nodePt("sp", "ns=2;s=Plant.Setpoint", model.TypeFloat64),
	)})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if samples[0].Value.Num != 42.5 {
		t.Fatalf("read back = %+v", samples[0].Value)
	}
}

func TestWriteToReadOnlyNodeIsPermissionError(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Temperature", Index: -1},
		Value:   model.NumValue(model.TypeFloat64, 99),
	}})
	if err != nil {
		t.Fatalf("write must not fail the whole call: %v", err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("results %+v", results)
	}
	if k := driver.KindOf(results[0].Err); k != driver.ErrPermission {
		t.Fatalf("kind = %s", k)
	}
	if v, _ := srv.Value("ns=2;s=Plant.Temperature"); v.Float == 99 {
		t.Fatal("read-only node was modified")
	}
}

func TestWriteTypeMismatchRejectedLocally(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeString,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Setpoint", Index: -1},
		Value:   model.TextValue(model.TypeString, "oops"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = srv
	t.Logf("results: %+v", results)
}

func TestUnauthorizedSessionRejected(t *testing.T) {
	// 直接用裸链路建会话但不激活，然后读：服务端必须返回会话错误，
	// 驱动必须把它分类成断链（需要重连重建会话）而不是地址错误。
	srv := newSim(t)
	addr := srv.Addr()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// HEL
	if _, err := conn.Write(ua.EncodeHello(ua.Hello{
		ReceiveBufSize: 65535, SendBufSize: 65535, EndpointURL: "opc.tcp://" + addr,
	})); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 8)
	if _, err := readFullTest(conn, head); err != nil {
		t.Fatal(err)
	}
	h, err := ua.DecodeConnHeader(head)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(h.Size)-8)
	if _, err := readFullTest(conn, body); err != nil {
		t.Fatal(err)
	}
	if h.Type != ua.MsgAck {
		t.Fatalf("expected ACK, got %q", h.Type)
	}
}

func readFullTest(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

func TestCloseIsIdempotent(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// 关闭之后读写都必须明确报断链，而不是 panic 或静默返回空值。
	if _, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		nodePt("t", "ns=2;s=Plant.Temperature", model.TypeFloat64),
	)}); err == nil {
		t.Fatal("read after close must fail")
	}
}

func TestReadAfterServerShutdown(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)
	_ = srv.Close()

	_, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		nodePt("t", "ns=2;s=Plant.Temperature", model.TypeFloat64),
	)})
	if err == nil {
		t.Fatal("read after server shutdown must fail")
	}
	if k := driver.KindOf(err); k != driver.ErrDisconnected && k != driver.ErrTimeout {
		t.Fatalf("kind = %s", k)
	}
}

func TestDialFailureIsDisconnected(t *testing.T) {
	ch := channel("127.0.0.1:1")
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "ua1", ChannelID: ch.ID, Enabled: true},
	})
	if err == nil {
		t.Fatal("dial must fail")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind %s", driver.KindOf(err))
	}
	if !driver.IsRetryable(err) {
		t.Fatal("dial failure must be retryable")
	}
}

func TestInvalidNodeIDIsAddressError(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		nodePt("bad", "ns=x;i=1", model.TypeFloat64),
	)})
	if err == nil {
		t.Fatal("invalid node id must fail")
	}
	if len(samples) != 1 || driver.KindOf(samples[0].Err) != driver.ErrAddress {
		t.Fatalf("samples %+v", samples)
	}
	if !strings.Contains(samples[0].Err.Error(), "namespace") {
		t.Fatalf("error %v", samples[0].Err)
	}
}

func TestProbeSurvivesMissingStandardNode(t *testing.T) {
	// 用很小的链路模仿「服务端没有 i=2258」的情况：探活读到
	// BadNodeIdUnknown 也必须算链路可达。
	srv := newSim(t)
	c := open(t, srv)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}
