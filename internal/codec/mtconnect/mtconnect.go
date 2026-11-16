// Package mtconnect 是 MTConnect Agent 响应的纯函数解析层。
//
// MTConnect 不是「寄存器 + 批量读」协议，也不是「URI 资源 + 键值」协议：
// Agent 把整台机床的信息模型摊成两份 XML——`/probe` 是**模型**（Device →
// 组件树 → 数据项），`/current` 是**快照**（每条数据项当前的值、时间戳与
// 序号）。因此这一层只有三个入口：ParseStreams、ParseDevices、ParseError。
//
// 四个刻意的取舍：
//
//  1. **不校验版本。** 从 1.1 到 2.x，`DeviceStream / ComponentStream /
//     Samples|Events|Conditions` 的形状没有变过，变的只是可选属性。按本地名
//     匹配、忽略命名空间，让同一份解析器能读十年的 Agent 输出；反过来
//     「只认 1.7」会让升级 Agent 变成改网关代码。
//  2. **元素名就是数据项类型。** MTConnect 的数据项类型（POSITION、
//     EXECUTION、LOAD…）编码在元素名里而不是属性里，所以不能用固定结构体
//     枚举——用 `,any` 收下任意子元素，再按本地名判断这是采样、事件还是
//     条件级别。
//  3. **条件是两层结构，而且有两种写法。** 规范写法是
//     `<System><Fault dataItemId="alm">主轴过载</Fault></System>`：外层元素
//     是条件类型，内层元素才是级别。早期实现与部分 Agent 用平铺写法
//     （`<Fault type="SYSTEM">…</Fault>`）。两种都要认，否则一台老机床的
//     报警永远读不到。
//  4. **同一个 dataItemId 的多条条件要合并。** 一台机床可以同时有 3 条
//     Fault 和 1 条 Warning；对平台来说这个点位只有一个值，取最严重的级别
//     （FAULT > WARNING > UNAVAILABLE > NORMAL）。合并规则写在这里而不是
//     驱动里，是因为它是「MTConnect 语义」而不是「怎么发 HTTP」。
//
// 这一层不做任何 I/O、不读时钟、不持有可变状态；HTTP 与超时由驱动负责。
package mtconnect

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPort 是 MTConnect Agent 的注册端口。
	//
	// 取 5000 而不是 80：Agent 是设备侧的一个独立进程，规范与现场部署都
	// 以 5000 为默认；把它写成 80 会让人以为要经过 Web 服务器反向代理。
	DefaultPort = 5000

	// Unavailable 是 MTConnect 的「无值」哨兵。
	//
	// 它不是一个数值，也不是空字符串，而是一个保留字面量：数据项存在、
	// 但此刻无法确定值（探头未装、轴未回零、代理与设备断链）。必须与
	// 「读到 0」区分开，否则温度会显示成 0 ℃。
	Unavailable = "UNAVAILABLE"

	// PathProbe / PathCurrent / PathSample 是驱动用到的三个资源路径。
	PathProbe   = "/probe"
	PathCurrent = "/current"
	PathSample  = "/sample"
)

// Category 是 MTConnect 的观测类别。
type Category string

const (
	// CategorySample 是连续量（位置、转速、负载）。
	CategorySample Category = "SAMPLE"
	// CategoryEvent 是离散量（执行状态、程序名、可用性）。
	CategoryEvent Category = "EVENT"
	// CategoryCondition 是报警/状态条件（Normal/Warning/Fault）。
	CategoryCondition Category = "CONDITION"
	// CategoryUnknown 表示类别属性缺失或拼错。解析器不拒绝它：宁可把
	// 点位读成「类型未知」，也不要因为一个属性让整份快照解析失败。
	CategoryUnknown Category = ""
)

// ConditionLevel 是条件观测的级别。
type ConditionLevel string

const (
	ConditionNormal      ConditionLevel = "NORMAL"
	ConditionWarning     ConditionLevel = "WARNING"
	ConditionFault       ConditionLevel = "FAULT"
	ConditionUnavailable ConditionLevel = "UNAVAILABLE"
)

