// Package mtconnect 是内置的 MTConnect Agent（测试 / 演示用）。
//
// 它实现的是驱动用到的那一小块 HTTP + XML 服务：`/probe`（设备模型）、
// `/current`（当前快照）、`/sample?from=&count=`（序号窗口）。刻意不做
// 资产（`/assets`）、interface、以及真实 Agent 的采样调度器——那些是独立
// 的子系统，而这个内置服务端只负责「让驱动能对着一个会变的 Agent 跑」。
//
// 数据模型刻意做成「一台三轴立式加工中心 + 控制器」：
//
//	VMC-3AXIS（uuid VMC-3AXIS-001）
//	  ├ Controller  controller：avail/exec/program/mode + 条件 alm
//	  ├ Linear      X / Y / Z：轴位置与负载（Xabs…Zload）
//	  ├ Rotary      S：主轴转速与负载（Sspeed/Sload）
//	  └ Systems     temp、toolOffset
//
// `toolOffset` 初始为 UNAVAILABLE：这是真实机床最常见的「数据项存在、但此刻
// 无值」场景，用来验证驱动把它折成坏质量而不是 0。
package mtconnect

import (
	"bytes"
	gz "compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	mtcodec "uic/internal/codec/mtconnect"
)

// 内置默认值。
const (
	// bufferCap 是 `/sample` 环形缓冲的容量。真实 Agent 的缓冲区按序号窗口
	// 管理，这里用一个有上限的切片模拟「旧观测被顶掉」。
	bufferCap = 256
	// defaultDeviceName / defaultDeviceUUID 是默认设备标识。
	defaultDeviceName = "VMC-3AXIS"
	defaultDeviceUUID = "VMC-3AXIS-001"
)

// Faults 是故障注入开关。零值即「一切正常」，每个字段独立，测试只打开自己
// 关心的那一个。
type Faults struct {
	// Status 覆盖 `/current` 的 HTTP 状态码（0 = 不覆盖）。
	Status int
	// FailProbe 让 `/probe` 返回 HTTP 500 + MTConnectError，用来验证驱动
	// 的「探测失败 → 整台设备不可用」路径。
	FailProbe bool
	// Malformed 让 `/current` 返回无法解析的 XML。
	Malformed bool
	// HTML 让 `/current` 返回一个 HTML 页面（模拟「地址指到了反向代理登录页」）。
	HTML bool
	// Delay 让每次 `/current` 前先睡这么久（0 = 不延迟），用来制造超时。
	Delay time.Duration
	// Omit 让 `/current` 省略一部分数据项（这里固定省略 X 轴位置），用来
	// 验证「点表里的 ID 在 Agent 侧不存在」被折成地址错误而不是链路错误。
	Omit bool
	// Unavailable 让全部数据项报告 UNAVAILABLE。
	Unavailable bool
	// Condition 给 alm 注入一条 FAULT 条件，消息取 ConditionText。
	Condition bool
	// ConditionText 是注入条件的消息文本。
	ConditionText string
}

// Options 是内置服务端的可调参数。
type Options struct {
	// Gzip 让 `/current` 以 Content-Encoding: gzip 返回，验证驱动的
	// 「Transport 自动解压」标准路径。
	Gzip bool
	// PathPrefix 是 Agent 挂载的子路径（如 "/mtconnect"），驱动基地址需要
	// 带同样的前缀。空表示挂在根。
	PathPrefix string
	// DeviceName / DeviceUUID 覆盖默认设备标识。
	DeviceName string
	DeviceUUID string
	// Faults 是故障注入开关。
	Faults Faults
}

// dataItemDef 是一个数据项的静态定义（来自 Agent 的模型）。
type dataItemDef struct {
	id          string
	component   string // XML 元素名，如 Linear / Controller / Systems
	name        string // ComponentStream 的 name 属性
	componentID string // ComponentStream 的 componentId 属性
	category    mtcodec.Category
	element     string // 观测元素名（POSITION / EXECUTION…，解析时其实不看）
	typeAttr    string // /probe 的 type 属性值
	units       string
	subType     string
}

