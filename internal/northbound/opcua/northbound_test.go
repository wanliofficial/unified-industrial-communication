package opcua

import (
	"context"
	"sync"
	"testing"
	"time"

	"uic/internal/cache"
	ua "uic/internal/codec/opcua"
	"uic/internal/driver"
	opcuadriver "uic/internal/driver/opcua"
	"uic/internal/model"
)

// fakeSource 是北向服务端对网关的全部依赖。用假实现而不是真 Runtime：
// 本包要测的是「点位表怎么变成地址空间」「状态码怎么映射」，把它们和
// 调度器、缓存、配置管理器缠在一起，失败信息会指向错误的方向。
type fakeSource struct {
	specs []Spec
	seeds []Seed

	events chan cache.Event

	mu     sync.Mutex
	writes []writeCall
	result Outcome
}

type writeCall struct {
	Device string
	Point  string
	Value  model.Value
}

func (f *fakeSource) OPCUASpecs() []Spec { return f.specs }

func (f *fakeSource) OPCUASeeds() []Seed { return f.seeds }

func (f *fakeSource) Stream(func(model.Sample) bool, int) (<-chan cache.Event, func()) {
	return f.events, func() {}
}

func (f *fakeSource) OPCUAWrite(_ context.Context, device, point string, v model.Value) Outcome {
	f.mu.Lock()
	f.writes = append(f.writes, writeCall{Device: device, Point: point, Value: v})
	res := f.result
	f.mu.Unlock()
	if res.Code == "" && res.Message == "" {
		res.OK = true
	}
	return res
}

func (f *fakeSource) lastWrite() (writeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return writeCall{}, false
	}
	return f.writes[len(f.writes)-1], true
}

func floatSpec(device, point string, writable bool) Spec {
	return Spec{Device: device, DeviceName: device, Point: point, Name: point,
		DataType: model.TypeFloat64, Writable: writable}
}

func startTest(t *testing.T, src *fakeSource, site string) *Server {
	t.Helper()
	if src.events == nil {
		src.events = make(chan cache.Event, 16)
	}
	s, err := Start(Config{Addr: "127.0.0.1:0", Site: site, Source: src})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func client(t *testing.T, s *Server) driver.Conn {
	t.Helper()
	ch := model.Channel{
		ID: "ch", Protocol: "opcua",
		Transport: model.Transport{Kind: "tcp", Address: s.Addr()},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := opcuadriver.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "ua1", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true,
		},
	})
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func symbolPoint(id, path string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: dt,
		Address: model.Address{Kind: model.AddressSymbol, Path: path, Index: -1},
	}
}

// 质量码映射是北向最容易出错的一段：它决定 SCADA 画面显示绿点还是红叉。
func TestStatusOf(t *testing.T) {
	cases := []struct {
		q    model.Quality
		want ua.StatusCode
	}{
		{model.QualityGood, ua.StatusGood},
		{model.QualityUncertain, ua.StatusUncertain},
		{model.QualityStale, ua.StatusUncertain},
		{model.QualityTimeout, ua.StatusBadTimeout},
		{model.QualityCommError, ua.StatusBadNoCommunication},
		{model.QualityAddressError, ua.StatusBadNodeIDUnknown},
		{model.QualityNotConnected, ua.StatusBadNotConnected},
		{model.QualityDisabled, ua.StatusBadNotConnected},
		{model.Quality("bogus"), ua.StatusBadNoData},
	}
	for _, c := range cases {
		if got := statusOf(c.q); got != c.want {
			t.Fatalf("statusOf(%q) = %#x, want %#x", c.q, got, c.want)
		}
	}
}

func TestWriteStatus(t *testing.T) {
	cases := []struct {
		code string
		want ua.StatusCode
	}{
		{"point.not_found", ua.StatusBadNodeIDUnknown},
		{"point.not_writable", ua.StatusBadNotWritable},
		{"point.disabled", ua.StatusBadNotWritable},
		{"value.type_mismatch", ua.StatusBadTypeMismatch},
		{"device.not_found", ua.StatusBadNoCommunication},
		{"device.not_ready", ua.StatusBadNoCommunication},
		{"service.not_ready", ua.StatusBadNoCommunication},
		{"something.else", ua.StatusBadInternalError},
	}
	for _, c := range cases {
		if got := writeStatus(Outcome{Code: c.code}); got != c.want {
			t.Fatalf("writeStatus(%q) = %#x, want %#x", c.code, got, c.want)
		}
	}
}

