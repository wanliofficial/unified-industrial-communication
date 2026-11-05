package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"uic/internal/alarm"
	"uic/internal/config"
	"uic/internal/northbound/httpapi"
)

// ---------------------------------------------------------------------------
// Backend: 配置
// ---------------------------------------------------------------------------

// Config 返回当前生效配置（凭据脱敏）。
func (r *Runtime) Config() (json.RawMessage, error) {
	canonical, err := r.cfg.Current().Canonical()
	if err != nil {
		return nil, httpapiInternal("encode config", err)
	}
	return redactJSON(canonical)
}

// ValidateConfig 只校验，不应用。
func (r *Runtime) ValidateConfig(raw json.RawMessage) error {
	var f config.File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return badConfig("cannot parse config: %v", err)
	}
	if err := f.Validate(); err != nil {
		return badConfig("%v", err)
	}
	return nil
}

// ConfigVersions 返回历史版本。
func (r *Runtime) ConfigVersions() []config.VersionInfo {
	return r.cfg.Versions()
}

// ConfigVersion 返回指定版本内容（脱敏）。
func (r *Runtime) ConfigVersion(id string) (json.RawMessage, error) {
	f, err := r.cfg.VersionConfig(id)
	if err != nil {
		return nil, httpapi.Errf(http.StatusNotFound, "config.version_not_found", "%v", err)
	}
	canonical, err := f.Canonical()
	if err != nil {
		return nil, httpapiInternal("encode config version", err)
	}
	return redactJSON(canonical)
}

// ConfigDiff 返回版本差异。
func (r *Runtime) ConfigDiff(from, to string) (config.Diff, error) {
	d, err := r.cfg.Diff(from, to)
	if err != nil {
		return config.Diff{}, httpapi.Errf(http.StatusNotFound, "config.version_not_found", "%v", err)
	}
	return d, nil
}

func badConfig(format string, args ...any) error {
	return httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid", format, args...)
}

func httpapiInternal(what string, err error) error {
	return httpapi.Errf(http.StatusInternalServerError, "internal.error", "%s: %v", what, err)
}

// redactJSON 把 token/password/secret 类字段替换为 ***，
// 并有选择地隐藏 webhook 自定义头的值（头名可能本身就是凭据）。
func redactJSON(raw []byte) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	v = redactValue(v, "")
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func redactValue(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "headers") {
				if hm, ok := val.(map[string]any); ok {
					for hk := range hm {
						hm[hk] = "***"
					}
					x[k] = hm
					continue
				}
			}
			x[k] = redactValue(val, k)
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactValue(x[i], key)
		}
		return x
	case string:
		if isSecretKey(key) {
			return "***"
		}
		return x
	default:
		return v
	}
}

func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"password", "token", "secret", "apikey", "api_key", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Backend: 命令（白名单）
// ---------------------------------------------------------------------------

// Command 执行一条白名单命令。未知动作一律拒绝，不做任何「猜测」。
func (r *Runtime) Command(ctx context.Context, action string, params map[string]any) (any, error) {
	switch action {
	case "driver.restart":
		id, err := strParam(params, "device")
		if err != nil {
			return nil, err
		}
		if _, ok := r.collector(id); !ok {
			return nil, httpapi.Errf(http.StatusNotFound, "device.not_found", "device %q not found", id)
		}
		if err := r.sched.Restart(id); err != nil {
			return nil, httpapi.Errf(http.StatusNotFound, "device.not_found", "%v", err)
		}
		gen, _ := r.sched.Generation(id)
		return map[string]any{"device": id, "generation": gen}, nil

	case "device.selftest":
		id, err := strParam(params, "device")
		if err != nil {
			return nil, err
		}
		c, ok := r.collector(id)
		if !ok {
			return nil, httpapi.Errf(http.StatusNotFound, "device.not_found", "device %q not found", id)
		}
		if err := r.sched.SubmitWrite(ctx, id, func(wctx context.Context) error {
			return c.probeNow(wctx)
		}); err != nil {
			return nil, httpapi.Errf(http.StatusServiceUnavailable, "device.selftest_failed", "%v", err)
		}
		return map[string]any{"device": id, "ok": true}, nil

	case "outbox.flush":
		if r.box == nil {
			return nil, httpapi.Errf(http.StatusConflict, "outbox.disabled", "no northbound sink configured")
		}
		if err := r.box.Flush(); err != nil {
			return nil, httpapi.Errf(http.StatusInternalServerError, "outbox.flush_failed", "%v", err)
		}
		r.wakeUp()
		return map[string]any{"flushed": true, "sinks": r.sinkNames}, nil

	case "outbox.drop":
		if r.box == nil {
			return nil, httpapi.Errf(http.StatusConflict, "outbox.disabled", "no northbound sink configured")
		}
		if !confirmed(params) {
			return nil, httpapi.Errf(http.StatusBadRequest, "request.confirm_required",
				"outbox.drop discards data; pass confirm: true to proceed")
		}
		sink, err := strParam(params, "sink")
		if err != nil {
			if len(r.sinkNames) == 1 {
				sink = r.sinkNames[0]
			} else {
				return nil, err
			}
		}
		before := time.Now()
		if v, ok := params["before"].(string); ok && v != "" {
			t, perr := time.Parse(time.RFC3339, v)
			if perr != nil {
				return nil, httpapi.Errf(http.StatusBadRequest, "request.invalid", "before must be RFC3339: %v", perr)
			}
			before = t
		}
		n := r.box.DropBefore(sink, before)
		r.wakeUp()
		return map[string]any{"sink": sink, "dropped": n, "before": before.UTC()}, nil

	case "config.rollback":
		if !confirmed(params) {
			return nil, httpapi.Errf(http.StatusBadRequest, "request.confirm_required",
				"config.rollback changes the running configuration; pass confirm: true")
		}
		version, err := strParam(params, "version")
		if err != nil {
			return nil, err
		}
		return r.Rollback(version)

	case "log.level":
		level, err := strParam(params, "level")
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(level) {
		case "debug", "info", "warn", "warning", "error":
		default:
			return nil, httpapi.Errf(http.StatusBadRequest, "request.invalid", "level must be debug/info/warn/error")
		}
		r.level.Set(level)
		return map[string]any{"level": r.level.Level().String()}, nil

	case "alarm.ack":
		by := ""
		if v, ok := params["by"].(string); ok {
			by = v
		}
		all := false
		if v, ok := params["all"].(bool); ok {
			all = v
		}
		ids, err := stringList(params["ids"])
		if err != nil {
			return nil, err
		}
		out, err := r.AckAlarms(ctx, ids, all, by)
		if err != nil {
			return nil, err
		}
		return map[string]any{"acknowledged": out, "total": len(out)}, nil

	case "diag.bundle":
		return map[string]any{
			"endpoint": "/api/v1/diagnostics/bundle",
			"hint":     "GET the endpoint to download a zip bundle",
		}, nil
	}
	return nil, httpapi.Errf(http.StatusBadRequest, "request.unknown_action", "unknown action %q", action)
}

