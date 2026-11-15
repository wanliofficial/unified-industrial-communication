// Package sparkplug 实现 Sparkplug B 边缘节点的语义层：主题命名、生命周期
// 消息、指标映射与序号管理。
//
// 分工：
//
//   - internal/codec/sparkplug 只负责 Protobuf 线格式；
//   - 本包负责「哪条消息该在什么时候发、带什么指标」；
//   - internal/runtime 的 sink 负责 MQTT 连接与投递。
//
// 这样节点状态机可以脱离网络单测：序号回绕、bdSeq、NDEATH 不带序号等最容易
// 出错的地方不需要真的连 Broker。
package sparkplug

import (
	"fmt"
	"strings"
	"sync"
	"time"

	codec "uic/internal/codec/sparkplug"
	"uic/internal/model"
)

// Message 是一条待发布的 Sparkplug 消息。
//
// Retain 一律为 false：Sparkplug B 只允许 STATE 主题使用 retained，
// 生命周期与数据消息用 retained 会让新订阅者收到一帧没有上下文的历史数据。
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// 主题里的消息类型。
const (
	KindNBIRTH = "NBIRTH"
	KindNDEATH = "NDEATH"
	KindDBIRTH = "DBIRTH"
	KindDDEATH = "DDEATH"
	KindDDATA  = "DDATA"
	KindNCMD   = "NCMD"
	KindDCMD   = "DCMD"
	KindSTATE  = "STATE"
)

// Topic 拼出一条 Sparkplug 主题。
func Topic(group, kind, node, device string) string {
	base := "spBv1.0/" + group + "/" + kind + "/" + node
	if device != "" {
		base += "/" + device
	}
	return base
}

// ParseTopic 解析 Sparkplug 主题。只接受 NCMD/DCMD/STATE 之外的标准类型，
// 其余返回 ok=false。
func ParseTopic(topic string) (group, kind, node, device string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[0] != "spBv1.0" {
		return "", "", "", "", false
	}
	group, kind, node = parts[1], parts[2], parts[3]
	// 空段是语法错误而不是「通配」：Broker 不会替我们补全，拼错的订阅
	// 会安静地收不到任何命令。
	if group == "" || node == "" {
		return "", "", "", "", false
	}
	switch kind {
	case KindNBIRTH, KindNDEATH, KindDBIRTH, KindDDEATH, KindDDATA, KindNCMD, KindDCMD, KindSTATE:
	default:
		return "", "", "", "", false
	}
	if len(parts) == 5 {
		device = parts[4]
		if device == "" {
			return "", "", "", "", false
		}
	} else if len(parts) > 5 {
		return "", "", "", "", false
	}
	return group, kind, node, device, true
}

// Node 维护一个边缘节点的序号与 bdSeq。
//
// 序号是**每个边缘节点一个**，不是每台设备一个：规范对 NBIRTH/NDATA/
// DBIRTH/DDATA/DDEATH 逐条写明「one greater than the previous MQTT message
// from the Edge Node」。所以节点自己的消息与它所有设备的消息共用同一个
// 0..255 计数器，只有 NBIRTH 会把它重置为 0（[tck-id-topics-nbirth-seq-num]）。
// 把设备当成独立序号作用域会让两台设备交替发布时线上出现 0,0,1,1——严格
// 的主机按「序号必须连续」判定就会请求重生或丢弃数据。
//
// NDEATH 例外：它不带序号，也不消耗序号。
type Node struct {
	GroupID string
	NodeID  string
	BdSeq   uint64

	mu  sync.Mutex
	seq uint64
	now func() time.Time
}

