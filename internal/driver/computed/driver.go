// Package computed 实现「计算点」驱动：点位的值不是从设备读来的，而是由
// 其他点位的值经表达式算出（能耗合计、温差、效率、简单判定）。
//
// 它是全项目唯一一个**没有物理链路**的驱动，因此有三条与别的驱动不同的
// 约定，必须在读代码前先知道：
//
//  1. 依赖只能通过缓存（driver.OpenConfig.Lookup）或同一次 Read 内已算出的
//     结果获得。驱动不直接访问 cache——那会绕过去重、死区、序号、代次四道
//     治理，也会让「这个值是谁写的」变得不可回答。
//  2. 同设备的计算点按拓扑序在同一次 Read 内求值：A 引用 B 时，B 一定是
//     本次算出的新值，不是上一轮。跨设备引用则是最终一致（下一轮生效）——
//     设备之间没有全局时序，强行同步会让一台慢设备拖垮整张依赖图。
//  3. 质量沿依赖传播：任一输入不可用，输出就不可用，且质量码取最差值。
//     **绝不**用 0 代替缺失输入——那会让「A 没数据」变成「合计 = B」，
//     一个看起来完全正常的错误数字。
package computed

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"uic/internal/calc"
	"uic/internal/driver"
	"uic/internal/model"
)

// Driver 是计算点驱动。
type Driver struct{}

// New 返回计算点驱动。
func New() *Driver { return &Driver{} }

// Meta 声明能力。Write 明确不支持：计算点是纯函数结果，没有可写回的地址。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "computed",
		Name:         "计算点（表达式）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleBoth},
		Transports:   []string{"memory"},
		AddressKinds: []model.AddressKind{model.AddressComputed},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapUnsupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "address.expr", Label: "表达式", Type: "string", Required: true,
				Scope: "point",
				Help:  "引用写法 device:point，例：line-a:power / line-a:rate"},
		},
	}
}

// bound 是一个点位与它编译好的表达式。
type bound struct {
	pt   model.Point
	prog *calc.Program
	// deps 是同设备内被依赖的计算点 ID（跨设备依赖不进这里，它们是
	// 最终一致的，不参与本次拓扑排序）。
	deps []string
}

type conn struct {
	devID  string
	order  []*bound // 拓扑序：被依赖者在前
	byID   map[string]*bound
	lookup func(device, point string) (model.Sample, bool)
	// 求值需要的临时缓冲按 Read 分配，不放进 conn：Read 由设备 actor
	// 串行调用，但把状态挂在连接上会让「同一连接被并发读」变成隐性错误。
}

// Open 编译全部表达式并做拓扑排序。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	byID := make(map[string]*bound, len(cfg.Device.Points))
	for _, p := range cfg.Device.Points {
		if p.Address.Kind != model.AddressComputed {
			// 配置期已经按 AddressKinds 拒绝过混配，这里跳过即可：真出现了
			// 也会因为「驱动没有返回该点位」被采集层显式降级，不会静默。
			continue
		}
		prog, err := calc.Compile(p.Address.Expr)
		if err != nil {
			return nil, driver.E(driver.ErrInternal, "open", cfg.Device.ID,
				fmt.Sprintf("point %q: %v", p.ID, err), false, nil)
		}
		byID[p.ID] = &bound{pt: p, prog: prog}
	}
	if len(byID) == 0 {
		return nil, driver.E(driver.ErrInternal, "open", cfg.Device.ID,
			"computed device has no computed points", false, nil)
	}
	for _, b := range byID {
		for _, ref := range b.prog.Refs() {
			dev, pt, ok := splitRef(ref)
			if !ok || dev != cfg.Device.ID {
				continue
			}
			if _, isComputed := byID[pt]; isComputed {
				b.deps = append(b.deps, pt)
			}
		}
	}
	order, err := topoSort(byID)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "open", cfg.Device.ID, err.Error(), false, nil)
	}
	return &conn{devID: cfg.Device.ID, order: order, byID: byID, lookup: cfg.Lookup}, nil
}

