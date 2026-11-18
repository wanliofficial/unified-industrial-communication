package snmp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// 本文件是 SNMPv3 USM 的密码学部分：口令 → 密钥 → 本地化密钥 → 摘要/加密。
//
// 全部用标准库实现，没有任何第三方依赖。四件事必须严格按顺序做对，任何一步
// 的偏差都表现为「代理不回话」——USM 的失败是**静默**的：摘要不对，代理直接
// 丢弃报文（可选回一个 usmStatsWrongDigests 的 Report，但那本身可能是伪造的，
// 所以主站不能依赖它来定位问题）。

// AuthProtocol 是 USM 的认证算法。
//
// 名字用标准写法（MD5/SHA/SHA-256…），配置里大小写与连字符不敏感。截断
// 长度**不是**统一的「摘要长度的一半」，也不全是 12：MD5/SHA-1 按RFC 3414
// 固定 12 字节（96 位），SHA-2 系列按 RFC 7860 是 16/24/32/48——见
// DigestLen 的完整说明。
type AuthProtocol int

const (
	AuthNone AuthProtocol = iota
	AuthMD5
	AuthSHA1
	AuthSHA224
	AuthSHA256
	AuthSHA384
	AuthSHA512
)

// PrivProtocol 是 USM 的加密算法。
type PrivProtocol int

const (
	PrivNone PrivProtocol = iota
	PrivDES
	PrivAES
	PrivAES192
	PrivAES256
)

func (a AuthProtocol) String() string {
	switch a {
	case AuthNone:
		return "none"
	case AuthMD5:
		return "MD5"
	case AuthSHA1:
		return "SHA"
	case AuthSHA224:
		return "SHA-224"
	case AuthSHA256:
		return "SHA-256"
	case AuthSHA384:
		return "SHA-384"
	case AuthSHA512:
		return "SHA-512"
	}
	return fmt.Sprintf("auth(%d)", int(a))
}

func (p PrivProtocol) String() string {
	switch p {
	case PrivNone:
		return "none"
	case PrivDES:
		return "DES"
	case PrivAES:
		return "AES"
	case PrivAES192:
		return "AES-192"
	case PrivAES256:
		return "AES-256"
	}
	return fmt.Sprintf("priv(%d)", int(p))
}

// ParseAuthProtocol 解析配置里的认证算法名。
//
// 接受 `SHA` 与 `SHA1` 两种写法：前者是标准里的名字（很多设备的界面也这么
// 写），后者是算法名。拒绝 `SHA-1` 之外的缩写（如 `SH`）——猜名字的代价是
// 一条永远对不上的摘要。
func ParseAuthProtocol(s string) (AuthProtocol, error) {
	n := strings.ToUpper(strings.TrimSpace(s))
	n = strings.ReplaceAll(n, "-", "")
	n = strings.ReplaceAll(n, "_", "")
	switch n {
	case "", "NONE", "NOAUTH":
		return AuthNone, nil
	case "MD5":
		return AuthMD5, nil
	case "SHA", "SHA1":
		return AuthSHA1, nil
	case "SHA224":
		return AuthSHA224, nil
	case "SHA256":
		return AuthSHA256, nil
	case "SHA384":
		return AuthSHA384, nil
	case "SHA512":
		return AuthSHA512, nil
	}
	return AuthNone, fmt.Errorf("snmp: unknown auth protocol %q (want MD5, SHA, SHA-224, SHA-256, SHA-384 or SHA-512)", s)
}

// ParsePrivProtocol 解析配置里的加密算法名。
//
// `AES` 是 AES-128（最普遍）；192/256 需要更长的本地化密钥，也就是需要
// SHA-256 及以上的认证算法——用 MD5 配 AES-256 在配置期就拒绝，而不是等
// 到派生密钥时才报「密钥太短」。
func ParsePrivProtocol(s string) (PrivProtocol, error) {
	n := strings.ToUpper(strings.TrimSpace(s))
	n = strings.ReplaceAll(n, "-", "")
	n = strings.ReplaceAll(n, "_", "")
	switch n {
	case "", "NONE", "NOPRIV":
		return PrivNone, nil
	case "DES":
		return PrivDES, nil
	case "AES", "AES128":
		return PrivAES, nil
	case "AES192":
		return PrivAES192, nil
	case "AES256":
		return PrivAES256, nil
	}
	return PrivNone, fmt.Errorf("snmp: unknown priv protocol %q (want DES, AES, AES-192 or AES-256)", s)
}

