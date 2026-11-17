package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"uic/internal/alarm"
	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/mqtt"
	"uic/internal/obs"
	"uic/internal/outbox"
	"uic/internal/scheduler"
	"uic/internal/sparkplug"
)

// sparkplugSink 是 MQTT 北向的 Sparkplug B 格式。
//
// 与 json 格式的根本区别在于「消费方需要上下文」：一条 DDATA 只有在
// 节点与设备都已 BIRTH 之后才有意义，序号也要求在同一会话内连续。因此
// 这里维护两份可变状态：设备（已见过的点位与最后值）与会话代数（重连后
// 必须重发 BIRTH）；节点序号与 bdSeq 由 internal/sparkplug 的状态机管理。
//
// 本 sink 只扮演 **Edge Node**：出生用 NBIRTH、死亡用 NDEATH，设备用
// DBIRTH/DDEATH。STATE 主题属于 Host Application（spBv1.0/STATE/{host_id}），
// 由 Host 发布、retain=true；边缘节点发 STATE 是冒用一个自己没有的身份。
// 本节点是否在线，Host 只看 NBIRTH/NDEATH。
//
// QoS 不是配置项：Sparkplug 对 Edge Node 的每类消息逐条给出 MUST（数据与
// 出生消息 QoS 0、NDEATH 遗嘱 QoS 1、命令订阅 QoS 1），全部由
// internal/sparkplug 按消息类型定死。retain 同理恒为 false。
//
// 失败语义与 json 格式一致：整条 DDATA 投递失败就返回错误，让 Outbox 从
// 最后成功的位点重发。BIRTH 与 DDATA 之间不是事务——如果 BIRTH 成功而
// DDATA 失败，重试只会重发 DDATA，这是正确的：设备已经出生。
type sparkplugSink struct {
	client *mqtt.Client
	node   *sparkplug.Node
	met    *obs.Metrics

	// pubMu 串行化所有发布。Sparkplug 的序号与「先 BIRTH 后 DATA」都是
	// 顺序契约，并发发布会让两条消息的序号交错，消费方只能报错或丢弃。
	pubMu sync.Mutex
	// epoch 是已经完成 BIRTH 的会话代数。它与 client.Epoch() 不等时说明
	// 连接重建过（或还没建），必须先补 NBIRTH/DBIRTH。
	epoch uint64
	// closed 阻止关闭之后还从命令回调里发消息。
	closed bool

	devices map[string]*sparkplugDevice
	// deviceOrder 让重连补发 DBIRTH 的顺序稳定：map 遍历顺序随机，
	// 每次重连的报文顺序不同会让抓包对比与测试断言失去意义。
	deviceOrder []string

	published uint64
	failed    uint64
}

type sparkplugDevice struct {
	// order 是点位首次出现的顺序，同样用于稳定 DBIRTH 的指标顺序。
	order []string
	last  map[string]model.Sample
}

