package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/alarm"
	"uic/internal/mqtt/mqtttest"
)

// newAlarmHarness 起一个「虚拟设备 + 报警规则」的最小运行时。
//
// 用虚拟驱动而不是 Modbus：报警的输入只需要「一个能被我精确设定、且能通过
// 正常采集路径进入缓存的值」。虚拟驱动的 mode=const 加上可写点正好满足，
// 而且不引入协议侧的抖动。
func newAlarmHarness(t *testing.T, mutate func(cfg map[string]any)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"schemaVersion": 1,
		"site":          "alarm-site",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": dir, "journal": false},
		"logging":       map[string]any{"level": "info"},
		"channels": []any{
			map[string]any{
				"id": "ch-virtual", "protocol": "virtual",
				"transport": map[string]any{"kind": "memory"}, "concurrency": 2, "timeout": "1s",
			},
		},
		"devices": []any{
			map[string]any{
				"id": "src", "name": "source", "channel": "ch-virtual",
				"interval": "100ms", "timeout": "1s", "enabled": true,
				"labels": map[string]any{"mode": "const"},
				"points": []any{
					map[string]any{"id": "power", "dataType": "float64", "enabled": true, "writable": true,
						"address": map[string]any{"kind": "register", "area": "holding", "offset": 0, "length": 2, "bit": -1}},
					map[string]any{"id": "flow", "dataType": "float64", "enabled": true, "writable": true,
						"address": map[string]any{"kind": "register", "area": "holding", "offset": 2, "length": 2, "bit": -1}},
				},
			},
		},
		"alarms": []any{
			map[string]any{
				"id": "high-power", "name": "功率过高", "severity": "critical",
				"point": "src:power", "high": 80, "hysteresis": 5, "onDelay": "100ms",
			},
			map[string]any{
				"id": "low-flow", "name": "流量过低", "severity": "warning",
				"point": "src:flow", "low": 10, "hysteresis": 2,
			},
			map[string]any{
				"id": "ratio", "name": "功率流量双高", "severity": "info",
				"expr": "src:power > 50 && src:flow > 20",
			},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	h := &harness{t: t, rt: rt, dir: dir, token: rt.Token()}
	h.srv = httptest.NewServer(rt.Handler())
	t.Cleanup(func() {
		h.srv.Close()
		cancel()
		_ = rt.Stop(5 * time.Second)
	})
	return h
}

// writePoint 通过 REST 写一个可写点（走完整的权限/确认/采集回读路径）。
func (h *harness) writePoint(device, point string, v float64) {
	h.t.Helper()
	body := `{"writes":[{"point":"` + point + `","value":{"type":"float64","num":` +
		jsonNumber(v) + `},"confirm":true}]}`
	code, resp := h.do(http.MethodPost, "/api/v1/points/"+device+"/write", body)
	if code != 200 {
		h.t.Fatalf("write %s: %d %s", point, code, resp)
	}
	var out struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Code  string `json:"code"`
			Error string `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		h.t.Fatalf("write resp: %v (%s)", err, resp)
	}
	if len(out.Results) != 1 || !out.Results[0].OK {
		h.t.Fatalf("write %s rejected: %s", point, resp)
	}
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// waitAlarm 等待某条报警进入指定状态。
func (h *harness) waitAlarm(id string, want alarm.State) alarm.Status {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last alarm.Status
	for time.Now().Before(deadline) {
		st, ok := h.rt.Alarm(id)
		if ok {
			last = st
			if st.State == want {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("alarm %q never reached %s (last = %+v)", id, want, last)
	return alarm.Status{}
}

func TestAlarmsEndToEnd(t *testing.T) {
	h := newAlarmHarness(t, nil)
	h.waitPointGood("src", "power")
	h.waitPointGood("src", "flow")

	// 初始：没有任何报警。规则数必须已经可见，否则「0 条 active」无法
	// 区分「一切正常」和「规则没加载」。
	code, body := h.do(http.MethodGet, "/api/v1/alarms", "")
	if code != 200 {
		t.Fatalf("list = %d %s", code, body)
	}
	var list struct {
		Items  []alarm.Status `json:"items"`
		Total  int            `json:"total"`
		Counts alarm.Counts   `json:"counts"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 3 || list.Counts.Total != 3 {
		t.Fatalf("rules not loaded: %s", body)
	}

	// 阈值 + onDelay：值超过阈值后先 pending，到点才 active。
	h.writePoint("src", "power", 90)
	h.waitAlarm("high-power", alarm.StatePending)
	st := h.waitAlarm("high-power", alarm.StateActive)
	if st.Value == nil || *st.Value < 80 {
		t.Fatalf("active value = %v", st.Value)
	}
	if st.Count != 1 {
		t.Fatalf("count = %d", st.Count)
	}

	// health 必须把报警算进去。
	code, body = h.do(http.MethodGet, "/api/v1/health", "")
	if code != 200 {
		t.Fatalf("health = %d", code)
	}
	var health struct {
		Alarms alarm.Counts `json:"alarms"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if health.Alarms.Active < 1 || health.Alarms.Total != 3 {
		t.Fatalf("health alarms = %+v", health.Alarms)
	}

	// 表达式规则：power>50 但 flow 仍是 42（const 默认值）→ 成立。
	ratio := h.waitAlarm("ratio", alarm.StateActive)
	if ratio.Value == nil || *ratio.Value != 1 {
		t.Fatalf("expr value = %v", ratio.Value)
	}

	// 确认：状态转 acked，并且在下一次采集后仍然保持（不会被评估冲掉）。
	code, body = h.do(http.MethodPost, "/api/v1/alarms/high-power/ack", `{"by":"alice"}`)
	if code != 200 {
		t.Fatalf("ack = %d %s", code, body)
	}
	var ack struct {
		Acknowledged []alarm.Status `json:"acknowledged"`
		Total        int            `json:"total"`
	}
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Total != 1 || ack.Acknowledged[0].State != alarm.StateAcked || ack.Acknowledged[0].AckBy != "alice" {
		t.Fatalf("ack resp = %s", body)
	}
	time.Sleep(200 * time.Millisecond)
	if st := h.waitAlarm("high-power", alarm.StateAcked); !st.Acknowledged {
		t.Fatalf("acked flag lost: %+v", st)
	}

	// 滞回：落回 75~80 之间仍然算「未恢复」。
	h.writePoint("src", "power", 78)
	time.Sleep(300 * time.Millisecond)
	if st, _ := h.rt.Alarm("high-power"); st.State != alarm.StateAcked {
		t.Fatalf("hysteresis band cleared the alarm: %+v", st)
	}

	// 跌破 75：已确认过的报警直接回 normal。
	h.writePoint("src", "power", 60)
	h.waitAlarm("high-power", alarm.StateNormal)

	// 低阈值规则。
	h.writePoint("src", "flow", 5)
	low := h.waitAlarm("low-flow", alarm.StateActive)
	if low.Value == nil || *low.Value != 5 {
		t.Fatalf("low value = %v", low.Value)
	}

	// 未确认的恢复 → cleared（而不是 normal）：运维还没看到这次报警。
	h.writePoint("src", "flow", 42)
	cleared := h.waitAlarm("low-flow", alarm.StateCleared)
	if cleared.ClearedAt == nil {
		t.Fatal("clearedAt should be set")
	}

	// cleared 也要能确认，确认后回 normal。
	code, body = h.do(http.MethodPost, "/api/v1/alarms/ack", `{"ids":["low-flow"],"by":"bob"}`)
	if code != 200 {
		t.Fatalf("ack cleared = %d %s", code, body)
	}
	h.waitAlarm("low-flow", alarm.StateNormal)

	// 已经 normal 的报警再确认：409 而不是 500，也不是静默成功。
	code, body = h.do(http.MethodPost, "/api/v1/alarms/low-flow/ack", "")
	if code != http.StatusConflict || !strings.Contains(string(body), "alarm.not_active") {
		t.Fatalf("ack normal = %d %s", code, body)
	}

	// 不存在的报警：404。
	code, body = h.do(http.MethodGet, "/api/v1/alarms/ghost", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), "alarm.not_found") {
		t.Fatalf("get ghost = %d %s", code, body)
	}

	// 全部确认：ratio 仍在 active。
	code, body = h.do(http.MethodPost, "/api/v1/alarms/ack", `{"all":true,"by":"carol"}`)
	if code != 200 {
		t.Fatalf("ack all = %d %s", code, body)
	}
	if st, _ := h.rt.Alarm("ratio"); st.State != alarm.StateAcked {
		t.Fatalf("ack all did not ack ratio: %+v", st)
	}
}

// 报警的 SSE 与点位 SSE 是两条流：这里验证事件名、载荷与心跳互不干扰。
func TestAlarmStreamEndToEnd(t *testing.T) {
	h := newAlarmHarness(t, nil)
	h.waitPointGood("src", "power")

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/alarms/stream?heartbeat=1s", nil)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var lines []string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v (got %v)", err, lines)
			}
			trimmed := strings.TrimRight(line, "\n")
			if trimmed == "" {
				return strings.Join(lines, "\n")
			}
			lines = append(lines, trimmed)
		}
	}
	hello := readEvent()
	if !strings.Contains(hello, "event: hello") {
		t.Fatalf("hello = %s", hello)
	}

	h.writePoint("src", "power", 90)
	// 同一轮里 ratio 也可能先报警；事件流的顺序不构成契约，
	// 消费方应当按 ID 匹配而不是依赖第一条。
	found := false
	for i := 0; i < 8 && !found; i++ {
		ev := readEvent()
		if !strings.Contains(ev, "event: raised") {
			t.Fatalf("unexpected event = %s", ev)
		}
		if strings.Contains(ev, `"id":"high-power"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no raised event for high-power")
	}
}

// 报警的 MQTT 出口：retained、独立主题、重连补发。
func TestAlarmMQTTRetained(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newAlarmHarness(t, func(cfg map[string]any) {
		cfg["northbound"] = map[string]any{"mqtt": map[string]any{
			"enabled": true, "broker": b.URL(), "qos": 1,
		}}
	})
	h.waitPointGood("src", "power")

	h.writePoint("src", "power", 90)
	h.waitAlarm("high-power", alarm.StateActive)
	b.WaitMessages(t, 1)

	// 连接建立时会先补发一次全量快照（含 normal），所以不能拿「第一条」
	// 匹配消息做断言——必须等到状态变成 active 的那一条。用最新一条匹配
	// 消息，而不是第一条。
	type alarmPayload struct {
		SchemaVersion int            `json:"schemaVersion"`
		ID            string         `json:"id"`
		State         alarm.State    `json:"state"`
		Severity      alarm.Severity `json:"severity"`
	}
	var last mqtttest.Message
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, m := range b.Messages() {
			if strings.Contains(m.Topic, "/$alarm/high-power") {
				last = m
			}
		}
		if last.Topic != "" {
			var p alarmPayload
			if err := json.Unmarshal(last.Payload, &p); err == nil && p.State == alarm.StateActive {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no active $alarm message published (last = %+v)", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.Topic != "uic/alarm-site/$alarm/high-power" {
		t.Fatalf("topic = %q", last.Topic)
	}
	if !last.Retain {
		t.Fatal("alarm state must be retained")
	}
	var payload alarmPayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("payload: %v (%s)", err, last.Payload)
	}
	if payload.SchemaVersion != 1 || payload.ID != "high-power" ||
		payload.State != alarm.StateActive || payload.Severity != alarm.SeverityCritical {
		t.Fatalf("payload = %+v", payload)
	}
}
