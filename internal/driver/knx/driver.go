// Package knx 实现 KNXnet/IP 隧道（TUNNELLING）主站驱动：组地址读写。
//
// 分层与其它驱动一致：Meta（能力声明）→ 本文件的会话层（CONNECT / 序号 /
// 心跳 / 重连）→ codec/knx（KNXnet/IP 信封、cEMI、组地址与 DPT 的纯函数
// 编解码）→ transport（UDP 数据报）。
//
// 五个刻意的边界：
//
//  1. **只做 TUNNELING（UDP 单播隧道），不做 ROUTING（组播）。** 路由模式
//     没有通道号、没有序号、也收不到 L_Data.con——看起来更简单，但它把
//     「丢包」与「设备没响应」合并成同一件事，而现场最需要的恰恰是把这两
//     件事故分开：前者重试有用，后者重试一万次也没用。组播路由还要求驱动
//     监听固定端口 3671，同一台服务器装两个实例就互抢。
//
//  2. **只做组地址读写（GroupValueRead / Response / Write），不做设备管理。**
//     设备管理连接（CONNECTION_TYPE=DEVICE_MANAGEMENT）是工程工具下载参数
//     用的属性通道，与「采集点位」是两套语义：它没有组地址，只有属性编号，
//     而且错误的写入会改掉执行器的物理配置。混进一个采集驱动里是事故源。
//
//  3. **不做 KNX Secure（SECURE_WRAPPER）。** 会话密钥协商是独立子系统，
//     外加一份密钥库；在没有密钥管理的前提下「顺手支持一下」只会让人以为
//     链路是加密的。
//
//  4. **DPT 必须由点表显式声明，驱动绝不从 dataType 反推。** 组地址不携带
//     类型：同一个 `1/2/3` 在 A 工程里是 1 位开关、在 B 工程里是 2 字节浮点
//     温度。从 dataType 反推会得到「float32 到底是 9.xxx 还是 14.xxx」这种
//     无法回答的问题，而猜错不报错——只会把 21.5 读成 3.7e-41。
//
//  5. **串行化整条会话。** KNX 隧道只有一个通道号与一组 8 位序号，网关按
//     「收到一帧、回一个 ACK、再收下一帧」工作。并发发两个 TUNNELLING_REQUEST
//     会让序号交叉、ACK 张冠李戴，而且总线本身就是 9600 波特——并发在这里
//     没有任何收益。
//
// 序号与 ACK 是这套协议里最容易做错的一处，因此单独说清楚：客户端每发一帧
// TUNNELLING_REQUEST，序号加一；**加一的时机是收到 ACK 之后，不是发出之后**。
// 超时重传用的仍是同一个序号（网关看到重复序号会重发 ACK 而不是重复执行）。
// 把序号在「发送后」就加一，遇到一次丢包，网关就会把后续所有帧当成序号错乱
// 而拒绝，表现是「前一分钟好好的，丢一个包之后整条链路不再出声」。
package knx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	knxcodec "uic/internal/codec/knx"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 抽象 UDP 建链，测试可注入内存数据报链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error)

// 设备级默认值与取值范围。
const (
	defaultAckTimeout = time.Second
	minAckTimeout     = 100 * time.Millisecond
	maxAckTimeout     = 10 * time.Second

	// defaultMaxRetries 是一帧 TUNNELLING_REQUEST 在等不到 ACK 时的重传
	// 次数。KNX 建议 1 秒超时、最多重试 3 次；这里默认 2 次，因为重试的
	// 目的只是吃掉偶发丢包，而「网关整个不在」用 3 次只换来 3 秒的等待。
	defaultMaxRetries = 2
	maxRetriesLimit   = 5

	// defaultHeartbeat 是 CONNECTIONSTATE_REQUEST 的间隔。
	//
	// 网关在 120 秒内收不到任何报文就会回收通道号。心跳取 60 秒，留一半
	// 余量：网络抖动或一次慢响应都能吃掉几十秒。间隔设得比 120 秒大等于
	// 「每隔一段时间就被网关踢一次」，所以上限直接卡在 60 秒。
	defaultHeartbeat = 60 * time.Second
	minHeartbeat     = time.Second
	maxHeartbeat     = 60 * time.Second

	// confirmWindow 是写入之后等待链路层确认（L_Data.con）的窗口。
	//
	// 这个确认能回答一个很实际的问题：「帧已经发到总线，但没有任何设备
	// 应答」。它不是协议强制要求客户端等待的，所以超时就按成功处理——
	// 有一部分网关根本不给隧道客户端回 con，把它们判成写失败会让这些
	// 网关完全不可用。
	confirmWindow = 300 * time.Millisecond

	// disconnectWait 是 Close 时等 DISCONNECT_RESPONSE 的窗口。等不到也
	// 照样关：网关会在 120 秒后自己回收通道，不值得为一个善后报文阻塞关闭。
	disconnectWait = 300 * time.Millisecond
)

// Driver 是 KNXnet/IP 主站驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 UDP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return transport.DialUDP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	return &Driver{dial: d}
}

