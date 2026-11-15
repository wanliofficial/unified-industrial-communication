// Package coap 实现 CoAP（Constrained Application Protocol）的 UDP 明文主站驱动。
//
// 与 Modbus 这类「寄存器 + 批量读」协议不同，CoAP 的数据模型是**资源**：
// 每个点位对应一个 URI（`/sensors/temp`），取值就是一次 GET，写入是一次
// PUT / POST。因此这里没有「批量读」：一条报文只能读一个资源，`Read` 接收
// 多个点位时是逐点串行。
//
// CoAP 跑在 UDP 上，所以可靠性由协议自己提供：
//
//   - **消息 ID + 令牌**：MID 配对 ACK，Token 配对「分离响应」。两者都不能省：
//     一条 CON 请求可能先收到一个空的 ACK（表示「已收到，稍后回复」），真正
//     的响应随后带着同一个 Token 从另一条报文回来。
//   - **CON 重传**：没收到 ACK 就指数退避重发，最多 `maxRetransmit` 次。NON
//     不重传（它本来就不要求确认）。
//   - **Block2 分块**：响应太大时服务端只回第一块并置 M=1，客户端按块号继续
//     GET。不处理它，稍大的 JSON 资源会被静默截断成非法 JSON。
//
// 能力边界：只做明文 UDP、GET/PUT/POST、text/plain 与 application/json 载荷、
// Block2 接收、`/.well-known/core` 资源发现。**不做 DTLS**（证书与握手是独立
// 子系统）、**不做 Observe 订阅**（设备主动推与调度式采集的语义冲突，混进来
// 会让超时/重连/死区三套逻辑互相干扰）、**不做 Block1 分块发送**（写载荷大
// 到需要分块时，正确做法通常是改设备模型，而不是在网关里拼半个事务）。
package coap

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	coapcodec "uic/internal/codec/coap"
	"uic/internal/codec/textval"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// DefaultPort 是 CoAP 的注册端口。
const DefaultPort = coapcodec.DefaultPort

// 写载荷的表示形态。名字是给配置用的，映射到 Content-Format 在内部完成。
const (
	FormatText   = "text"
	FormatJSON   = "json"
	FormatOctets = "octets"
)

// 写方法。
const (
	MethodPut  = "put"
	MethodPost = "post"
)

// 内置默认值。
const (
	defaultAckTimeout    = 2 * time.Second
	defaultMaxRetransmit = 4
	minAckTimeout        = 100 * time.Millisecond
	maxAckTimeout        = 30 * time.Second
	maxRetransmitLimit   = 8
	// maxBodyBytes 是 Block2 重组的上限。CoAP 资源通常是几百字节的 JSON，
	// 8 MiB 足以覆盖固件清单之类的大对象，同时把「服务端永远置 M=1」
	// 的失控场景挡在内存之外。
	maxBodyBytes = 8 << 20
	// maxBlocks 与 maxBodyBytes 配合：即使每块只有 16 字节，也不会无限循环。
	maxBlocks = 1 << 16
)

// discoveryPath 是 CoAP 规定（且被广泛实现）的资源发现入口。
const discoveryPath = "/.well-known/core"

// Dialer 抽象 UDP 建链，测试可注入内存数据报链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error)

// Driver 是 CoAP 主站驱动。
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
func NewWithDialer(d Dialer) *Driver {
	return &Driver{dial: d}
}

