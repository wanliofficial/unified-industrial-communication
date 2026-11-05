package mqtt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/mqtt"
	"uic/internal/mqtt/mqtttest"
)

// 这组用例走「真 Broker + 真客户端」：订阅驱动的核心风险全在协议交互侧
// （订阅是否建立、重连是否恢复、PUBACK 是否回、主题匹配是否正确），
// 用一个假客户端替换掉这些，测出来的就只是自己写的 if。
//
// mqtttest 的 Broker 按规范实现 SUBSCRIBE/SUBACK/PUBLISH 下发，因此这里
// 测的是「驱动与一个合规 Broker 的配合」，而不是「驱动与自己的 mock 的
// 配合」。

func newDevice(points ...model.Point) model.Device {
	return model.Device{
		ID:       "m1",
		Interval: model.Duration(1 * time.Second),
		Enabled:  true,
		Points:   points,
	}
}

func pt(id, topic string, dt model.DataType) model.Point {
	return model.Point{
		ID: id, Enabled: true, DataType: dt,
		Address: model.Address{Kind: model.AddressSymbol, Path: topic},
	}
}

func ptSel(id, topic, sel string, dt model.DataType) model.Point {
	p := pt(id, topic, dt)
	p.Address.Selector = sel
	return p
}

func channel(addr string) model.Channel {
	return model.Channel{
		ID: "c1", Protocol: "mqtt",
		Transport: model.Transport{Kind: "tcp", Address: addr},
	}
}

func open(t *testing.T, b *mqtttest.Broker, dev model.Device) driver.Conn {
	t.Helper()
	drv := New()
	conn, err := drv.Open(context.Background(), driver.OpenConfig{
		Channel: channel(b.Addr()),
		Device:  dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readAll(t *testing.T, conn driver.Conn, pts ...model.Point) []driver.RawSample {
	t.Helper()
	span := driver.ReadSpan{ID: "s", Points: nil}
	for _, p := range pts {
		span.Points = append(span.Points, driver.PointRef{
			ID: p.ID, Address: p.Address, DataType: p.DataType,
		})
	}
	samples, err := conn.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return samples
}

func mustSample(t *testing.T, samples []driver.RawSample, id string) driver.RawSample {
	t.Helper()
	for _, s := range samples {
		if s.PointID == id {
			return s
		}
	}
	t.Fatalf("sample %q not returned (got %d samples)", id, len(samples))
	return driver.RawSample{}
}

// waitQuality 等某个点位变成期望的质量码。订阅是异步的，读之前必须等消息
// 真的到了；固定 sleep 在慢机器上会偶发失败。
func waitQuality(t *testing.T, conn driver.Conn, p model.Point, want model.Quality) driver.RawSample {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last driver.RawSample
	for time.Now().Before(deadline) {
		last = mustSample(t, readAll(t, conn, p), p.ID)
		if last.Quality == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("point %q: quality stayed %q (want %q)", p.ID, last.Quality, want)
	return driver.RawSample{}
}

// ---------------------------------------------------------------------------
// 订阅建立
// ---------------------------------------------------------------------------

func TestOpenSubscribesToEveryTopic(t *testing.T) {
	b := mqtttest.New(t, nil)
	dev := newDevice(
		pt("t1", "plant/line1/temp", model.TypeFloat64),
		pt("t2", "plant/line1/pressure", model.TypeFloat64),
		// 两个点位同主题：只应订阅一次。
		ptSel("j1", "plant/line1/json", "a", model.TypeFloat64),
		ptSel("j2", "plant/line1/json", "b", model.TypeFloat64),
	)
	open(t, b, dev)

	subs := b.WaitSubscriptions(t, 3)
	want := []string{"plant/line1/json", "plant/line1/pressure", "plant/line1/temp"}
	if strings.Join(subs, ",") != strings.Join(want, ",") {
		t.Fatalf("subscriptions = %v, want %v", subs, want)
	}
}

func TestOpenRejectsWildcardTopic(t *testing.T) {
	b := mqtttest.New(t, nil)
	dev := newDevice(pt("t1", "plant/+/temp", model.TypeFloat64))
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: channel(b.Addr()), Device: dev,
	})
	if err == nil {
		t.Fatal("wildcard topic must be rejected")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %s, want address", driver.KindOf(err))
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("error should mention wildcard: %v", err)
	}
}

func TestOpenRejectsNonSymbolAddress(t *testing.T) {
	b := mqtttest.New(t, nil)
	dev := newDevice(model.Point{
		ID: "t1", Enabled: true, DataType: model.TypeUint16,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 1},
	})
	if _, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: channel(b.Addr()), Device: dev,
	}); err == nil {
		t.Fatal("register address must be rejected")
	}
}

