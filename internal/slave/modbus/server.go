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
// 边界：只支持 Modbus TCP（MBAP），不支持 RTU/ASCII 从站，不支持
// 多主站串行共享（TCP 天然多连接）、不支持文件记录与诊断功能码。
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

	// WriteTimeout 是单次南向写入的总预算，默认 5s。
	WriteTimeout time.Duration
	// IdleTimeout 是空闲连接回收时间，默认 5 分钟。
	IdleTimeout time.Duration
	Log         *slog.Logger
	Metrics     *obs.Metrics
}

// Server 是一个 Modbus TCP 从站实例。
type Server struct {
	id         string
	ln         net.Listener
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
	requests   atomic.Uint64
	exceptions atomic.Uint64
	degraded   atomic.Uint64
	writes     atomic.Uint64
	writeErrs  atomic.Uint64
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

// New 校验配置、绑定端口并开始服务。
//
// 绑定在构造期完成而不是 Start()：端口被占用是**启动期错误**，应该让
// 进程带着明确的日志退出，而不是启动成功后在后台反复重试、让运维以为
// 从站已经可用。
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
	ln, err := net.Listen("tcp", opt.Slave.Listen)
	if err != nil {
		return nil, fmt.Errorf("slave %q: listen %s: %w", opt.Slave.ID, opt.Slave.Listen, err)
	}
	s := &Server{
		id:      opt.Slave.ID,
		ln:      ln,
		units:   units,
		src:     opt.Source,
		writer:  opt.Writer,
		writeTo: opt.WriteTimeout,
		idle:    opt.IdleTimeout,
		log:     opt.Log,
		met:     opt.Metrics,
		slots:   make(chan struct{}, opt.Slave.EffectiveMaxConnections()),
		conns:   map[net.Conn]struct{}{},
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
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// ID 返回从站标识。
func (s *Server) ID() string { return s.id }

// Addr 返回实际监听地址（配置为 :0 时可用来取随机端口）。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Stats 返回累计计数，供指标与测试使用。
type Stats struct {
	Requests   uint64
	Exceptions uint64
	Degraded   uint64
	Writes     uint64
	WriteErrs  uint64
}

// Stats 返回当前累计计数。
func (s *Server) Stats() Stats {
	return Stats{
		Requests:   s.requests.Load(),
		Exceptions: s.exceptions.Load(),
		Degraded:   s.degraded.Load(),
		Writes:     s.writes.Load(),
		WriteErrs:  s.writeErrs.Load(),
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
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
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
		unitID := int(head[6])
		s.requests.Add(1)

		u, ok := s.units[unitID]
		var resp []byte
		if !ok {
			// 网关目标不存在：0x0B 比「不响应」好——主站会看到确定的
			// 错误码，而不是 3 秒超时后去查网络。
			resp = mb.ExceptionPDU(pdu[0], 0x0B)
		} else {
			resp = s.handle(u, pdu)
		}
		if len(resp) >= 1 && resp[0]&0x80 != 0 {
			s.exceptions.Add(1)
			if s.met != nil {
				s.met.Inc("uic_slave_requests_total", obs.Labels{
					"slave": s.id, "unit": itoa(unitID),
					"fc": itoa(int(pdu[0])), "result": "exception",
				})
			}
		} else if s.met != nil {
			s.met.Inc("uic_slave_requests_total", obs.Labels{
				"slave": s.id, "unit": itoa(unitID),
				"fc": itoa(int(pdu[0])), "result": "ok",
			})
		}

		out := mb.BuildTCP(mb.Frame{Transaction: tx, Unit: uint8(unitID), PDU: resp})
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

func (s *Server) handle(u *unit, pdu []byte) []byte {
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
