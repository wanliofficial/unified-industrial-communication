// Package dlt645 实现多功能电能表通信协议（2007 版）的主站驱动。
//
// 分层与其它驱动一致：Meta（能力声明）→ conn（事务/错误分类）→
// codec/dlt645（纯函数编解码）→ transport（串口字节流，经 serialport
// 共享同一根 RS-485 总线）。
//
// 三条刻意的边界：
//
//  1. **不做「连续读」**。协议的寻址是数据标识（DI）而不是寄存器偏移，
//     一次请求只能要一个 DI。因此每轮采集是「一点一个事务」，不存在
//     ReadSpan 合并的空间。说清楚这一点比假装支持批量更重要：现场把
//     300 个点位挂到一条 2400bps 的总线上，一轮采集要几十秒，这个
//     事实必须让配置的人看见，而不是被驱动藏起来。
//  2. **不做高危写命令的默认开启**。拉合闸（0x1C）、电表清零（0x1A）、
//     事件清零（0x1B）、改通信地址（0x15）、改速率（0x17）都需要显式
//     配置开关；一个「能读」的驱动不应该顺手能把闸拉了。
//  3. **不做广播**。广播校时、广播冻结必须由运维显式发起（CLI），不能
//     出现在任何周期任务里：一条配错的广播会同时改掉总线上所有表的时间。
//
// 地址约定：`kind=register`，`area` 是 8 位十六进制 DI（**手册书写顺序**
// DI3DI2DI1DI0，例如 `02010100` 是 A 相电压），`offset` 恒为 0，`length`
// 由 DI 表推导（数据块显式给出）。表地址取自 `Device.Labels["address"]`。
package dlt645

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"uic/internal/codec/dlt645"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
	"uic/internal/transport/serialport"
)

// 默认帧间静默。
//
// 标准只给了「字节间 ≤500ms、帧间 >20ms 且 ≤500ms」的宽松窗口。取 30ms
// 的理由：RS-485 收发器从发送切到接收通常在 1ms 内完成，30ms 足够覆盖
// USB 转 485 的批量传输延迟（实测 5~20ms），又不至于在 300 台表的总线上
// 把帧间隔放大到无法接受。
const defaultGap = 30 * time.Millisecond

// maxFollow 是一次读数据允许跟随的后续帧数上限。
//
// 读数据块（例如整包电能）按标准可以分多帧返回，每帧最多 200 字节数据。
// 理论上限是 255 帧，但一个「后续帧永远不为 0」的表会把驱动挂死在一次
// 事务里；64 帧（约 12KB）已经远超任何合法数据块。
const maxFollow = 64

// defaultTCPPort 是串口服务器/DTU 透传的常见端口，仅在配置省略端口时使用。
const defaultTCPPort = "4001"

// Options 是写命令的安全开关，来自通道配置。
type Options struct {
	// AllowWrite 允许普通写数据（0x14）。
	AllowWrite bool
	// AllowControl 允许拉合闸（0x1C）与清零（0x19/0x1A/0x1B）。
	AllowControl bool
	// Password 是写数据/控制用的密码权限（PA 的高 4 位）与密码（P0..P2）。
	Password  byte
	PasswordB [3]byte
	// Operator 是操作者代码（C0..C3），仅用于追溯。
	Operator [4]byte
	// Preamble 覆盖前导 FE 的个数（0 = 不发，标准值 4）。
	//
	// 做成可配是因为现场确有一批适配器在前导之后丢第一个字节，表现为
	// 「不发 FE 能通、发了就不通」。这类问题不该靠改驱动代码解决。
	Preamble int
	// Gap 覆盖帧间静默。
	Gap time.Duration
}

// Driver 是 DL/T 645 主站驱动。
type Driver struct {
	serial serialport.Opener
}

// New 返回使用真实串口的驱动。
func New() *Driver { return &Driver{} }

// NewWithSerial 返回注入串口实现的驱动（测试用）。
func NewWithSerial(o serialport.Opener) *Driver { return &Driver{serial: o} }

