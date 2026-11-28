package consensus

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 单节点：always-leader
// ---------------------------------------------------------------------------

func TestSingleNodeIsAlwaysLeader(t *testing.T) {
	app := newRecordingApplier()
	// 单节点：不注入 TestBackend 会走生产路径（需要 DataDir）。这里用内存 backend
	// 但 Peers 只含本节点，验证「单节点恒为 leader」。
	tc := newTestCluster(t, 1)
	defer tc.Close()

	id := tc.waitLeader(t, 5*time.Second)
	c := tc.node(id)
	if !c.IsLeader() {
		t.Fatalf("single node %s not leader", id)
	}
	if c.Role() != RoleLeader {
		t.Errorf("Role = %q, want leader", c.Role())
	}

	// 单节点提交应立即 commit 并 apply 到本地 applier。
	res, err := c.Propose(context.Background(), Command{Kind: KindCommand, InstanceID: id, Payload: []byte("x")})
	if err != nil {
		t.Fatalf("Propose on single leader: %v", err)
	}
	if res == nil {
		t.Error("expected non-nil apply response")
	}
	got := tc.applier(id).Applied()
	if len(got) != 1 || got[0].Kind != KindCommand {
		t.Fatalf("applier got %+v", got)
	}
	_ = app
}

// ---------------------------------------------------------------------------
// 三节点：选主 / 复制 / 非 leader 拒绝写 / failover
// ---------------------------------------------------------------------------