// Hash 返回该认证算法的摘要构造函数。
func (a AuthProtocol) Hash() func() hash.Hash {
	switch a {
	case AuthMD5:
		return md5.New
	case AuthSHA1:
		return sha1.New
	case AuthSHA224:
		return sha256.New224
	case AuthSHA256:
		return sha256.New
	case AuthSHA384:
		return sha512.New384
	case AuthSHA512:
		return sha512.New
	}
	return nil
}

// DigestLen 是 HMAC 在报文里的截断长度。
//
// **不能统一写成「摘要长度的一半」**——这是 USM 最容易被想当然的一处，也是
// 本地回环测不出来的那种错：主站与内置代理都按同一个（错的）长度算，互通
// 正常；一接真实设备就变成「认证失败/无响应」，而且没有任何可读的报错。
//
// 两个标准给的是两张固定表：
//
//	RFC 3414 §6/§7   HMAC-MD5-96、HMAC-SHA-96  → 固定 96 位 = 12 字节
//	RFC 7860 §3      SHA-224 → 128 位 = 16 字节
//	                 SHA-256 → 192 位 = 24 字节
//	                 SHA-384 → 256 位 = 32 字节
//	                 SHA-512 → 384 位 = 48 字节
//
// 注意 SHA-1 是 12（不是 20/2=10）、MD5 也是 12（不是 16/2=8）：两者都进
// 「96 位」那张表，与摘要长度无关。SHA-2 系列才恰好是「一半」，但 SHA-224
// 的 14 字节从来不是标准值，标准写的是 16。
func (a AuthProtocol) DigestLen() int {
	switch a {
	case AuthMD5, AuthSHA1:
		return 12
	case AuthSHA224:
		return 16
	case AuthSHA256:
		return 24
	case AuthSHA384:
		return 32
	case AuthSHA512:
		return 48
	}
	return 0
}

// KeyLen 是本地化密钥的字节数（等于摘要长度）。
func (a AuthProtocol) KeyLen() int {
	h := a.Hash()
	if h == nil {
		return 0
	}
	return h().Size()
}

// keyExpansionLen 是 RFC 3414 A.2.1 规定的口令扩展长度：1 MiB。
//
// 为什么要扩展：直接把口令当密钥会让短口令的熵几乎没有；扩展成 1 MiB 再
// 取摘要，把「口令长度」的影响压到最小。这一步不是安全上的银弹，但它是
// 协议的一部分——不照做，派生出的密钥与代理侧的永远不同。
const keyExpansionLen = 1048576

// PasswordToKey 把口令扩展成 1 MiB 后取摘要，得到 Ku。
//
// 空口令直接报错：「无口令」与「口令为空串」在 USM 里是两回事，后者派生
// 出一个固定的密钥，看起来能连上、实际是任何人都能算出来的密钥。
func PasswordToKey(a AuthProtocol, password string) ([]byte, error) {
	h := a.Hash()
	if h == nil {
		return nil, fmt.Errorf("snmp: auth protocol %v has no digest", a)
	}
	if password == "" {
		return nil, errors.New("snmp: auth password is empty")
	}
	pass := []byte(password)
	// 按口令长度重复填满 1 MiB。整块分配一次，避免在循环里反复扩容；
	// 这一步只在 Open 时做一次，1 MiB 的临时内存可以接受。
	buf := make([]byte, keyExpansionLen)
	off := 0
	for off < len(buf) {
		off += copy(buf[off:], pass)
	}
	d := h()
	d.Write(buf)
	return d.Sum(nil), nil
}

