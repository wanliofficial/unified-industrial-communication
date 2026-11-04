package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/mqtt"
	"uic/internal/mqtt/mqtttest"
	"uic/internal/scheduler"
)

// withMQTT 把北向配置成只启用 MQTT。
func withMQTT(url string, mutate func(map[string]any)) func(map[string]any) {
	return func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		m := map[string]any{"enabled": true, "broker": url, "qos": 1}
		if mutate != nil {
			mutate(m)
		}
		cfg["northbound"] = map[string]any{"mqtt": m}
	}
}

// 端到端：采集 → Outbox → MQTT → Broker 收到。
// 同时验证主题渲染、QoS，以及载荷与 REST 是同一契约。
func TestMQTTSinkDelivers(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTT(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	// write_me 是可写点，写入会产生可预测的变化事件。
	for _, v := range []int{11, 22, 33} {
		body := `{"writes":[{"point":"write_me","value":{"type":"uint16","num":` +
			itoa(v) + `},"confirm":true}]}`
		if code, resp := h.do(http.MethodPost, "/api/v1/points/sim01/write", body); code != 200 {
			t.Fatalf("write = %d %s", code, resp)
		}
		time.Sleep(40 * time.Millisecond)
	}

	b.WaitMessages(t, 1)
	wantPrefix := "uic/test-site/sim01/"
	var data []mqtttest.Message
	for _, m := range b.Messages() {
		// 同一主题树下还有 $birth/$state，这里只看采样值。
		if !strings.HasSuffix(m.Topic, "/value") {
			continue
		}
		data = append(data, m)
		if !strings.HasPrefix(m.Topic, wantPrefix) {
			t.Fatalf("topic = %q, want prefix %q", m.Topic, wantPrefix)
		}
		if m.QoS != 1 {
			t.Fatalf("qos = %d, want 1", m.QoS)
		}
	}
	if len(data) == 0 {
		t.Fatal("no /value message published")
	}

	var payload struct {
		SchemaVersion int    `json:"schemaVersion"`
		DeviceID      string `json:"device"`
		PointID       string `json:"point"`
		Quality       string `json:"quality"`
	}
	if err := json.Unmarshal(data[0].Payload, &payload); err != nil {
		t.Fatalf("payload not json: %v (%s)", err, data[0].Payload)
	}
	if payload.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d", payload.SchemaVersion)
	}
	if payload.DeviceID != "sim01" || payload.PointID == "" {
		t.Fatalf("payload = %+v", payload)
	}
}

