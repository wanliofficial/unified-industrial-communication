package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// randomToken 生成一个满足 auth.ValidateToken 的随机凭据。
func randomToken(t *testing.T) string {
	t.Helper()
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// newAuthHarness 起一个带多身份凭据表的 runtime。
//
// actor 参数给每个测试一个独立身份（否则凭据表会互相干扰），角色由调用方在
// users 里指定。返回 harness 与三个 Token：一个 viewer、一个 operator、一个
// admin——RBAC 的越权断言需要「有身份但权限不够」这一档，光有 admin 测不出来。
func newAuthHarness(t *testing.T, mutate func(cfg map[string]any)) (*harness, map[string]string) {
	t.Helper()
	tokens := map[string]string{
		"viewer":   randomToken(t),
		"operator": randomToken(t),
		"admin":    randomToken(t),
	}
	h := newHarness(t, func(cfg map[string]any) {
		cfg["server"] = map[string]any{"listen": "127.0.0.1:0", "users": []any{
			map[string]any{"name": "vic", "role": "viewer", "token": tokens["viewer"]},
			map[string]any{"name": "ops", "role": "operator", "token": tokens["operator"]},
			map[string]any{"name": "root", "role": "admin", "token": tokens["admin"]},
		}}
		if mutate != nil {
			mutate(cfg)
		}
	})
	h.actor = tokens["admin"]
	return h, tokens
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

func TestAuthRejectsBadToken(t *testing.T) {
	good := randomToken(t)
	h, _ := newAuthHarness(t, func(cfg map[string]any) {
		cfg["server"] = map[string]any{"listen": "127.0.0.1:0", "users": []any{
			map[string]any{"name": "alice", "role": "operator", "token": good},
		}}
	})

	if code, _ := h.do(http.MethodGet, "/api/v1/points", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
	if code, _ := h.do(http.MethodGet, "/api/v1/points", ""); code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", code)
	}

	// 正确 Token 必须能用：这是唯一能证明「不是简单拒绝一切」的断言。
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/points", nil)
	req.Header.Set("Authorization", "Bearer "+good)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good token = %d, want 200", resp.StatusCode)
	}
}

func TestAuthMeAndUsers(t *testing.T) {
	h, toks := newAuthHarness(t, nil)

	h.actor = toks["admin"]
	code, body := h.do(http.MethodGet, "/api/v1/auth/me", "")
	if code != http.StatusOK {
		t.Fatalf("auth/me = %d %s", code, body)
	}
	// /auth/me 回答的是「我是谁」，不是「有谁」。管理员用它确认自己的
	// 角色；任何匿名请求都不该从它拿到账号清单。
	var me struct {
		User          string   `json:"user"`
		Role          string   `json:"role"`
		Open          bool     `json:"open"`
		Authenticated bool     `json:"authenticated"`
		Permissions   []string `json:"permissions"`
	}
	decode(t, body, &me)
	if me.Open {
		t.Fatal("open = true, want false")
	}
	if me.User != "root" || me.Role != "admin" {
		t.Fatalf("me = %+v", me)
	}
	if !me.Authenticated {
		t.Fatal("authenticated = false for a valid token")
	}
	if len(me.Permissions) == 0 {
		t.Fatalf("no permissions reported: %+v", me)
	}

	code, body = h.do(http.MethodGet, "/api/v1/auth/users", "")
	if code != http.StatusOK {
		t.Fatalf("auth/users = %d %s", code, body)
	}
	var users struct {
		Items []map[string]any `json:"items"`
		Open  bool             `json:"open"`
		Count int              `json:"count"`
	}
	decode(t, body, &users)
	if len(users.Items) != 3 || users.Count != 3 {
		t.Fatalf("users = %+v", users)
	}
	if users.Open {
		t.Fatal("open = true, want false")
	}
	// 明文凭据绝不外泄：/auth/users 只返回用户名与角色。
	for _, u := range users.Items {
		if _, ok := u["token"]; ok {
			t.Fatalf("auth/users leaked a token field: %+v", u)
		}
		if _, ok := u["tokenSha256"]; ok {
			t.Fatalf("auth/users leaked a hash field: %+v", u)
		}
	}
}

// TestAuthUsersRequiresAuditRead 证明 auth/users 不是随便什么身份都能看的清单：
// 它列出全部账号，属于配置拓扑，不给 viewer。
func TestAuthUsersRequiresAuditRead(t *testing.T) {
	h, toks := newAuthHarness(t, nil)

	h.actor = toks["viewer"]
	if code, _ := h.do(http.MethodGet, "/api/v1/auth/users", ""); code != http.StatusForbidden {
		t.Fatalf("viewer auth/users = %d, want 403", code)
	}
	h.actor = toks["operator"]
	if code, _ := h.do(http.MethodGet, "/api/v1/auth/users", ""); code != http.StatusForbidden {
		t.Fatalf("operator auth/users = %d, want 403", code)
	}
	h.actor = toks["admin"]
	if code, _ := h.do(http.MethodGet, "/api/v1/auth/users", ""); code != http.StatusOK {
		t.Fatalf("admin auth/users = %d, want 200", code)
	}
}

// TestAuthAcceptsSha256OnlyCredential 证明生产配置（只写哈希）也能登录。
func TestAuthAcceptsSha256OnlyCredential(t *testing.T) {
	plain := randomToken(t)
	sum := sha256.Sum256([]byte(plain))
	h, _ := newAuthHarness(t, func(cfg map[string]any) {
		cfg["server"] = map[string]any{"listen": "127.0.0.1:0", "users": []any{
			map[string]any{"name": "svc", "role": "viewer", "tokenSha256": hex.EncodeToString(sum[:])},
		}}
	})

	h.actor = plain
	if code, body := h.do(http.MethodGet, "/api/v1/points", ""); code != http.StatusOK {
		t.Fatalf("sha256-only token = %d %s", code, body)
	}
}

// TestRemovingUserTakesEffectImmediately 是 RBAC 与审计同批交付的核心理由：
// 被删掉的人必须立刻失去写能力，不等重启。「已经删掉的人还能继续写点位」是
// 审计里最没法解释的一种记录。
func TestRemovingUserTakesEffectImmediately(t *testing.T) {
	tokens := map[string]string{"viewer": randomToken(t), "admin": randomToken(t)}
	dir := t.TempDir()
	base := map[string]any{
		"schemaVersion": 1, "site": "test-site",
		"storage": map[string]any{"dir": dir, "journal": true, "outboxMaxRecords": 1000},
		"logging": map[string]any{"level": "info"},
		"channels": []any{map[string]any{
			"id": "ch-modbus", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": "127.0.0.1:1"},
			"concurrency": 1, "timeout": "1s",
		}},
		"devices": []any{map[string]any{
			"id": "dev1", "name": "d", "channel": "ch-modbus",
			"unitId": 1, "interval": "1s", "timeout": "1s", "enabled": true,
			"points": []any{map[string]any{
				"id": "reg0", "dataType": "uint16", "enabled": true, "writable": true,
				"address": map[string]any{"kind": "register", "area": "holding", "offset": 0, "length": 1, "bit": -1},
			}},
		}},
	}
	users := func() []any {
		return []any{
			map[string]any{"name": "vic", "role": "viewer", "token": tokens["viewer"]},
			map[string]any{"name": "root", "role": "admin", "token": tokens["admin"]},
		}
	}
	cfg0 := map[string]any{}
	for k, v := range base {
		cfg0[k] = v
	}
	cfg0["server"] = map[string]any{"listen": "127.0.0.1:0", "users": users()}
	writeConfig(t, dir, cfg0)

	rt, err := New(Options{ConfigPath: filepath.Join(dir, "config.json"), DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	h := &harness{t: t, rt: rt, dir: dir, actor: tokens["viewer"]}
	h.srv = httptest.NewServer(rt.Handler())
	t.Cleanup(func() {
		h.srv.Close()
		cancel()
		_ = rt.Stop(5 * time.Second)
	})

	if code, _ := h.do(http.MethodGet, "/api/v1/points", ""); code != http.StatusOK {
		t.Fatalf("viewer read = %d, want 200", code)
	}
	if code, _ := h.do(http.MethodPost, "/api/v1/points/dev1/write",
		`{"writes":[{"point":"reg0","value":{"type":"uint16","num":7}}]}`); code != http.StatusForbidden {
		t.Fatalf("viewer write = %d, want 403", code)
	}

	// 应用一份删掉 viewer 的配置。
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var cur map[string]any
	decode(t, raw, &cur)
	cur["server"] = map[string]any{"listen": "127.0.0.1:0", "users": []any{
		map[string]any{"name": "root", "role": "admin", "token": tokens["admin"]},
	}}
	if _, err := rt.ApplyConfig(context.Background(), mustJSON(t, cur), "test"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if code, _ := h.do(http.MethodGet, "/api/v1/points", ""); code != http.StatusUnauthorized {
		t.Fatalf("after removal = %d, want 401", code)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(bytes.TrimSpace(b), v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}