// NewNode 创建一个节点。bdSeq 由调用方在进程启动时生成，并在整个 MQTT
// 会话期间保持不变：NDEATH 的遗嘱在 CONNECT 时就已固定，重连后 NBIRTH
// 必须用同一个 bdSeq，否则消费方无法把两者配对。
func NewNode(group, node string, bdSeq uint64) *Node {
	return &Node{
		GroupID: group,
		NodeID:  node,
		BdSeq:   bdSeq & 0xFF,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock 供测试固定时间。
func (n *Node) SetClock(fn func() time.Time) { n.now = fn }

// nextSeq 取出当前序号并把计数器推进一格（255 之后回绕到 0）。
func (n *Node) nextSeq() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	v := n.seq
	n.seq = (n.seq + 1) & 0xFF
	return v
}

// NBIRTH 生成节点出生消息。
//
// 规范要求 NBIRTH 的序号必须是 0（并重置计数器）：一次重生开启一个新的
// 消息序列，后续每条消息从 1 开始递增。
func (n *Node) NBIRTH() (Message, error) {
	n.mu.Lock()
	n.seq = 0
	n.mu.Unlock()
	now := n.now()
	p := codec.Payload{
		Timestamp: now,
		Seq:       n.nextSeq(),
		Metrics: []codec.Metric{
			bdSeqMetric(n.BdSeq, now),
			controlMetric("Node Control/Rebirth", now),
		},
	}
	return n.message(KindNBIRTH, "", p)
}

// NDEATH 生成节点死亡消息。MQTT 的遗嘱在建立连接时就要有完整载荷，因此
// 这个方法必须能在连接之前调用。
//
// NDEATH 是唯一不带序号的消息（[tck-id-topics-ndeath-seq]），也不消耗
// 序号：它由 Broker 在会话结束时代发，与节点自己发出的消息不在同一个
// 序列里。
func (n *Node) NDEATH() (Message, error) {
	now := n.now()
	p := codec.Payload{
		Timestamp: now,
		OmitSeq:   true,
		Metrics:   []codec.Metric{bdSeqMetric(n.BdSeq, now)},
	}
	return n.message(KindNDEATH, "", p)
}

// DBIRTH 生成设备出生消息。
//
// 序号不按设备重置：Sparkplug 的序号是边缘节点级的，DBIRTH 取「上一条消息
// 的序号 + 1」。设备重新出生只重发 BIRTH，不开启新的序号序列。
//
// samples 是该设备当前已知的全部点位：Sparkplug 要求 DBIRTH 列出设备的
// 全部指标，后续 DDATA 只更新值。只带首个点位出生会让消费方在出生时看到
// 一个不完整的设备。
func (n *Node) DBIRTH(device string, samples []model.Sample) (Message, error) {
	now := n.now()
	metrics := make([]codec.Metric, 0, len(samples)+1)
	metrics = append(metrics, controlMetric("Device Control/Rebirth", now))
	for _, s := range samples {
		metrics = append(metrics, MetricForSample(s, now))
	}
	p := codec.Payload{
		Timestamp: now,
		Seq:       n.nextSeq(),
		Metrics:   metrics,
	}
	return n.message(KindDBIRTH, device, p)
}

// DDEATH 生成设备死亡消息。载荷不带指标：设备已经不可达，保留最后一帧
// 值只会让消费方误以为那是当前值。
func (n *Node) DDEATH(device string) (Message, error) {
	p := codec.Payload{Timestamp: n.now(), Seq: n.nextSeq()}
	return n.message(KindDDEATH, device, p)
}

// DDATA 生成一条设备数据消息。
func (n *Node) DDATA(device string, s model.Sample) (Message, error) {
	now := n.now()
	p := codec.Payload{
		Timestamp: now,
		Seq:       n.nextSeq(),
		Metrics:   []codec.Metric{MetricForSample(s, now)},
	}
	return n.message(KindDDATA, device, p)
}

// State 生成 STATE 主题的 JSON 载荷（retained）。它不是 Protobuf：
// Sparkplug 的 STATE 是一个固定的 JSON 结构，消费方靠它判断节点在线。
func (n *Node) State(online bool) (Message, error) {
	now := n.now()
	payload := fmt.Sprintf(`{"timestamp":%d,"online":%t,"bdSeq":%d}`,
		now.UnixMilli(), online, n.BdSeq)
	return Message{Topic: Topic(n.GroupID, KindSTATE, n.NodeID, ""), Payload: []byte(payload), Retain: true}, nil
}

func (n *Node) message(kind, device string, p codec.Payload) (Message, error) {
	raw, err := p.Encode()
	if err != nil {
		return Message{}, err
	}
	return Message{Topic: Topic(n.GroupID, kind, n.NodeID, device), Payload: raw}, nil
}

// Subscriptions 返回需要订阅的命令主题。设备级命令用 `+` 通配，避免把
// 设备数量写进订阅列表。
func (n *Node) Subscriptions() []string {
	return []string{
		Topic(n.GroupID, KindNCMD, n.NodeID, ""),
		Topic(n.GroupID, KindDCMD, n.NodeID, "+"),
	}
}

// Command 是一条解析后的命令。
type Command struct {
	// Device 为空表示节点级命令。
	Device string
	// Rebirth 表示对端要求重发 BIRTH。
	Rebirth bool
}

// ParseCommand 解析 NCMD/DCMD。未知指标（包括本平台不支持的命令）返回
// ok=false：Sparkplug 允许订阅方发任意命令，边缘节点只回应自己实现的
// 那几个，其余静默忽略是协议的一部分。
func ParseCommand(topic string, payload []byte) (Command, bool) {
	group, kind, node, device, ok := ParseTopic(topic)
	if !ok || (kind != KindNCMD && kind != KindDCMD) {
		return Command{}, false
	}
	_ = group
	_ = node
	p, err := codec.Decode(payload)
	if err != nil {
		return Command{}, false
	}
	want := "Node Control/Rebirth"
	if kind == KindDCMD {
		want = "Device Control/Rebirth"
	}
	for _, m := range p.Metrics {
		if m.Name != want {
			continue
		}
		b, _ := m.Value.(bool)
		return Command{Device: device, Rebirth: b}, b
	}
	return Command{}, false
}

// ---------------------------------------------------------------------------
// 指标映射
// ---------------------------------------------------------------------------

// MetricForSample 把平台采样映射成 Sparkplug 指标。
//
// 质量不是 good 时用 is_null 表达：Sparkplug 没有质量位，把断线前的旧值
// 当成新值发出去，比不发更危险。原始质量放进 `quality` 属性，保留诊断
// 信息；单位放进标准的 `engUnit` 属性。
func MetricForSample(s model.Sample, now time.Time) codec.Metric {
	m := codec.Metric{
		Name:      s.PointID,
		Timestamp: s.CollectedAt.UTC(),
		DataType:  dataTypeOf(s.Value.Type),
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = now
	}
	if s.Unit != "" || s.Quality != model.QualityGood {
		m.Properties = map[string]string{}
		if s.Unit != "" {
			m.Properties["engUnit"] = s.Unit
		}
		if s.Quality != "" && s.Quality != model.QualityGood {
			m.Properties["quality"] = string(s.Quality)
		}
	}
	if s.Quality != model.QualityGood {
		m.IsNull = true
		return m
	}
	switch s.Value.Type {
	case model.TypeBool:
		m.Value = s.Value.Bool
	case model.TypeInt16, model.TypeInt32, model.TypeInt64:
		m.Value = int64(s.Value.Num)
	case model.TypeUint16, model.TypeUint32, model.TypeUint64:
		m.Value = uint64(s.Value.Num)
	case model.TypeFloat32:
		m.Value = float32(s.Value.Num)
	case model.TypeFloat64:
		m.Value = s.Value.Num
	case model.TypeString:
		m.Value = s.Value.Text
	case model.TypeBytes:
		m.Value = []byte(s.Value.Text)
	default:
		// BCD 之类在平台内是数值语义，按 double 发出去，避免在一帧里
		// 放一个空指标让消费方猜。
		m.DataType = codec.TypeDouble
		m.Value = s.Value.Num
	}
	return m
}

func dataTypeOf(dt model.DataType) codec.DataType {
	switch dt {
	case model.TypeBool:
		return codec.TypeBoolean
	case model.TypeInt16:
		return codec.TypeInt16
	case model.TypeUint16:
		return codec.TypeUInt16
	case model.TypeInt32:
		return codec.TypeInt32
	case model.TypeUint32:
		return codec.TypeUInt32
	case model.TypeInt64:
		return codec.TypeInt64
	case model.TypeUint64:
		return codec.TypeUInt64
	case model.TypeFloat32:
		return codec.TypeFloat
	case model.TypeFloat64:
		return codec.TypeDouble
	case model.TypeString:
		return codec.TypeString
	case model.TypeBytes:
		return codec.TypeBytes
	}
	return codec.TypeDouble
}

func bdSeqMetric(seq uint64, now time.Time) codec.Metric {
	return codec.Metric{
		Name:      "bdSeq",
		Timestamp: now,
		DataType:  codec.TypeInt64,
		Value:     int64(seq),
	}
}

func controlMetric(name string, now time.Time) codec.Metric {
	return codec.Metric{
		Name:      name,
		Timestamp: now,
		DataType:  codec.TypeBoolean,
		Value:     false,
	}
}
