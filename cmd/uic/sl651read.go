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

	sl651codec "uic/internal/codec/sl651"
	"uic/internal/driver"
	sl651drv "uic/internal/driver/sl651"
	"uic/internal/model"
)

// uic sl651read 是《水文监测数据通信规约》SL 651—2014 中心站侧的自检命令。
//
// 与其它协议的自检命令同理由，但 SL 651 的排查顺序更需要分阶段——因为它
// 是**查询-应答 + 整帧取全部**的结构，且地址与内容都不像寄存器那样能猜：
//
//	物理层  波特率/校验位（标准 1200/2400 8N1，个别终端 8E1）/接线
//	链路层  7E 7E 起头 + 长度字段 + CRC-16/MODBUS 变体（不取反）
//	站号    5 字节 BCD，装 10 位十进制；现场普遍是 8 位测站编码
//	报文层  功能码 + 要素标识符（引导符 1 + 数据定义字节 1）
//
// 因此命令的顺序是：
//
//  1. `-probe`：只发一次 37H（查询全部）等应答，验证前三层。不发点位、
//     不看要素，所以它单独验证「通不通」；
//  2. 不带参数：发一次 37H，把**整帧**要素逐个列出（名称/单位/原始字节/
//     工程值/点表地址）。这一步回答"这台站上送了哪些要素"，并直接给出
//     可粘贴进点表的地址；
//  3. `-points`：按给定的要素地址走驱动的 Read 路径，逐点读。它刻意与
//     第 2 步分开：自检通过而采集失败，通常是地址在两条路径上被解析成了
//     不同东西；
//  4. `-list`：打印附录 C 的要素表，不需要连站。
//
// 边界与驱动一致：只做中心站侧查询（37H/38H/3AH），不做配置表读写
// （40~43H）、控制命令（4C~4FH）、人工置数（35H）、图片报（36H）、
// M3 多包重组、ASCII 字符编码帧。
func sl651readCmd(args []string) {
	fs := flag.NewFlagSet("sl651read", flag.ExitOnError)
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "从站地址 host[:port]（tcp），端口省略时按 4001")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 2400, "波特率（serial）；SL 651 常用 1200 或 2400")
	dataBits := fs.Int("dataBits", 8, "数据位（serial）；规约固定 8")
	parity := fs.String("parity", "none", "校验：none | even | odd（serial）；标准 8N1")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")

	station := fs.String("station", "1", "遥测站号（5 字节 BCD，十进制，最多 10 位）")
	center := fs.Int("center", 1, "中心站地址 0..255（下行帧里的 1 字节）")
	password := fs.String("password", "1234", "通信密码（2 字节十六进制，如 1234）")
	points := fs.String("points", "", "要素地址列表，逗号分隔：Z,PT,VT 或 39,26,38")
	probe := fs.Bool("probe", false, "只发一次 37H 查询全部并验证有应答，不列要素")
	list := fs.Bool("list", false, "打印附录 C 要素表（不需要连站）")
	raw := fs.Bool("raw", false, "额外显示原始字节、数据定义字节与引导符")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	retries := fs.Int("retries", 2, "单请求重发次数（不含首次）")
	gap := fs.Duration("gap", 30*time.Millisecond, "帧间静默")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	if *list {
		printSL651Guides(*raw, *asJSON)
		return
	}

	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}
	if *retries < 0 || *retries > 10 {
		fmt.Fprintln(os.Stderr, "-retries 必须在 0..10 之间")
		os.Exit(2)
	}
	if *center < 0 || *center > 255 {
		fmt.Fprintln(os.Stderr, "-center 必须在 0..255 之间")
		os.Exit(2)
	}
	if _, err := parseSL651Station(*station); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if _, err := parseSL651Password(*password); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	// -points 先校验一遍地址：现场最常见的失败是把要素简称写成了中文名或
	// 别的协议的寄存器语法，早点报出来比等超时好。
	if *points != "" {
		for _, p := range sl651Refs(*points) {
			name, slot, ok := sl651SplitSlot(p)
			if !ok {
				fmt.Fprintf(os.Stderr, "要素地址 %q 的段号写法不对（应为 `<要素>[<段号>]`）\n", p)
				os.Exit(2)
			}
			g, known := sl651codec.LookupGuide(name)
			if !known {
				fmt.Fprintf(os.Stderr, "要素地址 %q 不是附录 C 表 C.1 里的引导符"+
					"（用 -list 查看可用简称）\n", p)
				os.Exit(2)
			}
			if slot > 0 {
				if info, _ := sl651codec.GuideInfo(g); info.Slot == 0 {
					fmt.Fprintf(os.Stderr, "要素 %q 不是批量要素，不接受段号（只有 DRP/DRZ 有多段）\n", name)
					os.Exit(2)
				}
			}
		}
	}

	ch := model.Channel{
		ID: "ch-sl651", Protocol: "sl651",
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
			"address": strings.TrimSpace(*station),
			"options": fmt.Sprintf("retries=%d,gap=%s,center=%d,password=%s",
				*retries, gap.String(), *center, strings.ToUpper(*password)),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*8)
	defer cancel()

	c, err := sl651drv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（37H 查询全部）失败: %v\n", err)
		printSL651Hints(err)
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "station": strings.TrimSpace(*station),
				"center": *center, "link": "up", "probe": "37H",
			})
			return
		}
		fmt.Printf("链路正常: 站号 %s 回应了 37H 查询（传输 %s，中心站地址 %d）\n",
			strings.TrimSpace(*station), ch.Transport.Kind, *center)
		return
	}

	if *points != "" {
		readSL651Points(ctx, c, *points, *raw, *asJSON)
		return
	}

	// 默认路径：发一次 37H，把整帧要素列全。
	caller, ok := c.(interface {
		Report(context.Context) (*sl651codec.Report, error)
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供整帧读取")
		os.Exit(1)
	}
	rep, err := caller.Report(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		printSL651Hints(err)
		os.Exit(1)
	}
	printSL651Report(rep, strings.TrimSpace(*station), ch.Transport.Kind, *raw, *asJSON)
}

