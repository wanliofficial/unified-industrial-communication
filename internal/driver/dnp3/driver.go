// Package dnp3 实现 DNP3（IEEE 1815）主站驱动。
//
// 与 Modbus 这类「问一句答一句」的驱动不同，DNP3 的数据通路有两条：
//
//  1. 主站发起总召唤（integrity poll），从站回一份静态值快照；
//  2. 从站把变化压进事件缓冲区，主站轮询类 1/2/3 取走，或由从站主动
//     发非请求响应（unsolicited response）。
//
// 因此 Read 和 IEC 104 一样，**不做任何网络交互**：它只读本地最新值表。
// 试图在 Read 里发一条「读这个点」的请求在 DNP3 里没有对应的协议动作——
// 能发的只有类轮询，而类轮询返回的是整类数据，代价与点位数量无关，也与
// 「读一个点」无关。
//
// 三个必须记住的协议事实：
//
//   - **应用层有确认**。带事件数据的响应会置 CON 位，主站必须回一条
//     application confirm，否则从站会一直重发同一批事件并把缓冲区撑爆。
//     这里的确认由接收循环统一处理，业务代码看不到。
//   - **序号有两层**。传输层 6 位（分片用），应用层 4 位（请求/响应配对）。
//     两层都回绕，不能用一个计数器混着算。
//   - **质量位不是布尔值**。ONLINE 为 0、COMM_LOST 为 1、RESTART 为 1
//     都表示「这个值不能用」，但含义不同：离线与通信丢失是 bad，重启、
//     强制、越限是 uncertain。把它们统一成 good 会让断线后的旧值冒充
//     新值——这是工业网关最危险的一类假数据。
//
// 能力边界：只做 TCP 主站、类 0 总召唤、类 1/2/3 事件轮询、直接控制
// （CROB 与模拟量输出）、非请求响应的接收与确认；不做串口、不做
// Secure Authentication、不做文件传输、不做时间同步写路径、不做
// 选择-执行（SBO）两段式控制（直接控制已覆盖绝大多数现场）。
package dnp3

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dnp3codec "uic/internal/codec/dnp3"
	"uic/internal/driver"
	"uic/internal/model"
)

// DefaultPort 是 DNP3 的注册端口。
const DefaultPort = 20000

// 地址区名。与配置里的 address.area 一一对应。
const (
	AreaBinaryInput     = "binary-input"
	AreaDoubleBitInput  = "double-bit-input"
	AreaBinaryOutput    = "binary-output"
	AreaCounter         = "counter"
	AreaFrozenCounter   = "frozen-counter"
	AreaAnalogInput     = "analog-input"
	AreaAnalogOutput    = "analog-output"
	maxObserved         = 4096
	defaultMasterAddr   = 1024
	defaultEventPoll    = time.Second
	defaultIntegrityPol = 5 * time.Minute
)

// Dialer 便于测试注入内存连接。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error)

// Driver 是 DNP3 主站驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		return c, nil
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	drv := New()
	drv.dial = d
	return drv
}

// Meta 声明 DNP3 的能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "dnp3",
		Name:         "DNP3 (IEEE 1815) master",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read: driver.CapSupported,
			// 直接控制（CROB / 模拟量输出）等到从站的响应才返回成功。
			Write: driver.CapSupported,
			// 连接时激活非请求响应（类 1/2/3）+ 事件轮询 + 总召唤共同填充
			// 本地表；从站不支持非请求响应时自动退回纯轮询。上层仍是拉取契约。
			Subscribe:   driver.CapSupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.20:20000", Scope: "channel",
				Help: "从站地址；省略端口时按 20000 处理"},
			{Name: "device.unitId", Label: "从站链路地址", Type: "int", Default: 1, Scope: "device",
				Help: "DNP3 链路层的目的地址（1..65519）"},
			{Name: "labels.masterAddress", Label: "主站链路地址", Type: "int", Default: 1024,
				Scope: "device", Help: "链路层的源地址；同一链路上必须唯一"},
			{Name: "labels.eventPoll", Label: "事件轮询周期", Type: "duration",
				Default: "1s", Scope: "device",
				Help: "类 1/2/3 的轮询间隔；从站有事件时才返回数据"},
			{Name: "labels.integrityPoll", Label: "总召唤周期", Type: "duration",
				Default: "5m", Scope: "device",
				Help: "类 0 静态值的刷新间隔；事件丢失时靠它兜底"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"register"}, Scope: "point"},
			{Name: "address.area", Label: "对象类别", Type: "enum", Required: true,
				Enum: []string{AreaBinaryInput, AreaDoubleBitInput, AreaBinaryOutput,
					AreaCounter, AreaFrozenCounter, AreaAnalogInput, AreaAnalogOutput},
				Scope: "point",
				Help:  "binary-input 为组 1；counter 为组 20；frozen-counter 为组 21；analog-input 为组 30；输出类可写"},
			{Name: "address.offset", Label: "点索引", Type: "int", Required: true,
				Scope: "point", Help: "DNP3 对象的 index（通常 0..65535）"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64",
					"float32", "float64"}, Scope: "point"},
		},
		Description: "DNP3 主站：总召唤 + 事件轮询 + 非请求响应（类 1/2/3）+ CROB/模拟量直接控制；IIN 报重启/溢出立即重召",
	}
}

