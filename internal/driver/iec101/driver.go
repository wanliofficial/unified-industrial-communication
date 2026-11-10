// Package iec101 实现串行总线上的非平衡式远动通信主站驱动（FT1.2 帧）。
//
// 与「从站主动上送、主站只管听」的以太网承载不同，串行总线是**主站拥有
// 总线**：从站不能自己开口，只有被点名时才回答。因此这个驱动的数据通路
// 与 104 正好相反：
//
//	复位链路 → （首次）总召唤 → 每轮采集：请求第 1 级 → 请求第 2 级
//
// 三个必须记住的事实：
//
//  1. **Read 会真的走线**。它不是一个「查本地表」的动作，而是发固定帧
//     「请求第 1 级数据」，把从站压进缓冲的事件取回来。上层仍按拉取契约
//     使用，但代价是一条总线往返——这也是为什么 interval 不能配得太小。
//  2. **帧计数位（FCB）会翻转**。每一帧带用户数据的主站报文都有一个帧
//     计数位，从站在收到重复位时**不再执行**、只回确认。重传必须原样
//     重发同一帧（FCB 不变），否则一条命令会被执行两次。
//  3. **确认分两层**。链路层的确认只说明「报文到了」，应用层的激活确认
//     才说明「命令被执行」。写入必须等到后者；把链路确认当成写成功，
//     是这类驱动最常见的一处谎报。
//
// 能力边界：只做非平衡式主站、FT1.2 三种帧、总召唤、第 1/2 级轮询、
// 单点/双点/设定值命令与激活确认；不做平衡式（从站可主动发送）、不做
// 文件传输、不做时钟同步的写路径、不做冗余通道切换、不做带时标的命令。
// 承载既可以是真实串口，也可以是串口服务器的 TCP 透传——两者的分帧完全
// 相同，内置从站因此可以同时服务自检命令与自动化测试。
package iec101

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"uic/internal/codec/iec101"
	"uic/internal/codec/iec60870"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
	"uic/internal/transport/serialport"
)

// 地址 area 与点位语义层共用同一份定义（见 iec60870 包）。
const (
	AreaIOA                = iec60870.AreaIOA
	AreaSingleCommand      = iec60870.AreaSingleCommand
	AreaDoubleCommand      = iec60870.AreaDoubleCommand
	AreaSetpointFloat      = iec60870.AreaSetpointFloat
	AreaSetpointScaled     = iec60870.AreaSetpointScaled
	AreaSetpointNormalized = iec60870.AreaSetpointNormalized
)

// 默认参数。
//
// 取 1 字节链路地址、1 字节公共地址、2 字节信息对象地址、无源发地址：
// 这是现场最常见的一组工程参数（一台装置一个公共地址，站内点数不超过
// 65535）。参数不是标准强制的，全部可以用设备标签覆盖——把默认值当成
// 「标准值」去写死，是接不同厂家设备时最先撞上的墙。
const (
	defaultLinkAddr     uint16 = 1
	defaultLinkAddrSize int    = 1
	defaultCASize       int    = 1
	defaultIOASize      int    = 2
	defaultOrgSize      int    = 0
	// defaultInterrogate 是两次总召唤之间的间隔。
	//
	// 总召唤是一次整站快照，几百个点要在 9600 的总线上跑好几秒。取 5 分钟
	// 是在「掉电/重启后能较快恢复全量」与「不把总线占满」之间的折中；
	// 现场常见值是 15 分钟。事件（第 1 级）才是常态数据源，总召唤只负责
	// 兜底。
	defaultInterrogate = 5 * time.Minute

	// maxClassFrames 是一轮采集里最多连续请求多少条第 1/2 级数据。
	//
	// 设上限的理由不是协议限制，而是**公平性**：一台疯狂上送事件的设备
	// 不能把整个采集周期占满，让同一进程里的其它设备饿死。超出的部分留在
	// 从站缓冲里，下一轮继续取。
	maxClassFrames = 32

	// maxInterrogationFrames 是一次总召唤最多取多少条。
	maxInterrogationFrames = 512

	// maxExchangeFrames 是一次请求-应答里最多忽略多少个「不是给我的」帧。
	//
	// 一条总线上有多台从站时，别的站的应答会出现在线上（半双工下更常见：
	// 读到自己刚发出去的回波）。跳过它们是正常行为，但必须有上限，否则
	// 一条坏总线能让一次事务永远读下去。
	maxExchangeFrames = 32

	// maxObserved 是「设备实际送了什么」旁路表的上限，理由与 104 相同。
	maxObserved = 4096

	// emptyGap 是轮询到「暂时没有数据」之后的最小间隔。
	emptyGap = 30 * time.Millisecond
)

