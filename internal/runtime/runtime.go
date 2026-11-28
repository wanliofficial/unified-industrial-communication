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
	"uic/internal/clockmon"
	"uic/internal/config"
	"uic/internal/consensus"
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
	"uic/internal/heartbeat"
	"uic/internal/history"
	"uic/internal/instanceid"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
	"uic/internal/outbox"
	"uic/internal/scheduler"
	slavemb "uic/internal/slave/modbus"
	"uic/internal/watchdog"
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

	// StallAfter 是「采集中枢多久没 tick 就判定卡死」的阈值；<=0 用默认 15s。
	// 主要供测试注入较短阈值，生产用默认即可。
	StallAfter time.Duration
	// HeartbeatInterval 是心跳文件的写入间隔；<=0 用默认 5s，<0 表示禁用。
	// 心跳文件写 <DataDir>/heartbeat.json，供不访问 HTTP 的外部 supervisor 判断
	// 「进程活着但采集中枢卡死」。禁用只影响这一个外部信号，不影响 /healthz。
	HeartbeatInterval time.Duration

	// ClockSource 是系统时钟的采样源。为 nil 时用 clockmon.SystemSource()。
	//
	// 它是测试注入缝而不是配置项：时钟跳变检测不需要「用户可调的阈值」——阈值
	// 取 1s 是一个有依据的常量（见 internal/clockmon），配置化只会带来「运维
	// 随手调成 0 从而每分钟报一次跳变」的风险。但「墙钟被拨了 1.5 秒」这种场景
	// 无法靠等待真实系统时间变化来测试，因此必须允许替换采样源。
	ClockSource func() clockmon.Reading

	// Consensus 是测试注入缝：非 nil 时 runtime 直接使用它，不按配置构造 raft。
	//
	// 为什么要这个缝：leader 闸/409/停采/提交式写都可以用一个假 Consensus 确定
	// 性验证（切角色、制造无多数派），不必在每个 runtime 测试里拉起真实集群。
	// 真实集群路径由 consensus 包的集成测试 + runtime 的单节点端到端测试覆盖。
	Consensus consensus.Consensus
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

	// contractVersion 是当前生效的北向契约版本（1 或 2）。publish 在不同
	// goroutine 里读它，reload 在热更新时写它——用原子值避免数据竞争，也让
	// 「改 contractVersion 不需要重启采集」成为可能。初值在 openSinks 种下。
	contractVersion atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	startedAt time.Time
	version   httpapi.VersionInfo

	// staged 是灰度发布的编排状态（见 staged.go）。用独立锁：灰度会调用
	// ApplyConfig（内部走 cfg.Apply 与 reload 的锁），塞进同一把锁会成环。
	staged stagedState

	// 采集中枢活性的看门狗状态。
	//
	// schedStalled 只用于「刚转 stalled / 刚恢复」的边沿去重（指标与日志各记一次），
	// 不是 Health() 的读缓存：Health() 每次都按实时的 LastTick 重新判定，避免
	// 「看门狗还没跑到、健康检查就先看到旧结论」的时序问题。
	schedStalled  atomic.Bool
	heartbeatWarn atomic.Bool // 心跳文件写入是否已告警过（避免反复刷日志）

	// clock 监测墙钟相对单调钟的跳变（docs/03-product/05-northbound-api-spec.md
	// §2.4 的 clock 健康块）。clockRead 是采样源：默认 SystemSource 的闭包，
	// 测试注入一个「叠加了固定偏移的真实源」。两者都由 New 一次性确定，之后不再
	// 改动——采样源不是配置，改了也没有语义。
	clock     *clockmon.Monitor
	clockRead func() clockmon.Reading

	// 跨实例协调（见 cluster.go）。consensus 为 nil 表示 cluster 未启用：所有
	// leader 闸恒为真，行为与引入 raft 之前逐字节一致。
	consensus   consensus.Consensus
	clusterCfg  *config.Cluster
	nodeID      string
	clusterAddr string

	// cluMu 串行化共识 apply 与快照捕获：两者都在 raft 的协程上，但 Snapshot
	// 可能与 Apply 并发（raft 会在继续 apply 的同时物化快照），需要一把锁把
	// 「配置 + 幂等表 + 记录」钉在同一个 apply 位置上。
	cluMu sync.Mutex
	clu   clusterRecords

	roleSinceMu sync.Mutex
	roleSince   time.Time

	// leaderActive/roleKnown 驱动「只在角色边沿改设备状态」的判定。设备被
	// SetEnabled(true) 会把 nextDue 拉到当下，每个 tick 都调会把采集周期变成
	// 500ms，所以必须做边沿检测。
	leaderActive atomic.Bool
	roleKnown    atomic.Bool
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

	// 时钟监测必须在 openSinks 之前构造：sink 的启动路径里可能已经读过一次
	// Health（例如 MQTT 的 retained 状态），那时 clock 不能是 nil。
	clockRead := opt.ClockSource
	if clockRead == nil {
		clockRead = clockmon.SystemSource()
	}
	// 阈值不暴露成配置：1s 的依据写在 clockmon.DefaultStepThreshold。暴露它等于
	// 邀请运维把它调小，然后开始忽略 uic_clock_jumps_total——那比不报更糟。
	r.clock = clockmon.New(clockmon.DefaultStepThreshold, r.onClockJump)
	r.clockRead = clockRead
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
		r.openConsensus,
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
	// 先停 raft：它可能在 apply 配置（调用 reload），必须在其他子系统拆掉
	// 之前停止，否则 apply 会对着已关闭的调度器/审计存储操作。
	r.closeConsensus()
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
	d("uic_scheduler_stalled_total", "counter", "采集调度卡死次数（进程活着但 tick 停摆）")
	d("uic_scheduler_tick_age_seconds", "gauge", "采集调度最近一次 tick 距今的秒数")
	d("uic_driver_connect_total", "counter", "驱动建链次数与结果")
	d("uic_write_total", "counter", "写入次数与结果")
	d("uic_outbox_pending", "gauge", "Outbox 待投递条数")
	d("uic_outbox_pending_bytes", "gauge", "Outbox 待投递字节数")
	d("uic_outbox_dropped_total", "counter", "Outbox 丢弃条数")
	d("uic_clock_jumps_total", "counter", "墙钟相对单调钟的跳变次数（按方向：forward/backward）")
	d("uic_clock_offset_seconds", "gauge", "墙钟相对单调钟的累计偏移（秒）")
	d("uic_clock_last_jump_seconds", "gauge", "最近一次时钟跳变的幅度（秒，有符号）")
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
	d("uic_leader_role", "gauge", "本副本在 raft 集群里的角色（1 表示当前角色）")
	d("uic_raft_term", "gauge", "raft 当前任期")
	d("uic_raft_commit_index", "gauge", "raft 已提交日志索引")
	d("uic_raft_proposals_total", "counter", "控制面命令提交结果（committed/rejected/timeout）")
	d("uic_raft_membership_changes_total", "counter", "raft 成员变更次数（按 op）")
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
	if r.consensus != nil {
		r.wg.Add(1)
		go r.clusterLoop(r.ctx)
	}
	r.wg.Add(1)
	go r.heartbeatLoop(r.ctx)
	// 播种一次时钟：不播种的话，启动后头一分钟 health 的 clock.verdict 恒为
	// unknown——watchdog 的 1s tick 也要等一秒才第一次跑。这跟 scheduler 活性
	// 看门狗「先种一次 LastTick 再开看门狗」是同一个道理：活性信号不能有空窗，
	// 否则启动瞬间的 health 全是 unknown，接进告警系统会先产生一波假阳性。
	r.sampleClock()
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

	// 采集/写协程已退出，再停 raft：apply 不会再调用 reload/审计。
	r.closeConsensus()

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
			// 采集中枢活性：检测「进程活着但调度器 tick 停摆」。
			r.checkSchedulerLiveness(now)
			// 系统时钟跳变：墙钟相对单调钟的一次性步进（见 internal/clockmon）。
			r.sampleClock()
		}
	}
}

