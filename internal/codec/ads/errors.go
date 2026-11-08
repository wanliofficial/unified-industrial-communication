package ads

import (
	"fmt"

	"uic/internal/driver"
)

// Code 是 AMS 错误码或 ADS 结果码。两者数值范围不重叠，共用一套分类。
//
// 分类的目的不是“把错误码翻译成人话”，而是决定上层要不要重试、要不要
// 熔断、质量码是什么。ADS 的码比 Modbus 多一个数量级，逐个映射时必须
// 区分三类完全不同的处置：
//
//   - 链路/负载类（mailbox full、busy、no memory）：值得重试，计入熔断；
//   - 地址类（invalid group/offset/size、symbol not found）：点表或变量名
//     写错，不重试、不熔断；
//   - 权限类（invalid access）：对象存在但不可写，不重试、不熔断。
//
// 未列出的码一律归 protocol，并保留原值——宁可报“未知协议错误 0x...”，
// 也不要猜一个类别。
type Code uint32

const (
	CodeOK Code = 0

	// AMS 层（0x0001..0x00FF）：报文没有被目标处理。
	CodeInternalError         Code = 0x0001
	CodeNoRtime               Code = 0x0002
	CodeAllocLockedMem        Code = 0x0003
	CodeMailboxFull           Code = 0x0004
	CodeWrongReceiveHMSG      Code = 0x0005
	CodeTargetPortNotFound    Code = 0x0006
	CodeTargetMachineNotFound Code = 0x0007
	CodeUnknownCommandID      Code = 0x0008
	CodeBadTaskID             Code = 0x0009
	CodeNoIO                  Code = 0x000A
	CodeUnknownAMSCmd         Code = 0x000B
	CodeWin32Error            Code = 0x000C
	CodePortNotConnected      Code = 0x000D
	CodeInvalidAMSLength      Code = 0x000E
	CodeInvalidAMSNetID       Code = 0x000F
	CodeLowInstallLevel       Code = 0x0010
	CodeNoDebug               Code = 0x0011
	CodePortAlreadyConnected  Code = 0x0012
	CodeAMSSyncWin32Error     Code = 0x0013
	CodeAMSSyncTimeout        Code = 0x0014
	CodeAMSNoMemory           Code = 0x0015
	CodeAMSTimeout            Code = 0x0016
	CodeAMSNoInterface        Code = 0x0017
	CodeAMSInvalidInterface   Code = 0x0018

	// ADS 设备层（0x0700..0x07FF）：命令到了设备，执行结果由设备返回。
	CodeDeviceError           Code = 0x0700
	CodeServiceNotSupported   Code = 0x0701
	CodeInvalidGroup          Code = 0x0702
	CodeInvalidOffset         Code = 0x0703
	CodeInvalidAccess         Code = 0x0704
	CodeInvalidSize           Code = 0x0705
	CodeInvalidData           Code = 0x0706
	CodeNotReady              Code = 0x0707
	CodeBusy                  Code = 0x0708
	CodeInvalidContext        Code = 0x0709
	CodeNoMemory              Code = 0x070A
	CodeInvalidParameter      Code = 0x070B
	CodeNotFound              Code = 0x070C
	CodeSyntaxError           Code = 0x070D
	CodeMismatch              Code = 0x070E
	CodeExists                Code = 0x070F
	CodeSymbolNotFound        Code = 0x0710
	CodeSymbolVersionInvalid  Code = 0x0711
	CodeInvalidState          Code = 0x0712
	CodeTransModeNotSupported Code = 0x0713
	CodeNotifyHandleInvalid   Code = 0x0714
	CodeClientUnknown         Code = 0x0715
	CodeNoMoreHandles         Code = 0x0716
	CodeInvalidWatchSize      Code = 0x0717
	CodeNotInitialized        Code = 0x0718
	CodeDeviceTimeout         Code = 0x0719
	CodeNoInterface           Code = 0x071A
	CodeInvalidInterface      Code = 0x071B
	CodeInvalidClient         Code = 0x071C
	CodeInvalidDirection      Code = 0x071D
	CodeInvalidVersion        Code = 0x071E
	CodeInvalidType           Code = 0x071F
	CodeInvalidLength         Code = 0x0720
	CodeInvalidParameter2     Code = 0x0721
	CodeInvalidParameter3     Code = 0x0722
	CodeInvalidParameter4     Code = 0x0723
	CodeInvalidParameter5     Code = 0x0724
	CodeInvalidParameter6     Code = 0x0725
	CodeInvalidParameter7     Code = 0x0726
	CodeInvalidParameter8     Code = 0x0727
	CodeInvalidParameter9     Code = 0x0728
	CodeInvalidParameter10    Code = 0x0729
	CodeInvalidParameter11    Code = 0x072A
	CodeInvalidParameter12    Code = 0x072B
	CodeInvalidParameter13    Code = 0x072C
	CodeInvalidParameter14    Code = 0x072D
	CodeInvalidParameter15    Code = 0x072E
	CodeInvalidParameter16    Code = 0x072F

	// CodeGlobalFail 是设备侧“无法给出更具体原因”的兜底值。
	CodeGlobalFail Code = 0xFFFFFFFF
)