// Meta 声明能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "dlt645",
		Name:         "多功能电能表（645）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster, driver.RoleSlave},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapUnsupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.port", Label: "串口", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0", Scope: "channel"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: 2400, Scope: "channel",
				Help: "标准速率 600/1200/2400/4800/9600/19200；现场多数表默认 2400 偶校验"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: "even", Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "标准要求偶校验；个别厂商出厂为无校验，需与表一致"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "半双工总线必须串行；同一物理串口由多个通道共享时按总线锁排队"},
			{Name: "labels.options", Label: "写命令开关", Type: "string", Default: "", Scope: "device",
				Help: "allow-write,allow-control,preamble=0,gap=50ms,password=000000"},
		},
		PointFields: []driver.ConfigField{
			{Name: "labels.address", Label: "表地址", Type: "string", Required: true, Scope: "device",
				Placeholder: "000000000042",
				Help:        "12 位 BCD 数字（可缩位写 0042，高位自动补 AA 通配）；广播地址为全 99"},
			{Name: "address.area", Label: "数据标识 DI", Type: "string", Required: true, Scope: "point",
				Placeholder: "02010100",
				Help:        "8 位十六进制，按手册顺序 DI3DI2DI1DI0；例如 02010100=A 相电压、00000000=组合有功总电能"},
			{Name: "address.length", Label: "长度（数据块）", Type: "int", Scope: "point",
				Help: "仅数据块（DI 含 FF）需要；普通数据项由 DI 表推导，写了会与推导值冲突"},
		},
		Description: "多功能电能表通信协议主站：按数据标识读取电量/瞬时量/参变量，写参数与可选的拉合闸控制（RS-485）",
	}
}

// link 是驱动需要的链路能力：字节流 + 截止时间 + 清空缓冲 + 总线锁。
//
// 抽象成接口而不是直接用 *serialport.Ref，是为了同时支持「串口服务器」
// （RS-485 转以太网，现场极常见）：它的字节序与串口完全一致，只是没有
// 物理总线锁——锁退化成每连接一把互斥量。
type link interface {
	transport.Stream
	SetDeadline(t time.Time) error
	Drain() error
	Lock()
	Unlock()
}

// Open 建链。支持两种承载：
//
//   - serial：真实 RS-485，走 serialport 共享同一物理端口；
//   - tcp：串口服务器 / DTU 透传，字节流与串口一致。
//
// 两种承载的分帧、校验与命令序列完全相同，因此只有「怎么拿到一段
// 字节流」不同。这也是能让内置 TCP 从站直接用于自检的原因。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	addr, err := addressOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	opt := parseOptions(cfg.Device)
	t := cfg.Channel.Transport

	var l link
	switch t.Kind {
	case "serial":
		sc := transport.SerialFromTransport(t.Port, t.Baud, t.DataBits, t.Parity, t.StopBits)
		if err := sc.Validate(); err != nil {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
		}
		ref, aerr := serialport.Acquire("dlt645", sc, d.serial)
		if aerr != nil {
			// 打开串口失败通常是「设备被拔了/权限不对/参数被占用」，属可重试。
			return nil, driver.E(driver.ErrDisconnected, "open", cfg.Device.ID,
				fmt.Sprintf("open serial %s failed", sc.Port), true, aerr)
		}
		l = ref
	case "tcp":
		addrStr := strings.TrimSpace(t.Address)
		if addrStr == "" {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
				"transport.address required (host[:port])", false, nil)
		}
		if _, _, err := net.SplitHostPort(addrStr); err != nil && !strings.Contains(addrStr, ":") {
			addrStr = net.JoinHostPort(addrStr, defaultTCPPort)
		}
		st, derr := transport.DialTCP(ctx, addrStr, cfg.Channel.EffectiveTimeout())
		if derr != nil {
			return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
				fmt.Sprintf("connect %s failed", addrStr), true, derr)
		}
		l = &tcpLink{st: st}
	default:
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("dlt645 requires serial or tcp transport, got %q", t.Kind), false, nil)
	}

	return &conn{
		cfg:     cfg,
		link:    l,
		addr:    addr,
		opt:     opt,
		devID:   cfg.Device.ID,
		timeout: cfg.Device.Timeout.Duration(),
	}, nil
}

// tcpLink 给串口服务器套上总线锁与 Drain 语义。
//
// Drain 在 TCP 上做不了「清空内核缓冲」；能做的只是把已到达的残留字节
// 读掉。这个区别值得写清楚：串口服务器上偶发的「读到上一轮应答」只能
// 靠帧同步（codec.Reader 的扫描）兜底，而不是靠 tcflush。
type tcpLink struct {
	st transport.Stream
	mu sync.Mutex
}

