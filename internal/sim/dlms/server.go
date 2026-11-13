// Package dlms 提供 DLMS/COSEM 的内存从站（虚拟电表），用于自检与演示。
//
// 从站复用 codec/dlms 的组帧/解帧、AARQ/AARE 与 A-XDR 编码，因此「主站
// 解出来的值」与「从站放进去的值」不会因为两边各写一套字节规则而漂移。
//
// # 它模拟什么（以及为什么只模拟这些）
//
//   - HDLC 层：SNRM ↔ UA、DISC ↔ UA、I 帧 ↔ I 帧、序号回绕；
//   - 应用层：AARQ ↔ AARE（LN + 无安全）、Get-Request-Normal ↔
//     Get-Response-Normal；
//   - 对象层：一张 COSEM 对象表（class + OBIS + attribute → A-XDR 值），
//     含 register / data / clock 几类，值带 scaler_unit。
//
// 刻意**不模拟**的：
//
//   - HLS/GMAC 认证：本从站只接受无安全的 AARQ。收到带 authentication-value
//     的请求会回 AARE result=rejected(1) + diagnostic=authentication-required，
//     这是真实表在密钥错时的行为——比"假装接受再拒绝数据"更容易排查。
//   - SET/ACTION：本从站只回答 GET，其它服务回 Data-Access-Result=
//     read-write-denied 或 ConfirmedServiceError。写方向没有实现，回"成功"
//     会让自检得出错误结论。
//   - 分块读（block transfer）：应答超过 max-PDU 时真实表会分块。本从站
//     直接把整段回出去——现场若要复原"分块"行为，把 max-PDU 调小并写一个
//     专门用例，而不是让默认路径就带分块状态机。
//   - 一个进程多块表：一个 Server 实例就是一台表（一个物理设备地址）。
//
// # 为什么从站也有错误注入
//
// 自检的价值在于证明**主站的错误路径是通的**，而不只是成功路径。因此
// 从站提供 SetSilent / SetBadChecksum / SetRejectAssociation 等开关，
// 让测试能构造"表不回话""校验序列坏""拒绝关联"这些现场故障。
package dlms

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	dl "uic/internal/codec/dlms"
	"uic/internal/transport"
)

// Object 是对象表里的一个属性。
type Object struct {
	// ClassID 是 COSEM 类号。
	ClassID uint16
	// OBIS 是逻辑名（6 字节）。
	OBIS dl.OBIS
	// Attribute 是属性号。
	Attribute int8
	// Value 是该属性的 A-XDR 值（未缩放，编码时原样发出）。
	Value dl.Value
	// Name 是给自检命令读的可读名。
	Name string
}

// Server 是一台内存 DLMS 从站（虚拟电表）。
type Server struct {
	mu sync.RWMutex

	// clientAddr / serverAddr / addrSize 是 HDLC 地址配置。
	clientAddr int
	serverAddr int
	addrSize   int

	// objects 是对象表，按 "class/obis/attr" 索引。
	objects map[string]Object
	// order 保持插入顺序，供自检命令稳定输出。
	order []string

	// associated 表示当前是否已建立应用关联。
	associated bool
	// ns/nr 是服务端 I 帧序号。
	ns, nr uint8

	// 故障注入。
	silent            bool
	badChecksum       bool
	rejectAssociation bool

	// 计数。
	requests  int64
	snrmCount int64
	aarqCount int64
	getCount  int64
	discCount int64

	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed bool
	stream transport.Stream
}

// Option 调整从站的初始状态。
type Option func(*Server)

// WithAddress 设置客户端与服务端地址、地址域字节数。
func WithAddress(client, server, size int) Option {
	return func(s *Server) {
		if client >= 0 && client <= 0x7F {
			s.clientAddr = client
		}
		if server >= 0 && server <= 0x3FFF {
			s.serverAddr = server
		}
		if size == 1 || size == 2 || size == 4 {
			s.addrSize = size
		}
	}
}

