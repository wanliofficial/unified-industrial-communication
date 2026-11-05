// Package httpapi 实现北向 REST + SSE 接口（docs/03-product/05-northbound-api-spec.md）。
//
// 该包只做「协议适配」：认证、路由、JSON 编解码、错误封套、SSE 分帧。
// 业务语义全部通过 Backend 接口下沉到 runtime，因此可以用 httptest 直接
// 测接口契约，不需要启动真实采集。
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"uic/internal/alarm"
	"uic/internal/cache"
	"uic/internal/config"
	"uic/internal/history"
	"uic/internal/model"
	"uic/internal/obs"
)

// Options 是 HTTP 层参数。
type Options struct {
	Token         string        // Bearer Token；为空表示关闭认证（仅限开发）
	BasePath      string        // 反向代理前缀，例如 /uic
	MetricsPublic bool          // /api/v1/metrics 是否免认证
	AllowedOrigin string        // 单值 CORS 白名单；为空表示不发送 CORS 头
	MaxBodyBytes  int64         // 默认 4MB
	Heartbeat     time.Duration // SSE 默认心跳
	Now           func() time.Time
}

func (o Options) withDefaults() Options {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 4 << 20
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.BasePath = strings.TrimSuffix(o.BasePath, "/")
	return o
}

// ---------------------------------------------------------------------------
// 对外视图
// ---------------------------------------------------------------------------

// DeviceStats 是设备运行统计（字段与调度器一致）。
type DeviceStats struct {
	ReadsTotal          uint64  `json:"readsTotal"`
	ReadsFailed         uint64  `json:"readsFailed"`
	Skipped             uint64  `json:"skipped"`
	Probes              uint64  `json:"probes"`
	SuccessRate         float64 `json:"successRate"`
	AvgDurationMs       float64 `json:"avgDurationMs"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
	LastError           string  `json:"lastError,omitempty"`
	LastErrorKind       string  `json:"lastErrorKind,omitempty"`
	LastSuccess         string  `json:"lastSuccess,omitempty"`
}

// PointCounts 是点位质量分布。
type PointCounts struct {
	Total     int `json:"total"`
	Good      int `json:"good"`
	Uncertain int `json:"uncertain"`
	Stale     int `json:"stale"`
	Bad       int `json:"bad"`
	Disabled  int `json:"disabled"`
}

// DeviceView 是设备详情：配置态与运行态必须同时返回。
type DeviceView struct {
	ID              string      `json:"id"`
	Name            string      `json:"name,omitempty"`
	Channel         string      `json:"channel"`
	Protocol        string      `json:"protocol"`
	ConfiguredState string      `json:"configuredState"`
	RuntimeState    string      `json:"runtimeState"`
	Since           time.Time   `json:"since"`
	Generation      uint64      `json:"generation"`
	Interval        string      `json:"interval"`
	NextDue         string      `json:"nextDue,omitempty"`
	Stats           DeviceStats `json:"stats"`
	Points          PointCounts `json:"points"`
}

// Problem 是健康聚合中的一条异常。
type Problem struct {
	Device string `json:"device"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Since  string `json:"since,omitempty"`
}

// DeviceCounts 是设备状态汇总。
type DeviceCounts struct {
	Total      int `json:"total"`
	Collecting int `json:"collecting"`
	Degraded   int `json:"degraded"`
	Faulted    int `json:"faulted"`
	Disabled   int `json:"disabled"`
}

// OutboxHealth 是队列水位。
type OutboxHealth struct {
	Pending   int64            `json:"pending"`
	Bytes     int64            `json:"pendingBytes"`
	Dropped   map[string]int64 `json:"dropped,omitempty"`
	Delivered uint64           `json:"delivered"`
}

