package runtime

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"uic/internal/config"
	"uic/internal/history"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
)

// openHistory 打开历史采样日志。
//
// 与 Outbox 的区别：Outbox 是「必须送达」的队列（不丢、有确认位点、可重放），
// 历史是「有界缓存」——写不进去就丢最新的、放不下就淘汰最旧的，绝不反压采集。
// 因此这里失败只返回错误让启动失败（配置说开启了历史却打不开目录，属于部署
// 错误），而运行期写入失败只计数。
func (r *Runtime) openHistory(f *config.File) error {
	h := f.Storage.History
	if !h.Enabled {
		return nil
	}
	store, err := history.Open(history.Options{
		Dir:          h.Dir,
		MaxBytes:     h.MaxBytes,
		MaxAge:       h.MaxAge.Duration(),
		SegmentBytes: h.SegmentBytes,
		Log:          r.log,
	})
	if err != nil {
		return httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid_history", "%v", err)
	}
	r.hist = store
	r.histWriter = newHistoryWriter(store)
	return nil
}

// historyPolicy 是每个采样点都要用到的三个参数。它们从当前配置读取，
// 因此调整 minInterval / heartbeat / deadband 不需要重启。
type historyPolicy struct {
	minInterval time.Duration
	heartbeat   time.Duration
	deadband    float64
}

func policyOf(h config.History) historyPolicy {
	p := historyPolicy{minInterval: h.MinInterval.Duration(), deadband: h.Deadband}
	if h.Heartbeat != nil {
		p.heartbeat = h.Heartbeat.Duration()
	}
	return p
}

// historyPointState 是每个点位「上次落盘」的水位。
//
// 它必须记录「上次落盘的值」而不是「上次采到的值」：死区比较的参照物是
// 磁盘上那条记录，否则一条缓慢爬升的曲线（每次变化都小于死区）会永远不落盘。
type historyPointState struct {
	lastAt  time.Time
	lastVal model.Value
	lastQ   model.Quality
}

// historyWriter 把缓存事件转换成有界的采样记录。
type historyWriter struct {
	store   *history.Store
	now     func() time.Time
	states  map[string]*historyPointState
	pending map[string]model.Sample

	written uint64
	errs    uint64
}

func newHistoryWriter(s *history.Store) *historyWriter {
	return &historyWriter{
		store:   s,
		now:     time.Now,
		states:  map[string]*historyPointState{},
		pending: map[string]model.Sample{},
	}
}

// consider 处理一次缓存事件，返回是否发生落盘。
//
// 三条规则，对应三种运维视角：
//   - 质量码 disabled 不记：点位是被显式关掉的，记录它只会污染趋势；
//   - 变化超过死区就记：曲线要能反映真实变化；
//   - 两次落盘至少间隔 minInterval：设备高频抖动不能把磁盘写爆。
//
// 被 minInterval 挡下的样本进入 pending，而不是直接丢弃——否则「快速变化
// 之后立刻稳定」的最终值永远不会落盘，曲线会停在变化前的那一刻。
func (w *historyWriter) consider(s model.Sample, p historyPolicy) bool {
	if s.Quality == model.QualityDisabled {
		return false
	}
	key := s.DeviceID + "/" + s.PointID
	st := w.states[key]
	if st == nil {
		st = &historyPointState{}
		w.states[key] = st
	}
	now := w.now()
	if p.minInterval > 0 && !st.lastAt.IsZero() && now.Sub(st.lastAt) < p.minInterval {
		w.pending[key] = s
		return false
	}
	if !w.changed(st, s.Value, s.Quality, p.deadband) {
		return false
	}
	delete(w.pending, key)
	return w.write(st, s, now)
}

