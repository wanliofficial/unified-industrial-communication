// Package auth 的 R-3 认证安全策略扩展：登录失败锁定（按 IP + 全局熔断）、
// 会话闲置超时、可信代理 IP 提取。
//
// 设计要点：
//
//   - **per-IP 锁定**：单 IP 连续认证失败次数达到阈值后，该 IP 在锁定窗口内被
//     拒绝（HTTP 429）。成功认证重置计数。
//   - **全局熔断**：当全系统连续失败次数在窗口内达到阈值后，所有非可信源均被
//     拒绝（HTTP 503），防止分布式绕过单 IP 限制。
//   - **可信代理白名单**：trusted_proxies 里的 IP/CIDR 才被信任转发真实客户端 IP
//     （X-Forwarded-For 从右往左跳过可信跳）；其余请求一律用 RemoteAddr。这
//     是「避免反代后误锁」的关键——没有它，所有用户共享反代 IP，一次失败全锁。
//   - **会话闲置超时**：服务端记住每个 Token 最后成功时间。超过闲置时长未活动，
//     该 Token 被拒（HTTP 401，提示「会话因闲置超时已关闭」）。Bearer Token 本
//     身不过期（无状态），但服务端拒绝超时请求，强迫运维轮换凭据——这正是等保
//     三级「及时关闭不需要的会话」的本义。
//   - **状态生命周期**：policy 状态随进程生命周期，重启后清空（可接受：边缘
//     网关重启是运维事件，重启后 Token 重新激活）。热更新凭据表时保留（合法用
//     户不会误触锁定）。
package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuthPolicy 是北向认证策略的配置视图（出自 config.Server.Auth）。
// 未配置（nil）时 auth.NewPolicy 使用默认值并启用策略。
type AuthPolicy struct {
	TrustedProxies        []string      `json:"trustedProxies,omitempty"`        // 可信代理 IP/CIDR，为空表示不信任任何转发
	MaxAuthFailures       int           `json:"maxAuthFailures,omitempty"`       // 单 IP 失败阈值，默认 5
	LockoutDuration       time.Duration `json:"lockoutDuration,omitempty"`       // 单 IP 锁定时长，默认 15 分钟
	GlobalMaxFailures     int           `json:"globalMaxFailures,omitempty"`     // 全局熔断阈值，默认 20
	GlobalFailureWindow   time.Duration `json:"globalFailureWindow,omitempty"`   // 全局计数窗口，默认 5 分钟
	GlobalLockoutDuration time.Duration `json:"globalLockoutDuration,omitempty"` // 全局熔断时长，默认 15 分钟
	IdleTimeout           time.Duration `json:"idleTimeout,omitempty"`           // 会话闲置超时，默认 30 分钟
}

// PolicyConfig 是内部化后的默认值视图（已把零值替换为默认）。
type policyConfig struct {
	maxAuthFailures       int
	lockoutDuration       time.Duration
	globalMaxFailures     int
	globalFailureWindow   time.Duration
	globalLockoutDuration time.Duration
	idleTimeout           time.Duration
}

func (p *AuthPolicy) toConfig() policyConfig {
	c := policyConfig{
		maxAuthFailures:       5,
		lockoutDuration:       15 * time.Minute,
		globalMaxFailures:     20,
		globalFailureWindow:   5 * time.Minute,
		globalLockoutDuration: 15 * time.Minute,
		idleTimeout:           30 * time.Minute,
	}
	if p == nil {
		return c
	}
	if p.MaxAuthFailures > 0 {
		c.maxAuthFailures = p.MaxAuthFailures
	}
	if p.LockoutDuration > 0 {
		c.lockoutDuration = p.LockoutDuration
	}
	if p.GlobalMaxFailures > 0 {
		c.globalMaxFailures = p.GlobalMaxFailures
	}
	if p.GlobalFailureWindow > 0 {
		c.globalFailureWindow = p.GlobalFailureWindow
	}
	if p.GlobalLockoutDuration > 0 {
		c.globalLockoutDuration = p.GlobalLockoutDuration
	}
	if p.IdleTimeout > 0 {
		c.idleTimeout = p.IdleTimeout
	}
	return c
}