// topoSort 返回「被依赖者在前」的顺序。环必须报错：环上的点位永远算不出
// 值，而如果放任它按 map 顺序求值，表现为「有时好有时坏」——最难排查的一类。
func topoSort(byID map[string]*bound) ([]*bound, error) {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 稳定输出：同一份配置每次启动的求值顺序一致

	const (
		white = 0 // 未访问
		grey  = 1 // 在栈上
		black = 2 // 已完成
	)
	color := make(map[string]int, len(ids))
	out := make([]*bound, 0, len(ids))
	var stack []string
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(stack, id), " -> "))
		}
		color[id] = grey
		stack = append(stack, id)
		b := byID[id]
		deps := append([]string(nil), b.deps...)
		sort.Strings(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		out = append(out, b)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// refState 累计一次求值中输入点的最差质量与第一条失败原因。
type refState struct {
	worst  model.Quality
	set    bool
	badRef string
	reason string
}

func (s *refState) note(q model.Quality, ref, reason string) {
	if !s.set || rank(q) > rank(s.worst) {
		s.worst = q
		s.set = true
	}
	if reason != "" && s.badRef == "" {
		s.badRef, s.reason = ref, reason
	}
}

// Read 按拓扑序求值，只返回本次请求的点位。
//
// 被依赖但不在此次 span 里的计算点也要算：它们是本次结果的输入。全部点位
// 都算一遍的成本可以忽略（纯算术，没有 IO），换来的是「同一次 Read 内的
// 值互相自洽」。
func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(spans))
	for _, sp := range spans {
		for _, p := range sp.Points {
			if _, ok := c.byID[p.ID]; ok {
				want[p.ID] = true
			}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}

	// vals 以 "device/point" 为键，同时容纳本次算出的计算点值。
	vals := make(map[string]model.Sample, len(c.order))
	out := make([]driver.RawSample, 0, len(want))
	for _, b := range c.order {
		s := c.evalOne(b, vals)
		vals[refKey(c.devID, b.pt.ID)] = s
		if !want[b.pt.ID] {
			continue
		}
		out = append(out, driver.RawSample{
			PointID: b.pt.ID,
			Value:   s.Value,
			Quality: s.Quality,
			Err:     errOf(s),
		})
	}
	return out, nil
}

// evalOne 求值单个点位。
//
// 质量规则：
//   - 所有输入都可用 → 用最差输入质量（输入 uncertain，输出也只能是
//     uncertain：算式本身没问题，但输入尚未确认）；
//   - 任一输入不可用 → **不计算**，直接沿用最差质量并说明是哪个输入；
//   - 求值本身失败（除零、溢出、越界）→ bad，并带上原因。
func (c *conn) evalOne(b *bound, vals map[string]model.Sample) model.Sample {
	var st refState
	resolve := func(ref string) (float64, bool) {
		dev, pt, ok := splitRef(ref)
		if !ok {
			st.note(model.QualityBad, ref, "malformed reference")
			return 0, false
		}
		s, found := vals[refKey(dev, pt)]
		if !found && c.lookup != nil {
			s, found = c.lookup(dev, pt)
		}
		if !found {
			st.note(model.QualityBad, ref, "no value yet")
			return 0, false
		}
		if !s.Quality.IsUsable() {
			st.note(s.Quality, ref, "input quality is "+string(s.Quality))
			return 0, false
		}
		v, ok := s.Value.AsFloat()
		if !ok {
			st.note(model.QualityBad, ref, "input is not numeric")
			return 0, false
		}
		st.note(s.Quality, ref, "")
		return v, true
	}

	sample := model.Sample{DeviceID: c.devID, PointID: b.pt.ID, Unit: b.pt.Unit}
	v, err := b.prog.Eval(resolve)
	// 先看输入状态，再看求值错误：输入不可用时求值必然报「引用没有值」，
	// 那是**结果**不是原因。真正的原因是某个输入超时/断链，质量码必须
	// 如实反映它（timeout 与 bad 在排障时指向完全不同的动作）。
	if st.badRef != "" {
		sample.Quality = st.worst
		sample.Error = fmt.Sprintf("input %s: %s", st.badRef, st.reason)
		return sample
	}
	if err != nil {
		sample.Quality = model.QualityBad
		sample.Error = fmt.Sprintf("expression %q: %v", b.prog.String(), err)
		return sample
	}
	val, err := encode(b.pt.DataType, v)
	if err != nil {
		sample.Quality = model.QualityBad
		sample.Error = err.Error()
		return sample
	}
	sample.Value = val
	if st.set {
		sample.Quality = st.worst
	} else {
		sample.Quality = model.QualityGood
	}
	return sample
}

