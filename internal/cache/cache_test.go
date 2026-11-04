package cache

import (
	"sync"
	"testing"
	"time"

	"uic/internal/model"
)

func sample(device, point string, v float64, q model.Quality) model.Sample {
	return model.Sample{
		DeviceID: device, PointID: point,
		Value: model.NumValue(model.TypeFloat32, v), Quality: q,
		CollectedAt: time.Unix(0, 0),
	}
}

func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestSetGetList(t *testing.T) {
	c := New()
	c.Set(sample("d2", "p1", 1, model.QualityGood))
	c.Set(sample("d1", "p2", 2, model.QualityGood))
	c.Set(sample("d1", "p1", 3, model.QualityGood))

	s, ok := c.Get("d1", "p1")
	if !ok || s.Value.Num != 3 {
		t.Fatalf("get = %+v %v", s, ok)
	}
	if _, ok := c.Get("d9", "p1"); ok {
		t.Fatal("missing point must report false")
	}

	all := c.List("")
	if len(all) != 3 {
		t.Fatalf("list = %d", len(all))
	}
	if all[0].DeviceID != "d1" || all[0].PointID != "p1" {
		t.Fatalf("list not sorted: %+v", all)
	}
	if got := c.List("d1"); len(got) != 2 {
		t.Fatalf("device list = %d", len(got))
	}

	n, byQ := c.Count()
	if n != 3 || byQ[model.QualityGood] != 3 {
		t.Fatalf("count = %d %+v", n, byQ)
	}
}

// 值未变且质量未变时不产生事件：这是死区之后最后一道去重。
func TestSetSuppressesIdenticalSample(t *testing.T) {
	c := New()
	ch, cancel := c.Subscribe(8, nil)
	defer cancel()

	c.Set(sample("d1", "p1", 1, model.QualityGood))
	if got := drain(ch); len(got) != 1 || got[0].Type != EventSample {
		t.Fatalf("first set = %+v", got)
	}
	c.Set(sample("d1", "p1", 1, model.QualityGood))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("identical sample emitted %+v", got)
	}
	// 值变化 → sample；质量变化 → quality（值不变时不再发 sample）。
	c.Set(sample("d1", "p1", 2, model.QualityGood))
	c.Set(sample("d1", "p1", 2, model.QualityTimeout))
	got := drain(ch)
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Type != EventSample {
		t.Fatalf("first = %s", got[0].Type)
	}
	if got[1].Type != EventQuality || got[1].From != model.QualityGood || got[1].To != model.QualityTimeout {
		t.Fatalf("quality event = %+v", got[1])
	}
	// 快照必须反映最新的坏质量。
	if s, _ := c.Get("d1", "p1"); s.Quality != model.QualityTimeout {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestQualityChangeWithValueChangeEmitsBoth(t *testing.T) {
	c := New()
	ch, cancel := c.Subscribe(8, nil)
	defer cancel()
	c.Set(sample("d1", "p1", 1, model.QualityGood))
	drain(ch)
	c.Set(sample("d1", "p1", 2, model.QualityCommError))
	got := drain(ch)
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Type != EventQuality || got[1].Type != EventSample {
		t.Fatalf("order = %s %s", got[0].Type, got[1].Type)
	}
}

func TestSubscribeFilterAndUnsubscribe(t *testing.T) {
	c := New()
	ch, cancel := c.Subscribe(8, func(s model.Sample) bool { return s.DeviceID == "d1" })
	if c.SubscriberCount() != 1 {
		t.Fatalf("subscribers = %d", c.SubscriberCount())
	}
	c.Set(sample("d1", "p1", 1, model.QualityGood))
	c.Set(sample("d2", "p1", 1, model.QualityGood))
	got := drain(ch)
	if len(got) != 1 || got[0].Sample.DeviceID != "d1" {
		t.Fatalf("filtered events = %+v", got)
	}

	cancel()
	if c.SubscriberCount() != 0 {
		t.Fatalf("cancel did not unregister: %d", c.SubscriberCount())
	}
	// 退订后继续写入不得 panic，也不得再收到事件。
	c.Set(sample("d1", "p1", 2, model.QualityGood))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("events after cancel = %+v", got)
	}
	cancel() // 二次取消必须幂等
}

// 慢订阅者不能阻塞采集：满了就丢弃最旧事件。
func TestSlowSubscriberDropsOldest(t *testing.T) {
	c := New()
	ch, cancel := c.Subscribe(2, nil)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			c.Set(sample("d1", "p1", float64(i), model.QualityGood))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Set blocked on a slow subscriber")
	}
	got := drain(ch)
	if len(got) == 0 {
		t.Fatal("expected some events")
	}
	// 最新值必须留在缓冲区里。
	last := got[len(got)-1]
	if last.Sample.Value.Num != 99 {
		t.Fatalf("last event = %v, want 99", last.Sample.Value.Num)
	}
}

// 并发写入与退订交错时不得 panic（曾经会因为 close(ch) 而崩）。
func TestConcurrentSetAndCancel(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Set(sample("d1", "p1", float64(n*1000+j), model.QualityGood))
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		_, cancel := c.Subscribe(4, nil)
		cancel()
	}
	wg.Wait()
}

func TestConcurrentReaders(t *testing.T) {
	c := New()
	c.Set(sample("d1", "p1", 1, model.QualityGood))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Set(sample("d1", "p1", float64(n*1000+j), model.QualityGood))
				_, _ = c.Get("d1", "p1")
				_ = c.List("d1")
				_, _ = c.Count()
			}
		}(i)
	}
	wg.Wait()
}
