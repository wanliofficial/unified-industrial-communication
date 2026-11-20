// Package dlms 实现 IEC 62056 DLMS/COSEM 主站驱动。
//
// 分层与仓库其它表计驱动一致：Meta（能力声明）→ conn（事务/建链/错误分类）
// → codec/dlms（HDLC 帧 + APDU + A-XDR 纯函数编解码）→ transport。
//
// # 定位：LN 引用 + 无安全 + HDLC 三层承载
//
// 这是 DLMS 能做到"离线写出点表"的唯一一条路径。理由逐条：
//
//   - **LN（logical name）引用**：地址是 6 字节 OBIS 码，含义由标准与
//     companion specification 固定（`1.0.1.8.0.255` 永远是正向有功电能）。
//     点表因此可以离线书写、跨表复用。
//   - **SN（short name）引用不做**：short name 是厂商在 object_list 里
//     分配的，同一个 OBIS 在不同表上 short name 不同。要用它必须先读
//     `object_list` 建立映射，属于另一套"先发现再寻址"的模型，与"点表
//     直接写地址"的采集方式不兼容。
//   - **无安全（Lowest Level Security）不做认证**：HLS/GMAC 需要密钥协商、
//     帧计数器、AES-GCM，且密钥分发方式各厂商完全不同。做了也只能覆盖
//     一部分现场，却会把"密码错"与"地址错"混成同一类超时。
//   - **HDLC 三层承载**：这是串口/RS-485 上最常见的形态。TCP/UDP 的 COSEM
//     wrapper 是另一套封装（version + wrapper 头），不在这里。
//
// # 不做 Set / Action / EventNotification
//
// 写方向（SET）、方法调用（ACTION）与服务端主动上报（EventNotification）
// 各自是另一套状态机：SET 要处理 selective access 与分块写，ACTION 要处理
// 方法参数与方法返回，EventNotification 是服务端主动发起的无编号帧——它们
// 不是"GET 换个方向"，半成品比不做更难排查。因此 Write 明确声明不支持。
//
// # 为什么 MultiRead 是 unsupported
//
// 一条 Get-Request 只能读**一个**属性。标准里的批量机制是
// `get-request-with-list`，但它要求服务端在 conformance 里声明
// `multiple-references`，且各属性必须共享同一个 class；实际现场极少支持。
// 更常见的替代是"读 object_list 再一次取全部"，那是按对象表批量，属于
// 另一套机制。因此本驱动逐点发请求，并如实声明 MultiRead 不支持——把
// 「N 个点 = N 次往返」这个事实暴露给配置的人，比假装合并更负责任。
//
// # 建链顺序（每一步失败的症状都不同）
//
//	SNRM  → 等 UA     物理层/波特率/地址错时这里就没有字节
//	AARQ  → 等 AARE   应用上下文/版本/conformance 不匹配时这里被拒
//	GET   → 等 GET-Rsp OBIS/类号/属性号错时这里返回 Data-Access-Result
//	DISC  → 等 UA     断链失败不影响数据，只记日志
package dlms

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	dlmscodec "uic/internal/codec/dlms"
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
	// 串口上的 HDLC 帧在长线/干扰下会丢字节；重发 2 次能显著提高单次成功率，
	// 又不至于让一台坏表把一轮采集拖到几十秒。
	defaultRetries = 2
	// defaultGap 是两次事务之间的最小静默。
	defaultGap = 20 * time.Millisecond
	// defaultClientAddr 是客户端 HDLC 地址。
	//
	// 1 是「客户端管理进程」的保留地址；16(0x10) 是「公共客户端」，权限最低
	// 但永远可用。用 16 是为了避免与表上其它客户端管理进程撞号。
	defaultClientAddr = 0x10
	// defaultServerAddr 是服务端（电表）的逻辑设备地址。
	//
	// 1 是「管理逻辑设备」，按标准每个物理设备都必须有，因此不需要先扫描
	// 就知道它存在。真实表上的测量数据通常在逻辑设备 1 上。
	defaultServerAddr = 0x01
)

// Options 是设备级配置开关（来自 labels.options）。
type Options struct {
	// ClientAddr 是客户端 HDLC 地址。
	ClientAddr int
	// ServerAddr 是服务端逻辑设备地址。
	ServerAddr int
	// AddrSize 是地址域字节数（1/2/4）。
	AddrSize int
	// Retries 是重发次数。
	Retries int
	// Gap 是事务间静默。
	Gap time.Duration
	// MaxPDU 是建链时声明并采用的 max-receive-PDU。
	MaxPDU int
}

// Driver 是 DLMS 主站驱动。
type Driver struct{}

// New 返回驱动实例。
func New() *Driver { return &Driver{} }

