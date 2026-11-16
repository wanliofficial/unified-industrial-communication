// Package iec61850 实现 IEC 61850（MMS over TCP）主站驱动：关联建立后的
// 变量读写与模型浏览。
//
// 分层与其它驱动一致：Meta（能力声明）→ 本文件的三段建链（COTP CR/CC →
// 会话 CN/AC → 表示 CP/CPA → ACSE AARQ/AARE → MMS Initiate）→ codec/iec61850
// （TPKT/COTP/会话/表示/ACSE/MMS 的纯函数编解码）→ transport（TCP 字节流）。
//
// 五个刻意的边界（与 KNX/S7 驱动同样的克制）：
//
//  1. **只做 MMS 应用子集（读/写变量、模型浏览），不做 GOOSE/SV、报告控制块
//     （BRCB/URCB）、文件传输、定值组、数据集读写、TLS。** 这些是独立的大
//     子系统：GOOSE/SV 是二层/三层组播，走的是另一套生命周期；报告控制块
//     是「服务端主动推」的模型，与本平台调度式采集是两种语义。混进一个采集
//     驱动里，会让超时、重连、死区三套逻辑互相干扰。
//
//  2. **值的多宽由数据对象决定，点表不写 length。** IEC 61850 的对象引用
//     （`LD/LN.FC.DO[.DA]`）只回答「读哪个变量」，不回答「读几个字节」：一个
//     变量在 MMS 层有固定的类型（布尔、整型、浮点，或结构体的某个叶子）。
//     这与 KNX 的 DPT 不同——KNX 的类型信息不在总线上、必须由工程组态工具
//     提供；IEC 61850 的类型在服务端，读回来才知道。因此地址策略是 WidthNone
//     且不带类型后缀。
//
//  3. **对象引用里的 FC 必须显式给出，或由 device.labels.defaultFC 补齐。**
//     MMS 的项名是 `LN$FC$DO$DA`，FC 是名字的一段而不是元数据。少了它，
//     `GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal` 是两个不同对象，只有一个存在。
//     默认值来自设备标签，绝不从数据属性名反推——反推需要完整 SCL 模型，
//     而驱动手上只有一份点表。
//
//  4. **invokeID 严格配对。** 一条关联上可以同时有多个在途请求，靠 invokeID
//     把响应分回对应的请求。不校验 invokeID 就接受第一个响应，会在「前一次
//     请求超时后迟到的响应」到达时把 A 的值写给 B。迟到的响应按 invokeID
//     丢弃，不当作当前请求的答案。
//
//  5. **串行化整条关联。** MMS 是请求/应答协议，一条 TCP 上一个关联同时只有
//     一个在途事务。并发发两个读会让 invokeID 交叉；与总线带宽无关，这是
//     协议本身的要求。因此整条会话用一把锁串起来。
package iec61850

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	ieccodec "uic/internal/codec/iec61850"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 抽象 TCP 建链，测试可注入内存字节流。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// 设备级默认值。
const (
	defaultTimeout   = 3 * time.Second
	minTimeout       = 200 * time.Millisecond
	maxTimeout       = 30 * time.Second
	disconnectWait   = 500 * time.Millisecond
	probeReadTimeout = 2 * time.Second
	// maxBrowsePages 是 getNameList 翻页的上限。没有它，一个「永远说还有
	// 下一页、游标却不前进」的对端会把自检命令变成死循环。
	maxBrowsePages = 256
)

// Driver 是 IEC 61850（MMS）主站驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return transport.DialTCP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	return &Driver{dial: d}
}

// Meta 声明 IEC 61850 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "iec61850",
		Name:         "IEC 61850 (MMS)",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:  driver.CapSupported,
			Write: driver.CapSupported,
			// 浏览（getNameList + getVariableAccessAttributes）协议支持，
			// 但那是一次性枚举整棵 IED 模型的独立操作，与调度式采集是两种
			// 模型。能力声明为 unsupported 以避免误用；自检命令走
			// 包级的 Browse 函数（见文件末尾），不经过能力位。
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "IED 地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:102", Scope: "channel",
				Help: "IEC 61850 IED 的 MMS 地址（ISO-on-TCP）；省略端口时按 102 处理"},
			{Name: "labels.defaultFC", Label: "默认功能约束", Type: "string",
				Scope: "device", Placeholder: "ST",
				Help: "对象引用省略 FC 时补齐的功能约束（如 ST/MX/SP）；引用里显式写了 FC 则此值被忽略"},
			{Name: "labels.probeObject", Label: "探针对象", Type: "string",
				Scope: "device", Placeholder: "LD0/LLN0.ST.Health",
				Help: "Probe 读取的对象引用（写法 LD/LN.FC.DO[.DA]）；留空表示 Probe 只建立关联不读值"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"symbol"}, Scope: "point"},
			{Name: "address.path", Label: "对象引用", Type: "string", Required: true,
				Scope: "point", Placeholder: "simpleIO/GGIO1.ST.Ind1.stVal",
				Help: "逻辑设备/逻辑节点.功能约束.数据对象[.数据属性]，如 simpleIO/GGIO1.ST.Ind1.stVal"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int8", "int16", "int32", "int64",
					"uint8", "uint16", "uint32", "uint64",
					"float32", "float64", "string", "bytes"},
				Scope: "point",
				Help:  "只声明平台侧怎么存；线宽与取值范围由数据对象的 MMS 类型决定"},
		},
		Description: "IEC 61850 (MMS over TCP 102) 主站：对象引用读写与模型浏览",
	}
}

