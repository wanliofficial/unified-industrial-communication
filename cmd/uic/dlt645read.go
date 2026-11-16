package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	dlt645codec "uic/internal/codec/dlt645"
	"uic/internal/driver"
	dlt645drv "uic/internal/driver/dlt645"
	"uic/internal/model"
)

// uic dlt645read 是一个极小的电能表（645 协议）主站自检命令。
//
// 与 mbread/mcread/finsread/enipread/bacread/iec104read 同理由：现场最缺的
// 是「不装任何第三方软件就能自证链路通」的手段。而电能表这块尤其需要：
//
//   - RS-485 上「不通」有七八种原因（波特率、校验位、表地址、前导字节、
//     表本身还在休眠），每一种的表现都是「没有应答」；
//   - 数据标识是 8 位十六进制，写错一位就变成另一个数据项，回的还是合法
//     帧，值看起来也「正常」但差了量级；
//   - 标准里同一个量的宽度不统一（电压 2 字节、电流 3 字节、电能 4 字节），
//     用万用表对照时很容易以为是表的问题。
//
// 这条命令把这三件事都摊开：先读通信地址（0x13）证明物理层通，再按 DI
// 读值并把原始字节一起打出来（-raw），最后才是点表该配什么。
//
// 边界：一次附着、若干个 DI、可选一次写；不做周期轮询、不做多表、不做
// 广播（广播校时/广播冻结会同时改掉总线上所有表的时间，必须显式走单独的
// 运维流程，不能藏在一条读命令里）。
func dlt645readCmd(args []string) {
	fs := flag.NewFlagSet("dlt645read", flag.ExitOnError)
	tr := fs.String("transport", "serial", "传输：serial | tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "从站地址 host:port（tcp）")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 2400, "波特率（serial）；标准速率 600/1200/2400/4800/9600/19200")
	dataBits := fs.Int("dataBits", 8, "数据位 5..8（serial）")
	parity := fs.String("parity", "even", "校验：none | even | odd（serial）；标准要求 even")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")
	address := fs.String("address", "000000000001", "表地址：12 位 BCD，可缩位（如 0042），高位自动补 AA 通配")
	diList := fs.String("di", "00000000,02010100", "数据标识，逗号分隔（8 位十六进制，手册顺序 DI3DI2DI1DI0）")
	password := fs.String("password", "", "写命令密码，6 位十六进制（PA P0 P1 P2 里的 P0..P2，3 字节）")
	write := fs.String("write", "", "写入值（按 DI 的数据项类型解释）")
	allowControl := fs.Bool("allow-control", false, "(本命令不支持) 拉合闸在驱动层就没有入口，给了会报错")
	probe := fs.Bool("probe", false, "只读通信地址（0x13）验证链路")
	raw := fs.Bool("raw", false, "额外显示数据块/字节串的原始数据")
	preamble := fs.Int("preamble", dlt645codec.PreambleLen, "前导 FE 字节数 0..8（默认 4）")
	gap := fs.Duration("gap", 30*time.Millisecond, "帧间静默")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	fs.Parse(args)

	// 传输方式的默认值取决于「用户到底给了哪一组参数」：现场绝大多数是
	// 串口，但自检时最常用的写法是只给 -addr 指向串口服务器。若 -transport
	// 没写却给了 -addr，就按 tcp 处理，免得为了一个打错的默认值先去改
	// USB 转 485。
	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}

	if *preamble < 0 || *preamble > 8 {
		fmt.Fprintln(os.Stderr, "-preamble 必须在 0..8 之间")
		os.Exit(2)
	}

	// 写命令的选项通过设备标签传给驱动：与运行时走同一条路径，避免
	// 「CLI 能写、网关不能写」这种只在一侧出现的差异。
	opts := []string{fmt.Sprintf("preamble=%d", *preamble), "gap=" + gap.String()}
	if *allowControl {
		// 驱动层根本没有控制入口；把 allow-control 透传下去只会在 Open
		// 时报错，但报在那里说不清是哪个参数，所以在这里先拦。
		fmt.Fprintln(os.Stderr, "本命令不提供拉合闸/清零（-allow-control 无效）：控制类命令请用厂商工具")
		os.Exit(2)
	}
	if *write != "" {
		opts = append(opts, "allow-write")
		if *password != "" {
			opts = append(opts, "password="+*password)
		}
	}
	labels := map[string]string{
		"address": *address,
		"options": strings.Join(opts, ","),
	}

	ch := model.Channel{
		ID: "ch-dlt645", Protocol: "dlt645",
		Transport: model.Transport{
			Kind: strings.ToLower(*tr), Address: *addr, Port: *port,
			Baud: *baud, DataBits: *dataBits, Parity: *parity, StopBits: *stopBits,
		},
		Timeout: model.Duration(*timeout),
	}
	if ch.Transport.Kind == "serial" {
		ch.Transport.Address = ""
	}
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels:   labels,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*8)
	defer cancel()

	c, err := dlt645drv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（读通信地址 0x13）失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：波特率/校验位 → 表地址 → 前导字节（-preamble 0）→ A/B 接线")
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "address": *address,
				"link": "up", "probe": "0x13",
			})
			return
		}
		fmt.Printf("链路正常: 表地址 %s（读通信地址 0x13 通过）\n", *address)
		return
	}

	dis, err := parseDIList(*diList)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	if *write != "" {
		runDLT645Write(ctx, c, dis, *write, *raw, *asJSON)
		return
	}

	spans := make([]driver.ReadSpan, 0, len(dis))
	for _, di := range dis {
		ref := dlt645PointRef(di)
		// 按 DI 逐个成 span：协议的寻址是数据标识，两个不同 DI 之间不存在
		// 「连续」的概念，硬塞进同一个 span 只会让输出顺序难以解释。
		spans = append(spans, driver.ReadSpan{ID: di.String(), Points: []driver.PointRef{ref}})
	}
	samples, rerr := c.Read(ctx, spans)
	if rerr != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", rerr)
		os.Exit(1)
	}

	rows := make([]dlt645Row, 0, len(samples))
	for _, s := range samples {
		row := dlt645Row{DI: s.PointID}
		item, known := dlt645codec.Lookup(mustDIQuiet(s.PointID))
		if known {
			row.Name = item.Name
			row.Width = item.Width
		}
		row.Quality = string(s.Quality)
		if s.Err != nil {
			row.Error = s.Err.Error()
			rows = append(rows, row)
			continue
		}
		row.Type = string(s.Value.Type)
		if s.Value.Type == model.TypeBytes {
			// 字节串直接按十六进制显示：它本来就不是文本，当成字符串输出
			// 会变成一堆控制字符和替换符，既看不懂也贴不进工单。
			rawBytes := []byte(s.Value.Text)
			row.Value = hexString(rawBytes)
			if item.DI.Block() {
				row.Width = len(rawBytes)
			}
		} else {
			row.Value = s.Value.String()
		}
		rows = append(rows, row)
	}

	if *asJSON {
		emitJSON(map[string]any{
			"transport": ch.Transport.Kind, "address": *address,
			"link": "up", "read": len(rows), "rows": rows,
		})
		return
	}

	fmt.Printf("表地址 %s  传输 %s\n\n", *address, ch.Transport.Kind)
	if *raw {
		printRow([]int{10, 28, 20, 12}, "DI", "数据项", "值", "质量")
		for _, r := range rows {
			printRow([]int{10, 28, 20, 12}, r.DI, r.Name, r.display(), r.Quality)
		}
		return
	}
	printRow([]int{10, 28, 20}, "DI", "数据项", "值")
	for _, r := range rows {
		printRow([]int{10, 28, 20}, r.DI, r.Name, r.display())
	}
}

