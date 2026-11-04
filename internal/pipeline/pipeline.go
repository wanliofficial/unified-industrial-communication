// Package pipeline 是采集结果的加工链：解码后的原始值 → 缩放 → 质量判定
// → 死区过滤 → 可用于快照与上报的 Sample。
//
// 设计约束：
//   - 不依赖缓存、驱动或调度，只做纯计算，因此可以被单测覆盖；
//   - 质量码必须贯穿：坏值不参与缩放，也不参与死区比较；
//   - 死区只抑制「变化事件」，不阻止快照更新。
package pipeline

import (
	"math"
	"sync"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

// Result 是一次加工的结果。
type Result struct {
	Sample  model.Sample `json:"sample"`
	Changed bool         `json:"changed"` // 是否生成「变化事件」（死区抑制后为 false）
}

// Processor 持有死区比较所需的上一次值。它是并发安全的，但同一设备的点位
// 由调度器串行处理，因此热路径几乎没有竞争。
type Processor struct {
	mu   sync.Mutex
	last map[string]model.Sample
}

// NewProcessor 创建加工器。
func NewProcessor() *Processor {
	return &Processor{last: map[string]model.Sample{}}
}

// Apply 加工一个原始采样。
//
//	dev  设备（提供默认字节序与单位，此处只用到 ID）
//	pt   点位配置（缩放、偏移、死区、单位）
//	raw  驱动返回的原始值
//	at   网关采集时刻（必须来自单调时钟换算）
//	seq  设备内单调序号
func (p *Processor) Apply(dev model.Device, pt model.Point, raw driver.RawSample, at time.Time, seq uint64) Result {
	q := raw.Quality
	if q == "" {
		q = model.QualityGood
	}
	val := raw.Value
	if val.Type == "" {
		val.Type = pt.DataType
	}

	// 质量不可用时不做任何数值加工：坏值乘上缩放系数只会变得更像真值。
	if q.IsUsable() {
		val = scaleValue(val, pt)
	}

	s := model.Sample{
		DeviceID:    dev.ID,
		PointID:     pt.ID,
		Value:       val,
		Quality:     q,
		CollectedAt: at,
		Seq:         seq,
		Unit:        pt.Unit,
	}
	if raw.Err != nil {
		s.Error = raw.Err.Error()
	}

	key := s.Key()
	p.mu.Lock()
	prev, existed := p.last[key]
	p.last[key] = s
	p.mu.Unlock()

	changed := true
	if existed {
		changed = p.isChanged(prev, s, pt.Deadband)
	}
	return Result{Sample: s, Changed: changed}
}

func (p *Processor) isChanged(prev, cur model.Sample, deadband float64) bool {
	// 质量变化永远算变化事件：否则「值没变但已经超时」会被死区吞掉。
	if prev.Quality != cur.Quality {
		return true
	}
	if !cur.Quality.IsUsable() {
		return false
	}
	if deadband <= 0 {
		return !prev.Value.Equal(cur.Value)
	}
	if cur.Value.Type == model.TypeBool || cur.Value.Type == model.TypeString || cur.Value.Type == model.TypeBytes {
		return !prev.Value.Equal(cur.Value)
	}
	a, ok1 := prev.Value.AsFloat()
	b, ok2 := cur.Value.AsFloat()
	if !ok1 || !ok2 {
		return !prev.Value.Equal(cur.Value)
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return true
	}
	return math.Abs(b-a) >= deadband
}

func scaleValue(v model.Value, pt model.Point) model.Value {
	if v.Type == model.TypeBool || v.Type == model.TypeString || v.Type == model.TypeBytes {
		return v
	}
	n, ok := v.AsFloat()
	if !ok {
		return v
	}
	return model.NumValue(v.Type, n*pt.EffectiveScale()+pt.Bias)
}

// Forget 丢弃某个设备的历史值（设备被删除或重建时调用），避免点位 ID 复用
// 后与旧设备的死区基准串味。
func (p *Processor) Forget(device string) {
	prefix := device + "/"
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.last {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(p.last, k)
		}
	}
}

// Last 返回某个点位的上次采样（用于调试与测试）。
func (p *Processor) Last(device, point string) (model.Sample, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.last[device+"/"+point]
	return s, ok
}

// Reset 清空全部历史值。
func (p *Processor) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = map[string]model.Sample{}
}
