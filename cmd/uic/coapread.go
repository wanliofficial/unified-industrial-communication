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

	coapcodec "uic/internal/codec/coap"
	"uic/internal/codec/textval"
	"uic/internal/driver"
	coapdrv "uic/internal/driver/coap"
	"uic/internal/model"
)

// uic coapread 是一个极小的 CoAP 主站自检命令。
//
// CoAP 与寄存器型协议不同：数据模型是「资源」，每个点位就是一个 URI。
// 因此自检的第一件事往往不是「读我配置的点」，而是「这台设备到底发布
// 了哪些资源」——那正是 `-discover`。`-probe` 用 `/.well-known/core`
// 区分「能被发现」与「连不上」，因为在 UDP 上，没有响应既可能是设备不在，
// 也可能是端口对但资源不存在。
//
// 它也能写：`-write` 按 `-type` 解释值，`-content-format` 决定发 text/
// plain、application/json 还是 application/octet-stream。带 `-selector`
// 时只发送该字段的子树（PUT 是替换资源，不做隐藏的读-改-写）。
//
// 边界与驱动一致：明文 UDP、GET/PUT/POST、Block2 接收、资源发现；不做
// DTLS、不做 Observe 订阅、不做 Block1 分块发送。
func coapreadCmd(args []string) {
	fs := flag.NewFlagSet("coapread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5683", "服务端地址 host[:port]，端口默认 5683")
	path := fs.String("path", "/sensors/temp", "资源路径（省略前导斜杠会被补上）")
	selector := fs.String("selector", "", "响应为 JSON 时取哪个字段，支持 a.b 与 a[0]")
	typ := fs.String("type", "float64", "数据类型：bool|int16|uint16|int32|uint32|int64|uint64|float32|float64|string|bytes")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	block := fs.Int("block", 0, "首选块大小 0|16|32|64|128|256|512|1024（0=跟随服务端）")
	probe := fs.Bool("probe", false, "只探测 /.well-known/core，不读资源")
	discover := fs.Bool("discover", false, "列出设备发布的资源（link-format）")
	write := fs.String("write", "", "写入值（按 -type 解释；bytes 按字面字节）")
	payload := fs.String("payload", "", "写入原始载荷（text/octets 按字面发送，覆盖 -write）")
	method := fs.String("method", "put", "写方法 put|post")
	format := fs.String("content-format", "text", "载荷格式 text|json|octets")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	raw := fs.Bool("raw", false, "bytes 值以十六进制显示，并保留原始载荷")
	fs.Parse(args)

	writeSet, payloadSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "write":
			writeSet = true
		case "payload":
			payloadSet = true
		}
	})

	if !validCoAPType(*typ) && !payloadSet {
		fmt.Fprintf(os.Stderr, "type: 不支持 %q\n", *typ)
		os.Exit(2)
	}
	if *block != 0 {
		if !validCoAPBlock(*block) {
			fmt.Fprintf(os.Stderr, "block: 必须是 0 或 16/32/64/128/256/512/1024，得到 %d\n", *block)
			os.Exit(2)
		}
	}

	labels := map[string]string{
		"contentFormat": normalizeCoAPFormat(*format),
		"writeMethod":   strings.ToLower(*method),
		"blockSize":     strconv.Itoa(*block),
	}

	// 写原始载荷时按 string 点位发送，确保驱动不会把字面字节再解释一次。
	pointType := *typ
	if payloadSet {
		pointType = string(model.TypeString)
	}
	point := model.Point{
		ID: "cli", Enabled: true, DataType: model.DataType(pointType),
		Address: model.Address{Kind: model.AddressSymbol, Path: *path, Selector: *selector},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	defer cancel()

	conn, err := openCoAP(ctx, *addr, *timeout, labels, []model.Point{point})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if *discover {
		runCoAPDiscover(ctx, conn, *addr, *asJSON)
		return
	}
	if *probe {
		if err := conn.Probe(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "探测失败: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			emitJSON(map[string]any{"addr": *addr, "link": "up"})
			return
		}
		fmt.Printf("链路正常: %s（/.well-known/core 有响应）\n", *addr)
		return
	}

	if writeSet || payloadSet {
		var value model.Value
		if payloadSet {
			value = model.TextValue(model.TypeString, *payload)
		} else {
			v, err := coapCLIValue(*write, *typ)
			if err != nil {
				fmt.Fprintf(os.Stderr, "-write %q: %v\n", *write, err)
				os.Exit(2)
			}
			value = v
		}
		runCoAPWrite(ctx, conn, point, value, strings.ToUpper(labels["writeMethod"]), *asJSON)
		return
	}

	runCoAPRead(ctx, conn, point, *asJSON, *raw)
}

func openCoAP(ctx context.Context, addr string, timeout time.Duration,
	labels map[string]string, points []model.Point) (driver.Conn, error) {
	dev := model.Device{
		ID: "cli", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(timeout),
		Labels:   labels,
		Points:   points,
	}
	ch := model.Channel{
		ID: "ch", Protocol: "coap",
		Transport: model.Transport{Kind: "udp", Address: addr},
		Timeout:   model.Duration(timeout),
	}
	return coapdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
}

