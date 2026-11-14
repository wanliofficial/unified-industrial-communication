// Package toledo 实现 METTLER TOLEDO「连续输出（Continuous Output）」的采集驱动。
//
// 分层与仓库其它驱动一致：Meta（能力声明）→ conn（推送模型 + 错误分类）→
// codec/toledo（帧分帧 + ASCII 三段式纯函数编解码）→ transport。
//
// # 定位：衡器是主动推送的一端，驱动是只读客户端
//
// TOLEDO 连续输出与仓库里其它协议有一个根本差别：衡器**自己不询问就持续往上送**
// 帧（典型 10~25 帧/秒），帧里没有站号、寄存器、功能码。因此本驱动：
//
//   - 只做客户端（C）角色：连上串口/TCP，被动收帧，不发命令；
//   - 只读：没有写、没有事务、没有应答配对；
//   - 点位用 symbol 寻址：每个点位是「当前读数」的一个字段（weight / status /
//     unit / mode / stable / motion / decimals），而不是某个寄存器。
//
// # 推送模型与拉取型驱动的根本不同
//
// 驱动契约是拉取型：Read 立即返回一批样本。而衡器不会因为你「问了一次」而回答。
// 两者的转换只有一种做法是对的：**Open 后起一个后台 goroutine 持续收帧，把最新一帧
// 读数存进 conn，Read 从 conn 里取**。
//
// 由此带来一条硬性约束，与 MQTT 驱动同源：**Read 里没有任何网络交互**，耗时与点位
// 数量无关，链路安静（衡器没推帧）也不会让它超时。真正断链时（读到底层错误）才把
// 连接标记为重连，而不是每次 Read 都去「探一下」——那会让安静的正常设备被误判为断链。
//
// # 质量码语义
//
// 推送型协议的质量码只能表达「读数有多可信」，不能表达「读到了寄存器」：
//
//   - 从未收到过帧              → comm_error（还没见过这设备）；
//   - 收到过但超过 staleAfter   → stale（衡器可能掉电/断线）；
//   - 最新帧状态 US（运动）      → 重量类点位 uncertain（数值在跳动，未稳定）；
//   - 最新帧状态 OL/UL           → 重量类点位 bad（超/欠量程，数值无意义）；
//   - 最新帧数值段是掩码          → 重量类点位 bad（衡器用占位符表达「出错」）；
//   - 其余                        → good。
//
// 注意 status / unit / mode / decimals 这些「描述性字段」不受称量状态影响：无论稳定
// 与否，它们本身都是有效信息，故恒为 good（在读数新鲜的前提下）。只有重量类点位
// 随称量状态降级。
package toledo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	toledocodec "uic/internal/codec/toledo"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
)

// 默认参数。
const (
	// defaultTCPPort 是串口服务器/DTU 透传的常见端口。
	defaultTCPPort = "4001"
	// defaultStaleAfter 是「多久没收到新帧就判 stale」的兜底值。
	//
	// 连续输出通常 10~25 帧/秒，正常几毫秒就有新帧。30 秒没帧只可能是衡器掉电、
	// 断线或链路真断。取 30 秒与 MQTT 驱动一致，避免一次抖动就把整页点位判 stale。
	defaultStaleAfter = 30 * time.Second
	// defaultReadTimeout 是后台收帧循环的读超时。
	//
	// 它必须**大于**衡器的帧间隔、又**小于** staleAfter：前者保证衡器正常推送时不会
	// 频繁触发超时重设，后者保证链路真断时能在 staleAfter 之前被察觉。取 10 秒——
	// 远大于 25 帧/秒的 40ms 间隔，又远小于 30 秒的 stale 阈值。
	defaultReadTimeout = 10 * time.Second
	// defaultProbeTimeout 是 Probe 等待首帧的上限。
	//
	// Probe 要验证「连上后能收到帧」，但衡器可能刚上电还没开始推。给 3 秒覆盖正常
	// 启动延迟，又不至于让一次探测把采集周期拖太久。
	defaultProbeTimeout = 3 * time.Second
)