var codeNames = map[Code]string{
	CodeOK:                    "no error",
	CodeInternalError:         "internal error",
	CodeNoRtime:               "no real-time context",
	CodeAllocLockedMem:        "locked memory exhausted",
	CodeMailboxFull:           "mailbox full",
	CodeWrongReceiveHMSG:      "wrong receive message",
	CodeTargetPortNotFound:    "target port not found",
	CodeTargetMachineNotFound: "target machine not found",
	CodeUnknownCommandID:      "unknown command id",
	CodeBadTaskID:             "bad task id",
	CodeNoIO:                  "no IO",
	CodeUnknownAMSCmd:         "unknown AMS command",
	CodeWin32Error:            "win32 error",
	CodePortNotConnected:      "port not connected",
	CodeInvalidAMSLength:      "invalid AMS length",
	CodeInvalidAMSNetID:       "invalid AMS netid",
	CodeLowInstallLevel:       "installation level too low",
	CodeNoDebug:               "no debug available",
	CodePortAlreadyConnected:  "port already connected",
	CodeAMSSyncWin32Error:     "AMS sync win32 error",
	CodeAMSSyncTimeout:        "AMS sync timeout",
	CodeAMSNoMemory:           "AMS no memory",
	CodeAMSTimeout:            "AMS timeout",
	CodeAMSNoInterface:        "AMS no interface",
	CodeAMSInvalidInterface:   "AMS invalid interface",
	CodeDeviceError:           "device error",
	CodeServiceNotSupported:   "service not supported",
	CodeInvalidGroup:          "invalid index group",
	CodeInvalidOffset:         "invalid index offset",
	CodeInvalidAccess:         "invalid access",
	CodeInvalidSize:           "invalid size",
	CodeInvalidData:           "invalid data",
	CodeNotReady:              "device not ready",
	CodeBusy:                  "device busy",
	CodeInvalidContext:        "invalid context",
	CodeNoMemory:              "out of memory",
	CodeInvalidParameter:      "invalid parameter",
	CodeNotFound:              "not found",
	CodeSyntaxError:           "syntax error",
	CodeMismatch:              "type mismatch",
	CodeExists:                "object already exists",
	CodeSymbolNotFound:        "symbol not found",
	CodeSymbolVersionInvalid:  "symbol version invalid",
	CodeInvalidState:          "invalid state",
	CodeTransModeNotSupported: "transmission mode not supported",
	CodeNotifyHandleInvalid:   "notification handle invalid",
	CodeClientUnknown:         "client unknown",
	CodeNoMoreHandles:         "no more handles",
	CodeInvalidWatchSize:      "invalid watch size",
	CodeNotInitialized:        "not initialized",
	CodeDeviceTimeout:         "device timeout",
	CodeNoInterface:           "no interface",
	CodeInvalidInterface:      "invalid interface",
	CodeInvalidClient:         "invalid client",
	CodeInvalidDirection:      "invalid direction",
	CodeInvalidVersion:        "invalid version",
	CodeInvalidType:           "invalid type",
	CodeInvalidLength:         "invalid length",
	CodeInvalidParameter2:     "invalid parameter (2)",
	CodeInvalidParameter3:     "invalid parameter (3)",
	CodeInvalidParameter4:     "invalid parameter (4)",
	CodeInvalidParameter5:     "invalid parameter (5)",
	CodeInvalidParameter6:     "invalid parameter (6)",
	CodeInvalidParameter7:     "invalid parameter (7)",
	CodeInvalidParameter8:     "invalid parameter (8)",
	CodeInvalidParameter9:     "invalid parameter (9)",
	CodeInvalidParameter10:    "invalid parameter (10)",
	CodeInvalidParameter11:    "invalid parameter (11)",
	CodeInvalidParameter12:    "invalid parameter (12)",
	CodeInvalidParameter13:    "invalid parameter (13)",
	CodeInvalidParameter14:    "invalid parameter (14)",
	CodeInvalidParameter15:    "invalid parameter (15)",
	CodeInvalidParameter16:    "invalid parameter (16)",
	CodeGlobalFail:            "global failure",
}

