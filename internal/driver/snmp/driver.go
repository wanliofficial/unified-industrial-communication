// Package snmp 实现 SNMP 主站驱动（UDP 161），支持 v1/v2c 与 v3（USM）。
//
// 这一层只做三件事：把点位的 OID 组织成 GET / SET 事务、把 varbind 的
// 线类型翻译成平台数据类型、把 error-status 与 v2c 异常标记翻译成驱动
// 错误分类。报文结构本身在 codec/snmp 里；v3 的引擎发现、时间同步与密钥
// 管理在 v3session.go 里。
//
// 三条设计取舍：
//
//  1. **v1/v2c 与 v3 共用同一套地址模型与批量预算**，只是 v3 外面多了一层
//     USM 封套。v1/v2c 的 `Build`/`Parse` 遇到版本 3 明确拒绝并把调用方指向
//     `BuildV3`/`ParseV3`，不会退回 v2c 去连只开 v3 的代理（那只会得到
//     超时，现场会误判成网络不通）。
//  2. **读用一条 GET 带多个 varbind**，并按**编码后的字节预算**分批
//     （不是只按个数）。SNMP 跑在 UDP 上，超过 MTU 的报文会被分片；现场
//     大量设备与防火墙直接丢分片，症状是「OID 少的时候能读，点表一多就
//     超时」。按 1400 字节估计可以覆盖最常见的 1500 MTU。
//  3. **v1 的 noSuchName 逐点重发。** v1 没有 per-varbind 异常：一个 OID
//     不存在会让整批返回 noSuchName，error-index 指向它，但**其余 OID 的
//     值也一起丢了**。如果不退化重发，一个写错的 OID 会让同批所有点位
//     变成地址错误，看起来像「设备突然不认所有 OID」。
//
// 不做的部分同样明确：不接收 Trap（轮询主站的采集语义与「设备主动推」
// 不同）、不做 MIB 编译/名字解析（需要一整套 MIB 文件与解析器）、不做
// Walk 作为生产采集（自检 CLI 里的遍历是诊断工具，不是点表语义）。
package snmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// sysDescrOID 是自检读的对象（设备描述）。用它的理由：所有代理都必须
// 实现 system 组；即使视图 ACL 只允许读别的子树，返回 noSuchObject 也
// 证明「报文能到、community 对、代理在回」。
var sysDescrOID = snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")

const (
	// defaultCommunity 是最常见的只读团体名。默认值不是「安全默认」，
	// 而是「现场第一次接通最少配置」；文档与配置表单都要求显式确认。
	defaultCommunity = "public"
	// defaultRetries 是超时后的额外重发次数。UDP 丢包常见，但 SNMP 代理
	// 通常是单线程处理，重试太多只会把一轮采集拖长。
	defaultRetries = 2
	// defaultMaxOids 是一帧内的 OID 个数上限（防御性上限，不是协议限制）。
	defaultMaxOids = 32
	// maxRequestBytes 是我们主动发送的报文预算。
	maxRequestBytes = 1400
)

// Dialer 允许测试注入内存数据报链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error)

// Driver 是 SNMP 主站驱动（v1/v2c/v3）。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 UDP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return transport.DialUDP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明 SNMP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "snmp",
		Name:         "SNMP",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"udp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:161", Scope: "channel",
				Help: "SNMP 代理地址；省略端口时按 161 处理"},
			{Name: "labels.community", Label: "团体名", Type: "string", Default: defaultCommunity,
				Secret: true,
				Scope:  "device",
				Help:   "v1/v2c 的 community，等同于只读/读写口令；界面按凭据掩码显示。v3 忽略此字段。"},
			{Name: "labels.version", Label: "版本", Type: "enum", Default: "2c", Scope: "device",
				Enum: []string{"1", "2c", "3"},
				Help: "1 没有 per-varbind 异常与 GetBulk；2c 是默认值；3 走 USM（引擎发现 + 时间窗口 + 可选认证/加密）"},
			// 以下五个字段只在 version=3 时生效，v1/v2c 配置里被忽略，
			// 这样同一份配置切换版本不会「残留的 v3 参数影响 v2c」。
			{Name: "labels.username", Label: "USM 用户名", Type: "string", Scope: "device",
				Help: "v3 必填（USM security name）；空用户名几乎总是配置漏了"},
			{Name: "labels.authProtocol", Label: "认证算法", Type: "enum", Default: "none", Scope: "device",
				Enum: []string{"none", "md5", "sha", "sha1", "sha224", "sha256", "sha384", "sha512"},
				Help: "v3 认证算法；noAuth 选 none。加密档位（privProtocol≠none）必须配认证算法"},
			{Name: "labels.authPassword", Label: "认证口令", Type: "string", Secret: true, Scope: "device",
				Help: "v3 认证口令（8..64 字节）；界面按凭据掩码，CLI 走 UIC_SNMP_AUTH_PASSWORD"},
			{Name: "labels.privProtocol", Label: "加密算法", Type: "enum", Default: "none", Scope: "device",
				Enum: []string{"none", "des", "aes", "aes128", "aes192", "aes256"},
				Help: "v3 加密算法；noPriv 选 none。AES-192/256 的密钥派生要求认证算法为 SHA-256 及以上"},
			{Name: "labels.privPassword", Label: "加密口令", Type: "string", Secret: true, Scope: "device",
				Help: "v3 加密口令；留空回退到 authPassword（USM 允许）。界面按凭据掩码，CLI 走 UIC_SNMP_PRIV_PASSWORD"},
			{Name: "labels.engineID", Label: "引擎 ID", Type: "string", Scope: "device",
				Placeholder: "80001f888000000002",
				Help:        "v3 可选；十六进制。留空则自动发现（首次多一次往返），填了则固定但写错会明确报错"},
			{Name: "labels.options", Label: "高级选项", Type: "string", Default: "", Scope: "device",
				Help: "retries=0..10、maxOids=1..128（每帧 OID 上限，实际还受 1400 字节预算约束）"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.path", Label: "OID", Type: "string", Required: true, Scope: "point",
				Placeholder: "1.3.6.1.2.1.1.1.0",
				Help:        "点分十进制对象标识符，通常以实例号结尾（.0 表示标量实例）"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true, Scope: "point",
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "string", "bytes"},
				Help: "决定 varbind 的线类型如何翻译：数值型接受 INTEGER/Counter/Gauge/TimeTicks，string/bytes 接受 OCTET STRING"},
		},
		Description: "SNMP 主站（v1/v2c/v3）：按 OID 批量 GET 与单点 SET，写 unsigned 用 Gauge32、写 string 用 OCTET STRING；v3 支持 USM 认证与加密",
	}
}

// Options 是设备级参数。
type Options struct {
	Community string
	Version   snmpcodec.Version
	Retries   int
	MaxOids   int

	// v3 字段。只在 Version==V3 时使用；非 v3 时这些字段被忽略，
	// 这样同一份配置切换版本不会「残留的 v3 参数影响 v2c」。
	Username     string
	AuthProtocol snmpcodec.AuthProtocol
	AuthPassword string
	PrivProtocol snmpcodec.PrivProtocol
	PrivPassword string
	// FixedEngineID 非空时跳过引擎发现（运维已知 engineID）。
	FixedEngineID []byte
}

