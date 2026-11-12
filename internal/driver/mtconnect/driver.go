// Package mtconnect 实现 MTConnect Agent 的 HTTP + XML 主站驱动。
//
// 与仓库里其它驱动最大的差别：**一次采集只有一条 HTTP 请求**。MTConnect 的
// `/current` 返回整台设备当前所有数据项的快照，所以「批量读」在这里不是
// 「把点位合并成若干段」，而是「把点表里的 dataItemId 拿去查一份文档」。
// 这也是 Meta 里 MultiRead 声明为 supported 的含义：点位多少都不影响请求数。
//
// 五个刻意的边界：
//
//  1. **只读。** 标准没有写数据项的服务：MTConnect 是信息模型，控制走的是
//     另一套接口（interface / 厂商私有）。因此 Write 显式返回 unsupported，
//     而不是「发一个没人实现的 PUT」。
//  2. **不做订阅。** `/sample` 是拉取历史序号窗口，不是推送；把轮询包装成
//     Subscribe 会让上层误以为有变化通知。能力位诚实标 unsupported。
//  3. **不做资产（`/assets`）与 interface。** 刀具、工件、夹具资产是独立
//     的文档族，语义与「读一个值」不同；半成品比不做更难排查。
//  4. **单点问题不升级为链路错误。** `/current` 一条请求覆盖全设备，若把
//     「某个点 UNAVAILABLE」「点表里的 ID 不存在」当成 Read 的错误返回，
//     上层会按错误分类丢掉整条 HTTP 连接——一个本来就不可用的点会让整台
//     设备每周期重连一次。因此这些只写进该点的 Err 与质量位，Read 本身
//     只在「文档拿不到 / 解析不了 / 设备选不出来」时返回错误。
//  5. **时间戳不冒充采集时刻。** MTConnect 的时间戳来自设备侧，机床控制器
//     的时钟经常不准（甚至没接 NTP）。平台用它只会得到「采集时刻倒退」的
//     假象，所以只把它当诊断信息。
package mtconnect

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	mtcodec "uic/internal/codec/mtconnect"
	"uic/internal/codec/textval"
	"uic/internal/driver"
	"uic/internal/model"
)

// 内置默认值与上限。
const (
	defaultTimeout = 3 * time.Second
	minTimeout     = 200 * time.Millisecond
	maxTimeout     = 30 * time.Second

	// maxDocumentBytes 是一份 XML 文档的读取上限。
	//
	// 一台五轴机床的 `/current` 通常几十 KB，大型柔性线（几百个数据项）
	// 也就几 MB。32 MiB 足以覆盖真实设备，同时把「Agent 把资产文档也塞进
	// 来」或「对端返回了一个无限流」挡在内存之外。
	maxDocumentBytes = 32 << 20

	// maxSampleCount 是 `/sample?count=` 的上限。自检命令用它翻页，一次
	// 几千条足够；再大应当用 `/current`。
	maxSampleCount = 10000
)

// Driver 是 MTConnect 主站驱动。
type Driver struct{}

// New 返回驱动。
func New() *Driver { return &Driver{} }

