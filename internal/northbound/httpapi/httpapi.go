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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"uic/internal/alarm"
	"uic/internal/audit"
	"uic/internal/auth"
	"uic/internal/cache"
	"uic/internal/config"
	"uic/internal/history"
	"uic/internal/model"
	"uic/internal/obs"
	"uic/internal/rbac"
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
	// Auth 是多身份凭据表。为 nil 时退化为「Token 为空即开放、非空即全权」的
	// 单凭据模式（供既有测试与最小部署使用）。
	//
	// 类型是接口而不是 *auth.Store：runtime 需要在配置热更新后整体替换凭据表，
	// 而 HTTP 层在启动时就拿到了它。见 auth.Reloadable 的注释。
	Auth Credentials
	// Audit 接收认证/授权/写入记录。为 nil 时不记录（测试环境）。
	Audit Auditor
}

// Credentials 是 HTTP 层需要的凭据能力。runtime 传 auth.Reloadable。
type Credentials interface {
	Authenticate(token string) (auth.Principal, bool)
	Open() bool
	Users() []auth.Principal
}

// Auditor 是 HTTP 层的审计出口。runtime 实现它；测试可以用假实现。
//
// 接口只有一个方法，参数是「已发生的事实」而不是业务对象：HTTP 层不该
// 知道审计日志怎么存、存在哪、怎么分片。
type Auditor interface {
	AuditHTTP(ev AuditEvent)
}

