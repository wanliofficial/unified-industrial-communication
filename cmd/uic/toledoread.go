package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"uic/internal/driver"
	toledodrv "uic/internal/driver/toledo"
	"uic/internal/model"
)

// uic toledoread 是 METTLER TOLEDO 连续输出（ASCII 逗号三段式）的自检命令。
//
// 与其它协议的自检命令同理由，但 Toledo 的排查顺序不同——它是**推送**协议：
// 衡器自己持续吐帧，没有命令可发。因此自检的重点不是「发什么、怎么配」，而是
// 「连上后到底收到了什么帧、解成了什么」。最常见的现场问题也因此不同：
//
//	物理层  波特率/校验位（连续输出常见 9600/8N1，也有 8E1）/接线/供电
//	帧格式  仪表被设成了**二进制** TOLEDO（STX 开头 18 字节）而非 ASCII 三段式
//	字段    把状态/毛净/单位三段看错位，或小数位与仪表实际不符
//
// 命令顺序：
//
//  1. `-probe`：连上等一个连续输出帧，验证「通不通」。不发点位、不看字段；
//  2. 不带参数：连上后把**最新一帧**的各字段逐个列出（重量/状态/毛净/单位/
//     稳定性/小数位），直接回答「这台秤现在送的是什么」；
//  3. `-points weight,status`：按字段名走驱动的 Read 路径逐点读，验证字段名在
//     采集路径上也成立；
//  4. `-list`：打印可用读数字段，不需要连秤。
//
// 边界与驱动一致：只读推送，不模拟二进制 TOLEDO 格式、不做 SICS 命令应答。
func toledoreadCmd(args []string) {
	fs := flag.NewFlagSet("toledoread", flag.ExitOnError)
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "秤地址 host[:port]（tcp），端口省略时按 4001")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 9600, "波特率（serial）；连续输出常见 9600")
	dataBits := fs.Int("dataBits", 8, "数据位（serial）；规约固定 8")
	parity := fs.String("parity", "none", "校验：none | even | odd（serial）；常见 8N1 或 8E1")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")

	points := fs.String("points", "", "读数字段列表，逗号分隔：weight,status,unit,mode,stable,motion,decimals")
	probe := fs.Bool("probe", false, "只连上并验证收到连续输出帧，不列字段")
	list := fs.Bool("list", false, "打印可用读数字段（不需要连秤）")
	raw := fs.Bool("raw", false, "额外显示原始帧文本与收帧统计（字节数/坏帧/噪声）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	timeout := fs.Duration("timeout", 3*time.Second, "连上与探测超时")
	fs.Parse(args)

	if *list {
		printToledoFields(*asJSON)
		return
	}

	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}

	// -points 先校验：字段名拼错是自检阶段就该拦住的，不该等连上才暴露。
	if *points != "" {
		for _, p := range toledoRefs(*points) {
			if !toledodrv.KnownSymbol(p) {
				fmt.Fprintf(os.Stderr, "读数字段 %q 不认识（用 -list 查看可用字段）\n", p)
				os.Exit(2)
			}
		}
	}

	ch := model.Channel{
		ID: "ch-toledo", Protocol: "toledo",
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
	// 探测与默认路径需要一个「有点位」的设备（驱动不接受 0 点位），所以这里
	// 根据本次要读的字段合成一组点位：-points 用点名的，probe/默认用全字段。
	// 这些点位的唯一作用是让驱动的 Open 通过；真正读什么字段由后面的 Read 决定。
	var readFields []string
	if *points != "" {
		readFields = toledoRefs(*points)
	} else {
		readFields = []string{"weight", "status", "mode", "unit", "stable", "motion", "decimals"}
	}
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(*timeout),
		Timeout:  model.Duration(*timeout),
		Labels:   map[string]string{},
	}
	for _, f := range readFields {
		dev.Points = append(dev.Points, model.Point{
			ID: f, Address: model.Address{Kind: model.AddressSymbol, Path: f},
			DataType: toledoDefaultType(f), Enabled: true,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*4)
	defer cancel()

	c, err := toledodrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		printToledoHints(err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "探测（等待连续输出帧）失败: %v\n", err)
		printToledoHints(err)
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			emitJSON(map[string]any{"transport": ch.Transport.Kind, "link": "up", "probe": "frame"})
			return
		}
		fmt.Printf("链路正常: %s 侧收到了连续输出帧（传输 %s）\n", "客户端", ch.Transport.Kind)
		return
	}

	if *points != "" {
		readToledoPoints(ctx, c, *points, *raw, *asJSON)
		return
	}

	// 默认路径：连上后列出最新一帧的全部字段。
	printToledoLatest(ctx, c, ch.Transport.Kind, *raw, *asJSON)
}

