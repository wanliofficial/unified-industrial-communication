package main

import (
	"testing"

	toledodrv "uic/internal/driver/toledo"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：字段名必须在发出任何帧
// 之前就拦住，并且报错要说清错在哪一层——Toledo 的失败在串口上全都表现成「没有
// 输出」，如果这里把字段名错报成超时，人会先去改波特率。

func TestToledoRefs(t *testing.T) {
	got := toledoRefs("  Weight , STATUS ,, Unit ")
	want := []string{"weight", "status", "unit"}
	if len(got) != len(want) {
		t.Fatalf("got %v，应为 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v，应为 %v", got, want)
		}
	}
}

func TestToledoDefaultType(t *testing.T) {
	cases := map[string]string{
		"weight":   "float64",
		"status":   "string",
		"unit":     "string",
		"mode":     "string",
		"stable":   "bool",
		"motion":   "bool",
		"decimals": "int64",
	}
	for field, want := range cases {
		if got := string(toledoDefaultType(field)); got != want {
			t.Errorf("defaultType(%q) = %s，应为 %s", field, got, want)
		}
	}
}

func TestToledoFieldValidation(t *testing.T) {
	// 合法字段名（大小写不敏感，CLI 会先小写）都应被 KnownSymbol 认出。
	for _, good := range []string{"weight", "status", "unit", "mode", "stable", "motion", "decimals"} {
		if !toledodrv.KnownSymbol(good) {
			t.Fatalf("KnownSymbol(%q) 应为 true", good)
		}
	}
	// 非法字段名：把 status 写成中文名、把 weight 写成寄存器语法，都应在连秤前被拒。
	for _, bad := range []string{"温度", "0", "w", "gross_weight", "净重"} {
		if toledodrv.KnownSymbol(bad) {
			t.Fatalf("KnownSymbol(%q) 应为 false", bad)
		}
	}
}

func TestToledoFieldDescCoversAll(t *testing.T) {
	// 每个合法字段都应有一句话说明（自检命令的可读性依赖它）。
	for _, f := range []string{"weight", "status", "mode", "unit", "stable", "motion", "decimals"} {
		if toledoFieldDesc(f) == "" {
			t.Errorf("字段 %q 缺少说明", f)
		}
	}
}
