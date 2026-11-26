package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
)

// 灰度发布（staged rollout）：把一次全量变更切成若干批设备，逐批下发，
// 一批比一批扩大影响面，任何时刻都能回滚到灰度前的版本。
//
// 为什么需要它：ApplyConfig 是「一次全量、原子生效」。一个改了 500 台设备
// 的配置，如果第 3 台就暴露出问题，全量应用会让 500 台一起受影响。灰度把
// 「爆炸半径」限定在第一批，观察没问题再放下一批。
//
// 与 config.StagePlan / MergeForBatch 的分工：那两个是纯函数，只算「怎么切」
// 和「切到第 k 批时完整配置长什么样」；本文件负责编排——校验目标配置、逐批
// Apply（复用 ApplyConfig 的全套校验/审计/热更新/脱敏还原）、维护进度状态、
// 观察窗口与自动回滚。
//
// 关键安全点：每批只改动该批设备。其余设备保持当前生效配置，连接与死区状态
// 不受影响（reload 按指纹只重建变化的设备，这是天然的批隔离）。

// batchBaseline 是一台设备在被本灰度批次下发时刻的累计读统计快照。
// 观察窗口的失败率必须按「本次下发之后新采上来的样本」来算，而不是设备
// 全生命周期的累计失败/总数：后者会把设备几周历史故障期的失败数一直
// 带进新批次的判定里——一台历史上就 flaky 的设备，新配置再健康也会在
// 第一批就被自动回滚；反过来，长期干净的设备即便新批次把链路打挂，也
// 会被巨大的历史分母稀释、迟迟不触发回滚。两者都让「失败率超阈值自动
// 回滚」要么永远误伤、要么形同虚设。以基线做差，得到的才是「这批新配置
// 造成的失败率」。
type batchBaseline struct {
	total, failed uint64
}

// stagedRollout 是一次灰度发布的内部状态。对外只暴露 httpapi.StagedStatus，
// 目标完整配置与批次计划留在进程内，避免把整份配置随每个状态查询回吐一遍。
type stagedRollout struct {
	status httpapi.StagedStatus
	target config.File
	plan   config.StagedPlan

	// baseline 记录每台已下发设备在被下发时刻的累计读统计。失败率按
	// 增量（当前累计 - 基线）计算，使健康判定只反映本批次观察窗口内的
	// 表现，不掺杂设备全生命周期历史。
	baseline map[string]batchBaseline
}

// stagedState 是进程内的灰度编排状态。用独立锁而不是 r.mu：灰度操作会调用
// ApplyConfig（内部走 cfg.Apply 与 reload 的锁），把灰度状态塞进同一把锁会
// 形成锁环。
//
// now / healthFn 是可注入的测试接缝：观察窗口与失败率都是时间/统计相关的，
// 直接依赖真实时钟与真实采集会让用例变慢且不稳定。生产路径下两者为 nil，
// 分别回退到 time.Now 与调度器统计。
type stagedState struct {
	mu       sync.Mutex
	cur      *stagedRollout
	now      func() time.Time
	healthFn func(ids []string) (failed, total uint64)
}

// stagedNowLocked 返回编排用的当前时间。调用方必须持有 r.staged.mu——
// now/healthFn 是可注入的测试接缝，与看门狗并发读写时必须串行化。
func (r *Runtime) stagedNowLocked() time.Time {
	if r.staged.now != nil {
		return r.staged.now()
	}
	return time.Now()
}

