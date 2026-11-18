// Package bacnet 提供 BACnet/IP 内存从站（UDP），用于无硬件自检。
//
// 从站只实现「对象属性 + ReadProperty / WriteProperty」这一组最少服务：
// 它验证的是主站的对象寻址、标签编解码与错误映射，而不是一个完整的
// BACnet 设备栈。刻意不做的有：Who-Is/I-Am、COV 订阅、ReadPropertyMultiple、
// 分段传输。这些能力缺失时主站应当退回单属性读（我们的主站本来也只做单读）。
//
// 属性表默认覆盖现场最常见的四类对象：模拟量输入（只读）、模拟量值（可写）、
// 二进制输入（枚举）、字符串对象。自检脚本可以直接按 examples/bacnet.json
// 里的地址读，不需要先配置一遍设备。
package bacnet

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
)

// prop 是一个属性的内存表示。数值类属性按「整数 / 浮点 / 文本 / 布尔」
// 四种存储，写入时再按目标属性的标签做类型转换。
type prop struct {
	tag     byte
	u       uint64
	f       float64
	text    string
	bytes   []byte
	boolean bool

	// writable 标记该属性是否接受 WriteProperty。真实设备上
	// present-value 的可写性取决于对象类型（AI 只读、AV 可写），
	// 从站必须复现这个差别，否则主站的权限错误映射测不到。
	writable bool

	// arr 非 nil 表示这是一个数组属性（如 priority-array）。
	// 数组的「不带下标读」返回元素个数，与标准一致。
	arr []*prop
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

// objectKey 是「对象类型 + 实例号」。
type objectKey struct {
	typ      int
	instance int
}

// Server 是 BACnet/IP 内存从站。
type Server struct {
	pc  *net.UDPConn
	log *slog.Logger

	mu      sync.Mutex
	objects map[objectKey]map[int]*prop
	closed  chan struct{}
	// autoIncrement 让 analog-value:3 每秒自增，便于观察 SSE 与死区。
	autoIncrement bool
}

// NewServer 启动从站。addr 为空时监听 127.0.0.1:0。
func NewServer(addr string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("bacnet sim: resolve %q: %w", addr, err)
	}
	pc, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		return nil, fmt.Errorf("bacnet sim: listen %q: %w", addr, err)
	}
	s := &Server{
		pc:            pc,
		log:           slog.Default(),
		objects:       map[objectKey]map[int]*prop{},
		closed:        make(chan struct{}),
		autoIncrement: true,
	}
	s.installDefaults()
	go s.serve()
	go s.tick()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.pc.LocalAddr().String() }

// Close 停止监听与自增。
func (s *Server) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return s.pc.Close()
}

// DisableAutoIncrement 关掉 analog-value:3 的每秒自增，便于断言固定值。
func (s *Server) DisableAutoIncrement() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoIncrement = false
}

func (s *Server) serve() {
	buf := make([]byte, 65535)
	for {
		n, raddr, err := s.pc.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 单条报文读失败不足以让从站退出：UDP 上偶发的截断错误
			// 很常见，继续服务下一包即可。
			continue
		}
		req := append([]byte(nil), buf[:n]...)
		resp := s.handle(req)
		if resp == nil {
			continue
		}
		if _, err := s.pc.WriteToUDP(resp, raddr); err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
		}
	}
}

