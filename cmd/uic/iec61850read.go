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

	ieccodec "uic/internal/codec/iec61850"
	"uic/internal/driver"
	iecdrv "uic/internal/driver/iec61850"
	"uic/internal/model"
	iecsim "uic/internal/sim/iec61850"
)

// uic iec61850read 是一个极小的 IEC 61850（MMS）自检命令。
//
// 它与寄存器型协议的自检命令有一处根本不同：**类型不在点表上，在装置上**。
// `simpleIO/GGIO1.MX.PhV.mag.f` 到底是 float32 还是 float64、是 1 个还是 4 个
// 字节，由装置的 MMS 模型决定，读回来才知道。所以 `-type` 只声明平台侧怎么
// 存，不参与线的解释；`-raw` 显示的是真正发出去的 MMS 项名
// （`simpleIO$GGIO1$MX$PhV$mag$f`），而不是按 `-type` 猜出来的字节。
//
// 四种典型用法：
//
//	uic iec61850read -addr 192.168.1.10:102 -probe
//	uic iec61850read -addr 192.168.1.10:102 -object simpleIO/GGIO1.ST.Ind1.stVal -type bool
//	uic iec61850read -addr 192.168.1.10:102 -object simpleIO/GGIO1.ST.Ind1.stVal -type bool -write true
//	uic iec61850read -addr 192.168.1.10:102 -browse -domain simpleIO
//
// 边界与驱动一致：ACSE + MMS 初始化 + 读/写变量 + 模型浏览（getNameList）。
// 不做 GOOSE/SV、报告控制块、文件传输、定值组、数据集读写、TLS。
func iec61850readCmd(args []string) {
	fs := flag.NewFlagSet("iec61850read", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:102", "IED 地址 host[:port]，端口默认 102")
	object := fs.String("object", "", "对象引用 LD/LN.FC.DO[.DA]，多个用逗号分隔")
	typ := fs.String("type", "float64", "平台侧数据类型（只影响怎么存；线宽由装置的 MMS 类型决定）")
	defaultFC := fs.String("fc", "", "对象引用省略 FC 时的默认功能约束（如 ST/MX/SP）")
	domain := fs.String("domain", "", "模型浏览的逻辑设备名；留空表示整个 VMD（只对 -browse 有意义）")
	write := fs.String("write", "", "写入值（按 -type 解释；bool 可用 true/false/on/off/1/0）")
	probe := fs.Bool("probe", false, "只建立关联（可选读一次探针对象），不读点位")
	browse := fs.Bool("browse", false, "枚举逻辑设备下的命名变量（MMS getNameList）")
	list := fs.Bool("list", false, "列出内置 IED 的默认对象表（配合 sim -protocol iec61850）")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "额外显示实际发出的 MMS 项名")
	fs.Parse(args)

	writeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "write" {
			writeSet = true
		}
	})

	if *list {
		listIECObjects(*asJSON)
		return
	}

	if !*probe && !*browse && strings.TrimSpace(*object) == "" {
		fmt.Fprintln(os.Stderr, "缺少 -object：IEC 61850 必须给出对象引用（或改用 -probe / -browse / -list）")
		fmt.Fprintln(os.Stderr, "  例：uic iec61850read -addr 192.168.1.10:102 -object simpleIO/GGIO1.ST.Ind1.stVal -type bool")
		fmt.Fprintln(os.Stderr, "      常见：ST.Ind1.stVal（状态）/ MX.PhV.mag.f（量测）/ CO.SPCs.stVal（控制）")
		os.Exit(2)
	}

	dt := model.DataType(strings.TrimSpace(*typ))
	if dt == "" {
		dt = model.TypeFloat64
	}
	if !dt.Valid() {
		fmt.Fprintf(os.Stderr, "-type %q 不是合法的平台类型\n", *typ)
		fmt.Fprintf(os.Stderr, "  可选：bool/int16/int32/int64/uint16/uint32/uint64/float32/float64/string/bytes\n")
		os.Exit(2)
	}

	labels := map[string]string{}
	if fc := strings.TrimSpace(*defaultFC); fc != "" {
		if !ieccodec.IsValidFC(fc) {
			fmt.Fprintf(os.Stderr, "-fc %q 不是功能约束（ST/MX/CO/SP/CF/DC/SG/SE）\n", fc)
			os.Exit(2)
		}
		labels["defaultFC"] = strings.ToUpper(fc)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	defer cancel()

	if *probe {
		runIECProbe(ctx, *addr, *timeout, labels, *asJSON)
		return
	}

	if *browse {
		names, err := iecdrv.Browse(ctx, iecOpenConfig(*addr, *timeout, labels, nil), *domain)
		if err != nil {
			if *asJSON {
				emitJSON(map[string]any{"addr": *addr, "domain": *domain, "ok": false, "error": err.Error()})
			} else {
				fmt.Fprintf(os.Stderr, "浏览失败: %v\n", err)
			}
			os.Exit(1)
		}
		emitIECNames(*addr, *domain, names, *asJSON, *raw)
		return
	}

	points, parsed, err := iecPoints(*object, dt, labels)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	conn, err := iecdrv.New().Open(ctx, iecOpenConfig(*addr, *timeout, labels, points))
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if writeSet {
		value, verr := iecCLIValue(*write, dt)
		if verr != nil {
			fmt.Fprintf(os.Stderr, "-write %q: %v\n", *write, verr)
			os.Exit(2)
		}
		runIECWrite(ctx, conn, points[0], value, *asJSON)
		return
	}
	runIECRead(ctx, conn, points, parsed, *asJSON, *raw)
}

