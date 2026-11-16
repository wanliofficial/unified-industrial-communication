package snmp

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// TestPasswordToKey_sha1 用 RFC 3414 A.3.1 的测试向量。
//
// 这是整个 USM 实现里最值得钉住的一条：口令扩展是 1 MiB 的重复填充，
// 「看起来对」的实现（比如直接哈希口令）在两侧都自研时也能互通，
// 但与任何真实设备都不通。只有拿标准向量验，才知道是否真的对。
func TestPasswordToKey_sha1(t *testing.T) {
	// RFC 3414 A.3.1：password "maplesyrup"，engineID 000000000000000000000002
	// Ku = 9fb5cc0381497b3793528939ff788d5d79145211
	ku, err := PasswordToKey(AuthSHA1, "maplesyrup")
	if err != nil {
		t.Fatalf("PasswordToKey: %v", err)
	}
	const wantKu = "9fb5cc0381497b3793528939ff788d5d79145211"
	if got := hex.EncodeToString(ku); got != wantKu {
		t.Fatalf("Ku = %s, want %s", got, wantKu)
	}
	engineID, _ := hex.DecodeString("000000000000000000000002")
	kul, err := LocalizeKey(AuthSHA1, ku, engineID)
	if err != nil {
		t.Fatalf("LocalizeKey: %v", err)
	}
	// Kul = 6695febc9288e36282235fc7151f128497b38f3f
	const wantKul = "6695febc9288e36282235fc7151f128497b38f3f"
	if got := hex.EncodeToString(kul); got != wantKul {
		t.Fatalf("Kul = %s, want %s", got, wantKul)
	}
}

// TestPasswordToKey_md5 用 RFC 3414 A.3.1 的 MD5 向量。
func TestPasswordToKey_md5(t *testing.T) {
	ku, err := PasswordToKey(AuthMD5, "maplesyrup")
	if err != nil {
		t.Fatalf("PasswordToKey: %v", err)
	}
	// Ku = 9faf3283884e92834ebc9847d8edd963
	if got := hex.EncodeToString(ku); got != "9faf3283884e92834ebc9847d8edd963" {
		t.Fatalf("Ku = %s", got)
	}
	engineID, _ := hex.DecodeString("000000000000000000000002")
	kul, err := LocalizeKey(AuthMD5, ku, engineID)
	if err != nil {
		t.Fatalf("LocalizeKey: %v", err)
	}
	// Kul = 526f5eed9fcce26f8964c2930787d82b
	if got := hex.EncodeToString(kul); got != "526f5eed9fcce26f8964c2930787d82b" {
		t.Fatalf("Kul = %s", got)
	}
}

// TestDigestLen 钉住 RFC 3414 / RFC 7860 的两张截断表。
//
// 这条用例的价值不在「数字对上了」，而在于钉住**不是**「摘要长度的一半」：
// MD5 是 12（不是 8）、SHA-1 是 12（不是 10）、SHA-224 是 16（不是 14）。
// 三类错法都能让主站与内置代理互通，却与任何真实设备都对不上——USM 的失败
// 是静默的，代理直接丢包，日志里只会看到「无响应」。
func TestDigestLen(t *testing.T) {
	keyLens := map[AuthProtocol]int{
		AuthMD5: 16, AuthSHA1: 20, AuthSHA224: 28, AuthSHA256: 32, AuthSHA384: 48, AuthSHA512: 64,
	}
	digestLens := map[AuthProtocol]int{
		AuthMD5: 12, AuthSHA1: 12, AuthSHA224: 16, AuthSHA256: 24, AuthSHA384: 32, AuthSHA512: 48,
	}
	for a, want := range keyLens {
		if got := a.KeyLen(); got != want {
			t.Errorf("%v KeyLen = %d, want %d", a, got, want)
		}
		if got := a.DigestLen(); got != digestLens[a] {
			t.Errorf("%v DigestLen = %d, want %d (RFC 3414 §6/§7 与 RFC 7860 §3)", a, got, digestLens[a])
		}
	}
	if AuthNone.DigestLen() != 0 || AuthNone.KeyLen() != 0 {
		t.Fatal("AuthNone must have no digest")
	}
}

