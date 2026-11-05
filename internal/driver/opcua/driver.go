// Package opcua 实现 OPC UA 二进制协议的客户端驱动。
//
// 分层与 Modbus / S7 一致：Meta（能力声明）→ conn（握手/会话/事务）→
// codec/opcua（纯函数编解码）→ transport（字节流）。区别在于 OPC UA
// 有三层状态：TCP 连接、安全通道、会话。三层都要显式建立、显式关闭，
// 且各自的失效有不同的恢复动作——把它们当成一个「已连接」布尔值，是
// 接入 OPC UA 最常见的错误来源。
//
// 能力边界：只支持 SecurityPolicy=None / SecurityMode=None 与匿名认证。
// 这不是「暂时没做」，而是明确的产品边界：证书轮换、信任列表、签名校验
// 属于另一套运维体系，混进来会让「连不上」的原因不可判定。
package opcua

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// 一次 Read 请求最多带的节点数。服务端的 MaxNodesPerRead 通常远大于
// 这个值（540 或更高），但不同厂商的默认 OperationLimits 差异很大；
// 100 是一个既不浪费往返、也不会被旧设备拒绝的折中值。
const maxNodesPerRead = 100

// 缓冲区大小。与 MaxMessageSize=0（不限）配合：本实现的单条响应都很小，
// 声明的缓冲区只影响对端的分块策略。
const bufferSize = 65535

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 OPC UA 客户端驱动。
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
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明 OPC UA 的能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:         "opcua",
		Name:       "OPC UA (binary, None/None)",
		Version:    "0.1.0",
		SDKVersion: driver.SDKVersion,
		Roles:      []driver.Role{driver.RoleMaster},
		Transports: []string{"tcp"},
		// 地址用 symbol 承载 NodeId 字符串：OPC UA 的寻址空间就是
		// 「节点标识」，不是区+偏移。把它硬塞进 register 会丢掉命名空间
		// 与字符串标识，现场那 90% 的 `ns=2;s=...` 点位就写不出来。
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapExperimental,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:4840", Scope: "channel",
				Help: "host:port，也可写 opc.tcp://host:port"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "本驱动单连接串行事务，保持 1"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"symbol"}, Scope: "point"},
			{Name: "address.path", Label: "NodeId", Type: "string", Required: true,
				Placeholder: "ns=2;s=Line1.Temperature", Scope: "point",
				Help: "支持 ns=2;s=... / ns=2;i=... / i=... / 裸字符串"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64",
					"float32", "float64", "string", "bytes"}, Scope: "point",
				Help: "写入时按此类型编码；读取时超出范围会判坏而不是截断"},
		},
		Description: "OPC UA 二进制客户端（SecurityPolicy None，匿名），支持批量读、写与端点发现",
	}
}

// Open 建立连接、安全通道与会话。
//
// 顺序不可省：HEL/ACK → OPN → CreateSession → ActivateSession。少一步
// 的表现各不相同：缺 OPN 时服务端直接断开；缺 CreateSession 时 Read
// 返回 BadSessionIdInvalid；缺 ActivateSession 时部分服务端允许读但拒绝
// 写，现场会变成「能看不能控」——最难排查的一种。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("opcua requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr, endpoint, err := parseTarget(t.Address)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}

	timeout := cfg.Channel.EffectiveTimeout()
	stream, err := d.dial(ctx, addr, timeout)
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}

	c := &conn{
		stream:   stream,
		deviceID: cfg.Device.ID,
		endpoint: endpoint,
		timeout:  timeout,
		session:  "uic-" + cfg.Device.ID,
	}
	if err := c.handshake(ctx); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return c, nil
}

// conn 是一条 OPC UA 连接。契约规定同一 Conn 不会被并发调用，这里仍然
// 加锁：Close 可能来自别的 goroutine（停机或热更新），无锁并发在 -race
// 下必须干净。
type conn struct {
	mu       sync.Mutex
	stream   transport.Stream
	deviceID string
	endpoint string
	session  string
	timeout  time.Duration

	seq    uint32
	reqID  uint32
	handle uint32

	channelID uint32
	tokenID   uint32

	sessionID  NodeID
	authToken  NodeID
	userPolicy string

	tokenIssued   time.Time
	tokenLifetime time.Duration

	closed bool
}

