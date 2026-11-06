// Package modbus 实现 Modbus TCP 从站（服务端）：把已经采集到的点位
// 以寄存器/线圈的形式暴露给外部主站（SCADA、DCS、第三方采集器）。
//
// 与「再写一个协议主站」相比，从站服务有三个不同的问题域，代码结构也
// 因此不同：
//
//  1. **数据是借来的**。值不来自链路，而来自 cache。因此这里只做
//     「映射 → 取采样 → 编码」，不做采集调度。
//  2. **请求是对端的，不是自己的**。主站可能读任意区间、任意长度，
//     所以必须处理「区间跨越多个映射」「读半个 32 位值」「读到没有
//     映射的空洞」这些情况，而不能像主站那样自己决定报文边界。
//  3. **写要穿透到南向**。外部主站写寄存器不是写本地缓存，而是要
//     真的下发到 PLC；这条路径必须复用北向写入的命令白名单与
//     设备 actor，否则就有了第二个绕过治理的写入口。
//
// 边界：支持 Modbus TCP（MBAP）与串口 RTU/ASCII 从站；不支持多主站
// 共享同一条串口（一条物理线只有一个从站实例）、不支持文件记录
// （FC14/15/20/21）。诊断与识别类功能码（FC8 / FC11 / FC17 / FC43）已实现
// ——它们不碰映射表，但决定上位机能否把这个从站识别成「一台设备」而不是
// 「一堆寄存器」。
package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mb "uic/internal/codec/modbus"
	"uic/internal/codec/value"
	"uic/internal/model"
	"uic/internal/obs"
	"uic/internal/transport"
)

// Source 提供映射点位的当前值。实现者必须并发安全。
type Source interface {
	Sample(device, point string) (model.Sample, bool)
}

// WriteRequest 是一次要下发到南向设备的写入。
type WriteRequest struct {
	Device string
	Point  string
	Value  model.Value
}

// WriteResult 是单次写入的结果。Err 为 nil 表示设备已确认。
type WriteResult struct {
	OK  bool
	Err error
}

// Writer 把外部主站的写入转发到南向设备。
//
// 实现者必须走与北向 REST 写入**同一条**路径（设备 actor、可写校验、
// 命令白名单），不允许直接操作驱动连接：否则从站就成了一个绕过治理
// 的写入口，而这正是现场最难审计的漏洞。
type Writer interface {
	Write(ctx context.Context, reqs []WriteRequest) []WriteResult
}

// Options 是创建从站的参数。
type Options struct {
	Slave  model.Slave
	Points map[string]model.Point // 键为 "device/point"
	Source Source
	Writer Writer

	// Revision 是 FC43 上报的固件/软件版本。空值由 New 填 "1.0"；
	// runtime 会把构建版本传进来，让上位机屏幕上的版本与进程一致。
	Revision string

	// WriteTimeout 是单次南向写入的总预算，默认 5s。
	WriteTimeout time.Duration
	// IdleTimeout 是空闲连接回收时间，默认 5 分钟（仅 TCP）。
	IdleTimeout time.Duration
	Log         *slog.Logger
	Metrics     *obs.Metrics

	// SerialOpener 打开串口，默认走 transport.OpenSerial。
	// 仅供测试注入：生产路径不存在第二种打开方式。
	SerialOpener func(transport.SerialConfig) (serialPort, error)
}

// Server 是一个 Modbus 从站实例。TCP 与串口（RTU/ASCII）二选一。
type Server struct {
	id         string
	addr       string
	ln         net.Listener
	st         serialPort
	framing    string
	serialCfg  transport.SerialConfig
	gap        time.Duration
	openSerial func(transport.SerialConfig) (serialPort, error)
	units      map[int]*unit
	src        Source
	writer     Writer
	writeTo    time.Duration
	idle       time.Duration
	log        *slog.Logger
	met        *obs.Metrics
	slots      chan struct{}
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	closed     bool
	wg         sync.WaitGroup
	ident      identity
	requests   atomic.Uint64
	events     atomic.Uint64
	exceptions atomic.Uint64
	degraded   atomic.Uint64
	writes     atomic.Uint64
	writeErrs  atomic.Uint64
	badFrames  atomic.Uint64
	crcErrors  atomic.Uint64
}