// Options 是设备级配置开关（来自 labels.options）。
type Options struct {
	// StaleAfter 覆盖「多久没新帧判 stale」。<=0 时用设备采集周期推导或兜底值。
	StaleAfter time.Duration
	// ReadTimeout 覆盖后台收帧的读超时。<=0 时用 defaultReadTimeout。
	ReadTimeout time.Duration
}

// Driver 是 TOLEDO 连续输出客户端驱动。
type Driver struct{}

// New 返回驱动实例。
func New() *Driver { return &Driver{} }

// Meta 声明能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "toledo",
		Name:         "METTLER TOLEDO 连续输出（ASCII 逗号三段式，只读推送）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster}, // 客户端（C）侧：只读、无应答（与 MQTT/MTConnect 同例）
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:  driver.CapSupported,
			Write: driver.CapUnsupported, // 衡器只读，无写命令
			// 声明 supported 指的是「协议本身是推送型、由驱动内部收帧」，
			// 不是「驱动对外提供推送回调」。上层拿到的仍然是 Read 拉取契约，
			// 推送在包内被消化成最新一帧读数。若声明 unsupported，配置界面
			// 会建议用户「换个协议」，方向就错了（与 MQTT 驱动同源）。
			Subscribe: driver.CapSupported,
			Browse:    driver.CapUnsupported, // 无对象表，只有读数字段
			// 连续输出没有发现/枚举服务，也没有位访问语义；显式声明而不是
			// 留空——留空会序列化成空串，不是能力四态之一。
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported, // 一帧读数覆盖全部点位
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.port", Label: "串口", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0", Scope: "channel"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: 9600, Scope: "channel"},
			{Name: "transport.address", Label: "设备地址", Type: "string", Required: true,
				Placeholder: "127.0.0.1:4001", Scope: "device",
				Help: "TCP 透传时填 host:port；串口时忽略"},
			{Name: "labels.options.staleAfter", Label: "陈旧阈值", Type: "duration",
				Default: "30s", Scope: "device",
				Help: "多久没收到新帧就把点位判为 stale"},
			{Name: "labels.options.readTimeout", Label: "读超时", Type: "duration",
				Default: "10s", Scope: "device",
				Help: "后台收帧循环的读超时，须小于陈旧阈值"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.path", Label: "读数字段", Type: "enum", Required: true,
				Enum:  []string{"weight", "status", "unit", "mode", "stable", "motion", "decimals"},
				Scope: "point",
				Help:  "weight=重量值 status=ST/US/OL/UL unit=单位 mode=GS/NT/TR stable=是否稳定 motion=是否运动中 decimals=小数位数"},
		},
		Description: "连接主动推送连续输出帧的称重仪表（含各品牌 Toledo compatible 仪表），只读采集重量、状态与单位。",
	}
}

// pointState 记录一个点位读的是哪一帧读数里的哪个字段。
type pointState struct {
	id     string
	symbol string
	dt     model.DataType
}

// conn 是一条 TOLEDO 连续输出连接。
type conn struct {
	deviceID    string
	link        link.Link
	points      map[string]*pointState
	staleAfter  time.Duration
	readTimeout time.Duration

	mu      sync.Mutex
	closed  bool
	linkErr error
	latest  toledocodec.Reading
	// latestAt 是最近一帧**成功解析**的时间。
	latestAt time.Time
	hasData  bool

	// 收帧诊断：用于区分「链路上根本没字节」与「有字节但一帧都解不出」。
	//
	// 这两者的排查方向完全不同：前者查接线/供电/仪表是否开启连续输出，后者查
	// 帧格式（二进制 vs ASCII）、分隔符与波特率/校验位。把它们混成一句「超时」，
	// 现场会先去换线。
	bytesSeen    int   // 从链路读到的原始字节数（含被当噪声丢掉的）
	dropped      int   // 被当作噪声丢弃的字节数
	badFrames    int   // 收到成行、但 Parse 失败的帧数
	lastParseErr error // 最近一次解析失败的原因（用于错误消息）
}

