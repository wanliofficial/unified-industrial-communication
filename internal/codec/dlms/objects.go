package dlms

import "strings"

// 本文件是「语义表」：类号、属性号、单位枚举与常见 OBIS 的中文含义。
//
// 它们**不是**协议的线路格式，但缺了它们点表就没法写：`1.0.1.8.0.255`
// 这个地址只说了「哪个对象」，没说「它属于哪个类、读哪个属性、单位是什么」。
// 现场手册通常给出一张「OBIS / 类号 / 属性 / 含义」表，本文件把这部分
// 常见条目固化下来，让点表可以只写 OBIS，由驱动补全类号。
//
// # 为什么类号必须显式给出
//
// 同一个 OBIS 在不同实现里可能落在不同类上（例如某些表把电压放 class 3
// Register，另一些放 class 4 Extended Register）。类号错的表现是
// `object-class-inconsistent(9)` 而不是"读不到"——这是一个**好**错误，
// 因为它明确告诉你是类号错而不是 OBIS 错。驱动据此把错误分类到
// ErrAddress 而不是 ErrProtocol。
//
// # 属性号的通用约定（几乎所有类一致）
//
//	1  logical_name            （6 字节 OBIS，只读）
//	2  value                   （测量值 / 主值）
//	3  scaler_unit             （structure {integer, enum}，部分类有）
//	4  status / unit
//	5  capture_time 等

// COSEM 类号（IEC 62056-62）。
const (
	ClassData                uint16 = 1
	ClassRegister            uint16 = 3
	ClassExtendedRegister    uint16 = 4
	ClassDemandRegister      uint16 = 5
	ClassRegisterActivation  uint16 = 6
	ClassProfileGeneric      uint16 = 7
	ClassClock               uint16 = 8
	ClassScriptTable         uint16 = 9
	ClassSchedule            uint16 = 10
	ClassSpecialDaysTable    uint16 = 11
	ClassAssociationSN       uint16 = 12
	ClassAssociationLN       uint16 = 15
	ClassSAPAssignment       uint16 = 17
	ClassImageTransfer       uint16 = 18
	ClassActivityCalendar    uint16 = 20
	ClassRegisterMonitor     uint16 = 21
	ClassSingleActionSch     uint16 = 22
	ClassIECLocalPortSetup   uint16 = 23
	ClassIECTwistedPairSetup uint16 = 24
	ClassMBusSlavePortSetup  uint16 = 25
	ClassUtilityTables       uint16 = 26
	ClassModemConfiguration  uint16 = 27
	ClassAutoAnswer          uint16 = 28
	ClassAutoConnect         uint16 = 29
	ClassPushSetup           uint16 = 40
	ClassTCPUDPSetup         uint16 = 41
	ClassIPTwistedPairSetup  uint16 = 42
	ClassPPPSetup            uint16 = 44
	ClassGPRSModemSetup      uint16 = 45
	ClassSMTP                uint16 = 46
	ClassGSMDiagnostic       uint16 = 47
	ClassIPv4Setup           uint16 = 48
)

// 属性号（通用）。
const (
	AttrLogicalName int8 = 1
	AttrValue       int8 = 2
	AttrScalerUnit  int8 = 3
	AttrStatus      int8 = 4
	AttrCaptureTime int8 = 5
)

