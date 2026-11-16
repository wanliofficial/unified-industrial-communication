// Package enip 实现 EtherNet/IP（CIP 显式报文，TCP 44818）主站驱动。
//
// 分层与 Modbus/S7/FINS 一致：Meta（能力声明）→ conn（会话/事务）→
// codec/enip（纯函数编解码）→ transport（TCP 字节流）。
//
// 只做 CIP 显式报文的**符号标签**访问（Read Tag 0x4C / Write Tag 0x4D）：
//
//   - 覆盖符号标签最常用的场景：按标签名读/写原子类型与 STRING；
//   - 通过 Multiple Service Packet（0x0A）把一轮采集里的多条读请求合并成
//     一个报文，减少往返；设备不支持时自动退回逐条请求并记住结论；
//   - 位访问用「读元素 → 改位 → 写回」的 RMW，绝不清零邻位。
//
// 刻意的边界：不做隐式 I/O（UDP 2222）、不做对象/属性寻址、不做
// 结构体/数组的自动展开。CIP 的对象属性段编码在不同厂商文档里有
// 0x2C/0x30 两种说法，没有真机验证前不猜；数组只支持「按元素偏移切片」，
// 不猜 Member/Element 段的编码。
package enip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	enipcodec "uic/internal/codec/enip"
	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 EtherNet/IP 主站驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return transport.DialTCP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明 EtherNet/IP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "enip",
		Name:         "EtherNet/IP (CIP explicit messaging)",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapSupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:44818", Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "CIP 显式报文共用一条 TCP 会话；建议 1"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "标签名", Type: "string", Required: true, Scope: "point",
				Help: "标签名（大小写不敏感），如 Motor1_Speed；程序域标签写 Program:Main.Tag"},
			{Name: "address.offset", Label: "起始元素", Type: "int", Default: 0, Scope: "point",
				Help: "数组的起始元素下标；驱动读 0..offset 后切片，不猜 CIP 元素段编码"},
			{Name: "address.length", Label: "长度", Type: "int", Scope: "point",
				Help: "bytes 类型是字节数；string 是字符容量；数值类型固定 1 个元素"},
			{Name: "address.bit", Label: "位号", Type: "int", Default: -1, Scope: "point",
				Help: "bool 点位：-1 表示标签本身是 BOOL；0..63 表示从整数标签里取位（RMW 写回）"},
		},
		Description: "EtherNet/IP CIP 显式报文（TCP 44818）：符号标签读/写、STRING、位访问与多服务合包",
	}
}

// Open 建立 TCP 链路并注册会话。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("enip requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr := t.Address
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host:port)", false, nil)
	}
	if !strings.Contains(addr, ":") {
		addr = addr + fmt.Sprintf(":%d", enipcodec.DefaultPort)
	}
	stream, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	c := &conn{cfg: cfg, stream: stream, msp: true}
	if err := c.register(ctx); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return c, nil
}

type conn struct {
	cfg    driver.OpenConfig
	stream transport.Stream

	// mu 串行化整条会话。CIP 显式报文没有事务号，靠「一问一答」定界；
	// 两个请求交错会让响应对不上请求，这种错误极难在现场定位。
	mu      sync.Mutex
	session uint32
	seq     uint64

	// msp 记录设备是否支持 Multiple Service Packet。一次不支持之后不再尝试，
	// 避免每轮采集都先失败一次再退回。
	msp bool
}

func (c *conn) Close() error {
	// UnRegisterSession 没有响应，失败也不影响关闭。
	if c.session != 0 {
		pkt, err := enipcodec.BuildPacket(enipcodec.CmdUnregister, c.session, 0, c.nextContext(), nil)
		if err == nil {
			_ = c.stream.Write(pkt)
		}
	}
	return c.stream.Close()
}

// Probe 用 Get Attributes All（Identity 对象 class 0x01 / instance 0x01）
// 验证链路。即使设备不支持该服务（返回 0x08/0x14），也说明 CIP 层是通的。
func (c *conn) Probe(ctx context.Context) error {
	path, err := enipcodec.LogicalPath(0x01, 0x01)
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	req, err := enipcodec.BuildRequest(enipcodec.ServiceGetAttrAll, path, nil)
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	reply, err := c.send(ctx, req)
	if err != nil {
		return err
	}
	switch reply.Status {
	case 0x00, 0x08, 0x14:
		return nil
	}
	return &enipcodec.StatusError{Service: enipcodec.ServiceGetAttrAll, Code: reply.Status, Extra: reply.Extra}
}

// ---------------------------------------------------------------------------
// 会话与事务
// ---------------------------------------------------------------------------

