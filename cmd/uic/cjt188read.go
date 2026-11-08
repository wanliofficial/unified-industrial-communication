package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	cj "uic/internal/codec/cjt188"
	"uic/internal/driver"
	cjdrv "uic/internal/driver/cjt188"
	"uic/internal/model"
)

// uic cjt188read 是户用计量仪表（水/气/热表）主站自检命令。
//
// 与 dlt645read 同理由：现场最缺的是「不装第三方软件就能自证链路通」的
// 手段。表计这块尤其需要，因为「读不到」有太多种同样表现的原因：
//
//   - 波特率/校验位不对（标准是 2400 8E1，但出厂常被改成无校验）；
//   - 表地址少一位、多一位，或把厂商码当成可选段；
//   - 前导 FE 个数不对（有的表要 2 个，有的要 4 个，有的干脆不要）；
//   - 仪表类型 T 配错：同一个 901F，水表按流量解，热量表按热量解，读出来
//     都是合法数字，但量纲完全不同。
//
// 因此这条命令的顺序是：先读地址（0x03）证明物理层通，再按 DI 读值。
// 读地址的请求是 T 与地址全 AA、报文标识固定 81 0A——它同时验证了波特率、
// 校验位、前导与控制码，是投入最小的一步。
//
// 边界：一次附着、若干字段、可选一次写（阀门或表地址）；不做周期轮询、
// 不做多表、不做广播（广播只用于读地址，且必须单机操作）。
func cjt188readCmd(args []string) {
	fs := flag.NewFlagSet("cjt188read", flag.ExitOnError)
	tr := fs.String("transport", "serial", "传输：serial | tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "从站地址 host:port（tcp）")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 2400, "波特率（serial）；标准典型值 2400")
	dataBits := fs.Int("dataBits", 8, "数据位 5..8（serial）")
	parity := fs.String("parity", "even", "校验：none | even | odd（serial）；标准要求 even")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")
	address := fs.String("address", "00000805000001", "表地址：完整 14 位十进制（含 4 位厂商码），不支持缩位")
	meterType := fs.String("type", "10", "仪表类型 T（2 位十六进制）：10 冷水/11 热水/12 直饮/13 中水/20 热量/21 冷量/30 燃气/40 其他")
	diList := fs.String("di", "901F:0", "字段列表，逗号分隔：DI[:偏移[:长度]]；901F 水表 0/5/10/17，热量表 0/5/10/15/20/25/28/31/34/41")
	write := fs.String("write", "", "写入值：-di A017 时为 open|close（或 1|0）；-di A018 时为 14 位新地址")
	allowControl := fs.Bool("allow-control", false, "允许阀门控制（A017）；高危，默认关闭")
	probe := fs.Bool("probe", false, "只读表地址（0x03）验证链路")
	raw := fs.Bool("raw", false, "额外显示未格式化的原始值（时间紧凑数、状态字十六进制、数据类型）")
	preamble := fs.Int("preamble", cj.PreambleLen, "前导 FE 字节数 0..4（默认 2）")
	gap := fs.Duration("gap", 30*time.Millisecond, "帧间静默")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	fs.Parse(args)

	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}
	if *preamble < 0 || *preamble > 4 {
		fmt.Fprintln(os.Stderr, "-preamble 必须在 0..4 之间")
		os.Exit(2)
	}

	opts := []string{fmt.Sprintf("preamble=%d", *preamble), "gap=" + gap.String()}
	if *write != "" {
		opts = append(opts, "allow-write")
		if *allowControl {
			opts = append(opts, "allow-control")
		}
	}
	labels := map[string]string{
		"address": *address,
		"type":    *meterType,
		"options": strings.Join(opts, ","),
	}

	ch := model.Channel{
		ID: "ch-cjt188", Protocol: "cjt188",
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

	c, err := cjdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（读表地址 0x03）失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：波特率/校验位（2400 8E1）→ 表地址 14 位 → 前导字节（-preamble 0）→ A/B 接线")
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "address": *address,
				"type": strings.ToUpper(*meterType), "link": "up", "probe": "0x03",
			})
			return
		}
		fmt.Printf("链路正常: 表地址 %s 类型 %s（读表地址 0x03 通过）\n",
			*address, strings.ToUpper(*meterType))
		return
	}

	mtype, err := parseMeterType(*meterType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	// 写命令的报文标识（A017/A018）不是可读数据项，不能走字段 schema；
	// 先分流，免得给阀门写一个「未收录的数据标识」的误导性错误。
	if *write != "" {
		di, err := parseWriteDI(*diList)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		runCJT188Write(ctx, c, di, *write, *allowControl, *asJSON)
		return
	}

	fields, err := parseFieldList(*diList, mtype)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	points := make([]driver.PointRef, 0, len(fields))
	for _, f := range fields {
		points = append(points, cjt188PointRef(f))
	}
	// 同一个 DI 的多个字段放进同一个 span：驱动按 DI 缓存应答，四个字段
	// 只发一帧；跨 DI 的字段同样可以放在一起，驱动会各自发一帧。
	samples, rerr := c.Read(ctx, []driver.ReadSpan{{ID: "selfcheck", Points: points}})
	if rerr != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", rerr)
		os.Exit(1)
	}

	rows := make([]cjt188Row, 0, len(samples))
	for i, s := range samples {
		row := cjt188Row{DI: fields[i].DI.String(), Offset: fields[i].Offset}
		row.Name = fields[i].Name
		if u := fields[i].Unit; u != "" {
			row.Unit = u
		}
		row.Quality = string(s.Quality)
		if s.Err != nil {
			row.Error = s.Err.Error()
			rows = append(rows, row)
			continue
		}
		row.Type = string(s.Value.Type)
		switch fields[i].Kind {
		case cj.FieldTime:
			row.Value = s.Value.String()
			if t, err := cj.FromCompactTime(s.Value.Num); err == nil {
				row.Display = t.Format("2006-01-02 15:04:05")
			}
		case cj.FieldStatus:
			v := uint16(s.Value.Num)
			row.Value = fmt.Sprintf("0x%04X", v)
			row.Display = cj.StatusText(v)
		default:
			row.Value = trimNum(s.Value.Num)
			row.Display = row.Value
			if row.Unit != "" {
				row.Display += " " + row.Unit
			}
		}
		rows = append(rows, row)
	}

	if *asJSON {
		emitJSON(map[string]any{
			"transport": ch.Transport.Kind, "address": *address,
			"type": strings.ToUpper(*meterType), "link": "up",
			"read": len(rows), "rows": rows,
		})
		return
	}

	fmt.Printf("表地址 %s  类型 %02X（%s）  传输 %s\n\n",
		*address, mtype, cj.TypeName(mtype), ch.Transport.Kind)
	if *raw {
		printRow([]int{8, 8, 22, 18, 10, 16, 10}, "DI", "偏移", "字段", "原始值", "类型", "解释", "质量")
		for _, r := range rows {
			printRow([]int{8, 8, 22, 18, 10, 16, 10},
				r.DI, strconv.Itoa(r.Offset), r.Name, r.rawValue(), r.Type, r.display(), r.Quality)
		}
		return
	}
	printRow([]int{8, 8, 22, 22}, "DI", "偏移", "字段", "值")
	for _, r := range rows {
		printRow([]int{8, 8, 22, 22}, r.DI, strconv.Itoa(r.Offset), r.Name, r.display())
	}
}

