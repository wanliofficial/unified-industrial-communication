// Package ads 实现 ADS（AMS/TCP）主站驱动：按 PLC 变量名读写。
//
// 这一层只做三件事：把点位地址翻译成 ADS 的「变量名 → 句柄 → 值」两步
// 访问、把 PLC 的小端字节翻译成平台值、把 AMS/ADS 错误码翻译成驱动错误
// 分类。报文结构在 codec/ads 里。
//
// 三个设计取舍：
//
//  1. **按变量名寻址，不做符号表编译。** 点位 path 就是 PLC 工程里的符号
//     名（`MAIN.nCounter`，数组写 `MAIN.arr[2]`）。不读符号表、不做 MIB
//     式浏览：符号表的上传格式随运行时版本变化，而现场点位本来就来自
//     变量表，不需要驱动再发现一次。
//  2. **句柄缓存 + 失效重解析。** 每读一个变量要先把名字换成句柄；每轮
//     都换会多付一次往返。缓存按变量名索引；读句柄返回 INVALID_OFFSET/
//     INVALID_GROUP（PLC 重启后句柄全失效）时清掉该条目、重新解析一次再
//     读。只重试一次——第二次还失败就是变量本身的问题。
//  3. **逐点读写，不合并。** ADS 的 ReadWrite 可以一次带多个句柄，但失败
//     时无法逐点归因，且不同运行时的合包语义有差异。逐点事务换来的是
//     「哪个点位错、错在哪一类」——这比省几次往返更重要。
//
// 明确不做：通知订阅（AddDeviceNotification 的句柄生命周期、失效重注册
// 与流式采样是另一套模型，采集侧是拉取语义）、符号表浏览、跨 AMS 路由
// 的自动发现、写回读校验（ADS 的 Write 应答只有 result，没有回显）。
package ads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	adscodec "uic/internal/codec/ads"
	codecvalue "uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
)

const (
	// defaultAMSPort 是第一个 PLC 运行时的 AMS 端口。第二个运行时通常
	// 是 801；端口不是 TCP 端口，配错时的表现是 AMS 头错误 0x0006。
	defaultAMSPort uint16 = 851
	// defaultLocalPort 是本地 AMS 端口。它只需要在本机唯一；真实客户端
	// 一般从 32768 以上随机取一个。
	defaultLocalPort uint16 = 32905
	// defaultRetries 是超时后的额外重发次数。TCP 上重发只在「对端进程
	// 卡住但连接没断」时有意义，因此默认只重试一次。
	defaultRetries = 1
	// defaultStrLen 是 STRING 点位的默认读取长度。
	//
	// PLC 里最常见的声明是 STRING(80)，它占 81 字节（含结尾 NUL）。读一个
	// 与变量宽度不符的长度会得到 INVALID_SIZE，所以这个值必须可配
	// （options=strlen=N），而不是按字符串实际内容猜。
	defaultStrLen = 81
	// maxValueLen 是单次读写的字节上限（ADS 报文本身可到几 MiB，但单个
	// PLC 变量不可能这么大；限制它只是为了在配置写错时早失败）。
	maxValueLen = 65535
)

// defaultLocalNetID 是本地 NetId 的默认值。
//
// AMS 路由器要求源 NetId 与设备上配置的路由/白名单匹配；用 127.0.0.1.1.1
// 是各类客户端最常见的默认值，现场若做了 NetId 白名单需要显式改
// labels.localnetid。
var defaultLocalNetID = adscodec.MustParseNetID("127.0.0.1.1.1")

// Driver 是 ADS 驱动入口。
type Driver struct{}

// New 创建驱动实例。
func New() *Driver { return &Driver{} }