// WithObjects 替换整张对象表。
func WithObjects(objs []Object) Option {
	return func(s *Server) {
		s.objects = map[string]Object{}
		s.order = nil
		for _, o := range objs {
			s.put(o)
		}
	}
}

// NewServer 创建并启动 TCP 从站；addr 为 "127.0.0.1:0" 时自动分配端口。
func NewServer(addr string, opts ...Option) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newServer(opts...)
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func newServer(opts ...Option) *Server {
	s := &Server{
		clientAddr: 0x10,
		serverAddr: 0x01,
		addrSize:   1,
		objects:    map[string]Object{},
		conns:      map[net.Conn]struct{}{},
	}
	for _, o := range opts {
		o(s)
	}
	if len(s.objects) == 0 {
		for _, o := range DefaultObjects() {
			s.put(o)
		}
	}
	return s
}

// key 是对象表索引键。
func objKey(class uint16, obis dl.OBIS, attr int8) string {
	return fmt.Sprintf("%d/%s/%d", class, obis.String(), attr)
}

// put 插入/替换一个对象（调用方持锁）。
func (s *Server) put(o Object) {
	k := objKey(o.ClassID, o.OBIS, o.Attribute)
	if _, exists := s.objects[k]; !exists {
		s.order = append(s.order, k)
	}
	s.objects[k] = o
}

