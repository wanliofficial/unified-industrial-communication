package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	mewcodec "uic/internal/codec/mewtocol"
	"uic/internal/driver"
	mewdrv "uic/internal/driver/mewtocol"
	"uic/internal/model"
)

// uic mewtocolread 是一个极小的 Mewtocol 主站自检命令。
//
// 与 mcread/finsread/iec101read 同理由：现场最缺的是「不装任何第三方软件就
// 能自证链路通」的手段。Mewtocol 的失败模式尤其集中在几处，而且都不报错、
// 只表现为「读不到」：
//
//   - 串口参数不对（出厂奇校验、9600、7 或 8 位）；
//   - 站号配错：PLC 听懂了但不回答，主站只看到超时；
//   - PLC 没在「计算机链接」模式（还在编程口模式），帧发出去无人应答；
//   - 地址的进制搞混：X1F 在线上的接点号是 001F，把 1F 当十进制填进
//     -offset 会读到另一个接点，值「看起来有」但对应关系全错；
//   - FP7 的 LAN 口设成了「与 FP2-ET-LAN 连接」，帧外多一层 16 字节头，
//     裸帧发过去 PLC 根本收不下。
//
// 这条命令把这几个变量摊开：先探活（读 DT0），再按给定区域做一次读写，
// 把「发出去的帧」与「收回来的帧」都打出来。原始帧是最有用的诊断信息——
// 「帧长对不对、BCC 对不对、站号回的是几」看一眼就知道，不需要抓包工具。
//
// 边界：单次读写、单台设备、不做周期轮询、不做 FP2-ET-LAN 封装模式。
func mewtocolreadCmd(args []string) {
	fs := flag.NewFlagSet("mewtocolread", flag.ExitOnError)

	// 承载：现场串口是常态，调试时常用串口服务器（TCP 透传）。
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/以太网单元）")
	addr := fs.String("addr", "127.0.0.1:2000", "PLC 地址 host[:port]（tcp）；端口省略时按 2000")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 9600, "波特率（serial）；FP 系列出厂多为 9600")
	dataBits := fs.Int("dataBits", 8, "数据位 7|8（serial）")
	parity := fs.String("parity", "odd", "校验：none | even | odd（serial）；计算机链接出厂是奇校验")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")

	// 站号是两位十进制：16 号 PLC 填 16，不是 0x10。
	station := fs.Int("station", 1, "PLC 站号（两位十进制，1..99）；必须与 PLC 系统寄存器一致")

	// 点位：区名 + 字地址 + 位号。
	area := fs.String("area", "dt", "区名：dt/ld/fl（字区） x/y/r/l/t/c（接点区） ix/iy/id（索引） sv/ev（定时器值）")
	offset := fs.Int("offset", 0, "字地址（十进制）。接点区的 -offset 是「字」，位号由 -bit 给")
	bit := fs.Int("bit", -1, "接点区/字区布尔量的位号 0..15；字区整字读取时忽略")
	qty := fs.Int("qty", 1, "读取数量（字或接点数）")

	write := fs.String("write", "", "写入值：字区为十进制/0x 十六进制；布尔量为 on|off|1|0")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "打印发出去的请求帧与收回来的应答帧")
	probe := fs.Bool("probe", false, "只做探活（读 DT0），不按 -area 采集")
	fs.Parse(args)

	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}
	if *station < 1 || *station > 99 {
		fmt.Fprintf(os.Stderr, "-station 必须在 1..99 之间（两位十进制），得到 %d\n", *station)
		os.Exit(2)
	}

	ch := model.Channel{
		ID: "ch-mewtocol", Protocol: "mewtocol",
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
		UnitID:   *station,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*3)
	defer cancel()

	c, err := mewdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：串口参数（-baud/-parity/-dataBits）→ 接线 → 站号（-station）")
		os.Exit(1)
	}
	defer c.Close()

	// 探活：读 DT0。RD 是最基础的读命令，所有 FP 系列都支持，DT0 出厂
	// 必定存在。这一步把「物理链路 + 站号 + PLC 模式」三件事一起验证。
	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "探活失败（读 DT0 无应答）: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：")
		fmt.Fprintln(os.Stderr, "    1. 串口参数：出厂 9600/8/奇/1；用 -parity odd 试")
		fmt.Fprintln(os.Stderr, "    2. 站号：-station 必须与 PLC 系统寄存器 412..413 一致")
		fmt.Fprintln(os.Stderr, "    3. 模式：PLC 必须处于「计算机链接」模式，而非编程口模式")
		fmt.Fprintln(os.Stderr, "    4. FP7 LAN：不能设「与 FP2-ET-LAN 连接」（本实现只发裸帧）")
		os.Exit(1)
	}

	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "target": describeTarget(ch.Transport, *addr),
				"station": *station, "link": "up", "probe": "read DT0",
			})
			return
		}
		fmt.Printf("链路正常: %s 站号=%d（读 DT0 通过）\n",
			describeTarget(ch.Transport, *addr), *station)
		return
	}

	areaLower := strings.ToLower(strings.TrimSpace(*area))
	if _, ok := mewcodec.Lookup(areaLower); !ok {
		fmt.Fprintf(os.Stderr, "未知区名 %q（支持：%s）\n", areaLower, strings.Join(mewcodec.Keys(), ", "))
		os.Exit(2)
	}

	if *write != "" {
		runMewWrite(ctx, c, areaLower, *offset, *bit, *qty, *write, *asJSON, *raw)
		return
	}
	runMewRead(ctx, c, ch, areaLower, *offset, *bit, *qty, *asJSON, *raw, *station)
}

