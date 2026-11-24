package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestConfigRoundTripKeepsSecrets 是最常见的运维路径：
// GET /config → 改一个点位 → POST /config/apply。
//
// 脱敏把 server.users[].tokenSha256 换成 ***；如果 apply 不还原占位符，
// 这条路径会因为「哈希必须是 64 位十六进制」直接失败，而运维的被修复方式
// 往往是「把整份配置重写一遍」——那才是真正容易改坏凭据的动作。
func TestConfigRoundTripKeepsSecrets(t *testing.T) {
	h, toks := newAuthHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/config", "")
	if code != http.StatusOK {
		t.Fatalf("get config = %d %s", code, body)
	}
	if !strings.Contains(string(body), `"***"`) {
		t.Fatalf("GET /config did not redact credentials:\n%s", body)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	// 只改一个无关字段，其余原样写回。
	cfg["logging"] = map[string]any{"level": "debug"}

	code, body = h.do(http.MethodPost, "/api/v1/config/apply", string(mustJSON(t, cfg)))
	if code != http.StatusOK {
		t.Fatalf("apply round-tripped config = %d %s", code, body)
	}

	// 凭据必须还在，而且原来的 Token 仍然能登录。
	for _, name := range []string{"viewer", "operator", "admin"} {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/points", nil)
		req.Header.Set("Authorization", "Bearer "+toks[name])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s token rejected after round trip: %d", name, resp.StatusCode)
		}
	}
}

// TestConfigApplyRejectsUnknownPlaceholder 验证「***」不能被凭空塞进一个
// 当前配置里不存在的字段：那说明调用方在猜值，而不是在「保持不变」。
func TestConfigApplyRejectsUnknownPlaceholder(t *testing.T) {
	h, _ := newAuthHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/config", "")
	if code != http.StatusOK {
		t.Fatalf("get config = %d", code)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["northbound"] = map[string]any{
		"mqtt": map[string]any{"enabled": false, "password": "***"},
	}

	code, body = h.do(http.MethodPost, "/api/v1/config/apply", string(mustJSON(t, cfg)))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("apply with fabricated placeholder = %d %s, want 422", code, body)
	}
	if !strings.Contains(string(body), "redacted placeholder") {
		t.Fatalf("body = %s", body)
	}
}

// TestConfigValidateAcceptsRedacted 验证「先 validate 再 apply」这条路径
// 也认占位符，否则运维会在校验阶段被误导成「配置本身有语法错误」。
func TestConfigValidateAcceptsRedacted(t *testing.T) {
	h, _ := newAuthHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/config", "")
	if code != http.StatusOK {
		t.Fatalf("get config = %d", code)
	}
	code, body = h.do(http.MethodPost, "/api/v1/config/validate", string(body))
	if code != http.StatusOK {
		t.Fatalf("validate round-tripped config = %d %s", code, body)
	}
}

