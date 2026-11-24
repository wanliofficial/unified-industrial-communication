package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"uic/internal/config"
	"uic/internal/northbound/httpapi"
)

// addDevices 在 harness 初始配置里追加 n 台设备（复制首台 sim01 的定义，
// 只改 id/name）。点位地址与 sim01 重叠无所谓——灰度测试关心的是配置收敛，
// 不是采集值。
func addDevices(n int) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		devs := cfg["devices"].([]any)
		base := devs[0].(map[string]any)
		for i := 2; i <= n+1; i++ {
			clone := deepCopyMap(base)
			id := deviceID(i)
			clone["id"] = id
			clone["name"] = id
			devs = append(devs, clone)
		}
		cfg["devices"] = devs
	}
}

func deviceID(i int) string { return fmt.Sprintf("sim%02d", i) }

func deepCopyMap(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// currentMap 取当前生效配置（脱敏后）并解成可编辑的 map。
func currentMap(t *testing.T, h *harness) map[string]any {
	t.Helper()
	raw, err := h.rt.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return m
}

// deviceNames 返回 id → name 的快照，用于断言「哪台变了、哪台没变」。
func deviceNames(t *testing.T, h *harness) map[string]string {
	t.Helper()
	m := currentMap(t, h)
	out := map[string]string{}
	for _, d := range m["devices"].([]any) {
		dm := d.(map[string]any)
		out[dm["id"].(string)] = dm["name"].(string)
	}
	return out
}

// targetWithRenames 基于当前配置构造目标：改 names 里设备的 name，删 deletes
// 里的设备，其余保持。返回可直接下发的 JSON 与期望的版本号。
func targetWithRenames(t *testing.T, h *harness, names map[string]string, deletes ...string) (json.RawMessage, string) {
	t.Helper()
	m := currentMap(t, h)
	kept := make([]any, 0)
	for _, d := range m["devices"].([]any) {
		dm := d.(map[string]any)
		id := dm["id"].(string)
		drop := false
		for _, del := range deletes {
			if id == del {
				drop = true
			}
		}
		if drop {
			continue
		}
		if n, ok := names[id]; ok {
			dm["name"] = n
		}
		kept = append(kept, dm)
	}
	m["devices"] = kept
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}
	// 目标版本 = 目标配置的规范化哈希。用 config 包算，与 StagePlan 同源。
	var f config.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal target: %v", err)
	}
	f.Normalize()
	canon, err := f.Canonical()
	if err != nil {
		t.Fatalf("canonical target: %v", err)
	}
	return raw, config.VersionOf(canon)
}

// 灰度必须最终收敛到目标：逐批下发后，进程内配置逐字节等于目标（含删除项）。
func TestStagedRolloutConvergesToTarget(t *testing.T) {
	h := newHarness(t, addDevices(2)) // sim01, sim02, sim03
	ctx := context.Background()

	target, wantVersion := targetWithRenames(t,
		h,
		map[string]string{"sim01": "sim01-staged", "sim02": "sim02-staged"},
		"sim03", // 删除项：不属于任何批次，最后一批之后落地
	)

	st, err := h.rt.BeginStaged(ctx, target, "test", 1)
	if err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}
	if st.TotalBatches != 2 {
		t.Fatalf("批数 = %d，want 2（只改 sim01/sim02，sim03 是删除项不进批次）", st.TotalBatches)
	}
	if !st.Active || st.NextBatch != 1 {
		t.Fatalf("首批后状态 = %+v，want active 且 nextBatch=1", st)
	}

	// 批隔离：第 1 批只改 sim01；sim02 未下发、sim03 待删除，都必须保持原样。
	names := deviceNames(t, h)
	if names["sim01"] != "sim01-staged" {
		t.Fatalf("第 1 批未更新 sim01：%v", names)
	}
	if names["sim02"] != "sim02" {
		t.Fatalf("第 1 批不应改动 sim02：%v", names)
	}
	if _, ok := names["sim03"]; !ok {
		t.Fatalf("第 1 批不应删除 sim03：%v", names)
	}

	st, err = h.rt.AdvanceStaged(ctx, "test")
	if err != nil {
		t.Fatalf("AdvanceStaged: %v", err)
	}
	if st.Active || !st.Completed {
		t.Fatalf("末批后状态 = %+v，want active=false completed=true", st)
	}

	// 收敛：版本号必须等于目标版本，删除项必须真的消失。
	if got := h.rt.cfg.Version(); got != wantVersion {
		t.Fatalf("收敛后版本 = %s，want %s", got, wantVersion)
	}
	if got := h.rt.cfg.Version(); got != st.TargetVersion {
		t.Fatalf("收敛后版本 = %s，与 status.TargetVersion=%s 不一致", got, st.TargetVersion)
	}
	final := deviceNames(t, h)
	if final["sim01"] != "sim01-staged" || final["sim02"] != "sim02-staged" {
		t.Fatalf("末批未完成改名：%v", final)
	}
	if _, ok := final["sim03"]; ok {
		t.Fatalf("删除项 sim03 未落地：%v", final)
	}
}