func (t *tcpLink) Write(p []byte) error           { return t.st.Write(p) }
func (t *tcpLink) ReadFull(n int) ([]byte, error) { return t.st.ReadFull(n) }
func (t *tcpLink) Close() error                   { return t.st.Close() }
func (t *tcpLink) Lock()                          { t.mu.Lock() }
func (t *tcpLink) Unlock()                        { t.mu.Unlock() }

func (t *tcpLink) SetDeadline(dl time.Time) error {
	if s, ok := t.st.(transport.DeadlineSetter); ok {
		return s.SetDeadline(dl)
	}
	return nil
}

// Drain 是一次「尽力而为」的排空：把 1 毫秒内能读到的字节丢掉。
func (t *tcpLink) Drain() error {
	if s, ok := t.st.(transport.DeadlineSetter); ok {
		_ = s.SetDeadline(time.Now().Add(time.Millisecond))
	}
	for i := 0; i < 64; i++ {
		if _, err := t.st.ReadFull(1); err != nil {
			break
		}
	}
	return nil
}

// addressOf 从设备标签里取表地址。
//
// 为什么不用 Device.UnitID：那个字段是 0..247 的单字节从站号，被所有
// 其它协议共用；645 的地址是 12 位 BCD，塞进去只能截断，而且会让
// 「广播地址 999999999999」这种合法值无法表达。
func addressOf(dev model.Device) ([dlt645.AddrLen]byte, error) {
	var out [dlt645.AddrLen]byte
	s := strings.TrimSpace(dev.Labels["address"])
	if s == "" {
		return out, driver.E(driver.ErrUnsupported, "open", dev.ID,
			"device.labels.address required (12 位 BCD 表地址)", false, nil)
	}
	a, err := dlt645.ParseAddr(s)
	if err != nil {
		return out, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
	}
	return a, nil
}

// parseOptions 读取通道 options 里的写命令开关。
//
// 用逗号分隔的字符串而不是在 model.Channel 上加一串 bool 字段：这些开关
// 只有一个协议用，加字段会让所有通道的 JSON 都多出几个永远为 false 的键。
func parseOptions(dev model.Device) Options {
	opt := Options{
		Password: 0x02, // 标准里 02 是「普通表明文」权限
		Gap:      defaultGap,
		Preamble: dlt645.PreambleLen,
	}
	if dev.Labels == nil {
		return opt
	}
	raw := dev.Labels["options"]
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "allow-write":
			opt.AllowWrite = !hasVal || truthy(val)
		case "allow-control":
			opt.AllowControl = !hasVal || truthy(val)
		case "preamble":
			if hasVal {
				n, err := parseInt(val)
				if err == nil && n >= 0 && n <= 8 {
					opt.Preamble = n
				}
			}
		case "gap":
			if hasVal {
				if d, err := time.ParseDuration(val); err == nil && d > 0 && d <= time.Second {
					opt.Gap = d
				}
			}
		case "password":
			if hasVal {
				if b, err := parseHexBytes(val); err == nil && len(b) == 3 {
					opt.PasswordB = [3]byte{b[0], b[1], b[2]}
				}
			}
		}
	}
	return opt
}

func truthy(s string) bool {
	switch strings.ToLower(s) {
	case "false", "0", "no", "off":
		return false
	}
	return true
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

func parseHexBytes(s string) ([]byte, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if len(s)%2 != 0 {
		return nil, errors.New("odd hex length")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var v byte
		if _, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &v); err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	cfg     driver.OpenConfig
	link    link
	addr    [dlt645.AddrLen]byte
	opt     Options
	devID   string
	timeout time.Duration

	// mu 串行化本连接上的事务。
	//
	// 总线锁（Ref.Lock）只保证「同一根线上不同设备不互相抢」，它不阻止
	// 同一个设备的两个事务并发——而 645 没有事务号，两个交错的请求会
	// 让应答无法配对。
	mu      sync.Mutex
	closed  bool
	seq     byte // 读后续帧的帧序号
	linkErr error
}

func (c *conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.link.Close()
}

// Probe 读通信地址（0x13）验证链路。
//
// 这是唯一不依赖点表的读命令：地址域全 AA，任何一台表都必须应答。
// 它同时验证了三件事——波特率/校验位对、帧分隔符对、以及**总线上真的
// 有一台表**。比「读一个 DI 然后猜为什么没响应」快得多。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", false, nil)
	}
	req, err := dlt645.ReadAddrRequest()
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, "probe")
	if err != nil {
		return err
	}
	fr, err := resp.Frame()
	if err != nil {
		return driver.E(driver.ErrProtocol, "probe", c.devID, err.Error(), true, err)
	}
	got, err := dlt645.ParseAddrReply(fr)
	if err != nil {
		return driver.E(driver.ErrProtocol, "probe", c.devID, err.Error(), true, err)
	}
	if want := c.addr; want[0] != dlt645.Wildcard && !dlt645.MatchAddr(got, want) {
		return driver.E(driver.ErrProtocol, "probe", c.devID,
			fmt.Sprintf("应答地址 %s 与配置 %s 不一致",
				dlt645.FormatAddr(got), dlt645.FormatAddr(want)), false, nil)
	}
	c.linkErr = nil
	return nil
}

