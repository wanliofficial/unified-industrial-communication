// Package opcuaserver 实现一个可复用的 OPC UA 二进制服务端内核：它把一张
// 内存地址空间暴露成标准 OPC UA 服务端，供两种完全不同的角色使用——
//
//   - 演示/自测：`NewSimulator` 起一个带演示点位的小服务端，让主站在没有
//     真实设备时也能跑通三层握手与推送链路；
//   - 北向集成：网关把采集到的点位映射成地址空间，SCADA 直接读写。
//
// 两者共用同一份协议实现是刻意的。服务端编解码如果和客户端分开写，编码
// 错误会被解码错误抵消，自测永远全绿；共用 codec/opcua 才能保证测的是
// 协议而不是笔误。
//
// 边界：HEL/ACK、OPN、CreateSession/ActivateSession/CloseSession、Read、
// Write、Browse、GetEndpoints，以及订阅（CreateSubscription /
// CreateMonitoredItems / Publish / DeleteSubscriptions / SetPublishingMode）。
// 不含历史读取、方法调用、事件、别名、证书、分块。
package opcuaserver

import (
	"fmt"
	"net"
	"sync"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/model"
)

// Node 是地址空间里的一个节点。
//
// 只有两种节点：对象（目录，靠 Parent 组成树）与变量（有值、可读、可能
// 可写）。这个子集覆盖了「把点位暴露给 SCADA」需要的全部语义；引入
// 方法、事件与类型系统会让服务端变成另一套产品。
type Node struct {
	ID     ua.NodeID
	Parent ua.NodeID

	// BrowseName 与 Namespace 决定客户端树上显示的名字。Namespace 与
	// ID 的命名空间通常一致，但不强制：标准节点可以挂在自定义命名空间
	// 的目录下。
	BrowseName  string
	Namespace   uint16
	DisplayName string

	// Class 是 ua.NodeClassObject / ua.NodeClassVariable。留空时按
	// 「有 DataType 或 Value 就是变量」推断。
	Class uint32

	// DataType 决定变量对外声明的类型：它同时用于 DataType 属性读取、
	// 写入类型校验与 Variant 编码。对象节点忽略。
	DataType model.DataType
	// Value 是当前值（变量节点）。初始值也放在这里。
	Value ua.Variant
	// Status 是随值一起返回的质量码。0 表示 Good；离线点位应当是
	// Bad* 而不是「最后一个好值」——SCADA 靠它区分「没变」与「断了」。
	Status ua.StatusCode
	// UpdatedAt 是源时间戳；零值表示「用当前时间」。
	UpdatedAt time.Time
	// Writable 决定客户端能否写这个变量。
	Writable bool
}

// Config 是服务端启动参数。
type Config struct {
	// Addr 是监听地址；"127.0.0.1:0" 表示自动分配端口。
	Addr string
	// Nodes 是完整地址空间。服务端不会自动补 Root/Objects——调用方要
	// 自己（或通过标准节点辅助函数）建树，否则浏览结果会是一堆孤岛。
	Nodes []Node

	// ApplicationName / ApplicationURI 出现在 GetEndpoints 与会话响应里。
	// 留空时给一组中性默认值，避免把演示名字带进生产。
	ApplicationName string
	ApplicationURI  string
	ProductURI      string

	// OnWrite 处理客户端的写请求。nil 表示「直接写内存值并推送」，
	// 这是演示模式的语义。
	//
	// 北向集成必须设置它：把值转给南向设备，由真实设备确认。返回
	// 非 Good 时节点值保持不变——「写入被接受」和「值已经改了」是两件
	// 事，混在一起会让 SCADA 显示一个设备从未接受过的值。
	OnWrite func(n Node, v model.Value, raw ua.Variant) ua.StatusCode

	// Log 接收结构性事件（订阅建立/拒绝、写入被拒）。nil 表示丢弃。
	Log func(msg string, args ...any)
}