// AuditEvent 是一条 HTTP 侧审计事实。
type AuditEvent struct {
	User      string
	Role      string
	Action    string
	Target    string
	Result    string
	Code      string
	Message   string
	Remote    string
	RequestID string
	Detail    map[string]string
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
	// InstanceID 是本实例的稳定身份（持久化在 dataDir）。同站点多实例部署时
	// 用它区分「这是哪台」，并与 MQTT clientID/Sparkplug nodeId 的默认值共享
	// 同一来源，使默认不冲突。空表示加载失败（见 runtime 启动日志）。
	InstanceID string `json:"instanceId,omitempty"`
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

// StagedPolicy 是灰度发布的推进策略。
type StagedPolicy struct {
	// BatchSize 是每批设备数；<=0 归一为 1。
	BatchSize int `json:"batchSize,omitempty"`
	// Observe 是每批下发后的观察窗口：窗口内拒绝推进下一批。
	Observe model.Duration `json:"observe,omitempty"`
	// MaxFailureRate 是观察窗口结束时「已下发设备」的采集失败率上限（0~1）。
	// 超过则以 baseVersion 自动回滚并终止灰度；0 表示不做健康检查。
	MaxFailureRate float64 `json:"maxFailureRate,omitempty"`
}

// StagedStatus 是灰度发布（staged rollout）的对外视图。
//
// 灰度把一次全量配置变更切成若干批设备逐批下发：每批只改动该批设备，其余
// 设备保持当前生效配置。Active=false 且 Completed=true 表示已全部下发；
// Aborted=true 表示自动或人工回滚过（PausedReason 说明原因）。
type StagedStatus struct {
	Active        bool   `json:"active"`
	TargetVersion string `json:"targetVersion"`
	BaseVersion   string `json:"baseVersion"`
	TotalBatches  int    `json:"totalBatches"`
	// NextBatch 是下一批的序号；等于 TotalBatches 表示已下发完。
	NextBatch int  `json:"nextBatch"`
	Completed bool `json:"completed"`
	Aborted   bool `json:"aborted"`
	// Batches 是每批的设备 ID。Devices 是它的拍平结果（便于看整体影响面）。
	Batches [][]string `json:"batches"`
	Devices []string   `json:"devices"`
	// Observing/ObserveUntil/FailureRate/MaxFailureRate 构成观察窗口与健康判定。
	Observing      bool           `json:"observing"`
	Observe        model.Duration `json:"observe,omitempty"`
	ObserveUntil   time.Time      `json:"observeUntil,omitempty"`
	FailureRate    float64        `json:"failureRate"`
	MaxFailureRate float64        `json:"maxFailureRate,omitempty"`
	// PausedReason 是自动回滚的原因（人工 abort 为空）。
	PausedReason string    `json:"pausedReason,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
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
	ApplyConfig(ctx context.Context, raw json.RawMessage, source string) (config.ApplyResult, error)
	ConfigVersions() []config.VersionInfo
	ConfigVersion(id string) (json.RawMessage, error)
	ConfigDiff(from, to string) (config.Diff, error)
	Rollback(ctx context.Context, version string) (config.ApplyResult, error)
	BeginStaged(ctx context.Context, raw json.RawMessage, source string, policy StagedPolicy) (StagedStatus, error)
	AdvanceStaged(ctx context.Context, source string) (StagedStatus, error)
	AbortStaged(ctx context.Context, source string) (config.ApplyResult, error)
	StagedStatusOf() StagedStatus

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

	AuditQuery(f audit.Filter) (audit.Result, error)
	AuditVerify() (audit.VerifyResult, error)
	AuditStats() (audit.Stats, bool)

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

// ---------------------------------------------------------------------------
// 身份与授权
// ---------------------------------------------------------------------------

type actorCtxKey struct{}

// Actor 是已通过认证的调用方。HTTP 层只用它做两件事：判定权限，以及写进
// 审计记录。角色解析到具体权限判定发生在 rbac 包。
type Actor struct {
	User      string
	Role      rbac.Role
	Remote    string
	RequestID string
}

// ActorFrom 从 context 取出调用方身份。ok=false 表示请求未经过认证流程
// （例如内部调用方），此时上游应当记 system。
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(Actor)
	return a, ok
}

// guard 认证 + 授权。它是所有业务路由的唯一入口：权限写在路由注册处，
// 漏写即编译期不可表达。
//
// 两种失败分开记：未认证（401 auth.unauthorized）是有意为之的探测或凭据
// 问题；越权（403 auth.forbidden）是「有身份但不够」——后者才需要进审计，
// 因为前者会被扫描器无限产生。
func (s *Server) guard(perm rbac.Permission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.authenticate(r)
		if !ok {
			s.auditDenied(r, actor, "auth.unauthorized", http.StatusUnauthorized,
				"missing or invalid bearer token", perm)
			w.Header().Set("WWW-Authenticate", `Bearer realm="uic"`)
			s.fail(w, r, Errf(http.StatusUnauthorized, "auth.unauthorized",
				"missing or invalid bearer token"))
			return
		}
		if !rbac.Can(actor.Role, perm) {
			s.auditDenied(r, actor, "auth.forbidden", http.StatusForbidden,
				"role "+string(actor.Role)+" lacks "+string(perm), perm)
			s.fail(w, r, Errf(http.StatusForbidden, "auth.forbidden",
				"role %q lacks permission %q", actor.Role, perm))
			return
		}
		actor.Remote = r.RemoteAddr
		actor.RequestID = requestID(r)
		next(w, r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, actor)))
	}
}

// guardReadPublic 保留旧语义：MetricsPublic 为 true 时 metrics 免认证。
func (s *Server) guardReadPublic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opt.MetricsPublic && r.URL.Path == "/api/v1/metrics" {
			next(w, r)
			return
		}
		s.guard(rbac.PermRead, next)(w, r)
	}
}

// authenticate 解析凭据。opt.Auth 为 nil 时退化为「Token 非空即全权」，这是
// 兼容旧部署的单凭据路径。
//
// 关键细节：opt.Auth 为 nil 只发生在调用方没接多身份凭据表时（例如旧测试
// 直接构造 Server）。此时如果 opt.Token 也是空，历史上是**完全开放**而不是
// 拒绝，runtime 依赖这个语义启动无认证的最小部署，因此这里保持放行。
func (s *Server) authenticate(r *http.Request) (Actor, bool) {
	if s.opt.Auth == nil {
		if s.opt.Token == "" {
			return Actor{User: "anonymous", Role: rbac.RoleAdmin}, true
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" {
			return Actor{}, false
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.Token)) != 1 {
			return Actor{}, false
		}
		return Actor{User: "api", Role: rbac.RoleAdmin}, true
	}
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	// 开放模式（未配置任何凭据）下认证被显式关闭：不带 Token 也应通过。
	// 这正是 opt.Auth 为 nil 时「Token 为空即开放」的等价语义，只是搬到了
	// 凭据表内部，以便热更新时能在「多身份」与「开放」之间切换。
	if s.opt.Auth.Open() {
		return Actor{User: "anonymous", Role: rbac.RoleAdmin}, true
	}
	if !strings.HasPrefix(raw, "Bearer ") {
		return Actor{}, false
	}
	p, ok := s.opt.Auth.Authenticate(strings.TrimSpace(raw[len("Bearer "):]))
	if !ok {
		return Actor{}, false
	}
	return Actor{User: p.User, Role: p.Role}, true
}

func (s *Server) auditDenied(r *http.Request, a Actor, code string, status int, msg string, perm rbac.Permission) {
	if s.opt.Audit == nil {
		return
	}
	// Action 记「发生了哪类拒绝」，Target 记「当时要的权限」：
	// 事后最常问的两个问题是「有没有人一直在试」与「他缺的是哪个权限」。
	s.opt.Audit.AuditHTTP(AuditEvent{
		User:      actorName(a),
		Role:      string(a.Role),
		Action:    code,
		Target:    string(perm),
		Result:    auditDeniedResult(status),
		Code:      code,
		Message:   msg,
		Remote:    r.RemoteAddr,
		RequestID: requestID(r),
	})
}

func actorName(a Actor) string {
	if a.User == "" {
		return "anonymous"
	}
	return a.User
}

func auditDeniedResult(status int) string {
	if status == http.StatusForbidden {
		return "denied"
	}
	return "error"
}

// whoami 回答「我在这台网关上是谁」。公开端点：客户端要先知道「这里到底
// 要不要凭据」才能决定弹不弹登录框。
//
// 它**不列举用户**。之前那版把整张凭据表的用户名与角色返回给任何匿名
// 请求，等于把账号清单（以及谁有 admin）免费送给扫描器；枚举是
// /auth/users 的事，需要 audit.read。
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) error {
	body := map[string]any{
		"user": "", "role": "", "open": false, "authenticated": false,
		"permissions": []string{},
	}
	actor, ok := s.authenticate(r)
	if !ok {
		// 认证开着但这次没带（或带错）凭据。200 而不是 401：调用方问的
		// 是「认证模式」，不是「我能进吗」——后者由真正的业务端点回答。
		writeOK(w, body)
		return nil
	}
	perms := rbac.Grants(actor.Role)
	names := make([]string, len(perms))
	for i, p := range perms {
		names[i] = string(p)
	}
	body["user"] = actor.User
	body["role"] = string(actor.Role)
	body["open"] = s.openMode()
	body["authenticated"] = !body["open"].(bool)
	body["permissions"] = names
	writeOK(w, body)
	return nil
}

// openMode 报告认证是否处于「无凭据」的开放状态。
func (s *Server) openMode() bool {
	if s.opt.Auth == nil {
		return s.opt.Token == ""
	}
	return s.opt.Auth.Open()
}

// authUsers 列出全部账号，需要 audit.read。
//
// 只返回用户名与角色：Token 与哈希一旦出现在响应里，它就同时进了浏览器
// 历史、代理日志和截图。凭据的轮换不走这个接口。
func (s *Server) authUsers(w http.ResponseWriter, _ *http.Request) error {
	items := []map[string]any{}
	if s.opt.Auth == nil {
		if s.opt.Token != "" {
			items = append(items, map[string]any{"user": "api", "role": string(rbac.RoleAdmin)})
		}
	} else {
		for _, p := range s.opt.Auth.Users() {
			items = append(items, map[string]any{"user": p.User, "role": string(p.Role)})
		}
	}
	writeOK(w, map[string]any{"items": items, "open": s.openMode(), "count": len(items)})
	return nil
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
	// route 把「路径模式 + 所需权限」绑在一起。权限写在路由旁边而不是
	// 集中在一张表里，是因为新增路由时最容易被忘掉的就是权限——写在这里，
	// 漏掉就是编译期可见的（route 的第二个参数不能省）。
	route := func(pattern string, perm rbac.Permission, fn func(http.ResponseWriter, *http.Request) error) {
		s.mux.HandleFunc(pattern, s.guard(perm, h(fn)))
	}
	// public 是无需任何权限的端点：健康探针与版本号必须能被负载均衡与
	// 监控系统直接访问，否则「服务是否活着」本身也要凭据才能回答。
	public := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		s.mux.HandleFunc(pattern, h(fn))
	}

	public("GET /healthz", s.healthz)
	public("GET /readyz", s.readyz)
	public("GET /api/v1/version", s.version)
	s.mux.HandleFunc("GET /api/v1/health", s.guardReadPublic(h(s.health)))
	s.mux.HandleFunc("GET /api/v1/metrics", s.guardReadPublic(h(s.metrics)))
	public("GET /api/v1/auth/me", s.whoami)

	route("GET /api/v1/devices", rbac.PermRead, s.devices)
	route("GET /api/v1/devices/{device}", rbac.PermRead, s.device)
	route("GET /api/v1/devices/{device}/points", rbac.PermRead, s.devicePoints)

	route("GET /api/v1/points", rbac.PermRead, s.points)
	route("GET /api/v1/points/{device}/{point}", rbac.PermRead, s.point)
	route("POST /api/v1/points/{device}/{point}/write", rbac.PermWrite, s.writeOne)
	route("POST /api/v1/points/{device}/write", rbac.PermWrite, s.writeBatch)

	route("GET /api/v1/config", rbac.PermConfigRead, s.configGet)
	route("POST /api/v1/config/validate", rbac.PermConfigWrite, s.configValidate)
	route("POST /api/v1/config/apply", rbac.PermConfigWrite, s.configApply)
	route("GET /api/v1/config/versions", rbac.PermConfigRead, s.configVersions)
	route("GET /api/v1/config/versions/{id}", rbac.PermConfigRead, s.configVersion)
	route("GET /api/v1/config/diff", rbac.PermConfigRead, s.configDiff)
	route("POST /api/v1/config/rollback", rbac.PermConfigWrite, s.configRollback)
	route("GET /api/v1/config/staged", rbac.PermConfigRead, s.configStagedStatus)
	route("POST /api/v1/config/staged", rbac.PermConfigWrite, s.configStagedBegin)
	route("POST /api/v1/config/staged/advance", rbac.PermConfigWrite, s.configStagedAdvance)
	route("POST /api/v1/config/staged/abort", rbac.PermConfigWrite, s.configStagedAbort)

	// 命令的路由级权限只到 PermCommand；具体动作是高危还是普通，要解析
	// 请求体才知道，因此在 handler 里再判一次（见 command）。
	route("POST /api/v1/commands", rbac.PermCommand, s.command)
	route("GET /api/v1/logs", rbac.PermDiagnostics, s.logs)
	route("GET /api/v1/stream", rbac.PermRead, s.stream)

	route("GET /api/v1/alarms", rbac.PermRead, s.alarmsList)
	route("POST /api/v1/alarms/ack", rbac.PermAlarmAck, s.alarmsAck)
	route("GET /api/v1/alarms/stream", rbac.PermRead, s.alarmsStream)
	route("GET /api/v1/alarms/{id}", rbac.PermRead, s.alarmGet)
	route("POST /api/v1/alarms/{id}/ack", rbac.PermAlarmAck, s.alarmAck)
	route("GET /api/v1/history", rbac.PermRead, s.history)
	route("GET /api/v1/diagnostics/bundle", rbac.PermDiagnostics, s.bundle)

	route("GET /api/v1/audit", rbac.PermAuditRead, s.auditList)
	route("GET /api/v1/audit/verify", rbac.PermAuditRead, s.auditVerify)
	route("GET /api/v1/audit/stats", rbac.PermAuditRead, s.auditStats)
	route("GET /api/v1/auth/users", rbac.PermAuditRead, s.authUsers)
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
// 这是指标标签的第一条纪律：标签值必须来自有限集合。Prometheus 风格的注册表
// 从不回收标签集合，每出现一个新的标签组合就多占一份内存且永不释放——一个
// 客户端（或误配置的探针）只要反复请求带不同路径参数的 URL，就能把 /metrics
// 撑爆。因此这里不仅归一化已知路由，还必须把每一类前缀下的**任意**子路径
// （包括不存在、返回 404 的那一段）都折叠到有限集合，否则 404 就成了绕过
// 归一化的缺口。
func metricPath(p string) string {
	const dev = "/api/v1/devices/"
	const pts = "/api/v1/points/"
	const ver = "/api/v1/config/versions/"
	const alm = "/api/v1/alarms/"
	switch {
	case strings.HasPrefix(p, dev):
		// seg[0] 是设备号，一律归一化为占位符；其后只保留固定的 "points"
		// 段，其余（如 /{device}/xyz、/{device}/points/{x} 这类并不存在的
		// 子路径）全部折叠，避免任意路径进入标签。
		seg := strings.Split(strings.TrimPrefix(p, dev), "/")
		out := dev + ":device"
		if len(seg) >= 2 && seg[1] == "points" {
			out += "/points"
		}
		return out
	case strings.HasPrefix(p, pts):
		seg := strings.Split(strings.TrimPrefix(p, pts), "/")
		if len(seg) >= 2 {
			switch {
			case seg[1] == "write":
				return pts + ":device/write"
			case len(seg) >= 3 && seg[2] == "write":
				return pts + ":device/:point/write"
			default:
				return pts + ":device/:point"
			}
		}
	case strings.HasPrefix(p, ver):
		return ver + ":id"
	case strings.HasPrefix(p, alm):
		// /alarms/{id}（查单条）与 /alarms/{id}/ack（确认）带动态 id，
		// 且查不到会 404——若不归一化，任意 id 都会成为新标签。/alarms
		// 列表本身是固定路径，走不到这个前缀。
		seg := strings.Split(strings.TrimPrefix(p, alm), "/")
		if len(seg) >= 2 && seg[1] == "ack" {
			return alm + ":id/ack"
		}
		return alm + ":id"
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
	// 直接调用 guard（测试或未来的非 ServeHTTP 入口）时 ctx 里没有注入的 ID，
	// 退回客户端提供的那个；都没有就留空，不编一个假的。
	return r.Header.Get("X-Request-Id")
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
	res, err := s.be.ApplyConfig(r.Context(), raw, "api")
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
	res, err := s.be.Rollback(r.Context(), body.Version)
	if err != nil {
		return err
	}
	writeOK(w, res)
	return nil
}

// ---------------------------------------------------------------------------
// 灰度发布（staged rollout）
// ---------------------------------------------------------------------------

// configStagedStatus 返回当前灰度进度。无灰度时返回 active=false。
func (s *Server) configStagedStatus(w http.ResponseWriter, _ *http.Request) error {
	writeOK(w, s.be.StagedStatusOf())
	return nil
}

// configStagedBegin 开始一次灰度发布：请求体是完整的目标配置与推进策略。
//
// 目标配置放在 `config` 字段下，避免与 batchSize 等控制字段混在同一层导致
// 「配置里恰好有叫 batchSize 的键」时歧义。
//
//	{"batchSize": 10, "observe": "30s", "maxFailureRate": 0.2, "config": { ... }}
func (s *Server) configStagedBegin(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		BatchSize      int             `json:"batchSize"`
		Observe        model.Duration  `json:"observe"`
		MaxFailureRate float64         `json:"maxFailureRate"`
		Config         json.RawMessage `json:"config"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		return Errf(http.StatusBadRequest, "request.bad_json", "decode body: %v", err)
	}
	if len(body.Config) == 0 {
		return Errf(http.StatusBadRequest, "request.invalid", "config is required")
	}
	policy := StagedPolicy{
		BatchSize:      body.BatchSize,
		Observe:        body.Observe,
		MaxFailureRate: body.MaxFailureRate,
	}
	st, err := s.be.BeginStaged(r.Context(), body.Config, "api", policy)
	if err != nil {
		return err
	}
	writeOK(w, st)
	return nil
}

