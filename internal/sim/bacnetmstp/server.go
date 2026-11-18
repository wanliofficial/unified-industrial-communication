// Package bacnetmstp 提供 BACnet MS/TP 内存从站（TCP 透传/串口服务器），
// 用于无硬件自检。
//
// 它模拟的是一台**从站**（MAC 128..254）：不产生令牌、不参与令牌环、
// 不应答 PollForMaster（那是主站之间的握手），只在收到发给自己的
// DataExpectingReply 时回一帧 DataNotExpectingReply，并应答 TestRequest。
// 这与现场大多数 MS/TP 设备（VAV 控制器、风机盘管、阀门执行器）的角色
// 一致；要验证主站的令牌逻辑，得用第二台主站，而本平台的驱动刻意不做
// 多主站。
//
// 对象表与 sim/bacnet（BACnet/IP）保持同一套默认值，这样同一份点表既能
// 指向 47808 的 IP 从站，也能指向 MS/TP 从站，切换承载时不必改点位。
// 代码刻意不复用 sim/bacnet 的 Server：那是一个 UDP 事件循环，把它抽出
// 成「与承载无关的对象模型」会牵动它 500 多行测试；而这里需要的是完全
// 不同的帧循环。两边的对象语义靠测试对齐，而不是靠共享可变状态。
package bacnetmstp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
)

// defaultMAC 是自检用从站的默认地址。128 是从站地址段的第一个地址，
// 与 examples/bacnet-mstp.json 对应。
const defaultMAC = 128

// Options 是启动参数。
type Options struct {
	Addr string
	// MACs 是本从站应答的地址集合。空表示只应答 128。
	MACs []byte
	// AutoIncrement 控制 analog-value:3 是否每秒自增。默认 true；
	// 测试需要固定值时用 DisableAutoIncrement 或这里置 false。
	AutoIncrement bool
}

// prop 是一个属性的内存表示（与 sim/bacnet 的语义一致）。
type prop struct {
	tag     byte
	u       uint64
	f       float64
	text    string
	bytes   []byte
	boolean bool

	writable bool
	arr      []*prop
}

func (p *prop) clone() *prop {
	if p == nil {
		return nil
	}
	c := *p
	if p.bytes != nil {
		c.bytes = append([]byte(nil), p.bytes...)
	}
	if p.arr != nil {
		c.arr = make([]*prop, len(p.arr))
		for i, e := range p.arr {
			c.arr[i] = e.clone()
		}
	}
	return &c
}

func (p *prop) encode() ([]byte, error) {
	switch p.tag {
	case bacnetcodec.AppNull:
		return bacnetcodec.EncodeApplicationValue(bacnetcodec.AppNull, 0, 0, nil, false)
	case bacnetcodec.AppBoolean:
		return bacnetcodec.EncodeApplicationValue(bacnetcodec.AppBoolean, 0, 0, nil, p.boolean)
	case bacnetcodec.AppCharacterStr:
		return bacnetcodec.EncodeApplicationValue(bacnetcodec.AppCharacterStr, 0, 0, []byte(p.text), false)
	case bacnetcodec.AppOctetString, bacnetcodec.AppBitString:
		return bacnetcodec.EncodeApplicationValue(p.tag, 0, 0, p.bytes, false)
	case bacnetcodec.AppReal, bacnetcodec.AppDouble:
		return bacnetcodec.EncodeApplicationValue(p.tag, 0, p.f, nil, false)
	}
	return bacnetcodec.EncodeApplicationValue(p.tag, p.u, 0, nil, false)
}

type objectKey struct {
	typ      int
	instance int
}

// Server 是 BACnet MS/TP 从站。
type Server struct {
	ln   net.Listener
	log  *slog.Logger
	macs map[byte]bool

	mu      sync.Mutex
	objects map[objectKey]map[int]*prop
	closed  chan struct{}
	conns   map[net.Conn]struct{}
	// autoIncrement 让 analog-value:3 每秒自增，便于观察 SSE 与死区。
	autoIncrement bool
	wg            sync.WaitGroup
}