// parseSL651Station 校验站号。
//
// 5 字节 BCD 装 10 位十进制，因此上限是 9999999999。刻意不按 99999 卡：
// 现场用的测站编码普遍是 8 位（如 31234567），按 5 位卡会把合法站号判错。
func parseSL651Station(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("站号为空（5 字节 BCD，十进制，最多 10 位）")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("站号 %q 不是十进制整数（5 字节 BCD，最多 10 位）", s)
	}
	if n < 0 || n > 9_999_999_999 {
		return 0, fmt.Errorf("站号 %d 超出 0..9999999999（5 字节 BCD，10 位十进制）", n)
	}
	return n, nil
}

// parseSL651Password 解析 2 字节十六进制密码。
func parseSL651Password(s string) ([2]byte, error) {
	var out [2]byte
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return out, fmt.Errorf("密码 %q 必须是 4 个十六进制字符（2 字节），例如 1234", s)
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return out, fmt.Errorf("密码 %q 不是十六进制（应为 4 个十六进制字符）", s)
	}
	out[0], out[1] = byte(v>>8), byte(v&0xFF)
	return out, nil
}

// printSL651Hints 按错误分类给出排查顺序。
//
// 这是自检命令最有价值的一段：SL 651 的失败在串口上都表现成「没有应答」，
// 而原因可能是波特率、站号、密码、CRC 中的任何一个。分类型给顺序，才不会
// 在现场反复试。
func printSL651Hints(err error) {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		fmt.Fprintln(os.Stderr, "  超时排查顺序：")
		fmt.Fprintln(os.Stderr, "    1) 站号是否与终端一致（5 字节 BCD，注意现场常用 8 位测站编码）")
		fmt.Fprintln(os.Stderr, "    2) 密码是否一致（密码不符时终端是**静默**，而不是回错误码）")
		fmt.Fprintln(os.Stderr, "    3) 波特率/校验位（标准 1200 或 2400 8N1；个别终端出厂 8E1）")
		fmt.Fprintln(os.Stderr, "    4) 中心站地址（-center）是否与终端里配的一致")
		fmt.Fprintln(os.Stderr, "    5) A/B 接线与终端供电；RS-485 还需终端电阻")
	case driver.ErrProtocol:
		fmt.Fprintln(os.Stderr, "  协议层排查：")
		fmt.Fprintln(os.Stderr, "    · 「坏帧」通常意味着波特率/校验位不对，先查物理层，不要先怀疑站号")
		fmt.Fprintln(os.Stderr, "    · 密码或站号错配时终端多半完全静默，不会回错误码")
		fmt.Fprintln(os.Stderr, "    · 自定义扩展要素（FFXXH）驱动显式拒绝：XX 的含义只有设备方知道")
	case driver.ErrUnsupported:
		fmt.Fprintln(os.Stderr, "  不支持：")
		fmt.Fprintln(os.Stderr, "    · ASCII 字符编码帧、M3 多包（包总数 > 1）本驱动不做")
		fmt.Fprintln(os.Stderr, "    · 写方向（配置表/控制命令/人工置数）不在驱动范围内")
	case driver.ErrAddress:
		fmt.Fprintln(os.Stderr, "  地址层排查：用不带参数运行本命令，看这台站实际送了哪些要素")
	}
}

