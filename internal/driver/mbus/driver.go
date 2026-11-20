// Package mbus 实现 M-Bus（EN 13757-2/3）主站驱动。
//
// 分层与仓库里其它表计驱动一致：Meta（能力声明）→ conn（事务/错误分类）→
// codec/mbus（纯函数编解码）→ transport（串口或串口服务器字节流，经
// serialport 共享同一根 RS-485 总线）。
//
// # 定位：单主站、主地址轮询
//
// M-Bus 的总线仲裁有两条常见实现：主站轮询主地址（1..250 的站号），
// 或用次级地址（8 字节全球唯一 ID）选中某台表。本驱动只做前者，理由：
//
//   - 主地址寻址是现场最常见、也最容易排查的一种：站号写错时"完全没应答"
//     与"应答了但不是这台表"是两个能立刻分辨的现象；
//   - 次级地址要求先发 SND_UD(CI=52h) 选中从站，之后**整帧的地址字段变成
//     8 字节且校验范围随之变化**。半成品实现比不支持更难排查，因此 codec
//     显式拒绝次级地址（见 codec/mbus 包注释）；
//   - 一条 RS-485 上挂 250 台表时，轮询由采集调度器按设备切分，驱动不需要
//     自己维护"总线扫描表"。
//
// # 一次事务读整张表
//
// REQ_UD2 让从站把**当前全部遥测记录**打包上送（一条 RSP_UD）。这与 Modbus
// 的"按区间读"根本不同：点表里的 `q:energy` 只是从这条报文里挑一条记录，
// 因此一轮采集无论挂多少个点位，总线上都只有一次请求。这是 MultiRead
// 声明为 supported 的原因——不是"驱动帮你合并了请求"，而是协议本身就以
// 整表为单位。
//
// 由此带来一个必须讲清楚的边界：**点位是新旧值混用风险的源头**。一条
// RSP_UD 是一次快照，所以同一条报文里的点位天然一致；但报文是周期性的，
// 两个采集周期之间表可能已经变了。这与"每点一次读"的驱动在语义上不同，
// 点表与文档里都必须说明。
//
// # FCB/FCV
//
// 主站请求的 C 字段里，FCV(bit4)=1 表示"帧计数有效"，FCB(bit5) 是翻转的
// 帧计数。从站据此区分"新请求"与"重传"：重传时 FCB 不变，从站会重发
// 上一次的应答（访问号不会再加一）。驱动因此必须在**同一轮重试里保持
// FCB 不变，跨轮才翻转**——这一点写错的表现是"偶尔能读到上一次的值"，
// 极难发现。
package mbus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	mbuscodec "uic/internal/codec/mbus"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
	"uic/internal/transport/serialport"
)

// defaultTCPPort 是串口服务器/DTU 透传的常见端口，仅在配置省略端口时使用。
const defaultTCPPort = "4001"

// 默认参数。
const (
	// defaultGap 是两次事务之间的最小静默。
	//
	// M-Bus 标准没有规定主站之间的最小间隔（它假定单一主站），但现场大量
	// 用 RS-485 转换器，从发送切回接收需要时间；实测 20ms 能覆盖绝大多数
	// USB 转 485 的批量传输延迟。取 30ms 与仓库里其它表计驱动一致。
	defaultGap = 30 * time.Millisecond
	// defaultRetries 是同一请求的重发次数（不含首次）。
	//
	// M-Bus 的应答可能长达 250 字节，在 2400bps 上要传近 1 秒；偶发丢字节
	// 在长总线上是常态。重发 2 次能让单次采集的成功率显著上升，又不至于让
	// 一台坏表把一轮采集拖到几十秒。
	defaultRetries = 2
	// defaultMaxFollow 是一次读表允许跟随的后续报文数。
	//
	// 从站用 DIF=1Fh 表示"还有后续"，主站需要再发 REQ_UD1 取下一段。
	// 32 段 × 约 240 字节已经远超任何真实表的数据量；不设上限的话，
	// 一个"永远说要更多"的从站会把驱动挂死在一次事务里。
	defaultMaxFollow = 32
)

