// Package upgrade 提供离线升级包的构建、校验与原子安装。
//
// 升级包的形态是标准库即可处理的 gzip+tar：一个 manifest.json（列出每个文件的
// sha256 与目标 os/arch），一个 manifest.sig（manifest.json 原文的 ed25519 签名，
// 可省略表示未签名），以及被升级的二进制本体。之所以不用 zip：zip 的作者是
// 外部生态，而 ADR-0001 要求依赖面按构造为空——archive/tar + compress/gzip 都是
// 标准库，升级这条供应链的关键路径上不引入任何第三方代码。
//
// 安全立场：升级会替换掉整个进程的可执行体，是少数几个「一次出错就不可逆」的
// 动作。因此校验是默认开启的，且分层：
//  1. 来源：签名用 ed25519 验证（不是「看起来像密钥就算」）。给了 -pubkey 但包
//     未签名、或未给 -pubkey 而包已签名，都直接拒绝——两种「半验证」都不可信。
//  2. 完整性：manifest 列的每个文件都要重算 sha256 与声明值比对。任何缺文件、
//     任何字节改动都使校验失败。
//  3. 适配性：manifest 声明的 os/arch 必须匹配当前运行时，避免把 arm64 的二进制
//     装到 amd64 上。
//  4. 原子性：安装走 tmp + rename，旧二进制先备份到 <path>.bak 以便人工回退。
//
// 安装刻意不自动重启：替换二进制与「用新二进制拉起新进程」之间要留出一道人工闸
// （-yes 确认），否则一个误传的包就能把网关重启掉，而重启会丢缓存与在飞写。
package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Schema 是升级包 manifest 的版本标识。不匹配即拒绝解析，避免把未来版本的包
// 当成当前版本理解（静默按旧语义解释一个字段改名了的 manifest，是这类工具的
// 经典事故）。
const Schema = "uic-upgrade/v1"

// 升级包内的固定文件名。
const (
	ManifestName  = "manifest.json"
	SignatureName = "manifest.sig"
)

// 退出码。调用方（cmd/uic）据此区分「校验失败」与「运行时错误」，便于脚本判定。
const (
	ExitOK     = 0 // 校验通过 / 安装成功 / 预览
	ExitError  = 1 // 运行时错误（读包、写文件、替换失败…）
	ExitVerify = 2 // 校验失败（签名、完整性、适配性）
)

// FileEntry 是 manifest 对包内单个文件的声明。
type FileEntry struct {
	// Path 是包内相对路径（如 "uic"、"config.default.json"）。
	Path string `json:"path"`
	// SHA256 是该文件内容的十六进制小写摘要。
	SHA256 string `json:"sha256"`
	// Mode 是安装时的权限位（八进制，如 493 表示 0755）。省略时用默认 0755。
	Mode int `json:"mode,omitempty"`
}

// Manifest 是升级包的完整清单。
type Manifest struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	GoOS    string `json:"os,omitempty"`
	GoArch  string `json:"arch,omitempty"`
	// Binary 是包内可执行体的路径。安装时它替换当前二进制，其余文件只是附带。
	Binary string      `json:"binary"`
	Files  []FileEntry `json:"files"`
	Note   string      `json:"note,omitempty"`
}

// Package 是解析后的升级包，全部文件在内存中（二进制 ≤40MB，可接受）。
type Package struct {
	Manifest Manifest
	// Raw 是 manifest.json 的原文，签名正是对这串字节的。改动一个字节都会使验签失败。
	Raw []byte
	// Signed 表示包内是否带有 manifest.sig。
	Signed bool
	// Sig 是验签用的原始签名字节（Signed 时非空）。
	Sig []byte
	// Files 是「包内路径 → 内容字节」的映射，含 manifest.json 与 manifest.sig。
	Files map[string][]byte
}

// Report 是校验的结论，含成功与否、是否签名、以及给人看的信息。
type Report struct {
	OK       bool
	Signed   bool
	Warnings []string
	// Version/OldOS 等来自 manifest，供调用方在预览里展示。
	Version string
	OS      string
	Arch    string
	Binary  string
}