// inferTransport 按「用户给了哪组参数」推断传输方式。
//
// 单独抽出来是因为这条规则只在一种情况下生效：`-transport` 没写。把它写成
// 纯函数，测试才能覆盖「只给 -addr」「只给 -port」「都不给」三种写法，而不必
// 真的去打开串口。
func inferTransport(given func(string) bool, current string) string {
	if given("transport") {
		return current
	}
	switch {
	case given("addr"):
		return "tcp"
	case given("port"):
		return "serial"
	}
	return current
}

// flagSet 判断某个 flag 是否被命令行显式给过。
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// dlt645Row 是自检输出的一行。
type dlt645Row struct {
	DI      string `json:"di"`
	Name    string `json:"name,omitempty"`
	Width   int    `json:"width,omitempty"`
	Type    string `json:"type,omitempty"`
	Value   string `json:"value,omitempty"`
	Quality string `json:"quality,omitempty"`
	Error   string `json:"error,omitempty"`
	Hex     string `json:"hex,omitempty"`
}

func (r dlt645Row) display() string {
	if r.Error != "" {
		return "错误: " + r.Error
	}
	if r.Hex != "" {
		return r.Value + "  [" + r.Hex + "]"
	}
	return r.Value
}

// runDLT645Write 下发一条写命令。
//
// 只做普通写数据（0x14）。拉合闸是另一套数据域形状（没有 DI，而是密码 +
// 操作者代码 + 控制类型 + 有效期），本驱动刻意不提供入口——一个「能读」
// 的驱动不应该顺手能把闸拉了，见包文档的边界说明。
func runDLT645Write(ctx context.Context, c driver.Conn, dis []dlt645codec.DI, value string, raw, asJSON bool) {
	if len(dis) != 1 {
		fmt.Fprintln(os.Stderr, "-write 只能配一个 -di（一次只写一个数据项）")
		os.Exit(2)
	}
	di := dis[0]
	item, ok := dlt645codec.Lookup(di)
	if !ok {
		fmt.Fprintf(os.Stderr, "未收录的数据标识 %s\n", di)
		os.Exit(2)
	}
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "trip" || lower == "allow" {
		fmt.Fprintln(os.Stderr, "拉合闸不在本驱动范围内（0x1C 是厂商备案扩展，各厂数据域不一致）；请用厂商工具")
		os.Exit(2)
	}

	val, err := dlt645Value(item, value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	ref := dlt645PointRef(di)
	res, werr := c.Write(ctx, []driver.WriteRequest{{
		PointID: di.String(), Address: ref.Address,
		DataType: ref.DataType, Value: val,
	}})
	if werr != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", werr)
		os.Exit(1)
	}
	if len(res) == 1 && res[0].OK {
		if asJSON {
			emitJSON(map[string]any{"di": di.String(), "name": item.Name,
				"value": val.String(), "ok": true})
			return
		}
		fmt.Printf("写入 %s（%s）= %s 完成\n", di, item.Name, val.String())
		return
	}
	var reason error
	if len(res) == 1 {
		reason = res[0].Err
	}
	if asJSON {
		emitJSON(map[string]any{"di": di.String(), "name": item.Name,
			"ok": false, "error": errText(reason)})
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "写入被拒绝: %v\n", reason)
	os.Exit(1)
}

