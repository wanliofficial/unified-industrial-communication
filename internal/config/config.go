// Package config 负责配置的加载、校验、规范化、版本化与回滚。
//
// 核心约定（与 docs/03-product/03-domain-model.md §10 一致）：
//
//	配置即制品：每次应用都产生一个内容哈希版本与一份快照；
//	写入原子：临时文件 + fsync + rename，掉电不产生半个文件；
//	失败可退：应用前备份当前版本，失败自动恢复；回滚本身也产生新版本；
//	校验前置：非法配置在加载/应用时被拒绝，绝不进入运行时。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"uic/internal/alarm"
	"uic/internal/calc"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/mqtt"
)

// SchemaVersion 是当前支持的配置格式版本。
const SchemaVersion = 1

// ErrVersionNotFound 表示请求的版本号在快照目录中不存在。
// 上层据此返回 404 而不是 422：这是「调用方给错了版本号」，不是配置本身有问题。
var ErrVersionNotFound = errors.New("config: version not found")

// Server 是进程级配置。
type Server struct {
	Listen   string `json:"listen,omitempty"`
	Token    string `json:"token,omitempty"`
	BasePath string `json:"basePath,omitempty"`
	TLS      TLS    `json:"tls,omitempty"`
}

// TLS 是 API 的 TLS 配置。默认关闭；生产要求开启。
type TLS struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	Auto     bool   `json:"auto,omitempty"` // 自动生成自签证书
}

// Northbound 是北向通道配置。
type Northbound struct {
	MQTT    MQTT    `json:"mqtt,omitempty"`
	OPCUA   OPCUA   `json:"opcua,omitempty"`
	Webhook Webhook `json:"webhook,omitempty"`
}