// Open 建立 UDP 套接字并解析设备参数。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "udp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("snmp requires udp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// 只在「没有冒号」时补默认端口；IPv6 字面量必须带方括号，
		// 其它写法交给 ResolveUDPAddr 保留原始错误。
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, snmpcodec.DefaultPort)
		}
	}
	opt, err := parseOptions(cfg.Device)
	if err != nil {
		return nil, err
	}
	// v3 的口令长度与算法组合在**拨号之前**校验：配置错了就不该占用
	// 一个套接字，更不该让 Probe 去撞一个永远对不上的摘要。
	var v3 *v3Session
	if opt.Version == snmpcodec.V3 {
		v3, err = newV3Session(cfg.Device, opt)
		if err != nil {
			return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
		}
	}
	conn, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	return &connState{
		cfg:       cfg,
		conn:      conn,
		addr:      addr,
		community: opt.Community,
		version:   opt.Version,
		retries:   opt.Retries,
		maxOids:   opt.MaxOids,
		maxSize:   maxRequestBytes,
		v3:        v3,
	}, nil
}

// parseOptions 读取设备标签里的 community / version / options。
func parseOptions(dev model.Device) (Options, error) {
	opt := Options{
		Community: defaultCommunity,
		Version:   snmpcodec.V2c,
		Retries:   defaultRetries,
		MaxOids:   defaultMaxOids,
	}
	if dev.Labels == nil {
		return opt, nil
	}
	if s, ok := dev.Labels["community"]; ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				"labels.community 为空；v1/v2c 必须有团体名", false, nil)
		}
		if len(s) > 255 {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("labels.community 长度 %d 超过 255 字节", len(s)), false, nil)
		}
		opt.Community = s
	}
	if s, ok := dev.Labels["version"]; ok {
		v, err := snmpcodec.ParseVersion(s)
		if err != nil {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		opt.Version = v
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
		case "retries":
			if !hasVal {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.retries 需要 =N", false, nil)
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 10 {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.retries %q 必须是 0..10", val), false, nil)
			}
			opt.Retries = n
		case "maxoids":
			if !hasVal {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.maxOids 需要 =N", false, nil)
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > 128 {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.maxOids %q 必须是 1..128", val), false, nil)
			}
			opt.MaxOids = n
		default:
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("未知的 options 项 %q（只支持 retries / maxOids；engineID 是 labels 顶层的独立字段）", key), false, nil)
		}
	}

	// v3 参数：用户名/认证/加密。放在 labels 顶层而不是 options 里，因为
	// 它们是安全参数（不是调优参数），配置页需要把它们标成 Secret 单独渲染。
	if opt.Version == snmpcodec.V3 {
		opt.Username = strings.TrimSpace(dev.Labels["username"])
		if opt.Username == "" {
			// USM 允许空用户名（代理上的匿名用户），但空用户名几乎总是
			// 配置漏了。在这里拒绝，避免「用户名忘填」被误诊成「认证失败」。
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				"version 3 requires labels.username (USM security name)", false, nil)
		}
		opt.AuthPassword = dev.Labels["authPassword"]
		opt.PrivPassword = dev.Labels["privPassword"]
		authName := strings.TrimSpace(dev.Labels["authProtocol"])
		privName := strings.TrimSpace(dev.Labels["privProtocol"])
		a, err := snmpcodec.ParseAuthProtocol(authName)
		if err != nil {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		p, err := snmpcodec.ParsePrivProtocol(privName)
		if err != nil {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		opt.AuthProtocol = a
		opt.PrivProtocol = p
		// 只在给了加密算法时要求加密口令；authPriv 缺 privPassword 时
		// newV3Session 会回退到 authPassword，这是常见做法（很多代理只
		// 配一个口令）。但「配了 priv 却两个口令都空」必须在这里拒绝。
		if p != snmpcodec.PrivNone && a == snmpcodec.AuthNone {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				"privProtocol 需要 authProtocol（USM 不允许只加密不认证）", false, nil)
		}
		if s, ok := dev.Labels["engineID"]; ok {
			s = strings.TrimSpace(s)
			if s != "" {
				id, err := parseEngineID(s)
				if err != nil {
					return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
				}
				opt.FixedEngineID = id
			}
		}
	}
	return opt, nil
}

// parseEngineID 解析配置里的 engineID 写法：十六进制，可用 `0x` 前缀与
// 冒号/空格分隔（运维通常从代理的配置页或抓包里抄一段十六进制）。
//
// 拒绝奇数长度的十六进制：任何一半字节的 engineID 都是抄漏了一位。
func parseEngineID(s string) ([]byte, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "0x")
	t = strings.TrimPrefix(t, "0X")
	t = strings.NewReplacer(":", "", " ", "", "-", "").Replace(t)
	if len(t) < 10 || len(t) > 64 {
		return nil, fmt.Errorf("engineID %q: hex length %d, must be 10..64 chars (5..32 bytes)", s, len(t))
	}
	if len(t)%2 != 0 {
		return nil, fmt.Errorf("engineID %q: odd number of hex digits", s)
	}
	out := make([]byte, len(t)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexVal(t[i*2])
		lo, ok2 := hexVal(t[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("engineID %q: %q is not hexadecimal", s, t[i*2:i*2+2])
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

// connState 是一条链路上的会话状态。
//
// v1/v2c 的所有状态就是 community；v3 需要一整套 USM 状态（engineID、
// engineBoots/engineTime、本地化密钥、salt 计数器），所以 v3 的状态单独放在
// v3 子结构里——两个版本的字段分开，才不会在「只需要 community」的路径上
// 误用密钥。
type connState struct {
	cfg       driver.OpenConfig
	conn      transport.Datagram
	addr      string
	community string
	version   snmpcodec.Version
	retries   int
	maxOids   int
	// maxSize 是我们在 msgMaxSize 里声明的接收能力。v1/v2c 不用它；
	// v3 必须有（代理按它裁剪响应），取与请求预算相同的 1400。
	maxSize int

	mu    sync.Mutex
	reqID int32

	// v3 只在 version==V3 时非 nil。discover() 负责初始化。
	v3 *v3Session
}

func (c *connState) Close() error { return c.conn.Close() }

// nextID 返回下一个非零 request-id。
func (c *connState) nextID() int32 {
	c.reqID++
	if c.reqID == 0 {
		c.reqID = 1
	}
	return c.reqID
}

// Probe 读 sysDescr.0，收到任何应答即认为链路通。
//
// 为什么 noSuchObject 也算通：它证明报文到达了代理、community 被接受、
// 代理回了应答。真正失败只有两种——没有任何报文（超时）或报文结构错，
// 两者都会在 exchange 里变成分类错误。
//
// v3 的 Probe 顺带完成引擎发现：发现是链路的一部分，不做的话第一条真
// 请求必然吃一个 Report 往返。
func (c *connState) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.version == snmpcodec.V3 {
		return c.probeV3(ctx)
	}
	id := c.nextID()
	req, err := snmpcodec.GetRequest(c.version, c.community, id, []snmpcodec.OID{sysDescrOID})
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	if _, err := c.exchange(ctx, req, id, "probe"); err != nil {
		if driver.KindOf(err) == driver.ErrTimeout {
			return driver.E(driver.ErrTimeout, "probe", c.cfg.Device.ID,
				"no response from agent; check community and view/ACL", true, err)
		}
		return err
	}
	return nil
}

func (c *connState) deadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.cfg.Channel.EffectiveTimeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		return dl
	}
	return d
}

func (c *connState) readError(op string, err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID,
			"no response within timeout", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "udp read failed", true, err)
}

