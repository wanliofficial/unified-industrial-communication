// Package alarm 实现报警引擎：阈值与滞回、延时确认、确认（ack）与清除
// （clear），并把每一次状态迁移发布给 SSE 与 MQTT。
//
// 三条不变量，每一条都来自现场会真实发生的失败，而不是设计偏好：
//
//  1. **输入不可信时绝不改变报警状态。** 坏质量既不能当「正常」（会掩盖真实
//     故障），也不能当「报警」（数据一断就是满屏假报警）。因此评估结果有三态：
//     满足 / 不满足 / 未知；未知只更新可观测字段，不动状态机。
//  2. **延时是「连续满足」，不是「累计满足」。** 断续抖动不应该攒够时间就
//     报警；`onDelay` 期间只要有一拍不满足就重新计时。
//  3. **确认不是清除，清除也不是确认。** 报警有五个状态：normal / pending /
//     active / acked / cleared。`active`（未确认）与 `cleared`（已恢复但没
//     人看过）都需要人处理；把它们合并成一个布尔，就会丢掉「谁确认了什么」。
//
// 报警状态刻意只放内存：它描述的是「当前事实」。进程重启后，仍然成立的条件
// 会重新报警（经过 onDelay），这比恢复一份可能已经过期的状态更安全。
package alarm

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"uic/internal/calc"
	"uic/internal/model"
)

// Severity 是报警级别。三档足够：再细的分级在现场最后都会退化成「都要人
// 处理」，而分级的价值在于排序与路由，不在于命名。
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Valid 报告级别是否合法。
func (s Severity) Valid() bool {
	switch s {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return true
	}
	return false
}

// rank 用于排序：越严重越靠前。
func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// State 是报警状态机的五个状态。
type State string

const (
	// StateNormal：条件不成立，无需处理。
	StateNormal State = "normal"
	// StatePending：条件成立但还没满足 onDelay，尚未成为报警。可见但不可确认。
	StatePending State = "pending"
	// StateActive：已报警，尚未确认。
	StateActive State = "active"
	// StateAcked：已报警且已确认（条件可能仍然成立）。
	StateAcked State = "acked"
	// StateCleared：条件已恢复正常，但尚未确认（运维还没看到这次恢复）。
	StateCleared State = "cleared"
)

// Valid 报告状态是否合法（用于查询过滤）。
func (s State) Valid() bool {
	switch s {
	case StateNormal, StatePending, StateActive, StateAcked, StateCleared:
		return true
	}
	return false
}

// 上限。不是省内存，而是让「配置写错」在加载期就暴露：
// 一个 48 小时的延时几乎一定是把秒写成了小时。
const (
	MaxExprBytes    = 1024
	MaxMessageBytes = 512
	MaxNameBytes    = 128
	MaxDelay        = 24 * time.Hour
)

var (
	idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// ctrlRe 匹配控制字符。消息会进 JSON、SSE、MQTT 与日志，允许换行会让
	// 一行日志变成多行，运维按行过滤时看到的是半截内容。
	ctrlRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// Rule 是一条报警规则。
//
// 判定条件二选一（同时配置会被拒绝，避免「到底按哪个判」的歧义）：
//
//	阈值：Point + High/Low + Hysteresis
//	表达式：Expr（复用 calc 引擎，必须是布尔表达式）
type Rule struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Severity Severity `json:"severity,omitempty"`
	Message  string   `json:"message,omitempty"`
	// Disabled 显式停用。默认启用——与设备相反，理由很实际：漏写
	// `enabled: true` 会让一条已经写好的报警永远不响，而这类错误没有任何
	// 外部表现；漏写设备启用则会立刻表现为「没有数据」。
	Disabled bool `json:"disabled,omitempty"`

	// 阈值条件。Point 是 `device:point`。
	Point      string   `json:"point,omitempty"`
	High       *float64 `json:"high,omitempty"`
	Low        *float64 `json:"low,omitempty"`
	Hysteresis float64  `json:"hysteresis,omitempty"`

	// 表达式条件。必须是布尔表达式（如 `line-a:power > line-a:rate * 2`）。
	Expr string `json:"expr,omitempty"`

	// OnDelay 是「持续满足多久才报警」，OffDelay 是「持续不满足多久才清除」。
	OnDelay  model.Duration `json:"onDelay,omitempty"`
	OffDelay model.Duration `json:"offDelay,omitempty"`

	// AutoAck 让恢复即归位，不需要人确认。只适用于纯提示类报警；
	// 默认 false（恢复后进入 cleared，等人确认）。
	AutoAck bool `json:"autoAck,omitempty"`
}

