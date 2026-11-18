// Package iec104 实现 IEC 60870-5-104 主站驱动（TCP 2404）。
//
// 这个驱动与寄存器型驱动有一个根本差别：**数据不是「读一次答一次」，而是
// 从站按变化主动上送**。主站能做的只有三件事：
//
//  1. 建立链路（STARTDT），并做一次总召唤（站召唤）拿到初始快照；
//  2. 持续接收从站上送的 I 帧，维护一张「IOA → 最新值」表；
//  3. 下发命令（单点/双点/设定值），等待激活确认。
//
// 因此 Read 与 MQTT 驱动一样，**不做任何网络交互**，只从本地表取数。试图
// 在 Read 里「发一条请求等应答」在 104 里没有对应的协议动作——你能发的只
// 有总召唤，而总召唤返回整站的快照，代价与点位数量无关，也与「读一个点」
// 这件事无关。
//
// 一个容易误导的实现选择：给缓存加「多久没更新就 stale」。104 的从站只在
// 值变化时上送，一个温度三十分钟不变是正常的，按时间降级会把整页点位刷成
// stale。所以这里的质量码只来自两处：ASDU 里的品质位（IV/NT/SB/BL/OV），
// 以及链路本身是否还活着（TESTFR 往返）。链路死了由 Read 返回整体错误，
// 上层会把所有点位标成 comm_error——这才是有意义的信号。
//
// 能力边界：只做平衡式 TCP 主站 + 总召唤 + 单点/双点/设定值命令；不做
// 101 串口、不做文件传输、不做时钟同步的写路径、不做冗余通道切换、不做
// 带时标的命令。总召唤限定词固定为站召唤（20）。
package iec104

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"uic/internal/codec/iec104"
	"uic/internal/codec/iec60870"
	"uic/internal/driver"
	"uic/internal/model"
)

// 默认定时参数。k/w 是「未确认 I 帧数」而不是字节数；t1/t2 分别对应
// 「发送后等确认」与「收到数据后最迟确认」。
const (
	defaultK = 12
	// defaultW 是接收窗口：未确认的收到帧达到这个数就立即确认，不必等
	// t2。标准把「等满 w 帧」与「等满 t2」并列为确认时机，两者缺一都
	// 会在对端窗口上出问题。
	defaultW  = 8
	defaultT1 = 15 * time.Second
	// defaultT2 是「收到数据后最多延迟多久确认」。标准默认 10 秒，这里
	// 取 1 秒：t2 只是**上限**，早确认永远合法，而 10 秒的延迟会让对端
	// 的发送窗口（同样 12 帧）在整个周期里处于半空状态，一条突发上送
	// 密集的链路会明显降速。
	defaultT2 = 1 * time.Second
	// defaultTestIdle 是链路上没有任何报文多久之后发一次 TESTFR act。
	// 选 20 秒：远小于绝大多数 NAT 表项的空闲超时（通常 2–10 分钟），
	// 又不会让一条安静但健康的链路每分钟都被打扰。
	defaultTestIdle = 20 * time.Second

	// maxObserved 是「设备实际送了什么」旁路表的条目上限。
	//
	// 现场点表通常是几百到几千个 IOA，4096 足够；设上限是因为 IOA 是 24 位
	// 的，一个异常从站（或错误配置的转发器）可以合法地送出几百万个不同
	// IOA，无上限的旁路表会把进程内存吃光。超限后只更新已知 IOA，新 IOA
	// 丢弃并计数。
	maxObserved = 4096
)

// 地址 area 的取值与「点位语义」层共用一份实现（见 iec60870 包）：同一套
// 对象类别、同一条命令推导规则，串行承载与以太网承载没有理由不同。
const (
	AreaIOA                = iec60870.AreaIOA
	AreaSingleCommand      = iec60870.AreaSingleCommand
	AreaDoubleCommand      = iec60870.AreaDoubleCommand
	AreaSetpointFloat      = iec60870.AreaSetpointFloat
	AreaSetpointScaled     = iec60870.AreaSetpointScaled
	AreaSetpointNormalized = iec60870.AreaSetpointNormalized
)

// DefaultPort 是 IEC 60870-5-104 的默认 TCP 端口。
const DefaultPort = iec104.DefaultPort

// link 是本驱动需要的链路能力。
//
// 不用 transport.Stream 是有原因的：那个抽象给每次 ReadFull 都压一个 5 秒
// 兜底 deadline，适合「一问一答」的事务，但 104 的接收循环要长期挂着等下
// 一帧上送。用它的后果是每分钟触发若干次超时，然后被当成断链。这里改用
// net.Conn：接收循环自己管理 deadline，写侧用 SetWriteDeadline 保证不会
// 无限阻塞。
type link interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// Dialer 建立链路。抽成函数是为了让测试能注入一条不联网的管道。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (link, error)

