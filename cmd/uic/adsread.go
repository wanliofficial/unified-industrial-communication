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

	adscodec "uic/internal/codec/ads"
	"uic/internal/driver"
	adsdrv "uic/internal/driver/ads"
	"uic/internal/model"
)

// uic adsread 是 ADS/AMS 主站的自检命令。
//
// ADS 的「读不到」与 Modbus 完全不同：TCP 能连上只证明 48898 端口在监听，
// 真正的失败点还有四层：
//
//   - AMS NetId 不对（错误 0x0007 target machine not found）；
//   - AMS 端口不对（0x0006 target port not found，851/801 是两个运行时）；
//   - 符号名不对（0x0710 symbol not found，大小写敏感、数组要写 [0]）；
//   - 读取宽度与 PLC 变量声明不符（0x0705 invalid size，STRING 最容易踩）。
//
// 因此命令把动作拆成互斥的几种：-probe 只做设备信息握手，-info/-state 看
// 设备，-symbol 读点位，-set 写点位，-control 改运行时状态。每一步只回答
// 一个问题，出错信息才能指到具体那一层。
//
// 符号写法：`NAME[:type[:length]]`，逗号分隔。type 省略时用 -type，
// length 省略时 string/bytes 用 -strlen。同一个 PLC 里不同变量本来就有
// 不同类型，按符号逐个声明比「一刀切 -type」更接近现场。
func adsreadCmd(args []string) {
	fs := flag.NewFlagSet("adsread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:48898", "AMS/TCP 地址 host:port（省略端口按 48898）")
	netid := fs.String("netid", "127.0.0.1.1.1", "目标 AMS NetId（6 段点分十进制，必须与 PLC 配置一致）")
	port := fs.Int("port", 851, "目标 AMS 端口（851 第一个运行时 / 801 第二个）")
	localNetID := fs.String("local-netid", "127.0.0.1.1.1", "本地 AMS NetId（设备做了白名单时必须匹配）")
	localPort := fs.Int("local-port", 32905, "本地 AMS 端口（只需本机唯一）")
	symbols := fs.String("symbol",
		"MAIN.nCounter:int16,MAIN.fTemperature:float32,MAIN.sRecipe:string:81",
		"符号列表，逗号分隔：NAME[:type[:length]]")
	defType := fs.String("type", "int16", "未在符号里声明类型时使用的类型")
	strlen := fs.Int("strlen", 81, "string/bytes 的默认读取长度（STRING(80)=81）")
	retries := fs.Int("retries", 1, "超时重发次数 0..10")
	setValue := fs.String("set", "", "写入值（需要恰好一个 -symbol）")
	setType := fs.String("setType", "", "写入类型；省略时用符号声明的类型")
	probe := fs.Bool("probe", false, "只做设备信息握手，验证链路/NetId/端口")
	info := fs.Bool("info", false, "打印设备名与版本")
	state := fs.Bool("state", false, "打印 PLC 运行时状态")
	control := fs.String("control", "", "改运行时状态：run | stop | reset（需要 -allow-control）")
	allowControl := fs.Bool("allow-control", false, "确认允许对 PLC 下控制命令")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "-timeout 必须大于 0")
		os.Exit(2)
	}
	if *retries < 0 || *retries > 10 {
		fmt.Fprintln(os.Stderr, "-retries 必须在 0..10 之间")
		os.Exit(2)
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "-port 必须在 1..65535 之间")
		os.Exit(2)
	}
	if *localPort < 1 || *localPort > 65535 {
		fmt.Fprintln(os.Stderr, "-local-port 必须在 1..65535 之间")
		os.Exit(2)
	}
	if *strlen < 1 || *strlen > 65535 {
		fmt.Fprintln(os.Stderr, "-strlen 必须在 1..65535 之间")
		os.Exit(2)
	}
	if _, err := adscodec.ParseNetID(*netid); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if _, err := adscodec.ParseNetID(*localNetID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	modes := 0
	for _, on := range []bool{*probe, *info, *state, *control != "", *setValue != ""} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "-probe / -info / -state / -control / -set 只能选一个")
		os.Exit(2)
	}

	specs, err := parseADSSymbols(*symbols, *defType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *setValue != "" && len(specs) != 1 {
		fmt.Fprintln(os.Stderr, "-set 需要恰好一个 -symbol")
		os.Exit(2)
	}
	if modes == 0 && len(specs) == 0 {
		fmt.Fprintln(os.Stderr, "-symbol 不能为空（或改用 -probe/-info/-state/-control）")
		os.Exit(2)
	}

	var ctrl adscodec.State
	if *control != "" {
		if !*allowControl {
			fmt.Fprintln(os.Stderr, "-control 会改 PLC 运行状态，确认后加 -allow-control")
			os.Exit(2)
		}
		ctrl, err = parseADSControl(*control)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
	}

	ch := model.Channel{
		ID: "ch-ads", Protocol: "ads",
		Transport: model.Transport{Kind: "tcp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels: map[string]string{
			"netid":      *netid,
			"port":       strconv.Itoa(*port),
			"localnetid": *localNetID,
			"localport":  strconv.Itoa(*localPort),
			"options":    fmt.Sprintf("retries=%d,strlen=%d", *retries, *strlen),
		},
	}

	budget := *timeout * time.Duration(2*len(specs)+8)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	// Open 内部已经做过 ReadDeviceInfo 握手：NetId/端口错误会在这里暴露，
	// 而不是等到第一条点位红掉。
	c, err := adsdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  排查顺序：AMS/TCP 地址（48898）→ labels.netid → AMS 端口 851/801 → PLC 运行时是否在运行")
		os.Exit(1)
	}
	defer c.Close()

	switch {
	case *probe:
		if *asJSON {
			emitJSON(map[string]any{
				"addr": *addr, "netid": *netid, "port": *port, "link": "up", "probe": "ReadDeviceInfo",
			})
			return
		}
		fmt.Printf("链路正常: %s（NetId %s，AMS 端口 %d，ReadDeviceInfo 收到应答）\n", *addr, *netid, *port)
		return
	case *info:
		runADSInfo(ctx, c, *asJSON)
		return
	case *state:
		runADSState(ctx, c, *asJSON)
		return
	case *control != "":
		runADSControl(ctx, c, ctrl, *asJSON)
		return
	case *setValue != "":
		runADSSet(ctx, c, specs[0], *setValue, *setType, *asJSON)
		return
	}

	runADSRead(ctx, c, specs, *asJSON)
}

