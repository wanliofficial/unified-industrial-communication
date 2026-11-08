package sparkplug

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	codec "uic/internal/codec/sparkplug"
	"uic/internal/model"
)

func fixedClock(ms int64) func() time.Time {
	t := time.UnixMilli(ms).UTC()
	return func() time.Time { return t }
}

func newTestNode(t *testing.T) *Node {
	t.Helper()
	n := NewNode("plant-a", "gw-01", 7)
	n.SetClock(fixedClock(1700000000000))
	return n
}

// 主题是 Sparkplug 唯一的「信封」：拼错一个斜杠对端就完全看不到数据，
// 而本地不会报任何错。这一组用例把 8 种主题逐字钉死。
func TestTopic(t *testing.T) {
	cases := []struct {
		group, kind, node, device, want string
	}{
		{"g", KindNBIRTH, "n", "", "spBv1.0/g/NBIRTH/n"},
		{"g", KindNDEATH, "n", "", "spBv1.0/g/NDEATH/n"},
		{"g", KindDBIRTH, "n", "d", "spBv1.0/g/DBIRTH/n/d"},
		{"g", KindDDEATH, "n", "d", "spBv1.0/g/DDEATH/n/d"},
		{"g", KindDDATA, "n", "d", "spBv1.0/g/DDATA/n/d"},
		{"g", KindNCMD, "n", "", "spBv1.0/g/NCMD/n"},
		{"g", KindDCMD, "n", "d", "spBv1.0/g/DCMD/n/d"},
		{"g", KindSTATE, "n", "", "spBv1.0/g/STATE/n"},
	}
	for _, c := range cases {
		if got := Topic(c.group, c.kind, c.node, c.device); got != c.want {
			t.Fatalf("Topic(%q,%q,%q,%q) = %q, want %q", c.group, c.kind, c.node, c.device, got, c.want)
		}
		g, k, n, d, ok := ParseTopic(c.want)
		if !ok || g != c.group || k != c.kind || n != c.node || d != c.device {
			t.Fatalf("ParseTopic(%q) = %q,%q,%q,%q,%v", c.want, g, k, n, d, ok)
		}
	}
}

func TestParseTopicRejects(t *testing.T) {
	bad := []string{
		"",
		"spBv1.0",
		"spBv1.0/g",
		"spBv1.0/g/DDATA",
		"SPBV1.0/g/DDATA/n/d",   // 命名空间大小写敏感
		"spBv1.0/g/BOGUS/n/d",   // 未知消息类型
		"spBv1.0/g/DDATA/n/d/x", // 多一层
		"spBv1.0//DDATA/n/d",    // 空 group
		"spBv1.0/g/DDATA//d",    // 空 node
	}
	for _, topic := range bad {
		if _, _, _, _, ok := ParseTopic(topic); ok {
			t.Fatalf("%q accepted", topic)
		}
	}
}

// 序号按作用域独立：节点序号与每个设备的序号互不影响，DBIRTH 把设备序号
// 重置为 0，255 之后回绕。
func TestSequenceScopes(t *testing.T) {
	n := newTestNode(t)

	birth, err := n.NBIRTH()
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, birth); got != 0 {
		t.Fatalf("NBIRTH seq = %d", got)
	}
	// 设备序号与节点序号互相独立；DDATA 用的是设备序号。
	for want := uint64(0); want <= 2; want++ {
		msg, err := n.DDATA("dev-a", sample("p", 1))
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeSeq(t, msg); got != want {
			t.Fatalf("DDATA %d seq = %d", want, got)
		}
	}
	// 另一个设备从 0 开始；DBIRTH 也必须是 0。
	db, err := n.DBIRTH("dev-b", []model.Sample{sample("p", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, db); got != 0 {
		t.Fatalf("DBIRTH seq = %d", got)
	}
	// 同一设备再 DBIRTH 重置回 0：消费方靠这个判定「设备重新出生」。
	if _, err := n.DBIRTH("dev-a", []model.Sample{sample("p", 1)}); err != nil {
		t.Fatal(err)
	}
	msg, err := n.DDATA("dev-a", sample("p", 2))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, msg); got != 1 {
		t.Fatalf("DDATA after rebirth seq = %d, want 1", got)
	}
	// 设备序号不影响节点序号。
	death, err := n.NDEATH()
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, death); got == 0 {
		t.Fatal("NDEATH reused node seq 0")
	}
}