// NodeID 是 codec 里 NodeID 的别名，避免驱动文件里到处写包名。
type NodeID = ua.NodeID

// ---------------------------------------------------------------------------
// 传输
// ---------------------------------------------------------------------------

func (c *conn) setDeadline(ctx context.Context) {
	if s, ok := c.stream.(transport.DeadlineSetter); ok {
		if dl, ok := ctx.Deadline(); ok {
			_ = s.SetDeadline(dl)
		}
	}
}

func (c *conn) write(ctx context.Context, frame []byte) error {
	if c.stream == nil {
		return driver.E(driver.ErrDisconnected, "write", c.deviceID, "connection closed", true, nil)
	}
	c.setDeadline(ctx)
	if err := c.stream.Write(frame); err != nil {
		return c.netErr("write", err)
	}
	return nil
}

// readConnMessage 读一条连接层报文（HEL/ACK/ERR）。连接层头是 8 字节，
// 与安全报文（12 字节）不同，必须先按类型分流再决定读多少。
func (c *conn) readConnMessage(ctx context.Context) (string, []byte, error) {
	c.setDeadline(ctx)
	head, err := c.stream.ReadFull(8)
	if err != nil {
		return "", nil, c.netErr("read-header", err)
	}
	ch, err := ua.DecodeConnHeader(head)
	if err != nil {
		return "", nil, driver.E(driver.ErrProtocol, "read", c.deviceID, err.Error(), true, err)
	}
	n := int(ch.Size) - 8
	body, err := c.stream.ReadFull(n)
	if err != nil {
		return "", nil, c.netErr("read-body", err)
	}
	return ch.Type, body, nil
}

// readSecureMessage 读一条 OPN/MSG/CLO 报文。若对端在任何阶段回 ERR，
// 转成带分类的错误：ERR 的 ErrorCode 是服务端给出的真正原因，
// 直接当成「协议错」会丢掉最有用的信息。
func (c *conn) readSecureMessage(ctx context.Context) (ua.SecureMessage, error) {
	c.setDeadline(ctx)
	head, err := c.stream.ReadFull(8)
	if err != nil {
		return ua.SecureMessage{}, c.netErr("read-header", err)
	}
	ch, err := ua.DecodeConnHeader(head)
	if err != nil {
		return ua.SecureMessage{}, driver.E(driver.ErrProtocol, "read", c.deviceID, err.Error(), true, err)
	}
	if ch.Type == ua.MsgError {
		body, err := c.stream.ReadFull(int(ch.Size) - 8)
		if err != nil {
			return ua.SecureMessage{}, c.netErr("read-error", err)
		}
		em, derr := ua.DecodeErrorMessage(body)
		if derr != nil {
			return ua.SecureMessage{}, driver.E(driver.ErrProtocol, "read", c.deviceID, derr.Error(), true, derr)
		}
		return ua.SecureMessage{}, driver.E(statusKind(em.Code), "service", c.deviceID,
			"server error: "+em.Reason, false, em.Code)
	}
	switch ch.Type {
	case ua.MsgOpen, ua.MsgMessage, ua.MsgClose:
	default:
		return ua.SecureMessage{}, driver.E(driver.ErrProtocol, "read", c.deviceID,
			fmt.Sprintf("unexpected message type %q", ch.Type), true, nil)
	}
	rest, err := c.stream.ReadFull(int(ch.Size) - 8)
	if err != nil {
		return ua.SecureMessage{}, c.netErr("read-body", err)
	}
	raw := append(head, rest...)
	m, err := ua.DecodeSecureMessage(raw)
	if err != nil {
		return ua.SecureMessage{}, driver.E(driver.ErrProtocol, "decode", c.deviceID, err.Error(), true, err)
	}
	return m, nil
}

