// Package planner 把离散点位合并为批量读请求（ReadSpan）。
//
// 规划是协议无关的：寄存器型按地址连续性与上限合并，符号型按前缀分组，
// 对象型按实例分组，文件型按文件/记录分组。驱动可以在 Read 内进一步合并，
// 但不能要求上层逐点调用。
package planner

import (
	"fmt"
	"sort"
	"strings"

	"uic/internal/driver"
	"uic/internal/model"
)

// Options 控制合并激进程度。
type Options struct {
	MaxGap int // 允许的寄存器空洞（默认 8）
}

// Stats 是一次规划的质量指标。
type Stats struct {
	Points   int     `json:"points"`
	Spans    int     `json:"spans"`
	Coverage float64 `json:"coverage"` // 有效寄存器数 / 覆盖寄存器数
}

// Build 生成批量读计划。
func Build(dev model.Device, opt Options) ([]driver.ReadSpan, Stats) {
	if opt.MaxGap <= 0 {
		opt.MaxGap = 8
	}
	refs := make([]driver.PointRef, 0, len(dev.Points))
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		refs = append(refs, driver.PointRefOf(dev, p))
	}
	if len(refs) == 0 {
		return nil, Stats{}
	}

	var spans []driver.ReadSpan
	regs := map[string][]driver.PointRef{}
	for _, r := range refs {
		if r.Address.Kind == model.AddressRegister {
			key := strings.ToLower(r.Address.Area)
			regs[key] = append(regs[key], r)
			continue
		}
	}
	areas := make([]string, 0, len(regs))
	for a := range regs {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, area := range areas {
		spans = append(spans, planRegisters(area, regs[area], opt)...)
	}
	spans = append(spans, planSymbols(refs)...)

	useful, covered := 0, 0
	for _, s := range spans {
		min, max := spanBounds(s)
		covered += max - min
		for _, p := range s.Points {
			useful += p.Length
		}
	}
	st := Stats{Points: len(refs), Spans: len(spans)}
	if covered > 0 {
		st.Coverage = float64(useful) / float64(covered)
	}
	return spans, st
}

func spanBounds(s driver.ReadSpan) (int, int) {
	if len(s.Points) == 0 {
		return 0, 0
	}
	min := s.Points[0].Address.Offset
	max := min + s.Points[0].Length
	for _, p := range s.Points[1:] {
		if p.Address.Offset < min {
			min = p.Address.Offset
		}
		if e := p.Address.Offset + p.Length; e > max {
			max = e
		}
	}
	return min, max
}

func isBitArea(area string) bool {
	switch strings.ToLower(area) {
	case "coil", "coils", "discrete", "discrete_input", "discreteinput":
		return true
	}
	return false
}

func planRegisters(area string, refs []driver.PointRef, opt Options) []driver.ReadSpan {
	sort.SliceStable(refs, func(i, j int) bool {
		return refs[i].Address.Offset < refs[j].Address.Offset
	})
	limit := 125
	maxGap := opt.MaxGap
	if isBitArea(area) {
		limit = 2000
		maxGap = opt.MaxGap * 16
	}
	var out []driver.ReadSpan
	var cur []driver.PointRef
	curStart, curEnd := 0, 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, driver.ReadSpan{
			ID:     fmt.Sprintf("reg:%s:%d-%d", area, curStart, curEnd),
			Points: append([]driver.PointRef(nil), cur...),
		})
		cur = nil
	}
	for _, r := range refs {
		l := r.Length
		if isBitArea(area) {
			l = 1
		}
		s, e := r.Address.Offset, r.Address.Offset+l
		if len(cur) == 0 {
			cur, curStart, curEnd = []driver.PointRef{r}, s, e
			continue
		}
		if s <= curEnd+maxGap && e-curStart <= limit {
			if e > curEnd {
				curEnd = e
			}
			cur = append(cur, r)
			continue
		}
		flush()
		cur, curStart, curEnd = []driver.PointRef{r}, s, e
	}
	flush()
	return out
}

func planSymbols(refs []driver.PointRef) []driver.ReadSpan {
	groups := map[string][]driver.PointRef{}
	order := []string{}
	for _, r := range refs {
		var key string
		switch r.Address.Kind {
		case model.AddressSymbol:
			key = "sym:" + symbolRoot(r.Address.Path)
		case model.AddressObject:
			key = fmt.Sprintf("obj:%s:%d", r.Address.ObjectType, r.Address.Instance)
		case model.AddressFile:
			key = fmt.Sprintf("file:%d:%d", r.Address.FileID, r.Address.RecordID)
		default:
			continue
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}
	sort.Strings(order)
	var out []driver.ReadSpan
	for _, k := range order {
		out = append(out, driver.ReadSpan{ID: k, Points: groups[k]})
	}
	return out
}

// symbolRoot 取符号路径的最后一段之前的公共根。
// 例如 "DB1.DBW0" -> "DB1"；"Program:Main.Tag" -> "Program:Main"。
func symbolRoot(p string) string {
	if i := strings.LastIndexAny(p, ".:"); i > 0 {
		return p[:i]
	}
	return p
}