// Meta 声明 KNXnet/IP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "knx",
		Name:         "KNXnet/IP",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"udp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:  driver.CapSupported,
			Write: driver.CapSupported,
			// 订阅要长期持有 TUNNELING 连接并把总线上的 L_Data.ind 主动
			// 分发给订阅者。KNX 确实支持，但那是一条独立的生命周期（订阅
			// 失效、网关重启后要重建），与本平台的调度式采集是两种模型，
			// 混在一起会让超时、重连、死区三套逻辑互相干扰。
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapUnsupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "网关地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:3671", Scope: "channel",
				Help: "KNXnet/IP 网关/接口地址；省略端口时按 3671 处理"},
			{Name: "labels.heartbeat", Label: "心跳间隔", Type: "duration",
				Default: "60s", Scope: "device",
				Help: "CONNECTIONSTATE 请求间隔；网关 120 秒收不到报文就会回收通道，因此限制在 1s..60s"},
			{Name: "labels.ackTimeout", Label: "隧道 ACK 超时", Type: "duration",
				Default: "1s", Scope: "device",
				Help: "等 TUNNELLING_ACK 的时长；超时会用同一序号重传，必须 ≤ device.timeout"},
			{Name: "labels.maxRetries", Label: "重传次数", Type: "int",
				Default: defaultMaxRetries, Scope: "device",
				Help: "0..5；只用于吃偶发丢包，重传仍失败会重建隧道"},
			{Name: "labels.sourceAddress", Label: "源物理地址", Type: "string",
				Scope: "device", Placeholder: "15.15.250",
				Help: "隧道客户端在总线上使用的物理地址；留空表示使用网关在 CONNECT_RESPONSE 里分配的地址。" +
					"只有不分配地址的网关才需要手工指定"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"symbol"}, Scope: "point"},
			{Name: "address.path", Label: "组地址", Type: "string", Required: true,
				Scope: "point", Placeholder: "1/2/3",
				Help: "主组/中间组/子组（0..31 / 0..7 / 0..255），也接受两级写法 1/300 或原始 16 位值"},
			{Name: "address.dpt", Label: "数据点类型（DPT）", Type: "string", Required: true,
				Scope: "point", Placeholder: "9.001",
				Help: "必填：组地址本身不携带类型，必须声明怎么解释字节（如 1.001 开关、9.001 温度、5.001 百分比）"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int8", "uint8", "int16", "uint16", "int32", "uint32",
					"float32", "float64"},
				Scope: "point",
				Help:  "只声明平台侧怎么存；线宽与取值范围由 DPT 决定"},
		},
		Description: "KNXnet/IP 隧道主站：组地址读写（TUNNELING over UDP 3671，DPT 由点表声明）",
	}
}

// settings 是设备级可调参数。
type settings struct {
	heartbeat  time.Duration
	ackTimeout time.Duration
	maxRetries int
	source     knxcodec.IndividualAddress
	hasSource  bool
}

func parseSettings(cfg driver.OpenConfig) (settings, error) {
	st := settings{
		heartbeat:  defaultHeartbeat,
		ackTimeout: defaultAckTimeout,
		maxRetries: defaultMaxRetries,
	}
	labels := cfg.Device.Labels
	if v := strings.TrimSpace(labels["heartbeat"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return st, fmt.Errorf("labels.heartbeat %q: %w", v, err)
		}
		if d < minHeartbeat || d > maxHeartbeat {
			return st, fmt.Errorf("labels.heartbeat %q: must be within %s..%s (the gateway reclaims an idle channel after 120s)",
				v, minHeartbeat, maxHeartbeat)
		}
		st.heartbeat = d
	}
	if v := strings.TrimSpace(labels["ackTimeout"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return st, fmt.Errorf("labels.ackTimeout %q: %w", v, err)
		}
		if d < minAckTimeout || d > maxAckTimeout {
			return st, fmt.Errorf("labels.ackTimeout %q: must be within %s..%s", v, minAckTimeout, maxAckTimeout)
		}
		st.ackTimeout = d
	}
	if st.ackTimeout > cfg.Device.TimeoutOrDefault() {
		// ACK 超时比设备预算还长，每次都还没等到 ACK 就被 ctx 取消：
		// 配置看起来「更宽容」，实际只会更慢、更难排查。
		return st, fmt.Errorf("labels.ackTimeout %s exceeds device.timeout %s; raise device.timeout or lower ackTimeout",
			st.ackTimeout, cfg.Device.TimeoutOrDefault())
	}
	if v := strings.TrimSpace(labels["maxRetries"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return st, fmt.Errorf("labels.maxRetries %q: not an integer", v)
		}
		if n < 0 || n > maxRetriesLimit {
			return st, fmt.Errorf("labels.maxRetries %d: must be 0..%d", n, maxRetriesLimit)
		}
		st.maxRetries = n
	}
	if v := strings.TrimSpace(labels["sourceAddress"]); v != "" {
		ia, err := knxcodec.ParseIndividualAddress(v)
		if err != nil {
			return st, fmt.Errorf("labels.sourceAddress %q: %w", v, err)
		}
		if ia == 0 {
			return st, fmt.Errorf("labels.sourceAddress %q: 0.0.0 is not a usable source address", v)
		}
		st.source = ia
		st.hasSource = true
	}
	return st, nil
}

// pointPlan 是一个点位解析后的组地址寻址。
type pointPlan struct {
	ref driver.PointRef
	ga  knxcodec.GroupAddress
	dpt knxcodec.DPT
}

// Open 解析点位并建立 UDP 套接字。
//
// 隧道（CONNECT_REQUEST）不在这里建：Open 只有一次机会，而网关的隧道槽位
// 往往是满的（工程组态工具、上一个进程的残留通道、同网段的另一台采集机）。把建隧道
// 放进 Open，等于让「网关刚好满 4 个通道」变成「驱动加载失败」，而实际上
// 过一会儿就能连上。真正的连接由第一次 Probe/Read/Write 触发，失败可以重试。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "udp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("knx requires udp transport, got %q", t.Kind), false, nil)
	}
	addr, err := knxcodec.HostPort(t.Address)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	st, err := parseSettings(cfg)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	plans, byID, err := buildPlans(cfg.Device)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID, err.Error(), false, err)
	}
	link, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	return &conn{
		devID:   cfg.Device.ID,
		addr:    addr,
		link:    link,
		timeout: cfg.Device.TimeoutOrDefault(),
		st:      st,
		log:     cfg.Log,
		points:  plans,
		byID:    byID,
	}, nil
}