// Meta 返回驱动的不可变元数据。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:         "ads",
		Name:       "ADS/AMS（PLC 变量读写，TCP 48898）",
		Version:    "0.1.0",
		SDKVersion: driver.SDKVersion,
		Roles:      []driver.Role{driver.RoleMaster},
		Transports: []string{"tcp"},
		AddressKinds: []model.AddressKind{
			model.AddressSymbol,
		},
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
			{
				Name: "address", Label: "AMS/TCP 地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:48898", Scope: "channel",
				Help: "AMS/TCP 的 TCP 端点；不是 AMS 端口。AMS 端口在设备的 labels.port 里配。",
			},
			{
				Name: "netid", Label: "目标 AMS NetId", Type: "string", Required: true,
				Placeholder: "192.168.1.10.1.1", Scope: "device",
				Help: "6 段点分十进制，必须与 PLC 上配置的 AMS NetId 完全一致。",
			},
			{
				Name: "port", Label: "目标 AMS 端口", Type: "int", Default: 851, Scope: "device",
				Help: "851 是第一个 PLC 运行时，801 是第二个。配错时 AMS 错误为 target port not found (0x0006)。",
			},
			{
				Name: "localnetid", Label: "本地 AMS NetId", Type: "string", Default: "127.0.0.1.1.1", Scope: "device",
				Help: "设备侧做了 NetId 白名单或路由时必须与登记值一致。",
			},
			{
				Name: "localport", Label: "本地 AMS 端口", Type: "int", Default: 32905, Scope: "device",
				Help: "只需本机唯一；同一网关的多个 ADS 设备不要复用同一个值。",
			},
			{
				Name: "options", Label: "选项", Type: "string", Scope: "device",
				Placeholder: "retries=1,strlen=81",
				Help:        "retries=0..10；strlen=1..65535 是 string 点位的读取长度（STRING(80) 占 81 字节）。",
			},
			{
				Name: "dataType", Label: "数据类型", Type: "enum", Required: true, Scope: "point",
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64",
					"float32", "float64", "string", "bytes"},
				Help: "必须与 PLC 变量声明一致；ADS 报错只给 invalid size/type，不会替你猜宽度。",
			},
			{
				Name: "length", Label: "读取长度", Type: "int", Scope: "point",
				Help: "仅 string/bytes 用：覆盖 options.strlen。STRING(80) 写 81。",
			},
		},
		Description: "ADS/AMS 主站：按变量名读写 PLC 符号；句柄缓存与失效重解析；" +
			"支持 read/write/readwrite、设备信息与运行时状态；小端数值",
	}
}

// Options 是设备级参数。
type Options struct {
	Target    adscodec.Endpoint
	Source    adscodec.Endpoint
	Retries   int
	StringLen int
}

// parseOptions 读取设备标签。所有错误都带“哪个标签、允许什么值”。
func parseOptions(dev model.Device) (Options, error) {
	opt := Options{
		Target:    adscodec.Endpoint{Port: defaultAMSPort},
		Source:    adscodec.Endpoint{NetID: defaultLocalNetID, Port: defaultLocalPort},
		Retries:   defaultRetries,
		StringLen: defaultStrLen,
	}
	labels := dev.Labels
	if labels == nil {
		return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
			"labels.netid 是必填项（目标 PLC 的 AMS NetId，如 192.168.1.10.1.1）", false, nil)
	}
	raw := strings.TrimSpace(labels["netid"])
	if raw == "" {
		return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
			"labels.netid 是必填项（目标 PLC 的 AMS NetId，如 192.168.1.10.1.1）", false, nil)
	}
	nid, err := adscodec.ParseNetID(raw)
	if err != nil {
		return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
	}
	opt.Target.NetID = nid

	if s, ok := labels["port"]; ok && strings.TrimSpace(s) != "" {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || v < 1 || v > 65535 {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("labels.port %q 必须是 1..65535（851 是第一个 PLC 运行时）", s), false, nil)
		}
		opt.Target.Port = uint16(v)
	}
	if s, ok := labels["localnetid"]; ok && strings.TrimSpace(s) != "" {
		v, err := adscodec.ParseNetID(s)
		if err != nil {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		opt.Source.NetID = v
	}
	if s, ok := labels["localport"]; ok && strings.TrimSpace(s) != "" {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || v < 1 || v > 65535 {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("labels.localport %q 必须是 1..65535", s), false, nil)
		}
		opt.Source.Port = uint16(v)
	}

	for _, part := range strings.Split(labels["options"], ",") {
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
		case "strlen":
			if !hasVal {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.strlen 需要 =N", false, nil)
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > maxValueLen {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.strlen %q 必须是 1..%d", val, maxValueLen), false, nil)
			}
			opt.StringLen = n
		default:
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("未知的 options 项 %q（支持 retries / strlen）", key), false, nil)
		}
	}
	return opt, nil
}