// Meta 声明能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "dlms",
		Name:         "DLMS/COSEM 表计（IEC 62056）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:      driver.CapSupported,
			Write:     driver.CapUnsupported,
			Subscribe: driver.CapUnsupported,
			// 浏览能力靠读 object_list（`0.0.40.0.0.255` 的 attribute 2），
			// 但那是"读一个属性"而不是协议层的 browse 服务，且需要解
			// 嵌套的关联对象表——本驱动没有内置这套语义，因此如实声明
			// 不支持，而不是把一段原始字节当成"浏览结果"。
			Browse:    driver.CapUnsupported,
			Discovery: driver.CapUnsupported,
			// 一条 Get-Request 只读一个属性；批量是 get-request-with-list，
			// 需要服务端声明 multiple-references（现场极少支持）。
			MultiRead:   driver.CapUnsupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapSupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.port", Label: "串口", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0", Scope: "channel"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: 9600, Scope: "channel",
				Help: "标准典型值 300/9600/19200/38400/115200；必须以现场实测为准"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: "none", Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "IEC 62056-46 的 HDLC profile 规定 8N1（无校验）；配成 even 会收到全错位的字节"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "半双工总线必须串行；同一物理串口由多个通道共享时按总线锁排队"},
			{Name: "labels.options", Label: "通信开关", Type: "string", Default: "", Scope: "device",
				Help: "client=16,server=1,addr-size=1,retries=2,gap=20ms,max-pdu=512"},
		},
		PointFields: []driver.ConfigField{
			{Name: "labels.address", Label: "表地址", Type: "string", Scope: "device",
				Placeholder: "1",
				Help:        "服务端逻辑设备地址（1 是管理逻辑设备，始终存在）。也可用 labels.options 的 server="},
			{Name: "address.path", Label: "OBIS 地址", Type: "string", Required: true, Scope: "point",
				Placeholder: "1.0.1.8.0.255",
				Help:        "OBIS 六段码，可选 `:<class>:<attribute>` 后缀；例如 1.0.1.8.0.255 或 1.0.1.8.0.255:3:2"},
		},
		Description: "DLMS/COSEM 主站（IEC 62056-46/-53/-62）：LN 逻辑名引用 + 无安全 + HDLC 三层承载，" +
			"按 OBIS 码 + 类号 + 属性号逐点读（串口 / 串口服务器）",
	}
}

// Open 建链并建立应用关联。
//
// 建链在 Open 里完成（而不是第一次 Read 时懒加载），理由与 ADS/61850 一致：
// 把"表根本不通"这类故障提前到启动期。一个配置错的服务端地址如果在第一次
// 采集时才暴露，现场看到的是"点位全坏"而不是"设备连不上"。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	opt := parseOptions(cfg.Device)
	addr, err := addressOf(cfg.Device, opt)
	if err != nil {
		return nil, err
	}
	l, err := link.Open(ctx, link.Options{
		Proto:       "dlms",
		DefaultPort: defaultTCPPort,
	}, cfg.Channel.Transport, cfg.Channel.EffectiveTimeout(), cfg.Device.ID)
	if err != nil {
		return nil, err
	}
	c := &conn{
		link:    l,
		opt:     opt,
		addr:    addr,
		devID:   cfg.Device.ID,
		timeout: cfg.Device.Timeout.Duration(),
	}
	if err := c.associate(ctx); err != nil {
		_ = l.Close()
		return nil, err
	}
	return c, nil
}

// addressOf 取服务端地址。
//
// 两个来源：labels.address（字符串，能把 "1" 与"未配置"分开）与
// labels.options 的 server=。为什么两个都收：labels.address 是仓库里
// 所有表计驱动共用的字段（点表模板不必为 DLMS 改一列），而 options 里的
// server= 让"地址域风格"（client/server/addr-size）能在同一处配齐。
func addressOf(dev model.Device, opt Options) (dlmscodec.Address, error) {
	server := opt.ServerAddr
	if s := strings.TrimSpace(dev.Labels["address"]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return dlmscodec.Address{}, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("device.labels.address %q 不是整数", s), false, err)
		}
		server = n
	}
	switch opt.AddrSize {
	case 1:
		return dlmscodec.ServerAddress(server, 0, 1), nil
	case 2:
		return dlmscodec.ServerAddress(server, 0, 2), nil
	case 4:
		return dlmscodec.ServerAddress(server, 0, 4), nil
	}
	return dlmscodec.Address{}, driver.E(driver.ErrUnsupported, "open", dev.ID,
		fmt.Sprintf("addr-size=%d 非法（只能是 1/2/4）", opt.AddrSize), false, nil)
}

// parseOptions 解析 labels.options。
func parseOptions(dev model.Device) Options {
	opt := Options{
		ClientAddr: defaultClientAddr,
		ServerAddr: defaultServerAddr,
		AddrSize:   1,
		Retries:    defaultRetries,
		Gap:        defaultGap,
		MaxPDU:     512,
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
		case "client":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= 0x7F {
				opt.ClientAddr = n
			}
		case "server":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= 0x3FFF {
				opt.ServerAddr = n
			}
		case "addr-size":
			if n, err := strconv.Atoi(val); err == nil && (n == 1 || n == 2 || n == 4) {
				opt.AddrSize = n
			}
		case "retries":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= 10 {
				opt.Retries = n
			}
		case "gap":
			if d, err := time.ParseDuration(val); err == nil && d >= 0 && d <= time.Second {
				opt.Gap = d
			}
		case "max-pdu":
			if n, err := strconv.Atoi(val); err == nil && n >= 64 && n <= 4096 {
				opt.MaxPDU = n
			}
		}
		_ = hasVal
	}
	return opt
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	link    link.Link
	opt     Options
	addr    dlmscodec.Address
	devID   string
	timeout time.Duration

	mu     sync.Mutex
	closed bool
	// ns/nr 是本端 I 帧的发送/接收序号（modulo 8）。
	//
	// 每发一个 I 帧 NS 加一、每收一个 I 帧 NR 加一。写错的表现是表回
	// FRMR 或干脆不回——HDLC 用序号判断"这一帧是不是重复/丢失"。
	ns, nr uint8
	// associated 表示 AARQ 已成功。
	associated bool
	// linkErr 记录首次链路级失败，避免每个点位都重复等一次超时。
	linkErr error
	// scalers 缓存每个 "class/obis" 的 scaler（来自 attribute 3）。
	//
	// 为什么要缓存：工程值 = attribute2 × 10^scaler，而 scaler 在同一个对象
	// 的 attribute 3 里。每轮采集都读一遍 scaler 会让往返数翻倍；而 scaler
	// 是对象的**静态属性**（配置参数），不会随测量值变化，因此进程内缓存
	// 是安全的。首次读某对象时读一次 attribute 3，之后复用。
	//
	// 读不到 scaler（表不提供 attribute 3，或返回错误）时缓存 nil 语义的
	// 哨兵值，避免每个点都重试一次注定失败的请求。
	scalers map[string]scalerEntry
	// version / conformance / vaaName 是协商结果，供自检命令打印。
	version     uint8
	conformance dlmscodec.Conformance
	vaaName     uint16
}