// Meta 声明 CoAP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "coap",
		Name:         "CoAP",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"udp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:  driver.CapSupported,
			Write: driver.CapSupported,
			// Observe 不在范围内：底层可以把 2.05 通知当成一次读，
			// 但「订阅」需要长期维护观察关系与序号，本驱动刻意不做。
			Subscribe: driver.CapUnsupported,
			// `/.well-known/core` 是标准发现入口，已实现。
			Discovery:   driver.CapSupported,
			Browse:      driver.CapUnsupported,
			MultiRead:   driver.CapUnsupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.30:5683", Scope: "channel",
				Help: "CoAP 服务端地址；省略端口时按 5683 处理"},
			{Name: "labels.contentFormat", Label: "载荷格式", Type: "enum",
				Enum: []string{FormatText, FormatJSON, FormatOctets}, Default: FormatText,
				Scope: "device",
				Help:  "text=text/plain、json=application/json、octets=application/octet-stream；只影响写入，读取按响应的 Content-Format 解码"},
			{Name: "labels.writeMethod", Label: "写方法", Type: "enum",
				Enum: []string{MethodPut, MethodPost}, Default: MethodPut, Scope: "device",
				Help: "PUT=替换资源、POST=提交/触发；大多数执行器用 PUT"},
			{Name: "labels.ackTimeout", Label: "ACK 超时", Type: "duration",
				Default: "2s", Scope: "device",
				Help: "CON 首轮等待 ACK 的时长，之后每次翻倍；必须 ≤ device.timeout"},
			{Name: "labels.maxRetransmit", Label: "最大重传次数", Type: "int",
				Default: defaultMaxRetransmit, Scope: "device",
				Help: "0..8；UDP 丢包时靠它兜底，设 0 表示只发一次"},
			{Name: "labels.blockSize", Label: "首选块大小", Type: "int", Default: 0,
				Scope: "device",
				Help:  "0=跟随服务端；16/32/…/1024 表示首块就请求该大小（服务端仍可改小）"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"symbol"}, Scope: "point"},
			{Name: "address.path", Label: "资源路径", Type: "string", Required: true,
				Scope: "point", Placeholder: "/sensors/temp",
				Help: "CoAP 资源 URI 路径，可省略前导斜杠"},
			{Name: "address.selector", Label: "JSON 字段", Type: "string", Scope: "point",
				Placeholder: "data.temperature",
				Help:        "响应为 application/json 时取哪个字段；支持 a.b 与 a[0]，留空表示整个文档"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64",
					"uint64", "float32", "float64", "string", "bytes"}, Scope: "point"},
			{Name: "address.length", Label: "最大字节数", Type: "int", Scope: "point",
				Help: "bytes/string 的接收上限；0 表示不限制"},
		},
		Description: "CoAP 主站：资源 GET/PUT/POST、Block2 分块接收、资源发现（UDP 5683）",
	}
}

// settings 是设备级可调参数。
type settings struct {
	contentFormat string
	writeMethod   string
	ackTimeout    time.Duration
	maxRetransmit int
	blockSize     uint32
}

// Open 建立 UDP 链路并解析点位。
//
// 不做「Open 即探测」：CoAP 的点位寻址就是资源路径，`Open` 阶段没有一条
// 对所有设备都成立的探测请求（有的设备不实现 `/.well-known/core`）。真正的
// 连通性检查交给 `Probe`，与 BACnet/IP 的做法一致。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "udp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("coap requires udp transport, got %q", t.Kind), false, nil)
	}
	addr, err := coapcodec.HostPort(t.Address)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	st, err := parseSettings(cfg)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	plans, byID, err := buildPlans(cfg.Device)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID, err.Error(), false, err)
	}

	c := &conn{
		devID:    cfg.Device.ID,
		addr:     addr,
		dial:     d.dial,
		timeout:  cfg.Device.TimeoutOrDefault(),
		st:       st,
		points:   plans,
		byID:     byID,
		tokenSeq: uint64(time.Now().UnixNano()),
		mid:      uint16(time.Now().UnixNano()),
	}
	link, err := c.dial(ctx, addr, c.timeout)
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	c.link = link
	return c, nil
}

func parseSettings(cfg driver.OpenConfig) (settings, error) {
	st := settings{
		contentFormat: FormatText,
		writeMethod:   MethodPut,
		ackTimeout:    defaultAckTimeout,
		maxRetransmit: defaultMaxRetransmit,
	}
	labels := cfg.Device.Labels

	if v := strings.TrimSpace(labels["contentFormat"]); v != "" {
		switch strings.ToLower(v) {
		case FormatText, "text/plain":
			st.contentFormat = FormatText
		case FormatJSON, "application/json":
			st.contentFormat = FormatJSON
		case FormatOctets, "application/octet-stream", "binary":
			st.contentFormat = FormatOctets
		default:
			return st, fmt.Errorf("labels.contentFormat %q: want %s | %s | %s",
				v, FormatText, FormatJSON, FormatOctets)
		}
	}
	if v := strings.TrimSpace(labels["writeMethod"]); v != "" {
		switch strings.ToLower(v) {
		case MethodPut, MethodPost:
			st.writeMethod = strings.ToLower(v)
		default:
			return st, fmt.Errorf("labels.writeMethod %q: want %s | %s", v, MethodPut, MethodPost)
		}
	}
	if v := strings.TrimSpace(labels["ackTimeout"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return st, fmt.Errorf("labels.ackTimeout %q: %w", v, err)
		}
		if d < minAckTimeout || d > maxAckTimeout {
			return st, fmt.Errorf("labels.ackTimeout %q: must be within %s..%s", v, minAckTimeout, maxAckTimeout)
		}
		st.ackTimeout = d
	}
	if v := strings.TrimSpace(labels["maxRetransmit"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return st, fmt.Errorf("labels.maxRetransmit %q: not an integer", v)
		}
		if n < 0 || n > maxRetransmitLimit {
			return st, fmt.Errorf("labels.maxRetransmit %d: must be 0..%d", n, maxRetransmitLimit)
		}
		st.maxRetransmit = n
	}
	if v := strings.TrimSpace(labels["blockSize"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return st, fmt.Errorf("labels.blockSize %q: not an integer", v)
		}
		if n != 0 && !validBlockSize(n) {
			return st, fmt.Errorf("labels.blockSize %d: want 0 or 16/32/64/128/256/512/1024", n)
		}
		st.blockSize = uint32(n)
	}
	// ackTimeout 超过整台设备的采集预算时，每次请求都会在拿到 ACK 前
	// 被 ctx 取消——配置看起来「更宽容」，实际只会更慢、更难排查。
	if st.ackTimeout > cfg.Device.TimeoutOrDefault() {
		return st, fmt.Errorf("labels.ackTimeout %s exceeds device.timeout %s; "+
			"raise device.timeout or lower ackTimeout",
			st.ackTimeout, cfg.Device.TimeoutOrDefault())
	}
	return st, nil
}