// Open 建立 TCP 链路、复位链路层、做一次总召唤。
//
// 总召唤在这里同步完成（与 IEC 104 的 STARTDT 不同）：DNP3 的链路复位
// 之后本来就必须读一次类 0 才知道从站有哪些点，早一点失败比让上层拿着
// 一张空表跑起来更有价值。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("dnp3 requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil && !strings.Contains(addr, ":") {
		addr = fmt.Sprintf("%s:%d", addr, DefaultPort)
	}

	dest := cfg.Device.UnitID
	if dest == 0 {
		dest = 1
	}
	if err := checkLinkAddr("unitId", dest); err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	src := defaultMasterAddr
	if v := strings.TrimSpace(cfg.Device.Labels["masterAddress"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
				fmt.Sprintf("labels.masterAddress %q: not an integer", v), false, err)
		}
		src = n
	}
	if err := checkLinkAddr("masterAddress", src); err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	if src == dest {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("masterAddress and unitId are both %d: DNP3 requires distinct link addresses", src), false, nil)
	}

	eventPoll, err := durationLabel(cfg.Device.Labels, "eventPoll", defaultEventPoll)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	integrity, err := durationLabel(cfg.Device.Labels, "integrityPoll", defaultIntegrityPol)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	if eventPoll < 100*time.Millisecond {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"labels.eventPoll must be >= 100ms", false, nil)
	}
	if integrity < eventPoll {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"labels.integrityPoll must be >= eventPoll", false, nil)
	}

	plans, byKey, err := buildPlans(cfg.Device)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID, err.Error(), false, err)
	}

	c := &conn{
		devID: cfg.Device.ID, addr: addr, dest: uint16(dest), src: uint16(src),
		timeout:   cfg.Device.TimeoutOrDefault(),
		eventPoll: eventPoll, integrityPoll: integrity,
		dial: d.dial, log: cfg.Log,
		points: plans, byKey: byKey,
		values:   map[string]cachedValue{},
		observed: map[pointKey]Observed{},
		done:     make(chan struct{}),
		linkAck:  make(chan secondaryFrame, 1),
	}

	openCtx, cancel := context.WithTimeout(ctx, 3*c.timeout)
	defer cancel()
	if err := c.connect(openCtx); err != nil {
		_ = c.Close()
		return nil, err
	}
	go c.pollLoop()
	return c, nil
}

func checkLinkAddr(field string, v int) error {
	if v < 1 || v > 65519 {
		return fmt.Errorf("%s %d out of range 1..65519", field, v)
	}
	return nil
}