// Read 逐点读。DI 型寻址无法合并，所以每个 span 里的每个点都是一次事务。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", false, nil)
	}

	var out []driver.RawSample
	var firstErr error
	ok := 0
	for _, span := range spans {
		for _, ref := range span.Points {
			if err := ctx.Err(); err != nil {
				return out, driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			}
			// 链路级故障（超时/断开）不是「这个点没有」，继续发请求只会
			// 让一轮采集把设备超时乘以点数。读到第一个链路错误就中止，
			// 剩下的点标记为坏——这与 IEC 104 的处置一致。
			if c.linkErr != nil {
				out = append(out, bad(ref, c.linkErr))
				continue
			}
			s, err := c.readPoint(ctx, ref)
			if err != nil {
				out = append(out, bad(ref, err))
				if firstErr == nil {
					firstErr = err
				}
				if isLinkFailure(err) {
					c.linkErr = err
					continue
				}
				continue
			}
			ok++
			out = append(out, s)
		}
	}
	if ok == 0 && firstErr != nil {
		return out, firstErr
	}
	return out, nil
}

// readPoint 读一个 DI，自动跟随后续帧。
func (c *conn) readPoint(ctx context.Context, ref driver.PointRef) (driver.RawSample, error) {
	di, item, err := resolve(ref)
	if err != nil {
		return driver.RawSample{}, err
	}
	req, err := dlt645.ReadRequest(c.addr, di)
	if err != nil {
		return driver.RawSample{}, driver.E(driver.ErrInternal, "read", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, "read")
	if err != nil {
		return driver.RawSample{}, err
	}
	fr, err := resp.Frame()
	if err != nil {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID, err.Error(), true, err)
	}
	if err := c.checkAddr(fr, "read"); err != nil {
		return driver.RawSample{}, err
	}
	reply, err := dlt645.ParseReadReply(fr, di)
	if err != nil {
		return driver.RawSample{}, c.classify(di, "read", err)
	}
	data := append([]byte(nil), reply.Data...)

	// 后续帧：帧序号从 1 开始，每次 +1（255 之后回 1）。
	//
	// 必须在这里跟完而不是把半包交给上层：一条被截断的电能读数会以
	// 「比实际少了几位」的形式入账，而且看不出任何异常。
	seq := byte(0)
	for reply.Follow {
		seq++
		if seq == 0 {
			seq = 1
		}
		next, nerr := c.readNext(ctx, di, seq)
		if nerr != nil {
			return driver.RawSample{}, nerr
		}
		data = append(data, next.Data...)
		reply.Follow = next.Follow
		if len(data) > dlt645.MaxData*maxFollow {
			return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID,
				"后续帧数据超过上限", false, nil)
		}
	}
	if reply.Seq != 0 {
		// 首帧不带序号；这里只是把它清成 0 以便下面统一处理。
		reply.Seq = 0
	}

	return c.sample(ref, item, data)
}

