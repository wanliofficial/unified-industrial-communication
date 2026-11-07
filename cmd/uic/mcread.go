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
	mcdrv "uic/internal/driver/mc"
	"uic/internal/model"
)

// uic mcread 是一个极小的 MC 3E 主站自检命令。
//
// 与 mbread 同理由：现场最缺的是「不装任何第三方软件就能自证链路通」的
// 手段。三菱没有 curl，商业工具要求先装软件再点几十下；一行命令就能把
// 链路验证变成可复制、可贴进工单的东西。
//
// 边界：只做单次读写，不做轮询、不解析设备型号。
func mcreadCmd(args []string) {
	fs := flag.NewFlagSet("mcread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5000", "PLC 地址 host:port")
	area := fs.String("area", "d", "软元件区：d/w/r/zr/m/x/y/b/f/l 等")
	offset := fs.Int("offset", 0, "起始编号（线上数值的十进制，X1F 填 31）")
	qty := fs.Int("qty", 1, "读取数量（字或位）")
	bit := fs.Int("bit", -1, "字软元件上的位号 0..15；位软元件忽略")
	write := fs.String("write", "", "写入值：字软元件为十进制/0x 十六进制；位软元件为 on|off|1|0")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	ch := model.Channel{
		ID: "ch-mc", Protocol: "mc",
		Transport: model.Transport{Kind: "tcp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	c, err := mcdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "selfcheck", ChannelID: ch.ID, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	areaLower := toLower(*area)
	bitDevice := isBitArea(areaLower)

	if *write != "" {
		val, err := parseWriteValue(*write, bitDevice)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		res, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID:  "write",
			Address:  model.Address{Kind: model.AddressRegister, Area: areaLower, Offset: *offset, Bit: *bit, Length: *qty},
			DataType: writeType(bitDevice),
			Value:    val,
		}})
		if err != nil {
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("写入 %s%d 完成（ok=%v）\n", areaLower, *offset, res[0].OK)
		return
	}

	span := driver.ReadSpan{ID: "read"}
	if bitDevice {
		// 位软元件：每位一个 bool 点位。
		for i := 0; i < *qty; i++ {
			span.Points = append(span.Points, driver.PointRef{
				ID:       fmt.Sprintf("%s%d", areaLower, *offset+i),
				Address:  model.Address{Kind: model.AddressRegister, Area: areaLower, Offset: *offset + i, Bit: -1},
				DataType: model.TypeBool,
			})
		}
	} else {
		// 字软元件：每字一个 uint16 点位。多字数值类型（float32 等）由
		// 配置文件里的点位覆盖，自检命令只看单字，避免把解码语义写死。
		for i := 0; i < *qty; i++ {
			span.Points = append(span.Points, driver.PointRef{
				ID:       fmt.Sprintf("%s%d", areaLower, *offset+i),
				Address:  model.Address{Kind: model.AddressRegister, Area: areaLower, Offset: *offset + i, Length: 1, Bit: -1},
				DataType: model.TypeUint16,
			})
		}
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	for _, s := range samples {
		if s.Err != nil {
			fmt.Printf("%s 错误: %v\n", s.PointID, s.Err)
			continue
		}
		fmt.Printf("%s = %v\n", s.PointID, s.Value)
	}
}

func toLower(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 32
		}
	}
	return string(out)
}

func isBitArea(area string) bool {
	switch area {
	case "m", "x", "y", "b", "f", "l", "sm", "sb", "s":
		return true
	}
	return false
}

func writeType(bit bool) model.DataType {
	if bit {
		return model.TypeBool
	}
	return model.TypeUint16
}

func parseWriteValue(s string, bit bool) (model.Value, error) {
	if bit {
		switch s {
		case "on", "1", "true", "ON":
			return model.BoolValue(true), nil
		case "off", "0", "false", "OFF":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("位写入值必须是 on|off|1|0，得到 %q", s)
	}
	n, err := parseNumber(s)
	if err != nil {
		return model.Value{}, err
	}
	return model.NumValue(model.TypeUint16, n), nil
}

// parseNumber 支持十进制与 0x 十六进制。MC 的地址与寄存器值在文档里习惯
// 用十六进制书写（D100、0x1234），命令里两种都要能认。
func parseNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		n, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil {
			return 0, fmt.Errorf("非法十六进制 %q: %w", s, err)
		}
		return float64(n), nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("非法数字 %q: %w", s, err)
	}
	return n, nil
}
