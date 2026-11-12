package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	mstpdrv "uic/internal/driver/bacnetmstp"
	"uic/internal/model"
)

// uic mstpread 是 BACnet MS/TP 主站的自检命令。
//
// 与 bacread 同理由：MS/TP 的失败模式几乎全部表现为「没有应答」——
// 串口参数不对、A/B 线接反、从站 MAC 落在主站地址段、波特率不匹配、
// 终端电阻缺失。这些故障在商用调试软件里往往只显示一个红色叹号；而
// 一条命令读一个对象的 present-value，能把「物理层不通」「地址不对」
// 「对象属性写错」三类问题分开：
//
//   - 完全没字节回来 → 物理层/波特率/接线；
//   - 有字节但帧 CRC 错 → 干扰、终端电阻、地线；
//   - 帧对了但从站回 Error(unknown-object) → 对象实例号不对；
//   - 读到值 → 链路与编解码都正常。
//
// 边界与驱动一致：单主站轮询从站，不做令牌环维护、不做 Who-Is 发现、
// 不做批量读。一次只读/写一个对象属性，因为自检的目标是定位而不是采集。
func mstpreadCmd(args []string) {
	fs := flag.NewFlagSet("mstpread", flag.ExitOnError)

	// 承载：串口是常态；自检时也常用串口服务器，所以两种都支持。
	tr := fs.String("transport", "tcp", "传输：serial（串口）| tcp（串口服务器/DTU 透传）")
	addr := fs.String("addr", "127.0.0.1:4001", "从站地址 host[:port]（tcp），端口省略时按 4001")
	port := fs.String("port", "/dev/ttyUSB0", "串口设备路径（serial）")
	baud := fs.Int("baud", 9600, "波特率（serial）；MS/TP 常用 9600/19200/38400")
	dataBits := fs.Int("dataBits", 8, "数据位（serial）；MS/TP 固定 8")
	parity := fs.String("parity", "none", "校验：none | even | odd（serial）；MS/TP 固定 none")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（serial）；MS/TP 固定 1")

	// 寻址：MS/TP 的 MAC 与 BACnet 的对象地址是两层，必须分别给出。
	mac := fs.String("mac", "128", "从站 MAC（128..254；0..127 是主站地址段）")
	master := fs.String("master", "0", "本机主站 MAC（0..127）")
	next := fs.String("next-station", "", "可选：下一个主站 MAC（被动收到 Token 时交还）")
	presilence := fs.Duration("presilence", 5*time.Millisecond, "发送前线路静默时间")

	// 对象属性：与 bacread 完全一致。
	objType := fs.String("type", "analog-input", "对象类型：analog-input/analog-value/binary-input/multi-state-value/device 等，也可写编号")
	instance := fs.Int("instance", 1, "对象实例号")
	property := fs.String("property", "present-value", "属性名：present-value/object-name/units/priority-array 等，也可写编号")
	index := fs.Int("index", 0, "数组属性下标（0 = 不带下标，读整个属性）")
	dtype := fs.String("dtype", "", "读写时的平台数据类型（默认按对象类型推断）")
	write := fs.String("write", "", "写入值；bool 为 on|off，字符串原样，数值为十进制或 0x 十六进制")
	probe := fs.Bool("probe", false, "只读 device:1 的 object-name 验证链路")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	if *instance < 0 || *instance > 0x3FFFFF {
		fmt.Fprintln(os.Stderr, "-instance 必须在 0..4194302 之间")
		os.Exit(2)
	}
	if *index < 0 {
		fmt.Fprintln(os.Stderr, "-index 必须 >= 0")
		os.Exit(2)
	}
	if *presilence < 0 {
		fmt.Fprintln(os.Stderr, "-presilence 必须 >= 0")
		os.Exit(2)
	}
	if _, err := strconv.ParseUint(strings.TrimSpace(*mac), 0, 8); err != nil {
		fmt.Fprintf(os.Stderr, "-mac 不是合法整数: %v\n", err)
		os.Exit(2)
	}
	// 对象属性在连接之前就解析：类型/属性写错是静态配置错误，不该等建链后
	// 才在读取阶段暴露。
	address, label, dt, err := resolveMSTPPoint(*objType, *instance, *property, *index, *dtype)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	labels := map[string]string{
		"address":    *mac,
		"master":     *master,
		"presilence": presilence.String(),
	}
	if strings.TrimSpace(*next) != "" {
		labels["nextStation"] = *next
	}

	transport := model.Transport{Kind: *tr, Address: *addr, Port: *port,
		Baud: *baud, DataBits: *dataBits, Parity: *parity, StopBits: *stopBits}
	if *tr != "serial" && *tr != "tcp" {
		fmt.Fprintf(os.Stderr, "-transport 必须是 serial 或 tcp，得到 %q\n", *tr)
		os.Exit(2)
	}
	// 串口参数只在 serial 时有意义；tcp 时把琐碎字段清掉，避免打印出
	// 一份「看起来像串口」的配置误导现场。
	if *tr == "tcp" {
		transport = model.Transport{Kind: "tcp", Address: *addr}
	}

	ch := model.Channel{
		ID: "ch-bacnet-mstp", Protocol: "bacnet-mstp",
		Transport: transport, Concurrency: 1, Timeout: model.Duration(*timeout),
	}
	dev := model.Device{ID: "selfcheck", ChannelID: ch.ID,
		Interval: model.Duration(time.Second), Enabled: true, Labels: labels}

	d := mstpdrv.New()
	ctx := context.Background()
	c, err := d.Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if *probe {
		if err := c.Probe(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "探测失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("链路可用：从站 MAC %s 应答了 device:1 的 object-name\n", *mac)
		return
	}

	if *write != "" {
		val, err := parseBacnetValue(*write, dt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		res, err := c.Write(ctx, []driver.WriteRequest{{
			PointID: label, Address: address, DataType: dt, Value: val,
		}})
		if err != nil {
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
			os.Exit(1)
		}
		if len(res) == 0 || !res[0].OK {
			fmt.Fprintf(os.Stderr, "写入被拒绝: %v\n", res[0].Err)
			os.Exit(1)
		}
		fmt.Printf("写入 %s（从站 MAC %s）完成\n", label, *mac)
		return
	}

	samples, err := c.Read(ctx, []driver.ReadSpan{{
		ID:     "read",
		Points: []driver.PointRef{{ID: label, Address: address, DataType: dt}},
	}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	for _, s := range samples {
		if s.Err != nil {
			fmt.Printf("%s 错误（%s）: %v\n", s.PointID, s.Quality, s.Err)
			continue
		}
		fmt.Printf("%s（从站 MAC %s）= %v（%s）\n", s.PointID, *mac, s.Value, s.Quality)
	}
}

// bacnetObjectType 把对象类型名或编号解析成类型编号，非法时给出可选值提示。
func bacnetObjectType(s string) (int, error) {
	if typ, ok := bacnetcodec.ObjectTypeFromName(s); ok {
		return typ, nil
	}
	return 0, fmt.Errorf("未知对象类型 %q（可选：%s）", s,
		strings.Join(bacnetcodec.ObjectTypeNames(), " / "))
}

// bacnetPropertyID 把属性名或编号解析成属性编号。
func bacnetPropertyID(s string) (int, error) {
	if prop, ok := bacnetcodec.PropertyFromName(s); ok {
		return prop, nil
	}
	return 0, fmt.Errorf("未知属性 %q（可选：%s）", s,
		strings.Join(bacnetcodec.PropertyNames(), " / "))
}

// resolveMSTPPoint 把命令行上的对象类型/实例/属性/下标/数据类型组合解析成
// 平台地址、展示标签与平台值类型。
//
// 独立成函数是为了让「对象属性文本 → 结构化地址」这层纯逻辑能被单测直接覆盖，
// 不必真的去连一条串口。它与 bacread 共享 bacnetDataType / parseBacnetValue，
// 只是这里多了一层「类型名或编号」的翻译。
func resolveMSTPPoint(objType string, instance int, property string, index int, dtype string) (model.Address, string, model.DataType, error) {
	typ, err := bacnetObjectType(objType)
	if err != nil {
		return model.Address{}, "", "", err
	}
	prop, err := bacnetPropertyID(property)
	if err != nil {
		return model.Address{}, "", "", err
	}
	dt, err := bacnetDataType(dtype, typ, prop)
	if err != nil {
		return model.Address{}, "", "", err
	}
	address := model.Address{
		Kind: model.AddressObject, ObjectType: objType, Instance: instance,
		Property: property, PropIndex: index,
	}
	label := bacnetcodec.FormatAddress(typ, instance, prop, index)
	return address, label, dt, nil
}
