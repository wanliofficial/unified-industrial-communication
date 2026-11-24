package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"uic/internal/instanceid"
)

// TestVersionExposesInstanceID：/api/v1/version 暴露本实例的稳定身份。
func TestVersionExposesInstanceID(t *testing.T) {
	h := newHarness(t, nil)
	var v struct {
		InstanceID string `json:"instanceId"`
	}
	_, b := h.do("GET", "/api/v1/version", "")
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("解析 version 响应失败: %s", b)
	}
	if v.InstanceID == "" {
		t.Fatal("version 未暴露 instanceId")
	}
	if len(v.InstanceID) < len("inst-") {
		t.Fatalf("instanceId %q 格式异常", v.InstanceID)
	}
	// 与磁盘上的持久化身份一致：同一数据目录二次 Load 必须得到同一值。
	ondisk, err := instanceid.Load(h.dir)
	if err != nil {
		t.Fatalf("读回实例身份: %v", err)
	}
	if ondisk != v.InstanceID {
		t.Fatalf("version 暴露的 %q 与磁盘身份 %q 不一致", v.InstanceID, ondisk)
	}
}

// TestRuntimeInstanceIDStableAcrossRestart：同数据目录重启后 instanceID 不变——
// 否则每次重启换身份，MQTT 会话身份随之漂移，消歧与运维识别都失效。
func TestRuntimeInstanceIDStableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	first := startInstanceRuntime(t, dir)
	second := startInstanceRuntime(t, dir)
	if first != second {
		t.Fatalf("重启后实例身份漂移: 首次 %q 重启后 %q", first, second)
	}
}

// startInstanceRuntime 用指定数据目录启动一个 runtime，返回其实例身份并清理。
func startInstanceRuntime(t *testing.T, dir string) string {
	t.Helper()
	h := newHarnessAt(t, dir, func(cfg map[string]any) {
		cfg["storage"] = map[string]any{"dir": dir, "journal": true, "outboxMaxRecords": 1000}
	})
	t.Cleanup(func() {
		h.srv.Close()
		_ = h.rt.Stop(5 * time.Second)
		_ = h.sim.Close()
	})
	var v struct {
		InstanceID string `json:"instanceId"`
	}
	_, b := h.do("GET", "/api/v1/version", "")
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("解析 version: %s", b)
	}
	if v.InstanceID == "" {
		t.Fatal("version 未暴露 instanceId")
	}
	return v.InstanceID
}

// TestTwoInstancesDistinctIDs：同站点两个独立数据目录得到不同身份——同站点
// 多实例默认不冲突的根基。
func TestTwoInstancesDistinctIDs(t *testing.T) {
	a := startInstanceRuntime(t, t.TempDir())
	b := startInstanceRuntime(t, t.TempDir())
	if a == b {
		t.Fatalf("两个独立实例得到相同身份 %q", a)
	}
}
