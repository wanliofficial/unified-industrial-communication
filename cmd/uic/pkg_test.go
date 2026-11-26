package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// buildUICache 把当前 uic 编到一个临时文件并缓存：整场测试只编一次，供 makepkg/
// upgrade 的端到端用例反复 exec。用真实二进制跑真实子命令，才能验证「校验失败」
// 的退出码与「安装」的真实文件替换。
var (
	buildOnce sync.Once
	uicBin    string
	buildErr  error
)

func uicBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		// 仓库根：cmd/uic/pkg_test.go 的上三级。
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		// 放到进程级临时目录（不是某个测试的 t.TempDir，否则首个测试结束就被清掉）。
		dir, err := os.MkdirTemp("", "uic-testbin")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "uic")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command(goBin(), "build", "-o", bin, "./cmd/uic")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("build output:\n%s", out)
			return
		}
		uicBin = bin
	})
	if buildErr != nil {
		t.Fatalf("编译 uic 失败: %v", buildErr)
	}
	return uicBin
}

func goBin() string {
	if g := os.Getenv("GO"); g != "" {
		return g
	}
	return "go"
}

type keyPair struct{ priv, pub string }

func genUpgradeKeys(t *testing.T) keyPair {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	return keyPair{priv: hex.EncodeToString(priv), pub: hex.EncodeToString(pub)}
}

// makePkg 用真实二进制构建一个升级包，返回包路径。
func makePkg(t *testing.T, priv string, version, goos, goarch string) string {
	t.Helper()
	dir := t.TempDir()
	srcBin := filepath.Join(dir, "fakebin")
	if err := os.WriteFile(srcBin, []byte("FAKE-"+version), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "uic.pkg")
	args := []string{"makepkg", "-out", pkg, "-binary", srcBin,
		"-version", version, "-os", goos, "-arch", goarch}
	if priv != "" {
		args = append(args, "-privkey", priv)
	}
	cmd := exec.Command(uicBinary(t), args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("makepkg 失败: %v\n%s", err, out)
	}
	return pkg
}

// run 执行 uic 子命令并返回 (退出码, 合并输出)。ExitOnError 会在 os.Exit 触发，
// 因此我们直接观察进程退出码而非进程内的 os.Exit。
func run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(uicBinary(t), args...)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("执行 uic %v 失败: %v\n%s", args, err, out)
	}
	return 0, string(out)
}

