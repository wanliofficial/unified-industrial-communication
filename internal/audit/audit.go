// Package audit 是「谁在何时对什么做了什么」的追加日志。
//
// 它与 history 是两类东西，尽管形状相似（都是 NDJSON 分段）：
//
//   - history 是**有损缓存**：写不进去就丢最新、放不下就淘汰最旧，丢一条
//     只影响一张趋势图；
//   - audit 是**证据**：必须能回答「上周三谁把 setpoint 改成了 120」，
//     因此每条记录带序号与前一条的哈希，形成可校验的链。篡改或误删中间
//     某条，Verify 会指出来。
//
// 但审计同样必须有界。无界日志最终会写满边缘设备的磁盘，而写满之后网关
// 连采集都跑不动——那时运维第一件事就是关掉审计。因此这里采用与 history
// 相同的保留策略：整段淘汰、保留期到期删除，并把淘汰事实记进 Stats。
//
// 边界（写进文档，避免被当成 SIEM 用）：
//   - 不提供实时转发（需要的话由 Outbox/Webhook 做，那是投递不是存储）；
//   - 不支持按字段建索引，查询是「扫重叠段 + 过滤」，由 maxBytes 保证上界；
//   - 哈希链防的是「事后悄悄改动」，不防「有磁盘写权限的人整段删除」
//     （删除会在 Verify 里表现为序号跳空，能被看见，但无法恢复）。
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultSegmentBytes 决定单段上限。审计量远小于采集量（一次写入一条），
	// 8MB 约等于 5 万条记录，够一个中型站点写几个月。
	DefaultSegmentBytes int64 = 8 << 20
	// DefaultMaxBytes 是审计目录默认上限。
	DefaultMaxBytes int64 = 64 << 20
	// DefaultMaxAge 是默认保留期。等保/审计通常要求 6 个月，但边缘设备
	// 磁盘有限，默认取 90 天并要求部署方按合规要求调大。
	DefaultMaxAge = 90 * 24 * time.Hour
	// DefaultLimit 是单次查询默认返回条数。
	DefaultLimit = 100
	// MaxLimit 是单次查询上限，防止一次拉爆内存。
	MaxLimit = 5000
	// maxScanRecords 是单次查询最多扫描的记录数。记录再多也会被 maxBytes
	// 挡住，这里只是兜底，避免配置被改坏时把进程拖死。
	maxScanRecords = 1 << 20
)

// Result 取值。三态而不是布尔：运维要能一眼区分「被拒绝」与「执行失败」——
// 前者是权限问题，后者要去查设备。
const (
	ResultOK     = "ok"
	ResultDenied = "denied"
	ResultError  = "error"
)

// ErrClosed 表示 Store 已关闭。
var ErrClosed = errors.New("audit: store is closed")

// ErrInvalidQuery 表示查询参数不合法。
var ErrInvalidQuery = errors.New("audit: invalid query")

// Record 是一条审计记录。
//
// Detail 只存字符串：哈希链要对记录做规范化编码，允许任意 JSON 类型就得
// 处理浮点格式化、map 顺序等一堆不稳定因素。审计的字段本来就该是「可读的
// 事实」而不是嵌套结构。
type Record struct {
	Seq       uint64            `json:"seq"`
	Time      time.Time         `json:"time"`
	User      string            `json:"user"`
	Role      string            `json:"role"`
	Action    string            `json:"action"`
	Target    string            `json:"target,omitempty"`
	Result    string            `json:"result"`
	Code      string            `json:"code,omitempty"`
	Message   string            `json:"message,omitempty"`
	Remote    string            `json:"remote,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
	Detail    map[string]string `json:"detail,omitempty"`
	Prev      string            `json:"prev,omitempty"`
	Hash      string            `json:"hash,omitempty"`
}

// Options 是 Open 的参数。零值字段使用默认值。
type Options struct {
	Dir          string
	MaxBytes     int64
	MaxAge       time.Duration
	SegmentBytes int64
	// Log 只用于报告淘汰与写入失败。审计写失败不允许回滚业务动作，
	// 但必须在日志与指标里留下痕迹。
	Log *slog.Logger
	// Now 注入时钟（测试用）。
	Now func() time.Time
}

// Stats 是审计日志的水位。
type Stats struct {
	Records     uint64    `json:"records"`
	Bytes       int64     `json:"bytes"`
	MaxBytes    int64     `json:"maxBytes"`
	Segments    int       `json:"segments"`
	Oldest      time.Time `json:"oldest,omitempty"`
	Newest      time.Time `json:"newest,omitempty"`
	Appended    uint64    `json:"appended"`
	Evicted     uint64    `json:"evicted"`
	WriteErrors uint64    `json:"writeErrors"`
	LastSeq     uint64    `json:"lastSeq"`
}

// Filter 是一次查询。
type Filter struct {
	From   time.Time
	To     time.Time
	User   string
	Role   string
	Action string
	Result string
	Q      string
	Limit  int
	Cursor string
}

// Result 是查询结果。Items 按时间倒序（最新在前）。
//
// Total 是「本次扫描范围内命中的条数」，不一定是全库命中数：查询为了
// 「最近 100 条不必读完 90 天日志」会在凑够一页后停止。Scanned 说明这次
// 到底扫了多少条，让调用方能判断 Total 的可信度。
type Result struct {
	Items      []Record `json:"items"`
	NextCursor string   `json:"nextCursor,omitempty"`
	Total      int      `json:"total"`
	Scanned    int      `json:"scanned"`
}

// VerifyResult 是哈希链校验结果。
type VerifyResult struct {
	Checked  int    `json:"checked"`
	OK       bool   `json:"ok"`
	BrokenAt uint64 `json:"brokenAt,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type segment struct {
	path  string
	start time.Time
	size  int64
	// records 是这段里完整的记录条数。Stats 的 Records 要跨重启仍然准确，
	// 就不能只靠「本进程 Append 了几次」；淘汰整段时也要能把这部分减掉。
	records uint64
	f       *os.File
	w       *bufio.Writer
}