// cjt188Row 是自检输出的一行。
type cjt188Row struct {
	DI      string `json:"di"`
	Offset  int    `json:"offset"`
	Name    string `json:"name,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Type    string `json:"type,omitempty"`
	Value   string `json:"value,omitempty"`
	Display string `json:"display,omitempty"`
	Quality string `json:"quality,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (r cjt188Row) display() string {
	if r.Error != "" {
		return "错误: " + r.Error
	}
	if r.Display == "" {
		return r.Value
	}
	return r.Display
}

func (r cjt188Row) rawValue() string {
	if r.Error != "" {
		return ""
	}
	return r.Value
}

// fieldSpec 是命令行里的一个字段。
type fieldSpec struct {
	DI     cj.DI
	Offset int
	Length int
	Name   string
	Unit   string
	Kind   cj.FieldKind
}

func parseMeterType(s string) (byte, error) {
	s = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "0X")
	n, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0, fmt.Errorf("仪表类型 %q 不是 2 位十六进制（如 10/20/30）", s)
	}
	t := byte(n)
	if t < 0x10 || t > 0x4F {
		return 0, fmt.Errorf("仪表类型 %02X 不是标准取值（10..4F）", t)
	}
	return t, nil
}

// parseFieldList 解析 `DI[:偏移[:长度]]` 列表。
//
// 偏移是 DI 数据域内的字节偏移，不是寄存器偏移；只给 DI 时默认取第一个
// 字段（偏移 0），因为绝大多数点表只关心当前累积量。
func parseFieldList(spec string, mtype byte) ([]fieldSpec, error) {
	var out []fieldSpec
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		segs := strings.Split(part, ":")
		if len(segs) > 3 {
			return nil, fmt.Errorf("字段 %q 格式应为 DI[:偏移[:长度]]", part)
		}
		di, err := cj.ParseDI(segs[0])
		if err != nil {
			return nil, err
		}
		offset := 0
		if len(segs) >= 2 && strings.TrimSpace(segs[1]) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(segs[1]))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("字段 %q 的偏移必须是非负整数", part)
			}
			offset = n
		}
		field, err := cj.LookupField(mtype, di, offset)
		if err != nil {
			return nil, err
		}
		if len(segs) == 3 && strings.TrimSpace(segs[2]) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(segs[2]))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("字段 %q 的长度必须是正整数", part)
			}
			if n != field.Width() {
				return nil, fmt.Errorf("字段 %s（%s:%d）长度为 %d，但 DI 定义为 %d 字节",
					field.Name, di, offset, n, field.Width())
			}
		}
		key := fmt.Sprintf("%s:%d", di, offset)
		if seen[key] {
			continue
		}
		seen[key] = true
		unit := field.Qty.Unit()
		switch field.Name {
		case "供水温度", "回水温度":
			unit = "℃"
		case "累积工作时间":
			unit = "h"
		}
		out = append(out, fieldSpec{
			DI: di, Offset: offset, Length: field.Width(),
			Name: field.Name, Unit: unit, Kind: field.Kind,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-di 至少要给一个字段")
	}
	return out, nil
}

