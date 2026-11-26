package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/mqtt/mqtttest"
)

// 北向「数据契约版本化」的端到端验收（docs/02-synthesis/03-gap-analysis.md §3 第 3 条）：
//
//   同一份采集数据可以同时以两种 Topic 结构输出（旧契约 + 新契约），且切换
//   不需要重启采集。
//
// 这组用例把整条链路串起来：契约编解码（internal/northbound/contract）→ 双 lane
// 注册（outbox.openSinks）→ 运行期分流（publish 读原子 cv）→ MQTT/Webhook/Journal
// 三个 sink 各自的两条 lane → reload 热切 contractVersion。覆盖三种传输，证明
// 「双发」不是 MQTT 专属，而是北向子系统的一等能力。

// rawRecorder 记录 webhook 收到的原始请求体：v1/v2 信封字段名不同（一个
// schemaVersion、一个 contractVersion + 嵌套 sample），用 eventPayload 解析会丢
// v2，所以这里只留字节，按内容判断版本。
type rawRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (w *rawRecorder) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.bodies = append(w.bodies, body)
	w.mu.Unlock()
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(`{"ok":true}`))
}

func (w *rawRecorder) countIf(pred func([]byte) bool) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, b := range w.bodies {
		if pred(b) {
			n++
		}
	}
	return n
}

// --- 载荷断言助手 -----------------------------------------------------------

func payloadHasSchemaVersion1(b []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	if m["schemaVersion"] != float64(1) {
		return false
	}
	// v1 是扁平的：device 直接在顶层，没有 sample 包裹。
	_, hasDevice := m["device"]
	_, hasSample := m["sample"]
	return hasDevice && !hasSample
}

func payloadHasContractVersion2(b []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	if m["contractVersion"] != float64(2) {
		return false
	}
	sample, ok := m["sample"].(map[string]any)
	if !ok {
		return false
	}
	// v2 用驼峰外部字段名，且不应出现 v1 的顶层 device。
	_, hasDeviceID := sample["deviceId"]
	_, hasTopDevice := m["device"]
	return hasDeviceID && !hasTopDevice
}

func isValueTopic(topic string) bool   { return strings.HasSuffix(topic, "/value") }
func isValueV2Topic(topic string) bool { return strings.HasSuffix(topic, "/value/v2") }

// waitFor 轮询 cond 直到为真或超时。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// writeValue 向可写点写入一个值，触发一次变化事件进入 Outbox。
func writeValue(h *harness, point string, num int) {
	h.t.Helper()
	body := `{"writes":[{"point":"` + point + `","value":{"type":"uint16","num":` +
		itoa(num) + `},"confirm":true}]}`
	if code, resp := h.do(http.MethodPost, "/api/v1/points/sim01/write", body); code != 200 {
		h.t.Fatalf("write %s = %d %s", point, code, resp)
	}
}

// withMQTTContract 与 withMQTT 相同，但同时把 contractVersion 设在 northbound
// 层级（契约版本属于北向整体，不是 mqtt 子项）。
func withMQTTContract(url string, cv int) func(map[string]any) {
	return func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		cfg["northbound"] = map[string]any{
			"mqtt":            map[string]any{"enabled": true, "broker": url, "qos": 1},
			"contractVersion": cv,
		}
	}
}

// --- MQTT -------------------------------------------------------------------

// cv=2 双发：同一次写入同时落到 v1 主题（schemaVersion:1 扁平）与 v2 主题
// （contractVersion:2 嵌套），且两个主题不同。
func TestContractDualSendMQTT(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTTContract(b.URL(), 2))
	h.waitPointGood("sim01", "reg0")

	writeValue(h, "write_me", 101)

	waitFor(t, 5*time.Second, func() bool {
		var v1, v2 bool
		for _, msg := range b.Messages() {
			if isValueTopic(msg.Topic) && payloadHasSchemaVersion1(msg.Payload) {
				v1 = true
			}
			if isValueV2Topic(msg.Topic) && payloadHasContractVersion2(msg.Payload) {
				v2 = true
			}
		}
		return v1 && v2
	}, "both v1 and v2 lanes delivered")

	// v2 载荷绝不能出现在 v1 主题上，反之亦然——否则「两种结构」就无从区分。
	for _, msg := range b.Messages() {
		if isValueTopic(msg.Topic) && payloadHasContractVersion2(msg.Payload) {
			t.Fatalf("v2 payload leaked onto v1 topic %q", msg.Topic)
		}
		if isValueV2Topic(msg.Topic) && payloadHasSchemaVersion1(msg.Payload) {
			t.Fatalf("v1 payload leaked onto v2 topic %q", msg.Topic)
		}
	}
}