func (s *segment) close() error {
	var first error
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			first = err
		}
	}
	if s.f != nil {
		if err := s.f.Sync(); err != nil && first == nil {
			first = err
		}
		if err := s.f.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.w, s.f = nil, nil
	return first
}

// Store 是一份有界审计日志。所有方法并发安全。
type Store struct {
	mu           sync.Mutex
	dir          string
	maxBytes     int64
	segmentBytes int64
	maxAge       time.Duration
	now          func() time.Time
	log          *slog.Logger

	segs     []*segment // 按 start 升序
	active   *segment
	bytes    int64
	seq      uint64
	lastHash string
	count    uint64

	appended    uint64
	evicted     uint64
	writeErrors uint64
	closed      bool
}

// Open 打开（或创建）审计目录，并恢复哈希链的尾状态。
func Open(o Options) (*Store, error) {
	if strings.TrimSpace(o.Dir) == "" {
		return nil, errors.New("audit: dir is required")
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
		return nil, fmt.Errorf("audit: create dir: %w", err)
	}
	s := &Store{
		dir: o.Dir, maxBytes: o.MaxBytes, segmentBytes: o.SegmentBytes,
		maxAge: o.MaxAge, now: o.Now, log: o.Log,
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	if err := s.resumeChain(); err != nil {
		return nil, err
	}
	if err := s.openActive(); err != nil {
		return nil, err
	}
	s.enforceLocked()
	return s, nil
}

// scan 读取目录里已有的段。首行取起点，大小取文件信息，记录条数则要读
// 整段——Stats 的 Records 在重启后必须仍然准确，否则「审计还在工作吗」
// 这个最常被问的问题会在每次重启后给出 0。
func (s *Store) scan() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("audit: read dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".ndjson") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(s.dir, name)
		seg := &segment{path: path, size: info.Size(), start: info.ModTime().UTC()}
		if t, ok := firstRecordTime(path); ok {
			seg.start = t
		}
		seg.records = countRecords(path)
		s.segs = append(s.segs, seg)
		s.bytes += info.Size()
		s.count += seg.records
	}
	sort.Slice(s.segs, func(i, j int) bool {
		if s.segs[i].start.Equal(s.segs[j].start) {
			return s.segs[i].path < s.segs[j].path
		}
		return s.segs[i].start.Before(s.segs[j].start)
	})
	return nil
}

// resumeChain 从最后一段的尾部取回 seq 与 hash，让重启后链能接上。
//
// 尾部可能因为掉电留下半行：从后往前找第一条能解析的记录。找不到就用
// 空链起点（seq=0）——这意味着链从这里重新开始，Verify 会把之前的段
// 视为「没有可校验的链」，而不是伪造一个连不上的 Prev。
func (s *Store) resumeChain() error {
	if len(s.segs) == 0 {
		return nil
	}
	last := s.segs[len(s.segs)-1]
	rec, ok := lastRecord(last.path, last.size)
	if !ok {
		s.log.Warn("audit_chain_head_missing", "path", last.path)
		return nil
	}
	s.seq = rec.Seq
	s.lastHash = rec.Hash
	return nil
}

