package iec60870

// 本文件是「点位语义」层：把平台点位声明翻译成远动应用层的对象/命令，
// 以及把上送对象翻译成平台值。
//
// 为什么它属于共享包而不是各自驱动：
//
//   - 「双点 0/3 是非法状态而不是『中间』」「归一化设定值必须落在 -1..1」
//     「未声明命令类型的可写点按平台类型推导命令」——这些判断与承载方式
//     （以太网 / 串行总线）无关，写两遍必然漂移；
//   - 漂移的症状不是编译错误，而是「同一份点表走串口和走网口行为不一样」，
//     现场极难定位。
//
// 与驱动的关系：这里只做纯映射，不碰链路、不做重传、不读时钟。链路层
// 的差异（序号确认 vs FT1.2 轮询）留在两个驱动里。

import (
	"fmt"
	"strings"

	"uic/internal/driver"
	"uic/internal/model"
)

// 地址 area 的取值：声明点位的对象类别，决定写入时用哪种命令。
//
// 为什么把「命令类型」放在 area 而不是 dataType：同一份点表里，一个 float32
// 点位可能是短浮点设定值，也可能是标度化设定值（放大后的整数）。dataType
// 只说明平台怎么存这个数，不说明设备接受哪种命令。
const (
	AreaIOA                = "ioa"                 // 监视方向，写入时按 dataType 推导命令类型
	AreaSingleCommand      = "single-command"      // 单点命令
	AreaDoubleCommand      = "double-command"      // 双点命令
	AreaSetpointFloat      = "setpoint-float"      // 短浮点设定值命令
	AreaSetpointScaled     = "setpoint-scaled"     // 标度化设定值命令
	AreaSetpointNormalized = "setpoint-normalized" // 归一化设定值命令
)

// Areas 返回全部合法的对象类别，供配置界面枚举。
func Areas() []string {
	return []string{AreaIOA, AreaSingleCommand, AreaDoubleCommand,
		AreaSetpointFloat, AreaSetpointScaled, AreaSetpointNormalized}
}

// Plan 是一个点位的应用层规划结果。
//
// 刻意不含「第几字节」「寄存器个数」这类承载相关的字段：远动应用层按
// 信息对象地址寻址，宽度由类型标识决定，点位声明里写 length 只可能是误配。
type Plan struct {
	Ref      driver.PointRef
	IOA      uint32
	Area     string
	DataType model.DataType
}

// PlanPoint 校验并规范化一条点位声明。
//
// proto 只用于错误消息（"iec101" / "iec104"）；maxIOA 是承载方允许的信息
// 对象地址上限，由剖面决定（1 字节地址空间与 3 字节地址空间差三个数量级，
// 用同一个上限去校验会让错误推迟到编码阶段才暴露）。
func PlanPoint(p driver.PointRef, proto string, maxIOA uint32) (*Plan, error) {
	if p.Address.Kind != model.AddressRegister {
		return nil, driver.E(driver.ErrAddress, "open", "",
			fmt.Sprintf("point %q: %s requires register addressing (area=ioa, offset=IOA), got %q",
				p.ID, proto, p.Address.Kind), false, nil)
	}
	area := strings.ToLower(strings.TrimSpace(p.Address.Area))
	switch area {
	case AreaIOA, AreaSingleCommand, AreaDoubleCommand,
		AreaSetpointFloat, AreaSetpointScaled, AreaSetpointNormalized:
	default:
		return nil, driver.E(driver.ErrAddress, "open", "",
			fmt.Sprintf("point %q: unknown area %q (want %s)",
				p.ID, p.Address.Area, strings.Join(Areas(), ", ")), false, nil)
	}
	ioa := p.Address.Offset
	if ioa <= 0 || uint32(ioa) > maxIOA {
		return nil, driver.E(driver.ErrAddress, "open", "",
			fmt.Sprintf("point %q: IOA %d out of range 1..%d", p.ID, ioa, maxIOA), false, nil)
	}
	if p.Address.Length != 0 || p.Address.Bit > 0 {
		return nil, driver.E(driver.ErrAddress, "open", "",
			fmt.Sprintf("point %q: length/bit are not used by %s", p.ID, proto), false, nil)
	}
	switch p.DataType {
	case model.TypeBool, model.TypeInt16, model.TypeUint16, model.TypeInt32, model.TypeUint32,
		model.TypeInt64, model.TypeUint64, model.TypeFloat32, model.TypeFloat64:
	default:
		return nil, driver.E(driver.ErrAddress, "open", "",
			fmt.Sprintf("point %q: dataType %q is not supported by %s", p.ID, p.DataType, proto), false, nil)
	}
	return &Plan{Ref: p, IOA: uint32(ioa), Area: area, DataType: p.DataType}, nil
}

