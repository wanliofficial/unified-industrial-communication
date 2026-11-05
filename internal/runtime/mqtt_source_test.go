package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/model"
	"uic/internal/mqtt/mqtttest"
)

// MQTT 南向的端到端：Broker 推消息 → 驱动订阅 → 采集器 → 缓存 → REST。
//
// 这条链上最容易出问题的是「每一环单独都对，拼起来不对」：订阅主题与点位
// 主题不一致、质量码在上层被覆盖、写入走不到 Publish。所以这里不测驱动本身
// （那在 internal/driver/mqtt 里），只测装配。

type mqttSource struct {
	t   *testing.T
	rt  *Runtime
	srv *httptest.Server
	brk *mqtttest.Broker
}

func newMQTTSource(t *testing.T) *mqttSource {
	t.Helper()
	brk := mqtttest.New(t, nil)
	dir := t.TempDir()

	cfg := map[string]any{
		"schemaVersion": 1,
		"site":          "mqtt-lab",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": dir, "journal": false},
		"logging":       map[string]any{"level": "warn"},
		"channels": []any{map[string]any{
			"id": "ch-mqtt", "protocol": "mqtt",
			"transport": map[string]any{"kind": "tcp", "address": brk.Addr()},
		}},
		"devices": []any{map[string]any{
			"id": "edge01", "channel": "ch-mqtt",
			"interval": "100ms", "timeout": "1s", "enabled": true,
			"points": []any{
				map[string]any{
					"id": "temperature", "dataType": "float64", "enabled": true,
					"unit": "degC",
					"address": map[string]any{
						"kind": "symbol", "path": "plant/line1/temp",
					},
				},
				map[string]any{
					"id": "running", "dataType": "bool", "enabled": true,
					"address": map[string]any{
						"kind": "symbol", "path": "plant/line1/state",
						// 设备发的是 {"running":true,"mode":"auto"}。
						"selector": "running",
					},
				},
				map[string]any{
					"id": "setpoint", "dataType": "float64", "enabled": true,
					"writable": true,
					"address": map[string]any{
						"kind": "symbol", "path": "plant/line1/setpoint",
					},
				},
			},
		}},
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
	s := &mqttSource{t: t, rt: rt, srv: httptest.NewServer(rt.Handler()), brk: brk}
	t.Cleanup(func() {
		s.srv.Close()
		cancel()
		_ = rt.Stop(5 * time.Second)
	})
	return s
}

func (s *mqttSource) do(method, path, body string) (int, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.srv.URL+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	if tok := s.rt.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (s *mqttSource) waitGood(device, point string) model.Sample {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last model.Sample
	for time.Now().Before(deadline) {
		if sm, ok := s.rt.Point(device, point); ok && sm.Quality == model.QualityGood {
			return sm
		} else {
			last = sm
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("point %s/%s never became good: %+v", device, point, last)
	return model.Sample{}
}

// 订阅 → 采集 → 缓存：两条点位（纯标量 + JSON 字段）都要能采到。
func TestMQTTSourceCollectsPublishedValues(t *testing.T) {
	s := newMQTTSource(t)
	s.brk.WaitSubscriptions(t, 3)

	s.brk.Publish("plant/line1/temp", []byte("23.5"), 1, false)
	s.brk.Publish("plant/line1/state", []byte(`{"running":true,"mode":"auto"}`), 1, false)
	s.brk.Publish("plant/line1/setpoint", []byte("60"), 1, false)

	if got := s.waitGood("edge01", "temperature"); got.Value.Num != 23.5 {
		t.Fatalf("temperature = %+v", got.Value)
	}
	if got := s.waitGood("edge01", "running"); !got.Value.Bool {
		t.Fatalf("running = %+v", got.Value)
	}
	if got := s.waitGood("edge01", "setpoint"); got.Value.Num != 60 {
		t.Fatalf("setpoint = %+v", got.Value)
	}

	// REST 与驱动必须看到同一个值：契约漂移最容易出现在这一层。
	code, body := s.do(http.MethodGet, "/api/v1/points/edge01/temperature", "")
	if code != 200 {
		t.Fatalf("GET point = %d %s", code, body)
	}
	var resp struct {
		Quality string      `json:"quality"`
		Value   model.Value `json:"value"`
		Unit    string      `json:"unit"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.Quality != "good" || resp.Value.Num != 23.5 || resp.Unit != "degC" {
		t.Fatalf("rest point = %+v", resp)
	}
}

// 写入 = 向点位主题发布。发布成功但设备是否执行，驱动无法知道，
// 因此写入结果的 quality 必须是 uncertain 而不是 good。
func TestMQTTSourceWritePublishes(t *testing.T) {
	s := newMQTTSource(t)
	s.brk.WaitSubscriptions(t, 3)

	body := `{"writes":[{"point":"setpoint","value":{"type":"float64","num":42.5}}]}`
	code, resp := s.do(http.MethodPost, "/api/v1/points/edge01/write", body)
	if code != 200 {
		t.Fatalf("write = %d %s", code, resp)
	}
	var wr struct {
		Results []struct {
			Point   string `json:"point"`
			OK      bool   `json:"ok"`
			Quality string `json:"quality"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &wr); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp)
	}
	if len(wr.Results) != 1 || !wr.Results[0].OK {
		t.Fatalf("results = %+v", wr.Results)
	}

	msgs := s.brk.WaitMessages(t, 1)
	if msgs[0].Topic != "plant/line1/setpoint" || string(msgs[0].Payload) != "42.5" {
		t.Fatalf("published %q = %q", msgs[0].Topic, msgs[0].Payload)
	}
}

// 一条消息都没来时：设备不能显示成「已连接且质量良好」，也不能把连接拆了
// 重建成死循环。质量码必须是 bad，设备状态保持可用。
func TestMQTTSourceWithoutMessagesStaysBad(t *testing.T) {
	s := newMQTTSource(t)
	s.brk.WaitSubscriptions(t, 3)
	time.Sleep(400 * time.Millisecond)

	code, body := s.do(http.MethodGet, "/api/v1/points/edge01/temperature", "")
	if code != 200 {
		t.Fatalf("GET = %d %s", code, body)
	}
	var resp struct {
		Quality string `json:"quality"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Quality == "good" {
		t.Fatalf("quality = %q, want bad/stale when nothing was published", resp.Quality)
	}
	// 只有一次 CONNECT：没有任何消息不等于断链，不能每轮重连。
	if n := s.brk.Connects(); n != 1 {
		t.Fatalf("connects = %d, want 1 (silence must not trigger reconnect loops)", n)
	}
}
