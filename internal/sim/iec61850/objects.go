package iec61850

import ieccodec "uic/internal/codec/iec61850"

// DefaultObjects 返回内置从站的默认变量树（单一事实来源）。
//
// 键是 MMS 名（域 + "$" + 项），值与 examples/iec61850.json 的点位一一对应：
// 逻辑设备 simpleIO 下挂一个 GGIO1 通用节点加一组量测，覆盖 bool / int / float
// 三种平台类型，让驱动的读、写、归一、错误分类三条路径都有真实数据可走。
//
// 为什么用 DataFromValue 构造而不是手拼字节：DataFromValue 是平台值与 MMS 线
// 类型之间唯一的转换函数，从站与驱动共用它，就保证了「从站写进去的字节」与
// 「驱动解出来的值」用的是同一套编码——任何一侧的偏差都会在往返测试里暴露。
func DefaultObjects() map[string]ieccodec.Data {
	out := map[string]ieccodec.Data{}

	// 逻辑设备 simpleIO：LLN0 的健康指示。所有 IED 都有这个通用节点，
	// 驱动 Probe 默认读它，因此它必须存在且可读。
	//
	// 用 ST$Health 而不是 LLN0 的 Beh：Beh 挂在 MOD（修改）功能约束下，而 MOD
	// 不在 IEC 61850-7-3 的标准功能约束集合里，平台的地址校验会直接拒绝它。
	// Health 属于 ST（状态），既真实又能通过校验。
	put(out, "simpleIO$LLN0$ST$Health", ieccodec.Value{Type: "int32", Num: 1})

	// GGIO1：通用节点，演示状态（ST）与量测（MX）两类功能约束。
	put(out, "simpleIO$GGIO1$ST$Ind1$stVal", ieccodec.Value{Type: "bool", Bool: true})
	put(out, "simpleIO$GGIO1$ST$Ind2$stVal", ieccodec.Value{Type: "bool", Bool: false})
	put(out, "simpleIO$GGIO1$ST$Str$Pos$stVal", ieccodec.Value{Type: "int32", Num: 0})
	put(out, "simpleIO$GGIO1$MX$PhV$mag$f", ieccodec.Value{Type: "float32", Num: 230.5})
	put(out, "simpleIO$GGIO1$MX$A$mag$f", ieccodec.Value{Type: "float32", Num: 1.2})

	// GGIO1 的控制（CO）与定值（SP）：可写对象，写测试用。
	put(out, "simpleIO$GGIO1$CO$SPCs$stVal", ieccodec.Value{Type: "bool", Bool: false})
	put(out, "simpleIO$GGIO1$SP$SETV$setMag$f", ieccodec.Value{Type: "float32", Num: 0})

	return out
}

// put 把平台值折成 MMS Data 收入变量树；类型不识别时跳过（默认树全是合法类型）。
func put(m map[string]ieccodec.Data, name string, v ieccodec.Value) {
	if d, err := ieccodec.DataFromValue(v); err == nil {
		m[name] = d
	}
}