func buildPlans(dev model.Device) ([]pointPlan, map[string]pointPlan, error) {
	plans := make([]pointPlan, 0, len(dev.Points))
	byID := make(map[string]pointPlan, len(dev.Points))
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		plan, err := planPoint(driver.PointRefOf(dev, p))
		if err != nil {
			return nil, nil, err
		}
		if prev, dup := byID[plan.ref.ID]; dup {
			return nil, nil, fmt.Errorf("point %q: duplicate id (also used by %s)", plan.ref.ID, prev.ga)
		}
		plans = append(plans, plan)
		byID[plan.ref.ID] = plan
	}
	return plans, byID, nil
}

func planPoint(ref driver.PointRef) (pointPlan, error) {
	if ref.Address.Kind != model.AddressSymbol {
		return pointPlan{}, fmt.Errorf("point %q: knx requires symbol addressing (a group address such as 1/2/3), got %q",
			ref.ID, ref.Address.Kind)
	}
	ga, err := knxcodec.ParseGroupAddress(ref.Address.Path)
	if err != nil {
		return pointPlan{}, fmt.Errorf("point %q: %w", ref.ID, err)
	}
	// Selector 与 Length 在 KNX 里没有对应概念：选择器是「取载荷的哪一段」
	// （MQTT 的 JSON 字段），长度由 DPT 决定。配置里写了它们说明点表是
	// 从别的协议抄过来的，必须报错而不是悄悄忽略。
	if s := strings.TrimSpace(ref.Address.Selector); s != "" {
		return pointPlan{}, fmt.Errorf("point %q: address.selector %q has no meaning in knx (the type belongs in address.dpt)",
			ref.ID, s)
	}
	// Index 只拦「明确写了数组下标」的情况：JSON 里省略 index 时零值就是
	// 0，与「第 0 个元素」无法区分，因此 0 放行、正数报错。
	if ref.Address.Index > 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.index %d has no meaning in knx (group addresses have no array index)",
			ref.ID, ref.Address.Index)
	}
	if ref.Address.Length > 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.length %d: the width comes from the datapoint type (address.dpt)",
			ref.ID, ref.Address.Length)
	}
	dpt, err := knxcodec.ParseDPT(ref.Address.DPT)
	if err != nil {
		return pointPlan{}, fmt.Errorf("point %q: %w", ref.ID, err)
	}
	if !ref.DataType.Valid() || ref.DataType == "" {
		return pointPlan{}, fmt.Errorf("point %q: invalid dataType %q", ref.ID, ref.DataType)
	}
	if err := dpt.Accepts(ref.DataType); err != nil {
		return pointPlan{}, fmt.Errorf("point %q (%s): %w", ref.ID, ga, err)
	}
	return pointPlan{ref: ref, ga: ga, dpt: dpt}, nil
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

// localAddrer 由真实 UDP 链路实现；测试用的内存链路可以不实现。
type localAddrer interface{ LocalAddr() string }

type conn struct {
	devID   string
	addr    string
	timeout time.Duration
	st      settings
	log     *slog.Logger

	// mu 串行化整条会话（见包注释第 5 条）。
	mu     sync.Mutex
	link   transport.Datagram
	closed bool

	// 隧道状态。
	connected bool
	channel   uint8
	seq       uint8
	source    knxcodec.IndividualAddress

	// lastActivity 是最近一次成功收发的时间，用于判断是否需要心跳。
	lastActivity time.Time
	// inbox 暂存本事务里已收到、尚未被判定的 cEMI 报文。网关可能先回
	// 响应再回 ACK，丢掉任何一侧都会让判定变成偶发失败。
	inbox []knxcodec.LData
	// lastFrame 是最近一帧 TUNNELLING_REQUEST，供超时重传。
	lastFrame []byte

	points []pointPlan
	byID   map[string]pointPlan

	// badDatagrams 记录解析失败的报文数，供诊断观察。它不影响事务：
	// UDP 上收到别人的包或被截断的包都不该让当前请求失败。
	badDatagrams uint64
}

// Probe 验证网关可达、隧道能建起来且通道有效。
//
// 刻意不读某个组地址：KNX 工程里「这个组地址上恰好没有设备」是常态，而且
// 新装的系统在调试期总线上可能一台设备都没有。把 Probe 定义成「读通一个
// 值时算活」，会让一半以上的正常网关被判成故障。总线级的活性由每个点位
// 自己的超时报告——那里有地址，能说出是哪一个没应答。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", true, nil)
	}
	if err := c.ensure(ctx); err != nil {
		return err
	}
	return c.checkState(ctx, "probe", true)
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.connected {
		c.disconnect(disconnectWait)
	}
	link := c.link
	c.link = nil
	if link != nil {
		return link.Close()
	}
	return nil
}

// ensure 保证隧道可用：必要时建链、建隧道、发心跳。
func (c *conn) ensure(ctx context.Context) error {
	if c.closed {
		return driver.E(driver.ErrDisconnected, "open", c.devID, "connection closed", true, nil)
	}
	if c.link == nil {
		if err := c.redial(ctx); err != nil {
			return err
		}
	}
	if !c.connected {
		return c.connect(ctx)
	}
	if time.Since(c.lastActivity) >= c.st.heartbeat {
		return c.checkState(ctx, "heartbeat", false)
	}
	return nil
}

