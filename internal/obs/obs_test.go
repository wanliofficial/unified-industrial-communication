package obs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMetricsTextFormat(t *testing.T) {
	m := New()
	m.Declare("uic_test_total", "counter", "测试计数", nil)
	m.Inc("uic_test_total", Labels{"device": "d1"})
	m.Add("uic_test_total", 2, Labels{"device": "d1"})
	m.Set("uic_gauge", 3.5, Labels{"device": "d1"})
	m.Observe("uic_latency_seconds", 0.25, Labels{})

	var b bytes.Buffer
	if err := m.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# HELP uic_test_total 测试计数",
		"# TYPE uic_test_total counter",
		`uic_test_total{device="d1"} 3`,
		`uic_gauge{device="d1"} 3.5`,
		"# TYPE uic_latency_seconds histogram",
		`uic_latency_seconds_bucket{le="+Inf"} 1`,
		"uic_latency_seconds_sum 0.25",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if m.Value("uic_test_total", Labels{"device": "d1"}) != 3 {
		t.Fatal("value mismatch")
	}
}

// 标签顺序不影响同一个 series：否则指标会重复。
func TestMetricsLabelOrderIndependent(t *testing.T) {
	m := New()
	m.Inc("uic_x", Labels{"a": "1", "b": "2"})
	m.Inc("uic_x", Labels{"b": "2", "a": "1"})
	if got := m.Value("uic_x", Labels{"a": "1", "b": "2"}); got != 2 {
		t.Fatalf("value = %v, want 2", got)
	}
}

func TestMetricsEscaping(t *testing.T) {
	m := New()
	m.Set("uic_esc", 1, Labels{"path": `/api/v1/devices/:device`, "q": "a\"b"})
	var b bytes.Buffer
	m.WriteText(&b)
	if !strings.Contains(b.String(), `q="a\"b"`) {
		t.Fatalf("label not escaped:\n%s", b.String())
	}
}

func TestRingQuery(t *testing.T) {
	ring := NewRing(32)
	log := NewLogger(&bytes.Buffer{}, NewLevelVar("debug"), ring)
	log.Info("hello", "device", "d1", "n", 1)
	log.Warn("slow", "device", "d2")
	log.Error("boom", "device", "d1", "error", "timeout")

	if ring.Len() != 3 {
		t.Fatalf("len = %d", ring.Len())
	}
	if got := ring.Query("d1", "", 10); len(got) != 2 {
		t.Fatalf("d1 logs = %d", len(got))
	}
	if got := ring.Query("", "warn", 10); len(got) != 2 {
		t.Fatalf("warn+ logs = %d", len(got))
	}
	got := ring.Query("", "", 2)
	if len(got) != 2 {
		t.Fatalf("limit ignored: %d", len(got))
	}
	// Query 返回「尾部」且按时间正序：日志调用方通常要顺序阅读。
	if got[0].Message != "slow" || got[1].Message != "boom" {
		t.Fatalf("order = %+v", got)
	}
	if got[1].Attrs["device"] != "d1" || got[1].Attrs["error"] != "timeout" {
		t.Fatalf("attrs = %+v", got[1].Attrs)
	}
}

func TestRingEvictsOldest(t *testing.T) {
	ring := NewRing(4)
	log := NewLogger(&bytes.Buffer{}, NewLevelVar("debug"), ring)
	for i := 0; i < 10; i++ {
		log.Info("m", "i", i)
	}
	if ring.Len() != 4 {
		t.Fatalf("len = %d", ring.Len())
	}
	got := ring.Query("", "", 10)
	if len(got) != 4 {
		t.Fatalf("query = %d", len(got))
	}
}

func TestLevelVar(t *testing.T) {
	lv := NewLevelVar("INFO")
	if lv.Level() != slog.LevelInfo {
		t.Fatalf("level = %v", lv.Level())
	}
	lv.Set("debug")
	if lv.Level() != slog.LevelDebug {
		t.Fatal("debug should be enabled")
	}
	lv.Set("warning")
	if lv.Level() != slog.LevelWarn {
		t.Fatalf("warning alias = %v", lv.Level())
	}
	lv.Set("bogus")
	if lv.Level() != slog.LevelInfo {
		t.Fatalf("bogus should fall back to info, got %v", lv.Level())
	}
}

// 日志级别可变：调高后 debug 必须被过滤。
func TestLoggerLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	lv := NewLevelVar("info")
	log := NewLogger(&buf, lv, nil)
	log.Debug("hidden")
	log.Info("visible")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("debug leaked: %s", out)
	}
	if !strings.Contains(out, "visible") {
		t.Fatalf("info missing: %s", out)
	}
	lv.Set("debug")
	log.Debug("now-visible")
	if !strings.Contains(buf.String(), "now-visible") {
		t.Fatal("level change did not take effect")
	}
}

func TestMultiHandlerFansOut(t *testing.T) {
	ring := NewRing(8)
	var buf bytes.Buffer
	h := MultiHandler{NewLogger(&buf, NewLevelVar("info"), nil).Handler(), ring}
	log := slog.New(h)
	log.Info("fan", "device", "d1")
	if !strings.Contains(buf.String(), "fan") {
		t.Fatal("buffer handler missed record")
	}
	if ring.Len() != 1 {
		t.Fatalf("ring = %d", ring.Len())
	}
	if got := ring.Query("d1", "", 1); len(got) != 1 || got[0].Message != "fan" {
		t.Fatalf("ring query = %+v", got)
	}
}

func TestDriverAdapter(t *testing.T) {
	m := New()
	a := DriverAdapter{M: m}
	fn := a.Counter("uic_drv_connect_total", map[string]string{"device": "d1"})
	fn(1)
	fn(2)
	if got := m.Value("uic_drv_connect_total", Labels{"device": "d1"}); got != 3 {
		t.Fatalf("counter = %v", got)
	}
	a.Observe("uic_drv_latency", 0.5, map[string]string{"device": "d1"})
	var b bytes.Buffer
	m.WriteText(&b)
	if !strings.Contains(b.String(), `uic_drv_latency_sum{device="d1"} 0.5`) {
		t.Fatalf("observe missing:\n%s", b.String())
	}
}

func TestReset(t *testing.T) {
	m := New()
	m.Set("x", 1, Labels{"a": "b"})
	m.Reset()
	if v := m.Value("x", Labels{"a": "b"}); v != 0 {
		t.Fatalf("value after reset = %v", v)
	}
}
