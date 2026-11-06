// Package auth 把「一个 Bearer Token」变成「一个可判定的身份」。
//
// 它与 rbac 分工明确：auth 只回答「你是谁」，rbac 只回答「你能不能」。
// 合在一起写会让两件事互相牵连——比如为了做审计而把角色塞进 Token 里，
// 结果改角色要重发 Token。
//
// 设计要点：
//
//   - **Token 先哈希再比对**。直接比较明文字符串会让「长度不同」比「长度
//     相同但内容不同」快一点，长期观测能还原出 Token 长度；先做 SHA-256
//     再定长比较就把这条侧信道抹平了。内存里也不留明文。
//   - **没有配置任何凭据**时不创建「万能 Token」，而是标记 Open 模式。
//     由启动流程打印醒目警告。这比偷偷生成一个默认口令好：默认口令不会
//     被改，Open 模式至少会被日志看见。
//   - **身份可以来自平台账号**。Token 里不带角色，角色在配置里；这样改
//     权限不需要重发凭据。
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"uic/internal/rbac"
)

// Principal 是已认证的调用方。User 用于审计，Role 用于授权。
type Principal struct {
	User string    `json:"user"`
	Role rbac.Role `json:"role"`
	// Anonymous 表示这是「未配置任何凭据」时的开放访问。审计里必须能
	// 区分「有身份的人做的」与「谁都能做的」。
	Anonymous bool `json:"anonymous,omitempty"`
}

// Anonymous 是开放模式下的身份。
var Anonymous = Principal{User: "anonymous", Role: rbac.RoleAdmin, Anonymous: true}

// User 是一条配置里的凭据。
//
// Token 与 Sha256 二选一：给出 Sha256 时表示「配置文件里只有哈希」，此时
// 明文 Token 不存在于任何地方（除了部署时的那次输出）。
type User struct {
	Name   string
	Role   rbac.Role
	Token  string
	Sha256 string
}

type entry struct {
	principal Principal
	sum       [32]byte
}

// Reloadable 是一份可以在运行期整体替换的凭据表。
//
// 为什么需要它：配置热更新要求「删掉的用户立刻失效」，而 HTTP 层在启动时
// 就拿走了凭据表。如果直接把 *Store 存进 HTTP 层，热更新只能换掉 runtime
// 手里那个指针——HTTP 层仍然对着旧表认证，于是「配置说删了、进程还能用」。
//
// 替代方案是让 HTTP 层每次请求回调 runtime 取表，但那会把「HTTP 层怎么拿
// 身份」变成一个运行时依赖，测试里必须造一个 runtime。这里把「可替换」做成
// 凭据表自身的属性：调用方只看到 Authenticate/Open/Users，替换对它是透明的。
type Reloadable struct{ v atomic.Pointer[Store] }

// NewReloadable 包装一份凭据表。
func NewReloadable(st *Store) *Reloadable {
	r := &Reloadable{}
	r.v.Store(st)
	return r
}

// Set 原子替换凭据表。已在进行中的请求继续用旧表完成，新请求立刻用新表。
func (r *Reloadable) Set(st *Store) { r.v.Store(st) }

// Store 返回当前凭据表。
func (r *Reloadable) Store() *Store {
	if r == nil {
		return nil
	}
	return r.v.Load()
}

// Authenticate 见 Store.Authenticate。
func (r *Reloadable) Authenticate(token string) (Principal, bool) {
	return r.Store().Authenticate(token)
}

// Open 见 Store.Open。
func (r *Reloadable) Open() bool { return r.Store().Open() }

// Len 见 Store.Len。
func (r *Reloadable) Len() int { return r.Store().Len() }

// Users 见 Store.Users。
func (r *Reloadable) Users() []Principal { return r.Store().Users() }

// Store 是凭据表。构造后不可变，因此并发安全。
type Store struct {
	entries []entry
	// open 表示没有任何凭据：认证被显式关闭。
	open bool
}

// MinTokenLen 是 Token 的最小长度。12 太短（8 位随机 + 常见词根就能撞），
// 16 位 base64url 约 96 bit 熵，边缘设备上也没有计算成本问题。
const MinTokenLen = 16

