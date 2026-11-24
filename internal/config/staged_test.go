package config

import (
	"testing"

	"uic/internal/model"
)

// dev 构造一台最小合法设备。id 同时作为点位 ID 前缀，便于断言「点位跟着
// 设备一起走」。
func dev(id string, offset int) model.Device {
	return model.Device{
		ID:        id,
		ChannelID: "ch",
		Interval:  model.Duration(1_000_000_000), // 1s
		Timeout:   model.Duration(2_000_000_000),
		Enabled:   true,
		Points: []model.Point{{
			ID:       id + "-p",
			Address:  model.Address{Kind: model.AddressRegister, Area: "holding", Offset: offset, Length: 1, Bit: -1},
			DataType: model.TypeUint16,
			Enabled:  true,
		}},
	}
}

func stagedBase(devices ...model.Device) *File {
	f := &File{
		SchemaVersion: SchemaVersion,
		Site:          "test-site",
		Server:        Server{Listen: "127.0.0.1:0"},
		Channels: []model.Channel{{
			ID: "ch", Protocol: "virtual",
			Transport: model.Transport{Kind: "memory"},
		}},
		Devices: devices,
	}
	f.Normalize()
	return f
}

func mustCanonical(t *testing.T, f *File) string {
	t.Helper()
	b, err := f.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	return string(b)
}

// 没有任何设备变更时：计划为空，不能「发一批空变更」把版本推上去。
func TestStagePlanNoChanges(t *testing.T) {
	before := stagedBase(dev("d1", 0), dev("d2", 2))
	target := stagedBase(dev("d1", 0), dev("d2", 2))

	plan := StagePlan(before, target, 10)
	if plan.BatchCount() != 0 {
		t.Fatalf("零变更应得到空计划，实得 %d 批：%v", plan.BatchCount(), plan.Batches)
	}
	if plan.TargetVersion == "" {
		t.Fatal("TargetVersion 不应为空")
	}
}

// 每个变更设备必须恰好出现在一个批次里，且批次顺序稳定（排序后切）。
func TestStagePlanCoversEachChangedDeviceOnce(t *testing.T) {
	before := stagedBase(dev("a", 0), dev("b", 2), dev("c", 4))
	// 改 a、改 c、新增 d、b 不变。
	target := stagedBase(dev("a", 10), dev("b", 2), dev("c", 14), dev("d", 6))

	plan := StagePlan(before, target, 2)
	if got := plan.BatchCount(); got != 2 {
		t.Fatalf("批次 = %d，want 2（a,c,d 排序后两两一批）", got)
	}
	seen := map[string]int{}
	for _, batch := range plan.Batches {
		for _, id := range batch {
			seen[id]++
		}
	}
	if _, ok := seen["b"]; ok {
		t.Fatalf("未变更的 b 不应进批次：%v", plan.Batches)
	}
	for _, id := range []string{"a", "c", "d"} {
		if seen[id] != 1 {
			t.Fatalf("设备 %q 出现 %d 次，want 1：%v", id, seen[id], plan.Batches)
		}
	}
	// 排序保证：第一批应是 a,c（字典序），而不是 map 遍历的随机顺序。
	if plan.Batches[0][0] != "a" || plan.Batches[0][1] != "c" {
		t.Fatalf("第一批 = %v，want [a c]（必须稳定排序）", plan.Batches[0])
	}
}

// batchSize<=0 归一为 1：一次一台，绝不因为参数写错而「全量应用」。
func TestStagePlanBatchSizeDefaultsToOne(t *testing.T) {
	before := stagedBase(dev("a", 0), dev("b", 2))
	target := stagedBase(dev("a", 10), dev("b", 12))

	for _, size := range []int{0, -5} {
		plan := StagePlan(before, target, size)
		if plan.BatchCount() != 2 {
			t.Fatalf("batchSize=%d 应归一为 1（2 批），实得 %d", size, plan.BatchCount())
		}
	}
}

