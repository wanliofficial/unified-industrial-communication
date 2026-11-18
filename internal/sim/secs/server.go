// Package secs 提供一个内置的 HSMS/SECS-II 设备（从站）仿真，供自检与测试使用。
//
// 它实现最小采集闭环必需的消息：Select/Deselect/Linktest/Separate 控制消息，
// S1F1/S1F2（Are You There），S1F3/S1F4（状态变量），S2F13/S2F14（设备常数），
// S2F15/S2F16（写设备常数）。另外提供主动推送 S6F11（事件报告）与 S5F1（报警）
// 的能力，用来验证主站在收到异步推送时能正确忽略、继续等自己的应答。
//
// 为什么需要它：SECS/GEM 的握手与事务语义（系统字节配对、Select 前置、应答
// W 位清零）只有在真实连线上跑一遍才可靠。纯内存 loopback 无法覆盖 TCP 帧边界
// 与 HSMS 状态机。
package secs

import (
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	secscodec "uic/internal/codec/secs"
)

// DefaultPort 是 HSMS/TCP 的默认端口。
const DefaultPort = secscodec.DefaultPort

// variable 是一个 SVID 或 ECID 的当前值。
//
// object 必须保留：SVID 与 ECID 是两套独立的编号空间，同一个数字 1 可以既是
// 状态变量又是设备常数。共用一张表会让「把 SVID 编号发到 S2F13」这类主站错误
// 被静默接受——那正是仿真最该抓住的错误。
type variable struct {
	id       uint32
	object   string // svid / ecid
	value    *secscodec.Item
	name     string
	units    string
	writable bool
}

// Options 是可选参数。
type Options struct {
	Name string
	// RejectWrites 为真时，模拟「ACKC7=0 但值随后不生效」的设备：应答照收，
	// 存储层保留旧值。真实设备上有几种机制会造成这个结果（内部联锁拉回、
	// 写进影子副本而非生效副本、设备侧异步校验失败），而 S2F16 都不反映
	// 这些——只有写后重读能发现。自检命令的 -verify 依赖这种设备来验证
	// 「OK 不等于生效」这条判定真的会失败。
	RejectWrites bool
}

// Server 是一个 HSMS 设备仿真。
type Server struct {
	ln   net.Listener
	name string

	mu           sync.Mutex
	vars         map[vkey]*variable // 按 (对象类型, 编号) 索引
	order        []vkey             // 保持插入顺序，便于自检输出
	selected     map[net.Conn]bool
	rejectWrites bool // 写应答照收，但存储层不落值（见 Options）
	wg           sync.WaitGroup
	closed       bool

	// stats
	requests int
	writes   int
}

// NewServer 在 addr（如 "127.0.0.1:0"）上监听 HSMS/TCP。
func NewServer(addr string) (*Server, error) { return NewServerWithOptions(addr, Options{}) }

// NewServerWithOptions 带参数启动。
func NewServerWithOptions(addr string, opt Options) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("secs sim: listen %q: %w", addr, err)
	}
	name := opt.Name
	if name == "" {
		name = "UIC SECS sim"
	}
	s := &Server{
		ln:           ln,
		name:         name,
		rejectWrites: opt.RejectWrites,
		vars:         map[vkey]*variable{},
		selected:     map[net.Conn]bool{},
	}
	s.loadDefaults()
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Name 返回设备名（自检输出用；S1F12 的名称来自变量表，不是它）。
func (s *Server) Name() string { return s.name }

// Requests 返回收到的 DATA 消息数。
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Writes 返回成功写入的设备常数次数。
func (s *Server) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// Close 停止服务并关闭所有连接。可重复调用。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.selected))
	for c := range s.selected {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	_ = s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return nil
}

// vkey 是变量表的键：对象类型 + 编号。
type vkey struct {
	object string
	id     uint32
}

// SetVID 设置（或新增）一个状态变量的值。
func (s *Server) SetVID(id uint32, v *secscodec.Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(secscodec.ObjectSVID, id, v, "", "", false)
}

// SetVIDNamed 设置状态变量，并给出名称与单位（S1F12 会返回它们）。
func (s *Server) SetVIDNamed(id uint32, v *secscodec.Item, name, units string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(secscodec.ObjectSVID, id, v, name, units, false)
}

// SetECID 设置（或新增）一个设备常数的值，并标记为可写。
func (s *Server) SetECID(id uint32, v *secscodec.Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(secscodec.ObjectECID, id, v, "", "", true)
}