// Convert 把信息对象转换成点位声明的平台类型。
//
// 第二个返回值 false 表示「这个类型标识与这个平台类型根本对不上」（例如把
// 一条总召唤 ASDU 当成测量值），调用方应报协议错误而不是猜一个值。
func Convert(o Object, typeID byte, dt model.DataType) (model.Value, model.Quality, bool) {
	q := QualityOf(o.Quality)
	if dt == model.TypeBool {
		switch typeID {
		case TypeSinglePoint, TypeSinglePointTime:
			return model.BoolValue(o.Bool), q, true
		case TypeDoublePoint, TypeDoublePointTime:
			switch o.Uint {
			case 1: // 分
				return model.BoolValue(false), q, true
			case 2: // 合
				return model.BoolValue(true), q, true
			default:
				// 0/3 是「中间状态 / 不确定」。把 false 报成 good 会让
				// 「阀门状态未知」显示成「已关闭」——最危险的一类误报。
				return model.BoolValue(false), Worse(q, model.QualityUncertain), true
			}
		}
		return model.Value{}, model.QualityBad, false
	}
	if !dt.IsNumeric() {
		return model.Value{}, model.QualityBad, false
	}
	switch typeID {
	case TypeMeasuredNormalized, TypeMeasuredNormalizedTime,
		TypeMeasuredScaled, TypeMeasuredScaledTime,
		TypeMeasuredFloat, TypeMeasuredFloatTime:
		return model.NumValue(dt, o.Num), q, true
	case TypeIntegratedTotals, TypeIntegratedTotalsTime:
		return model.NumValue(dt, float64(o.Uint)), q, true
	case TypeSinglePoint, TypeSinglePointTime:
		if o.Bool {
			return model.NumValue(dt, 1), q, true
		}
		return model.NumValue(dt, 0), q, true
	case TypeDoublePoint, TypeDoublePointTime:
		return model.NumValue(dt, float64(o.Uint)), q, true
	}
	return model.Value{}, model.QualityBad, false
}

// QualityOf 把应用层品质位折成平台质量码。
func QualityOf(q Quality) model.Quality {
	switch {
	case q.Has(QInvalid):
		return model.QualityBad
	case q.Has(QNotTopical):
		return model.QualityStale
	case q.Has(QSubstituted), q.Has(QBlocked), q.Has(QOverflow):
		return model.QualityUncertain
	}
	return model.QualityGood
}

