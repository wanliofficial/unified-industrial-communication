// Package secs 实现半导体设备与工厂通信最常用的 SECS/GEM（HSMS 传输 +
// SECS-II 数据）主站驱动。
//
// 寻址模型：点位地址是「对象类型 + 实例号」，用对象地址表达：
//
//	svid:1001.value   状态变量（SVID），通过 S1F3/S1F4 批量读取
//	ecid:200.value    设备常数（ECID），S2F13/S2F14 读，S2F15/S2F16 写
//
// 为什么用对象地址而不是符号地址：SECS 的编号（SVID/ECID）在标准里就是
// 32 位无符号整数，没有字符串名字（名字在 S1F12 里另行查询）。用对象
// address.kind + instance 承载编号，与 OPC UA 的对象节点、SNMP 的 OID 一
// 样是「协议原生」的寻址，不需要发明一套映射语法。
//
// 读是批量的：一个 ReadSpan 里的多个 svid 点被合并成一次 S1F3（L,n { U4 }），
// 一次 S1F4 应答里带回全部取值，按编号分发回各点。这与 Modbus 的「一次读
// 多个寄存器」是同一个优化：SECS 事务有往返成本，逐点读会让 N 个 SVID 花
// N 次握手。
//
// 写只有设备常数：SVID 在 SECS/GEM 里是「只读状态」，写它要走 S2F49/S2F41
// 那套「远程命令 + 应答确认」的复杂流程，不在最小范围内。因此本驱动只把
// ecid 暴露为可写（S2F15/S2F16，ACKC7=0 为接受），svid 一律只读。
package secs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	secscodec "uic/internal/codec/secs"
	"uic/internal/codec/textval"
	"uic/internal/driver"
	"uic/internal/model"
)

// DefaultPort 是 HSMS/TCP 的默认端口。
const DefaultPort = secscodec.DefaultPort

// 对象类型常量。与 pointtable 的对象地址配合，构成 SECS 的寻址词汇表。
const (
	objectSVID = secscodec.ObjectSVID // 状态变量：S1F3/S1F4 读
	objectECID = secscodec.ObjectECID // 设备常数：S2F13/S2F14 读、S2F15/S2F16 写
)

// 内置默认值。
const (
	// defaultRetries 是超时后的额外重发次数。HSMS 跑在 TCP 上，重发只在
	// 「对端进程卡住但连接没断」时有意义，默认重试一次。
	defaultRetries = 1
)

// Driver 是 SECS/GEM 主站驱动入口。
type Driver struct{}

// New 创建驱动实例。
func New() *Driver { return &Driver{} }

// Meta 声明 SECS/GEM 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "secs",
		Name:         "SECS/GEM (HSMS)",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressObject},
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
				Placeholder: "192.168.1.40:5000", Scope: "channel",
				Help: "HSMS/TCP 端点（设备侧端口，通常 5000）；省略端口时按 5000 处理"},
			{Name: "labels.commTimeout", Label: "Select 超时", Type: "duration",
				Default: "5s", Scope: "device",
				Help: "Open 时等待 Select.rsp 的时长；超时即 Open 失败"},
			{Name: "labels.retries", Label: "重试次数", Type: "int",
				Default: defaultRetries, Scope: "device",
				Help: "事务超时后的额外重发次数；0 表示只发一次"},
			{Name: "labels.establishComm", Label: "建立通信", Type: "bool",
				Default: false, Scope: "device",
				Help: "Select 后发 S1F13/S1F14 建立通信（GEM 要求）；接 GEM 设备开启，老 SECS 机台保持关闭"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"object"}, Scope: "point"},
			{Name: "address.objectType", Label: "对象类型", Type: "enum", Required: true,
				Enum: []string{objectSVID, objectECID}, Scope: "point",
				Help: "svid=状态变量（只读，S1F3 读）；ecid=设备常数（S2F13 读、S2F15 写）"},
			{Name: "address.instance", Label: "编号", Type: "int", Required: true,
				Scope: "point", Help: "SVID / ECID 编号（32 位无符号整数）"},
			{Name: "address.property", Label: "属性", Type: "enum", Required: true,
				Enum: []string{"value"}, Scope: "point"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64",
					"uint64", "float32", "float64", "string", "bytes"}, Scope: "point"},
			{Name: "address.length", Label: "最大字节数", Type: "int", Scope: "point",
				Help: "bytes/string 的接收上限；0 表示不限制"},
		},
		Description: "SECS/GEM 主站：HSMS/TCP + Select 握手、SVID 批量读（S1F3/S1F4）、设备常数读写（S2F13~S2F16）、Linktest 探活",
	}
}

// settings 是设备级可调参数。
type settings struct {
	commTimeout time.Duration
	retries     int
	// establishComm 控制 Select 之后是否发 S1F13/S1F14 建立通信。
	// GEM 设备必须先建立通信才接受 S1F3——接 GEM 设备时配
	// labels.establishComm=true；默认关闭是因为 S1F13 属于 GEM 状态模型，
	// 不是 SECS-II 本体必需，老 SECS 机台可能根本不实现它，默认开启会让
	// 这类设备的 Open 多等一轮超时。
	establishComm bool
}

