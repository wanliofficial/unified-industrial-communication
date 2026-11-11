package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	knxcodec "uic/internal/codec/knx"
	"uic/internal/driver"
	knxdrv "uic/internal/driver/knx"
	"uic/internal/model"
	knxsim "uic/internal/sim/knx"
)

// uic knxread 是一个极小的 KNXnet/IP 组地址自检命令。
//
// 它与寄存器型协议的自检命令有一处根本不同：**必须显式给出 DPT**。组地址
// 在总线上不携带类型，同一个 `1/2/3` 在 A 工程里是 1 位开关、在 B 工程里是
// 2 字节浮点温度。所以每个点位是 `组地址@DPT` 的形式（`2/1/1@9.001`），
// 而不是由 `-type` 反推：反推会得到「float32 到底是 9.xxx 还是 14.xxx」
// 这种无法回答的问题，猜错了还不报错——只会把 21.5 读成 3.7e-41。
//
// 三种典型用法：
//
//	uic knxread -addr 192.168.1.10:3671 -probe
//	uic knxread -addr 192.168.1.10:3671 -dpt 9.001 -group 2/1/1
//	uic knxread -addr 192.168.1.10:3671 -dpt 1.001 -group 1/1/1 -write on
//
// 边界与驱动一致：TUNNELING over UDP 3671、GroupValueRead/Response/Write；
// 不做 ROUTING 组播、不做设备管理属性、不做 KNX Secure、不做订阅。
func knxreadCmd(args []string) {
	fs := flag.NewFlagSet("knxread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:3671", "网关地址 host[:port]，端口默认 3671")
	group := fs.String("group", "1/1/1", "组地址：主组/中间组/子组（0..31 / 0..7 / 0..255）")
	dpt := fs.String("dpt", "", "数据点类型，例如 9.001 / 1.001 / 5.001（必填，除非 -list）")
	typ := fs.String("type", "", "平台数据类型；留空按 DPT 推断最自然的类型")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	heartbeat := fs.Duration("heartbeat", 60*time.Second, "心跳间隔（1s..60s）")
	retries := fs.Int("retries", 2, "隧道帧重传次数（0..5）")
	source := fs.String("source", "", "源物理地址（如 15.15.250）；留空用网关分配的地址")
	probe := fs.Bool("probe", false, "只建隧道并做通道状态检查，不读组地址")
	write := fs.String("write", "", "写入值（按 DPT 与 -type 解释；1 位 DPT 可用 on/off）")
	list := fs.Bool("list", false, "列出内置网关的默认对象表（配合 sim -protocol knx）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "同时显示线格式字节")
	fs.Parse(args)

	writeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "write" {
			writeSet = true
		}
	})

	if *list {
		listKNXObjects(*asJSON)
		return
	}

	groupText, dptText := *group, *dpt
	// 允许把 DPT 直接写在组地址后面：`1/2/3@9.001`。这是点表里的规范
	// 写法，命令行走同一条解析路径，免得两处语法漂移。
	if i := strings.LastIndex(groupText, "@"); i >= 0 {
		if dptText == "" {
			dptText = groupText[i+1:]
		}
		groupText = groupText[:i]
	}
	groupText = strings.TrimSpace(groupText)
	dptText = strings.TrimSpace(dptText)

	labels := map[string]string{
		"heartbeat":  heartbeat.String(),
		"maxRetries": strconv.Itoa(*retries),
	}
	if *source != "" {
		labels["sourceAddress"] = *source
	}

	// -probe 只验证「能不能建隧道」：它不该要求一个合法的组地址与 DPT，
	// 因为探测往往发生在还不知道工程组地址分配的时候。
	if *probe {
		runKNXProbe(*addr, *timeout, labels, *asJSON)
		return
	}

	if dptText == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dpt：KNX 的组地址不携带类型，必须声明怎么解释字节")
		fmt.Fprintln(os.Stderr, "  例：uic knxread -addr 192.168.1.10:3671 -dpt 9.001 -group 2/1/1")
		fmt.Fprintln(os.Stderr, "      常见：1.001 开关 / 9.001 温度 / 5.001 百分比 / 14.056 功率")
		os.Exit(2)
	}

	d, err := knxcodec.ParseDPT(dptText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-dpt %q: %v\n", dptText, err)
		os.Exit(2)
	}
	ga, err := knxcodec.ParseGroupAddress(groupText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-group %q: %v\n", groupText, err)
		os.Exit(2)
	}

	// 平台类型：给了 -type 就用它，否则按 DPT 选一个「能装下全部取值」的
	// 类型。这一步是安全的，因为它只影响平台侧怎么存，不影响线宽——线宽
	// 与取值范围始终由 DPT 决定。
	dt := model.DataType(*typ)
	if dt == "" {
		dt = knxDefaultType(d)
	}
	if err := d.Accepts(dt); err != nil {
		fmt.Fprintf(os.Stderr, "-type %q 与 -dpt %s 不匹配: %v\n", *typ, d, err)
		os.Exit(2)
	}

	point := model.Point{
		ID: "cli", Enabled: true, DataType: dt,
		Address: model.Address{Kind: model.AddressSymbol, Path: ga.String(), DPT: d.String()},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	defer cancel()

	conn, err := openKNX(ctx, *addr, *timeout, labels, []model.Point{point})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if writeSet {
		value, err := knxCLIValue(*write, d, dt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-write %q: %v\n", *write, err)
			os.Exit(2)
		}
		runKNXWrite(ctx, conn, point, value, *asJSON)
		return
	}

	runKNXRead(ctx, conn, point, d, *asJSON, *raw)
}

