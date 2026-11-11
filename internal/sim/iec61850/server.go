// Package iec61850（sim）实现一个最小 IEC 61850（MMS over TCP）内置从站，用于自测、
// 演示与驱动一致性测试。它复用 codec/iec61850 的编解码函数，避免「主站与从站各写
// 一套」导致语义漂移——尤其是 TPKT/COTP/会话/表示/ACSE/MMS 这一长串封套的长度与
// 上下文编号，最容易在主站、从站两侧各写一遍然后对不上。
//
// 边界：实现关联（COTP CR/CC、会话 CN/AC、表示 CP/CPA、ACSE AARQ/AARE、MMS
// Initiate）、读变量、写变量、模型浏览（getNameList）、有序释放。不含 GOOSE/SV、
// 报告控制块、文件传输、定值组、数据集读写、TLS。它存在的目的不是模拟一台真 IED
// 的全部能力，而是让主站在没有真实 IED 的机器上跑通完整链路，并能按需制造几类
// 真机会发生的故障（关联被拒、对象不存在、读超时）。
//
// 刻意做的两个行为（对应真机会发生的事）：
//
//  1. **未知对象回 object-non-existent，不静默成功。** MMS 对不存在的命名变量有
//     明确的错误码；从站若礼貌地回一个默认值，主站「点位名拼错」这条最常走的排错
//     路径就永远测不到。
//
//  2. **关联可被拒。** 通过 RejectAssociation 让从站回一个 ACSE 拒绝，验证主站
//     把「关联被拒」与「读超时」分成两类事故——前者重试无用，后者可能只是装置忙。
package iec61850

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	ieccodec "uic/internal/codec/iec61850"
)

// Server 是一个内存变量树模型的 IEC 61850 从站。
type Server struct {
	mu     sync.RWMutex
	ln     net.Listener
	vars   map[string]ieccodec.Data // 键 = MMSName（域 + "$" + 项）
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool

	// 观测计数（不含握手）。
	assocCount int
	readCount  int
	writeCount int

	// 故障注入。
	rejectAssoc bool // true 时回 ACSE 拒绝而不是接受
	dropReads   bool // true 时对读请求不回响应（制造读超时）
}

// NewServer 创建从站；addr 为 "127.0.0.1:0" 时自动分配端口。
func NewServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:    ln,
		vars:  map[string]ieccodec.Data{},
		conns: map[net.Conn]struct{}{},
	}
	// 单一事实来源：默认变量树。examples/iec61850.json 的点位与之一一对应。
	for name, data := range DefaultObjects() {
		s.vars[name] = data
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 停止监听并等待连接退出。
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
// 变量树
// ---------------------------------------------------------------------------

// Set 写入或覆盖一个变量（键是 MMSName，如 "simpleIO$GGIO1$ST$Ind1$stVal"）。
func (s *Server) Set(name string, data ieccodec.Data) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vars[name] = data
}

// SetBool 是 Set 的布尔便捷入口。
func (s *Server) SetBool(name string, v bool) {
	d, _ := ieccodec.DataFromValue(ieccodec.Value{Type: "bool", Bool: v})
	s.Set(name, d)
}

// SetFloat 是 Set 的浮点便捷入口（32 位）。
func (s *Server) SetFloat(name string, v float64) {
	d, _ := ieccodec.DataFromValue(ieccodec.Value{Type: "float32", Num: v})
	s.Set(name, d)
}

// SetInt 是 Set 的整型便捷入口。
func (s *Server) SetInt(name string, v int64) {
	d, _ := ieccodec.DataFromValue(ieccodec.Value{Type: "int32", Num: float64(v)})
	s.Set(name, d)
}

// Get 读出一个变量的当前值（不存在返回 false）。
func (s *Server) Get(name string) (ieccodec.Data, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.vars[name]
	return d, ok
}

// Names 返回全部变量名（升序），模型浏览用。
func (s *Server) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.vars))
	for n := range s.vars {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 故障注入
// ---------------------------------------------------------------------------

// RejectAssociation 让下一次关联被 ACSE 拒绝（验证主站把「关联被拒」单独分类）。
func (s *Server) RejectAssociation(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectAssoc = on
}

// DropReads 让从站对读请求不回响应（制造读超时，验证主站的超时路径）。
func (s *Server) DropReads(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropReads = on
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

// AssocCount 返回累计成功建立的关联数。
func (s *Server) AssocCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.assocCount
}

// ReadCount 返回累计读请求数。
func (s *Server) ReadCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readCount
}

// WriteCount 返回累计写请求数。
func (s *Server) WriteCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeCount
}