func (s *Server) configStagedAdvance(w http.ResponseWriter, r *http.Request) error {
	st, err := s.be.AdvanceStaged(r.Context(), "api")
	if err != nil {
		return err
	}
	writeOK(w, st)
	return nil
}

func (s *Server) configStagedAbort(w http.ResponseWriter, r *http.Request) error {
	res, err := s.be.AbortStaged(r.Context(), "api")
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
	// 路由级权限只到 PermCommand；高危动作要在这里再判一次。判定表在
	// rbac.CommandPermission，与命令白名单放在同一个仓库里评审。
	if need := rbac.CommandPermission(body.Action); need != rbac.PermCommand {
		actor, _ := ActorFrom(r.Context())
		if !rbac.Can(actor.Role, need) {
			s.auditDenied(r, actor, "auth.forbidden", http.StatusForbidden,
				"action "+body.Action+" requires "+string(need), need)
			return Errf(http.StatusForbidden, "auth.forbidden",
				"role %q cannot run %q (requires %q)", actor.Role, body.Action, need)
		}
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

// ---------------------------------------------------------------------------
// 审计查询
// ---------------------------------------------------------------------------

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := audit.Filter{
		User:   q.Get("user"),
		Role:   q.Get("role"),
		Action: q.Get("action"),
		Result: q.Get("result"),
		Q:      q.Get("q"),
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return Errf(http.StatusBadRequest, "request.invalid", "from must be RFC3339: %v", err)
		}
		f.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return Errf(http.StatusBadRequest, "request.invalid", "to must be RFC3339: %v", err)
		}
		f.To = t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Errf(http.StatusBadRequest, "request.invalid", "limit must be a non-negative integer")
		}
		f.Limit = n
	}
	f.Cursor = q.Get("cursor")
	res, err := s.be.AuditQuery(f)
	var ae *APIError
	if errors.As(err, &ae) {
		return err
	}
	if err != nil {
		return Errf(http.StatusInternalServerError, "audit.query_failed", "%v", err)
	}
	// 时间统一 UTC 输出（docs/03-product/05-northbound-api-spec.md §2.1）。
	items := make([]map[string]any, 0, len(res.Items))
	for _, rec := range res.Items {
		items = append(items, auditView(rec))
	}
	next := res.NextCursor
	if next == "" {
		next = ""
	}
	writeOK(w, map[string]any{
		"items": items, "nextCursor": next,
		"total": res.Total, "scanned": res.Scanned,
	})
	return nil
}