// Driver 是 IEC 104 主站驱动。
type Driver struct {
	dial Dialer

	// testIdle 是空闲多久发一次 TESTFR。0 表示用默认值。
	// 测试里可以调小，验证「静默链路会被判死」。
	testIdle time.Duration
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (link, error) {
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

// Meta 声明 IEC 104 的能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "iec104",
		Name:         "IEC 60870-5-104 (client)",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read: driver.CapSupported,
			// 写入 = 下发命令并等待激活确认。声明 supported 是诚实的：
			// 我们确实等到设备的确认才返回成功，不是「发出去了就算成功」。
			Write: driver.CapSupported,
			// 从站主动上送，由驱动内部消化成最新值表；上层拿到的仍是
			// Read 拉取契约。若声明 unsupported，配置界面会建议换协议。
			Subscribe: driver.CapSupported,
			Browse:    driver.CapUnsupported,
			Discovery: driver.CapUnsupported,
			// 读本地表，成本与点位数量无关。
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.30:2404", Scope: "channel",
				Help: "104 从站地址；省略端口时按 2404 处理"},
			{Name: "device.unitId", Label: "公共地址（CA）", Type: "int", Default: 1, Scope: "device",
				Help: "ASDU 的公共地址，1..65535；0 表示广播，不允许"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "104 有 15 位发送/接收序号，必须串行"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"register"}, Scope: "point"},
			{Name: "address.area", Label: "对象类别", Type: "enum", Required: true,
				Enum: []string{AreaIOA, AreaSingleCommand, AreaDoubleCommand,
					AreaSetpointFloat, AreaSetpointScaled, AreaSetpointNormalized},
				Scope: "point",
				Help:  "ioa 为监视方向；其余为命令类型，写入时按它下发"},
			{Name: "address.offset", Label: "信息对象地址（IOA）", Type: "int", Required: true,
				Scope: "point", Help: "1..16777215"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64",
					"float32", "float64"}, Scope: "point"},
		},
		Description: "IEC 60870-5-104 主站：总召唤 + 突发上送 + 单点/双点/设定值命令",
	}
}

