// Package snmp 实现 SNMP v1/v2c 主站驱动（UDP 161）。
//
// 这一层只做三件事：把点位的 OID 组织成 GET / SET 事务、把 varbind 的
// 线类型翻译成平台数据类型、把 error-status 与 v2c 异常标记翻译成驱动
// 错误分类。报文结构本身在 codec/snmp 里。
//
// 三条设计取舍：
//
//  1. **只做 v1/v2c。** v3 的 USM 认证与加密是另一套报文结构与密钥管理，
//     在配置期显式拒绝（见 codec.ParseVersion），而不是退回 v2c 碰运气。
//  2. **读用一条 GET 带多个 varbind**，并按**编码后的字节预算**分批
//     （不是只按个数）。SNMP 跑在 UDP 上，超过 MTU 的报文会被分片；现场
//     大量设备与防火墙直接丢分片，症状是「OID 少的时候能读，点表一多就
//     超时」。按 1400 字节估计可以覆盖最常见的 1500 MTU。
//  3. **v1 的 noSuchName 逐点重发。** v1 没有 per-varbind 异常：一个 OID
//     不存在会让整批返回 noSuchName，error-index 指向它，但**其余 OID 的
//     值也一起丢了**。如果不退化重发，一个写错的 OID 会让同批所有点位
//     变成地址错误，看起来像「设备突然不认所有 OID」。
//
// 不做的部分同样明确：不接收 Trap（轮询主站的采集语义与「设备主动推」
// 不同）、不做 MIB 编译/名字解析（需要一整套 MIB 文件与解析器）、不做
// Walk 作为生产采集（自检 CLI 里的遍历是诊断工具，不是点表语义）。
package snmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// sysDescrOID 是自检读的对象（设备描述）。用它的理由：所有代理都必须
// 实现 system 组；即使视图 ACL 只允许读别的子树，返回 noSuchObject 也
// 证明「报文能到、community 对、代理在回」。
var sysDescrOID = snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0")

const (
	// defaultCommunity 是最常见的只读团体名。默认值不是「安全默认」，
	// 而是「现场第一次接通最少配置」；文档与配置表单都要求显式确认。
	defaultCommunity = "public"
	// defaultRetries 是超时后的额外重发次数。UDP 丢包常见，但 SNMP 代理
	// 通常是单线程处理，重试太多只会把一轮采集拖长。
	defaultRetries = 2
	// defaultMaxOids 是一帧内的 OID 个数上限（防御性上限，不是协议限制）。
	defaultMaxOids = 32
	// maxRequestBytes 是我们主动发送的报文预算。
	maxRequestBytes = 1400
)

// Dialer 允许测试注入内存数据报链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error)

// Driver 是 SNMP v1/v2c 主站驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 UDP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return transport.DialUDP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明 SNMP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "snmp",
		Name:         "SNMP v1/v2c",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"udp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:161", Scope: "channel",
				Help: "SNMP 代理地址；省略端口时按 161 处理"},
			{Name: "labels.community", Label: "团体名", Type: "password", Default: defaultCommunity,
				Scope: "device",
				Help:  "v1/v2c 的 community，等同于只读/读写口令；界面按凭据掩码显示。"},
			{Name: "labels.version", Label: "版本", Type: "enum", Default: "2c", Scope: "device",
				Enum: []string{"1", "2c"},
				Help: "1 没有 per-varbind 异常与 GetBulk；2c 是默认值。v3（USM）未经实现，配置会被拒绝"},
			{Name: "labels.options", Label: "高级选项", Type: "string", Default: "", Scope: "device",
				Help: "retries=0..10、maxOids=1..128（每帧 OID 上限，实际还受 1400 字节预算约束）"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.path", Label: "OID", Type: "string", Required: true, Scope: "point",
				Placeholder: "1.3.6.1.2.1.1.1.0",
				Help:        "点分十进制对象标识符，通常以实例号结尾（.0 表示标量实例）"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true, Scope: "point",
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "string", "bytes"},
				Help: "决定 varbind 的线类型如何翻译：数值型接受 INTEGER/Counter/Gauge/TimeTicks，string/bytes 接受 OCTET STRING"},
		},
		Description: "SNMP v1/v2c 主站：按 OID 批量 GET 与单点 SET，写 unsigned 用 Gauge32、写 string 用 OCTET STRING",
	}
}

// Options 是设备级参数。
type Options struct {
	Community string
	Version   snmpcodec.Version
	Retries   int
	MaxOids   int
}