func validBlockSize(n int) bool {
	for szx := 0; szx <= 6; szx++ {
		if n == 1<<(szx+4) {
			return true
		}
	}
	return false
}

// pointPlan 是一个点位解析后的资源寻址。
type pointPlan struct {
	ref      driver.PointRef
	path     string
	selector string
	typ      model.DataType
	maxLen   int
}

func buildPlans(d model.Device) ([]pointPlan, map[string]pointPlan, error) {
	plans := make([]pointPlan, 0, len(d.Points))
	byID := make(map[string]pointPlan, len(d.Points))
	for _, p := range d.Points {
		if !p.Enabled {
			continue
		}
		plan, err := planPoint(driver.PointRefOf(d, p))
		if err != nil {
			return nil, nil, err
		}
		plans = append(plans, plan)
		byID[plan.ref.ID] = plan
	}
	return plans, byID, nil
}

func planPoint(ref driver.PointRef) (pointPlan, error) {
	if ref.Address.Kind != model.AddressSymbol {
		return pointPlan{}, fmt.Errorf("point %q: coap requires symbol addressing, got %q",
			ref.ID, ref.Address.Kind)
	}
	path := strings.TrimSpace(ref.Address.Path)
	if path == "" {
		return pointPlan{}, fmt.Errorf("point %q: address.path required (resource URI)", ref.ID)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// 查询串与片段没有对应的地址字段。塞进 path 会在编码时被当作路径段
	// 的一部分发给设备，得到一个 4.04 而不是「配置写错了」。
	if strings.ContainsAny(path, "?#") {
		return pointPlan{}, fmt.Errorf("point %q: address.path %q must not contain '?' or '#'",
			ref.ID, ref.Address.Path)
	}
	if ref.DataType == "" || !ref.DataType.Valid() {
		return pointPlan{}, fmt.Errorf("point %q: invalid dataType %q", ref.ID, ref.DataType)
	}
	if ref.Address.Length < 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.length must be >= 0", ref.ID)
	}
	return pointPlan{
		ref:      ref,
		path:     path,
		selector: ref.Address.Selector,
		typ:      ref.DataType,
		maxLen:   ref.Address.Length,
	}, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	devID   string
	addr    string
	dial    Dialer
	timeout time.Duration
	st      settings

	mu     sync.Mutex
	link   transport.Datagram
	closed bool

	mid      uint16
	tokenSeq uint64

	points []pointPlan
	byID   map[string]pointPlan

	// badDatagrams 记录解析失败的报文数，供 CLI/诊断观察。它不影响
	// 事务：UDP 上收到别人的包或被截断的包都不该让当前请求失败。
	badDatagrams uint64
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	link := c.link
	c.link = nil
	if link != nil {
		return link.Close()
	}
	return nil
}

func (c *conn) ensure(ctx context.Context) error {
	if c.closed {
		return driver.E(driver.ErrDisconnected, "open", c.devID, "connection closed", true, nil)
	}
	if c.link != nil {
		return nil
	}
	link, err := c.dial(ctx, c.addr, c.timeout)
	if err != nil {
		return driver.E(driver.ErrDisconnected, "dial", c.devID,
			fmt.Sprintf("connect %s failed", c.addr), true, err)
	}
	c.link = link
	return nil
}

