//go:build !linux

package transport

import "fmt"

// OpenSerial 在非 Linux 平台返回显式错误。
//
// 这不是「暂时不支持」的占位，而是一条产品判断：Windows 的串口要用 DCB
// 结构走另一套 API，在没有真实硬件可验证之前，写一个测不了的实现比不写
// 更危险。需要串口的 Windows 部署，走 C# 辅助进程桥接（见 ADR 0001）。
func OpenSerial(cfg SerialConfig) (Stream, error) {
	return nil, fmt.Errorf("%w (port %s)", ErrSerialUnsupported, cfg.Port)
}