// DefaultObjects 返回一台典型三相电表的对象表。
//
// 值的选取覆盖 A-XDR 的主要分支：
//
//	uint32       正向/反向有功电能（Wh，实际要配合 scaler_unit 才是 kWh）
//	uint16       电压/电流（原始值，需乘 10^scaler）
//	struct        scaler_unit（{integer, enum}）
//	octet-string 表号、逻辑设备名
//	visible      厂商名、固件版本
//	date_time    时钟（12 字节）
//
// 这正是现场一块表上会同时出现的形态：同一个 Get 服务，返回值类型取决于
// 对象——点表写 length 没有意义，宽度由应答里的标签决定。
func DefaultObjects() []Object {
	reg := func(obis string, class uint16, name string, v dl.Value, scaler int, unit int) []Object {
		o, err := dl.ParseOBIS(obis)
		if err != nil {
			panic(err) // 内置表写错是编程错误，不是运行时错误
		}
		return []Object{
			{ClassID: class, OBIS: o, Attribute: dl.AttrValue, Value: v, Name: name},
			{ClassID: class, OBIS: o, Attribute: dl.AttrScalerUnit,
				Value: dl.Structure(dl.I8(int8(scaler)), dl.Enum(uint8(unit))),
				Name:  name + " 的 scaler_unit"},
			{ClassID: class, OBIS: o, Attribute: dl.AttrLogicalName,
				Value: dl.Octets(o[:]), Name: name + " 的 logical_name"},
		}
	}
	out := []Object{}
	// 电能：以 Wh 为基本单位、scaler=0（不做缩放）。
	//
	// 单位码必须写对：29 才是 Wh，30 是 VAh。写错不会报错，只会让自检命令
	// 把电量标成"视在电能"——而数值一切正常，最难发现。
	out = append(out, reg("1.0.1.8.0.255", dl.ClassRegister, "正向有功电能（总量）",
		dl.U32(12345678), 0, 29)...) // Wh
	out = append(out, reg("1.0.1.8.1.255", dl.ClassRegister, "正向有功电能（费率 1）",
		dl.U32(4567890), 0, 29)...)
	out = append(out, reg("1.0.2.8.0.255", dl.ClassRegister, "反向有功电能（总量）",
		dl.U32(234567), 0, 29)...)
	out = append(out, reg("1.0.3.8.0.255", dl.ClassRegister, "正向无功电能（总量）",
		dl.U32(987654), 0, 31)...) // varh
	// 瞬时量：以 0.1 为单位存，scaler=-1。
	out = append(out, reg("1.0.32.7.0.255", dl.ClassRegister, "A 相电压",
		dl.U16(2201), -1, 34)...) // 220.1 V
	out = append(out, reg("1.0.52.7.0.255", dl.ClassRegister, "B 相电压",
		dl.U16(2198), -1, 34)...)
	out = append(out, reg("1.0.72.7.0.255", dl.ClassRegister, "C 相电压",
		dl.U16(2205), -1, 34)...)
	out = append(out, reg("1.0.31.7.0.255", dl.ClassRegister, "A 相电流",
		dl.U16(512), -2, 32)...) // 5.12 A
	out = append(out, reg("1.0.51.7.0.255", dl.ClassRegister, "B 相电流",
		dl.U16(498), -2, 32)...)
	out = append(out, reg("1.0.71.7.0.255", dl.ClassRegister, "C 相电流",
		dl.U16(507), -2, 32)...)
	out = append(out, reg("1.0.1.7.0.255", dl.ClassRegister, "总有功功率",
		dl.I32(3456), -1, 26)...) // 345.6 W
	out = append(out, reg("1.0.13.7.0.255", dl.ClassRegister, "功率因数",
		dl.I16(985), -3, 0)...) // 0.985
	out = append(out, reg("1.0.14.7.0.255", dl.ClassRegister, "电网频率",
		dl.U16(5001), -2, 43)...) // 50.01 Hz

	// 标识类：Data(1)，值分别是 octet-string / visible-string。
	add := func(obis string, class uint16, attr int8, name string, v dl.Value) {
		o, err := dl.ParseOBIS(obis)
		if err != nil {
			panic(err)
		}
		out = append(out, Object{ClassID: class, OBIS: o, Attribute: attr, Value: v, Name: name})
	}
	add("0.0.42.0.0.255", dl.ClassData, dl.AttrValue, "逻辑设备名", dl.Octets([]byte("METER-LD1")))
	add("0.0.96.1.0.255", dl.ClassData, dl.AttrValue, "表号", dl.Octets([]byte("12345678")))
	add("0.0.96.1.2.255", dl.ClassData, dl.AttrValue, "固件版本", dl.Text("V1.0.4"))
	add("0.0.96.1.3.255", dl.ClassData, dl.AttrValue, "厂商名称", dl.Text("UIC"))

	// 时钟：2026-11-13（星期五）08:30:00，UTC。
	//
	// 12 字节按 IEC 62056-62 §4.1.6.1 排列：
	//	07EA 年=2026  0B 月=11  0D 日=13  05 星期=周五
	//	08 时   1E 分=30  00 秒  00 百分秒  0000 时区偏差  00 状态
	// 星期这一字节在日月之后、时分之前，不是"日"——顺序记错就会解出一个
	// 月余之后的日期，或者干脆解不出来。
	clock := dl.Value{Tag: dl.TagDateTime, Raw: []byte{
		0x07, 0xEA, 0x0B, 0x0D, 0x05, 0x08, 0x1E, 0x00, 0x00, 0x00, 0x00, 0x00,
	}}
	add("0.0.1.0.0.255", dl.ClassClock, dl.AttrValue, "电表时钟", clock)

	// Association LN 的 object_list（attribute 2）：这里给一个短的占位结构，
	// 让自检能看到"对象表存在"而不是 object-undefined。真实表的 object_list
	// 是 array of structure{long, long, long(obis)}，本从站只放 3 条。
	add("0.0.40.0.0.255", dl.ClassAssociationLN, dl.AttrValue, "关联对象表（节选）",
		dl.Array(
			dl.Structure(dl.U16(dl.ClassData), dl.U8(0), dl.Octets([]byte{0x00, 0x00, 0x2A, 0x00, 0x00, 0xFF})),
			dl.Structure(dl.U16(dl.ClassRegister), dl.U8(0), dl.Octets([]byte{0x01, 0x00, 0x01, 0x08, 0x00, 0xFF})),
			dl.Structure(dl.U16(dl.ClassClock), dl.U8(0), dl.Octets([]byte{0x00, 0x00, 0x01, 0x00, 0x00, 0xFF})),
		))
	return out
}