func (c *conn) dropLink() {
	if c.link != nil {
		_ = c.link.Close()
		c.link = nil
	}
}

// nextToken 返回一个非零的 4 字节令牌。
//
// 令牌只在「这条请求与它的响应」之间有意义，但必须与迟到的旧响应区分开。
// 用一个进程内单调递增的计数器（初值取墙上时间）比随机数更容易在日志里
// 对齐，也让测试不依赖随机源。
func (c *conn) nextToken() []byte {
	c.tokenSeq++
	var tok [4]byte
	binary.BigEndian.PutUint32(tok[:], uint32(c.tokenSeq))
	return tok[:]
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// roundTrip 发送一条请求并等待匹配的响应。
//
// 匹配规则：
//   - 空 ACK：按 MID 匹配，表示「请求已收到、响应稍后单独发」；
//   - 搭载响应的 ACK：按 MID 匹配，响应体就在这条报文里；
//   - 分离响应（NON/CON）：按 Token 匹配，CON 还要回一条空 ACK。
func (c *conn) roundTrip(ctx context.Context, typ coapcodec.Type, code coapcodec.Code,
	opts []coapcodec.Option, payload []byte, op string) (coapcodec.Message, error) {

	var zero coapcodec.Message
	msg := coapcodec.Message{Type: typ, Code: code, Options: opts, Payload: payload}
	c.mid++
	msg.MID = c.mid
	msg.Token = c.nextToken()
	raw, err := coapcodec.Encode(msg)
	if err != nil {
		return zero, driver.E(driver.ErrInternal, op, c.devID, "encode request: "+err.Error(), false, err)
	}

	acknowledged := false
	var lastErr error
	for attempt := 0; attempt <= c.st.maxRetransmit; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, err)
		}
		if err := c.ensure(ctx); err != nil {
			return zero, err
		}
		deadline := c.attemptDeadline(ctx, attempt)
		if err := c.link.SetDeadline(deadline); err != nil {
			c.dropLink()
			return zero, driver.E(driver.ErrDisconnected, op, c.devID, "set udp deadline", true, err)
		}
		if err := c.link.Write(raw); err != nil {
			c.dropLink()
			return zero, driver.E(driver.ErrDisconnected, op, c.devID, "udp write failed", true, err)
		}

		retransmit := false
		for {
			dg, rerr := c.link.Read()
			if rerr != nil {
				if ctx.Err() != nil {
					return zero, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
				}
				if isTimeout(rerr) {
					lastErr = driver.E(driver.ErrTimeout, op, c.devID,
						fmt.Sprintf("no response within %s", time.Until(deadline).Round(time.Millisecond)),
						true, rerr)
					// 已经收到空 ACK：请求不会再被重发，只能等分离响应。
					if acknowledged {
						return zero, lastErr
					}
					retransmit = true
					break
				}
				c.dropLink()
				return zero, driver.E(driver.ErrDisconnected, op, c.devID, "udp read failed", true, rerr)
			}
			resp, derr := coapcodec.Decode(dg)
			if derr != nil {
				c.badDatagrams++
				continue
			}
			if resp.Type == coapcodec.TypeReset && resp.MID == msg.MID {
				return zero, driver.E(driver.ErrProtocol, op, c.devID,
					"peer answered with RST (request rejected)", false, nil)
			}
			if resp.Type == coapcodec.TypeAck && resp.MID == msg.MID {
				if resp.Code == coapcodec.CodeEmpty {
					// 分离响应：延长窗口，等对方把真正的响应发回来。
					acknowledged = true
					next := time.Now().Add(c.st.ackTimeout)
					if dl, ok := ctx.Deadline(); ok && dl.Before(next) {
						next = dl
					}
					deadline = next
					_ = c.link.SetDeadline(deadline)
					continue
				}
				if resp.Code.IsResponse() {
					if !bytes.Equal(resp.Token, msg.Token) {
						continue
					}
					return resp, nil
				}
				continue
			}
			if resp.Code.IsResponse() && bytes.Equal(resp.Token, msg.Token) {
				if resp.Type == coapcodec.TypeConfirmable {
					c.sendEmpty(coapcodec.TypeAck, resp.MID, deadline)
				}
				return resp, nil
			}
			// 与本事务无关的 CON：可能是 Observe 通知或上一次超时后迟到的
			// 响应。回 RST 让对方停止重传，但继续等自己的响应。
			if resp.Type == coapcodec.TypeConfirmable {
				c.sendEmpty(coapcodec.TypeReset, resp.MID, deadline)
			}
		}
		if !retransmit {
			break
		}
	}
	if lastErr == nil {
		lastErr = driver.E(driver.ErrTimeout, op, c.devID, "no response", true, nil)
	}
	return zero, lastErr
}