func durationLabel(labels map[string]string, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(labels[key])
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("labels.%s %q: %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("labels.%s %q: must be positive", key, v)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// 点位映射
// ---------------------------------------------------------------------------

type pointKey struct {
	area  string
	index uint32
}

type pointPlan struct {
	id       string
	key      pointKey
	dataType model.DataType
}

type cachedValue struct {
	value     model.Value
	quality   model.Quality
	at        time.Time
	group     byte
	variation byte
}

// Observed 是「从站实际送了什么」的诊断快照。自检 CLI 用它建立陌生设备
// 的点表：先看设备有哪些 index、用什么组/变体，再写配置。
type Observed struct {
	Group     byte
	Variation byte
	Area      string
	Index     uint32
	Flags     byte
	HasFlags  bool
	Value     any
	At        time.Time
}

func buildPlans(d model.Device) (map[string]*pointPlan, map[pointKey][]*pointPlan, error) {
	plans := make(map[string]*pointPlan, len(d.Points))
	byKey := map[pointKey][]*pointPlan{}
	for _, p := range d.Points {
		if !p.Enabled {
			continue
		}
		if p.Address.Kind != model.AddressRegister {
			return nil, nil, fmt.Errorf("point %q: dnp3 requires register addressing", p.ID)
		}
		area, ok := normalizeArea(p.Address.Area)
		if !ok {
			return nil, nil, fmt.Errorf("point %q: unknown area %q (want %s)", p.ID, p.Address.Area,
				strings.Join(AreaNames(), ", "))
		}
		if p.Address.Offset > 0xFFFFFF {
			return nil, nil, fmt.Errorf("point %q: index %d exceeds 24-bit range", p.ID, p.Address.Offset)
		}
		if err := checkDataType(area, p.DataType); err != nil {
			return nil, nil, fmt.Errorf("point %q: %w", p.ID, err)
		}
		key := pointKey{area: area, index: uint32(p.Address.Offset)}
		plan := &pointPlan{id: p.ID, key: key, dataType: p.DataType}
		plans[p.ID] = plan
		byKey[key] = append(byKey[key], plan)
	}
	return plans, byKey, nil
}

func checkDataType(area string, dt model.DataType) error {
	switch area {
	case AreaBinaryInput, AreaDoubleBitInput, AreaBinaryOutput:
		if dt == model.TypeBool || dt.IsNumeric() {
			return nil
		}
		return fmt.Errorf("dataType %q: binary objects need bool or a numeric type", dt)
	default:
		if !dt.IsNumeric() {
			return fmt.Errorf("dataType %q: analog/counter objects need a numeric type", dt)
		}
		return nil
	}
}

// AreaNames 返回可用的地址区名，错误信息与配置界面共用。
func AreaNames() []string {
	return []string{AreaBinaryInput, AreaDoubleBitInput, AreaBinaryOutput,
		AreaCounter, AreaFrozenCounter, AreaAnalogInput, AreaAnalogOutput}
}

func normalizeArea(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case AreaBinaryInput, "bi", "binaryinput":
		return AreaBinaryInput, true
	case AreaDoubleBitInput, "dbi", "doublebitinput":
		return AreaDoubleBitInput, true
	case AreaBinaryOutput, "bo", "binaryoutput":
		return AreaBinaryOutput, true
	case AreaCounter, "ctr", "counters":
		return AreaCounter, true
	case AreaFrozenCounter, "fctr", "frozen-counters", "frozencounter":
		return AreaFrozenCounter, true
	case AreaAnalogInput, "ai", "analoginput":
		return AreaAnalogInput, true
	case AreaAnalogOutput, "ao", "analogoutput":
		return AreaAnalogOutput, true
	}
	return "", false
}

func areaOfGroup(g byte) (string, bool) {
	switch g {
	case dnp3codec.GroupBinaryInput, dnp3codec.GroupBinaryEvent:
		return AreaBinaryInput, true
	case dnp3codec.GroupDoubleBitInput, dnp3codec.GroupDoubleBitEvent:
		return AreaDoubleBitInput, true
	case dnp3codec.GroupBinaryOutput, dnp3codec.GroupBinaryOutputEvent:
		return AreaBinaryOutput, true
	case dnp3codec.GroupCounter, dnp3codec.GroupCounterEvent:
		return AreaCounter, true
	case dnp3codec.GroupFrozenCounter, dnp3codec.GroupFrozenCounterEvent:
		return AreaFrozenCounter, true
	case dnp3codec.GroupAnalogInput, dnp3codec.GroupAnalogEvent:
		return AreaAnalogInput, true
	case dnp3codec.GroupAnalogOutput:
		return AreaAnalogOutput, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

// secondaryFrame 是一条收到的二级帧（ACK / NACK / 链路状态 / 不支持）。
// 链路层等待方需要功能码才能区分「对方接受了」与「对方拒绝了」。
type secondaryFrame struct {
	fn byte
}

type conn struct {
	devID         string
	addr          string
	dest          uint16
	src           uint16
	timeout       time.Duration
	eventPoll     time.Duration
	integrityPoll time.Duration
	dial          Dialer
	log           *slog.Logger

	points map[string]*pointPlan
	byKey  map[pointKey][]*pointPlan

	mu       sync.Mutex
	link     net.Conn
	linkErr  error
	closed   bool
	values   map[string]cachedValue
	observed map[pointKey]Observed
	dropped  uint64
	lastIIN  uint16

	wmu  sync.Mutex // 串行化「写一整条链路帧」
	cmu  sync.Mutex // 串行化应用事务（一问一答）
	tseq uint8
	aseq uint8

	rmu        sync.Mutex
	pending    chan dnp3codec.AppMessage
	pendingSeq uint8

	linkAck   chan secondaryFrame
	done      chan struct{}
	closeOnce sync.Once
}

// connect 建立一条新链路并完成复位与首次总召唤。
func (c *conn) connect(ctx context.Context) error {
	nc, err := c.dial(ctx, c.addr, c.timeout)
	if err != nil {
		return driver.E(driver.ErrDisconnected, "open", c.devID,
			fmt.Sprintf("dial %s", c.addr), true, err)
	}
	c.setLink(nc)
	if err := c.linkReset(ctx); err != nil {
		_ = nc.Close()
		return err
	}
	objects, err := dnp3codec.ClassObjects(0, 1, 2, 3)
	if err != nil {
		return driver.E(driver.ErrInternal, "open", c.devID, err.Error(), false, err)
	}
	if _, err := c.request(ctx, dnp3codec.AppRead, objects); err != nil {
		_ = nc.Close()
		return err
	}
	// 启动完整性轮询后激活非请求响应（类 1/2/3）。声明里写的「非请求响应
	// 的接收与确认」要靠这一条才成立：不激活，readLoop 里的 AppUnsolicited
	// 分支就永远不可达，所谓订阅退化成纯 1 秒事件轮询。
	// 从站不支持时（IIN 报 FuncNotSupport）退回纯轮询——Enable 失败不该让
	// 连接失败，只是推送变成轮询。
	enable, err := dnp3codec.ClassObjects(1, 2, 3)
	if err != nil {
		return driver.E(driver.ErrInternal, "open", c.devID, err.Error(), false, err)
	}
	resp, err := c.request(ctx, dnp3codec.AppEnableUnsolicited, enable)
	if err != nil {
		if c.log != nil {
			c.log.Warn("dnp3_enable_unsolicited_failed", "device", c.devID, "error", err.Error())
		}
	} else if resp.IIN&dnp3codec.IINFuncNotSupport != 0 {
		if c.log != nil {
			c.log.Warn("dnp3_enable_unsolicited_rejected", "device", c.devID,
				"hint", "从站不支持非请求响应，保持事件轮询")
		}
	}
	return nil
}

func (c *conn) setLink(nc net.Conn) {
	c.mu.Lock()
	old := c.link
	c.link = nc
	c.linkErr = nil
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go c.readLoop(nc)
}

func (c *conn) readLoop(nc net.Conn) {
	var dec dnp3codec.Decoder
	var re dnp3codec.Reassembler
	buf := make([]byte, 4096)
	for {
		n, err := nc.Read(buf)
		if n > 0 {
			dec.Feed(buf[:n])
			for {
				f, ok, derr := dec.Next()
				if derr != nil {
					// 坏帧不致命：丢掉、计数，继续读。TCP 是字节流，
					// 一个 CRC 错误之后下一帧仍可能完整。
					c.noteBadFrame()
					continue
				}
				if !ok {
					break
				}
				if herr := c.handleFrame(nc, f, &re); herr != nil {
					c.noteBadFrame()
				}
			}
		}
		if err != nil {
			if !c.isClosed() {
				c.failOn(nc, err)
			}
			return
		}
	}
}

func (c *conn) handleFrame(nc net.Conn, f dnp3codec.Frame, re *dnp3codec.Reassembler) error {
	if !f.Primary() {
		// 二级帧：ACK / NACK / 链路状态。只用于复位与探活，但必须把功能码
		// 带上去——NACK 与 NOT_SUPPORTED 不是「成功」。
		select {
		case c.linkAck <- secondaryFrame{fn: f.Function()}:
		default:
		}
		return nil
	}
	switch f.Function() {
	case dnp3codec.FuncConfirmedUserData:
		// 从站用确认方式上送时必须回 ACK，否则它会停在那里等。
		if err := c.writeLink(dnp3codec.FuncAck, nil); err != nil {
			return err
		}
		return c.handleApp(nc, f.Data, re)
	case dnp3codec.FuncUnconfirmedData:
		return c.handleApp(nc, f.Data, re)
	case dnp3codec.FuncRequestLinkStatus:
		return c.writeLink(dnp3codec.FuncLinkStatus, nil)
	}
	return nil
}

func (c *conn) handleApp(nc net.Conn, data []byte, re *dnp3codec.Reassembler) error {
	if len(data) < 1 {
		return errors.New("dnp3: empty transport frame")
	}
	h := data[0]
	app, done, err := re.Push(h&dnp3codec.TransportFIR != 0, h&dnp3codec.TransportFIN != 0,
		h&dnp3codec.TransportSeq, data[1:])
	if err != nil {
		return err
	}
	if !done {
		return nil
	}
	m, err := dnp3codec.DecodeApp(app)
	if err != nil {
		return err
	}
	switch m.Function {
	case dnp3codec.AppResponse:
		// 先落值再交付：调用方拿到响应后立刻 Read，不能读到旧值。
		c.applyObjects(m.Objects, m.IIN)
		c.deliver(m)
	case dnp3codec.AppUnsolicited:
		c.applyObjects(m.Objects, m.IIN)
	case dnp3codec.AppConfirm:
		// 从站确认了我们的请求；没有需要做的事。
	default:
		// 从站发来的请求（冷重启、时间同步等）本实现不支持。
		// 明确回一个「功能不支持」，而不是沉默——沉默会让对端重试到超时。
		if err := c.sendApp(dnp3codec.EncodeResponse(
			dnp3codec.AppControl{FIR: true, FIN: true, Seq: m.Control.Seq},
			dnp3codec.AppResponse, dnp3codec.IINFuncNotSupport, nil)); err != nil {
			return err
		}
	}
	if m.Control.CON {
		// 事件响应通常要求应用层确认。放在最后发：值已经落表，即使确认
		// 丢了、从站重发，也只是重复一次幂等的写入。
		return c.sendApp(dnp3codec.EncodeRequest(
			dnp3codec.AppControl{FIR: true, FIN: true, Seq: m.Control.Seq},
			dnp3codec.AppConfirm, nil))
	}
	return nil
}

func (c *conn) deliver(m dnp3codec.AppMessage) {
	c.rmu.Lock()
	ch, seq := c.pending, c.pendingSeq
	c.rmu.Unlock()
	if ch == nil || m.Control.Seq != seq {
		return
	}
	select {
	case ch <- m:
	default:
	}
}

// applyObjects 把对象写进点位表与诊断表。
func (c *conn) applyObjects(raw []byte, iin uint16) {
	objs, err := dnp3codec.ParseObjects(raw)
	if err != nil {
		var ue *dnp3codec.UnsupportedError
		if !errors.As(err, &ue) {
			c.noteBadFrame()
		}
		// UnsupportedError 时 objs 里是已解析的部分，继续落表。
	}
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.lastIIN
	c.lastIIN = iin
	// DeviceRestart(0x0080) / EventOverflow(0x0800) 要立即补一次完整性重召：
	// 重启意味着类 0 快照全变，等周期到达最坏 5 分钟旧数据；溢出意味着
	// 中间丢过事件，同样只有类 0 能补救。位转换守卫防止连环重召（重启位
	// 在从站被读过之后自清；溢出位在事件重新入队后自清）。轮询在后台做，
	// 不阻塞当前响应的落表。
	if newly := iin & ^old & (dnp3codec.IINDeviceRestart | dnp3codec.IINEventOverflow); newly != 0 {
		if c.log != nil {
			c.log.Warn("dnp3_iin_integrity_poll", "device", c.devID,
				"hint", "从站报重启/事件溢出，立即完整性重召")
		}
		go c.integrityPollNow()
	}
	for _, o := range objs {
		area, ok := areaOfGroup(o.Group)
		if !ok {
			continue
		}
		key := pointKey{area: area, index: o.Index}
		if _, seen := c.observed[key]; !seen && len(c.observed) >= maxObserved {
			c.dropped++
			continue
		}
		c.observed[key] = Observed{
			Group: o.Group, Variation: o.Variation, Area: area, Index: o.Index,
			Flags: o.Flags, HasFlags: o.HasFlags, Value: o.Value, At: now,
		}
		for _, p := range c.byKey[key] {
			v, q, ok := convertObject(o, p.dataType)
			if !ok {
				continue
			}
			c.values[p.id] = cachedValue{
				value: v, quality: q, at: now, group: o.Group, variation: o.Variation,
			}
		}
	}
}

// request 发一条应用请求并等匹配序号的响应。
func (c *conn) request(ctx context.Context, fn byte, objects []byte) (dnp3codec.AppMessage, error) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if err := c.checkLink(); err != nil {
		return dnp3codec.AppMessage{}, err
	}
	c.aseq = (c.aseq + 1) & 0x0F
	seq := c.aseq
	ch := make(chan dnp3codec.AppMessage, 4)
	c.rmu.Lock()
	c.pending, c.pendingSeq = ch, seq
	c.rmu.Unlock()
	defer func() {
		c.rmu.Lock()
		c.pending = nil
		c.rmu.Unlock()
	}()

	payload := dnp3codec.EncodeRequest(dnp3codec.AppControl{FIR: true, FIN: true, Seq: seq}, fn, objects)
	if err := c.sendApp(payload); err != nil {
		return dnp3codec.AppMessage{}, err
	}

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	for {
		select {
		case m := <-ch:
			if m.Control.Seq != seq {
				continue
			}
			return m, nil
		case <-timer.C:
			return dnp3codec.AppMessage{}, driver.E(driver.ErrTimeout, "request", c.devID,
				fmt.Sprintf("no application response within %s", c.timeout), true, nil)
		case <-ctx.Done():
			return dnp3codec.AppMessage{}, driver.E(driver.ErrTimeout, "request", c.devID,
				"context done", true, ctx.Err())
		case <-c.done:
			return dnp3codec.AppMessage{}, driver.E(driver.ErrDisconnected, "request", c.devID,
				"connection closed", true, nil)
		}
	}
}

func (c *conn) sendApp(payload []byte) error {
	chunks := dnp3codec.EncodeTransport(payload, c.tseq)
	c.tseq = (c.tseq + uint8(len(chunks))) & 0x3F
	for _, ch := range chunks {
		if err := c.writeLink(dnp3codec.FuncConfirmedUserData, ch); err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) writeLink(fn byte, data []byte) error {
	c.mu.Lock()
	nc := c.link
	c.mu.Unlock()
	if nc == nil {
		return driver.E(driver.ErrDisconnected, "write", c.devID, "link is closed", true, nil)
	}
	ctrl := byte(dnp3codec.ControlDir | dnp3codec.ControlPrm | fn)
	raw, err := dnp3codec.EncodeFrame(ctrl, c.dest, c.src, data)
	if err != nil {
		return driver.E(driver.ErrInternal, "write", c.devID, err.Error(), false, err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = nc.SetWriteDeadline(time.Now().Add(c.timeout))
	if _, err := nc.Write(raw); err != nil {
		return driver.E(driver.ErrDisconnected, "write", c.devID, "write frame", true, err)
	}
	return nil
}

func (c *conn) checkLink() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "request", c.devID, "connection closed", false, nil)
	}
	if c.linkErr != nil {
		return driver.E(driver.ErrDisconnected, "request", c.devID, "link is down", true, c.linkErr)
	}
	return nil
}

func (c *conn) failOn(nc net.Conn, err error) {
	c.mu.Lock()
	if c.link == nc && !c.closed {
		c.linkErr = err
	}
	c.mu.Unlock()
}

func (c *conn) noteBadFrame() {
	c.mu.Lock()
	c.dropped++
	c.mu.Unlock()
}

func (c *conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *conn) drainAcks() {
	for {
		select {
		case <-c.linkAck:
		default:
			return
		}
	}
}

// waitAck 等待一条**二级帧应答**，并区分它的种类。
//
// DNP3 的链路层确认不带事务号：同一时刻只允许一条未确认的复位/探活，所以
// 「收到下一帧二级帧」在时序上足够。但「收到二级帧」不等于「对方接受了请求」
// ——从站可以回 NACK（1）表示拒绝处理，回 NOT_SUPPORTED（15）表示不支持。
// 把这两种当成 ACK 会让复位被拒的设备被当成链路已就绪，失败被推迟到
// 应用层超时，真实原因（NACK）从不出现在日志里。因此这里按功能码分流。
func (c *conn) waitAck(ctx context.Context, what string) error {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case ack := <-c.linkAck:
		switch ack.fn {
		case dnp3codec.FuncAck, dnp3codec.FuncLinkStatus:
			return nil
		case dnp3codec.FuncNack:
			return driver.E(driver.ErrProtocol, what, c.devID,
				"outstation replied NACK: it refused the link-layer request", true, nil)
		case dnp3codec.FuncNotSupported:
			return driver.E(driver.ErrUnsupported, what, c.devID,
				"outstation replied not-supported: link-layer function not implemented", false, nil)
		default:
			return driver.E(driver.ErrProtocol, what, c.devID,
				fmt.Sprintf("unexpected secondary frame function %d", ack.fn), true, nil)
		}
	case <-timer.C:
		return driver.E(driver.ErrTimeout, what, c.devID, "no link-layer ACK within 3s", true, nil)
	case <-ctx.Done():
		return driver.E(driver.ErrTimeout, what, c.devID, "context done", true, ctx.Err())
	case <-c.done:
		return driver.E(driver.ErrDisconnected, what, c.devID, "connection closed", true, nil)
	}
}

func (c *conn) linkReset(ctx context.Context) error {
	c.drainAcks()
	if err := c.writeLink(dnp3codec.FuncResetLink, nil); err != nil {
		return err
	}
	if err := c.waitAck(ctx, "reset-link"); err != nil {
		return err
	}
	return nil
}

func (c *conn) linkTest(ctx context.Context) error {
	c.drainAcks()
	if err := c.writeLink(dnp3codec.FuncTestLink, nil); err != nil {
		return err
	}
	return c.waitAck(ctx, "test-link")
}

// ---------------------------------------------------------------------------
// 轮询与重连
// ---------------------------------------------------------------------------

func (c *conn) pollLoop() {
	events := time.NewTicker(c.eventPoll)
	defer events.Stop()
	integrity := time.NewTicker(c.integrityPoll)
	defer integrity.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-events.C:
			if err := c.poll([]byte{1, 2, 3}); err != nil {
				c.recover()
			}
		case <-integrity.C:
			if err := c.poll([]byte{0, 1, 2, 3}); err != nil {
				c.recover()
			}
		}
	}
}

