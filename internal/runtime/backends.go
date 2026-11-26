package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"uic/internal/alarm"
	"uic/internal/config"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/probe"
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
	filled, err := r.fillRedacted(raw)
	if err != nil {
		return err
	}
	var f config.File
	dec := json.NewDecoder(bytes.NewReader(filled))
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

// redactedValue 是 GET /config 用来替换凭据的字面量。apply 时必须把它
// 还原回当前配置里的真实值。
const redactedValue = "***"

// fillRedacted 把提交配置里的 *** 占位符还原成当前配置里的对应值。
//
// 为什么必须做：GET /api/v1/config 会脱敏 token/password/secret 类字段，
// 而最常见、也最安全的运维路径正是「读出配置 → 只改一个点位 → 写回去」。
// 如果 apply 不接受占位符，这条路会死在
// `server.users[0].tokenSha256 must be 64 hex characters` 上，运维只能整份
// 重写配置——那才是真正容易把凭据改坏的场景。
//
// 语义：*** 表示「保持当前值不变」。想清空某个密码，就显式写空字符串或
// 删掉该字段，不能靠 ***。
func (r *Runtime) fillRedacted(raw json.RawMessage) (json.RawMessage, error) {
	if !bytes.Contains(raw, []byte(redactedValue)) {
		return raw, nil
	}
	canonical, err := r.cfg.Current().Canonical()
	if err != nil {
		return nil, httpapiInternal("encode current config", err)
	}
	var sub, cur any
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, badConfig("cannot parse config: %v", err)
	}
	if err := json.Unmarshal(canonical, &cur); err != nil {
		return nil, httpapiInternal("parse current config", err)
	}
	resolved, err := resolvePlaceholders(sub, cur, "")
	if err != nil {
		return nil, badConfig("%v", err)
	}
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, badConfig("cannot encode config: %v", err)
	}
	return out, nil
}