// cjt188PointRef 把字段变成驱动需要的点位视图。
func cjt188PointRef(f fieldSpec) driver.PointRef {
	dt := model.TypeFloat64
	if f.Kind == cj.FieldStatus {
		dt = model.TypeUint16
	}
	return driver.PointRef{
		ID: f.DI.String() + ":" + strconv.Itoa(f.Offset),
		Address: model.Address{
			Kind: model.AddressRegister, Area: f.DI.String(),
			Offset: f.Offset, Length: 0, Bit: -1,
		},
		DataType: dt,
	}
}

// parseWriteDI 解析写命令的报文标识：只接受单个 DI，且只能是没有字段
// 概念的 A017/A018。写成别的 DI 在本地就拒绝，不能让它变成一次「写进去
// 但表不认」的静默失败。
func parseWriteDI(spec string) (cj.DI, error) {
	part := strings.TrimSpace(spec)
	if strings.Contains(part, ",") {
		return cj.DI{}, fmt.Errorf("-write 只能配一个 -di（一次只写一个报文标识）")
	}
	if i := strings.IndexByte(part, ':'); i >= 0 {
		part = part[:i]
	}
	di, err := cj.ParseDI(part)
	if err != nil {
		return cj.DI{}, err
	}
	switch di {
	case cj.DIValve, cj.DIWriteAddr:
		return di, nil
	}
	return cj.DI{}, fmt.Errorf("CJ/T 188 只支持写阀门（-di A017）与写表地址（-di A018），不支持 %s", di)
}

// runCJT188Write 下发一条写命令：阀门（A017）或表地址（A018）。
func runCJT188Write(ctx context.Context, c driver.Conn, di cj.DI, value string, allowControl, asJSON bool) {
	var val model.Value
	switch di {
	case cj.DIValve:
		if !allowControl {
			fmt.Fprintln(os.Stderr, "写阀门是高危操作，必须显式加 -allow-control")
			os.Exit(2)
		}
		v, err := parseValve(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		val = model.NumValue(model.TypeFloat64, v)
	case cj.DIWriteAddr:
		a, err := cj.ParseAddr(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		if a.HasWildcard() {
			fmt.Fprintln(os.Stderr, "写表地址必须给完整的 14 位数字，不能含 AA 通配字节")
			os.Exit(2)
		}
		val = model.TextValue(model.TypeString, a.String())
	default:
		fmt.Fprintf(os.Stderr, "CJ/T 188 只支持写阀门（-di A017）与写表地址（-di A018），不支持 %s\n", di)
		os.Exit(2)
	}

	res, werr := c.Write(ctx, []driver.WriteRequest{{
		PointID: di.String(),
		Address: model.Address{Kind: model.AddressRegister, Area: di.String(), Bit: -1},
		Value:   val,
	}})
	if werr != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", werr)
		os.Exit(1)
	}
	if len(res) == 1 && res[0].OK {
		if asJSON {
			emitJSON(map[string]any{"di": di.String(), "value": val.String(), "ok": true})
			return
		}
		fmt.Printf("写入 %s = %s 完成\n", di, val.String())
		return
	}
	var reason error
	if len(res) == 1 {
		reason = res[0].Err
	}
	if asJSON {
		emitJSON(map[string]any{"di": di.String(), "ok": false, "error": errText(reason)})
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "写入被拒绝: %v\n", reason)
	os.Exit(1)
}

// parseValve 把命令行字符串翻译成阀门值（1 开、0 关）。
func parseValve(s string) (float64, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open", "on", "1", "开":
		return 1, nil
	case "close", "off", "0", "关":
		return 0, nil
	}
	return 0, fmt.Errorf("阀门值 %q 不合法（open/1/开 或 close/0/关）", s)
}

// trimNum 去掉浮点数的多余小数位。
func trimNum(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
