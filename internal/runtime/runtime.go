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

	"uic/internal/alarm"
	"uic/internal/audit"
	"uic/internal/auth"
	"uic/internal/cache"
	"uic/internal/config"
	"uic/internal/driver"
	adsdrv "uic/internal/driver/ads"
	bacnetdrv "uic/internal/driver/bacnet"
	bacnetmstpdrv "uic/internal/driver/bacnetmstp"
	cjt188drv "uic/internal/driver/cjt188"
	coapdrv "uic/internal/driver/coap"
	"uic/internal/driver/computed"
	dlmsdrv "uic/internal/driver/dlms"
	dlt645drv "uic/internal/driver/dlt645"
	dnp3drv "uic/internal/driver/dnp3"
	enipdrv "uic/internal/driver/enip"
	finsdrv "uic/internal/driver/fins"
	hostlinkdrv "uic/internal/driver/hostlink"
	iec101drv "uic/internal/driver/iec101"
	"uic/internal/driver/iec104"
	iec61850drv "uic/internal/driver/iec61850"
	knxdrv "uic/internal/driver/knx"
	mbusdrv "uic/internal/driver/mbus"
	mcdrv "uic/internal/driver/mc"
	mewtocoldrv "uic/internal/driver/mewtocol"
	"uic/internal/driver/modbus"
	mqttdriver "uic/internal/driver/mqtt"
	mtconndrv "uic/internal/driver/mtconnect"
	opcuadriver "uic/internal/driver/opcua"
	"uic/internal/driver/s7"
	secsdrv "uic/internal/driver/secs"
	sl651drv "uic/internal/driver/sl651"
	snmpdrv "uic/internal/driver/snmp"
	toledodrv "uic/internal/driver/toledo"
	"uic/internal/driver/virtual"
	"uic/internal/history"
	"uic/internal/instanceid"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
	"uic/internal/outbox"
	"uic/internal/scheduler"
	slavemb "uic/internal/slave/modbus"
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