// adsSymbol 是命令行声明的一个点位。
type adsSymbol struct {
	Name   string
	Type   model.DataType
	Length int
}

var adsTypes = map[model.DataType]bool{
	model.TypeBool: true, model.TypeInt16: true, model.TypeUint16: true,
	model.TypeInt32: true, model.TypeUint32: true, model.TypeInt64: true,
	model.TypeUint64: true, model.TypeFloat32: true, model.TypeFloat64: true,
	model.TypeString: true, model.TypeBytes: true,
}

// parseADSSymbols 解析 `NAME[:type[:length]]` 列表。
//
// 冒号只在「后面是已知类型/数字」时才当分隔符：PLC 符号名本身几乎不会
// 带冒号，但把歧义留给猜测会产出难懂的错误。冒号格式不合法时直接报错，
// 并指出是哪一段。
func parseADSSymbols(raw, defType string) ([]adsSymbol, error) {
	def := model.DataType(strings.TrimSpace(defType))
	if !adsTypes[def] {
		return nil, fmt.Errorf("未知默认类型 %q（支持 bool/int16/uint16/int32/uint32/int64/uint64/float32/float64/string/bytes）", defType)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []adsSymbol
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		spec := adsSymbol{Name: strings.TrimSpace(fields[0]), Type: def}
		switch len(fields) {
		case 1:
		case 2:
			t := model.DataType(strings.TrimSpace(fields[1]))
			if !adsTypes[t] {
				return nil, fmt.Errorf("符号 %q 的类型 %q 不合法（支持 bool/int16/uint16/int32/uint32/int64/uint64/float32/float64/string/bytes）",
					spec.Name, fields[1])
			}
			spec.Type = t
		case 3:
			t := model.DataType(strings.TrimSpace(fields[1]))
			if !adsTypes[t] {
				return nil, fmt.Errorf("符号 %q 的类型 %q 不合法", spec.Name, fields[1])
			}
			n, err := strconv.Atoi(strings.TrimSpace(fields[2]))
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("符号 %q 的长度 %q 必须是 1..65535 的整数", spec.Name, fields[2])
			}
			if t != model.TypeString && t != model.TypeBytes {
				return nil, fmt.Errorf("符号 %q：只有 string/bytes 需要显式长度，%s 的宽度由类型决定", spec.Name, t)
			}
			spec.Type, spec.Length = t, n
		default:
			return nil, fmt.Errorf("符号 %q 的写法不合法：NAME[:type[:length]]", part)
		}
		if spec.Name == "" {
			return nil, fmt.Errorf("符号名不能为空")
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-symbol 里没有有效符号")
	}
	return out, nil
}

// parseADSControl 把命令行里的运行状态转成 ADS 状态值。
func parseADSControl(raw string) (adscodec.State, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "run":
		return adscodec.StateRun, nil
	case "stop":
		return adscodec.StateStop, nil
	case "reset":
		return adscodec.StateReset, nil
	}
	return 0, fmt.Errorf("未知控制命令 %q（支持 run | stop | reset）", raw)
}