// Driver 是 IEC 101 主站驱动。
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
		ID:           "iec101",
		Name:         "IEC 60870-5-101 (serial master)",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read: driver.CapSupported,
			// 写入 = 下发命令并等待应用层激活确认。与 104 一样，我们确实
			// 等到设备的回执才返回成功。
			Write: driver.CapSupported,
			// 从站不能主动开口：没有订阅，只有轮询。声明 supported 会让
			// 上层以为「不读也有新值」。
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported, // 一次轮询覆盖本轮所有点位
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "串口/地址", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0 或 192.168.1.30:4001", Scope: "channel",
				Help: "kind=serial 时是串口设备路径；kind=tcp 时是串口服务器地址，省略端口按 4001"},
			{Name: "labels.linkAddress", Label: "链路地址", Type: "int", Default: 1, Scope: "device",
				Help: "FT1.2 链路地址；同一条总线上每台从站必须不同"},
			{Name: "labels.linkAddressSize", Label: "链路地址宽度", Type: "enum", Default: 1, Scope: "device",
				Enum: []string{"0", "1", "2"}, Help: "链路地址占几个字节，0 表示不带链路地址"},
			{Name: "labels.commonAddress", Label: "公共地址（CA）", Type: "int", Default: 1, Scope: "device",
				Help: "ASDU 的公共地址；留空时取 device.unitId"},
			{Name: "labels.commonAddressSize", Label: "公共地址宽度", Type: "enum", Default: 1, Scope: "device",
				Enum: []string{"1", "2"}, Help: "ASDU 公共地址占几个字节"},
			{Name: "labels.ioaSize", Label: "信息对象地址宽度", Type: "enum", Default: 2, Scope: "device",
				Enum: []string{"1", "2", "3"}, Help: "IOA 占几个字节；决定点表里的 IOA 上限"},
			{Name: "labels.orgSize", Label: "源发地址宽度", Type: "enum", Default: 0, Scope: "device",
				Enum: []string{"0", "1"}, Help: "ASDU 是否带 1 字节源发地址；多数现场为 0"},
			{Name: "labels.interrogate", Label: "总召唤周期", Type: "duration", Default: "5m", Scope: "device",
				Help: "两次总召唤之间的间隔；0 表示只在新连接后召唤一次"},
			{Name: "labels.class2", Label: "轮询第 2 级", Type: "bool", Default: true, Scope: "device",
				Help: "每轮采集是否顺带请求第 2 级数据（周期/背景数据）"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"register"}, Scope: "point"},
			{Name: "address.area", Label: "对象类别", Type: "enum", Required: true,
				Enum: iec60870.Areas(), Scope: "point",
				Help: "ioa 为监视方向；其余为命令类型，写入时按它下发"},
			{Name: "address.offset", Label: "信息对象地址（IOA）", Type: "int", Required: true,
				Scope: "point", Help: "上限由 labels.ioaSize 决定"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64",
					"float32", "float64"}, Scope: "point"},
		},
		Description: "IEC 60870-5-101 主站（FT1.2 非平衡轮询）：总召唤 + 事件轮询 + 单点/双点/设定值命令",
	}
}

// options 是从设备标签解析出来的工程参数。
type options struct {
	linkAddr     uint16
	linkAddrSize int
	ca           uint16
	profile      iec60870.Profile
	interrogate  time.Duration
	class2       bool
}

func parseOptions(dev model.Device) (options, error) {
	o := options{
		linkAddr:     defaultLinkAddr,
		linkAddrSize: defaultLinkAddrSize,
		ca:           uint16(dev.UnitID),
		profile: iec60870.Profile{
			OrgSize: defaultOrgSize, CASize: defaultCASize, IOASize: defaultIOASize,
		},
		interrogate: defaultInterrogate,
		class2:      true,
	}
	if o.ca == 0 {
		o.ca = 1
	}

	var err error
	if o.linkAddrSize, err = labelInt(dev, "linkAddressSize", o.linkAddrSize, 0, 2); err != nil {
		return o, err
	}
	var linkAddr int
	if linkAddr, err = labelInt(dev, "linkAddress", int(o.linkAddr), 0, 0xFFFF); err != nil {
		return o, err
	}
	o.linkAddr = uint16(linkAddr)
	if o.linkAddrSize == 0 && o.linkAddr != 0 {
		return o, fmt.Errorf("labels.linkAddress %d cannot be used with linkAddressSize=0", o.linkAddr)
	}
	if o.linkAddrSize == 1 && o.linkAddr > 0xFF {
		return o, fmt.Errorf("labels.linkAddress %d exceeds 1 byte", o.linkAddr)
	}
	if _, ok := dev.Labels["commonAddress"]; ok {
		ca, err := labelInt(dev, "commonAddress", int(o.ca), 1, 0xFFFF)
		if err != nil {
			return o, err
		}
		o.ca = uint16(ca)
	}
	if o.profile.CASize, err = labelInt(dev, "commonAddressSize", o.profile.CASize, 1, 2); err != nil {
		return o, err
	}
	if o.profile.IOASize, err = labelInt(dev, "ioaSize", o.profile.IOASize, 1, 3); err != nil {
		return o, err
	}
	if o.profile.OrgSize, err = labelInt(dev, "orgSize", o.profile.OrgSize, 0, 1); err != nil {
		return o, err
	}
	if err := o.profile.Validate(); err != nil {
		return o, err
	}
	// 公共地址宽度决定可表达的范围：2 字节宽度下配一个 300 的地址是合法
	// 的，1 字节宽度下就会在编码时静默截断——这里提前拒绝。
	if o.profile.CASize == 1 && o.ca > 0xFF {
		return o, fmt.Errorf("common address %d exceeds 1 byte (labels.commonAddressSize=1)", o.ca)
	}
	if v := strings.TrimSpace(dev.Labels["interrogate"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return o, fmt.Errorf("labels.interrogate %q: %w", v, err)
		}
		if d < 0 {
			return o, fmt.Errorf("labels.interrogate %q must not be negative", v)
		}
		o.interrogate = d
	}
	if v := strings.TrimSpace(dev.Labels["class2"]); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return o, fmt.Errorf("labels.class2 %q: %w", v, err)
		}
		o.class2 = b
	}
	return o, nil
}