// scalerEntry 是 scaler 缓存项。
type scalerEntry struct {
	scaler int
	// known=false 表示"已经试过但表不支持 attribute 3"，不要再试。
	known bool
}

func (c *conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	assoc := c.associated
	c.associated = false
	c.mu.Unlock()

	if assoc {
		// 断链失败不影响资源释放：连接马上要关掉，DISC 只是礼貌。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.disconnect(ctx)
		cancel()
	}
	return c.link.Close()
}

// Probe 验证链路 + 应用关联。
//
// 与 Open 的区别：Open 已经建过一次，Probe 可以复用它（不重新 AARQ）；
// 但如果上一次失败，Probe 会重试一次完整的 SNRM+AARQ。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", false, nil)
	}
	if c.associated {
		return nil
	}
	return c.associateLocked(ctx)
}

// associate 在持锁外调用（Open 用）。
func (c *conn) associate(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.associateLocked(ctx)
}

// associateLocked 执行 SNRM → AARQ。
//
// 为什么两步都做：SNRM 建立 HDLC 层的正常响应模式（表才会回 I 帧），
// AARQ 建立应用层关联（表才允许 Get）。少一步的症状分别是：
//   - 少 SNRM：AARQ 发出去表不回（它还在断开模式里）
//   - 少 AARQ：Get-Request 发出去回 Data-Access-Result 或直接被拒
func (c *conn) associateLocked(ctx context.Context) error {
	if err := c.snrm(ctx); err != nil {
		return err
	}
	aare, err := c.aarq(ctx)
	if err != nil {
		return err
	}
	if !aare.ApplicationOK {
		// 应用关联被拒：这是协议级拒绝，不是链路错。带着 result 与诊断
		// 一起报，现场才知道是"上下文不支持"还是"认证失败"。
		return driver.E(driver.ErrProtocol, "open", c.devID,
			fmt.Sprintf("AARE 拒绝关联（result=%d，诊断=%d）：应用上下文不支持或需要认证", aare.Result, aare.Diagnostic),
			false, nil)
	}
	if !aare.HasInitiate {
		return driver.E(driver.ErrProtocol, "open", c.devID,
			"AARE 缺少 xDLMS-Initiate.response（user-information 解析失败）", false, nil)
	}
	if aare.Initiate.DLMSVersion < 6 {
		return driver.E(driver.ErrUnsupported, "open", c.devID,
			fmt.Sprintf("表协商的 DLMS 版本是 %d，本驱动要求 6", aare.Initiate.DLMSVersion), false, nil)
	}
	// 采用服务端给的 max PDU：它比我们声明的更小就不能超过它。
	if int(aare.Initiate.MaxReceivePDU) < c.opt.MaxPDU && aare.Initiate.MaxReceivePDU > 0 {
		c.opt.MaxPDU = int(aare.Initiate.MaxReceivePDU)
	}
	c.version = aare.Initiate.DLMSVersion
	c.conformance = aare.Initiate.Conformance
	c.vaaName = aare.Initiate.VAAName
	c.associated = true
	c.linkErr = nil
	return nil
}