// exchange 发送一条报文并等待 request-id 相同的 Response。
//
// v1/v2c 用这条：报文结构由 community 承载，无需额外处理。
func (c *connState) exchange(ctx context.Context, payload []byte, id int32, op string) (snmpcodec.Message, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return snmpcodec.Message{}, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
		}
		if err := c.conn.SetDeadline(c.deadline(ctx)); err != nil {
			return snmpcodec.Message{}, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"set udp deadline", true, err)
		}
		if err := c.conn.Write(payload); err != nil {
			return snmpcodec.Message{}, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"udp write failed", true, err)
		}
		for {
			datagram, err := c.conn.Read()
			if err != nil {
				lastErr = c.readError(op, err, ctx)
				break
			}
			m, perr := snmpcodec.Parse(datagram)
			if perr != nil {
				return snmpcodec.Message{}, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"malformed SNMP datagram: "+perr.Error(), false, perr)
			}
			// 只接受与我们 request-id 配对的 Response：UDP 上可能收到
			// 上一次事务迟到的应答，也可能收到设备发的 Trap。
			if m.PDU.Type != snmpcodec.PDUResponse || m.PDU.RequestID != id {
				continue
			}
			return m, nil
		}
		// 只有超时值得重发；协议/链路错误立即上报。
		if driver.KindOf(lastErr) != driver.ErrTimeout || ctx.Err() != nil {
			return snmpcodec.Message{}, lastErr
		}
	}
	return snmpcodec.Message{}, lastErr
}

// readV3Datagram 读一条可解析为 v3 的报文。
//
// 只做「结构上是不是 v3」的判断，**不做 request-id 配对**：加密应答的
// request-id 在密文里，读这一层还没解密。配对交给调用方在解密之后做
// （见 v3Round 的 matchID）。把配对放在这里会导致「所有加密应答都被
// 当成不匹配的报文丢掉」，症状是「authPriv 永远超时、authNoPriv 正常」。
//
// 这里**不重试**，也不负责重发：重发必须同时重设截止时间并重新组帧（新
// 的 salt），那是 v3Round 的职责。早先这里有一个「只 Read 不 Write」的
// 尝试循环——第一次超时后截止时间已经过去，后续每次 Read 立刻返回，既
// 不重发也不延长等待，唯一的效果是让超时路径多空转几次、并把「重发」
// 这件事伪装成已经实现。
func (c *connState) readV3Datagram(ctx context.Context, op string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
	}
	datagram, err := c.conn.Read()
	if err != nil {
		return nil, c.readError(op, err, ctx)
	}
	if _, perr := snmpcodec.ParseV3(datagram); perr != nil {
		// 可能是 v1/v2c 应答：给一个更明确的诊断（「代理说的不是
		// v3」比「malformed v3」有用）。
		if _, cperr := snmpcodec.Parse(datagram); cperr == nil {
			return nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
				"agent replied with a v1/v2c message, but v3 was configured", false, nil)
		}
		return nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
			"malformed v3 datagram: "+perr.Error(), false, perr)
	}
	return datagram, nil
}

// sendDatagram 设置期限并发出一个报文。
func (c *connState) sendDatagram(ctx context.Context, payload []byte, op string) error {
	if err := ctx.Err(); err != nil {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
	}
	if err := c.conn.SetDeadline(c.deadline(ctx)); err != nil {
		return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "set udp deadline", true, err)
	}
	if err := c.conn.Write(payload); err != nil {
		return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "udp write failed", true, err)
	}
	return nil
}

// transact 是 v3 的请求入口：处理发现、认证/加密、Report 分类与重试。
//
// op 是 PDU 类型（GetRequestPDU/SetRequestPDU 等构造好的 PDU 传进来）。
// 返回的 PDU 是应答或 nil（Report 已被处理）。
func (c *connState) transact(ctx context.Context, pdu snmpcodec.PDU, op string) (snmpcodec.PDU, error) {
	if c.v3 == nil {
		return snmpcodec.PDU{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, "v3 session not initialized", false, nil)
	}
	s := c.v3
	if err := s.ensureReady(ctx, c); err != nil {
		return snmpcodec.PDU{}, err
	}
	keys := s.currentKeys()
	if keys == nil {
		return snmpcodec.PDU{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, "v3 keys not ready", false, nil)
	}
	// 最多两轮：第一轮可能拿到 Report（时间窗口/重启），刷新后再试一次。
	var lastErr error
	for round := 0; round < 2; round++ {
		resp, err := c.v3Round(ctx, pdu, keys, op)
		if err != nil {
			// 应答的引擎状态与缓存不符（代理重启 / 报文重放 / 时钟跑偏）。
			// 刷新基准后重试一次；再不符就判协议错，不能静默接受——
			// 那等于把「重放防护」整条关掉。
			var stale *staleEngineError
			if errors.As(err, &stale) {
				if round == 1 {
					return snmpcodec.PDU{}, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
						"agent keeps reporting a different engine state: "+stale.Error(), false, stale)
				}
				// 时间取「报告值与本地估计的较大者」：重放报文的时间偏旧，
				// 用它覆盖缓存只会让下一轮再被判坏；boots 则必须采信报告值
				// （代理重启后旧 boots 一定作废）。
				t := stale.engineTime
				if cur := s.estimatedEngineTime(); t < cur {
					t = cur
				}
				s.setEngine(stale.engineID, stale.engineBoots, t)
				if err := s.deriveKeys(); err != nil {
					return snmpcodec.PDU{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, err.Error(), false, err)
				}
				s.keysReady.Store(true)
				keys = s.currentKeys()
				continue
			}
			return snmpcodec.PDU{}, err
		}
		if resp.pdu != nil {
			return *resp.pdu, nil
		}
		// Report：分类处理。
		kind := snmpcodec.ClassifyReport(*resp.report)
		switch kind {
		case snmpcodec.ReportNotInTimeWindow, snmpcodec.ReportUnknownEngineID:
			// 时间/引擎状态失效：重新发现（会刷新 boots/time 或 engineID），
			// 下一轮重试。这是唯一「值得多一轮」的 Report。
			if round == 1 {
				return snmpcodec.PDU{}, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"agent still reports time/engine mismatch after re-discovery", false, nil)
			}
			// 两种失效的重发现起点不同：
			//
			//   - notInTimeWindow：引擎身份还是对的，只是时间基准过期。
			//     保留 engineID，只重学 boots/time。
			//   - unknownEngineID：代理明确说不认识我们手里的 engineID
			//     （它重启/重装/恢复了旧配置）。必须先丢掉旧身份回到空
			//     engineID 重做发现——拿旧身份再算一遍密钥，代理只会再回
			//     一次同一个 Report，两次之后我们报「re-discovery 也没用」，
			//     而真正要做的事是重新问一遍「你是谁」。
			if kind == snmpcodec.ReportUnknownEngineID {
				s.resetEngine()
			}
			s.keysReady.Store(false)
			if err := s.ensureReady(ctx, c); err != nil {
				return snmpcodec.PDU{}, err
			}
			keys = s.currentKeys()
			lastErr = nil
			continue
		case snmpcodec.ReportWrongDigest, snmpcodec.ReportUnknownUserName, snmpcodec.ReportUnsupportedSecLevel:
			// 认证/用户/安全级别问题：重试无意义，直接判权限错误。
			return snmpcodec.PDU{}, driver.E(driver.ErrPermission, op, c.cfg.Device.ID, kind.Hint(), false, nil)
		case snmpcodec.ReportDecryptionError:
			return snmpcodec.PDU{}, driver.E(driver.ErrPermission, op, c.cfg.Device.ID, kind.Hint(), false, nil)
		default:
			return snmpcodec.PDU{}, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
				"unrecognized Report PDU", false, nil)
		}
	}
	return snmpcodec.PDU{}, lastErr
}

