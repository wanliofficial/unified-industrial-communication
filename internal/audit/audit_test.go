package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T, o Options) *Store {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 1 << 20
	}
	if o.SegmentBytes == 0 {
		o.SegmentBytes = 64 << 10
	}
	if o.MaxAge == 0 {
		o.MaxAge = 24 * time.Hour
	}
	s, err := Open(o)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendN(t *testing.T, s *Store, n int, user string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Append(Record{
			User: user, Role: "operator", Action: "point.write",
			Target: "plc/reg0", Result: ResultOK,
			Detail: map[string]string{"value": "1"},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

// TestAppendChainsRecords 验证序号递增与哈希链成立。
func TestAppendChainsRecords(t *testing.T) {
	s := openStore(t, Options{})

	var prev string
	for i := 1; i <= 5; i++ {
		rec, err := s.Append(Record{User: "alice", Action: "a", Result: ResultOK})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if rec.Seq != uint64(i) {
			t.Fatalf("seq = %d, want %d", rec.Seq, i)
		}
		if rec.Prev != prev {
			t.Fatalf("rec %d prev = %q, want %q", i, rec.Prev, prev)
		}
		if want := chainHash(rec); rec.Hash != want {
			t.Fatalf("rec %d hash = %q, want %q", i, rec.Hash, want)
		}
		prev = rec.Hash
	}

	res, err := s.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK || res.Checked != 5 {
		t.Fatalf("verify = %+v", res)
	}
}

// TestVerifyDetectsTampering 是哈希链存在的全部理由：改动一条记录必须被抓住。
func TestVerifyDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Dir: dir})
	appendN(t, s, 3, "alice")

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			path = filepath.Join(dir, e.Name())
		}
	}
	if path == "" {
		t.Fatal("no segment file")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 只改动第二条记录的目标字段，不动 hash——这正是「事后悄悄改一笔」。
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	lines[1] = strings.Replace(lines[1], `"target":"plc/reg0"`, `"target":"plc/reg9"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(Options{Dir: dir, SegmentBytes: 64 << 10, MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	res, err := s2.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.OK {
		t.Fatal("tampering was not detected")
	}
	if res.BrokenAt != 2 {
		t.Fatalf("brokenAt = %d, want 2 (reason: %s)", res.BrokenAt, res.Reason)
	}
	if !strings.Contains(res.Reason, "hash") {
		t.Fatalf("reason = %q, want it to mention the hash", res.Reason)
	}
}

// TestVerifyDetectsDeletedRecord 证明整条删除也会被序号跳空抓住。
func TestVerifyDetectsDeletedRecord(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Dir: dir})
	appendN(t, s, 4, "alice")

	ents, _ := os.ReadDir(dir)
	var path string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			path = filepath.Join(dir, e.Name())
		}
	}
	_ = s.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	kept := append(lines[:1], lines[2:]...)
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(Options{Dir: dir, SegmentBytes: 64 << 10, MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	res, err := s2.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("deleted record was not detected")
	}
	if !strings.Contains(res.Reason, "seq gap") {
		t.Fatalf("reason = %q, want a seq gap", res.Reason)
	}
}

// TestChainResumesAcrossRestart 保证重启后链能接上，而不是从 1 重来
// （重来会让 Verify 看到两条 seq=1）。
func TestChainResumesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Dir: dir})
	appendN(t, s, 3, "alice")
	before := s.Stats()
	_ = s.Close()

	s2, err := Open(Options{Dir: dir, SegmentBytes: 64 << 10, MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	rec, err := s2.Append(Record{User: "bob", Action: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Seq != before.LastSeq+1 {
		t.Fatalf("seq after restart = %d, want %d", rec.Seq, before.LastSeq+1)
	}
	res, err := s2.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Checked != 4 {
		t.Fatalf("verify = %+v", res)
	}
}

// TestTornTailStartsNewSegment 验证掉电留下的半行不会污染新记录。
func TestTornTailStartsNewSegment(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Dir: dir})
	appendN(t, s, 2, "alice")
	_ = s.Close()

	ents, _ := os.ReadDir(dir)
	var path string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			path = filepath.Join(dir, e.Name())
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// 没有换行的半行：模拟写到一半掉电。
	if _, err := f.WriteString(`{"seq":3,"user":"ali`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	s2, err := Open(Options{Dir: dir, SegmentBytes: 64 << 10, MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if _, err := s2.Append(Record{User: "bob", Action: "a"}); err != nil {
		t.Fatal(err)
	}
	res, err := s2.Query(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// 两条完整记录 + 新的一条；半行被跳过。
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
	for _, rec := range res.Items {
		if rec.User == "ali" {
			t.Fatal("torn record was parsed as a valid record")
		}
	}
}

// TestQueryPaging 验证游标分页不漏不重。
func TestQueryPaging(t *testing.T) {
	s := openStore(t, Options{})
	appendN(t, s, 25, "alice")

	var seen []uint64
	cursor := ""
	for page := 0; page < 10; page++ {
		res, err := s.Query(Filter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range res.Items {
			seen = append(seen, rec.Seq)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(seen) != 25 {
		t.Fatalf("got %d records across pages, want 25", len(seen))
	}
	// 倒序：25,24,...,1
	for i, seq := range seen {
		want := uint64(25 - i)
		if seq != want {
			t.Fatalf("position %d seq = %d, want %d", i, seq, want)
		}
	}
}

// TestQueryLastPageHasNoCursor 防止最后一页凭空多出一个游标（客户端会多请求一次）。
func TestQueryLastPageHasNoCursor(t *testing.T) {
	s := openStore(t, Options{})
	appendN(t, s, 5, "alice")

	res, err := s.Query(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.NextCursor != "" {
		t.Fatalf("nextCursor = %q, want empty on the last page", res.NextCursor)
	}
	if len(res.Items) != 5 {
		t.Fatalf("items = %d, want 5", len(res.Items))
	}
}

// TestQueryFilters 验证按用户/动作/结果/关键字过滤。
func TestQueryFilters(t *testing.T) {
	s := openStore(t, Options{})
	appendN(t, s, 2, "alice")
	if _, err := s.Append(Record{User: "bob", Action: "config.apply", Result: ResultDenied, Code: "auth.forbidden"}); err != nil {
		t.Fatal(err)
	}

	if res, _ := s.Query(Filter{User: "bob"}); res.Total != 1 {
		t.Fatalf("user filter = %d, want 1", res.Total)
	}
	if res, _ := s.Query(Filter{Action: "point.write"}); res.Total != 2 {
		t.Fatalf("action filter = %d, want 2", res.Total)
	}
	if res, _ := s.Query(Filter{Role: "operator"}); res.Total != 2 {
		t.Fatalf("role filter = %d, want 2", res.Total)
	}
	if res, _ := s.Query(Filter{Result: ResultDenied}); res.Total != 1 {
		t.Fatalf("result filter = %d, want 1", res.Total)
	}
	if res, _ := s.Query(Filter{Q: "forbidden"}); res.Total != 1 {
		t.Fatalf("q filter = %d, want 1", res.Total)
	}
}

// TestQueryRejectsBadInput 验证非法参数是客户端错误。
func TestQueryRejectsBadInput(t *testing.T) {
	s := openStore(t, Options{})
	appendN(t, s, 1, "alice")

	if _, err := s.Query(Filter{Cursor: "nope"}); err == nil {
		t.Fatal("bad cursor accepted")
	}
	now := time.Now()
	if _, err := s.Query(Filter{From: now, To: now.Add(-time.Hour)}); err == nil {
		t.Fatal("inverted range accepted")
	}
}

// TestRetentionDropsOldestSegments 验证按字节上限整段淘汰，且永远保留最后一段。
func TestRetentionDropsOldestSegments(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{
		Dir: dir, MaxBytes: 8 << 10, SegmentBytes: 2 << 10, MaxAge: 24 * time.Hour,
	})
	// 每条记录约 150 字节，写 200 条 ≈ 30KB，远超 8KB 上限。
	appendN(t, s, 200, "alice")

	st := s.Stats()
	if st.Evicted == 0 {
		t.Fatal("no segments were evicted; retention is not working")
	}
	if st.Segments == 0 {
		t.Fatal("all segments were removed; at least one must always stay")
	}
	if st.Bytes > st.MaxBytes*3/2 {
		t.Fatalf("bytes = %d, way above maxBytes %d", st.Bytes, st.MaxBytes)
	}

	// 最新的记录必须还在：淘汰只能发生在最旧的一端。
	res, err := s.Query(Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Seq != 200 {
		t.Fatalf("newest record = %+v", res.Items)
	}
	// 淘汰后链仍然可校验（起点允许 Prev 为空）。
	vres, err := s.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !vres.OK {
		t.Fatalf("verify after eviction = %+v", vres)
	}

	// Records 必须随整段淘汰一起下降，并与磁盘上真实剩余的条数一致。
	var onDisk uint64
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			onDisk += countRecords(filepath.Join(dir, e.Name()))
		}
	}
	if st.Records != onDisk {
		t.Fatalf("stats.records = %d, on disk = %d", st.Records, onDisk)
	}
	if st.Records == st.Appended {
		t.Fatalf("records = %d equals appended; eviction did not adjust the count", st.Records)
	}
}

// TestSegmentRotation 验证单段上限生效。
func TestSegmentRotation(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{
		Dir: dir, MaxBytes: 1 << 20, SegmentBytes: 1 << 10, MaxAge: 24 * time.Hour,
	})
	appendN(t, s, 100, "alice")

	ents, _ := os.ReadDir(dir)
	segs := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			segs++
		}
	}
	if segs < 3 {
		t.Fatalf("segments = %d, want at least 3 (rotation not working)", segs)
	}
}

// TestDirAndFileMode 验证审计文件不是所有人都能读。
//
// 只在会保留权限位的文件系统上断言：容器卷、FAT/exFAT 挂载点会忽略 0600，
// 在那里断言会得到一条与代码无关的失败。
func TestDirAndFileMode(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(probe); err == nil && info.Mode().Perm() != 0o600 {
		t.Skipf("filesystem does not preserve permission bits (probe mode %o)", info.Mode().Perm())
	}
	s := openStore(t, Options{Dir: dir})
	appendN(t, s, 1, "alice")

	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s mode = %o, want 600", e.Name(), mode)
		}
	}
}