// 一个点位一个变量，NodeId 形如 ns=2;s={site}/{device}/{point}。
// 地址空间是 SCADA 集成时唯一要背下来的约定，值得用真实客户端验证。
func TestAddressSpaceAndSeed(t *testing.T) {
	src := &fakeSource{
		specs: []Spec{
			floatSpec("line-a", "temperature", false),
			floatSpec("line-a", "setpoint", true),
		},
		seeds: []Seed{{
			Device: "line-a", Point: "temperature",
			Sample: model.Sample{
				DeviceID: "line-a", PointID: "temperature",
				Value: model.NumValue(model.TypeFloat64, 21.5), Quality: model.QualityGood,
			},
		}},
	}
	s := startTest(t, src, "plant")
	c := client(t, s)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		symbolPoint("temperature", "ns=2;s=plant/line-a/temperature", model.TypeFloat64),
		symbolPoint("setpoint", "ns=2;s=plant/line-a/setpoint", model.TypeFloat64),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Num != 21.5 || samples[0].Value.Type != model.TypeFloat64 {
		t.Fatalf("seeded temperature = %+v", samples[0])
	}
	// 没有种子值的变量必须是「未连接」，不能是 0.0 + Good：后者会让
	// 上电瞬间的 SCADA 显示一个设备从未有过的读数。
	if samples[1].Quality != model.QualityCommError || samples[1].Err == nil {
		t.Fatalf("unseeded setpoint = %+v", samples[1])
	}

	// 未知 NodeId 必须是地址错误，而不是「设备掉线」。
	bad, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{symbolPoint("x", "ns=2;s=plant/line-a/nope", model.TypeFloat64)}}})
	if err == nil {
		t.Fatal("unknown node must fail")
	}
	if bad[0].Quality != model.QualityAddressError {
		t.Fatalf("unknown node quality = %s", bad[0].Quality)
	}
}