// sampleClock 采一次系统时钟，喂给 clockmon.Monitor。
//
// 挂在 watchdog 已有的 1s tick 上，不另起协程：时钟监测本来就是「每秒看一眼墙
// 钟有没有动」，而 watchdog 的 tick 已经保证每秒跑一次（它的语义就是靠这个
// tick 维持的）。另起一个每秒 ticker 只会得到两个需要互相同步的定时器，没有
// 任何额外信息。
//
// 跳变告警与计数（日志 + 指标）在 Monitor 内部按「锁外回调」执行，所以这里
// 不用加锁、也不用再记一次数。若未来需要在这里加处理，注意它跑在 watchdog 的
// 采样协程上——必须足够快，不能阻塞 tick。
func (r *Runtime) sampleClock() {
	if r.clock == nil {
		return
	}
	_, _ = r.clock.Sample(r.clockRead())
}

// heartbeatLoop 独立负责心跳文件，不共用 watchdog 的 1s ticker。
//
// 两条信号必须解耦：
//   - 看门狗 tick 负责「判断是否卡死」，固定 1s 就够；
//   - 心跳文件负责「对外声明我还活着」，间隔是用户可配的。
//
// 早先把两者放在同一个 1s ticker 上，结果是 HeartbeatInterval 小于 1s 时被
// 静默吞掉（永远不会按配置的频率写）——这是「配置被静默忽略」类缺陷，必须
// 让心跳自己按配置的节奏走。
func (r *Runtime) heartbeatLoop(ctx context.Context) {
	defer r.wg.Done()
	interval := r.opt.HeartbeatInterval
	if interval < 0 {
		return // 显式禁用：不生成心跳文件
	}
	if interval == 0 {
		interval = heartbeatDefault
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// 启动即写一次：外部 supervisor 在进程刚起来时就能看到第一份心跳，不必
	// 等一个完整间隔。
	r.writeHeartbeat(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.writeHeartbeat(now)
		}
	}
}

