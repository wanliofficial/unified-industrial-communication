// Package ads 是 ADS（AMS/TCP）的纯函数编解码层。
//
// 三层结构：
//
//	AMS/TCP 帧头（6 字节）         reserved uint16 + length uint32（小端）
//	AMS 头（32 字节）              两端的 NetId/端口 + 命令 + 标志 + 长度 + 错误 + invoke
//	ADS 载荷                        按命令解释（读/写/设备信息/状态/通知）
//
// 与 Modbus/S7 不同，ADS 全线使用**小端**：长度、错误码、句柄、数值都是
// little-endian。把大端的字节直接喂给 ADS，设备要么长度错位，要么把一个
// float 解释成另一个数——而且不会报错。
//
// 本层不做 I/O、不读时钟、不持有状态；连接、invoke-id 递增与重试都在
// driver 里。
package ads

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// TCPHeaderLen 是 AMS/TCP 头长度：reserved(2) + length(4)。
	TCPHeaderLen = 6
	// HeaderLen 是 AMS 头长度。
	//
	//	target NetId(6) + target port(2) +
	//	source NetId(6) + source port(2) +
	//	command(2) + state flags(2) +
	//	length(4) + error code(4) + invoke id(4)
	HeaderLen = 32
	// DefaultPort 是 AMS/TCP 的标准端口。
	DefaultPort = 48898
	// MaxDatagram 是一条 AMS 报文的上限。
	//
	// 取 4 MiB 而不是 1472：ADS 允许一次读整段数组或整个符号表，报文会
	// 超过 MTU 由 IP 分片。我们主动发送的请求只有几十字节，不会分片；
	// 这个上限只用来拒绝畸形的长度字段。
	MaxDatagram = 4 << 20
)

// NetID 是 AMS NetId：6 字节，点分十进制显示（如 192.168.1.10.1.1）。
//
// 它是路由标识而不是 IP：AMS 路由器依据 NetId 把报文转给目标设备。
// 直连 AMS/TCP 端口时，目标 NetId 仍必须与设备上配置的一致。
type NetID [6]byte

// ParseNetID 解析 “a.b.c.d.e.f”。每段 0..255，必须恰好 6 段。
func ParseNetID(s string) (NetID, error) {
	var n NetID
	s = strings.TrimSpace(s)
	if s == "" {
		return n, fmt.Errorf("ads: empty netid")
	}
	parts := strings.Split(s, ".")
	if len(parts) != 6 {
		return n, fmt.Errorf("ads: netid %q must have exactly 6 dot-separated bytes", s)
	}
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 || v > 255 {
			return n, fmt.Errorf("ads: netid %q: segment %q must be 0..255", s, p)
		}
		n[i] = byte(v)
	}
	return n, nil
}

// MustParseNetID 供常量与测试使用；非法输入直接 panic。
func MustParseNetID(s string) NetID {
	n, err := ParseNetID(s)
	if err != nil {
		panic(err)
	}
	return n
}

func (n NetID) String() string {
	return fmt.Sprintf("%d.%d.%d.%d.%d.%d", n[0], n[1], n[2], n[3], n[4], n[5])
}

// IsZero 表示未配置（全 0）。
func (n NetID) IsZero() bool { return n == NetID{} }

// Endpoint 是一个 AMS 端点：NetId + 端口。
//
// 端口不是 TCP 端口，而是 AMS 端口：851 是第一个 PLC 运行时，801 是
// 第二个，其余由设备侧分配。TCP 端口固定 48898（或路由器端口）。
type Endpoint struct {
	NetID NetID
	Port  uint16
}

func (e Endpoint) String() string { return fmt.Sprintf("%s:%d", e.NetID, e.Port) }

// Command 是 AMS 命令号。
type Command uint16

const (
	CmdInvalid                  Command = 0
	CmdReadDeviceInfo           Command = 1
	CmdRead                     Command = 2
	CmdWrite                    Command = 3
	CmdReadState                Command = 4
	CmdWriteControl             Command = 5
	CmdAddDeviceNotification    Command = 6
	CmdDeleteDeviceNotification Command = 7
	CmdDeviceNotification       Command = 8
	CmdReadWrite                Command = 9
)

func (c Command) String() string {
	switch c {
	case CmdInvalid:
		return "Invalid"
	case CmdReadDeviceInfo:
		return "ReadDeviceInfo"
	case CmdRead:
		return "Read"
	case CmdWrite:
		return "Write"
	case CmdReadState:
		return "ReadState"
	case CmdWriteControl:
		return "WriteControl"
	case CmdAddDeviceNotification:
		return "AddDeviceNotification"
	case CmdDeleteDeviceNotification:
		return "DeleteDeviceNotification"
	case CmdDeviceNotification:
		return "DeviceNotification"
	case CmdReadWrite:
		return "ReadWrite"
	}
	return fmt.Sprintf("Command(%d)", uint16(c))
}

// Flags 是 AMS 状态标志。
type Flags uint16

const (
	// FlagResponse 标记这是应答（请求为 0）。
	FlagResponse Flags = 0x0001
	// FlagNoReturn 表示不需要应答（Trap 类通知使用）。
	FlagNoReturn Flags = 0x0002
	// FlagADS 表示载荷是 ADS 命令（几乎所有请求都带这一位）。
	FlagADS Flags = 0x0004
)

// Frame 是一条 AMS 报文。Data 是 ADS 载荷（不含 AMS 头）。
type Frame struct {
	Target Endpoint
	Source Endpoint

	Command Command
	Flags   Flags
	// Error 是 AMS 头里的错误码（0 表示 AMS 层没有失败）。
	// 它与 ADS 载荷里的 result 是两级：AMS 错表示报文没被目标处理，
	// ADS result 表示命令执行结果。
	Error  Code
	Invoke uint32
	Data   []byte
}

// IsResponse 表示这是应答帧。
func (f Frame) IsResponse() bool { return f.Flags&FlagResponse != 0 }