// LocalizeKey 计算 Kul = H(Ku || engineID || Ku)。
//
// 为什么必须本地化：同一个用户在不同代理上有不同的密钥（因为 engineID
// 不同）。这样即使两台设备的用户口令相同，截获的报文也无法互相重放。
// 「两台设备用同一份配置」在这里是自然的——配置里存的是口令，不是密钥。
func LocalizeKey(a AuthProtocol, ku, engineID []byte) ([]byte, error) {
	h := a.Hash()
	if h == nil {
		return nil, fmt.Errorf("snmp: auth protocol %v has no digest", a)
	}
	if len(ku) == 0 {
		return nil, errors.New("snmp: master key is empty")
	}
	if len(engineID) == 0 {
		return nil, errors.New("snmp: engine ID is empty (run discovery first)")
	}
	d := h()
	d.Write(ku)
	d.Write(engineID)
	d.Write(ku)
	return d.Sum(nil), nil
}

// authDigest 计算 HMAC 并按算法截断。
func authDigest(a AuthProtocol, key, msg []byte) ([]byte, error) {
	h := a.Hash()
	if h == nil {
		return nil, fmt.Errorf("snmp: auth protocol %v has no digest", a)
	}
	m := hmac.New(h, key)
	m.Write(msg)
	n := a.DigestLen()
	sum := m.Sum(nil)
	if len(sum) < n {
		return nil, fmt.Errorf("snmp: HMAC output %d bytes, need %d", len(sum), n)
	}
	return sum[:n], nil
}

// privacyKeyLen 返回加密算法需要的密钥字节数。
func (p PrivProtocol) KeyLen() int {
	switch p {
	case PrivDES:
		return 16 // 8 字节 DES 密钥 + 8 字节前序 IV
	case PrivAES:
		return 16
	case PrivAES192:
		return 24
	case PrivAES256:
		return 32
	}
	return 0
}

// saltLen 是 privParameters 的长度：AES 用 8 字节，DES 用 4 字节。
func (p PrivProtocol) saltLen() int {
	if p == PrivDES {
		return 4
	}
	return 8
}

// encryptScoped 加密一个 ScopedPDU。
//
// DES 与 AES 的差别不只是算法，**IV 的来历也不同**，这是最容易做错的一处：
//
//	DES-CBC：privParameters = engineBoots(4) || salt(4)，IV = preIV XOR privParameters
//	         其中 preIV 是本地化密钥的后 8 字节。报文必须补到 8 字节整数倍。
//	AES-CFB：privParameters = salt(8)，IV = engineBoots(4) || engineTime(4) || salt
//	         CFB 是流模式，不需要补齐。
//
// 把 DES 的「密钥后 8 字节当 preIV」照搬到 AES，得到的密文代理能解但内容是
// 垃圾——症状是「认证通过、PDU 解析失败」，比直接丢弃更难查。
func encryptScoped(p PrivProtocol, key []byte, engineBoots, engineTime int, salt, plain []byte) ([]byte, []byte, error) {
	if p == PrivNone {
		return nil, nil, errors.New("snmp: priv protocol is none")
	}
	need := p.KeyLen()
	if len(key) < need {
		return nil, nil, fmt.Errorf("snmp: %v needs a %d-byte key, localized key is %d bytes (use a longer auth protocol)",
			p, need, len(key))
	}
	switch p {
	case PrivDES:
		if engineBoots < 0 || engineBoots > 0x7FFFFFFF {
			return nil, nil, fmt.Errorf("snmp: engineBoots %d out of range", engineBoots)
		}
		// DES 的 privParameters 是 engineBoots(4) || salt(4)；注意**只有
		// engineBoots**，没有 engineTime。这是 DES 与 AES 的第一个差异。
		boots := make([]byte, 4)
		binary.BigEndian.PutUint32(boots, uint32(engineBoots))
		if len(salt) != 4 {
			return nil, nil, fmt.Errorf("snmp: DES salt must be 4 bytes, got %d", len(salt))
		}
		privParams := make([]byte, 8)
		copy(privParams, boots)
		copy(privParams[4:], salt)
		iv := make([]byte, 8)
		preIV := key[8:16]
		for i := range iv {
			iv[i] = preIV[i] ^ privParams[i]
		}
		block, err := des.NewCipher(key[:8])
		if err != nil {
			return nil, nil, fmt.Errorf("snmp: des: %w", err)
		}
		// 补齐到 8 字节：填充内容任意，接收方按 ASN.1 解析时会丢弃尾部。
		padded := plain
		if rem := len(padded) % 8; rem != 0 {
			padded = append(append([]byte(nil), plain...), make([]byte, 8-rem)...)
		}
		out := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
		return out, privParams, nil
	default:
		var aesKey []byte
		var bits int
		switch p {
		case PrivAES:
			aesKey, bits = key[:16], 128
		case PrivAES192:
			aesKey, bits = key[:24], 192
		case PrivAES256:
			aesKey, bits = key[:32], 256
		}
		if len(salt) != 8 {
			return nil, nil, fmt.Errorf("snmp: AES salt must be 8 bytes, got %d", len(salt))
		}
		// AES 的 IV 是 engineBoots(4) || engineTime(4) || salt(8)：**含
		// engineTime**，且 salt 直接进 IV（不 XOR）。这是与 DES 的第二
		// 个差异，两个都照搬错一处就会得到「认证通过、PDU 是垃圾」。
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv[0:4], uint32(engineBoots))
		binary.BigEndian.PutUint32(iv[4:8], uint32(engineTime))
		copy(iv[8:], salt)
		block, err := aes.NewCipher(aesKey)
		if err != nil {
			return nil, nil, fmt.Errorf("snmp: aes-%d: %w", bits, err)
		}
		out := make([]byte, len(plain))
		cipher.NewCFBEncrypter(block, iv).XORKeyStream(out, plain)
		return out, append([]byte(nil), salt...), nil
	}
}