// attemptDeadline 计算第 attempt 次发送的等待截止时间。
//
// 首次用 ackTimeout，之后每次翻倍；同时不越过 ctx 的截止时间——设备级
// 超时是整轮采集的预算，越过它只会让剩余点位全部变成「context done」。
func (c *conn) attemptDeadline(ctx context.Context, attempt int) time.Time {
	d := c.st.ackTimeout
	for i := 0; i < attempt && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d <= 0 || d > 5*time.Minute {
		d = 5 * time.Minute
	}
	dl := time.Now().Add(d)
	if x, ok := ctx.Deadline(); ok && x.Before(dl) {
		dl = x
	}
	return dl
}

// sendEmpty 发送一条空报文（ACK 或 RST）。失败只影响对方的停止条件，
// 不影响当前事务，因此不向上报错。
func (c *conn) sendEmpty(typ coapcodec.Type, mid uint16, deadline time.Time) {
	if c.link == nil {
		return
	}
	b, err := coapcodec.Encode(coapcodec.Message{Type: typ, Code: coapcodec.CodeEmpty, MID: mid})
	if err != nil {
		return
	}
	_ = c.link.SetDeadline(deadline)
	_ = c.link.Write(b)
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
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			for _, rest := range refs[i:] {
				out = append(out, errSample(rest.ID, e))
			}
			if firstErr == nil {
				firstErr = e
			}
			break
		}
		plan, ok := c.byID[ref.ID]
		if !ok {
			var err error
			plan, err = planPoint(ref)
			if err != nil {
				e := driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
				out = append(out, errSample(ref.ID, e))
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
		}
		v, err := c.readOne(ctx, plan)
		if err != nil {
			e := pointError("read", c.devID, plan, err)
			out = append(out, errSample(plan.ref.ID, e))
			if firstErr == nil {
				firstErr = e
			}
			// 链路级失败后剩下的点位不可能成功；继续逐个等超时只会把
			// 一轮采集拖成 N 倍时长。
			if k := driver.KindOf(err); k == driver.ErrTimeout || k == driver.ErrDisconnected {
				for _, rest := range refs[i+1:] {
					out = append(out, errSample(rest.ID, e))
				}
				break
			}
			continue
		}
		out = append(out, driver.RawSample{PointID: plan.ref.ID, Value: v, Quality: model.QualityGood})
	}
	return out, firstErr
}

func (c *conn) readOne(ctx context.Context, plan pointPlan) (model.Value, error) {
	msg, err := c.get(ctx, plan.path, "read")
	if err != nil {
		return model.Value{}, err
	}
	return decodeValue(plan, msg)
}

// get 读取一个资源，自动续读 Block2。
//
// 默认不在首个请求里带 Block2 选项：让服务端决定是否分块、用什么块大小。
// 这样对「不支持 Block2」的极简服务端最友好；一旦响应里出现 Block2 且
// M=1，再按服务端给出的块号与大小继续取。
func (c *conn) get(ctx context.Context, path, op string) (coapcodec.Message, error) {
	var zero coapcodec.Message
	var body []byte
	var first coapcodec.Message
	num := uint32(0)
	size := c.st.blockSize
	usingBlock := size > 0
	fallbackDone := false

	for {
		var opts []coapcodec.Option
		opts = append(opts, pathOptions(path)...)
		if usingBlock {
			bv, err := coapcodec.EncodeBlock(coapcodec.Block{Num: num, Size: size})
			if err != nil {
				return zero, driver.E(driver.ErrInternal, op, c.devID, err.Error(), false, err)
			}
			opts = append(opts, coapcodec.Option{Number: coapcodec.OptBlock2, Value: bv})
		}
		resp, err := c.roundTrip(ctx, coapcodec.TypeConfirmable, coapcodec.CodeGET, opts, nil, op)
		if err != nil {
			return zero, err
		}
		if resp.Code == coapcodec.CodeBadOption && usingBlock && !fallbackDone {
			// 服务端不认识 Block2：退回「由服务端决定」，并且不再主动带选项。
			usingBlock = false
			fallbackDone = true
			num, size = 0, 0
			body = body[:0]
			first = coapcodec.Message{}
			continue
		}
		if !resp.Code.Success() {
			return zero, c.codeError(op, resp)
		}
		if first.Code == coapcodec.CodeEmpty {
			first = resp
			first.Payload = nil
		}

		blockValue, hasBlock := resp.Get(coapcodec.OptBlock2)
		if !hasBlock {
			body = append(body, resp.Payload...)
			break
		}
		blk, err := coapcodec.DecodeBlock(blockValue)
		if err != nil {
			return zero, driver.E(driver.ErrProtocol, op, c.devID,
				fmt.Sprintf("malformed Block2 option: %v", err), false, err)
		}
		if uint64(blk.Num)*uint64(blk.Size) != uint64(len(body)) {
			return zero, driver.E(driver.ErrProtocol, op, c.devID,
				fmt.Sprintf("Block2 out of order: block %d size %d at offset %d, have %d bytes",
					blk.Num, blk.Size, uint64(blk.Num)*uint64(blk.Size), len(body)), false, nil)
		}
		body = append(body, resp.Payload...)
		if len(body) > maxBodyBytes {
			return zero, driver.E(driver.ErrProtocol, op, c.devID,
				fmt.Sprintf("block-wise response exceeds %d bytes", maxBodyBytes), false, nil)
		}
		if !blk.More {
			break
		}
		usingBlock = true
		size = blk.Size
		num = blk.Num + 1
		if num > maxBlocks {
			return zero, driver.E(driver.ErrProtocol, op, c.devID,
				fmt.Sprintf("block-wise response exceeds %d blocks", maxBlocks), false, nil)
		}
	}
	first.Payload = body
	first.Options = withoutOption(first.Options, coapcodec.OptBlock2)
	return first, nil
}