func errText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// dlt645PointRef 把 DI 变成驱动需要的点位视图。
//
// 数据块（DI 含 FF）用 bytes 承载：它是若干数据项的拼接，没有单一数值
// 含义，硬解成 float64 只会给出一个「看起来像读数」的假值。
func dlt645PointRef(di dlt645codec.DI) driver.PointRef {
	dt := model.TypeFloat64
	if di.Block() {
		dt = model.TypeBytes
	}
	length := 0
	if item, ok := dlt645codec.Lookup(di); ok {
		length = item.Width
	}
	return driver.PointRef{
		ID: di.String(),
		Address: model.Address{
			Kind: model.AddressRegister, Area: di.String(),
			Offset: 0, Length: length, Bit: -1,
		},
		DataType: dt,
	}
}

// dlt645Value 把命令行字符串翻译成平台值。
//
// 时间类数据项接受 `YYYYMMDDhhmmss`（与驱动读出来的紧凑数同形），也接受
// `YYYY-MM-DD hh:mm:ss` 这种人写的形式。日期类（结算日等）没有年，另外接受
// 8 位 `MMDDhhmm`——因为读出来就是这个形状，「把刚读到的值原样写回」必须能用
// 同一条命令完成。
func dlt645Value(item dlt645codec.Item, s string) (model.Value, error) {
	s = strings.TrimSpace(s)
	switch item.Kind {
	case dlt645codec.KindTime:
		t, err := parseCompactTime(s)
		if err != nil {
			return model.Value{}, err
		}
		return model.NumValue(model.TypeFloat64, float64(t)), nil
	case dlt645codec.KindDate:
		t, err := parseDateCompact(s)
		if err != nil {
			return model.Value{}, err
		}
		return model.NumValue(model.TypeFloat64, float64(t)), nil
	case dlt645codec.KindAddr:
		addr, err := dlt645codec.ParseAddr(s)
		if err != nil {
			return model.Value{}, err
		}
		// 读的时候可以用缩位地址（高位由主站补 AA 通配），写的时候不行：
		// 把 `42` 写进通信地址数据项，等于让表号变成 `AA AA AA AA AA 42`，
		// 之后除了通配读就再也点不到它。
		for _, b := range addr {
			if b == dlt645codec.Wildcard {
				return model.Value{}, fmt.Errorf(
					"写通信地址必须给完整的 12 位数字（缩位写法 %q 会写入通配字节）", s)
			}
		}
		return model.TextValue(model.TypeString, dlt645codec.FormatAddr(addr)), nil
	}
	n, err := parseNumber(s)
	if err != nil {
		return model.Value{}, err
	}
	return model.NumValue(model.TypeFloat64, n), nil
}