// NewServer 启动一个只应答 MAC 128 的从站。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string) (*Server, error) {
	return NewServerWithOptions(Options{Addr: addr, AutoIncrement: true})
}

// NewServerWithOptions 按 Options 启动从站。
func NewServerWithOptions(opt Options) (*Server, error) {
	addr := opt.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	macs := opt.MACs
	if len(macs) == 0 {
		macs = []byte{defaultMAC}
	}
	set := make(map[byte]bool, len(macs))
	for _, m := range macs {
		if !bacnetcodec.IsSlaveMAC(m) {
			return nil, fmt.Errorf("bacnet-mstp sim: MAC %d is not a slave address (128..254)", m)
		}
		set[m] = true
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bacnet-mstp sim: listen %q: %w", addr, err)
	}
	s := &Server{
		ln:            ln,
		log:           slog.Default(),
		macs:          set,
		objects:       map[objectKey]map[int]*prop{},
		closed:        make(chan struct{}),
		conns:         map[net.Conn]struct{}{},
		autoIncrement: opt.AutoIncrement,
	}
	s.installDefaults()
	s.wg.Add(1)
	go s.accept()
	if s.autoIncrement {
		s.wg.Add(1)
		go s.tick()
	}
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// MACs 返回从站应答的地址列表，供自检命令打印。
func (s *Server) MACs() []byte {
	out := make([]byte, 0, len(s.macs))
	for m := range s.macs {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Close 停止监听、断开所有连接并停止自增。幂等。
func (s *Server) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			// 监听还在却 accept 失败（fd 耗尽等）：短暂退避后继续，
			// 不要让一个瞬时错误把从站整体带走。
			time.Sleep(50 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	br := bufio.NewReaderSize(conn, 1024)
	for {
		frame, err := readFrame(br)
		if err != nil {
			select {
			case <-s.closed:
			default:
				if !isClosed(err) {
					s.log.Debug("bacnetmstp_sim_read", "err", err)
				}
			}
			return
		}
		resp := s.handleFrame(frame)
		if resp == nil {
			continue
		}
		wire, err := bacnetcodec.BuildMSTPFrame(*resp)
		if err != nil {
			continue
		}
		// 从站回帧前留一点方向切换时间。TCP 上不需要，串口透传上是必需的；
		// 用固定 1ms 而不是按波特率计算，是为了让自检的时序与真实设备接近
		// 又不会把测试拖慢。
		time.Sleep(time.Millisecond)
		if _, err := conn.Write(wire); err != nil {
			return
		}
	}
}

// handleFrame 根据帧类型产生应答。返回 nil 表示不应答。
func (s *Server) handleFrame(frame bacnetcodec.MSTPFrame) *bacnetcodec.MSTPFrame {
	switch frame.Type {
	case bacnetcodec.FrameTestRequest:
		if frame.Dest != bacnetcodec.MSTPBroadcast && !s.owns(frame.Dest) {
			return nil
		}
		return &bacnetcodec.MSTPFrame{
			Type: bacnetcodec.FrameTestResponse, Dest: frame.Src, Src: frame.Dest,
		}
	case bacnetcodec.FrameDataExpectingReply:
		// 广播读没有意义（所有从站同时回会撞车），只接受点名叫本机的请求。
		if !s.owns(frame.Dest) {
			return nil
		}
		apdu, _, err := bacnetcodec.ParseNPDU(frame.Data)
		if err != nil {
			return nil
		}
		resp := s.handleAPDU(apdu)
		if resp == nil {
			return nil
		}
		return &bacnetcodec.MSTPFrame{
			Type: bacnetcodec.FrameDataNotExpectingReply,
			Dest: frame.Src,
			Src:  frame.Dest,
			// 应答的 NPDU control 不置「期望应答」位：从站不再等确认。
			Data: bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlNone, resp),
		}
	case bacnetcodec.FrameDataNotExpectingReply:
		// 主站的纯上送/广播写。本从站不执行广播写：地址冲突时无法回
		// 错误，静默改写会造成「看起来成功了」的假象。
		return nil
	case bacnetcodec.FrameToken, bacnetcodec.FramePollForMaster,
		bacnetcodec.FrameReplyToPollForMaster, bacnetcodec.FrameTestResponse,
		bacnetcodec.FrameReplyPostponed:
		// 从站不持有令牌、不应答 PollForMaster。收到这类帧说明总线上的
		// 主站配置有问题（例如把从站地址当成了下一站），静默忽略。
		return nil
	}
	return nil
}

func (s *Server) owns(mac byte) bool { return s.macs[mac] }

// handleAPDU 处理一条 ConfirmedRequest，返回要回送的 APDU。
func (s *Server) handleAPDU(apdu []byte) []byte {
	req, err := bacnetcodec.ParseConfirmedRequest(apdu)
	if err != nil {
		return nil
	}
	if req.Segmented {
		return bacnetcodec.BuildAbort(true, req.InvokeID, bacnetcodec.AbortReasonSegmentationNotSup)
	}
	switch req.Service {
	case bacnetcodec.ServiceReadProperty:
		return s.readProperty(req)
	case bacnetcodec.ServiceWriteProperty:
		return s.writeProperty(req)
	}
	return bacnetcodec.BuildError(req.Service, req.InvokeID,
		bacnetcodec.ErrorClassServices, bacnetcodec.ErrorCodeOther)
}

func (s *Server) readProperty(req bacnetcodec.ConfirmedRequest) []byte {
	objType, instance, property, index, err := bacnetcodec.ReadPropertyRequestParams(req.Params)
	if err != nil {
		return bacnetcodec.BuildReject(req.InvokeID, bacnetcodec.RejectReasonInvalidTag)
	}
	s.mu.Lock()
	p, ec, ecode := s.lookup(objType, instance, property)
	var encoded []byte
	if p != nil {
		if index > 0 && p.arr != nil {
			if index > len(p.arr) {
				p = nil
				ec, ecode = bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidArrayIndex
			} else {
				p = p.arr[index-1]
			}
		} else if index > 0 {
			p = nil
			ec, ecode = bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidArrayIndex
		}
	}
	if p != nil {
		encoded, err = p.encode()
		if err != nil {
			p = nil
			ec, ecode = bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidDataType
		}
	}
	s.mu.Unlock()

	if p == nil {
		return bacnetcodec.BuildError(req.Service, req.InvokeID, ec, ecode)
	}
	resp, err := bacnetcodec.BuildComplexACK(bacnetcodec.ServiceReadProperty, req.InvokeID,
		objType, instance, property, encoded)
	if err != nil {
		return bacnetcodec.BuildReject(req.InvokeID, bacnetcodec.RejectReasonInvalidTag)
	}
	return resp
}

func (s *Server) lookup(objType, instance, property int) (*prop, byte, byte) {
	obj, ok := s.objects[objectKey{objType, instance}]
	if !ok {
		return nil, bacnetcodec.ErrorClassObject, bacnetcodec.ErrorCodeUnknownObject
	}
	p, ok := obj[property]
	if !ok {
		return nil, bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeUnknownProperty
	}
	return p, 0, 0
}

func (s *Server) writeProperty(req bacnetcodec.ConfirmedRequest) []byte {
	objType, instance, property, _, value, err := bacnetcodec.WritePropertyRequestParams(req.Params)
	if err != nil {
		return bacnetcodec.BuildReject(req.InvokeID, bacnetcodec.RejectReasonInvalidTag)
	}
	// WritePropertyRequestParams 丢弃数组下标，这里单独取。
	_, _, indexBytes, _, _, splitErr := bacnetcodec.SplitWritePropertyParams(req.Params)
	index := 0
	if splitErr == nil && indexBytes != nil {
		index = int(decodeIndex(indexBytes))
	}

	tag, u, f, b, bl, decErr := bacnetcodec.DecodeApplicationValue(value)
	if decErr != nil {
		return bacnetcodec.BuildError(req.Service, req.InvokeID,
			bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidDataType)
	}

	s.mu.Lock()
	p, ec, ecode := s.lookup(objType, instance, property)
	if p == nil {
		s.mu.Unlock()
		return bacnetcodec.BuildError(req.Service, req.InvokeID, ec, ecode)
	}
	target := p
	if index > 0 {
		if p.arr == nil || index > len(p.arr) {
			s.mu.Unlock()
			return bacnetcodec.BuildError(req.Service, req.InvokeID,
				bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidArrayIndex)
		}
		target = p.arr[index-1]
	}
	if !target.writable {
		s.mu.Unlock()
		return bacnetcodec.BuildError(req.Service, req.InvokeID,
			bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeWriteAccessDenied)
	}
	updated, convErr := coerce(target, tag, u, f, b, bl)
	if convErr != nil {
		s.mu.Unlock()
		return bacnetcodec.BuildError(req.Service, req.InvokeID,
			bacnetcodec.ErrorClassProperty, bacnetcodec.ErrorCodeInvalidDataType)
	}
	updated.writable = target.writable
	*target = *updated
	s.mu.Unlock()

	return bacnetcodec.BuildSimpleACK(bacnetcodec.ServiceWriteProperty, req.InvokeID)
}

func decodeIndex(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// coerce 把写入值收敛到目标属性自己的存储类型（规则与 sim/bacnet 相同）。
func coerce(target *prop, tag byte, u uint64, f float64, b []byte, bl bool) (*prop, error) {
	out := target.clone()
	switch target.tag {
	case bacnetcodec.AppReal, bacnetcodec.AppDouble:
		switch tag {
		case bacnetcodec.AppReal, bacnetcodec.AppDouble:
			out.f = f
		case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated:
			out.f = float64(u)
		case bacnetcodec.AppSigned:
			out.f = float64(int64(u))
		default:
			return nil, fmt.Errorf("cannot write application tag %d to a real property", tag)
		}
	case bacnetcodec.AppEnumerated, bacnetcodec.AppUnsigned, bacnetcodec.AppSigned:
		switch tag {
		case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated:
			out.u = u
		case bacnetcodec.AppSigned:
			out.u = uint64(int64(u))
		case bacnetcodec.AppBoolean:
			if bl {
				out.u = 1
			} else {
				out.u = 0
			}
		default:
			return nil, fmt.Errorf("cannot write application tag %d to an integer property", tag)
		}
	case bacnetcodec.AppBoolean:
		switch tag {
		case bacnetcodec.AppBoolean:
			out.boolean = bl
		case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated, bacnetcodec.AppSigned:
			out.boolean = u != 0
		default:
			return nil, fmt.Errorf("cannot write application tag %d to a boolean property", tag)
		}
	case bacnetcodec.AppCharacterStr:
		if tag != bacnetcodec.AppCharacterStr {
			return nil, fmt.Errorf("cannot write application tag %d to a string property", tag)
		}
		out.text = string(b)
	case bacnetcodec.AppOctetString, bacnetcodec.AppBitString:
		switch tag {
		case bacnetcodec.AppOctetString, bacnetcodec.AppBitString, bacnetcodec.AppCharacterStr:
			out.bytes = append([]byte(nil), b...)
		default:
			return nil, fmt.Errorf("cannot write application tag %d to an octet-string property", tag)
		}
	case bacnetcodec.AppNull:
		if tag != bacnetcodec.AppNull {
			return nil, fmt.Errorf("null property only accepts a null value")
		}
	default:
		return nil, fmt.Errorf("unsupported property tag %d", target.tag)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 对象表
// ---------------------------------------------------------------------------

func (s *Server) installDefaults() {
	// analog-input:1 —— 只读温度。
	_ = s.setProp(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "AI-1"})
	_ = s.setProp(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropUnits,
		&prop{tag: bacnetcodec.AppEnumerated, u: 62}) // 62 = degrees-celsius
	_ = s.setProp(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: 42.5})

	// analog-value:2 —— 可写设定值，带 16 级 priority-array。
	_ = s.setProp(bacnetcodec.ObjectAnalogValue, 2, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "AV-2"})
	_ = s.setProp(bacnetcodec.ObjectAnalogValue, 2, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: 0, writable: true})
	prio := make([]*prop, 16)
	for i := range prio {
		prio[i] = &prop{tag: bacnetcodec.AppNull, writable: true}
	}
	_ = s.setProp(bacnetcodec.ObjectAnalogValue, 2, bacnetcodec.PropPriorityArray,
		&prop{tag: bacnetcodec.AppNull, arr: prio})

	// analog-value:3 —— 每秒自增的计数。
	_ = s.setProp(bacnetcodec.ObjectAnalogValue, 3, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: 0})

	// binary-input:1 —— 枚举 1 = active。
	_ = s.setProp(bacnetcodec.ObjectBinaryInput, 1, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "BI-1"})
	_ = s.setProp(bacnetcodec.ObjectBinaryInput, 1, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppEnumerated, u: 1})

	// character-string:1 —— 可写字符串。
	_ = s.setProp(bacnetcodec.ObjectCharacterString, 1, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "PUMP-01", writable: true})

	// device:1 —— Probe 读的就是它的 object-name。
	_ = s.setProp(bacnetcodec.ObjectDevice, 1, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "UIC-MSTP-SIM"})
	_ = s.setProp(bacnetcodec.ObjectDevice, 1, bacnetcodec.PropModelName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "sim-mstp"})
}