// adsInspector 是驱动的自检扩展接口。放进 driver.Conn 会让生产采集也
// 背上设备信息/状态/控制，而它们只在自检与运维时使用。
type adsInspector interface {
	DeviceInfo(context.Context) (adscodec.DeviceInfo, error)
	DeviceState(context.Context) (adscodec.DeviceState, error)
	WriteControl(context.Context, adscodec.State) error
}

func runADSInfo(ctx context.Context, c driver.Conn, asJSON bool) {
	in, ok := c.(adsInspector)
	if !ok {
		fmt.Fprintln(os.Stderr, "驱动不支持设备信息查询")
		os.Exit(1)
	}
	info, err := in.DeviceInfo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取设备信息失败: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		emitJSON(map[string]any{
			"name": info.Name, "version": info.VersionString(),
			"major": info.Major, "minor": info.Minor, "build": info.Version,
		})
		return
	}
	fmt.Printf("设备名: %s\n", info.Name)
	fmt.Printf("版本:   %s\n", info.VersionString())
}

func runADSState(ctx context.Context, c driver.Conn, asJSON bool) {
	in, ok := c.(adsInspector)
	if !ok {
		fmt.Fprintln(os.Stderr, "驱动不支持状态查询")
		os.Exit(1)
	}
	st, err := in.DeviceState(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取运行状态失败: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		emitJSON(map[string]any{"state": st.ADS.String(), "stateCode": uint16(st.ADS), "device": st.Device})
		return
	}
	fmt.Printf("ADS 状态: %s（%d）  设备状态: %d\n", st.ADS, uint16(st.ADS), st.Device)
}

func runADSControl(ctx context.Context, c driver.Conn, state adscodec.State, asJSON bool) {
	in, ok := c.(adsInspector)
	if !ok {
		fmt.Fprintln(os.Stderr, "驱动不支持状态控制")
		os.Exit(1)
	}
	if err := in.WriteControl(ctx, state); err != nil {
		fmt.Fprintf(os.Stderr, "控制命令 %s 失败: %v\n", state, err)
		os.Exit(1)
	}
	if asJSON {
		emitJSON(map[string]any{"control": state.String(), "stateCode": uint16(state), "ok": true})
		return
	}
	fmt.Printf("控制命令已下发: %s\n", state)
}

func runADSRead(ctx context.Context, c driver.Conn, specs []adsSymbol, asJSON bool) {
	refs := make([]driver.PointRef, 0, len(specs))
	for _, s := range specs {
		refs = append(refs, driver.PointRef{
			ID:       s.Name,
			Address:  model.Address{Kind: model.AddressSymbol, Path: s.Name, Index: -1, Length: s.Length},
			DataType: s.Type,
		})
	}
	samples, err := c.Read(ctx, []driver.ReadSpan{{ID: "selfcheck", Points: refs}})
	if err != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}

	rows := make([]adsRow, 0, len(samples))
	failed := 0
	for _, s := range samples {
		row := adsRowOf(s)
		if row.Error != "" {
			failed++
		}
		rows = append(rows, row)
	}
	if asJSON {
		out := map[string]any{"read": len(rows), "failed": failed, "rows": rows}
		if err != nil {
			out["error"] = err.Error()
		}
		emitJSON(out)
		if err != nil || failed > 0 {
			os.Exit(1)
		}
		return
	}
	printRow([]int{28, 10, 46, 12}, "符号", "类型", "值", "质量")
	for _, r := range rows {
		printRow([]int{28, 10, 46, 12}, r.Symbol, r.Type, r.display(), r.Quality)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取中断: %v\n", err)
		os.Exit(1)
	}
	if failed > 0 {
		// 逐点错误不影响连接，但脚本需要能靠退出码判断「有没有坏点」。
		fmt.Fprintf(os.Stderr, "%d/%d 个点位读取失败\n", failed, len(rows))
		os.Exit(1)
	}
}

func runADSSet(ctx context.Context, c driver.Conn, spec adsSymbol, raw, setType string, asJSON bool) {
	typ := spec.Type
	if s := strings.TrimSpace(setType); s != "" {
		typ = model.DataType(s)
	}
	val, err := parseADSSetValue(raw, typ)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	res, werr := c.Write(ctx, []driver.WriteRequest{{
		PointID:  spec.Name,
		Address:  model.Address{Kind: model.AddressSymbol, Path: spec.Name, Index: -1, Length: spec.Length},
		DataType: typ,
		Value:    val,
	}})
	if werr != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", werr)
		os.Exit(1)
	}
	if len(res) == 1 && res[0].OK {
		if asJSON {
			emitJSON(map[string]any{"symbol": spec.Name, "type": string(typ), "value": val.String(), "ok": true})
			return
		}
		fmt.Printf("写入 %s = %s（%s）完成\n", spec.Name, val.String(), typ)
		return
	}
	var reason error
	if len(res) == 1 {
		reason = res[0].Err
	}
	if asJSON {
		emitJSON(map[string]any{"symbol": spec.Name, "type": string(typ), "ok": false, "error": errText(reason)})
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "写入被拒绝: %v\n", reason)
	os.Exit(1)
}