// Options 是设备级配置开关（来自 labels.options）。
type Options struct {
	// Retries 是同一请求的重发次数（不含首次）。
	Retries int
	// Gap 覆盖帧间静默。
	Gap time.Duration
	// SendNKE 决定每次采集前是否先发 SND_NKE 复位从站。
	//
	// 默认只在实际读取失败之后发一次：SND_NKE 会让从站把 FCB 状态清零，
	// 每轮都发等于每轮都重新握手，在慢速总线上白白多花一次往返。但有些
	// 从站（尤其是长期运行的电池表）会忘记 FCB，表现为"每隔几轮读一次"，
	// 这时把它打开。
	SendNKE bool
	// MaxFollow 覆盖后续报文上限。
	MaxFollow int
}

// Driver 是 M-Bus 主站驱动。
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
		ID:           "mbus",
		Name:         "M-Bus 表计（EN 13757）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster, driver.RoleSlave},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:      driver.CapSupported,
			Write:     driver.CapUnsupported,
			Subscribe: driver.CapUnsupported,
			Browse:    driver.CapUnsupported,
			Discovery: driver.CapUnsupported,
			// 一条 RSP_UD 覆盖整表，所以无论挂多少点位都只发一次请求。
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.port", Label: "串口", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0", Scope: "channel"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: 2400, Scope: "channel",
				Help: "标准典型值 300/2400/9600；M-Bus 电平转换器通常自适应 2400，必须以现场实测为准"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: "even", Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "EN 13757-2 规定偶校验；个别转换器/从站出厂为无校验"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "半双工总线必须串行；同一物理串口由多个通道共享时按总线锁排队"},
			{Name: "labels.options", Label: "通信开关", Type: "string", Default: "", Scope: "device",
				Help: "retries=2,gap=30ms,send-nke,max-follow=32"},
		},
		PointFields: []driver.ConfigField{
			{Name: "labels.address", Label: "主地址", Type: "string", Required: true, Scope: "device",
				Placeholder: "1",
				Help:        "从站的主地址 0..250（1..250 常用；0 通常留给服务地址）。也可以用 device.unitId"},
			{Name: "address.path", Label: "记录地址", Type: "string", Required: true, Scope: "point",
				Placeholder: "q:energy",
				Help:        "q:<物理量>[ @存储号][/第几条] 或 r:<记录序号>；例如 q:energy、q:volume@1、q:power-max"},
		},
		Description: "M-Bus 主站（EN 13757-2/3）：按主地址轮询从站，一次 REQ_UD2 取回整表遥测记录，点位按物理量名或记录序号寻址（RS-485 / 串口服务器）",
	}
}

// Open 建链。
//
// 承载与其它表计驱动一致：serial（真实 RS-485）或 tcp（串口服务器/DTU
// 透传）。两者分帧、校验与命令序列完全相同，因此内置 TCP 从站可以直接
// 当作自检目标。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	addr, err := addressOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	opt := parseOptions(cfg.Device)

	l, err := link.Open(ctx, link.Options{
		Proto:       "mbus",
		DefaultPort: defaultTCPPort,
		Opener:      d.serial,
	}, cfg.Channel.Transport, cfg.Channel.EffectiveTimeout(), cfg.Device.ID)
	if err != nil {
		return nil, err
	}
	return &conn{
		link:    l,
		addr:    addr,
		opt:     opt,
		devID:   cfg.Device.ID,
		timeout: cfg.Device.Timeout.Duration(),
	}, nil
}