func (s *Server) setProp(objType, instance, property int, p *prop) error {
	if p == nil {
		return errors.New("bacnet-mstp sim: nil property")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := objectKey{objType, instance}
	obj, ok := s.objects[key]
	if !ok {
		obj = map[int]*prop{}
		s.objects[key] = obj
	}
	obj[property] = p
	return nil
}

// DisableAutoIncrement 关掉 analog-value:3 的每秒自增。
func (s *Server) DisableAutoIncrement() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoIncrement = false
}

func (s *Server) tick() {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			s.mu.Lock()
			if s.autoIncrement {
				if obj, ok := s.objects[objectKey{bacnetcodec.ObjectAnalogValue, 3}]; ok {
					if p, ok := obj[bacnetcodec.PropPresentValue]; ok {
						p.f++
					}
				}
			}
			s.mu.Unlock()
		}
	}
}

// SetAnalog 设置 analog-input:<instance> 的 present-value。
func (s *Server) SetAnalog(instance int, v float64) error {
	return s.setProp(bacnetcodec.ObjectAnalogInput, instance, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: v})
}

// SetAnalogValue 设置 analog-value:<instance> 的 present-value（可写）。
func (s *Server) SetAnalogValue(instance int, v float64) error {
	return s.setProp(bacnetcodec.ObjectAnalogValue, instance, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: v, writable: true})
}

