// Package mewtocol 实现 Mewtocol（计算机链接方式）主站驱动。
//
// 分层与其它驱动一致：Meta（能力声明）→ conn（事务/错误分类）→
// codec/mewtocol（纯函数编解码）→ link（串口或串口服务器 TCP 透传）。
//
// 三条刻意的边界：
//
//  1. **只做裸帧，不做 FP2-ET-LAN 的 16 字节封装。** FP7 的 LAN 口在
//     「与 FP2-ET-LAN 连接」模式下会在 MEWTOCOL 帧外再套一层头（`10H 00H`
//     + 长度 + 8 个 0 + 站号 + 连接号）。默认模式（不与 FP2-ET-LAN 连接）
//     只发裸帧。本实现只支持默认模式；遇到封装模式会表现为「发出去没有
//     任何应答」，需要在 PLC 侧改设置，而不是加一个开关去猜。
//
//  2. **布尔点位一律走单点写（WCS），不做「读字 → 改位 → 写字」。**
//     后者把一次写变成两次事务，而且两次事务之间别的客户端可能改了同
//     一个字的邻位——最后写入者会把对方的位一起覆盖掉。协议既然提供了
//     原子的单接点写，就没有理由不用。
//
//  3. **字区上的布尔点位可读不可写。** 协议没有「数据寄存器第 n 位」的
//     写命令（WCS 只覆盖 Y/R/L 这类接点区），只能读字改位再写回。这正是
//     上一条要避免的写模式，所以这里明确拒绝，并给出替代做法。拒绝比
//     悄悄做一次非原子写安全。
//
// 地址约定：`kind=register`，`area` 是区名（`x`/`y`/`r`/`l`/`t`/`c`/
// `dt`/`ld`/`fl`/`ix`/`iy`/`id`/`sv`/`ev`），`offset` 是**字地址**，
// `bit` 是字内位号（0..15，仅接点区与字区的布尔点位使用）。
//
// 为什么 offset 用字地址而不是「接点号」：协议里接点号的末位是十六进制、
// 前面是十进制（X1F 与 X16.0 是同一个接点），把它当成一个整数会与用户的
// 直觉冲突，也让「X1F 之后是 X20」这种常识性错误无法被发现。分成
// 「字 + 位」两个十进制整数之后，进制混排只存在于编解码层。
package mewtocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"uic/internal/codec/mewtocol"
	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
	"uic/internal/transport/serialport"
)

// defaultTCPPort 是 Mewtocol over TCP 的常见端口。
//
// 这事没有标准答案：不同厂商的网关分别用 2000、60001、9094，FP7 的编程口
// 是 32769..32772。取 2000（最常见的以太网单元默认值），但必须可配——
// 把它硬编码成默认值只是为了让「host 不带端口」能连上，不是协议规定。
const defaultTCPPort = "2000"

// defaultGap 是帧间静默。
//
// ASCII 帧在 9600bps 下一个字符约 1ms，118 字符的帧约 120ms；但帧**之间**
// 只需要给对端留出「从发送切回接收」的时间。RS-485 收发器通常在 1ms 内
// 完成切换，USB 转 485 的批量传输延迟实测 5~20ms。取 10ms：足够覆盖
// 大多数适配器，又不会让 24 字一次的事务序列明显变慢。
const defaultGap = 10 * time.Millisecond

// Driver 是 Mewtocol 主站驱动。
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
		ID:           "mewtocol",
		Name:         "松下 FP 系列（Mewtocol 计算机链接）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read:      driver.CapSupported,
			Write:     driver.CapSupported,
			Subscribe: driver.CapUnsupported,
			Browse:    driver.CapUnsupported,
			Discovery: driver.CapUnsupported,
			// 一次请求可覆盖连续的一段（最多 24 字），规划层合并后的
			// span 会被驱动再切成合法长度，因此是「支持」而不是「实验」。
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapSupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.port", Label: "串口", Type: "string", Scope: "channel",
				Placeholder: "/dev/ttyUSB0", Help: "串口承载时必填；TCP 承载留空"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: 9600, Scope: "channel",
				Help: "FP 系列出厂多为 9600；必须与 PLC 的系统寄存器设置一致"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: "odd", Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "计算机链接的出厂设置是奇校验；改成无校验会「能发不能收」"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
			{Name: "transport.address", Label: "TCP 地址", Type: "string", Scope: "channel",
				Placeholder: "192.168.1.10:2000",
				Help:        "串口服务器/以太网单元的 host:port；省略端口时用 2000"},
			{Name: "unitId", Label: "站号", Type: "int", Default: 1, Scope: "device",
				Help: "PLC 的单元号，1..99（两位十进制）。同一根线上必须唯一"},
			{Name: "labels.options", Label: "扩展选项", Type: "string", Scope: "device",
				Placeholder: "gap=10ms",
				Help:        "逗号分隔：gap=<时长> 覆盖帧间静默"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address", Label: "地址", Type: "string", Required: true, Scope: "point",
				Placeholder: "dt:100 或 x:1.15",
				Help:        "区名:字地址[.位号]。字区 dt/ld/fl，接点区 x/y/r/l/t/c，索引 ix/iy/id，定时器值 sv/ev"},
		},
		Description: "松下 FP 系列 PLC 的 Mewtocol（计算机链接）主站。" +
			"字区走 RD/WD，接点区走 RCC/WCC 按字读写、布尔量走 WCS 单点写，" +
			"定时器/计数器设定值与经过值走 RS/WS/RK/WK。",
	}
}

