package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	dnp3codec "uic/internal/codec/dnp3"
	"uic/internal/driver"
	dnp3drv "uic/internal/driver/dnp3"
	"uic/internal/model"
)

// uic dnp3read 是一个极小的 DNP3 主站自检命令。
//
// 与 mbread/iec104read 同理由：现场最缺的是「不装任何第三方软件就能自证
// 链路通」的手段。DNP3 尤其需要——它是三层协议，`telnet 20000` 能连上
// 完全不代表链路复位能过，更不代表应用层的总召唤有数据回来。三层任何
// 一层错位都表现为「连上了但读不到」，只有把每一层单独打出来才能定位。
//
// 它回答的是「这台设备实际送哪些对象、用哪些组/变体、带不带品质位」，
// 而不是「我配置的点现在是什么值」。后者用网关的 /api/v1/points 看，
// 那里才有缩放、报警与历史。这里刻意不依赖点表：初次接一台陌生设备时，
// 点表本身就是要靠这条命令做出来的。
//
// 边界：一次附着 + 总召唤 + 汇总一屏，支持直接控制；不做事件持久化、
// 不做多设备、不做选择-执行、不做文件传输。
func dnp3readCmd(args []string) {
	fs := flag.NewFlagSet("dnp3read", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:20000", "从站地址 host[:port]，端口默认 20000")
	master := fs.Int("master", 1024, "主站链路地址，1..65519")
	unit := fs.Int("unit", 1, "从站链路地址，1..65519")
	wait := fs.Duration("wait", 2*time.Second, "总召唤后等待上送的时间")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	only := fs.String("index", "", "只显示这些索引，逗号分隔（默认为空表示全部）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "额外显示组/变体与原始品质位")
	probe := fs.Bool("probe", false, "只做链路复位 + TEST_LINK 往返，不召唤")

	var binWrites stringList
	var anaWrites stringList
	fs.Var(&binWrites, "binary", "直接控制二进制输出，格式 INDEX=on|off，可重复")
	fs.Var(&anaWrites, "analog", "直接控制模拟量输出，格式 INDEX=VALUE，可重复")
	fs.Parse(args)

	if *master < 1 || *master > 65519 {
		fmt.Fprintf(os.Stderr, "master: 必须在 1..65519 之间，得到 %d\n", *master)
		os.Exit(2)
	}
	if *unit < 1 || *unit > 65519 {
		fmt.Fprintf(os.Stderr, "unit: 必须在 1..65519 之间，得到 %d\n", *unit)
		os.Exit(2)
	}
	if *unit == *master {
		fmt.Fprintln(os.Stderr, "master 与 unit 不能相同：DNP3 要求链路上两个地址可区分")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		*timeout+*wait+15*time.Second)
	defer cancel()

	conn, err := openDNP3(ctx, *addr, *master, *unit, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// 链路复位与总召唤已经在 Open 里完成；这里再做一次 TEST_LINK，把
	// 「TCP 接受连接但链路层没起来」与「链路层正常」区分开。
	if err := conn.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（TEST_LINK）失败: %v\n", err)
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			emitJSON(map[string]any{"addr": *addr, "master": *master, "unit": *unit, "link": "up"})
			return
		}
		fmt.Printf("链路正常: %s 主站=%d 从站=%d（复位链路与 TEST_LINK 均通过）\n",
			*addr, *master, *unit)
		return
	}

	if len(binWrites) > 0 || len(anaWrites) > 0 {
		runDNP3Writes(ctx, conn, binWrites, anaWrites, *asJSON)
		return
	}

	// Open 里已经发过总召唤（类 0/1/2/3），这里只等首批事件与静态值到齐，
	// 不再重复召唤：短时间内重复召唤会被从站当成风暴忽略。
	time.Sleep(*wait)

	obs, ok := conn.(interface {
		Observed() []dnp3drv.Observed
		Dropped() uint64
		IIN() uint16
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供上送对象快照")
		os.Exit(1)
	}
	rows := make([]dnp3Row, 0, 32)
	for _, o := range obs.Observed() {
		rows = append(rows, dnp3Row{
			Area:      o.Area,
			Group:     o.Group,
			Variation: o.Variation,
			Index:     o.Index,
			Quality:   dnp3Quality(o.Flags, o.HasFlags, o.Group),
			RawFlags:  fmt.Sprintf("0x%02X", o.Flags),
			Value:     formatDNP3Value(o),
			At:        o.At.Format("15:04:05.000"),
		})
	}
	total := len(rows)
	if *only != "" {
		rows = filterDNP3Index(rows, *only)
	}

	if *asJSON {
		emitJSON(map[string]any{
			"addr": *addr, "master": *master, "unit": *unit, "link": "up",
			"observed": len(rows), "total": total,
			"dropped": obs.Dropped(), "iin": fmt.Sprintf("0x%04X", obs.IIN()),
			"rows": rows,
		})
		return
	}

	fmt.Printf("从站 %s  主站=%d 从站=%d\n", *addr, *master, *unit)
	fmt.Printf("设备上送 %d 个对象", len(rows))
	if total != len(rows) {
		fmt.Printf("（共 %d 个，已按 -index 过滤）", total)
	}
	fmt.Println()
	if d := obs.Dropped(); d > 0 {
		fmt.Printf("警告: 有 %d 个坏帧或超出诊断表上限的对象被丢弃\n", d)
	}
	if iin := obs.IIN(); iin != 0 {
		fmt.Printf("最近一条响应的 IIN = 0x%04X（%s）\n", iin, dnp3IINText(iin))
	}
	fmt.Println()

	if *raw {
		printRow([]int{20, 9, 8, 9, 10}, "类别", "组/变体", "索引", "质量", "值", "原始 flags")
		for _, r := range rows {
			printRow([]int{20, 9, 8, 9, 10},
				r.Area,
				fmt.Sprintf("%d/%d", r.Group, r.Variation),
				strconv.FormatUint(uint64(r.Index), 10),
				r.Quality, r.Value, r.RawFlags)
		}
		return
	}
	printRow([]int{20, 9, 8, 10}, "类别", "组/变体", "索引", "质量", "值")
	for _, r := range rows {
		printRow([]int{20, 9, 8, 10},
			r.Area,
			fmt.Sprintf("%d/%d", r.Group, r.Variation),
			strconv.FormatUint(uint64(r.Index), 10),
			r.Quality, r.Value)
	}
}

// dnp3Row 是自检输出的一行。
type dnp3Row struct {
	Area      string `json:"area"`
	Group     byte   `json:"group"`
	Variation byte   `json:"variation"`
	Index     uint32 `json:"index"`
	Quality   string `json:"quality"`
	RawFlags  string `json:"rawFlags,omitempty"`
	Value     string `json:"value"`
	At        string `json:"at,omitempty"`
}

// openDNP3 附着到从站。
//
// 点表只放一个探针点位：Open 里的总召唤会把整站对象记进诊断表，与点表
// 无关。探针选 binary-input[0]，是因为几乎不可能有从站连一个二进制输入
// 都没有；它只用于满足 buildPlans 的非空约束。
func openDNP3(ctx context.Context, addr string, master, unit int, timeout time.Duration) (driver.Conn, error) {
	dev := model.Device{
		ID: "probe", ChannelID: "ch", UnitID: unit, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Labels: map[string]string{
			"masterAddress": strconv.Itoa(master),
			// 诊断命令不需要高频事件轮询；把周期放大，避免输出被后续
			// 事件不断刷新而看不清初始快照。
			"eventPoll":     "5s",
			"integrityPoll": "10m",
		},
		Points: []model.Point{{
			ID: "probe", Enabled: true, DataType: model.TypeBool,
			Address: model.Address{
				Kind: model.AddressRegister, Area: dnp3drv.AreaBinaryInput,
				Offset: 0, Bit: -1,
			},
		}},
	}
	ch := model.Channel{
		ID: "ch", Protocol: "dnp3",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(timeout),
	}
	return dnp3drv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
}

func formatDNP3Value(o dnp3drv.Observed) string {
	switch v := o.Value.(type) {
	case bool:
		if v {
			return "on"
		}
		return "off"
	case uint64:
		if o.Area == dnp3drv.AreaDoubleBitInput {
			switch v {
			case 0:
				return "0（中间）"
			case 1:
				return "1（分）"
			case 2:
				return "2（合）"
			case 3:
				return "3（不确定）"
			}
		}
		return strconv.FormatUint(v, 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case dnp3codec.CROB:
		return fmt.Sprintf("status=%s", crobStatusText(v.Status))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func dnp3Quality(flags byte, hasFlags bool, group byte) string {
	if !hasFlags {
		return "good"
	}
	// 与驱动同一套规则：状态位不是品质位。
	f := flags
	switch group {
	case dnp3codec.GroupBinaryInput, dnp3codec.GroupBinaryEvent,
		dnp3codec.GroupBinaryOutput, dnp3codec.GroupBinaryOutputEvent:
		f &^= dnp3codec.FlagState
	case dnp3codec.GroupDoubleBitInput, dnp3codec.GroupDoubleBitEvent:
		f &^= 0xC0
	}
	switch {
	case f&dnp3codec.FlagOnline == 0 || f&dnp3codec.FlagCommLost != 0:
		return "bad"
	case f&(dnp3codec.FlagRestart|dnp3codec.FlagRemoteForced|dnp3codec.FlagLocalForced|
		dnp3codec.FlagChatter|dnp3codec.FlagRefErr) != 0:
		return "uncertain"
	}
	return "good"
}

func dnp3IINText(iin uint16) string {
	var parts []string
	for _, b := range []struct {
		bit  uint16
		name string
	}{
		{dnp3codec.IINAllStations, "all-stations"},
		{dnp3codec.IINClass1Events, "class-1-events"},
		{dnp3codec.IINClass2Events, "class-2-events"},
		{dnp3codec.IINClass3Events, "class-3-events"},
		{dnp3codec.IINNeedTime, "need-time"},
		{dnp3codec.IINLocalControl, "local-control"},
		{dnp3codec.IINDeviceTrouble, "device-trouble"},
		{dnp3codec.IINDeviceRestart, "device-restart"},
		{dnp3codec.IINFuncNotSupport, "func-not-supported"},
		{dnp3codec.IINObjectUnknown, "object-unknown"},
		{dnp3codec.IINParamError, "parameter-error"},
		{dnp3codec.IINEventOverflow, "event-overflow"},
		{dnp3codec.IINAlreadyExec, "already-executing"},
		{dnp3codec.IINConfigCorrupt, "config-corrupt"},
	} {
		if iin&b.bit != 0 {
			parts = append(parts, b.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func crobStatusText(status byte) string {
	switch status {
	case 0:
		return "success"
	case 1:
		return "timeout"
	case 2:
		return "no-select"
	case 3:
		return "format-error"
	case 4:
		return "not-supported"
	case 5:
		return "already-active"
	case 6:
		return "hardware-error"
	case 7:
		return "local-control"
	case 8:
		return "too-many-ops"
	case 9:
		return "not-authorized"
	}
	return fmt.Sprintf("0x%02X", status)
}

func filterDNP3Index(rows []dnp3Row, spec string) []dnp3Row {
	want := map[uint32]bool{}
	max := uint32(0)
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseUint(part, 0, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-index: 忽略非法值 %q\n", part)
			continue
		}
		want[uint32(n)] = true
		if uint32(n) > max {
			max = uint32(n)
		}
	}
	out := rows[:0]
	for _, r := range rows {
		if want[r.Index] {
			out = append(out, r)
		}
	}
	_ = max
	return out
}

// runDNP3Writes 下发直接控制并打印从站回显。
func runDNP3Writes(ctx context.Context, conn driver.Conn, bin, ana stringList, asJSON bool) {
	var reqs []driver.WriteRequest
	type label struct{ id string }
	var labels []label
	for _, spec := range bin {
		idx, val, err := parseDNP3Assignment(spec, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-binary %q: %v\n", spec, err)
			os.Exit(2)
		}
		b := strings.EqualFold(val, "on") || val == "1" || strings.EqualFold(val, "true")
		reqs = append(reqs, driver.WriteRequest{
			PointID: fmt.Sprintf("binary-output[%d]", idx), DataType: model.TypeBool,
			Address: model.Address{Kind: model.AddressRegister, Area: dnp3drv.AreaBinaryOutput, Offset: idx, Bit: -1},
			Value:   model.BoolValue(b),
		})
		labels = append(labels, label{id: fmt.Sprintf("binary[%d]=%s", idx, map[bool]string{true: "on", false: "off"}[b])})
	}
	for _, spec := range ana {
		idx, val, err := parseDNP3Assignment(spec, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-analog %q: %v\n", spec, err)
			os.Exit(2)
		}
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-analog %q: 不是数字\n", spec)
			os.Exit(2)
		}
		reqs = append(reqs, driver.WriteRequest{
			PointID: fmt.Sprintf("analog-output[%d]", idx), DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressRegister, Area: dnp3drv.AreaAnalogOutput, Offset: idx, Bit: -1},
			Value:   model.NumValue(model.TypeFloat64, n),
		})
		labels = append(labels, label{id: fmt.Sprintf("analog[%d]=%g", idx, n)})
	}

	results, err := conn.Write(ctx, reqs)
	type row struct {
		Request string `json:"request"`
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(results))
	for i, res := range results {
		r := row{Request: labels[i].id, OK: res.OK}
		if res.Err != nil {
			r.Error = res.Err.Error()
		}
		rows = append(rows, r)
	}
	if asJSON {
		emitJSON(map[string]any{"results": rows})
	} else {
		for _, r := range rows {
			if r.OK {
				fmt.Printf("OK   %s\n", r.Request)
			} else {
				fmt.Printf("FAIL %s: %s\n", r.Request, r.Error)
			}
		}
	}
	if err != nil {
		os.Exit(1)
	}
}

// parseDNP3Assignment 解析 INDEX=VALUE。索引只接受非负整数，避免把
// 「0x10」这类十六进制写法误当成十进制。
func parseDNP3Assignment(spec string, binary bool) (int, string, error) {
	k, v, ok := strings.Cut(spec, "=")
	if !ok {
		return 0, "", fmt.Errorf("格式应为 INDEX=VALUE")
	}
	n, err := strconv.ParseUint(strings.TrimSpace(k), 0, 31)
	if err != nil {
		return 0, "", fmt.Errorf("索引 %q 不是非负整数", k)
	}
	v = strings.TrimSpace(v)
	if binary {
		switch strings.ToLower(v) {
		case "on", "off", "1", "0", "true", "false":
		default:
			return 0, "", fmt.Errorf("二进制值应为 on|off，得到 %q", v)
		}
	}
	return int(n), v, nil
}

// stringList 是可重复的字符串参数。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
