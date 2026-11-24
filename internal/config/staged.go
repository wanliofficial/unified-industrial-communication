package config

import (
	"encoding/json"
	"sort"

	"uic/internal/alarm"
	"uic/internal/model"
)

// 灰度发布（staged rollout）的纯函数部分：把「一次全量变更」切成若干批，
// 每批只带一部分设备的改动，其余设备保持当前生效配置。
//
// 为什么切批在 config 包而不是 runtime：切批只依赖两份配置（当前 / 目标），
// 是一个纯函数；放进 runtime 会让它和连接重建、调度器搅在一起，既难测也
// 容易在「重置下一批」时把状态搞乱。runtime 只负责「取一份合成好的配置去
// Apply」，批的推进由它自己的编排状态机管。
//
// 语义边界（必须写清楚，否则灰度会变成「悄悄改了配置的一部分」）：
//
//  1. 灰度只针对**设备及其点位**。通道、报警规则、北向、存储等「全局面」在
//     任意一批里都会被改成目标值——它们不是按设备影响的，拆批没有意义，
//     硬拆只会让「第 1 批之后通道变了、第 2 批的设备连不上」。
//  2. 同一台设备要么整台在该批里（拿到目标定义），要么完全保持（拿到当前
//     定义）。不做「一台设备一半点位是新的一半是旧的」——那会产生一个两边
//     都不对的中间态。
//  3. 被删除的设备不属于任何一批（它没有「新定义」可下放），必须在灰度
//     开始前就删掉，或在最后一批之后一次性落地。Plan 的批里不含它们，
//     PendingStagedDevices 会显式报出它们，由调用方决定。

// StagedPlan 是一次灰度发布的批次划分。
type StagedPlan struct {
	// TargetVersion 是最终要达到的配置版本（内容哈希）。
	TargetVersion string `json:"targetVersion"`
	// Batches 是按顺序执行的设备批次。每批的 ID 互不重复。
	Batches [][]string `json:"batches"`
}

// BatchIDs 返回第 idx 批要下发的设备 ID。越界返回 nil。
func (p StagedPlan) BatchIDs(idx int) []string {
	if idx < 0 || idx >= len(p.Batches) {
		return nil
	}
	return p.Batches[idx]
}

// BatchCount 是批数。
func (p StagedPlan) BatchCount() int { return len(p.Batches) }

// deviceFingerprint 把一台设备的定义序列化成稳定字符串，用于「变没变」比较。
// 用 JSON 而不是逐字段比对：设备定义嵌套点位/地址/标签，逐字段等于把
// Device 的每个字段再抄一遍，漏一个就是一个「灰度把改动漏掉了」的静默 bug。
func deviceFingerprint(d model.Device) string {
	b, _ := json.Marshal(d)
	return string(b) // File 已 Normalize，字段顺序确定
}

// changedDeviceIDs 比较两份配置，返回「设备定义实际发生变化」的设备 ID
// （新增或修改），已排序。删除的设备不在此列——它们没有新定义可下放。
func changedDeviceIDs(before, target *File) []string {
	oldDefs := make(map[string]string, len(before.Devices))
	for _, d := range before.Devices {
		oldDefs[d.ID] = deviceFingerprint(d)
	}
	out := make([]string, 0)
	for _, d := range target.Devices {
		if fp, ok := oldDefs[d.ID]; !ok || fp != deviceFingerprint(d) {
			out = append(out, d.ID)
		}
	}
	// 排序让同样的输入产生同样的批次——否则 map 遍历顺序会让每次生成的
	// 「第 1 批是哪几台」都不一样，运维无法预先核对影响面。
	sort.Strings(out)
	return out
}

// StagePlan 把「before → target」的设备变更切成 batchSize 大小的批次。
//
// 零变更时返回空计划（BatchCount()==0）：调用方据此判断「没有什么可灰度
// 的」，而不是误发一批空变更把版本推上去。batchSize<=0 归一为 1（一次一台）。
func StagePlan(before, target *File, batchSize int) StagedPlan {
	if batchSize <= 0 {
		batchSize = 1
	}
	changed := changedDeviceIDs(before, target)
	var batches [][]string
	for i := 0; i < len(changed); i += batchSize {
		end := i + batchSize
		if end > len(changed) {
			end = len(changed)
		}
		batches = append(batches, changed[i:end])
	}
	canonical, _ := target.Canonical()
	return StagedPlan{
		TargetVersion: VersionOf(canonical),
		Batches:       batches,
	}
}

