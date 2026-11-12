package mbus

import (
	"fmt"
	"strings"
)

// 本文件把链路层的帧与「业务上的应答」接起来。
//
// 驱动只关心「这帧是数据还是错误」，不该自己判断 CI 是 72 还是 73、该调
// ParseVariableData 还是 ParseFixedData、固定结构要不要再包一层。把这段
// 分派放在 codec 里，主站与内置从站才能对着同一份判定逻辑写测试。

// DataKind 是应答里数据部分的解释方式。
type DataKind byte

const (
	// DataVariable 是 CI=72h/76h 可变结构。
	DataVariable DataKind = iota + 1
	// DataFixed 是 CI=73h/77h 固定结构。
	DataFixed
	// DataNone 表示没有数据域（控制帧 / 空应答）。
	DataNone
)

func (k DataKind) String() string {
	switch k {
	case DataVariable:
		return "variable"
	case DataFixed:
		return "fixed"
	}
	return "none"
}

// Response 是一条从站应答解出的业务内容。
//
// Records 把可变结构与固定结构统一成同一种形状：驱动按 `q:` 名字取一条
// 记录，不必关心报文用的是哪种结构。固定结构的两个计数器会被命名为
// `q:energy`、`q:energy-2` 这样的量名（见 ParseFixedData）。
type Response struct {
	Kind   DataKind
	Header Header
	// Records 是按报文顺序排列的记录。固定结构恒为 2 条。
	Records []Record
	// MoreRecordsFollow 表示从站还有后续报文。
	MoreRecordsFollow bool
	// ErrorCode 是 CI=70h 时的应用错误码。
	ErrorCode byte
	// ErrorText 是错误码的可读解释。
	ErrorText string
	// VendorData 是厂商特定记录的原始字节。
	VendorData []byte
}

// ApplicationError 把 CI=70h 的错误码翻译成中文说明。
//
// 这些码是标准枚举（EN 13757-3 §6.6），不是厂商私有串；把它们翻出来
// 是因为现场最常见的问题（CI 不支持、缓冲溢出）都靠它定位。
func ApplicationError(code byte) string {
	switch code {
	case 0x00:
		return "未指明的错误"
	case 0x01:
		return "从站不支持该 CI（控制信息）"
	case 0x02:
		return "从站缓冲区溢出（应答过长）"
	case 0x03:
		return "记录过多"
	case 0x04:
		return "记录提前结束（数据不完整）"
	case 0x05:
		return "DIFE 过多"
	case 0x06:
		return "VIFE 过多"
	case 0x07:
		return "存储号过多"
	case 0x08:
		return "单位/量纲不支持"
	case 0x09:
		return "单位/量纲被禁止"
	case 0x0A:
		return "该记录不允许本方向读取"
	case 0x0B:
		return "该记录不允许本方向写入"
	case 0x0C:
		return "无此数据（下一条记录不存在）"
	case 0x0D:
		return "数据长度与 DIF 不符"
	case 0x0E:
		return "未知的记录结构"
	case 0x0F:
		return "记录过长"
	case 0x10:
		return "记录过短"
	case 0x11:
		return "请求未被接受"
	}
	return fmt.Sprintf("未知错误码 %02X", code)
}

// CheckResponse 判断一帧是不是合格的从站数据应答。
//
// 除了校验 C 字段（PRM=0、功能码 8 = RSP_UD），还要看 DFC：置位表示
// 从站缓冲已满，标准建议主站此时收缩后续请求。这里不自动重试，只把
// 事实告诉调用方——重试策略属于驱动。
func CheckResponse(fr Frame) error {
	if fr.Type != FrameLong && fr.Type != FrameControl && fr.Type != FrameShort {
		return fmt.Errorf("%w: 期望长帧应答，得到 %s 帧", ErrFrame, fr.Type)
	}
	if fr.Master() {
		return fmt.Errorf("%w: C=%02X 的 PRM 位为 1（这是主站报文，不是应答）", ErrFrame, fr.C)
	}
	if fr.Type == FrameShort {
		// 短帧应答只用于 SND_NKE 的极端变体；正常 RSP_UD 一定带数据。
		return fmt.Errorf("%w: 短帧不含数据域，不能作为读表应答", ErrFrame)
	}
	if fr.Function() != FuncRSPUD {
		return fmt.Errorf("%w: 应答功能码是 %X，应为 RSP_UD(8)", ErrFrame, fr.Function())
	}
	return nil
}