// Open 建立 TCP 链路并完成 HSMS Select 握手。
//
// 不做「Open 即读值」：SECS 的连通性由 Select.rsp 的 status 直接给出，一次
// Select 往返就是最准确的探活。真正的读交给 `Read`，链路探活交给 `Probe`
// （Linktest）。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if kind := strings.ToLower(strings.TrimSpace(t.Kind)); kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("secs requires tcp transport (HSMS %d), got %q", DefaultPort, t.Kind),
			false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address 不能为空（HSMS/TCP 端点，如 192.168.1.40:5000）", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, DefaultPort)
		}
	}
	st, err := parseSettings(cfg)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}
	plans, byID, err := buildPlans(cfg.Device)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID, err.Error(), false, err)
	}

	dialCtx := ctx
	cancel := func() {}
	if timeout := cfg.Channel.EffectiveTimeout(); timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	rawConn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "open", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	tcpConn := rawConn.(*net.TCPConn)
	_ = tcpConn.SetNoDelay(true)

	c := &conn{
		devID:   cfg.Device.ID,
		conn:    tcpConn,
		addr:    addr,
		timeout: cfg.Device.TimeoutOrDefault(),
		st:      st,
		points:  plans,
		byID:    byID,
		sysSeq:  uint32(time.Now().UnixNano()),
		log:     cfg.Log,
	}
	if err := c.selectLink(ctx, st.commTimeout); err != nil {
		_ = tcpConn.Close()
		return nil, err
	}
	if st.establishComm {
		if err := c.establish(ctx); err != nil {
			_ = tcpConn.Close()
			return nil, err
		}
	}
	return c, nil
}

// establish 发 S1F13（`L,0`）建立通信并等 S1F14。
//
// GEM 规范要求先建立通信再请求数据：不少设备在通信未建立前直接拒绝
// S1F3，表现为「连上了但读什么都超时」。三种结局：
//
//   - COMMACK 0（接受）/ 3（已建立）：通过；
//   - COMMACK 1（拒绝）/ 2（CRA 不支持）：硬失败——设备明确说了「现在
//     不能建立」，硬着头皮发数据请求大概率也是被拒；
//   - 没有应答：**不判死**。S1F13 是 GEM 的要求，不是 SECS-II 本体的，
//     老机台可能根本不实现它——退回不建立通信继续采集，日志里留下
//     指引。
func (c *conn) establish(ctx context.Context) error {
	body, err := secscodec.EncodeEmptyList()
	if err != nil {
		return driver.E(driver.ErrInternal, "establish", c.devID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, secscodec.SFCommRequest, body, "establish")
	if err != nil {
		if driver.KindOf(err) == driver.ErrTimeout && c.log != nil {
			c.log.Warn("secs_establish_no_reply", "device", c.devID,
				"hint", "设备未应答 S1F13（可能非 GEM 机台），继续采集")
		}
		return nil
	}
	code, err := secscodec.DecodeCommAckCode(resp.Body)
	if err != nil {
		return driver.E(driver.ErrProtocol, "establish", c.devID, err.Error(), false, err)
	}
	if code != 0 && code != 3 {
		return driver.E(driver.ErrPermission, "establish", c.devID,
			fmt.Sprintf("建立通信被拒绝（COMMACK=%d: 1=denied, 2=CRA not supported）", code), false, nil)
	}
	return nil
}

func parseSettings(cfg driver.OpenConfig) (settings, error) {
	st := settings{
		commTimeout:   5 * time.Second,
		retries:       defaultRetries,
		establishComm: false,
	}
	labels := cfg.Device.Labels

	if v := strings.TrimSpace(labels["commTimeout"]); v != "" {
		dur, err := time.ParseDuration(v)
		if err != nil {
			return st, fmt.Errorf("labels.commTimeout %q: %w", v, err)
		}
		if dur <= 0 {
			return st, fmt.Errorf("labels.commTimeout %s must be positive", dur)
		}
		st.commTimeout = dur
	}
	if v := strings.TrimSpace(labels["retries"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return st, fmt.Errorf("labels.retries %q: not an integer", v)
		}
		if n < 0 || n > 5 {
			return st, fmt.Errorf("labels.retries %d: must be 0..5", n)
		}
		st.retries = n
	}
	if v := strings.TrimSpace(labels["establishComm"]); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return st, fmt.Errorf("labels.establishComm %q: not a bool", v)
		}
		st.establishComm = b
	}
	return st, nil
}

// pointPlan 是一个点位解析后的对象寻址。
type pointPlan struct {
	ref      driver.PointRef
	object   string // svid / ecid
	id       uint32 // 编号
	typ      model.DataType
	maxLen   int
	writable bool
}

func buildPlans(d model.Device) ([]pointPlan, map[string]pointPlan, error) {
	plans := make([]pointPlan, 0, len(d.Points))
	byID := make(map[string]pointPlan, len(d.Points))
	for _, p := range d.Points {
		if !p.Enabled {
			continue
		}
		plan, err := planPoint(driver.PointRefOf(d, p), p.Writable)
		if err != nil {
			return nil, nil, err
		}
		plans = append(plans, plan)
		byID[plan.ref.ID] = plan
	}
	return plans, byID, nil
}