// settings 是设备级可调参数。
type settings struct {
	timeout     time.Duration
	defaultFC   string
	probeObject string
}

func parseSettings(cfg driver.OpenConfig) (settings, error) {
	st := settings{
		timeout:     defaultTimeout,
		defaultFC:   strings.TrimSpace(cfg.Device.Labels["defaultFC"]),
		probeObject: strings.TrimSpace(cfg.Device.Labels["probeObject"]),
	}
	if st.defaultFC != "" {
		if !ieccodec.IsValidFC(st.defaultFC) {
			return st, fmt.Errorf("labels.defaultFC %q is not a functional constraint (want ST/MX/CO/SP/CF/DC/SG/SE)", st.defaultFC)
		}
		st.defaultFC = strings.ToUpper(st.defaultFC)
	}
	if v := strings.TrimSpace(cfg.Device.Labels["timeout"]); v != "" {
		dur, err := time.ParseDuration(v)
		if err != nil {
			return st, fmt.Errorf("labels.timeout %q: %w", v, err)
		}
		if dur < minTimeout || dur > maxTimeout {
			return st, fmt.Errorf("labels.timeout %s must be within %s..%s", v, minTimeout, maxTimeout)
		}
		if dur > cfg.Device.TimeoutOrDefault() {
			return st, fmt.Errorf("labels.timeout %s exceeds device.timeout %s; raise device.timeout or lower timeout",
				dur, cfg.Device.TimeoutOrDefault())
		}
		st.timeout = dur
	} else if t := cfg.Device.TimeoutOrDefault(); t > 0 {
		st.timeout = t
	}
	return st, nil
}

// pointPlan 是一个点位解析后的对象引用。
type pointPlan struct {
	ref  driver.PointRef
	oref ieccodec.ObjectRef
}

// Open 解析点位并建立 TCP 套接字。
//
// 关联建立（COTP/会话/表示/ACSE/MMS 四段握手）不在这里做：Open 只有一次机会，
// 而 IED 的 MMS 服务端口往往同时只接受有限的关联数，且握手比一条读写往返慢
// 一个数量级。真正的关联由第一次 Probe/Read/Write 触发，失败可以重试。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("iec61850 requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr, err := hostPort(t.Address)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, "invalid transport address", false, err)
	}
	st, err := parseSettings(cfg)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, "invalid device settings", false, err)
	}
	plans, byID, err := buildPlans(cfg.Device, st.defaultFC)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID, "invalid point table", false, err)
	}
	link, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	return &conn{
		devID:   cfg.Device.ID,
		addr:    addr,
		dial:    d.dial,
		link:    link,
		timeout: st.timeout,
		st:      st,
		log:     cfg.Log,
		points:  plans,
		byID:    byID,
	}, nil
}

func buildPlans(dev model.Device, defaultFC string) ([]pointPlan, map[string]pointPlan, error) {
	plans := make([]pointPlan, 0, len(dev.Points))
	byID := make(map[string]pointPlan, len(dev.Points))
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		plan, err := planPoint(driver.PointRefOf(dev, p), defaultFC)
		if err != nil {
			return nil, nil, err
		}
		if prev, dup := byID[plan.ref.ID]; dup {
			return nil, nil, fmt.Errorf("point %q: duplicate id (also used by %s)", plan.ref.ID, prev.oref)
		}
		plans = append(plans, plan)
		byID[plan.ref.ID] = plan
	}
	return plans, byID, nil
}

