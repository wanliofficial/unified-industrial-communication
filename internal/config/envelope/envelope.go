// Package envelope 实现配置内敏感字段的信封加密（AES-256-GCM）。
//
// 存在的理由：R-2（等保 2.0 数据保密性/剩余信息保护）——南向链路凭据（MQTT
// CONNECT、OPC UA 用户名密码）若明文落在配置文件，一旦配置文件被备份、贴进
// 工单、误提交 git，等于凭据泄露。本包提供「写盘加密、读盘解密」的收口，
// 让配置文件里的 Password 字段不再是明文。
//
// 信封密钥（KEK）来源两条路径，都不引入第三方依赖（ADR-0001 §10 零第三方）：
//   - 环境变量 UIC_CONFIG_KEK：base64 编码的 32 字节密钥，运维通过密钥管理
//     系统注入，不落盘、不进配置、不进日志。
//   - 回退到 SHA-256(server.token)：server.token 已是 admin 凭据，用它派生
//     一个 KEK，避免「为了加密凭据必须先配一个 KEK」的鸡生蛋。安全性弱于
//     专用 KEK（token 泄露即 KEK 泄露），但好过明文。
//
// 存储格式：enc:v1:<base64(nonce ‖ ciphertext+tag)>。前缀 enc:v1: 让解密端
// 一眼识别；不带前缀的 Password 视为明文（向后兼容存量配置，首次 Apply 时
// 自动加密迁移）。
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Prefix 标识密文字段。Decrypt 只认带这个前缀的输入；不带前缀的视为明文。
const Prefix = "enc:v1:"

// KEKSize 是 AES-256 要求的密钥长度（32 字节）。
const KEKSize = 32

// ErrDecrypt 是解密失败的哨兵（密文被篡改、KEK 不匹配、格式错误）。
// 调用方应 fail-closed：解不开就不返回任何明文，绝不放行「当作明文继续用」。
var ErrDecrypt = errors.New("config/envelope: decrypt failed")

// IsCiphertext 报告 s 是否是密文（带 enc:v1: 前缀）。
func IsCiphertext(s string) bool {
	return strings.HasPrefix(s, Prefix)
}

// DeriveKEK 从任意口令派生 32 字节 KEK。用 SHA-256 而非 PBKDF2：这里 KEK
// 本身已是高熵密钥（环境变量注入的随机 32 字节，或 server.token 这种 ≥16 字符
// 的强口令），不需要慢哈希；慢哈希只在「用户输入低熵口令」时才必要。
func DeriveKEK(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Encrypt 用 KEK 加密明文，返回带 enc:v1: 前缀的字符串。空串原样返回
// （空密码不是敏感字段，无需加密）。
func Encrypt(kek []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(kek) != KEKSize {
		return "", fmt.Errorf("config/envelope: KEK must be %d bytes, got %d", KEKSize, len(kek))
	}

	block, err := aes.NewCipher([]byte(kek))
	if err != nil {
		return "", fmt.Errorf("config/envelope: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("config/envelope: new GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("config/envelope: nonce: %w", err)
	}
	// Seal 把密文+认证标签接在 nonce 后，一次性打包便于存储。
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密带 enc:v1: 前缀的字符串。不带前缀的输入视为明文原样返回
// （向后兼容存量配置）；带前缀但解密失败（篡改/KEK 不匹配）返回 ErrDecrypt，
// 调用方必须 fail-closed，绝不放行「当作空密码继续用」。
func Decrypt(kek []byte, stored string) (string, error) {
	if !IsCiphertext(stored) {
		return stored, nil // 明文，原样返回
	}
	if len(kek) != KEKSize {
		return "", fmt.Errorf("%w: KEK must be %d bytes", ErrDecrypt, KEKSize)
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, Prefix))
	if err != nil {
		return "", fmt.Errorf("%w: base64: %v", ErrDecrypt, err)
	}

	block, err := aes.NewCipher([]byte(kek))
	if err != nil {
		return "", fmt.Errorf("%w: new cipher: %v", ErrDecrypt, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("%w: new GCM: %v", ErrDecrypt, err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: ciphertext too short", ErrDecrypt)
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return string(plaintext), nil
}
