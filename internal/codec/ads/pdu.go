package ads

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// 符号访问用的 IndexGroup。
//
// ADS 的“读一个 PLC 变量”是两步：先把变量名换成句柄（ReadWrite +
// IGSymHandleByName），再用句柄读写（Read/Write + IGSymValueByHandle）。
// 句柄属于会话，PLC 重启后失效；驱动必须缓存并在失效时重新解析。
const (
	// IGSymHandleByName 由变量名取句柄。写入内容是“变量名 + NUL”，
	// 读出内容是 4 字节句柄。
	IGSymHandleByName uint32 = 0xF003
	// IGSymValueByHandle 用句柄读写变量值；index offset 是句柄本身。
	IGSymValueByHandle uint32 = 0xF005
	// IGSymReleaseHandle 释放句柄（Write，载荷 4 字节句柄）。
	IGSymReleaseHandle uint32 = 0xF006
)

// State 是 ADS 状态机的状态值（ReadState/WriteControl 使用）。
type State uint16

const (
	StateInvalid      State = 0
	StateIdle         State = 1
	StateReset        State = 2
	StateInit         State = 3
	StateStart        State = 4
	StateRun          State = 5
	StateStop         State = 6
	StateSaveConfig   State = 7
	StateLoadConfig   State = 8
	StatePowerFailure State = 9
	StatePowerGood    State = 10
	StateError        State = 11
	StateShutdown     State = 12
	StateSuspend      State = 13
	StateResume       State = 14
	StateConfig       State = 15
	StateReconfig     State = 16
	StateStopping     State = 17
	StateIncompatible State = 18
	StateException    State = 19
)

func (s State) String() string {
	switch s {
	case StateInvalid:
		return "Invalid"
	case StateIdle:
		return "Idle"
	case StateReset:
		return "Reset"
	case StateInit:
		return "Init"
	case StateStart:
		return "Start"
	case StateRun:
		return "Run"
	case StateStop:
		return "Stop"
	case StateSaveConfig:
		return "SaveConfig"
	case StateLoadConfig:
		return "LoadConfig"
	case StatePowerFailure:
		return "PowerFailure"
	case StatePowerGood:
		return "PowerGood"
	case StateError:
		return "Error"
	case StateShutdown:
		return "Shutdown"
	case StateSuspend:
		return "Suspend"
	case StateResume:
		return "Resume"
	case StateConfig:
		return "Config"
	case StateReconfig:
		return "Reconfig"
	case StateStopping:
		return "Stopping"
	case StateIncompatible:
		return "Incompatible"
	case StateException:
		return "Exception"
	}
	return fmt.Sprintf("State(%d)", uint16(s))
}

// DeviceInfo 是 ReadDeviceInfo 的结果。
type DeviceInfo struct {
	Major   uint8
	Minor   uint8
	Version uint16
	Name    string
}

func (d DeviceInfo) VersionString() string {
	return fmt.Sprintf("%d.%d.%d", d.Major, d.Minor, d.Version)
}

// DeviceState 是 ReadState 的结果。
type DeviceState struct {
	ADS    State
	Device uint16
}

// Peer 是一次 AMS 会话的两端。它只是编解码输入，不持有连接状态。
type Peer struct {
	Source Endpoint
	Target Endpoint
}

// frame 组装一条请求帧。所有请求都带 FlagADS；应答位留给对端。
func (p Peer) frame(cmd Command, invoke uint32, payload []byte) Frame {
	return Frame{
		Target:  p.Target,
		Source:  p.Source,
		Command: cmd,
		Flags:   FlagADS,
		Invoke:  invoke,
		Data:    payload,
	}
}

// ReadDeviceInfo 构造“读设备信息”请求。
func (p Peer) ReadDeviceInfo(invoke uint32) Frame {
	return p.frame(CmdReadDeviceInfo, invoke, nil)
}

// Read 构造“按 IndexGroup/IndexOffset 读”请求。
func (p Peer) Read(invoke, group, offset, length uint32) Frame {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[0:4], group)
	binary.LittleEndian.PutUint32(b[4:8], offset)
	binary.LittleEndian.PutUint32(b[8:12], length)
	return p.frame(CmdRead, invoke, b)
}

// Write 构造写请求（载荷就是要写的字节）。
func (p Peer) Write(invoke, group, offset uint32, data []byte) Frame {
	b := make([]byte, 12+len(data))
	binary.LittleEndian.PutUint32(b[0:4], group)
	binary.LittleEndian.PutUint32(b[4:8], offset)
	binary.LittleEndian.PutUint32(b[8:12], uint32(len(data)))
	copy(b[12:], data)
	return p.frame(CmdWrite, invoke, b)
}

// ReadWrite 构造“带写入的读”（一次往返里先写后读）。
//
// 按变量名取句柄用的就是它：写入变量名，读回 4 字节句柄。
func (p Peer) ReadWrite(invoke, group, offset, readLen uint32, data []byte) Frame {
	b := make([]byte, 16+len(data))
	binary.LittleEndian.PutUint32(b[0:4], group)
	binary.LittleEndian.PutUint32(b[4:8], offset)
	binary.LittleEndian.PutUint32(b[8:12], readLen)
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(data)))
	copy(b[16:], data)
	return p.frame(CmdReadWrite, invoke, b)
}

