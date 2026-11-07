package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	bacnetdrv "uic/internal/driver/bacnet"
	"uic/internal/model"
)

// uic bacread 是一个极小的 BACnet/IP 主站自检命令。
//
// 与 mbread/mcread/finsread/enipread 同理由：现场最缺的是「不装任何第三方
// 软件就能自证链路通」的手段。BACnet 尤其如此——它是楼宇自控的标准协议，
// 但商用调试工具通常只在 Windows 上跑、要求先导入设备数据库。一行命令
// 读一个对象的 present-value，能把「连不上」与「地址写错」立刻区分开。
//
// 边界：只做单次属性读写，不做 Who-Is 发现、不做 COV 订阅、不做批量读。
func bacreadCmd(args []string) {
	fs := flag.NewFlagSet("bacread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:47808", "设备地址 host[:port]，端口默认 47808")
	objType := fs.String("type", "analog-input", "对象类型：analog-input/analog-value/binary-input/multi-state-value/device 等，也可写编号")
	instance := fs.Int("instance", 1, "对象实例号")
	property := fs.String("property", "present-value", "属性名：present-value/object-name/units/priority-array 等，也可写编号")
	index := fs.Int("index", 0, "数组属性下标（0 = 不带下标，读整个属性）")
	dtype := fs.String("dtype", "", "写入时的平台数据类型（默认按对象类型推断）")
	write := fs.String("write", "", "写入值；bool 为 on|off，字符串原样，数值为十进制或 0x 十六进制")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	fs.Parse(args)

	if *instance < 0 || *instance > 0x3FFFFF {
		fmt.Fprintln(os.Stderr, "-instance 必须在 0..4194302 之间")
		os.Exit(2)
	}
	if *index < 0 {
		fmt.Fprintln(os.Stderr, "-index 必须 >= 0")
		os.Exit(2)
	}
	typ, ok := bacnetcodec.ObjectTypeFromName(*objType)
	if !ok {
		fmt.Fprintf(os.Stderr, "未知对象类型 %q（可选：%s）\n", *objType,
			strings.Join(bacnetcodec.ObjectTypeNames(), " / "))
		os.Exit(2)
	}
	prop, ok := bacnetcodec.PropertyFromName(*property)
	if !ok {
		fmt.Fprintf(os.Stderr, "未知属性 %q（可选：%s）\n", *property,
			strings.Join(bacnetcodec.PropertyNames(), " / "))
		os.Exit(2)
	}

	dt, err := bacnetDataType(*dtype, typ, prop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	ch := model.Channel{
		ID: "ch-bacnet", Protocol: "bacnet",
		Transport: model.Transport{Kind: "udp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	d := bacnetdrv.New()
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{ID: "selfcheck", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	address := model.Address{
		Kind: model.AddressObject, ObjectType: *objType, Instance: *instance,
		Property: *property, PropIndex: *index,
	}
	label := bacnetcodec.FormatAddress(typ, *instance, prop, *index)

	if *write != "" {
		val, err := parseBacnetValue(*write, dt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		res, err := c.Write(context.Background(), []driver.WriteRequest{{
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
		fmt.Printf("写入 %s 完成\n", label)
		return
	}

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{
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
		fmt.Printf("%s = %v（%s）\n", s.PointID, s.Value, s.Quality)
	}
}

// bacnetDataType 决定读写时用哪个平台类型解释值。
//
// BACnet 的 present-value 类型由对象决定（模拟量 REAL、二进制量
// ENUMERATED、多状态量 UNSIGNED），因此默认按对象类型推断；object-name
// 等文本属性一律 string。-dtype 可以覆盖，用于读那些「属性类型与对象
// 类型不一致」的少数情况（例如 priority-array 的单元素）。
func bacnetDataType(explicit string, objType, property int) (model.DataType, error) {
	if explicit != "" {
		switch strings.ToLower(strings.TrimSpace(explicit)) {
		case "bool", "boolean":
			return model.TypeBool, nil
		case "int16":
			return model.TypeInt16, nil
		case "uint16":
			return model.TypeUint16, nil
		case "int32":
			return model.TypeInt32, nil
		case "uint32":
			return model.TypeUint32, nil
		case "int64":
			return model.TypeInt64, nil
		case "uint64":
			return model.TypeUint64, nil
		case "float32", "real":
			return model.TypeFloat32, nil
		case "float64", "double":
			return model.TypeFloat64, nil
		case "string":
			return model.TypeString, nil
		case "bytes":
			return model.TypeBytes, nil
		}
		return "", fmt.Errorf("未知类型 %q：可选 bool/int16/uint16/int32/uint32/int64/uint64/float32/float64/string/bytes", explicit)
	}
	if property == bacnetcodec.PropObjectName || property == bacnetcodec.PropModelName ||
		property == bacnetcodec.PropDescription || property == bacnetcodec.PropVendorName ||
		property == bacnetcodec.PropActiveText || property == bacnetcodec.PropInactiveText ||
		property == bacnetcodec.PropStateText {
		return model.TypeString, nil
	}
	switch bacnetcodec.KindOfObject(objType) {
	case bacnetcodec.KindAnalog:
		return model.TypeFloat32, nil
	case bacnetcodec.KindBinary:
		return model.TypeBool, nil
	case bacnetcodec.KindMultiState:
		return model.TypeUint32, nil
	case bacnetcodec.KindString:
		return model.TypeString, nil
	}
	// 未知对象类型（如 device、schedule）：属性多半是枚举或文本，
	// 用 float32 兜底能读出 units/priority-array 之类的数值属性。
	return model.TypeFloat32, nil
}

// parseBacnetValue 按数据类型解析写入值。
func parseBacnetValue(s string, dt model.DataType) (model.Value, error) {
	switch dt {
	case model.TypeBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "on", "1", "true", "yes", "active":
			return model.BoolValue(true), nil
		case "off", "0", "false", "no", "inactive":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("bool 写入值必须是 on|off|1|0，得到 %q", s)
	case model.TypeString:
		return model.TextValue(dt, s), nil
	case model.TypeBytes:
		if strings.HasPrefix(strings.ToLower(s), "hex:") {
			raw := strings.ReplaceAll(strings.TrimSpace(s[4:]), " ", "")
			out := make([]byte, len(raw)/2)
			if len(raw)%2 != 0 {
				return model.Value{}, fmt.Errorf("hex 字节串长度必须为偶数，得到 %d 个字符", len(raw))
			}
			for i := 0; i < len(out); i++ {
				var v uint64
				for _, c := range raw[i*2 : i*2+2] {
					d, ok := hexDigit(c)
					if !ok {
						return model.Value{}, fmt.Errorf("非法十六进制字符 %q", string(c))
					}
					v = v<<4 | uint64(d)
				}
				out[i] = byte(v)
			}
			return model.BytesValue(out), nil
		}
		return model.BytesValue([]byte(s)), nil
	}
	n, err := parseNumber(s)
	if err != nil {
		return model.Value{}, err
	}
	return model.NumValue(dt, n), nil
}

func hexDigit(c rune) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}
