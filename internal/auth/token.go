package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// DefaultTokenBytes 是生成 Token 时的默认随机字节数。
//
// 24 字节 → 32 个 base64url 字符（192 bit）。比 MinTokenLen 留出的余量
// 不是为了「更长更安全」，而是为了让运维一眼看出「这是工具生成的」，
// 而不会把 16 位的业务编号当成 Token 粘进配置。
const DefaultTokenBytes = 24

// MinTokenBytes 是生成 Token 允许的最小随机字节数（对应 16 个字符）。
//
// 再少就会跌破 MinTokenLen——那说明调用方并不需要「生成一个合规 Token」，
// 而是想要一个短口令，这里不应该配合。
const MinTokenBytes = 12

// GenerateToken 生成一个密码学随机的 Token。
//
// 用 base64url 无填充编码：结果只包含 [A-Za-z0-9_-]，可以安全地放进
// HTTP 头、环境变量、JSON 与 URL 查询串，不会因为 `+`/`/`/`=` 被某一层
// 转义或截断——Token 被中转层改写是最难排查的一类「明明配对了却 401」。
func GenerateToken(nbytes int) (string, error) {
	if nbytes < MinTokenBytes {
		nbytes = MinTokenBytes
	}
	if nbytes > 128 {
		return "", fmt.Errorf("auth: token length %d bytes is unreasonable (max 128)", nbytes)
	}
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 返回 Token 的 sha256 十六进制串，也就是配置里的 tokenSha256。
//
// 导出它是为了让 `uic token` 与 NewStore 用同一套编码：如果工具输出一种
// 大小写、配置解析要求另一种，用户会在「生成的哈希不能用」上浪费一小时。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}