// Open 建立 TCP 链路、启动 STARTDT 并发起总召唤。
//
// 只等 STARTDT 确认。总召唤的整站快照可能有几千条 ASDU，等它全部收完会让
// 每次重连的耗时与从站规模成正比；快照由后台接收循环逐步填进缓存，重连后
// 头一两轮采集出现「还没收到」是完全正常的。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("iec104 requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil && !strings.Contains(addr, ":") {
		addr = fmt.Sprintf("%s:%d", addr, DefaultPort)
	}

	ca := cfg.Device.UnitID
	if ca == 0 {
		ca = 1
	}
	if ca < 1 || ca > 65535 {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID,
			fmt.Sprintf("device.unitId (common address) %d out of range 1..65535", ca), false, nil)
	}

	plans, byIOA, err := buildPlans(cfg.Device)
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"device has no enabled points", false, nil)
	}

	l, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}

	c := &conn{
		link:     l,
		deviceID: cfg.Device.ID,
		addr:     addr,
		ca:       uint16(ca),
		timeout:  cfg.Channel.EffectiveTimeout(),
		points:   plans,
		byIOA:    byIOA,
		values:   make(map[string]cachedValue, len(plans)),
		done:     make(chan struct{}),
		ackCh:    make(chan struct{}, 1),
		started:  make(chan struct{}),
		pending:  map[uint32]chan iec104.ASDU{},
		dead:     make(chan struct{}),
	}
	if d.testIdle > 0 {
		c.testIdle = d.testIdle
	} else {
		c.testIdle = defaultTestIdle
	}
	c.lastRecv.Store(time.Now().UnixNano())

	go c.readLoop()
	go c.keeper()

	if err := c.startDataTransfer(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	// 总召唤失败不拆链：从站可能还没就绪，突发数据仍会到达。
	if err := c.interrogate(); err != nil {
		cfg.Log.Warn("初期总召唤未完成，等待从站上送",
			"device", cfg.Device.ID, "error", err)
	}
	// 累计量（电能量）不在站召唤范围内，必须单独召唤一次。只发站召唤的
	// 实现在现场表现为「电度点位永远是 bad」——它其实只是走另一条召唤
	// 路径。
	//
	// 无条件发送：主站无法从点表推断设备有没有累计量（累计量是设备用哪
	// 种 ASDU 上送决定的，不是点表属性）。没有累计量的设备会回一条否定
	// 确认「未知类型」，协议上完全正常，这里按调试日志处理，不影响连接。
	if err := c.counterInterrogate(); err != nil {
		cfg.Log.Debug("初期电能量召唤未完成", "device", cfg.Device.ID, "error", err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 点位规划
// ---------------------------------------------------------------------------

type pointPlan struct {
	ref      driver.PointRef
	ioa      uint32
	area     string
	dataType model.DataType
}

func buildPlans(dev model.Device) (map[string]*pointPlan, map[uint32][]*pointPlan, error) {
	plans := make(map[string]*pointPlan, len(dev.Points))
	byIOA := map[uint32][]*pointPlan{}
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		plan, err := planPoint(driver.PointRefOf(dev, p))
		if err != nil {
			return nil, nil, err
		}
		plans[plan.ref.ID] = plan
		byIOA[plan.ioa] = append(byIOA[plan.ioa], plan)
	}
	return plans, byIOA, nil
}

func planPoint(p driver.PointRef) (*pointPlan, error) {
	// 104 的 IOA 固定 3 字节，上限 16777215；规划逻辑本身与承载无关，
	// 放在 iec60870 里与 101 共用。
	pl, err := iec60870.PlanPoint(p, "iec104", 0xFFFFFF)
	if err != nil {
		return nil, err
	}
	return &pointPlan{ref: pl.Ref, ioa: pl.IOA, area: pl.Area, dataType: pl.DataType}, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type cachedValue struct {
	value model.Value
	q     model.Quality
	at    time.Time
}

// Observed 是「设备在上送方向实际发过的一个信息对象」的原始记录。
//
// 它与点位表是两条独立的路径，解决的是另一个问题：点位表回答「我配置的
// 点现在是什么值」，而现场初次接入某台设备时真正的问题是「它到底送哪些
// IOA、用哪种 ASDU 类型、带不带时标」。这个问题无法用配置推测（同一个值
// 既能用标度化送也能用短浮点送，点位声明的 dataType 只是我们怎么存它），
// 只能由链路自己记录下来。
//
// 保存的是解码后的原始对象（不是转换到点位类型之后的值），因为诊断要看
// 的正是「设备发的原样」。
type Observed struct {
	IOA    uint32
	TypeID byte
	Cause  byte
	Object iec104.Object
	At     time.Time
	// Points 是该 IOA 在点位表里映射到的点位 ID（可能为空：设备送了我们
	// 没配置的 IOA，这本身就是要给现场看的信息）。
	Points []string
}

type conn struct {
	link     link
	deviceID string
	addr     string
	ca       uint16
	timeout  time.Duration
	testIdle time.Duration

	points map[string]*pointPlan
	byIOA  map[uint32][]*pointPlan

	// mu 保护缓存、链路状态与等待者。所有「先取状态、再发帧」的路径都
	// 必须先放掉 mu 再进 wmu，反过来也一样，避免自锁。
	mu sync.Mutex
	// values 按**点位 ID** 而不是 IOA 缓存。同一个 IOA 可以既被监视
	// （遥信/遥测）又被遥控（命令点位），两边的平台类型不同：用 IOA
	// 做键会让后写入的一方覆盖另一方，症状是「遥信布尔点显示成
	// double 的 2」。键换成点位 ID 之后，各点位各存一份，互不干扰。
	values   map[string]cachedValue
	linkErr  error
	closed   bool
	testWait chan error
	// badFrames 统计「收到但解析不了」的帧数。它与「一条都没收到」是
	// 两种完全不同的故障，混成一个数字就分不清了。
	badFrames uint64
	// observed 是「设备实际送了什么」的旁路表；dropped 记录因超出
	// maxObserved 而丢弃的 IOA 数。
	observed map[uint32]Observed
	dropped  uint64
	// lastGI 防止 EndOfInit 风暴引发反复总召唤。
	lastGI time.Time

	// 序号由接收循环更新、发送侧只读，因此用原子量。
	recvSeq    atomic.Uint32
	pendingAck atomic.Int32
	lastRecv   atomic.Int64
	// ackSince 是「当前这批待确认帧里最早那一帧的到达时刻」。t2 是**从
	// 收到数据算起**的延迟上限，不是从上一次确认算起——用后者会让一条
	// 稀疏但持续的上送流永远等不到确认。
	ackSince atomic.Int64
	// t2 是收到数据后最多延迟多久确认（0 表示用 defaultT2）。测试里可以
	// 调小，验证「不确认就会被判死」。
	t2 time.Duration

	// wmu 串行化「分配 N(S) + 写帧」。104 没有帧内事务号，两个 I 帧交错
	// 写入会让序号与实际到达顺序不一致，错误现象是「偶尔丢一条命令」。
	wmu     sync.Mutex
	sendSeq uint16
	// unacked 是「已发出、对端尚未确认」的 I 帧数。它必须按对端的 N(R)
	// 精确递减，不能收到任何一帧就清零：一个只确认到更早一帧的 S 帧会把
	// 整个窗口打开，k 就形同虚设，序号会一路写穿到回绕。
	// 由 wmu 保护（与 sendSeq 同一把锁，二者总是一起变）。
	unacked  int
	ackPoint uint16 // 对端已确认到的下一个 N(S)（即最新 N(R)）

	// cmu 串行化命令：同一时刻只允许一条待确认命令，避免确认报文与命令
	// 张冠李戴。设备 actor 本身已经串行，这一层是给直接调用（自检 CLI）
	// 兜底。
	cmu     sync.Mutex
	pending map[uint32]chan iec104.ASDU

	testOutstanding atomic.Bool
	testAt          atomic.Int64

	done      chan struct{}
	closeOnce sync.Once
	started   chan struct{}
	startOnce sync.Once
	ackCh     chan struct{}
	// dead 在链路第一次判死时关闭。done 只在驱动被 Close（通常是驱动整体
	// 卸载）时关，两者语义不同：链路断了但驱动还活着（下一轮会重连），
	// 正在等确认的命令必须立刻醒来返回断连，而不是挂满整个 c.timeout。
	dead     chan struct{}
	deadOnce sync.Once
}

// startDataTransfer 发送 STARTDT act 并等待 con。
func (c *conn) startDataTransfer(ctx context.Context) error {
	if err := c.sendU(iec104.UStartAct); err != nil {
		return driver.E(driver.ErrDisconnected, "open", c.deviceID,
			"send STARTDT act", true, err)
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case <-c.started:
		return nil
	case <-c.done:
		return driver.E(driver.ErrDisconnected, "open", c.deviceID,
			"connection closed before STARTDT con", true, c.errLocked())
	case <-timer.C:
		return driver.E(driver.ErrTimeout, "open", c.deviceID,
			fmt.Sprintf("no STARTDT con within %s", c.timeout), true, nil)
	case <-ctx.Done():
		return driver.E(driver.ErrTimeout, "open", c.deviceID, "context done", true, ctx.Err())
	}
}

// interrogate 发送站总召唤。
func (c *conn) interrogate() error {
	c.mu.Lock()
	if c.closed || c.linkErr != nil {
		err := c.errLocked()
		c.mu.Unlock()
		return driver.E(driver.ErrDisconnected, "interrogate", c.deviceID, "link is down", true, err)
	}
	if time.Since(c.lastGI) < time.Second {
		c.mu.Unlock()
		return nil
	}
	c.lastGI = time.Now()
	c.mu.Unlock()

	asdu, err := iec104.BuildInterrogation(c.ca, iec104.QOIStation)
	if err != nil {
		return driver.E(driver.ErrInternal, "interrogate", c.deviceID, err.Error(), false, err)
	}
	return c.sendASDU(asdu)
}

// counterInterrogate 发送电能量召唤。
func (c *conn) counterInterrogate() error {
	c.mu.Lock()
	if c.closed || c.linkErr != nil {
		err := c.errLocked()
		c.mu.Unlock()
		return driver.E(driver.ErrDisconnected, "interrogate", c.deviceID, "link is down", true, err)
	}
	c.mu.Unlock()
	// 限定词 5：召唤「总的」电能量，不含各费率分项。
	asdu, err := iec104.BuildCounterInterrogation(c.ca, 5)
	if err != nil {
		return driver.E(driver.ErrInternal, "interrogate", c.deviceID, err.Error(), false, err)
	}
	return c.sendASDU(asdu)
}

func (c *conn) errLocked() error {
	if c.linkErr != nil {
		return c.linkErr
	}
	return errors.New("closed")
}

// Close 停止后台协程并关闭链路。
func (c *conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
		// 关闭也是「链路不再可用」，等确认的命令同样要立刻醒来。
		// dead 为 nil 只可能出现在测试直接构造的 conn 上。
		if c.dead != nil {
			c.deadOnce.Do(func() { close(c.dead) })
		}
		// 尽力发 STOPDT act 再断开；失败无所谓——链路可能已经死了。
		_ = c.sendU(iec104.UStopAct)
		err = c.link.Close()
		// 链路可能已经被 fail() 关掉过。此时 Close 返回「already closed」
		// 对上层毫无意义：连接确实已经是关闭的，这正是调用方想要的。
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
	})
	return err
}

// Probe 用 TESTFR 往返证明链路还活着。
//
// 只查「连接标志」是不够的：拔网线时 TCP 不会立刻报错，本地状态仍然是
// 已连接。TESTFR act/con 是协议内唯一一个「强制对端现在回答」的动作。
func (c *conn) Probe(ctx context.Context) error {
	ch := make(chan error, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "connection closed", true, nil)
	}
	if c.linkErr != nil {
		err := c.linkErr
		c.mu.Unlock()
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "link is down", true, err)
	}
	c.testWait = ch
	c.mu.Unlock()

	if err := c.sendU(iec104.UTestAct); err != nil {
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "send TESTFR act", true, err)
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-c.done:
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "connection closed", true, nil)
	case <-c.dead:
		c.mu.Lock()
		inner := c.linkErr
		c.mu.Unlock()
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "link is down", true, inner)
	case <-timer.C:
		return driver.E(driver.ErrTimeout, "probe", c.deviceID,
			fmt.Sprintf("no TESTFR con within %s", c.timeout), true, nil)
	case <-ctx.Done():
		return driver.E(driver.ErrTimeout, "probe", c.deviceID, "context done", true, ctx.Err())
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// Read 从本地最新值表取数，不做任何网络交互（§3.10.9）。
//
// 因此它天然满足 §3.3.1 的「有成功点就返回 nil」：每个点位的坏都有自己的
// 原因（没收到过 / 控制方向 IOA / 设备上报 IV），全部写进各自的样本，不会
// 升级成整轮错误；而「一个点位都没成功」时按契约上抛第一个错误，否则熔断
// 器与告警永远看不到异常。**链路判死**是例外：即便样本看起来都是好的也要
// 上抛，见下方注释。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if err := ctx.Err(); err != nil {
		e := driver.E(driver.ErrTimeout, "read", c.deviceID, "context done", true, err)
		return c.markAllBad(spans, e), e
	}
	c.mu.Lock()
	closed, linkErr := c.closed, c.linkErr
	out := make([]driver.RawSample, 0, len(spans))
	firstErr := error(nil)
	good := 0
	for _, span := range spans {
		for _, p := range span.Points {
			s := c.lookupLocked(p.ID)
			if s.Quality == model.QualityGood || s.Quality == model.QualityUncertain ||
				s.Quality == model.QualityStale {
				good++
			} else if firstErr == nil {
				firstErr = s.Err
			}
			out = append(out, s)
		}
	}
	c.mu.Unlock()

	if closed {
		e := driver.E(driver.ErrDisconnected, "read", c.deviceID, "connection closed", true, nil)
		return c.markAllBad(spans, e), e
	}
	if linkErr != nil {
		e := driver.E(driver.ErrDisconnected, "read", c.deviceID, "link is down", true, linkErr)
		// 链路级失败：整轮样本显式标坏再上抛（§3.3.1 的例外分支）。不这
		// 样做的话，一批可能是几小时前的值会带着 good 继续进缓存与告警。
		return c.markAllBad(spans, e), e
	}
	return out, driver.PartialReadError(good, firstErr)
}

