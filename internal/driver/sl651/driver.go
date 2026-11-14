// Package sl651 实现《水文监测数据通信规约》SL 651—2014 中心站侧驱动。
//
// 分层与仓库其它表计驱动一致：Meta（能力声明）→ conn（事务/错误分类）
// → codec/sl651（帧 + 报文 + 要素纯函数编解码）→ transport。
//
// # 定位：中心站侧主站、查询-应答、整帧取全部
//
// 本驱动只做中心站（主站）角色：发 37H（查询全部）/38H（查询时段）/3AH
// （查询指定要素），解析上行应答。遥测站自报（2F/30/31/32/33/34）由 sim 包
// 内置从站模拟，不在驱动里主动发起。
//
// 为什么一次 Read 只发一次查询：SL 651 的应答帧是**整帧取全部要素**——一条
// 32H 定时报包含当前降水量、水位、电压、状态字等多个要素，37H 查询全部的
// 应答与定时报同形。这与 M-Bus 的 REQ_UD2（一条 RSP_UD 取整表）类似，与
// DLMS 的 Get-Request（一条读一个属性）不同。逐点发 37H 不仅慢，还会让同一
// 设备的点位落在不同快照上——那是一种「看起来对、其实前后矛盾」的错误。
//
// # 地址模型：5 字节 BCD 站号
//
// SL 651 的遥测站地址是 5 字节 BCD（A5..A1，A5 为高位），装 10 位十进制，
// 最大 9999999999。现场用的测站编码普遍是 8 位（如 31234567），因此不能按
// 「5 位数」截断。点表用 `device.unitId` 或 `labels.address` 表达，驱动内部
// 转成 [5]byte（高位补 00）。
//
// # 点位地址：要素 ASCII 简称或两位十六进制
//
// SL 651 的点位是「要素标识符」，即附录 C 表 C.1 的引导符（1 字节）+
// 数据定义字节（1 字节）。点表用 ASCII 简称（如 "Z"、"PT"、"VT"）或两位
// 十六进制（如 "39"、"26"、"38"）表达，驱动查 codec/sl651.LookupGuide 解析
// 成引导符后，在应答帧的要素组里找匹配的 Field。
//
// 示例点表：
//
//	{"id":"z1","address":{"kind":"symbol","path":"Z"}}      // 水位
//	{"id":"p1","address":{"kind":"symbol","path":"PT"}}     // 降水量累计值
//	{"id":"v","address":{"kind":"symbol","path":"VT"}}      // 电压
//
// # 明确不做
//
//   - 遥测站自报帧（2F/30/31/32/33/34）的主动发起：这是从站行为，sim 包
//     内置从站模拟；驱动只解析中心站收到的自报（通过 Reader 分帧）。
//   - 配置表读写（40~43H）、控制命令（4C~4FH）、图片报（36H）：这些是
//     中心站配置从站或从站应答控制，不属于常规采集路径。
//   - M3 多包重组：包总数 > 1 时驱动报 ErrUnsupported，不尝试跨包拼接。
package sl651

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	sl651codec "uic/internal/codec/sl651"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
)

// 默认参数。
const (
	// defaultTCPPort 是串口服务器/DTU 透传的常见端口。
	defaultTCPPort = "4001"
	// defaultRetries 是同一请求的重发次数（不含首次）。
	//
	// 串口上的 SL 651 帧在长线/干扰下会丢字节；重发 2 次能显著提高单次成功率，
	// 又不至于让一台坏终端把一轮采集拖到几十秒。
	defaultRetries = 2
	// defaultGap 是两次事务之间的最小静默。
	//
	// SL 651 标准没有规定主站之间的最小间隔（它假定单一主站），但现场大量
	// 用 RS-485 转换器，从发送切回接收需要时间；实测 30ms 能覆盖绝大多数
	// USB 转 485 的批量传输延迟。取 30ms 与仓库里 M-Bus 驱动一致。
	defaultGap = 30 * time.Millisecond
	// defaultTimeout 是单次事务超时（无配置时）。
	//
	// SL 651 的应答帧可达数百字节（小时报 34H 含 12 组 DRP + 12 组 DRZ），
	// 9600bps 下约 1 秒；留 3 秒覆盖转换器延迟与拆包。
	defaultTimeout = 3 * time.Second
)