// Meta 声明 MTConnect 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "mtconnect",
		Name:         "MTConnect",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read: driver.CapSupported,
			// 标准没有写数据项的服务。声明 supported 再逐次返回错误，
			// 会让配置界面把点位标成「可写」，现场点了才失败。
			Write: driver.CapUnsupported,
			// `/sample` 是按序号拉历史，不是推送。轮询不能伪装成订阅。
			Subscribe: driver.CapUnsupported,
			// 模型浏览（`/probe`）是协议支持的一次性枚举，但它不是采集
			// 路径：能力位按「调度式采集能不能用」来标，自检命令走包级
			// 的 ProbeDevices，不经过能力位（与 IEC 61850 的 Browse 同例）。
			Browse:    driver.CapUnsupported,
			Discovery: driver.CapUnsupported,
			// 一条 `/current` 覆盖全部点位，点位多少不影响请求数。
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "Agent 地址", Type: "string", Required: true,
				Placeholder: "192.168.1.20:5000", Scope: "channel",
				Help: "MTConnect Agent 的基地址；可写成 host:port、http://host:port 或 https://host:port，省略端口按 5000；地址里可以带子路径（如 http://host:5000/mtconnect）"},
			{Name: "labels.deviceUuid", Label: "设备 UUID", Type: "string", Scope: "device",
				Placeholder: "VMC-3AXIS-001",
				Help:        "多设备 Agent 上选择 DeviceStream 的 uuid；留空且只有一台设备时自动选中，多台时报错"},
			{Name: "labels.deviceName", Label: "设备名", Type: "string", Scope: "device",
				Placeholder: "VMC-3AXIS",
				Help:        "按 DeviceStream 的 name 选择设备（deviceUuid 优先）；两者都留空时要求只有一台设备"},
			{Name: "labels.timeout", Label: "请求超时", Type: "duration", Scope: "device",
				Default: "3s",
				Help:    "单次 HTTP 请求（含读完响应体）的超时；必须 ≤ device.timeout"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.kind", Label: "寻址方式", Type: "enum", Required: true,
				Enum: []string{"symbol"}, Scope: "point"},
			{Name: "address.path", Label: "数据项 ID", Type: "string", Required: true,
				Scope: "point", Placeholder: "Xabs",
				Help: "`/probe` 返回的 dataItemId；它是 Agent 侧的稳定标识，不是显示名（name）"},
			{Name: "dataType", Label: "数据类型", Type: "enum", Required: true,
				Enum: []string{"bool", "int16", "uint16", "int32", "uint32", "int64",
					"uint64", "float32", "float64", "string", "bytes"},
				Scope: "point",
				Help:  "MTConnect 的线上值是文本，由这里声明怎么解释；UNAVAILABLE 会变成 bad 质量而不是 0"},
		},
		Description: "MTConnect Agent 主站：一条 GET /current 读整台设备的数据项（HTTP + XML，默认端口 5000）",
	}
}

// settings 是设备级可调参数。
type settings struct {
	deviceUUID string
	deviceName string
	timeout    time.Duration
}

func parseSettings(cfg driver.OpenConfig) (settings, error) {
	st := settings{
		deviceUUID: strings.TrimSpace(cfg.Device.Labels["deviceUuid"]),
		deviceName: strings.TrimSpace(cfg.Device.Labels["deviceName"]),
		timeout:    cfg.Device.TimeoutOrDefault(),
	}
	if v := strings.TrimSpace(cfg.Device.Labels["timeout"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return st, fmt.Errorf("labels.timeout %q: %w", v, err)
		}
		if d < minTimeout || d > maxTimeout {
			return st, fmt.Errorf("labels.timeout %s must be within %s..%s", v, minTimeout, maxTimeout)
		}
		if d > cfg.Device.TimeoutOrDefault() {
			return st, fmt.Errorf("labels.timeout %s exceeds device.timeout %s; "+
				"raise device.timeout or lower timeout", d, cfg.Device.TimeoutOrDefault())
		}
		st.timeout = d
	}
	return st, nil
}

// pointPlan 是一个点位解析后的数据项寻址。
type pointPlan struct {
	ref        driver.PointRef
	dataItemID string
}

func buildPlans(d model.Device) (map[string]pointPlan, error) {
	plans := make(map[string]pointPlan, len(d.Points))
	for _, p := range d.Points {
		if !p.Enabled {
			continue
		}
		plan, err := planPoint(driver.PointRefOf(d, p))
		if err != nil {
			return nil, err
		}
		if prev, dup := plans[plan.ref.ID]; dup {
			return nil, fmt.Errorf("point %q: duplicate id (also used by data item %q)", plan.ref.ID, prev.dataItemID)
		}
		plans[plan.ref.ID] = plan
	}
	return plans, nil
}

func planPoint(ref driver.PointRef) (pointPlan, error) {
	if ref.Address.Kind != model.AddressSymbol {
		return pointPlan{}, fmt.Errorf("point %q: mtconnect requires symbol addressing (a dataItemId), got %q",
			ref.ID, ref.Address.Kind)
	}
	id := strings.TrimSpace(ref.Address.Path)
	if id == "" {
		return pointPlan{}, fmt.Errorf("point %q: address.path required (dataItemId from /probe)", ref.ID)
	}
	// dataItemId 是 XML 属性值，允许字母、数字、下划线、连字符与点；
	// 空白与斜杠不会出现在合法 ID 里，出现了几乎一定是把别的协议的
	// 地址（路径 / 符号名）抄了过来。
	if strings.ContainsAny(id, " \t\r\n/?#") {
		return pointPlan{}, fmt.Errorf("point %q: address.path %q is not a dataItemId "+
			"(it must not contain spaces, '/', '?' or '#')", ref.ID, ref.Address.Path)
	}
	if ref.Address.Selector != "" {
		return pointPlan{}, fmt.Errorf("point %q: address.selector %q has no meaning in mtconnect "+
			"(the value is the text content of the data item)", ref.ID, ref.Address.Selector)
	}
	if ref.Address.Index > 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.index %d has no meaning in mtconnect "+
			"(array-like data items have their own ids)", ref.ID, ref.Address.Index)
	}
	if ref.Address.Length > 0 {
		return pointPlan{}, fmt.Errorf("point %q: address.length %d: the value width comes from the data item's text",
			ref.ID, ref.Address.Length)
	}
	if ref.DataType == "" || !ref.DataType.Valid() {
		return pointPlan{}, fmt.Errorf("point %q: invalid dataType %q", ref.ID, ref.DataType)
	}
	return pointPlan{ref: ref, dataItemID: id}, nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