// iecOpenConfig 组装 OpenConfig：驱动只关心通道地址、设备超时与设备标签。
func iecOpenConfig(addr string, timeout time.Duration, labels map[string]string, points []model.Point) driver.OpenConfig {
	dev := model.Device{
		ID: "cli", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Labels:   labels,
		Points:   points,
	}
	ch := model.Channel{
		ID: "ch", Protocol: "iec61850",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(timeout),
	}
	return driver.OpenConfig{Channel: ch, Device: dev}
}

// iecPoints 把逗号分隔的对象引用变成点位，并返回「点位 ID → 解析后的引用」，
// 供输出时显示规范形式与 MMS 项名。
func iecPoints(text string, dt model.DataType, labels map[string]string) ([]model.Point, []ieccodec.ObjectRef, error) {
	parts := strings.Split(text, ",")
	points := make([]model.Point, 0, len(parts))
	refs := make([]ieccodec.ObjectRef, 0, len(parts))
	for i, part := range parts {
		path := strings.TrimSpace(part)
		if path == "" {
			continue
		}
		oref, err := ieccodec.ParseObjectRef(path, labels["defaultFC"])
		if err != nil {
			return nil, nil, fmt.Errorf("对象引用 %q: %w", path, err)
		}
		points = append(points, model.Point{
			ID: "p" + strconv.Itoa(i), Enabled: true, DataType: dt,
			Address: model.Address{Kind: model.AddressSymbol, Path: oref.String(), Index: -1},
		})
		refs = append(refs, oref)
	}
	if len(points) == 0 {
		return nil, nil, fmt.Errorf("-object 里没有解析出任何对象引用")
	}
	return points, refs, nil
}

func runIECProbe(ctx context.Context, addr string, timeout time.Duration, labels map[string]string, asJSON bool) {
	conn, err := iecdrv.New().Open(ctx, iecOpenConfig(addr, timeout, labels, nil))
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
		emitJSON(map[string]any{"addr": addr, "ok": true, "association": "established"})
		return
	}
	fmt.Printf("链路正常: %s（COTP/会话/表示/ACSE/MMS 五段握手完成）\n", addr)
}