// Options 是设备级可调参数。
type Options struct {
	// Gap 覆盖帧间静默。
	Gap time.Duration
}

// Open 建链并校验站号。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	devID := cfg.Device.ID
	st, err := stationOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	opt, err := parseOptions(cfg.Device)
	if err != nil {
		return nil, err
	}
	l, err := link.Open(ctx, link.Options{
		Proto:       "mewtocol",
		DefaultPort: defaultTCPPort,
		Opener:      d.serial,
	}, cfg.Channel.Transport, cfg.Channel.EffectiveTimeout(), devID)
	if err != nil {
		return nil, err
	}
	return &conn{
		cfg:     cfg,
		link:    l,
		station: st,
		opt:     opt,
		devID:   devID,
		timeout: cfg.Device.Timeout.Duration(),
	}, nil
}

// stationOf 读取并校验站号。
//
// 缺省 1：手册说「没有特别指定时填 01」，而绝大多数单机连接就是 1。把它
// 做成必填只会让每个点表都多写一行没有信息量的配置。
func stationOf(dev model.Device) (mewtocol.Station, error) {
	n := dev.UnitID
	if n == 0 {
		n = 1
	}
	st := mewtocol.Station(n)
	if err := st.Validate(); err != nil {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
	}
	return st, nil
}

// parseOptions 解析 device.labels["options"]。
//
// 与 hostlink/ads 同一纪律：非法值、缺值、未知项都报错，绝不悄悄回默认值
// ——「帧间静默变成默认 10ms」这类症状在链路层根本看不出配置错了。
func parseOptions(dev model.Device) (Options, error) {
	opt := Options{Gap: defaultGap}
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
		case "gap":
			if !hasVal {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.gap 需要 =时长（如 10ms）", false, nil)
			}
			d, err := time.ParseDuration(val)
			if err != nil || d < 0 || d > time.Second {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.gap %q 必须是 0..1s 的时长", val), false, nil)
			}
			opt.Gap = d
		default:
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("未知的 options 项 %q（支持 gap）", key), false, nil)
		}
	}
	return opt, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	cfg     driver.OpenConfig
	link    link.Link
	station mewtocol.Station
	opt     Options
	devID   string
	timeout time.Duration

	// mu 串行化本连接上的事务。契约上同一 Conn 不会被并发调用，加锁是
	// 为了在错误使用下宁可慢一点，也不要让两条请求的字节交错。
	mu     sync.Mutex
	closed bool
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