func planPoint(ref driver.PointRef, defaultFC string) (pointPlan, error) {
	if ref.Address.Kind != model.AddressSymbol {
		return pointPlan{}, fmt.Errorf("point %q: iec61850 requires symbol addressing (an object reference such as LD/LN.FC.DO), got %q",
			ref.ID, ref.Address.Kind)
	}
	oref, err := ieccodec.ParseObjectRef(ref.Address.Path, defaultFC)
	if err != nil {
		return pointPlan{}, fmt.Errorf("point %q: %w", ref.ID, err)
	}
	// Selector/Index/Length 在 IEC 61850 的对象引用里没有对应概念：它们是 MQTT
	// 载荷字段与数组下标/宽度的表达。对象引用已经把层级写全，再带这些只会让
	// 点表从别的协议抄过来而不自知。
	if ref.Address.Selector != "" {
		return pointPlan{}, fmt.Errorf("point %q: address.selector %q has no meaning in iec61850 (the object reference is fully qualified)",
			ref.ID, ref.Address.Selector)
	}
	// 只拦「明确写了数组下标」的情况：JSON 里省略 index 时零值就是 0，而
	// model.Address 把 `Index >= 0` 当作「有下标」。用 `> 0` 与 KNX/ADS 保持
	// 一致——否则每一份没写 index 的点表都会在 Open 阶段被判成地址错（这是
	// 端到端回环里真的踩到的一个缺陷）。
	if ref.Address.Index > 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.index %d has no meaning in iec61850 (array indices belong inside the object reference)",
			ref.ID, ref.Address.Index)
	}
	if ref.Address.Length > 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.length %d: the value width comes from the data object's MMS type",
			ref.ID, ref.Address.Length)
	}
	if !ref.DataType.Valid() || ref.DataType == "" {
		return pointPlan{}, fmt.Errorf("point %q: invalid dataType %q", ref.ID, ref.DataType)
	}
	return pointPlan{ref: ref, oref: oref}, nil
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

type conn struct {
	devID   string
	addr    string
	timeout time.Duration
	dial    Dialer
	st      settings
	log     *slog.Logger

	// mu 串行化整条关联（见包注释第 5 条）。它与链路读写共用一把锁：MMS
	// 是请求/应答协议，没有「两条流水线」的并发收益，反而有 invokeID 交叉
	// 的风险。
	mu     sync.Mutex
	link   transport.Stream
	closed bool

	// 关联状态。
	associated bool
	invokeID   uint32

	points []pointPlan
	byID   map[string]pointPlan

	// badFrames 记录解析失败的帧数，供诊断观察。它不影响事务：
	// TCP 上会收到别人的残留包、被截断的包，把它们变成当前请求的失败会让
	// 「网上有别人」成为本设备的故障。
	badFrames uint64
}

// Probe 建立关联并读一次 LLN0 通用节点属性，验证 IED 真的在 MMS 层响应。
//
// 探针对象留空时退化为「只建立关联」：关联本身已经回答了「TCP 通、MMS 服务
// 在跑」，这对很多只需要确认链路活着的场景已经足够；读一个具体对象失败则
// 可能是对象名配错，不必然代表链路坏。默认读 LLN0$ST$Health（所有 IED 都有
// 这个通用节点健康指示，且 ST 是标准功能约束），读不到时降级为仅关联成功。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", true, nil)
	}
	if err := c.ensure(ctx); err != nil {
		return err
	}
	if c.st.probeObject == "" {
		return nil
	}
	oref, err := ieccodec.ParseObjectRef(c.st.probeObject, c.st.defaultFC)
	if err != nil {
		// 探针对象配错不是链路故障：记录并降级为仅关联成功，避免一个
		// 标签拼写错误把整台 IED 判成离线。
		c.debug("probe object %q is not a valid reference: %v; treating probe as association-only", c.st.probeObject, err)
		return nil
	}
	_, err = c.readObject(ctx, oref, "probe")
	if err != nil {
		c.debug("probe read of %s failed: %v; treating probe as association-only", oref, err)
	}
	return nil
}

// Browse 枚举一个逻辑设备下的命名变量（MMS getNameList），按页收齐。
//
// 它刻意不进入 driver.Conn 契约：平台把 Browse 能力位声明为 unsupported，
// 因为调度式采集按点表读，不需要枚举整棵模型；而「枚举一次」是运维/自检
// 动作，用完即走。把这种一次性枚举混进 Read 会让采集侧多一条难以解释的
// 慢路径。domain 为空表示整个 VMD。
func (c *conn) Browse(ctx context.Context, domain string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "browse", c.devID, "connection closed", true, nil)
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	return c.listNames(ctx, domain)
}

