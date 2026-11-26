// Package cjt188 实现户用计量仪表（水/气/热表）主站驱动。
//
// 分层与同仓库的其它表计驱动一致：
//
//	codec/cjt188（纯函数编解码）→ transport（字节流，经 serialport）
//	    → driver/link（串口/串口服务器统一建链与总线锁）→ driver/cjt188
//
// 与 DL/T 645 的差异集中在三处，代码里都按「不能共用」处理：
//
//   - 一个 DI 是一组字段，不是单一数值：按 (表类型, DI, 字段偏移) 取 schema，
//     同一次 Read 里同 DI 的多个字段复用一次事务；
//   - 单位是数据域里的显式字节，解码后统一换算到 m³/m³/h/GJ/kW；
//   - 写命令只有阀门（A017）与写地址（A018），其余 DI 一律本地拒绝。
package cjt188

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"uic/internal/codec/cjt188"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
	"uic/internal/transport/serialport"
)

const (
	// defaultTCPPort 是串口服务器/DTU 透传的常见端口，仅在配置省略端口时使用。
	defaultTCPPort = "4001"
	// defaultGap 是帧间静默。表计多为电池供电，收到帧后要唤醒再应答；
	// 30ms 足以让上一轮的总线从发送切回接收。
	defaultGap = 30 * time.Millisecond
	// defaultType 是未配置表类型时的默认值（冷水水表）。
	defaultType = 0x10
)

// Options 是写命令的安全开关，来自设备标签。
type Options struct {
	// AllowWrite 允许写地址（A018）。
	AllowWrite bool
	// AllowControl 允许阀门控制（A017）。
	AllowControl bool
	// Preamble 覆盖前导 FE 的个数（0 = 不发，标准建议 2..4）。
	Preamble int
	// Gap 覆盖帧间静默。
	Gap time.Duration
}

// Driver 是 CJ/T 188 主站驱动。
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
		ID:           "cjt188",
		Name:         "户用计量仪表（188）",
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
				Help: "标准典型值 2400；现场也有 1200/4800/9600，必须与表一致"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: "even", Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "标准要求偶校验；个别厂商出厂为无校验"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "半双工总线必须串行；同一物理串口由多个通道共享时按总线锁排队"},
			{Name: "labels.type", Label: "仪表类型", Type: "string", Default: "10", Scope: "device",
				Help: "2 位十六进制：10 冷水/11 热水/12 直饮/13 中水/20 热量/21 冷量/30 燃气/40 其他"},
			{Name: "labels.address", Label: "表地址", Type: "string", Required: true, Scope: "device",
				Placeholder: "00000805000001",
				Help:        "14 位十进制 BCD（含 4 位厂商码），不支持缩位；低字节在线上先传"},
			{Name: "labels.options", Label: "写命令开关", Type: "string", Default: "", Scope: "device",
				Help: "allow-write（写地址）、allow-control（阀门）、preamble=0..4、gap=50ms"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "数据标识 DI", Type: "string", Required: true, Scope: "point",
				Placeholder: "901F",
				Help:        "4 位十六进制，手册顺序；例如 901F=当前计量数据、D120=上 1 月结算日量"},
			{Name: "address.offset", Label: "字段偏移", Type: "int", Required: true, Scope: "point",
				Help: "DI 数据域内的字节偏移：901F 水表 0=当前累积流量、5=结算日累积流量、10=实时时间、17=状态；热量表 0/5/10/15/20/25/28/31/34/41"},
		},
		Description: "户用计量仪表通信协议主站：按数据标识读取水/气/热量表的累积量与状态，写阀门与表地址（RS-485 / 串口服务器）",
	}
}

// Open 建链。serial 与 tcp 两种承载的分帧、校验与命令序列完全相同。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	addr, err := addressOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	typ, err := typeOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	opt := parseOptions(cfg.Device)

	l, err := link.Open(ctx, link.Options{
		Proto:       "cjt188",
		DefaultPort: defaultTCPPort,
		Opener:      d.serial,
	}, cfg.Channel.Transport, cfg.Channel.EffectiveTimeout(), cfg.Device.ID)
	if err != nil {
		return nil, err
	}
	return &conn{
		cfg:       cfg,
		link:      l,
		addr:      addr,
		meterType: typ,
		opt:       opt,
		devID:     cfg.Device.ID,
		timeout:   cfg.Channel.EffectiveTimeout(),
	}, nil
}