type conn struct {
	devID  string
	base   *url.URL
	client *http.Client
	st     settings
	log    *slog.Logger

	mu     sync.Mutex
	closed bool

	plans map[string]pointPlan

	// version 是最近一次响应里的 MTConnect 版本（优先取响应头，其次取
	// XML 头）。自检命令用它回答「对端到底是哪一版」。
	version string
}

// Open 解析点位与基地址，建立 HTTP 客户端。
//
// 不在 Open 里发请求：Agent 可能很慢，而 Open 只有一次机会，失败后没有
// 重试语义。真正的连通性检查交给 Probe。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("mtconnect requires tcp transport (HTTP), got %q", t.Kind), false, nil)
	}
	base, err := ParseBaseURL(t.Address)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, "invalid transport address", false, err)
	}
	st, err := parseSettings(cfg)
	if err != nil {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID, "invalid device settings", false, err)
	}
	plans, err := buildPlans(cfg.Device)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "open", cfg.Device.ID, "invalid point table", false, err)
	}

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   st.timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   st.timeout,
		ExpectContinueTimeout: 1 * time.Second,
		// 自己不做压缩协商：由 Transport 自动加 `Accept-Encoding: gzip`
		// 并透明解压，这是标准路径。服务端「声明了没压缩、实际给了 gzip」
		// 的畸形情况由 readBody 兜底。
		DisableCompression: false,
	}
	return &conn{
		devID:  cfg.Device.ID,
		base:   base,
		client: &http.Client{Transport: tr, Timeout: st.timeout},
		st:     st,
		log:    cfg.Log,
		plans:  plans,
	}, nil
}

// ParseBaseURL 规范化 Agent 基地址。
//
// 接受三种写法：`host:port`、`http://host:port`、`https://host:port`。
// 省略端口时按 5000（MTConnect Agent 的注册端口），而不是按 scheme 推
// 80/443：Agent 是设备侧的独立进程，现场几乎总是显式跑在 5000 上。
// 地址可以带子路径（反向代理把 Agent 挂在 `/mtconnect` 下），但不能再带
// 查询串或片段——那会与驱动自己拼的 `?from=` 冲突。
func ParseBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("address is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in %q", raw)
	}
	if u.User != nil {
		// URL 里的凭据会进日志与错误文案；MTConnect 也没有标准的
		// HTTP 认证方式，写在这里多半是从别处抄来的。
		return nil, errors.New("credentials in the URL are not supported")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("address must not contain a query or fragment")
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		host := u.Hostname()
		port := strconv.Itoa(mtcodec.DefaultPort)
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]:" + port
		} else {
			u.Host = host + ":" + port
		}
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.client.CloseIdleConnections()
	return nil
}

