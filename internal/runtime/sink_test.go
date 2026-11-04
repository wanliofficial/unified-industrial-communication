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
	"sync/atomic"
	"testing"
	"time"

	"uic/internal/northbound/httpapi"
)

// webhookRecorder 记录收到的批次。
type webhookRecorder struct {
	mu       sync.Mutex
	batches  [][]eventPayload
	headers  []http.Header
	status   atomic.Int32
	requests atomic.Int64
}

func (w *webhookRecorder) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		SchemaVersion int            `json:"schemaVersion"`
		Items         []eventPayload `json:"items"`
	}
	_ = json.Unmarshal(body, &payload)
	w.mu.Lock()
	w.headers = append(w.headers, r.Header.Clone())
	w.batches = append(w.batches, payload.Items)
	w.mu.Unlock()

	code := int(w.status.Load())
	if code == 0 {
		code = 200
	}
	rw.WriteHeader(code)
	_, _ = rw.Write([]byte(`{"ok":true}`))
}

func (w *webhookRecorder) snapshot() ([][]eventPayload, []http.Header) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]eventPayload(nil), w.batches...), append([]http.Header(nil), w.headers...)
}

func withWebhook(url string, batch int, headers map[string]string) func(map[string]any) {
	return func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		cfg["northbound"] = map[string]any{
			"webhook": map[string]any{
				"enabled": true, "url": url, "batch": batch,
				"timeout": "2s", "headers": headers,
			},
		}
	}
}

// Webhook 通道：真实 POST、批量、自定义头、schemaVersion 必须都对。
func TestWebhookSinkDelivers(t *testing.T) {
	rec := &webhookRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, withWebhook(srv.URL, 2, map[string]string{"X-Tenant": "plant-a"}))
	h.waitPointGood("sim01", "reg0")

	// 触发可预测的变化事件（reg10 是常量，写入 write_me 会产生变化）。
	for _, v := range []int{11, 22, 33, 44} {
		body := `{"writes":[{"point":"write_me","value":{"type":"uint16","num":` +
			itoa(v) + `},"confirm":true}]}`
		if code, resp := h.do(http.MethodPost, "/api/v1/points/sim01/write", body); code != 200 {
			t.Fatalf("write = %d %s", code, resp)
		}
		time.Sleep(30 * time.Millisecond)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		batches, headers := rec.snapshot()
		if len(batches) > 0 {
			for i, b := range batches {
				if len(b) == 0 {
					t.Fatalf("batch %d is empty", i)
				}
				if len(b) > 2 {
					t.Fatalf("batch %d has %d items, want <= 2", i, len(b))
				}
				for _, item := range b {
					if item.SchemaVersion != 1 {
						t.Fatalf("schemaVersion = %d", item.SchemaVersion)
					}
					if item.DeviceID != "sim01" || item.PointID == "" {
						t.Fatalf("payload = %+v", item)
					}
				}
			}
			if got := headers[0].Get("X-Tenant"); got != "plant-a" {
				t.Fatalf("custom header lost: %q", got)
			}
			if ct := headers[0].Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type = %q", ct)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("webhook never received a batch")
}

// 端点返回 5xx 时不得推进确认位点：数据必须留在队列里等重试。
func TestWebhookFailureKeepsRecordsPending(t *testing.T) {
	rec := &webhookRecorder{}
	rec.status.Store(500)
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, withWebhook(srv.URL, 8, nil))
	h.waitPointGood("sim01", "reg0")

	for _, v := range []int{7, 8, 9} {
		code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
			`{"writes":[{"point":"write_me","value":{"type":"uint16","num":`+itoa(v)+`}}]}`)
		if code != 200 {
			t.Fatalf("write = %d %s", code, body)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec.requests.Load() > 0 {
			code, body := h.do(http.MethodGet, "/api/v1/health", "")
			if code != 200 {
				t.Fatalf("health = %d", code)
			}
			var health httpapi.Health
			if err := json.Unmarshal(body, &health); err != nil {
				t.Fatal(err)
			}
			if health.Outbox.Pending == 0 {
				t.Fatalf("records must stay pending after a 5xx: %+v", health.Outbox)
			}
			if health.Outbox.Delivered != 0 {
				t.Fatalf("delivered must stay 0: %+v", health.Outbox)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("webhook never called")
}

// 端点恢复后，积压必须自动补投（at-least-once）。
func TestWebhookRecoversAfterOutage(t *testing.T) {
	rec := &webhookRecorder{}
	rec.status.Store(500)
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, withWebhook(srv.URL, 8, nil))
	h.waitPointGood("sim01", "reg0")

	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":1234},"confirm":true}]}`)
	if code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}

	// 等到至少一次失败投递，再让端点恢复。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && rec.requests.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if rec.requests.Load() == 0 {
		t.Fatal("webhook never attempted")
	}
	rec.status.Store(200)

	// 手动触发一次 flush 加速重试。
	if code, body := h.do(http.MethodPost, "/api/v1/commands", `{"action":"outbox.flush"}`); code != 200 {
		t.Fatalf("flush = %d %s", code, body)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body := h.do(http.MethodGet, "/api/v1/health", "")
		if code == 200 {
			var health httpapi.Health
			if err := json.Unmarshal(body, &health); err == nil {
				if health.Outbox.Pending == 0 && health.Outbox.Delivered > 0 {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("backlog was never delivered after the endpoint recovered")
}

// journal 是「断网留痕」的最低成本实现：Stop 必须 Sync 落盘。
func TestJournalSinkFlushesOnStop(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	// 触发一次变化。
	if code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":2025},"confirm":true}]}`); code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}
	time.Sleep(200 * time.Millisecond)

	path := filepath.Join(h.dir, "journal.ndjson")
	if err := h.rt.Stop(5 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(raw) == 0 || !strings.Contains(string(raw), "\"schemaVersion\":1") {
		t.Fatalf("journal content = %q", raw)
	}
}

// 只配了 MQTT（未实现）时必须显式告警，而不是让队列无声增长。
func TestMQTTEnabledWarnsButDoesNotCreateSink(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		cfg["northbound"] = map[string]any{
			"mqtt": map[string]any{"enabled": true, "broker": "tcp://127.0.0.1:1883"},
		}
	})
	h.waitPointGood("sim01", "reg0")

	if code, body := h.do(http.MethodPost, "/api/v1/commands", `{"action":"outbox.flush"}`); code != 409 {
		t.Fatalf("flush should be 409 with no sink: %d %s", code, body)
	}
	logs := h.rt.Logs("", "warn", 50)
	found := false
	for _, e := range logs {
		if e.Message == "mqtt_sink_not_implemented" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected explicit warning, logs = %+v", logs)
	}
}

// webhook 启用但没给 URL：不能静默成功。
func TestWebhookEnabledWithoutURL(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		cfg["northbound"] = map[string]any{"webhook": map[string]any{"enabled": true}}
	})
	h.waitPointGood("sim01", "reg0")
	logs := h.rt.Logs("", "warn", 50)
	found := false
	for _, e := range logs {
		if e.Message == "webhook_enabled_without_url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warning, logs = %+v", logs)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