// ErrUnsigned 表示「包未签名、而调用方要求签名（默认）」。它是哨兵错误，便于
// 调用方区分「没签名」与「签名错了」。
var ErrUnsigned = errors.New("upgrade: 包未签名（未提供 -pubkey 且未显式允许未签名包）")

// ErrKeyMismatch 表示「给了 -pubkey 但包未签名」。
var ErrKeyMismatch = errors.New("upgrade: 提供了 -pubkey 但包未签名：签名包必须用对应公钥验证")

// ErrPubkeyRequired 表示「包已签名但没给 -pubkey」。它是哨兵错误：调用方与测试
// 都能凭它区分「真的缺公钥」与「公钥解析/验签顺带报的别的错」——若这里只是随手
// errors.New，一个跳过该分支的缺陷会被后面 decodeKey 的失败掩盖，测试仍全绿。
var ErrPubkeyRequired = errors.New("upgrade: 包已签名但缺少 -pubkey：签名包必须给出对应公钥")

// VerifyOptions 控制校验行为。
type VerifyOptions struct {
	// PublicKeyHex 是 ed25519 公钥的十六进制。非空表示要求签名并验签。
	PublicKeyHex string
	// AllowUnsigned 允许未签名包通过（仅 sha256 完整性检查）。默认 false。
	AllowUnsigned bool
	// RuntimeOS/RuntimeArch 缺省取 runtime.GOOS/GOARCH。校验用注入值，便于测试。
	RuntimeOS   string
	RuntimeArch string
}

// BuildSpec 描述要构建的升级包。
type BuildSpec struct {
	Manifest   Manifest
	BinaryPath string
	BinaryName string // 包内可执行体的名字，默认 "uic"
	// Extras 是附加文件：包内路径 → 磁盘路径（如随包下发的默认配置）。
	Extras map[string]string
	// PrivateKeyHex 若非空，则对 manifest 原文签名并写入 manifest.sig。
	PrivateKeyHex string
}

// Build 从磁盘上的二进制（与可选附加文件）构建一份升级包到 dst。
//
// manifest.Files 会由实际文件内容重新计算 sha256 生成，调用方无需手填，杜绝
// 「manifest 里的哈希与实际内容对不上」这一类人为失误。
func Build(dst string, spec BuildSpec) error {
	if strings.TrimSpace(spec.BinaryPath) == "" {
		return errors.New("upgrade: BuildSpec.BinaryPath 必填")
	}
	binName := strings.TrimSpace(spec.BinaryName)
	if binName == "" {
		binName = "uic"
	}
	binBytes, err := os.ReadFile(spec.BinaryPath)
	if err != nil {
		return fmt.Errorf("upgrade: 读取二进制 %s: %w", spec.BinaryPath, err)
	}
	binInfo, err := os.Stat(spec.BinaryPath)
	if err != nil {
		return fmt.Errorf("upgrade: _stat 二进制: %w", err)
	}

	files := map[string][]byte{
		binName: binBytes,
	}
	for name, diskPath := range spec.Extras {
		if diskPath == "" {
			continue
		}
		b, err := os.ReadFile(diskPath)
		if err != nil {
			return fmt.Errorf("upgrade: 读取附加文件 %s: %w", name, err)
		}
		files[name] = b
	}

	manifest := spec.Manifest
	manifest.Schema = Schema
	manifest.Binary = binName
	if manifest.Version == "" {
		manifest.Version = "dev"
	}
	manifest.Files = nil
	for name := range files {
		sum := sha256.Sum256(files[name])
		manifest.Files = append(manifest.Files, FileEntry{
			Path:   name,
			SHA256: hex.EncodeToString(sum[:]),
			Mode:   modeOf(name, binInfo.Mode().Perm()),
		})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("upgrade: 序列化 manifest: %w", err)
	}
	raw = append(raw, '\n')

	var sigBytes []byte
	signed := strings.TrimSpace(spec.PrivateKeyHex) != ""
	if signed {
		priv, err := decodeKey(spec.PrivateKeyHex)
		if err != nil {
			return fmt.Errorf("upgrade: 私钥: %w", err)
		}
		sigBytes = ed25519.Sign(priv, raw)
	}

	return writeTarGz(dst, raw, sigBytes, files)
}

