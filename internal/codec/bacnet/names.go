// 本文件把 BACnet 的对象类型名与属性名映射到标准编号，供驱动、内置从站
// 与 CLI 共用。
//
// 为什么单独一层：现场文档里同一个属性会有三种写法——标准名（present-value）、
// 下划线名（present_value，点表工具喜欢的写法）、缩写（pv，工程师口头语）。
// 让每个调用方各自写一遍 switch，迟早会出现「驱动认识 present-value、
// 从站只认 present_value」这种自相矛盾的行为。
package bacnet

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// normalizeName 统一大小写与分隔符：`Present-Value`、`present_value`、
// ` present value ` 都归一成 `presentvalue`。
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(s)
}

var objectTypeNames = map[int][]string{
	ObjectAnalogInput:       {"analog-input", "analoginput", "ai"},
	ObjectAnalogOutput:      {"analog-output", "analogoutput", "ao"},
	ObjectAnalogValue:       {"analog-value", "analogvalue", "av"},
	ObjectBinaryInput:       {"binary-input", "binaryinput", "bi"},
	ObjectBinaryOutput:      {"binary-output", "binaryoutput", "bo"},
	ObjectBinaryValue:       {"binary-value", "binaryvalue", "bv"},
	ObjectDevice:            {"device", "dev"},
	ObjectMultiStateInput:   {"multi-state-input", "multistateinput", "msi"},
	ObjectMultiStateValue:   {"multi-state-value", "multistatevalue", "msv"},
	ObjectMultiStateOutput:  {"multi-state-output", "multistateoutput", "mso"},
	ObjectCharacterString:   {"character-string", "characterstring", "csv"},
	ObjectNotificationClass: {"notification-class", "notificationclass", "nc"},
	ObjectFile:              {"file"},
	ObjectLoop:              {"loop"},
	ObjectProgram:           {"program"},
	ObjectSchedule:          {"schedule"},
	ObjectTrend:             {"trend-log", "trendlog"},
}

var objectTypeByAlias = func() map[string]int {
	m := make(map[string]int, 32)
	for typ, aliases := range objectTypeNames {
		for _, a := range aliases {
			m[normalizeName(a)] = typ
		}
	}
	return m
}()

// ObjectTypeFromName 把对象类型名或编号解析为标准类型号。
//
// 编号必须写成纯十进制（`0`、`63`）；不先做 normalize 再解析，因为
// 归一化会删掉 `-`，`-1` 会被读成 `1`——一个安静的、指向别的对象的
// 错误比一个明确的报错危险得多。
func ObjectTypeFromName(s string) (int, bool) {
	if n, ok := parseNumberLiteral(strings.TrimSpace(s), 1023); ok {
		return n, true
	}
	typ, ok := objectTypeByAlias[normalizeName(s)]
	return typ, ok
}

// ObjectTypeName 返回对象类型的首选短横线写法；未知类型回退为编号。
func ObjectTypeName(typ int) string {
	if names, ok := objectTypeNames[typ]; ok && len(names) > 0 {
		return names[0]
	}
	return strconv.Itoa(typ)
}

// ObjectTypeNames 返回全部可识别的对象类型名，用于错误提示。
func ObjectTypeNames() []string {
	out := make([]string, 0, len(objectTypeNames))
	for _, names := range objectTypeNames {
		if len(names) > 0 {
			out = append(out, names[0])
		}
	}
	sort.Strings(out)
	return out
}