func planPoint(ref driver.PointRef, writable bool) (pointPlan, error) {
	if ref.Address.Kind != model.AddressObject {
		return pointPlan{}, fmt.Errorf("point %q: secs requires object addressing, got %q",
			ref.ID, ref.Address.Kind)
	}
	obj := strings.ToLower(strings.TrimSpace(ref.Address.ObjectType))
	if obj != objectSVID && obj != objectECID {
		return pointPlan{}, fmt.Errorf("point %q: unknown object type %q (want %s or %s)",
			ref.ID, ref.Address.ObjectType, objectSVID, objectECID)
	}
	// property 只支持 value：本驱动不做 S1F12 名称查询，也没有多级属性。
	prop := strings.ToLower(strings.TrimSpace(ref.Address.Property))
	if prop != "" && prop != "value" {
		return pointPlan{}, fmt.Errorf("point %q: property %q unsupported (only \"value\")",
			ref.ID, ref.Address.Property)
	}
	if ref.Address.Instance <= 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.instance must be a positive SVID/ECID", ref.ID)
	}
	if ref.DataType == "" || !ref.DataType.Valid() {
		return pointPlan{}, fmt.Errorf("point %q: invalid dataType %q", ref.ID, ref.DataType)
	}
	if ref.Address.Length < 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.length must be >= 0", ref.ID)
	}
	// SVID 只读：SECS/GEM 的状态变量没有写语义，写它要走远程命令流程。
	// writable 参数来自 model.Point.Writable（设备配置）；只有 ecid 且配置允许
	// 写时才是可写，其余一律只读。
	canWrite := writable && obj == objectECID
	return pointPlan{
		ref:      ref,
		object:   obj,
		id:       uint32(ref.Address.Instance),
		typ:      ref.DataType,
		maxLen:   ref.Address.Length,
		writable: canWrite,
	}, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	devID   string
	conn    *net.TCPConn
	addr    string
	timeout time.Duration
	st      settings
	log     *slog.Logger

	mu     sync.Mutex
	closed bool
	sysSeq uint32

	points []pointPlan
	byID   map[string]pointPlan
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	// 主动发 Separate.req 再关连接：对端据此区分「正常结束」与「断链」，
	// 它的日志与重连策略会不同。发送失败不影响关闭结果。
	if c.conn != nil {
		c.sendNoWait(secscodec.NewSeparateReq(c.nextSys()))
		_ = c.conn.Close()
	}
	return nil
}

// nextSys 返回一个非零、递增的系统字节。
//
// 系统字节是 HSMS 事务配对的唯一依据（与 ADS 的 invoke、CoAP 的 MID 同类）。
// 用进程内单调计数器，初值取墙上时间，测试可预期、日志可对齐。
func (c *conn) nextSys() uint32 {
	c.sysSeq++
	if c.sysSeq == 0 {
		c.sysSeq = 1
	}
	return c.sysSeq
}

func (c *conn) sendNoWait(m secscodec.Message) {
	if c.conn == nil {
		return
	}
	raw, err := secscodec.Encode(m)
	if err != nil {
		return
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	_, _ = c.conn.Write(raw)
}

// selectLink 完成 HSMS Select 握手。
//
// TCP 连上后链路处于 NOT SELECTED 状态，DATA 消息必须等 Select.rsp 的
// status=0 才能交换。因此握手放在 Open 里：失败即 Open 失败，调用方拿到
// 的就是一条不可用的连接。
func (c *conn) selectLink(ctx context.Context, wait time.Duration) error {
	req := secscodec.NewSelectReq(c.nextSys())
	raw, err := secscodec.Encode(req)
	if err != nil {
		return driver.E(driver.ErrInternal, "select", c.devID, err.Error(), false, err)
	}
	if dl, ok := ctx.Deadline(); ok && dl.Before(time.Now().Add(wait)) {
		wait = time.Until(dl)
	}
	deadline := time.Now().Add(wait)
	if err := c.conn.SetDeadline(deadline); err != nil {
		return driver.E(driver.ErrDisconnected, "select", c.devID, "set deadline", true, err)
	}
	if _, err := c.conn.Write(raw); err != nil {
		return driver.E(driver.ErrDisconnected, "select", c.devID,
			"write Select.req failed", true, err)
	}
	m, err := c.readOne()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(secscodec.TypeName(m.MsgType), "SelectRsp") {
		return driver.E(driver.ErrProtocol, "select", c.devID,
			fmt.Sprintf("expected Select.rsp, got %s", m.String()), false, nil)
	}
	if m.Status() != secscodec.StatusOK {
		return driver.E(driver.ErrDisconnected, "select", c.devID,
			fmt.Sprintf("Select rejected: status=%d(%s)",
				m.Status(), secscodec.StatusName(m.Status())), true, nil)
	}
	return nil
}

// readOne 从连接读一条 HSMS 消息。
//
// 它容忍两类「不是我要的帧」：① 帧还没读全（半包）；② 设备主动推来的
// 其它消息（S6F11 事件、S5F1 报警、上一次迟到的应答）。前者继续等，后者
// 丢弃——本驱动不订阅事件，异步推送对当前事务没有意义。
func (c *conn) readOne() (secscodec.Message, error) {
	head := make([]byte, secscodec.FrameLen)
	if _, err := io.ReadFull(c.conn, head); err != nil {
		return secscodec.Message{}, c.readErr("read header", err)
	}
	total := secscodec.FrameLen
	if n, err := secscodec.FrameDataLen(head); err != nil {
		return secscodec.Message{}, driver.E(driver.ErrProtocol, "read", c.devID,
			"malformed HSMS header: "+err.Error(), false, err)
	} else {
		total += int(n)
	}
	body := make([]byte, total-secscodec.FrameLen)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return secscodec.Message{}, c.readErr("read body", err)
	}
	frame := append(head, body...)
	m, err := secscodec.Decode(frame)
	if err != nil {
		return secscodec.Message{}, driver.E(driver.ErrProtocol, "read", c.devID,
			"decode HSMS frame: "+err.Error(), false, err)
	}
	return m, nil
}