// MQTT 是北向上报的 MQTT 参数。
type MQTT struct {
	Enabled  bool   `json:"enabled"`
	Broker   string `json:"broker,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Topic    string `json:"topic,omitempty"`

	// QoS 用指针表达三态：缺省（nil）取默认 1，显式 0 表示 QoS0。
	// 用 int 无法区分「没写」和「写了 0」，而两者的投递语义差别很大：
	// QoS0 是 at-most-once，QoS1 是 at-least-once。
	QoS *int `json:"qos,omitempty"`

	Retain   bool   `json:"retain,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// EffectiveQoS 返回生效的 QoS（默认 1）。
func (m MQTT) EffectiveQoS() int {
	if m.QoS == nil {
		return 1
	}
	return *m.QoS
}

// OPCUA 是 OPC UA 服务端参数。
type OPCUA struct {
	Enabled   bool   `json:"enabled"`
	Listen    string `json:"listen,omitempty"`
	Policy    string `json:"policy,omitempty"`
	AllowAnon bool   `json:"allowAnonymous,omitempty"`
}

// Webhook 是一个最小的 HTTP 上报通道，用于演示与轻量集成。
type Webhook struct {
	Enabled bool              `json:"enabled"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout model.Duration    `json:"timeout,omitempty"`
	Batch   int               `json:"batch,omitempty"`
}

// Storage 是数据目录与队列上限。
type Storage struct {
	Dir              string         `json:"dir,omitempty"`
	OutboxMaxBytes   int64          `json:"outboxMaxBytes,omitempty"`
	OutboxMaxRecords int64          `json:"outboxMaxRecords,omitempty"`
	OutboxTTL        model.Duration `json:"outboxTtl,omitempty"`
	// Journal 开启本地 NDJSON 归档 sink：每条变化事件追加到
	// <dir>/journal.ndjson。它既是断网期间的兜底留痕，也让 Outbox 在
	// 没有 MQTT/Webhook 的开发环境里可被观察与验证。
	Journal bool `json:"journal,omitempty"`
}

// Logging 是日志配置。
type Logging struct {
	Level  string `json:"level,omitempty"`
	Format string `json:"format,omitempty"`
	File   string `json:"file,omitempty"`
}

// File 是一份完整配置。
type File struct {
	SchemaVersion int             `json:"schemaVersion"`
	Site          string          `json:"site"`
	Server        Server          `json:"server"`
	Channels      []model.Channel `json:"channels"`
	Devices       []model.Device  `json:"devices"`
	// Slaves 把采集到的点位以 Modbus 从站形式暴露给外部主站。它不参与
	// 采集调度，因此与 channels/devices 并列而不是挂在设备下。
	Slaves []model.Slave `json:"slaves,omitempty"`
	// Alarms 是报警规则。与设备并列：一条规则可以跨设备引用点位，
	// 挂在某台设备下会暗示「它只属于那台设备」。
	Alarms     []alarm.Rule `json:"alarms,omitempty"`
	Northbound Northbound   `json:"northbound,omitempty"`
	Storage    Storage      `json:"storage,omitempty"`
	Logging    Logging      `json:"logging,omitempty"`
}

var siteRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Normalize 填充默认值。它必须幂等：多次调用结果一致，否则版本哈希会漂移。
func (f *File) Normalize() {
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if f.Server.Listen == "" {
		f.Server.Listen = ":8080"
	}
	if f.Storage.Dir == "" {
		f.Storage.Dir = "./data"
	}
	if f.Storage.OutboxMaxBytes <= 0 {
		f.Storage.OutboxMaxBytes = 64 << 20
	}
	if f.Storage.OutboxMaxRecords <= 0 {
		f.Storage.OutboxMaxRecords = 200_000
	}
	if f.Storage.OutboxTTL <= 0 {
		f.Storage.OutboxTTL = model.Duration(24 * time.Hour)
	}
	if f.Logging.Level == "" {
		f.Logging.Level = "info"
	}
	if f.Logging.Format == "" {
		f.Logging.Format = "json"
	}
	for i := range f.Alarms {
		f.Alarms[i].Normalize()
	}
	if f.Northbound.MQTT.Topic == "" {
		f.Northbound.MQTT.Topic = "uic/{site}/{device}/{point}/value"
	}
	if f.Northbound.Webhook.Timeout <= 0 {
		f.Northbound.Webhook.Timeout = model.Duration(5 * time.Second)
	}
	if f.Northbound.Webhook.Batch <= 0 {
		f.Northbound.Webhook.Batch = 100
	}
	if f.Northbound.OPCUA.Listen == "" {
		f.Northbound.OPCUA.Listen = ":4840"
	}
	if f.Northbound.OPCUA.Policy == "" {
		f.Northbound.OPCUA.Policy = "Basic256Sha256"
	}
	for i := range f.Channels {
		if f.Channels[i].Concurrency == 0 {
			f.Channels[i].Concurrency = 4
		}
		if f.Channels[i].Timeout == 0 {
			f.Channels[i].Timeout = model.Duration(3 * time.Second)
		}
	}
	chTimeout := model.Duration(3 * time.Second)
	if len(f.Channels) > 0 && f.Channels[0].Timeout > 0 {
		chTimeout = f.Channels[0].Timeout
	}
	for i := range f.Devices {
		if f.Devices[i].Interval == 0 {
			f.Devices[i].Interval = model.Duration(1 * time.Second)
		}
		if f.Devices[i].Timeout == 0 {
			f.Devices[i].Timeout = chTimeout
		}
	}
}

// Validate 校验全部不变量。它会先 Normalize，因此对同一份配置多次校验是稳定的。
func (f *File) Validate() error {
	f.Normalize()
	if f.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion: want %d, got %d", SchemaVersion, f.SchemaVersion)
	}
	if !siteRe.MatchString(f.Site) {
		return fmt.Errorf("site %q: must match %s", f.Site, siteRe.String())
	}
	if len(f.Channels) == 0 {
		return errors.New("channels: at least one channel required")
	}

	channels := make(map[string]model.Channel, len(f.Channels))
	serial := map[string]model.Transport{}
	for _, ch := range f.Channels {
		if err := ch.Validate(); err != nil {
			return err
		}
		if _, dup := channels[ch.ID]; dup {
			return fmt.Errorf("channel %q: duplicate id", ch.ID)
		}
		if _, ok := driver.Get(ch.Protocol); !ok {
			return fmt.Errorf("channel %q: protocol %q not registered", ch.ID, ch.Protocol)
		}
		if ch.Transport.Kind == "serial" {
			if prev, ok := serial[ch.Transport.Port]; ok {
				if prev != ch.Transport {
					return fmt.Errorf("channel %q: serial port %q conflicts with another channel (parameters differ)",
						ch.ID, ch.Transport.Port)
				}
			}
			serial[ch.Transport.Port] = ch.Transport
		}
		channels[ch.ID] = ch
	}

	devices := make(map[string]bool, len(f.Devices))
	for _, d := range f.Devices {
		if err := d.Validate(); err != nil {
			return err
		}
		if devices[d.ID] {
			return fmt.Errorf("device %q: duplicate id", d.ID)
		}
		devices[d.ID] = true
		ch, ok := channels[d.ChannelID]
		if !ok {
			return fmt.Errorf("device %q: channel %q not found", d.ID, d.ChannelID)
		}
		drv, _ := driver.Get(ch.Protocol)
		meta := drv.Meta()
		for _, p := range d.Points {
			if !meta.Accepts(p.Address.Kind) {
				return fmt.Errorf("device %q point %q: protocol %q does not accept %q addressing",
					d.ID, p.ID, ch.Protocol, p.Address.Kind)
			}
		}
	}
	if err := f.validateComputed(); err != nil {
		return err
	}
	if err := f.validateAlarms(); err != nil {
		return err
	}
	if err := f.validateSlaves(); err != nil {
		return err
	}
	return f.validateNorthbound()
}

// validateAlarms 校验报警规则：规则自身自洽、ID 唯一、引用的点位存在且可比较。
//
// 引用不存在的点位和「引用了一个文本点位」都属于静态错误：前者运行期表现
// 为一条永远 unknown 的报警（没人会去看），后者表现相同但更难猜。
func (f *File) validateAlarms() error {
	if len(f.Alarms) == 0 {
		return nil
	}
	index := make(map[pointKey]model.Point)
	for _, d := range f.Devices {
		for _, p := range d.Points {
			index[pointKey{d.ID, p.ID}] = p
		}
	}
	seen := make(map[string]bool, len(f.Alarms))
	for _, r := range f.Alarms {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("alarm %q: %w", r.ID, err)
		}
		if seen[r.ID] {
			return fmt.Errorf("alarm %q: duplicate id", r.ID)
		}
		seen[r.ID] = true
		refs, err := r.References()
		if err != nil {
			return fmt.Errorf("alarm %q: %w", r.ID, err)
		}
		for _, ref := range refs {
			dev, pt, ok := splitRef(ref)
			if !ok {
				return fmt.Errorf("alarm %q: malformed reference %q", r.ID, ref)
			}
			p, exists := index[pointKey{dev, pt}]
			if !exists {
				return fmt.Errorf("alarm %q: reference %q does not exist", r.ID, ref)
			}
			if !p.DataType.IsNumeric() {
				return fmt.Errorf("alarm %q: reference %q is %s, which cannot be compared with a threshold",
					r.ID, ref, p.DataType)
			}
		}
	}
	return nil
}

// validateComputed 校验计算点：表达式能编译、引用存在、类型自洽、没有环。
//
// 这四项全部放在配置期，理由是同一条：**它们都是静态错误**。等到运行时
// 才暴露，代价是一次设备启动失败 + 一条「expression ...: division by zero」
// 的日志，而真正的原因（写错了一个点位名）已经很难回溯。
func (f *File) validateComputed() error {
	index := make(map[pointKey]model.Point)
	for _, d := range f.Devices {
		for _, p := range d.Points {
			index[pointKey{d.ID, p.ID}] = p
		}
	}
	// programs 缓存编译结果，避免同一表达式被编译两次（校验 + 依赖分析）。
	programs := make(map[pointKey]*calc.Program)
	for _, d := range f.Devices {
		for _, p := range d.Points {
			if p.Address.Kind != model.AddressComputed {
				continue
			}
			prog, err := calc.Compile(p.Address.Expr)
			if err != nil {
				return fmt.Errorf("device %q point %q: %w", d.ID, p.ID, err)
			}
			programs[pointKey{d.ID, p.ID}] = prog
			// 顶层类型必须与点位声明一致。布尔点只能接布尔表达式：
			// 允许数值隐式转布尔，就等于允许 `alarm = temperature`，
			// 而它永远为真（非零即真）——一个不会报错也不会工作的报警。
			wantBool := p.DataType == model.TypeBool
			switch {
			case wantBool && prog.Kind() != calc.KindBool:
				return fmt.Errorf("device %q point %q: dataType is bool but the expression is %s "+
					"(compare explicitly, e.g. `x > 0`)", d.ID, p.ID, prog.Kind())
			case !wantBool && prog.Kind() != calc.KindNumber:
				return fmt.Errorf("device %q point %q: dataType is %s but the expression is %s",
					d.ID, p.ID, p.DataType, prog.Kind())
			}
			for _, ref := range prog.Refs() {
				dev, pt, ok := splitRef(ref)
				if !ok {
					return fmt.Errorf("device %q point %q: malformed reference %q", d.ID, p.ID, ref)
				}
				if _, exists := index[pointKey{dev, pt}]; !exists {
					return fmt.Errorf("device %q point %q: reference %q does not exist", d.ID, p.ID, ref)
				}
			}
		}
	}
	return detectComputedCycles(programs)
}

// detectComputedCycles 在计算点之间找环。跨设备也要查：A 依赖 B、B 依赖 A
// 时两边都只能拿到对方上一轮的值，表现为「值永远慢一拍」，而每台设备看起来
// 都完全正常。这种问题在生产现场极难定位，必须在配置期拦掉。
func detectComputedCycles(programs map[pointKey]*calc.Program) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[pointKey]int, len(programs))
	ids := make([]pointKey, 0, len(programs))
	for k := range programs {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].device != ids[j].device {
			return ids[i].device < ids[j].device
		}
		return ids[i].point < ids[j].point
	})
	var stack []pointKey
	var visit func(k pointKey) error
	visit = func(k pointKey) error {
		switch color[k] {
		case black:
			return nil
		case grey:
			path := make([]string, 0, len(stack)+1)
			for _, s := range append(stack, k) {
				path = append(path, s.device+":"+s.point)
			}
			return fmt.Errorf("computed points form a dependency cycle: %s", strings.Join(path, " -> "))
		}
		color[k] = grey
		stack = append(stack, k)
		prog := programs[k]
		// 排序保证报错路径稳定（每次启动都指向同一条环）。
		refs := prog.Refs()
		sort.Strings(refs)
		for _, ref := range refs {
			dev, pt, ok := splitRef(ref)
			if !ok {
				continue
			}
			dep := pointKey{dev, pt}
			if _, isComputed := programs[dep]; !isComputed {
				continue
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[k] = black
		return nil
	}
	for _, k := range ids {
		if err := visit(k); err != nil {
			return err
		}
	}
	return nil
}

// pointKey 是「设备/点位」的复合键。
type pointKey struct{ device, point string }

// splitRef 拆分 `device:point`。与 calc 的引用语法保持一致。
func splitRef(ref string) (string, string, bool) {
	i := strings.IndexByte(ref, ':')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// validateSlaves 校验从站映射。
//
// 需要先有 devices 的索引：映射宽度的唯一来源是点位类型，可写性也来自
// 点位本身。把这两件事分开定义，迟早会出现「从站说可写、点位说只读」
// 的矛盾配置。
func (f *File) validateSlaves() error {
	if len(f.Slaves) == 0 {
		return nil
	}
	points := make(map[string]model.Point)
	for _, d := range f.Devices {
		for _, p := range d.Points {
			points[d.ID+"/"+p.ID] = p
		}
	}
	seen := make(map[string]bool, len(f.Slaves))
	for _, s := range f.Slaves {
		if err := s.Validate(points); err != nil {
			return err
		}
		if seen[s.ID] {
			return fmt.Errorf("slave %q: duplicate id", s.ID)
		}
		seen[s.ID] = true
		// 与北向 HTTP 监听撞端口是最常见的配置错误，值得单独拦一次。
		if listenConflicts(s.Listen, f.Server.Listen) {
			return fmt.Errorf("slave %q: listen %q conflicts with server.listen", s.ID, s.Listen)
		}
	}
	return nil
}

// validateNorthbound 校验北向通道参数。
//
// 这些是静态错误：配了 mqtt.enabled 却没有 broker，永远不可能投递成功。
// 让它在启动前失败，比让设备采集正常、数据默默发不出去要好得多。
func (f *File) validateNorthbound() error {
	m := f.Northbound.MQTT
	if m.Enabled {
		if m.Broker == "" {
			return errors.New("northbound.mqtt.broker required when mqtt.enabled")
		}
		// 只支持 0/1：客户端不实现 QoS2 的两段握手。静默降级会让运维
		// 以为买到了「恰好一次」，因此显式报错。
		if q := m.EffectiveQoS(); q < 0 || q > 1 {
			return fmt.Errorf("northbound.mqtt.qos %d: only 0 and 1 are supported by the publisher", q)
		}
		if !strings.Contains(m.Topic, "{device}") || !strings.Contains(m.Topic, "{point}") {
			return fmt.Errorf("northbound.mqtt.topic %q must contain {device} and {point}", m.Topic)
		}
		// 与运行时共用同一份地址规则：ssl:// 之类的 scheme 必须在这里就失败。
		if _, err := mqtt.NormalizeAddr(m.Broker); err != nil {
			return fmt.Errorf("northbound.mqtt.broker: %w", err)
		}
	}
	return nil
}

// listenConflicts 判断两个监听地址是否必然冲突。
//
// 只做保守判断：端口为 0（随机分配）永远不算冲突；端口相同且主机相同
// 或任一是通配地址时才算。宁可漏报也不要误报——把合法配置拦下来，
// 运维只能去改端口，问题反而被掩盖。
func listenConflicts(a, b string) bool {
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if pa != pb || pa == "0" || pa == "" {
		return false
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" }
	return ha == hb || wild(ha) || wild(hb)
}

// Canonical 输出确定性 JSON，作为版本哈希的输入。
func (f *File) Canonical() ([]byte, error) {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// VersionOf 计算配置版本（sha256 前缀形式）。
func VersionOf(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 版本与状态
// ---------------------------------------------------------------------------

// State 是版本状态文件的内容（对应 docs/03-product/02-architecture.md §7）。
type State struct {
	CurrentVersion string    `json:"currentVersion"`
	DesiredVersion string    `json:"desiredVersion"`
	Source         string    `json:"source"`
	Action         string    `json:"action"`
	Status         string    `json:"status"`
	BackupFile     string    `json:"backupFile,omitempty"`
	RollbackCount  int       `json:"rollbackCount"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// VersionInfo 是版本列表中的一项。
type VersionInfo struct {
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
	Size    int64     `json:"size"`
	Current bool      `json:"current"`
	Source  string    `json:"source,omitempty"`
	Action  string    `json:"action,omitempty"`
}

// ChangeSummary 是应用配置时的影响面统计。
type ChangeSummary struct {
	ChannelsAdded   int `json:"channelsAdded"`
	ChannelsRemoved int `json:"channelsRemoved"`
	DevicesAdded    int `json:"devicesAdded"`
	DevicesModified int `json:"devicesModified"`
	DevicesRemoved  int `json:"devicesRemoved"`
	PointsAdded     int `json:"pointsAdded"`
	PointsModified  int `json:"pointsModified"`
	PointsRemoved   int `json:"pointsRemoved"`
}

// ApplyResult 是应用/回滚的结果。
type ApplyResult struct {
	Version    string        `json:"version"`
	Previous   string        `json:"previous,omitempty"`
	Applied    bool          `json:"applied"`
	Changed    ChangeSummary `json:"changed"`
	Restarted  []string      `json:"restartedDevices"`
	DurationMs int64         `json:"durationMs"`
}

// Diff 是两个版本之间的差异。
type Diff struct {
	From            string   `json:"from"`
	To              string   `json:"to"`
	ChannelsAdded   []string `json:"channelsAdded,omitempty"`
	ChannelsRemoved []string `json:"channelsRemoved,omitempty"`
	DevicesAdded    []string `json:"devicesAdded,omitempty"`
	DevicesRemoved  []string `json:"devicesRemoved,omitempty"`
	DevicesModified []string `json:"devicesModified,omitempty"`
	PointsAdded     int      `json:"pointsAdded"`
	PointsRemoved   int      `json:"pointsRemoved"`
	PointsModified  int      `json:"pointsModified"`
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager 管理一份「当前生效」配置及其历史快照。
type Manager struct {
	path      string
	statePath string
	snapDir   string

	mu        sync.Mutex
	current   *File
	canonical []byte
	version   string
	state     State
}

// Open 加载配置并初始化版本目录。dataDir 为空时使用配置文件所在目录。
func Open(path, dataDir string) (*Manager, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("config: invalid: %w", err)
	}
	canonical, err := f.Canonical()
	if err != nil {
		return nil, err
	}
	if dataDir == "" {
		dataDir = filepath.Dir(path)
	}
	m := &Manager{
		path:      path,
		statePath: filepath.Join(dataDir, "config.state.json"),
		snapDir:   filepath.Join(dataDir, "snapshots"),
		current:   &f,
		canonical: canonical,
		version:   VersionOf(canonical),
	}
	if err := os.MkdirAll(m.snapDir, 0o755); err != nil {
		return nil, fmt.Errorf("config: mkdir snapshots: %w", err)
	}
	if err := m.loadState(); err != nil {
		return nil, err
	}
	// 当前配置也必须有一份快照，否则回滚列表里看不到它。
	if err := m.writeSnapshot(m.version, canonical); err != nil {
		return nil, err
	}
	m.state.CurrentVersion = m.version
	m.state.DesiredVersion = m.version
	if m.state.Status == "" {
		m.state.Status = "applied"
	}
	if m.state.Source == "" {
		m.state.Source = "local"
	}
	m.state.UpdatedAt = time.Now().UTC()
	if err := m.saveState(); err != nil {
		return nil, err
	}
	return m, nil
}

// Path 返回配置文件路径。
func (m *Manager) Path() string { return m.path }

// Current 返回当前生效配置。调用方不得修改返回值。
func (m *Manager) Current() *File {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Version 返回当前版本。
func (m *Manager) Version() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// State 返回版本状态快照。
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Validate 校验一份候选配置（不应用）。
func (m *Manager) Validate(f *File) error {
	return f.Validate()
}

// Apply 校验并应用一份配置。相同内容不重复应用（幂等）。
func (m *Manager) Apply(f *File, source string) (ApplyResult, error) {
	start := time.Now()
	if source == "" {
		source = "api"
	}
	if err := f.Validate(); err != nil {
		return ApplyResult{}, err
	}
	canonical, err := f.Canonical()
	if err != nil {
		return ApplyResult{}, err
	}
	version := VersionOf(canonical)

	m.mu.Lock()
	defer m.mu.Unlock()

	if version == m.version {
		return ApplyResult{
			Version: version, Previous: version, Applied: false,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	prevVersion := m.version
	prevCanonical := m.canonical
	summary := summarize(m.current, f)

	// 1. 备份当前版本（快照本身即备份）。
	if err := m.writeSnapshot(prevVersion, prevCanonical); err != nil {
		return ApplyResult{}, fmt.Errorf("config: backup: %w", err)
	}
	// 2. 原子写入新配置。
	if err := atomicWrite(m.path, canonical); err != nil {
		return ApplyResult{}, fmt.Errorf("config: write: %w", err)
	}
	// 3. 写新版本快照。
	if err := m.writeSnapshot(version, canonical); err != nil {
		_ = atomicWrite(m.path, prevCanonical)
		return ApplyResult{}, fmt.Errorf("config: snapshot: %w", err)
	}
	// 4. 更新状态；失败则恢复原配置。
	next := State{
		CurrentVersion: version,
		DesiredVersion: version,
		Source:         source,
		Action:         "apply",
		Status:         "applied",
		BackupFile:     m.snapshotPath(prevVersion),
		RollbackCount:  m.state.RollbackCount,
		UpdatedAt:      time.Now().UTC(),
	}
	prevState := m.state
	m.state = next
	if err := m.saveState(); err != nil {
		m.state = prevState
		_ = atomicWrite(m.path, prevCanonical)
		return ApplyResult{}, fmt.Errorf("config: state: %w", err)
	}

	m.current = f
	m.canonical = canonical
	m.version = version
	_ = m.prune(50)

	return ApplyResult{
		Version: version, Previous: prevVersion, Applied: true,
		Changed: summary, DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// Rollback 回滚到指定版本。回滚本身产生一个新版本（内容等于目标版本）。
func (m *Manager) Rollback(version string) (ApplyResult, error) {
	f, raw, err := m.loadVersion(version)
	if err != nil {
		return ApplyResult{}, err
	}
	res, err := m.Apply(f, "rollback")
	if err != nil {
		return res, err
	}
	m.mu.Lock()
	m.state.Action = "rollback"
	m.state.RollbackCount++
	m.state.Status = "rolled_back"
	m.state.UpdatedAt = time.Now().UTC()
	if err := m.saveState(); err != nil {
		_ = raw
		m.mu.Unlock()
		return res, err
	}
	m.mu.Unlock()
	return res, nil
}

// Versions 返回历史版本（时间倒序，当前版本标记 Current）。
func (m *Manager) Versions() []VersionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	ents, err := os.ReadDir(m.snapDir)
	if err != nil {
		return nil
	}
	out := make([]VersionInfo, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "v-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ver := "sha256:" + strings.TrimSuffix(strings.TrimPrefix(e.Name(), "v-"), ".json")
		vi := VersionInfo{
			Version: ver, Time: info.ModTime().UTC(), Size: info.Size(),
			Current: ver == m.version,
		}
		if ver == m.version {
			vi.Source = m.state.Source
			vi.Action = m.state.Action
		}
		out = append(out, vi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// VersionConfig 读取指定版本的内容。
func (m *Manager) VersionConfig(version string) (*File, error) {
	f, _, err := m.loadVersion(version)
	return f, err
}

// Diff 计算两个版本的差异；from/to 为空时表示当前版本。
func (m *Manager) Diff(from, to string) (Diff, error) {
	m.mu.Lock()
	cur := m.current
	curVer := m.version
	m.mu.Unlock()

	var a, b *File
	var err error
	if from == "" || from == curVer {
		a = cur
		from = curVer
	} else if a, err = m.VersionConfig(from); err != nil {
		return Diff{}, err
	}
	if to == "" || to == curVer {
		b = cur
		to = curVer
	} else if b, err = m.VersionConfig(to); err != nil {
		return Diff{}, err
	}
	return computeDiff(a, b, from, to), nil
}

func (m *Manager) loadVersion(version string) (*File, []byte, error) {
	if version == "" {
		return nil, nil, ErrVersionNotFound
	}
	p := m.snapshotPath(version)
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 「版本不存在」是客户端错误（404），不是应用失败（422）：
			// 上层据此区分「你给错了版本号」与「配置本身有问题」。
			return nil, nil, fmt.Errorf("%w: %s", ErrVersionNotFound, version)
		}
		return nil, nil, fmt.Errorf("config: version %s: %w", version, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, fmt.Errorf("config: version %s: %w", version, err)
	}
	if VersionOf(raw) != version {
		return nil, nil, fmt.Errorf("config: version %s: snapshot content hash mismatch", version)
	}
	if err := f.Validate(); err != nil {
		return nil, nil, fmt.Errorf("config: version %s invalid: %w", version, err)
	}
	return &f, raw, nil
}

func (m *Manager) snapshotPath(version string) string {
	return filepath.Join(m.snapDir, "v-"+strings.TrimPrefix(version, "sha256:")+".json")
}

func (m *Manager) writeSnapshot(version string, raw []byte) error {
	p := m.snapshotPath(version)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return atomicWrite(p, raw)
}

func (m *Manager) prune(keep int) error {
	if keep <= 0 {
		keep = 50
	}
	ents, err := os.ReadDir(m.snapDir)
	if err != nil {
		return err
	}
	type item struct {
		name string
		mod  time.Time
	}
	items := make([]item, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "v-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{e.Name(), info.ModTime()})
	}
	if len(items) <= keep {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for _, it := range items[keep:] {
		if it.name == "v-"+strings.TrimPrefix(m.version, "sha256:")+".json" {
			continue
		}
		_ = os.Remove(filepath.Join(m.snapDir, it.name))
	}
	return nil
}

func (m *Manager) loadState() error {
	b, err := os.ReadFile(m.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		// 状态文件损坏不是致命错误：重建即可，配置本身仍然可信。
		return nil
	}
	m.state = st
	return nil
}

func (m *Manager) saveState() error {
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.statePath, append(b, '\n'))
}

// ---------------------------------------------------------------------------
// 差异计算
// ---------------------------------------------------------------------------

func summarize(a, b *File) ChangeSummary {
	d := computeDiff(a, b, "", "")
	return ChangeSummary{
		ChannelsAdded: len(d.ChannelsAdded), ChannelsRemoved: len(d.ChannelsRemoved),
		DevicesAdded: len(d.DevicesAdded), DevicesModified: len(d.DevicesModified),
		DevicesRemoved: len(d.DevicesRemoved),
		PointsAdded:    d.PointsAdded, PointsModified: d.PointsModified, PointsRemoved: d.PointsRemoved,
	}
}

func computeDiff(a, b *File, from, to string) Diff {
	d := Diff{From: from, To: to}
	if a == nil || b == nil {
		return d
	}
	ac := map[string]model.Channel{}
	for _, c := range a.Channels {
		ac[c.ID] = c
	}
	bc := map[string]model.Channel{}
	for _, c := range b.Channels {
		bc[c.ID] = c
	}
	for id := range bc {
		if _, ok := ac[id]; !ok {
			d.ChannelsAdded = append(d.ChannelsAdded, id)
		}
	}
	for id := range ac {
		if _, ok := bc[id]; !ok {
			d.ChannelsRemoved = append(d.ChannelsRemoved, id)
		}
	}

	ad := map[string]model.Device{}
	for _, x := range a.Devices {
		ad[x.ID] = x
	}
	bd := map[string]model.Device{}
	for _, x := range b.Devices {
		bd[x.ID] = x
	}
	for id, nb := range bd {
		oa, ok := ad[id]
		if !ok {
			d.DevicesAdded = append(d.DevicesAdded, id)
			d.PointsAdded += len(nb.Points)
			continue
		}
		ja, _ := json.Marshal(oa)
		jb, _ := json.Marshal(nb)
		if string(ja) != string(jb) {
			d.DevicesModified = append(d.DevicesModified, id)
			pa, pb := pointMap(oa), pointMap(nb)
			for pid := range pb {
				if old, ok := pa[pid]; !ok {
					d.PointsAdded++
				} else if old != pb[pid] {
					d.PointsModified++
				}
			}
			for pid := range pa {
				if _, ok := pb[pid]; !ok {
					d.PointsRemoved++
				}
			}
		}
	}
	for id := range ad {
		if _, ok := bd[id]; !ok {
			d.DevicesRemoved = append(d.DevicesRemoved, id)
			d.PointsRemoved += len(ad[id].Points)
		}
	}
	sort.Strings(d.ChannelsAdded)
	sort.Strings(d.ChannelsRemoved)
	sort.Strings(d.DevicesAdded)
	sort.Strings(d.DevicesRemoved)
	sort.Strings(d.DevicesModified)
	return d
}

func pointMap(d model.Device) map[string]string {
	out := make(map[string]string, len(d.Points))
	for _, p := range d.Points {
		b, _ := json.Marshal(p)
		out[p.ID] = string(b)
	}
	return out
}

// ---------------------------------------------------------------------------
// 原子写
// ---------------------------------------------------------------------------

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
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
