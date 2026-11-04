// Package runtime 是唯一的编排层：把配置、驱动、调度、缓存、Outbox 与
// 北向接口装配成一个可运行的服务。
//
// 依赖方向（docs/03-product/02-architecture.md §3）：
//
//	runtime → config / scheduler / cache / outbox / northbound / obs
//	runtime → driver（只通过注册表）
//
// 这里是唯一允许「知道所有模块」的地方；其他包都保持单向依赖，
// 从而可以被单独测试。
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"uic/internal/cache"
	"uic/internal/config"
	"uic/internal/driver"
	"uic/internal/driver/modbus"
	"uic/internal/driver/virtual"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
	"uic/internal/outbox"
	"uic/internal/scheduler"
)

// Options 是 runtime 的启动参数。
type Options struct {
	ConfigPath    string
	DataDir       string // 为空时使用 config.storage.dir
	Listen        string // 覆盖 config.server.listen（-listen）
	Token         string // 覆盖 config.server.token（-token）
	LogLevel      string // 覆盖 config.logging.level（-log-level）
	Version       string
	Commit        string
	BuildTime     string
	MetricsPublic bool
}

// Runtime 是装配好的服务实例。
type Runtime struct {
	opt     Options
	dataDir string
	cfg     *config.Manager
	cache   *cache.Cache
	box     *outbox.Store
	sched   *scheduler.Scheduler
	metrics *obs.Metrics
	ring    *obs.Ring
	level   *obs.LevelVar
	log     *slog.Logger
	api     *httpapi.Server

	logFile     io.Closer
	journal     *os.File
	journalSink *journalSink
	mqttSink    *mqttSink

	mu         sync.Mutex
	collectors map[string]*collector
	sinks      map[string]deliverFunc
	sinkNames  []string

	dropSeen map[string]map[string]int64
	idem     map[string]idemEntry

	started  atomic.Bool
	ready    atomic.Bool
	stopping atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	startedAt time.Time
	version   httpapi.VersionInfo
}

type idemEntry struct {
	at      time.Time
	results []httpapi.WriteResult
}

var (
	registerOnce sync.Once
	registerErr  error
)

// RegisterBuiltins 注册内置驱动。重复调用安全；供 main 在 -check 等
// 不启动 runtime 的场景下使用。
func RegisterBuiltins() error {
	registerOnce.Do(func() {
		for _, d := range []driver.Driver{modbus.New(), modbus.NewRTU(), virtual.New()} {
			if err := driver.Register(d); err != nil {
				registerErr = err
				return
			}
		}
	})
	return registerErr
}

// New 加载配置并装配全部模块（不启动调度）。
func New(opt Options) (*Runtime, error) {
	if err := RegisterBuiltins(); err != nil {
		return nil, err
	}
	mgr, err := config.Open(opt.ConfigPath, opt.DataDir)
	if err != nil {
		return nil, err
	}
	f := mgr.Current()

	dataDir := opt.DataDir
	if dataDir == "" {
		dataDir = f.Storage.Dir
	}
	if dataDir == "" {
		dataDir = "./data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: data dir: %w", err)
	}

	ring := obs.NewRing(2000)
	level := obs.NewLevelVar(firstNonEmpty(opt.LogLevel, f.Logging.Level))
	out := io.Writer(os.Stderr)
	var logFile io.Closer
	if f.Logging.File != "" {
		fh, err := os.OpenFile(f.Logging.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("runtime: log file: %w", err)
		}
		out = io.MultiWriter(os.Stderr, fh)
		logFile = fh
	}
	logger := obs.NewLogger(out, level, ring).With("site", f.Site)

	met := obs.New()
	r := &Runtime{
		opt: opt, dataDir: dataDir, cfg: mgr,
		cache: cache.New(), metrics: met, ring: ring, level: level,
		log: logger, logFile: logFile,
		collectors: map[string]*collector{},
		sinks:      map[string]deliverFunc{},
		dropSeen:   map[string]map[string]int64{},
		idem:       map[string]idemEntry{},
		wake:       make(chan struct{}, 1),
		startedAt:  time.Now(),
	}
	r.declareMetrics()
	r.sched = scheduler.New(scheduler.Options{
		// 设备运行态变化同时投递到 MQTT（retained）。scheduler 用独立
		// goroutine 串行回调，所以即使 Broker 慢也不会拖住采集。
		OnState: func(ev scheduler.StateEvent) {
			if s := r.mqttSink; s != nil {
				s.onState(ev)
			}
		},
	}, logger, met)

	if err := r.build(mgr.Current()); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}
	if err := r.openSinks(mgr.Current()); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}

	r.api = httpapi.New(httpapi.Options{
		Token:         firstNonEmpty(opt.Token, f.Server.Token),
		BasePath:      f.Server.BasePath,
		MetricsPublic: opt.MetricsPublic,
	}, r, logger)

	r.version = httpapi.VersionInfo{
		Version: opt.Version, Commit: opt.Commit, BuildTime: opt.BuildTime,
		GoVersion: runtime.Version(), Schema: config.SchemaVersion, APIVer: 1,
	}
	return r, nil
}