func (s *Server) setLocked(object string, id uint32, v *secscodec.Item, name, units string, writable bool) {
	k := vkey{object: object, id: id}
	if _, ok := s.vars[k]; !ok {
		s.order = append(s.order, k)
	}
	s.vars[k] = &variable{id: id, object: object, value: v, name: name, units: units, writable: writable}
}

// Value 返回一个状态变量（SVID）的当前值。
func (s *Server) Value(id uint32) (*secscodec.Item, bool) {
	return s.value(secscodec.ObjectSVID, id)
}

// ValueOf 返回某对象类型下一个编号的当前值。
func (s *Server) ValueOf(object string, id uint32) (*secscodec.Item, bool) {
	return s.value(object, id)
}

func (s *Server) value(object string, id uint32) (*secscodec.Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vars[vkey{object: object, id: id}]
	if !ok {
		return nil, false
	}
	return v.value, true
}

// IDs 返回所有 SVID 编号（按插入顺序）。
func (s *Server) IDs() []uint32 {
	return s.IDsOf(secscodec.ObjectSVID)
}

// IDsOf 返回某对象类型下的全部编号（按插入顺序）。
func (s *Server) IDsOf(object string) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, 0, len(s.order))
	for _, k := range s.order {
		if k.object == object {
			out = append(out, k.id)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 内置默认值
// ---------------------------------------------------------------------------

func (s *Server) loadDefaults() {
	s.SetVIDNamed(1, secscodec.Int(secscodec.FormatU4, 25), "Temperature", "C")
	s.SetVIDNamed(2, secscodec.ASCII("RUN"), "State", "")
	s.SetVIDNamed(3, secscodec.Float(secscodec.FormatF4, 21.5), "Reading", "V")
	s.SetECID(10, secscodec.Int(secscodec.FormatU4, 300))
	s.SetECID(11, secscodec.ASCII("AUTO"))
}

// ---------------------------------------------------------------------------
// 连接处理
// ---------------------------------------------------------------------------

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(c)
		}()
	}
}

// serveConn 处理一条 TCP 连接：先做控制消息状态机，再处理 DATA 消息。
//
// 注意：本仿真假设同一连接上只有一个客户端（主站）。真实场景也是「一个主站
// 连一台设备」，因此不做多客户端并发选择管理。
func (s *Server) serveConn(c net.Conn) {
	s.mu.Lock()
	s.selected[c] = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.selected, c)
		s.mu.Unlock()
		_ = c.Close()
	}()

	for {
		m, err := readFrame(c)
		if err != nil {
			return
		}
		if !m.IsData() {
			s.handleControl(c, m)
			continue
		}
		s.handleData(c, m)
	}
}

// handleControl 处理 Select/Deselect/Linktest/Separate。
func (s *Server) handleControl(c net.Conn, m secscodec.Message) {
	switch m.MsgType {
	case secscodec.MsgTypeSelectReq:
		s.mu.Lock()
		s.selected[c] = true
		s.mu.Unlock()
		s.write(c, secscodec.NewSelectRsp(m.SystemBytes, secscodec.StatusOK))
	case secscodec.MsgTypeDeselectReq:
		s.mu.Lock()
		s.selected[c] = false
		s.mu.Unlock()
		s.write(c, secscodec.NewDeselectRsp(m.SystemBytes, secscodec.StatusOK))
	case secscodec.MsgTypeLinktestReq:
		s.write(c, secscodec.NewLinktestRsp(m.SystemBytes, secscodec.StatusOK))
	case secscodec.MsgTypeSeparateReq:
		_ = c.Close()
	}
}

// handleData 处理一条 DATA 消息。只有处于 selected 状态才应答数据；未选中时
// 回 Reject（真实设备行为），让驱动在 Open 阶段就能发现「连上了但没选中」。
func (s *Server) handleData(c net.Conn, m secscodec.Message) {
	s.mu.Lock()
	sel := s.selected[c]
	s.requests++
	s.mu.Unlock()
	if !sel {
		s.write(c, secscodec.NewRejectReq(m.SystemBytes, m.MsgType, 0x01))
		return
	}
	dm, err := secscodec.ParseData(m)
	if err != nil {
		return
	}
	switch {
	case dm.Stream == 1 && dm.Function == 1 && dm.WBit:
		s.replyData(c, dm, s.onLineData())
	case dm.Stream == 1 && dm.Function == 3:
		s.replyStatus(c, dm, secscodec.ObjectSVID)
	case dm.Stream == 1 && dm.Function == 11:
		s.replyNameList(c, dm)
	case dm.Stream == 2 && dm.Function == 13:
		s.replyStatus(c, dm, secscodec.ObjectECID)
	case dm.Stream == 2 && dm.Function == 15:
		s.replyConstantAck(c, dm)
	}
}