func (c *conn) redial(ctx context.Context) error {
	link, err := transport.DialUDP(ctx, c.addr, c.timeout)
	if err != nil {
		return driver.E(driver.ErrDisconnected, "dial", c.devID,
			fmt.Sprintf("connect %s failed", c.addr), true, err)
	}
	c.link = link
	c.connected = false
	return nil
}

// localHPAI 返回「网关往哪儿回包」的地址。
//
// 优先用本端套接字的实际地址。全零 HPAI（route-back）在协议里是合法的，
// 表示「按本报文源地址回送」，但一部分网关会把它当成「客户端不接收」而
// 直接拒绝建隧道——而客户端自己又拿不到「网关为什么拒绝」的任何信息。
// 只有在链路报不出本端地址时（内存链路）才退回 route-back。
func (c *conn) localHPAI() knxcodec.HPAI {
	if la, ok := c.link.(localAddrer); ok {
		if h, err := knxcodec.NewHPAI(la.LocalAddr()); err == nil {
			return h
		}
	}
	return knxcodec.HPAI{Protocol: knxcodec.ProtoUDP4}
}

// connect 发送 CONNECT_REQUEST 并等待 CONNECT_RESPONSE。
func (c *conn) connect(ctx context.Context) error {
	local := c.localHPAI()
	payload := knxcodec.ConnectRequest{
		Control: local,
		Data:    local,
		Layer:   knxcodec.LayerLink,
	}.Encode()
	if err := c.writeFrame(knxcodec.SvcConnectRequest, payload, "connect"); err != nil {
		return err
	}
	deadline := c.deadline(ctx)
	if err := c.link.SetDeadline(deadline); err != nil {
		return c.linkError("connect", "set udp deadline", err)
	}
	for {
		f, err := c.readFrame(ctx, "connect")
		if err != nil {
			return err
		}
		switch f.Service {
		case knxcodec.SvcConnectResponse:
			resp, perr := knxcodec.ParseConnectResponse(f.Payload)
			if perr != nil {
				return driver.E(driver.ErrProtocol, "connect", c.devID,
					"malformed CONNECT_RESPONSE: "+perr.Error(), false, perr)
			}
			if resp.Status != knxcodec.StatusNoError {
				return c.connectStatusError(resp)
			}
			c.establish(resp)
			// 建链期设的 deadline 会被后续读循环继承；清零，让每个阶段
			// 自己决定等多久。
			if err := c.link.SetDeadline(time.Time{}); err != nil {
				return c.linkError("connect", "clear udp deadline", err)
			}
			c.debug("tunnel established", "channel", resp.Channel, "assigned", resp.Assigned.String())
			return nil
		case knxcodec.SvcTunnellingRequest:
			// 建链之前不该有隧道报文；真收到了也不能 ACK（没有通道号），
			// 直接丢弃并继续等。
			continue
		default:
			continue
		}
	}
}

func (c *conn) establish(resp knxcodec.ConnectResponse) {
	c.channel = resp.Channel
	c.seq = 0
	c.connected = true
	c.inbox = c.inbox[:0]
	c.lastActivity = time.Now()
	// 源地址的优先级：点表/设备上的显式配置 > 网关在 CRD 里分配的地址 >
	// 0.0.0（由网关替换成它自己的地址）。
	//
	// 0.0.0 不是「错地址」：隧道模式下这个字段是「客户端希望使用的地址」，
	// 网关可以改写；不分配地址（响应里没有 CRD）的网关基本都会填自己的。
	// 反过来，如果点表写了 sourceAddress 却被网关改写，日志里能看到
	// CONNECT 之后的第一帧用了哪个地址。
	c.source = c.st.source
	if !c.st.hasSource && resp.HasCRD {
		c.source = resp.Assigned
	}
	if resp.Data.RouteBack() {
		// 网关说「往报文源地址回」：我们用的是已连接的 UDP 套接字，报文
		// 天然发往同一个对端，什么都不用做。
		return
	}
	if got, want := resp.Data.String(), c.addr; got != want {
		// 真实网关偶尔会给出「数据端点与控制端点不同」的地址（分离端口的
		// 接口设备）。已连接套接字发不到别处，这里只记录，不尝试改路由：
		// 强行改用 WriteTo 会让「源地址过滤」这一类网关直接丢包。
		c.debug("gateway announced a separate data endpoint; using the connected socket",
			"data", got, "socket", want)
	}
}

// connectStatusError 把 CONNECT_RESPONSE 的状态码翻成驱动错误。
func (c *conn) connectStatusError(resp knxcodec.ConnectResponse) error {
	msg := fmt.Sprintf("gateway refused the tunnel: 0x%02X %s", resp.Status, knxcodec.StatusName(resp.Status))
	switch resp.Status {
	case knxcodec.StatusNoMoreConnections, knxcodec.StatusNoMoreUniqueConnections,
		knxcodec.StatusDataConnection, knxcodec.StatusKNXConnection:
		// 槽位满、地址耗尽、总线侧暂时不可用：过一会儿重试是有意义的，
		// 而配置本身没错，不该把设备判成「地址错误」。
		return driver.E(driver.ErrDisconnected, "connect", c.devID, msg, true, nil)
	}
	if resp.Channel != 0 {
		// 网关分配了通道号却报了错：按协议必须回 DISCONNECT 把通道还回去，
		// 否则这个槽位会一直占着，直到 120 秒后自然超时。
		c.channel = resp.Channel
		c.connected = true
		c.disconnect(disconnectWait)
	}
	return driver.E(driver.ErrProtocol, "connect", c.devID, msg, false, nil)
}