// Normalize 填充默认值。必须幂等。
func (r *Rule) Normalize() {
	if r.Severity == "" {
		r.Severity = SeverityWarning
	}
}

// Validate 做与引擎语义相关的静态校验。
//
// 引用是否存在、点位类型是否可比较属于配置层的事（需要全局点位索引），
// 这里只校验「单条规则自身是否自洽」。
func (r Rule) Validate() error {
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("id %q: must match %s", r.ID, idRe.String())
	}
	if !r.Severity.Valid() {
		return fmt.Errorf("severity %q: must be info|warning|critical", r.Severity)
	}
	if len(r.Name) > MaxNameBytes {
		return fmt.Errorf("name: %d bytes exceeds the %d-byte limit", len(r.Name), MaxNameBytes)
	}
	if len(r.Message) > MaxMessageBytes {
		return fmt.Errorf("message: %d bytes exceeds the %d-byte limit", len(r.Message), MaxMessageBytes)
	}
	if ctrlRe.MatchString(r.Name) || ctrlRe.MatchString(r.Message) {
		return fmt.Errorf("name/message: control characters are not allowed")
	}
	if r.OnDelay.Duration() < 0 || r.OffDelay.Duration() < 0 {
		return fmt.Errorf("onDelay/offDelay: must be >= 0")
	}
	if r.OnDelay.Duration() > MaxDelay || r.OffDelay.Duration() > MaxDelay {
		return fmt.Errorf("onDelay/offDelay: must be <= %s", MaxDelay)
	}

	hasThreshold := r.Point != "" || r.High != nil || r.Low != nil || r.Hysteresis != 0
	hasExpr := r.Expr != ""
	switch {
	case hasThreshold && hasExpr:
		return fmt.Errorf("point/high/low/hysteresis and expr are mutually exclusive")
	case !hasThreshold && !hasExpr:
		return fmt.Errorf("one of (point with high/low) or expr is required")
	}

	if hasExpr {
		if len(r.Expr) > MaxExprBytes {
			return fmt.Errorf("expr: %d bytes exceeds the %d-byte limit", len(r.Expr), MaxExprBytes)
		}
		prog, err := calc.Compile(r.Expr)
		if err != nil {
			return fmt.Errorf("expr: %w", err)
		}
		// 报警条件必须是布尔。允许数值隐式转布尔等于允许
		// `expr: "line-a:power"`——它永远为真（非零即真），
		// 一个不会报错但也不会按预期工作的报警。
		if prog.Kind() != calc.KindBool {
			return fmt.Errorf("expr: must be a boolean expression, got %s", prog.Kind())
		}
		return nil
	}

	if _, _, ok := SplitRef(r.Point); !ok {
		return fmt.Errorf("point %q: must be device:point", r.Point)
	}
	if r.High == nil && r.Low == nil {
		return fmt.Errorf("high or low: at least one threshold is required")
	}
	for name, v := range map[string]*float64{"high": r.High, "low": r.Low} {
		if v == nil {
			continue
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return fmt.Errorf("%s: must be a finite number", name)
		}
	}
	if math.IsNaN(r.Hysteresis) || math.IsInf(r.Hysteresis, 0) || r.Hysteresis < 0 {
		return fmt.Errorf("hysteresis: must be a finite number >= 0")
	}
	if r.High != nil && r.Low != nil {
		hi, lo := *r.High, *r.Low
		if hi <= lo {
			return fmt.Errorf("high (%v) must be greater than low (%v)", hi, lo)
		}
		// 两侧越限时的恢复带：必须非空，否则报警永远无法清除（或一清除
		// 就立刻再次触发，表现为高频抖动）。
		if hi-lo <= 2*r.Hysteresis {
			return fmt.Errorf("hysteresis (%v) is too large: high-low (%v) must exceed 2*hysteresis",
				r.Hysteresis, hi-lo)
		}
	}
	return nil
}

// References 返回规则依赖的点位引用（`device:point`）。
// 阈值规则只有一条；表达式规则来自编译后的引用表。
func (r Rule) References() ([]string, error) {
	if r.Expr == "" {
		if r.Point == "" {
			return nil, nil
		}
		return []string{r.Point}, nil
	}
	prog, err := calc.Compile(r.Expr)
	if err != nil {
		return nil, err
	}
	return prog.Refs(), nil
}

