package consensus

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// testCluster 是同进程、TCP loopback transport 的多节点 raft 集群，供确定性测试用。
//
// 为什么用 TCP loopback 而不是 raft 的 InmemTransport：InmemTransport 只被 raft
// 自己的**单次 RPC 单元测试**使用，多节点组网时它的 pipeline/consumer 在提交
// 传播上表现不稳定——实测「leader 已 commit，follower 的 commitIndex 要数秒才
// 前进、甚至始终不动」，导致测试 flaky。raft 官方多节点集成测试（integ_test.go
// 的 MakeRaft）用的正是真实 TCP（localhost:0）。这里照搬官方做法：每个节点绑
// 127.0.0.1:0（内核分配空闲端口），既贴近生产 transport 路径，又稳定。
//
// 集群成长方式也用 RAFT 官方推荐：单节点种子自行引导成为 leader，再用 AddVoter
// 逐个把其余节点加入，避免「多节点各自 BootstrapCluster」的歧义窗口。
type testCluster struct {
	mu    sync.Mutex
	nodes map[string]*raftConsensus
	// appliers 记录每个节点的状态机（用于断言各副本 apply 了相同命令）。
	appliers map[string]*recordingApplier
	trans    map[string]*raft.NetworkTransport
	stores   map[string]*raft.InmemStore
	snaps    map[string]*raft.InmemSnapshotStore
}

// recordingApplier 是一个记录所有 apply 命令的假 Applier，并实现快照。
type recordingApplier struct {
	mu       sync.Mutex
	applied  []Command
	results  map[string]any
	failWith error // 非 nil 时 ApplyCommand 返回它（测 fail-closed）
}

func newRecordingApplier() *recordingApplier {
	return &recordingApplier{results: make(map[string]any)}
}

func (a *recordingApplier) ApplyCommand(cmd Command) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failWith != nil {
		return nil, a.failWith
	}
	a.applied = append(a.applied, cmd)
	if res, ok := a.results[string(cmd.Kind)]; ok {
		return res, nil
	}
	return fmt.Sprintf("applied:%s", cmd.Kind), nil
}

func (a *recordingApplier) Snapshot() ([]byte, error) { return []byte("snap"), nil }
func (a *recordingApplier) Restore(data []byte) error { return nil }

func (a *recordingApplier) Applied() []Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Command(nil), a.applied...)
}

func (a *recordingApplier) setFail(err error) {
	a.mu.Lock()
	a.failWith = err
	a.mu.Unlock()
}

func (a *recordingApplier) clearFail() {
	a.mu.Lock()
	a.failWith = nil
	a.mu.Unlock()
}

// newTestCluster 构造 n 个节点的集群并全部启动并完成扩容。
func newTestCluster(t *testing.T, n int) *testCluster {
	t.Helper()
	tc := &testCluster{
		nodes:    make(map[string]*raftConsensus),
		appliers: make(map[string]*recordingApplier),
		trans:    make(map[string]*raft.NetworkTransport),
		stores:   make(map[string]*raft.InmemStore),
		snaps:    make(map[string]*raft.InmemSnapshotStore),
	}

	ids := make([]string, n)
	addrs := make([]raft.ServerAddress, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("node-%d", i)
		// 127.0.0.1:0 让内核分配空闲端口；返回的 LocalAddr 是实际监听地址。
		tr, err := raft.NewTCPTransport("127.0.0.1:0", nil, 2, 10*time.Second, io.Discard)
		if err != nil {
			t.Fatalf("NewTCPTransport(%d): %v", i, err)
		}
		tc.trans[ids[i]] = tr
		addrs[i] = tr.LocalAddr()
	}

	// 先建种子节点（node-0），以「只含自身」的配置引导，立刻成为单节点 leader；
	// 再建其余节点（不引导，空配置等待加入）；最后在种子上逐个 AddServer。
	mkNode := func(i int) *raftConsensus {
		id := ids[i]
		store := raft.NewInmemStore()
		snap := raft.NewInmemSnapshotStore()
		app := newRecordingApplier()
		tc.stores[id] = store
		tc.snaps[id] = snap
		tc.appliers[id] = app
		backend := &raftBackend{
			logs:    store,
			stable:  store,
			snaps:   snap,
			trans:   tc.trans[id],
			cleanup: nil, // 测试由 tc.Close 统一释放
		}
		opts := Options{
			NodeID:        id,
			Addr:          string(addrs[i]),
			TestBackend:   backend,
			CommitTimeout: 3 * time.Second,
			// 时间参数：必须容忍 `-race` 的调度放大——50ms 一档在无竞态时够用，
			// 但 race 检测器会放大 goroutine 调度延迟，导致 leader 心跳超时、
			// follower 反复选举（term 抖动），已提交条目迟迟传不到副本。200ms
			// 既能在 race 下稳定，又把提交传播控制在百毫秒级。生产用默认 1s。
			HeartbeatTimeout:   200 * time.Millisecond,
			ElectionTimeout:    200 * time.Millisecond,
			LeaderLeaseTimeout: 200 * time.Millisecond,
		}
		if i == 0 {
			// 种子：只含自身的初始配置 → 单节点 always-leader。
			opts.Peers = []Server{{ID: id, Addr: string(addrs[i])}}
			opts.Bootstrap = true
		}
		if os.Getenv("CONSENSUS_TEST_LOG") != "" {
			opts.LogOutput = os.Stderr
		}
		c, err := newRaftConsensus(opts, app)
		if err != nil {
			t.Fatalf("newRaftConsensus(%s): %v", id, err)
		}
		tc.nodes[id] = c
		return c
	}

	seed := mkNode(0)
	// 等种子成为 leader（单节点集群应近乎立即）。
	if !waitCond(10*time.Second, seed.IsLeader) {
		t.Fatal("seed node never became leader")
	}

	// 建其余节点并逐个 AddServer 加入集群。
	for i := 1; i < n; i++ {
		mkNode(i)
		if err := seed.AddServer(context.Background(), ids[i], string(addrs[i]), 0); err != nil {
			t.Fatalf("AddServer(%s): %v", ids[i], err)
		}
	}

	// 等一个 leader 出现（扩容后 leader 仍是种子，通用等待兜底）。
	tc.waitLeader(t, 10*time.Second)
	return tc
}

// waitCond 轮询 cond 直到为真或超时，返回是否满足。
func waitCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// waitLeader 阻塞直到集群里出现一个 leader，返回 leader 的 nodeID。
func (tc *testCluster) waitLeader(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if id := tc.LeaderID(); id != "" {
			return id
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no leader elected within %v", timeout)
	return ""
}

// LeaderID 返回当前 leader 的节点 ID（空表示暂无）。
func (tc *testCluster) LeaderID() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for id, c := range tc.nodes {
		if c.IsLeader() {
			return id
		}
	}
	return ""
}

// node 返回指定节点。
func (tc *testCluster) node(id string) *raftConsensus {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.nodes[id]
}

// applier 返回指定节点的 applier。
func (tc *testCluster) applier(id string) *recordingApplier {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.appliers[id]
}

// ids 返回所有节点 ID（确定性顺序 node-0..node-n-1）。
func (tc *testCluster) ids() []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make([]string, 0, len(tc.nodes))
	for i := 0; i < len(tc.nodes); i++ {
		out = append(out, fmt.Sprintf("node-%d", i))
	}
	return out
}

// Close 关闭所有节点与 transport。
func (tc *testCluster) Close() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for _, c := range tc.nodes {
		_ = c.Shutdown()
	}
	for _, tr := range tc.trans {
		_ = tr.Close()
	}
}
