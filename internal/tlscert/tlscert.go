// Package tlscert 生成自签 TLS 证书，供 server.tls.auto 使用。
//
// 存在的理由：边缘网关常常部署在「拿到就能跑」的现场，运维要求加密传输（等保
// 2.0 安全通信 / 数据保密性），但未必有 CA 签发的服务端证书可分发。server.tls.auto
// 让这种场景开箱即用——进程启动时生成一张自签证书，浏览器/客户端首次连接时信任
// 它即可。自签证书不提供「对端身份认证」的强保证（没有 CA 链），它解决的是
// 「传输加密」而非「来源可信」；要来源可信请用 certFile/keyFile 提供 CA 签发证书。
//
// 只用标准库（crypto/ecdsa + crypto/x509），不引入任何第三方加密库（ADR-0001
// §10 零第三方依赖）。密钥类型选 ECDSA P-256：比 RSA-2048 更小的密钥与更快的握手，
// 对边缘设备的 CPU 与带宽更友好，且现代客户端普遍支持。
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// DefaultTTL 是自动生成的自签证书默认有效期。
//
// 刻意选较短有效期（而非自签常见的 10 年）：自签证书没有 CA 自动续期机制，长有效期
// 意味着这张证书的私钥一旦泄露，可利用窗口长达数年；短有效期把最坏情况框住，并
// 提醒运维「生产环境应换 CA 签发的正式证书」。到期前进程重启会重新生成一张新的。
const DefaultTTL = 90 * 24 * time.Hour

// Generate 生成一张自签 ECDSA P-256 证书，返回可直接喂给 tls.Server 的 tls.Certificate。
//
// host 是证书的 SAN（Subject Alternative Name）：同时写入 DNS 名与 IP 地址（若 host
// 是合法 IP），使客户端按主机名或 IP 校验都通过。空 host 只写 SAN 为空——仍能握手，
// 但客户端按主机名校验会失败，仅用于联调。
//
// ttl <= 0 时用 DefaultTTL。
func Generate(host string, ttl time.Duration) (tls.Certificate, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: generate key: %w", err)
	}

	now := time.Now().UTC()
	// 序列号用加密随机源，避免可预测（证书序列号可预测是已知的弱项）。64 位足够唯一。
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: generate serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "uic"},
		NotBefore:             now.Add(-time.Hour), // 回拨 1 小时容差，避免启动瞬间与客户端时钟轻微偏差导致 NotYetValid
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // 自签即自身的根，便于客户端把它当信任锚导入
	}
	if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: load key pair: %w", err)
	}
	return pair, nil
}

// Fingerprint 返回证书 DER 的 SHA-256 十六进制指纹（冒号分组），供日志打印，
// 让运维能在启动日志里看到「这次 auto 生成了哪张证书」，便于首次连接时人工核对。
func Fingerprint(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificate[0])
	const hex = "0123456789abcdef"
	out := make([]byte, len(sum)*3-1)
	for i, b := range sum {
		if i > 0 {
			out[i*3-1] = ':'
		}
		out[i*3] = hex[b>>4]
		out[i*3+1] = hex[b&0x0f]
	}
	return string(out)
}