// pointState 是一个数据项的当前可变状态。
type pointState struct {
	text  string                 // 普通观测的值文本
	level mtcodec.ConditionLevel // 条件观测的级别
	msg   string                 // 条件的消息文本
}

// snapshot 是环形缓冲里的一条：某个序号时刻的整份 XML。
type snapshot struct {
	seq uint64
	xml string
}

// Server 是内置 MTConnect Agent 服务端。
type Server struct {
	httpServer *http.Server
	addr       string
	basePrefix string

	defs  []dataItemDef
	order []string // 数据项 id 的展示顺序（按组件分组）

	mu       sync.Mutex
	points   map[string]*pointState
	seq      uint64
	buffer   []snapshot
	instance uint64

	faults Faults
	gzip   bool
}

// NewServer 在 addr（如 "127.0.0.1:0"）上监听 HTTP。
func NewServer(addr string) (*Server, error) {
	return NewServerWithOptions(addr, Options{})
}

// NewServerWithOptions 带参数启动服务端。
func NewServerWithOptions(addr string, opt Options) (*Server, error) {
	ua, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mtconnect sim: resolve %q: %w", addr, err)
	}
	ln, err := net.ListenTCP("tcp", ua)
	if err != nil {
		return nil, fmt.Errorf("mtconnect sim: listen %q: %w", addr, err)
	}
	s := &Server{
		addr:       ln.Addr().String(),
		basePrefix: normalizePrefix(opt.PathPrefix),
		defs:       defaultDefs(),
		points:     map[string]*pointState{},
		seq:        0,
		instance:   1,
		faults:     opt.Faults,
		gzip:       opt.Gzip,
	}
	s.order = make([]string, 0, len(s.defs))
	for _, d := range s.defs {
		s.points[d.id] = &pointState{}
		s.order = append(s.order, d.id)
	}
	s.loadDefaults()

	mux := http.NewServeMux()
	mux.HandleFunc(s.basePrefix+mtcodec.PathProbe, s.handleProbe)
	mux.HandleFunc(s.basePrefix+mtcodec.PathCurrent, s.handleCurrent)
	mux.HandleFunc(s.basePrefix+mtcodec.PathSample, s.handleSample)
	mux.HandleFunc(s.basePrefix+"/", s.handleRoot)
	s.httpServer = &http.Server{Handler: mux}
	go func() { _ = s.httpServer.Serve(ln) }()
	return s, nil
}

func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}

