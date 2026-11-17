package sparkplug

import (
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
	n := NewNode("plant-a", "gw-01")
	n.SetClock(fixedClock(1700000000000))
	// 真实调用点（MQTT 客户端构造 CONNECT 时）一定会先 BeginSession，
	// 单测也必须照做，否则 NBIRTH 里的 bdSeq 是「没上过线的 0」，
	// 与「刚上线过一次的 0」看起来一样，测不出状态机是否推进。
	n.BeginSession()
	return n
}

// 主题是 Sparkplug 唯一的「信封」：拼错一个斜杠对端就完全看不到数据，
// 而本地不会报任何错。这一组用例把 Edge Node 的 7 种主题逐字钉死。
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

// STATE 属于 Host Application，不属于 Edge Node。
//
// 本包刻意不产生 STATE：它是 Host 的出生/死亡证书，主题只有
// spBv1.0/STATE/{host_id}（4 段、没有 group 段）。边缘节点发它就是冒用
// 一个自己没有的身份，真正的 Host 会据此认为「有个 Host 在线」。所以
// ParseTopic 必须把它当成「不认识的主题」，而不是解析出一个 group。
func TestStateTopicIsNotEdgeNodeSyntax(t *testing.T) {
	for _, topic := range []string{
		"spBv1.0/STATE/host-1",
		"spBv1.0/g/STATE/n",
	} {
		if _, _, _, _, ok := ParseTopic(topic); ok {
			t.Fatalf("%q parsed as an edge-node topic; STATE belongs to the Host", topic)
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

// 序号是**边缘节点级**的：节点自己的消息与它所有设备的消息共用同一个
// 0..255 计数器（规范对每种消息都写「one greater than the previous MQTT
// message from the Edge Node」）。只有 NBIRTH 把它重置为 0。
//
// 这条用例同时钉住两个容易写错的地方：
//   - 设备消息不能各自从 0 开始（两台设备交替时会变成 0,0,1,1）；
//   - NDEATH 不带序号，也不消耗序号。
func TestSequenceIsPerEdgeNode(t *testing.T) {
	n := newTestNode(t)

	birth, err := n.NBIRTH()
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, birth); got != 0 {
		t.Fatalf("NBIRTH seq = %d", got)
	}
	// 节点出生之后，设备消息接着节点序号走，不从 0 重新开始。
	for want := uint64(1); want <= 3; want++ {
		msg, err := n.DDATA("dev-a", sample("p", 1))
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeSeq(t, msg); got != want {
			t.Fatalf("DDATA %d seq = %d", want, got)
		}
	}
	// 另一台设备的 DBIRTH 继续同一个计数器，不重置。
	db, err := n.DBIRTH("dev-b", []model.Sample{sample("p", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, db); got != 4 {
		t.Fatalf("DBIRTH seq = %d, want 4 (node-wide counter)", got)
	}
	// 设备重生同样不重置序号。
	if _, err := n.DBIRTH("dev-a", []model.Sample{sample("p", 1)}); err != nil {
		t.Fatal(err)
	}
	msg, err := n.DDATA("dev-a", sample("p", 2))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, msg); got != 6 {
		t.Fatalf("DDATA after DBIRTH seq = %d, want 6", got)
	}
	// DDEATH 也是序列里的一条。
	death, err := n.DDEATH("dev-b")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, death); got != 7 {
		t.Fatalf("DDEATH seq = %d, want 7", got)
	}

	// 再发一次 NBIRTH（重生）：计数器回到 0，下一条是 1。
	if _, err := n.NBIRTH(); err != nil {
		t.Fatal(err)
	}
	after, err := n.DDATA("dev-a", sample("p", 3))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, after); got != 1 {
		t.Fatalf("DDATA after rebirth seq = %d, want 1", got)
	}
}

// NDEATH 必须不带序号，也不能消耗序号。
func TestNDEATHHasNoSeq(t *testing.T) {
	n := newTestNode(t)
	if _, err := n.NBIRTH(); err != nil { // seq 0
		t.Fatal(err)
	}
	if _, err := n.NBIRTH(); err != nil { // 重置后又是 seq 0
		t.Fatal(err)
	}
	death, err := n.NDEATH()
	if err != nil {
		t.Fatal(err)
	}
	if p := decodePayload(t, death); p.HasSeq {
		t.Fatalf("NDEATH carries seq=%d; spec: NDEATH MUST NOT include a sequence number", p.Seq)
	}
	// NDEATH 没有消耗序号：下一条仍是 NBIRTH 之后的 1。
	msg, err := n.DDATA("d", sample("p", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSeq(t, msg); got != 1 {
		t.Fatalf("seq after NDEATH = %d, want 1 (NDEATH must not consume a seq)", got)
	}
}

func TestSequenceWrap(t *testing.T) {
	n := newTestNode(t)
	if _, err := n.NBIRTH(); err != nil { // 占用 0
		t.Fatal(err)
	}
	// 从 1 连续发到 255，下一条必须回到 0。
	for want := uint64(1); want <= 255; want++ {
		msg, err := n.DDATA("d", sample("p", 1))
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeSeq(t, msg); got != want {
			t.Fatalf("seq = %d, want %d", got, want)
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
	// retain 恒为 false：Message 里没有这个字段，这里顺带断言 QoS 也由
	// 消息类型定死（出生消息必须是 QoS 0）。
	if msg.Topic != "spBv1.0/plant-a/NBIRTH/gw-01" || msg.QoS != 0 {
		t.Fatalf("NBIRTH = %s qos=%d", msg.Topic, msg.QoS)
	}
	p := decodePayload(t, msg)
	if len(p.Metrics) != 2 {
		t.Fatalf("NBIRTH metrics = %d", len(p.Metrics))
	}
	if p.Metrics[0].Name != "bdSeq" || p.Metrics[0].Value != int64(0) {
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

// bdSeq 是「每个新 CONNECT +1、0 开始、255 后回绕 0」，并且必须同时出现在
// CONNECT 的遗嘱载荷与随后的 NBIRTH 里（[tck-id-payloads-ndeath-bdseq]）。
//
// 这条用例同时钉住三个容易写错的地方：
//   - bdSeq 不能是进程级常量（重连后 NDEATH/NBIRTH 会一直停在同一个值）；
//   - BeginSession 必须同时推进与记录，否则 NDEATH 与 NBIRTH 用的是两个值，
//     Host 无法把这对消息配起来；
//   - 回绕必须落在 0 而不是越界到 256。
func TestBdSeqPerSession(t *testing.T) {
	n := NewNode("g", "n")
	n.SetClock(fixedClock(1700000000000))

	// 未上线时 BdSeq 是 0，这只表示「还没有会话」，不能与「第一次会话的
	// bdSeq=0」混淆——所以真正的断言全靠下面的 BeginSession 返回值。
	if got := n.BdSeq(); got != 0 {
		t.Fatalf("initial bdSeq = %d", got)
	}

	for want := uint64(0); want < 3; want++ {
		got := n.BeginSession()
		if got != want {
			t.Fatalf("BeginSession #%d = %d, want %d", want, got, want)
		}
		if n.BdSeq() != want {
			t.Fatalf("BdSeq after session %d = %d", want, n.BdSeq())
		}
		// 同一次会话里的 NDEATH 与 NBIRTH 必须是同一个值。
		death, err := n.NDEATH()
		if err != nil {
			t.Fatal(err)
		}
		dp := decodePayload(t, death)
		if len(dp.Metrics) != 1 || dp.Metrics[0].Name != "bdSeq" {
			t.Fatalf("NDEATH metrics = %+v", dp.Metrics)
		}
		if dp.Metrics[0].Value != int64(want) {
			t.Fatalf("NDEATH bdSeq = %v, want %d", dp.Metrics[0].Value, want)
		}

		birth, err := n.NBIRTH()
		if err != nil {
			t.Fatal(err)
		}
		bp := decodePayload(t, birth)
		if bp.Metrics[0].Name != "bdSeq" || bp.Metrics[0].Value != int64(want) {
			t.Fatalf("NBIRTH bdSeq = %+v, want %d", bp.Metrics[0], want)
		}
	}
}

// bdSeq 回绕：255 之后是 0，不能变成 256 或停在 255。
func TestBdSeqWrap(t *testing.T) {
	n := NewNode("g", "n")
	for want := uint64(0); want <= 255; want++ {
		if got := n.BeginSession(); got != want {
			t.Fatalf("session %d bdSeq = %d", want, got)
		}
	}
	if got := n.BeginSession(); got != 0 {
		t.Fatalf("bdSeq after 255 = %d, want 0", got)
	}
}

// 每类消息的 QoS 由规范按类型定死，不是配置项。
//
// 数据与出生消息 QoS 0；NDEATH 的遗嘱要求 QoS 1，断开前的主动发布沿用
// 同一个值。把它做成可配，就等于允许用户配出一个「本地看起来正常、
// 严格 Host 直接丢弃」的部署。
func TestQoSByMessageKind(t *testing.T) {
	n := newTestNode(t)

	born, err := n.NBIRTH()
	if err != nil {
		t.Fatal(err)
	}
	device, err := n.DBIRTH("dev", []model.Sample{sample("p", 1)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := n.DDATA("dev", sample("p", 2))
	if err != nil {
		t.Fatal(err)
	}
	deviceDeath, err := n.DDEATH("dev")
	if err != nil {
		t.Fatal(err)
	}
	death, err := n.NDEATH()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		msg  Message
		want byte
	}{
		{"NBIRTH", born, 0},
		{"DBIRTH", device, 0},
		{"DDATA", data, 0},
		{"DDEATH", deviceDeath, 0},
		// 遗嘱本身要求 QoS1；主动发布与遗嘱必须同值，否则「主动告知的
		// 死亡」反而比「Broker 代发的遗嘱」更容易丢。
		{"NDEATH", death, 1},
	} {
		if c.msg.QoS != c.want {
			t.Fatalf("%s qos = %d, want %d", c.name, c.msg.QoS, c.want)
		}
	}
	// 订阅命令一律 QoS1：QoS0 会在链路抖动时静默丢掉重生请求。
	if SubscribeQoS != 1 {
		t.Fatalf("SubscribeQoS = %d", SubscribeQoS)
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

// 命令载荷按规范不带序号（[tck-id-payloads-ncmd-seq]/[tck-id-payloads-dcmd-seq]），
// 这一层只解析不发送，所以夹具要按真实主机的样子构造。
func TestParseCommand(t *testing.T) {
	mk := func(names ...codec.Metric) []byte {
		raw, err := codec.Payload{Metrics: names, OmitSeq: true}.Encode()
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
	n := NewNode("g", "n")
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