// BeginStaged 开始一次灰度发布。
//
// raw 是「最终要达到的完整配置」，policy 控制每批设备数与观察窗口。返回第一批
// 下发后的状态。若目标与当前配置没有设备变更，直接收敛到目标并返回
// completed=true。
func (r *Runtime) BeginStaged(ctx context.Context, raw json.RawMessage, source string, policy httpapi.StagedPolicy) (httpapi.StagedStatus, error) {
	filled, err := r.fillRedacted(raw)
	if err != nil {
		return httpapi.StagedStatus{}, err
	}
	var target config.File
	if err := json.Unmarshal(filled, &target); err != nil {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusBadRequest, "config.invalid_json",
			"cannot parse config: %v", err)
	}
	// 先规范化再校验：Canonical/Validate 都假定默认值已填好。
	target.Normalize()
	if err := target.Validate(); err != nil {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid", "%v", err)
	}
	if policy.MaxFailureRate < 0 || policy.MaxFailureRate > 1 {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusBadRequest, "config.invalid_policy",
			"maxFailureRate must be within [0,1], got %v", policy.MaxFailureRate)
	}
	if policy.Observe < 0 {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusBadRequest, "config.invalid_policy",
			"observe must not be negative")
	}

	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	if r.staged.cur != nil && r.staged.cur.status.Active {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusConflict, "config.staged_in_progress",
			"a staged rollout is already in progress (next batch %d/%d); advance or abort it first",
			r.staged.cur.status.NextBatch, r.staged.cur.status.TotalBatches)
	}

	base := r.cfg.Current()
	plan := config.StagePlan(base, &target, policy.BatchSize)
	now := r.stagedNowLocked()

	ro := &stagedRollout{
		status: httpapi.StagedStatus{
			Active:         true,
			TargetVersion:  plan.TargetVersion,
			BaseVersion:    r.cfg.Version(),
			TotalBatches:   plan.BatchCount(),
			Batches:        plan.Batches,
			MaxFailureRate: policy.MaxFailureRate,
			StartedAt:      now.UTC(),
			UpdatedAt:      now.UTC(),
		},
		target:   target,
		plan:     plan,
		baseline: make(map[string]batchBaseline),
	}
	r.staged.cur = ro
	r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "started"})

	if plan.BatchCount() == 0 {
		// 没有设备变更：仍需把全局面/删除项收敛到目标，一次应用即可。
		if err := r.applyStagedTargetLocked(ctx, ro, source); err != nil {
			r.staged.cur = nil
			return httpapi.StagedStatus{}, err
		}
		return r.stagedViewLocked(ro, now), nil
	}

	if err := r.applyStagedBatchLocked(ctx, ro, 0, source, policy.Observe.Duration()); err != nil {
		r.staged.cur = nil
		return httpapi.StagedStatus{}, err
	}
	return r.stagedViewLocked(ro, now), nil
}

// AdvanceStaged 下发下一批。
//
// 两道闸门都在这一个入口上，顺序不能反：
//  1. 观察窗口还没结束 → 409，拒绝推进（这是「观察窗口」）；
//  2. 窗口已结束但已下发批次的失败率超阈值 → 自动回滚并 409（这是「自动暂停」）。
//
// 把判定放在推进入口而不是后台定时器上，是为了让「能不能推进」有唯一、
// 可审计的判定点；后台看门狗只负责「没人来推进时也要自动回滚」（见
// checkStagedHealth）。
func (r *Runtime) AdvanceStaged(ctx context.Context, source string) (httpapi.StagedStatus, error) {
	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	ro := r.staged.cur
	if ro == nil || !ro.status.Active {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusNotFound, "config.no_staged_rollout",
			"no staged rollout in progress")
	}
	now := r.stagedNowLocked()

	if ro.status.NextBatch >= ro.status.TotalBatches {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusConflict, "config.staged_complete",
			"all %d batches already applied; nothing to advance", ro.status.TotalBatches)
	}
	if wait := ro.observingFor(now); wait > 0 {
		ro.status.Observing = true
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusConflict, "config.staged_observing",
			"observation window still open for %s; refusing to advance batch %d/%d",
			wait.Truncate(time.Millisecond), ro.status.NextBatch+1, ro.status.TotalBatches)
	}
	if err := r.abortIfUnhealthyLocked(ctx, ro, now); err != nil {
		return httpapi.StagedStatus{}, err
	}
	if err := r.applyStagedBatchLocked(ctx, ro, ro.status.NextBatch, source, ro.observe()); err != nil {
		// 状态保持在下发前：调用方可以修正外部原因（例如设备暂时不可达）后重试。
		return httpapi.StagedStatus{}, err
	}
	return r.stagedViewLocked(ro, now), nil
}