// ParseResponse 解析一帧从站应答。
//
// CI 有两种"模式"：Mode 1 用 72h/73h，Mode 2（老式、MSB 置位）用 76h/77h。
// 两种数据域布局完全一致，只是控制字节不同；把 76h 漏掉会让一整批老表
// 被报成"不支持的 CI"，而报文内容其实是好的。
func ParseResponse(fr Frame) (Response, error) {
	if err := CheckResponse(fr); err != nil {
		return Response{}, err
	}
	out := Response{}
	switch fr.CIField {
	case CIVariableData, 0x76:
		out.Kind = DataVariable
		vd, err := ParseVariableData(fr.Data)
		if err != nil {
			return out, err
		}
		out.Header = vd.Header
		out.Records = vd.Records
		out.MoreRecordsFollow = vd.MoreRecordsFollow
		out.VendorData = vd.VendorData
		if vd.Header.StatusError() {
			out.ErrorText = "从站状态字报应用错误：" + vd.Header.StatusText()
		}
		return out, nil
	case CIFixedData, 0x77:
		out.Kind = DataFixed
		fd, err := ParseFixedData(fr.Data)
		if err != nil {
			return out, err
		}
		out.Header = fd.Header
		out.Records = fd.Counters[:]
		if fd.Header.StatusError() {
			out.ErrorText = "从站状态字报应用错误：" + fd.Header.StatusText()
		}
		return out, nil
	case CIGeneralError:
		out.Kind = DataNone
		if len(fr.Data) > 0 {
			out.ErrorCode = fr.Data[0]
			out.ErrorText = ApplicationError(fr.Data[0])
		} else {
			out.ErrorText = "通用错误应答（无错误码）"
		}
		return out, nil
	case CIAlarmStatus:
		out.Kind = DataNone
		out.ErrorText = "告警状态应答（CI=71h），本实现不解析其内容"
		return out, nil
	}
	return out, fmt.Errorf("%w: 不支持的 CI=%02X（支持 70h/71h/72h/73h/76h/77h）", ErrFrame, fr.CIField)
}

// Describe 返回一条记录的单行描述，供自检命令与日志使用。
//
// 输出刻意包含 DIF/VIF 原始字节：现场拿到一份"读出来不对"的报文时，
// 唯一能对照手册核对的锚点就是这几个十六进制值。
func (r Record) Describe() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d] %-24s", r.Index, r.Quantity)
	switch r.Kind {
	case KindNumeric:
		sb.WriteString(fmt.Sprintf("%-16s", trimFloat(r.Num)))
	case KindText:
		sb.WriteString(fmt.Sprintf("%-16q", r.Text))
	case KindBinary:
		sb.WriteString(fmt.Sprintf("%-16x", r.Raw))
	default:
		sb.WriteString(fmt.Sprintf("%-16s", "-"))
	}
	if r.Unit != "" {
		fmt.Fprintf(&sb, " %-8s", r.Unit)
	} else {
		sb.WriteString(fmt.Sprintf("%-8s", ""))
	}
	fmt.Fprintf(&sb, " DIF=%02X VIF=%02X", r.DIF, r.VIF)
	if len(r.VIFEs) > 0 {
		fmt.Fprintf(&sb, " VIFE=%x", r.VIFEs)
	}
	if len(r.DIFEs) > 0 {
		fmt.Fprintf(&sb, " DIFE=%x", r.DIFEs)
	}
	if r.Storage > 0 {
		fmt.Fprintf(&sb, " storage=%d", r.Storage)
	}
	if r.Tariff > 0 {
		fmt.Fprintf(&sb, " tariff=%d", r.Tariff)
	}
	if r.Subunit > 0 {
		fmt.Fprintf(&sb, " subunit=%d", r.Subunit)
	}
	return sb.String()
}

// trimFloat 去掉浮点数尾部多余的零，但保留足够精度。
func trimFloat(v float64) string {
	s := fmt.Sprintf("%.6f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
