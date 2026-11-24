package runtime

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
)

// 命令白名单：每个动作的参数校验与错误码都要能对上文档。
func TestCommandWhitelist(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	type cmdCase struct {
		name   string
		body   string
		code   int
		errKey string
	}
	cases := []cmdCase{
		{"missing device", `{"action":"driver.restart","params":{}}`, 400, "request.invalid"},
		{"unknown device", `{"action":"driver.restart","params":{"device":"ghost"}}`, 404, "device.not_found"},
		{"selftest ok", `{"action":"device.selftest","params":{"device":"sim01"}}`, 200, ""},
		{"selftest ghost", `{"action":"device.selftest","params":{"device":"ghost"}}`, 404, "device.not_found"},
		{"flush", `{"action":"outbox.flush"}`, 200, ""},
		{"drop needs confirm", `{"action":"outbox.drop","params":{}}`, 400, "request.confirm_required"},
		{"drop confirmed", `{"action":"outbox.drop","params":{"confirm":true}}`, 200, ""},
		{"rollback needs confirm", `{"action":"config.rollback","params":{"version":"sha256:x"}}`, 400, "request.confirm_required"},
		{"rollback bad version", `{"action":"config.rollback","params":{"version":"sha256:x","confirm":true}}`, 404, "config.version_not_found"},
		{"log level invalid", `{"action":"log.level","params":{"level":"verbose"}}`, 400, "request.invalid"},
		{"log level ok", `{"action":"log.level","params":{"level":"debug"}}`, 200, ""},
		{"diag hint", `{"action":"diag.bundle"}`, 200, ""},
		{"unknown action", `{"action":"shell.exec"}`, 400, "request.unknown_action"},
	}
	for _, tc := range cases {
		code, body := h.do(http.MethodPost, "/api/v1/commands", tc.body)
		if code != tc.code {
			t.Fatalf("%s: code = %d body=%s", tc.name, code, body)
		}
		if tc.errKey != "" {
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("%s: %v (%s)", tc.name, err, body)
			}
			if env.Error.Code != tc.errKey {
				t.Fatalf("%s: error code = %q, want %q", tc.name, env.Error.Code, tc.errKey)
			}
		}
	}
}

// 配置端点：validate / versions / 单版本 / diff / diagnostics 全部要走真实装配。
func TestConfigAndDiagnosticsEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	// validate：合法与非法。
	raw, err := h.rt.Config()
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.do(http.MethodPost, "/api/v1/config/validate", string(raw))
	if code != 200 || !strings.Contains(string(body), `"valid":true`) {
		t.Fatalf("validate = %d %s", code, body)
	}
	code, body = h.do(http.MethodPost, "/api/v1/config/validate", `{"schemaVersion":1,"site":"BAD SITE"}`)
	if code != 422 {
		t.Fatalf("invalid validate = %d %s", code, body)
	}

	// 写一个新区间版本，再列版本、取单版本、做 diff。
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["devices"].([]any)[0].(map[string]any)["interval"] = "150ms"
	patched, _ := json.Marshal(cfg)
	code, body = h.do(http.MethodPost, "/api/v1/config/apply", string(patched))
	if code != 200 {
		t.Fatalf("apply = %d %s", code, body)
	}

	var vers struct {
		Items []struct {
			Version string `json:"version"`
			Current bool   `json:"current"`
		} `json:"items"`
	}
	code, body = h.do(http.MethodGet, "/api/v1/config/versions", "")
	if code != 200 {
		t.Fatalf("versions = %d", code)
	}
	if err := json.Unmarshal(body, &vers); err != nil {
		t.Fatal(err)
	}
	if len(vers.Items) < 2 {
		t.Fatalf("versions = %+v", vers.Items)
	}
	var cur, prev string
	for _, v := range vers.Items {
		if v.Current {
			cur = v.Version
		} else if prev == "" {
			prev = v.Version
		}
	}
	if cur == "" || prev == "" {
		t.Fatalf("could not resolve versions: %+v", vers.Items)
	}

	code, body = h.do(http.MethodGet, "/api/v1/config/versions/"+cur, "")
	if code != 200 || !strings.Contains(string(body), "150ms") {
		t.Fatalf("version body = %d %s", code, body)
	}
	code, body = h.do(http.MethodGet, "/api/v1/config/versions/sha256:deadbeef", "")
	if code != 404 {
		t.Fatalf("missing version = %d %s", code, body)
	}

	code, body = h.do(http.MethodGet, "/api/v1/config/diff?from="+prev+"&to="+cur, "")
	if code != 200 {
		t.Fatalf("diff = %d %s", code, body)
	}
	var d config.Diff
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.DevicesModified) != 1 || d.DevicesModified[0] != "sim01" {
		t.Fatalf("diff = %+v", d)
	}

	// 诊断包必须是能打开的 zip，且含关键文件与脱敏后的配置。
	code, body = h.do(http.MethodGet, "/api/v1/diagnostics/bundle", "")
	if code != 200 {
		t.Fatalf("bundle = %d", code)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("bundle is not a zip: %v", err)
	}
	names := map[string]bool{}
	var manifest map[string]any
	for _, f := range zr.File {
		names[f.Name] = true
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		content, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "manifest.json" {
			if err := json.Unmarshal(content, &manifest); err != nil {
				t.Fatalf("manifest: %v", err)
			}
		}
		if f.Name == "config.redacted.json" && h.token != "" && strings.Contains(string(content), h.token) {
			t.Fatalf("bundle leaked the API token")
		}
	}
	for _, want := range []string{"manifest.json", "config.redacted.json", "health.json", "metrics.txt", "logs/uic.json", "devices.json", "outbox.json"} {
		if !names[want] {
			t.Fatalf("bundle missing %s (got %v)", want, names)
		}
	}
	if manifest["version"] == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	// 诊断包必须带上实例身份：同站点多实例部署时，仅凭 site 无法区分一份
	// 诊断包来自哪台实例，而它与 MQTT clientID/Sparkplug nodeId 同源。
	if manifest["instanceId"] == "" || manifest["instanceId"] == nil {
		t.Fatalf("manifest 缺 instanceId: %+v", manifest)
	}

	// 命令形式只给提示，不返回 payload。
	code, body = h.do(http.MethodPost, "/api/v1/commands", `{"action":"diag.bundle"}`)
	if code != 200 || !strings.Contains(string(body), "diagnostics/bundle") {
		t.Fatalf("diag hint = %d %s", code, body)
	}
}