// Addr 返回监听地址（TCP 模式）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Requests 返回收到的完整 HDLC 请求帧数。
func (s *Server) Requests() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.requests
}

// Counts 返回各阶段计数（SNRM/AARQ/GET/DISC），供自检断言。
func (s *Server) Counts() (snrm, aarq, get, disc int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snrmCount, s.aarqCount, s.getCount, s.discCount
}

// Objects 返回对象表的副本（按插入顺序）。
func (s *Server) Objects() []Object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Object, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.objects[k])
	}
	return out
}

// SetValue 替换一个对象的值（按 class + OBIS + attribute 定位）。
func (s *Server) SetValue(class uint16, obis string, attr int8, v dl.Value) error {
	o, err := dl.ParseOBIS(obis)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := objKey(class, o, attr)
	obj, ok := s.objects[k]
	if !ok {
		return fmt.Errorf("从站里没有对象 %s", k)
	}
	obj.Value = v
	s.objects[k] = obj
	return nil
}

// Value 读取一个对象的当前值。
func (s *Server) Value(class uint16, obis string, attr int8) (dl.Value, bool) {
	o, err := dl.ParseOBIS(obis)
	if err != nil {
		return dl.Value{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[objKey(class, o, attr)]
	return obj.Value, ok
}

// SetSilent 开关"完全不应答"（模拟地址错/断线）。
func (s *Server) SetSilent(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = on
}

// SetBadChecksum 开关"应答的 FCS 坏一个字节"（模拟线路干扰）。
func (s *Server) SetBadChecksum(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badChecksum = on
}

// SetRejectAssociation 开关"拒绝应用关联"（模拟需要认证的表）。
func (s *Server) SetRejectAssociation(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectAssociation = on
}

// Close 停止从站；幂等。
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
	st := s.stream
	s.mu.Unlock()

	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	if st != nil {
		_ = st.Close()
	}
	s.wg.Wait()
	return err
}

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
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	st := &tcpStream{conn: c}
	_ = s.ServeStream(st)
}

// ServeStream 在一条字节流上跑从站循环。串口/pty 与 TCP 共用它。
func (s *Server) ServeStream(st transport.Stream) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.stream = st
	// 一次新连接的开始意味着重新建链。
	s.associated = false
	s.ns, s.nr = 0, 0
	s.mu.Unlock()

	rd := dl.NewReader(st)
	for {
		fr, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || isClosed(err) {
				return nil
			}
			return err
		}
		resp, silent := s.handle(fr)
		if silent || len(resp) == 0 {
			continue
		}
		if err := dl.WriteFrame(st, resp); err != nil {
			return err
		}
	}
}