// toledoRefs 解析逗号分隔的字段列表（给单测复用）。
func toledoRefs(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}

// printToledoFields 打印可用读数字段表。
//
// 为什么连这张表一起打印：Toledo 点位地址是**语义字段名**而不是寄存器，现场
// 最常见的问题是「把 status 当成了 weight」「不知道有 mode/stable」。列出来比
// 口头解释有效。
func printToledoFields(asJSON bool) {
	type row struct {
		Field   string `json:"field"`
		Meaning string `json:"meaning"`
		Type    string `json:"type"`
	}
	rows := []row{
		{"weight", "重量值（带符号，单位见 unit 字段）", "float64"},
		{"status", "称量状态 ST=稳定 US=运动中 OL=超量程 UL=欠量程", "string"},
		{"mode", "毛净模式 GS=毛重 NT=净重 TR=皮重", "string"},
		{"unit", "计量单位（kg/lb/g/t/oz/kN… 或 lb_oz 磅盎司）", "string"},
		{"stable", "是否稳定（status=ST）", "bool"},
		{"motion", "是否运动中（status=US）", "bool"},
		{"decimals", "数值段小数位数", "int64"},
	}
	if asJSON {
		emitJSON(map[string]any{"fields": rows, "count": len(rows)})
		return
	}
	fmt.Printf("TOLEDO 连续输出读数字段（%d 个）\n\n", len(rows))
	printRow([]int{12, 56, 10}, "字段", "含义", "类型")
	for _, r := range rows {
		printRow([]int{12, 56, 10}, r.Field, r.Meaning, r.Type)
	}
	fmt.Println()
	fmt.Println("用法：uic toledoread -addr 127.0.0.1:4001")
	fmt.Println("      uic toledoread -addr 127.0.0.1:4001 -points weight,status,stable")
}

// printToledoLatest 连上后列出最新一帧的各字段。
//
// 推送模型下「最新一帧」就是此刻秤的读数。连续输出通常 10~25 帧/秒，因此连上
// 后稍等让收帧循环攒一帧，再读一次即可。
//
// 注意重试判据不能用「样本数 > 0」：驱动在还没收到帧时也会为每个点位返回一条
// comm_error 样本（这是对的，否则采集层会把点位当「响应里缺项」）。判据必须是
// 「至少有一个点位不是 comm_error」，否则这段循环等价于只读一次。
func printToledoLatest(ctx context.Context, c driver.Conn, transport string, raw, asJSON bool) {
	fields := []string{"weight", "status", "mode", "unit", "stable", "motion", "decimals"}
	refs := make([]driver.PointRef, 0, len(fields))
	for _, f := range fields {
		refs = append(refs, driver.PointRef{
			ID:       f,
			Address:  model.Address{Kind: model.AddressSymbol, Path: f},
			DataType: toledoDefaultType(f),
		})
	}

	deadline := time.Now().Add(2 * time.Second)
	var out []driver.RawSample
	for time.Now().Before(deadline) {
		samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "latest", Points: refs}})
		if err == nil && hasFreshFrame(samples) {
			out = samples
			break
		}
		if out == nil && err == nil {
			// 先留住第一份（含 comm_error 的）结果，超时后照实打印。
			out = samples
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	if len(out) == 0 {
		fmt.Fprintln(os.Stderr, "未读到样本（连上后未收到连续输出帧）")
		os.Exit(1)
	}

	rows := make([]toledoRow, 0, len(out))
	for _, s := range out {
		row := toledoRow{Field: s.PointID, Quality: string(s.Quality)}
		if s.Err != nil {
			row.Error = s.Err.Error()
			rows = append(rows, row)
			continue
		}
		row.Value = s.Value.String()
		rows = append(rows, row)
	}
	if asJSON {
		out := map[string]any{"transport": transport, "rows": rows, "count": len(rows)}
		if raw {
			out["stats"] = toledoStatsMap(c)
		}
		emitJSON(out)
		return
	}
	fmt.Printf("最新一帧  传输 %s\n\n", transport)
	printRow([]int{12, 24, 14, 20}, "字段", "值", "质量", "说明")
	for _, r := range rows {
		v := r.Value
		if r.Error != "" {
			v = "错误: " + r.Error
		}
		printRow([]int{12, 24, 14, 20}, r.Field, v, r.Quality, toledoFieldDesc(r.Field))
	}
	if raw {
		printToledoRaw(c)
	}
}