// SplitRef 拆分 `device:point`。
func SplitRef(ref string) (device, point string, ok bool) {
	i := strings.IndexByte(ref, ':')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// ---------------------------------------------------------------------------
// 编译后的规则
// ---------------------------------------------------------------------------

type compiled struct {
	rule Rule
	prog *calc.Program
	dev  string
	pt   string
	// hash 覆盖「影响判定结果」的字段。只有它变化才重置状态：改一个
	// Message 不该把一条正在报警的记录打成 normal。
	hash string
}

func compile(rule Rule) (*compiled, error) {
	if err := rule.Validate(); err != nil {
		return nil, err
	}
	c := &compiled{rule: rule, hash: conditionHash(rule)}
	if rule.Expr != "" {
		prog, err := calc.Compile(rule.Expr)
		if err != nil {
			return nil, err
		}
		c.prog = prog
		return c, nil
	}
	dev, pt, _ := SplitRef(rule.Point)
	c.dev, c.pt = dev, pt
	return c, nil
}

// conditionHash 用 FNV-1a 而不是 SHA-256：这里只用于「同一进程内判断规则
// 定义是否变化」，不是安全边界，也不进任何对外契约。
func conditionHash(r Rule) string {
	h := fnv.New64a()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(string(r.Severity))
	write(r.Point)
	if r.High != nil {
		write(fmt.Sprintf("high=%v", *r.High))
	}
	if r.Low != nil {
		write(fmt.Sprintf("low=%v", *r.Low))
	}
	write(fmt.Sprintf("hyst=%v", r.Hysteresis))
	write(r.Expr)
	write(r.OnDelay.Duration().String())
	write(r.OffDelay.Duration().String())
	write(fmt.Sprintf("autoAck=%t", r.AutoAck))
	// Disabled 决定「这条规则是否参与判定」，因此属于判定条件：把一条正在
	// 报警的规则停用，必须让状态归位并清掉 retained，而不是把它挂在 active。
	write(fmt.Sprintf("disabled=%t", r.Disabled))
	return fmt.Sprintf("%x", h.Sum64())
}

// trigger 是「是否应当开始报警」的判定。
func (c *compiled) trigger(v float64) bool {
	switch {
	case c.rule.High != nil && c.rule.Low != nil:
		return v > *c.rule.High || v < *c.rule.Low
	case c.rule.High != nil:
		return v > *c.rule.High
	default:
		return v < *c.rule.Low
	}
}

// holds 是「报警是否可以维持」的判定：恢复阈值与触发阈值之间留出滞回带。
//
// 为什么必须有这一步：`power > 80` 报警，值在 80.0 附近抖动时，如果清除也
// 用 80，就会产生一串 raised/cleared 事件。滞回把「触发」与「恢复」拆成两条
// 阈值，抖动被夹在带内，状态保持不变。
func (c *compiled) holds(v float64) bool {
	h := c.rule.Hysteresis
	switch {
	case c.rule.High != nil && c.rule.Low != nil:
		// 两侧都可能报警：只要还在任意一侧的滞回带内就维持。
		return v > *c.rule.High-h || v < *c.rule.Low+h
	case c.rule.High != nil:
		return v > *c.rule.High-h
	default:
		return v < *c.rule.Low+h
	}
}

// eval 返回一次评估的结果。holding 为 true 时按恢复阈值判定（只影响阈值规则）。
func (c *compiled) eval(lookup Lookup, holding bool) (cond bool, value float64, quality model.Quality, known bool, reason string) {
	if lookup == nil {
		return false, 0, model.QualityNotConnected, false, "no value source"
	}
	if c.prog != nil {
		return c.evalExpr(lookup)
	}
	s, ok := lookup(c.dev, c.pt)
	if !ok {
		return false, 0, model.QualityNotConnected, false, "no value yet"
	}
	if !s.Quality.IsUsable() {
		return false, 0, s.Quality, false, "input quality is " + string(s.Quality)
	}
	v, ok := s.Value.AsFloat()
	if !ok {
		return false, 0, model.QualityBad, false, "value is not numeric"
	}
	if holding {
		return c.holds(v), v, s.Quality, true, ""
	}
	return c.trigger(v), v, s.Quality, true, ""
}

// evalExpr 求值布尔表达式，并对输入做质量门控。
//
// 短路求值意味着「没有被求值的引用」不会影响结果，因此也不参与质量判定：
// `false && dead:x > 0` 的结果确实是 false，不属于「未知」。
func (c *compiled) evalExpr(lookup Lookup) (bool, float64, model.Quality, bool, string) {
	worst := model.QualityGood
	badRef, badWhy := "", ""
	get := func(ref string) (float64, bool) {
		dev, pt, ok := SplitRef(ref)
		if !ok {
			if badRef == "" {
				badRef, badWhy = ref, "malformed reference"
			}
			return 0, false
		}
		s, found := lookup(dev, pt)
		if !found {
			if badRef == "" {
				badRef, badWhy = ref, "no value yet"
			}
			return 0, false
		}
		if !s.Quality.IsUsable() {
			worst = worse(worst, s.Quality)
			if badRef == "" {
				badRef, badWhy = ref, "input quality is "+string(s.Quality)
			}
			return 0, false
		}
		v, ok := s.Value.AsFloat()
		if !ok {
			if badRef == "" {
				badRef, badWhy = ref, "value is not numeric"
			}
			return 0, false
		}
		worst = worse(worst, s.Quality)
		return v, true
	}
	b, err := c.prog.EvalBool(get)
	if badRef != "" {
		return false, 0, worst, false, "input " + badRef + ": " + badWhy
	}
	if err != nil {
		return false, 0, model.QualityBad, false, "expression: " + err.Error()
	}
	v := 0.0
	if b {
		v = 1
	}
	return b, v, worst, true, ""
}

// worse 返回两个质量码里更差的那个（与采集层一致的排序）。
func worse(a, b model.Quality) model.Quality {
	if qualityRank(a) >= qualityRank(b) {
		return a
	}
	return b
}

func qualityRank(q model.Quality) int {
	switch q {
	case model.QualityGood:
		return 0
	case model.QualityUncertain:
		return 1
	case model.QualityStale:
		return 2
	case model.QualityTimeout:
		return 3
	case model.QualityNotConnected:
		return 4
	case model.QualityCommError:
		return 5
	case model.QualityAddressError:
		return 6
	}
	return 7
}

// ---------------------------------------------------------------------------
// 对外视图
// ---------------------------------------------------------------------------

// Status 是一条报警的当前快照。
type Status struct {
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	Severity     Severity      `json:"severity"`
	Message      string        `json:"message,omitempty"`
	State        State         `json:"state"`
	Active       bool          `json:"active"`
	Pending      bool          `json:"pending"`
	Acknowledged bool          `json:"acknowledged"`
	Disabled     bool          `json:"disabled,omitempty"`
	Count        uint64        `json:"count"`
	Value        *float64      `json:"value,omitempty"`
	ValueKnown   bool          `json:"valueKnown"`
	Quality      model.Quality `json:"quality,omitempty"`
	// Since 是最近一次进入 active 的时刻，不是「进入当前状态」的时刻：
	// 运维关心的是「这个故障从什么时候开始的」。
	Since     *time.Time `json:"since,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
	AckAt     *time.Time `json:"ackAt,omitempty"`
	AckBy     string     `json:"ackBy,omitempty"`
	ClearedAt *time.Time `json:"clearedAt,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// EventType 是报警事件类型。
type EventType string

const (
	EventRaised       EventType = "raised"
	EventAcknowledged EventType = "acknowledged"
	EventCleared      EventType = "cleared"
	// EventReset 表示状态因配置变更被重置——不是条件变化。
	// 消费方（尤其 MQTT 的 retained 订阅者）必须知道这一点，
	// 否则它会把「规则被改掉」误读成「故障恢复」。
	EventReset EventType = "reset"
	// EventRemoved 表示规则已从配置里删除。消费方应删除自己的记录 /
	// retained 主题，而不是把它当成「恢复正常」。
	EventRemoved EventType = "removed"
)

// Event 是一次状态迁移。
type Event struct {
	Type   EventType `json:"type"`
	At     time.Time `json:"at"`
	From   State     `json:"from"`
	Status Status    `json:"status"`
}

// Filter 是列表查询条件。
type Filter struct {
	States     []State
	Severities []Severity
	// Q 匹配 ID / Name / Message 的子串（不区分大小写）。
	Q     string
	Limit int
}

// Counts 是报警状态汇总。
type Counts struct {
	Total   int `json:"total"`
	Normal  int `json:"normal"`
	Pending int `json:"pending"`
	Active  int `json:"active"`
	Acked   int `json:"acked"`
	Cleared int `json:"cleared"`
	// Unknown 统计「最近一次评估时输入不可信」的规则数。它不进 status，
	// 但必须可见：一片 unknown 说明数据链路出了问题，而不是设备都正常。
	Unknown int `json:"unknown"`
}

// Lookup 返回某点位的最新采样（由 runtime 注入 cache.Get）。
//
// 与 driver.OpenConfig.Lookup 一样，这是一个**只读**入口：报警引擎能读
// 当前值，但拿不到写入与订阅能力，因此不可能绕过采集层的治理。
type Lookup func(device, point string) (model.Sample, bool)

// Options 是引擎参数。
type Options struct {
	Rules  []Rule
	Lookup Lookup
	Log    *slog.Logger
	Now    func() time.Time
	// Sweep 是兜底评估周期（默认 200ms）。它负责让「延时到点」在没有新
	// 采样的情况下也能生效。
	Sweep time.Duration
}

// 引擎级错误。
var (
	ErrNotFound  = fmt.Errorf("alarm: not found")
	ErrNotActive = fmt.Errorf("alarm: not active")
)

// ---------------------------------------------------------------------------
// 内部状态
// ---------------------------------------------------------------------------

type status struct {
	rule     Rule
	state    State
	acked    bool
	count    uint64
	value    float64
	hasValue bool
	quality  model.Quality
	known    bool
	reason   string

	since     time.Time // 本次激活时刻
	updatedAt time.Time
	ackAt     time.Time
	ackBy     string
	clearedAt time.Time

	pendingSince time.Time
	clearSince   time.Time
}

func (s *status) snapshot() Status {
	out := Status{
		ID: s.rule.ID, Name: s.rule.Name, Severity: s.rule.Severity,
		Message: s.rule.Message, State: s.state,
		Active:  s.state == StateActive || s.state == StateAcked,
		Pending: s.state == StatePending,
		// 已恢复且已确认的报警不再需要人处理，因此对外的 acknowledged
		// 在 normal 时也应为 true —— 只有「从未确认过」才是 false。
		Acknowledged: s.acked,
		Disabled:     s.rule.Disabled,
		Count:        s.count,
		ValueKnown:   s.known,
		Quality:      s.quality,
		UpdatedAt:    s.updatedAt,
		AckBy:        s.ackBy,
		Reason:       s.reason,
	}
	if s.hasValue {
		v := s.value
		out.Value = &v
	}
	if !s.since.IsZero() {
		t := s.since
		out.Since = &t
	}
	if !s.ackAt.IsZero() {
		t := s.ackAt
		out.AckAt = &t
	}
	if !s.clearedAt.IsZero() {
		t := s.clearedAt
		out.ClearedAt = &t
	}
	return out
}

// ---------------------------------------------------------------------------
// 引擎
// ---------------------------------------------------------------------------

// Engine 是报警引擎。并发安全；评估、确认、查询可以来自不同协程。
type Engine struct {
	mu     sync.Mutex
	rules  []*compiled
	byID   map[string]*compiled
	states map[string]*status
	index  map[string][]*compiled // "device/point" → 依赖它的规则
	lookup Lookup
	log    *slog.Logger
	now    func() time.Time
	sweep  time.Duration

	subs   map[int]*subscriber
	nextID int

	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

type subscriber struct {
	ch     chan Event
	closed bool
}

// New 编译规则并返回引擎。规则非法直接失败：报警配错必须在启动期暴露，
// 而不是等某个条件真的发生时才发现判据写反了。
func New(opt Options) (*Engine, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Sweep <= 0 {
		opt.Sweep = 200 * time.Millisecond
	}
	e := &Engine{
		byID:   map[string]*compiled{},
		states: map[string]*status{},
		index:  map[string][]*compiled{},
		lookup: opt.Lookup,
		log:    opt.Log,
		now:    opt.Now,
		sweep:  opt.Sweep,
		subs:   map[int]*subscriber{},
	}
	e.mu.Lock()
	err := e.reloadLocked(opt.Rules)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Reload 替换规则集。判定条件未变的规则保留当前状态（避免改一句说明就
// 把正在报警的记录打成正常），变化或新增的规则从 normal 开始。
func (e *Engine) Reload(rules []Rule) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reloadLocked(rules)
}

func (e *Engine) reloadLocked(rules []Rule) error {
	compiledRules := make([]*compiled, 0, len(rules))
	byID := make(map[string]*compiled, len(rules))
	for _, r := range rules {
		r.Normalize()
		c, err := compile(r)
		if err != nil {
			return fmt.Errorf("alarm %q: %w", r.ID, err)
		}
		if _, dup := byID[r.ID]; dup {
			return fmt.Errorf("alarm %q: duplicate id", r.ID)
		}
		byID[r.ID] = c
		compiledRules = append(compiledRules, c)
	}
	sort.Slice(compiledRules, func(i, j int) bool { return compiledRules[i].rule.ID < compiledRules[j].rule.ID })

	states := make(map[string]*status, len(compiledRules))
	events := make([]Event, 0)
	now := e.now()
	for _, c := range compiledRules {
		prev, existed := e.states[c.rule.ID]
		if existed && conditionHash(prev.rule) == c.hash {
			prev.rule = c.rule // 刷新 Name/Message/Severity 等元数据
			states[c.rule.ID] = prev
			continue
		}
		st := &status{rule: c.rule, state: StateNormal, known: false, updatedAt: now}
		if c.rule.Disabled {
			st.acked = true
			st.reason = "disabled"
		}
		states[c.rule.ID] = st
		if existed && prev.state != StateNormal {
			// 定义变了，旧状态不再有意义，但消费方必须收到一次收尾，
			// 否则它缓存的「正在报警」/「待确认」永远不会消失。
			// cleared 也要收尾：retained 主题上它是一个真实的、「已恢复待
			// 确认」的状态，规则被改掉后这个待确认项已经不存在了。
			events = append(events, Event{Type: EventReset, At: now, From: prev.state, Status: st.snapshot()})
		}
	}
	// 被删掉的规则也要收尾：MQTT 的报警状态是 retained 的，不显式清一次，
	// 主题上会永久留着一个「正在报警」的幽灵。
	for _, old := range e.rules {
		if _, kept := states[old.rule.ID]; kept {
			continue
		}
		from := StateNormal
		if prev := e.states[old.rule.ID]; prev != nil {
			from = prev.state
		}
		ghost := Status{
			ID: old.rule.ID, Name: old.rule.Name, Severity: old.rule.Severity,
			State: StateNormal, Disabled: true, UpdatedAt: now,
		}
		events = append(events, Event{Type: EventRemoved, At: now, From: from, Status: ghost})
	}

	index := make(map[string][]*compiled)
	for _, c := range compiledRules {
		if c.rule.Disabled {
			continue
		}
		refs := []string{c.rule.Point}
		if c.prog != nil {
			refs = c.prog.Refs()
		}
		for _, ref := range refs {
			dev, pt, ok := SplitRef(ref)
			if !ok {
				continue
			}
			key := dev + "/" + pt
			index[key] = append(index[key], c)
		}
	}

	e.rules, e.byID, e.states, e.index = compiledRules, byID, states, index
	for _, ev := range events {
		e.emitLocked(ev)
	}
	return nil
}

// Start 启动兜底评估循环。重复调用安全。
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.done = make(chan struct{})
	e.mu.Unlock()

	go e.loop(ctx)
}

// Stop 停止评估循环并等待退出。
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.started = false
	e.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

func (e *Engine) loop(ctx context.Context) {
	defer close(e.done)
	t := time.NewTicker(e.sweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Sweep()
		}
	}
}

// Sweep 重新评估全部启用规则。它负责两件事：
//   - 让到点的延时在没有新采样时也能生效；
//   - 让「输入变坏」被及时看到（否则 unknown 只在下次采样时才更新）。
//
// 评估是纯计算 + 内存读，成本与规则数成正比，与点位数量无关。
func (e *Engine) Sweep() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, c := range e.rules {
		if c.rule.Disabled {
			continue
		}
		e.evaluateLocked(c, now)
	}
}