// Worse 取两个质量码里更差的一个（good < uncertain < stale < bad）。
func Worse(a, b model.Quality) model.Quality {
	rank := map[model.Quality]int{
		model.QualityGood: 0, model.QualityUncertain: 1, model.QualityStale: 2, model.QualityBad: 3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Numeric 把平台值读成浮点数（布尔按 0/1）。
func Numeric(v model.Value) (float64, bool) {
	if v.Type == model.TypeBool {
		if v.Bool {
			return 1, true
		}
		return 0, true
	}
	if !v.Type.IsNumeric() {
		return 0, false
	}
	return v.Num, true
}

// DoubleState 把平台值映射成双点命令的两位状态。
//
// 双点命令只有两个合法状态：1=分、2=合。0 与 3 在标准里是「不允许」，
// 不是「中间态」——把它们当合法值发下去，好设备会回否定确认，差设备
// 会静默不动，两种情况在现场都表现为「按钮点了没反应」。因此这里直接
// 拒绝，让错误在点击的那一刻就暴露出来。
func DoubleState(v model.Value) (byte, bool) {
	if v.Type == model.TypeBool {
		if v.Bool {
			return 2, true // 合
		}
		return 1, true // 分
	}
	f, ok := Numeric(v)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	switch int(f) {
	case 1, 2:
		return byte(f), true
	}
	return 0, false
}

// BuildCommand 构造一条控制方向的 ASDU（以太网承载的固定剖面）。
func BuildCommand(area string, ioa uint32, dataType model.DataType,
	req driver.WriteRequest, ca uint16) ([]byte, model.Value, error) {
	return BuildCommandProfile(area, ioa, dataType, req, ca, Profile104)
}

// BuildCommandProfile 按指定剖面构造控制方向的 ASDU。
func BuildCommandProfile(area string, ioa uint32, dataType model.DataType,
	req driver.WriteRequest, ca uint16, p Profile) ([]byte, model.Value, error) {
	a, value, err := CommandASDU(area, ioa, dataType, req, ca)
	if err != nil {
		return nil, model.Value{}, err
	}
	b, err := BuildASDUProfile(a, p)
	if err != nil {
		return nil, model.Value{}, driver.E(driver.ErrInternal, "write", "", err.Error(), false, err)
	}
	return b, value, nil
}

// CommandASDU 构造控制方向的 ASDU 结构，并返回「写下去的值」。
//
// 返回值里的 model.Value 是给驱动回填缓存用的：设备确认后、突发上送到达
// 之前，读回的值应当是刚写下去的那个，而不是上一轮采集的旧值。
//
// dataType 只在 area 为 ioa（未显式声明命令类型）时用于推导命令类型，以及
// 用于构造回填值的平台类型。
func CommandASDU(area string, ioa uint32, dataType model.DataType,
	req driver.WriteRequest, ca uint16) (ASDU, model.Value, error) {

	if area == AreaIOA {
		// 未显式声明命令类型的可写点：按平台类型推导。这个默认值在文档
		// 里写明，避免「同一份点表在两台设备上走不同命令」。
		switch dataType {
		case model.TypeBool:
			area = AreaSingleCommand
		case model.TypeFloat32, model.TypeFloat64:
			area = AreaSetpointFloat
		default:
			area = AreaSetpointScaled
		}
	}
	bad := func(format string, args ...any) (ASDU, model.Value, error) {
		return ASDU{}, model.Value{}, driver.E(driver.ErrAddress, "write", "",
			fmt.Sprintf(format, args...), false, nil)
	}
	switch area {
	case AreaSingleCommand:
		if req.Value.Type != model.TypeBool {
			return bad("point %q: single command requires bool value, got %s", req.PointID, req.Value.Type)
		}
		return ASDU{TypeID: TypeSingleCommand, Cause: CauseActivation, CommonAddr: ca,
			Objects: []Object{{IOA: ioa, Bool: req.Value.Bool}}}, req.Value, nil
	case AreaDoubleCommand:
		state, ok := DoubleState(req.Value)
		if !ok {
			return bad("point %q: double command requires bool or numeric 1 (off) / 2 (on), got %+v",
				req.PointID, req.Value)
		}
		return ASDU{TypeID: TypeDoubleCommand, Cause: CauseActivation, CommonAddr: ca,
			Objects: []Object{{IOA: ioa, Uint: uint32(state)}}}, req.Value, nil
	case AreaSetpointFloat:
		f, ok := Numeric(req.Value)
		if !ok {
			return bad("point %q: setpoint requires numeric value, got %s", req.PointID, req.Value.Type)
		}
		return ASDU{TypeID: TypeSetpointFloat, Cause: CauseActivation, CommonAddr: ca,
			Objects: []Object{{IOA: ioa, Num: float64(float32(f))}}}, model.NumValue(dataType, f), nil
	case AreaSetpointNormalized:
		f, ok := Numeric(req.Value)
		if !ok || f < -1 || f > 1 {
			return bad("point %q: normalized setpoint requires numeric value in -1..1", req.PointID)
		}
		return ASDU{TypeID: TypeSetpointNormalized, Cause: CauseActivation, CommonAddr: ca,
			Objects: []Object{{IOA: ioa, Num: f}}}, model.NumValue(dataType, f), nil
	case AreaSetpointScaled:
		f, ok := Numeric(req.Value)
		if !ok {
			return bad("point %q: setpoint requires numeric value, got %s", req.PointID, req.Value.Type)
		}
		if f < -32768 || f > 32767 {
			return bad("point %q: scaled setpoint %v out of int16 range", req.PointID, f)
		}
		return ASDU{TypeID: TypeSetpointScaled, Cause: CauseActivation, CommonAddr: ca,
			Objects: []Object{{IOA: ioa, Num: float64(int16(f))}}}, model.NumValue(dataType, f), nil
	}
	return bad("point %q: area %q is not a command type", req.PointID, area)
}
