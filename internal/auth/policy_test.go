package auth

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// fakeClock 注入固定 now，让测试能精确控制时间跳跃。
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *fakeClock) now() time.Time          { return c.t }

// mustParseIP 简化测试里的 IP 构造。
func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad ip %q", s)
	}
	return ip
}

// reqWithRemote 构造测试用 HTTP 请求。
func reqWithRemote(remote, xff string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

// --- IP 提取 ---

func TestSourceIP_NoXFF(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(nil, nil)
	p.now = c.now
	r := reqWithRemote("192.0.2.1:1234", "")
	if got := p.SourceIP(r); got != "192.0.2.1" {
		t.Fatalf("SourceIP = %q, want 192.0.2.1", got)
	}
}

func TestSourceIP_TrustedProxyXFF(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(nil, []string{"10.0.0.1"})
	p.now = c.now
	// 请求来自可信代理 10.0.0.1，XFF 含真实客户端 198.51.100.5
	r := reqWithRemote("10.0.0.1:8080", "198.51.100.5, 10.0.0.1")
	if got := p.SourceIP(r); got != "198.51.100.5" {
		t.Fatalf("SourceIP = %q, want 198.51.100.5", got)
	}
}

func TestSourceIP_UntrustedProxyIgnoresXFF(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(nil, []string{"10.0.0.1"})
	p.now = c.now
	// 请求来自不可信代理 203.0.113.1，XFF 里的地址被忽略。
	r := reqWithRemote("203.0.113.1:1234", "198.51.100.5")
	if got := p.SourceIP(r); got != "203.0.113.1" {
		t.Fatalf("SourceIP = %q, want 203.0.113.1 (XFF ignored)", got)
	}
}

func TestSourceIP_XFFMultipleHops(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(nil, []string{"10.0.0.1", "10.0.0.2"})
	p.now = c.now
	// XFF: client, proxy1, proxy2；从右往左跳过两个可信跳，取 client。
	r := reqWithRemote("10.0.0.2:8080", "198.51.100.5, 10.0.0.1, 10.0.0.2")
	if got := p.SourceIP(r); got != "198.51.100.5" {
		t.Fatalf("SourceIP = %q, want 198.51.100.5", got)
	}
}

func TestSourceIP_CIDRTrustedProxy(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(nil, []string{"10.0.0.0/24"})
	p.now = c.now
	r := reqWithRemote("10.0.0.5:8080", "198.51.100.5")
	if got := p.SourceIP(r); got != "198.51.100.5" {
		t.Fatalf("SourceIP = %q, want 198.51.100.5", got)
	}
}

// --- per-IP 锁定 ---

func TestAllow_PerIPLockout(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{
		MaxAuthFailures: 2,
		LockoutDuration: 15 * time.Minute,
	}, nil)
	p.now = c.now
	ip := "198.51.100.5"

	// 前两次失败，允许。
	for i := 0; i < 2; i++ {
		if ok, _ := p.Allow(ip); !ok {
			t.Fatalf("attempt %d: unexpected lockout", i)
		}
		p.RecordFailure(ip)
	}
	// 第三次前就被锁（达到阈值）。
	if ok, _ := p.Allow(ip); ok {
		t.Fatal("expected lockout after threshold failures")
	}
}

func TestAllow_PerIPLockoutExpires(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{
		MaxAuthFailures: 2,
		LockoutDuration: 15 * time.Minute,
	}, nil)
	p.now = c.now
	ip := "198.51.100.5"

	for i := 0; i < 2; i++ {
		p.RecordFailure(ip)
	}
	if ok, _ := p.Allow(ip); ok {
		t.Fatal("expected lockout")
	}
	// 过 15 分钟后解锁。
	c.advance(15 * time.Minute)
	if ok, _ := p.Allow(ip); !ok {
		t.Fatal("expected unlock after lockout duration")
	}
}

func TestRecordSuccess_ResetsPerIP(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{
		MaxAuthFailures: 2,
		LockoutDuration: 15 * time.Minute,
	}, nil)
	p.now = c.now
	ip := "198.51.100.5"

	for i := 0; i < 2; i++ {
		p.RecordFailure(ip)
	}
	// 成功后重置。
	p.RecordSuccess(ip, [32]byte{})
	if ok, _ := p.Allow(ip); !ok {
		t.Fatal("expected unlock after success")
	}
}

// --- 全局熔断 ---

func TestGlobalCircuitBreaker(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{
		GlobalMaxFailures:     3,
		GlobalFailureWindow:   1 * time.Minute,
		GlobalLockoutDuration: 10 * time.Minute,
	}, nil)
	p.now = c.now
	ips := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}

	// 3 次不同 IP 失败，触发全局熔断。
	for _, ip := range ips {
		p.RecordFailure(ip)
	}
	if ok, _ := p.Allow("198.51.100.99"); ok {
		t.Fatal("expected global circuit open")
	}
}

func TestGlobalCircuitBreaker_Expires(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{
		GlobalMaxFailures:     2,
		GlobalFailureWindow:   1 * time.Minute,
		GlobalLockoutDuration: 5 * time.Minute,
	}, nil)
	p.now = c.now

	p.RecordFailure("198.51.100.1")
	p.RecordFailure("198.51.100.2")
	if ok, _ := p.Allow("198.51.100.3"); ok {
		t.Fatal("expected global circuit open")
	}
	// 熔断过期后恢复。
	c.advance(6 * time.Minute)
	if ok, _ := p.Allow("198.51.100.3"); !ok {
		t.Fatal("expected global circuit closed after expiry")
	}
}

// --- 会话闲置超时 ---

func TestSessionAlive_FirstUse(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(nil, nil)
	p.now = c.now
	var sum [32]byte
	if !p.SessionAlive(sum) {
		t.Fatal("first use should be alive")
	}
}

func TestSessionAlive_WithinWindow(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{IdleTimeout: 30 * time.Minute}, nil)
	p.now = c.now
	var sum [32]byte
	p.SessionAlive(sum)
	c.advance(10 * time.Minute)
	if !p.SessionAlive(sum) {
		t.Fatal("expected alive within window")
	}
}

func TestSessionAlive_Expires(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	p := NewPolicy(&AuthPolicy{IdleTimeout: 30 * time.Minute}, nil)
	p.now = c.now
	var sum [32]byte
	p.SessionAlive(sum)
	c.advance(31 * time.Minute)
	if p.SessionAlive(sum) {
		t.Fatal("expected session expired")
	}
}

// --- 默认值 ---

func TestDefaultThresholds(t *testing.T) {
	p := NewPolicy(nil, nil)
	if p.cfg.maxAuthFailures != 5 {
		t.Fatalf("maxAuthFailures = %d, want 5", p.cfg.maxAuthFailures)
	}
	if p.cfg.lockoutDuration != 15*time.Minute {
		t.Fatalf("lockoutDuration = %v, want 15m", p.cfg.lockoutDuration)
	}
	if p.cfg.idleTimeout != 30*time.Minute {
		t.Fatalf("idleTimeout = %v, want 30m", p.cfg.idleTimeout)
	}
}
