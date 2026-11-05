// Package history 是有界的点位采样日志。
//
// 它不是时序数据库：没有索引文件、没有压缩、没有跨点位 join、没有 SQL。
// 结构只有两件事——**按时间切段的追加文件**与**超限淘汰**，查询是「扫重叠
// 段 + 过滤 + 可选定步长聚合」。这样做的理由是硬约束：本项目零第三方依赖、
// 零 CGO、单个二进制可交叉编译；纯 Go 的 SQLite 要引入数万行转译代码，
// CGO 版本直接破坏交叉编译，而为「按点位查一段时间」引入数据库也过重。
//
// 边界（写进 README 与 NFR，避免被当成 TSDB 用）：
//   - 只支持等值过滤 device+point，不支持多点点位组合查询；
//   - 聚合在扫描时算，不物化，因此跨度越大越慢（由 maxRange 限制）；
//   - 写入是「批量缓冲 + 定期 flush」，进程崩溃最多丢一个 flush 周期；
//   - 最旧的段整段淘汰，不做样本级裁剪。
//
// 需要 SQL 的部署可以把 Store 换掉：上层（runtime/httpapi）只依赖
// Write / Query / Stats / Flush / Close 五个方法。
package history

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"uic/internal/model"
)

const (
	// DefaultSegmentBytes 是单段的默认大小。16MB 约等于 20 万条记录：
	// 够大，查询不必打开几百个文件；又够小，淘汰时不会一次扔掉一周数据。
	DefaultSegmentBytes int64 = 16 << 20
	// DefaultMaxBytes 是历史目录的默认上限。
	DefaultMaxBytes int64 = 256 << 20
	// DefaultMaxAge 是默认保留期。
	DefaultMaxAge = 7 * 24 * time.Hour
	// DefaultLimit 是单次查询默认返回的原始点数。
	DefaultLimit = 1000
	// MaxLimit 是单次查询允许的最大原始点数，防止一次拉爆内存。
	MaxLimit = 10000
	// MaxBuckets 是聚合查询允许的最大桶数。
	MaxBuckets = 5000
)

// ErrClosed 表示 Store 已关闭。
var ErrClosed = errors.New("history: store is closed")

// ErrInvalidRange 表示查询时间区间不合法。
var ErrInvalidRange = errors.New("history: invalid time range")

// ErrInvalidQuery 表示查询参数不合法。
var ErrInvalidQuery = errors.New("history: invalid query")

// Record 是一条采样记录。
type Record struct {
	Time    time.Time
	Device  string
	Point   string
	Value   model.Value
	Quality model.Quality
}

// Options 是 Open 的参数。零值字段使用默认值。
type Options struct {
	Dir          string
	MaxBytes     int64
	MaxAge       time.Duration
	SegmentBytes int64
	// Log 只用于报告淘汰/写入失败；历史是有损的旁路，不允许影响采集。
	Log *slog.Logger
	// Now 注入时钟（测试与「同一纳秒内创建多个段」的确定性）。
	Now func() time.Time
}

// Stats 是历史库的水位。
type Stats struct {
	Segments    int       `json:"segments"`
	Bytes       int64     `json:"bytes"`
	MaxBytes    int64     `json:"maxBytes"`
	Oldest      time.Time `json:"oldest,omitempty"`
	Newest      time.Time `json:"newest,omitempty"`
	Appended    uint64    `json:"appended"`
	WriteErrors uint64    `json:"writeErrors"`
	Evicted     uint64    `json:"evicted"`
}

type segment struct {
	path  string
	start time.Time
	end   time.Time
	size  int64
	f     *os.File
	w     *bufio.Writer
}

func (s *segment) close() error {
	var firstErr error
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			firstErr = err
		}
	}
	if s.f != nil {
		if err := s.f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.w, s.f = nil, nil
	return firstErr
}

// Store 是一份有界的历史采样日志。所有方法并发安全。
type Store struct {
	mu           sync.Mutex
	dir          string
	maxBytes     int64
	segmentBytes int64
	maxAge       time.Duration
	now          func() time.Time
	log          *slog.Logger

	segs        []*segment // 按 start 升序；最后一个可能是 active
	active      *segment
	bytes       int64
	appended    uint64
	writeErrors uint64
	evicted     uint64
	closed      bool
}

