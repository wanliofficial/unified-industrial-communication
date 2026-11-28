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
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"uic/internal/alarm"
	"uic/internal/auth"
	"uic/internal/calc"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/mqtt"
	"uic/internal/rbac"
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
	// Users 是多身份凭据表。存在时 server.token 仍被接受（映射为名为
	// "api" 的 admin 凭据），这样老配置升级后不会突然变成「无凭据」。
	Users []User `json:"users,omitempty"`
}

// User 是一条北向凭据。
//
// Token 与 TokenSha256 二选一：
//   - Token 是明文，便于联调时「从配置里读回自己的口令」；
//   - TokenSha256 是它的 SHA-256 十六进制，生产环境应该用这个——配置文件
//     会被备份、贴进工单、进 git，明文 Token 一旦这样流转就等于泄露。
//
// 两者都支持而不是只留哈希，是因为现场最常见的事故是「Token 只在部署时
// 出现过一次，然后丢了」。`uic token` 一次生成两者，写哪份由部署方决定。
type User struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Token       string `json:"token,omitempty"`
	TokenSha256 string `json:"tokenSha256,omitempty"`
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

	// ContractVersion 选择北向数据契约：0（未配置，按 1）/ 1 为现状（只发 v1），
	// 2 为双发过渡（同一份采集数据同时以 v1 与新契约 v2 输出）。它是运行期
	// 可热切的——改这个值不需要重启采集。契约的字节形态与 v2 lane 命名规则
	// 集中在 internal/northbound/contract。
	ContractVersion int `json:"contractVersion,omitempty"`
}

// EffectiveContractVersion 返回生效的契约版本（缺省视为 1）。
//
// 刻意不复用 Normalize 去把字段写成 1：ContractVersion 是 omitempty 的，
// 若 Normalize 无条件把 0 写成 1，会让所有存量配置的规范化内容哈希凭空
// 漂移（Canonical 是整个 File 进哈希）。这里只在读取时把「未配置（<=0）」
// 解释为 1，字段本身保持 0，存量配置的哈希不受影响。
func (n Northbound) EffectiveContractVersion() int {
	if n.ContractVersion < 1 {
		return 1
	}
	return n.ContractVersion
}

// MQTT 北向的载荷格式。
const (
	// MQTTFormatJSON 是平台自有契约：一条消息一个采样，主题由模板渲染。
	// 默认值，也是与 REST/SSE 同构的那一种。
	MQTTFormatJSON = "json"
	// MQTTFormatSparkplugB 走 Sparkplug B 的生命周期与 Protobuf 载荷。
	// 主题由组/节点/设备名拼出来，不再使用 topic 模板。
	MQTTFormatSparkplugB = "sparkplug-b"
)