// Header 是 MTConnect 文档的公共头。
//
// 序号（sequence）是 MTConnect 的核心概念：Agent 为每条观测分配一个单调
// 递增的序号，`/sample?from=N&count=M` 靠它取一段历史。`/current` 只需要
// 读值，但把序号留下来，自检命令才能解释「为什么 /sample 从 100 开始」。
type Header struct {
	CreationTime          string
	Sender                string
	InstanceID            uint64
	Version               string
	DeviceModelChangeTime string
	TestIndicator         bool
	BufferSize            uint64
	AssetBufferSize       uint64
	AssetCount            uint64
	FirstSequence         uint64
	LastSequence          uint64
	NextSequence          uint64
}

// Item 是一条观测：一个数据项在一次快照里的值。
type Item struct {
	DataItemID       string
	Name             string
	SubType          string
	Units            string
	NativeUnits      string
	CoordinateSystem string
	NativeCode       string
	// Type 是 MTConnect 的数据项类型。采样/事件里它来自元素名
	// （`<Position>` → "Position"）；条件里它来自外层元素或 `type` 属性
	// （"System"、"TEMPERATURE"）。
	Type      string
	Category  Category
	Condition ConditionLevel

	// Timestamp 是设备侧的观测时刻；解析失败时为零值，原文留在
	// TimestampText 里。时间戳不是平台采样的时刻（那是网关收到数据的
	// 时刻），所以驱动不把它当成采集时间，只用于诊断。
	Timestamp     time.Time
	TimestampText string
	Sequence      uint64

	// Text 是元素正文。条件的两层写法里，外层元素的正文通常为空，
	// 真正的报警文字在内层级别元素上。
	Text string
}

// Value 返回该观测的「值文本」。
//
// 条件观测的值是**级别**而不是报警文字：平台侧一个点位只有一个值，级别
// 才能参与比较与报警（`== "FAULT"`），文字只用于人看。这也让同一个点位
// 在 Normal 与 Fault 之间切换时产生一次真正的值变化。
func (it Item) Value() string {
	if it.Category == CategoryCondition && it.Condition != "" {
		return string(it.Condition)
	}
	return strings.TrimSpace(it.Text)
}

// Unavailable 表示该观测此刻没有可用值。
//
// 条件观测的写法是级别为 UNAVAILABLE；其余观测是正文等于保留字。比较
// 时忽略大小写与首尾空白：规范要求大写，但现场代理的输出并不总是严格。
func (it Item) Unavailable() bool {
	if it.Category == CategoryCondition && it.Condition != "" {
		return it.Condition == ConditionUnavailable
	}
	return strings.EqualFold(strings.TrimSpace(it.Text), Unavailable)
}

// IsCondition 表示该观测是否带条件级别。
func (it Item) IsCondition() bool {
	return it.Category == CategoryCondition && it.Condition != ""
}

// LevelKnown 表示这条观测是不是「带合法级别的条件」。
//
// 它与 IsCondition 的差别只在 Category==CONDITION 但级别缺失时：Agent 可能
// 用了非标准的级别元素名（`<BogusLevel>OVERHEAT</BogusLevel>`），或外层只
// 写条件类型、内层一个级别元素都没有。这类观测**不能**当成采样/事件处理
// ——它的 Semantic 仍然是「报警」，只是我们读不出级别。
//
// 为什么需要这个区分：调用方若只看 `Condition == ""` 就回落成普通文本，会把
// 一条无法识别的告警当成正常数据上报（quality=good、value="OVERHEAT"）。
// 把「级别缺失」显式暴露出来，调用方才能把它归成坏质量。
func (it Item) LevelKnown() bool {
	return it.Category != CategoryCondition || it.Condition != ""
}

// DeviceStream 是一台设备在快照里的数据。
type DeviceStream struct {
	Name       string
	UUID       string
	Components []ComponentStream
}

// ComponentStream 是一个组件（轴、主轴、控制器）下的观测。
type ComponentStream struct {
	// Component 是组件类型（XML 元素名，如 Linear / Rotary / Controller）。
	Component string
	Name      string
	ID        string
	UUID      string
	Items     []Item
}

