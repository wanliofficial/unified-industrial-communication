// 二进制 sbomgen 从编译产物中提取模块清单，生成 CycloneDX SBOM。
//
// 存在理由：docs/todo.md 的 G12 要求「SBOM、CI 扫描」。本项目依赖面按构造
// 为零（ADR-0004：无第三方 import、无 go.sum / vendor），所以 SBOM 不需要任何
// 第三方扫描器——标准库自带的 `go version -m -json <产物>` 已经把「构建时实际
// 链接了哪些模块、什么版本、什么构建标志」如实记录在二进制里。本工具只读这
// 段内嵌清单并转成 CycloneDX，不联网、不依赖第三方库、可复现。
//
// 用法：
//
//	go run ./tools/sbomgen -binary dist/uic-linux-amd64 -output dist/sbom.cdx.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// buildInfo 是 `go version -m -json <产物>` 输出的子集。go 在编译时把模块图
// 内嵌进二进制（path: "debug/buildinfo"），这里只取 SBOM 关心的字段。
type buildInfo struct {
	GoVersion string `json:"GoVersion"`
	Path      string `json:"Path"`
	Main      module `json:"Main"`
	Deps      []module `json:"Deps,omitempty"`
}

type module struct {
	Path    string  `json:"Path"`
	Version string  `json:"Version"`
	Replace *module `json:"Replace,omitempty"`
}

// CycloneDX 1.5 文档的最小子集：声明应用自身、工具链（Go）与第三方组件。
// 零依赖项目里 components 为空，dependencies 为空——这正是「依赖面按构造
// 为零」的可机读表达。
type sbomDoc struct {
	BOMFormat   string       `json:"bomFormat"`
	SpecVersion string       `json:"specVersion"`
	Version     int          `json:"version"`
	Metadata    sbomMetadata `json:"metadata"`
	Components  []sbomComponent `json:"components,omitempty"`
}

type sbomMetadata struct {
	Timestamp string       `json:"timestamp"`
	Tools     sbomTools    `json:"tools"`
	Component sbomComponent `json:"component"`
}

type sbomTools struct {
	Components []sbomComponent `json:"components"`
}

type sbomComponent struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	BOMRef  string `json:"bom-ref,omitempty"`
	PURL    string `json:"purl,omitempty"`
}

// goPURL 把 Go module 路径/版本翻成 Package URL（pkg:golang/<path>@<version>）。
func goPURL(path, version string) string {
	if version == "" {
		return "pkg:golang/" + path
	}
	return "pkg:golang/" + path + "@" + version
}

func main() {
	var (
		binary = flag.String("binary", "", "待扫描的编译产物路径（必填）")
		output = flag.String("output", "sbom.cdx.json", "SBOM 输出路径")
	)
	flag.Parse()

	if *binary == "" {
		fatalf("-binary 是必填项")
	}
	info, err := readBuildInfo(*binary)
	if err != nil {
		fatalf("读取产物模块清单失败：%v", err)
	}

	doc := buildDoc(info, *binary)
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fatalf("序列化 SBOM 失败：%v", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(*output, raw, 0o644); err != nil {
		fatalf("写出 SBOM 失败：%v", err)
	}
	// 产物指纹打到 stderr：不进 SBOM（否则同一产物两次生成会因时间戳之外
	// 的字段产生 diff），但留在终端供调用方核对「这份 SBOM 对应哪个二进制」。
	if fp := fingerprint(*binary); fp != "" {
		fmt.Fprintf(os.Stderr, "  产物 sha256: %s\n", fp)
	}
	fmt.Printf("SBOM 已生成：%s（组件数 %d，Go %s）\n",
		*output, len(doc.Components), info.GoVersion)
}

// readBuildInfo 跑 `go version -m -json` 并解析内嵌清单。
func readBuildInfo(binary string) (buildInfo, error) {
	cmd := exec.Command("go", "version", "-m", "-json", binary)
	out, err := cmd.Output()
	if err != nil {
		return buildInfo{}, fmt.Errorf("执行 go version -m：%w", err)
	}
	var info buildInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return buildInfo{}, fmt.Errorf("解析 buildinfo JSON：%w", err)
	}
	if info.Main.Path == "" {
		return buildInfo{}, fmt.Errorf("产物 %s 未内嵌模块清单（请用当前 Go 工具链构建）", binary)
	}
	return info, nil
}

// buildDoc 把 buildinfo 组装成 CycloneDX 文档。
func buildDoc(info buildInfo, binary string) sbomDoc {
	mainVersion := info.Main.Version
	if mainVersion == "" {
		mainVersion = "dev"
	}
	goVer := info.GoVersion
	if goVer == "" {
		goVer = "unknown"
	}

	doc := sbomDoc{
		BOMFormat:   "CycloneDX",
		SpecVersion: "1.5",
		Version:     1,
		Metadata: sbomMetadata{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tools: sbomTools{Components: []sbomComponent{{
				Type:    "application",
				Name:    "go",
				Version: goVer,
				BOMRef:  goVer,
				PURL:    "pkg:golang/go@" + goVer,
			}}},
			Component: sbomComponent{
				Type:    "application",
				Name:    info.Main.Path,
				Version: mainVersion,
				BOMRef:  info.Main.Path + "@" + mainVersion,
				PURL:    goPURL(info.Main.Path, mainVersion),
			},
		},
	}

	// 第三方依赖逐个落成 component + dependency 边。零依赖项目里这段为空。
	for _, dep := range info.Deps {
		path, version := dep.Path, dep.Version
		if dep.Replace != nil { // replace 指令：SBOM 记被替换后的真实来源
			path, version = dep.Replace.Path, dep.Replace.Version
		}
		doc.Components = append(doc.Components, sbomComponent{
			Type:    "library",
			Name:    path,
			Version: version,
			BOMRef:  path + "@" + version,
			PURL:    goPURL(path, version),
		})
	}
	return doc
}

// fingerprint 给出产物的 sha256，便于校验 SBOM 与产物一致（打印到 stderr，
// 不进 SBOM，避免同一产物两次生成产生 diff）。
func fingerprint(binary string) string {
	raw, err := os.ReadFile(binary)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sbomgen: "+format+"\n", args...)
	os.Exit(1)
}
