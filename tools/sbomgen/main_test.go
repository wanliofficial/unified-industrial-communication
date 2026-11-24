package main

import (
	"testing"
)

func TestBuildDocZeroDeps(t *testing.T) {
	// 零依赖项目（本项目常态）：components 必须为空，主组件与工具链如实记录。
	info := buildInfo{
		GoVersion: "go1.26.10",
		Main:      module{Path: "uic", Version: "v1.2.3"},
	}
	doc := buildDoc(info, "dist/uic-linux-amd64")

	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != "1.5" {
		t.Fatalf("文档头错误：format=%q spec=%q", doc.BOMFormat, doc.SpecVersion)
	}
	if len(doc.Components) != 0 {
		t.Fatalf("零依赖项目 components 应为 0，实得 %d：%+v", len(doc.Components), doc.Components)
	}
	if got := doc.Metadata.Component.Name; got != "uic" {
		t.Fatalf("主组件名 = %q，want uic", got)
	}
	if got := doc.Metadata.Component.Version; got != "v1.2.3" {
		t.Fatalf("主组件版本 = %q，want v1.2.3", got)
	}
	tc := doc.Metadata.Tools.Components
	if len(tc) != 1 || tc[0].Name != "go" || tc[0].Version != "go1.26.10" {
		t.Fatalf("工具链记录错误：%+v", tc)
	}
}

func TestBuildDocMainVersionDefaultsToDev(t *testing.T) {
	// 产物未嵌版本（例如非 VCS 构建）时，主组件版本回退到 dev，不留空串。
	info := buildInfo{GoVersion: "go1.26.10", Main: module{Path: "uic"}}
	if got := buildDoc(info, "x").Metadata.Component.Version; got != "dev" {
		t.Fatalf("空版本应回退 dev，实得 %q", got)
	}
}

func TestBuildDocRecordsDepsWithPurl(t *testing.T) {
	// 有第三方依赖时：每个依赖落成 library component，purl 按 golang 生态拼。
	info := buildInfo{
		GoVersion: "go1.26.10",
		Main:      module{Path: "uic", Version: "v1.0.0"},
		Deps: []module{
			{Path: "golang.org/x/text", Version: "v0.3.0"},
		},
	}
	doc := buildDoc(info, "uic")
	if len(doc.Components) != 1 {
		t.Fatalf("components = %d，want 1", len(doc.Components))
	}
	c := doc.Components[0]
	if c.Type != "library" || c.Name != "golang.org/x/text" || c.Version != "v0.3.0" {
		t.Fatalf("依赖 component 字段错误：%+v", c)
	}
	if want := "pkg:golang/golang.org/x/text@v0.3.0"; c.PURL != want {
		t.Fatalf("purl = %q，want %q", c.PURL, want)
	}
}

func TestBuildDocReplaceDirective(t *testing.T) {
	// replace 指令：SBOM 记被替换后的真实来源，而非原始 require 路径。
	info := buildInfo{
		GoVersion: "go1.26.10",
		Main:      module{Path: "uic", Version: "v1.0.0"},
		Deps: []module{{
			Path:    "example.com/fork/pkg",
			Version: "v0.0.0",
			Replace: &module{Path: "real.com/pkg", Version: "v1.5.0"},
		}},
	}
	doc := buildDoc(info, "uic")
	if len(doc.Components) != 1 {
		t.Fatalf("components = %d，want 1", len(doc.Components))
	}
	c := doc.Components[0]
	if c.Name != "real.com/pkg" || c.Version != "v1.5.0" {
		t.Fatalf("replace 未生效：%+v", c)
	}
	if want := "pkg:golang/real.com/pkg@v1.5.0"; c.PURL != want {
		t.Fatalf("replace 后 purl = %q，want %q", c.PURL, want)
	}
}

func TestGoPURL(t *testing.T) {
	cases := []struct{ path, ver, want string }{
		{"uic", "v1.0.0", "pkg:golang/uic@v1.0.0"},
		{"golang.org/x/sys", "v0.1.0", "pkg:golang/golang.org/x/sys@v0.1.0"},
		{"uic", "", "pkg:golang/uic"}, // 无版本时省略 @
	}
	for _, tc := range cases {
		if got := goPURL(tc.path, tc.ver); got != tc.want {
			t.Fatalf("goPURL(%q,%q) = %q，want %q", tc.path, tc.ver, got, tc.want)
		}
	}
}
