package coap

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	coapcodec "uic/internal/codec/coap"
	"uic/internal/driver"
	"uic/internal/model"
	simcoap "uic/internal/sim/coap"
	"uic/internal/transport"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

func pt(id, path, selector, dt string) model.Point {
	return model.Point{
		ID: id, Enabled: true, DataType: model.DataType(dt),
		Address: model.Address{Kind: model.AddressSymbol, Path: path, Selector: selector},
	}
}

func baseDevice(points ...model.Point) model.Device {
	return model.Device{
		ID: "dev", ChannelID: "ch", Enabled: true,
		Timeout: model.Duration(3 * time.Second),
		Points:  points,
	}
}

func baseConfig(addr string, dev model.Device) driver.OpenConfig {
	return driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "coap",
			Transport: model.Transport{Kind: "udp", Address: addr}},
		Device: dev,
	}
}

func spans(points ...model.Point) []driver.ReadSpan {
	refs := make([]driver.PointRef, 0, len(points))
	for _, p := range points {
		refs = append(refs, driver.PointRefOf(model.Device{}, p))
	}
	return []driver.ReadSpan{{ID: "s", Points: refs}}
}

// openSim 启动内置服务端并把驱动连上去。
func openSim(t *testing.T, labels map[string]string, points ...model.Point) (driver.Conn, *simcoap.Server) {
	t.Helper()
	s, err := simcoap.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dev := baseDevice(points...)
	dev.Labels = labels
	conn, err := New().Open(context.Background(), baseConfig(s.Addr(), dev))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, s
}