func (c *conn) readErr(op string, err error) error {
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.devID,
			"no response within "+c.timeout.String(), true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.devID, "tcp read failed", true, err)
}

// exchange 发送一条 DATA 主消息并等待匹配的 Secondary 应答。
//
// 匹配规则：同一系统字节 + Stream/Function 是应答（W 位清零）。不匹配的消息
// （如设备异步推送的 S6F11）被丢弃。超时后按 settings.retries 重发主消息。
func (c *conn) exchange(ctx context.Context, sf secscodec.SF, body []byte, op string) (secscodec.DataMessage, error) {
	var zero secscodec.DataMessage
	var lastErr error
	for attempt := 0; attempt <= c.st.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, err)
		}
		sys := c.nextSys()
		dm := secscodec.DataMessage{
			SystemBytes: sys, Stream: sf.Stream, Function: sf.Function,
			WBit: true, Body: body,
		}
		raw, err := secscodec.Encode(dm.ToMessage())
		if err != nil {
			return zero, driver.E(driver.ErrInternal, op, c.devID, "encode: "+err.Error(), false, err)
		}
		deadline := c.attemptDeadline(ctx)
		if err := c.conn.SetDeadline(deadline); err != nil {
			return zero, driver.E(driver.ErrDisconnected, op, c.devID, "set deadline", true, err)
		}
		if _, err := c.conn.Write(raw); err != nil {
			if isTimeout(err) || ctx.Err() != nil {
				return zero, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, err)
			}
			return zero, driver.E(driver.ErrDisconnected, op, c.devID, "tcp write failed", true, err)
		}
		resp, rerr := c.awaitSecondary(ctx, sys, sf)
		if rerr != nil {
			lastErr = rerr
			if k := driver.KindOf(rerr); k == driver.ErrTimeout || k == driver.ErrDisconnected {
				continue
			}
			return zero, rerr
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = driver.E(driver.ErrTimeout, op, c.devID, "no response", true, nil)
	}
	return zero, lastErr
}

// attemptDeadline 计算本次尝试的等待截止时间：设备级超时与 ctx 取更早者。
func (c *conn) attemptDeadline(ctx context.Context) time.Time {
	dl := time.Now().Add(c.timeout)
	if x, ok := ctx.Deadline(); ok && x.Before(dl) {
		dl = x
	}
	return dl
}

