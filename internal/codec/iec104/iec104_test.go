package iec104

import (
	"bytes"
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// APCI 帧
// ---------------------------------------------------------------------------

func TestBuildAndParseIFrame(t *testing.T) {
	asdu := []byte{0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x14}
	frame, err := BuildIFrame(0, 0, asdu)
	if err != nil {
		t.Fatal(err)
	}
	// 0x68 长度=4+7 控制域四字节全零 ASDU
	want := append([]byte{0x68, 11, 0x00, 0x00, 0x00, 0x00}, asdu...)
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = % X, want % X", frame, want)
	}
	got, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindI || got.SendSeq != 0 || got.RecvSeq != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	if !bytes.Equal(got.ASDU, asdu) {
		t.Fatalf("asdu = % X", got.ASDU)
	}
}

func TestIFrameSequenceBitLayout(t *testing.T) {
	// N(S)=0x1234 时，控制域低 16 位应是 0x2468（左移一位）。
	frame, err := BuildIFrame(0x1234, 0x0ABC, []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	if frame[2] != 0x68 || frame[3] != 0x24 {
		t.Fatalf("send seq bytes = %02X %02X", frame[2], frame[3])
	}
	// N(R)=0x0ABC -> 0x1578
	if frame[4] != 0x78 || frame[5] != 0x15 {
		t.Fatalf("recv seq bytes = %02X %02X", frame[4], frame[5])
	}
	got, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.SendSeq != 0x1234 || got.RecvSeq != 0x0ABC {
		t.Fatalf("parsed seq = %d/%d", got.SendSeq, got.RecvSeq)
	}
}

func TestSequenceRangeChecked(t *testing.T) {
	if _, err := BuildIFrame(0x8000, 0, []byte{0x01}); err == nil {
		t.Fatal("send seq 0x8000 must be rejected")
	}
	if _, err := BuildIFrame(0, 0x8000, []byte{0x01}); err == nil {
		t.Fatal("recv seq 0x8000 must be rejected")
	}
	if _, err := BuildIFrame(0, 0, nil); err == nil {
		t.Fatal("I frame without ASDU must be rejected")
	}
	big := make([]byte, maxASDU+1)
	if _, err := BuildIFrame(0, 0, big); err == nil {
		t.Fatal("oversized ASDU must be rejected")
	}
	if _, err := BuildSFrame(0x8000); err == nil {
		t.Fatal("S frame seq 0x8000 must be rejected")
	}
}

func TestBuildAndParseSFrame(t *testing.T) {
	frame, err := BuildSFrame(5)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x68, 0x04, 0x01, 0x00, 0x0A, 0x00}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = % X", frame)
	}
	got, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindS || got.RecvSeq != 5 {
		t.Fatalf("parsed = %+v", got)
	}
}

func TestBuildAndParseUFrame(t *testing.T) {
	for _, fn := range []byte{UStartAct, UStartCon, UStopAct, UStopCon, UTestAct, UTestCon} {
		frame, err := BuildUFrame(fn)
		if err != nil {
			t.Fatalf("%s: %v", UName(fn), err)
		}
		got, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("%s: %v", UName(fn), err)
		}
		if got.Kind != KindU || got.UFunc != fn {
			t.Fatalf("%s parsed = %+v", UName(fn), got)
		}
	}
	if _, err := BuildUFrame(0x03); err == nil {
		t.Fatal("bare 0x03 is not a valid U function")
	}
}

func TestParseFrameRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"short":              {0x68},
		"bad start":          {0x67, 0x04, 0x01, 0x00, 0x00, 0x00},
		"length below 4":     {0x68, 0x03, 0x01, 0x00, 0x00, 0x00},
		"length above max":   {0x68, 0xFF, 0x01, 0x00, 0x00, 0x00},
		"length mismatch":    {0x68, 0x05, 0x01, 0x00, 0x00, 0x00},
		"unknown ctrl":       {0x68, 0x04, 0x02, 0x00, 0x00, 0x00},
		"U with extra bytes": {0x68, 0x04, 0x43, 0x00, 0x00, 0x01},
	}
	for name, b := range cases {
		if _, err := ParseFrame(b); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

// S 帧必须是 ctrl[0]==0x01 且 ctrl[1]==0；把 ctrl 高位置 1 会变成 I 帧。
func TestSFrameDiscrimination(t *testing.T) {
	f, err := ParseFrame([]byte{0x68, 0x04, 0x01, 0x00, 0x02, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindS || f.RecvSeq != 1 {
		t.Fatalf("parsed = %+v", f)
	}
	// ctrl[0]=0x03 -> U 帧
	f, err = ParseFrame([]byte{0x68, 0x04, 0x03, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindU || f.UFunc != 0x03 {
		t.Fatalf("parsed = %+v", f)
	}
	if kind := KindU.String(); kind != "U" {
		t.Fatalf("KindU.String = %q", kind)
	}
	if KindI.String() != "I" || KindS.String() != "S" || Kind(9).String() != "?" {
		t.Fatal("Kind.String coverage")
	}
}

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
		{TypeID: TypeMeasuredNormalized, Cause: CausePeriodic, CommonAddr: 7,
			Objects: []Object{{IOA: 200, Num: 0.5, Quality: QBlocked}}},
		{TypeID: TypeMeasuredScaled, Cause: CausePeriodic, CommonAddr: 7,
			Objects: []Object{{IOA: 201, Num: -1234}}},
		{TypeID: TypeMeasuredFloat, Cause: CauseBackground, CommonAddr: 7,
			Objects: []Object{{IOA: 202, Num: 42.5}}},
		{TypeID: TypeIntegratedTotals, Cause: CauseSpontaneous, CommonAddr: 7,
			Objects: []Object{{IOA: 300, Uint: 123456, Raw: 3}}},
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
		TypeSinglePoint: 1, TypeDoublePoint: 1, TypeMeasuredNormalized: 3,
		TypeMeasuredScaled: 3, TypeMeasuredFloat: 5, TypeIntegratedTotals: 5,
		TypeSinglePointTime: 8, TypeDoublePointTime: 8, TypeClockSync: 7,
		TypeMeasuredNormalizedTime: 10, TypeMeasuredScaledTime: 10,
		TypeMeasuredFloatTime: 12, TypeSingleCommand: 1, TypeDoubleCommand: 1,
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
	for _, typ := range []byte{TypeInterrogation, TypeSingleCommand, TypeClockSync} {
		if !IsCommandType(typ) {
			t.Fatalf("%s must be a command", TypeName(typ))
		}
		if IsData(typ) {
			t.Fatalf("%s must not be data", TypeName(typ))
		}
	}
	for _, typ := range []byte{TypeSinglePoint, TypeMeasuredFloat, TypeIntegratedTotals} {
		if !IsData(typ) || IsCommandType(typ) {
			t.Fatalf("%s classifier wrong", TypeName(typ))
		}
	}
	if !IsTimeTagged(TypeMeasuredFloatTime) || IsTimeTagged(TypeMeasuredFloat) {
		t.Fatal("IsTimeTagged wrong")
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