// Server 是 OPC UA 服务端。
type Server struct {
	mu       sync.RWMutex
	ln       net.Listener
	cfg      Config
	nodes    map[string]*node
	order    []string
	conns    map[net.Conn]struct{}
	sessions map[uint32]bool
	nextSess uint32

	// subs 是全部订阅。Publish 可能阻塞数秒，所以订阅有自己的锁，
	// 不能靠 s.mu 串行化——否则一个空闲订阅会把 Read/Write 一起堵住。
	subs    map[uint32]*subscription
	nextSub uint32

	wg     sync.WaitGroup
	closed bool
	doneCh chan struct{}
	log    func(msg string, args ...any)
}

// node 是 Node 的内部形态：只在启动时由公开结构构造一次，运行期只改
// value/status/at 三个字段。
type node struct {
	id       ua.NodeID
	parent   ua.NodeID
	browse   ua.QualifiedName
	display  string
	class    uint32
	ptype    model.DataType
	value    ua.Variant
	status   ua.StatusCode
	at       time.Time
	writable bool
	// pub 是构造时的快照，OnWrite 拿到的就是它。写入回调不应该看到
	// 服务端内部指针，否则一次误改就能让地址空间与真实设备长期不一致。
	pub Node
}

// New 创建服务端并开始监听。监听失败直接返回错误：端口被占用必须让
// 启动失败，而不是「网关起来了但 SCADA 永远连不上」。
func New(cfg Config) (*Server, error) {
	if cfg.ApplicationName == "" {
		cfg.ApplicationName = "UIC OPC UA Server"
	}
	if cfg.ApplicationURI == "" {
		cfg.ApplicationURI = "urn:uic:server"
	}
	if cfg.ProductURI == "" {
		cfg.ProductURI = "urn:uic"
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		cfg:      cfg,
		nodes:    map[string]*node{},
		conns:    map[net.Conn]struct{}{},
		sessions: map[uint32]bool{},
		subs:     map[uint32]*subscription{},
		doneCh:   make(chan struct{}),
		log:      cfg.Log,
	}
	if s.log == nil {
		s.log = func(string, ...any) {}
	}
	for _, n := range cfg.Nodes {
		s.add(n)
	}
	// 标准 CurrentTime 由服务端自己维护：让调用方去起一个 ticking
	// goroutine，结果一定是「演示时对、生产时忘了」。
	if _, ok := s.nodes[ua.Numeric(0, 2258).String()]; ok {
		s.wg.Add(1)
		go s.clockLoop()
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 停止监听、关闭全部连接并等待 goroutine 退出。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.doneCh)
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// ---------------------------------------------------------------------------
// 地址空间
// ---------------------------------------------------------------------------

func (s *Server) add(n Node) {
	if n.Class == 0 {
		if n.DataType != "" || n.Value.Type != 0 {
			n.Class = ua.NodeClassVariable
		} else {
			n.Class = ua.NodeClassObject
		}
	}
	if n.DisplayName == "" {
		n.DisplayName = n.BrowseName
	}
	inner := &node{
		id: n.ID, parent: n.Parent,
		browse:  ua.QualifiedName{Namespace: n.Namespace, Name: n.BrowseName},
		display: n.DisplayName, class: n.Class,
		ptype: n.DataType, value: n.Value, status: n.Status, at: n.UpdatedAt,
		writable: n.Writable, pub: n,
	}
	key := inner.id.String()
	if _, ok := s.nodes[key]; ok {
		return
	}
	s.nodes[key] = inner
	s.order = append(s.order, key)
}

// Set 按 NodeId 字符串更新变量值（Good 质量）。演示计时器与测试用；
// 需要带质量码时用 Update。
func (s *Server) Set(id string, v ua.Variant) {
	nid, err := ua.ParseNodeID(id)
	if err != nil {
		return
	}
	s.Update(nid, v, ua.StatusGood, time.Now())
}

// Update 更新变量值与质量码，并推送给所有监视该节点的订阅。
//
// 质量码决定 DataValue 是否携带值：Bad 状态按规范不该带值。带着旧值
// 一起发「坏」，客户端会把旧值当当前值——这正是「设备断了但画面还在
// 动」的成因。
func (s *Server) Update(nid ua.NodeID, v ua.Variant, status ua.StatusCode, at time.Time) {
	key := nid.String()
	s.mu.Lock()
	n, ok := s.nodes[key]
	if ok {
		n.value = v
		n.status = status
		n.at = at
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	// 先解锁再分发：notifyChange 要拿订阅锁，而 Publish 路径持有订阅锁
	// 时会读节点表（判存在/类型），两把锁反序就是互等。
	s.notifyChange(nid, v, status, at)
}

// Value 读取一个变量的当前值与质量码。
func (s *Server) Value(id string) (ua.Variant, ua.StatusCode, bool) {
	nid, err := ua.ParseNodeID(id)
	if err != nil {
		return ua.Variant{}, ua.StatusGood, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[nid.String()]
	if !ok {
		return ua.Variant{}, ua.StatusGood, false
	}
	return n.value, n.status, true
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = conn.Close()
		}()
	}
}

// session 是一条连接上的协议状态。三层状态放在一起，但各自独立判断：
// 安全通道有效不代表会话有效，这是 OPC UA 与单层协议最大的不同。
type session struct {
	conn      net.Conn
	channelID uint32
	tokenID   uint32
	seq       uint32
	sessionID uint32
	activated bool

	// writeMu 让「响应写回」串行化：Publish 在独立 goroutine 里等通知，
	// 它与 Read/Write 的响应会并发写同一条连接。OPC UA 允许乱序响应
	// （按 RequestId 匹配），但不允许两个 goroutine 同时往一个 socket
	// 写半条报文。
	writeMu sync.Mutex
	// done 在本连接结束时关闭，用来唤醒阻塞中的 Publish。
	done chan struct{}
}

func (sess *session) respond(requestID, serviceID uint32, body []byte) bool {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	sess.seq++
	_, err := sess.conn.Write(ua.EncodeSecureMessage(ua.MsgMessage,
		sess.channelID, sess.tokenID,
		ua.SequenceHeader{Number: sess.seq, RequestID: requestID},
		serviceID, body))
	return err == nil
}

func (s *Server) handleConn(conn net.Conn) {
	sess := &session{conn: conn, done: make(chan struct{})}
	defer func() {
		// 先唤醒 Publish 再清订阅：反过来的话，阻塞中的 Publish 会先把
		// 「订阅不存在」的错误写回一条已经关掉的连接。
		close(sess.done)
		if sess.sessionID != 0 {
			s.dropSessionSubscriptions(sess.sessionID)
			s.mu.Lock()
			delete(s.sessions, sess.sessionID)
			s.mu.Unlock()
		}
	}()
	buf := make([]byte, 8)
	for {
		if _, err := readFull(conn, buf); err != nil {
			return
		}
		h, err := ua.DecodeConnHeader(buf)
		if err != nil {
			s.sendError(conn, ua.StatusBadCommunicationError, err.Error())
			return
		}
		if int(h.Size) < 8 || h.Size > 8<<20 {
			s.sendError(conn, ua.StatusBadCommunicationError, "invalid message size")
			return
		}
		body := make([]byte, int(h.Size)-8)
		if _, err := readFull(conn, body); err != nil {
			return
		}

		switch h.Type {
		case ua.MsgHello:
			hello, derr := ua.DecodeHello(body)
			if derr != nil {
				s.sendError(conn, ua.StatusBadCommunicationError, derr.Error())
				return
			}
			_ = hello
			if _, err := conn.Write(ua.EncodeAcknowledge(ua.Acknowledge{
				Version: 0, ReceiveBufSize: 65535, SendBufSize: 65535,
				MaxMessageSize: 0, MaxChunkCount: 0,
			})); err != nil {
				return
			}
		case ua.MsgOpen, ua.MsgMessage, ua.MsgClose:
			raw := append(buf, body...)
			m, derr := ua.DecodeSecureMessage(raw)
			if derr != nil {
				s.sendError(conn, ua.StatusBadCommunicationError, derr.Error())
				return
			}
			if !s.dispatch(sess, m) {
				return
			}
		case ua.MsgError:
			return
		default:
			s.sendError(conn, ua.StatusBadCommunicationError,
				fmt.Sprintf("unexpected message type %q", h.Type))
			return
		}
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

func (s *Server) sendError(c net.Conn, code ua.StatusCode, reason string) {
	_, _ = c.Write(ua.EncodeErrorMessage(ua.ErrorMessage{Code: code, Reason: reason}))
}

// dispatch 处理一条安全报文；返回 false 表示连接应当关闭。
func (s *Server) dispatch(sess *session, m ua.SecureMessage) bool {
	switch m.Type {
	case ua.MsgOpen:
		resp := s.handleOpenSecureChannel(sess, m.Body)
		sess.writeMu.Lock()
		defer sess.writeMu.Unlock()
		sess.seq++
		_, err := sess.conn.Write(ua.EncodeOpenMessage(sess.channelID,
			ua.AsymmetricSecurityHeader{PolicyURI: ua.PolicyNone},
			ua.SequenceHeader{Number: sess.seq, RequestID: m.Seq.RequestID},
			ua.SvcOpenSecureChannelResponse, resp))
		return err == nil
	case ua.MsgClose:
		return false
	case ua.MsgMessage:
		// Publish 会一直等到有通知或保活周期到。同步处理它会让这条连接
		// 上的 DeleteSubscriptions 永远排不到，关闭时只能靠超时——所以
		// 它单独开一个 goroutine，响应按 RequestId 乱序返回是规范允许的。
		if m.ServiceID == ua.SvcPublishRequest {
			body := m.Body
			reqID := m.Seq.RequestID
			go func() {
				serviceID, out := s.handleMessage(sess, ua.SvcPublishRequest, body)
				sess.respond(reqID, serviceID, out)
			}()
			return true
		}
		serviceID, body := s.handleMessage(sess, m.ServiceID, m.Body)
		return sess.respond(m.Seq.RequestID, serviceID, body)
	}
	return false
}

func (s *Server) handleOpenSecureChannel(sess *session, body []byte) []byte {
	req, err := ua.DecodeOpenSecureChannelRequest(body)
	if err != nil {
		return s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	sess.channelID = 1
	sess.tokenID++
	resp := ua.OpenSecureChannelResponse{
		Header: ua.ResponseHeader{
			Timestamp:     time.Now(),
			RequestHandle: req.Header.RequestHandle,
			ServiceResult: ua.StatusGood,
		},
		ServerProtocolVersion: 0,
		Token: ua.ChannelSecurityToken{
			ChannelID:       sess.channelID,
			TokenID:         sess.tokenID,
			CreatedAt:       time.Now(),
			RevisedLifetime: req.RequestedLifetime,
		},
		ServerNonce: nil,
	}
	if resp.Token.RevisedLifetime == 0 {
		resp.Token.RevisedLifetime = 3600000
	}
	return resp.Encode()
}

// handleMessage 分发服务请求，返回响应的服务 ID 与正文。任何失败都
// 变成 ServiceFault——这是 OPC UA 的约定：服务级错误仍是一条正常响应，
// 而不是断开连接。
func (s *Server) handleMessage(sess *session, serviceID uint32, body []byte) (uint32, []byte) {
	switch serviceID {
	case ua.SvcGetEndpointsRequest:
		return s.handleGetEndpoints(sess, body)
	case ua.SvcCreateSessionRequest:
		return s.handleCreateSession(sess, body)
	case ua.SvcActivateSessionRequest:
		return s.handleActivateSession(sess, body)
	case ua.SvcCloseSessionRequest:
		req, err := ua.DecodeCloseSessionRequest(body)
		if err != nil {
			return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
		}
		sess.activated = false
		if sess.sessionID != 0 {
			s.dropSessionSubscriptions(sess.sessionID)
			s.mu.Lock()
			delete(s.sessions, sess.sessionID)
			s.mu.Unlock()
		}
		sess.sessionID = 0
		resp := ua.CloseSessionResponse{Header: ua.ResponseHeader{
			Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle,
		}}
		return ua.SvcCloseSessionResponse, resp.Encode()
	case ua.SvcReadRequest:
		return s.handleRead(sess, body)
	case ua.SvcWriteRequest:
		return s.handleWrite(sess, body)
	case ua.SvcBrowseRequest:
		return s.handleBrowse(sess, body)
	case ua.SvcCreateSubscriptionRequest:
		return s.handleCreateSubscription(sess, body)
	case ua.SvcCreateMonitoredItemsRequest:
		return s.handleCreateMonitoredItems(sess, body)
	case ua.SvcDeleteMonitoredItemsRequest:
		return s.handleDeleteMonitoredItems(sess, body)
	case ua.SvcDeleteSubscriptionsRequest:
		return s.handleDeleteSubscriptions(sess, body)
	case ua.SvcSetPublishingModeRequest:
		return s.handleSetPublishingMode(sess, body)
	case ua.SvcPublishRequest:
		return s.handlePublish(sess, body)
	}
	return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadServiceUnsupported,
		fmt.Sprintf("service %d is not implemented", serviceID))
}

func (s *Server) fault(h *ua.RequestHeader, handle uint32, code ua.StatusCode, msg string) []byte {
	f := ua.ServiceFault{Header: ua.ResponseHeader{
		Timestamp:     time.Now(),
		RequestHandle: handle,
		ServiceResult: code,
		StringTable:   []string{msg},
	}}
	if h != nil {
		f.Header.RequestHandle = h.RequestHandle
	}
	return f.Encode()
}

func (s *Server) checkSession(sess *session, token ua.NodeID, handle uint32) (uint32, []byte) {
	if sess.sessionID == 0 || token.IsNull() || token.ID != sess.sessionID {
		return ua.SvcServiceFaultResponse, s.fault(nil, handle, ua.StatusBadSessionIDInvalid, "session not found")
	}
	if !sess.activated {
		return ua.SvcServiceFaultResponse, s.fault(nil, handle, ua.StatusBadSessionIDInvalid, "session not activated")
	}
	return 0, nil
}

func (s *Server) endpoint() ua.EndpointDescription {
	return ua.EndpointDescription{
		EndpointURL: "opc.tcp://" + s.Addr(),
		Server: ua.ApplicationDescription{
			ApplicationURI:  s.cfg.ApplicationURI,
			ProductURI:      s.cfg.ProductURI,
			ApplicationName: ua.LocalizedText{Text: s.cfg.ApplicationName},
			ApplicationType: ua.AppTypeServer,
		},
		SecurityMode:      ua.SecurityModeNone,
		SecurityPolicyURI: ua.PolicyNone,
		UserIdentityTokens: []ua.UserTokenPolicy{{
			PolicyID:          "anonymous",
			TokenType:         ua.TokenTypeAnonymous,
			SecurityPolicyURI: ua.PolicyNone,
		}},
		TransportProfileURI: ua.TransportBinary,
		SecurityLevel:       0,
	}
}

func (s *Server) handleGetEndpoints(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeGetEndpointsRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	resp := ua.GetEndpointsResponse{
		Header: ua.ResponseHeader{
			Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle,
		},
		Endpoints: []ua.EndpointDescription{s.endpoint()},
	}
	return ua.SvcGetEndpointsResponse, resp.Encode()
}

func (s *Server) handleCreateSession(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeCreateSessionRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	s.mu.Lock()
	s.nextSess++
	id := s.nextSess
	s.sessions[id] = true
	s.mu.Unlock()

	sess.sessionID = id
	resp := ua.CreateSessionResponse{
		Header: ua.ResponseHeader{
			Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle,
		},
		SessionID:             ua.Numeric(1, id),
		AuthenticationToken:   ua.Numeric(1, id),
		RevisedSessionTimeout: 600000,
		ServerNonce:           nil,
		ServerCertificate:     nil,
		ServerEndpoints:       []ua.EndpointDescription{s.endpoint()},
		MaxRequestMessageSize: 0,
	}
	return ua.SvcCreateSessionResponse, resp.Encode()
}

func (s *Server) handleActivateSession(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeActivateSessionRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if sess.sessionID == 0 || req.Header.AuthenticationToken.ID != sess.sessionID {
		return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
			ua.StatusBadSessionIDInvalid, "session not found")
	}
	// 只接受匿名令牌：服务端不该假装支持用户名密码，否则调用方会以为
	// 「配置了用户就能用」。证书/用户体系是另一套运维域。
	if req.UserIdentityToken.TypeID.ID != 321 {
		return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
			ua.StatusBadUserAccessDenied, "only anonymous identity is supported")
	}
	sess.activated = true
	resp := ua.ActivateSessionResponse{Header: ua.ResponseHeader{
		Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle,
	}}
	return ua.SvcActivateSessionResponse, resp.Encode()
}

func (s *Server) handleRead(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeReadRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}

	now := time.Now()
	values := make([]ua.DataValue, len(req.Nodes))

	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, rv := range req.Nodes {
		n, ok := s.nodes[rv.NodeID.String()]
		if !ok {
			values[i] = ua.DataValue{Status: ua.StatusBadNodeIDUnknown, SourceTimestamp: now, ServerTimestamp: now}
			continue
		}
		switch rv.AttributeID {
		case ua.AttrValue:
			if n.class != ua.NodeClassVariable {
				values[i] = ua.DataValue{Status: ua.StatusBadAttributeInvalid, SourceTimestamp: now, ServerTimestamp: now}
				continue
			}
			values[i] = dataValueOf(n, now)
		case ua.AttrDisplayName:
			v := ua.Variant{Type: ua.TypeLocalized, Str: n.display}
			values[i] = ua.DataValue{Value: &v, SourceTimestamp: now, ServerTimestamp: now}
		case ua.AttrBrowseName:
			v := ua.Variant{Type: ua.TypeQualified, Uint: uint64(n.browse.Namespace) << 16, Str: n.browse.Name}
			values[i] = ua.DataValue{Value: &v, SourceTimestamp: now, ServerTimestamp: now}
		case ua.AttrNodeClass:
			v := ua.Variant{Type: ua.TypeInt32, Int: int64(n.class)}
			values[i] = ua.DataValue{Value: &v, SourceTimestamp: now, ServerTimestamp: now}
		case ua.AttrDataType:
			v := ua.Variant{Type: ua.TypeNodeID, NodeID: dataTypeNode(n.ptype)}
			values[i] = ua.DataValue{Value: &v, SourceTimestamp: now, ServerTimestamp: now}
		default:
			values[i] = ua.DataValue{Status: ua.StatusBadAttributeInvalid, SourceTimestamp: now, ServerTimestamp: now}
		}
	}
	resp := ua.ReadResponse{Header: ua.ResponseHeader{
		Timestamp: now, RequestHandle: req.Header.RequestHandle,
	}, Values: values}
	return ua.SvcReadResponse, resp.Encode()
}