// Streams 是一份 `/current` 或 `/sample` 文档。
type Streams struct {
	Header  Header
	Devices []DeviceStream
}

// Items 摊平全部设备下的全部观测。
func (s *Streams) Items() []Item {
	if s == nil {
		return nil
	}
	n := 0
	for _, d := range s.Devices {
		for _, c := range d.Components {
			n += len(c.Items)
		}
	}
	out := make([]Item, 0, n)
	for _, d := range s.Devices {
		for _, c := range d.Components {
			out = append(out, c.Items...)
		}
	}
	return out
}

// Index 是按 dataItemId 建立的查找表。
type Index struct {
	byID       map[string]Item
	order      []string
	Duplicates int
}

// BuildIndex 建立查找表，并按下面的规则合并同一个 dataItemId 的多条观测：
//
//   - 两条都是条件：取更严重的级别（FAULT > WARNING > UNAVAILABLE > NORMAL），
//     级别相同时取序号更大的一条（更新的那条）；
//   - 一条是条件、一条不是：保留条件。同一 ID 既是采样又是条件只可能是
//     Agent 的模型错乱，条件至少能告诉运维「这里有报警」；
//   - 都不是条件：取序号更大的一条，序号相同则取文档里靠后的一条。
//
// 重复条数记在 Duplicates 里，供自检命令观察——它通常意味着 Agent 把同一个
// 数据项挂在了多个组件下，值得报给设备厂商。
func BuildIndex(items []Item) *Index {
	idx := &Index{byID: make(map[string]Item, len(items))}
	for _, it := range items {
		if it.DataItemID == "" {
			continue
		}
		prev, seen := idx.byID[it.DataItemID]
		if !seen {
			idx.byID[it.DataItemID] = it
			idx.order = append(idx.order, it.DataItemID)
			continue
		}
		idx.Duplicates++
		idx.byID[it.DataItemID] = mergeItems(prev, it)
	}
	sort.Strings(idx.order)
	return idx
}

// Index 是 Streams 的便捷入口。
func (s *Streams) Index() *Index { return BuildIndex(s.Items()) }

// Lookup 按 dataItemId 取一条观测。
func (i *Index) Lookup(id string) (Item, bool) {
	if i == nil {
		return Item{}, false
	}
	it, ok := i.byID[id]
	return it, ok
}

// Len 返回不同 dataItemId 的个数。
func (i *Index) Len() int {
	if i == nil {
		return 0
	}
	return len(i.byID)
}

// IDs 返回全部 dataItemId（已排序）。
func (i *Index) IDs() []string {
	if i == nil {
		return nil
	}
	return append([]string(nil), i.order...)
}

func mergeItems(prev, next Item) Item {
	// 判定「这是条件」用 Category，而不是 IsCondition()。
	//
	// IsCondition() 额外要求「已有可识别的级别」，级别缺失时为 false。若用它，
	// 一条级别读不出来的条件（`<System><BogusLevel>OVERHEAT</BogusLevel></System>`）
	// 会被当成普通观测，与同一 ID 的采样/事件比序号，从而可能被顶掉——一条
	// 报警就此消失。语义上它仍然是报警，只是严重度读不出来，必须保留参与合并。
	pc := prev.Category == CategoryCondition
	nc := next.Category == CategoryCondition
	switch {
	case pc && nc:
		if rp, rn := conditionRank(prev.Condition), conditionRank(next.Condition); rp != rn {
			if rn > rp {
				return next
			}
			return prev
		}
		if next.Sequence >= prev.Sequence {
			return next
		}
		return prev
	case pc:
		return prev
	case nc:
		return next
	}
	if next.Sequence >= prev.Sequence {
		return next
	}
	return prev
}

// conditionRank 给级别排序。UNAVAILABLE 排在 NORMAL 之上：它表示「不知道」，
// 比「正常」更需要人看一眼。
//
// 级别缺失（空串，或 Agent 写了不认识的名字）排在**最高**：这类观测的语义是
// 「有报警，但严重度读不出来」，绝不能因为排不出严重度就在合并时输给一条
// 明确 NORMAL 的记录——那会把 bad 静默变成 good，与驱动层
// conditionQuality「级别缺失一律判坏」自相矛盾。
func conditionRank(l ConditionLevel) int {
	switch l {
	case ConditionFault:
		return 3
	case ConditionWarning:
		return 2
	case ConditionUnavailable:
		return 1
	case ConditionNormal:
		return 0
	}
	// 未知级别：高于 FAULT，使其在合并中胜出（fail-closed）。
	return 4
}