func labelInt(dev model.Device, key string, def, min, max int) (int, error) {
	v := strings.TrimSpace(dev.Labels[key])
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("labels.%s %q: not an integer", key, v)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("labels.%s %d out of range %d..%d", key, n, min, max)
	}
	return n, nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("want true/false")
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

// pointPlan 是一个点位的驱动侧视图。
//
// 字段与 iec60870.Plan 一一对应，但保留本地类型：点位规划的共享逻辑在
// 共享包里，驱动只关心「这张表怎么用」。
type pointPlan struct {
	ref      driver.PointRef
	ioa      uint32
	area     string
	dataType model.DataType
}

func buildPlans(dev model.Device, maxIOA uint32) (map[string]*pointPlan, map[uint32][]*pointPlan, error) {
	plans := make(map[string]*pointPlan, len(dev.Points))
	byIOA := map[uint32][]*pointPlan{}
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		pl, err := iec60870.PlanPoint(driver.PointRefOf(dev, p), "iec101", maxIOA)
		if err != nil {
			return nil, nil, err
		}
		plan := &pointPlan{ref: pl.Ref, ioa: pl.IOA, area: pl.Area, dataType: pl.DataType}
		plans[plan.ref.ID] = plan
		byIOA[plan.ioa] = append(byIOA[plan.ioa], plan)
	}
	return plans, byIOA, nil
}

func planPoint(p driver.PointRef, maxIOA uint32) (*pointPlan, error) {
	pl, err := iec60870.PlanPoint(p, "iec101", maxIOA)
	if err != nil {
		return nil, err
	}
	return &pointPlan{ref: pl.Ref, ioa: pl.IOA, area: pl.Area, dataType: pl.DataType}, nil
}

// cachedValue 是一个 IOA 的最新值。
type cachedValue struct {
	value model.Value
	q     model.Quality
	at    time.Time
}

// Observed 是「设备在上送方向实际发过的一个信息对象」的原始记录。
//
// 与 104 的同名结构一致：它回答的是「这台设备到底送哪些 IOA、用哪种
// ASDU、带不带时标」，而不是「我配的点现在是什么值」。初次接入一台陌生
// 装置时，后者用 /api/v1/points 看，前者只能靠链路自己记录。
type Observed struct {
	IOA    uint32
	TypeID byte
	Cause  byte
	Object iec60870.Object
	At     time.Time
	Points []string
}

type conn struct {
	link     link.Link
	rd       *iec101.Reader
	deviceID string
	timeout  time.Duration

	opt     options
	maxIOA  uint32
	points  map[string]*pointPlan
	byIOA   map[uint32][]*pointPlan
	profile iec60870.Profile

	// mu 保护缓存、旁路表与关闭标志。它**不**保护链路：链路由 link.Lock
	// 串行化，锁序永远是「先总线锁、后 mu」，反向获取只会在一条总线上的
	// 两台设备互相等待时暴露成死锁。
	mu sync.Mutex
	// values 按**点位 ID** 而不是 IOA 缓存：同一个 IOA 可以同时是遥信
	// 与遥控（两套语义、两种平台类型），按 IOA 存会让两者互相覆盖。
	values   map[string]cachedValue
	observed map[uint32]Observed
	dropped  uint64
	badFrame uint64
	closed   bool

	// 以下字段仅在持有总线锁时读写。
	fcb       bool // 下一条用户数据帧的帧计数位
	linkOK    bool // 链路是否已复位过
	lastGI    time.Time
	giSeen    bool // 本轮总召唤是否收到终止报文
	framesOut uint64
	framesIn  uint64

	closeOnce sync.Once
}