// Open 读取并解析一份升级包（不解到磁盘，全部在内存）。
func Open(path string) (*Package, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("upgrade: 打开包 %s: %w", path, err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("upgrade: 解包（非 gzip/tar？）: %w", err)
	}
	tr := tar.NewReader(gr)

	pkg := &Package{Files: map[string][]byte{}}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("upgrade: 读取 tar 条目: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("upgrade: 读取条目 %s: %w", hdr.Name, err)
			}
			pkg.Files[cleanName(hdr.Name)] = b
		}
	}
	if err := gr.Close(); err != nil {
		return nil, fmt.Errorf("upgrade: 关闭 gzip: %w", err)
	}

	raw, ok := pkg.Files[ManifestName]
	if !ok {
		return nil, errors.New("upgrade: 包内缺少 manifest.json")
	}
	pkg.Raw = raw
	if err := json.Unmarshal(raw, &pkg.Manifest); err != nil {
		return nil, fmt.Errorf("upgrade: 解析 manifest.json: %w", err)
	}
	if sig, ok := pkg.Files[SignatureName]; ok {
		pkg.Signed = true
		pkg.Sig = sig
	}
	return pkg, nil
}

// Verify 执行分层校验。返回 Report 与错误；校验失败时 Report.OK 为 false 且 err 非空。
func Verify(pkg *Package, opt VerifyOptions) (*Report, error) {
	rep := &Report{
		Version: pkg.Manifest.Version,
		OS:      pkg.Manifest.GoOS,
		Arch:    pkg.Manifest.GoArch,
		Binary:  pkg.Manifest.Binary,
		Signed:  pkg.Signed,
	}
	if pkg == nil {
		return rep, errors.New("upgrade: nil 包")
	}
	if pkg.Manifest.Schema != Schema {
		return rep, fmt.Errorf("upgrade: manifest schema 不匹配：got %q, want %q", pkg.Manifest.Schema, Schema)
	}

	// 1) 来源：签名。
	if pkg.Signed {
		if strings.TrimSpace(opt.PublicKeyHex) == "" {
			return rep, ErrPubkeyRequired
		}
		pub, err := decodeKey(opt.PublicKeyHex)
		if err != nil {
			return rep, fmt.Errorf("upgrade: 公钥: %w", err)
		}
		if !ed25519.Verify(pub, pkg.Raw, pkg.Sig) {
			return rep, errors.New("upgrade: 签名验证失败：manifest 被改动或密钥不匹配")
		}
	} else {
		if strings.TrimSpace(opt.PublicKeyHex) != "" {
			return rep, ErrKeyMismatch
		}
		if !opt.AllowUnsigned {
			return rep, ErrUnsigned
		}
		rep.Warnings = append(rep.Warnings, "未签名包：仅校验了 sha256 完整性，未做来源验证")
	}

	// 2) 适配性：os/arch。
	rtOS := opt.RuntimeOS
	if rtOS == "" {
		rtOS = runtime.GOOS
	}
	rtArch := opt.RuntimeArch
	if rtArch == "" {
		rtArch = runtime.GOARCH
	}
	if pkg.Manifest.GoOS != "" && !strings.EqualFold(pkg.Manifest.GoOS, rtOS) {
		return rep, fmt.Errorf("upgrade: 目标系统不匹配：包是 %s/%s，当前是 %s/%s",
			pkg.Manifest.GoOS, pkg.Manifest.GoArch, rtOS, rtArch)
	}
	if pkg.Manifest.GoArch != "" && !strings.EqualFold(pkg.Manifest.GoArch, rtArch) {
		return rep, fmt.Errorf("upgrade: 目标架构不匹配：包是 %s/%s，当前是 %s/%s",
			pkg.Manifest.GoOS, pkg.Manifest.GoArch, rtOS, rtArch)
	}

	// 3) 完整性：逐文件 sha256。
	if strings.TrimSpace(pkg.Manifest.Binary) == "" {
		return rep, errors.New("upgrade: manifest 未声明 binary 入口")
	}
	for _, fe := range pkg.Manifest.Files {
		b, ok := pkg.Files[cleanName(fe.Path)]
		if !ok {
			return rep, fmt.Errorf("upgrade: 包内缺少文件 %s", fe.Path)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != fe.SHA256 {
			return rep, fmt.Errorf("upgrade: 文件 %s 完整性校验失败（sha256 不符）", fe.Path)
		}
	}

	rep.OK = true
	return rep, nil
}

// ApplyResult 描述一次安装的结果。
type ApplyResult struct {
	// BackupPath 是旧二进制的备份路径（<current>.bak）。失败时为 ""。
	BackupPath string
	// Current 是被替换的二进制路径。
	Current string
	// Reverted 表示因后续步骤失败而回滚了（当前仍是旧版本）。
	Reverted bool
}

// Apply 把 newBinary 原子地安装到 current：先备份 current → <current>.bak，
// 再把 newBinary 以 tmp 写入 current 后 rename 就位。任一步失败都会尽量回滚，
// 保证 current 要么是旧二进制、要么是新二进制，绝不可能是写了一半的文件。
//
// 返回备份路径，供人工回退使用。它不重启进程。
func Apply(current string, newBinary []byte, mode os.FileMode) (ApplyResult, error) {
	res := ApplyResult{Current: current}
	if strings.TrimSpace(current) == "" {
		return res, errors.New("upgrade: current 路径不能为空")
	}

	tmp := current + ".new"
	if err := os.WriteFile(tmp, newBinary, mode); err != nil {
		_ = os.Remove(tmp)
		return res, fmt.Errorf("upgrade: 写入临时文件 %s: %w", tmp, err)
	}

	backup := BackupPath(current)
	// 备份旧二进制（若存在）。备份本身覆盖上一次备份——只保留最近一份回退点。
	var hadBackup bool
	if _, err := os.Stat(current); err == nil {
		if err := os.Rename(current, backup); err != nil {
			_ = os.Remove(tmp)
			return res, fmt.Errorf("upgrade: 备份当前二进制失败: %w", err)
		}
		hadBackup = true
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
		return res, fmt.Errorf("upgrade: stat 当前二进制: %w", err)
	}

	if err := os.Rename(tmp, current); err != nil {
		if hadBackup {
			_ = os.Rename(backup, current)
			res.Reverted = true
		}
		return res, fmt.Errorf("upgrade: 就位新二进制失败，已回滚: %w", err)
	}
	if hadBackup {
		res.BackupPath = backup
	}
	return res, nil
}

// BackupPath 返回给定二进制路径对应的备份路径（<path>.bak）。
func BackupPath(current string) string { return current + ".bak" }

// modeOf 给出包内文件的默认权限：二进制可执行，其余 0644。
func modeOf(name string, binPerm os.FileMode) int {
	if isBinaryName(name) {
		return int(binPerm & 0o777)
	}
	return 0o644
}

func isBinaryName(name string) bool {
	base := filepath.Base(name)
	return base == "uic" || base == "uic.exe" || strings.HasSuffix(base, ".exe")
}

// decodeKey 解析十六进制的 ed25519 公钥或私钥。
func decodeKey(hexStr string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(hexStr))
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize && len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("密钥长度 %d 既不是公钥(%d)也不是私钥(%d)",
			len(b), ed25519.PublicKeySize, ed25519.PrivateKeySize)
	}
	return b, nil
}

// cleanName 规范化 tar 条目名（去掉前导 "./"），与 Build 写入时一致。
func cleanName(name string) string {
	return strings.TrimPrefix(name, "./")
}

// writeTarGz 把 manifest 原文、可选签名与文件表写进一份 gzip+tar。
func writeTarGz(dst string, manifestRaw, sig []byte, files map[string][]byte) error {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := writeEntry(tw, ManifestName, manifestRaw, 0o644); err != nil {
		return err
	}
	if sig != nil {
		if err := writeEntry(tw, SignatureName, sig, 0o644); err != nil {
			return err
		}
	}
	for name := range files {
		mode := os.FileMode(0o644)
		if isBinaryName(name) {
			mode = 0o755
		}
		if err := writeEntry(tw, name, files[name], mode); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

func writeEntry(tw *tar.Writer, name string, data []byte, mode os.FileMode) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(mode & 0o777),
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("upgrade: tar 头 %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("upgrade: tar 体 %s: %w", name, err)
	}
	return nil
}
