package secscodec

import (
	"errors"
	"fmt"
)

// 本文件只做「SxFy 消息体 ↔ 平台结构」的纯函数转换，不做 I/O。
//
// 这里刻意只实现网关采集真正用到的一小组消息：
//
//	建立通信  S1F1/S1F2（Are You There）、S1F13/S1F14（Establish Communications）
//	状态变量  S1F3/S1F4（Selected Equipment Status）、S1F11/S1F12（Namelist）
//	设备常数  S2F13/S2F14（Equipment Constant Request）、S2F15/S2F16（New EC Send）
//	事件报告  S6F11/S6F12（Event Report Send）
//	报警报告  S5F1/S5F2（Alarm Report Send）
//
// 不做 S2F17（时间）、S2F31（时钟）、S2F33~S2F45（报告定义/链接）等：它们
// 都是「改设备侧配置」的动作，配错会让设备停止上报，属于工程调试工具而
// 不是采集进程该做的事。

// 对象类型的线上词汇。SVID 与 ECID 是两套独立编号空间，仿真与驱动共用这
// 两个常量，避免一边写 "svid"、一边写 "SVID" 的字符串漂移。
const (
	ObjectSVID = "svid"
	ObjectECID = "ecid"
)

// SF 是一条消息的 Stream/Function 标识。
type SF struct {
	Stream   uint8
	Function uint8
}

// String 渲染为 `S1F3`。
func (s SF) String() string { return fmt.Sprintf("S%dF%d", s.Stream, s.Function) }

// 常用消息的 SF 常量。命名用「请求 → 应答」成对出现，读代码时不必回头
// 查标准。
var (
	SFAreYouThere      = SF{1, 1}
	SFOnLineData       = SF{1, 2}
	SFSelectedStatusRQ = SF{1, 3}
	SFSelectedStatus   = SF{1, 4}
	SFStatusNamelistRQ = SF{1, 11}
	SFStatusNamelist   = SF{1, 12}
	SFEqConstantRQ     = SF{2, 13}
	SFEqConstant       = SF{2, 14}
	SFEqConstantSend   = SF{2, 15}
	SFEqConstantAck    = SF{2, 16}
	SFAlarmReport      = SF{5, 1}
	SFAlarmAck         = SF{5, 2}
	SFEventReport      = SF{6, 11}
	SFEventAck         = SF{6, 12}
	SFCommRequest      = SF{1, 13}
	SFCommEstablished  = SF{1, 14}
)

// ---------------------------------------------------------------------------
// SVID / ECID 请求与应答
// ---------------------------------------------------------------------------

// EncodeIDList 构造 `L,n { U4 }`，用于 S1F3 / S2F13 / S1F11 的请求体。
//
// 用 U4 而不是 U2：SVID/ECID 的上限在标准里是 4 字节，现场大量设备对
// U2 与 U4 都接受，但**只接受其中一种**。选择 U4 是因为它不会因为编号
// 超过 65535 而被静默截断——截断后读到的是另一个变量的值，比报错难查
// 得多。
func EncodeIDList(ids []uint32) ([]byte, error) {
	items := make([]*Item, 0, len(ids))
	for _, id := range ids {
		items = append(items, Int(FormatU4, int64(id)))
	}
	return EncodeItem(List(items...))
}