// 写入必须真的走到南向：返回 Good 代表设备接受了这次写入。
func TestWriteGoesSouth(t *testing.T) {
	src := &fakeSource{
		specs: []Spec{
			floatSpec("line-a", "setpoint", true),
			floatSpec("line-a", "temperature", false),
		},
	}
	s := startTest(t, src, "plant")
	c := client(t, s)
	ctx := context.Background()

	res, err := c.Write(ctx, []driver.WriteRequest{{
		PointID: "setpoint", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=plant/line-a/setpoint", Index: -1},
		Value:   model.NumValue(model.TypeFloat64, 42.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("write result = %+v", res)
	}
	w, ok := src.lastWrite()
	if !ok || w.Device != "line-a" || w.Point != "setpoint" || w.Value.Num != 42.5 {
		t.Fatalf("southbound write = %+v ok=%v", w, ok)
	}

	// 写入成功后客户端必须读回自己写进去的值。
	samples, _ := c.Read(ctx, []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{symbolPoint("sp", "ns=2;s=plant/line-a/setpoint", model.TypeFloat64)}}})
	if samples[0].Value.Num != 42.5 || samples[0].Quality != model.QualityGood {
		t.Fatalf("read back = %+v", samples[0])
	}

	// 只读点位在服务端就被拒，不能走到设备。
	before := len(src.writes)
	res, err = c.Write(ctx, []driver.WriteRequest{{
		PointID: "temperature", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=plant/line-a/temperature", Index: -1},
		Value:   model.NumValue(model.TypeFloat64, 99),
	}})
	if err != nil {
		t.Fatalf("write read-only: %v", err)
	}
	if res[0].OK {
		t.Fatal("read-only point accepted a write")
	}
	if got := driver.KindOf(res[0].Err); got != driver.ErrPermission {
		t.Fatalf("read-only kind = %s", got)
	}
	src.mu.Lock()
	after := len(src.writes)
	src.mu.Unlock()
	if after != before {
		t.Fatalf("read-only write reached southbound: %d -> %d", before, after)
	}
}

// 设备拒绝写入时，客户端必须看到失败。报 Good 会让 SCADA 显示一个
// 从未生效的设定值——这类错误在现场极难被发现。
func TestWriteRejectionSurfaces(t *testing.T) {
	src := &fakeSource{
		specs:  []Spec{floatSpec("line-a", "setpoint", true)},
		result: Outcome{Code: "device.not_ready", Message: "device is not running"},
	}
	s := startTest(t, src, "plant")
	c := client(t, s)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=plant/line-a/setpoint", Index: -1},
		Value:   model.NumValue(model.TypeFloat64, 42.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res[0].OK || res[0].Err == nil {
		t.Fatalf("rejected write reported success: %+v", res[0])
	}
	if k := driver.KindOf(res[0].Err); k != driver.ErrDisconnected {
		t.Fatalf("kind = %s", k)
	}
}

// 缓存事件要实时进地址空间；质量迁移（stale）也必须推，否则 SCADA 上
// 「设备安静」与「设备掉线」长得一模一样。
func TestPushUpdatesValueAndQuality(t *testing.T) {
	src := &fakeSource{
		specs: []Spec{floatSpec("line-a", "temperature", false)},
		seeds: []Seed{{Device: "line-a", Point: "temperature", Sample: model.Sample{
			DeviceID: "line-a", PointID: "temperature",
			Value: model.NumValue(model.TypeFloat64, 1), Quality: model.QualityGood,
		}}},
	}
	s := startTest(t, src, "plant")
	c := client(t, s)

	src.events <- cache.Event{Type: cache.EventSample, Sample: model.Sample{
		DeviceID: "line-a", PointID: "temperature",
		Value: model.NumValue(model.TypeFloat64, 30.25), Quality: model.QualityGood,
		CollectedAt: time.Now(),
	}}
	read := func() driver.RawSample {
		t.Helper()
		samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
			Points: []driver.PointRef{symbolPoint("t", "ns=2;s=plant/line-a/temperature", model.TypeFloat64)}}})
		if err != nil && samples[0].Err == nil {
			t.Fatalf("read: %v", err)
		}
		return samples[0]
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		smp := read()
		if smp.Value.Num == 30.25 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("value never pushed: %+v", smp)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 质量事件：值不变，质量变 stale。
	deadline = time.Now().Add(2 * time.Second)
	src.events <- cache.Event{Type: cache.EventQuality, Sample: model.Sample{
		DeviceID: "line-a", PointID: "temperature",
		Value: model.NumValue(model.TypeFloat64, 30.25), Quality: model.QualityStale,
		CollectedAt: time.Now(),
	}}
	for {
		smp := read()
		if smp.Quality == model.QualityUncertain {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("quality never pushed: %+v", smp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 推送里出现地址空间里没有的点位（计算点、报警内部点）必须被忽略，
// 而不是创建一个未声明的变量。
func TestPushIgnoresUnknownPoints(t *testing.T) {
	src := &fakeSource{specs: []Spec{floatSpec("line-a", "temperature", false)}}
	s := startTest(t, src, "plant")
	c := client(t, s)

	src.events <- cache.Event{Type: cache.EventSample, Sample: model.Sample{
		DeviceID: "other", PointID: "ghost",
		Value: model.NumValue(model.TypeFloat64, 5), Quality: model.QualityGood,
	}}
	time.Sleep(50 * time.Millisecond)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{symbolPoint("g", "ns=2;s=plant/other/ghost", model.TypeFloat64)}}})
	if err == nil {
		t.Fatal("ghost point appeared in address space")
	}
	if samples[0].Quality != model.QualityAddressError {
		t.Fatalf("ghost quality = %s", samples[0].Quality)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	src := &fakeSource{specs: []Spec{floatSpec("line-a", "temperature", false)}}
	s := startTest(t, src, "plant")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if s.Addr() == "" {
		t.Fatal("addr must stay readable after close")
	}
}
