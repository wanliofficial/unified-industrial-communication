package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/alarm"
	"uic/internal/audit"
	"uic/internal/cache"
	"uic/internal/config"
	"uic/internal/history"
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

	auditLog []audit.Record
	auditOff bool

	alarms      []alarm.Status
	alarmCounts alarm.Counts
	ackFn       func(ctx context.Context, ids []string, all bool, by string) ([]alarm.Status, error)
	alarmCh     chan alarm.Event

	historyFn    func(q history.Query) (history.Result, error)
	historyStats history.Stats
	historyOn    bool

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
func (f *fakeBackend) ApplyConfig(_ context.Context, raw json.RawMessage, source string) (config.ApplyResult, error) {
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
func (f *fakeBackend) Rollback(_ context.Context, version string) (config.ApplyResult, error) {
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

func (f *fakeBackend) Alarms(fl alarm.Filter) []alarm.Status {
	out := make([]alarm.Status, 0, len(f.alarms))
	for _, st := range f.alarms {
		if len(fl.States) > 0 && !slicesContainsState(fl.States, st.State) {
			continue
		}
		if len(fl.Severities) > 0 {
			ok := false
			for _, want := range fl.Severities {
				if st.Severity == want {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, st)
	}
	return out
}

func (f *fakeBackend) Alarm(id string) (alarm.Status, bool) {
	for _, st := range f.alarms {
		if st.ID == id {
			return st, true
		}
	}
	return alarm.Status{}, false
}

func (f *fakeBackend) AlarmCounts() alarm.Counts { return f.alarmCounts }

func (f *fakeBackend) AckAlarms(ctx context.Context, ids []string, all bool, by string) ([]alarm.Status, error) {
	if f.ackFn != nil {
		return f.ackFn(ctx, ids, all, by)
	}
	var out []alarm.Status
	for _, st := range f.alarms {
		if all || slicesContains(ids, st.ID) {
			st.State = alarm.StateAcked
			st.Acknowledged = true
			st.AckBy = by
			out = append(out, st)
		}
	}
	return out, nil
}

func slicesContainsState(hay []alarm.State, needle alarm.State) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func (f *fakeBackend) AlarmStream(buffer int) (<-chan alarm.Event, func()) {
	if f.alarmCh == nil {
		f.alarmCh = make(chan alarm.Event, buffer)
	}
	return f.alarmCh, func() {}
}

func (f *fakeBackend) History(q history.Query) (history.Result, error) {
	if f.historyFn != nil {
		return f.historyFn(q)
	}
	return history.Result{Device: q.Device, Point: q.Point, From: q.From, To: q.To,
		Step: model.Duration(q.Step), Agg: q.Agg}, nil
}

func (f *fakeBackend) HistoryStats() (history.Stats, bool) {
	return f.historyStats, f.historyOn
}

func (f *fakeBackend) DiagnosticsBundle(ctx context.Context) ([]byte, error) {
	return f.bundle, nil
}

func (f *fakeBackend) AuditQuery(fl audit.Filter) (audit.Result, error) {
	if f.auditOff {
		return audit.Result{}, Errf(http.StatusConflict, "audit.disabled", "audit storage is not enabled")
	}
	items := make([]audit.Record, 0, len(f.auditLog))
	for i := len(f.auditLog) - 1; i >= 0; i-- {
		items = append(items, f.auditLog[i])
	}
	if fl.Limit > 0 && len(items) > fl.Limit {
		items = items[:fl.Limit]
	}
	return audit.Result{Items: items, Total: len(items), Scanned: len(f.auditLog)}, nil
}

func (f *fakeBackend) AuditVerify() (audit.VerifyResult, error) {
	if f.auditOff {
		return audit.VerifyResult{}, Errf(http.StatusConflict, "audit.disabled", "audit storage is not enabled")
	}
	return audit.VerifyResult{Checked: len(f.auditLog), OK: true}, nil
}

func (f *fakeBackend) AuditStats() (audit.Stats, bool) {
	if f.auditOff {
		return audit.Stats{}, false
	}
	return audit.Stats{Records: uint64(len(f.auditLog)), LastSeq: uint64(len(f.auditLog))}, true
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
		// 报警单条与确认带动态 id，且查不到会 404：必须归一化，否则任意 id
		// 都会成为新的指标标签、撑爆基数。
		"/api/v1/alarms/rule-high-temp":    "/api/v1/alarms/:id",
		"/api/v1/alarms/rule-low-tank/ack": "/api/v1/alarms/:id/ack",
		// 不存在（404）的设备/点位子路径同样不能把原始 id 带进标签。
		"/api/v1/devices/d1/whatever":     "/api/v1/devices/:device",
		"/api/v1/devices/d1/points/extra": "/api/v1/devices/:device/points",
		"/api/v1/points/d1/p1/extra":      "/api/v1/points/:device/:point",
	}
	for in, want := range cases {
		if got := metricPath(in); got != want {
			t.Fatalf("metricPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMetricPathBoundedCardinality 是上面那条的性质化版本：同一类前缀下无论
// 出现多少个不同的路径参数，归一化后的标签集合都必须有限。这正是 metricPath
// 存在的理由——Prometheus 风格注册表从不回收标签组合。
func TestMetricPathBoundedCardinality(t *testing.T) {
	ids := []string{"AAAA", "BBBB", "rule-x", "0", "../../../etc", "带空格"}
	templates := []string{
		"/api/v1/alarms/%s",
		"/api/v1/alarms/%s/ack",
		"/api/v1/devices/%s",
		"/api/v1/devices/%s/points",
		"/api/v1/devices/%s/points/extra",
		"/api/v1/devices/%s/whatever",
		"/api/v1/points/%s/p1",
		"/api/v1/points/d1/%s",
		"/api/v1/points/%s/p1/extra",
	}
	seen := map[string]bool{}
	for _, tpl := range templates {
		for _, id := range ids {
			got := metricPath(fmt.Sprintf(tpl, id))
			if strings.Contains(got, id) {
				t.Fatalf("metricPath(%q) = %q 仍含原始路径参数 %q", tpl, got, id)
			}
			seen[got] = true
		}
	}
	// 9 个模板 × 6 个 id，但归一化后远少于 54 个标签，且全部不含原始 id。
	if len(seen) == 0 {
		t.Fatal("no labels produced")
	}
	t.Logf("归一化后得到 %d 个有限标签", len(seen))
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

// ---------------------------------------------------------------------------
// 报警
// ---------------------------------------------------------------------------

func alarmServer(t *testing.T) (*Server, *fakeBackend) {
	t.Helper()
	s, be := newServer(t, Options{})
	be.health = Health{Status: "ok", Alarms: alarm.Counts{Total: 3, Active: 1, Cleared: 1, Unknown: 1}}
	be.alarmCounts = be.health.Alarms
	be.alarms = []alarm.Status{
		{ID: "high-temp", Name: "高温", Severity: alarm.SeverityCritical, State: alarm.StateActive,
			Active: true, Count: 2, Message: "temperature above limit",
			UpdatedAt: time.Unix(1000, 0).UTC()},
		{ID: "low-level", Name: "低液位", Severity: alarm.SeverityWarning, State: alarm.StateCleared,
			Count: 1, UpdatedAt: time.Unix(1001, 0).UTC()},
		{ID: "ratio", Severity: alarm.SeverityInfo, State: alarm.StateNormal,
			UpdatedAt: time.Unix(1002, 0).UTC()},
	}
	return s, be
}

func TestAlarmsList(t *testing.T) {
	s, be := alarmServer(t)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/alarms", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var env struct {
		Items  []alarm.Status `json:"items"`
		Total  int            `json:"total"`
		Counts alarm.Counts   `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Total != 3 || len(env.Items) != 3 {
		t.Fatalf("total = %d items = %d", env.Total, len(env.Items))
	}
	if env.Counts.Active != 1 || env.Counts.Total != 3 {
		t.Fatalf("counts = %+v", env.Counts)
	}
	if env.Items[0].ID != "high-temp" || env.Items[0].State != alarm.StateActive {
		t.Fatalf("first item = %+v", env.Items[0])
	}

	// 过滤条件透传给 backend。
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/alarms?state=active,cleared&severity=warning", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("filtered body = %s", rec.Body)
	}

	// 未知取值必须报错：静默忽略会返回「全部规则」，看起来像过滤生效了。
	for _, q := range []string{"state=bogus", "severity=fatal"} {
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/alarms?"+q, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "request.invalid") {
			t.Fatalf("%s: code = %d body = %s", q, rec.Code, rec.Body)
		}
	}
	_ = be
}

func TestAlarmGet(t *testing.T) {
	s, _ := alarmServer(t)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/alarms/low-level", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var st alarm.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.ID != "low-level" || st.State != alarm.StateCleared {
		t.Fatalf("status = %+v", st)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/alarms/nope", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "alarm.not_found") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}

func TestAlarmAckSingle(t *testing.T) {
	s, be := alarmServer(t)
	var gotBy string
	be.ackFn = func(_ context.Context, ids []string, all bool, by string) ([]alarm.Status, error) {
		gotBy = by
		if all || len(ids) != 1 || ids[0] != "high-temp" {
			t.Fatalf("ids = %v all = %v", ids, all)
		}
		return []alarm.Status{{ID: "high-temp", State: alarm.StateAcked, Acknowledged: true, AckBy: by}}, nil
	}

	// 空 body：允许（只靠路径 ID）。
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alarms/high-temp/ack", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	if gotBy != "" {
		t.Fatalf("by = %q", gotBy)
	}
	if !strings.Contains(rec.Body.String(), `"acknowledged"`) || !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("body = %s", rec.Body)
	}

	// 带操作人。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alarms/high-temp/ack", strings.NewReader(`{"by":"alice"}`))
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotBy != "alice" {
		t.Fatalf("code = %d by = %q body = %s", rec.Code, gotBy, rec.Body)
	}

	// 单条确认不允许再用 body 指定 ids/all。
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alarms/high-temp/ack", strings.NewReader(`{"all":true}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}

	// by 过长。
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alarms/high-temp/ack",
		strings.NewReader(`{"by":"`+strings.Repeat("x", 65)+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}

func TestAlarmAckErrorsPassThrough(t *testing.T) {
	s, be := alarmServer(t)
	be.ackFn = func(context.Context, []string, bool, string) ([]alarm.Status, error) {
		return nil, Errf(http.StatusConflict, "alarm.not_active", "alarm %q is normal", "high-temp")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alarms/high-temp/ack", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "alarm.not_active") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}

func TestAlarmsAckBatch(t *testing.T) {
	s, be := alarmServer(t)
	var gotIDs []string
	var gotAll bool
	var gotBy string
	be.ackFn = func(_ context.Context, ids []string, all bool, by string) ([]alarm.Status, error) {
		gotIDs, gotAll, gotBy = ids, all, by
		return []alarm.Status{{ID: "high-temp", State: alarm.StateAcked, Acknowledged: true}}, nil
	}

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alarms/ack", strings.NewReader(body)))
		return rec
	}

	if rec := post(`{"ids":["high-temp","low-level"],"by":"bob"}`); rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	if len(gotIDs) != 2 || gotAll || gotBy != "bob" {
		t.Fatalf("ids = %v all = %v by = %q", gotIDs, gotAll, gotBy)
	}

	if rec := post(`{"all":true}`); rec.Code != http.StatusOK || !gotAll {
		t.Fatalf("all: code = %d body = %s", rec.Code, rec.Body)
	}

	// 参数互斥与缺省。
	for _, body := range []string{`{}`, `{"all":true,"ids":["x"]}`, `{"ids":[]}`} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d body = %s", body, rec.Code, rec.Body)
		}
	}
}

func TestAlarmStream(t *testing.T) {
	s, be := alarmServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/alarms/stream?heartbeat=1s", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	be.alarmCh <- alarm.Event{
		Type: alarm.EventRaised, At: time.Unix(2000, 0).UTC(), From: alarm.StateNormal,
		Status: alarm.Status{ID: "high-temp", Severity: alarm.SeverityCritical, State: alarm.StateActive, Active: true},
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
	if !strings.Contains(hello, "event: hello") || !strings.Contains(hello, `"active":1`) {
		t.Fatalf("hello = %s", hello)
	}
	joined := readEvent()
	if !strings.Contains(joined, "event: raised") {
		t.Fatalf("missing event name:\n%s", joined)
	}
	if !strings.Contains(joined, `"id":"high-temp"`) {
		t.Fatalf("missing status payload:\n%s", joined)
	}
}

func TestHealthIncludesAlarmCounts(t *testing.T) {
	s, _ := alarmServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var h struct {
		Alarms alarm.Counts `json:"alarms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Alarms.Total != 3 || h.Alarms.Active != 1 || h.Alarms.Cleared != 1 {
		t.Fatalf("alarms = %+v", h.Alarms)
	}
}

func TestHistoryQueryParsing(t *testing.T) {
	s, be := newServer(t, Options{Now: func() time.Time {
		return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	}})
	var got history.Query
	be.historyFn = func(q history.Query) (history.Result, error) {
		got = q
		return history.Result{Device: q.Device, Point: q.Point, From: q.From, To: q.To,
			Step: model.Duration(q.Step), Agg: q.Agg, Count: 1,
			Points: []history.Point{{Time: q.From, Value: model.NumValue(model.TypeFloat64, 1.5),
				Quality: model.QualityGood}}}, nil
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/history?device=press&point=power&from=-2h&step=5m&agg=avg&limit=100", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if got.Device != "press" || got.Point != "power" {
		t.Fatalf("query = %+v", got)
	}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if !got.From.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("from = %s", got.From)
	}
	// to 缺省时必须由 runtime/store 补成「现在」，而不是零值。
	if !got.To.IsZero() {
		t.Fatalf("to = %s, want zero so the store applies its default", got.To)
	}
	if got.Step != 5*time.Minute || got.Agg != history.AggAvg || got.Limit != 100 {
		t.Fatalf("step/agg/limit = %s/%s/%d", got.Step, got.Agg, got.Limit)
	}
	if !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// 绝对时刻也要接受。
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/history?device=press&point=power&to=2026-03-01T12:00:00Z", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !got.To.Equal(now) {
		t.Fatalf("to = %s", got.To)
	}
}

func TestHistoryQueryErrors(t *testing.T) {
	s, _ := newServer(t, Options{})
	cases := []struct {
		name string
		url  string
		code int
		want string
	}{
		{"no device", "/api/v1/history?point=power", 400, "device and point are required"},
		{"no point", "/api/v1/history?device=press", 400, "device and point are required"},
		{"bad from", "/api/v1/history?device=d&point=p&from=yesterday", 400, "history.invalid_range"},
		{"bad to", "/api/v1/history?device=d&point=p&to=12:00", 400, "history.invalid_range"},
		{"bad step", "/api/v1/history?device=d&point=p&step=five", 400, "history.invalid_query"},
		{"zero step", "/api/v1/history?device=d&point=p&step=0s", 400, "step must be"},
		{"bad agg", "/api/v1/history?device=d&point=p&step=1m&agg=p95", 400, "history.invalid_query"},
		{"bad limit", "/api/v1/history?device=d&point=p&limit=abc", 400, "limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
			if rec.Code != c.code {
				t.Fatalf("code = %d, want %d (body %s)", rec.Code, c.code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("body = %s, want mention of %q", rec.Body.String(), c.want)
			}
		})
	}
}

func TestHistoryDisabled(t *testing.T) {
	s, be := newServer(t, Options{})
	be.historyFn = func(q history.Query) (history.Result, error) {
		return history.Result{}, Errf(http.StatusConflict, "history.disabled",
			"history storage is not enabled in this configuration")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/history?device=d&point=p", nil))
	if rec.Code != 409 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "history.disabled") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHealthSerializesHistory(t *testing.T) {
	s, be := newServer(t, Options{})
	be.health = Health{Status: "ok", History: HistoryHealth{
		Enabled: true, Records: 42, Bytes: 4096, MaxBytes: 1 << 20, Segments: 2,
		Evicted: 3, Errors: 1,
	}}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var body struct {
		History HistoryHealth `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.History.Enabled || body.History.Records != 42 || body.History.Segments != 2 {
		t.Fatalf("history = %+v", body.History)
	}
	// 未启用时也要有 history 字段（enabled=false），否则客户端要写两套分支。
	be.health = Health{Status: "ok"}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if !strings.Contains(rec.Body.String(), `"history":{"enabled":false}`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
