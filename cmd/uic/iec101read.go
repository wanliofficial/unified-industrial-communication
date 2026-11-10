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

	iec60870 "uic/internal/codec/iec60870"
	"uic/internal/driver"
	iec101drv "uic/internal/driver/iec101"
	"uic/internal/model"
)

// uic iec101read 是一个极小的串行远动主站自检命令（FT1.2 非平衡轮询）。
//
// 与 dlt645read/cjt188read/iec104read 同理由：现场最缺的是「不装任何第三方
// 软件就能自证链路通」的手段。而串行远动这块尤其需要，因为它的失败模式
// 几乎全部表现为「没有应答」：
//
//   - 串口参数（波特率/校验位/停止位）与装置不一致；
//   - 链路地址配错：装置听懂了但不回答，主站只能看到超时；
//   - 公共地址 / 信息对象地址宽度配错：帧结构对了，ASDU 解出来是垃圾；
//   - 装置只支持第 1 级事件、不支持总召唤，或者反过来；
//   - 遥测在总召唤里，电能量必须单独召唤——只做站召唤时电度点位永远空。
//
// 这条命令把这几件事摊开：先复位链路（证明物理层与链路层通），再请求
// 链路状态，然后做一次完整采集（总召唤 + 电能量召唤 + 两级轮询），最后
// 把装置实际送过的信息对象按 IOA 列出来。检查点、超时、帧数都打印出来，
// 一眼能看出「卡在哪一层」。
//
// 它回答的是「这台装置实际送哪些 IOA、用哪种类型、带不带时标」，而不是
// 「我配置的点现在是什么值」。后者用网关的 /api/v1/points 看，那里才有
// 点位映射、报警与历史。这里刻意不依赖点表：初次接一台陌生装置时，点表
// 本身就是要靠这条命令做出来的。
//
// 边界：一次附着 + 一次（可选两次）轮询 + 汇总一屏，支持下发一条命令；
// 不做周期轮询、不做多设备、不做平衡式（从站主动发送）、不做带时标命令、
// 不做文件传输与时钟同步的写路径。
func iec101readCmd(args []string) {
	fs := flag.NewFlagSet("iec101read", flag.ExitOnError)

	// 承载：串口是常态，自检时最常用的是只给 -addr 指向串口服务器。
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "从站地址 host[:port]（tcp），端口省略时按 4001")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 9600, "波特率（serial）；远动常用 9600/19200，也有 1200/2400 的老装置")
	dataBits := fs.Int("dataBits", 8, "数据位 5..8（serial）")
	parity := fs.String("parity", "even", "校验：none | even | odd（serial）；FT1.2 标准要求 even")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）")

	// 链路层与 ASDU 剖面。这些值没有「标准默认」，只能按装置手册配；
	// 全部显示出来，是为了让「我这套参数」与「装置手册那套」能逐项对照。
	linkAddr := fs.Int("link-addr", 1, "链路地址（FT1.2 地址域）；同一条总线上每台从站不同，1..65535")
	linkSize := fs.Int("link-size", 1, "链路地址宽度 0..2 字节（0 表示不带地址）")
	ca := fs.Int("ca", 1, "公共地址（CA），1..65535")
	caSize := fs.Int("ca-size", 1, "公共地址宽度 1|2 字节")
	ioaSize := fs.Int("ioa-size", 2, "信息对象地址宽度 1|2|3 字节（决定 IOA 上限与点表可用范围）")
	orgSize := fs.Int("org-size", 0, "源发地址宽度 0|1 字节")

	interrogate := fs.Duration("interrogate", 5*time.Minute, "总召唤周期；首次采集必定召唤一次，0 表示只召唤一次")
	class2 := fs.Bool("class2", true, "每轮采集是否顺带请求第 2 级（周期/背景数据）")
	wait := fs.Duration("wait", 2*time.Second, "首轮采集后再等多久做第二轮（用于观察自发上送的事件）")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")

	// 过滤与输出。
	only := fs.String("ioa", "", "只显示这些 IOA，逗号分隔（默认为空表示全部）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "额外显示传送原因、原始品质位、命中点位与到达时刻")
	probe := fs.Bool("probe", false, "只做「复位链路 + 请求链路状态」，不召唤")

	// 命令下发。
	cmdArea := fs.String("area", "", "下发命令时的对象类别：single-command | double-command | "+
		"setpoint-float | setpoint-scaled | setpoint-normalized")
	cmdIOA := fs.Int("cmd-ioa", 0, "下发命令的信息对象地址（IOA）")
	write := fs.String("write", "", "下发命令的值：单/双点写 on|off|1|2；设定值写数字")
	fs.Parse(args)

	// 传输方式的默认值取决于「用户到底给了哪一组参数」。dlt645read 已经
	// 有同样的判断函数，直接复用，避免两条自检命令对同一组参数给出不同
	// 解释——那种差异会让人以为是驱动的问题。
	*tr = inferTransport(func(name string) bool { return flagSet(fs, name) }, *tr)
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}

	if *linkSize < 0 || *linkSize > 2 {
		fmt.Fprintln(os.Stderr, "-link-size 必须在 0..2 之间")
		os.Exit(2)
	}
	if *linkAddr < 0 || *linkAddr > 65535 {
		fmt.Fprintln(os.Stderr, "-link-addr 必须在 0..65535 之间")
		os.Exit(2)
	}
	if *linkSize == 0 && *linkAddr != 0 {
		fmt.Fprintln(os.Stderr, "-link-size 0 表示不带链路地址，此时 -link-addr 必须为 0")
		os.Exit(2)
	}
	if *linkSize == 1 && *linkAddr > 0xFF {
		fmt.Fprintln(os.Stderr, "-link-size 1 时 -link-addr 不能超过 255")
		os.Exit(2)
	}
	if *ca < 1 || *ca > 65535 {
		fmt.Fprintf(os.Stderr, "-ca 必须在 1..65535 之间，得到 %d\n", *ca)
		os.Exit(2)
	}
	if *caSize != 1 && *caSize != 2 {
		fmt.Fprintln(os.Stderr, "-ca-size 必须是 1 或 2")
		os.Exit(2)
	}
	if *caSize == 1 && *ca > 0xFF {
		fmt.Fprintf(os.Stderr, "-ca %d 超出 1 字节公共地址范围（-ca-size 1）\n", *ca)
		os.Exit(2)
	}
	if *ioaSize < 1 || *ioaSize > 3 {
		fmt.Fprintln(os.Stderr, "-ioa-size 必须是 1、2 或 3")
		os.Exit(2)
	}
	maxIOA := map[int]int{1: 0xFF, 2: 0xFFFF, 3: 0xFFFFFF}[*ioaSize]
	if *cmdIOA > maxIOA {
		fmt.Fprintf(os.Stderr, "-cmd-ioa %d 超出 -ioa-size %d 的地址空间（上限 %d）\n", *cmdIOA, *ioaSize, maxIOA)
		os.Exit(2)
	}

	labels := map[string]string{
		"linkAddress":       strconv.Itoa(*linkAddr),
		"linkAddressSize":   strconv.Itoa(*linkSize),
		"commonAddress":     strconv.Itoa(*ca),
		"commonAddressSize": strconv.Itoa(*caSize),
		"ioaSize":           strconv.Itoa(*ioaSize),
		"orgSize":           strconv.Itoa(*orgSize),
		"interrogate":       interrogate.String(),
		"class2":            strconv.FormatBool(*class2),
	}

	ch := model.Channel{
		ID: "ch-iec101", Protocol: "iec101",
		Transport: model.Transport{
			Kind: *tr, Address: *addr, Port: *port,
			Baud: *baud, DataBits: *dataBits, Parity: *parity, StopBits: *stopBits,
		},
		Timeout: model.Duration(*timeout),
	}
	if ch.Transport.Kind == "serial" {
		ch.Transport.Address = ""
	}

	// 探针点位：驱动要求至少一个可用点位才肯开口，而诊断列的是驱动旁路
	// 记录的上送对象，与点表无关。选地址空间的上界而不是 1，避免与真实
	// 对象撞车（撞上了也只会多一行值，不会影响诊断）。
	probeIOA := maxIOA
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels:   labels,
		Points: []model.Point{{
			ID: "probe", Enabled: true, DataType: model.TypeFloat64,
			Address: model.Address{
				Kind: model.AddressRegister, Area: iec101drv.AreaIOA,
				Offset: probeIOA, Bit: -1,
			},
		}},
	}

	// Open 只做链路复位；总召唤由第一次 Read 触发。给足预算：总召唤可能
	// 要取几百条报文，一条一轮往返，几百毫秒到几秒都正常。
	ctx, cancel := context.WithTimeout(context.Background(), *timeout*4+*wait*2)
	defer cancel()

	c, err := iec101drv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败（链路复位未通过）: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：串口参数（-baud/-parity）→ 接线 A/B → 链路地址（-link-addr/-link-size）")
		os.Exit(1)
	}
	defer c.Close()

	// 复位链路只证明「地址对、帧结构对」；链路状态请求再证明一次装置
	// 确实在按协议回答，而不是碰巧回了一个字节。
	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（请求链路状态）失败: %v\n", err)
		os.Exit(1)
	}

	if *write != "" || *cmdArea != "" || *cmdIOA != 0 {
		runIEC101Write(ctx, c, *cmdArea, *cmdIOA, *write, *asJSON)
		return
	}

	if *probe {
		if *asJSON {
			emitJSON(map[string]any{
				"transport": ch.Transport.Kind, "linkAddress": *linkAddr,
				"commonAddress": *ca, "link": "up", "probe": "link-status",
			})
			return
		}
		fmt.Printf("链路正常: %s 链路地址=%d CA=%d（复位链路与链路状态均通过）\n",
			describeTarget(ch.Transport, *addr), *linkAddr, *ca)
		return
	}

	// 第一轮：总召唤 + 电能量召唤 + 第 1/2 级轮询。这一轮的成功与否就是
	// 「装置有没有数据」的答案，所以把错误留着而不是立刻退出——已经取到
	// 的对象仍然值得打出来。
	readErr := readOnce(ctx, c, probeIOA)

	obsAPI, ok := c.(interface {
		Observed() []iec101drv.Observed
		Dropped() uint64
		BadFrames() uint64
		Frames() (uint64, uint64)
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供上送对象快照")
		os.Exit(1)
	}
	first := obsAPI.Observed()

	// 第二轮：串行总线上事件不会自己到，必须再点名一次才会被取回来。
	// 这一轮既展示「等待期间有没有自发的变化」，也顺带验证第 1 级轮询
	// 在没有总召唤的情况下能独立工作。
	var second []iec101drv.Observed
	if *wait > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(*wait):
		}
		if err := readOnce(ctx, c, probeIOA); err != nil && readErr == nil {
			readErr = err
		}
		second = obsAPI.Observed()
	}

	if len(first) == 0 && readErr != nil {
		fmt.Fprintf(os.Stderr, "采集失败: %v\n", readErr)
		fmt.Fprintln(os.Stderr, "  排查顺序：公共地址（-ca/-ca-size）→ IOA 宽度（-ioa-size）→ "+
			"装置是否支持总召唤（试 -interrogate 0 后单看第 1 级）")
		os.Exit(1)
	}

	rows := make([]iec101Row, 0, len(second))
	updated := 0
	prev := make(map[uint32]time.Time, len(first))
	for _, o := range first {
		prev[o.IOA] = o.At
	}
	for _, o := range second {
		if at, ok := prev[o.IOA]; !ok || o.At.After(at) {
			updated++
		}
	}
	for _, o := range second {
		rows = append(rows, iec101Row{
			IOA:     o.IOA,
			Type:    iec60870.TypeName(o.TypeID),
			TypeID:  o.TypeID,
			Cause:   iec60870.CauseName(o.Cause),
			Quality: qualityText(o.Object.Quality),
			RawQual: o.Object.Quality.String(),
			Value:   formatObject(o.TypeID, o.Object),
			Time:    formatStamp(o.Object.Time),
			Points:  o.Points,
			At:      o.At.Format("15:04:05.000"),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].IOA < rows[j].IOA })
	total := len(rows)
	if *only != "" {
		rows = filterIEC101(rows, *only)
	}

	out, in := obsAPI.Frames()

	if *asJSON {
		payload := map[string]any{
			"transport": ch.Transport.Kind, "address": describeTarget(ch.Transport, *addr),
			"linkAddress": *linkAddr, "linkSize": *linkSize,
			"commonAddress": *ca, "caSize": *caSize, "ioaSize": *ioaSize, "orgSize": *orgSize,
			"link": "up", "observed": len(rows), "total": total,
			"updated": updated, "firstPoll": len(first), "secondPoll": len(second),
			"framesOut": out, "framesIn": in,
			"dropped": obsAPI.Dropped(), "badFrames": obsAPI.BadFrames(),
			"rows": rows,
		}
		if readErr != nil {
			payload["error"] = readErr.Error()
		}
		emitJSON(payload)
		return
	}

	fmt.Printf("从站 %s  链路地址=%d CA=%d  IOA/CA 宽度=%d/%d\n",
		describeTarget(ch.Transport, *addr), *linkAddr, *ca, *ioaSize, *caSize)
	fmt.Printf("装置上送 %d 个信息对象", len(rows))
	if total != len(rows) {
		fmt.Printf("（共 %d 个，已按 -ioa 过滤）", total)
	}
	fmt.Printf("；本轮轮询新增/更新 %d 个（首轮 %d，末轮 %d）\n", updated, len(first), len(second))
	fmt.Printf("帧计数：发出 %d / 收到 %d", out, in)
	if bad := obsAPI.BadFrames(); bad > 0 {
		fmt.Printf("；解析失败或公共地址不符 %d 帧", bad)
	}
	if d := obsAPI.Dropped(); d > 0 {
		fmt.Printf("；%d 个 IOA 超出诊断表上限被丢弃", d)
	}
	fmt.Println()
	if readErr != nil {
		// 已经取到的对象照样打出来，但错误必须显式出现：它通常意味着
		// 「这一轮有设备没答」，而不是「一切正常」。
		fmt.Printf("采集告警: %v\n", readErr)
	}
	fmt.Println()
	if len(rows) == 0 {
		fmt.Println("装置没有上送任何信息对象。可能原因：点表外的 IOA 未被召唤、装置只支持事件不支持总召唤、")
		fmt.Println("或者公共地址/IOA 宽度与装置不一致（帧能通但 ASDU 解不出来）。")
		return
	}

	// 手工对齐而不是用 tabwriter：中文是双宽字符，tabwriter 按字节算
	// 宽度，含中文的表头与数据行会参差不齐——这种输出恰好是给人看的。
	if *raw {
		printRow([]int{9, 24, 10, 20}, "IOA", "类型", "质量", "值", "传送原因", "时标")
		for _, r := range rows {
			printRow([]int{9, 24, 10, 20},
				strconv.FormatUint(uint64(r.IOA), 10),
				r.Type, r.Quality, r.Value, r.Cause, r.Time)
		}
		return
	}
	printRow([]int{9, 24, 10, 20}, "IOA", "类型", "质量", "值", "时标")
	for _, r := range rows {
		printRow([]int{9, 24, 10, 20},
			strconv.FormatUint(uint64(r.IOA), 10),
			r.Type, r.Quality, r.Value, r.Time)
	}
}

