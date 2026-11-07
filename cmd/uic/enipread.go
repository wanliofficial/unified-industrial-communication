package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"uic/internal/driver"
	enipdrv "uic/internal/driver/enip"
	"uic/internal/model"
)

// uic enipread 是一个极小的 EtherNet/IP CIP 主站自检命令。
//
// 与 mbread/mcread/finsread 同理由：现场最缺的是「不装任何第三方软件就能
// 自证链路通」的手段。CIP 比 Modbus 多两层封装（RegisterSession + CPF），
// 任何一层错位都表现为「连上了但读不到」，普通 ping/telnet 看不出来。
//
// 边界：只做单次标签读写，不做轮询、不做隐式 I/O、不做对象/属性寻址。
func enipreadCmd(args []string) {
	fs := flag.NewFlagSet("enipread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:44818", "PLC 地址 host[:port]，端口默认 44818")
	tag := fs.String("tag", "DINT_TAG", "标签名（大小写不敏感），如 Motor1_Speed")
	typ := fs.String("type", "dint", "数据类型：bool/sint/int/dint/lint/usint/uint/udint/ulint/real/lreal/string/bytes")
	offset := fs.Int("offset", 0, "起始元素下标（数组标签）")
	length := fs.Int("length", 0, "bytes 的字节数 / string 的字符容量；数值类型忽略")
	bit := fs.Int("bit", -1, "bool 点位：-1 表示标签本身是 BOOL；>=0 表示从整数标签取位并 RMW 写回")
	write := fs.String("write", "", "写入值：bool 为 on|off；字符串/bytes 为文本（bytes 支持 hex:0102）；数值为十进制或 0x 十六进制")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	fs.Parse(args)

	dt, err := parseENIPType(*typ)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *offset < 0 {
		fmt.Fprintln(os.Stderr, "-offset 必须 >= 0")
		os.Exit(2)
	}
	if *length < 0 {
		fmt.Fprintln(os.Stderr, "-length 必须 >= 0")
		os.Exit(2)
	}
	if dt == model.TypeBytes && *length <= 0 {
		fmt.Fprintln(os.Stderr, "bytes 类型必须显式给 -length（字节数）")
		os.Exit(2)
	}

	ch := model.Channel{
		ID: "ch-enip", Protocol: "enip",
		Transport: model.Transport{Kind: "tcp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	c, err := enipdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{ID: "selfcheck", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	address := model.Address{Kind: model.AddressRegister, Area: *tag,
		Offset: *offset, Length: *length, Bit: *bit}

	if *write != "" {
		val, err := parseENIPValue(*write, dt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		res, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: *tag, Address: address, DataType: dt, Value: val,
		}})
		if err != nil {
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("写入 %s 完成（ok=%v）\n", *tag, res[0].OK)
		return
	}

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "read",
		Points: []driver.PointRef{{
			ID: *tag, Address: address, DataType: dt,
		}},
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

// parseENIPType 把命令行里的类型名映射到平台类型。
//
// 同时接受 CIP 名（DINT）与平台名（int32）：现场文档里两种写法都常见，
// 强迫运维记住其中一种只会制造口误。
func parseENIPType(s string) (model.DataType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bool", "boolean":
		return model.TypeBool, nil
	case "sint", "int8", "usint", "uint8", "byte", "bytes":
		// 平台没有 1 字节整数类型：SINT/USINT 走 bytes（-length 1），
		// 驱动会先从应答里学实际宽度再取值，不会把 1 字节当 2 字节。
		return model.TypeBytes, nil
	case "int", "int16":
		return model.TypeInt16, nil
	case "uint", "uint16", "word":
		return model.TypeUint16, nil
	case "dint", "int32":
		return model.TypeInt32, nil
	case "udint", "uint32", "dword":
		return model.TypeUint32, nil
	case "lint", "int64":
		return model.TypeInt64, nil
	case "ulint", "uint64", "lword":
		return model.TypeUint64, nil
	case "real", "float32":
		return model.TypeFloat32, nil
	case "lreal", "float64":
		return model.TypeFloat64, nil
	case "string":
		return model.TypeString, nil
	}
	return "", fmt.Errorf("未知类型 %q：可选 bool/sint/int/dint/lint/usint/uint/udint/ulint/real/lreal/string/bytes", s)
}

// parseENIPValue 按数据类型解析写入值。
//
// 数值统一走 parseNumber（十进制 + 0x 十六进制）；bool 接受 on/off 与 0/1；
// string/bytes 原样作为文本，bytes 额外支持 hex: 前缀，避免不可打印字符
// 只能靠 shell 转义。
func parseENIPValue(s string, dt model.DataType) (model.Value, error) {
	switch dt {
	case model.TypeBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "on", "1", "true", "yes":
			return model.BoolValue(true), nil
		case "off", "0", "false", "no":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("bool 写入值必须是 on|off|1|0，得到 %q", s)
	case model.TypeString:
		return model.TextValue(dt, s), nil
	case model.TypeBytes:
		if strings.HasPrefix(strings.ToLower(s), "hex:") {
			raw := strings.ReplaceAll(strings.TrimSpace(s[4:]), " ", "")
			if len(raw)%2 != 0 {
				return model.Value{}, fmt.Errorf("hex 字节串长度必须为偶数，得到 %d 个字符", len(raw))
			}
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
		return model.BytesValue([]byte(s)), nil
	}
	n, err := parseNumber(s)
	if err != nil {
		return model.Value{}, err
	}
	return model.NumValue(dt, n), nil
}