// checkSchedulerLiveness 用 watchdog 的纯判定逻辑检查调度器 tick 活性，并在状态
// 迁移时记指标、写日志、更新供 Health() 读取的原子标记。
//
// 为什么单独做而不并进 Health()：Health() 是被动查询，只在有人调用时才算；一个
// 「没人看的健康检查」无法主动发现卡死。看门狗是主动的，每秒都会判定一次，因此
// 转为 stalled 的那一刻就能立刻记指标/日志，而不是等到下一次有人 curl。
func (r *Runtime) checkSchedulerLiveness(now time.Time) {
	last, ok := r.sched.LastTick()
	st := watchdog.Evaluate(now, last, ok, watchdog.Options{StallAfter: r.stallAfter()})
	was := r.schedStalled.Load()
	stalled := st.Verdict == watchdog.VerdictStalled
	switch {
	case stalled && !was:
		r.schedStalled.Store(true)
		r.metrics.Inc("uic_scheduler_stalled_total", nil)
		r.log.Error("scheduler_stalled", "age", st.Age.String(), "stallAfter", st.StallAfter.String(),
			"detail", "采集调度 tick 停摆：进程仍在响应 HTTP，但设备已不再被调度")
	case !stalled && was:
		r.schedStalled.Store(false)
		r.log.Warn("scheduler_recovered", "age", st.Age.String())
	}
	// 用指标暴露「多久没 tick」，便于做趋势告警（而不只是二值卡死）。
	r.metrics.Set("uic_scheduler_tick_age_seconds", st.Age.Seconds(), nil)
}

// heartbeatDefault 是心跳文件的默认写入间隔。导出给 cmd/uic 引用：CLI 校验
// 上下界必须与运行时默认值同源，否则两处各写一份 5s 迟早漂移。
const heartbeatDefault = 5 * time.Second