// TestRedactedArrayMatchesByID 验证数组按 id 而不是下标还原。
//
// 下标匹配在「删掉中间一个用户」时会把 B 的凭据安到 C 头上——不报错，
// 只是几天后有人发现自己能登进别人的账号。
func TestRedactedArrayMatchesByID(t *testing.T) {
	h, toks := newAuthHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/config", "")
	if code != http.StatusOK {
		t.Fatalf("get config = %d", code)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	server := cfg["server"].(map[string]any)
	users := server["users"].([]any)

	// 删掉 viewer，并把剩下的顺序颠倒：ops/admin 的 *** 必须按 name 找回去。
	kept := make([]any, 0, len(users))
	for _, u := range users {
		if u.(map[string]any)["name"] != "vic" {
			kept = append(kept, u)
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	server["users"] = kept

	code, body = h.do(http.MethodPost, "/api/v1/config/apply", string(mustJSON(t, cfg)))
	if code != http.StatusOK {
		t.Fatalf("apply reordered users = %d %s", code, body)
	}

	// ops 仍然是 operator，admin 仍然是 admin（而不是互换）。
	code, body = h.do(http.MethodGet, "/api/v1/auth/me", "")
	if code != http.StatusOK {
		t.Fatalf("me = %d %s", code, body)
	}

	h.actor = toks["operator"]
	code, body = h.do(http.MethodGet, "/api/v1/auth/me", "")
	var me struct {
		User string `json:"user"`
		Role string `json:"role"`
	}
	decode(t, body, &me)
	if code != http.StatusOK || me.User != "ops" || me.Role != "operator" {
		t.Fatalf("ops identity = %d %+v", code, me)
	}

	// 被删掉的 viewer 立刻失效。
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/points", nil)
	req.Header.Set("Authorization", "Bearer "+toks["viewer"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed user still works: %d", resp.StatusCode)
	}
}

// TestConfigApplyWithoutPlaceholderStillWorks 保证普通提交（不带 ***）
// 不受还原逻辑影响。
func TestConfigApplyWithoutPlaceholderStillWorks(t *testing.T) {
	h, _ := newAuthHarness(t, nil)

	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"site":          "test-site",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": t.TempDir(), "journal": false},
		"logging":       map[string]any{"level": "warn"},
		"channels": []any{map[string]any{
			"id": "ch-v", "protocol": "virtual",
			"transport": map[string]any{"kind": "memory"}, "concurrency": 1, "timeout": "1s",
		}},
		"devices": []any{map[string]any{
			"id": "v1", "name": "v", "channel": "ch-v",
			"interval": "200ms", "timeout": "1s", "enabled": true,
			"labels": map[string]any{"mode": "sine"},
			"points": []any{map[string]any{"id": "sine", "dataType": "float32", "enabled": true,
				"address": map[string]any{"kind": "register", "area": "holding", "offset": 0, "length": 2, "bit": -1}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(raw))
	if code != http.StatusOK {
		t.Fatalf("apply = %d %s", code, body)
	}
}

// TestRedactJSONMasksSNMPCredentials 钉住「凭据声明的单一事实源」。
//
// SNMP 的 labels.community/authPassword/privPassword 都是实打实的口令，但
// 它们不含 password/token 字样（community 尤其如此）。脱敏若只按字段名子串
// 猜，团体名会在 GET /api/v1/config 里明文回显——这是真实发生过的泄漏形态。
// 这里用 redactJSON 直接验证：社区名与两个 v3 口令都必须变成 ***，
// 而 labels.version / labels.username 这类非凭据字段必须原样保留。
func TestRedactJSONMasksSNMPCredentials(t *testing.T) {
	raw := []byte(`{
	  "channels": [{"id": "ch-snmp", "protocol": "snmp"}],
	  "devices": [{
	    "id": "switch01",
	    "labels": {
	      "community": "public-secret",
	      "authPassword": "auth-secret",
	      "privPassword": "priv-secret",
	      "version": "3",
	      "username": "uic-ops"
	    }
	  }]
	}`)
	out, err := redactJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, leak := range []string{"public-secret", "auth-secret", "priv-secret"} {
		if strings.Contains(s, leak) {
			t.Fatalf("凭据 %q 未脱敏：\n%s", leak, s)
		}
	}
	// 非凭据字段必须保留，否则运维看不清自己配的是什么。
	for _, keep := range []string{`"community": "***"`, `"authPassword": "***"`, `"privPassword": "***"`,
		`"username": "uic-ops"`} {
		if !strings.Contains(s, keep) {
			t.Fatalf("期望保留 %s，实得：\n%s", keep, s)
		}
	}
}

// TestResolvePlaceholdersRestoresCommunity 验证往返：脱敏后的 *** 在原路径
// 上被还原成当前值。社区名与 v3 口令都必须能「GET 后原样写回」而不丢。
func TestResolvePlaceholdersRestoresCommunity(t *testing.T) {
	cur := map[string]any{
		"devices": []any{map[string]any{
			"id": "switch01",
			"labels": map[string]any{
				"community":    "public-secret",
				"authPassword": "auth-secret",
				"privPassword": "priv-secret",
			},
		}},
	}
	sub := map[string]any{
		"devices": []any{map[string]any{
			"id": "switch01",
			"labels": map[string]any{
				"community":    "***",
				"authPassword": "***",
				"privPassword": "***",
			},
		}},
	}
	got, err := resolvePlaceholders(sub, cur, "")
	if err != nil {
		t.Fatal(err)
	}
	g := got.(map[string]any)["devices"].([]any)[0].(map[string]any)["labels"].(map[string]any)
	for key, want := range map[string]string{
		"community": "public-secret", "authPassword": "auth-secret", "privPassword": "priv-secret",
	} {
		if g[key] != want {
			t.Fatalf("%s = %v，want %q", key, g[key], want)
		}
	}
}
