package modbus

import (
	"context"
	"fmt"
	"sync"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/transport"
	"uic/internal/transport/serialport"
)

// ASCII 是 Modbus ASCII 主站驱动。
//
// 与 RTU 共用 conn / codec，只在分帧层不同——异常码分类、部分失败保留
// 好样本、批量写逐项结果这些语义如果分两份实现，迟早会在某条错误路径
// 上分叉，而那条路径通常只在现场出现。
type ASCII struct {
	*Driver
}

// NewASCII 返回使用真实串口的 ASCII 驱动。
func NewASCII() *ASCII { return &ASCII{Driver: New()} }

// NewASCIIWithSerial 返回注入串口实现的 ASCII 驱动（测试用）。
func NewASCIIWithSerial(o SerialOpener) *ASCII {
	return &ASCII{Driver: NewWithSerial(o)}
}

// Meta 声明 ASCII 能力。除 ID、名称、传输与描述外与 RTU 一致。
func (d *ASCII) Meta() driver.Meta {
	m := d.Driver.Meta()
	m.ID = "modbus-ascii"
	m.Name = "Modbus ASCII"
	m.Version = "0.3.0"
	m.Transports = []string{"serial"}
	m.ConfigFields = []driver.ConfigField{
		{Name: "transport.port", Label: "串口", Type: "string", Required: true,
			Placeholder: "/dev/ttyUSB0", Scope: "channel"},
		{Name: "transport.baud", Label: "波特率", Type: "int", Default: 9600, Scope: "channel",
			Help: "300/600/1200/2400/4800/9600/19200/38400/57600/115200/230400/460800/921600"},
		{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: 7, Scope: "channel",
			Help: "ASCII 帧是 7 位文本，但现代设备普遍支持 8 位；以设备手册为准"},
		{Name: "transport.parity", Label: "校验", Type: "enum", Default: "even", Scope: "channel",
			Enum: []string{"none", "even", "odd"},
			Help: "ASCII 规范推荐偶校验，实际按设备手册配置"},
		{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: 1, Scope: "channel"},
		{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
			Help: "半双工总线必须串行；共享同一串口的设备由驱动统一串行化"},
	}
	m.Description = "Modbus ASCII 主站（串口 / 半双工），支持 FC1/2/3/4/5/6/15/16"
	return m
}

// Open 打开串口并建立 ASCII 分帧。
func (d *ASCII) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "serial" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("modbus-ascii requires serial transport, got %q", t.Kind), false, nil)
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
	// proto 参与端口冲突判定：同一根物理线上不可能同时跑 RTU 与 ASCII，
	// 静默复用会让两种分帧交错，症状是「随机 CRC/LRC 错」。
	ref, err := serialport.Acquire("ascii", sc, opener)
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "open", cfg.Device.ID,
			fmt.Sprintf("open serial %s failed", sc.Port), true, err)
	}

	fr := &asciiPortFraming{ref: ref, unit: uint8(cfg.Device.UnitID),
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

// asciiFraming 在一条字节流上做 ASCII 分帧。
type asciiFraming struct {
	stream transport.Stream
	unit   uint8
	gap    time.Duration
}

func (t *asciiFraming) ProbeLink(ctx context.Context) error {
	// 与 RTU 同理：刚打开时线上可能还有上次崩溃留下的残留字符，
	// 不清掉的话第一帧会从半个旧报文中间开始。
	if f, ok := t.stream.(transport.Flusher); ok {
		if err := f.Drain(); err != nil {
			return driver.E(driver.ErrDisconnected, "open", "", "flush serial failed", true, err)
		}
	}
	if t.gap > 0 {
		select {
		case <-ctx.Done():
			return driver.E(driver.ErrTimeout, "open", "", "context done", true, ctx.Err())
		case <-time.After(t.gap):
		}
	}
	return nil
}

func (t *asciiFraming) RoundTrip(ctx context.Context, pdu []byte) ([]byte, error) {
	if len(pdu) == 0 {
		return nil, driver.E(driver.ErrInternal, "encode", "", "empty pdu", false, nil)
	}
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := t.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if f, ok := t.stream.(transport.Flusher); ok {
		_ = f.Drain()
	}
	if err := t.stream.Write(mbcodec.BuildASCII(t.unit, pdu)); err != nil {
		return nil, classifyNetErr("write", err)
	}

	frame, err := t.readFrame(ctx)
	if err != nil {
		return nil, err
	}
	unit, respPDU, err := mbcodec.ParseASCII(frame)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, "read", "", err.Error(), true, err)
	}
	if unit != t.unit {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("ascii unit mismatch: got %d want %d", unit, t.unit), true, nil)
	}
	if len(respPDU) > 0 && respPDU[0]&0x7f != pdu[0] {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("ascii function echo %#x, want %#x (unit %d)", respPDU[0], pdu[0], unit), true, nil)
	}
	return respPDU, nil
}

