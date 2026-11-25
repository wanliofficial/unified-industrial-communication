package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	sim "uic/internal/sim/modbus"
)

// 端到端：真实 Modbus TCP 回环（内置从站）+ 真实驱动 + 真实 HTTP 层。
type harness struct {
	t     *testing.T
	rt    *Runtime
	srv   *httptest.Server
	sim   *sim.Server
	dir   string
	token string
	// actor 覆盖身份。空表示沿用旧行为（带 Token 时以 api/admin 身份调用）。
	actor string
}

func newHarness(t *testing.T, mutate func(cfg map[string]any)) *harness {
	return newHarnessAt(t, t.TempDir(), mutate)
}

// newHarnessAt 与 newHarness 相同，但用调用方指定的数据目录。用于「同站点
// 多实例」（同一 site、不同数据目录）与「同数据目录重启」两类场景的测试。
func newHarnessAt(t *testing.T, dir string, mutate func(cfg map[string]any)) *harness {
	t.Helper()
	return newHarnessTuned(t, dir, mutate, nil)
}

// newHarnessTuned 在 newHarnessAt 之上允许调整 runtime.Options（如注入更短的
// StallAfter / 更快的 HeartbeatInterval）。看门狗类测试需要在不等待 15s 的前提下
// 观察卡死判定，因此必须能从构造层注入。
func newHarnessTuned(t *testing.T, dir string, mutate func(cfg map[string]any), tune func(*Options)) *harness {
	t.Helper()

	srv, err := sim.NewServer("127.0.0.1:0", 256)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	srv.SetHolding(0, 1234, 5678, 42)
	srv.SetHolding(5, 0)
	srv.SetHolding(20, 0x0008)
	srv.SetCoil(0, true)

	cfg := map[string]any{
		"schemaVersion": 1,
		"site":          "test-site",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": dir, "journal": true, "outboxMaxRecords": 1000},
		"logging":       map[string]any{"level": "info"},
		"channels": []any{map[string]any{
			"id": "ch-modbus", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": srv.Addr()},
			"concurrency": 2, "timeout": "1s",
		}},
		"devices": []any{map[string]any{
			"id": "sim01", "name": "sim", "channel": "ch-modbus",
			"unitId": 1, "interval": "100ms", "timeout": "1s", "enabled": true,
			"points": []any{
				map[string]any{
					"id": "reg0", "dataType": "uint16", "enabled": true,
					"address": map[string]any{"kind": "register", "area": "holding", "offset": 0, "length": 1, "bit": -1},
				},
				map[string]any{
					"id": "reg10", "dataType": "uint16", "enabled": true,
					"address": map[string]any{"kind": "register", "area": "holding", "offset": 10, "length": 1, "bit": -1},
				},
				map[string]any{
					"id": "write_me", "dataType": "uint16", "writable": true, "enabled": true,
					"address": map[string]any{"kind": "register", "area": "holding", "offset": 5, "length": 1, "bit": -1},
				},
				map[string]any{
					"id": "coil0", "dataType": "bool", "enabled": true,
					"address": map[string]any{"kind": "register", "area": "coil", "offset": 0, "length": 1, "bit": -1},
				},
				map[string]any{
					"id": "bit3", "dataType": "bool", "enabled": true,
					"address": map[string]any{"kind": "register", "area": "holding", "offset": 20, "length": 1, "bit": 3},
				},
				map[string]any{
					"id": "bit0", "dataType": "bool", "enabled": true,
					"address": map[string]any{"kind": "register", "area": "holding", "offset": 20, "length": 1, "bit": 0},
				},
				map[string]any{
					"id": "readonly_probe", "dataType": "uint16", "enabled": true,
					"address": map[string]any{"kind": "register", "area": "input", "offset": 0, "length": 1, "bit": -1},
				},
			},
		}},
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

	opt := Options{ConfigPath: path, DataDir: dir, Version: "test", MetricsPublic: true}
	if tune != nil {
		tune(&opt)
	}
	rt, err := New(opt)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	h := &harness{t: t, rt: rt, sim: srv, dir: dir, token: rt.Token()}
	h.srv = httptest.NewServer(rt.Handler())
	t.Cleanup(func() {
		h.srv.Close()
		cancel()
		_ = rt.Stop(5 * time.Second)
		_ = srv.Close()
	})
	return h
}

func (h *harness) do(method, path, body string) (int, []byte) {
	h.t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if h.actor != "" {
		req.Header.Set("Authorization", "Bearer "+h.actor)
	} else if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (h *harness) waitPointGood(device, point string) model.Sample {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last model.Sample
	for time.Now().Before(deadline) {
		s, ok := h.rt.Point(device, point)
		if ok && s.Quality == model.QualityGood {
			return s
		}
		last = s
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("point %s/%s never became good: %+v", device, point, last)
	return model.Sample{}
}

func TestRuntimeEndToEnd(t *testing.T) {
	h := newHarness(t, nil)

	reg0 := h.waitPointGood("sim01", "reg0")
	if reg0.Value.Num != 1234 {
		t.Fatalf("reg0 = %+v", reg0.Value)
	}
	if s := h.waitPointGood("sim01", "coil0"); !s.Value.Bool {
		t.Fatalf("coil0 = %+v", s.Value)
	}
	if s := h.waitPointGood("sim01", "bit3"); !s.Value.Bool {
		t.Fatal("bit3 should be true")
	}
	if s := h.waitPointGood("sim01", "bit0"); s.Value.Bool {
		t.Fatal("bit0 should be false")
	}

	// health：设备在采集，点位全部 good。
	//
	// Outbox 的投递是异步的：publish 只是入队 + 唤醒 dispatchLoop，真正
	// drain 取决于调度。拿到第一个 good 样本的瞬间就断言 Delivered>0，测的
	// 是 goroutine 的调度时机而不是补发语义——GOMAXPROCS=1 或机器繁忙时
	// 会稳定失败。所以这里等它收敛。
	//
	// 同理，「设备态已 collecting、点位全 good」也不能一次性断言：点位是
	// 逐点 cache.Set，设备态要到本轮 finish 才转 collecting，中途必然存在
	// 「读到半个批次」的窗口（Good=6、Bad=1，设备还是 connecting）。健康检查
	// 本身是瞬时快照，断言它的一瞬间等价于断言调度器的抢占顺序。
	code, body := h.do(http.MethodGet, "/api/v1/health", "")
	if code != 200 {
		t.Fatalf("health = %d %s", code, body)
	}
	var health httpapi.Health
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	healthDeadline := time.Now().Add(10 * time.Second)
	for {
		if health.Devices.Collecting == 1 && health.Points.Good > 0 && health.Points.Bad == 0 {
			break
		}
		if time.Now().After(healthDeadline) {
			t.Fatalf("health never converged: %+v", health)
		}
		time.Sleep(10 * time.Millisecond)
		code, body = h.do(http.MethodGet, "/api/v1/health", "")
		if code != 200 {
			t.Fatalf("health = %d %s", code, body)
		}
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatal(err)
		}
	}
	deliverDeadline := time.Now().Add(10 * time.Second)
	for health.Outbox.Delivered == 0 {
		if time.Now().After(deliverDeadline) {
			t.Fatalf("journal sink should have delivered events: %+v", health.Outbox)
		}
		time.Sleep(10 * time.Millisecond)
		code, body = h.do(http.MethodGet, "/api/v1/health", "")
		if code != 200 {
			t.Fatalf("health = %d %s", code, body)
		}
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatal(err)
		}
	}

	// 设备视图：配置态与运行态分别返回。
	//
	// 注意这里有两次「先值后账」的时序：点位进 cache 发生在本轮采集内，
	// 而 readsTotal 在收尾记账时才 +1。所以缓存已经是 good 之后的一瞬间，
	// 读计数仍然是 0。断言必须等到记账完成，否则就是在测调度器的调度时机。
	//
	// 同理，`runtimeState` 是瞬时值：-race 下并发跑多个包时，某一次读可能
	// 因为 CPU 抢占而超时，让设备短暂落到 degraded。这里等的是「收敛到
	// collecting」而不是「任意时刻都是 collecting」——后者会把调度抖动
	// 当成产品缺陷。
	var dv httpapi.DeviceView
	statsDeadline := time.Now().Add(10 * time.Second)
	for {
		code, body = h.do(http.MethodGet, "/api/v1/devices/sim01", "")
		if code != 200 {
			t.Fatalf("device = %d %s", code, body)
		}
		if err := json.Unmarshal(body, &dv); err != nil {
			t.Fatal(err)
		}
		ok := dv.Stats.ReadsTotal > 0 && dv.Points.Good > 0 &&
			dv.ConfiguredState == "enabled" && dv.RuntimeState == "collecting"
		if ok {
			break
		}
		if time.Now().After(statsDeadline) {
			t.Fatalf("device view never converged: %+v", dv)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 写入 + 回读确认。
	code, body = h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":777},"confirm":true}]}`)
	if code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}
	var wr struct {
		Results []httpapi.WriteResult `json:"results"`
	}
	if err := json.Unmarshal(body, &wr); err != nil {
		t.Fatal(err)
	}
	if len(wr.Results) != 1 || !wr.Results[0].OK || wr.Results[0].Quality != model.QualityGood {
		t.Fatalf("write results = %+v", wr.Results)
	}
	if got := h.sim.Holding(5); got != 777 {
		t.Fatalf("sim holding[5] = %d, want 777", got)
	}

	// 只读点位与未知点位必须逐项失败，且整体仍是 200。
	code, body = h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"reg0","value":{"type":"uint16","num":1}},{"point":"ghost","value":{"type":"uint16","num":1}},{"point":"write_me","value":{"type":"float32","num":1}}]}`)
	if code != 200 {
		t.Fatalf("partial write = %d %s", code, body)
	}
	if err := json.Unmarshal(body, &wr); err != nil {
		t.Fatal(err)
	}
	if len(wr.Results) != 3 {
		t.Fatalf("results = %+v", wr.Results)
	}
	if wr.Results[0].Code != "point.not_writable" {
		t.Fatalf("readonly code = %q", wr.Results[0].Code)
	}
	if wr.Results[1].Code != "point.not_found" {
		t.Fatalf("unknown code = %q", wr.Results[1].Code)
	}
	if wr.Results[2].Code != "value.type_mismatch" {
		t.Fatalf("type code = %q", wr.Results[2].Code)
	}

	// 幂等键：第二次相同 key 不得重复写入。
	code, _ = h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":100}}]}`)
	if code != 200 {
		t.Fatalf("write1 = %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/points/sim01/write",
		strings.NewReader(`{"writes":[{"point":"write_me","value":{"type":"uint16","num":200}}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", "e2e-1")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// 同一个 key 再发一次，值不同但应返回缓存结果。
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/points/sim01/write",
		strings.NewReader(`{"writes":[{"point":"write_me","value":{"type":"uint16","num":201}}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", "e2e-1")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var replay struct {
		Results []httpapi.WriteResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(replay.Results) != 1 || !replay.Results[0].OK {
		t.Fatalf("idempotent replay = %+v", replay.Results)
	}
	time.Sleep(300 * time.Millisecond)
	if got := h.sim.Holding(5); got == 201 {
		t.Fatal("idempotent key must prevent the second write")
	}

	// 命令白名单。
	code, body = h.do(http.MethodPost, "/api/v1/commands",
		`{"action":"driver.restart","params":{"device":"sim01"}}`)
	if code != 200 {
		t.Fatalf("restart = %d %s", code, body)
	}
	var rres struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(body, &rres); err != nil {
		t.Fatal(err)
	}
	if gen, ok := rres.Result["generation"].(float64); !ok || gen < 1 {
		t.Fatalf("generation = %+v", rres.Result)
	}
	// 重启后应重新采集成功。
	h.waitPointGood("sim01", "reg0")

	code, body = h.do(http.MethodPost, "/api/v1/commands", `{"action":"nope"}`)
	if code != 400 {
		t.Fatalf("unknown action = %d %s", code, body)
	}
	code, body = h.do(http.MethodPost, "/api/v1/commands", `{"action":"outbox.drop","params":{}}`)
	if code != 400 {
		t.Fatalf("drop without confirm = %d %s", code, body)
	}

	// 指标暴露了设备与队列。
	code, body = h.do(http.MethodGet, "/api/v1/metrics", "")
	if code != 200 {
		t.Fatalf("metrics = %d", code)
	}
	for _, want := range []string{"uic_collect_total", "uic_device_state", "uic_outbox_pending"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics missing %s", want)
		}
	}

	// journal 真实落盘。
	//
	// journalSink 一次 Write 写入整批（多行）；读者可能在写入中途读到半行。
	// 因此只认「已以 \n 结尾」的行，解析失败就继续等，而不是把半行当成
	// 写坏了——这同样是读-写并发下的观察时机问题，不是产品缺陷。
	journal := filepath.Join(h.dir, "journal.ndjson")
	deadline := time.Now().Add(5 * time.Second)
	var lastLine string
	for {
		if b, err := os.ReadFile(journal); err == nil {
			if i := strings.IndexByte(string(b), '\n'); i >= 0 {
				line := string(b)[:i]
				lastLine = line
				var ev map[string]any
				if err := json.Unmarshal([]byte(line), &ev); err == nil {
					if ev["schemaVersion"] != float64(1) || ev["device"] != "sim01" {
						t.Fatalf("journal payload = %v", ev)
					}
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal.ndjson never received a complete record (last line %q)", lastLine)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 配置热更新：只重启指纹变化的设备。
	raw, err := h.rt.Config()
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	devs := cfg["devices"].([]any)
	dev := devs[0].(map[string]any)
	dev["interval"] = "200ms"
	patched, _ := json.Marshal(cfg)
	code, body = h.do(http.MethodPost, "/api/v1/config/apply", string(patched))
	if code != 200 {
		t.Fatalf("apply = %d %s", code, body)
	}
	var applied map[string]any
	json.Unmarshal(body, &applied)
	if applied["applied"] != true {
		t.Fatalf("apply result = %v", applied)
	}
	if restarted, _ := applied["restartedDevices"].([]any); len(restarted) != 1 || restarted[0] != "sim01" {
		t.Fatalf("restarted = %v", applied["restartedDevices"])
	}

	// 非法配置：422，且运行时不受影响。
	code, _ = h.do(http.MethodPost, "/api/v1/config/apply", `{"schemaVersion":1,"site":"X","channels":[],"devices":[]}`)
	if code != 422 {
		t.Fatalf("invalid apply = %d", code)
	}
	h.waitPointGood("sim01", "reg0")

	// 回滚到上一版本。
	code, body = h.do(http.MethodGet, "/api/v1/config/versions", "")
	if code != 200 {
		t.Fatalf("versions = %d", code)
	}
	var vers struct {
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal(body, &vers)
	if len(vers.Items) < 2 {
		t.Fatalf("versions = %+v", vers.Items)
	}
	var target string
	for _, v := range vers.Items {
		if v["current"] != true {
			target = v["version"].(string)
			break
		}
	}
	code, body = h.do(http.MethodPost, "/api/v1/config/rollback", `{"version":"`+target+`"}`)
	if code != 200 || !strings.Contains(string(body), `"applied":true`) {
		t.Fatalf("rollback = %d %s", code, body)
	}
}

// 配置里没有 sink 时 Outbox 必须整体关闭，而不是静默占盘。
func TestRuntimeWithoutSinks(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		cfg["storage"] = map[string]any{"dir": cfg["storage"].(map[string]any)["dir"], "journal": false}
	})
	h.waitPointGood("sim01", "reg0")

	code, body := h.do(http.MethodPost, "/api/v1/commands", `{"action":"outbox.flush"}`)
	if code != 409 {
		t.Fatalf("flush without sink = %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "outbox")); err == nil {
		t.Fatal("outbox dir must not exist when no sink is configured")
	}
}

// 设备不可达时点位必须是 bad/comm_error，而不是保留旧好值。
func TestRuntimeMarksUnreachablePoints(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		// 指向一个几乎不可能有人监听的端口。
		cfg["channels"] = []any{map[string]any{
			"id": "ch-modbus", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": "127.0.0.1:1"},
			"concurrency": 1, "timeout": "200ms",
		}}
		cfg["devices"].([]any)[0].(map[string]any)["interval"] = "100ms"
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := h.rt.Point("sim01", "reg0")
		if ok && s.Quality != model.QualityGood && s.Quality != model.QualityNotConnected {
			if s.Quality != model.QualityCommError && s.Quality != model.QualityTimeout {
				t.Fatalf("unexpected quality %s", s.Quality)
			}
			// health 必须承认这是降级。
			code, body := h.do(http.MethodGet, "/api/v1/health", "")
			if code != 200 {
				t.Fatalf("health = %d", code)
			}
			var health httpapi.Health
			json.Unmarshal(body, &health)
			if health.Status == "ok" || len(health.Problems) == 0 {
				t.Fatalf("health should report a problem: %+v", health)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("points never degraded")
}

func TestRegisterBuiltinsIdempotent(t *testing.T) {
	if err := RegisterBuiltins(); err != nil {
		t.Fatal(err)
	}
	if err := RegisterBuiltins(); err != nil {
		t.Fatalf("second registration must be a no-op: %v", err)
	}
}

func TestRedaction(t *testing.T) {
	raw := []byte(`{"server":{"token":"supersecret"},"northbound":{"webhook":{"headers":{"Authorization":"Bearer x"},"url":"http://x"}},"mqtt":{"password":"pw"}}`)
	out, err := redactJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, leak := range []string{"supersecret", "Bearer x", `"pw"`} {
		if strings.Contains(s, leak) {
			t.Fatalf("leaked %q in %s", leak, s)
		}
	}
	if !strings.Contains(s, "http://x") {
		t.Fatalf("non-secret values must survive: %s", s)
	}
}

func TestErrTextAndHash(t *testing.T) {
	if errText(nil) != "" {
		t.Fatal("nil error must render empty")
	}
	if errText(fmt.Errorf("boom")) != "boom" {
		t.Fatal("error text lost")
	}
	a := hashJSON(map[string]string{"x": "1"})
	b := hashJSON(map[string]string{"x": "1"})
	c := hashJSON(map[string]string{"x": "2"})
	if a != b || a == c || len(a) == 0 {
		t.Fatalf("hash unstable: %q %q %q", a, b, c)
	}
}

// 写入时省略 value.type：运行时按点位 dataType 补齐。回读确认必须用
// 「补齐后的值」比较，否则空类型永远不等于读回类型，一次成功的写入会被
// 报成 uncertain——前端看到的提示是「读回值不一致」，而设备里其实已经写进去了。
func TestWriteConfirmWithoutExplicitType(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "write_me")

	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"num":321},"confirm":true}]}`)
	if code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}
	var wr struct {
		Results []httpapi.WriteResult `json:"results"`
	}
	if err := json.Unmarshal(body, &wr); err != nil {
		t.Fatal(err)
	}
	if len(wr.Results) != 1 || !wr.Results[0].OK {
		t.Fatalf("results = %+v", wr.Results)
	}
	if wr.Results[0].Quality != model.QualityGood {
		t.Fatalf("quality = %s (%s)", wr.Results[0].Quality, wr.Results[0].Message)
	}
	if got := h.sim.Holding(5); got != 321 {
		t.Fatalf("sim holding[5] = %d, want 321", got)
	}
}

func TestValuesMatch(t *testing.T) {
	num := func(typ model.DataType, v float64) model.Value { return model.NumValue(typ, v) }
	cases := []struct {
		name string
		got  model.Value
		want model.Value
		ok   bool
	}{
		{"identical int", num(model.TypeUint16, 777), num(model.TypeUint16, 777), true},
		{"different int", num(model.TypeUint16, 778), num(model.TypeUint16, 777), false},
		{"int vs float tag", num(model.TypeFloat32, 62), num(model.TypeUint32, 62), true},
		{"float32 rounding", num(model.TypeFloat32, 0.10000000149011612), num(model.TypeFloat32, 0.1), true},
		{"float32 real difference", num(model.TypeFloat32, 0.11), num(model.TypeFloat32, 0.1), false},
		{"float64 rounding", num(model.TypeFloat64, 0.30000000000000004), num(model.TypeFloat64, 0.3), true},
		{"bool same", model.BoolValue(true), model.BoolValue(true), true},
		{"bool differs", model.BoolValue(false), model.BoolValue(true), false},
		{"bool vs number", num(model.TypeUint16, 1), model.BoolValue(true), false},
		{"string same", model.TextValue(model.TypeString, "PUMP-01"), model.TextValue(model.TypeString, "PUMP-01"), true},
		{"string differs", model.TextValue(model.TypeString, "PUMP-02"), model.TextValue(model.TypeString, "PUMP-01"), false},
		{"string vs bytes", model.BytesValue([]byte("x")), model.TextValue(model.TypeString, "x"), false},
		{"empty type", num("", 1), num("", 1), true},
	}
	for _, tc := range cases {
		if got := valuesMatch(tc.got, tc.want); got != tc.ok {
			t.Fatalf("%s: valuesMatch = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