// hasFreshFrame 判断一批样本里是否至少有一个点位真正拿到了帧。
//
// comm_error 表示「还没收到过帧」（见驱动质量码语义），它不是读数。
func hasFreshFrame(samples []driver.RawSample) bool {
	for _, s := range samples {
		if s.Quality != model.QualityCommError {
			return true
		}
	}
	return false
}

// toledoStats 是驱动收帧统计的本地镜像（CLI 的输出结构）。
type toledoStats struct {
	BytesSeen int    `json:"bytesSeen"`
	Dropped   int    `json:"dropped"`
	BadFrames int    `json:"badFrames"`
	HasFrame  bool   `json:"hasFrame"`
	FrameAge  int64  `json:"frameAgeMs"`
	LastError string `json:"lastError,omitempty"`
	Raw       string `json:"raw,omitempty"`
}

// statsOf 通过可选接口取驱动的收帧统计。
//
// 用接口断言而不是把连接断言成驱动内部类型：与 sl651read 取 Report 的方式一致，
// 自检命令只依赖驱动导出的能力，不依赖它的内部结构。
func statsOf(c driver.Conn) (toledoStats, bool) {
	getter, ok := c.(interface{ Stats() toledodrv.Stats })
	if !ok {
		return toledoStats{}, false
	}
	s := getter.Stats()
	return toledoStats{
		BytesSeen: s.BytesSeen,
		Dropped:   s.Dropped,
		BadFrames: s.BadFrames,
		HasFrame:  s.HasFrame,
		FrameAge:  s.FrameAge.Milliseconds(),
		LastError: s.LastError,
		Raw:       s.Raw,
	}, true
}

func toledoStatsMap(c driver.Conn) any {
	if s, ok := statsOf(c); ok {
		return s
	}
	return nil
}

// printToledoRaw 打印原始帧与收帧统计。
//
// 这是 Toledo 自检最有价值的一部分：只有推送、没有命令可发，现场唯一能看的就是
// 「线上到底来了什么字节」。字节数 > 0 而坏帧 > 0 且没有好帧，指向帧格式/参数；
// 字节数为 0 才指向接线与供电。
func printToledoRaw(c driver.Conn) {
	s, ok := statsOf(c)
	if !ok {
		return
	}
	fmt.Println()
	fmt.Println("收帧统计")
	printRow([]int{16, 40}, "项目", "值")
	printRow([]int{16, 40}, "读到字节", strconv.Itoa(s.BytesSeen))
	printRow([]int{16, 40}, "噪声丢弃", strconv.Itoa(s.Dropped))
	printRow([]int{16, 40}, "坏帧", strconv.Itoa(s.BadFrames))
	printRow([]int{16, 40}, "最近帧龄", strconv.FormatInt(s.FrameAge, 10)+"ms")
	if s.LastError != "" {
		printRow([]int{16, 40}, "最近解析错误", s.LastError)
	}
	if s.Raw != "" {
		fmt.Printf("\n最近一帧原文（不含 CR/LF）：%q\n", s.Raw)
	}
	if s.BytesSeen > 0 && !s.HasFrame {
		fmt.Println("\n提示：读到了字节但一帧都没成。查仪表帧格式（应为 ASCII 连续输出）、")
		fmt.Println("      分隔符设置与波特率/校验位，而不是先查接线。")
	}
}

