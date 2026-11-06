package history

import (
	"errors"
	"fmt"
	"os"
	"time"

	"uic/internal/model"
)

// WindowStats 汇总某个点位在「最近 window」内的数值样本。
//
// 这个方法是「历史窗口聚合」（计算点与报警里的 `avg(dev:pt, 5m)`）的唯一
// 数据来源。它刻意不是 Query 的一层包装：
//
//   - Query 会按 Limit 截断并构造 Time/Quality 等完整结果，窗口聚合只需要
//     四个累加量；用 Query 做窗口会在高频点位上产生大量无用分配。
//   - 窗口的**过滤条件是固定的**：只要 good 的数值样本。把这个判断交给调用方，
//     迟早会有人用 Query 拿到 raw 点后自己相加，于是 stale 值也被算了进去。
//
// 返回 ok=false 的语义不在这里表达（由调用方的适配器负责），这里只返回
// error：区间非法、存储已关闭、扫描失败都算「无法提供」。
func (s *Store) WindowStats(device, point string, window time.Duration) (model.WindowStats, error) {
	var out model.WindowStats
	if device == "" || point == "" {
		return out, fmt.Errorf("%w: device and point are required", ErrInvalidQuery)
	}
	if window <= 0 {
		return out, fmt.Errorf("%w: window must be > 0", ErrInvalidQuery)
	}
	// 用历史库自己的时钟，而不是 time.Now：测试注入的假时钟必须同样作用于
	// 窗口计算，否则「窗口内样本」与「写入时刻」会落在两条时间线上。
	to := s.now().UTC()
	from := to.Add(-window)
	segs, err := s.snapshotSegments(from, to)
	if err != nil {
		return out, err
	}
	for _, seg := range segs {
		_, _, err := scanSegment(seg.path, func(rec Record) bool {
			if rec.Device != device || rec.Point != point {
				return true
			}
			if rec.Time.Before(from) || rec.Time.After(to) {
				return true
			}
			// 只认 good：uncertain 已经意味着「这个值可能不对」，把它平均进去
			// 会让一条本来该报警的曲线看起来平滑。
			if rec.Quality != model.QualityGood {
				return true
			}
			// 只有数值类型参与。bool 的 AsFloat 虽然是 1/0，但把开关量平均成
			// 0.37 没有工程意义（与历史聚合 Bucket 的规则保持一致）。
			if !rec.Value.Type.IsNumeric() {
				return true
			}
			v, ok := rec.Value.AsFloat()
			if !ok {
				return true
			}
			if out.Count == 0 {
				out.Min, out.Max = v, v
			} else {
				if v < out.Min {
					out.Min = v
				}
				if v > out.Max {
					out.Max = v
				}
			}
			out.Count++
			out.Sum += v
			return true
		})
		if err != nil {
			// 段可能在快照之后被淘汰：这不是错误，只是这段数据已经不在
			// 保留窗口内，与 Query 的处理保持一致。
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return out, fmt.Errorf("history: scan %s: %w", seg.path, err)
		}
	}
	return out, nil
}
