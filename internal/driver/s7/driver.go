// Package s7 实现 S7comm 主站驱动（ISO-on-TCP / RFC1006 之上）。
//
// 与 Modbus 驱动的结构相同：Meta（能力声明）→ conn（事务/错误分类）→
// codec/s7（纯函数编解码）→ transport（字节流）。差别在于「一次事务
// 可以带多项」，因此规划层的连续性在这里不重要，重要的是按 PDU 预算
// 切块。
package s7

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"uic/internal/codec/s7"
	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 S7 主站驱动。
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

// Meta 声明 S7 能力。
//
// BitAccess 声明 supported：DBX/MX/IX 位访问是 S7 的日常用法，且比
// 「读整字节再自己拆位」可靠——后者在字节被别的点位同时改写的场景下
// 会读到撕裂值。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "s7",
		Name:         "Siemens S7 (S7comm)",
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
				Placeholder: "192.168.1.10:102", Scope: "channel"},
			{Name: "rack", Label: "机架号", Type: "int", Default: 0, Scope: "channel",
				Help: "S7-300/400/1200/1500 通常都是 0"},
			{Name: "slot", Label: "槽号", Type: "int", Default: 2, Scope: "channel",
				Help: "S7-300/400 通常 2；S7-1200/1500 通常 1"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "多数 S7 CPU 只允许很少的并行连接；保持 1 最稳"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "区", Type: "enum", Required: true,
				Enum: []string{"db", "input", "output", "merker", "counter", "timer"}, Scope: "point"},
			{Name: "address.db", Label: "DB 号", Type: "int", Scope: "point",
				Help: "area=db 时必填，如 DB1 填 1"},
			{Name: "address.offset", Label: "字节偏移", Type: "int", Required: true, Scope: "point"},
			{Name: "address.bit", Label: "位号", Type: "int", Default: -1, Scope: "point",
				Help: "仅 bool 点位使用，0..7；其余类型留空"},
			{Name: "address.length", Label: "字节宽度", Type: "int", Scope: "point",
				Help: "string/bytes 必填。string 的宽度含 2 字节头（DB STRING[n] = 2+n），写入自动带 [max,cur] 头、读取剥头；bytes 是裸字节，不含头"},
		},
		Description: "S7comm 主站（TCP 102），支持 DB/I/Q/M/C/T 区读写与位访问（DB STRING 带 2 字节长度头）",
	}
}

// Open 建立 TPKT/COTP 连接并协商 S7 PDU 长度。
//
// 握手顺序不能省：先 COTP CR/CC 建立 ISO 连接，再 Setup Communication
// 协商 PDU。跳过 Setup 直接读写在部分 CPU 上「看起来能用」，但超过
// 240 字节的请求会被静默丢弃，现场表现为「读几个点正常，读多了就没反应」。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("s7 requires tcp transport, got %q", t.Kind), false, nil)
	}
	if t.Address == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host:port)", false, nil)
	}
	rack, slot := cfg.Channel.EffectiveRack(), cfg.Channel.EffectiveSlot()
	if rack < 0 || rack > 7 {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("rack %d out of range 0..7", rack), false, nil)
	}
	if slot < 0 || slot > 31 {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("slot %d out of range 0..31", slot), false, nil)
	}

	stream, err := d.dial(ctx, t.Address, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", t.Address), true, err)
	}

	c := &conn{
		stream:   stream,
		deviceID: cfg.Device.ID,
		timeout:  cfg.Channel.EffectiveTimeout(),
		pduLen:   480,
	}
	if err := c.handshake(ctx, rack, slot); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return c, nil
}

// conn 是一条 S7 连接。同一 Conn 不会被并发调用（契约），但仍加锁：
// Close 可能来自别的 goroutine，裸指针竞争在 -race 下必须干净。
type conn struct {
	mu       sync.Mutex
	stream   transport.Stream
	deviceID string
	timeout  time.Duration
	pduLen   int
	ref      atomic.Uint32
	closed   bool
}

func (c *conn) nextRef() uint16 { return uint16(c.ref.Add(1)) }