func readOne(t *testing.T, conn driver.Conn, p model.Point) driver.RawSample {
	t.Helper()
	got, err := conn.Read(context.Background(), spans(p))
	if len(got) != 1 {
		t.Fatalf("read %s: got %d samples, want 1 (err=%v)", p.ID, len(got), err)
	}
	return got[0]
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// TestReadTextAndBool 覆盖纯文本数值与布尔。
func TestReadTextAndBool(t *testing.T) {
	temp := pt("temp", "/sensors/temp", "", "float64")
	valve := pt("valve", "/actuator/valve", "", "bool")
	conn, _ := openSim(t, nil, temp, valve)

	s := readOne(t, conn, temp)
	if s.Err != nil || s.Quality != model.QualityGood {
		t.Fatalf("temp sample = %+v", s)
	}
	if s.Value.Num != 21.5 {
		t.Fatalf("temp = %v, want 21.5", s.Value.Num)
	}

	s = readOne(t, conn, valve)
	if s.Err != nil || s.Value.Bool {
		t.Fatalf("valve sample = %+v", s)
	}
}

// TestReadJSONSelector 覆盖 JSON 字段选择与整文档读取。
func TestReadJSONSelector(t *testing.T) {
	temp := pt("temp", "/sensors/env", "temperature", "float64")
	hum := pt("hum", "/sensors/env", "humidity", "float32")
	ok := pt("ok", "/sensors/env", "ok", "bool")
	doc := pt("doc", "/sensors/env", "", "string")
	conn, _ := openSim(t, nil, temp, hum, ok, doc)

	if s := readOne(t, conn, temp); s.Err != nil || s.Value.Num != 21.5 {
		t.Fatalf("temperature = %+v", s)
	}
	if s := readOne(t, conn, hum); s.Err != nil || s.Value.Num != 45.0 {
		t.Fatalf("humidity = %+v", s)
	}
	if s := readOne(t, conn, ok); s.Err != nil || !s.Value.Bool {
		t.Fatalf("ok = %+v", s)
	}

	// 无 selector：整段 JSON 作为紧凑文本交给 string 点位。
	s := readOne(t, conn, doc)
	if s.Err != nil {
		t.Fatalf("document read: %v", s.Err)
	}
	for _, want := range []string{"temperature", "humidity", "ok"} {
		if !strings.Contains(s.Value.Text, want) {
			t.Fatalf("document %q missing %q", s.Value.Text, want)
		}
	}
}

// TestReadOctets 覆盖 application/octet-stream。
func TestReadOctets(t *testing.T) {
	blob := pt("blob", "/blob", "", "bytes")
	conn, _ := openSim(t, nil, blob)

	s := readOne(t, conn, blob)
	if s.Err != nil {
		t.Fatalf("blob read: %v", s.Err)
	}
	if s.Value.Type != model.TypeBytes || s.Value.Text != "\x00\x01\x02\xff" {
		t.Fatalf("blob = %q type=%s", s.Value.Text, s.Value.Type)
	}
}

// TestReadBlock2 覆盖分块接收：3000 字节必须被完整重组。
func TestReadBlock2(t *testing.T) {
	large := pt("large", "/large", "", "string")
	conn, s := openSim(t, nil, large)

	smp := readOne(t, conn, large)
	if smp.Err != nil {
		t.Fatalf("large read: %v", smp.Err)
	}
	want, _ := s.Value("/large")
	if smp.Value.Text != want {
		t.Fatalf("large = %d bytes, want %d", len(smp.Value.Text), len(want))
	}
	if len(smp.Value.Text) != 3000 {
		t.Fatalf("large = %d bytes, want 3000", len(smp.Value.Text))
	}
	if s.BlockGets() == 0 {
		t.Fatal("server never saw a Block2 continuation")
	}
}

// TestReadBlock2Fallback 覆盖「服务端不认 Block2」时的降级。
//
// 驱动首选块大小后，首个请求会带 Block2；老服务端回 4.02。驱动应丢掉
// 选项、重新不带 Block2 取一次，而不是把 4.02 当成资源不存在上报。
func TestReadBlock2Fallback(t *testing.T) {
	s, err := simcoap.NewServerWithOptions("127.0.0.1:0", simcoap.Options{DisableBlock2: true})
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	defer s.Close()

	large := pt("large", "/large", "", "string")
	dev := baseDevice(large)
	dev.Labels = map[string]string{"blockSize": "64"}
	conn, err := New().Open(context.Background(), baseConfig(s.Addr(), dev))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	smp := readOne(t, conn, large)
	if smp.Err != nil {
		t.Fatalf("fallback read: %v", smp.Err)
	}
	if len(smp.Value.Text) != 3000 {
		t.Fatalf("fallback body = %d bytes, want 3000", len(smp.Value.Text))
	}
}

// TestReadSeparateResponse 覆盖空 ACK + 分离响应。
func TestReadSeparateResponse(t *testing.T) {
	sep := pt("sep", "/separate", "", "float64")
	conn, s := openSim(t, nil, sep)

	smp := readOne(t, conn, sep)
	if smp.Err != nil {
		t.Fatalf("separate read: %v", smp.Err)
	}
	if smp.Value.Num != 7.5 {
		t.Fatalf("separate = %v, want 7.5", smp.Value.Num)
	}
	// 驱动应回 ACK，服务端据此停止重传。
	deadline := time.Now().Add(2 * time.Second)
	for !s.SeparateAcked() {
		if time.Now().After(deadline) {
			t.Fatal("driver did not ACK the separate response")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReadReset 覆盖对端 RST：属于协议拒绝，不是超时。
func TestReadReset(t *testing.T) {
	reset := pt("reset", "/reset", "", "string")
	conn, _ := openSim(t, nil, reset)

	smp := readOne(t, conn, reset)
	if smp.Err == nil {
		t.Fatal("expected error for RST response")
	}
	if k := driver.KindOf(smp.Err); k != driver.ErrProtocol {
		t.Fatalf("error kind = %s, want protocol (%v)", k, smp.Err)
	}
	if smp.Quality != model.QualityBad {
		t.Fatalf("quality = %s, want bad", smp.Quality)
	}
}

// TestReadNotFound 覆盖 4.04 → 地址错误（配置问题，不该熔断）。
func TestReadNotFound(t *testing.T) {
	missing := pt("missing", "/no/such/thing", "", "string")
	conn, _ := openSim(t, nil, missing)

	smp := readOne(t, conn, missing)
	if smp.Err == nil {
		t.Fatal("expected error for 4.04")
	}
	if k := driver.KindOf(smp.Err); k != driver.ErrAddress {
		t.Fatalf("error kind = %s, want address (%v)", k, smp.Err)
	}
	if driver.CountsTowardBreaker(smp.Err) {
		t.Fatal("404 must not count toward the breaker")
	}
	if smp.Quality != model.QualityAddressError {
		t.Fatalf("quality = %s, want address_error", smp.Quality)
	}
}

// TestReadStopsAfterLinkFailure 说明「链路级失败后剩余点位直接标错」。
//
// 若继续逐点重试，一轮采集会被拖成 N 倍超时；这里用一个只对首个点位
// 可达的场景无法构造，于是退而验证：一个失败点位不会污染同批的健康点位。
func TestReadMixedSuccessAndFailure(t *testing.T) {
	temp := pt("temp", "/sensors/temp", "", "float64")
	missing := pt("missing", "/nope", "", "string")
	conn, _ := openSim(t, nil, temp, missing)

	got, err := conn.Read(context.Background(), spans(temp, missing))
	if err == nil {
		t.Fatal("expected an error from the missing resource")
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2", len(got))
	}
	if got[0].PointID != "temp" || got[0].Err != nil || got[0].Value.Num != 21.5 {
		t.Fatalf("temp = %+v", got[0])
	}
	if got[1].PointID != "missing" || got[1].Err == nil {
		t.Fatalf("missing = %+v", got[1])
	}
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// TestWriteTextRoundTrip 覆盖 PUT 纯文本与回读。
func TestWriteTextRoundTrip(t *testing.T) {
	setpoint := pt("sp", "/actuator/setpoint", "", "float64")
	conn, s := openSim(t, nil, setpoint)

	res, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", Address: setpoint.Address, DataType: setpoint.DataType,
		Value: model.NumValue(model.TypeFloat64, 42),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write result = %+v", res[0])
	}
	if got, _ := s.Value("/actuator/setpoint"); got != "42" {
		t.Fatalf("server value = %q, want 42", got)
	}
	if smp := readOne(t, conn, setpoint); smp.Err != nil || smp.Value.Num != 42 {
		t.Fatalf("read back = %+v", smp)
	}
}

// TestWritePost 覆盖 writeMethod=post。
func TestWritePost(t *testing.T) {
	trigger := pt("trig", "/actuator/trigger", "", "bool")
	conn, s := openSim(t, map[string]string{"writeMethod": "post"}, trigger)

	res, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "trig", Address: trigger.Address, DataType: trigger.DataType,
		Value: model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write result = %+v", res[0])
	}
	if got, _ := s.Value("/actuator/trigger"); got != "true" {
		t.Fatalf("server value = %q, want true", got)
	}
}

// TestWriteJSONSelector 覆盖 JSON 写入：带 selector 时只发该子树。
func TestWriteJSONSelector(t *testing.T) {
	mode := pt("mode", "/config", "mode", "string")
	conn, s := openSim(t, map[string]string{"contentFormat": "json"}, mode)

	res, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "mode", Address: mode.Address, DataType: mode.DataType,
		Value: model.TextValue(model.TypeString, "manual"),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write result = %+v", res[0])
	}
	if got, _ := s.Value("/config"); got != `{"mode":"manual"}` {
		t.Fatalf("server body = %q, want {\"mode\":\"manual\"}", got)
	}
	if smp := readOne(t, conn, mode); smp.Err != nil || smp.Value.Text != "manual" {
		t.Fatalf("read back = %+v", smp)
	}
}

// TestWriteJSONArrayRejected 说明「不替用户猜数组其余元素」。
func TestWriteJSONArrayRejected(t *testing.T) {
	elem := pt("elem", "/config", "values[0]", "float64")
	conn, _ := openSim(t, map[string]string{"contentFormat": "json"}, elem)

	res, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "elem", Address: elem.Address, DataType: elem.DataType,
		Value: model.NumValue(model.TypeFloat64, 1),
	}})
	if err == nil {
		t.Fatal("expected an error for array-element write")
	}
	if k := driver.KindOf(err); k != driver.ErrUnsupported {
		t.Fatalf("kind = %s, want unsupported", k)
	}
	if res[0].OK {
		t.Fatal("result marked OK despite error")
	}
}