// exchange 发送一条 MSG 服务请求并等待同一 RequestId 的响应。
//
// RequestId 过滤不是洁癖：一次超时之后，迟到的响应仍会到达。不按
// RequestId 丢弃，下一个请求就会拿到上一个请求的结果——值看起来完全
// 正常，只是晚了一拍，这类错误在现场几乎无法定位。
func (c *conn) exchange(ctx context.Context, serviceID uint32, body []byte) (ua.SecureMessage, error) {
	c.seq++
	c.reqID++
	frame := ua.EncodeSecureMessage(ua.MsgMessage, c.channelID, c.tokenID,
		ua.SequenceHeader{Number: c.seq, RequestID: c.reqID}, serviceID, body)
	if err := c.write(ctx, frame); err != nil {
		return ua.SecureMessage{}, err
	}
	want := c.reqID
	for i := 0; i < 16; i++ {
		m, err := c.readSecureMessage(ctx)
		if err != nil {
			return ua.SecureMessage{}, err
		}
		if m.Seq.RequestID == want {
			return m, nil
		}
	}
	return ua.SecureMessage{}, driver.E(driver.ErrProtocol, "exchange", c.deviceID,
		"no response matched request id", true, nil)
}

// ---------------------------------------------------------------------------
// 握手
// ---------------------------------------------------------------------------

func (c *conn) handshake(ctx context.Context) error {
	hello := ua.Hello{
		Version:        0,
		ReceiveBufSize: bufferSize,
		SendBufSize:    bufferSize,
		MaxMessageSize: 0,
		MaxChunkCount:  0,
		EndpointURL:    c.endpoint,
	}
	if err := c.write(ctx, ua.EncodeHello(hello)); err != nil {
		return err
	}
	typ, body, err := c.readConnMessage(ctx)
	if err != nil {
		return err
	}
	switch typ {
	case ua.MsgAck:
		ack, derr := ua.DecodeAcknowledge(body)
		if derr != nil {
			return driver.E(driver.ErrProtocol, "hello", c.deviceID, derr.Error(), true, derr)
		}
		if ack.ReceiveBufSize == 0 || ack.SendBufSize == 0 {
			return driver.E(driver.ErrProtocol, "hello", c.deviceID,
				"server acknowledged zero buffer size", false, nil)
		}
	case ua.MsgError:
		em, derr := ua.DecodeErrorMessage(body)
		if derr != nil {
			return driver.E(driver.ErrProtocol, "hello", c.deviceID, derr.Error(), true, derr)
		}
		return driver.E(statusKind(em.Code), "hello", c.deviceID, "server refused connection: "+em.Reason, false, em.Code)
	default:
		return driver.E(driver.ErrProtocol, "hello", c.deviceID,
			fmt.Sprintf("expected ACK, got %q", typ), true, nil)
	}

	if err := c.openChannel(ctx, ua.TokenRequestIssue); err != nil {
		return err
	}
	if err := c.createSession(ctx); err != nil {
		return err
	}
	return c.activateSession(ctx)
}

// openChannel 打开或续订安全通道。
func (c *conn) openChannel(ctx context.Context, requestType uint32) error {
	req := ua.OpenSecureChannelRequest{
		Header:                c.newHeader(),
		ClientProtocolVersion: 0,
		RequestType:           requestType,
		SecurityMode:          ua.SecurityModeNone,
		ClientNonce:           nil,
		RequestedLifetime:     3600000, // 1 小时；服务端可下调
	}
	c.seq++
	c.reqID++
	frame := ua.EncodeOpenMessage(c.channelID,
		ua.AsymmetricSecurityHeader{PolicyURI: ua.PolicyNone},
		ua.SequenceHeader{Number: c.seq, RequestID: c.reqID},
		ua.SvcOpenSecureChannelRequest, req.Encode())
	if err := c.write(ctx, frame); err != nil {
		return err
	}
	want := c.reqID
	var m ua.SecureMessage
	for i := 0; i < 16; i++ {
		got, err := c.readSecureMessage(ctx)
		if err != nil {
			return err
		}
		if got.Seq.RequestID == want {
			m = got
			break
		}
	}
	if m.ServiceID == 0 {
		return driver.E(driver.ErrProtocol, "open-channel", c.deviceID, "no matching response", true, nil)
	}
	if m.ServiceID == ua.SvcServiceFaultResponse {
		return c.faultErr("open-channel", m.Body)
	}
	resp, err := ua.DecodeOpenSecureChannelResponse(m.Body)
	if err != nil {
		return driver.E(driver.ErrProtocol, "open-channel", c.deviceID, err.Error(), true, err)
	}
	if resp.Header.ServiceResult.IsBad() {
		return driver.E(statusKind(resp.Header.ServiceResult), "open-channel", c.deviceID,
			resp.Header.ServiceResult.Text(), false, resp.Header.ServiceResult)
	}
	switch {
	case m.ChannelID != 0:
		c.channelID = m.ChannelID
	case resp.Token.ChannelID != 0:
		c.channelID = resp.Token.ChannelID
	}
	c.tokenID = resp.Token.TokenID
	c.tokenIssued = time.Now()
	if resp.Token.RevisedLifetime > 0 {
		c.tokenLifetime = time.Duration(resp.Token.RevisedLifetime) * time.Millisecond
	}
	return nil
}

