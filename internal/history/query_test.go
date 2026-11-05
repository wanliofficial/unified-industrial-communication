package history

import (
	"errors"
	"math"
	"testing"
	"time"

	"uic/internal/model"
)

func TestQueryRawFiltersAndRange(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	mustWrite(t, s, numRec(base.Add(1*time.Second), "press", "power", 1))
	mustWrite(t, s, numRec(base.Add(2*time.Second), "press", "power", 2))
	mustWrite(t, s, numRec(base.Add(3*time.Second), "press", "power", 3))
	mustWrite(t, s, numRec(base.Add(2*time.Second), "press", "temp", 20))
	mustWrite(t, s, numRec(base.Add(2*time.Second), "oven", "power", 99))

	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(10 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 || len(res.Points) != 3 {
		t.Fatalf("count = %d points = %+v", res.Count, res.Points)
	}
	for i, p := range res.Points {
		if p.Value.Num != float64(i+1) {
			t.Fatalf("points[%d] = %v", i, p.Value.Num)
		}
	}
	if res.Skipped != 0 || res.Scanned == 0 {
		t.Fatalf("diagnostics = scanned %d skipped %d", res.Scanned, res.Skipped)
	}

	// 区间是左闭右闭的：from/to 正好命中首尾记录时必须包含。
	res, err = s.Query(Query{Device: "press", Point: "power",
		From: base.Add(1 * time.Second), To: base.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 {
		t.Fatalf("inclusive range count = %d, want 2", res.Count)
	}
}

func TestQueryDefaultsToLastHour(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	mustWrite(t, s, numRec(base.Add(-2*time.Hour), "press", "power", 1))
	mustWrite(t, s, numRec(base.Add(-10*time.Minute), "press", "power", 2))
	mustWrite(t, s, numRec(base.Add(-time.Minute), "press", "power", 3))

	res, err := s.Query(Query{Device: "press", Point: "power"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2 (the 2-hour-old sample is outside the default window)", res.Count)
	}
	if !res.To.Equal(base) || !res.From.Equal(base.Add(-time.Hour)) {
		t.Fatalf("window = %s..%s", res.From, res.To)
	}
}

func TestQueryLimitAndTruncated(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	for i := 0; i < 10; i++ {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", float64(i)))
	}
	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(time.Minute), Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 4 || !res.Truncated {
		t.Fatalf("len = %d truncated = %v", len(res.Points), res.Truncated)
	}
	// 截断标记的含义是「还有更多」，而不是「正好取满」。
	res, err = s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 10 || res.Truncated {
		t.Fatalf("len = %d truncated = %v", len(res.Points), res.Truncated)
	}
	// limit 超过上限时被夹住而不是报错。
	res, err = s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(time.Minute), Limit: MaxLimit + 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 10 {
		t.Fatalf("len = %d", len(res.Points))
	}
}

func TestQueryOrdersPointsByTime(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	// 写入顺序不等于时间顺序（时钟回拨 / 补写历史）。北向契约承诺升序。
	mustWrite(t, s, numRec(base.Add(3*time.Second), "press", "power", 3))
	mustWrite(t, s, numRec(base.Add(1*time.Second), "press", "power", 1))
	mustWrite(t, s, numRec(base.Add(2*time.Second), "press", "power", 2))

	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range res.Points {
		if p.Value.Num != float64(i+1) {
			t.Fatalf("points = %+v", res.Points)
		}
	}
}

func TestQueryValidation(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	now := clock.Now()

	cases := []struct {
		name string
		q    Query
		want error
	}{
		{"no-device", Query{Point: "p", From: now.Add(-time.Hour), To: now}, ErrInvalidQuery},
		{"no-point", Query{Device: "d", From: now.Add(-time.Hour), To: now}, ErrInvalidQuery},
		{"empty-range", Query{Device: "d", Point: "p", From: now, To: now}, ErrInvalidRange},
		{"backwards", Query{Device: "d", Point: "p", From: now, To: now.Add(-time.Hour)}, ErrInvalidRange},
		{"negative-step", Query{Device: "d", Point: "p", From: now.Add(-time.Hour), To: now, Step: -time.Second}, ErrInvalidQuery},
		{"agg-without-step", Query{Device: "d", Point: "p", From: now.Add(-time.Hour), To: now, Agg: AggAvg}, ErrInvalidQuery},
		{"unknown-agg", Query{Device: "d", Point: "p", From: now.Add(-time.Hour), To: now, Step: time.Second, Agg: "p95"}, ErrInvalidQuery},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Query(c.q); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestQueryBucketLimit(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	now := clock.Now()
	_, err := s.Query(Query{Device: "d", Point: "p",
		From: now.Add(-24 * time.Hour), To: now, Step: time.Second})
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryAggregations(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	// 两个 10s 桶：第一桶 1,2,3,4；第二桶 10。
	for i, v := range []float64{1, 2, 3, 4} {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", v))
	}
	mustWrite(t, s, numRec(base.Add(12*time.Second), "press", "power", 10))

	agg := func(a Aggregation) Bucket {
		t.Helper()
		res, err := s.Query(Query{Device: "press", Point: "power",
			From: base, To: base.Add(20 * time.Second), Step: 10 * time.Second, Agg: a})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Buckets) != 2 {
			t.Fatalf("%s buckets = %+v", a, res.Buckets)
		}
		return res.Buckets[0]
	}

	b := agg(AggLast)
	if b.Last.Num != 4 || b.First.Num != 1 || b.Count != 4 {
		t.Fatalf("last = %+v", b)
	}
	if b.Time != base {
		t.Fatalf("bucket time = %s, want %s", b.Time, base)
	}
	if b.Avg == nil || *b.Avg != 2.5 {
		t.Fatalf("avg = %v", b.Avg)
	}
	if b.Min == nil || *b.Min != 1 || b.Max == nil || *b.Max != 4 {
		t.Fatalf("min/max = %v/%v", b.Min, b.Max)
	}

	if b := agg(AggFirst); b.First.Num != 1 {
		t.Fatalf("first = %+v", b)
	}
	if b := agg(AggMin); b.Min == nil || *b.Min != 1 {
		t.Fatalf("min = %+v", b)
	}
	if b := agg(AggMax); b.Max == nil || *b.Max != 4 {
		t.Fatalf("max = %+v", b)
	}
	if b := agg(AggCount); b.Count != 4 {
		t.Fatalf("count = %+v", b)
	}

	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(20 * time.Second), Step: 10 * time.Second, Agg: AggLast})
	if err != nil {
		t.Fatal(err)
	}
	if res.Buckets[1].Time != base.Add(10*time.Second) || res.Buckets[1].Last.Num != 10 {
		t.Fatalf("second bucket = %+v", res.Buckets[1])
	}
	if res.Step.Duration() != 10*time.Second || res.Agg != AggLast {
		t.Fatalf("result metadata = %+v", res)
	}
}

func TestQueryBucketQualityTakesWorst(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	good := numRec(base, "press", "power", 1)
	warn := numRec(base.Add(time.Second), "press", "power", 2)
	warn.Quality = model.QualityUncertain
	bad := numRec(base.Add(2*time.Second), "press", "power", 3)
	bad.Quality = model.QualityCommError
	// 桶内最后一条是 good，但最差是 comm_error：整桶的均值不可信。
	last := numRec(base.Add(3*time.Second), "press", "power", 4)
	for _, r := range []Record{good, warn, bad, last} {
		mustWrite(t, s, r)
	}
	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(10 * time.Second), Step: 10 * time.Second, Agg: AggAvg})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 1 || res.Buckets[0].Quality != model.QualityCommError {
		t.Fatalf("bucket = %+v", res.Buckets)
	}
}

func TestQueryNonNumericAggregation(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	boolRec := func(off int, v bool) Record {
		return Record{Time: base.Add(time.Duration(off) * time.Second), Device: "press",
			Point: "running", Value: model.BoolValue(v), Quality: model.QualityGood}
	}
	textRec := func(off int, v string) Record {
		return Record{Time: base.Add(time.Duration(off) * time.Second), Device: "press",
			Point: "mode", Value: model.TextValue(model.TypeString, v), Quality: model.QualityGood}
	}
	for _, r := range []Record{boolRec(0, true), boolRec(1, false),
		textRec(0, "auto"), textRec(1, "manual")} {
		mustWrite(t, s, r)
	}

	for _, pt := range []string{"running", "mode"} {
		res, err := s.Query(Query{Device: "press", Point: pt,
			From: base, To: base.Add(10 * time.Second), Step: 10 * time.Second, Agg: AggAvg})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Buckets) != 1 {
			t.Fatalf("%s buckets = %+v", pt, res.Buckets)
		}
		b := res.Buckets[0]
		// Min/Max/Avg 必须是 null：把开关量平均成 0.5、把文本平均成 0 都是
		// 编造出来的数字。
		if b.Min != nil || b.Max != nil || b.Avg != nil {
			t.Fatalf("%s bucket has numeric aggregates: %+v", pt, b)
		}
		if b.Count != 2 {
			t.Fatalf("%s count = %d", pt, b.Count)
		}
	}
}

func TestQueryAggregationLimit(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	for i := 0; i < 20; i++ {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", float64(i)))
	}
	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(20 * time.Second), Step: time.Second, Agg: AggLast, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 5 || !res.Truncated || res.Count != 5 {
		t.Fatalf("len = %d truncated = %v count = %d", len(res.Buckets), res.Truncated, res.Count)
	}
}

func TestQueryAcrossRotatedSegments(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{SegmentBytes: 300, MaxBytes: 1 << 20})
	base := clock.Now()
	const n = 50
	for i := 0; i < n; i++ {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", float64(i)))
	}
	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base, To: base.Add(time.Hour), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != n {
		t.Fatalf("count = %d, want %d", res.Count, n)
	}
	sum := 0.0
	for _, p := range res.Points {
		sum += p.Value.Num
	}
	if want := float64(n*(n-1)) / 2; math.Abs(sum-want) > 1e-9 {
		t.Fatalf("sum = %v, want %v (segments missed or duplicated)", sum, want)
	}
}

func TestQueryUnwrittenStoreReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	now := clock.Now()
	res, err := s.Query(Query{Device: "d", Point: "p",
		From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 0 || len(res.Points) != 0 {
		t.Fatalf("result = %+v", res)
	}
}