// Open 建立 UDP 套接字并解析设备参数。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "udp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("snmp requires udp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// 只在「没有冒号」时补默认端口；IPv6 字面量必须带方括号，
		// 其它写法交给 ResolveUDPAddr 保留原始错误。
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, snmpcodec.DefaultPort)
		}
	}
	opt, err := parseOptions(cfg.Device)
	if err != nil {
		return nil, err
	}
	conn, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	return &connState{
		cfg:       cfg,
		conn:      conn,
		addr:      addr,
		community: opt.Community,
		version:   opt.Version,
		retries:   opt.Retries,
		maxOids:   opt.MaxOids,
	}, nil
}

// parseOptions 读取设备标签里的 community / version / options。
func parseOptions(dev model.Device) (Options, error) {
	opt := Options{
		Community: defaultCommunity,
		Version:   snmpcodec.V2c,
		Retries:   defaultRetries,
		MaxOids:   defaultMaxOids,
	}
	if dev.Labels == nil {
		return opt, nil
	}
	if s, ok := dev.Labels["community"]; ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				"labels.community 为空；v1/v2c 必须有团体名", false, nil)
		}
		if len(s) > 255 {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("labels.community 长度 %d 超过 255 字节", len(s)), false, nil)
		}
		opt.Community = s
	}
	if s, ok := dev.Labels["version"]; ok {
		v, err := snmpcodec.ParseVersion(s)
		if err != nil {
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID, err.Error(), false, err)
		}
		opt.Version = v
	}
	for _, part := range strings.Split(dev.Labels["options"], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "retries":
			if !hasVal {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.retries 需要 =N", false, nil)
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 10 {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.retries %q 必须是 0..10", val), false, nil)
			}
			opt.Retries = n
		case "maxoids":
			if !hasVal {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.maxOids 需要 =N", false, nil)
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > 128 {
				return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.maxOids %q 必须是 1..128", val), false, nil)
			}
			opt.MaxOids = n
		default:
			return opt, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("未知的 options 项 %q（支持 retries / maxOids）", key), false, nil)
		}
	}
	return opt, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type connState struct {
	cfg       driver.OpenConfig
	conn      transport.Datagram
	addr      string
	community string
	version   snmpcodec.Version
	retries   int
	maxOids   int

	// mu 串行化整条会话：request-id 配对在 UDP 上必须唯一，交错事务
	// 会让迟到的应答被错误地当成当前事务的结果。
	mu    sync.Mutex
	reqID int32
}

func (c *connState) Close() error { return c.conn.Close() }

// nextID 返回下一个非零 request-id。
func (c *connState) nextID() int32 {
	c.reqID++
	if c.reqID == 0 {
		c.reqID = 1
	}
	return c.reqID
}

// Probe 读 sysDescr.0，收到任何应答即认为链路通。
//
// 为什么 noSuchObject 也算通：它证明报文到达了代理、community 被接受、
// 代理回了应答。真正失败只有两种——没有任何报文（超时）或报文结构错，
// 两者都会在 exchange 里变成分类错误。
func (c *connState) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID()
	req, err := snmpcodec.GetRequest(c.version, c.community, id, []snmpcodec.OID{sysDescrOID})
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	if _, err := c.exchange(ctx, req, id, "probe"); err != nil {
		if driver.KindOf(err) == driver.ErrTimeout {
			return driver.E(driver.ErrTimeout, "probe", c.cfg.Device.ID,
				"no response from agent; check community and view/ACL", true, err)
		}
		return err
	}
	return nil
}

// exchange 发送一条报文并等待 request-id 相同的 Response。
func (c *connState) exchange(ctx context.Context, payload []byte, id int32, op string) (snmpcodec.Message, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return snmpcodec.Message{}, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
		}
		if err := c.conn.SetDeadline(c.deadline(ctx)); err != nil {
			return snmpcodec.Message{}, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"set udp deadline", true, err)
		}
		if err := c.conn.Write(payload); err != nil {
			return snmpcodec.Message{}, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"udp write failed", true, err)
		}
		for {
			datagram, err := c.conn.Read()
			if err != nil {
				lastErr = c.readError(op, err, ctx)
				break
			}
			m, perr := snmpcodec.Parse(datagram)
			if perr != nil {
				return snmpcodec.Message{}, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"malformed SNMP datagram: "+perr.Error(), false, perr)
			}
			// 只接受与我们 request-id 配对的 Response：UDP 上可能收到
			// 上一次事务迟到的应答，也可能收到设备发的 Trap。
			if m.PDU.Type != snmpcodec.PDUResponse || m.PDU.RequestID != id {
				continue
			}
			return m, nil
		}
		// 只有超时值得重发；协议/链路错误立即上报。
		if driver.KindOf(lastErr) != driver.ErrTimeout || ctx.Err() != nil {
			return snmpcodec.Message{}, lastErr
		}
	}
	return snmpcodec.Message{}, lastErr
}