// northboundSink 是 MQTT 北向的两种载荷格式共用的接口。
//
// json 与 sparkplug-b 共用 Broker 连接、凭据和 Outbox，但生命周期差异
// 很大：前者一条采样一条消息，后者要先 BIRTH 再 DDATA。把差异关在接口
// 后面，runtime 只认识「投递 / 状态 / 删除 / 报警 / 关闭」几件事。
//
// onRemove 是设备从配置里被整体删掉时（reload 检测到 desired 集合里不再有
// 它）的回调。json 格式没有设备生命周期，删除只是清掉自己缓存的状态；但
// Sparkplug 不行——DBIRTH 会把它列出的全部点位重新告诉消费方，设备早已
// 不在配置里却还在「出生」，Host 会一直以为它在线。所以删除必须显式
// 走 onRemove 发 DDEATH、忘掉它的点位，而不是等下一次重连补发陈旧 BIRTH。
type northboundSink interface {
	deliver(ctx context.Context, recs []outbox.Record) (int, error)
	onState(ev scheduler.StateEvent)
	onRemove(deviceID string)
	onAlarmEvent(ev alarm.Event)
	seedAlarms(statuses []alarm.Status)
	setMetrics(m *obs.Metrics)
	close() error
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
	// instanceID 是本实例的稳定身份（持久化在 dataDir）。用于派生 MQTT
	// clientID/Sparkplug nodeId 的默认值（同站点多实例默认不冲突）与运维识别。
	instanceID string

	logFile     io.Closer
	journal     *os.File
	journalSink *journalSink
	mqttSink    northboundSink
	alarms      *alarm.Engine
	hist        *history.Store
	histWriter  *historyWriter
	win         *historyWindow
	audit       *audit.Store
	auth        *auth.Reloadable
	auditErrors atomic.Uint64

	// nbMu 只保护北向 OPC UA 实例的启停。不能复用 mu：reload 已经持着 mu
	// 调 syncOPCUA，再拿一次就是自锁。
	nbMu    sync.Mutex
	nbOPCUA *opcuaNorthbound

	// reloadMu 串行化整次热更新（含释放 mu 之后的北向通知）。配置应用与
	// 回滚可以并发进来：若不串行化，两次 reload 交错执行时，「删除设备」
	// 与「同 ID 重配」两条路径的 notifyRemoved 顺序可能颠倒，sink 会为
	// 一台刚重建的设备补发 DDEATH。
	reloadMu sync.Mutex

	mu          sync.Mutex
	collectors  map[string]*collector
	slaves      map[string]*slavemb.Server
	slavePrints map[string]string
	sinks       map[string]deliverFunc
	sinkNames   []string

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

	// staged 是灰度发布的编排状态（见 staged.go）。用独立锁：灰度会调用
	// ApplyConfig（内部走 cfg.Apply 与 reload 的锁），塞进同一把锁会成环。
	staged stagedState
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
		for _, d := range []driver.Driver{
			modbus.New(), modbus.NewRTU(), modbus.NewASCII(),
			s7.New(), opcuadriver.New(), mqttdriver.New(), computed.New(), virtual.New(),
			mcdrv.New(),
			finsdrv.New(),
			enipdrv.New(),
			bacnetdrv.New(),
			bacnetmstpdrv.New(),
			iec104.New(),
			iec101drv.New(),
			mewtocoldrv.New(),
			hostlinkdrv.New(),
			dlt645drv.New(),
			dnp3drv.New(),
			cjt188drv.New(),
			snmpdrv.New(),
			adsdrv.New(),
			coapdrv.New(),
			knxdrv.New(),
			iec61850drv.New(),
			secsdrv.New(),
			mtconndrv.New(),
			mbusdrv.New(),
			dlmsdrv.New(),
			sl651drv.New(),
			toledodrv.New(),
		} {
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
	// 窗口聚合的数据源必须在 openAlarms 之前就位（报警是第二步，历史是
	// 第三步）。适配器内部按需读取 r.hist，因此这里构造时历史还没打开也
	// 不会让它永久不可用。
	r.win = r.newHistoryWindowSource()
	// 版本信息必须早于任何子系统：从站（FC43）、MQTT 的 $birth、OPC UA 的
	// BuildInfo 都会在启动阶段就把版本报给外部。晚一步赋值，这些对外声明
	// 就会永久停留在空值上——配置热更新也不会纠正它们。
	// 实例身份：先于 openSinks 就位，因为 sink 的 clientID/nodeId 默认值要
	// 派生自它。加载失败不致命——记日志后继续，缺身份只影响「多实例默认不冲突」
	// 这层兜底，不该让整个进程起不来。
	if id, err := instanceid.Load(dataDir); err != nil {
		logger.Warn("instance_id_load_failed", "error", errText(err))
	} else {
		r.instanceID = id
	}
	r.version = httpapi.VersionInfo{
		Version: opt.Version, Commit: opt.Commit, BuildTime: opt.BuildTime,
		GoVersion: runtime.Version(), Schema: config.SchemaVersion, APIVer: 1,
		InstanceID: r.instanceID,
	}
	r.sched = scheduler.New(scheduler.Options{
		// 设备运行态变化同时投递到 MQTT（retained）。scheduler 用独立
		// goroutine 串行回调，所以即使 Broker 慢也不会拖住采集。
		OnState: r.onDeviceState,
	}, logger, met)

	// 启动失败的每一步都走同一个收口：逐个 case 手写 Close 会漏——每加一个
	// 子系统就多一条 return 路径，而「进程启动失败但审计段还开着」在测试里
	// 表现为临时目录删不掉，在生产里表现为重启后段文件被锁。
	steps := []func(*config.File) error{
		r.build,
		r.openAlarms,
		r.openHistory,
		r.buildAuth,
		r.openAudit,
		r.openSlaves,
		r.openOPCUA,
		r.openSinks,
	}
	for _, step := range steps {
		if err := step(mgr.Current()); err != nil {
			r.abortStartup()
			return nil, err
		}
	}

	r.api = httpapi.New(httpapi.Options{
		Token:         firstNonEmpty(opt.Token, f.Server.Token),
		BasePath:      f.Server.BasePath,
		MetricsPublic: opt.MetricsPublic,
		Auth:          r.auth,
		Audit:         r,
	}, r, logger)

	return r, nil
}

// abortStartup 按打开的逆序释放已获取的资源。启动失败时调用。
//
// 它不追求「和 Stop 一样完整」——进程马上就要退出——但必须把**持有文件
// 句柄与后台 goroutine** 的东西关掉。否则：测试里临时目录删不掉；生产里
// 重启后旧进程还没退干净，新进程抢不到段文件锁。
func (r *Runtime) abortStartup() {
	r.closeOPCUA()
	r.closeSlaves()
	if r.alarms != nil {
		r.alarms.Stop()
	}
	_ = r.closeAudit()
	if r.hist != nil {
		_ = r.hist.Close()
		r.hist = nil
	}
	if r.box != nil {
		_ = r.box.Flush()
		_ = r.box.Close()
	}
	if r.mqttSink != nil {
		_ = r.mqttSink.close()
	}
	if r.journal != nil {
		_ = r.journal.Sync()
		_ = r.journal.Close()
	}
	if r.logFile != nil {
		_ = r.logFile.Close()
		r.logFile = nil
	}
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
	d("uic_slave_requests_total", "counter", "从站请求数（按单元/功能码/结果）")
	d("uic_slave_degraded_reads_total", "counter", "从站用非好质量数据应答的次数")
	d("uic_slave_write_total", "counter", "从站写入转发次数与结果")
	d("uic_slave_bad_frames_total", "counter", "从站收到的坏帧数（按原因：crc/lrc/short/noise）")
	d("uic_alarm_events_total", "counter", "报警状态迁移次数（按类型/级别）")
	d("uic_alarm_active", "gauge", "当前未恢复的报警数（按级别，含已确认）")
	d("uic_alarm_pending", "gauge", "处于 onDelay 等待期的报警数")
	d("uic_alarm_cleared", "gauge", "已恢复但未确认的报警数")
	d("uic_alarm_unknown", "gauge", "最近一次评估时输入不可信的报警数")
	d("uic_alarm_rules", "gauge", "已加载的报警规则数")
	d("uic_history_bytes", "gauge", "历史库占用字节数")
	d("uic_history_segments", "gauge", "历史库段文件数")
	d("uic_history_records_total", "gauge", "历史库累计写入记录数")
	d("uic_history_evicted_total", "gauge", "因保留策略淘汰的段数")
	d("uic_history_write_errors_total", "gauge", "历史写入失败次数")
	d("uic_history_query_total", "counter", "历史查询次数与结果")
	d("uic_history_window_total", "counter", "历史窗口聚合查询次数与结果（ok/empty/unavailable）")
	d("uic_opcua_server_up", "gauge", "北向 OPC UA 服务端是否在监听")
	d("uic_opcua_server_writes_total", "counter", "北向 OPC UA 写入次数与结果")
	d("uic_audit_records_total", "counter", "写入审计日志的记录数")
	d("uic_audit_write_errors_total", "counter", "审计写入失败次数（不等于业务失败）")
	d("uic_audit_last_seq", "gauge", "审计链最后一条记录的序号")
	d("uic_audit_query_total", "counter", "审计查询次数")
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

// notifyRemoved 把「一台设备已从配置里消失」告诉北向 sink。
//
// 必须在持有 r.mu 的情况下调用（调用点都在 reload 的临界区内）。sink 各自
// 负责把自己与设备相关的状态清干净——json 格式只丢状态快照，Sparkplug 要
// 补一条 DDEATH 并忘掉点位。sink 为 nil（还没开出北向通道）时无需通知。
func (r *Runtime) notifyRemoved(id string) {
	if s := r.mqttSink; s != nil {
		s.onRemove(id)
	}
}

// Start 启动调度、Outbox 投递与看门狗。
func (r *Runtime) Start(ctx context.Context) {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.sched.Start(r.ctx)
	if r.alarms != nil {
		r.alarms.Start(r.ctx)
		r.wg.Add(1)
		go r.alarmSampleLoop(r.ctx)
		r.wg.Add(1)
		go r.alarmEventLoop(r.ctx)
		// 先播种再开采集：这样第一条状态迁移（可能在 200ms 内就发生）
		// 不会在 sink 还没拿到初始快照时把它覆盖掉。
		r.seedAlarmSink()
	}
	if r.hist != nil {
		r.wg.Add(1)
		go r.historyLoop(r.ctx)
		r.refreshHistoryMetrics()
	}
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
	// 先关北向 OPC UA：它接受外部写入，必须在调度与设备开始收缩之前停止，
	// 否则 SCADA 会看到「连接还在、写进去没反应」。
	r.closeOPCUA()
	if r.cancel != nil {
		r.cancel()
	}
	_ = r.sched.Close(timeout / 2)
	if r.alarms != nil {
		r.alarms.Stop()
	}

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout / 2):
		r.log.Warn("runtime_stop_timeout", "stage", "workers")
	}

	r.closeSlaves()

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
	if r.hist != nil {
		// 采样循环已经随 ctx 退出；这里再刷一次并关闭，保证缓冲区里的
		// 最后几条记录落盘、所有段文件句柄释放。
		if err := r.hist.Close(); err != nil && first == nil {
			first = err
		}
	}
	if r.audit != nil {
		if err := r.audit.Close(); err != nil && first == nil {
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
			// 灰度观察窗口结束后的健康判定：没人来推进时也要能自动回滚。
			r.checkStagedHealth(ctx, now)
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
func (r *Runtime) ApplyConfig(ctx context.Context, raw json.RawMessage, source string) (config.ApplyResult, error) {
	filled, err := r.fillRedacted(raw)
	if err != nil {
		apiErr, _ := err.(*httpapi.APIError)
		code, msg := "config.invalid", err.Error()
		if apiErr != nil {
			code, msg = apiErr.Code, apiErr.Message
		}
		r.auditConfigChange(ctx, "config.apply", "rejected", source, "error", code, msg)
		return config.ApplyResult{}, err
	}
	var f config.File
	if err := json.Unmarshal(filled, &f); err != nil {
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
		apiErr := httpapi.Errf(http.StatusUnprocessableEntity, "config.apply_failed", "%v", err)
		r.auditConfigChange(ctx, "config.apply", "rejected", source, "error", apiErr.Code, apiErr.Message)
		return config.ApplyResult{}, apiErr
	}
	if !res.Applied {
		// 内容哈希相同：没改任何东西。仍记一条——「谁在什么时候试图改配置」
		// 本身就是审计要回答的问题，即使最终没有生效。
		r.auditConfigChange(ctx, "config.apply", res.Version, source, "ok", "config.unchanged", "content hash unchanged")
		return res, nil
	}
	res.Restarted = r.reload(before)
	r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "ok"})
	r.auditConfigChange(ctx, "config.apply", res.Version, source, "ok", "", "")

	after := r.cfg.Current()
	afterNB, _ := json.Marshal(struct {
		N config.Northbound `json:"n"`
		S config.Storage    `json:"s"`
	}{after.Northbound, after.Storage})
	if string(beforeNB) != string(afterNB) {
		r.log.Warn("northbound_config_changed", "detail", "MQTT/Webhook 参数变更需重启进程生效；OPC UA 北向已热重建")
	}
	// 历史库的目录与保留策略在 Open 时就固定了。采样参数可以热改，
	// 但这四项不能——必须明确告诉运维「改了但没生效」，否则他会盯着
	// 一个没变的水位以为配置没生效。
	if historyFingerprint(before.Storage.History) != historyFingerprint(after.Storage.History) {
		r.log.Warn("history_config_changed",
			"detail", "storage.history 的 dir/maxBytes/segmentBytes/maxAge 变更需重启进程生效；minInterval/heartbeat/deadband 已即时生效")
	}
	return res, nil
}

// Rollback 回滚到指定版本并热更新。
func (r *Runtime) Rollback(ctx context.Context, version string) (config.ApplyResult, error) {
	before := r.cfg.Current()
	res, err := r.cfg.Rollback(version)
	if err != nil {
		var apiErr *httpapi.APIError
		if errors.Is(err, config.ErrVersionNotFound) {
			apiErr = httpapi.Errf(http.StatusNotFound, "config.version_not_found", "%v", err)
		} else {
			apiErr = httpapi.Errf(http.StatusUnprocessableEntity, "config.rollback_failed", "%v", err)
		}
		r.auditConfigChange(ctx, "config.rollback", version, "rollback", "error", apiErr.Code, apiErr.Message)
		return config.ApplyResult{}, apiErr
	}
	// 回滚同样重建凭据表：回滚最常见的用途就是「刚才那次变更引入了错误」，
	// 而那次变更可能同时改掉了用户表。
	if err := r.buildAuth(r.cfg.Current()); err != nil {
		// 不回滚整次 Apply（配置已落盘，业务面要继续服务），但凭据必须
		// 换成新表：保留旧表等于「配置说删了、进程还说在」。
		r.log.Error("auth_reload_failed", "error", errText(err))
	}
	res.Restarted = r.reload(before)
	r.auditConfigChange(ctx, "config.rollback", res.Version, "rollback", "ok", "", "restored "+version)
	return res, nil
}

// reload 只重建「指纹变化」的设备，未变化的设备连接与死区状态保持不变。
func (r *Runtime) reload(before *config.File) []string {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	after := r.cfg.Current()
	if err := r.buildAuth(after); err != nil {
		// 不回滚整次 Apply（配置已落盘，业务面要继续服务），但凭据必须
		// 换成新表：保留旧表等于「配置说删了、进程还说在」。
		r.log.Error("auth_reload_failed", "error", errText(err))
	}
	if err := r.openAudit(after); err != nil {
		// 保留旧存储继续写：审计不可用只影响留痕，不该让已经在跑的服务
		// 丢掉整段历史。错误已经打日志，/api/v1/audit/stats 也会显示 enabled=false。
		r.log.Error("audit_reload_failed", "error", errText(err))
	}

	chans := make(map[string]model.Channel, len(after.Channels))
	for _, ch := range after.Channels {
		chans[ch.ID] = ch
	}
	desired := make(map[string]model.Device, len(after.Devices))
	for _, d := range after.Devices {
		desired[d.ID] = d
	}

	r.mu.Lock()

	var restarted []string
	// removed 收集「需要告诉北向 sink 的设备」。实际通知放在释放 r.mu 之后：
	// notifyRemoved 会走网络（Sparkplug 补 DDEATH、json 清 retained 状态），
	// 带 3 秒超时。在锁里做网络 IO 会让一次配置变更把所有 collector 查找
	// 阻塞到 Broker 超时，那是「改个配置，采集停摆三秒」。
	var removed []string
	for id, c := range r.collectors {
		nd, keep := desired[id]
		if !keep {
			r.sched.Remove(id)
			delete(r.collectors, id)
			removed = append(removed, id)
			restarted = append(restarted, id)
			continue
		}
		nch := chans[nd.ChannelID]
		if fingerprint(c.dev, c.ch) == fingerprint(nd, nch) {
			continue
		}
		r.sched.Remove(id)
		delete(r.collectors, id)
		// 设备以同一 ID 重启（改了点位/通道）。它不是「消失」，但点位集合
		// 可能变了：先让 sink 忘掉旧点位（发 DDEATH），下一轮采集就会为它
		// 重新 DBIRTH 一份完整且正确的点位表，而不是沿用删掉的旧点位。
		removed = append(removed, id)
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
	if r.alarms != nil {
		if err := r.alarms.Reload(after.Alarms); err != nil {
			// 配置层已校验过，走到这里说明有校验没覆盖到的路径。保留旧规则
			// 比让引擎处于未知状态安全，但必须留下显式日志。
			r.log.Error("alarm_reload_failed", "error", errText(err))
		}
		// 规则集变化会改变「规则总数」这类 gauge；纯元数据变更（改名）不发
		// 事件，等下一次迁移再刷新就会长期显示旧值。
		r.refreshAlarmMetrics()
	}
	restarted = append(restarted, r.syncSlaves(after)...)
	// 北向 OPC UA 的地址空间在启动时固化，配置变了只能重建。这里做在
	// 设备重建之后：新地址空间的种子值要来自最新的点位表。
	r.syncOPCUA(after, before)
	r.mu.Unlock()

	// 通知放在锁外：它会走网络（Sparkplug 补 DDEATH、json 清 retained
	// 状态），带 3 秒超时。持锁做网络 IO 会让一次配置变更把所有 collector
	// 查找阻塞到 Broker 超时。
	for _, id := range removed {
		r.notifyRemoved(id)
	}

	sort.Strings(restarted)
	if len(restarted) > 0 {
		r.log.Info("config_applied", "version", r.cfg.Version(), "restarted", restarted)
	}
	return restarted
}

func (r *Runtime) collector(id string) (*collector, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.collectors[id]
	return c, ok
}

// onDeviceState 把调度器的设备状态迁移交给北向 sink。
//
// 这里必须按配置核对设备是否还活着：调度器的状态事件是异步派发的，reload
// 删掉设备之后，队列里可能还压着它的事件，会在 onRemove 之后才被派发。放行
// 这样一条事件，就会在北向上重新写出一条 retained `$state`——而 onRemove
// 已经清掉了待补发记录，这条幽灵再也没有人负责清理，看板上会永远挂着一台
// 不存在的设备。
//
// 核对放在 runtime 而不是 sink：sink 分辨不了「删除」与「同 ID 重配后重新
// 出现」，只有配置是唯一事实。设备仍在配置里才放行，sink 就不必自己维护
// 墓碑，也就不会把重新配置的设备永久静音。
func (r *Runtime) onDeviceState(ev scheduler.StateEvent) {
	s := r.mqttSink
	if s == nil {
		return
	}
	if _, _, ok := r.deviceConfig(ev.DeviceID); !ok {
		return
	}
	s.onState(ev)
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
		Alarms:        r.AlarmCounts(),
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
	if st, ok := r.HistoryStats(); ok {
		h.History = httpapi.HistoryHealth{
			Enabled: true, Records: st.Appended, Bytes: st.Bytes, MaxBytes: st.MaxBytes,
			Segments: st.Segments, Evicted: st.Evicted, Errors: st.WriteErrors,
		}
		if !st.Oldest.IsZero() {
			oldest := st.Oldest
			h.History.Oldest = &oldest
		}
		if !st.Newest.IsZero() {
			newest := st.Newest
			h.History.Newest = &newest
		}
	}
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
	// 状态与原因必须同时出现。
	//
	// 设备从 collecting 掉到 faulted 需要连续 3 次失败（防抖），而点位的坏
	// 质量在第一次失败时就有了。如果只把「设备态」写进 problems，就会出现
	// 一段真实存在的窗口：status=degraded、points.bad=7、problems=[]。
	// 运维看到的是一个「说自己有病但说不出哪里疼」的健康检查——这类响应
	// 最后一定会被接进告警系统，然后变成没人看的一行绿字。
	if h.Status == "degraded" && len(h.Problems) == 0 {
		h.Problems = append(h.Problems, httpapi.Problem{
			State: "degraded",
			Reason: fmt.Sprintf("point quality degraded: bad=%d stale=%d uncertain=%d (of %d)",
				h.Points.Bad, h.Points.Stale, h.Points.Uncertain, h.Points.Total),
		})
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
	// 逐项审计：一次批量写可能一半成功一半被拒绝，只记一条「写入了 3 个点」
	// 无法回答事后最重要的问题——**到底哪个点没写进去**。
	for i, it := range items {
		res := results[i]
		detail := map[string]string{
			"device": device,
			"point":  it.Point,
			"value":  it.Value.String(),
		}
		if old, ok := r.Point(device, it.Point); ok && old.Quality == model.QualityGood {
			detail["oldValue"] = old.Value.String()
		}
		result, code, msg := "ok", "", ""
		if !res.OK {
			result, code, msg = "error", res.Code, res.Message
		}
		r.auditFrom(ctx, "point.write", device+"/"+it.Point, result, code, msg, detail)
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
