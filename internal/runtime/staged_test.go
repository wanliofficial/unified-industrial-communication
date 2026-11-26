package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/model"
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

	st, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{BatchSize: 1})
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

	st, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{BatchSize: 1})
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
	if _, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{BatchSize: 1}); err != nil {
		t.Fatalf("回滚后 BeginStaged 失败：%v", err)
	}
}

// 同时只允许一个灰度：第二次 begin 必须被拒（冲突），而不是把第一个灰度
// 悄悄覆盖掉——那会让运维以为在推进 A，实际在推进 B。
func TestStagedBeginRejectsConcurrentRollout(t *testing.T) {
	h := newHarness(t, addDevices(1))
	ctx := context.Background()

	target, _ := targetWithRenames(t, h, map[string]string{"sim01": "a", "sim02": "b"})
	if _, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{BatchSize: 1}); err != nil {
		t.Fatalf("首次 BeginStaged: %v", err)
	}
	_, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{BatchSize: 1})
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
	st, err := h.rt.BeginStaged(ctx, raw, "test", httpapi.StagedPolicy{BatchSize: 5})
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

// fakeClock 是可手动推进的测试时钟。起点设为「24 小时后」而不是过去：
// 真实看门狗仍按 time.Now 判断，若假时钟在过去，它会先于测试把窗口判成
// 已过期，导致用例与后台协程抢跑。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Now().Add(24 * time.Hour)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// injectStagedHooks 注入测试时钟与健康统计。走灰度锁是为了与看门狗并发读
// 不产生数据竞争。
func injectStagedHooks(h *harness, clock *fakeClock, health func(ids []string) (failed, total uint64)) {
	h.rt.staged.mu.Lock()
	defer h.rt.staged.mu.Unlock()
	h.rt.staged.now = clock.Now
	h.rt.staged.healthFn = health
}

// 观察窗口内拒绝推进；窗口结束后才允许下一批。这是「观察窗口」的核心语义。
func TestStagedObserveWindowBlocksAdvance(t *testing.T) {
	h := newHarness(t, addDevices(1))
	ctx := context.Background()
	clock := newFakeClock()
	injectStagedHooks(h, clock, nil) // 健康统计用真实调度器；这里只看窗口

	target, _ := targetWithRenames(t, h, map[string]string{"sim01": "a", "sim02": "b"})
	st, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{
		BatchSize: 1,
		Observe:   model.Duration(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}
	if st.ObserveUntil.IsZero() {
		t.Fatal("首批后 ObserveUntil 不应为零")
	}

	// 窗口内推进：拒绝。
	_, err = h.rt.AdvanceStaged(ctx, "test")
	if err == nil {
		t.Fatal("观察窗口内应拒绝推进")
	}
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "config.staged_observing" {
		t.Fatalf("错误码 = %v，want config.staged_observing", err)
	}
	if s := h.rt.StagedStatusOf(); !s.Observing || s.NextBatch != 1 {
		t.Fatalf("窗口内状态 = %+v，want observing 且 nextBatch=1", s)
	}

	// 窗口结束：允许推进并完成。
	clock.Advance(31 * time.Second)
	st, err = h.rt.AdvanceStaged(ctx, "test")
	if err != nil {
		t.Fatalf("窗口结束后 AdvanceStaged: %v", err)
	}
	if st.Active || !st.Completed {
		t.Fatalf("末批后状态 = %+v", st)
	}
}

// 窗口结束时失败率超阈值：推进被拒 + 自动回滚到灰度前版本。
func TestStagedAutoAbortOnUnhealthyBatch(t *testing.T) {
	h := newHarness(t, addDevices(1))
	ctx := context.Background()
	baseVersion := h.rt.cfg.Version()
	clock := newFakeClock()
	injectStagedHooks(h, clock, func([]string) (uint64, uint64) { return 9, 10 }) // 90% 失败

	target, _ := targetWithRenames(t, h, map[string]string{"sim01": "a", "sim02": "b"})
	if _, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{
		BatchSize:      1,
		Observe:        model.Duration(10 * time.Second),
		MaxFailureRate: 0.2,
	}); err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}

	clock.Advance(11 * time.Second)
	_, err := h.rt.AdvanceStaged(ctx, "test")
	if err == nil {
		t.Fatal("失败率超阈值时应自动回滚并拒绝推进")
	}
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "config.staged_auto_aborted" {
		t.Fatalf("错误码 = %v，want config.staged_auto_aborted", err)
	}

	if got := h.rt.cfg.Version(); got != baseVersion {
		t.Fatalf("自动回滚后版本 = %s，want %s", got, baseVersion)
	}
	s := h.rt.StagedStatusOf()
	if s.Active || !s.Aborted || s.PausedReason == "" {
		t.Fatalf("自动回滚后状态 = %+v，want aborted 且有原因", s)
	}
	if names := deviceNames(t, h); names["sim01"] != "sim" {
		t.Fatalf("自动回滚后 sim01 未恢复：%v", names)
	}
}

