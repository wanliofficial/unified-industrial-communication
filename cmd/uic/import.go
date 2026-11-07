package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/pointtable"
	uicruntime "uic/internal/runtime"
)

// uic import 把点表 CSV 合并进设备（或整份配置）。
//
// 这是「现场拿到一份 Excel 之后的第一步」。它不做任何网络动作，只做静态
// 解析与合并，因此可以放进 CI、可以在改配置之前反复跑。默认不覆盖已有点位：
// 覆盖会连缩放、死区、单位一起冲掉，而这些恰恰是调试时手工补的。
func importCmd(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	csvPath := fs.String("csv", "", "点表 CSV 文件")
	deviceID := fs.String("device", "", "目标设备 ID")
	channel := fs.String("channel", "", "设备所挂通道 ID（新设备必填；已有设备可省略）")
	protocol := fs.String("protocol", "", "地址协议（省略时取通道协议）")
	interval := fs.Duration("interval", 0, "设备采集周期（新设备默认 1s）")
	name := fs.String("name", "", "设备名称")
	basePath := fs.String("config", "", "基准配置文件；给出时输出整份配置")
	out := fs.String("out", "", "输出文件（默认打印到标准输出）")
	replace := fs.Bool("replace", false, "覆盖已有点位（默认冲突即失败）")
	dryRun := fs.Bool("dry-run", false, "只打印将发生的变更，不写文件")
	asJSON := fs.Bool("json", false, "以 JSON 输出摘要")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `用法:
  uic import -csv points.csv -device press -channel ch-modbus -protocol modbus-tcp
  uic import -csv points.csv -device press -config config.json -out config.new.json
  uic import -csv points.csv -device press -config config.json -dry-run

参数:
  -csv       点表 CSV（必填）
  -device    目标设备 ID（必填）
  -channel   通道 ID；新设备必填，已有设备默认沿用
  -protocol  地址协议；默认取通道的 protocol
  -interval  采集周期；新设备默认 1s
  -name      设备名称
  -config    基准配置；给出时把设备合并回整份配置
  -out       输出文件；不给则打印到标准输出
  -replace   同 ID 点位内容不同时覆盖（默认报冲突）
  -dry-run   只预览，不写文件
  -json      JSON 摘要
`)
	}
	fs.Parse(args)

	if *csvPath == "" || *deviceID == "" {
		fmt.Fprintln(os.Stderr, "缺少 -csv 或 -device")
		os.Exit(2)
	}
	// 配置校验会检查「通道协议是否已注册」；CLI 不是常驻进程，必须自己
	// 注册内置驱动，否则任何配置都会因为「协议未注册」而失败。
	if err := uicruntime.RegisterBuiltins(); err != nil {
		fmt.Fprintf(os.Stderr, "驱动注册失败: %v\n", err)
		os.Exit(1)
	}
	// 1. 基准配置（可选）。
	var base *config.File
	if *basePath != "" {
		raw, err := os.ReadFile(*basePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
			os.Exit(1)
		}
		var f config.File
		if err := decodeStrict(raw, &f); err != nil {
			fmt.Fprintf(os.Stderr, "配置解析失败: %v\n", err)
			os.Exit(1)
		}
		if err := f.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "配置校验失败: %v\n", err)
			os.Exit(1)
		}
		base = &f
	}

	// 2. 定位已有设备与通道，决定协议。
	var existing *model.Device
	if base != nil {
		for i := range base.Devices {
			if base.Devices[i].ID == *deviceID {
				d := base.Devices[i]
				existing = &d
				break
			}
		}
	}
	if existing != nil && *channel == "" {
		*channel = existing.ChannelID
	}

	ch := findChannel(base, *channel)
	if ch == nil && base != nil && *channel != "" {
		fmt.Fprintf(os.Stderr, "配置里没有通道 %q（可选：%s）\n", *channel, channelIDs(base))
		os.Exit(1)
	}
	resolved := *protocol
	if resolved == "" && ch != nil {
		resolved = ch.Protocol
	}
	if resolved == "" {
		fmt.Fprintln(os.Stderr, "无法确定协议：请给 -protocol，或让 -channel 指向配置里已有的通道")
		os.Exit(2)
	}
	if *protocol != "" && ch != nil && !strings.EqualFold(ch.Protocol, *protocol) {
		fmt.Fprintf(os.Stderr, "协议冲突：-protocol=%s，但通道 %q 是 %s\n", *protocol, ch.ID, ch.Protocol)
		os.Exit(1)
	}

	// 3. 解析点表。CSV 错误一次性全部列出（带行号与列名）。
	f, err := os.Open(*csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开点表失败: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	parsed, err := pointtable.ParseCSV(f, resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "点表解析失败:\n%v\n", err)
		os.Exit(1)
	}

	// 4. 组装设备。
	var dev model.Device
	if existing != nil {
		dev = *existing
	} else {
		if *channel == "" {
			fmt.Fprintln(os.Stderr, "新设备必须给 -channel")
			os.Exit(2)
		}
		dev = model.Device{
			ID:        *deviceID,
			Name:      *name,
			ChannelID: *channel,
			Interval:  model.Duration(time.Second),
			Enabled:   true,
		}
	}
	if *channel != "" {
		dev.ChannelID = *channel
	}
	if *name != "" {
		dev.Name = *name
	}
	if *interval != 0 {
		if *interval < 100*time.Millisecond {
			fmt.Fprintln(os.Stderr, "-interval 必须 >= 100ms")
			os.Exit(2)
		}
		dev.Interval = model.Duration(*interval)
	}
	if dev.ID != *deviceID {
		dev.ID = *deviceID
	}

	merged, actions, err := pointtable.Merge(dev.Points, parsed.Points, *replace)
	if err != nil {
		s := importSummary{
			Device: dev.ID, Channel: dev.ChannelID, Protocol: resolved,
			Points: len(merged), Actions: actions, Counts: pointtable.Count(actions),
		}
		for _, w := range parsed.Warnings {
			s.Warnings = append(s.Warnings, w.String())
		}
		printSummary(s, *asJSON, os.Stderr)
		fmt.Fprintf(os.Stderr, "合并失败: %v\n", err)
		os.Exit(1)
	}
	dev.Points = merged
	if err := dev.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "设备不合法: %v\n", err)
		os.Exit(1)
	}

	// 5. 输出。dry-run 与真写走的是同一份合并结果，预览不会和实际结果分叉。
	summary := importSummary{
		Device:   dev.ID,
		Channel:  dev.ChannelID,
		Protocol: resolved,
		DryRun:   *dryRun,
		Out:      *out,
		Points:   len(dev.Points),
		Actions:  actions,
		Counts:   pointtable.Count(actions),
	}
	for _, w := range parsed.Warnings {
		summary.Warnings = append(summary.Warnings, w.String())
	}

	if *dryRun {
		printSummary(summary, *asJSON)
		return
	}

	var document []byte
	if base != nil {
		replaced := false
		for i := range base.Devices {
			if base.Devices[i].ID == dev.ID {
				base.Devices[i] = dev
				replaced = true
				break
			}
		}
		if !replaced {
			base.Devices = append(base.Devices, dev)
		}
		if err := base.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "合并后的配置不合法: %v\n", err)
			os.Exit(1)
		}
		document, err = base.Canonical()
		if err != nil {
			fmt.Fprintf(os.Stderr, "序列化配置失败: %v\n", err)
			os.Exit(1)
		}
	} else {
		document, err = json.MarshalIndent(dev, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "序列化设备失败: %v\n", err)
			os.Exit(1)
		}
		document = append(document, '\n')
	}

	if *out == "" {
		os.Stdout.Write(document)
		if *asJSON {
			// 文档已经占了标准输出，摘要改走标准错误。
			printSummary(summary, true, os.Stderr)
		}
		return
	}
	if err := config.WriteFile(*out, document, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", *out, err)
		os.Exit(1)
	}
	summary.Out = *out
	if *asJSON {
		printSummary(summary, true, os.Stdout)
	} else {
		printSummary(summary, false, os.Stderr)
	}
}