// 签名包 round-trip：makepkg → upgrade -yes 应替换目标二进制。
func TestUpgradeSignedReplace(t *testing.T) {
	keys := genUpgradeKeys(t)
	pkg := makePkg(t, keys.priv, "1.5.0", runtime.GOOS, runtime.GOARCH)

	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	if err := os.WriteFile(target, []byte("OLD-VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out := run(t, "upgrade", "-pkg", pkg, "-pubkey", keys.pub, "-binary", target, "-yes")
	if code != 0 {
		t.Fatalf("upgrade 退出码 = %d, 期望 0\n%s", code, out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "FAKE-1.5.0" {
		t.Fatalf("目标二进制未被替换，内容 = %q", got)
	}
	// 旧版本应备份。
	bak, err := os.ReadFile(target + ".bak")
	if err != nil {
		t.Fatalf("缺少备份: %v", err)
	}
	if string(bak) != "OLD-VERSION" {
		t.Fatalf("备份内容 = %q, want OLD-VERSION", bak)
	}
	if !strings.Contains(out, "旧二进制备份") {
		t.Fatalf("输出应提示备份路径:\n%s", out)
	}
}

// 错误公钥：必须拒绝且退出码为 ExitVerify(2)，目标二进制不动。
func TestUpgradeWrongKeyRejected(t *testing.T) {
	keys := genUpgradeKeys(t)
	pkg := makePkg(t, keys.priv, "1.5.0", runtime.GOOS, runtime.GOARCH)
	other := genUpgradeKeys(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)

	code, out := run(t, "upgrade", "-pkg", pkg, "-pubkey", other.pub, "-binary", target, "-yes")
	if code != 2 {
		t.Fatalf("错误公钥应退出码 2，got %d\n%s", code, out)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatalf("校验失败后目标不应被改动: %q", got)
	}
}

// 签名包缺公钥：拒绝（签名包必须给公钥）。
func TestUpgradeSignedNeedsPubkey(t *testing.T) {
	keys := genUpgradeKeys(t)
	pkg := makePkg(t, keys.priv, "1.5.0", runtime.GOOS, runtime.GOARCH)
	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)
	code, _ := run(t, "upgrade", "-pkg", pkg, "-binary", target, "-yes")
	if code != 2 {
		t.Fatalf("签名包缺公钥应退出码 2，got %d", code)
	}
}

// 未签名包默认拒绝；显式 -allow-unsigned 才通过。
func TestUpgradeUnsignedPolicy(t *testing.T) {
	pkg := makePkg(t, "", "1.5.0", runtime.GOOS, runtime.GOARCH) // 未签名
	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)

	code, _ := run(t, "upgrade", "-pkg", pkg, "-binary", target, "-yes")
	if code != 2 {
		t.Fatalf("未签名包默认应拒绝（退出码 2），got %d", code)
	}

	// 允许未签名 + 确认 → 替换。
	code, out := run(t, "upgrade", "-pkg", pkg, "-binary", target, "-allow-unsigned", "-yes")
	if code != 0 {
		t.Fatalf("allow-unsigned 应通过，got %d\n%s", code, out)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "FAKE-1.5.0" {
		t.Fatalf("allow-unsigned 应替换: %q", got)
	}
}

// 不给 -yes：只预览，不替换。
func TestUpgradeRequiresConfirm(t *testing.T) {
	keys := genUpgradeKeys(t)
	pkg := makePkg(t, keys.priv, "1.5.0", runtime.GOOS, runtime.GOARCH)
	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)

	code, out := run(t, "upgrade", "-pkg", pkg, "-pubkey", keys.pub, "-binary", target)
	if code != 0 {
		t.Fatalf("预览应退出码 0，got %d\n%s", code, out)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatalf("未确认不应替换: %q", got)
	}
	if !strings.Contains(out, "-yes") {
		t.Fatalf("预览应提示加 -yes:\n%s", out)
	}
}

// dry-run：即使给了 -yes 也不替换（dry-run 优先级高于确认）。
func TestUpgradeDryRun(t *testing.T) {
	keys := genUpgradeKeys(t)
	pkg := makePkg(t, keys.priv, "1.5.0", runtime.GOOS, runtime.GOARCH)
	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)

	code, out := run(t, "upgrade", "-pkg", pkg, "-pubkey", keys.pub, "-binary", target, "-dry-run", "-yes")
	if code != 0 {
		t.Fatalf("dry-run 应退出码 0，got %d\n%s", code, out)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatalf("dry-run 不应替换（即使给了 -yes）: %q", got)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("输出应含 dry-run 说明:\n%s", out)
	}
	// 也不应留下任何备份。
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatal("dry-run 不应产生备份")
	}
}

// 架构不匹配的包：拒绝。
func TestUpgradeArchMismatch(t *testing.T) {
	keys := genUpgradeKeys(t)
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "windows"
	}
	pkg := makePkg(t, keys.priv, "1.5.0", other, runtime.GOARCH)
	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)
	code, out := run(t, "upgrade", "-pkg", pkg, "-pubkey", keys.pub, "-binary", target, "-yes")
	if code != 2 {
		t.Fatalf("架构不匹配应退出码 2，got %d\n%s", code, out)
	}
}

// 篡改包内容后 sha256 校验失败：拒绝。
func TestUpgradeTamperedRejected(t *testing.T) {
	keys := genUpgradeKeys(t)
	pkg := makePkg(t, keys.priv, "1.5.0", runtime.GOOS, runtime.GOARCH)
	// 往包里追加一个不属于 manifest 的文件不影响；改 manifest 里的二进制名会
	// 触发缺文件。这里改用「删掉 manifest.json」：解压不现实，改为构造一个
	// 内容被替换的包——直接读字节翻转后再写回（gzip 被破坏 → Open 失败 → 退出码非 0）。
	b, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xFF
	tampered := filepath.Join(t.TempDir(), "tampered.pkg")
	os.WriteFile(tampered, b, 0o644)
	dir := t.TempDir()
	target := filepath.Join(dir, "uic")
	os.WriteFile(target, []byte("OLD"), 0o755)
	code, _ := run(t, "upgrade", "-pkg", tampered, "-pubkey", keys.pub, "-binary", target, "-yes")
	if code == 0 {
		t.Fatal("被破坏的包应被拒绝（非 0 退出码）")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatalf("校验失败后不应替换: %q", got)
	}
}