// handle 处理一帧请求，返回应答 body（不含标志位）。
func (s *Server) handle(fr dl.Frame) ([]byte, bool) {
	s.mu.Lock()
	s.requests++
	silent := s.silent
	clientAddr := s.clientAddr
	serverAddr := s.serverAddr
	addrSize := s.addrSize
	s.mu.Unlock()

	if silent {
		return nil, true
	}
	// 地址过滤：dest 必须是我，src 必须是配置的客户端。不匹配就静默——
	// 替别人的地址应答是总线串扰。
	wantDest := dl.ServerAddress(serverAddr, 0, addrSize)
	if fr.Dest.Upper != wantDest.Upper || fr.Dest.Size != wantDest.Size {
		return nil, true
	}
	if fr.Src.Upper != clientAddr || fr.Src.Size != 1 {
		return nil, true
	}

	// 应答帧：dest/src 对调，LLC 头用 LLCResponse。
	reply := func(kind dl.FrameKind, ctl dl.Control, info []byte) []byte {
		s.mu.Lock()
		if kind == dl.KindI {
			ctl.NS = int(s.ns)
			ctl.NR = int(s.nr)
			s.ns = (s.ns + 1) & 0x07
		}
		s.mu.Unlock()
		out := dl.Frame{
			Dest:    dl.ClientAddress(clientAddr),
			Src:     dl.ServerAddress(serverAddr, 0, addrSize),
			Control: ctl,
		}
		if kind == dl.KindI && len(info) > 0 {
			out.Info = append([]byte(dl.LLCResponse), info...)
		}
		b, err := out.Encode()
		if err != nil {
			return nil
		}
		return b
	}

	switch fr.Kind() {
	case dl.KindSNRM:
		s.mu.Lock()
		s.snrmCount++
		s.ns, s.nr = 0, 0
		s.associated = false
		s.mu.Unlock()
		return reply(dl.KindUA, dl.Control{Kind: dl.KindUA, PF: true}, nil), false

	case dl.KindDISC:
		s.mu.Lock()
		s.discCount++
		s.associated = false
		s.mu.Unlock()
		return reply(dl.KindUA, dl.Control{Kind: dl.KindUA, PF: true}, nil), false

	case dl.KindI:
		// 收到 I 帧：更新 NR。
		s.mu.Lock()
		s.nr = (uint8(fr.Control.NS) + 1) & 0x07
		s.mu.Unlock()
		apdu, ok := fr.APDU()
		if !ok || len(apdu) == 0 {
			return nil, true
		}
		respAPDU := s.handleAPDU(apdu)
		if respAPDU == nil {
			return nil, true
		}
		body := reply(dl.KindI, dl.Control{Kind: dl.KindI, PF: true}, respAPDU)
		if s.badChecksum {
			// 改坏末尾 FCS 的前一字节，而不是改内容：这是总线上真实会
			// 发生的形态，且主站必须靠校验序列（而不是内容）发现它。
			if len(body) >= 3 {
				body[len(body)-2] ^= 0xFF
			}
		}
		return body, false

	case dl.KindRR:
		// 主站确认收到，无需应答。
		s.mu.Lock()
		s.nr = uint8(fr.Control.NR)
		s.mu.Unlock()
		return nil, true
	}
	return nil, true
}

// handleAPDU 分派到 AARQ / Get。
func (s *Server) handleAPDU(apdu []byte) []byte {
	switch apdu[0] {
	case dl.TagAARQ:
		s.mu.Lock()
		s.aarqCount++
		reject := s.rejectAssociation
		s.mu.Unlock()
		return s.handleAARQ(apdu, reject)
	case dl.TagGetRequestNormal:
		s.mu.Lock()
		s.getCount++
		assoc := s.associated
		s.mu.Unlock()
		if !assoc {
			// 未建关联就读：回 read-write-denied。真实表在多数情况下直接
			// 回 DM 或不回，但回一个明确的结果码对自检更有价值。
			return dl.EncodeGetResponseResult(dl.DARReadWriteDenied)
		}
		return s.handleGet(apdu)
	case dl.TagGetRequestNext, dl.TagGetRequestWithList:
		return dl.EncodeGetResponseResult(dl.DARDataBlockUnavailable)
	}
	return nil
}

// handleAARQ 回应 AARQ。
func (s *Server) handleAARQ(apdu []byte, reject bool) []byte {
	// 只做最基本的合法性检查：OSI 层标签是不是 AARQ。真正的字段解析在
	// 主站侧；从站只需要区分"能接"与"不能接"。
	if reject {
		return buildAARE(false, 1, 0, 0x0200)
	}
	s.mu.Lock()
	s.associated = true
	s.mu.Unlock()
	return buildAARE(true, 0, 0, 0x0200)
}