func pathOptions(path string) []coapcodec.Option {
	var opts []coapcodec.Option
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if seg == "" {
			continue
		}
		opts = append(opts, coapcodec.Option{Number: coapcodec.OptURIPath, Value: []byte(seg)})
	}
	return opts
}

func withoutOption(opts []coapcodec.Option, num uint16) []coapcodec.Option {
	out := opts[:0]
	for _, o := range opts {
		if o.Number != num {
			out = append(out, o)
		}
	}
	return out
}

// decodeValue 按响应里的 Content-Format 解码。
//
// 这里的取舍是「按协议声明的格式解码，不猜」：一个 `Content-Format=50`
// 的响应里出现 `"42"` 是 JSON 字符串，不是数字；按文本解析会把引号一起
// 变成 string 点位的值。缺省（没有 Content-Format）按 text/plain 处理，
// 这是现场最常见的写法。
func decodeValue(plan pointPlan, msg coapcodec.Message) (model.Value, error) {
	format := uint16(coapcodec.FormatTextPlain)
	if v, ok := msg.UintOption(coapcodec.OptContentFormat); ok {
		format = uint16(v)
	}
	if len(msg.Payload) == 0 {
		return model.Value{}, fmt.Errorf("empty payload for %s", plan.path)
	}
	switch format {
	case coapcodec.FormatTextPlain:
		if plan.typ == model.TypeBytes {
			if err := checkLen(plan, len(msg.Payload)); err != nil {
				return model.Value{}, err
			}
			return model.BytesValue(msg.Payload), nil
		}
		v, err := textval.Decode(msg.Payload, plan.typ)
		if err != nil {
			return model.Value{}, err
		}
		return v, checkLen(plan, len(msg.Payload))
	case coapcodec.FormatJSON:
		if plan.typ == model.TypeBytes {
			return model.Value{}, fmt.Errorf("application/json cannot be decoded into bytes; " +
				"use text/plain or application/octet-stream")
		}
		var raw []byte
		var err error
		if plan.selector != "" {
			raw, err = textval.Select(msg.Payload, plan.selector)
		} else {
			raw, err = textval.JSONValue(msg.Payload)
		}
		if err != nil {
			return model.Value{}, err
		}
		if err := checkLen(plan, len(raw)); err != nil {
			return model.Value{}, err
		}
		return textval.Decode(raw, plan.typ)
	case coapcodec.FormatOctets:
		if err := checkLen(plan, len(msg.Payload)); err != nil {
			return model.Value{}, err
		}
		switch plan.typ {
		case model.TypeBytes:
			return model.BytesValue(msg.Payload), nil
		case model.TypeString:
			return model.TextValue(model.TypeString, string(msg.Payload)), nil
		default:
			return model.Value{}, fmt.Errorf(
				"application/octet-stream is only supported for bytes/string points, got %s", plan.typ)
		}
	default:
		return model.Value{}, fmt.Errorf("unsupported Content-Format %d (%s)",
			format, coapcodec.ContentFormatName(format))
	}
}

