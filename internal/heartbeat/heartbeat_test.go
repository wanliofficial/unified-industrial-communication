package heartbeat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Write 后能被读回，且字段一致。这是心跳文件的唯一契约：外部脚本解析它。
func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{
		TS:            1234567890,
		TSAt:          "2026-03-01T12:00:00Z",
		PID:           4242,
		InstanceID:    "inst-abc123",
		Version:       "test",
		UptimeSeconds: 99,
		Scheduler:     Scheduler{Verdict: "healthy", AgeMs: 50, StallAfterMs: 15000},
		Devices:       Devices{Total: 3, Collecting: 2, Faulted: 1},
	}
	if err := Write(dir, snap); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TS != snap.TS || got.PID != snap.PID || got.InstanceID != snap.InstanceID {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Scheduler.Verdict != "healthy" || got.Scheduler.AgeMs != 50 {
		t.Fatalf("scheduler = %+v", got.Scheduler)
	}
	if got.Devices.Faulted != 1 {
		t.Fatalf("devices = %+v", got.Devices)
	}
}

// 原子写：目录里不能残留 .tmp 文件（rename 成功后临时文件已不存在）。
func TestWriteNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Snapshot{TS: 1}); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
	// 正文件必须在。
	if _, err := os.Stat(Path(dir)); err != nil {
		t.Fatalf("心跳文件不存在: %v", err)
	}
}

// 覆盖写：第二次写必须覆盖第一次，而不是追加/失败。
func TestWriteOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Snapshot{TS: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, Snapshot{TS: 2}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path(dir))
	var got Snapshot
	_ = json.Unmarshal(b, &got)
	if got.TS != 2 {
		t.Fatalf("ts = %d, want 2（应覆盖）", got.TS)
	}
}

// Path 稳定指向 <dir>/heartbeat.json。
func TestPath(t *testing.T) {
	if got := Path("/data"); got != filepath.Join("/data", "heartbeat.json") {
		t.Fatalf("path = %q", got)
	}
}

// 亚秒级心跳：TS（秒）会重复，TSMs 必须仍然前进，否则读方无法判断文件是否
// 在更新——这正是「快于 1s 的心跳看起来像卡住」的缺陷来源。
func TestSnapshotMillisecondPrecision(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	first := Snapshot{TS: now.Unix(), TSMs: now.UnixMilli()}
	laterAt := now.Add(30 * time.Millisecond)
	later := Snapshot{TS: laterAt.Unix(), TSMs: laterAt.UnixMilli(), TSAt: laterAt.Format(time.RFC3339Nano)}
	if first.TS != later.TS {
		t.Skip("前提不成立：30ms 跨过了秒边界")
	}
	if first.TSMs == later.TSMs {
		t.Fatal("同一秒内 TSMs 无法区分两次心跳")
	}
	if err := Write(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, later); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path(dir))
	var got Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.TSMs != later.TSMs {
		t.Fatalf("tsMs = %d, want %d", got.TSMs, later.TSMs)
	}
	if got.TSAt == "" {
		t.Fatal("缺少 tsAt")
	}
}
