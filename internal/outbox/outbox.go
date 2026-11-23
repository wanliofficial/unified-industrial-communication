// Package outbox 实现「断网不丢数据」的持久化上报队列。
//
// 设计要点（与 docs/03-product/02-architecture.md §7、05-northbound-api-spec.md §6.4 一致）：
//
//	按目标端点（sink）分区：每个 sink 一个 WAL 文件 + 一个确认位点文件；
//	追加写 + 定期 flush：正常路径不做每条 fsync（Sync 选项可开启）；
//	三重上限：条数 / 字节 / TTL，超限时「丢旧保新」并计数；
//	至少一次投递：投递成功后推进确认位点，重启后从位点重放；
//	定期压缩：已确认记录占比过半时重写文件，避免无限增长。
//
// 语义边界（必须明确，文档与实现一致）：
//   - 保证 at-least-once，不保证 exactly-once；去重键为 (sink,key,seq)；
//   - 单 sink 内按入队顺序投递；跨 sink 不保证；
//   - 进程崩溃时最多丢失最后一次 flush 之后的消息（默认 ≤ 200ms 或 64 条）。
package outbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrClosed 表示存储已关闭。
var ErrClosed = errors.New("outbox: closed")

// Options 是队列上限与持久化策略。
type Options struct {
	MaxRecords int64         // 单 sink 最大条数，默认 200000
	MaxBytes   int64         // 单 sink 最大字节数，默认 64MB
	TTL        time.Duration // 单条消息最长保留，默认 24h
	Sync       bool          // 每条 fsync（慢，但掉电不丢）
	Batch      int           // flush 阈值，默认 64 条
}

func (o Options) withDefaults() Options {
	if o.MaxRecords <= 0 {
		o.MaxRecords = 200_000
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 64 << 20
	}
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.Batch <= 0 {
		o.Batch = 64
	}
	return o
}

// Record 是队列中的一条消息。Data 由调用方定义（本产品中是 JSON 采样），
// 存储层不解释其内容。
type Record struct {
	Sink string    `json:"sink"`
	Seq  uint64    `json:"seq"`
	Key  string    `json:"key"`
	Ts   time.Time `json:"ts"`
	Data []byte    `json:"data"`
}

// SinkStats 是单个 sink 的队列状态。
type SinkStats struct {
	Name         string           `json:"name"`
	Pending      int64            `json:"pending"`
	PendingBytes int64            `json:"pendingBytes"`
	Delivered    uint64           `json:"delivered"`
	Dropped      map[string]int64 `json:"dropped,omitempty"`
	CursorSeq    uint64           `json:"cursorSeq"`
}

type entry struct {
	seq uint64
	off int64
	n   int32
	ts  int64 // unix nano
}

type sink struct {
	name       string
	dataPath   string
	cursorPath string

	w        *os.File
	bw       *bufio.Writer
	r        *os.File
	dirty    int
	lastSync time.Time

	cursorSeq uint64
	nextSeq   uint64
	entries   []entry
	head      int
	bytes     int64
	fileSize  int64
	dropped   map[string]int64
	delivered uint64
}

// Store 是 outbox 的根对象。
type Store struct {
	dir string
	opt Options

	mu     sync.Mutex
	sinks  map[string]*sink
	closed bool
}

type cursorFile struct {
	Acked uint64 `json:"acked"`
	Next  uint64 `json:"next"`
}