// awaitSecondary 循环读帧，直到拿到 sys 对应的 Secondary（W 位清零）。
//
// Secondary 的 Function 是 Primary 的 +1（S1F3→S1F4、S2F15→S2F16）。这里按
// 「同 Stream、Function 为请求 +1、W 位清零」匹配，而不是拿请求 Function 去比——
// 否则会把真正的应答当成无关消息跳过，一直读到超时。
func (c *conn) awaitSecondary(ctx context.Context, sys uint32, sf secscodec.SF) (secscodec.DataMessage, error) {
	for {
		if err := ctx.Err(); err != nil {
			return secscodec.DataMessage{}, driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
		}
		m, err := c.readOne()
		if err != nil {
			return secscodec.DataMessage{}, err
		}
		if !m.IsData() {
			// 控制消息（Linktest/Reject/Select 等）不是当前事务的应答，丢弃。
			continue
		}
		dm, err := secscodec.ParseData(m)
		if err != nil {
			continue
		}
		if dm.SystemBytes != sys {
			// 迟到的上一次应答或与本次无关；丢弃。
			continue
		}
		if dm.Stream != sf.Stream || dm.WBit || dm.Function != sf.Function+1 {
			// 不是期待的那条应答。
			continue
		}
		return dm, nil
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// Read 把 span 内的点按对象类型分组，批量读取。
//
// svid 点合并成一次 S1F3，ecid 点合并成一次 S2F13。两组各自一次往返，因此
// 一个 span 最多两次事务，而不是每个点一次。
//
// 编号在分组阶段就解析成 pointPlan 随组带走，而不是在读的时候按点位 ID 回查
// byID：外部调用方（自检 CLI、北向按需读）传进来的点可能不在设备的点表里，
// 回查会得到零值编号并静默读 0 号。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", true, nil)
	}

	out := make([]driver.RawSample, 0, len(spans))
	var firstErr error

	groups := make([]readGroupReq, 0, 2)
	index := map[string]int{}

	for _, span := range spans {
		for _, ref := range span.Points {
			plan, err := c.planFor(ref)
			if err != nil {
				e := driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
				out = append(out, driver.RawSample{PointID: ref.ID, Quality: qualityFor(driver.ErrAddress), Err: e})
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			i, ok := index[plan.object]
			if !ok {
				i = len(groups)
				index[plan.object] = i
				groups = append(groups, readGroupReq{object: plan.object})
			}
			groups[i].plans = append(groups[i].plans, plan)
		}
	}

	var linkErr error
	// 固定顺序：先 SVID 再 ECID。组顺序影响的是日志与重试顺序，不影响正确性，
	// 但固定下来能让「同一个 span 两次读」产生可比对的线程序。
	order := []string{objectSVID, objectECID}
	for oi, object := range order {
		i, ok := index[object]
		if !ok {
			continue
		}
		if err := c.readGroup(ctx, groups[i], &out, &firstErr); err != nil {
			// 链路级失败：后面的组不可能成功，剩余点必须显式标坏——
			// 让它们「消失」会被采集层当成设备安静，把真正的超时原因盖掉。
			linkErr = err
			for _, rest := range order[oi+1:] {
				j, ok := index[rest]
				if !ok {
					continue
				}
				for _, plan := range groups[j].plans {
					out = append(out, driver.RawSample{
						PointID: plan.ref.ID,
						Quality: qualityFor(driver.KindOf(err)),
						Err:     err,
					})
				}
			}
			break
		}
	}

	if linkErr != nil {
		return out, linkErr
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误。
	return out, driver.PartialReadError(countGood(out), firstErr)
}

// countGood 数结果里的成功点位，用于部分失败判定。
func countGood(samples []driver.RawSample) int {
	n := 0
	for _, s := range samples {
		if s.Err == nil && s.Quality == model.QualityGood {
			n++
		}
	}
	return n
}

// planFor 取点位的解析结果：优先用 Open 时算好的计划（它带着 Writable 等
// 设备配置），没有则按点位自带的地址临时解析（只读）。
func (c *conn) planFor(ref driver.PointRef) (pointPlan, error) {
	if plan, ok := c.byID[ref.ID]; ok {
		return plan, nil
	}
	return planPoint(ref, false)
}

// readGroupReq 是一次批量读：同一对象类型下的一组点。
type readGroupReq struct {
	object string
	plans  []pointPlan
}

// readGroup 合并一组同类型编号，发一次请求，把取值分发回各点。
//
// 分发失败（应答里缺某个编号、编号格式解析失败）只影响那一个点，其余点照常
// 返回——批量读的价值正是「一次往返拿到多数」，不能因为一个坏编号让整组作废。
//
// 返回值只在**链路级失败**（超时/断连）时非 nil：那时后面的组不可能成功，
// 调用方应当立即收尾并把整轮判错。组内的点级失败写进 firstErr 后返回 nil。
func (c *conn) readGroup(ctx context.Context, g readGroupReq, out *[]driver.RawSample, firstErr *error) error {
	obj := g.object

	var sf secscodec.SF
	switch obj {
	case objectSVID:
		sf = secscodec.SFSelectedStatusRQ
	case objectECID:
		sf = secscodec.SFEqConstantRQ
	default:
		for _, plan := range g.plans {
			e := driver.E(driver.ErrProtocol, "read", c.devID, "unknown object group "+obj, false, nil)
			*out = append(*out, driver.RawSample{PointID: plan.ref.ID, Quality: qualityFor(driver.ErrProtocol), Err: e})
			if *firstErr == nil {
				*firstErr = e
			}
		}
		return nil
	}

	ids := make([]uint32, 0, len(g.plans))
	for _, plan := range g.plans {
		ids = append(ids, plan.id)
	}
	fail := func(kind driver.ErrorKind, err error) {
		for _, plan := range g.plans {
			*out = append(*out, driver.RawSample{PointID: plan.ref.ID, Quality: qualityFor(kind), Err: err})
		}
		if *firstErr == nil {
			*firstErr = err
		}
	}

	reqBody, err := secscodec.EncodeIDList(ids)
	if err != nil {
		fail(driver.ErrInternal, driver.E(driver.ErrInternal, "read", c.devID,
			fmt.Sprintf("%s: encode request", sf), false, err))
		return nil
	}

	dm, err := c.exchange(ctx, sf, reqBody, "read:"+obj)
	if err != nil {
		// 链路级失败：整组都拿不到，逐点标注同一错误。
		fail(driver.KindOf(err), err)
		if driver.IsLinkFailure(err) {
			return err
		}
		return nil
	}
	// S1F4 是按位置的纯值列表（E5 不回显 SVID）；S2F14 才按 ECID 配对。
	var byID map[uint32]secscodec.StatusValue
	if obj == objectSVID {
		items, derr := secscodec.DecodeValueReply(dm.Body)
		if derr != nil {
			fail(driver.ErrProtocol, driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("decode %s", sf), false, derr))
			return nil
		}
		if len(items) != len(ids) {
			fail(driver.ErrProtocol, driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("S1F4 元素数 %d 与请求数 %d 不一致", len(items), len(ids)), false, nil))
			return nil
		}
		byID = make(map[uint32]secscodec.StatusValue, len(ids))
		for i, id := range ids {
			it := items[i]
			// 零长 Binary 是内置从站的「编号不存在」占位。
			if it.Format == secscodec.FormatBinary && len(it.Raw) == 0 {
				continue
			}
			byID[id] = secscodec.StatusValue{ID: id, Value: it}
		}
	} else {
		vals, derr := secscodec.DecodeStatusReply(dm.Body)
		if derr != nil {
			fail(driver.ErrProtocol, driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("decode %s", sf), false, derr))
			return nil
		}
		byID = make(map[uint32]secscodec.StatusValue, len(vals))
		for _, v := range vals {
			byID[v.ID] = v
		}
	}

	for _, plan := range g.plans {
		sv, ok := byID[plan.id]
		if !ok {
			e := driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("point %q: %s reply has no value for %s %d",
					plan.ref.ID, sf, obj, plan.id), false, nil)
			*out = append(*out, driver.RawSample{PointID: plan.ref.ID, Quality: qualityFor(driver.ErrAddress), Err: e})
			if *firstErr == nil {
				*firstErr = e
			}
			continue
		}
		v, verr := itemValue(sv.Value, plan.typ, plan.maxLen)
		if verr != nil {
			e := driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("point %q: %s %d", plan.ref.ID, obj, plan.id), false, verr)
			*out = append(*out, driver.RawSample{PointID: plan.ref.ID, Quality: qualityFor(driver.ErrProtocol), Err: e})
			if *firstErr == nil {
				*firstErr = e
			}
			continue
		}
		*out = append(*out, driver.RawSample{PointID: plan.ref.ID, Value: v, Quality: model.QualityGood})
	}
	return nil
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// Write 把 ecid 点合并成一次 S2F15，按 S2F16 的 ACKC7 逐点判定结果。
//
// ACKC7 只有一个字节：0=接受，非 0=拒绝（值非法、编号未知、只读等）。它对整批
// 给出一个结论，因此这里把「整批被拒」翻译成每个点的失败；真正的逐点写确认
// 要到 S2F49/S2F50（远程命令流程），不在最小范围内。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "write", c.devID, "connection closed", true, nil)
	}

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error

	var constants []secscodec.Constant
	var plans []pointPlan
	for _, req := range reqs {
		// 写只认点表里的点：writable 是设备配置里声明过的写权限，凭一个地址
		// 就允许写等于让任何能构造请求的人绕过它。读则可以按地址临时解析
		// （自检与北向按需读要用），两者刻意不对称。
		plan, ok := c.byID[req.PointID]
		if !ok {
			e := driver.E(driver.ErrPermission, "write", c.devID,
				fmt.Sprintf("point %q: not in device point table", req.PointID), false, nil)
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: model.QualityBad, Err: e})
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		if !plan.writable {
			e := driver.E(driver.ErrPermission, "write", c.devID,
				fmt.Sprintf("point %q: %s is read-only", req.PointID, plan.object), false, nil)
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: model.QualityBad, Err: e})
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		item, err := valueItem(req.Value, plan.typ)
		if err != nil {
			e := driver.E(driver.ErrUnsupported, "write", c.devID,
				fmt.Sprintf("point %q: cannot encode value", req.PointID), false, err)
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: qualityFor(driver.ErrUnsupported), Err: e})
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		constants = append(constants, secscodec.Constant{ID: plan.id, Value: item})
		plans = append(plans, plan)
	}

	if len(constants) == 0 {
		return results, firstErr
	}

	body, err := secscodec.EncodeConstantSend(constants)
	if err != nil {
		for _, plan := range plans {
			e := driver.E(driver.ErrInternal, "write", c.devID, err.Error(), false, err)
			results = append(results, driver.WriteResult{PointID: plan.ref.ID, Quality: qualityFor(driver.ErrInternal), Err: e})
			if firstErr == nil {
				firstErr = e
			}
		}
		return results, firstErr
	}

	dm, err := c.exchange(ctx, secscodec.SFEqConstantSend, body, "write")
	if err != nil {
		for _, plan := range plans {
			results = append(results, driver.WriteResult{PointID: plan.ref.ID, Quality: qualityFor(driver.KindOf(err)), Err: err})
			if firstErr == nil {
				firstErr = err
			}
		}
		return results, firstErr
	}
	ack, aerr := secscodec.DecodeAck(dm.Body)
	if aerr != nil {
		e := driver.E(driver.ErrProtocol, "write", c.devID,
			"decode S2F16 ACK: "+aerr.Error(), false, aerr)
		for _, plan := range plans {
			results = append(results, driver.WriteResult{PointID: plan.ref.ID, Quality: qualityFor(driver.ErrProtocol), Err: e})
			if firstErr == nil {
				firstErr = e
			}
		}
		return results, firstErr
	}
	for _, plan := range plans {
		if ack != 0 {
			e := driver.E(driver.ErrUnsupported, "write", c.devID,
				fmt.Sprintf("point %q: device rejected write (ACKC7=%d)", plan.ref.ID, ack), false, nil)
			results = append(results, driver.WriteResult{PointID: plan.ref.ID, Quality: model.QualityBad, Err: e})
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		results = append(results, driver.WriteResult{PointID: plan.ref.ID, OK: true, Quality: model.QualityGood})
	}
	return results, firstErr
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

// Probe 用 Linktest 验证链路。
//
// Linktest 不改变链路状态，只问一句「还在吗」，因此适合作为周期性探活。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", true, nil)
	}
	req := secscodec.NewLinktestReq(c.nextSys())
	raw, err := secscodec.Encode(req)
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.devID, err.Error(), false, err)
	}
	deadline := c.attemptDeadline(ctx)
	if err := c.conn.SetDeadline(deadline); err != nil {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "set deadline", true, err)
	}
	if _, err := c.conn.Write(raw); err != nil {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "tcp write failed", true, err)
	}
	m, err := c.readOne()
	if err != nil {
		return err
	}
	// E37 里 Linktest.rsp 没有状态字段——收到它本身就是「链路活着」的
	// 全部语义，额外的状态检查只会把好消息当坏消息。
	if m.MsgType != secscodec.MsgTypeLinktestRsp {
		return driver.E(driver.ErrProtocol, "probe", c.devID,
			fmt.Sprintf("expected Linktest.rsp, got %s", m.String()), false, nil)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 自检用的只读扩展
// ---------------------------------------------------------------------------

// Variable 是 S1F12 返回的一条状态变量元信息（编号、名称、单位）。
type Variable = secscodec.StatusVariable

// Inspector 是驱动在基础 Conn 契约之外提供的自检扩展。
//
// 它单独成一个接口，而不塞进 driver.Conn：这两件事只有「人」会用到（排查
// 现场），调度与北向永远用不到。让 Conn 保持最小，其他协议就不必为一个用
// 不到的诊断能力写空实现。
type Inspector interface {
	// Describe 用 S1F11/S1F12 查询状态变量的名称与单位。
	Describe(ctx context.Context, ids []uint32) ([]Variable, error)
	// RawRead 读一个编号的原始 SECS-II 项，不做类型转换。
	RawRead(ctx context.Context, object string, id uint32) (*secscodec.Item, error)
}

// 编译期确认 *conn 实现了自检扩展。
var _ Inspector = (*conn)(nil)

// Describe 用 S1F11/S1F12 查询状态变量的名称与单位。
//
// 它不参与采集：点表只关心值，名字属于「人读」的信息。把它放在驱动上而不是
// 自检命令里，是因为它需要已建立的 HSMS 会话（系统字节配对、重发策略），
// 在命令里重新实现一遍事务层只会多一份会走样的代码。
func (c *conn) Describe(ctx context.Context, ids []uint32) ([]Variable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "describe", c.devID, "connection closed", true, nil)
	}
	body, err := secscodec.EncodeIDList(ids)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "describe", c.devID, err.Error(), false, err)
	}
	dm, err := c.exchange(ctx, secscodec.SFStatusNamelistRQ, body, "describe")
	if err != nil {
		return nil, err
	}
	names, derr := secscodec.DecodeStatusNameReply(dm.Body)
	if derr != nil {
		return nil, driver.E(driver.ErrProtocol, "describe", c.devID,
			"decode S1F12: "+derr.Error(), false, derr)
	}
	return names, nil
}

