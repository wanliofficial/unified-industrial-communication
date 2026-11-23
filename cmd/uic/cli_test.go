package main

import (
	"strings"
	"testing"

	"uic/internal/config"
	"uic/internal/model"
)

func TestParamFlagsMap(t *testing.T) {
	var p paramFlags
	if err := p.Set("index=1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("name=  line a  "); err != nil {
		t.Fatal(err)
	}
	m, err := p.Map()
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if m["index"] != "1" {
		t.Fatalf("index = %q", m["index"])
	}
	// 只裁掉键两侧空白，值的空白可能是有意义的（名称里带空格）。
	if m["name"] != "  line a  " {
		t.Fatalf("name = %q", m["name"])
	}
}

func TestParamFlagsRejects(t *testing.T) {
	cases := []paramFlags{
		{"novalue"},
		{"=x"},
		{"a=1", "a=2"},
	}
	for _, c := range cases {
		if _, err := c.Map(); err == nil {
			t.Fatalf("%v must fail", c)
		}
	}
}

func TestFindChannelAndIDs(t *testing.T) {
	f := &config.File{Channels: []model.Channel{
		{ID: "ch-b", Protocol: "s7"},
		{ID: "ch-a", Protocol: "modbus-tcp"},
	}}
	if findChannel(nil, "ch-a") != nil {
		t.Fatal("nil config must yield nil channel")
	}
	if findChannel(f, "") != nil {
		t.Fatal("empty id must yield nil channel")
	}
	ch := findChannel(f, "ch-a")
	if ch == nil || ch.Protocol != "modbus-tcp" {
		t.Fatalf("channel = %+v", ch)
	}
	if ids := channelIDs(f); ids != "ch-a, ch-b" {
		t.Fatalf("ids = %q", ids)
	}
	if ids := channelIDs(nil); ids != "无" {
		t.Fatalf("nil ids = %q", ids)
	}
	if !strings.Contains(channelIDs(&config.File{}), "无") {
		t.Fatal("empty config must report 无")
	}
}

// TestCommandTableCoversUsage 保证 usage 里宣传的每个 `uic <cmd>` 子命令都能
// 被 main 派发出去。
//
// 为什么需要它：子命令曾经是一段手写 switch，而 usage 文本与 handler 实现
// 各自维护。「实现了 handler、usage 也写了、唯独 switch 漏一个 case」的症状
// 是 `uic mbread` 直接报 unknown command——usage 里明明有它。这类不一致在
// 编译期看不出来，只能靠一条对照检查兜住。现在 dispatch 由 commands 表驱动，
// 本测试把「usage 宣传」与「表里有」这两件事钉在一起。
func TestCommandTableCoversUsage(t *testing.T) {
	// usage 里以 "  uic " 开头的行是命令示例，取每行第一个命令词。
	for _, line := range strings.Split(usageText, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "uic ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		cmd := fields[1]
		// 去掉 `[run]` 这类可选写法与 `-flag` 开头（那是 `uic -check` 的全局参数）。
		cmd = strings.Trim(cmd, "[]")
		if cmd == "" || strings.HasPrefix(cmd, "-") {
			continue
		}
		// 只认纯字母数字的子命令名；标题行「uic — 统一…」的门面词会被跳过。
		if !isCommandWord(cmd) {
			continue
		}
		if cmd == "run" || cmd == "version" {
			// main 直接处理的特例（默认命令 + version），不在表里。
			continue
		}
		if _, ok := commands[cmd]; !ok {
			t.Errorf("usage 宣传了 %q，但 commands 表里没有它（`uic %s` 会报 unknown command）", cmd, cmd)
		}
	}
}

// TestCommandTableNonEmpty 防止表被误删空——空表会让所有子命令都走进
// unknown command 分支，而上面的测试此时什么都不检查、静默通过。
func TestCommandTableNonEmpty(t *testing.T) {
	if len(commands) < 20 {
		t.Fatalf("commands 表只有 %d 项，疑似被误删", len(commands))
	}
	for name, fn := range commands {
		if fn == nil {
			t.Errorf("子命令 %q 的 handler 为 nil", name)
		}
	}
}

// isCommandWord 判断一个 token 是否像子命令名（纯字母数字）。
func isCommandWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