func (c *conn) poll(classes []byte) error {
	objects, err := dnp3codec.ClassObjects(classes...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	_, err = c.request(ctx, dnp3codec.AppRead, objects)
	return err
}

// integrityPollNow 在 IIN 报出从站重启/事件溢出时立即补一次类 0 完整性
// 轮询，不等 integrityPoll 周期。失败不重试：下一轮周期轮询兜底。
func (c *conn) integrityPollNow() {
	objects, err := dnp3codec.ClassObjects(0)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	_, _ = c.request(ctx, dnp3codec.AppRead, objects)
}

// recover 在主站侧断链后循环重连，直到成功或进程关闭。
//
// 退避上限 5 秒：现场交换机重启通常几秒到几十秒，退避再长会让恢复时间
// 不可控；但也不能零间隔重连，否则一个拒绝连接的从站会被打成半开连接
// 风暴。
func (c *conn) recover() {
	backoff := 200 * time.Millisecond
	for {
		select {
		case <-c.done:
			return
		case <-time.After(backoff):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*c.timeout)
		err := c.connect(ctx)
		cancel()
		if err == nil {
			if c.log != nil {
				c.log.Info("dnp3_reconnected", "device", c.devID, "addr", c.addr)
			}
			return
		}
		if c.log != nil {
			c.log.Warn("dnp3_reconnect_failed", "device", c.devID, "error", err.Error())
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// ---------------------------------------------------------------------------
// Conn 契约
// ---------------------------------------------------------------------------

// Probe 用链路层 TEST_LINK 做一次往返。
//
// 不复用总召唤：探活的代价必须是常数，不能与被测设备的点表规模相关。
func (c *conn) Probe(ctx context.Context) error {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if err := c.checkLink(); err != nil {
		return err
	}
	return c.linkTest(ctx)
}

// Read 只读本地表。DNP3 的「读」是整类轮询，由后台循环完成。
//
// 链路断开时有一件必须做的事：本表里的值都是**断链前**的快照。把它们原样
// 标着 good 返回，正好构成工业网关最危险的一类假数据——断线期间上层看到的是
// 一串「正常」的旧值。`runtime/collector.go` 会对返回的**每个**样本调用
// apply（即便同时拿到了错误），所以「把错误一并返回」并不足以保护上游。
// 因此链路级失败时逐点降级为 stale 并写明原因，值本身保留（诊断需要知道
// 断线前是多少），只是不再冒充「当前有效」。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
	}
	c.mu.Lock()
	closed, linkErr := c.closed, c.linkErr
	out := make([]driver.RawSample, 0, len(spans))
	for _, span := range spans {
		for _, p := range span.Points {
			out = append(out, c.lookupLocked(p.ID))
		}
	}
	c.mu.Unlock()

	switch {
	case closed:
		return degradeSamples(out, c.devID, "connection closed",
				driver.ErrDisconnected), driver.E(driver.ErrDisconnected, "read", c.devID,
				"connection closed", true, nil)
	case linkErr != nil:
		return degradeSamples(out, c.devID, "link is down; value is the last snapshot before the outage",
				driver.ErrDisconnected), driver.E(driver.ErrDisconnected, "read", c.devID,
				"link is down", true, linkErr)
	}
	return out, nil
}

// degradeSamples 把一轮样本整体降级为「陈旧」：值保留、质量改成 stale、原因
// 写进 Err。已经带错误的样本保持它自己的错误（那更具体），只把「看起来正常」
// 的那些换掉——否则断线期间的旧值会以 good 流进上层。
func degradeSamples(in []driver.RawSample, devID, msg string, kind driver.ErrorKind) []driver.RawSample {
	for i := range in {
		if in[i].Err != nil {
			continue
		}
		in[i].Quality = model.QualityStale
		in[i].Err = driver.E(kind, "read", devID, msg, true, nil)
	}
	return in
}

func (c *conn) lookupLocked(id string) driver.RawSample {
	plan, ok := c.points[id]
	if !ok {
		return driver.RawSample{
			PointID: id, Quality: model.QualityAddressError,
			Err: driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("point %q is not configured", id), false, nil),
		}
	}
	cv, seen := c.values[id]
	if !seen {
		msg := fmt.Sprintf("no data received for %s[%d] yet (not in the outstation's class 0 set?)",
			plan.key.area, plan.key.index)
		return driver.RawSample{
			PointID: id, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.devID, msg, true, nil),
		}
	}
	if cv.quality == model.QualityBad {
		return driver.RawSample{
			PointID: id, Value: cv.value, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("%s[%d] reported invalid quality", plan.key.area, plan.key.index), true, nil),
		}
	}
	return driver.RawSample{PointID: id, Value: cv.value, Quality: cv.quality}
}