// DataItem 是 `/probe` 里的一条数据项定义（只有模型，没有值）。
type DataItem struct {
	ID               string
	Name             string
	Type             string
	Category         Category
	SubType          string
	Units            string
	NativeUnits      string
	CoordinateSystem string
	// Component 是数据项所在的组件路径（如 "Axes/X"），用于自检输出与
	// 「这个 ID 到底属于哪个轴」这类问题。
	Component string
	Discrete  bool
}

// DeviceInfo 是 `/probe` 里的一台设备。
type DeviceInfo struct {
	ID           string
	Name         string
	UUID         string
	Description  string
	Manufacturer string
	Model        string
	SerialNumber string
	Station      string
	DataItems    []DataItem
}

// Devices 是一份 `/probe` 文档。
type Devices struct {
	Header  Header
	Devices []DeviceInfo
}

// ErrorDocument 是 Agent 返回的 MTConnectError 文档。
//
// 它是比 HTTP 状态码更有信息量的一层：同一个 500 可能是
// `OUT_OF_RANGE`（`from` 超出了缓冲区）、`UNSUPPORTED`（不支持某个参数）
// 或 `INTERNAL_ERROR`。驱动把它拼进错误文案，现场就不用抓包猜。
type ErrorDocument struct {
	Header   Header
	Code     string
	Messages []string
}

func (e *ErrorDocument) Error() string {
	if e == nil {
		return "MTConnect error"
	}
	msg := "MTConnect error"
	if e.Code != "" {
		msg += " " + e.Code
	}
	if len(e.Messages) > 0 {
		msg += ": " + strings.Join(e.Messages, "; ")
	}
	return msg
}

// ---------------------------------------------------------------------------
// /current 与 /sample
// ---------------------------------------------------------------------------

// ParseStreams 解析一份 `/current` 或 `/sample` 响应。
func ParseStreams(data []byte) (*Streams, error) {
	body, err := prepare(data)
	if err != nil {
		return nil, err
	}
	var doc xmlStreams
	if err := decodeXML(body, &doc); err != nil {
		return nil, err
	}
	switch doc.XMLName.Local {
	case "MTConnectStreams":
		return doc.convert(), nil
	case "MTConnectError":
		if e, ok := ParseError(body); ok {
			return nil, e
		}
		return nil, errors.New("agent returned an MTConnectError document")
	case "":
		return nil, errors.New("response body is not XML")
	default:
		return nil, fmt.Errorf("unexpected root element <%s> (want <MTConnectStreams>)", doc.XMLName.Local)
	}
}

type xmlStreams struct {
	XMLName xml.Name
	Header  xmlHeader    `xml:"Header"`
	Streams xmlStreamSet `xml:"Streams"`
}

type xmlStreamSet struct {
	Devices []xmlDeviceStream `xml:"DeviceStream"`
}

type xmlDeviceStream struct {
	Name       string               `xml:"name,attr"`
	UUID       string               `xml:"uuid,attr"`
	Components []xmlComponentStream `xml:"ComponentStream"`
}

type xmlComponentStream struct {
	Component  string   `xml:"component,attr"`
	Name       string   `xml:"name,attr"`
	ID         string   `xml:"componentId,attr"`
	UUID       string   `xml:"uuid,attr"`
	Samples    xmlGroup `xml:"Samples"`
	Events     xmlGroup `xml:"Events"`
	Conditions xmlGroup `xml:"Conditions"`
	// Condition 是 1.1 时代的容器名（单数）。留着它，老 Agent 的报警
	// 才不会静默丢光。
	Condition xmlGroup `xml:"Condition"`
}

