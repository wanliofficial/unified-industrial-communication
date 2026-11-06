package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"uic/internal/rbac"
)

// tok 造一个长度可控的 Token。刻意不用「重复同一个字符」这种长度边界的
// 值去撞 weakTokens 黑名单，否则测的就不是长度规则了。
func tok(n int, ch byte) string {
	if n <= 0 {
		return ""
	}
	base := "Zx9Qm2Lp7Rt4Vb8Nc3Hf6Kd1Sg5Wj0Ya"
	out := make([]byte, n)
	for i := range out {
		out[i] = base[i%len(base)]
	}
	if ch != 0 {
		out[n-1] = ch
	}
	return string(out)
}

// TestOpenModeWhenNoCredentials 验证「没配凭据」时是开放模式，而不是造一个
// 默认口令。默认口令永远不会被改，开放模式至少会出现在启动日志里。
func TestOpenModeWhenNoCredentials(t *testing.T) {
	s, err := NewStore(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Open() {
		t.Fatal("empty store is not open")
	}
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0", s.Len())
	}
	p, ok := s.Authenticate("")
	if !ok || !p.Anonymous {
		t.Fatalf("open mode rejected anonymous: %+v %v", p, ok)
	}
	if p.Role != rbac.RoleAdmin {
		t.Fatalf("open mode role = %s, want admin", p.Role)
	}
	// 带任何 Token 也接受（认证被关闭）。
	if _, ok := s.Authenticate("whatever"); !ok {
		t.Fatal("open mode rejected a random token")
	}
}

// TestNilStoreIsOpen 保证未初始化的凭据表不会 panic 或误拒。
func TestNilStoreIsOpen(t *testing.T) {
	var s *Store
	if !s.Open() {
		t.Fatal("nil store should be open")
	}
	if s.Len() != 0 {
		t.Fatal("nil store should have no users")
	}
	if _, ok := s.Authenticate("x"); !ok {
		t.Fatal("nil store rejected a token")
	}
}