// TestAppendAfterCloseFails 验证关闭后不再悄悄丢弃记录。
func TestAppendAfterCloseFails(t *testing.T) {
	s := openStore(t, Options{})
	_ = s.Close()
	if _, err := s.Append(Record{Action: "a"}); err == nil {
		t.Fatal("append after close succeeded")
	}
}

// TestRecordTimesAreUTC 保证对外时间戳统一 UTC（规范 §2.1）。
func TestRecordTimesAreUTC(t *testing.T) {
	s := openStore(t, Options{})
	loc := time.FixedZone("CST", 8*3600)
	rec, err := s.Append(Record{Action: "a", Time: time.Now().In(loc)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Time.Location() != time.UTC {
		t.Fatalf("stored time location = %v, want UTC", rec.Time.Location())
	}
	if _, offset := rec.Time.Zone(); offset != 0 {
		t.Fatalf("stored time offset = %d, want 0", offset)
	}
}

// TestDetailKeysAreCanonical 验证 Detail 顺序不影响哈希（否则同一份数据
// 两次写入会得到不同 hash，链校验随机失败）。
func TestDetailKeysAreCanonical(t *testing.T) {
	a := Record{Seq: 1, Time: time.Unix(1, 0).UTC(), User: "u", Action: "a",
		Detail: map[string]string{"b": "2", "a": "1", "c": "3"}}
	b := Record{Seq: 1, Time: time.Unix(1, 0).UTC(), User: "u", Action: "a",
		Detail: map[string]string{"c": "3", "a": "1", "b": "2"}}
	if chainHash(a) != chainHash(b) {
		t.Fatal("hash depends on map iteration order")
	}
}

// TestHashFieldSeparatorInjection 防止「值里塞分隔符」构造出碰撞。
func TestHashFieldSeparatorInjection(t *testing.T) {
	a := Record{Seq: 1, Time: time.Unix(1, 0).UTC(), User: "ab", Action: "c"}
	b := Record{Seq: 1, Time: time.Unix(1, 0).UTC(), User: "a", Action: "bc"}
	if chainHash(a) == chainHash(b) {
		t.Fatal("field boundary is not encoded; concatenation collision possible")
	}
}

// TestStatsReportsWatermarks 验证水位字段。
func TestStatsReportsWatermarks(t *testing.T) {
	s := openStore(t, Options{})
	appendN(t, s, 3, "alice")

	st := s.Stats()
	if st.Records != 3 || st.Appended != 3 || st.LastSeq != 3 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Oldest.IsZero() || st.Newest.IsZero() {
		t.Fatalf("watermarks = %+v", st)
	}
	if st.Segments != 1 {
		t.Fatalf("segments = %d, want 1", st.Segments)
	}

	// 重启后水位必须仍然准确：Records 反映磁盘上的记录数，Appended 只反映
	// 本进程写过多少。把两者混用，会让「审计还在工作吗」在每次重启后归零。
	dir := s.Dir()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, Options{Dir: dir})
	st2 := s2.Stats()
	if st2.Records != 3 || st2.Appended != 0 || st2.LastSeq != 3 {
		t.Fatalf("stats after reopen = %+v", st2)
	}
}

// TestEncodeDecodeRoundTrip 保证写出去的行能被原样读回来。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	rec := Record{
		Seq: 7, Time: time.Unix(1700000000, 123456789).UTC(),
		User: "alice", Role: "admin", Action: "point.write", Target: "plc/reg0",
		Result: ResultOK, Code: "", Message: "", Remote: "127.0.0.1:5000",
		RequestID: "req-1", Detail: map[string]string{"value": "42"},
		Prev: "abc", Hash: "def",
	}
	line, err := encodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecord(line)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(rec)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("round trip mismatch:\n%s\n%s", a, b)
	}
}

// TestEmptyDirIsAnError 验证目录必填（否则会在进程 cwd 里写出一堆文件）。
func TestEmptyDirIsAnError(t *testing.T) {
	if _, err := Open(Options{Dir: "   "}); err == nil {
		t.Fatal("empty dir accepted")
	}
}