func (c *conn) readNext(ctx context.Context, di dlt645.DI, seq byte) (dlt645.ReadReply, error) {
	req, err := dlt645.ReadNextRequest(c.addr, di, seq)
	if err != nil {
		return dlt645.ReadReply{}, driver.E(driver.ErrInternal, "read", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, "read-next")
	if err != nil {
		return dlt645.ReadReply{}, err
	}
	fr, err := resp.Frame()
	if err != nil {
		return dlt645.ReadReply{}, driver.E(driver.ErrProtocol, "read", c.devID, err.Error(), true, err)
	}
	if err := c.checkAddr(fr, "read-next"); err != nil {
		return dlt645.ReadReply{}, err
	}
	reply, err := dlt645.ParseReadNextReply(fr, di, seq)
	if err != nil {
		return dlt645.ReadReply{}, c.classify(di, "read-next", err)
	}
	return reply, nil
}

// sample 把数据域解码成平台值。
func (c *conn) sample(ref driver.PointRef, item dlt645.Item, data []byte) (driver.RawSample, error) {
	if len(data) == 0 {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("%s（%s）应答没有数据", item.Name, item.DI), false, nil)
	}
	// 数据块是「若干数据项的拼接」，没有单一数值含义（例如电能数据块是
	// 15 个 4 字节项的串联）。把它硬解成一个 float64 只会给出一个看起来
	// 像读数、实际是前 8 个字节的假值；因此数据块只按 bytes 交付，由上层
	// 或后处理按项拆分。
	if item.DI.Block() {
		if ref.DataType != model.TypeBytes && ref.DataType != model.TypeString {
			return driver.RawSample{}, driver.E(driver.ErrUnsupported, "read", c.devID,
				fmt.Sprintf("%s（%s）是数据块，点位 dataType 必须是 bytes", item.Name, item.DI),
				false, nil)
		}
		return driver.RawSample{
			PointID: ref.ID,
			Value:   model.BytesValue(data),
			Quality: model.QualityGood,
		}, nil
	}
	// 数据块的宽度由响应决定；非数据块若长度不符，说明对端回的不是
	// 这个数据项——宁可报错，也不要把错位字节按小数位解释。
	if item.Width > 0 && len(data) != item.Width {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("%s（%s）应返回 %d 字节，实际 %d 字节",
				item.Name, item.DI, item.Width, len(data)), false, nil)
	}
	v, err := dlt645.Decode(item, data)
	if err != nil {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID, err.Error(), false, err)
	}
	return driver.RawSample{
		PointID: ref.ID,
		Value:   model.NumValue(numericType(ref.DataType, item), v),
		Quality: model.QualityGood,
	}, nil
}

// numericType 决定用什么 DataType 承载解码结果。
//
// 协议里的 3 字节量（电流/功率）在平台没有对应类型，一律用 float64 承载：
// 值已经按 DI 的小数位换算成工程值，再用一个「未定义宽度」的整数类型去
// 存只会让北向多一层解释。
func numericType(dt model.DataType, item dlt645.Item) model.DataType {
	switch item.Kind {
	case dlt645.KindTime, dlt645.KindDate, dlt645.KindAddr, dlt645.KindDemand, dlt645.KindBCD:
		return model.TypeFloat64
	}
	if dt == model.TypeBool || dt == model.TypeString || dt == model.TypeBytes ||
		dt == model.TypeBCD16 || dt == model.TypeBCD32 {
		return model.TypeFloat64
	}
	if !dt.Valid() {
		return model.TypeFloat64
	}
	return dt
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "write", c.devID, "connection closed", false, nil)
	}
	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		if err := ctx.Err(); err != nil {
			out = append(out, driver.WriteResult{PointID: req.PointID,
				Err: driver.E(driver.ErrTimeout, "write", c.devID, "context done", true, err)})
			continue
		}
		out = append(out, c.writeOne(ctx, req))
	}
	return out, nil
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) driver.WriteResult {
	res := driver.WriteResult{PointID: req.PointID}
	di, item, err := resolveWrite(req)
	if err != nil {
		res.Err = err
		return res
	}
	if !item.Writable {
		// 标准里这些数据项只读。允许写会让「配置错一个 DI」变成一次
		// 对电表的非法编程，而电表通常只回「其它错误」。可写性只认 DI
		// 表：不按 DI 区间开例外——那会把状态字也放进来，而状态字恰恰
		// 是最容易被误配成「可写」的一类点位。
		res.Err = driver.E(driver.ErrPermission, "write", c.devID,
			fmt.Sprintf("%s（%s）是只读数据项", item.Name, di), false, nil)
		return res
	}
	if !c.opt.AllowWrite {
		res.Err = driver.E(driver.ErrPermission, "write", c.devID,
			"未开启写权限（通道 options 需包含 allow-write）", false, nil)
		return res
	}

	payload, err := encodeWriteValue(item, req)
	if err != nil {
		res.Err = err
		return res
	}
	frame, err := dlt645.WriteRequest(c.addr, di, c.opt.Password, c.opt.PasswordB,
		c.opt.Operator, payload)
	if err != nil {
		res.Err = driver.E(driver.ErrInternal, "write", c.devID, err.Error(), false, err)
		return res
	}
	resp, err := c.exchange(ctx, frame, "write")
	if err != nil {
		res.Err = err
		return res
	}
	fr, err := resp.Frame()
	if err != nil {
		res.Err = driver.E(driver.ErrProtocol, "write", c.devID, err.Error(), true, err)
		return res
	}
	if err := c.checkAddr(fr, "write"); err != nil {
		res.Err = err
		return res
	}
	if err := dlt645.WriteAck(fr); err != nil {
		res.Err = c.classify(di, "write", err)
		return res
	}
	res.OK = true
	res.Quality = model.QualityGood
	return res
}

