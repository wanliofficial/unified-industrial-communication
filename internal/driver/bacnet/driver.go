// Package bacnet 实现 BACnet/IP（Annex J，UDP 47808）主站驱动。
//
// 分层与其它驱动一致：Meta（能力声明）→ conn（UDP 事务）→ codec/bacnet
// （纯函数编解码）→ transport（UDP 数据报）。
//
// 只做 ReadProperty（0x0C）与 WriteProperty（0x0F）：
//
//   - 一轮采集按「对象 + 属性」逐条读，不做 ReadPropertyMultiple（0x0E），
//     因为后者在嵌入式设备上的支持率远低于单属性读，且失败时很难定位是
//     哪一个属性出的问题；
//   - 不做 Who-Is/I-Am 广播发现：网关永远只连配置里点名的设备，广播在
//     多网段/多 BBMD 现场不可控；
//   - 不做 COV 订阅：那是「设备主动推」的模型，与本平台的调度式采集语义
//     不同，混进来会让超时、重连、死区三套逻辑互相干扰。
//
// UDP 是无连接的，因此「请求-应答」必须靠 invoke ID 配对。驱动串行化整条
// 会话：一个套接字上同时挂两个未完成事务时，迟到的应答会张冠李戴，而
// BACnet 的事务号只有 8 位，现场几乎必然复用。
package bacnet

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存数据报链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error)

// Driver 是 BACnet/IP 主站驱动。
type Driver struct {
	dial Dialer
	// retries 是一次事务在超时后额外重试的次数。UDP 丢包是常态，
	// 但 BACnet 设备大多按「每事务一次应答」实现，重试过多只会让
	// 一轮采集的整体时延不可控。
	retries int
}

// New 返回使用真实 UDP 的驱动。
func New() *Driver {
	return &Driver{
		dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
			return transport.DialUDP(ctx, addr, timeout)
		},
		retries: 2,
	}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	drv := New()
	drv.dial = d
	return drv
}

// Meta 声明 BACnet/IP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "bacnet",
		Name:         "BACnet/IP",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"udp"},
		AddressKinds: []model.AddressKind{model.AddressObject},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapUnsupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.20:47808", Scope: "channel",
				Help: "BACnet/IP 设备地址；省略端口时按 47808 处理"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "UDP 用 8 位 invoke ID 配对请求与应答；必须串行，建议 1"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.objectType", Label: "对象类型", Type: "string", Required: true, Scope: "point",
				Placeholder: "analog-input",
				Help:        "analog-input / analog-output / analog-value / binary-input / binary-value / multi-state-value / device，也可写对象类型编号"},
			{Name: "address.instance", Label: "实例号", Type: "int", Required: true, Scope: "point",
				Placeholder: "1", Help: "对象实例号 0..4194302"},
			{Name: "address.property", Label: "属性", Type: "string", Required: true, Scope: "point",
				Placeholder: "present-value",
				Help:        "present-value / object-name / units / priority-array 等，也可写属性编号"},
			{Name: "address.propIndex", Label: "数组下标", Type: "int", Default: 0, Scope: "point",
				Help: "读取数组属性时的下标；0 表示不带下标（读整个属性）"},
		},
		Description: "BACnet/IP 主站：对象属性读写（ReadProperty / WriteProperty，UDP 47808）",
	}
}

// Open 建立 UDP 套接字。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "udp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("bacnet requires udp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// 没有冒号、或冒号后不是端口（IPv6 字面量必须自带方括号）。
		// 只在「缺少端口」这一种情况下补默认端口；其它写法原样交给
		// ResolveUDPAddr，让真正的错误信息保留下来。
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, bacnetcodec.DefaultPort)
		}
	}
	conn, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	return &connState{
		cfg:     cfg,
		conn:    conn,
		addr:    addr,
		retries: d.retries,
	}, nil
}

type connState struct {
	cfg  driver.OpenConfig
	conn transport.Datagram
	addr string

	// mu 串行化整条会话：8 位 invoke ID + UDP 无连接，交错事务必然
	// 让应答与请求错配。
	mu     sync.Mutex
	invoke byte

	retries int
}

func (c *connState) Close() error {
	return c.conn.Close()
}

// nextInvoke 返回下一个非零 invoke ID（1..255 循环）。
//
// 0 保留给「设备主动发起」的报文，作为事务号没有意义；跳过它能让
// 日志里一眼看出哪条是我们发的。
func (c *connState) nextInvoke() byte {
	c.invoke++
	if c.invoke == 0 {
		c.invoke = 1
	}
	return c.invoke
}