// ---------------------------------------------------------------------------
// 连接循环
// ---------------------------------------------------------------------------

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			_ = c.Close()
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
	for {
		frame, err := readFrame(c)
		if err != nil {
			return
		}
		payload, err := ieccodec.ParseTPKT(frame)
		if err != nil || len(payload) < 2 {
			return
		}
		switch payload[1] {
		case ieccodec.COTPCR:
			// 连接确认：refs 随意，只要类型是 CC 主站就接受。
			if _, err := c.Write(ieccodec.BuildCOTPConfirm(ieccodec.DefaultCOTPOptions(), 1, 1)); err != nil {
				return
			}
		case ieccodec.COTPDT:
			if len(payload) < 3 {
				return
			}
			resp, ok := s.handleData(payload[3:])
			if !ok {
				return
			}
			// 空响应表示「刻意不回包」（如 DropReads 制造读超时）：连接保持，
			// 主站靠自己的 deadline 超时。
			if len(resp) == 0 {
				continue
			}
			if _, err := c.Write(ieccodec.BuildCOTPData(resp)); err != nil {
				return
			}
		case ieccodec.COTPDR:
			return
		default:
			return
		}
	}
}

// readFrame 读一个完整的 TPKT 帧（含 4 字节头）。
func readFrame(c net.Conn) ([]byte, error) {
	head := make([]byte, 4)
	if _, err := readFull(c, head); err != nil {
		return nil, err
	}
	if head[0] != ieccodec.TPKTVersion {
		return nil, fmt.Errorf("sim-iec61850: bad tpkt version %d", head[0])
	}
	n := int(head[2])<<8 | int(head[3])
	if n < 4 || n > 8192 {
		return nil, fmt.Errorf("sim-iec61850: bad tpkt length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		if err != nil {
			return got, err
		}
		got += n
	}
	return got, nil
}

// ---------------------------------------------------------------------------
// MMS 业务
// ---------------------------------------------------------------------------

// handleData 处理一段会话层内容：关联 / 数据 / 释放。
//
// 返回 (响应, true) 表示正常回应（响应可能为空：此时不回包，例如故障注入
// 里刻意不回读响应）；(nil, false) 表示连接应当关闭。
func (s *Server) handleData(session []byte) ([]byte, bool) {
	// DT 的头是固定四字节、没有 LI 语义，ParseSPDU 对它走单独分支；这里两个
	// 分支共用同一个解析入口，避免在 sim 里再抄一遍 DT 头判据。
	spdu, err := ieccodec.ParseSPDU(session)
	if err != nil {
		return nil, false
	}
	switch spdu.ID {
	case ieccodec.SPDUConnect:
		return s.handleAssociate(), true
	case ieccodec.SPDUDataTransfer:
		return s.handleRequest(session), true
	case ieccodec.SPDUFinish, ieccodec.SPDUDisconnect:
		// 有序释放：回 RLRE（会话 FN + 表示 ACSE RLRE）。
		rel, err := ieccodec.BuildReleaseResponse()
		if err != nil {
			return nil, false
		}
		return rel, true
	default:
		return nil, false
	}
}

// handleAssociate 回应关联：会话 AC + 表示 CPA + ACSE AARE + MMS Initiate 响应。
func (s *Server) handleAssociate() []byte {
	s.mu.Lock()
	reject := s.rejectAssoc
	// 只有被接受的关联才计数：被拒的关联没有建立任何状态，把它算进
	// AssocCount 会让「关联被拒」这条测试永远看到 1，从而失去意义。
	if !reject {
		s.assocCount++
	}
	s.mu.Unlock()

	var aarePayload []byte
	if reject {
		// 永久拒绝（result=1）：验证主站把关联被拒单独分类、不无限重试。
		aarePayload = ieccodec.BuildAARE(1, 0, nil)
	} else {
		initResp := ieccodec.BuildInitiateResponse(ieccodec.DefaultInitiateRequest())
		aarePayload = ieccodec.BuildAARE(ieccodec.ACSEResultAccepted, 0, initResp)
	}
	pres, err := ieccodec.BuildPresentationAccept(aarePayload)
	if err != nil {
		return nil
	}
	sess, err := ieccodec.BuildSessionAccept(ieccodec.DefaultSessionParams(), pres)
	if err != nil {
		return nil
	}
	return sess
}

// handleRequest 处理一个 MMS 请求（读 / 写 / 模型浏览 / Conclude）。
//
// 返回的是**会话层**载荷：MMS 响应必须先过表示层与会话 DT（WrapData），再由
// serve 补 TPKT+COTP。漏掉 WrapData 时主站的 UnwrapData 会直接失败，表现是
// 「关联成功、但每次读都被对端关连接」——响应体看着像 MMS，外层却不是会话层。
func (s *Server) handleRequest(session []byte) []byte {
	mms, err := ieccodec.UnwrapData(session)
	if err != nil {
		return nil
	}
	tag, err := ieccodec.MMSServiceTag(mms)
	if err != nil {
		return nil
	}
	var resp []byte
	switch tag {
	case ieccodec.ServiceTagRead:
		resp = s.handleRead(mms)
	case ieccodec.ServiceTagWrite:
		resp = s.handleWrite(mms)
	case ieccodec.ServiceTagGetNameList:
		resp = s.handleGetNameList(mms)
	case ieccodec.ServiceTagConclude:
		// Conclude：回 Conclude-Response。
		resp = ieccodec.BuildConcludeResponse()
	default:
		// 不认识的服务：不回响应（主站超时后按协议错误处理）。
		return nil
	}
	if len(resp) == 0 {
		return nil
	}
	dt, err := ieccodec.WrapData(resp)
	if err != nil {
		return nil
	}
	return dt
}