// encodeWriteValue 按 DI 的定义把平台值编码成参数字节。
func encodeWriteValue(item dlt645.Item, req driver.WriteRequest) ([]byte, error) {
	switch item.Kind {
	case dlt645.KindTime:
		t, ok := req.Value.AsFloat()
		if !ok {
			return nil, driver.E(driver.ErrUnsupported, "write", "", "时间类数据项需要数值时间", false, nil)
		}
		tt, terr := fromCompact(t)
		if terr != nil {
			return nil, driver.E(driver.ErrUnsupported, "write", "", terr.Error(), false, terr)
		}
		return dlt645.EncodeTime(tt), nil
	case dlt645.KindDate:
		t, ok := req.Value.AsFloat()
		if !ok {
			return nil, driver.E(driver.ErrUnsupported, "write", "", "日期类数据项需要数值时间", false, nil)
		}
		tt, terr := fromDateCompact(t)
		if terr != nil {
			return nil, driver.E(driver.ErrUnsupported, "write", "", terr.Error(), false, terr)
		}
		b, err := dlt645.EncodeDate(tt)
		if err != nil {
			return nil, driver.E(driver.ErrUnsupported, "write", "", err.Error(), false, err)
		}
		return b, nil
	case dlt645.KindAddr:
		if req.Value.Type != model.TypeString {
			return nil, driver.E(driver.ErrUnsupported, "write", "",
				"地址类数据项需要字符串值（12 位 BCD）", false, nil)
		}
		b, err := dlt645.EncodeAddr(req.Value.Text)
		if err != nil {
			return nil, driver.E(driver.ErrUnsupported, "write", "", err.Error(), false, err)
		}
		return b, nil
	}
	v, ok := req.Value.AsFloat()
	if !ok {
		return nil, driver.E(driver.ErrUnsupported, "write", "", "需要数值", false, nil)
	}
	b, err := dlt645.EncodeValue(item, v)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "write", "", err.Error(), false, err)
	}
	return b, nil
}

// fromDateCompact 把「日期类」数据项的数值还原成时间。
//
// 4 字节日期数据项（结算日、时区日等）在线路上只有 MMDDhhmm，没有年；
// 读出来的值因此是 8 位而非 14 位。写回时必须接受同形的值——否则「把刚
// 读到的结算日原样写回」会得到一个月=0 的垃圾日期。14 位输入仍然接受，
// 因为 CLI 与人更习惯完整时间。
func fromDateCompact(v float64) (time.Time, error) {
	n := int64(v)
	if n >= 1e12 {
		return fromCompact(v)
	}
	if n < 1e6 || n > 12312359 {
		return time.Time{}, fmt.Errorf("日期值 %v 必须是 MMDDhhmm 或 YYYYMMDDhhmmss", v)
	}
	mon := int(n / 1e6)
	day := int(n / 1e4 % 100)
	hour := int(n / 100 % 100)
	min := int(n % 100)
	if err := checkDateParts(mon, day, hour, min, 0); err != nil {
		return time.Time{}, err
	}
	// 年取 0：EncodeDate 只用月/日/时/分，用一个具体年份只会让调用方
	// 误以为这个数据项里真的有年。
	return time.Date(0, time.Month(mon), day, hour, min, 0, 0, time.Local), nil
}