// markAllBad 为 spans 里的每个点位生成一条坏样本。
//
// 数量必须与传入点位一一对应：采集器对「驱动返回的点位少了」的处理是再补
// 一条 protocol/bad（runtime/collector.go），那条兜底消息会把真正的原因
// （断链/超时）盖掉。
func (c *conn) markAllBad(spans []driver.ReadSpan, e error) []driver.RawSample {
	out := make([]driver.RawSample, 0, len(spans))
	for _, span := range spans {
		for _, p := range span.Points {
			out = append(out, driver.RawSample{
				PointID: p.ID, Quality: model.QualityCommError, Err: e,
			})
		}
	}
	return out
}

func (c *conn) lookupLocked(id string) driver.RawSample {
	plan, ok := c.points[id]
	if !ok {
		return driver.RawSample{
			PointID: id, Quality: model.QualityAddressError,
			Err: driver.E(driver.ErrAddress, "read", c.deviceID,
				fmt.Sprintf("point %q is not configured", id), false, nil),
		}
	}
	cv, seen := c.values[plan.ref.ID]
	if !seen {
		msg := fmt.Sprintf("no data received for IOA %d yet (not in the station's transmission set?)", plan.ioa)
		if !c.monitoredLocked(plan.ioa) {
			// 只挂在控制方向的 IOA 在监视方向根本不存在，永远等不到
			// 上送。把它说成「还没收到」会让人一直去查点表/召唤范围，
			// 而真实答案是「这个 IOA 本来就没有状态」——只有写过之后
			// 才有本地缓存值（质量 uncertain）。
			msg = fmt.Sprintf("IOA %d is only used in the control direction (area %q): "+
				"stations report no state for it; write a command to populate the local cache",
				plan.ioa, plan.area)
		}
		return driver.RawSample{
			PointID: id, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.deviceID, msg, true, nil),
		}
	}
	if cv.q == model.QualityBad {
		return driver.RawSample{
			PointID: id, Value: cv.value, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.deviceID,
				fmt.Sprintf("IOA %d reported invalid quality", plan.ioa), true, nil),
		}
	}
	return driver.RawSample{PointID: id, Value: cv.value, Quality: cv.q}
}

