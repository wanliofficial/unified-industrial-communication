package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"uic/internal/audit"
	"uic/internal/auth"
	"uic/internal/rbac"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// recAuditor 收集 HTTP 层发出的审计事实。
type recAuditor struct {
	mu  sync.Mutex
	evs []AuditEvent
}

func (a *recAuditor) AuditHTTP(ev AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
}

func (a *recAuditor) all() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.evs...)
}

func (a *recAuditor) byAction(action string) []AuditEvent {
	var out []AuditEvent
	for _, ev := range a.all() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func testToken(ch byte) string {
	base := "Zx9Qm2Lp7Rt4Vb8Nc3Hf6Kd1Sg5Wj0Ya"
	b := []byte(base[:24])
	b[len(b)-1] = ch
	return string(b)
}

// newRBACServer 起一个带三角色凭据表与审计收集器的 HTTP 层。
func newRBACServer(t *testing.T) (*Server, map[string]string, *recAuditor) {
	t.Helper()
	tokens := map[string]string{
		"viewer":   testToken('a'),
		"operator": testToken('b'),
		"admin":    testToken('c'),
	}
	st, err := auth.NewStore([]auth.User{
		{Name: "vic", Role: rbac.RoleViewer, Token: tokens["viewer"]},
		{Name: "ops", Role: rbac.RoleOperator, Token: tokens["operator"]},
		{Name: "root", Role: rbac.RoleAdmin, Token: tokens["admin"]},
	}, "")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	au := &recAuditor{}
	s, _ := newServer(t, Options{Auth: auth.NewReloadable(st), Audit: au})
	return s, tokens, au
}

func doAs(t *testing.T, s *Server, tok, method, path, body string) (int, string) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// ---------------------------------------------------------------------------
// 认证与授权
// ---------------------------------------------------------------------------

// TestRBACEndpoints 把「谁不能做什么」做成端到端表格。
func TestRBACEndpoints(t *testing.T) {
	s, tok, _ := newRBACServer(t)

	type tc struct {
		name   string
		token  string
		method string
		path   string
		body   string
		want   int
	}
	cases := []tc{
		{"viewer reads devices", tok["viewer"], http.MethodGet, "/api/v1/devices", "", http.StatusOK},
		{"viewer reads point", tok["viewer"], http.MethodGet, "/api/v1/points/d1/p1", "", http.StatusOK},
		{"viewer cannot write", tok["viewer"], http.MethodPost, "/api/v1/points/d1/p1/write", `{"value":{"type":"float32","num":2}}`, http.StatusForbidden},
		{"viewer cannot ack", tok["viewer"], http.MethodPost, "/api/v1/alarms/ack", `{"all":true}`, http.StatusForbidden},
		{"viewer cannot read config", tok["viewer"], http.MethodGet, "/api/v1/config", "", http.StatusForbidden},
		{"viewer cannot read audit", tok["viewer"], http.MethodGet, "/api/v1/audit", "", http.StatusForbidden},
		{"viewer cannot bundle", tok["viewer"], http.MethodGet, "/api/v1/diagnostics/bundle", "", http.StatusForbidden},

		{"operator reads config", tok["operator"], http.MethodGet, "/api/v1/config", "", http.StatusOK},
		{"operator writes", tok["operator"], http.MethodPost, "/api/v1/points/d1/p1/write", `{"value":{"type":"float32","num":2}}`, http.StatusOK},
		{"operator acks", tok["operator"], http.MethodPost, "/api/v1/alarms/ack", `{"all":true}`, http.StatusOK},
		{"operator cannot apply config", tok["operator"], http.MethodPost, "/api/v1/config/apply", `{}`, http.StatusForbidden},
		{"operator cannot read audit", tok["operator"], http.MethodGet, "/api/v1/audit", "", http.StatusForbidden},
		{"operator can flush outbox", tok["operator"], http.MethodPost, "/api/v1/commands", `{"action":"outbox.flush"}`, http.StatusOK},

		{"admin reads audit", tok["admin"], http.MethodGet, "/api/v1/audit", "", http.StatusOK},
		{"admin verifies audit", tok["admin"], http.MethodGet, "/api/v1/audit/verify", "", http.StatusOK},
		{"admin reads audit stats", tok["admin"], http.MethodGet, "/api/v1/audit/stats", "", http.StatusOK},
		{"admin applies config", tok["admin"], http.MethodPost, "/api/v1/config/apply", `{}`, http.StatusOK},
		{"admin rolls back", tok["admin"], http.MethodPost, "/api/v1/config/rollback", `{"version":"sha256:a"}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := doAs(t, s, c.token, c.method, c.path, c.body)
			if code != c.want {
				t.Fatalf("%s %s = %d, want %d (%s)", c.method, c.path, code, c.want, body)
			}
		})
	}
}

// TestDangerCommandNeedsSecondCheck 验证高危命令在 handler 内被二次拦截：
// 路由级权限只有 PermCommand，operator 有它，但 outbox.drop 不给。
func TestDangerCommandNeedsSecondCheck(t *testing.T) {
	s, tok, au := newRBACServer(t)

	code, body := doAs(t, s, tok["operator"], http.MethodPost, "/api/v1/commands",
		`{"action":"outbox.drop","params":{"sink":"journal"},"confirm":true}`)
	if code != http.StatusForbidden {
		t.Fatalf("operator outbox.drop = %d %s, want 403", code, body)
	}
	if !strings.Contains(body, "auth.forbidden") {
		t.Fatalf("body = %s", body)
	}
	// 越权必须留痕，且带上是哪个权限不够。
	evs := au.byAction("auth.forbidden")
	if len(evs) != 1 {
		t.Fatalf("forbidden audit events = %d, want 1", len(evs))
	}
	if evs[0].User != "ops" || evs[0].Target != string(rbac.PermCommandDanger) {
		t.Fatalf("audit event = %+v", evs[0])
	}

	// admin 能执行。
	if code, body := doAs(t, s, tok["admin"], http.MethodPost, "/api/v1/commands",
		`{"action":"outbox.drop","params":{"sink":"journal"},"confirm":true}`); code != http.StatusOK {
		t.Fatalf("admin outbox.drop = %d %s", code, body)
	}
}

// TestUnauthorizedIsAudited 验证带错 Token 的请求会被记录（且 401 带
// WWW-Authenticate，客户端才知道该用 Bearer）。
func TestUnauthorizedIsAudited(t *testing.T) {
	s, _, au := newRBACServer(t)

	code, _ := doAs(t, s, "wrong-token-value-here", http.MethodGet, "/api/v1/devices", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", code)
	}
	evs := au.byAction("auth.unauthorized")
	if len(evs) != 1 {
		t.Fatalf("unauthorized audit events = %d, want 1", len(evs))
	}
	if evs[0].Result != "error" {
		t.Fatalf("result = %q, want error", evs[0].Result)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="uic"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

// TestForbiddenIsAuditedAsDenied 区分「未认证」（error）与「越权」（denied）：
// 只有后者才需要人去看，前者多半是扫描器。
func TestForbiddenIsAuditedAsDenied(t *testing.T) {
	s, tok, au := newRBACServer(t)

	code, _ := doAs(t, s, tok["viewer"], http.MethodPost, "/api/v1/points/d1/p1/write",
		`{"value":{"type":"float32","num":2}}`)
	if code != http.StatusForbidden {
		t.Fatalf("viewer write = %d", code)
	}
	evs := au.byAction("auth.forbidden")
	if len(evs) != 1 {
		t.Fatalf("forbidden events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Result != "denied" {
		t.Fatalf("result = %q, want denied", ev.Result)
	}
	if ev.User != "vic" || ev.Role != string(rbac.RoleViewer) {
		t.Fatalf("actor = %+v", ev)
	}
	if ev.Target != string(rbac.PermWrite) {
		t.Fatalf("target = %q, want %q", ev.Target, rbac.PermWrite)
	}
	if ev.RequestID == "" || ev.Remote == "" {
		t.Fatalf("request id / remote missing: %+v", ev)
	}
}

// TestPublicEndpointsNeedNoToken 健康探针与版本号必须不需要凭据。
func TestPublicEndpointsNeedNoToken(t *testing.T) {
	s, _, au := newRBACServer(t)
	for _, p := range []string{"/healthz", "/readyz", "/api/v1/version"} {
		if code, body := doAs(t, s, "", http.MethodGet, p, ""); code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p, code, body)
		}
	}
	// 未认证探测不能污染审计（否则一次端口扫描能写满审计日志）。
	if n := len(au.byAction("auth.unauthorized")); n != 0 {
		t.Fatalf("public endpoints produced %d unauthorized audit events", n)
	}
}

// TestMetricsPublicSemantics 保留 MetricsPublic：为 true 时免认证。
func TestMetricsPublicSemantics(t *testing.T) {
	st, err := auth.NewStore([]auth.User{{Name: "u", Role: rbac.RoleAdmin, Token: testToken('a')}}, "")
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := newServer(t, Options{Auth: auth.NewReloadable(st), MetricsPublic: true})
	if code, _ := doAs(t, pub, "", http.MethodGet, "/api/v1/metrics", ""); code != http.StatusOK {
		t.Fatalf("public metrics = %d, want 200", code)
	}

	priv, _ := newServer(t, Options{Auth: auth.NewReloadable(st), MetricsPublic: false})
	if code, _ := doAs(t, priv, "", http.MethodGet, "/api/v1/metrics", ""); code != http.StatusUnauthorized {
		t.Fatalf("private metrics = %d, want 401", code)
	}
}

// TestAuthMeInOpenMode 开放模式下 /auth/me 必须明确说出 open=true。
func TestAuthMeInOpenMode(t *testing.T) {
	st, err := auth.NewStore(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(t, Options{Auth: auth.NewReloadable(st)})

	code, body := doAs(t, s, "", http.MethodGet, "/api/v1/auth/me", "")
	if code != http.StatusOK {
		t.Fatalf("auth/me = %d %s", code, body)
	}
	var v struct {
		Open          bool     `json:"open"`
		Authenticated bool     `json:"authenticated"`
		User          string   `json:"user"`
		Role          string   `json:"role"`
		Permissions   []string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Open {
		t.Fatal("open mode not reported by /auth/me")
	}
	if v.Authenticated {
		t.Fatal("open mode claimed an authenticated identity")
	}
	if v.User != "anonymous" || v.Role != string(rbac.RoleAdmin) {
		t.Fatalf("open mode identity = %+v", v)
	}
	if len(v.Permissions) == 0 {
		t.Fatal("open mode has no permissions, but it is admin")
	}
}

// TestAuthMeDoesNotLeakUserList 是一条安全回归：公开端点不能列举账号。
func TestAuthMeDoesNotLeakUserList(t *testing.T) {
	s, tok, _ := newRBACServer(t)

	code, body := doAs(t, s, "", http.MethodGet, "/api/v1/auth/me", "")
	if code != http.StatusOK {
		t.Fatalf("auth/me = %d %s", code, body)
	}
	if strings.Contains(body, "vic") || strings.Contains(body, "root") || strings.Contains(body, "ops") {
		t.Fatalf("anonymous /auth/me leaked user names: %s", body)
	}
	var v struct {
		Open          bool `json:"open"`
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Open || v.Authenticated {
		t.Fatalf("anonymous /auth/me = %+v, want open=false authenticated=false", v)
	}

	// 带合法 Token 时必须说出自己的身份与权限。
	code, body = doAs(t, s, tok["operator"], http.MethodGet, "/api/v1/auth/me", "")
	if code != http.StatusOK {
		t.Fatalf("auth/me with token = %d %s", code, body)
	}
	if !strings.Contains(body, `"user":"ops"`) {
		t.Fatalf("auth/me did not identify the caller: %s", body)
	}
}

// TestActorFromContext 验证 handler 能从 ctx 拿到身份（审计与「谁确认了
// 报警」都依赖它）。
func TestActorFromContext(t *testing.T) {
	s, tok, _ := newRBACServer(t)
	var got Actor
	var ok bool
	h := s.guard(rbac.PermRead, func(w http.ResponseWriter, r *http.Request) {
		got, ok = ActorFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+tok["operator"])
	w := httptest.NewRecorder()
	h(w, req)

	if !ok {
		t.Fatal("actor missing from context")
	}
	if got.User != "ops" || got.Role != rbac.RoleOperator {
		t.Fatalf("actor = %+v", got)
	}
	// RequestID 由 ServeHTTP 注入；直接调 guard 时它来自 X-Request-Id 头，
	// 两者都没有就留空——不编造一个假 ID。
	req.Header.Set("X-Request-Id", "req-test-1")
	w2 := httptest.NewRecorder()
	h(w2, req)
	if got, _ := ActorFrom(req.Context()); got.RequestID != "" {
		t.Fatalf("ctx without RequestID should not invent one: %+v", got)
	}
	if got.Remote == "" {
		t.Fatalf("actor missing remote address: %+v", got)
	}

	// 没有身份时 ok 必须为 false，上游据此记 system。
	if _, ok := ActorFrom(context.Background()); ok {
		t.Fatal("ActorFrom reported an actor for an empty context")
	}
}

// TestAuditEndpointsShape 验证三个审计端点的响应结构。
func TestAuditEndpointsShape(t *testing.T) {
	s, tok, _ := newRBACServer(t)
	be := s.be.(*fakeBackend)
	be.auditLog = []audit.Record{
		{Seq: 1, User: "alice", Role: "operator", Action: "point.write", Target: "d1/p1", Result: "ok"},
		{Seq: 2, User: "vic", Role: "viewer", Action: "auth.forbidden", Result: "denied", Code: "auth.forbidden"},
	}

	code, body := doAs(t, s, tok["admin"], http.MethodGet, "/api/v1/audit?limit=10", "")
	if code != http.StatusOK {
		t.Fatalf("audit = %d %s", code, body)
	}
	var res struct {
		Items []struct {
			Seq    uint64 `json:"seq"`
			Time   string `json:"time"`
			User   string `json:"user"`
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"items"`
		Total   int `json:"total"`
		Scanned int `json:"scanned"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.Items[0].Seq != 2 {
		t.Fatalf("items = %+v (want newest first)", res.Items)
	}
	if res.Total != 2 {
		t.Fatalf("total = %d", res.Total)
	}

	code, body = doAs(t, s, tok["admin"], http.MethodGet, "/api/v1/audit/verify", "")
	if code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("verify = %d %s", code, body)
	}

	code, body = doAs(t, s, tok["admin"], http.MethodGet, "/api/v1/audit/stats", "")
	if code != http.StatusOK || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("stats = %d %s", code, body)
	}
}

// TestAuditListRejectsBadTime 验证非法时间参数是 400 而不是 500。
func TestAuditListRejectsBadTime(t *testing.T) {
	s, tok, _ := newRBACServer(t)
	for _, q := range []string{"from=yesterday", "to=2026-13-01", "limit=-1"} {
		code, body := doAs(t, s, tok["admin"], http.MethodGet, "/api/v1/audit?"+q, "")
		if code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", q, code, body)
		}
	}
}

// TestAuditDisabledReturnsConflict 验证审计关掉时端点给出可解释的 409。
func TestAuditDisabledReturnsConflict(t *testing.T) {
	st, err := auth.NewStore(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s, be := newServer(t, Options{Auth: auth.NewReloadable(st)})
	be.auditOff = true

	code, body := doAs(t, s, "", http.MethodGet, "/api/v1/audit", "")
	if code != http.StatusConflict {
		t.Fatalf("audit with disabled storage = %d %s, want 409", code, body)
	}
}