// readOnce 做一轮采集，并忽略探针点位自身的「没有数据」。
//
// 探针点位的值永远不该出现；它若出现，说明装置的地址空间与配置不一致，
// 但那属于诊断信息（会出现在 Observed 里），不该让整轮采集报错。
func readOnce(ctx context.Context, c driver.Conn, probeIOA int) error {
	ref := driver.PointRef{
		ID: "probe",
		Address: model.Address{
			Kind: model.AddressRegister, Area: iec101drv.AreaIOA,
			Offset: probeIOA, Bit: -1, Length: 0,
		},
		DataType: model.TypeFloat64,
	}
	// 探针点位的样本错误不影响这一轮的结论：真正的结论来自 Observed。
	_, err := c.Read(ctx, []driver.ReadSpan{{ID: "probe", Points: []driver.PointRef{ref}}})
	return err
}

// describeTarget 给出一句人能读的目标描述。
func describeTarget(t model.Transport, addr string) string {
	if t.Kind == "serial" {
		return fmt.Sprintf("%s @ %d,%d%s%d", t.Port, t.Baud, t.DataBits, parityLetter(t.Parity), t.StopBits)
	}
	return addr
}

func parityLetter(p string) string {
	switch strings.ToLower(p) {
	case "even":
		return "E"
	case "odd":
		return "O"
	}
	return "N"
}