func (c *conn) nextContext() [8]byte {
	c.seq++
	var ctx [8]byte
	binary.LittleEndian.PutUint64(ctx[:], c.seq)
	return ctx
}

// register 发送 RegisterSession 并保存会话句柄。
func (c *conn) register(ctx context.Context) error {
	ctxBytes := c.nextContext()
	pkt, err := enipcodec.BuildPacket(enipcodec.CmdRegisterSession, 0, 0, ctxBytes,
		enipcodec.BuildRegisterSession(1))
	if err != nil {
		return driver.E(driver.ErrInternal, "register", c.cfg.Device.ID, err.Error(), false, err)
	}
	h, body, err := c.roundTrip(ctx, enipcodec.CmdRegisterSession, ctxBytes, pkt)
	if err != nil {
		return err
	}
	if h.Status != 0 {
		return c.encapStatus("register", h.Status)
	}
	if h.Session == 0 {
		return driver.E(driver.ErrProtocol, "register", c.cfg.Device.ID,
			"device returned session handle 0", false, nil)
	}
	version, options, err := enipcodec.ParseRegisterSession(body)
	if err != nil {
		return driver.E(driver.ErrProtocol, "register", c.cfg.Device.ID, err.Error(), false, err)
	}
	if version != 1 || options != 0 {
		return driver.E(driver.ErrProtocol, "register", c.cfg.Device.ID,
			fmt.Sprintf("unsupported protocol version %d/options %d", version, options), false, nil)
	}
	c.session = h.Session
	return nil
}

// roundTrip 写一个封装报文并读回一个。
func (c *conn) roundTrip(ctx context.Context, wantCmd uint16, wantCtx [8]byte, pkt []byte) (enipcodec.Header, []byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := c.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := c.stream.Write(pkt); err != nil {
		return enipcodec.Header{}, nil, classifyNetErr("write", err)
	}
	head, err := c.stream.ReadFull(enipcodec.HeaderLen)
	if err != nil {
		return enipcodec.Header{}, nil, classifyNetErr("read-header", err)
	}
	h, err := enipcodec.ParseHeader(head)
	if err != nil {
		return enipcodec.Header{}, nil, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	if h.Command != wantCmd {
		return enipcodec.Header{}, nil, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("unexpected encapsulation command 0x%04X, want 0x%04X", h.Command, wantCmd), false, nil)
	}
	// 发送方上下文必须原样回显。全零视为「设备不实现该字段」而放行：
	// 少数嵌入式协议栈确实如此，为此拒绝会让设备完全不可用。
	if h.Context != wantCtx && h.Context != [8]byte{} {
		return enipcodec.Header{}, nil, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"sender context mismatch (stale or interleaved response)", false, nil)
	}
	body, err := c.stream.ReadFull(int(h.Length))
	if err != nil {
		return enipcodec.Header{}, nil, classifyNetErr("read-body", err)
	}
	if h.Status != 0 {
		return h, nil, c.encapStatus("read", h.Status)
	}
	return h, body, nil
}

func (c *conn) encapStatus(op string, status uint32) error {
	msg := fmt.Sprintf("encapsulation status 0x%08X (%s)", status, enipcodec.EncapsulationStatusText(status))
	if status == 0x64 {
		// 会话失效：重新建链会重新注册，属于可自愈。
		return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, msg, true, nil)
	}
	if status == 0x01 || status == 0x03 {
		return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID, msg, false, nil)
	}
	return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID, msg, false, nil)
}

