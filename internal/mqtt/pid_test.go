package mqtt

// packet id 分配的回归测试。这些是白盒测试（在 mqtt 包内），因为要直接观察
// pending 表与 nextPID 计数器的状态——而这两样在外部测试里够不着。
//
// 为什么值得一个专门的测试：QoS1 发布靠 packet id 把 PUBACK 对回具体的发布者。
// 单调计数器一旦回绕到某个「仍在 pending」的 id，新发布者的 ackCh 会覆盖旧的，
// 旧发布者永远收不到自己的 PUBACK。这条竞态在低频下永不触发（要 6.5 万条在飞），
// 却在高吞吐北向通路上真实存在——正是「测了也复现不了」的那类隐患。

import "testing"

// allocFreePID 必须跳过 pending 里已占用的 id。
func TestAllocFreePIDSkipsInFlight(t *testing.T) {
	c := New(Options{})

	// 模拟一条仍在飞的发布占了 pid=5。
	sentinel := make(chan error, 1)
	c.pendingMu.Lock()
	c.pending[5] = sentinel
	// 把计数器拨到 4：单调的 allocPID 会立刻返回 5（撞上占位）。
	c.nextPID.Store(4)
	c.pendingMu.Unlock()

	ch := make(chan error, 1)
	pid := c.allocFreePID(ch)
	if pid == 5 {
		t.Fatalf("allocFreePID handed out pid 5 which is still in flight")
	}
	// 计数器从 4 递增，第一个非零且空闲的是 6（5 被占）。
	if pid != 6 {
		t.Fatalf("pid = %d, want 6 (next free after in-flight 5)", pid)
	}

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.pending[5] != sentinel {
		t.Fatal("in-flight pid 5 was evicted by a new allocation")
	}
	if c.pending[6] != ch {
		t.Fatal("new ackCh was not registered under the returned pid")
	}
}

// 连续分配应得到两两不同的 id，且都被登记进 pending。
func TestAllocFreePIDIsUniqueAndRegistered(t *testing.T) {
	c := New(Options{})
	const n = 1000
	chans := make([]chan error, n)
	seen := map[uint16]bool{}
	for i := 0; i < n; i++ {
		ch := make(chan error, 1)
		chans[i] = ch
		pid := c.allocFreePID(ch)
		if pid == 0 {
			t.Fatalf("allocFreePID returned reserved pid 0")
		}
		if seen[pid] {
			t.Fatalf("pid %d handed out twice", pid)
		}
		seen[pid] = true
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for i, ch := range chans {
		// 反查：每个 ackCh 都应在表里（用值比对，map 无序）。
		found := false
		for _, v := range c.pending {
			if v == ch {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ackCh %d not registered in pending", i)
		}
	}
}