// readFrame 读到 CRLF 为止。
//
// ASCII 没有长度字段，分帧只能靠终止符。这里先跳过 ':' 之前的杂散字节
// （半双工总线上隐私残留很常见），再逐字节累积到 '\n'。逐字节读看起来
// 朴素，但串口本身按字符到达，9600 波特下一个字符就要 1ms，系统调用
// 次数根本不是这条路径的瓶颈；反过来，一次 ReadFull(N) 需要预知 N，
// 而 N 正是我们不知道的东西。
func (t *asciiFraming) readFrame(ctx context.Context) ([]byte, error) {
	// 先对齐到 ':'。最多跳过 64 个字符，防止把持续噪声当成帧头。
	for skipped := 0; ; skipped++ {
		ch, err := t.stream.ReadFull(1)
		if err != nil {
			return nil, classifyNetErr("read", err)
		}
		if ch[0] == ':' {
			break
		}
		if skipped >= 64 {
			return nil, driver.E(driver.ErrProtocol, "read", "",
				fmt.Sprintf("ascii no ':' start in %d bytes (got %#02x)", skipped+1, ch[0]), true, nil)
		}
	}

	frame := make([]byte, 0, 32)
	frame = append(frame, ':')
	for len(frame) < mbcodec.ASCIIMaxFrame {
		ch, err := t.stream.ReadFull(1)
		if err != nil {
			return nil, driver.E(driver.ErrTimeout, "read", "",
				fmt.Sprintf("ascii truncated frame after %d chars", len(frame)), true, err)
		}
		frame = append(frame, ch[0])
		if ch[0] == '\n' {
			return frame, nil
		}
	}
	return nil, driver.E(driver.ErrProtocol, "read", "",
		fmt.Sprintf("ascii frame exceeds %d chars without CRLF", mbcodec.ASCIIMaxFrame), false, nil)
}

// asciiPortFraming 在共享串口上做 ASCII 分帧：整个写-读过程持有总线锁。
type asciiPortFraming struct {
	ref     *serialport.Ref
	unit    uint8
	gap     time.Duration
	closeMu sync.Mutex
	closed  bool
}

func (t *asciiPortFraming) ProbeLink(ctx context.Context) error {
	t.ref.Lock()
	defer t.ref.Unlock()
	inner := &asciiFraming{stream: t.ref, unit: t.unit, gap: t.gap}
	return inner.ProbeLink(ctx)
}

func (t *asciiPortFraming) RoundTrip(ctx context.Context, pdu []byte) ([]byte, error) {
	t.ref.Lock()
	defer t.ref.Unlock()

	inner := &asciiFraming{stream: t.ref, unit: t.unit, gap: t.gap}
	resp, err := inner.RoundTrip(ctx, pdu)
	if err != nil {
		return nil, err
	}
	if t.gap > 0 {
		select {
		case <-ctx.Done():
			return resp, nil
		case <-time.After(t.gap):
		}
	}
	return resp, nil
}

func (t *asciiPortFraming) Close() error {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil
	}
	t.closed = true
	t.closeMu.Unlock()
	return t.ref.Close()
}