func (r *Runtime) declareMetrics() {
	d := func(name, typ, help string) { r.metrics.Declare(name, typ, help, nil) }
	d("uic_collect_total", "counter", "采集次数与结果")
	d("uic_collect_errors_total", "counter", "采集错误（按分类）")
	d("uic_collect_duration_seconds", "histogram", "单次采集耗时")
	d("uic_collect_avg_ms", "gauge", "设备平均采集耗时")
	d("uic_device_state", "gauge", "设备运行态（1 表示当前态）")
	d("uic_scheduler_skipped_total", "counter", "因设备在飞而跳过的轮次")
	d("uic_driver_connect_total", "counter", "驱动建链次数与结果")
	d("uic_write_total", "counter", "写入次数与结果")
	d("uic_outbox_pending", "gauge", "Outbox 待投递条数")
	d("uic_outbox_pending_bytes", "gauge", "Outbox 待投递字节数")
	d("uic_outbox_dropped_total", "counter", "Outbox 丢弃条数")
	d("uic_http_requests_total", "counter", "HTTP 请求数")
	d("uic_http_request_duration_seconds", "histogram", "HTTP 请求耗时")
	d("uic_stale_total", "counter", "点位被判定为 stale 的次数")
	d("uic_config_apply_total", "counter", "配置应用次数与结果")
	d("uic_mqtt_publish_total", "counter", "MQTT 发布次数与结果")
}

// build 按配置创建采集器并注册到调度器。
func (r *Runtime) build(f *config.File) error {
	chans := make(map[string]model.Channel, len(f.Channels))
	for _, ch := range f.Channels {
		chans[ch.ID] = ch
	}
	for _, dev := range f.Devices {
		ch, ok := chans[dev.ChannelID]
		if !ok {
			return httpapi.Errf(422, "config.invalid_channel", "device %q: channel %q not found", dev.ID, dev.ChannelID)
		}
		c, err := newCollector(r, dev, ch)
		if err != nil {
			return err
		}
		if err := r.sched.Add(dev, ch, c); err != nil {
			return err
		}
		r.collectors[dev.ID] = c
		r.markDisabled(dev)
	}
	return nil
}

// markDisabled 把停用设备的点位显式标为 disabled，而不是让它们保持
// 上一次的好值（这正是「配置了但没采」必须可见的原因）。
func (r *Runtime) markDisabled(dev model.Device) {
	if dev.Enabled {
		return
	}
	now := time.Now().UTC()
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		r.cache.Set(model.Sample{
			DeviceID: dev.ID, PointID: p.ID, Quality: model.QualityDisabled,
			Value:       model.Value{Type: p.DataType},
			CollectedAt: now, Error: "device disabled",
		})
	}
}

// Start 启动调度、Outbox 投递与看门狗。
func (r *Runtime) Start(ctx context.Context) {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.sched.Start(r.ctx)
	for name, fn := range r.sinks {
		sink, deliver := name, fn
		r.wg.Add(1)
		go r.dispatchLoop(r.ctx, sink, deliver)
	}
	r.wg.Add(1)
	go r.watchdog(r.ctx)
	r.ready.Store(true)
	r.log.Info("runtime_started",
		"config", r.cfg.Path(), "version", r.cfg.Version(),
		"devices", len(r.collectors), "sinks", r.sinkNames, "data", r.dataDir)
}