// snrm 发 SNRM 并等 UA。
func (c *conn) snrm(ctx context.Context) error {
	fr := dlmscodec.Frame{
		Dest:    c.addr,
		Src:     dlmscodec.ClientAddress(c.opt.ClientAddr),
		Control: dlmscodec.Control{Kind: dlmscodec.KindSNRM, PF: true},
	}
	fr.Control.PF = true // SNRM 必须带 P/F 位
	body, err := fr.Encode()
	if err != nil {
		return driver.E(driver.ErrUnsupported, "snrm", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, "snrm", body, dlmscodec.KindUA)
	if err != nil {
		return err
	}
	if resp.Kind() != dlmscodec.KindUA {
		if resp.Kind() == dlmscodec.KindDM {
			return driver.E(driver.ErrProtocol, "snrm", c.devID,
				"表回了 DM（断开模式）：通常是客户端地址或逻辑设备地址不对", false, nil)
		}
		return driver.E(driver.ErrProtocol, "snrm", c.devID,
			fmt.Sprintf("SNRM 的应答应是 UA，收到 %s 帧", resp.Kind()), false, nil)
	}
	// 建链后序号清零。
	c.ns, c.nr = 0, 0
	return nil
}

// aarq 发 AARQ 并等 AARE。
func (c *conn) aarq(ctx context.Context) (dlmscodec.AssociateResponse, error) {
	req := dlmscodec.AssociateRequest{
		Initiate: dlmscodec.InitiateRequest{
			DLMSVersion: 6,
			// 只声明真正会用的：GET 与 attribute0-get（读 object_list 需要）。
			Conformance:   dlmscodec.ConfGet | dlmscodec.ConfAttribute0Get,
			MaxReceivePDU: uint16(c.opt.MaxPDU),
		},
	}
	apdu, err := req.Encode()
	if err != nil {
		return dlmscodec.AssociateResponse{}, driver.E(driver.ErrInternal, "aarq", c.devID, err.Error(), false, err)
	}
	fr := dlmscodec.Frame{
		Dest:    c.addr,
		Src:     dlmscodec.ClientAddress(c.opt.ClientAddr),
		Control: dlmscodec.Control{Kind: dlmscodec.KindI, NS: int(c.ns), NR: int(c.nr), PF: true},
		Info:    append([]byte(dlmscodec.LLCCommand), apdu...),
	}
	body, err := fr.Encode()
	if err != nil {
		return dlmscodec.AssociateResponse{}, driver.E(driver.ErrInternal, "aarq", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, "aarq", body, dlmscodec.KindI)
	if err != nil {
		return dlmscodec.AssociateResponse{}, err
	}
	c.ns = (c.ns + 1) & 0x07
	c.nr = (uint8(resp.Control.NS) + 1) & 0x07

	ap, ok := resp.APDU()
	if !ok || len(ap) == 0 {
		return dlmscodec.AssociateResponse{}, driver.E(driver.ErrProtocol, "aarq", c.devID,
			"AARE 帧缺少 LLC 头或 APDU 为空", false, nil)
	}
	aare, err := dlmscodec.ParseAssociateResponse(ap)
	if err != nil {
		return dlmscodec.AssociateResponse{}, driver.E(driver.ErrProtocol, "aarq", c.devID,
			"AARE 解析失败："+err.Error(), false, err)
	}
	return aare, nil
}

// disconnect 发 DISC 并等 UA。
func (c *conn) disconnect(ctx context.Context) error {
	fr := dlmscodec.Frame{
		Dest:    c.addr,
		Src:     dlmscodec.ClientAddress(c.opt.ClientAddr),
		Control: dlmscodec.Control{Kind: dlmscodec.KindDISC, PF: true},
	}
	body, err := fr.Encode()
	if err != nil {
		return err
	}
	_, err = c.exchange(ctx, "disconnect", body, dlmscodec.KindUA)
	return err
}

// Read 逐点读。
//
// 一条 Get-Request 一个属性：这是协议的硬约束，不是实现偷懒。因此
// 一轮采集的往返数是点位数。把这一点写进错误信息与文档，比让配置的人
// 以为"驱动会合并"更重要——300 个点挂 9600bps 的串口，一轮要十几秒。
//
// 单个点位失败（OBIS 写错、类号不匹配）不升级为链路失败：其它点位仍然
// 读到了，说明链路是通的。只有链路级错误（超时/断连）才把整批标坏并
// 记住 linkErr，避免每个点位都重复等一次超时。
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
	if !c.associated {
		if err := c.associateLocked(ctx); err != nil {
			return allFailed(refs, err), err
		}
	}

	out := make([]driver.RawSample, 0, len(refs))
	for _, ref := range refs {
		s, err := c.readPoint(ctx, ref)
		if err != nil {
			out = append(out, driver.RawSample{PointID: ref.ID, Quality: qualityOf(err), Err: err})
			// 链路级错误：记住并中止，剩下的点位不必再各等一次超时。
			if isLinkFailure(err) {
				c.linkErr = err
				for _, rest := range refs[len(out):] {
					out = append(out, driver.RawSample{
						PointID: rest.ID, Quality: qualityOf(err), Err: err,
					})
				}
				return out, err
			}
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// readPoint 读一个属性并解码。
//
// 分两步：先发一次 Get-Request 取回 A-XDR 值，再按需要应用 scaler。
// 之所以不把 scaler 逻辑塞进请求里（例如一次读两块），是因为 DLMS 一条
// Get 只能读一个属性——这是协议硬约束，读 scaler 必然是一次额外往返。
func (c *conn) readPoint(ctx context.Context, ref driver.PointRef) (driver.RawSample, error) {
	desc, err := attributeOf(ref.Address)
	if err != nil {
		return driver.RawSample{}, err
	}
	v, err := c.getAttribute(ctx, ref.ID, desc)
	if err != nil {
		return driver.RawSample{}, err
	}
	// 只有 attribute 2（value）需要应用 scaler。attribute 3 本身就是
	// scaler_unit 结构、attribute 1 是 OBIS，乘 10^n 没有意义。
	if desc.Attribute == dlmscodec.AttrValue {
		scaler, has, serr := c.scalerOf(ctx, desc)
		switch {
		case serr != nil:
			// Register 家族的值取不到 scaler：拿到的原始整数可能是 2201（真值
			// 220.1 V），也可能是别的量级，当成「不缩放」报出去就是静默回绕——
			// 与 codec 层 scalerUnit「形态不对不默认 0，真值可能差 1000 倍」的
			// 既定原则一致。标坏而不是报一个可能错 10^n 倍的 good 值；下一轮
			// 会重试，链路/点表恢复后自动回到正确缩放。
			return driver.RawSample{}, serr
		case has:
			if n, numeric := v.Number(); numeric {
				v = dlmscodec.Value{Tag: dlmscodec.TagFloat64,
					Float: dlmscodec.ApplyScaler(n, scaler)}
			}
		}
		// has=false && serr==nil：这个类本身没有 scaler_unit（Data/Clock 等），
		// 原始值即终值，按 good 上送。
	}
	val, err := valueOf(ref, v)
	if err != nil {
		return driver.RawSample{}, err
	}
	return driver.RawSample{PointID: ref.ID, Value: val, Quality: model.QualityGood}, nil
}

// scalerOf 取某对象的 scaler（来自 attribute 3），带进程内缓存。
//
// 返回值三种语义：
//
//	(scaler, true,  nil)   Register 家族，scaler_unit 读到了；
//	(0,     false, nil)    这个类按标准没有 scaler_unit（Data/Clock 等）——
//	                       这是一个**类级别**的事实、与网络无关，缓存 known=false 是安全的；
//	(0,     false, err)    Register 家族但 attribute 3 读失败/无法解析——**不缓存**，
//	                       把错误交回调用方判坏。见下。
//
// 缓存失效面：表在运行中被重新配置（改 register 的 scaler_unit）时缓存会
// 变旧。这个风险是刻意接受的——scaler 是计费参数，运行期改变它属于运维
// 动作，通常伴随重启采集。相比之下每轮每点都多读一次 attribute 3 会让
// 往返数翻倍，在 300 点的表上是不可接受的代价。
//
// 关键：Register 家族按标准**一定有** scaler_unit。这里读 attribute 3 失败，
// 可能是瞬时链路抖动，也可能是表本身缺这个属性——两者都不是「类没有 scaler」
// 这一类级别的事实。早先实现把这种失败也缓存成 known=false，于是：
//
//  1. 该点此后永远按未缩放的原始值上报（getter 命中缓存直接返回），
//     即便原始值需要 ×10^-1 才是对的（2201 被当成 220.1 V 的 10 倍）；
//  2. 缓存没有失效路径，链路/点表随后恢复也回不到正确缩放。
//
// 因此 Register 家族的读失败一律**不缓存**，交回调用方按本轮判坏、下一轮重试。
// 只有类级别的「没有 scaler_unit」才缓存 known=false。
func (c *conn) scalerOf(ctx context.Context, desc dlmscodec.AttributeDescriptor) (int, bool, error) {
	key := fmt.Sprintf("%d/%s", desc.ClassID, dlmscodec.OBIS(desc.InstanceID).String())
	if e, ok := c.scalers[key]; ok {
		return e.scaler, e.known, nil
	}
	if c.scalers == nil {
		c.scalers = map[string]scalerEntry{}
	}
	// 只有 Register 家族才有 scaler_unit；其它类（Data/Clock）**按类**确定没有，
	// 缓存 known=false 安全。
	switch desc.ClassID {
	case dlmscodec.ClassRegister, dlmscodec.ClassExtendedRegister, dlmscodec.ClassDemandRegister:
	default:
		c.scalers[key] = scalerEntry{}
		return 0, false, nil
	}
	scDesc := desc
	scDesc.Attribute = dlmscodec.AttrScalerUnit
	v, err := c.getAttribute(ctx, "", scDesc)
	if err != nil {
		// 不缓存：可能是瞬时抖动，下一轮要重试。交回调用方判坏。
		return 0, false, err
	}
	scaler, ok := scalerUnit(v)
	if !ok {
		// 形态不对同样不缓存：按 scalerUnit 的既定原则报「没有 scaler」，
		// 绝不默认 0（默认 0 = 不缩放 = 真值可能差 1000 倍）。
		return 0, false, driver.E(driver.ErrProtocol, "read", c.devID,
			"scaler_unit 结构无法解析（应为 {integer, enum}）", false, nil)
	}
	c.scalers[key] = scalerEntry{scaler: scaler, known: true}
	return scaler, true, nil
}

// getAttribute 发一条 Get-Request-Normal 并解出 A-XDR 值。
func (c *conn) getAttribute(ctx context.Context, pointID string, desc dlmscodec.AttributeDescriptor) (dlmscodec.Value, error) {
	req := dlmscodec.GetRequest{Attribute: desc}
	apdu := req.Encode()
	fr := dlmscodec.Frame{
		Dest:    c.addr,
		Src:     dlmscodec.ClientAddress(c.opt.ClientAddr),
		Control: dlmscodec.Control{Kind: dlmscodec.KindI, NS: int(c.ns), NR: int(c.nr), PF: true},
		Info:    append([]byte(dlmscodec.LLCCommand), apdu...),
	}
	body, err := fr.Encode()
	if err != nil {
		return dlmscodec.Value{}, driver.E(driver.ErrInternal, "read", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, "read", body, dlmscodec.KindI)
	if err != nil {
		return dlmscodec.Value{}, err
	}
	c.ns = (c.ns + 1) & 0x07
	c.nr = (uint8(resp.Control.NS) + 1) & 0x07

	ap, ok := resp.APDU()
	if !ok || len(ap) == 0 {
		return dlmscodec.Value{}, driver.E(driver.ErrProtocol, "read", c.devID,
			"Get-Response 缺少 LLC 头或 APDU 为空", false, nil)
	}
	gr, err := dlmscodec.ParseGetResponse(ap)
	if err != nil {
		return dlmscodec.Value{}, driver.E(driver.ErrProtocol, "read", c.devID,
			"Get-Response 解析失败："+err.Error(), false, err)
	}
	// 失败结果码：Data-Access-Result 是**协议里的应用答复**，不是链路错。
	// object-undefined / object-class-inconsistent 都归 ErrAddress，因为
	// 它们指向点表写错（OBIS 或类号），重试无用。
	if gr.IsResult {
		return dlmscodec.Value{}, driver.E(driver.ErrAddress, "read", c.devID,
			fmt.Sprintf("point %q: %s", pointID, gr.Result), false, nil)
	}
	if gr.Kind == 2 {
		// 数据块：本轮不支持拼块。明确报告而不是把半截数据当真值。
		return dlmscodec.Value{}, driver.E(driver.ErrUnsupported, "read", c.devID,
			fmt.Sprintf("point %q: 表返回数据块（last=%v block=%d），本驱动不支持分块读；"+
				"请把 max-pdu 调大到能装下该属性", pointID, gr.Block.LastBlock, gr.Block.BlockNo),
			false, nil)
	}
	if gr.Kind == 3 {
		return dlmscodec.Value{}, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("point %q: 表返回 Get-Response-With-List，但请求的是单属性", pointID), false, nil)
	}
	if !gr.HasData {
		return dlmscodec.Value{}, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("point %q: 应答没有数据也没有结果码", pointID), false, nil)
	}
	return gr.Data, nil
}

// valueOf 把 A-XDR 值转成 model.Value。
//
// 三个必须处理好的形态：
//
//  1. **标量**：直接取数值。bool 按 0/1 交出去（继电器状态点很常见）。
//  2. **scaler_unit 结构**：`{integer, enum}`。它本身不是一个"测量值"，
//     而是 attribute 3；被它缩放的原始值在 attribute 2。因此这里把结构
//     按 `{scaler, unit}` 文本交出去（供诊断），缩放乘法由 readPoint 在
//     拿到两个属性后做——把结构硬压成单值会丢掉 unit，也会让 attribute 2
//     的原始值看起来已经缩放过了。
//  3. **octet-string / visible-string**：表号、逻辑设备名是文本；能打印成
//     可见字符就给原文，否则给十六进制。
//
// null-data（含 dontcare）不是 0：它表示表此刻没有可信值（未置值、对象
// 数据尚未生成）。把 null 解成 good 质量的 0，下游看到的是"读数正常、
// 恰好为零"——电表上 0 和无值天差地别（负荷曲线空白 vs 停电）。这里按
// ErrProtocol 标坏，下一轮采集重试，值生成了自然恢复。
func valueOf(ref driver.PointRef, v dlmscodec.Value) (model.Value, error) {
	switch v.Kind() {
	case dlmscodec.KindArray:
		// structure / array：scaler_unit 或 object_list 的一段。统一按可读
		// 文本交出去，并保留完整括号结构——把结构体硬压成单值是"看起来
		// 成功、其实丢了一半"。
		return model.TextValue(model.TypeString, v.String()), nil
	case dlmscodec.KindBytes:
		// octet-string：表号/逻辑设备名是 ASCII；二进制（如 clock 状态）也走
		// 这条路径。能打印成可见字符就转成文本，否则给十六进制。
		return model.TextValue(model.TypeString, printableOrHex(v.Bytes)), nil
	case dlmscodec.KindDateTime:
		if t, ok := v.DateTime(); ok {
			return model.TextValue(model.TypeString, t.Format("2006-01-02T15:04:05")), nil
		}
		return model.TextValue(model.TypeString, v.String()), nil
	case dlmscodec.KindString:
		return model.TextValue(model.TypeString, v.Text), nil
	case dlmscodec.KindNull:
		return model.Value{}, driver.E(driver.ErrProtocol, "decode", ref.ID,
			"meter returned null-data (no value available)", false, nil)
	}
	// 数值 / 布尔
	if n, ok := v.Number(); ok {
		return model.NumValue(model.TypeFloat64, n), nil
	}
	return model.TextValue(model.TypeString, v.String()), nil
}

// scalerUnit 从 `structure { integer scaler, enum unit }` 里取出 scaler。
//
// 只返回 scaler，不返回"换算后的值"：scaler_unit 结构里**没有**被测值，
// 被测值在同一个对象的 attribute 2。工程值 = attribute2 的原始整数 ×
// 10^scaler，这个乘法必须在拿到两个属性之后才能做（见 applyScaler）。
func scalerUnit(v dlmscodec.Value) (int, bool) {
	if len(v.Elems) != 2 {
		return 0, false
	}
	// 第一元素必须是整数（scaler），第二元素是枚举（unit）。
	raw, ok1 := v.Elems[0].Number()
	if !ok1 {
		return 0, false
	}
	if _, ok2 := v.Elems[1].Number(); !ok2 {
		return 0, false
	}
	return int(raw), true
}

// printableOrHex 把字节转成可读文本：全可打印时当 ASCII，否则十六进制。
func printableOrHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return fmt.Sprintf("% X", b)
		}
	}
	return string(b)
}