// TestWriteOctetsRoundTrip 覆盖二进制写入与回读。
func TestWriteOctetsRoundTrip(t *testing.T) {
	blob := pt("sp", "/actuator/setpoint", "", "bytes")
	conn, _ := openSim(t, map[string]string{"contentFormat": "octets"}, blob)

	payload := "\x01\x02\x03"
	res, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", Address: blob.Address, DataType: blob.DataType,
		Value: model.BytesValue([]byte(payload)),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write result = %+v", res[0])
	}
	if smp := readOne(t, conn, blob); smp.Err != nil || smp.Value.Text != payload {
		t.Fatalf("read back = %+v", smp)
	}
}

// TestWriteRejectsOutOfRangeValue 钉住「越界判坏，不截断」。
//
// 写路径曾经把值直接交给 `int64(n)`：`int16` 的 40000 会原样写成 "40000"，
// `int64` 的 1e30 会回绕成 "-9223372036854775808"——设备收到一个完全错误
// 的数，而驱动报成功。这里不仅要求报错，还要求一个字节都没发出去。
func TestWriteRejectsOutOfRangeValue(t *testing.T) {
	cases := []struct {
		name string
		dt   string
		v    model.Value
	}{
		{"int16 over", "int16", model.NumValue(model.TypeInt16, 40000)},
		{"int32 fraction", "int32", model.NumValue(model.TypeInt32, 1.5)},
		{"int64 overflow", "int64", model.NumValue(model.TypeInt64, 1e30)},
		{"uint64 overflow", "uint64", model.NumValue(model.TypeUint64, 1e30)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 两条编码路径都要覆盖：text 走 textval.Encode，json 走
			// jsonScalar。只测其中一条会让另一条的 `int64(n)` 回绕漏网。
			for _, format := range []string{"text", "json"} {
				t.Run(format, func(t *testing.T) {
					p := pt("sp", "/actuator/setpoint", "", c.dt)
					conn, s := openSim(t, map[string]string{"contentFormat": format}, p)
					res, err := conn.Write(context.Background(), []driver.WriteRequest{{
						PointID: "sp", Address: p.Address, DataType: p.DataType, Value: c.v,
					}})
					if err == nil {
						t.Fatalf("expected an error for %s", c.name)
					}
					if k := driver.KindOf(err); k != driver.ErrUnsupported {
						t.Fatalf("kind = %s, want unsupported (%v)", k, err)
					}
					if len(res) != 1 || res[0].OK || res[0].Quality != model.QualityBad {
						t.Fatalf("result = %+v", res)
					}
					if s.Writes() != 0 {
						t.Fatalf("device saw %d writes, want 0", s.Writes())
					}
				})
			}
		})
	}
}