// TestParseProtocols 钉住配置名的容错范围：大小写/连字符不敏感，
// 但**不猜**相近的写法。
func TestParseProtocols(t *testing.T) {
	authOK := map[string]AuthProtocol{
		"": AuthNone, "none": AuthNone, "MD5": AuthMD5, "md5": AuthMD5,
		"sha": AuthSHA1, "SHA-1": AuthSHA1, "sha1": AuthSHA1,
		"sha-224": AuthSHA224, "SHA256": AuthSHA256, "SHA_384": AuthSHA384, "sha512": AuthSHA512,
	}
	for in, want := range authOK {
		got, err := ParseAuthProtocol(in)
		if err != nil || got != want {
			t.Errorf("ParseAuthProtocol(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"md4", "sh", "sha-0", "hmac"} {
		if _, err := ParseAuthProtocol(bad); err == nil {
			t.Errorf("ParseAuthProtocol(%q) must fail", bad)
		}
	}
	privOK := map[string]PrivProtocol{
		"": PrivNone, "none": PrivNone, "DES": PrivDES, "des": PrivDES,
		"aes": PrivAES, "AES-128": PrivAES, "aes192": PrivAES192, "AES-256": PrivAES256,
	}
	for in, want := range privOK {
		got, err := ParsePrivProtocol(in)
		if err != nil || got != want {
			t.Errorf("ParsePrivProtocol(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"3des", "aes-64", "idea"} {
		if _, err := ParsePrivProtocol(bad); err == nil {
			t.Errorf("ParsePrivProtocol(%q) must fail", bad)
		}
	}
}

func TestValidateSecret(t *testing.T) {
	if err := ValidateSecret("authPassword", "1234567"); err == nil {
		t.Fatal("7 bytes must fail")
	}
	if err := ValidateSecret("authPassword", "12345678"); err != nil {
		t.Fatalf("8 bytes: %v", err)
	}
	long := strings.Repeat("x", 65)
	if err := ValidateSecret("authPassword", long); err == nil {
		t.Fatal("65 bytes must fail")
	}
}

// TestEncryptDecryptRoundTrip 覆盖 DES 与三种 AES 的往返。
//
// DES 与 AES 的 IV 来历不同（DES 用密钥后 8 字节 XOR privParams，
// AES 用 engineBoots||engineTime||salt），把两者的用例放在一起，
// 将来改一处实现时另一处会立刻失败。
func TestEncryptDecryptRoundTrip(t *testing.T) {
	cases := []struct {
		priv  PrivProtocol
		auth  AuthProtocol
		label string
	}{
		{PrivDES, AuthSHA1, "des"},
		{PrivAES, AuthSHA1, "aes128"},
		{PrivAES192, AuthSHA256, "aes192"},
		{PrivAES256, AuthSHA256, "aes256"},
	}
	plain := []byte("ScopedPDU payload of a non-multiple-of-8 length!!")
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			ku, err := PasswordToKey(c.auth, "privpass123")
			if err != nil {
				t.Fatal(err)
			}
			key, err := LocalizeKey(c.auth, ku, []byte{0x80, 0, 0x1f, 0x88, 0x80})
			if err != nil {
				t.Fatal(err)
			}
			if len(key) < c.priv.KeyLen() {
				t.Fatalf("localized key %d bytes, %v needs %d", len(key), c.priv, c.priv.KeyLen())
			}
			const boots, etime = 1, 100
			keys := &V3Keys{Priv: c.priv, PrivKey: key}
			salt := keys.Salt()
			ct, privParams, err := encryptScoped(c.priv, key, boots, etime, salt, plain)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			if bytes.Equal(ct, plain) {
				t.Fatal("ciphertext equals plaintext")
			}
			if c.priv == PrivDES {
				if len(privParams) != 8 {
					t.Fatalf("DES privParameters = %d bytes, want 8", len(privParams))
				}
				if len(ct)%8 != 0 {
					t.Fatalf("DES ciphertext %d bytes, want multiple of 8", len(ct))
				}
			} else if len(privParams) != 8 {
				t.Fatalf("%v privParameters = %d bytes, want 8", c.priv, len(privParams))
			}
			got, err := decryptScoped(c.priv, key, boots, etime, privParams, ct)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			// DES 会补到 8 字节整数倍，因此前缀相同即可。
			if !bytes.HasPrefix(got, plain) {
				t.Fatalf("decrypted = %q, want prefix %q", got, plain)
			}
		})
	}
}

// TestSaltIncrements 钉住 salt 每次不同：同一 (key, IV) 用两次会把
// 两份密文的异或变成两份明文的异或。
func TestSaltIncrements(t *testing.T) {
	k := &V3Keys{Priv: PrivAES}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := k.Salt()
		if len(s) != 8 {
			t.Fatalf("AES salt = %d bytes", len(s))
		}
		if seen[string(s)] {
			t.Fatalf("salt repeated at iteration %d", i)
		}
		seen[string(s)] = true
	}
	kd := &V3Keys{Priv: PrivDES}
	if got := kd.Salt(); len(got) != 4 {
		t.Fatalf("DES salt = %d bytes, want 4", len(got))
	}
}

// TestEncryptRejectsShortKey：AES-256 配 MD5 必须在派生后就能被拒绝。
func TestEncryptRejectsShortKey(t *testing.T) {
	ku, _ := PasswordToKey(AuthMD5, "privpass123")
	key, _ := LocalizeKey(AuthMD5, ku, []byte{0x80, 0, 0x1f, 0x88, 0x80})
	if len(key) >= 32 {
		t.Fatalf("MD5 localizes to %d bytes (expected < 32)", len(key))
	}
	_, _, err := encryptScoped(PrivAES256, key, 1, 1, make([]byte, 8), []byte("x"))
	if err == nil {
		t.Fatal("AES-256 with a 16-byte key must fail")
	}
	if !strings.Contains(err.Error(), "longer auth protocol") {
		t.Fatalf("error = %v, want a hint about the auth protocol", err)
	}
}