func emitIECNames(addr, domain string, names []string, asJSON, raw bool) {
	type row struct {
		Object string `json:"object"`
		MMS    string `json:"mms,omitempty"`
	}
	rows := make([]row, 0, len(names))
	for _, n := range names {
		rows = append(rows, row{Object: iecDisplayName(domain, n), MMS: n})
	}
	if asJSON {
		emitJSON(map[string]any{"addr": addr, "domain": domain, "count": len(rows), "objects": rows})
		return
	}
	if domain != "" {
		fmt.Printf("%s 的命名变量（%d 个）\n", domain, len(rows))
	} else {
		fmt.Printf("VMD 的命名变量（%d 个）\n", len(rows))
	}
	for _, r := range rows {
		if raw {
			fmt.Printf("  %-48s %s\n", r.Object, r.MMS)
			continue
		}
		fmt.Printf("  %s\n", r.Object)
	}
}

// iecDisplayName 把 MMS 项名渲染成对象引用形态（`GGIO1$ST$Ind1$stVal` →
// `GGIO1.ST.Ind1.stVal`）。域为空（VMD）时项名本身就是逻辑节点名。
func iecDisplayName(domain, item string) string {
	ln, fc, path := ieccodec.SplitMMSName(item)
	if fc == "" && path == "" {
		// 项名只有一段：不是 `LN$FC$DO` 形态，原样返回。
		if domain != "" {
			return domain + "/" + item
		}
		return item
	}
	out := ln + "." + fc
	if path != "" {
		out += "." + path
	}
	if domain != "" {
		return domain + "/" + out
	}
	return out
}