// 灰度收敛性：最后一批之后，合成配置必须逐字节等于目标配置。
// 这是「灰度最终等于全量」的核心断言——少一批、漏一台、点位没跟上都会变红。
func TestMergeForBatchConvergesToTarget(t *testing.T) {
	before := stagedBase(dev("a", 0), dev("b", 2), dev("c", 4))
	target := stagedBase(dev("a", 20), dev("b", 2), dev("c", 24), dev("d", 6))

	plan := StagePlan(before, target, 1)
	if plan.BatchCount() == 0 {
		t.Fatal("应至少有一批")
	}

	// 中间批：未下发的设备保持 before 定义。
	first := MergeForBatch(before, target, plan, 0)
	if first.Devices[0].ID != "a" || first.Devices[0].Points[0].Address.Offset != 20 {
		t.Fatalf("第一批未下发 a 的目标定义：%+v", first.Devices[0])
	}
	// b 不在任何批（未变更）→ 保持原值；c 尚未下发 → 保持原值。
	for _, d := range first.Devices {
		if d.ID == "c" && d.Points[0].Address.Offset != 4 {
			t.Fatalf("未下发的 c 被提前改了：offset=%d", d.Points[0].Address.Offset)
		}
	}

	// 最后一批：等于 target。
	last := MergeForBatch(before, target, plan, plan.BatchCount()-1)
	if got, want := mustCanonical(t, last), mustCanonical(t, target); got != want {
		t.Fatalf("最后一批未收敛到目标：\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// 被删除的设备不属于任何批次：PendingStagedDevices 必须显式报出它，
// 否则「删掉的设备永远留在进程里继续跑」。
func TestPendingStagedDevicesReportsDeletion(t *testing.T) {
	before := stagedBase(dev("keep", 0), dev("gone", 2))
	target := stagedBase(dev("keep", 10))

	plan := StagePlan(before, target, 10)
	pending := PendingStagedDevices(before, target, plan)

	if len(pending) != 1 || pending[0] != "gone" {
		t.Fatalf("待处理 = %v，want [gone]", pending)
	}
}

// 计划不完整（目标里有设备却不在任何批次）也要报出来，而不是静默当成没变。
func TestPendingStagedDevicesReportsUncoveredNewDevice(t *testing.T) {
	before := stagedBase(dev("a", 0))
	target := stagedBase(dev("a", 10), dev("b", 2))

	// 手工构造一个「漏掉 b」的计划。
	plan := StagedPlan{TargetVersion: "x", Batches: [][]string{{"a"}}}
	pending := PendingStagedDevices(before, target, plan)
	if len(pending) != 1 || pending[0] != "b" {
		t.Fatalf("待处理 = %v，want [b]（计划漏了 b）", pending)
	}
}

// DeepCopy 必须深拷贝指针字段：改副本不能影响原对象。
func TestDeepCopyIsolatesPointers(t *testing.T) {
	yes := true
	orig := stagedBase(dev("a", 0))
	orig.Storage.Audit.Enabled = &yes

	cp := DeepCopy(orig)
	if cp.Storage.Audit.Enabled == nil || !*cp.Storage.Audit.Enabled {
		t.Fatal("副本丢了 Audit.Enabled 指针")
	}
	if cp.Storage.Audit.Enabled == orig.Storage.Audit.Enabled {
		t.Fatal("副本与原对象共享 Audit.Enabled 指针（浅拷贝）")
	}
	*cp.Storage.Audit.Enabled = false
	if !*orig.Storage.Audit.Enabled {
		t.Fatal("改副本影响了原对象：不是深拷贝")
	}
}

// MergeForBatch 不能改动 before/target（纯函数）：调用两次结果一致，
// 且入参保持不变。
func TestMergeForBatchDoesNotMutateInputs(t *testing.T) {
	before := stagedBase(dev("a", 0), dev("b", 2))
	target := stagedBase(dev("a", 10), dev("b", 12))
	beforeCanon := mustCanonical(t, before)
	targetCanon := mustCanonical(t, target)

	plan := StagePlan(before, target, 1)
	_ = MergeForBatch(before, target, plan, 0)
	_ = MergeForBatch(before, target, plan, 1)

	if got := mustCanonical(t, before); got != beforeCanon {
		t.Fatal("MergeForBatch 改动了 before")
	}
	if got := mustCanonical(t, target); got != targetCanon {
		t.Fatal("MergeForBatch 改动了 target")
	}
}