// maybeRenew 在令牌到期前续订。续订失败必须让本次请求失败：拿着过期
// 令牌继续发请求，服务端会回 BadSecureChannelIdInvalid，而驱动会把它
// 当断链重连——两者表现相似，但日志完全不同，容易误判成网络问题。
func (c *conn) maybeRenew(ctx context.Context) error {
	if c.tokenLifetime <= 0 {
		return nil
	}
	if time.Since(c.tokenIssued) < c.tokenLifetime*3/4 {
		return nil
	}
	return c.openChannel(ctx, ua.TokenRequestRenew)
}

func (c *conn) createSession(ctx context.Context) error {
	req := ua.CreateSessionRequest{
		Header: c.newHeader(),
		ClientDescription: ua.ApplicationDescription{
			ApplicationURI:  "urn:uic:client",
			ProductURI:      "urn:uic",
			ApplicationName: ua.LocalizedText{Text: "UIC"},
			ApplicationType: ua.AppTypeClient,
		},
		EndpointURL:             c.endpoint,
		SessionName:             c.session,
		RequestedSessionTimeout: 600000,
		MaxResponseMessageSize:  0,
	}
	m, err := c.exchange(ctx, ua.SvcCreateSessionRequest, req.Encode())
	if err != nil {
		return err
	}
	if m.ServiceID == ua.SvcServiceFaultResponse {
		return c.faultErr("create-session", m.Body)
	}
	resp, err := ua.DecodeCreateSessionResponse(m.Body)
	if err != nil {
		return driver.E(driver.ErrProtocol, "create-session", c.deviceID, err.Error(), true, err)
	}
	if resp.Header.ServiceResult.IsBad() {
		return driver.E(statusKind(resp.Header.ServiceResult), "create-session", c.deviceID,
			resp.Header.ServiceResult.Text(), false, resp.Header.ServiceResult)
	}
	c.sessionID = resp.SessionID
	c.authToken = resp.AuthenticationToken
	// 端点信息只在 CreateSession 响应里出现一次；把匿名策略的 PolicyID
	// 在这里就记住，避免为拿它多发一次 GetEndpoints。
	if policy, ok := anonymousPolicyID(resp.ServerEndpoints); ok {
		c.userPolicy = policy
	}
	return nil
}

// anonymousPolicyID 从服务端端点里挑出匿名令牌的 PolicyID。
//
// 不能硬编码 "anonymous"：PolicyID 由服务端定义，不同产品取值不同
// （有的叫 "anonymous"，有的叫 "Anonymous"，有的是 UUID）。
// 用错的值会得到 BadIdentityTokenInvalid，而这个错误码看起来像
// 「不允许匿名」，实际只是字符串不匹配。
func anonymousPolicyID(endpoints []ua.EndpointDescription) (string, bool) {
	for _, ep := range endpoints {
		for _, t := range ep.UserIdentityTokens {
			if t.TokenType == ua.TokenTypeAnonymous {
				return t.PolicyID, true
			}
		}
	}
	return "", false
}