func strParam(p map[string]any, key string) (string, error) {
	v, ok := p[key]
	if !ok {
		return "", httpapi.Errf(http.StatusBadRequest, "request.invalid", "param %q is required", key)
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", httpapi.Errf(http.StatusBadRequest, "request.invalid", "param %q must be a non-empty string", key)
	}
	return s, nil
}

// stringList 接受 JSON 数组（[]any）或单个字符串。
func stringList(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, httpapi.Errf(http.StatusBadRequest, "request.invalid",
					"ids must be a list of non-empty strings")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, httpapi.Errf(http.StatusBadRequest, "request.invalid", "ids must be a string or a list of strings")
}

func confirmed(p map[string]any) bool {
	for _, k := range []string{"_confirm", "confirm"} {
		if b, ok := p[k].(bool); ok && b {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Backend: 诊断包
// ---------------------------------------------------------------------------

// DiagnosticsBundle 生成一个内存 zip：清单、脱敏配置、指标、日志、健康与设备状态。
// 目标场景是「现场不能 SSH，只能让用户点一个按钮把包发回来」。
func (r *Runtime) DiagnosticsBundle(_ context.Context) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	add := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	manifest := map[string]any{
		"product":       "uic",
		"version":       r.version.Version,
		"commit":        r.version.Commit,
		"buildTime":     r.version.BuildTime,
		"generatedAt":   time.Now().UTC(),
		"site":          r.cfg.Current().Site,
		"configVersion": r.cfg.Version(),
		"uptimeSeconds": int64(time.Since(r.startedAt).Seconds()),
		"apiVersion":    1,
		"schemaVersion": config.SchemaVersion,
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := add("manifest.json", mb); err != nil {
		return nil, err
	}

	cfg, err := r.Config()
	if err != nil {
		return nil, err
	}
	if err := add("config.redacted.json", cfg); err != nil {
		return nil, err
	}
	if err := add("metrics.txt", []byte(r.Metrics())); err != nil {
		return nil, err
	}
	logs, _ := json.MarshalIndent(r.ring.Query("", "", 5000), "", "  ")
	if err := add("logs/uic.json", logs); err != nil {
		return nil, err
	}
	health, _ := json.MarshalIndent(r.Health(), "", "  ")
	if err := add("health.json", health); err != nil {
		return nil, err
	}
	devices, _ := json.MarshalIndent(r.Devices(""), "", "  ")
	if err := add("devices.json", devices); err != nil {
		return nil, err
	}
	if r.box != nil {
		stats, _ := json.MarshalIndent(r.box.Stats(), "", "  ")
		if err := add("outbox.json", stats); err != nil {
			return nil, err
		}
	}
	if r.alarms != nil {
		// 规则与状态一起导出：只有状态看不出「为什么没报警」。
		alarms, _ := json.MarshalIndent(map[string]any{
			"rules":  r.alarms.Rules(),
			"status": r.alarms.List(alarm.Filter{}),
			"counts": r.alarms.Counts(),
		}, "", "  ")
		if err := add("alarms.json", alarms); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