func (c *connState) deadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.cfg.Channel.EffectiveTimeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		return dl
	}
	return d
}

func (c *connState) readError(op string, err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID,
			"no response within timeout", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "udp read failed", true, err)
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// readItem 是「点位下标 + 解析后的 OID」。点位下标用于把结果放回原顺序：
// 分批会打乱事务顺序，但调用方看到的样本顺序必须与请求一致。
type readItem struct {
	pos int
	oid snmpcodec.OID
}

func (c *connState) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var points []driver.PointRef
	for _, span := range spans {
		points = append(points, span.Points...)
	}
	out := make([]driver.RawSample, len(points))
	filled := make([]bool, len(points))

	items := make([]readItem, 0, len(points))
	for i, p := range points {
		oid, err := c.oidOf(p)
		if err != nil {
			out[i] = errSample(p.ID, err)
			filled[i] = true
			continue
		}
		items = append(items, readItem{pos: i, oid: oid})
	}

	var firstErr error
	// 任一链路级失败后，后续批次不可能成功；统一标记并停止。
	failRest := func(from int, err error) {
		if firstErr == nil {
			firstErr = err
		}
		for _, it := range items[from:] {
			if !filled[it.pos] {
				out[it.pos] = errSample(points[it.pos].ID, err)
				filled[it.pos] = true
			}
		}
	}

	batch := make([]readItem, 0, c.maxOids)
	start := 0
	estimate := 48 // 报文头 + PDU 头的粗略上界

	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		res, linkErr := c.readBatch(ctx, points, batch)
		if linkErr != nil {
			failRest(start, linkErr)
			return false
		}
		for _, r := range res {
			if r.err != nil {
				out[r.pos] = errSample(points[r.pos].ID, r.err)
				// 地址错误是点表问题，不该成为整轮采集的 firstErr。
				if firstErr == nil && driver.KindOf(r.err) != driver.ErrAddress {
					firstErr = r.err
				}
			} else {
				out[r.pos] = driver.RawSample{PointID: points[r.pos].ID, Value: r.value, Quality: model.QualityGood}
			}
			filled[r.pos] = true
		}
		batch = batch[:0]
		estimate = 48
		return true
	}

	for i, it := range items {
		size := estimateOID(it.oid) + 4
		if len(batch) > 0 && (len(batch) >= c.maxOids || estimate+size > maxRequestBytes) {
			if !flush() {
				return out, firstErr
			}
			start = i
		}
		batch = append(batch, it)
		estimate += size
	}
	flush()
	return out, firstErr
}

// estimateOID 是 OID 编码长度的保守上界：TLV 头 2 字节 + 每子标识符 5 字节。
func estimateOID(o snmpcodec.OID) int {
	return 2 + 5*(len(o)-1) + 2 // 最后 +2 是 varbind 的 SEQUENCE 头
}

type batchResult struct {
	pos   int
	value model.Value
	err   error
}

// readBatch 读一批 OID。返回的 linkErr 表示链路级失败（超时/断链），
// 调用方据此停止后续批次；协议/地址级问题放在 batchResults 里逐点返回。
func (c *connState) readBatch(ctx context.Context, points []driver.PointRef, batch []readItem) ([]batchResult, error) {
	oids := make([]snmpcodec.OID, len(batch))
	for i, it := range batch {
		oids[i] = it.oid
	}
	id := c.nextID()
	req, err := snmpcodec.GetRequest(c.version, c.community, id, oids)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, id, "read")
	if err != nil {
		switch driver.KindOf(err) {
		case driver.ErrTimeout, driver.ErrDisconnected:
			return nil, err
		}
		// 协议/内部错误：整批标记，但不当作链路故障，继续下一批。
		res := make([]batchResult, len(batch))
		for i, it := range batch {
			res[i] = batchResult{pos: it.pos, err: err}
		}
		return res, nil
	}

	if st := snmpcodec.ErrorStatus(resp.PDU.ErrorStatus); st != snmpcodec.ErrNoError {
		if c.version == snmpcodec.V1 && st == snmpcodec.ErrNoSuchName {
			return c.readOneByOne(ctx, points, batch)
		}
		e := c.statusError("read", st, nil, resp.PDU.ErrorIndex)
		res := make([]batchResult, len(batch))
		for i, it := range batch {
			res[i] = batchResult{pos: it.pos, err: e}
		}
		return res, nil
	}
	if len(resp.PDU.Varbinds) != len(batch) {
		e := driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("agent returned %d varbinds for %d OIDs", len(resp.PDU.Varbinds), len(batch)), false, nil)
		res := make([]batchResult, len(batch))
		for i, it := range batch {
			res[i] = batchResult{pos: it.pos, err: e}
		}
		return res, nil
	}
	res := make([]batchResult, len(batch))
	for i, it := range batch {
		res[i] = c.decodeVarbind(points[it.pos], it.pos, it.oid, resp.PDU.Varbinds[i])
	}
	return res, nil
}