// parseCompactTime 把两种写法统一成 YYYYMMDDhhmmss。
func parseCompactTime(s string) (int64, error) {
	if len(s) == 14 && !strings.ContainsAny(s, "-: T") {
		// 不直接按整数解析：`20261307120000` 也是 14 位数字，但它不是
		// 一个日期。走到这里的东西都会被写进电表时钟，范围检查必须在
		// 本地完成，而不是让表去判断。
		t, err := time.ParseInLocation("20060102150405", s, time.Local)
		if err != nil {
			return 0, fmt.Errorf("时间 %q 不是合法日期时间（格式 YYYYMMDDhhmmss）", s)
		}
		return compactOf(t), nil
	}
	layouts := []string{
		"2006-01-02 15:04:05", "2006-01-02T15:04:05",
		"2006-01-02 15:04", "2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return compactOf(t), nil
		}
	}
	return 0, fmt.Errorf("时间 %q 无法解析（可用 20261107123456 或 2026-11-07 12:34:56）", s)
}

func compactOf(t time.Time) int64 {
	return int64(t.Year())*1e10 + int64(t.Month())*1e8 + int64(t.Day())*1e6 +
		int64(t.Hour())*1e4 + int64(t.Minute())*100 + int64(t.Second())
}

// parseDateCompact 处理「没有年」的 4 字节日期数据项。
//
// 输入可以是 8 位 MMDDhhmm（读出来的形状），也可以是完整时间；后者只取
// 月/日/时/分。返回值的两个形状（8 位或 14 位）驱动都认——所以这里不做
// 归一化，保留用户写的那一种，出错时的提示才对得上输入。
func parseDateCompact(s string) (int64, error) {
	if len(s) == 8 && !strings.ContainsAny(s, "-: T") {
		t, err := time.ParseInLocation("01021504", s, time.Local)
		if err != nil {
			return 0, fmt.Errorf("日期 %q 不是合法的 MMDDhhmm（如 11071200 表示 11 月 7 日 12:00）", s)
		}
		return int64(t.Month())*1e6 + int64(t.Day())*1e4 +
			int64(t.Hour())*100 + int64(t.Minute()), nil
	}
	n, err := parseCompactTime(s)
	if err != nil {
		return 0, fmt.Errorf("%w；日期类数据项也接受 8 位 MMDDhhmm（如 11071200）", err)
	}
	return n, nil
}

// parseDIList 解析逗号分隔的 DI 列表。
func parseDIList(spec string) ([]dlt645codec.DI, error) {
	var out []dlt645codec.DI
	seen := map[dlt645codec.DI]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		di, err := dlt645codec.ParseDI(part)
		if err != nil {
			return nil, err
		}
		if di.DI0 == 0xFF && di.DI1 == 0xFF && di.DI2 == 0xFF && di.DI3 == 0xFF {
			// 全 FF 既不是合法数据项，也不是标准数据块；它几乎总是
			// 「地址还没填」的占位符。
			return nil, fmt.Errorf("数据标识 %s 不是有效的数据项", di)
		}
		if seen[di] {
			continue
		}
		seen[di] = true
		out = append(out, di)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-di 至少要给一个数据标识")
	}
	return out, nil
}

// mustDIQuiet 是给输出用的宽松解析：DI 一定已经由 parseDIList 校验过。
func mustDIQuiet(s string) dlt645codec.DI {
	di, _ := dlt645codec.ParseDI(s)
	return di
}

// hexString 把字节串渲染成 `01 02 03`。
func hexString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", v)
	}
	return sb.String()
}
