// Package i18n 为北向 HTTP API 的错误消息提供按请求语言渲染的能力（中/英）。
//
// 设计约束来自 docs/03-product/05-northbound-api-spec.md §4.9：
//
//   - **对外契约是 `error.code`**。它是稳定的机器可读标识，任何语言下都一模
//     一样；`error.message` 才是给人看的，随语言变化。客户端按 `code` 分支，
//     按 `message` 展示——这是本包存在的全部理由。
//   - **默认语言是英语**。升级到本版本后，没有声明语言偏好的客户端收到的字节
//     与升级前完全一致：不存在「客户端按 message 做断言」被打断的风险。中文
//     是「要了才给」，不是「默认塞给你」。
//   - **解析顺序**：`?lang=` 显式指定 → `Accept-Language` → 默认英语。浏览器
//     默认带 `Accept-Language: zh-CN,zh;q=0.9`，所以 Web 控制台不需要额外传参
//     就能拿到中文；而脚本/监控客户端不传，就该拿到它一直在用的英语。
//   - **翻译以「源语言模板」为 key，而不是错误码**。同一个 code 下有多条不同
//     文案（`request.invalid` 就有十余条），用 code 做 key 只能压成一句无法代
//     入参数的废话。以模板为 key 的代价是「改了源文案，翻译会静默失效」——这
//     个代价用测试抵消：catalog_test.go 用 go/ast 扫出全仓所有 Errf 模板，
//     要求「每条模板要么有翻译、要么在 passThrough 里显式声明」，并校验占位符
//     顺序一致。
//
// 本包不依赖 net/http：HTTP 相关的解析（查询参数、头）留在 httpapi 层，这里
// 只接受字符串，因此可以被任何入口复用、也可以脱离 HTTP 单测。
package i18n

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Locale 是受支持的语言标识（BCP 47 主语言标签）。
type Locale string

const (
	// EN 是英语，也是默认语言。
	EN Locale = "en"
	// ZH 是简体中文。
	ZH Locale = "zh"
)

// Default 是没有任何语言偏好、也没有命中任何受支持语言时使用的语言。
//
// 选英语而不是中文，是为了让「升级」这件事对既有客户端零影响：message 的字节
// 与升级前完全相同。想要中文的调用方显式声明即可（浏览器会自动声明）。
const Default = EN

// Tag 返回可直接写入 Content-Language 响应头的语言标签。
func (l Locale) Tag() string {
	if l == "" {
		return string(Default)
	}
	return string(l)
}

// Supported 报告 l 是否是本包内置翻译的语言。
func (l Locale) Supported() bool {
	_, ok := catalog[l]
	return ok || l == Default
}

// Negotiate 依据显式查询参数与 Accept-Language 头选出本次请求使用的语言。
//
// 两级都落空（或声明了不支持的语言）时返回 Default，永远不返回空值——调用方
// 不需要再判空。
func Negotiate(queryLang, acceptLanguage string) Locale {
	if l, ok := parseTag(queryLang); ok {
		return l
	}
	for _, tag := range preferredTags(acceptLanguage) {
		if l, ok := parseTag(tag); ok {
			return l
		}
	}
	return Default
}

// Translate 把源语言模板渲染成 l 语言的消息。
//
// 没有对应翻译时回退源模板——这也是 Default 语言的行为，因此「默认语言」与
// 「未翻译」在字节上不可区分，没有必要分成两条路径。
func Translate(l Locale, format string, args ...any) string {
	if t, ok := catalog[l][format]; ok {
		return fmt.Sprintf(t, args...)
	}
	return fmt.Sprintf(format, args...)
}

// Has 报告 l 语言下是否存在 format 的翻译。测试与诊断用。
func Has(l Locale, format string) bool {
	_, ok := catalog[l][format]
	return ok
}

// SourceTemplates 返回某个语言 catalog 收录的全部源模板（按字典序）。
func SourceTemplates(l Locale) []string {
	m := catalog[l]
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseTag 把 zh-CN / zh_Hans / ZH 这类标签归一成内置语言。
//
// 只看主语言子标签：`zh-Hans-CN`、`zh-TW`、`zh` 都归到中文。繁体与简体共用一
// 套文案是有意的取舍——本产品的文案以简体撰写，给繁体用户看简体，比给英文更
// 接近他的阅读习惯；真要分繁简，是新增 ZH_HANT 一档，不动这里的结构。
func parseTag(tag string) (Locale, bool) {
	tag = strings.TrimSpace(strings.ToLower(tag))
	// "zh-CN;q=0.9" 这种误把参数塞进标签的写法也容错。
	if i := strings.IndexAny(tag, ";,"); i >= 0 {
		tag = tag[:i]
	}
	tag = strings.ReplaceAll(tag, "_", "-")
	if i := strings.IndexByte(tag, '-'); i >= 0 {
		tag = tag[:i]
	}
	switch tag {
	case "zh":
		return ZH, true
	case "en":
		return EN, true
	}
	// "*"（任意语言）与未知语言都落到下一候选，最终由 Negotiate 兜到 Default。
	return "", false
}

// weighted 是 Accept-Language 里的一个候选。
type weighted struct {
	tag string
	q   float64
	seq int
}

// preferredTags 按 q 值从高到低返回 Accept-Language 里的语言标签。
//
//   - q=0 表示「明确不接受」，直接丢弃（RFC 7231 §5.3.1）。
//   - q 缺省为 1；q 写坏了也按 1 处理——一个拼错的 q 参数不该让整条请求掉回
//     英语，那会让「浏览器发中文、服务端回英文」这类问题极难排查。
//   - q 相同时保持头里的先后顺序（先出现的更优先），所以用稳定排序。
func preferredTags(header string) []string {
	var ws []weighted
	for i, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		tag := strings.TrimSpace(fields[0])
		if tag == "" {
			continue
		}
		q := 1.0
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if len(f) < 2 || !strings.EqualFold(f[:2], "q=") {
				continue
			}
			if v, err := strconv.ParseFloat(strings.TrimSpace(f[2:]), 64); err == nil {
				q = v
			}
		}
		if q <= 0 {
			continue
		}
		ws = append(ws, weighted{tag: tag, q: q, seq: i})
	}
	sort.SliceStable(ws, func(a, b int) bool { return ws[a].q > ws[b].q })
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.tag
	}
	return out
}
