// Package transport 提供字节流抽象与 TCP / 串口实现。
//
// 协议分帧（MBAP、TPKT、RTU 长度等）属于协议层，不属于本包；本包只保证
// 「写满」「读满或超时」「关闭幂等」三件事。
package transport

import (
	"errors"
	"fmt"
)

// ErrSerialUnsupported 表示当前平台没有串口实现。
//
// 这是一个**显式失败**，不是静默降级：串口涉及 termios / DCB 这类平台
// 专属结构，宁可让配置在启动时报错，也不要让现场以为设备连上了。
var ErrSerialUnsupported = errors.New("transport: serial not supported on this platform")

// SerialConfig 是串口参数。零值字段由 OpenSerial 填默认值。
type SerialConfig struct {
	Port     string // /dev/ttyS0、/dev/ttyUSB0、COM3
	Baud     int    // 默认 9600
	DataBits int    // 默认 8
	Parity   string // none | even | odd，默认 none
	StopBits int    // 1 | 2，默认 1
}

// Normalized 补齐默认值。
//
// 只把**零值**当作「未设置」：负数不补默认值，否则配置里写成 -1 会被
// 悄悄改成 1，现场看到的是一台永远读不到数据的设备，而不是一条错误。
func (c SerialConfig) Normalized() SerialConfig {
	if c.Baud == 0 {
		c.Baud = 9600
	}
	if c.DataBits == 0 {
		c.DataBits = 8
	}
	if c.StopBits == 0 {
		c.StopBits = 1
	}
	if c.Parity == "" {
		c.Parity = "none"
	}
	return c
}

// Validate 校验参数组合。串口参数错了不会报错，只会「读不到东西」，
// 因此必须在建链之前拦住。
func (c SerialConfig) Validate() error {
	c = c.Normalized()
	if c.Port == "" {
		return errors.New("serial: port required")
	}
	if c.DataBits < 5 || c.DataBits > 8 {
		return fmt.Errorf("serial: dataBits %d out of range 5..8", c.DataBits)
	}
	switch c.Parity {
	case "none", "even", "odd":
	default:
		return fmt.Errorf("serial: parity %q must be none|even|odd", c.Parity)
	}
	if c.StopBits != 1 && c.StopBits != 2 {
		return fmt.Errorf("serial: stopBits %d must be 1 or 2", c.StopBits)
	}
	switch c.Baud {
	case 300, 600, 1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600:
	default:
		return fmt.Errorf("serial: baud %d unsupported", c.Baud)
	}
	return nil
}

// String 便于日志与诊断包展示。
func (c SerialConfig) String() string {
	c = c.Normalized()
	return fmt.Sprintf("%s %d/%d/%s/%d", c.Port, c.Baud, c.DataBits, c.Parity, c.StopBits)
}

// SerialFromTransport 把通道配置里的 serial 参数转成 SerialConfig。
//
// 放在 transport 包而不是驱动里，是为了让所有基于串口的协议（Modbus RTU、
// DL/T 645、IEC 101 等）共用同一套默认值与校验，而不是各写一遍。
func SerialFromTransport(port string, baud, dataBits int, parity string, stopBits int) SerialConfig {
	return SerialConfig{
		Port:     port,
		Baud:     baud,
		DataBits: dataBits,
		Parity:   parity,
		StopBits: stopBits,
	}.Normalized()
}

// Flusher 清空接收缓冲。
//
// 半双工链路（RS-485）上必须用它：上一轮请求的迟到响应如果留在缓冲区里，
// 会被本轮当成响应头解析，产生「CRC 正确但内容错位」的诡异故障。
type Flusher interface {
	Drain() error
}