// Open 建立链路并复位远方链路。
//
// 只做链路层复位（固定帧，一来一回），**不做总召唤**：一台几百个点的装置
// 在 9600 的总线上传完整站快照要好几秒，把它塞进 Open 会让「连接超时」
// 与「装置没数据」两件事混在一起。总召唤由第一次 Read 触发，失败也只影响
// 那一轮。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	opt, err := parseOptions(cfg.Device)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	plans, byIOA, err := buildPlans(cfg.Device, maxIOAOf(opt.profile))
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"device has no enabled points", false, nil)
	}

	l, err := link.Open(ctx, link.Options{
		Proto:       "iec101",
		DefaultPort: "4001",
		Opener:      d.serial,
	}, cfg.Channel.Transport, cfg.Channel.EffectiveTimeout(), cfg.Device.ID)
	if err != nil {
		return nil, err
	}

	c := &conn{
		link:     l,
		rd:       iec101.NewReader(l, opt.linkAddrSize),
		deviceID: cfg.Device.ID,
		timeout:  cfg.Channel.EffectiveTimeout(),
		opt:      opt,
		maxIOA:   maxIOAOf(opt.profile),
		profile:  opt.profile,
		points:   plans,
		byIOA:    byIOA,
		values:   make(map[string]cachedValue, len(plans)),
		observed: make(map[uint32]Observed, 32),
	}
	c.link.Lock()
	err = c.resetLink(ctx)
	c.link.Unlock()
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	// 链路状态请求是诊断信息，不是建链的必要条件：不少装置不实现它，
	// 回一个否定确认。这里只记调试日志，不让它挡住采集。
	c.link.Lock()
	if err := c.requestLinkStatus(ctx); err != nil {
		cfg.Log.Debug("链路状态请求未获响应", "device", cfg.Device.ID, "error", err)
	}
	c.link.Unlock()
	return c, nil
}

func maxIOAOf(p iec60870.Profile) uint32 {
	switch p.IOASize {
	case 1:
		return 0xFF
	case 2:
		return 0xFFFF
	}
	return 0xFFFFFF
}

func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
	})
	return c.link.Close()
}

// ---------------------------------------------------------------------------
// 帧收发
// ---------------------------------------------------------------------------

func (c *conn) fixedFrame(fc byte) ([]byte, error) {
	return iec101.BuildFixed(iec101.Control{PRM: true, Func: fc},
		c.opt.linkAddr, c.opt.linkAddrSize)
}

// userFrame 组一条带用户数据的帧，并翻转帧计数位。
//
// 翻转发生在**组帧之前**：重传必须复用同一帧字节（否则从站会把重传当成
// 新命令再执行一次），所以调用方拿到的是「一条已经定稿的帧」，重传时直接
// 重写同一份字节。
func (c *conn) userFrame(asdu []byte) ([]byte, error) {
	c.fcb = !c.fcb
	return iec101.BuildVariable(iec101.Control{
		PRM: true, FCB: c.fcb, FCV: true, Func: iec101.FCSendConfirm,
	}, c.opt.linkAddr, c.opt.linkAddrSize, asdu)
}

// setDeadline 把本次交换的截止时间压到链路上。
//
// 取「设备超时」与「调用方 ctx」中更早的那个：前者防止一条不回话的总线
// 把采集协程挂住，后者保证上层取消（停机、写请求超时）能立刻生效。
func (c *conn) setDeadline(ctx context.Context) {
	d := time.Now().Add(c.timeout)
	if t, ok := ctx.Deadline(); ok && t.Before(d) {
		d = t
	}
	_ = c.link.SetDeadline(d)
}

// exchange 发一帧，并读到一条「可接受」的应答。
//
// accept 的返回值语义：
//   - (true, nil)：这条应答就是要等的那一条，返回；
//   - (false, nil)：不是给我的（别的从站的应答、自己的回波），继续读；
//   - (_, err)：这条应答本身代表一个协议错误，直接返回。
func (c *conn) exchange(ctx context.Context, op string, frame []byte,
	accept func(iec101.Frame) (bool, error)) (iec101.Frame, error) {

	c.setDeadline(ctx)
	if err := c.link.Write(frame); err != nil {
		c.linkOK = false
		return iec101.Frame{}, driver.E(driver.ErrDisconnected, op, c.deviceID,
			"写链路失败", true, err)
	}
	c.framesOut++

	for i := 0; i < maxExchangeFrames; i++ {
		fr, err := c.rd.Next()
		if err != nil {
			c.linkOK = false
			return iec101.Frame{}, c.readError(op, err, ctx)
		}
		c.framesIn++
		// 单字节帧没有地址；其它帧的链路地址必须匹配，否则是总线上另一台
		// 从站的应答。把它当成本机的，会让两台设备的读数互相覆盖。
		if fr.Kind != iec101.KindSingle && fr.LinkAddr != c.opt.linkAddr {
			continue
		}
		done, err := accept(fr)
		if err != nil {
			return fr, err
		}
		if done {
			return fr, nil
		}
	}
	return iec101.Frame{}, driver.E(driver.ErrTimeout, op, c.deviceID,
		"链路上持续出现不属于本机的帧", true, nil)
}

func (c *conn) readError(op string, err error, ctx context.Context) error {
	c.rd.Reset()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.deviceID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.deviceID,
			fmt.Sprintf("等待应答超时（%s）", c.timeout), true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.deviceID, "读链路失败", true, err)
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