// xmlGroup 收下任意名字的子元素。
//
// `,any` 是这里的关键：MTConnect 的数据项类型是无限集合（POSITION、
// EXECUTION、TOOL_OFFSET、…），而且厂商可以扩展自定义类型。用固定字段
// 枚举必然漏，漏掉的观测在平台侧就是「这个点永远没值」。
type xmlGroup struct {
	Any []xmlItem `xml:",any"`
}

type xmlItem struct {
	XMLName          xml.Name
	DataItemID       string       `xml:"dataItemId,attr"`
	Name             string       `xml:"name,attr"`
	TypeAttr         string       `xml:"type,attr"`
	SubType          string       `xml:"subType,attr"`
	Units            string       `xml:"units,attr"`
	NativeUnits      string       `xml:"nativeUnits,attr"`
	CoordinateSystem string       `xml:"coordinateSystem,attr"`
	NativeCode       string       `xml:"nativeCode,attr"`
	SampleRate       string       `xml:"sampleRate,attr"`
	TimestampText    string       `xml:"timestamp,attr"`
	Sequence         tolerantUint `xml:"sequence,attr"`
	Text             string       `xml:",chardata"`
	Children         []xmlItem    `xml:",any"`
}

func (doc *xmlStreams) convert() *Streams {
	out := &Streams{Header: doc.Header.convert()}
	for _, ds := range doc.Streams.Devices {
		dev := DeviceStream{Name: ds.Name, UUID: ds.UUID}
		for _, cs := range ds.Components {
			comp := ComponentStream{
				Component: cs.Component, Name: cs.Name, ID: cs.ID, UUID: cs.UUID,
			}
			comp.Items = append(comp.Items, cs.Samples.convert(CategorySample)...)
			comp.Items = append(comp.Items, cs.Events.convert(CategoryEvent)...)
			comp.Items = append(comp.Items, cs.Conditions.convert(CategoryCondition)...)
			comp.Items = append(comp.Items, cs.Condition.convert(CategoryCondition)...)
			dev.Components = append(dev.Components, comp)
		}
		out.Devices = append(out.Devices, dev)
	}
	return out
}

func (g xmlGroup) convert(cat Category) []Item {
	out := make([]Item, 0, len(g.Any))
	for _, x := range g.Any {
		if cat == CategoryCondition {
			out = append(out, x.convertCondition()...)
			continue
		}
		out = append(out, x.convertItem(cat))
	}
	return out
}

func (x xmlItem) convertItem(cat Category) Item {
	ts, ok := parseTimestamp(x.TimestampText)
	it := Item{
		DataItemID:       strings.TrimSpace(x.DataItemID),
		Name:             x.Name,
		SubType:          x.SubType,
		Units:            x.Units,
		NativeUnits:      x.NativeUnits,
		CoordinateSystem: x.CoordinateSystem,
		NativeCode:       x.NativeCode,
		Type:             x.XMLName.Local,
		Category:         cat,
		TimestampText:    x.TimestampText,
		Sequence:         uint64(x.Sequence),
		Text:             x.Text,
	}
	if ok {
		it.Timestamp = ts
	}
	if cat == CategoryCondition {
		// 平铺写法：元素名就是级别，`type` 属性是条件类型。
		if lv, isLevel := levelOf(x.XMLName.Local); isLevel {
			it.Condition = lv
			it.Type = x.TypeAttr
		} else if lv, isLevel := levelOf(x.TypeAttr); isLevel {
			it.Condition = lv
		}
	}
	return it
}

// convertCondition 处理条件的两层写法。
func (x xmlItem) convertCondition() []Item {
	// 外层元素的正文若是 UNAVAILABLE，说明 Agent 用平铺写法表达了
	// 「条件不可用」；此时没有内层级别元素。
	var levels []xmlItem
	for _, c := range x.Children {
		if _, isLevel := levelOf(c.XMLName.Local); isLevel {
			levels = append(levels, c)
		}
	}
	if len(levels) == 0 {
		return []Item{x.convertItem(CategoryCondition)}
	}
	out := make([]Item, 0, len(levels))
	for _, c := range levels {
		it := c.convertItem(CategoryCondition)
		lv, _ := levelOf(c.XMLName.Local)
		it.Condition = lv
		// 内层元素按规范只带 dataItemId/timestamp/sequence/nativeCode；
		// 但现场也有只在外层写 dataItemId 的 Agent，缺什么补什么。
		if it.DataItemID == "" {
			it.DataItemID = strings.TrimSpace(x.DataItemID)
		}
		if it.Type == "" || isLevelName(it.Type) {
			it.Type = x.XMLName.Local
		}
		if it.TimestampText == "" {
			it.TimestampText = x.TimestampText
			it.Timestamp = zeroIfUnset(x.TimestampText, it.Timestamp)
		}
		if it.Sequence == 0 {
			it.Sequence = uint64(x.Sequence)
		}
		if it.Text == "" {
			it.Text = x.Text
		}
		out = append(out, it)
	}
	return out
}