func (c *conn) activateSession(ctx context.Context) error {
	req := ua.ActivateSessionRequest{
		Header:            c.newHeader(),
		UserIdentityToken: ua.AnonymousIdentityToken(c.userPolicy),
	}
	m, err := c.exchange(ctx, ua.SvcActivateSessionRequest, req.Encode())
	if err != nil {
		return err
	}
	if m.ServiceID == ua.SvcServiceFaultResponse {
		return c.faultErr("activate-session", m.Body)
	}
	resp, err := ua.DecodeActivateSessionResponse(m.Body)
	if err != nil {
		return driver.E(driver.ErrProtocol, "activate-session", c.deviceID, err.Error(), true, err)
	}
	if resp.Header.ServiceResult.IsBad() {
		return driver.E(statusKind(resp.Header.ServiceResult), "activate-session", c.deviceID,
			resp.Header.ServiceResult.Text(), false, resp.Header.ServiceResult)
	}
	for _, s := range resp.Results {
		if s.IsBad() {
			return driver.E(statusKind(s), "activate-session", c.deviceID, s.Text(), false, s)
		}
	}
	return nil
}

func (c *conn) newHeader() ua.RequestHeader {
	c.handle++
	return ua.RequestHeader{
		AuthenticationToken: c.authToken,
		Timestamp:           time.Now(),
		RequestHandle:       c.handle,
		TimeoutHint:         uint32(c.timeout / time.Millisecond),
	}
}

// faultErr 把 ServiceFault 转成分类错误。
func (c *conn) faultErr(op string, body []byte) error {
	f, err := ua.DecodeServiceFault(body)
	if err != nil {
		return driver.E(driver.ErrProtocol, op, c.deviceID, "service fault: "+err.Error(), true, err)
	}
	s := f.Header.ServiceResult
	return driver.E(statusKind(s), op, c.deviceID, s.Text(), false, s)
}

// ---------------------------------------------------------------------------
// Conn 实现
// ---------------------------------------------------------------------------

// Probe 读服务端的 ServerStatus.CurrentTime。
//
// 任何「节点不存在」之类的单项错误都算链路可达：探活要回答的是
// 「服务端还应答吗」，不是「这个标准节点在不在」。把它当断链会让
// 一台精简实现的服务器永远被判死，而它其实工作正常。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil {
		return driver.E(driver.ErrDisconnected, "probe", c.deviceID, "connection closed", true, nil)
	}
	if err := c.maybeRenew(ctx); err != nil {
		return err
	}
	_, err := c.readNodes(ctx, []NodeID{ua.Numeric(0, 2258)})
	return err
}

// readNodes 读取若干个 NodeId 的 Value 属性，返回与输入等长的 DataValue。
func (c *conn) readNodes(ctx context.Context, nodes []NodeID) ([]ua.DataValue, error) {
	req := ua.ReadRequest{
		Header:             c.newHeader(),
		MaxAge:             0,
		TimestampsToReturn: ua.TimestampsBoth,
		Nodes:              make([]ua.ReadValueID, len(nodes)),
	}
	for i, n := range nodes {
		req.Nodes[i] = ua.ReadValueID{NodeID: n, AttributeID: ua.AttrValue}
	}
	m, err := c.exchange(ctx, ua.SvcReadRequest, req.Encode())
	if err != nil {
		return nil, err
	}
	if m.ServiceID == ua.SvcServiceFaultResponse {
		return nil, c.faultErr("read", m.Body)
	}
	if m.ServiceID != ua.SvcReadResponse {
		return nil, driver.E(driver.ErrProtocol, "read", c.deviceID,
			fmt.Sprintf("unexpected service response %d", m.ServiceID), true, nil)
	}
	resp, err := ua.DecodeReadResponse(m.Body)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, "read", c.deviceID, err.Error(), true, err)
	}
	if resp.Header.ServiceResult.IsBad() {
		return nil, driver.E(statusKind(resp.Header.ServiceResult), "read", c.deviceID,
			resp.Header.ServiceResult.Text(), false, resp.Header.ServiceResult)
	}
	if len(resp.Values) != len(nodes) {
		return nil, driver.E(driver.ErrProtocol, "read", c.deviceID,
			fmt.Sprintf("got %d values for %d nodes", len(resp.Values), len(nodes)), true, nil)
	}
	return resp.Values, nil
}

type readItem struct {
	point driver.PointRef
	node  ua.NodeID
}

