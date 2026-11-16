// Package driver 定义协议驱动的契约（Codec / Transport / Driver 三层）。
//
// 契约版本为 1.0。所有 I/O 方法必须带 context.Context，所有错误必须分类，
// 所有驱动必须声明能力四态与可接受的地址类型。详见
// docs/03-product/04-driver-sdk-spec.md。
package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"uic/internal/model"
)

// SDKVersion 是当前驱动契约版本。
const SDKVersion = "1.0"

// Role 描述驱动在协议中的角色。
type Role string

const (
	RoleMaster Role = "master"
	RoleSlave  Role = "slave"
	RoleBoth   Role = "both"
)

// CapState 是能力四态。禁止在不确定时声明 CapSupported。
type CapState string

const (
	CapSupported        CapState = "supported"
	CapUnsupported      CapState = "unsupported"
	CapRuntimeDependent CapState = "runtime_dependent"
	CapExperimental     CapState = "experimental"
)

// Capabilities 描述一个驱动的能力集合。
type Capabilities struct {
	Read        CapState `json:"read"`
	Write       CapState `json:"write"`
	Subscribe   CapState `json:"subscribe"`
	Browse      CapState `json:"browse"`
	Discovery   CapState `json:"discovery"`
	MultiRead   CapState `json:"multiRead"`
	BitAccess   CapState `json:"bitAccess"`
	StructTypes CapState `json:"structTypes"`
}