func zeroIfUnset(text string, fallback time.Time) time.Time {
	if t, ok := parseTimestamp(text); ok {
		return t
	}
	return fallback
}

// levelOf 把元素名或属性值识别成条件级别。
func levelOf(s string) (ConditionLevel, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "NORMAL":
		return ConditionNormal, true
	case "WARNING":
		return ConditionWarning, true
	case "FAULT":
		return ConditionFault, true
	case "UNAVAILABLE":
		return ConditionUnavailable, true
	}
	return "", false
}

func isLevelName(s string) bool {
	_, ok := levelOf(s)
	return ok
}

// ---------------------------------------------------------------------------
// /probe
// ---------------------------------------------------------------------------

// ParseDevices 解析一份 `/probe` 响应。
func ParseDevices(data []byte) (*Devices, error) {
	body, err := prepare(data)
	if err != nil {
		return nil, err
	}
	var doc xmlDevices
	if err := decodeXML(body, &doc); err != nil {
		return nil, err
	}
	switch doc.XMLName.Local {
	case "MTConnectDevices":
		return doc.convert(), nil
	case "MTConnectError":
		if e, ok := ParseError(body); ok {
			return nil, e
		}
		return nil, errors.New("agent returned an MTConnectError document")
	case "":
		return nil, errors.New("response body is not XML")
	default:
		return nil, fmt.Errorf("unexpected root element <%s> (want <MTConnectDevices>)", doc.XMLName.Local)
	}
}

type xmlDevices struct {
	XMLName xml.Name
	Header  xmlHeader  `xml:"Header"`
	Devices xmlDevList `xml:"Devices"`
}

type xmlDevList struct {
	Devices []xmlDevice `xml:"Device"`
}

type xmlDevice struct {
	ID           string        `xml:"id,attr"`
	Name         string        `xml:"name,attr"`
	UUID         string        `xml:"uuid,attr"`
	Description  string        `xml:"Description"`
	Manufacturer string        `xml:"Manufacturer"`
	Model        string        `xml:"Model"`
	SerialNumber string        `xml:"SerialNumber"`
	Station      string        `xml:"Station"`
	DataItems    xmlDataItems  `xml:"DataItems"`
	Components   xmlComponents `xml:"Components"`
}

type xmlComponents struct {
	Any []xmlComponent `xml:",any"`
}

type xmlComponent struct {
	XMLName     xml.Name
	ID          string        `xml:"id,attr"`
	Name        string        `xml:"name,attr"`
	Type        string        `xml:"type,attr"`
	Description string        `xml:"Description"`
	DataItems   xmlDataItems  `xml:"DataItems"`
	Components  xmlComponents `xml:"Components"`
}

type xmlDataItems struct {
	Items []xmlDataItem `xml:"DataItem"`
}

type xmlDataItem struct {
	ID               string `xml:"id,attr"`
	Name             string `xml:"name,attr"`
	Category         string `xml:"category,attr"`
	Type             string `xml:"type,attr"`
	SubType          string `xml:"subType,attr"`
	Units            string `xml:"units,attr"`
	NativeUnits      string `xml:"nativeUnits,attr"`
	CoordinateSystem string `xml:"coordinateSystem,attr"`
	Discrete         bool   `xml:"discrete,attr"`
}

