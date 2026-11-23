package iec60870

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// ASDU
// ---------------------------------------------------------------------------

func TestBuildParseRoundTripAllTypes(t *testing.T) {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 800*int(time.Millisecond), time.Local)
	cases := []ASDU{
		{TypeID: TypeSinglePoint, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 100, Bool: true, Quality: QSubstituted}}},
		{TypeID: TypeDoublePoint, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 101, Uint: 2}}},
		{TypeID: TypeStepPosition, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 102, Uint: 5, Quality: QSubstituted}}},
		{TypeID: TypeBitstring, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 103, Uint: 0x80000001}}},
		{TypeID: TypeMeasuredNormalized, Cause: CausePeriodic, CommonAddr: 7,
			Objects: []Object{{IOA: 200, Num: 0.5, Quality: QBlocked}}},
		{TypeID: TypeMeasuredScaled, Cause: CausePeriodic, CommonAddr: 7,
			Objects: []Object{{IOA: 201, Num: -1234}}},
		{TypeID: TypeMeasuredFloat, Cause: CauseBackground, CommonAddr: 7,
			Objects: []Object{{IOA: 202, Num: 42.5}}},
		{TypeID: TypeIntegratedTotals, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 300, Uint: 123456, Raw: 3}}},
		{TypeID: TypeIntegratedTotalsTime, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 301, Uint: 654321, Raw: 3, Time: &ts}}},
		{TypeID: TypeSinglePointTime, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 400, Bool: true, Time: &ts}}},
		{TypeID: TypeDoublePointTime, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 401, Uint: 1, Time: &ts}}},
		{TypeID: TypeMeasuredNormalizedTime, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 402, Num: -0.25, Time: &ts}}},
		{TypeID: TypeMeasuredScaledTime, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 403, Num: 99, Time: &ts}}},
		{TypeID: TypeMeasuredFloatTime, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 404, Num: 1.5, Time: &ts}}},
		{TypeID: TypeSingleCommand, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{IOA: 500, Bool: true, Select: true}}},
		{TypeID: TypeDoubleCommand, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{IOA: 501, Uint: 2}}},
		{TypeID: TypeStepCommand, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{IOA: 505, Uint: 2}}},
		{TypeID: TypeSetpointNormalized, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{IOA: 502, Num: -0.5}}},
		{TypeID: TypeSetpointScaled, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{IOA: 503, Num: -300}}},
		{TypeID: TypeSetpointFloat, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{IOA: 504, Num: 12.25}}},
		{TypeID: TypeEndOfInit, Cause: CauseInitialized, CommonAddr: 7,
			Objects: []Object{{Qualifier: 0}}},
		{TypeID: TypeInterrogation, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{Qualifier: QOIStation}}},
		{TypeID: TypeCounterInterrogation, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{Qualifier: 5}}},
		{TypeID: TypeClockSync, Cause: CauseActivation, CommonAddr: 7,
			Objects: []Object{{Time: &ts}}},
	}
	for _, in := range cases {
		raw, err := BuildASDU(in)
		if err != nil {
			t.Fatalf("%s: build: %v", TypeName(in.TypeID), err)
		}
		out, err := ParseASDU(raw)
		if err != nil {
			t.Fatalf("%s: parse: %v", TypeName(in.TypeID), err)
		}
		if out.TypeID != in.TypeID || out.Cause != in.Cause || out.CommonAddr != in.CommonAddr {
			t.Fatalf("%s: header mismatch %+v", TypeName(in.TypeID), out)
		}
		if len(out.Objects) != len(in.Objects) {
			t.Fatalf("%s: objects = %d", TypeName(in.TypeID), len(out.Objects))
		}
		for i := range in.Objects {
			if out.Objects[i].IOA != in.Objects[i].IOA && in.Objects[i].IOA != 0 {
				t.Fatalf("%s: ioa = %d", TypeName(in.TypeID), out.Objects[i].IOA)
			}
		}
	}
}