// monitoredLocked 判断某个 IOA 是否出现在监视方向的点位里（调用方持锁）。
//
// 同一个 IOA 可以既被召唤（监视）又被遥控（控制），这时「还没收到值」是
// 一条真正的告警；只有那些**仅**在控制方向出现的 IOA 才该给出另一句解释。
func (c *conn) monitoredLocked(ioa uint32) bool {
	for _, p := range c.byIOA[ioa] {
		if p.area == AreaIOA {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// Write 逐条下发命令并等待激活确认。
//
// 一条命令的生命周期是：激活（cause=6）→ 激活确认（cause=7）→ 可选的激活
// 终止（cause=10）。等到 7 就认为设备已接受；否定确认（P/N=1）或 44..47 的
// 「未知类型/原因/公共地址/IOA」会转成分类错误，而不是笼统的「写入失败」。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			// ctx 到期：剩余项一次性标坏，已下发的项保留自己的结果。
			e := driver.E(driver.ErrTimeout, "write", c.deviceID, "context done", true, err)
			for _, rest := range reqs[i:] {
				results = append(results, driver.WriteResult{PointID: rest.PointID, OK: false,
					Quality: model.QualityTimeout, Err: e})
			}
			if firstErr == nil {
				firstErr = e
			}
			return results, firstErr
		}
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// 链路级失败：后续命令不可能成功，逐项标坏后立即上抛，
			// 不再对着一根死链路逐条等超时。
			if driver.IsLinkFailure(err) && i+1 < len(reqs) {
				for _, rest := range reqs[i+1:] {
					e := driver.E(driver.ErrDisconnected, "write", c.deviceID,
						"link is down", true, err)
					results = append(results, driver.WriteResult{PointID: rest.PointID, OK: false,
						Quality: model.QualityCommError, Err: e})
				}
				return results, firstErr
			}
		}
	}
	return results, firstErr
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	if err := ctx.Err(); err != nil {
		e := driver.E(driver.ErrTimeout, "write", c.deviceID, "context done", true, err)
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityTimeout, Err: e}, e
	}
	c.cmu.Lock()
	defer c.cmu.Unlock()

	plan, err := planPoint(driver.PointRef{ID: req.PointID, Address: req.Address, DataType: req.DataType})
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityAddressError, Err: err}, err
	}
	comm, err := buildCommand(plan, req, c.ca)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityBad, Err: err}, err
	}

	ch := make(chan iec104.ASDU, 1)
	c.mu.Lock()
	if c.closed || c.linkErr != nil {
		err := c.errLocked()
		c.mu.Unlock()
		e := driver.E(driver.ErrDisconnected, "write", c.deviceID, "link is down", true, err)
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityCommError, Err: e}, e
	}
	c.pending[plan.ioa] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if cur, ok := c.pending[plan.ioa]; ok && cur == ch {
			delete(c.pending, plan.ioa)
		}
		c.mu.Unlock()
	}()

	// sentAt 用于区分「命令确认之前就已经在缓存里的旧值」与「确认之后从站
	// 自己推上来的新值」。没有这个时间点，回填会覆盖掉一条更新的上送——
	// 而 104 只在变化时上送，被覆盖的值可能很久都不会再来一条。
	sentAt := time.Now()
	if err := c.sendASDU(comm.asdu); err != nil {
		// 发送失败的错误种类决定质量码：写超时是暂时性（可重试），
		// 链路断连是永久。不能一律写成 comm_error，否则上层会把一个
		// 本可重试的软超时当成硬故障处理。
		q := model.QualityCommError
		if driver.KindOf(err) == driver.ErrTimeout {
			q = model.QualityTimeout
		}
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: q, Err: err}, err
	}

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	for {
		select {
		case asdu := <-ch:
			if asdu.Negative ||
				(asdu.Cause != iec104.CauseActivationCon && asdu.Cause != iec104.CauseActivationTerm) {
				e := negativeConfirm(c.deviceID, plan.ioa, asdu)
				q := model.QualityBad
				if driver.KindOf(e) == driver.ErrAddress {
					q = model.QualityAddressError
				}
				return driver.WriteResult{PointID: req.PointID, OK: false, Quality: q, Err: e}, e
			}
			// 设备已确认执行。在从站的突发上送到达之前，用请求值填充缓存，
			// 让紧接着的 read-back 确认读到的是「刚写下去的值」，而不是上
			// 一次采集的旧值。质量先标 uncertain：等设备自己上报后转 good。
			//
			// 只补「没有」或「比命令更旧」的条目：从站的突发上送常常紧跟
			// 在激活确认后面到达，甚至先于本协程被唤醒。无条件覆盖会把一条
			// 带真实品质位的上送换成本地推测值，而这个推测值在 104 里可能
			// 长期得不到纠正（只在变化时上送）。
			c.mu.Lock()
			if cur, ok := c.values[plan.ref.ID]; !ok || cur.at.Before(sentAt) {
				c.values[plan.ref.ID] = cachedValue{
					value: comm.value, q: model.QualityUncertain, at: time.Now()}
			}
			c.mu.Unlock()
			return driver.WriteResult{PointID: req.PointID, OK: true,
				Quality: model.QualityGood}, nil
		case <-c.done:
			e := driver.E(driver.ErrDisconnected, "write", c.deviceID, "connection closed", true, nil)
			return driver.WriteResult{PointID: req.PointID, OK: false,
				Quality: model.QualityCommError, Err: e}, e
		case <-c.dead:
			// 链路判死：不必等满 c.timeout。真实原因（超时探测失败/序号
			// 错位/对端关闭）在 linkErr 里，原样带出去，别改写成笼统的
			// 「写失败」。
			var inner error
			c.mu.Lock()
			inner = c.linkErr
			c.mu.Unlock()
			e := driver.E(driver.ErrDisconnected, "write", c.deviceID, "link is down", true, inner)
			return driver.WriteResult{PointID: req.PointID, OK: false,
				Quality: model.QualityCommError, Err: e}, e
		case <-timer.C:
			e := driver.E(driver.ErrTimeout, "write", c.deviceID,
				fmt.Sprintf("no activation confirmation for IOA %d within %s", plan.ioa, c.timeout), true, nil)
			return driver.WriteResult{PointID: req.PointID, OK: false,
				Quality: model.QualityTimeout, Err: e}, e
		case <-ctx.Done():
			e := driver.E(driver.ErrTimeout, "write", c.deviceID, "context done", true, ctx.Err())
			return driver.WriteResult{PointID: req.PointID, OK: false,
				Quality: model.QualityTimeout, Err: e}, e
		}
	}
}