// MQTT 是北向上报的 MQTT 参数。
type MQTT struct {
	Enabled  bool   `json:"enabled"`
	Broker   string `json:"broker,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Topic    string `json:"topic,omitempty"`

	// TopicV2 是双发过渡期（northbound.contractVersion=2）v2 lane 使用的主题
	// 模板，仅 json 格式生效。缺省（空）在 Normalize 里派生为 Topic + "/v2"，
	// 让两条 lane 天然落到不同主题；显式配置则必须自带 {device}/{point}。
	// 用 omitempty 且只在 cv==2 时填充，避免 cv==1 的存量配置哈希漂移。
	TopicV2 string `json:"topicV2,omitempty"`

	// Format 决定载荷与主题的生成方式：json（默认）或 sparkplug-b。
	// 两者共用 Broker 连接、凭据与 Outbox，但生命周期完全不同——所以
	// 它是显式的二选一，而不是「自动识别」。
	Format string `json:"format,omitempty"`
	// GroupID / NodeID 只在 sparkplug-b 下生效。Group 是现场的命名空间
	// （通常就是站点），Node 是这台网关。缺省分别为 site 与 uic-<site>。
	GroupID string `json:"groupId,omitempty"`
	NodeID  string `json:"nodeId,omitempty"`

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
	// History 是本地历史采样日志（有界、按段淘汰）。它独立于 Journal：
	// Journal 记的是「每一次变化事件」，用于排障；History 记的是「按采样
	// 策略抽取的时间序列」，用于查历史曲线。两者写同一个目录会让保留
	// 策略互相干扰，所以 History 有自己的一套字节上限与保留期。
	History History `json:"history,omitempty"`
	// Audit 是审计日志（有界、按段淘汰、哈希链）。默认开启：一个「审计
	// 可以关掉」的部署，在出事那天一定会发现它一直是关的。
	Audit Audit `json:"audit,omitempty"`
}

// Audit 是审计日志的保留策略。
type Audit struct {
	// Enabled 用指针以区分「没配」与「显式关闭」。没配 = 开启。
	Enabled      *bool          `json:"enabled,omitempty"`
	Dir          string         `json:"dir,omitempty"`
	MaxBytes     int64          `json:"maxBytes,omitempty"`
	MaxAge       model.Duration `json:"maxAge,omitempty"`
	SegmentBytes int64          `json:"segmentBytes,omitempty"`
}

// History 是历史采样日志的策略。
//
// 采样是「事件驱动 + 心跳补点」：值变了（超过死区）就记一条，没变也至少
// 每 heartbeat 记一条。只靠事件会让一条稳定运行的产线在图上变成一条稀疏的
// 虚线；只靠定时轮询又会把「值没变」写成海量重复点。
type History struct {
	Enabled bool `json:"enabled,omitempty"`
	// Dir 默认 <storage.dir>/history。
	Dir string `json:"dir,omitempty"`
	// MaxBytes 是整个历史目录的字节上限（默认 256MB）。超过后按段淘汰
	// 最旧的数据：历史是有界缓存，不是无限账本。
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// MaxAge 是保留期（默认 7 天）。
	MaxAge model.Duration `json:"maxAge,omitempty"`
	// SegmentBytes 是单段大小（默认 16MB）。查询按段并行/顺序扫描，
	// 段太大则淘汰粒度粗，段太小则查询要打开很多文件。
	SegmentBytes int64 `json:"segmentBytes,omitempty"`
	// MinInterval 是同一测点两次落盘的最小间隔（默认 1s）。它是对
	// 「高频抖动」的最后一道闸门：即使设备每 50ms 采一次，也不会写爆磁盘。
	MinInterval model.Duration `json:"minInterval,omitempty"`
	// Heartbeat 是「值没变也补一个点」的周期。未设置时默认 5m；显式写
	// "0s" 表示不补点（趋势图只显示真实变化，长时间稳定时是一段空白）。
	// 用指针是为了把「没配」与「配成 0」区分开——两者语义相反。
	Heartbeat *model.Duration `json:"heartbeat,omitempty"`
	// Deadband 是数值死区（默认 0）：变化小于它不落盘。0 表示任何变化
	// 都记录（因为缓存事件本身就只在变化时触发，重复值不会到这里）。
	Deadband float64 `json:"deadband,omitempty"`
}

// Logging 是日志配置。
type Logging struct {
	Level  string `json:"level,omitempty"`
	Format string `json:"format,omitempty"`
	File   string `json:"file,omitempty"`
}

// Cluster 是跨实例协调（raft）配置。
//
// 默认关闭：单实例部署行为与引入 raft 之前完全一致（无 raft、无额外监听、
// 无额外磁盘）。开启后本进程以 raft 集群成员身份运行：唯一 leader 采集与
// 接受写，其余副本复制控制面状态机（配置/写记录/命令记录/报警确认）。
//
// Peers 描述的是**期望的集群成员表**（含本节点），供两件事使用：
//   - 单节点/多节点判定（空或只含自身 = 单节点，Bootstrap 由 runtime 置 true）；
//   - leader 对账：leader 启动后把表里尚未加入的节点经 raft 原生 AddServer
//     加入。Bootstrap 只写「自身」一条初始配置，绝不在引导时把未启动的节点
//     写进投票集——否则种子节点会因凑不齐多数派而永远选不出 leader。
type Cluster struct {
	Enabled bool `json:"enabled"`
	// NodeID 是本节点在集群里的身份。空时复用 instanceid 的持久值；多节点
	// 部署必须显式填写，否则各节点无法从 Peers 里认出「自己」。
	NodeID string `json:"nodeId,omitempty"`
	// DataDir 是 raft 日志/稳定存储/快照目录。相对路径基于 storage.dir 解析。
	DataDir string `json:"dataDir,omitempty"`
	// Listen 是 raft transport 的监听地址（host:port）。多节点必须可被对端
	// 路由；单节点可用环回地址。
	Listen string `json:"listen,omitempty"`
	// Bootstrap 表示本节点是集群种子（唯一一台写入初始配置的节点）。
	// 单节点部署由 runtime 自动置 true，无需配置；多节点必须恰好一台为 true。
	Bootstrap bool   `json:"bootstrap,omitempty"`
	Peers     []Peer `json:"peers,omitempty"`
}

// Peer 是一个集群成员：身份 + raft transport 地址。
type Peer struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

// ClusterEnabled 报告集群协调是否启用。
func (f *File) ClusterEnabled() bool { return f.Cluster != nil && f.Cluster.Enabled }

// File 是一份完整配置。
type File struct {
	SchemaVersion int    `json:"schemaVersion"`
	Site          string `json:"site"`
	Server        Server `json:"server"`
	// Cluster 用指针 + omitempty：使「未启用」的存量配置规范化输出逐字节不变，
	// 版本哈希不因升级而漂移（struct 的 omitempty 不会省略零值，会让所有存量
	// 配置凭空多出一个 "cluster":{}）。
	Cluster  *Cluster        `json:"cluster,omitempty"`
	Channels []model.Channel `json:"channels"`
	Devices  []model.Device  `json:"devices"`
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
	if f.Storage.History.Enabled {
		h := &f.Storage.History
		if h.Dir == "" {
			h.Dir = filepath.Join(f.Storage.Dir, "history")
		}
		if h.MaxBytes <= 0 {
			h.MaxBytes = 256 << 20
		}
		if h.MaxAge <= 0 {
			h.MaxAge = model.Duration(7 * 24 * time.Hour)
		}
		if h.SegmentBytes <= 0 {
			h.SegmentBytes = 16 << 20
		}
		if h.MinInterval <= 0 {
			h.MinInterval = model.Duration(time.Second)
		}
		if h.Heartbeat == nil {
			d := model.Duration(5 * time.Minute)
			h.Heartbeat = &d
		}
		// 段不能大于总上限：否则第一条记录就触发「淘汰又立刻超限」，
		// 表现为历史目录里永远只有一个段。夹住而不是报错，是因为
		// 「只调小了 maxBytes」是完全合理的配置动作。
		if h.SegmentBytes > h.MaxBytes {
			h.SegmentBytes = h.MaxBytes
		}
		if h.Deadband < 0 {
			h.Deadband = 0
		}
	}
	// 审计默认开启：nil 表示「没配」，不是「关掉」。
	if f.Storage.Audit.Enabled == nil {
		on := true
		f.Storage.Audit.Enabled = &on
	}
	if f.Storage.Audit.Enabled != nil && *f.Storage.Audit.Enabled {
		a := &f.Storage.Audit
		if a.Dir == "" {
			a.Dir = filepath.Join(f.Storage.Dir, "audit")
		}
		if a.MaxBytes <= 0 {
			a.MaxBytes = 64 << 20
		}
		if a.MaxAge <= 0 {
			a.MaxAge = model.Duration(90 * 24 * time.Hour)
		}
		if a.SegmentBytes <= 0 {
			a.SegmentBytes = 8 << 20
		}
		if a.SegmentBytes > a.MaxBytes {
			a.SegmentBytes = a.MaxBytes
		}
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
	mq := &f.Northbound.MQTT
	if mq.Format == "" {
		mq.Format = MQTTFormatJSON
	}
	mq.Format = strings.ToLower(strings.TrimSpace(mq.Format))
	if mq.Format == MQTTFormatSparkplugB {
		// Sparkplug 的主题由组/节点/设备名拼成，没有模板可填；默认值
		// 只在不覆盖用户显式配置的前提下补齐。
		if mq.GroupID == "" {
			mq.GroupID = f.Site
		}
		if mq.NodeID == "" {
			mq.NodeID = "uic-" + f.Site
		}
	} else if mq.Topic == "" {
		mq.Topic = "uic/{site}/{device}/{point}/value"
	}
	// 双发过渡期：cv==2 且 json 格式时，给 v2 lane 一个默认主题模板（在 v1 的
	// Topic 后追加 /v2），让两条 lane 落到不同主题、消费方能各取所需。只在
	// cv==2 且用户没填时补——cv 为 0/1 时不碰这个字段，存量配置的内容哈希保持
	// 不变；cv==2 时这是有意的规范化（该配置本就打算双发，主题默认值稳定）。
	if f.Northbound.EffectiveContractVersion() == 2 && mq.Format != MQTTFormatSparkplugB && mq.TopicV2 == "" {
		mq.TopicV2 = mq.Topic + "/v2"
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
		// 默认必须是本实现真正支持的那一个：拿 Basic256Sha256 当默认值，
		// 等于给每个启用者埋一条「启动就报错」或「配了加密其实没加密」。
		f.Northbound.OPCUA.Policy = "None"
	}
	for i := range f.Channels {
		if f.Channels[i].Concurrency == 0 {
			f.Channels[i].Concurrency = 4
		}
		if f.Channels[i].Timeout == 0 {
			f.Channels[i].Timeout = model.Duration(3 * time.Second)
		}
	}
	// 设备没配 timeout 时继承「它自己所属 channel」的超时——不是 Channels[0]。
	// 多通道、各通道超时不同的配置里，挂在慢通道上的设备若被第一个（可能
	// 很快）通道的超时提前掐断，会表现为「明明配了 30s 却 3s 就超时」，且
	// 没有任何报错。channel 超时已在上面的循环里默认过，这里直接按 id 建表。
	channelTimeout := make(map[string]model.Duration, len(f.Channels))
	for _, ch := range f.Channels {
		channelTimeout[ch.ID] = ch.Timeout
	}
	for i := range f.Devices {
		if f.Devices[i].Interval == 0 {
			f.Devices[i].Interval = model.Duration(1 * time.Second)
		}
		if f.Devices[i].Timeout == 0 {
			f.Devices[i].Timeout = channelTimeout[f.Devices[i].ChannelID]
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
	if err := f.validateServer(); err != nil {
		return err
	}
	if err := f.validateCluster(); err != nil {
		return err
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
	if err := f.validateHistory(); err != nil {
		return err
	}
	if err := f.validateAudit(); err != nil {
		return err
	}
	return f.validateNorthbound()
}

// validateServer 校验凭据表。
//
// 凭据校验放在配置期而不是启动期：一份「用户表写错了」的配置如果能通过
// Apply，运行中的进程就会带着半套身份继续服务，而被删掉的那个用户直到
// 下次重启才发现自己的 Token 还能用。宁可在写入时就拒绝。
func (f *File) validateServer() error {
	if err := f.validateServerTLS(); err != nil {
		return err
	}
	seenUser := map[string]bool{}
	seenToken := map[string]bool{}
	if t := strings.TrimSpace(f.Server.Token); t != "" {
		if err := auth.ValidateToken(t); err != nil {
			return fmt.Errorf("server.token: %w", err)
		}
		seenToken[t] = true
	}
	for i, u := range f.Server.Users {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return fmt.Errorf("server.users[%d]: name is required", i)
		}
		if seenUser[name] {
			return fmt.Errorf("server.users[%d]: duplicate name %q", i, name)
		}
		seenUser[name] = true
		if _, err := rbac.Parse(u.Role); err != nil {
			return fmt.Errorf("server.users[%d] (%s): %w", i, name, err)
		}
		if u.Token != "" && u.TokenSha256 != "" {
			return fmt.Errorf("server.users[%d] (%s): token and tokenSha256 are mutually exclusive", i, name)
		}
		switch {
		case u.Token != "":
			if err := auth.ValidateToken(u.Token); err != nil {
				return fmt.Errorf("server.users[%d] (%s): %w", i, name, err)
			}
			if seenToken[u.Token] {
				return fmt.Errorf("server.users[%d] (%s): duplicate token", i, name)
			}
			seenToken[u.Token] = true
		case u.TokenSha256 != "":
			if len(u.TokenSha256) != 64 {
				return fmt.Errorf("server.users[%d] (%s): tokenSha256 must be 64 hex characters", i, name)
			}
			if _, err := hex.DecodeString(u.TokenSha256); err != nil {
				return fmt.Errorf("server.users[%d] (%s): tokenSha256 is not valid hex: %v", i, name, err)
			}
		default:
			return fmt.Errorf("server.users[%d] (%s): one of token or tokenSha256 is required", i, name)
		}
	}
	return nil
}

// validateServerTLS 在配置期校验 TLS 配置，fail-closed。
//
// 这是等保 2.0 安全通信「传输保密性」与本项目 NFR §8「不支持的组合应在配置期
// 失败、而非运行期静默降级」两条要求的收口：历史上 server.tls.enabled=true 却
// 没配证书时，进程会打一条 WARN 然后**回退明文 HTTP 继续服务**——运维以为开了
// 加密，实际鉴权 Token 等敏感数据可被同网窃听，这是最危险的一种「兼容」。现在
// 配置期直接拒绝这种状态，让它在 `uic -check` 或启动加载配置时就暴露。
//
// 三种合法形态：
//   - enabled=false：明文（明确的、允许的选择，例如纯内网联调）。
//   - enabled=true + certFile/keyFile：用 CA 签发证书（来源可信，生产推荐）。
//   - enabled=true + auto：启动时生成自签证书（传输加密、免证书分发，联调/边缘
//     开箱即用；不提供来源可信，详见 internal/tlscert 包注释）。
//
// 非法形态（配置期拒绝）：
//   - enabled=true 但既无 certFile/keyFile 也无 auto（旧的静默降级，现 fail-closed）。
//   - certFile/keyFile 只给其一（半配证书必然加载失败，提前拦下给出清晰报错）。
func (f *File) validateServerTLS() error {
	t := f.Server.TLS
	if !t.Enabled {
		return nil
	}
	hasCert := t.CertFile != ""
	hasKey := t.KeyFile != ""
	if hasCert != hasKey {
		// 半配：只给证书没给私钥，或反之。tls.LoadX509KeyPair 必然失败，
		// 提前在配置期指出，比运行期 ListenAndServeTLS 报错更可操作。
		return errors.New("server.tls: certFile and keyFile must be configured together")
	}
	if hasCert {
		return nil // 证书文件形态，校验通过。
	}
	if t.Auto {
		return nil // 自签形态，校验通过。
	}
	// enabled 但既无证书文件也无 auto：过去会静默降级明文，现在配置期拒绝。
	return errors.New("server.tls: enabled but no certFile/keyFile and auto=false; " +
		"refusing to fall back to plaintext HTTP (configure certFile/keyFile or set auto=true)")
}

// validateCluster 校验跨实例协调配置。
//
// 全部在配置期 fail-fast：一次集群配置错误（缺地址、两节点、端口冲突）如果
// 拖到启动期才发现，表现会是一场「选不出 leader」的静默故障——所有写都 409/504，
// 而日志里只有 raft 的连接失败，运维很难联想到是配置少写了一个字段。
func (f *File) validateCluster() error {
	if f.Cluster == nil || !f.Cluster.Enabled {
		return nil
	}
	c := f.Cluster
	if c.Listen == "" {
		return errors.New("cluster.listen: required when cluster.enabled")
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("cluster.listen %q: must be host:port: %v", c.Listen, err)
	} else if port == "" || port == "0" {
		// 端口 0 是随机分配：对端无法据此连进来，多节点必然失败。
		return fmt.Errorf("cluster.listen %q: port must be a fixed non-zero port", c.Listen)
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = "raft"
	}
	if len(c.Peers) == 0 {
		// 单节点集群：Bootstrap 由 runtime 置 true，本节点恒为 leader。
		return nil
	}
	ids := make(map[string]bool, len(c.Peers))
	addrs := make(map[string]bool, len(c.Peers))
	self := false
	for i, p := range c.Peers {
		id := strings.TrimSpace(p.ID)
		if id == "" {
			return fmt.Errorf("cluster.peers[%d]: id is required", i)
		}
		if ids[id] {
			return fmt.Errorf("cluster.peers[%d]: duplicate id %q", i, id)
		}
		ids[id] = true
		addr := strings.TrimSpace(p.Addr)
		if addr == "" {
			return fmt.Errorf("cluster.peers[%d] (%s): addr is required", i, id)
		}
		if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
			return fmt.Errorf("cluster.peers[%d] (%s): addr %q must be host:port", i, id, addr)
		}
		if addrs[addr] {
			return fmt.Errorf("cluster.peers[%d] (%s): duplicate addr %q", i, id, addr)
		}
		addrs[addr] = true
		if id == c.NodeID {
			self = true
			if addr != c.Listen {
				return fmt.Errorf("cluster.peers[%d] (%s): addr %q must equal cluster.listen %q", i, id, addr, c.Listen)
			}
		}
	}
	if len(c.Peers) == 2 {
		// 2 节点集群没有容错价值：任一节点故障就凑不齐多数派，反而比单节点
		// 更不可用。要么单节点，要么 >=3。
		return errors.New("cluster.peers: exactly 2 nodes has no fault tolerance; use 1 (single node) or >=3")
	}
	if c.NodeID == "" {
		return errors.New("cluster.nodeId: required when cluster.peers is set (it must match this node's peers[].id)")
	}
	if !self {
		return fmt.Errorf("cluster.peers: nodeId %q must appear in peers (include this node)", c.NodeID)
	}
	return nil
}

// validateAudit 校验审计保留策略。
func (f *File) validateAudit() error {
	a := f.Storage.Audit
	if a.Enabled == nil || !*a.Enabled {
		return nil
	}
	if strings.TrimSpace(a.Dir) == "" {
		return errors.New("storage.audit.dir: must not be empty")
	}
	if a.MaxBytes < 1024 {
		return fmt.Errorf("storage.audit.maxBytes: %d is too small (min 1024)", a.MaxBytes)
	}
	if a.SegmentBytes < 1024 {
		return fmt.Errorf("storage.audit.segmentBytes: %d is too small (min 1024)", a.SegmentBytes)
	}
	if a.SegmentBytes > a.MaxBytes {
		return fmt.Errorf("storage.audit.segmentBytes: %d exceeds maxBytes %d", a.SegmentBytes, a.MaxBytes)
	}
	if a.MaxAge.Duration() <= 0 {
		return fmt.Errorf("storage.audit.maxAge: must be > 0, got %s", a.MaxAge)
	}
	if a.MaxAge.Duration() > 5*365*24*time.Hour {
		return fmt.Errorf("storage.audit.maxAge: %s exceeds the 5-year limit", a.MaxAge)
	}
	return nil
}

// validateHistory 校验历史库策略。
//
// 这里只拦「逻辑上不可能成立」的组合：段比总量还大、心跳比最小间隔还短。
// 绝对大小（比如最少 1MB）不拦——测试和边缘小盒子需要小段，而真正的保护
// 是「段 ≤ 总量」这条不变量。
func (f *File) validateHistory() error {
	h := f.Storage.History
	if !h.Enabled {
		return nil
	}
	if strings.TrimSpace(h.Dir) == "" {
		return errors.New("storage.history.dir: must not be empty")
	}
	if h.MaxBytes < 1024 {
		return fmt.Errorf("storage.history.maxBytes: %d is too small (min 1024)", h.MaxBytes)
	}
	if h.SegmentBytes < 1024 {
		return fmt.Errorf("storage.history.segmentBytes: %d is too small (min 1024)", h.SegmentBytes)
	}
	if h.SegmentBytes > h.MaxBytes {
		return fmt.Errorf("storage.history.segmentBytes: %d exceeds maxBytes %d", h.SegmentBytes, h.MaxBytes)
	}
	if h.MaxAge.Duration() <= 0 {
		return fmt.Errorf("storage.history.maxAge: must be > 0, got %s", h.MaxAge)
	}
	if h.MaxAge.Duration() > 365*24*time.Hour {
		return fmt.Errorf("storage.history.maxAge: %s exceeds the 365-day limit", h.MaxAge)
	}
	if h.MinInterval.Duration() < 0 {
		return errors.New("storage.history.minInterval: must be >= 0")
	}
	if h.MinInterval.Duration() > time.Hour {
		return fmt.Errorf("storage.history.minInterval: %s exceeds 1h", h.MinInterval)
	}
	if h.Heartbeat != nil {
		if h.Heartbeat.Duration() < 0 {
			return errors.New("storage.history.heartbeat: must be >= 0 (use \"0s\" to disable)")
		}
		if h.Heartbeat.Duration() > 24*time.Hour {
			return fmt.Errorf("storage.history.heartbeat: %s exceeds 24h", h.Heartbeat)
		}
	}
	if math.IsNaN(h.Deadband) || math.IsInf(h.Deadband, 0) {
		return errors.New("storage.history.deadband: must be a finite number")
	}
	return nil
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
		if r.Expr != "" {
			prog, err := calc.Compile(r.Expr)
			if err != nil {
				return fmt.Errorf("alarm %q: %w", r.ID, err)
			}
			if err := f.validateWindows("alarm \""+r.ID+"\"", prog, index); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateWindows 校验表达式里的历史窗口聚合。
//
// 三条检查全部是静态的，也都对应一种「不报错但永远算不出值」的现场：
//   - 历史没开启 → 窗口求值必然失败，报警永远 unknown；
//   - 引用的点位不存在 / 是文本点 → 历史里根本没有可聚合的数值；
//   - 窗口比保留期长 → 老样本已经被淘汰，聚合悄悄只覆盖了最近一段，
//     结果「看起来正常」但含义与配置写的不一致。
func (f *File) validateWindows(where string, prog *calc.Program, index map[pointKey]model.Point) error {
	if !prog.UsesWindow() {
		return nil
	}
	if !f.Storage.History.Enabled {
		return fmt.Errorf("%s: expression %q uses a history window, but storage.history.enabled is false",
			where, prog.String())
	}
	maxAge := f.Storage.History.MaxAge.Duration()
	for _, w := range prog.Windows() {
		dev, pt, ok := splitRef(w.Ref)
		if !ok {
			return fmt.Errorf("%s: malformed window reference %q", where, w.Ref)
		}
		p, exists := index[pointKey{dev, pt}]
		if !exists {
			return fmt.Errorf("%s: window reference %q does not exist", where, w.Ref)
		}
		if !p.DataType.IsNumeric() {
			return fmt.Errorf("%s: window reference %q is %s, which has no numeric history",
				where, w.Ref, p.DataType)
		}
		if maxAge > 0 && w.Window > maxAge {
			return fmt.Errorf("%s: window %s for %q exceeds storage.history.maxAge %s; "+
				"older samples are evicted, so the aggregate would silently cover less than configured",
				where, w.Window, w.Ref, maxAge)
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
			if err := f.validateWindows(
				fmt.Sprintf("device %q point %q", d.ID, p.ID), prog, index); err != nil {
				return err
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
	// 串口是独占资源：同一个物理端口不能既做主机又做从站，也不能被两个
	// 从站同时打开。半双工总线上两个角色会互相抢答，症状是「偶发 CRC 错」
	// 或「读到的值来自另一台设备」——必须在配置期拦住。
	//
	// 比较的是**字面路径**：`/dev/ttyUSB0` 与 `/dev/serial/by-id/...` 指
	// 向同一台设备时这里看不出来。符号链接的风险写在驱动规范里，不假装
	// 能自动识别。
	serialOwner := make(map[string]string, len(f.Channels)+len(f.Slaves))
	for _, ch := range f.Channels {
		if ch.Transport.Kind == "serial" && ch.Transport.Port != "" {
			serialOwner[ch.Transport.Port] = "channel " + ch.ID
		}
	}
	seen := make(map[string]bool, len(f.Slaves))
	type listenEntry struct{ id, addr string }
	listens := make([]listenEntry, 0, len(f.Slaves))
	for _, s := range f.Slaves {
		if err := s.Validate(points); err != nil {
			return err
		}
		if seen[s.ID] {
			return fmt.Errorf("slave %q: duplicate id", s.ID)
		}
		seen[s.ID] = true
		if s.Serial != nil {
			port := s.Serial.Normalized().Port
			if owner, ok := serialOwner[port]; ok {
				return fmt.Errorf("slave %q: serial port %q already used by %s; one port can be either a master or a slave, not both", s.ID, port, owner)
			}
			serialOwner[port] = "slave " + s.ID
			continue
		}
		// 与北向 HTTP 监听撞端口是最常见的配置错误，值得单独拦一次。
		if listenConflicts(s.Listen, f.Server.Listen) {
			return fmt.Errorf("slave %q: listen %q conflicts with server.listen", s.ID, s.Listen)
		}
		// 两个从站监听同一地址同样要拦：真正的失败会推迟到第二个 bind，
		// 那时第一个从站已经起来了，启动失败的日志会更难读。
		for _, e := range listens {
			if listenConflicts(s.Listen, e.addr) {
				return fmt.Errorf("slave %q: listen %q conflicts with slave %q", s.ID, s.Listen, e.id)
			}
		}
		listens = append(listens, listenEntry{s.ID, s.Listen})
	}
	return nil
}

// validateNorthbound 校验北向通道参数。
//
// 这些是静态错误：配了 mqtt.enabled 却没有 broker，永远不可能投递成功。
// 让它在启动前失败，比让设备采集正常、数据默默发不出去要好得多。
func (f *File) validateNorthbound() error {
	// 契约版本：0（未配置，按 1）、1、2 合法；其余显式拒绝。0 必须放行——
	// 它是 omitempty 的「未配置」态，不能因为规范化不写字段就把它当非法值。
	cv := f.Northbound.ContractVersion
	if cv != 0 && cv != 1 && cv != 2 {
		return fmt.Errorf("northbound.contractVersion %d: only 0 (default), 1 and 2 are supported", cv)
	}
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
		switch m.Format {
		case MQTTFormatJSON, "":
			if !strings.Contains(m.Topic, "{device}") || !strings.Contains(m.Topic, "{point}") {
				return fmt.Errorf("northbound.mqtt.topic %q must contain {device} and {point}", m.Topic)
			}
			// TopicV2 只有在双发（cv==2）时才有意义；但只要用户填了就必须是
			// 合法模板，否则双发时 v2 lane 会发到错误主题。空串由 Normalize
			// 在 cv==2 时派生，cv==1 时保持空（不参与投递）。
			if strings.TrimSpace(m.TopicV2) != "" &&
				(!strings.Contains(m.TopicV2, "{device}") || !strings.Contains(m.TopicV2, "{point}")) {
				return fmt.Errorf("northbound.mqtt.topicV2 %q must contain {device} and {point}", m.TopicV2)
			}
		case MQTTFormatSparkplugB:
			// 主题是协议契约的一部分，不能让模板覆盖：一个「自定义了
			// topic」的 Sparkplug 配置在本地看着配好了，对端却永远收不到。
			if strings.TrimSpace(m.Topic) != "" {
				return errors.New("northbound.mqtt.topic must be empty when format is sparkplug-b: Sparkplug derives topics from groupId/nodeId/device")
			}
			// Sparkplug 有自己的契约与主题生成，不参与 json v2 双发。cv==2
			// 与 sparkplug-b 同时出现是一种自相矛盾的配置，必须显式拒绝。
			if cv == 2 {
				return errors.New("northbound.contractVersion 2 (dual-send) is not supported with format sparkplug-b: Sparkplug B carries its own contract")
			}
			// TopicV2 在 sparkplug 下没有任何消费方，留着是「配了但静默忽略」
			// 的隐患，与 topic 同样直接拒绝。
			if strings.TrimSpace(m.TopicV2) != "" {
				return errors.New("northbound.mqtt.topicV2 must be empty when format is sparkplug-b: Sparkplug derives topics from groupId/nodeId/device")
			}
			if m.Retain {
				return errors.New("northbound.mqtt.retain must be false when format is sparkplug-b: every edge-node message is non-retained (only a Host Application's STATE topic is retained)")
			}
			// QoS 在 Sparkplug 里由消息类型定死（数据/出生 0、NDEATH 遗嘱 1、
			// 命令订阅 1）。允许配置等于允许用户配出一个「本地看起来正常、
			// 严格 Host 直接丢弃」的部署，所以显式拒绝而不是静默忽略。
			if m.QoS != nil {
				return errors.New("northbound.mqtt.qos must be omitted when format is sparkplug-b: QoS is fixed per message type by the specification")
			}
			if err := validateSparkplugLevel("groupId", m.GroupID); err != nil {
				return err
			}
			if err := validateSparkplugLevel("nodeId", m.NodeID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("northbound.mqtt.format %q: only %q and %q are supported", m.Format, MQTTFormatJSON, MQTTFormatSparkplugB)
		}
		// 与运行时共用同一份地址规则：ssl:// 之类的 scheme 必须在这里就失败。
		if _, err := mqtt.NormalizeAddr(m.Broker); err != nil {
			return fmt.Errorf("northbound.mqtt.broker: %w", err)
		}
	}

	o := f.Northbound.OPCUA
	if o.Enabled {
		if o.Listen == "" {
			return errors.New("northbound.opcua.listen required when opcua.enabled")
		}
		// 端口冲突在这里拦一次：等到 Listen 失败只会得到一句
		// "address already in use"，运维还得自己去猜是谁占了。
		if listenConflicts(o.Listen, f.Server.Listen) {
			return fmt.Errorf("northbound.opcua.listen %q conflicts with server.listen", o.Listen)
		}
		for _, s := range f.Slaves {
			if listenConflicts(o.Listen, s.Listen) {
				return fmt.Errorf("northbound.opcua.listen %q conflicts with slave %q", o.Listen, s.ID)
			}
		}
		// 只做 None/None + 匿名。配置里写 true 之外的组合必须显式失败：
		// 悄悄降级成「无加密」是最危险的一种「兼容」。
		if !o.AllowAnon {
			return errors.New("northbound.opcua.allowAnonymous must be true: only anonymous access is supported")
		}
		if !strings.EqualFold(o.Policy, "None") {
			return fmt.Errorf("northbound.opcua.policy %q: only \"None\" is supported", o.Policy)
		}
	}
	return nil
}

// validateSparkplugLevel 校验 Sparkplug 主题里的一个命名层级。
//
// 这一层是 MQTT 主题的一部分，`/`、`+`、`#` 会改变主题结构；空串会让
// 主题少一段。三者都不会在本机报错，只会在对端「安静地收不到」。
func validateSparkplugLevel(field, v string) error {
	if v == "" {
		return fmt.Errorf("northbound.mqtt.%s required when format is sparkplug-b", field)
	}
	if strings.ContainsAny(v, "/+#") {
		return fmt.Errorf("northbound.mqtt.%s %q must not contain / + #", field, v)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("northbound.mqtt.%s %q contains control characters", field, v)
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
//
// cluster 块被**排除**在版本之外：它是节点本地启动参数（nodeId/listen/
// dataDir/bootstrap），不属于集群共享配置。若纳入哈希：
//   - 同一份逻辑配置在各节点会算出不同版本（nodeId 必然不同），
//     「全集群同一个配置版本」这个可核对的契约就没了；
//   - 节点身份会被写进版本号、快照文件名与 /api/v1/config 的对外响应。
//
// 排除它不会让存量配置哈希漂移：未启用集群时该字段本来就是 nil（omitempty）。
func (f *File) Canonical() ([]byte, error) {
	cp := *f
	cp.Cluster = nil
	b, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// MarshalFull 输出包含节点本地块的完整 JSON，用于落盘与版本快照。
//
// 与 Canonical 的分工：Canonical 是「全集群共享内容」的哈希输入；MarshalFull
// 是「本节点完整配置」的持久化形式。写盘用后者，否则每次配置应用都会把本地
// cluster 块从配置文件里抹掉（重启后集群身份丢失）。
func (f *File) MarshalFull() ([]byte, error) {
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

	mu      sync.Mutex
	current *File
	// raw 是落盘原文（含节点本地 cluster 块）。回滚/失败恢复用的就是它，
	// 不能用 Canonical()——后者刻意排除了 cluster 块。
	raw     []byte
	version string
	state   State
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
	full, err := f.MarshalFull()
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
		raw:       full,
		version:   VersionOf(canonical),
	}
	if err := os.MkdirAll(m.snapDir, 0o755); err != nil {
		return nil, fmt.Errorf("config: mkdir snapshots: %w", err)
	}
	if err := m.loadState(); err != nil {
		return nil, err
	}
	// 当前配置也必须有一份快照，否则回滚列表里看不到它。
	if err := m.writeSnapshot(m.version, full); err != nil {
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
//
// cluster 块是节点本地启动参数，**不从入参继承**：无论配置来自 API、回滚快照
// 还是共识日志（可能带着另一个节点的 nodeId/listen），落盘与生效的都是本机
// 原来的 cluster 块。集群拓扑只能改本地配置文件并重启，运行期 Apply 改不动它。
func (m *Manager) Apply(f *File, source string) (ApplyResult, error) {
	start := time.Now()
	if source == "" {
		source = "api"
	}
	m.mu.Lock()
	localCluster := m.current.Cluster
	m.mu.Unlock()
	if localCluster != nil {
		f = DeepCopy(f)
		cp := *localCluster
		f.Cluster = &cp
	}
	if err := f.Validate(); err != nil {
		return ApplyResult{}, err
	}
	// 提前算好「变更前」的版本号：Apply 内部会更新 m.current，等返回后再
	// 取就拿到新值了。
	beforeVersion, _ := m.currentVersion()
	canonical, err := f.Canonical()
	if err != nil {
		return ApplyResult{}, err
	}
	full, err := f.MarshalFull()
	if err != nil {
		return ApplyResult{}, err
	}
	version := VersionOf(canonical)

	m.mu.Lock()
	defer m.mu.Unlock()

	if version == m.version {
		return ApplyResult{
			Version: version, Previous: beforeVersion, Applied: false,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	prevVersion := m.version
	prevRaw := m.raw
	summary := summarize(m.current, f)

	// 1. 备份当前版本（快照本身即备份）。
	if err := m.writeSnapshot(prevVersion, prevRaw); err != nil {
		return ApplyResult{}, fmt.Errorf("config: backup: %w", err)
	}
	// 2. 原子写入新配置（完整内容，含本地 cluster 块）。
	if err := atomicWrite(m.path, full); err != nil {
		return ApplyResult{}, fmt.Errorf("config: write: %w", err)
	}
	// 3. 写新版本快照。
	if err := m.writeSnapshot(version, full); err != nil {
		_ = atomicWrite(m.path, prevRaw)
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
		_ = atomicWrite(m.path, prevRaw)
		return ApplyResult{}, fmt.Errorf("config: state: %w", err)
	}

	m.current = f
	m.raw = full
	m.version = version
	_ = m.prune(50)

	return ApplyResult{
		Version: version, Previous: prevVersion, Applied: true,
		Changed: summary, DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// currentVersion 返回当前生效配置的内容哈希版本。
// Apply 用它判断「这次提交是否真的改了东西」。配置管理器不导出 m.mu，
// 所以在这里包一层。
func (m *Manager) currentVersion() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version, nil
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

// DescribeInfo 是站点侧配置的只读描述，供中心管理平面做「配置下发」决策。
//
// 它刻意不含配置正文——正文走 GET /config（脱敏后的完整 canonical JSON）。
// 这里只给中心「比对与预览」所需的最小集合：当前配置哈希（中心与目标哈希
// 比对，相同则跳过下发）、站点与实例身份、契约版本、以及本次下发的影响面
// 统计（设备/点位/通道数，中心据此预估 500 台设备批量下发的规模与分组）。
type DescribeInfo struct {
	// ConfigVersion 是当前生效配置的版本哈希（sha256 前缀形式）。它是
	// VersionOf(Canonical()) 的结果，排除 cluster 块：中心下发的配置面向全
	// 集群共享内容，与节点的本地集群身份无关，比对哈希时也必须排除它。
	ConfigVersion string `json:"configVersion"`
	// Site 是站点标识。
	Site string `json:"site"`
	// Devices/Points/Channels 是配置影响面计数（下发会重建的设备规模）。
	Devices  int `json:"devices"`
	Points   int `json:"points"`
	Channels int `json:"channels"`
}

// Describe 返回当前生效配置的只读描述。
func (m *Manager) Describe() DescribeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.current
	points := 0
	for i := range f.Devices {
		points += len(f.Devices[i].Points)
	}
	return DescribeInfo{
		ConfigVersion: m.version,
		Site:          f.Site,
		Devices:       len(f.Devices),
		Points:        points,
		Channels:      len(f.Channels),
	}
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
	return WriteFile(path, data, 0o600)
}

// WriteFile 原子地写入文件：先写同目录临时文件、fsync，再 rename。
//
// 配置文件与点表都是「写坏就等于没配」的东西，不能出现半截文件。权限
// 默认由调用方给定（配置文件习惯用 0600，因为里面可能有凭据明文）。
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".uic-*")
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
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