// Read 批量读取。每个点位一个 NodeId，按 maxNodesPerRead 切块。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stream == nil {
		return nil, driver.E(driver.ErrDisconnected, "read", c.deviceID, "connection closed", true, nil)
	}
	if err := c.maybeRenew(ctx); err != nil {
		return nil, err
	}

	var (
		items  []readItem
		out    []driver.RawSample
		badPre []driver.RawSample
	)
	for _, span := range spans {
		for _, p := range span.Points {
			node, err := ua.ParseNodeID(p.Address.Path)
			if err != nil || node.IsNull() {
				msg := "empty node id for point " + p.ID
				if err != nil {
					msg = err.Error()
				}
				e := driver.E(driver.ErrAddress, "encode", c.deviceID, msg, false, err)
				badPre = append(badPre, driver.RawSample{
					PointID: p.ID, Quality: model.QualityAddressError, Err: e,
				})
				continue
			}
			items = append(items, readItem{point: p, node: node})
		}
	}
	out = append(out, badPre...)
	var firstErr error
	if len(badPre) > 0 {
		firstErr = badPre[0].Err
	}
	if len(items) == 0 {
		return out, firstErr
	}

	good := 0

	for start := 0; start < len(items); start += maxNodesPerRead {
		end := start + maxNodesPerRead
		if end > len(items) {
			end = len(items)
		}
		chunk := items[start:end]
		nodes := make([]ua.NodeID, len(chunk))
		for i, it := range chunk {
			nodes[i] = it.node
		}

		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.deviceID, "context done", true, err)
			if firstErr == nil {
				firstErr = e
			}
			for _, it := range chunk {
				out = append(out, errSample(it.point.ID, e))
			}
			continue
		}

		values, err := c.readNodes(ctx, nodes)
		if err != nil {
			// 链路/会话级错误：本块与后续块都不必再试，但剩余点位必须
			// 显式标记，否则上层只会看到「点位缺失」，把真正的原因
			// （会话失效）掩盖成配置问题。
			if firstErr == nil {
				firstErr = err
			}
			for _, rest := range items[start:] {
				out = append(out, errSample(rest.point.ID, err))
			}
			return out, firstErr
		}

		for i, it := range chunk {
			dv := values[i]
			if dv.Status.IsBad() {
				e := driver.E(statusKind(dv.Status), "read", c.deviceID,
					dv.Status.Text(), false, dv.Status)
				out = append(out, driver.RawSample{
					PointID: it.point.ID, Quality: ua.QualityOf(dv.Status), Err: e,
				})
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			if dv.Value == nil {
				e := driver.E(driver.ErrProtocol, "read", c.deviceID,
					"server returned no value for "+it.point.ID, false, nil)
				out = append(out, driver.RawSample{PointID: it.point.ID, Quality: model.QualityBad, Err: e})
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			val, derr := ua.DecodeValue(*dv.Value, it.point.DataType)
			if derr != nil {
				e := driver.E(driver.ErrProtocol, "decode", c.deviceID,
					it.point.ID+": "+derr.Error(), false, derr)
				out = append(out, driver.RawSample{PointID: it.point.ID, Quality: model.QualityBad, Err: e})
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			out = append(out, driver.RawSample{
				PointID: it.point.ID,
				Value:   val,
				Quality: ua.QualityOf(dv.Status),
			})
			good++
		}
	}
	// 与 Modbus / S7 保持同一约定：有任何一个点位成功就向上返回 nil，
	// 单点错误通过 sample.Err 表达；全部失败才返回错误。这样「一个点位
	// 配错」不会把整台设备判故障，而「整台设备坏了」仍然能被熔断器看到。
	if good == 0 && firstErr != nil {
		return out, firstErr
	}
	return out, nil
}

// Write 批量写入 Value 属性。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	if c.stream == nil {
		return nil, driver.E(driver.ErrDisconnected, "write", c.deviceID, "connection closed", true, nil)
	}
	if err := c.maybeRenew(ctx); err != nil {
		return nil, err
	}

	type pending struct {
		req  driver.WriteRequest
		node ua.NodeID
	}
	var valid []pending
	for _, req := range reqs {
		node, err := ua.ParseNodeID(req.Address.Path)
		if err != nil || node.IsNull() {
			results = append(results, driver.WriteResult{
				PointID: req.PointID, Quality: model.QualityAddressError,
				Err: driver.E(driver.ErrAddress, "encode", c.deviceID, "invalid node id", false, err),
			})
			continue
		}
		if _, err := ua.EncodeValue(req.DataType, req.Value); err != nil {
			results = append(results, driver.WriteResult{
				PointID: req.PointID, Quality: model.QualityBad,
				Err: driver.E(driver.ErrUnsupported, "encode", c.deviceID, err.Error(), false, err),
			})
			continue
		}
		valid = append(valid, pending{req: req, node: node})
	}
	if len(valid) == 0 {
		return results, nil
	}

	for start := 0; start < len(valid); start += maxNodesPerRead {
		end := start + maxNodesPerRead
		if end > len(valid) {
			end = len(valid)
		}
		chunk := valid[start:end]

		nodes := make([]ua.WriteValue, len(chunk))
		for i, p := range chunk {
			variant, err := ua.EncodeValue(p.req.DataType, p.req.Value)
			if err != nil {
				// 前面已校验过，这里只可能是并发修改；保守处理为失败。
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: model.QualityBad,
					Err: driver.E(driver.ErrInternal, "encode", c.deviceID, err.Error(), false, err),
				})
				continue
			}
			nodes[i] = ua.WriteValue{
				NodeID:      p.node,
				AttributeID: ua.AttrValue,
				Value:       ua.DataValue{Value: &variant, SourceTimestamp: time.Now()},
			}
		}
		req := ua.WriteRequest{Header: c.newHeader(), Nodes: nodes}
		m, err := c.exchange(ctx, ua.SvcWriteRequest, req.Encode())
		if err != nil {
			for _, p := range chunk {
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: model.QualityCommError, Err: err,
				})
			}
			return results, err
		}
		if m.ServiceID == ua.SvcServiceFaultResponse {
			ferr := c.faultErr("write", m.Body)
			for _, p := range chunk {
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: model.QualityBad, Err: ferr,
				})
			}
			return results, ferr
		}
		resp, derr := ua.DecodeWriteResponse(m.Body)
		if derr != nil {
			e := driver.E(driver.ErrProtocol, "write", c.deviceID, derr.Error(), true, derr)
			for _, p := range chunk {
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: model.QualityBad, Err: e,
				})
			}
			return results, e
		}
		if resp.Header.ServiceResult.IsBad() {
			e := driver.E(statusKind(resp.Header.ServiceResult), "write", c.deviceID,
				resp.Header.ServiceResult.Text(), false, resp.Header.ServiceResult)
			for _, p := range chunk {
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: model.QualityBad, Err: e,
				})
			}
			return results, e
		}
		for i, p := range chunk {
			if i >= len(resp.Results) {
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: model.QualityBad,
					Err: driver.E(driver.ErrProtocol, "write", c.deviceID,
						"missing write result", true, nil),
				})
				continue
			}
			s := resp.Results[i]
			if s.IsBad() {
				results = append(results, driver.WriteResult{
					PointID: p.req.PointID, Quality: ua.QualityOf(s),
					Err: driver.E(statusKind(s), "write", c.deviceID, s.Text(), false, s),
				})
				continue
			}
			results = append(results, driver.WriteResult{
				PointID: p.req.PointID, OK: true, Quality: model.QualityGood,
			})
		}
	}
	return results, nil
}