// tick 处理到期的 pending 样本。返回是否有写入发生。
func (w *historyWriter) tick(p historyPolicy) bool {
	if len(w.pending) == 0 {
		return false
	}
	now := w.now()
	wrote := false
	for key, s := range w.pending {
		if s.Quality == model.QualityDisabled {
			delete(w.pending, key)
			continue
		}
		st := w.states[key]
		if st == nil {
			st = &historyPointState{}
			w.states[key] = st
		}
		if p.minInterval > 0 && !st.lastAt.IsZero() && now.Sub(st.lastAt) < p.minInterval {
			continue
		}
		delete(w.pending, key)
		if !w.changed(st, s.Value, s.Quality, p.deadband) {
			continue
		}
		if w.write(st, s, now) {
			wrote = true
		}
	}
	return wrote
}

// heartbeat 对「还在采集但值长期不变」的点位补一个点。
//
// 只有样本本身还是新鲜的才补：设备已经停了、缓存里的值停在 10 分钟前，
// 这时按心跳写一条「此刻仍然是 42」就是在编造数据。判定用 CollectedAt
// 而不是「缓存里有值」——后者在设备掉线后依然成立。
func (w *historyWriter) heartbeat(f *config.File, p historyPolicy, now time.Time, get func(dev, pt string) (model.Sample, bool)) bool {
	if p.heartbeat <= 0 {
		return false
	}
	wrote := false
	for _, dev := range f.Devices {
		if !dev.Enabled {
			continue
		}
		for _, pt := range dev.Points {
			if !pt.Enabled {
				continue
			}
			s, ok := get(dev.ID, pt.ID)
			if !ok || s.Quality == model.QualityDisabled {
				continue
			}
			if !s.CollectedAt.IsZero() && now.Sub(s.CollectedAt) > p.heartbeat {
				continue // 采集已经停了，补点等于伪造新鲜度
			}
			key := dev.ID + "/" + pt.ID
			st := w.states[key]
			if st != nil && now.Sub(st.lastAt) < p.heartbeat {
				continue
			}
			if st == nil {
				st = &historyPointState{}
				w.states[key] = st
			}
			if w.write(st, s, now) {
				wrote = true
			}
		}
	}
	return wrote
}

func (w *historyWriter) changed(st *historyPointState, v model.Value, q model.Quality, deadband float64) bool {
	if st.lastAt.IsZero() {
		return true
	}
	if st.lastQ != q || st.lastVal.Type != v.Type {
		return true
	}
	if v.Type.IsNumeric() {
		d := math.Abs(v.Num - st.lastVal.Num)
		// 死区为 0 时要求「有变化」，而不是「差值 ≥ 0」——后者对每一帧
		// 都成立，等于没有死区。
		return d > 0 && d >= deadband
	}
	return !v.Equal(st.lastVal)
}

func (w *historyWriter) write(st *historyPointState, s model.Sample, now time.Time) bool {
	ts := s.CollectedAt
	if ts.IsZero() || !ts.After(st.lastAt) || ts.After(now) {
		// 采集时刻不可用（或比上一条还早）时退化为落盘时刻：历史查询
		// 承诺按时间升序返回，写入端必须保证单调。
		ts = now
	}
	rec := history.Record{
		Time: ts.UTC(), Device: s.DeviceID, Point: s.PointID,
		Value: s.Value, Quality: s.Quality,
	}
	if err := w.store.Write(rec); err != nil {
		// 历史是旁路：写失败只计数，绝不反压采集。ErrClosed 之后继续写
		// 只会重复报错，但循环本身马上会随 ctx 退出，不必特殊处理。
		w.errs++
		return false
	}
	st.lastAt, st.lastVal, st.lastQ = ts, s.Value, s.Quality
	w.written++
	return true
}

