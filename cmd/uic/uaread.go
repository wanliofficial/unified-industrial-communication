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
	opcuadriver "uic/internal/driver/opcua"
	"uic/internal/model"
)

// uaread 是一个极小的 OPC UA 客户端，用来在没有第三方工具的情况下自检
// 北向服务端（以及任何真实的 OPC UA 设备）。
//
// 存在的理由与 mbread 一样：现场最缺的不是功能，而是「不装软件就能证明
// 链路通」的手段。这里的额外价值在于 NodeId —— 北向地址空间的约定是
// ns=2;s={site}/{device}/{point}，一条命令就能验证「网关到底暴露了什么」。
//
// 边界：单次读/写，不做浏览、订阅、方法调用；只支持匿名 + None。
func uareadCmd(args []string) {
	fs := flag.NewFlagSet("uaread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4840", "服务端地址 host:port 或 opc.tcp://host:port")
	nodes := fs.String("nodes", "ns=0;i=2258", "逗号分隔的 NodeId 列表")
	typ := fs.String("type", "float64", "解码类型（读）或写入值类型（写）：bool|int32|uint32|int64|float32|float64|string")
	write := fs.String("write", "", "写入值；只能配合单个 NodeId 使用")
	timeout := fs.Duration("timeout", 5*time.Second, "连接与单次事务超时")
	fs.Parse(args)

	dt := model.DataType(strings.ToLower(*typ))
	if !dt.Valid() {
		fmt.Fprintf(os.Stderr, "未知类型 %q\n", *typ)
		os.Exit(2)
	}
	ids := splitNodes(*nodes)
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "-nodes 不能为空")
		os.Exit(2)
	}
	if *write != "" && len(ids) != 1 {
		fmt.Fprintln(os.Stderr, "-write 只能配合单个 NodeId")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ch := model.Channel{
		ID: "ch-uaread", Protocol: "opcua",
		Transport: model.Transport{Kind: "tcp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	c, err := opcuadriver.New().Open(ctx, driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "uaread", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接 %s 失败: %v\n", *addr, err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "探测 %s 失败: %v\n", *addr, err)
		os.Exit(1)
	}

	if *write != "" {
		uaWrite(ctx, c, ids[0], dt, *write)
		return
	}
	uaRead(ctx, c, ids, dt)
}

func splitNodes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func uaRead(ctx context.Context, c driver.Conn, ids []string, dt model.DataType) {
	span := driver.ReadSpan{ID: "uaread"}
	for _, id := range ids {
		span.Points = append(span.Points, driver.PointRef{
			// 用 NodeId 当点位 ID：解码失败时驱动的错误信息会带上它，
			// 否则只能看到一个下标，还得回头数列表。
			ID:       id,
			DataType: dt,
			Address:  model.Address{Kind: model.AddressSymbol, Path: id, Index: -1},
		})
	}
	samples, err := c.Read(ctx, []driver.ReadSpan{span})
	if err != nil && len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	for i, s := range samples {
		id := ids[i]
		if s.Err != nil {
			// 坏质量也照常打印：地址错、未连接、类型不符都是运维要看的信息。
			fmt.Printf("%s  quality=%s  error=%s\n", id, s.Quality, s.Err.Error())
			continue
		}
		fmt.Printf("%s  %s  quality=%s\n", id, s.Value.String(), s.Quality)
	}
	if err != nil {
		os.Exit(1)
	}
}

func uaWrite(ctx context.Context, c driver.Conn, id string, dt model.DataType, raw string) {
	v, err := parseUAValue(dt, raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入值 %q: %v\n", raw, err)
		os.Exit(2)
	}
	res, err := c.Write(ctx, []driver.WriteRequest{{
		PointID: "0", DataType: dt,
		Address: model.Address{Kind: model.AddressSymbol, Path: id, Index: -1},
		Value:   v,
	}})
	if err != nil && len(res) == 0 {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		os.Exit(1)
	}
	if len(res) == 0 || !res[0].OK {
		msg := "unknown error"
		if len(res) > 0 && res[0].Err != nil {
			msg = res[0].Err.Error()
		}
		fmt.Fprintf(os.Stderr, "写入 %s 被拒绝: %s\n", id, msg)
		os.Exit(1)
	}
	fmt.Printf("写入 %s = %s 成功\n", id, v.String())
}

func parseUAValue(dt model.DataType, raw string) (model.Value, error) {
	switch dt {
	case model.TypeBool:
		return model.BoolValue(parseBool(raw)), nil
	case model.TypeString:
		return model.TextValue(model.TypeString, raw), nil
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return model.Value{}, fmt.Errorf("不是数字: %w", err)
	}
	return model.NumValue(dt, n), nil
}