// readOneByOne 是 v1 noSuchName 的退化路径：逐点重发，把「哪一个 OID
// 不存在」与「其余 OID 的值」重新分开。
func (c *connState) readOneByOne(ctx context.Context, points []driver.PointRef, batch []readItem) ([]batchResult, error) {
	res := make([]batchResult, 0, len(batch))
	for i, it := range batch {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
			for _, rest := range batch[i:] {
				res = append(res, batchResult{pos: rest.pos, err: e})
			}
			return res, nil
		}
		id := c.nextID()
		req, err := snmpcodec.GetRequest(c.version, c.community, id, []snmpcodec.OID{it.oid})
		if err != nil {
			res = append(res, batchResult{pos: it.pos, err: driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)})
			continue
		}
		resp, err := c.exchange(ctx, req, id, "read")
		if err != nil {
			if driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected {
				for _, rest := range batch[i:] {
					res = append(res, batchResult{pos: rest.pos, err: err})
				}
				return res, nil
			}
			res = append(res, batchResult{pos: it.pos, err: err})
			continue
		}
		if st := snmpcodec.ErrorStatus(resp.PDU.ErrorStatus); st != snmpcodec.ErrNoError {
			res = append(res, batchResult{pos: it.pos, err: c.statusError("read", st, it.oid, resp.PDU.ErrorIndex)})
			continue
		}
		if len(resp.PDU.Varbinds) != 1 {
			res = append(res, batchResult{pos: it.pos, err: driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
				fmt.Sprintf("single-OID GET returned %d varbinds", len(resp.PDU.Varbinds)), false, nil)})
			continue
		}
		res = append(res, c.decodeVarbind(points[it.pos], it.pos, it.oid, resp.PDU.Varbinds[0]))
	}
	return res, nil
}

func (c *connState) decodeVarbind(p driver.PointRef, pos int, want snmpcodec.OID, vb snmpcodec.Varbind) batchResult {
	if vb.OID.Compare(want) != 0 {
		return batchResult{pos: pos, err: driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("agent answered OID %s for requested %s", vb.OID, want), false, nil)}
	}
	if vb.Value.IsException() {
		return batchResult{pos: pos, err: driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("OID %s: %s", want, vb.Value.Type), false, nil)}
	}
	v, err := decodeValue(p, vb.Value)
	if err != nil {
		return batchResult{pos: pos, err: err}
	}
	return batchResult{pos: pos, value: v}
}