// decryptScoped 解密一个 ScopedPDU。
func decryptScoped(p PrivProtocol, key []byte, engineBoots, engineTime int, privParams, ciphertext []byte) ([]byte, error) {
	if p == PrivNone {
		return nil, errors.New("snmp: priv protocol is none")
	}
	need := p.KeyLen()
	if len(key) < need {
		return nil, fmt.Errorf("snmp: %v needs a %d-byte key, localized key is %d bytes", p, need, len(key))
	}
	switch p {
	case PrivDES:
		if len(privParams) != 8 {
			return nil, fmt.Errorf("snmp: DES privParameters must be 8 bytes, got %d", len(privParams))
		}
		if len(ciphertext) == 0 || len(ciphertext)%8 != 0 {
			return nil, fmt.Errorf("snmp: DES ciphertext is %d bytes (must be a non-zero multiple of 8)", len(ciphertext))
		}
		iv := make([]byte, 8)
		preIV := key[8:16]
		for i := range iv {
			iv[i] = preIV[i] ^ privParams[i]
		}
		block, err := des.NewCipher(key[:8])
		if err != nil {
			return nil, fmt.Errorf("snmp: des: %w", err)
		}
		out := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ciphertext)
		return out, nil
	default:
		var aesKey []byte
		switch p {
		case PrivAES:
			aesKey = key[:16]
		case PrivAES192:
			aesKey = key[:24]
		case PrivAES256:
			aesKey = key[:32]
		}
		if len(privParams) != 8 {
			return nil, fmt.Errorf("snmp: AES salt must be 8 bytes, got %d", len(privParams))
		}
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv[0:4], uint32(engineBoots))
		binary.BigEndian.PutUint32(iv[4:8], uint32(engineTime))
		copy(iv[8:], privParams)
		block, err := aes.NewCipher(aesKey)
		if err != nil {
			return nil, fmt.Errorf("snmp: aes: %w", err)
		}
		out := make([]byte, len(ciphertext))
		cipher.NewCFBDecrypter(block, iv).XORKeyStream(out, ciphertext)
		return out, nil
	}
}

// ValidateSecret 检查 USM 口令的可用性。
//
// 最短 8 字节是 RFC 3414 的硬要求，代理侧会直接拒绝更短的口令；在配置期
// 就报错，比让现场看到「认证失败」然后去翻代理日志要快得多。上限 64 是
// 自我约束：更长的口令不会更安全（扩展后都被压成摘要），只会让人误以为
// 「口令越长越好」。
func ValidateSecret(what, s string) error {
	if len(s) < 8 {
		return fmt.Errorf("snmp: %s must be at least 8 bytes, got %d", what, len(s))
	}
	if len(s) > 64 {
		return fmt.Errorf("snmp: %s is %d bytes (max 64)", what, len(s))
	}
	return nil
}