// Probe 读 Device 对象的 object-name 验证链路。
//
// 设备实例号在配置里不可知（现场从 1 到 4194302 都有），所以这里用
// 实例 1 试探：返回 ComplexACK 说明能读；返回 Error（例如 unknown-object）
// 同样说明 BACnet 层是通的，只是猜错了实例。只有超时/非法报文才算失败。
func (c *connState) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	invoke := c.nextInvoke()
	req, err := bacnetcodec.ReadPropertyRequest(invoke, bacnetcodec.ObjectDevice, 1,
		bacnetcodec.PropObjectName, 0)
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, invoke, "probe")
	if err != nil {
		return err
	}
	switch bacnetcodec.ClassifyAPDU(resp[0]) {
	case bacnetcodec.APDUKindComplexACK, bacnetcodec.APDUKindError,
		bacnetcodec.APDUKindReject, bacnetcodec.APDUKindAbort:
		return nil
	}
	return driver.E(driver.ErrProtocol, "probe", c.cfg.Device.ID,
		fmt.Sprintf("unexpected APDU 0x%02X for probe", resp[0]), false, nil)
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// exchange 发送一条 APDU 并等待 invoke ID 相同的应答。
func (c *connState) exchange(ctx context.Context, apdu []byte, invoke byte, op string) ([]byte, error) {
	msg := bacnetcodec.BuildMessage(1, bacnetcodec.NPDUControlExpectReply, apdu)

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
		}
		if err := c.conn.SetDeadline(c.attemptDeadline(ctx)); err != nil {
			return nil, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"set udp deadline", true, err)
		}
		if err := c.conn.Write(msg); err != nil {
			return nil, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
				"udp write failed", true, err)
		}
		for {
			datagram, err := c.conn.Read()
			if err != nil {
				lastErr = c.readError(op, err, ctx)
				break
			}
			resp, _, perr := bacnetcodec.ParseMessage(datagram)
			if perr != nil {
				return nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"malformed BACnet/IP datagram: "+perr.Error(), false, perr)
			}
			// UDP 上可能收到上一条请求迟到的应答；invoke ID 不匹配就丢弃。
			if id, ok := bacnetcodec.AcknowledgeInvoke(resp); ok && id == invoke {
				return resp, nil
			}
		}
		// 只有超时才值得重试；协议错误与链路错误立即上报。
		if driver.KindOf(lastErr) != driver.ErrTimeout || ctx.Err() != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (c *connState) attemptDeadline(ctx context.Context) time.Time {
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

// pointPlan 是一个点位解析后的对象寻址。
type pointPlan struct {
	ref      driver.PointRef
	objType  int
	instance int
	property int
	index    int
}

func planPoint(p driver.PointRef) (pointPlan, error) {
	if p.Address.Kind != model.AddressObject {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: bacnet requires object addressing, got %q", p.ID, p.Address.Kind), false, nil)
	}
	objType, ok := bacnetcodec.ObjectTypeFromName(p.Address.ObjectType)
	if !ok {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: unknown object type %q (examples: %s)", p.ID, p.Address.ObjectType,
				strings.Join(bacnetcodec.ObjectTypeNames(), ", ")), false, nil)
	}
	if p.Address.Instance < 0 || p.Address.Instance > 0x3FFFFF {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: instance %d out of range 0..4194302", p.ID, p.Address.Instance), false, nil)
	}
	property, ok := bacnetcodec.PropertyFromName(p.Address.Property)
	if !ok {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: unknown property %q (examples: %s)", p.ID, p.Address.Property,
				strings.Join(bacnetcodec.PropertyNames(), ", ")), false, nil)
	}
	if p.Address.PropIndex < 0 {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: propIndex must be >= 0", p.ID), false, nil)
	}
	return pointPlan{
		ref: p, objType: objType, instance: p.Address.Instance,
		property: property, index: p.Address.PropIndex,
	}, nil
}