// decodeValue 按点位数据类型解释 varbind 值。
//
// 关键判断：**线类型由设备决定，而不是由点表决定**。同一个 OID 在
// 不同厂商的设备上可能是 INTEGER、Gauge32 甚至 OCTET STRING；点表的
// dataType 只决定「按什么读」，转换失败必须报错而不是硬塞一个 0。
func decodeValue(p driver.PointRef, v snmpcodec.Value) (model.Value, error) {
	dev := ""
	if p.ID != "" {
		dev = "point " + p.ID + ": "
	}
	switch p.DataType {
	case model.TypeString:
		switch v.Type {
		case snmpcodec.TypeOctetString, snmpcodec.TypeOpaque:
			return model.TextValue(model.TypeString, string(v.Bytes)), nil
		case snmpcodec.TypeOID:
			return model.TextValue(model.TypeString, v.OID.String()), nil
		case snmpcodec.TypeIPAddress:
			return model.TextValue(model.TypeString, net.IP(v.Bytes).String()), nil
		}
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s设备返回 %s，但点位 dataType=string（OCTET STRING/OID/IpAddress 才能按字符串读）", dev, v.Type), false, nil)
	case model.TypeBytes:
		if v.Type != snmpcodec.TypeOctetString && v.Type != snmpcodec.TypeOpaque {
			return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
				"%s设备返回 %s，但点位 dataType=bytes", dev, v.Type), false, nil)
		}
		return model.BytesValue(v.Bytes), nil
	case model.TypeBool:
		switch v.Type {
		case snmpcodec.TypeInteger:
			if v.Int == 0 || v.Int == 1 {
				return model.BoolValue(v.Int == 1), nil
			}
		case snmpcodec.TypeCounter32, snmpcodec.TypeGauge32, snmpcodec.TypeTimeTicks, snmpcodec.TypeUInteger32:
			if v.Uint == 0 || v.Uint == 1 {
				return model.BoolValue(v.Uint == 1), nil
			}
		}
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s点位 dataType=bool，但设备返回 %s=%s（只接受 INTEGER/无符号的 0/1）", dev, v.Type, v), false, nil)
	}

	// 数值型：接受所有整数/无符号/计数器；拒绝字符串与地址类。
	var num float64
	switch v.Type {
	case snmpcodec.TypeInteger:
		num = float64(v.Int)
	case snmpcodec.TypeCounter32, snmpcodec.TypeGauge32, snmpcodec.TypeTimeTicks, snmpcodec.TypeUInteger32, snmpcodec.TypeCounter64:
		// Counter64 经 float64 承载：超过 2^53 会丢精度。平台的数值通道
		// 是 float64，这在设计上就无法避免；文档的边界一节明确写出。
		num = float64(v.Uint)
	case snmpcodec.TypeOctetString, snmpcodec.TypeOpaque:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s设备返回 OCTET STRING（%q），但点位是数值型；把 dataType 改成 string/bytes，或在设备侧换用数值 OID",
			dev, truncate(v.Bytes, 32)), false, nil)
	case snmpcodec.TypeIPAddress, snmpcodec.TypeOID:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s设备返回 %s，不能作为数值；把 dataType 改成 string", dev, v.Type), false, nil)
	case snmpcodec.TypeNull:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", dev+"设备返回 NULL", false, nil)
	default:
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf(
			"%s不支持的 varbind 类型 %s", dev, v.Type), false, nil)
	}
	if math.IsNaN(num) || math.IsInf(num, 0) {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", "", dev+"数值不是有限值", false, nil)
	}
	return model.NumValue(p.DataType, num), nil
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *connState) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]driver.WriteResult, 0, len(reqs))
	for _, req := range reqs {
		if err := ctx.Err(); err != nil {
			out = append(out, driver.WriteResult{PointID: req.PointID,
				Err: driver.E(driver.ErrTimeout, "write", c.cfg.Device.ID, "context done", true, err)})
			continue
		}
		out = append(out, c.writeOne(ctx, req))
	}
	return out, nil
}

func (c *connState) writeOne(ctx context.Context, req driver.WriteRequest) driver.WriteResult {
	res := driver.WriteResult{PointID: req.PointID}
	if req.Address.Kind != model.AddressSymbol {
		res.Err = driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q: snmp requires symbol (OID) addressing, got %q", req.PointID, req.Address.Kind), false, nil)
		return res
	}
	oid, err := parseOID(req.Address)
	if err != nil {
		res.Err = err
		return res
	}
	val, err := encodeWriteValue(req)
	if err != nil {
		res.Err = err
		return res
	}
	id := c.nextID()
	payload, err := snmpcodec.SetRequest(c.version, c.community, id, []snmpcodec.Varbind{{OID: oid, Value: val}})
	if err != nil {
		res.Err = driver.E(driver.ErrInternal, "write", c.cfg.Device.ID, err.Error(), false, err)
		return res
	}
	resp, err := c.exchange(ctx, payload, id, "write")
	if err != nil {
		res.Err = err
		return res
	}
	if st := snmpcodec.ErrorStatus(resp.PDU.ErrorStatus); st != snmpcodec.ErrNoError {
		res.Err = c.statusError("write", st, oid, resp.PDU.ErrorIndex)
		return res
	}
	if len(resp.PDU.Varbinds) != 1 || resp.PDU.Varbinds[0].OID.Compare(oid) != 0 {
		res.Err = driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			"SET response does not echo the written OID", false, nil)
		return res
	}
	// 标准要求 SET 应答回显写入后的值。校验它能把「设备接受了请求
	// 但按自己的解释改了值」（例如把 1 写成 100）当场暴露出来。
	if !valueEqual(resp.PDU.Varbinds[0].Value, val) {
		res.Err = driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("SET echoed %s, want %s (device may have coerced the value)",
				resp.PDU.Varbinds[0].Value, val), false, nil)
		return res
	}
	res.OK = true
	res.Quality = model.QualityGood
	return res
}