// Open 打开（或创建）outbox 目录，并加载所有已存在的 sink。
func Open(dir string, opt Options) (*Store, error) {
	if dir == "" {
		return nil, errors.New("outbox: dir required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("outbox: mkdir: %w", err)
	}
	s := &Store{dir: dir, opt: opt.withDefaults(), sinks: map[string]*sink{}}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("outbox: read dir: %w", err)
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wal") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".wal")
		if err := s.loadSink(name); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) sinkFiles(name string) (string, string) {
	return filepath.Join(s.dir, name+".wal"), filepath.Join(s.dir, name+".cursor")
}

func (s *Store) loadSink(name string) error {
	dataPath, cursorPath := s.sinkFiles(name)
	k := &sink{
		name: name, dataPath: dataPath, cursorPath: cursorPath,
		dropped: map[string]int64{}, lastSync: time.Now(),
	}
	if b, err := os.ReadFile(cursorPath); err == nil {
		var c cursorFile
		if json.Unmarshal(b, &c) == nil {
			k.cursorSeq = c.Acked
			k.nextSeq = c.Next
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("outbox: read cursor: %w", err)
	}

	f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("outbox: open wal: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("outbox: stat wal: %w", err)
	}
	k.fileSize = st.Size()
	// 崩溃可能留下「最后一行没有换行符」。先补一个换行，否则下一次追加会
	// 粘在残行后面，产生一条永远无法解析的记录。
	if k.fileSize > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], k.fileSize-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				f.Close()
				return fmt.Errorf("outbox: repair wal tail: %w", err)
			}
			k.fileSize++
		}
	}
	k.w = f
	k.bw = bufio.NewWriterSize(f, 64<<10)
	if err := k.scan(); err != nil {
		f.Close()
		return err
	}
	r, err := os.Open(dataPath)
	if err != nil {
		f.Close()
		return fmt.Errorf("outbox: open read handle: %w", err)
	}
	k.r = r
	if k.nextSeq == 0 {
		k.nextSeq = 1
	}
	s.sinks[name] = k
	return nil
}

// scan 重建内存索引：跳过已确认的记录，统计待投递条数与字节。
func (k *sink) scan() error {
	if k.fileSize == 0 {
		return nil
	}
	r, err := os.Open(k.dataPath)
	if err != nil {
		return fmt.Errorf("outbox: scan open: %w", err)
	}
	defer r.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	var off int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec Record
			if jerr := json.Unmarshal(line, &rec); jerr == nil {
				if rec.Seq > k.cursorSeq {
					k.entries = append(k.entries, entry{seq: rec.Seq, off: off, n: int32(len(line)), ts: rec.Ts.UnixNano()})
					k.bytes += int64(len(line))
				}
				if rec.Seq >= k.nextSeq {
					k.nextSeq = rec.Seq + 1
				}
			} else {
				// 半行/损坏行：直接跳过。WAL 的语义是「能读多少读多少」。
				_ = jerr
			}
			off += int64(len(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("outbox: scan: %w", err)
		}
	}
	return nil
}