func (doc *xmlDevices) convert() *Devices {
	out := &Devices{Header: doc.Header.convert()}
	for _, d := range doc.Devices.Devices {
		dev := DeviceInfo{
			ID: d.ID, Name: d.Name, UUID: d.UUID,
			Description:  strings.TrimSpace(d.Description),
			Manufacturer: strings.TrimSpace(d.Manufacturer),
			Model:        strings.TrimSpace(d.Model),
			SerialNumber: strings.TrimSpace(d.SerialNumber),
			Station:      strings.TrimSpace(d.Station),
		}
		// Device 自己的 DataItems 属于设备级组件（可用性、序列号…），
		// 组件路径写成设备名，与 ComponentStream 的 componentId 对齐。
		for _, di := range d.DataItems.Items {
			dev.DataItems = append(dev.DataItems, convertDataItem(di, d.Name))
		}
		for _, c := range d.Components.Any {
			walkComponent(c, nil, &dev)
		}
		out.Devices = append(out.Devices, dev)
	}
	return out
}

func walkComponent(c xmlComponent, path []string, dev *DeviceInfo) {
	label := strings.TrimSpace(c.Name)
	if label == "" {
		label = strings.TrimSpace(c.ID)
	}
	here := path
	if label != "" {
		here = append(append([]string(nil), path...), label)
	}
	for _, di := range c.DataItems.Items {
		dev.DataItems = append(dev.DataItems, convertDataItem(di, strings.Join(here, "/")))
	}
	for _, child := range c.Components.Any {
		walkComponent(child, here, dev)
	}
}

func convertDataItem(di xmlDataItem, component string) DataItem {
	return DataItem{
		ID:               strings.TrimSpace(di.ID),
		Name:             di.Name,
		Type:             di.Type,
		Category:         normalizeCategory(di.Category),
		SubType:          di.SubType,
		Units:            di.Units,
		NativeUnits:      di.NativeUnits,
		CoordinateSystem: di.CoordinateSystem,
		Component:        component,
		Discrete:         di.Discrete,
	}
}

func normalizeCategory(s string) Category {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SAMPLE":
		return CategorySample
	case "EVENT":
		return CategoryEvent
	case "CONDITION":
		return CategoryCondition
	}
	return CategoryUnknown
}

// ---------------------------------------------------------------------------
// /error
// ---------------------------------------------------------------------------

// ParseError 解析 MTConnectError 文档。第二个返回值表示「这确实是错误文档」。
//
// 它不是 Protocol 层的异常：Agent 在参数超出缓冲区、资源不支持、内部故障时
// 都会返回它，HTTP 状态码可能是 400 也可能是 500。驱动用它把错误文案补全。
func ParseError(data []byte) (*ErrorDocument, bool) {
	body, err := prepare(data)
	if err != nil {
		return nil, false
	}
	var doc xmlError
	if err := decodeXML(body, &doc); err != nil {
		return nil, false
	}
	if doc.XMLName.Local != "MTConnectError" {
		return nil, false
	}
	out := &ErrorDocument{Header: doc.Header.convert()}
	for _, e := range doc.Errors.Errors {
		if c := strings.TrimSpace(e.Code); c != "" && out.Code == "" {
			out.Code = c
		}
		if msg := strings.TrimSpace(e.Text); msg != "" {
			out.Messages = append(out.Messages, msg)
		}
	}
	return out, true
}

type xmlError struct {
	XMLName xml.Name
	Header  xmlHeader `xml:"Header"`
	Errors  struct {
		Errors []struct {
			Code string `xml:"errorCode,attr"`
			Text string `xml:",chardata"`
		} `xml:"Error"`
	} `xml:"Errors"`
}

// ---------------------------------------------------------------------------
// 公共头与工具
// ---------------------------------------------------------------------------

type xmlHeader struct {
	CreationTime          string       `xml:"creationTime,attr"`
	Sender                string       `xml:"sender,attr"`
	InstanceID            tolerantUint `xml:"instanceId,attr"`
	Version               string       `xml:"version,attr"`
	DeviceModelChangeTime string       `xml:"deviceModelChangeTime,attr"`
	TestIndicator         bool         `xml:"testIndicator,attr"`
	BufferSize            tolerantUint `xml:"bufferSize,attr"`
	AssetBufferSize       tolerantUint `xml:"assetBufferSize,attr"`
	AssetCount            tolerantUint `xml:"assetCount,attr"`
	FirstSequence         tolerantUint `xml:"firstSequence,attr"`
	LastSequence          tolerantUint `xml:"lastSequence,attr"`
	NextSequence          tolerantUint `xml:"nextSequence,attr"`
}