// attributeOf 把点位地址解成 COSEM 属性描述符。
//
// 地址语法：`<OBIS>`、`<OBIS>:<class>` 或 `<OBIS>:<class>:<attribute>`。
// 只写 OBIS 时类号与属性号从内置常见表推导；不在表里则**报错而不是猜**——
// 猜错类的表现是 object-class-inconsistent(9)，比直接说"这个 OBIS 不认识，
// 请显式给类号"难查得多。
//
// 切分交给 dlmscodec.SplitRef：OBIS 的标准写法本身含冒号
// （`1-0:1-8-0-255`），在这里自己按冒号切会把标准写法切坏。
func attributeOf(a model.Address) (dlmscodec.AttributeDescriptor, error) {
	var out dlmscodec.AttributeDescriptor
	if a.Kind != model.AddressSymbol {
		return out, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("DLMS 点位必须用符号地址（kind=symbol），得到 %q", a.Kind), false, nil)
	}
	path := strings.TrimSpace(a.Path)
	if path == "" {
		return out, driver.E(driver.ErrAddress, "read", "", "OBIS 地址为空", false, nil)
	}
	obisPart, classID, attrID, err := dlmscodec.SplitRef(path)
	if err != nil {
		return out, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("地址 %q 无法解析: %v", path, err), false, err)
	}
	obis, err := dlmscodec.ParseOBIS(obisPart)
	if err != nil {
		return out, driver.E(driver.ErrAddress, "read", "", err.Error(), false, err)
	}
	out.InstanceID = [6]byte(obis)
	if classID < 0 {
		obj, ok := dlmscodec.LookupOBIS(obis.String())
		if !ok {
			return out, driver.E(driver.ErrAddress, "read", "",
				fmt.Sprintf("OBIS %s 不在内置常见表里，请显式写 `%s:<class>:<attribute>`",
					obis, obis), false, nil)
		}
		out.ClassID = obj.Class
		out.Attribute = obj.Attribute
		return out, nil
	}
	out.ClassID = uint16(classID)
	if attrID < 0 {
		// 给了类号却没给属性号：用通用默认（attribute 2 = value）。
		out.Attribute = dlmscodec.AttrValue
	} else {
		out.Attribute = int8(attrID)
	}
	return out, nil
}