// runKNXProbe 只验证「链路通、隧道能建、通道状态正常」，不读任何组地址。
//
// 它与读一次点位是两件事：读点位同时验证了「网关对不对」和「工程组地址配
// 得对不对」，现场排障时经常需要把这两件事拆开。隧道槽位满、网关重启、本地
// 防火墙丢 UDP，都会让 Probe 失败而没有任何组地址参与。
func runKNXProbe(addr string, timeout time.Duration, labels map[string]string, asJSON bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+10*time.Second)
	defer cancel()

	conn, err := openKNX(ctx, addr, timeout, labels, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := conn.Probe(ctx); err != nil {
		if asJSON {
			emitJSON(map[string]any{"addr": addr, "ok": false, "error": err.Error()})
		} else {
			fmt.Fprintf(os.Stderr, "探测失败: %v\n", err)
		}
		os.Exit(1)
	}
	if asJSON {
		emitJSON(map[string]any{"addr": addr, "ok": true, "tunnel": "established"})
		return
	}
	fmt.Printf("链路正常: %s（隧道已建立，通道状态检查通过）\n", addr)
}

func openKNX(ctx context.Context, addr string, timeout time.Duration,
	labels map[string]string, points []model.Point) (driver.Conn, error) {
	dev := model.Device{
		ID: "cli", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Labels:   labels,
		Points:   points,
	}
	ch := model.Channel{
		ID: "ch", Protocol: "knx",
		Transport: model.Transport{Kind: "udp", Address: addr},
		Timeout:   model.Duration(timeout),
	}
	return knxdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
}

func runKNXRead(ctx context.Context, conn driver.Conn, p model.Point, d knxcodec.DPT, asJSON, raw bool) {
	samples, err := conn.Read(ctx, []driver.ReadSpan{{
		ID: "cli", Points: []driver.PointRef{driver.PointRefOf(model.Device{}, p)},
	}})
	if len(samples) == 0 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "读取失败: 没有返回样本")
		}
		os.Exit(1)
	}
	s := samples[0]

	type row struct {
		Group   string `json:"group"`
		DPT     string `json:"dpt"`
		Type    string `json:"type"`
		Value   string `json:"value,omitempty"`
		Wire    string `json:"wire,omitempty"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	r := row{
		Group:   p.Address.Path,
		DPT:     d.String(),
		Type:    string(p.DataType),
		Quality: string(s.Quality),
	}
	if s.Err != nil {
		r.Error = s.Err.Error()
	} else {
		r.Value = knxValueText(s.Value)
		if raw {
			r.Wire = knxWireOf(s.Value, d, p.DataType)
		}
	}

	if asJSON {
		emitJSON(r)
	} else if s.Err != nil {
		fmt.Printf("%s@%s: 失败（%s）: %s\n", r.Group, r.DPT, r.Quality, r.Error)
	} else if raw {
		fmt.Printf("%s@%s = %s（%s，线格式 %s）\n", r.Group, r.DPT, r.Value, r.Quality, r.Wire)
	} else {
		fmt.Printf("%s@%s = %s（%s）\n", r.Group, r.DPT, r.Value, r.Quality)
	}
	if err != nil {
		os.Exit(1)
	}
}

func runKNXWrite(ctx context.Context, conn driver.Conn, p model.Point, value model.Value, asJSON bool) {
	results, err := conn.Write(ctx, []driver.WriteRequest{{
		PointID: p.ID, Address: p.Address, DataType: p.DataType, Value: value,
	}})
	type row struct {
		Group   string `json:"group"`
		DPT     string `json:"dpt"`
		Value   string `json:"value"`
		OK      bool   `json:"ok"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	r := row{
		Group: p.Address.Path, DPT: p.Address.DPT,
		Value: knxValueText(value), Quality: string(model.QualityBad),
	}
	if len(results) > 0 {
		r.OK = results[0].OK
		r.Quality = string(results[0].Quality)
		if results[0].Err != nil {
			r.Error = results[0].Err.Error()
		}
	}
	if asJSON {
		emitJSON(r)
	} else if r.OK {
		fmt.Printf("OK   %s@%s ← %s\n", r.Group, r.DPT, r.Value)
	} else {
		fmt.Printf("FAIL %s@%s: %s\n", r.Group, r.DPT, r.Error)
	}
	if err != nil || !r.OK {
		os.Exit(1)
	}
}