// defaultDefs 构建默认设备的数据项定义。
func defaultDefs() []dataItemDef {
	lin := func(comp, name, id, typ, sub, units string) dataItemDef {
		return dataItemDef{id: id, component: comp, name: name, componentID: name,
			category: mtcodec.CategorySample, element: capitalize(typ), typeAttr: typ,
			units: units, subType: sub}
	}
	evt := func(comp, name, id, typ string) dataItemDef {
		return dataItemDef{id: id, component: comp, name: name, componentID: name,
			category: mtcodec.CategoryEvent, element: capitalize(typ), typeAttr: typ}
	}
	return []dataItemDef{
		// 控制器事件 + 条件。
		evt("Controller", "controller", "avail", "AVAILABILITY"),
		evt("Controller", "controller", "exec", "EXECUTION"),
		evt("Controller", "controller", "program", "PROGRAM"),
		evt("Controller", "controller", "mode", "CONTROLLER_MODE"),
		{id: "alm", component: "Controller", name: "controller", componentID: "controller",
			category: mtcodec.CategoryCondition, element: "System", typeAttr: "SYSTEM"},
		// 三个直线轴：位置 + 负载。
		lin("Linear", "X", "Xabs", "POSITION", "ACTUAL", "MILLIMETER"),
		lin("Linear", "X", "Xload", "LOAD", "ACTUAL", "PERCENT"),
		lin("Linear", "Y", "Yabs", "POSITION", "ACTUAL", "MILLIMETER"),
		lin("Linear", "Y", "Yload", "LOAD", "ACTUAL", "PERCENT"),
		lin("Linear", "Z", "Zabs", "POSITION", "ACTUAL", "MILLIMETER"),
		lin("Linear", "Z", "Zload", "LOAD", "ACTUAL", "PERCENT"),
		// 主轴：转速 + 负载。
		lin("Rotary", "S", "Sspeed", "ROTARY_VELOCITY", "ACTUAL", "REVOLUTION/MINUTE"),
		lin("Rotary", "S", "Sload", "ROTARY_LOAD", "ACTUAL", "PERCENT"),
		// 系统：温度、刀具偏置。
		lin("Systems", "temperature", "temp", "TEMPERATURE", "", "CELSIUS"),
		lin("Systems", "tool", "toolOffset", "TOOL_OFFSET", "", "MILLIMETER"),
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// loadDefaults 设置初始观测值。
func (s *Server) loadDefaults() {
	set := func(id, text string) { s.points[id].text = text }
	set("avail", "AVAILABLE")
	set("exec", "ACTIVE")
	set("program", "O0001")
	set("mode", "AUTOMATIC")
	set("Xabs", "12.3456")
	set("Xload", "0.0")
	set("Yabs", "-3.2000")
	set("Yload", "12.5")
	set("Zabs", "0.0000")
	set("Zload", "0.0")
	set("Sspeed", "3500")
	set("Sload", "42.0")
	set("temp", "24.5")
	// toolOffset 初始即 UNAVAILABLE（真实机床：刀具未装 / 未对刀）。
	s.points["toolOffset"].text = mtcodec.Unavailable
	// 条件初始为 Normal。
	s.points["alm"].level = mtcodec.ConditionNormal
	s.points["alm"].msg = "all good"
}

// ---------------------------------------------------------------------------
// 对外控制（测试夹具）
// ---------------------------------------------------------------------------

// SetValue 改写某个普通数据项的文本值。ID 不存在返回 false。
func (s *Server) SetValue(id, text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.points[id]
	if !ok {
		return false
	}
	p.text = text
	return true
}

// SetUnavailable 让某个数据项报告 UNAVAILABLE。ID 不存在返回 false。
func (s *Server) SetUnavailable(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.points[id]
	if !ok {
		return false
	}
	p.text = mtcodec.Unavailable
	return true
}

// SetCondition 给某个条件数据项设置级别与消息。ID 不存在返回 false。
func (s *Server) SetCondition(id string, level mtcodec.ConditionLevel, msg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.points[id]
	if !ok {
		return false
	}
	p.level = level
	p.msg = msg
	return true
}

// Tick 推进一次观测：递增全局序号、渲染当前状态并压入环形缓冲。
//
// 真实 Agent 由设备侧的采样循环持续喂缓冲；这里把「喂一次」显式化成
// Tick，测试就能精确控制 `/sample?from=` 窗口里有哪些序号。
//
// 返回本 tick 分配到的序号。
func (s *Server) Tick() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	body := s.renderStreams(s.seq)
	s.pushSnapshot(s.seq, body)
	return s.seq
}

func (s *Server) pushSnapshot(seq uint64, body string) {
	s.buffer = append(s.buffer, snapshot{seq: seq, xml: body})
	if len(s.buffer) > bufferCap {
		s.buffer = s.buffer[len(s.buffer)-bufferCap:]
	}
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.addr }

// URL 返回 Agent 的基地址（含子路径前缀）。
func (s *Server) URL() string {
	return "http://" + s.addr + s.basePrefix
}

// Close 停止服务端。
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// Devices 返回当前设备模型（供测试断言）。
func (s *Server) Devices() *mtcodec.Devices {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, _ := mtcodec.ParseDevices([]byte(s.renderDevices()))
	return doc
}

// ---------------------------------------------------------------------------
// HTTP 处理
// ---------------------------------------------------------------------------

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// 挂在根路径上的其它请求返回 404 + 友好提示，模拟「地址指到了别处」。
	writeErrorDoc(w, http.StatusNotFound,
		"UNKNOWN_PATH", "resource not found; the agent serves /probe, /current, /sample")
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fail := s.faults.FailProbe
	body := s.renderDevices()
	s.mu.Unlock()
	if fail {
		writeErrorDoc(w, http.StatusInternalServerError, "INTERNAL_ERROR", "probe disabled by test fixture")
		return
	}
	writeXML(w, http.StatusOK, "MTConnectDevices", body)
}