func (c *conn) handshake(ctx context.Context, rack, slot int) error {
	if err := c.writeFrame(ctx, s7.COTPConnectWith(rack, slot)); err != nil {
		return err
	}
	frame, err := c.readFrame(ctx)
	if err != nil {
		return err
	}
	if err := s7.ParseCOTPConnect(frame); err != nil {
		return driver.E(driver.ErrProtocol, "connect", c.deviceID, err.Error(), true, err)
	}

	pdu := s7.BuildPDU(s7.ROSCTRJob, c.nextRef(), s7.SetupParams(480), nil)
	if err := c.writeFrame(ctx, s7.WrapDT(pdu)); err != nil {
		return err
	}
	resp, err := c.readPDU(ctx)
	if err != nil {
		return err
	}
	n, err := s7.ParseSetupAck(resp)
	if err != nil {
		return driver.E(driver.ErrProtocol, "setup", c.deviceID, err.Error(), true, err)
	}
	c.pduLen = n
	return nil
}

func (c *conn) writeFrame(ctx context.Context, frame []byte) error {
	if c.stream == nil {
		return driver.E(driver.ErrDisconnected, "write", c.deviceID, "connection closed", true, nil)
	}
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := c.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := c.stream.Write(frame); err != nil {
		return classifyNetErr(c.deviceID, "write", err)
	}
	return nil
}

// readFrame 先读 4 字节 TPKT 头拿到总长，再读满剩余部分。
//
// 这里必须做长度上限检查：TPKT 长度字段是对端给的，损坏时可能声称
// 65535 字节。不检查就会让一次坏帧吃掉 64KB 内存并一直等到超时。
func (c *conn) readFrame(ctx context.Context) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := c.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	head, err := c.stream.ReadFull(4)
	if err != nil {
		return nil, classifyNetErr(c.deviceID, "read-header", err)
	}
	if head[0] != s7.TPKTVersion {
		return nil, driver.E(driver.ErrProtocol, "read", c.deviceID,
			fmt.Sprintf("tpkt version %d, want 3", head[0]), true, nil)
	}
	n := int(head[2])<<8 | int(head[3])
	if n < 4 || n > 8192 {
		return nil, driver.E(driver.ErrProtocol, "read", c.deviceID,
			fmt.Sprintf("tpkt length %d out of range", n), true, nil)
	}
	if n == 4 {
		return head, nil
	}
	body, err := c.stream.ReadFull(n - 4)
	if err != nil {
		return nil, classifyNetErr(c.deviceID, "read-body", err)
	}
	return append(head, body...), nil
}

func (c *conn) roundTrip(ctx context.Context, pdu []byte) (s7.PDU, error) {
	if err := c.writeFrame(ctx, s7.WrapDT(pdu)); err != nil {
		return s7.PDU{}, err
	}
	return c.readPDU(ctx)
}

func (c *conn) readPDU(ctx context.Context) (s7.PDU, error) {
	frame, err := c.readFrame(ctx)
	if err != nil {
		return s7.PDU{}, err
	}
	resp, err := s7.Parse(frame)
	if err != nil {
		return s7.PDU{}, driver.E(driver.ErrProtocol, "read", c.deviceID, err.Error(), true, err)
	}
	if err := resp.Error(); err != nil {
		// ACK / ACK_DATA 的顶层错误意味着整帧失败，参数区与数据区都不可信。
		// 继续按单项返回码解析会得到一串「看似正常」的错位数据。
		return s7.PDU{}, driver.E(driver.ErrProtocol, "read", c.deviceID, err.Error(), true, err)
	}
	return resp, nil
}

// Probe 读 M0.0（一个标志位字节）。
//
// 单项返回「地址不存在」也算链路可达：探活要回答的是「CPU 还应答吗」，
// 不是「这个地址配得对吗」。把地址错误当断链会让一个配错的探活点
// 把整台设备永久判死。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	items := []s7.Item{{Area: s7.AreaMerker, DB: 0, ByteAddr: 0, Bit: -1, Bytes: 1}}
	pdu, err := s7.BuildRead(c.nextRef(), items)
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.deviceID, err.Error(), false, err)
	}
	resp, err := c.roundTrip(ctx, pdu)
	if err != nil {
		return err
	}
	if _, _, err := s7.ParseReadAck(resp, items); err != nil {
		return driver.E(driver.ErrProtocol, "probe", c.deviceID, err.Error(), true, err)
	}
	return nil
}

// readRef 是一个点位与其 S7 变量说明的配对。
type readRef struct {
	point driver.PointRef
	item  s7.Item
}