type command struct {
	asdu  []byte
	value model.Value
}

func buildCommand(plan *pointPlan, req driver.WriteRequest, ca uint16) (command, error) {
	asdu, value, err := iec60870.BuildCommand(plan.area, plan.ioa, plan.dataType, req, ca)
	if err != nil {
		return command{}, err
	}
	return command{asdu: asdu, value: value}, nil
}

// numeric / doubleState 是共享层的一行转发，保留本地名字是因为单元测试与
// 命令构造的调用点都按「本协议的语义」读它们。
var (
	numeric     = iec60870.Numeric
	doubleState = iec60870.DoubleState
)

func negativeConfirm(deviceID string, ioa uint32, asdu iec104.ASDU) error {
	kind := driver.ErrPermission
	switch asdu.Cause {
	case iec104.CauseUnknownType, iec104.CauseUnknownCause:
		kind = driver.ErrProtocol
	case iec104.CauseUnknownCommonAddr, iec104.CauseUnknownIOA:
		kind = driver.ErrAddress
	}
	msg := fmt.Sprintf("command for IOA %d rejected: %s", ioa, iec104.CauseName(asdu.Cause))
	return driver.E(kind, "write", deviceID, msg, false, nil)
}

// ---------------------------------------------------------------------------
// 帧收发
// ---------------------------------------------------------------------------

func (c *conn) sendU(fn byte) error {
	frame, err := iec104.BuildUFrame(fn)
	if err != nil {
		return err
	}
	return c.writeFrame(frame)
}

func (c *conn) sendS() error {
	frame, err := iec104.BuildSFrame(uint16(c.recvSeq.Load()))
	if err != nil {
		return err
	}
	return c.writeFrame(frame)
}

// writeFrame 在 wmu 保护下写出一帧，并压一个写超时。
func (c *conn) writeFrame(frame []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.link.SetWriteDeadline(time.Now().Add(c.timeout))
	_, err := c.link.Write(frame)
	return err
}

// sendASDU 分配 N(S) 并写出一帧 I 帧。
func (c *conn) sendASDU(asdu []byte) error {
	deadline := time.Now().Add(c.timeout)
	for {
		c.wmu.Lock()
		if c.unacked < defaultK {
			break
		}
		c.wmu.Unlock()

		// k 窗口满：等对端确认。命令是一条一条发的，正常永远不会走到这
		// 里；等不到说明设备既不确认也不断开，必须报超时而不是把序号用光。
		if time.Now().After(deadline) {
			return driver.E(driver.ErrTimeout, "write", c.deviceID,
				fmt.Sprintf("send window full (%d unacknowledged I frames)", defaultK), true, nil)
		}
		select {
		case <-c.ackCh:
		case <-c.done:
			return driver.E(driver.ErrDisconnected, "write", c.deviceID, "connection closed", true, nil)
		case <-c.dead:
			c.mu.Lock()
			inner := c.linkErr
			c.mu.Unlock()
			return driver.E(driver.ErrDisconnected, "write", c.deviceID, "link is down", true, inner)
		case <-time.After(20 * time.Millisecond):
		}
	}

	frame, err := iec104.BuildIFrame(c.sendSeq, uint16(c.recvSeq.Load()), asdu)
	if err != nil {
		c.wmu.Unlock()
		return driver.E(driver.ErrInternal, "write", c.deviceID, err.Error(), false, err)
	}
	_ = c.link.SetWriteDeadline(time.Now().Add(c.timeout))
	if _, err := c.link.Write(frame); err != nil {
		c.wmu.Unlock()
		return driver.E(driver.ErrDisconnected, "write", c.deviceID, "write failed", true, err)
	}
	c.sendSeq = (c.sendSeq + 1) & 0x7FFF
	c.unacked++
	c.wmu.Unlock()
	// 发出 I 帧本身就捎带了 N(R)，收到的帧已经被这一帧确认过了。
	c.pendingAck.Store(0)
	return nil
}