// abort 必须把配置回滚到灰度前的版本——这正是「影响面可收回」的落点。
func TestStagedAbortRollsBackToBase(t *testing.T) {
	h := newHarness(t, addDevices(1)) // sim01, sim02
	ctx := context.Background()
	baseVersion := h.rt.cfg.Version()

	target, _ := targetWithRenames(t, h, map[string]string{
		"sim01": "sim01-staged",
		"sim02": "sim02-staged",
	})

	st, err := h.rt.BeginStaged(ctx, target, "test", 1)
	if err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}
	if st.BaseVersion != baseVersion {
		t.Fatalf("status.BaseVersion = %s，want %s", st.BaseVersion, baseVersion)
	}
	if names := deviceNames(t, h); names["sim01"] != "sim01-staged" {
		t.Fatalf("首批未生效：%v", names)
	}

	res, err := h.rt.AbortStaged(ctx, "test")
	if err != nil {
		t.Fatalf("AbortStaged: %v", err)
	}
	if res.Version != baseVersion {
		t.Fatalf("回滚结果版本 = %s，want %s", res.Version, baseVersion)
	}
	if got := h.rt.cfg.Version(); got != baseVersion {
		t.Fatalf("回滚后版本 = %s，want %s", got, baseVersion)
	}
	if names := deviceNames(t, h); names["sim01"] != "sim" {
		t.Fatalf("回滚后 sim01 未恢复：%v", names)
	}
	if s := h.rt.StagedStatusOf(); s.Active {
		t.Fatalf("回滚后灰度仍 active：%+v", s)
	}

	// 回滚后必须能开始新的灰度（状态已释放）。
	if _, err := h.rt.BeginStaged(ctx, target, "test", 1); err != nil {
		t.Fatalf("回滚后 BeginStaged 失败：%v", err)
	}
}

// 同时只允许一个灰度：第二次 begin 必须被拒（冲突），而不是把第一个灰度
// 悄悄覆盖掉——那会让运维以为在推进 A，实际在推进 B。
func TestStagedBeginRejectsConcurrentRollout(t *testing.T) {
	h := newHarness(t, addDevices(1))
	ctx := context.Background()

	target, _ := targetWithRenames(t, h, map[string]string{"sim01": "a", "sim02": "b"})
	if _, err := h.rt.BeginStaged(ctx, target, "test", 1); err != nil {
		t.Fatalf("首次 BeginStaged: %v", err)
	}
	_, err := h.rt.BeginStaged(ctx, target, "test", 1)
	if err == nil {
		t.Fatal("并发 BeginStaged 应被拒绝")
	}
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "config.staged_in_progress" {
		t.Fatalf("错误码 = %v，want config.staged_in_progress", err)
	}
}

// 目标与当前配置没有设备变更：直接收敛、不产生批次、不推版本。
func TestStagedNoChangesCompletesImmediately(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	baseVersion := h.rt.cfg.Version()

	raw, _ := h.rt.Config() // 原样回传当前配置
	st, err := h.rt.BeginStaged(ctx, raw, "test", 5)
	if err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}
	if st.TotalBatches != 0 {
		t.Fatalf("零变更批数 = %d，want 0", st.TotalBatches)
	}
	if st.Active || !st.Completed {
		t.Fatalf("零变更状态 = %+v，want active=false completed=true", st)
	}
	if got := h.rt.cfg.Version(); got != baseVersion {
		t.Fatalf("零变更不应推版本：%s → %s", baseVersion, got)
	}
}

// HTTP 接线：begin / status / advance / abort 四个端点各自可达，
// 权限与既有 config 端点一致（配置读/写）。
func TestStagedHTTPEndpoints(t *testing.T) {
	h := newHarness(t, addDevices(1))
	target, wantVersion := targetWithRenames(t, h, map[string]string{
		"sim01": "sim01-staged",
		"sim02": "sim02-staged",
	})

	body, _ := json.Marshal(map[string]any{"batchSize": 1, "config": json.RawMessage(target)})
	code, resp := h.do(http.MethodPost, "/api/v1/config/staged", string(body))
	if code != 200 {
		t.Fatalf("POST /config/staged = %d %s", code, resp)
	}
	var st httpapi.StagedStatus
	if err := json.Unmarshal(resp, &st); err != nil {
		t.Fatalf("decode begin resp: %v (%s)", err, resp)
	}
	if !st.Active || st.TotalBatches != 2 || st.NextBatch != 1 {
		t.Fatalf("begin 状态 = %+v", st)
	}

	code, resp = h.do(http.MethodGet, "/api/v1/config/staged", "")
	if code != 200 {
		t.Fatalf("GET /config/staged = %d %s", code, resp)
	}
	var got httpapi.StagedStatus
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got.NextBatch != 1 || len(got.Devices) != 2 {
		t.Fatalf("status 视图 = %+v，want nextBatch=1 devices=2", got)
	}

	code, resp = h.do(http.MethodPost, "/api/v1/config/staged/advance", "")
	if code != 200 {
		t.Fatalf("POST advance = %d %s", code, resp)
	}
	if err := json.Unmarshal(resp, &st); err != nil {
		t.Fatalf("decode advance: %v", err)
	}
	if st.Active || !st.Completed {
		t.Fatalf("advance 后状态 = %+v", st)
	}
	if got := h.rt.cfg.Version(); got != wantVersion {
		t.Fatalf("HTTP 灰度收敛版本 = %s，want %s", got, wantVersion)
	}

	// 再来一次并 abort：端点可正常回滚。
	body, _ = json.Marshal(map[string]any{"batchSize": 2, "config": json.RawMessage(target)})
	if code, resp := h.do(http.MethodPost, "/api/v1/config/staged", string(body)); code != 200 {
		// 目标与当前一致，会立即完成——也算成功路径。
		t.Fatalf("第二次 begin = %d %s", code, resp)
	}
	// 目标已收敛，这里 abort 应回到 404（没有进行中的灰度）。
	code, _ = h.do(http.MethodPost, "/api/v1/config/staged/abort", "")
	if code != 404 {
		t.Fatalf("已完成灰度上 abort = %d，want 404", code)
	}
}
