package knx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	knxsim "uic/internal/sim/knx"
	"uic/internal/transport"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

func testChannel(addr string) model.Channel {
	return model.Channel{
		ID: "ch", Protocol: "knx",
		Transport: model.Transport{Kind: "udp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
}

func newSim(t *testing.T) *knxsim.Server {
	t.Helper()
	srv, err := knxsim.NewServerWithOptions("127.0.0.1:0", knxsim.Options{DisableAutoIncrement: true})
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// openSim 打开一个连到内置网关的驱动连接。
func openSim(t *testing.T, srv *knxsim.Server, mut func(*model.Device)) *conn {
	t.Helper()
	dev := model.Device{ID: "gateway", ChannelID: "ch"}
	if mut != nil {
		mut(&dev)
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// ref 构造一个 KNX 点位视图：组地址 + DPT + 平台类型。
func ref(id, group, dpt string, t model.DataType) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		Address:  model.Address{Kind: model.AddressSymbol, Path: group, DPT: dpt},
		DataType: t,
	}
}

func readOne(t *testing.T, c driver.Conn, r driver.PointRef) driver.RawSample {
	t.Helper()
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{r}}})
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	return samples[0]
}

// ---------------------------------------------------------------------------
// Meta / 配置
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "knx" {
		t.Fatalf("id = %q", m.ID)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "udp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("read/write = %v/%v", m.Capabilities.Read, m.Capabilities.Write)
	}
	// 订阅必须明确不支持：它是一条独立的生命周期，别让人以为可以用。
	if m.Capabilities.Subscribe != driver.CapUnsupported {
		t.Fatalf("subscribe = %v", m.Capabilities.Subscribe)
	}
	if len(m.AddressKinds) != 1 || m.AddressKinds[0] != model.AddressSymbol {
		t.Fatalf("addressKinds = %v", m.AddressKinds)
	}
	if !m.Accepts(model.AddressSymbol) {
		t.Fatal("symbol addressing must be accepted")
	}
	if m.Accepts(model.AddressRegister) {
		t.Fatal("register addressing must be rejected")
	}
	// DPT 是必填字段：点表层/控制台都要能看到它。
	var sawDPT bool
	for _, f := range m.PointFields {
		if f.Name == "address.dpt" {
			sawDPT = true
			if !f.Required {
				t.Fatal("address.dpt must be required")
			}
		}
	}
	if !sawDPT {
		t.Fatal("point field address.dpt missing")
	}
}

func TestOpenRejectsNonUDP(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "knx",
			Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:3671"}},
		Device: model.Device{ID: "d"},
	})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAppendsDefaultPort(t *testing.T) {
	var got string
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		got = addr
		return nopDatagram{}, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "knx",
			Transport: model.Transport{Kind: "udp", Address: "10.0.0.5"}},
		Device: model.Device{ID: "d"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "10.0.0.5:3671" {
		t.Fatalf("addr = %q", got)
	}
}