// Write 下发直接控制。
//
// binary-output 走 CROB（组 12），analog-output 走组 41。两者都等从站的
// 应用响应：DNP3 的「已下发」与「已接受」是两件事，只写 socket 不等待
// 会把拒绝当成成功。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return results, firstErr
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	fail := func(kind driver.ErrorKind, msg string) (driver.WriteResult, error) {
		err := driver.E(kind, "write", c.devID, msg, kind != driver.ErrAddress && kind != driver.ErrUnsupported, nil)
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityBad, Err: err}, err
	}
	if req.Address.Kind != model.AddressRegister {
		return fail(driver.ErrAddress, fmt.Sprintf("point %q: dnp3 requires register addressing", req.PointID))
	}
	area, ok := normalizeArea(req.Address.Area)
	if !ok {
		return fail(driver.ErrAddress, fmt.Sprintf("point %q: unknown area %q", req.PointID, req.Address.Area))
	}
	index := uint32(req.Address.Offset)

	switch area {
	case AreaBinaryOutput:
		v, ok := boolOf(req.Value)
		if !ok {
			return fail(driver.ErrUnsupported, fmt.Sprintf("point %q: binary output needs bool/numeric, got %q", req.PointID, req.Value.Type))
		}
		code := dnp3codec.CROBLatchOff
		if v {
			code = dnp3codec.CROBLatchOn
		}
		objects, err := dnp3codec.CROBObjects(index, code, 1, 0, 0)
		if err != nil {
			return fail(driver.ErrInternal, err.Error())
		}
		resp, err := c.request(ctx, dnp3codec.AppDirectOperate, objects)
		if err != nil {
			// 保留 request 的真实分类（断连 ≠ 超时），不洗白成 timeout。
			return c.writeFailed(req, err)
		}
		if msg := iinError(resp.IIN); msg != "" {
			return fail(driver.ErrProtocol, msg)
		}
		if objs, perr := dnp3codec.ParseObjects(resp.Objects); perr == nil {
			for _, o := range objs {
				if c, ok := o.Value.(dnp3codec.CROB); ok && !c.Success() {
					return fail(driver.ErrProtocol,
						fmt.Sprintf("outstation rejected CROB at index %d: status 0x%02X", o.Index, c.Status))
				}
			}
		}
		c.storeWritten(req.PointID, req.DataType, model.BoolValue(v))
		return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil

	case AreaAnalogOutput:
		n, ok := numericOf(req.Value)
		if !ok {
			return fail(driver.ErrUnsupported, fmt.Sprintf("point %q: analog output needs a numeric value, got %q", req.PointID, req.Value.Type))
		}
		variation := byte(0)
		switch req.DataType {
		case model.TypeInt16:
			variation = 2
		case model.TypeInt32, model.TypeUint16, model.TypeUint32, model.TypeInt64, model.TypeUint64:
			// 组 41 没有无符号变体；按 int32 下发，范围由配置与从站
			// 各自校验。把 uint32 硬塞进 int32 会让大于 2^31 的值变成
			// 负数——这类配置错误应该在点表评审时发现，而不是在这里
			// 悄悄截断。
			variation = 1
		case model.TypeFloat32:
			variation = 3
		case model.TypeFloat64:
			variation = 4
		default:
			variation = 1
		}
		// 发报文前先确认这个值能装进选定的线格式。组 41 没有无符号变体，
		// 整数输出按 int32/int16 下发：越界或带小数的值此前会被
		// uint32(int32(value)) 静默回绕后发给从站，而驱动报成功——现场表现为
		// 「下发了错误设定点却显示成功」。与 CoAP 写路径共用同一条原则：
		// 语义可判定时就判坏并给明确诊断，而不是发一个看起来正常的错误值。
		if !analogFitsWire(variation, n) {
			return fail(driver.ErrUnsupported,
				fmt.Sprintf("point %q: value %g cannot be represented in the selected output type", req.PointID, n))
		}
		objects, err := dnp3codec.AnalogOutputObjects(index, variation, n)
		if err != nil {
			return fail(driver.ErrInternal, err.Error())
		}
		resp, err := c.request(ctx, dnp3codec.AppDirectOperate, objects)
		if err != nil {
			// 保留 request 的真实分类（断连 ≠ 超时），不洗白成 timeout。
			return c.writeFailed(req, err)
		}
		if msg := iinError(resp.IIN); msg != "" {
			return fail(driver.ErrProtocol, msg)
		}
		c.storeWritten(req.PointID, req.DataType, model.NumValue(req.DataType, n))
		return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil

	default:
		return fail(driver.ErrUnsupported,
			fmt.Sprintf("area %q is read-only; writable areas are %s and %s", area, AreaBinaryOutput, AreaAnalogOutput))
	}
}

