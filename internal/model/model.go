// Package model 定义 UIC 的领域模型：通道、设备、点位、地址、采样与质量码。
//
// 该包不依赖任何其他内部包，是全部模块的共同语言。所有不变量在
// Validate 系列方法中集中校验，配置加载、模板实例化与 API 写入都必须
// 走同一套校验。
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 基础类型
// ---------------------------------------------------------------------------

// Duration 是带 JSON 字符串形态（"1s"）的时长，便于配置可读。
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = 0
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be string or nanoseconds: %w", err)
	}
	*d = Duration(time.Duration(n))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(time.Duration(d).String())), nil
}

// AddressKind 是地址的寻址范式。
type AddressKind string

const (
	AddressRegister AddressKind = "register" // 区 + 偏移 + 长度 + 位偏移
	AddressSymbol   AddressKind = "symbol"   // 路径 + 下标
	AddressObject   AddressKind = "object"   // 对象类型 + 实例 + 属性 + 索引
	AddressFile     AddressKind = "file"     // 文件 + 记录 + 偏移
	// AddressComputed 表示「值由表达式算出」，没有物理地址。表达式放在
	// Address.Expr 而不是 Point 上，是因为 driver.Read 只拿到 PointRef
	// （ID + Address + DataType）；放 Point 会迫使所有驱动都接收一个带
	// 表达式的结构，或给 PointRef 加一个只有一种驱动使用的字段。
	AddressComputed AddressKind = "computed"
)

var validAddressKinds = map[AddressKind]bool{
	AddressRegister: true, AddressSymbol: true, AddressObject: true, AddressFile: true,
	AddressComputed: true,
}

// DataType 是平台统一的数据类型；协议类型在驱动内映射到该集合。
type DataType string

const (
	TypeBool    DataType = "bool"
	TypeInt16   DataType = "int16"
	TypeUint16  DataType = "uint16"
	TypeInt32   DataType = "int32"
	TypeUint32  DataType = "uint32"
	TypeInt64   DataType = "int64"
	TypeUint64  DataType = "uint64"
	TypeFloat32 DataType = "float32"
	TypeFloat64 DataType = "float64"
	TypeString  DataType = "string"
	TypeBCD16   DataType = "bcd16"
	TypeBCD32   DataType = "bcd32"
	TypeBytes   DataType = "bytes"
)

var typeBytes = map[DataType]int{
	TypeBool: 0, TypeInt16: 2, TypeUint16: 2, TypeInt32: 4, TypeUint32: 4,
	TypeInt64: 8, TypeUint64: 8, TypeFloat32: 4, TypeFloat64: 8,
	TypeBCD16: 2, TypeBCD32: 4,
}

// IsNumeric 表示该类型可以用 float64 承载。
func (t DataType) IsNumeric() bool {
	switch t {
	case TypeInt16, TypeUint16, TypeInt32, TypeUint32, TypeInt64, TypeUint64,
		TypeFloat32, TypeFloat64, TypeBCD16, TypeBCD32:
		return true
	}
	return false
}

// Bytes 返回类型的字节宽度；bool 为 0（按位寻址），字符串/字节为 -1（需显式长度）。
func (t DataType) Bytes() int {
	if n, ok := typeBytes[t]; ok {
		return n
	}
	return -1
}

// Registers 返回该类型占用的 16 位寄存器个数。
func (t DataType) Registers() int {
	n := t.Bytes()
	if n <= 0 {
		return 1
	}
	return (n + 1) / 2
}

func (t DataType) Valid() bool {
	_, ok := typeBytes[t]
	if ok {
		return true
	}
	return t == TypeString || t == TypeBytes
}

// ByteOrder 描述 32/64 位值在 16 位寄存器之间的排列。
type ByteOrder string

const (
	OrderABCD ByteOrder = "ABCD"
	OrderCDAB ByteOrder = "CDAB"
	OrderBADC ByteOrder = "BADC"
	OrderDCBA ByteOrder = "DCBA"
)