// Open 建立 AMS/TCP 连接并做一次 ReadDeviceInfo 握手。
//
// 握手不是可选的：TCP 能连上只说明 48898 端口在监听，NetId/AMS 端口配错
// 要等到第一次读才会暴露。把失败提前到启动期，运维第一时间就能看到
// “target port not found”而不是一条条红色的点位。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if kind := strings.ToLower(strings.TrimSpace(t.Kind)); kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("ads requires tcp transport (AMS/TCP %d), got %q", adscodec.DefaultPort, t.Kind),
			false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address 不能为空（AMS/TCP 端点，如 192.168.1.10:48898）", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, adscodec.DefaultPort)
		}
	}
	opt, err := parseOptions(cfg.Device)
	if err != nil {
		return nil, err
	}

	dialCtx := ctx
	cancel := func() {}
	if timeout := cfg.Channel.EffectiveTimeout(); timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	var nd net.Dialer
	nc, err := nd.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		// ADS 是请求/应答小报文：延迟敏感，Nagle 会把两次往返的请求合并
		// 出一段额外的 40ms 等待。
		_ = tc.SetNoDelay(true)
	}

	c := &connState{
		cfg:     cfg,
		conn:    nc,
		peer:    adscodec.Peer{Source: opt.Source, Target: opt.Target},
		timeout: cfg.Channel.EffectiveTimeout(),
		retries: opt.Retries,
		strlen:  opt.StringLen,
		handles: map[string]uint32{},
	}
	if _, err := c.deviceInfo(ctx); err != nil {
		_ = nc.Close()
		if driver.KindOf(err) == driver.ErrTimeout {
			return nil, driver.E(driver.ErrTimeout, "open", cfg.Device.ID,
				"AMS 握手超时；检查 transport.address（TCP 48898）、labels.netid、"+
					"labels.port（851/801）与 AMS 路由", true, err)
		}
		return nil, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type connState struct {
	cfg     driver.OpenConfig
	conn    net.Conn
	peer    adscodec.Peer
	timeout time.Duration
	retries int
	strlen  int

	mu      sync.Mutex
	invoke  uint32
	handles map[string]uint32
	closed  bool
}

// Close 关闭连接。可重复调用。
//
// 不主动释放句柄：句柄属于本次会话，TCP 断开后由运行时回收；在 Close
// 里再发一串 Release 会把退出时间拖到一个超时周期以上，而且对端已经
// 不可达时每个 Release 都会等一次超时。
func (c *connState) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

func (c *connState) nextInvoke() uint32 {
	c.invoke++
	if c.invoke == 0 {
		c.invoke = 1
	}
	return c.invoke
}

func (c *connState) deadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		return dl
	}
	return d
}

// errBadFrame 标记“收到字节，但不是一个合法 AMS 帧”。它与网络错误必须
// 分开：前者是 protocol（重试无用），后者按超时/断链处理。
var errBadFrame = errors.New("ads: bad frame")

// exchange 发送一条请求并等回对应的应答。
//
// 只接受「命令相同、invoke 相同、带应答位」的帧；通知帧（DeviceNotification）
// 与迟到的旧应答直接丢弃。TCP 上不会乱序，但上一轮超时后对端可能晚一点
// 才把应答写出来——不校验 invoke 就会把上一轮的值当成本轮的。
func (c *connState) exchange(ctx context.Context, req adscodec.Frame, op string) (adscodec.Frame, error) {
	raw, err := req.Encode()
	if err != nil {
		return adscodec.Frame{}, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, err.Error(), false, err)
	}
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return adscodec.Frame{}, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
		}
		if err := c.conn.SetDeadline(c.deadline(ctx)); err != nil {
			return adscodec.Frame{}, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"set tcp deadline", true, err)
		}
		if _, err := c.conn.Write(raw); err != nil {
			return adscodec.Frame{}, c.writeError(op, err, ctx)
		}
		for {
			resp, err := readFrame(c.conn)
			if err != nil {
				if errors.Is(err, errBadFrame) {
					return adscodec.Frame{}, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
						"malformed AMS frame: "+err.Error(), false, err)
				}
				lastErr = c.readError(op, err, ctx)
				break
			}
			if resp.Command == adscodec.CmdDeviceNotification {
				// 本驱动不建立订阅；设备侧可能仍有历史订阅（同一 NetId
				// 的其它客户端），静默丢弃而不是判协议错。
				continue
			}
			if !resp.IsResponse() || resp.Command != req.Command || resp.Invoke != req.Invoke {
				continue
			}
			if resp.Error != adscodec.CodeOK && resp.Error.Temporary() && attempt < c.retries {
				lastErr = c.codeError(op, resp.Error, "AMS")
				break
			}
			return resp, nil
		}
		// 只有「等一下可能会好」的两类值得重发：链路超时，以及 AMS 层
		// 明确标记为临时性的错误（mailbox full / busy / no memory）。
		// 断链与地址/权限错重发一万次也不会变好，重试只会让一轮采集
		// 多花几十次超时。
		switch driver.KindOf(lastErr) {
		case driver.ErrTimeout:
		case driver.ErrProtocol:
			if !driver.IsRetryable(lastErr) {
				return adscodec.Frame{}, lastErr
			}
		default:
			return adscodec.Frame{}, lastErr
		}
		if ctx.Err() != nil {
			return adscodec.Frame{}, lastErr
		}
	}
	return adscodec.Frame{}, lastErr
}

// writeError 与 readError 用同一套分类：写超时是 timeout（重发可能成功），
// 不是断链。把两者混为一谈会让「对端短暂卡住」被当成链路已死，设备被
// 直接标红退避。
func (c *connState) writeError(op string, err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID,
			"tcp write timeout; check link and PLC runtime state", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "tcp write failed", true, err)
}