// readToledoPoints 按字段列表走驱动的 Read 逐点读取。
//
// 走驱动的 Read（而不是自己解帧），与其它自检命令一致：让这条命令同时验证
// 「字段名在采集路径上也成立」。
func readToledoPoints(ctx context.Context, c driver.Conn, spec string, raw, asJSON bool) {
	parts := toledoRefs(spec)
	if len(parts) == 0 {
		fmt.Fprintln(os.Stderr, "-points 至少要给一个字段")
		os.Exit(2)
	}
	refs := make([]driver.PointRef, 0, len(parts))
	for _, p := range parts {
		refs = append(refs, driver.PointRef{
			ID:       p,
			Address:  model.Address{Kind: model.AddressSymbol, Path: p},
			DataType: toledoDefaultType(p),
		})
	}
	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "selfcheck", Points: refs}})
	if err != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		printToledoHints(err)
		os.Exit(1)
	}
	rows := make([]toledoRow, 0, len(samples))
	for _, s := range samples {
		row := toledoRow{Field: s.PointID, Quality: string(s.Quality)}
		if s.Err != nil {
			row.Error = s.Err.Error()
			rows = append(rows, row)
			continue
		}
		row.Value = s.Value.String()
		rows = append(rows, row)
	}
	if asJSON {
		emitJSON(map[string]any{"rows": rows, "count": len(rows)})
		return
	}
	printRow([]int{12, 24, 14, 20}, "字段", "值", "质量", "说明")
	for _, r := range rows {
		v := r.Value
		if r.Error != "" {
			v = "错误: " + r.Error
		}
		printRow([]int{12, 24, 14, 20}, r.Field, v, r.Quality, toledoFieldDesc(r.Field))
	}
}

// toledoRow 是逐字段输出的一行。
type toledoRow struct {
	Field   string `json:"field"`
	Value   string `json:"value,omitempty"`
	Quality string `json:"quality,omitempty"`
	Error   string `json:"error,omitempty"`
}

// toledoDefaultType 按字段给默认数据类型（与驱动 defaultDataType 一致）。
func toledoDefaultType(field string) model.DataType {
	switch field {
	case "stable", "motion":
		return model.TypeBool
	case "decimals":
		return model.TypeInt64
	case "status", "unit", "mode":
		return model.TypeString
	default:
		return model.TypeFloat64
	}
}

// toledoFieldDesc 给字段一句话说明（输出用）。
func toledoFieldDesc(field string) string {
	switch field {
	case "weight":
		return "重量值（带符号）"
	case "status":
		return "ST=稳定 US=运动 OL=超量程 UL=欠量程"
	case "mode":
		return "GS=毛重 NT=净重 TR=皮重"
	case "unit":
		return "计量单位"
	case "stable":
		return "是否稳定"
	case "motion":
		return "是否运动中"
	case "decimals":
		return "小数位数"
	default:
		return ""
	}
}

// printToledoHints 按错误分类给出排查顺序。
//
// 推送型协议最大的坑是「收到字节但一帧都解不出」与「根本没收到」被混为一谈。
// 这里按驱动返回的错误分类分别给顺序，避免把帧格式选错（二进制 vs ASCII）误报
// 成链路断。
func printToledoHints(err error) {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		fmt.Fprintln(os.Stderr, "  超时排查顺序：")
		fmt.Fprintln(os.Stderr, "    1) 波特率/校验位（连续输出常见 9600 8N1，也有 8E1；8O1 偶见）")
		fmt.Fprintln(os.Stderr, "    2) 接线与供电：RS-232/RS-485 的 A/B、地线、24V 称重传感器供电")
		fmt.Fprintln(os.Stderr, "    3) 仪表是否真的在连续输出（有的型号需在菜单里开启 Continuous Output）")
	case driver.ErrProtocol:
		fmt.Fprintln(os.Stderr, "  协议层排查：")
		fmt.Fprintln(os.Stderr, "    · 若报错提到「二进制 TOLEDO 连续帧」：仪表被设成了 STX 开头的 18 字节")
		fmt.Fprintln(os.Stderr, "      二进制格式，本驱动只实现 ASCII 三段式，请在仪表上切到 ASCII/连续输出格式")
		fmt.Fprintln(os.Stderr, "    · 「帧应分为 3 段」通常是小数点/逗号被设成了别的分隔符，或掺了厂商前缀")
	case driver.ErrUnsupported:
		fmt.Fprintln(os.Stderr, "  不支持：")
		fmt.Fprintln(os.Stderr, "    · 本驱动只读 ASCII 连续输出；二进制 TOLEDO 格式、SICS 命令应答不在范围内")
	case driver.ErrAddress:
		fmt.Fprintln(os.Stderr, "  地址层排查：用 -list 查看可用读数字段")
	}
}