// OnSample 在采样更新时立即评估依赖它的规则。
//
// 与 Sweep 并存不重复触发：状态迁移是「时间 + 条件」的函数，重复评估同一
// 时刻不会产生第二次事件。前者给的是精度（onDelay=0 时立即报警），
// 后者给的是兜底（设备静默、只剩定时器在跑）。
func (e *Engine) OnSample(s model.Sample) {
	key := s.DeviceID + "/" + s.PointID
	e.mu.Lock()
	defer e.mu.Unlock()
	rules := e.index[key]
	if len(rules) == 0 {
		return
	}
	now := e.now()
	for _, c := range rules {
		e.evaluateLocked(c, now)
	}
}

func (e *Engine) evaluateLocked(c *compiled, now time.Time) {
	st, ok := e.states[c.rule.ID]
	if !ok {
		return
	}
	if c.rule.Disabled {
		return
	}
	prevState := st.state
	activeLike := prevState == StateActive || prevState == StateAcked
	cond, value, quality, known, reason := c.eval(e.lookup, activeLike)

	st.quality = quality
	st.known = known
	st.reason = reason
	st.updatedAt = now
	if known {
		st.value, st.hasValue = value, true
	}

	if !known {
		// 未知：只更新可观测字段。
		//
		// 唯一允许的状态变化是取消 pending —— 一段「不知道」的时间不能被
		// 用来凑够 onDelay。已经 active/acked 的报警保持原状：数据断了就
		// 清除，等于把「看不见」当成「好了」。
		if st.state == StatePending {
			st.state = StateNormal
			st.pendingSince = time.Time{}
		}
		return
	}

	on := c.rule.OnDelay.Duration()
	off := c.rule.OffDelay.Duration()

	switch st.state {
	case StateNormal, StateCleared:
		if !cond {
			return
		}
		if on <= 0 {
			e.raiseLocked(st, now, prevState)
			return
		}
		st.state = StatePending
		st.pendingSince = now

	case StatePending:
		if !cond {
			st.state = StateNormal
			st.pendingSince = time.Time{}
			return
		}
		if on <= 0 || now.Sub(st.pendingSince) >= on {
			e.raiseLocked(st, now, prevState)
		}

	case StateActive, StateAcked:
		if cond {
			st.clearSince = time.Time{}
			return
		}
		if st.clearSince.IsZero() {
			st.clearSince = now
		}
		if off <= 0 || now.Sub(st.clearSince) >= off {
			e.clearLocked(st, now, prevState)
		}
	}
}