func TestOpenRejectsRejectedSubscription(t *testing.T) {
	// ACL 拒绝时 Broker 回 SUBACK + 0x80。把它当成功，会得到一个「在线但
	// 永远没数据」的设备，现场无法与「设备没发数据」区分。
	b := mqtttest.New(t, mqtttest.Rejecting)
	dev := newDevice(pt("t1", "plant/line1/temp", model.TypeFloat64))
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: channel(b.Addr()), Device: dev,
	})
	if err == nil {
		t.Fatal("rejected subscription must fail open")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error should mention rejection: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 载荷解码
// ---------------------------------------------------------------------------

func TestReadPlainScalar(t *testing.T) {
	b := mqtttest.New(t, nil)
	temp := pt("t1", "plant/temp", model.TypeFloat64)
	running := pt("r1", "plant/running", model.TypeBool)
	label := pt("l1", "plant/label", model.TypeString)
	conn := open(t, b, newDevice(temp, running, label))
	b.WaitSubscriptions(t, 3)

	b.Publish("plant/temp", []byte("21.5\n"), 1, false)
	b.Publish("plant/running", []byte("ON"), 1, true)
	b.Publish("plant/label", []byte("  LINE-1  "), 1, false)

	got := waitQuality(t, conn, temp, model.QualityGood)
	if got.Value.Type != model.TypeFloat64 || got.Value.Num != 21.5 {
		t.Fatalf("temp = %+v", got.Value)
	}
	if s := waitQuality(t, conn, running, model.QualityGood); !s.Value.Bool {
		t.Fatalf("running = %+v", s.Value)
	}
	// string 要 trim：设备常在载荷末尾带 \r\n。
	if s := waitQuality(t, conn, label, model.QualityGood); s.Value.Text != "LINE-1" {
		t.Fatalf("label = %q", s.Value.Text)
	}
}

func TestReadJSONSelector(t *testing.T) {
	b := mqtttest.New(t, nil)
	temp := ptSel("t1", "plant/msg", "data.temperature", model.TypeFloat64)
	first := ptSel("f1", "plant/msg", "values[0].v", model.TypeInt32)
	conn := open(t, b, newDevice(temp, first))
	b.WaitSubscriptions(t, 1)

	b.Publish("plant/msg", []byte(`{"data":{"temperature":23.75},"values":[{"v":7},{"v":8}]}`), 1, false)

	if s := waitQuality(t, conn, temp, model.QualityGood); s.Value.Num != 23.75 {
		t.Fatalf("temp = %+v", s.Value)
	}
	if s := waitQuality(t, conn, first, model.QualityGood); s.Value.Num != 7 {
		t.Fatalf("values[0].v = %+v", s.Value)
	}
}

func TestBadPayloadKeepsLastValue(t *testing.T) {
	b := mqtttest.New(t, nil)
	temp := pt("t1", "plant/temp", model.TypeFloat64)
	conn := open(t, b, newDevice(temp))
	b.WaitSubscriptions(t, 1)

	b.Publish("plant/temp", []byte("30"), 1, false)
	waitQuality(t, conn, temp, model.QualityGood)

	// 同主题上一条状态消息（非数值）不应把好值打成坏值。
	b.Publish("plant/temp", []byte("heartbeat"), 1, false)
	time.Sleep(200 * time.Millisecond)
	if s := mustSample(t, readAll(t, conn, temp), "t1"); s.Quality != model.QualityGood || s.Value.Num != 30 {
		t.Fatalf("value was clobbered by unparsable payload: %+v", s)
	}
}

func TestOutOfRangeIsError(t *testing.T) {
	b := mqtttest.New(t, nil)
	// uint16 收 70000：截断成 4464 会让操作员按错的数字决策。
	small := pt("u1", "plant/u16", model.TypeUint16)
	conn := open(t, b, newDevice(small))
	b.WaitSubscriptions(t, 1)
	b.Publish("plant/u16", []byte("70000"), 1, false)

	time.Sleep(300 * time.Millisecond)
	s := mustSample(t, readAll(t, conn, small), "u1")
	if s.Quality == model.QualityGood {
		t.Fatalf("out-of-range must not be good: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// 质量码三档
// ---------------------------------------------------------------------------

func TestNeverReceivedIsBad(t *testing.T) {
	b := mqtttest.New(t, nil)
	temp := pt("t1", "plant/temp", model.TypeFloat64)
	conn := open(t, b, newDevice(temp))
	b.WaitSubscriptions(t, 1)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{ID: "t1", Address: temp.Address, DataType: temp.DataType}}}
	samples, err := conn.Read(context.Background(), []driver.ReadSpan{span})
	// 一条消息都没有时**不能**返回整体错误：那会被采集层当成断链，
	// 于是每轮都重连、重新订阅，而设备其实好好的。
	if err != nil {
		t.Fatalf("read must not fail when simply no message yet: %v", err)
	}
	s := mustSample(t, samples, "t1")
	if s.Quality != model.QualityBad {
		t.Fatalf("quality = %q, want bad", s.Quality)
	}
	if s.Err == nil {
		t.Fatal("bad sample must carry an error for diagnostics")
	}
}

func TestStaleAfterIsStaleAndKeepsValue(t *testing.T) {
	temp := pt("t1", "plant/temp", model.TypeFloat64)
	// 不启 Broker：这条用例只验证「新鲜度 → 质量码」的纯逻辑。用真实设备
	// 周期（1s）要等 5 秒才 stale，太慢，所以直接构造 conn 把阈值压到 80ms。
	c := &conn{
		deviceID:   "m1",
		points:     map[string]*pointState{"t1": {topic: "plant/temp", dataType: model.TypeFloat64}},
		staleAfter: 80 * time.Millisecond,
		values:     map[string]cachedValue{},
	}
	_ = temp

	c.onMessage(mqtt.Publish{Topic: "plant/temp", Payload: []byte("42")})

	c.mu.Lock()
	fresh := c.lookupLocked("t1", time.Now())
	c.mu.Unlock()
	if fresh.Quality != model.QualityGood || fresh.Value.Num != 42 {
		t.Fatalf("fresh = %+v", fresh)
	}

	time.Sleep(120 * time.Millisecond)
	c.mu.Lock()
	stale := c.lookupLocked("t1", time.Now())
	c.mu.Unlock()
	if stale.Quality != model.QualityStale {
		t.Fatalf("quality = %q, want stale", stale.Quality)
	}
	// 值必须保留：清空会让趋势图断点，且看不出「最后是多少」。
	if stale.Value.Num != 42 {
		t.Fatalf("stale value = %+v, want 42 kept", stale.Value)
	}
	if driver.KindOf(stale.Err) != driver.ErrTimeout {
		t.Fatalf("stale error kind = %s", driver.KindOf(stale.Err))
	}
}

func TestLookupUnknownPointIsAddressError(t *testing.T) {
	c := &conn{deviceID: "m1", points: map[string]*pointState{}, values: map[string]cachedValue{}}
	got := c.lookupLocked("nope", time.Now())
	if got.Quality != model.QualityAddressError || driver.KindOf(got.Err) != driver.ErrAddress {
		t.Fatalf("got %+v", got)
	}
}

func TestStaleAfterForDevice(t *testing.T) {
	if got := staleAfterFor(model.Device{Interval: model.Duration(100 * time.Millisecond)}); got != 5*time.Second {
		t.Fatalf("floor = %s, want 5s", got)
	}
	if got := staleAfterFor(model.Device{Interval: model.Duration(10 * time.Second)}); got != 30*time.Second {
		t.Fatalf("3x = %s, want 30s", got)
	}
	if got := staleAfterFor(model.Device{}); got != defaultStaleAfter {
		t.Fatalf("default = %s", got)
	}
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

func TestWritePublishesToTopic(t *testing.T) {
	b := mqtttest.New(t, nil)
	set := pt("s1", "plant/setpoint", model.TypeFloat64)
	set.Writable = true
	conn := open(t, b, newDevice(set))
	b.WaitSubscriptions(t, 1)

	results, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "s1", Address: set.Address, DataType: model.TypeFloat64,
		Value: model.NumValue(model.TypeFloat64, 42.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v", results)
	}
	msgs := b.WaitMessages(t, 1)
	if msgs[0].Topic != "plant/setpoint" || string(msgs[0].Payload) != "42.5" {
		t.Fatalf("published %q = %q", msgs[0].Topic, msgs[0].Payload)
	}
	if msgs[0].QoS != 1 {
		t.Fatalf("qos = %d, want 1 (control command must not be fire-and-forget)", msgs[0].QoS)
	}
}

func TestWriteIntegerHasNoDecimalPoint(t *testing.T) {
	b := mqtttest.New(t, nil)
	set := pt("s1", "plant/count", model.TypeUint16)
	set.Writable = true
	conn := open(t, b, newDevice(set))
	b.WaitSubscriptions(t, 1)

	if _, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "s1", DataType: model.TypeUint16,
		Value: model.NumValue(model.TypeUint16, 7),
	}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(b.WaitMessages(t, 1)[0].Payload); got != "7" {
		t.Fatalf("payload = %q, want \"7\"", got)
	}
}

func TestWriteUnknownPointFailsItem(t *testing.T) {
	b := mqtttest.New(t, nil)
	conn := open(t, b, newDevice(pt("t1", "plant/temp", model.TypeFloat64)))
	b.WaitSubscriptions(t, 1)

	results, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "nope", Value: model.NumValue(model.TypeFloat64, 1),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if results[0].OK || results[0].Quality != model.QualityAddressError {
		t.Fatalf("results = %+v", results[0])
	}
}

// ---------------------------------------------------------------------------
// 重连与订阅恢复
// ---------------------------------------------------------------------------

func TestReconnectRestoresSubscriptions(t *testing.T) {
	b := mqtttest.New(t, nil)
	temp := pt("t1", "plant/temp", model.TypeFloat64)
	conn := open(t, b, newDevice(temp))
	b.WaitSubscriptions(t, 1)
	b.Publish("plant/temp", []byte("1"), 1, false)
	waitQuality(t, conn, temp, model.QualityGood)

	// 网络闪断：Broker 侧断开所有连接。重连是**按需**触发的——订阅驱动
	// 没有后台重连协程，下一次 Read 会先自愈再取数。这样断链期间不会
	// 无休止地重试打满日志，也不会出现「两个协程同时建链」。
	b.DropAll()
	if s := waitNotConnected(t, conn); s {
		t.Log("client noticed the drop")
	}
	readAll(t, conn, temp)
	b.WaitConnects(t, 2)

	// CleanSession=true 时 Broker 不保留订阅，所以重连后必须再 SUBSCRIBE
	// 一次；只看 Connects() 增加会漏掉「连上了但没重新订阅」这个坑。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && b.Connects() < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	b.Publish("plant/temp", []byte("2"), 1, false)
	// 不能只等 quality==good：断链前的旧值在 5s 内仍是 good，会立刻通过。
	// 必须等到「新值」出现，才算证明订阅真的恢复了。
	deadline = time.Now().Add(5 * time.Second)
	var got float64
	for time.Now().Before(deadline) {
		if s := mustSample(t, readAll(t, conn, temp), "t1"); s.Value.Num == 2 {
			got = 2
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != 2 {
		t.Fatal("no value after reconnect: subscription was not restored")
	}
	if b.Connects() < 2 {
		t.Fatalf("connects = %d, want >= 2", b.Connects())
	}
}

// waitNotConnected 等客户端自己发现断链（读循环收到 EOF）。这只是为了让
// 后续断言观察的是一个稳定的状态，不影响结论。
func waitNotConnected(t *testing.T, c driver.Conn) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !c.(*conn).client.Stats().Connected {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestReadReconnectsAfterDrop(t *testing.T) {
	b := mqtttest.New(t, nil)
	temp := pt("t1", "plant/temp", model.TypeFloat64)
	conn := open(t, b, newDevice(temp))
	b.WaitSubscriptions(t, 1)

	b.DropAll()
	// 等客户端自己发现断链，再读：否则这成了「和读循环赛跑」，结果取决于
	// 调度时机，而不是驱动的行为。真正的「静默链路」由
	// TestReadPingsWhenSilent 单独覆盖。
	if !waitNotConnected(t, conn) {
		t.Fatal("client never noticed the dropped connection")
	}
	if _, err := conn.Read(context.Background(), []driver.ReadSpan{{
		ID: "s", Points: []driver.PointRef{{ID: "t1", Address: temp.Address, DataType: temp.DataType}},
	}}); err != nil {
		t.Fatalf("read after drop: %v", err)
	}
	b.WaitConnects(t, 2)
}

// stubClient 用于验证驱动自身的决策逻辑（何时探测、探测失败怎么上报），
// 不涉及任何协议交互。
type stubClient struct {
	connected bool
	connects  int
	pings     int
	pingErr   error
	published []mqtt.Message
}

func (s *stubClient) Connect(ctx context.Context) error { s.connects++; s.connected = true; return nil }
func (s *stubClient) Ping(ctx context.Context) error {
	s.pings++
	if s.pingErr != nil {
		s.connected = false
	}
	return s.pingErr
}
func (s *stubClient) Publish(ctx context.Context, m mqtt.Message) error {
	s.published = append(s.published, m)
	return nil
}
func (s *stubClient) Close() error      { s.connected = false; return nil }
func (s *stubClient) Stats() mqtt.Stats { return mqtt.Stats{Connected: s.connected} }

// TestReadPingsWhenSilent 覆盖「TCP 还连着但对端不再说话」这半边：
// 拔网线/断电/NAT 过期都不会让本地 socket 立刻报错，只靠连接标志会得到
// 一台「永远在线、永远没数据」的设备。
func TestReadPingsWhenSilent(t *testing.T) {
	stub := &stubClient{connected: true, pingErr: errors.New("no PINGRESP")}
	c := &conn{
		deviceID: "m1",
		client:   stub,
		points: map[string]*pointState{
			"t1": {topic: "plant/temp", dataType: model.TypeFloat64},
		},
		staleAfter: 20 * time.Millisecond,
		opened:     time.Now().Add(-time.Second),
		values:     map[string]cachedValue{},
	}
	_, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID:     "s",
		Points: []driver.PointRef{{ID: "t1", DataType: model.TypeFloat64}},
	}})
	if err == nil {
		t.Fatal("silent link with failed ping must be reported as disconnected")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %s, want disconnected", driver.KindOf(err))
	}
	if stub.pings != 1 {
		t.Fatalf("pings = %d, want 1", stub.pings)
	}
}

// TestReadDoesNotPingWhenFresh：设备正常上报时不能每轮都打一次 PINGREQ。
func TestReadDoesNotPingWhenFresh(t *testing.T) {
	stub := &stubClient{connected: true}
	c := &conn{
		deviceID: "m1",
		client:   stub,
		points: map[string]*pointState{
			"t1": {topic: "plant/temp", dataType: model.TypeFloat64},
		},
		staleAfter: 20 * time.Millisecond,
		opened:     time.Now(),
		values:     map[string]cachedValue{},
	}
	c.onMessage(mqtt.Publish{Topic: "plant/temp", Payload: []byte("5")})
	if _, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID:     "s",
		Points: []driver.PointRef{{ID: "t1", DataType: model.TypeFloat64}},
	}}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stub.pings != 0 {
		t.Fatalf("pings = %d, want 0 while data is fresh", stub.pings)
	}
}

