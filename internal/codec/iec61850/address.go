package iec61850

import (
	"fmt"
	"strings"
)

// 本文件把点表里那种「人写得出来」的 IEC 61850 对象引用，翻译成 MMS 的
// ObjectName（域 + 项）。
//
// 这件事看起来只是字符串替换，实际是本协议最容易埋雷的地方：一个对象引用
// 有三段语义（逻辑设备、逻辑节点内的数据对象、数据属性），到了 MMS 层却被
// 压成「域 = 逻辑设备，项 = 其余部分用 $ 连接」。写错一处不会报语法错，
// 只会得到 object-non-existent——而人看到的名字和报文里的名字长得几乎一样。
//
// 语法（与点表的 symbol 地址一致）：
//
//	LD/LN.FC.DO[.DA...]      例：simpleIO/GGIO1.ST.Ind1.stVal
//	LD/LN.DO[.DA...]         省略 FC，由 device.labels.defaultFC 补齐
//
// 为什么 FC（功能约束）必须出现：MMS 的项名是 `LN$FC$DO$DA`，FC 是名字的
// 一段而不是元数据。少了它，`GGIO1$Ind1$stVal` 与 `GGIO1$ST$Ind1$stVal`
// 是两个不同的对象，只有一个存在。默认值来自设备标签，而不是从数据属性名
// 反推——反推需要完整的 SCL 模型，而驱动手上只有一份点表。

// standardFCs 是 IEC 61850-7-3 定义的功能约束。
//
// 只列会在 MMS 名字里出现的那一组。未知的两位大写串不当作 FC：把
// `LD/LN.AB.CD` 里的 `AB` 当成 FC 会悄悄改掉对象名，而这种错误在
// 报错信息里完全看不出来。
var standardFCs = map[string]bool{
	"ST": true, // 状态信息
	"MX": true, // 测量值
	"CO": true, // 控制
	"SP": true, // 定值
	"SV": true, // 取代值
	"CF": true, // 配置
	"DC": true, // 描述
	"SG": true, // 定值组
	"SE": true, // 定值组编辑
	"EX": true, // 扩展定义
	"SR": true, // 服务响应
	"OR": true, // 运行时间
	"BL": true, // 闭锁
}

// IsValidFC 判断 s 是否是 IEC 61850-7-3 的功能约束（ST/MX/CO/SP/CF/DC/SG/SE 等）。
//
// 驱动在解析 labels.defaultFC 时用：一个拼错的 FC（如 "ST " 带空格、或把数据
// 属性名 "stVal" 误当 FC）如果不在这里拦下，会变成对象名里的一段，MMS 层报
// object-non-existent——那种错误看着像服务端问题，实际是本地配置问题。
func IsValidFC(s string) bool {
	s = strings.ToUpper(strings.TrimSpace(s))
	return standardFCs[s]
}

// ObjectRef 是解析后的对象引用。
type ObjectRef struct {
	// LD 是逻辑设备名（MMS 的 domain）。
	LD string
	// LN 是逻辑节点名（如 LLN0、MMXU1）。
	LN string
	// FC 是功能约束（如 ST、MX）。
	FC string
	// Path 是 FC 之后的部分（如 `Ind1.stVal` 或 `TotW.mag.f`）。
	Path string
	// Raw 是原始文本，用于错误信息与日志。
	Raw string
}

// ParseObjectRef 解析一个对象引用。
//
// defaultFC 在引用里没写 FC 时使用；为空表示「必须写」。
func ParseObjectRef(expr, defaultFC string) (ObjectRef, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return ObjectRef{}, fmt.Errorf("iec61850: empty object reference")
	}
	slash := strings.IndexByte(raw, '/')
	if slash <= 0 {
		return ObjectRef{}, fmt.Errorf(
			"iec61850: object reference %q must be LD/LN.FC.DO[.DA] (a logical device prefix is required)", raw)
	}
	if slash == len(raw)-1 {
		return ObjectRef{}, fmt.Errorf("iec61850: object reference %q has an empty part after %q", raw, raw[:slash])
	}
	ld := raw[:slash]
	if err := checkIdentifier(ld, "logical device"); err != nil {
		return ObjectRef{}, err
	}

	parts := strings.Split(raw[slash+1:], ".")
	if len(parts) < 2 {
		// 只有 `LD/LN` 是一个逻辑节点，不是一个可读写的变量：MMS 里
		// 逻辑节点不是对象，它的数据对象才是。
		return ObjectRef{}, fmt.Errorf(
			"iec61850: object reference %q names a logical node; add a data object (e.g. %s.%s.Ind1.stVal)",
			raw, raw, "ST")
	}
	ln := parts[0]
	if err := checkIdentifier(ln, "logical node"); err != nil {
		return ObjectRef{}, err
	}
	if len(parts) < 3 {
		if fc := strings.ToUpper(defaultFC); fc != "" {
			return ObjectRef{}, fmt.Errorf(
				"iec61850: object reference %q has no data object; add FC.DO (e.g. %s.%s.Ind1.stVal)", raw, raw, fc)
		}
	}

	fc := ""
	rest := parts[1:]
	if isFC(rest[0]) {
		fc = rest[0]
		rest = rest[1:]
	} else if def := strings.ToUpper(strings.TrimSpace(defaultFC)); def != "" {
		if !standardFCs[def] {
			return ObjectRef{}, fmt.Errorf(
				"iec61850: defaultFC %q is not a functional constraint (want one of ST/MX/CO/SP/CF/DC/SG/SE)", defaultFC)
		}
		fc = def
	}
	if fc == "" {
		return ObjectRef{}, fmt.Errorf(
			"iec61850: object reference %q does not say which functional constraint the variable belongs to; "+
				"write it as %s/%s.<FC>.<DO>[.<DA>] (e.g. %s/%s.ST.<DO>.stVal) "+
				"or set labels.defaultFC on the device",
			raw, ld, ln, ld, ln)
	}
	if len(rest) == 0 {
		return ObjectRef{}, fmt.Errorf(
			"iec61850: object reference %q has FC %s but no data object after it", raw, fc)
	}
	for _, p := range rest {
		if p == "" {
			return ObjectRef{}, fmt.Errorf("iec61850: object reference %q has an empty segment (double dot?)", raw)
		}
		if err := checkIdentifier(p, "data object/attribute"); err != nil {
			return ObjectRef{}, err
		}
	}

	return ObjectRef{LD: ld, LN: ln, FC: fc, Path: strings.Join(rest, "."), Raw: raw}, nil
}