// v3RoundResult 是一次 v3 往返的结果。
//
// pdu 与 report 二选一：成功时 pdu 非 nil；收到 Report 时 report 非 nil。
// 分成两个字段而不是用「PDU.Type == Report」判断，是因为 Report 的 varbind
// 是 usmStats 计数器，与应答 varbitd 的语义完全不同，混用一个字段容易在
// 上层误把计数器当业务值。
type v3RoundResult struct {
	pdu    *snmpcodec.PDU
	report *snmpcodec.PDU
}

// v3Round 发一条 v3 请求并解析应答。
//
// 配对（request-id）放在解密之后：加密应答的 request-id 在密文里，
// 读这一层只能看到密文。这条顺序是 v3 与 v1/v2c 最容易搞错的地方——
// 先把报文读出来、解密、再配对，才既不会漏掉 authPriv 的应答，也不会
// 把迟到的旧应答当成当前结果。
func (c *connState) v3Round(ctx context.Context, pdu snmpcodec.PDU, keys *snmpcodec.V3Keys, op string) (v3RoundResult, error) {
	s := c.v3
	id := c.nextID()
	pdu.RequestID = id
	hdr := snmpcodec.HeaderData{
		MsgID:    s.nextMsgID(),
		MaxSize:  c.maxSize,
		Flags:    s.flags(),
		SecModel: snmpcodec.SecurityModelUSM,
	}
	sec := snmpcodec.USMSecurityParameters{
		EngineID:    append([]byte(nil), s.engineID...),
		EngineBoots: s.engineBoots,
		EngineTime:  s.estimatedEngineTime(),
		UserName:    s.user,
	}
	scoped, err := snmpcodec.BuildScopedPDU(s.engineID, "", pdu)
	if err != nil {
		return v3RoundResult{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, err.Error(), false, err)
	}
	for attempt := 0; attempt <= c.retries; attempt++ {
		// 每一轮都重新组帧（也就是重新加密）。**不能**把 BuildV3 提到循环
		// 外面复用同一份密文：AES-CFB/DES-CBC 的安全性要求同一把密钥下
		// salt（进而 IV）绝不重复（RFC 3414 §8、RFC 3826 §3.1）。重发同一
		// 份密文等于用同一个 (key, IV) 加密同一段明文两次——攻击者拿到两份
		// 相同报文立刻知道这是重传，而若两次明文不同（这里相同）就会泄露
		// 明文异或。salt 由 V3Keys.Salt() 单调推进，重新组帧天然满足唯一性。
		raw, err := snmpcodec.BuildV3(hdr, sec, scoped, keys)
		if err != nil {
			return v3RoundResult{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, err.Error(), false, err)
		}
		if err := c.sendDatagram(ctx, raw, op); err != nil {
			return v3RoundResult{}, err
		}
		res, err := c.readV3Round(ctx, id, keys, op)
		if err != nil {
			// 超时才重发（与 exchange 同一条纪律）：协议错、验签失败、
			// 解密失败重发一万次结果相同，只会把一次可诊断的失败拖成超时。
			//
			// 这段以前不存在——readV3Datagram 内部有个只读不写的「重试」
			// 循环，而外层这个分支永远拿不到 nil，于是 v3 的 retries 配置
			// 实际完全无效：UDP 丢一个包就是一轮采集失败。丢掉一个包在
			// 现场是常态，而这一条只在真实网络里暴露。
			if driver.KindOf(err) == driver.ErrTimeout && attempt < c.retries && ctx.Err() == nil {
				continue
			}
			return v3RoundResult{}, err
		}
		return *res, nil
	}
	return v3RoundResult{}, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID,
		"no matching v3 response after retries", true, nil)
}

// readV3Round 读一条属于本次请求的 v3 报文；返回 nil 表示只收到无关报文。
func (c *connState) readV3Round(ctx context.Context, id int32, keys *snmpcodec.V3Keys, op string) (*v3RoundResult, error) {
	for {
		datagram, err := c.readV3Datagram(ctx, op)
		if err != nil {
			return nil, err
		}
		msg, perr := snmpcodec.ParseV3(datagram)
		if perr != nil {
			return nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
				"malformed v3 datagram: "+perr.Error(), false, perr)
		}
		// 认证校验必须放在解密与配对之前：伪造的 Report 可以随便填
		// request-id，只要签名不对就应当直接拒绝。
		//
		// 判据用「应答自己带了认证」而不是「本端请求带了认证」：代理回
		// Report 时通常不带认证（发现阶段、摘要失败都如此），而带认证的
		// 应答必须验签。反过来用本端 flags 判断会把不带认证的 Report 也
		// 拿去验签，得到「auth parameters are 0 bytes」的假失败。
		if msg.Header.Flags.HasAuth() {
			if err := snmpcodec.VerifyV3(datagram, msg.Security, keys.Auth, keys.AuthKey); err != nil {
				return nil, driver.E(driver.ErrPermission, op, c.cfg.Device.ID, err.Error(), false, err)
			}
		}
		// 解密（若是密文）。
		if msg.Encrypted {
			if err := msg.DecryptScoped(keys.Priv, keys.PrivKey); err != nil {
				return nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"decrypt response: "+err.Error(), false, err)
			}
		}
		// 现在是明文 PDU，可以配对。
		//
		// Report 的 request-id 可能是 0：加密请求在认证/解密失败时，
		// 代理读不到我们的 request-id（它在密文里），RFC 3412 §7.1 允许
		// 此时填 0。拒收这种 Report 会让「认证配错」表现为超时。
		if msg.PDU.Type == snmpcodec.PDUReport {
			if msg.PDU.RequestID != id && msg.PDU.RequestID != 0 {
				continue
			}
			p := msg.PDU
			return &v3RoundResult{report: &p}, nil
		}
		if msg.PDU.Type != snmpcodec.PDUResponse || msg.PDU.RequestID != id {
			// 迟到的旧应答 / Trap：继续读。
			continue
		}
		// 配对之后再校验引擎身份与时间窗口（RFC 3414 §3.2）。**只查
		// Response**：Report 的职责恰恰是带来新的 boots/time（尤其
		// notInTimeWindow），把它挡掉会让恢复路径失效。
		if stale := c.v3.staleEngine(msg.Security); stale != nil {
			return nil, stale
		}
		p := msg.PDU
		return &v3RoundResult{pdu: &p}, nil
	}
}

// flags 返回本次请求的安全级别标志。
func (s *v3Session) flags() snmpcodec.MsgFlags {
	f := snmpcodec.FlagReportable
	if s.auth != snmpcodec.AuthNone {
		f |= snmpcodec.FlagAuth
	}
	if s.priv != snmpcodec.PrivNone {
		f |= snmpcodec.FlagPriv
	}
	return f
}