// fromCompact 把 Decode 输出的 YYYYMMDDhhmmss 还原成时间。
//
// 平台的 Value 没有时间类型，读时压成一个可排序的数，写时再解回来。这个
// 往返必须放在同一处，否则读和写对「20261107123456」的解释会分叉。
func fromCompact(v float64) (time.Time, error) {
	n := int64(v)
	sec := int(n % 100)
	n /= 100
	min := int(n % 100)
	n /= 100
	hour := int(n % 100)
	n /= 100
	day := int(n % 100)
	n /= 100
	mon := int(n % 100)
	n /= 100
	year := int(n)
	if year < 100 {
		year += 2000
	}
	if year < 2000 || year > 2099 {
		return time.Time{}, fmt.Errorf("年份 %d 超出 2000..2099", year)
	}
	if err := checkDateParts(mon, day, hour, min, sec); err != nil {
		return time.Time{}, err
	}
	t := time.Date(year, time.Month(mon), day, hour, min, sec, 0, time.Local)
	// 字段都合法但日期不存在（2 月 30 日）时 time.Date 会进到下个月。
	// 反向比对一次，避免把「20260230120000」写进电表。
	if t.Year() != year || int(t.Month()) != mon || t.Day() != day {
		return time.Time{}, fmt.Errorf("%04d-%02d-%02d 不存在", year, mon, day)
	}
	return t, nil
}

func checkDateParts(mon, day, hour, min, sec int) error {
	switch {
	case mon < 1 || mon > 12:
		return fmt.Errorf("月份 %d 必须在 1..12", mon)
	case day < 1 || day > 31:
		return fmt.Errorf("日 %d 必须在 1..31", day)
	case hour < 0 || hour > 23:
		return fmt.Errorf("小时 %d 必须在 0..23", hour)
	case min < 0 || min > 59:
		return fmt.Errorf("分钟 %d 必须在 0..59", min)
	case sec < 0 || sec > 59:
		return fmt.Errorf("秒 %d 必须在 0..59", sec)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// reply 把收到的帧连同原始字节一起带回。
//
// 保留原始字节是为了失败样本：现场排查「为什么这个点读不到」时，光有
// 「校验和错误」四个字没有用，需要看到实际收到的那 20 个字节。
type reply struct {
	fr  dlt645.Frame
	raw []byte
}

func (r reply) Frame() (dlt645.Frame, error) {
	if len(r.raw) == 0 && r.fr.Ctrl == 0 {
		return dlt645.Frame{}, errors.New("dlt645: 空应答")
	}
	return r.fr, nil
}

// exchange 发一帧并收一帧。
//
// 全程持有总线锁：从「清空接收缓冲」到「读完整帧」之间不能有其它设备
// 的事务插入，否则收到的字节会来自另一台表。
func (c *conn) exchange(ctx context.Context, req []byte, op string) (reply, error) {
	c.link.Lock()
	defer c.link.Unlock()

	// 帧间静默：给上一轮的总线另一次机会从发送切回接收。
	if c.opt.Gap > 0 {
		select {
		case <-ctx.Done():
			return reply{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		case <-time.After(c.opt.Gap):
		}
	}
	// 半双工纪律：发之前清空接收缓冲，避免把上一轮迟到的响应当成本轮。
	//
	// 顺序很关键：Drain 会临时把读截止时间压到「现在」，如果先设好本次
	// 事务的截止时间再 Drain，那个截止时间就被压掉了，接下来的 Write
	// 会立刻超时——症状是「连上了，但每次写都 timeout」。
	_ = c.link.Drain()
	if ctxDeadline, ok := ctx.Deadline(); ok {
		_ = c.link.SetDeadline(ctxDeadline)
	} else {
		_ = c.link.SetDeadline(time.Now().Add(c.deadline()))
	}

	if c.opt.Preamble != dlt645.PreambleLen {
		req = withPreamble(req, c.opt.Preamble)
	}
	if err := c.link.Write(req); err != nil {
		c.linkErr = driver.E(driver.ErrDisconnected, op, c.devID, "串口写入失败", true, err)
		return reply{}, c.linkErr
	}

	rd := dlt645.NewReader(c.link)
	fr, err := rd.Next()
	if err != nil {
		e := c.readError(op, err, ctx)
		if isLinkFailure(e) {
			c.linkErr = e
		}
		return reply{}, e
	}
	c.linkErr = nil
	return reply{fr: fr}, nil
}

func (c *conn) deadline() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return 3 * time.Second
}

func (c *conn) readError(op string, err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.devID, "等待应答超时", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.devID, "串口读取失败", true, err)
}

// checkAddr 校验应答地址。
//
// 缩位寻址时从站必须回**实际**地址，主站必须校验：多表总线上收到别的表
// 的应答如果被接受，值会记到错误的设备头上，而且完全看不出异常。
func (c *conn) checkAddr(fr dlt645.Frame, op string) error {
	if dlt645.MatchAddr(fr.Addr, c.addr) {
		return nil
	}
	return driver.E(driver.ErrProtocol, op, c.devID,
		fmt.Sprintf("应答地址 %s 与请求 %s 不匹配（总线上可能有另一台表在应答）",
			fr.AddrString(), dlt645.FormatAddr(c.addr)), true, nil)
}

// classify 把编解码层的错误映射为驱动错误分类。
func (c *conn) classify(di dlt645.DI, op string, err error) error {
	var ex *dlt645.Exception
	if errors.As(err, &ex) {
		switch ex.Kind() {
		case dlt645.ExceptionAddress:
			return driver.E(driver.ErrAddress, op, c.devID,
				fmt.Sprintf("%s：%s", di, ex.Text()), false, err)
		case dlt645.ExceptionPermission:
			return driver.E(driver.ErrPermission, op, c.devID,
				fmt.Sprintf("%s：%s", di, ex.Text()), false, err)
		}
		return driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("%s：%s", di, ex.Text()), true, err)
	}
	var ck *dlt645.ChecksumError
	if errors.As(err, &ck) {
		return driver.E(driver.ErrProtocol, op, c.devID, err.Error(), true, err)
	}
	return driver.E(driver.ErrProtocol, op, c.devID, err.Error(), true, err)
}