// Read 按 PDU 预算切块，每块一次事务。
//
// 与 Modbus 的差异：S7 的多项请求不要求地址连续，因此一个块可以同时
// 覆盖 DB1、DB2、M0、I0 —— 这正是 S7 相比 Modbus 的吞吐优势来源。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		refs   []readRef
		out    []driver.RawSample
		badPre []driver.RawSample
	)
	for _, span := range spans {
		for _, p := range span.Points {
			it, err := s7.ItemOf(p)
			if err != nil {
				badPre = append(badPre, errSample(p.ID, err))
				continue
			}
			// 单项都装不进协商 PDU 的，切块再怎么切也装不下：对端要么直接
			// 不回（表现为超时），要么把响应截断。这是**地址配置错**
			// （address.length 大于 PDU 能承载的上限），发出去只会把问题
			// 伪装成链路故障，必须在本地判坏。
			if ok, need := fitsPDU(it, c.pduLen, false); !ok {
				badPre = append(badPre, errSample(p.ID, pduBudgetErr("read", c.deviceID, need, c.pduLen)))
				continue
			}
			refs = append(refs, readRef{point: p, item: it})
		}
	}
	out = append(out, badPre...)
	if len(refs) == 0 {
		return out, nil
	}

	items := make([]s7.Item, len(refs))
	for i, r := range refs {
		items[i] = r.item
	}
	chunks := planChunks(items, c.pduLen, false)

	var firstErr error
	okSamples := 0

	for ci, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.deviceID, "context done", true, err)
			if firstErr == nil {
				firstErr = e
			}
			for _, k := range chunk {
				out = append(out, errSample(refs[k].point.ID, e))
			}
			continue
		}
		pdu, err := s7.BuildRead(c.nextRef(), itemsOf(refs, chunk))
		if err != nil {
			e := driver.E(driver.ErrInternal, "encode", c.deviceID, err.Error(), false, err)
			if firstErr == nil {
				firstErr = e
			}
			for _, k := range chunk {
				out = append(out, errSample(refs[k].point.ID, e))
			}
			continue
		}
		resp, err := c.roundTrip(ctx, pdu)
		if err != nil {
			// 链路级错误：本块与后续所有块都不必再试。把剩余点位显式标坏，
			// 而不是让它们「消失」——上层看到缺失点会二次判坏，但错误文案
			// 会变成「point missing」，掩盖真正的断链原因。
			//
			// 上抛的必须是**链路错误本身**：不能被本块之前某个点级错误
			// （如「对象不存在」→ address）顶掉。collector 按 kind 决定是否
			// 重连，拿到 address 就不会重连，设备会一直卡在「点配错」的假象
			// 里而真正断掉的链路永不重建（§3.3.1：链路级失败立即返回该错误）。
			for _, rest := range chunks[ci:] {
				for _, k := range rest {
					out = append(out, errSample(refs[k].point.ID, err))
				}
			}
			return out, err
		}
		values, errs, perr := s7.ParseReadAck(resp, itemsOf(refs, chunk))
		if perr != nil {
			e := driver.E(driver.ErrProtocol, "decode", c.deviceID, perr.Error(), true, perr)
			if firstErr == nil {
				firstErr = e
			}
			for _, k := range chunk {
				out = append(out, errSample(refs[k].point.ID, e))
			}
			continue
		}
		for j, k := range chunk {
			p := refs[k].point
			if errs[j] != nil {
				out = append(out, errSample(p.ID, errs[j]))
				if firstErr == nil {
					firstErr = errs[j]
				}
				continue
			}
			sample := decodeSample(c.deviceID, p, refs[k].item, values[j])
			if sample.Quality == model.QualityGood {
				okSamples++
			}
			out = append(out, sample)
		}
	}
	// 地址类错误不应因为「有别的点位成功」而被完全吞掉，但也不该让一台
	// 设备整体判故障；与 Modbus 保持一致：全部失败才向上返回错误。
	if okSamples == 0 && firstErr != nil {
		return out, firstErr
	}
	return out, nil
}

