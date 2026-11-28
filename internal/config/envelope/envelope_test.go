package envelope

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// validKEK 是测试用的 32 字节密钥（固定值，非随机，便于复现）。
func validKEK() []byte {
	k := make([]byte, KEKSize)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// TestRoundTrip 守护「加密→解密还原」：任意明文加密后必须能解回原值。
func TestRoundTrip(t *testing.T) {
	kek := validKEK()
	for _, plaintext := range []string{"", "hunter2", "P@ssw0rd!复杂密码中文", strings.Repeat("x", 1000)} {
		encrypted, err := Encrypt(kek, plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plaintext, err)
		}
		// 空串不加密（直接返回 ""），IsCiphertext 必须报 false。
		if plaintext == "" {
			if encrypted != "" {
				t.Fatalf("empty plaintext should return empty, got %q", encrypted)
			}
			continue
		}
		// 非空串加密后必须带前缀。
		if !IsCiphertext(encrypted) {
			t.Fatalf("encrypted %q missing prefix", encrypted)
		}
		// 密文不能泄露明文（GCM 是流密码，理论上可能碰撞，但概率可忽略）。
		if strings.Contains(encrypted, plaintext) && len(plaintext) > 4 {
			t.Fatalf("ciphertext %q contains plaintext %q", encrypted, plaintext)
		}
		decrypted, err := Decrypt(kek, encrypted)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", encrypted, err)
		}
		if decrypted != plaintext {
			t.Fatalf("round-trip mismatch: got %q, want %q", decrypted, plaintext)
		}
	}
}

// TestDecryptPlaintextPassthrough 守护「明文兼容」：不带 enc:v1: 前缀的输入
// 原样返回（向后兼容存量配置）。
func TestDecryptPlaintextPassthrough(t *testing.T) {
	kek := validKEK()
	for _, plaintext := range []string{"", "plain", "hunter2", "enc:v2:future"} {
		decrypted, err := Decrypt(kek, plaintext)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", plaintext, err)
		}
		if decrypted != plaintext {
			t.Fatalf("passthrough changed %q to %q", plaintext, decrypted)
		}
	}
}

// TestTamperDetection 守护「GCM 认证标签」：密文被篡改后必须报 ErrDecrypt，
// 不能返回任何部分明文（GCM 的设计目标）。
func TestTamperDetection(t *testing.T) {
	kek := validKEK()
	encrypted, err := Encrypt(kek, "secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// 篡改 base64 解码后的最后一个字节（密文尾部，认证标签所在）。
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(encrypted, Prefix))
	raw[len(raw)-1] ^= 0xFF
	tampered := Prefix + base64.StdEncoding.EncodeToString(raw)

	_, err = Decrypt(kek, tampered)
	if err == nil {
		t.Fatal("tampered ciphertext not rejected")
	}
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("error = %v, want ErrDecrypt", err)
	}
}

// TestWrongKEK 守护「KEK 不匹配」：用错误的 KEK 解密必须报 ErrDecrypt。
func TestWrongKEK(t *testing.T) {
	kek1 := validKEK()
	kek2 := make([]byte, KEKSize)
	copy(kek2, kek1)
	kek2[0] ^= 0xFF // 改一个字节

	encrypted, _ := Encrypt(kek1, "secret")
	_, err := Decrypt(kek2, encrypted)
	if err == nil {
		t.Fatal("wrong KEK not rejected")
	}
}

// TestKEKLengthValidation 守护「KEK 长度」：非 32 字节的 KEK 必须报错。
func TestKEKLengthValidation(t *testing.T) {
	for _, kek := range [][]byte{make([]byte, 16), make([]byte, 64), nil} {
		if _, err := Encrypt(kek, "x"); err == nil {
			t.Fatalf("Encrypt with %d-byte KEK should fail", len(kek))
		}
	}
}

// TestNonceUniqueness 守护「nonce 随机」：同一明文加密两次必须产生不同密文
// （nonce 随机，GCM 要求）。若 nonce 重复，攻击者可通过两次密文 XOR 恢复明文。
func TestNonceUniqueness(t *testing.T) {
	kek := validKEK()
	e1, _ := Encrypt(kek, "same")
	e2, _ := Encrypt(kek, "same")
	if e1 == e2 {
		t.Fatal("same plaintext encrypted to same ciphertext (nonce reuse)")
	}
}

// TestDeriveKEK 守护「派生确定性」：同一 secret 派生出同一 KEK（SHA-256 是
// 确定性函数）。
func TestDeriveKEK(t *testing.T) {
	k1 := DeriveKEK("my-token")
	k2 := DeriveKEK("my-token")
	if !bytes.Equal(k1, k2) {
		t.Fatal("DeriveKEK not deterministic")
	}
	if len(k1) != KEKSize {
		t.Fatalf("derived KEK size = %d, want %d", len(k1), KEKSize)
	}
	// 不同 secret 必须派生不同 KEK。
	k3 := DeriveKEK("other-token")
	if bytes.Equal(k1, k3) {
		t.Fatal("different secrets derived same KEK (collision)")
	}
}

// 编译期保证 rand.Reader 被使用（若包内 import 了但未用会编译失败）。
var _ = rand.Reader