// send 发送一条 CIP 报文并解析应答。
func (c *conn) send(ctx context.Context, cip []byte) (enipcodec.Reply, error) {
	payload, err := enipcodec.BuildSendRRData(cip)
	if err != nil {
		return enipcodec.Reply{}, driver.E(driver.ErrInternal, "encode", c.cfg.Device.ID, err.Error(), false, err)
	}
	ctxBytes := c.nextContext()
	pkt, err := enipcodec.BuildPacket(enipcodec.CmdSendRRData, c.session, 0, ctxBytes, payload)
	if err != nil {
		return enipcodec.Reply{}, driver.E(driver.ErrInternal, "encode", c.cfg.Device.ID, err.Error(), false, err)
	}
	_, body, err := c.roundTrip(ctx, enipcodec.CmdSendRRData, ctxBytes, pkt)
	if err != nil {
		return enipcodec.Reply{}, err
	}
	raw, err := enipcodec.ParseSendRRData(body)
	if err != nil {
		return enipcodec.Reply{}, driver.E(driver.ErrProtocol, "decode", c.cfg.Device.ID, err.Error(), false, err)
	}
	reply, err := enipcodec.ParseReply(raw)
	if err != nil {
		return enipcodec.Reply{}, driver.E(driver.ErrProtocol, "decode", c.cfg.Device.ID, err.Error(), false, err)
	}
	return reply, nil
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// maxReadBytes 是一次 Read Tag 的保守上限。CIP 连接的标准报文尺寸通常
// 是 504 字节；超过后设备会回 0x06（partial transfer）。与其让设备报错，
// 不如在驱动侧明确拒绝并给出「缩小 length」的提示。
const maxReadBytes = 400

func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []driver.RawSample
	var firstErr error
	okSamples := 0
	for si, span := range spans {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
			// 剩余 span 的点位必须显式标坏，不能让它们从结果里消失——
			// 消失的点位在采集层表现为「没有新样本」，会被当成设备安静，
			// 把真正的超时原因盖掉。
			for _, rest := range spans[si:] {
				out = append(out, failed(rest, e)...)
			}
			return out, e
		}
		samples, err := c.readSpan(ctx, span)
		out = append(out, samples...)
		okSamples += goodCount(samples)
		if err != nil {
			// 链路级失败：立即返回，不由下面的闸门吞掉——一轮采集被周期
			// 超时打断本身就是「设备太慢」的信号，需要让调度器看到。
			if driver.IsLinkFailure(err) {
				for _, rest := range spans[si+1:] {
					out = append(out, failed(rest, err)...)
				}
				return out, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误。
	return out, driver.PartialReadError(okSamples, firstErr)
}

// goodCount 数一段结果里的成功点位。
func goodCount(samples []driver.RawSample) int {
	n := 0
	for _, s := range samples {
		if s.Err == nil && s.Quality == model.QualityGood {
			n++
		}
	}
	return n
}

func (c *conn) readSpan(ctx context.Context, span driver.ReadSpan) ([]driver.RawSample, error) {
	if len(span.Points) == 0 {
		return nil, nil
	}
	plans := make([]pointPlan, len(span.Points))
	reqs := make([][]byte, len(span.Points))
	for i, p := range span.Points {
		plan, req, err := buildRead(p)
		if err != nil {
			return failed(span, err), err
		}
		plans[i], reqs[i] = plan, req
	}
	replies, err := c.readBatch(ctx, reqs)
	if err != nil {
		return failed(span, err), err
	}
	out := make([]driver.RawSample, 0, len(plans))
	var firstErr error
	for i, plan := range plans {
		if replies[i].err != nil {
			out = append(out, errSample(plan.point.ID, replies[i].err))
			if firstErr == nil {
				firstErr = replies[i].err
			}
			continue
		}
		s, err := decodeRead(plan, replies[i].reply)
		if nm, ok := err.(*needMore); ok {
			// bytes 点位的元素宽度只能从应答里学到；学到之后按实际宽度
			// 重新请求一次，避免把 int32 数组当 SINT 读（那会直接读到
			// 类型不匹配，或者更糟：读回半截数据）。
			req, berr := enipcodec.BuildReadTag(plan.point.Address.Area, uint16(nm.count))
			if berr != nil {
				err = driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, berr.Error(), false, berr)
			} else if reply, rerr := c.send(ctx, req); rerr != nil {
				err = rerr
			} else {
				s, err = decodeRead(plan, reply)
			}
		}
		if err != nil {
			out = append(out, errSample(plan.point.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, s)
	}
	return out, firstErr
}

type batchReply struct {
	reply enipcodec.Reply
	err   error
}

// readBatch 尽量用 Multiple Service Packet 合并请求；设备不支持时退回逐条。
func (c *conn) readBatch(ctx context.Context, reqs [][]byte) ([]batchReply, error) {
	out := make([]batchReply, len(reqs))
	if len(reqs) == 1 {
		reply, err := c.send(ctx, reqs[0])
		out[0] = batchReply{reply: reply, err: err}
		return out, err
	}
	if c.msp {
		batch := make([][]byte, 0, len(reqs))
		idx := make([]int, 0, len(reqs))
		for i, r := range reqs {
			if len(batch) > 0 && packetSize(batch)+len(r) > 450 {
				if err := c.sendMultiple(ctx, batch, idx, out); err != nil {
					return out, err
				}
				batch, idx = batch[:0], idx[:0]
			}
			batch = append(batch, r)
			idx = append(idx, i)
		}
		if len(batch) > 0 {
			if err := c.sendMultiple(ctx, batch, idx, out); err != nil {
				return out, err
			}
		}
		return out, nil
	}
	var firstErr error
	for i, r := range reqs {
		reply, err := c.send(ctx, r)
		out[i] = batchReply{reply: reply, err: err}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return out, firstErr
}

func packetSize(reqs [][]byte) int {
	n := 2 + 2*len(reqs)
	for _, r := range reqs {
		n += len(r)
	}
	return n
}

func (c *conn) sendMultiple(ctx context.Context, reqs [][]byte, idx []int, out []batchReply) error {
	pkt, err := enipcodec.BuildMultiple(reqs)
	if err != nil {
		for _, i := range idx {
			out[i] = batchReply{err: driver.E(driver.ErrInternal, "encode", c.cfg.Device.ID, err.Error(), false, err)}
		}
		return err
	}
	reply, err := c.send(ctx, pkt)
	if err != nil {
		for _, i := range idx {
			out[i] = batchReply{err: err}
		}
		return err
	}
	if reply.Status == 0x08 || reply.Status == 0x14 {
		// 设备不支持多服务合包：关掉它，本批退回逐条请求。
		c.msp = false
		var firstErr error
		for k, i := range idx {
			r, err := c.send(ctx, reqs[k])
			out[i] = batchReply{reply: r, err: err}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	replies, err := enipcodec.ParseMultipleReply(reply)
	if err != nil {
		for _, i := range idx {
			out[i] = batchReply{err: err}
		}
		return err
	}
	if len(replies) != len(idx) {
		err = driver.E(driver.ErrProtocol, "decode", c.cfg.Device.ID,
			fmt.Sprintf("multiple service returned %d replies for %d requests", len(replies), len(idx)), false, nil)
		for _, i := range idx {
			out[i] = batchReply{err: err}
		}
		return err
	}
	for k, i := range idx {
		r, err := enipcodec.ParseReply(replies[k])
		if err != nil {
			out[i] = batchReply{err: driver.E(driver.ErrProtocol, "decode", c.cfg.Device.ID, err.Error(), false, err)}
			continue
		}
		out[i] = batchReply{reply: r}
	}
	return nil
}

// pointPlan 描述一个点位如何从 Read Tag 应答里取值。
type pointPlan struct {
	point    driver.PointRef
	kind     planKind
	cipType  byte
	elemSize int
	skip     int // 应答数据里要跳过的字节数（offset × 元素宽度）
	want     int // 要取用的字节数
	bit      int
}

type planKind int

const (
	planNumeric planKind = iota
	planBoolTag
	planBoolBit
	planString
	planBytes
)

// address.length 的语义按数据类型解释：bytes 是字节数、string 是字符容量、
// 数值/bool 是元素个数（只支持 1）。不能直接用 PointRef.Length：它按
// 「16 位寄存器数」推导，int32 会得到 2，而 CIP 的 int32 是 1 个元素。

func buildRead(p driver.PointRef) (pointPlan, []byte, error) {
	tag := p.Address.Area
	if strings.TrimSpace(tag) == "" {
		return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: address.area (tag name) required", p.ID), false, nil)
	}
	if p.Address.Offset < 0 {
		return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: address.offset must be >= 0", p.ID), false, nil)
	}
	plan := pointPlan{point: p, bit: p.Address.Bit}

	switch p.DataType {
	case model.TypeString:
		if p.Address.Offset != 0 {
			return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
				fmt.Sprintf("point %q: STRING does not support offset > 0", p.ID), false, nil)
		}
		plan.kind = planString
		plan.cipType = enipcodec.TypeSTRING
	case model.TypeBytes:
		n := p.Address.Length
		if n <= 0 {
			return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
				fmt.Sprintf("point %q: bytes requires address.length (byte count)", p.ID), false, nil)
		}
		plan.kind = planBytes
		plan.want = n
	case model.TypeBool:
		if p.Address.Bit >= 0 {
			// 位在整数标签里：元素宽度要等应答里的实际类型才知道，
			// 先按 1 占位，解码时再校正。
			plan.kind = planBoolBit
		} else {
			plan.kind = planBoolTag
			plan.elemSize = 1
		}
	default:
		plan.kind = planNumeric
		typ, ok := cipTypeOf(p.DataType)
		if !ok {
			return pointPlan{}, nil, driver.E(driver.ErrUnsupported, "read", "",
				fmt.Sprintf("point %q: dataType %q has no CIP mapping", p.ID, p.DataType), false, nil)
		}
		if p.Address.Length > 1 {
			return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
				fmt.Sprintf("point %q: numeric points support one element; use bytes for arrays", p.ID), false, nil)
		}
		plan.cipType = typ
		plan.elemSize = enipcodec.TypeSize(typ)
		plan.want = plan.elemSize
		plan.skip = p.Address.Offset * plan.elemSize
	}

	count := 1
	if plan.kind == planBytes {
		// 先读一个元素学类型/宽度；解码阶段发现不够会带 count 重试。
		count = p.Address.Offset + 1
	} else if plan.kind == planNumeric {
		count = p.Address.Offset + 1
	} else if plan.kind == planBoolTag || plan.kind == planBoolBit {
		count = p.Address.Offset + 1
	}
	if plan.kind == planNumeric {
		if count*max(1, plan.elemSize) > maxReadBytes {
			return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
				fmt.Sprintf("point %q: %d elements exceed the %d-byte read limit; split the point or use a smaller length",
					p.ID, count, maxReadBytes), false, nil)
		}
	}
	req, err := enipcodec.BuildReadTag(tag, uint16(count))
	if err != nil {
		return pointPlan{}, nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: %v", p.ID, err), false, err)
	}
	return plan, req, nil
}

func cipTypeOf(t model.DataType) (byte, bool) {
	switch t {
	case model.TypeBool:
		return enipcodec.TypeBOOL, true
	case model.TypeInt16:
		return enipcodec.TypeINT, true
	case model.TypeUint16:
		return enipcodec.TypeUINT, true
	case model.TypeInt32:
		return enipcodec.TypeDINT, true
	case model.TypeUint32:
		return enipcodec.TypeUDINT, true
	case model.TypeInt64:
		return enipcodec.TypeLINT, true
	case model.TypeUint64:
		return enipcodec.TypeULINT, true
	case model.TypeFloat32:
		return enipcodec.TypeREAL, true
	case model.TypeFloat64:
		return enipcodec.TypeLREAL, true
	}
	return 0, false
}

func decodeRead(plan pointPlan, reply enipcodec.Reply) (driver.RawSample, error) {
	typ, count, data, err := enipcodec.ParseReadTagReply(reply)
	if err != nil {
		return driver.RawSample{}, wrapStatus(err)
	}
	p := plan.point
	if count < 1 {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "decode", "",
			fmt.Sprintf("point %q: device returned 0 elements", p.ID), false, nil)
	}
	switch plan.kind {
	case planBoolTag:
		if typ != enipcodec.TypeBOOL {
			return driver.RawSample{}, typeMismatch(p, enipcodec.TypeBOOL, typ)
		}
		if len(data) <= p.Address.Offset {
			return driver.RawSample{}, shortReply(p)
		}
		return goodSample(p.ID, model.BoolValue(data[p.Address.Offset] != 0)), nil

	case planBoolBit:
		size := enipcodec.TypeSize(typ)
		if size <= 0 || !isInteger(typ) {
			return driver.RawSample{}, driver.E(driver.ErrAddress, "decode", "",
				fmt.Sprintf("point %q: bit access requires an integer tag, device returned %s", p.ID, enipcodec.TypeText(typ)), false, nil)
		}
		if plan.bit < 0 || plan.bit >= size*8 {
			return driver.RawSample{}, driver.E(driver.ErrAddress, "decode", "",
				fmt.Sprintf("point %q: bit %d outside %s (0..%d)", p.ID, plan.bit, enipcodec.TypeText(typ), size*8-1), false, nil)
		}
		need := (p.Address.Offset + 1) * size
		if len(data) < need {
			return driver.RawSample{}, shortReply(p)
		}
		elem := data[p.Address.Offset*size : need]
		v := leUint(elem)
		return goodSample(p.ID, model.BoolValue(v&(1<<uint(plan.bit)) != 0)), nil

	case planString:
		if typ != enipcodec.TypeSTRING {
			return driver.RawSample{}, typeMismatch(p, enipcodec.TypeSTRING, typ)
		}
		s, err := enipcodec.DecodeString(data)
		if err != nil {
			return driver.RawSample{}, driver.E(driver.ErrProtocol, "decode", "",
				fmt.Sprintf("point %q: %v", p.ID, err), false, err)
		}
		return goodSample(p.ID, model.TextValue(model.TypeString, s)), nil

	case planBytes:
		size := enipcodec.TypeSize(typ)
		if size <= 0 {
			return driver.RawSample{}, driver.E(driver.ErrAddress, "decode", "",
				fmt.Sprintf("point %q: bytes requires a fixed-size tag, device returned %s", p.ID, enipcodec.TypeText(typ)), false, nil)
		}
		skip := p.Address.Offset * size
		if len(data) < skip+plan.want {
			// 需要的元素数按「覆盖到 skip+want 的最小整数个元素」计算。
			// 学到宽度之后必须再核一次上限：第一次请求只有 1 个元素，
			// 不可能在 buildRead 阶段就知道实际要读多少字节。
			need := p.Address.Offset + (plan.want+size-1)/size
			if need*size > maxReadBytes {
				return driver.RawSample{}, driver.E(driver.ErrAddress, "decode", "",
					fmt.Sprintf("point %q: %d bytes exceed the %d-byte read limit; split the point or use a smaller length",
						p.ID, need*size, maxReadBytes), false, nil)
			}
			return driver.RawSample{}, &needMore{count: need}
		}
		return goodSample(p.ID, model.BytesValue(data[skip:skip+plan.want])), nil
	}

	// 数值：应答类型必须与配置同宽、同符号族。
	size := enipcodec.TypeSize(typ)
	if size <= 0 {
		return driver.RawSample{}, typeMismatch(p, plan.cipType, typ)
	}
	if !compatible(plan.cipType, typ) {
		return driver.RawSample{}, typeMismatch(p, plan.cipType, typ)
	}
	need := (p.Address.Offset + 1) * size
	if len(data) < need {
		return driver.RawSample{}, shortReply(p)
	}
	elem := data[p.Address.Offset*size : need]
	v, err := value.Decode(reverse(elem), p.DataType, model.OrderABCD)
	if err != nil {
		return driver.RawSample{}, driver.E(driver.ErrProtocol, "decode", "",
			fmt.Sprintf("point %q: %v", p.ID, err), false, err)
	}
	return goodSample(p.ID, v), nil
}

func goodSample(id string, v model.Value) driver.RawSample {
	return driver.RawSample{PointID: id, Value: v, Quality: model.QualityGood}
}

func typeMismatch(p driver.PointRef, want, got byte) error {
	return driver.E(driver.ErrAddress, "decode", "",
		fmt.Sprintf("point %q: tag type is %s (0x%02X), configured for %s (0x%02X)",
			p.ID, enipcodec.TypeText(got), got, enipcodec.TypeText(want), want), false, nil)
}

func shortReply(p driver.PointRef) error {
	return driver.E(driver.ErrProtocol, "decode", "",
		fmt.Sprintf("point %q: Read Tag reply shorter than requested elements", p.ID), false, nil)
}

// wrapStatus 把 codec 的 StatusError 原样返回，其它错误归为协议错。
func wrapStatus(err error) error {
	var se *enipcodec.StatusError
	if errors.As(err, &se) {
		return se
	}
	return driver.E(driver.ErrProtocol, "decode", "", err.Error(), false, err)
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		for i := range results {
			if results[i].Err == nil {
				results[i].Err = firstErr
			}
		}
	}
	return results, firstErr
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	tag := req.Address.Area
	if strings.TrimSpace(tag) == "" {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: address.area (tag name) required", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	if req.Address.Offset < 0 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: address.offset must be >= 0", req.PointID), false, nil)
		return writeFail(req, err), err
	}

	switch req.DataType {
	case model.TypeString:
		return c.writeString(ctx, req)
	case model.TypeBytes:
		return c.writeBytes(ctx, req)
	case model.TypeBool:
		if req.Address.Bit >= 0 {
			return c.writeBit(ctx, req)
		}
		return c.writeRaw(ctx, req, enipcodec.TypeBOOL, 1, boolByte(req.Value.Bool))
	}
	typ, ok := cipTypeOf(req.DataType)
	if !ok {
		err := driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: dataType %q has no CIP mapping", req.PointID, req.DataType), false, nil)
		return writeFail(req, err), err
	}
	if req.Address.Length > 1 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: numeric points support one element; use bytes for arrays", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	raw, err := value.Encode(req.Value, req.DataType, model.OrderABCD)
	if err != nil {
		err = driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	return c.writeRaw(ctx, req, typ, enipcodec.TypeSize(typ), reverse(raw))
}