// MergeForBatch 生成「执行完第 idx 批之后」应当生效的完整配置：
// 第 0..idx 批的设备取 target 定义，其余一切（含尚未下发的设备）保持
// before；全局面（通道/报警/北向/存储/日志）取 target。
//
// 不变式：idx 走到最后一维时，结果应等于 target（除被删除的设备外）。
// 测试用这一点断言「灰度最终收敛到全量」。
func MergeForBatch(before, target *File, plan StagedPlan, idx int) *File {
	out := DeepCopy(before)
	batchIDs := make(map[string]bool)
	for b := 0; b <= idx && b < len(plan.Batches); b++ {
		for _, id := range plan.Batches[b] {
			batchIDs[id] = true
		}
	}

	targetByName := make(map[string]int, len(target.Devices))
	for i, d := range target.Devices {
		targetByName[d.ID] = i
	}

	// 1. 已下发的设备：用目标定义替换（新增的追加到末尾）。
	replaced := make(map[string]bool, len(batchIDs))
	for i, d := range out.Devices {
		if !batchIDs[d.ID] {
			continue
		}
		if ti, ok := targetByName[d.ID]; ok {
			out.Devices[i] = target.Devices[ti]
			replaced[d.ID] = true
		}
	}
	for _, d := range target.Devices {
		if batchIDs[d.ID] && !replaced[d.ID] {
			out.Devices = append(out.Devices, d)
		}
	}

	// 2. 全局面：直接取目标（见包注释第 1 条）。
	targetCopy := DeepCopy(target)
	out.Channels = targetCopy.Channels
	out.Alarms = cloneAlarms(target.Alarms)
	out.Northbound = target.Northbound
	out.Storage = target.Storage
	out.Logging = target.Logging
	out.Site = target.Site
	out.SchemaVersion = target.SchemaVersion
	return out
}

// PendingStagedDevices 返回「没有出现在任何批次里、需要额外处理」的设备 ID：
//   - 目标里新出现却不在任何批次（计划不完整）；
//   - before 里存在、target 里已被删除（它没有新定义可下放，需最后整体落地）。
//
// 正常计划下这一项只包含「被删除的设备」。显式暴露而不是静默忽略——静默
// 忽略会让删掉的设备永远留在进程里继续跑。
func PendingStagedDevices(before, target *File, plan StagedPlan) []string {
	staged := make(map[string]bool)
	for _, batch := range plan.Batches {
		for _, id := range batch {
			staged[id] = true
		}
	}
	targetSet := make(map[string]bool, len(target.Devices))
	var out []string
	for _, d := range target.Devices {
		targetSet[d.ID] = true
		if !staged[d.ID] {
			out = append(out, d.ID) // 新增却未进批次：漏了
		}
	}
	for _, d := range before.Devices {
		if !targetSet[d.ID] {
			out = append(out, d.ID) // 被删除：没有批次能承载
		}
	}
	sort.Strings(out)
	return out
}

// DeepCopy 深拷贝一份配置。用 JSON round-trip：File 里嵌套了指针（audit
// enabled、history heartbeat、报警 high/low 指针），浅拷贝会让「改一份
// 影响另一份」。配置是低频对象，这点序列化开销无所谓，换来的是不可能
// 漏掉某个字段的拷贝。
func DeepCopy(f *File) *File {
	if f == nil {
		return &File{}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		// File 只含可序列化字段，序列化不该失败；失败时返回一份空配置
		// 好过返回与入参共享指针的浅拷贝（那会静默污染调用方）。
		return &File{}
	}
	var out File
	if err := json.Unmarshal(raw, &out); err != nil {
		return &File{}
	}
	return &out
}

// cloneAlarms 深拷贝报警规则（Rule 含 *float64 阈值指针）。
func cloneAlarms(in []alarm.Rule) []alarm.Rule {
	if in == nil {
		return nil
	}
	out := make([]alarm.Rule, len(in))
	for i, r := range in {
		out[i] = r
		out[i].High = cloneFloat(r.High)
		out[i].Low = cloneFloat(r.Low)
	}
	return out
}

func cloneFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
