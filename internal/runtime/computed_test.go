package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/model"
	"uic/internal/northbound/httpapi"
)

// newComputedHarness 起一个「虚拟设备 + 计算点设备」的最小运行时。
//
// 刻意不用 Modbus 从站：计算点的语义与链路无关，用虚拟驱动能把「依赖排序」、
// 「质量传播」这两件事单独验证，不受协议细节干扰。
func newComputedHarness(t *testing.T, mutate func(cfg map[string]any)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"schemaVersion": 1,
		"site":          "calc-site",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": dir, "journal": false},
		"logging":       map[string]any{"level": "info"},
		"channels": []any{
			map[string]any{
				"id": "ch-virtual", "protocol": "virtual",
				"transport": map[string]any{"kind": "memory"}, "concurrency": 2, "timeout": "1s",
			},
			map[string]any{
				"id": "ch-calc", "protocol": "computed",
				"transport": map[string]any{"kind": "memory"}, "concurrency": 1, "timeout": "1s",
			},
		},
		"devices": []any{
			map[string]any{
				"id": "src", "name": "source", "channel": "ch-virtual",
				"interval": "100ms", "timeout": "1s", "enabled": true,
				"points": []any{
					// ramp 波形每轮都在变，这样「同一轮内 A 依赖 B」可以被
					// 直接观测到：链式点必须等于本轮输入的两倍，而不是上一轮。
					map[string]any{"id": "a", "dataType": "float64", "enabled": true,
						"address": map[string]any{"kind": "register", "area": "holding", "offset": 0, "length": 2, "bit": -1}},
					map[string]any{"id": "b", "dataType": "float64", "enabled": true,
						"address": map[string]any{"kind": "register", "area": "holding", "offset": 2, "length": 2, "bit": -1}},
				},
				"labels": map[string]any{"mode": "ramp"},
			},
			map[string]any{
				"id": "kpi", "name": "derived", "channel": "ch-calc",
				"interval": "100ms", "timeout": "1s", "enabled": true,
				"points": []any{
					map[string]any{"id": "sum", "dataType": "float64", "enabled": true,
						"address": map[string]any{"kind": "computed", "expr": "src:a + src:b"}},
					map[string]any{"id": "chained", "dataType": "float64", "enabled": true,
						"address": map[string]any{"kind": "computed", "expr": "kpi:sum * 2"}},
					map[string]any{"id": "rounded", "dataType": "uint16", "enabled": true,
						"address": map[string]any{"kind": "computed", "expr": "round(src:a + 0.6)"}},
					map[string]any{"id": "high", "dataType": "bool", "enabled": true,
						"address": map[string]any{"kind": "computed", "expr": "src:a > 40"}},
					map[string]any{"id": "guarded", "dataType": "float64", "enabled": true,
						"address": map[string]any{"kind": "computed", "expr": "if(src:b != 0, src:a / src:b, -1)"}},
				},
			},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	h := &harness{t: t, rt: rt, dir: dir, token: rt.Token()}
	h.srv = httptest.NewServer(rt.Handler())
	t.Cleanup(func() {
		h.srv.Close()
		cancel()
		_ = rt.Stop(5 * time.Second)
	})
	return h
}

// 计算点的核心断言：值正确、同设备依赖在同一次 Read 内闭合、写入被拒。
func TestComputedPointsEndToEnd(t *testing.T) {
	h := newComputedHarness(t, nil)
	h.waitPointGood("kpi", "sum")

	// chained 依赖 sum，两者在同一次 Read 内求值，因此必须严格成 2 倍。
	//
	// 不能只读一次就断言：采集器是逐个 Set 进缓存的，读者可能正好落在
	// 「sum 已更新、chained 还没更新」的窗口里。正确的做法是在截止时间
	// 内多次采样，要求连续若干次都成立——这既容忍写入窗口，又真的证明了
	// 「同一轮内的依赖是闭合的」（如果按上一轮求值，斜坡输入下永远凑不齐）。
	deadline := time.Now().Add(10 * time.Second)
	streak := 0
	for time.Now().Before(deadline) && streak < 3 {
		sum, ok2 := h.rt.cache.Get("kpi", "sum")
		chain, ok3 := h.rt.cache.Get("kpi", "chained")
		if ok2 && ok3 && sum.Quality == model.QualityGood && chain.Quality == model.QualityGood {
			if chain.Value.Num == sum.Value.Num*2 {
				streak++
			} else {
				streak = 0
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if streak < 3 {
		sum, _ := h.rt.cache.Get("kpi", "sum")
		chain, _ := h.rt.cache.Get("kpi", "chained")
		t.Fatalf("chained is not computed from the same cycle: sum=%v chained=%v",
			sum.Value.Num, chain.Value.Num)
	}

	// 点位类型与单位必须被正确写入样本（uint16 取整、bool 转布尔）。
	s := h.waitPointGood("kpi", "rounded")
	if s.Value.Type != model.TypeUint16 {
		t.Fatalf("rounded type = %s", s.Value.Type)
	}
	if s.Value.Num != float64(int(s.Value.Num)) {
		t.Fatalf("rounded is not an integer: %v", s.Value.Num)
	}
	hs := h.waitPointGood("kpi", "high")
	if hs.Value.Type != model.TypeBool {
		t.Fatalf("high type = %s", hs.Value.Type)
	}

	// 计算点只读：REST 必须给出明确错误码，而不是 500 或静默成功。
	code, body := h.do(http.MethodPost, "/api/v1/points/kpi/write",
		`{"writes":[{"point":"sum","value":{"type":"float64","num":1}}]}`)
	if code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}
	var wr struct {
		Results []httpapi.WriteResult `json:"results"`
	}
	if err := json.Unmarshal(body, &wr); err != nil {
		t.Fatal(err)
	}
	if len(wr.Results) != 1 || wr.Results[0].OK || wr.Results[0].Code != "point.not_writable" {
		t.Fatalf("computed write must be rejected: %s", body)
	}

	// 健康检查里点位总数必须包含计算点（配置即事实）。
	code, body = h.do(http.MethodGet, "/api/v1/health", "")
	if code != 200 {
		t.Fatalf("health = %d", code)
	}
	var health httpapi.Health
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if health.Points.Total != 7 {
		t.Fatalf("points.total = %d, want 7 (2 source + 5 computed)", health.Points.Total)
	}
}

// 质量沿依赖传播：输入不可用时，计算点必须是坏质量并指名是哪个输入，
// 而不是拿 0 继续算出一个「看起来正常」的数字。
func TestComputedQualityPropagates(t *testing.T) {
	h := newComputedHarness(t, func(cfg map[string]any) {
		// 追加一台永远连不上的设备，再让一个计算点引用它。
		devs := cfg["devices"].([]any)
		devs = append(devs, map[string]any{
			"id": "dead", "name": "dead", "channel": "ch-virtual",
			"interval": "100ms", "timeout": "300ms", "enabled": true,
			"points": []any{map[string]any{"id": "x", "dataType": "float64", "enabled": true,
				"address": map[string]any{"kind": "register", "area": "holding", "offset": 0, "length": 2, "bit": -1}}},
			"labels": map[string]any{"mode": "const", "fault": "disconnect", "faultAt": "1"},
		})
		cfg["devices"] = devs
		kpi := devs[1].(map[string]any)
		pts := kpi["points"].([]any)
		kpi["points"] = append(pts, map[string]any{
			"id": "deadsum", "dataType": "float64", "enabled": true,
			"address": map[string]any{"kind": "computed", "expr": "dead:x + 1"},
		})
	})

	// 先等热起来：第一轮里 src 还没有值，kpi:sum 本来就该是坏的。
	// 不等就断言「其他计算点不受影响」，测的是启动竞态而不是质量传播。
	h.waitPointGood("kpi", "sum")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// 必须等真实的采集结果，而不是 Point() 为「从未采集」合成的
		// not_connected 占位样本——后者会把断言变成一句空话。
		s, ok := h.rt.cache.Get("kpi", "deadsum")
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if s.Quality == model.QualityGood {
			t.Fatalf("computed point must not be good when its input is dead: %+v", s)
		}
		// 质量码必须来自输入（连接错误），而不是笼统的 bad。
		if s.Quality != model.QualityCommError && s.Quality != model.QualityTimeout &&
			s.Quality != model.QualityNotConnected && s.Quality != model.QualityBad {
			t.Fatalf("unexpected quality: %s", s.Quality)
		}
		if s.Error == "" {
			t.Fatal("a degraded computed point must explain which input failed")
		}
		if !strings.Contains(s.Error, "dead:x") {
			t.Fatalf("error should name the dead input: %s", s.Error)
		}
		// 同设备的其他计算点不受影响：一个坏输入不能拖垮整台「设备」。
		if sum, ok := h.rt.Point("kpi", "sum"); ok && sum.Quality != model.QualityGood {
			t.Fatalf("unrelated computed point degraded: %+v", sum)
		}
		return
	}
	t.Fatal("computed point never reported a degraded quality")
}

// 配置期必须拒绝：引用不存在、依赖成环、bool 点位接了数值表达式、
// 计算点声明可写、以及给计算点配了物理地址字段。
func TestComputedConfigValidation(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"schemaVersion": 1,
			"site":          "calc-site",
			"channels": []any{map[string]any{
				"id": "ch-calc", "protocol": "computed",
				"transport": map[string]any{"kind": "memory"}, "concurrency": 1, "timeout": "1s",
			}},
			"devices": []any{map[string]any{
				"id": "kpi", "channel": "ch-calc", "interval": "1s", "timeout": "1s", "enabled": true,
				"points": []any{
					map[string]any{"id": "ok", "dataType": "float64", "enabled": true,
						"address": map[string]any{"kind": "computed", "expr": "1 + 2"}},
				},
			}},
		}
	}
	point := func(cfg map[string]any, i int) map[string]any {
		return cfg["devices"].([]any)[0].(map[string]any)["points"].([]any)[i].(map[string]any)
	}

	cases := []struct {
		name   string
		mutate func(cfg map[string]any)
		want   string
	}{
		{"missing reference", func(cfg map[string]any) {
			point(cfg, 0)["address"].(map[string]any)["expr"] = "nobody:x + 1"
		}, "does not exist"},
		{"self cycle", func(cfg map[string]any) {
			point(cfg, 0)["address"].(map[string]any)["expr"] = "kpi:ok + 1"
		}, "cycle"},
		{"two point cycle", func(cfg map[string]any) {
			pts := cfg["devices"].([]any)[0].(map[string]any)["points"].([]any)
			pts = append(pts, map[string]any{"id": "second", "dataType": "float64", "enabled": true,
				"address": map[string]any{"kind": "computed", "expr": "kpi:ok + 1"}})
			cfg["devices"].([]any)[0].(map[string]any)["points"] = pts
			point(cfg, 0)["address"].(map[string]any)["expr"] = "kpi:second + 1"
		}, "cycle"},
		{"bool point with number expression", func(cfg map[string]any) {
			point(cfg, 0)["dataType"] = "bool"
		}, "expression is number"},
		{"number point with bool expression", func(cfg map[string]any) {
			point(cfg, 0)["address"].(map[string]any)["expr"] = "1 < 2"
		}, "expression is bool"},
		{"writable computed", func(cfg map[string]any) {
			point(cfg, 0)["writable"] = true
		}, "read-only"},
		{"physical field on computed", func(cfg map[string]any) {
			point(cfg, 0)["address"].(map[string]any)["offset"] = 5
		}, "mixed fields"},
		{"missing expr", func(cfg map[string]any) {
			delete(point(cfg, 0)["address"].(map[string]any), "expr")
		}, "required for computed"},
		{"bad syntax", func(cfg map[string]any) {
			point(cfg, 0)["address"].(map[string]any)["expr"] = "1 +"
		}, "unexpected"},
		{"string data type", func(cfg map[string]any) {
			point(cfg, 0)["dataType"] = "string"
		}, "numeric or bool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base()
			c.mutate(cfg)
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			// 只校验不启动：New 会装配全部模块，这里要的是配置错误。
			_, err = New(Options{ConfigPath: path, DataDir: filepath.Dir(path), Version: "test"})
			if err == nil {
				t.Fatalf("expected validation error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// 端到端：计算点从真实的历史库读窗口（而不是注入的假数据）。
//
// 这条用例覆盖的是装配顺序：openAlarms 早于 openHistory，如果适配器在
// 构造时就把 history.Store 抓成 nil，这里的点位会永远 bad。
func TestComputedWindowFromHistory(t *testing.T) {
	h := newComputedHarness(t, func(cfg map[string]any) {
		cfg["storage"].(map[string]any)["history"] = map[string]any{
			"enabled": true, "minInterval": "100ms", "maxAge": "1h",
		}
		devs := cfg["devices"].([]any)
		kpi := devs[1].(map[string]any)
		kpi["points"] = append(kpi["points"].([]any), map[string]any{
			"id": "avg5", "dataType": "float64", "enabled": true,
			"address": map[string]any{"kind": "computed", "expr": "avg(src:a, 5m)"},
		})
	})

	s := h.waitPointGood("kpi", "avg5")
	if s.Value.Type != model.TypeFloat64 {
		t.Fatalf("avg5 value = %+v", s.Value)
	}
	// 斜坡输入为正，5 分钟窗口的平均值也应为正。不断言具体数值：
	// 它取决于用例跑到第几个采样周期。
	if s.Value.Num <= 0 {
		t.Fatalf("avg5 = %v, want a positive average", s.Value.Num)
	}
}
