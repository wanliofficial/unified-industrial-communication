package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"uic/internal/upgrade"
)

// pkgCmd 构建离线升级包（gzip+tar，内含 manifest.json + 可选签名 + 二进制）。
//
// 存在的理由：升级包的 manifest.Files 由实际文件内容重算 sha256 生成，杜绝
// 「manifest 里的哈希与实际内容对不上」这类人为失误。签名用标准库 ed25519，
// 私钥经 -privkey 传入（不落盘）。配套 uic upgrade 做校验与原子安装。
func pkgCmd(args []string) {
	fs := flag.NewFlagSet("makepkg", flag.ExitOnError)
	out := fs.String("out", "", "输出包路径（必填）")
	binary := fs.String("binary", "", "要打包的二进制路径（必填）")
	version := fs.String("version", "dev", "目标版本号（写入 manifest）")
	goos := fs.String("os", runtime.GOOS, "目标系统（默认当前系统）")
	goarch := fs.String("arch", runtime.GOARCH, "目标架构（默认当前架构）")
	binaryName := fs.String("binary-name", "uic", "包内可执行体的名字")
	note := fs.String("note", "", "备注（写入 manifest）")
	privkey := fs.String("privkey", "", "ed25519 私钥（hex）；给出则对 manifest 签名")
	extra := stringList{}
	fs.Var(&extra, "extra", "附加文件 name=path（可重复），随包下发")
	asJSON := fs.Bool("json", false, "以 JSON 输出摘要")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法:
  uic makepkg -out dist/uic.pkg -binary dist/uic-linux-amd64 -version 1.5.0 -os linux -arch amd64 -privkey <hex>

参数:
  -out          输出包路径（必填）
  -binary       要打包的二进制（必填）
  -version      目标版本号
  -os           目标系统（默认当前系统）
  -arch         目标架构（默认当前架构）
  -binary-name  包内可执行体的名字（默认 uic）
  -note         备注
  -privkey      ed25519 私钥（hex）；给出则签名，否则未签名
  -extra        name=path 附加文件（可重复）
  -json         JSON 摘要
`)
	}
	fs.Parse(args)

	if *out == "" || *binary == "" {
		fmt.Fprintln(os.Stderr, "缺少 -out 或 -binary")
		os.Exit(2)
	}
	if _, err := os.Stat(*binary); err != nil {
		fmt.Fprintf(os.Stderr, "-binary 不可用: %v\n", err)
		os.Exit(2)
	}

	extras := map[string]string{}
	for _, kv := range extra {
		name, path, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(path) == "" {
			fmt.Fprintf(os.Stderr, "-extra 格式应为 name=path，got %q\n", kv)
			os.Exit(2)
		}
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "-extra %q: %v\n", kv, err)
			os.Exit(2)
		}
		extras[strings.TrimSpace(name)] = strings.TrimSpace(path)
	}

	spec := upgrade.BuildSpec{
		Manifest: upgrade.Manifest{
			Version: *version,
			GoOS:    *goos,
			GoArch:  *goarch,
			Note:    *note,
		},
		BinaryPath:    *binary,
		BinaryName:    *binaryName,
		Extras:        extras,
		PrivateKeyHex: *privkey,
	}
	if err := upgrade.Build(*out, spec); err != nil {
		fmt.Fprintf(os.Stderr, "构建失败: %v\n", err)
		os.Exit(upgrade.ExitError)
	}

	absOut, _ := filepath.Abs(*out)
	if *asJSON {
		raw, _ := hex.DecodeString(strings.TrimSpace(*privkey))
		signed := len(raw) == ed25519.PrivateKeySize
		fmt.Printf("{\"out\":%q,\"version\":%q,\"os\":%q,\"arch\":%q,\"binary\":%q,\"signed\":%v}\n",
			absOut, *version, *goos, *goarch, *binaryName, signed)
		return
	}
	fmt.Printf("已生成 %s\n", absOut)
	fmt.Printf("  版本    %s\n", *version)
	fmt.Printf("  目标    %s/%s\n", *goos, *goarch)
	fmt.Printf("  入口    %s\n", *binaryName)
	if strings.TrimSpace(*privkey) != "" {
		fmt.Println("  签名    已签名（ed25519）")
	} else {
		fmt.Println("  签名    未签名（仅 sha256 完整性）")
	}
}

// upgradeCmd 校验并安装离线升级包。
//
// 校验是默认开启、分层的（来源 ed25519 + 完整性 sha256 + 适配性 os/arch）。安装
// 走 tmp+rename 并先备份旧二进制到 <path>.bak。安装前需要 -yes 确认——替换二进制
// 与重启之间留人工闸，误传包不能直接把网关重启掉。
func upgradeCmd(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	pkgPath := fs.String("pkg", "", "升级包路径（必填）")
	pubkey := fs.String("pubkey", "", "ed25519 公钥（hex）；签名包必须提供")
	current := fs.String("binary", "", "当前二进制路径（默认 os.Executable()）")
	dryRun := fs.Bool("dry-run", false, "只校验并打印计划，不替换")
	allowUnsigned := fs.Bool("allow-unsigned", false, "允许安装未签名包（仅完整性校验）")
	confirm := fs.Bool("yes", false, "确认替换当前二进制")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法:
  uic upgrade -pkg dist/uic.pkg -pubkey <hex> -yes
  uic upgrade -pkg dist/uic.pkg -dry-run

参数:
  -pkg            升级包路径（必填）
  -pubkey         ed25519 公钥（hex）；签名包必须提供
  -binary         当前二进制路径（默认 os.Executable()）
  -dry-run        只校验并打印计划，不替换
  -allow-unsigned 允许安装未签名包（仅完整性校验）
  -yes            确认替换当前二进制
`)
	}
	fs.Parse(args)

	if *pkgPath == "" {
		fmt.Fprintln(os.Stderr, "缺少 -pkg")
		os.Exit(2)
	}
	if *current == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法定位当前二进制，请用 -binary 指定: %v\n", err)
			os.Exit(upgrade.ExitError)
		}
		*current = exe
	}

	pkg, err := upgrade.Open(*pkgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开升级包失败: %v\n", err)
		os.Exit(upgrade.ExitError)
	}
	rep, err := upgrade.Verify(pkg, upgrade.VerifyOptions{
		PublicKeyHex:  *pubkey,
		AllowUnsigned: *allowUnsigned,
	})
	if err != nil {
		printUpgradeReport(rep, *pkgPath, true)
		fmt.Fprintf(os.Stderr, "\n校验失败: %v\n", err)
		os.Exit(upgrade.ExitVerify)
	}

	binName := pkg.Manifest.Binary
	newBinary := pkg.Files[binName]
	if len(newBinary) == 0 {
		fmt.Fprintf(os.Stderr, "包内缺少可执行体 %q\n", binName)
		os.Exit(upgrade.ExitVerify)
	}

	printUpgradeReport(rep, *pkgPath, false)
	fmt.Printf("\n计划：用包内 %s 替换 %s\n", binName, *current)

	if *dryRun {
		fmt.Println("（-dry-run：未做任何改动）")
		return
	}
	if !*confirm {
		fmt.Println("未替换任何文件。确认无误后加 -yes 重新执行。")
		return
	}

	mode := os.FileMode(0o755)
	res, err := upgrade.Apply(*current, newBinary, mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "安装失败: %v\n", err)
		os.Exit(upgrade.ExitError)
	}
	fmt.Printf("\n已替换 %s\n", *current)
	if res.BackupPath != "" {
		fmt.Printf("旧二进制备份在 %s（回退：把它改回 %s）\n", res.BackupPath, *current)
	}
	fmt.Println("未自动重启。请手动重启以加载新二进制。")
}

func printUpgradeReport(rep *upgrade.Report, pkgPath string, failed bool) {
	status := "通过"
	if failed {
		status = "未通过"
	}
	fmt.Fprintf(os.Stderr, "升级包 %s 校验%s\n", pkgPath, status)
	fmt.Fprintf(os.Stderr, "  版本    %s\n", rep.Version)
	if rep.OS != "" || rep.Arch != "" {
		fmt.Fprintf(os.Stderr, "  目标    %s/%s\n", rep.OS, rep.Arch)
	}
	fmt.Fprintf(os.Stderr, "  入口    %s\n", rep.Binary)
	if rep.Signed {
		fmt.Fprintln(os.Stderr, "  来源    已签名（ed25519）")
	} else {
		fmt.Fprintln(os.Stderr, "  来源    未签名")
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(os.Stderr, "  警告    %s\n", w)
	}
}