// checkState 发一次 CONNECTIONSTATE_REQUEST。
//
// force=false 时只在确实空闲够久时才发（由 ensure 调度）；force=true 用于
// Probe 与显式检查。
func (c *conn) checkState(ctx context.Context, op string, force bool) error {
	if !force && time.Since(c.lastActivity) < c.st.heartbeat {
		return nil
	}
	payload := knxcodec.ConnectionStateRequest{Channel: c.channel, Control: c.localHPAI()}.Encode()
	if err := c.writeFrame(knxcodec.SvcConnectionStateReq, payload, op); err != nil {
		return err
	}
	deadline := c.deadline(ctx)
	if err := c.link.SetDeadline(deadline); err != nil {
		return c.linkError(op, "set udp deadline", err)
	}
	for {
		f, err := c.readFrame(ctx, op)
		if err != nil {
			// 心跳失败不代表链路已断（可能只是丢了这一个包），但也不能
			// 继续假装隧道可用：下一次 ensure 会重建。
			c.dropTunnel()
			return err
		}
		switch f.Service {
		case knxcodec.SvcConnectionStateRes:
			resp, perr := knxcodec.ParseConnectionStateResponse(f.Payload)
			if perr != nil {
				return driver.E(driver.ErrProtocol, op, c.devID,
					"malformed CONNECTIONSTATE_RESPONSE: "+perr.Error(), false, perr)
			}
			if resp.Status != knxcodec.StatusNoError {
				c.dropTunnel()
				if resp.Status == knxcodec.StatusConnectionID {
					return driver.E(driver.ErrDisconnected, op, c.devID,
						"gateway no longer knows this tunnel (channel reclaimed); it will be re-established", true, nil)
				}
				return driver.E(driver.ErrProtocol, op, c.devID,
					fmt.Sprintf("gateway rejected CONNECTIONSTATE: 0x%02X %s",
						resp.Status, knxcodec.StatusName(resp.Status)), false, nil)
			}
			c.lastActivity = time.Now()
			if err := c.link.SetDeadline(time.Time{}); err != nil {
				return c.linkError(op, "clear udp deadline", err)
			}
			return nil
		case knxcodec.SvcTunnellingRequest:
			if err := c.acceptInbound(op, f.Payload); err != nil {
				return err
			}
		case knxcodec.SvcDisconnectRequest:
			c.answerServerDisconnect(f.Payload)
			c.dropTunnel()
			return driver.E(driver.ErrDisconnected, op, c.devID, "gateway closed the tunnel", true, nil)
		case knxcodec.SvcConnectionStateReq:
			c.answerStateRequest(f.Payload, op)
		default:
			continue
		}
	}
}

// disconnect 尽力把通道还给网关。
func (c *conn) disconnect(wait time.Duration) {
	link := c.link
	if link == nil {
		c.dropTunnel()
		return
	}
	payload := knxcodec.DisconnectRequest{Channel: c.channel, Control: c.localHPAI()}.Encode()
	if err := c.writeFrame(knxcodec.SvcDisconnectRequest, payload, "close"); err != nil {
		c.dropTunnel()
		return
	}
	if err := link.SetDeadline(time.Now().Add(wait)); err != nil {
		c.dropTunnel()
		return
	}
	// readFrame 在读写失败时会把 c.link 置空（dropLink），因此这里必须持有
	// 建链时的局部引用，不能用 c.link——否则「网关不回 DISCONNECT_RESPONSE」
	// 这条最常见的路径会变成对 nil 的 SetDeadline 崩掉。
	defer func() { _ = link.SetDeadline(time.Time{}) }()
	for {
		f, err := c.readFrame(context.Background(), "close")
		if err != nil {
			break
		}
		if f.Service == knxcodec.SvcDisconnectResponse {
			break
		}
		if f.Service == knxcodec.SvcTunnellingRequest {
			if err := c.acceptInbound("close", f.Payload); err != nil {
				break
			}
		}
	}
	c.dropTunnel()
}

// dropTunnel 忘记当前通道与序号，下一次 ensure 会重新 CONNECT。
func (c *conn) dropTunnel() {
	c.connected = false
	c.channel = 0
	c.seq = 0
	c.inbox = c.inbox[:0]
}

// dropLink 连套接字一起丢掉：读写本身出错时，套接字状态已经不可信。
func (c *conn) dropLink() {
	c.dropTunnel()
	if c.link != nil {
		_ = c.link.Close()
		c.link = nil
	}
}

// ---------------------------------------------------------------------------
// 收发
// ---------------------------------------------------------------------------

func (c *conn) writeFrame(service uint16, payload []byte, op string) error {
	if c.link == nil {
		return driver.E(driver.ErrDisconnected, op, c.devID, "link is down", true, nil)
	}
	if err := c.link.Write(knxcodec.BuildFrame(service, payload)); err != nil {
		c.dropLink()
		return c.linkError(op, "udp write failed", err)
	}
	return nil
}

// readFrame 读一帧 KNXnet/IP。
//
// 无法解析的报文直接跳过：UDP 上会收到同网段的广播发现报文、别的进程的
// 残留包、被截断的包。把它们变成当前事务的失败，会让「网上有别人」成为
// 本设备的故障。
func (c *conn) readFrame(ctx context.Context, op string) (knxcodec.Frame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return knxcodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, err)
		}
		if c.link == nil {
			return knxcodec.Frame{}, driver.E(driver.ErrDisconnected, op, c.devID, "link is down", true, nil)
		}
		raw, err := c.link.Read()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return knxcodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
			}
			if isTimeout(err) {
				return knxcodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "no datagram within timeout", true, err)
			}
			c.dropLink()
			return knxcodec.Frame{}, c.linkError(op, "udp read failed", err)
		}
		c.lastActivity = time.Now()
		f, perr := knxcodec.ParseFrame(raw)
		if perr != nil {
			c.badDatagrams++
			continue
		}
		return f, nil
	}
}

