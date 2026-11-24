package loadtest

import (
	"context"
	"testing"
	"time"
)

// TestRunMeetsNFR 把 ADR-0001 C4 的硬约束变成可回归的断言：
// 「1000 点 / 50 台设备常驻 ≤ 150 MB」，且负载确实在跑（吞吐 > 0、
// 所有设备都被调度到）。
//
// 用 800ms 的短周期跑：验证的是「装配完整链路后的稳态常驻」，不是长跑
// 的抖动。真正做容量规划时用 cmd/loadgen 跑更长时长。
func TestRunMeetsNFR(t *testing.T) {
	const (
		devices = 50
		points  = 20 // 50×20 = 1000 点，ADR C4 场景
		limit   = 150 << 20
	)
	rep, err := Run(context.Background(), Spec{
		Devices:         devices,
		PointsPerDevice: points,
		Interval:        100 * time.Millisecond,
		Concurrency:     8,
		Duration:        800 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.DevicesSampled != devices {
		t.Fatalf("被采样的设备 = %d，want %d（有设备未进入调度）", rep.DevicesSampled, devices)
	}
	if rep.TotalReads == 0 {
		t.Fatalf("完全没有采集发生（ThroughputPPS=%v）", rep.ThroughputPPS)
	}
	if rep.ThroughputPPS <= 0 {
		t.Fatalf("吞吐 = %v，应为正", rep.ThroughputPPS)
	}
	if rep.HeapAllocBytes > limit {
		t.Fatalf("稳态 heap %s 超过 ADR-0001 C4 上限 %s",
			humanBytes(rep.HeapAllocBytes), humanBytes(limit))
	}
	t.Logf("1000 点实测：heap=%s sys=%s goroutines=%d 吞吐=%.0f 点/秒 总采集=%d",
		humanBytes(rep.HeapAllocBytes), humanBytes(rep.HeapSysBytes),
		rep.Goroutines, rep.ThroughputPPS, rep.TotalReads)
}

// TestGoroutinesDoNotScaleWithDevices 验证 ADR-0001 C5：数百设备并发，但
// 协程数 O(worker) 而非 O(设备)——单设备串行、worker 池复用。
//
// 判据用「设备数翻三倍，协程增量仍远小于设备增量」：若每设备一个常驻
// goroutine，60 设备会比 20 设备多约 40 个；O(worker) 则接近不变。
func TestGoroutinesDoNotScaleWithDevices(t *testing.T) {
	run := func(devices int) Report {
		t.Helper()
		rep, err := Run(context.Background(), Spec{
			Devices:         devices,
			PointsPerDevice: 5,
			Interval:        100 * time.Millisecond,
			Concurrency:     8,
			Duration:        500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Run(%d devices): %v", devices, err)
		}
		return rep
	}

	small := run(20)
	big := run(60)

	// 设备 +40；若协程也 +40 就是 O(设备)。留足余量：允许 +20 以内
	//（测试框架自身的若干常驻协程、以及 GC/调度带来的噪声）。
	const maxGrowth = 20
	growth := big.Goroutines - small.Goroutines
	if growth >= maxGrowth {
		t.Fatalf("协程随设备线性增长：20 设备 %d 个，60 设备 %d 个（增量 %d，ADR-0001 C5 要求 O(worker)）",
			small.Goroutines, big.Goroutines, growth)
	}
	t.Logf("协程数：20 设备=%d，60 设备=%d（增量 %d，O(worker) 成立）",
		small.Goroutines, big.Goroutines, growth)
}