func (s *Server) onLineData() []byte {
	body, _ := secscodec.EncodeOnLineData("UIC-SECS-01", "1.0")
	return body
}

// replyStatus 应答 S1F3（SVID）或 S2F13（ECID）：按请求里的编号列表回取值。
//
// 编号缺失时不回该编号的取值（主站据此判 address_error）；全部缺失时回空列表。
// 两张表分开查：SVID 编号发到 S2F13 时应该在应答里缺席，而不是「碰巧读到
// 同号设备常数」。
func (s *Server) replyStatus(c net.Conn, dm secscodec.DataMessage, object string) {
	ids, err := secscodec.DecodeIDList(dm.Body)
	if err != nil {
		return
	}
	// 每个请求编号都必须有应答位：S1F4 是按位置的纯值列表，省略未知
	// 编号会让后面所有值错位。未知编号用零长 Binary 占位，主站按
	// 「空二进制 = 不存在」归类为地址错误。
	vals := make([]secscodec.StatusValue, 0, len(ids))
	s.mu.Lock()
	for _, id := range ids {
		if v, ok := s.vars[vkey{object: object, id: id}]; ok {
			vals = append(vals, secscodec.StatusValue{ID: id, Value: v.value})
		} else {
			vals = append(vals, secscodec.StatusValue{ID: id,
				Value: secscodec.Binary(nil)})
		}
	}
	s.mu.Unlock()
	// S1F4 = L,n{V}（不回显 SVID）；S2F14 = L,n{L,2{ECID,V}}（回显 ECID）。
	// 两条路径的应答体不同是 SEMI E5 的规定，不是可以共用的实现细节。
	var body []byte
	if dm.Stream == 1 {
		items := make([]*secscodec.Item, 0, len(vals))
		for _, v := range vals {
			items = append(items, v.Value)
		}
		var e error
		body, e = secscodec.EncodeValueReply(items)
		if e != nil {
			return
		}
	} else {
		var e error
		body, e = secscodec.EncodeStatusReply(vals)
		if e != nil {
			return
		}
	}
	if err != nil {
		return
	}
	// S1F3 → S1F4（Stream 1）；S2F13 → S2F14（Stream 2），Function 是请求的 +1。
	s.replyData(c, dm, body, secscodec.SF{Stream: dm.Stream, Function: dm.Function + 1})
}

// replyNameList 应答 S1F11：按请求编号回「编号 + 名称 + 单位」。
//
// 不存在的 SVID 不出现在应答里——与真实设备的 S1F12 一致（只回认识的编号）。
func (s *Server) replyNameList(c net.Conn, dm secscodec.DataMessage) {
	ids, err := secscodec.DecodeIDList(dm.Body)
	if err != nil {
		return
	}
	names := make([]secscodec.StatusVariable, 0, len(ids))
	s.mu.Lock()
	if len(ids) == 0 {
		// 空列表 = 全部（S1F11 的标准语义）。
		for _, k := range s.order {
			if k.object != secscodec.ObjectSVID {
				continue
			}
			v := s.vars[k]
			names = append(names, secscodec.StatusVariable{ID: v.id, Name: v.name, Units: v.units})
		}
	} else {
		for _, id := range ids {
			if v, ok := s.vars[vkey{object: secscodec.ObjectSVID, id: id}]; ok {
				names = append(names, secscodec.StatusVariable{ID: v.id, Name: v.name, Units: v.units})
			}
		}
	}
	s.mu.Unlock()
	body, err := secscodec.EncodeStatusNameReply(names)
	if err != nil {
		return
	}
	s.replyData(c, dm, body, secscodec.SFStatusNamelist)
}