// listNames 逐页调用 getNameList，直到对端说没有下一页。
//
// 翻页靠「上一页最后一项」做 continueAfter：MMS 没有偏移量，只有字典序游标。
// 上限 maxBrowsePages 防的是「永远说还有、游标却不前进」的畸形对端。
func (c *conn) listNames(ctx context.Context, domain string) ([]string, error) {
	out := make([]string, 0, 16)
	cursor := ""
	for page := 0; page < maxBrowsePages; page++ {
		c.invokeID++
		req := ieccodec.BuildGetNameListRequest(c.invokeID, ieccodec.GetNameListRequest{
			Class:         ieccodec.ObjectClassNamedVariable,
			Domain:        domain,
			ContinueAfter: cursor,
		})
		mms, err := c.roundTrip(ctx, "browse", req)
		if err != nil {
			return nil, err
		}
		resp, err := ieccodec.ParseGetNameListResponse(mms)
		if err != nil {
			return nil, c.classifyMMS(err, "browse", domain)
		}
		out = append(out, resp.Identifiers...)
		if !resp.MoreFollows {
			return out, nil
		}
		// 对端说「还有」却回了空页：游标无从推进，再问只会空转。
		if len(resp.Identifiers) == 0 {
			return out, nil
		}
		cursor = resp.Identifiers[len(resp.Identifiers)-1]
	}
	return nil, driver.E(driver.ErrProtocol, "browse", c.devID,
		fmt.Sprintf("getNameList did not terminate after %d pages", maxBrowsePages), false, nil)
}

// Browse 建一条独立关联并枚举一个逻辑设备下的命名变量，用完即关。
//
// 这是给自检命令用的包级入口，不是 driver.Conn 的一部分：它与能力位
// （Browse=unsupported）不矛盾——能力位描述的是采集路径，这里是显式的一次性
// 枚举。独立建链而不是复用采集连接，避免枚举期间把采集的 invokeID 搅乱。
func Browse(ctx context.Context, cfg driver.OpenConfig, domain string) ([]string, error) {
	c, err := New().Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ic, ok := c.(*conn)
	if !ok {
		return nil, driver.E(driver.ErrInternal, "browse", cfg.Device.ID, "unexpected connection type", false, nil)
	}
	return ic.Browse(ctx, domain)
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.associated {
		c.release()
	}
	link := c.link
	c.link = nil
	if link != nil {
		return link.Close()
	}
	return nil
}

// ensure 保证关联可用：必要时建 TCP、建四段关联。
func (c *conn) ensure(ctx context.Context) error {
	if c.closed {
		return driver.E(driver.ErrDisconnected, "open", c.devID, "connection closed", true, nil)
	}
	if c.link == nil {
		if err := c.redial(ctx); err != nil {
			return err
		}
	}
	if !c.associated {
		return c.associate(ctx)
	}
	return nil
}

func (c *conn) redial(ctx context.Context) error {
	link, err := c.dial(ctx, c.addr, c.timeout)
	if err != nil {
		return driver.E(driver.ErrDisconnected, "dial", c.devID,
			fmt.Sprintf("connect %s failed", c.addr), true, err)
	}
	c.link = link
	c.associated = false
	return nil
}

// associate 完成四段握手：COTP CR/CC → 会话 CN/AC → 表示 CP/CPA → ACSE AARQ/AARE → MMS Initiate。
//
// 每一段的封套层级不同，这是本协议最容易写错的地方，逐段钉死：
//
//   - COTP CR：COTPConnect() 自己就带 TPKT 头，是一整帧，直接发；
//   - 会话 CN：BuildConnectPDU 返回的是会话层 SPDU（不含 TPKT），要用
//     BuildCOTPData 包成「TPKT(COTP-DT(会话 CN))」再发；
//   - COTP CC、会话 AC：都是完整 TPKT 帧，用 readFrame 读整帧再按类型拆。
func (c *conn) associate(ctx context.Context) error {
	c.invokeID = 0

	// 1. COTP 连接请求：COTPConnect() 已含 TPKT 头，直接发。
	if err := c.sendRaw(ieccodec.COTPConnect(), "associate"); err != nil {
		return err
	}
	// 等 COTP CC。
	if err := c.awaitCOTPConfirm(ctx); err != nil {
		return err
	}

	// 2..4. 会话 CN/AC + 表示 CP/CPA + ACSE AARQ/AARE 合成一次 BuildConnectPDU。
	init := ieccodec.DefaultInitiateRequest()
	connect, err := ieccodec.BuildConnectPDU(init, nil)
	if err != nil {
		return driver.E(driver.ErrProtocol, "associate", c.devID, "build connect PDU failed", false, err)
	}
	// connect 是会话层 SPDU（不含 TPKT）：包成 COTP DT 再发。
	if err := c.sendSession(connect, "associate"); err != nil {
		return err
	}
	// 读一段会话 AC + 表示 CPA + ACSE AARE + Initiate 响应，它们被包在同
	// 一个会话 AC 的 COTP DT 里返回。
	if err := c.awaitAssociation(ctx); err != nil {
		return err
	}

	c.associated = true
	c.debug("associated with %s", c.addr)
	return nil
}