// Options 是设备级配置开关（来自 labels.options）。
type Options struct {
	// Retries 是同一请求的重发次数（不含首次）。
	Retries int
	// Gap 覆盖帧间静默。
	Gap time.Duration
	// CenterAddr 是中心站地址（1 字节，0 表示不指定）。
	CenterAddr int
	// Password 是 2 字节密码（十六进制，如 "1234"）。
	Password [2]byte
}

// Driver 是 SL 651 中心站驱动。
type Driver struct{}

// New 返回驱动实例。
func New() *Driver { return &Driver{} }

// Meta 声明能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "sl651",
		Name:         "水文监测数据通信规约 SL 651（中心站）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:      driver.CapSupported,
			Write:     driver.CapUnsupported,      // 控制命令 4C~4FH 未实现
			Subscribe: driver.CapRuntimeDependent, // 遥测站自报（2F/30/32/34）
			Browse:    driver.CapUnsupported,      // 无对象表，只有要素标识符
			// 声明 supported 指的是「协议本身支持自报帧」，不是「驱动对外提供
			// 推送回调」：中心站不主动发自报帧，2F/30/32/34 的解析能力只供内置
			// 从站造帧与自检命令打印抓包。上层拿到的仍是 Read 拉取契约（与 MQTT
			// 推送驱动声明 supported 同源，见 driver-sdk-spec §3.10）。
			MultiRead: driver.CapSupported, // 一条查询取整帧全部要素
			// Discovery 与 BitAccess 显式声明 unsupported 而不是留空：留空会
			// 序列化成空串，不是能力四态之一（CapState 零值陷阱）。
			Discovery:   driver.CapUnsupported, // 无发现/枚举服务，地址是要素标识符
			BitAccess:   driver.CapUnsupported, // 要素值是标量，无位访问语义
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.port", Label: "串口", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0", Scope: "channel"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: 1200, Scope: "channel",
				Help: "标准典型值 1200/2400/9600；SL 651 常用 1200 或 2400"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: "none", Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "SL 651 标准 8N1；个别终端出厂为 8E1"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "半双工总线必须串行；同一物理串口由多个通道共享时按总线锁排队"},
			{Name: "labels.options", Label: "通信开关", Type: "string", Default: "", Scope: "device",
				Help: "retries=2,gap=30ms,center=1,password=1234"},
		},
		PointFields: []driver.ConfigField{
			{Name: "labels.address", Label: "要素地址", Type: "string", Required: true, Scope: "point",
				Placeholder: "Z",
				Help:        "要素 ASCII 简称（Z/PT/VT）或两位十六进制（39/26/38）"},
		},
		Description: "SL 651 水文监测终端（中心站）：按 5 字节 BCD 站号寻址，发 37H/38H/3AH 查询，一条应答取整帧全部要素，点位按要素标识符寻址（RS-485 / 串口服务器）",
	}
}

