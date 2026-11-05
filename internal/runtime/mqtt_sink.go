package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"uic/internal/alarm"
	"uic/internal/config"
	"uic/internal/mqtt"
	"uic/internal/obs"
	"uic/internal/outbox"
	"uic/internal/scheduler"
)

// mqttSink 把 Outbox 里的变化事件发布到 MQTT。
//
// 投递语义是 at-least-once：QoS1 且必须等到 PUBACK 才返回成功。乐观地
// 「发出去就算成功」会让 Outbox 推进位点，一旦 Broker 没收到，那条数据
// 就永久丢了——对一个「断网不丢数据」的产品来说这是不可接受的。
type mqttSink struct {
	client *mqtt.Client
	// topicTemplate 形如 uic/{site}/{device}/{point}/value。
	topicTemplate string
	site          string
	qos           byte
	retain        bool

	// 设备状态的最后已知值。Broker 重启会丢掉 retained 消息，重连后要
	// 靠这份缓存把当前状态补发回去，否则消费方会一直看到旧状态。
	stateMu sync.Mutex
	states  map[string]sinkState

	// 报警是「当前状态」而不是数据流：发布时不进 Outbox，走 retained，
	// 并在（重）连后整体重发。积压一条过期报警毫无价值，而漏掉「当前正在
	// 报警」是致命的。
	alarmMu     sync.Mutex
	alarmStates map[string]alarm.Status
	// alarmCleared 记录「规则已删除但清空消息还没发出去」的 ID。断线时
	// 删除规则会留下一个 retained 幽灵，必须在重连后补发空载荷清掉。
	alarmCleared map[string]bool

	met *obs.Metrics

	published atomic.Uint64
	failed    atomic.Uint64
}

type sinkState struct {
	State    string    `json:"state"`
	Previous string    `json:"previous,omitempty"`
	At       time.Time `json:"at"`
	Error    string    `json:"error,omitempty"`
}

func newMQTTSink(cfg config.MQTT, site, gatewayID string) (*mqttSink, error) {
	if cfg.Broker == "" {
		return nil, fmt.Errorf("northbound.mqtt.broker required when mqtt.enabled")
	}
	tmpl := cfg.Topic
	if tmpl == "" {
		tmpl = defaultMQTTTopic
	}
	if !strings.Contains(tmpl, "{device}") || !strings.Contains(tmpl, "{point}") {
		return nil, fmt.Errorf("northbound.mqtt.topic %q must contain {device} and {point}", tmpl)
	}

	clientID := cfg.ClientID
	if clientID == "" {
		// 默认按站点命名：同一站点多实例连同一 Broker 会互相顶掉会话，
		// 这种情况必须显式配 clientId 区分，而不是让两个进程无声打架。
		clientID = gatewayID
	}
	s := &mqttSink{
		topicTemplate: tmpl,
		site:          site,
		qos:           byte(cfg.EffectiveQoS()),
		retain:        cfg.Retain,
		states:        map[string]sinkState{},
		alarmStates:   map[string]alarm.Status{},
		alarmCleared:  map[string]bool{},
	}
	s.client = mqtt.New(mqtt.Options{
		Addr:      cfg.Broker,
		ClientID:  clientID,
		Username:  cfg.Username,
		Password:  cfg.Password,
		KeepAlive: 30 * time.Second,
		// 必须小于 Outbox 单轮 drain 的 5s 预算：否则「Broker 收了但不确认」
		// 会先撞上外层 context 超时，日志里只剩 context deadline exceeded，
		// 看不出到底是网络慢还是 Broker 没回 PUBACK。
		AckTimeout: 3 * time.Second,
		// Broker 在网关异常掉线时代发 $death（retained），消费方因此不必
		// 依赖超时猜测网关是否在线。
		WillTopic:   fmt.Sprintf("uic/%s/$death", sanitizeTopicLevel(site)),
		WillPayload: []byte(`{"schemaVersion":1,"online":false}`),
		WillQoS:     1,
		WillRetain:  true,
		// 重连（含 Broker 重启）后补发 birth 与全部设备状态。
		OnConnect: func() { s.republish(context.Background()) },
	})
	return s, nil
}

// defaultMQTTTopic 与 docs/03-product/05-northbound-api-spec.md §6.1 一致。
const defaultMQTTTopic = "uic/{site}/{device}/{point}/value"

