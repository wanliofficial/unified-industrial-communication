package i18n

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// passThrough 列出「不含自然语言、不需要翻译」的模板。
//
// 没有译文不是遗漏：这些模板只把底层错误文本原样塞进信封（`%v` 是 `err`，
// `from: %v` 是查询参数名 + 时间解析错误）。给它们编中文只会制造噪音，回退源
// 模板才是正确行为。任何新增条目都必须在这里写明理由——测试会要求「Errf 用
// 到的每条模板，要么有译文、要么在这张表里」，所以漏掉一条会直接变红。
var passThrough = map[string]string{
	"%v":       "只透传底层错误文本",
	"%s":       "只透传底层错误文本",
	"%s: %v":   "透传底层错误文本，前面加一段上下文",
	"from: %v": "只有查询参数名 from 与时间解析错误",
	"to: %v":   "只有查询参数名 to 与时间解析错误",
}

// templateCollectors 是「把 format 参数原样转发给 Errf」的包装函数，以及它的
// format 参数下标。它们自身不是模板来源，但它们的调用点是。
var templateCollectors = map[string]int{
	"badConfig": 0,
}

// templates 扫描全仓非测试 Go 源码，收集所有经过 Errf 的源语言模板。
// 返回 模板 → 首个出现位置，便于失败时直接指到文件行号。
//
// 三个用例都要这份数据，所以只扫一次（整仓 parse 不便宜，而测试跑在 -race 下）。
func templates(t *testing.T) map[string]string {
	t.Helper()
	scanOnce.Do(func() { scanned, scanErr = scanTemplates() })
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	return scanned
}

var (
	scanOnce sync.Once
	scanned  map[string]string
	scanErr  error
)

func scanTemplates() (map[string]string, error) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	out := make(map[string]string)

	var files []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "vendor" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
	}
	if len(files) < 10 {
		return nil, fmt.Errorf("only %d source files found under %q; the scan root is wrong", len(files), root)
	}

	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			idx, ok := formatArgIndex(call.Fun)
			if !ok {
				return true
			}
			if len(call.Args) <= idx {
				return true
			}
			lit, ok := call.Args[idx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				// format 是变量：内容在别处构造，无法静态收集（例如已经由
				// templateCollectors 里的包装函数转发）。
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if _, dup := out[s]; !dup {
				out[s] = fset.Position(call.Pos()).String()
			}
			return true
		})
	}
	return out, nil
}

// formatArgIndex 判断被调函数是不是模板来源，并返回其 format 参数下标。
func formatArgIndex(fun ast.Expr) (int, bool) {
	name := ""
	switch f := fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		name = f.Sel.Name
	default:
		return 0, false
	}
	if name == "Errf" {
		// Errf(status, code, format, args...)
		return 2, true
	}
	if idx, ok := templateCollectors[name]; ok {
		return idx, true
	}
	return 0, false
}

// TestEveryErrfTemplateIsTranslated 是「没有静默漏翻」的闸门。
func TestEveryErrfTemplateIsTranslated(t *testing.T) {
	all := templates(t)
	var missing []string
	for tmpl, pos := range all {
		if Has(ZH, tmpl) {
			continue
		}
		if _, ok := passThrough[tmpl]; ok {
			continue
		}
		missing = append(missing, pos+"  "+tmpl)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("以下 %d 条 Errf 模板既没有中文翻译、也没有在 passThrough 里声明"+
			"（新增/修改错误消息时请同步 internal/i18n/catalog.go）:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestCatalogHasNoDeadEntries 反向校验：catalog 里的 key 必须仍然被代码使用。
// 改了源文案却忘了改 key，译文就会永远不生效——这比漏翻更隐蔽，因为测试全绿。
func TestCatalogHasNoDeadEntries(t *testing.T) {
	all := templates(t)
	var dead []string
	for _, key := range SourceTemplates(ZH) {
		if _, ok := all[key]; ok {
			continue
		}
		if _, ok := passThrough[key]; ok {
			continue
		}
		dead = append(dead, key)
	}
	if len(dead) > 0 {
		t.Fatalf("catalog 里有 %d 条 key 在源码中已找不到（文案被改了？）:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}

// TestPassThroughIsLive 防止 passThrough 变成「历史垃圾场」：里面的条目也必须
// 仍然被代码使用。
func TestPassThroughIsLive(t *testing.T) {
	all := templates(t)
	var stale []string
	for key := range passThrough {
		if _, ok := all[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("passThrough 里的模板在源码中已不存在，请删除: %v", stale)
	}
}

// TestTranslationsPreservePlaceholders 校验译文的占位符与源模板「同序同型」。
//
// 这是 i18n 最容易出错的地方，而且出错后表现是「静默产生 %!q(MISSING)」或
// 「设备 ID 丢了引号」——运行时看不出来，只能在测试里卡死。
func TestTranslationsPreservePlaceholders(t *testing.T) {
	for _, key := range SourceTemplates(ZH) {
		val := catalog[ZH][key]
		kv, kok := verbs(key)
		vv, vok := verbs(val)
		if !kok {
			t.Fatalf("源模板里有无法识别的 %% 序列: %q", key)
		}
		if !vok {
			t.Fatalf("译文里有无法识别的 %% 序列（写成字面 %% 需要 %%%%）: %q", val)
		}
		if !slices.Equal(kv, vv) {
			t.Fatalf("占位符不一致:\n  源: %q -> %v\n  译: %q -> %v", key, kv, val, vv)
		}
		args := sampleArgs(kv)
		if got := fmt.Sprintf(val, args...); strings.Contains(got, "%!") {
			t.Fatalf("译文渲染出格式错误 %q（源: %q，参数: %v）", got, key, args)
		}
		if got := fmt.Sprintf(key, args...); strings.Contains(got, "%!") {
			t.Fatalf("源模板渲染出格式错误 %q（参数: %v）", got, args)
		}
		// 含自然语言的模板必须真的翻译了：原样照抄 key 等于没做。
		if hasLetter(key) && val == key {
			t.Fatalf("译文与源模板完全相同，等价于没翻译: %q", key)
		}
	}
}

// verbs 返回模板里按顺序出现的格式化动词（含 flags/宽度，如 "%q"、"%02d"）。
// 第二个返回值报告是否遇到无法识别的 % 序列——那说明模板本身写错了。
func verbs(s string) ([]string, bool) {
	var out []string
	ok := true
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '%' {
			i++ // 转义的字面百分号
			continue
		}
		j := i + 1
		for j < len(s) && strings.ContainsRune("+-# 0123456789.*[]", rune(s[j])) {
			j++
		}
		if j >= len(s) || !strings.ContainsRune("vTtbcdoOqxXUeEfgGspw", rune(s[j])) {
			ok = false
			continue
		}
		out = append(out, s[i:j+1])
		i = j
	}
	return out, ok
}

func sampleArgs(vs []string) []any {
	out := make([]any, 0, len(vs))
	for _, v := range vs {
		switch v[len(v)-1] {
		case 'd', 'b', 'o', 'O', 'x', 'X', 'c', 'U':
			out = append(out, 7)
		case 'e', 'E', 'f', 'g', 'G':
			out = append(out, 1.5)
		case 't':
			out = append(out, true)
		default:
			out = append(out, "id-1")
		}
	}
	return out
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}