// 心跳默认写入间隔。与 -heartbeat-interval 的 0 语义对应。
const DefaultHeartbeatInterval = heartbeatDefault

// DefaultStallAfter 是「多久没 tick 判卡死」的默认阈值。导出给 cmd/uic 校验
// 文案引用，避免 CLI 文档与运行时各写一份 15s 而漂移。
const DefaultStallAfter = watchdog.DefaultStallAfter

// 看门狗阈值与心跳间隔的下限。CLI 校验引用同一处，保证「命令行文档说的」与
// 「运行时实际用的」是同一份事实。
const (
	// MinStallAfter 是允许的最小卡死阈值：调度器默认每 50ms tick 一次，低于
	// 若干 tick 周期只会让正常的调度抖动被误报成卡死。
	MinStallAfter = 500 * time.Millisecond
	// MinHeartbeatEvery 是允许的最小心跳间隔：心跳是写盘操作，过密无收益。
	MinHeartbeatEvery = 100 * time.Millisecond
)

// stallAfter 返回生效的卡死阈值。
func (r *Runtime) stallAfter() time.Duration {
	if r.opt.StallAfter > 0 {
		return r.opt.StallAfter
	}
	return watchdog.DefaultStallAfter
}

// writeHeartbeat 写一次心跳文件。禁用与间隔判断由 heartbeatLoop 负责——这里
// 只做「组装并落盘」，保持单一职责。
func (r *Runtime) writeHeartbeat(now time.Time) {
	snap := r.heartbeatSnapshot(now)
	if err := heartbeat.Write(r.dataDir, snap); err != nil {
		// 只在首次失败时告警，避免高频刷屏；恢复后重置标记。
		if r.heartbeatWarn.CompareAndSwap(false, true) {
			r.log.Warn("heartbeat_write_failed", "err", err.Error(), "path", heartbeat.Path(r.dataDir))
		}
	} else {
		r.heartbeatWarn.Store(false)
	}
}