// listKNXObjects 打印内置网关的默认对象表。
//
// 它连的是本机 127.0.0.1 的 `uic sim -protocol knx`，而不是真网关：真网关
// 没有「列出所有组地址」这种服务（组地址表属于工程，不属协议）。这条命令
// 存在的意义只是让自检流程能一眼看到「自检网关上有什么可读」。
func listKNXObjects(asJSON bool) {
	objs := knxsim.DefaultObjects()
	if asJSON {
		type row struct {
			Group string `json:"group"`
			DPT   string `json:"dpt"`
			Label string `json:"label"`
		}
		out := make([]row, 0, len(objs))
		for _, o := range objs {
			out = append(out, row{Group: o.Group, DPT: o.DPT, Label: o.Label})
		}
		emitJSON(map[string]any{"objects": out})
		return
	}
	fmt.Println("内置 KNX 网关的默认对象表（uic sim -protocol knx）")
	printRow([]int{12, 12, 28}, "组地址", "DPT", "说明")
	for _, o := range objs {
		printRow([]int{12, 12, 28}, o.Group, o.DPT, o.Label)
	}
}

// knxValueText 把平台值渲染成人能读的文本。
func knxValueText(v model.Value) string {
	switch v.Type {
	case model.TypeBool:
		return strconv.FormatBool(v.Bool)
	case model.TypeString, model.TypeBytes:
		return v.Text
	}
	if v.Num == float64(int64(v.Num)) {
		return strconv.FormatInt(int64(v.Num), 10)
	}
	return strconv.FormatFloat(v.Num, 'g', 6, 64)
}

// knxWireOf 把值重新编码成线格式，用于 `-raw` 时对照。
//
// 重新编码而不是「保存收到的字节」：这条命令只看到解码后的值，把值再编回去
// 恰好能验证「编码-解码是自洽的」——两边不一致会立刻在输出里看出来。
func knxWireOf(v model.Value, d knxcodec.DPT, t model.DataType) string {
	p, err := d.Encode(v, t)
	if err != nil {
		return "编码失败: " + err.Error()
	}
	if d.Bits == 1 {
		if p.Bit {
			return "01"
		}
		return "00"
	}
	return hex.EncodeToString(p.Data)
}

// knxCLIValue 把命令行字符串解成平台值。
//
// 1 位 DPT 单独处理：`on`/`off` 比 `1`/`0` 自然，而 true/false 也照收。
// 其余类型按平台类型解析，与点表侧的转换保持一致。
func knxCLIValue(s string, d knxcodec.DPT, t model.DataType) (model.Value, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return model.Value{}, fmt.Errorf("值是空的")
	}
	if t == model.TypeBool {
		switch strings.ToLower(text) {
		case "on", "true", "1", "开":
			return model.BoolValue(true), nil
		case "off", "false", "0", "关":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("bool 只接受 on/off/true/false/1/0，得到 %q", s)
	}
	if t == model.TypeBytes {
		return model.BytesValue([]byte(text)), nil
	}
	if t == model.TypeString {
		return model.TextValue(model.TypeString, text), nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return model.Value{}, fmt.Errorf("不是数字: %q", s)
	}
	// 先按 DPT 的取值范围校验一次，让错误在本地就出现，而不是等网关回
	// 「总线确认失败」那种看不出原因的答复。
	if f < d.Min || f > d.Max {
		return model.Value{}, fmt.Errorf("%s 的取值范围是 %v..%v，得到 %v", d, d.Min, d.Max, f)
	}
	return model.NumValue(t, f), nil
}

// knxDefaultType 按 DPT 选一个能表示全部取值、又不过宽的平台类型。
//
// 平台侧没有 8 位整型：KNX 的 8 位 DPT 一律落到 16 位。多出来的 8 位是空的，
// 但「装得下」比「刚好」重要——把 0..255 挤进更窄的类型才是错的。
func knxDefaultType(d knxcodec.DPT) model.DataType {
	if d.Bits == 1 {
		return model.TypeBool
	}
	switch d.Kind {
	case knxcodec.KindFloat16, knxcodec.KindFloat32, knxcodec.KindScaled:
		return model.TypeFloat32
	case knxcodec.KindSigned:
		if d.Bits <= 16 {
			return model.TypeInt16
		}
		return model.TypeInt32
	default:
		if d.Bits <= 16 {
			return model.TypeUint16
		}
		return model.TypeUint32
	}
}