// Broker 不回 PUBACK 时，Outbox 位点绝不能推进——否则那条数据永久丢失。
func TestMQTTSinkKeepsDataWhenBrokerSilent(t *testing.T) {
	b := mqtttest.New(t, func(b *mqtttest.Broker) { b.AckPublish = false })
	h := newHarness(t, withMQTT(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	body := `{"writes":[{"point":"write_me","value":{"type":"uint16","num":77},"confirm":true}]}`
	if code, resp := h.do(http.MethodPost, "/api/v1/points/sim01/write", body); code != 200 {
		t.Fatalf("write = %d %s", code, resp)
	}

	b.WaitMessages(t, 1)
	time.Sleep(500 * time.Millisecond)

	pending := int64(0)
	for _, st := range h.rt.box.Stats() {
		if st.Name == "mqtt" {
			pending = st.Pending
		}
	}
	if pending == 0 {
		t.Fatal("outbox advanced past an unacked publish: data would be lost")
	}
}

// Broker 恢复后，积压记录必须自动补投（断网不丢数据）。
func TestMQTTSinkResumesAfterBrokerComesBack(t *testing.T) {
	b := mqtttest.New(t, func(b *mqtttest.Broker) { b.AckPublish = false })
	h := newHarness(t, withMQTT(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	if code, resp := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":88},"confirm":true}]}`); code != 200 {
		t.Fatalf("write = %d %s", code, resp)
	}
	b.WaitMessages(t, 1)

	// 恢复确认能力，模拟 Broker 恢复正常。
	b.SetAckPublish(true)
	before := len(b.Messages())

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if len(b.Messages()) > before {
			return // 积压记录被重投了
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("queued records were never redelivered after broker recovered")
}

// mqtt.enabled 但没有 broker：配置校验必须拒绝。
func TestMQTTEnabledWithoutBrokerRejected(t *testing.T) {
	f := config.File{}
	f.SchemaVersion = config.SchemaVersion
	f.Site = "site-a"
	f.Channels = []model.Channel{{ID: "ch", Protocol: "virtual",
		Transport: model.Transport{Kind: "memory"}}}
	f.Northbound.MQTT.Enabled = true
	if err := f.Validate(); err == nil {
		t.Fatal("mqtt.enabled without broker must be rejected")
	} else if !strings.Contains(err.Error(), "broker") {
		t.Fatalf("err = %v", err)
	}
}

// QoS2 必须显式拒绝，不能静默降级成 1。
func TestMQTTQoS2Rejected(t *testing.T) {
	two := 2
	f := config.File{}
	f.SchemaVersion = config.SchemaVersion
	f.Site = "site-a"
	f.Channels = []model.Channel{{ID: "ch", Protocol: "virtual",
		Transport: model.Transport{Kind: "memory"}}}
	f.Northbound.MQTT = config.MQTT{Enabled: true, Broker: "127.0.0.1:1883", QoS: &two}
	if err := f.Validate(); err == nil {
		t.Fatal("qos 2 must be rejected")
	}
}

// 主题模板缺少 {device}/{point} 时必须拒绝：否则所有点位会挤到一个主题上。
func TestMQTTTopicTemplateValidated(t *testing.T) {
	f := config.File{}
	f.SchemaVersion = config.SchemaVersion
	f.Site = "site-a"
	f.Channels = []model.Channel{{ID: "ch", Protocol: "virtual",
		Transport: model.Transport{Kind: "memory"}}}
	f.Northbound.MQTT = config.MQTT{Enabled: true, Broker: "127.0.0.1:1883", Topic: "static"}
	if err := f.Validate(); err == nil {
		t.Fatal("topic without {device}/{point} must be rejected")
	}
}

// 主题渲染：{site}/{device}/{point} 必须逐段替换，且层级分隔符不能从
// 数据里"漏"进来——一个含 '/' 的设备 ID 会让消息悄悄跑到别人的主题上。
func TestMQTTTopicRendering(t *testing.T) {
	s := &mqttSink{topicTemplate: "uic/{site}/{device}/{point}/value", site: "plant-a"}
	cases := []struct{ device, point, want string }{
		{"dev1", "temp", "uic/plant-a/dev1/temp/value"},
		{"a/b", "c+d", "uic/plant-a/a_b/c_d/value"},
		{"#", "x", "uic/plant-a/_/x/value"},
	}
	for _, c := range cases {
		if got := s.topicFor(c.device, c.point); got != c.want {
			t.Fatalf("%q/%q -> %q, want %q", c.device, c.point, got, c.want)
		}
	}
}

func TestSanitizeTopicLevel(t *testing.T) {
	cases := map[string]string{
		"":      "_",
		"ok":    "ok",
		"a/b":   "a_b",
		"a+b":   "a_b",
		"a#b":   "a_b",
		"中文-01": "中文-01",
	}
	for in, want := range cases {
		if got := sanitizeTopicLevel(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSplitKey(t *testing.T) {
	cases := []struct{ key, device, point string }{
		{"dev1/temp", "dev1", "temp"},
		{"dev1/a/b", "dev1", "a/b"},
		{"dev1", "dev1", "_"},
	}
	for _, c := range cases {
		d, p := splitKey(c.key)
		if d != c.device || p != c.point {
			t.Fatalf("%q -> %q/%q, want %q/%q", c.key, d, p, c.device, c.point)
		}
	}
}

// 设备状态必须是 retained 的持久事实：消费方刚连上就要能读到，而不是
// 等下一次状态变化。
func TestMQTTStateIsRetained(t *testing.T) {
	b := mqtttest.New(t, nil)
	qos := 1
	s, err := newMQTTSink(config.MQTT{
		Enabled: true, Broker: b.URL(), QoS: &qos,
	}, "plant-a", "uic-plant-a")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer s.close()

	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateCollecting, To: scheduler.StateFaulted,
		At: time.Now(), Error: "read timeout",
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range b.Messages() {
			if m.Topic != "uic/plant-a/dev1/$state" {
				continue
			}
			if !m.Retain {
				t.Fatal("$state must be retained")
			}
			var payload struct {
				SchemaVersion int    `json:"schemaVersion"`
				State         string `json:"state"`
				Previous      string `json:"previous"`
				Error         string `json:"error"`
			}
			if err := json.Unmarshal(m.Payload, &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if payload.SchemaVersion != 1 || payload.State != "faulted" || payload.Previous != "collecting" {
				t.Fatalf("payload = %+v", payload)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no $state message published")
}

// republish 必须同时补发 birth 与全部已知设备状态：Broker 重启会清空
// retained 消息，不补发就意味着消费方永远停在旧状态。
func TestMQTTRepublishPublishesBirthAndState(t *testing.T) {
	b := mqtttest.New(t, nil)
	s, err := newMQTTSink(config.MQTT{Enabled: true, Broker: b.URL()}, "plant-a", "uic-plant-a")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer s.close()

	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateConnecting, To: scheduler.StateCollecting,
		At: time.Now(),
	})
	b.WaitMessages(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.republish(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hist := map[string]int{}
		for _, m := range b.Messages() {
			hist[m.Topic]++
		}
		if hist["uic/plant-a/$birth"] >= 2 && hist["uic/plant-a/dev1/$state"] >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	hist := map[string]int{}
	for _, m := range b.Messages() {
		hist[m.Topic]++
	}
	t.Fatalf("republish did not resend retained facts: %v", hist)
}

// 重连后必须自动补发：这是 MQTT 链路自愈的关键一环，不能只靠人手工重启。
func TestMQTTRepublishesAfterReconnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	s, err := newMQTTSink(config.MQTT{Enabled: true, Broker: b.URL()}, "plant-a", "uic-plant-a")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer s.close()

	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateConnecting, To: scheduler.StateCollecting,
		At: time.Now(),
	})
	b.WaitMessages(t, 1)

	// 等客户端真的察觉断线再重发。DropAll 后立刻 Publish 会落在
	// 「对端已关、本端未察觉」的窗口里：本地写仍可能成功，于是不会触发
	// 重连，测试就变成了在赌时序。
	b.DropAll()
	deadline := time.Now().Add(3 * time.Second)
	for s.client.Stats().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.client.Stats().Connected {
		t.Fatalf("client did not notice dropped connection (connects=%d, stats=%+v)",
			b.Connects(), s.client.Stats())
	}

	// QoS1 才会在真正成功前等到 PUBACK；QoS0 在已半关闭的 socket 上
	// 本地写仍可能成功，永远触发不到重连检测。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.client.Publish(ctx, mqtt.Message{Topic: "probe", Payload: []byte("x"), QoS: 1}); err != nil {
		t.Fatalf("publish after drop: %v", err)
	}
	if n := b.Connects(); n < 2 {
		t.Fatalf("connects = %d, want >= 2", n)
	}

	// $state 只会因 onState 或 republish 而发；出现两次即证明重连触发了补发。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		states := 0
		for _, m := range b.Messages() {
			if m.Topic == "uic/plant-a/dev1/$state" {
				states++
			}
		}
		if states >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state was not republished after reconnect (connects=%d)", b.Connects())
}

// 正常停机必须把 online=false 落成 retained：否则 retained 的 $birth 会
// 一直告诉消费方「网关在线」。
func TestMQTTClosePublishesDeath(t *testing.T) {
	b := mqtttest.New(t, nil)
	s, err := newMQTTSink(config.MQTT{Enabled: true, Broker: b.URL()}, "plant-a", "uic-plant-a")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.client.Publish(ctx, mqtt.Message{Topic: "warmup", Payload: []byte("x"), QoS: 1}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var death *mqtttest.Message
	for _, m := range b.Messages() {
		if m.Topic == "uic/plant-a/$death" {
			m := m
			death = &m
		}
	}
	if death == nil {
		t.Fatal("$death not published on graceful shutdown")
	}
	if !death.Retain {
		t.Fatal("$death must be retained")
	}
	var payload struct {
		Online bool `json:"online"`
	}
	if err := json.Unmarshal(death.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Online {
		t.Fatalf("$death payload = %s, want online=false", death.Payload)
	}
}