// heartbeatSnapshot 组装心跳内容。与 Health() 同源：都读 scheduler 的实时状态。
func (r *Runtime) heartbeatSnapshot(now time.Time) heartbeat.Snapshot {
	last, ok := r.sched.LastTick()
	st := watchdog.Evaluate(now, last, ok, watchdog.Options{StallAfter: r.stallAfter()})
	var devs heartbeat.Devices
	for _, s := range r.sched.Status() {
		devs.Total++
		switch s.RuntimeState {
		case scheduler.StateCollecting:
			devs.Collecting++
		case scheduler.StateFaulted:
			devs.Faulted++
		case scheduler.StateDisabled:
			devs.Disabled++
		case scheduler.StateDegraded:
			devs.Degraded++
		}
	}
	return heartbeat.Snapshot{
		TS:            now.Unix(),
		TSMs:          now.UnixMilli(),
		TSAt:          now.UTC().Format(time.RFC3339Nano),
		PID:           os.Getpid(),
		InstanceID:    r.instanceID,
		Version:       r.version.Version,
		UptimeSeconds: int64(now.Sub(r.startedAt).Seconds()),
		Scheduler: heartbeat.Scheduler{
			Verdict:      string(st.Verdict),
			AgeMs:        st.Age.Milliseconds(),
			StallAfterMs: st.StallAfter.Milliseconds(),
		},
		Devices: devs,
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
//
// 提交式语义（cluster 启用时）：leader 闸在最外层，然后本地做完整校验（填充
// 脱敏占位符、解析、Validate、算 canonical 哈希），只有校验通过才把它作为 raft
// 命令提交。这样一份笔误的配置得到的是 422 而不是「apply 失败→状态机判分歧→
// 本副本停写」：校验是确定性的、每个副本都会做同样的判断，因此不需要靠共识
// 去发现它。
//
// 闸门放在校验之前是为了让契约单一：非 leader 对任何写请求都回 409 + leader
// 地址，调用方只需处理一种「换个节点重试」的情形，不必分辨这个 422 是不是
// 因为自己问错了副本。
func (r *Runtime) ApplyConfig(ctx context.Context, raw json.RawMessage, source string) (config.ApplyResult, error) {
	if err := r.requireLeader(); err != nil {
		r.auditConfigChange(ctx, "config.apply", "rejected", source, "error", errorCode(err), errorMessage(err))
		return config.ApplyResult{}, err
	}
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
	if err := f.Validate(); err != nil {
		apiErr := httpapi.Errf(http.StatusUnprocessableEntity, "config.apply_failed", "%v", err)
		r.auditConfigChange(ctx, "config.apply", "rejected", source, "error", apiErr.Code, apiErr.Message)
		return config.ApplyResult{}, apiErr
	}
	canonical, err := f.Canonical()
	if err != nil {
		return config.ApplyResult{}, httpapi.Errf(http.StatusUnprocessableEntity, "config.apply_failed", "%v", err)
	}

	var res config.ApplyResult
	if r.consensus == nil {
		res, err = r.applyConfigCanonical(ctx, canonical, source)
		if err != nil {
			apiErr := httpapi.Errf(http.StatusUnprocessableEntity, "config.apply_failed", "%v", err)
			r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "error"})
			r.auditConfigChange(ctx, "config.apply", "rejected", source, "error", apiErr.Code, apiErr.Message)
			return config.ApplyResult{}, apiErr
		}
	} else {
		out, perr := r.propose(ctx, consensus.KindApplyConfig, commandPayload{
			Source: source, Config: canonical, At: time.Now().UTC(),
		})
		if perr != nil {
			apiErr := r.clusterWriteError(perr)
			r.auditConfigChange(ctx, "config.apply", "rejected", source, "error", errorCode(apiErr), errorMessage(apiErr))
			return config.ApplyResult{}, apiErr
		}
		got, ok := out.(config.ApplyResult)
		if !ok {
			return config.ApplyResult{}, httpapi.Errf(http.StatusInternalServerError, "internal.error",
				"unexpected apply result type %T", out)
		}
		res = got
	}

	if !res.Applied {
		// 内容哈希相同：没改任何东西。仍记一条——「谁在什么时候试图改配置」
		// 本身就是审计要回答的问题，即使最终没有生效。
		r.auditConfigChange(ctx, "config.apply", res.Version, source, "ok", "config.unchanged", "content hash unchanged")
		return res, nil
	}
	r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "ok"})
	r.auditConfigChange(ctx, "config.apply", res.Version, source, "ok", "", "")
	return res, nil
}

// northboundRestartFingerprint 计算「MQTT/Webhook 参数变更需重启进程生效」
// 这条警告所依据的指纹。它在内容哈希之外再做两处归一化：
//
//   - 排除 ContractVersion：契约版本是运行期热切的（reload 里原子 Store），
//     改它不该谎报「需重启」。两条 lane 在 openSinks 时就已建好。
//   - 归一化 TopicV2 的派生默认值（Topic + "/v2"）：cv 从 1 切到 2 时
//     config.Normalize 才把这个字段填出来——这是切契约的副产物，不是运维
//     改了主题模板。而显式配置的 TopicV2（!= 派生值）仍需重启才生效，故保留。
func northboundRestartFingerprint(nb config.Northbound, st config.Storage) []byte {
	nb.ContractVersion = 0
	if nb.MQTT.TopicV2 == nb.MQTT.Topic+"/v2" {
		nb.MQTT.TopicV2 = ""
	}
	b, _ := json.Marshal(struct {
		N config.Northbound `json:"n"`
		S config.Storage    `json:"s"`
	}{nb, st})
	return b
}