// dataValueOf 把节点当前状态包成 DataValue。值只在 Good/Uncertain 时附加：
// 按规范 Bad 不该带值，带上旧值会让客户端把「断了」显示成「还在跑」。
func dataValueOf(n *node, now time.Time) ua.DataValue {
	at := n.at
	if at.IsZero() {
		at = now
	}
	dv := ua.DataValue{Status: n.status, SourceTimestamp: at, ServerTimestamp: now}
	if !n.status.IsBad() {
		v := n.value
		dv.Value = &v
	}
	return dv
}

func (s *Server) handleWrite(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeWriteRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}

	type planned struct {
		index   int
		pub     Node
		model   model.Value
		variant ua.Variant
	}

	results := make([]ua.StatusCode, len(req.Nodes))
	var plans []planned

	// 第一段只做校验与解码，不持锁调用外部写路径：北向模式下 OnWrite
	// 会走到设备，耗时以秒计；持 s.mu 期间所有 Read/Browse 都会被堵住。
	s.mu.RLock()
	for i, wv := range req.Nodes {
		n, ok := s.nodes[wv.NodeID.String()]
		switch {
		case !ok:
			results[i] = ua.StatusBadNodeIDUnknown
		case wv.AttributeID != ua.AttrValue:
			results[i] = ua.StatusBadAttributeInvalid
		case n.class != ua.NodeClassVariable:
			results[i] = ua.StatusBadAttributeInvalid
		case !n.writable:
			results[i] = ua.StatusBadNotWritable
		case wv.Value.Value == nil:
			results[i] = ua.StatusBadTypeMismatch
		default:
			// 先按 OPC UA 的规则比对 Variant 类型与节点声明的 DataType：
			// 真实服务端只接受完全一致（或子类型）的 Variant，不做隐式
			// 转换。如果只用 DecodeValue 往平台类型上转，double 会被
			// 「成功地」转成 string 存进 Label 节点——写入报 Good，
			// 值却是一串 "1"，这是最难排查的一类静默错误。
			want, verr := ua.VariantType(n.ptype)
			if verr != nil || wv.Value.Value.Type != want {
				results[i] = ua.StatusBadTypeMismatch
				continue
			}
			mv, derr := ua.DecodeValue(*wv.Value.Value, n.ptype)
			if derr != nil {
				results[i] = ua.StatusBadTypeMismatch
				continue
			}
			nv, eerr := ua.EncodeValue(n.ptype, mv)
			if eerr != nil {
				results[i] = ua.StatusBadTypeMismatch
				continue
			}
			plans = append(plans, planned{index: i, pub: n.pub, model: mv, variant: nv})
		}
	}
	s.mu.RUnlock()

	now := time.Now()
	for _, p := range plans {
		if s.cfg.OnWrite != nil {
			if st := s.cfg.OnWrite(p.pub, p.model, p.variant); !st.IsGood() {
				results[p.index] = st
				s.log("opcua_write_rejected",
					"node", p.pub.ID.String(), "point", p.pub.BrowseName, "status", st.Text())
				continue
			}
		}
		key := p.pub.ID.String()
		s.mu.Lock()
		n, ok := s.nodes[key]
		if !ok {
			s.mu.Unlock()
			results[p.index] = ua.StatusBadNodeIDUnknown
			continue
		}
		n.value = p.variant
		n.status = ua.StatusGood
		n.at = now
		s.mu.Unlock()
		results[p.index] = ua.StatusGood
		// 写入回流：订阅客户端必须看到自己写进去的值，否则现场表现为
		// 「控制生效了但状态栏不变」。
		s.notifyChange(p.pub.ID, p.variant, ua.StatusGood, now)
	}

	resp := ua.WriteResponse{Header: ua.ResponseHeader{
		Timestamp: now, RequestHandle: req.Header.RequestHandle,
	}, Results: results}
	return ua.SvcWriteResponse, resp.Encode()
}