// resourceURL 拼接基地址与资源路径。
func (c *conn) resourceURL(path string, q url.Values) string {
	u := *c.base
	u.Path = c.base.Path + path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// httpResult 是一次 HTTP 往返的原始结果。非 2xx 不在这里判错：Probe 需要
// 看 404 才能决定是否退回 `/current`，Read 才把非 2xx 转成分类错误。
type httpResult struct {
	status int
	header http.Header
	body   []byte
}

func (c *conn) get(ctx context.Context, path string, q url.Values, op string) (*httpResult, error) {
	target := c.resourceURL(path, q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, op, c.devID, "build request failed", false, err)
	}
	req.Header.Set("Accept", "application/xml, text/xml;q=0.9")
	req.Header.Set("User-Agent", "uic-mtconnect/"+driver.SDKVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, op, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readBody(resp, maxDocumentBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return nil, driver.E(driver.ErrProtocol, op, c.devID,
				fmt.Sprintf("response exceeds %d bytes; is transport.address pointing at an asset or log endpoint?", maxDocumentBytes),
				false, nil)
		}
		if ctx.Err() != nil {
			return nil, driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
		}
		if isTimeout(err) {
			return nil, driver.E(driver.ErrTimeout, op, c.devID,
				"timeout while reading the response body", true, err)
		}
		return nil, driver.E(driver.ErrDisconnected, op, c.devID,
			"read of the response body failed", true, err)
	}

	if v := strings.TrimSpace(resp.Header.Get("X-MTConnect-Version")); v != "" {
		c.version = v
	}
	if hint := htmlHint(resp.Header, body); hint != "" {
		return nil, driver.E(driver.ErrProtocol, op, c.devID,
			fmt.Sprintf("agent answered with HTML instead of MTConnect XML (%s); "+
				"check that transport.address points at the agent, not at a login page or reverse-proxy error", hint),
			false, nil)
	}
	return &httpResult{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// transportError 把 net/http 的错误归入驱动错误分类。
func (c *conn) transportError(ctx context.Context, op, target string, err error) error {
	if ctx.Err() != nil {
		return driver.E(driver.ErrTimeout, op, c.devID, "context done", true, ctx.Err())
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.devID,
			fmt.Sprintf("no response from %s within %s", c.base.Host, c.st.timeout), true, err)
	}
	// 连接被拒/重置/DNS 失败：都是「这条链路现在不通」，可重试。
	return driver.E(driver.ErrDisconnected, op, c.devID,
		fmt.Sprintf("request to %s failed", target), true, err)
}

var errBodyTooLarge = errors.New("response body too large")

// readBody 读完响应体，处理上限、gzip 兜底与「声明与实际不符」。
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	lr := io.LimitReader(resp.Body, limit+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	// 标准路径下 Transport 已经解压并清掉了 Content-Encoding。这里兜底
	// 处理「服务端无视 Accept-Encoding、直接给了 gzip」的畸形响应：
	// 按魔数判断，比按头判断可靠。
	if len(body) >= 2 && body[0] == 0x1F && body[1] == 0x8B {
		zr, zerr := gzip.NewReader(bytes.NewReader(body))
		if zerr != nil {
			return nil, fmt.Errorf("response looks gzipped but cannot be decompressed: %w", zerr)
		}
		defer func() { _ = zr.Close() }()
		plain, zerr := io.ReadAll(io.LimitReader(zr, limit+1))
		if zerr != nil {
			return nil, fmt.Errorf("decompress gzip response: %w", zerr)
		}
		if int64(len(plain)) > limit {
			return nil, errBodyTooLarge
		}
		body = plain
	}
	return body, nil
}

// htmlHint 识别「对端返回了一个网页」。
//
// 这是现场最常见的错配：把反向代理的登录页、路由器管理页、或者
// `http://host/` 的欢迎页当成 Agent 地址。XML 解析会报一行语法错，但那
// 句话对运维没有指向性；这里直接说明「你连的不是 Agent」。
func htmlHint(h http.Header, body []byte) string {
	ct := strings.TrimSpace(h.Get("Content-Type"))
	if ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			if mt == "text/html" || mt == "application/xhtml+xml" {
				return "Content-Type: " + mt
			}
		}
	}
	head := bytes.TrimSpace(body)
	if len(head) > 0 && head[0] == '<' {
		lower := bytes.ToLower(head)
		if bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) {
			return "body starts with <html>"
		}
	}
	return ""
}

