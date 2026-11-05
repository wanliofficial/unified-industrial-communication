package history

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"uic/internal/model"
)

// Aggregation 是聚合方式。只在 step > 0 时有意义。
type Aggregation string

const (
	// AggLast 取桶内最后一个值——趋势图默认用它：它等于「这个时刻的值」。
	AggLast Aggregation = "last"
	// AggFirst 取桶内第一个值（与 Last 对照可用于判断桶内方向）。
	AggFirst Aggregation = "first"
	AggAvg   Aggregation = "avg"
	AggMin   Aggregation = "min"
	AggMax   Aggregation = "max"
	// AggCount 只关心「有多少条」，用于密度/心跳检查。
	AggCount Aggregation = "count"
)

// Valid 报告聚合方式是否合法。
func (a Aggregation) Valid() bool {
	switch a {
	case AggLast, AggFirst, AggAvg, AggMin, AggMax, AggCount:
		return true
	}
	return false
}

// Query 是一次历史查询。
//
// Device/Point 必填；From/To 为空时分别取「一小时前」与「现在」。
// Step > 0 时做定步长聚合，否则返回原始点。
type Query struct {
	Device string
	Point  string
	From   time.Time
	To     time.Time
	Limit  int
	Step   time.Duration
	Agg    Aggregation
}

// Point 是原始查询返回的一个采样点。
type Point struct {
	Time    time.Time     `json:"time"`
	Value   model.Value   `json:"value"`
	Quality model.Quality `json:"quality"`
}

// Bucket 是聚合查询返回的一个桶。
//
// Min/Max/Avg 用指针：文本/布尔点位没有数值语义，它们必须是 null 而不是 0，
// 否则前端会把「无法聚合」画成一条落在 0 的线。Bool 同理不算数值——把
// 一串开关量平均成 0.37 在工程上没有任何解释。
type Bucket struct {
	Time    time.Time     `json:"time"`
	Count   int           `json:"count"`
	First   model.Value   `json:"first"`
	Last    model.Value   `json:"last"`
	Min     *float64      `json:"min,omitempty"`
	Max     *float64      `json:"max,omitempty"`
	Avg     *float64      `json:"avg,omitempty"`
	Quality model.Quality `json:"quality"`
}

// Result 是一次查询的结果。Points 与 Buckets 互斥。
type Result struct {
	Device    string         `json:"device"`
	Point     string         `json:"point"`
	From      time.Time      `json:"from"`
	To        time.Time      `json:"to"`
	Step      model.Duration `json:"step,omitempty"`
	Agg       Aggregation    `json:"agg,omitempty"`
	Points    []Point        `json:"points,omitempty"`
	Buckets   []Bucket       `json:"buckets,omitempty"`
	Count     int            `json:"count"`
	Truncated bool           `json:"truncated"`
	// Scanned/Skipped 是诊断量：扫了多少行、跳过了多少坏行。放在响应里
	// 而不是只进日志，因为「查询变慢」与「文件损坏」在现场很难区分。
	Scanned int `json:"scanned"`
	Skipped int `json:"skipped"`
}

