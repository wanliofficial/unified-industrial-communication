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
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// SerialOpener 允许测试注入内存串口。
type SerialOpener func(cfg transport.SerialConfig) (transport.Stream, error)

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
	mu    sync.Mutex
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
	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
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
