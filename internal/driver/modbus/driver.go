// Package modbus 实现 Modbus 主站驱动，覆盖 TCP 与 RTU 两种链路。
//
// 分层：driver.Meta（能力声明）→ Conn（事务/错误分类）→
// codec/modbus（纯函数编解码）→ transport（字节流）。
//
// TCP 与 RTU 只在「分帧」这一层不同，其余（批读、写入、异常码分类、
// 部分失败保留好样本）完全共用同一份代码。这是刻意的：两份实现迟早
// 会在异常路径上分叉。
package modbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
	"uic/internal/transport/serialport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// SerialOpener 允许测试注入内存串口。
//
// 定义为 serialport.Opener 的别名而不是新类型：否则「共享串口」包与调用方
// 之间每次传递都要显式转换，而那些转换点恰好散落在最容易漏改的错误分支里。
type SerialOpener = serialport.Opener

// Driver 是 Modbus 主站驱动。
type Driver struct {
	dial   Dialer
	serial SerialOpener
}

// New 返回使用真实 TCP 与真实串口的驱动。
func New() *Driver {
	return &Driver{
		dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
			return transport.DialTCP(ctx, addr, timeout)
		},
		serial: transport.OpenSerial,
	}
}

// NewWithDialer 返回使用自定义 TCP 链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	drv := New()
	drv.dial = d
	return drv
}

// NewWithSerial 返回使用自定义串口链路的驱动（测试用）。
func NewWithSerial(o SerialOpener) *Driver {
	drv := New()
	drv.serial = o
	return drv
}

// Meta 声明驱动能力。Modbus 不支持订阅、浏览与设备发现。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "modbus-tcp",
		Name:         "Modbus TCP",
		Version:      "0.2.0",
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
				Placeholder: "192.168.1.10:502", Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 4, Scope: "channel",
				Help: "同一通道允许的最大在飞请求数"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "区", Type: "enum", Required: true,
				Enum: []string{"holding", "input", "coil", "discrete"}, Scope: "point"},
			{Name: "address.offset", Label: "偏移", Type: "int", Required: true, Scope: "point"},
			{Name: "address.bit", Label: "位", Type: "int", Default: -1, Scope: "point"},
		},
		Description: "Modbus TCP 主站，支持 FC1/2/3/4/5/6/15/16",
	}
}

// Open 建立链路并按传输类型选择分帧方式。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	tr, err := d.openTransport(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &conn{
		cfg:   cfg,
		codec: mbcodec.NewCodec(cfg.Device.ID),
		tr:    tr,
	}, nil
}

func (d *Driver) openTransport(ctx context.Context, cfg driver.OpenConfig) (framed, error) {
	t := cfg.Channel.Transport
	switch t.Kind {
	case "tcp":
		stream, err := d.dial(ctx, t.Address, cfg.Channel.EffectiveTimeout())
		if err != nil {
			return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID, "connect failed", true, err)
		}
		return &tcpFraming{stream: stream, unit: uint8(cfg.Device.UnitID)}, nil
	case "serial":
		// 串口走独立的 modbus-rtu 驱动（rtu.go）：它需要端口复用与总线
		// 串行化，这两件事在只处理 TCP 的驱动里没有位置。
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"serial transport requires driver modbus-rtu", false, nil)
	case "":
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.kind required (tcp|serial)", false, nil)
	}
	return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
		fmt.Sprintf("modbus does not support transport %q", t.Kind), false, nil)
}

// framed 是一次 PDU 往返。TCP 与 RTU 各自实现，上层 conn 不感知差异。
type framed interface {
	RoundTrip(ctx context.Context, pdu []byte) ([]byte, error)
	Close() error
}

type conn struct {
	mu sync.Mutex
	// wmu 串行化写入。写本身已经是低频路径，而「读-改-写」要求同一寄存器的
	// 两次事务之间不被同一驱动的其他写入插进来——否则两个并发写入会互相
	// 覆盖对方改的位。读不加这把锁：读之间没有互斥需求。
	wmu   sync.Mutex
	cfg   driver.OpenConfig
	tr    framed
	codec *mbcodec.Codec
}