// topicFor 按模板渲染主题。device / point 来自 Sample 主键，因此天然满足
// 「设备内有序、跨设备不保证」的契约。
func (m *mqttSink) topicFor(device, point string) string {
	r := strings.NewReplacer(
		"{site}", sanitizeTopicLevel(m.site),
		"{device}", sanitizeTopicLevel(device),
		"{point}", sanitizeTopicLevel(point),
	)
	return r.Replace(m.topicTemplate)
}

// sanitizeTopicLevel 清掉 MQTT 主题层级分隔符与通配符。
//
// 设备 ID 来自配置文件，理论上受 idRe 约束；但这里是最后一道闸门：
// 一个含 '/' 的 ID 会让消息悄悄跑到别的主题上（甚至被别人的 ACL 放行），
// 而发送方完全看不出问题。
func sanitizeTopicLevel(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '+', '#', 0:
			return '_'
		}
		return r
	}, s)
}

// deliver 实现 deliverFunc 契约。
//
// 逐条发布而不是把整批塞进一条消息：北向契约是「一条 Sample 一条消息」，
// 这样消费方可以按主题路由，也便于 Broker 做 ACL 与限流。
//
// 前缀语义：一旦某条失败就停止，返回已成功的条数。Outbox 只会推进到
// 最后一个成功的位点，因此失败之后的消息会在下一轮重发，不会跳号。
func (m *mqttSink) deliver(ctx context.Context, recs []outbox.Record) (int, error) {
	done := 0
	for _, rec := range recs {
		device, point := splitKey(rec.Key)
		topic := m.topicFor(device, point)
		msg := mqtt.Message{
			Topic:   topic,
			Payload: rec.Data,
			QoS:     m.qos,
			Retain:  m.retain,
		}
		if err := m.client.Publish(ctx, msg); err != nil {
			m.failed.Add(1)
			m.count("error")
			return done, fmt.Errorf("mqtt publish %s: %w", topic, err)
		}
		m.published.Add(1)
		m.count("ok")
		done++
	}
	return done, nil
}

// onState 把设备状态迁移发布为 retained 消息。
//
// retained 是这里的核心：新接入的消费方（比如刚打开的看板）不需要等下一次
// 状态变化，连上就能读到每台设备当前是否在线。状态是「当前事实」而非事件流，
// 用 retained 表达比要求消费方回放历史更可靠。
func (m *mqttSink) onState(ev scheduler.StateEvent) {
	st := sinkState{State: string(ev.To), Previous: string(ev.From), At: ev.At.UTC(), Error: ev.Error}
	m.stateMu.Lock()
	m.states[ev.DeviceID] = st
	m.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.publishState(ctx, ev.DeviceID, st); err != nil {
		// 状态消息不是数据通路：失败只影响可观测性，不影响采集与补发。
		// 下次状态变化或重连会重新发布。
		m.failed.Add(1)
		m.count("state_error")
	}
}

func (m *mqttSink) publishState(ctx context.Context, device string, st sinkState) error {
	payload, err := json.Marshal(struct {
		SchemaVersion int `json:"schemaVersion"`
		sinkState
	}{1, st})
	if err != nil {
		return err
	}
	topic := fmt.Sprintf("uic/%s/%s/$state", sanitizeTopicLevel(m.site), sanitizeTopicLevel(device))
	return m.client.Publish(ctx, mqtt.Message{
		Topic: topic, Payload: payload, QoS: m.qos, Retain: true,
	})
}

// republish 在（重）连成功后补发 birth 与已知设备状态。
func (m *mqttSink) republish(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	payload, _ := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"online":        true,
		"startedAt":     time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err := m.client.Publish(ctx, mqtt.Message{
		Topic:   fmt.Sprintf("uic/%s/$birth", sanitizeTopicLevel(m.site)),
		Payload: payload, QoS: m.qos, Retain: true,
	}); err != nil {
		return
	}

	m.stateMu.Lock()
	snapshot := make(map[string]sinkState, len(m.states))
	for k, v := range m.states {
		snapshot[k] = v
	}
	m.stateMu.Unlock()

	for device, st := range snapshot {
		if err := m.publishState(ctx, device, st); err != nil {
			return
		}
	}
	// 报警状态同样要补：Broker 重启会丢掉 retained，而「网关重连后看板上
	// 的报警全部消失」比没有报警更危险。
	m.resendAlarms(ctx)
}

func (m *mqttSink) count(result string) {
	if m.met != nil {
		m.met.Inc("uic_mqtt_publish_total", obs.Labels{"result": result})
	}
}

