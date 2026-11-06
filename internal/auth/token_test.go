package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestGenerateTokenIsUsable 保证生成出来的东西马上能过配置校验。
func TestGenerateTokenIsUsable(t *testing.T) {
	for i := 0; i < 20; i++ {
		tok, err := GenerateToken(DefaultTokenBytes)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if err := ValidateToken(tok); err != nil {
			t.Fatalf("generated token rejected by ValidateToken: %v (%q)", err, tok)
		}
		if len(tok) < MinTokenLen {
			t.Fatalf("generated token too short: %q", tok)
		}
		// 生成结果必须能直接进 store，并且能认证通过。
		s, err := NewStore([]User{{Name: "u", Role: "viewer", Token: tok}}, "")
		if err != nil {
			t.Fatalf("generated token rejected by NewStore: %v", err)
		}
		if _, ok := s.Authenticate(tok); !ok {
			t.Fatal("generated token does not authenticate")
		}
	}
}

// TestGenerateTokenIsUnique 防「随机源退化成常量」这种灾难。
func TestGenerateTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, err := GenerateToken(DefaultTokenBytes)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = true
	}
}

// TestGenerateTokenAlphabet 保证结果里不会出现需要转义的字符。
func TestGenerateTokenAlphabet(t *testing.T) {
	tok, err := GenerateToken(DefaultTokenBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			t.Fatalf("token contains %q, which may be mangled by intermediaries: %q", r, tok)
		}
	}
}

// TestGenerateTokenClampsMinimum 防止调用方要一个 4 字节 Token。
func TestGenerateTokenClampsMinimum(t *testing.T) {
	for _, n := range []int{0, 1, 4, MinTokenBytes - 1} {
		tok, err := GenerateToken(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) < MinTokenLen {
			t.Fatalf("GenerateToken(%d) = %q, shorter than MinTokenLen", n, tok)
		}
	}
}

func TestGenerateTokenRejectsAbsurdLength(t *testing.T) {
	if _, err := GenerateToken(1 << 20); err == nil {
		t.Fatal("absurd length accepted")
	}
}

// TestHashTokenMatchesStore 保证工具输出的 hash 与 NewStore 期望的一致。
func TestHashTokenMatchesStore(t *testing.T) {
	tok, err := GenerateToken(DefaultTokenBytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := HashToken(tok)
	if len(sum) != 64 {
		t.Fatalf("hash length = %d, want 64", len(sum))
	}
	if _, err := hex.DecodeString(sum); err != nil {
		t.Fatalf("hash is not hex: %v", err)
	}
	s, err := NewStore([]User{{Name: "u", Role: "admin", Sha256: sum}}, "")
	if err != nil {
		t.Fatalf("store rejected its own hash: %v", err)
	}
	if _, ok := s.Authenticate(tok); !ok {
		t.Fatal("sha256-only credential does not accept the original token")
	}
}

// TestHashTokenTrimsWhitespace 与 Authenticate 的行为保持一致：
// 复制 Token 时带上换行不应该导致「哈希对不上」。
func TestHashTokenTrimsWhitespace(t *testing.T) {
	tok, err := GenerateToken(DefaultTokenBytes)
	if err != nil {
		t.Fatal(err)
	}
	if HashToken("  "+tok+"\n") != HashToken(tok) {
		t.Fatal("hash is sensitive to surrounding whitespace")
	}
}

// TestHashTokenIsCaseSensitive 是最基本的要求：大小写不同就是不同凭据。
func TestHashTokenIsCaseSensitive(t *testing.T) {
	a := HashToken(strings.Repeat("a", 24))
	b := HashToken(strings.Repeat("A", 24))
	if a == b {
		t.Fatal("hash ignores case")
	}
}
