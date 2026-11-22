package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	iec104codec "uic/internal/codec/iec104"
	"uic/internal/driver"
	iec104drv "uic/internal/driver/iec104"
	"uic/internal/model"
)

// uic iec104read 是一个极小的 IEC 60870-5-104 主站自检命令。
//
// 与 mbread/mcread/finsread/enipread/bacread 同理由：现场最缺的是「不装
// 任何第三方软件就能自证链路通」的手段。104 尤其需要——它是「从站主动
// 上送」的协议，`telnet 2404` 能连上不代表 STARTDT 能过、更不代表总召唤
// 有数据回来。三层（TCP / APCI 握手 / ASDU）任何一层错位都表现为「连上
// 了但读不到」，只有把每一层单独打出来才能定位。
//
// 它回答的是「这台设备实际送哪些 IOA、用哪种类型、带不带时标」，而不是
// 「我配置的点现在是什么值」。后者用网关的 /api/v1/points 看，那里才有
// 点位映射、报警与历史。这里刻意不依赖点表：初次接一台陌生设备时，
// 点表本身就是要靠这条命令做出来的。
//
// 边界：一次附着 + 总召唤 + 汇总一屏，支持下发一条命令；不做周期轮询、
// 不做多设备、不做带时标命令、不做文件传输。
func iec104readCmd(args []string) {
	fs := flag.NewFlagSet("iec104read", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:2404", "从站地址 host[:port]，端口默认 2404")
	ca := fs.Int("ca", 1, "公共地址（CA），1..65535")
	wait := fs.Duration("wait", 2*time.Second, "总召唤后等待上送的时间")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	only := fs.String("ioa", "", "只显示这些 IOA，逗号分隔（默认为空表示全部）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "额外显示传送原因、限定词与原始品质位")
	probe := fs.Bool("probe", false, "只做 STARTDT + TESTFR 往返，不召唤")

	cmdArea := fs.String("area", "", "下发命令时的对象类别：single-command | double-command | setpoint-float | setpoint-scaled | setpoint-normalized")
	cmdIOA := fs.Int("cmd-ioa", 0, "下发命令的信息对象地址（IOA）")
	write := fs.String("write", "", "下发命令的值：单/双点写 on|off|1|2；设定值写数字")
	fs.Parse(args)

	caVal := *ca
	if caVal < 1 || caVal > 65535 {
		fmt.Fprintf(os.Stderr, "ca: 必须在 1..65535 之间，得到 %d\n", caVal)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		*timeout+*wait+10*time.Second)
	defer cancel()

	conn, err := openIEC104(ctx, *addr, caVal, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// STARTDT 只证明链路层通。再做一次 TESTFR 往返，才能把「对端 TCP
	// 接受连接但应用层没起来」与「应用层正常」区分开——这是 104 现场最
	// 常见的两种「连上了却没数据」。
	if err := conn.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（TESTFR）失败: %v\n", err)
		os.Exit(1)
	}

	// 命令写法优先：只要给了 -write / -area / -cmd-ioa 任一个就进命令路径。
	if *write != "" || *cmdArea != "" || *cmdIOA != 0 {
		runIEC104Write(ctx, conn, *cmdArea, *cmdIOA, *write, *asJSON)
		return
	}

	if *probe {
		if *asJSON {
			emitJSON(map[string]any{"addr": *addr, "ca": caVal, "link": "up"})
			return
		}
		fmt.Printf("链路正常: %s CA=%d（STARTDT 与 TESTFR 均通过）\n", *addr, caVal)
		return
	}

	// 等待总召唤快照与首批突发上送。Open 里已经发过站召唤，这里不再重
	// 复发送：短时间内重复召唤会被从站当成风暴忽略，反而看不出问题。
	time.Sleep(*wait)

	obs, ok := conn.(interface {
		Observed() []iec104drv.Observed
		Dropped() uint64
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供上送对象快照")
		os.Exit(1)
	}
	rows := make([]iec104Row, 0, 32)
	for _, o := range obs.Observed() {
		rows = append(rows, iec104Row{
			IOA:     o.IOA,
			Type:    iec104codec.TypeName(o.TypeID),
			TypeID:  o.TypeID,
			Cause:   iec104codec.CauseName(o.Cause),
			Quality: qualityText(o.Object.Quality),
			RawQual: o.Object.Quality.String(),
			Value:   formatObject(o.TypeID, o.Object),
			Time:    formatStamp(o.Object.Time),
			Points:  o.Points,
			At:      o.At.Format("15:04:05.000"),
		})
	}
	total := len(rows)
	if *only != "" {
		rows = filterIOA(rows, *only)
	}

	if *asJSON {
		emitJSON(map[string]any{
			"addr": *addr, "ca": caVal, "link": "up",
			"observed": len(rows), "total": total,
			"dropped": obs.Dropped(),
			"rows":    rows,
		})
		return
	}

	fmt.Printf("从站 %s  CA=%d\n", *addr, caVal)
	fmt.Printf("设备上送 %d 个信息对象", len(rows))
	if total != len(rows) {
		fmt.Printf("（共 %d 个，已按 -ioa 过滤）", total)
	}
	fmt.Println()
	if d := obs.Dropped(); d > 0 {
		fmt.Printf("警告: 有 %d 个 IOA 超出诊断表上限被丢弃\n", d)
	}
	fmt.Println()

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

// printRow 按显示宽度对齐一列文本。widths 给出每个非末列的宽度；末列
// 不补齐（避免行尾空格）。
func printRow(widths []int, cells ...string) {
	var b strings.Builder
	for i, c := range cells {
		if i == len(cells)-1 {
			b.WriteString(c)
			break
		}
		w := 24
		if i < len(widths) {
			w = widths[i]
		}
		b.WriteString(c)
		if pad := w - displayWidth(c); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		} else {
			b.WriteString(" ")
		}
	}
	fmt.Println(strings.TrimRight(b.String(), " "))
}

// displayWidth 估算终端显示宽度：CJK 与全角标点占两列。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // 谚文字母
			r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、假名、汉字
			r >= 0xAC00 && r <= 0xD7A3, // 谚文音节
			r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
			r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
			r >= 0xFF00 && r <= 0xFF60, // 全角 ASCII
			r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
			w += 2
		default:
			w++
		}
	}
	return w
}

