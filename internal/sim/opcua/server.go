// Package opcua（sim）实现一个最小 OPC UA 服务端，用于自测、演示与驱动
// 一致性测试。它与主站共用 codec/opcua，所以「Variant 怎么编码」「状态码
// 怎么解释」这两件事不会在主从之间分叉。
//
// 边界：只实现 HEL/ACK、OPN、CreateSession/ActivateSession/CloseSession、
// Read、Write、Browse、GetEndpoints 与订阅（CreateSubscription /
// CreateMonitoredItems / Publish / DeleteSubscriptions）。不含历史、方法
// 调用、事件、别名、证书。订阅只按「值变化」推送，没有采样定时器——
// 这恰好是网关关心的那部分语义。它存在的意义是让主站在没有真实服务端的
// 机器上跑通完整三层握手与推送链路，而不是模拟一套完整地址空间。
package opcua

import (
	"fmt"
	"net"
	"sync"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/model"
)

// Server 是内存地址空间的 OPC UA 服务端。
type Server struct {
	mu       sync.RWMutex
	ln       net.Listener
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
}

type node struct {
	id       ua.NodeID
	parent   ua.NodeID
	browse   ua.QualifiedName
	display  string
	class    uint32
	ptype    model.DataType
	value    ua.Variant
	writable bool
}

// NewServer 创建服务端；addr 为 "127.0.0.1:0" 时自动分配端口。
func NewServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		nodes:    map[string]*node{},
		conns:    map[net.Conn]struct{}{},
		sessions: map[uint32]bool{},
		subs:     map[uint32]*subscription{},
	}
	s.buildAddressSpace()
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

// buildAddressSpace 建一棵足够演示的最小树：
//
//	Root
//	└─ Objects
//	   ├─ Server
//	   │  └─ ServerStatus
//	   │     └─ CurrentTime
//	   └─ Plant
//	      ├─ Counter / Temperature / Running / Label
//	      └─ Setpoint（可写）
//
// 变量都挂在 ns=2 下，用字符串 NodeId：现场点位几乎都是这种形式，
// 用数值 NodeId 演示会让 examples 与真实配置差得太远。
func (s *Server) buildAddressSpace() {
	s.add(node{id: ua.Numeric(0, ua.NodeRootFolder), browse: ua.QualifiedName{Name: "Root"}, display: "Root", class: ua.NodeClassObject})
	s.add(node{id: ua.Numeric(0, ua.NodeObjectsFolder), parent: ua.Numeric(0, ua.NodeRootFolder), browse: ua.QualifiedName{Name: "Objects"}, display: "Objects", class: ua.NodeClassObject})
	s.add(node{id: ua.Numeric(0, ua.NodeTypesFolder), parent: ua.Numeric(0, ua.NodeRootFolder), browse: ua.QualifiedName{Name: "Types"}, display: "Types", class: ua.NodeClassObject})
	s.add(node{id: ua.Numeric(0, ua.NodeViewsFolder), parent: ua.Numeric(0, ua.NodeRootFolder), browse: ua.QualifiedName{Name: "Views"}, display: "Views", class: ua.NodeClassObject})

	s.add(node{id: ua.Numeric(0, ua.NodeServer), parent: ua.Numeric(0, ua.NodeObjectsFolder), browse: ua.QualifiedName{Name: "Server"}, display: "Server", class: ua.NodeClassObject})
	s.add(node{id: ua.Numeric(0, ua.NodeServerStatus), parent: ua.Numeric(0, ua.NodeServer), browse: ua.QualifiedName{Name: "ServerStatus"}, display: "ServerStatus", class: ua.NodeClassObject})
	s.add(node{
		id: ua.Numeric(0, 2258), parent: ua.Numeric(0, ua.NodeServerStatus),
		browse: ua.QualifiedName{Name: "CurrentTime"}, display: "CurrentTime",
		class: ua.NodeClassVariable, ptype: model.TypeString,
		value: ua.Variant{Type: ua.TypeDateTime, Time: time.Now()},
	})

	plant := ua.StringNode(2, "Plant")
	s.add(node{id: plant, parent: ua.Numeric(0, ua.NodeObjectsFolder), browse: ua.QualifiedName{Namespace: 2, Name: "Plant"}, display: "Plant", class: ua.NodeClassObject})

	s.addVar(plant, 2, "Plant.Counter", "Counter", model.TypeInt32,
		ua.Variant{Type: ua.TypeInt32, Int: 0}, false)
	s.addVar(plant, 2, "Plant.Temperature", "Temperature", model.TypeFloat64,
		ua.Variant{Type: ua.TypeDouble, Float: 21.5}, false)
	s.addVar(plant, 2, "Plant.Running", "Running", model.TypeBool,
		ua.Variant{Type: ua.TypeBoolean, Bool: true}, false)
	s.addVar(plant, 2, "Plant.Label", "Label", model.TypeString,
		ua.Variant{Type: ua.TypeString, Str: "SIM-01"}, true)
	s.addVar(plant, 2, "Plant.Setpoint", "Setpoint", model.TypeFloat64,
		ua.Variant{Type: ua.TypeDouble, Float: 60}, true)
}

