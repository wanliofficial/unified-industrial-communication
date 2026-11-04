package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/cache"
	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/obs"
)

// fakeBackend 实现全部 Backend 能力，只用内存数据，便于测接口契约。
type fakeBackend struct {
	health  Health
	ready   Ready
	version VersionInfo
	metrics string
	logs    []obs.Entry

	devices []DeviceView
	samples map[string]model.Sample
	writeFn func(ctx context.Context, device string, items []WriteItem) ([]WriteResult, error)

	cfg       json.RawMessage
	versions  []config.VersionInfo
	configs   map[string]json.RawMessage
	diff      config.Diff
	applyRes  config.ApplyResult
	rollbackR config.ApplyResult

	commandFn func(ctx context.Context, action string, params map[string]any) (any, error)

	streamFn func(filter func(model.Sample) bool, buffer int) (<-chan cache.Event, func())

	bundle []byte

	mu       sync.Mutex
	recorded []string
}

func (f *fakeBackend) Health() Health       { return f.health }
func (f *fakeBackend) Ready() Ready         { return f.ready }
func (f *fakeBackend) Version() VersionInfo { return f.version }
func (f *fakeBackend) Metrics() string      { return f.metrics }
func (f *fakeBackend) Logs(device, level string, limit int) []obs.Entry {
	return f.logs
}