// addressOf 从设备标签里取 14 位表地址。
//
// 与 DL/T 645 不同，这里不支持缩位：645 的 AA 通配在总线上常用于「点对点
// 问一台表」，而 CJ/T 188 的 AA 只属于读地址命令。把缩位地址当普通地址
// 发出去会变成广播，多表总线上所有表都会应答。
func addressOf(dev model.Device) (cjt188.Addr, error) {
	var out cjt188.Addr
	s := strings.TrimSpace(dev.Labels["address"])
	if s == "" {
		return out, driver.E(driver.ErrUnsupported, "open", dev.ID,
			"device.labels.address required (14 位十进制表地址)", false, nil)
	}
	a, err := cjt188.ParseAddr(s)
	if err != nil {
		return out, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
	}
	if a.HasWildcard() {
		return out, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("表地址 %s 含 AA 通配/广播字节；读数据必须使用完整的 14 位地址", a), false, nil)
	}
	return a, nil
}

// typeOf 从设备标签里取仪表类型 T。
func typeOf(dev model.Device) (byte, error) {
	s := strings.TrimSpace(dev.Labels["type"])
	if s == "" {
		return defaultType, nil
	}
	s = strings.TrimPrefix(strings.ToUpper(s), "0X")
	n, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("labels.type %q 不是 2 位十六进制（如 10/20/30）", s), false, err)
	}
	t := byte(n)
	if t == 0xAA || t < 0x10 || t > 0x4F {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("labels.type %02X 不是标准仪表类型（10..4F）", t), false, nil)
	}
	return t, nil
}

// parseOptions 读取设备标签 options 里的写命令开关。
func parseOptions(dev model.Device) Options {
	opt := Options{Gap: defaultGap, Preamble: cjt188.PreambleLen}
	if dev.Labels == nil {
		return opt
	}
	for _, part := range strings.Split(dev.Labels["options"], ",") {
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
				if n, err := parseInt(val); err == nil && n >= 0 && n <= 4 {
					opt.Preamble = n
				}
			}
		case "gap":
			if hasVal {
				if d, err := time.ParseDuration(val); err == nil && d > 0 && d <= time.Second {
					opt.Gap = d
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

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	cfg       driver.OpenConfig
	link      link.Link
	addr      cjt188.Addr
	meterType byte
	opt       Options
	devID     string
	timeout   time.Duration

	// mu 串行化本连接上的事务。总线锁保证「同一根线上不同设备不互相抢」，
	// 但不阻止同一设备的并发调用；CJ/T 188 没有事务号，交错的请求会让
	// 应答无法配对。
	mu      sync.Mutex
	closed  bool
	ser     byte // 报文序列号，每次请求递增
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

// nextSER 返回下一个序列号。SER 是 1 字节，回绕到 0 继续用。
func (c *conn) nextSER() byte {
	c.ser++
	return c.ser
}

// Probe 读地址（0x03）验证链路。
//
// T 与地址域都是 AA；从站用自己的真实地址应答。它验证了三件事：波特率/
// 校验位对、帧分隔符对、以及总线上真的有一台表。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", false, nil)
	}
	req, err := cjt188.ReadAddrRequest(c.nextSER())
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
	typ, got, err := cjt188.ParseAddrReply(fr)
	if err != nil {
		// 异常应答说明表收到了读地址命令但拒绝执行：重发结果相同，不能标成
		// 可重试（否则探测会白等多轮超时）。帧结构错则可能只是线路噪声，保留
		// 可重试——两者的区别正好由 Exception 类型承载。
		var ex *cjt188.Exception
		temporary := !errors.As(err, &ex)
		return driver.E(driver.ErrProtocol, "probe", c.devID, err.Error(), temporary, err)
	}
	if got != c.addr {
		return driver.E(driver.ErrProtocol, "probe", c.devID,
			fmt.Sprintf("应答地址 %s 与配置 %s 不一致（总线上可能有另一台表）", got, c.addr), false, nil)
	}
	// 类型不一致不一定是故障（不少表把热水表统一报成 10H），只记录事实，
	// 不让 Probe 失败。这里保留变量是为了让上面的调用形态显式。
	_ = typ
	c.linkErr = nil
	return nil
}

// Read 逐点读。
//
// 一次 Read 内按 DI 缓存应答：901F 上同时配了「当前累积流量、结算日流量、
// 时间、状态」四个点时只发一帧。跨 DI 不合并——协议没有多 DI 批量读，
// 硬合并只能靠发多帧再拼，收益为零。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", false, nil)
	}

	// 先把 span 摊平：后面要在任意下标处「把剩下的点全部标坏」，嵌套循环
	// 做不到这件事而又不写两套收尾逻辑。
	var points []driver.PointRef
	for _, span := range spans {
		points = append(points, span.Points...)
	}

	cache := make(map[cjt188.DI][]byte)
	out := make([]driver.RawSample, 0, len(points))
	var firstErr error
	ok := 0

	// failRest 把 from 之后的点位显式标坏。
	//
	// 链路已经断了还逐点发请求，代价是「点数 × 设备超时」；而少给样本
	// 又会被采集器当成「驱动漏报」——两种处理都会把一次断线放大成一轮
	// 静默故障。显式补坏样本是唯一既不撒谎又不拖延的收尾方式。
	failRest := func(from int, err error) {
		for _, ref := range points[from:] {
			out = append(out, bad(ref, err))
		}
	}

	for i, ref := range points {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			failRest(i, e)
			return out, e
		}
		// linkErr 是上一次事务留下的链路级故障（exchange 成功时会清空）。
		// 它同样属于「这一轮别再发了」，按链路级例外立即收尾。
		if c.linkErr != nil {
			failRest(i, c.linkErr)
			return out, c.linkErr
		}
		s, err := c.readPoint(ctx, ref, cache)
		if err != nil {
			out = append(out, bad(ref, err))
			if firstErr == nil {
				firstErr = err
			}
			if driver.IsLinkFailure(err) {
				// 链路级失败是「有成功点也返回错误」的例外（docs §3.3.1）：
				// 设备太慢/已断开必须让调度器看见，不能混在部分失败里。
				c.linkErr = err
				failRest(i+1, err)
				return out, err
			}
			continue
		}
		ok++
		out = append(out, s)
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误。
	return out, driver.PartialReadError(ok, firstErr)
}