func (s *Server) addVar(parent ua.NodeID, ns uint16, id, name string, t model.DataType, v ua.Variant, writable bool) {
	s.add(node{
		id: ua.StringNode(ns, id), parent: parent,
		browse: ua.QualifiedName{Namespace: ns, Name: name}, display: name,
		class: ua.NodeClassVariable, ptype: t, value: v, writable: writable,
	})
}

func (s *Server) add(n node) {
	key := n.id.String()
	if _, ok := s.nodes[key]; ok {
		return
	}
	s.nodes[key] = &n
	s.order = append(s.order, key)
}

// Set 更新一个变量的值（按节点 NodeId 字符串定位）。供 sim 命令的
// 计时器与测试使用。
func (s *Server) Set(id string, v ua.Variant) {
	nid, err := ua.ParseNodeID(id)
	if err != nil {
		return
	}
	s.mu.Lock()
	n, ok := s.nodes[nid.String()]
	if ok {
		n.value = v
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	// 先解锁再分发：notifyChange 要拿订阅锁，而 Publish 路径持有订阅锁
	// 时会读节点表（判存在/类型），两把锁反序就是互等。
	s.notifyChange(nid, v, ua.StatusGood, time.Now())
}

// Value 读取一个变量的当前值。
func (s *Server) Value(id string) (ua.Variant, bool) {
	nid, err := ua.ParseNodeID(id)
	if err != nil {
		return ua.Variant{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[nid.String()]
	if !ok {
		return ua.Variant{}, false
	}
	return n.value, true
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
			ApplicationURI:  "urn:uic:sim",
			ProductURI:      "urn:uic",
			ApplicationName: ua.LocalizedText{Text: "UIC Simulator"},
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
	// 只接受匿名令牌：演示服务端不该假装支持用户名密码，
	// 否则调用方会以为「配置了用户就能用」。
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
			v := n.value
			values[i] = ua.DataValue{Value: &v, SourceTimestamp: now, ServerTimestamp: now}
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

func (s *Server) handleWrite(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeWriteRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}

	results := make([]ua.StatusCode, len(req.Nodes))
	type change struct {
		node ua.NodeID
		val  ua.Variant
	}
	var changes []change
	s.mu.Lock()
	for i, wv := range req.Nodes {
		n, ok := s.nodes[wv.NodeID.String()]
		switch {
		case !ok:
			results[i] = ua.StatusBadNodeIDUnknown
		case wv.AttributeID != ua.AttrValue:
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
			n.value = nv
			results[i] = ua.StatusGood
			changes = append(changes, change{node: wv.NodeID, val: nv})
		}
	}
	s.mu.Unlock()

	// 写入也要触发订阅。少这一步，订阅客户端会看到「自己写进去的值
	// 迟迟不回流」——现场表现为「控制生效了但状态栏不变」。
	now := time.Now()
	for _, ch := range changes {
		s.notifyChange(ch.node, ch.val, ua.StatusGood, now)
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
