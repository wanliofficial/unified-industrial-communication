package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/audit"
	"uic/internal/model"
	slavemb "uic/internal/slave/modbus"
)

// auditItems 读回审计日志（倒序）。
func auditItems(t *testing.T, rt *Runtime, limit int) []audit.Record {
	t.Helper()
	res, err := rt.AuditQuery(audit.Filter{Limit: limit})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	return res.Items
}

func findAction(items []audit.Record, action string) []audit.Record {
	var out []audit.Record
	for _, it := range items {
		if it.Action == action {
			out = append(out, it)
		}
	}
	return out
}

// TestAuditRecordsWrites 是审计最核心的一条：一次写入必须留下「谁、哪个点、
// 旧值、新值、结果、来源」。
func TestAuditRecordsWrites(t *testing.T) {
	h, toks := newAuthHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	h.actor = toks["operator"]
	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":4321}}]}`)
	if code != http.StatusOK {
		t.Fatalf("write = %d %s", code, body)
	}

	items := auditItems(t, h.rt, 50)
	writes := findAction(items, "point.write")
	if len(writes) == 0 {
		t.Fatal("write left no audit record")
	}
	rec := writes[0]
	if rec.User != "ops" || rec.Role != "operator" {
		t.Fatalf("audit user/role = %q/%q", rec.User, rec.Role)
	}
	if rec.Target != "sim01/write_me" {
		t.Fatalf("target = %q", rec.Target)
	}
	if rec.Result != audit.ResultOK {
		t.Fatalf("result = %q", rec.Result)
	}
	if rec.Detail["value"] == "" {
		t.Fatalf("no value in detail: %+v", rec.Detail)
	}
	if rec.Detail["via"] != "api" {
		t.Fatalf("via = %q, want api", rec.Detail["via"])
	}
	if rec.RequestID == "" {
		t.Fatal("audit record has no request id; correlating with access logs becomes impossible")
	}
}

// TestAuditRecordsFailedWrite 验证被拒绝的写入也留痕（失败才是要查的）。
func TestAuditRecordsFailedWrite(t *testing.T) {
	h, toks := newAuthHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	h.actor = toks["operator"]
	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"nope","value":{"type":"uint16","num":1}}]}`)
	if code != http.StatusOK {
		t.Fatalf("write = %d %s", code, body)
	}

	writes := findAction(auditItems(t, h.rt, 50), "point.write")
	if len(writes) == 0 {
		t.Fatal("failed write left no audit record")
	}
	if writes[0].Result != audit.ResultError || writes[0].Code != "point.not_found" {
		t.Fatalf("record = %+v", writes[0])
	}
}

// TestAuditRecordsCommands 验证每条命令都留痕（含未知命令）。
func TestAuditRecordsCommands(t *testing.T) {
	h, toks := newAuthHarness(t, nil)
	h.actor = toks["admin"]

	if code, body := h.do(http.MethodPost, "/api/v1/commands", `{"action":"outbox.flush"}`); code != http.StatusOK {
		t.Fatalf("flush = %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/api/v1/commands", `{"action":"no.such.thing"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown = %d %s", code, body)
	}

	items := auditItems(t, h.rt, 50)
	if len(findAction(items, "command.outbox.flush")) != 1 {
		t.Fatalf("flush command not audited: %+v", findAction(items, "command.outbox.flush"))
	}
	unknown := findAction(items, "command.no.such.thing")
	if len(unknown) != 1 {
		t.Fatal("unknown command not audited")
	}
	if unknown[0].Result != audit.ResultError || unknown[0].Code != "request.unknown_action" {
		t.Fatalf("unknown command record = %+v", unknown[0])
	}
}

// TestAuditRecordsAlarmAck 验证确认动作记录「谁按下了确认」。
func TestAuditRecordsAlarmAck(t *testing.T) {
	h, toks := newAuthHarness(t, func(cfg map[string]any) {
		cfg["alarms"] = []any{map[string]any{
			"id": "high-power", "name": "power high", "point": "sim01:reg0",
			"high": 100, "hysteresis": 5, "severity": "critical",
		}}
	})
	h.waitPointGood("sim01", "reg0")

	h.actor = toks["operator"]
	if code, body := h.do(http.MethodPost, "/api/v1/alarms/ack", `{"all":true,"by":"alice"}`); code != http.StatusOK {
		t.Fatalf("ack = %d %s", code, body)
	}

	acks := findAction(auditItems(t, h.rt, 50), "alarm.ack")
	if len(acks) == 0 {
		t.Fatal("ack left no audit record")
	}
	if acks[0].Detail["by"] != "alice" {
		t.Fatalf("by = %q", acks[0].Detail["by"])
	}
	if acks[0].Target != "*" {
		t.Fatalf("target = %q, want *", acks[0].Target)
	}
}

// TestAuditRecordsConfigApply 验证配置变更（成功与被拒绝）都留痕。
func TestAuditRecordsConfigApply(t *testing.T) {
	h, toks := newAuthHarness(t, nil)
	h.actor = toks["admin"]

	// 成功：改一个站点无关的字段（logging.level）。
	raw, err := os.ReadFile(filepath.Join(h.dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["logging"] = map[string]any{"level": "debug"}
	code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(mustJSON(t, cfg)))
	if code != http.StatusOK {
		t.Fatalf("apply = %d %s", code, body)
	}

	// 失败：非法配置。
	cfg["schemaVersion"] = 999
	if code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(mustJSON(t, cfg))); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad apply = %d %s", code, body)
	}

	applies := findAction(auditItems(t, h.rt, 50), "config.apply")
	if len(applies) < 2 {
		t.Fatalf("config.apply audit records = %d, want at least 2", len(applies))
	}
	var sawOK, sawErr bool
	for _, a := range applies {
		if a.Result == audit.ResultOK {
			sawOK = true
			if !strings.HasPrefix(a.Target, "sha256:") {
				t.Fatalf("ok record target = %q, want the version hash", a.Target)
			}
		}
		if a.Result == audit.ResultError {
			sawErr = true
		}
	}
	if !sawOK || !sawErr {
		t.Fatalf("ok=%v err=%v, want both", sawOK, sawErr)
	}
}