// Stop 按「停止接收 → 停调度 → 排空队列 → 关连接」的顺序关闭。
func (r *Runtime) Stop(timeout time.Duration) error {
	if !r.stopping.CompareAndSwap(false, true) {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	r.ready.Store(false)
	if r.cancel != nil {
		r.cancel()
	}
	_ = r.sched.Close(timeout / 2)

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout / 2):
		r.log.Warn("runtime_stop_timeout", "stage", "workers")
	}

	var first error
	if r.box != nil {
		if err := r.box.Flush(); err != nil {
			first = err
		}
		if err := r.box.Close(); err != nil && first == nil {
			first = err
		}
	}
	if r.mqttSink != nil {
		// 先关 MQTT：它会发 DISCONNECT，让 Broker 不触发遗嘱消息。
		// 放在 Outbox 关闭之后，避免还在投递时就断链。
		if err := r.mqttSink.close(); err != nil && first == nil {
			first = err
		}
	}
	if r.journal != nil {
		_ = r.journal.Sync()
		_ = r.journal.Close()
	}
	if r.logFile != nil {
		_ = r.logFile.Close()
	}
	r.log.Info("runtime_stopped")
	return first
}

// Handler 返回北向 HTTP handler（REST + SSE）。
func (r *Runtime) Handler() http.Handler { return r.api }

// Logger 返回结构化日志器。
func (r *Runtime) Logger() *slog.Logger { return r.log }

// Ring 返回日志环形缓冲（/api/v1/logs 与诊断包使用）。
func (r *Runtime) Ring() *obs.Ring { return r.ring }

// ConfigManager 返回配置管理器。
func (r *Runtime) ConfigManager() *config.Manager { return r.cfg }

// ListenAddr 返回实际监听地址（来自配置或 -listen）。
func (r *Runtime) ListenAddr() string {
	if r.opt.Listen != "" {
		return r.opt.Listen
	}
	return r.cfg.Current().Server.Listen
}

// Token 返回生效的 API Token（空表示关闭认证）。
func (r *Runtime) Token() string {
	return firstNonEmpty(r.opt.Token, r.cfg.Current().Server.Token)
}

func (r *Runtime) now() time.Time { return time.Now() }

// ---------------------------------------------------------------------------
// 看门狗
// ---------------------------------------------------------------------------

func (r *Runtime) watchdog(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.markStale(now)
		}
	}
}

// markStale 把「超过 3 个采集周期没有更新」的可信点位降级为 stale。
// 这是周期采集最容易漏掉的一条：设备静默掉线时，最后一个好值会一直
// 挂在界面上，看起来一切正常。
func (r *Runtime) markStale(now time.Time) {
	f := r.cfg.Current()
	for _, dev := range f.Devices {
		if !dev.Enabled {
			continue
		}
		interval := dev.Interval.Duration()
		if interval <= 0 {
			continue
		}
		limit := 3 * interval
		for _, p := range dev.Points {
			if !p.Enabled {
				continue
			}
			s, ok := r.cache.Get(dev.ID, p.ID)
			if !ok || !s.Quality.IsUsable() {
				continue
			}
			age := now.Sub(s.CollectedAt)
			if age <= limit {
				continue
			}
			s.Quality = model.QualityStale
			s.Error = fmt.Sprintf("stale: no update for %s (limit %s)", age.Truncate(time.Millisecond), limit)
			r.cache.Set(s)
			r.metrics.Inc("uic_stale_total", obs.Labels{"device": dev.ID, "point": p.ID})
			r.log.Warn("point_stale", "device", dev.ID, "point", p.ID, "age", age.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 热更新
// ---------------------------------------------------------------------------

// ApplyConfig 校验并应用配置，返回需要重启的设备列表。
func (r *Runtime) ApplyConfig(raw json.RawMessage, source string) (config.ApplyResult, error) {
	var f config.File
	if err := json.Unmarshal(raw, &f); err != nil {
		return config.ApplyResult{}, httpapi.Errf(http.StatusBadRequest, "config.invalid_json",
			"cannot parse config: %v", err)
	}
	before := r.cfg.Current()
	beforeNB, _ := json.Marshal(struct {
		N config.Northbound `json:"n"`
		S config.Storage    `json:"s"`
	}{before.Northbound, before.Storage})

	res, err := r.cfg.Apply(&f, source)
	if err != nil {
		r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "error"})
		return config.ApplyResult{}, httpapi.Errf(http.StatusUnprocessableEntity, "config.apply_failed", "%v", err)
	}
	if !res.Applied {
		return res, nil
	}
	res.Restarted = r.reload(before)
	r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "ok"})

	after := r.cfg.Current()
	afterNB, _ := json.Marshal(struct {
		N config.Northbound `json:"n"`
		S config.Storage    `json:"s"`
	}{after.Northbound, after.Storage})
	if string(beforeNB) != string(afterNB) {
		r.log.Warn("northbound_config_changed", "detail", "MQTT/OPC UA/Webhook 参数变更需重启进程生效")
	}
	return res, nil
}