// countRecords 数一段里完整的记录行（以 '\n' 结尾且非空）。
//
// 末尾可能因为掉电留下半行：它不会被计入——那条记录没有成功返回给调用方，
// 也不该出现在水位里。
func countRecords(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 256<<10)
	var n uint64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 1 && line[len(line)-1] == '\n' && len(bytes.TrimSpace(line)) > 0 {
			n++
		}
		if err != nil {
			return n
		}
	}
}

func firstRecordTime(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	for i := 0; i < 4; i++ {
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

func lastRecord(path string, size int64) (Record, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false
	}
	defer f.Close()
	const tail = 64 << 10
	off := size - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return Record{}, false
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i]) == 0 {
			continue
		}
		if rec, derr := decodeRecord(lines[i]); derr == nil {
			return rec, true
		}
	}
	return Record{}, false
}

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

// openActive 打开最后一个段继续追加。与 history 同样的规则：尾部不完整
// 就换新段，绝不把新记录拼进那条永远解析不了的行里。
func (s *Store) openActive() error {
	if n := len(s.segs); n > 0 {
		last := s.segs[n-1]
		ok, err := endsWithNewline(last.path, last.size)
		if err != nil {
			s.log.Warn("audit_segment_probe_failed", "path", last.path, "error", err.Error())
		}
		if !ok {
			s.log.Warn("audit_segment_torn_tail", "path", last.path)
		} else if last.size < s.segmentBytes*3/4 {
			f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				last.f = f
				last.w = bufio.NewWriterSize(f, 32<<10)
				s.active = last
				return nil
			}
			s.log.Warn("audit_segment_open_failed", "path", last.path, "error", err.Error())
		}
	}
	return s.createSegmentLocked()
}

func (s *Store) createSegmentLocked() error {
	now := s.now().UTC()
	for i := 0; i < 100; i++ {
		path := filepath.Join(s.dir, fmt.Sprintf("audit-%020d.ndjson", now.UnixNano()+int64(i)))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("audit: create segment: %w", err)
		}
		// 0600：审计里有操作人标识与配置片段，不该让同机其他用户读。
		seg := &segment{path: path, f: f, w: bufio.NewWriterSize(f, 32<<10), start: now}
		s.segs = append(s.segs, seg)
		s.active = seg
		return nil
	}
	return errors.New("audit: cannot allocate a unique segment name")
}

// Append 追加一条记录并返回补齐了 Seq/Prev/Hash 的副本。
//
// 写入后立刻 Flush：审计记录必须在上层返回响应之前到达内核，否则「客户端
// 收到 200，审计里没有」这类缺口会让事后复盘直接失真。
func (s *Store) Append(r Record) (Record, error) {
	if r.Result == "" {
		r.Result = ResultOK
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return r, ErrClosed
	}
	if r.Time.IsZero() {
		r.Time = s.now()
	}
	r.Time = r.Time.UTC()
	r.Seq = s.seq + 1
	r.Prev = s.lastHash
	r.Hash = chainHash(r)

	line, err := encodeRecord(r)
	if err != nil {
		s.writeErrors++
		return r, err
	}
	if s.active == nil {
		if err := s.createSegmentLocked(); err != nil {
			s.writeErrors++
			return r, err
		}
	}
	if s.active.size > 0 && s.active.size+int64(len(line)) > s.segmentBytes {
		if err := s.rotateLocked(); err != nil {
			s.writeErrors++
			return r, err
		}
	}
	n, err := s.active.w.Write(line)
	if err != nil {
		s.writeErrors++
		return r, err
	}
	if err := s.active.w.Flush(); err != nil {
		s.writeErrors++
		return r, err
	}
	s.active.size += int64(n)
	s.active.records++
	s.bytes += int64(n)
	s.seq = r.Seq
	s.lastHash = r.Hash
	s.count++
	s.appended++
	s.enforceLocked()
	return r, nil
}

func (s *Store) rotateLocked() error {
	if s.active != nil {
		if err := s.active.close(); err != nil {
			s.log.Warn("audit_segment_close_failed", "path", s.active.path, "error", err.Error())
		}
		s.active = nil
	}
	return s.createSegmentLocked()
}