// Query 执行查询。它先快照段索引（短锁），再在锁外顺序扫描。
func (s *Store) Query(q Query) (Result, error) {
	if q.Device == "" || q.Point == "" {
		return Result{}, fmt.Errorf("%w: device and point are required", ErrInvalidQuery)
	}
	now := s.now()
	if q.To.IsZero() {
		q.To = now
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-time.Hour)
	}
	// 请求里的相对时间来自服务器本地时区；统一成 UTC 后，响应里的
	// from/to/bucket.time 与记录时间才是同一种表示。
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if !q.From.Before(q.To) {
		return Result{}, fmt.Errorf("%w: from must be before to", ErrInvalidRange)
	}
	if q.Step < 0 {
		return Result{}, fmt.Errorf("%w: step must be >= 0", ErrInvalidQuery)
	}
	if q.Agg == "" {
		q.Agg = AggLast
	}
	if q.Step == 0 && q.Agg != AggLast {
		return Result{}, fmt.Errorf("%w: agg requires step", ErrInvalidQuery)
	}
	if !q.Agg.Valid() {
		return Result{}, fmt.Errorf("%w: unknown agg %q", ErrInvalidQuery, q.Agg)
	}
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	if q.Step > 0 {
		buckets := int64(q.To.Sub(q.From)/q.Step) + 1
		if buckets > MaxBuckets {
			return Result{}, fmt.Errorf("%w: %d buckets exceeds %d; increase step or narrow the range",
				ErrInvalidQuery, buckets, MaxBuckets)
		}
	}

	segs, err := s.snapshotSegments(q.From, q.To)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		Device: q.Device, Point: q.Point, From: q.From, To: q.To,
		Step: model.Duration(q.Step), Agg: q.Agg,
	}
	var agg map[int64]*bucketAgg
	if q.Step > 0 {
		agg = make(map[int64]*bucketAgg)
	}
	// 原始查询按 limit 截断并标记 truncated：客户端不需要额外发一次
	// 「count」查询就知道自己看到的不是全部。
	stop := false
	for _, seg := range segs {
		if stop {
			break
		}
		read, skipped, err := scanSegment(seg.path, func(rec Record) bool {
			if rec.Device != q.Device || rec.Point != q.Point {
				return true
			}
			if rec.Time.Before(q.From) || rec.Time.After(q.To) {
				return true
			}
			if agg != nil {
				idx := rec.Time.Sub(q.From) / q.Step
				b := agg[int64(idx)]
				if b == nil {
					b = &bucketAgg{time: q.From.Add(time.Duration(idx) * q.Step)}
					agg[int64(idx)] = b
				}
				b.add(rec)
				return true
			}
			if len(res.Points) >= q.Limit {
				res.Truncated = true
				stop = true
				return false
			}
			res.Points = append(res.Points, Point{Time: rec.Time, Value: rec.Value, Quality: rec.Quality})
			return true
		})
		res.Scanned += read
		res.Skipped += skipped
		if err != nil {
			// 段可能在扫描开始后被写入线程淘汰掉了（快照只是索引，
			// 文件本身随时可能被删）。这不是错误，只是「这段数据已经
			// 不在保留窗口内」。
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return res, fmt.Errorf("history: scan %s: %w", seg.path, err)
		}
	}

	if agg == nil {
		// 段按时间升序，但单段内不保证严格有序（时钟回拨、外部导入），
		// 而北向契约承诺「points 按时间升序」。
		sort.SliceStable(res.Points, func(i, j int) bool {
			return res.Points[i].Time.Before(res.Points[j].Time)
		})
	}
	if agg != nil {
		keys := make([]int64, 0, len(agg))
		for k := range agg {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		res.Buckets = make([]Bucket, 0, len(keys))
		for _, k := range keys {
			res.Buckets = append(res.Buckets, agg[k].bucket())
		}
		// 聚合结果同样受 limit 约束，避免一次返回上万个桶。
		if len(res.Buckets) > q.Limit {
			res.Buckets = res.Buckets[:q.Limit]
			res.Truncated = true
		}
		res.Count = len(res.Buckets)
		return res, nil
	}
	res.Count = len(res.Points)
	return res, nil
}

// bucketAgg 是扫描过程中的可变累加器。
type bucketAgg struct {
	time  time.Time
	count int

	first, last model.Value
	quality     model.Quality

	numeric    bool
	numCount   int
	sum        float64
	minV, maxV float64
}

func (b *bucketAgg) add(rec Record) {
	if b.count == 0 {
		b.first = rec.Value
	}
	b.last = rec.Value
	b.count++
	// 桶质量取「最差」而不是「最后一条」：一个桶里只要有一条 bad，
	// 整桶的均值/极值就不可信；标成 good 会让前端把一段坏数据当成好数据
	// 画出来。raw 查询是逐条记录，不受这条影响。
	q := rec.Quality
	if q == "" {
		q = model.QualityGood
	}
	if b.count == 1 || qualityRank(q) > qualityRank(b.quality) {
		b.quality = q
	}
	// 只有数值类型参与 min/max/avg；BOOL 虽然 AsFloat 可用，但把开关量
	// 平均成 0.37 没有工程意义。
	if v, ok := rec.Value.AsFloat(); ok && rec.Value.Type.IsNumeric() {
		if !b.numeric {
			b.numeric = true
			b.minV, b.maxV = v, v
		} else {
			if v < b.minV {
				b.minV = v
			}
			if v > b.maxV {
				b.maxV = v
			}
		}
		b.numCount++
		b.sum += v
	}
}

// qualityRank 给出质量的「坏」程度排序，用于桶内取最差。
func qualityRank(q model.Quality) int {
	switch q {
	case model.QualityGood:
		return 0
	case model.QualityUncertain:
		return 1
	case model.QualityStale:
		return 2
	case model.QualityBad:
		return 3
	case model.QualityTimeout:
		return 4
	case model.QualityCommError:
		return 5
	case model.QualityAddressError:
		return 6
	case model.QualityNotConnected:
		return 7
	case model.QualityDisabled:
		return 8
	}
	return 9
}

func (b *bucketAgg) bucket() Bucket {
	out := Bucket{
		Time: b.time, Count: b.count, First: b.first, Last: b.last,
		Quality: b.quality,
	}
	if b.numeric && b.numCount > 0 {
		minV, maxV, avg := b.minV, b.maxV, b.sum/float64(b.numCount)
		out.Min, out.Max, out.Avg = &minV, &maxV, &avg
	}
	return out
}