// addressOf 取从站主地址。
//
// 优先 labels.address（字符串，能把 0 与"未配置"分开），退化到
// device.unitId（int，0 表示未配置）。为什么两种都收：unitId 是仓库里
// 所有驱动共用的字段，MBus 的主力地址恰好也是 0..250 的小整数，硬要求
// 再写一遍地址会让很多现成的点表不能用；而 labels.address 能显式表达
// "站号就是 0"。
func addressOf(dev model.Device) (byte, error) {
	if s := strings.TrimSpace(dev.Labels["address"]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("device.labels.address %q 不是整数（主地址 0..%d）", s, mbuscodec.MaxPrimaryAddr),
				false, err)
		}
		if err := mbuscodec.ValidatePrimaryAddress(byte(n)); err != nil {
			return 0, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		return byte(n), nil
	}
	if dev.UnitID > 0 {
		if err := mbuscodec.ValidatePrimaryAddress(byte(dev.UnitID)); err != nil {
			return 0, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		return byte(dev.UnitID), nil
	}
	return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
		"device.labels.address 或 device.unitId required（M-Bus 主地址 0..250）", false, nil)
}

// parseOptions 解析 labels.options。
//
// 用逗号分隔的字符串而不是给 model.Device 加一串只有本协议使用的 bool
// 字段：那些字段会出现在所有设备的 JSON 里，永远为 false。
func parseOptions(dev model.Device) Options {
	opt := Options{Retries: defaultRetries, Gap: defaultGap, MaxFollow: defaultMaxFollow}
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
		case "retries":
			if hasVal {
				if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= 10 {
					opt.Retries = n
				}
			}
		case "gap":
			if hasVal {
				if d, err := time.ParseDuration(val); err == nil && d >= 0 && d <= time.Second {
					opt.Gap = d
				}
			}
		case "send-nke":
			opt.SendNKE = !hasVal || truthy(val)
		case "max-follow":
			if hasVal {
				if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 256 {
					opt.MaxFollow = n
				}
			}
		}
	}
	if opt.Gap < 0 {
		opt.Gap = 0
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

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	link    link.Link
	addr    byte
	opt     Options
	devID   string
	timeout time.Duration

	// mu 串行化本连接上的事务。
	//
	// 总线锁（Ref.Lock）保证同一根线上不同设备不互相抢，但它不阻止同一台
	// 设备的两个事务并发；而 M-Bus 没有事务号，两个交错的请求会让应答
	// 无法配对。
	mu     sync.Mutex
	closed bool
	// fcb 是当前帧计数位。跨轮翻转、同一轮重传保持（见包注释）。
	fcb bool
	// inited 表示本连接已经至少成功完成过一次完整交互。
	inited  bool
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

// Probe 只验证链路：发 SND_NKE，等一个 0xE5。
//
// 为什么用 SND_NKE 而不是直接 REQ_UD2：SND_NKE 的应答是**单字节**，
// 不依赖可变数据结构的任何细节。这样"完全没字节回来"（物理层/波特率/
// 地址错）与"有字节但解析不了"（协议/参数错）就能分开——两种故障的
// 处置方式完全不同。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", false, nil)
	}
	if err := c.sendNKE(ctx, "probe"); err != nil {
		return err
	}
	c.inited = true
	c.linkErr = nil
	return nil
}

// sendNKE 发 SND_NKE 并等单字符 ACK。
func (c *conn) sendNKE(ctx context.Context, op string) error {
	req := mbuscodec.BuildShort(mbuscodec.CtrlSNDNKE, c.addr)
	_, err := c.transact(ctx, op, req, func(fr mbuscodec.Frame) error {
		if fr.Type != mbuscodec.FrameAck {
			return fmt.Errorf("%w: SND_NKE 的应答应是单字符 E5，收到 %s 帧（C=%02X）",
				mbuscodec.ErrFrame, fr.Type, fr.C)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 标准要求 SND_NKE 之后从站的两个 FCB 状态都是 0；下一帧用 FCB=1 开始，
	// 这样从站能明确把它与"复位前的上一帧"区分开。
	c.fcb = true
	return nil
}

// Read 读本轮全部点位。
//
// 一次事务读整张表：因为同一条 RSP_UD 里所有记录来自同一次快照，
// 点位之间天然一致。逐点发 REQ_UD2 不仅慢，还会让同一设备上的点位
// 落在不同的快照上——那是一种"看起来对、其实前后矛盾"的错误。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", false, nil)
	}
	refs := flattenRefs(spans)
	if len(refs) == 0 {
		return nil, nil
	}
	if c.linkErr != nil {
		return allFailed(refs, c.linkErr), c.linkErr
	}

	resp, err := c.readTable(ctx, "read")
	if err != nil {
		return allFailed(refs, err), err
	}

	out := make([]driver.RawSample, 0, len(refs))
	for _, ref := range refs {
		s, err := decodeRef(resp, ref)
		if err != nil {
			out = append(out, driver.RawSample{
				PointID: ref.ID,
				Quality: model.QualityAddressError,
				Err: driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("point %q: %s", ref.ID, err), false, err),
			})
			continue
		}
		out = append(out, s)
	}
	// 单点找不到记录不升级为链路错误：其它点位仍然读到了，链路是通的。
	return out, nil
}