// Write 逐项编码、按 PDU 预算成组发送。
//
// 逐项返回是硬契约（§3.4 规则 1）：**每个请求恰好一个结果**，不重复、不消失。
// 因此这里按请求下标预分配槽位，每个失败分支只填「还没报告过」的槽位，
// 最后按请求顺序收拢——链路在第 N 块断掉时，后续条目的结果仍然是显式的
// 失败项，而不是从切片里凭空少掉（上层会把缺失项兜底成「driver returned
// no result」，真正的原因就此丢失）。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	type pending struct {
		req   driver.WriteRequest
		item  s7.Item
		value []byte
		idx   int // 在 reqs 中的下标
	}
	slots := make([]driver.WriteResult, len(reqs))
	reported := make([]bool, len(reqs))
	var valid []pending
	report := func(idx int, r driver.WriteResult) {
		if reported[idx] {
			return
		}
		r.PointID = reqs[idx].PointID
		slots[idx] = r
		reported[idx] = true
	}
	fail := func(idx int, quality model.Quality, err error) {
		report(idx, driver.WriteResult{OK: false, Quality: quality, Err: err})
	}
	failSpan := func(from int, quality model.Quality, err error) {
		for i := from; i < len(valid); i++ {
			fail(valid[i].idx, quality, err)
		}
	}
	collect := func() []driver.WriteResult {
		out := make([]driver.WriteResult, 0, len(reqs))
		for i := range reqs {
			if !reported[i] {
				// 理论上不可达（每个分支都填了槽位）。留一个显式诊断而不是
				// 零值 WriteResult，避免「静默成功」这种最坏的兜底。
				out = append(out, driver.WriteResult{
					PointID: reqs[i].PointID, Quality: model.QualityBad,
					Err: driver.E(driver.ErrInternal, "write", c.deviceID,
						"internal: no result recorded for this request", false, nil),
				})
				continue
			}
			out = append(out, slots[i])
		}
		return out
	}

	for i, req := range reqs {
		it, v, err := encodeWrite(req)
		if err != nil {
			fail(i, qualityOf(err), err)
			continue
		}
		// 单项写请求本身超过协商 PDU：切块装不下，对端只会断连或超时。
		// 地址配置错（address.length 太大），本地判坏。
		if ok, need := fitsPDU(it, c.pduLen, true); !ok {
			fail(i, model.QualityAddressError, pduBudgetErr("write", c.deviceID, need, c.pduLen))
			continue
		}
		valid = append(valid, pending{req: req, item: it, value: v, idx: i})
	}
	if len(valid) == 0 {
		return collect(), nil
	}

	all := make([]s7.Item, len(valid))
	for i, p := range valid {
		all[i] = p.item
	}
	chunks := planChunks(all, c.pduLen, true)

	var firstErr error
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "write", c.deviceID, "context done", true, err)
			for _, k := range chunk {
				fail(valid[k].idx, model.QualityTimeout, e)
			}
			// 链路/上下文已失效：剩下的块不必再逐个等超时。
			failSpan(chunk[len(chunk)-1]+1, model.QualityTimeout, e)
			if firstErr == nil {
				firstErr = e
			}
			return collect(), firstErr
		}
		witems := make([]s7.Item, 0, len(chunk))
		wvals := make([][]byte, 0, len(chunk))
		for _, k := range chunk {
			witems = append(witems, valid[k].item)
			wvals = append(wvals, valid[k].value)
		}
		pdu, err := s7.BuildWrite(c.nextRef(), witems, wvals)
		if err != nil {
			e := driver.E(driver.ErrInternal, "encode", c.deviceID, err.Error(), false, err)
			if firstErr == nil {
				firstErr = e
			}
			for _, k := range chunk {
				fail(valid[k].idx, model.QualityBad, e)
			}
			continue
		}
		resp, err := c.roundTrip(ctx, pdu)
		if err != nil {
			// 链路级失败：本块与后续块无法再试。已成功的块保持成功
			// （§3.4 规则 4），剩余条目显式标坏并从结果里返回。
			failSpan(chunk[0], model.QualityCommError, err)
			return collect(), err
		}
		errs, perr := s7.ParseWriteAck(resp, len(chunk))
		if perr != nil {
			e := driver.E(driver.ErrProtocol, "decode", c.deviceID, perr.Error(), true, perr)
			if firstErr == nil {
				firstErr = e
			}
			for _, k := range chunk {
				fail(valid[k].idx, model.QualityBad, e)
			}
			continue
		}
		for j, k := range chunk {
			if errs[j] != nil {
				fail(valid[k].idx, qualityOf(errs[j]), errs[j])
				if firstErr == nil {
					firstErr = errs[j]
				}
				continue
			}
			report(valid[k].idx, driver.WriteResult{OK: true, Quality: model.QualityGood})
		}
	}
	return collect(), firstErr
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stream == nil {
		return nil
	}
	c.closed = true
	err := c.stream.Close()
	c.stream = nil
	return err
}