// isFC 判断一段是否是功能约束。
func isFC(s string) bool {
	// 功能约束必须是大写的两位。小写（st、mx）在 IEC 61850 的命名里是
	// 数据属性名的一部分，不是 FC：把它当 FC 会让 `LD/LN.st.Ind1` 悄悄
	// 变成 `LD/LN$st$Ind1`，而报出来的错误是 object-non-existent。
	return standardFCs[s] && s == strings.ToUpper(s)
}

// checkIdentifier 校验一段名字。
//
// IEC 61850 的标识符只允许字母、数字与下划线，且不能以数字开头。放宽它
// 没有好处：名字里出现 `-` 或空格时，几乎总是「从别的地方抄地址时带进来的
// 分隔符」，应当在使用前就报出来。
func checkIdentifier(s, what string) error {
	if s == "" {
		return fmt.Errorf("iec61850: empty %s name", what)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("iec61850: %s name %q must not start with a digit", what, s)
			}
		default:
			return fmt.Errorf("iec61850: %s name %q contains %q; only letters, digits and _ are allowed", what, s, r)
		}
	}
	return nil
}

// ObjectName 把对象引用转成 MMS 对象名。
//
// `simpleIO/GGIO1.ST.Ind1.stVal` →
// `domain=simpleIO, item=GGIO1$ST$Ind1$stVal`。
func (r ObjectRef) ObjectName() ObjectName {
	item := r.LN + "$" + r.FC
	if r.Path != "" {
		item += "$" + strings.ReplaceAll(r.Path, ".", "$")
	}
	return DomainObject(r.LD, item)
}

// String 返回规范化的对象引用（`LD/LN.FC.DO.DA`）。
func (r ObjectRef) String() string {
	out := r.LD + "/" + r.LN + "." + r.FC
	if r.Path != "" {
		out += "." + r.Path
	}
	return out
}

// DataSetName 构造一个数据集的对象名（`LD/LLN0.DS` → `LD` / `LLN0$DS`）。
//
// 数据集不是变量：MMS 里它是「命名变量表」，读它可以一次拿到整组成员。
// 本轮驱动只读变量，但模型浏览要用同一个名字构造函数，所以单独给一个入口。
func DataSetName(ld, ln, ds string) (ObjectName, error) {
	if err := checkIdentifier(ld, "logical device"); err != nil {
		return ObjectName{}, err
	}
	if err := checkIdentifier(ln, "logical node"); err != nil {
		return ObjectName{}, err
	}
	if err := checkIdentifier(ds, "data set"); err != nil {
		return ObjectName{}, err
	}
	return DomainObject(ld, ln+"$"+ds), nil
}

// SplitMMSName 把一个 MMS 项名（`GGIO1$ST$Ind1$stVal`）拆回 `LN.FC.Path`。
//
// 模型浏览时对端只回项名，人要看的是对象引用形态，这一步是展示用的。
// 不保证可逆：项名里的 `$` 是分隔符，而标识符本身不允许 `$`。
func SplitMMSName(item string) (ln, fc, path string) {
	parts := strings.Split(item, "$")
	if len(parts) == 0 {
		return "", "", ""
	}
	ln = parts[0]
	if len(parts) > 1 {
		fc = parts[1]
	}
	if len(parts) > 2 {
		path = strings.Join(parts[2:], ".")
	}
	return ln, fc, path
}
