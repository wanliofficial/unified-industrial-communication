package history

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/model"
)

// testClock 是可推进的假时钟。所有用例都用它而不是 time.Now：
// 历史库的所有判定（轮转、保留期、查询区间）都基于时间，用真实时钟只能
// 靠 sleep 制造时间流逝，既慢又不稳定。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *testClock { return &testClock{t: t} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTest(t *testing.T, dir string, o Options) (*Store, *testClock) {
	t.Helper()
	clock := newClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	o.Dir = dir
	o.Now = clock.Now
	o.Log = quietLogger()
	s, err := Open(o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock
}

func numRec(at time.Time, dev, pt string, v float64) Record {
	return Record{Time: at, Device: dev, Point: pt,
		Value: model.NumValue(model.TypeFloat64, v), Quality: model.QualityGood}
}

func mustWrite(t *testing.T, s *Store, r Record) {
	t.Helper()
	if err := s.Write(r); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seg-") && strings.HasSuffix(e.Name(), ".ndjson") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 123_000_000, time.UTC)
	cases := []struct {
		name  string
		value model.Value
		check func(t *testing.T, got model.Value)
	}{
		{"float", model.NumValue(model.TypeFloat64, 73.25), func(t *testing.T, got model.Value) {
			if got.Type != model.TypeFloat64 || got.Num != 73.25 {
				t.Fatalf("got %+v", got)
			}
		}},
		{"int32-zero", model.NumValue(model.TypeInt32, 0), func(t *testing.T, got model.Value) {
			// 0 必须与「没有值」区分开：短键编码里 v 用 *float64。
			if got.Type != model.TypeInt32 || got.Num != 0 {
				t.Fatalf("got %+v", got)
			}
		}},
		{"bool-false", model.BoolValue(false), func(t *testing.T, got model.Value) {
			if got.Type != model.TypeBool || got.Bool {
				t.Fatalf("got %+v", got)
			}
		}},
		{"string-empty", model.TextValue(model.TypeString, ""), func(t *testing.T, got model.Value) {
			if got.Type != model.TypeString || got.Text != "" {
				t.Fatalf("got %+v", got)
			}
		}},
		{"string", model.TextValue(model.TypeString, "运行"), func(t *testing.T, got model.Value) {
			if got.Type != model.TypeString || got.Text != "运行" {
				t.Fatalf("got %+v", got)
			}
		}},
		{"bytes", model.TextValue(model.TypeBytes, "\x00\x01\xff"), func(t *testing.T, got model.Value) {
			if got.Type != model.TypeBytes || got.Text != "\x00\x01\xff" {
				t.Fatalf("got %+v", got)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, err := encodeRecord(Record{Time: at, Device: "press", Point: "p1",
				Value: c.value, Quality: model.QualityUncertain})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(line), "\n") {
				t.Fatal("encoded line must end with a newline")
			}
			rec, err := decodeRecord(line)
			if err != nil {
				t.Fatalf("decode: %v (%s)", err, line)
			}
			if !rec.Time.Equal(at) {
				t.Fatalf("time = %s, want %s", rec.Time, at)
			}
			if rec.Device != "press" || rec.Point != "p1" {
				t.Fatalf("ids = %s/%s", rec.Device, rec.Point)
			}
			if rec.Quality != model.QualityUncertain {
				t.Fatalf("quality = %s", rec.Quality)
			}
			c.check(t, rec.Value)
		})
	}
}