// Open 打开（或创建）历史目录，扫描已有段并恢复可追加位置。
func Open(o Options) (*Store, error) {
	if strings.TrimSpace(o.Dir) == "" {
		return nil, errors.New("history: dir is required")
	}
	if o.SegmentBytes <= 0 {
		o.SegmentBytes = DefaultSegmentBytes
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.SegmentBytes > o.MaxBytes {
		o.SegmentBytes = o.MaxBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("history: create dir: %w", err)
	}
	s := &Store{
		dir: o.Dir, maxBytes: o.MaxBytes, segmentBytes: o.SegmentBytes,
		maxAge: o.MaxAge, now: o.Now, log: o.Log,
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	if err := s.openActive(); err != nil {
		return nil, err
	}
	// 打开时也执行一次淘汰：崩溃重启不会跳过保留策略。
	s.enforceLocked()
	return s, nil
}

// scan 读取目录里已有的段，按首条记录时间排序。
//
// 只读每个文件的首行与末行：段内的记录按时间单调追加，首行是下界，末行是
// 上界。读全文件会让重启耗时与历史数据量成正比，而启动时间是边缘设备最
// 敏感的指标之一。不用 mtime 作上界：mtime 是「最后一次写」，但保留期要
// 判断的是「数据有多老」，两者只在正常退出时一致；掉电重启后 mtime 可能
// 比真实数据新，会让本该淘汰的段被当成新鲜的。
func (s *Store) scan() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("history: read dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".ndjson") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(s.dir, name)
		seg := &segment{path: path, size: info.Size(), end: info.ModTime().UTC()}
		if start, ok := firstRecordTime(path); ok {
			seg.start = start
		} else {
			seg.start = info.ModTime().UTC()
		}
		if end, ok := lastRecordTime(path, info.Size()); ok {
			seg.end = end
		} else {
			seg.end = info.ModTime().UTC()
		}
		s.segs = append(s.segs, seg)
		s.bytes += info.Size()
	}
	sort.Slice(s.segs, func(i, j int) bool {
		if s.segs[i].start.Equal(s.segs[j].start) {
			return s.segs[i].path < s.segs[j].path
		}
		return s.segs[i].start.Before(s.segs[j].start)
	})
	return nil
}

func firstRecordTime(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	for i := 0; i < 4; i++ { // 容忍开头最多 3 行垃圾
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if rec, derr := decodeRecord(line); derr == nil {
				return rec.Time, true
			}
		}
		if err != nil {
			return time.Time{}, false
		}
	}
	return time.Time{}, false
}

// endsWithNewline 检查段文件是否以换行结束（空文件视为安全）。
func endsWithNewline(path string, size int64) (bool, error) {
	if size <= 0 {
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return false, err
	}
	return b[0] == '\n', nil
}

// lastRecordTime 从文件尾部最多读 64KB，取最后一条可解析的记录。
// 尾部可能因为掉电留下半行，所以从后往前找第一条能解析的。
func lastRecordTime(path string, size int64) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	const tail = 64 << 10
	off := size - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, false
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i]) == 0 {
			continue
		}
		if rec, derr := decodeRecord(lines[i]); derr == nil {
			return rec.Time, true
		}
	}
	return time.Time{}, false
}

// openActive 打开最后一个段继续追加；它已经接近段上限时改开新段。
func (s *Store) openActive() error {
	if n := len(s.segs); n > 0 {
		last := s.segs[n-1]
		ok, err := endsWithNewline(last.path, last.size)
		if err != nil {
			s.log.Warn("history_segment_probe_failed", "path", last.path, "error", err.Error())
		}
		if !ok {
			// 掉电留下半行：绝不能接着这一行写，否则新记录会被拼进那条
			// 永远解析不了的垃圾行里，一起丢掉。旧段保留（前面的记录仍可
			// 查），新开一段从干净的边界开始。
			s.log.Warn("history_segment_torn_tail", "path", last.path)
		} else if last.size < s.segmentBytes*3/4 {
			f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				last.f = f
				last.w = bufio.NewWriterSize(f, 64<<10)
				s.active = last
				return nil
			}
			// 打开失败不该让整个运行时起不来：退化为开新段，旧段保留可查。
			s.log.Warn("history_segment_open_failed", "path", last.path, "error", err.Error())
		}
	}
	return s.createSegmentLocked()
}

func (s *Store) createSegmentLocked() error {
	// 所有落盘与对外暴露的时间统一用 UTC：同一份响应里混着本地时区
	// 与 Z 结尾的时间戳，UI 上会显示成相差 8 小时的两条曲线。
	now := s.now().UTC()
	for i := 0; i < 100; i++ {
		path := filepath.Join(s.dir, fmt.Sprintf("seg-%020d.ndjson", now.UnixNano()+int64(i)))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("history: create segment: %w", err)
		}
		seg := &segment{path: path, f: f, w: bufio.NewWriterSize(f, 64<<10), start: now, end: now}
		s.segs = append(s.segs, seg)
		s.active = seg
		return nil
	}
	return errors.New("history: cannot allocate a unique segment name")
}

