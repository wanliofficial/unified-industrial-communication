package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"uic/internal/config"
)

// TestExamplesAreValid 把 `examples/` 下所有示例配置过一遍真实校验。
//
// 为什么值得单独一个测试：示例是用户复制的第一份配置，它一旦与校验规则
// 漂移，表现就是「照着文档做，第一步就失败」——而单元测试全绿，因为没人
// 拿示例当输入。这个测试曾经抓到过一次真实漂移：Token 最小长度提高后，
// 全部示例里的 dev token 都变成了非法值。
//
// 放在 runtime 包而不是 config 包：校验需要驱动注册表（协议名要能解析到
// 驱动），而注册内置驱动的 `RegisterBuiltins` 在 runtime 里。config 反过来
// 依赖不了 runtime（会成环）。
//
// 只读、不写：不调用 config.Open（会建 snapshots/ 与 state 文件），只做
// 「解析 → Validate」，与加载路径的前半段完全一致。
func TestExamplesAreValid(t *testing.T) {
	if err := RegisterBuiltins(); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no example configs found")
	}
	for _, path := range paths {
		base := filepath.Base(path)
		// 运行时产物（config.state.json、snapshots/*.json）不是示例配置。
		if strings.HasSuffix(base, ".state.json") {
			continue
		}
		t.Run(base, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var f config.File
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := f.Validate(); err != nil {
				t.Fatalf("invalid example %s: %v", base, err)
			}
		})
	}
}