// weakTokens 是必须拒绝的常见值。不是为了防暴力破解（长度已经够了），
// 而是防「照着文档抄一个 demo Token 上生产」——这类值在扫描器里是第一
// 优先级字典。
var weakTokens = map[string]bool{
	"changeme": true, "changeme12345678": true, "password": true,
	"1234567890123456": true, "admin123456789012": true,
	"uic": true, "uictoken": true, "testtoken": true,
	"0000000000000000": true, "aaaaaaaaaaaaaaaa": true,
}

// NewStore 构造凭据表。legacyToken 是 server.token（历史字段），
// 它被当作一个名为 "api" 的 admin 凭据——老配置升级后不能变成匿名。
func NewStore(users []User, legacyToken string) (*Store, error) {
	s := &Store{}
	seenName := map[string]bool{}
	seenToken := map[string]bool{}

	add := func(u User) error {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return errors.New("auth: user name is required")
		}
		if seenName[name] {
			return fmt.Errorf("auth: duplicate user name %q", name)
		}
		seenName[name] = true
		if !rbac.Valid(u.Role) {
			return fmt.Errorf("auth: user %q: unknown role %q", name, u.Role)
		}
		var sum [32]byte
		switch {
		case u.Sha256 != "":
			raw, err := hex.DecodeString(strings.TrimSpace(u.Sha256))
			if err != nil || len(raw) != 32 {
				return fmt.Errorf("auth: user %q: tokenSha256 must be 64 hex characters", name)
			}
			copy(sum[:], raw)
		case u.Token != "":
			token := strings.TrimSpace(u.Token)
			if err := ValidateToken(token); err != nil {
				return fmt.Errorf("auth: user %q: %w", name, err)
			}
			if seenToken[token] {
				return fmt.Errorf("auth: user %q: duplicate token", name)
			}
			seenToken[token] = true
			sum = sha256.Sum256([]byte(token))
		default:
			return fmt.Errorf("auth: user %q: one of token or tokenSha256 is required", name)
		}
		s.entries = append(s.entries, entry{
			principal: Principal{User: name, Role: u.Role},
			sum:       sum,
		})
		return nil
	}

	for _, u := range users {
		if err := add(u); err != nil {
			return nil, err
		}
	}
	if t := strings.TrimSpace(legacyToken); t != "" {
		if !seenToken[t] {
			if err := add(User{Name: "api", Role: rbac.RoleAdmin, Token: t}); err != nil {
				return nil, err
			}
		}
	}
	if len(s.entries) == 0 {
		s.open = true
	}
	// 按用户名排序：Authenticate 的耗时不应随配置顺序变化，审计与日志里
	// 列出凭据时顺序也应该稳定。
	sort.Slice(s.entries, func(i, j int) bool {
		return s.entries[i].principal.User < s.entries[j].principal.User
	})
	return s, nil
}

// Open 报告是否处于「无凭据」的开放模式。
func (s *Store) Open() bool { return s == nil || s.open }

// Len 返回凭据条数。
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// Users 返回全部身份（不含 Token），供 /api/v1/auth/me 与 UI 使用。
func (s *Store) Users() []Principal {
	if s == nil {
		return nil
	}
	out := make([]Principal, len(s.entries))
	for i, e := range s.entries {
		out[i] = e.principal
	}
	return out
}

// Authenticate 校验 Token 并返回身份。
//
// 开放模式下任何 Token（包括空）都被接受为匿名管理员：这正是「关闭认证」
// 的语义。调用方必须把 Open() 的结果打进启动日志。
func (s *Store) Authenticate(token string) (Principal, bool) {
	if s == nil || s.open {
		return Anonymous, true
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	var hit *entry
	for i := range s.entries {
		// 不能提前 break：一旦命中就返回，会让「命中的位置越靠前越快」，
		// 攻击者能据此推断用户名顺序。
		if subtle.ConstantTimeCompare(sum[:], s.entries[i].sum[:]) == 1 {
			hit = &s.entries[i]
		}
	}
	if hit == nil {
		return Principal{}, false
	}
	return hit.principal, true
}

// ValidateToken 校验 Token 的强度与形态。
func ValidateToken(token string) error {
	if token == "" {
		return errors.New("token is required")
	}
	if len(token) < MinTokenLen {
		return fmt.Errorf("token must be at least %d characters (got %d)", MinTokenLen, len(token))
	}
	if len(token) > 512 {
		return errors.New("token must be at most 512 characters")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return errors.New("token must not contain whitespace")
	}
	if weakTokens[strings.ToLower(token)] {
		return errors.New("token is a well-known placeholder; generate one with `uic token`")
	}
	return nil
}