// TestAuditChainVerifiesEndToEnd 验证真实运行产生的链可以校验通过。
func TestAuditChainVerifiesEndToEnd(t *testing.T) {
	h, toks := newAuthHarness(t, nil)
	h.actor = toks["operator"]
	for i := 0; i < 3; i++ {
		if code, body := h.do(http.MethodPost, "/api/v1/commands", `{"action":"outbox.flush"}`); code != http.StatusOK {
			t.Fatalf("command = %d %s", code, body)
		}
	}

	res, err := h.rt.AuditVerify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("verify = %+v", res)
	}
	if res.Checked < 3 {
		t.Fatalf("checked = %d, want at least 3", res.Checked)
	}

	st, ok := h.rt.AuditStats()
	if !ok {
		t.Fatal("audit reported as disabled")
	}
	if st.LastSeq < 3 {
		t.Fatalf("lastSeq = %d", st.LastSeq)
	}
}

// TestAuditOnDiskSurvivesRestart 验证审计记录真的落盘（而不是只在内存里）。
func TestAuditOnDiskSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := map[string]any{
		"schemaVersion": 1, "site": "test-site",
		"server":  map[string]any{"listen": "127.0.0.1:0"},
		"storage": map[string]any{"dir": dir, "journal": true, "outboxMaxRecords": 1000},
		"logging": map[string]any{"level": "warn"},
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
	}
	path := writeConfig(t, dir, cfg)

	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	if _, err := rt.Command(context.Background(), "outbox.flush", nil); err != nil {
		t.Fatalf("command: %v", err)
	}
	before, _ := rt.AuditStats()
	if err := rt.Stop(5 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	cancel()

	// 审计目录必须真的有段文件。
	ents, err := os.ReadDir(filepath.Join(dir, "audit"))
	if err != nil {
		t.Fatalf("audit dir: %v", err)
	}
	segs := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			segs++
		}
	}
	if segs == 0 {
		t.Fatal("no audit segment on disk after stop")
	}

	// 重启后链必须接上：序号继续，而不是从 1 重来。
	rt2, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { _ = rt2.Stop(5 * time.Second) }()
	if _, err := rt2.Command(context.Background(), "outbox.flush", nil); err != nil {
		t.Fatalf("command after restart: %v", err)
	}
	after, _ := rt2.AuditStats()
	if after.LastSeq <= before.LastSeq {
		t.Fatalf("lastSeq after restart = %d, previous = %d (chain restarted)", after.LastSeq, before.LastSeq)
	}
	res, err := rt2.AuditVerify()
	if err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
	if !res.OK {
		t.Fatalf("chain broken across restart: %+v", res)
	}
}

// TestAuditDisabledIsHonored 验证显式关闭后端点给出 409 而不是假装成功。
func TestAuditDisabledIsHonored(t *testing.T) {
	dir := t.TempDir()
	cfg := map[string]any{
		"schemaVersion": 1, "site": "test-site",
		"server": map[string]any{"listen": "127.0.0.1:0"},
		"storage": map[string]any{
			"dir": dir, "journal": true, "outboxMaxRecords": 1000,
			"audit": map[string]any{"enabled": false},
		},
		"logging": map[string]any{"level": "warn"},
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
	}
	path := writeConfig(t, dir, cfg)

	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	defer func() { _ = rt.Stop(5 * time.Second) }()

	if _, ok := rt.AuditStats(); ok {
		t.Fatal("audit reported enabled after being explicitly disabled")
	}
	if _, err := rt.AuditQuery(audit.Filter{}); err == nil {
		t.Fatal("AuditQuery succeeded with audit disabled")
	}
	// 写审计不能导致业务失败：Command 仍要正常工作。
	if _, err := rt.Command(context.Background(), "outbox.flush", nil); err != nil {
		t.Fatalf("command with audit disabled: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit")); err == nil {
		t.Fatal("audit directory was created even though audit is disabled")
	}
}

// TestSlaveWriteIsAuditedAsSlave 验证从站写入的来源可区分。
func TestSlaveWriteIsAuditedAsSlave(t *testing.T) {
	h, _ := newAuthHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	// 直接走从站适配器的写入路径（等价于外部主站写入）。
	w := slaveWriter{rt: h.rt}
	out := w.Write(context.Background(), []slavemb.WriteRequest{{Device: "sim01", Point: "write_me", Value: model.NumValue(model.TypeUint16, 4321)}})
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("slave write = %+v", out)
	}

	writes := findAction(auditItems(t, h.rt, 50), "point.write")
	if len(writes) == 0 {
		t.Fatal("slave write left no audit record")
	}
	if writes[0].Detail["via"] != "modbus-slave" {
		t.Fatalf("via = %q, want modbus-slave", writes[0].Detail["via"])
	}
	if writes[0].User != "system" {
		t.Fatalf("user = %q, want system for internal writes", writes[0].User)
	}
}