func (s *Server) handleBrowse(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeBrowseRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([]ua.BrowseResult, len(req.Nodes))
	for i, bc := range req.Nodes {
		if _, ok := s.nodes[bc.NodeID.String()]; !ok {
			results[i] = ua.BrowseResult{Status: ua.StatusBadNodeIDUnknown}
			continue
		}
		res := ua.BrowseResult{Status: ua.StatusGood}
		for _, key := range s.order {
			n := s.nodes[key]
			if n.parent.String() != bc.NodeID.String() {
				continue
			}
			res.References = append(res.References, ua.ReferenceDescription{
				ReferenceTypeID: ua.Numeric(0, refTypeFor(bc.NodeID, n)),
				IsForward:       true,
				Target:          ua.ExpandedNodeID{NodeID: n.id},
				BrowseName:      n.browse,
				DisplayName:     ua.LocalizedText{Text: n.display},
				NodeClass:       n.class,
				TypeDefinition:  ua.ExpandedNodeID{NodeID: typeDefinitionOf(n)},
			})
		}
		results[i] = res
	}
	resp := ua.BrowseResponse{Header: ua.ResponseHeader{
		Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle,
	}, Results: results}
	return ua.SvcBrowseResponse, resp.Encode()
}

// refTypeFor 选择引用类型：文件夹之间用 Organizes，对象到其组成部分用
// HasComponent。用错不会影响读值，但会让通用客户端把树画错。
func refTypeFor(parent ua.NodeID, child *node) uint32 {
	if parent == ua.Numeric(0, ua.NodeRootFolder) || parent == ua.Numeric(0, ua.NodeObjectsFolder) {
		return ua.RefOrganizes
	}
	if child.class == ua.NodeClassObject {
		return ua.RefOrganizes
	}
	return ua.RefHasComponent
}

func typeDefinitionOf(n *node) ua.NodeID {
	if n.class == ua.NodeClassVariable {
		return ua.Numeric(0, 63) // BaseDataVariableType
	}
	if n.parent.IsNull() {
		return ua.Numeric(0, 61) // FolderType
	}
	return ua.Numeric(0, 58) // BaseObjectType
}

// dataTypeNode 把平台类型映射到 DataType 节点的 NodeId。
func dataTypeNode(t model.DataType) ua.NodeID {
	vt, err := ua.VariantType(t)
	if err != nil {
		return ua.Numeric(0, 24) // BaseDataType
	}
	return ua.Numeric(0, uint32(vt))
}
