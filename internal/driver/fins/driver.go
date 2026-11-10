// Package fins 实现 FINS/TCP 主站驱动。
//
// 分层：Meta（能力声明）→ 本文件的 framing（FINS/TCP 封套与节点号协商）
// → driver/finscore（FINS 命令语义与内存区读写）→ codec/fins（纯函数编解码）。
//
// 这一层只做三件与「TCP 封装」有关的事：
//
//  1. **节点号协商。** 连上就发命令 0，拿回客户端/服务端节点号。没有它，
//     帧头里的 DA1/SA1 无从填写——客户端节点号是 PLC 分配的，不能自己编。
//  2. **定长封套。** 每帧前面 16 字节（`FINS` + 长度 + 命令 + 错误码）。
//     长度字段是「命令+错误码+负载」，不含 magic 与长度字段自身；写成
//     总长会让对端把后 8 字节当成下一帧的开头。
//  3. **SID 分配与应答校验。** SID 由这里统一递增并比对，调用方构造帧时
//     第 10 字节留空即可。
//
// 内存区语义（区名、字/位混排、字节序、错误码分类）全部在 finscore，与
// Host Link 共用同一份实现。想改「bool 点位在字区上怎么写」这类逻辑，改
// 一处即可，不会出现两个驱动行为不一致。
package fins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	finscodec "uic/internal/codec/fins"
	"uic/internal/driver"
	"uic/internal/driver/finscore"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 FINS/TCP 主站驱动。
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

// Meta 声明 FINS 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "fins",
		Name:         "Omron FINS/TCP",
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
				Placeholder: "192.168.1.10:9600", Scope: "channel",
				Help: "只填主机名时默认端口 9600"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "FINS/TCP 是单连接一问一答；建议 1"},
			{Name: "device.unitId", Label: "目标节点号", Type: "int", Default: 0, Scope: "device",
				Help: "0 表示使用节点号协商返回的 PLC 节点"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "内存区", Type: "enum", Required: true,
				Enum: finscodec.Areas(), Scope: "point",
				Help: "cio/wr/hr/ar/dm/em/em0..em12；D/W/H/A 是别名"},
			{Name: "address.offset", Label: "字地址", Type: "int", Required: true, Scope: "point",
				Help: "从 0 开始；CIO0.3 的 offset 是 0、bit 是 3"},
			{Name: "address.length", Label: "字数", Type: "int", Scope: "point",
				Help: "多字类型按数据类型推导；字符串/字节用此宽度"},
			{Name: "address.bit", Label: "位号", Type: "int", Default: -1, Scope: "point",
				Help: "bool 点位必填：0..15（0 = LSB）"},
		},
		Description: "欧姆龙 FINS/TCP（端口 9600），支持 CIO/WR/HR/AR/DM/EM 内存区的字/位读写",
	}
}

// Open 建立 TCP 链路并完成节点号协商。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("fins requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if !strings.Contains(addr, ":") {
		addr += ":" + finscodec.DefaultPort
	}
	stream, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	f := &framing{stream: stream}
	if err := f.handshake(ctx, cfg.Device.UnitID); err != nil {
		_ = stream.Close()
		kind := driver.ErrProtocol
		retry := false
		var te *finscodec.TCPError
		if errors.As(err, &te) {
			kind = driver.ErrDisconnected
			retry = true
		} else if driver.KindOf(err) != "" {
			kind = driver.KindOf(err)
			retry = true
		}
		return nil, driver.E(kind, "handshake", cfg.Device.ID, "FINS node negotiation failed", retry, err)
	}
	return finscore.New(cfg, f, finscore.DefaultLimits()), nil
}

// ---------------------------------------------------------------------------
// TCP 分帧与节点号协商
// ---------------------------------------------------------------------------

// framing 实现 finscore.Transactor。
type framing struct {
	stream transport.Stream

	mu         sync.Mutex
	sid        byte
	clientNode byte
	serverNode byte
	destNode   byte
}