// historyLoop 消费缓存事件并定时补点/刷盘。
//
// 固定 200ms tick 而不是「按 minInterval 动态调整」：配置可以热改，而
// 重建 ticker 会让 pending 的到期时刻变得难以推理。200ms 的空转成本是
// 一次 map 长度检查 + 一次时间比较。
func (r *Runtime) historyLoop(ctx context.Context) {
	defer r.wg.Done()
	if r.histWriter == nil {
		return
	}
	ch, cancel := r.cache.Subscribe(2048, nil)
	defer cancel()

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	nextHeartbeat := time.Time{}
	dirty := false

	for {
		select {
		case <-ctx.Done():
			r.flushHistory()
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			f := r.cfg.Current()
			if !f.Storage.History.Enabled {
				continue
			}
			if r.histWriter.consider(ev.Sample, policyOf(f.Storage.History)) {
				dirty = true
			}
		case now := <-tick.C:
			f := r.cfg.Current()
			if !f.Storage.History.Enabled {
				r.flushHistory()
				dirty = false
				continue
			}
			p := policyOf(f.Storage.History)
			if r.histWriter.tick(p) {
				dirty = true
			}
			if p.heartbeat > 0 && (nextHeartbeat.IsZero() || !now.Before(nextHeartbeat)) {
				if r.histWriter.heartbeat(f, p, now, r.cache.Get) {
					dirty = true
				}
				nextHeartbeat = now.Add(p.heartbeat)
			}
			if dirty {
				// 每个 tick 都刷一次：进程被杀最多丢 200ms 的数据，
				// 而历史查询看到的最新点不会落后于实时值太多。
				r.flushHistory()
				dirty = false
			}
			r.refreshHistoryMetrics()
		}
	}
}

func (r *Runtime) flushHistory() {
	if r.hist == nil {
		return
	}
	if err := r.hist.Flush(); err != nil {
		r.log.Warn("history_flush_failed", "error", errText(err))
	}
}

// refreshHistoryMetrics 把存储水位投影成 gauge。
func (r *Runtime) refreshHistoryMetrics() {
	if r.hist == nil || r.metrics == nil {
		return
	}
	st := r.hist.Stats()
	r.metrics.Set("uic_history_bytes", float64(st.Bytes), nil)
	r.metrics.Set("uic_history_segments", float64(st.Segments), nil)
	r.metrics.Set("uic_history_records_total", float64(st.Appended), nil)
	r.metrics.Set("uic_history_evicted_total", float64(st.Evicted), nil)
	if w := r.histWriter; w != nil {
		r.metrics.Set("uic_history_write_errors_total", float64(w.errs), nil)
	}
}

// History 执行一次历史查询。它是北向 Backend 的实现。
func (r *Runtime) History(q history.Query) (history.Result, error) {
	if r.hist == nil {
		return history.Result{}, httpapi.Errf(http.StatusConflict, "history.disabled",
			"history storage is not enabled in this configuration")
	}
	res, err := r.hist.Query(q)
	switch {
	case err == nil:
		r.metrics.Inc("uic_history_query_total", obs.Labels{"result": "ok"})
		return res, nil
	case errors.Is(err, history.ErrInvalidRange):
		r.metrics.Inc("uic_history_query_total", obs.Labels{"result": "invalid"})
		return res, httpapi.Errf(http.StatusBadRequest, "history.invalid_range", "%v", err)
	case errors.Is(err, history.ErrInvalidQuery):
		r.metrics.Inc("uic_history_query_total", obs.Labels{"result": "invalid"})
		return res, httpapi.Errf(http.StatusBadRequest, "history.invalid_query", "%v", err)
	case errors.Is(err, history.ErrClosed):
		r.metrics.Inc("uic_history_query_total", obs.Labels{"result": "closed"})
		return res, httpapi.Errf(http.StatusServiceUnavailable, "history.unavailable",
			"history storage is closed")
	default:
		r.metrics.Inc("uic_history_query_total", obs.Labels{"result": "error"})
		return res, err
	}
}

// HistoryStats 返回历史库水位。第二个返回值表示是否启用。
func (r *Runtime) HistoryStats() (history.Stats, bool) {
	if r.hist == nil {
		return history.Stats{}, false
	}
	return r.hist.Stats(), true
}

// historyFingerprint 描述「必须重启才能生效」的那部分历史配置。
func historyFingerprint(h config.History) string {
	if !h.Enabled {
		return "disabled"
	}
	// 只需相等比较，不需要抗碰撞：用可读字符串比哈希更好排查。
	return h.Dir + "|" + strconv.FormatInt(h.MaxBytes, 10) + "|" +
		strconv.FormatInt(h.SegmentBytes, 10) + "|" + h.MaxAge.String()
}