func (e *Engine) raiseLocked(st *status, now time.Time, from State) {
	st.state = StateActive
	st.acked = false
	st.count++
	st.since = now
	st.clearedAt = time.Time{}
	st.clearSince = time.Time{}
	st.pendingSince = time.Time{}
	st.ackAt = time.Time{}
	st.ackBy = ""
	e.emitLocked(Event{Type: EventRaised, At: now, From: from, Status: st.snapshot()})
}

func (e *Engine) clearLocked(st *status, now time.Time, from State) {
	wasAcked := st.acked
	st.clearedAt = now
	st.clearSince = time.Time{}
	switch {
	case st.rule.AutoAck:
		st.state = StateNormal
		st.acked = true
		st.ackAt = now
		st.ackBy = "auto"
		st.reason = ""
	case wasAcked:
		// 报警期间已经确认过；恢复后无事可做。
		st.state = StateNormal
	default:
		// 恢复了但没人确认过：留在 cleared，等人处理。
		st.state = StateCleared
	}
	e.emitLocked(Event{Type: EventCleared, At: now, From: from, Status: st.snapshot()})
}

// ---------------------------------------------------------------------------
// 确认
// ---------------------------------------------------------------------------

// Ack 确认一条报警。by 是操作人标识（可为空，表示匿名）。
//
// 幂等性是有意的：ack 请求会被客户端重试，重复确认一条已确认的报警返回
// 成功更安全。但确认一条从未报警的规则是明确的错误（ErrNotActive），
// 因为那说明调用方拿错了 ID 或状态已经过期。
func (e *Engine) Ack(id, by string) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ackLocked(id, by)
}