// do 是**唯一**的请求入口：按版本分派到 community 报文或 v3 事务。
//
// 收敛成一个入口的理由：v3 与 v1/v2c 的差别不只在报文封装，还在「一条请求
// 可能要额外往返几次」（发现、时间窗口修复）。若 7 处请求构造各自判断版本，
// 迟早会有一处忘记走 v3 分支，症状是「v3 设备大部分 OID 能读、某一类操作
// 永远超时」——那种问题极难归因。这里只要 pdu 组好，版本差异全部消化掉。
func (c *connState) do(ctx context.Context, pdu snmpcodec.PDU, op string) (snmpcodec.PDU, error) {
	if c.version == snmpcodec.V3 {
		return c.transact(ctx, pdu, op)
	}
	payload, err := snmpcodec.Build(snmpcodec.Message{
		Version:   c.version,
		Community: c.community,
		PDU:       pdu,
	})
	if err != nil {
		return snmpcodec.PDU{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, err.Error(), false, err)
	}
	m, err := c.exchange(ctx, payload, pdu.RequestID, op)
	if err != nil {
		return snmpcodec.PDU{}, err
	}
	return m.PDU, nil
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// readItem 是「点位下标 + 解析后的 OID」。点位下标用于把结果放回原顺序：
// 分批会打乱事务顺序，但调用方看到的样本顺序必须与请求一致。
type readItem struct {
	pos int
	oid snmpcodec.OID
}

func (c *connState) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var points []driver.PointRef
	for _, span := range spans {
		points = append(points, span.Points...)
	}
	out := make([]driver.RawSample, len(points))
	filled := make([]bool, len(points))

	items := make([]readItem, 0, len(points))
	for i, p := range points {
		oid, err := c.oidOf(p)
		if err != nil {
			out[i] = errSample(p.ID, err)
			filled[i] = true
			continue
		}
		items = append(items, readItem{pos: i, oid: oid})
	}

	var firstErr error
	goodSamples := 0
	// 任一链路级失败后，后续批次不可能成功；统一标记并停止。
	failRest := func(from int, err error) {
		if firstErr == nil {
			firstErr = err
		}
		for _, it := range items[from:] {
			if !filled[it.pos] {
				out[it.pos] = errSample(points[it.pos].ID, err)
				filled[it.pos] = true
			}
		}
	}

	batch := make([]readItem, 0, c.maxOids)
	start := 0
	estimate := 48 // 报文头 + PDU 头的粗略上界

	// linkErr 记录链路级失败：它必须绕过下面的部分失败闸门立即抛给调度器。
	var linkErr error
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		batchEnd := start + len(batch)
		res, err := c.readBatch(ctx, points, batch)
		// **先吸收本批已拿到的结果，再看链路错误**：v1 的逐点退化路径可能在
		// 读到一半时超时，前半段的成功样本必须留下。早先这里是「有 err 就
		// 整批标坏」，一次中途超时会把已经读到的值全部丢掉。
		for _, r := range res {
			if r.err != nil {
				out[r.pos] = errSample(points[r.pos].ID, r.err)
				// 地址错误是点表问题，不该成为整轮采集的 firstErr。
				if firstErr == nil && driver.KindOf(r.err) != driver.ErrAddress {
					firstErr = r.err
				}
			} else {
				goodSamples++
				out[r.pos] = driver.RawSample{PointID: points[r.pos].ID, Value: r.value, Quality: model.QualityGood}
			}
			filled[r.pos] = true
		}
		batch = batch[:0]
		estimate = 48
		if err != nil {
			// 链路级失败：本批没有结果的点位与后续所有批次都显式标坏。
			for _, it := range items[start:batchEnd] {
				if !filled[it.pos] {
					out[it.pos] = errSample(points[it.pos].ID, err)
					filled[it.pos] = true
				}
			}
			failRest(batchEnd, err)
			linkErr = err
			return false
		}
		return true
	}

	for i, it := range items {
		size := estimateOID(it.oid) + 4
		if len(batch) > 0 && (len(batch) >= c.maxOids || estimate+size > maxRequestBytes) {
			if !flush() {
				return out, linkErr
			}
			start = i
		}
		batch = append(batch, it)
		estimate += size
	}
	flush()
	if linkErr != nil {
		return out, linkErr
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误。
	return out, driver.PartialReadError(goodSamples, firstErr)
}

// estimateOID 是 OID 编码长度的保守上界：TLV 头 2 字节 + 每子标识符 5 字节。
func estimateOID(o snmpcodec.OID) int {
	return 2 + 5*(len(o)-1) + 2 // 最后 +2 是 varbind 的 SEQUENCE 头
}

type batchResult struct {
	pos   int
	value model.Value
	err   error
}

// readBatch 读一批 OID。返回的 linkErr 表示链路级失败（超时/断链），
// 调用方据此停止后续批次；协议/地址级问题放在 batchResults 里逐点返回。
func (c *connState) readBatch(ctx context.Context, points []driver.PointRef, batch []readItem) ([]batchResult, error) {
	oids := make([]snmpcodec.OID, len(batch))
	for i, it := range batch {
		oids[i] = it.oid
	}
	id := c.nextID()
	req, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, id, oids)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.do(ctx, req, "read")
	if err != nil {
		switch driver.KindOf(err) {
		case driver.ErrTimeout, driver.ErrDisconnected:
			return nil, err
		}
		// 协议/内部错误：整批标记，但不当作链路故障，继续下一批。
		res := make([]batchResult, len(batch))
		for i, it := range batch {
			res[i] = batchResult{pos: it.pos, err: err}
		}
		return res, nil
	}

	if st := snmpcodec.ErrorStatus(resp.ErrorStatus); st != snmpcodec.ErrNoError {
		if c.version == snmpcodec.V1 && st == snmpcodec.ErrNoSuchName {
			return c.readOneByOne(ctx, points, batch)
		}
		e := c.statusError("read", st, nil, resp.ErrorIndex)
		res := make([]batchResult, len(batch))
		for i, it := range batch {
			res[i] = batchResult{pos: it.pos, err: e}
		}
		return res, nil
	}
	if len(resp.Varbinds) != len(batch) {
		e := driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("agent returned %d varbinds for %d OIDs", len(resp.Varbinds), len(batch)), false, nil)
		res := make([]batchResult, len(batch))
		for i, it := range batch {
			res[i] = batchResult{pos: it.pos, err: e}
		}
		return res, nil
	}
	res := make([]batchResult, len(batch))
	for i, it := range batch {
		res[i] = c.decodeVarbind(points[it.pos], it.pos, it.oid, resp.Varbinds[i])
	}
	return res, nil
}

// readOneByOne 是 v1 noSuchName 的退化路径：逐点重发，把「哪一个 OID
// 不存在」与「其余 OID 的值」重新分开。
func (c *connState) readOneByOne(ctx context.Context, points []driver.PointRef, batch []readItem) ([]batchResult, error) {
	res := make([]batchResult, 0, len(batch))
	for i, it := range batch {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
			for _, rest := range batch[i:] {
				res = append(res, batchResult{pos: rest.pos, err: e})
			}
			// 把链路错误交还给调用方：逐点退化可能已经读到几个点位，
			// 但剩下的不可能再成功，继续发下一批只会把一次超时拖成 N 次。
			return res, e
		}
		id := c.nextID()
		req, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, id, []snmpcodec.OID{it.oid})
		if err != nil {
			res = append(res, batchResult{pos: it.pos, err: driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)})
			continue
		}
		resp, err := c.do(ctx, req, "read")
		if err != nil {
			if driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected {
				for _, rest := range batch[i:] {
					res = append(res, batchResult{pos: rest.pos, err: err})
				}
				return res, err
			}
			res = append(res, batchResult{pos: it.pos, err: err})
			continue
		}
		if st := snmpcodec.ErrorStatus(resp.ErrorStatus); st != snmpcodec.ErrNoError {
			res = append(res, batchResult{pos: it.pos, err: c.statusError("read", st, it.oid, resp.ErrorIndex)})
			continue
		}
		if len(resp.Varbinds) != 1 {
			res = append(res, batchResult{pos: it.pos, err: driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
				fmt.Sprintf("single-OID GET returned %d varbinds", len(resp.Varbinds)), false, nil)})
			continue
		}
		res = append(res, c.decodeVarbind(points[it.pos], it.pos, it.oid, resp.Varbinds[0]))
	}
	return res, nil
}