func (c *conn) readPoint(ctx context.Context, ref driver.PointRef, cache map[cjt188.DI][]byte) (driver.RawSample, error) {
	di, field, err := c.resolveRead(ref)
	if err != nil {
		return driver.RawSample{}, err
	}
	payload, hit := cache[di]
	if !hit {
		payload, err = c.readDI(ctx, di)
		if err != nil {
			return driver.RawSample{}, err
		}
		cache[di] = payload
	}
	if field.End() > len(payload) {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("%s（%s:%d）需要数据域至少 %d 字节，实际 %d",
				field.Name, di, field.Offset, field.End(), len(payload)), false, nil)
	}
	v, err := cjt188.DecodeField(field, payload[field.Offset:field.End()])
	if err != nil {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "read", c.devID, err.Error(), false, err)
	}
	dt := model.TypeFloat64
	switch field.Kind {
	case cjt188.FieldStatus:
		dt = model.TypeUint16
	case cjt188.FieldTime:
		dt = model.TypeFloat64
	default:
		dt = numericType(ref.DataType)
	}
	return driver.RawSample{
		PointID: ref.ID,
		Value:   model.NumValue(dt, v),
		Quality: model.QualityGood,
	}, nil
}

// readDI 发一次读数据请求，返回 DI 的完整数据域（不含 DI 与 SER）。
func (c *conn) readDI(ctx context.Context, di cjt188.DI) ([]byte, error) {
	ser := c.nextSER()
	req, err := cjt188.ReadRequest(c.meterType, c.addr, di, ser)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "read", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, "read")
	if err != nil {
		return nil, err
	}
	fr, err := resp.Frame()
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, "read", c.devID, err.Error(), true, err)
	}
	if err := c.checkAddr(fr, "read"); err != nil {
		return nil, err
	}
	reply, err := cjt188.ParseReadReply(fr, di, ser)
	if err != nil {
		return nil, c.classify(di, "read", err)
	}
	want, werr := cjt188.TotalWidth(c.meterType, di)
	if werr != nil {
		return nil, driver.E(driver.ErrAddress, "read", c.devID, werr.Error(), false, werr)
	}
	if len(reply.Payload) != want {
		return nil, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("%s 的数据域应为 %d 字节，实际 %d 字节", di, want, len(reply.Payload)), false, nil)
	}
	return reply.Payload, nil
}