// writeRaw 写一个固定宽度类型的单元素（或带前缀的整个尾部）。
func (c *conn) writeRaw(ctx context.Context, req driver.WriteRequest, typ byte, size int, elem []byte) (driver.WriteResult, error) {
	if size <= 0 {
		err := driver.E(driver.ErrInternal, "write", c.cfg.Device.ID, "invalid element size", false, nil)
		return writeFail(req, err), err
	}
	payload := elem
	count := 1
	writeType := typ
	if req.Address.Offset > 0 {
		// 写 offset>0 必须先读前缀：Read Tag 没有偏移参数，直接写 count
		// 会把前缀元素一起覆盖成零。
		prefix, actual, err := c.readElements(ctx, req.Address.Area, req.Address.Offset+1)
		if err != nil {
			return writeFail(req, err), err
		}
		if !compatible(typ, actual) {
			err = typeMismatchForWrite(req, typ, actual)
			return writeFail(req, err), err
		}
		start := req.Address.Offset * size
		if len(prefix) < start+size {
			err := shortReplyForWrite(req)
			return writeFail(req, err), err
		}
		payload = append(prefix[:start], elem...)
		count = req.Address.Offset + 1
		writeType = actual
	}
	return c.sendWrite(ctx, req, req.Address.Area, writeType, count, payload)
}