// Open 建链。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	addr, err := addressOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	opt := parseOptions(cfg.Device)

	l, err := link.Open(ctx, link.Options{
		Proto:       "sl651",
		DefaultPort: defaultTCPPort,
		Opener:      nil, // SL 651 只用 serial/tcp，不需要自定义 opener
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

// 站号范围：5 字节 BCD 一共 10 位十进制，最大 99 99 99 99 99。
//
// 这里不能按「5 位数」限制成 99999：SL 651 的遥测站地址就是 5 字节 BCD，
// 现场用的测站编码普遍是 8 位（如 31234567），按 99999 卡会把合法站号
// 直接判成配置错误。
const maxStation = 9_999_999_999

// addressOf 取遥测站 5 字节 BCD 地址。
//
// 优先 labels.address（字符串，能表达前导零），退化到 device.unitId
// （int，转成 5 字节 BCD）。为什么两种都收：unitId 是仓库里所有驱动共用的
// 字段，硬要求再写一遍地址会让很多现成的点表不能用；而 labels.address 能
// 显式表达「站号就是 00123456」这种带前导零的编码。
func addressOf(dev model.Device) ([5]byte, error) {
	var addr [5]byte
	raw := strings.TrimSpace(dev.Labels["address"])
	var n int64
	switch {
	case raw != "":
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return addr, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("device.labels.address %q 不是十进制整数（站号 0..%d）", raw, maxStation),
				false, err)
		}
		n = v
	case dev.UnitID > 0:
		n = int64(dev.UnitID)
	default:
		return addr, driver.E(driver.ErrUnsupported, "open", dev.ID,
			"device.labels.address 或 device.unitId required（SL 651 站号 0..9999999999）",
			false, nil)
	}
	if n < 0 || n > maxStation {
		return addr, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("站号 %d 超出 0..%d（5 字节 BCD，10 位十进制）", n, maxStation),
			false, nil)
	}
	// 低位在 addr[4]、高位在 addr[0]（A5..A1，A5 为高位字节）。
	for i := 4; i >= 0; i-- {
		addr[i] = byte(toBCD(int(n % 100)))
		n /= 100
	}
	return addr, nil
}

// toBCD 把 0..99 的两位十进制转成 BCD 字节。
func toBCD(n int) int {
	return (n/10)*16 + (n % 10)
}

// parseOptions 解析 labels.options。
func parseOptions(dev model.Device) Options {
	opt := Options{Retries: defaultRetries, Gap: defaultGap}
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
		case "center":
			if hasVal {
				if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= 255 {
					opt.CenterAddr = n
				}
			}
		case "password":
			if hasVal && len(val) == 4 {
				// 十六进制字符串 "1234" → [2]byte{0x12, 0x34}
				if hi, err := strconv.ParseUint(val[0:2], 16, 8); err == nil {
					if lo, err := strconv.ParseUint(val[2:4], 16, 8); err == nil {
						opt.Password = [2]byte{byte(hi), byte(lo)}
					}
				}
			}
		}
	}
	if opt.Gap < 0 {
		opt.Gap = 0
	}
	return opt
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	link    link.Link
	addr    [5]byte
	opt     Options
	devID   string
	timeout time.Duration

	// mu 串行化本连接上的事务。
	//
	// 总线锁（link.Lock）保证同一根线上不同设备不互相抢，但它不阻止同一台
	// 设备的两个事务并发；而 SL 651 没有事务号，两个交错的查询会让应答
	// 无法配对。
	mu     sync.Mutex
	closed bool
	// linkErr 记录上一次链路级错误，后续 Read 直接返回而不重试。
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