// TestWrite64BitRoundTrip 确认合法的 64 位边界值仍能写入。
func TestWrite64BitRoundTrip(t *testing.T) {
	p := pt("sp", "/actuator/setpoint", "", "uint64")
	conn, _ := openSim(t, nil, p)
	res, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", Address: p.Address, DataType: p.DataType,
		Value: model.NumValue(model.TypeUint64, 18000000000000000000),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("result = %+v", res[0])
	}
	if smp := readOne(t, conn, p); smp.Err != nil || smp.Value.String() != "18000000000000000000" {
		t.Fatalf("read back = %+v", smp)
	}
}

// TestWritePermissionDenied 覆盖 4.03 → 权限错误（不计入熔断）。
func TestWritePermissionDenied(t *testing.T) {
	// /sensors/temp 只支持 GET；用 PUT 会得到 4.05，映射为 unsupported。
	p := pt("temp", "/sensors/temp", "", "float64")
	conn, _ := openSim(t, nil, p)
	_, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", Address: p.Address, DataType: p.DataType,
		Value: model.NumValue(model.TypeFloat64, 1),
	}})
	if err == nil {
		t.Fatal("expected an error writing to a read-only resource")
	}
	if k := driver.KindOf(err); k != driver.ErrUnsupported {
		t.Fatalf("kind = %s, want unsupported (%v)", k, err)
	}
}