// encodeWriteValue 把平台值翻译成 varbind 值。
//
// 映射规则（文档 §3.10.12 有同一张表）：
//   - bool → INTEGER 0/1
//   - int16/int32/int64 → INTEGER
//   - uint16/uint32 → Gauge32（无符号的可写类型）
//   - string/bytes → OCTET STRING
//
// float 与 BCD 显式拒绝：SNMP 没有浮点标准类型，各厂商的私有用
// OCTET STRING 编法互不兼容，猜一个只会写出错误的值。
func encodeWriteValue(req driver.WriteRequest) (snmpcodec.Value, error) {
	dev := ""
	if req.PointID != "" {
		dev = "point " + req.PointID + ": "
	}
	bad := func(format string, args ...any) (snmpcodec.Value, error) {
		return snmpcodec.Value{}, driver.E(driver.ErrUnsupported, "write", "",
			dev+fmt.Sprintf(format, args...), false, nil)
	}
	switch req.DataType {
	case model.TypeBool:
		if req.Value.Bool {
			return snmpcodec.Integer(1), nil
		}
		return snmpcodec.Integer(0), nil
	case model.TypeInt16:
		n, err := intValue(req.Value, math.MinInt16, math.MaxInt16)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Integer(n), nil
	case model.TypeInt32:
		n, err := intValue(req.Value, math.MinInt32, math.MaxInt32)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Integer(n), nil
	case model.TypeInt64:
		n, err := intValue(req.Value, math.MinInt64, math.MaxInt64)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Integer(n), nil
	case model.TypeUint16:
		n, err := uintValue(req.Value, 0xFFFF)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Unsigned(snmpcodec.TypeGauge32, n), nil
	case model.TypeUint32:
		n, err := uintValue(req.Value, 0xFFFFFFFF)
		if err != nil {
			return bad("%v", err)
		}
		return snmpcodec.Unsigned(snmpcodec.TypeGauge32, n), nil
	case model.TypeUint64:
		n, err := uintValue(req.Value, 0xFFFFFFFF)
		if err != nil {
			return bad("%v（SNMP 没有 64 位可写无符号整型；大于 2^32-1 的值只能只读）", err)
		}
		return snmpcodec.Unsigned(snmpcodec.TypeGauge32, n), nil
	case model.TypeString:
		return snmpcodec.String(req.Value.Text), nil
	case model.TypeBytes:
		return snmpcodec.Octets([]byte(req.Value.Text)), nil
	case model.TypeFloat32, model.TypeFloat64:
		return bad("SNMP 没有标准浮点类型；请改用整型 OID，或在设备侧确认私有编码")
	case model.TypeBCD16, model.TypeBCD32:
		return bad("SNMP 没有 BCD 类型；请改用整型 OID")
	}
	return bad("不支持的数据类型 %q", req.DataType)
}

func intValue(v model.Value, min, max int64) (int64, error) {
	if v.Type != model.TypeBool && !v.Type.IsNumeric() {
		return 0, fmt.Errorf("值类型 %q 不是数值", v.Type)
	}
	num, ok := v.AsFloat()
	if !ok {
		return 0, fmt.Errorf("值类型 %q 不能作为数值", v.Type)
	}
	if num != math.Trunc(num) {
		return 0, fmt.Errorf("需要整数值，得到 %v", num)
	}
	// float64 对 int64 两端并不精确；用边界比较时留出让 Convert 安全的
	// 判断空间（超出 [min,max] 一律拒绝，不做饱和截断）。
	if num < float64(min) || num > float64(max) {
		return 0, fmt.Errorf("值 %v 超出范围 %d..%d", num, min, max)
	}
	return int64(num), nil
}

func uintValue(v model.Value, max uint64) (uint64, error) {
	if v.Type == model.TypeBool {
		if v.Bool {
			return 1, nil
		}
		return 0, nil
	}
	if !v.Type.IsNumeric() {
		return 0, fmt.Errorf("值类型 %q 不是数值", v.Type)
	}
	num, ok := v.AsFloat()
	if !ok {
		return 0, fmt.Errorf("值类型 %q 不能作为数值", v.Type)
	}
	if num != math.Trunc(num) {
		return 0, fmt.Errorf("需要整数值，得到 %v", num)
	}
	if num < 0 || num > float64(max) {
		return 0, fmt.Errorf("值 %v 超出范围 0..%d", num, max)
	}
	return uint64(num), nil
}

