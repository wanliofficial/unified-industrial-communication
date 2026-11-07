package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"uic/internal/codec/s7"
	s7driver "uic/internal/driver/s7"
)

// s7blocks / s7upload 是 S7 块服务的自检与备份工具。
//
// 为什么放在 CLI 而不是北向 API：块上传是「组态/备份/排障」动作，一次
// 可能传几 MB，和周期采集的延迟目标完全冲突；它也不该被普通 operator
// 随手触发。放在命令行里，权限由操作系统账号控制，行为可复制进工单。
func s7blocksCmd(args []string) {
	fs := flag.NewFlagSet("s7blocks", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:15102", "PLC 地址 host:port")
	rack := fs.Int("rack", 0, "机架号 0..7")
	slot := fs.Int("slot", 2, "槽位号 0..31（S7-1200/1500 常为 1）")
	typ := fs.String("type", "", "块类型：ob|db|sdb|fc|sfc|fb|sfb（留空则列全部数量）")
	num := fs.Int("num", -1, "块号；配合 -type 查看单个块属性")
	timeout := fs.Duration("timeout", 5*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	fs.Parse(args)

	sess, err := s7driver.OpenSession(context.Background(), s7driver.SessionOptions{
		Address: *addr, Rack: *rack, Slot: *slot, Timeout: *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch {
	case *typ == "":
		counts, err := sess.ListBlocks(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "列块目录失败: %v\n", err)
			os.Exit(1)
		}
		printBlockCounts(counts, *addr, sess.PDULen(), *asJSON)

	case *num < 0:
		bt, err := parseBlockType(*typ)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		nums, err := sess.ListBlocksOfType(ctx, bt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "列 %s 块失败: %v\n", s7.BlockTypeName(bt), err)
			os.Exit(1)
		}
		printBlockNumbers(bt, nums, *asJSON)

	default:
		bt, err := parseBlockType(*typ)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		info, err := sess.BlockInfo(ctx, bt, *num)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读 %s%d 属性失败: %v\n", s7.BlockTypeName(bt), *num, err)
			os.Exit(1)
		}
		printBlockInfo(info, *asJSON)
	}
}

func s7uploadCmd(args []string) {
	fs := flag.NewFlagSet("s7upload", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:15102", "PLC 地址 host:port")
	rack := fs.Int("rack", 0, "机架号 0..7")
	slot := fs.Int("slot", 2, "槽位号 0..31（S7-1200/1500 常为 1）")
	typ := fs.String("type", "", "块类型：ob|db|sdb|fc|sfc|fb|sfb（必填）")
	num := fs.Int("num", -1, "块号（必填）")
	out := fs.String("out", "", "写入文件；\"-\" 表示标准输出；留空只打印摘要")
	timeout := fs.Duration("timeout", 10*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出摘要")
	fs.Parse(args)

	if *typ == "" || *num < 0 {
		fmt.Fprintln(os.Stderr, "s7upload 需要 -type 与 -num")
		os.Exit(2)
	}
	bt, err := parseBlockType(*typ)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	sess, err := s7driver.OpenSession(context.Background(), s7driver.SessionOptions{
		Address: *addr, Rack: *rack, Slot: *slot, Timeout: *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer sess.Close()

	data, err := sess.UploadBlock(context.Background(), bt, *num)
	if err != nil {
		fmt.Fprintf(os.Stderr, "上传 %s%d 失败: %v\n", s7.BlockTypeName(bt), *num, err)
		os.Exit(1)
	}
	sum := sha256.Sum256(data)

	if *out == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintf(os.Stderr, "写标准输出: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *out != "" {
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "写文件 %s: %v\n", *out, err)
			os.Exit(1)
		}
	}

	summary := uploadSummary{
		Address: *addr,
		Type:    s7.BlockTypeName(bt),
		Number:  *num,
		Bytes:   len(data),
		SHA256:  hex.EncodeToString(sum[:]),
		File:    *out,
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
		return
	}
	fmt.Printf("已上传 %s%d：%d 字节\n", summary.Type, summary.Number, summary.Bytes)
	fmt.Printf("  sha256  %s\n", summary.SHA256)
	if *out != "" {
		fmt.Printf("  文件    %s\n", *out)
	} else if len(data) > 0 {
		preview := data
		if len(preview) > 64 {
			preview = preview[:64]
		}
		fmt.Printf("  前 64 字节  % x\n", preview)
		fmt.Println("  （未写文件；加 -out <path> 或 -out - 输出完整内容）")
	}
}

type uploadSummary struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	Number  int    `json:"number"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
	File    string `json:"file,omitempty"`
}

type blockCountJSON struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

func printBlockCounts(counts []s7.BlockCount, addr string, pduLen int, asJSON bool) {
	if asJSON {
		out := make([]blockCountJSON, 0, len(counts))
		for _, c := range counts {
			out = append(out, blockCountJSON{Type: s7.BlockTypeName(c.Type), Count: c.Count})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"address": addr,
			"pduLen":  pduLen,
			"blocks":  out,
		})
		return
	}
	total := 0
	for _, c := range counts {
		total += c.Count
	}
	fmt.Printf("%s  协商 PDU %d  共 %d 个块\n", addr, pduLen, total)
	for _, c := range counts {
		fmt.Printf("  %-4s %d\n", s7.BlockTypeName(c.Type), c.Count)
	}
}

func printBlockNumbers(bt byte, nums []int, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"type":  s7.BlockTypeName(bt),
			"count": len(nums),
			"list":  nums,
		})
		return
	}
	fmt.Printf("%s 块（%d 个）\n", s7.BlockTypeName(bt), len(nums))
	for _, n := range nums {
		fmt.Printf("  %s%d\n", s7.BlockTypeName(bt), n)
	}
	if len(nums) == 0 {
		fmt.Println("  （无）")
	}
}

func printBlockInfo(info s7.BlockInfo, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"type":      s7.BlockTypeName(info.Type),
			"number":    info.Number,
			"language":  fmt.Sprintf("0x%02X", info.Lang),
			"version":   info.Version,
			"flags":     fmt.Sprintf("0x%02X", info.Flags),
			"loadSize":  info.LoadSize,
			"mc7Size":   info.MC7Size,
			"sbbLength": info.SBBLength,
			"localData": info.LocalData,
			"checksum":  fmt.Sprintf("0x%04X", info.Checksum),
			"author":    info.Author,
			"family":    info.Family,
			"header":    info.Header,
			"codeDate":  hex.EncodeToString(info.CodeDate),
			"intfDate":  hex.EncodeToString(info.IntfDate),
		})
		return
	}
	fmt.Printf("%s%d\n", s7.BlockTypeName(info.Type), info.Number)
	fmt.Printf("  语言      0x%02X  版本 %d  标志 0x%02X\n", info.Lang, info.Version, info.Flags)
	fmt.Printf("  装载大小  %d 字节\n", info.LoadSize)
	fmt.Printf("  MC7 大小  %d 字节\n", info.MC7Size)
	fmt.Printf("  局部数据  %d 字节  SBB %d 字节\n", info.LocalData, info.SBBLength)
	fmt.Printf("  校验和    0x%04X\n", info.Checksum)
	fmt.Printf("  作者      %q  家族 %q  头 %q\n", info.Author, info.Family, info.Header)
}

func parseBlockType(name string) (byte, error) {
	bt, err := s7.BlockTypeOf(name)
	if err != nil {
		return 0, fmt.Errorf("块类型 %q 无效（可选：%s）", name, strings.Join(
			[]string{"ob", "db", "sdb", "fc", "sfc", "fb", "sfb"}, " | "))
	}
	return bt, nil
}