// awaitCOTPConfirm 读并校验 COTP CC（完整 TPKT 帧）。
func (c *conn) awaitCOTPConfirm(ctx context.Context) error {
	deadline := c.deadline(ctx)
	if err := c.setDeadline(deadline); err != nil {
		return c.linkError("associate", "set deadline", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return driver.E(driver.ErrTimeout, "associate", c.devID, "context done", true, err)
		}
		frame, err := c.readFrame(ctx, "cotp")
		if err != nil {
			return err
		}
		if ieccodec.IsCOTPDisconnect(frame) {
			return driver.E(driver.ErrDisconnected, "associate", c.devID,
				"IED sent COTP disconnect during association", true, nil)
		}
		if _, err := ieccodec.ParseCOTPConfirm(frame); err != nil {
			// 不是 CC 也不是 DR：握手阶段不该收到别的 COTP，继续等真正的 CC。
			c.badFrames++
			continue
		}
		return nil
	}
}

// awaitAssociation 读会话 AC，拆出表示 CPA、ACSE AARE 与 Initiate 响应。
//
// 关联响应是「TPKT(COTP-DT(会话 AC { 表示 CPA { ACSE AARE { MMS Initiate-response } } }))」，
// 先剥 TPKT/COTP 得会话层内容，再交给 ParseConnectResponse 逐层拆。
func (c *conn) awaitAssociation(ctx context.Context) error {
	deadline := c.deadline(ctx)
	if err := c.setDeadline(deadline); err != nil {
		return c.linkError("associate", "set deadline", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return driver.E(driver.ErrTimeout, "associate", c.devID, "context done", true, err)
		}
		frame, err := c.readFrame(ctx, "session")
		if err != nil {
			return err
		}
		if ieccodec.IsCOTPDisconnect(frame) {
			return driver.E(driver.ErrDisconnected, "associate", c.devID,
				"IED sent COTP disconnect during association", true, nil)
		}
		session, _, err := ieccodec.ParseCOTPData(frame)
		if err != nil {
			// 关联响应必须走 COTP DT；CC 之类不该在这里出现。
			c.badFrames++
			continue
		}
		aare, _, perr := ieccodec.ParseConnectResponse(session)
		if perr != nil {
			return driver.E(driver.ErrProtocol, "associate", c.devID,
				"association response: "+perr.Error(), false, perr)
		}
		if aare.Result != ieccodec.ACSEResultAccepted {
			reason := ieccodec.ACSEReason(aare.Result, aare.Diagnostic)
			if reason == "" {
				reason = "association rejected"
			}
			return driver.E(driver.ErrProtocol, "associate", c.devID,
				"IED rejected the MMS association: "+reason, false, nil)
		}
		return nil
	}
}

// release 有序释放关联：会话 FN（ACSE RLRQ）。
func (c *conn) release() {
	rel, err := ieccodec.BuildReleasePDU()
	if err != nil {
		return
	}
	_ = c.sendSession(rel, "close")
	// 不等 RLRE 也照常关：IED 会自己超时回收关联，不值得为一个善后报文阻塞
	// 关闭。给一个很短的窗口吃掉它，让日志里能看到正常的释放往返。
	_ = c.setDeadline(time.Now().Add(disconnectWait))
	_, _ = c.readFrame(context.Background(), "close")
	c.associated = false
}

// ---------------------------------------------------------------------------
// 收发
// ---------------------------------------------------------------------------

// sendRaw 发一整帧（已含 TPKT 头，如 COTP CR/CC）。
func (c *conn) sendRaw(frame []byte, op string) error {
	if c.link == nil {
		return driver.E(driver.ErrDisconnected, op, c.devID, "link is down", true, nil)
	}
	if err := c.link.Write(frame); err != nil {
		c.dropLink()
		return c.linkError(op, "tcp write failed", err)
	}
	return nil
}

// sendSession 发一段会话层内容（CN/AC、DT、FIN）：包成 TPKT(COTP-DT(...))。
//
// 会话层 SPDU 自己不带 TPKT；对端也用 COTP DT 承载会话层，所以发送侧必须
// 补上 TPKT + COTP-DT 两层封套，读响应侧才能对称剥掉。
func (c *conn) sendSession(sessionPayload []byte, op string) error {
	return c.sendRaw(ieccodec.BuildCOTPData(sessionPayload), op)
}