func TestMeasuredValueRoundTrip(t *testing.T) {
	raw, err := BuildASDU(ASDU{TypeID: TypeMeasuredFloat, Cause: CausePeriodic, CommonAddr: 1,
		Objects: []Object{{IOA: 1, Num: 42.5, Quality: QSubstituted | QNotTopical}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(out.Objects[0].Num-42.5) > 1e-6 {
		t.Fatalf("num = %v", out.Objects[0].Num)
	}
	if !out.Objects[0].Quality.Has(QSubstituted) || !out.Objects[0].Quality.Has(QNotTopical) {
		t.Fatalf("quality = %v", out.Objects[0].Quality)
	}
}

func TestNormalizedRangeClamped(t *testing.T) {
	raw, err := BuildASDU(ASDU{TypeID: TypeMeasuredNormalized, Cause: CausePeriodic, CommonAddr: 1,
		Objects: []Object{{IOA: 1, Num: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Objects[0].Num > 1.0 {
		t.Fatalf("normalized not clamped: %v", out.Objects[0].Num)
	}
	raw, err = BuildASDU(ASDU{TypeID: TypeMeasuredNormalized, Cause: CausePeriodic, CommonAddr: 1,
		Objects: []Object{{IOA: 1, Num: -5}}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = ParseASDU(raw)
	if out.Objects[0].Num < -1.0 {
		t.Fatalf("normalized not clamped: %v", out.Objects[0].Num)
	}
}

func TestSQConsecutiveIOA(t *testing.T) {
	a := ASDU{TypeID: TypeMeasuredFloat, Cause: CauseInterrogated, CommonAddr: 1, SQ: true,
		Objects: []Object{{IOA: 10, Num: 1}, {IOA: 11, Num: 2}, {IOA: 12, Num: 3}}}
	raw, err := BuildASDU(a)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !out.SQ || len(out.Objects) != 3 {
		t.Fatalf("parsed = %+v", out)
	}
	for i, o := range out.Objects {
		if o.IOA != uint32(10+i) {
			t.Fatalf("object %d ioa = %d", i, o.IOA)
		}
		if o.Num != float64(i+1) {
			t.Fatalf("object %d num = %v", i, o.Num)
		}
	}
	// SQ=1 但 IOA 不连续是编程错误。
	a.Objects[2].IOA = 99
	if _, err := BuildASDU(a); err == nil {
		t.Fatal("non-consecutive IOA with SQ=1 must be rejected")
	}
}

func TestBuildASDUValidation(t *testing.T) {
	ts := time.Now()
	cases := map[string]ASDU{
		"cause too big":      {TypeID: TypeSinglePoint, Cause: 100},
		"unknown type":       {TypeID: 200},
		"ioa too big":        {TypeID: TypeSinglePoint, Objects: []Object{{IOA: 0x1000000}}},
		"double point 4":     {TypeID: TypeDoublePoint, Objects: []Object{{Uint: 4}}},
		"scaled overflow":    {TypeID: TypeMeasuredScaled, Objects: []Object{{Num: 40000}}},
		"setpoint overflow":  {TypeID: TypeSetpointScaled, Objects: []Object{{Num: 1e9}}},
		"too many objects":   {TypeID: TypeSinglePoint, Objects: make([]Object, 128)},
		"normalized too big": {TypeID: TypeSetpointNormalized, Objects: []Object{{Num: 2}}},
		"double cmd 9":       {TypeID: TypeDoubleCommand, Objects: []Object{{Uint: 9}}},
		"_unused":            {TypeID: TypeClockSync, Objects: []Object{{Time: &ts}}},
	}
	delete(cases, "_unused")
	for name, a := range cases {
		if _, err := BuildASDU(a); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestParseASDURejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"short":             {0x01, 0x01},
		"unknown type":      {200, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00},
		"trailing bytes":    {TypeSinglePoint, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0xFF},
		"zero obj trailing": {TypeSinglePoint, 0x00, 0x03, 0x00, 0x01, 0x00, 0xFF},
		"truncated element": {TypeMeasuredFloat, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00},
		"truncated ioa":     {TypeSinglePointTime, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x00},
	}
	for name, b := range cases {
		if _, err := ParseASDU(b); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestElementSizeAndClassifiers(t *testing.T) {
	sizes := map[byte]int{
		TypeSinglePoint: 1, TypeDoublePoint: 1, TypeStepPosition: 2, TypeBitstring: 4,
		TypeMeasuredNormalized: 3,
		TypeMeasuredScaled:     3, TypeMeasuredFloat: 5, TypeIntegratedTotals: 5,
		TypeIntegratedTotalsTime: 12,
		TypeSinglePointTime:      8, TypeDoublePointTime: 8, TypeClockSync: 7,
		TypeMeasuredNormalizedTime: 10, TypeMeasuredScaledTime: 10,
		TypeMeasuredFloatTime: 12, TypeSingleCommand: 1, TypeDoubleCommand: 1,
		TypeStepCommand:        1,
		TypeSetpointNormalized: 3, TypeSetpointScaled: 3, TypeSetpointFloat: 5,
		TypeEndOfInit: 1, TypeInterrogation: 1, TypeCounterInterrogation: 1,
	}
	for typ, want := range sizes {
		got, ok := ElementSize(typ)
		if !ok || got != want {
			t.Fatalf("%s: size = %d/%v, want %d", TypeName(typ), got, ok, want)
		}
	}
	if _, ok := ElementSize(199); ok {
		t.Fatal("unknown type must not have a size")
	}
	for _, typ := range []byte{TypeInterrogation, TypeSingleCommand, TypeDoubleCommand, TypeStepCommand, TypeClockSync} {
		if !IsCommandType(typ) {
			t.Fatalf("%s must be a command", TypeName(typ))
		}
		if IsData(typ) {
			t.Fatalf("%s must not be data", TypeName(typ))
		}
	}
	for _, typ := range []byte{TypeSinglePoint, TypeMeasuredFloat, TypeIntegratedTotals, TypeIntegratedTotalsTime} {
		if !IsData(typ) || IsCommandType(typ) {
			t.Fatalf("%s classifier wrong", TypeName(typ))
		}
	}
	if !IsTimeTagged(TypeMeasuredFloatTime) || IsTimeTagged(TypeMeasuredFloat) {
		t.Fatal("IsTimeTagged wrong")
	}
	if !IsTimeTagged(TypeIntegratedTotalsTime) || IsTimeTagged(TypeIntegratedTotals) {
		t.Fatal("IsTimeTagged wrong for integrated totals")
	}
	if IsTimeTagged(TypeSingleCommand) {
		t.Fatal("commands are not time tagged")
	}
}

// ---------------------------------------------------------------------------
// 时标
// ---------------------------------------------------------------------------

func TestCP56Time2aRoundTrip(t *testing.T) {
	in := time.Date(2026, 11, 7, 13, 45, 12, 345*int(time.Millisecond), time.Local)
	b := EncodeCP56Time2a(in)
	if len(b) != 7 {
		t.Fatalf("len = %d", len(b))
	}
	out, ok := DecodeCP56Time2a(b)
	if !ok {
		t.Fatal("decode failed")
	}
	if !out.Equal(in.Truncate(time.Millisecond)) {
		t.Fatalf("out = %v, want %v", out, in)
	}
}

func TestCP56Time2aRejectsInvalid(t *testing.T) {
	// IV 位（分钟字节最高位）置位表示时间无效。
	b := EncodeCP56Time2a(time.Now())
	b[2] |= 0x80
	if _, ok := DecodeCP56Time2a(b); ok {
		t.Fatal("IV bit must invalidate the timestamp")
	}
	// 毫秒超范围
	b = EncodeCP56Time2a(time.Now())
	b[0], b[1] = 0xFF, 0xFF
	if _, ok := DecodeCP56Time2a(b); ok {
		t.Fatal("out-of-range milliseconds must be rejected")
	}
	// 月份 0
	b = EncodeCP56Time2a(time.Now())
	b[5] = 0
	if _, ok := DecodeCP56Time2a(b); ok {
		t.Fatal("month 0 must be rejected")
	}
	// 太短
	if _, ok := DecodeCP56Time2a([]byte{0}); ok {
		t.Fatal("short buffer must be rejected")
	}
	// nil 编码成 IV=1 的占位时标
	nb := encodeCP56Time2a(nil)
	if nb[2]&0x80 == 0 {
		t.Fatal("nil time must encode IV=1")
	}
}

func TestCP56Time2aSundayIsSeven(t *testing.T) {
	// 2026-11-08 是周日。
	sunday := time.Date(2026, 11, 8, 0, 0, 0, 0, time.Local)
	b := EncodeCP56Time2a(sunday)
	if dow := b[4] >> 5; dow != 7 {
		t.Fatalf("day of week = %d, want 7", dow)
	}
}

// 年字节只有低 7 位有定义，最高位是保留位。不掩掉它，一个把保留位置 1
// 的从站会让 2026 年变成 2126 年——而 2126 是语法上完全合法的时间，下游
// 没有任何理由拒绝它，于是「时标错了一百年」会静默进入历史库。
func TestCP56Time2aMasksYearReservedBit(t *testing.T) {
	in := time.Date(2026, 11, 7, 13, 45, 12, 0, time.Local)
	b := EncodeCP56Time2a(in)
	b[6] |= 0x80
	out, ok := DecodeCP56Time2a(b)
	if !ok {
		t.Fatal("a set reserved bit must not invalidate the timestamp")
	}
	if out.Year() != 2026 {
		t.Fatalf("year = %d, want 2026 (reserved bit must be masked)", out.Year())
	}
}

// time.Date 会把越界日期静默规范化（2 月 30 日 → 3 月 2 日）。那是把「设备
// 给了非法时间」改写成「一个合法但错误的时间」，必须判无效。
func TestCP56Time2aRejectsImpossibleDate(t *testing.T) {
	for _, mk := range []func([]byte){
		func(b []byte) { b[4] = 30; b[5] = 2 },            // 2 月 30 日
		func(b []byte) { b[4] = 31; b[5] = 4 },            // 4 月 31 日
		func(b []byte) { b[4] = 29; b[5] = 2; b[6] = 26 }, // 2026-02-29（非闰年）
		func(b []byte) { b[4] = 0 },                       // 日 0
		func(b []byte) { b[3] = 24 },                      // 时 24
	} {
		b := EncodeCP56Time2a(time.Date(2026, 11, 7, 13, 45, 12, 0, time.Local))
		mk(b)
		if _, ok := DecodeCP56Time2a(b); ok {
			t.Fatalf("impossible date/time accepted: % X", b)
		}
	}
}

// 归一化编解码必须用同一个分母（32768）。用 32767 编码会让每次读写往返
// 都缩小一点，反复往返后数值会持续爬向 0。
func TestNormalizedRoundTripIsStable(t *testing.T) {
	for _, want := range []float64{0, 0.5, -0.5, 1, -1, 0.123} {
		raw, err := BuildASDU(ASDU{TypeID: TypeMeasuredNormalized, Cause: CausePeriodic,
			CommonAddr: 1, Objects: []Object{{IOA: 1, Num: want}}})
		if err != nil {
			t.Fatalf("build %v: %v", want, err)
		}
		got, err := ParseASDU(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		v := got.Objects[0].Num
		if want == 1 {
			// +1 在 N/32768 定标下不可表示，上限是 1-2^-15。要求的是
			// 「接近 1 且不是负数」——旧实现在这里会回绕成 -1。
			if v <= 0.99 {
				t.Fatalf("normalized %v decoded to %v (sign flip on full scale?)", want, v)
			}
			continue
		}
		if math.Abs(v-want) > 1e-4 {
			t.Fatalf("normalized %v decoded to %v", want, v)
		}
		// 再往返一次，值必须稳定（不许持续漂移）。
		raw2, err := BuildASDU(ASDU{TypeID: TypeMeasuredNormalized, Cause: CausePeriodic,
			CommonAddr: 1, Objects: []Object{{IOA: 1, Num: v}}})
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		got2, err := ParseASDU(raw2)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		if got2.Objects[0].Num != v {
			t.Fatalf("value drifted across a round trip: %v -> %v", v, got2.Objects[0].Num)
		}
	}
}

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

func TestBuildInterrogation(t *testing.T) {
	raw, err := BuildInterrogation(3, QOIStation)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.TypeID != TypeInterrogation || a.Cause != CauseActivation || a.CommonAddr != 3 {
		t.Fatalf("asdu = %+v", a)
	}
	if a.Objects[0].Qualifier != QOIStation {
		t.Fatalf("qoi = %d", a.Objects[0].Qualifier)
	}
	if _, err := BuildInterrogation(1, 10); err == nil {
		t.Fatal("QOI < 20 must be rejected")
	}
	if _, err := BuildInterrogation(1, 40); err == nil {
		t.Fatal("QOI > 36 must be rejected")
	}
	if _, err := BuildCounterInterrogation(1, 5); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCommands(t *testing.T) {
	raw, err := BuildSingleCommand(2, 1000, true, true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	o := a.Objects[0]
	if !o.Bool || !o.Select || a.CommonAddr != 2 || o.IOA != 1000 {
		t.Fatalf("single command = %+v", o)
	}

	if _, err := BuildDoubleCommand(1, 1, 0, false); err == nil {
		t.Fatal("double command state 0 must be rejected")
	}
	if _, err := BuildDoubleCommand(1, 1, 3, false); err == nil {
		t.Fatal("double command state 3 must be rejected")
	}
	raw, err = BuildDoubleCommand(1, 5, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = ParseASDU(raw)
	if a.Objects[0].Uint != 2 {
		t.Fatalf("double command = %+v", a.Objects[0])
	}

	// 调节步命令：1=降档、2=升档，0/3 拒绝。
	if _, err := BuildStepCommand(1, 1, 0, false); err == nil {
		t.Fatal("step command direction 0 must be rejected")
	}
	if _, err := BuildStepCommand(1, 1, 3, false); err == nil {
		t.Fatal("step command direction 3 must be rejected")
	}
	raw, err = BuildStepCommand(1, 6, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = ParseASDU(raw)
	if a.Objects[0].Uint != 2 || a.TypeID != TypeStepCommand {
		t.Fatalf("step command = %+v", a.Objects[0])
	}

	if _, err := BuildSetpointNormalized(1, 1, 2, false); err == nil {
		t.Fatal("normalized setpoint out of range must be rejected")
	}
	if _, err := BuildSetpointScaled(1, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSetpointFloat(1, 1, 3.5, false); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildClockSync(1, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSetpointFloatPrecision(t *testing.T) {
	raw, err := BuildSetpointFloat(1, 7, 12.345, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(a.Objects[0].Num-12.345) > 1e-3 {
		t.Fatalf("num = %v", a.Objects[0].Num)
	}
}

// ---------------------------------------------------------------------------
// 命名
// ---------------------------------------------------------------------------

func TestTypeNameAndLookup(t *testing.T) {
	if TypeName(TypeMeasuredFloat) == "" || TypeName(199) == "" {
		t.Fatal("unknown types still need a readable name")
	}
	cases := map[string]byte{
		"1":                   TypeSinglePoint,
		"13":                  TypeMeasuredFloat,
		"single":              TypeSinglePoint,
		"float":               TypeMeasuredFloat,
		"m_me_nc_1":           TypeMeasuredFloat,
		"M_ME_NC_1":           TypeMeasuredFloat,
		"setpoint-normalized": TypeSetpointNormalized,
		"setpoint_normalized": TypeSetpointNormalized,
		"double-command":      TypeDoubleCommand,
		"step-command":        TypeStepCommand,
		"c_rc_na_1":           TypeStepCommand,
		"counter-time":        TypeIntegratedTotalsTime,
		"m_it_tb_1":           TypeIntegratedTotalsTime,
		"M_IT_TB_1":           TypeIntegratedTotalsTime,
	}
	for in, want := range cases {
		got, ok := TypeFromName(in)
		if !ok || got != want {
			t.Fatalf("TypeFromName(%q) = %d/%v, want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "-1", "300", "nope", "999"} {
		if _, ok := TypeFromName(bad); ok {
			t.Fatalf("TypeFromName(%q) must fail", bad)
		}
	}
	names := TypeNames()
	if len(names) == 0 {
		t.Fatal("TypeNames empty")
	}
}

func TestCauseName(t *testing.T) {
	cases := map[byte]string{
		CauseSpontaneous:   "突发",
		CauseActivationCon: "激活确认",
		CauseInterrogated:  "响应站召唤",
		CauseGroup1:        "响应第 1 组召唤",
		CauseGroup1 + 3:    "响应第 4 组召唤",
		CauseUnknownIOA:    "未知信息对象地址",
	}
	for c, want := range cases {
		if got := CauseName(c); got != want {
			t.Fatalf("CauseName(%d) = %q, want %q", c, got, want)
		}
	}
	if CauseName(60) == "" {
		t.Fatal("unknown cause still needs a readable name")
	}
}

func TestQualityString(t *testing.T) {
	if Quality(0).String() != "good" {
		t.Fatal("zero quality must read as good")
	}
	q := QInvalid | QBlocked
	s := q.String()
	if s != "blocked,invalid" {
		t.Fatalf("quality = %q", s)
	}
	if !q.Has(QInvalid) || q.Has(QOverflow) {
		t.Fatal("Has wrong")
	}
}

func TestASDUString(t *testing.T) {
	a := ASDU{TypeID: TypeSinglePoint, Cause: CauseSpontaneous, Negative: true, CommonAddr: 9,
		Objects: []Object{{IOA: 1}}}
	s := a.String()
	if s == "" {
		t.Fatal("String empty")
	}
}

// SIQ 的品质位整体左移 3 位：低 4 位是状态与保留位。
func TestSIQQualityShift(t *testing.T) {
	raw, err := BuildASDU(ASDU{TypeID: TypeSinglePoint, Cause: CauseSpontaneous, CommonAddr: 1,
		Objects: []Object{{IOA: 1, Bool: true, Quality: QInvalid}}})
	if err != nil {
		t.Fatal(err)
	}
	// ASDU 头 6 字节 + IOA 3 字节 + SIQ
	siq := raw[9]
	if siq&0x01 != 1 {
		t.Fatalf("state bit lost: %02X", siq)
	}
	if siq&0x80 == 0 {
		t.Fatalf("IV must land in bit 7: %02X", siq)
	}
}

// 累计量（BCR）第 5 字节里的 IV/CY/CA 是设备对「这个读数还能不能用来结算」
// 的明确表态。整体丢弃这一字节，等于把「无效的读数」翻译成 good。
func TestIntegratedTotalsKeepsBCRFlags(t *testing.T) {
	cases := []struct {
		bcr  byte
		want Quality
	}{
		{0x00, 0},
		{0x05, 0}, // 只有序号位，不是品质
		{0x80, QInvalid},
		{0x20, QOverflow},
		{0x40, QSubstituted},
		{0xE5, QInvalid | QOverflow | QSubstituted},
	}
	for _, c := range cases {
		// 直接构造报文元素：低 4 字节计数 + 第 5 字节 BCR 标志。
		var elem []byte
		elem = binary.LittleEndian.AppendUint32(elem, 1234)
		elem = append(elem, c.bcr)
		raw, err := BuildASDU(ASDU{TypeID: TypeIntegratedTotals, Cause: CauseSpontaneous,
			CommonAddr: 1, Objects: []Object{{IOA: 300, Uint: 1234, Raw: c.bcr & 0x1F,
				Quality: decodeBCRQuality(c.bcr)}}})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		// 编码侧的 BCR 字节必须与输入一致（标志位不能丢）。
		if got := raw[len(raw)-1]; got != c.bcr {
			t.Fatalf("bcr 0x%02X: encoded byte = 0x%02X", c.bcr, got)
		}
		out, err := ParseASDU(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		o := out.Objects[0]
		if o.Quality != c.want {
			t.Fatalf("bcr 0x%02X: quality = %q, want %q", c.bcr, o.Quality, c.want)
		}
		if o.Uint != 1234 {
			t.Fatalf("bcr 0x%02X: counter = %d", c.bcr, o.Uint)
		}
		if o.Raw&0x1F != c.bcr&0x1F {
			t.Fatalf("bcr 0x%02X: sequence bits = 0x%02X", c.bcr, o.Raw&0x1F)
		}
		// 再从设备原始字节解码一遍，确认解码侧认这三位。
		decoded, n, err := decodeElement(TypeIntegratedTotals, elem)
		if err != nil || n != 5 {
			t.Fatalf("bcr 0x%02X: decodeElement = %v, %d", c.bcr, err, n)
		}
		if decoded.Quality != c.want {
			t.Fatalf("bcr 0x%02X: decodeElement quality = %q, want %q", c.bcr, decoded.Quality, c.want)
		}
	}
}

// 带时标累计量 M_IT_TB_1（类型 32）= BCR（5 字节）+ CP56Time2a（7 字节）。
// 时标不改变取值，但**必须消费掉**：少读 7 字节会让同一 ASDU 里后续信息
// 对象的 IOA 整体前移，症状是「第一个电度对上、第二个电度开始串位」。
func TestIntegratedTotalsTimeKeepsTimeTag(t *testing.T) {
	ts1 := time.Date(2026, 11, 22, 8, 30, 15, 250*int(time.Millisecond), time.Local)
	ts2 := ts1.Add(15 * time.Minute)
	raw, err := BuildASDU(ASDU{TypeID: TypeIntegratedTotalsTime, Cause: CausePeriodic,
		CommonAddr: 1, Objects: []Object{
			{IOA: 300, Uint: 1000, Raw: 0x05, Time: &ts1},
			{IOA: 301, Uint: 0xFFFFFFFF, Raw: 0xE0, Quality: QInvalid | QOverflow | QSubstituted, Time: &ts2},
		}})
	if err != nil {
		t.Fatal(err)
	}
	// ASDU 头 6 字节 + 每个对象 (3 字节 IOA + 12 字节元素)。
	if want := 6 + 2*(3+12); len(raw) != want {
		t.Fatalf("frame = %d bytes, want %d", len(raw), want)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Objects) != 2 {
		t.Fatalf("objects = %d, want 2", len(out.Objects))
	}
	want := []struct {
		ioa   uint32
		value uint32
		raw   byte
		ts    time.Time
	}{
		{300, 1000, 0x05, ts1},
		{301, 0xFFFFFFFF, 0xE0, ts2},
	}
	for i, w := range want {
		o := out.Objects[i]
		if o.IOA != w.ioa || o.Uint != w.value {
			t.Fatalf("object %d = IOA %d value %d, want %d/%d", i, o.IOA, o.Uint, w.ioa, w.value)
		}
		if o.Raw != w.raw {
			t.Fatalf("object %d raw = 0x%02X, want 0x%02X", i, o.Raw, w.raw)
		}
		if o.Time == nil || !o.Time.Equal(w.ts) {
			t.Fatalf("object %d time = %v, want %v", i, o.Time, w.ts)
		}
	}
	// 解码侧也必须报告消费了 12 字节，否则调用方按 5 字节推进会串位。
	if _, n, err := decodeElement(TypeIntegratedTotalsTime, raw[6:]); err != nil || n != 12 {
		t.Fatalf("decodeElement = %d, %v; want 12", n, err)
	}
}

// IV 的累计量必须折算成 bad，而不是跟着「值不为 0」一起当 good 上报。
func TestIntegratedTotalsInvalidQualityConvertsToBad(t *testing.T) {
	o := Object{IOA: 300, Uint: 99, Raw: 0x80}
	o.Quality = decodeBCRQuality(0x80)
	_, q, ok := Convert(o, TypeIntegratedTotals, model.TypeUint32)
	if !ok {
		t.Fatal("convert failed")
	}
	if q != model.QualityBad {
		t.Fatalf("quality = %s, want bad", q)
	}
}

// 带时标累计量的取值与无时标版本一致：平台侧仍是一个 uint32 计数，
// 时标只用于旁路表展示，不能因为「多了时标」就被判成不认识的类型。
func TestIntegratedTotalsTimeConvertsLikeCounter(t *testing.T) {
	ts := time.Date(2026, 11, 22, 9, 0, 0, 0, time.Local)
	o := Object{IOA: 300, Uint: 4321, Raw: 0x01, Time: &ts}
	v, q, ok := Convert(o, TypeIntegratedTotalsTime, model.TypeUint32)
	if !ok {
		t.Fatal("convert failed")
	}
	if v.Type != model.TypeUint32 || v.Num != 4321 || q != model.QualityGood {
		t.Fatalf("value = %+v quality = %s", v, q)
	}
}

// 测量值的 QDS 品质位（[QDS] = OV(bit0)+保留(bit1..3)+BL(bit4)+SB(bit5)+
// NT(bit6)+IV(bit7)）必须按标准位置逐位解出。旧实现用 `Quality(b & 0x1F)`
// 读 bit0..4，把 IV 落在 bit7 整个漏掉——一个「无效」的模拟量会被报成
// good（fail-open），而 BL 落在 bit4 又被误读成 IV。这里用标准位置手拼
// QDS 字节，钉死每一位的折算结果。
func TestMeasuredQDSQualityBits(t *testing.T) {
	cases := []struct {
		name string
		qds  byte
		want model.Quality
	}{
		{"OV bit0", 0x01, model.QualityUncertain},
		{"BL bit4", 0x10, model.QualityUncertain},
		{"SB bit5", 0x20, model.QualityUncertain},
		{"NT bit6", 0x40, model.QualityStale},
		{"IV bit7", 0x80, model.QualityBad},
		{"IV+NT bit7|bit6", 0xC0, model.QualityBad}, // bad 压过 stale
	}
	for _, c := range cases {
		// M_ME_NA_1 元素：2 字节归一化值 + 1 字节 QDS。
		elem := []byte{0x00, 0x40, c.qds}
		o, n, err := decodeElement(TypeMeasuredNormalized, elem)
		if err != nil || n != 3 {
			t.Fatalf("%s: decodeElement = %v, %d", c.name, err, n)
		}
		if q := QualityOf(o.Quality); q != c.want {
			t.Fatalf("%s: QDS=0x%02X quality = %s, want %s", c.name, c.qds, q, c.want)
		}
	}

	// 编码侧必须把 IV 写回 bit7（不是 bit4）。旧实现 `Quality & 0x1F` 会
	// 把 QInvalid 写到 0x10（BL 的位置），与真实装置对不上。
	o := Object{Num: 0.5, Quality: QInvalid}
	b, err := appendElement(nil, TypeMeasuredNormalized, o)
	if err != nil {
		t.Fatal(err)
	}
	if b[2]&0x80 == 0 {
		t.Fatalf("IV must land in bit 7: QDS=0x%02X", b[2])
	}
	if b[2]&0x10 != 0 {
		t.Fatalf("IV must not leak into BL bit 4: QDS=0x%02X", b[2])
	}
	// 往返一次，IV 仍是 bad。
	back, _, err := decodeElement(TypeMeasuredNormalized, b)
	if err != nil {
		t.Fatal(err)
	}
	if q := QualityOf(back.Quality); q != model.QualityBad {
		t.Fatalf("round-trip IV quality = %s, want bad", q)
	}
}
