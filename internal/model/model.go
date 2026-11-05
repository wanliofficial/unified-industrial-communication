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

	// object
	ObjectType string `json:"objectType,omitempty"`
	Instance   int    `json:"instance,omitempty"`
	Property   string `json:"property,omitempty"`
	PropIndex  int    `json:"propIndex,omitempty"`

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
		if a.Path != "" || a.ObjectType != "" || a.FileID != 0 || a.Selector != "" || a.Expr != "" {
			return errors.New("address: mixed fields for register kind")
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
		if a.Selector != "" && !selectorRe.MatchString(a.Selector) {
			return errors.New("address.selector: must not contain spaces/control chars")
		}
		if a.Area != "" || a.ObjectType != "" || a.FileID != 0 || a.Expr != "" {
			return errors.New("address: mixed fields for symbol kind")
		}
	case AddressObject:
		if a.ObjectType == "" || a.Property == "" {
			return errors.New("address.objectType/property: required for object")
		}
		if a.Instance < 0 {
			return errors.New("address.instance: must be >= 0")
		}
		if a.Area != "" || a.Path != "" || a.FileID != 0 || a.Selector != "" || a.Expr != "" {
			return errors.New("address: mixed fields for object kind")
		}
	case AddressFile:
		if a.FileID < 0 || a.RecordID < 0 || a.Offset < 0 {
			return errors.New("address.file: fileId/recordId/offset must be >= 0")
		}
		if a.Area != "" || a.Path != "" || a.ObjectType != "" || a.Selector != "" || a.Expr != "" {
			return errors.New("address: mixed fields for file kind")
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
		if a.Area != "" || a.Path != "" || a.ObjectType != "" || a.FileID != 0 ||
			a.Selector != "" || a.Offset != 0 || a.Length != 0 || a.Bit != 0 || a.DB != 0 ||
			a.Index != 0 || a.Instance != 0 || a.PropIndex != 0 || a.RecordID != 0 {
			return errors.New("address: mixed fields for computed kind")
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
		if a.Selector != "" {
			// 用 `#` 分隔：主题里不会出现 `#`（MQTT 通配符在点位的
			// path 侧被禁止），因此不会与合法主题产生歧义。
			return base + "#" + a.Selector
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
		return strconv.FormatInt(int64(v.Num), 10)
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
	if p.Address.Kind == AddressRegister && p.Address.Bit > 0 && p.DataType != TypeBool {
		return fmt.Errorf("point %q: address.bit > 0 requires bool dataType", p.ID)
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

// Slave 是一个从站监听端点。
//
// 存在即启用：停用一个从站的方式是从配置里删掉它。额外的 enabled 字段
// 只会制造「配置了但没生效」的状态，而这正是本项目要避免的那一类问题。
type Slave struct {
	ID             string      `json:"id"`
	Listen         string      `json:"listen"`
	MaxConnections int         `json:"maxConnections,omitempty"`
	Units          []SlaveUnit `json:"units"`
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
	if s.Listen == "" {
		return fmt.Errorf("slave %q: listen required", s.ID)
	}
	if s.MaxConnections < 0 || s.MaxConnections > 64 {
		return fmt.Errorf("slave %q: maxConnections %d out of range 0..64", s.ID, s.MaxConnections)
	}
	if len(s.Units) == 0 {
		return fmt.Errorf("slave %q: at least one unit required", s.ID)
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