// readFrame 读一个完整的 TPKT 帧（含 4 字节 TPKT 头），返回原始字节。
//
// 分帧靠 TPKT 头部的总长度：先读 4 字节头，再按长度读满剩余。返回的是带 TPKT
// 头的整帧，调用方用 ParseCOTPConfirm / ParseCOTPData / IsCOTPDisconnect 按
// COTP 类型再拆——它们各自会先剥 TPKT。
func (c *conn) readFrame(ctx context.Context, op string) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, err)
		}
		if c.link == nil {
			return nil, driver.E(driver.ErrDisconnected, op, c.devID, "link is down", true, nil)
		}
		head, err := c.link.ReadFull(4)
		if err != nil {
			if isTimeout(err) {
				return nil, driver.E(driver.ErrTimeout, op, c.devID, "no data within timeout", true, err)
			}
			c.dropLink()
			return nil, c.linkError(op, "tcp read failed", err)
		}
		total, err := ieccodec.TPKTLength(head)
		if err != nil {
			// 头不对：丢掉这一读，继续尝试（可能是别人的残留或粘包的前半段）。
			c.badFrames++
			continue
		}
		rest := total - 4
		var body []byte
		if rest > 0 {
			body, err = c.link.ReadFull(rest)
			if err != nil {
				if isTimeout(err) {
					return nil, driver.E(driver.ErrTimeout, op, c.devID, "no data within timeout", true, err)
				}
				c.dropLink()
				return nil, c.linkError(op, "tcp read failed", err)
			}
		}
		return append(head, body...), nil
	}
}

// roundTrip 发一个 MMS confirmed 请求，按 invokeID 配对等待响应。
//
// req 是 BuildReadRequest/BuildWriteRequest 已组装好的**完整** confirmed-request
// （含 invokeID 与服务体），这里不再包一层 confirmed-request——包两层会让对端按
// 错误的外层标签解码。但 MMS 不是直接塞进 COTP 的：它还要过表示层与会话 DT 两层，
// 这一步由 WrapData 完成。漏掉它，对端看到的是一段以 0xA1/0xA4 开头的裸 MMS，
// 会话层解析直接失败——表现是「关联成功，但每次读都被对端关连接」。
//
// invokeID 从 req 里取出，用于把迟到的响应分回对应的请求。
//
// 迟到响应（invokeID 与当前不符）按包注释第 4 条丢弃：继续等真正的响应，
// 而不是把上一次请求的迟到应答当成这次的结果。
func (c *conn) roundTrip(ctx context.Context, op string, req []byte) (mmsPayload []byte, err error) {
	id, _, err := ieccodec.ConfirmedInvokeID(req)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, op, c.devID, "request carries no invokeID", false, err)
	}
	// 包成「会话 DT { 表示 用户数据 { MMS } }」，再交给 sendSession 补 TPKT+COTP。
	dt, err := ieccodec.WrapData(req)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, op, c.devID, "wrap request failed", false, err)
	}
	if err := c.sendSession(dt, op); err != nil {
		return nil, err
	}
	deadline := c.deadline(ctx)
	if err := c.setDeadline(deadline); err != nil {
		return nil, c.linkError(op, "set deadline", err)
	}
	defer func() { _ = c.setDeadline(time.Time{}) }()

	for {
		if err := ctx.Err(); err != nil {
			return nil, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, err)
		}
		frame, rerr := c.readFrame(ctx, op)
		if rerr != nil {
			return nil, rerr
		}
		if ieccodec.IsCOTPDisconnect(frame) {
			return nil, driver.E(driver.ErrDisconnected, op, c.devID, "IED sent COTP disconnect", true, nil)
		}
		session, _, uerr := ieccodec.ParseCOTPData(frame)
		if uerr != nil {
			c.badFrames++
			continue
		}
		mms, uerr := ieccodec.UnwrapData(session)
		if uerr != nil {
			// 释放/中止类报文：链路正在被对端收尾。
			if rel := ieccodec.ParseRelease(session); rel != "" {
				c.associated = false
				return nil, driver.E(driver.ErrDisconnected, op, c.devID,
					"IED released the association ("+rel+")", true, nil)
			}
			c.badFrames++
			continue
		}
		// invokeID 配对。
		gotID, _, ierr := ieccodec.ConfirmedInvokeID(mms)
		if ierr != nil {
			c.badFrames++
			continue
		}
		if gotID != id {
			// 迟到响应：属于上一次（或更早的）请求，丢弃并继续等。
			continue
		}
		return mms, nil
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
	okSamples := 0
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			for _, rest := range refs[i:] {
				out = append(out, errSample(rest.ID, e))
			}
			// 链路级：不走下面的部分失败闸门。
			return out, e
		}
		plan, ok := c.byID[ref.ID]
		if !ok {
			var err error
			plan, err = planPoint(ref, c.st.defaultFC)
			if err != nil {
				e := driver.E(driver.ErrAddress, "read", c.devID, "invalid point reference", false, err)
				out = append(out, errSample(ref.ID, e))
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
		}
		v, err := c.readObject(ctx, plan.oref, "read")
		sample := driver.RawSample{PointID: ref.ID, Quality: model.QualityGood}
		if err != nil {
			sample.Quality = qualityFor(driver.KindOf(err))
			sample.Err = err
			if firstErr == nil {
				firstErr = err
			}
			out = append(out, sample)
			// 链路级失败时剩下的点不可能成功，继续逐个等超时只会把一轮
			// 采集拖成 N 倍时长。地址类错误不影响其它点，继续。
			if driver.IsLinkFailure(err) {
				for _, rest := range refs[i+1:] {
					out = append(out, errSample(rest.ID, err))
				}
				return out, err
			}
			continue
		}
		sample.Value = v
		okSamples++
		out = append(out, sample)
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误，
	// 否则点表里一个对象引用写错会让整台 IED 每周期断链重连。
	return out, driver.PartialReadError(okSamples, firstErr)
}

