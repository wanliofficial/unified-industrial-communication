package modbus

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

var txCounter atomic.Uint32

func nextTx() uint16 {
	return uint16(txCounter.Add(1))
}

// tcpFraming 是 Modbus TCP 的 MBAP 分帧。
type tcpFraming struct {
	stream transport.Stream
	unit   uint8
}

// RoundTrip 写一个 PDU（自动加 MBAP），再按长度字段读满整帧并返回 PDU。
func (t *tcpFraming) RoundTrip(ctx context.Context, pdu []byte) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := t.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	adu := mbcodec.BuildTCP(mbcodec.Frame{Transaction: nextTx(), Unit: t.unit, PDU: pdu})
	if err := t.stream.Write(adu); err != nil {
		return nil, classifyNetErr("write", err)
	}
	head, err := t.stream.ReadFull(7)
	if err != nil {
		return nil, classifyNetErr("read-header", err)
	}
	length := int(binary.BigEndian.Uint16(head[4:6]))
	if length < 2 || length > 260 {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("invalid MBAP length %d", length), false, nil)
	}
	body, err := t.stream.ReadFull(length - 1)
	if err != nil {
		return nil, classifyNetErr("read-body", err)
	}
	return body, nil
}

func (t *tcpFraming) Close() error { return t.stream.Close() }

// rtuFraming 是 Modbus RTU 的分帧。
//
// RTU 与 TCP 的根本差异不在 CRC，而在**没有长度字段**：响应长度只能由
// 功能码 + 字节计数推导。因此这里必须逐字节探测，不能像 TCP 那样一次
// ReadFull(7)。半包是常态（串口一次只到 1~2 字节），也是「能跑通」与
// 「能上生产」之间最大的鸿沟。
type rtuFraming struct {
	stream transport.Stream
	unit   uint8
	// gap 是帧间静默间隔。RS-485 上从站需要时间从接收切到发送，
	// 也要防止把上一帧的尾巴当成本帧的开头。
	gap time.Duration
}

func (t *rtuFraming) ProbeLink(ctx context.Context) error {
	// 刚打开串口时线上可能还有上一次进程崩溃前的残留字节。这里主动
	// 丢弃一次，否则第一批请求会从半个旧帧中间开始解析。
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

// RoundTrip 写 RTU 帧，再按推导出的长度读满响应。
func (t *rtuFraming) RoundTrip(ctx context.Context, pdu []byte) ([]byte, error) {
	if len(pdu) == 0 {
		return nil, driver.E(driver.ErrInternal, "encode", "", "empty pdu", false, nil)
	}
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := t.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	// 半双工纪律：发之前清空接收缓冲，避免把上一轮迟到的响应当成本轮。
	if f, ok := t.stream.(transport.Flusher); ok {
		_ = f.Drain()
	}
	frame := mbcodec.BuildRTU(t.unit, pdu)
	if err := t.stream.Write(frame); err != nil {
		return nil, classifyNetErr("write", err)
	}

	body, err := t.readResponse(ctx, pdu)
	if err != nil {
		return nil, err
	}
	// CRC 校验与地址过滤都在 ParseRTU 内完成。
	unit, respPDU, err := mbcodec.ParseRTU(body)
	if err != nil {
		return nil, driver.E(driver.ErrProtocol, "read", "", err.Error(), true, err)
	}
	if unit != t.unit {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("rtu unit mismatch: got %d want %d", unit, t.unit), true, nil)
	}
	return respPDU, nil
}

// readResponse 读满一条 RTU 响应。
//
// 策略分两段：先读 2 字节（地址 + 功能码），据此判断是异常帧（5 字节
// 定长）还是正常帧（长度由请求推导），再一次性读满剩余部分。只有
// 「长度推不出来」的极少数情况才退回逐字节探测。
//
// 这样做的直接收益：正常路径只需 2~3 次读系统调用；字节计数被写坏的
// 从站会在读完固定长度后立刻被 CRC 拒绝，而不是把主站挂到超时。
func (t *rtuFraming) readResponse(ctx context.Context, reqPDU []byte) ([]byte, error) {
	head, err := t.stream.ReadFull(2)
	if err != nil {
		return nil, classifyNetErr("read", err)
	}
	unit, fc := head[0], head[1]

	// 异常响应：地址 + (FC|0x80) + 异常码 + CRC(2) = 5 字节。
	if fc&0x80 != 0 {
		rest, err := t.stream.ReadFull(3)
		if err != nil {
			return nil, driver.E(driver.ErrTimeout, "read", "",
				"rtu truncated exception frame", true, err)
		}
		return append(head, rest...), nil
	}

	want, perr := mbcodec.RTUExpectedResponseLen(reqPDU)
	if perr != nil {
		return nil, driver.E(driver.ErrProtocol, "read", "", perr.Error(), false, perr)
	}
	if len(reqPDU) > 1 && reqPDU[0] != fc {
		// 功能码不回显说明总线上有另一台从站/上一轮残留；继续读只会
		// 把错位数据进行更深，直接判定协议错误。
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("rtu function echo %#x, want %#x (unit %d)", fc, reqPDU[0], unit), true, nil)
	}
	if want < 2 || want > 256 {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("rtu implausible response length %d", want), false, nil)
	}
	rest, err := t.stream.ReadFull(want - 2)
	if err != nil {
		return nil, driver.E(driver.ErrTimeout, "read", "",
			fmt.Sprintf("rtu truncated frame (%d of %d bytes)", 2, want), true, err)
	}
	return append(head, rest...), nil
}

func (t *rtuFraming) Close() error { return t.stream.Close() }

// effectiveFrameGap 按 Modbus 规范算帧间静默间隔：3.5 个字符时间；
// 波特率高于 19200 时规范建议固定 1750μs（否则间隔短到无法可靠检测）。
//
// 这个间隔在 TCP 上毫无意义，在 RS-485 上却是「偶发丢帧」的头号原因：
// 少等这 3~4ms，从站还没从接收切到发送，主站就开始读了。
func effectiveFrameGap(t model.Transport) time.Duration {
	baud := t.Baud
	if baud <= 0 {
		baud = 9600
	}
	if baud > 19200 {
		return 1750 * time.Microsecond
	}
	bits := 1 + t.DataBits + t.StopBits // 起始位 + 数据位 + 停止位
	switch t.Parity {
	case "even", "odd":
		bits++
	}
	if bits <= 0 {
		bits = 10
	}
	return time.Duration(float64(bits)*3.5/float64(baud)*float64(time.Second)) + 200*time.Microsecond
}