// DefaultPolicy 返回等保三级基线默认策略（所有阈值均为零值时的回退）。
var DefaultPolicy = AuthPolicy{
	MaxAuthFailures:       5,
	LockoutDuration:       15 * time.Minute,
	GlobalMaxFailures:     20,
	GlobalFailureWindow:   5 * time.Minute,
	GlobalLockoutDuration: 15 * time.Minute,
	IdleTimeout:           30 * time.Minute,
}

// ErrIPLocked 是 per-IP 锁定的哨兵错误。
var ErrIPLocked = errors.New("auth: ip locked")

// ErrGlobalCircuitOpen 是全局熔断的哨兵错误。
var ErrGlobalCircuitOpen = errors.New("auth: global circuit breaker open")

// ErrSessionExpired 是会话闲置超时的哨兵错误。
var ErrSessionExpired = errors.New("auth: session expired due to idle timeout")

// failureState 是单 IP 的失败计数状态。
type failureState struct {
	failures    int
	firstFail   time.Time
	lockedUntil time.Time
}

// Policy 是认证安全策略的运行态（独立于 Store，Store 热更新时保留）。
type Policy struct {
	mu      sync.Mutex
	cfg     policyConfig
	trusted []*net.IPNet // 解析后的 trusted_proxies
	// per-IP 失败状态
	perIP map[string]*failureState
	// 全局熔断状态
	globalCount     int
	globalWindowEnd time.Time
	globalLockedEnd time.Time
	// 会话表：sha256(token) -> 最后成功时间
	sessions map[[32]byte]time.Time
	// 时钟（测试注入用）
	now func() time.Time
}

// defaultConfig 返回默认策略配置（等保三级基线）。
func defaultConfig() policyConfig {
	return policyConfig{
		maxAuthFailures:       5,
		lockoutDuration:       15 * time.Minute,
		globalMaxFailures:     20,
		globalFailureWindow:   5 * time.Minute,
		globalLockoutDuration: 15 * time.Minute,
		idleTimeout:           30 * time.Minute,
	}
}

// NewPolicy 构造认证安全策略。cfg 为 nil 时使用默认值并启用策略（所有默认
// 参数满足等保三级基线）。trustedProxies 为空表示不信任任何转发。
func NewPolicy(cfg *AuthPolicy, trustedProxies []string) *Policy {
	c := defaultConfig()
	if cfg != nil {
		c = cfg.toConfig()
	}
	p := &Policy{
		cfg:      c,
		perIP:    make(map[string]*failureState),
		sessions: make(map[[32]byte]time.Time),
		now:      time.Now,
	}
	// 解析可信代理 CIDR。IPv4 用 /32、IPv6 用 /128 的主机掩码。
	for _, s := range trustedProxies {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			_, n, err := net.ParseCIDR(s)
			if err == nil {
				p.trusted = append(p.trusted, n)
			}
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			p.trusted = append(p.trusted, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)})
		} else {
			p.trusted = append(p.trusted, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
		}
	}
	return p
}