// replyConstantAck 应答 S2F15：写值并回 S2F16 ACKC7=0。
//
// 只接受已存在的 ECID：往不存在的编号写会拒绝（ACKC7=1）。真实设备对未知
// 常数编号的应答正是「拒绝」而不是「新建一个」。
func (s *Server) replyConstantAck(c net.Conn, dm secscodec.DataMessage) {
	consts, err := secscodec.DecodeStatusReply(dm.Body)
	if err != nil {
		return
	}
	ack := 0
	s.mu.Lock()
	for _, kv := range consts {
		v, ok := s.vars[vkey{object: secscodec.ObjectECID, id: kv.ID}]
		if !ok {
			ack = 1
			continue
		}
		// rejectWrites 模拟「ACK 被接受但值不生效」的设备：应答照回 0，
		// 存储层保留旧值。自检命令靠它验证「OK 不等于生效」这条判定。
		if s.rejectWrites {
			continue
		}
		v.value = kv.Value
		s.writes++
	}
	s.mu.Unlock()
	body, _ := secscodec.EncodeConstantAck(ack == 0)
	s.replyData(c, dm, body, secscodec.SF{Stream: 2, Function: 16})
}

// replyData 发送一条 Secondary（W 位清零）应答。
func (s *Server) replyData(c net.Conn, dm secscodec.DataMessage, body []byte, sf ...secscodec.SF) {
	stream, function := dm.Stream, dm.Function+1
	if len(sf) > 0 {
		stream, function = sf[0].Stream, sf[0].Function
	}
	resp := secscodec.DataMessage{
		SystemBytes: dm.SystemBytes,
		Stream:      stream,
		Function:    function,
		WBit:        false,
		Body:        body,
	}
	s.write(c, resp.ToMessage())
}

// write 发送一条消息。
//
// 只设写截止时间（SetWriteDeadline），绝不用 SetDeadline：后者同时给读设置
// 截止时间，于是「空闲超过 2 秒的连接」会在下一次读取时直接超时断开——设备
// 侧看到的就是「主站每几秒重连一次」。写截止时间只防「对端不读导致阻塞」。
func (s *Server) write(c net.Conn, m secscodec.Message) {
	raw, err := secscodec.Encode(m)
	if err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(raw)
	_ = c.SetWriteDeadline(time.Time{})
}

// ---------------------------------------------------------------------------
// 主动推送（测试驱动能否忽略异步消息）
// ---------------------------------------------------------------------------

// PushEvent 向所有已选中的连接主动推送一条 S6F11 事件报告。
//
// 它模拟真实设备的异步上报：主站正在等自己的应答时，这条推送从另一条路径进来，
// 必须被主站丢弃。用它在驱动测试里钉住「异步推送不破坏事务」。
func (s *Server) PushEvent(ceid uint32, report secscodec.Report) {
	body, err := secscodec.EncodeEventReport(secscodec.Event{
		DataID: 1, CEID: ceid, Reports: []secscodec.Report{report},
	})
	if err != nil {
		return
	}
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.selected))
	for c, sel := range s.selected {
		if sel {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()
	for _, c := range conns {
		s.write(c, secscodec.DataMessage{
			SystemBytes: s.nextSys(), Stream: 6, Function: 11, WBit: false, Body: body,
		}.ToMessage())
	}
}

// PushAlarm 向所有已选中的连接主动推送一条 S5F1 报警报告。
func (s *Server) PushAlarm(a secscodec.Alarm) {
	body, err := secscodec.EncodeAlarmReport(a)
	if err != nil {
		return
	}
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.selected))
	for c, sel := range s.selected {
		if sel {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()
	for _, c := range conns {
		s.write(c, secscodec.DataMessage{
			SystemBytes: s.nextSys(), Stream: 5, Function: 1, WBit: false, Body: body,
		}.ToMessage())
	}
}

func (s *Server) nextSys() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint32(time.Now().UnixNano() & 0x7fffffff)
}

// ---------------------------------------------------------------------------
// 帧读取
// ---------------------------------------------------------------------------

// readFrame 从连接读一条完整的 HSMS 消息。
func readFrame(c net.Conn) (secscodec.Message, error) {
	var head [secscodec.FrameLen]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return secscodec.Message{}, err
	}
	total := secscodec.FrameLen
	if n, err := secscodec.FrameDataLen(head[:]); err != nil {
		return secscodec.Message{}, err
	} else {
		total += int(n)
	}
	buf := make([]byte, total)
	copy(buf, head[:])
	if _, err := io.ReadFull(c, buf[secscodec.FrameLen:]); err != nil {
		return secscodec.Message{}, err
	}
	return secscodec.Decode(buf)
}

// SortedIDs 返回按数值排序的 SVID 编号（自检输出用）。
func (s *Server) SortedIDs() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, 0, len(s.order))
	for _, k := range s.order {
		if k.object == secscodec.ObjectSVID {
			out = append(out, k.id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