// errOf 把样本上的原因转成带分类的错误，让上层能给出一致的错误码
// （例如 bad 与 timeout 在 UI 上的排障提示完全不同）。
func errOf(s model.Sample) error {
	if s.Error == "" {
		return nil
	}
	kind := driver.ErrProtocol
	switch s.Quality {
	case model.QualityBad:
		kind = driver.ErrInternal
	case model.QualityTimeout:
		kind = driver.ErrTimeout
	case model.QualityCommError, model.QualityNotConnected:
		kind = driver.ErrDisconnected
	case model.QualityAddressError:
		kind = driver.ErrAddress
	}
	return driver.E(kind, "read", s.DeviceID, s.Error, true, nil)
}

// Write 永远拒绝：计算点没有物理地址。
func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	out := make([]driver.WriteResult, len(reqs))
	for i, r := range reqs {
		out[i] = driver.WriteResult{
			PointID: r.PointID,
			OK:      false,
			Quality: model.QualityBad,
			Err:     driver.E(driver.ErrPermission, "write", c.devID, "computed points are read-only", false, nil),
		}
	}
	return out, nil
}

// Probe 永远成功：计算点不依赖任何外部链路，没有「连不上」这回事。
func (c *conn) Probe(ctx context.Context) error { return ctx.Err() }

// Close 无资源需要释放。
func (c *conn) Close() error { return nil }

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func refKey(device, point string) string { return device + "/" + point }

// splitRef 拆分 `device:point`。
func splitRef(ref string) (string, string, bool) {
	i := strings.IndexByte(ref, ':')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// worse 返回两个质量码里更差的那个。
//
// 等级不是随便排的：它反映「这个值离可用有多远」。uncertain 仍可用（写入
// 后尚未回读确认），stale 已不可用但至少曾经真实，bad 是彻底没有可信来源。
func worse(a, b model.Quality) model.Quality {
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

func rank(q model.Quality) int {
	switch q {
	case model.QualityGood:
		return 0
	case model.QualityUncertain:
		return 1
	case model.QualityStale:
		return 2
	case model.QualityTimeout:
		return 3
	case model.QualityNotConnected:
		return 4
	case model.QualityCommError:
		return 5
	case model.QualityAddressError:
		return 6
	default:
		return 7
	}
}

// encode 把表达式结果转成点位声明的数据类型。
//
// 整数类型做**范围检查**而不是截断：`70000` 写进 uint16 变成 `4464`，
// 会让操作员按一个错误的数字决策——这比「没有数据」糟得多。小数部分按
// 四舍五入处理（`2.7` → `3`），因为配置写的是整数类型，用户要的是最近的
// 整数而不是向下取整的偏差。
func encode(dt model.DataType, v float64) (model.Value, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return model.Value{}, fmt.Errorf("result is not a finite number")
	}
	if dt == model.TypeBool {
		return model.BoolValue(v != 0), nil
	}
	if !dt.IsNumeric() {
		return model.Value{}, fmt.Errorf("dataType %q cannot hold a computed number", dt)
	}
	switch dt {
	case model.TypeInt16:
		return intValue(dt, v, math.MinInt16, math.MaxInt16)
	case model.TypeUint16:
		return intValue(dt, v, 0, math.MaxUint16)
	case model.TypeInt32:
		return intValue(dt, v, math.MinInt32, math.MaxInt32)
	case model.TypeUint32:
		return intValue(dt, v, 0, math.MaxUint32)
	case model.TypeInt64:
		return intValue(dt, v, math.MinInt64, math.MaxInt64)
	case model.TypeUint64:
		return intValue(dt, v, 0, math.MaxUint64)
	case model.TypeFloat32:
		f := float32(v)
		if math.IsInf(float64(f), 0) {
			return model.Value{}, fmt.Errorf("%v overflows float32", v)
		}
		return model.NumValue(dt, float64(f)), nil
	}
	return model.NumValue(dt, v), nil
}

func intValue(dt model.DataType, v, lo, hi float64) (model.Value, error) {
	r := math.Round(v)
	if r < lo || r > hi {
		return model.Value{}, fmt.Errorf("%v out of range for %s", v, dt)
	}
	return model.NumValue(dt, r), nil
}