// iec104Row 是自检输出的一行。
type iec104Row struct {
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

// openIEC104 附着到从站。
//
// 点表只放一个探针点位，因为 buildPlans 要求至少一个可用的点才肯开口。
// 探针刻意选一个几乎不可能存在的 IOA（16777215）：它若真的收到值反而说
// 明设备地址空间异常，而这个点位本身不参与任何输出——诊断列的是驱动
// 旁路记录的上送对象，与点表无关。
func openIEC104(ctx context.Context, addr string, ca int, timeout time.Duration) (driver.Conn, error) {
	dev := model.Device{
		ID: "probe", ChannelID: "ch", UnitID: ca, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Points: []model.Point{{
			ID: "probe", Enabled: true, DataType: model.TypeFloat64,
			Address: model.Address{
				Kind: model.AddressRegister, Area: iec104drv.AreaIOA,
				Offset: 0xFFFFFF, Bit: -1,
			},
		}},
	}
	ch := model.Channel{
		ID: "ch", Protocol: "iec104",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(timeout),
	}
	return iec104drv.New().Open(ctx, driver.OpenConfig{
		Channel: ch, Device: dev,
	})
}

// formatObject 把原始信息对象格式化成一行值。
//
// 直接按 ASDU 类型解释，不经过点位类型：诊断要的就是设备原样，而不是
// 「我们愿意把它存成什么」。双点 0/3 单列出来，因为它们是设备明确告知
// 「状态未知/不允许」，不是两个普通的布尔值。
func formatObject(typeID byte, o iec104codec.Object) string {
	switch typeID {
	case iec104codec.TypeSinglePoint, iec104codec.TypeSinglePointTime:
		if o.Bool {
			return "on"
		}
		return "off"
	case iec104codec.TypeDoublePoint, iec104codec.TypeDoublePointTime:
		switch o.Uint {
		case 1:
			return "off (1)"
		case 2:
			return "on (2)"
		default:
			return fmt.Sprintf("不确定 (%d)", o.Uint)
		}
	case iec104codec.TypeMeasuredNormalized, iec104codec.TypeMeasuredNormalizedTime:
		return fmt.Sprintf("%.5f", o.Num)
	case iec104codec.TypeMeasuredScaled, iec104codec.TypeMeasuredScaledTime:
		return strconv.FormatInt(int64(o.Num), 10)
	case iec104codec.TypeMeasuredFloat, iec104codec.TypeMeasuredFloatTime:
		return trimFloat(o.Num)
	case iec104codec.TypeIntegratedTotals, iec104codec.TypeIntegratedTotalsTime:
		return strconv.FormatUint(uint64(o.Uint), 10)
	case iec104codec.TypeStepPosition:
		return strconv.FormatUint(uint64(o.Uint), 10)
	case iec104codec.TypeBitstring:
		return fmt.Sprintf("0x%08X", o.Uint)
	case iec104codec.TypeClockSync:
		return ""
	}
	return fmt.Sprintf("%+v", o)
}

// trimFloat 去掉浮点数尾巴上的零（2.5 → "2.5"，12 → "12"）。
func trimFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// qualityText 把品质位集合压成一个词，与网关点位质量同一套语义。
func qualityText(q iec104codec.Quality) string {
	switch {
	case q.Has(iec104codec.QInvalid):
		return "bad"
	case q.Has(iec104codec.QNotTopical):
		return "stale"
	case q.Has(iec104codec.QSubstituted),
		q.Has(iec104codec.QBlocked),
		q.Has(iec104codec.QOverflow):
		return "uncertain"
	}
	return "good"
}

func formatStamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02 15:04:05.000")
}