func (c *connState) readError(op string, err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID,
			"no response within timeout; check labels.netid/port and PLC runtime state", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "tcp read failed", true, err)
}

func (c *connState) codeError(op string, code adscodec.Code, where string) error {
	return driver.E(code.Kind(), op, c.cfg.Device.ID,
		where+" returned "+code.String()+hint(code), code.Temporary(), nil)
}

// hint 把最常见的几个错误码直接翻译成排查动作。错误信息是要给人看的，
// “invalid size (0x705)”本身不说明该改哪里。
func hint(code adscodec.Code) string {
	switch code {
	case adscodec.CodeTargetPortNotFound:
		return "；检查 labels.port（851 是第一个 PLC 运行时，801 是第二个）以及 PLC 是否在运行"
	case adscodec.CodeTargetMachineNotFound:
		return "；AMS 路由找不到目标 NetId，检查 labels.netid 与设备侧路由配置"
	case adscodec.CodeInvalidAMSNetID:
		return "；labels.netid 必须是 6 段 0..255 的点分十进制"
	case adscodec.CodeSymbolNotFound:
		return "；符号名区分大小写，数组元素写成 NAME[0]，结构体成员逐级写全"
	case adscodec.CodeInvalidSize, adscodec.CodeInvalidLength:
		return "；读长度与 PLC 变量宽度不符：STRING 用 options=strlen，bytes 用点位 length"
	case adscodec.CodeInvalidAccess:
		return "；该符号在 PLC 侧不可写（需要 VAR_GLOBAL 且允许读写）"
	}
	return ""
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readFrame 读一条完整的 AMS/TCP 报文。
func readFrame(r io.Reader) (adscodec.Frame, error) {
	var hdr [adscodec.TCPHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return adscodec.Frame{}, err
	}
	n, err := adscodec.FrameLen(hdr[:])
	if err != nil {
		return adscodec.Frame{}, fmt.Errorf("%w: %v", errBadFrame, err)
	}
	buf := make([]byte, n)
	copy(buf, hdr[:])
	if _, err := io.ReadFull(r, buf[adscodec.TCPHeaderLen:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return adscodec.Frame{}, fmt.Errorf("%w: %v", errBadFrame, err)
		}
		return adscodec.Frame{}, err
	}
	f, err := adscodec.Decode(buf)
	if err != nil {
		return adscodec.Frame{}, fmt.Errorf("%w: %v", errBadFrame, err)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// Probe / 设备信息 / 状态
// ---------------------------------------------------------------------------

// Probe 读设备信息，成功即认为链路通。
func (c *connState) Probe(ctx context.Context) error {
	_, err := c.deviceInfo(ctx)
	return err
}

// DeviceInfo 返回设备名与版本，供自检 CLI 使用。
func (c *connState) DeviceInfo(ctx context.Context) (adscodec.DeviceInfo, error) {
	return c.deviceInfo(ctx)
}

func (c *connState) deviceInfo(ctx context.Context) (adscodec.DeviceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return adscodec.DeviceInfo{}, driver.E(driver.ErrDisconnected, "info", c.cfg.Device.ID, "connection closed", true, nil)
	}
	resp, err := c.exchange(ctx, c.peer.ReadDeviceInfo(c.nextInvoke()), "info")
	if err != nil {
		return adscodec.DeviceInfo{}, err
	}
	if resp.Error != adscodec.CodeOK {
		return adscodec.DeviceInfo{}, c.codeError("info", resp.Error, "AMS")
	}
	info, code, err := adscodec.ParseReadDeviceInfo(resp)
	if err != nil {
		return adscodec.DeviceInfo{}, driver.E(driver.ErrProtocol, "info", c.cfg.Device.ID, err.Error(), false, err)
	}
	if code != adscodec.CodeOK {
		return adscodec.DeviceInfo{}, c.codeError("info", code, "ReadDeviceInfo")
	}
	return info, nil
}

// DeviceState 返回 PLC 运行时状态，供自检 CLI 使用。
func (c *connState) DeviceState(ctx context.Context) (adscodec.DeviceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return adscodec.DeviceState{}, driver.E(driver.ErrDisconnected, "state", c.cfg.Device.ID, "connection closed", true, nil)
	}
	resp, err := c.exchange(ctx, c.peer.ReadState(c.nextInvoke()), "state")
	if err != nil {
		return adscodec.DeviceState{}, err
	}
	if resp.Error != adscodec.CodeOK {
		return adscodec.DeviceState{}, c.codeError("state", resp.Error, "AMS")
	}
	st, code, err := adscodec.ParseReadState(resp)
	if err != nil {
		return adscodec.DeviceState{}, driver.E(driver.ErrProtocol, "state", c.cfg.Device.ID, err.Error(), false, err)
	}
	if code != adscodec.CodeOK {
		return adscodec.DeviceState{}, c.codeError("state", code, "ReadState")
	}
	return st, nil
}

// WriteControl 改 PLC 运行时状态（Run/Stop/Reset）。属于控制操作，CLI 里
// 必须显式给 -allow-control 才允许。
func (c *connState) WriteControl(ctx context.Context, state adscodec.State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "control", c.cfg.Device.ID, "connection closed", true, nil)
	}
	resp, err := c.exchange(ctx, c.peer.WriteControl(c.nextInvoke(), state, 0, nil), "control")
	if err != nil {
		return err
	}
	if resp.Error != adscodec.CodeOK {
		return c.codeError("control", resp.Error, "AMS")
	}
	code, err := adscodec.ParseWriteControl(resp)
	if err != nil {
		return driver.E(driver.ErrProtocol, "control", c.cfg.Device.ID, err.Error(), false, err)
	}
	if code != adscodec.CodeOK {
		return c.codeError("control", code, "WriteControl")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

// Read 逐点读。链路级失败会让剩余点位快速失败；地址/权限错误只影响
// 当前点位，不进入整轮 firstErr（与其它驱动一致：点表错误不该熔断设备）。
func (c *connState) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var points []driver.PointRef
	for _, span := range spans {
		points = append(points, span.Points...)
	}
	out := make([]driver.RawSample, 0, len(points))
	if c.closed {
		for _, p := range points {
			out = append(out, errSample(p.ID, driver.E(driver.ErrDisconnected, "read", c.cfg.Device.ID,
				"connection closed", true, nil)))
		}
		return out, driver.E(driver.ErrDisconnected, "read", c.cfg.Device.ID, "connection closed", true, nil)
	}

	var firstErr error
	goodSamples := 0
	for i, p := range points {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
			for _, rest := range points[i:] {
				out = append(out, errSample(rest.ID, e))
			}
			return out, e
		}
		s, linkErr := c.readOne(ctx, p)
		out = append(out, s)
		if linkErr != nil {
			// 链路失败：剩余点位不可能成功，统一标记后返回。
			for _, rest := range points[i+1:] {
				out = append(out, errSample(rest.ID, linkErr))
			}
			return out, linkErr
		}
		if s.Err != nil {
			if firstErr == nil && driver.KindOf(s.Err) != driver.ErrAddress {
				firstErr = s.Err
			}
			continue
		}
		goodSamples++
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误，否则
	// 一个变量名写错会让 collector 丢弃连接、整台 PLC 每周期重连。
	return out, driver.PartialReadError(goodSamples, firstErr)
}

