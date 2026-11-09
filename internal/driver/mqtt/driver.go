// Package mqtt 实现 MQTT 3.1.1 南向驱动（订阅取数）。
//
// 这个驱动与其余所有驱动有一个根本差别：MQTT 是**推送型**协议，设备不会
// 因为「你问了一次」而回答。而驱动契约是拉取型（Read 立即返回一批样本）。
//
// 两者之间的转换只有一种做法是对的：**在连接建立时订阅，把收到的消息存进
// 本地最新值表，Read 从表里取值**。任何试图在 Read 里「发一条请求等回应」
// 的实现都会：
//
//  1. 在没有新消息时一直等到超时（采一个点耗时等于超时时间）；
//  2. 把 retained 的历史消息当成「刚刚发生的变化」；
//  3. 无法处理设备主动上报频率与采集周期不一致的情况。
//
// 由此带来的一条硬性约束：**MQTT 点位的质量码只能反映「消息新鲜度」**。
// 一个点位只要收到过消息、且未超过 staleAfter，就是 good；超过就是 stale；
// 从未收到过是 bad。这与寄存器型协议的「读到了就是 good」语义不同，
// 但它是这个协议下唯一诚实的表达。
//
// 能力边界：只支持 MQTT 3.1.1 明文 TCP（无 TLS/WebSocket）、QoS0/1
// （无 QoS2）、载荷为 UTF-8 文本或 JSON（无二进制协议绑定）。
package mqtt

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"uic/internal/codec/textval"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/mqtt"
)

// 默认参数。
const (
	// defaultStaleAfter 是「多久没收到消息就判 stale」的兜底值。正常情况
	// 由设备采集周期推导（3 倍周期），这样改周期时不用同时改这里。
	defaultStaleAfter = 30 * time.Second
	// defaultQoS 是订阅与写入共用的 QoS。
	//
	// 订阅侧用 1 而不是 0：QoS0 在网络抖动时会静默丢消息，而订阅驱动没有
	// 「补读」这回事——丢一条就意味着这个点位要等下一条才更新，在低频设备
	// 上表现为「数据长时间不动」。写入侧用 1 是因为控制命令不能丢。
	defaultQoS = 1
)

// Client 是驱动依赖的 MQTT 客户端能力。抽成接口是为了让测试能注入
// 一个「不联网的假客户端」——订阅驱动的核心逻辑是「消息进来后怎么存」，
// 没必要每次都起一个真 Broker。
type Client interface {
	Connect(ctx context.Context) error
	Ping(ctx context.Context) error
	Publish(ctx context.Context, m mqtt.Message) error
	Close() error
	Stats() mqtt.Stats
}

// Dialer 构造一个带回调的客户端。Options 由驱动填好订阅与回调。
type Dialer func(opt mqtt.Options) Client

// Driver 是 MQTT 南向驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(opt mqtt.Options) Client { return mqtt.New(opt) }}
}