func (o ByteOrder) Valid() bool {
	switch o {
	case "", OrderABCD, OrderCDAB, OrderBADC, OrderDCBA:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 地址
// ---------------------------------------------------------------------------

// Address 是四类寻址的统一表达。同一条地址只应填写与 Kind 对应的字段。
type Address struct {
	Kind AddressKind `json:"kind"`

	// register
	Area   string `json:"area,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Length int    `json:"length,omitempty"`
	Bit    int    `json:"bit,omitempty"`
	// DB 是 S7 数据块号（area=db 时必填）。其他协议留空。
	// 放进通用地址而不是塞进 Area 字符串，是为了让「同一个 area、不同 DB」
	// 在规划阶段就是两个不同的组——否则 DB1.DBW0 与 DB2.DBW0 会被合并成
	// 一次读取，读回来的值张冠李戴。
	DB int `json:"db,omitempty"`

	// symbol
	Path  string `json:"path,omitempty"`
	Index int    `json:"index,omitempty"`
	// Selector 是符号地址内部的二级选择器。MQTT 南向用它指定 JSON 载荷
	// 里的字段路径（"data.temperature" / "values[0].temp"）；其他符号型
	// 协议（如 OPC UA）留空。
	//
	// 为什么单开一个字段而不是塞进 Path：主题里允许出现 `#` 与 `+`，
	// 用分隔符拼接会与 MQTT 通配符撞车；而且「订阅哪个主题」与「取
	// 载荷里哪一段」是两件独立的事，混在一个字符串里无法分别校验。
	Selector string `json:"selector,omitempty"`

	// DPT 是 KNX 数据点类型（"9.001"、"1.001"、"5.001"…）。其他协议留空。
	//
	// KNX 的组地址只回答「哪个对象」，不回答「这个对象的值有多宽、怎么
	// 解释」：同一个 `1/2/3` 在 A 工程里是 1 位开关，在 B 工程里是 2 字节
	// 浮点温度。类型信息不在总线上，只在工程的组地址表里，因此必须随
	// 点位显式给出——由 dataType 反推会得到「f32 到底是 9.xxx 还是 14.xxx」
	// 这种无法回答的问题，而猜错不会报错，只会把温度读成 3.7e-41。
	//
	// 单开一个字段而不是复用 Selector：Selector 的语义是「载荷里的
	// 哪一段」（MQTT 的 JSON 字段路径），DPT 是「怎么解释整段字节」。
	// 混用会让 `1/2/3#9.001` 看起来像「取 JSON 的 9.001 字段」。
	DPT string `json:"dpt,omitempty"`

	// object
	ObjectType string `json:"objectType,omitempty"`
	Instance   int    `json:"instance,omitempty"`
	Property   string `json:"property,omitempty"`
	PropIndex  int    `json:"propIndex,omitempty"`
	// Priority 是 BACnet 写命令的优先级（1..16，0 = 不携带，设备按默认
	// 处理）。binary-output / analog-output 这类可命令对象靠它决定谁说了
	// 算；单开一个字段而不是塞进 Property 字符串，与 DPT 同理——「写哪个
	// 属性」和「以什么优先级写」是两件独立的事。
	Priority int `json:"priority,omitempty"`

	// file
	FileID   int `json:"fileId,omitempty"`
	RecordID int `json:"recordId,omitempty"`

	// computed
	// Expr 是计算点的表达式（`line-a:power / line-a:rate`）。引用写法用
	// 冒号而不是点号：设备 ID 与点位 ID 都允许包含点号，用点号分隔会产生
	// 「a.b.c 到底指谁」的歧义。
	Expr string `json:"expr,omitempty"`
}

var pathRe = regexp.MustCompile(`^[^\x00-\x1f\s]+$`)

// selectorRe 比 pathRe 更宽松一点：允许空格（JSON 字段名里出现空格虽少见
// 但合法），只禁止控制字符。空选择器由调用方视为「整个载荷」。
var selectorRe = regexp.MustCompile(`^[^\x00-\x1f]+$`)

// dptRe 只拦「明显不是 DPT」的字符串，不做归一化。
//
// 归一化（`9.1` → `9.001`、`DPT9` → `9`）与「这个子类型存不存在」的判断
// 都归协议 codec：那里有唯一一份类型表，模型再抄一份一定会漂移。模型这一
// 层只需要保证 DPT 字段里不是别的什么东西（例如把 selector 写进来了）。
var dptRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// 每个字段对哪些 Kind 合法。这是「一条地址只应填写与 Kind 对应的字段」的
// 唯一事实源：Validate 用它拒绝任何越界字段，Canonical 用同一分类丢弃越界
// 字段。历史上五个 Kind 各写一份内联检查，只有 computed 分支检查得全，其余
// 分支各自漏检了若干字段——一个 register 地址误带 `instance` 能通过校验，而
// Canonical 又把它静默丢掉，于是两个语义不同的地址坍缩成同一字符串、点表去
// 重时张冠李戴。集中成一张表后，新增字段只需在此登记一次，不可能再漏。
//
// 注意有两类字段对多个 Kind 都合法，不能从任何一侧拒绝：
//   - Offset：register 与 file 都用；
//   - Length：register 与 symbol 都用（symbol 用于 ADS STRING/bytes 的宽度）。
var fieldsByKind = map[AddressKind]map[string]bool{
	AddressRegister: {"area": true, "offset": true, "length": true, "bit": true, "db": true},
	AddressSymbol:   {"path": true, "index": true, "length": true, "selector": true, "dpt": true},
	AddressObject:   {"objecttype": true, "instance": true, "property": true, "propindex": true, "priority": true},
	AddressFile:     {"fileid": true, "recordid": true, "offset": true},
	AddressComputed: {"expr": true},
}

// mixedFieldsForKind 返回 a 中携带的、不属于该 Kind 的字段名列表（已排序，
// 便于错误信息与测试稳定）。空列表表示没有越界字段。
func mixedFieldsForKind(a Address) []string {
	allowed := fieldsByKind[a.Kind]
	var extra []string
	for name, present := range addressFieldPresence(a) {
		if present && !allowed[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return extra
}

// addressFieldPresence 报告每个字段是否「已被设置」。字符串字段以非空为准，
// 数值字段以非零为准——这与 Canonical 的丢弃逻辑一致（空串/0 在规范串里不
// 出现，因此它们被带上与否都不会改变地址身份，不算越界）。
func addressFieldPresence(a Address) map[string]bool {
	return map[string]bool{
		"area":       a.Area != "",
		"offset":     a.Offset != 0,
		"length":     a.Length != 0,
		"bit":        a.Bit != 0, // 注意：register 的 -1（无位）由 range 校验处理，不是越界
		"db":         a.DB != 0,
		"path":       a.Path != "",
		"index":      a.Index != 0,
		"selector":   a.Selector != "",
		"dpt":        a.DPT != "",
		"objecttype": a.ObjectType != "",
		"instance":   a.Instance != 0,
		"property":   a.Property != "",
		"propindex":  a.PropIndex != 0,
		"priority":   a.Priority != 0,
		"fileid":     a.FileID != 0,
		"recordid":   a.RecordID != 0,
		"expr":       a.Expr != "",
	}
}

// Validate 校验地址自身的一致性与范围。
func (a Address) Validate() error {
	if !validAddressKinds[a.Kind] {
		return fmt.Errorf("address.kind: unknown %q", a.Kind)
	}
	switch a.Kind {
	case AddressRegister:
		if a.Area == "" {
			return errors.New("address.area: required for register")
		}
		if a.Offset < 0 {
			return errors.New("address.offset: must be >= 0")
		}
		if a.Length < 0 || a.Length > 125 {
			return fmt.Errorf("address.length: must be 0..125, got %d", a.Length)
		}
		if a.Bit < -1 || a.Bit > 15 {
			return fmt.Errorf("address.bit: must be -1..15, got %d", a.Bit)
		}
		if extra := mixedFieldsForKind(a); len(extra) > 0 {
			return fmt.Errorf("address: mixed fields for register kind: %s", strings.Join(extra, ", "))
		}
		if a.DB < 0 {
			return errors.New("address.db: must be >= 0")
		}
		lower := strings.ToLower(a.Area)
		if lower == "db" && a.DB < 1 {
			return errors.New("address.db: required (>= 1) when area is db")
		}
		if lower != "db" && a.DB != 0 {
			return fmt.Errorf("address.db: only valid with area db, got area %q", a.Area)
		}
	case AddressSymbol:
		if a.Path == "" || !pathRe.MatchString(a.Path) {
			return errors.New("address.path: required and must not contain spaces/control chars")
		}
		if a.Index < -1 {
			return errors.New("address.index: must be >= -1")
		}
		if a.Length < 0 {
			return errors.New("address.length: must be >= 0")
		}
		if a.Selector != "" && !selectorRe.MatchString(a.Selector) {
			return errors.New("address.selector: must not contain spaces/control chars")
		}
		if a.DPT != "" && !dptRe.MatchString(a.DPT) {
			return fmt.Errorf("address.dpt: %q is not a datapoint type (want e.g. 9.001)", a.DPT)
		}
		if extra := mixedFieldsForKind(a); len(extra) > 0 {
			return fmt.Errorf("address: mixed fields for symbol kind: %s", strings.Join(extra, ", "))
		}
	case AddressObject:
		if a.ObjectType == "" || a.Property == "" {
			return errors.New("address.objectType/property: required for object")
		}
		if a.Instance < 0 {
			return errors.New("address.instance: must be >= 0")
		}
		// Priority 是 BACnet 写优先级：0（不携带）或 1..16。
		if a.Priority < 0 || a.Priority > 16 {
			return errors.New("address.priority: must be 0..16")
		}
		if extra := mixedFieldsForKind(a); len(extra) > 0 {
			return fmt.Errorf("address: mixed fields for object kind: %s", strings.Join(extra, ", "))
		}
	case AddressFile:
		if a.FileID < 0 || a.RecordID < 0 || a.Offset < 0 {
			return errors.New("address.file: fileId/recordId/offset must be >= 0")
		}
		if extra := mixedFieldsForKind(a); len(extra) > 0 {
			return fmt.Errorf("address: mixed fields for file kind: %s", strings.Join(extra, ", "))
		}
	case AddressComputed:
		if strings.TrimSpace(a.Expr) == "" {
			return errors.New("address.expr: required for computed")
		}
		// 上限不是为了省内存，而是为了防止一份配置把表达式引擎拖进
		// 指数级求值（深层嵌套的调用）。1024 字节足够写出任何现场公式。
		if len(a.Expr) > 1024 {
			return fmt.Errorf("address.expr: %d bytes exceeds the 1024-byte limit", len(a.Expr))
		}
		if !selectorRe.MatchString(a.Expr) {
			return errors.New("address.expr: must not contain control chars")
		}
		if extra := mixedFieldsForKind(a); len(extra) > 0 {
			return fmt.Errorf("address: mixed fields for computed kind: %s", strings.Join(extra, ", "))
		}
	}
	return nil
}

// Canonical 生成平台统一的规范化字符串，用于唯一性、日志与北向节点名。
// 驱动不得自行实现该规则。
func (a Address) Canonical() string {
	switch a.Kind {
	case AddressRegister:
		bit := ""
		if a.Bit >= 0 {
			bit = "." + strconv.Itoa(a.Bit)
		}
		if a.DB > 0 {
			return fmt.Sprintf("%s%d:%d%s:%d", a.Area, a.DB, a.Offset, bit, a.Length)
		}
		return fmt.Sprintf("%s:%d%s:%d", a.Area, a.Offset, bit, a.Length)
	case AddressSymbol:
		base := a.Path
		if a.Index >= 0 {
			base = fmt.Sprintf("%s[%d]", a.Path, a.Index)
		}
		if a.DPT != "" {
			// DPT 是地址的一部分而不是点位属性：同一个组地址在不同工程里
			// 可以是开关也可以是温度，两个点位必须能被区分开——否则点表
			// 的重复地址检查会把它们当成同一个数据项。
			base += "@" + a.DPT
		}
		if a.Selector != "" {
			// 用 `#` 分隔：主题里不会出现 `#`（MQTT 通配符在点位的
			// path 侧被禁止），因此不会与合法主题产生歧义。
			base += "#" + a.Selector
		}
		// Length 只对「宽度由点表给出」的符号协议有意义（ADS 的
		// STRING/bytes）；输出成 `NAME:81` 才能与导入语法往返。
		if a.Length > 0 {
			base += ":" + strconv.Itoa(a.Length)
		}
		return base
	case AddressObject:
		if a.PropIndex > 0 {
			return fmt.Sprintf("%s:%d.%s[%d]", a.ObjectType, a.Instance, a.Property, a.PropIndex)
		}
		return fmt.Sprintf("%s:%d.%s", a.ObjectType, a.Instance, a.Property)
	case AddressFile:
		return fmt.Sprintf("file:%d/%d:%d", a.FileID, a.RecordID, a.Offset)
	case AddressComputed:
		return "expr:" + a.Expr
	}
	return ""
}

// ---------------------------------------------------------------------------
// 值
// ---------------------------------------------------------------------------

// Value 是点位值的统一表达。数值放 Num，布尔放 Bool，文本放 Text。
type Value struct {
	Type DataType `json:"type"`
	Num  float64  `json:"num,omitempty"`
	Text string   `json:"text,omitempty"`
	Bool bool     `json:"bool,omitempty"`
}

func NumValue(t DataType, v float64) Value { return Value{Type: t, Num: v} }
func BoolValue(v bool) Value               { return Value{Type: TypeBool, Bool: v} }
func TextValue(t DataType, v string) Value { return Value{Type: t, Text: v} }
func BytesValue(v []byte) Value            { return Value{Type: TypeBytes, Text: string(v)} }

// AsFloat 把数值型值转为 float64；非数值返回 false。
func (v Value) AsFloat() (float64, bool) {
	if v.Type == TypeBool {
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

func (v Value) String() string {
	switch v.Type {
	case TypeBool:
		return strconv.FormatBool(v.Bool)
	case TypeString, TypeBytes:
		return v.Text
	}
	if v.Num == math.Trunc(v.Num) {
		// 只有落在 int64 范围内的整数才能走 FormatInt：uint64 的上半段
		// 直接转会把 1.8e19 变成 -9223372036854775808，日志与自检输出
		// 会给出一个完全错误的数。
		switch {
		case v.Num >= -9223372036854775808 && v.Num < 9223372036854775808:
			return strconv.FormatInt(int64(v.Num), 10)
		case v.Type == TypeUint64 && v.Num >= 0 && v.Num < 18446744073709551616:
			return strconv.FormatUint(uint64(v.Num), 10)
		}
	}
	return strconv.FormatFloat(v.Num, 'g', -1, 64)
}

// Equal 用于死区判断。数值用精确相等（死区在管线内单独处理）。
func (v Value) Equal(o Value) bool {
	if v.Type != o.Type {
		return false
	}
	switch v.Type {
	case TypeBool:
		return v.Bool == o.Bool
	case TypeString, TypeBytes:
		return v.Text == o.Text
	}
	return v.Num == o.Num
}

// ---------------------------------------------------------------------------
// 质量码
// ---------------------------------------------------------------------------

// Quality 描述数值可信度。
type Quality string

const (
	QualityGood         Quality = "good"
	QualityUncertain    Quality = "uncertain"
	QualityStale        Quality = "stale"
	QualityBad          Quality = "bad"
	QualityTimeout      Quality = "timeout"
	QualityCommError    Quality = "comm_error"
	QualityAddressError Quality = "address_error"
	QualityNotConnected Quality = "not_connected"
	QualityDisabled     Quality = "disabled"
)

var qualitySet = map[Quality]bool{
	QualityGood: true, QualityUncertain: true, QualityStale: true, QualityBad: true,
	QualityTimeout: true, QualityCommError: true, QualityAddressError: true,
	QualityNotConnected: true, QualityDisabled: true,
}

func (q Quality) Valid() bool { return qualitySet[q] }

// IsUsable 表示该质量的值可以参与计算与上报。
func (q Quality) IsUsable() bool {
	return q == QualityGood || q == QualityUncertain
}

// ---------------------------------------------------------------------------
// 点位
// ---------------------------------------------------------------------------

// Point 是设备内的一个可读写数据项。
type Point struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Address     Address   `json:"address"`
	DataType    DataType  `json:"dataType"`
	ByteOrder   ByteOrder `json:"byteOrder,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	Scale       float64   `json:"scale,omitempty"`
	Bias        float64   `json:"bias,omitempty"`
	Deadband    float64   `json:"deadband,omitempty"`
	Interval    Duration  `json:"interval,omitempty"`
	Writable    bool      `json:"writable,omitempty"`
	Enabled     bool      `json:"enabled"`
	Description string    `json:"description,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Scale 的有效值：0 视为 1（配置省略字段）。
func (p Point) EffectiveScale() float64 {
	if p.Scale == 0 {
		return 1
	}
	return p.Scale
}

// EffectiveInterval 返回点位自身周期或设备默认周期。
func (p Point) EffectiveInterval(d Device) time.Duration {
	if p.Interval > 0 {
		return p.Interval.Duration()
	}
	return d.Interval.Duration()
}

// Length 返回地址长度；未显式指定时由数据类型推导。
func (p Point) Length() int {
	if p.Address.Kind == AddressRegister && p.Address.Length > 0 {
		return p.Address.Length
	}
	if n := p.DataType.Registers(); n > 0 {
		return n
	}
	return 1
}

// Validate 校验点位全部不变量。
func (p Point) Validate() error {
	if !idRe.MatchString(p.ID) {
		return fmt.Errorf("point.id %q: must match %s", p.ID, idRe.String())
	}
	if err := p.Address.Validate(); err != nil {
		return fmt.Errorf("point %q: %w", p.ID, err)
	}
	if !p.DataType.Valid() {
		return fmt.Errorf("point %q: unknown dataType %q", p.ID, p.DataType)
	}
	if !p.ByteOrder.Valid() {
		return fmt.Errorf("point %q: unknown byteOrder %q", p.ID, p.ByteOrder)
	}
	if p.Scale == 0 {
		// 0 视为未配置，合法。
	}
	if p.Interval < 0 || (p.Interval > 0 && p.Interval.Duration() < 100*time.Millisecond) {
		return fmt.Errorf("point %q: interval must be 0 or >= 100ms", p.ID)
	}
	if p.Address.Kind == AddressRegister && p.Address.Bit >= 0 && p.DataType != TypeBool {
		return fmt.Errorf("point %q: address.bit >= 0 requires bool dataType", p.ID)
	}
	if p.Address.Kind == AddressComputed {
		// 计算点是纯函数的结果，没有可以写回去的地址。允许它 writable 会
		// 产生一条永远失败的写入路径——运维点了「下发」，只得到一个权限错，
		// 却不知道为什么。在配置期拒绝，错误信息才能说清原因。
		if p.Writable {
			return fmt.Errorf("point %q: computed points are read-only", p.ID)
		}
		// 表达式的结果只能是数值或布尔：字符串/Bytes 没有对应的运算符，
		// BCD 不是「值」而是一种寄存器编码，算出来也没有意义。
		switch p.DataType {
		case TypeBool, TypeInt16, TypeUint16, TypeInt32, TypeUint32,
			TypeInt64, TypeUint64, TypeFloat32, TypeFloat64:
		default:
			return fmt.Errorf("point %q: computed points require a numeric or bool dataType, got %q",
				p.ID, p.DataType)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 设备与通道
// ---------------------------------------------------------------------------

// Device 是一个可寻址的从站/站点。
type Device struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	ChannelID  string            `json:"channel"`
	UnitID     int               `json:"unitId,omitempty"`
	Interval   Duration          `json:"interval"`
	Timeout    Duration          `json:"timeout,omitempty"`
	ByteOrder  ByteOrder         `json:"byteOrder,omitempty"`
	Enabled    bool              `json:"enabled"`
	Points     []Point           `json:"points"`
	TemplateID string            `json:"templateId,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// TimeoutOrDefault 返回单次请求超时。
func (d Device) TimeoutOrDefault() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout.Duration()
	}
	return 3 * time.Second
}

// Validate 校验设备级不变量与点位集合。
func (d Device) Validate() error {
	if !idRe.MatchString(d.ID) {
		return fmt.Errorf("device.id %q: must match %s", d.ID, idRe.String())
	}
	if d.ChannelID == "" {
		return fmt.Errorf("device %q: channel required", d.ID)
	}
	if d.Interval.Duration() < 100*time.Millisecond {
		return fmt.Errorf("device %q: interval must be >= 100ms", d.ID)
	}
	seen := make(map[string]bool, len(d.Points))
	for _, p := range d.Points {
		if seen[p.ID] {
			return fmt.Errorf("device %q: duplicate point %q", d.ID, p.ID)
		}
		seen[p.ID] = true
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Transport 描述链路参数。
type Transport struct {
	Kind     string `json:"kind"` // tcp | serial | udp | memory
	Address  string `json:"address,omitempty"`
	Port     string `json:"port,omitempty"`
	Baud     int    `json:"baud,omitempty"`
	DataBits int    `json:"dataBits,omitempty"`
	Parity   string `json:"parity,omitempty"`
	StopBits int    `json:"stopBits,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
}

// Channel 是一条物理或逻辑链路，是并发与故障的边界。
type Channel struct {
	ID          string    `json:"id"`
	Protocol    string    `json:"protocol"`
	Transport   Transport `json:"transport"`
	Concurrency int       `json:"concurrency,omitempty"`
	Timeout     Duration  `json:"timeout,omitempty"`

	// Rack/Slot 是 S7 的 CPU 寻址参数，其他协议忽略。
	// Slot 用 0 表示「未配置」，按 2 处理：槽 0 是电源模块，不可能是
	// 通信目标，因此这个零值不会与合法配置冲突。
	Rack int `json:"rack,omitempty"`
	Slot int `json:"slot,omitempty"`

	// Subscribe 是 OPC UA 的取数方式开关（其他协议忽略）：true 表示
	// 让服务端建立订阅、主动推送变化；false（默认）表示每轮用 Read 轮询。
	//
	// 为什么是一个显式开关而不是「自动挑最好的」：两者的失败模式、
	// 带宽特征与服务端负载完全不同。自动切换会让同一份配置在两种
	// 行为之间漂移，现场无法判断「数据变慢了」是网络问题还是驱动
	// 悄悄换成了轮询。
	Subscribe bool `json:"subscribe,omitempty"`
	// PublishingInterval 是订阅的发布周期；0 表示跟随设备采集周期。
	// 只有 Subscribe=true 时有意义。
	PublishingInterval Duration `json:"publishingInterval,omitempty"`
	// SamplingInterval 是服务端的采样周期；0 表示跟随设备采集周期。
	// 它通常应当 ≤ PublishingInterval：采样比发布慢会让一个发布周期里
	// 丢掉中间的变化。
	SamplingInterval Duration `json:"samplingInterval,omitempty"`

	// Username/Password 是链路级凭据（MQTT 的 CONNECT、以及未来的
	// OPC UA 用户名认证）。放在通道而不是设备上：凭据属于 Broker/服务端，
	// 同一通道下的多台设备共用一份。
	//
	// 明文存在配置文件里是当前版本的真实状态：加密存储需要密钥管理，
	// 属于另一件事。文档里明确要求配置文件权限 0600，并在启动日志里
	// 只打印「是否配置了凭据」，不打印值。
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// EffectiveRack 返回机架号（默认 0）。
func (c Channel) EffectiveRack() int {
	if c.Rack < 0 {
		return 0
	}
	return c.Rack
}

// EffectiveSlot 返回槽号（默认 2，S7-300/400 的常见值）。
func (c Channel) EffectiveSlot() int {
	if c.Slot <= 0 {
		return 2
	}
	return c.Slot
}

// EffectiveConcurrency 返回通道并发上限（默认 4，范围 1..64）。
func (c Channel) EffectiveConcurrency() int {
	if c.Concurrency <= 0 {
		return 4
	}
	if c.Concurrency > 64 {
		return 64
	}
	return c.Concurrency
}

func (c Channel) EffectiveTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout.Duration()
	}
	return 3 * time.Second
}

// Validate 校验通道参数。
func (c Channel) Validate() error {
	if !idRe.MatchString(c.ID) {
		return fmt.Errorf("channel.id %q: must match %s", c.ID, idRe.String())
	}
	if c.Protocol == "" {
		return fmt.Errorf("channel %q: protocol required", c.ID)
	}
	switch c.Transport.Kind {
	case "tcp", "udp":
		if c.Transport.Address == "" {
			return fmt.Errorf("channel %q: transport.address required for %s", c.ID, c.Transport.Kind)
		}
	case "serial":
		if c.Transport.Port == "" {
			return fmt.Errorf("channel %q: transport.port required for serial", c.ID)
		}
		if c.Transport.Baud < 0 {
			return fmt.Errorf("channel %q: transport.baud must be positive", c.ID)
		}
		if c.Transport.DataBits != 0 && (c.Transport.DataBits < 5 || c.Transport.DataBits > 8) {
			return fmt.Errorf("channel %q: transport.dataBits %d out of range 5..8", c.ID, c.Transport.DataBits)
		}
		switch c.Transport.Parity {
		case "", "none", "even", "odd":
		default:
			return fmt.Errorf("channel %q: transport.parity %q must be none|even|odd", c.ID, c.Transport.Parity)
		}
		if c.Transport.StopBits != 0 && c.Transport.StopBits != 1 && c.Transport.StopBits != 2 {
			return fmt.Errorf("channel %q: transport.stopBits %d must be 1 or 2", c.ID, c.Transport.StopBits)
		}
	case "memory":
	default:
		return fmt.Errorf("channel %q: unknown transport kind %q", c.ID, c.Transport.Kind)
	}
	if c.Rack < 0 || c.Rack > 7 {
		return fmt.Errorf("channel %q: rack %d out of range 0..7", c.ID, c.Rack)
	}
	if c.Slot < 0 || c.Slot > 31 {
		return fmt.Errorf("channel %q: slot %d out of range 0..31", c.ID, c.Slot)
	}
	if c.PublishingInterval < 0 {
		return fmt.Errorf("channel %q: publishingInterval must be >= 0", c.ID)
	}
	if c.SamplingInterval < 0 {
		return fmt.Errorf("channel %q: samplingInterval must be >= 0", c.ID)
	}
	// 发布/采样周期只有订阅模式下才有意义。配了但没开订阅时静默忽略，
	// 会让「我把周期从 1s 改成 100ms，为什么还是 1s」变成一个没有线索
	// 的问题——所以这里显式拒绝。允许等于 0（跟随设备采集周期）。
	if !c.Subscribe && (c.PublishingInterval > 0 || c.SamplingInterval > 0) {
		return fmt.Errorf("channel %q: publishingInterval/samplingInterval require subscribe=true", c.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 采样
// ---------------------------------------------------------------------------

// Sample 是一次采集的产物，也是北向契约的核心对象。
type Sample struct {
	DeviceID    string     `json:"device"`
	PointID     string     `json:"point"`
	Value       Value      `json:"value"`
	Quality     Quality    `json:"quality"`
	DeviceTime  *time.Time `json:"deviceTime,omitempty"`
	CollectedAt time.Time  `json:"collectedAt"`
	Seq         uint64     `json:"seq"`
	Unit        string     `json:"unit,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// Key 返回快照键。
func (s Sample) Key() string { return s.DeviceID + "/" + s.PointID }

// WindowStats 是某个点位在一段时间窗口内的数值样本摘要。
//
// 放在 model 而不是 calc 或 history：它是三者之间的共享词表——历史库产出它、
// 计算点消费它、报警通过计算点间接使用它。让 history 依赖 calc（或反过来）
// 会把「表达式引擎」和「存储」绑在一起，而它们本可以各自独立测试。
//
// 只统计质量为 good 的数值样本：把 stale（断线时保留的最后值）算进平均，
// 等于让一段没有数据的时间也参与计算，这是最隐蔽的一类假数据。
type WindowStats struct {
	Count int     `json:"count"`
	Sum   float64 `json:"sum"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

// Avg 返回算术平均。Count 为 0 时返回 0：调用方必须先判 Count，
// 但也绝不能因为漏判而得到 NaN（NaN 会一路传播成一条坏曲线）。
func (w WindowStats) Avg() float64 {
	if w.Count <= 0 {
		return 0
	}
	return w.Sum / float64(w.Count)
}

// Validate 做最小一致性检查，防止坏值进入快照。
func (s Sample) Validate() error {
	if s.DeviceID == "" || s.PointID == "" {
		return errors.New("sample: device and point required")
	}
	if !s.Quality.Valid() {
		return fmt.Errorf("sample: unknown quality %q", s.Quality)
	}
	if s.Quality == QualityGood && s.Value.Type == "" {
		return errors.New("sample: good quality requires a typed value")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 从站映射（把点位暴露给外部主站）
// ---------------------------------------------------------------------------

// MappingArea 是映射使用的寄存器区，与 Modbus 的四个功能区一一对应。
type MappingArea string

const (
	MappingHolding  MappingArea = "holding"  // FC3 / FC6 / FC16
	MappingInput    MappingArea = "input"    // FC4
	MappingCoil     MappingArea = "coil"     // FC1 / FC5 / FC15
	MappingDiscrete MappingArea = "discrete" // FC2
)

func (a MappingArea) Valid() bool {
	switch a {
	case MappingHolding, MappingInput, MappingCoil, MappingDiscrete:
		return true
	}
	return false
}

// IsBits 表示该区按位寻址（每个映射占 1 位）。
func (a MappingArea) IsBits() bool { return a == MappingCoil || a == MappingDiscrete }

// AllowsWrite 表示该区允许外部主站写入。输入区与离散输入区是只读的，
// 这一点由协议决定，不是实现取舍。
func (a MappingArea) AllowsWrite() bool { return a == MappingHolding || a == MappingCoil }

// SlaveMapping 把「设备上的一个点位」映射到「从站的一个地址」。
//
// 没有单独的编码配置：字节序取所属单元，宽度取点位类型与地址长度。
// 允许逐点覆盖这些参数听起来更灵活，实际只会制造「同一台设备在上面读
// 出来是 12.5、在下面读出来是 0.0」的调试噩梦。
type SlaveMapping struct {
	Point    string      `json:"point"` // device/point
	Area     MappingArea `json:"area"`
	Offset   int         `json:"offset"`
	Writable bool        `json:"writable,omitempty"`
}

// SlaveUnit 是一个从站单元（Unit ID）的映射表。
type SlaveUnit struct {
	UnitID    int            `json:"unitId"`
	ByteOrder ByteOrder      `json:"byteOrder,omitempty"`
	Mappings  []SlaveMapping `json:"mappings"`
}

// SlaveIdentity 是从站对外暴露的设备标识（Modbus FC43 读设备标识 / FC17
// 报告服务器 ID）。字段全部可选：空值由协议层填默认值。
//
// 它不是「产品信息展示」这么简单：SCADA 往往用 VendorName + ProductCode
// 做设备类型识别与点表自动匹配，填错会让上位机把网关当成另一种设备。
type SlaveIdentity struct {
	VendorName          string `json:"vendorName,omitempty"`
	ProductCode         string `json:"productCode,omitempty"`
	MajorMinorRevision  string `json:"majorMinorRevision,omitempty"`
	VendorURL           string `json:"vendorUrl,omitempty"`
	ProductName         string `json:"productName,omitempty"`
	ModelName           string `json:"modelName,omitempty"`
	UserApplicationName string `json:"userApplicationName,omitempty"`
}

// SlaveSerial 是从站的串口监听端点（Modbus RTU / ASCII 从站）。
//
// 与 TCP 从站的差别不只是「换一种传输」：
//   - 一条串口线上可以挂多个 Unit ID，但同一时刻只服务一个请求；
//   - 帧没有长度前缀，必须按功能码推导长度；CRC/LRC 错了只能丢弃并等
//     下一个静默间隔重新同步；
//   - 同一个物理端口不能既做从站又做主站（半双工总线上两个角色会互相
//     抢答）。这条互斥由配置层跨通道检查。
type SlaveSerial struct {
	Port     string `json:"port"`
	Baud     int    `json:"baud,omitempty"`
	DataBits int    `json:"dataBits,omitempty"`
	Parity   string `json:"parity,omitempty"`
	StopBits int    `json:"stopBits,omitempty"`
	// Framing 选择串口上的帧格式：rtu | ascii。
	//
	// 不做自动探测：两种帧的校验方式与分帧规则完全不同，探测失败会把
	// 故障推迟到第一次采样，而且现场看到的只是「读不到数据」。
	Framing string `json:"framing"`
}

// Normalized 补齐串口默认值。只把零值当作「未设置」。
//
// 与 transport.SerialConfig.Normalized 保持一致（model 不依赖内部包，
// 因此这里是同一套规则的第二份表述；slave 包有交叉测试盯着两者）。
func (s SlaveSerial) Normalized() SlaveSerial {
	if s.Baud == 0 {
		s.Baud = 9600
	}
	if s.DataBits == 0 {
		s.DataBits = 8
	}
	if s.Parity == "" {
		s.Parity = "none"
	}
	if s.StopBits == 0 {
		s.StopBits = 1
	}
	return s
}

// Validate 校验串口参数。参数错了不会报错，只会「读不到东西」。
func (s SlaveSerial) Validate() error {
	if s.Port == "" {
		return errors.New("serial.port: required")
	}
	switch s.Framing {
	case "rtu", "ascii":
	default:
		return fmt.Errorf("serial.framing %q: must be rtu or ascii", s.Framing)
	}
	n := s.Normalized()
	if n.DataBits < 5 || n.DataBits > 8 {
		return fmt.Errorf("serial.dataBits %d: must be 5..8", n.DataBits)
	}
	switch n.Parity {
	case "none", "even", "odd":
	default:
		return fmt.Errorf("serial.parity %q: must be none|even|odd", n.Parity)
	}
	if n.StopBits != 1 && n.StopBits != 2 {
		return fmt.Errorf("serial.stopBits %d: must be 1 or 2", n.StopBits)
	}
	switch n.Baud {
	case 300, 600, 1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600:
	default:
		return fmt.Errorf("serial.baud %d: unsupported", n.Baud)
	}
	return nil
}

// Slave 是一个从站监听端点。
//
// 存在即启用：停用一个从站的方式是从配置里删掉它。额外的 enabled 字段
// 只会制造「配置了但没生效」的状态，而这正是本项目要避免的那一类问题。
//
// Listen 与 Serial 二选一：前者是 TCP 监听，后者是串口（RTU/ASCII）。
type Slave struct {
	ID             string         `json:"id"`
	Listen         string         `json:"listen,omitempty"`
	Serial         *SlaveSerial   `json:"serial,omitempty"`
	MaxConnections int            `json:"maxConnections,omitempty"`
	Identity       *SlaveIdentity `json:"identity,omitempty"`
	Units          []SlaveUnit    `json:"units"`
}

// Endpoint 返回用于日志与诊断的端点描述。
func (s Slave) Endpoint() string {
	if s.Serial != nil {
		n := s.Serial.Normalized()
		return fmt.Sprintf("%s %s", n.Port, n.Framing)
	}
	return s.Listen
}

// EffectiveMaxConnections 返回并发连接上限（默认 8，范围 1..64）。
func (s Slave) EffectiveMaxConnections() int {
	if s.MaxConnections <= 0 {
		return 8
	}
	if s.MaxConnections > 64 {
		return 64
	}
	return s.MaxConnections
}

// RegisterWidth 返回点位映射到 16 位寄存器区时占用的寄存器个数。
//
// 与 Length() 的差别：Length() 是驱动侧的读请求参数，缺省时由类型推导；
// 映射必须给出确定宽度，否则同一个地址可能被两个点位同时声明，而这种
// 冲突在运行期表现为「谁后写谁生效」，几乎不可能靠日志发现。
func (p Point) RegisterWidth() int {
	if p.DataType == TypeBool {
		return 1
	}
	if n := p.Length(); n > 0 {
		return n
	}
	return 1
}

// Validate 校验从站配置。points 的键是 "device/point"，由配置层构造：
// 只有拿到点位元数据，才能判断宽度、可写性与地址重叠。
func (s Slave) Validate(points map[string]Point) error {
	if !idRe.MatchString(s.ID) {
		return fmt.Errorf("slave.id %q: must match %s", s.ID, idRe.String())
	}
	// 监听端点必须恰好选一个：两个都给会让「实际监听在哪」取决于实现
	// 里 if 的先后，两个都不给则是一条永远起不来的配置。
	hasTCP, hasSerial := s.Listen != "", s.Serial != nil
	switch {
	case hasTCP && hasSerial:
		return fmt.Errorf("slave %q: listen and serial are mutually exclusive", s.ID)
	case !hasTCP && !hasSerial:
		return fmt.Errorf("slave %q: one of listen or serial required", s.ID)
	}
	if s.Serial != nil {
		if err := s.Serial.Validate(); err != nil {
			return fmt.Errorf("slave %q: %w", s.ID, err)
		}
		// 串口是单主站总线，连接数这个概念不成立。配了却不生效比报错
		// 更糟：运维会以为「设了 8 就能接 8 条链路」。
		if s.MaxConnections != 0 {
			return fmt.Errorf("slave %q: maxConnections is not applicable to serial endpoints", s.ID)
		}
	}
	if s.MaxConnections < 0 || s.MaxConnections > 64 {
		return fmt.Errorf("slave %q: maxConnections %d out of range 0..64", s.ID, s.MaxConnections)
	}
	if len(s.Units) == 0 {
		return fmt.Errorf("slave %q: at least one unit required", s.ID)
	}
	if s.Identity != nil {
		if err := s.Identity.Validate(); err != nil {
			return fmt.Errorf("slave %q: %w", s.ID, err)
		}
	}
	seen := make(map[int]bool, len(s.Units))
	for _, u := range s.Units {
		if err := u.Validate(points); err != nil {
			return fmt.Errorf("slave %q: %w", s.ID, err)
		}
		if seen[u.UnitID] {
			return fmt.Errorf("slave %q: duplicate unitId %d", s.ID, u.UnitID)
		}
		seen[u.UnitID] = true
	}
	return nil
}

// Validate 校验设备标识。
//
// 长度上限不是审美问题：FC43 的响应必须塞进 253 字节的 PDU，单个对象值
// 超过 120 字节时，哪怕只返回一个对象也可能放不下，主站只会看到异常码。
func (i SlaveIdentity) Validate() error {
	fields := []struct {
		name string
		val  string
		max  int
	}{
		{"identity.vendorName", i.VendorName, 120},
		{"identity.productCode", i.ProductCode, 120},
		{"identity.majorMinorRevision", i.MajorMinorRevision, 32},
		{"identity.vendorUrl", i.VendorURL, 120},
		{"identity.productName", i.ProductName, 120},
		{"identity.modelName", i.ModelName, 120},
		{"identity.userApplicationName", i.UserApplicationName, 120},
	}
	for _, f := range fields {
		if len(f.val) > f.max {
			return fmt.Errorf("%s: %d bytes exceeds limit %d", f.name, len(f.val), f.max)
		}
		if strings.IndexFunc(f.val, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return fmt.Errorf("%s: must not contain control characters", f.name)
		}
	}
	return nil
}

// Validate 校验单元映射：地址范围不能重叠，可写映射必须指向可写点位。
func (u SlaveUnit) Validate(points map[string]Point) error {
	// 0 是广播地址，不能作为单播目标；248..255 是保留值。
	if u.UnitID < 0 || u.UnitID > 247 {
		return fmt.Errorf("unitId %d out of range 0..247", u.UnitID)
	}
	if !u.ByteOrder.Valid() {
		return fmt.Errorf("unit %d: byteOrder %q invalid", u.UnitID, u.ByteOrder)
	}
	if len(u.Mappings) == 0 {
		return fmt.Errorf("unit %d: at least one mapping required", u.UnitID)
	}

	type span struct {
		start int
		end   int
		point string
	}
	spans := map[MappingArea][]span{}
	for i, m := range u.Mappings {
		dev, pt, ok := strings.Cut(m.Point, "/")
		if !ok || dev == "" || pt == "" {
			return fmt.Errorf("unit %d mapping[%d]: point %q must be device/point", u.UnitID, i, m.Point)
		}
		p, ok := points[m.Point]
		if !ok {
			return fmt.Errorf("unit %d mapping[%d]: point %q not found", u.UnitID, i, m.Point)
		}
		if !m.Area.Valid() {
			return fmt.Errorf("unit %d mapping[%d]: area %q must be holding|input|coil|discrete",
				u.UnitID, i, m.Area)
		}
		if m.Offset < 0 || m.Offset > 65535 {
			return fmt.Errorf("unit %d mapping[%d]: offset %d out of range 0..65535", u.UnitID, i, m.Offset)
		}
		if m.Writable {
			if !m.Area.AllowsWrite() {
				return fmt.Errorf("unit %d mapping[%d]: area %q is read-only", u.UnitID, i, m.Area)
			}
			if !p.Writable {
				return fmt.Errorf("unit %d mapping[%d]: point %q is not writable", u.UnitID, i, m.Point)
			}
		}
		width := 1
		if !m.Area.IsBits() {
			width = p.RegisterWidth()
		}
		if m.Offset+width > 65536 {
			return fmt.Errorf("unit %d mapping[%d]: %s@%d width %d overflows address space",
				u.UnitID, i, m.Area, m.Offset, width)
		}
		spans[m.Area] = append(spans[m.Area], span{m.Offset, m.Offset + width, m.Point})
	}

	for area, list := range spans {
		sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
		for i := 1; i < len(list); i++ {
			if list[i].start < list[i-1].end {
				return fmt.Errorf("unit %d: %s@%d (%s) overlaps %s@%d (%s)",
					u.UnitID, area, list[i].start, list[i].point,
					area, list[i-1].start, list[i-1].point)
			}
		}
	}
	return nil
}