func (c *connState) readOne(ctx context.Context, p driver.PointRef) (driver.RawSample, error) {
	name, err := c.symbolName(p.Address)
	if err != nil {
		return errSample(p.ID, err), nil
	}
	length, err := c.valueLen("read", p.Address, p.DataType)
	if err != nil {
		return errSample(p.ID, err), nil
	}
	handle, err := c.handleLocked(ctx, name, "read")
	if err != nil {
		return errSample(p.ID, err), linkFailure(err)
	}
	data, code, err := c.readByHandle(ctx, handle, length)
	if err != nil {
		return errSample(p.ID, err), linkFailure(err)
	}
	if code != adscodec.CodeOK && staleHandle(code) {
		// 句柄在 PLC 重启后失效。清掉缓存、重新解析一次；第二次还失败
		// 就按真实错误上报，避免「PLC 一直重启、驱动一直重试」的死循环。
		delete(c.handles, name)
		handle, herr := c.handleLocked(ctx, name, "read")
		if herr != nil {
			return errSample(p.ID, herr), linkFailure(herr)
		}
		data, code, err = c.readByHandle(ctx, handle, length)
		if err != nil {
			return errSample(p.ID, err), linkFailure(err)
		}
	}
	if code != adscodec.CodeOK {
		return errSample(p.ID, c.codeError("read", code, name)), nil
	}
	v, err := decodeValue(data, p.DataType)
	if err != nil {
		return errSample(p.ID, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("%s: %v", name, err), false, err)), nil
	}
	return driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood}, nil
}