var propertyNames = map[int][]string{
	PropActiveText:        {"active-text", "activetext"},
	PropCovIncrement:      {"cov-increment", "covincrement"},
	PropDeadband:          {"deadband"},
	PropDescription:       {"description", "desc"},
	PropDeviceType:        {"device-type", "devicetype"},
	PropEventState:        {"event-state", "eventstate"},
	PropFirmwareRevision:  {"firmware-revision", "firmwarerevision"},
	PropHighLimit:         {"high-limit", "highlimit"},
	PropInactiveText:      {"inactive-text", "inactivetext"},
	PropLowLimit:          {"low-limit", "lowlimit"},
	PropModelName:         {"model-name", "modelname"},
	PropNumberOfStates:    {"number-of-states", "numberofstates"},
	PropObjectIdentifier:  {"object-identifier", "objectidentifier", "oid"},
	PropObjectName:        {"object-name", "objectname", "name"},
	PropObjectType:        {"object-type", "objecttype"},
	PropOutOfService:      {"out-of-service", "outofservice", "oos"},
	PropOutputUnits:       {"output-units", "outputunits"},
	PropPolarity:          {"polarity"},
	PropPresentValue:      {"present-value", "presentvalue", "pv"},
	PropPriorityArray:     {"priority-array", "priorityarray"},
	PropReliability:       {"reliability"},
	PropRelinquishDefault: {"relinquish-default", "relinquishdefault"},
	PropResolution:        {"resolution"},
	PropStateText:         {"state-text", "statetext"},
	PropStatusFlags:       {"status-flags", "statusflags"},
	PropSystemStatus:      {"system-status", "systemstatus"},
	PropUnits:             {"units"},
	PropVendorIdentifier:  {"vendor-identifier", "vendoridentifier"},
	PropVendorName:        {"vendor-name", "vendorname"},
}

var propertyByAlias = func() map[string]int {
	m := make(map[string]int, len(propertyNames)+64)
	for prop, aliases := range propertyNames {
		for _, a := range aliases {
			m[normalizeName(a)] = prop
		}
	}
	return m
}()

// PropertyFromName 把属性名或编号解析为标准属性号。
func PropertyFromName(s string) (int, bool) {
	if n, ok := parseNumberLiteral(strings.TrimSpace(s), 4194303); ok {
		return n, true
	}
	prop, ok := propertyByAlias[normalizeName(s)]
	return prop, ok
}

// parseNumberLiteral 只接受纯十进制数字串（不接受符号、空格、0x 前缀）。
func parseNumberLiteral(s string, max int) (int, bool) {
	if s == "" || len(s) > 7 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > max {
		return 0, false
	}
	return n, true
}

// PropertyName 返回属性的首选短横线写法；未知属性回退为编号。
func PropertyName(prop int) string {
	if names, ok := propertyNames[prop]; ok && len(names) > 0 {
		return names[0]
	}
	return strconv.Itoa(prop)
}

// PropertyNames 返回全部可识别的属性名，用于错误提示。
func PropertyNames() []string {
	out := make([]string, 0, len(propertyNames))
	for _, names := range propertyNames {
		if len(names) > 0 {
			out = append(out, names[0])
		}
	}
	sort.Strings(out)
	return out
}

// ObjectKind 是「对象类型 → 值语义」的分组。读写时用它决定用哪种
// 应用值编码：模拟量是 REAL，二进制量是 ENUMERATED，多状态量是
// UNSIGNED，字符串对象是 CHARACTER_STRING。
type ObjectKind int

const (
	KindUnknown ObjectKind = iota
	KindAnalog
	KindBinary
	KindMultiState
	KindString
)

// KindOfObject 把对象类型归入值语义分组。
func KindOfObject(typ int) ObjectKind {
	switch typ {
	case ObjectAnalogInput, ObjectAnalogOutput, ObjectAnalogValue, ObjectLoop:
		return KindAnalog
	case ObjectBinaryInput, ObjectBinaryOutput, ObjectBinaryValue:
		return KindBinary
	case ObjectMultiStateInput, ObjectMultiStateValue, ObjectMultiStateOutput:
		return KindMultiState
	case ObjectCharacterString:
		return KindString
	}
	return KindUnknown
}

// FormatAddress 生成 `analog-input:1.present-value` 形式的可读地址，
// 只用于日志与错误消息；平台唯一性仍以 model.Address.Canonical 为准。
func FormatAddress(objType, instance, property, index int) string {
	base := fmt.Sprintf("%s:%d.%s", ObjectTypeName(objType), instance, PropertyName(property))
	if index > 0 {
		base += fmt.Sprintf("[%d]", index)
	}
	return base
}
