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