func TestOpenRejectsBadSettings(t *testing.T) {
	cases := []struct {
		name  string
		label map[string]string
		want  string
	}{
		{"heartbeat too long", map[string]string{"heartbeat": "90s"}, "heartbeat"},
		{"heartbeat zero", map[string]string{"heartbeat": "0s"}, "heartbeat"},
		{"heartbeat garbage", map[string]string{"heartbeat": "soon"}, "heartbeat"},
		{"ack too short", map[string]string{"ackTimeout": "10ms"}, "ackTimeout"},
		{"ack exceeds timeout", map[string]string{"ackTimeout": "5s"}, "device.timeout"},
		{"retries too many", map[string]string{"maxRetries": "9"}, "maxRetries"},
		{"retries garbage", map[string]string{"maxRetries": "many"}, "maxRetries"},
		{"bad source", map[string]string{"sourceAddress": "gateway"}, "sourceAddress"},
		{"zero source", map[string]string{"sourceAddress": "0.0.0"}, "sourceAddress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
				return nopDatagram{}, nil
			})
			_, err := d.Open(context.Background(), driver.OpenConfig{
				Channel: testChannel("127.0.0.1:3671"),
				Device:  model.Device{ID: "d", Labels: tc.label},
			})
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// 缺少 DPT 是最该拦住的一种配置：组地址不携带类型，猜出来的类型
// 不会报错，只会把温度读成 3.7e-41。
func TestOpenRejectsPointWithoutDPT(t *testing.T) {
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return nopDatagram{}, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("127.0.0.1:3671"),
		Device: model.Device{ID: "d", Points: []model.Point{{
			ID: "p1", Enabled: true,
			Address:  model.Address{Kind: model.AddressSymbol, Path: "1/2/3"},
			DataType: model.TypeFloat32,
		}}},
	})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenRejectsDPTTypeMismatch(t *testing.T) {
	// 9.001 是 2 字节浮点，却声明成 bool：必须报错，而不是把 21.5 塞进布尔。
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return nopDatagram{}, nil
	})
	_, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: testChannel("127.0.0.1:3671"),
		Device: model.Device{ID: "d", Points: []model.Point{{
			ID: "p1", Enabled: true,
			Address:  model.Address{Kind: model.AddressSymbol, Path: "1/2/3", DPT: "9.001"},
			DataType: model.TypeBool,
		}}},
	})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanPointRejectsForeignAddressFields(t *testing.T) {
	cases := []struct {
		name string
		addr model.Address
		want string
	}{
		{"selector", model.Address{Kind: model.AddressSymbol, Path: "1/2/3", DPT: "9.001", Selector: "temp"}, "selector"},
		{"index", model.Address{Kind: model.AddressSymbol, Path: "1/2/3", DPT: "9.001", Index: 2}, "index"},
		{"length", model.Address{Kind: model.AddressSymbol, Path: "1/2/3", DPT: "9.001", Length: 4}, "length"},
		{"group syntax", model.Address{Kind: model.AddressSymbol, Path: "50/1/1", DPT: "9.001"}, "group address"},
		{"bad dpt", model.Address{Kind: model.AddressSymbol, Path: "1/2/3", DPT: "abc"}, "dpt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planPoint(driver.PointRef{ID: "p", Address: tc.addr, DataType: model.TypeFloat32})
			if err == nil {
				t.Fatalf("want error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 建隧道 / 读
// ---------------------------------------------------------------------------

func TestProbeEstablishesTunnel(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	// Open 不建隧道：此时网关还没见过任何报文。
	if n := srv.Connects(); n != 0 {
		t.Fatalf("open must not connect, connects = %d", n)
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n := srv.Connects(); n != 1 {
		t.Fatalf("connects = %d", n)
	}
	// Probe 不读任何组地址。
	if n := srv.Reads(); n != 0 {
		t.Fatalf("probe must not read group values, reads = %d", n)
	}
}

func TestReadOneBit(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("lamp", "1/1/1", "1.001", model.TypeBool))
	if s.Err != nil {
		t.Fatalf("read: %v", s.Err)
	}
	if s.Value.Type != model.TypeBool || s.Value.Bool {
		t.Fatalf("value = %+v", s.Value)
	}
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %v", s.Quality)
	}
}

func TestReadTwoByteFloat(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("temp", "2/1/1", "9.001", model.TypeFloat32))
	if s.Err != nil {
		t.Fatalf("read: %v", s.Err)
	}
	if got := s.Value.Num; got < 21.49 || got > 21.51 {
		t.Fatalf("temp = %v, want 21.5", got)
	}
}

func TestReadFourByteFloat(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("power", "4/1/1", "14.056", model.TypeFloat32))
	if s.Err != nil {
		t.Fatalf("read: %v", s.Err)
	}
	if s.Value.Num != 1234.5 {
		t.Fatalf("power = %v, want 1234.5", s.Value.Num)
	}
}

func TestReadScaledPercent(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("dim", "3/1/1", "5.001", model.TypeFloat32))
	if s.Err != nil {
		t.Fatalf("read: %v", s.Err)
	}
	// 50% 在线上是 raw 128，来回一圈应该回到 50% 附近（量化误差 ≤ 0.4）。
	if s.Value.Num < 49.6 || s.Value.Num > 50.4 {
		t.Fatalf("dim = %v", s.Value.Num)
	}
}

func TestReadUnknownGroupAddressTimesOut(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	c.timeout = 300 * time.Millisecond
	s := readOne(t, c, ref("nobody", "9/7/9", "9.001", model.TypeFloat32))
	if s.Err == nil {
		t.Fatal("want timeout")
	}
	if driver.KindOf(s.Err) != driver.ErrTimeout {
		t.Fatalf("kind = %v (err=%v)", driver.KindOf(s.Err), s.Err)
	}
	if s.Quality != model.QualityTimeout {
		t.Fatalf("quality = %v", s.Quality)
	}
	// 空的组地址上不该有任何设备出声。
	if n := srv.Reads(); n != 0 {
		t.Fatalf("reads = %d", n)
	}
}

func TestReadWrongDPTIsAddressError(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	// 2/1/1 在线上是 2 字节浮点；按 4 字节读会得到长度不符。
	s := readOne(t, c, ref("bad", "2/1/1", "14.056", model.TypeFloat32))
	if s.Err == nil {
		t.Fatal("want error")
	}
	if driver.KindOf(s.Err) != driver.ErrAddress {
		t.Fatalf("kind = %v (err=%v)", driver.KindOf(s.Err), s.Err)
	}
}

func TestReadAbortsRemainingAfterLinkFailure(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	c.timeout = 200 * time.Millisecond
	start := time.Now()
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("a", "9/7/1", "9.001", model.TypeFloat32),
		ref("b", "9/7/2", "9.001", model.TypeFloat32),
		ref("c", "9/7/3", "9.001", model.TypeFloat32),
	}}})
	if err == nil {
		t.Fatal("want error")
	}
	// 第一次超时之后不该再逐个等超时：三个点只应消耗一份预算。
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Fatalf("took %s; the loop failed to abort after the first link error", d)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d", len(samples))
	}
	for _, s := range samples {
		if driver.KindOf(s.Err) != driver.ErrTimeout {
			t.Fatalf("sample %s: err = %v", s.PointID, s.Err)
		}
	}
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func TestWriteSwitchAndReadBack(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	r := ref("lamp", "1/1/2", "1.001", model.TypeBool)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: r.ID, Address: r.Address, DataType: r.DataType,
		Value: model.BoolValue(false),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("results = %+v", res)
	}
	// 从站对象表必须真的变了：写入不是「发出去就算成功」。
	p, ok := srv.Payload("1/1/2")
	if !ok {
		t.Fatal("group address vanished")
	}
	if p.Bit {
		t.Fatalf("sim still on: %+v", p)
	}
	s := readOne(t, c, r)
	if s.Err != nil || s.Value.Bool {
		t.Fatalf("read back = %+v err=%v", s.Value, s.Err)
	}
}

