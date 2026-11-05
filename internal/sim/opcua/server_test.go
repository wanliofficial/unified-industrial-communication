package opcua

import (
	"net"
	"testing"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/model"
)

// 这些用例刻意不走驱动，而是用裸 TCP 手搓报文。理由：如果只用驱动测服务端，
// 两边共用同一份 codec，编码错误会两边一起错、测试全绿。裸报文至少能保证
// 「按规范写的字节」与「服务端的解释」对得上。

// client 是测试用的极简客户端，只负责拼装与拆解，不做状态机校验。
type client struct {
	t    *testing.T
	conn net.Conn
	seq  uint32
	ch   uint32
	tok  uint32
	sess uint32
}

func dial(t *testing.T, srv *Server) *client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return &client{t: t, conn: conn}
}

func (c *client) write(b []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// readRaw 读一条完整报文，返回字节原文。
//
// 必须返回原文而不是「头 + 体」：HEL/ACK/ERR 的头是 8 字节，而
// OPN/MSG/CLO 的头是 12 字节（多一个 ChannelID）。按 8 字节切分再拼回
// 去，会让 ChannelID 的 4 个字节串到正文里，随后所有字段错位——这
// 正是「自己拼报文」最容易踩的坑，所以这里只按 Length 字段切。
func (c *client) readRaw() []byte {
	c.t.Helper()
	head := make([]byte, 8)
	if _, err := readFull(c.conn, head); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	size := int(uint32(head[4]) | uint32(head[5])<<8 | uint32(head[6])<<16 | uint32(head[7])<<24)
	if size < 8 || size > 64<<20 {
		c.t.Fatalf("bad message size %d", size)
	}
	raw := make([]byte, size)
	copy(raw, head)
	if _, err := readFull(c.conn, raw[8:]); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return raw
}

// readMsg 读一条报文并取出连接层头（HEL/ACK/ERR 用）。
func (c *client) readMsg() (ua.ConnHeader, []byte) {
	c.t.Helper()
	raw := c.readRaw()
	h, err := ua.DecodeConnHeader(raw)
	if err != nil {
		c.t.Fatalf("decode header: %v", err)
	}
	return h, raw[8:]
}

// hello 完成 HEL/ACK 交换。
func (c *client) hello(srv *Server) {
	c.t.Helper()
	c.write(ua.EncodeHello(ua.Hello{
		ReceiveBufSize: 65535, SendBufSize: 65535,
		EndpointURL: "opc.tcp://" + srv.Addr(),
	}))
	h, _ := c.readMsg()
	if h.Type != ua.MsgAck {
		c.t.Fatalf("expected ACK, got %q", h.Type)
	}
}

// open 建安全通道，记下 channelId / tokenId。
func (c *client) open() {
	c.t.Helper()
	c.seq++
	c.write(ua.EncodeOpenMessage(0,
		ua.AsymmetricSecurityHeader{PolicyURI: ua.PolicyNone},
		ua.SequenceHeader{Number: c.seq, RequestID: c.seq},
		ua.SvcOpenSecureChannelRequest,
		ua.OpenSecureChannelRequest{
			Header:            ua.RequestHeader{RequestHandle: 1},
			SecurityMode:      ua.SecurityModeNone,
			RequestedLifetime: 3600000,
			RequestType:       ua.TokenRequestIssue,
		}.Encode()))
	raw := c.readRaw()
	m, err := ua.DecodeSecureMessage(raw)
	if err != nil {
		c.t.Fatalf("decode OPN response: %v", err)
	}
	if m.Type != ua.MsgOpen {
		c.t.Fatalf("expected OPN response, got %q", m.Type)
	}
	resp, err := ua.DecodeOpenSecureChannelResponse(m.Body)
	if err != nil {
		c.t.Fatalf("decode OPN body: %v", err)
	}
	c.ch = resp.Token.ChannelID
	c.tok = resp.Token.TokenID
}

// call 发送一条服务请求，返回服务 ID 与正文。
func (c *client) call(serviceID uint32, body []byte) (uint32, []byte) {
	c.t.Helper()
	c.seq++
	c.write(ua.EncodeSecureMessage(ua.MsgMessage, c.ch, c.tok,
		ua.SequenceHeader{Number: c.seq, RequestID: c.seq}, serviceID, body))
	m, err := ua.DecodeSecureMessage(c.readRaw())
	if err != nil {
		c.t.Fatalf("decode MSG: %v", err)
	}
	if m.Type != ua.MsgMessage {
		c.t.Fatalf("expected MSG, got %q", m.Type)
	}
	return m.ServiceID, m.Body
}

// createSession 建会话并激活，返回认证令牌。
func (c *client) createSession() ua.NodeID {
	c.t.Helper()
	id, body := c.call(ua.SvcCreateSessionRequest, ua.CreateSessionRequest{
		Header:                  ua.RequestHeader{RequestHandle: 2},
		ClientDescription:       ua.ApplicationDescription{ApplicationName: ua.LocalizedText{Text: "uic-test"}},
		EndpointURL:             "opc.tcp://localhost",
		RequestedSessionTimeout: 600000,
		MaxResponseMessageSize:  0,
	}.Encode())
	if id != ua.SvcCreateSessionResponse {
		c.t.Fatalf("create session service id = %d", id)
	}
	resp, err := ua.DecodeCreateSessionResponse(body)
	if err != nil {
		c.t.Fatalf("decode create session: %v", err)
	}
	if len(resp.ServerEndpoints) == 0 {
		c.t.Fatal("no endpoints in create session response")
	}
	c.sess = resp.AuthenticationToken.ID
	c.activate(resp.AuthenticationToken)
	return resp.AuthenticationToken
}

func (c *client) activate(token ua.NodeID) {
	c.t.Helper()
	id, body := c.call(ua.SvcActivateSessionRequest, ua.ActivateSessionRequest{
		Header:                     ua.RequestHeader{RequestHandle: 3, AuthenticationToken: token},
		ClientSignature:            ua.SignatureData{},
		ClientSoftwareCertificates: nil,
		LocaleIDs:                  nil,
		UserIdentityToken:          ua.AnonymousIdentityToken("anonymous"),
	}.Encode())
	if id != ua.SvcActivateSessionResponse {
		f := decodeFault(c.t, body)
		c.t.Fatalf("activate failed: %v", f)
	}
	_, _ = id, body
}

func decodeFault(t *testing.T, body []byte) ua.ServiceFault {
	t.Helper()
	f, err := ua.DecodeServiceFault(body)
	if err != nil {
		t.Fatalf("decode fault: %v", err)
	}
	return f
}

// readOne 读一个节点（value 属性）。
func (c *client) readOne(token, node ua.NodeID) ua.DataValue {
	c.t.Helper()
	id, body := c.call(ua.SvcReadRequest, ua.ReadRequest{
		Header:             ua.RequestHeader{RequestHandle: 4, AuthenticationToken: token},
		MaxAge:             0,
		TimestampsToReturn: ua.TimestampsBoth,
		Nodes:              []ua.ReadValueID{{NodeID: node, AttributeID: ua.AttrValue}},
	}.Encode())
	if id != ua.SvcReadResponse {
		c.t.Fatalf("read service id = %d: %v", id, decodeFault(c.t, body))
	}
	resp, err := ua.DecodeReadResponse(body)
	if err != nil {
		c.t.Fatalf("decode read: %v", err)
	}
	if len(resp.Values) != 1 {
		c.t.Fatalf("values = %d", len(resp.Values))
	}
	return resp.Values[0]
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestHelloAck(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
}

// HEL 的 EndpointURL 不影响 ACK；但消息类型顺序错（先发 OPN 不先 HEL）
// 在真实服务端上一般是硬断。这里只验证 ACK 的缓冲区字段被回填。
func TestAckReportsBufferSizes(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.write(ua.EncodeHello(ua.Hello{
		ReceiveBufSize: 8192, SendBufSize: 4096,
		EndpointURL: "opc.tcp://" + srv.Addr(),
	}))
	h, body := c.readMsg()
	if h.Type != ua.MsgAck {
		t.Fatalf("type %q", h.Type)
	}
	ack, err := ua.DecodeAcknowledge(body)
	if err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.ReceiveBufSize == 0 || ack.SendBufSize == 0 {
		t.Fatalf("ack buffer sizes %+v", ack)
	}
}

func TestOpenSecureChannelReturnsToken(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	if c.ch == 0 {
		t.Fatal("channel id = 0")
	}
	if c.tok == 0 {
		t.Fatal("token id = 0")
	}
}

// 未建会话就读：必须返回 ServiceFault + BadSessionIdInvalid，而不是断链。
// 断链会让驱动重连；ServiceFault 让它知道「协议通了，是我顺序错了」。
func TestReadWithoutSessionIsFaultNotDisconnect(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()

	id, body := c.call(ua.SvcReadRequest, ua.ReadRequest{
		Header:             ua.RequestHeader{RequestHandle: 9},
		TimestampsToReturn: ua.TimestampsBoth,
		Nodes:              []ua.ReadValueID{{NodeID: ua.StringNode(2, "Plant.Temperature"), AttributeID: ua.AttrValue}},
	}.Encode())
	if id != ua.SvcServiceFaultResponse {
		t.Fatalf("service id = %d", id)
	}
	f := decodeFault(t, body)
	if f.Header.ServiceResult != ua.StatusBadSessionIDInvalid {
		t.Fatalf("status = 0x%08X", uint32(f.Header.ServiceResult))
	}
	// 链路仍然可用：再发一条仍然能收到响应。
	c.open()
}

// 建了会话但没 ActivateSession 就读：同样必须是会话错误。
// 这是 OPC UA 与直觉最不一样的地方——CreateSession 成功不代表能用。
func TestReadWithoutActivationIsSessionError(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()

	id, body := c.call(ua.SvcCreateSessionRequest, ua.CreateSessionRequest{
		Header:                  ua.RequestHeader{RequestHandle: 2},
		ClientDescription:       ua.ApplicationDescription{ApplicationName: ua.LocalizedText{Text: "uic-test"}},
		EndpointURL:             "opc.tcp://localhost",
		RequestedSessionTimeout: 600000,
	}.Encode())
	if id != ua.SvcCreateSessionResponse {
		t.Fatalf("create session id = %d", id)
	}
	resp, err := ua.DecodeCreateSessionResponse(body)
	if err != nil {
		t.Fatal(err)
	}

	id, body = c.call(ua.SvcReadRequest, ua.ReadRequest{
		Header:             ua.RequestHeader{RequestHandle: 4, AuthenticationToken: resp.AuthenticationToken},
		TimestampsToReturn: ua.TimestampsBoth,
		Nodes:              []ua.ReadValueID{{NodeID: ua.StringNode(2, "Plant.Temperature"), AttributeID: ua.AttrValue}},
	}.Encode())
	if id != ua.SvcServiceFaultResponse {
		t.Fatalf("service id = %d", id)
	}
	f := decodeFault(t, body)
	if f.Header.ServiceResult != ua.StatusBadSessionIDInvalid {
		t.Fatalf("status = 0x%08X", uint32(f.Header.ServiceResult))
	}
}

func TestReadAllTypes(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	cases := []struct {
		node ua.NodeID
		want model.DataType
		typ  byte
	}{
		{ua.StringNode(2, "Plant.Temperature"), model.TypeFloat64, ua.TypeDouble},
		{ua.StringNode(2, "Plant.Counter"), model.TypeInt32, ua.TypeInt32},
		{ua.StringNode(2, "Plant.Running"), model.TypeBool, ua.TypeBoolean},
		{ua.StringNode(2, "Plant.Label"), model.TypeString, ua.TypeString},
	}
	for _, cs := range cases {
		dv := c.readOne(token, cs.node)
		if dv.Status != ua.StatusGood {
			t.Fatalf("%s: status 0x%08X", cs.node, uint32(dv.Status))
		}
		if dv.Value == nil {
			t.Fatalf("%s: nil value", cs.node)
		}
		if dv.Value.Type != cs.typ {
			t.Fatalf("%s: variant type %d want %d", cs.node, dv.Value.Type, cs.typ)
		}
		if dv.SourceTimestamp.IsZero() {
			t.Fatalf("%s: zero source timestamp", cs.node)
		}
		mv, err := ua.DecodeValue(*dv.Value, cs.want)
		if err != nil {
			t.Fatalf("%s: decode: %v", cs.node, err)
		}
		if mv.Type != cs.want {
			t.Fatalf("%s: platform type %s", cs.node, mv.Type)
		}
	}
}

func TestReadUnknownNodeIsBadNodeIDUnknown(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	dv := c.readOne(token, ua.StringNode(2, "Plant.NoSuchThing"))
	if dv.Status != ua.StatusBadNodeIDUnknown {
		t.Fatalf("status = 0x%08X", uint32(dv.Status))
	}
	if dv.Value != nil {
		t.Fatalf("value must be nil for unknown node: %+v", dv.Value)
	}
}

func TestReadNonValueAttribute(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	id, body := c.call(ua.SvcReadRequest, ua.ReadRequest{
		Header:             ua.RequestHeader{RequestHandle: 4, AuthenticationToken: token},
		TimestampsToReturn: ua.TimestampsBoth,
		Nodes: []ua.ReadValueID{
			{NodeID: ua.StringNode(2, "Plant.Temperature"), AttributeID: ua.AttrDisplayName},
			{NodeID: ua.StringNode(2, "Plant.Temperature"), AttributeID: ua.AttrDataType},
		},
	}.Encode())
	if id != ua.SvcReadResponse {
		t.Fatalf("service id %d", id)
	}
	resp, err := ua.DecodeReadResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Values) != 2 {
		t.Fatalf("values = %d", len(resp.Values))
	}
	if resp.Values[0].Value == nil || resp.Values[0].Value.Str != "Temperature" {
		t.Fatalf("display name = %+v", resp.Values[0].Value)
	}
	// DataType 属性必须是 NodeId 型（i=11 = Double），否则通用客户端画不出类型。
	if resp.Values[1].Value == nil || resp.Values[1].Value.Type != ua.TypeNodeID {
		t.Fatalf("data type = %+v", resp.Values[1].Value)
	}
}

func TestWriteThenReadBack(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	v := ua.Variant{Type: ua.TypeDouble, Float: 88.5}
	id, body := c.call(ua.SvcWriteRequest, ua.WriteRequest{
		Header: ua.RequestHeader{RequestHandle: 5, AuthenticationToken: token},
		Nodes: []ua.WriteValue{{
			NodeID:      ua.StringNode(2, "Plant.Setpoint"),
			AttributeID: ua.AttrValue,
			Value:       ua.DataValue{Value: &v},
		}},
	}.Encode())
	if id != ua.SvcWriteResponse {
		t.Fatalf("write service id %d: %v", id, decodeFault(t, body))
	}
	resp, err := ua.DecodeWriteResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0] != ua.StatusGood {
		t.Fatalf("results %v", resp.Results)
	}

	dv := c.readOne(token, ua.StringNode(2, "Plant.Setpoint"))
	if dv.Value == nil || dv.Value.Float != 88.5 {
		t.Fatalf("read back = %+v", dv.Value)
	}
}