func (c *connState) readByHandle(ctx context.Context, handle uint32, length int) ([]byte, adscodec.Code, error) {
	resp, err := c.exchange(ctx, c.peer.ReadByHandle(c.nextInvoke(), handle, uint32(length)), "read")
	if err != nil {
		return nil, 0, err
	}
	if resp.Error != adscodec.CodeOK {
		return nil, 0, c.codeError("read", resp.Error, "AMS")
	}
	data, code, err := adscodec.ParseRead(resp)
	if err != nil {
		return nil, 0, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	return data, code, nil
}

// handleLocked 返回变量句柄，必要时向设备申请一次并缓存。
func (c *connState) handleLocked(ctx context.Context, name, op string) (uint32, error) {
	if h, ok := c.handles[name]; ok {
		return h, nil
	}
	resp, err := c.exchange(ctx, c.peer.HandleByName(c.nextInvoke(), name), op)
	if err != nil {
		return 0, err
	}
	if resp.Error != adscodec.CodeOK {
		return 0, c.codeError(op, resp.Error, "AMS")
	}
	data, code, err := adscodec.ParseReadWrite(resp)
	if err != nil {
		return 0, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID, err.Error(), false, err)
	}
	if code != adscodec.CodeOK {
		return 0, c.codeError(op, code, "symbol handle "+name)
	}
	if len(data) < 4 {
		return 0, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
			fmt.Sprintf("handle response for %s is %d bytes, need 4", name, len(data)), false, nil)
	}
	h := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
	if h == 0 {
		return 0, driver.E(driver.ErrAddress, op, c.cfg.Device.ID,
			"device returned handle 0 for "+name, false, nil)
	}
	c.handles[name] = h
	return h, nil
}

// ---------------------------------------------------------------------------
// Write
// ---------------------------------------------------------------------------

func (c *connState) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]driver.WriteResult, 0, len(reqs))
	if c.closed {
		for _, r := range reqs {
			out = append(out, writeErr(r.PointID, driver.E(driver.ErrDisconnected, "write", c.cfg.Device.ID,
				"connection closed", true, nil)))
		}
		return out, driver.E(driver.ErrDisconnected, "write", c.cfg.Device.ID, "connection closed", true, nil)
	}
	for _, req := range reqs {
		res, linkErr := c.writeOne(ctx, req)
		out = append(out, res)
		if linkErr != nil {
			for _, rest := range reqs[len(out):] {
				out = append(out, writeErr(rest.PointID, linkErr))
			}
			return out, linkErr
		}
	}
	return out, nil
}

func (c *connState) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	name, err := c.symbolName(req.Address)
	if err != nil {
		return writeErr(req.PointID, err), nil
	}
	data, err := c.encodeValue(req)
	if err != nil {
		return writeErr(req.PointID, err), nil
	}
	handle, err := c.handleLocked(ctx, name, "write")
	if err != nil {
		return writeErr(req.PointID, err), linkFailure(err)
	}
	resp, err := c.exchange(ctx, c.peer.WriteByHandle(c.nextInvoke(), handle, data), "write")
	if err != nil {
		return writeErr(req.PointID, err), linkFailure(err)
	}
	if resp.Error != adscodec.CodeOK {
		e := c.codeError("write", resp.Error, "AMS")
		return writeErr(req.PointID, e), linkFailure(e)
	}
	code, err := adscodec.ParseWrite(resp)
	if err != nil {
		e := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeErr(req.PointID, e), nil
	}
	if code != adscodec.CodeOK && staleHandle(code) {
		delete(c.handles, name)
		handle, herr := c.handleLocked(ctx, name, "write")
		if herr != nil {
			return writeErr(req.PointID, herr), linkFailure(herr)
		}
		resp, err = c.exchange(ctx, c.peer.WriteByHandle(c.nextInvoke(), handle, data), "write")
		if err != nil {
			return writeErr(req.PointID, err), linkFailure(err)
		}
		if resp.Error == adscodec.CodeOK {
			code, err = adscodec.ParseWrite(resp)
			if err != nil {
				e := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
				return writeErr(req.PointID, e), nil
			}
		} else {
			e := c.codeError("write", resp.Error, "AMS")
			return writeErr(req.PointID, e), linkFailure(e)
		}
	}
	if code != adscodec.CodeOK {
		return writeErr(req.PointID, c.codeError("write", code, name)), nil
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// ---------------------------------------------------------------------------
// 值
// ---------------------------------------------------------------------------

// valueLen 返回该点位在线上占用的字节数。
func (c *connState) valueLen(op string, a model.Address, dt model.DataType) (int, error) {
	var n int
	switch dt {
	case model.TypeBool:
		n = 1 // PLC 的 BOOL 占 1 字节（位变量在符号层不单独暴露）
	case model.TypeString, model.TypeBytes:
		n = a.Length
		if n == 0 {
			n = c.strlen
		}
	case model.TypeBCD16, model.TypeBCD32:
		return 0, driver.E(driver.ErrUnsupported, op, c.cfg.Device.ID,
			"ADS 没有 BCD 线类型；点表请改成 uint16/uint32/bytes", false, nil)
	default:
		n = dt.Bytes()
	}
	if n <= 0 {
		return 0, driver.E(driver.ErrUnsupported, op, c.cfg.Device.ID,
			fmt.Sprintf("dataType %q 没有确定的字节宽度", dt), false, nil)
	}
	if n > maxValueLen {
		return 0, driver.E(driver.ErrUnsupported, op, c.cfg.Device.ID,
			fmt.Sprintf("读取长度 %d 超过上限 %d", n, maxValueLen), false, nil)
	}
	return n, nil
}

// encodeValue 把平台值编码成 PLC 的小端表示。
func (c *connState) encodeValue(req driver.WriteRequest) ([]byte, error) {
	dt := req.DataType
	switch dt {
	case model.TypeBool:
		b := byte(0)
		if req.Value.Bool || req.Value.Num != 0 {
			b = 1
		}
		return []byte{b}, nil
	case model.TypeString, model.TypeBytes:
		n, err := c.valueLen("write", req.Address, dt)
		if err != nil {
			return nil, err
		}
		raw := []byte(req.Value.Text)
		if len(raw) > n {
			return nil, driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID,
				fmt.Sprintf("值 %d 字节超过目标长度 %d（STRING 需要留 1 字节给结尾 NUL）",
					len(raw), n), false, nil)
		}
		if dt == model.TypeString && len(raw) == n {
			return nil, driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID,
				fmt.Sprintf("字符串 %d 字节没有留出结尾 NUL；把 length/strlen 调到 %d",
					len(raw), len(raw)+1), false, nil)
		}
		out := make([]byte, n)
		copy(out, raw)
		return out, nil
	case model.TypeBCD16, model.TypeBCD32:
		return nil, driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID,
			"ADS 没有 BCD 线类型；点表请改成 uint16/uint32", false, nil)
	}
	if err := checkRange(dt, req.Value.Num); err != nil {
		return nil, driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID, err.Error(), false, err)
	}
	raw, err := codecvalue.Encode(req.Value, dt, model.OrderDCBA)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID, err.Error(), false, err)
	}
	return raw, nil
}