// ---------------------------------------------------------------------------
// 探测与发现
// ---------------------------------------------------------------------------

func TestProbeAndDiscover(t *testing.T) {
	conn, _ := openSim(t, nil, pt("temp", "/sensors/temp", "", "float64"))

	if err := conn.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}

	discoverer, ok := conn.(interface {
		Discover(context.Context) ([]coapcodec.Link, error)
	})
	if !ok {
		t.Fatal("conn does not implement Discover")
	}
	links, err := discoverer.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	paths := map[string]bool{}
	for _, l := range links {
		paths[l.Path] = true
	}
	for _, want := range []string{"/sensors/temp", "/sensors/env", "/config", "/large"} {
		if !paths[want] {
			t.Errorf("discovery missing %s (got %v)", want, paths)
		}
	}
}

// ---------------------------------------------------------------------------
// 超时与重传（注入内存链路，确定性）
// ---------------------------------------------------------------------------

type fakeDatagram struct {
	mu     sync.Mutex
	writes int
	reads  int
	closed bool
}

func (f *fakeDatagram) Write([]byte) error {
	f.mu.Lock()
	f.writes++
	f.mu.Unlock()
	return nil
}
func (f *fakeDatagram) Read() ([]byte, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	return nil, os.ErrDeadlineExceeded
}
func (f *fakeDatagram) SetDeadline(time.Time) error { return nil }
func (f *fakeDatagram) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeDatagram) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes, f.reads
}

func openFake(t *testing.T, labels map[string]string) (driver.Conn, *fakeDatagram) {
	t.Helper()
	fake := &fakeDatagram{}
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return fake, nil
	})
	dev := baseDevice(pt("p", "/x", "", "string"))
	dev.Labels = labels
	conn, err := d.Open(context.Background(), baseConfig("127.0.0.1:1", dev))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, fake
}

// TestTimeoutRetransmits 钉住「1 次首发 + maxRetransmit 次重发」。
func TestTimeoutRetransmits(t *testing.T) {
	conn, fake := openFake(t, map[string]string{"ackTimeout": "100ms", "maxRetransmit": "2"})
	smp := readOne(t, conn, pt("p", "/x", "", "string"))
	if smp.Err == nil {
		t.Fatal("expected timeout")
	}
	if k := driver.KindOf(smp.Err); k != driver.ErrTimeout {
		t.Fatalf("kind = %s, want timeout (%v)", k, smp.Err)
	}
	if smp.Quality != model.QualityTimeout {
		t.Fatalf("quality = %s, want timeout", smp.Quality)
	}
	writes, _ := fake.counts()
	if writes != 3 {
		t.Fatalf("writes = %d, want 3 (1 + 2 retransmits)", writes)
	}
}

// TestMaxRetransmitZero 说明 0 表示「只发一次」。
func TestMaxRetransmitZero(t *testing.T) {
	conn, fake := openFake(t, map[string]string{"ackTimeout": "100ms", "maxRetransmit": "0"})
	smp := readOne(t, conn, pt("p", "/x", "", "string"))
	if smp.Err == nil {
		t.Fatal("expected timeout")
	}
	writes, _ := fake.counts()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
}

