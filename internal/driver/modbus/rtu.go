package modbus

import (
	"context"
	"fmt"
	"sync"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/transport"
)

// RTU 是 Modbus RTU 主站驱动。
//
// 它与 TCP 驱动共用 conn / codec，只在分帧层不同（framing.go）。这样
// 异常码分类、部分失败保留好样本、批量写逐项结果等语义不会分叉。
type RTU struct {
	*Driver
}

// NewRTU 返回使用真实串口的 RTU 驱动。
func NewRTU() *RTU { return &RTU{Driver: New()} }

// NewRTUWithSerial 返回注入串口实现的 RTU 驱动（测试用）。
func NewRTUWithSerial(o SerialOpener) *RTU {
	return &RTU{Driver: NewWithSerial(o)}
}

// Meta 声明 RTU 能力。与 TCP 相比只差 ID、名称、传输与配置字段。
func (d *RTU) Meta() driver.Meta {
	m := d.Driver.Meta()
	m.ID = "modbus-rtu"
	m.Name = "Modbus RTU"
	m.Version = "0.2.0"
	m.Transports = []string{"serial"}
	m.ConfigFields = []driver.ConfigField{
		{Name: "transport.port", Label: "串口", Type: "string", Required: true,
			Placeholder: "/dev/ttyUSB0", Scope: "channel"},
		{Name: "transport.baud", Label: "波特率", Type: "int", Default: 9600, Scope: "channel",
			Help: "300/600/1200/2400/4800/9600/19200/38400/57600/115200/230400/460800/921600"},
		{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 8, Scope: "channel"},
		{Name: "transport.parity", Label: "校验", Type: "enum", Default: "none", Scope: "channel",
			Enum: []string{"none", "even", "odd"}},
		{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
		{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
			Help: "RS-485 是半双工总线，必须串行；共享同一串口的设备由驱动统一串行化"},
	}
	m.Description = "Modbus RTU 主站（串口 / 半双工），支持 FC1/2/3/4/5/6/15/16"
	return m
}

// Open 打开串口并建立 RTU 分帧。
//
// 同一串口上挂多台从站时，每个设备会各自 Open 一次。物理串口由
// acquirePort 做引用计数复用，事务由 portRef 的总线锁串行化。
func (d *RTU) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "serial" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("modbus-rtu requires serial transport, got %q", t.Kind), false, nil)
	}
	sc := transport.SerialConfig{
		Port:     t.Port,
		Baud:     t.Baud,
		DataBits: t.DataBits,
		Parity:   t.Parity,
		StopBits: t.StopBits,
	}.Normalized()
	if err := sc.Validate(); err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, err.Error(), false, err)
	}

	opener := d.serial
	if opener == nil {
		opener = transport.OpenSerial
	}
	ref, err := acquirePort(sc, opener)
	if err != nil {
		// 打开串口失败通常是「设备被拔了/权限不对/参数被占用」，属可重试。
		return nil, driver.E(driver.ErrDisconnected, "open", cfg.Device.ID,
			fmt.Sprintf("open serial %s failed", sc.Port), true, err)
	}

	fr := &rtuPortFraming{ref: ref, unit: uint8(cfg.Device.UnitID),
		gap: effectiveFrameGap(t)}
	if err := fr.ProbeLink(ctx); err != nil {
		_ = ref.Close()
		return nil, err
	}
	return &conn{
		cfg:   cfg,
		codec: mbcodec.NewCodec(cfg.Device.ID),
		tr:    fr,
	}, nil
}

// rtuPortFraming 在共享串口上做 RTU 分帧：整个写-读过程持有总线锁。
type rtuPortFraming struct {
	ref     *portRef
	unit    uint8
	gap     time.Duration
	closeMu sync.Mutex
	closed  bool
}

func (t *rtuPortFraming) ProbeLink(ctx context.Context) error {
	t.ref.port.mu.Lock()
	defer t.ref.port.mu.Unlock()

	inner := &rtuFraming{stream: t.ref, unit: t.unit, gap: t.gap}
	return inner.ProbeLink(ctx)
}

func (t *rtuPortFraming) RoundTrip(ctx context.Context, pdu []byte) ([]byte, error) {
	t.ref.port.mu.Lock()
	defer t.ref.port.mu.Unlock()

	inner := &rtuFraming{stream: t.ref, unit: t.unit, gap: t.gap}
	resp, err := inner.RoundTrip(ctx, pdu)
	if err != nil {
		return nil, err
	}
	// 帧间静默：给从站留出从发送切回接收的时间。
	if t.gap > 0 {
		select {
		case <-ctx.Done():
			return resp, nil
		case <-time.After(t.gap):
		}
	}
	return resp, nil
}

// Close 释放一次端口引用；最后一个使用者负责真正关闭。
func (t *rtuPortFraming) Close() error {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil
	}
	t.closed = true
	t.closeMu.Unlock()
	return t.ref.Close()
}