// RawRead 读一个编号的原始 SECS-II 项，不做任何类型转换。
//
// 自检场景下这比「按声明类型读」有用得多：现场最常见的问题是「设备回的格式
// 与点表声明不一致」（例如点表写 int32、设备回 A "RUN"），先看原始项就能一眼
// 定位，而不是从一个「类型不符」的错误里猜。
func (c *conn) RawRead(ctx context.Context, object string, id uint32) (*secscodec.Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", true, nil)
	}
	var sf secscodec.SF
	switch strings.ToLower(strings.TrimSpace(object)) {
	case objectSVID:
		sf = secscodec.SFSelectedStatusRQ
	case objectECID:
		sf = secscodec.SFEqConstantRQ
	default:
		return nil, driver.E(driver.ErrAddress, "read", c.devID,
			fmt.Sprintf("unknown object type %q (want %s or %s)", object, objectSVID, objectECID), false, nil)
	}
	body, err := secscodec.EncodeIDList([]uint32{id})
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "read", c.devID, err.Error(), false, err)
	}
	dm, err := c.exchange(ctx, sf, body, "read:"+object)
	if err != nil {
		return nil, err
	}
	if object == objectSVID {
		// S1F4 = L,n{V} 按位置对应；零长 Binary 是「编号不存在」占位。
		items, derr := secscodec.DecodeValueReply(dm.Body)
		if derr != nil {
			return nil, driver.E(driver.ErrProtocol, "read", c.devID,
				"decode "+sf.String()+": "+derr.Error(), false, derr)
		}
		if len(items) == 1 {
			it := items[0]
			if it.Format == secscodec.FormatBinary && len(it.Raw) == 0 {
				return nil, driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("%s: device has no SVID %d", sf, id), false, nil)
			}
			return it, nil
		}
		return nil, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("%s reply has %d items, want 1", sf, len(items)), false, nil)
	}
	vals, derr := secscodec.DecodeStatusReply(dm.Body)
	if derr != nil {
		return nil, driver.E(driver.ErrProtocol, "read", c.devID,
			"decode "+sf.String()+": "+derr.Error(), false, derr)
	}
	for _, v := range vals {
		if v.ID == id {
			return v.Value, nil
		}
	}
	return nil, driver.E(driver.ErrAddress, "read", c.devID,
		fmt.Sprintf("%s reply has no value for %s %d", sf, object, id), false, nil)
}