// acceptInbound 处理网关发来的 TUNNELLING_REQUEST：ACK 它，并把 cEMI 收进
// inbox。ACK 必须无条件发出——网关也在等 ACK，我们不发，它就不会再往下推，
// 而响应很可能正是被它压在后面的那一帧。
func (c *conn) acceptInbound(op string, payload []byte) error {
	req, err := knxcodec.ParseTunnellingRequest(payload)
	if err != nil {
		c.badDatagrams++
		return nil
	}
	status := byte(knxcodec.StatusNoError)
	if req.Channel != c.channel {
		status = knxcodec.StatusConnectionID
	}
	ack := knxcodec.TunnellingACK{Channel: req.Channel, Seq: req.Seq, Status: status}.Encode()
	if err := c.writeFrame(knxcodec.SvcTunnellingACK, ack, op); err != nil {
		return err
	}
	if status != knxcodec.StatusNoError {
		return nil
	}
	m, perr := knxcodec.ParseLData(req.CEMI)
	if perr != nil {
		c.badDatagrams++
		return nil
	}
	// 上限 64 条：一轮读只产生寥寥几条，超过说明对面在灌总线上别人的
	// 报文；留着不判定的报文只会让后面的等待从垃圾开始。
	if len(c.inbox) < 64 {
		c.inbox = append(c.inbox, m)
	}
	return nil
}

func (c *conn) answerStateRequest(payload []byte, op string) {
	req, err := knxcodec.ParseConnectionStateRequest(payload)
	if err != nil {
		c.badDatagrams++
		return
	}
	status := byte(knxcodec.StatusNoError)
	if !c.connected || req.Channel != c.channel {
		status = knxcodec.StatusConnectionID
	}
	resp := knxcodec.ConnectionStateResponse{Channel: req.Channel, Status: status}.Encode()
	_ = c.writeFrame(knxcodec.SvcConnectionStateRes, resp, op)
}