func TestWriteFloat(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	r := ref("temp", "2/1/2", "9.007", model.TypeFloat32)
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: r.ID, Address: r.Address, DataType: r.DataType,
		Value: model.NumValue(model.TypeFloat32, 22.5),
	}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := readOne(t, c, r)
	if s.Err != nil {
		t.Fatalf("read: %v", s.Err)
	}
	if s.Value.Num < 22.49 || s.Value.Num > 22.51 {
		t.Fatalf("temp = %v", s.Value.Num)
	}
}

// 写一个总线上没人拥有的组地址：链路层确认仍然正常，写入判成功。
//
// 这是协议语义，不是宽容：组地址是广播语义，写一个没人订阅的地址时帧确实
// 上了总线，没有任何设备需要为它改变状态。把它判成故障，会让「写了但没
// 生效」和「网络断了」变成同一件事——而这两件事的处理方式完全不同。
func TestWriteUnknownGroupAddressSucceeds(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	r := ref("ghost", "9/7/7", "1.001", model.TypeBool)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: r.ID, Address: r.Address, DataType: r.DataType,
		Value: model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("results = %+v", res)
	}
}

// 链路层确认报「有错」时，写入必须失败：帧发出去了，但总线级发送失败
// （总线忙、冲突、执行器离线）。这与「网关没收到我们的帧」是两件事。
func TestWriteFailsWhenLinkLayerConfirmReportsAnError(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	srv.FailNextConfirms(1)
	r := ref("lamp", "1/1/1", "1.001", model.TypeBool)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: r.ID, Address: r.Address, DataType: r.DataType,
		Value: model.BoolValue(true),
	}})
	if err == nil {
		t.Fatal("want error: the link layer reported a bus-level failure")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %v (err=%v)", driver.KindOf(err), err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("results = %+v", res)
	}
	// 下一次必须恢复正常：注入只影响一条。
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: r.ID, Address: r.Address, DataType: r.DataType,
		Value: model.BoolValue(false),
	}}); err != nil {
		t.Fatalf("follow-up write: %v", err)
	}
}