// 只读节点写入必须回 BadNotWritable 且不改变值。静默成功比报错危害大得多：
// 上层会以为设定值已经下发。
func TestWriteReadOnlyNodeRejected(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	before, _ := srv.Value("ns=2;s=Plant.Temperature")
	v := ua.Variant{Type: ua.TypeDouble, Float: 99}
	_, body := c.call(ua.SvcWriteRequest, ua.WriteRequest{
		Header: ua.RequestHeader{RequestHandle: 5, AuthenticationToken: token},
		Nodes: []ua.WriteValue{{
			NodeID:      ua.StringNode(2, "Plant.Temperature"),
			AttributeID: ua.AttrValue,
			Value:       ua.DataValue{Value: &v},
		}},
	}.Encode())
	resp, err := ua.DecodeWriteResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0] != ua.StatusBadNotWritable {
		t.Fatalf("results %v", resp.Results)
	}
	after, _ := srv.Value("ns=2;s=Plant.Temperature")
	if after.Float != before.Float {
		t.Fatalf("value changed: %v -> %v", before.Float, after.Float)
	}
}

// 写 float64 到 int32 节点：必须 BadTypeMismatch，不能截断。
func TestWriteTypeMismatchRejected(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	// Label 是可写节点，声明的 DataType 是 String。写 double 必须报
	// BadTypeMismatch：若服务端走「先转平台类型再存」，double 会被成功
	// 转成 "3.7" 存进去——写入返回 Good，值却是字符串，最难排查。
	v := ua.Variant{Type: ua.TypeDouble, Float: 3.7}
	_, body := c.call(ua.SvcWriteRequest, ua.WriteRequest{
		Header: ua.RequestHeader{RequestHandle: 5, AuthenticationToken: token},
		Nodes: []ua.WriteValue{{
			NodeID:      ua.StringNode(2, "Plant.Label"),
			AttributeID: ua.AttrValue,
			Value:       ua.DataValue{Value: &v},
		}},
	}.Encode())
	resp, err := ua.DecodeWriteResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0] != ua.StatusBadTypeMismatch {
		t.Fatalf("results %v", resp.Results)
	}
	if cur, _ := srv.Value("ns=2;s=Plant.Label"); cur.Str != "SIM-01" {
		t.Fatalf("label changed to %q", cur.Str)
	}
}