// SetBinary 设置 binary-input:<instance> 的 present-value。
func (s *Server) SetBinary(instance int, v bool) error {
	n := uint64(0)
	if v {
		n = 1
	}
	return s.setProp(bacnetcodec.ObjectBinaryInput, instance, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppEnumerated, u: n})
}

// SetString 设置 character-string:<instance> 的 present-value。
func (s *Server) SetString(instance int, v string) error {
	return s.setProp(bacnetcodec.ObjectCharacterString, instance, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppCharacterStr, text: v, writable: true})
}

// SetValue 按对象名/属性名写入一个值（float64/int/bool/string/[]byte）。
func (s *Server) SetValue(objectType string, instance int, property string, value any) error {
	objType, ok := bacnetcodec.ObjectTypeFromName(objectType)
	if !ok {
		return fmt.Errorf("bacnet-mstp sim: unknown object type %q", objectType)
	}
	propID, ok := bacnetcodec.PropertyFromName(property)
	if !ok {
		return fmt.Errorf("bacnet-mstp sim: unknown property %q", property)
	}
	p := &prop{writable: true}
	switch v := value.(type) {
	case float64:
		p.tag, p.f = bacnetcodec.AppReal, v
	case float32:
		p.tag, p.f = bacnetcodec.AppReal, float64(v)
	case int:
		p.tag, p.u = bacnetcodec.AppUnsigned, uint64(v)
	case int32:
		p.tag, p.u = bacnetcodec.AppSigned, uint64(int64(v))
	case uint64:
		p.tag, p.u = bacnetcodec.AppUnsigned, v
	case bool:
		p.tag, p.boolean = bacnetcodec.AppBoolean, v
	case string:
		p.tag, p.text = bacnetcodec.AppCharacterStr, v
	case []byte:
		p.tag, p.bytes = bacnetcodec.AppOctetString, append([]byte(nil), v...)
	default:
		return fmt.Errorf("bacnet-mstp sim: unsupported value type %T", value)
	}
	return s.setProp(objType, instance, propID, p)
}

