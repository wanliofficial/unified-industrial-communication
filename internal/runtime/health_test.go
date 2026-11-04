package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/scheduler"
)

// 看门狗：超过 3 个采集周期没有更新的可信点位必须降级为 stale，
// 否则设备静默掉线时界面上会一直挂着最后一个好值。
func TestMarkStaleDegradesSilentPoints(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	// 用一个明显未来的时间触发，避免测试等待真实秒级 tick。
	h.rt.markStale(time.Now().Add(time.Hour))

	s, ok := h.rt.Point("sim01", "reg0")
	if !ok {
		t.Fatal("point missing")
	}
	if s.Quality != model.QualityStale {
		t.Fatalf("quality = %s", s.Quality)
	}
	if s.Error == "" {
		t.Fatal("stale point must carry an explanation")
	}

	// 再次触发不得把已降级的点位重复计数（IsUsable 为假时跳过）。
	before := h.rt.metrics.Value("uic_stale_total", map[string]string{"device": "sim01", "point": "reg0"})
	h.rt.markStale(time.Now().Add(2 * time.Hour))
	after := h.rt.metrics.Value("uic_stale_total", map[string]string{"device": "sim01", "point": "reg0"})
	if after != before {
		t.Fatalf("stale counted twice: %v -> %v", before, after)
	}

	// 下一次正常采集必须把质量恢复为 good。
	h.waitPointGood("sim01", "reg0")
}

// 熔断后的探活路径：链路正常时 Probe 成功，链路断掉时失败并丢弃连接。
func TestCollectorProbe(t *testing.T) {
	h := newHarness(t, nil)
	h.waitPointGood("sim01", "reg0")

	c, ok := h.rt.collector("sim01")
	if !ok {
		t.Fatal("collector missing")
	}
	gen, _ := h.rt.sched.Generation("sim01")
	lease := &scheduler.Lease{Device: "sim01", Generation: gen}
	if err := c.Probe(context.Background(), lease); err != nil {
		t.Fatalf("probe on healthy link: %v", err)
	}

	// 关掉从站：探活必须失败，并且连接被丢弃以便下次重建。
	if err := h.sim.Close(); err != nil {
		t.Fatal(err)
	}
	err := c.Probe(context.Background(), lease)
	if err == nil {
		t.Fatal("probe must fail after the server is gone")
	}
	c.mu.Lock()
	conned := c.conn != nil
	c.mu.Unlock()
	if conned {
		t.Fatal("dead connection must be dropped")
	}
}

func TestQualityOf(t *testing.T) {
	cases := map[driver.ErrorKind]model.Quality{
		driver.ErrTimeout:      model.QualityTimeout,
		driver.ErrAddress:      model.QualityAddressError,
		driver.ErrDisconnected: model.QualityCommError,
		driver.ErrUnsupported:  model.QualityBad,
		driver.ErrPermission:   model.QualityBad,
		driver.ErrProtocol:     model.QualityBad,
		driver.ErrInternal:     model.QualityBad,
	}
	for kind, want := range cases {
		if got := qualityOf(driver.E(kind, "read", "d", "x", false, nil)); got != want {
			t.Fatalf("%s -> %s, want %s", kind, got, want)
		}
	}
	if got := qualityOf(errors.New("plain")); got != model.QualityBad {
		t.Fatalf("plain error -> %s", got)
	}
	if got := qualityOf(context.DeadlineExceeded); got != model.QualityTimeout {
		t.Fatalf("deadline -> %s", got)
	}
}