// statusError 把非 2xx 响应翻译成分类错误。
func (c *conn) statusError(op string, r *httpResult) error {
	kind := driver.ErrProtocol
	temporary := false
	switch {
	case r.status == http.StatusUnauthorized, r.status == http.StatusForbidden:
		kind = driver.ErrPermission
	case r.status == http.StatusNotFound, r.status == http.StatusGone:
		kind = driver.ErrAddress
	case r.status == http.StatusMethodNotAllowed, r.status == http.StatusNotAcceptable:
		kind = driver.ErrUnsupported
	case r.status == http.StatusRequestTimeout, r.status == http.StatusGatewayTimeout:
		kind, temporary = driver.ErrTimeout, true
	case r.status == http.StatusTooManyRequests:
		// Agent 忙/被限流：值得退避重试。
		kind, temporary = driver.ErrProtocol, true
	case r.status >= 500:
		kind, temporary = driver.ErrProtocol, true
	}
	msg := fmt.Sprintf("agent answered HTTP %d %s", r.status, http.StatusText(r.status))
	if doc, ok := mtcodec.ParseError(r.body); ok {
		msg += ": " + doc.Error()
	}
	if r.status == http.StatusNotFound {
		msg += " (check that transport.address points at the agent root, e.g. http://host:5000)"
	}
	return driver.E(kind, op, c.devID, msg, temporary, nil)
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

// Probe 访问 `/probe`。
//
// `/probe` 是规范要求必须实现的资源，返回的是设备模型（几百字节到几 MB），
// 正好用来区分「TCP 通、Agent 在跑」与「端口上其实是别的服务」。
//
// 少数裁剪版 Agent 只实现 `/current`：遇到 404/405 时退回 `/current` 再试
// 一次。退一步的依据是——我们要采的就是 `/current`，它能通就说明设备可用，
// 没有理由因为一个可选的诊断资源把整台设备判成离线。
func (c *conn) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", true, nil)
	}
	r, err := c.get(ctx, mtcodec.PathProbe, nil, "probe")
	if err != nil {
		return err
	}
	if r.status >= 200 && r.status < 300 {
		return nil
	}
	if r.status == http.StatusNotFound || r.status == http.StatusMethodNotAllowed {
		r2, err := c.get(ctx, mtcodec.PathCurrent, nil, "probe")
		if err != nil {
			return err
		}
		if r2.status >= 200 && r2.status < 300 {
			return nil
		}
		return c.statusError("probe", r2)
	}
	return c.statusError("probe", r)
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, driver.E(driver.ErrDisconnected, "read", c.devID, "connection closed", true, nil)
	}
	refs := flattenRefs(spans)
	if len(refs) == 0 {
		return nil, nil
	}

	res, err := c.get(ctx, mtcodec.PathCurrent, nil, "read")
	if err != nil {
		return allFailed(refs, err), err
	}
	if res.status < 200 || res.status >= 300 {
		e := c.statusError("read", res)
		return allFailed(refs, e), e
	}
	doc, perr := mtcodec.ParseStreams(res.body)
	if perr != nil {
		e := driver.E(driver.ErrProtocol, "read", c.devID, "malformed /current response", false, perr)
		return allFailed(refs, e), e
	}
	if doc.Header.Version != "" {
		c.version = doc.Header.Version
	}

	idx, selErr := c.selectIndex(doc)
	if selErr != nil {
		return allFailed(refs, selErr), selErr
	}

	out := make([]driver.RawSample, 0, len(refs))
	for _, ref := range refs {
		plan, ok := c.plans[ref.ID]
		if !ok {
			p, err := planPoint(ref)
			if err != nil {
				out = append(out, errSample(ref.ID, driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("point %q: %s", ref.ID, err), false, err)))
				continue
			}
			plan = p
		}
		out = append(out, c.decodeSample(plan, idx))
	}
	// 单点问题不升级为链路错误（见包注释第 4 条）。
	return out, nil
}