// NewWithDialer 返回注入客户端的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明 MQTT 南向的能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:         "mqtt",
		Name:       "MQTT 3.1.1 (subscribe)",
		Version:    "0.1.0",
		SDKVersion: driver.SDKVersion,
		Roles:      []driver.Role{driver.RoleMaster},
		Transports: []string{"tcp"},
		// 地址的 path 是订阅主题，selector 是 JSON 载荷里的字段路径。
		// 用 symbol 而不是 register：主题不是「区 + 偏移」。
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read: driver.CapSupported,
			// 写入 = 向命令主题发布一条消息。是否真的能控设备取决于
			// 对端有没有订阅，驱动无法验证，因此这里声明 supported 但
			// 在文档里明确「发布成功 ≠ 设备已执行」。
			Write: driver.CapSupported,
			// 声明 supported 指的是「协议本身是推送型、由驱动内部订阅」，
			// 不是「驱动对外提供推送回调」。上层拿到的仍然是 Read 拉取
			// 契约，推送在同包内被消化成最新值表。若把这里改成
			// unsupported，配置界面会建议用户「换个协议」，方向就错了。
			Subscribe: driver.CapSupported,
			Browse:    driver.CapUnsupported,
			Discovery: driver.CapUnsupported,
			// 每次 Read 都在本地表里取，不存在「批量请求」的概念；
			// 声明 supported 是因为一次 Read 可以覆盖任意多点位且成本恒定。
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "Broker", Type: "string", Required: true,
				Placeholder: "127.0.0.1:1883", Scope: "channel",
				Help: "host:port 或 tcp://host:port；不支持 TLS/WebSocket"},
			{Name: "username", Label: "用户名", Type: "string", Scope: "channel"},
			{Name: "password", Label: "密码", Type: "string", Scope: "channel", Secret: true},
			{Name: "clientId", Label: "客户端 ID", Type: "string", Scope: "channel",
				Help: "留空自动生成；同 Broker 上必须唯一"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 2, Scope: "channel",
				Help: "订阅驱动读本地表，并发不产生协议交互"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"symbol"}, Scope: "point"},
			{Name: "address.path", Label: "订阅主题", Type: "string", Required: true,
				Placeholder: "plant/line1/temperature", Scope: "point",
				Help: "必须是精确主题（不支持 + / # 通配符）"},
			{Name: "address.selector", Label: "JSON 字段", Type: "string", Scope: "point",
				Placeholder: "data.temperature", Help: "留空表示整个载荷就是值"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64",
					"float32", "float64", "string"}, Scope: "point"},
		},
		Description: "MQTT 3.1.1 订阅取数（QoS0/1）。值来自最近一条消息，质量码反映消息新鲜度",
	}
}

// Open 建立连接、恢复订阅并开始收消息。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("mqtt requires tcp transport, got %q", t.Kind), false, nil)
	}
	if t.Address == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host:port)", false, nil)
	}

	points := make(map[string]*pointState, len(cfg.Device.Points))
	filterSet := map[string]bool{}
	var filters []mqtt.TopicFilter
	for _, p := range cfg.Device.Points {
		if !p.Enabled {
			continue
		}
		if p.Address.Kind != model.AddressSymbol {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
				fmt.Sprintf("point %q: mqtt requires symbol addresses, got %q", p.ID, p.Address.Kind), false, nil)
		}
		if err := validateTopic(p.Address.Path); err != nil {
			return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID,
				fmt.Sprintf("point %q: %v", p.ID, err), false, err)
		}
		if p.Writable && p.DataType == "" {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
				fmt.Sprintf("point %q: dataType required", p.ID), false, nil)
		}
		points[p.ID] = &pointState{
			topic:    p.Address.Path,
			selector: p.Address.Selector,
			dataType: p.DataType,
		}
		if !filterSet[p.Address.Path] {
			filterSet[p.Address.Path] = true
			filters = append(filters, mqtt.TopicFilter{Filter: p.Address.Path, QoS: defaultQoS})
		}
	}
	if len(points) == 0 {
		// 一个点位都没有的设备不需要连接；但也不能静默返回成功，
		// 否则「配置写错导致 0 点位」会表现为「设备在线但没数据」。
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"device has no enabled points", false, nil)
	}

	c := &conn{
		deviceID:   cfg.Device.ID,
		points:     points,
		staleAfter: staleAfterFor(cfg.Device),
		values:     make(map[string]cachedValue, len(points)),
		opened:     time.Now(),
	}

	opt := mqtt.Options{
		Addr:           t.Address,
		ClientID:       clientID(cfg.Channel.ID, cfg.Device.ID),
		Username:       cfg.Channel.Username,
		Password:       cfg.Channel.Password,
		KeepAlive:      30 * time.Second,
		ConnectTimeout: cfg.Channel.EffectiveTimeout(),
		AckTimeout:     cfg.Channel.EffectiveTimeout(),
		Subscriptions:  filters,
		OnMessage:      c.onMessage,
		OnConnect:      c.onConnect,
	}
	client := d.dial(opt)
	c.client = client

	if err := client.Connect(ctx); err != nil {
		_ = client.Close()
		return nil, driver.E(driver.ErrDisconnected, "open", cfg.Device.ID,
			"mqtt connect", true, err)
	}
	return c, nil
}