func auditView(rec audit.Record) map[string]any {
	v := map[string]any{
		"seq":    rec.Seq,
		"time":   rec.Time.UTC().Format(time.RFC3339Nano),
		"user":   rec.User,
		"role":   rec.Role,
		"action": rec.Action,
		"result": rec.Result,
	}
	if rec.Target != "" {
		v["target"] = rec.Target
	}
	if rec.Code != "" {
		v["code"] = rec.Code
	}
	if rec.Message != "" {
		v["message"] = rec.Message
	}
	if rec.Remote != "" {
		v["remote"] = rec.Remote
	}
	if rec.RequestID != "" {
		v["requestId"] = rec.RequestID
	}
	if len(rec.Detail) > 0 {
		keys := make([]string, 0, len(rec.Detail))
		for k := range rec.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		detail := make(map[string]string, len(keys))
		for _, k := range keys {
			detail[k] = rec.Detail[k]
		}
		v["detail"] = detail
	}
	return v
}

func (s *Server) auditVerify(w http.ResponseWriter, _ *http.Request) error {
	res, err := s.be.AuditVerify()
	var ae *APIError
	if errors.As(err, &ae) {
		return err
	}
	if err != nil {
		return Errf(http.StatusInternalServerError, "audit.verify_failed", "%v", err)
	}
	status := "ok"
	if !res.OK {
		status = "broken"
	}
	writeOK(w, map[string]any{
		"status": status, "checked": res.Checked,
		"brokenAt": res.BrokenAt, "reason": res.Reason,
	})
	return nil
}

func (s *Server) auditStats(w http.ResponseWriter, _ *http.Request) error {
	st, enabled := s.be.AuditStats()
	if !enabled {
		writeOK(w, map[string]any{"enabled": false})
		return nil
	}
	writeOK(w, map[string]any{
		"enabled":     true,
		"records":     st.Records,
		"bytes":       st.Bytes,
		"maxBytes":    st.MaxBytes,
		"segments":    st.Segments,
		"oldest":      st.Oldest.UTC().Format(time.RFC3339),
		"newest":      st.Newest.UTC().Format(time.RFC3339),
		"appended":    st.Appended,
		"evicted":     st.Evicted,
		"writeErrors": st.WriteErrors,
		"lastSeq":     st.LastSeq,
	})
	return nil
}