// Probe 读一个数据寄存器，验证链路。
//
// 为什么是 DT00000 而不是别的：RD 是最基础、所有 FP 系列都支持的读命令，
// 而 DT0 在出厂配置里一定存在。用接点区会让「PLC 的接点区编号从 0 还是
// 从 1 开始」这类型号差异混进探活结果里。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", false, nil)
	}
	area, ok := mewtocol.Lookup("dt")
	if !ok {
		return driver.E(driver.ErrInternal, "probe", c.devID, "area dt missing", false, nil)
	}
	if _, err := c.readWords(ctx, area, 0, 1, "probe"); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

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
	// goodSamples 而不是 goodSpans：一个 span 成功返回不等于其中有点位成功。
	// readSpan 在解码阶段逐点失败时（bool 缺位号、类型与宽度不符）仍然返回
	// nil error，于是「全员失败」会被当成成功、Read 返回 nil——上层看到的
	// 是一台「在采但全坏」的设备，熔断与告警永不触发。见 docs §3.3.1。
	goodSamples := 0
	var firstSampleErr error
	countGood := func(samples []driver.RawSample) {
		for _, s := range samples {
			if s.Err == nil && (s.Quality == model.QualityGood || s.Quality == "") {
				goodSamples++
				continue
			}
			if firstSampleErr != nil {
				continue
			}
			firstSampleErr = s.Err
			if firstSampleErr == nil {
				// 质量码说坏、却没有带错误：不能因为「错误是 nil」就让
				// Read 在这条点位上一路沉默。合成一条，把无声变成可诊断。
				firstSampleErr = driver.E(driver.ErrProtocol, "read", c.devID,
					fmt.Sprintf("point %s came back with quality %q but no error", s.PointID, s.Quality),
					false, nil)
			}
		}
	}
	// markFailed 把一整个 span 的点位标成失败，但**跳过已经拿到样本的点位**：
	// 一个 span 可能被切成多个请求（每帧最多 24 字），前面的帧成功了、
	// 后面的帧失败时，前面的值不该被一起抹掉——它们是真实读到的数据，
	// 而重复追加还会让同一 ID 出现两个样本。
	markFailed := func(span driver.ReadSpan, err error) {
		seen := make(map[string]bool, len(out))
		for _, s := range out {
			seen[s.PointID] = true
		}
		for _, p := range span.Points {
			if seen[p.ID] {
				continue
			}
			out = append(out, driver.RawSample{PointID: p.ID, Quality: qualityOf(err), Err: err})
			if firstSampleErr == nil {
				firstSampleErr = err
			}
		}
	}
	// markRemaining 把从 i 开始的所有点位标成失败：链路级失败时它们还没
	// 发出去，若直接从结果里消失，采集层会当成「设备安静」而不是读失败。
	markRemaining := func(i int, err error) {
		for _, span := range spans[i:] {
			markFailed(span, err)
		}
	}
	for i, span := range spans {
		if err := ctx.Err(); err != nil {
			linkErr := driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			if firstErr == nil {
				firstErr = linkErr
			}
			// 本轮剩余点位一个字节都没发出去，必须显式标坏。
			markRemaining(i, linkErr)
			break
		}
		samples, err := c.readSpan(ctx, span)
		// 先收下已经拿到的样本（含多帧 span 里成功的那几帧），再处理失败：
		// 反过来会把这些值丢掉。
		countGood(samples)
		out = append(out, samples...)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// 整段失败时把每个点位都标成失败，而不是让它们从结果里消失：
			// 消失的点位在采集层表现为「没有新样本」，会被当成设备安静。
			markFailed(span, err)
			// 链路级失败（超时/断连）短路：继续逐个等完剩余 span 只会把
			// 一轮采集拖成 N 倍超时，而结果一定是全坏。
			if driver.IsLinkFailure(err) {
				markRemaining(i+1, err)
				break
			}
			continue
		}
	}
	if goodSamples == 0 && firstSampleErr == nil {
		firstSampleErr = firstErr
	}
	return out, driver.PartialReadError(goodSamples, firstSampleErr)
}

func (c *conn) readSpan(ctx context.Context, span driver.ReadSpan) ([]driver.RawSample, error) {
	if len(span.Points) == 0 {
		return nil, nil
	}
	area, err := c.areaOf(span.Points[0])
	if err != nil {
		return nil, err
	}
	for _, p := range span.Points {
		a, err := c.areaOf(p)
		if err != nil {
			return nil, err
		}
		if a.Key != area.Key {
			return nil, driver.E(driver.ErrInternal, "read", c.devID,
				fmt.Sprintf("span mixes areas %q and %q", area.Key, a.Key), false, nil)
		}
	}

	// T/C 接点没有「字内位」概念，也不能按字读（手册会把它们按每字 16 个
	// 接点重新编号，与 T/C 的十进制编号是两套体系）。它们只能逐点或
	// 最多 8 点一组地读，因此单独一条路径。
	if isTimerContact(area) {
		return c.readTimerContacts(ctx, span, area)
	}

	min, max, err := c.spanWordRange(span, area)
	if err != nil {
		return nil, err
	}
	if max < min {
		return nil, nil
	}

	// 一次请求最多 MaxWords 个字。规划层按 125 个寄存器合并，这里必须
	// 再切：超长的请求会被 PLC 回 41H，而那个错误信息说不清是哪一段太长。
	//
	// 切分之后**先把整段拼进一个连续缓冲、最后统一解码**，而不是「读一帧
	// 解一帧」。逐帧解码是这一处的原始缺陷：decodeSpan 会遍历 span 里的
	// **全部**点位，于是前半帧为后面的点位报 "point lies before the
	// response window"、后半帧为前面的点位报 "response shorter than
	// requested range"。两半各有一半点位坏，而 Read 因有成功样本仍返回
	// nil——症状是「连续 24 字以上的点位组每轮坏一半」，静默且无日志。
	//
	// 拼接还顺带解决了「单个点位比一帧还宽」：例如 32 字的字符串点位，
	// 逐帧解码无论怎么对齐都解不出来，拼接之后就是一段连续的字节。
	total := make([]byte, (max-min+1)*2)
	nFrames := (max - min + mewtocol.MaxWords) / mewtocol.MaxWords
	frameErr := make([]error, nFrames)
	// attempted 区分「读失败」与「根本没发」。少了它，链路级失败后没发出的
	// 那几帧会被当成成功，points 从全零的缓冲里解出 0 —— 一个假的、看起来
	// 完全正常的读数。
	attempted := make([]bool, nFrames)
	var firstErr error
	for i := 0; i < nFrames; i++ {
		start := min + i*mewtocol.MaxWords
		end := start + mewtocol.MaxWords - 1
		if end > max {
			end = max
		}
		attempted[i] = true
		data, err := c.readWords(ctx, area, start, end, "read")
		if err != nil {
			frameErr[i] = err
			if firstErr == nil {
				firstErr = err
			}
			// 链路级失败后剩下的帧不可能成功，继续发只会把一轮采集拖成
			// N 倍超时。
			if driver.IsLinkFailure(err) {
				break
			}
			continue
		}
		copy(total[(start-min)*2:], data)
	}

	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		w := pointWords(p)
		// 点位覆盖的帧区间：一个点位可以跨帧（拼接缓冲的用途之一），
		// 但它只有在**覆盖它的每一帧都读成功**时才能解码，否则那半个
		// 值是「新帧 + 旧帧残留」拼出来的垃圾。
		first := (p.Address.Offset - min) / mewtocol.MaxWords
		last := (p.Address.Offset + w - 1 - min) / mewtocol.MaxWords
		var frameFailure error
		for f := first; f <= last; f++ {
			if !attempted[f] {
				// 这一帧因为前面的链路级失败而没有发出去。
				frameFailure = firstErr
				if frameFailure == nil {
					frameFailure = driver.E(driver.ErrInternal, "read", c.devID,
						"frame was neither read nor failed", false, nil)
				}
				break
			}
			if frameErr[f] != nil {
				frameFailure = frameErr[f]
				break
			}
		}
		if frameFailure != nil {
			out = append(out, errSample(p.ID, frameFailure))
			continue
		}
		s, err := decodePoint(area, p, (p.Address.Offset-min)*2, total)
		if err != nil {
			out = append(out, errSample(p.ID, err))
			continue
		}
		out = append(out, s)
	}
	return out, firstErr
}