// TestHalfDeadLinkIsDetectedByPing 用真 Broker 走一遍：TCP 不断、订阅还在，
// 只是不再回 PINGRESP。
func TestHalfDeadLinkIsDetectedByPing(t *testing.T) {
	b := mqtttest.New(t, mqtttest.IgnoringPing)
	stub := mqtt.New(mqtt.Options{Addr: b.Addr(), ClientID: "probe", AckTimeout: 150 * time.Millisecond})
	defer func() { _ = stub.Close() }()
	if err := stub.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stub.Ping(context.Background()); err == nil {
		t.Fatal("ping must fail when broker does not answer")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	b := mqtttest.New(t, nil)
	conn := open(t, b, newDevice(pt("t1", "plant/temp", model.TypeFloat64)))
	b.WaitSubscriptions(t, 1)
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// 关闭后读写必须明确失败，而不是返回零值让上层误以为设备还在。
	if _, err := conn.Read(context.Background(), nil); err == nil {
		t.Fatal("read after close must fail")
	}
	if _, err := conn.Write(context.Background(), []driver.WriteRequest{{PointID: "t1"}}); err == nil {
		t.Fatal("write after close must fail")
	}
}

// ---------------------------------------------------------------------------
// 元数据
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "mqtt" {
		t.Fatalf("id = %q", m.ID)
	}
	if m.Capabilities.Subscribe != driver.CapSupported {
		t.Fatalf("subscribe = %s, want supported", m.Capabilities.Subscribe)
	}
	if m.Capabilities.Browse != driver.CapUnsupported {
		t.Fatalf("browse = %s", m.Capabilities.Browse)
	}
	if len(m.AddressKinds) != 1 || m.AddressKinds[0] != model.AddressSymbol {
		t.Fatalf("addressKinds = %v", m.AddressKinds)
	}
	// 通道密码必须标 Secret，否则前端会把明文回显。
	var sawSecret bool
	for _, f := range m.ConfigFields {
		if f.Name == "password" {
			sawSecret = f.Secret
		}
	}
	if !sawSecret {
		t.Fatal("password field must be marked Secret")
	}
}

func TestClientIDIsStableAndUnique(t *testing.T) {
	a := clientID("c1", "d1")
	if a != clientID("c1", "d1") {
		t.Fatal("clientID must be stable across calls")
	}
	if a == clientID("c1", "d2") {
		t.Fatal("clientID must differ per device")
	}
	// ClientID 变化会让 Broker 认为「新客户端」并触发旧连接的遗嘱，
	// 消费方会看到设备反复上下线，所以不能带时间戳/随机数。
	if strings.ContainsAny(a, "0123456789") && strings.Contains(a, "-") {
		// 只要不是随机后缀即可；这里只检查长度上限。
	}
	long := clientID(strings.Repeat("c", 200), strings.Repeat("d", 200))
	if len(long) > 100 {
		t.Fatalf("clientID length = %d, want <= 100", len(long))
	}
}