func newSparkplugSink(cfg config.MQTT) (*sparkplugSink, error) {
	if cfg.Broker == "" {
		return nil, fmt.Errorf("northbound.mqtt.broker required when mqtt.enabled")
	}
	group := cfg.GroupID
	nodeID := cfg.NodeID
	if group == "" || nodeID == "" {
		return nil, fmt.Errorf("northbound.mqtt.groupId/nodeId required when format is sparkplug-b")
	}
	n := sparkplug.NewNode(group, nodeID)

	clientID := cfg.ClientID
	if clientID == "" {
		// 用 Node ID 而不是站点名：同一站点内两台网关配了不同 nodeId 却
		// 忘了 clientId 时，用站点名会互相顶掉会话，而 Sparkplug 恰恰
		// 依赖每个节点各自独立。
		clientID = nodeID
	}
	s := &sparkplugSink{
		node:    n,
		devices: map[string]*sparkplugDevice{},
	}
	filters := make([]mqtt.TopicFilter, 0, 2)
	for _, topic := range n.Subscriptions() {
		// 订阅 QoS 由协议定死为 1，与 cfg.QoS 无关：QoS 0 的订阅会在链路
		// 抖动时静默丢掉重生请求，表现为「点了重生没反应」。
		filters = append(filters, mqtt.TopicFilter{Filter: topic, QoS: sparkplug.SubscribeQoS})
	}
	s.client = mqtt.New(mqtt.Options{
		Addr:      cfg.Broker,
		ClientID:  clientID,
		Username:  cfg.Username,
		Password:  cfg.Password,
		KeepAlive: 30 * time.Second,
		// 与 json sink 同样的理由：必须短于 Outbox 单轮 drain 的预算。
		AckTimeout: 3 * time.Second,
		// 遗嘱必须每次 CONNECT 重新生成：bdSeq 是「每个新 CONNECT +1」
		// （[tck-id-topics-nbirth-bdseq-increment]），而它就写在 NDEATH
		// 载荷里。常量遗嘱会让重连之后所有死亡消息都带同一个 bdSeq，
		// Host 无法区分「这次掉线」与「上一次的旧遗嘱」。
		Will: func() *mqtt.Will {
			// BeginSession 同时推进 bdSeq 并记录下来，随后的 NBIRTH 必须
			// 用同一个值，Host 靠它把这一对 NDEATH/NBIRTH 配起来。
			n.BeginSession()
			death, err := n.NDEATH()
			if err != nil {
				// 编码失败意味着 bdSeq 指标装不进载荷，这只会发生在内存
				// 耗尽这类灾难场景。返回 nil 就是「不带遗嘱」——比发出
				// 一条内容错误的遗嘱安全：Broker 不会代发垃圾，Host 会
				// 靠 keepalive 超时发现节点消失。
				return nil
			}
			return &mqtt.Will{Topic: death.Topic, Payload: death.Payload, QoS: death.QoS, Retain: false}
		},
		Subscriptions: filters,
		OnMessage:     s.handleCommand,
		OnConnect:     func() { s.resync(context.Background()) },
	})
	return s, nil
}

// setMetrics 注入指标出口。
func (s *sparkplugSink) setMetrics(met *obs.Metrics) { s.met = met }

func (s *sparkplugSink) count(result string) {
	if s.met != nil {
		s.met.Inc("uic_mqtt_publish_total", obs.Labels{"result": result})
	}
}

// deviceName 清掉主题分隔符。设备 ID 在配置期已受约束，这里是最后一道
// 闸门：一个含 `/` 的 ID 会让 DBIRTH 悄悄跑到别的设备主题上。
func (s *sparkplugSink) deviceName(id string) string { return sanitizeTopicLevel(id) }

// deliver 实现 deliverFunc 契约：一条变化事件一条 DDATA。
//
// 新设备或新点位先补 DBIRTH。Sparkplug 要求 BIRTH 列出设备的全部指标，
// 所以这里保存每个点位最后一个采样；点位集合变化时重发一次完整 BIRTH。
func (s *sparkplugSink) deliver(ctx context.Context, recs []outbox.Record) (int, error) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.closed {
		return 0, fmt.Errorf("sparkplug: sink closed")
	}
	if err := s.ensureSession(ctx); err != nil {
		s.failed++
		s.count("error")
		return 0, err
	}

	done := 0
	for _, rec := range recs {
		var ev eventPayload
		if err := json.Unmarshal(rec.Data, &ev); err != nil {
			// 记录由本进程写入，解析失败说明数据已损坏。跳过并计数，
			// 而不是让一条坏记录把整个队列永久卡住。
			s.failed++
			s.count("decode_error")
			done++
			continue
		}
		device := s.deviceName(ev.DeviceID)
		dev := s.devices[device]
		known := false
		if dev != nil {
			_, known = dev.last[ev.PointID]
		}
		if dev == nil || !known {
			if dev == nil {
				dev = &sparkplugDevice{last: map[string]model.Sample{}}
				s.devices[device] = dev
				s.deviceOrder = append(s.deviceOrder, device)
			}
			birth, berr := s.node.DBIRTH(device, dev.snapshot(&ev.Sample))
			if err := s.publishLocked(ctx, birth, berr); err != nil {
				return done, err
			}
			dev.add(ev.Sample)
		}
		data, derr := s.node.DDATA(device, ev.Sample)
		if err := s.publishLocked(ctx, data, derr); err != nil {
			return done, err
		}
		dev.add(ev.Sample)
		done++
	}
	return done, nil
}

// snapshot 返回当前已知点位的快照，不改变状态。extra 非 nil 时把它一并
// 算进去（新点位首次出生，还没写进 last）。
func (d *sparkplugDevice) snapshot(extra *model.Sample) []model.Sample {
	out := make([]model.Sample, 0, len(d.order)+1)
	for _, point := range d.order {
		out = append(out, d.last[point])
	}
	if extra != nil {
		if _, ok := d.last[extra.PointID]; !ok {
			out = append(out, *extra)
		}
	}
	return out
}

