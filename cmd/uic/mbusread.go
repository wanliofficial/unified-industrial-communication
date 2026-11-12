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

	mb "uic/internal/codec/mbus"
	"uic/internal/driver"
	mbdrv "uic/internal/driver/mbus"
	"uic/internal/model"
)

// uic mbusread 是 M-Bus（EN 13757）主站的自检命令。
//
// 与仓库里其它表计自检命令同理由：M-Bus 的"读不到"有太多同样表现的原因，
// 而 M-Bus 比 DL/T 645 / CJ/T 188 更难自查，因为**它没有固定的数据地址**：
// 报文的记录顺序、宽度、量纲全由表自己决定。因此这里的顺序是刻意设计的：
//
//  1. `-scan`：只发 SND_NKE，看哪些主地址回了 0xE5。应答是单字节，不依赖
//     任何数据结构，所以它单独验证了波特率/校验位/接线/站号四件事；
//  2. `-probe`：对单个地址做同一件事，用于确认"就这一台表在不在"；
//  3. 不带参数：读整表并逐条列出记录（`q:` 地址、单位、原始 DIF/VIF）。
//     这一步才是"这台表到底有什么"的答案，也直接给出可以抄进点表的地址。
//
// 边界与驱动一致：单主站、主地址寻址、只读。不做次级地址选择（需要先
// SND_UD 且地址域变 8 字节）、不做 SND_UD 写入、不做多表并发轮询。
func mbusreadCmd(args []string) {
	fs := flag.NewFlagSet("mbusread", flag.ExitOnError)
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "从站地址 host[:port]（tcp），端口省略时按 4001")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 2400, "波特率（serial）；标准典型值 300/2400/9600")
	dataBits := fs.Int("dataBits", 8, "数据位（serial）；EN 13757-2 固定 8")
	parity := fs.String("parity", "even", "校验：none | even | odd（serial）；标准要求 even")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")

	primary := fs.String("address", "1", "从站主地址 0..250（十进制或 0x 十六进制）")
	records := fs.String("records", "", "记录地址列表，逗号分隔：q:energy,q:volume@1,r:0；留空表示读整表")
	probe := fs.Bool("probe", false, "只发 SND_NKE 验证链路（应答是单字节 0xE5）")
	scan := fs.String("scan", "", "扫描主地址范围，如 1-20 或 0-250；只发 SND_NKE")
	raw := fs.Bool("raw", false, "额外显示 DIF/VIF/DIFE 原始字节与原始整数")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	retries := fs.Int("retries", 2, "单请求重发次数（不含首次）")
	gap := fs.Duration("gap", 30*time.Millisecond, "帧间静默")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}
	if *retries < 0 || *retries > 10 {
		fmt.Fprintln(os.Stderr, "-retries 必须在 0..10 之间")
		os.Exit(2)
	}
	if *scan != "" {
		runMBusScan(*scan, *tr, *addr, *port, *baud, *dataBits, *parity, *stopBits,
			*gap, *timeout, *retries, *asJSON)
		return
	}

	a, err := parsePrimaryAddr(*primary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	ch := model.Channel{
		ID: "ch-mbus", Protocol: "mbus",
		Transport: model.Transport{
			Kind: strings.ToLower(*tr), Address: *addr, Port: *port,
			Baud: *baud, DataBits: *dataBits, Parity: *parity, StopBits: *stopBits,
		},
		Concurrency: 1,
		Timeout:     model.Duration(*timeout),
	}
	if ch.Transport.Kind == "serial" {
		ch.Transport.Address = ""
	}
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels: map[string]string{
			"address": strconv.Itoa(int(a)),
			"options": fmt.Sprintf("retries=%d,gap=%s", *retries, gap.String()),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*8)
	defer cancel()

	c, err := mbdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（SND_NKE）失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：波特率/校验位（2400 8E1）→ 主地址 → 转换器供电与 A/B 接线 "+
			"→ 终端电阻 → 用 -scan 确认总线上有哪些站")
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "address": a, "link": "up", "probe": "snd-nke",
			})
			return
		}
		fmt.Printf("链路正常: 主地址 %d 回了 E5（SND_NKE 通过，传输 %s）\n", a, ch.Transport.Kind)
		return
	}

	caller, ok := c.(interface {
		Table(context.Context) (mb.Response, error)
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供整表读取")
		os.Exit(1)
	}
	resp, err := caller.Table(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}

	if *records != "" {
		printMBusRecords(ctx, c, resp, *records, *raw, *asJSON)
		return
	}
	printMBusTable(resp, a, ch.Transport.Kind, *raw, *asJSON)
}

