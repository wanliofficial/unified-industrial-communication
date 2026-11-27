package mqtt

// dropConn 的契约是「关闭当前连接并清空状态」。旧连接的 goroutine 晚于
// 重连醒来时，它手里的 conn 早已不是当前连接——此时只能关掉这条死 socket，
// 绝不能重置新连接的 state、失败新连接的在飞发布、或误伤新连接的 PING。
//
// 这条路径真实可达：保活循环对旧连接 A 判死后 dropConn(A)，驱动随即重连到
// B（state=2）；旧 readLoop(A) 因 A 被 Close 而读到 EOF 醒来，再调一次
// dropConn(A)。此时 c.conn==B，无条件清零会把健康的 B 打成「未连接」。

import (
	"net"
	"testing"
)

func TestDropConnOfStaleConnectionLeavesCurrentIntact(t *testing.T) {
	c := New(Options{})

	// 两条独立管道：a 是已经死掉的旧连接，b 是当前连接。
	a, aPeer := net.Pipe()
	defer aPeer.Close()
	b, bPeer := net.Pipe()
	defer bPeer.Close()

	// 当前连接是 B，且处于已连接状态。
	c.mu.Lock()
	c.conn = b
	c.mu.Unlock()
	c.state.Store(2)

	// B 上有一条在飞的 QoS1 发布，正等 PUBACK。
	ch := make(chan error, 1)
	c.pendingMu.Lock()
	c.pending[7] = ch
	c.pendingMu.Unlock()

	// 旧连接 A 的残留 goroutine 晚一步醒来，报告 A 已死。
	c.dropConn(a)

	if got := c.state.Load(); got != 2 {
		t.Fatalf("state = %d, want 2: 旧连接的 dropConn 不得重置当前连接的状态", got)
	}
	select {
	case err := <-ch:
		t.Fatalf("旧连接的 dropConn 失败了一条属于当前连接的在飞发布: %v", err)
	default:
	}

	c.mu.Lock()
	cur := c.conn
	c.mu.Unlock()
	if cur != b {
		t.Fatal("旧连接的 dropConn 清空了当前连接")
	}
	if got := c.reconnect.Load(); got != 0 {
		t.Fatalf("reconnect = %d, want 0: 旧连接不得计入一次重连", got)
	}
}