// Rollback 回滚到指定版本并热更新。
//
// 与 ApplyConfig 同样的提交式语义：先在本地确认目标版本存在（这是确定性的
// 检查，不该靠共识发现，否则「版本号写错」会被报成 503），再提交 raft 命令。
func (r *Runtime) Rollback(ctx context.Context, version string) (config.ApplyResult, error) {
	if err := r.requireLeader(); err != nil {
		r.auditConfigChange(ctx, "config.rollback", version, "rollback", "error", errorCode(err), errorMessage(err))
		return config.ApplyResult{}, err
	}
	// 目标版本必须存在：这是「调用方拿错版本号」，与集群无关。
	if _, err := r.cfg.VersionConfig(version); err != nil {
		apiErr := httpapi.Errf(http.StatusNotFound, "config.version_not_found", "%v", err)
		r.auditConfigChange(ctx, "config.rollback", version, "rollback", "error", apiErr.Code, apiErr.Message)
		return config.ApplyResult{}, apiErr
	}

	var res config.ApplyResult
	var err error
	if r.consensus == nil {
		res, err = r.rollbackCanonical(ctx, version)
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
	} else {
		out, perr := r.propose(ctx, consensus.KindRollbackConfig, commandPayload{
			Version: version, Source: "rollback", At: time.Now().UTC(),
		})
		if perr != nil {
			apiErr := r.clusterWriteError(perr)
			r.auditConfigChange(ctx, "config.rollback", version, "rollback", "error", errorCode(apiErr), errorMessage(apiErr))
			return config.ApplyResult{}, apiErr
		}
		got, ok := out.(config.ApplyResult)
		if !ok {
			return config.ApplyResult{}, httpapi.Errf(http.StatusInternalServerError, "internal.error",
				"unexpected rollback result type %T", out)
		}
		res = got
	}
	r.auditConfigChange(ctx, "config.rollback", res.Version, "rollback", "ok", "", "restored "+version)
	return res, nil
}