// parsePrimaryAddr 解析主地址，顺带把"为什么不能是这些值"讲清楚。
func parsePrimaryAddr(s string) (byte, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.ParseUint(s, 0, 16)
	if err != nil {
		return 0, fmt.Errorf("主地址 %q 不是整数（0..%d）", s, mb.MaxPrimaryAddr)
	}
	if n > 0xFF {
		return 0, fmt.Errorf("主地址 %q 超出单字节范围（0..%d）", s, mb.MaxPrimaryAddr)
	}
	a := byte(n)
	if err := mb.ValidatePrimaryAddress(a); err != nil {
		return 0, err
	}
	return a, nil
}

// parseScanRange 解析 `1-20` / `0-250` / 单个 `5`。
func parseScanRange(s string) (byte, byte, error) {
	s = strings.TrimSpace(s)
	lo, hi := s, s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		lo, hi = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	a, err := parsePrimaryAddr(lo)
	if err != nil {
		return 0, 0, err
	}
	b, err := parsePrimaryAddr(hi)
	if err != nil {
		return 0, 0, err
	}
	if a > b {
		return 0, 0, fmt.Errorf("扫描范围 %q 的起点大于终点", s)
	}
	return a, b, nil
}

// runMBusScan 逐个主地址发 SND_NKE。
//
// 为什么扫描只看 SND_NKE：它的应答是单字节 0xE5，不涉及任何数据结构，
// 因此"有应答"这件事本身就说明波特率、校验位、接线、站号都对上了。
// 反过来，若某个地址能应答 SND_NKE 却读不出 REQ_UD2，问题一定在应用层
// （CI 不支持、记录结构不认识），而不是物理层——这个分界是扫描的价值。
func runMBusScan(rangeSpec, transport, addr, port string, baud, dataBits int,
	parity string, stopBits int, gap, timeout time.Duration, retries int, asJSON bool) {
	lo, hi, err := parseScanRange(rangeSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	type found struct {
		Addr byte `json:"address"`
	}
	var hits []found
	for a := int(lo); a <= int(hi); a++ {
		ch := model.Channel{
			ID: "ch-mbus", Protocol: "mbus",
			Transport: model.Transport{
				Kind: strings.ToLower(transport), Address: addr, Port: port,
				Baud: baud, DataBits: dataBits, Parity: parity, StopBits: stopBits,
			},
			Concurrency: 1,
			Timeout:     model.Duration(timeout),
		}
		if ch.Transport.Kind == "serial" {
			ch.Transport.Address = ""
		}
		dev := model.Device{
			ID: "scan", ChannelID: ch.ID, Enabled: true,
			Interval: model.Duration(time.Second),
			Timeout:  model.Duration(timeout),
			Labels: map[string]string{
				"address": strconv.Itoa(a),
				"options": fmt.Sprintf("retries=%d,gap=%s", retries, gap.String()),
			},
		}
		c, err := mbdrv.New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
		if err != nil {
			fmt.Fprintf(os.Stderr, "主地址 %d: 打开失败: %v\n", a, err)
			continue
		}
		// 扫描用短超时：扫 250 个地址时，单站 3 秒超时会把整轮拖到十几分钟。
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		perr := c.Probe(ctx)
		cancel()
		_ = c.Close()
		if perr == nil {
			hits = append(hits, found{Addr: byte(a)})
			if !asJSON {
				fmt.Printf("主地址 %3d  应答 E5\n", a)
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Addr < hits[j].Addr })
	if asJSON {
		emitJSON(map[string]any{"scan": rangeSpec, "found": hits, "count": len(hits)})
		return
	}
	if len(hits) == 0 {
		fmt.Printf("扫描 %s：没有从站应答\n", rangeSpec)
		fmt.Println("  排查：转换器供电 → A/B 接线 → 波特率/校验位（2400 8E1）→ 终端电阻")
		return
	}
	fmt.Printf("扫描 %s：发现 %d 台从站\n", rangeSpec, len(hits))
	fmt.Println("  下一步：uic mbusread -address <主地址> 读整表")
}

// mbusRow 是整表输出的一行。
type mbusRow struct {
	Ref      string  `json:"ref"`
	Quantity string  `json:"quantity"`
	Unit     string  `json:"unit,omitempty"`
	Kind     string  `json:"kind"`
	Value    string  `json:"value"`
	Numeric  float64 `json:"numeric,omitempty"`
	RawInt   int64   `json:"rawInt,omitempty"`
	Storage  uint32  `json:"storage,omitempty"`
	Tariff   uint16  `json:"tariff,omitempty"`
	Subunit  byte    `json:"subunit,omitempty"`
	DIF      string  `json:"dif"`
	VIF      string  `json:"vif"`
	Scale    float64 `json:"scale"`
	Raw      string  `json:"raw,omitempty"`
	Quality  string  `json:"quality,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// rowOf 把一条记录变成可打印的一行。
//
// Ref 直接写成可以粘进点表的形态（`q:energy`、`q:volume@1`、`q:power-max`），
// 这是自检命令最有用的输出：现场不需要再去翻手册猜量名。
func rowOf(rec mb.Record, base map[string]int) mbusRow {
	key := mb.NormalizeQuantity(rec.Quantity)
	ref := "q:" + key
	if rec.Storage > 0 {
		ref += "@" + strconv.FormatUint(uint64(rec.Storage), 10)
	}
	if n := base[key]; n > 0 {
		ref += "/" + strconv.Itoa(n)
	}
	base[key]++
	row := mbusRow{
		Ref: ref, Quantity: rec.Quantity, Unit: rec.Unit, Kind: rec.Kind.String(),
		RawInt: rec.RawInt, Storage: rec.Storage, Tariff: rec.Tariff, Subunit: rec.Subunit,
		DIF: fmt.Sprintf("%02X", rec.DIF), VIF: fmt.Sprintf("%02X", rec.VIF),
		Scale: rec.Scale, Raw: hexBytes(rec.Raw),
	}
	switch rec.Kind {
	case mb.KindNumeric:
		row.Numeric = rec.Num
		row.Value = trimNum(rec.Num)
		if rec.Unit != "" {
			row.Value += " " + rec.Unit
		}
	case mb.KindText:
		row.Value = rec.Text
		if rec.Text == "" {
			// 时间无效位被置位时 decodeDateTime 返回空串：明确说出来，
			// 否则表格里一个空格会被当成"表没这项数据"。
			row.Value = "(时间无效)"
		}
	case mb.KindBinary:
		row.Value = hexBytes(rec.Raw)
	}
	if len(rec.VIFEs) > 0 {
		row.VIF += " " + hexBytes(rec.VIFEs)
	}
	if len(rec.DIFEs) > 0 {
		row.DIF += " " + hexBytes(rec.DIFEs)
	}
	return row
}

func hexBytes(b []byte) string {
	var sb strings.Builder
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", c)
	}
	return sb.String()
}

// printMBusTable 打印固定头 + 全部记录。
func printMBusTable(resp mb.Response, addr byte, transport string, raw, asJSON bool) {
	base := map[string]int{}
	rows := make([]mbusRow, 0, len(resp.Records))
	for _, rec := range resp.Records {
		rows = append(rows, rowOf(rec, base))
	}
	h := resp.Header
	header := map[string]any{
		"id": h.IDText, "manufacturer": h.ManufacturerID,
		"version": h.Version, "medium": h.Medium,
		"mediumName": h.MediumName, "accessNo": h.AccessNo,
		"status": h.Status, "statusText": h.StatusText(),
		"signature": h.Signature,
	}
	if asJSON {
		emitJSON(map[string]any{
			"transport": transport, "address": addr, "link": "up",
			"dataKind": resp.Kind.String(), "header": header,
			"records": rows, "count": len(rows),
			"moreRecordsFollow": resp.MoreRecordsFollow,
			"error":             resp.ErrorText,
		})
		return
	}

	fmt.Printf("主地址 %d  传输 %s  数据结构 %s（%d 条记录）\n",
		addr, transport, resp.Kind, len(rows))
	fmt.Printf("表号 %s  厂商 %s  介质 %02X（%s）  版本 %d  访问号 %d  状态 0x%02X（%s）\n\n",
		h.IDText, h.ManufacturerID, h.Medium, h.MediumName, h.Version, h.AccessNo,
		h.Status, h.StatusText())
	if resp.ErrorText != "" {
		fmt.Printf("注意: %s\n\n", resp.ErrorText)
	}
	if resp.MoreRecordsFollow {
		fmt.Println("注意: 从站声明还有后续报文；本命令只打印已取回的部分")
		fmt.Println()
	}

	if raw {
		printRow([]int{20, 20, 8, 22, 12, 10, 10},
			"点表地址", "物理量", "类型", "值", "原始整数", "DIF/VIF", "倍率")
		for _, r := range rows {
			printRow([]int{20, 20, 8, 22, 12, 10, 10},
				r.Ref, r.Quantity, r.Kind, r.Value,
				strconv.FormatInt(r.RawInt, 10), r.DIF+"/"+r.VIF,
				strconv.FormatFloat(r.Scale, 'g', 6, 64))
		}
		return
	}
	printRow([]int{20, 20, 24}, "点表地址", "物理量", "值")
	for _, r := range rows {
		printRow([]int{20, 20, 24}, r.Ref, r.Quantity, r.Value)
	}
}

// printMBusRecords 按用户给的 `q:`/`r:` 列表逐条读取。
//
// 走驱动的 Read（而不是自己从整表里挑），目的就是让这条命令同时验证
// 点表地址在采集路径上也能用——自检通过而采集失败是最难排查的一类问题。
func printMBusRecords(ctx context.Context, c driver.Conn, resp mb.Response, spec string, raw, asJSON bool) {
	parts := strings.Split(spec, ",")
	refs := make([]driver.PointRef, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		refs = append(refs, driver.PointRef{
			ID:       p,
			Address:  model.Address{Kind: model.AddressSymbol, Path: p},
			DataType: model.TypeFloat64,
		})
	}
	if len(refs) == 0 {
		fmt.Fprintln(os.Stderr, "-records 至少要给一个记录地址")
		os.Exit(2)
	}
	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "selfcheck", Points: refs}})
	if err != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	rows := make([]mbusRow, 0, len(samples))
	for _, s := range samples {
		row := mbusRow{Ref: s.PointID, Quality: string(s.Quality)}
		if s.Err != nil {
			row.Error = s.Err.Error()
			rows = append(rows, row)
			continue
		}
		row.Kind = string(s.Value.Type)
		row.Value = s.Value.String()
		if v, ok := s.Value.AsFloat(); ok {
			row.Numeric = v
		}
		rows = append(rows, row)
	}
	if asJSON {
		emitJSON(map[string]any{"address": resp.Header.IDText, "rows": rows})
		return
	}
	printRow([]int{20, 12, 26, 10}, "记录地址", "类型", "值", "质量")
	for _, r := range rows {
		v := r.Value
		if r.Error != "" {
			v = "错误: " + r.Error
		}
		printRow([]int{20, 12, 26, 10}, r.Ref, r.Kind, v, r.Quality)
	}
}

// mbusRefs 是给单测用的记录地址列表解析（与 printMBusRecords 共用语义）。
func mbusRefs(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