// Enqueue 追加一条消息并返回其序号。
func (s *Store) Enqueue(sinkName, key string, data []byte, now time.Time) (uint64, error) {
	if sinkName == "" {
		return 0, errors.New("outbox: sink required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	k, err := s.ensureSink(sinkName)
	if err != nil {
		return 0, err
	}
	rec := Record{Sink: sinkName, Seq: k.nextSeq, Key: key, Ts: now, Data: data}
	line, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("outbox: marshal: %w", err)
	}
	line = append(line, '\n')
	off := k.fileSize
	if _, err := k.bw.Write(line); err != nil {
		return 0, fmt.Errorf("outbox: write: %w", err)
	}
	k.fileSize += int64(len(line))
	k.entries = append(k.entries, entry{seq: rec.Seq, off: off, n: int32(len(line)), ts: now.UnixNano()})
	k.bytes += int64(len(line))
	k.nextSeq++
	k.dirty++

	if dropped := s.enforce(k, now); dropped {
		if err := s.persistCursor(k); err != nil {
			return 0, err
		}
	}
	if s.opt.Sync || k.dirty >= s.opt.Batch {
		if err := k.flush(); err != nil {
			return 0, err
		}
	}
	return rec.Seq, nil
}

func (s *Store) ensureSink(name string) (*sink, error) {
	if k, ok := s.sinks[name]; ok {
		return k, nil
	}
	if err := s.loadSink(name); err != nil {
		return nil, err
	}
	k, ok := s.sinks[name]
	if !ok {
		return nil, fmt.Errorf("outbox: sink %q not created", name)
	}
	return k, nil
}

func (k *sink) flush() error {
	if k.bw != nil && k.bw.Buffered() > 0 {
		if err := k.bw.Flush(); err != nil {
			return fmt.Errorf("outbox: flush: %w", err)
		}
	}
	if k.w != nil && k.dirty > 0 {
		if err := k.w.Sync(); err != nil {
			return fmt.Errorf("outbox: sync: %w", err)
		}
	}
	k.dirty = 0
	k.lastSync = time.Now()
	return nil
}

// enforce 执行三重上限；丢旧保新。返回是否发生了丢弃（需要持久化位点）。
func (s *Store) enforce(k *sink, now time.Time) bool {
	ttl := s.opt.TTL.Nanoseconds()
	dropped := false
	for k.head < len(k.entries) {
		e := k.entries[k.head]
		reason := ""
		switch {
		case ttl > 0 && now.Sub(time.Unix(0, e.ts)) > s.opt.TTL:
			reason = "ttl"
		case int64(len(k.entries)-k.head) > s.opt.MaxRecords:
			reason = "count"
		case k.bytes > s.opt.MaxBytes:
			reason = "size"
		}
		if reason == "" {
			return dropped
		}
		k.dropHead(reason)
		dropped = true
	}
	return dropped
}

func (k *sink) dropHead(reason string) {
	if k.head >= len(k.entries) {
		return
	}
	e := k.entries[k.head]
	k.head++
	k.bytes -= int64(e.n)
	k.dropped[reason]++
	k.cursorSeq = e.seq
}

// Pending 返回最多 limit 条待投递记录（按入队顺序）。
// 调用方必须在投递成功后调用 Ack 推进位点。
func (s *Store) Pending(sinkName string, limit int, now time.Time) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	k, ok := s.sinks[sinkName]
	if !ok {
		return nil, nil
	}
	if err := k.flush(); err != nil {
		return nil, err
	}
	if s.enforce(k, now) {
		if err := s.persistCursor(k); err != nil {
			return nil, err
		}
	}
	if k.head >= len(k.entries) {
		return nil, nil
	}
	end := k.head + limit
	if end > len(k.entries) {
		end = len(k.entries)
	}
	out := make([]Record, 0, end-k.head)
	buf := make([]byte, 0, 512)
	// 逐条读取；损坏记录原地从索引里剔除，绝不推进 head / cursorSeq。
	//
	// 旧实现遇到损坏记录调用 dropHead，但 dropHead 只删「队头」——损坏的
	// 在批次中段时，被删掉的是队头那条**好且未 Ack** 的记录，并且 cursorSeq
	// 被推进。调用方还没拿到 Ack 确认，位点却已前移；若此刻崩溃，重启后
	// scan 会跳过 seq <= cursorSeq 的记录，那条好记录就永久丢了。这违反本包
	// 承诺的 at-least-once。损坏记录只是「读不出」不是「已送达」，它既不该
	// 带走别的好记录，也不该替调用方确认任何东西——与 scan 的语义一致
	// （scan 遇到反序列化失败同样不建索引）。
	i := k.head
	for i < end {
		e := k.entries[i]
		buf = buf[:0]
		if cap(buf) < int(e.n) {
			buf = make([]byte, e.n)
		}
		buf = buf[:e.n]
		if _, err := k.r.ReadAt(buf, e.off); err != nil {
			return out, fmt.Errorf("outbox: read record %d: %w", e.seq, err)
		}
		var rec Record
		if err := json.Unmarshal(buf, &rec); err != nil {
			// 损坏记录：原地剔除这一条，只扣它自己的字节，记一次 dropped；
			// head 仍是第一条未确认的好记录，cursorSeq 不动。i 不前进，
			// 因为 splice 后下标 i 处已是下一条。
			k.entries = append(k.entries[:i], k.entries[i+1:]...)
			k.bytes -= int64(e.n)
			k.dropped["corrupt"]++
			end--
			continue
		}
		out = append(out, rec)
		i++
	}
	return out, nil
}

// Ack 推进确认位点；落后于 head 的确认是幂等的空操作。
func (s *Store) Ack(sinkName string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	k, ok := s.sinks[sinkName]
	if !ok {
		return nil
	}
	advanced := false
	for k.head < len(k.entries) && k.entries[k.head].seq <= seq {
		e := k.entries[k.head]
		k.head++
		k.bytes -= int64(e.n)
		k.delivered++
		advanced = true
	}
	if !advanced {
		return nil
	}
	if k.head > 0 {
		k.cursorSeq = k.entries[k.head-1].seq
	}
	if err := s.persistCursor(k); err != nil {
		return err
	}
	if k.head >= 256 && k.head*2 >= len(k.entries) {
		return s.compact(k)
	}
	return nil
}