// ---------------------------------------------------------------------------
// 编解码辅助
// ---------------------------------------------------------------------------

func itemsOf(refs []readRef, idx []int) []s7.Item {
	out := make([]s7.Item, 0, len(idx))
	for _, k := range idx {
		out = append(out, refs[k].item)
	}
	return out
}

// PDU 预算里各部分的固定开销。请求侧：S7 头 10 + 参数头 2；每一项还要
// 12 字节的变量说明（参数区）；写请求的数据区每项多一个 4 字节数据头。
// 响应侧：读响应每项是 4 字节的返回码+传输尺寸+长度，再加数据本身（按偶数
// 对齐）；写响应每项只有 1 字节返回码。
const (
	pduHeader    = 10
	pduParamHead = 2
	pduItemSpec  = 12
	pduItemHdr   = 4
	pduMaxItems  = 20
)

// itemBudget 返回单项在请求区与响应区各占用多少字节。
func itemBudget(it s7.Item, write bool) (req, resp int) {
	width := it.Bytes
	if it.BitAccess {
		width = 1
	}
	if width < 0 {
		width = 0
	}
	// 数据区按偶数字节对齐。
	dataWidth := width + (width % 2)
	req = pduItemSpec
	resp = pduItemHdr + dataWidth
	if write {
		req += pduItemHdr + dataWidth
		resp += 1
	}
	return req, resp
}

// fitsPDU 判断单项（单独成块、独占一帧时）能否装进协商的 PDU。
// need 是该项所需的最大字节数，用于诊断文案。
func fitsPDU(it s7.Item, pduLen int, write bool) (ok bool, need int) {
	if pduLen <= 0 {
		pduLen = 480
	}
	req, resp := itemBudget(it, write)
	need = pduHeader + pduParamHead + req
	if r := pduHeader + pduParamHead + resp; r > need {
		need = r
	}
	return need <= pduLen, need
}

// pduBudgetErr 是「单项超过协商 PDU」的诊断。
//
// 这是**地址配置错**而不是链路故障：`address.length` 要求的字节数超过了
// 对端能承载的帧长。发出去的对端行为不可预期（S7 常见的表现是干脆不回，
// 于是从超时反推原因非常困难），因此本地判坏，归 ErrAddress：不重试、
// 不熔断整台设备。
func pduBudgetErr(op, deviceID string, need, pduLen int) error {
	return driver.E(driver.ErrAddress, op, deviceID,
		fmt.Sprintf("item needs %d bytes of PDU budget, negotiated PDU is %d (address.length too large)", need, pduLen),
		false, nil)
}

// planChunks 按「项数 ≤ 20」与「请求/响应不超过协商 PDU」两个约束切块。
//
// 预算按最坏情况估算而不是发送后看错误：S7 的 PDU 超限通常表现为对端
// 直接不响应（而不是返回一个明确的「太长」错误码），从超时反推原因
// 非常困难。宁可多发一帧，也不要赌对面会不会接受。
//
// 单项本身就超预算的情况由调用方先用 fitsPDU 拦下（见 Read/Write）；
// 本函数不负责拒绝，只负责在「装得下」的项之间切分。
func planChunks(items []s7.Item, pduLen int, write bool) [][]int {
	if pduLen <= 0 {
		pduLen = 480
	}
	var (
		chunks [][]int
		cur    []int
		req    = pduHeader + pduParamHead
		resp   = pduHeader + pduParamHead
	)
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
			req = pduHeader + pduParamHead
			resp = pduHeader + pduParamHead
		}
	}
	for i, it := range items {
		addReq, addResp := itemBudget(it, write)
		if len(cur) >= pduMaxItems || (len(cur) > 0 && (req+addReq > pduLen || resp+addResp > pduLen)) {
			flush()
		}
		cur = append(cur, i)
		req += addReq
		resp += addResp
	}
	flush()
	return chunks
}

