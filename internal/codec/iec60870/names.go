package iec60870

import (
	"fmt"
	"strconv"
	"strings"
)

// typeNames 是 ASDU 类型标识的中文说明。名称只描述语义，不复述任何标准
// 文本；同样的理由见 docs/03-product/04-driver-sdk-spec.md。
var typeNames = map[byte]string{
	TypeSinglePoint:            "单点信息",
	TypeDoublePoint:            "双点信息",
	TypeMeasuredNormalized:     "归一化测量值",
	TypeMeasuredScaled:         "标度化测量值",
	TypeMeasuredFloat:          "短浮点测量值",
	TypeIntegratedTotals:       "累计量",
	TypeSinglePointTime:        "带时标单点信息",
	TypeDoublePointTime:        "带时标双点信息",
	TypeMeasuredNormalizedTime: "带时标归一化测量值",
	TypeMeasuredScaledTime:     "带时标标度化测量值",
	TypeMeasuredFloatTime:      "带时标短浮点测量值",
	TypeSingleCommand:          "单点命令",
	TypeDoubleCommand:          "双点命令",
	TypeSetpointNormalized:     "归一化设定值命令",
	TypeSetpointScaled:         "标度化设定值命令",
	TypeSetpointFloat:          "短浮点设定值命令",
	TypeEndOfInit:              "初始化结束",
	TypeInterrogation:          "总召唤",
	TypeCounterInterrogation:   "电能量召唤",
	TypeClockSync:              "时钟同步",
}

// aliases 是命令行与配置里允许的类型别名。现场文档里「单点」「single」
// 「M_SP_NA_1」三种写法都常见，强迫用户记其中一种只会制造口误。
var aliases = map[string]byte{
	"single":              TypeSinglePoint,
	"singlepoint":         TypeSinglePoint,
	"single-point":        TypeSinglePoint,
	"m_sp_na_1":           TypeSinglePoint,
	"bool":                TypeSinglePoint,
	"double":              TypeDoublePoint,
	"doublepoint":         TypeDoublePoint,
	"double-point":        TypeDoublePoint,
	"m_dp_na_1":           TypeDoublePoint,
	"normalized":          TypeMeasuredNormalized,
	"measured":            TypeMeasuredNormalized,
	"m_me_na_1":           TypeMeasuredNormalized,
	"scaled":              TypeMeasuredScaled,
	"m_me_nb_1":           TypeMeasuredScaled,
	"float":               TypeMeasuredFloat,
	"shortfloat":          TypeMeasuredFloat,
	"m_me_nc_1":           TypeMeasuredFloat,
	"counter":             TypeIntegratedTotals,
	"m_it_na_1":           TypeIntegratedTotals,
	"single-time":         TypeSinglePointTime,
	"m_sp_tb_1":           TypeSinglePointTime,
	"double-time":         TypeDoublePointTime,
	"m_dp_tb_1":           TypeDoublePointTime,
	"normalized-time":     TypeMeasuredNormalizedTime,
	"m_me_td_1":           TypeMeasuredNormalizedTime,
	"scaled-time":         TypeMeasuredScaledTime,
	"m_me_te_1":           TypeMeasuredScaledTime,
	"float-time":          TypeMeasuredFloatTime,
	"m_me_tf_1":           TypeMeasuredFloatTime,
	"single-command":      TypeSingleCommand,
	"c_sc_na_1":           TypeSingleCommand,
	"double-command":      TypeDoubleCommand,
	"c_dc_na_1":           TypeDoubleCommand,
	"setpoint-normalized": TypeSetpointNormalized,
	"c_se_na_1":           TypeSetpointNormalized,
	"setpoint-scaled":     TypeSetpointScaled,
	"c_se_nb_1":           TypeSetpointScaled,
	"setpoint-float":      TypeSetpointFloat,
	"c_se_nc_1":           TypeSetpointFloat,
}

// TypeName 返回类型的可读名；未知类型返回「未知类型(N)」而不是空串，
// 日志里至少能看出是哪条 ASDU 被丢弃。
func TypeName(id byte) string {
	if n, ok := typeNames[id]; ok {
		return n
	}
	return fmt.Sprintf("未知类型(%d)", id)
}

// TypeFromName 把类型别名或十进制编号解析成类型标识。
//
// 纯十进制优先于别名：现场文档只给编号时不必为了一个数字去查别名表。
// 负数不合法——`-1` 不会被住动 trim 成 `1`，那会安静地指向另一种类型。
func TypeFromName(s string) (byte, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 255 {
			return 0, false
		}
		if _, ok := typeNames[byte(n)]; !ok {
			return 0, false
		}
		return byte(n), true
	}
	key := strings.ToLower(s)
	key = strings.ReplaceAll(key, "_", "-")
	key = strings.ReplaceAll(key, " ", "")
	// 别名表里同时存在下划线与连字符写法，统一成连字符再查一次。
	if id, ok := aliases[key]; ok {
		_, known := ElementSize(id)
		return id, known
	}
	if id, ok := aliases[strings.ReplaceAll(key, "-", "_")]; ok {
		_, known := ElementSize(id)
		return id, known
	}
	return 0, false
}

// TypeNames 返回全部支持的类型（按类型标识排序），供配置界面生成下拉框。
func TypeNames() []string {
	ids := make([]int, 0, len(typeNames))
	for id := range typeNames {
		ids = append(ids, int(id))
	}
	// 插入排序：只有 20 项，避免为了一个下拉框引入 sort 依赖。
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = TypeName(byte(id))
	}
	return out
}

// CauseName 返回传送原因的中文说明。
func CauseName(cause byte) string {
	switch cause {
	case CausePeriodic:
		return "周期"
	case CauseBackground:
		return "背景扫描"
	case CauseSpontaneous:
		return "突发"
	case CauseInitialized:
		return "初始化"
	case CauseRequest:
		return "请求"
	case CauseActivation:
		return "激活"
	case CauseActivationCon:
		return "激活确认"
	case CauseDeactivation:
		return "停止激活"
	case CauseDeactivationCon:
		return "停止激活确认"
	case CauseActivationTerm:
		return "激活终止"
	case CauseInterrogated:
		return "响应站召唤"
	case CauseUnknownType:
		return "未知类型标识"
	case CauseUnknownCause:
		return "未知传送原因"
	case CauseUnknownCommonAddr:
		return "未知公共地址"
	case CauseUnknownIOA:
		return "未知信息对象地址"
	}
	if cause >= CauseGroup1 && cause <= 36 {
		return fmt.Sprintf("响应第 %d 组召唤", int(cause-CauseGroup1)+1)
	}
	return fmt.Sprintf("原因(%d)", cause)
}