func (d *sparkplugDevice) add(s model.Sample) {
	if _, ok := d.last[s.PointID]; !ok {
		d.order = append(d.order, s.PointID)
	}
	d.last[s.PointID] = s
}

// ensureSession 保证当前连接上已经发过 BIRTH。调用方必须持有 pubMu。
//
// OnConnect 是异步回调，不能保证它在第一条 Publish 之前执行；这里用
// 连接代数做「恰好一次」的判定，两条路径谁先到都可以。
func (s *sparkplugSink) ensureSession(ctx context.Context) error {
	if err := s.client.Connect(ctx); err != nil {
		return fmt.Errorf("sparkplug: connect: %w", err)
	}
	epoch := s.client.Epoch()
	if epoch == 0 || epoch == s.epoch {
		return nil
	}
	// NBIRTH 必须带本次会话的 bdSeq 与 Node Control/Rebirth；它是节点每次
	// 重新出生都要走的唯一路径。bdSeq 已由 Will 回调里的 BeginSession 推进。
	birth, berr := s.node.NBIRTH()
	if err := s.publishLocked(ctx, birth, berr); err != nil {
		return err
	}
	for _, device := range s.deviceOrder {
		dev := s.devices[device]
		if dev == nil || len(dev.order) == 0 {
			continue
		}
		msg, err := s.node.DBIRTH(device, dev.snapshot(nil))
		if err := s.publishLocked(ctx, msg, err); err != nil {
			return err
		}
	}
	s.epoch = epoch
	return nil
}

// resync 在每次（重）连成功后补 NBIRTH/DBIRTH。
//
// 它与 ensureSession 共用同一把锁与同一个代数判定：Broker 先完成连接、
// 采集线程紧接着发数据时，两者只会有一个真正执行 BIRTH。
func (s *sparkplugSink) resync(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.closed {
		return
	}
	if err := s.ensureSession(ctx); err != nil {
		s.failed++
		s.count("birth_error")
	}
}

func (s *sparkplugSink) publishLocked(ctx context.Context, msg sparkplug.Message, err error) error {
	if err != nil {
		s.failed++
		s.count("encode_error")
		return fmt.Errorf("sparkplug: encode: %w", err)
	}
	if err := s.client.Publish(ctx, mqtt.Message{
		Topic:   msg.Topic,
		Payload: msg.Payload,
		// QoS 跟着消息类型走（数据/出生 0，NDEATH 1）。retain 恒为 false，
		// 所以这里连字段都不传——mqtt.Message 的零值就是协议要求的值。
		QoS: msg.QoS,
	}); err != nil {
		s.failed++
		s.count("error")
		return fmt.Errorf("sparkplug: publish %s: %w", msg.Topic, err)
	}
	s.published++
	s.count("ok")
	return nil
}

// onState 把设备运行态迁移映射成 Sparkplug 的设备生命周期消息。
//
// 六个运行态里只有三种对 Sparkplug 消费方有意义：
//
//   - collecting：设备可用，接下来会有 DDATA。若此前已死，下一条数据会
//     触发新的 DBIRTH（deliver 的 known 判定），这里不需要预先重启。
//   - faulted / disabled：设备已不可达或不该被读。发 DDEATH 并忘掉点位，
//     避免重连时把一台已经下线的设备重新 BIRTH 出来。
//   - connecting / degraded / configured：链路还在，只是不完整或正在重连。
//     不发 DDEATH——DDATA 里的 is_null 已经表达「这个点位现在没有好值」，
//     而设备级 DDEATH 的语义是「这台设备再也不会有数据了」，用它会撒谎。
//
// 特别地：degraded 是「部分点位失败、其余仍在采集」。发 DDEATH 会让 Host
// 把整台设备从模型里摘掉，正在正常上报的点位也跟着消失——这比不发严重
// 得多，属于协议语义上的错报。
func (s *sparkplugSink) onState(ev scheduler.StateEvent) {
	switch ev.To {
	case scheduler.StateFaulted, scheduler.StateDisabled:
	default:
		return
	}
	s.publishDeath(ev.DeviceID, "state_"+string(ev.To))
}