// Rollback 回滚到指定版本并热更新。
func (r *Runtime) Rollback(version string) (config.ApplyResult, error) {
	before := r.cfg.Current()
	res, err := r.cfg.Rollback(version)
	if err != nil {
		if errors.Is(err, config.ErrVersionNotFound) {
			return config.ApplyResult{}, httpapi.Errf(http.StatusNotFound, "config.version_not_found", "%v", err)
		}
		return config.ApplyResult{}, httpapi.Errf(http.StatusUnprocessableEntity, "config.rollback_failed", "%v", err)
	}
	res.Restarted = r.reload(before)
	return res, nil
}

// reload 只重建「指纹变化」的设备，未变化的设备连接与死区状态保持不变。
func (r *Runtime) reload(before *config.File) []string {
	after := r.cfg.Current()
	chans := make(map[string]model.Channel, len(after.Channels))
	for _, ch := range after.Channels {
		chans[ch.ID] = ch
	}
	desired := make(map[string]model.Device, len(after.Devices))
	for _, d := range after.Devices {
		desired[d.ID] = d
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var restarted []string
	for id, c := range r.collectors {
		nd, keep := desired[id]
		if !keep {
			r.sched.Remove(id)
			delete(r.collectors, id)
			restarted = append(restarted, id)
			continue
		}
		nch := chans[nd.ChannelID]
		if fingerprint(c.dev, c.ch) == fingerprint(nd, nch) {
			continue
		}
		r.sched.Remove(id)
		delete(r.collectors, id)
		if nc, err := newCollector(r, nd, nch); err == nil {
			if err := r.sched.Add(nd, nch, nc); err == nil {
				r.collectors[id] = nc
				r.markDisabled(nd)
			}
		} else {
			r.log.Error("device_rebuild_failed", "device", id, "error", errText(err))
		}
		restarted = append(restarted, id)
	}
	for id, nd := range desired {
		if _, ok := r.collectors[id]; ok {
			continue
		}
		nch := chans[nd.ChannelID]
		nc, err := newCollector(r, nd, nch)
		if err != nil {
			r.log.Error("device_build_failed", "device", id, "error", errText(err))
			continue
		}
		if err := r.sched.Add(nd, nch, nc); err != nil {
			r.log.Error("device_register_failed", "device", id, "error", errText(err))
			continue
		}
		r.collectors[id] = nc
		r.markDisabled(nd)
		restarted = append(restarted, id)
	}
	sort.Strings(restarted)
	if len(restarted) > 0 {
		r.log.Info("config_applied", "version", r.cfg.Version(), "restarted", restarted)
	}
	_ = before
	return restarted
}

func (r *Runtime) collector(id string) (*collector, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.collectors[id]
	return c, ok
}

func (r *Runtime) deviceConfig(id string) (model.Device, model.Channel, bool) {
	f := r.cfg.Current()
	for _, d := range f.Devices {
		if d.ID != id {
			continue
		}
		for _, ch := range f.Channels {
			if ch.ID == d.ChannelID {
				return d, ch, true
			}
		}
		return d, model.Channel{}, false
	}
	return model.Device{}, model.Channel{}, false
}

// ---------------------------------------------------------------------------
// Backend: 系统
// ---------------------------------------------------------------------------

// Health 聚合设备状态、点位质量与队列水位。
func (r *Runtime) Health() httpapi.Health {
	sts := r.sched.Status()
	h := httpapi.Health{
		UptimeSeconds: int64(time.Since(r.startedAt).Seconds()),
		Version:       r.version.Version,
		ConfigVersion: r.cfg.Version(),
	}
	for _, st := range sts {
		h.Devices.Total++
		switch st.RuntimeState {
		case scheduler.StateCollecting:
			h.Devices.Collecting++
		case scheduler.StateFaulted:
			h.Devices.Faulted++
		case scheduler.StateDisabled:
			h.Devices.Disabled++
		case scheduler.StateDegraded:
			h.Devices.Degraded++
		}
		if st.RuntimeState == scheduler.StateFaulted || st.RuntimeState == scheduler.StateDegraded {
			h.Problems = append(h.Problems, httpapi.Problem{
				Device: st.ID, State: string(st.RuntimeState),
				Reason: st.Stats.LastError, Since: st.Since.UTC().Format(time.RFC3339),
			})
		}
	}
	h.Points = r.pointCounts("")
	if r.box != nil {
		for _, st := range r.box.Stats() {
			h.Outbox.Pending += st.Pending
			h.Outbox.Bytes += st.PendingBytes
			h.Outbox.Delivered += st.Delivered
			for reason, n := range st.Dropped {
				if h.Outbox.Dropped == nil {
					h.Outbox.Dropped = map[string]int64{}
				}
				h.Outbox.Dropped[reason] += n
			}
		}
	}
	h.Status = "ok"
	switch {
	case h.Devices.Total == 0:
		h.Status = "empty"
	case h.Devices.Faulted > 0:
		h.Status = "degraded"
	case h.Points.Bad > 0 || h.Points.Stale > 0:
		h.Status = "degraded"
	}
	return h
}

// pointCounts 以「配置」为准统计点位质量：未采集过的点位计入 bad，
// 停用点位单独计入 disabled。这样 total 永远等于配置中的点位数。
func (r *Runtime) pointCounts(device string) httpapi.PointCounts {
	f := r.cfg.Current()
	var c httpapi.PointCounts
	for _, dev := range f.Devices {
		if device != "" && dev.ID != device {
			continue
		}
		for _, p := range dev.Points {
			c.Total++
			s, ok := r.cache.Get(dev.ID, p.ID)
			if !ok {
				if dev.Enabled && p.Enabled {
					c.Bad++
				} else {
					c.Disabled++
				}
				continue
			}
			switch {
			case s.Quality == model.QualityDisabled:
				c.Disabled++
			case s.Quality == model.QualityStale:
				c.Stale++
			case s.Quality == model.QualityGood:
				c.Good++
			case s.Quality == model.QualityUncertain:
				c.Uncertain++
			default:
				c.Bad++
			}
		}
	}
	return c
}

// Ready 检查配置、存储与驱动注册表。
func (r *Runtime) Ready() httpapi.Ready {
	checks := []httpapi.Check{{Name: "config", OK: true, Detail: r.cfg.Version()}}
	storage := false
	detail := ""
	if fh, err := os.CreateTemp(r.dataDir, ".ready-*"); err == nil {
		name := fh.Name()
		_ = fh.Close()
		_ = os.Remove(name)
		storage = true
		detail = "writable"
	} else {
		detail = err.Error()
	}
	checks = append(checks, httpapi.Check{Name: "storage", OK: storage, Detail: detail})
	n := len(driver.List())
	checks = append(checks, httpapi.Check{Name: "drivers", OK: n > 0, Detail: fmt.Sprintf("%d registered", n)})
	if r.box != nil {
		ok := true
		checks = append(checks, httpapi.Check{Name: "outbox", OK: ok, Detail: fmt.Sprintf("%d sinks", len(r.sinkNames))})
	}
	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	return httpapi.Ready{Ready: ok, Checks: checks}
}

// Version 返回构建信息。
func (r *Runtime) Version() httpapi.VersionInfo { return r.version }

// Metrics 返回 Prometheus 文本。
func (r *Runtime) Metrics() string {
	var b strings.Builder
	if err := r.metrics.WriteText(&b); err != nil {
		return ""
	}
	return b.String()
}

// Logs 查询环形日志。
func (r *Runtime) Logs(device, level string, limit int) []obs.Entry {
	return r.ring.Query(device, level, limit)
}

// RecordHTTP 记录 API 指标。
func (r *Runtime) RecordHTTP(path string, code int, d time.Duration) {
	r.metrics.Inc("uic_http_requests_total", obs.Labels{
		"path": path, "code": fmt.Sprintf("%d", code),
	})
	r.metrics.Observe("uic_http_request_duration_seconds", d.Seconds(), obs.Labels{"path": path})
}

// Stream 订阅缓存变更。
func (r *Runtime) Stream(filter func(model.Sample) bool, buffer int) (<-chan cache.Event, func()) {
	return r.cache.Subscribe(buffer, filter)
}

// ---------------------------------------------------------------------------
// Backend: 设备与点位
// ---------------------------------------------------------------------------

// Devices 返回设备列表（含运行态摘要）。
func (r *Runtime) Devices(q string) []httpapi.DeviceView {
	f := r.cfg.Current()
	chans := make(map[string]model.Channel, len(f.Channels))
	for _, ch := range f.Channels {
		chans[ch.ID] = ch
	}
	q = strings.ToLower(strings.TrimSpace(q))
	out := make([]httpapi.DeviceView, 0, len(f.Devices))
	for _, d := range f.Devices {
		if q != "" && !strings.Contains(strings.ToLower(d.ID), q) && !strings.Contains(strings.ToLower(d.Name), q) {
			continue
		}
		st, ok := r.sched.StatusOf(d.ID)
		if !ok {
			continue
		}
		out = append(out, r.deviceView(d, chans[d.ChannelID], st))
	}
	return out
}

// Device 返回单台设备详情。
func (r *Runtime) Device(id string) (httpapi.DeviceView, bool) {
	dev, ch, ok := r.deviceConfig(id)
	if !ok {
		return httpapi.DeviceView{}, false
	}
	st, ok := r.sched.StatusOf(id)
	if !ok {
		return httpapi.DeviceView{}, false
	}
	return r.deviceView(dev, ch, st), true
}

func (r *Runtime) deviceView(dev model.Device, ch model.Channel, st scheduler.DeviceStatus) httpapi.DeviceView {
	return httpapi.DeviceView{
		ID: dev.ID, Name: dev.Name, Channel: dev.ChannelID, Protocol: ch.Protocol,
		ConfiguredState: st.ConfiguredState, RuntimeState: string(st.RuntimeState),
		Since: st.Since, Generation: st.Generation, Interval: st.Interval, NextDue: st.NextDue,
		Stats: httpapi.DeviceStats{
			ReadsTotal: st.Stats.ReadsTotal, ReadsFailed: st.Stats.ReadsFailed,
			Skipped: st.Stats.Skipped, Probes: st.Stats.Probes,
			SuccessRate: st.Stats.SuccessRate, AvgDurationMs: st.Stats.AvgDurationMs,
			ConsecutiveFailures: st.Stats.ConsecutiveFailures,
			LastError:           st.Stats.LastError, LastErrorKind: st.Stats.LastErrorKind,
			LastSuccess: st.Stats.LastSuccess,
		},
		Points: r.pointCounts(dev.ID),
	}
}

// Points 返回点位当前值；未采集过的点位以 not_connected 合成。
func (r *Runtime) Points(f httpapi.PointFilter) []model.Sample {
	cur := r.cfg.Current()
	q := strings.ToLower(strings.TrimSpace(f.Q))
	qual := make(map[string]bool, len(f.Quality))
	for _, x := range f.Quality {
		qual[x] = true
	}
	out := make([]model.Sample, 0, 64)
	for _, dev := range cur.Devices {
		if f.Device != "" && dev.ID != f.Device {
			continue
		}
		for _, p := range dev.Points {
			if q != "" && !strings.Contains(strings.ToLower(dev.ID), q) &&
				!strings.Contains(strings.ToLower(p.ID), q) &&
				!strings.Contains(strings.ToLower(p.Name), q) {
				continue
			}
			s, ok := r.cache.Get(dev.ID, p.ID)
			if !ok {
				s = model.Sample{
					DeviceID: dev.ID, PointID: p.ID,
					Value:   model.Value{Type: p.DataType},
					Quality: model.QualityNotConnected,
					Unit:    p.Unit,
					Error:   "not collected yet",
				}
				if !dev.Enabled || !p.Enabled {
					s.Quality = model.QualityDisabled
					s.Error = "disabled"
				}
			}
			if len(qual) > 0 && !qual[string(s.Quality)] {
				continue
			}
			out = append(out, s)
			if f.Limit > 0 && len(out) >= f.Limit {
				return out
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].PointID < out[j].PointID
	})
	return out
}

// Point 返回单点当前值。
func (r *Runtime) Point(device, point string) (model.Sample, bool) {
	dev, _, ok := r.deviceConfig(device)
	if !ok {
		return model.Sample{}, false
	}
	var pt model.Point
	found := false
	for _, p := range dev.Points {
		if p.ID == point {
			pt, found = p, true
			break
		}
	}
	if !found {
		return model.Sample{}, false
	}
	if s, ok := r.cache.Get(device, point); ok {
		return s, true
	}
	q := model.QualityNotConnected
	msg := "not collected yet"
	if !dev.Enabled || !pt.Enabled {
		q, msg = model.QualityDisabled, "disabled"
	}
	return model.Sample{
		DeviceID: device, PointID: point,
		Value: model.Value{Type: pt.DataType}, Quality: q, Unit: pt.Unit, Error: msg,
	}, true
}

// Write 写入单点或批量。逐项返回结果：部分失败不返回 5xx。
func (r *Runtime) Write(ctx context.Context, device string, items []httpapi.WriteItem) ([]httpapi.WriteResult, error) {
	dev, _, ok := r.deviceConfig(device)
	if !ok {
		return nil, httpapi.Errf(http.StatusNotFound, "device.not_found", "device %q not found", device)
	}
	c, ok := r.collector(device)
	if !ok {
		return nil, httpapi.Errf(http.StatusServiceUnavailable, "device.not_ready", "device %q is not running", device)
	}

	if key := httpapi.IdempotencyKey(ctx); key != "" {
		if prev, ok := r.idemGet(device + "\x00" + key); ok {
			return prev, nil
		}
	}

	points := make(map[string]model.Point, len(dev.Points))
	for _, p := range dev.Points {
		points[p.ID] = p
	}
	results := make([]httpapi.WriteResult, len(items))
	var valid []preparedWrite
	var idx []int
	for i, it := range items {
		pt, ok := points[it.Point]
		if !ok {
			results[i] = httpapi.WriteResult{
				Point: it.Point, OK: false, Quality: model.QualityAddressError,
				Code: "point.not_found", Message: "point not found on device",
			}
			continue
		}
		if !pt.Writable {
			results[i] = httpapi.WriteResult{
				Point: it.Point, OK: false, Quality: model.QualityBad,
				Code: "point.not_writable", Message: "point is read-only",
			}
			continue
		}
		if !pt.Enabled {
			results[i] = httpapi.WriteResult{
				Point: it.Point, OK: false, Quality: model.QualityDisabled,
				Code: "point.disabled", Message: "point is disabled",
			}
			continue
		}
		v := it.Value
		if v.Type == "" {
			v.Type = pt.DataType
		}
		if v.Type != pt.DataType {
			results[i] = httpapi.WriteResult{
				Point: it.Point, OK: false, Quality: model.QualityBad,
				Code:    "value.type_mismatch",
				Message: fmt.Sprintf("want %s, got %s", pt.DataType, v.Type),
			}
			continue
		}
		bo := pt.ByteOrder
		if bo == "" {
			bo = dev.ByteOrder
		}
		valid = append(valid, preparedWrite{
			item: it, pt: pt,
			req: driver.WriteRequest{
				PointID: pt.ID, Address: pt.Address, DataType: pt.DataType,
				ByteOrder: bo, Value: v,
			},
		})
		idx = append(idx, i)
	}

	if len(valid) > 0 {
		confirm := false
		for _, v := range valid {
			if v.item.Confirm {
				confirm = true
				break
			}
		}
		var wr []httpapi.WriteResult
		err := r.sched.SubmitWrite(ctx, device, func(wctx context.Context) error {
			wr = c.write(wctx, valid, confirm)
			return nil
		})
		if err != nil {
			return results, httpapi.Errf(http.StatusServiceUnavailable, "service.not_ready",
				"device %q unavailable: %v", device, err)
		}
		for i, res := range wr {
			results[idx[i]] = res
		}
	}

	if key := httpapi.IdempotencyKey(ctx); key != "" {
		r.idemPut(device+"\x00"+key, results)
	}
	return results, nil
}

func (r *Runtime) idemGet(key string) ([]httpapi.WriteResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.idem[key]
	if !ok || time.Since(e.at) > 5*time.Minute {
		delete(r.idem, key)
		return nil, false
	}
	out := make([]httpapi.WriteResult, len(e.results))
	copy(out, e.results)
	return out, true
}

func (r *Runtime) idemPut(key string, res []httpapi.WriteResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.idem) > 512 {
		for k, e := range r.idem {
			if time.Since(e.at) > 5*time.Minute {
				delete(r.idem, k)
			}
		}
	}
	cp := make([]httpapi.WriteResult, len(res))
	copy(cp, res)
	r.idem[key] = idemEntry{at: time.Now(), results: cp}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