// readTable 发 REQ_UD2 并解析应答，必要时跟随后续报文。
func (c *conn) readTable(ctx context.Context, op string) (mbuscodec.Response, error) {
	resp, err := c.readOnce(ctx, op)
	if err != nil {
		// 失败后复位从站的 FCB 状态再试一次。
		//
		// 这一步是 M-Bus 与 Modbus 最大的运维差别：很多表在断电/复位/上位机
		// 换人之后会"忘记"自己期待哪个 FCB，表现为问答完全对不上（发了请求
		// 没反应，或收到上一次的应答）。SND_NKE 是唯一能把它拉回已知状态
		// 的帧。
		if nerr := c.sendNKE(ctx, op); nerr == nil {
			resp, err = c.readOnce(ctx, op)
		}
		if err != nil {
			return mbuscodec.Response{}, err
		}
	}
	// CI=70h（通用错误）/ 71h（告警状态）应答没有记录：错误码说明的是从站
	// 的整体状态（CI 不支持、缓冲溢出…），不是哪个点配错了。在这里上抛，
	// 让整批点位拿到协议错——把它降级成逐点 address_error，用户会拿着一堆
	// "报文里没有记录"去查点表，而真正的问题是表在报错。codec 已把错误码
	// 翻译进 ErrorText；「有头无记录」的应答（76h/空表）不算错误。
	if resp.Kind == mbuscodec.DataNone {
		return resp, driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("从站报错误应答：%s", resp.ErrorText), false, nil)
	}
	c.inited = true
	c.linkErr = nil

	// 后续报文：从站用 DIF=1Fh 表示"还有"。
	for i := 1; resp.MoreRecordsFollow; i++ {
		if i > c.opt.MaxFollow {
			return resp, driver.E(driver.ErrProtocol, op, c.devID,
				fmt.Sprintf("后续报文超过 %d 段仍未结束", c.opt.MaxFollow), true, nil)
		}
		more, err := c.readOnce(ctx, op)
		if err != nil {
			return resp, err
		}
		resp.Records = append(resp.Records, reindex(more.Records, len(resp.Records))...)
		resp.MoreRecordsFollow = more.MoreRecordsFollow
	}
	return resp, nil
}

// readOnce 发一次 REQ_UD2 并解析一帧应答。
func (c *conn) readOnce(ctx context.Context, op string) (mbuscodec.Response, error) {
	req := mbuscodec.BuildShort(mbuscodec.ReqUD2Control(c.fcb), c.addr)
	var out mbuscodec.Response
	_, err := c.transact(ctx, op, req, func(fr mbuscodec.Frame) error {
		resp, perr := mbuscodec.ParseResponse(fr)
		if perr != nil {
			return perr
		}
		out = resp
		return nil
	})
	if err != nil {
		return mbuscodec.Response{}, err
	}
	// 成功一次就翻转 FCB：下一次是新请求。
	c.fcb = !c.fcb
	return out, nil
}

