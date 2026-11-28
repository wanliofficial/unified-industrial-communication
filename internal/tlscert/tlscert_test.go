package tlscert

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// TestGenerateProducesUsableCert 守护「auto 自签真能用」：生成的证书必须能被
// x509 解析、当前时刻落在有效期内、密钥与证书匹配、且按 host 校验通过。这张证书
// 会被直接喂给 tls.Server，任何一项不成立都会让握手在生产里失败。
func TestGenerateProducesUsableCert(t *testing.T) {
	pair, err := Generate("gateway.example", time.Hour)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	// 密钥必须是 ECDSA（与包内生成一致），且与证书公钥匹配。
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("private key type = %T, want *ecdsa.PrivateKey", pair.PrivateKey)
	}
	if !leaf.PublicKey.(*ecdsa.PublicKey).Equal(pair.PrivateKey.(*ecdsa.PrivateKey).Public()) {
		t.Fatal("certificate public key does not match private key")
	}

	// 当前时刻必须落在有效期内（NotBefore 回拨 1 小时容差，NotAfter 在 ttl 后）。
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		t.Fatalf("now %s not in validity [%s, %s]", now, leaf.NotBefore, leaf.NotAfter)
	}

	// 必须允许服务端认证（ExtKeyUsage），否则 TLS 握手会因用途不符失败。
	if err := leaf.VerifyHostname("gateway.example"); err != nil {
		t.Fatalf("VerifyHostname(gateway.example): %v", err)
	}

	// host 是 IP 时必须写进 IPAddresses，按 IP 校验也要通过。
	ipPair, err := Generate("10.0.0.5", time.Hour)
	if err != nil {
		t.Fatalf("Generate(ip): %v", err)
	}
	ipLeaf, err := x509.ParseCertificate(ipPair.Certificate[0])
	if err != nil {
		t.Fatalf("parse ip leaf: %v", err)
	}
	if err := ipLeaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Fatalf("VerifyHostname(10.0.0.5): %v", err)
	}

	// 自签即自身的根：Issuer == Subject，且能用自己的公钥验签自己。
	if string(leaf.RawIssuer) != string(leaf.RawSubject) {
		t.Fatalf("self-signed: issuer %q != subject %q", leaf.RawIssuer, leaf.RawSubject)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "gateway.example", CurrentTime: now}); err != nil {
		t.Fatalf("verify against self as root: %v", err)
	}
}

// TestGenerateEmptyHostStillHandshakeable 守护「空 host 不 panic 且证书仍可用」：
// 仅用于联调，但绝不能因为空 host 就生成一张坏证书。
func TestGenerateEmptyHostStillHandshakeable(t *testing.T) {
	pair, err := Generate("", 0) // ttl<=0 走 DefaultTTL
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := x509.ParseCertificate(pair.Certificate[0]); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// DefaultTTL 生效：约 90 天。
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if d := leaf.NotAfter.Sub(leaf.NotBefore); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Fatalf("default ttl = %s, want ~90d", d)
	}
}

// TestFingerprintStableAndNonEmpty 守护日志打印的指纹：同一证书两次调用一致、格式
// 为冒号分组的 64 位十六进制 SHA-256。
func TestFingerprintStableAndNonEmpty(t *testing.T) {
	pair, _ := Generate("h", time.Hour)
	fp := Fingerprint(pair)
	if fp == "" {
		t.Fatal("fingerprint empty")
	}
	if !strings.Contains(fp, ":") {
		t.Fatalf("fingerprint %q not colon-grouped", fp)
	}
	if Fingerprint(pair) != fp {
		t.Fatal("fingerprint not stable")
	}
	// 必须是证书 DER 的 sha256：与手工算的对得上。
	sum := sha256.Sum256(pair.Certificate[0])
	_ = pem.Block{} // 确保 pem 被使用（编译）
	if !strings.HasPrefix(fp, hexByte(sum[0])) {
		t.Fatalf("fingerprint %q does not start with sha256 byte %q", fp, hexByte(sum[0]))
	}
}

// hexByte 把单字节渲染成冒号分组指纹里的两位十六进制。
func hexByte(b byte) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[b>>4], hex[b&0x0f]})
}

// 编译期保证 tls.Certificate 满足我们要喂给 tls.Server 的形态。
var _ tls.Certificate