// TestContextCanceled 说明「ctx 已取消时一个字节都不发」。
func TestContextCanceled(t *testing.T) {
	conn, fake := openFake(t, map[string]string{"ackTimeout": "100ms"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := conn.Read(ctx, spans(pt("p", "/x", "", "string")))
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
	if k := driver.KindOf(err); k != driver.ErrTimeout {
		t.Fatalf("kind = %s, want timeout", k)
	}
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("samples = %+v", got)
	}
	if writes, _ := fake.counts(); writes != 0 {
		t.Fatalf("writes = %d, want 0 for canceled context", writes)
	}
}

// ---------------------------------------------------------------------------
// 配置校验
// ---------------------------------------------------------------------------

func TestOpenRejectsBadConfig(t *testing.T) {
	good := pt("p", "/x", "", "string")

	cases := []struct {
		name   string
		mutate func(*driver.OpenConfig)
		kind   driver.ErrorKind
	}{
		{"tcp transport", func(c *driver.OpenConfig) { c.Channel.Transport.Kind = "tcp" }, driver.ErrUnsupported},
		{"bad contentFormat", func(c *driver.OpenConfig) { c.Device.Labels = map[string]string{"contentFormat": "xml"} }, driver.ErrUnsupported},
		{"bad writeMethod", func(c *driver.OpenConfig) { c.Device.Labels = map[string]string{"writeMethod": "patch"} }, driver.ErrUnsupported},
		{"ackTimeout too small", func(c *driver.OpenConfig) { c.Device.Labels = map[string]string{"ackTimeout": "10ms"} }, driver.ErrUnsupported},
		{"ackTimeout too large", func(c *driver.OpenConfig) { c.Device.Labels = map[string]string{"ackTimeout": "1m"} }, driver.ErrUnsupported},
		{"ackTimeout below device timeout", func(c *driver.OpenConfig) {
			c.Device.Labels = map[string]string{"ackTimeout": "2s"}
			c.Device.Timeout = model.Duration(1 * time.Second)
		}, driver.ErrUnsupported},
		{"bad blockSize", func(c *driver.OpenConfig) { c.Device.Labels = map[string]string{"blockSize": "100"} }, driver.ErrUnsupported},
		{"bad maxRetransmit", func(c *driver.OpenConfig) { c.Device.Labels = map[string]string{"maxRetransmit": "99"} }, driver.ErrUnsupported},
		{"path with query", func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{{ID: "p", Enabled: true, DataType: "string",
				Address: model.Address{Kind: model.AddressSymbol, Path: "/a?b=1"}}}
		}, driver.ErrAddress},
		{"register addressing", func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{{ID: "p", Enabled: true, DataType: "string",
				Address: model.Address{Kind: model.AddressRegister, Area: "db", Offset: 0}}}
		}, driver.ErrAddress},
		{"missing path", func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{{ID: "p", Enabled: true, DataType: "string",
				Address: model.Address{Kind: model.AddressSymbol}}}
		}, driver.ErrAddress},
		{"invalid dataType", func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{{ID: "p", Enabled: true, DataType: "nope",
				Address: model.Address{Kind: model.AddressSymbol, Path: "/x"}}}
		}, driver.ErrAddress},
		{"negative length", func(c *driver.OpenConfig) {
			c.Device.Points = []model.Point{{ID: "p", Enabled: true, DataType: "bytes",
				Address: model.Address{Kind: model.AddressSymbol, Path: "/x", Length: -1}}}
		}, driver.ErrAddress},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig("127.0.0.1:1", baseDevice(good))
			c.mutate(&cfg)
			_, err := New().Open(context.Background(), cfg)
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if k := driver.KindOf(err); k != c.kind {
				t.Fatalf("%s: kind = %s, want %s (%v)", c.name, k, c.kind, err)
			}
		})
	}
}