// Close 依次关闭会话、安全通道、TCP 连接。每一步都可能失败，但都必须
// 尝试：只关 TCP 会让服务端保留会话直到超时，某些产品因此拒绝新的
// 连接（「会话数已满」），现场表现为「重启网关后连不上了」。
func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stream == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if !c.sessionID.IsNull() {
		req := ua.CloseSessionRequest{Header: c.newHeader(), DeleteSubscriptions: true}
		_, _ = c.exchange(ctx, ua.SvcCloseSessionRequest, req.Encode())
	}
	if c.channelID != 0 {
		req := ua.CloseSecureChannelRequest{Header: c.newHeader()}
		c.seq++
		c.reqID++
		frame := ua.EncodeSecureMessage(ua.MsgClose, c.channelID, c.tokenID,
			ua.SequenceHeader{Number: c.seq, RequestID: c.reqID},
			ua.SvcCloseSecureChannelRequest, req.Encode())
		_ = c.write(ctx, frame)
	}
	err := c.stream.Close()
	c.stream = nil
	return err
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func (c *conn) netErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return driver.E(driver.ErrTimeout, op, c.deviceID, "network timeout", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return driver.E(driver.ErrTimeout, op, c.deviceID, "deadline exceeded", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.deviceID, "network error", true, err)
}

