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
	b := mqtttest.New(t, mqtttest.NoAck)
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
	b := mqtttest.New(t, mqtttest.NoAck)
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

	// 等客户端真的察觉断线。注意不能假设「察觉后一定停留在断开状态」：
	// 保活或并发的补发都可能已经触发重连。因此判定条件是「断线被观察到」
	// （Connected 变 false 或 Connects 增加），而不是「Connected == false」。
	// 后者在重连快到毫秒级时必然超时，是个纯粹的时序赌局。
	b.DropAll()
	connectsBefore := b.Connects()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.client.Stats().Connected || b.Connects() > connectsBefore {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := s.client.Stats(); st.Connected && b.Connects() == connectsBefore {
		t.Fatalf("client did not notice dropped connection (connects=%d, stats=%+v)",
			b.Connects(), st)
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

// 设备被从配置里删除时，Broker 上那条 retained 的 $state 必须真的消失。
//
// 只删本地快照是不够的：retained 是 Broker 侧的事实，新接入的消费方照样
// 会读到一台已经不存在的设备。唯一能删掉它的手段是发零长度 retained 载荷。
func TestMQTTRemoveClearsRetainedState(t *testing.T) {
	b := mqtttest.New(t, nil)
	s, err := newMQTTSink(config.MQTT{Enabled: true, Broker: b.URL()}, "plant-a", "uic-plant-a")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer s.close()

	topic := "uic/plant-a/dev1/$state"
	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateConnecting, To: scheduler.StateCollecting,
		At: time.Now(),
	})
	waitRetained(t, b, topic)

	s.onRemove("dev1")

	b.WaitRetainedGone(t, topic)
	// 本地快照也必须清掉：留着的话下一次 republish 会把幽灵重新发出去。
	s.stateMu.Lock()
	_, hadState := s.states["dev1"]
	_, pending := s.stateCleared["dev1"]
	s.stateMu.Unlock()
	if hadState {
		t.Fatal("state snapshot survived the removal")
	}
	if pending {
		t.Fatal("cleared marker survived: republish would clear a device that no longer exists")
	}
}

// 断线期间删除设备：清空消息发不出去，必须在重连后补发。
//
// 丢弃这个记录会让幽灵 retained 永久留在 Broker 上——Broker 重启之前
// 再也没有机会清理它，而「重连后看板上多出一台已删设备」正是最难解释的
// 一类现场问题。
func TestMQTTRemoveWhileOfflineIsResentOnReconnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	s, err := newMQTTSink(config.MQTT{Enabled: true, Broker: b.URL()}, "plant-a", "uic-plant-a")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer s.close()

	topic := "uic/plant-a/dev1/$state"
	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateConnecting, To: scheduler.StateCollecting,
		At: time.Now(),
	})
	waitRetained(t, b, topic)

	// 制造一个确定性的离线窗口：后续每一次 CONNECT 都被 Broker 立刻关闭，
	// 因此 onRemove 的那次清空发布必然失败，只能落成待补发记录。
	b.SetDropOnConnect(true)
	b.DropAll()

	// onRemove 此刻发出的清空一定失败（Broker 不接受连接），因此必须留下
	// 一条 pending 记录，而不是静默丢掉。
	s.onRemove("dev1")
	s.stateMu.Lock()
	pending := s.stateCleared["dev1"]
	had := false
	if _, ok := s.states["dev1"]; ok {
		had = true
	}
	s.stateMu.Unlock()
	if had {
		t.Fatal("state snapshot survived the removal")
	}
	if !pending {
		t.Fatal("offline removal did not leave a pending clear: the ghost retained would never be cleaned up")
	}
	// 幽灵此刻确实还在：这正是「发不出去」的含义。
	if !retainedHas(b, topic) {
		t.Fatal("retained state vanished while the broker was refusing connections")
	}

	// 恢复可达性：重连后 republish 必须把待补发的清空发出去。
	b.SetDropOnConnect(false)
	// 触发一次重连（任何一条成功发布都会建链并触发 OnConnect）。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.client.Publish(ctx, mqtt.Message{Topic: "probe", Payload: []byte("x"), QoS: 1}); err != nil {
		t.Fatalf("publish after reconnect: %v", err)
	}

	b.WaitRetainedGone(t, topic)

	// republish 的顺序是「先发清空，再删记录」，因此保留消息消失的那一刻
	// 记录可能还在。等它也被清掉才是终态——否则下一次重连会为这台已经不
	// 存在的设备再补发一次清空。
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.stateMu.Lock()
		pending = s.stateCleared["dev1"]
		s.stateMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleared marker never consumed: republish keeps re-clearing a device that no longer exists")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// retainedHas 报告某个主题当前是否有保留消息。
func retainedHas(b *mqtttest.Broker, topic string) bool {
	for _, m := range b.Retained() {
		if m.Topic == topic {
			return true
		}
	}
	return false
}

// waitRetained 等到某个主题的保留消息出现在 Broker 上。
func waitRetained(t *testing.T, b *mqtttest.Broker, topic string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range b.Retained() {
			if m.Topic == topic {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("retained %q never appeared; holding %v", topic, b.RetainedTopics())
}

// 端到端：通过 API 删除设备后，消费方不应再读到它的 retained 状态。
//
// 这条用例把整条链路串起来（配置热更新 → reload → sink.onRemove → Broker
// 删除保留消息），是「删设备」这个动作唯一有意义的验收标准：本地对象清空
// 只证明本进程忘了，消费方能不能读到才是结果。
func TestMQTTRemoveDeviceViaConfigClearsRetainedState(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTT(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	topic := "uic/test-site/sim01/$state"
	waitRetained(t, b, topic)

	raw, err := h.rt.Config()
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["devices"] = []any{} // 删掉唯一的设备
	patched, _ := json.Marshal(cfg)
	if code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(patched)); code != 200 {
		t.Fatalf("apply = %d %s", code, body)
	}

	b.WaitRetainedGone(t, topic)
}

// 删除设备后，一条迟到的状态事件不得把它复活。
//
// 现实路径：调度器的状态事件是异步派发的，reload 删掉设备时队列里可能还压着
// 该设备的事件。若放行，Broker 上会重新出现一条 retained `$state`，而
// onRemove 已经清掉了待补发记录——这条幽灵再也没有人负责清理，看板上会
// 永远挂着一台不存在的设备。
//
// 断言在 runtime 的入口上（onDeviceState）：它按配置核对设备是否还在，这正是
// 真实链路里状态事件的唯一入口。sink 自己不设门槛，因为它分辨不了「删除」
// 与「同 ID 重配后重新出现」。
func TestLateStateEventForRemovedDeviceIsDropped(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTT(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	topic := "uic/test-site/sim01/$state"
	waitRetained(t, b, topic)

	// 通过 API 删掉唯一的设备，Broker 上的 retained 状态随之消失。
	raw, err := h.rt.Config()
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["devices"] = []any{}
	patched, _ := json.Marshal(cfg)
	if code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(patched)); code != 200 {
		t.Fatalf("apply = %d %s", code, body)
	}
	b.WaitRetainedGone(t, topic)

	// 模拟调度器队列里那条迟到的事件，从真实入口投递。
	h.rt.onDeviceState(scheduler.StateEvent{
		DeviceID: "sim01", From: scheduler.StateCollecting, To: scheduler.StateFaulted,
		At: time.Now(),
	})

	time.Sleep(200 * time.Millisecond)
	if retainedHas(b, topic) {
		t.Fatalf("late state event resurrected a removed device; retained now %v",
			b.RetainedTopics())
	}
}