func TestAuthenticateMatchingToken(t *testing.T) {
	s, err := NewStore([]User{{Name: "alice", Role: rbac.RoleOperator, Token: tok(24, 'a')}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Open() {
		t.Fatal("store with a user reported open")
	}
	p, ok := s.Authenticate(tok(24, 'a'))
	if !ok {
		t.Fatal("valid token rejected")
	}
	if p.User != "alice" || p.Role != rbac.RoleOperator || p.Anonymous {
		t.Fatalf("principal = %+v", p)
	}
}

// TestAuthenticateRejects 覆盖各种错误 Token。
func TestAuthenticateRejects(t *testing.T) {
	good := tok(24, 'a')
	s, err := NewStore([]User{{Name: "alice", Role: rbac.RoleViewer, Token: good}}, "")
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"",                            // 空
		" ",                           // 空白
		good[:len(good)-1],            // 少一位
		good + "x",                    // 多一位
		strings.ToUpper(good),         // 大小写不同
		good[:23] + string(byte('b')), // 只差最后一个字节
	}
	for _, tok := range bad {
		if _, ok := s.Authenticate(tok); ok {
			t.Fatalf("Authenticate(%q) accepted", tok)
		}
	}
}

// TestWhitespaceIsTrimmed 验证复制粘贴带换行的 Token 也能用。
func TestWhitespaceIsTrimmed(t *testing.T) {
	good := tok(24, 'a')
	s, err := NewStore([]User{{Name: "a", Role: rbac.RoleViewer, Token: good}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate("  " + good + "\n"); !ok {
		t.Fatal("token with surrounding whitespace rejected")
	}
}

// TestSha256Credential 验证只写哈希的配置能登录，且明文不被接受。
func TestSha256Credential(t *testing.T) {
	plain := tok(24, 'z')
	sum := sha256.Sum256([]byte(plain))
	s, err := NewStore([]User{{
		Name: "svc", Role: rbac.RoleAdmin, Sha256: hex.EncodeToString(sum[:]),
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate(plain); !ok {
		t.Fatal("sha256-only credential rejected the real token")
	}
	// 表里只有哈希，明文当然不该被当作「另一个用户」。
	if _, ok := s.Authenticate("another-value-entirely"); ok {
		t.Fatal("unknown token accepted")
	}
}

func TestSha256MustBeHex(t *testing.T) {
	if _, err := NewStore([]User{{Name: "x", Role: rbac.RoleViewer, Sha256: "nothex"}}, ""); err == nil {
		t.Fatal("bad hex accepted")
	}
	if _, err := NewStore([]User{{Name: "x", Role: rbac.RoleViewer, Sha256: strings.Repeat("a", 63)}}, ""); err == nil {
		t.Fatal("short hash accepted")
	}
}

// TestLegacyTokenBecomesAdmin 是老配置的升级路径：server.token 曾经是
// 「一个凭据、全权」，升级后必须还是这样。
func TestLegacyTokenBecomesAdmin(t *testing.T) {
	good := tok(24, 'l')
	s, err := NewStore(nil, good)
	if err != nil {
		t.Fatal(err)
	}
	if s.Open() {
		t.Fatal("legacy token did not count as a credential")
	}
	p, ok := s.Authenticate(good)
	if !ok {
		t.Fatal("legacy token rejected")
	}
	if p.User != "api" || p.Role != rbac.RoleAdmin {
		t.Fatalf("legacy principal = %+v", p)
	}
}

// TestLegacyTokenDuplicatedInUsers 验证同一个 Token 出现在两处时不会报错
// （否则升级时会炸在启动阶段）。
func TestLegacyTokenDuplicatedInUsers(t *testing.T) {
	good := tok(24, 'd')
	s, err := NewStore([]User{{Name: "alice", Role: rbac.RoleViewer, Token: good}}, good)
	if err != nil {
		t.Fatalf("duplicate legacy token: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("len = %d, want 1", s.Len())
	}
}

func TestRejectDuplicateNames(t *testing.T) {
	if _, err := NewStore([]User{
		{Name: "a", Role: rbac.RoleViewer, Token: tok(24, 'a')},
		{Name: "a", Role: rbac.RoleAdmin, Token: tok(24, 'b')},
	}, ""); err == nil {
		t.Fatal("duplicate names accepted")
	}
}

func TestRejectDuplicateTokens(t *testing.T) {
	same := tok(24, 'c')
	if _, err := NewStore([]User{
		{Name: "a", Role: rbac.RoleViewer, Token: same},
		{Name: "b", Role: rbac.RoleAdmin, Token: same},
	}, ""); err == nil {
		t.Fatal("duplicate tokens accepted: two names would share one identity")
	}
}

func TestRejectUnknownRole(t *testing.T) {
	if _, err := NewStore([]User{{Name: "a", Role: "root", Token: tok(24, 'a')}}, ""); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestRejectMissingName(t *testing.T) {
	if _, err := NewStore([]User{{Role: rbac.RoleViewer, Token: tok(24, 'a')}}, ""); err == nil {
		t.Fatal("missing name accepted")
	}
}

func TestRejectEmptyCredential(t *testing.T) {
	if _, err := NewStore([]User{{Name: "a", Role: rbac.RoleViewer}}, ""); err == nil {
		t.Fatal("user without token or hash accepted")
	}
}

// TestValidateToken 覆盖强度规则。
func TestValidateToken(t *testing.T) {
	if err := ValidateToken(tok(MinTokenLen, 'a')); err != nil {
		t.Fatalf("min-length token rejected: %v", err)
	}
	if err := ValidateToken(tok(MinTokenLen-1, 'a')); err == nil {
		t.Fatal("short token accepted")
	}
	if err := ValidateToken(""); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := ValidateToken(tok(513, 'a')); err == nil {
		t.Fatal("oversized token accepted")
	}
	if err := ValidateToken("abc def ghi jkl mno"); err == nil {
		t.Fatal("token with whitespace accepted")
	}
	// 换行会被 HTTP 头截断，必须在配置期拒绝。
	if err := ValidateToken(tok(20, 'a') + "\n"); err == nil {
		t.Fatal("token with newline accepted")
	}
}

// TestWeakTokensRejected 防「照着文档抄一个 demo 口令上生产」。
func TestWeakTokensRejected(t *testing.T) {
	for _, t0 := range []string{
		"changeme", "changeme12345678", "password", "1234567890123456",
		"admin123456789012", "uic", "uictoken", "testtoken",
		"0000000000000000", "aaaaaaaaaaaaaaaa", "PASSWORD", "Changeme12345678",
	} {
		if err := ValidateToken(t0); err == nil {
			t.Fatalf("weak token %q accepted", t0)
		}
	}
}

// TestUsersAreSorted 保证枚举顺序稳定（日志、UI、审计都依赖它）。
func TestUsersAreSorted(t *testing.T) {
	s, err := NewStore([]User{
		{Name: "zoe", Role: rbac.RoleViewer, Token: tok(24, 'a')},
		{Name: "alice", Role: rbac.RoleAdmin, Token: tok(24, 'b')},
		{Name: "mike", Role: rbac.RoleOperator, Token: tok(24, 'c')},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	users := s.Users()
	want := []string{"alice", "mike", "zoe"}
	if len(users) != len(want) {
		t.Fatalf("users = %+v", users)
	}
	for i, w := range want {
		if users[i].User != w {
			t.Fatalf("users[%d] = %s, want %s", i, users[i].User, w)
		}
	}
}

// TestUsersNeverExposeTokens 是安全红线：凭据枚举接口不能带出 Token 或哈希。
func TestUsersNeverExposeTokens(t *testing.T) {
	s, err := NewStore([]User{{Name: "a", Role: rbac.RoleAdmin, Token: tok(24, 'a')}}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Users() {
		if strings.Contains(strings.ToLower(p.User), "a") && p.User != "a" {
			t.Fatalf("unexpected user name %q", p.User)
		}
	}
}

// TestReloadableSwapsAtomically 是热更新的核心保证：HTTP 层持有的引用
// 不变，但认证结果立刻跟着新表走。
func TestReloadableSwapsAtomically(t *testing.T) {
	first, err := NewStore([]User{{Name: "old", Role: rbac.RoleAdmin, Token: tok(24, 'a')}}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := NewReloadable(first)
	if _, ok := r.Authenticate(tok(24, 'a')); !ok {
		t.Fatal("first store rejected")
	}

	second, err := NewStore([]User{{Name: "new", Role: rbac.RoleViewer, Token: tok(24, 'b')}}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Set(second)

	p, ok := r.Authenticate(tok(24, 'b'))
	if !ok || p.User != "new" {
		t.Fatalf("after swap = %+v %v", p, ok)
	}
	if _, ok := r.Authenticate(tok(24, 'a')); ok {
		t.Fatal("removed user still authenticates after swap")
	}
	if r.Open() {
		t.Fatal("reloadable reported open with a user")
	}
	if len(r.Users()) != 1 {
		t.Fatalf("users = %+v", r.Users())
	}
}

// TestReloadableCanSwitchToOpen 验证「删掉所有用户」= 关闭认证，而不是锁死。
func TestReloadableCanSwitchToOpen(t *testing.T) {
	first, err := NewStore([]User{{Name: "a", Role: rbac.RoleAdmin, Token: tok(24, 'a')}}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := NewReloadable(first)

	empty, err := NewStore(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Set(empty)
	if !r.Open() {
		t.Fatal("reloadable did not follow the new store into open mode")
	}
}

// TestNilReloadableDoesNotPanic 保证构造顺序有误时不会崩。
func TestNilReloadableDoesNotPanic(t *testing.T) {
	var r *Reloadable
	if r.Store() != nil {
		t.Fatal("nil reloadable returned a store")
	}
	if !r.Open() {
		t.Fatal("nil reloadable should behave as open")
	}
	if r.Len() != 0 || r.Users() != nil {
		t.Fatal("nil reloadable should expose nothing")
	}
	if _, ok := r.Authenticate("x"); !ok {
		t.Fatal("nil reloadable rejected a token")
	}
}