// isTimerContact 判断是不是 T/C 接点区（定时器接点 / 计数器接点）。
//
// 它们与 X/Y/R/L 的区别在于编号：X/Y/R/L 是「字 + 位」，T/C 是「一个号
// 一个接点」。这个差异决定了读命令：前者按字读（RCC），后者只能按点读
// （RCS/RCP）。
func isTimerContact(area mewtocol.Area) bool {
	return area.Kind == mewtocol.KindContact && (area.Code == "T" || area.Code == "C")
}

// readTimerContacts 按点读 T/C 接点（RCS 单点 / RCP 最多 8 点一组）。
//
// 为什么不用 RCC 的按字读：手册允许，但会把 T/C 按「每字 16 个接点」重新
// 编号（0000 表示 T0..T15），与 T/C 自身的十进制编号是两套体系。手册自己
// 也建议不要这么读。按点读的代价是点位多时事务次数多，但 T/C 通常只有
// 几只，而正确性优先。
func (c *conn) readTimerContacts(ctx context.Context, span driver.ReadSpan, area mewtocol.Area) ([]driver.RawSample, error) {
	out := make([]driver.RawSample, 0, len(span.Points))
	for start := 0; start < len(span.Points); start += mewtocol.MaxContacts {
		end := start + mewtocol.MaxContacts
		if end > len(span.Points) {
			end = len(span.Points)
		}
		group := span.Points[start:end]
		contacts := make([]mewtocol.Contact, 0, len(group))
		// pointIdx 记下每个接点对应组内哪个点位。被跳过的坏点位不进
		// contacts，若最后按 group 的下标去取 vals，坏点位会拿到邻点的
		// 值（先报地址错、再报一个好值），而真正请求的点位反而没有样本。
		pointIdx := make([]int, 0, len(group))
		for i, p := range group {
			if p.Address.Bit > 0 {
				// T/C 没有位号；地址上写了位号是点表错误。
				out = append(out, errSample(p.ID, driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("point %s: %s contacts are numbered, not bit-addressed (bit %d)", p.ID, area.Code, p.Address.Bit), false, nil)))
				continue
			}
			contacts = append(contacts, mewtocol.Contact{Area: area, Word: p.Address.Offset, Bit: 0})
			pointIdx = append(pointIdx, i)
		}
		if len(contacts) == 0 {
			continue
		}

		var (
			frame []byte
			err   error
		)
		if len(contacts) == 1 {
			frame, err = mewtocol.BuildReadContact(c.station, area, contacts[0].Word, 0)
		} else {
			frame, err = mewtocol.BuildReadContacts(c.station, contacts)
		}
		if err != nil {
			return out, driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
		}
		resp, err := c.exchange(ctx, frame, "read")
		if err != nil {
			return out, err
		}
		vals, err := mewtocol.DecodeContacts(resp.Data)
		if err != nil {
			return out, driver.E(driver.ErrProtocol, "read", c.devID, err.Error(), true, err)
		}
		if len(vals) < len(contacts) {
			return out, driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("response carries %d contact values, request asked for %d", len(vals), len(contacts)), true, nil)
		}
		for k, gi := range pointIdx {
			if k >= len(vals) {
				break
			}
			p := group[gi]
			out = append(out, driver.RawSample{
				PointID: p.ID, Value: model.BoolValue(vals[k]), Quality: model.QualityGood,
			})
		}
	}
	return out, nil
}