// enforceLocked 执行保留策略：先按年龄删整段，再按总字节数删最旧的段。
// 永远保留至少一段，否则「刚写的记录立刻被自己删掉」。
func (s *Store) enforceLocked() {
	if s.maxAge > 0 {
		cut := s.now().UTC().Add(-s.maxAge)
		for len(s.segs) > 1 {
			seg := s.segs[0]
			if seg == s.active || !seg.start.Before(cut) {
				break
			}
			s.dropLocked(seg)
		}
	}
	for len(s.segs) > 1 && s.bytes > s.maxBytes {
		seg := s.segs[0]
		if seg == s.active {
			break
		}
		s.dropLocked(seg)
	}
}

func (s *Store) dropLocked(seg *segment) {
	if err := os.Remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("audit_segment_remove_failed", "path", seg.path, "error", err.Error())
		return
	}
	if seg.size > 0 {
		s.bytes -= seg.size
		if s.bytes < 0 {
			s.bytes = 0
		}
	}
	// Records 必须跟着整段淘汰一起减少，否则它只是「本进程写过多少条」，
	// 在长期运行的网关上会一路涨到与磁盘内容无关的数字。
	if seg.records >= s.count {
		s.count = 0
	} else {
		s.count -= seg.records
	}
	for i, x := range s.segs {
		if x == seg {
			s.segs = append(s.segs[:i], s.segs[i+1:]...)
			break
		}
	}
	s.evicted++
}

// Query 执行一次查询。Items 按时间倒序返回，游标是不透明的偏移量。
func (s *Store) Query(f Filter) (Result, error) {
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.To.Before(f.From) {
		return Result{}, fmt.Errorf("%w: to is before from", ErrInvalidQuery)
	}
	offset, err := decodeCursor(f.Cursor)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	segs := append([]*segment(nil), s.segs...)
	s.mu.Unlock()

	// 多取一条用来判断「还有没有下一页」，否则最后一页会凭空多出一个游标。
	need := offset + f.Limit + 1
	matches := make([]Record, 0, 128)
	scanned := 0
scan:
	for i := len(segs) - 1; i >= 0 && len(matches) < need; i-- {
		seg := segs[i]
		recs, err := readSegment(seg.path, seg.size)
		if err != nil {
			return Result{}, fmt.Errorf("audit: read segment %s: %w", seg.path, err)
		}
		// 段内按时间升序，倒着扫能让最新的记录最先命中，命中够了就停——
		// 查询「最近 100 条」不必读完 90 天的日志。
		for j := len(recs) - 1; j >= 0; j-- {
			scanned++
			if scanned > maxScanRecords {
				break scan
			}
			if !match(recs[j], f) {
				continue
			}
			matches = append(matches, recs[j])
			if len(matches) >= need {
				break
			}
		}
	}

	// matches 已经是「全局倒序」：段从新到旧、段内从后到前。
	// 多取的那一条只用于判断还有没有下一页，不返回给调用方。
	total := len(matches)
	if total > offset+f.Limit {
		total = offset + f.Limit
	}
	res := Result{Total: total, Scanned: scanned}
	if offset >= len(matches) {
		return res, nil
	}
	end := offset + f.Limit
	if end > len(matches) {
		end = len(matches)
	}
	res.Items = matches[offset:end]
	if len(matches) > end {
		res.NextCursor = encodeCursor(end)
	}
	return res, nil
}

func match(r Record, f Filter) bool {
	if !f.From.IsZero() && r.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && r.Time.After(f.To) {
		return false
	}
	if f.User != "" && !strings.EqualFold(r.User, f.User) {
		return false
	}
	if f.Role != "" && !strings.EqualFold(r.Role, f.Role) {
		return false
	}
	if f.Action != "" && !strings.EqualFold(r.Action, f.Action) {
		return false
	}
	if f.Result != "" && !strings.EqualFold(r.Result, f.Result) {
		return false
	}
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		// Code 也必须可搜：运维排查时手里往往只有一个错误码
		// （auth.forbidden、point.not_writable），而不是用户名。
		hit := strings.Contains(strings.ToLower(r.User), q) ||
			strings.Contains(strings.ToLower(r.Action), q) ||
			strings.Contains(strings.ToLower(r.Target), q) ||
			strings.Contains(strings.ToLower(r.Message), q) ||
			strings.Contains(strings.ToLower(r.Code), q)
		if !hit {
			return false
		}
	}
	return true
}