// AbortStaged 终止灰度并回滚到灰度前的版本（baseVersion）。已下发的批次会被
// 撤销——这正是灰度存在的意义：影响面可收回。
func (r *Runtime) AbortStaged(ctx context.Context, source string) (config.ApplyResult, error) {
	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	ro := r.staged.cur
	if ro == nil || !ro.status.Active {
		return config.ApplyResult{}, httpapi.Errf(http.StatusNotFound, "config.no_staged_rollout",
			"no staged rollout in progress")
	}
	res, err := r.Rollback(ctx, ro.status.BaseVersion)
	if err != nil {
		return config.ApplyResult{}, err
	}
	r.markAbortedLocked(ro, "")
	r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "aborted"})
	return res, nil
}

// StagedStatusOf 返回当前灰度进度（无灰度时 Active=false）。
//
// 观测窗口与失败率是「实时算」的：状态里不缓存上一次的结论，避免界面显示
// 「观察中」而实际早该自动回滚。自动回滚本身由 AdvanceStaged 与看门狗执行。
func (r *Runtime) StagedStatusOf() httpapi.StagedStatus {
	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	if r.staged.cur == nil {
		return httpapi.StagedStatus{Active: false, Batches: [][]string{}}
	}
	return r.stagedViewLocked(r.staged.cur, r.stagedNowLocked())
}

// stagedViewLocked 把内部状态转成对外视图，并按 now 实时更新观察窗口与失败率。
func (r *Runtime) stagedViewLocked(ro *stagedRollout, now time.Time) httpapi.StagedStatus {
	out := ro.status
	out.Devices = stagedDeviceList(ro.plan.Batches)
	if out.Batches == nil {
		out.Batches = [][]string{}
	}
	out.Observing = ro.observingFor(now) > 0
	out.FailureRate = r.failureRateLocked(ro, appliedDeviceIDs(ro))
	return out
}

// observingFor 返回观察窗口的剩余时间；窗口已结束或未启用时返回 0。
func (ro *stagedRollout) observingFor(now time.Time) time.Duration {
	if ro.status.ObserveUntil.IsZero() {
		return 0
	}
	return ro.status.ObserveUntil.Sub(now)
}

// observe 返回本次编排的观察窗口长度（从 ObserveUntil 反推不安全，这里用
// 状态里保存的原始值：ObserveUntil 每批都会重设）。
func (ro *stagedRollout) observe() time.Duration { return ro.status.Observe.Duration() }

// appliedDeviceIDs 返回「已经下发过的批次」里的全部设备 ID。
func appliedDeviceIDs(ro *stagedRollout) []string {
	ids := make([]string, 0)
	for b := 0; b < ro.status.NextBatch && b < len(ro.plan.Batches); b++ {
		ids = append(ids, ro.plan.Batches[b]...)
	}
	return ids
}

// readCountersLocked 读取一台设备的累计读统计快照。设备不存在（尚未注册
// 或刚被删除）时返回零值——调用方据此把它的增量视为 0。
func (r *Runtime) readCountersLocked(id string) batchBaseline {
	st, ok := r.sched.StatusOf(id)
	if !ok {
		return batchBaseline{}
	}
	return batchBaseline{total: st.Stats.ReadsTotal, failed: st.Stats.ReadsFailed}
}

// failureRateLocked 计算给定设备「观察窗口内」的采集失败率：以每台设备被
// 本批次下发时刻的累计读统计为基线，只统计基线之后新采上来的样本。
//
// total 为 0（窗口内还没采过）时返回 0：「还没开始采」不是「采得很好」，但
// 也不该被当成故障去回滚一个刚下发的批次。
//
// healthFn 是测试接缝：注入时直接返回窗口内的 (failed, total)，跳过基线
// 增量计算（测试用假时钟/假统计驱动，不需要真实调度器读数）。
func (r *Runtime) failureRateLocked(ro *stagedRollout, ids []string) float64 {
	if len(ids) == 0 {
		return 0
	}
	if r.staged.healthFn != nil {
		failed, total := r.staged.healthFn(ids)
		return ratio(failed, total)
	}
	pairs := make([]counterPair, 0, len(ids))
	for _, id := range ids {
		pairs = append(pairs, counterPair{cur: r.readCountersLocked(id), base: ro.baseline[id]})
	}
	return windowedFailureRate(pairs)
}