// isTrusted 报告 ip 是否在可信代理列表中（用于判断是否可从 XFF 提取真实 IP）。
func (p *Policy) isTrusted(ip net.IP) bool {
	for _, n := range p.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SourceIP 从请求提取客户端真实 IP。仅当请求直接来自可信代理时才解析
// X-Forwarded-For；其余一律用 RemoteAddr（去端口）。XFF 解析从右往左跳
// 过可信跳，防止客户端伪造 XFF 绕过锁定。
func (p *Policy) SourceIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	ip := net.ParseIP(remote)
	if ip == nil {
		return r.RemoteAddr
	}
	if !p.isTrusted(ip) {
		return remote
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	// 从最右（最近的可信跳）往左找第一个不在可信列表里的地址。
	for i := len(parts) - 1; i >= 0; i-- {
		cand := strings.TrimSpace(parts[i])
		ip := net.ParseIP(cand)
		if ip == nil {
			continue
		}
		if p.isTrusted(ip) {
			continue
		}
		return ip.String()
	}
	// 全链可信（极端情况），回退到 RemoteAddr。
	return remote
}

// Allow 在认证前调用：检查该 IP 是否被允许尝试认证。
//   - per-IP 锁定中 → (false, "ip locked")
//   - 全局熔断中   → (false, "global circuit open")
//   - 允许尝试     → (true, "")
func (p *Policy) Allow(ip string) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	// 全局熔断。
	if now.Before(p.globalLockedEnd) {
		return false, "global circuit open"
	}
	// 全局窗口过期则重置。
	if now.After(p.globalWindowEnd) {
		p.globalCount = 0
		p.globalWindowEnd = now.Add(p.cfg.globalFailureWindow)
	}
	// per-IP 锁定。
	st, ok := p.perIP[ip]
	if ok && !st.lockedUntil.IsZero() && now.Before(st.lockedUntil) {
		return false, "ip locked"
	}
	// 锁定已过期（或未设定锁定）时清理旧记录，让计数重新开始。
	if ok && !st.lockedUntil.IsZero() && now.After(st.lockedUntil) {
		delete(p.perIP, ip)
	}
	return true, ""
}

// RecordFailure 认证失败后调用。可能触发 per-IP 锁定或全局熔断。
func (p *Policy) RecordFailure(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	// 全局窗口过期则重置。
	if now.After(p.globalWindowEnd) {
		p.globalCount = 0
		p.globalWindowEnd = now.Add(p.cfg.globalFailureWindow)
	}
	p.globalCount++
	if p.globalCount >= p.cfg.globalMaxFailures {
		p.globalLockedEnd = now.Add(p.cfg.globalLockoutDuration)
		return
	}
	// per-IP 计数。
	st, ok := p.perIP[ip]
	if !ok || now.After(st.firstFail.Add(p.cfg.lockoutDuration*2)) {
		p.perIP[ip] = &failureState{
			failures:  1,
			firstFail: now,
		}
		return
	}
	st.failures++
	if st.failures >= p.cfg.maxAuthFailures {
		st.lockedUntil = now.Add(p.cfg.lockoutDuration)
	}
}

// RecordSuccess 认证成功后调用。重置 per-IP 失败计数，更新会话 lastSeen。
func (p *Policy) RecordSuccess(ip string, tokenSum [32]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.perIP, ip)
	p.sessions[tokenSum] = p.now()
}

// SessionAlive 报告该 Token 会话是否仍在闲置窗口内。首次见到（无记录）返回
// true 并登记；超过闲置时长返回 false。重启后状态清空，Token 重新激活。
func (p *Policy) SessionAlive(tokenSum [32]byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if t, ok := p.sessions[tokenSum]; ok {
		return now.Sub(t) <= p.cfg.idleTimeout
	}
	// 首次使用：接受并登记。
	p.sessions[tokenSum] = now
	return true
}

// DumpState 返回内部状态的快照（仅测试/诊断用，不保证线程安全）。
func (p *Policy) DumpState() (lockedIPs int, globalLocked bool, sessions int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.perIP), p.now().Before(p.globalLockedEnd), len(p.sessions)
}

// String 返回 policy 运行态摘要（供诊断与审计）。调用方必须传一个显式的
// now 函数，避免 String 自身触发 now() 调用与锁的潜在问题。
func (p *Policy) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("policy{lockedIPs=%d, globalLocked=%v, sessions=%d}",
		len(p.perIP), p.now().Before(p.globalLockedEnd), len(p.sessions))
}