// validateTopic 校验点位主题。
//
// 禁止通配符是刻意的：一个点位声明 `plant/+/temp`，收到的每条消息都会
// 命中它，「这个点位的值」就失去了唯一含义——上一次是 1 号线还是 2 号线？
// 想要多主题请配多个点位，每个用精确主题。订阅报文里仍然一次带全部精确
// 主题，不会增加往返。
func validateTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("address.path (topic) required")
	}
	if strings.ContainsAny(topic, "+#") {
		return fmt.Errorf("topic %q must not contain MQTT wildcards (+ / #); use one point per exact topic", topic)
	}
	if len(topic) > 65535 {
		return fmt.Errorf("topic too long (%d bytes)", len(topic))
	}
	return nil
}

// staleAfterFor 由设备采集周期推导新鲜度阈值。
//
// 3 倍周期是经验值：小于 2 倍时，一次网络抖动就会让整页点位变 stale；
// 大于 5 倍时，设备真的停了要等太久才告警。
func staleAfterFor(dev model.Device) time.Duration {
	interval := dev.Interval.Duration()
	if interval <= 0 {
		return defaultStaleAfter
	}
	d := 3 * interval
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// clientID 生成稳定的客户端 ID。
//
// 必须稳定（不能每次重连变）：Broker 用 ClientID 识别会话，变化的 ID 会
// 让 Broker 认为「新客户端」并触发旧连接的遗嘱消息，消费方会看到设备在
// 反复上下线。
func clientID(channelID, deviceID string) string {
	id := "uic-" + channelID + "-" + deviceID
	if len(id) > 100 {
		id = id[:100]
	}
	return id
}

// pointState 是一个点位的静态配置。
type pointState struct {
	topic    string
	selector string
	dataType model.DataType
}

// cachedValue 是某个点位最近一次收到的值。
type cachedValue struct {
	value model.Value
	at    time.Time
}

// conn 是一个 MQTT 南向连接。
type conn struct {
	deviceID string
	client   Client

	points     map[string]*pointState
	staleAfter time.Duration
	opened     time.Time

	mu     sync.Mutex
	values map[string]cachedValue
	// msgs / badPayloads 是排障计数：「收到了但解析不了」和「一条都没
	// 收到」是两种完全不同的故障，混成一个数字就分不清了。
	msgs        uint64
	badPayloads uint64
	// lastMsgAt 是最近一条（任何主题的）消息到达时刻。它用来判断「安静」
	// 是否已经久到值得做一次链路自检。
	lastMsgAt time.Time
	closed    bool
}

func (c *conn) onConnect() {
	// 重连成功即代表订阅已恢复（客户端在 Connect 里同步完成 SUBSCRIBE）。
	// 这里不重置已缓存的值：短暂断线后继续用旧值比清空更有用，新鲜度
	// 由 staleAfter 自然降级。
}

// onMessage 在读循环里同步执行，必须快速返回。
func (c *conn) onMessage(p mqtt.Publish) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.msgs++
	// 按主题筛出候选点位，再逐个解码。点位数量在几十的量级，线性扫一遍
	// 比维护「主题 → 点位列表」的二级索引更省心，也不会漏掉新增点位。
	now := time.Now()
	c.lastMsgAt = now
	for id, ps := range c.points {
		if ps.topic != p.Topic {
			continue
		}
		v, err := decodePayload(p.Payload, ps)
		if err != nil {
			// 载荷解析失败不能把上一次的好值覆盖成坏值：同一个主题上很
			// 可能还有状态/日志类消息。保留旧值，让它按 staleAfter 自然
			// 降级；解析失败的条数单独计数，供排障区分「没收到」与
			// 「收到了但解不出」。
			c.badPayloads++
			continue
		}
		c.values[id] = cachedValue{value: v, at: now}
	}
	c.mu.Unlock()
}