// readLoop 持续读取并分发帧。
func (c *conn) readLoop() {
	head := make([]byte, 2)
	for {
		if _, err := io.ReadFull(c.link, head); err != nil {
			c.fail(err)
			return
		}
		if head[0] != iec104.StartByte {
			c.fail(fmt.Errorf("unexpected start byte 0x%02X", head[0]))
			return
		}
		n := int(head[1])
		if n < 4 || n > iec104.MaxAPDU {
			c.fail(fmt.Errorf("invalid APDU length %d", n))
			return
		}
		frame := make([]byte, 2+n)
		copy(frame, head)
		if _, err := io.ReadFull(c.link, frame[2:]); err != nil {
			c.fail(err)
			return
		}
		parsed, err := iec104.ParseFrame(frame)
		if err != nil {
			c.fail(err)
			return
		}
		c.lastRecv.Store(time.Now().UnixNano())
		if err := c.handleFrame(parsed); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *conn) handleFrame(f iec104.Frame) error {
	switch f.Kind {
	case iec104.KindI:
		// 序号必须严格连续。跳过一帧意味着我们已经漏掉了数据，继续接收
		// 只会让后面的值错位；宁可断链让上层重建。
		if uint16(c.recvSeq.Load()) != f.SendSeq {
			return fmt.Errorf("I frame out of order: want N(S)=%d, got %d", c.recvSeq.Load(), f.SendSeq)
		}
		c.recvSeq.Store(uint32((f.SendSeq + 1) & 0x7FFF))
		// 记录本批第一帧的到达时刻，供 t2 延迟确认使用。
		if c.pendingAck.Add(1) == 1 {
			c.ackSince.Store(time.Now().UnixNano())
		}
		// I 帧捎带的 N(R) 同样是对我们的确认。
		c.noteAck(f.RecvSeq)
		if len(f.ASDU) > 0 {
			c.dispatch(f.ASDU)
		}
	case iec104.KindS:
		if !withinAck(f.RecvSeq, c.sentSeq()) {
			return fmt.Errorf("S frame acknowledges N(S)=%d beyond our sent %d", f.RecvSeq, c.sentSeq())
		}
		c.noteAck(f.RecvSeq)
	case iec104.KindU:
		switch f.UFunc {
		case iec104.UStartCon:
			c.startOnce.Do(func() { close(c.started) })
		case iec104.UTestAct:
			if err := c.sendU(iec104.UTestCon); err != nil {
				return err
			}
			c.testOutstanding.Store(false)
		case iec104.UTestCon:
			c.testOutstanding.Store(false)
			c.mu.Lock()
			ch := c.testWait
			c.testWait = nil
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- nil:
				default:
				}
			}
		case iec104.UStopAct:
			_ = c.sendU(iec104.UStopCon)
			return errors.New("station requested STOPDT")
		case iec104.UStopCon:
			// 正常关闭流程的一部分。
		default:
			return fmt.Errorf("unknown U-frame function 0x%02X", f.UFunc)
		}
	}
	return nil
}

func (c *conn) sentSeq() uint16 {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.sendSeq
}

// noteAck 处理对端帧里携带的 N(R)：把发送窗口前移到 ack，并唤醒等待者。
//
// ack 是「对端下一个期望收到的 N(S)」，因此它之前的帧都已经确认。窗口里
// 未确认的帧数是 sendSeq-ack（模 32768）。不能像早期实现那样「收到任何帧
// 就清零」：S 帧可能只确认到更早的一帧（对端可以延迟确认），清零会让 k
// 失效，进而把 15 位序号一路写穿——现象是「偶发丢命令」，而日志里一切正常。
func (c *conn) noteAck(ack uint16) {
	c.wmu.Lock()
	// 只接受单调前进的确认：对端可以延迟确认，一个只确认到更早一帧的
	// 迟到 N(R) 不得把窗口宽度算大。
	if withinAck(ack, c.sendSeq) {
		acked := int(uint16((ack - c.ackPoint) & 0x7FFF))
		if acked > 0 && acked <= c.unacked {
			c.unacked -= acked
			c.ackPoint = ack
		}
	}
	c.wmu.Unlock()
	select {
	case c.ackCh <- struct{}{}:
	default:
	}
}

func withinAck(ack, sent uint16) bool {
	// 15 位序号下的「ack 没有越过 sent」判断（按模 32768 处理）。
	return uint16((sent-ack)&0x7FFF) < 0x4000
}

// dispatch 处理一条 ASDU。
//
// 解析失败或类型不认识都不拆链：104 的 APDU 有长度字段，丢弃整条 ASDU 不
// 会破坏后续帧。这类帧单独计数，与「链路静默」区分开。
func (c *conn) dispatch(raw []byte) {
	asdu, err := iec104.ParseASDU(raw)
	if err != nil {
		c.mu.Lock()
		c.badFrames++
		c.mu.Unlock()
		return
	}
	if asdu.CommonAddr != c.ca {
		// 同一链路上可能有多个站；只处理本设备配置的公共地址。
		return
	}
	if asdu.Test {
		// 带试验位的命令必须原样回送确认，不执行。
		if iec104.IsCommandType(asdu.TypeID) {
			asdu.Test = false
			asdu.Cause = iec104.CauseActivationCon
			if out, err := iec104.BuildASDU(asdu); err == nil {
				_ = c.sendASDU(out)
			}
		}
		return
	}

	switch asdu.TypeID {
	case iec104.TypeEndOfInit:
		// 从站重启了，它现在的值可能全变了；重新总召唤是唯一安全的做法。
		go func() { _ = c.interrogate() }()
		return
	}

	if iec104.IsCommandType(asdu.TypeID) {
		if len(asdu.Objects) == 0 {
			return
		}
		ioa := asdu.Objects[0].IOA
		c.mu.Lock()
		ch := c.pending[ioa]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- asdu:
			default:
			}
		}
		return
	}

	if !iec104.IsData(asdu.TypeID) {
		return
	}
	c.storeValues(asdu)
}