// transact 是唯一的事务入口：排空 → 设截止时间 → 发送 → 收帧 → 校验。
//
// 重发次数覆盖"完全没应答"与"帧坏了"两类失败。校验和错误也重发，因为
// 它在长总线上通常是瞬态干扰，而且从站已经收到了请求（访问号会加一），
// 重发同一 FCB 让从站回放上一次的应答，不会让表的存储值被重读。
func (c *conn) transact(ctx context.Context, op string, req []byte,
	check func(mbuscodec.Frame) error) (mbuscodec.Frame, error) {
	c.link.Lock()
	defer c.link.Unlock()

	var lastErr error
	for attempt := 0; attempt <= c.opt.Retries; attempt++ {
		if attempt > 0 && c.opt.Gap > 0 {
			select {
			case <-ctx.Done():
				return mbuscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
			case <-time.After(c.opt.Gap):
			}
		}
		fr, err := c.exchange(ctx, req, op, check)
		if err == nil {
			return fr, nil
		}
		lastErr = err
		// 断链与参数错误重发没有意义。
		if driver.KindOf(err) == driver.ErrDisconnected || driver.KindOf(err) == driver.ErrUnsupported {
			return mbuscodec.Frame{}, err
		}
		// 尊重 exchange 已经分好的「可重发」标记。M-Bus 的应答校验和错是
		// 瞬态干扰（可重发），而 C 字段/CI 不对、PRM 位错位、次级地址被拒
		// 这类 `ErrFrame` 表示**从站在正常应答、只是我们理解错了**——
		// 重发同一请求只会得到同样的应答，重试纯属浪费一次往返。spec
		// §3.10.25.6 对此有明文约定，exchange 已在 OpError 上标好
		// temporary=false；这里若只看错误种类（ErrProtocol）就会把两类失败
		// 一起重试，与「校验和错才重发」的语义相悖。
		if !driver.IsRetryable(err) {
			return mbuscodec.Frame{}, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return mbuscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
		}
	}
	if isLinkFailure(lastErr) {
		c.linkErr = lastErr
	}
	return mbuscodec.Frame{}, lastErr
}

func (c *conn) exchange(ctx context.Context, req []byte, op string,
	check func(mbuscodec.Frame) error) (mbuscodec.Frame, error) {
	if c.opt.Gap > 0 {
		select {
		case <-ctx.Done():
			return mbuscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		case <-time.After(c.opt.Gap):
		}
	}
	// 半双工纪律：发之前清空接收缓冲，避免把上一轮迟到的响应当成本轮。
	//
	// 顺序很关键：Drain 会把读截止时间压到「现在」，若先设好本次事务的
	// 截止时间再 Drain，那个截止时间就被压掉了，接下来的 Write 会立刻
	// 超时——症状是「连上了，但每次写都 timeout」。
	_ = c.link.Drain()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.link.SetDeadline(dl)
	} else {
		_ = c.link.SetDeadline(time.Now().Add(c.deadline()))
	}
	if err := c.link.Write(req); err != nil {
		return mbuscodec.Frame{}, driver.E(driver.ErrDisconnected, op, c.devID, "串口写入失败", true, err)
	}

	rd := mbuscodec.NewReader(c.link)
	fr, err := rd.Next()
	if err != nil {
		return mbuscodec.Frame{}, c.readError(op, err, ctx, rd.BadFrames())
	}
	if err := check(fr); err != nil {
		// 帧到达了但内容不对：区分"链路坏"与"从站回了别的东西"。
		//
		// 校验和错是瞬态干扰（可重试）；C 字段/CI 不对说明从站在正常应答，
		// 只是我们理解错了，重试没用，直接报协议错误。
		if errors.Is(err, mbuscodec.ErrChecksum) {
			return mbuscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.devID,
				"应答校验和错误（总线干扰或终端电阻缺失）", true, err)
		}
		return mbuscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.devID, err.Error(), false, err)
	}
	return fr, nil
}