// Open 建链并启动后台收帧。
//
// 只接受 serial 与 tcp：衡器的连续输出在现场要么是直连 RS-232/RS-485，要么是
// 串口服务器透传到以太网。两种接法的分帧与解析完全一样，差别只在字节从哪来。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "serial" && t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("toledo requires serial or tcp transport, got %q", t.Kind), false, nil)
	}

	opt := parseOptions(cfg.Device)
	staleAfter := staleAfterFor(cfg.Device, opt)
	readTimeout := opt.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	// 读超时必须**小于**陈旧阈值，否则链路真断时 Read 早已把点位判 stale 而收帧
	// 循环还没察觉，会掩盖断链。这里做一致性校正而不是报错：配置者不该为两个
	// 字段的耦合费心。
	//
	// 校正分两步，且**顺序不能反**：先对半，再设下限。反过来（先设 1 秒下限）
	// 会在 staleAfter ≤ 2s 时让 readTimeout == staleAfter，把「必须小于」这条
	// 不变式破坏掉。
	if readTimeout >= staleAfter {
		readTimeout = staleAfter / 2
	}
	if readTimeout > 0 && readTimeout < time.Second && staleAfter > 2*time.Second {
		readTimeout = time.Second
	}
	if readTimeout <= 0 {
		// staleAfter 被配得极小（< 2ns 级别）时对半也会得到 0；保底一个非零值，
		// 否则 SetDeadline(now+0) 会立刻超时，收帧循环空转。
		readTimeout = time.Millisecond
	}

	points := make(map[string]*pointState, len(cfg.Device.Points))
	for _, p := range cfg.Device.Points {
		if !p.Enabled {
			continue
		}
		if p.Address.Kind != model.AddressSymbol {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
				fmt.Sprintf("point %q: toledo requires symbol addresses, got %q", p.ID, p.Address.Kind), false, nil)
		}
		sym := strings.ToLower(strings.TrimSpace(p.Address.Path))
		if !knownSymbol(sym) {
			return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID,
				fmt.Sprintf("point %q: 未知读数字段 %q（可用：weight/status/unit/mode/stable/motion/decimals）", p.ID, p.Address.Path),
				false, nil)
		}
		dt := p.DataType
		if dt == "" {
			dt = defaultDataType(sym)
		}
		points[p.ID] = &pointState{id: p.ID, symbol: sym, dt: dt}
	}
	if len(points) == 0 {
		// 一个点位都没有的设备不需要连接；但不能静默成功，否则「配置写错导致 0
		// 点位」会表现为「设备在线但没数据」。
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"device has no enabled points", false, nil)
	}

	lnk, err := link.Open(ctx, link.Options{
		Proto:       "toledo",
		DefaultPort: defaultTCPPort,
	}, t, cfg.Channel.EffectiveTimeout(), cfg.Device.ID)
	if err != nil {
		return nil, err
	}

	c := &conn{
		deviceID:    cfg.Device.ID,
		link:        lnk,
		points:      points,
		staleAfter:  staleAfter,
		readTimeout: readTimeout,
	}
	go c.pump()
	return c, nil
}

// pump 是后台收帧循环：持续读帧、解析、把最新读数存进 conn。
//
// 它是推送模型的核心。Read 从这里取「最新一帧」，绝不自己读链路——否则安静时
// Read 会阻塞到超时，把正常设备误判为断链。
func (c *conn) pump() {
	r := toledocodec.NewReader(c.link)
	for {
		c.link.SetDeadline(time.Now().Add(c.readTimeout))
		line, err := r.Next()
		// 每一轮都把 Reader 的累计量同步进 conn：Read/Probe 要用它区分
		// 「链路上一点动静都没有」与「有字节但一帧都解不出」。
		c.noteBytes(r.BytesRead(), r.Dropped())
		if err != nil {
			if c.isClosed() {
				return
			}
			if isTimeout(err) {
				// 读超时在连续输出下通常只是「衡器暂时安静」，不是断链。继续等，
				// 由 Read 依据 latestAt 判 stale。只有非超时错误才标记断链。
				continue
			}
			c.setLinkErr(err)
			return
		}
		reading, perr := toledocodec.Parse(line)
		if perr != nil {
			// 解析失败不等于链路坏：多半是波特率/校验位配错或帧格式选了二进制。
			// 不记 linkErr（那会让 Read 整体上抛），但必须计数——否则「有字节
			// 但一帧都不成」会被 Read 当成「没收到」，把现场的排查方向引到
			// 接线/供电上，而不是仪表帧格式。
			c.noteBadFrame(perr)
			continue
		}
		c.mu.Lock()
		c.latest = reading
		c.latestAt = time.Now()
		c.hasData = true
		c.mu.Unlock()
	}
}

