package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	codec "uic/internal/codec/sparkplug"
	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/mqtt/mqtttest"
	"uic/internal/outbox"
	"uic/internal/scheduler"
)

// withSparkplug 把北向配置成 Sparkplug B。
func withSparkplug(url string, mutate func(map[string]any)) func(map[string]any) {
	return func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		m := map[string]any{
			"enabled": true, "broker": url, "qos": 1,
			"format": "sparkplug-b", "groupId": "plant-a", "nodeId": "gw-01",
		}
		if mutate != nil {
			mutate(m)
		}
		cfg["northbound"] = map[string]any{"mqtt": m}
	}
}

// waitTopic 等到指定主题的消息出现，返回最后一条。
func waitTopic(t *testing.T, b *mqtttest.Broker, topic string) mqtttest.Message {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last mqtttest.Message
	for time.Now().Before(deadline) {
		for _, m := range b.Messages() {
			if m.Topic == topic {
				last = m
			}
		}
		if last.Topic != "" {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no message on %s; got topics %v", topic, topicsOf(b.Messages()))
	return last
}

func topicsOf(msgs []mqtttest.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Topic)
	}
	return out
}

func countTopics(msgs []mqtttest.Message, topic string) int {
	n := 0
	for _, m := range msgs {
		if m.Topic == topic {
			n++
		}
	}
	return n
}

func decodeMsg(t *testing.T, m mqtttest.Message) codec.Payload {
	t.Helper()
	p, err := codec.Decode(m.Payload)
	if err != nil {
		t.Fatalf("%s: %v", m.Topic, err)
	}
	return p
}

// 端到端：采集 → Outbox → Sparkplug。顺序必须是
// STATE(online) → NBIRTH → DBIRTH → DDATA，序号在同一设备内连续。
func TestSparkplugBirthOrderAndData(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withSparkplug(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	state := waitTopic(t, b, "spBv1.0/plant-a/STATE/gw-01")
	if !state.Retain {
		t.Fatal("STATE must be retained")
	}
	var st struct {
		Online bool `json:"online"`
		BdSeq  int  `json:"bdSeq"`
	}
	if err := json.Unmarshal(state.Payload, &st); err != nil {
		t.Fatalf("STATE: %v", err)
	}
	if !st.Online {
		t.Fatal("STATE must report online=true")
	}

	subs := b.WaitSubscriptions(t, 2)
	want := map[string]bool{
		"spBv1.0/plant-a/NCMD/gw-01":   false,
		"spBv1.0/plant-a/DCMD/gw-01/+": false,
	}
	for _, s := range subs {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Fatalf("missing subscription %s in %v", s, subs)
		}
	}

	birth := waitTopic(t, b, "spBv1.0/plant-a/NBIRTH/gw-01")
	p := decodeMsg(t, birth)
	if p.Seq != 0 {
		t.Fatalf("NBIRTH seq = %d", p.Seq)
	}
	if len(p.Metrics) < 2 || p.Metrics[0].Name != "bdSeq" {
		t.Fatalf("NBIRTH metrics = %+v", p.Metrics)
	}
	if bd, ok := p.Metrics[0].Value.(int64); !ok || bd != int64(st.BdSeq) {
		t.Fatalf("bdSeq = %v, STATE says %d", p.Metrics[0].Value, st.BdSeq)
	}
	var rebirth bool
	for _, m := range p.Metrics {
		if m.Name == "Node Control/Rebirth" {
			rebirth = true
		}
	}
	if !rebirth {
		t.Fatal("NBIRTH must declare Node Control/Rebirth")
	}

	data := waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/sim01")
	dp := decodeMsg(t, data)
	if len(dp.Metrics) != 1 || dp.Metrics[0].Name == "" {
		t.Fatalf("DDATA metrics = %+v", dp.Metrics)
	}

	// DBIRTH 是设备的完整点位表，不能只带第一个点。等所有点位都出现过。
	deadline := time.Now().Add(10 * time.Second)
	var dbirth codec.Payload
	for time.Now().Before(deadline) {
		for _, m := range b.Messages() {
			if m.Topic != "spBv1.0/plant-a/DBIRTH/gw-01/sim01" {
				continue
			}
			p := decodeMsg(t, m)
			if len(p.Metrics) > len(dbirth.Metrics) {
				dbirth = p
			}
		}
		if len(dbirth.Metrics) >= 8 { // Device Control/Rebirth + 7 点
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(dbirth.Metrics) < 8 {
		t.Fatalf("DBIRTH metrics = %d, want the whole device point table", len(dbirth.Metrics))
	}
	if dbirth.Metrics[0].Name != "Device Control/Rebirth" {
		t.Fatalf("DBIRTH first metric = %q", dbirth.Metrics[0].Name)
	}
}

// 设备进入 faulted 必须发 DDEATH，并在恢复后重新 DBIRTH。为了不依赖
// 调制解调器的故障注入时序，这里直接驱动 sink。
func TestSparkplugDeviceDeath(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateCollecting, To: scheduler.StateFaulted,
		At: time.Now(),
	})
	death := waitTopic(t, b, "spBv1.0/plant-a/DDEATH/gw-01/dev1")
	if p := decodeMsg(t, death); len(p.Metrics) != 0 {
		t.Fatalf("DDEATH metrics = %+v", p.Metrics)
	}

	// 恢复采集：第一条数据必须先 DBIRTH，再 DDATA。
	before := len(b.Messages())
	deliverSample(t, s, "dev1", "p1", 2)
	msgs := waitAfter(t, b, before, 2)
	var order []string
	for _, m := range msgs {
		if strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DBIRTH/gw-01/dev1") {
			order = append(order, "DBIRTH")
		}
		if strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DDATA/gw-01/dev1") {
			order = append(order, "DDATA")
		}
	}
	if len(order) < 2 || order[0] != "DBIRTH" || order[1] != "DDATA" {
		t.Fatalf("rebirth order = %v", order)
	}
}