func (c *connState) decodeVarbind(p driver.PointRef, pos int, want snmpcodec.OID, vb snmpcodec.Varbind) batchResult {
	if vb.OID.Compare(want) != 0 {
		return batchResult{pos: pos, err: driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("agent answered OID %s for requested %s", vb.OID, want), false, nil)}
	}
	if vb.Value.IsException() {
		return batchResult{pos: pos, err: driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("OID %s: %s", want, vb.Value.Type), false, nil)}
	}
	v, err := decodeValue(p, vb.Value)
	if err != nil {
		return batchResult{pos: pos, err: err}
	}
	return batchResult{pos: pos, value: v}
}

// decodeValue 按点位数据类型解释 varbind 值。
//
// 关键判断：**线类型由设备决定，而不是由点表决定**。同一个 OID 在
// 不同厂商的设备上可能是 INTEGER、Gauge32 甚至 OCTET STRING；点表的
// dataType 只决定「按什么读」，转换失败必须报错而不是硬塞一个 0。
func decodeValue(p driver.PointRef, v snmpcodec.Value) (model.Value, error) {
	dev := ""
	if p.ID != "" {
		dev = "point " + p.ID + ": "
	}
	switch p.DataType {
	case model.TypeString:
		switch v.Type {
		case snmpcodec.TypeOctetString, snmpcodec.TypeOpaque:
			return model.TextValue(model.TypeString, string(v.Bytes)), nil
		case snmpcodec.TypeOID:
			return model.TextValue(model.TypeString, v.OID.String()), nil
		case snmpcodec.TypeIPAddress:
			return model.TextValue(model.TypeString, net.IP(v.Bytes).String()), nil
		}
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s设备返回 %s，但点位 dataType=string（OCTET STRING/OID/IpAddress 才能按字符串读）", dev, v.Type), false, nil)
	case model.TypeBytes:
		if v.Type != snmpcodec.TypeOctetString && v.Type != snmpcodec.TypeOpaque {
			return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
				"%s设备返回 %s，但点位 dataType=bytes", dev, v.Type), false, nil)
		}
		return model.BytesValue(v.Bytes), nil
	case model.TypeBool:
		switch v.Type {
		case snmpcodec.TypeInteger:
			if v.Int == 0 || v.Int == 1 {
				return model.BoolValue(v.Int == 1), nil
			}
		case snmpcodec.TypeCounter32, snmpcodec.TypeGauge32, snmpcodec.TypeTimeTicks:
			if v.Uint == 0 || v.Uint == 1 {
				return model.BoolValue(v.Uint == 1), nil
			}
		}
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s点位 dataType=bool，但设备返回 %s=%s（只接受 INTEGER/无符号的 0/1）", dev, v.Type, v), false, nil)
	}

	// 数值型：接受所有整数/无符号/计数器；拒绝字符串与地址类。
	var num float64
	switch v.Type {
	case snmpcodec.TypeInteger:
		num = float64(v.Int)
	case snmpcodec.TypeCounter32, snmpcodec.TypeGauge32, snmpcodec.TypeTimeTicks, snmpcodec.TypeCounter64:
		// Counter64 经 float64 承载：超过 2^53 会丢精度。平台的数值通道
		// 是 float64，这在设计上就无法避免；文档的边界一节明确写出。
		num = float64(v.Uint)
	case snmpcodec.TypeOctetString, snmpcodec.TypeOpaque:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s设备返回 OCTET STRING（%q），但点位是数值型；把 dataType 改成 string/bytes，或在设备侧换用数值 OID",
			dev, truncate(v.Bytes, 32)), false, nil)
	case snmpcodec.TypeIPAddress, snmpcodec.TypeOID:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s设备返回 %s，不能作为数值；把 dataType 改成 string", dev, v.Type), false, nil)
	case snmpcodec.TypeNull:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", dev+"设备返回 NULL", false, nil)
	default:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s不支持的 varbind 类型 %s", dev, v.Type), false, nil)
	}
	if math.IsNaN(num) || math.IsInf(num, 0) {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", dev+"数值不是有限值", false, nil)
	}
	return model.NumValue(p.DataType, num), nil
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *connState) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		if err := ctx.Err(); err != nil {
			out = append(out, driver.WriteResult{PointID: req.PointID,
				Err: driver.E(driver.ErrTimeout, "write", c.cfg.Device.ID, "context done", true, err)})
			continue
		}
		out = append(out, c.writeOne(ctx, req))
	}
	return out, nil
}

func (c *connState) writeOne(ctx context.Context, req driver.WriteRequest) driver.WriteResult {
	res := driver.WriteResult{PointID: req.PointID}
	// 失败路径必须带质量位。留空会让上层各自补默认值（collector 补 bad），
	// 于是「超时」「地址写错」「设备拒绝」在下游看起来一模一样——现场排查
	// 时最需要区分的恰恰就是这三类。读路径的 errSample 一直是这样做的，
	// 写路径不能例外。
	fail := func(err error) driver.WriteResult {
		res.Err = err
		res.Quality = qualityOf(err)
		return res
	}
	if req.Address.Kind != model.AddressSymbol {
		return fail(driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: snmp requires symbol (OID) addressing, got %q", req.PointID, req.Address.Kind), false, nil))
	}
	oid, err := parseOID(req.Address)
	if err != nil {
		return fail(err)
	}
	val, err := encodeWriteValue(req)
	if err != nil {
		return fail(err)
	}
	id := c.nextID()
	pdu, err := snmpcodec.NewSetPDU(id, []snmpcodec.Varbind{{OID: oid, Value: val}})
	if err != nil {
		return fail(driver.E(driver.ErrInternal, "write", c.cfg.Device.ID, err.Error(), false, err))
	}
	resp, err := c.do(ctx, pdu, "write")
	if err != nil {
		return fail(err)
	}
	if st := snmpcodec.ErrorStatus(resp.ErrorStatus); st != snmpcodec.ErrNoError {
		return fail(c.statusError("write", st, oid, resp.ErrorIndex))
	}
	if len(resp.Varbinds) != 1 || resp.Varbinds[0].OID.Compare(oid) != 0 {
		return fail(driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			"SET response does not echo the written OID", false, nil))
	}
	// 标准要求 SET 应答回显写入后的值。校验它能把「设备接受了请求
	// 但按自己的解释改了值」（例如把 1 写成 100）当场暴露出来。
	if !valueEqual(resp.Varbinds[0].Value, val) {
		return fail(driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("SET echoed %s, want %s (device may have coerced the value)",
				resp.Varbinds[0].Value, val), false, nil))
	}
	res.OK = true
	res.Quality = model.QualityGood
	return res
}