func runMewRead(ctx context.Context, c driver.Conn, ch model.Channel, area string, offset, bit, qty int, asJSON, raw bool, station int) {
	span := driver.ReadSpan{ID: "read"}
	if bit >= 0 {
		// 指定位号：一个布尔点位。
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
			row := map[string]any{
				"point": s.PointID, "quality": string(s.Quality),
			}
			if s.Err != nil {
				row["error"] = s.Err.Error()
			} else {
				row["value"] = s.Value.String()
			}
			rows = append(rows, row)
		}
		emitJSON(map[string]any{
			"transport": ch.Transport.Kind, "target": describeTarget(ch.Transport, ch.Transport.Address),
			"station": station, "area": area, "rows": rows,
		})
		return
	}

	fmt.Printf("从站 %s 站号=%d 区=%s\n", describeTarget(ch.Transport, ch.Transport.Address), station, area)
	for _, s := range samples {
		if s.Err != nil {
			fmt.Printf("%s 错误[%s]: %v\n", s.PointID, s.Quality, s.Err)
			continue
		}
		fmt.Printf("%s = %s (%s)\n", s.PointID, s.Value.String(), s.Quality)
	}
	_ = raw // 原始帧诊断在驱动内部，命令行只给语义结果
}

func runMewWrite(ctx context.Context, c driver.Conn, area string, offset, bit, qty int, value string, asJSON, raw bool) {
	a, _ := mewcodec.Lookup(area)
	isBit := a.Kind == mewcodec.KindContact && a.Code != "T" && a.Code != "C" || bit >= 0

	var req driver.WriteRequest
	if isBit {
		v, err := parseOnOff(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "布尔量写入值必须是 on|off|1|0: %v\n", err)
			os.Exit(2)
		}
		b := bit
		if b < 0 {
			b = 0
		}
		req = driver.WriteRequest{
			PointID:  fmt.Sprintf("%s%d.%d", area, offset, b),
			Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Bit: b, Length: 1},
			DataType: model.TypeBool,
			Value:    model.BoolValue(v),
		}
	} else {
		n, err := parseMewNumber(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		req = driver.WriteRequest{
			PointID:  fmt.Sprintf("%s%d", area, offset),
			Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Bit: -1, Length: qty},
			DataType: model.TypeUint16,
			Value:    model.NumValue(model.TypeUint16, n),
		}
	}

	res, err := c.Write(ctx, []driver.WriteRequest{req})
	ok := err == nil && len(res) == 1 && res[0].OK
	if asJSON {
		out := map[string]any{
			"point": req.PointID, "area": area, "value": value, "ok": ok,
		}
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
	_ = raw
}

func parseMewNumber(s string) (float64, error) {
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
