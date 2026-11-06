package runtime

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"uic/internal/calc"
	"uic/internal/model"
	"uic/internal/obs"
)

// errHistoryOff 表示配置里没有开启历史。单独一个错误值是为了让适配器能把
// 「功能没开」与「查询失败」分开计数——前者是配置问题，后者是运维问题。
var errHistoryOff = errors.New("history is disabled")

// windowKey 是缓存键。同一个点位取 5 分钟与 1 小时是两次独立查询。
type windowKey struct {
	device string
	point  string
	window time.Duration
}

// windowEntry 是一次窗口查询的缓存结果。错误也缓存：磁盘故障时每一拍都去
// 重试只会让故障更严重，而且日志会被同一行错误刷爆。
type windowEntry struct {
	stats   model.WindowStats
	ok      bool
	expires time.Time
}

// historyWindow 给「历史窗口聚合」提供带 TTL 的缓存。
//
// 为什么必须缓存：报警引擎每 200ms 做一次兜底评估，每条带窗口的规则都会
// 扫多个历史段。一个 5 分钟窗口在 7 天保留期里可能要打开几十个段文件——
// 不缓存时，一个 200ms 的扫描循环会把磁盘 IO 打满，而 5 分钟均值在 1 秒内
// 几乎不变。计算点驱动的采集周期更短，同样受益。
//
// TTL 取 window/60，夹在 1s..30s：窗口越长容忍的陈旧度越大；但绝不超过
// 30s，否则「最近 1 分钟均值」在报警判定里会明显慢半拍。
type historyWindow struct {
	mu      sync.Mutex
	entries map[windowKey]windowEntry
	query   func(device, point string, window time.Duration) (model.WindowStats, error)
	log     *slog.Logger
	metrics *obs.Metrics
	now     func() time.Time
}

func newHistoryWindow(
	query func(device, point string, window time.Duration) (model.WindowStats, error),
	log *slog.Logger,
	metrics *obs.Metrics,
) *historyWindow {
	return &historyWindow{
		entries: map[windowKey]windowEntry{},
		query:   query,
		log:     log,
		metrics: metrics,
		now:     time.Now,
	}
}

// ttlFor 是缓存有效期。
func ttlFor(window time.Duration) time.Duration {
	ttl := window / 60
	if ttl < time.Second {
		ttl = time.Second
	}
	if ttl > 30*time.Second {
		ttl = 30 * time.Second
	}
	return ttl
}

// lookup 返回窗口摘要。ok=false 表示无法提供（历史未开启或查询失败）。
//
// 这里刻意不把「缓存命中」与「真实查询」分开报告：调用方只关心结果能否
// 使用；缓存效率由 TTL 与查询次数间接体现。
func (w *historyWindow) lookup(ref string, window time.Duration) (model.WindowStats, bool) {
	dev, pt, ok := strings.Cut(ref, ":")
	if !ok || dev == "" || pt == "" {
		// 配置期已经校验过引用格式，这里只可能来自直接调用。
		return model.WindowStats{}, false
	}
	key := windowKey{dev, pt, window}
	now := w.now()

	w.mu.Lock()
	if e, hit := w.entries[key]; hit && now.Before(e.expires) {
		w.mu.Unlock()
		return e.stats, e.ok
	}
	w.mu.Unlock()

	// 查询在锁外：扫盘可能几十毫秒，持锁会让所有其他窗口排队。
	st, err := w.query(dev, pt, window)

	e := windowEntry{stats: st, ok: err == nil, expires: now.Add(ttlFor(window))}
	result := "ok"
	cache := true
	switch {
	case errors.Is(err, errHistoryOff):
		// 历史没开是启动期的配置状态（而且 openHistory 在 openAlarms 之后），
		// 不缓存：一是不该让「刚打开历史」多等一个 TTL，二是这个判断只是
		// 一次 nil 比较，没有缓存的价值。
		e.stats, e.ok = model.WindowStats{}, false
		result, cache = "unavailable", false
	case err != nil:
		e.stats, e.ok = model.WindowStats{}, false
		result = "unavailable"
	case st.Count == 0:
		result = "empty"
		// 空窗口的 TTL 压到 1s：这是「数据还没积累够」的暂态，5 分钟的
		// 窗口如果也缓存 5s，刚启动的计算点会白白多等 5 秒才开始出值。
		if ttl := ttlFor(window); ttl > time.Second {
			e.expires = now.Add(time.Second)
		}
	}

	if cache {
		w.mu.Lock()
		// 配置热更新会移除点位/规则，缓存因此需要上限：超过 512 条时清理
		// 所有已过期条目。窗口数量由表达式数量决定，正常远小于这个值。
		if len(w.entries) > 512 {
			for k, v := range w.entries {
				if !now.Before(v.expires) {
					delete(w.entries, k)
				}
			}
		}
		w.entries[key] = e
		w.mu.Unlock()
	}

	if result == "unavailable" && !errors.Is(err, errHistoryOff) {
		// 每个 TTL 只记一次：磁盘故障期间每 200ms 一行日志会淹没真正
		// 有用的告警。历史未开启不在这里报：配置校验已经挡住了这种组合，
		// 运行期只可能是启动瞬间，而它每 200ms 会被重问一次。
		w.log.Warn("history_window_unavailable",
			"device", dev, "point", pt, "window", window.String(), "err", errString(err))
	}
	if w.metrics != nil {
		w.metrics.Inc("uic_history_window_total", obs.Labels{
			"result": result,
		})
	}
	return e.stats, e.ok
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// historyWindowAdapter 把 history.Store 包成 driver/alarm 需要的签名。
//
// 用闭包读 r.hist 而不是在构造时取一次：openAlarms 早于 openHistory 执行，
// 构造时 r.hist 还是 nil；如果那时捕获了值，所有窗口聚合会永远不可用。
func (r *Runtime) historyWindow(ref string, window time.Duration) (model.WindowStats, bool) {
	if r.win == nil {
		return model.WindowStats{}, false
	}
	return r.win.lookup(ref, window)
}

// calcWindow 给报警引擎用的适配器（calc 的 WindowStats 与 model 的分开，
// 是为了让表达式引擎保持零内部依赖）。
func (r *Runtime) calcWindow(ref string, window time.Duration) (calc.WindowStats, bool) {
	st, ok := r.historyWindow(ref, window)
	if !ok {
		return calc.WindowStats{}, false
	}
	return calc.WindowStats{Count: st.Count, Sum: st.Sum, Min: st.Min, Max: st.Max}, true
}

// newHistoryWindowSource 创建适配器。query 在每次真正查询时读取 r.hist，
// 因此启动顺序（报警先于历史）不会把它变成永久 nil。
func (r *Runtime) newHistoryWindowSource() *historyWindow {
	return newHistoryWindow(func(device, point string, window time.Duration) (model.WindowStats, error) {
		h := r.hist
		if h == nil {
			return model.WindowStats{}, errHistoryOff
		}
		return h.WindowStats(device, point, window)
	}, r.log, r.metrics)
}