// Write 追加一条记录。写失败只返回错误并计数，绝不 panic / 阻塞采集。
func (s *Store) Write(r Record) error {
	if r.Quality == "" {
		r.Quality = model.QualityGood
	}
	if r.Time.IsZero() {
		r.Time = s.now()
	}
	r.Time = r.Time.UTC()
	line, err := encodeRecord(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.active == nil {
		if err := s.createSegmentLocked(); err != nil {
			s.writeErrors++
			return err
		}
	}
	// 先判断再加：一条记录不能跨段，否则被切开的半行会让整段尾部不可读。
	if s.active.size > 0 && s.active.size+int64(len(line)) > s.segmentBytes {
		if err := s.rotateLocked(); err != nil {
			s.writeErrors++
			return err
		}
	}
	n, err := s.active.w.Write(line)
	if err != nil {
		s.writeErrors++
		return err
	}
	s.active.size += int64(n)
	s.active.end = r.Time
	if s.active.start.IsZero() || r.Time.Before(s.active.start) {
		s.active.start = r.Time
	}
	s.bytes += int64(n)
	s.appended++
	// 每条都检查一次上限是廉价的（只是比较），而等到 flush 周期再检查，
	// 突发写入可能在两次检查之间把磁盘写满。
	s.enforceLocked()
	return nil
}

func (s *Store) rotateLocked() error {
	if s.active != nil {
		if err := s.active.close(); err != nil {
			s.log.Warn("history_segment_close_failed", "path", s.active.path, "error", err.Error())
		}
		s.active = nil
	}
	return s.createSegmentLocked()
}

// enforceLocked 按「总字节上限」与「保留期」淘汰最旧的段。
//
// 永远保留至少一个段（当前正在写的那个）：否则一台长期空闲的网关会在启动
// 后立刻把自己的目录清空，而「没有数据」与「数据被淘汰」在 API 上无法区分。
func (s *Store) enforceLocked() {
	now := s.now()
	for len(s.segs) > 1 {
		oldest := s.segs[0]
		if oldest == s.active {
			return
		}
		overBytes := s.bytes > s.maxBytes
		overAge := s.maxAge > 0 && !oldest.end.IsZero() && now.Sub(oldest.end) > s.maxAge
		if !overBytes && !overAge {
			return
		}
		if err := os.Remove(oldest.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("history_segment_remove_failed", "path", oldest.path, "error", err.Error())
			return
		}
		s.bytes -= oldest.size
		if s.bytes < 0 {
			s.bytes = 0
		}
		s.segs = s.segs[1:]
		s.evicted++
	}
}

// Flush 把缓冲写入操作系统。它不 fsync：历史是可重算的旁路数据，
// 每个 flush 周期都 fsync 会让 SD 卡寿命与写入延迟都变得不可接受。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.active == nil || s.active.w == nil {
		return nil
	}
	return s.active.w.Flush()
}

// Close 刷盘、fsync 并关闭当前段。重复调用安全。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.active == nil {
		return nil
	}
	return s.active.close()
}

// Stats 返回当前水位。
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Segments: len(s.segs), Bytes: s.bytes, MaxBytes: s.maxBytes,
		Appended: s.appended, WriteErrors: s.writeErrors, Evicted: s.evicted,
	}
	if len(s.segs) > 0 {
		st.Oldest = s.segs[0].start
		st.Newest = s.segs[len(s.segs)-1].end
	}
	return st
}

// Dir 返回历史目录（诊断包使用）。
func (s *Store) Dir() string { return s.dir }

// segmentSnapshot 是查询时对段索引的只读快照：查询不能在持锁状态下扫文件，
// 否则一次大范围查询会阻塞采集写入。
type segmentSnapshot struct {
	path       string
	start, end time.Time
}

func (s *Store) snapshotSegments(from, to time.Time) ([]segmentSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	// 先把缓冲区刷出去，否则「刚写进去的最新点」在查询里看不到，
	// 表现为监控页面的曲线总比实时值慢一个 flush 周期。
	if s.active != nil && s.active.w != nil {
		if err := s.active.w.Flush(); err != nil {
			s.writeErrors++
		}
	}
	out := make([]segmentSnapshot, 0, len(s.segs))
	for _, seg := range s.segs {
		if seg.size == 0 {
			continue
		}
		if seg.end.Before(from) || seg.start.After(to) {
			continue
		}
		out = append(out, segmentSnapshot{path: seg.path, start: seg.start, end: seg.end})
	}
	return out, nil
}

// scanSegment 逐行解析一个段。损坏的行被跳过并计数：历史文件可能因为掉电
// 留下半行，丢一条记录远好过让整段查询失败。
func scanSegment(path string, onRecord func(Record) bool) (read, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 256<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			read++
			if rec, derr := decodeRecord(line); derr == nil {
				if !onRecord(rec) {
					return read, skipped, nil
				}
			} else {
				skipped++
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return read, skipped, nil
			}
			return read, skipped, rerr
		}
	}
}