func (c *conn) writeString(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	if req.Address.Offset != 0 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: STRING does not support offset > 0", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	capacity := req.Address.Length
	if capacity <= 0 {
		capacity = 82 // 常见实现的默认 STRING 容量
	}
	data, err := enipcodec.EncodeString(req.Value.Text, capacity)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	return c.sendWrite(ctx, req, req.Address.Area, enipcodec.TypeSTRING, 1, data)
}

func (c *conn) writeBytes(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	n := req.Address.Length
	if n <= 0 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: bytes requires address.length (byte count)", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	// 先读一个元素学类型与宽度：标签可能是 SINT、INT 或 DINT 数组，
	// 用错类型写会被设备拒绝；即使宽度对得上，也必须保留目标区间之外
	// 的字节。
	data, typ, err := c.readElements(ctx, req.Address.Area, req.Address.Offset+1)
	if err != nil {
		return writeFail(req, err), err
	}
	size := enipcodec.TypeSize(typ)
	if size <= 0 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: bytes requires a fixed-size tag, got %s", req.PointID, enipcodec.TypeText(typ)), false, nil)
		return writeFail(req, err), err
	}
	skip := req.Address.Offset * size
	needElems := req.Address.Offset + (n+size-1)/size
	if len(data) < needElems*size {
		data, typ, err = c.readElements(ctx, req.Address.Area, needElems)
		if err != nil {
			return writeFail(req, err), err
		}
		size = enipcodec.TypeSize(typ)
		skip = req.Address.Offset * size
	}
	if len(data) < skip+n {
		err := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: tag shorter than requested byte range", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	raw := []byte(req.Value.Text)
	if len(raw) > n {
		raw = raw[:n]
	}
	payload := append([]byte(nil), data...)
	copy(payload[skip:skip+len(raw)], raw)
	return c.sendWrite(ctx, req, req.Address.Area, typ, len(payload)/size, payload)
}