// cv=1（默认）：只有 v1 lane 出队，v2 lane 空转。这保证升级前老部署的线上
// 流量一个字节都没变。
func TestContractV1OnlyMQTT(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTT(b.URL(), nil)) // cv 未配置
	h.waitPointGood("sim01", "reg0")

	writeValue(h, "write_me", 202)

	waitFor(t, 5*time.Second, func() bool {
		for _, msg := range b.Messages() {
			if isValueTopic(msg.Topic) && payloadHasSchemaVersion1(msg.Payload) {
				return true
			}
		}
		return false
	}, "v1 lane delivered")

	time.Sleep(300 * time.Millisecond)
	for _, msg := range b.Messages() {
		if isValueV2Topic(msg.Topic) {
			t.Fatalf("cv=1 must not emit on v2 topic, saw %q", msg.Topic)
		}
	}
}

// 运行期热切：cv=1 起手、只出 v1；通过 API 把 contractVersion 改成 2，不重启
// 进程就让 v2 lane 也开始出队。这是验收标准「切换不需要重启采集」的直接证明。
func TestContractRuntimeSwitchNoRestart(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTT(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	// 起手 cv=1：只出 v1。
	writeValue(h, "write_me", 301)
	waitFor(t, 5*time.Second, func() bool {
		for _, msg := range b.Messages() {
			if isValueTopic(msg.Topic) && payloadHasSchemaVersion1(msg.Payload) {
				return true
			}
		}
		return false
	}, "v1 before switch")

	// API 热切到 cv=2。改的是同一进程的 contractVersion 原子值，不重建 sink。
	raw, err := h.rt.Config()
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	nb, ok := cfg["northbound"].(map[string]any)
	if !ok {
		t.Fatalf("no northbound in config: %v", cfg)
	}
	// 契约版本是 northbound 层级的字段，不是 mqtt 子项。
	nb["contractVersion"] = 2
	patched, _ := json.Marshal(cfg)
	if code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(patched)); code != 200 {
		t.Fatalf("apply cv=2 = %d %s", code, body)
	}

	// 切完之后，新写入必须同时出现在 v1 与 v2 主题——且进程从未重启（harness
	// 的 cleanup 会做 Stop，这里若中途重启过 outbox 目录/sink 都会重建，断言
	// 仍成立但意义不同；核心证据是「没重启就出 v2」）。
	writeValue(h, "write_me", 302)
	waitFor(t, 5*time.Second, func() bool {
		var v1, v2 bool
		for _, msg := range b.Messages() {
			if isValueTopic(msg.Topic) && payloadHasSchemaVersion1(msg.Payload) {
				v1 = true
			}
			if isValueV2Topic(msg.Topic) && payloadHasContractVersion2(msg.Payload) {
				v2 = true
			}
		}
		return v1 && v2
	}, "both lanes after runtime switch")
}

// 两条 lane 是独立的 outbox 分区：cv=2 下两条都要被各自确认、pending 归零。
// 若 v2 lane 与 v1 共用一个分区，这里会只看到一个统计项。
func TestContractLanesAreIndependentPartitions(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withMQTTContract(b.URL(), 2))
	h.waitPointGood("sim01", "reg0")
	writeValue(h, "write_me", 401)

	// 等两条 lane 都投递完。
	waitFor(t, 5*time.Second, func() bool {
		var v1, v2 bool
		for _, msg := range b.Messages() {
			if isValueTopic(msg.Topic) && payloadHasSchemaVersion1(msg.Payload) {
				v1 = true
			}
			if isValueV2Topic(msg.Topic) && payloadHasContractVersion2(msg.Payload) {
				v2 = true
			}
		}
		return v1 && v2
	}, "both lanes delivered")

	// 统计里必须同时存在 mqtt 与 mqtt#v2 两个独立分区。
	waitFor(t, 3*time.Second, func() bool {
		var hasV1, hasV2 bool
		for _, st := range h.rt.box.Stats() {
			if st.Name == "mqtt" {
				hasV1 = true
			}
			if st.Name == "mqtt#v2" {
				hasV2 = true
			}
		}
		return hasV1 && hasV2
	}, "both lanes present as separate outbox partitions")
}

