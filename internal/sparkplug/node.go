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
//
// **本包只实现 Edge Node 角色**。Sparkplug 的另一半是 Host Application
// （订阅 spBv1.0/#、维护指标状态、发布 STATE 出生/死亡证书）。STATE 是
// Host 的证书，主题形如 spBv1.0/STATE/{host_id}，Edge Node 发布它就是冒用
// 一个自己并不具备的身份——真正的 Host 会据此认为「有个 Host 在线」。
// 边缘节点的在线状态由 NBIRTH/NDEATH 表达（[tck-id-message-flow-edge-node-
// birth-publish-nbirth-topic] 与 [tck-id-topics-ndeath-topic]），不需要也不
// 应该发 STATE。
package sparkplug

import (
	"strings"
	"sync"
	"time"

	codec "uic/internal/codec/sparkplug"
	"uic/internal/model"
)

// Message 是一条待发布的 Sparkplug 消息。
//
// QoS 由**消息类型**定死，不是配置项：规范对每类消息逐条给出 MUST
// （[tck-id-topics-*-mqtt] 一律 QoS 0；只有 NDEATH 的遗嘱要求 QoS 1）。
// 把它做成可配，就等于允许用户配出一个「本地看起来正常、严格 Host 直接
// 丢弃」的部署。
//
// retain 不放进结构体：Sparkplug 对 Edge Node 的每一条消息都要求
// retain=false（同一组 TCK 项），一个恒为 false 的开关只会让人以为它可配。
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
}

const (
	// edgeQoS 是 Edge Node 数据与出生消息的 QoS。规范对 NBIRTH/NDATA/
	// DBIRTH/DDATA/DDEATH 逐条写明 MUST be published with MQTT QoS equal
	// to 0（[tck-id-topics-nbirth-mqtt] 等）。
	edgeQoS byte = 0
	// ndeathQoS 是 NDEATH 的 QoS。它的主要身份是 CONNECT 里的遗嘱，
	// 而遗嘱的 QoS 必须是 1（[tck-id-payloads-ndeath-will-message-qos]）；
	// 断开前的主动发布沿用同一个值，否则「主动告知的死亡」反而比
	// 「Broker 代发的遗嘱」更容易丢。
	ndeathQoS byte = 1
	// SubscribeQoS 是命令订阅的 QoS。规范要求 Edge Node 以 QoS 1 订阅
	// NCMD、以 QoS 1 订阅 DCMD（[tck-id-message-flow-edge-node-ncmd-
	// subscribe]、[tck-id-message-flow-device-dcmd-subscribe]）：QoS 0 的
	// 订阅会在链路抖动时静默丢掉重生请求，表现为「点了重生没反应」。
	SubscribeQoS byte = 1
)

// 主题里的消息类型。
const (
	KindNBIRTH = "NBIRTH"
	KindNDEATH = "NDEATH"
	KindDBIRTH = "DBIRTH"
	KindDDEATH = "DDEATH"
	KindDDATA  = "DDATA"
	KindNCMD   = "NCMD"
	KindDCMD   = "DCMD"
)

// Topic 拼出一条 Sparkplug 主题。
//
// 只覆盖 Edge Node 的 7 类消息（namespace/group_id/message_type/
// edge_node_id[/device_id]）。Host 的 STATE 主题没有 group 段，形态不同，
// 见包注释：本实现不产生它。
func Topic(group, kind, node, device string) string {
	base := "spBv1.0/" + group + "/" + kind + "/" + node
	if device != "" {
		base += "/" + device
	}
	return base
}

// ParseTopic 解析 Sparkplug 主题，返回各层级。
//
// 只接受 Edge Node 的 7 类消息：STATE 属于 Host，不在这里的语法范围内，
// 遇到就返回 ok=false（调用方只会把它当成「不是发给我的命令」忽略）。
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
	case KindNBIRTH, KindNDEATH, KindDBIRTH, KindDDEATH, KindDDATA, KindNCMD, KindDCMD:
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
//
// bdSeq 是另一套计数，语义完全不同：它**每次 MQTT CONNECT** 前进一格
// （0 开始，255 之后回绕 0），并同时出现在 CONNECT 的 Will 载荷与随后的
// NBIRTH 里（[tck-id-payloads-ndeath-bdseq]）。Host 靠它把一对
// NDEATH/NBIRTH 配对，并且在消息乱序时区分「这次掉线」与「上一次的旧
// 遗嘱」。所以它不能是进程级常量，也不能只生成一次。
type Node struct {
	GroupID string
	NodeID  string

	mu    sync.Mutex
	seq   uint64 // 下一条消息的序号（0..255）
	bdSeq uint64 // 当前 MQTT 会话的 bdSeq（0..255）
	next  uint64 // 下一次 CONNECT 要分配的 bdSeq
	now   func() time.Time
}