// Scan 读 object_list 附近的常见对象，供 CLI 的 `-scan` 用。
//
// 只试内置常见表里的条目：真正的"全表扫描"必须读 object_list
// （`0.0.40.0.0.255` 的 attribute 2），那是一个嵌套结构，需要按
// Association LN 的对象表逐条解出 class/version/OBIS。本驱动没有内置
// 这套语义（见 Meta 里 Browse 的说明），所以 Scan 只回答"库里的常见量
// 这台表有没有"，并如实标注。
func (c *conn) Scan(ctx context.Context) ([]ScanResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "scan", c.devID, "connection closed", false, nil)
	}
	if !c.associated {
		if err := c.associateLocked(ctx); err != nil {
			return nil, err
		}
	}
	var out []ScanResult
	for _, obj := range dlmscodec.CommonObjects() {
		obis, err := dlmscodec.ParseOBIS(obj.OBIS)
		if err != nil {
			continue
		}
		ref := driver.PointRef{
			ID:      obj.OBIS,
			Address: model.Address{Kind: model.AddressSymbol, Path: obj.OBIS},
		}
		s, err := c.readPoint(ctx, ref)
		res := ScanResult{OBIS: obj.OBIS, Name: obj.Name, Class: obj.Class}
		if err != nil {
			if isLinkFailure(err) {
				return out, err
			}
			res.Err = err.Error()
			out = append(out, res)
			continue
		}
		res.OK = true
		res.Text = s.Value.String()
		// 顺手读出 scaler_unit（attribute 3）——现场核对电量单位必看。
		if cu, cerr := c.readPoint(ctx, driver.PointRef{
			ID: obj.OBIS + ":3",
			Address: model.Address{Kind: model.AddressSymbol,
				Path: fmt.Sprintf("%s:%d:3", obj.OBIS, obj.Class)},
		}); cerr == nil {
			res.ScalerUnit = cu.Value.String()
		}
		_ = obis
		out = append(out, res)
	}
	return out, nil
}