// readObject 读一个对象引用并转成平台值。
func (c *conn) readObject(ctx context.Context, oref ieccodec.ObjectRef, op string) (model.Value, error) {
	// 关联可能因上一次 release 或超时已失效；这里补建。
	if !c.associated {
		if err := c.associate(ctx); err != nil {
			return model.Value{}, err
		}
	}
	c.invokeID++
	req, err := ieccodec.BuildReadRequest(c.invokeID, ieccodec.ReadRequest{Names: []ieccodec.ObjectName{oref.ObjectName()}})
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, op, c.devID, "build read request failed", false, err)
	}
	mms, err := c.roundTrip(ctx, op, req)
	if err != nil {
		return model.Value{}, err
	}
	resp, err := ieccodec.ParseReadResponse(mms)
	if err != nil {
		return model.Value{}, c.classifyMMS(err, op, "")
	}
	if len(resp.Results) == 0 {
		return model.Value{}, driver.E(driver.ErrProtocol, op, c.devID, "read response carries no result", false, nil)
	}
	r := resp.Results[0]
	if !r.OK {
		return model.Value{}, c.dataError(op, r.Error, oref.String())
	}
	v, err := r.Data.ToValue()
	if err != nil {
		// 结构/数组整体：提示取叶子，而不是判成协议错。消息只说明「为什么
		// 这里读不出来」，具体原因留在 cause 里——两处都写会得到同一句话
		// 重复两遍的文案。
		return model.Value{}, driver.E(driver.ErrAddress, op, c.devID,
			fmt.Sprintf("%s: not a scalar value (read the leaf attribute instead, e.g. .mag.f)", oref), false, err)
	}
	return toModelValue(v), nil
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
		if err != nil && driver.IsRetryable(err) {
			for _, rest := range reqs[i+1:] {
				results = append(results, writeFail(rest, err))
			}
			break
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
	}, c.st.defaultFC)
	if err != nil {
		return fail(driver.ErrAddress, "invalid point reference", false, err)
	}
	if !c.associated {
		if err := c.associate(ctx); err != nil {
			return fail(driver.KindOf(err), "association is not available", driver.IsRetryable(err), err)
		}
	}
	data, err := ieccodec.DataFromValue(toCodecValue(req.Value))
	if err != nil {
		return fail(driver.ErrAddress, "value cannot be encoded for this object", false, err)
	}
	c.invokeID++
	reqMMS, err := ieccodec.BuildWriteRequest(c.invokeID, ieccodec.WriteRequest{
		Names:  []ieccodec.ObjectName{plan.oref.ObjectName()},
		Values: []ieccodec.Data{data},
	})
	if err != nil {
		return fail(driver.ErrProtocol, "build write request failed", false, err)
	}
	mms, err := c.roundTrip(ctx, "write", reqMMS)
	if err != nil {
		kind := driver.KindOf(err)
		if kind == driver.ErrTimeout {
			return fail(kind, "no response to the write (the IED may not have committed the value)", true, err)
		}
		return fail(kind, err.Error(), driver.IsRetryable(err), err)
	}
	resp, err := ieccodec.ParseWriteResponse(mms)
	if err != nil {
		return fail(driver.KindOf(err), err.Error(), driver.IsRetryable(err), err)
	}
	if len(resp.Results) == 0 {
		return fail(driver.ErrProtocol, "write response carries no result", false, nil)
	}
	if !resp.Results[0].OK {
		de := c.dataError("write", resp.Results[0].Error, plan.oref.String())
		return fail(driver.KindOf(de), de.Error(), driver.IsRetryable(de), de)
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// toCodecValue 把平台 model.Value 折成 codec 自己的 Value（字段一一对应，
// 只是类型不同：codec 不依赖 model）。
func toCodecValue(v model.Value) ieccodec.Value {
	return ieccodec.Value{Type: string(v.Type), Num: v.Num, Text: v.Text, Bool: v.Bool}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// classifyMMS 把编解码层的普通错误归入协议类。单项数据访问错误走 dataError。
func (c *conn) classifyMMS(err error, op, item string) error {
	return driver.E(driver.ErrProtocol, op, c.devID, "unexpected MMS payload", false, err)
}

// dataError 把一个 MMS 单项数据访问错误码翻译成带分类的驱动错误。
//
// 这些错误码不是网络错误，逐条映射才能让「点位配错了」和「装置正在重启」在
// 上层被分开处理：地址类错误不计入熔断、直接告诉运维改点表；临时类错误可重试。
func (c *conn) dataError(op string, code uint8, item string) error {
	// msg 已经把这个错误码和对象引用完整讲了一遍，所以 cause 必须留空：
	// driver.E 会把 cause 的文本再拼到消息尾部，非 nil 就会得到
	// 「... on X: ... on X」这种同一句话重复两遍的错误文案。
	msg := (&ieccodec.DataError{Code: code, Item: item}).Error()
	switch code {
	case ieccodec.DataErrObjectNonExistent, ieccodec.DataErrObjectUndefined,
		ieccodec.DataErrInvalidAddress, ieccodec.DataErrTypeUnsupported:
		// 对象不存在 / 未定义 / 地址无效 / 类型不支持：点表或服务端模型
		// 对不上，重试一万次也没用，判成地址错误。
		return driver.E(driver.ErrAddress, op, c.devID, msg, false, nil)
	case ieccodec.DataErrAccessDenied:
		return driver.E(driver.ErrPermission, op, c.devID, msg, false, nil)
	case ieccodec.DataErrTemporarilyUnavail, ieccodec.DataErrHardwareFault:
		// 暂时不可用 / 硬件故障：装置侧临时状态，值得重试。
		return driver.E(driver.ErrProtocol, op, c.devID, msg, true, nil)
	default:
		return driver.E(driver.ErrProtocol, op, c.devID, msg, false, nil)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func (c *conn) deadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		return dl
	}
	return d
}

func (c *conn) setDeadline(t time.Time) error {
	if ds, ok := c.link.(transport.DeadlineSetter); ok {
		return ds.SetDeadline(t)
	}
	return nil
}

func (c *conn) dropLink() {
	c.associated = false
	if c.link != nil {
		_ = c.link.Close()
		c.link = nil
	}
}

func (c *conn) linkError(op, msg string, err error) error {
	return driver.E(driver.ErrDisconnected, op, c.devID, msg, true, err)
}

func (c *conn) debug(msg string, args ...any) {
	if c.log != nil {
		c.log.Debug("iec61850: "+msg, args...)
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne interface{ Timeout() bool }
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

// toModelValue 把 codec 解出的 Go 值折成平台 model.Value。
//
// codec 侧 ToValue 返回 any（bool/int64/float64/string/[]byte）。这里按点位的
// dataType 归一，字符串/字节保留原文，其余走 AsFloat 语义。布尔已在 ToValue
// 里是 bool，用 BoolValue 保留。
func toModelValue(v any) model.Value {
	switch x := v.(type) {
	case bool:
		return model.BoolValue(x)
	case string:
		return model.TextValue(model.TypeString, x)
	case []byte:
		return model.BytesValue(x)
	case int64:
		return model.NumValue(model.TypeInt64, float64(x))
	case float64:
		return model.NumValue(model.TypeFloat64, x)
	default:
		return model.TextValue(model.TypeString, fmt.Sprintf("%v", x))
	}
}

// hostPort 规范化地址：省略端口时按 102 处理。
func hostPort(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("address is required")
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, nil
	}
	// 没有端口：可能是纯主机名或 IPv4/IPv6 地址。
	if strings.Contains(addr, ":") {
		// IPv6 无端口（形如 ::1 或 fe80::1）：补端口。
		return "[" + addr + "]:102", nil
	}
	return addr + ":102", nil
}