// resolvePlaceholders 按相同路径把 *** 换成 cur 里的值。
//
// 数组元素优先按 id/name 匹配而不是按下标：运维调整 users 顺序或删掉中间
// 一项时，按下标会把 A 的密码安到 B 头上——这类错误不会报错，只会让人
// 在几天后发现「某个账号登录不上、另一个账号拿到了不该有的权限」。
func resolvePlaceholders(sub, cur any, path string) (any, error) {
	switch x := sub.(type) {
	case string:
		if x != redactedValue {
			return x, nil
		}
		if s, ok := cur.(string); ok {
			return s, nil
		}
		return nil, fmt.Errorf("config: %s is a redacted placeholder but has no current value", pathOrRoot(path))
	case map[string]any:
		curMap, _ := cur.(map[string]any)
		out := make(map[string]any, len(x))
		for k, v := range x {
			child := k
			if path != "" {
				child = path + "." + k
			}
			cv, ok := curMap[k]
			if !ok {
				if containsPlaceholder(v) {
					return nil, fmt.Errorf("config: %s is a redacted placeholder but the field does not exist", child)
				}
				out[k] = v
				continue
			}
			rv, err := resolvePlaceholders(v, cv, child)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		curArr, _ := cur.([]any)
		out := make([]any, len(x))
		for i, v := range x {
			idx := i
			if key, val, ok := matchKey(v); ok {
				idx = -1
				for j, c := range curArr {
					if k2, v2, ok2 := matchKey(c); ok2 && k2 == key && v2 == val {
						idx = j
						break
					}
				}
			}
			child := fmt.Sprintf("%s[%d]", path, i)
			if idx < 0 || idx >= len(curArr) {
				if containsPlaceholder(v) {
					return nil, fmt.Errorf("config: %s is a redacted placeholder but no matching element exists", child)
				}
				out[i] = v
				continue
			}
			rv, err := resolvePlaceholders(v, curArr[idx], child)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return sub, nil
	}
}

// matchKey 返回数组元素用于匹配的键（id 或 name）与值。
func matchKey(v any) (string, string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", "", false
	}
	for _, k := range []string{"id", "name"} {
		if s, ok := m[k].(string); ok && s != "" {
			return k, s, true
		}
	}
	return "", "", false
}

// containsPlaceholder 判断子树里是否还有 ***（用于「新增字段里带占位符」
// 这种无法还原的情况）。
func containsPlaceholder(v any) bool {
	switch x := v.(type) {
	case string:
		return x == redactedValue
	case map[string]any:
		for _, vv := range x {
			if containsPlaceholder(vv) {
				return true
			}
		}
	case []any:
		for _, vv := range x {
			if containsPlaceholder(vv) {
				return true
			}
		}
	}
	return false
}

func pathOrRoot(p string) string {
	if p == "" {
		return "config"
	}
	return p
}

// isSecretKey 判断一个配置字段名是否看起来像凭据。委托给 driver.LooksSecret，
// 它是脱敏与一致性套件共用的单一事实源（覆盖 community/privatekey 等不
// 含 password/token 字样的凭据名）。
func isSecretKey(k string) bool {
	return driver.LooksSecret(k)
}

// ---------------------------------------------------------------------------
// Backend: 命令（白名单）
// ---------------------------------------------------------------------------

// Command 执行一条白名单命令。未知动作一律拒绝，不做任何「猜测」。
// command 是命令白名单的实际实现。导出的 Command 在它外面包一层审计。
func (r *Runtime) command(ctx context.Context, action string, params map[string]any) (any, error) {
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
		before := time.Now()
		if v, ok := params["before"].(string); ok && v != "" {
			t, perr := time.Parse(time.RFC3339, v)
			if perr != nil {
				return nil, httpapi.Errf(http.StatusBadRequest, "request.invalid", "before must be RFC3339: %v", perr)
			}
			before = t
		}
		// 显式指定 sink：只丢那一条 lane。未指定：丢所有 lane（含 v2 lane）——
		// 双发后每个传输都有两条 lane，运维说「清队列」时指的就是全部，不该
		// 因为 lane 数变多就被迫补一个 sink 参数。返回按 lane 拆分的明细。
		if sink, err := strParam(params, "sink"); err == nil {
			n := r.box.DropBefore(sink, before)
			r.wakeUp()
			return map[string]any{"sink": sink, "dropped": n, "before": before.UTC()}, nil
		}
		dropped := make(map[string]int64, len(r.sinkNames))
		var total int64
		for _, name := range r.sinkNames {
			n := r.box.DropBefore(name, before)
			dropped[name] = n
			total += n
		}
		r.wakeUp()
		return map[string]any{"sinks": dropped, "dropped": total, "before": before.UTC()}, nil

	case "config.rollback":
		if !confirmed(params) {
			return nil, httpapi.Errf(http.StatusBadRequest, "request.confirm_required",
				"config.rollback changes the running configuration; pass confirm: true")
		}
		version, err := strParam(params, "version")
		if err != nil {
			return nil, err
		}
		return r.Rollback(ctx, version)

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
		// instanceId 让运维把一份诊断包对应到具体实例：同站点多实例部署时，
		// 仅凭 site 无法区分，而它与 MQTT clientID/Sparkplug nodeId 同源。
		"instanceId": r.instanceID,
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
	if r.hist != nil {
		// 水位 + 策略一起导出：只看字节数无法判断「为什么只存了 3 天」。
		hist, _ := json.MarshalIndent(map[string]any{
			"stats":  r.hist.Stats(),
			"policy": r.cfg.Current().Storage.History,
			"dir":    r.hist.Dir(),
		}, "", "  ")
		if err := add("history.json", hist); err != nil {
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

// ---------------------------------------------------------------------------
// Backend: 网络探测
// ---------------------------------------------------------------------------

// Probe 对当前配置里的网络端点做连通性探测，回答「为什么连不上」的第一个分叉：
// TCP 连不上 → 路由/端口/防火墙；TCP 连得上但驱动读不到 → 协议参数或设备应用层。
//
// 只接受配置内已声明的通道/设备（r.cfg.Current()），不接受任意主机，因此不存在
// SSRF 面。q.Channels 与 q.Devices 都为空时探测全部通道；指定时把设备解析到其
// 通道、与显式通道合并去重后探测，且只要有一个 id 在配置里不存在就整体报 400，
// 避免调用方以为「探测了」实际漏掉。
//
// 返回类型用 httpapi 的 DTO（与 Runtime 其它 Backend 方法一致，北向契约的
// 对外表示归 httpapi 所有）；内部探测仍走 internal/probe 包。
//
// 与驱动层 Conn.Probe 的分工：Conn.Probe 走真实协议，本方法只做 TCP 握手或
// UDP 地址解析、不发任何协议字节。探测是显式的、由运维触发的诊断动作。
func (r *Runtime) Probe(ctx context.Context, q httpapi.ProbeQuery) (httpapi.ProbeReport, error) {
	cfg := r.cfg.Current()
	selected, err := resolveProbeTargets(cfg.Channels, cfg.Devices, q)
	if err != nil {
		return httpapi.ProbeReport{}, err
	}
	results := probe.Probe(ctx, selected, probe.Options{Timeout: q.Timeout})
	return httpapi.ProbeReport{
		GeneratedAt: time.Now().UTC(),
		Total:       len(results),
		Results:     toProbeResults(results),
		Summary:     summarizeProbe(results),
	}, nil
}

// toProbeResults 把内部探测结果映射成 httpapi DTO。映射在这里集中，是为了让
// 内部 probe 包的演进不泄漏到北向 API。
func toProbeResults(in []probe.Result) []httpapi.ProbeResult {
	out := make([]httpapi.ProbeResult, len(in))
	for i, r := range in {
		out[i] = httpapi.ProbeResult{
			Channel:   r.Channel,
			Protocol:  r.Protocol,
			Transport: r.Transport,
			Address:   r.Address,
			Status:    r.Status,
			Reachable: r.Reachable,
			RTTMs:     r.RTTMs,
			Error:     r.Error,
		}
	}
	return out
}

// resolveProbeTargets 把探测请求解析成要探测的通道列表。
//
//   - q.Channels 与 q.Devices 都为空 → 全部通道。
//   - q.Devices 中的每个设备映射到它的 ChannelID（网络可达性属于通道）；同一
//     条线上的多台设备去重后只探一次。
//   - q.Channels 与解析出的通道合并去重。
//   - 任何 channel/device id 在配置里不存在 → 400，而不是静默少探：静默漏探
//     会让「都正常」的假象掩盖真实故障。
//
// 返回顺序遵循配置里 channels 的声明顺序，便于调用方对照。
func resolveProbeTargets(channels []model.Channel, devices []model.Device, q httpapi.ProbeQuery) ([]model.Channel, error) {
	// 全部通道：两个过滤条件都未指定。
	if len(q.Channels) == 0 && len(q.Devices) == 0 {
		return channels, nil
	}

	channelKnown := make(map[string]bool, len(channels))
	for _, ch := range channels {
		channelKnown[ch.ID] = true
	}
	deviceChannel := make(map[string]string, len(devices))
	for _, d := range devices {
		deviceChannel[d.ID] = d.ChannelID
	}

	want := make(map[string]bool, len(q.Channels)+len(q.Devices))
	var missing []string
	for _, id := range q.Channels {
		if !channelKnown[id] {
			missing = append(missing, "channel:"+id)
			continue
		}
		want[id] = true
	}
	for _, id := range q.Devices {
		chID, ok := deviceChannel[id]
		if !ok {
			missing = append(missing, "device:"+id)
			continue
		}
		want[chID] = true
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, httpapi.Errf(http.StatusBadRequest, "probe.unknown_channel",
			"not found in current config: %s", strings.Join(missing, ", "))
	}

	out := make([]model.Channel, 0, len(want))
	for _, ch := range channels {
		if want[ch.ID] {
			out = append(out, ch)
		}
	}
	return out, nil
}

// summarizeProbe 统计各状态计数，让看板与「概览告警」不必再遍历 Results。
func summarizeProbe(results []probe.Result) httpapi.ProbeSummary {
	var s httpapi.ProbeSummary
	for _, r := range results {
		switch r.Status {
		case probe.StatusOK:
			s.OK++
		case probe.StatusFail:
			s.Fail++
		case probe.StatusUnverified:
			s.Unverified++
		case probe.StatusUnsupported:
			s.Unsupported++
		}
	}
	return s
}