func runCoAPRead(ctx context.Context, conn driver.Conn, p model.Point, asJSON, raw bool) {
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
		Path     string `json:"path"`
		Selector string `json:"selector,omitempty"`
		Type     string `json:"type"`
		Value    string `json:"value,omitempty"`
		Quality  string `json:"quality"`
		Error    string `json:"error,omitempty"`
	}
	r := row{
		Path:     p.Address.Path,
		Selector: p.Address.Selector,
		Type:     string(p.DataType),
		Quality:  string(s.Quality),
	}
	if s.Err != nil {
		r.Error = s.Err.Error()
	} else {
		r.Value = coapValueText(s.Value, raw)
	}

	if asJSON {
		emitJSON(r)
	} else if s.Err != nil {
		fmt.Printf("%s: 失败（%s）: %s\n", r.Path, r.Quality, r.Error)
	} else {
		fmt.Printf("%s = %s（%s）\n", r.Path, r.Value, r.Quality)
	}
	if err != nil {
		os.Exit(1)
	}
}

func runCoAPWrite(ctx context.Context, conn driver.Conn, p model.Point, value model.Value, method string, asJSON bool) {
	results, err := conn.Write(ctx, []driver.WriteRequest{{
		PointID: p.ID, Address: p.Address, DataType: p.DataType, Value: value,
	}})
	type row struct {
		Path    string `json:"path"`
		Method  string `json:"method"`
		OK      bool   `json:"ok"`
		Quality string `json:"quality"`
		Error   string `json:"error,omitempty"`
	}
	r := row{Path: p.Address.Path, Method: method, Quality: string(model.QualityBad)}
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
		fmt.Printf("OK   %s %s\n", r.Method, r.Path)
	} else {
		fmt.Printf("FAIL %s: %s\n", r.Path, r.Error)
	}
	if err != nil || !r.OK {
		os.Exit(1)
	}
}

func runCoAPDiscover(ctx context.Context, conn driver.Conn, addr string, asJSON bool) {
	d, ok := conn.(interface {
		Discover(context.Context) ([]coapcodec.Link, error)
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "内部错误: 驱动不提供资源发现")
		os.Exit(1)
	}
	links, err := d.Discover(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "发现失败: %v\n", err)
		os.Exit(1)
	}

	if asJSON {
		type link struct {
			Path   string `json:"path"`
			Type   string `json:"rt,omitempty"`
			Format string `json:"ct,omitempty"`
			Flags  string `json:"flags,omitempty"`
		}
		out := make([]link, 0, len(links))
		for _, l := range links {
			fl := []string{}
			for k := range l.Params {
				if l.Params[k] == "" {
					fl = append(fl, k)
				}
			}
			out = append(out, link{
				Path:   l.Path,
				Type:   l.Params["rt"],
				Format: l.Params["ct"],
				Flags:  strings.Join(fl, ","),
			})
		}
		emitJSON(map[string]any{"addr": addr, "resources": out})
		return
	}

	fmt.Printf("设备 %s 发布 %d 个资源\n", addr, len(links))
	printRow([]int{32, 22, 8}, "路径", "资源类型", "格式")
	for _, l := range links {
		printRow([]int{32, 22, 8}, l.Path, l.Params["rt"], l.Params["ct"])
	}
}

func coapValueText(v model.Value, raw bool) string {
	switch v.Type {
	case model.TypeBool:
		return strconv.FormatBool(v.Bool)
	case model.TypeString:
		return v.Text
	case model.TypeBytes:
		if raw {
			return hex.EncodeToString([]byte(v.Text))
		}
		return fmt.Sprintf("%q", v.Text)
	default:
		return strconv.FormatFloat(v.Num, 'g', -1, 64)
	}
}

// coapCLIValue 把命令行字符串解成平台值。bytes 按字面字节，其余走与
// MQTT/CoAP 相同的文本规则——同一个 "on" 在这里和网关里解析成一样的东西。
func coapCLIValue(s, dt string) (model.Value, error) {
	if dt == string(model.TypeBytes) {
		return model.BytesValue([]byte(s)), nil
	}
	return textval.Decode([]byte(s), model.DataType(dt))
}

func validCoAPType(t string) bool {
	switch model.DataType(t) {
	case model.TypeBool, model.TypeInt16, model.TypeUint16, model.TypeInt32,
		model.TypeUint32, model.TypeInt64, model.TypeUint64, model.TypeFloat32,
		model.TypeFloat64, model.TypeString, model.TypeBytes:
		return true
	}
	return false
}

func validCoAPBlock(n int) bool {
	for szx := 0; szx <= 6; szx++ {
		if n == 1<<(szx+4) {
			return true
		}
	}
	return false
}

func normalizeCoAPFormat(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "json", "application/json":
		return "json"
	case "octets", "binary", "application/octet-stream":
		return "octets"
	default:
		return "text"
	}
}