// flattenRefs 摊平 spans 并按 PointID 去重。
func flattenRefs(spans []driver.ReadSpan) []driver.PointRef {
	n := 0
	for _, s := range spans {
		n += len(s.Points)
	}
	out := make([]driver.PointRef, 0, n)
	seen := make(map[string]bool, n)
	for _, s := range spans {
		for _, r := range s.Points {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// selectIndex 选出要读的 DeviceStream 并建立 dataItemId 索引。
//
// 多设备 Agent（一条产线上多台机床）会把多个 DeviceStream 放进同一份
// `/current`。不选就取 ID，会出现「A 机床的 Xabs 覆盖 B 机床的 Xabs」——
// 这种错配不会报错，只会让两台设备的坐标互相串。因此宁可要求显式配置。
func (c *conn) selectIndex(doc *mtcodec.Streams) (*mtcodec.Index, error) {
	if len(doc.Devices) == 0 {
		return nil, driver.E(driver.ErrProtocol, "read", c.devID,
			"/current carries no DeviceStream (the agent has no device configured, or the address points at another resource)",
			false, nil)
	}
	dev := doc.Devices[0]
	switch {
	case c.st.deviceUUID != "":
		found := false
		for _, d := range doc.Devices {
			if strings.EqualFold(d.UUID, c.st.deviceUUID) {
				dev, found = d, true
				break
			}
		}
		if !found {
			return nil, driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("no DeviceStream with uuid %q in /current (present: %s); "+
					"check labels.deviceUuid", c.st.deviceUUID, deviceList(doc)), false, nil)
		}
	case c.st.deviceName != "":
		found := false
		for _, d := range doc.Devices {
			if d.Name == c.st.deviceName {
				dev, found = d, true
				break
			}
		}
		if !found {
			for _, d := range doc.Devices {
				if strings.EqualFold(d.Name, c.st.deviceName) {
					dev, found = d, true
					break
				}
			}
		}
		if !found {
			return nil, driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("no DeviceStream with name %q in /current (present: %s); "+
					"check labels.deviceName", c.st.deviceName, deviceList(doc)), false, nil)
		}
	case len(doc.Devices) > 1:
		return nil, driver.E(driver.ErrAddress, "read", c.devID,
			fmt.Sprintf("/current carries %d DeviceStreams (%s) but neither labels.deviceUuid "+
				"nor labels.deviceName selects one", len(doc.Devices), deviceList(doc)), false, nil)
	}

	items := make([]mtcodec.Item, 0, 64)
	for _, comp := range dev.Components {
		items = append(items, comp.Items...)
	}
	return mtcodec.BuildIndex(items), nil
}

func deviceList(doc *mtcodec.Streams) string {
	parts := make([]string, 0, len(doc.Devices))
	for _, d := range doc.Devices {
		switch {
		case d.UUID != "" && d.Name != "":
			parts = append(parts, fmt.Sprintf("%s (%s)", d.Name, d.UUID))
		case d.UUID != "":
			parts = append(parts, d.UUID)
		case d.Name != "":
			parts = append(parts, d.Name)
		default:
			parts = append(parts, "<unnamed>")
		}
	}
	return strings.Join(parts, ", ")
}

// decodeSample 把一条观测折成平台采样。
//
// 三类失败都在这里变成**该点的**错误，而不是 Read 的错误：
//   - ID 不在文档里 → address：点表与设备模型对不上；
//   - 值是 UNAVAILABLE → bad：数据项存在但此刻无值，这是正常工况；
//   - 文本按声明的 dataType 解析不了 → address：类型声明错了。
func (c *conn) decodeSample(plan pointPlan, idx *mtcodec.Index) driver.RawSample {
	item, ok := idx.Lookup(plan.dataItemID)
	if !ok {
		return driver.RawSample{PointID: plan.ref.ID, Quality: model.QualityAddressError,
			Err: driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("point %q: data item %q is not in /current (%d data items present); "+
					"use `uic mtconnectread -probe` to list the ids the agent publishes",
					plan.ref.ID, plan.dataItemID, idx.Len()), false, nil)}
	}
	if item.Unavailable() {
		return driver.RawSample{PointID: plan.ref.ID, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("point %q: data item %q reports UNAVAILABLE", plan.ref.ID, plan.dataItemID),
				false, nil)}
	}
	raw := item.Value()
	if raw == "" {
		return driver.RawSample{PointID: plan.ref.ID, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.devID,
				fmt.Sprintf("point %q: data item %q carries an empty value", plan.ref.ID, plan.dataItemID),
				false, nil)}
	}
	v, err := textval.Decode([]byte(raw), plan.ref.DataType)
	if err != nil {
		hint := ""
		if item.Category == mtcodec.CategoryCondition {
			hint = "; a condition point must be declared as string — its value is the level (NORMAL/WARNING/FAULT)"
		}
		return driver.RawSample{PointID: plan.ref.ID, Quality: model.QualityAddressError,
			Err: driver.E(driver.ErrAddress, "read", c.devID,
				fmt.Sprintf("point %q: data item %q value %q cannot be read as %s%s",
					plan.ref.ID, plan.dataItemID, raw, plan.ref.DataType, hint), false, err)}
	}
	return driver.RawSample{PointID: plan.ref.ID, Value: v, Quality: conditionQuality(item.Condition)}
}