func (c Code) String() string {
	if s, ok := codeNames[c]; ok {
		return fmt.Sprintf("%s (0x%08X)", s, uint32(c))
	}
	return fmt.Sprintf("0x%08X", uint32(c))
}

// Kind 返回错误分类。
func (c Code) Kind() driver.ErrorKind {
	switch c {
	case CodeOK:
		return ""
	case CodeTargetPortNotFound, CodeInvalidAMSNetID, CodeAMSInvalidInterface,
		CodeInvalidGroup, CodeInvalidOffset, CodeInvalidSize, CodeInvalidData,
		CodeInvalidParameter, CodeNotFound, CodeSyntaxError, CodeSymbolNotFound,
		CodeSymbolVersionInvalid, CodeNotifyHandleInvalid, CodeInvalidWatchSize,
		CodeInvalidInterface, CodeInvalidDirection, CodeInvalidVersion,
		CodeInvalidType, CodeInvalidLength,
		CodeInvalidParameter2, CodeInvalidParameter3, CodeInvalidParameter4,
		CodeInvalidParameter5, CodeInvalidParameter6, CodeInvalidParameter7,
		CodeInvalidParameter8, CodeInvalidParameter9, CodeInvalidParameter10,
		CodeInvalidParameter11, CodeInvalidParameter12, CodeInvalidParameter13,
		CodeInvalidParameter14, CodeInvalidParameter15, CodeInvalidParameter16:
		return driver.ErrAddress
	case CodeInvalidAccess:
		return driver.ErrPermission
	case CodeServiceNotSupported, CodeTransModeNotSupported, CodeLowInstallLevel:
		return driver.ErrUnsupported
	case CodeTargetMachineNotFound, CodeNoIO, CodePortNotConnected,
		CodeAMSNoInterface, CodeNoInterface:
		return driver.ErrDisconnected
	case CodeAMSSyncTimeout, CodeAMSTimeout, CodeNoRtime, CodeDeviceTimeout:
		return driver.ErrTimeout
	}
	return driver.ErrProtocol
}

// Temporary 表示值得原样重试一次。
//
// 只把“资源暂时不足/忙/超时”标成可重试：地址与权限错误重试一万次也
// 不会变好，把它们标成可重试只会让一轮采集多花几十次超时。
func (c Code) Temporary() bool {
	switch c {
	case CodeInternalError, CodeNoRtime, CodeAllocLockedMem, CodeMailboxFull,
		CodeTargetMachineNotFound, CodePortNotConnected, CodeAMSSyncTimeout,
		CodeAMSNoMemory, CodeAMSTimeout, CodeAMSNoInterface,
		CodeDeviceError, CodeNotReady, CodeBusy, CodeNoMemory, CodeInvalidState,
		CodeNoMoreHandles, CodeNotInitialized, CodeDeviceTimeout:
		return true
	}
	return false
}