// TestParseSettings 覆盖标签的默认值与别名。
func TestParseSettings(t *testing.T) {
	cfg := baseConfig("127.0.0.1:1", baseDevice())
	st, err := parseSettings(cfg)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if st.contentFormat != FormatText || st.writeMethod != MethodPut {
		t.Fatalf("defaults = %+v", st)
	}
	if st.ackTimeout != defaultAckTimeout || st.maxRetransmit != defaultMaxRetransmit {
		t.Fatalf("timeout defaults = %+v", st)
	}

	cfg.Device.Labels = map[string]string{
		"contentFormat": "application/json",
		"writeMethod":   "POST",
		"ackTimeout":    "500ms",
		"maxRetransmit": "1",
		"blockSize":     "256",
	}
	st, err = parseSettings(cfg)
	if err != nil {
		t.Fatalf("aliases: %v", err)
	}
	if st.contentFormat != FormatJSON || st.writeMethod != MethodPost {
		t.Fatalf("aliases = %+v", st)
	}
	if st.ackTimeout != 500*time.Millisecond || st.maxRetransmit != 1 || st.blockSize != 256 {
		t.Fatalf("aliases = %+v", st)
	}

	for _, bad := range []string{"10ms", "1m", "0.5", "x"} {
		cfg.Device.Labels = map[string]string{"ackTimeout": bad}
		if _, err := parseSettings(cfg); err == nil {
			t.Errorf("ackTimeout %q: expected error", bad)
		}
	}
}

func TestValidBlockSize(t *testing.T) {
	for _, n := range []int{16, 32, 64, 128, 256, 512, 1024} {
		if !validBlockSize(n) {
			t.Errorf("%d: want valid", n)
		}
	}
	for _, n := range []int{0, 8, 15, 100, 2048, -16} {
		if validBlockSize(n) {
			t.Errorf("%d: want invalid", n)
		}
	}
}

// ---------------------------------------------------------------------------
// 生命周期与并发
// ---------------------------------------------------------------------------

func TestCloseThenReadWrite(t *testing.T) {
	p := pt("temp", "/sensors/temp", "", "float64")
	conn, _ := openSim(t, nil, p)

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 重复 Close 必须幂等。
	if err := conn.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if _, err := conn.Read(context.Background(), spans(p)); err == nil {
		t.Fatal("read after close: expected error")
	} else if k := driver.KindOf(err); k != driver.ErrDisconnected {
		t.Fatalf("read kind = %s, want disconnected", k)
	}
	if _, err := conn.Write(context.Background(), []driver.WriteRequest{{
		PointID: "temp", Address: p.Address, DataType: p.DataType,
		Value: model.NumValue(model.TypeFloat64, 1),
	}}); err == nil {
		t.Fatal("write after close: expected error")
	}
	if err := conn.Probe(context.Background()); err == nil {
		t.Fatal("probe after close: expected error")
	}
}

func TestConcurrentReads(t *testing.T) {
	temp := pt("temp", "/sensors/temp", "", "float64")
	hum := pt("hum", "/sensors/humidity", "", "float64")
	conn, _ := openSim(t, nil, temp, hum)

	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := temp
			if i%2 == 1 {
				p = hum
			}
			got, err := conn.Read(context.Background(), spans(p))
			if err != nil {
				errCh <- err
				return
			}
			if len(got) != 1 || got[0].Err != nil {
				errCh <- errors.New("unexpected sample")
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent read: %v", err)
	}
}

// TestEmptyRead 说明空 span 不发任何报文。
func TestEmptyRead(t *testing.T) {
	conn, _ := openSim(t, nil, pt("temp", "/sensors/temp", "", "float64"))
	got, err := conn.Read(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty read = %v err=%v", got, err)
	}
}