// Probe 验证链路：发 37H 查询全部，等一个应答。
//
// 为什么用 37H 而不是 2FH 链路维持：2FH 是遥测站自报，中心站不能主动发；
// 37H 查询全部是最常见的查询，应答帧包含至少一个要素，能验证地址/波特率/
// 校验位/要素解析全链路。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", false, nil)
	}
	b := sl651codec.Builder{
		Center:   c.opt.CenterAddr,
		Station:  c.addr,
		Password: c.opt.Password,
	}
	req, err := b.BuildQuery(sl651codec.FuncQueryAll, 1, time.Now())
	if err != nil {
		return driver.E(driver.ErrProtocol, "probe", c.devID, err.Error(), false, err)
	}
	_, err = c.exchange(ctx, req, "probe", func(f *sl651codec.Frame) error {
		if !sl651codec.IsResponse(f.Func) {
			return fmt.Errorf("probe 应答功能码 %02X 不是可解析的上行报文", f.Func)
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.linkErr = nil
	return nil
}

// Read 读本轮全部点位。
//
// 一次事务发 37H 查询全部，解析应答帧里的所有要素。点位地址是要素标识符
// （ASCII 简称或两位十六进制），驱动在应答帧的 Fields 里找匹配的 Field。
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

	// 发 37H 查询全部。
	b := sl651codec.Builder{
		Center:   c.opt.CenterAddr,
		Station:  c.addr,
		Password: c.opt.Password,
	}
	req, err := b.BuildQuery(sl651codec.FuncQueryAll, 1, time.Now())
	if err != nil {
		return allFailed(refs, err), err
	}
	rep, err := c.query(ctx, "read", req)
	if err != nil {
		return allFailed(refs, err), err
	}

	// 逐点位解码：在 rep.Fields 里找匹配的 Field。
	out := make([]driver.RawSample, 0, len(refs))
	for _, ref := range refs {
		s, err := decodeRef(rep, ref)
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
	return out, nil
}

// Report 发一次 37H（查询全部）并把整帧报文解析结果返回给调用方。
//
// 这是给自检命令用的入口：现场最常见的诉求是「这台站到底上送了哪些要素」，
// 而驱动的 Read 只回答「我点表里那几个点位有没有」。把整帧交出来，自检命令
// 就能把要素、分类码、观测时间、电压一次列全，并直接给出可粘贴的点位地址。
// 它不改变 Read 的语义，也不额外发帧。
func (c *conn) Report(ctx context.Context) (*sl651codec.Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "report", c.devID, "connection closed", false, nil)
	}
	b := sl651codec.Builder{
		Center:   c.opt.CenterAddr,
		Station:  c.addr,
		Password: c.opt.Password,
	}
	req, err := b.BuildQuery(sl651codec.FuncQueryAll, 1, time.Now())
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, "report", c.devID, err.Error(), false, err)
	}
	return c.query(ctx, "report", req)
}

// query 发查询并解析应答。
func (c *conn) query(ctx context.Context, op string, req []byte) (*sl651codec.Report, error) {
	var rep *sl651codec.Report
	_, err := c.transact(ctx, op, req, func(f *sl651codec.Frame) error {
		if !sl651codec.IsResponse(f.Func) {
			return fmt.Errorf("应答功能码 %02X 不是可解析的上行报文", f.Func)
		}
		r, err := sl651codec.ParseReport(f)
		if err != nil {
			return err
		}
		rep = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// transact 是唯一的事务入口：排空 → 设截止时间 → 发送 → 收帧 → 校验。
//
// 重发次数覆盖"完全没应答"与"帧坏了"两类失败。校验和错误也重发，因为
// 它在长总线上通常是瞬态干扰，而且终端已经收到了请求（流水号会加一），
// 重发同一流水号让终端回放上一次的应答，不会让终端重发数据。
func (c *conn) transact(ctx context.Context, op string, req []byte,
	check func(*sl651codec.Frame) error) (*sl651codec.Frame, error) {
	c.link.Lock()
	defer c.link.Unlock()

	var lastErr error
	for attempt := 0; attempt <= c.opt.Retries; attempt++ {
		if attempt > 0 && c.opt.Gap > 0 {
			select {
			case <-ctx.Done():
				return nil, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
			case <-time.After(c.opt.Gap):
			}
		}
		f, err := c.exchange(ctx, req, op, check)
		if err == nil {
			return f, nil
		}
		lastErr = err
		// 断链与参数错误重发没有意义。
		if driver.KindOf(err) == driver.ErrDisconnected || driver.KindOf(err) == driver.ErrUnsupported {
			return nil, err
		}
		// 尊重 exchange 已经分好的「可重发」标记。CRC 错是瞬态干扰（可重发），
		// 而功能码不符、上行报文解不开这类失败说明**终端在正常应答、只是我们
		// 理解错了**——重发同一请求只会拿到同样的应答。两者在错误种类上同为
		// ErrProtocol，只看 KindOf 会把「不重发」的那类一起重试到上限。
		if !driver.IsRetryable(err) {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
		}
	}
	if isLinkFailure(lastErr) {
		c.linkErr = lastErr
	}
	return nil, lastErr
}

func (c *conn) exchange(ctx context.Context, req []byte, op string,
	check func(*sl651codec.Frame) error) (*sl651codec.Frame, error) {
	if c.opt.Gap > 0 {
		select {
		case <-ctx.Done():
			return nil, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		case <-time.After(c.opt.Gap):
		}
	}
	// 半双工纪律：发之前清空接收缓冲，避免把上一轮迟到的应答应成本轮。
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
		return nil, driver.E(driver.ErrDisconnected, op, c.devID, "串口写入失败", true, err)
	}

	rd := sl651codec.NewReader(c.link)
	f, err := rd.Next()
	if err != nil {
		return nil, c.readError(op, err, ctx, rd.BadBytes())
	}
	if err := check(f); err != nil {
		// 帧到达了但内容不对：区分"链路坏"与"终端回了别的东西"。
		//
		// 校验和错是瞬态干扰（可重试）；功能码不对说明终端在正常应答，
		// 只是我们理解错了，重试没用，直接报协议错误。
		if errors.Is(err, sl651codec.ErrBadFrame) {
			return nil, driver.E(driver.ErrProtocol, op, c.devID,
				"应答帧结构错误（长度/CRC/结束符）", true, err)
		}
		return nil, driver.E(driver.ErrProtocol, op, c.devID, err.Error(), false, err)
	}
	return f, nil
}

func (c *conn) deadline() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return defaultTimeout
}

func (c *conn) readError(op string, err error, ctx context.Context, badFrames int) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
	}
	// 关键区分：线路上**有字节回来，但一帧都没成**。
	//
	// Reader 会跳过坏帧继续扫描，所以它最终报出来的总是"超时"；但超时的
	// 原因可能截然不同。若已经跳过了坏帧，说明终端确实在应答，只是我们
	// 没解对——最常见的是波特率/校验位配错（1200 8N1 配成 2400 8E1 时
	// 收到的全是被采错的位），或地址错导致终端无视查询。把这归成
	// ErrProtocol 而不是 ErrTimeout，现场才会去查参数而不是去查线。
	if badFrames > 0 {
		return driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("收到 %d 个字节流但无一成帧：检查波特率/校验位（标准 1200 8N1）与站号", badFrames),
			true, err)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.devID,
			fmt.Sprintf("等待遥测站 %05d 应答超时", bcdToInt(c.addr)), true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.devID, "串口读取失败", true, err)
}