// ---------------------------------------------------------------------------
// 便利函数
// ---------------------------------------------------------------------------

// resolve 把点表地址解析成 DI 与数据项定义。
func resolve(ref driver.PointRef) (dlt645.DI, dlt645.Item, error) {
	di, err := parseDI(ref.Address.Area)
	if err != nil {
		return dlt645.DI{}, dlt645.Item{}, err
	}
	item, ok := dlt645.Lookup(di)
	if !ok {
		return dlt645.DI{}, dlt645.Item{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("未收录的数据标识 %s（点表需改用标准 DI）", di), false, nil)
	}
	// 数据块由点表给长度；非数据块的宽度以 DI 表为准，点表写错在这里
	// 就拦住，而不是等应答长度不符再报一个含糊的「字节数不对」。
	if item.Width == 0 {
		if ref.Address.Length > 0 {
			item.Width = ref.Address.Length
		}
	}
	return di, item, nil
}

func resolveWrite(req driver.WriteRequest) (dlt645.DI, dlt645.Item, error) {
	di, err := parseDI(req.Address.Area)
	if err != nil {
		return dlt645.DI{}, dlt645.Item{}, err
	}
	item, ok := dlt645.Lookup(di)
	if !ok {
		return dlt645.DI{}, dlt645.Item{}, driver.E(driver.ErrAddress, "write", "",
			fmt.Sprintf("未收录的数据标识 %s", di), false, nil)
	}
	if item.Width == 0 && req.Address.Length > 0 {
		item.Width = req.Address.Length
	}
	return di, item, nil
}

func parseDI(s string) (dlt645.DI, error) {
	di, err := dlt645.ParseDI(s)
	if err != nil {
		return dlt645.DI{}, driver.E(driver.ErrAddress, "read", "", err.Error(), false, err)
	}
	return di, nil
}

// bad 生成一条坏样本。失败样本必须带上点位 ID：采集层按 ID 归位，
// 少一条会让北向看到一个「卡在旧值」的点，比明确报错更危险。
func bad(ref driver.PointRef, err error) driver.RawSample {
	return driver.RawSample{
		PointID: ref.ID,
		Quality: model.QualityCommError,
		Err:     err,
	}
}

// isLinkFailure 判断错误是否意味着「这一轮别再发了」。
func isLinkFailure(err error) bool {
	switch driver.KindOf(err) {
	case driver.ErrTimeout, driver.ErrDisconnected:
		return true
	}
	return false
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// withPreamble 按需重写前导字节个数。
func withPreamble(frame []byte, n int) []byte {
	i := 0
	for i < len(frame) && frame[i] == dlt645.Preamble {
		i++
	}
	out := make([]byte, 0, n+len(frame)-i)
	for j := 0; j < n; j++ {
		out = append(out, dlt645.Preamble)
	}
	return append(out, frame[i:]...)
}
