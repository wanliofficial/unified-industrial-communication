package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	mtcodec "uic/internal/codec/mtconnect"
	"uic/internal/driver"
	mtconndrv "uic/internal/driver/mtconnect"
	"uic/internal/model"
)

// mtReader 是驱动的自检接口。用类型断言而不是把它塞进 driver.Conn：
// 生产采集只需要 Read/Write/Probe，/probe、/current、/sample 是自检工具的能力
// （与 CoAP 的 discover、SNMP 的 walk 同例）。
type mtReader interface {
	ProbeDevices(context.Context) (*mtcodec.Devices, string, error)
	Current(context.Context) (*mtcodec.Streams, string, error)
	Sample(context.Context, uint64, int) (*mtcodec.Streams, string, error)
	BaseURL() string
}

// uic mtconnectread 是 MTConnect Agent 的自检命令。
//
// MTConnect 的「读不到」在现场通常只有一句「超时」，但原因至少有三层：
// 地址/端口不通、Agent 只实现了 /current 没有 /probe、数据项 ID 写错（把
// name 当 id、把别的协议的路径抄过来）。这条命令把各层拆开：
//
//   - `-probe` 读 /probe：拿到设备模型（有哪些数据项、uuid、序列号）；
//   - `-current` 读 /current：整台设备当前所有数据项的快照；
//   - `-id` 指定数据项：从 /current 里挑出这几个点，验证 ID 对不对；
//   - `-sample -from -count` 按序号翻历史窗口，验证缓冲与 /sample 参数。
//
// 只读协议，没有写操作：MTConnect 标准没有写数据项的服务。
func runMTConnectRead(args []string) {
	fs := flag.NewFlagSet("mtconnectread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5000", "Agent 地址 host:port（省略端口按 5000；可带 http:// 或子路径）")
	probe := fs.Bool("probe", false, "读 /probe，列出设备模型与数据项 ID")
	current := fs.Bool("current", false, "读 /current，显示整台设备当前所有数据项")
	ids := fs.String("id", "", "数据项 ID，逗号分隔（从 /current 里取这几个点）")
	sample := fs.Bool("sample", false, "读 /sample 序号窗口")
	from := fs.Uint64("from", 0, "窗口起始序号（配合 -sample；0 表示从最早的可用序号）")
	count := fs.Int("count", 0, "窗口最多返回几条（配合 -sample；0 表示不限）")
	deviceUUID := fs.String("device-uuid", "", "多设备 Agent 上按 uuid 选择 DeviceStream")
	deviceName := fs.String("device-name", "", "多设备 Agent 上按 name 选择 DeviceStream")
	timeout := fs.Duration("timeout", 3*time.Second, "单次 HTTP 请求超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	fs.Parse(args)

	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "-timeout 必须大于 0")
		os.Exit(2)
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, "-count 不能为负")
		os.Exit(2)
	}
	if *from < 0 {
		fmt.Fprintln(os.Stderr, "-from 不能为负")
		os.Exit(2)
	}

	labels := map[string]string{}
	if v := strings.TrimSpace(*deviceUUID); v != "" {
		labels["deviceUuid"] = v
	}
	if v := strings.TrimSpace(*deviceName); v != "" {
		labels["deviceName"] = v
	}

	ch := model.Channel{
		ID: "ch-mtconnect", Protocol: "mtconnect",
		Transport: model.Transport{Kind: "tcp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels:   labels,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*4)
	defer cancel()

	c, err := mtconndrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()
	mc, ok := c.(mtReader)
	if !ok {
		fmt.Fprintln(os.Stderr, "驱动不支持自检读取")
		os.Exit(1)
	}

	// 默认动作：先探链路，再给一份当前快照概览。显式指定了某个模式就只走那个。
	switch {
	case *probe:
		runMTConnectProbe(ctx, mc, *asJSON)
		return
	case *sample:
		runMTConnectSample(ctx, mc, *from, *count, *asJSON)
		return
	case *current:
		doc, version, err := mc.Current(ctx)
		runMTConnectCurrent(doc, version, err, nil, *asJSON)
		return
	case strings.TrimSpace(*ids) != "":
		want := splitCSV(*ids)
		doc, version, err := mc.Current(ctx)
		runMTConnectCurrent(doc, version, err, want, *asJSON)
		return
	default:
		if err := c.Probe(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "链路探测（/probe，失败时退回 /current）失败: %v\n", err)
			fmt.Fprintln(os.Stderr, "  排查顺序：地址/端口（Agent 默认 5000）→ 是否只实现了 /current → 地址是否指向了反向代理登录页")
			os.Exit(1)
		}
		doc, version, err := mc.Current(ctx)
		runMTConnectCurrent(doc, version, err, nil, *asJSON)
	}
}