// iec101Row 是自检输出的一行。
type iec101Row struct {
	IOA     uint32   `json:"ioa"`
	Type    string   `json:"type"`
	TypeID  byte     `json:"typeId"`
	Cause   string   `json:"cause,omitempty"`
	Quality string   `json:"quality"`
	RawQual string   `json:"rawQuality,omitempty"`
	Value   string   `json:"value"`
	Time    string   `json:"time,omitempty"`
	Points  []string `json:"points,omitempty"`
	At      string   `json:"at,omitempty"`
}

func filterIEC101(rows []iec101Row, spec string) []iec101Row {
	if strings.TrimSpace(spec) == "" {
		return rows
	}
	want := map[uint32]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "-ioa: %q 不是合法的 IOA\n", part)
			os.Exit(2)
		}
		want[uint32(n)] = true
	}
	out := make([]iec101Row, 0, len(rows))
	for _, r := range rows {
		if want[r.IOA] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IOA < out[j].IOA })
	return out
}

// runIEC101Write 下发一条命令并打印确认结果。
//
// 串行总线上的写入有一个与以太网不同的地方：命令必须等**应用层**激活
// 确认。链路层的 E5 只说明帧到了，把那个当成成功是这类驱动最常见的一处
// 谎报。驱动已经等到了应用层回执才返回，这里只负责把结果说清楚。
func runIEC101Write(ctx context.Context, conn driver.Conn, area string, ioa int, value string, asJSON bool) {
	if area == "" {
		fmt.Fprintln(os.Stderr, "下发命令必须给 -area（对象类别）")
		os.Exit(2)
	}
	if ioa <= 0 {
		fmt.Fprintln(os.Stderr, "-cmd-ioa 必须 >= 1")
		os.Exit(2)
	}
	if value == "" {
		fmt.Fprintln(os.Stderr, "下发命令必须给 -write（要写的值）")
		os.Exit(2)
	}
	req, err := buildIEC101Write(area, ioa, value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	res, werr := conn.Write(ctx, []driver.WriteRequest{req})
	if asJSON {
		out := map[string]any{
			"point": req.PointID, "area": strings.ToLower(area), "ioa": ioa, "value": value,
			"ok": len(res) == 1 && res[0].OK,
		}
		if werr != nil {
			out["error"] = werr.Error()
		}
		if len(res) == 1 {
			out["quality"] = string(res[0].Quality)
		}
		emitJSON(out)
		if werr != nil {
			os.Exit(1)
		}
		return
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "命令失败: %v\n", werr)
		if len(res) == 1 {
			fmt.Fprintf(os.Stderr, "  质量码: %s\n", res[0].Quality)
		}
		os.Exit(1)
	}
	fmt.Printf("命令已确认执行（应用层激活确认）: %s IOA=%d value=%s\n", strings.ToLower(area), ioa, value)
}