func (c *connState) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var points []driver.PointRef
	for _, span := range spans {
		points = append(points, span.Points...)
	}
	out := make([]driver.RawSample, 0, len(points))
	var firstErr error
	okSamples := 0
	for i, p := range points {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
			for _, rest := range points[i:] {
				out = append(out, errSample(rest.ID, e))
			}
			// 链路级：即便本轮已有好样本也要让调度器看到（周期被超时打断
			// 本身就是「设备太慢」），因此不走下面的部分失败闸门。
			return out, e
		}
		plan, err := planPoint(p)
		if err != nil {
			out = append(out, errSample(p.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		v, err := c.readOne(ctx, plan)
		if err != nil {
			out = append(out, errSample(p.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			// 链路级失败（超时/断链）时剩下的点不可能成功，继续逐个超时
			// 只会把一轮采集拖成 N 倍时长。
			if driver.IsLinkFailure(err) {
				for _, rest := range points[i+1:] {
					out = append(out, errSample(rest.ID, err))
				}
				return out, err
			}
			continue
		}
		okSamples++
		out = append(out, driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood})
	}
	// 部分失败语义（docs §3.3.1）：只要有一个点成功就不向上返回错误，
	// 否则一个属性名写错会让 collector 丢弃连接、整台设备每周期重连。
	return out, driver.PartialReadError(okSamples, firstErr)
}

func (c *connState) readOne(ctx context.Context, plan pointPlan) (model.Value, error) {
	invoke := c.nextInvoke()
	req, err := bacnetcodec.ReadPropertyRequest(invoke, plan.objType, plan.instance,
		plan.property, plan.index)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, invoke, "read")
	if err != nil {
		return model.Value{}, err
	}
	if bacnetcodec.ClassifyAPDU(resp[0]) != bacnetcodec.APDUKindComplexACK {
		return model.Value{}, c.apduError("read", resp, plan)
	}
	kind, service, _, payload, err := bacnetcodec.ParseACK(resp)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	if kind != bacnetcodec.APDUKindComplexACK || service != bacnetcodec.ServiceReadProperty {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("unexpected ACK service 0x%02X", service), false, nil)
	}
	appValue, has, err := bacnetcodec.ReadPropertyACK(payload)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"malformed ReadProperty-ACK: "+err.Error(), false, err)
	}
	if !has {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"ReadProperty-ACK carries no property value", false, nil)
	}
	tag, u, f, b, bl, err := bacnetcodec.DecodeApplicationValue(appValue)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"decode property value: "+err.Error(), false, err)
	}
	return toValue(plan, tag, u, f, b, bl)
}

// toValue 把 BACnet 应用值转成平台值。
//
// 对数值类型刻意宽松：同一台设备的 analog-input 可能回 REAL，也可能回
// UNSIGNED/SIGNED（老设备或网关转换过），把配置类型当成「怎么解释字节」
// 而不是「设备必须回什么标签」，现场会少一大半「能连上但读不出值」。
// 反过来 bool 与 string 严格：把字符串当数字读出来只会产生垃圾值。
func toValue(plan pointPlan, tag byte, u uint64, f float64, b []byte, bl bool) (model.Value, error) {
	switch plan.ref.DataType {
	case model.TypeBool:
		switch tag {
		case bacnetcodec.AppBoolean:
			return model.BoolValue(bl), nil
		case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated, bacnetcodec.AppSigned:
			return model.BoolValue(u != 0), nil
		}
		return model.Value{}, typeError(plan, tag, "bool")
	case model.TypeString:
		if tag != bacnetcodec.AppCharacterStr {
			return model.Value{}, typeError(plan, tag, "string")
		}
		return model.TextValue(model.TypeString, string(b)), nil
	case model.TypeBytes:
		switch tag {
		case bacnetcodec.AppOctetString, bacnetcodec.AppBitString, bacnetcodec.AppCharacterStr:
			return model.BytesValue(b), nil
		}
		return model.Value{}, typeError(plan, tag, "bytes")
	}

	// 数值类型。
	switch tag {
	case bacnetcodec.AppReal, bacnetcodec.AppDouble:
		return model.NumValue(plan.ref.DataType, f), nil
	case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated:
		return model.NumValue(plan.ref.DataType, float64(u)), nil
	case bacnetcodec.AppSigned:
		return model.NumValue(plan.ref.DataType, float64(int64(u))), nil
	}
	return model.Value{}, typeError(plan, tag, string(plan.ref.DataType))
}