func checkLen(plan pointPlan, n int) error {
	if plan.maxLen > 0 && n > plan.maxLen {
		return fmt.Errorf("payload is %d bytes, exceeds address.length %d", n, plan.maxLen)
	}
	return nil
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
	fail := func(kind driver.ErrorKind, msg string, temporary bool, cause error) (driver.WriteResult, error) {
		err := driver.E(kind, "write", c.devID, fmt.Sprintf("point %q: %s", req.PointID, msg), temporary, cause)
		return driver.WriteResult{PointID: req.PointID, Quality: qualityFor(kind), Err: err}, err
	}

	plan, err := planPoint(driver.PointRef{
		ID: req.PointID, Address: req.Address, DataType: req.DataType, ByteOrder: req.ByteOrder,
	})
	if err != nil {
		return fail(driver.ErrAddress, err.Error(), false, err)
	}
	payload, format, err := encodeWritePayload(plan, req.Value, c.st.contentFormat)
	if err != nil {
		return fail(driver.ErrUnsupported, err.Error(), false, err)
	}

	opts := []coapcodec.Option{{
		Number: coapcodec.OptContentFormat,
		Value:  coapcodec.EncodeUint(uint32(format)),
	}}
	code := coapcodec.CodePUT
	if c.st.writeMethod == MethodPost {
		code = coapcodec.CodePOST
	}
	resp, err := c.roundTrip(ctx, coapcodec.TypeConfirmable, code,
		append(pathOptions(plan.path), opts...), payload, "write")
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: qualityFor(driver.KindOf(err)), Err: err}, err
	}
	if !resp.Code.Success() {
		e := c.codeError("write", resp)
		return driver.WriteResult{PointID: req.PointID, Quality: qualityFor(driver.KindOf(e)), Err: e}, e
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// encodeWritePayload 按设备级载荷格式编码写入值。
//
// JSON 的语义需要说清楚：PUT 是**替换整个资源**，不是合并。带 selector 时
// 驱动只发送 `{selector: value}` 这一棵子树，因此点位的 selector 应当与设备
// 资源的字段粒度一致；资源是多字段文档时，应把 selector 留空并让点位对应
// 整个文档。驱动刻意不做「先读回来再合并」——那会在一次写里偷偷插入一次读，
// 失败模式（读成功、写失败、读到旧值）远比现在复杂。
func encodeWritePayload(plan pointPlan, v model.Value, format string) ([]byte, uint16, error) {
	switch format {
	case FormatText:
		if plan.typ == model.TypeBytes {
			return []byte(v.Text), coapcodec.FormatTextPlain, nil
		}
		b, err := textval.Encode(v, plan.typ)
		return b, coapcodec.FormatTextPlain, err
	case FormatJSON:
		if plan.typ == model.TypeBytes {
			return nil, 0, fmt.Errorf("bytes cannot be written as JSON; set labels.contentFormat to text or octets")
		}
		b, err := jsonWritePayload(plan.selector, v, plan.typ)
		return b, coapcodec.FormatJSON, err
	case FormatOctets:
		switch plan.typ {
		case model.TypeBytes, model.TypeString:
			return []byte(v.Text), coapcodec.FormatOctets, nil
		default:
			return nil, 0, fmt.Errorf("application/octet-stream writes need a bytes/string point, got %s", plan.typ)
		}
	default:
		return nil, 0, fmt.Errorf("unknown content format %q", format)
	}
}

// jsonWritePayload 生成 JSON 标量，或按 selector 包成嵌套对象。
func jsonWritePayload(selector string, v model.Value, dt model.DataType) ([]byte, error) {
	scalar, err := jsonScalar(v, dt)
	if err != nil {
		return nil, err
	}
	if selector == "" {
		return scalar, nil
	}
	if strings.ContainsAny(selector, "[]") {
		// 数组元素写入需要决定「其余元素怎么办」：PUT 会替换整个数组，
		// 而我们只知道一个下标。与其发送一个把其它元素抹成 null 的文档，
		// 不如明确拒绝。
		return nil, fmt.Errorf("selector %q: writing into an array element is not supported; "+
			"configure the whole array resource instead", selector)
	}
	var node any = json.RawMessage(scalar)
	parts := strings.Split(selector, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.TrimSpace(parts[i]) == "" {
			return nil, fmt.Errorf("selector %q: empty path segment", selector)
		}
		node = map[string]any{parts[i]: node}
	}
	return json.Marshal(node)
}

func jsonScalar(v model.Value, dt model.DataType) ([]byte, error) {
	t := dt
	if t == "" {
		t = v.Type
	}
	var out []byte
	switch t {
	case model.TypeBool:
		out = []byte(strconv.FormatBool(v.Bool))
	case model.TypeString:
		b, err := json.Marshal(v.Text)
		if err != nil {
			return nil, err
		}
		out = b
	default:
		n, ok := v.AsFloat()
		if !ok {
			return nil, fmt.Errorf("cannot encode %s value as %s", v.Type, t)
		}
		// 与文本写共用同一条校验+格式化路径：越界、带小数、NaN/Inf 都在
		// 这里判错，不能靠 `int64(n)` 静默回绕（`int64(1e30)` 会变成
		// `-9223372036854775808`，写进设备的是一个完全错误的数）。
		txt, err := textval.FormatNumber(n, t)
		if err != nil {
			return nil, err
		}
		out = txt
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("value %q cannot be represented as JSON", out)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 探测与发现
// ---------------------------------------------------------------------------

// Probe 访问 `/.well-known/core` 验证链路。
//
// 2.xx 说明「能发现」；4.xx 说明「是个 CoAP 端点但不支持发现」——两者都
// 证明链路活着。只有 5.xx（服务端自己出错）算失败：那说明对端在，但业务
// 不可用，重试才有意义。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", true, nil)
	}
	opts := append(pathOptions(discoveryPath), coapcodec.Option{
		Number: coapcodec.OptAccept, Value: coapcodec.EncodeUint(uint32(coapcodec.FormatLink)),
	})
	resp, err := c.roundTrip(ctx, coapcodec.TypeConfirmable, coapcodec.CodeGET, opts, nil, "probe")
	if err != nil {
		return err
	}
	if resp.Code.Class() >= 2 && resp.Code.Class() <= 4 {
		return nil
	}
	return c.codeError("probe", resp)
}

// Discover 读取 `/.well-known/core` 并解析 link-format。
func (c *conn) Discover(ctx context.Context) ([]coapcodec.Link, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "discover", c.devID, "connection closed", true, nil)
	}
	resp, err := c.get(ctx, discoveryPath, "discover")
	if err != nil {
		return nil, err
	}
	if len(resp.Payload) == 0 {
		return nil, nil
	}
	links, err := coapcodec.ParseLinkFormat(resp.Payload)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, "discover", c.devID,
			"malformed link-format: "+err.Error(), false, err)
	}
	return links, nil
}