// noteBytes 记录从链路读到的字节数与噪声丢弃数。
func (c *conn) noteBytes(bytesRead, dropped int) {
	c.mu.Lock()
	if bytesRead > c.bytesSeen {
		c.bytesSeen = bytesRead
	}
	c.dropped = dropped
	c.mu.Unlock()
}

// noteBadFrame 记录一次「成行但解不出」的帧。
func (c *conn) noteBadFrame(err error) {
	c.mu.Lock()
	c.badFrames++
	c.lastParseErr = err
	c.mu.Unlock()
}

// Stats 是收帧过程的只读快照，供自检命令与排障输出使用。
//
// 它不改变 Read 的语义，只是把「链路上发生了什么」暴露出来：Toledo 只有推送、
// 没有命令可发，现场唯一能看的就是收帧质量（收到多少字节、坏帧几次、最后一帧
// 多旧）。
type Stats struct {
	// BytesSeen 是从链路读到的原始字节数（含被当噪声丢弃的）。
	BytesSeen int
	// Dropped 是被当作噪声丢弃的字节数（没有落在合法帧起点上的字节）。
	Dropped int
	// BadFrames 是收到成行但解析失败的帧数。
	BadFrames int
	// HasFrame 表示是否至少成功解析过一帧。
	HasFrame bool
	// FrameAge 是距最近一帧成功解析的时间；HasFrame 为假时无意义。
	FrameAge time.Duration
	// LastError 是最近一次解析失败的原因（可能为空）。
	LastError string
	// Raw 是最近一帧成功解析的原始文本（不含 CR/LF）。
	Raw string
}

// Stats 返回收帧统计快照。
func (c *conn) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Stats{
		BytesSeen: c.bytesSeen,
		Dropped:   c.dropped,
		BadFrames: c.badFrames,
		HasFrame:  c.hasData,
		Raw:       c.latest.Raw,
	}
	if c.hasData {
		st.FrameAge = time.Since(c.latestAt)
	}
	if c.lastParseErr != nil {
		st.LastError = c.lastParseErr.Error()
	}
	return st
}

// noFrameError 构造「一直没有可用帧」的错误，并区分两种截然不同的原因。
//
// 这是本驱动最容易写错、也最影响现场排查的一处：Reader 会把噪声与解析失败都
// 跳过继续扫描，最后报出来的总是「超时」。但：
//
//   - 一个字节都没读到 → 链路/接线/仪表没开连续输出，归 ErrTimeout；
//   - 读到了字节但一帧都没成 → 帧格式或参数不对，归 ErrProtocol。
//
// 混为一谈时，现场会先去改波特率，而不是去查仪表的帧格式设置。
func (c *conn) noFrameError(op string) error {
	c.mu.Lock()
	bytesSeen := c.bytesSeen
	badFrames := c.badFrames
	lastErr := c.lastParseErr
	c.mu.Unlock()

	if bytesSeen > 0 {
		msg := fmt.Sprintf("收到 %d 字节但无一成帧（坏帧 %d）", bytesSeen, badFrames)
		if lastErr != nil {
			msg += "：" + lastErr.Error()
		} else {
			msg += "：检查仪表帧格式（应为 ASCII 连续输出 ST,GS,+00149.0kg）、分隔符与波特率/校验位"
		}
		return driver.E(driver.ErrProtocol, op, c.deviceID, msg, false, lastErr)
	}
	return nil
}

