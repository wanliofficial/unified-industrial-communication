package outbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T, opt Options) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, dir
}

func enqueueN(t *testing.T, s *Store, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Enqueue("s1", fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"i":%d}`, i)), now); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}

func TestEnqueuePendingAck(t *testing.T) {
	s, dir := openTemp(t, Options{Sync: true})
	defer s.Close()
	now := time.Now()
	enqueueN(t, s, 3, now)

	st := s.Stats()
	if len(st) != 1 || st[0].Pending != 3 {
		t.Fatalf("stats = %+v", st)
	}
	recs, err := s.Pending("s1", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("pending = %d", len(recs))
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("seq[%d] = %d", i, r.Seq)
		}
		if string(r.Data) != fmt.Sprintf(`{"i":%d}`, i) {
			t.Fatalf("data[%d] = %s", i, r.Data)
		}
	}
	if err := s.Ack("s1", recs[1].Seq); err != nil {
		t.Fatal(err)
	}
	recs, _ = s.Pending("s1", 10, now)
	if len(recs) != 1 || recs[0].Seq != 3 {
		t.Fatalf("after ack: %+v", recs)
	}
	if st := s.Stats()[0]; st.Delivered != 2 || st.Pending != 1 {
		t.Fatalf("stats after ack = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "s1.cursor")); err != nil {
		t.Fatalf("cursor file missing: %v", err)
	}
}

// 未确认的消息必须在重启后重放（至少一次语义）。
func TestReplayAfterRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s, err := Open(dir, Options{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	enqueueN(t, s, 5, now)
	recs, _ := s.Pending("s1", 10, now)
	if len(recs) != 5 {
		t.Fatalf("pending = %d", len(recs))
	}
	if err := s.Ack("s1", recs[1].Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir, Options{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	recs, err = s2.Pending("s1", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("after restart pending = %d, want 3", len(recs))
	}
	if recs[0].Seq != 3 {
		t.Fatalf("first replay seq = %d, want 3", recs[0].Seq)
	}
	// 继续追加时序号不能回退。
	seq, err := s2.Enqueue("s1", "new", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 6 {
		t.Fatalf("next seq = %d, want 6", seq)
	}
}

// 崩溃留下的半行既不能被当作有效记录，也不能阻塞后续写入。
func TestRepairsTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s, err := Open(dir, Options{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	enqueueN(t, s, 2, now)
	s.Close()

	path := filepath.Join(dir, "s1.wal")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"sink":"s1","seq":3,"key":"trunc`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2, err := Open(dir, Options{Sync: true})
	if err != nil {
		t.Fatalf("reopen after truncation: %v", err)
	}
	defer s2.Close()
	recs, err := s2.Pending("s1", 10, now)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("pending = %d, want 2", len(recs))
	}
	if _, err := s2.Enqueue("s1", "after", []byte(`{}`), now); err != nil {
		t.Fatalf("append after truncated tail: %v", err)
	}
	recs, err = s2.Pending("s1", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || string(recs[2].Data) != `{}` {
		t.Fatalf("tail append broken: %+v", recs)
	}
}

func TestCountCapDropsOldest(t *testing.T) {
	s, _ := openTemp(t, Options{Sync: true, MaxRecords: 3})
	defer s.Close()
	now := time.Now()
	enqueueN(t, s, 10, now)

	st := s.Stats()[0]
	if st.Pending != 3 {
		t.Fatalf("pending = %d, want 3", st.Pending)
	}
	if st.Dropped["count"] != 7 {
		t.Fatalf("dropped = %+v", st.Dropped)
	}
	recs, _ := s.Pending("s1", 10, now)
	if len(recs) != 3 || recs[0].Seq != 8 {
		t.Fatalf("kept = %+v", recs)
	}
}

func TestSizeCapDropsOldest(t *testing.T) {
	// 每条记录约 60+ 字节；限制 200 字节只应保留 2-3 条。
	s, _ := openTemp(t, Options{Sync: true, MaxBytes: 200})
	defer s.Close()
	now := time.Now()
	enqueueN(t, s, 10, now)

	st := s.Stats()[0]
	if st.PendingBytes > 200 {
		t.Fatalf("bytes = %d, want <= 200", st.PendingBytes)
	}
	if st.Dropped["size"] == 0 {
		t.Fatalf("expected size drops, got %+v", st.Dropped)
	}
	if st.Pending == 0 {
		t.Fatal("size cap must keep newest records")
	}
}