func (s *Server) handleRead(mms []byte) []byte {
	invoke, req, err := ieccodec.ParseReadRequest(mms)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.readCount++
	drop := s.dropReads
	s.mu.Unlock()
	if drop {
		// 制造读超时：不回响应，让主站自己超时。
		return nil
	}
	results := make([]ieccodec.AccessResult, 0, len(req.Names))
	for _, name := range req.Names {
		key := domainItem(name)
		s.mu.RLock()
		data, ok := s.vars[key]
		s.mu.RUnlock()
		if !ok {
			results = append(results, ieccodec.AccessResult{OK: false, Error: ieccodec.DataErrObjectNonExistent})
			continue
		}
		results = append(results, ieccodec.AccessResult{OK: true, Data: data})
	}
	return ieccodec.BuildReadResponse(invoke, results)
}

func (s *Server) handleWrite(mms []byte) []byte {
	invoke, req, err := ieccodec.ParseWriteRequest(mms)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.writeCount++
	s.mu.Unlock()
	results := make([]ieccodec.WriteResult, 0, len(req.Names))
	for i, name := range req.Names {
		if i >= len(req.Values) {
			results = append(results, ieccodec.WriteResult{OK: false, Error: ieccodec.DataErrObjectNonExistent})
			continue
		}
		key := domainItem(name)
		s.mu.Lock()
		_, exists := s.vars[key]
		if exists {
			s.vars[key] = req.Values[i]
		}
		s.mu.Unlock()
		if !exists {
			results = append(results, ieccodec.WriteResult{OK: false, Error: ieccodec.DataErrObjectNonExistent})
			continue
		}
		results = append(results, ieccodec.WriteResult{OK: true})
	}
	return ieccodec.BuildWriteResponse(invoke, results)
}

func (s *Server) handleGetNameList(mms []byte) []byte {
	invoke, req, err := ieccodec.ParseGetNameListRequest(mms)
	if err != nil {
		return nil
	}
	names := s.Names()
	// 按域过滤，并**剥掉域前缀**再回。
	//
	// 变量树里的键是 `simpleIO$GGIO1$ST$Ind1$stVal`（域 + 项），那是本从站
	// 内部用的扁平索引；MMS 的 identifier 在域内唯一，域由 objectScope 单独
	// 携带。把整键原样回出去，客户端会以为项名里还含域，拼一遍就变成
	// `simpleIO/simpleIO.GGIO1...`。这里只回项名 `GGIO1$ST$Ind1$stVal`。
	//
	// 域为空（VMD 作用域）时标准语义是列出域名字典，而不是列出所有域的所有
	// 变量；内置 IED 只有一棵域内的树，为保持「回什么就能读什么」，这里同样
	// 剥掉域前缀——VMD 作用域下等于把唯一的域摊平。
	itemOf := func(n string) string {
		if i := strings.IndexByte(n, '$'); i >= 0 {
			return n[i+1:]
		}
		return n
	}
	if req.Domain != "" {
		prefix := req.Domain + "$"
		filtered := make([]string, 0, len(names))
		for _, n := range names {
			if strings.HasPrefix(n, prefix) && len(n) > len(prefix) {
				filtered = append(filtered, n[len(prefix):])
			}
		}
		names = filtered
	} else {
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, itemOf(n))
		}
		names = out
	}
	// 续读游标：只回 continueAfter 之后的项（升序）。
	if req.ContinueAfter != "" {
		start := 0
		for i, n := range names {
			if n > req.ContinueAfter {
				start = i
				break
			}
			start = i + 1
		}
		names = names[start:]
	}
	const pageSize = 3
	more := len(names) > pageSize
	if more {
		names = names[:pageSize]
	}
	// BuildGetNameListResponse 只产出服务体，invokeID 由 ConfirmedResponse 补上——
	// 主站靠 invokeID 把这一页分回对应的 getNameList 请求。
	return ieccodec.ConfirmedResponse(invoke, ieccodec.BuildGetNameListResponse(names, more))
}

// domainItem 把 MMS ObjectName 折成变量树的键（域 + "$" + 项）。
func domainItem(name ieccodec.ObjectName) string {
	if name.Kind == "domain" {
		return name.Domain + "$" + name.Item
	}
	return name.Name
}