// bcdToInt 把 5 字节 BCD 地址转成整数（用于错误消息与自检输出）。
//
// SL 651 站号 A5..A1，A5 为高位字节，因此 addr[0] 是最高两位。
// 5 字节 BCD 装 10 位十进制，例如 00 31 23 45 67 → 31234567。
func bcdToInt(addr [5]byte) int64 {
	var n int64
	for i := 0; i < 5; i++ {
		n = n*100 + int64(addr[i]>>4)*10 + int64(addr[i]&0x0F)
	}
	return n
}

// Write 不支持。
//
// SL 651 的写方向是配置表修改（40~43H）、控制命令（4C~4FH）、人工置数（35H）。
// 这些命令的语义、参数格式、应答格式各不相同，且写错会改变终端的采集参数
// 或控制现场设备（闸门/水泵）。声明不支持，让上层把它当成配置错误，比
// "支持但只对某些终端有效"更诚实。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, driver.WriteResult{
			PointID: req.PointID,
			Quality: model.QualityBad,
			Err: driver.E(driver.ErrUnsupported, "write", c.devID,
				"SL 651 驱动不支持写入（配置表/控制命令/人工置数需按终端型号构造报文）", false, nil),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 点位解码
// ---------------------------------------------------------------------------

// decodeRef 把一条点位地址映射到应答里的一个要素字段。
//
// 地址语法：`<要素>` 或 `<要素>[<段号>]`。段号只对批量要素（DRP 的 12 段
// 5min 雨量、DRZ 的 12 段 5min 水位）有意义；不给段号时取第 0 段。
//
// 为什么要段号：DRP/DRZ 在报文里是「1 个 12/24 字节数据」，语义上却是
// 12 个独立时刻的值。不给段号就只能把整块当一个数（会溢出成垃圾值），
// 或者只能取第 0 段（丢掉其余 11 个时刻）。现场要看「最近 1h 每 5min 雨量」
// 时，丢掉的正是最有用的部分。
func decodeRef(rep *sl651codec.Report, ref driver.PointRef) (driver.RawSample, error) {
	guide, slot, err := guideOfWithSlot(ref.Address)
	if err != nil {
		return driver.RawSample{}, err
	}
	// 电压（VT）在报文里独立编报，codec 把它放在 Report.Voltage 而不是
	// Fields 里（表 32 的结构就是要素组之后单列电压）。这里要单独查一次，
	// 否则「VT 点位永远读不到」——而 CRC 与其它要素都正常，极难发现。
	var field sl651codec.Field
	if f, ok := rep.First(guide); ok {
		field = f
	} else if rep.Voltage != nil && rep.Voltage.Guide == guide {
		field = *rep.Voltage
	} else {
		return driver.RawSample{}, fmt.Errorf("报文里没有要素 %02X", guide)
	}
	v, err := field.SlotNumber(0, slot)
	if err != nil {
		// 缺测不是「地址写错」：规约 6.6.4.5 d) 允许某个时段没有数据，
		// 终端如实填 F。所以按 quality=bad 处理，让上层按缺失处理，而不是
		// 当作配置错误去报警。
		if errors.Is(err, sl651codec.ErrNoData) {
			return driver.RawSample{
				PointID: ref.ID,
				Value:   model.NumValue(model.TypeFloat64, 0),
				Quality: model.QualityBad,
			}, nil
		}
		return driver.RawSample{}, err
	}
	return driver.RawSample{
		PointID: ref.ID,
		Value:   model.NumValue(model.TypeFloat64, v),
		Quality: model.QualityGood,
	}, nil
}