// ReadState 构造“读状态”请求。
func (p Peer) ReadState(invoke uint32) Frame {
	return p.frame(CmdReadState, invoke, nil)
}

// WriteControl 构造“改状态”请求（启动/停止 PLC 运行时）。
func (p Peer) WriteControl(invoke uint32, state State, device uint16, data []byte) Frame {
	b := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint16(b[0:2], uint16(state))
	binary.LittleEndian.PutUint16(b[2:4], device)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(data)))
	copy(b[8:], data)
	return p.frame(CmdWriteControl, invoke, b)
}

// HandleByName 构造“按变量名取句柄”请求。
//
// 变量名以 NUL 结尾（PLC 侧的符号表是 C 字符串）。名字里的尾巴不能
// 省略：少了 NUL，设备会把内存里的下一个字节当成名字的一部分。
func (p Peer) HandleByName(invoke uint32, name string) Frame {
	b := make([]byte, len(name)+1)
	copy(b, name)
	return p.ReadWrite(invoke, IGSymHandleByName, 0, 4, b)
}

// ReadByHandle 构造“按句柄读值”请求。
func (p Peer) ReadByHandle(invoke, handle, length uint32) Frame {
	return p.Read(invoke, IGSymValueByHandle, handle, length)
}

// WriteByHandle 构造“按句柄写值”请求。
func (p Peer) WriteByHandle(invoke, handle uint32, data []byte) Frame {
	return p.Write(invoke, IGSymValueByHandle, handle, data)
}

// ReleaseHandle 构造“释放句柄”请求。
func (p Peer) ReleaseHandle(invoke, handle uint32) Frame {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, handle)
	return p.Write(invoke, IGSymReleaseHandle, 0, b)
}

// ParseRead 解析 Read 应答：result(4) + length(4) + data。
//
// result 非 0 时设备可能只回 4 字节，因此“有错”路径不要求长度字段。
func ParseRead(f Frame) ([]byte, Code, error) {
	if len(f.Data) < 4 {
		return nil, 0, fmt.Errorf("ads: read response %d bytes, need at least 4", len(f.Data))
	}
	code := Code(binary.LittleEndian.Uint32(f.Data[0:4]))
	if code != CodeOK {
		return nil, code, nil
	}
	if len(f.Data) < 8 {
		return nil, code, fmt.Errorf("ads: read response misses length field")
	}
	n := int(binary.LittleEndian.Uint32(f.Data[4:8]))
	body := f.Data[8:]
	if n < 0 || n > len(body) {
		return nil, code, fmt.Errorf("ads: read response length %d, payload has %d", n, len(body))
	}
	return body[:n], code, nil
}

// ParseReadWrite 解析 ReadWrite 应答，布局与 Read 相同。
func ParseReadWrite(f Frame) ([]byte, Code, error) { return ParseRead(f) }

// ParseWrite 解析 Write 应答：只有 result(4)。
func ParseWrite(f Frame) (Code, error) {
	if len(f.Data) < 4 {
		return 0, fmt.Errorf("ads: write response %d bytes, need 4", len(f.Data))
	}
	return Code(binary.LittleEndian.Uint32(f.Data[0:4])), nil
}

// ParseReadDeviceInfo 解析设备信息：result(4) + major(1) + minor(1) +
// version(2) + name(16, NUL 填充)。
func ParseReadDeviceInfo(f Frame) (DeviceInfo, Code, error) {
	var d DeviceInfo
	if len(f.Data) < 4 {
		return d, 0, fmt.Errorf("ads: device-info response %d bytes, need at least 4", len(f.Data))
	}
	code := Code(binary.LittleEndian.Uint32(f.Data[0:4]))
	if code != CodeOK {
		return d, code, nil
	}
	if len(f.Data) < 24 {
		return d, code, fmt.Errorf("ads: device-info response %d bytes, need 24", len(f.Data))
	}
	d.Major = f.Data[4]
	d.Minor = f.Data[5]
	d.Version = binary.LittleEndian.Uint16(f.Data[6:8])
	d.Name = cString(f.Data[8:24])
	return d, code, nil
}

// ParseReadState 解析状态：result(4) + adsState(2) + deviceState(2)。
func ParseReadState(f Frame) (DeviceState, Code, error) {
	var s DeviceState
	if len(f.Data) < 4 {
		return s, 0, fmt.Errorf("ads: read-state response %d bytes, need at least 4", len(f.Data))
	}
	code := Code(binary.LittleEndian.Uint32(f.Data[0:4]))
	if code != CodeOK {
		return s, code, nil
	}
	if len(f.Data) < 8 {
		return s, code, fmt.Errorf("ads: read-state response misses state fields")
	}
	s.ADS = State(binary.LittleEndian.Uint16(f.Data[4:6]))
	s.Device = binary.LittleEndian.Uint16(f.Data[6:8])
	return s, code, nil
}

// ParseWriteControl 解析“改状态”应答：只有 result(4)。
func ParseWriteControl(f Frame) (Code, error) { return ParseWrite(f) }

// cString 取 NUL 结尾字符串，并去掉尾部残留的 NUL/空格。
func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimRight(string(b), " ")
}