// resolveRead 把点表地址解析成 DI 与字段定义。
func (c *conn) resolveRead(ref driver.PointRef) (cjt188.DI, cjt188.Field, error) {
	di, err := c.parseDI("read", ref.Address.Area)
	if err != nil {
		return cjt188.DI{}, cjt188.Field{}, err
	}
	field, err := cjt188.LookupField(c.meterType, di, ref.Address.Offset)
	if err != nil {
		return cjt188.DI{}, cjt188.Field{}, driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
	}
	// 点表显式写长度时，必须与 schema 一致。宽度由 DI 定义决定，写错
	// 一位就会把相邻字段当成这个字段的一部分。
	if ref.Address.Length > 0 && ref.Address.Length != field.Width() {
		return cjt188.DI{}, cjt188.Field{}, driver.E(driver.ErrAddress, "read", c.devID,
			fmt.Sprintf("%s（%s:%d）长度为 %d 字节，但 DI 定义为 %d 字节",
				field.Name, di, field.Offset, ref.Address.Length, field.Width()), false, nil)
	}
	return di, field, nil
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
	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			// ctx 到期后剩下的每一条都注定失败。逐条重试只会把「超时」
			// 乘以条数；但也不能少给结果——逐项返回是写路径的硬契约。
			e := driver.E(driver.ErrTimeout, "write", c.devID, "context done", true, err)
			for _, rest := range reqs[i:] {
				out = append(out, driver.WriteResult{
					PointID: rest.PointID, Quality: qualityOf(e), Err: e,
				})
			}
			return out, e
		}
		res := c.writeOne(ctx, req)
		out = append(out, res)
		if driver.IsLinkFailure(res.Err) {
			// 链路级失败：剩余写请求不可能成功，标坏后把批级错误经第二个
			// 返回值上抛（docs §3.4 规则 4）。已成功的条目保持原样。
			for _, rest := range reqs[i+1:] {
				out = append(out, driver.WriteResult{
					PointID: rest.PointID, Quality: qualityOf(res.Err), Err: res.Err,
				})
			}
			return out, res.Err
		}
	}
	return out, nil
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) driver.WriteResult {
	res := driver.WriteResult{PointID: req.PointID}
	di, err := c.parseDI("write", req.Address.Area)
	if err != nil {
		res.Err = err
		res.Quality = qualityOf(err)
		return res
	}
	switch di {
	case cjt188.DIValve:
		res.Err = c.writeValve(ctx, req)
	case cjt188.DIWriteAddr:
		res.Err = c.writeAddr(ctx, req)
	default:
		res.Err = driver.E(driver.ErrUnsupported, "write", c.devID,
			fmt.Sprintf("CJ/T 188 只支持写阀门（A017）与写地址（A018），不支持写 %s", di), false, nil)
	}
	if res.Err != nil {
		// 失败项必须带质量码：留空会让上层兜底成 bad，于是「超时」「地址
		// 写错」「设备拒绝」在下游看起来一模一样（docs §3.4 规则 5）。
		res.Quality = qualityOf(res.Err)
		return res
	}
	res.OK = true
	res.Quality = model.QualityGood
	return res
}