// NCMD 的 Node Control/Rebirth 必须重发 NBIRTH（以及已知设备的 DBIRTH）。
func TestSparkplugNodeRebirthCommand(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	b.WaitMessages(t, 1)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")

	raw, err := (codec.Payload{Metrics: []codec.Metric{{
		Name: "Node Control/Rebirth", DataType: codec.TypeBoolean, Value: true,
	}}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	b.Publish("spBv1.0/plant-a/NCMD/gw-01", raw, 1, false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countTopics(b.Messages(), "spBv1.0/plant-a/NBIRTH/gw-01") >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no second NBIRTH after NCMD; topics = %v", topicsOf(b.Messages()))
}

// 连接断开后（Broker 重启 / 网络抖动），下一条数据必须先补 BIRTH。
// 这条路径覆盖「OnConnect 是异步的」这一现实：不能假设它先于投递执行。
func TestSparkplugReconnectRebirths(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/NBIRTH/gw-01")
	first := countTopics(b.Messages(), "spBv1.0/plant-a/NBIRTH/gw-01")

	b.DropAll()
	// 客户端要一点时间才能从读循环发现断链。失败是预期：Outbox 会重试。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := s.deliver(ctx, []outbox.Record{recordFor(t, "dev1", "p1", 2)})
		cancel()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if countTopics(b.Messages(), "spBv1.0/plant-a/NBIRTH/gw-01") > first {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no NBIRTH after reconnect; topics = %v", topicsOf(b.Messages()))
}

// close 必须留下「已下线」的 STATE，否则消费方会一直看到 online=true。
func TestSparkplugCloseLeavesOfflineState(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/STATE/gw-01")
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var last mqtttest.Message
	for _, m := range b.Messages() {
		if m.Topic == "spBv1.0/plant-a/STATE/gw-01" {
			last = m
		}
	}
	if last.Topic == "" {
		t.Fatal("no STATE published")
	}
	var st struct {
		Online bool `json:"online"`
	}
	if err := json.Unmarshal(last.Payload, &st); err != nil {
		t.Fatalf("STATE: %v", err)
	}
	if st.Online {
		t.Fatal("final STATE must be online=false")
	}
	// NDEATH 也必须发出去（正常停机时 Broker 不会代发遗嘱）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countTopics(b.Messages(), "spBv1.0/plant-a/NDEATH/gw-01") > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no NDEATH on close; topics = %v", topicsOf(b.Messages()))
}

func newTestSparkplugSink(t *testing.T, url string) *sparkplugSink {
	t.Helper()
	qos := 1
	s, err := newSparkplugSink(config.MQTT{
		Enabled: true, Broker: url, QoS: &qos,
		Format: config.MQTTFormatSparkplugB, GroupID: "plant-a", NodeID: "gw-01",
	})
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	return s
}

func recordFor(t *testing.T, device, point string, v float64) outbox.Record {
	t.Helper()
	payload, err := json.Marshal(eventPayload{SchemaVersion: 1, Sample: model.Sample{
		DeviceID: device, PointID: point,
		Value: model.NumValue(model.TypeFloat64, v), Quality: model.QualityGood,
		CollectedAt: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.Record{Sink: "mqtt", Key: device + "/" + point, Data: payload, Ts: time.Now()}
}

func deliverSample(t *testing.T, s *sparkplugSink, device, point string, v float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.deliver(ctx, []outbox.Record{recordFor(t, device, point, v)}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

// waitAfter 等到消息数超过 before 至少 n 条，返回新增部分。
func waitAfter(t *testing.T, b *mqtttest.Broker, before, n int) []mqtttest.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := b.Messages()
		if len(msgs) >= before+n {
			return msgs[before:]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d messages, want %d more", len(b.Messages()), n)
	return nil
}