// onRemove：设备从配置里被删除（或同 ID 重建）。必须发 DDEATH。
//
// 不发的话，重连时的 DBIRTH 会把一台已经不在配置里的设备重新宣布为
// 「在线且带若干点位」，Host 会一直采集一堆不存在的量。规范允许设备被
// 整体移除时发 DDEATH（op.adoc:736），这也是唯一能让 Host 模型收敛的手段。
func (s *sparkplugSink) onRemove(deviceID string) {
	s.publishDeath(deviceID, "removed")
}

// publishDeath 发一条 DDEATH 并忘掉设备的点位。
//
// reason 只用于指标标签，不进线上载荷：DDEATH 的载荷不带指标。
func (s *sparkplugSink) publishDeath(deviceID, reason string) {
	device := s.deviceName(deviceID)
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.closed {
		return
	}
	if _, ok := s.devices[device]; !ok {
		// 从没出生过的设备不需要死亡消息：Host 本来就不认识它，发一条
		// DDEATH 只会让严格实现报告「未知设备的死亡」。
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	msg, merr := s.node.DDEATH(device)
	if err := s.publishLocked(ctx, msg, merr); err != nil {
		// 设备状态不属于数据通路：失败只影响可观测性，下次重连会通过
		// DBIRTH 重建完整视图（而设备已经不在缓存里，不会被重发）。
		return
	}
	s.forgetDevice(device)
	s.count("death_" + reason)
}

// forgetDevice 删掉设备的点位表。调用方必须持有 pubMu。
func (s *sparkplugSink) forgetDevice(device string) {
	delete(s.devices, device)
	for i, id := range s.deviceOrder {
		if id == device {
			s.deviceOrder = append(s.deviceOrder[:i], s.deviceOrder[i+1:]...)
			return
		}
	}
}

// onAlarmEvent / seedAlarms：Sparkplug 没有报警主题。
//
// 报警是平台自有语义，硬塞进 DDATA 会让 Sparkplug 消费方看到一堆来路
// 不明的指标。需要报警请用 json 格式或 REST/SSE。
func (s *sparkplugSink) onAlarmEvent(alarm.Event)           {}
func (s *sparkplugSink) seedAlarms(statuses []alarm.Status) {}

// handleCommand 解析 NCMD/DCMD 的 Rebirth。
//
// 它在 MQTT 读循环里同步执行，必须立刻返回：真正的发布放到 goroutine，
// 否则一条慢 Broker 会卡住整个客户端的入站处理（包括 PUBACK）。
func (s *sparkplugSink) handleCommand(p mqtt.Publish) {
	cmd, ok := sparkplug.ParseCommand(p.Topic, p.Payload)
	if !ok || !cmd.Rebirth {
		return
	}
	go s.rebirth(cmd.Device)
}

func (s *sparkplugSink) rebirth(device string) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.closed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ensureSession(ctx); err != nil {
		s.failed++
		s.count("rebirth_error")
		return
	}
	if device == "" {
		// 节点级重生：重发 NBIRTH 与全部 DBIRTH（序号从 0 重新开始）。
		birth, berr := s.node.NBIRTH()
		if err := s.publishLocked(ctx, birth, berr); err != nil {
			return
		}
		for _, id := range s.deviceOrder {
			dev := s.devices[id]
			if dev == nil {
				continue
			}
			msg, merr := s.node.DBIRTH(id, dev.snapshot(nil))
			if err := s.publishLocked(ctx, msg, merr); err != nil {
				return
			}
		}
		return
	}
	dev := s.devices[device]
	if dev == nil || len(dev.order) == 0 {
		// 对端点名一个本节点不认识的设备：静默忽略是 Sparkplug 的约定。
		return
	}
	msg, merr := s.node.DBIRTH(device, dev.snapshot(nil))
	_ = s.publishLocked(ctx, msg, merr)
}

// close 先发 NDEATH，再断开。
//
// 正常停机时 Broker 不会替我们发遗嘱（发了 DISCONNECT 就抑制 LWT），所以
// 主动补一条是这个场景下唯一的死亡通知。随后 client.Close() 发 DISCONNECT。
//
// 注意顺序：先 NDEATH 再 DISCONNECT。反过来的话，DISCONNECT 一到 Broker
// 就认为会话正常结束，之后那条 NDEATH 会被当成一条普通消息——虽然 QoS1
// 仍能送达，但 Host 看到的「会话结束」早于「死亡消息」，严格实现会把它
// 当作迟到消息丢弃。
func (s *sparkplugSink) close() error {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	death, derr := s.node.NDEATH()
	_ = s.publishLocked(ctx, death, derr)
	return s.client.Close()
}