// DecodeIDList 解析 `L,n { U4 }`（同时容忍 U2/U1，现场设备两种写法都有）。
func DecodeIDList(body []byte) ([]uint32, error) {
	it, err := decodeSingle(body)
	if err != nil {
		return nil, err
	}
	if it.Format != FormatList {
		return nil, formatErr(FormatList, it.Format)
	}
	out := make([]uint32, 0, len(it.Children))
	for i, c := range it.Children {
		v, err := itemUint(c)
		if err != nil {
			return nil, fmt.Errorf("secscodec: 第 %d 个编号: %w", i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// StatusValue 是一个 SVID/ECID 的取值。
//
// 值本身用 *Item 保留原始格式：同一个 SVID 在不同设备上可能是 A、F4、U4，
// 平台侧按点位声明的类型再转换。放弃原始格式（例如一律转成 float64）会
// 让字符串型状态变量变成一串 0。
type StatusValue struct {
	ID    uint32
	Value *Item
}

// EncodeStatusReply 构造 `L,n { L,2 { U4, value } }`（S1F4 / S2F14 共用）。
func EncodeStatusReply(vals []StatusValue) ([]byte, error) {
	items := make([]*Item, 0, len(vals))
	for _, v := range vals {
		if v.Value == nil {
			return nil, fmt.Errorf("secscodec: SVID/ECID %d 没有值", v.ID)
		}
		items = append(items, List(Int(FormatU4, int64(v.ID)), v.Value))
	}
	return EncodeItem(List(items...))
}

// DecodeStatusReply 解析 `L,n { L,2 { U4, value } }`。
//
// 严格按「两个子项」校验：少一项说明对端把 S1F4 与 S2F14 的布局搞混了，
// 这时按位置硬猜只会把值配错点位。
func DecodeStatusReply(body []byte) ([]StatusValue, error) {
	it, err := decodeSingle(body)
	if err != nil {
		return nil, err
	}
	if it.Format != FormatList {
		return nil, formatErr(FormatList, it.Format)
	}
	out := make([]StatusValue, 0, len(it.Children))
	for i, c := range it.Children {
		if c.Format != FormatList || len(c.Children) != 2 {
			return nil, fmt.Errorf("secscodec: 第 %d 项不是 L,2（%s）", i+1, c.Text())
		}
		id, err := itemUint(c.Children[0])
		if err != nil {
			return nil, fmt.Errorf("secscodec: 第 %d 项的编号: %w", i+1, err)
		}
		out = append(out, StatusValue{ID: id, Value: c.Children[1]})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// S1F11 / S1F12：状态变量名
// ---------------------------------------------------------------------------

// StatusVariable 是一条 SVID 的名称与单位（S1F12 的一项）。
type StatusVariable struct {
	ID    uint32
	Name  string
	Units string
}

// EncodeStatusNameReply 构造 `L,n { L,3 { U4, A, A } }`（S1F12）。
func EncodeStatusNameReply(names []StatusVariable) ([]byte, error) {
	items := make([]*Item, 0, len(names))
	for _, n := range names {
		items = append(items, List(
			Int(FormatU4, int64(n.ID)), ASCII(n.Name), ASCII(n.Units)))
	}
	return EncodeItem(List(items...))
}

// DecodeStatusNameReply 解析 S1F12。
//
// 只接受 3 子项（编号、名称、单位）。有些设备的 S1F12 会带第 4 项
// （上下限），这里显式接受 3 或 4 项——4 项时忽略多出来的一项。
func DecodeStatusNameReply(body []byte) ([]StatusVariable, error) {
	it, err := decodeSingle(body)
	if err != nil {
		return nil, err
	}
	if it.Format != FormatList {
		return nil, formatErr(FormatList, it.Format)
	}
	out := make([]StatusVariable, 0, len(it.Children))
	for i, c := range it.Children {
		if c.Format != FormatList || (len(c.Children) != 3 && len(c.Children) != 4) {
			return nil, fmt.Errorf("secscodec: 第 %d 项不是 L,3/L,4（%s）", i+1, c.Text())
		}
		id, err := itemUint(c.Children[0])
		if err != nil {
			return nil, fmt.Errorf("secscodec: 第 %d 项的编号: %w", i+1, err)
		}
		name, _ := c.Children[1].String()
		units, _ := c.Children[2].String()
		out = append(out, StatusVariable{ID: id, Name: name, Units: units})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// S2F15 / S2F16：写设备常数
// ---------------------------------------------------------------------------

// Constant 是一条待写入的设备常数。
type Constant struct {
	ID    uint32
	Value *Item
}

// EncodeConstantSend 构造 `L,n { L,2 { U4, value } }`（S2F15）。
// 与 S1F4 的布局相同，但语义是「写」而不是「读的应答」。
func EncodeConstantSend(cs []Constant) ([]byte, error) {
	vals := make([]StatusValue, 0, len(cs))
	for _, c := range cs {
		vals = append(vals, StatusValue{ID: c.ID, Value: c.Value})
	}
	return EncodeStatusReply(vals)
}

// EncodeConstantAck 构造 S2F16 的应答体 `B,1`（EAC：0 表示接受）。
func EncodeConstantAck(ok bool) ([]byte, error) {
	b := byte(0)
	if !ok {
		b = 1
	}
	return EncodeItem(&Item{Format: FormatBinary, Raw: []byte{b}})
}

// DecodeAck 解析 `B,1` 形式的应答（S2F16 / S6F12 / S5F2 共用）：0 表示接受。
func DecodeAck(body []byte) (byte, error) {
	it, err := decodeSingle(body)
	if err != nil {
		return 0, err
	}
	if it.Format != FormatBinary {
		return 0, formatErr(FormatBinary, it.Format)
	}
	if len(it.Raw) != 1 {
		return 0, fmt.Errorf("secscodec: 应答码应是 1 字节，实际 %d", len(it.Raw))
	}
	return it.Raw[0], nil
}

// ---------------------------------------------------------------------------
// S1F13 / S1F14：建立通信
// ---------------------------------------------------------------------------

// EncodeEmptyList 构造 `L,0`（S1F13 的请求体与 S1F14 的第二项）。
func EncodeEmptyList() ([]byte, error) { return EncodeItem(List()) }

// EncodeCommAck 构造 `L,2 { B,1 COMMACK, L,0 }`（S1F14）。
func EncodeCommAck(ok bool) ([]byte, error) {
	b := byte(0)
	if !ok {
		b = 1
	}
	return EncodeItem(List(
		&Item{Format: FormatBinary, Raw: []byte{b}},
		List(),
	))
}

// DecodeCommAck 解析 S1F14，返回 COMMACK 是否为 0（接受）。
func DecodeCommAck(body []byte) (bool, error) {
	it, err := decodeSingle(body)
	if err != nil {
		return false, err
	}
	if it.Format != FormatList || len(it.Children) < 1 {
		return false, fmt.Errorf("secscodec: S1F14 应答体不是 L,2")
	}
	c := it.Children[0]
	if c.Format != FormatBinary || len(c.Raw) != 1 {
		return false, fmt.Errorf("secscodec: COMMACK 不是 B,1（%s）", c.Text())
	}
	return c.Raw[0] == 0, nil
}

// ---------------------------------------------------------------------------
// S1F1 / S1F2
// ---------------------------------------------------------------------------

// EncodeOnLineData 构造 `L,2 { A MDLN, A SOFTREV }`（S1F2）。
func EncodeOnLineData(mdln, softrev string) ([]byte, error) {
	return EncodeItem(List(ASCII(mdln), ASCII(softrev)))
}

// DecodeOnLineData 解析 S1F2，返回机型与软件版本。
func DecodeOnLineData(body []byte) (mdln, softrev string, err error) {
	it, err := decodeSingle(body)
	if err != nil {
		return "", "", err
	}
	if it.Format != FormatList || len(it.Children) < 2 {
		return "", "", fmt.Errorf("secscodec: S1F2 应答体不是 L,2")
	}
	mdln, _ = it.Children[0].String()
	softrev, _ = it.Children[1].String()
	return mdln, softrev, nil
}

// ---------------------------------------------------------------------------
// S6F11：事件报告
// ---------------------------------------------------------------------------

// Report 是一条报告（RPTID + 若干 VID 取值）。
type Report struct {
	ReportID uint32
	Values   []StatusValue
}

// Event 是一条 S6F11 事件报告。
//
// 结构：`L,3 { DATAID, CEID, L,n { L,2 { RPTID, L,m { L,2 { VID, V } } } } }`。
// 这里把「报告」这一层保留下来而不是拍平成一张 VID 表：同一个 CEID 可以
// 带多份报告（不同 RPTID），拍平后同名 VID 会互相覆盖。
type Event struct {
	DataID  uint32
	CEID    uint32
	Reports []Report
}

// DecodeEventReport 解析 S6F11 的请求体。
func DecodeEventReport(body []byte) (Event, error) {
	var ev Event
	it, err := decodeSingle(body)
	if err != nil {
		return ev, err
	}
	if it.Format != FormatList || len(it.Children) < 3 {
		return ev, fmt.Errorf("secscodec: S6F11 请求体不是 L,3")
	}
	if ev.DataID, err = itemUint(it.Children[0]); err != nil {
		return ev, fmt.Errorf("secscodec: DATAID: %w", err)
	}
	if ev.CEID, err = itemUint(it.Children[1]); err != nil {
		return ev, fmt.Errorf("secscodec: CEID: %w", err)
	}
	list := it.Children[2]
	if list.Format != FormatList {
		return ev, fmt.Errorf("secscodec: 报告列表不是 L（%s）", list.Text())
	}
	for i, r := range list.Children {
		if r.Format != FormatList || len(r.Children) != 2 {
			return ev, fmt.Errorf("secscodec: 第 %d 份报告不是 L,2（%s）", i+1, r.Text())
		}
		rptID, err := itemUint(r.Children[0])
		if err != nil {
			return ev, fmt.Errorf("secscodec: 第 %d 份报告的 RPTID: %w", i+1, err)
		}
		vals := r.Children[1]
		if vals.Format != FormatList {
			return ev, fmt.Errorf("secscodec: 第 %d 份报告的取值不是 L（%s）", i+1, vals.Text())
		}
		rep := Report{ReportID: rptID}
		for j, v := range vals.Children {
			if v.Format != FormatList || len(v.Children) != 2 {
				return ev, fmt.Errorf("secscodec: 第 %d 份报告的第 %d 个值不是 L,2（%s）",
					i+1, j+1, v.Text())
			}
			vid, err := itemUint(v.Children[0])
			if err != nil {
				return ev, fmt.Errorf("secscodec: 第 %d 份报告的第 %d 个 VID: %w", i+1, j+1, err)
			}
			rep.Values = append(rep.Values, StatusValue{ID: vid, Value: v.Children[1]})
		}
		ev.Reports = append(ev.Reports, rep)
	}
	return ev, nil
}

// EncodeEventReport 构造 S6F11 的请求体（内置从站用）。
func EncodeEventReport(ev Event) ([]byte, error) {
	reports := make([]*Item, 0, len(ev.Reports))
	for _, r := range ev.Reports {
		vals := make([]*Item, 0, len(r.Values))
		for _, v := range r.Values {
			if v.Value == nil {
				return nil, fmt.Errorf("secscodec: VID %d 没有值", v.ID)
			}
			vals = append(vals, List(Int(FormatU4, int64(v.ID)), v.Value))
		}
		reports = append(reports, List(Int(FormatU4, int64(r.ReportID)), List(vals...)))
	}
	return EncodeItem(List(
		Int(FormatU4, int64(ev.DataID)),
		Int(FormatU4, int64(ev.CEID)),
		List(reports...),
	))
}

// LookupVID 在一条事件里按 VID 取值。第二个返回值为 false 表示这条事件
// 没有携带该 VID。
//
// 多份报告里出现同一个 VID 时取**最后一份**：设备按报告顺序发送，
// 后面的覆盖前面的，与「最新状态」的直觉一致。
func (ev Event) LookupVID(vid uint32) (*Item, bool) {
	var found *Item
	for _, r := range ev.Reports {
		for _, v := range r.Values {
			if v.ID == vid {
				found = v.Value
			}
		}
	}
	return found, found != nil
}

// ---------------------------------------------------------------------------
// S5F1：报警报告
// ---------------------------------------------------------------------------

// Alarm 是一条 S5F1 报警报告。
//
// ALCD 是一个字节：bit7 置位表示「报警发生」，否则表示「报警解除」；
// 低 7 位是报警类别。把「发生/解除」与「类别」塞进同一个字节是现场最
// 常见的误读来源——把整个字节当布尔，会让「解除」被当成「发生」。
type Alarm struct {
	ALCD  uint8
	ALID  uint32
	ALTX  string
	IsSet bool  // ALCD bit7
	Kind  uint8 // ALCD 低 7 位
}

// DecodeAlarmReport 解析 S5F1 的请求体 `L,3 { B ALCD, U4 ALID, A ALTX }`。
func DecodeAlarmReport(body []byte) (Alarm, error) {
	var a Alarm
	it, err := decodeSingle(body)
	if err != nil {
		return a, err
	}
	if it.Format != FormatList || len(it.Children) < 2 {
		return a, fmt.Errorf("secscodec: S5F1 请求体不是 L,3")
	}
	alcd := it.Children[0]
	if alcd.Format != FormatBinary || len(alcd.Raw) != 1 {
		return a, fmt.Errorf("secscodec: ALCD 不是 B,1（%s）", alcd.Text())
	}
	a.ALCD = alcd.Raw[0]
	a.IsSet = a.ALCD&0x80 != 0
	a.Kind = a.ALCD & 0x7F
	if a.ALID, err = itemUint(it.Children[1]); err != nil {
		return a, fmt.Errorf("secscodec: ALID: %w", err)
	}
	if len(it.Children) > 2 {
		a.ALTX, _ = it.Children[2].String()
	}
	return a, nil
}

// EncodeAlarmReport 构造 S5F1 的请求体（内置从站用）。
//
// ALCD 的语义是「bit7 发生/解除」与「低 7 位类别」拼在同一个字节里：编码器
// 必须重新拼，否则会丢掉类别，或把上一次遗留的 bit7 带进新的报警里。
func EncodeAlarmReport(a Alarm) ([]byte, error) {
	alcd := a.ALCD & 0x7F
	if a.IsSet {
		alcd |= 0x80
	} else {
		alcd &^= 0x80
	}
	if a.Kind != 0 {
		alcd = a.Kind&0x7F | alcd&0x80
	}
	return EncodeItem(List(
		&Item{Format: FormatBinary, Raw: []byte{alcd}},
		Int(FormatU4, int64(a.ALID)),
		ASCII(a.ALTX),
	))
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// decodeSingle 要求 body 里正好是一个顶层数据项。
//
// 「正好一个」而不是「至少一个」是刻意严格的：多出来的项说明对端把布局
// 搞错了（例如把 L,0 也编进去），按第一项硬解会把真正的字段读成别的。
func decodeSingle(body []byte) (*Item, error) {
	if len(body) == 0 {
		return nil, errors.New("secscodec: 消息体为空")
	}
	it, used, err := DecodeItem(body)
	if err != nil {
		return nil, err
	}
	if used != len(body) {
		return nil, fmt.Errorf("secscodec: 消息体有 %d 字节未被解析", len(body)-used)
	}
	return it, nil
}

// itemUint 从 U1/U2/U4/U8/I1/I2/I4/I8 里取一个非负整数（编号类字段）。
// 编号在标准里是无符号的；接受有符号写法是因为部分设备用 I4 编码 SVID。
func itemUint(it *Item) (uint32, error) {
	switch it.Format {
	case FormatU1:
		v, err := it.U1()
		return uint32(v), err
	case FormatU2:
		v, err := it.U2()
		return uint32(v), err
	case FormatU4:
		v, err := it.U4()
		return v, err
	case FormatU8:
		v, err := it.U8()
		if v > 0xFFFFFFFF {
			return 0, fmt.Errorf("secscodec: 编号 %d 超出 32 位", v)
		}
		return uint32(v), err
	case FormatI1:
		v, err := it.I1()
		if v < 0 {
			return 0, fmt.Errorf("secscodec: 编号为负（%d）", v)
		}
		return uint32(v), err
	case FormatI2:
		v, err := it.I2()
		if v < 0 {
			return 0, fmt.Errorf("secscodec: 编号为负（%d）", v)
		}
		return uint32(v), err
	case FormatI4:
		v, err := it.I4()
		if v < 0 {
			return 0, fmt.Errorf("secscodec: 编号为负（%d）", v)
		}
		return uint32(v), err
	}
	return 0, fmt.Errorf("%w：编号不能是 %s", ErrFormatMismatch, FormatName(it.Format))
}