func valueEqual(a, b snmpcodec.Value) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case snmpcodec.TypeInteger:
		return a.Int == b.Int
	case snmpcodec.TypeCounter32, snmpcodec.TypeGauge32, snmpcodec.TypeTimeTicks,
		snmpcodec.TypeUInteger32, snmpcodec.TypeCounter64:
		return a.Uint == b.Uint
	case snmpcodec.TypeOctetString, snmpcodec.TypeOpaque, snmpcodec.TypeIPAddress:
		return bytes.Equal(a.Bytes, b.Bytes)
	case snmpcodec.TypeOID:
		return a.OID.Compare(b.OID) == 0
	case snmpcodec.TypeNull:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 自检接口
// ---------------------------------------------------------------------------
//
// GetRaw / Walk 只服务于 `uic snmpread`：自检时我们**不知道**点表的
// dataType，也不该猜一个——猜错会把 OCTET STRING 当数值读成 0。因此它们
// 直接返回线类型与原始值，由 CLI 原样展示。
//
// 它们不进入 driver.Conn 接口：生产采集必须走 Read 的类型化路径，否则
// 会绕过「线类型与点表不符就报错」这道检查。

// GetResult 是一组自检读结果。
type GetResult struct {
	OID   snmpcodec.OID
	Value snmpcodec.Value
	Err   error
}

// GetRaw 以原始 varbind 形式读一组 OID。
func (c *connState) GetRaw(ctx context.Context, oids []snmpcodec.OID) ([]GetResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(oids) == 0 {
		return nil, driver.E(driver.ErrUnsupported, "read", c.cfg.Device.ID, "at least one OID is required", false, nil)
	}

	out := make([]GetResult, 0, len(oids))
	for start := 0; start < len(oids); {
		end := start + c.maxOids
		if end > len(oids) {
			end = len(oids)
		}
		// 字节预算与 Read 相同：先按保守上界切，避免一条大 GET 被分片。
		for end > start+1 {
			total := 48
			for _, o := range oids[start:end] {
				total += estimateOID(o) + 4
			}
			if total <= maxRequestBytes {
				break
			}
			end--
		}
		res, linkErr := c.getRawBatch(ctx, oids[start:end])
		out = append(out, res...)
		if linkErr != nil {
			for _, o := range oids[end:] {
				out = append(out, GetResult{OID: o, Err: linkErr})
			}
			return out, linkErr
		}
		start = end
	}
	return out, nil
}

func (c *connState) getRawBatch(ctx context.Context, oids []snmpcodec.OID) ([]GetResult, error) {
	id := c.nextID()
	req, err := snmpcodec.GetRequest(c.version, c.community, id, oids)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, id, "read")
	if err != nil {
		switch driver.KindOf(err) {
		case driver.ErrTimeout, driver.ErrDisconnected:
			return nil, err
		}
		out := make([]GetResult, len(oids))
		for i, o := range oids {
			out[i] = GetResult{OID: o, Err: err}
		}
		return out, nil
	}
	if st := snmpcodec.ErrorStatus(resp.PDU.ErrorStatus); st != snmpcodec.ErrNoError {
		if c.version == snmpcodec.V1 && st == snmpcodec.ErrNoSuchName {
			// 自检时逐点重发，才能把「哪个 OID 不存在」说清楚。
			out := make([]GetResult, 0, len(oids))
			for i, o := range oids {
				id := c.nextID()
				one, err := snmpcodec.GetRequest(c.version, c.community, id, []snmpcodec.OID{o})
				if err != nil {
					out = append(out, GetResult{OID: o, Err: err})
					continue
				}
				r, err := c.exchange(ctx, one, id, "read")
				if err != nil {
					if driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected {
						for _, rest := range oids[i:] {
							out = append(out, GetResult{OID: rest, Err: err})
						}
						return out, err
					}
					out = append(out, GetResult{OID: o, Err: err})
					continue
				}
				if st := snmpcodec.ErrorStatus(r.PDU.ErrorStatus); st != snmpcodec.ErrNoError {
					out = append(out, GetResult{OID: o, Err: c.statusError("read", st, o, r.PDU.ErrorIndex)})
					continue
				}
				if len(r.PDU.Varbinds) != 1 {
					out = append(out, GetResult{OID: o, Err: driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
						"single-OID GET returned a different varbind count", false, nil)})
					continue
				}
				out = append(out, GetResult{OID: r.PDU.Varbinds[0].OID, Value: r.PDU.Varbinds[0].Value})
			}
			return out, nil
		}
		e := c.statusError("read", st, nil, resp.PDU.ErrorIndex)
		out := make([]GetResult, len(oids))
		for i, o := range oids {
			out[i] = GetResult{OID: o, Err: e}
		}
		return out, nil
	}
	if len(resp.PDU.Varbinds) != len(oids) {
		e := driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("agent returned %d varbinds for %d OIDs", len(resp.PDU.Varbinds), len(oids)), false, nil)
		out := make([]GetResult, len(oids))
		for i, o := range oids {
			out[i] = GetResult{OID: o, Err: e}
		}
		return out, nil
	}
	out := make([]GetResult, len(oids))
	for i := range oids {
		out[i] = GetResult{OID: resp.PDU.Varbinds[i].OID, Value: resp.PDU.Varbinds[i].Value}
	}
	return out, nil
}

