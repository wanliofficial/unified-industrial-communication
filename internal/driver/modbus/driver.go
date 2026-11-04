// Package modbus 实现 Modbus TCP 主站驱动。
//
// 分层：driver.Meta（能力声明）→ Conn（事务/错误分类）→
// codec/modbus（纯函数编解码）→ transport（字节流与 MBAP 分帧）。
package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 Modbus TCP 驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return transport.DialTCP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回使用自定义链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明驱动能力。Modbus 不支持订阅、浏览与设备发现。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "modbus-tcp",
		Name:         "Modbus TCP",
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

// Open 建立连接。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	if cfg.Channel.Transport.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"modbus-tcp requires tcp transport", false, nil)
	}
	stream, err := d.dial(ctx, cfg.Channel.Transport.Address, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID, "connect failed", true, err)
	}
	return &conn{
		cfg:    cfg,
		stream: stream,
		codec:  mbcodec.NewCodec(cfg.Device.ID),
		tr:     &mbTransport{stream: stream, unit: uint8(cfg.Device.UnitID)},
	}, nil
}

type conn struct {
	mu     sync.Mutex
	cfg    driver.OpenConfig
	stream transport.Stream
	tr     *mbTransport
	codec  *mbcodec.Codec
	tx     uint16
}

func (c *conn) Probe(ctx context.Context) error {
	// 读 1 个保持寄存器；即使返回异常码，也说明链路可达。
	pdu := []byte{mbcodec.FCReadHoldingRegs, 0, 0, 0, 1}
	_, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return err
	}
	return nil
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
	return c.stream.Close()
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

// ---------------------------------------------------------------------------
// MBAP 分帧
// ---------------------------------------------------------------------------

type mbTransport struct {
	stream transport.Stream
	unit   uint8
}

// RoundTrip 写一个 PDU（自动加 MBAP），再按长度字段读满整帧并返回 PDU。
func (t *mbTransport) RoundTrip(ctx context.Context, pdu []byte) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := t.stream.(interface{ SetDeadline(time.Time) error }); ok {
			_ = s.SetDeadline(dl)
		}
	}
	tx := nextTx()
	adu := mbcodec.BuildTCP(mbcodec.Frame{Transaction: tx, Unit: t.unit, PDU: pdu})
	if err := t.stream.Write(adu); err != nil {
		return nil, classifyNetErr("write", err)
	}
	head, err := t.stream.ReadFull(7)
	if err != nil {
		return nil, classifyNetErr("read-header", err)
	}
	length := int(binary.BigEndian.Uint16(head[4:6]))
	if length < 2 || length > 260 {
		return nil, driver.E(driver.ErrProtocol, "read", "", fmt.Sprintf("invalid MBAP length %d", length), false, nil)
	}
	body, err := t.stream.ReadFull(length - 1)
	if err != nil {
		return nil, classifyNetErr("read-body", err)
	}
	return body, nil
}

func (t *mbTransport) Close() error { return t.stream.Close() }

var txCounter uint32

func nextTx() uint16 {
	txCounter++
	return uint16(txCounter)
}

func classifyNetErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return driver.E(driver.ErrTimeout, op, "", "network timeout", true, err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return driver.E(driver.ErrDisconnected, op, "", "connection closed", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return driver.E(driver.ErrTimeout, op, "", "deadline exceeded", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, "", "network error", true, err)
}
