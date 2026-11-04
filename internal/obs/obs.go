// Package obs 提供三类可观测性基础设施：Prometheus 文本格式指标、结构化
// 日志、以及供 /api/v1/logs 使用的内存日志环形缓冲。
//
// 该包不依赖任何第三方库，也不依赖其他内部包，因此可以被任何模块引用而
// 不会引入依赖环。
package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 指标
// ---------------------------------------------------------------------------

// Labels 是指标标签集合。高基数字段（点位 ID 等）禁止作为标签，只能进日志。
type Labels map[string]string

// DefaultBuckets 是直方图的默认分桶，覆盖毫秒到十秒。
var DefaultBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type series struct {
	labels  Labels
	key     string
	value   float64
	sum     float64
	count   uint64
	buckets []uint64
}

type family struct {
	name    string
	help    string
	typ     string // counter | gauge | histogram
	buckets []float64
	series  map[string]*series
}

// Metrics 是一个并发安全的指标注册表。未知指标在首次写入时自动注册，
// 类型冲突时以先注册者为准（避免运行期 panic）。
type Metrics struct {
	mu       sync.RWMutex
	families map[string]*family
}

// New 创建空指标表。指标名建议遵循 uic_<subsystem>_<name>。
func New() *Metrics {
	return &Metrics{families: map[string]*family{}}
}

