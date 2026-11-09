package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	secscodec "uic/internal/codec/secs"
	"uic/internal/codec/textval"
	"uic/internal/driver"
	secsdrv "uic/internal/driver/secs"
	"uic/internal/model"
)

// uic secsread 是一个极小的 SECS/GEM 主站自检命令。
//
// SECS 的寻址是「对象类型 + 编号」：状态变量（SVID）与设备常数（ECID）都是
// 32 位无符号整数，没有可以打出来的符号名。因此自检的第一步不是「读哪个
// 地址」，而是「这台设备有哪些编号、它们叫什么」——那正是 `-describe`
// （S1F11/S1F12）。跳过它直接读，现场最常见的失败是「编号猜错」和「点表
// 声明的类型与设备回的格式不符」，两者都在 `-raw` 下能一眼看出来。
//
// 默认读 SVID（`-object svid`），因为那是设备状态的入口；`-object ecid`
// 读设备常数，也只有它可以写：S2F15/S2F16，ACKC7=0 视为接受。SVID 在
// SECS/GEM 里没有写语义，写它要走远程命令流程，本驱动不做。
//
// 边界与驱动一致：只做 HSMS/TCP 主站、S1/S2/S5/S6 的少量必备消息；不做
// 时间设置（S2F17/S2F31）、不做报告定义与链接（S2F33~S2F45）。
func secsreadCmd(args []string) {
	fs := flag.NewFlagSet("secsread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5000", "设备地址 host[:port]，端口默认 5000")
	object := fs.String("object", "svid", "对象类型：svid（状态变量，只读）| ecid（设备常数，可写）")
	ids := fs.String("id", "1", "编号，可用逗号分隔多个（如 1,2,3）")
	typ := fs.String("type", "uint32", "数据类型：bool|int16|uint16|int32|uint32|int64|uint64|float32|float64|string|bytes")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	commTimeout := fs.Duration("comm-timeout", 5*time.Second, "Select 握手超时（Open 内完成）")
	retries := fs.Int("retries", 1, "事务超时后的重发次数 0..5")
	probe := fs.Bool("probe", false, "只做 Linktest 探活，不读编号")
	describe := fs.Bool("describe", false, "用 S1F11/S1F12 查询状态变量的名称与单位")
	raw := fs.Bool("raw", false, "读取原始 SECS-II 项（不做类型转换，直接看设备回什么格式）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")

	write := fs.String("write", "", "写入值（仅 ecid；按 -type 解释）")
	fs.Parse(args)

	hasWrite := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "write" {
			hasWrite = true
		}
	})

	obj := strings.ToLower(strings.TrimSpace(*object))
	if obj != "svid" && obj != "ecid" {
		fmt.Fprintf(os.Stderr, "object: 不支持 %q（svid | ecid）\n", *object)
		os.Exit(2)
	}
	if hasWrite && obj != "ecid" {
		fmt.Fprintln(os.Stderr, "SECS/GEM 的状态变量（svid）只读：写设备常数请加 -object ecid")
		os.Exit(2)
	}
	if *retries < 0 || *retries > 5 {
		fmt.Fprintf(os.Stderr, "retries: 必须在 0..5，得到 %d\n", *retries)
		os.Exit(2)
	}
	list, err := parseSECSIDs(*ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "id: %v\n", err)
		os.Exit(2)
	}
	if hasWrite && len(list) != 1 {
		fmt.Fprintln(os.Stderr, "写入只支持一个编号：S2F16 只对整个批次回一个 ACKC7，多编号写无法区分是哪一个被拒")
		os.Exit(2)
	}
	if !hasWrite && !*probe && !validSECSType(*typ) && !*raw {
		fmt.Fprintf(os.Stderr, "type: 不支持 %q\n", *typ)
		os.Exit(2)
	}
	var writeValue model.Value
	if hasWrite {
		v, err := textval.Decode([]byte(*write), model.DataType(*typ))
		if err != nil {
			fmt.Fprintf(os.Stderr, "-write %q: %v\n", *write, err)
			os.Exit(2)
		}
		writeValue = v
	}

	labels := map[string]string{
		"commTimeout": commTimeout.String(),
		"retries":     strconv.Itoa(*retries),
	}

	// 探活与枚举不依赖具体点位，用空点表打开即可。
	points := make([]model.Point, 0, len(list))
	for _, id := range list {
		points = append(points, secsPoint(obj, id, model.DataType(*typ), hasWrite))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+*commTimeout+10*time.Second)
	defer cancel()

	conn, err := openSECS(ctx, *addr, *timeout, labels, points)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if *probe {
		if err := conn.Probe(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "探测失败: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			emitJSON(map[string]any{"addr": *addr, "link": "up"})
			return
		}
		fmt.Printf("链路正常: %s（Linktest 有响应）\n", *addr)
		return
	}

	if *describe {
		runSECSDescribe(ctx, conn, list, *asJSON)
		return
	}

	if hasWrite {
		runSECSWrite(ctx, conn, secsPoint(obj, list[0], model.DataType(*typ), true), writeValue, *asJSON)
		return
	}

	if *raw {
		runSECSRaw(ctx, conn, obj, list, *asJSON)
		return
	}

	runSECSRead(ctx, conn, *addr, points, obj, *typ, *asJSON)
}