func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	f := s.faults
	if f.Delay > 0 {
		s.mu.Unlock()
		time.Sleep(f.Delay)
		s.mu.Lock()
	}
	var body, ct string
	var status int
	switch {
	case f.Malformed:
		body, status, ct = "<MTConnectStreams><Header>", http.StatusOK, "application/xml"
	case f.HTML:
		body, status, ct = "<!doctype html><html><body>Agent login</body></html>", http.StatusOK, "text/html"
	default:
		s.seq++
		body = s.renderStreams(s.seq)
		s.pushSnapshot(s.seq, body)
		status, ct = http.StatusOK, "application/xml"
	}
	gzip := s.gzip
	s.mu.Unlock()
	writeResponse(w, status, ct, body, gzip)
}

func (s *Server) handleSample(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, _ := strconv.ParseUint(q.Get("from"), 10, 64)
	count, _ := strconv.Atoi(q.Get("count"))

	s.mu.Lock()
	body := s.renderSample(from, count)
	s.mu.Unlock()
	writeXML(w, http.StatusOK, "MTConnectStreams", body)
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

// header 渲染公共头。调用方须持锁。
func (s *Server) header() string {
	var first, last, next uint64
	if n := len(s.buffer); n > 0 {
		first = s.buffer[0].seq
		last = s.buffer[n-1].seq
		next = last + 1
	} else {
		first, last, next = s.seq, s.seq, s.seq+1
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`<Header version="2.0" sender="mtconnect-sim" instanceId="%d" `+
			`bufferSize="%d" assetBufferSize="0" assetCount="0" `+
			`creationTime="%s" firstSequence="%d" lastSequence="%d" nextSequence="%d"/>`,
		s.instance, bufferCap, ts, first, last, next)
}

// renderDevices 渲染 `/probe` 的 MTConnectDevices 文档。调用方须持锁。
func (s *Server) renderDevices() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MTConnectDevices>`)
	b.WriteString(s.header())
	b.WriteString(`<Devices>`)
	b.WriteString(fmt.Sprintf(`<Device id="%s" name="%s" uuid="%s">`,
		defaultDeviceUUID, defaultDeviceName, defaultDeviceUUID))
	b.WriteString(`<Description>内置三轴立式加工中心（模拟 Agent）</Description>`)
	b.WriteString(`<DataItems>`)
	b.WriteString(s.renderDataItemsByComponent(""))
	b.WriteString(`</DataItems>`)
	b.WriteString(`<Components>`)
	b.WriteString(s.renderComponents())
	b.WriteString(`</Components>`)
	b.WriteString(`</Device>`)
	b.WriteString(`</Devices></MTConnectDevices>`)
	return b.String()
}

// renderComponents 渲染设备下的组件树（扁平一层，足够覆盖驱动的解析路径）。
func (s *Server) renderComponents() string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, d := range s.defs {
		key := d.component + "|" + d.name
		if seen[key] {
			continue
		}
		seen[key] = true
		b.WriteString(fmt.Sprintf(`<%s id="%s" name="%s" type="%s">`,
			d.component, d.componentID, d.name, strings.ToLower(d.component)))
		b.WriteString(`<DataItems>`)
		b.WriteString(s.renderDataItemsByComponent(d.name))
		b.WriteString(`</DataItems>`)
		b.WriteString(fmt.Sprintf(`</%s>`, d.component))
	}
	return b.String()
}

// renderDataItemsByComponent 渲染某个组件（name 为空则渲染设备级）下的
// <DataItem> 定义。默认模型把所有数据项都挂在组件下，设备级为空。
func (s *Server) renderDataItemsByComponent(component string) string {
	var b strings.Builder
	for _, d := range s.defs {
		if d.name != component {
			continue
		}
		b.WriteString(fmt.Sprintf(`<DataItem category="%s" type="%s" id="%s"`,
			d.category, d.typeAttr, d.id))
		if d.units != "" {
			b.WriteString(fmt.Sprintf(` units="%s" nativeUnits="%s"`, d.units, d.units))
		}
		if d.subType != "" {
			b.WriteString(fmt.Sprintf(` subType="%s"`, d.subType))
		}
		b.WriteString(`/>`)
	}
	return b.String()
}

// renderStreams 渲染 `/current` 与 `/sample` 的 MTConnectStreams 文档。
//
// 调用方须持锁。omit 用于故障注入（省略 X 轴位置）。
func (s *Server) renderStreams(seq uint64) string {
	return s.renderStreamsWith(seq, s.faults.Omit)
}

func (s *Server) renderStreamsWith(seq uint64, omit bool) string {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MTConnectStreams>`)
	b.WriteString(s.header())
	b.WriteString(`<Streams>`)
	b.WriteString(fmt.Sprintf(`<DeviceStream name="%s" uuid="%s">`,
		defaultDeviceName, defaultDeviceUUID))

	seen := map[string]bool{}
	for _, d := range s.defs {
		key := d.component + "|" + d.name
		if seen[key] {
			continue
		}
		seen[key] = true
		b.WriteString(fmt.Sprintf(`<ComponentStream component="%s" name="%s" componentId="%s">`,
			d.component, d.name, d.componentID))
		b.WriteString(s.renderItemsForComponent(d.name, seq, ts, omit))
		b.WriteString(`</ComponentStream>`)
	}
	b.WriteString(`</DeviceStream></Streams></MTConnectStreams>`)
	return b.String()
}