// Header 返回本次事务的 FINS 请求帧头。
func (f *framing) Header() finscodec.Header {
	return finscodec.CommandHeader(f.destNode, f.clientNode, 0)
}

// handshake 完成 FINS/TCP 节点号协商（命令 0 → 1）。
func (f *framing) handshake(ctx context.Context, destOverride int) error {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := f.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := f.stream.Write(finscodec.BuildNodeRequest(0)); err != nil {
		return classifyNetErr("handshake-write", err)
	}
	head, err := f.stream.ReadFull(16)
	if err != nil {
		return classifyNetErr("handshake-read", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		return driver.E(driver.ErrProtocol, "handshake", "", err.Error(), false, err)
	}
	if th.Command != finscodec.TCPCommandNodeResponse {
		return driver.E(driver.ErrProtocol, "handshake", "",
			fmt.Sprintf("unexpected FINS/TCP command %d", th.Command), false, nil)
	}
	if th.Error != 0 {
		return &finscodec.TCPError{Code: th.Error}
	}
	payloadLen := th.Length - 8
	if payloadLen < 8 {
		return driver.E(driver.ErrProtocol, "handshake", "",
			fmt.Sprintf("node response payload too short: %d", payloadLen), false, nil)
	}
	payload, err := f.stream.ReadFull(payloadLen)
	if err != nil {
		return classifyNetErr("handshake-payload", err)
	}
	f.clientNode = payload[3]
	f.serverNode = payload[7]
	if f.clientNode == 0 && f.serverNode == 0 {
		return driver.E(driver.ErrProtocol, "handshake", "", "PLC returned no node addresses", false, nil)
	}
	if destOverride > 0 && destOverride <= 254 {
		f.destNode = byte(destOverride)
	} else {
		f.destNode = f.serverNode
	}
	return nil
}

// RoundTrip 发送一个 FINS 帧并读回应答。SID 由这里统一分配并校验。
func (f *framing) RoundTrip(ctx context.Context, req []byte) (finscodec.Frame, error) {
	if len(req) < 12 {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", "",
			"FINS frame too short", false, nil)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sid++
	sid := f.sid
	frame := append([]byte(nil), req...)
	frame[9] = sid

	pkt, err := finscodec.BuildTCPFrame(frame)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", "", err.Error(), false, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := f.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := f.stream.Write(pkt); err != nil {
		return finscodec.Frame{}, classifyNetErr("write", err)
	}
	head, err := f.stream.ReadFull(16)
	if err != nil {
		return finscodec.Frame{}, classifyNetErr("read-header", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "", err.Error(), false, err)
	}
	if th.Command != finscodec.TCPCommandFrame {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("unexpected FINS/TCP command %d", th.Command), false, nil)
	}
	if th.Error != 0 {
		// 封装层错误码大多是「连接数满 / 节点被占用 / 对端重启」这类瞬态
		// 问题，必须带上「可重试」的分类；否则调度器会当作内部错误直接
		// 熔断，把一次可以自愈的重连拖成需要人工介入。
		return finscodec.Frame{}, driver.E(driver.ErrDisconnected, "read", "",
			"FINS/TCP transport error", true, &finscodec.TCPError{Code: th.Error})
	}
	body, err := f.stream.ReadFull(th.Length - 8)
	if err != nil {
		return finscodec.Frame{}, classifyNetErr("read-body", err)
	}
	resp, err := finscodec.Parse(body)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "", err.Error(), false, err)
	}
	if !resp.IsResponse() {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "",
			"FINS frame is not a response", false, nil)
	}
	if resp.Header.SID != sid {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("SID mismatch: sent %d, got %d", sid, resp.Header.SID), false, nil)
	}
	if resp.EndCode != 0 {
		return resp, &finscodec.EndCodeError{Code: resp.EndCode}
	}
	return resp, nil
}

func (f *framing) Close() error { return f.stream.Close() }

func classifyNetErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, "", "i/o timeout", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, "", "connection lost", true, err)
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	if t, ok := err.(timeout); ok && t.Timeout() {
		return true
	}
	return false
}