// 访问器：SSE、设备列表、点位过滤、日志、ready。
func TestReadAccessors(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	if h.rt.ListenAddr() != "127.0.0.1:0" {
		t.Fatalf("listen addr = %q", h.rt.ListenAddr())
	}
	if h.rt.Logger() == nil || h.rt.Ring() == nil || h.rt.ConfigManager() == nil {
		t.Fatal("accessors must not be nil")
	}

	devs := h.rt.Devices("")
	if len(devs) != 1 || devs[0].ID != "sim01" {
		t.Fatalf("devices = %+v", devs)
	}
	if got := h.rt.Devices("sim"); len(got) != 1 {
		t.Fatalf("filtered devices = %+v", got)
	}
	if _, ok := h.rt.Device("ghost"); ok {
		t.Fatal("ghost device must not exist")
	}

	// 点位过滤：按设备 + 质量 + 仅变化/Q。
	all := h.rt.Points(httpapi.PointFilter{})
	if len(all) != 7 {
		t.Fatalf("points = %d", len(all))
	}
	if got := h.rt.Points(httpapi.PointFilter{Device: "sim01", Quality: []string{string(model.QualityGood)}}); len(got) == 0 {
		t.Fatal("quality filter returned nothing")
	}
	if got := h.rt.Points(httpapi.PointFilter{Device: "ghost"}); len(got) != 0 {
		t.Fatalf("ghost points = %d", len(got))
	}

	// SSE 流：订阅后制造一次值变化，必须能收到样例事件。
	ch, cancel := h.rt.Stream(func(s model.Sample) bool { return s.DeviceID == "sim01" }, 8)
	defer cancel()
	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":4321}}]}`)
	if code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed")
			}
			if ev.Type == "sample" || ev.Type == "quality" {
				return
			}
		case <-deadline:
			t.Fatal("no stream event within 3s")
		}
	}
}

func TestReadyAndLogs(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	r := h.rt.Ready()
	if !r.Ready {
		t.Fatalf("ready = %+v", r)
	}
	if len(r.Checks) == 0 {
		t.Fatal("ready checks must not be empty")
	}

	if h.rt.Version().Version == "" {
		t.Fatal("version must be set")
	}

	// 触发一条错误日志，再按设备与级别过滤。
	h.rt.log.Error("probe boom", "device", "sim01")
	logs := h.rt.Logs("sim01", "error", 10)
	found := false
	for _, e := range logs {
		if strings.Contains(e.Message, "probe boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs = %+v", logs)
	}
	if got := h.rt.Logs("ghost", "", 10); len(got) != 0 {
		t.Fatalf("ghost logs = %+v", got)
	}
}

// 不可达设备：先降级、再熔断、再探活；整个过程 health 必须持续反映问题。
func TestUnreachableDeviceReachesFaultedAndProbes(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		cfg["channels"] = []any{map[string]any{
			"id": "ch-modbus", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": "127.0.0.1:1"},
			"concurrency": 1, "timeout": "100ms",
		}}
		cfg["devices"].([]any)[0].(map[string]any)["interval"] = "120ms"
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := h.rt.Device("sim01")
		if ok && v.RuntimeState == "faulted" {
			// 熔断后必须进入探活，而不是继续空转采集。
			code, body := h.do(http.MethodGet, "/api/v1/health", "")
			if code != 200 {
				t.Fatalf("health = %d", code)
			}
			var health httpapi.Health
			if err := json.Unmarshal(body, &health); err != nil {
				t.Fatal(err)
			}
			if health.Status == "ok" {
				t.Fatalf("health must report a problem while faulted: %+v", health)
			}
			if health.Devices.Faulted == 0 {
				t.Fatalf("faulted counter = %+v", health.Devices)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("device never reached faulted state")
}

// selftest 必须走真实驱动探活；不可达设备应返回失败而不是假装成功。
func TestSelftestOnUnreachableDeviceFails(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		cfg["channels"] = []any{map[string]any{
			"id": "ch-modbus", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": "127.0.0.1:1"},
			"concurrency": 1, "timeout": "200ms",
		}}
	})
	code, body := h.do(http.MethodPost, "/api/v1/commands",
		`{"action":"device.selftest","params":{"device":"sim01"}}`)
	if code == 200 {
		t.Fatalf("selftest must fail for unreachable device: %s", body)
	}
}

// 缺少配置文件时 New 必须报错，而不是起一个空服务假装健康。
func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without config path must fail")
	}
}

var _ = errors.New