// Declare 预先声明指标类型与帮助文本，便于在没有任何样本时也能输出元信息。
func (m *Metrics) Declare(name, typ, help string, buckets []float64) {
	if typ == "" {
		typ = "counter"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.families[name]
	if !ok {
		if len(buckets) == 0 {
			buckets = DefaultBuckets
		}
		f = &family{name: name, help: help, typ: typ, buckets: buckets, series: map[string]*series{}}
		m.families[name] = f
		return
	}
	if f.help == "" {
		f.help = help
	}
}

func (m *Metrics) get(name, typ, help string) *family {
	if f, ok := m.families[name]; ok {
		return f
	}
	buckets := DefaultBuckets
	f := &family{name: name, help: help, typ: typ, buckets: buckets, series: map[string]*series{}}
	m.families[name] = f
	return f
}

func (m *Metrics) seriesFor(name, typ, help string, l Labels) *series {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.get(name, typ, help)
	key := labelKey(l)
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: copyLabels(l), key: key}
		if f.typ == "histogram" {
			s.buckets = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Inc 计数器加一。
func (m *Metrics) Inc(name string, l Labels) { m.Add(name, 1, l) }

// Add 计数器加 delta。
func (m *Metrics) Add(name string, delta float64, l Labels) {
	s := m.seriesFor(name, "counter", "", l)
	m.mu.Lock()
	s.value += delta
	m.mu.Unlock()
}

// Set 设置 gauge 值。
func (m *Metrics) Set(name string, v float64, l Labels) {
	s := m.seriesFor(name, "gauge", "", l)
	m.mu.Lock()
	s.value = v
	m.mu.Unlock()
}

// Observe 记录一次直方图观测。
func (m *Metrics) Observe(name string, v float64, l Labels) {
	s := m.seriesFor(name, "histogram", "", l)
	m.mu.Lock()
	defer m.mu.Unlock()
	s.sum += v
	s.count++
	f := m.families[name]
	for i, ub := range f.buckets {
		if v <= ub {
			s.buckets[i]++
		}
	}
}

// Value 读取指定序列的当前值（counter/gauge），用于断言与健康检查。
func (m *Metrics) Value(name string, l Labels) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.families[name]
	if !ok {
		return 0
	}
	s, ok := f.series[labelKey(l)]
	if !ok {
		return 0
	}
	return s.value
}

// source 用于避免在 Observe 内重复加锁（下面的实现会用到）。

// WriteText 输出 Prometheus 文本格式（exposition format 0.0.4）。
func (m *Metrics) WriteText(w io.Writer) error {
	m.mu.RLock()
	type snapshot struct {
		name, help, typ string
		buckets         []float64
		series          []series
	}
	all := make([]snapshot, 0, len(m.families))
	for _, f := range m.families {
		s := snapshot{name: f.name, help: f.help, typ: f.typ, buckets: f.buckets}
		for _, x := range f.series {
			cp := series{labels: copyLabels(x.labels), key: x.key, value: x.value, sum: x.sum, count: x.count}
			if x.buckets != nil {
				cp.buckets = append([]uint64(nil), x.buckets...)
			}
			s.series = append(s.series, cp)
		}
		all = append(all, s)
	}
	m.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	var b strings.Builder
	for _, f := range all {
		sort.Slice(f.series, func(i, j int) bool { return f.series[i].key < f.series[j].key })
		if f.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", f.name, escapeHelp(f.help))
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.name, f.typ)
		switch f.typ {
		case "histogram":
			for _, s := range f.series {
				for i, ub := range f.buckets {
					b.WriteString(f.name + "_bucket" + labelString(withLabel(s.labels, "le", formatFloat(ub))) + " " + strconv.FormatUint(s.buckets[i], 10) + "\n")
				}
				b.WriteString(f.name + "_bucket" + labelString(withLabel(s.labels, "le", "+Inf")) + " " + strconv.FormatUint(s.count, 10) + "\n")
				b.WriteString(f.name + "_sum" + labelString(s.labels) + " " + formatFloat(s.sum) + "\n")
				b.WriteString(f.name + "_count" + labelString(s.labels) + " " + strconv.FormatUint(s.count, 10) + "\n")
			}
		default:
			for _, s := range f.series {
				b.WriteString(f.name + labelString(s.labels) + " " + formatFloat(s.value) + "\n")
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func withLabel(l Labels, k, v string) Labels {
	out := copyLabels(l)
	if out == nil {
		out = make(Labels, 1)
	}
	out[k] = v
	return out
}

func copyLabels(l Labels) Labels {
	if len(l) == 0 {
		return nil
	}
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func labelKey(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
	}
	return b.String()
}

func labelString(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"=\""+escapeLabel(l[k])+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func escapeHelp(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Reset 清空全部样本，保留已声明的族。仅用于测试。
func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.families {
		f.series = map[string]*series{}
	}
}

// DriverAdapter 把 *Metrics 适配为 driver.Metrics 接口（避免 obs 依赖 driver）。
type DriverAdapter struct{ M *Metrics }

func (a DriverAdapter) Counter(name string, labels map[string]string) func(delta float64) {
	l := Labels(labels)
	f := a.M.seriesFor(name, "counter", "", l)
	return func(delta float64) {
		a.M.mu.Lock()
		f.value += delta
		a.M.mu.Unlock()
	}
}

func (a DriverAdapter) Observe(name string, v float64, labels map[string]string) {
	a.M.Observe(name, v, Labels(labels))
}

// ---------------------------------------------------------------------------
// 结构化日志
// ---------------------------------------------------------------------------

// Ring 是一个固定容量的日志环形缓冲，实现 slog.Handler。
// 它服务于「现场没有 SSH 时远程拉日志」这一需求。
//
// 注意：底层缓冲通过指针共享，因此 logger.With(...) 派生出的 handler 写入
// 的记录仍然会出现在同一个环形缓冲里。
type Ring struct {
	buf    *ringBuf
	attrs  []slog.Attr
	groups []string
}

type ringBuf struct {
	mu      sync.RWMutex
	max     int
	entries []Entry
}

// Entry 是一条被缓冲的日志。
type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// NewRing 创建容量为 max 的环形缓冲（默认 2000）。
func NewRing(max int) *Ring {
	if max <= 0 {
		max = 2000
	}
	return &Ring{buf: &ringBuf{max: max}}
}

func (r *Ring) Enabled(context.Context, slog.Level) bool { return true }

func (r *Ring) Handle(_ context.Context, rec slog.Record) error {
	e := Entry{
		Time:    rec.Time,
		Level:   levelName(rec.Level),
		Message: rec.Message,
		Attrs:   map[string]any{},
	}
	for _, a := range r.attrs {
		addAttr(e.Attrs, prefixGroups(r.groups, a))
	}
	rec.Attrs(func(a slog.Attr) bool {
		addAttr(e.Attrs, prefixGroups(r.groups, a))
		return true
	})
	if len(e.Attrs) == 0 {
		e.Attrs = nil
	}
	b := r.buf
	b.mu.Lock()
	if len(b.entries) == b.max {
		copy(b.entries, b.entries[1:])
		b.entries[len(b.entries)-1] = e
	} else {
		b.entries = append(b.entries, e)
	}
	b.mu.Unlock()
	return nil
}

func (r *Ring) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Ring{
		buf:    r.buf,
		attrs:  append(append([]slog.Attr(nil), r.attrs...), attrs...),
		groups: r.groups,
	}
}

func (r *Ring) WithGroup(name string) slog.Handler {
	return &Ring{
		buf:    r.buf,
		attrs:  r.attrs,
		groups: append(append([]string(nil), r.groups...), name),
	}
}

// Query 按设备与级别过滤，返回最近 limit 条（时间正序）。
func (r *Ring) Query(device, level string, limit int) []Entry {
	b := r.buf
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit <= 0 || limit > len(b.entries) {
		limit = len(b.entries)
	}
	want := 0
	if level != "" {
		for i, n := range []string{"debug", "info", "warn", "error"} {
			if n == strings.ToLower(level) {
				want = i
			}
		}
	}
	out := make([]Entry, 0, limit)
	for i := len(b.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := b.entries[i]
		if device != "" {
			if v, ok := e.Attrs["device"]; !ok || fmt.Sprint(v) != device {
				continue
			}
		}
		if level != "" && levelRank(e.Level) < want {
			continue
		}
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Len 返回当前缓冲条数。
func (r *Ring) Len() int {
	b := r.buf
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}

func levelRank(l string) int {
	switch l {
	case "error":
		return 3
	case "warn":
		return 2
	case "info":
		return 1
	}
	return 0
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

func prefixGroups(groups []string, a slog.Attr) slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		a = slog.Group(groups[i], a)
	}
	return a
}

func addAttr(dst map[string]any, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		for _, sub := range a.Value.Group() {
			addAttr(dst, sub)
		}
	default:
		dst[a.Key] = a.Value.Any()
	}
}

// MultiHandler 把日志同时写往多个 handler（文件 + 环形缓冲）。
type MultiHandler []slog.Handler

func (m MultiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(MultiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m MultiHandler) WithGroup(name string) slog.Handler {
	out := make(MultiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

// LevelVar 允许运行期调整日志级别（远程命令 log.level）。
type LevelVar struct{ v atomic.Int32 }

// NewLevelVar 解析级别字符串（debug/info/warn/error）。
func NewLevelVar(level string) *LevelVar {
	lv := &LevelVar{}
	lv.Set(level)
	return lv
}

func (l *LevelVar) Set(level string) {
	switch strings.ToLower(level) {
	case "debug":
		l.v.Store(int32(slog.LevelDebug))
	case "warn", "warning":
		l.v.Store(int32(slog.LevelWarn))
	case "error":
		l.v.Store(int32(slog.LevelError))
	default:
		l.v.Store(int32(slog.LevelInfo))
	}
}

func (l *LevelVar) Level() slog.Level { return slog.Level(l.v.Load()) }

// NewLogger 构造结构化日志器：始终输出到 w，同时写入 ring（ring 可为 nil）。
func NewLogger(w io.Writer, level *LevelVar, ring *Ring) *slog.Logger {
	if level == nil {
		level = NewLevelVar("info")
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level.Level()})
	// JSONHandler 的 Level 在构造时固定，这里用一个可变 level 包装。
	vh := &levelHandler{inner: h, lv: level}
	if ring == nil {
		return slog.New(vh)
	}
	return slog.New(MultiHandler{vh, ring})
}

type levelHandler struct {
	inner slog.Handler
	lv    *LevelVar
}

func (h *levelHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.lv.Level() }

func (h *levelHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}

func (h *levelHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &levelHandler{inner: h.inner.WithAttrs(a), lv: h.lv}
}

func (h *levelHandler) WithGroup(n string) slog.Handler {
	return &levelHandler{inner: h.inner.WithGroup(n), lv: h.lv}
}
