package upgrade

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// genKey 生成一对 ed25519 密钥，返回十六进制私钥与公钥。
func genKey(t *testing.T) (privHex, pubHex string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	return hex.EncodeToString(priv), hex.EncodeToString(pub)
}

// 构建一份最小包到磁盘（单个「二进制」文件），返回包路径。
func buildMinimal(t *testing.T, dst string, priv string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakebin")
	if err := os.WriteFile(bin, []byte("FAKE-BINARY-CONTENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := BuildSpec{
		Manifest:      Manifest{Version: "1.2.3", GoOS: runtime.GOOS, GoArch: runtime.GOARCH},
		BinaryPath:    bin,
		PrivateKeyHex: priv,
	}
	if err := Build(dst, spec); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return dst
}

// Build→Open→Verify 全链路：签名包必须能通过验签。
func TestBuildSignedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	priv, pub := genKey(t)
	buildMinimal(t, pkg, priv)

	p, err := Open(pkg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !p.Signed {
		t.Fatal("签名包应 Signed=true")
	}
	if p.Manifest.Version != "1.2.3" {
		t.Fatalf("version = %q", p.Manifest.Version)
	}
	rep, err := Verify(p, VerifyOptions{PublicKeyHex: pub})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK || !rep.Signed {
		t.Fatalf("report = %+v", rep)
	}
}

// 未签名包在默认策略下必须被拒（ErrUnsigned）。
func TestUnsignedRejectedByDefault(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	buildMinimal(t, pkg, "") // 无签名

	p, err := Open(pkg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if p.Signed {
		t.Fatal("空私钥不应签名")
	}
	_, err = Verify(p, VerifyOptions{})
	if err != ErrUnsigned {
		t.Fatalf("默认应拒绝未签名包，got %v", err)
	}
	// 显式允许才通过，且带警告。
	rep, err := Verify(p, VerifyOptions{AllowUnsigned: true})
	if err != nil {
		t.Fatalf("AllowUnsigned 应通过: %v", err)
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("未签名应有警告")
	}
}

// 签名包必须带公钥；给了错误的公钥必须验签失败。
func TestSignedNeedsCorrectKey(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	priv, _ := genKey(t)
	buildMinimal(t, pkg, priv)

	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	// 缺公钥：必须是那条专门的哨兵错误，而不是后面 decodeKey 顺带报的别的错。
	if _, err := Verify(p, VerifyOptions{}); err != ErrPubkeyRequired {
		t.Fatalf("签名包缺公钥应 ErrPubkeyRequired，got %v", err)
	}
	// 错误公钥。
	_, wrongPub := genKey(t)
	if _, err := Verify(p, VerifyOptions{PublicKeyHex: wrongPub}); err == nil {
		t.Fatal("错误公钥应验签失败")
	}
}

// 给了公钥但包未签名：两种「半验证」都要拒绝（ErrKeyMismatch）。
func TestKeyGivenButUnsignedRejected(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	buildMinimal(t, pkg, "")
	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	_, pub := genKey(t)
	_, err = Verify(p, VerifyOptions{PublicKeyHex: pub})
	if err != ErrKeyMismatch {
		t.Fatalf("给了公钥但包未签名应 ErrKeyMismatch，got %v", err)
	}
}

// 完整性：改动包内任意文件字节，sha256 校验必须失败。
func TestTamperedFileFailsIntegrity(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	priv, pub := genKey(t)
	buildMinimal(t, pkg, priv)

	// 篡改 manifest 里声明的 sha256（不重签名）。
	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Manifest.Files {
		p.Manifest.Files[i].SHA256 = strings.Repeat("0", 64)
	}
	// Files 里的内容不动，只改声明 → 哈希不符。
	_, err = Verify(p, VerifyOptions{PublicKeyHex: pub})
	if err == nil || !strings.Contains(err.Error(), "完整性") {
		t.Fatalf("篡改 sha256 应报完整性失败，got %v", err)
	}
}

// 改动 manifest 一个字节，签名必须失效（ed25519 对原文敏感）。
func TestTamperedManifestBreaksSignature(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	priv, pub := genKey(t)
	buildMinimal(t, pkg, priv)

	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	p.Manifest.Version = "9.9.9" // 改了 manifest 但 Raw 不变 → 签名仍对 Raw
	// 注意：签名是对 Raw（原文），改 Manifest 结构体不动 Raw，所以验签仍过；
	// 真正要测的是「Raw 被改」。我们直接构造一个 Raw 不一致的场景：
	p.Raw = append([]byte("tampered"), p.Raw...)
	_, err = Verify(p, VerifyOptions{PublicKeyHex: pub})
	if err == nil || !strings.Contains(err.Error(), "签名") {
		t.Fatalf("Raw 被改应验签失败，got %v", err)
	}
}

// 适配性：声明的 os/arch 必须匹配当前运行时。
func TestArchMismatch(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	bin := filepath.Join(t.TempDir(), "fakebin")
	os.WriteFile(bin, []byte("x"), 0o755)
	// 与当前 os 相同、arch 不同，确保只由 arch 维度触发。
	otherArch := "riscv64"
	if runtime.GOARCH == "riscv64" {
		otherArch = "amd64"
	}
	if err := Build(pkg, BuildSpec{
		Manifest:   Manifest{Version: "1", GoOS: runtime.GOOS, GoArch: otherArch},
		BinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(p, VerifyOptions{AllowUnsigned: true})
	if err == nil || !strings.Contains(err.Error(), "架构不匹配") {
		t.Fatalf("arch 不匹配应报错，got %v", err)
	}
}

// 适配性：os 不匹配也必须拒绝（只匹配 arch 不够）。
func TestOSMismatch(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	bin := filepath.Join(t.TempDir(), "fakebin")
	os.WriteFile(bin, []byte("x"), 0o755)
	otherOS := "plan9"
	if runtime.GOOS == "plan9" {
		otherOS = "linux"
	}
	if err := Build(pkg, BuildSpec{
		Manifest:   Manifest{Version: "1", GoOS: otherOS, GoArch: runtime.GOARCH},
		BinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(p, VerifyOptions{AllowUnsigned: true})
	if err == nil || !strings.Contains(err.Error(), "系统不匹配") {
		t.Fatalf("os 不匹配应报错，got %v", err)
	}
}

// manifest 声明了某个文件、但包里没有它：必须报「缺少文件」，不能静默跳过。
func TestMissingDeclaredFile(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	priv, pub := genKey(t)
	buildMinimal(t, pkg, priv)
	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	// 从包内删掉一个 manifest 声明的文件（Raw 不动，签名仍有效），模拟「声明了
	// 却没打进去」的构建事故。
	if len(p.Manifest.Files) == 0 {
		t.Fatal("包内应至少声明一个文件")
	}
	delete(p.Files, cleanName(p.Manifest.Files[0].Path))
	_, err = Verify(p, VerifyOptions{PublicKeyHex: pub})
	if err == nil || !strings.Contains(err.Error(), "缺少文件") {
		t.Fatalf("声明缺失文件应报错，got %v", err)
	}
}

// schema 不匹配必须拒绝。
func TestSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uic.pkg")
	priv, pub := genKey(t)
	buildMinimal(t, pkg, priv)
	p, err := Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	p.Manifest.Schema = "something-else/v99"
	// Raw 仍是原文（签名对 Raw 有效），但 schema 字段校验应先于签名失败。
	_, err = Verify(p, VerifyOptions{PublicKeyHex: pub})
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema 不符应拒绝，got %v", err)
	}
}

// Apply：旧二进制被备份、新二进制就位，且内容与包内一致。
func TestApplyReplacesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "uic")
	if err := os.WriteFile(cur, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := []byte("NEW-BINARY")
	res, err := Apply(cur, newBytes, 0o755)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Reverted {
		t.Fatal("不应回滚")
	}
	if res.BackupPath != cur+".bak" {
		t.Fatalf("备份路径 = %q", res.BackupPath)
	}
	got, err := os.ReadFile(cur)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY" {
		t.Fatalf("当前二进制 = %q, want NEW-BINARY", got)
	}
	bak, err := os.ReadFile(res.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != "OLD" {
		t.Fatalf("备份 = %q, want OLD", bak)
	}
	// 无 .new 残留。
	if _, err := os.Stat(cur + ".new"); !os.IsNotExist(err) {
		t.Fatal("应无 .new 残留")
	}
}

// Apply 到不存在的当前路径（首次安装）：不备份，直接就位。
func TestApplyFreshInstall(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "uic")
	res, err := Apply(cur, []byte("FIRST"), 0o755)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.BackupPath != "" || res.Reverted {
		t.Fatalf("首次安装不应有备份: %+v", res)
	}
	got, _ := os.ReadFile(cur)
	if string(got) != "FIRST" {
		t.Fatalf("got %q", got)
	}
}

// Build 必须能从磁盘实际文件重算 sha256：改二进制内容后，旧 manifest 的哈希失效。
func TestBuildRecomputesHash(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakebin")
	os.WriteFile(bin, []byte("v1"), 0o755)
	pkg := filepath.Join(dir, "uic.pkg")
	priv, pub := genKey(t)
	if err := Build(pkg, BuildSpec{Manifest: Manifest{Version: "1"}, BinaryPath: bin, PrivateKeyHex: priv}); err != nil {
		t.Fatal(err)
	}
	// 改二进制后重新 Build：新包应基于新内容哈希，仍能自验。
	os.WriteFile(bin, []byte("v2-different"), 0o755)
	pkg2 := filepath.Join(dir, "uic2.pkg")
	if err := Build(pkg2, BuildSpec{Manifest: Manifest{Version: "1"}, BinaryPath: bin, PrivateKeyHex: priv}); err != nil {
		t.Fatal(err)
	}
	p, err := Open(pkg2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(p, VerifyOptions{PublicKeyHex: pub}); err != nil {
		t.Fatalf("重算哈希后新包应自验: %v", err)
	}
}

// Build 缺二进制路径必须报错，不能静默产出空包。
func TestBuildRequiresBinary(t *testing.T) {
	if err := Build(filepath.Join(t.TempDir(), "x.pkg"), BuildSpec{}); err == nil {
		t.Fatal("缺 BinaryPath 应报错")
	}
}

// Open 对非 gzip/tar 文件要报清晰错误。
func TestOpenRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.pkg")
	os.WriteFile(p, []byte("not a gzip"), 0o644)
	if _, err := Open(p); err == nil {
		t.Fatal("垃圾输入应报错")
	}
}