func (h xmlHeader) convert() Header {
	out := Header{
		CreationTime:          h.CreationTime,
		Sender:                h.Sender,
		InstanceID:            uint64(h.InstanceID),
		Version:               h.Version,
		DeviceModelChangeTime: h.DeviceModelChangeTime,
		TestIndicator:         h.TestIndicator,
		BufferSize:            uint64(h.BufferSize),
		AssetBufferSize:       uint64(h.AssetBufferSize),
		AssetCount:            uint64(h.AssetCount),
		FirstSequence:         uint64(h.FirstSequence),
		LastSequence:          uint64(h.LastSequence),
		NextSequence:          uint64(h.NextSequence),
	}
	// 老 Agent 不写 nextSequence：按「最后一条 + 1」补齐，让调用方不必
	// 到处判断零值。
	if out.NextSequence == 0 && out.LastSequence > 0 {
		out.NextSequence = out.LastSequence + 1
	}
	return out
}

func parseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// prepare 去掉 UTF-8 BOM 与首尾空白，并把「空响应」拦在这一层。
func prepare(data []byte) ([]byte, error) {
	body := bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("empty response body")
	}
	return body, nil
}

// decodeXML 用带字符集支持的解码器解析。
//
// 只用标准库：MTConnect 规范要求 UTF-8，但现场 Agent 的 XML 声明里偶尔
// 会出现 ISO-8859-1（老版本的工具链）。转码比直接失败更有价值——报警
// 文字里的一个重音符号不该让整台设备离线。
//
// 数值属性（InstanceID/Sequence/…）用 tolerantUint 承接：标准没有为它们定义
// 空值，但现场 Agent 确实会写出 `sequence=""` 或 `bufferSize="abc"`。若直接用
// `xml:"x,attr"` 到 uint64，一行坏属性会让整份 /current 解析失败，**全部**点位
// 每周期都变坏——一条诊断字段的噪声升级成整台设备离线。这里把「非数字」降级成
// 0（只影响诊断/合并排序，不影响任何采集值），与「单点问题不升级为链路错误」
// 同一原则。
func decodeXML(data []byte, v any) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = charsetReader
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse XML: %w", err)
	}
	return nil
}

// tolerantUint 是「尽力而为」的无符号整数属性：空值或非数字解析成 0，绝不
// 让属性错误中断整份文档。它实现 xml.UnmarshalerAttribute。
type tolerantUint uint64

func (u *tolerantUint) UnmarshalXMLAttr(attr xml.Attr) error {
	v := strings.TrimSpace(attr.Value)
	if v == "" {
		*u = 0
		return nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		*u = 0
		return nil
	}
	*u = tolerantUint(n)
	return nil
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "iso8859-1", "windows-1252", "cp1252":
		// windows-1252 与 latin-1 在 0x80..0x9F 有差异（引号、破折号）。
		// 按 latin-1 处理会让那一小段字符显示成控制字符，但不会丢数据，
		// 也不会让解析失败——比为了几十个字符拒绝整份文档好。
		return &latin1Reader{r: bufio.NewReader(input)}, nil
	}
	return nil, fmt.Errorf("unsupported XML charset %q (MTConnect requires UTF-8)", charset)
}

// latin1Reader 把单字节字符流转成 UTF-8。
type latin1Reader struct {
	r *bufio.Reader
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		b, err := l.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b < 0x80 {
			p[n] = b
			n++
			continue
		}
		if n+2 > len(p) {
			// 放不下两个字节：把这个字节留到下一次读。
			_ = l.r.UnreadByte()
			return n, nil
		}
		p[n] = 0xC0 | b>>6
		p[n+1] = 0x80 | b&0x3F
		n += 2
	}
	return n, nil
}