func (e *Engine) ackLocked(id, by string) (Status, error) {
	st, ok := e.states[id]
	if !ok {
		return Status{}, fmt.Errorf("%w: alarm %q", ErrNotFound, id)
	}
	now := e.now()
	switch st.state {
	case StateActive:
		st.state = StateAcked
		st.acked = true
		st.ackAt = now
		st.ackBy = by
		e.emitLocked(Event{Type: EventAcknowledged, At: now, From: StateActive, Status: st.snapshot()})
	case StateCleared:
		st.state = StateNormal
		st.acked = true
		st.ackAt = now
		st.ackBy = by
		st.reason = ""
		e.emitLocked(Event{Type: EventAcknowledged, At: now, From: StateCleared, Status: st.snapshot()})
	case StateAcked:
		// 已确认：幂等返回，不产生事件。
	case StateNormal:
		return st.snapshot(), fmt.Errorf("%w: alarm %q is %s", ErrNotActive, id, st.state)
	case StatePending:
		return st.snapshot(), fmt.Errorf("%w: alarm %q is pending (waiting for onDelay)", ErrNotActive, id)
	}
	return st.snapshot(), nil
}

// AckAll 确认全部可确认的报警（active / cleared），返回被确认的快照。
// 已经 acked 的不重复计数，pending / normal 静默跳过——批量操作不该因为
// 一条「还没到延时」而整体失败。
func (e *Engine) AckAll(by string) []Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Status, 0, len(e.states))
	for _, c := range e.rules {
		st := e.states[c.rule.ID]
		if st == nil {
			continue
		}
		if st.state != StateActive && st.state != StateCleared {
			continue
		}
		if s, err := e.ackLocked(c.rule.ID, by); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// AckMany 批量确认。先整体校验再应用：不会出现「前两条成功、第三条失败」
// 的半完成结果，那会让调用方无法判断到底该重试哪一条。
func (e *Engine) AckMany(ids []string, by string) ([]Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		st, ok := e.states[id]
		if !ok {
			return nil, fmt.Errorf("%w: alarm %q", ErrNotFound, id)
		}
		if st.state != StateActive && st.state != StateCleared && st.state != StateAcked {
			return nil, fmt.Errorf("%w: alarm %q is %s", ErrNotActive, id, st.state)
		}
	}
	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			continue
		}
		delete(seen, id)
		if s, err := e.ackLocked(id, by); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// List 返回满足条件的报警，按「严重度降序、激活时间升序、ID」排序。