func filterIOA(rows []iec104Row, spec string) []iec104Row {
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
	out := make([]iec104Row, 0, len(rows))
	for _, r := range rows {
		if want[r.IOA] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IOA < out[j].IOA })
	return out
}

// runIEC104Write 下发一条命令并打印确认结果。
func runIEC104Write(ctx context.Context, conn driver.Conn, area string, ioa int, value string, asJSON bool) {
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
	req, err := buildIEC104Write(area, ioa, value)
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
	fmt.Printf("命令已确认执行: %s IOA=%d value=%s\n", strings.ToLower(area), ioa, value)
}

// buildIEC104Write 把命令行参数翻译成一条写入请求。
//
// 校验放在这里而不是驱动里，是因为命令行能给出更具体的提示；驱动侧的
// 校验仍然保留，它服务的是运行时（那里拿不到命令行上下文）。
func buildIEC104Write(area string, ioa int, value string) (driver.WriteRequest, error) {
	area = strings.ToLower(strings.TrimSpace(area))
	addr := model.Address{
		Kind: model.AddressRegister, Area: area, Offset: ioa, Bit: -1,
	}
	req := driver.WriteRequest{
		PointID: fmt.Sprintf("%s:%d", area, ioa), Address: addr,
	}

	switch area {
	case iec104drv.AreaSingleCommand:
		v, err := parseOnOff(value)
		if err != nil {
			return req, fmt.Errorf("单点命令: %w", err)
		}
		req.DataType = model.TypeBool
		req.Value = model.BoolValue(v)
	case iec104drv.AreaDoubleCommand:
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
	case iec104drv.AreaSetpointFloat,
		iec104drv.AreaSetpointScaled,
		iec104drv.AreaSetpointNormalized:
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

func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "1", "true", "合", "close":
		return true, nil
	case "off", "0", "false", "分", "open":
		return false, nil
	}
	return false, fmt.Errorf("%q 不是合法状态（on|off|1|0）", s)
}

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "序列化输出失败: %v\n", err)
		os.Exit(1)
	}
}