// tick 让 analog-value:3 每秒自增一次。
func (s *Server) tick() {
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

// ---------------------------------------------------------------------------
// 报文处理
// ---------------------------------------------------------------------------

// handle 处理一条 BACnet/IP 报文，返回要回送的整包（含 BVLC 头）。
// 返回 nil 表示不该应答（例如对端发来的是应答而不是请求）。
func (s *Server) handle(datagram []byte) []byte {
	apdu, _, err := bacnetcodec.ParseMessage(datagram)
	if err != nil {
		// 无法定位 invoke ID 时不可能构造合法应答；静默丢弃是唯一正确选择。
		return nil
	}
	req, err := bacnetcodec.ParseConfirmedRequest(apdu)
	if err != nil {
		return nil
	}
	if req.Segmented {
		// 我们只接受单段请求：分段需要段窗口与重组，超出从站范围。
		// 「收到不支持的分段报文」按标准回 Abort(segmentation-not-supported)，
		// 不是 Reject——Reject 表里没有这个原因。
		return bacnetcodec.BuildMessage(1, bacnetcodec.NPDUControlExpectReply,
			bacnetcodec.BuildAbort(true, req.InvokeID, bacnetcodec.AbortReasonSegmentationNotSup))
	}

	var resp []byte
	switch req.Service {
	case bacnetcodec.ServiceReadProperty:
		resp = s.readProperty(req)
	case bacnetcodec.ServiceWriteProperty:
		resp = s.writeProperty(req)
	default:
		resp = bacnetcodec.BuildError(req.Service, req.InvokeID,
			bacnetcodec.ErrorClassServices, bacnetcodec.ErrorCodeOther)
	}
	if resp == nil {
		return nil
	}
	return bacnetcodec.BuildMessage(1, bacnetcodec.NPDUControlExpectReply, resp)
}

func (s *Server) readProperty(req bacnetcodec.ConfirmedRequest) []byte {
	objType, instance, property, index, err := bacnetcodec.ReadPropertyRequestParams(req.Params)
	if err != nil {
		return bacnetcodec.BuildReject(req.InvokeID, bacnetcodec.RejectReasonInvalidTag)
	}
	s.mu.Lock()
	p, ec, ecode := s.lookup(objType, instance, property, index)
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
			// 对非数组属性带下标读取是错误，而不是「忽略下标」——
			// 后者会让配置里的下标笔误悄悄读到整个属性。
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

// lookup 找到属性；失败时返回 nil 与要回送的错误类/码。
func (s *Server) lookup(objType, instance, property, index int) (*prop, byte, byte) {
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
	// 数组下标单独取：WritePropertyRequestParams 丢弃了它。
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
	p, ec, ecode := s.lookup(objType, instance, property, index)
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

// decodeIndex 把数组下标的原始字节转成整数（可能是 1/2/4 字节）。
func decodeIndex(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// coerce 把写入值转换成目标属性自己的存储类型。
//
// 真实设备对 present-value 的类型要求由对象决定：analog-value 收 REAL，
// binary-value 收 ENUMERATED。这里同样做一次收敛，从而能对
// 「把字符串写进模拟量」这类错误回 invalid-data-type，而不是默默存下
// 一个无法再读回来的值。
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
		// 写 NULL 到优先级数组的元素表示「释放该优先级」，存储就是一个 NULL。
		if tag != bacnetcodec.AppNull {
			return nil, fmt.Errorf("null property only accepts a null value")
		}
	default:
		return nil, fmt.Errorf("unsupported property tag %d", target.tag)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 对象表写入（测试与自检用）
// ---------------------------------------------------------------------------

func (s *Server) installDefaults() {
	// analog-input:1 —— 只读温度，object-name / units / present-value 都有。
	_ = s.setProp(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "AI-1"})
	_ = s.setProp(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropUnits,
		&prop{tag: bacnetcodec.AppEnumerated, u: 62}) // 62 = degrees-celsius
	_ = s.setProp(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: 42.5})

	// analog-value:2 —— 可写设定值，带 16 个优先级的 priority-array。
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

	// analog-value:3 —— 每秒自增的计数，用来观察采集与 SSE。
	_ = s.setProp(bacnetcodec.ObjectAnalogValue, 3, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: 0})

	// binary-input:1 —— 枚举值 1 = active。
	_ = s.setProp(bacnetcodec.ObjectBinaryInput, 1, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "BI-1"})
	_ = s.setProp(bacnetcodec.ObjectBinaryInput, 1, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppEnumerated, u: 1})

	// character-string:1 —— 可写的字符串对象。
	_ = s.setProp(bacnetcodec.ObjectCharacterString, 1, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "PUMP-01", writable: true})

	// device:1 —— Probe 读的就是它的 object-name。
	_ = s.setProp(bacnetcodec.ObjectDevice, 1, bacnetcodec.PropObjectName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "UIC-SIM"})
	_ = s.setProp(bacnetcodec.ObjectDevice, 1, bacnetcodec.PropModelName,
		&prop{tag: bacnetcodec.AppCharacterStr, text: "sim"})
}

func (s *Server) setProp(objType, instance, property int, p *prop) error {
	if p == nil {
		return errors.New("bacnet sim: nil property")
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

// SetAnalog 设置 analog-input:<instance> 的 present-value。
func (s *Server) SetAnalog(instance int, v float64) error {
	return s.setProp(bacnetcodec.ObjectAnalogInput, instance, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: v})
}

// SetAnalogValue 设置 analog-value:<instance> 的 present-value（可写对象）。
func (s *Server) SetAnalogValue(instance int, v float64) error {
	return s.setProp(bacnetcodec.ObjectAnalogValue, instance, bacnetcodec.PropPresentValue,
		&prop{tag: bacnetcodec.AppReal, f: v, writable: true})
}

// SetBinary 设置 binary-input:<instance> 的 present-value（true → active）。
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

// SetValue 按对象名/属性名写入一个值。value 支持 float64、int、bool、string、[]byte。
//
// 它存在的意义是给测试一个「不依赖具体对象类型」的入口：验证驱动对
// UNSIGNED/DOUBLE/OCTET STRING 的处理时，不必为每种组合新增一个方法。
func (s *Server) SetValue(objectType string, instance int, property string, value any) error {
	objType, ok := bacnetcodec.ObjectTypeFromName(objectType)
	if !ok {
		return fmt.Errorf("bacnet sim: unknown object type %q", objectType)
	}
	propID, ok := bacnetcodec.PropertyFromName(property)
	if !ok {
		return fmt.Errorf("bacnet sim: unknown property %q", property)
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
		return fmt.Errorf("bacnet sim: unsupported value type %T", value)
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

// Objects 返回对象表的可读摘要（"analog-input:1" 列表），供自检命令打印。
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