func (c *conn) Probe(ctx context.Context) error {
	// 读 1 个保持寄存器；即使返回异常码，也说明链路可达。
	pdu := []byte{mbcodec.FCReadHoldingRegs, 0, 0, 0, 1}
	_, err := c.tr.RoundTrip(ctx, pdu)
	return err
}

func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	var out []driver.RawSample
	var firstErr error
	ok := 0
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return out, driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
		}
		samples, err := c.readSpan(ctx, span)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			out = append(out, failedSamples(span, err)...)
			continue
		}
		ok++
		out = append(out, samples...)
	}
	if ok == 0 && firstErr != nil {
		return out, firstErr
	}
	return out, nil
}

func (c *conn) readSpan(ctx context.Context, span driver.ReadSpan) ([]driver.RawSample, error) {
	pdu, err := c.codec.EncodeRead(span)
	if err != nil {
		return nil, err
	}
	resp, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return nil, err
	}
	return c.codec.DecodeRead(span, resp)
}

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		if needsBitRMW(req) {
			res, err := c.writeBitRMW(ctx, req)
			results = append(results, res)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		pdu, err := c.codec.EncodeWrite(req)
		if err != nil {
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: model.QualityBad, Err: err})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		resp, err := c.tr.RoundTrip(ctx, pdu)
		if err != nil {
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res, err := c.codec.DecodeWrite(req, resp)
		if err != nil {
			res.Quality = model.QualityBad
			res.Err = err
			if firstErr == nil {
				firstErr = err
			}
		}
		results = append(results, res)
	}
	return results, firstErr
}

// needsBitRMW 判断一次写入是否需要「读-改-写」：布尔点落在保持寄存器里，
// 且指明了具体位号。线圈是整位区（FC5 直接写），不在此列。
func needsBitRMW(req driver.WriteRequest) bool {
	return req.DataType == model.TypeBool &&
		req.Address.Kind == model.AddressRegister &&
		mbcodec.IsHoldingArea(req.Address.Area) &&
		req.Address.Bit >= 0
}

// writeBitRMW 以「读整字 → 改一位 → 写整字」完成寄存器内位写。
//
// 为什么不能直接写 FC6：写 0/1 会把同一寄存器里的其他 15 位一并覆盖。
// 这类错误在调试时表现为「改了 A 点，B 点跟着变了」，而配置与代码都看不出
// 问题——必须从协议层就避免。
//
// 原子性边界（诚实说明）：
//   - 同一驱动的并发写入由 wmu 串行化，两个位写不会互相吞掉；
//   - 同一串口总线上的其他设备若恰好写同一个 unit 的同一个寄存器，仍可能
//     插在这两帧之间。那是配置冲突（一个寄存器只能有一个所有者），不是
//     驱动能替用户解决的；文档里已写明。
//   - 另一个主站（SCADA 直连同一从站）不受本驱动控制，同样需要现场约束。
func (c *conn) writeBitRMW(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	bit := req.Address.Bit
	if bit < 0 || bit > 15 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			"register bit write requires address.bit 0..15", false, nil)
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}

	pdu, err := c.codec.EncodeReadRegisters(req.Address.Offset, 1)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}
	resp, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err}, err
	}
	words, err := c.codec.DecodeRegisters(resp, 1)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}, err
	}

	next := mbcodec.WordSetBit(words[0], bit, req.Value.Bool)
	wpdu, err := c.codec.EncodeWriteRegister(req.Address.Offset, next)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}
	resp, err = c.tr.RoundTrip(ctx, wpdu)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err}, err
	}
	res, err := c.codec.DecodeWrite(req, resp)
	if err != nil {
		res.Quality = model.QualityBad
		res.Err = err
	}
	return res, err
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tr == nil {
		return nil
	}
	err := c.tr.Close()
	c.tr = nil
	return err
}

func failedSamples(span driver.ReadSpan, err error) []driver.RawSample {
	q := qualityOf(err)
	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		out = append(out, driver.RawSample{PointID: p.ID, Quality: q, Err: err})
	}
	return out
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
	if errors.Is(err, errClosedStream) {
		return driver.E(driver.ErrDisconnected, op, "", "connection closed", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, "", "network error", true, err)
}

var errClosedStream = errors.New("transport: closed")