// renderItemsForComponent 渲染一个组件下的观测分组（Samples/Events/Conditions）。
func (s *Server) renderItemsForComponent(component string, seq uint64, ts string, omit bool) string {
	samples, events, conds := "", "", ""
	for _, d := range s.defs {
		if d.name != component {
			continue
		}
		if omit && d.id == "Xabs" {
			continue
		}
		st := s.points[d.id]
		elem := fmt.Sprintf(`<%s dataItemId="%s" timestamp="%s" sequence="%d"`,
			d.element, d.id, ts, seq)
		switch d.category {
		case mtcodec.CategorySample:
			units := ""
			if d.units != "" {
				units = fmt.Sprintf(` units="%s"`, d.units)
			}
			sub := ""
			if d.subType != "" {
				sub = fmt.Sprintf(` subType="%s"`, d.subType)
			}
			samples += fmt.Sprintf(`<%s dataItemId="%s" timestamp="%s" sequence="%d"%s%s>%s</%s>`,
				d.element, d.id, ts, seq, sub, units, escapeXML(s.valueText(d, st)), d.element)
		case mtcodec.CategoryEvent:
			events += elem + fmt.Sprintf(`>%s</%s>`, escapeXML(st.text), d.element)
		case mtcodec.CategoryCondition:
			conds += s.renderCondition(d, st, ts, seq)
		}
	}
	var b strings.Builder
	if samples != "" {
		b.WriteString(`<Samples>` + samples + `</Samples>`)
	}
	if events != "" {
		b.WriteString(`<Events>` + events + `</Events>`)
	}
	if conds != "" {
		b.WriteString(`<Conditions>` + conds + `</Conditions>`)
	}
	return b.String()
}

