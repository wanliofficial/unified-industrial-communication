package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	finscodec "uic/internal/codec/fins"
	hlcodec "uic/internal/codec/hostlink"
	"uic/internal/driver"
	hldrv "uic/internal/driver/hostlink"
	"uic/internal/model"
)

// uic hostlinkread 是一个极小的 Host Link FINS 主站自检命令。
//
// 与 mcread/finsread/mewtocolread 同理由：现场最缺的是「不装任何第三方软件
// 就能自证链路通」的手段。Host Link 的失败模式集中在几处，而且全都不报错、
// 只表现为「读不到」或「读到垃圾」：
//
//   - **串口参数不对。** 出厂是 9600-7-E-2（7 位数据位、偶校验、2 停止位）。
//     套用常见的 8-N-1 会「发得出去、收不回来」：PLC 收到的是位错乱的字节，
//     按格式错处理，一声不吭。
//   - **单元号不对。** PLC 听懂了但不是点名给自己，完全不回应。共享总线上
//     两台 PLC 单元号相同时两台都会回，主站会把 A 的数记到 B 头上。
//   - **端口设成了无协议或 C 模式。** 前者完全不理；后者回的是 `@00RR…`
//     这种字母头码的帧，FINS 解析器会直接报「不是 FINS 命令」。
//   - **响应等待时间写成两位十六进制。** 整帧错位，PLC 回 14（格式错）。
//   - **单次读超过 269 字。** 整帧超过上限，PLC 回 18（帧长错）。
//
// 这条命令把这几个变量全部摊开成命令行开关：先探活（读 DM0），再按给定
// 内存区做一次读写。参数默认值就是出厂值，因此「不加任何参数」最接近真机
// 开机状态。
//
// 边界：单次读写、单台设备、不做周期轮询；只做 FINS 帧（头码 FA），不做
// C 模式（RD/WD/RR/WR），也不做跨网长格式。
func hostlinkreadCmd(args []string) {
	fs := flag.NewFlagSet("hostlinkread", flag.ExitOnError)

	// 承载：串口是常态，串口服务器/以太网单元透传也很常见。
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/以太网单元）")
	addr := fs.String("addr", "127.0.0.1:2000", "PLC 地址 host[:port]（tcp）；端口省略时按 2000")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	// 这三个默认值是协议事实：出厂 9600、7 位、偶校验、2 停止位。
	baud := fs.Int("baud", 9600, "波特率（serial）；上位链接出厂 9600")
	dataBits := fs.Int("dataBits", 7, "数据位 7|8（serial）；整帧是可打印 ASCII，7 位即可")
	parity := fs.String("parity", "even", "校验：none | even | odd（serial）；出厂是偶校验")
	stopBits := fs.Int("stopBits", 2, "停止位 1|2（serial）；出厂是 2 位")

	// 单元号是两位十进制：31 号填 31，不是 0x1F。
	unit := fs.Int("unit", 0, "上位链接单元号（两位十进制，0..31）；必须与 PLC 设置一致")
	wait := fs.Int("wait", 0, "响应等待时间 0..15（一位十六进制，单位 10ms）；0 = 收到就答")

	// 点位：内存区 + 字地址 + 位号。
	area := fs.String("area", "dm", "内存区：cio/wr/hr/ar/dm/em/em0..em12（D/W/H/A 是别名）")
	offset := fs.Int("offset", 0, "字地址（十进制）")
	bit := fs.Int("bit", -1, "位号 0..15（bool 点位必填）")
	qty := fs.Int("qty", 1, "读取数量（字或位）")

	write := fs.String("write", "", "写入值：字区为十进制/0x 十六进制；布尔量为 on|off|1|0")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	probe := fs.Bool("probe", false, "只做探活（读 DM0），不按 -area 采集")
	showLimits := fs.Bool("limits", false, "只打印本协议的单次读写上限与推导，不发帧")
	fs.Parse(args)

	// -limits 是纯信息开关：帧长预算是最容易照抄错的地方（照抄 FINS/TCP 的
	// 999 会稳定收到帧长错），所以把它变成一条随时能查的命令。
	if *showLimits {
		printHostLinkLimits(*asJSON)
		return
	}

	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}
	if *unit < 0 || *unit > hlcodec.MaxUnit {
		fmt.Fprintf(os.Stderr, "-unit 必须在 0..%d 之间（两位十进制），得到 %d\n", hlcodec.MaxUnit, *unit)
		os.Exit(2)
	}
	if *wait < 0 || *wait > hlcodec.MaxWait {
		fmt.Fprintf(os.Stderr, "-wait 必须在 0..%d 之间（一位十六进制），得到 %d\n", hlcodec.MaxWait, *wait)
		os.Exit(2)
	}
	// 位号与数量在发帧前就拦住：位号越界会变成 FINS 结束码 0x1103（地址错），
	// 现场看到的是「PLC 说地址错」，而不是「你 -bit 填了 16」。位号 -1 是
	// 「不做位访问」的合法写法，其余负数同样是笔误。
	if *bit < -1 || *bit > 15 {
		fmt.Fprintf(os.Stderr, "-bit 必须在 0..15 之间（0 = LSB）或 -1（不做位访问），得到 %d\n", *bit)
		os.Exit(2)
	}
	if *qty < 1 {
		fmt.Fprintf(os.Stderr, "-qty 必须 >= 1，得到 %d\n", *qty)
		os.Exit(2)
	}

	ch := model.Channel{
		ID: "ch-hostlink", Protocol: "hostlink",
		Transport: model.Transport{
			Kind: *tr, Address: *addr, Port: *port,
			Baud: *baud, DataBits: *dataBits, Parity: *parity, StopBits: *stopBits,
		},
		Timeout: model.Duration(*timeout),
	}
	if ch.Transport.Kind == "serial" {
		ch.Transport.Address = ""
	}

	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		UnitID:   *unit,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels:   map[string]string{"options": fmt.Sprintf("wait=%d", *wait)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*3)
	defer cancel()

	c, err := hldrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：串口参数（-baud/-dataBits/-parity/-stopBits）→ 接线 → 单元号（-unit）")
		os.Exit(1)
	}
	defer c.Close()

	// 探活：读 DM0。DM 是最常见、也几乎一定存在的区，且不依赖 PLC 程序里
	// 的任何逻辑。这一步把「物理链路 + 单元号 + 端口模式」三件事一起验证。
	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "探活失败（读 DM0 无应答）: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：")
		fmt.Fprintln(os.Stderr, "    1. 串口参数：出厂 9600/7/偶/2；先确认不是 8-N-1")
		fmt.Fprintln(os.Stderr, "    2. 单元号：-unit 必须与 PLC 的「上位链接单元号」一致（0..31）")
		fmt.Fprintln(os.Stderr, "    3. 端口模式：必须是「上位链接（Host Link）」，不能是无协议或 C 模式")
		fmt.Fprintln(os.Stderr, "    4. 波特率：必须与 PLC 的上位链接端口设置一致")
		os.Exit(1)
	}

	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "target": describeTarget(ch.Transport, *addr),
				"unit": *unit, "wait": *wait, "link": "up", "probe": "read DM0",
			})
			return
		}
		fmt.Printf("链路正常: %s 单元号=%d（读 DM0 通过）\n",
			describeTarget(ch.Transport, *addr), *unit)
		return
	}

	areaLower := strings.ToLower(strings.TrimSpace(*area))
	if _, ok := finscodec.Lookup(areaLower); !ok {
		fmt.Fprintf(os.Stderr, "未知内存区 %q（支持：%s）\n", areaLower, strings.Join(finscodec.Areas(), ", "))
		os.Exit(2)
	}

	if *write != "" {
		runHostLinkWrite(ctx, c, areaLower, *offset, *bit, *qty, *write, *asJSON)
		return
	}
	runHostLinkRead(ctx, c, ch, areaLower, *offset, *bit, *qty, *asJSON, *unit)
}