func iinError(iin uint16) string {
	var msgs []string
	if iin&dnp3codec.IINFuncNotSupport != 0 {
		msgs = append(msgs, "function not supported")
	}
	if iin&dnp3codec.IINObjectUnknown != 0 {
		msgs = append(msgs, "object unknown")
	}
	if iin&dnp3codec.IINParamError != 0 {
		msgs = append(msgs, "parameter error")
	}
	if iin&dnp3codec.IINAlreadyExec != 0 {
		msgs = append(msgs, "already executing")
	}
	if len(msgs) == 0 {
		return ""
	}
	return "outstation IIN: " + strings.Join(msgs, ", ")
}

func (c *conn) storeWritten(id string, dt model.DataType, v model.Value) {
	c.mu.Lock()
	c.values[id] = cachedValue{value: v, quality: model.QualityGood, at: time.Now().UTC()}
	c.mu.Unlock()
}

// writeFailed 把一次「请求阶段就失败」的写整理成结果 + 错误。
//
// 关键点是**不得洗白错误类别**。`c.request` 已经按真实原因分类：链路断开是
// `disconnected`、事务超时是 `timeout`、ctx 到期是 `timeout`、写帧失败是
// `disconnected`。此前这里统一 `fail(driver.ErrTimeout, ...)`，等于把所有写
// 失败都报成「超时可重试」——一条已经断掉的链路会被调度器当成「设备慢」而
// 原地重试，永远不触发重建连接（§3.10.14.12 的错误分类表把拨号/断连明确列为
// `disconnected`）。所以这里保留原错误的 `KindOf`，只做质量码换算。
func (c *conn) writeFailed(req driver.WriteRequest, err error) (driver.WriteResult, error) {
	q := model.QualityBad
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		q = model.QualityTimeout
	case driver.ErrDisconnected:
		q = model.QualityCommError
	}
	return driver.WriteResult{PointID: req.PointID, OK: false, Quality: q, Err: err}, err
}