// HistoryHealth 是历史库水位。历史不可用时 enabled=false，其余字段留空。
type HistoryHealth struct {
	Enabled  bool   `json:"enabled"`
	Records  uint64 `json:"records,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	MaxBytes int64  `json:"maxBytes,omitempty"`
	Segments int    `json:"segments,omitempty"`
	// 指针 + omitempty：time.Time 是结构体，零值不会被 omitempty 省掉，
	// 结果就是每个响应里都挂着一个 "0001-01-01T00:00:00Z"。
	Oldest  *time.Time `json:"oldest,omitempty"`
	Newest  *time.Time `json:"newest,omitempty"`
	Evicted uint64     `json:"evicted,omitempty"`
	Errors  uint64     `json:"writeErrors,omitempty"`
}

// Health 是业务健康聚合。
type Health struct {
	Status        string       `json:"status"`
	UptimeSeconds int64        `json:"uptimeSeconds"`
	Devices       DeviceCounts `json:"devices"`
	Points        PointCounts  `json:"points"`
	Outbox        OutboxHealth `json:"outbox"`
	// Alarms 是报警状态汇总。放在 health 里而不是要求调用方额外拉一次
	// /alarms：看板判断「这台网关是否健康」时，未确认报警必须算进去。
	Alarms        alarm.Counts  `json:"alarms"`
	History       HistoryHealth `json:"history"`
	Problems      []Problem     `json:"problems,omitempty"`
	Version       string        `json:"version,omitempty"`
	ConfigVersion string        `json:"configVersion,omitempty"`
}

// Check 是就绪检查的一项。
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Ready 是依赖就绪状态。
type Ready struct {
	Ready  bool    `json:"ready"`
	Checks []Check `json:"checks"`
}

// VersionInfo 是构建信息。
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"buildTime,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
	Schema    int    `json:"schemaVersion"`
	APIVer    int    `json:"apiVersion"`
}

// PointFilter 是点位查询条件。
type PointFilter struct {
	Device  string
	Q       string
	Quality []string
	Limit   int
}

// WriteItem 是一次写入请求。
type WriteItem struct {
	Point   string      `json:"point"`
	Value   model.Value `json:"value"`
	Confirm bool        `json:"confirm,omitempty"`
}

// WriteResult 是单点写入结果。
type WriteResult struct {
	Point     string        `json:"point"`
	OK        bool          `json:"ok"`
	Quality   model.Quality `json:"quality,omitempty"`
	AppliedAt *time.Time    `json:"appliedAt,omitempty"`
	Code      string        `json:"code,omitempty"`
	Message   string        `json:"message,omitempty"`
}

// APIError 是带 HTTP 状态与错误码的领域错误。Backend 用它表达可预期的失败。
type APIError struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Errf 构造一个 APIError。
func Errf(status int, code, format string, args ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Backend 是 runtime 需要实现的全部业务能力。
type Backend interface {
	Health() Health
	Ready() Ready
	Version() VersionInfo
	Metrics() string
	Logs(device, level string, limit int) []obs.Entry

	Devices(q string) []DeviceView
	Device(id string) (DeviceView, bool)
	Points(f PointFilter) []model.Sample
	Point(device, point string) (model.Sample, bool)
	Write(ctx context.Context, device string, items []WriteItem) ([]WriteResult, error)

	Config() (json.RawMessage, error)
	ValidateConfig(raw json.RawMessage) error
	ApplyConfig(raw json.RawMessage, source string) (config.ApplyResult, error)
	ConfigVersions() []config.VersionInfo
	ConfigVersion(id string) (json.RawMessage, error)
	ConfigDiff(from, to string) (config.Diff, error)
	Rollback(version string) (config.ApplyResult, error)

	Command(ctx context.Context, action string, params map[string]any) (any, error)

	Stream(filter func(model.Sample) bool, buffer int) (<-chan cache.Event, func())

	Alarms(f alarm.Filter) []alarm.Status
	Alarm(id string) (alarm.Status, bool)
	AlarmCounts() alarm.Counts
	AckAlarms(ctx context.Context, ids []string, all bool, by string) ([]alarm.Status, error)
	AlarmStream(buffer int) (<-chan alarm.Event, func())

	History(q history.Query) (history.Result, error)
	HistoryStats() (history.Stats, bool)

	DiagnosticsBundle(ctx context.Context) ([]byte, error)

	RecordHTTP(path string, code int, d time.Duration)
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// Server 是 HTTP handler。
type Server struct {
	opt Options
	be  Backend
	log *slog.Logger

	mux     *http.ServeMux
	reqSeq  atomic.Uint64
	sseLive atomic.Int64
}

// New 构造 HTTP 层并注册全部路由。
func New(opt Options, be Backend, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{opt: opt.withDefaults(), be: be, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

// SSEClients 返回当前 SSE 连接数（用于健康与测试）。
func (s *Server) SSEClients() int64 { return s.sseLive.Load() }

func (s *Server) routes() {
	h := s.handle
	s.mux.HandleFunc("GET /healthz", h(s.healthz))
	s.mux.HandleFunc("GET /readyz", h(s.readyz))
	s.mux.HandleFunc("GET /api/v1/health", h(s.health))
	s.mux.HandleFunc("GET /api/v1/version", h(s.version))
	s.mux.HandleFunc("GET /api/v1/metrics", h(s.metrics))

	s.mux.HandleFunc("GET /api/v1/devices", h(s.devices))
	s.mux.HandleFunc("GET /api/v1/devices/{device}", h(s.device))
	s.mux.HandleFunc("GET /api/v1/devices/{device}/points", h(s.devicePoints))

	s.mux.HandleFunc("GET /api/v1/points", h(s.points))
	s.mux.HandleFunc("GET /api/v1/points/{device}/{point}", h(s.point))
	s.mux.HandleFunc("POST /api/v1/points/{device}/{point}/write", h(s.writeOne))
	s.mux.HandleFunc("POST /api/v1/points/{device}/write", h(s.writeBatch))

	s.mux.HandleFunc("GET /api/v1/config", h(s.configGet))
	s.mux.HandleFunc("POST /api/v1/config/validate", h(s.configValidate))
	s.mux.HandleFunc("POST /api/v1/config/apply", h(s.configApply))
	s.mux.HandleFunc("GET /api/v1/config/versions", h(s.configVersions))
	s.mux.HandleFunc("GET /api/v1/config/versions/{id}", h(s.configVersion))
	s.mux.HandleFunc("GET /api/v1/config/diff", h(s.configDiff))
	s.mux.HandleFunc("POST /api/v1/config/rollback", h(s.configRollback))

	s.mux.HandleFunc("POST /api/v1/commands", h(s.command))
	s.mux.HandleFunc("GET /api/v1/logs", h(s.logs))
	s.mux.HandleFunc("GET /api/v1/stream", h(s.stream))

	s.mux.HandleFunc("GET /api/v1/alarms", h(s.alarmsList))
	s.mux.HandleFunc("POST /api/v1/alarms/ack", h(s.alarmsAck))
	s.mux.HandleFunc("GET /api/v1/alarms/stream", h(s.alarmsStream))
	s.mux.HandleFunc("GET /api/v1/alarms/{id}", h(s.alarmGet))
	s.mux.HandleFunc("POST /api/v1/alarms/{id}/ack", h(s.alarmAck))
	s.mux.HandleFunc("GET /api/v1/history", h(s.history))
	s.mux.HandleFunc("GET /api/v1/diagnostics/bundle", h(s.bundle))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.opt.Now()
	path := r.URL.Path
	if s.opt.BasePath != "" {
		if path == s.opt.BasePath {
			path = "/"
		} else if strings.HasPrefix(path, s.opt.BasePath+"/") {
			path = strings.TrimPrefix(path, s.opt.BasePath)
		} else {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = path
		r = r2
	}

	code := 0
	ww := &statusWriter{ResponseWriter: w, code: &code}
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = fmt.Sprintf("req-%d-%d", start.UnixNano(), s.reqSeq.Add(1))
	}
	ww.Header().Set("X-Request-Id", reqID)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, reqID))

	if s.opt.AllowedOrigin != "" {
		ww.Header().Set("Access-Control-Allow-Origin", s.opt.AllowedOrigin)
		ww.Header().Set("Vary", "Origin")
		ww.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, X-Idempotency-Key")
		ww.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			ww.WriteHeader(http.StatusNoContent)
			return
		}
	}

	if !s.authorized(r) {
		ww.Header().Set("WWW-Authenticate", `Bearer realm="uic"`)
		s.fail(ww, r, Errf(http.StatusUnauthorized, "auth.unauthorized", "missing or invalid bearer token"))
		s.record(r, ww.status(), start)
		return
	}

	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("http_panic", "path", r.URL.Path, "panic", rec)
			if code == 0 {
				s.fail(ww, r, Errf(http.StatusInternalServerError, "internal.panic", "internal error"))
			}
		}
		s.record(r, ww.status(), start)
	}()

	s.mux.ServeHTTP(ww, r)
}

func (s *Server) record(r *http.Request, code int, start time.Time) {
	if s.be != nil {
		s.be.RecordHTTP(metricPath(r.URL.Path), code, s.opt.Now().Sub(start))
	}
}

// metricPath 把路径参数替换为占位符，避免设备/点位 ID 造成指标基数爆炸。
// 这是指标标签的第一条纪律：标签值必须来自有限集合。
func metricPath(p string) string {
	const dev = "/api/v1/devices/"
	const pts = "/api/v1/points/"
	const ver = "/api/v1/config/versions/"
	switch {
	case strings.HasPrefix(p, dev):
		seg := strings.Split(strings.TrimPrefix(p, dev), "/")
		if len(seg) == 1 {
			return dev + ":device"
		}
		if len(seg) == 2 && seg[1] == "points" {
			return dev + ":device/points"
		}
	case strings.HasPrefix(p, pts):
		seg := strings.Split(strings.TrimPrefix(p, pts), "/")
		if len(seg) == 2 {
			if seg[1] == "write" {
				return pts + ":device/write"
			}
			return pts + ":device/:point"
		}
		if len(seg) == 3 && seg[2] == "write" {
			return pts + ":device/:point/write"
		}
	case strings.HasPrefix(p, ver):
		return ver + ":id"
	}
	return p
}

func (s *Server) authorized(r *http.Request) bool {
	if s.opt.Token == "" {
		return true
	}
	switch r.URL.Path {
	case "/healthz", "/readyz", "/api/v1/version":
		return true
	case "/api/v1/metrics":
		if s.opt.MetricsPublic {
			return true
		}
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(auth[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.Token)) == 1
}

type ctxKeyRequestID struct{}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyRequestID{}).(string); ok {
		return v
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	code *int
}

func (w *statusWriter) WriteHeader(code int) {
	if *w.code == 0 {
		*w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if *w.code == 0 {
		*w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) status() int {
	if *w.code == 0 {
		return http.StatusOK
	}
	return *w.code
}

// handle 把 handler 适配为 http.HandlerFunc 并统一处理错误。
func (s *Server) handle(fn func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			s.fail(w, r, err)
		}
	}
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"requestId,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusInternalServerError
	apiCode := "internal.error"
	msg := "internal error"
	var details map[string]any
	var ae *APIError
	if errors.As(err, &ae) {
		code = ae.Status
		apiCode = ae.Code
		msg = ae.Message
		details = ae.Details
	} else if errors.Is(err, context.Canceled) {
		code, apiCode, msg = 499, "request.canceled", "request canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, apiCode, msg = http.StatusGatewayTimeout, "request.timeout", "request timed out"
	} else {
		s.log.Error("http_error", "path", r.URL.Path, "error", err)
	}
	writeJSON(w, code, errorEnvelope{Error: errorDetail{
		Code: apiCode, Message: msg, RequestID: requestID(r), Details: details,
	}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeOK(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

func (s *Server) body(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return Errf(http.StatusBadRequest, "request.bad_json", "invalid JSON body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 系统端点
// ---------------------------------------------------------------------------

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "ts": s.opt.Now().UTC()})
	return nil
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) error {
	rd := s.be.Ready()
	code := http.StatusOK
	if !rd.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, rd)
	return nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) error {
	writeOK(w, s.be.Health())
	return nil
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) error {
	writeOK(w, s.be.Version())
	return nil
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, s.be.Metrics())
	_, _ = fmt.Fprintf(w, "# HELP uic_sse_clients 当前 SSE 连接数\n# TYPE uic_sse_clients gauge\nuic_sse_clients %d\n", s.sseLive.Load())
	return nil
}

// ---------------------------------------------------------------------------
// 设备与点位
// ---------------------------------------------------------------------------

func (s *Server) devices(w http.ResponseWriter, r *http.Request) error {
	items := s.be.Devices(strings.TrimSpace(r.URL.Query().Get("q")))
	if items == nil {
		items = []DeviceView{}
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
	return nil
}

func (s *Server) device(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("device")
	d, ok := s.be.Device(id)
	if !ok {
		return Errf(http.StatusNotFound, "device.not_found", "device %q not found", id)
	}
	writeOK(w, d)
	return nil
}

func (s *Server) devicePoints(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("device")
	if _, ok := s.be.Device(id); !ok {
		return Errf(http.StatusNotFound, "device.not_found", "device %q not found", id)
	}
	limit := intQuery(r, "limit", 1000)
	items := s.be.Points(PointFilter{Device: id, Q: r.URL.Query().Get("q"), Limit: limit})
	if items == nil {
		items = []model.Sample{}
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
	return nil
}

func (s *Server) points(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	var quality []string
	for _, v := range q["quality"] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				quality = append(quality, p)
			}
		}
	}
	limit := intQuery(r, "limit", 100)
	if limit > 1000 {
		limit = 1000
	}
	items := s.be.Points(PointFilter{
		Device: q.Get("device"), Q: q.Get("q"), Quality: quality, Limit: limit,
	})
	if items == nil {
		items = []model.Sample{}
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
	return nil
}

func (s *Server) point(w http.ResponseWriter, r *http.Request) error {
	device, point := r.PathValue("device"), r.PathValue("point")
	smp, ok := s.be.Point(device, point)
	if !ok {
		return Errf(http.StatusNotFound, "point.not_found", "point %q not found on device %q", point, device)
	}
	writeOK(w, smp)
	return nil
}

type writeBody struct {
	Writes []WriteItem `json:"writes"`
}

func (s *Server) writeOne(w http.ResponseWriter, r *http.Request) error {
	device, point := r.PathValue("device"), r.PathValue("point")
	var item WriteItem
	if err := s.body(w, r, &item); err != nil {
		return err
	}
	if item.Point == "" {
		item.Point = point
	}
	if item.Point != point {
		return Errf(http.StatusBadRequest, "request.invalid", "body point %q does not match path %q", item.Point, point)
	}
	return s.doWrite(w, r, device, []WriteItem{item})
}

func (s *Server) writeBatch(w http.ResponseWriter, r *http.Request) error {
	device := r.PathValue("device")
	var body writeBody
	if err := s.body(w, r, &body); err != nil {
		return err
	}
	if len(body.Writes) == 0 {
		return Errf(http.StatusBadRequest, "request.invalid", "writes must not be empty")
	}
	if len(body.Writes) > 100 {
		return Errf(http.StatusBadRequest, "request.invalid", "at most 100 writes per request")
	}
	return s.doWrite(w, r, device, body.Writes)
}

func (s *Server) doWrite(w http.ResponseWriter, r *http.Request, device string, items []WriteItem) error {
	ctx := r.Context()
	if idem := r.Header.Get("X-Idempotency-Key"); idem != "" {
		ctx = context.WithValue(ctx, idemKey{}, idem)
	}
	results, err := s.be.Write(ctx, device, items)
	if err != nil {
		return err
	}
	writeOK(w, map[string]any{"results": results})
	return nil
}

type idemKey struct{}

// IdempotencyKey 从 context 中取幂等键（runtime 用于 5 分钟去重）。
func IdempotencyKey(ctx context.Context) string {
	if v, ok := ctx.Value(idemKey{}).(string); ok {
		return v
	}
	return ""
}

func intQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ---------------------------------------------------------------------------
// 报警
// ---------------------------------------------------------------------------

// alarmStates / alarmSeverities 是查询过滤的合法取值。
func parseAlarmFilter(r *http.Request) (alarm.Filter, error) {
	q := r.URL.Query()
	f := alarm.Filter{Q: strings.TrimSpace(q.Get("q")), Limit: intQuery(r, "limit", 0)}

	collect := func(key string) []string {
		var out []string
		for _, v := range q[key] {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
		return out
	}
	for _, v := range collect("state") {
		st := alarm.State(v)
		if !st.Valid() {
			return f, Errf(http.StatusBadRequest, "request.invalid",
				"unknown state %q (want normal|pending|active|acked|cleared)", v)
		}
		f.States = append(f.States, st)
	}
	for _, v := range collect("severity") {
		sv := alarm.Severity(v)
		if !sv.Valid() {
			return f, Errf(http.StatusBadRequest, "request.invalid",
				"unknown severity %q (want info|warning|critical)", v)
		}
		f.Severities = append(f.Severities, sv)
	}
	return f, nil
}

func (s *Server) alarmsList(w http.ResponseWriter, r *http.Request) error {
	f, err := parseAlarmFilter(r)
	if err != nil {
		return err
	}
	items := s.be.Alarms(f)
	if items == nil {
		items = []alarm.Status{}
	}
	writeOK(w, map[string]any{
		"items":  items,
		"total":  len(items),
		"counts": s.be.AlarmCounts(),
	})
	return nil
}

func (s *Server) alarmGet(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	st, ok := s.be.Alarm(id)
	if !ok {
		return Errf(http.StatusNotFound, "alarm.not_found", "alarm %q not found", id)
	}
	writeOK(w, st)
	return nil
}

type alarmAckBody struct {
	IDs []string `json:"ids"`
	All bool     `json:"all"`
	// By 是操作人标识。由调用方提供而不是从 Token 推导：Token 是共享的
	// 接入凭据，不等同于「谁按下了确认」。
	By string `json:"by,omitempty"`
}

func (s *Server) alarmsAck(w http.ResponseWriter, r *http.Request) error {
	var body alarmAckBody
	if err := s.body(w, r, &body); err != nil {
		return err
	}
	if body.All && len(body.IDs) > 0 {
		return Errf(http.StatusBadRequest, "request.invalid", "all and ids are mutually exclusive")
	}
	if !body.All && len(body.IDs) == 0 {
		return Errf(http.StatusBadRequest, "request.invalid", "ids must not be empty unless all is true")
	}
	return s.doAck(w, r, body.IDs, body.All, body.By)
}

func (s *Server) alarmAck(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	var body alarmAckBody
	// 单条确认允许空 body，但配了 body 时仍要严格解析。
	if r.ContentLength != 0 {
		if err := s.body(w, r, &body); err != nil {
			return err
		}
	}
	if body.All || len(body.IDs) > 0 {
		return Errf(http.StatusBadRequest, "request.invalid", "body ids/all are not allowed on a single-alarm ack")
	}
	return s.doAck(w, r, []string{id}, false, body.By)
}

func (s *Server) doAck(w http.ResponseWriter, r *http.Request, ids []string, all bool, by string) error {
	by = strings.TrimSpace(by)
	if len(by) > 64 {
		return Errf(http.StatusBadRequest, "request.invalid", "by must be <= 64 bytes")
	}
	out, err := s.be.AckAlarms(r.Context(), ids, all, by)
	if err != nil {
		return err
	}
	if out == nil {
		out = []alarm.Status{}
	}
	writeOK(w, map[string]any{"acknowledged": out, "total": len(out)})
	return nil
}

// alarmsStream 把报警状态迁移推给浏览器。
//
// 与 /api/v1/stream（点位的值）分开：消费方完全不同——点位流是趋势图和
// 实时值，报警流是弹窗与声音。合成一路会让前端在一堆采样里筛选报警。
func (s *Server) alarmsStream(w http.ResponseWriter, r *http.Request) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Errf(http.StatusInternalServerError, "internal.sse_unsupported", "streaming not supported")
	}
	hb := s.heartbeat(r)
	ch, cancel := s.be.AlarmStream(256)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s.sseLive.Add(1)
	defer s.sseLive.Add(-1)

	hello := map[string]any{
		"server": "uic", "apiVersion": 1,
		"heartbeatSeconds": int(hb / time.Second),
		"counts":           s.be.AlarmCounts(),
	}
	if err := writeSSE(w, "hello", "", hello); err != nil {
		return nil
	}
	flusher.Flush()

	tick := time.NewTicker(hb)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-tick.C:
			if err := writeSSE(w, "heartbeat", "", map[string]any{"ts": s.opt.Now().UTC()}); err != nil {
				return nil
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := writeSSE(w, string(ev.Type), ev.Status.ID, ev); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

// heartbeat 从查询串解析 SSE 心跳周期（1s ~ 5min，非法值回落默认）。
func (s *Server) heartbeat(r *http.Request) time.Duration {
	hb := s.opt.Heartbeat
	if v := r.URL.Query().Get("heartbeat"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Second && d <= 5*time.Minute {
			hb = d
		}
	}
	return hb
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

func (s *Server) configGet(w http.ResponseWriter, _ *http.Request) error {
	raw, err := s.be.Config()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	return nil
}

func (s *Server) configValidate(w http.ResponseWriter, r *http.Request) error {
	raw, err := s.readConfig(w, r)
	if err != nil {
		return err
	}
	if err := s.be.ValidateConfig(raw); err != nil {
		return err
	}
	writeOK(w, map[string]any{"valid": true})
	return nil
}

func (s *Server) configApply(w http.ResponseWriter, r *http.Request) error {
	raw, err := s.readConfig(w, r)
	if err != nil {
		return err
	}
	res, err := s.be.ApplyConfig(raw, "api")
	if err != nil {
		return err
	}
	if len(res.Restarted) == 0 {
		res.Restarted = []string{}
	}
	writeOK(w, res)
	return nil
}

func (s *Server) readConfig(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, Errf(http.StatusBadRequest, "request.bad_json", "read body: %v", err)
	}
	if len(raw) == 0 {
		return nil, Errf(http.StatusBadRequest, "request.bad_json", "empty body")
	}
	return json.RawMessage(raw), nil
}

func (s *Server) configVersions(w http.ResponseWriter, _ *http.Request) error {
	items := s.be.ConfigVersions()
	if items == nil {
		items = []config.VersionInfo{}
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
	return nil
}

func (s *Server) configVersion(w http.ResponseWriter, r *http.Request) error {
	raw, err := s.be.ConfigVersion(r.PathValue("id"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	return nil
}

func (s *Server) configDiff(w http.ResponseWriter, r *http.Request) error {
	d, err := s.be.ConfigDiff(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		return err
	}
	writeOK(w, d)
	return nil
}

func (s *Server) configRollback(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Version string `json:"version"`
	}
	if err := s.body(w, r, &body); err != nil {
		return err
	}
	if body.Version == "" {
		return Errf(http.StatusBadRequest, "request.invalid", "version is required")
	}
	res, err := s.be.Rollback(body.Version)
	if err != nil {
		return err
	}
	writeOK(w, res)
	return nil
}

// ---------------------------------------------------------------------------
// 命令与诊断
// ---------------------------------------------------------------------------

func (s *Server) command(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Action  string         `json:"action"`
		Params  map[string]any `json:"params"`
		Confirm bool           `json:"confirm"`
	}
	if err := s.body(w, r, &body); err != nil {
		return err
	}
	if body.Action == "" {
		return Errf(http.StatusBadRequest, "request.invalid", "action is required")
	}
	if body.Params == nil {
		body.Params = map[string]any{}
	}
	if body.Confirm {
		body.Params["_confirm"] = true
	}
	res, err := s.be.Command(r.Context(), body.Action, body.Params)
	if err != nil {
		return err
	}
	writeOK(w, map[string]any{"action": body.Action, "ok": true, "result": res})
	return nil
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit := intQuery(r, "limit", 200)
	if limit > 2000 {
		limit = 2000
	}
	items := s.be.Logs(q.Get("device"), q.Get("level"), limit)
	if items == nil {
		items = []obs.Entry{}
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
	return nil
}

// ---------------------------------------------------------------------------
// 历史查询
// ---------------------------------------------------------------------------

// history 查询一段时间内的采样。
//
// 参数全部走 query string（幂等、可缓存、可直接贴进浏览器），因此这里对
// 每个参数的解析都必须严格：静默忽略一个拼错的 `stpe=1m` 会让调用方拿到
// 一堆原始点，然后以为是聚合坏了。
func (s *Server) history(w http.ResponseWriter, r *http.Request) error {
	q, err := parseHistoryQuery(r, s.opt.Now())
	if err != nil {
		return err
	}
	res, err := s.be.History(q)
	if err != nil {
		return err
	}
	writeOK(w, res)
	return nil
}

func parseHistoryQuery(r *http.Request, now time.Time) (history.Query, error) {
	v := r.URL.Query()
	var q history.Query
	q.Device = strings.TrimSpace(v.Get("device"))
	q.Point = strings.TrimSpace(v.Get("point"))
	if q.Device == "" || q.Point == "" {
		return q, Errf(http.StatusBadRequest, "request.invalid",
			"device and point are required (e.g. ?device=press-01&point=power)")
	}

	from, err := parseTimeParam(v.Get("from"), now)
	if err != nil {
		return q, Errf(http.StatusBadRequest, "history.invalid_range", "from: %v", err)
	}
	to, err := parseTimeParam(v.Get("to"), now)
	if err != nil {
		return q, Errf(http.StatusBadRequest, "history.invalid_range", "to: %v", err)
	}
	q.From, q.To = from, to

	if raw := strings.TrimSpace(v.Get("step")); raw != "" {
		d, derr := time.ParseDuration(raw)
		if derr != nil {
			return q, Errf(http.StatusBadRequest, "history.invalid_query",
				"step: %q is not a duration (try 10s, 1m, 1h)", raw)
		}
		if d <= 0 {
			return q, Errf(http.StatusBadRequest, "history.invalid_query", "step must be > 0")
		}
		q.Step = d
	}
	if raw := strings.TrimSpace(v.Get("agg")); raw != "" {
		q.Agg = history.Aggregation(raw)
		if !q.Agg.Valid() {
			return q, Errf(http.StatusBadRequest, "history.invalid_query",
				"agg: %q is not one of last|first|avg|min|max|count", raw)
		}
	}
	if raw := strings.TrimSpace(v.Get("limit")); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n <= 0 {
			return q, Errf(http.StatusBadRequest, "request.invalid", "limit must be a positive integer")
		}
		q.Limit = n
	}
	return q, nil
}

// parseTimeParam 接受三种写法：
//
//	""             → 交给 store 用默认窗口
//	"now"          → 服务器当前时间
//	"-1h" / "+30m" → 相对服务器当前时间（运维最常用的写法）
//	RFC3339        → 绝对时刻
func parseTimeParam(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "":
		return time.Time{}, nil
	case "now":
		return now, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a duration (-1h) or an RFC3339 timestamp", raw)
}

func (s *Server) bundle(w http.ResponseWriter, r *http.Request) error {
	data, err := s.be.DiagnosticsBundle(r.Context())
	if err != nil {
		return err
	}
	name := "uic-diag-" + s.opt.Now().UTC().Format("20060102T150405Z") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return nil
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

func (s *Server) stream(w http.ResponseWriter, r *http.Request) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Errf(http.StatusInternalServerError, "internal.sse_unsupported", "streaming not supported")
	}
	q := r.URL.Query()
	pat, err := parsePatterns(q.Get("points"))
	if err != nil {
		return err
	}
	quality := parseSet(q.Get("quality"))
	hb := s.opt.Heartbeat
	if v := q.Get("heartbeat"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Second && d <= 5*time.Minute {
			hb = d
		}
	}

	filter := func(sm model.Sample) bool {
		if !pat.match(sm.DeviceID, sm.PointID) {
			return false
		}
		if len(quality) > 0 && !quality[string(sm.Quality)] {
			return false
		}
		return true
	}
	ch, cancel := s.be.Stream(filter, 512)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s.sseLive.Add(1)
	defer s.sseLive.Add(-1)

	hello := map[string]any{
		"server": "uic", "apiVersion": 1,
		"heartbeatSeconds": int(hb / time.Second),
		"points":           pat.String(),
	}
	if err := writeSSE(w, "hello", "", hello); err != nil {
		return nil
	}
	flusher.Flush()

	tick := time.NewTicker(hb)
	defer tick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-tick.C:
			if err := writeSSE(w, "heartbeat", "", map[string]any{"ts": s.opt.Now().UTC()}); err != nil {
				return nil
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := writeEvent(w, ev); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w io.Writer, ev cache.Event) error {
	sm := ev.Sample
	id := fmt.Sprintf("%s:%s:%d", sm.DeviceID, sm.PointID, sm.Seq)
	switch ev.Type {
	case cache.EventQuality:
		return writeSSE(w, "quality", id, map[string]any{
			"device": sm.DeviceID, "point": sm.PointID,
			"from": ev.From, "to": ev.To, "at": sm.CollectedAt,
		})
	default:
		return writeSSE(w, "sample", id, sm)
	}
}

func writeSSE(w io.Writer, event, id string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	_, err = io.WriteString(w, b.String())
	return err
}

type patterns struct {
	list [][2]string // [device, point]
	all  bool
}

func parsePatterns(s string) (patterns, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return patterns{all: true}, nil
	}
	var p patterns
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "/")
		if i <= 0 || i == len(part)-1 {
			return p, Errf(http.StatusBadRequest, "request.invalid", "invalid points pattern %q (want device/point)", part)
		}
		dev, pt := part[:i], part[i+1:]
		if strings.Contains(dev, "*") && dev != "*" {
			return p, Errf(http.StatusBadRequest, "request.invalid", "invalid device wildcard %q", dev)
		}
		if strings.Contains(pt, "*") && pt != "*" {
			return p, Errf(http.StatusBadRequest, "request.invalid", "invalid point wildcard %q", pt)
		}
		p.list = append(p.list, [2]string{dev, pt})
	}
	if len(p.list) == 0 {
		p.all = true
	}
	return p, nil
}

// String 返回可读的通配列表，用于 hello 事件回显。
func (p patterns) String() []string {
	out := make([]string, 0, len(p.list))
	for _, x := range p.list {
		out = append(out, x[0]+"/"+x[1])
	}
	return out
}

func (p patterns) match(device, point string) bool {
	if p.all {
		return true
	}
	for _, x := range p.list {
		if (x[0] == "*" || x[0] == device) && (x[1] == "*" || x[1] == point) {
			return true
		}
	}
	return false
}

func parseSet(s string) map[string]bool {
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