func (c *conn) deadline() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	// M-Bus 的应答可能接近 250 字节，2400bps 下约 1 秒；留 3 秒覆盖
	// 转换器延迟与拆包。
	return 3 * time.Second
}

func (c *conn) readError(op string, err error, ctx context.Context, badFrames int) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
	}
	// 关键区分：线路上**有字节回来，但一帧都没成**。
	//
	// Reader 会跳过坏帧继续扫描，所以它最终报出来的总是"超时"；但超时的
	// 原因可能截然不同。若已经跳过了坏帧，说明从站确实在应答，只是我们
	// 没解对——最常见的是波特率/校验位配错（4800 8E1 配成 2400 8N1 时
	// 收到的全是被采错的位）、终端电阻缺失导致的边沿畸变，或地址是广播
	// 而总线上多台表同时应答互相叠加。把这归成 ErrProtocol 而不是
	// ErrTimeout，现场才会去查参数而不是去查线。
	if badFrames > 0 {
		return driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("收到 %d 个字节流但无一成帧：检查波特率/校验位（标准 2400 8E1）与终端电阻", badFrames),
			true, err)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.devID,
			fmt.Sprintf("等待从站 %d 应答超时", c.addr), true, err)
	}
	if errors.Is(err, mbuscodec.ErrChecksum) {
		return driver.E(driver.ErrProtocol, op, c.devID, "应答校验和错误", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.devID, "串口读取失败", true, err)
}

// Table 读整表并返回解码后的应答（含全部记录与固定头）。
//
// 它只服务自检命令：驱动的正常采集走 Read，点位地址已经声明了要哪条记录；
// 而"这台表到底有什么"必须能看到全部记录，否则现场只能靠猜地址。把这条
// 通路做成公开方法而不是让 CLI 自己组帧，是为了让自检与采集共用同一套
// 事务/重试/FCB 逻辑——自检若自己发帧，就证明不了采集路径是通的。
func (c *conn) Table(ctx context.Context) (mbuscodec.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return mbuscodec.Response{}, driver.E(driver.ErrDisconnected, "table", c.devID, "connection closed", false, nil)
	}
	return c.readTable(ctx, "table")
}

// Write 不支持。
//
// M-Bus 的写方向是 SND_UD（CI=51h 发数据 / 5Eh 改地址 / 50h 校时）。本驱动
// 不实现，理由有两条，都是刻意的：
//
//  1. 写什么、写成什么格式，完全由表厂的应用层剖面决定（一次写整条记录
//     需要主站构造 DIF/VIF 与数据域，写错会改变表的计费参数）；
//  2. 写地址/改速率这类命令会让表从当前站号"消失"，需要现场人工介入。
//
// 声明不支持，让上层把它当成配置错误，比"支持但只对某些表有效"更诚实。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, driver.WriteResult{
			PointID: req.PointID,
			Quality: model.QualityBad,
			Err: driver.E(driver.ErrUnsupported, "write", c.devID,
				"M-Bus 驱动不支持写入（SND_UD 需按表厂剖面构造记录，且改地址/速率会让表失联）", false, nil),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 点位解码
// ---------------------------------------------------------------------------

// decodeRef 把一条点位地址映射到应答里的一条记录。
func decodeRef(resp mbuscodec.Response, ref driver.PointRef) (driver.RawSample, error) {
	expr, err := recordExpr(ref.Address)
	if err != nil {
		return driver.RawSample{}, err
	}
	rec, ok, err := mbuscodec.FindRecord(resp.Records, expr)
	if !ok {
		if err == nil {
			return driver.RawSample{}, fmt.Errorf("报文里没有记录 %q", expr)
		}
		return driver.RawSample{}, err
	}
	// 从站状态字报应用错误时，值仍然上送了，但不能当成可信值。
	if resp.Header.StatusError() {
		return driver.RawSample{
			PointID: ref.ID,
			Value:   recordValue(ref, rec),
			Quality: model.QualityUncertain,
			Err: driver.E(driver.ErrProtocol, "read", "", "从站状态字报应用错误："+
				resp.Header.StatusText(), false, nil),
		}, nil
	}
	return driver.RawSample{PointID: ref.ID, Value: recordValue(ref, rec), Quality: model.QualityGood}, nil
}