// ScanResult 是 Scan 的一条结果。
type ScanResult struct {
	OBIS       string
	Name       string
	Class      uint16
	OK         bool
	Text       string
	ScalerUnit string
	Err        string
}

// AssociationInfo 是本端已协商出的应用关联参数。
//
// 它只服务自检命令：现场核对"表到底答应了哪些服务"必须看**协商后**的
// conformance，而不是我们自己请求的那份。标准里服务端有权砍掉一半能力
// （见 AARE 的 0x00501F 示例），按请求值判断会得出错误结论。
type AssociationInfo struct {
	// ClientAddr / ServerAddr / AddrSize 是本端使用的 HDLC 地址。
	ClientAddr int
	ServerAddr int
	AddrSize   int
	// DLMSVersion 是协商出的版本（正常为 6）。
	DLMSVersion uint8
	// Conformance 是服务端在 AARE 里回的位块。
	Conformance dlmscodec.Conformance
	// MaxPDU 是本次采用的 max-receive-PDU（取双方较小值）。
	MaxPDU int
	// VAAName 是服务端关联名（LN 引用常见 0x0007）。
	VAAName uint16
	// Associated 表示当前是否持有有效关联。
	Associated bool
}

// Association 返回当前协商结果，供自检命令打印。
func (c *conn) Association() AssociationInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return AssociationInfo{
		ClientAddr:  c.opt.ClientAddr,
		ServerAddr:  c.opt.ServerAddr,
		AddrSize:    c.opt.AddrSize,
		DLMSVersion: c.version,
		Conformance: c.conformance,
		MaxPDU:      c.opt.MaxPDU,
		VAAName:     c.vaaName,
		Associated:  c.associated,
	}
}

