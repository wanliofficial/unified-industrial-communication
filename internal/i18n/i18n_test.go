package i18n

import (
	"strings"
	"testing"
)

func TestTagAndSupported(t *testing.T) {
	if got := Locale("").Tag(); got != "en" {
		t.Fatalf("empty locale tag = %q, want en", got)
	}
	if got := ZH.Tag(); got != "zh" {
		t.Fatalf("ZH tag = %q", got)
	}
	cases := map[Locale]bool{EN: true, ZH: true, "fr": false, "": false}
	for l, want := range cases {
		if got := l.Supported(); got != want {
			t.Fatalf("Supported(%q) = %v, want %v", l, got, want)
		}
	}
}

// Negotiate 是「哪个语言」这一个问题的全部答案，所以边界条件都压在这张表里。
func TestNegotiate(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		header string
		want   Locale
	}{
		// 两级都空 → 默认英语（升级零影响）。
		{"nothing", "", "", EN},
		// 显式查询参数，大小写 / 地区后缀 / 下划线都要能认。
		{"query zh", "zh", "", ZH},
		{"query ZH upper", "ZH", "", ZH},
		{"query zh-CN", "zh-CN", "", ZH},
		{"query zh_Hans_CN", "zh_Hans_CN", "", ZH},
		{"query en-US", "en-US", "", EN},
		// 查询参数不认识 → 退到 header 而不是直接掉回默认。
		{"unsupported query defers to header", "fr", "zh-CN,zh;q=0.9", ZH},
		{"unsupported query no header", "fr", "", EN},
		// 查询参数优先于 header。
		{"query beats header", "en", "zh-CN,zh;q=0.9", EN},
		// 典型浏览器头。
		{"browser zh", "", "zh-CN,zh;q=0.9,en;q=0.8", ZH},
		{"browser en", "", "en-US,en;q=0.9", EN},
		// 不支持的语言被跳过，继续找下一个。
		{"skip unsupported", "", "fr-FR,fr;q=0.9,zh;q=0.5", ZH},
		// q 值决定优先级，与出现顺序无关。
		{"q prefers en", "", "zh;q=0.2, en;q=0.9", EN},
		{"q prefers zh", "", "en;q=0.2, zh;q=0.9", ZH},
		// q 相同 → 头里先出现的赢（稳定排序）。
		{"q tie keeps order", "", "en;q=0.5, zh;q=0.5", EN},
		{"q tie keeps order 2", "", "zh;q=0.5, en;q=0.5", ZH},
		// q=0 是「明确不接受」，必须丢弃，不能因为它是第一个就选中。
		{"q zero dropped", "", "zh;q=0, en", EN},
		{"q zero only", "", "zh;q=0", EN},
		// 通配符不退化成任意语言。
		{"wildcard", "", "*", EN},
		// 空格 / 空项容错。
		{"whitespace", "", " zh ; q = 0.8 , en ; q = 0.7 ", ZH},
		{"empty items", "", ",,,,zh", ZH},
		// q 写坏了按 1 处理，不让整条请求掉回英语。
		{"bogus q", "", "zh;q=bogus", ZH},
		// 带地区的中文在 header 里多次出现，仍归到中文。
		{"zh variants", "", "zh-CN;q=0.9, en;q=0.8, zh-Hans;q=0.7", ZH},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Negotiate(tc.query, tc.header); got != tc.want {
				t.Fatalf("Negotiate(%q, %q) = %q, want %q", tc.query, tc.header, got, tc.want)
			}
		})
	}
}

func TestTranslate(t *testing.T) {
	// 有翻译：代入参数、换成中文。
	if got := Translate(ZH, "device %q not found", "plc-01"); got != `设备 "plc-01" 不存在` {
		t.Fatalf("ZH translate = %q", got)
	}
	// 英语：源模板，与引入 i18n 之前逐字节一致。
	if got := Translate(EN, "device %q not found", "plc-01"); got != `device "plc-01" not found` {
		t.Fatalf("EN translate = %q", got)
	}
	// 空语言等价于默认语言。
	if got := Translate("", "writes must not be empty"); got != "writes must not be empty" {
		t.Fatalf("empty locale = %q", got)
	}
	// 未收录的模板回退源模板（不是空串、不是占位符）。
	if got := Translate(ZH, "brand new message %d", 3); got != "brand new message 3" {
		t.Fatalf("fallback = %q", got)
	}
	// 未支持的语言回退源模板。
	if got := Translate("fr", "writes must not be empty"); got != "writes must not be empty" {
		t.Fatalf("unsupported locale = %q", got)
	}
}

func TestHas(t *testing.T) {
	const deviceNotFound = "device %q not found"
	if !Has(ZH, deviceNotFound) {
		t.Fatalf("ZH should have a translation for %q", deviceNotFound)
	}
	if Has(EN, deviceNotFound) {
		t.Fatal("EN is the source language; it must not carry a catalog entry")
	}
	if Has(ZH, "brand new message %v") {
		t.Fatal("unknown template must not report as translated")
	}
}

func TestSourceTemplatesSorted(t *testing.T) {
	got := SourceTemplates(ZH)
	if len(got) == 0 {
		t.Fatal("empty catalog")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("SourceTemplates not sorted: %q before %q", got[i-1], got[i])
		}
	}
	// 同一份数据必须每次返回同样的顺序，否则内容哈希/diff 类断言会抖。
	if strings.Join(got, "\x00") != strings.Join(SourceTemplates(ZH), "\x00") {
		t.Fatal("SourceTemplates is not deterministic")
	}
}