func printHostLinkLimits(asJSON bool) {
	if asJSON {
		emitJSON(map[string]any{
			"maxCommandChars":  hlcodec.MaxCommandChars,
			"maxResponseChars": hlcodec.MaxResponseChars,
			"maxTextChars":     hlcodec.MaxTextChars,
			"maxDataChars":     hlcodec.MaxDataChars,
			"maxFrame":         hlcodec.MaxFrame,
			"readWords":        hlcodec.MaxReadWords,
			"writeWords":       hlcodec.MaxWriteWords,
			"readBits":         hlcodec.MaxReadBits,
			"writeBits":        hlcodec.MaxWriteBits,
		})
		return
	}
	fmt.Println("Host Link（头码 FA）单帧字符预算：")
	fmt.Printf("  命令 1114 字符、应答 1115 字符（跨网长格式；直连短格式少 12 字符）\n")
	fmt.Printf("  请求正文 %d 字符 = %d 字节；应答数据 %d 字符\n",
		hlcodec.MaxTextChars, hlcodec.MaxTextChars/2, hlcodec.MaxDataChars)
	fmt.Println("由此推导的单次上限（不是照抄 FINS/TCP 的 999）：")
	fmt.Printf("  读 %d 字 / %d 位；写 %d 字 / %d 位\n",
		hlcodec.MaxReadWords, hlcodec.MaxReadBits, hlcodec.MaxWriteWords, hlcodec.MaxWriteBits)
	fmt.Println("  读受应答帧长约束、写受命令帧长约束，所以写比读少 2 字")
	fmt.Println("FINS 命令不能拆成多帧发送：超限请分多次读，或把点位拆细")
}