// Walk 从 prefix 的后继开始按字典序遍历，直到离开该子树或达到 max。
//
// 只给自检用：生产点表必须是明确的 OID，Walk 出来的名字没有 MIB 也
// 无法赋予业务含义。
func (c *connState) Walk(ctx context.Context, prefix snmpcodec.OID, max int) ([]GetResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := prefix.Validate(); err != nil {
		return nil, driver.E(driver.ErrAddress, "walk", c.cfg.Device.ID, err.Error(), false, err)
	}
	if max <= 0 {
		max = 128
	}
	if max > 1024 {
		max = 1024
	}

	var out []GetResult
	cur := prefix
	for len(out) < max {
		if err := ctx.Err(); err != nil {
			return out, driver.E(driver.ErrTimeout, "walk", c.cfg.Device.ID, "context done", true, err)
		}
		id := c.nextID()
		req, err := snmpcodec.GetNextRequest(c.version, c.community, id, []snmpcodec.OID{cur})
		if err != nil {
			return out, driver.E(driver.ErrInternal, "walk", c.cfg.Device.ID, err.Error(), false, err)
		}
		resp, err := c.exchange(ctx, req, id, "walk")
		if err != nil {
			return out, err
		}
		if st := snmpcodec.ErrorStatus(resp.PDU.ErrorStatus); st != snmpcodec.ErrNoError {
			// v1 的表尾就是 noSuchName；这不是错误，是遍历结束。
			if c.version == snmpcodec.V1 && st == snmpcodec.ErrNoSuchName {
				return out, nil
			}
			return out, c.statusError("walk", st, cur, resp.PDU.ErrorIndex)
		}
		if len(resp.PDU.Varbinds) != 1 {
			return out, driver.E(driver.ErrProtocol, "walk", c.cfg.Device.ID,
				fmt.Sprintf("GETNEXT returned %d varbinds", len(resp.PDU.Varbinds)), false, nil)
		}
		vb := resp.PDU.Varbinds[0]
		if vb.Value.Type == snmpcodec.TypeEndOfMibView {
			return out, nil
		}
		if v := vb.OID.Compare(cur); v <= 0 {
			// 代理返回了一个不前进的 OID：继续循环只会挂死。
			return out, driver.E(driver.ErrProtocol, "walk", c.cfg.Device.ID,
				fmt.Sprintf("GETNEXT did not advance past %s (returned %s)", cur, vb.OID), false, nil)
		}
		if !prefix.HasPrefix(vb.OID) {
			return out, nil
		}
		out = append(out, GetResult{OID: vb.OID, Value: vb.Value})
		cur = vb.OID
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func (c *connState) oidOf(p driver.PointRef) (snmpcodec.OID, error) {
	if p.Address.Kind != model.AddressSymbol {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("point %q: snmp requires symbol (OID) addressing, got %q", p.ID, p.Address.Kind), false, nil)
	}
	return parseOID(p.Address)
}

func parseOID(a model.Address) (snmpcodec.OID, error) {
	if a.Index > 0 {
		return nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("OID %q 不支持数组下标 [%d]；实例号应写进 OID 本身（例如 .1.3.6.1.2.1.2.2.1.10.%d）",
				a.Path, a.Index, a.Index), false, nil)
	}
	if a.Selector != "" {
		return nil, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("OID 地址不能带 #%s；selector 只属于 MQTT 载荷字段", a.Selector), false, nil)
	}
	oid, err := snmpcodec.ParseOID(a.Path)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", "", err.Error(), false, err)
	}
	return oid, nil
}

func (c *connState) statusError(op string, st snmpcodec.ErrorStatus, oid snmpcodec.OID, index int) error {
	msg := fmt.Sprintf("agent returned %s", st)
	if index > 0 {
		msg += fmt.Sprintf(" at varbind %d", index)
	}
	if oid != nil {
		msg += " for " + oid.String()
	}
	return driver.E(st.Kind(), op, c.cfg.Device.ID, msg, st.Temporary(), nil)
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
