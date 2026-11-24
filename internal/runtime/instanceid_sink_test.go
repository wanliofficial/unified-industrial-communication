package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/mqtt/mqtttest"
	"uic/internal/outbox"
)

// connectRecord 构造一条最小投递记录，用于触发 Sparkplug sink 的惰性连接。
func connectRecord(t *testing.T, device, point string) outbox.Record {
	t.Helper()
	payload, err := json.Marshal(eventPayload{SchemaVersion: 1, Sample: model.Sample{
		DeviceID: device, PointID: point,
		Value: model.NumValue(model.TypeFloat64, 1), Quality: model.QualityGood,
		CollectedAt: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.Record{Sink: "mqtt", Key: device + "/" + point, Data: payload, Ts: time.Now()}
}

// closeSinks 关闭测试里 openSinks 建出的 outbox，避免临时目录删不掉。
func (r *Runtime) closeSinks() {
	if r.box != nil {
		_ = r.box.Close()
	}
}

// quietLog 复用 history_test.go 里的同名 helper（同包），保持日志构造一致。

// TestOpenSinksDefaultClientIDIncludesInstanceShort：JSON 格式 MQTT 未显式配
// clientID 时，openSinks 派生的默认 clientID 必须带上实例身份短码——同站点
// 多实例默认不冲突的根基。
func TestOpenSinksDefaultClientIDIncludesInstanceShort(t *testing.T) {
	b := mqtttest.New(t, nil)
	r := &Runtime{instanceID: "inst-a1b2c3d4e5f6", log: quietLog()}

	cfg := config.Northbound{
		MQTT: config.MQTT{
			Enabled: true, Broker: b.URL(), Format: config.MQTTFormatJSON,
			Topic: "uic/{site}/{device}/{point}/value",
		},
	}
	if err := r.openSinks(&config.File{Site: "plant-a", Northbound: cfg}); err != nil {
		t.Fatalf("openSinks: %v", err)
	}
	defer r.closeSinks()

	// JSON sink 也是惰连接的：第一次 publish 才连 Broker，故先投递一条触发。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = r.sinks["mqtt"](ctx, []outbox.Record{connectRecord(t, "dev1", "p1")})
	cancel()

	conns := b.WaitConnections(t, 1)
	want := "uic-plant-a-a1b2c3d4e5f6"
	if conns[0].ClientID != want {
		t.Fatalf("默认 clientID = %q, want %q", conns[0].ClientID, want)
	}
}

// TestOpenSinksExplicitClientIDPreserved：用户显式配了 clientID 时，openSinks
// 不得覆盖——实例消歧只在「默认值」这一层兜底，不能改写用户意图。
func TestOpenSinksExplicitClientIDPreserved(t *testing.T) {
	b := mqtttest.New(t, nil)
	r := &Runtime{instanceID: "inst-a1b2c3d4e5f6", log: quietLog()}

	cfg := config.Northbound{
		MQTT: config.MQTT{
			Enabled: true, Broker: b.URL(), Format: config.MQTTFormatJSON,
			Topic: "uic/{site}/{device}/{point}/value", ClientID: "my-gateway",
		},
	}
	if err := r.openSinks(&config.File{Site: "plant-a", Northbound: cfg}); err != nil {
		t.Fatalf("openSinks: %v", err)
	}
	defer r.closeSinks()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = r.sinks["mqtt"](ctx, []outbox.Record{connectRecord(t, "dev1", "p1")})
	cancel()

	conns := b.WaitConnections(t, 1)
	if conns[0].ClientID != "my-gateway" {
		t.Fatalf("显式 clientID 被改写为 %q", conns[0].ClientID)
	}
}

// TestOpenSinksSparkplugNodeIDIncludesInstanceShort：Sparkplug 裸默认 nodeId
// （config.Normalize 补的「uic-<site>」）必须被追加实例短码，使同站点多
// 实例默认不冲突。
//
// Sparkplug sink 是惰连接的：只有第一次 deliver（发 NBIRTH）才连 Broker。所以
// 这里先 deliver 一条触发 CONNECT，再断言 clientID。
func TestOpenSinksSparkplugNodeIDIncludesInstanceShort(t *testing.T) {
	b := mqtttest.New(t, nil)
	r := &Runtime{instanceID: "inst-a1b2c3d4e5f6", log: quietLog()}

	cfg := config.Northbound{
		MQTT: config.MQTT{
			Enabled: true, Broker: b.URL(), Format: config.MQTTFormatSparkplugB,
			GroupID: "group-a", NodeID: "uic-plant-a", // Normalize 的裸默认值
		},
	}
	if err := r.openSinks(&config.File{Site: "plant-a", Northbound: cfg}); err != nil {
		t.Fatalf("openSinks: %v", err)
	}
	defer r.closeSinks()

	// 触发一次投递，逼 sink 连上 Broker（clientID 在 CONNECT 包里）。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = r.sinks["mqtt"](ctx, []outbox.Record{connectRecord(t, "dev1", "p1")})
	cancel()

	conns := b.WaitConnections(t, 1)
	// Sparkplug clientID 默认取 nodeID（见 sparkplug_sink），故这里验证 nodeID
	// 已被追加短码。
	want := "uic-plant-a-a1b2c3d4e5f6"
	if conns[0].ClientID != want {
		t.Fatalf("Sparkplug clientID(=nodeID) = %q, want %q", conns[0].ClientID, want)
	}
}

// TestOpenSinksSparkplugExplicitNodeIDPreserved：Sparkplug 用户显式配了非裸
// 默认的 nodeId 时不得改写。
func TestOpenSinksSparkplugExplicitNodeIDPreserved(t *testing.T) {
	b := mqtttest.New(t, nil)
	r := &Runtime{instanceID: "inst-a1b2c3d4e5f6", log: quietLog()}

	cfg := config.Northbound{
		MQTT: config.MQTT{
			Enabled: true, Broker: b.URL(), Format: config.MQTTFormatSparkplugB,
			GroupID: "group-a", NodeID: "edge-node-1",
		},
	}
	if err := r.openSinks(&config.File{Site: "plant-a", Northbound: cfg}); err != nil {
		t.Fatalf("openSinks: %v", err)
	}
	defer r.closeSinks()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = r.sinks["mqtt"](ctx, []outbox.Record{connectRecord(t, "dev1", "p1")})
	cancel()

	conns := b.WaitConnections(t, 1)
	if conns[0].ClientID != "edge-node-1" {
		t.Fatalf("显式 nodeId 被改写为 %q", conns[0].ClientID)
	}
}

// TestInstanceShortExtraction：instanceShort 从「inst-<hex>」取 hex 段；
// 身份缺失时返回空串（退化回旧默认，不阻断）。
func TestInstanceShortExtraction(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"inst-a1b2c3d4e5f6", "a1b2c3d4e5f6"},
		{"", ""},
		{"garbage", ""},
		{"inst-", ""}, // 前缀但无 hex 段
	}
	for _, c := range cases {
		r := &Runtime{instanceID: c.id}
		if got := r.instanceShort(); got != c.want {
			t.Fatalf("instanceShort(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}
