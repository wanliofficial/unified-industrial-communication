package bacnetmstp

import (
	"context"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

// 部分失败语义是 §3.7.4/§3.9.5 的硬契约：只要有任意点位成功，`Read` 就不向上
// 返回错误（好样本必须保留）；全部点位失败才返回第一个错误，让设备进入故障/
// 退避。链路级错误（超时/断连）是例外：立即返回，并把剩余点位显式标坏。
//
// 这三条此前在本驱动上不满足：无论成功几个点位，`Read` 都无条件返回 firstErr，
// 于是一个配错的点位会把整台健康设备打成 error 指标。
func TestReadPartialFailureSemantics(t *testing.T) {
	t.Run("partial success keeps good samples and returns no error", func(t *testing.T) {
		_, c := newTestConn(t, nil)
		good := objPoint("good", "analog-input", 1, "present-value", model.TypeFloat32)
		bad := objPoint("bad", "analog-input", 1, "no-such-prop", model.TypeFloat32)

		samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
			Points: []driver.PointRef{good, bad}}})
		if err != nil {
			t.Fatalf("partial failure returned error %v; want nil (a good sample exists)", err)
		}
		if len(samples) != 2 {
			t.Fatalf("samples = %d, want 2 (good samples must not disappear)", len(samples))
		}
		byID := map[string]driver.RawSample{}
		for _, s := range samples {
			byID[s.PointID] = s
		}
		if g := byID["good"]; g.Err != nil || g.Quality != model.QualityGood {
			t.Fatalf("good sample = %+v, want err=nil quality=good", g)
		}
		if b := byID["bad"]; b.Err == nil || b.Quality != model.QualityAddressError {
			t.Fatalf("bad sample = %+v, want err!=nil quality=address_error", b)
		}
	})

	t.Run("all points failing returns the first error", func(t *testing.T) {
		_, c := newTestConn(t, nil)
		p1 := objPoint("p1", "analog-input", 1, "no-such-prop", model.TypeFloat32)
		p2 := objPoint("p2", "analog-input", 1, "also-missing", model.TypeFloat32)

		samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
			Points: []driver.PointRef{p1, p2}}})
		if err == nil {
			t.Fatal("all points failed but Read returned nil; breaker needs the aggregate error")
		}
		if got := driver.KindOf(err); got != driver.ErrAddress {
			t.Fatalf("kind = %s, want address", got)
		}
		if len(samples) != 2 {
			t.Fatalf("samples = %d, want 2 (failed points stay visible, not dropped)", len(samples))
		}
		for _, s := range samples {
			if s.Err == nil {
				t.Fatalf("%s: expected a per-point error", s.PointID)
			}
		}
	})

	t.Run("link-level failure returns immediately and marks the rest bad", func(t *testing.T) {
		// fakeBus 从不应答数据帧：ReadFull 立刻返回 os.ErrDeadlineExceeded，
		// 于是每个点位在首个 attempt 就以超时收场——不需要真等定时器。
		bus := &fakeBus{}
		bus.respond = func(wire []byte) [][]byte { return nil }
		c := openFake(t, bus, map[string]string{"address": "128"})

		p1 := objPoint("p1", "analog-input", 1, "present-value", model.TypeFloat32)
		p2 := objPoint("p2", "analog-input", 1, "present-value", model.TypeFloat32)
		samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
			Points: []driver.PointRef{p1, p2}}})
		if err == nil {
			t.Fatal("link-level failure returned nil; want the timeout surfaced")
		}
		if got := driver.KindOf(err); got != driver.ErrTimeout {
			t.Fatalf("kind = %s, want timeout", got)
		}
		// 链路级失败时剩余点位必须被显式标坏，而不是「消失」成 point missing。
		if len(samples) != 2 {
			t.Fatalf("samples = %d, want 2 (rest must be marked bad, not dropped)", len(samples))
		}
		for _, s := range samples {
			if s.Err == nil {
				t.Fatalf("%s: expected a per-point error on link failure", s.PointID)
			}
			if s.Quality != model.QualityTimeout {
				t.Fatalf("%s quality = %s, want timeout", s.PointID, s.Quality)
			}
		}
	})
}