func runHostLinkRead(ctx context.Context, c driver.Conn, ch model.Channel, area string, offset, bit, qty int, asJSON bool, unit int) {
	span := driver.ReadSpan{ID: "read"}
	if bit >= 0 {
		span.Points = append(span.Points, driver.PointRef{
			ID:       fmt.Sprintf("%s%d.%d", area, offset, bit),
			Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Bit: bit, Length: 1},
			DataType: model.TypeBool,
		})
	} else {
		for i := 0; i < qty; i++ {
			span.Points = append(span.Points, driver.PointRef{
				ID:       fmt.Sprintf("%s%d", area, offset+i),
				Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset + i, Length: 1, Bit: -1},
				DataType: model.TypeUint16,
			})
		}
	}

	samples, err := c.Read(ctx, []driver.ReadSpan{span})
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		rows := make([]map[string]any, 0, len(samples))
		for _, s := range samples {
			row := map[string]any{"point": s.PointID, "quality": string(s.Quality)}
			if s.Err != nil {
				row["error"] = s.Err.Error()
			} else {
				row["value"] = s.Value.String()
			}
			rows = append(rows, row)
		}
		emitJSON(map[string]any{
			"transport": ch.Transport.Kind, "target": describeTarget(ch.Transport, ch.Transport.Address),
			"unit": unit, "area": area, "rows": rows,
		})
		return
	}

	fmt.Printf("从站 %s 单元号=%d 区=%s\n", describeTarget(ch.Transport, ch.Transport.Address), unit, area)
	for _, s := range samples {
		if s.Err != nil {
			fmt.Printf("%s 错误[%s]: %v\n", s.PointID, s.Quality, s.Err)
			continue
		}
		fmt.Printf("%s = %s (%s)\n", s.PointID, s.Value.String(), s.Quality)
	}
}

// hostLinkWriteRequest 把命令行开关翻译成一条写请求。
//
// 抽成纯函数是为了能单测：这条判断一旦错了，错法是「把用户没点名的字或位一
// 起改掉」，而不是报错——只能在回环里跑一遍才看得出来。
//
// 两条判定：
//
//  1. **给了 -bit 就按位写，与区有没有原生位访问码无关。** CIO/WR/HR/AR/DM
//     有原生位写命令（原子、不碰邻位），EM 当前区没有，驱动退化
//     「读整字 → 改位 → 写整字」。这个分叉属于驱动，CLI 不该替它决定：
//     早先用 `bit >= 0 && a.BitCapable()` 判断，`-area em -bit 3 -write on`
//     就掉进字写分支——值是 `on` 按数字解析直接报错；换成 `-write 1` 更糟，
//     它真的发出整字写，把 em5 其余 15 位全清成 0。
//
//  2. **字写一次只写一个字。** 把 -qty 当写入宽度会让后面的字全部补零
//     （悄悄清邻字），而且宽度变了还会切换多字字节序（8 字节走 DCBA），
//     连本字的值都变形：`-qty 4 -write 5` 实测写成 dm10=0x0500、
//     dm11..13=0x0000。多字写入应当走点表（那里有类型与宽度）。
func hostLinkWriteRequest(area string, offset, bit, qty int, value string) (driver.WriteRequest, error) {
	if bit >= 0 {
		v, err := parseOnOff(value)
		if err != nil {
			return driver.WriteRequest{}, fmt.Errorf("布尔量写入值必须是 on|off|1|0: %w", err)
		}
		return driver.WriteRequest{
			PointID:  fmt.Sprintf("%s%d.%d", area, offset, bit),
			Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Bit: bit, Length: 1},
			DataType: model.TypeBool,
			Value:    model.BoolValue(v),
		}, nil
	}
	if qty != 1 {
		return driver.WriteRequest{}, fmt.Errorf(
			"字写一次只写一个字：-qty 必须为 1（位写忽略 -qty），得到 %d", qty)
	}
	n, err := parseHostLinkNumber(value)
	if err != nil {
		return driver.WriteRequest{}, err
	}
	return driver.WriteRequest{
		PointID:  fmt.Sprintf("%s%d", area, offset),
		Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Bit: -1, Length: 1},
		DataType: model.TypeUint16,
		Value:    model.NumValue(model.TypeUint16, n),
	}, nil
}

func runHostLinkWrite(ctx context.Context, c driver.Conn, area string, offset, bit, qty int, value string, asJSON bool) {
	req, err := hostLinkWriteRequest(area, offset, bit, qty, value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	res, err := c.Write(ctx, []driver.WriteRequest{req})
	ok := err == nil && len(res) == 1 && res[0].OK
	if asJSON {
		out := map[string]any{"point": req.PointID, "area": area, "value": value, "ok": ok}
		if err != nil {
			out["error"] = err.Error()
		}
		if len(res) == 1 {
			out["quality"] = string(res[0].Quality)
		}
		emitJSON(out)
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		if len(res) == 1 {
			fmt.Fprintf(os.Stderr, "  质量码: %s\n", res[0].Quality)
		}
		os.Exit(1)
	}
	fmt.Printf("写入 %s = %s 完成\n", req.PointID, value)
}

func parseHostLinkNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		n, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil {
			return 0, fmt.Errorf("非法十六进制 %q: %w", s, err)
		}
		return float64(n), nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("非法数字 %q: %w", s, err)
	}
	return n, nil
}