// Write 不支持（见包注释）。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, driver.WriteResult{
			PointID: req.PointID,
			Quality: model.QualityBad,
			Err: driver.E(driver.ErrUnsupported, "write", c.devID,
				"DLMS 驱动不支持写入：SET 需要 selective access 与分块写状态机，且写错会改表的计费参数", false, nil),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// exchange 发一帧并等一帧，按 expect 校验帧类型；内部带重发。
func (c *conn) exchange(ctx context.Context, op string, body []byte, expect dlmscodec.FrameKind) (dlmscodec.Frame, error) {
	c.link.Lock()
	defer c.link.Unlock()

	var lastErr error
	for attempt := 0; attempt <= c.opt.Retries; attempt++ {
		if attempt > 0 && c.opt.Gap > 0 {
			select {
			case <-ctx.Done():
				return dlmscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
			case <-time.After(c.opt.Gap):
			}
		}
		fr, err := c.exchangeOnce(ctx, op, body, expect)
		if err == nil {
			return fr, nil
		}
		lastErr = err
		switch driver.KindOf(err) {
		case driver.ErrDisconnected, driver.ErrUnsupported, driver.ErrPermission, driver.ErrAddress:
			return dlmscodec.Frame{}, err
		}
		// 尊重 exchangeOnce 已经分好的「可重发」标记。HCS/FCS 校验错是线路
		// 干扰（可重发），而 DM/FRMR、LLC 头不是应答形态、期望的帧类型不符
		// 这类失败说明**表在正常应答、只是我们理解错了**（地址错、序号不同步、
		// 服务端主动上报）——重发同一帧只会拿到同样的结果。两者在错误种类上
		// 同为 ErrProtocol，只看 KindOf 会把「不重发」的那类一起重试到上限。
		if !driver.IsRetryable(err) {
			return dlmscodec.Frame{}, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return dlmscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
		}
	}
	if isLinkFailure(lastErr) {
		c.linkErr = lastErr
	}
	return dlmscodec.Frame{}, lastErr
}

// exchangeOnce 是一次「排空 → 设截止 → 发 → 收 → 校验」。
func (c *conn) exchangeOnce(ctx context.Context, op string, body []byte, expect dlmscodec.FrameKind) (dlmscodec.Frame, error) {
	if c.opt.Gap > 0 {
		select {
		case <-ctx.Done():
			return dlmscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		case <-time.After(c.opt.Gap):
		}
	}
	// 半双工纪律：发之前清空接收缓冲，避免把上一轮迟到的响应当成本轮。
	// 顺序必须 Drain → SetDeadline → 发送（Drain 会把截止时间压到 now）。
	_ = c.link.Drain()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.link.SetDeadline(dl)
	} else {
		_ = c.link.SetDeadline(time.Now().Add(c.deadline()))
	}
	if err := dlmscodec.WriteFrame(c.link, body); err != nil {
		return dlmscodec.Frame{}, driver.E(driver.ErrDisconnected, op, c.devID, "串口写入失败", true, err)
	}

	rd := dlmscodec.NewReader(c.link)
	fr, err := rd.Next()
	if err != nil {
		return dlmscodec.Frame{}, c.readError(op, err, ctx, rd.BadFrames())
	}
	if fr.Kind() != expect {
		if fr.Kind() == dlmscodec.KindDM {
			return dlmscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.devID,
				"表回了 DM（断开模式）：客户端或逻辑设备地址不对，或表拒绝建链", false, nil)
		}
		if fr.Kind() == dlmscodec.KindFRMR {
			return dlmscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.devID,
				"表回了 FRMR（帧拒绝）：I 帧序号或帧结构不被接受", false, nil)
		}
		return dlmscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("期望 %s 帧，收到 %s 帧", expect, fr.Kind()), false, nil)
	}
	// 应答必须带 LLCResponse 头：这是"这一帧是应答"的唯一信号。
	if expect == dlmscodec.KindI && !fr.IsResponse() {
		return dlmscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.devID,
			"I 帧的 LLC 头不是应答形态（E6 E7 00），可能是服务端主动上报", false, nil)
	}
	return fr, nil
}

func (c *conn) deadline() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	// 9600bps 下一帧 200 字节约 0.2 秒；留 3 秒覆盖拆包与慢表。
	return 3 * time.Second
}

// readError 把底层错误分类成驱动的错误分类。
//
// 关键区分：线路上**有字节回来但一帧都没成**。
//
// Reader 会跳过坏帧继续扫描，所以它最终报出来的总是"超时"；但超时的原因
// 可能截然不同。若已经跳过了坏帧，说明表确实在应答，只是我们没解对——
// 最常见的是波特率/校验位配错（HDLC profile 是 8N1，配成 8E1 时收到的
// 全是被采错的位），或终端电阻缺失导致的边沿畸变。把这归成 ErrProtocol
// 而不是 ErrTimeout，现场才会去查参数而不是去查线。
func (c *conn) readError(op string, err error, ctx context.Context, badFrames int) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctxErr)
	}
	if badFrames > 0 {
		return driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("收到 %d 个坏帧：检查波特率/校验位（HDLC profile 为 8N1）与线路终端电阻", badFrames),
			true, err)
	}
	if errors.Is(err, dlmscodec.ErrChecksum) {
		return driver.E(driver.ErrProtocol, op, c.devID, "应答校验序列错误（总线干扰）", true, err)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.devID,
			fmt.Sprintf("等待表 %s 应答超时", c.addr), true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.devID, "链路读取失败", true, err)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// flattenRefs 摊平 spans 并按 PointID 去重。
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