// recordValue 把记录转成 model.Value。
//
// 数值记录按 float64 交给上层：记录本身就是"整数 × 10 的幂"，用 float64
// 能保住工程值（1234.567 kWh 这种小数位是表算出来的，不是我们编的）。
// 上层要整数类型时由点表的 dataType 做一次显式转换，而不是在驱动里悄悄
// 截断。文本/字节记录按原样交给上层。
func recordValue(ref driver.PointRef, rec mbuscodec.Record) model.Value {
	switch rec.Kind {
	case mbuscodec.KindNumeric:
		return model.NumValue(model.TypeFloat64, rec.Num)
	case mbuscodec.KindText:
		return model.TextValue(model.TypeString, rec.Text)
	case mbuscodec.KindBinary:
		return model.BytesValue(rec.Raw)
	}
	return model.NumValue(model.TypeFloat64, 0)
}

// recordExpr 从点位地址里取出记录表达式（`q:...` / `r:...` 的冒号之后部分）。
//
// 地址在点表里写作 `kind=symbol, path="q:energy"`。为什么保留 `q:` 前缀
// 而不是直接写 `energy`：
//
//   - 前缀把"按物理量取名"与"按记录序号取"两种寻址分开，点表一眼能看出
//     这条点位依赖的是标准量名还是原始位置；
//   - 它也让地址里出现 `@`、`/`、`#` 时不会被误当成别的语法。
func recordExpr(a model.Address) (string, error) {
	if a.Kind != model.AddressSymbol {
		return "", fmt.Errorf("M-Bus 点位必须用符号地址（kind=symbol），得到 %q", a.Kind)
	}
	path := strings.TrimSpace(a.Path)
	if path == "" {
		return "", errors.New("记录地址为空")
	}
	switch {
	case strings.HasPrefix(path, "q:"):
		return strings.TrimSpace(path[2:]), nil
	case strings.HasPrefix(path, "r:"):
		return path, nil
	}
	// 没有前缀时按物理量名处理：`energy` 与 `q:energy` 等价。
	// 这不算宽容，而是因为点表模板里漏写前缀太常见，而 `energy` 也不
	// 可能是别的语法。
	return path, nil
}

// flattenRefs 摊平 spans 并按 PointID 去重。
//
// 去重是必要的：一个点位可能同时出现在多个 span 里（不同采集周期叠加），
// 重复返回会让上层把同一个值写两次，序号白白前进。
func flattenRefs(spans []driver.ReadSpan) []driver.PointRef {
	n := 0
	for _, s := range spans {
		n += len(s.Points)
	}
	out := make([]driver.PointRef, 0, n)
	seen := make(map[string]bool, n)
	for _, s := range spans {
		for _, p := range s.Points {
			if p.ID == "" || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out
}

// allFailed 给每条点位造一个坏样本。
func allFailed(refs []driver.PointRef, err error) []driver.RawSample {
	out := make([]driver.RawSample, 0, len(refs))
	q := qualityOf(err)
	for _, ref := range refs {
		out = append(out, driver.RawSample{PointID: ref.ID, Quality: q, Err: err})
	}
	return out
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrUnsupported, driver.ErrPermission:
		return model.QualityBad
	case driver.ErrProtocol:
		return model.QualityBad
	}
	return model.QualityCommError
}

// reindex 在拼接多段报文时重编记录序号。
//
// `r:<n>` 是对外承诺的"报文里第 n 条记录"；如果不重编，第二段的第 0 条
// 会与第一段的第 0 条撞号，`r:` 寻址就会指向错的那条。
func reindex(recs []mbuscodec.Record, base int) []mbuscodec.Record {
	for i := range recs {
		recs[i].Index = base + i
	}
	return recs
}

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