// decodeSample 把响应里的原始字节解成平台值。
//
// 解码失败归 ErrAddress 而不是 ErrProtocol：响应长度与点位声明的类型/
// 宽度不符，绝大多数是**点位配置错**（类型写错、address.length 与类型
// 不匹配），换一个点照样能读。归 ErrProtocol 会熔断整台设备。
func decodeSample(deviceID string, p driver.PointRef, it s7.Item, raw []byte) driver.RawSample {
	if it.BitAccess {
		// 位访问返回 1 字节，最低位即值；有的 CPU 会把整个字节的其它位
		// 也带上来，所以只取 bit0。
		return driver.RawSample{
			PointID: p.ID,
			Value:   model.BoolValue(len(raw) > 0 && raw[0]&0x01 != 0),
			Quality: model.QualityGood,
		}
	}
	if p.DataType == model.TypeString {
		return decodeS7String(deviceID, p, raw)
	}
	v, err := value.Decode(raw, p.DataType, p.ByteOrder)
	if err != nil {
		return errSample(p.ID, driver.E(driver.ErrAddress, "decode", deviceID, err.Error(), false, err))
	}
	return driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood}
}

// decodeS7String 解 DB STRING：byte[0]=声明的最大长度，byte[1]=当前实际
// 长度，其后是字符。
//
// 西门子 DB 里的 STRING[n] 占 2+n 字节，头两字节就是这两个长度。早先把
// 整段按裸字节转文本——头两字节（0x07 0x07 这样的控制字符）被并进文本，
// 接真实 CPU 时读出来的是乱码；内置从站存裸字节，回环测不出这个错。
// cur 超出地址宽度是设备侧的坏数据：报地址错而不是按声明截断——截断值
// 会被当好值上报。
func decodeS7String(deviceID string, p driver.PointRef, raw []byte) driver.RawSample {
	if len(raw) < 2 {
		return errSample(p.ID, driver.E(driver.ErrAddress, "decode", deviceID,
			fmt.Sprintf("string needs address.length >= 2 (max+cur header), got %d", len(raw)), false, nil))
	}
	cur := int(raw[1])
	if cur > len(raw)-2 {
		return errSample(p.ID, driver.E(driver.ErrAddress, "decode", deviceID,
			fmt.Sprintf("string current length %d exceeds area width %d (max declared %d)",
				cur, len(raw), raw[0]), false, nil))
	}
	return driver.RawSample{
		PointID: p.ID,
		Value:   model.TextValue(model.TypeString, string(raw[2:2+cur])),
		Quality: model.QualityGood,
	}
}

func errSample(pointID string, err error) driver.RawSample {
	return driver.RawSample{
		PointID: pointID,
		Quality: qualityOf(err),
		Err:     err,
	}
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrUnsupported, driver.ErrPermission:
		return model.QualityBad
	}
	return model.QualityBad
}