func runIECRead(ctx context.Context, conn driver.Conn, points []model.Point,
	refs []ieccodec.ObjectRef, asJSON, raw bool) {
	spans := make([]driver.ReadSpan, 0, len(points))
	for i, p := range points {
		spans = append(spans, driver.ReadSpan{
			ID:     "s" + strconv.Itoa(i),
			Points: []driver.PointRef{driver.PointRefOf(model.Device{}, p)},
		})
	}
	samples, err := conn.Read(ctx, spans)

	type row struct {
		Object  string `json:"object"`
		MMS     string `json:"mms,omitempty"`
		Type    string `json:"type,omitempty"`
		Value   string `json:"value,omitempty"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(points))
	for i := range points {
		r := row{Quality: string(model.QualityBad)}
		if i < len(refs) {
			r.Object = refs[i].String()
			if raw {
				r.MMS = refs[i].ObjectName().MMSName()
			}
		}
		if i < len(samples) {
			s := samples[i]
			r.Quality = string(s.Quality)
			if s.Err != nil {
				r.Error = s.Err.Error()
			} else {
				// 打印**设备回的类型**，而不是 -type：这正是本协议与寄存器
				// 型协议的区别，也是自检最想看到的信息。
				r.Type = string(s.Value.Type)
				r.Value = iecValueText(s.Value)
			}
		} else {
			r.Error = "没有返回样本"
		}
		rows = append(rows, r)
	}

	if asJSON {
		if len(rows) == 1 {
			emitJSON(rows[0])
		} else {
			emitJSON(map[string]any{"objects": rows})
		}
	} else {
		for _, r := range rows {
			switch {
			case r.Error != "":
				fmt.Printf("%s: 失败（%s）: %s\n", r.Object, r.Quality, r.Error)
			case raw:
				fmt.Printf("%s = %s（%s，%s，MMS 项名 %s）\n", r.Object, r.Value, r.Type, r.Quality, r.MMS)
			default:
				fmt.Printf("%s = %s（%s，%s）\n", r.Object, r.Value, r.Type, r.Quality)
			}
		}
	}
	if err != nil {
		os.Exit(1)
	}
}

func runIECWrite(ctx context.Context, conn driver.Conn, p model.Point, value model.Value, asJSON bool) {
	results, err := conn.Write(ctx, []driver.WriteRequest{{
		PointID: p.ID, Address: p.Address, DataType: p.DataType, Value: value,
	}})
	type row struct {
		Object  string `json:"object"`
		Type    string `json:"type"`
		Value   string `json:"value"`
		OK      bool   `json:"ok"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	r := row{
		Object: p.Address.Path, Type: string(value.Type),
		Value: iecValueText(value), Quality: string(model.QualityBad),
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
		fmt.Printf("OK   %s ← %s（%s）\n", r.Object, r.Value, r.Type)
	} else {
		fmt.Printf("FAIL %s: %s\n", r.Object, r.Error)
	}
	if err != nil || !r.OK {
		os.Exit(1)
	}
}

// listIECObjects 打印内置从站（uic sim -protocol iec61850）的默认变量树。
//
// 真 IED 没有「列出所有变量」的通用入口，只能用 -browse 按逻辑设备枚举；
// 这条命令存在的意义是让自检流程能一眼看到「内置 IED 上有什么可读」。
func listIECObjects(asJSON bool) {
	objs := iecsim.DefaultObjects()
	names := make([]string, 0, len(objs))
	for name := range objs {
		names = append(names, name)
	}
	sort.Strings(names)

	type row struct {
		Object string `json:"object"`
		MMS    string `json:"mms"`
		Type   string `json:"type"`
	}
	rows := make([]row, 0, len(names))
	for _, full := range names {
		domain, item, ok := strings.Cut(full, "$")
		if !ok {
			continue
		}
		d := objs[full]
		rows = append(rows, row{
			Object: iecDisplayName(domain, item),
			MMS:    full,
			Type:   iecDataKind(d),
		})
	}
	if asJSON {
		emitJSON(map[string]any{"objects": rows})
		return
	}
	fmt.Println("内置 IEC 61850 IED 的默认对象表（uic sim -protocol iec61850）")
	printRow([]int{40, 34, 10}, "对象引用", "MMS 项名", "类型")
	for _, r := range rows {
		printRow([]int{40, 34, 10}, r.Object, r.MMS, r.Type)
	}
}

// iecDataKind 返回 MMS 数据的类型名，只用于展示。
func iecDataKind(d ieccodec.Data) string {
	if name := d.KindName(); name != "" {
		return name
	}
	return "?"
}

// iecValueText 把平台值渲染成人能读的文本。
func iecValueText(v model.Value) string {
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

// iecCLIValue 把命令行字符串解成平台值。
//
// 与点表侧的转换保持同一套规则：bool 接受 true/false/on/off/1/0，其余按
// 平台类型解析数字或文本。不做范围裁剪——越界应当由装置回
// object-value-invalid，而不是由命令行悄悄截断。
func iecCLIValue(s string, dt model.DataType) (model.Value, error) {
	text := strings.TrimSpace(s)
	switch dt {
	case model.TypeBool:
		switch strings.ToLower(text) {
		case "on", "true", "1", "yes", "开":
			return model.BoolValue(true), nil
		case "off", "false", "0", "no", "关":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("bool 只接受 true/false/on/off/1/0，得到 %q", s)
	case model.TypeString:
		return model.TextValue(dt, text), nil
	case model.TypeBytes:
		if strings.HasPrefix(strings.ToLower(text), "hex:") {
			raw := strings.ReplaceAll(strings.TrimSpace(text[4:]), " ", "")
			out := make([]byte, len(raw)/2)
			for i := 0; i < len(out); i++ {
				v, err := strconv.ParseUint(raw[i*2:i*2+2], 16, 8)
				if err != nil {
					return model.Value{}, fmt.Errorf("非法十六进制字节 %q", raw[i*2:i*2+2])
				}
				out[i] = byte(v)
			}
			return model.BytesValue(out), nil
		}
		return model.BytesValue([]byte(text)), nil
	}
	if text == "" {
		return model.Value{}, fmt.Errorf("值是空的")
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return model.Value{}, fmt.Errorf("不是数字: %q", s)
	}
	return model.NumValue(dt, f), nil
}