func TestClusterElectsSingleLeader(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()

	leader := tc.waitLeader(t, 10*time.Second)
	leaders := 0
	for id := range tc.nodes {
		if tc.node(id).IsLeader() {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("expected exactly 1 leader, got %d (leader=%s)", leaders, leader)
	}
	// leader 与自身的 LeaderID 一致。
	if got := tc.node(leader).LeaderID(); got != leader {
		t.Errorf("leader.LeaderID() = %q, want %q", got, leader)
	}
}

func TestProposeReplicatesToAllAppliers(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()

	leader := tc.waitLeader(t, 10*time.Second)
	c := tc.node(leader)
	cmd := Command{Kind: KindApplyConfig, InstanceID: leader, Payload: []byte(`{"v":1}`)}
	if _, err := c.Propose(context.Background(), cmd); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	// 所有节点的 applier 最终都应 apply 同一条命令（复制 + 按序 apply）。
	deadline := time.Now().Add(5 * time.Second)
	for _, id := range []string{"node-0", "node-1", "node-2"} {
		app := tc.applier(id)
		for time.Now().Before(deadline) {
			if len(app.Applied()) >= 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		got := app.Applied()
		if len(got) != 1 || got[0].Kind != KindApplyConfig {
			t.Errorf("node %s applier got %+v, want 1 apply_config", id, got)
		}
	}
}

func TestFollowerRejectsPropose(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()

	leader := tc.waitLeader(t, 10*time.Second)
	var follower *raftConsensus
	for id := range tc.nodes {
		if id != leader {
			follower = tc.node(id)
			break
		}
	}
	if follower == nil {
		t.Fatal("no follower found")
	}
	// 非 leader Propose 必须立即 ErrNotLeader（fail-closed），不得等待超时。
	start := time.Now()
	_, err := follower.Propose(context.Background(), Command{Kind: KindCommand})
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower Propose err = %v, want ErrNotLeader", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("follower reject took %v, should be immediate", elapsed)
	}
}

func TestFailoverAfterLeaderShutdown(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()

	first := tc.waitLeader(t, 10*time.Second)
	// 关闭当前 leader（模拟崩溃/失联）。
	if err := tc.node(first).Shutdown(); err != nil {
		t.Fatalf("shutdown leader: %v", err)
	}
	// 剩余多数派应在 election timeout 内选出新 leader。deadline 放宽到 30s：
	// 200ms 选举超时在正常负载下 1-2s 完成，但整包与机器高负载下调度会被放大，
	// 15s 偶发不够（放宽窗口不改语义，只消除瞬态超时）。
	deadline := time.Now().Add(30 * time.Second)
	var second string
	for time.Now().Before(deadline) {
		for id, c := range tc.nodes {
			if id != first && c.IsLeader() {
				second = id
				break
			}
		}
		if second != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second == "" {
		t.Fatal("no new leader elected after old leader shutdown")
	}
	if second == first {
		t.Fatal("new leader is the shut-down node")
	}
	// 新 leader 能提交（term 前进、集群恢复可用）。
	if _, err := tc.node(second).Propose(context.Background(), Command{Kind: KindCommand}); err != nil {
		t.Fatalf("new leader %s cannot propose: %v", second, err)
	}
}

func TestApplyFailureMarksReplicaUnhealthy(t *testing.T) {
	tc := newTestCluster(t, 1)
	defer tc.Close()
	leader := tc.waitLeader(t, 5*time.Second)
	c := tc.node(leader)

	// 让 applier 抛错：Propose 应把 FSM 的 apply 错误返回给调用方（不静默成功）。
	tc.applier(leader).setFail(errors.New("apply boom"))
	_, err := c.Propose(context.Background(), Command{Kind: KindCommand})
	if err == nil {
		t.Fatal("Propose should surface apply error, not succeed silently")
	}
	if c.fsm.IsHealthy() {
		t.Error("FSM should be unhealthy after apply failure")
	}
	// 分歧是粘性的：applier 恢复正常也不再接受写。副本已经无法保证它与
	// 日志一致，只能靠快照恢复追平（见 TestRestoreReenablesPropose），
	// 否则「一次失败后悄悄恢复」会把分歧留在状态里。
	tc.applier(leader).clearFail()
	if _, err := c.Propose(context.Background(), Command{Kind: KindCommand}); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("Propose after transient recovery = %v, want ErrUnhealthy", err)
	}
}

// TestRestoreReenablesPropose 验证快照恢复是分歧副本唯一的复位路径。
func TestRestoreReenablesPropose(t *testing.T) {
	tc := newTestCluster(t, 1)
	defer tc.Close()
	leader := tc.waitLeader(t, 5*time.Second)
	c := tc.node(leader)

	tc.applier(leader).setFail(errors.New("apply boom"))
	if _, err := c.Propose(context.Background(), Command{Kind: KindCommand}); err == nil {
		t.Fatal("Propose should surface apply error")
	}
	tc.applier(leader).clearFail()
	if err := c.fsm.Restore(io.NopCloser(strings.NewReader(""))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !c.Healthy() {
		t.Fatal("Consensus should be healthy after a successful Restore")
	}
	if _, err := c.Propose(context.Background(), Command{Kind: KindCommand}); err != nil {
		t.Fatalf("Propose after restore: %v", err)
	}
}

func TestTermAdvancesAfterFailover(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()

	first := tc.waitLeader(t, 10*time.Second)
	firstTerm := tc.node(first).Term()

	if err := tc.node(first).Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// 放宽到 30s：200ms 选举超时在正常负载下 1-2s 内即可 failover，但当整包与
	// 其它 raft 集成测试并发、或机器正跑 `go test ./...`（高 CPU 负载 + GC）时，
	// 心跳/选举的 goroutine 调度会被放大，15s 偶发不够（曾观察到一次 no new
	// leader 超时）。这是放宽窗口、不改语义；term 前进的断言本身不受影响。
	deadline := time.Now().Add(30 * time.Second)
	var second string
	for time.Now().Before(deadline) {
		for id, c := range tc.nodes {
			if id != first && c.IsLeader() {
				second = id
			}
		}
		if second != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second == "" {
		t.Fatal("no new leader")
	}
	if got := tc.node(second).Term(); got <= firstTerm {
		t.Errorf("term did not advance: first=%d new=%d", firstTerm, got)
	}
}