// needMore 表示 bytes 点位需要按实际元素宽度扩大读取范围。
type needMore struct{ count int }

func (e *needMore) Error() string {
	return fmt.Sprintf("enip: need %d elements", e.count)
}

// writeBit 做「读元素 → 改一位 → 写回」，保留同一元素里的其它位。
func (c *conn) writeBit(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	data, actual, err := c.readElements(ctx, req.Address.Area, req.Address.Offset+1)
	if err != nil {
		return writeFail(req, err), err
	}
	size := enipcodec.TypeSize(actual)
	if size <= 0 || !isInteger(actual) {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: bit access requires an integer tag, got %s (%0X)",
				req.PointID, enipcodec.TypeText(actual), actual), false, nil)
		return writeFail(req, err), err
	}
	if req.Address.Bit < 0 || req.Address.Bit >= size*8 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: bit %d outside %s (0..%d)", req.PointID, req.Address.Bit,
				enipcodec.TypeText(actual), size*8-1), false, nil)
		return writeFail(req, err), err
	}
	start := req.Address.Offset * size
	if len(data) < start+size {
		err := shortReplyForWrite(req)
		return writeFail(req, err), err
	}
	elem := append([]byte(nil), data[start:start+size]...)
	v := leUint(elem)
	if req.Value.Bool {
		v |= 1 << uint(req.Address.Bit)
	} else {
		v &^= 1 << uint(req.Address.Bit)
	}
	putLE(elem, v)
	payload := append(append([]byte(nil), data[:start]...), elem...)
	return c.sendWrite(ctx, req, req.Address.Area, actual, req.Address.Offset+1, payload)
}

