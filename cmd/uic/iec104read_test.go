package main

import (
	"testing"

	iec104codec "uic/internal/codec/iec104"
	iec104drv "uic/internal/driver/iec104"
	"uic/internal/model"
)

func TestBuildIEC104WriteCommandAreas(t *testing.T) {
	cases := []struct {
		name  string
		area  string
		value string
		want  model.Value
		ioa   int
	}{
		{"single on", iec104drv.AreaSingleCommand, "on", model.BoolValue(true), 4001},
		{"single 0", iec104drv.AreaSingleCommand, "0", model.BoolValue(false), 4001},
		{"double on", iec104drv.AreaDoubleCommand, "on", model.NumValue(model.TypeUint16, 2), 2003},
		{"double 1", iec104drv.AreaDoubleCommand, "1", model.NumValue(model.TypeUint16, 1), 2003},
		{"setpoint float", iec104drv.AreaSetpointFloat, "12.5", model.NumValue(model.TypeFloat32, 12.5), 4002},
		{"setpoint scaled", iec104drv.AreaSetpointScaled, "-300", model.NumValue(model.TypeFloat32, -300), 4003},
		{"normalized", iec104drv.AreaSetpointNormalized, "0.25", model.NumValue(model.TypeFloat32, 0.25), 4004},
	}
	for _, c := range cases {
		req, err := buildIEC104Write(c.area, c.ioa, c.value)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if req.Address.Area != c.area || req.Address.Offset != c.ioa {
			t.Fatalf("%s: address = %+v", c.name, req.Address)
		}
		if req.DataType != c.want.Type {
			t.Fatalf("%s: dataType = %s, want %s", c.name, req.DataType, c.want.Type)
		}
		if req.Value.Type != c.want.Type || req.Value.Num != c.want.Num || req.Value.Bool != c.want.Bool {
			t.Fatalf("%s: value = %+v, want %+v", c.name, req.Value, c.want)
		}
	}
}

func TestBuildIEC104WriteRejections(t *testing.T) {
	cases := []struct {
		name  string
		area  string
		value string
	}{
		{"unknown area", "coil", "on"},
		{"single garbage", iec104drv.AreaSingleCommand, "maybe"},
		{"double zero", iec104drv.AreaDoubleCommand, "0"},
		{"double three", iec104drv.AreaDoubleCommand, "3"},
		{"setpoint garbage", iec104drv.AreaSetpointFloat, "abc"},
		{"scaled garbage", iec104drv.AreaSetpointScaled, ""},
	}
	for _, c := range cases {
		if _, err := buildIEC104Write(c.area, 1, c.value); err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
	}
}

func TestFormatObject(t *testing.T) {
	cases := []struct {
		name   string
		typeID byte
		obj    iec104codec.Object
		want   string
	}{
		{"single on", iec104codec.TypeSinglePoint, iec104codec.Object{Bool: true}, "on"},
		{"single off", iec104codec.TypeSinglePointTime, iec104codec.Object{Bool: false}, "off"},
		{"double off", iec104codec.TypeDoublePoint, iec104codec.Object{Uint: 1}, "off (1)"},
		{"double on", iec104codec.TypeDoublePointTime, iec104codec.Object{Uint: 2}, "on (2)"},
		{"double indeterminate", iec104codec.TypeDoublePoint, iec104codec.Object{Uint: 3}, "不确定 (3)"},
		{"float", iec104codec.TypeMeasuredFloat, iec104codec.Object{Num: 42.5}, "42.5"},
		{"float integral", iec104codec.TypeMeasuredFloat, iec104codec.Object{Num: 12}, "12"},
		{"normalized", iec104codec.TypeMeasuredNormalized, iec104codec.Object{Num: 0.5}, "0.50000"},
		// 标度化是整数语义：显示 1234 而不是 1.234e+03。
		{"scaled", iec104codec.TypeMeasuredScaled, iec104codec.Object{Num: 1234}, "1234"},
		{"scaled negative", iec104codec.TypeMeasuredScaledTime, iec104codec.Object{Num: -7}, "-7"},
		{"counter", iec104codec.TypeIntegratedTotals, iec104codec.Object{Uint: 900}, "900"},
	}
	for _, c := range cases {
		if got := formatObject(c.typeID, c.obj); got != c.want {
			t.Fatalf("%s: formatObject = %q, want %q", c.name, got, c.want)
		}
	}
}

// 品质位到「一个词」的压缩必须与网关点位质量同一套映射：同一个 SB 位
// 在自检里显示 uncertain，在 /api/v1/points 里也必须是 uncertain。
func TestQualityText(t *testing.T) {
	cases := []struct {
		q    iec104codec.Quality
		want string
	}{
		{0, "good"},
		{iec104codec.QSubstituted, "uncertain"},
		{iec104codec.QBlocked, "uncertain"},
		{iec104codec.QOverflow, "uncertain"},
		{iec104codec.QNotTopical, "stale"},
		{iec104codec.QInvalid, "bad"},
		// IV 优先于其他所有位。
		{iec104codec.QInvalid | iec104codec.QSubstituted, "bad"},
		// NT 优先于 SB。
		{iec104codec.QNotTopical | iec104codec.QBlocked, "stale"},
	}
	for _, c := range cases {
		if got := qualityText(c.q); got != c.want {
			t.Fatalf("qualityText(%v) = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"abc":       3,
		"短浮点测量值":    12,
		"on (2)":    6,
		"带时标短浮点测量值": 18,
		"CA=1":      4,
		"与abc":      5,
		"":          0,
	}
	for s, want := range cases {
		if got := displayWidth(s); got != want {
			t.Fatalf("displayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestFilterIOA(t *testing.T) {
	rows := []iec104Row{{IOA: 3}, {IOA: 1}, {IOA: 2}}
	got := filterIOA(rows, "1, 3")
	if len(got) != 2 || got[0].IOA != 1 || got[1].IOA != 3 {
		t.Fatalf("filtered = %+v", got)
	}
	// 空过滤条件不改变结果（由调用方决定是否调用，但函数本身要安全）。
	if len(filterIOA(rows, "")) != 3 {
		t.Fatal("empty filter must keep everything")
	}
}

func TestTrimFloat(t *testing.T) {
	cases := map[float64]string{
		2.5:   "2.5",
		12:    "12",
		-3.5:  "-3.5",
		0:     "0",
		1e20:  "1e+20",
		0.001: "0.001",
	}
	for f, want := range cases {
		if got := trimFloat(f); got != want {
			t.Fatalf("trimFloat(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestFormatStamp(t *testing.T) {
	if got := formatStamp(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
}
