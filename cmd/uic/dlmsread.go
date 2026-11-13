package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	dl "uic/internal/codec/dlms"
	"uic/internal/driver"
	dlmsdrv "uic/internal/driver/dlms"
	"uic/internal/model"
)

// uic dlmsread 是 DLMS/COSEM（IEC 62056）主站的自检命令。
//
// 与其它表计自检命令同理由，但 DLMS 的排查顺序比 645/188/M-Bus 更需要
// 分阶段——因为它有**四层**，每层失败的表现都是"读不到"，但原因完全不同：
//
//	物理层  波特率/校验位（8N1）/接线；症状：SNRM 之后完全没有字节
//	HDLC    SNRM → UA；地址域（客户端 16 / 逻辑设备 1）或地址字节数错时
//	        表回 DM（断开模式）或静默
//	ACSE    AARQ → AARE；应用上下文或不支持无安全关联时 result≠0
//	COSEM   Get-Request → Get-Response；OBIS/类号/属性号错时回
//	        Data-Access-Result，而不是超时
//
// 因此命令的顺序是：
//
//  1. `-probe`：只建链（SNRM+AARQ）并打印**协商结果**。这一步不读任何点位，
//     所以它单独验证了前三层；
//  2. 不带参数：逐点读默认点位表（正向有功电能、三相电压/电流等），逐行
//     列出值、单位、scaler 与类号——这一步验证第四层；
//  3. `-obis`：只读用户指定的 OBIS，用于核对点表地址；
//  4. `-scan`：把内置常见 OBIS 表逐条试读，回答"这台表有哪些常见量"，
//     并直接给出可以粘进点表的地址。
//
// 边界与驱动一致：LN 引用 + 无安全 + HDLC 承载；只读，不做 SET/ACTION，
// 不做 HLS/GMAC 认证，不做 SN 引用，不做分块读。
func dlmsreadCmd(args []string) {
	fs := flag.NewFlagSet("dlmsread", flag.ExitOnError)
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4051", "从站地址 host[:port]（tcp），端口省略时按 4051")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 9600, "波特率（serial）；典型值 300/9600/19200/38400/115200")
	dataBits := fs.Int("dataBits", 8, "数据位（serial）；HDLC 固定 8")
	parity := fs.String("parity", "none", "校验：none | even | odd（serial）；HDLC profile 规定 8N1")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")

	server := fs.String("address", "1", "服务端逻辑设备地址 0..16383（1 是管理逻辑设备）")
	client := fs.Int("client", 16, "客户端 HDLC 地址 0..127（16 是公共客户端）")
	addrSize := fs.Int("addrSize", 1, "地址域字节数 1|2|4（必须与表的 profile 一致）")
	obis := fs.String("obis", "", "OBIS 地址列表，逗号分隔：1.0.1.8.0.255 或 1.0.1.8.0.255:3:2")
	probe := fs.Bool("probe", false, "只建链（SNRM + AARQ）并打印协商结果，不读点位")
	scan := fs.Bool("scan", false, "逐条试读内置常见 OBIS 表，回答「这台表有哪些常见量」")
	list := fs.Bool("list", false, "打印内置常见 OBIS 表（不需要连表）")
	raw := fs.Bool("raw", false, "额外显示类号/属性号/scaler 原始值")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	retries := fs.Int("retries", 2, "单请求重发次数（不含首次）")
	gap := fs.Duration("gap", 20*time.Millisecond, "帧间静默")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	maxPDU := fs.Int("maxPDU", 512, "建链时声明并采用的 max-receive-PDU")
	fs.Parse(args)

	if *list {
		printDLMSCommon(*asJSON)
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
	if *client < 0 || *client > 0x7F {
		fmt.Fprintln(os.Stderr, "-client 必须在 0..127 之间")
		os.Exit(2)
	}
	srvAddr, err := parseServerAddr(*server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *addrSize != 1 && *addrSize != 2 && *addrSize != 4 {
		fmt.Fprintln(os.Stderr, "-addrSize 只能是 1、2 或 4")
		os.Exit(2)
	}
	if *maxPDU < 64 || *maxPDU > 4096 {
		fmt.Fprintln(os.Stderr, "-maxPDU 必须在 64..4096 之间")
		os.Exit(2)
	}

	ch := model.Channel{
		ID: "ch-dlms", Protocol: "dlms",
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
			"address": strconv.Itoa(srvAddr),
			"options": fmt.Sprintf("client=%d,server=%d,addr-size=%d,retries=%d,gap=%s,max-pdu=%d",
				*client, srvAddr, *addrSize, *retries, gap.String(), *maxPDU),
		},
	}

	// 建链本身可能要走 SNRM + AARQ 两次往返，留足余量。
	ctx, cancel := context.WithTimeout(context.Background(), *timeout*8)
	defer cancel()

	c, err := dlmsdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		printDLMSHints(err)
		os.Exit(1)
	}
	defer c.Close()

	info, ok := c.(interface {
		Association() dlmsdrv.AssociationInfo
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供关联信息")
		os.Exit(1)
	}

	if *probe {
		printDLMSProbe(info.Association(), srvAddr, ch.Transport.Kind, *asJSON)
		return
	}

	scanner, ok := c.(interface {
		Scan(context.Context) ([]dlmsdrv.ScanResult, error)
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供扫描")
		os.Exit(1)
	}

	if *scan {
		results, err := scanner.Scan(ctx)
		if err != nil && len(results) == 0 {
			fmt.Fprintf(os.Stderr, "扫描失败: %v\n", err)
			printDLMSHints(err)
			os.Exit(1)
		}
		printDLMSScan(results, srvAddr, ch.Transport.Kind, *raw, *asJSON)
		return
	}

	// 默认点位：一台三相电表上最常读的几项。选这些是因为它们覆盖了
	// A-XDR 的三种形态（uint32 电能、带 scaler 的 uint16 瞬时量、
	// octet-string 表号），任何一层解码错位都会在这里露出来。
	spec := *obis
	if strings.TrimSpace(spec) == "" {
		spec = strings.Join(defaultDLMSPoints(), ",")
	}
	readDLMSPoints(ctx, c, spec, info.Association(), *raw, *asJSON)
}

// defaultDLMSPoints 返回自检默认读取的 OBIS 列表。
func defaultDLMSPoints() []string {
	return []string{
		"1.0.1.8.0.255",  // 正向有功电能（class 3 Register，uint32）
		"1.0.32.7.0.255", // A 相电压（class 3 Register，uint16 + scaler）
		"1.0.31.7.0.255", // A 相电流
		"1.0.1.7.0.255",  // 总有功功率
		"1.0.13.7.0.255", // 功率因数
		"1.0.14.7.0.255", // 电网频率
		"0.0.96.1.0.255", // 表号（class 1 Data，octet-string）
	}
}

// parseServerAddr 解析服务端逻辑设备地址。
//
// 为什么单独抽一个函数：这个值的合法范围取决于地址域字节数（1 字节只有
// 7 位、2 字节 14 位、4 字节 28 位），但地址域本身也是配置项——把两个
// 检查放在一处，才能给出"用 2 字节地址域才能到 0x1234"这种可操作提示。
func parseServerAddr(s string) (int, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.ParseUint(s, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("服务端地址 %q 不是整数（0..16383）", s)
	}
	if n > 0x3FFF {
		return 0, fmt.Errorf("服务端地址 %q 超出 14 位范围（0..16383）；"+
			"更大的地址需要 4 字节地址域，超出本驱动的自检范围", s)
	}
	if n == 0x7F {
		return 0, fmt.Errorf("服务端地址 0x7F 是广播地址，不能作为单表地址")
	}
	return int(n), nil
}

// printDLMSHints 按错误分类给出排查顺序。
//
// 这一段是自检命令最有价值的部分：DLMS 的四层都把失败报成"读不到"，
// 只有知道"该先查哪一层"才不会在现场反复改波特率。
func printDLMSHints(err error) {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		fmt.Fprintln(os.Stderr, "  超时排查顺序：")
		fmt.Fprintln(os.Stderr, "    1) 波特率/校验位——HDLC profile 是 8N1，配成 8E1 会收到全错位的字节")
		fmt.Fprintln(os.Stderr, "    2) 客户端地址（默认 16）与逻辑设备地址（默认 1）是否与表一致")
		fmt.Fprintln(os.Stderr, "    3) 地址域字节数（-addrSize 1/2/4）是否与表的 profile 一致")
		fmt.Fprintln(os.Stderr, "    4) A/B 接线与终端电阻")
	case driver.ErrProtocol:
		fmt.Fprintln(os.Stderr, "  协议层排查：")
		fmt.Fprintln(os.Stderr, "    · 表回 DM（断开模式）通常是客户端或逻辑设备地址不对")
		fmt.Fprintln(os.Stderr, "    · AARE result≠0 说明表拒绝了关联：可能要求 HLS/GMAC 认证（本驱动不支持）")
		fmt.Fprintln(os.Stderr, "    · 若提示「收到坏帧」，先确认波特率与校验位，而不是怀疑地址")
	case driver.ErrAddress:
		fmt.Fprintln(os.Stderr, "  地址层排查：")
		fmt.Fprintln(os.Stderr, "    · object-undefined 是 OBIS 写错；object-class-inconsistent 是类号写错")
		fmt.Fprintln(os.Stderr, "    · 私有 OBIS 必须显式写 `:<class>:<attribute>`，驱动不会猜类号")
		fmt.Fprintln(os.Stderr, "    · 用 -scan 看这台表上实际有哪些常见量")
	}
}

// printDLMSCommon 打印内置常见 OBIS 表。
//
// 这张表的价值不是"标准大全"，而是"跨厂商一致的条目"：现场直接抄地址
// 就能读，不需要先读 object_list。厂商私有扩展刻意不在表里。
func printDLMSCommon(asJSON bool) {
	objs := dl.CommonObjects()
	type row struct {
		OBIS      string `json:"obis"`
		Class     uint16 `json:"class"`
		ClassName string `json:"className"`
		Attribute int8   `json:"attribute"`
		Name      string `json:"name"`
		Unit      string `json:"unit,omitempty"`
		PointAddr string `json:"pointAddress"`
	}
	rows := make([]row, 0, len(objs))
	for _, o := range objs {
		rows = append(rows, row{
			OBIS: o.OBIS, Class: o.Class, ClassName: dl.ClassName(o.Class),
			Attribute: o.Attribute, Name: o.Name, Unit: o.Unit,
			// PointAddr 是"能直接粘进点表"的形态：显式带上类号与属性号，
			// 这样即使换个表、内置表推导不出类号也能用。
			PointAddr: fmt.Sprintf("%s:%d:%d", o.OBIS, o.Class, o.Attribute),
		})
	}
	if asJSON {
		emitJSON(map[string]any{"commonObjects": rows, "count": len(rows)})
		return
	}
	fmt.Printf("内置常见 OBIS 表（%d 条，跨厂商一致条目；私有扩展不在表内）\n\n", len(rows))
	printRow([]int{20, 34, 16, 10},
		"点表地址（可粘贴）", "含义", "类", "属性")
	for _, r := range rows {
		printRow([]int{20, 34, 16, 10},
			r.PointAddr, r.Name, r.ClassName, strconv.Itoa(int(r.Attribute)))
	}
	fmt.Println()
	fmt.Println("用法：uic dlmsread -addr 127.0.0.1:4051 -obis 1.0.1.8.0.255")
}

// printDLMSProbe 打印建链结果与协商参数。
func printDLMSProbe(a dlmsdrv.AssociationInfo, server int, transport string, asJSON bool) {
	if asJSON {
		emitJSON(map[string]any{
			"transport": transport, "serverAddress": server, "link": "up",
			"associated": a.Associated, "dlmsVersion": a.DLMSVersion,
			"conformance": a.Conformance.String(),
			"conformanceRaw": fmt.Sprintf("%02X %02X %02X",
				a.Conformance.Bytes()[0], a.Conformance.Bytes()[1], a.Conformance.Bytes()[2]),
			"maxPDU": a.MaxPDU, "vaaName": fmt.Sprintf("0x%04X", a.VAAName),
			"clientAddress": a.ClientAddr, "addressSize": a.AddrSize,
		})
		return
	}
	c := a.Conformance.Bytes()
	fmt.Printf("链路正常: 逻辑设备 %d 已建链并通过应用关联（传输 %s）\n", server, transport)
	fmt.Printf("  HDLC   客户端 %d / 服务端 %d / 地址域 %d 字节\n",
		a.ClientAddr, a.ServerAddr, a.AddrSize)
	fmt.Printf("  ACSE   DLMS 版本 %d  VAA 0x%04X  max-PDU %d\n", a.DLMSVersion, a.VAAName, a.MaxPDU)
	fmt.Printf("  COSEM  conformance %02X %02X %02X（%s）\n",
		c[0], c[1], c[2], a.Conformance.String())
	fmt.Println()
	fmt.Println("注意：上面的 conformance 是**服务端协商后**的位块，不是本端请求的那份。")
	fmt.Println("      若缺少 get，则本表不允许读属性（会回 read-write-denied）。")
	if !a.Conformance.Has(dl.ConfGet) {
		fmt.Println("      ⚠ 协商结果不含 get：读取大概率失败，先确认关联上下文是否正确")
	}
}

// dlmsRow 是逐点输出的一行。
type dlmsRow struct {
	Ref        string  `json:"ref"`
	OBIS       string  `json:"obis"`
	Class      uint16  `json:"class"`
	ClassName  string  `json:"className"`
	Attribute  int8    `json:"attribute"`
	Name       string  `json:"name,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	Type       string  `json:"type"`
	Value      string  `json:"value"`
	Numeric    float64 `json:"numeric,omitempty"`
	HasNumeric bool    `json:"hasNumeric,omitempty"`
	Quality    string  `json:"quality,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// dlmsSplitAddr 把 `<OBIS>[:<class>[:<attribute>]]` 拆成 OBIS 与后缀。
//
// 必须走 dlmscodec.SplitRef：OBIS 的标准写法（`1-0:1-8-0-255`）本身就含
// 冒号，按"第一个冒号"切会把它切坏。SplitRef 靠"前缀必须能解析成 6 段"
// 唯一确定切点。
func dlmsSplitAddr(s string) (obis, suffix string) {
	s = strings.TrimSpace(s)
	part, classID, attrID, err := dl.SplitRef(s)
	if err != nil {
		// 解析不了的地址原样返回，交给表格显示原始字段，不要在这里崩。
		return s, ""
	}
	if classID < 0 {
		return part, ""
	}
	if attrID < 0 {
		return part, strconv.Itoa(classID)
	}
	return part, strconv.Itoa(classID) + ":" + strconv.Itoa(attrID)
}

// dlmsRowOf 把一次采样变成可打印的一行。
func dlmsRowOf(s driver.RawSample, ref driver.PointRef) dlmsRow {
	obis, suffix := dlmsSplitAddr(ref.Address.Path)
	row := dlmsRow{
		Ref: s.PointID, OBIS: obis, Attribute: dl.AttrValue,
		Quality: string(s.Quality),
	}
	// 名称与单位始终按 OBIS 查内置表：即使地址里显式写了类号，表格的
	// 「含义」一列也应该有中文名，否则现场无法核对地址对不对。
	obj, known := dl.LookupOBIS(obis)
	if known {
		row.Name, row.Unit = obj.Name, obj.Unit
	}
	// 类号与属性号：给了后缀就用后缀，否则用内置表的推导值。都拿不到就
	// 留空，让「不认识的私有 OBIS」在输出里显式留白，而不是填一个猜的值。
	if suffix != "" {
		parts := strings.Split(suffix, ":")
		if n, err := strconv.Atoi(parts[0]); err == nil {
			row.Class = uint16(n)
		}
		if len(parts) > 1 {
			if m, err := strconv.Atoi(parts[1]); err == nil {
				row.Attribute = int8(m)
			}
		}
	} else if known {
		row.Class, row.Attribute = obj.Class, obj.Attribute
	}
	row.ClassName = dl.ClassName(row.Class)

	if s.Err != nil {
		row.Error = s.Err.Error()
		return row
	}
	row.Type = string(s.Value.Type)
	row.Value = s.Value.String()
	if row.Unit != "" {
		row.Value += " " + row.Unit
	}
	if v, ok := s.Value.AsFloat(); ok {
		row.Numeric, row.HasNumeric = v, true
	}
	return row
}

// readDLMSPoints 按地址列表逐点读取并打印。
//
// 走驱动的 Read（而不是自己组帧），目的与其它自检命令一致：让这条命令
// 同时验证「点表地址在采集路径上也成立」。自检通过而采集失败是最难排查
// 的一类问题，通常就是地址在两条路径上被解析成了不同东西。
func readDLMSPoints(ctx context.Context, c driver.Conn, spec string,
	a dlmsdrv.AssociationInfo, raw, asJSON bool) {
	parts := dlmsRefs(spec)
	if len(parts) == 0 {
		fmt.Fprintln(os.Stderr, "-obis 至少要给一个地址")
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
		printDLMSHints(err)
		os.Exit(1)
	}
	rows := make([]dlmsRow, 0, len(samples))
	byID := map[string]driver.PointRef{}
	for _, r := range refs {
		byID[r.ID] = r
	}
	for _, s := range samples {
		rows = append(rows, dlmsRowOf(s, byID[s.PointID]))
	}
	if asJSON {
		emitJSON(map[string]any{
			"serverAddress": a.ServerAddr, "associated": a.Associated,
			"maxPDU": a.MaxPDU, "rows": rows, "count": len(rows),
		})
		return
	}
	if raw {
		printRow([]int{20, 16, 10, 8, 22, 12},
			"点表地址", "类", "属性", "类型", "值", "质量")
		for _, r := range rows {
			v := r.Value
			if r.Error != "" {
				v = "错误: " + r.Error
			}
			printRow([]int{20, 16, 10, 8, 22, 12},
				r.Ref, r.ClassName, strconv.Itoa(int(r.Attribute)), r.Type, v, r.Quality)
		}
		return
	}
	printRow([]int{20, 30, 24, 10}, "点表地址", "含义", "值", "质量")
	for _, r := range rows {
		v := r.Value
		if r.Error != "" {
			v = "错误: " + r.Error
		}
		name := r.Name
		if name == "" {
			name = r.ClassName
		}
		printRow([]int{20, 30, 24, 10}, r.Ref, name, v, r.Quality)
	}
}

// printDLMSScan 打印常见 OBIS 表的试读结果。
func printDLMSScan(results []dlmsdrv.ScanResult, server int, transport string, raw, asJSON bool) {
	type row struct {
		OBIS       string `json:"obis"`
		PointAddr  string `json:"pointAddress"`
		Name       string `json:"name"`
		Class      uint16 `json:"class"`
		ClassName  string `json:"className"`
		OK         bool   `json:"ok"`
		Value      string `json:"value,omitempty"`
		ScalerUnit string `json:"scalerUnit,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(results))
	okCount := 0
	for _, r := range results {
		if r.OK {
			okCount++
		}
		rows = append(rows, row{
			OBIS: r.OBIS, PointAddr: fmt.Sprintf("%s:%d:%d", r.OBIS, r.Class, 2),
			Name: r.Name, Class: r.Class, ClassName: dl.ClassName(r.Class),
			OK: r.OK, Value: r.Text, ScalerUnit: r.ScalerUnit, Error: r.Err,
		})
	}
	if asJSON {
		emitJSON(map[string]any{
			"serverAddress": server, "transport": transport,
			"rows": rows, "count": len(rows), "ok": okCount,
		})
		return
	}
	fmt.Printf("逻辑设备 %d  传输 %s  常见 OBIS 试读：%d/%d 可用\n\n",
		server, transport, okCount, len(rows))
	if raw {
		printRow([]int{20, 26, 14, 20, 18},
			"点表地址（可粘贴）", "含义", "类", "值", "scaler_unit")
		for _, r := range rows {
			v := r.Value
			if !r.OK {
				v = "✗ " + firstLine(r.Error)
			}
			printRow([]int{20, 26, 14, 20, 18},
				r.PointAddr, r.Name, r.ClassName, v, r.ScalerUnit)
		}
		return
	}
	printRow([]int{20, 26, 20}, "点表地址（可粘贴）", "含义", "值")
	for _, r := range rows {
		v := r.Value
		if !r.OK {
			v = "✗ " + firstLine(r.Error)
		}
		printRow([]int{20, 26, 20}, r.PointAddr, r.Name, v)
	}
	fmt.Println()
	fmt.Println("提示：把「点表地址」那一列直接抄进点表即可（已含类号与属性号）。")
	fmt.Println("      标 ✗ 的条目说明这台表没有该对象，或类号与内置表不一致。")
}

// firstLine 只取错误信息的首行：结果码已经很明确，后续的排查建议在
// 表格里会把列宽撑爆。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// dlmsRefs 解析逗号分隔的地址列表（给单测复用）。
func dlmsRefs(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