func typeError(plan pointPlan, tag byte, want string) error {
	return driver.E(driver.ErrAddress, "decode", "",
		fmt.Sprintf("point %q (%s): device returned application tag %d, configured as %s",
			plan.ref.ID, bacnetcodec.FormatAddress(plan.objType, plan.instance, plan.property, plan.index),
			tag, want), false, nil)
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *connState) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
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
		if err != nil && (driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected) {
			// 链路已断：剩下的写请求不可能成功，直接标失败避免再等 N 个超时。
			for _, rest := range reqs[len(results):] {
				results = append(results, writeFail(rest, err))
			}
			break
		}
	}
	return results, firstErr
}

func (c *connState) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	plan, err := planPoint(driver.PointRef{
		ID: req.PointID, Address: req.Address, DataType: req.DataType, ByteOrder: req.ByteOrder,
	})
	if err != nil {
		return writeFail(req, err), err
	}
	appValue, err := encodeWriteValue(plan, req)
	if err != nil {
		return writeFail(req, err), err
	}
	invoke := c.nextInvoke()
	apdu, err := bacnetcodec.WritePropertyRequest(invoke, plan.objType, plan.instance,
		plan.property, plan.index, 0, appValue)
	if err != nil {
		e := driver.E(driver.ErrInternal, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, e), e
	}
	resp, err := c.exchange(ctx, apdu, invoke, "write")
	if err != nil {
		return writeFail(req, err), err
	}
	if bacnetcodec.ClassifyAPDU(resp[0]) != bacnetcodec.APDUKindSimpleACK {
		e := c.apduError("write", resp, plan)
		return writeFail(req, e), e
	}
	kind, service, _, _, err := bacnetcodec.ParseACK(resp)
	if err != nil {
		e := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, e), e
	}
	if kind != bacnetcodec.APDUKindSimpleACK || service != bacnetcodec.ServiceWriteProperty {
		e := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("unexpected ACK service 0x%02X", service), false, nil)
		return writeFail(req, e), e
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// encodeWriteValue 按对象语义选择应用值编码。
//
// present-value 的类型由对象决定，而不是由平台 dataType 决定：
// binary-value 的 present-value 是 ENUMERATED，analog-value 的是 REAL。
// 只按 dataType 编码会把 `true` 写成 BOOLEAN 标签，设备回
// invalid-data-type——这是 BACnet 现场最常见的写失败原因之一。
func encodeWriteValue(plan pointPlan, req driver.WriteRequest) ([]byte, error) {
	kind := bacnetcodec.KindOfObject(plan.objType)
	var tag byte = bacnetcodec.AppReal
	if plan.property == bacnetcodec.PropPresentValue {
		switch kind {
		case bacnetcodec.KindAnalog:
			tag = bacnetcodec.AppReal
		case bacnetcodec.KindBinary:
			tag = bacnetcodec.AppEnumerated
		case bacnetcodec.KindMultiState:
			tag = bacnetcodec.AppUnsigned
		case bacnetcodec.KindString:
			tag = bacnetcodec.AppCharacterStr
		}
	} else {
		switch req.DataType {
		case model.TypeBool:
			tag = bacnetcodec.AppBoolean
		case model.TypeString:
			tag = bacnetcodec.AppCharacterStr
		case model.TypeBytes:
			tag = bacnetcodec.AppOctetString
		case model.TypeFloat64:
			tag = bacnetcodec.AppDouble
		}
	}

	var (
		u   uint64
		f   float64
		b   []byte
		bl  bool
		err error
	)
	switch tag {
	case bacnetcodec.AppBoolean:
		if req.DataType != model.TypeBool {
			err = fmt.Errorf("point %q: boolean property requires bool value", req.PointID)
		}
		bl = req.Value.Bool
	case bacnetcodec.AppCharacterStr:
		if req.DataType != model.TypeString && req.DataType != model.TypeBytes {
			err = fmt.Errorf("point %q: character string property requires string value", req.PointID)
		}
		b = []byte(req.Value.Text)
	case bacnetcodec.AppOctetString:
		if req.DataType != model.TypeBytes && req.DataType != model.TypeString {
			err = fmt.Errorf("point %q: octet string property requires bytes value", req.PointID)
		}
		b = []byte(req.Value.Text)
	case bacnetcodec.AppEnumerated, bacnetcodec.AppUnsigned:
		n, ok := req.Value.AsFloat()
		// ENUMERATED / UNSIGNED 在 BACnet 里是整数值：带小数的 1.5 强转成
		// uint64 会被静默截成 1，设备收到并接受了另一个值。和 mc/s7/enip 的
		// checkRange 一样，非整数在写报文前判错而不是悄悄改掉。
		if !ok || n != math.Trunc(n) || n < 0 || n > float64(^uint32(0)) {
			err = fmt.Errorf("point %q: enumerated property requires a non-negative integer, got %s", req.PointID, req.Value.String())
		} else {
			u = uint64(n)
		}
	case bacnetcodec.AppDouble:
		n, ok := req.Value.AsFloat()
		if !ok {
			err = fmt.Errorf("point %q: numeric property requires a number, got %s", req.PointID, req.Value.String())
		} else {
			f = n
		}
	default: // AppReal
		n, ok := req.Value.AsFloat()
		// REAL 在 BACnet 里是 4 字节 float32。一个合法的 float64（例如点表配
		// dataType=float64、写 analog 的 present-value）可能超出 float32 范围，
		// 强转后变 +Inf / 下溢成 0——设备会原样接受，写下去的是另一个值。
		// 与 s7/enip/mc/ads 的 checkRange 纪律一致：越界/非有限在写报文前拒绝。
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			err = fmt.Errorf("point %q: numeric property requires a finite number, got %s", req.PointID, req.Value.String())
		} else if f32 := float32(n); math.IsInf(float64(f32), 0) || f32 == 0 && n != 0 {
			err = fmt.Errorf("point %q: value %s out of range for REAL (float32)", req.PointID, req.Value.String())
		} else {
			f = n
		}
	}
	if err != nil {
		e := driver.E(driver.ErrAddress, "write", "", err.Error(), false, nil)
		return nil, e
	}
	out, err := bacnetcodec.EncodeApplicationValue(tag, u, f, b, bl)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "write", "",
			fmt.Sprintf("point %q: %v", req.PointID, err), false, err)
	}
	return out, nil
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// apduError 把 Error / Reject / Abort 映射成驱动错误分类。
func (c *connState) apduError(op string, apdu []byte, plan pointPlan) error {
	switch bacnetcodec.ClassifyAPDU(apdu[0]) {
	case bacnetcodec.APDUKindError:
		_, class, code, err := bacnetcodec.ParseError(apdu)
		if err != nil {
			return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
				"malformed Error APDU: "+err.Error(), false, err)
		}
		return mapBacnetError(op, c.cfg.Device.ID, plan, class, code)
	case bacnetcodec.APDUKindReject:
		_, reason, _ := bacnetcodec.ParseReject(apdu)
		return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
			fmt.Sprintf("device rejected request (reason 0x%02X)", reason), false, nil)
	case bacnetcodec.APDUKindAbort:
		_, reason, _ := bacnetcodec.ParseAbort(apdu)
		return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
			fmt.Sprintf("device aborted transaction (reason 0x%02X)", reason), false, nil)
	}
	return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
		fmt.Sprintf("unexpected APDU 0x%02X", apdu[0]), false, nil)
}