// mtRow 是自检输出的一行。
type mtRow struct {
	ID        string `json:"id"`
	Type      string `json:"type,omitempty"`
	Category  string `json:"category,omitempty"`
	Component string `json:"component,omitempty"`
	Value     string `json:"value,omitempty"`
	Sequence  uint64 `json:"sequence,omitempty"`
	Quality   string `json:"quality,omitempty"`
	Error     string `json:"error,omitempty"`
}

func runMTConnectProbe(ctx context.Context, mc mtReader, asJSON bool) {
	doc, version, err := mc.ProbeDevices(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 /probe 失败: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		devs := make([]map[string]any, 0, len(doc.Devices))
		for _, d := range doc.Devices {
			items := make([]map[string]any, 0, len(d.DataItems))
			for _, di := range d.DataItems {
				items = append(items, map[string]any{
					"id": di.ID, "type": di.Type, "category": string(di.Category),
					"component": di.Component, "units": di.Units,
				})
			}
			devs = append(devs, map[string]any{
				"uuid": d.UUID, "name": d.Name, "manufacturer": d.Manufacturer,
				"model": d.Model, "serialNumber": d.SerialNumber, "dataItems": items,
			})
		}
		emitJSON(map[string]any{"version": version, "base": mc.BaseURL(), "devices": devs})
		return
	}
	fmt.Printf("Agent 基地址: %s\n", mc.BaseURL())
	fmt.Printf("MTConnect 版本: %s\n", version)
	for _, d := range doc.Devices {
		fmt.Printf("\n设备 %s (uuid=%s)\n", d.Name, d.UUID)
		if d.Manufacturer != "" || d.Model != "" {
			fmt.Printf("  厂商 %s  型号 %s  序列号 %s\n", d.Manufacturer, d.Model, d.SerialNumber)
		}
		fmt.Printf("  数据项 %d 个：\n", len(d.DataItems))
		printRow([]int{18, 10, 12, 22}, "ID", "类别", "类型", "组件")
		for _, di := range d.DataItems {
			printRow([]int{18, 10, 12, 22}, di.ID, string(di.Category), di.Type, di.Component)
		}
	}
}

func runMTConnectCurrent(doc *mtcodec.Streams, version string, err error, only []string, asJSON bool) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 /current 失败: %v\n", err)
		os.Exit(1)
	}
	idx := doc.Index()
	if asJSON {
		rows := mtRowsFromIndex(idx, only)
		emitJSON(map[string]any{"version": version, "lastSequence": doc.Header.LastSequence, "read": len(rows), "rows": rows})
		return
	}
	fmt.Printf("MTConnect 版本: %s   序号 last=%d next=%d   数据项 %d 个\n",
		version, doc.Header.LastSequence, doc.Header.NextSequence, idx.Len())
	rows := mtRowsFromIndex(idx, only)
	printMTConnectRows(rows)
}

func runMTConnectSample(ctx context.Context, mc mtReader, from uint64, count int, asJSON bool) {
	doc, version, err := mc.Sample(ctx, from, count)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 /sample 失败: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		emitJSON(map[string]any{
			"version": version, "firstSequence": doc.Header.FirstSequence,
			"lastSequence": doc.Header.LastSequence,
			"items":        len(doc.Items()), "streams": len(doc.Devices),
		})
		return
	}
	fmt.Printf("MTConnect 版本: %s   窗口 first=%d last=%d\n",
		version, doc.Header.FirstSequence, doc.Header.LastSequence)
	for _, d := range doc.Devices {
		for _, comp := range d.Components {
			for _, it := range comp.Items {
				printRow([]int{18, 44, 12}, it.DataItemID, it.Value(), strconv.FormatUint(it.Sequence, 10))
			}
		}
	}
}

// mtRowsFromIndex 把索引里的观测折成输出行。only 非空时只保留这些 ID。
func mtRowsFromIndex(idx *mtcodec.Index, only []string) []mtRow {
	var filter map[string]bool
	if len(only) > 0 {
		filter = map[string]bool{}
		for _, s := range only {
			filter[s] = true
		}
	}
	ids := idx.IDs()
	rows := make([]mtRow, 0, len(ids))
	for _, id := range ids {
		if filter != nil && !filter[id] {
			continue
		}
		it, _ := idx.Lookup(id)
		row := mtRow{
			ID: id, Type: it.Type, Category: string(it.Category),
			Value: it.Value(), Sequence: it.Sequence,
		}
		if it.Unavailable() {
			row.Quality = string(model.QualityBad)
		}
		rows = append(rows, row)
	}
	return rows
}

func printMTConnectRows(rows []mtRow) {
	printRow([]int{18, 10, 12, 24, 12}, "ID", "类别", "类型", "值", "序号")
	for _, r := range rows {
		printRow([]int{18, 10, 12, 24, 12}, r.ID, r.Category, r.Type, r.Value, strconv.FormatUint(r.Sequence, 10))
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