// ---------------------------------------------------------------------------
// 值转换
// ---------------------------------------------------------------------------

// itemValue 把 SECS-II 数据项按平台声明的数据类型转成 model.Value。
//
// 取值走「按声明解码、不猜」：平台声明 int32，就从项里读整数并做范围校验；
// 项的实际格式与声明不符时报错，而不是把一个 U4 项悄悄塞进 int16 点。
func itemValue(it *secscodec.Item, dt model.DataType, maxLen int) (model.Value, error) {
	switch dt {
	case model.TypeBool:
		b, err := it.Bool()
		if err != nil {
			return model.Value{}, err
		}
		return model.BoolValue(b), nil
	case model.TypeString:
		s, err := it.String()
		if err != nil {
			return model.Value{}, err
		}
		if maxLen > 0 && len(s) > maxLen {
			return model.Value{}, fmt.Errorf("string is %d bytes, exceeds address.length %d", len(s), maxLen)
		}
		return model.TextValue(model.TypeString, s), nil
	case model.TypeBytes:
		b, err := it.Bytes()
		if err != nil {
			return model.Value{}, err
		}
		if maxLen > 0 && len(b) > maxLen {
			return model.Value{}, fmt.Errorf("bytes is %d, exceeds address.length %d", len(b), maxLen)
		}
		return model.BytesValue(b), nil
	}

	// 数值类型：先从项里取出 float64（覆盖 U1..U8/I1..I8/F4/F8），再做范围校验。
	f, err := itemFloat(it)
	if err != nil {
		return model.Value{}, err
	}
	if err := textvalCheck(dt, f); err != nil {
		return model.Value{}, err
	}
	return model.NumValue(dt, f), nil
}

