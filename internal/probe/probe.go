// Package probe 提供对配置内网络端点的最小副作用连通性探测。
//
// 它与驱动层的 Conn.Probe 分工不同：驱动 Probe 走真实协议（读一个点、握手），
// 回答「设备应用层是否正常」；本包只做 TCP 握手或 UDP 地址解析，回答「到端点
// 的**网络路径**通不通、RTT 多少」。当一台设备显示「连接失败」时，先用本包
// 区分故障层：TCP 连不上 → 路由/端口/防火墙问题；TCP 连得上但驱动读不到 →
// 协议参数或设备应用层问题。这是「为什么连不上」最常见的第一个分叉。
//
// 安全边界：只接受配置里已声明的通道地址，不接受任意主机——因此不存在 SSRF
// 面。探测是显式的、由运维触发的诊断动作（RBAC 与诊断包同级），对 TCP 端点
// 只做 connect 然后立即 close，不发送任何协议字节。需要说明的代价：部分设备
// （串口服务器、某些 PLC）只允许一个 TCP 会话，探测会短暂地多开一条连接——
// 这与运维手动 `telnet host port` 等价，风险有界且由人主动触发，故可接受。
package probe

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"uic/internal/model"
)

// 状态枚举。用显式字符串而不是裸 bool，是因为「探测手段无法判定」与「探测
// 失败」是两回事：UDP 无连接，拨号成功不代表对端可达，把它混进 reachable
// 会误导看板，故单列 unverified。
const (
	// StatusOK：TCP 握手成功。
	StatusOK = "ok"
	// StatusFail：TCP 连接失败（端口不可达、超时、拒绝等）。
	StatusFail = "fail"
	// StatusUnverified：探测手段无法判定可达性（UDP 无连接握手）。
	StatusUnverified = "unverified"
	// StatusUnsupported：传输方式不是网络端点（串口 / 内存）。
	StatusUnsupported = "unsupported"
)

// 探测并发上限：一个站点可能有几十上百条通道，串行探测会让最坏情况（每条都
// 超时）长达分钟级；无界并发又会瞬时打满本地端口。取一个适中值。
const defaultConcurrency = 16

// Result 是单个端点的探测结果。
type Result struct {
	Channel   string `json:"channel"`
	Protocol  string `json:"protocol,omitempty"`
	Transport string `json:"transport"`
	Address   string `json:"address,omitempty"`
	// Status 取值见上面的状态枚举。
	Status string `json:"status"`
	// Reachable 仅 StatusOK 为 true。unverified 时保持 false：UDP 不能确认
	// 可达，把它当可达会让上层误判。
	Reachable bool `json:"reachable"`
	// RTTMs 是 TCP 握手往返耗时（毫秒）；仅 StatusOK 有意义。
	RTTMs int64 `json:"rttMs,omitempty"`
	// Error 是失败/不可判定时的人类可读原因。
	Error string `json:"error,omitempty"`
}

// Options 控制探测行为。零值可用。
type Options struct {
	// Timeout 是每个端点探测的超时；<=0 用 defaultTimeout。
	Timeout time.Duration
	// Concurrency 是并发探测上限；<=0 用 defaultConcurrency。
	Concurrency int
}

const defaultTimeout = 2 * time.Second

// Probe 对给定通道做连通性探测，返回与输入同序的结果（便于调用方对照）。
//
// 不修改任何状态、不建立持久连接：每条 TCP 探测 connect 后立即 close。串行/
// 并发由内部 worker 池管理，整体受 ctx 与各端点 Timeout 约束。
func Probe(ctx context.Context, channels []model.Channel, opt Options) []Result {
	if opt.Timeout <= 0 {
		opt.Timeout = defaultTimeout
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = defaultConcurrency
	}
	results := make([]Result, len(channels))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opt.Concurrency)
	for i := range channels {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeOne(ctx, channels[i], opt.Timeout)
		}()
	}
	// 等待全部 worker 完成或 ctx 取消。ctx 取消时各 worker 的探测会各自超时
	// 返回，结果切片里对应位置保留「超时/失败」的部分结果，不会丢条目。
	wg.Wait()
	return results
}

// probeOne 探测单个通道。所有判定都走这条，保证语义集中在一处。
func probeOne(ctx context.Context, ch model.Channel, timeout time.Duration) Result {
	r := Result{
		Channel:   ch.ID,
		Protocol:  ch.Protocol,
		Transport: ch.Transport.Kind,
		Address:   strings.TrimSpace(ch.Transport.Address),
	}
	switch ch.Transport.Kind {
	case "tcp":
		return probeTCP(ctx, r, timeout)
	case "udp":
		return probeUDP(ctx, r)
	default:
		r.Status = StatusUnsupported
		r.Error = "transport " + ch.Transport.Kind + " is not a network endpoint"
		return r
	}
}

// probeTCP 做真正的 TCP 连接探测：握手成功即 close，测 RTT。入参 r 已填好身份
// 字段，这里只补状态与结果。
func probeTCP(ctx context.Context, r Result, timeout time.Duration) Result {
	if r.Address == "" {
		r.Status = StatusFail
		r.Error = "transport.address required"
		return r
	}
	if !hasPort(r.Address) {
		// 不做端口猜测：往错误端口探测会把「配置写错」误报成「网络不可达」。
		r.Status = StatusFail
		r.Error = "address " + r.Address + " has no port; add :port to transport.address"
		return r
	}
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", r.Address)
	if err != nil {
		r.Status = StatusFail
		r.Error = err.Error()
		return r
	}
	_ = conn.Close()
	r.Status = StatusOK
	r.Reachable = true
	r.RTTMs = time.Since(start).Milliseconds()
	return r
}

// probeUDP 诚实处理无连接语义：connected UDP 拨号成功只表示地址可解析、本地
// 套接字可建，并不代表对端可达（没有握手）。因此标 unverified 而非 reachable。
func probeUDP(ctx context.Context, r Result) Result {
	if r.Address == "" {
		r.Status = StatusFail
		r.Error = "transport.address required"
		return r
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "udp", r.Address)
	if err != nil {
		r.Status = StatusFail
		r.Error = err.Error()
		return r
	}
	_ = conn.Close()
	r.Status = StatusUnverified
	r.Error = "UDP is connectionless: dial only confirms the address resolves; " +
		"actual reachability needs an app-layer exchange"
	return r
}

// hasPort 报告地址是否带端口。host:port 或 [v6]:port 都算；裸 host / 裸 IPv4
// 不算。用 net.SplitHostPort 而非手写字符串判断，避免把 IPv6 地址误判。
func hasPort(addr string) bool {
	_, _, err := net.SplitHostPort(addr)
	return err == nil
}