// spanWordRange 计算 span 覆盖的字地址闭区间。
//
// 布尔接点只占一个字的一部分，宽度算 1 个字；其它类型按 length 折算成字。
//
// 索引寄存器没有编号，区间由区宽固定（IX/IY 是 0..0，ID 是 0..1），因此
// 这里还要把两类配置错误显式拦下——它们此前都被静默忽略：
//
//   - offset 非 0：帧里的九个 0 就是「没有编号」的编码，那个 offset 根本
//     发不出去。忽略它会让一个本该指向 DT100 的点位悄悄读到索引寄存器。
//   - 点位宽度超过区宽：IX 只有 1 个字，声明成 uint32 就需要 2 个字，而
//     应答只回 1 个字。不拦的结果是运行期报「response shorter than
//     requested range」，看不出是哪条配置错了。
func (c *conn) spanWordRange(span driver.ReadSpan, area mewtocol.Area) (int, int, error) {
	if area.Kind == mewtocol.KindIndex {
		w := mewtocol.IndexWords(area)
		for _, p := range span.Points {
			if p.Address.Offset != 0 {
				return 0, 0, driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("point %s: %s has no address (index registers are not numbered), "+
						"address.offset must be 0, got %d", p.ID, area.Key, p.Address.Offset), false, nil)
			}
			if pw := pointWords(p); pw > w {
				return 0, 0, driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("point %s: %s holds %d word(s), point asks for %d (%s)"+
						"; declare %s points as uint16", p.ID, area.Key, w, pw, p.DataType, area.Key), false, nil)
			}
		}
		return 0, w - 1, nil
	}
	min := -1
	max := 0
	for _, p := range span.Points {
		w := pointWords(p)
		if p.Address.Offset < 0 {
			return 0, 0, driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("point %s: negative word address %d", p.ID, p.Address.Offset), false, nil)
		}
		if min < 0 || p.Address.Offset < min {
			min = p.Address.Offset
		}
		if e := p.Address.Offset + w - 1; e > max {
			max = e
		}
	}
	if min < 0 {
		return 0, -1, nil
	}
	return min, max, nil
}

// readWords 读 [start, end] 闭区间，返回大端规范化的字节（每字 2 字节）。
func (c *conn) readWords(ctx context.Context, area mewtocol.Area, start, end int, op string) ([]byte, error) {
	var (
		frame []byte
		err   error
	)
	switch area.Kind {
	case mewtocol.KindWord, mewtocol.KindIndex:
		frame, err = mewtocol.BuildReadWords(c.station, area, start, end)
	case mewtocol.KindContact:
		frame, err = mewtocol.BuildReadContactWords(c.station, area, start, end)
	case mewtocol.KindSetValue, mewtocol.KindElapsed:
		frame, err = mewtocol.BuildReadTimerValues(c.station, area, start, end)
	default:
		return nil, driver.E(driver.ErrUnsupported, op, c.devID,
			fmt.Sprintf("area %q cannot be read", area.Key), false, nil)
	}
	if err != nil {
		// 组帧失败几乎总是地址越界或超长，属于配置问题。
		return nil, driver.E(driver.ErrAddress, op, c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, frame, op)
	if err != nil {
		return nil, err
	}
	if want := end - start + 1; mewtocol.Words(resp.Data) != want {
		return nil, driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("response carries %d words, request asked for %d（%s %d..%d）",
				mewtocol.Words(resp.Data), want, area.Key, start, end), true, nil)
	}
	data, err := mewtocol.DecodeWords(resp.Data)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, op, c.devID, err.Error(), true, err)
	}
	return data, nil
}

// decodePoint 从一段连续的字数据里取出一个点位的值。
//
// byteOff 是该点位在 data 里的字节偏移（已由调用方按 span 起点折算）。
func decodePoint(area mewtocol.Area, p driver.PointRef, byteOff int, data []byte) (driver.RawSample, error) {
	width := pointWords(p)
	if byteOff < 0 || byteOff+width*2 > len(data) {
		// 走到这里说明 spanWordRange 与实际读取范围不一致，属于内部错误，
		// 不是设备行为——但也不能静默返回零值。
		return driver.RawSample{}, driver.E(driver.ErrInternal, "decode", "",
			fmt.Sprintf("point %s: window %d..%d bytes outside the %d-byte response",
				p.ID, byteOff, byteOff+width*2, len(data)), false, nil)
	}
	raw := data[byteOff : byteOff+width*2]

	if p.DataType == model.TypeBool {
		bit := p.Address.Bit
		if bit < 0 || bit > 15 {
			return driver.RawSample{}, driver.E(driver.ErrAddress, "decode", "",
				fmt.Sprintf("point %s: bool point on %s needs address.bit 0..15, got %d",
					p.ID, area.Key, bit), false, nil)
		}
		word := uint16(raw[0])<<8 | uint16(raw[1])
		return driver.RawSample{
			PointID: p.ID,
			Value:   model.BoolValue(word&(1<<uint(bit)) != 0),
			Quality: model.QualityGood,
		}, nil
	}

	v, err := value.Decode(raw, p.DataType, orderFor(width*2, p.ByteOrder))
	if err != nil {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "decode", p.ID, err.Error(), false, err)
	}
	return driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood}, nil
}