// parseADSSetValue 把命令行字符串按指定类型解析成平台值。
//
// 整数用 base 0（十进制与 0x 十六进制都接受）；浮点单独一条路径，因为
// ADS 的 REAL/LREAL 本来就是浮点，用整型解析会把 23.5 直接判成非法。
func parseADSSetValue(raw string, typ model.DataType) (model.Value, error) {
	raw = strings.TrimSpace(raw)
	switch typ {
	case model.TypeBool:
		switch strings.ToLower(raw) {
		case "true", "1", "on", "yes", "开":
			return model.BoolValue(true), nil
		case "false", "0", "off", "no", "关":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("布尔值 %q 不合法（true/false、1/0、on/off）", raw)
	case model.TypeInt16:
		n, err := strconv.ParseInt(raw, 0, 16)
		if err != nil {
			return model.Value{}, fmt.Errorf("int16 值 %q 不合法或越界（-32768..32767）", raw)
		}
		return model.NumValue(model.TypeInt16, float64(n)), nil
	case model.TypeInt32:
		n, err := strconv.ParseInt(raw, 0, 32)
		if err != nil {
			return model.Value{}, fmt.Errorf("int32 值 %q 不合法或越界", raw)
		}
		return model.NumValue(model.TypeInt32, float64(n)), nil
	case model.TypeInt64:
		n, err := strconv.ParseInt(raw, 0, 64)
		if err != nil {
			return model.Value{}, fmt.Errorf("int64 值 %q 不合法或越界", raw)
		}
		return model.NumValue(model.TypeInt64, float64(n)), nil
	case model.TypeUint16:
		n, err := strconv.ParseUint(raw, 0, 16)
		if err != nil {
			return model.Value{}, fmt.Errorf("uint16 值 %q 不合法或越界（0..65535）", raw)
		}
		return model.NumValue(model.TypeUint16, float64(n)), nil
	case model.TypeUint32:
		n, err := strconv.ParseUint(raw, 0, 32)
		if err != nil {
			return model.Value{}, fmt.Errorf("uint32 值 %q 不合法或越界（0..4294967295）", raw)
		}
		return model.NumValue(model.TypeUint32, float64(n)), nil
	case model.TypeUint64:
		n, err := strconv.ParseUint(raw, 0, 64)
		if err != nil {
			return model.Value{}, fmt.Errorf("uint64 值 %q 不合法或越界", raw)
		}
		return model.NumValue(model.TypeUint64, float64(n)), nil
	case model.TypeFloat32:
		f, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return model.Value{}, fmt.Errorf("float32 值 %q 不合法", raw)
		}
		return model.NumValue(model.TypeFloat32, f), nil
	case model.TypeFloat64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return model.Value{}, fmt.Errorf("float64 值 %q 不合法", raw)
		}
		return model.NumValue(model.TypeFloat64, f), nil
	case model.TypeString:
		return model.TextValue(model.TypeString, raw), nil
	case model.TypeBytes:
		return model.TextValue(model.TypeBytes, raw), nil
	case model.TypeBCD16, model.TypeBCD32:
		return model.Value{}, fmt.Errorf("ADS 没有 BCD 线类型（%s）；请用 uint16/uint32", typ)
	}
	return model.Value{}, fmt.Errorf("未知写入类型 %q（支持 bool|int16|uint16|int32|uint32|int64|uint64|float32|float64|string|bytes）", typ)
}

type adsRow struct {
	Symbol  string `json:"symbol"`
	Type    string `json:"type,omitempty"`
	Value   string `json:"value,omitempty"`
	Quality string `json:"quality,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (r adsRow) display() string {
	if r.Error != "" {
		return "错误: " + r.Error
	}
	return r.Value
}

func adsRowOf(s driver.RawSample) adsRow {
	row := adsRow{Symbol: s.PointID}
	if s.Err != nil {
		row.Quality = string(adsQuality(s.Err))
		row.Error = s.Err.Error()
		return row
	}
	row.Quality = string(s.Quality)
	row.Type = string(s.Value.Type)
	row.Value = s.Value.String()
	if s.Value.Type == model.TypeBytes {
		// bytes 不是文本：直接打印会输出控制字符，终端里看起来像空值。
		row.Value = "0x" + hex.EncodeToString([]byte(s.Value.Text))
	}
	return row
}

func adsQuality(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}