func TestTTLDropsExpired(t *testing.T) {
	s, _ := openTemp(t, Options{Sync: true, TTL: time.Second})
	defer s.Close()
	old := time.Now().Add(-time.Hour)
	enqueueN(t, s, 2, old)
	// 过期判定在 Pending 时执行（Stats 是只读快照）。
	if _, err := s.Pending("s1", 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats()[0]; st.Pending != 0 || st.Dropped["ttl"] != 2 {
		t.Fatalf("stats = %+v", st)
	}
	now := time.Now()
	enqueueN(t, s, 2, now)
	if st := s.Stats()[0]; st.Pending != 2 {
		t.Fatalf("fresh records must survive, got %+v", st)
	}
}

// 已确认记录超过一半且超过 256 条时压缩 WAL。
func TestCompaction(t *testing.T) {
	s, dir := openTemp(t, Options{Sync: true})
	defer s.Close()
	now := time.Now()
	enqueueN(t, s, 400, now)
	before, err := os.Stat(filepath.Join(dir, "s1.wal"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := s.Pending("s1", 400, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 400 {
		t.Fatalf("pending = %d", len(recs))
	}
	if err := s.Ack("s1", recs[299].Seq); err != nil {
		t.Fatal(err)
	}
	if before.Size() == 0 {
		t.Fatal("wal should not be empty")
	}
	remaining, err := s.Pending("s1", 400, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 100 {
		t.Fatalf("remaining = %d, want 100", len(remaining))
	}
	if remaining[0].Seq != 301 {
		t.Fatalf("first remaining = %d", remaining[0].Seq)
	}
	after, _ := os.Stat(filepath.Join(dir, "s1.wal"))
	if after.Size() >= before.Size() {
		t.Fatalf("compaction did not shrink: %d -> %d", before.Size(), after.Size())
	}
	// 压缩后仍可继续写入，且位点正确。
	if _, err := s.Enqueue("s1", "post", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	remaining, _ = s.Pending("s1", 400, now)
	if len(remaining) != 101 || remaining[100].Seq != 401 {
		t.Fatalf("post-compaction append broken: len=%d", len(remaining))
	}
}

func TestDropBefore(t *testing.T) {
	s, _ := openTemp(t, Options{Sync: true})
	defer s.Close()
	base := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := s.Enqueue("s1", "k", []byte(`{}`), base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.DropBefore("s1", base.Add(2*time.Second)); n != 2 {
		t.Fatalf("dropped = %d, want 2", n)
	}
	if st := s.Stats()[0]; st.Pending != 2 || st.Dropped["manual"] != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestMultipleSinksAreIndependent(t *testing.T) {
	s, _ := openTemp(t, Options{Sync: true})
	defer s.Close()
	now := time.Now()
	enqueueN(t, s, 2, now)
	for i := 0; i < 3; i++ {
		if _, err := s.Enqueue("s2", "k", []byte(`{}`), now); err != nil {
			t.Fatal(err)
		}
	}
	recs1, _ := s.Pending("s1", 10, now)
	if len(recs1) != 2 {
		t.Fatalf("s1 pending = %d", len(recs1))
	}
	// ack s1 不应影响 s2。
	_ = s.Ack("s1", recs1[0].Seq)
	recs2, _ := s.Pending("s2", 10, now)
	if len(recs2) != 3 {
		t.Fatalf("s2 pending = %d", len(recs2))
	}
	if got := s.Sinks(); len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("sinks = %v", got)
	}
}

func TestClosedStoreRejectsWrites(t *testing.T) {
	s, _ := openTemp(t, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("s1", "k", []byte(`{}`), time.Now()); err != ErrClosed {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if _, err := s.Pending("s1", 1, time.Now()); err != ErrClosed {
		t.Fatalf("pending err = %v, want ErrClosed", err)
	}
	if err := s.Flush(); err != ErrClosed {
		t.Fatalf("flush err = %v, want ErrClosed", err)
	}
}