func (c *conn) answerServerDisconnect(payload []byte) {
	req, err := knxcodec.ParseDisconnectRequest(payload)
	if err != nil {
		c.badDatagrams++
		return
	}
	status := byte(knxcodec.StatusNoError)
	if req.Channel != c.channel {
		status = knxcodec.StatusConnectionID
	}
	resp := knxcodec.DisconnectResponse{Channel: req.Channel, Status: status}.Encode()
	_ = c.writeFrame(knxcodec.SvcDisconnectResponse, resp, "gateway-disconnect")
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// waitFunc 判定一帧 cEMI 是否就是本次事务要等的东西。
type waitFunc func(knxcodec.LData) (done bool, err error)

// exchange 发一帧 L_Data.req，等它的 ACK，再等 wait 认可的总线报文。
//
// wait 为 nil 表示不等总线报文（写操作只要链路层确认）。
func (c *conn) exchange(ctx context.Context, op string, cemi []byte, wait waitFunc) error {
	if c.link == nil {
		return driver.E(driver.ErrDisconnected, op, c.devID, "link is down", true, nil)
	}
	c.inbox = c.inbox[:0]
	seq := c.seq
	c.lastFrame = knxcodec.BuildFrame(knxcodec.SvcTunnellingRequest,
		knxcodec.TunnellingRequest{Channel: c.channel, Seq: seq, CEMI: cemi}.Encode())
	if err := c.sendLast(ctx, c.st.ackTimeout); err != nil {
		return err
	}
	if err := c.awaitACK(ctx, op, seq); err != nil {
		return err
	}
	if wait == nil {
		// 等链路层确认（L_Data.con）。等不到（网关不给隧道客户端回 con）
		// 按成功处理：一部分网关就是这样实现的，把它们判成写失败会让这些
		// 网关完全不可用。但**收到**且「确认有错」时不能静默：那是「帧发
		// 出去了，总线上没人接」——最典型的配置错误。
		window := confirmWindow
		if window > c.st.ackTimeout {
			window = c.st.ackTimeout
		}
		err := c.awaitValue(ctx, op, window, func(m knxcodec.LData) (bool, error) {
			if m.MsgCode != knxcodec.MsgLDataCon {
				return false, nil
			}
			if m.ConfirmationError() {
				// 帧上了总线，但链路层报发送失败（总线忙、冲突、重复发送、
				// 执行器离线）。注意这不是「没人订阅这个组地址」——后者在
				// 链路层是成功的，见包注释。
				return true, driver.E(driver.ErrProtocol, op, c.devID,
					"the frame reached the gateway but the bus-level confirmation reports a failure "+
						"(bus busy, collision, or no device acknowledged at link layer)", false, nil)
			}
			return true, nil
		})
		if err != nil && driver.KindOf(err) != driver.ErrTimeout {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
		}
		return nil
	}
	return c.awaitValue(ctx, op, c.timeout, wait)
}

// sendLast 发送 lastFrame，并把读截止时间设成 window 内（同时不超过 ctx
// 与设备超时）。
//
// 等 ACK 的窗口必须比设备总超时短：网关没反应时，我们希望在 ackTimeout
// 之后立刻重传，而不是把整个设备预算都耗在第一次发送上。等总线应答用
// 满设备预算——那是另一段时间，语义也不同。
func (c *conn) sendLast(ctx context.Context, window time.Duration) error {
	if c.link == nil {
		return driver.E(driver.ErrDisconnected, "send", c.devID, "link is down", true, nil)
	}
	deadline := c.deadline(ctx)
	if d := time.Now().Add(window); d.Before(deadline) {
		deadline = d
	}
	if err := c.link.SetDeadline(deadline); err != nil {
		return c.linkError("send", "set udp deadline", err)
	}
	if err := c.link.Write(c.lastFrame); err != nil {
		c.dropLink()
		return c.linkError("send", "udp write failed", err)
	}
	return nil
}

// awaitACK 等这一帧的 TUNNELLING_ACK，等不到就重传同一序号。
func (c *conn) awaitACK(ctx context.Context, op string, seq uint8) error {
	for retry := 0; ; {
		f, err := c.readFrame(ctx, op)
		if err != nil {
			if driver.KindOf(err) != driver.ErrTimeout || ctx.Err() != nil {
				return err
			}
			if retry >= c.st.maxRetries {
				// 序号已经和网关对不上（它可能收到了但 ACK 丢了），隧道
				// 无法继续使用：重建比猜序号安全得多。
				c.dropTunnel()
				return driver.E(driver.ErrTimeout, op, c.devID,
					fmt.Sprintf("no TUNNELLING_ACK after %d attempt(s); tunnel reset", retry+1), true, err)
			}
			retry++
			if err := c.sendLast(ctx, c.st.ackTimeout); err != nil {
				return err
			}
			continue
		}
		switch f.Service {
		case knxcodec.SvcTunnellingACK:
			ack, perr := knxcodec.ParseTunnellingACK(f.Payload)
			if perr != nil {
				c.badDatagrams++
				continue
			}
			if ack.Channel != c.channel || ack.Seq != seq {
				// 上一条请求迟到的 ACK。序号是 8 位、会绕回，所以只能按
				// 当前在途的序号比对，不能按大小比较。
				continue
			}
			if ack.Status != knxcodec.StatusNoError {
				return c.ackError(op, ack.Status)
			}
			// 序号在 ACK 之后才加一（见包注释）。
			c.seq++
			return nil
		case knxcodec.SvcTunnellingRequest:
			if err := c.acceptInbound(op, f.Payload); err != nil {
				return err
			}
		case knxcodec.SvcDisconnectRequest:
			c.answerServerDisconnect(f.Payload)
			c.dropTunnel()
			return driver.E(driver.ErrDisconnected, op, c.devID, "gateway closed the tunnel", true, nil)
		case knxcodec.SvcConnectionStateReq:
			c.answerStateRequest(f.Payload, op)
		default:
			continue
		}
	}
}

func (c *conn) ackError(op string, status uint8) error {
	msg := fmt.Sprintf("gateway rejected TUNNELLING_REQUEST: 0x%02X %s", status, knxcodec.StatusName(status))
	switch status {
	case knxcodec.StatusSequenceNumber:
		// 序号错乱说明两端的计数器已经分叉。任何一次「猜」都可能让网关
		// 执行一条我们不打算发的报文，唯一可靠的复位方式是重建隧道。
		c.dropTunnel()
		return driver.E(driver.ErrProtocol, op, c.devID,
			msg+"; the tunnel will be re-established so the sequence numbers resynchronise", true, nil)
	case knxcodec.StatusConnectionID:
		c.dropTunnel()
		return driver.E(driver.ErrDisconnected, op, c.devID,
			msg+"; the tunnel will be re-established", true, nil)
	}
	return driver.E(driver.ErrProtocol, op, c.devID, msg, false, nil)
}

// awaitValue 在 window 内等待 wait 认可的总线报文。
//
// 先扫 inbox：网关完全可能在 ACK 之前就把响应推过来（先回响应再回 ACK 的
// 实现并不少见），此时再读只会等到超时。
func (c *conn) awaitValue(ctx context.Context, op string, window time.Duration, wait waitFunc) error {
	deadline := c.deadline(ctx)
	if d := time.Now().Add(window); d.Before(deadline) {
		deadline = d
	}
	if err := c.link.SetDeadline(deadline); err != nil {
		return c.linkError(op, "set udp deadline", err)
	}
	defer func() { _ = c.link.SetDeadline(time.Time{}) }()

	for {
		// 先消化 inbox。
		rest := c.inbox[:0]
		for _, m := range c.inbox {
			done, err := wait(m)
			if err != nil {
				c.inbox = rest
				return err
			}
			if done {
				return nil
			}
			rest = append(rest, m)
		}
		c.inbox = rest

		f, err := c.readFrame(ctx, op)
		if err != nil {
			return err
		}
		switch f.Service {
		case knxcodec.SvcTunnellingRequest:
			if err := c.acceptInbound(op, f.Payload); err != nil {
				return err
			}
		case knxcodec.SvcDisconnectRequest:
			c.answerServerDisconnect(f.Payload)
			c.dropTunnel()
			return driver.E(driver.ErrDisconnected, op, c.devID, "gateway closed the tunnel", true, nil)
		case knxcodec.SvcConnectionStateReq:
			c.answerStateRequest(f.Payload, op)
		default:
			// ACK、心跳响应、别人的报文：与本次等待无关。
			continue
		}
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", true, nil)
	}
	var refs []driver.PointRef
	for _, span := range spans {
		refs = append(refs, span.Points...)
	}
	out := make([]driver.RawSample, 0, len(refs))
	var firstErr error
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			for _, rest := range refs[i:] {
				out = append(out, errSample(rest.ID, e))
			}
			if firstErr == nil {
				firstErr = e
			}
			break
		}
		plan, ok := c.byID[ref.ID]
		if !ok {
			var err error
			plan, err = planPoint(ref)
			if err != nil {
				e := driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
				out = append(out, errSample(ref.ID, e))
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
		}
		if err := c.ensure(ctx); err != nil {
			out = append(out, errSample(ref.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			for _, rest := range refs[i+1:] {
				out = append(out, errSample(rest.ID, err))
			}
			break
		}
		v, err := c.readOne(ctx, plan)
		if err != nil {
			out = append(out, errSample(ref.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			// 链路级失败时剩下的点不可能成功，继续逐个等超时只会把一轮
			// 采集拖成 N 倍时长。地址类错误不影响其它点，继续。
			if k := driver.KindOf(err); k == driver.ErrTimeout || k == driver.ErrDisconnected {
				for _, rest := range refs[i+1:] {
					out = append(out, errSample(rest.ID, err))
				}
				break
			}
			continue
		}
		out = append(out, driver.RawSample{PointID: ref.ID, Value: v, Quality: model.QualityGood})
	}
	return out, firstErr
}

func (c *conn) readOne(ctx context.Context, plan pointPlan) (model.Value, error) {
	cemi, err := knxcodec.BuildLData(knxcodec.MsgLDataReq, uint16(c.source), uint16(plan.ga), true,
		knxcodec.APCIGroupValueRead, nil, false)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrInternal, "read", c.devID, err.Error(), false, err)
	}
	var payload knxcodec.Payload
	var got bool
	err = c.exchange(ctx, "read", cemi, func(m knxcodec.LData) (bool, error) {
		if !m.IsGroup || m.Dest != uint16(plan.ga) {
			return false, nil
		}
		switch m.APCI {
		case knxcodec.APCIGroupValueResponse:
		case knxcodec.APCIGroupValueRead, knxcodec.APCIGroupValueWrite:
			// 总线上别的设备在同一时刻发起读/写（例如按钮按下时面板广播
			// 一个写）：它不是对我们这次读的回答。
			return false, nil
		default:
			return false, nil
		}
		p, derr := knxcodec.DecodeGroupValue(m, plan.dpt)
		if derr != nil {
			// 线格式与点表声明的 DPT 不符：多半是点表把类型写错了。
			// 这是配置问题，不是总线故障，所以判成地址错误。
			return true, driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("point %q (%s, dpt %s): %v", plan.ref.ID, plan.ga, plan.dpt, derr), false, derr)
		}
		payload, got = p, true
		return true, nil
	})
	if err != nil {
		if driver.KindOf(err) == driver.ErrTimeout {
			return model.Value{}, driver.E(driver.ErrTimeout, "read", c.devID,
				fmt.Sprintf("point %q (%s, dpt %s): no GroupValueResponse (no device answers this group address, or the gateway did not forward the bus frame)",
					plan.ref.ID, plan.ga, plan.dpt), true, err)
		}
		return model.Value{}, err
	}
	if !got {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("point %q (%s): read completed without a value", plan.ref.ID, plan.ga), false, nil)
	}
	v, err := plan.dpt.Decode(payload, plan.ref.DataType)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrAddress, "read", c.devID,
			fmt.Sprintf("point %q (%s, dpt %s): %v", plan.ref.ID, plan.ga, plan.dpt, err), false, err)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "write", c.devID, "connection closed", true, nil)
	}
	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for i, req := range reqs {
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if err != nil {
			if k := driver.KindOf(err); k == driver.ErrTimeout || k == driver.ErrDisconnected {
				for _, rest := range reqs[i+1:] {
					results = append(results, writeFail(rest, err))
				}
				break
			}
		}
	}
	return results, firstErr
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	fail := func(kind driver.ErrorKind, msg string, temporary bool, cause error) (driver.WriteResult, error) {
		e := driver.E(kind, "write", c.devID, fmt.Sprintf("point %q: %s", req.PointID, msg), temporary, cause)
		return driver.WriteResult{PointID: req.PointID, Quality: qualityFor(kind), Err: e}, e
	}
	plan, err := planPoint(driver.PointRef{
		ID: req.PointID, Address: req.Address, DataType: req.DataType, ByteOrder: req.ByteOrder,
	})
	if err != nil {
		return fail(driver.ErrAddress, err.Error(), false, err)
	}
	if err := c.ensure(ctx); err != nil {
		return fail(driver.KindOf(err), "tunnel is not available", driver.IsRetryable(err), err)
	}

	payload, err := plan.dpt.Encode(req.Value, req.DataType)
	if err != nil {
		return fail(driver.ErrAddress, err.Error(), false, err)
	}
	cemi, err := knxcodec.BuildLData(knxcodec.MsgLDataReq, uint16(c.source), uint16(plan.ga), true,
		knxcodec.APCIGroupValueWrite, payload.Data, payload.Bit)
	if err != nil {
		return fail(driver.ErrInternal, err.Error(), false, err)
	}
	if err := c.exchange(ctx, "write", cemi, nil); err != nil {
		kind := driver.KindOf(err)
		if kind == driver.ErrTimeout {
			return fail(kind, fmt.Sprintf("no TUNNELLING_ACK for the write to %s", plan.ga), true, err)
		}
		return fail(kind, err.Error(), driver.IsRetryable(err), err)
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// deadline 返回本次等待的截止时间：设备超时与 ctx 截止取较早者。
func (c *conn) deadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.timeout)
	if d2, ok := ctx.Deadline(); ok && d2.Before(d) {
		return d2
	}
	return d
}

func (c *conn) linkError(op, msg string, err error) error {
	return driver.E(driver.ErrDisconnected, op, c.devID, msg, true, err)
}

func (c *conn) debug(msg string, args ...any) {
	if c.log != nil {
		c.log.Debug("knx: "+msg, args...)
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityFor(driver.KindOf(err)), Err: err}
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityFor(driver.KindOf(err)), Err: err}
}

func qualityFor(kind driver.ErrorKind) model.Quality {
	switch kind {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrAddress:
		return model.QualityAddressError
	default:
		return model.QualityBad
	}
}