// 这个顺序就是运维处理顺序：越严重越靠前，同级里越早发生的越靠前。
func (e *Engine) List(f Filter) []Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	states := make(map[State]bool, len(f.States))
	for _, s := range f.States {
		states[s] = true
	}
	sevs := make(map[Severity]bool, len(f.Severities))
	for _, s := range f.Severities {
		sevs[s] = true
	}
	q := strings.ToLower(strings.TrimSpace(f.Q))

	out := make([]Status, 0, len(e.rules))
	for _, c := range e.rules {
		st := e.states[c.rule.ID]
		if st == nil {
			continue
		}
		if len(states) > 0 && !states[st.state] {
			continue
		}
		if len(sevs) > 0 && !sevs[st.rule.Severity] {
			continue
		}
		if q != "" {
			hay := strings.ToLower(st.rule.ID + " " + st.rule.Name + " " + st.rule.Message)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, st.snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity.rank() > out[j].Severity.rank()
		}
		si, sj := out[i].Since, out[j].Since
		switch {
		case si != nil && sj != nil && !si.Equal(*sj):
			return si.Before(*sj)
		case si != nil && sj == nil:
			return true
		case si == nil && sj != nil:
			return false
		}
		return out[i].ID < out[j].ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// Get 返回单条报警。
func (e *Engine) Get(id string) (Status, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.states[id]
	if !ok {
		return Status{}, false
	}
	return st.snapshot(), true
}

// Counts 返回状态汇总。
func (e *Engine) Counts() Counts {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := Counts{Total: len(e.rules)}
	for _, rule := range e.rules {
		st := e.states[rule.rule.ID]
		if st == nil {
			continue
		}
		switch st.state {
		case StateNormal:
			c.Normal++
		case StatePending:
			c.Pending++
		case StateActive:
			c.Active++
		case StateAcked:
			c.Acked++
		case StateCleared:
			c.Cleared++
		}
		// 停用的规则永远评不出可信值，不能算进 unknown —— 否则「关掉一半
		// 报警」会表现为「一半数据链路坏了」。
		if !st.known && !st.rule.Disabled {
			c.Unknown++
		}
	}
	return c
}

// Rules 返回当前规则（只读副本），用于诊断包与配置对账。
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Rule, len(e.rules))
	for i, c := range e.rules {
		out[i] = c.rule
	}
	return out
}

// ---------------------------------------------------------------------------
// 订阅（SSE / MQTT）
// ---------------------------------------------------------------------------

// Subscribe 订阅状态迁移事件。buffer<=0 时取 64。返回的 cancel 必须调用。
func (e *Engine) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	e.mu.Lock()
	id := e.nextID
	e.nextID++
	sub := &subscriber{ch: make(chan Event, buffer)}
	e.subs[id] = sub
	e.mu.Unlock()
	return sub.ch, func() {
		e.mu.Lock()
		if s, ok := e.subs[id]; ok {
			delete(e.subs, id)
			s.closed = true
		}
		e.mu.Unlock()
	}
}

// emitLocked 把事件推给所有订阅者。丢弃最旧而不是阻塞：采集与 REST 都不能
// 被一个慢客户端拖住，而报警事件的价值随时间快速衰减，积压比丢一条更糟。
func (e *Engine) emitLocked(ev Event) {
	for _, sub := range e.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}