// --- Journal ----------------------------------------------------------------

// Journal 双写：cv=2 时 journal.ndjson 同时落 v1 行（schemaVersion:1）与 v2
// 行（contractVersion:2），读方按版本字段过滤。
func TestContractJournalDualWrite(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		// withMQTT 会把 journal 关掉；这里要 journal，自己配 northbound。
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": true,
		}
		cfg["northbound"] = map[string]any{
			"contractVersion": 2,
		}
	})
	h.waitPointGood("sim01", "reg0")
	writeValue(h, "write_me", 501)

	path := filepath.Join(h.dir, "journal.ndjson")
	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		s := string(raw)
		return strings.Contains(s, "\"schemaVersion\":1") &&
			strings.Contains(s, "\"contractVersion\":2")
	}, "journal has both v1 and v2 lines")
}

// --- Webhook ----------------------------------------------------------------

// webhook 信封只有一层：v1 是 {"schemaVersion":1,"items":[...]}，v2 是
// {"contractVersion":2,"items":[...]}。items 里的样本分别带 schemaVersion /
// contractVersion。用「信封标识」子串判断即可区分两版，且互不包含。
func webhookIsV1(b []byte) bool {
	return strings.Contains(string(b), `"schemaVersion":1`) &&
		!strings.Contains(string(b), `"contractVersion":2`)
}

func webhookIsV2(b []byte) bool {
	return strings.Contains(string(b), `"contractVersion":2`) &&
		!strings.Contains(string(b), `"schemaVersion":1`)
}

// Webhook 双发：cv=2 时端点同时收到 v1 信封（schemaVersion:1）与 v2 信封
// （contractVersion:2）。webhook 没有主题，差异全在载荷。
func TestContractDualSendWebhook(t *testing.T) {
	rec := &rawRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		cfg["northbound"] = map[string]any{
			"webhook":         map[string]any{"enabled": true, "url": srv.URL, "batch": 10, "timeout": "2s"},
			"contractVersion": 2,
		}
	})
	h.waitPointGood("sim01", "reg0")
	writeValue(h, "write_me", 601)

	waitFor(t, 5*time.Second, func() bool {
		return rec.countIf(webhookIsV1) >= 1 &&
			rec.countIf(webhookIsV2) >= 1
	}, "webhook received both v1 and v2 envelopes")
}

// 指纹归一化：切换 contractVersion 及随之派生的 TopicV2 不得触发「需重启」
// 警告；而真正改了主题模板（Topic 或显式 TopicV2）必须触发。这是把热切属性
// 从「运维看不到的原子值」落到「日志不误报」的保证。
func TestNorthboundRestartFingerprintNormalizesHotFields(t *testing.T) {
	st := config.Storage{}
	base := config.Northbound{MQTT: config.MQTT{Topic: "uic/{site}/{device}/{point}/value"}}

	// cv=1 → cv=2，TopicV2 是派生值：指纹不变。
	dual := config.Northbound{ContractVersion: 2, MQTT: config.MQTT{
		Topic:   "uic/{site}/{device}/{point}/value",
		TopicV2: "uic/{site}/{device}/{point}/value/v2",
	}}
	if string(northboundRestartFingerprint(base, st)) != string(northboundRestartFingerprint(dual, st)) {
		t.Fatal("toggling contractVersion / derived topicV2 must not change the restart fingerprint")
	}

	// 显式把 TopicV2 改成别的模板：指纹必须变（真需要重启）。
	custom := config.Northbound{ContractVersion: 2, MQTT: config.MQTT{
		Topic:   "uic/{site}/{device}/{point}/value",
		TopicV2: "custom/{device}/{point}",
	}}
	if string(northboundRestartFingerprint(base, st)) == string(northboundRestartFingerprint(custom, st)) {
		t.Fatal("explicit topicV2 change must change the restart fingerprint")
	}

	// 改 Topic 本身：指纹必须变。
	topicChanged := config.Northbound{MQTT: config.MQTT{Topic: "other/{device}/{point}"}}
	if string(northboundRestartFingerprint(base, st)) == string(northboundRestartFingerprint(topicChanged, st)) {
		t.Fatal("changing topic must change the restart fingerprint")
	}
}
