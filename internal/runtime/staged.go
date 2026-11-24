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
// 支持回滚。
//
// 关键安全点：每批只改动该批设备。其余设备保持当前生效配置，连接与死区状态
// 不受影响（reload 按指纹只重建变化的设备，这是天然的批隔离）。

// stagedRollout 是一次灰度发布的内部状态。对外只暴露 httpapi.StagedStatus，
// 目标完整配置与批次计划留在进程内，避免把整份配置随每个状态查询回吐一遍。
type stagedRollout struct {
	status httpapi.StagedStatus
	target config.File
	plan   config.StagedPlan
}

// stagedState 是进程内的灰度编排状态。用独立锁而不是 r.mu：灰度操作会调用
// ApplyConfig（内部走 cfg.Apply 与 reload 的锁），把灰度状态塞进同一把锁会
// 形成锁环。
type stagedState struct {
	mu  sync.Mutex
	cur *stagedRollout
}

// BeginStaged 开始一次灰度发布。
//
// raw 是「最终要达到的完整配置」，batchSize 是每批设备数。返回第一批下发后的
// 状态。若目标与当前配置没有设备变更，直接收敛到目标并返回 completed=true。
func (r *Runtime) BeginStaged(ctx context.Context, raw json.RawMessage, source string, batchSize int) (httpapi.StagedStatus, error) {
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

	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	if r.staged.cur != nil && r.staged.cur.status.Active {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusConflict, "config.staged_in_progress",
			"a staged rollout is already in progress (next batch %d/%d); advance or abort it first",
			r.staged.cur.status.NextBatch, r.staged.cur.status.TotalBatches)
	}

	base := r.cfg.Current()
	plan := config.StagePlan(base, &target, batchSize)

	ro := &stagedRollout{
		status: httpapi.StagedStatus{
			Active:        true,
			TargetVersion: plan.TargetVersion,
			BaseVersion:   r.cfg.Version(),
			TotalBatches:  plan.BatchCount(),
			Batches:       plan.Batches,
			StartedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		},
		target: target,
		plan:   plan,
	}
	r.staged.cur = ro
	r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "started"})

	if plan.BatchCount() == 0 {
		// 没有设备变更：仍需把全局面/删除项收敛到目标，一次应用即可。
		if err := r.applyStagedTargetLocked(ctx, ro, source); err != nil {
			r.staged.cur = nil
			return httpapi.StagedStatus{}, err
		}
		return stagedView(ro), nil
	}

	if err := r.applyStagedBatchLocked(ctx, ro, 0, source); err != nil {
		r.staged.cur = nil
		return httpapi.StagedStatus{}, err
	}
	return stagedView(ro), nil
}

// AdvanceStaged 下发下一批。返回下发后的状态。
func (r *Runtime) AdvanceStaged(ctx context.Context, source string) (httpapi.StagedStatus, error) {
	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	ro := r.staged.cur
	if ro == nil || !ro.status.Active {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusNotFound, "config.no_staged_rollout",
			"no staged rollout in progress")
	}
	if ro.status.NextBatch >= ro.status.TotalBatches {
		return httpapi.StagedStatus{}, httpapi.Errf(http.StatusConflict, "config.staged_complete",
			"all %d batches already applied; nothing to advance", ro.status.TotalBatches)
	}
	if err := r.applyStagedBatchLocked(ctx, ro, ro.status.NextBatch, source); err != nil {
		// 状态保持在下发前：调用方可以修正外部原因（例如设备暂时不可达）后重试。
		return httpapi.StagedStatus{}, err
	}
	return stagedView(ro), nil
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
	ro.status.Active = false
	ro.status.Completed = false
	ro.status.UpdatedAt = time.Now().UTC()
	r.metrics.Inc("uic_config_staged_total", obs.Labels{"status": "aborted"})
	return res, nil
}

// StagedStatusOf 返回当前灰度进度（无灰度时 Active=false）。
func (r *Runtime) StagedStatusOf() httpapi.StagedStatus {
	r.staged.mu.Lock()
	defer r.staged.mu.Unlock()
	if r.staged.cur == nil {
		return httpapi.StagedStatus{Active: false, Batches: [][]string{}}
	}
	return stagedView(r.staged.cur)
}

// stagedView 把内部状态转成对外视图。Devices 由 Batches 拍平，保证与批次一致。
func stagedView(ro *stagedRollout) httpapi.StagedStatus {
	out := ro.status
	out.Devices = stagedDeviceList(ro.plan.Batches)
	if out.Batches == nil {
		out.Batches = [][]string{}
	}
	return out
}

// applyStagedBatchLocked 下发第 idx 批：用当前生效配置作底、把第 0..idx 批的
// 设备换成目标定义，走与全量应用完全相同的校验/审计/热更新路径。
//
// 以「当前配置」而不是「灰度起点配置」作底是有意的：第 0..idx-1 批已在前几轮
// 落地，再次覆盖它们是幂等的；这样做还能把两批之间发生的其它合法变更（例如
// 另一名运维改了报警规则）自然保留到下一批，而不是被灰度计划一键回退。
//
// 调用方必须持有 r.staged.mu。
func (r *Runtime) applyStagedBatchLocked(ctx context.Context, ro *stagedRollout, idx int, source string) error {
	merged := config.MergeForBatch(r.cfg.Current(), &ro.target, ro.plan, idx)
	raw, err := json.Marshal(merged)
	if err != nil {
		return httpapiInternal("encode staged batch", err)
	}
	if _, err := r.ApplyConfig(ctx, raw, stagedSource(source, idx)); err != nil {
		return err
	}
	ro.status.NextBatch = idx + 1
	ro.status.UpdatedAt = time.Now().UTC()
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
	ro.status.NextBatch = ro.status.TotalBatches
	ro.status.Completed = true
	ro.status.Active = false
	ro.status.UpdatedAt = time.Now().UTC()
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