// Verify 逐条重算哈希链。它读全部段，因此不建议放进高频调用路径
// （由 /api/v1/audit/verify 手动触发，或诊断包里跑一次）。
func (s *Store) Verify() (VerifyResult, error) {
	s.mu.Lock()
	segs := append([]*segment(nil), s.segs...)
	s.mu.Unlock()

	res := VerifyResult{OK: true}
	var prevHash string
	var prevSeq uint64
	first := true
	for _, seg := range segs {
		recs, err := readSegment(seg.path, seg.size)
		if err != nil {
			return res, fmt.Errorf("audit: read segment %s: %w", seg.path, err)
		}
		for _, rec := range recs {
			res.Checked++
			// 链的起点：seq 必须为 1，或（被淘汰过时）Prev 为空但 seq>1。
			if first {
				first = false
			} else {
				if rec.Seq != prevSeq+1 {
					res.OK = false
					res.BrokenAt = rec.Seq
					res.Reason = fmt.Sprintf("seq gap: want %d, got %d", prevSeq+1, rec.Seq)
					return res, nil
				}
				if rec.Prev != prevHash {
					res.OK = false
					res.BrokenAt = rec.Seq
					res.Reason = "prev hash mismatch"
					return res, nil
				}
			}
			if got := chainHash(rec); got != rec.Hash {
				res.OK = false
				res.BrokenAt = rec.Seq
				res.Reason = "record hash mismatch"
				return res, nil
			}
			prevSeq, prevHash = rec.Seq, rec.Hash
		}
	}
	return res, nil
}

// Stats 返回水位。
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Records: s.count, Bytes: s.bytes, MaxBytes: s.maxBytes,
		Segments: len(s.segs), Appended: s.appended, Evicted: s.evicted,
		WriteErrors: s.writeErrors, LastSeq: s.seq,
	}
	if len(s.segs) > 0 {
		st.Oldest = s.segs[0].start
		if rec, ok := lastRecord(s.segs[len(s.segs)-1].path, s.segs[len(s.segs)-1].size); ok {
			st.Newest = rec.Time
		}
	}
	return st
}

// Dir 返回存储目录。runtime 用它判断「配置是否把审计指到了别处」。
func (s *Store) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// Flush 把缓冲刷到内核。Append 已经逐条 Flush，这里是给 Close 前的显式调用
// 与运维命令用的。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.w == nil {
		return nil
	}
	return s.active.w.Flush()
}

// Close 关闭并刷盘。之后 Append 返回 ErrClosed，Query 仍可读。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var first error
	for _, seg := range s.segs {
		if err := seg.close(); err != nil && first == nil {
			first = err
		}
	}
	s.active = nil
	return first
}

// ---------------------------------------------------------------------------
// 编码与哈希链
// ---------------------------------------------------------------------------

func encodeRecord(r Record) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("audit: encode record: %w", err)
	}
	return append(b, '\n'), nil
}

func decodeRecord(line []byte) (Record, error) {
	var r Record
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return r, errors.New("audit: empty line")
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	return r, nil
}

// appendField 用「长度:内容」编码每个字段。用 \x1f 之类的分隔符会让
// 「值里含分隔符」变成一次可构造的哈希碰撞——审计的规范化编码不能留
// 这种口子。
func appendField(b []byte, s string) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}

// chainHash 计算一条记录的哈希：prev + 全部字段的规范化编码。
func chainHash(r Record) string {
	b := make([]byte, 0, 256)
	b = appendField(b, r.Prev)
	b = strconv.AppendUint(b, r.Seq, 10)
	b = append(b, '|')
	b = appendField(b, r.Time.UTC().Format(time.RFC3339Nano))
	b = appendField(b, r.User)
	b = appendField(b, r.Role)
	b = appendField(b, r.Action)
	b = appendField(b, r.Target)
	b = appendField(b, r.Result)
	b = appendField(b, r.Code)
	b = appendField(b, r.Message)
	b = appendField(b, r.Remote)
	b = appendField(b, r.RequestID)
	keys := make([]string, 0, len(r.Detail))
	for k := range r.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b = appendField(b, k)
		b = appendField(b, r.Detail[k])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readSegment 读取整段。审计量小，读全文件比维护偏移索引简单得多；
// 半行（掉电）直接跳过，不让一条垃圾毁掉整个查询。
func readSegment(path string, size int64) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	out := make([]Record, 0, 256)
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if rec, derr := decodeRecord(line); derr == nil {
				out = append(out, rec)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, err
		}
	}
	return out, nil
}

func encodeCursor(off int) string {
	if off <= 0 {
		return ""
	}
	return "o" + strconv.Itoa(off)
}

func decodeCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	if !strings.HasPrefix(s, "o") {
		return 0, fmt.Errorf("%w: bad cursor", ErrInvalidQuery)
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: bad cursor", ErrInvalidQuery)
	}
	return n, nil
}