func (c *conn) readElements(ctx context.Context, tag string, count int) ([]byte, byte, error) {
	req, err := enipcodec.BuildReadTag(tag, uint16(count))
	if err != nil {
		return nil, 0, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	reply, err := c.send(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	typ, got, data, err := enipcodec.ParseReadTagReply(reply)
	if err != nil {
		return nil, 0, wrapStatus(err)
	}
	size := enipcodec.TypeSize(typ)
	if size <= 0 {
		return nil, 0, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("tag %q returned non-atomic type %s", tag, enipcodec.TypeText(typ)), false, nil)
	}
	if int(got) < count || len(data) < count*size {
		return nil, 0, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("tag %q returned %d elements, want %d", tag, got, count), false, nil)
	}
	return data[:count*size], typ, nil
}

func (c *conn) sendWrite(ctx context.Context, req driver.WriteRequest, tag string, typ byte, count int, data []byte) (driver.WriteResult, error) {
	if len(data) > maxReadBytes {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: %d bytes exceed the %d-byte write limit", req.PointID, len(data), maxReadBytes), false, nil)
		return writeFail(req, err), err
	}
	msg, err := enipcodec.BuildWriteTag(tag, typ, uint16(count), data)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	reply, err := c.send(ctx, msg)
	if err != nil {
		return writeFail(req, err), err
	}
	if _, _, err := enipcodec.ParseWriteTagReply(reply); err != nil {
		err = wrapStatus(err)
		return writeFail(req, err), err
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

func typeMismatchForWrite(req driver.WriteRequest, want, got byte) error {
	return driver.E(driver.ErrAddress, "write", "",
		fmt.Sprintf("point %q: tag type is %s, configured for %s",
			req.PointID, enipcodec.TypeText(got), enipcodec.TypeText(want)), false, nil)
}

func shortReplyForWrite(req driver.WriteRequest) error {
	return driver.E(driver.ErrProtocol, "write", "",
		fmt.Sprintf("point %q: Read Tag reply shorter than requested elements", req.PointID), false, nil)
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}
}

// ---------------------------------------------------------------------------
// 类型兼容与位操作
// ---------------------------------------------------------------------------

// compatible 判断配置类型与设备实际类型是否可互操作。
//
// 允许同宽的 signed/unsigned 互换：现场把 DINT 标签配成 uint32 很常见，
// 拒绝这种配置只会逼运维去改标签类型。位宽不同则一定是配错了。
func compatible(expected, actual byte) bool {
	family := func(t byte) (size int, numeric bool, signed bool) {
		switch t {
		case enipcodec.TypeSINT:
			return 1, true, true
		case enipcodec.TypeUSINT, enipcodec.TypeBYTE:
			return 1, true, false
		case enipcodec.TypeINT:
			return 2, true, true
		case enipcodec.TypeUINT, enipcodec.TypeWORD:
			return 2, true, false
		case enipcodec.TypeDINT:
			return 4, true, true
		case enipcodec.TypeUDINT, enipcodec.TypeDWORD:
			return 4, true, false
		case enipcodec.TypeLINT:
			return 8, true, true
		case enipcodec.TypeULINT, enipcodec.TypeLWORD:
			return 8, true, false
		case enipcodec.TypeREAL:
			return 4, false, false
		case enipcodec.TypeLREAL:
			return 8, false, false
		case enipcodec.TypeBOOL:
			return 1, false, false
		}
		return -1, false, false
	}
	es, en, _ := family(expected)
	as, an, _ := family(actual)
	if es != as || es < 0 {
		return false
	}
	// 浮点与非浮点不能互换（位模式解释完全不同）。
	return en == an
}

func isInteger(t byte) bool {
	switch t {
	case enipcodec.TypeSINT, enipcodec.TypeINT, enipcodec.TypeDINT, enipcodec.TypeLINT,
		enipcodec.TypeUSINT, enipcodec.TypeUINT, enipcodec.TypeUDINT, enipcodec.TypeULINT,
		enipcodec.TypeBYTE, enipcodec.TypeWORD, enipcodec.TypeDWORD, enipcodec.TypeLWORD:
		return true
	}
	return false
}

func boolByte(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

func leUint(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func putLE(b []byte, v uint64) {
	for i := range b {
		b[i] = byte(v)
		v >>= 8
	}
}

// reverse 把 CIP 的小端元素转成 value 包约定的大端。
func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

func failed(span driver.ReadSpan, err error) []driver.RawSample {
	q := qualityOf(err)
	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		out = append(out, driver.RawSample{PointID: p.ID, Quality: q, Err: err})
	}
	return out
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityOf(err), Err: err}
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}

func classifyNetErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return driver.E(driver.ErrTimeout, op, "", "network timeout", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return driver.E(driver.ErrTimeout, op, "", "deadline exceeded", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, "", "connection lost", true, err)
}
