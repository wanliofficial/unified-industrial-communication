package probe

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"uic/internal/model"
)

// tcpChan 构造一条 tcp 通道。
func tcpChan(id, addr string) model.Channel {
	return model.Channel{ID: id, Protocol: "modbus-tcp",
		Transport: model.Transport{Kind: "tcp", Address: addr}}
}

// TestProbeTCPReachable：真实监听一个 TCP 端口，探测必须 ok、reachable、RTT>=0。
func TestProbeTCPReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// 后台立刻接受并关闭连接：模拟一个「端口开着」的端点。
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	channels := []model.Channel{tcpChan("ch1", ln.Addr().String())}
	res := Probe(context.Background(), channels, Options{Timeout: time.Second})
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1", len(res))
	}
	r := res[0]
	if r.Status != StatusOK || !r.Reachable {
		t.Fatalf("status=%q reachable=%v, want ok/true (err=%q)", r.Status, r.Reachable, r.Error)
	}
	if r.RTTMs < 0 {
		t.Fatalf("RTTMs = %d, want >= 0", r.RTTMs)
	}
	if r.Channel != "ch1" {
		t.Fatalf("Channel = %q, want ch1", r.Channel)
	}
}

// TestProbeTCPUnreachable：一个没人监听的端口，探测必须 fail 且带错误。
func TestProbeTCPUnreachable(t *testing.T) {
	// 拿到一个端口再立刻关掉：端口此刻无人监听。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	res := Probe(context.Background(), []model.Channel{tcpChan("ch2", addr)}, Options{Timeout: time.Second})
	r := res[0]
	if r.Status != StatusFail || r.Reachable {
		t.Fatalf("status=%q reachable=%v, want fail/false", r.Status, r.Reachable)
	}
	if r.Error == "" {
		t.Fatal("fail 必须带错误原因")
	}
}

// TestProbeTCPTimeout：用一个会「连得上但迟迟不握手」的地址不应误判——这里用
// 不可路由地址验证超时路径返回 fail 而非挂起。
func TestProbeTCPTimeout(t *testing.T) {
	// TEST-NET-1（192.0.2.0/24，RFC 5737 文档保留地址）不可路由，连接会一直
	// 等到超时。用很短的超时，确保测试快速返回且判定为 fail。
	res := Probe(context.Background(), []model.Channel{tcpChan("ch3", "192.0.2.1:502")},
		Options{Timeout: 300 * time.Millisecond})
	r := res[0]
	if r.Status != StatusFail {
		t.Fatalf("status=%q, want fail (timeout)", r.Status)
	}
}

// TestProbeTCPNoPort：地址缺端口必须报配置错误，而不是去猜端口。
func TestProbeTCPNoPort(t *testing.T) {
	res := Probe(context.Background(), []model.Channel{tcpChan("ch4", "127.0.0.1")},
		Options{Timeout: time.Second})
	r := res[0]
	if r.Status != StatusFail {
		t.Fatalf("status=%q, want fail", r.Status)
	}
	if !strings.Contains(r.Error, "no port") {
		t.Fatalf("error=%q, want mention of missing port", r.Error)
	}
}

// TestProbeUDPUnverified：UDP 无连接，拨号成功只能确认地址可解析，必须标
// unverified 而非 reachable——否则看板会把「能解析」误当「可达」。
func TestProbeUDPUnverified(t *testing.T) {
	// 127.0.0.1:1 上不会有 UDP 服务，但 UDP 拨号不需要对端在线，依然成功。
	ch := model.Channel{ID: "u1", Protocol: "bacnet-ip",
		Transport: model.Transport{Kind: "udp", Address: "127.0.0.1:47808"}}
	res := Probe(context.Background(), []model.Channel{ch}, Options{Timeout: time.Second})
	r := res[0]
	if r.Status != StatusUnverified {
		t.Fatalf("status=%q, want unverified", r.Status)
	}
	if r.Reachable {
		t.Fatal("UDP 不得标 reachable：无握手，无法确认对端可达")
	}
	if r.Error == "" {
		t.Fatal("unverified 必须解释为什么无法确认可达性")
	}
}

// TestProbeUnsupported：串口 / 内存不是网络端点，必须 unsupported。
func TestProbeUnsupported(t *testing.T) {
	cases := []model.Channel{
		{ID: "s1", Protocol: "modbus-rtu", Transport: model.Transport{Kind: "serial", Port: "/dev/ttyUSB0"}},
		{ID: "m1", Protocol: "virtual", Transport: model.Transport{Kind: "memory"}},
	}
	res := Probe(context.Background(), cases, Options{Timeout: time.Second})
	for i, r := range res {
		if r.Status != StatusUnsupported {
			t.Fatalf("case %d (%s): status=%q, want unsupported", i, cases[i].ID, r.Status)
		}
	}
}

// TestProbePreservesOrder：并发探测后结果顺序必须与输入一致，调用方才能
// 逐条对照。这是并发写各自下标 + 主协程等待的联合保证。
func TestProbePreservesOrder(t *testing.T) {
	var chans []model.Channel
	var want []string
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen %d: %v", i, err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		id := "ch" + string(rune('a'+i))
		chans = append(chans, tcpChan(id, ln.Addr().String()))
		want = append(want, id)
	}
	res := Probe(context.Background(), chans, Options{Timeout: time.Second, Concurrency: 4})
	if len(res) != len(want) {
		t.Fatalf("results=%d want %d", len(res), len(want))
	}
	for i := range res {
		if res[i].Channel != want[i] {
			t.Fatalf("result[%d].Channel=%q, want %q (顺序被打乱)", i, res[i].Channel, want[i])
		}
	}
}

// TestProbeContextCancel：ctx 取消时 Probe 必须返回（不挂起），结果条目数
// 与输入一致。
func TestProbeContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	res := Probe(ctx, []model.Channel{tcpChan("c1", "192.0.2.1:502")}, Options{Timeout: 5 * time.Second})
	if len(res) != 1 {
		t.Fatalf("results=%d, want 1（ctx 取消也必须返回完整切片）", len(res))
	}
}

// TestProbeConcurrencyBounded：并发上限生效——用大量不可达端点 + 短超时，
// 验证在「数量/并发 * 超时」量级内完成，而不是串行累加。这是软性断言，主要
// 防回归到串行实现。
func TestProbeConcurrencyBounded(t *testing.T) {
	const n = 32
	var chans []model.Channel
	for i := 0; i < n; i++ {
		chans = append(chans, tcpChan("b"+string(rune('a'+i%26)), "192.0.2.1:502"))
	}
	start := time.Now()
	res := Probe(context.Background(), chans, Options{Timeout: 200 * time.Millisecond, Concurrency: 8})
	elapsed := time.Since(start)
	if len(res) != n {
		t.Fatalf("results=%d, want %d", len(res), n)
	}
	// 串行需要 ~n*200ms=6.4s；并发 8 应远小于此（给 2s 宽松上界）。
	if elapsed > 2*time.Second {
		t.Fatalf("32 个端点并发探测耗时 %v，疑似退化为串行", elapsed)
	}
}