// Read 从最新一帧读数里取各点位的字段值。无任何网络交互。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	closed := c.closed
	linkErr := c.linkErr
	reading := c.latest
	latestAt := c.latestAt
	hasData := c.hasData
	c.mu.Unlock()

	if closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.deviceID, "connection closed", true, nil)
	}
	if linkErr != nil {
		return nil, driver.E(driver.ErrDisconnected, "read", c.deviceID,
			"link read error", true, linkErr)
	}

	refs := flattenRefs(spans)
	if len(refs) == 0 {
		return nil, nil
	}

	// 一帧都没收到时，把「链路上没字节」与「有字节但解不出」分开报。
	// 这是本驱动唯一会在 Read 里返回错误的分支，其余一律按点位降级。
	if !hasData {
		if err := c.noFrameError("read"); err != nil {
			return nil, err
		}
	}

	now := time.Now()
	stale := hasData && now.Sub(latestAt) > c.staleAfter

	out := make([]driver.RawSample, 0, len(refs))
	for _, ref := range refs {
		ps := c.points[ref.ID]
		if ps == nil {
			out = append(out, driver.RawSample{
				PointID: ref.ID,
				Quality: model.QualityAddressError,
				Err: driver.E(driver.ErrAddress, "read", c.deviceID,
					fmt.Sprintf("point %q not configured", ref.ID), false, nil),
			})
			continue
		}
		out = append(out, c.sampleFor(ps, reading, hasData, stale))
	}
	return out, nil
}

// sampleFor 按点位字段从一帧读数里取值并定质量码。
func (c *conn) sampleFor(ps *pointState, reading toledocodec.Reading, hasData, stale bool) driver.RawSample {
	// 从没有过帧：comm_error 语义（还没见过这设备）。
	if !hasData {
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.Value{Type: ps.dt},
			Quality: model.QualityCommError,
			Err: driver.E(driver.ErrTimeout, "read", c.deviceID,
				"no frame received yet", false, nil),
		}
	}
	// 收过但陈旧。
	if stale {
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.Value{Type: ps.dt},
			Quality: model.QualityStale,
		}
	}

	switch ps.symbol {
	case "weight":
		return c.weightSample(ps, reading)
	case "status":
		return textSample(ps, string(reading.Status), model.QualityGood)
	case "unit":
		return textSample(ps, string(reading.Unit), model.QualityGood)
	case "mode":
		return textSample(ps, string(reading.Mode), model.QualityGood)
	case "decimals":
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.NumValue(model.TypeInt64, float64(reading.Decimals)),
			Quality: model.QualityGood,
		}
	case "stable":
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.BoolValue(reading.Status == toledocodec.StatusStable),
			Quality: model.QualityGood,
		}
	case "motion":
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.BoolValue(reading.Status == toledocodec.StatusMotion),
			Quality: model.QualityGood,
		}
	}
	// 已知 symbol 一定在上面处理过；这里兜底，不应到达。
	return driver.RawSample{
		PointID: ps.id,
		Value:   model.Value{Type: ps.dt},
		Quality: model.QualityBad,
	}
}

// weightSample 按称量状态给重量点位定质量码。
//
// 只有重量类点位随称量状态降级；status/unit 等描述性字段不降级（见包注释）。
func (c *conn) weightSample(ps *pointState, reading toledocodec.Reading) driver.RawSample {
	switch {
	case reading.Masked:
		// 数值段是掩码占位：衡器在表达「出错」，重量无意义。
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.Value{Type: ps.dt},
			Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.deviceID,
				"weight masked (scale error / overload placeholder)", false, nil),
		}
	case reading.Status == toledocodec.StatusOverload || reading.Status == toledocodec.StatusUnderload:
		// 超/欠量程：数值越界，不可信。
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.NumValue(ps.dt, reading.Weight),
			Quality: model.QualityBad,
		}
	case reading.Status == toledocodec.StatusMotion:
		// 运动中：数值在跳动，未稳定，标 uncertain。
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.NumValue(ps.dt, reading.Weight),
			Quality: model.QualityUncertain,
		}
	default:
		return driver.RawSample{
			PointID: ps.id,
			Value:   model.NumValue(ps.dt, reading.Weight),
			Quality: model.QualityGood,
		}
	}
}