// itemFloat 从标量项取出数值。字符串项也接受（部分设备把数值编成 A）。
func itemFloat(it *secscodec.Item) (float64, error) {
	switch it.Format {
	case secscodec.FormatU1:
		v, err := it.U1()
		return float64(v), err
	case secscodec.FormatU2:
		v, err := it.U2()
		return float64(v), err
	case secscodec.FormatU4:
		v, err := it.U4()
		return float64(v), err
	case secscodec.FormatU8:
		v, err := it.U8()
		return float64(v), err
	case secscodec.FormatI1:
		v, err := it.I1()
		return float64(v), err
	case secscodec.FormatI2:
		v, err := it.I2()
		return float64(v), err
	case secscodec.FormatI4:
		v, err := it.I4()
		return float64(v), err
	case secscodec.FormatI8:
		v, err := it.I8()
		return float64(v), err
	case secscodec.FormatF4:
		v, err := it.F4()
		return float64(v), err
	case secscodec.FormatF8:
		return it.F8()
	case secscodec.FormatASCII, secscodec.FormatJIS:
		s, err := it.String()
		if err != nil {
			return 0, err
		}
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f); err != nil {
			return 0, fmt.Errorf("cannot read %q as number", s)
		}
		return f, nil
	}
	return 0, fmt.Errorf("cannot read %s as number", secscodec.FormatName(it.Format))
}

// valueItem 把平台值按声明类型编成 SECS-II 标量项。
//
// 值的实际类型必须与声明类型对得上。这一层校验不能省：`TypeString` 分支如果
// 直接取 v.Text，一个数值值（Text 为空）就会静默变成空字符串写进设备——比
// 报错危险得多，因为设备会「成功地」收到一个空标签。
func valueItem(v model.Value, dt model.DataType) (*secscodec.Item, error) {
	switch dt {
	case model.TypeBool:
		if v.Type != model.TypeBool {
			return nil, fmt.Errorf("cannot encode %s value as %s", v.Type, dt)
		}
		return secscodec.Bool(v.Bool), nil
	case model.TypeString:
		if v.Type != model.TypeString {
			return nil, fmt.Errorf("cannot encode %s value as %s", v.Type, dt)
		}
		return secscodec.ASCII(v.Text), nil
	case model.TypeBytes:
		if v.Type != model.TypeBytes {
			return nil, fmt.Errorf("cannot encode %s value as %s", v.Type, dt)
		}
		return secscodec.Binary([]byte(v.Text)), nil
	}
	n, ok := v.AsFloat()
	if !ok {
		return nil, fmt.Errorf("cannot encode %s value as %s", v.Type, dt)
	}
	switch dt {
	case model.TypeInt16:
		return secscodec.Int(secscodec.FormatI2, int64(n)), nil
	case model.TypeUint16:
		return secscodec.Int(secscodec.FormatU2, int64(n)), nil
	case model.TypeInt32:
		return secscodec.Int(secscodec.FormatI4, int64(n)), nil
	case model.TypeUint32:
		return secscodec.Int(secscodec.FormatU4, int64(n)), nil
	case model.TypeInt64:
		return secscodec.Int(secscodec.FormatI8, int64(n)), nil
	case model.TypeUint64:
		return secscodec.Int(secscodec.FormatU8, int64(n)), nil
	case model.TypeFloat32:
		return secscodec.Float(secscodec.FormatF4, n), nil
	case model.TypeFloat64:
		return secscodec.Float(secscodec.FormatF8, n), nil
	}
	return nil, fmt.Errorf("unknown data type %q", dt)
}

// textvalCheck 校验数值能否被目标类型无损承载。直接复用 textval 的范围表，
// 避免在这里复制一份「哪些整数类型不接受小数」的规则。
func textvalCheck(dt model.DataType, n float64) error {
	return textval.CheckRange(dt, n)
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func qualityFor(kind driver.ErrorKind) model.Quality {
	switch kind {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrAddress:
		return model.QualityAddressError
	default:
		return model.QualityBad
	}
}