// conditionQuality 把条件级别映射成平台质量位。条件点的值本身就是级别，因此
// 级别即质量：FAULT 是坏值、WARNING 是不确定、NORMAL 是好值。
func conditionQuality(level mtcodec.ConditionLevel) model.Quality {
	switch level {
	case mtcodec.ConditionFault:
		return model.QualityBad
	case mtcodec.ConditionWarning:
		return model.QualityUncertain
	default:
		return model.QualityGood
	}
}

// ---------------------------------------------------------------------------
// 写：协议不支持
// ---------------------------------------------------------------------------

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		e := driver.E(driver.ErrUnsupported, "write", c.devID,
			fmt.Sprintf("point %q: MTConnect is a read-only information model "+
				"(the standard has no write service for data items)", req.PointID), false, nil)
		results = append(results, driver.WriteResult{
			PointID: req.PointID, Quality: model.QualityBad, Err: e,
		})
		if firstErr == nil {
			firstErr = e
		}
	}
	return results, firstErr
}

// ---------------------------------------------------------------------------
// 自检入口（不是 driver.Conn 的一部分）
// ---------------------------------------------------------------------------

// ProbeDevices 读取 `/probe` 并返回设备模型。
//
// 能力位把 Browse 标为 unsupported，因为调度式采集不需要枚举模型；而
// 「这台机床有哪些 dataItemId」是运维最先要回答的问题。两者不矛盾：
// 能力位描述采集路径，这个方法是一次性诊断（与 IEC 61850 的 Browse 同例）。
func (c *conn) ProbeDevices(ctx context.Context) (*mtcodec.Devices, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, "", driver.E(driver.ErrDisconnected, "probe", c.devID, "connection closed", true, nil)
	}
	r, err := c.get(ctx, mtcodec.PathProbe, nil, "probe")
	if err != nil {
		return nil, "", err
	}
	if r.status < 200 || r.status >= 300 {
		return nil, "", c.statusError("probe", r)
	}
	doc, perr := mtcodec.ParseDevices(r.body)
	if perr != nil {
		return nil, "", driver.E(driver.ErrProtocol, "probe", c.devID, "malformed /probe response", false, perr)
	}
	version := c.version
	if doc.Header.Version != "" {
		version = doc.Header.Version
	}
	return doc, version, nil
}

// Current 读取 `/current` 并返回解析后的快照。
func (c *conn) Current(ctx context.Context) (*mtcodec.Streams, string, error) {
	return c.streams(ctx, mtcodec.PathCurrent, nil, "current")
}

// Sample 读取 `/sample?from=N&count=M` 返回的序号窗口。
func (c *conn) Sample(ctx context.Context, from uint64, count int) (*mtcodec.Streams, string, error) {
	if count <= 0 || count > maxSampleCount {
		return nil, "", driver.E(driver.ErrUnsupported, "sample", c.devID,
			fmt.Sprintf("count %d must be within 1..%d", count, maxSampleCount), false, nil)
	}
	q := url.Values{}
	q.Set("from", strconv.FormatUint(from, 10))
	q.Set("count", strconv.Itoa(count))
	return c.streams(ctx, mtcodec.PathSample, q, "sample")
}

func (c *conn) streams(ctx context.Context, path string, q url.Values, op string) (*mtcodec.Streams, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, "", driver.E(driver.ErrDisconnected, op, c.devID, "connection closed", true, nil)
	}
	r, err := c.get(ctx, path, q, op)
	if err != nil {
		return nil, "", err
	}
	if r.status < 200 || r.status >= 300 {
		return nil, "", c.statusError(op, r)
	}
	doc, perr := mtcodec.ParseStreams(r.body)
	if perr != nil {
		return nil, "", driver.E(driver.ErrProtocol, op, c.devID, "malformed response", false, perr)
	}
	version := c.version
	if doc.Header.Version != "" {
		version = doc.Header.Version
	}
	return doc, version, nil
}

// LastVersion 返回最近一次响应里看到的 MTConnect 版本。
func (c *conn) LastVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// BaseURL 返回规范化后的 Agent 基地址（自检命令展示用）。
func (c *conn) BaseURL() string { return c.base.String() }

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func allFailed(refs []driver.PointRef, err error) []driver.RawSample {
	out := make([]driver.RawSample, 0, len(refs))
	for _, r := range refs {
		out = append(out, errSample(r.ID, err))
	}
	return out
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityFor(driver.KindOf(err)), Err: err}
}

func qualityFor(kind driver.ErrorKind) model.Quality {
	switch kind {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrAddress:
		return model.QualityAddressError
	default:
		return model.QualityBad
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
