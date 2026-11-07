package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/template"
)

// uic instantiate 把一份参数化模板变成一份设备 JSON。
//
// 为什么不把模板实例化直接挂在 `run` 上：模板的价值在「交付前生成、评审
// 后入库」。让运行期按模板动态生成设备，等于让配置随时间漂移，热更新与
// 回滚都失去意义。因此这里只输出文件，是否并入配置由人决定。
func instantiateCmd(args []string) {
	fs := flag.NewFlagSet("instantiate", flag.ExitOnError)
	tplPath := fs.String("template", "", "模板文件路径")
	channel := fs.String("channel", "", "覆盖设备所挂通道 ID")
	id := fs.String("id", "", "覆盖设备 ID（默认取模板生成值）")
	out := fs.String("out", "", "输出文件（默认打印到标准输出）")
	asJSON := fs.Bool("json", false, "以 JSON 包装输出（含模板 ID）")
	var params paramFlags
	fs.Var(&params, "param", "模板参数 key=value，可重复")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `用法:
  uic instantiate -template meter.json -param index=1 -channel ch-meter
  uic instantiate -template meter.json -param index=1 -param unitId=9 -out meter-1.json -json

参数:
  -template  模板文件（必填）
  -param     模板参数 key=value，可重复；未提供的取模板默认值
  -channel   覆盖设备所挂通道
  -id        覆盖设备 ID
  -out       输出文件；不给则打印到标准输出
  -json      输出 {template, device} 包装，便于脚本消费
`)
	}
	fs.Parse(args)

	if *tplPath == "" {
		fmt.Fprintln(os.Stderr, "缺少 -template")
		os.Exit(2)
	}
	values, err := params.Map()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	def, err := template.Load(*tplPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	dev, err := def.Instantiate(values)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if *id != "" {
		dev.ID = *id
	}
	if *channel != "" {
		dev.ChannelID = *channel
	}
	if dev.Interval == 0 {
		dev.Interval = model.Duration(time.Second)
	}
	// 校验在这里就要做：模板写错一处，现场启动时才炸是最差的结果。
	if err := dev.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "实例化结果不合法: %v\n", err)
		if dev.ChannelID == "" {
			fmt.Fprintln(os.Stderr, "提示：模板里没有写 channel，用 -channel 指定")
		}
		os.Exit(1)
	}

	payload := any(dev)
	if *asJSON {
		payload = struct {
			Template string       `json:"template"`
			Device   model.Device `json:"device"`
		}{Template: def.ID, Device: dev}
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化失败: %v\n", err)
		os.Exit(1)
	}
	raw = append(raw, '\n')

	if *out == "" {
		os.Stdout.Write(raw)
		return
	}
	if err := config.WriteFile(*out, raw, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s（模板 %s，%d 个点位）\n", *out, def.ID, len(dev.Points))
}

// paramFlags 收集可重复的 key=value 参数。
type paramFlags []string

func (p *paramFlags) String() string { return strings.Join(*p, ",") }

func (p *paramFlags) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// Map 转成 map，并拒绝重复键：重复给同一个参数多半是复制粘贴事故，
// 后一个静默覆盖前一个会让「我明明写的是 A」无从解释。
func (p paramFlags) Map() (map[string]string, error) {
	out := make(map[string]string, len(p))
	for _, item := range p {
		i := strings.IndexByte(item, '=')
		if i <= 0 {
			return nil, fmt.Errorf("参数 %q 不是 key=value 形式", item)
		}
		k, v := strings.TrimSpace(item[:i]), item[i+1:]
		if k == "" {
			return nil, fmt.Errorf("参数 %q 缺少名称", item)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("参数 %q 重复", k)
		}
		out[k] = v
	}
	return out, nil
}