func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		c.closed = true
		nc := c.link
		c.link = nil
		c.mu.Unlock()
		if nc != nil {
			_ = nc.Close()
		}
	})
	return nil
}

// Observed 返回从站实际送过的对象，按区与索引排序。
func (c *conn) Observed() []Observed {
	c.mu.Lock()
	out := make([]Observed, 0, len(c.observed))
	for _, o := range c.observed {
		out = append(out, o)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Area != out[j].Area {
			return out[i].Area < out[j].Area
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// Dropped 返回因坏帧或超出诊断表上限而丢弃的对象数。
func (c *conn) Dropped() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// IIN 返回最近一条响应的内部指示字，供诊断使用。
func (c *conn) IIN() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastIIN
}

// ---------------------------------------------------------------------------
// 值转换
// ---------------------------------------------------------------------------

func convertObject(o dnp3codec.Object, dt model.DataType) (model.Value, model.Quality, bool) {
	q := qualityOf(o)
	switch v := o.Value.(type) {
	case bool:
		if dt == model.TypeBool {
			return model.BoolValue(v), q, true
		}
		if dt.IsNumeric() {
			if v {
				return model.NumValue(dt, 1), q, true
			}
			return model.NumValue(dt, 0), q, true
		}
		return model.Value{}, q, false
	case uint64:
		if o.Group == dnp3codec.GroupDoubleBitInput || o.Group == dnp3codec.GroupDoubleBitEvent {
			if dt == model.TypeBool {
				switch v {
				case 1: // 分
					return model.BoolValue(false), q, true
				case 2: // 合
					return model.BoolValue(true), q, true
				default:
					// 0/3 是中间状态/不确定。报成 false good 会让
					// 「未知」显示成「已关闭」。
					return model.BoolValue(false), worse(q, model.QualityUncertain), true
				}
			}
		}
		if dt.IsNumeric() {
			return model.NumValue(dt, float64(v)), q, true
		}
		if dt == model.TypeBool {
			return model.BoolValue(v != 0), q, true
		}
		return model.Value{}, q, false
	case int64:
		if dt.IsNumeric() {
			return model.NumValue(dt, float64(v)), q, true
		}
		return model.Value{}, q, false
	case float64:
		if dt.IsNumeric() {
			return model.NumValue(dt, v), q, true
		}
		return model.Value{}, q, false
	case dnp3codec.CROB:
		if dt == model.TypeBool {
			return model.BoolValue(v.Success()), q, true
		}
		if dt.IsNumeric() {
			return model.NumValue(dt, float64(v.Status)), q, true
		}
		return model.Value{}, q, false
	}
	return model.Value{}, model.QualityBad, false
}

// qualityOf 把品质位翻译成平台质量码。
//
// 品质字节的高位不是品质：二进制量的 bit7、双点量的 bit6-7 是过程状态，
// 只有低 6 位是品质。若把双点量的「合」(状态 2 → bit6) 当成 REF_ERR，
// 每个合位的开关都会变成 uncertain——这是「所有阀门都不可信」的假故障。
// 模拟量与计数器的整字节都是品质，不存在这个问题。
//
// ONLINE=0 或 COMM_LOST=1 是 bad；RESTART/强置/越限/参考误差是
// uncertain——值可能仍然有效，但不能当成正常值参与联锁。
//
// 关于 !o.HasFlags 直接返回 good：**这不是 fail-open，而是 IEEE 1815 的明
// 确定义**。HasFlags=false 只可能来自「without flag」变体——计数器组 20
// 变体 5/6、模拟量组 30 变体 3/4（详见 codec 的 numericSpecOf）。这些变体
// 的线格式里**根本没有品质字节**，标准对它们的定义就是「值被假定有效」
// （省略品质信息的变体，接收方按值有效处理）。因此这里没有 ONLINE 位可判，
// 返回 good 是与协议一致的。
//
// 这与 MTConnect 的「条件缺级别」有本质差别：那边是「语义存在但读不出
// 级别」，读不出就该判坏（否则告警被静默吞掉）；这边是「协议本就无此字
// 段」，无从判断时标准规定假定有效。若把这里也改成判坏，会让所有使用
// without-flag 变体的真实设备（现场很常见）上报「全部离线」，是另一种
// 方向的假故障。测试 TestQualityWithoutFlagIsGood 从两个方向钉住这个判断。
func qualityOf(o dnp3codec.Object) model.Quality {
	if !o.HasFlags {
		return model.QualityGood
	}
	f := o.Flags
	switch o.Group {
	case dnp3codec.GroupBinaryInput, dnp3codec.GroupBinaryEvent,
		dnp3codec.GroupBinaryOutput, dnp3codec.GroupBinaryOutputEvent:
		f &^= dnp3codec.FlagState
	case dnp3codec.GroupDoubleBitInput, dnp3codec.GroupDoubleBitEvent:
		f &^= 0xC0
	}
	if f&dnp3codec.FlagOnline == 0 || f&dnp3codec.FlagCommLost != 0 {
		return model.QualityBad
	}
	if f&(dnp3codec.FlagRestart|dnp3codec.FlagRemoteForced|dnp3codec.FlagLocalForced|
		dnp3codec.FlagChatter|dnp3codec.FlagRefErr) != 0 {
		return model.QualityUncertain
	}
	return model.QualityGood
}

func worse(a, b model.Quality) model.Quality {
	rank := map[model.Quality]int{
		model.QualityGood: 0, model.QualityUncertain: 1, model.QualityStale: 2,
		model.QualityBad: 3, model.QualityTimeout: 4, model.QualityCommError: 5,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func boolOf(v model.Value) (bool, bool) {
	if v.Type == model.TypeBool {
		return v.Bool, true
	}
	if v.Type.IsNumeric() {
		return v.Num != 0, true
	}
	return false, false
}

func numericOf(v model.Value) (float64, bool) {
	if v.Type == model.TypeBool {
		if v.Bool {
			return 1, true
		}
		return 0, true
	}
	if v.Type.IsNumeric() {
		return v.Num, true
	}
	return 0, false
}

// analogFitsWire 判断一个浮点值能否按选定线格式原样下发。
//
// 组 41 的整型输出没有无符号变体，点位类型到变体的映射是：
//
//	int16                 → v2 (int16)
//	int32/uint16/uint32/
//	int64/uint64          → v1 (int32)
//	float32               → v3 (float32)
//	float64               → v4 (float64)
//
// 对整型变体，越界（含 uint32 上半段、int64 大值）与带小数的值都必须在这里
// 判坏：它们会被编码端的 uint32(int32(value))/uint16(int16(value)) 静默
// 截断或回绕。uint32/uint64 点位映射到 v1 是协议本身的限制（组 41 无无符号
// 变体），不是这里能绕过的——值超过 int32 上界时只能拒绝并让点表评审改类型。
func analogFitsWire(variation byte, n float64) bool {
	switch variation {
	case 1: // int32
		return n == math.Trunc(n) && n >= -2147483648 && n <= 2147483647
	case 2: // int16
		return n == math.Trunc(n) && n >= -32768 && n <= 32767
	case 3: // float32：拒绝 NaN/Inf 与超出 float32 幅度的值（会溢出成 Inf）
		return !math.IsInf(n, 0) && !math.IsNaN(n) && math.Abs(n) <= math.MaxFloat32
	default: // 4：float64
		return !math.IsInf(n, 0) && !math.IsNaN(n)
	}
}