// busyError 把从站的否定确认翻成「链路忙，可重试」。
//
// 它与「超时」不同：否定确认说明从站听懂了但暂时处理不了（缓冲满、
// 正在初始化），值得立刻重试；超时说明话没说到，可能是接线/波特率。
func (c *conn) busyError(op string) error {
	return driver.E(driver.ErrProtocol, op, c.deviceID,
		"从站否定确认（链路忙）", true, nil)
}

// resetLink 复位远方链路（固定帧 FC=0）。
//
// 复位是每次建立连接后的第一步，也是链路不同步时的唯一恢复手段：它让
// 双方把帧计数位归零，从此帧序号对得上。没有它，重连后的第一条命令会
// 因为 FCB 与设备记忆不符而被当成重复帧丢弃。
func (c *conn) resetLink(ctx context.Context) error {
	frame, err := c.fixedFrame(iec101.FCResetLink)
	if err != nil {
		return driver.E(driver.ErrInternal, "open", c.deviceID, err.Error(), false, err)
	}
	_, err = c.exchange(ctx, "reset-link", frame, func(f iec101.Frame) (bool, error) {
		if f.Kind == iec101.KindSingle {
			if f.Byte == iec101.NackByte {
				return true, c.busyError("reset-link")
			}
			return true, nil
		}
		if f.Kind == iec101.KindFixed && f.Ctrl.Func == iec101.FCNack {
			return true, c.busyError("reset-link")
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	c.linkOK = true
	c.fcb = false // 复位后第一帧用户数据用 FCB=1（见 userFrame 的翻转）
	return nil
}

// requestLinkStatus 请求链路状态（固定帧 FC=9）。
func (c *conn) requestLinkStatus(ctx context.Context) error {
	frame, err := c.fixedFrame(iec101.FCRequestLinkStatus)
	if err != nil {
		return err
	}
	_, err = c.exchange(ctx, "link-status", frame, func(f iec101.Frame) (bool, error) {
		if f.Kind == iec101.KindSingle && f.Byte == iec101.NackByte {
			return true, c.busyError("link-status")
		}
		return true, nil
	})
	return err
}

// ensureLink 在链路未复位（或上一次事务失败）时补一次复位。
func (c *conn) ensureLink(ctx context.Context, op string) error {
	if c.linkOK {
		return nil
	}
	if err := c.resetLink(ctx); err != nil {
		return driver.E(driver.ErrDisconnected, op, c.deviceID, "链路复位失败", true, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 应用层收发
// ---------------------------------------------------------------------------

// sendUser 发一条带用户数据的 ASDU，并等链路层确认。
//
// 返回的 bool 是从站应答里的访问需求位（ACD）：true 表示「我这边还有第 1
// 级数据」。它只是提示，不能代替后续的取数请求——有的从站置了 ACD 却不
// 在确认帧里带数据，必须显式请求第 1 级。
func (c *conn) sendUser(ctx context.Context, asdu []byte, op string) (bool, error) {
	frame, err := c.userFrame(asdu)
	if err != nil {
		return false, driver.E(driver.ErrInternal, op, c.deviceID, err.Error(), false, err)
	}
	var acd bool
	accept := func(f iec101.Frame) (bool, error) {
		switch f.Kind {
		case iec101.KindSingle:
			if f.Byte == iec101.NackByte {
				return true, c.busyError(op)
			}
			return true, nil
		case iec101.KindVariable:
			// 少数装置不先回链路确认，直接送数据。把数据吃掉，但不要
			// 假装自己收到了确认——调用方仍会去取应用层回执。
			acd = f.Ctrl.ACD
			c.consume(f.Payload)
			return true, nil
		}
		if f.Ctrl.Func == iec101.FCNack {
			return true, c.busyError(op)
		}
		acd = f.Ctrl.ACD
		return true, nil
	}
	_, err = c.exchange(ctx, op, frame, accept)
	if err != nil && driver.KindOf(err) == driver.ErrTimeout {
		// 链路确认丢了就原样重发：帧计数位不变，从站会把这一帧识别成
		// 重复帧——只回确认，不重复执行。这正是 FT1.2 用 FCB 而不是用
		// 事务号的全部意义。重发仍用同一份字节，绝不能重新组帧。
		acd = false
		_, err = c.exchange(ctx, op, frame, accept)
	}
	if err != nil {
		return false, err
	}
	return acd, nil
}

// classResult 是一次取数请求的结论。
type classResult struct {
	// more 表示从站还想让我们继续取（访问需求位）。
	more bool
	// frames 是本次应答里处理过的 ASDU 条数。
	frames int
	// confirm 是本轮遇到的最后一条控制方向回执。
	confirm *iec60870.ASDU
}

// classOnce 请求一次指定级别的数据（固定帧 FC=10 / FC=11）。
func (c *conn) classOnce(ctx context.Context, fc byte) (classResult, error) {
	op := "poll-class1"
	if fc == iec101.FCRequestClass2 {
		op = "poll-class2"
	}
	frame, err := c.fixedFrame(fc)
	if err != nil {
		return classResult{}, driver.E(driver.ErrInternal, op, c.deviceID, err.Error(), false, err)
	}
	var res classResult
	_, err = c.exchange(ctx, op, frame, func(f iec101.Frame) (bool, error) {
		switch f.Kind {
		case iec101.KindSingle:
			if f.Byte == iec101.NackByte {
				return true, c.busyError(op)
			}
			return true, nil // E5：没有数据
		case iec101.KindVariable:
			res.more = f.Ctrl.ACD
			conf := c.consume(f.Payload)
			if conf != nil {
				res.confirm = conf
			}
			res.frames++
			return true, nil
		}
		// 固定帧应答：确认（FC=0）/ 无数据（FC=9）/ 链路状态（FC=11）。
		if f.Ctrl.Func == iec101.FCNack {
			return true, c.busyError(op)
		}
		res.more = f.Ctrl.ACD
		return true, nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// pollClass 连续取某个级别的数据，直到从站表示「没有了」或达到上限。
//
// 后续轮次一律请求第 1 级：第 1 级事件优先于第 2 级周期数据，这是协议里
// 定的优先级，反过来会让事件被周期数据挤在后面。
func (c *conn) pollClass(ctx context.Context, fc byte, max int) (int, error) {
	total := 0
	for i := 0; i < max; i++ {
		res, err := c.classOnce(ctx, fc)
		if err != nil {
			return total, err
		}
		total += res.frames
		if !res.more {
			return total, nil
		}
		fc = iec101.FCRequestClass1
	}
	return total, nil
}

// consume 解析并处理一条 ASDU，返回其中的控制方向回执（如果有）。
func (c *conn) consume(payload []byte) *iec60870.ASDU {
	a, err := iec60870.ParseASDUProfile(payload, c.profile)
	if err != nil {
		c.mu.Lock()
		c.badFrame++
		c.mu.Unlock()
		return nil
	}
	if a.CommonAddr != c.opt.ca {
		// 公共地址不匹配：同一条总线上可能有多个站共用链路地址（不该
		// 出现，但现场有），按「不是给我的」丢弃，并计入坏帧以便诊断。
		c.mu.Lock()
		c.badFrame++
		c.mu.Unlock()
		return nil
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	// 总召唤的「结束」只认两种：类型 100 的激活终止，或类型 70 的初始化
	// 结束。**不能**把类型 100 的激活确认当成结束——不少装置把确认放在
	// 数据之前，那样会在第一条数据到达时就收工。
	if (a.TypeID == iec60870.TypeInterrogation || a.TypeID == iec60870.TypeCounterInterrogation) &&
		a.Cause == iec60870.CauseActivationTerm {
		c.giSeen = true
	}
	if a.TypeID == iec60870.TypeEndOfInit {
		c.giSeen = true
	}

	if iec60870.IsCommandType(a.TypeID) {
		return &a
	}
	if !iec60870.IsData(a.TypeID) {
		return nil
	}
	for _, o := range a.Objects {
		c.observeLocked(a, o, now)
		plans := c.byIOA[o.IOA]
		if len(plans) == 0 {
			continue
		}
		for _, p := range plans {
			v, q, ok := iec60870.Convert(o, a.TypeID, p.dataType)
			if !ok {
				// 类型对不上：保留旧值，等下一次上送。把类型错的值写进
				// 缓存，比没有值更危险——它看起来是正常的。
				continue
			}
			c.values[p.ref.ID] = cachedValue{value: v, q: q, at: now}
		}
	}
	return nil
}

func (c *conn) observeLocked(a iec60870.ASDU, o iec60870.Object, now time.Time) {
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
		IOA: o.IOA, TypeID: a.TypeID, Cause: a.Cause,
		Object: o, At: now, Points: ids,
	}
}

// ---------------------------------------------------------------------------
// 总召唤
// ---------------------------------------------------------------------------

// needInterrogation 判断本轮是否需要做一次总召唤。
func (c *conn) needInterrogation(now time.Time) bool {
	if c.lastGI.IsZero() {
		return true
	}
	if c.opt.interrogate <= 0 {
		return false
	}
	return now.Sub(c.lastGI) >= c.opt.interrogate
}

// interrogate 发起一次站总召唤并取回数据。
//
// 流程是「一条命令 + 若干次取数」：
//
//	主站 → 总召唤（带用户数据，需要链路确认）
//	从站 → 确认（ACD=1 表示有第 1 级数据）
//	主站 → 请求第 1 级 × N，直到访问需求位清零或收到终止报文
//
// 终止报文有三种常见形态（类型 100 的激活终止、类型 100 的激活确认、
// 类型 70 的初始化结束），现场三种都见得到。把它认死成一种，会在换一台
// 装置时表现为「总召唤永远不结束」。
func (c *conn) interrogate(ctx context.Context) error {
	asdu, err := iec60870.BuildInterrogationProfile(c.opt.ca, iec60870.QOIStation, c.profile)
	if err != nil {
		return driver.E(driver.ErrInternal, "interrogate", c.deviceID, err.Error(), false, err)
	}
	// 先记账再发送：即使装置不回话，也不该在每一个采集周期里重发总召唤
	// ——那会把总线彻底占满，连事件都取不回来。
	c.lastGI = time.Now()
	c.giSeen = false

	if _, err := c.sendUser(ctx, asdu, "interrogate"); err != nil {
		return err
	}
	for i := 0; i < maxInterrogationFrames; i++ {
		res, err := c.classOnce(ctx, iec101.FCRequestClass1)
		if err != nil {
			return err
		}
		if c.giSeen {
			break
		}
		if !res.more && res.frames == 0 {
			// 没有更多数据，也没有终止报文：装置要么不支持总召唤，要么
			// 把数据放在了周期上送里。不算失败——本轮已经尽量取了。
			break
		}
	}

	// 累计量（电能量）不在站总召唤范围内，必须单独召唤一次。只发站召唤
	// 的实现现场表现为「电度点位永远没有值」——它其实只是走另一条路径。
	//
	// 无条件发送：主站无法从点表推断设备有没有累计量（这由设备用哪种
	// ASDU 上送决定，不是点表属性）。没有累计量的设备会回否定确认或直接
	// 不给数据，协议上完全正常。
	return c.counterInterrogate(ctx)
}

// counterInterrogate 发起一次电能量召唤并取回数据。
func (c *conn) counterInterrogate(ctx context.Context) error {
	asdu, err := iec60870.BuildASDUProfile(iec60870.ASDU{
		TypeID: iec60870.TypeCounterInterrogation, Cause: iec60870.CauseActivation,
		CommonAddr: c.opt.ca,
		Objects:    []iec60870.Object{{Qualifier: 5}}, // 总请求
	}, c.profile)
	if err != nil {
		return driver.E(driver.ErrInternal, "interrogate", c.deviceID, err.Error(), false, err)
	}
	if _, err := c.sendUser(ctx, asdu, "counter-interrogate"); err != nil {
		return err
	}
	c.giSeen = false
	for i := 0; i < maxInterrogationFrames; i++ {
		res, err := c.classOnce(ctx, iec101.FCRequestClass1)
		if err != nil {
			return err
		}
		if c.giSeen {
			break
		}
		if !res.more && res.frames == 0 {
			break
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// Read 先轮询总线，再从缓存组装样本。
//
// 轮询失败时不抛掉已经拿到的值：一条总线上偶发一次超时，不该让上一轮
// 采到的、还在有效期内的值全部变成「通信错误」。错误仍会返回，由采集层
// 决定如何降级。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	pollErr := c.poll(ctx)

	c.mu.Lock()
	closed := c.closed
	out := make([]driver.RawSample, 0, len(spans))
	for _, span := range spans {
		for _, p := range span.Points {
			out = append(out, c.lookupLocked(p.ID))
		}
	}
	c.mu.Unlock()

	if closed {
		return out, driver.E(driver.ErrDisconnected, "read", c.deviceID, "connection closed", true, nil)
	}
	return out, pollErr
}

// poll 做一轮完整采集：按需总召唤，然后取第 1 级、第 2 级数据。
func (c *conn) poll(ctx context.Context) error {
	c.link.Lock()
	defer c.link.Unlock()

	// 半双工纪律：发之前把上一轮迟到的字节排空。101 的从站只在被点名时
	// 发言，残留字节通常来自超时后晚到的应答；不排空就会把它当成本轮的。
	_ = c.link.Drain()
	c.rd.Reset()

	if err := c.ensureLink(ctx, "read"); err != nil {
		return err
	}
	if c.needInterrogation(time.Now()) {
		if err := c.interrogate(ctx); err != nil {
			return err
		}
	}
	if _, err := c.pollClass(ctx, iec101.FCRequestClass1, maxClassFrames); err != nil {
		return err
	}
	if c.opt.class2 {
		if _, err := c.pollClass(ctx, iec101.FCRequestClass2, maxClassFrames); err != nil {
			return err
		}
	}
	return nil
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
		msg := fmt.Sprintf("no data received for IOA %d yet (not in the station's interrogation set?)", plan.ioa)
		if !c.monitoredLocked(plan.ioa) {
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

// Write 逐条下发命令并等待应用层激活确认。
//
// 串行总线没有事务号，两条命令交错下发会让回执无法配对；这里的串行化
// 由总线锁保证，`Write` 与 `Read` 共用同一把锁。
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
	plan, err := planPoint(driver.PointRef{ID: req.PointID, Address: req.Address, DataType: req.DataType}, c.maxIOA)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityAddressError, Err: err}, err
	}
	asdu, value, err := iec60870.BuildCommandProfile(plan.area, plan.ioa, plan.dataType, req, c.opt.ca, c.profile)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityBad, Err: err}, err
	}

	c.link.Lock()
	defer c.link.Unlock()

	_ = c.link.Drain()
	c.rd.Reset()
	if err := c.ensureLink(ctx, "write"); err != nil {
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityCommError, Err: err}, err
	}

	sentAt := time.Now()
	if _, err := c.sendUser(ctx, asdu, "write"); err != nil {
		e := driver.E(driver.KindOf(err), "write", c.deviceID,
			"下发命令失败", true, err)
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityCommError, Err: e}, e
	}

	conf, err := c.waitCommand(ctx, plan.ioa)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, OK: false,
			Quality: model.QualityTimeout, Err: err}, err
	}
	if conf.Negative ||
		(conf.Cause != iec60870.CauseActivationCon && conf.Cause != iec60870.CauseActivationTerm) {
		e := negativeConfirm(c.deviceID, plan.ioa, *conf)
		q := model.QualityBad
		if driver.KindOf(e) == driver.ErrAddress {
			q = model.QualityAddressError
		}
		return driver.WriteResult{PointID: req.PointID, OK: false, Quality: q, Err: e}, e
	}

	// 设备已确认执行。在从站的自发上送到达之前，用请求值填充缓存，让
	// 紧接着的 read-back 读到「刚写下去的值」。只补「没有」或「比命令
	// 更旧」的条目：无条件覆盖会抹掉一条更新的上送，而事件型上送可能
	// 很久都不再来一条。
	c.mu.Lock()
	if cur, ok := c.values[plan.ref.ID]; !ok || cur.at.Before(sentAt) {
		c.values[plan.ref.ID] = cachedValue{value: value, q: model.QualityUncertain, at: time.Now()}
	}
	c.mu.Unlock()

	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// waitCommand 取数直到拿到指定 IOA 的激活确认/终止。
//
// 回执走第 1 级数据，但现场确有装置把它放在第 2 级；两级都试。两次都
// 没有数据时短暂让出总线，避免在一条安静的总线上打出成串的空请求。
func (c *conn) waitCommand(ctx context.Context, ioa uint32) (*iec60870.ASDU, error) {
	deadline := time.Now().Add(c.timeout)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	for time.Now().Before(deadline) {
		res, err := c.classOnce(ctx, iec101.FCRequestClass1)
		if err != nil {
			return nil, driver.E(driver.KindOf(err), "write", c.deviceID,
				"等待命令确认时链路出错", true, err)
		}
		if match := matchConfirm(res.confirm, ioa); match != nil {
			return match, nil
		}
		if !res.more {
			res, err = c.classOnce(ctx, iec101.FCRequestClass2)
			if err != nil {
				return nil, driver.E(driver.KindOf(err), "write", c.deviceID,
					"等待命令确认时链路出错", true, err)
			}
			if match := matchConfirm(res.confirm, ioa); match != nil {
				return match, nil
			}
			if res.frames == 0 {
				select {
				case <-ctx.Done():
					return nil, driver.E(driver.ErrTimeout, "write", c.deviceID, "context done", true, ctx.Err())
				case <-time.After(emptyGap):
				}
			}
		}
	}
	return nil, driver.E(driver.ErrTimeout, "write", c.deviceID,
		fmt.Sprintf("no activation confirmation for IOA %d within %s", ioa, c.timeout), true, nil)
}

func matchConfirm(a *iec60870.ASDU, ioa uint32) *iec60870.ASDU {
	if a == nil || len(a.Objects) == 0 {
		return nil
	}
	for _, o := range a.Objects {
		if o.IOA == ioa {
			return a
		}
	}
	return nil
}

func negativeConfirm(deviceID string, ioa uint32, a iec60870.ASDU) error {
	kind := driver.ErrPermission
	switch a.Cause {
	case iec60870.CauseUnknownType, iec60870.CauseUnknownCause:
		kind = driver.ErrProtocol
	case iec60870.CauseUnknownCommonAddr, iec60870.CauseUnknownIOA:
		kind = driver.ErrAddress
	}
	msg := fmt.Sprintf("command for IOA %d rejected: %s", ioa, iec60870.CauseName(a.Cause))
	return driver.E(kind, "write", deviceID, msg, false, nil)
}

// ---------------------------------------------------------------------------
// 探活与诊断
// ---------------------------------------------------------------------------

// Probe 用链路状态请求证明对端还在。
//
// 只查「连接标志」在串口上没有意义：一根被拔掉的 485 线不会让本地状态
// 变成断开。链路状态请求是协议内唯一一个「强制对端现在回答」的动作。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "connection closed", true, nil)
	}

	c.link.Lock()
	defer c.link.Unlock()
	_ = c.link.Drain()
	c.rd.Reset()
	if err := c.requestLinkStatus(ctx); err != nil {
		return err
	}
	c.linkOK = true
	return nil
}

// Observed 返回「设备实际送过的信息对象」快照，按 IOA 升序。
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

// BadFrames 返回「帧校验通过但 ASDU 解析失败或公共地址不符」的条数。
func (c *conn) BadFrames() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.badFrame
}

// Frames 返回累计收发的帧数，供自检命令展示链路活跃度。
func (c *conn) Frames() (out, in uint64) {
	c.link.Lock()
	defer c.link.Unlock()
	return c.framesOut, c.framesIn
}