// NewNode 创建一个节点。bdSeq 由 BeginSession 维护，首次为 0。
func NewNode(group, node string) *Node {
	return &Node{
		GroupID: group,
		NodeID:  node,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock 供测试固定时间。
func (n *Node) SetClock(fn func() time.Time) { n.now = fn }

// BdSeq 返回当前会话的 bdSeq，供日志与诊断使用。
func (n *Node) BdSeq() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.bdSeq
}

// BeginSession 登记一次新的 MQTT CONNECT：bdSeq 前进一格并记为本次会话的
// 值，返回该值。
//
// 「0 开始、每次 CONNECT +1、255 之后回绕 0」是规范的 MUST
// （[tck-id-topics-nbirth-bdseq-increment]）。调用点是 MQTT 客户端构造
// CONNECT 报文的那一刻：遗嘱载荷要用这个值，之后同一条连接上的 NBIRTH
// 必须用同一个值，所以推进与记录必须同时发生。
func (n *Node) BeginSession() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.bdSeq = n.next
	n.next = (n.next + 1) & 0xFF
	return n.bdSeq
}

// takeSeqLocked 取出当前序号并把计数器推进一格（调用方持锁）。
func (n *Node) takeSeqLocked() uint64 {
	v := n.seq
	n.seq = (n.seq + 1) & 0xFF
	return v
}

// nextSeq 取出当前序号并把计数器推进一格（255 之后回绕到 0）。
func (n *Node) nextSeq() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.takeSeqLocked()
}

// NBIRTH 生成节点出生消息。
//
// 规范要求 NBIRTH 的序号必须是 0（并重置计数器）：一次重生开启一个新的
// 消息序列，后续每条消息从 1 开始递增。
func (n *Node) NBIRTH() (Message, error) {
	now := n.now()
	// 归零与取号必须在同一把锁里：分两段会让一条并发消息插在中间，
	// 于是 NBIRTH 拿到的不是 0，或者同一条消息里出现「两条 0 号」。
	n.mu.Lock()
	n.seq = 0
	seq, bd := n.takeSeqLocked(), n.bdSeq
	n.mu.Unlock()

	p := codec.Payload{
		Timestamp: now,
		Seq:       seq,
		Metrics: []codec.Metric{
			bdSeqMetric(bd, now),
			controlMetric("Node Control/Rebirth", now),
		},
	}
	return n.message(KindNBIRTH, "", p)
}

// NDEATH 生成节点死亡消息。MQTT 的遗嘱在建立连接时就要有完整载荷，因此
// 这个方法必须能在连接之前调用（sink 在每次 CONNECT 之前调它）。
//
// NDEATH 是唯一不带序号的消息（[tck-id-topics-ndeath-seq]），也不消耗
// 序号：它由 Broker 在会话结束时代发，与节点自己发出的消息不在同一个
// 序列里。载荷只允许一条 bdSeq 指标（[tck-id-topics-ndeath-payload]）。
func (n *Node) NDEATH() (Message, error) {
	now := n.now()
	n.mu.Lock()
	bd := n.bdSeq
	n.mu.Unlock()

	p := codec.Payload{
		Timestamp: now,
		OmitSeq:   true,
		Metrics:   []codec.Metric{bdSeqMetric(bd, now)},
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

func (n *Node) message(kind, device string, p codec.Payload) (Message, error) {
	raw, err := p.Encode()
	if err != nil {
		return Message{}, err
	}
	qos := edgeQoS
	if kind == KindNDEATH {
		qos = ndeathQoS
	}
	return Message{Topic: Topic(n.GroupID, kind, n.NodeID, device), Payload: raw, QoS: qos}, nil
}

// Subscriptions 返回需要订阅的命令主题。设备级命令用 `+` 通配，避免把
// 设备数量写进订阅列表。
//
// 顺序固定（NCMD 在前）：订阅是一次 SUBSCRIBE 报文，顺序稳定才便于抓包
// 对比。QoS 由调用方统一取 SubscribeQoS。
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
	_, kind, _, device, ok := ParseTopic(topic)
	if !ok || (kind != KindNCMD && kind != KindDCMD) {
		return Command{}, false
	}
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
		// 显式判类型：`b, _ := m.Value.(bool)` 会把「值不是布尔」悄悄当成
		// false，于是把一个坏掉的载荷读成「对方不要求重生」——两侧都没错，
		// 但命令被无声吞掉。类型不符一律按「不认识这条命令」处理。
		b, isBool := m.Value.(bool)
		if !isBool {
			return Command{}, false
		}
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
