package runtime

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"

	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/probe"
)

// TestResolveProbeTargets 覆盖「全部/指定通道/指定设备/未知 id」四档语义：
// 设备解析到通道、去重、未知 id 报错（而不是静默漏探）。
func TestResolveProbeTargets(t *testing.T) {
	chans := []model.Channel{
		{ID: "a", Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"}},
		{ID: "b", Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:2"}},
		{ID: "c", Transport: model.Transport{Kind: "serial", Port: "/dev/ttyUSB0"}},
	}
	devs := []model.Device{
		{ID: "d1", ChannelID: "a"},
		{ID: "d2", ChannelID: "b"},
		{ID: "d3", ChannelID: "a"}, // 与 d1 同一通道：去重后只探一次
	}

	// 两个过滤条件都空 = 全部通道。
	if got, err := resolveProbeTargets(chans, devs, httpapi.ProbeQuery{}); err != nil || len(got) != 3 {
		t.Fatalf("空查询: got=%d err=%v, want 3/nil", len(got), err)
	}
	// 指定通道子集，顺序保持配置顺序（b 在 c 前）。
	got, err := resolveProbeTargets(chans, devs, httpapi.ProbeQuery{Channels: []string{"c", "b"}})
	if err != nil {
		t.Fatalf("指定通道: %v", err)
	}
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("子集 = %+v, want [b c]（按配置顺序）", got)
	}
	// 指定设备：d1+d3 同属通道 a，d2 属于 b → 解析为 {a,b}，按配置顺序 [a b]。
	got, err = resolveProbeTargets(chans, devs, httpapi.ProbeQuery{Devices: []string{"d1", "d3", "d2"}})
	if err != nil {
		t.Fatalf("指定设备: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("设备解析 = %+v, want [a b]（d1/d3 去重）", got)
	}
	// 未知 channel/device 必须报错，且携带 probe.unknown_channel。
	if _, err := resolveProbeTargets(chans, devs, httpapi.ProbeQuery{Channels: []string{"a", "nope"}}); err == nil {
		t.Fatal("未知 channel 必须报错")
	} else if !strings.Contains(err.Error(), "probe.unknown_channel") {
		t.Fatalf("错误 = %q, want 含 probe.unknown_channel", err.Error())
	}
	if _, err := resolveProbeTargets(chans, devs, httpapi.ProbeQuery{Devices: []string{"ghost"}}); err == nil {
		t.Fatal("未知 device 必须报错")
	}
}

// TestSummarizeProbe 校验各状态计数。
func TestSummarizeProbe(t *testing.T) {
	results := []probe.Result{
		{Status: probe.StatusOK}, {Status: probe.StatusOK}, {Status: probe.StatusFail},
		{Status: probe.StatusUnverified}, {Status: probe.StatusUnsupported},
	}
	s := summarizeProbe(results)
	if s.OK != 2 || s.Fail != 1 || s.Unverified != 1 || s.Unsupported != 1 {
		t.Fatalf("summary = %+v, want ok=2 fail=1 unverified=1 unsupported=1", s)
	}
}

// TestToProbeResults 校验内部→DTO 映射逐字段一致。
func TestToProbeResults(t *testing.T) {
	in := []probe.Result{{
		Channel: "ch", Protocol: "modbus-tcp", Transport: "tcp",
		Address: "127.0.0.1:502", Status: probe.StatusOK, Reachable: true, RTTMs: 7,
	}}
	out := toProbeResults(in)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	r := out[0]
	if r.Channel != "ch" || r.Protocol != "modbus-tcp" || r.Transport != "tcp" ||
		r.Address != "127.0.0.1:502" || r.Status != probe.StatusOK ||
		!r.Reachable || r.RTTMs != 7 {
		t.Fatalf("映射不一致: %+v", r)
	}
}

// ---------------------------------------------------------------------------
// 端到端：真实 loopback 通道 → /api/v1/diagnostics/probe
// ---------------------------------------------------------------------------

// TestProbeEndpointReachable：默认配置里的 ch-modbus 指向真实 Modbus 从站
// （sim.Server），探测必须 ok、reachable、带 RTT。这证明探测走的是真实网络
// 握手，不是桩。
func TestProbeEndpointReachable(t *testing.T) {
	h := newHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/diagnostics/probe", "")
	if code != http.StatusOK {
		t.Fatalf("probe = %d, want 200: %s", code, body)
	}
	var rep httpapi.ProbeReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Total != 1 || len(rep.Results) != 1 {
		t.Fatalf("total=%d results=%d, want 1/1", rep.Total, len(rep.Results))
	}
	r := rep.Results[0]
	if r.Channel != "ch-modbus" {
		t.Fatalf("channel = %q, want ch-modbus", r.Channel)
	}
	if r.Status != "ok" || !r.Reachable {
		t.Fatalf("status=%q reachable=%v, want ok/true (addr=%s err=%s)", r.Status, r.Reachable, r.Address, r.Error)
	}
	// RTT 单位是毫秒：真实网络才有意义，loopback 是亚毫秒会被截断成 0，
	// 因此只断言非负（负值只会来自 bug）。status/reachable 才是有意义的判定。
	if r.RTTMs < 0 {
		t.Fatalf("RTTMs = %d, want >= 0", r.RTTMs)
	}
	if rep.Summary.OK != 1 {
		t.Fatalf("summary.ok = %d, want 1", rep.Summary.OK)
	}
}

// TestProbeEndpointFilter：?channel= 只探测指定通道；未知 channel 返回 400
// probe.unknown_channel。这验证「指定但拼错」不会静默漏探。
func TestProbeEndpointFilter(t *testing.T) {
	h := newHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/diagnostics/probe?channel=ch-modbus", "")
	if code != http.StatusOK {
		t.Fatalf("filtered probe = %d, want 200: %s", code, body)
	}
	var rep httpapi.ProbeReport
	_ = json.Unmarshal(body, &rep)
	if rep.Total != 1 {
		t.Fatalf("filtered total = %d, want 1", rep.Total)
	}

	// 未知 channel → 400 + probe.unknown_channel。
	code, body = h.do(http.MethodGet, "/api/v1/diagnostics/probe?channel=does-not-exist", "")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown channel = %d, want 400: %s", code, body)
	}
	if !strings.Contains(string(body), "probe.unknown_channel") {
		t.Fatalf("body = %s, want probe.unknown_channel", body)
	}
}

// TestProbeEndpointRBAC：probe 与诊断包同级（PermDiagnostics），viewer 无权限，
// 必须 403。这证明权限被正确接在路由上而不是「读类端点」。
func TestProbeEndpointRBAC(t *testing.T) {
	h, tokens := newAuthHarness(t, nil)

	// viewer 无 diagnostics 权限 → 403。
	h.actor = tokens["viewer"]
	if code, _ := h.do(http.MethodGet, "/api/v1/diagnostics/probe", ""); code != http.StatusForbidden {
		t.Fatalf("viewer probe = %d, want 403", code)
	}
	// operator 有 diagnostics 权限 → 200。
	h.actor = tokens["operator"]
	if code, _ := h.do(http.MethodGet, "/api/v1/diagnostics/probe", ""); code != http.StatusOK {
		t.Fatalf("operator probe = %d, want 200", code)
	}
}

// TestProbeEndpointUnreachable：配置一条指向未监听端口的通道，探测必须 fail 且
// 带错误原因。这是「为什么连不上」最直接的回归。
func TestProbeEndpointUnreachable(t *testing.T) {
	// 拿一个空闲端口再关掉，制造「无人监听」。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	h := newHarness(t, func(cfg map[string]any) {
		chans := cfg["channels"].([]any)
		chans = append(chans, map[string]any{
			"id": "ch-dead", "protocol": "modbus-tcp",
			"transport": map[string]any{"kind": "tcp", "address": deadAddr},
			"timeout":   "1s",
		})
		cfg["channels"] = chans
	})

	code, body := h.do(http.MethodGet, "/api/v1/diagnostics/probe?channel=ch-dead", "")
	if code != http.StatusOK {
		t.Fatalf("probe = %d, want 200（探测失败不是 HTTP 错误）: %s", code, body)
	}
	var rep httpapi.ProbeReport
	_ = json.Unmarshal(body, &rep)
	if rep.Total != 1 {
		t.Fatalf("total = %d, want 1", rep.Total)
	}
	r := rep.Results[0]
	if r.Status != "fail" || r.Reachable {
		t.Fatalf("status=%q reachable=%v, want fail/false", r.Status, r.Reachable)
	}
	if r.Error == "" {
		t.Fatal("fail 必须带错误原因")
	}
	if rep.Summary.Fail != 1 {
		t.Fatalf("summary.fail = %d, want 1", rep.Summary.Fail)
	}
}