// ClassName 返回类号的中文名。
//
// 现场最有用的三个是 Register(3)、ExtendedRegister(4)、ProfileGeneric(7)：
// 前两个装测量值，第三个装历史曲线。看到 class 7 就知道这一条大概率需要
// 分块读（历史记录是变长数组），不能按普通单值处理。
func ClassName(id uint16) string {
	switch id {
	case ClassData:
		return "Data(1)"
	case ClassRegister:
		return "Register(3)"
	case ClassExtendedRegister:
		return "ExtendedRegister(4)"
	case ClassDemandRegister:
		return "DemandRegister(5)"
	case ClassRegisterActivation:
		return "RegisterActivation(6)"
	case ClassProfileGeneric:
		return "ProfileGeneric(7)"
	case ClassClock:
		return "Clock(8)"
	case ClassScriptTable:
		return "ScriptTable(9)"
	case ClassSchedule:
		return "Schedule(10)"
	case ClassSpecialDaysTable:
		return "SpecialDaysTable(11)"
	case ClassAssociationSN:
		return "AssociationSN(12)"
	case ClassAssociationLN:
		return "AssociationLN(15)"
	case ClassSAPAssignment:
		return "SAPAssignment(17)"
	case ClassImageTransfer:
		return "ImageTransfer(18)"
	case ClassActivityCalendar:
		return "ActivityCalendar(20)"
	case ClassRegisterMonitor:
		return "RegisterMonitor(21)"
	case ClassSingleActionSch:
		return "SingleActionSchedule(22)"
	case ClassIECLocalPortSetup:
		return "IECLocalPortSetup(23)"
	case ClassIECTwistedPairSetup:
		return "IECTwistedPairSetup(24)"
	case ClassMBusSlavePortSetup:
		return "MBusSlavePortSetup(25)"
	case ClassUtilityTables:
		return "UtilityTables(26)"
	case ClassModemConfiguration:
		return "ModemConfiguration(27)"
	case ClassAutoAnswer:
		return "AutoAnswer(28)"
	case ClassAutoConnect:
		return "AutoConnect(29)"
	case ClassPushSetup:
		return "PushSetup(40)"
	case ClassTCPUDPSetup:
		return "TCPUDPSetup(41)"
	case ClassIPTwistedPairSetup:
		return "IPTwistedPairSetup(42)"
	case ClassPPPSetup:
		return "PPPSetup(44)"
	case ClassGPRSModemSetup:
		return "GPRSModemSetup(45)"
	case ClassSMTP:
		return "SMTP(46)"
	case ClassGSMDiagnostic:
		return "GSMDiagnostic(47)"
	case ClassIPv4Setup:
		return "IPv4Setup(48)"
	}
	return "未知类(" + itoa(int(id)) + ")"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// Unit 是 COSEM 单位枚举（IEC 62056-62 Annex A）。
//
// 只列常见项。遇到未列出的值，驱动把它原样上报为数字——**不要**猜成
// 某个单位：单位错会让换算结果差 1000 倍，而"单位 42"至少是诚实的。
func UnitName(u int) string {
	switch u {
	case 0:
		return ""
	case 1:
		return "a"
	case 2:
		return "mo"
	case 3:
		return "wk"
	case 4:
		return "d"
	case 5:
		return "h"
	case 6:
		return "min"
	case 7:
		return "s"
	case 8:
		return "°"
	case 9:
		return "°C"
	case 10:
		return "currency"
	case 11:
		return "m"
	case 12:
		return "m/s"
	case 13:
		return "m³"
	case 14:
		return "m³/h"
	case 15:
		return "m³/s"
	case 16:
		return "m³ (normal)"
	case 17:
		return "m³ (normal)/h"
	case 18:
		return "m³ (normal)/s"
	case 19:
		return "kg"
	case 20:
		return "N"
	case 21:
		return "Nm"
	case 22:
		return "Pa"
	case 23:
		return "bar"
	case 24:
		return "J"
	case 25:
		return "J/h"
	case 26:
		return "W"
	case 27:
		return "VA"
	case 28:
		return "var"
	case 29:
		return "Wh"
	case 30:
		return "VAh"
	case 31:
		return "varh"
	case 32:
		return "A"
	case 33:
		return "C"
	case 34:
		return "V"
	case 35:
		return "V/m"
	case 36:
		return "F"
	case 37:
		return "Ω"
	case 38:
		return "S"
	case 39:
		return "S/m"
	case 40:
		return "Wb"
	case 41:
		return "T"
	case 42:
		return "H"
	case 43:
		return "Hz"
	case 44:
		return "1/(Wh)"
	case 45:
		return "1/(varh)"
	case 46:
		return "1/(VAh)"
	case 47:
		return "1/(V²h)"
	case 48:
		return "1/(A²h)"
	case 49:
		return "1/(m³)"
	case 50:
		return "%"
	case 55:
		return "dBm"
	case 60:
		return "W/m²"
	case 70:
		return "dB"
	case 254:
		return "其他"
	}
	return "unit(" + itoa(u) + ")"
}

// ApplyScaler 按 COSEM 的 scaler_unit 约定把原始整数换算成工程值。
//
// scaler_unit 是 `structure { integer scaler, enum unit }`，语义是：
//
//	工程值 = 原始值 × 10^scaler
//
// **scaler 为负是除**，这是最容易写反的一处：`scaler=-2` 表示"这个值以
// 0.01 为单位存储"，所以 1234 要变成 12.34。写成乘法会得到 123400，
// 差 10000 倍，而读数"看起来是个数"，不会报错。
func ApplyScaler(raw float64, scaler int) float64 {
	if scaler == 0 {
		return raw
	}
	// 先把 10^|scaler| 算成一个整数，再**一次**乘除。
	//
	// 为什么不逐次乘 10：5001 除以 10 得 500.1，再除以 10 得
	// 50.010000000000005——两次舍入误差叠加，显示出来就是一串尾数。
	// 一次除以 100 只舍入一次，得到 50.01。scaler 范围很小
	// （-128..127），10^128 仍是有限 double，所以这一步不会溢出。
	p := 1.0
	if scaler > 0 {
		for i := 0; i < scaler; i++ {
			p *= 10
		}
		return raw * p
	}
	for i := 0; i > scaler; i-- {
		p *= 10
	}
	return raw / p
}

// ---------------------------------------------------------------------------
// 常见 OBIS
// ---------------------------------------------------------------------------

// CommonObject 描述一个常见 COSEM 对象。
type CommonObject struct {
	OBIS      string
	Class     uint16
	Attribute int8
	Name      string
	Unit      string
}

// commonObjects 是常见 OBIS 表（IEC 62056-62 / DLMS UA Blue Book）。
//
// 只收**跨厂商一致**的条目：正向/反向有功电能、费率、瞬时量、表号、
// 逻辑设备名、时钟。厂商私有扩展（例如某些表的谐波分析、相角）逐个表
// 不同，不能进这张表——把它们放进来会让点表在换表后静默读到"另一个量"。
var commonObjects = []CommonObject{
	// 电能（class 3 Register，attribute 2 = value）
	{"1.0.1.8.0.255", ClassRegister, AttrValue, "正向有功电能（总量）", "Wh"},
	{"1.0.1.8.1.255", ClassRegister, AttrValue, "正向有功电能（费率 1）", "Wh"},
	{"1.0.1.8.2.255", ClassRegister, AttrValue, "正向有功电能（费率 2）", "Wh"},
	{"1.0.1.8.3.255", ClassRegister, AttrValue, "正向有功电能（费率 3）", "Wh"},
	{"1.0.1.8.4.255", ClassRegister, AttrValue, "正向有功电能（费率 4）", "Wh"},
	{"1.0.2.8.0.255", ClassRegister, AttrValue, "反向有功电能（总量）", "Wh"},
	{"1.0.2.8.1.255", ClassRegister, AttrValue, "反向有功电能（费率 1）", "Wh"},
	{"1.0.3.8.0.255", ClassRegister, AttrValue, "正向无功电能（总量）", "varh"},
	{"1.0.4.8.0.255", ClassRegister, AttrValue, "反向无功电能（总量）", "varh"},
	// 瞬时量（class 3 Register 或 class 4 ExtendedRegister）
	{"1.0.31.7.0.255", ClassRegister, AttrValue, "A 相电流", "A"},
	{"1.0.51.7.0.255", ClassRegister, AttrValue, "B 相电流", "A"},
	{"1.0.71.7.0.255", ClassRegister, AttrValue, "C 相电流", "A"},
	{"1.0.32.7.0.255", ClassRegister, AttrValue, "A 相电压", "V"},
	{"1.0.52.7.0.255", ClassRegister, AttrValue, "B 相电压", "V"},
	{"1.0.72.7.0.255", ClassRegister, AttrValue, "C 相电压", "V"},
	{"1.0.1.7.0.255", ClassRegister, AttrValue, "总有功功率", "W"},
	{"1.0.2.7.0.255", ClassRegister, AttrValue, "总无功功率", "var"},
	{"1.0.3.7.0.255", ClassRegister, AttrValue, "总视在功率", "VA"},
	{"1.0.13.7.0.255", ClassRegister, AttrValue, "功率因数", ""},
	{"1.0.14.7.0.255", ClassRegister, AttrValue, "电网频率", "Hz"},
	// 标识（class 1 Data / class 3 Register）
	{"0.0.42.0.0.255", ClassData, AttrValue, "逻辑设备名", ""},
	{"0.0.96.1.0.255", ClassData, AttrValue, "表号（序列号）", ""},
	{"0.0.96.1.1.255", ClassData, AttrValue, "逻辑设备名（备用）", ""},
	{"0.0.96.1.2.255", ClassData, AttrValue, "固件版本", ""},
	{"0.0.96.1.3.255", ClassData, AttrValue, "厂商名称", ""},
	// 关联与寻址
	{"0.0.40.0.0.255", ClassAssociationLN, AttrValue, "Association LN 对象表", ""},
	{"0.0.41.0.0.255", ClassSAPAssignment, AttrValue, "SAP 分配表", ""},
	// 时钟（class 8，attribute 2 = time）
	{"0.0.1.0.0.255", ClassClock, AttrValue, "电表时钟", ""},
	// 需量（class 5）
	{"1.0.1.6.0.255", ClassDemandRegister, AttrValue, "正向有功需量", "W"},
}

// LookupOBIS 查常见 OBIS 的语义。ok=false 表示不在表里（很可能是厂商私有）。
func LookupOBIS(s string) (CommonObject, bool) {
	norm := normalizeOBIS(s)
	for _, o := range commonObjects {
		if normalizeOBIS(o.OBIS) == norm {
			return o, true
		}
	}
	return CommonObject{}, false
}

// CommonObjects 返回常见 OBIS 表的副本，供 CLI 的 `-list` 使用。
func CommonObjects() []CommonObject {
	out := make([]CommonObject, len(commonObjects))
	copy(out, commonObjects)
	return out
}

// normalizeOBIS 把 `1-0-1-8-0-255` 与 `1.0.1.8.0.255` 归一，也去掉前导零。
func normalizeOBIS(s string) string {
	o, err := ParseOBIS(s)
	if err != nil {
		return strings.TrimSpace(s)
	}
	return o.String()
}
