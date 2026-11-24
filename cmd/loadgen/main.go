// 二进制 loadgen 是负载测试 harness 的命令行入口：生成 N 设备 × M 点的
// virtual 配置，驱动真实 runtime 跑一段时间，打印吞吐/内存/协程报告。
//
// 对应 docs/todo.md 的「1000 设备负载测试」缺口与 ADR-0001 C4/C5 两条硬约束。
// 零网络、零串口、零外部依赖——可在开发机或 CI 里直接复跑。
//
// 用法：
//
//	go run ./cmd/loadgen                       # 默认 50×20=1000 点，跑 10s
//	go run ./cmd/loadgen -devices 200 -points 10 -duration 30s
//	go run ./cmd/loadgen -json                 # 机器可读输出，便于进 CI 归档
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"uic/internal/loadtest"
)

func main() {
	var spec loadtest.Spec
	flag.IntVar(&spec.Devices, "devices", 50, "设备数")
	flag.IntVar(&spec.PointsPerDevice, "points", 20, "每台设备的点位数")
	interval := flag.Duration("interval", time.Second, "设备采集周期")
	flag.IntVar(&spec.Concurrency, "concurrency", 8, "通道 worker 并发数")
	duration := flag.Duration("duration", 10*time.Second, "运行时长")
	asJSON := flag.Bool("json", false, "以 JSON 输出报告")
	flag.Parse()

	spec.Interval = *interval
	spec.Duration = *duration

	rep, err := loadtest.Run(context.Background(), spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println(rep.String())
}