// buildAARE 组 AARE APDU。
//
// 参数 accepted 决定 result；diagnostic 只在拒绝时有意义。
func buildAARE(accepted bool, result, diag int, maxPDU uint16) []byte {
	ctx := dl.OIDLNNoSecurity
	// application-context-name [1] { OID }
	ctxField := append([]byte{dl.TagCtxName, byte(2 + len(ctx)), 0x06, byte(len(ctx))}, ctx...)
	// result [2] { INTEGER }
	resField := []byte{dl.TagResult, 0x03, 0x02, 0x01, byte(result)}
	// result-source-diagnostic [3] { acse-service-user { INTEGER } }
	diagField := []byte{dl.TagResultDiag, 0x05, 0xA1, 0x03, 0x02, 0x01, byte(diag)}

	// InitiateResponse: 08 00 06 5F 1F 04 00 <conf 3> <maxPDU 2> <VAA 2>
	var conf dl.Conformance
	if accepted {
		// 协商结果：只给 GET 与 attribute0-get（与驱动请求的一致），
		// 再加上 block-transfer-get 以便说明"我们不支持分块"时能从
		// conformance 看出表本来支持。
		conf = dl.ConfGet | dl.ConfAttribute0Get
	}
	init := []byte{dl.TagInitResp, 0x00, 0x06, dl.TagConformance, dl.TagConformance2, 0x04, 0x00}
	init = append(init, conf.Bytes()...)
	init = append(init, byte(maxPDU>>8), byte(maxPDU), 0x00, 0x07)

	userInfo := append([]byte{dl.TagUSerInfo, byte(2 + len(init)), 0x04, byte(len(init))}, init...)

	body := append(ctxField, resField...)
	body = append(body, diagField...)
	body = append(body, userInfo...)
	return append([]byte{dl.TagAARE, byte(len(body))}, body...)
}

// handleGet 处理 Get-Request-Normal。
func (s *Server) handleGet(apdu []byte) []byte {
	if len(apdu) < 3 {
		return dl.EncodeGetResponseResult(dl.DARTypeUnmatched)
	}
	ln := int(apdu[1])
	if len(apdu) < 2+ln {
		return dl.EncodeGetResponseResult(dl.DARTypeUnmatched)
	}
	body := apdu[2 : 2+ln]
	// body = invoke(1) + descriptor(9)
	if len(body) < 10 {
		return dl.EncodeGetResponseResult(dl.DARTypeUnmatched)
	}
	desc, _, err := dl.DecodeAttributeDescriptor(body[1:])
	if err != nil {
		return dl.EncodeGetResponseResult(dl.DARTypeUnmatched)
	}
	var obis dl.OBIS
	copy(obis[:], desc.InstanceID[:])

	s.mu.RLock()
	obj, ok := s.objects[objKey(desc.ClassID, obis, desc.Attribute)]
	s.mu.RUnlock()
	if !ok {
		// 类号错与 OBIS 错在语义上是不同的结果码：先看"这个 OBIS 有没有
		// 别的类"，有则说明是类号不一致，否则是对象根本没定义。
		s.mu.RLock()
		anyClass := false
		for k, o := range s.objects {
			if o.OBIS == obis && o.Attribute == desc.Attribute {
				_ = k
				anyClass = true
				break
			}
		}
		s.mu.RUnlock()
		if anyClass {
			return dl.EncodeGetResponseResult(dl.DARObjectClassInconsistent)
		}
		return dl.EncodeGetResponseResult(dl.DARObjectUndefined)
	}
	apduOut, err := dl.EncodeGetResponseNormal(obj.Value)
	if err != nil {
		return dl.EncodeGetResponseResult(dl.DARHardwareFault)
	}
	return apduOut
}

// ---------------------------------------------------------------------------
// TCP 流适配
// ---------------------------------------------------------------------------

type tcpStream struct{ conn net.Conn }

func (t *tcpStream) ReadFull(n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(t.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (t *tcpStream) Write(p []byte) error {
	_, err := t.conn.Write(p)
	return err
}

func (t *tcpStream) Close() error { return t.conn.Close() }

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// sortObjects 只用于稳定输出（自检命令按 OBIS 排序打印）。
func sortObjects(objs []Object) []Object {
	out := append([]Object(nil), objs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.OBIS != b.OBIS {
			return a.OBIS.String() < b.OBIS.String()
		}
		if a.ClassID != b.ClassID {
			return a.ClassID < b.ClassID
		}
		return a.Attribute < b.Attribute
	})
	return out
}

// Clock 返回从站的"当前时间"，供测试构造期望值。
func (s *Server) Clock() time.Time { return time.Now().UTC() }