// TestProbeEndpointTimeoutClamp：?timeout= 越界必须夹取到 [100ms,10s] 而非报错
// 或忽略。用一个超大值验证不报错、仍返回。
func TestProbeEndpointTimeoutClamp(t *testing.T) {
	h := newHarness(t, nil)
	code, body := h.do(http.MethodGet, "/api/v1/diagnostics/probe?timeout=999s", "")
	if code != http.StatusOK {
		t.Fatalf("超时夹取后 probe = %d, want 200: %s", code, body)
	}
}

// TestProbeEndpointByDevice：?device= 把设备解析到其通道并探测——这是运维
// 「这台设备为什么连不上」的直觉入口。sim01 在 ch-modbus 上，探测应 ok。
func TestProbeEndpointByDevice(t *testing.T) {
	h := newHarness(t, nil)

	code, body := h.do(http.MethodGet, "/api/v1/diagnostics/probe?device=sim01", "")
	if code != http.StatusOK {
		t.Fatalf("device probe = %d, want 200: %s", code, body)
	}
	var rep httpapi.ProbeReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Total != 1 || rep.Results[0].Channel != "ch-modbus" {
		t.Fatalf("device 解析 = total %d, results %+v, want 1 条 ch-modbus", rep.Total, rep.Results)
	}
	if rep.Results[0].Status != "ok" {
		t.Fatalf("status = %q, want ok", rep.Results[0].Status)
	}

	// 未知 device → 400。
	code, body = h.do(http.MethodGet, "/api/v1/diagnostics/probe?device=ghost", "")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown device = %d, want 400: %s", code, body)
	}
	if !strings.Contains(string(body), "probe.unknown_channel") {
		t.Fatalf("body = %s, want probe.unknown_channel", body)
	}
}