// checkRange 拒绝越界与带小数的整数写入。
//
// codec/value 的 Encode 用强制转换落地，70000 写进 INT16 会静默变成
// 4464；写入被“接受”了，但设备上的值已经不是调用方给的值。这类错误
// 比写入失败更难发现，必须在发报文之前拦下。
func checkRange(dt model.DataType, v float64) error {
	switch dt {
	case model.TypeInt16:
		if v != math.Trunc(v) || v < math.MinInt16 || v > math.MaxInt16 {
			return fmt.Errorf("%v 超出 int16 范围或不是整数", v)
		}
	case model.TypeUint16:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint16 {
			return fmt.Errorf("%v 超出 uint16 范围或不是整数", v)
		}
	case model.TypeInt32:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return fmt.Errorf("%v 超出 int32 范围或不是整数", v)
		}
	case model.TypeUint32:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint32 {
			return fmt.Errorf("%v 超出 uint32 范围或不是整数", v)
		}
	case model.TypeInt64:
		if v != math.Trunc(v) || v < math.MinInt64 || v >= 9223372036854775808.0 {
			return fmt.Errorf("%v 超出 int64 范围或不是整数", v)
		}
	case model.TypeUint64:
		if v != math.Trunc(v) || v < 0 || v >= 18446744073709551616.0 {
			return fmt.Errorf("%v 超出 uint64 范围或不是整数", v)
		}
	case model.TypeFloat32:
		// codec/value 的 Encode 用 `float32(v)` 强制落地：1e300 会被静默
		// 写成 +Inf，1e-50 会被静默写成 0。和整数越界一样，这种「写下去的
		// 值已经不是调用方给的值」的缺陷必须在发报文前拦下（对比 DNP3
		// 驱动的 `analogFitsWire` 对 float32 做的同一类检查）。
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%v 不是有限浮点数，无法写入 %s", v, dt)
		}
		if f := float32(v); math.IsInf(float64(f), 0) {
			return fmt.Errorf("%v 超出 float32 幅度（会溢出成 Inf），无法写入 %s", v, dt)
		} else if f == 0 && v != 0 {
			return fmt.Errorf("%v 低于 float32 最小可表示值（会静默变成 0），无法写入 %s", v, dt)
		}
	case model.TypeFloat64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%v 不是有限浮点数，无法写入 %s", v, dt)
		}
	}
	return nil
}