// textSample 组一个文本点位样本。
func textSample(ps *pointState, s string, q model.Quality) driver.RawSample {
	return driver.RawSample{
		PointID: ps.id,
		Value:   model.TextValue(model.TypeString, s),
		Quality: q,
	}
}

// Probe 验证链路：连上后等一个连续输出帧。
//
// 推送型协议没有「握手」可探，能做的只是确认「一段时间内能收到帧」。用 ctx 的
// 截止 + 首帧到达双条件，谁先到谁返回。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "connection closed", true, nil)
	}
	deadline := time.NewTimer(defaultProbeTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		has := c.hasData
		c.mu.Unlock()
		if has {
			return nil
		}
		select {
		case <-ctx.Done():
			return c.probeTimeout(ctx.Err())
		case <-deadline.C:
			return c.probeTimeout(nil)
		case <-ticker.C:
		}
	}
}

// probeTimeout 构造探测失败的错误，并复用 noFrameError 的两种原因判据。
//
// 探测窗口内没帧时，「读到过字节但解不出」不该报成超时——那会让现场去查接线，
// 而真正要改的是仪表的帧格式。
func (c *conn) probeTimeout(cause error) error {
	if err := c.noFrameError("probe"); err != nil {
		return err
	}
	if cause != nil {
		return driver.E(driver.ErrTimeout, "probe", c.deviceID,
			"no frame within probe window", true, cause)
	}
	return driver.E(driver.ErrTimeout, "probe", c.deviceID,
		"no frame received within "+defaultProbeTimeout.String(), false, nil)
}

// Write 不支持：衡器只读。
//
// 按仓库约定逐项返回 ErrUnsupported，而不是返回一个顶层错误：顶层错误会被
// 采集层摊成「全部点位写失败 + comm_error」，看起来像链路故障；逐项返回能让
// 调用方看到「这个协议就是不支持写」。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, driver.WriteResult{
			PointID: req.PointID,
			Quality: model.QualityBad,
			Err: driver.E(driver.ErrUnsupported, "write", c.deviceID,
				"Toledo 连续输出是只读推送，驱动不支持写入", false, nil),
		})
	}
	return out, nil
}

// Close 停止收帧并关链路。
func (c *conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	// 关链路会让阻塞在 ReadFull 的收帧循环立刻返回错误，从而退出。
	return c.link.Close()
}

func (c *conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *conn) setLinkErr(err error) {
	c.mu.Lock()
	if !c.closed {
		c.linkErr = err
	}
	c.mu.Unlock()
}

// Symbol 是一个读数字段的定义：名字 + 默认数据类型。
//
// 字段名单此前散在四处（knownSymbol / defaultDataType / sampleFor 的分支 /
// 自检命令的 fields 切片与类型映射），任何一处改了别处就得跟着改，否则自检
// 会「认得一个驱动读不到的字段」。现在唯一事实源是 Symbols()：knownSymbol、
// defaultDataType 与自检命令都从它派生，sampleFor 仍按 symbol 取值但只处理
// 这张表里的字段。
type Symbol struct {
	// Name 是点表里 symbol 地址用的字段名（小写）。
	Name string
	// Type 是该字段的默认数据类型。
	Type model.DataType
}

// symbols 是全部受支持的读数字段，顺序即自检命令「默认读哪些字段」的顺序。
//
// 顺序刻意把重量放在最前：自检默认输出时人第一眼看到的就是它。新增字段只需
// 在这里加一行，knownSymbol/defaultDataType/自检命令会自动跟上。
var symbols = []Symbol{
	{Name: "weight", Type: model.TypeFloat64},
	{Name: "status", Type: model.TypeString},
	{Name: "mode", Type: model.TypeString},
	{Name: "unit", Type: model.TypeString},
	{Name: "stable", Type: model.TypeBool},
	{Name: "motion", Type: model.TypeBool},
	{Name: "decimals", Type: model.TypeInt64},
}