// sl651Row 是整帧输出的一行。
//
// Count 是**输出的值个数**，不是报文里的数据个数：批量要素（DRP/DRZ）在
// 报文里是 1 个 12/24 字节数据，输出时按规约段宽展开成 12 段。若两者混用，
// 「组数 1、值 12 个」的表格会让人以为解析错了。
type sl651Row struct {
	Ref     string    `json:"ref"`
	Alias   string    `json:"alias,omitempty"`
	Name    string    `json:"name"`
	Unit    string    `json:"unit,omitempty"`
	Guide   string    `json:"guide"`
	DataDef string    `json:"dataDef"`
	Count   int       `json:"count"`
	Values  []string  `json:"values,omitempty"`
	Numbers []float64 `json:"numbers,omitempty"`
	Missing []bool    `json:"missing,omitempty"`
	Raw     []string  `json:"raw,omitempty"`
	Voltage bool      `json:"voltage,omitempty"`
}

// sl651RowOf 把一个要素变成可打印的一行。
//
// Ref 直接写成能粘进点表的形态（就是 ASCII 简称；没有简称时退化成两位
// 十六进制）。这是自检命令最实用的输出：现场不需要再翻附录 C。
func sl651RowOf(f sl651codec.Field, isVolt bool) sl651Row {
	g, _ := sl651codec.GuideInfo(f.Guide)
	row := sl651Row{
		Name:    g.Name,
		Unit:    g.Unit,
		Guide:   fmt.Sprintf("%02X", f.Guide),
		DataDef: fmt.Sprintf("%02X", f.DataDef),
		Count:   f.Count(),
		Voltage: isVolt,
	}
	row.Alias = g.Alias
	row.Ref = g.Alias
	if row.Ref == "" {
		row.Ref = fmt.Sprintf("%02X", f.Guide)
	}
	for i := 0; i < f.Count(); i++ {
		// 批量要素（DRP/DRZ）在报文里是「1 个 12/24 字节数据」，但语义上
		// 是 12 段 5min 值。必须按规约的 Slot 宽度切开再逐段输出，否则
		// 12 字节会被当成一个 HEX 整数解出 1.8e18 这种垃圾值——不报错，
		// 但完全不真实。切分与换算都走 codec 的 Slot*，CLI 只负责排版。
		for j := 0; j < f.SlotCount(i); j++ {
			row.Raw = append(row.Raw, hexBytes(f.SlotBytes(i, j)))
			if f.SlotNoData(i, j) {
				row.Missing = append(row.Missing, true)
				row.Values = append(row.Values, "缺测")
				row.Numbers = append(row.Numbers, 0)
				continue
			}
			row.Missing = append(row.Missing, false)
			if g.Hex && g.Scale == 0 {
				// ZT 这类状态字没有数值含义，按原始 HEX 输出。
				row.Numbers = append(row.Numbers, 0)
				row.Values = append(row.Values, fmt.Sprintf("0x%0*X",
					len(f.SlotBytes(i, j))*2, f.SlotHex(i, j)))
				continue
			}
			v, err := f.SlotNumber(i, j)
			if err != nil {
				row.Numbers = append(row.Numbers, 0)
				row.Values = append(row.Values, "错误: "+err.Error())
				continue
			}
			row.Numbers = append(row.Numbers, v)
			row.Values = append(row.Values, trimNum(v))
		}
	}
	// Count 与 Values 保持一致：批量要素展开后是 12 段，不是报文里的 1 个数据。
	row.Count = len(row.Values)
	return row
}