func classifyNetErr(deviceID, op string, err error) error {
	if err == nil {
		return nil
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return driver.E(driver.ErrTimeout, op, deviceID, "network timeout", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return driver.E(driver.ErrTimeout, op, deviceID, "deadline exceeded", true, err)
	}
	if errors.Is(err, errClosedStream) {
		return driver.E(driver.ErrDisconnected, op, deviceID, "connection closed", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, deviceID, "network error", true, err)
}

var errClosedStream = errors.New("transport: closed")

// encodeWrite 把一次写请求转换成 S7 变量 + 数据字节。
func encodeWrite(req driver.WriteRequest) (s7.Item, []byte, error) {
	it, err := s7.ItemOf(driver.PointRef{
		ID: req.PointID, Address: req.Address, DataType: req.DataType,
		ByteOrder: req.ByteOrder, Length: req.Address.Length,
	})
	if err != nil {
		return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "", err.Error(), false, err)
	}
	if it.BitAccess {
		var b byte
		if req.Value.Type == model.TypeBool {
			if req.Value.Bool {
				b = 1
			}
		} else if req.Value.Num != 0 {
			b = 1
		}
		return it, []byte{b}, nil
	}
	switch req.DataType {
	case model.TypeBytes:
		if it.Bytes <= 0 {
			return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "",
				fmt.Sprintf("point %q needs explicit address.length for %s", req.PointID, req.DataType), false, nil)
		}
		// 超长必须拒绝，不能截断：把 10 个字符写进 4 字节地址只发出 "ABCD"，
		// 调用方却收到成功——设备上的值已经不是调用方给的值，而且现场看到
		// 的是「写成功了、值不对」，排查成本极高（§3.4 规则 3）。
		// 与 FINS 的 STRING/bytes 处理一致：恰好填满照常写（按宽度补零）。
		raw := []byte(req.Value.Text)
		if len(raw) > it.Bytes {
			return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "",
				fmt.Sprintf("point %q value is %d bytes, address.length gives only %d bytes",
					req.PointID, len(raw), it.Bytes), false, nil)
		}
		out := make([]byte, it.Bytes)
		copy(out, raw)
		return it, out, nil
	case model.TypeString:
		// DB STRING 带 2 字节头（max + cur）：address.length 是含头的总宽，
		// 与 TIA Portal 的 STRING[n] = 2+n 字节一致。写 [max, cur, 字符...]：
		// 清空 max/cur 的裸字节写会让 CPU 把字符串长度当 0——「写成功、
		// 读回全空」。裸字节语义走 bytes 类型（不含头，原样写）。
		if it.Bytes < 2 {
			return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "",
				fmt.Sprintf("point %q: string needs address.length >= 2 (max+cur header), got %d",
					req.PointID, it.Bytes), false, nil)
		}
		raw := []byte(req.Value.Text)
		if len(raw) > it.Bytes-2 {
			return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "",
				fmt.Sprintf("point %q value is %d chars, address.length %d leaves only %d after the 2-byte header",
					req.PointID, len(raw), it.Bytes, it.Bytes-2), false, nil)
		}
		out := make([]byte, it.Bytes)
		out[0] = byte(it.Bytes - 2)
		out[1] = byte(len(raw))
		copy(out[2:], raw)
		return it, out, nil
	}
	// 数值范围校验必须在编码之前：value.Encode 用强制转换落地，70000 写
	// int16 会静默变成 4464，1e300 写 float32 会变成 +Inf（§3.4 规则 3）。
	if err := checkRange(req.DataType, req.Value.Num); err != nil {
		return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "", err.Error(), false, err)
	}
	raw, err := value.Encode(req.Value, req.DataType, req.ByteOrder)
	if err != nil {
		// 编码失败是值/类型配置错（类型不支持、值形状不对），与越界同类：
		// 归 ErrAddress，不熔断、不重试。
		return s7.Item{}, nil, driver.E(driver.ErrAddress, "encode", "", err.Error(), false, err)
	}
	if len(raw) != it.Bytes {
		return s7.Item{}, nil, driver.E(driver.ErrInternal, "encode", "",
			fmt.Sprintf("encoded %d bytes for %s, address expects %d", len(raw), req.DataType, it.Bytes), false, nil)
	}
	return it, raw, nil
}

// checkRange 拒绝越界与带小数的整数写入，以及 float32 溢出成 ±Inf / 下溢成 0。
//
// 与 finscore/mc 的 checkRange 同构：codec/value.Encode 无范围校验，越界值
// 会被强制转换悄悄改成另一个合法值（70000 → 4464），调用方却收到成功。
func checkRange(dt model.DataType, v float64) error {
	switch dt {
	case model.TypeInt16:
		if v != math.Trunc(v) || v < math.MinInt16 || v > math.MaxInt16 {
			return fmt.Errorf("%v 超出 int16 范围或不是整数", v)
		}
	case model.TypeUint16:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint16 {
			return fmt.Errorf("%v 超出 uint16 范围或不是整数", v)
		}
	case model.TypeInt32:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return fmt.Errorf("%v 超出 int32 范围或不是整数", v)
		}
	case model.TypeUint32:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint32 {
			return fmt.Errorf("%v 超出 uint32 范围或不是整数", v)
		}
	case model.TypeInt64:
		if v != math.Trunc(v) || v < math.MinInt64 || v >= 9223372036854775808.0 {
			return fmt.Errorf("%v 超出 int64 范围或不是整数", v)
		}
	case model.TypeUint64:
		if v != math.Trunc(v) || v < 0 || v >= 18446744073709551616.0 {
			return fmt.Errorf("%v 超出 uint64 范围或不是整数", v)
		}
	case model.TypeFloat32:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%v 不是合法的 float32 值", v)
		}
		if v != 0 {
			f := float32(v)
			if math.IsInf(float64(f), 0) || f == 0 {
				return fmt.Errorf("%v 超出 float32 可表示范围", v)
			}
		}
	case model.TypeFloat64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%v 不是合法的 float64 值", v)
		}
	}
	return nil
}