func (s *Store) persistCursor(k *sink) error {
	b, err := json.Marshal(cursorFile{Acked: k.cursorSeq, Next: k.nextSeq})
	if err != nil {
		return err
	}
	return atomicWrite(k.cursorPath, b)
}

// compact 重写 WAL，仅保留未确认记录。
func (s *Store) compact(k *sink) error {
	if err := k.flush(); err != nil {
		return err
	}
	tmpPath := k.dataPath + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("outbox: compact create: %w", err)
	}
	bw := bufio.NewWriterSize(tmp, 64<<10)
	newEntries := make([]entry, 0, len(k.entries)-k.head)
	var off int64
	buf := make([]byte, 0, 512)
	for _, e := range k.entries[k.head:] {
		if cap(buf) < int(e.n) {
			buf = make([]byte, e.n)
		}
		buf = buf[:e.n]
		if _, err := k.r.ReadAt(buf, e.off); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("outbox: compact read: %w", err)
		}
		if _, err := bw.Write(buf); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("outbox: compact write: %w", err)
		}
		newEntries = append(newEntries, entry{seq: e.seq, off: off, n: e.n, ts: e.ts})
		off += int64(e.n)
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if k.r != nil {
		_ = k.r.Close()
	}
	if k.w != nil {
		_ = k.w.Close()
	}
	if err := os.Rename(tmpPath, k.dataPath); err != nil {
		return fmt.Errorf("outbox: compact rename: %w", err)
	}
	f, err := os.OpenFile(k.dataPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("outbox: compact reopen: %w", err)
	}
	k.w = f
	k.bw = bufio.NewWriterSize(f, 64<<10)
	r, err := os.Open(k.dataPath)
	if err != nil {
		f.Close()
		return err
	}
	k.r = r
	k.entries = newEntries
	k.head = 0
	k.fileSize = off
	k.bytes = 0
	for _, e := range newEntries {
		k.bytes += int64(e.n)
	}
	k.dirty = 0
	return nil
}

// Flush 把所有 sink 的缓冲写入磁盘。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	for _, k := range s.sinks {
		if err := k.flush(); err != nil {
			return err
		}
	}
	return nil
}

// DropBefore 丢弃指定时间之前的消息（运维命令 outbox.drop）。
func (s *Store) DropBefore(sinkName string, before time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.sinks[sinkName]
	if !ok {
		return 0
	}
	var n int64
	for k.head < len(k.entries) && time.Unix(0, k.entries[k.head].ts).Before(before) {
		k.dropHead("manual")
		n++
	}
	if n > 0 {
		_ = s.persistCursor(k)
	}
	return n
}

// Stats 返回全部 sink 的状态（按名称排序）。
func (s *Store) Stats() []SinkStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SinkStats, 0, len(s.sinks))
	for _, k := range s.sinks {
		dropped := make(map[string]int64, len(k.dropped))
		for r, n := range k.dropped {
			dropped[r] = n
		}
		out = append(out, SinkStats{
			Name: k.name, Pending: int64(len(k.entries) - k.head),
			PendingBytes: k.bytes, Delivered: k.delivered,
			Dropped: dropped, CursorSeq: k.cursorSeq,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Dir 返回 Outbox 根目录。
func (s *Store) Dir() string { return s.dir }

// Sinks 返回已存在的 sink 名称。
func (s *Store) Sinks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sinks))
	for n := range s.sinks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Close 刷新并关闭全部文件句柄。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var first error
	for _, k := range s.sinks {
		if err := k.flush(); err != nil && first == nil {
			first = err
		}
		_ = s.persistCursor(k)
		if k.r != nil {
			_ = k.r.Close()
		}
		if k.w != nil {
			_ = k.w.Close()
		}
	}
	return first
}

// atomicWrite 写临时文件 + fsync + 原子改名。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