// 健康达标时窗口结束应正常推进，不能误伤。
func TestStagedHealthyBatchAdvances(t *testing.T) {
	h := newHarness(t, addDevices(1))
	ctx := context.Background()
	clock := newFakeClock()
	injectStagedHooks(h, clock, func([]string) (uint64, uint64) { return 1, 100 }) // 1% 失败

	target, _ := targetWithRenames(t, h, map[string]string{"sim01": "a", "sim02": "b"})
	if _, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{
		BatchSize: 1, Observe: model.Duration(5 * time.Second), MaxFailureRate: 0.2,
	}); err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}
	clock.Advance(6 * time.Second)
	st, err := h.rt.AdvanceStaged(ctx, "test")
	if err != nil {
		t.Fatalf("健康批次应允许推进：%v", err)
	}
	if st.Active || !st.Completed {
		t.Fatalf("末批后状态 = %+v", st)
	}
}

// 看门狗路径：没人调用 advance 时，观察窗口结束 + 健康不达标也必须自动回滚。
func TestStagedWatchdogAutoAborts(t *testing.T) {
	h := newHarness(t, addDevices(1))
	ctx := context.Background()
	baseVersion := h.rt.cfg.Version()
	clock := newFakeClock()
	injectStagedHooks(h, clock, func([]string) (uint64, uint64) { return 50, 100 }) // 50% 失败

	target, _ := targetWithRenames(t, h, map[string]string{"sim01": "a", "sim02": "b"})
	if _, err := h.rt.BeginStaged(ctx, target, "test", httpapi.StagedPolicy{
		BatchSize: 1, Observe: model.Duration(3 * time.Second), MaxFailureRate: 0.1,
	}); err != nil {
		t.Fatalf("BeginStaged: %v", err)
	}

	clock.Advance(4 * time.Second)
	// 直接驱动看门狗的那一次检查（真实 ticker 用的是 time.Now，假时钟在未来）。
	h.rt.checkStagedHealth(ctx, clock.Now())

	if got := h.rt.cfg.Version(); got != baseVersion {
		t.Fatalf("看门狗未回滚：版本 %s，want %s", got, baseVersion)
	}
	s := h.rt.StagedStatusOf()
	if s.Active || !s.Aborted {
		t.Fatalf("看门狗未标记终止：%+v", s)
	}
}

// 失败率必须只统计「观察窗口内」（基线之后）的样本，而不是设备全生命周期的
// 累计值。这是自动回滚正确性的核心：历史 flaky 的设备新配置健康时不该被误伤，
// 历史干净的设备新批次打挂时也不该被巨大分母稀释。
func TestWindowedFailureRateIgnoresLifetimeHistory(t *testing.T) {
	cases := []struct {
		name string
		pair counterPair
		want float64
	}{
		{
			// 设备历史上 1000 次里失败 900（90%），但窗口内新采 100 次全好。
			// 全生命周期口径会得到 ~92%（误伤），窗口口径必须是 0%。
			name: "历史 flaky、窗口健康 → 0",
			pair: counterPair{
				base: batchBaseline{total: 1000, failed: 900},
				cur:  batchBaseline{total: 1100, failed: 900},
			},
			want: 0,
		},
		{
			// 设备历史干净（10000 次全好），但新批次窗口内 100 次失败 60。
			// 全生命周期口径只有 ~0.6%（漏报），窗口口径必须是 60%。
			name: "历史干净、窗口打挂 → 0.6",
			pair: counterPair{
				base: batchBaseline{total: 10000, failed: 0},
				cur:  batchBaseline{total: 10100, failed: 60},
			},
			want: 0.6,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := windowedFailureRate([]counterPair{c.pair})
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("windowedFailureRate = %v, want %v", got, c.want)
			}
		})
	}
}

// 窗口内还没采到样本时不能算出「0% 失败」然后当成健康放行——但也不该
// 当成故障回滚刚下发的批次。约定是返回 0（由调用方结合 total 判断）。
func TestWindowedFailureRateEmptyWindow(t *testing.T) {
	got := windowedFailureRate([]counterPair{{
		base: batchBaseline{total: 500, failed: 250},
		cur:  batchBaseline{total: 500, failed: 250}, // 窗口内无新增
	}})
	if got != 0 {
		t.Fatalf("窗口内无样本时失败率 = %v, want 0", got)
	}
}

// 计数器回退（设备重启把累计值归位）不能让无符号减法下溢成一个天文数字，
// 否则一次重启就会被误判成「100% 失败」而回滚。
func TestWindowDeltaGuardsCounterReset(t *testing.T) {
	failed, total := windowDelta(
		batchBaseline{total: 10, failed: 3}, // 重启后归位
		batchBaseline{total: 1000, failed: 900},
	)
	if failed != 0 || total != 0 {
		t.Fatalf("计数器回退时增量 = (%d,%d), want (0,0)", failed, total)
	}
	if r := windowedFailureRate([]counterPair{{
		base: batchBaseline{total: 1000, failed: 900},
		cur:  batchBaseline{total: 10, failed: 3},
	}}); r != 0 {
		t.Fatalf("重启后失败率 = %v, want 0（不因下溢误判）", r)
	}
}