// buildIEC101Write 把命令行参数翻译成一条写入请求。
//
// 校验放在这里而不是驱动里，是因为命令行能给出更具体的提示；驱动侧的
// 校验仍然保留，它服务的是运行时（那里拿不到命令行上下文）。
func buildIEC101Write(area string, ioa int, value string) (driver.WriteRequest, error) {
	area = strings.ToLower(strings.TrimSpace(area))
	addr := model.Address{
		Kind: model.AddressRegister, Area: area, Offset: ioa, Bit: -1,
	}
	req := driver.WriteRequest{
		PointID: fmt.Sprintf("%s:%d", area, ioa), Address: addr,
	}

	switch area {
	case iec101drv.AreaSingleCommand:
		v, err := parseOnOff(value)
		if err != nil {
			return req, fmt.Errorf("单点命令: %w", err)
		}
		req.DataType = model.TypeBool
		req.Value = model.BoolValue(v)
	case iec101drv.AreaDoubleCommand:
		// 双点命令只接受 1（分）/2（合）；on/off 是给人用的别名。
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "on", "2", "合":
			req.DataType = model.TypeUint16
			req.Value = model.NumValue(model.TypeUint16, 2)
		case "off", "1", "分":
			req.DataType = model.TypeUint16
			req.Value = model.NumValue(model.TypeUint16, 1)
		default:
			return req, fmt.Errorf("双点命令取值 %q 非法（on|off|1|2）", value)
		}
	case iec101drv.AreaSetpointFloat,
		iec101drv.AreaSetpointScaled,
		iec101drv.AreaSetpointNormalized:
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return req, fmt.Errorf("设定值 %q 不是数字", value)
		}
		req.DataType = model.TypeFloat32
		req.Value = model.NumValue(model.TypeFloat32, f)
	default:
		return req, fmt.Errorf("-area %q 未知（single-command | double-command | "+
			"setpoint-float | setpoint-scaled | setpoint-normalized）", area)
	}
	return req, nil
}