// printSL651Report 打印整帧报文的头部与全部要素。
func printSL651Report(rep *sl651codec.Report, station, transport string, raw, asJSON bool) {
	rows := make([]sl651Row, 0, len(rep.Fields)+1)
	for _, f := range rep.Fields {
		rows = append(rows, sl651RowOf(f, false))
	}
	if rep.Voltage != nil {
		rows = append(rows, sl651RowOf(*rep.Voltage, true))
	}

	head := map[string]any{
		"station":    station,
		"serial":     int(rep.Serial[0])<<8 | int(rep.Serial[1]),
		"reportTime": rep.ReportTime.Format("2006-01-02 15:04:05"),
		"obsTime":    rep.ObsTime.Format("2006-01-02 15:04"),
		"classCode":  fmt.Sprintf("%02X", rep.ClassCode),
		"className":  sl651codec.ClassName(rep.ClassCode),
		"func":       fmt.Sprintf("%02X", rep.Frame.Func),
		"funcName":   sl651codec.FuncName(rep.Frame.Func),
		"transport":  transport,
		"stationRaw": hexBytes(rep.Station[:]),
		"restBytes":  hexBytes(rep.Rest),
	}
	if rep.TimeStep != nil {
		head["timeStep"] = rep.TimeStep.String()
	}
	if asJSON {
		emitJSON(map[string]any{
			"header": head, "rows": rows, "count": len(rows),
		})
		return
	}

	fmt.Printf("站号 %s  传输 %s  分类码 %02X（%s）\n",
		station, transport, rep.ClassCode, sl651codec.ClassName(rep.ClassCode))
	fmt.Printf("功能码 %02X（%s）  流水号 %d\n",
		rep.Frame.Func, sl651codec.FuncName(rep.Frame.Func),
		int(rep.Serial[0])<<8|int(rep.Serial[1]))
	if !rep.ReportTime.IsZero() {
		fmt.Printf("发报时间 %s  观测时间 %s\n",
			rep.ReportTime.Format("2006-01-02 15:04:05"),
			rep.ObsTime.Format("2006-01-02 15:04"))
	}
	if rep.TimeStep != nil {
		fmt.Printf("时间步长 %s\n", rep.TimeStep.String())
	}
	if len(rep.Rest) > 0 {
		// 尾部多余字节在现场常见（厂商追加私有字段）。明确说出来，而不是
		// 悄悄忽略——否则「为什么少了一个点位」会被当成解析 bug。
		fmt.Printf("注意: 报文尾部有 %d 字节未解释: %s\n", len(rep.Rest), hexBytes(rep.Rest))
	}
	fmt.Println()

	if raw {
		printRow([]int{16, 22, 8, 8, 10, 24},
			"点表地址", "含义", "引导符", "定义字节", "段数", "值")
		for _, r := range rows {
			v := strings.Join(r.Values, ", ")
			if r.Unit != "" {
				v += " " + r.Unit
			}
			// 段数 = 实际输出的值个数，而不是报文里的「数据个数」：DRP 在
			// 报文里是 1 个 12 字节数据，但会输出 12 段，标 1 会与后面列出的
			// 12 个值自相矛盾。
			printRow([]int{16, 22, 8, 8, 10, 24},
				r.Ref, r.Name, r.Guide, r.DataDef, strconv.Itoa(len(r.Values)), v)
		}
		return
	}
	printRow([]int{16, 24, 30}, "点表地址", "含义", "值")
	for _, r := range rows {
		v := strings.Join(r.Values, ", ")
		if r.Unit != "" {
			v += " " + r.Unit
		}
		if r.Voltage {
			v += "（电压）"
		}
		printRow([]int{16, 24, 30}, r.Ref, r.Name, v)
	}
	fmt.Println()
	fmt.Println("提示：把「点表地址」那一列直接抄进点表即可（要素 ASCII 简称或两位十六进制）。")
	fmt.Println("      批量要素（DRP/DRZ）的多段值用 `<要素>[<段号>]` 单独寻址，段号从 0 开始。")
}