// close 先补发一条 retained 的 $death，再断开连接。
//
// 正常停机时 Broker 不会触发 LWT（因为我们发了 DISCONNECT），而 $birth 是
// retained 的：不补这一条，消费方会一直看到 online=true，直到有人手动清理
// retained。运维视角下「网关已经停了但监控说在线」比没有状态更糟。
func (m *mqttSink) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"schemaVersion": 1, "online": false})
	_ = m.client.Publish(ctx, mqtt.Message{
		Topic:   fmt.Sprintf("uic/%s/$death", sanitizeTopicLevel(m.site)),
		Payload: payload, QoS: m.qos, Retain: true,
	})
	return m.client.Close()
}

// splitKey 把 Outbox 的 key（device/point）拆成两段。
func splitKey(key string) (device, point string) {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, "_"
}

// String 便于日志展示。注意不要打印用户名/密码。
func (m *mqttSink) String() string {
	return "mqtt(" + m.topicTemplate + ",qos=" + strconv.Itoa(int(m.qos)) + ")"
}

// ---------------------------------------------------------------------------
// 报警出口
// ---------------------------------------------------------------------------

// alarmTopic 是单条报警的状态主题。用 `$alarm` 前缀把控制面（`$state`/
// `$birth`）与数据面（`uic/{site}/{device}/{point}/value`）分开：消费方
// 订阅 `uic/{site}/#` 取数据时不会把报警混进去。
func (m *mqttSink) alarmTopic(id string) string {
	return fmt.Sprintf("uic/%s/$alarm/%s", sanitizeTopicLevel(m.site), sanitizeTopicLevel(id))
}

// onAlarmEvent 把一次状态迁移发布为 retained 消息。
func (m *mqttSink) onAlarmEvent(ev alarm.Event) {
	id := ev.Status.ID
	m.alarmMu.Lock()
	if ev.Type == alarm.EventRemoved {
		delete(m.alarmStates, id)
		m.alarmCleared[id] = true
	} else {
		m.alarmStates[id] = ev.Status
	}
	m.alarmMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.publishAlarm(ctx, ev.Status, ev.Type == alarm.EventRemoved); err != nil {
		// 与设备状态一致：报警发布失败只影响可观测性，不能影响采集。
		// 重连时会整体重发，所以这里不回滚内存中的状态。
		m.failed.Add(1)
		m.count("alarm_error")
	}
}

// seedAlarms 用引擎的当前快照初始化本地缓存（启动时调用一次）。
func (m *mqttSink) seedAlarms(statuses []alarm.Status) {
	m.alarmMu.Lock()
	defer m.alarmMu.Unlock()
	if m.alarmStates == nil {
		m.alarmStates = map[string]alarm.Status{}
	}
	for _, st := range statuses {
		m.alarmStates[st.ID] = st
	}
}

// resendAlarms 在连接建立后重发全部报警状态与待清理的幽灵记录。
func (m *mqttSink) resendAlarms(ctx context.Context) {
	m.alarmMu.Lock()
	snapshot := make([]alarm.Status, 0, len(m.alarmStates))
	for _, st := range m.alarmStates {
		snapshot = append(snapshot, st)
	}
	cleared := make([]string, 0, len(m.alarmCleared))
	for id := range m.alarmCleared {
		cleared = append(cleared, id)
	}
	m.alarmMu.Unlock()

	for _, id := range cleared {
		if err := m.publishAlarmID(ctx, id, nil); err != nil {
			return
		}
		m.alarmMu.Lock()
		delete(m.alarmCleared, id)
		m.alarmMu.Unlock()
	}
	for _, st := range snapshot {
		if err := m.publishAlarm(ctx, st, false); err != nil {
			return
		}
	}
}

func (m *mqttSink) publishAlarm(ctx context.Context, st alarm.Status, remove bool) error {
	var payload []byte
	if !remove {
		b, err := json.Marshal(struct {
			SchemaVersion int `json:"schemaVersion"`
			alarm.Status
		}{1, st})
		if err != nil {
			return err
		}
		payload = b
	}
	return m.publishAlarmID(ctx, st.ID, payload)
}

// publishAlarmID 发布一条报警消息；payload 为 nil 表示清空 retained。
//
// MQTT 规范里「零长度 retained 载荷」就是删除该主题的 retained 消息。
// 这是唯一能让已删规则从消费方视野里彻底消失的方式。
func (m *mqttSink) publishAlarmID(ctx context.Context, id string, payload []byte) error {
	if err := m.client.Publish(ctx, mqtt.Message{
		Topic: m.alarmTopic(id), Payload: payload, QoS: m.qos, Retain: true,
	}); err != nil {
		return err
	}
	m.published.Add(1)
	m.count("alarm")
	return nil
}