// Symbols 返回全部读数字段的定义（名字 + 默认类型）。
//
// 这是本驱动读数字段的唯一事实源：Open 校验字段名、按字段定类型、以及自检命令
// 要读哪些字段、各是什么类型，都从这里取。返回的是内部表的直接引用（只读使用），
// 调用方不应修改它。
func Symbols() []Symbol { return symbols }

// SymbolsOf 返回全部读数字段名的有序列表（不含类型）。
//
// 自检命令合成「默认读哪些字段」的 PointRef 时用，避免它自己再维护一份名单。
func SymbolsOf() []string {
	out := make([]string, len(symbols))
	for i, s := range symbols {
		out[i] = s.Name
	}
	return out
}

// knownSymbol 判断是否是受支持的读数字段。
func knownSymbol(s string) bool {
	return symbolType(s) != ""
}

// symbolType 返回字段的默认类型；不认识的字段返回空串。
//
// 空串既表示「不是字段」，也借 model.DataType 的零值同时表达「类型未知」——
// 两者对「未知字段」是一回事：Open 会在建链前用 KnownSymbol 拦掉。
func symbolType(s string) model.DataType {
	for _, sym := range symbols {
		if sym.Name == s {
			return sym.Type
		}
	}
	return ""
}

// SymbolType 返回字段的默认数据类型；不认识的字段返回空串。
//
// 自检命令合成点位时用它取类型，与驱动 Open 走同一张表（defaultDataType
// 也委托这里），保证「自检给的类型」和「驱动认的类型」永远不会分叉。
func SymbolType(s string) model.DataType { return symbolType(s) }

// KnownSymbol 是 knownSymbol 的导出版本，供自检命令在连秤之前校验字段名。
func KnownSymbol(s string) bool { return knownSymbol(s) }

// defaultDataType 按字段给默认数据类型（委托给唯一事实源 symbolType）。
func defaultDataType(sym string) model.DataType {
	return symbolType(sym)
}

// parseOptions 解析 labels.options。
//
// 与其它表计驱动一致，options 是一个「key=value,key=value」逗号分隔的字符串，
// 例如 `"staleAfter=30s,readTimeout=10s"`。未知键与解析失败的值一律忽略并保留
// 默认——配置写错不该让建链失败。
func parseOptions(dev model.Device) Options {
	opt := Options{}
	raw := dev.Labels["options"]
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if !hasVal {
			continue
		}
		switch key {
		case "staleafter":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				opt.StaleAfter = d
			}
		case "readtimeout":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				opt.ReadTimeout = d
			}
		}
	}
	return opt
}

// staleAfterFor 由设备采集周期推导新鲜度阈值。
//
// 3 倍周期是经验值：小于 2 倍时一次抖动就把整页点位判 stale；大于 5 倍时设备真
// 停了要等太久才告警。连续输出通常远快于采集周期，故取兜底与「3 倍周期」的较大者，
// 但采集周期很短时（连续输出场景常见）以兜底为准，避免过于敏感。
func staleAfterFor(dev model.Device, opt Options) time.Duration {
	if opt.StaleAfter > 0 {
		return opt.StaleAfter
	}
	d := dev.Interval.Duration()
	if d <= 0 {
		return defaultStaleAfter
	}
	derived := 3 * d
	if derived < defaultStaleAfter {
		return defaultStaleAfter
	}
	return derived
}

// flattenRefs 把 ReadSpan 拍平成点位引用列表。
func flattenRefs(spans []driver.ReadSpan) []driver.PointRef {
	var refs []driver.PointRef
	for _, sp := range spans {
		refs = append(refs, sp.Points...)
	}
	return refs
}

// isTimeout 判断错误是否是读超时（含底层 i/o timeout 与 ctx 取消）。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