func TestWriteOutOfRangeValueIsAddressError(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	r := ref("dim", "3/1/1", "5.001", model.TypeFloat32)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: r.ID, Address: r.Address, DataType: r.DataType,
		Value: model.NumValue(model.TypeFloat32, 250), // 5.001 上限 100%
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// 序号 / 重传 / 隧道重建
// ---------------------------------------------------------------------------

func TestRetransmitOnDroppedACK(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// 丢掉接下来两个 ACK：一次读要多试几次才能成功。
	srv.DropNextACKs(2)
	s := readOne(t, c, ref("temp", "2/1/1", "9.001", model.TypeFloat32))
	if s.Err != nil {
		t.Fatalf("read after dropped ACKs: %v", s.Err)
	}
	if n := srv.Retransmits(); n == 0 {
		t.Fatal("sim never saw a retransmission")
	}
	// 重传之后序号必须仍然同步：再来一次读必须成功。
	if s := readOne(t, c, ref("temp", "2/1/1", "9.001", model.TypeFloat32)); s.Err != nil {
		t.Fatalf("follow-up read: %v", s.Err)
	}
}

func TestSequenceErrorRebuildsTunnel(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	srv.FailNextSequence()
	s := readOne(t, c, ref("temp", "2/1/1", "9.001", model.TypeFloat32))
	if s.Err == nil {
		t.Fatal("want error")
	}
	if driver.KindOf(s.Err) != driver.ErrProtocol {
		t.Fatalf("kind = %v (err=%v)", driver.KindOf(s.Err), s.Err)
	}
	if n := srv.SeqErrors(); n == 0 {
		t.Fatal("sim never rejected a sequence number")
	}
	// 序号错乱后驱动应丢掉隧道；下一次读会重建并成功。
	if s := readOne(t, c, ref("temp", "2/1/1", "9.001", model.TypeFloat32)); s.Err != nil {
		t.Fatalf("read after resync: %v", s.Err)
	}
	if n := srv.Connects(); n < 2 {
		t.Fatalf("connects = %d, want the tunnel to be re-established", n)
	}
}

func TestExpiredTunnelIsRedetected(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	srv.ExpireTunnels()
	// 心跳会发现通道失效。把心跳窗口压到 0，让下一次 ensure 立刻检查。
	c.lastActivity = time.Now().Add(-2 * c.st.heartbeat)
	err := c.Probe(context.Background())
	if err == nil {
		t.Fatal("probe should notice the reclaimed channel")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %v (err=%v)", driver.KindOf(err), err)
	}
	// 再读一次：驱动应重建隧道并读成功。
	if s := readOne(t, c, ref("temp", "2/1/1", "9.001", model.TypeFloat32)); s.Err != nil {
		t.Fatalf("read after reconnect: %v", s.Err)
	}
}

// ---------------------------------------------------------------------------
// 网关不给链路层确认
// ---------------------------------------------------------------------------

func TestWriteSucceedsWhenGatewaySendsNoConfirm(t *testing.T) {
	srv, err := knxsim.NewServerWithOptions("127.0.0.1:0", knxsim.Options{
		DisableAutoIncrement: true,
		DisableConfirm:       true,
	})
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c := openSim(t, srv, nil)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "lamp",
		Address:  model.Address{Kind: model.AddressSymbol, Path: "1/1/1", DPT: "1.001"},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write must succeed without a link-layer confirmation: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("results = %+v", res)
	}
	if p, ok := srv.Payload("1/1/1"); !ok || !p.Bit {
		t.Fatalf("sim object = %+v ok=%v", p, ok)
	}
}