// BadDatagrams 返回解析失败的报文数（诊断用）。
func (c *conn) BadDatagrams() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.badDatagrams
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// codeError 把非 2.xx 响应翻译成分类错误，并保留对端的诊断载荷。
func (c *conn) codeError(op string, resp coapcodec.Message) error {
	kind := driver.ErrProtocol
	temporary := false
	switch resp.Code {
	case coapcodec.CodeNotFound:
		kind = driver.ErrAddress
	case coapcodec.CodeUnauthorized, coapcodec.CodeForbidden:
		kind = driver.ErrPermission
	case coapcodec.CodeBadOption, coapcodec.CodeMethodNotAllowed,
		coapcodec.CodeNotAcceptable, coapcodec.CodeUnsupportedFormat:
		kind = driver.ErrUnsupported
	case coapcodec.CodeBadRequest, coapcodec.CodePreconditionFailed,
		coapcodec.CodeRequestEntityTooLarge:
		kind = driver.ErrProtocol
	default:
		if resp.Code.Class() == 5 {
			temporary = true
		}
	}
	msg := "peer answered " + resp.Code.String()
	if d := diagnostic(resp.Payload); d != "" {
		msg += ": " + d
	}
	return driver.E(kind, op, c.devID, msg, temporary, nil)
}

// diagnostic 把对端的诊断载荷压成一行，最多 160 字节。
func diagnostic(payload []byte) string {
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{
		PointID: id,
		Quality: qualityFor(driver.KindOf(err)),
		Err:     err,
	}
}

func pointError(op, devID string, plan pointPlan, err error) error {
	kind := driver.KindOf(err)
	msg := err.Error()
	temporary := driver.IsRetryable(err)
	var oe *driver.OpError
	if errors.As(err, &oe) && oe.Msg != "" {
		msg = oe.Msg
		if oe.Err != nil {
			msg += ": " + oe.Err.Error()
		}
	}
	return driver.E(kind, op, devID,
		fmt.Sprintf("point %q (%s): %s", plan.ref.ID, plan.path, msg), temporary, nil)
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