// statusKind 把 OPC UA 状态码映射到驱动的错误分类。
//
// 分类决定上层是否重试与是否熔断，因此这里必须区分「这个点位不存在」
// 与「服务端不让我读」——两者都是 Bad，但前者是配置问题（不熔断），
// 后者是权限问题（不重试）。一刀切成 protocol 会让熔断器被配置错误
// 反复触发，把整台设备拖下水。
func statusKind(s ua.StatusCode) driver.ErrorKind {
	switch s {
	case ua.StatusBadNodeIDUnknown, ua.StatusBadNodeIDInvalid, ua.StatusBadAttributeInvalid:
		return driver.ErrAddress
	case ua.StatusBadUserAccessDenied, ua.StatusBadSecurityChecks, ua.StatusBadNotWritable:
		return driver.ErrPermission
	case ua.StatusBadServiceUnsupported:
		return driver.ErrUnsupported
	case ua.StatusBadTimeout, ua.StatusBadWaitingForData:
		return driver.ErrTimeout
	case ua.StatusBadSessionIDInvalid, ua.StatusBadSessionClosed,
		ua.StatusBadSecureChannelID, ua.StatusBadNoCommunication,
		ua.StatusBadNotConnected, ua.StatusBadCommunicationError:
		return driver.ErrDisconnected
	}
	return driver.ErrProtocol
}

func errSample(pointID string, err error) driver.RawSample {
	return driver.RawSample{PointID: pointID, Quality: qualityFor(err), Err: err}
}

func qualityFor(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrPermission:
		return model.QualityBad
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrUnsupported:
		return model.QualityDisabled
	}
	return model.QualityBad
}

// parseTarget 归一化通道地址：允许 host:port 与 opc.tcp://host:port 两种写法。
//
// 统一在这里做，是因为配置里两种写法现场都会出现（抄自 UaExpert 的是 URL，
// 手写的是 host:port）。让驱动同时接受，比在文档里要求用户改配置更可靠。
func parseTarget(raw string) (addr, endpoint string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("transport.address required (host:port)")
	}
	if strings.Contains(raw, "://") {
		lower := strings.ToLower(raw)
		if !strings.HasPrefix(lower, "opc.tcp://") {
			return "", "", fmt.Errorf("unsupported scheme in %q (only opc.tcp://)", raw)
		}
		host := raw[len("opc.tcp://"):]
		host = strings.TrimSuffix(host, "/")
		if host == "" {
			return "", "", fmt.Errorf("invalid endpoint %q", raw)
		}
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(strings.Trim(host, "[]"), "4840")
		}
		return host, "opc.tcp://" + host, nil
	}
	host := raw
	if _, port, serr := net.SplitHostPort(host); serr == nil {
		// 端口必须是数字。SplitHostPort 只切分不校验，"host:badport"
		// 会原样通过，直到 dial 阶段才变成「连接失败」——把配置错误
		// 伪装成网络问题。
		if perr := validatePort(port); perr != nil {
			return "", "", fmt.Errorf("invalid address %q: %v", raw, perr)
		}
	} else {
		// 只有纯主机名时才补默认端口；`host:bad` 这类错误必须原样暴露，
		// 否则会变成「连 4840 超时」，掩盖真正的配置错误。
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			return "", "", fmt.Errorf("invalid address %q: %v", raw, serr)
		}
		host = net.JoinHostPort(strings.Trim(host, "[]"), "4840")
	}
	return host, "opc.tcp://" + host, nil
}

// validatePort 校验端口是 1..65535 的十进制数字。
func validatePort(p string) error {
	n, err := strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("port %q is not a number", p)
	}
	if n <= 0 || n > 65535 {
		return fmt.Errorf("port %d out of range 1..65535", n)
	}
	return nil
}
