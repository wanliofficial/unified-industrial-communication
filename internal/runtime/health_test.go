package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
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

// 健康检查不能「说自己有病但说不出哪里疼」：只要 status 不是 ok，
// problems 就必须有内容，否则接进告警系统后就是一行没人看的绿字。
func TestHealthStatusAlwaysExplainsItself(t *testing.T) {
	h := newHarness(t, func(cfg map[string]any) {
		// 指向几乎不可能有人监听的端口：点位会很快变坏，但设备要连续 3 次
		// 失败才转 faulted，中间存在一段「点位全坏、设备仍 collecting、
		// problems 为空」的真实窗口。
		cfg["channels"] = []any{map[string]any{
			"id": "ch-modbus", "protocol": "modbus-tcp",
			"transport":   map[string]any{"kind": "tcp", "address": "127.0.0.1:1"},
			"concurrency": 1, "timeout": "200ms",
		}}
		cfg["devices"].([]any)[0].(map[string]any)["interval"] = "100ms"
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body := h.do(http.MethodGet, "/api/v1/health", "")
		if code != 200 {
			t.Fatalf("health = %d %s", code, body)
		}
		var health httpapi.Health
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if health.Points.Bad > 0 {
			if health.Status == "ok" {
				t.Fatalf("bad points must not report ok: %+v", health)
			}
			if len(health.Problems) == 0 {
				t.Fatalf("status %q without any problem entry: %+v", health.Status, health)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("points never went bad")
}