// decodeValue 解码 PLC 返回的小端字节。
func decodeValue(data []byte, dt model.DataType) (model.Value, error) {
	switch dt {
	case model.TypeBool:
		if len(data) < 1 {
			return model.Value{}, fmt.Errorf("empty bool value")
		}
		switch data[0] {
		case 0:
			return model.BoolValue(false), nil
		case 1:
			return model.BoolValue(true), nil
		}
		return model.Value{}, fmt.Errorf("BOOLean byte 0x%02X is neither 0 nor 1", data[0])
	case model.TypeString:
		// PLC 的 STRING(n) 是**定长缓冲 + NUL 结尾**：第一个 NUL 之后是填充，
		// 不属于字符串内容。这里必须按「首个 NUL 截断」处理，不能用
		// `TrimRight(s, "\x00")`——那只去尾部 NUL，会把缓冲区里残留的字节
		// 连同内嵌 NUL 一起当成值（`"batch-42\x00\x00junk"` 变成携带控制
		// 字符的字符串，落到 JSON 日志/OPC UA 里各家长度语义还不一样）。
		// 同包 codec 的 `cString` 对符号名用的就是这个语义。
		return model.TextValue(dt, firstNUL(string(data))), nil
	case model.TypeBytes:
		return model.BytesValue(append([]byte(nil), data...)), nil
	case model.TypeBCD16, model.TypeBCD32:
		return model.Value{}, fmt.Errorf("ADS has no BCD wire type")
	}
	return codecvalue.Decode(data, dt, model.OrderDCBA)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// firstNUL 在 NUL 处截断字符串，NUL 之后的字节是定长缓冲区的填充/残留。
func firstNUL(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

// symbolName 把点位地址还原成 PLC 符号名（数组元素带 [index]）。
func (c *connState) symbolName(a model.Address) (string, error) {
	if a.Kind != model.AddressSymbol {
		return "", driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, fmt.Sprintf(
			"ADS 只接受 symbol 地址（变量名），got kind %q", a.Kind), false, nil)
	}
	if a.Selector != "" {
		return "", driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			"ADS 没有载荷字段选择器（#...）；请直接写变量名", false, nil)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, "变量名为空", false, nil)
	}
	if a.Index > 0 {
		return "", driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("数组下标请写进变量名（%s[%d]），不要用 address.index", a.Path, a.Index), false, nil)
	}
	// 变量名就是 PLC 符号表里的完整路径，数组元素带 `[n]`；不要在驱动里
	// 重新拼下标——「[0] 与未配置」在 JSON 里无法区分。
	return a.Path, nil
}

// staleHandle 判断某个结果码是否可能由“句柄失效”引起。
//
// 只看 handle 相关的两类：INVALID_GROUP / INVALID_OFFSET。INVALID_SIZE
// 是长度配错，重新解析句柄不会让它变好；把它也算进来会让每个配错的
// 点位多付一次往返。
func staleHandle(code adscodec.Code) bool {
	return code == adscodec.CodeInvalidGroup || code == adscodec.CodeInvalidOffset
}

// linkFailure 表示这个错误已经让连接不可继续使用，剩余点位应当快速失败。
//
// 地址/权限/不支持三类是「这个点位的问题」，连接仍然可用（换一个变量名照样能读）；
// 其余（超时、断链、协议错、设备内部错）都按链路级处理。
//
// 这里把 `protocol` 也算链路级，是因为 ADS 的 `protocol` 无法区分「帧结构错」
// 与「AMS 应答内容错」：前者意味着 TCP 字节流已经错位、后续帧边界不可信，
// 只能断链重建。宁可对一个点位的解析失败多重连一次，也不能在错位的字节流上
// 继续解析——那会把下一个点的值读成上一条报文的残片，静默给出错误数字。
//
// 这与 BACnet/IP、SNMP 这类数据报协议不同：那里每一帧独立成包，一帧畸形只
// 影响这一帧（见 docs §3.3.1 对两类承载层的区分）。
func linkFailure(err error) error {
	switch driver.KindOf(err) {
	case driver.ErrAddress, driver.ErrPermission, driver.ErrUnsupported:
		return nil
	}
	return err
}

// qualityOf 按错误种类派生质量码。
//
// 质量码不是「展示层的事」：`pipeline.Apply` 直接取样本里的 `Quality`
// （只有空串才兜成 `good`），`runtime/collector` 对写结果同样只兜空串，
// 下游 OPC UA（`timeout → BadTimeout`、`comm_error → BadNoCommunication`、
// `address_error → BadNodeIDUnknown`）、历史查询与报警分级都按质量码区分。
//
// 早先这里的 `errSample`/`writeErr` 写死 `bad`，于是「PLC 没应答」、「变量名
// 写错」、「没有权限」在运行时侧全变成同一个值：OPC UA 客户端看到的是
// `BadNoData` 而不是 `BadTimeout`，运维也就无从判断该查网络还是查点表。
// 这与 docs §3.4 规则 5 冲突——`qualityOf(err)` 本来就能给出更准确的码。
func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	// 权限/不支持/协议/内部错误：点位级坏值，没有更细的码可用。
	return model.QualityBad
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityOf(err), Err: err}
}

func writeErr(id string, err error) driver.WriteResult {
	return driver.WriteResult{PointID: id, Quality: qualityOf(err), Err: err}
}