func openSECS(ctx context.Context, addr string, timeout time.Duration,
	labels map[string]string, points []model.Point) (driver.Conn, error) {
	dev := model.Device{
		ID: "cli", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Labels:   labels,
		Points:   points,
	}
	ch := model.Channel{
		ID: "ch", Protocol: "secs",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(timeout),
	}
	return secsdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
}

func secsPoint(object string, id uint32, dt model.DataType, writable bool) model.Point {
	return model.Point{
		ID:       fmt.Sprintf("%s-%d", object, id),
		Enabled:  true,
		DataType: dt,
		Writable: writable,
		Address: model.Address{
			Kind: model.AddressObject, ObjectType: object,
			Instance: int(id), Property: "value",
		},
	}
}

func runSECSRead(ctx context.Context, conn driver.Conn, addr string, points []model.Point,
	object, typ string, asJSON bool) {
	refs := make([]driver.PointRef, 0, len(points))
	for _, p := range points {
		refs = append(refs, driver.PointRefOf(model.Device{}, p))
	}
	samples, err := conn.Read(ctx, []driver.ReadSpan{{ID: "cli", Points: refs}})

	type row struct {
		Object  string `json:"object"`
		ID      int    `json:"id"`
		Type    string `json:"type"`
		Format  string `json:"format,omitempty"`
		Value   string `json:"value,omitempty"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(points))
	byID := map[string]driver.RawSample{}
	for _, s := range samples {
		byID[s.PointID] = s
	}
	for _, p := range points {
		r := row{Object: object, ID: p.Address.Instance, Type: typ, Quality: string(model.QualityBad)}
		s, ok := byID[p.ID]
		if !ok {
			r.Error = "没有返回样本"
		} else {
			r.Quality = string(s.Quality)
			if s.Err != nil {
				r.Error = s.Err.Error()
			} else {
				r.Value = secsValueText(s.Value)
			}
		}
		rows = append(rows, r)
	}

	if asJSON {
		emitJSON(map[string]any{"addr": addr, "object": object, "rows": rows})
	} else {
		printRow([]int{8, 10, 12, 24, 10}, "对象", "编号", "类型", "值", "质量")
		for _, r := range rows {
			v := r.Value
			if r.Error != "" {
				v = "失败: " + r.Error
			}
			printRow([]int{8, 10, 12, 24, 10},
				r.Object, strconv.Itoa(r.ID), r.Type, v, r.Quality)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
}

func runSECSRaw(ctx context.Context, conn driver.Conn, object string, ids []uint32, asJSON bool) {
	r, ok := conn.(secsdrv.Inspector)
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供原始项读取")
		os.Exit(1)
	}

	type row struct {
		Object string `json:"object"`
		ID     int    `json:"id"`
		Format string `json:"format,omitempty"`
		Text   string `json:"text,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(ids))
	for _, id := range ids {
		rw := row{Object: object, ID: int(id)}
		it, err := r.RawRead(ctx, object, id)
		if err != nil {
			rw.Error = err.Error()
		} else {
			rw.Format = secscodec.FormatName(it.Format)
			rw.Text = it.Text()
		}
		rows = append(rows, rw)
	}

	if asJSON {
		emitJSON(map[string]any{"object": object, "rows": rows})
		return
	}
	printRow([]int{8, 10, 10, 44}, "对象", "编号", "格式", "原始项")
	for _, rw := range rows {
		text := rw.Text
		if rw.Error != "" {
			text = "失败: " + rw.Error
		}
		printRow([]int{8, 10, 10, 44}, rw.Object, strconv.Itoa(rw.ID), rw.Format, text)
	}
	for _, rw := range rows {
		if rw.Error != "" {
			os.Exit(1)
		}
	}
}

func runSECSWrite(ctx context.Context, conn driver.Conn, p model.Point,
	value model.Value, asJSON bool) {
	results, err := conn.Write(ctx, []driver.WriteRequest{{
		PointID: p.ID, Address: p.Address, DataType: p.DataType, Value: value,
	}})

	type row struct {
		Object  string `json:"object"`
		ID      int    `json:"id"`
		OK      bool   `json:"ok"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	r := row{Object: p.Address.ObjectType, ID: p.Address.Instance, Quality: string(model.QualityBad)}
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
		fmt.Printf("OK   %s %d = %s\n", r.Object, r.ID, secsValueText(value))
	} else {
		fmt.Printf("FAIL %s %d: %s\n", r.Object, r.ID, r.Error)
	}
	if err != nil || !r.OK {
		if err != nil && !asJSON {
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		}
		os.Exit(1)
	}
}

func runSECSDescribe(ctx context.Context, conn driver.Conn, ids []uint32, asJSON bool) {
	d, ok := conn.(secsdrv.Inspector)
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供 S1F11 查询")
		os.Exit(1)
	}
	vars, err := d.Describe(ctx, ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
		os.Exit(1)
	}

	if asJSON {
		type row struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			Units string `json:"units,omitempty"`
		}
		out := make([]row, 0, len(vars))
		for _, v := range vars {
			out = append(out, row{ID: int(v.ID), Name: v.Name, Units: v.Units})
		}
		emitJSON(map[string]any{"variables": out})
		return
	}
	fmt.Printf("设备声明 %d 个状态变量\n", len(vars))
	printRow([]int{10, 28, 12}, "编号", "名称", "单位")
	for _, v := range vars {
		printRow([]int{10, 28, 12}, strconv.Itoa(int(v.ID)), v.Name, v.Units)
	}
	// 请求了却没回：说明该编号在设备上不存在，值得显式提示而不是静默少一行。
	got := map[uint32]bool{}
	for _, v := range vars {
		got[v.ID] = true
	}
	for _, id := range ids {
		if !got[id] {
			fmt.Printf("（设备未返回编号 %d）\n", id)
		}
	}
}

func secsValueText(v model.Value) string {
	switch v.Type {
	case model.TypeBool:
		return strconv.FormatBool(v.Bool)
	case model.TypeString:
		return v.Text
	case model.TypeBytes:
		return fmt.Sprintf("%x", []byte(v.Text))
	default:
		return strconv.FormatFloat(v.Num, 'g', -1, 64)
	}
}

func parseSECSIDs(s string) ([]uint32, error) {
	fields := strings.Split(s, ",")
	out := make([]uint32, 0, len(fields))
	seen := map[uint32]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q 不是 32 位无符号整数", f)
		}
		if n == 0 {
			return nil, fmt.Errorf("编号 0 在 SECS 里不是合法寻址")
		}
		if seen[uint32(n)] {
			continue
		}
		seen[uint32(n)] = true
		out = append(out, uint32(n))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少给一个编号")
	}
	return out, nil
}

func validSECSType(t string) bool {
	switch model.DataType(t) {
	case model.TypeBool, model.TypeInt16, model.TypeUint16, model.TypeInt32,
		model.TypeUint32, model.TypeInt64, model.TypeUint64, model.TypeFloat32,
		model.TypeFloat64, model.TypeString, model.TypeBytes:
		return true
	}
	return false
}