type importSummary struct {
	Device   string                        `json:"device"`
	Channel  string                        `json:"channel"`
	Protocol string                        `json:"protocol"`
	DryRun   bool                          `json:"dryRun"`
	Out      string                        `json:"out,omitempty"`
	Points   int                           `json:"points"`
	Actions  []pointtable.Action           `json:"actions"`
	Counts   map[pointtable.ActionKind]int `json:"counts"`
	Warnings []string                      `json:"warnings,omitempty"`
}

func printSummary(s importSummary, asJSON bool, w ...*os.File) {
	out := os.Stdout
	if len(w) > 0 && w[0] != nil {
		out = w[0]
	}
	if asJSON {
		raw, _ := json.MarshalIndent(s, "", "  ")
		fmt.Fprintf(out, "%s\n", raw)
		return
	}
	newN := s.Counts[pointtable.ActionNew]
	un := s.Counts[pointtable.ActionUnchanged]
	rp := s.Counts[pointtable.ActionReplaced]
	cf := s.Counts[pointtable.ActionConflict]
	verb := "导入"
	if s.DryRun {
		verb = "预览"
	}
	fmt.Fprintf(out, "%s %s：共 %d 点（新增 %d、未变 %d、替换 %d、冲突 %d）\n",
		verb, s.Device, s.Points, newN, un, rp, cf)
	for _, w := range s.Warnings {
		fmt.Fprintf(out, "  警告: %s\n", w)
	}
	for _, a := range s.Actions {
		if a.Kind == pointtable.ActionNew || a.Kind == pointtable.ActionUnchanged {
			continue
		}
		fmt.Fprintf(out, "  %-9s %s: %s\n", a.Kind, a.PointID, a.Reason)
	}
	if !s.DryRun && s.Out != "" {
		fmt.Fprintf(out, "已写入 %s\n", s.Out)
	}
}

func findChannel(f *config.File, id string) *model.Channel {
	if f == nil || id == "" {
		return nil
	}
	for i := range f.Channels {
		if f.Channels[i].ID == id {
			return &f.Channels[i]
		}
	}
	return nil
}

func channelIDs(f *config.File) string {
	if f == nil || len(f.Channels) == 0 {
		return "无"
	}
	out := make([]string, 0, len(f.Channels))
	for _, c := range f.Channels {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