func TestSequenceWrap(t *testing.T) {
	n := newTestNode(t)
	// 直接消费到 255，再要一次必须是 0。
	for i := 0; i < 256; i++ {
		if _, err := n.DDATA("d", sample("p", 1)); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := n.DDATA("d", sample("p", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, msg); got != 0 {
		t.Fatalf("seq after wrap = %d, want 0", got)
	}
}

// NBIRTH 必须带 bdSeq 与 Node Control/Rebirth；DBIRTH 必须带
// Device Control/Rebirth 与设备当前全部点位。
func TestBirthPayloads(t *testing.T) {
	n := newTestNode(t)
	msg, err := n.NBIRTH()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Topic != "spBv1.0/plant-a/NBIRTH/gw-01" || msg.Retain {
		t.Fatalf("NBIRTH = %+v", msg)
	}
	p := decodePayload(t, msg)
	if len(p.Metrics) != 2 {
		t.Fatalf("NBIRTH metrics = %d", len(p.Metrics))
	}
	if p.Metrics[0].Name != "bdSeq" || p.Metrics[0].Value != int64(7) {
		t.Fatalf("bdSeq = %+v", p.Metrics[0])
	}
	if p.Metrics[1].Name != "Node Control/Rebirth" || p.Metrics[1].Value != false {
		t.Fatalf("rebirth = %+v", p.Metrics[1])
	}

	db, err := n.DBIRTH("dev", []model.Sample{sample("a", 1), sample("b", 2)})
	if err != nil {
		t.Fatal(err)
	}
	p = decodePayload(t, db)
	if p.Metrics[0].Name != "Device Control/Rebirth" {
		t.Fatalf("DBIRTH first metric = %q", p.Metrics[0].Name)
	}
	if len(p.Metrics) != 3 || p.Metrics[1].Name != "a" || p.Metrics[2].Name != "b" {
		t.Fatalf("DBIRTH metrics = %+v", p.Metrics)
	}
}

// STATE 是 JSON 而不是 Protobuf，而且必须 retained。
func TestState(t *testing.T) {
	n := newTestNode(t)
	for _, online := range []bool{true, false} {
		msg, err := n.State(online)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Topic != "spBv1.0/plant-a/STATE/gw-01" || !msg.Retain {
			t.Fatalf("STATE = %+v", msg)
		}
		var got struct {
			Timestamp int64 `json:"timestamp"`
			Online    bool  `json:"online"`
			BdSeq     int   `json:"bdSeq"`
		}
		if err := json.Unmarshal(msg.Payload, &got); err != nil {
			t.Fatalf("STATE payload %q: %v", msg.Payload, err)
		}
		if got.Online != online || got.BdSeq != 7 || got.Timestamp != 1700000000000 {
			t.Fatalf("STATE = %+v", got)
		}
	}
}

func TestSubscriptions(t *testing.T) {
	n := newTestNode(t)
	subs := n.Subscriptions()
	want := []string{"spBv1.0/plant-a/NCMD/gw-01", "spBv1.0/plant-a/DCMD/gw-01/+"}
	if len(subs) != len(want) {
		t.Fatalf("subs = %v", subs)
	}
	for i := range want {
		if subs[i] != want[i] {
			t.Fatalf("subs = %v, want %v", subs, want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	mk := func(names ...codec.Metric) []byte {
		raw, err := codec.Payload{Metrics: names}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	on := codec.Metric{Name: "Node Control/Rebirth", DataType: codec.TypeBoolean, Value: true}
	off := codec.Metric{Name: "Node Control/Rebirth", DataType: codec.TypeBoolean, Value: false}

	c, ok := ParseCommand("spBv1.0/g/NCMD/n", mk(on))
	if !ok || !c.Rebirth || c.Device != "" {
		t.Fatalf("NCMD rebirth = %+v %v", c, ok)
	}
	// Rebirth=false 是合法命令，但不需要动作：返回 ok=false 更省事，
	// 也不会被误当成「要求重生」。
	if _, ok := ParseCommand("spBv1.0/g/NCMD/n", mk(off)); ok {
		t.Fatal("rebirth=false accepted as action")
	}
	dc := codec.Metric{Name: "Device Control/Rebirth", DataType: codec.TypeBoolean, Value: true}
	c, ok = ParseCommand("spBv1.0/g/DCMD/n/dev-1", mk(dc))
	if !ok || !c.Rebirth || c.Device != "dev-1" {
		t.Fatalf("DCMD rebirth = %+v %v", c, ok)
	}
	// 节点命令里的设备命令、设备命令里的节点命令都不应触发。
	if _, ok := ParseCommand("spBv1.0/g/NCMD/n", mk(dc)); ok {
		t.Fatal("DCMD metric on NCMD accepted")
	}
	if _, ok := ParseCommand("spBv1.0/g/DCMD/n/d", mk(on)); ok {
		t.Fatal("NCMD metric on DCMD accepted")
	}
	// 非命令主题、坏载荷、未知指标全部静默忽略。
	if _, ok := ParseCommand("spBv1.0/g/DDATA/n/d", mk(on)); ok {
		t.Fatal("DDATA accepted as command")
	}
	if _, ok := ParseCommand("spBv1.0/g/NCMD/n", []byte{0xFF}); ok {
		t.Fatal("garbage accepted")
	}
	if _, ok := ParseCommand("spBv1.0/g/NCMD/n", mk(codec.Metric{Name: "x", DataType: codec.TypeBoolean, Value: true})); ok {
		t.Fatal("unknown metric accepted")
	}
}

// 质量非 good 用 is_null 表达：把断线前的旧值当成新值发出去，比不发更危险。
func TestMetricForSampleQuality(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	s := sample("p", 1)
	s.Quality = model.QualityStale
	s.Unit = "℃"
	m := MetricForSample(s, now)
	if !m.IsNull || m.Value != nil {
		t.Fatalf("stale metric = %+v", m)
	}
	if m.Properties["quality"] != "stale" || m.Properties["engUnit"] != "℃" {
		t.Fatalf("properties = %v", m.Properties)
	}
	if !m.Timestamp.Equal(s.CollectedAt) {
		t.Fatalf("timestamp = %v", m.Timestamp)
	}

	bad := sample("p", 1)
	bad.Quality = model.QualityBad
	m = MetricForSample(bad, now)
	if !m.IsNull || m.Properties["quality"] != "bad" {
		t.Fatalf("bad metric = %+v", m)
	}

	good := sample("p", 1)
	good.Unit = "℃"
	m = MetricForSample(good, now)
	if m.IsNull || m.Properties["quality"] != "" || m.Properties["engUnit"] != "℃" {
		t.Fatalf("good metric = %+v", m)
	}
}

func TestMetricForSampleTypes(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		v    model.Value
		dt   codec.DataType
		want any
	}{
		{model.BoolValue(true), codec.TypeBoolean, true},
		{model.NumValue(model.TypeInt16, -5), codec.TypeInt16, int64(-5)},
		{model.NumValue(model.TypeInt32, -5), codec.TypeInt32, int64(-5)},
		{model.NumValue(model.TypeInt64, -5), codec.TypeInt64, int64(-5)},
		{model.NumValue(model.TypeUint16, 5), codec.TypeUInt16, uint64(5)},
		{model.NumValue(model.TypeUint32, 5), codec.TypeUInt32, uint64(5)},
		{model.NumValue(model.TypeUint64, 5), codec.TypeUInt64, uint64(5)},
		{model.NumValue(model.TypeFloat32, 1.5), codec.TypeFloat, float32(1.5)},
		{model.NumValue(model.TypeFloat64, 1.5), codec.TypeDouble, 1.5},
		{model.TextValue(model.TypeString, "x"), codec.TypeString, "x"},
		{model.BytesValue([]byte{1, 2}), codec.TypeBytes, []byte{1, 2}},
		// 平台内有 BCD 这类数值语义类型，Sparkplug 没有对应枚举，按
		// double 发出去，而不是塞一个空指标。
		{model.NumValue(model.TypeBCD16, 12), codec.TypeDouble, 12.0},
	}
	for _, c := range cases {
		s := model.Sample{DeviceID: "d", PointID: "p", Value: c.v, Quality: model.QualityGood, CollectedAt: now}
		m := MetricForSample(s, now)
		if m.DataType != c.dt {
			t.Fatalf("%v datatype = %v, want %v", c.v.Type, m.DataType, c.dt)
		}
		if c.dt == codec.TypeBytes || c.dt == codec.TypeString {
			if got, ok := m.Value.([]byte); ok {
				if string(got) != string(c.want.([]byte)) {
					t.Fatalf("%v value = %v", c.v.Type, m.Value)
				}
			} else if m.Value != c.want {
				t.Fatalf("%v value = %v", c.v.Type, m.Value)
			}
			continue
		}
		if m.Value != c.want {
			t.Fatalf("%v value = %#v (%T), want %#v (%T)", c.v.Type, m.Value, m.Value, c.want, c.want)
		}
	}

	// 编码器必须接受这些映射：类型校验在 Encode 里，单测不跑一遍就等于
	// 没验证。
	samples := make([]model.Sample, 0, len(cases))
	for i, c := range cases {
		samples = append(samples, model.Sample{
			DeviceID: "d", PointID: "p" + strings.Repeat("x", i),
			Value: c.v, Quality: model.QualityGood, CollectedAt: now,
		})
	}
	n := NewNode("g", "n", 0)
	n.SetClock(func() time.Time { return now })
	msg, err := n.DBIRTH("d", samples)
	if err != nil {
		t.Fatalf("DBIRTH with mapped types: %v", err)
	}
	if _, err := codec.Decode(msg.Payload); err != nil {
		t.Fatalf("decode DBIRTH: %v", err)
	}
}

func TestDDEATHHasNoMetrics(t *testing.T) {
	n := newTestNode(t)
	msg, err := n.DDEATH("dev")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Topic != "spBv1.0/plant-a/DDEATH/gw-01/dev" {
		t.Fatalf("topic = %q", msg.Topic)
	}
	if p := decodePayload(t, msg); len(p.Metrics) != 0 {
		t.Fatalf("DDEATH metrics = %+v", p.Metrics)
	}
}

func sample(point string, v float64) model.Sample {
	return model.Sample{
		DeviceID:    "dev",
		PointID:     point,
		Value:       model.NumValue(model.TypeFloat64, v),
		Quality:     model.QualityGood,
		CollectedAt: time.UnixMilli(1700000000000).UTC(),
	}
}

func decodePayload(t *testing.T, msg Message) codec.Payload {
	t.Helper()
	p, err := codec.Decode(msg.Payload)
	if err != nil {
		t.Fatalf("%s decode: %v", msg.Topic, err)
	}
	return p
}

func decodeSeq(t *testing.T, msg Message) uint64 {
	t.Helper()
	return decodePayload(t, msg).Seq
}