func mapBacnetError(op, device string, plan pointPlan, class, code byte) error {
	addr := bacnetcodec.FormatAddress(plan.objType, plan.instance, plan.property, plan.index)
	msg := fmt.Sprintf("%s: device error class %d code %d", addr, class, code)
	switch class {
	case bacnetcodec.ErrorClassObject:
		// 对象不存在：配置里写了设备没有的实例号。
		return driver.E(driver.ErrAddress, op, device, msg, false, nil)
	case bacnetcodec.ErrorClassProperty:
		switch code {
		case bacnetcodec.ErrorCodeUnknownProperty, bacnetcodec.ErrorCodeInvalidArrayIndex:
			return driver.E(driver.ErrAddress, op, device, msg, false, nil)
		case bacnetcodec.ErrorCodeWriteAccessDenied, bacnetcodec.ErrorCodeReadAccessDenied:
			return driver.E(driver.ErrPermission, op, device, msg, false, nil)
		case bacnetcodec.ErrorCodeValueOutOfRange, bacnetcodec.ErrorCodeInvalidDataType:
			return driver.E(driver.ErrAddress, op, device, msg, false, nil)
		}
	case bacnetcodec.ErrorClassResources:
		// 设备资源耗尽（缓冲满、段不支持）：链路仍在，属于可自愈。
		return driver.E(driver.ErrDisconnected, op, device, msg, true, nil)
	case bacnetcodec.ErrorClassSecurity:
		return driver.E(driver.ErrPermission, op, device, msg, false, nil)
	}
	return driver.E(driver.ErrProtocol, op, device, msg, false, nil)
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