// Value 读回一个属性，用于断言写入是否真的落到了从站。
func (s *Server) Value(objectType string, instance int, property string) (any, bool) {
	objType, ok := bacnetcodec.ObjectTypeFromName(objectType)
	if !ok {
		return nil, false
	}
	propID, ok := bacnetcodec.PropertyFromName(property)
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[objectKey{objType, instance}]
	if !ok {
		return nil, false
	}
	p, ok := obj[propID]
	if !ok || p.arr != nil {
		return nil, false
	}
	switch p.tag {
	case bacnetcodec.AppBoolean:
		return p.boolean, true
	case bacnetcodec.AppCharacterStr:
		return p.text, true
	case bacnetcodec.AppOctetString, bacnetcodec.AppBitString:
		return p.bytes, true
	case bacnetcodec.AppReal, bacnetcodec.AppDouble:
		return p.f, true
	case bacnetcodec.AppNull:
		return nil, true
	}
	return p.u, true
}

// Objects 返回对象表摘要（"analog-input:1" 列表），供自检命令打印。
func (s *Server) Objects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for key := range s.objects {
		out = append(out, fmt.Sprintf("%s:%d", bacnetcodec.ObjectTypeName(key.typ), key.instance))
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 分帧
// ---------------------------------------------------------------------------

// readFrame 从 TCP 流转出下一帧（与驱动侧的扫描逻辑相同：逐字节找
// preamble、校验两条 CRC，坏帧丢弃重扫）。
func readFrame(br *bufio.Reader) (bacnetcodec.MSTPFrame, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return bacnetcodec.MSTPFrame{}, err
		}
		if b != bacnetcodec.MSTPPreamble1 {
			continue
		}
		b2, err := br.ReadByte()
		if err != nil {
			return bacnetcodec.MSTPFrame{}, err
		}
		if b2 != bacnetcodec.MSTPPreamble2 {
			if b2 == bacnetcodec.MSTPPreamble1 {
				_ = br.UnreadByte()
			}
			continue
		}
		head := make([]byte, bacnetcodec.MSTPHeaderLen-2)
		if _, err := io.ReadFull(br, head); err != nil {
			return bacnetcodec.MSTPFrame{}, err
		}
		length := int(head[3])<<8 | int(head[4])
		if length > bacnetcodec.MSTPMaxData || !bacnetcodec.CheckHeaderCRC(head[:6]) {
			continue
		}
		body := make([]byte, length+2)
		if _, err := io.ReadFull(br, body); err != nil {
			return bacnetcodec.MSTPFrame{}, err
		}
		if !bacnetcodec.CheckDataCRC(body) {
			continue
		}
		return bacnetcodec.MSTPFrame{
			Type: head[0], Dest: head[1], Src: head[2],
			Data: append([]byte(nil), body[:length]...),
		}, nil
	}
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