func (f *fakeBackend) Devices(q string) []DeviceView {
	if q == "" {
		return f.devices
	}
	var out []DeviceView
	for _, d := range f.devices {
		if strings.Contains(d.ID, q) || strings.Contains(d.Name, q) {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeBackend) Device(id string) (DeviceView, bool) {
	for _, d := range f.devices {
		if d.ID == id {
			return d, true
		}
	}
	return DeviceView{}, false
}

func (f *fakeBackend) Points(fl PointFilter) []model.Sample {
	var out []model.Sample
	for _, s := range f.samples {
		if fl.Device != "" && s.DeviceID != fl.Device {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (f *fakeBackend) Point(device, point string) (model.Sample, bool) {
	s, ok := f.samples[device+"/"+point]
	return s, ok
}

func (f *fakeBackend) Write(ctx context.Context, device string, items []WriteItem) ([]WriteResult, error) {
	if f.writeFn != nil {
		return f.writeFn(ctx, device, items)
	}
	out := make([]WriteResult, 0, len(items))
	for _, it := range items {
		out = append(out, WriteResult{Point: it.Point, OK: true, Quality: model.QualityGood})
	}
	return out, nil
}

func (f *fakeBackend) Config() (json.RawMessage, error) { return f.cfg, nil }
func (f *fakeBackend) ValidateConfig(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return Errf(422, "config.invalid", "bad config: %v", err)
	}
	return nil
}
func (f *fakeBackend) ApplyConfig(raw json.RawMessage, source string) (config.ApplyResult, error) {
	return f.applyRes, nil
}
func (f *fakeBackend) ConfigVersions() []config.VersionInfo { return f.versions }
func (f *fakeBackend) ConfigVersion(id string) (json.RawMessage, error) {
	if v, ok := f.configs[id]; ok {
		return v, nil
	}
	return nil, Errf(http.StatusNotFound, "config.version_not_found", "no such version %q", id)
}
func (f *fakeBackend) ConfigDiff(from, to string) (config.Diff, error) { return f.diff, nil }
func (f *fakeBackend) Rollback(version string) (config.ApplyResult, error) {
	return f.rollbackR, nil
}

func (f *fakeBackend) Command(ctx context.Context, action string, params map[string]any) (any, error) {
	if f.commandFn != nil {
		return f.commandFn(ctx, action, params)
	}
	return map[string]any{"ok": true}, nil
}

func (f *fakeBackend) Stream(filter func(model.Sample) bool, buffer int) (<-chan cache.Event, func()) {
	if f.streamFn != nil {
		return f.streamFn(filter, buffer)
	}
	ch := make(chan cache.Event, buffer)
	return ch, func() { close(ch) }
}

func (f *fakeBackend) DiagnosticsBundle(ctx context.Context) ([]byte, error) {
	return f.bundle, nil
}

func (f *fakeBackend) RecordHTTP(path string, code int, d time.Duration) {
	f.mu.Lock()
	f.recorded = append(f.recorded, path)
	f.mu.Unlock()
}

func newServer(t *testing.T, opt Options) (*Server, *fakeBackend) {
	t.Helper()
	be := &fakeBackend{
		health:  Health{Status: "ok"},
		ready:   Ready{Ready: true, Checks: []Check{{Name: "config", OK: true}}},
		version: VersionInfo{Version: "test", APIVer: 1},
		metrics: "# HELP x y\nx 1\n",
		devices: []DeviceView{{ID: "d1", Name: "device one", ConfiguredState: "enabled", RuntimeState: "collecting"}},
		samples: map[string]model.Sample{
			"d1/p1": {DeviceID: "d1", PointID: "p1", Value: model.NumValue(model.TypeFloat32, 1.5), Quality: model.QualityGood},
		},
		cfg:      json.RawMessage(`{"site":"test"}`),
		versions: []config.VersionInfo{{Version: "sha256:a", Current: true}},
		configs:  map[string]json.RawMessage{"sha256:a": json.RawMessage(`{"site":"a"}`)},
		bundle:   []byte("PK\x03\x04fake"),
	}
	s := New(opt, be, nil)
	return s, be
}

func TestAuthRequired(t *testing.T) {
	s, _ := newServer(t, Options{Token: "secret"})

	cases := []struct {
		path string
		code int
	}{
		{"/api/v1/health", 401},
		{"/healthz", 200},
		{"/readyz", 200},
		{"/api/v1/version", 200},
		{"/api/v1/metrics", 401},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.code {
			t.Fatalf("%s = %d, want %d", tc.path, rec.Code, tc.code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("authorized request = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("wrong token = %d", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "auth.unauthorized" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
}

func TestMetricsPublic(t *testing.T) {
	s, _ := newServer(t, Options{Token: "secret", MetricsPublic: true})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "uic_sse_clients") {
		t.Fatalf("missing sse metric: %s", rec.Body.String())
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	s, _ := newServer(t, Options{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/devices/ghost", nil))
	if rec.Code != 404 {
		t.Fatalf("code = %d", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "device.not_found" || env.Error.RequestID == "" {
		t.Fatalf("envelope = %+v", env)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id")
	}
}

func TestRequestIDPassthrough(t *testing.T) {
	s, _ := newServer(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("X-Request-Id", "client-123")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "client-123" {
		t.Fatalf("request id = %q", got)
	}
}

func TestDevicesAndPoints(t *testing.T) {
	s, _ := newServer(t, Options{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/devices?q=device", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var list struct {
		Items []DeviceView `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Items[0].ID != "d1" {
		t.Fatalf("list = %+v", list)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/points/d1/p1", nil))
	if rec.Code != 200 {
		t.Fatalf("point code = %d", rec.Code)
	}
	var sm model.Sample
	if err := json.Unmarshal(rec.Body.Bytes(), &sm); err != nil {
		t.Fatal(err)
	}
	if sm.DeviceID != "d1" || sm.Quality != model.QualityGood {
		t.Fatalf("sample = %+v", sm)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/points/d1/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("missing point code = %d", rec.Code)
	}
}

func TestWriteOneAndBatch(t *testing.T) {
	s, be := newServer(t, Options{})
	be.writeFn = func(ctx context.Context, device string, items []WriteItem) ([]WriteResult, error) {
		if got := IdempotencyKey(ctx); got != "" && got != "idem-1" {
			t.Errorf("idempotency key = %q", got)
		}
		out := make([]WriteResult, 0, len(items))
		for _, it := range items {
			out = append(out, WriteResult{Point: it.Point, OK: true, Quality: model.QualityGood})
		}
		return out, nil
	}

	body := `{"point":"p1","value":{"type":"float32","num":2.5}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/p1/write", strings.NewReader(body))
	req.Header.Set("X-Idempotency-Key", "idem-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		Results []WriteResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || !res.Results[0].OK {
		t.Fatalf("results = %+v", res.Results)
	}

	// path 与 body 的 point 不一致必须拒绝。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/p2/write",
		strings.NewReader(`{"point":"p1","value":{"type":"float32","num":1}}`))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("mismatch code = %d", rec.Code)
	}

	// 批量：空列表拒绝。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write", strings.NewReader(`{"writes":[]}`))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("empty batch code = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write",
		strings.NewReader(`{"writes":[{"point":"p1","value":{"type":"float32","num":1}},{"point":"p2","value":{"type":"float32","num":2}}]}`))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("batch code = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("batch results = %+v", res.Results)
	}
}

func TestBadJSONRejected(t *testing.T) {
	s, _ := newServer(t, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write", strings.NewReader(`{"writes":`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
	var env errorEnvelope
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "request.bad_json" {
		t.Fatalf("code = %q", env.Error.Code)
	}

	// 未知字段必须拒绝（严格解码），避免拼写错误被静默忽略。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write",
		strings.NewReader(`{"writes":[],"typo":true}`))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("unknown field code = %d", rec.Code)
	}
}

func TestConfigEndpoints(t *testing.T) {
	s, be := newServer(t, Options{})
	be.applyRes = config.ApplyResult{Version: "sha256:b", Applied: true}
	be.rollbackR = config.ApplyResult{Version: "sha256:a", Applied: true}
	be.diff = config.Diff{From: "sha256:a", To: "sha256:b", ChannelsAdded: []string{"ch2"}}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if rec.Code != 200 || rec.Body.String() != `{"site":"test"}` {
		t.Fatalf("config get = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/config/validate", strings.NewReader(`{"site":"x"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("validate = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/config/apply", strings.NewReader(`{"site":"x"}`)))
	if rec.Code != 200 {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body.String())
	}
	var applied config.ApplyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Restarted == nil {
		t.Fatalf("apply result = %+v (restartedDevices must be [] not null)", applied)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config/versions", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "sha256:a") {
		t.Fatalf("versions = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config/versions/sha256:a", nil))
	if rec.Code != 200 || rec.Body.String() != `{"site":"a"}` {
		t.Fatalf("version = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config/diff?from=sha256:a&to=sha256:b", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ch2") {
		t.Fatalf("diff = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback", strings.NewReader(`{"version":"sha256:a"}`)))
	if rec.Code != 200 {
		t.Fatalf("rollback = %d %s", rec.Code, rec.Body.String())
	}

	// 缺少 version 必须 400。
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback", strings.NewReader(`{}`)))
	if rec.Code != 400 {
		t.Fatalf("rollback without version = %d", rec.Code)
	}
}

func TestCommand(t *testing.T) {
	s, be := newServer(t, Options{})
	be.commandFn = func(ctx context.Context, action string, params map[string]any) (any, error) {
		if action == "boom" {
			return nil, Errf(409, "outbox.disabled", "no sink")
		}
		if v, ok := params["_confirm"].(bool); !ok || !v {
			t.Errorf("confirm not propagated: %+v", params)
		}
		return map[string]any{"device": params["device"]}, nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands",
		strings.NewReader(`{"action":"driver.restart","params":{"device":"d1"},"confirm":true}`))
	s.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("command = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/commands", strings.NewReader(`{"action":"boom"}`)))
	if rec.Code != 409 {
		t.Fatalf("boom = %d", rec.Code)
	}
	var env errorEnvelope
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "outbox.disabled" {
		t.Fatalf("code = %q", env.Error.Code)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/commands", strings.NewReader(`{"params":{}}`)))
	if rec.Code != 400 {
		t.Fatalf("missing action = %d", rec.Code)
	}
}

func TestSSEHelloAndSample(t *testing.T) {
	s, be := newServer(t, Options{Heartbeat: time.Hour})
	be.streamFn = func(filter func(model.Sample) bool, buffer int) (<-chan cache.Event, func()) {
		ch := make(chan cache.Event, 4)
		sm := model.Sample{
			DeviceID: "d1", PointID: "p1", Value: model.NumValue(model.TypeFloat32, 2),
			Quality: model.QualityGood, Seq: 9,
		}
		if filter(sm) {
			ch <- cache.Event{Type: cache.EventSample, Sample: sm}
		}
		ch <- cache.Event{Type: cache.EventQuality, Sample: sm, From: model.QualityGood, To: model.QualityTimeout}
		return ch, func() {}
	}

	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/stream?points=d1/p1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}

	r := bufio.NewReader(resp.Body)
	var events []string
	var data []string
	for i := 0; i < 40; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			events = append(events, strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
		if len(events) >= 3 {
			break
		}
	}
	cancel()
	if len(events) < 3 {
		t.Fatalf("events = %v data = %v", events, data)
	}
	if events[0] != "hello" || events[1] != "sample" || events[2] != "quality" {
		t.Fatalf("event order = %v", events)
	}
	var hello map[string]any
	if err := json.Unmarshal([]byte(data[0]), &hello); err != nil {
		t.Fatal(err)
	}
	if pts, ok := hello["points"].([]any); !ok || len(pts) != 1 || pts[0] != "d1/p1" {
		t.Fatalf("hello = %+v", hello)
	}
}

func TestSSEBadPattern(t *testing.T) {
	s, _ := newServer(t, Options{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/stream?points=badpattern", nil))
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestDiagnosticsBundleHeaders(t *testing.T) {
	s, _ := newServer(t, Options{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/bundle", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Fatalf("content disposition = %q", cd)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty bundle")
	}
}

func TestCORSAndPreflight(t *testing.T) {
	s, _ := newServer(t, Options{AllowedOrigin: "https://ui.example"})
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/devices", nil)
	req.Header.Set("Origin", "https://ui.example")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("preflight = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://ui.example" {
		t.Fatal("missing CORS header")
	}
}

func TestBasePath(t *testing.T) {
	s, _ := newServer(t, Options{BasePath: "/uic"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/uic/api/v1/health", nil))
	if rec.Code != 200 {
		t.Fatalf("base path = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != 404 {
		t.Fatalf("outside base path = %d", rec.Code)
	}
}

func TestMetricPathNormalization(t *testing.T) {
	cases := map[string]string{
		"/api/v1/health":                     "/api/v1/health",
		"/api/v1/devices/d1":                 "/api/v1/devices/:device",
		"/api/v1/devices/d1/points":          "/api/v1/devices/:device/points",
		"/api/v1/points/d1/p1":               "/api/v1/points/:device/:point",
		"/api/v1/points/d1/p1/write":         "/api/v1/points/:device/:point/write",
		"/api/v1/points/d1/write":            "/api/v1/points/:device/write",
		"/api/v1/config/versions/sha256:abc": "/api/v1/config/versions/:id",
		"/api/v1/stream":                     "/api/v1/stream",
	}
	for in, want := range cases {
		if got := metricPath(in); got != want {
			t.Fatalf("metricPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordsMetricPath(t *testing.T) {
	s, be := newServer(t, Options{})
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/devices/d1", nil))
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/devices/d2", nil))
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.recorded) != 2 {
		t.Fatalf("recorded = %v", be.recorded)
	}
	if be.recorded[0] != "/api/v1/devices/:device" {
		t.Fatalf("recorded path = %q", be.recorded[0])
	}
}

func TestFailMapsContextErrors(t *testing.T) {
	s, be := newServer(t, Options{})
	be.commandFn = func(context.Context, string, map[string]any) (any, error) {
		return nil, context.DeadlineExceeded
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/commands", strings.NewReader(`{"action":"x"}`)))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d", rec.Code)
	}

	be.commandFn = func(context.Context, string, map[string]any) (any, error) {
		return nil, errors.New("unexpected")
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/commands", strings.NewReader(`{"action":"x"}`)))
	if rec.Code != 500 {
		t.Fatalf("internal code = %d", rec.Code)
	}
	var env errorEnvelope
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "internal.error" || strings.Contains(env.Error.Message, "unexpected") {
		t.Fatalf("internal errors must not leak details: %+v", env.Error)
	}
}

func TestReadyNotReady(t *testing.T) {
	s, be := newServer(t, Options{})
	be.ready = Ready{Ready: false, Checks: []Check{{Name: "storage", OK: false, Detail: "ro"}}}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestBodyTooLarge(t *testing.T) {
	s, _ := newServer(t, Options{MaxBodyBytes: 16})
	big := `{"writes":[{"point":"p1","value":{"type":"float32","num":1}}]}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write", strings.NewReader(big)))
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestSSEPatternParsing(t *testing.T) {
	p, err := parsePatterns("")
	if err != nil || !p.all {
		t.Fatalf("empty = %+v %v", p, err)
	}
	p, err = parsePatterns("d1/p1, d2/p2")
	if err != nil {
		t.Fatal(err)
	}
	if !p.match("d1", "p1") || !p.match("d2", "p2") || p.match("d1", "p2") {
		t.Fatalf("match rules wrong: %+v", p)
	}
	if got := p.String(); len(got) != 2 || got[0] != "d1/p1" {
		t.Fatalf("string = %v", got)
	}
	for _, bad := range []string{"nope", "/p1", "d1/", "d*/p1", "d1/p*"} {
		if _, err := parsePatterns(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestWriteResultsShape(t *testing.T) {
	s, be := newServer(t, Options{})
	be.writeFn = func(ctx context.Context, device string, items []WriteItem) ([]WriteResult, error) {
		return []WriteResult{{Point: "p1", OK: false, Code: "point.not_found", Message: "nope"}}, nil
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write",
		strings.NewReader(`{"writes":[{"point":"p1","value":{"type":"float32","num":1}}]}`)))
	if rec.Code != 200 {
		t.Fatalf("partial failure must stay 200, got %d", rec.Code)
	}
	var got struct {
		Results []WriteResult `json:"results"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Results) != 1 || got.Results[0].Code != "point.not_found" {
		t.Fatalf("results = %+v", got.Results)
	}
}
