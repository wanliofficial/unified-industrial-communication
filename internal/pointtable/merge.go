package pointtable

import (
	"fmt"
	"reflect"
	"strings"

	"uic/internal/model"
)

// Merge 把导入的点合并进已有设备。
//
// 这是「预览新增/冲突」的实现核心：它不写文件、不碰网络，只回答每个点位
// 会发生什么。CLI 的 -dry-run 与真正写入走同一条路径——预览和实际结果
// 用两份代码算，是「预览说没问题、写完却报错」的经典来源。
type ActionKind string

const (
	ActionNew       ActionKind = "new"
	ActionUnchanged ActionKind = "unchanged"
	ActionReplaced  ActionKind = "replaced"
	ActionConflict  ActionKind = "conflict"
)

// Action 描述一个点位在合并中的命运。
type Action struct {
	PointID string     `json:"point"`
	Kind    ActionKind `json:"kind"`
	Reason  string     `json:"reason,omitempty"`
}

// Merge 返回合并后的点位集合与逐点动作。
//
// replace=false（默认）时，同一个点位 ID 但内容不同视为冲突并整体失败：
// 点表导入最常见的破坏是「地址改了一半」，静默覆盖会让已经调试好的缩放、
// 死区、单位一起消失。要么完全一致（幂等重导），要么显式 -replace。
func Merge(existing, incoming []model.Point, replace bool) ([]model.Point, []Action, error) {
	merged := append([]model.Point(nil), existing...)
	index := make(map[string]int, len(merged))
	for i, p := range merged {
		index[p.ID] = i
	}

	incomingIDs := make(map[string]bool, len(incoming))
	var conflicts []string
	actions := make([]Action, 0, len(incoming))

	for _, p := range incoming {
		if incomingIDs[p.ID] {
			// ParseCSV 已经查过一次；这里是给程序化调用者的防线。
			return nil, nil, fmt.Errorf("duplicate point %q in imported set", p.ID)
		}
		incomingIDs[p.ID] = true

		i, ok := index[p.ID]
		switch {
		case !ok:
			index[p.ID] = len(merged)
			merged = append(merged, p)
			actions = append(actions, Action{PointID: p.ID, Kind: ActionNew})
		case reflect.DeepEqual(merged[i], p):
			actions = append(actions, Action{PointID: p.ID, Kind: ActionUnchanged})
		case replace:
			old := merged[i]
			merged[i] = p
			actions = append(actions, Action{PointID: p.ID, Kind: ActionReplaced, Reason: describeDiff(old, p)})
		default:
			actions = append(actions, Action{
				PointID: p.ID, Kind: ActionConflict,
				Reason: describeDiff(merged[i], p),
			})
			conflicts = append(conflicts, fmt.Sprintf("point %q: %s", p.ID, describeDiff(merged[i], p)))
		}
	}
	if len(conflicts) > 0 {
		return merged, actions, fmt.Errorf("%d point conflict(s): %s (use -replace to overwrite)",
			len(conflicts), strings.Join(conflicts, "; "))
	}
	return merged, actions, nil
}

// describeDiff 只说出「哪一项不一样」，不打印整个点位：现场看日志时，
// 一行能读完的信息比一份 JSON diff 有用。
func describeDiff(old, new model.Point) string {
	var diffs []string
	if old.Address.Canonical() != new.Address.Canonical() {
		diffs = append(diffs, fmt.Sprintf("address %s -> %s", old.Address.Canonical(), new.Address.Canonical()))
	}
	if old.DataType != new.DataType {
		diffs = append(diffs, fmt.Sprintf("dataType %s -> %s", old.DataType, new.DataType))
	}
	if old.ByteOrder != new.ByteOrder {
		diffs = append(diffs, fmt.Sprintf("byteOrder %s -> %s", old.ByteOrder, new.ByteOrder))
	}
	if old.Scale != new.Scale {
		diffs = append(diffs, fmt.Sprintf("scale %g -> %g", old.Scale, new.Scale))
	}
	if old.Bias != new.Bias {
		diffs = append(diffs, fmt.Sprintf("bias %g -> %g", old.Bias, new.Bias))
	}
	if old.Unit != new.Unit {
		diffs = append(diffs, fmt.Sprintf("unit %q -> %q", old.Unit, new.Unit))
	}
	if old.Writable != new.Writable {
		diffs = append(diffs, fmt.Sprintf("writable %v -> %v", old.Writable, new.Writable))
	}
	if old.Enabled != new.Enabled {
		diffs = append(diffs, fmt.Sprintf("enabled %v -> %v", old.Enabled, new.Enabled))
	}
	if old.Name != new.Name {
		diffs = append(diffs, fmt.Sprintf("name %q -> %q", old.Name, new.Name))
	}
	if len(diffs) == 0 {
		// 剩余字段（deadband/interval/description/tags）不同。
		diffs = append(diffs, "configuration differs")
	}
	return strings.Join(diffs, ", ")
}

// Count 汇总动作数量，供 CLI 打印一行摘要。
func Count(actions []Action) map[ActionKind]int {
	out := map[ActionKind]int{}
	for _, a := range actions {
		out[a.Kind]++
	}
	return out
}