// valueText 返回普通观测的值文本（故障注入 Unavailable 时全部变 UNAVAILABLE）。
func (s *Server) valueText(d dataItemDef, st *pointState) string {
	if s.faults.Unavailable {
		return mtcodec.Unavailable
	}
	if d.category == mtcodec.CategoryCondition {
		return ""
	}
	return st.text
}

// renderCondition 用规范的两层写法渲染条件：
//
//	<System dataItemId="alm" type="SYSTEM"><Fault ...>消息</Fault></System>
//
// 级别为 Normal 时内层用 <Normal>。faults.Condition 会把 alm 覆盖成 FAULT。
func (s *Server) renderCondition(d dataItemDef, st *pointState, ts string, seq uint64) string {
	level := st.level
	msg := st.msg
	if s.faults.Condition && d.id == "alm" {
		level = mtcodec.ConditionFault
		if s.faults.ConditionText != "" {
			msg = s.faults.ConditionText
		}
	}
	inner := capitalize(string(level))
	return fmt.Sprintf(`<%s dataItemId="%s" type="%s" timestamp="%s" sequence="%d">`+
		`<%s timestamp="%s" sequence="%d" nativeCode="">%s</%s></%s>`,
		d.element, d.id, d.typeAttr, ts, seq, inner, ts, seq, escapeXML(msg), inner, d.element)
}

// renderSample 渲染 `/sample` 窗口：从 from 起取至多 count 条缓冲观测，
// 每条用当时的序号包成一段 DeviceStream。
func (s *Server) renderSample(from uint64, count int) string {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MTConnectStreams>`)
	b.WriteString(s.header())
	b.WriteString(`<Streams>`)
	n := 0
	for _, snap := range s.buffer {
		if snap.seq < from {
			continue
		}
		if count > 0 && n >= count {
			break
		}
		// 缓冲里存的是整份文档；这里把每条观测拆成独立 DeviceStream 段，
		// 让 /sample 的语义（序号窗口）与 /current（整台当前）有区分。
		b.WriteString(s.renderSampleSegment(snap.seq, ts))
		n++
	}
	b.WriteString(`</Streams></MTConnectStreams>`)
	return b.String()
}

// renderSampleSegment 渲染单条序号观测为一段 DeviceStream（复用当前值）。
func (s *Server) renderSampleSegment(seq uint64, ts string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<DeviceStream name="%s" uuid="%s">`,
		defaultDeviceName, defaultDeviceUUID))
	seen := map[string]bool{}
	for _, d := range s.defs {
		key := d.component + "|" + d.name
		if seen[key] {
			continue
		}
		seen[key] = true
		b.WriteString(fmt.Sprintf(`<ComponentStream component="%s" name="%s" componentId="%s">`,
			d.component, d.name, d.componentID))
		b.WriteString(s.renderItemsForComponent(d.name, seq, ts, false))
		b.WriteString(`</ComponentStream>`)
	}
	b.WriteString(`</DeviceStream>`)
	return b.String()
}

// ---------------------------------------------------------------------------
// 响应写出
// ---------------------------------------------------------------------------

func writeXML(w http.ResponseWriter, status int, root, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-MTConnect-Version", "2.0")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeResponse(w http.ResponseWriter, status int, contentType, body string, gzip bool) {
	w.Header().Set("X-MTConnect-Version", "2.0")
	if !gzip {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	var buf bytes.Buffer
	zw := gz.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		zw.Close()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	_ = zw.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeErrorDoc(w http.ResponseWriter, status int, code, message string) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
		`<MTConnectError><Header version="2.0" sender="mtconnect-sim" instanceId="1" `+
		`creationTime="%s"/><Errors><Error errorCode="%s">%s</Error></Errors></MTConnectError>`,
		ts, code, escapeXML(message))
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-MTConnect-Version", "2.0")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func escapeXML(s string) string {
	repl := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	)
	return repl.Replace(s)
}

// orderIDs 返回全部数据项 id（按定义顺序）。供测试观察。
func (s *Server) orderIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}