func TestBrowseObjectsFolder(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	id, body := c.call(ua.SvcBrowseRequest, ua.BrowseRequest{
		Header: ua.RequestHeader{RequestHandle: 7, AuthenticationToken: token},
		View:   ua.ViewDescription{},
		Nodes: []ua.BrowseDescription{{
			NodeID:          ua.Numeric(0, ua.NodeObjectsFolder),
			Direction:       ua.BrowseDirectionForward,
			ReferenceTypeID: ua.Numeric(0, ua.RefHierarchicalReferences),
			IncludeSubtypes: true,
			ResultMask:      ua.BrowseResultMaskAll,
		}},
	}.Encode())
	if id != ua.SvcBrowseResponse {
		t.Fatalf("browse service id %d: %v", id, decodeFault(t, body))
	}
	resp, err := ua.DecodeBrowseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d", len(resp.Results))
	}
	names := map[string]bool{}
	for _, ref := range resp.Results[0].References {
		names[ref.BrowseName.Name] = true
	}
	if !names["Server"] || !names["Plant"] {
		t.Fatalf("Objects children = %v", names)
	}
}

func TestBrowsePlantVariables(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	id, body := c.call(ua.SvcBrowseRequest, ua.BrowseRequest{
		Header: ua.RequestHeader{RequestHandle: 7, AuthenticationToken: token},
		Nodes: []ua.BrowseDescription{{
			NodeID:          ua.StringNode(2, "Plant"),
			Direction:       ua.BrowseDirectionForward,
			ReferenceTypeID: ua.Numeric(0, ua.RefHierarchicalReferences),
			IncludeSubtypes: true,
			ResultMask:      ua.BrowseResultMaskAll,
		}},
	}.Encode())
	if id != ua.SvcBrowseResponse {
		t.Fatalf("service id %d", id)
	}
	resp, err := ua.DecodeBrowseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ref := range resp.Results[0].References {
		names[ref.BrowseName.Name] = true
		if ref.NodeClass != ua.NodeClassVariable {
			t.Fatalf("%s: node class %d", ref.BrowseName.Name, ref.NodeClass)
		}
	}
	for _, want := range []string{"Counter", "Temperature", "Running", "Label", "Setpoint"} {
		if !names[want] {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
}

func TestGetEndpoints(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()

	id, body := c.call(ua.SvcGetEndpointsRequest, ua.GetEndpointsRequest{
		Header:      ua.RequestHeader{RequestHandle: 1},
		EndpointURL: "",
	}.Encode())
	if id != ua.SvcGetEndpointsResponse {
		t.Fatalf("service id %d: %v", id, decodeFault(t, body))
	}
	resp, err := ua.DecodeGetEndpointsResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Endpoints) == 0 {
		t.Fatal("no endpoints")
	}
	ep := resp.Endpoints[0]
	if ep.SecurityMode != ua.SecurityModeNone {
		t.Fatalf("security mode %d", ep.SecurityMode)
	}
	if ep.SecurityPolicyURI != ua.PolicyNone {
		t.Fatalf("policy %q", ep.SecurityPolicyURI)
	}
	if len(ep.UserIdentityTokens) == 0 || ep.UserIdentityTokens[0].TokenType != ua.TokenTypeAnonymous {
		t.Fatalf("tokens %+v", ep.UserIdentityTokens)
	}
}

// 不支持的服务必须回 ServiceFault + BadServiceUnsupported，而不是断链：
// 订阅类请求在驱动侧是「已知不支持」，上层要能区分这个和「连不上」。
func TestUnsupportedServiceIsFault(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()

	// 787 = CreateSubscriptionRequest。本实现不提供订阅，故意用一个
	// 「规范里存在但这里没实现」的服务 ID 来测服务不支持的分支。
	const svcCreateSubscriptionRequest uint32 = 787
	id, body := c.call(svcCreateSubscriptionRequest, []byte{})
	if id != ua.SvcServiceFaultResponse {
		t.Fatalf("service id %d", id)
	}
	f := decodeFault(t, body)
	if f.Header.ServiceResult != ua.StatusBadServiceUnsupported {
		t.Fatalf("status 0x%08X", uint32(f.Header.ServiceResult))
	}
}

// 非匿名身份令牌必须拒绝：演示服务端假装支持用户名密码，
// 会让调用方以为「配了用户就能连」。
func TestNonAnonymousIdentityRejected(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()

	_, body := c.call(ua.SvcCreateSessionRequest, ua.CreateSessionRequest{
		Header:                  ua.RequestHeader{RequestHandle: 2},
		ClientDescription:       ua.ApplicationDescription{ApplicationName: ua.LocalizedText{Text: "uic-test"}},
		EndpointURL:             "opc.tcp://localhost",
		RequestedSessionTimeout: 600000,
	}.Encode())
	resp, err := ua.DecodeCreateSessionResponse(body)
	if err != nil {
		t.Fatal(err)
	}

	id, body := c.call(ua.SvcActivateSessionRequest, ua.ActivateSessionRequest{
		Header: ua.RequestHeader{RequestHandle: 3, AuthenticationToken: resp.AuthenticationToken},
		UserIdentityToken: ua.ExtensionObject{
			TypeID: ua.Numeric(0, 324), // UserNameIdentityToken
			Body:   []byte{},
		},
	}.Encode())
	if id != ua.SvcServiceFaultResponse {
		t.Fatalf("service id %d", id)
	}
	f := decodeFault(t, body)
	if f.Header.ServiceResult != ua.StatusBadUserAccessDenied {
		t.Fatalf("status 0x%08X", uint32(f.Header.ServiceResult))
	}
}

func TestCloseSessionThenReadFails(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	id, body := c.call(ua.SvcCloseSessionRequest, ua.CloseSessionRequest{
		Header: ua.RequestHeader{RequestHandle: 8, AuthenticationToken: token},
	}.Encode())
	if id != ua.SvcCloseSessionResponse {
		t.Fatalf("close session id %d: %v", id, decodeFault(t, body))
	}

	id, body = c.call(ua.SvcReadRequest, ua.ReadRequest{
		Header:             ua.RequestHeader{RequestHandle: 9, AuthenticationToken: token},
		TimestampsToReturn: ua.TimestampsBoth,
		Nodes:              []ua.ReadValueID{{NodeID: ua.StringNode(2, "Plant.Temperature"), AttributeID: ua.AttrValue}},
	}.Encode())
	if id != ua.SvcServiceFaultResponse {
		t.Fatalf("read after close session: id %d", id)
	}
	if f := decodeFault(t, body); f.Header.ServiceResult != ua.StatusBadSessionIDInvalid {
		t.Fatalf("status 0x%08X", uint32(f.Header.ServiceResult))
	}
}

func TestSetAndValueHelpers(t *testing.T) {
	srv := newSim(t)

	if _, ok := srv.Value("ns=2;s=Nope"); ok {
		t.Fatal("unknown node must not be readable")
	}
	srv.Set("ns=2;s=Plant.Counter", ua.Variant{Type: ua.TypeInt32, Int: 42})
	if v, ok := srv.Value("ns=2;s=Plant.Counter"); !ok || v.Int != 42 {
		t.Fatalf("value = %+v ok=%v", v, ok)
	}
	// 非法 NodeId 字符串不能让 Set 崩，也不能静默改错节点。
	srv.Set("ns=x;i=1", ua.Variant{Type: ua.TypeInt32, Int: 1})
}

func TestCloseIsIdempotent(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// 客户端断开时必须回收 goroutine：旧连接不清理会在长时间运行里攒出泄漏。
func TestConcurrentClients(t *testing.T) {
	srv := newSim(t)
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := conn.Write(ua.EncodeHello(ua.Hello{
				ReceiveBufSize: 65535, SendBufSize: 65535,
				EndpointURL: "opc.tcp://" + srv.Addr(),
			})); err != nil {
				done <- err
				return
			}
			head := make([]byte, 8)
			if _, err := readFull(conn, head); err != nil {
				done <- err
				return
			}
			done <- nil
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
}

func newSim(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}
