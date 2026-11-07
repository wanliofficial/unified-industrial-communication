package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"uic/internal/driver"
	finsdrv "uic/internal/driver/fins"
	"uic/internal/model"
)

// uic finsread 是一个极小的 FINS/TCP 主站自检命令。
//
// 与 mbread/mcread 同理由：现场最缺的是「不装任何第三方软件就能自证链路
// 通」的手段。FINS 还要多一步节点号协商，这一步出问题时普通 ping/telnet
// 看不出来；一行命令把「能连上」和「能读到数」分开验证。
//
// 边界：只做单次内存区读写，不做轮询、不切换 PLC 运行模式。
func finsreadCmd(args []string) {
	fs := flag.NewFlagSet("finsread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9600", "PLC 地址 host[:port]，端口默认 9600")
	area := fs.String("area", "dm", "内存区：cio/wr/hr/ar/dm/em/em0..em12，支持 d/w/h/a 简写")
	offset := fs.Int("offset", 0, "字地址（CIO0.3 的 offset 是 0）")
	bit := fs.Int("bit", -1, "位号 0..15；>=0 时读写单个位")
	qty := fs.Int("qty", 1, "读取字数（位访问时忽略）")
	node := fs.Int("node", 0, "目标节点号；0 = 使用握手协商值")
	write := fs.String("write", "", "写入值：字为十进制/0x 十六进制；位为 on|off|1|0")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	ch := model.Channel{
		ID: "ch-fins", Protocol: "fins",
		Transport: model.Transport{Kind: "tcp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	c, err := finsdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{ID: "selfcheck", ChannelID: ch.ID, UnitID: *node,
			Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	areaLower := toLower(*area)
	bitAccess := *bit >= 0

	if *write != "" {
		if !bitAccess && *qty > 1 {
			// 字写只带一个值；把 -qty 也当写入宽度会让后面的字被补零，
			// 相当于悄悄清掉邻字。宁可让用户显式写两次。
			fmt.Fprintln(os.Stderr, "字写一次只写一个字：请用 -qty 1（位写忽略 -qty）")
			os.Exit(2)
		}
		val, err := parseWriteValue(*write, bitAccess)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		length := *qty
		dt := model.TypeUint16
		if bitAccess {
			length = 1
			dt = model.TypeBool
		}
		res, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: "write",
			Address: model.Address{Kind: model.AddressRegister, Area: areaLower,
				Offset: *offset, Bit: *bit, Length: length},
			DataType: dt,
			Value:    val,
		}})
		if err != nil {
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("写入 %s%d%s 完成（ok=%v）\n", areaLower, *offset, bitSuffix(*bit), res[0].OK)
		return
	}

	span := driver.ReadSpan{ID: "read"}
	if bitAccess {
		span.Points = append(span.Points, driver.PointRef{
			ID:       fmt.Sprintf("%s%d.%d", areaLower, *offset, *bit),
			Address:  model.Address{Kind: model.AddressRegister, Area: areaLower, Offset: *offset, Bit: *bit},
			DataType: model.TypeBool,
		})
	} else {
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

func bitSuffix(bit int) string {
	if bit < 0 {
		return ""
	}
	return fmt.Sprintf(".%d", bit)
}