func TestEncodeRejectsIncomplete(t *testing.T) {
	if _, err := encodeRecord(Record{Point: "p"}); err == nil {
		t.Fatal("missing device must fail")
	}
	if _, err := encodeRecord(Record{Device: "d"}); err == nil {
		t.Fatal("missing point must fail")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	cases := []string{
		`not json`,
		`{"t":1,"d":"a"}`,                        // 缺 point
		`{"t":0,"d":"a","p":"b","v":1}`,          // 缺时间
		`{"t":1,"d":"a","p":"b","q":"good"}`,     // 没有任何值
		`{"t":1,"d":"a","p":"b","ty":"float64"}`, // 有类型但没有值
	}
	for _, c := range cases {
		if _, err := decodeRecord([]byte(c)); err == nil {
			t.Fatalf("%s: expected error", c)
		}
	}
}

func TestDecodeDefaultsQualityAndToleratesUnknownFields(t *testing.T) {
	line := []byte(`{"t":1700000000000,"d":"a","p":"b","v":1,"future":"x"}`)
	rec, err := decodeRecord(line)
	if err != nil {
		t.Fatal(err)
	}
	// 老记录可能没写质量码。默认 good 比默认 bad 更接近事实：bad 会让整段
	// 趋势在 UI 上被判为不可信。
	if rec.Quality != model.QualityGood {
		t.Fatalf("quality = %s", rec.Quality)
	}
}

func TestWriteFlushAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	for i := 0; i < 5; i++ {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", float64(i)))
	}
	clock.Advance(time.Minute)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开后应接着同一个段追加，而不是把旧数据丢掉。
	s2, err := Open(Options{Dir: dir, Now: clock.Now, Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if st := s2.Stats(); st.Appended != 0 || st.Segments != 1 || st.Bytes == 0 {
		t.Fatalf("stats after reopen = %+v", st)
	}
	mustWrite(t, s2, numRec(clock.Now(), "press", "power", 99))

	res, err := s2.Query(Query{Device: "press", Point: "power",
		From: base.Add(-time.Minute), To: clock.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 6 {
		t.Fatalf("count = %d, want 6", res.Count)
	}
	if got := res.Points[len(res.Points)-1].Value.Num; got != 99 {
		t.Fatalf("last = %v", got)
	}
	if len(segmentFiles(t, dir)) != 1 {
		t.Fatalf("files = %v", segmentFiles(t, dir))
	}
}

func TestRotationCreatesSegments(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{SegmentBytes: 400, MaxBytes: 1 << 20})
	base := clock.Now()
	for i := 0; i < 40; i++ {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", float64(i)))
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	files := segmentFiles(t, dir)
	if len(files) < 2 {
		t.Fatalf("expected rotation, files = %v", files)
	}
	for _, name := range files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 400*2 {
			// 单条记录约 70 字节，段上限 400：允许「最后一条放不下就换段」
			// 造成的轻微超出，但不应该翻倍。
			t.Fatalf("%s = %d bytes, way over the 400-byte limit", name, info.Size())
		}
	}
	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base.Add(-time.Second), To: base.Add(time.Hour), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 40 {
		t.Fatalf("count = %d, want 40", res.Count)
	}
	for i, p := range res.Points {
		if p.Value.Num != float64(i) {
			t.Fatalf("points[%d] = %v (order broken across segments)", i, p.Value.Num)
		}
	}
}

func TestEvictionByBytesKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	// 每段约 400 字节、上限 1200 字节：写到第 6 段时旧的必须被淘汰，
	// 且永远保留至少一段。
	s, clock := openTest(t, dir, Options{SegmentBytes: 300, MaxBytes: 900})
	base := clock.Now()
	for i := 0; i < 60; i++ {
		mustWrite(t, s, numRec(base.Add(time.Duration(i)*time.Second), "press", "power", float64(i)))
	}
	st := s.Stats()
	if st.Bytes > 900 {
		t.Fatalf("bytes = %d exceeds max 900", st.Bytes)
	}
	if st.Evicted == 0 {
		t.Fatal("nothing evicted")
	}
	if len(segmentFiles(t, dir)) == 0 {
		t.Fatal("all segments removed; at least the active one must survive")
	}
	// 最新的数据必须还在。
	res, err := s.Query(Query{Device: "press", Point: "power",
		From: base.Add(50 * time.Second), To: base.Add(time.Hour), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count == 0 || res.Points[len(res.Points)-1].Value.Num != 59 {
		t.Fatalf("newest sample lost: %+v", res.Points)
	}
}

func TestEvictionByAge(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{SegmentBytes: 260, MaxBytes: 1 << 20, MaxAge: time.Hour})
	// 每段两三条，时间跨度 1 分钟；写满两小时后最早的段早已过期。
	for i := 0; i < 120; i++ {
		// 时钟必须跟着记录一起走：保留期判定用的是「现在 - 最旧记录」，
		// 时钟不动的话，写两小时数据也永远没有东西过期。
		clock.Advance(time.Minute)
		mustWrite(t, s, numRec(clock.Now(), "press", "power", float64(i)))
	}
	st := s.Stats()
	if st.Evicted == 0 {
		t.Fatal("nothing evicted by age")
	}
	if !st.Oldest.IsZero() && clock.Now().Sub(st.Oldest) > 2*time.Hour {
		t.Fatalf("oldest = %s is far older than the retention window", st.Oldest)
	}
}

func TestAtLeastOneSegmentAlwaysRemains(t *testing.T) {
	dir := t.TempDir()
	// 上限小到任何一条记录都「超限」：仍然必须保留当前段，否则一台空闲
	// 网关会把自己的目录清空。
	s, clock := openTest(t, dir, Options{SegmentBytes: 64, MaxBytes: 64})
	mustWrite(t, s, numRec(clock.Now(), "press", "power", 1))
	if st := s.Stats(); st.Segments != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if len(segmentFiles(t, dir)) != 1 {
		t.Fatal("the active segment must never be evicted")
	}
}

func TestCorruptLinesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	base := clock.Now()
	mustWrite(t, s, numRec(base, "press", "power", 1))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 模拟掉电留下的半行：进程被杀时缓冲里只有半条记录。
	files := segmentFiles(t, dir)
	f, err := os.OpenFile(filepath.Join(dir, files[0]), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t":1762345678901,"d":"press","p":"pow`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// 重开时必须另起一段，而不是接着半行继续写。否则新记录会被拼进那条
	// 永远解析不了的垃圾行里，跟着一起丢掉——「重启后第一条数据消失」
	// 这种最难查的故障就是这么来的。
	s2, err := Open(Options{Dir: dir, Now: clock.Now, Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(segmentFiles(t, dir)) < 2 {
		t.Fatalf("torn tail must trigger a fresh segment, files = %v", segmentFiles(t, dir))
	}
	mustWrite(t, s2, numRec(base.Add(time.Second), "press", "power", 2))

	res, err := s2.Query(Query{Device: "press", Point: "power",
		From: base.Add(-time.Second), To: base.Add(time.Minute)})
	if err != nil {
		t.Fatalf("a corrupt line must not fail the query: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2", res.Count)
	}
	if res.Skipped == 0 {
		t.Fatal("skipped counter must report the corrupt line")
	}
	// 半行后面紧跟的正常记录不能被吞掉（这也是「一条记录不跨段」的原因）。
	if res.Points[1].Value.Num != 2 {
		t.Fatalf("points = %+v", res.Points)
	}
}

func TestFirstAndLastRecordTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seg-00000000000000000001.ndjson")
	good := func(n int64) string {
		b, err := encodeRecord(numRec(time.UnixMilli(n).UTC(), "d", "p", float64(n)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	body := "garbage\n" + good(1000) + good(2000) + `{"t":3000,"d":"d","p":"p"`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	start, ok := firstRecordTime(path)
	if !ok || !start.Equal(time.UnixMilli(1000).UTC()) {
		t.Fatalf("first = %s, ok = %v", start, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	end, ok := lastRecordTime(path, info.Size())
	if !ok || !end.Equal(time.UnixMilli(2000).UTC()) {
		t.Fatalf("last = %s, ok = %v", end, ok)
	}
}

func TestClosedStoreRejectsWritesAndQueries(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("double Close must be safe: %v", err)
	}
	if err := s.Write(numRec(clock.Now(), "d", "p", 1)); err != ErrClosed {
		t.Fatalf("write err = %v", err)
	}
	if _, err := s.Query(Query{Device: "d", Point: "p",
		From: clock.Now().Add(-time.Hour), To: clock.Now()}); err != ErrClosed {
		t.Fatalf("query err = %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush after close = %v", err)
	}
}

func TestOpenRequiresDir(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("empty dir must fail")
	}
}

func TestStatsAndDir(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{MaxBytes: 4096})
	mustWrite(t, s, numRec(clock.Now(), "d", "p", 1))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Appended != 1 || st.Segments != 1 || st.Bytes == 0 || st.MaxBytes != 4096 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Oldest.IsZero() || st.Newest.IsZero() {
		t.Fatalf("stats timestamps = %+v", st)
	}
	if s.Dir() != dir {
		t.Fatalf("dir = %q", s.Dir())
	}
	// Stats 要被诊断包直接序列化。
	if _, err := json.Marshal(st); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWriteAndQuery(t *testing.T) {
	dir := t.TempDir()
	s, clock := openTest(t, dir, Options{SegmentBytes: 2048, MaxBytes: 8192})
	base := clock.Now()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			_ = s.Write(numRec(base.Add(time.Duration(i)*time.Millisecond), "press", "power", float64(i)))
		}
		close(stop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 查询与淘汰可能并发删段：允许返回「此刻看不见」，但不能报错。
			if _, err := s.Query(Query{Device: "press", Point: "power",
				From: base.Add(-time.Second), To: base.Add(time.Minute),
				Step: time.Second, Agg: AggAvg, Limit: 100}); err != nil {
				t.Errorf("query: %v", err)
				return
			}
			_ = s.Stats()
			_ = s.Flush()
		}
	}()
	wg.Wait()
	if st := s.Stats(); st.Appended != 400 {
		t.Fatalf("appended = %d", st.Appended)
	}
}

// 段内记录时间并不保证单调：多台设备的采集互不相关，写入按事件到达顺序
// 落盘，后写的记录完全可能早于先写的。段索引的 end 必须是段内**最大**
// 时间，写成「最后一条」会得到 end < start 的自相矛盾区间，
// snapshotSegments 判重叠时整段跳过，段内仍落在窗口里的 good 样本全部
// 漏统计——趋势曲线会凭空少一段，「平均值」也随之偏低。
func TestSegmentIndexUsesMaxTimeNotLast(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s, _ := openTest(t, dir, Options{SegmentBytes: 1 << 20, MaxBytes: 1 << 20, MaxAge: 24 * time.Hour})

	// 先写较晚的 200s，再写较早的 150s（模拟跨设备交错到达）。
	if err := s.Write(numRec(base.Add(200*time.Second), "dev", "p", 20)); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(numRec(base.Add(150*time.Second), "dev", "p", 15)); err != nil {
		t.Fatal(err)
	}
	// 运行期：内存索引的 end 必须已经取最大值，不能等重启才自愈。
	if got := s.segs[0].end; got.Before(base.Add(200 * time.Second)) {
		t.Fatalf("live segment end = %v, want >= %v", got, base.Add(200*time.Second))
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开：scan 从末行恢复时必须得到同一个上界。
	clock := newClock(base.Add(210 * time.Second))
	s2, err := Open(Options{Dir: dir, SegmentBytes: 1 << 20, MaxBytes: 1 << 20,
		MaxAge: 24 * time.Hour, Now: clock.Now, Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, want := s2.segs[0].end, base.Add(200*time.Second); !got.Equal(want) {
		t.Fatalf("scanned segment end = %v, want %v", got, want)
	}
	st, err := s2.WindowStats("dev", "p", 50*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Count != 1 || st.Sum != 20 {
		t.Fatalf("WindowStats = count %d sum %v, want count 1 sum 20 (segment was skipped)", st.Count, st.Sum)
	}
}

// 旧版本可能已经把 end 写坏（end < start）。启动扫描必须自愈，否则这段
// 数据会在每次重启后继续被漏掉，且没有任何错误提示。
func TestScanRepairsTornSegmentBounds(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s, _ := openTest(t, dir, Options{SegmentBytes: 1 << 20, MaxBytes: 1 << 20, MaxAge: 24 * time.Hour})
	if err := s.Write(numRec(base.Add(200*time.Second), "dev", "p", 20)); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(numRec(base.Add(150*time.Second), "dev", "p", 15)); err != nil {
		t.Fatal(err)
	}
	path := s.segs[0].path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 人为把内存索引不存在、但「磁盘上的末行」就是较小时间的旧库形态
	// 交给 Open：它只能靠 scan 从末行推断，正是旧版本写坏的形态。
	clock := newClock(base.Add(210 * time.Second))
	s2, err := Open(Options{Dir: dir, SegmentBytes: 1 << 20, MaxBytes: 1 << 20,
		MaxAge: 24 * time.Hour, Now: clock.Now, Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	seg := s2.segs[0]
	if !seg.end.Equal(base.Add(200 * time.Second)) {
		t.Fatalf("segment %s end = %v, want %v", path, seg.end, base.Add(200*time.Second))
	}
	if seg.end.Before(seg.start) {
		t.Fatalf("segment bounds still inverted: start %v end %v", seg.start, seg.end)
	}
}