// reload 只重建「指纹变化」的设备，未变化的设备连接与死区状态保持不变。
func (r *Runtime) reload(before *config.File) []string {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	after := r.cfg.Current()
	// 契约版本热切：改 contractVersion 不需要重启进程，也不需要重建 sink /
	// 协程——两条 lane 早已注册，这里只翻转 publish 是否往 v2 lane 入队。
	// 放在最前：即便后面的设备重建有失败，契约版本仍是配置里最新的意图。
	prevContract := r.contractVersion.Swap(int64(after.Northbound.EffectiveContractVersion()))
	if cur := r.contractVersion.Load(); prevContract != cur {
		r.log.Info("northbound_contract_version_changed", "from", prevContract, "to", cur,
			"detail", "v1/v2 双发目标已热切，无需重启采集")
	}
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
func (r *Runtime) Health() httpapi.Health { return r.healthAt(time.Now()) }

// healthAt 是 Health 的可注入时钟形式：看门狗类测试需要一个「未来时刻」来
// 观察 stalled 判定，而不必真的等 15 秒或冻结真实协程。
func (r *Runtime) healthAt(now time.Time) httpapi.Health {
	sts := r.sched.Status()
	h := httpapi.Health{
		UptimeSeconds: int64(time.Since(r.startedAt).Seconds()),
		Version:       r.version.Version,
		ConfigVersion: r.cfg.Version(),
		Alarms:        r.AlarmCounts(),
		Leader:        r.Leader(),
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
	// 采集中枢活性：即使设备全都在 collecting，若 tick 停摆，所有设备都会
	// 在「下一个周期」不再更新——但它们的 runtimeState 仍是 collecting（没有
	// 新的失败可触发迁移）。因此必须单独判定 tick 活性，否则「全都正常」的假象
	// 会一直挂着。
	{
		last, ok := r.sched.LastTick()
		st := watchdog.Evaluate(now, last, ok, watchdog.Options{StallAfter: r.stallAfter()})
		h.Scheduler = httpapi.SchedulerHealth{
			Verdict:      string(st.Verdict),
			AgeMs:        st.Age.Milliseconds(),
			StallAfterMs: st.StallAfter.Milliseconds(),
		}
		if st.Verdict == watchdog.VerdictStalled {
			h.Problems = append(h.Problems, httpapi.Problem{
				State:  "stalled",
				Reason: st.Detail,
			})
		}
	}
	// 时钟监测块（见 clockmon 包注释）。它不参与 status 判定：时钟被拨过是事实，
	// 不是故障——一台网关完全可以在时钟跳变 8 小时后照常采集、照常投递。判它坏的
	// 是接在下游的告警，不是 /health 的 status。这也是把跳变从 Problems 里排除
	// 的原因：把事实写进故障列表，运维迟早学会忽略那一行。
	//
	// 不管有没有采到样，verdict 与 stepThresholdMs 都填上：还没采样时 verdict 是
	// "unknown"（契约里的合法枚举），而不是空字符串——空字符串不是四个合法取值之
	// 一，会让按枚举校验的消费方直接判坏。age 用 SampledAt 与 health 自己的 now
	// 参数算，而不是 time.Since：healthAt 是 Health 的可注入时钟形式，看门狗类测试
	// 需要一个「未来时刻」来观察判定，而不必真的等一秒。
	{
		s := r.clock.Snapshot()
		h.Clock = httpapi.ClockHealth{
			Verdict:       string(s.Verdict()), // Samples==0 时即 "unknown"
			OffsetSeconds: s.Offset.Seconds(),
			Jumps: httpapi.ClockJumpCounts{
				Forward:  s.Forward,
				Backward: s.Backward,
			},
			StepThresholdMs: s.Threshold.Milliseconds(),
		}
		if s.LastJump != nil {
			lj := *s.LastJump
			h.Clock.LastJump = &httpapi.ClockJump{
				At:        lj.At.UTC().Format(time.RFC3339),
				Seconds:   lj.Seconds(),
				Direction: lj.Direction(),
			}
		}
		// AgeMs 回答「采样协程是不是还活着」，不是「时间准不准」：采样挂在
		// watchdog 的 1s tick 上，进程还在但那条 tick 停摆时，这个值会一直涨，
		// verdict 却冻在最后一次采样上。健康检查可能在采样之后、下一次 tick 之前
		// 被调用，用 now 减一个未来时刻的采样会得到负数——那没有意义，夹到 0。
		// 尚未采样时 SampledAt 是零值，now.Sub 会是一个天文数字，因此必须跳过。
		if s.Samples > 0 {
			if age := now.Sub(s.SampledAt); age > 0 {
				h.Clock.AgeMs = age.Milliseconds()
			}
		}
	}
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
	case h.Scheduler.Verdict == string(watchdog.VerdictStalled):
		// 采集中枢卡死是最高级别的故障：设备态可能仍显示 collecting，
		// 但实际已不再被调度。放在最前面，避免被 devices/points 的「看似正常」掩盖。
		h.Status = "stalled"
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

// onClockJump 是时钟跳变被检测到时的副作用：日志一次 + 计数一次。
//
// 它跑在 watchdog 的采样协程上（也就是 Monitor.Sample 的解锁回调路径），所以
// 必须足够快：一条 Warn 是亚毫秒级的，不构成风险；它不会也不该阻塞去查 NTP。
// 跳变本身由外部 NTP 客户端去处理，runtime 的职责只是「让它可见」。
//
// 计数器按 dir 打标签而不是只用一个总计数器：向前跳变（时钟被往前拨，样本时
// 戳会倒退/重复）与向后跳变（TTL 提前到期、数据被提前丢弃）的后果是反的，运维
// 要能一眼看出是哪一类。标签值来自 clockmon.Direction() 的固定两个取值，基数
// 有界，不会造成指标标签爆炸。
func (r *Runtime) onClockJump(j clockmon.Jump) {
	r.log.Warn("clock_jump",
		"direction", j.Direction(), "seconds", j.Seconds(), "at", j.At.UTC())
	r.metrics.Inc("uic_clock_jumps_total", obs.Labels{"dir": j.Direction()})
	//「最近一次跳变幅度」是状态：用 gauge 存有符号值，便于做趋势与「最后一次跳
	// 了多久」的直接查询；计数器只回答「跳了几次」，回答不了「跳了多大」。
	r.metrics.Set("uic_clock_last_jump_seconds", j.Seconds(), nil)
}

// refreshClockGauges 把时钟 gauge 刷成当前快照的值。
//
// 为什么在抓取时刷：offset 与 last-jump 都是「状态」而不是「事件」，事件有
// counter，状态有 gauge。但 runtime 没有独立协程去维护这两个 gauge（时钟已经
// 由 watchdog 的 tick 采样了，再起一个协程纯属浪费），所以在唯一会读它们的
// 时刻——Prometheus 抓取——顺手算一次。这是幂等的：同一次抓取内写两次同值，
// 结果一样；跳过它，gauge 就会永远停在进程启动时的 0。
func (r *Runtime) refreshClockGauges() {
	if r.clock == nil {
		return
	}
	s := r.clock.Snapshot()
	if s.Samples == 0 {
		return
	}
	r.metrics.Set("uic_clock_offset_seconds", s.Offset.Seconds(), nil)
}

// Metrics 返回 Prometheus 文本。
func (r *Runtime) Metrics() string {
	r.refreshClockGauges()
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
//
// 提交式语义（cluster 启用时）：设备 I/O 只在 leader 执行一次（副本不会对着
// 自己的设备再写一遍），执行结果随 raft 日志复制，供故障转移后按幂等键重放。
// 若本地写完但无法提交，返回 504 并明确「可能已在本地执行」——静默当成功是
// 缺陷：客户端会以为写没发生，而现场设备已经动了。
func (r *Runtime) Write(ctx context.Context, device string, items []httpapi.WriteItem) ([]httpapi.WriteResult, error) {
	if err := r.requireLeader(); err != nil {
		return nil, err
	}
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
		if r.consensus == nil {
			r.idemPut(device+"\x00"+key, results)
		}
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
	// 最后提交写结果：审计先落，保证「本地确实执行过」这件事有留痕，即使
	// 提交失败。幂等键缺失时不提交——没有可重放的结论，也没有必要让一次
	// 不可去重的写进入共识日志。
	if key := httpapi.IdempotencyKey(ctx); key != "" && r.consensus != nil {
		if _, err := r.propose(ctx, consensus.KindWritePoints, commandPayload{
			Device: device, IdemKey: key, Items: items, Results: results, At: time.Now().UTC(),
		}); err != nil {
			return results, r.clusterWriteError(err)
		}
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
	r.idemPutAt(key, res, time.Now())
}

// idemPutAt 是幂等记录的「带时刻」写入：共识 apply 与快照恢复用 leader 决定
// 该命令的时刻，而不是本副本 apply 的时刻——否则同一条日志在不同副本上会
// 得到不同的过期结论（副本可能晚几秒才 apply）。
func (r *Runtime) idemPutAt(key string, res []httpapi.WriteResult, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.idem) > 512 {
		now := time.Now()
		for k, e := range r.idem {
			if now.Sub(e.at) > 5*time.Minute {
				delete(r.idem, k)
			}
		}
	}
	cp := make([]httpapi.WriteResult, len(res))
	copy(cp, res)
	r.idem[key] = idemEntry{at: at, results: cp}
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

// errorCode/errorMessage 从错误里取审计用的稳定错误码与消息。
//
// 审计要记的是对外契约里的 code（machine-readable、不随语言变化），而不是
// err.Error() 的拼接文本——后者会随包装层数变化，事后按 code 检索就失效了。
func errorCode(err error) string {
	var ae *httpapi.APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return "internal.error"
}

func errorMessage(err error) string {
	var ae *httpapi.APIError
	if errors.As(err, &ae) {
		return ae.Message
	}
	return errText(err)
}

func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