func (c *conn) writeValve(ctx context.Context, req driver.WriteRequest) error {
	if !c.opt.AllowControl {
		return driver.E(driver.ErrPermission, "write", c.devID,
			"未开启阀门控制权限（设备 options 需包含 allow-control）", false, nil)
	}
	open, err := valveValue(req.Value)
	if err != nil {
		return err
	}
	ser := c.nextSER()
	frame, err := cjt188.ValveRequest(c.meterType, c.addr, ser, open)
	if err != nil {
		return driver.E(driver.ErrInternal, "write", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, frame, "write-valve")
	if err != nil {
		return err
	}
	fr, err := resp.Frame()
	if err != nil {
		return driver.E(driver.ErrProtocol, "write", c.devID, err.Error(), true, err)
	}
	if err := c.checkAddr(fr, "write-valve"); err != nil {
		return err
	}
	return c.checkWriteAck(fr, cjt188.CtrlWrite|cjt188.BitReply, cjt188.DIValve, ser, "阀门控制")
}

func (c *conn) writeAddr(ctx context.Context, req driver.WriteRequest) error {
	if !c.opt.AllowWrite {
		return driver.E(driver.ErrPermission, "write", c.devID,
			"未开启写地址权限（设备 options 需包含 allow-write）", false, nil)
	}
	next, err := writeAddrValue(req.Value)
	if err != nil {
		return err
	}
	ser := c.nextSER()
	frame, err := cjt188.WriteAddrRequest(c.meterType, c.addr, ser, next)
	if err != nil {
		return driver.E(driver.ErrInternal, "write", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, frame, "write-addr")
	if err != nil {
		return err
	}
	fr, err := resp.Frame()
	if err != nil {
		return driver.E(driver.ErrProtocol, "write", c.devID, err.Error(), true, err)
	}
	// 写地址的应答可能来自旧地址，也可能已经来自新地址；两者都接受。
	if fr.Addr != c.addr && fr.Addr != next {
		return driver.E(driver.ErrProtocol, "write", c.devID,
			fmt.Sprintf("写地址应答地址 %s 既不是原地址 %s 也不是新地址 %s",
				fr.Addr, c.addr, next), false, nil)
	}
	if err := c.checkWriteAck(fr, cjt188.CtrlWriteAddr|cjt188.BitReply, cjt188.DIWriteAddr, ser, "写地址"); err != nil {
		return err
	}
	// 表已经改名：后续事务必须用新地址，否则这条连接从下一帧开始就
	// 全部超时。调用方（配置）仍需自行更新，重连后以配置为准。
	c.addr = next
	return nil
}

// checkWriteAck 校验写应答：正常应答的控制码必须是「请求 + 0x80」且 DI 与 SER
// 回显一致；异常应答的数据域则是 SER + 状态 ST(2)，不带错误码。
func (c *conn) checkWriteAck(fr cjt188.Frame, ctrl byte, want cjt188.DI, ser byte, what string) error {
	if fr.IsError() {
		// 写被拒同样按点级处理（ErrPermission 更贴切：这是「表不允许这次写」，
		// 不是链路坏了，也不该重连）。188 没有错误码，只能说明是哪条写命令。
		er, err := cjt188.ParseErrorReply(fr)
		if err != nil {
			return driver.E(driver.ErrProtocol, "write", c.devID, err.Error(), false, err)
		}
		return driver.E(driver.ErrPermission, "write", c.devID,
			fmt.Sprintf("%s被拒绝（SER %d，状态 ST 0x%04X）", what, er.SER, er.ST), false, nil)
	}
	if fr.Ctrl != ctrl {
		return driver.E(driver.ErrProtocol, "write", c.devID,
			fmt.Sprintf("%s应答控制码 %02X，应为 %02X", what, fr.Ctrl, ctrl), false, nil)
	}
	if _, err := cjt188.ParseReadReply(fr, want, ser); err != nil {
		return driver.E(driver.ErrProtocol, "write", c.devID, err.Error(), false, err)
	}
	return nil
}

// valveValue 把平台值翻译成开/关。
//
// 接受 bool 与数值：true/1 = 开，false/0 = 关。字符串（open/close/开/关）
// 由北向先转成布尔值；驱动只认能落到这两个状态的输入。
func valveValue(v model.Value) (bool, error) {
	n, ok := v.AsFloat()
	if !ok {
		return false, driver.E(driver.ErrUnsupported, "write", "", "阀门值需要 bool 或 0/1", false, nil)
	}
	switch n {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, driver.E(driver.ErrUnsupported, "write", "",
		fmt.Sprintf("阀门值 %v 不合法（0=关，1=开）", n), false, nil)
}

// writeAddrValue 把平台值翻译成 14 位新地址。
//
// 必须完整 14 位且不含 AA：写进去的 AA 会让表在之后的总线上变成通配地址，
// 除了广播就再也点不到它。
func writeAddrValue(v model.Value) (cjt188.Addr, error) {
	var out cjt188.Addr
	if v.Type != model.TypeString {
		return out, driver.E(driver.ErrUnsupported, "write", "",
			"写地址需要字符串值（14 位十进制）", false, nil)
	}
	a, err := cjt188.ParseAddr(v.Text)
	if err != nil {
		return out, driver.E(driver.ErrUnsupported, "write", "", err.Error(), false, err)
	}
	if a.HasWildcard() {
		return out, driver.E(driver.ErrUnsupported, "write", "",
			"写地址必须给完整的 14 位数字，不能含 AA 通配字节", false, nil)
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

type reply struct {
	fr  cjt188.Frame
	raw []byte
}

func (r reply) Frame() (cjt188.Frame, error) {
	if len(r.raw) == 0 && r.fr.Ctrl == 0 {
		return cjt188.Frame{}, errors.New("cjt188: 空应答")
	}
	return r.fr, nil
}

// exchange 发一帧并收一帧，全程持有总线锁。
func (c *conn) exchange(ctx context.Context, req []byte, op string) (reply, error) {
	c.link.Lock()
	defer c.link.Unlock()

	if c.opt.Gap > 0 {
		select {
		case <-ctx.Done():
			return reply{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		case <-time.After(c.opt.Gap):
		}
	}
	// 半双工纪律：发之前排空接收缓冲。顺序必须是「先 Drain 再 SetDeadline」，
	// 反序会让本次事务的截止时间被 Drain 压到「现在」，表现为每次写都超时。
	_ = c.link.Drain()
	if ctxDeadline, ok := ctx.Deadline(); ok {
		_ = c.link.SetDeadline(ctxDeadline)
	} else {
		_ = c.link.SetDeadline(time.Now().Add(c.deadline()))
	}

	if c.opt.Preamble != cjt188.PreambleLen {
		req = withPreamble(req, c.opt.Preamble)
	}
	if err := c.link.Write(req); err != nil {
		c.linkErr = driver.E(driver.ErrDisconnected, op, c.devID, "串口写入失败", true, err)
		return reply{}, c.linkErr
	}

	rd := cjt188.NewReader(c.link)
	fr, err := rd.Next()
	if err != nil {
		e := c.readError(op, err, ctx)
		if driver.IsLinkFailure(e) {
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
func (c *conn) checkAddr(fr cjt188.Frame, op string) error {
	if fr.Addr == c.addr {
		return nil
	}
	return driver.E(driver.ErrProtocol, op, c.devID,
		fmt.Sprintf("应答地址 %s 与请求 %s 不匹配（总线上可能有另一台表在应答）",
			fr.Addr, c.addr), true, nil)
}

// classify 把编解码层/异常应答映射为驱动错误分类。
//
// CJ/T 188 的异常应答**不带错误码**：数据域只有 SER + 状态 ST(2)，主站只能从「是哪条
// 命令、什么参数」推断原因。因此这里的分类必须保守地按「点级」处理：
//
//   - 异常应答（控制码 D6=1）说明表收到了请求但拒绝执行。最典型的来源是点表
//     里配了一个该表不支持的数据标识——这是**点表问题**，不是链路故障。归成
//     ErrProtocol + temporary 会让采集器每周期丢弃连接重连一次，还会被
//     CountsTowardBreaker 计入熔断：一个写错的 DI 就能把整台正常的表标成坏设备。
//     按 ErrAddress（不重试、不熔断、点级）上报，剩下点位照常读。
//   - 帧结构类错误（DI/SER 不匹配、控制码不对）说明这一帧应答与请求配不上，
//     但在「一问一答 + 排空 + 地址校验」之下下一帧是独立的，同样按点级处理；
//     只有 exchange 层的超时/断连才是链路级。
func (c *conn) classify(di cjt188.DI, op string, err error) error {
	var ex *cjt188.Exception
	if errors.As(err, &ex) {
		return driver.E(driver.ErrAddress, op, c.devID,
			fmt.Sprintf("%s：%s", di, ex.Error()), false, err)
	}
	return driver.E(driver.ErrProtocol, op, c.devID,
		fmt.Sprintf("%s：%v", di, err), false, err)
}

// ---------------------------------------------------------------------------
// 便利函数
// ---------------------------------------------------------------------------

func (c *conn) parseDI(op, s string) (cjt188.DI, error) {
	di, err := cjt188.ParseDI(s)
	if err != nil {
		return cjt188.DI{}, driver.E(driver.ErrAddress, op, c.devID, err.Error(), false, err)
	}
	return di, nil
}

// numericType 决定用什么 DataType 承载解码结果。
func numericType(dt model.DataType) model.DataType {
	if dt == model.TypeBool || dt == model.TypeString || dt == model.TypeBytes ||
		dt == model.TypeBCD16 || dt == model.TypeBCD32 || !dt.Valid() {
		return model.TypeFloat64
	}
	return dt
}

func bad(ref driver.PointRef, err error) driver.RawSample {
	return driver.RawSample{
		PointID: ref.ID,
		Quality: qualityOf(err),
		Err:     err,
	}
}

// qualityOf 把驱动错误分类映射为质量码。
//
// 写死 comm_error 会把「超时」「地址错」「权限不足」一起说成通讯故障：
// 下游据此做的告警分级、熔断计数与现场排查方向全部出错。同一条错误在
// 「错误码」与「质量」两个维度上必须给出一致结论。
func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress, driver.ErrUnsupported:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrPermission:
		return model.QualityBad
	}
	return model.QualityBad
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
	for i < len(frame) && frame[i] == cjt188.Preamble {
		i++
	}
	out := make([]byte, 0, n+len(frame)-i)
	for j := 0; j < n; j++ {
		out = append(out, cjt188.Preamble)
	}
	return append(out, frame[i:]...)
}