// encodeWriteValue 把平台值翻译成 varbind 值。
//
// 映射规则（文档 §3.10.12 有同一张表）：
//   - bool → INTEGER 0/1
//   - int16/int32/int64 → INTEGER
//   - uint16/uint32 → Gauge32（无符号的可写类型）
//   - string/bytes → OCTET STRING
//
// float 与 BCD 显式拒绝：SNMP 没有浮点标准类型，各厂商的私有用
// OCTET STRING 编法互不兼容，猜一个只会写出错误的值。
func encodeWriteValue(req driver.WriteRequest) (snmpcodec.Value, error) {
	dev := ""
	if req.PointID != "" {
		dev = "point " + req.PointID + ": "
	}
	bad := func(format string, args ...any) (snmpcodec.Value, error) {
		return snmpcodec.Value{}, driver.E(driver.ErrUnsupported, "write", "",
			dev+fmt.Sprintf(format, args...), false, nil)
	}
	switch req.DataType {
	case model.TypeBool:
		if req.Value.Bool {
			return snmpcodec.Integer(1), nil
		}
		return snmpcodec.Integer(0), nil
	case model.TypeInt16:
		n, err := intValue(req.Value, math.MinInt16, math.MaxInt16)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Integer(n), nil
	case model.TypeInt32:
		n, err := intValue(req.Value, math.MinInt32, math.MaxInt32)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Integer(n), nil
	case model.TypeInt64:
		n, err := intValue(req.Value, math.MinInt64, math.MaxInt64)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Integer(n), nil
	case model.TypeUint16:
		n, err := uintValue(req.Value, 0xFFFF)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Unsigned(snmpcodec.TypeGauge32, n), nil
	case model.TypeUint32:
		n, err := uintValue(req.Value, 0xFFFFFFFF)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Unsigned(snmpcodec.TypeGauge32, n), nil
	case model.TypeUint64:
		n, err := uintValue(req.Value, 0xFFFFFFFF)
		if err != nil {
			return bad("%v（SNMP 没有 64 位可写无符号整型；大于 2^32-1 的值只能只读）", err)
		}
		return snmpcodec.Unsigned(snmpcodec.TypeGauge32, n), nil
	case model.TypeString:
		return snmpcodec.String(req.Value.Text), nil
	case model.TypeBytes:
		return snmpcodec.Octets([]byte(req.Value.Text)), nil
	case model.TypeFloat32, model.TypeFloat64:
		return bad("SNMP 没有标准浮点类型；请改用整型 OID，或在设备侧确认私有编码")
	case model.TypeBCD16, model.TypeBCD32:
		return bad("SNMP 没有 BCD 类型；请改用整型 OID")
	}
	return bad("不支持的数据类型 %q", req.DataType)
}

func intValue(v model.Value, min, max int64) (int64, error) {
	if v.Type != model.TypeBool && !v.Type.IsNumeric() {
		return 0, fmt.Errorf("值类型 %q 不是数值", v.Type)
	}
	num, ok := v.AsFloat()
	if !ok {
		return 0, fmt.Errorf("值类型 %q 不能作为数值", v.Type)
	}
	if num != math.Trunc(num) {
		return 0, fmt.Errorf("需要整数值，得到 %v", num)
	}
	// float64 对 int64 两端并不精确；用边界比较时留出让 Convert 安全的
	// 判断空间（超出 [min,max] 一律拒绝，不做饱和截断）。
	if num < float64(min) || num > float64(max) {
		return 0, fmt.Errorf("值 %v 超出范围 %d..%d", num, min, max)
	}
	return int64(num), nil
}

func uintValue(v model.Value, max uint64) (uint64, error) {
	if v.Type == model.TypeBool {
		if v.Bool {
			return 1, nil
		}
		return 0, nil
	}
	if !v.Type.IsNumeric() {
		return 0, fmt.Errorf("值类型 %q 不是数值", v.Type)
	}
	num, ok := v.AsFloat()
	if !ok {
		return 0, fmt.Errorf("值类型 %q 不能作为数值", v.Type)
	}
	if num != math.Trunc(num) {
		return 0, fmt.Errorf("需要整数值，得到 %v", num)
	}
	if num < 0 || num > float64(max) {
		return 0, fmt.Errorf("值 %v 超出范围 0..%d", num, max)
	}
	return uint64(num), nil
}

func valueEqual(a, b snmpcodec.Value) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case snmpcodec.TypeInteger:
		return a.Int == b.Int
	case snmpcodec.TypeCounter32, snmpcodec.TypeGauge32, snmpcodec.TypeTimeTicks,
		snmpcodec.TypeCounter64:
		return a.Uint == b.Uint
	case snmpcodec.TypeOctetString, snmpcodec.TypeOpaque, snmpcodec.TypeIPAddress:
		return bytes.Equal(a.Bytes, b.Bytes)
	case snmpcodec.TypeOID:
		return a.OID.Compare(b.OID) == 0
	case snmpcodec.TypeNull:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 自检接口
// ---------------------------------------------------------------------------
//
// GetRaw / Walk 只服务于 `uic snmpread`：自检时我们**不知道**点表的
// dataType，也不该猜一个——猜错会把 OCTET STRING 当数值读成 0。因此它们
// 直接返回线类型与原始值，由 CLI 原样展示。
//
// 它们不进入 driver.Conn 接口：生产采集必须走 Read 的类型化路径，否则
// 会绕过「线类型与点表不符就报错」这道检查。

// GetResult 是一组自检读结果。
type GetResult struct {
	OID   snmpcodec.OID
	Value snmpcodec.Value
	Err   error
}

// GetRaw 以原始 varbind 形式读一组 OID。
func (c *connState) GetRaw(ctx context.Context, oids []snmpcodec.OID) ([]GetResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(oids) == 0 {
		return nil, driver.E(driver.ErrUnsupported, "read", c.cfg.Device.ID, "at least one OID is required", false, nil)
	}

	out := make([]GetResult, 0, len(oids))
	for start := 0; start < len(oids); {
		end := start + c.maxOids
		if end > len(oids) {
			end = len(oids)
		}
		// 字节预算与 Read 相同：先按保守上界切，避免一条大 GET 被分片。
		for end > start+1 {
			total := 48
			for _, o := range oids[start:end] {
				total += estimateOID(o) + 4
			}
			if total <= maxRequestBytes {
				break
			}
			end--
		}
		res, linkErr := c.getRawBatch(ctx, oids[start:end])
		out = append(out, res...)
		if linkErr != nil {
			for _, o := range oids[end:] {
				out = append(out, GetResult{OID: o, Err: linkErr})
			}
			return out, linkErr
		}
		start = end
	}
	return out, nil
}

func (c *connState) getRawBatch(ctx context.Context, oids []snmpcodec.OID) ([]GetResult, error) {
	id := c.nextID()
	req, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, id, oids)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.do(ctx, req, "read")
	if err != nil {
		switch driver.KindOf(err) {
		case driver.ErrTimeout, driver.ErrDisconnected:
			return nil, err
		}
		out := make([]GetResult, len(oids))
		for i, o := range oids {
			out[i] = GetResult{OID: o, Err: err}
		}
		return out, nil
	}
	if st := snmpcodec.ErrorStatus(resp.ErrorStatus); st != snmpcodec.ErrNoError {
		if c.version == snmpcodec.V1 && st == snmpcodec.ErrNoSuchName {
			// 自检时逐点重发，才能把「哪个 OID 不存在」说清楚。
			out := make([]GetResult, 0, len(oids))
			for i, o := range oids {
				id := c.nextID()
				one, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, id, []snmpcodec.OID{o})
				if err != nil {
					out = append(out, GetResult{OID: o, Err: err})
					continue
				}
				r, err := c.do(ctx, one, "read")
				if err != nil {
					if driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected {
						for _, rest := range oids[i:] {
							out = append(out, GetResult{OID: rest, Err: err})
						}
						return out, err
					}
					out = append(out, GetResult{OID: o, Err: err})
					continue
				}
				if st := snmpcodec.ErrorStatus(r.ErrorStatus); st != snmpcodec.ErrNoError {
					out = append(out, GetResult{OID: o, Err: c.statusError("read", st, o, r.ErrorIndex)})
					continue
				}
				if len(r.Varbinds) != 1 {
					out = append(out, GetResult{OID: o, Err: driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
						"single-OID GET returned a different varbind count", false, nil)})
					continue
				}
				out = append(out, GetResult{OID: r.Varbinds[0].OID, Value: r.Varbinds[0].Value})
			}
			return out, nil
		}
		e := c.statusError("read", st, nil, resp.ErrorIndex)
		out := make([]GetResult, len(oids))
		for i, o := range oids {
			out[i] = GetResult{OID: o, Err: e}
		}
		return out, nil
	}
	if len(resp.Varbinds) != len(oids) {
		e := driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("agent returned %d varbinds for %d OIDs", len(resp.Varbinds), len(oids)), false, nil)
		out := make([]GetResult, len(oids))
		for i, o := range oids {
			out[i] = GetResult{OID: o, Err: e}
		}
		return out, nil
	}
	out := make([]GetResult, len(oids))
	for i := range oids {
		out[i] = GetResult{OID: resp.Varbinds[i].OID, Value: resp.Varbinds[i].Value}
	}
	return out, nil
}

