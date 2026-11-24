// Package loadtest 是可复跑的负载测试 harness：程序化生成 N 设备 × M 点的
// virtual 配置，驱动真实 runtime 的采集链路跑一段时间，回报吞吐、常驻内存
// 与协程数。
//
// 存在理由：docs/todo.md 的「测试」缺口要求 1000 设备负载测试，ADR-0001 C4/C5
// 又给了两条硬约束——「1000 点 / 50 台设备常驻 ≤ 150 MB」「数百设备并发但
// goroutine 数 O(worker) 而非 O(设备)」。没有可复跑的 harness，这两条只能靠
// 口头估计。本包把它们变成可测量的断言（见 loadtest_test.go）。
//
// 为什么走真实 runtime 而不是自己拼一个 scheduler + fake target：内存大头是
// 每点的 model.Sample 缓存、pipeline 处理器、outbox，以及 collector 里的
// 读计划——这些只有在装配完整链路时才会真实分配。用假 target 测出来的
// goroutine 数与内存都偏乐观，验证不了 C4。
package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/runtime"
)

// Spec 描述一次负载测试的规模与形态。
type Spec struct {
	// Devices 是设备数。默认 50（ADR-0001 C4 的场景）。
	Devices int
	// PointsPerDevice 是每台设备的点位数。默认 20 → 50×20=1000 点。
	PointsPerDevice int
	// Interval 是采集周期。默认 1s（设备级最小合法周期是 100ms）。
	Interval time.Duration
	// Concurrency 是通道的 worker 并发数。默认 8。
	Concurrency int
	// Duration 是 wall-clock 运行时长。默认 10s。
	Duration time.Duration
}

// withDefaults 填充零值。保持幂等，便于 cmd 与测试共用。
func (s Spec) withDefaults() Spec {
	if s.Devices <= 0 {
		s.Devices = 50
	}
	if s.PointsPerDevice <= 0 {
		s.PointsPerDevice = 20
	}
	if s.Interval <= 0 {
		s.Interval = time.Second
	}
	if s.Concurrency <= 0 {
		s.Concurrency = 8
	}
	if s.Duration <= 0 {
		s.Duration = 10 * time.Second
	}
	return s
}

// validate 拒绝无法构成合法配置的参数。周期下限与 model.Device.Validate 对齐，
// 避免在 runtime.New 里才报「interval must be >= 100ms」这种远离现场的错。
func (s Spec) validate() error {
	if s.Interval < 100*time.Millisecond {
		return fmt.Errorf("interval %s 低于设备最小合法周期 100ms", s.Interval)
	}
	if s.Duration < 100*time.Millisecond {
		return fmt.Errorf("duration %s 过短（至少 100ms）", s.Duration)
	}
	return nil
}

// Report 是一次负载测试的结果。
type Report struct {
	Devices         int           `json:"devices"`
	Points          int           `json:"points"`
	Interval        time.Duration `json:"interval"`
	Concurrency     int           `json:"concurrency"`
	Duration        time.Duration `json:"duration"`
	DevicesSampled  int           `json:"devicesSampled"`
	TotalReads      uint64        `json:"totalReads"`
	ReadsFailed     uint64        `json:"readsFailed"`
	CyclesPerDevice float64       `json:"cyclesPerDevice"`
	ThroughputPPS   float64       `json:"throughputPointsPerSec"`
	HeapAllocBytes  uint64        `json:"heapAllocBytes"`
	HeapSysBytes    uint64        `json:"heapSysBytes"`
	Goroutines      int           `json:"goroutines"`
}

// String 返回人类可读的一行摘要，供 cmd 直接打印。
func (r Report) String() string {
	return fmt.Sprintf(
		"设备 %d × 点 %d = %d 点 | 周期 %s | 并发 %d | 运行 %s\n"+
			"  采集: %d 次成功 / %d 次失败 | 每设备 %.1f 周期 | 吞吐 %.0f 点/秒\n"+
			"  稳态内存: heap %s / sys %s | 协程 %d",
		r.Devices, r.Points/r.max(1, r.Devices), r.Points,
		r.Interval, r.Concurrency, r.Duration,
		r.TotalReads, r.ReadsFailed, r.CyclesPerDevice, r.ThroughputPPS,
		humanBytes(r.HeapAllocBytes), humanBytes(r.HeapSysBytes), r.Goroutines,
	)
}