// counterPair 是一台设备的「当前累计」与「下发时刻基线」。
type counterPair struct{ cur, base batchBaseline }

// windowedFailureRate 把所有设备「观察窗口内」的增量聚合成一个失败率纯函数。
// total 为 0（窗口内还没采过）时返回 0。
func windowedFailureRate(pairs []counterPair) float64 {
	var failed, total uint64
	for _, p := range pairs {
		df, dt := windowDelta(p.cur, p.base)
		failed += df
		total += dt
	}
	return ratio(failed, total)
}

func ratio(failed, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(failed) / float64(total)
}

// windowDelta 用当前累计减基线得到窗口内的增量，并防御计数器回退（重启、
// 设备重建会归位累计值）导致的无符号下溢。
func windowDelta(cur, base batchBaseline) (failed, total uint64) {
	if cur.total >= base.total {
		total = cur.total - base.total
	}
	if cur.failed >= base.failed {
		failed = cur.failed - base.failed
	}
	return failed, total
}

// abortIfUnhealthyLocked 在观察窗口结束后检查失败率，超阈值则自动回滚。
// 返回非 nil 表示已回滚（调用方不应继续推进）。调用方必须持有 r.staged.mu。
func (r *Runtime) abortIfUnhealthyLocked(ctx context.Context, ro *stagedRollout, now time.Time) error {
	max := ro.status.MaxFailureRate
	if max <= 0 || ro.status.ObserveUntil.IsZero() || now.Before(ro.status.ObserveUntil) {
		return nil
	}
	rate := r.failureRateLocked(ro, appliedDeviceIDs(ro))
	if rate <= max {
		return nil
	}
	reason := fmt.Sprintf("采集失败率 %.1f%% 超过阈值 %.1f%%（已下发 %d 台），灰度自动回滚",
		rate*100, max*100, len(appliedDeviceIDs(ro)))
	if _, err := r.Rollback(ctx, ro.status.BaseVersion); err != nil {
		return httpapiInternal("rollback unhealthy staged batch", err)
	}
	r.markAbortedLocked(ro, reason)
	r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "auto_aborted"})
	r.log.Warn("staged_auto_abort", "reason", reason, "failureRate", rate, "limit", max)
	return httpapi.Errf(http.StatusConflict, "config.staged_auto_aborted", "%s", reason)
}

// checkStagedHealth 由看门狗每秒调用：即使没有人来推进，观察窗口结束时也
// 要按失败率决定是否自动回滚。这样「自动暂停」不依赖外部调用方。
//
// 回滚在锁外执行（Rollback 会走 reload 与北向通知，可能耗时数秒）；回滚
// 完成后再取锁更新状态，若其间灰度已被人工终止/替换则不再改状态。
func (r *Runtime) checkStagedHealth(ctx context.Context, now time.Time) {
	r.staged.mu.Lock()
	ro := r.staged.cur
	if ro == nil || !ro.status.Active || ro.status.NextBatch == 0 {
		r.staged.mu.Unlock()
		return
	}
	max := ro.status.MaxFailureRate
	if max <= 0 || ro.status.ObserveUntil.IsZero() || now.Before(ro.status.ObserveUntil) {
		r.staged.mu.Unlock()
		return
	}
	rate := r.failureRateLocked(ro, appliedDeviceIDs(ro))
	if rate <= max {
		ro.status.Observing = false
		r.staged.mu.Unlock()
		return
	}
	base := ro.status.BaseVersion
	r.staged.mu.Unlock()

	reason := fmt.Sprintf("采集失败率 %.1f%% 超过阈值 %.1f%%，灰度自动回滚", rate*100, max*100)
	if _, err := r.Rollback(ctx, base); err != nil {
		r.log.Error("staged_auto_abort_failed", "error", errText(err))
		return
	}
	r.staged.mu.Lock()
	if r.staged.cur == ro && ro.status.Active {
		r.markAbortedLocked(ro, reason)
		r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "auto_aborted"})
		r.log.Warn("staged_auto_abort", "reason", reason, "failureRate", rate, "limit", max)
	}
	r.staged.mu.Unlock()
}