// ConfigField 用于前端生成配置表单。
type ConfigField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // string | int | float | bool | enum | duration
	Required    bool     `json:"required"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Scope       string   `json:"scope"` // channel | device | point
	// Secret 标记「这个字段是凭据」。渲染配置界面时必须做成密码框，
	// 且回显时用固定掩码而不是真实值——把密码原样吐回前端是最常见
	// 的一类凭据泄漏。
	Secret bool `json:"secret,omitempty"`
}

// Meta 是驱动的不可变元数据。
type Meta struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Version      string              `json:"version"`
	SDKVersion   string              `json:"sdkVersion"`
	Roles        []Role              `json:"roles"`
	Transports   []string            `json:"transports"`
	AddressKinds []model.AddressKind `json:"addressKinds"`
	Capabilities Capabilities        `json:"capabilities"`
	ConfigFields []ConfigField       `json:"configFields,omitempty"`
	PointFields  []ConfigField       `json:"pointFields,omitempty"`
	Description  string              `json:"description,omitempty"`
}

// Accepts 返回该驱动是否接受某种寻址。
func (m Meta) Accepts(k model.AddressKind) bool {
	for _, a := range m.AddressKinds {
		if a == k {
			return true
		}
	}
	return false
}

// OpenConfig 是驱动实例化所需的全部输入。驱动不得读取全局配置。
type OpenConfig struct {
	Channel model.Channel
	Device  model.Device
	Log     *slog.Logger
	Metrics Metrics
	// Lookup 返回某点位的最新采样，供「需要引用其他点位」的驱动使用
	// （目前只有 computed）。它在装配期由 runtime 注入 cache.Get。
	//
	// 为什么给驱动一个读取入口而不是让它直接持有 cache：这个函数是**只读**
	// 的，拿不到写入与订阅能力；驱动因此不可能绕过采集层的去重、死区、
	// 序号与代次治理。为 nil 时驱动必须按「没有值」处理，而不是 panic。
	Lookup func(device, point string) (model.Sample, bool)
	// History 返回某点位在最近 window 内的数值样本摘要，供驱动/报警里的
	// 历史窗口聚合（`avg(line-a:power, 5m)`）使用。
	//
	// 第二个返回值 false 表示「无法提供窗口数据」（历史未开启、查询失败），
	// 与「窗口内没有 good 样本」不同。为 nil 时窗口聚合会显式失败，而不是
	// 悄悄返回 0——没注入历史却写了一个窗口，是装配错误，必须被看见。
	//
	// 由 runtime 注入 history.Store 的适配器（带短 TTL 缓存，避免报警
	// 200ms 的兜底扫描反复扫盘）。
	History func(ref string, window time.Duration) (model.WindowStats, bool)
}

// Metrics 是驱动可用的最小埋点接口；可为 nil。
type Metrics interface {
	Counter(name string, labels map[string]string) func(delta float64)
	Observe(name string, v float64, labels map[string]string)
}

// Driver 是驱动入口。
type Driver interface {
	Meta() Meta
	Open(ctx context.Context, cfg OpenConfig) (Conn, error)
}

// Conn 是一条到设备/站点的连接。同一 Conn 不会被并发调用。
type Conn interface {
	Probe(ctx context.Context) error
	Read(ctx context.Context, spans []ReadSpan) ([]RawSample, error)
	Write(ctx context.Context, reqs []WriteRequest) ([]WriteResult, error)
	Close() error
}

// ReadSpan 是一次批量读覆盖的点位集合。
type ReadSpan struct {
	ID     string     `json:"id"`
	Points []PointRef `json:"points"`
}

// PointRef 是驱动读写所需的点位视图。
type PointRef struct {
	ID        string          `json:"id"`
	Address   model.Address   `json:"address"`
	DataType  model.DataType  `json:"dataType"`
	ByteOrder model.ByteOrder `json:"byteOrder,omitempty"`
	Length    int             `json:"length"`
}

// PointRefOf 从模型点位构造驱动视图，应用设备级字节序默认值。
func PointRefOf(d model.Device, p model.Point) PointRef {
	bo := p.ByteOrder
	if bo == "" {
		bo = d.ByteOrder
	}
	return PointRef{
		ID: p.ID, Address: p.Address, DataType: p.DataType,
		ByteOrder: bo, Length: p.Length(),
	}
}

// RawSample 是驱动返回的原始结果（未缩放）。
type RawSample struct {
	PointID string
	Value   model.Value
	Quality model.Quality
	Err     error
}

// WriteRequest 是一次写入项。
type WriteRequest struct {
	PointID   string
	Address   model.Address
	DataType  model.DataType
	ByteOrder model.ByteOrder
	Value     model.Value
}

// WriteResult 是单点写入结果。
type WriteResult struct {
	PointID string        `json:"point"`
	OK      bool          `json:"ok"`
	Quality model.Quality `json:"quality"`
	Err     error         `json:"-"`
}

// Codec 是纯函数编解码层：不做 I/O、不读时钟、不持有状态。
type Codec interface {
	EncodeRead(span ReadSpan) ([]byte, error)
	DecodeRead(span ReadSpan, resp []byte) ([]RawSample, error)
	EncodeWrite(req WriteRequest) ([]byte, error)
	DecodeWrite(req WriteRequest, resp []byte) (WriteResult, error)
}

// Transport 是一次请求/响应往返的抽象。
type Transport interface {
	RoundTrip(ctx context.Context, req []byte) ([]byte, error)
	Close() error
}

// ---------------------------------------------------------------------------
// 错误模型
// ---------------------------------------------------------------------------

// ErrorKind 是错误分类，决定上层是否重试与是否熔断。
type ErrorKind string

const (
	ErrTimeout      ErrorKind = "timeout"
	ErrDisconnected ErrorKind = "disconnected"
	ErrProtocol     ErrorKind = "protocol"
	ErrAddress      ErrorKind = "address"
	ErrPermission   ErrorKind = "permission"
	ErrUnsupported  ErrorKind = "unsupported"
	ErrInternal     ErrorKind = "internal"
)

// OpError 是所有驱动错误的统一包装。
type OpError struct {
	Kind      ErrorKind `json:"kind"`
	Op        string    `json:"op"`
	Device    string    `json:"device,omitempty"`
	Protocol  string    `json:"protocol,omitempty"`
	Msg       string    `json:"message"`
	Temporary bool      `json:"temporary"`
	Err       error     `json:"-"`
}

// Error 输出 `设备 操作 类别: 说明`。
//
// Protocol 字段是可选的（目前没有驱动填写）：早期实现把它当成前缀硬编码，
// 于是所有驱动错误都显示成「/read timeout: ...」——一个以斜杠开头、既没有
// 设备也没有协议的字符串。错误信息是要被人读的，前缀必须真的存在才打印。
func (e *OpError) Error() string {
	var b strings.Builder
	if e.Protocol != "" {
		b.WriteString(e.Protocol)
		b.WriteByte(' ')
	}
	if e.Device != "" {
		b.WriteString(e.Device)
		b.WriteByte(' ')
	}
	b.WriteString(e.Op)
	b.WriteByte(' ')
	b.WriteString(string(e.Kind))
	b.WriteString(": ")
	b.WriteString(e.Msg)
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *OpError) Unwrap() error { return e.Err }

// E 构造一个带分类的错误。
func E(kind ErrorKind, op, device, msg string, temporary bool, err error) *OpError {
	return &OpError{Kind: kind, Op: op, Device: device, Msg: msg, Temporary: temporary, Err: err}
}

// Kinded 允许协议层错误自带分类。
//
// S7 的单项返回码（地址不存在、无权限）不是网络错误，也不该在驱动里
// 逐处包装成 OpError：让错误对象自己回答「我属于哪一类」最不容易漏。
type Kinded interface {
	Kind() ErrorKind
}

// KindOf 提取错误分类；非 OpError 视为 internal。
func KindOf(err error) ErrorKind {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Kind
	}
	var k Kinded
	if errors.As(err, &k) {
		return k.Kind()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrTimeout
	}
	return ErrInternal
}

// IsRetryable 表示该错误是否值得重试。
func IsRetryable(err error) bool {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Temporary
	}
	return false
}

// CountsTowardBreaker 表示该错误是否计入熔断。
// 地址错误与权限错误属于配置问题，不应导致整台设备被熔断。
func CountsTowardBreaker(err error) bool {
	switch KindOf(err) {
	case ErrAddress, ErrPermission, ErrUnsupported:
		return false
	}
	return true
}

// IsLinkFailure 表示该错误是否说明「链路本身已经不可继续」。
//
// 超时与断连是链路级：同一个连接上的后续点位不可能成功，继续逐个等超时只会
// 把一轮采集拖成 N 倍时长。地址/权限/不支持是**点级**问题（点表配错、符号写错），
// 换一个点位照样能读，因此不能短路整轮。
//
// 这个谓词必须由 driver 包统一给出而不是各驱动各写一遍：漏掉一处就会出现
// 「一个坏点让整台设备每周期断链重连」——而且它只在运行期表现为「采集很慢」，
// 单测里看不出来。
func IsLinkFailure(err error) bool {
	switch KindOf(err) {
	case ErrTimeout, ErrDisconnected:
		return true
	}
	return false
}

// PartialReadError 决定一次「逐点读」的结果里该向调用方返回哪个错误。
//
// 契约（docs §3.3.1）：**只要有任意一个点位成功，`Read` 就不向上
// 返回错误**——好样本必须保留，单点失败由该样本自己的 Err + 质量位表达。
// 全部点位失败才返回第一个错误，让调度器进入故障/退避。
//
// good 是成功点位数。这不是「宽容」而是必须的：上层 collector 看到
// Read 返回错误会按错误分类丢弃连接（ErrDisconnected/ErrTimeout/ErrProtocol
// 直接 Close 重连），一个属性名写错的点位就会让整台设备每周期重连一次。
func PartialReadError(good int, firstErr error) error {
	if good == 0 && firstErr != nil {
		return firstErr
	}
	return nil
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

var (
	regMu sync.RWMutex
	reg   = map[string]Driver{}
)

// Register 注册驱动；重复 ID 返回错误。
func Register(d Driver) error {
	m := d.Meta()
	if m.ID == "" {
		return errors.New("driver: empty id")
	}
	if m.SDKVersion != SDKVersion {
		return fmt.Errorf("driver %q: sdkVersion %q, want %q", m.ID, m.SDKVersion, SDKVersion)
	}
	if len(m.AddressKinds) == 0 {
		return fmt.Errorf("driver %q: addressKinds required", m.ID)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := reg[m.ID]; exists {
		return fmt.Errorf("driver %q: already registered", m.ID)
	}
	reg[m.ID] = d
	return nil
}

// MustRegister 在注册失败时 panic，供内置驱动初始化使用。
func MustRegister(d Driver) {
	if err := Register(d); err != nil {
		panic(err)
	}
}

// Get 按协议 ID 查找驱动。
func Get(id string) (Driver, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	d, ok := reg[id]
	return d, ok
}

// List 返回全部驱动元数据（按 ID 排序）。
func List() []Meta {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Meta, 0, len(reg))
	for _, d := range reg {
		out = append(out, d.Meta())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