func (r Report) max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Run 执行一次负载测试。它在临时目录里落一份 virtual 配置，启动真实 runtime
// 跑满 spec.Duration（或 ctx 取消），采样后关闭。
//
// ctx 只控制「是否提前中止」；跑多久由 spec.Duration 决定。这样调用方既能
// 用固定时长做基准，也能用 signal/测试超时提前打断。
func Run(ctx context.Context, spec Spec) (Report, error) {
	spec = spec.withDefaults()
	if err := spec.validate(); err != nil {
		return Report{}, err
	}

	dir, err := os.MkdirTemp("", "uic-loadtest-")
	if err != nil {
		return Report{}, fmt.Errorf("loadtest: 建临时目录: %w", err)
	}
	defer os.RemoveAll(dir)
	dataDir := filepath.Join(dir, "data")

	cfg := buildConfig(spec, dataDir)
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Report{}, fmt.Errorf("loadtest: 序列化配置: %w", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return Report{}, fmt.Errorf("loadtest: 写配置: %w", err)
	}

	rt, err := runtime.New(runtime.Options{
		ConfigPath: cfgPath,
		DataDir:    dataDir,
		LogLevel:   "error", // 压测时不希望日志成为瓶颈或噪声源
	})
	if err != nil {
		return Report{}, fmt.Errorf("loadtest: 装配 runtime: %w", err)
	}
	defer func() { _ = rt.Stop(5 * time.Second) }()

	rt.Start(ctx)

	timer := time.NewTimer(spec.Duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}

	// 采样时 runtime 仍在运行（Stop 在 defer 里），因此读到的是稳态而非
	// 收尾后的残留。
	views := rt.Devices("")
	var totalReads, failed uint64
	for _, v := range views {
		totalReads += v.Stats.ReadsTotal
		failed += v.Stats.ReadsFailed
	}

	// 强制 GC 再读 heap：不 GC 的 HeapAlloc 含大量待回收垃圾，会把「稳态
	// 常驻」测成「压测期间的峰值」。ADR C4 约束的是常驻，不是瞬时峰值。
	goruntime.GC()
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)

	rep := Report{
		Devices:        spec.Devices,
		Points:         spec.Devices * spec.PointsPerDevice,
		Interval:       spec.Interval,
		Concurrency:    spec.Concurrency,
		Duration:       spec.Duration,
		DevicesSampled: len(views),
		TotalReads:     totalReads,
		ReadsFailed:    failed,
		HeapAllocBytes: ms.HeapAlloc,
		HeapSysBytes:   ms.HeapSys,
		Goroutines:     goruntime.NumGoroutine(),
	}
	if spec.Devices > 0 {
		rep.CyclesPerDevice = float64(totalReads) / float64(spec.Devices)
	}
	if secs := spec.Duration.Seconds(); secs > 0 {
		rep.ThroughputPPS = float64(totalReads*uint64(spec.PointsPerDevice)) / secs
	}
	return rep, nil
}

// buildConfig 生成一份只含采集链路的最小配置：
//   - 一个 virtual/memory 通道（零网络、零串口）；
//   - spec.Devices 台设备，每台 spec.PointsPerDevice 个 float32 寄存器点；
//   - 显式关闭 audit / history / journal——它们的文件 IO 会污染内存与吞吐
//     测量，且与 C4/C5 无关；
//   - server.listen 绑 127.0.0.1:0（Start 不监听端口，但配置校验要求地址合法）。
func buildConfig(spec Spec, dataDir string) config.File {
	off := false
	ch := model.Channel{
		ID:          "ch-load",
		Protocol:    "virtual",
		Transport:   model.Transport{Kind: "memory"},
		Concurrency: spec.Concurrency,
		Timeout:     model.Duration(2 * time.Second),
	}
	devices := make([]model.Device, spec.Devices)
	for i := 0; i < spec.Devices; i++ {
		points := make([]model.Point, spec.PointsPerDevice)
		for j := 0; j < spec.PointsPerDevice; j++ {
			points[j] = model.Point{
				ID: fmt.Sprintf("p%d", j),
				Address: model.Address{
					Kind:   model.AddressRegister,
					Area:   "holding",
					Offset: j * 2, // float32 占 2 个寄存器，逐点错开不重叠
					Length: 2,
					Bit:    -1,
				},
				DataType: model.TypeFloat32,
				Enabled:  true,
			}
		}
		devices[i] = model.Device{
			ID:        fmt.Sprintf("dev-%03d", i),
			ChannelID: "ch-load",
			Interval:  model.Duration(spec.Interval),
			Timeout:   model.Duration(2 * time.Second),
			Enabled:   true,
			Points:    points,
		}
	}
	return config.File{
		SchemaVersion: config.SchemaVersion,
		Site:          "loadtest",
		Server:        config.Server{Listen: "127.0.0.1:0"},
		Channels:      []model.Channel{ch},
		Devices:       devices,
		// 关掉所有会落盘的子系统，保证测的是采集链路的常驻内存。
		Storage: config.Storage{
			Dir:     dataDir,
			Journal: false,
			History: config.History{Enabled: false},
			Audit:   config.Audit{Enabled: &off},
		},
		Logging: config.Logging{Level: strings.TrimSpace("error")},
	}
}