// guideOf 从点位地址里取出要素引导符（段号固定为 0）。
func guideOf(a model.Address) (byte, error) {
	g, _, err := guideOfWithSlot(a)
	return g, err
}

// guideOfWithSlot 解析 `<要素>` 或 `<要素>[<段号>]`。
//
// 段号只对批量要素有效（见 Field.Slots）。语法用方括号而不是冒号/斜杠：
// DLMS 用冒号表达类号/属性号、M-Bus 用 `@` 与 `/` 表达存储号与序号，
// 而 SL 651 的「段」是同一数组的下标，方括号最不容易与其它协议混淆。
func guideOfWithSlot(a model.Address) (byte, int, error) {
	if a.Kind != model.AddressSymbol {
		return 0, 0, fmt.Errorf("SL 651 点位必须用符号地址（kind=symbol），得到 %q", a.Kind)
	}
	path := strings.TrimSpace(a.Path)
	if path == "" {
		return 0, 0, errors.New("要素地址为空")
	}
	slot := 0
	if i := strings.IndexByte(path, '['); i >= 0 {
		if !strings.HasSuffix(path, "]") {
			return 0, 0, fmt.Errorf("要素地址 %q 的段号缺少右方括号", path)
		}
		n, err := strconv.Atoi(strings.TrimSpace(path[i+1 : len(path)-1]))
		if err != nil {
			return 0, 0, fmt.Errorf("要素地址 %q 的段号不是整数", path)
		}
		if n < 0 {
			return 0, 0, fmt.Errorf("要素地址 %q 的段号不能为负", path)
		}
		slot = n
		path = strings.TrimSpace(path[:i])
	}
	guide, ok := sl651codec.LookupGuide(path)
	if !ok {
		return 0, 0, fmt.Errorf("要素地址 %q 不是附录 C 表 C.1 里的引导符", path)
	}
	// 段号只有在批量要素上才有意义。这里提前拦下，而不是让 SlotNumber
	// 返回「越界」——「这个要素没有段」比「段 3 越界」更好定位。
	if slot > 0 {
		if info, ok := sl651codec.GuideInfo(guide); !ok || info.Slot == 0 {
			return 0, 0, fmt.Errorf("要素 %q 不是批量要素，不接受段号（只有 DRP/DRZ 有多段）", path)
		}
	}
	return guide, slot, nil
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