// bulkMaxRepetitions 是 Walk 一次 GetBulk 取的后继条数。
// 25 是保守值：足够把一张 200 行表的往返数从 200 降到 8，又不至于让
// 小内存代理回包过大（agent 可以直接回 tooBig，那 Walk 就退化了）。
const bulkMaxRepetitions = 25

// Walk 从 prefix 的后继开始按字典序遍历，直到离开该子树或达到 max。
//
// 只给自检用：生产点表必须是明确的 OID，Walk 出来的名字没有 MIB 也
// 无法赋予业务含义。
//
// v2c/v3 走 GetBulk（一次往返批量取后继）；v1 没有 GetBulk，保持
// GETNEXT 逐条。codec 的 GetBulkRequest 与内置从站的 GETBULK 应答
// 早已就位，此前主站一直没接——大表遍历退化成 N 次往返。
func (c *connState) Walk(ctx context.Context, prefix snmpcodec.OID, max int) ([]GetResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := prefix.Validate(); err != nil {
		return nil, driver.E(driver.ErrAddress, "walk", c.cfg.Device.ID, err.Error(), false, err)
	}
	if max <= 0 {
		max = 128
	}
	if max > 1024 {
		max = 1024
	}
	if c.version >= snmpcodec.V2c {
		return c.walkBulk(ctx, prefix, max)
	}
	return c.walkNext(ctx, prefix, max)
}

// walkBulk 用 GetBulk 遍历：一次往返取 max-repetitions 条后继。
//
// 应答里的 EndOfMibView 与离开子树的 OID 都按「遍历结束」处理（GetBulk
// 会越过子树边界继续取后继，越界项不能进结果）；代理不前进是协议错。
func (c *connState) walkBulk(ctx context.Context, prefix snmpcodec.OID, max int) ([]GetResult, error) {
	var out []GetResult
	cur := prefix
	for len(out) < max {
		if err := ctx.Err(); err != nil {
			return out, driver.E(driver.ErrTimeout, "walk", c.cfg.Device.ID, "context done", true, err)
		}
		id := c.nextID()
		reps := bulkMaxRepetitions
		if n := max - len(out); n < reps {
			reps = n
		}
		pdu := snmpcodec.PDU{
			Type:           snmpcodec.PDUGetBulk,
			RequestID:      id,
			MaxRepetitions: reps,
			Varbinds:       []snmpcodec.Varbind{{OID: cur, Value: snmpcodec.Null()}},
		}
		resp, err := c.do(ctx, pdu, "walk")
		if err != nil {
			return out, err
		}
		if st := snmpcodec.ErrorStatus(resp.ErrorStatus); st != snmpcodec.ErrNoError {
			return out, c.statusError("walk", st, cur, resp.ErrorIndex)
		}
		if len(resp.Varbinds) == 0 {
			return out, driver.E(driver.ErrProtocol, "walk", c.cfg.Device.ID,
				"GETBULK returned no varbinds", false, nil)
		}
		for _, vb := range resp.Varbinds {
			if vb.Value.Type == snmpcodec.TypeEndOfMibView {
				return out, nil
			}
			if v := vb.OID.Compare(cur); v <= 0 {
				// 代理返回了一个不前进的 OID：继续循环只会挂死。
				return out, driver.E(driver.ErrProtocol, "walk", c.cfg.Device.ID,
					fmt.Sprintf("GETBULK did not advance past %s (returned %s)", cur, vb.OID), false, nil)
			}
			if !prefix.HasPrefix(vb.OID) {
				return out, nil
			}
			out = append(out, GetResult{OID: vb.OID, Value: vb.Value})
			cur = vb.OID
			if len(out) >= max {
				break
			}
		}
	}
	return out, nil
}

// walkNext 用 GETNEXT 逐条遍历（v1 没有 GetBulk，表尾就是 noSuchName）。
func (c *connState) walkNext(ctx context.Context, prefix snmpcodec.OID, max int) ([]GetResult, error) {
	var out []GetResult
	cur := prefix
	for len(out) < max {
		if err := ctx.Err(); err != nil {
			return out, driver.E(driver.ErrTimeout, "walk", c.cfg.Device.ID, "context done", true, err)
		}
		id := c.nextID()
		req, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGetNext, id, []snmpcodec.OID{cur})
		if err != nil {
			return out, driver.E(driver.ErrInternal, "walk", c.cfg.Device.ID, err.Error(), false, err)
		}
		resp, err := c.do(ctx, req, "walk")
		if err != nil {
			return out, err
		}
		if st := snmpcodec.ErrorStatus(resp.ErrorStatus); st != snmpcodec.ErrNoError {
			// v1 的表尾就是 noSuchName；这不是错误，是遍历结束。
			if c.version == snmpcodec.V1 && st == snmpcodec.ErrNoSuchName {
				return out, nil
			}
			return out, c.statusError("walk", st, cur, resp.ErrorIndex)
		}
		if len(resp.Varbinds) != 1 {
			return out, driver.E(driver.ErrProtocol, "walk", c.cfg.Device.ID,
				fmt.Sprintf("GETNEXT returned %d varbinds", len(resp.Varbinds)), false, nil)
		}
		vb := resp.Varbinds[0]
		if vb.Value.Type == snmpcodec.TypeEndOfMibView {
			return out, nil
		}
		if v := vb.OID.Compare(cur); v <= 0 {
			// 代理返回了一个不前进的 OID：继续循环只会挂死。
			return out, driver.E(driver.ErrProtocol, "walk", c.cfg.Device.ID,
				fmt.Sprintf("GETNEXT did not advance past %s (returned %s)", cur, vb.OID), false, nil)
		}
		if !prefix.HasPrefix(vb.OID) {
			return out, nil
		}
		out = append(out, GetResult{OID: vb.OID, Value: vb.Value})
		cur = vb.OID
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func (c *connState) oidOf(p driver.PointRef) (snmpcodec.OID, error) {
	if p.Address.Kind != model.AddressSymbol {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("point %q: snmp requires symbol (OID) addressing, got %q", p.ID, p.Address.Kind), false, nil)
	}
	return parseOID(p.Address)
}

func parseOID(a model.Address) (snmpcodec.OID, error) {
	if a.Index > 0 {
		return nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("OID %q 不支持数组下标 [%d]；实例号应写进 OID 本身（例如 .1.3.6.1.2.1.2.2.1.10.%d）",
				a.Path, a.Index, a.Index), false, nil)
	}
	if a.Selector != "" {
		return nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("OID 地址不能带 #%s；selector 只属于 MQTT 载荷字段", a.Selector), false, nil)
	}
	oid, err := snmpcodec.ParseOID(a.Path)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", "", err.Error(), false, err)
	}
	return oid, nil
}

func (c *connState) statusError(op string, st snmpcodec.ErrorStatus, oid snmpcodec.OID, index int) error {
	msg := fmt.Sprintf("agent returned %s", st)
	if index > 0 {
		msg += fmt.Sprintf(" at varbind %d", index)
	}
	if oid != nil {
		msg += " for " + oid.String()
	}
	return driver.E(st.Kind(), op, c.cfg.Device.ID, msg, st.Temporary(), nil)
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityOf(err), Err: err}
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}