// Probe 做一次真正的往返探测。
//
// 订阅驱动没有「读一个寄存器」这种可探动作，PINGREQ/PINGRESP 是唯一能证明
// 「Broker 还在说话」的信号。不能只看连接标志：拔网线时 TCP 不会立刻报错，
// 写进内核缓冲也会返回成功，只有等不到 PINGRESP 才能判定链路已死。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "connection closed", true, nil)
	}
	if err := c.client.Ping(ctx); err != nil {
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "mqtt ping", true, err)
	}
	return nil
}

// Read 从本地最新值表取数。
//
// 这里**没有任何网络交互**：所有值都来自订阅回调。Read 的耗时与点位数量、
// 网络质量无关，「设备安静」也不会让它超时。
//
// 一条容易搞错的地方：**消息没到不等于链路坏了**，所以这里在链路正常时不
// 返回整体错误。如果因为「还没收到消息」就返回 timeout，采集层会按「断链」
// 处理，把连接拆了重建——于是每一轮都在重连、重新订阅，而设备那边根本没
// 有故障。真正断链时（客户端不在连接状态且重连失败）才返回 ErrDisconnected。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.deviceID, "connection closed", true, nil)
	}
	// 先确认链路：断了要按断链上抛（上层随后重建连接），其余情况一律
	// 「本地取数 + 逐点质量码」。
	if !c.client.Stats().Connected {
		if err := c.client.Connect(ctx); err != nil {
			return nil, driver.E(driver.ErrDisconnected, "read", c.deviceID, "mqtt reconnect", true, err)
		}
	}

	now := time.Now()
	out := make([]driver.RawSample, 0, len(spans))

	c.mu.Lock()
	goodCount := 0
	quietSince := now
	if !c.lastMsgAt.IsZero() {
		quietSince = c.lastMsgAt
	} else if !c.opened.IsZero() {
		quietSince = c.opened
	}
	for _, sp := range spans {
		for _, p := range sp.Points {
			s := c.lookupLocked(p.ID, now)
			if s.Quality == model.QualityGood {
				goodCount++
			}
			out = append(out, s)
		}
	}
	c.mu.Unlock()

	// 链路自检：一条好消息都没有、而且已经安静得超过一个新鲜度窗口时，
	// 主动做一次 PINGREQ 往返。
	//
	// 为什么必须这么做：拔网线、对端断电、NAT 表项过期这几种情况下 TCP
	// 不会立刻报错，本地状态仍然是「已连接」，但再也不会有消息进来。没有
	// 这一步，设备会一直显示在线、所有点位停在 stale，直到有人重启进程。
	// 只在「已经安静到该怀疑」时才探测，避免设备正常低频上报时每轮都打
	// 一次多余的 PING。
	if goodCount == 0 && now.Sub(quietSince) > c.staleAfter {
		if err := c.client.Ping(ctx); err != nil {
			return out, driver.E(driver.ErrDisconnected, "read", c.deviceID,
				"link is silent and ping failed", true, err)
		}
	}
	return out, nil
}

// lookupLocked 取单个点位的当前样本。调用方必须持有 c.mu。
//
// 抽成独立方法是为了让「新鲜度 → 质量码」这条规则可以脱离连接单独测试：
// 它不涉及任何 I/O，是三档判定唯一的事实来源。
func (c *conn) lookupLocked(id string, now time.Time) driver.RawSample {
	ps, ok := c.points[id]
	if !ok {
		return driver.RawSample{
			PointID: id, Quality: model.QualityAddressError,
			Err: driver.E(driver.ErrAddress, "read", c.deviceID,
				fmt.Sprintf("point %q is not subscribed", id), false, nil),
		}
	}
	cv, seen := c.values[id]
	if !seen {
		return driver.RawSample{
			PointID: id, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.deviceID,
				fmt.Sprintf("no message received on %q yet", ps.topic), true, nil),
		}
	}
	if now.Sub(cv.at) > c.staleAfter {
		// 有过值但太久没更新：值仍然保留，质量降为 stale。清空值会让趋势图
		// 出现断点，运维也看不出「最后是多少」。
		return driver.RawSample{
			PointID: id, Value: cv.value, Quality: model.QualityStale,
			Err: driver.E(driver.ErrTimeout, "read", c.deviceID,
				fmt.Sprintf("last message on %q was %s ago", ps.topic,
					now.Sub(cv.at).Truncate(time.Second)), true, nil),
		}
	}
	return driver.RawSample{PointID: id, Value: cv.value, Quality: model.QualityGood}
}