// unit 是一个 Unit ID 的映射表。cells 按区拆分并排序，查找走二分。
type unit struct {
	id    int
	order model.ByteOrder
	cells map[model.MappingArea][]cell
}

// cell 是一个点位占用的连续地址段。
type cell struct {
	start    int
	width    int // 寄存器个数；位区恒为 1
	device   string
	point    string
	dt       model.DataType
	writable bool
}

func (c cell) end() int { return c.start + c.width }

// New 校验配置、绑定端点并开始服务。
//
// 绑定在构造期完成而不是 Start()：端口被占用（串口被占用）是**启动期
// 错误**，应该让进程带着明确的日志退出，而不是启动成功后在后台反复重试、
// 让运维以为从站已经可用。
func New(opt Options) (*Server, error) {
	if opt.Source == nil {
		return nil, errors.New("slave/modbus: source required")
	}
	if opt.Writer == nil {
		return nil, errors.New("slave/modbus: writer required")
	}
	if err := opt.Slave.Validate(opt.Points); err != nil {
		return nil, err
	}
	units, err := buildUnits(opt.Slave, opt.Points)
	if err != nil {
		return nil, err
	}
	s := &Server{
		id:         opt.Slave.ID,
		ident:      newIdentity(opt.Slave.Identity, opt.Revision),
		units:      units,
		src:        opt.Source,
		writer:     opt.Writer,
		writeTo:    opt.WriteTimeout,
		idle:       opt.IdleTimeout,
		log:        opt.Log,
		met:        opt.Metrics,
		slots:      make(chan struct{}, opt.Slave.EffectiveMaxConnections()),
		conns:      map[net.Conn]struct{}{},
		openSerial: opt.SerialOpener,
	}
	if s.writeTo <= 0 {
		s.writeTo = 5 * time.Second
	}
	if s.idle <= 0 {
		s.idle = 5 * time.Minute
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.openSerial == nil {
		s.openSerial = openSerial
	}

	if sl := opt.Slave.Serial; sl != nil {
		n := sl.Normalized()
		s.serialCfg = transport.SerialConfig{
			Port: n.Port, Baud: n.Baud, DataBits: n.DataBits,
			Parity: n.Parity, StopBits: n.StopBits,
		}
		s.framing = n.Framing
		s.gap = frameGap(n)
		s.addr = n.Port
		st, err := s.openSerial(s.serialCfg)
		if err != nil {
			return nil, fmt.Errorf("slave %q: open serial %s: %w", opt.Slave.ID, n.Port, err)
		}
		s.st = st
		s.wg.Add(1)
		go s.serveSerial()
		return s, nil
	}

	ln, err := net.Listen("tcp", opt.Slave.Listen)
	if err != nil {
		return nil, fmt.Errorf("slave %q: listen %s: %w", opt.Slave.ID, opt.Slave.Listen, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// ID 返回从站标识。
func (s *Server) ID() string { return s.id }

// Addr 返回实际监听地址。TCP 配置为 :0 时返回内核分配的端口；
// 串口返回设备路径（如 /dev/ttyUSB0）。
func (s *Server) Addr() string { return s.addr }

// Stats 返回累计计数，供指标与测试使用。
type Stats struct {
	Requests   uint64
	Exceptions uint64
	Degraded   uint64
	Writes     uint64
	WriteErrs  uint64
	BadFrames  uint64
}

// Stats 返回当前累计计数。
func (s *Server) Stats() Stats {
	return Stats{
		Requests:   s.requests.Load(),
		Exceptions: s.exceptions.Load(),
		Degraded:   s.degraded.Load(),
		Writes:     s.writes.Load(),
		WriteErrs:  s.writeErrs.Load(),
		BadFrames:  s.badFrames.Load(),
	}
}

// Close 停止监听并关闭所有连接。可重复调用。
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
	ln, st := s.ln, s.st
	s.mu.Unlock()

	var err error
	if ln != nil {
		if e := ln.Close(); e != nil {
			err = e
		}
	}
	if st != nil {
		// 关串口会让阻塞中的 Read 立刻返回，串口循环因此能退出。
		_ = st.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// isClosed 供串口循环判断是否该退出。
func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func buildUnits(sl model.Slave, points map[string]model.Point) (map[int]*unit, error) {
	out := make(map[int]*unit, len(sl.Units))
	for _, su := range sl.Units {
		u := &unit{id: su.UnitID, order: su.ByteOrder, cells: map[model.MappingArea][]cell{}}
		for _, m := range su.Mappings {
			dev, pt, _ := strings.Cut(m.Point, "/")
			p := points[m.Point]
			width := 1
			if !m.Area.IsBits() {
				width = p.RegisterWidth()
			}
			u.cells[m.Area] = append(u.cells[m.Area], cell{
				start: m.Offset, width: width,
				device: dev, point: pt, dt: p.DataType, writable: m.Writable,
			})
		}
		for area, list := range u.cells {
			sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
			u.cells[area] = list
		}
		out[u.id] = u
	}
	return out, nil
}

// find 返回覆盖 addr 的 cell；没有映射时返回 false。
func (u *unit) find(area model.MappingArea, addr int) (cell, bool) {
	list := u.cells[area]
	i := sort.Search(len(list), func(i int) bool { return list[i].end() > addr })
	if i == len(list) || list[i].start > addr {
		return cell{}, false
	}
	return list[i], true
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			// 超过连接上限：立刻回绝而不是排队。Modbus 主站超时后
			// 会重试，排队只会把「过载」伪装成「网络慢」。
			_ = c.Close()
			s.log.Warn("slave_conn_rejected", "slave", s.id, "reason", "max_connections")
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			<-s.slots
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				_ = c.Close()
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				<-s.slots
			}()
			s.serve(c)
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	head := make([]byte, 7)
	for {
		_ = c.SetReadDeadline(time.Now().Add(s.idle))
		if _, err := io.ReadFull(c, head); err != nil {
			return
		}
		if binary.BigEndian.Uint16(head[2:4]) != 0 {
			return
		}
		length := int(binary.BigEndian.Uint16(head[4:6]))
		// length 含 unitId；PDU 上限 253 字节（FC16 写 125 个寄存器即 256）。
		if length < 2 || length > 254 {
			return
		}
		pdu := make([]byte, length-1)
		if _, err := io.ReadFull(c, pdu); err != nil {
			return
		}
		tx := binary.BigEndian.Uint16(head[0:2])
		resp := s.respond(int(head[6]), pdu)

		out := mb.BuildTCP(mb.Frame{Transaction: tx, Unit: head[6], PDU: resp})
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// respond 处理一个已解析的请求：查单元、生成响应、更新计数与指标。
//
// 未知单元的响应码是 0x0B（网关目标失败）而不是静默：TCP 上主站能收到
// 确定的错误码，比等 3 秒超时再去查网络要好。串口不同——一条总线上有
// 多个从站，替别人的地址应答就是串扰，所以串口路径在调用前就把非本机
// 单元过滤掉了。
func (s *Server) respond(unitID int, pdu []byte) []byte {
	s.requests.Add(1)
	u, ok := s.units[unitID]
	var resp []byte
	if !ok {
		resp = mb.ExceptionPDU(pdu[0], 0x0B)
	} else {
		resp = s.handle(u, pdu)
	}
	if len(resp) >= 1 && resp[0]&0x80 != 0 {
		s.exceptions.Add(1)
		s.noteRequest(unitID, pdu[0], "exception")
	} else {
		s.noteRequest(unitID, pdu[0], "ok")
	}
	// FC11 自己不计数：主站读事件计数器时，读到的是「这条读之前」
	// 已经完成的消息数，否则每次查询都会让计数 +1，曲线变成查询频率。
	if len(resp) > 0 && resp[0]&0x80 == 0 && pdu[0] != mb.FC11 {
		s.events.Add(1)
	}
	return resp
}

func (s *Server) noteRequest(unitID int, fc byte, result string) {
	if s.met == nil {
		return
	}
	s.met.Inc("uic_slave_requests_total", obs.Labels{
		"slave": s.id, "unit": itoa(unitID),
		"fc": itoa(int(fc)), "result": result,
	})
}

func (s *Server) handle(u *unit, pdu []byte) []byte {
	if len(pdu) == 0 {
		return mb.ExceptionPDU(0, 0x01)
	}
	// 诊断与识别类请求的 PDU 形状与读写不同，必须先分派再解析。
	switch pdu[0] {
	case mb.FC08:
		return s.diagnostics(pdu)
	case mb.FC11:
		if len(pdu) != 1 {
			return mb.ExceptionPDU(mb.FC11, 0x03)
		}
		return s.commEventCounter()
	case mb.FC17:
		if len(pdu) != 1 {
			return mb.ExceptionPDU(mb.FC17, 0x03)
		}
		return s.reportServerID()
	case mb.FC43:
		return s.readDeviceID(pdu)
	}
	fc, addr, qty, values, err := mb.ParseRequestPDU(pdu)
	if err != nil {
		code := byte(0x01) // 非法功能码
		if len(pdu) > 0 {
			switch pdu[0] {
			case mb.FCReadCoils, mb.FCReadDiscreteInputs, mb.FCReadHoldingRegs,
				mb.FCReadInputRegs, mb.FCWriteSingleCoil, mb.FCWriteSingleRegister,
				mb.FCWriteMultipleCoils, mb.FCWriteMultipleRegs:
				code = 0x03 // 功能码认识，但报文长度不对
			}
			return mb.ExceptionPDU(pdu[0], code)
		}
		return mb.ExceptionPDU(0, code)
	}
	switch fc {
	case mb.FCReadHoldingRegs:
		return s.readRegs(u, fc, model.MappingHolding, addr, qty)
	case mb.FCReadInputRegs:
		return s.readRegs(u, fc, model.MappingInput, addr, qty)
	case mb.FCReadCoils:
		return s.readBits(u, fc, model.MappingCoil, addr, qty)
	case mb.FCReadDiscreteInputs:
		return s.readBits(u, fc, model.MappingDiscrete, addr, qty)
	case mb.FCWriteSingleCoil:
		return s.writeSingleCoil(u, fc, addr, values)
	case mb.FCWriteSingleRegister:
		return s.writeSingleRegister(u, fc, addr, values)
	case mb.FCWriteMultipleCoils:
		return s.writeMultipleCoils(u, fc, addr, qty, values)
	case mb.FCWriteMultipleRegs:
		return s.writeMultipleRegs(u, fc, addr, qty, values)
	}
	return mb.ExceptionPDU(fc, 0x01)
}

// ---------------------------------------------------------------------------
// 诊断与设备识别（FC8 / FC11 / FC17 / FC43）
// ---------------------------------------------------------------------------
//
// 这四类功能码不读映射表，却决定上位机能否把这个从站当成「一台设备」：
// 没有它们，SCADA 的设备树里只能看到一堆无名的寄存器；有它们，才能显示
// 厂商、型号、版本，并在设备替换时自动匹配点表。

// FC43 的对象 id（Modbus Application Protocol §6.21）。
const (
	idVendorName  byte = 0x00
	idProductCode byte = 0x01
	idRevision    byte = 0x02
	idVendorURL   byte = 0x03
	idProductName byte = 0x04
	idModelName   byte = 0x05
	idAppName     byte = 0x06

	meiReadDeviceID byte = 0x0E
)

// identity 是填好默认值的设备标识，FC17 与 FC43 共用。
type identity struct {
	vendor    string
	product   string
	revision  string
	vendorURL string
	name      string
	model     string
	app       string
}

// newIdentity 用配置覆盖默认值。默认值不是占位符：没配 identity 的从站也
// 必须能被识别——FC43 的 0x00/0x01/0x02 三个对象是 basic 级别必选。
func newIdentity(cfg *model.SlaveIdentity, revision string) identity {
	if revision == "" {
		revision = "1.0"
	}
	id := identity{
		vendor:   "UIC",
		product:  "uic-slave",
		revision: revision,
		name:     "UIC Modbus Slave",
		model:    "uic",
		app:      "UIC",
	}
	if cfg == nil {
		return id
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&id.vendor, cfg.VendorName)
	set(&id.product, cfg.ProductCode)
	set(&id.revision, cfg.MajorMinorRevision)
	set(&id.vendorURL, cfg.VendorURL)
	set(&id.name, cfg.ProductName)
	set(&id.model, cfg.ModelName)
	set(&id.app, cfg.UserApplicationName)
	return id
}

// idObject 是 FC43 响应里的一个对象。
type idObject struct {
	id    byte
	value string
}

// conformityExtended 是本从站的符合级别：extended + stream & individual
// access（0x83）。
//
// 按规范，Conformity Level 描述的是**设备能力**，不是「这次返回了哪些
// 对象」——不能因为主站请求 basic 就回报 0x01。否则主站会认为该设备不
// 支持按对象 id 单独读取，明明实现的能力反而被藏起来。
const conformityExtended byte = 0x83

// list 返回全部非空对象（按 id 升序）。空值对象直接省略：返回一个长度
// 为 0 的对象等于让上位机显示一个空字段。
func (i identity) list() []idObject {
	all := []idObject{
		{idVendorName, i.vendor},
		{idProductCode, i.product},
		{idRevision, i.revision},
		{idVendorURL, i.vendorURL},
		{idProductName, i.name},
		{idModelName, i.model},
		{idAppName, i.app},
	}
	out := make([]idObject, 0, len(all))
	for _, o := range all {
		if o.value != "" {
			out = append(out, o)
		}
	}
	return out
}

// objects 返回某个访问级别（1=basic / 2=regular / 3=extended）的对象列表。
func (i identity) objects(code byte) []idObject {
	maxID := idAppName
	switch code {
	case 1:
		maxID = idRevision
	case 2:
		maxID = idModelName
	}
	out := make([]idObject, 0, 7)
	for _, o := range i.list() {
		if o.id <= maxID {
			out = append(out, o)
		}
	}
	return out
}

// objectByID 供「按对象 id 读」（code=4）使用。
func (i identity) objectByID(id byte) (idObject, bool) {
	for _, o := range i.list() {
		if o.id == id {
			return o, true
		}
	}
	return idObject{}, false
}

// diagnostics 处理 FC8。子功能返回的计数一律截断到 16 位：协议字段就是
// 两个字节，假装能表示 64 位只会让上位机看到一个错误的数字。
func (s *Server) diagnostics(pdu []byte) []byte {
	fc := mb.FC08
	if len(pdu) != 5 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	sub := binary.BigEndian.Uint16(pdu[1:3])
	data := binary.BigEndian.Uint16(pdu[3:5])
	var out uint16
	switch sub {
	case 0x0000: // Return Query Data：原样回显
		out = data
	case 0x000A: // Clear Counters：只清协议计数，不动指标（指标是累计量）
		s.requests.Store(0)
		s.exceptions.Store(0)
		s.events.Store(0)
		s.crcErrors.Store(0)
		out = 0
	case 0x000B: // Bus Message Count
		out = uint16(s.requests.Load())
	case 0x000C: // Bus Communication Error Count
		// TCP 上恒为 0：没有 CRC/字符层；串口上如实报告 CRC/LRC 错误数，
		// 否则这个「诊断」功能码在唯一有意义的总线上什么也没诊断。
		if s.framing != "" {
			out = uint16(s.crcErrors.Load())
		}
	case 0x000D: // Bus Exception Error Count
		out = uint16(s.exceptions.Load())
	case 0x000E: // Server Message Count
		out = uint16(s.requests.Load())
	case 0x000F: // Server No Response Count：TCP 从站总是应答
		out = 0
	case 0x0010: // Server NAK Count：本实现不产生 NAK
		out = 0
	case 0x0011: // Server Busy Count：本实现不返回 0x06
		out = 0
	case 0x0012: // Bus Character Overrun Count：字节流由内核分帧，无此概念
		out = 0
	default:
		return mb.ExceptionPDU(fc, 0x01)
	}
	resp := []byte{fc, pdu[1], pdu[2], 0, 0}
	binary.BigEndian.PutUint16(resp[3:5], out)
	return resp
}

// commEventCounter 处理 FC11：status 恒为 0（无未处理事件），count 是成功
// 处理过的消息数。计数在 serve 里递增，FC11 自己不计（否则变成查询频率）。
func (s *Server) commEventCounter() []byte {
	resp := make([]byte, 5)
	resp[0] = mb.FC11
	binary.BigEndian.PutUint16(resp[3:5], uint16(s.events.Load()))
	return resp
}

// reportServerID 处理 FC17：产品代码 + 运行指示（0xFF = ON）。
func (s *Server) reportServerID() []byte {
	data := append([]byte(s.ident.product), 0xFF)
	resp := make([]byte, 0, 2+len(data))
	resp = append(resp, mb.FC17, byte(len(data)))
	return append(resp, data...)
}

// readDeviceID 处理 FC43 / MEI 0x0E。
//
// 响应必须放进 253 字节 PDU，所以对象要分页：放不下时 more=0xFF，
// nextObjectID 指向下一页的起点。上位机按这个指针续读，不需要自己猜。
func (s *Server) readDeviceID(pdu []byte) []byte {
	fc := mb.FC43
	if len(pdu) < 4 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	if pdu[1] != meiReadDeviceID {
		// 只实现 MEI 0x0E；CANopen 等其他封装接口返回非法功能码。
		return mb.ExceptionPDU(fc, 0x01)
	}
	code := pdu[2]
	start := pdu[3]

	var objs []idObject
	switch code {
	case 1, 2, 3:
		objs = s.ident.objects(code)
	case 4:
		o, ok := s.ident.objectByID(start)
		if !ok {
			return mb.ExceptionPDU(fc, 0x02)
		}
		objs = []idObject{o}
	default:
		return mb.ExceptionPDU(fc, 0x03)
	}

	const maxPayload = 240 // 253 - 7 字节头，留一点余量
	payload := make([]byte, 0, 96)
	var next, more byte
	count := 0
	for _, o := range objs {
		if o.id < start {
			continue
		}
		need := 2 + len(o.value)
		if len(payload)+need > maxPayload {
			more = 0xFF
			next = o.id
			break
		}
		payload = append(payload, o.id, byte(len(o.value)))
		payload = append(payload, o.value...)
		count++
	}
	if count == 0 {
		return mb.ExceptionPDU(fc, 0x02)
	}
	resp := make([]byte, 0, 7+len(payload))
	resp = append(resp, fc, meiReadDeviceID, code, conformityExtended, more, next, byte(count))
	return append(resp, payload...)
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func (s *Server) readRegs(u *unit, fc byte, area model.MappingArea, addr, qty int) []byte {
	if qty < 1 || qty > 125 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	if addr < 0 || addr+qty > 65536 {
		return mb.ExceptionPDU(fc, 0x02)
	}
	out := make([]byte, qty*2)
	end := addr + qty
	for pos := addr; pos < end; {
		c, ok := u.find(area, pos)
		if !ok {
			// 空洞按异常处理而不是补零：补零会让主站把一个「没配映射」
			// 当成真实值 0，这类错误在现场只会被当成工艺数据。
			return mb.ExceptionPDU(fc, 0x02)
		}
		raw, degraded, err := s.encodeReg(u, c)
		if err != nil {
			s.log.Warn("slave_read_failed", "slave", s.id, "unit", u.id,
				"point", c.device+"/"+c.point, "error", err.Error())
			return mb.ExceptionPDU(fc, 0x04)
		}
		s.noteDegraded(degraded)
		// 允许主站读半个 32 位值：协议没禁止，拒绝反而会让一些
		// 「先读 1 个寄存器试探」的主站直接失败。
		stop := c.end()
		if stop > end {
			stop = end
		}
		copy(out[(pos-addr)*2:], raw[(pos-c.start)*2:(stop-c.start)*2])
		pos = stop
	}
	return append([]byte{fc, byte(len(out))}, out...)
}

func (s *Server) readBits(u *unit, fc byte, area model.MappingArea, addr, qty int) []byte {
	if qty < 1 || qty > 2000 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	if addr < 0 || addr+qty > 65536 {
		return mb.ExceptionPDU(fc, 0x02)
	}
	n := (qty + 7) / 8
	out := make([]byte, 2+n)
	out[0] = fc
	out[1] = byte(n)
	for i := 0; i < qty; i++ {
		c, ok := u.find(area, addr+i)
		if !ok {
			return mb.ExceptionPDU(fc, 0x02)
		}
		b, degraded, err := s.encodeBit(c)
		if err != nil {
			s.log.Warn("slave_read_failed", "slave", s.id, "unit", u.id,
				"point", c.device+"/"+c.point, "error", err.Error())
			return mb.ExceptionPDU(fc, 0x04)
		}
		s.noteDegraded(degraded)
		if b != 0 {
			out[2+i/8] |= 1 << uint(i%8)
		}
	}
	return out
}

// sample 取出映射点位当前值。
//
// 还没采到值（网关刚启动、设备尚未完成首次采集）时返回错误，让本次请求
// 以 0x04 失败。用 0 顶替会让主站把「没数据」记成「值为 0」，而这两者
// 在工艺上完全不同。
func (s *Server) sample(c cell) (model.Sample, bool, error) {
	sample, ok := s.src.Sample(c.device, c.point)
	if !ok {
		return model.Sample{}, false, fmt.Errorf("no sample for %s/%s yet", c.device, c.point)
	}
	return sample, sample.Quality == model.QualityGood, nil
}

// noteDegraded 记录一次「用旧值应答」。Modbus 没有质量位，坏质量只能靠
// 指标暴露；这也是文档里要求消费方同时读网关 REST 接口的原因。
func (s *Server) noteDegraded(degraded bool) {
	if !degraded {
		return
	}
	s.degraded.Add(1)
	if s.met != nil {
		s.met.Inc("uic_slave_degraded_reads_total", obs.Labels{"slave": s.id})
	}
}

// encodeReg 把当前采样编码成寄存器区的线上字节（宽度 = cell.width * 2）。
func (s *Server) encodeReg(u *unit, c cell) ([]byte, bool, error) {
	sample, good, err := s.sample(c)
	if err != nil {
		return nil, false, err
	}
	width := c.width * 2
	out := make([]byte, width)
	switch c.dt {
	case model.TypeBool:
		if boolByte(sample.Value) != 0 {
			out[width-1] = 1
		}
	case model.TypeString:
		raw := []byte(sample.Value.Text)
		if len(raw) > width {
			raw = raw[:width]
		}
		copy(out, raw)
	case model.TypeBytes:
		copy(out, []byte(sample.Value.Text))
	default:
		raw, err := value.Encode(sample.Value, c.dt, u.order)
		if err != nil {
			return nil, good, err
		}
		if len(raw) != width {
			return nil, good, fmt.Errorf("encoded %d bytes for %s, mapping width is %d",
				len(raw), c.dt, width)
		}
		copy(out, raw)
	}
	return out, !good, nil
}

// encodeBit 把当前采样编码成线圈值。
func (s *Server) encodeBit(c cell) (byte, bool, error) {
	sample, good, err := s.sample(c)
	if err != nil {
		return 0, false, err
	}
	return boolByte(sample.Value), !good, nil
}

func boolByte(v model.Value) byte {
	if v.Type == model.TypeBool {
		if v.Bool {
			return 1
		}
		return 0
	}
	if v.Num != 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (s *Server) writeSingleCoil(u *unit, fc byte, addr int, raw []byte) []byte {
	if len(raw) < 2 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	v := binary.BigEndian.Uint16(raw)
	// 协议只允许 0xFF00 / 0x0000；其它值视为非法数据值。
	if v != 0xFF00 && v != 0x0000 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	c, ok := u.find(model.MappingCoil, addr)
	if !ok || !c.writable {
		return mb.ExceptionPDU(fc, 0x02)
	}
	if !s.forward(fc, []WriteRequest{{Device: c.device, Point: c.point, Value: model.BoolValue(v == 0xFF00)}}) {
		return mb.ExceptionPDU(fc, 0x04)
	}
	return []byte{fc, byte(addr >> 8), byte(addr), byte(v >> 8), byte(v)}
}

func (s *Server) writeSingleRegister(u *unit, fc byte, addr int, raw []byte) []byte {
	if len(raw) < 2 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	c, ok := u.find(model.MappingHolding, addr)
	if !ok || !c.writable {
		return mb.ExceptionPDU(fc, 0x02)
	}
	// 单寄存器写只能写 1 个寄存器宽的点位。对 32 位点位写一半会让
	// 设备收到一个撕裂值，而这种写多半是主站配置错误——直接拒绝。
	if c.width != 1 {
		return mb.ExceptionPDU(fc, 0x02)
	}
	val, err := decodeCellValue(c, raw[:2], u.order)
	if err != nil {
		return mb.ExceptionPDU(fc, 0x03)
	}
	if !s.forward(fc, []WriteRequest{{Device: c.device, Point: c.point, Value: val}}) {
		return mb.ExceptionPDU(fc, 0x04)
	}
	return []byte{fc, byte(addr >> 8), byte(addr), raw[0], raw[1]}
}

func (s *Server) writeMultipleCoils(u *unit, fc byte, addr, qty int, raw []byte) []byte {
	if qty < 1 || qty > 1968 || len(raw) != (qty+7)/8 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	reqs := make([]WriteRequest, 0, qty)
	for i := 0; i < qty; i++ {
		c, ok := u.find(model.MappingCoil, addr+i)
		if !ok || !c.writable || c.width != 1 {
			return mb.ExceptionPDU(fc, 0x02)
		}
		on := raw[i/8]&(1<<uint(i%8)) != 0
		reqs = append(reqs, WriteRequest{Device: c.device, Point: c.point, Value: model.BoolValue(on)})
	}
	if !s.forward(fc, reqs) {
		return mb.ExceptionPDU(fc, 0x04)
	}
	return []byte{fc, byte(addr >> 8), byte(addr), byte(qty >> 8), byte(qty)}
}

func (s *Server) writeMultipleRegs(u *unit, fc byte, addr, qty int, raw []byte) []byte {
	if qty < 1 || qty > 123 || len(raw) != qty*2 {
		return mb.ExceptionPDU(fc, 0x03)
	}
	if addr < 0 || addr+qty > 65536 {
		return mb.ExceptionPDU(fc, 0x02)
	}
	end := addr + qty
	reqs := make([]WriteRequest, 0, 4)
	for pos := addr; pos < end; {
		c, ok := u.find(model.MappingHolding, pos)
		if !ok || !c.writable {
			return mb.ExceptionPDU(fc, 0x02)
		}
		// 多寄存器写必须覆盖完整点位：半个 float 写下去毫无意义。
		if c.start < addr || c.end() > end {
			return mb.ExceptionPDU(fc, 0x02)
		}
		val, err := decodeCellValue(c, raw[(c.start-addr)*2:(c.end()-addr)*2], u.order)
		if err != nil {
			return mb.ExceptionPDU(fc, 0x03)
		}
		reqs = append(reqs, WriteRequest{Device: c.device, Point: c.point, Value: val})
		pos = c.end()
	}
	if !s.forward(fc, reqs) {
		return mb.ExceptionPDU(fc, 0x04)
	}
	return []byte{fc, byte(addr >> 8), byte(addr), byte(qty >> 8), byte(qty)}
}

// forward 把写入转发给南向。全部成功才返回 true：Modbus 的写响应没有
// 「部分成功」的表达方式，返回 OK 而实际只写进去一半会静默丢数据。
func (s *Server) forward(fc byte, reqs []WriteRequest) bool {
	if len(reqs) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTo)
	defer cancel()
	s.writes.Add(1)
	res := s.writer.Write(ctx, reqs)
	ok := len(res) == len(reqs)
	for _, r := range res {
		if !r.OK {
			ok = false
			s.writeErrs.Add(1)
			if r.Err != nil {
				s.log.Warn("slave_write_failed", "slave", s.id, "fc", int(fc),
					"error", r.Err.Error())
			}
		}
	}
	if s.met != nil {
		result := "ok"
		if !ok {
			result = "error"
		}
		s.met.Inc("uic_slave_write_total", obs.Labels{"slave": s.id, "result": result})
	}
	return ok
}

// decodeCellValue 把线上字节还原成模型值（写路径）。
func decodeCellValue(c cell, raw []byte, order model.ByteOrder) (model.Value, error) {
	switch c.dt {
	case model.TypeBool:
		for _, b := range raw {
			if b != 0 {
				return model.BoolValue(true), nil
			}
		}
		return model.BoolValue(false), nil
	case model.TypeString:
		return model.TextValue(c.dt, strings.TrimRight(string(raw), "\x00")), nil
	case model.TypeBytes:
		return model.BytesValue(raw), nil
	}
	return value.Decode(raw, c.dt, order)
}

// itoa 避免为几个指标标签引入 strconv 的分配噪音。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