// ---------------------------------------------------------------------------
// 隧道槽位
// ---------------------------------------------------------------------------

func TestTunnelSlotExhaustionIsRetryable(t *testing.T) {
	srv, err := knxsim.NewServerWithOptions("127.0.0.1:0", knxsim.Options{
		MaxTunnels:           1,
		DisableAutoIncrement: true,
	})
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	first := openSim(t, srv, nil)
	if err := first.Probe(context.Background()); err != nil {
		t.Fatalf("first probe: %v", err)
	}

	second := openSim(t, srv, nil)
	err = second.Probe(context.Background())
	if err == nil {
		t.Fatal("second tunnel must be refused")
	}
	// 槽位满不是配置错误：换一台设备也不该被判成地址错，必须可重试。
	if driver.KindOf(err) != driver.ErrDisconnected || !driver.IsRetryable(err) {
		t.Fatalf("kind = %v retryable = %v (err=%v)", driver.KindOf(err), driver.IsRetryable(err), err)
	}
	if n := srv.Refusals(); n == 0 {
		t.Fatal("sim never refused a connection")
	}
	// 释放第一个槽位后，第二个应该能连上。
	_ = first.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := second.Probe(context.Background()); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("second tunnel never got the freed slot")
}

func TestCloseReturnsSlot(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n := srv.Tunnels(); n != 1 {
		t.Fatalf("tunnels = %d", n)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := srv.Tunnels(); n != 0 {
		t.Fatalf("tunnels after close = %d, want the slot to be released", n)
	}
}

// ---------------------------------------------------------------------------
// 源地址
// ---------------------------------------------------------------------------

func TestSourceAddressFromLabels(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, func(d *model.Device) {
		d.Labels = map[string]string{"sourceAddress": "1.1.200"}
	})
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := c.source.String(); got != "1.1.200" {
		t.Fatalf("source = %s", got)
	}
}

func TestSourceAddressDefaultsToAssigned(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// 内置网关分配 15.15.<通道号>。
	if got := c.source.String(); !strings.HasPrefix(got, "15.15.") {
		t.Fatalf("source = %s, want the address assigned by the gateway", got)
	}
}

// ---------------------------------------------------------------------------
// 辅助类型
// ---------------------------------------------------------------------------

type nopDatagram struct{}

func (nopDatagram) Write([]byte) error          { return nil }
func (nopDatagram) Read() ([]byte, error)       { return nil, errors.New("nop") }
func (nopDatagram) SetDeadline(time.Time) error { return nil }
func (nopDatagram) Close() error                { return nil }

var _ transport.Datagram = nopDatagram{}

// TestReadPartialFailureKeepsGoodSamples 钉住部分失败契约（docs §3.7.4 /
// §3.9.5）：一个组地址配错（ErrAddress）时，只要还有别的点读成功，Read 就
// 不向上返回错误。否则 collector 会丢弃隧道、整台网关每周期重连。
func TestReadPartialFailureKeepsGoodSamples(t *testing.T) {
	c := openSim(t, newSim(t), nil)
	good := ref("lamp", "1/1/1", "1.001", model.TypeBool)
	bad := ref("bad", "9/9/9", "1.001", model.TypeBool)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{good, bad},
	}})
	if err != nil {
		t.Fatalf("有成功点时 Read 不该返回错误，得到 %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].Err != nil || samples[0].Quality != model.QualityGood {
		t.Fatalf("好点不该被连累: %+v", samples[0])
	}
	if samples[1].Err == nil || driver.KindOf(samples[1].Err) != driver.ErrAddress {
		t.Fatalf("坏点应带 address 错误: %+v", samples[1])
	}
}

// 全部点位失败时必须返回错误，让熔断器看到「设备全坏」。
func TestReadAllFailedStillReturnsError(t *testing.T) {
	c := openSim(t, newSim(t), nil)
	bad1 := ref("bad1", "9/9/8", "1.001", model.TypeBool)
	bad2 := ref("bad2", "9/9/9", "1.001", model.TypeBool)

	if _, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{bad1, bad2},
	}}); err == nil {
		t.Fatal("全部点位失败时必须返回错误")
	}
}
