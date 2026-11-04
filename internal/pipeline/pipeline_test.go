package pipeline

import (
	"math"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

func dev() model.Device { return model.Device{ID: "d1", ChannelID: "c1", Enabled: true} }

func numPoint(scale, bias, deadband float64) model.Point {
	return model.Point{
		ID: "p1", DataType: model.TypeFloat32, Enabled: true,
		Scale: scale, Bias: bias, Deadband: deadband, Unit: "℃",
	}
}

func rawNum(v float64) driver.RawSample {
	return driver.RawSample{PointID: "p1", Value: model.NumValue(model.TypeFloat32, v), Quality: model.QualityGood}
}

func TestApplyScalesAndOffsets(t *testing.T) {
	p := NewProcessor()
	r := p.Apply(dev(), numPoint(2, 10, 0), rawNum(5), time.Unix(0, 0), 1)
	if r.Sample.Value.Num != 20 {
		t.Fatalf("got %v, want 20", r.Sample.Value.Num)
	}
	if r.Sample.Unit != "℃" || r.Sample.DeviceID != "d1" || r.Sample.Seq != 1 {
		t.Fatalf("metadata wrong: %+v", r.Sample)
	}
	if !r.Changed {
		t.Fatal("first sample must be a change")
	}
}

// 坏值绝不能参与缩放：否则 0 * scale 会伪装成好值。
func TestBadQualitySkipsScaling(t *testing.T) {
	p := NewProcessor()
	raw := driver.RawSample{
		PointID: "p1", Value: model.NumValue(model.TypeFloat32, 0),
		Quality: model.QualityTimeout,
		Err:     driver.E(driver.ErrTimeout, "read", "d1", "timeout", true, nil),
	}
	r := p.Apply(dev(), numPoint(100, 5, 0), raw, time.Unix(0, 0), 1)
	if r.Sample.Value.Num != 0 {
		t.Fatalf("bad quality must not scale, got %v", r.Sample.Value.Num)
	}
	if r.Sample.Quality != model.QualityTimeout {
		t.Fatalf("quality = %s", r.Sample.Quality)
	}
	if r.Sample.Error == "" {
		t.Fatal("error text should be preserved")
	}
}

// 死区只抑制事件，不抑制快照更新。
func TestDeadbandSuppressesEventButKeepsSample(t *testing.T) {
	p := NewProcessor()
	pt := numPoint(1, 0, 1.0)
	base := time.Unix(100, 0)

	if r := p.Apply(dev(), pt, rawNum(10), base, 1); !r.Changed {
		t.Fatal("first value is a change")
	}
	if r := p.Apply(dev(), pt, rawNum(10.5), base, 2); r.Changed {
		t.Fatal("0.5 within deadband 1.0 must be suppressed")
	}
	last, ok := p.Last("d1", "p1")
	if !ok || last.Value.Num != 10.5 {
		t.Fatalf("snapshot must still update, got %+v", last)
	}
	if r := p.Apply(dev(), pt, rawNum(11.5), base, 3); !r.Changed {
		t.Fatal("delta == deadband must emit")
	}
	// 回落也要比较与「最近一次快照」的差，而不是与第一次的差。
	if r := p.Apply(dev(), pt, rawNum(10.7), base, 4); r.Changed {
		t.Fatal("0.8 from 11.5 is within deadband")
	}
}

// 质量变化必须穿透死区：值没变但已经超时不能被吞掉。
func TestQualityChangeAlwaysEmits(t *testing.T) {
	p := NewProcessor()
	pt := numPoint(1, 0, 100)
	at := time.Unix(0, 0)
	p.Apply(dev(), pt, rawNum(42), at, 1)

	bad := driver.RawSample{PointID: "p1", Value: model.NumValue(model.TypeFloat32, 42), Quality: model.QualityTimeout}
	if r := p.Apply(dev(), pt, bad, at, 2); !r.Changed {
		t.Fatal("good -> timeout must be a change event")
	}
	if r := p.Apply(dev(), pt, bad, at, 3); r.Changed {
		t.Fatal("timeout -> timeout with same value must be suppressed")
	}
}

func TestBoolIgnoresDeadband(t *testing.T) {
	p := NewProcessor()
	pt := model.Point{ID: "b", DataType: model.TypeBool, Enabled: true, Deadband: 10}
	at := time.Unix(0, 0)
	on := driver.RawSample{PointID: "b", Value: model.BoolValue(true), Quality: model.QualityGood}
	off := driver.RawSample{PointID: "b", Value: model.BoolValue(false), Quality: model.QualityGood}

	p.Apply(dev(), pt, on, at, 1)
	if r := p.Apply(dev(), pt, on, at, 2); r.Changed {
		t.Fatal("same bool must not change")
	}
	if r := p.Apply(dev(), pt, off, at, 3); !r.Changed {
		t.Fatal("bool flip must always emit")
	}
}

func TestNaNAlwaysEmits(t *testing.T) {
	p := NewProcessor()
	pt := numPoint(1, 0, 1)
	at := time.Unix(0, 0)
	nan := driver.RawSample{PointID: "p1", Value: model.NumValue(model.TypeFloat32, math.NaN()), Quality: model.QualityGood}
	p.Apply(dev(), pt, nan, at, 1)
	if r := p.Apply(dev(), pt, nan, at, 2); !r.Changed {
		t.Fatal("NaN must not compare equal to itself")
	}
}

func TestForgetAndReset(t *testing.T) {
	p := NewProcessor()
	pt := numPoint(1, 0, 1)
	at := time.Unix(0, 0)
	p.Apply(dev(), pt, rawNum(1), at, 1)
	p.Apply(model.Device{ID: "d2", ChannelID: "c1"}, pt, rawNum(2), at, 1)

	p.Forget("d1")
	if _, ok := p.Last("d1", "p1"); ok {
		t.Fatal("d1 must be forgotten")
	}
	if _, ok := p.Last("d2", "p1"); !ok {
		t.Fatal("d2 must be preserved")
	}
	p.Reset()
	if _, ok := p.Last("d2", "p1"); ok {
		t.Fatal("reset must clear everything")
	}
}

func TestApplyConcurrentSafe(t *testing.T) {
	p := NewProcessor()
	pt := numPoint(1, 0, 0)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				p.Apply(dev(), pt, rawNum(float64(n*1000+j)), time.Unix(0, 0), uint64(j))
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if _, ok := p.Last("d1", "p1"); !ok {
		t.Fatal("expected a last value")
	}
}

func TestRawSampleErrorPreserved(t *testing.T) {
	p := NewProcessor()
	pt := model.Point{ID: "p1", DataType: model.TypeUint16, Enabled: true}
	raw := driver.RawSample{
		PointID: "p1", Value: model.NumValue(model.TypeUint16, 0),
		Quality: model.QualityAddressError,
		Err:     driver.E(driver.ErrAddress, "read", "d1", "bad register", false, nil),
	}
	r := p.Apply(dev(), pt, raw, time.Unix(0, 0), 1)
	if r.Sample.Error == "" {
		t.Fatal("expected error text")
	}
	if r.Sample.Quality != model.QualityAddressError {
		t.Fatalf("quality = %s", r.Sample.Quality)
	}
}