// storeValues 把一条数据 ASDU 写进缓存。
//
// 一个 IOA 可能挂多个点位（同一物理量被两个业务点引用），因此按 IOA 找到
// 全部点位逐个解码。设备上送哪种 ASDU 类型由它自己决定，驱动只做「能无损
// 转换就转」。
func (c *conn) storeValues(asdu iec104.ASDU) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range asdu.Objects {
		c.observeLocked(asdu, o, now)
		plans := c.byIOA[o.IOA]
		if len(plans) == 0 {
			continue
		}
		for _, p := range plans {
			v, q, ok := convert(o, asdu.TypeID, p.dataType)
			if !ok {
				// 类型对不上：保留旧值，下一次上送再试。不把缓存写成一个
				// 「类型错但看起来正常」的值。
				continue
			}
			c.values[p.ref.ID] = cachedValue{value: v, q: q, at: now}
		}
	}
}

// observeLocked 记录一条原始上送对象（调用方持锁）。
func (c *conn) observeLocked(asdu iec104.ASDU, o iec104.Object, now time.Time) {
	if c.observed == nil {
		c.observed = make(map[uint32]Observed, 32)
	}
	if _, known := c.observed[o.IOA]; !known && len(c.observed) >= maxObserved {
		c.dropped++
		return
	}
	var ids []string
	for _, p := range c.byIOA[o.IOA] {
		ids = append(ids, p.ref.ID)
	}
	sort.Strings(ids)
	c.observed[o.IOA] = Observed{
		IOA: o.IOA, TypeID: asdu.TypeID, Cause: asdu.Cause,
		Object: o, At: now, Points: ids,
	}
}

// Observed 返回「设备实际送过的信息对象」快照，按 IOA 升序。
//
// 这是诊断接口：它只读本地旁路表，不产生任何网络交互。主要消费者是自检
// 命令行与「点表发现」（第一次接入一台设备时，先看它送什么，再生成点表）。
func (c *conn) Observed() []Observed {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Observed, 0, len(c.observed))
	for _, o := range c.observed {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IOA < out[j].IOA })
	return out
}

// Dropped 返回因超出旁路表上限而丢弃的 IOA 数。
func (c *conn) Dropped() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// convert 把信息对象转换成点位声明的平台类型。
func convert(o iec104.Object, typeID byte, dt model.DataType) (model.Value, model.Quality, bool) {
	return iec60870.Convert(o, typeID, dt)
}

func qualityOf(q iec104.Quality) model.Quality { return iec60870.QualityOf(q) }

func worse(a, b model.Quality) model.Quality { return iec60870.Worse(a, b) }

// keeper 负责接收循环不能阻塞去做的三件事：确认收到的帧、空闲时发测试帧、
// 把「对端已经很久不说话」变成一条明确的链路错误。
func (c *conn) keeper() {
	// 轮询周期取 t2 的 1/4：t2 是延迟上限，晚一个周期确认仍然合法，但
	// 周期若接近 t2 就会让实际延迟在 t2..2*t2 之间抖动。上限留在 500ms
	// 是为了在 t2 很大时也不会让 TESTFR 的判死判断太迟。
	tick := c.effectiveT2() / 4
	if tick > 500*time.Millisecond {
		tick = 500 * time.Millisecond
	}
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		// 确认时机（§5.3）：待确认帧数达到 w，或最早一帧已等满 t2。
		// 只按 t2 判断会让一条突发上送密集的链路（一周期几十帧）把对端
		// 的发送窗口拖住——对端同样有 w 帧上限。
		if n := c.pendingAck.Load(); n > 0 {
			due := n >= defaultW
			if !due {
				since := time.Unix(0, c.ackSince.Load())
				due = time.Since(since) >= c.effectiveT2()
			}
			if due {
				if err := c.sendS(); err != nil {
					c.fail(err)
					return
				}
				c.pendingAck.Store(0)
			}
		}
		now := time.Now()
		last := time.Unix(0, c.lastRecv.Load())
		if c.testOutstanding.Load() {
			if now.Sub(time.Unix(0, c.testAt.Load())) > defaultT1 {
				c.fail(fmt.Errorf("no TESTFR con within %s", defaultT1))
				return
			}
			continue
		}
		if now.Sub(last) > c.testIdle {
			if err := c.sendU(iec104.UTestAct); err != nil {
				c.fail(err)
				return
			}
			c.testOutstanding.Store(true)
			c.testAt.Store(now.UnixNano())
		}
	}
}

// effectiveT2 返回实际使用的 t2 延迟。
func (c *conn) effectiveT2() time.Duration {
	if c.t2 > 0 {
		return c.t2
	}
	return defaultT2
}

// fail 记录链路错误、唤醒等待者并关闭连接。
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.linkErr == nil {
		c.linkErr = err
	}
	ch := c.testWait
	c.testWait = nil
	c.mu.Unlock()
	if c.dead != nil {
		c.deadOnce.Do(func() { close(c.dead) })
	}
	if ch != nil {
		e := driver.E(driver.ErrDisconnected, "probe", c.deviceID, "link is down", true, err)
		select {
		case ch <- e:
		default:
		}
	}
	_ = c.link.Close()
}