// markAbortedLocked 标记灰度已终止。reason 非空表示自动回滚。
func (r *Runtime) markAbortedLocked(ro *stagedRollout, reason string) {
	ro.status.Active = false
	ro.status.Completed = false
	ro.status.Observing = false
	ro.status.ObserveUntil = time.Time{}
	ro.status.Aborted = true
	ro.status.PausedReason = reason
	ro.status.UpdatedAt = r.stagedNowLocked().UTC()
}

// applyStagedBatchLocked 下发第 idx 批：用当前生效配置作底、把第 0..idx 批的
// 设备换成目标定义，走与全量应用完全相同的校验/审计/热更新路径。
//
// 以「当前配置」而不是「灰度起点配置」作底是有意的：第 0..idx-1 批已在前几轮
// 落地，再次覆盖它们是幂等的；这样做还能把两批之间发生的其它合法变更（例如
// 另一名运维改了报警规则）自然保留到下一批，而不是被灰度计划一键回退。
//
// 调用方必须持有 r.staged.mu。
func (r *Runtime) applyStagedBatchLocked(ctx context.Context, ro *stagedRollout, idx int, source string, observe time.Duration) error {
	merged := config.MergeForBatch(r.cfg.Current(), &ro.target, ro.plan, idx)
	raw, err := json.Marshal(merged)
	if err != nil {
		return httpapiInternal("encode staged batch", err)
	}
	if _, err := r.ApplyConfig(ctx, raw, stagedSource(source, idx)); err != nil {
		return err
	}
	// 基线必须在 ApplyConfig 之后取：ApplyConfig 会立刻重启该批设备的采集并
	// 产生新样本。取此刻的累计值作基线，观察窗口的失败率就只统计「本批次
	// 下发之后」的样本，不被设备全生命周期的历史故障污染。
	if ro.baseline == nil {
		ro.baseline = make(map[string]batchBaseline)
	}
	for _, id := range ro.plan.BatchIDs(idx) {
		ro.baseline[id] = r.readCountersLocked(id)
	}
	now := r.stagedNowLocked()
	ro.status.NextBatch = idx + 1
	ro.status.Observe = model.Duration(observe)
	ro.status.ObserveUntil = now.Add(observe) // observe=0 → 立刻到期，等于不观察
	ro.status.Active = true
	ro.status.Aborted = false
	ro.status.PausedReason = ""
	ro.status.UpdatedAt = now.UTC()
	if ro.status.NextBatch >= ro.status.TotalBatches {
		// 最后一批：把删除项一次性收敛到目标（它们不属于任何批次）。
		return r.applyStagedTargetLocked(ctx, ro, stagedSource(source, idx))
	}
	return nil
}

// applyStagedTargetLocked 把完整目标配置应用一次，用于「没有设备变更」与
// 「最后一批之后收敛删除项」。它确保灰度结束时进程内配置逐字节等于目标。
func (r *Runtime) applyStagedTargetLocked(ctx context.Context, ro *stagedRollout, source string) error {
	raw, err := json.Marshal(&ro.target)
	if err != nil {
		return httpapiInternal("encode staged target", err)
	}
	if _, err := r.ApplyConfig(ctx, raw, source); err != nil {
		return err
	}
	now := r.stagedNowLocked()
	ro.status.NextBatch = ro.status.TotalBatches
	ro.status.Completed = true
	ro.status.Active = false
	ro.status.Observing = false
	ro.status.ObserveUntil = time.Time{}
	ro.status.UpdatedAt = now.UTC()
	r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "completed"})
	return nil
}

func stagedSource(source string, idx int) string {
	if source == "" {
		source = "api"
	}
	return fmt.Sprintf("%s:staged-batch-%d", source, idx+1)
}

// stagedDeviceList 把批次拍平（排序），用于状态视图里的「影响面」汇总。
func stagedDeviceList(batches [][]string) []string {
	out := make([]string, 0, len(batches))
	for _, b := range batches {
		out = append(out, b...)
	}
	sort.Strings(out)
	return out
}