// orderFor 返回多字数值的字节序。
//
// 协议的原生布局是「字内低字节在前、低字在前」：32 位值 H00010002 线上是
// `0200 0100`，两处交换叠加之后正好等于「整体按 16 位字倒序」。本包的
// DecodeWords 已经消掉了字内交换，剩下的只是字序——对应 CDAB（32 位）
// 与 DCBA（64 位）。用户显式配置时以配置为准。
func orderFor(n int, order model.ByteOrder) model.ByteOrder {
	if order != "" {
		return order
	}
	if n >= 8 {
		return model.OrderDCBA
	}
	return model.OrderCDAB
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

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		if err := ctx.Err(); err != nil {
			err = driver.E(driver.ErrTimeout, "write", c.devID, "context done", true, err)
			results = append(results, writeFail(req, err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// 逐项返回（docs §3.4）：成功的条目**不能**因为同批另一条失败而被回填
	// 上错误。回填会让调用方把一条已经写下去的点位当成失败去重发——对计数
	// 类点位就是重复累加。
	return results, firstErr
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	area, err := c.areaOfRef(req)
	if err != nil {
		return writeFail(req, err), err
	}

	if req.DataType == model.TypeBool {
		return c.writeBit(ctx, req, area)
	}
	return c.writeWords(ctx, req, area)
}

// writeBit 写一个布尔量。
//
// 只走接点区的单点写（WCS）。字区上的布尔点位在这里被明确拒绝：协议没有
// 「数据寄存器第 n 位」的写命令，只能读字改位再写回，那是一次非原子写。
// 与其悄悄做，不如让人把点位改成 uint16 并按整字写。
func (c *conn) writeBit(ctx context.Context, req driver.WriteRequest, area mewtocol.Area) (driver.WriteResult, error) {
	if area.Kind != mewtocol.KindContact {
		err := driver.E(driver.ErrUnsupported, "write", c.devID,
			fmt.Sprintf("%s is not a contact area: a bool point on a word area has no atomic write "+
				"in this protocol (declare the point as uint16 and write the whole word)", area.Key),
			false, nil)
		return writeFail(req, err), err
	}
	if area.Code == "T" || area.Code == "C" {
		// T/C 的接点号就是定时器/计数器号，没有「第几位」的概念。
		// 点表里这类布尔量不带位号（bit = -1），显式写 0 也接受。
		if req.Address.Bit > 0 {
			err := driver.E(driver.ErrAddress, "write", c.devID,
				fmt.Sprintf("%s contacts are numbered by timer/counter, address.bit must be 0 or omitted, got %d",
					area.Code, req.Address.Bit), false, nil)
			return writeFail(req, err), err
		}
	}
	frame, err := mewtocol.BuildWriteContact(c.station, area, req.Address.Offset, req.Address.Bit, req.Value.Bool)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.devID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	if _, err := c.exchange(ctx, frame, "write"); err != nil {
		return writeFail(req, err), err
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

func (c *conn) writeWords(ctx context.Context, req driver.WriteRequest, area mewtocol.Area) (driver.WriteResult, error) {
	words := reqWords(req)
	if words <= 0 {
		words = 1
	}
	if words > mewtocol.MaxWords {
		err := driver.E(driver.ErrAddress, "write", c.devID,
			fmt.Sprintf("%s hold %d words, one frame carries at most %d",
				req.DataType, words, mewtocol.MaxWords), false, nil)
		return writeFail(req, err), err
	}
	// 索引寄存器的宽度由区码固定（IX/IY 1 字，ID 2 字），写请求必须正好
	// 是这个宽度：WDID 只带一个字会让 I1 留在旧值上却回「写成功」，而 I1
	// 正是 32 位索引的高半部分——这类「写成功但只写了一半」的错误在现场
	// 表现为「索引值偶尔不对」，比直接报错难查得多。
	if area.Kind == mewtocol.KindIndex {
		w := mewtocol.IndexWords(area)
		if words != w {
			err := driver.E(driver.ErrAddress, "write", c.devID,
				fmt.Sprintf("point %s: %s is a %d-word register, write carries %d (%s)"+
					"; declare it as %s", req.PointID, area.Key, w, words, req.DataType,
					map[int]string{1: "uint16", 2: "uint32"}[w]), false, nil)
			return writeFail(req, err), err
		}
	}

	data, err := value.Encode(req.Value, req.DataType, orderFor(words*2, req.ByteOrder))
	if err != nil {
		err = driver.E(driver.ErrUnsupported, "write", c.devID, err.Error(), false, err)
		return writeFail(req, err), err
	}

	var frame []byte
	switch area.Kind {
	case mewtocol.KindWord, mewtocol.KindIndex:
		frame, err = mewtocol.BuildWriteWords(c.station, area, req.Address.Offset, data)
	case mewtocol.KindContact:
		frame, err = mewtocol.BuildWriteContactWords(c.station, area, req.Address.Offset, data)
	case mewtocol.KindSetValue, mewtocol.KindElapsed:
		frame, err = mewtocol.BuildWriteTimerValues(c.station, area, req.Address.Offset, data)
	default:
		err = driver.E(driver.ErrUnsupported, "write", c.devID,
			fmt.Sprintf("area %q cannot be written", area.Key), false, nil)
	}
	if err != nil {
		if driver.KindOf(err) == driver.ErrUnsupported {
			return writeFail(req, err), err
		}
		err = driver.E(driver.ErrAddress, "write", c.devID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	if _, err := c.exchange(ctx, frame, "write"); err != nil {
		return writeFail(req, err), err
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// reqWords 计算一次写入占的字数。写请求不带点位定义，宽度只能从地址与
// 数据类型自身推断：显式给了 address.length 就用它，否则按类型宽度。
func reqWords(req driver.WriteRequest) int {
	if req.Address.Length > 0 {
		return req.Address.Length
	}
	if n := req.DataType.Registers(); n > 0 {
		return n
	}
	return 1
}

// pointWords 计算一个读点位占的字数。
//
// 写请求不带点位定义，只能从数据类型自身推断宽度；读点位则有地址长度
// 可用。布尔接点无论 dataType 是否为 bool 都只占一个字（16 个接点打包），
// 其它类型优先用 address.length，缺省时按类型的寄存器宽度折算。
func pointWords(p driver.PointRef) int {
	if p.DataType == model.TypeBool {
		return 1
	}
	if p.Length > 0 {
		return p.Length
	}
	if n := p.DataType.Registers(); n > 0 {
		return n
	}
	return 1
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// exchange 发一帧并收一帧，返回已校验的应答。
//
// 全程持有总线锁：从「排空」到「收完一帧」之间不能有别的设备插入事务，
// 否则收到的字节会来自另一台 PLC。
func (c *conn) exchange(ctx context.Context, frame []byte, op string) (mewtocol.Response, error) {
	c.link.Lock()
	defer c.link.Unlock()

	if c.opt.Gap > 0 {
		select {
		case <-ctx.Done():
			return mewtocol.Response{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		case <-time.After(c.opt.Gap):
		}
	}
	// 顺序很关键：Drain 会把读截止时间压到「现在 + 1ms」，必须在它之后
	// 再设本次事务的截止时间。反过来会让截止时间被覆盖成过去时刻，
	// 症状是「连上了，但每次写都 timeout」。
	_ = c.link.Drain()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.link.SetDeadline(dl)
	} else {
		_ = c.link.SetDeadline(time.Now().Add(c.deadline()))
	}

	if err := c.link.Write(frame); err != nil {
		return mewtocol.Response{}, driver.E(driver.ErrDisconnected, op, c.devID, "write failed", true, err)
	}

	rd := mewtocol.NewReader(c.link)
	raw, err := rd.Next()
	if err != nil {
		return mewtocol.Response{}, c.readError(op, err, ctx)
	}
	resp, err := mewtocol.ParseResponse(raw)
	if err != nil {
		return mewtocol.Response{}, c.classify(op, err)
	}
	// 站号必须校验：总线上若有两台 PLC 配成同一个站号，两台的应答都会
	// 发出来，不校验就会把 A 的数据记到 B 头上，而且完全看不出异常。
	if err := c.checkStation(resp, op); err != nil {
		return mewtocol.Response{}, err
	}
	if err := c.checkEcho(frame, resp, op); err != nil {
		return mewtocol.Response{}, err
	}
	if resp.ErrCode != 0 {
		return mewtocol.Response{}, c.classify(op, resp.Err())
	}
	return resp, nil
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
		return driver.E(driver.ErrTimeout, op, c.devID,
			"no response; check station number, baud rate/parity, and whether the PLC is in link mode", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.devID, "read failed", true, err)
}

// checkStation 校验应答站号。
func (c *conn) checkStation(resp mewtocol.Response, op string) error {
	if c.station == mewtocol.Broadcast {
		// 广播请求本来就不该有应答；走到这里说明总线上有设备回了。
		return driver.E(driver.ErrProtocol, op, c.devID,
			"broadcast request received a response", true, nil)
	}
	if resp.Station == c.station {
		return nil
	}
	return driver.E(driver.ErrProtocol, op, c.devID,
		fmt.Sprintf("response from station %s, request addressed station %s "+
			"(another PLC on the line probably shares the same station number)",
			resp.Station.Format(), c.station.Format()), true, nil)
}

// checkEcho 校验应答回显的命令码。
//
// 只比前两位：应答里 `RCC`/`RCS`/`RCP` 一律回 `RC`，`WCC`/`WCS` 回 `WC`。
// 按整条命令码比会把每一条接点命令都判成错帧。
func (c *conn) checkEcho(frame []byte, resp mewtocol.Response, op string) error {
	// 帧结构是 `%` + 站号2 + `#` + 命令码，命令码从第 4 个字节开始。
	if len(frame) < 6 {
		return nil
	}
	cmd := string(frame[4:6])
	if resp.Code == "" {
		// 异常应答不带命令码回显，无从比较。
		return nil
	}
	if resp.Code != mewtocol.EchoCode(cmd) {
		return driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("response echoes %q, request was %q", resp.Code, cmd), true, nil)
	}
	return nil
}

// classify 把编解码层的错误映射成驱动错误分类。
func (c *conn) classify(op string, err error) error {
	var ec *mewtocol.ErrorCode
	if errors.As(err, &ec) {
		switch {
		case ec.Code == 0x66 || ec.Code == 0x61 || ec.Code == 0x60 || ec.Code == 0x67:
			// 地址错、数据范围错、参数错、无数据：都是点表配置问题。
			return driver.E(driver.ErrAddress, op, c.devID, ec.Error(), false, err)
		case ec.Code == 0x65:
			// 写保护：权限/模式问题，重试无用。
			return driver.E(driver.ErrPermission, op, c.devID, ec.Error(), false, err)
		case ec.Code == 0x63:
			// 运行模式不允许该命令（例如 PLC 在编程模式）。
			return driver.E(driver.ErrPermission, op, c.devID, ec.Error(), ec.Retryable(), err)
		}
		return driver.E(driver.ErrProtocol, op, c.devID, ec.Error(), ec.Retryable(), err)
	}
	var ck *mewtocol.ChecksumError
	if errors.As(err, &ck) {
		// BCC 错说明总线上有噪声或位错，重发一次是有意义的。
		return driver.E(driver.ErrProtocol, op, c.devID, err.Error(), true, err)
	}
	return driver.E(driver.ErrProtocol, op, c.devID, err.Error(), true, err)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// areaOf 把点位的区名解析成区域定义。
func (c *conn) areaOf(p driver.PointRef) (mewtocol.Area, error) {
	return c.areaOfName("read", p.Address.Area, p.ID)
}

func (c *conn) areaOfRef(req driver.WriteRequest) (mewtocol.Area, error) {
	return c.areaOfName("write", req.Address.Area, req.PointID)
}

// areaOfName 解析区名。op 由调用方给出而不是写死：区名写错是点表问题，
// 报错里的操作名必须指向**真正失败的那一步**（采集还是写入），否则现场照着
// 「open 失败」去查建链，永远查不到。
func (c *conn) areaOfName(op, name, pointID string) (mewtocol.Area, error) {
	area, ok := mewtocol.Lookup(name)
	if !ok {
		msg := fmt.Sprintf("unknown area %q (supported: %s)",
			name, strings.Join(mewtocol.Keys(), ", "))
		if pointID != "" {
			msg = fmt.Sprintf("point %s: %s", pointID, msg)
		}
		return mewtocol.Area{}, driver.E(driver.ErrAddress, op, c.devID, msg, false, nil)
	}
	return area, nil
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityOf(err), Err: err}
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}
}

func qualityOf(err error) model.Quality {
	var ec *mewtocol.ErrorCode
	if errors.As(err, &ec) {
		switch ec.Code {
		case 0x60, 0x61, 0x66, 0x67:
			// 参数错、无数据、地址错、数据范围错：点表配置问题。
			return model.QualityAddressError
		}
		// 链路类（ClassLink，0x22..0x38 整段）与可重试的协议/处理类，
		// 都是「重连或重发可能就好了」的通信问题。这里从 ErrorCode 自身
		// 的 Kind()/Retryable() 派生，而不是手抄一份码表——手抄的列表曾
		// 漏掉 0x29/2A/2B/2C/2D/2E/2F/31/34/35/37 共 11 个链路码，让它们
		// 的通信故障显示成 bad，运维会去查点表而不是查线路。
		if ec.Kind() == mewtocol.ClassLink || ec.Retryable() {
			return model.QualityCommError
		}
		return model.QualityBad
	}
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
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var t interface{ Timeout() bool }
	if errors.As(err, &t) {
		return t.Timeout()
	}
	// 串口读超时在不同平台上是不同的错误值；按措辞兜底。
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}