// Write 向点位主题发布一条消息。
//
// 「发布成功」只代表 Broker 接收了（QoS1 等到 PUBACK），**不代表设备
// 已经执行**。真正的确认需要另一个反馈主题，那不在这层契约里。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, driver.E(driver.ErrDisconnected, "write", c.deviceID, "connection closed", true, nil)
	}

	results := make([]driver.WriteResult, 0, len(reqs))
	for _, r := range reqs {
		ps, ok := c.points[r.PointID]
		if !ok {
			results = append(results, driver.WriteResult{
				PointID: r.PointID, OK: false, Quality: model.QualityAddressError,
				Err: driver.E(driver.ErrAddress, "write", c.deviceID,
					fmt.Sprintf("point %q is not configured", r.PointID), false, nil),
			})
			continue
		}
		payload, err := encodePayload(r.Value, r.DataType)
		if err != nil {
			results = append(results, driver.WriteResult{
				PointID: r.PointID, OK: false, Quality: model.QualityBad,
				Err: driver.E(driver.ErrUnsupported, "write", c.deviceID, err.Error(), false, err),
			})
			continue
		}
		// 写入主题：约定与订阅主题相同。现场也有「读一个主题、写另一个
		// 主题」的约定（如 .../stat 与 .../cmd），那需要点位单独声明
		// 写主题——本版不做，避免引入一个只有少数场景用得上的字段。
		err = c.client.Publish(ctx, mqtt.Message{
			Topic:   ps.topic,
			Payload: payload,
			QoS:     defaultQoS,
		})
		if err != nil {
			results = append(results, driver.WriteResult{
				PointID: r.PointID, OK: false, Quality: model.QualityCommError,
				Err: driver.E(driver.ErrDisconnected, "write", c.deviceID, "mqtt publish", true, err),
			})
			continue
		}
		results = append(results, driver.WriteResult{
			PointID: r.PointID, OK: true, Quality: model.QualityGood,
		})
	}
	return results, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.client.Close()
}

// ---------------------------------------------------------------------------
// 载荷解码
// ---------------------------------------------------------------------------

// decodePayload 把一条消息载荷解成平台值。
//
// 支持两种形态：
//
//   - 纯文本：`"21.5"` / `"true"` / `"RUN"`，整个载荷就是值；
//   - JSON：`{"temperature":21.5}`，按 selector 取字段。
//
// 判断依据是「selector 是否为空」而不是「载荷像不像 JSON」：一个纯文本
// 载荷恰好是 `123`，按 JSON 解析会得到数字，按文本解析会得到 "123"，
// 两者对 string 点位的结果不同。由配置显式决定，比猜更可靠。
//
// 具体的类型规则在 codec/textval（CoAP 也用它），这里只负责选对入口。
func decodePayload(payload []byte, ps *pointState) (model.Value, error) {
	raw := payload
	if ps.selector != "" {
		v, err := textval.Select(payload, ps.selector)
		if err != nil {
			return model.Value{}, err
		}
		raw = v
	}
	return textval.Decode(raw, ps.dataType)
}

// encodePayload 把平台值编成发布载荷。
func encodePayload(v model.Value, dt model.DataType) ([]byte, error) {
	return textval.Encode(v, dt)
}