// sl651PointRow 是逐点读取的一行。
type sl651PointRow struct {
	Ref     string  `json:"ref"`
	Name    string  `json:"name,omitempty"`
	Unit    string  `json:"unit,omitempty"`
	Guide   string  `json:"guide,omitempty"`
	Value   string  `json:"value,omitempty"`
	Numeric float64 `json:"numeric,omitempty"`
	Quality string  `json:"quality,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// readSL651Points 按要素地址列表走驱动的 Read 逐点读取。
//
// 走驱动的 Read（而不是从整帧里自己挑），目的与其它自检命令一致：让这条
// 命令同时验证「点表地址在采集路径上也成立」。自检通过而采集失败是最难
// 排查的一类问题，通常就是地址在两条路径上被解析成了不同东西。
func readSL651Points(ctx context.Context, c driver.Conn, spec string, raw, asJSON bool) {
	parts := sl651Refs(spec)
	if len(parts) == 0 {
		fmt.Fprintln(os.Stderr, "-points 至少要给一个要素地址")
		os.Exit(2)
	}
	refs := make([]driver.PointRef, 0, len(parts))
	for _, p := range parts {
		refs = append(refs, driver.PointRef{
			ID:       p,
			Address:  model.Address{Kind: model.AddressSymbol, Path: p},
			DataType: model.TypeFloat64,
		})
	}
	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "selfcheck", Points: refs}})
	if err != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		printSL651Hints(err)
		os.Exit(1)
	}
	rows := make([]sl651PointRow, 0, len(samples))
	for _, s := range samples {
		row := sl651PointRow{Ref: s.PointID, Quality: string(s.Quality)}
		if g, ok := sl651codec.LookupGuide(s.PointID); ok {
			info, _ := sl651codec.GuideInfo(g)
			row.Name, row.Unit = info.Name, info.Unit
			row.Guide = fmt.Sprintf("%02X", g)
		}
		if s.Err != nil {
			row.Error = s.Err.Error()
			rows = append(rows, row)
			continue
		}
		row.Value = s.Value.String()
		if v, ok := s.Value.AsFloat(); ok {
			row.Numeric = v
		}
		rows = append(rows, row)
	}
	if asJSON {
		emitJSON(map[string]any{"rows": rows, "count": len(rows)})
		return
	}
	if raw {
		printRow([]int{16, 22, 8, 22, 12},
			"点表地址", "含义", "引导符", "值", "质量")
		for _, r := range rows {
			v := r.Value
			if r.Unit != "" {
				v += " " + r.Unit
			}
			if r.Error != "" {
				v = "错误: " + r.Error
			}
			printRow([]int{16, 22, 8, 22, 12}, r.Ref, r.Name, r.Guide, v, r.Quality)
		}
		return
	}
	printRow([]int{16, 24, 22, 12}, "点表地址", "含义", "值", "质量")
	for _, r := range rows {
		v := r.Value
		if r.Unit != "" {
			v += " " + r.Unit
		}
		if r.Error != "" {
			v = "错误: " + r.Error
		}
		printRow([]int{16, 24, 22, 12}, r.Ref, r.Name, v, r.Quality)
	}
}

// printSL651Guides 打印附录 C 的要素表。
//
// 为什么要连这张表一起打印：SL 651 的点位地址是**规约定义**的引导符，
// 现场最常见的问题是「简称记错」（Z 是水位、PT 是降水量累计值、VT 是
// 电压，而 P1 是 1h 时段降水量）。列出来比口头解释有效。
func printSL651Guides(raw, asJSON bool) {
	gs := sl651codec.Guides()
	type row struct {
		Guide  string `json:"guide"`
		Alias  string `json:"alias,omitempty"`
		Name   string `json:"name"`
		Unit   string `json:"unit,omitempty"`
		Digits int    `json:"digits,omitempty"`
		Decs   int    `json:"decimals,omitempty"`
		Width  int    `json:"width"`
		Def    string `json:"dataDef"`
		Hex    bool   `json:"hex,omitempty"`
		Fixed  bool   `json:"fixedWidth,omitempty"`
		// Slot 是批量要素的段宽（DRP 1 字节、DRZ 2 字节），0 表示不是批量要素。
		Slot int `json:"slot,omitempty"`
		// Slots 是批量要素的段数（宽度 ÷ 段宽），非批量要素不输出。
		Slots int    `json:"slots,omitempty"`
		Point string `json:"pointAddress"`
	}
	rows := make([]row, 0, len(gs))
	for _, g := range gs {
		info, ok := sl651codec.GuideInfo(g)
		if !ok {
			continue
		}
		point := info.Alias
		if point == "" {
			point = fmt.Sprintf("%02X", g)
		}
		slots := 0
		if info.Slot > 0 && info.Bytes() > 0 {
			slots = info.Bytes() / info.Slot
			// 批量要素的点表地址写成 `DRP[0..11]`：不给段号只能取第 0 段，
			// 段号是这类要素实际落点的必要条件，所以必须在地址列里显式提示。
			point = fmt.Sprintf("%s[0..%d]", point, slots-1)
		}
		rows = append(rows, row{
			Guide: fmt.Sprintf("%02X", g), Alias: info.Alias, Name: info.Name,
			Unit: info.Unit, Digits: info.Digits, Decs: info.Decimals,
			Width: info.Bytes(), Def: fmt.Sprintf("%02X", info.DataDef()),
			Hex: info.Hex, Fixed: info.FixedWidth > 0,
			Slot: info.Slot, Slots: slots, Point: point,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Guide < rows[j].Guide })
	if asJSON {
		emitJSON(map[string]any{"guides": rows, "count": len(rows)})
		return
	}
	fmt.Printf("附录 C 表 C.1 要素表（%d 项；引导符 + 数据定义字节）\n\n", len(rows))
	if raw {
		printRow([]int{10, 14, 34, 10, 10, 10, 20},
			"引导符", "简称", "含义", "单位", "字节数", "小数位", "点表地址")
		for _, r := range rows {
			printRow([]int{10, 14, 34, 10, 10, 10, 20},
				r.Guide, r.Alias, r.Name, r.Unit,
				strconv.Itoa(r.Width), strconv.Itoa(r.Decs), r.Point)
		}
		return
	}
	printRow([]int{10, 14, 40, 10, 20}, "引导符", "简称", "含义", "单位", "点表地址")
	for _, r := range rows {
		printRow([]int{10, 14, 40, 10, 20}, r.Guide, r.Alias, r.Name, r.Unit, r.Point)
	}
	fmt.Println()
	fmt.Println("用法：uic sl651read -addr 127.0.0.1:4001 -points Z,PT,VT")
	fmt.Println("      批量要素（DRP/DRZ）按段取：-points DRP,DRP[3],DRZ1[11]（段号从 0 开始）")
}

// sl651Refs 解析逗号分隔的要素地址列表（给单测复用）。
func sl651Refs(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sl651SplitSlot 把 `<要素>[<段号>]` 拆成要素与段号（段号缺省为 0）。
//
// 与驱动里的 guideOfWithSlot 保持同一套语法：自检命令先拆一遍只是为了
// 尽早报出「地址写错」，真正的解析仍在驱动里做——不然两条路径各写一套
// 规则，正是「自检通过、采集失败」的经典成因。
func sl651SplitSlot(s string) (string, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '[')
	if i < 0 {
		return s, 0, true
	}
	if !strings.HasSuffix(s, "]") {
		return s, 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s[i+1 : len(s)-1]))
	if err != nil || n < 0 {
		return s, 0, false
	}
	return strings.TrimSpace(s[:i]), n, true
}
