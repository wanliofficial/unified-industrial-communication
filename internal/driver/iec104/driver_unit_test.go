package iec104

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/codec/iec104"
	"uic/internal/driver"
	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// 假链路：可以精确控制「对端说什么」，也能注入写失败。
// ---------------------------------------------------------------------------

type wireLink struct {
	mu       sync.Mutex
	in       chan []byte
	rest     []byte
	written  [][]byte
	closed   chan struct{}
	once     sync.Once
	writeErr error
	readErr  error
}

func newWireLink() *wireLink {
	return &wireLink{in: make(chan []byte, 32), closed: make(chan struct{})}
}

func (w *wireLink) Read(p []byte) (int, error) {
	if w.readErr != nil {
		return 0, w.readErr
	}
	w.mu.Lock()
	if len(w.rest) > 0 {
		n := copy(p, w.rest)
		w.rest = w.rest[n:]
		w.mu.Unlock()
		return n, nil
	}
	w.mu.Unlock()
	select {
	case b := <-w.in:
		w.mu.Lock()
		w.rest = b
		w.mu.Unlock()
		return w.Read(p)
	case <-w.closed:
		return 0, io.EOF
	}
}

func (w *wireLink) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.written = append(w.written, append([]byte(nil), p...))
	return len(p), nil
}

func (w *wireLink) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func (w *wireLink) SetReadDeadline(time.Time) error  { return nil }
func (w *wireLink) SetWriteDeadline(time.Time) error { return nil }

func (w *wireLink) feed(b []byte) { w.in <- append([]byte(nil), b...) }

func (w *wireLink) frames() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.written...)
}

func (w *wireLink) last() []byte {
	f := w.frames()
	if len(f) == 0 {
		return nil
	}
	return f[len(f)-1]
}

// newTestConn 构造一个已经连上（但未 STARTDT）的 conn，绕开 Open 的网络
// 与握手步骤。接收循环与 keeper 需要调用方自行启动。
func newTestConn(t *testing.T, w *wireLink, dev model.Device) *conn {
	t.Helper()
	plans, byIOA, err := buildPlans(dev)
	if err != nil {
		t.Fatalf("buildPlans: %v", err)
	}
	c := &conn{
		link:     w,
		deviceID: dev.ID,
		ca:       1,
		timeout:  200 * time.Millisecond,
		testIdle: time.Hour,
		points:   plans,
		byIOA:    byIOA,
		values:   map[uint32]cachedValue{},
		done:     make(chan struct{}),
		ackCh:    make(chan struct{}, 1),
		started:  make(chan struct{}),
		pending:  map[uint32]chan iec104.ASDU{},
	}
	c.lastRecv.Store(time.Now().UnixNano())
	return c
}

func testDevice(points ...model.Point) model.Device {
	return model.Device{
		ID: "dev1", ChannelID: "ch", UnitID: 1, Enabled: true,
		Interval: model.Duration(time.Second),
		Points:   points,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s: condition never held", what)
}

func (c *conn) linkError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.linkErr
}

// ---------------------------------------------------------------------------
// numeric / doubleState / convert / qualityOf / worse
// ---------------------------------------------------------------------------

func TestNumeric(t *testing.T) {
	cases := []struct {
		v    model.Value
		want float64
		ok   bool
	}{
		{model.BoolValue(true), 1, true},
		{model.BoolValue(false), 0, true},
		{model.NumValue(model.TypeInt16, -5), -5, true},
		{model.NumValue(model.TypeFloat64, 1.25), 1.25, true},
		{model.TextValue(model.TypeString, "x"), 0, false},
		{model.Value{}, 0, false},
	}
	for _, c := range cases {
		got, ok := numeric(c.v)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("numeric(%+v) = %v/%v, want %v/%v", c.v, got, ok, c.want, c.ok)
		}
	}
}

func TestDoubleState(t *testing.T) {
	cases := []struct {
		v    model.Value
		want byte
		ok   bool
	}{
		{model.BoolValue(true), 2, true},
		{model.BoolValue(false), 1, true},
		{model.NumValue(model.TypeUint16, 1), 1, true},
		{model.NumValue(model.TypeUint16, 2), 2, true},
		// 0 与 3 在标准里是「不允许」，不是中间态，必须拒绝。
		{model.NumValue(model.TypeUint16, 0), 0, false},
		{model.NumValue(model.TypeUint16, 3), 0, false},
		{model.NumValue(model.TypeUint16, 4), 0, false},
		{model.NumValue(model.TypeFloat32, 1.5), 0, false},
		{model.NumValue(model.TypeUint16, -1), 0, false},
		{model.TextValue(model.TypeString, "on"), 0, false},
	}
	for _, c := range cases {
		got, ok := doubleState(c.v)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("doubleState(%+v) = %d/%v, want %d/%v", c.v, got, ok, c.want, c.ok)
		}
	}
}

func TestConvert(t *testing.T) {
	// 单点 → bool
	v, q, ok := convert(iec104.Object{Bool: true}, iec104.TypeSinglePoint, model.TypeBool)
	if !ok || !v.Bool || q != model.QualityGood {
		t.Fatalf("single/bool = %+v %s %v", v, q, ok)
	}
	// 双点 1/2 → bool
	for _, c := range []struct {
		raw  uint32
		want bool
	}{{1, false}, {2, true}} {
		v, q, ok = convert(iec104.Object{Uint: c.raw}, iec104.TypeDoublePoint, model.TypeBool)
		if !ok || !q.IsUsable() || v.Bool != c.want {
			t.Fatalf("double %d = %+v %s %v", c.raw, v, q, ok)
		}
	}
	// 双点 0/3 是中间态：值给 false，质量降为 uncertain。
	for _, raw := range []uint32{0, 3} {
		v, q, ok = convert(iec104.Object{Uint: raw}, iec104.TypeDoublePoint, model.TypeBool)
		if !ok || q != model.QualityUncertain || v.Bool {
			t.Fatalf("double %d = %+v %s %v", raw, v, q, ok)
		}
	}
	// bool 点位收到测量值：类型不匹配，必须报告失败而不是硬转。
	if _, _, ok := convert(iec104.Object{Num: 1}, iec104.TypeMeasuredFloat, model.TypeBool); ok {
		t.Fatal("measured value must not be silently accepted as bool")
	}
	// 非数值目标类型（字符串）不支持。
	if _, _, ok := convert(iec104.Object{Num: 1}, iec104.TypeMeasuredFloat, model.TypeString); ok {
		t.Fatal("string target must be rejected")
	}
	// 数值目标接受单点/双点/测量值/累计量。
	for _, tc := range []struct {
		obj    iec104.Object
		typeID byte
		want   float64
	}{
		{iec104.Object{Bool: true}, iec104.TypeSinglePoint, 1},
		{iec104.Object{Bool: false}, iec104.TypeSinglePoint, 0},
		{iec104.Object{Uint: 3}, iec104.TypeDoublePoint, 3},
		{iec104.Object{Num: 0.5}, iec104.TypeMeasuredNormalized, 0.5},
		{iec104.Object{Num: -12}, iec104.TypeMeasuredScaled, -12},
		{iec104.Object{Num: 2.5}, iec104.TypeMeasuredFloat, 2.5},
		{iec104.Object{Uint: 900}, iec104.TypeIntegratedTotals, 900},
		{iec104.Object{Bool: true}, iec104.TypeSinglePointTime, 1},
		{iec104.Object{Uint: 2}, iec104.TypeDoublePointTime, 2},
		{iec104.Object{Num: 0.25}, iec104.TypeMeasuredNormalizedTime, 0.25},
		{iec104.Object{Num: 7}, iec104.TypeMeasuredScaledTime, 7},
		{iec104.Object{Num: 3.5}, iec104.TypeMeasuredFloatTime, 3.5},
	} {
		v, _, ok := convert(tc.obj, tc.typeID, model.TypeFloat32)
		if !ok || v.Num != tc.want {
			t.Fatalf("convert %s = %+v %v, want %v", iec104.TypeName(tc.typeID), v, ok, tc.want)
		}
	}
	// 未知的数据类型标识。
	if _, _, ok := convert(iec104.Object{}, iec104.TypeInterrogation, model.TypeFloat32); ok {
		t.Fatal("command type must not convert to a value")
	}
}

func TestQualityOf(t *testing.T) {
	cases := []struct {
		in   iec104.Quality
		want model.Quality
	}{
		{0, model.QualityGood},
		{iec104.QInvalid, model.QualityBad},
		{iec104.QNotTopical, model.QualityStale},
		{iec104.QSubstituted, model.QualityUncertain},
		{iec104.QBlocked, model.QualityUncertain},
		{iec104.QOverflow, model.QualityUncertain},
		// IV 优先于一切：值不可信比「被取代」更严重。
		{iec104.QInvalid | iec104.QSubstituted, model.QualityBad},
		// NT 优先于 SB：过时优先于不确定。
		{iec104.QNotTopical | iec104.QBlocked, model.QualityStale},
	}
	for _, c := range cases {
		if got := qualityOf(c.in); got != c.want {
			t.Fatalf("qualityOf(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestWorse(t *testing.T) {
	order := []model.Quality{
		model.QualityGood, model.QualityUncertain, model.QualityStale, model.QualityBad,
	}
	for i, a := range order {
		for j, b := range order {
			want := a
			if j > i {
				want = b
			}
			if got := worse(a, b); got != want {
				t.Fatalf("worse(%s,%s) = %s, want %s", a, b, got, want)
			}
		}
	}
}

func TestWithinAck(t *testing.T) {
	if !withinAck(0, 0) || !withinAck(5, 10) {
		t.Fatal("normal ack must be accepted")
	}
	// 回绕：已发 2，对端确认 32767（即 -1），在模空间里是 1 帧之前。
	if !withinAck(0x7FFF, 2) {
		t.Fatal("wrap-around ack within window must be accepted")
	}
	// 确认一个远未发送的序号必须被拒。
	if withinAck(100, 10) {
		t.Fatal("ack beyond sent window must be rejected")
	}
}

// ---------------------------------------------------------------------------
// planPoint / buildPlans
// ---------------------------------------------------------------------------

func TestPlanPointAcceptsAllAreas(t *testing.T) {
	for _, area := range []string{
		AreaIOA, AreaSingleCommand, AreaDoubleCommand,
		AreaSetpointFloat, AreaSetpointScaled, AreaSetpointNormalized,
		"IOA", " Single-Command ",
	} {
		p, err := planPoint(driver.PointRef{
			ID: "p", DataType: model.TypeFloat32,
			Address: model.Address{Kind: model.AddressRegister, Area: area, Offset: 10, Bit: -1},
		})
		if err != nil {
			t.Fatalf("area %q: %v", area, err)
		}
		if p.ioa != 10 {
			t.Fatalf("ioa = %d", p.ioa)
		}
	}
}

func TestPlanPointRejections(t *testing.T) {
	base := func() driver.PointRef {
		return driver.PointRef{
			ID: "p", DataType: model.TypeFloat32,
			Address: model.Address{Kind: model.AddressRegister, Area: AreaIOA, Offset: 1, Bit: -1},
		}
	}
	cases := map[string]func(*driver.PointRef){
		"symbol kind": func(p *driver.PointRef) {
			p.Address = model.Address{Kind: model.AddressSymbol, Path: "x"}
		},
		"object kind": func(p *driver.PointRef) {
			p.Address = model.Address{Kind: model.AddressObject, ObjectType: "analog-input", Property: "present-value"}
		},
		"unknown area": func(p *driver.PointRef) { p.Address.Area = "coil" },
		"ioa zero":     func(p *driver.PointRef) { p.Address.Offset = 0 },
		"ioa negative": func(p *driver.PointRef) { p.Address.Offset = -1 },
		"ioa too big":  func(p *driver.PointRef) { p.Address.Offset = 0x1000000 },
		"length set":   func(p *driver.PointRef) { p.Address.Length = 1 },
		"bit set":      func(p *driver.PointRef) { p.Address.Bit = 3 },
		"bad type":     func(p *driver.PointRef) { p.DataType = model.TypeString },
		"bytes type":   func(p *driver.PointRef) { p.DataType = model.TypeBytes },
	}
	for name, mut := range cases {
		p := base()
		mut(&p)
		_, err := planPoint(p)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("%s: kind = %v", name, driver.KindOf(err))
		}
	}
}

func TestBuildPlansSkipsDisabled(t *testing.T) {
	dev := testDevice(
		pt("on", AreaIOA, 1, model.TypeFloat32, false),
		func() model.Point {
			p := pt("off", AreaIOA, 2, model.TypeFloat32, false)
			p.Enabled = false
			return p
		}(),
		// 同一个 IOA 两个点位必须都保留。
		pt("on2", AreaIOA, 1, model.TypeFloat64, false),
	)
	plans, byIOA, err := buildPlans(dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d", len(plans))
	}
	if _, ok := plans["off"]; ok {
		t.Fatal("disabled point must be skipped")
	}
	if len(byIOA[1]) != 2 {
		t.Fatalf("IOA 1 plans = %d", len(byIOA[1]))
	}
	// 一个非法点位必须让整个 buildPlans 失败，而不是被静默丢弃。
	bad := testDevice(pt("x", "nope", 1, model.TypeFloat32, false))
	if _, _, err := buildPlans(bad); err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// buildCommand：五种命令 + 三类错误
// ---------------------------------------------------------------------------

func TestBuildCommandAllAreas(t *testing.T) {
	cases := []struct {
		name   string
		area   string
		dt     model.DataType
		value  model.Value
		typeID byte
	}{
		{"single on", AreaSingleCommand, model.TypeBool, model.BoolValue(true), iec104.TypeSingleCommand},
		{"single off", AreaSingleCommand, model.TypeBool, model.BoolValue(false), iec104.TypeSingleCommand},
		{"double bool", AreaDoubleCommand, model.TypeBool, model.BoolValue(true), iec104.TypeDoubleCommand},
		{"double num", AreaDoubleCommand, model.TypeUint16, model.NumValue(model.TypeUint16, 2), iec104.TypeDoubleCommand},
		{"setpoint float", AreaSetpointFloat, model.TypeFloat32, model.NumValue(model.TypeFloat32, 1.5), iec104.TypeSetpointFloat},
		{"setpoint scaled", AreaSetpointScaled, model.TypeInt16, model.NumValue(model.TypeInt16, -300), iec104.TypeSetpointScaled},
		{"setpoint normalized", AreaSetpointNormalized, model.TypeFloat32, model.NumValue(model.TypeFloat32, 0.5), iec104.TypeSetpointNormalized},
		// area=ioa 时按 dataType 推导。
		{"auto bool", AreaIOA, model.TypeBool, model.BoolValue(true), iec104.TypeSingleCommand},
		{"auto float", AreaIOA, model.TypeFloat32, model.NumValue(model.TypeFloat32, 2), iec104.TypeSetpointFloat},
		{"auto int", AreaIOA, model.TypeInt32, model.NumValue(model.TypeInt32, 9), iec104.TypeSetpointScaled},
	}
	for _, c := range cases {
		plan := &pointPlan{ioa: 100, area: c.area, dataType: c.dt}
		cmd, err := buildCommand(plan, driver.WriteRequest{
			PointID: "p", DataType: c.dt, Value: c.value,
		}, 7)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a, err := iec104.ParseASDU(cmd.asdu)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		if a.TypeID != c.typeID {
			t.Fatalf("%s: type = %s, want %s", c.name,
				iec104.TypeName(a.TypeID), iec104.TypeName(c.typeID))
		}
		if a.CommonAddr != 7 {
			t.Fatalf("%s: CA = %d", c.name, a.CommonAddr)
		}
		if len(a.Objects) != 1 || a.Objects[0].IOA != 100 {
			t.Fatalf("%s: objects = %+v", c.name, a.Objects)
		}
		if !a.Objects[0].Select == false {
			t.Fatalf("%s: select must be false", c.name)
		}
	}
}

func TestBuildCommandRejections(t *testing.T) {
	cases := map[string]struct {
		plan  *pointPlan
		value model.Value
		want  string
	}{
		"single without bool": {
			&pointPlan{ioa: 1, area: AreaSingleCommand, dataType: model.TypeBool},
			model.NumValue(model.TypeFloat32, 1), "single command requires bool",
		},
		"double bad state": {
			&pointPlan{ioa: 1, area: AreaDoubleCommand, dataType: model.TypeUint16},
			model.NumValue(model.TypeUint16, 9), "double command requires",
		},
		"double state 0": {
			&pointPlan{ioa: 1, area: AreaDoubleCommand, dataType: model.TypeUint16},
			model.NumValue(model.TypeUint16, 0), "double command requires",
		},
		"setpoint without number": {
			&pointPlan{ioa: 1, area: AreaSetpointFloat, dataType: model.TypeFloat32},
			model.TextValue(model.TypeString, "x"), "setpoint requires numeric",
		},
		"normalized out of range": {
			&pointPlan{ioa: 1, area: AreaSetpointNormalized, dataType: model.TypeFloat32},
			model.NumValue(model.TypeFloat32, 1.5), "normalized setpoint",
		},
		"scaled out of int16": {
			&pointPlan{ioa: 1, area: AreaSetpointScaled, dataType: model.TypeInt16},
			model.NumValue(model.TypeInt32, 40000), "out of int16 range",
		},
		"scaled non-numeric": {
			&pointPlan{ioa: 1, area: AreaSetpointScaled, dataType: model.TypeInt16},
			model.TextValue(model.TypeString, "x"), "setpoint requires numeric",
		},
	}
	for name, c := range cases {
		_, err := buildCommand(c.plan, driver.WriteRequest{
			PointID: "p", DataType: c.plan.dataType, Value: c.value,
		}, 1)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("%s: kind = %v", name, driver.KindOf(err))
		}
	}
}

// 写入时缓存里的值必须是「补齐类型后的请求值」，而不是原始输入。
func TestBuildCommandNormalisesCachedValue(t *testing.T) {
	plan := &pointPlan{ioa: 1, area: AreaSetpointScaled, dataType: model.TypeInt16}
	cmd, err := buildCommand(plan, driver.WriteRequest{
		PointID: "p", DataType: model.TypeInt16,
		Value: model.NumValue(model.TypeFloat64, 12),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.value.Type != model.TypeInt16 {
		t.Fatalf("cached value type = %s", cmd.value.Type)
	}
}

// ---------------------------------------------------------------------------
// negativeConfirm：错误分类
// ---------------------------------------------------------------------------

func TestNegativeConfirmClassification(t *testing.T) {
	cases := map[byte]driver.ErrorKind{
		iec104.CauseUnknownType:       driver.ErrProtocol,
		iec104.CauseUnknownCause:      driver.ErrProtocol,
		iec104.CauseUnknownCommonAddr: driver.ErrAddress,
		iec104.CauseUnknownIOA:        driver.ErrAddress,
		99:                            driver.ErrPermission,
	}
	for cause, want := range cases {
		err := negativeConfirm("d", 42, iec104.ASDU{Cause: cause})
		if driver.KindOf(err) != want {
			t.Fatalf("cause %d: kind = %v, want %v", cause, driver.KindOf(err), want)
		}
		if err.Error() == "" {
			t.Fatalf("cause %d: empty message", cause)
		}
	}
}

// ---------------------------------------------------------------------------
// 帧分发
// ---------------------------------------------------------------------------

func TestDispatchStoresValues(t *testing.T) {
	dev := testDevice(
		pt("f", AreaIOA, 1001, model.TypeFloat32, false),
		pt("b", AreaIOA, 2001, model.TypeBool, false),
	)
	w := newWireLink()
	c := newTestConn(t, w, dev)

	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 1001, Num: 3.5}},
	})
	c.dispatch(raw)
	c.mu.Lock()
	v, ok := c.values[1001]
	c.mu.Unlock()
	if !ok || v.value.Num != 3.5 {
		t.Fatalf("value = %+v/%v", v, ok)
	}

	// 不同公共地址的帧必须被忽略。
	raw, _ = iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous, CommonAddr: 9,
		Objects: []iec104.Object{{IOA: 1001, Num: 99}},
	})
	c.dispatch(raw)
	c.mu.Lock()
	v = c.values[1001]
	c.mu.Unlock()
	if v.value.Num == 99 {
		t.Fatal("frame for another common address must be ignored")
	}

	// 未配置的 IOA 不产生缓存条目，也不报错。
	raw, _ = iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 7777, Num: 1}},
	})
	c.dispatch(raw)
	c.mu.Lock()
	_, ok = c.values[7777]
	c.mu.Unlock()
	if ok {
		t.Fatal("unconfigured IOA must not be cached")
	}
}

// 类型对不上时保留旧值，不要写成一个错误类型但看起来正常的值。
func TestDispatchKeepsOldValueOnTypeMismatch(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	c.mu.Lock()
	c.values[1001] = cachedValue{value: model.NumValue(model.TypeFloat32, 1), q: model.QualityGood}
	c.mu.Unlock()

	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeSinglePoint, Cause: iec104.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 1001, Bool: true}},
	})
	c.dispatch(raw)
	c.mu.Lock()
	v := c.values[1001]
	c.mu.Unlock()
	// 单点 → 数值是允许的转换（true=1），所以这里应当更新成 1。
	if v.value.Num != 1 {
		t.Fatalf("value = %+v", v.value)
	}

	// bool 点位收到测量值：没有有损转换可用，缓存里不得出现条目。
	dev2 := testDevice(pt("b", AreaIOA, 1002, model.TypeBool, false))
	c2 := newTestConn(t, newWireLink(), dev2)
	raw, _ = iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 1002, Num: 5}},
	})
	c2.dispatch(raw)
	c2.mu.Lock()
	_, ok := c2.values[1002]
	c2.mu.Unlock()
	if ok {
		t.Fatal("mismatched type must not be cached")
	}
}

// 解析失败的 ASDU 只计数，不断链；带试验位的命令原样回确认为激活确认。
func TestDispatchTestBitEcho(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false))
	w := newWireLink()
	c := newTestConn(t, w, dev)

	// 坏 ASDU。
	c.dispatch([]byte{iec104.TypeSinglePoint, 0x03})
	c.mu.Lock()
	bad := c.badFrames
	c.mu.Unlock()
	if bad != 1 {
		t.Fatalf("badFrames = %d", bad)
	}

	// 带 T 位的命令必须回激活确认（且 T 位清零、不执行）。
	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseActivation, Test: true,
		CommonAddr: 1, Objects: []iec104.Object{{IOA: 4001, Bool: true}},
	})
	c.dispatch(raw)
	waitFor(t, "test-bit echo", func() bool { return len(w.frames()) > 0 })
	out, err := iec104.ParseFrame(w.last())
	if err != nil {
		t.Fatalf("echo frame: %v", err)
	}
	a, err := iec104.ParseASDU(out.ASDU)
	if err != nil {
		t.Fatalf("echo asdu: %v", err)
	}
	if a.Cause != iec104.CauseActivationCon || a.Test || a.Negative {
		t.Fatalf("echo = %+v", a)
	}
}

// EndOfInit：从站重启后主站必须重新总召唤。
func TestDispatchEndOfInitTriggersInterrogation(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false))
	w := newWireLink()
	c := newTestConn(t, w, dev)

	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeEndOfInit, Cause: iec104.CauseInitialized, CommonAddr: 1,
		Objects: []iec104.Object{{Qualifier: 0}},
	})
	c.dispatch(raw)
	waitFor(t, "re-interrogation", func() bool {
		for _, f := range w.frames() {
			fr, err := iec104.ParseFrame(f)
			if err != nil || len(fr.ASDU) == 0 {
				continue
			}
			a, err := iec104.ParseASDU(fr.ASDU)
			if err == nil && a.TypeID == iec104.TypeInterrogation {
				return true
			}
		}
		return false
	})
}

// 命令确认必须送到等待它的 writeOne。
func TestDispatchDeliversCommandConfirmation(t *testing.T) {
	dev := testDevice(pt("c", AreaSingleCommand, 4001, model.TypeBool, true))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	ch := make(chan iec104.ASDU, 1)
	c.mu.Lock()
	c.pending[4001] = ch
	c.mu.Unlock()

	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseActivationCon, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 4001, Bool: true}},
	})
	c.dispatch(raw)
	select {
	case a := <-ch:
		if a.Cause != iec104.CauseActivationCon {
			t.Fatalf("asdu = %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation was not delivered")
	}

	// 没有等待者的命令确认必须被静默丢弃，不得阻塞接收循环。
	c.mu.Lock()
	delete(c.pending, 4001)
	c.mu.Unlock()
	c.dispatch(raw)
}

// 空对象表的命令确认不得 panic。
func TestDispatchCommandWithoutObjects(t *testing.T) {
	dev := testDevice(pt("c", AreaSingleCommand, 4001, model.TypeBool, true))
	c := newTestConn(t, newWireLink(), dev)
	raw := []byte{iec104.TypeSingleCommand, 0x00, 0x07, 0x00, 0x01, 0x00}
	c.dispatch(raw)
}

// 不是数据也不是命令、也没实现的类型：静默忽略。
func TestDispatchIgnoresUnknownDataFrame(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeClockSync, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Time: func() *time.Time { t := time.Now(); return &t }()}},
	})
	c.dispatch(raw)
	c.mu.Lock()
	_, cached := c.values[1]
	c.mu.Unlock()
	if cached {
		t.Fatal("clock sync must not produce a value")
	}
}

// ---------------------------------------------------------------------------
// handleFrame：U/S 帧分支
// ---------------------------------------------------------------------------

func TestHandleFrameSAndU(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false))
	w := newWireLink()
	c := newTestConn(t, w, dev)

	// S 帧确认未发送的序号：协议错。
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindS, RecvSeq: 10}); err == nil {
		t.Fatal("S frame beyond sent sequence must fail")
	}
	// S 帧确认已发送的序号：接受。
	c.sendSeq = 5
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindS, RecvSeq: 5}); err != nil {
		t.Fatalf("valid S frame: %v", err)
	}

	// STARTDT con 只生效一次。
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindU, UFunc: iec104.UStartCon}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.started:
	default:
		t.Fatal("STARTDT con did not signal")
	}

	// TESTFR act 必须回 con。
	before := len(w.frames())
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindU, UFunc: iec104.UTestAct}); err != nil {
		t.Fatal(err)
	}
	if len(w.frames()) != before+1 {
		t.Fatalf("no TESTFR con written")
	}

	// TESTFR con 唤醒 Probe 等待者。
	wait := make(chan error, 1)
	c.mu.Lock()
	c.testWait = wait
	c.mu.Unlock()
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindU, UFunc: iec104.UTestCon}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("waiter got %v", err)
		}
	default:
		t.Fatal("TESTFR con did not wake the waiter")
	}

	// STOPDT act：回 con 并让链路判死。
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindU, UFunc: iec104.UStopAct}); err == nil {
		t.Fatal("STOPDT act must terminate the link")
	}
	// STOPDT con：正常关闭流程，接受。
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindU, UFunc: iec104.UStopCon}); err != nil {
		t.Fatalf("STOPDT con: %v", err)
	}
	// 未知 U 功能码：协议错。
	if err := c.handleFrame(iec104.Frame{Kind: iec104.KindU, UFunc: 0x03}); err == nil {
		t.Fatal("unknown U function must fail")
	}
}

// ---------------------------------------------------------------------------
// sendASDU：写失败、窗口满
// ---------------------------------------------------------------------------

func TestSendASDUWriteFailure(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.link.(*wireLink).writeErr = errors.New("boom")
	asdu, _ := iec104.BuildInterrogation(1, iec104.QOIStation)
	err := c.sendASDU(asdu)
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("err = %v / %v", err, driver.KindOf(err))
	}
}

// 发送窗口满时必须等到确认，等不到就报超时（而不是把 15 位序号写穿）。
func TestSendASDUWindowFullTimesOut(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.timeout = 80 * time.Millisecond
	c.unacked = defaultK
	asdu, _ := iec104.BuildInterrogation(1, iec104.QOIStation)
	start := time.Now()
	err := c.sendASDU(asdu)
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v / %v", err, driver.KindOf(err))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("window wait took %v", elapsed)
	}
}

// 对端确认后窗口必须放行。
func TestSendASDUWindowReopensOnAck(t *testing.T) {
	w := newWireLink()
	c := newTestConn(t, w, testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.timeout = time.Second
	c.unacked = defaultK
	asdu, _ := iec104.BuildInterrogation(1, iec104.QOIStation)

	done := make(chan error, 1)
	go func() { done <- c.sendASDU(asdu) }()
	time.Sleep(30 * time.Millisecond)
	c.noteAck()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not resume after ack")
	}
	if c.sendSeq != 1 {
		t.Fatalf("sendSeq = %d", c.sendSeq)
	}
}

// 连接关闭时等待窗口的发送方必须立刻返回，而不是熬到超时。
func TestSendASDUWindowAbortsOnClose(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.timeout = 5 * time.Second
	c.unacked = defaultK
	asdu, _ := iec104.BuildInterrogation(1, iec104.QOIStation)
	done := make(chan error, 1)
	go func() { done <- c.sendASDU(asdu) }()
	time.Sleep(30 * time.Millisecond)
	close(c.done)
	select {
	case err := <-done:
		if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
			t.Fatalf("err = %v / %v", err, driver.KindOf(err))
		}
	case <-time.After(time.Second):
		t.Fatal("send did not abort on close")
	}
}

// 序号超范围（构造非法 N(S)）在编解码层就被拒，驱动转成内部错。
func TestSendASDURejectsBadSequence(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.sendSeq = 0x8000
	asdu, _ := iec104.BuildInterrogation(1, iec104.QOIStation)
	if err := c.sendASDU(asdu); err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// keeper：空闲探测与超时判死
// ---------------------------------------------------------------------------

func TestKeeperSendsTestFrameWhenIdle(t *testing.T) {
	w := newWireLink()
	c := newTestConn(t, w, testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.testIdle = 30 * time.Millisecond
	go c.keeper()
	defer c.Close()

	waitFor(t, "TESTFR act", func() bool {
		got := w.last()
		return len(got) >= 6 && got[2] == iec104.UTestAct
	})
}

// TESTFR 没有回应：keeper 必须在 t1 之后判死，而不是永远等。
func TestKeeperFailsWhenTestFrameUnanswered(t *testing.T) {
	w := newWireLink()
	c := newTestConn(t, w, testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.testIdle = 20 * time.Millisecond
	// 让「发出去的时刻」看起来已经过了 t1，省掉 15 秒等待。
	c.testOutstanding.Store(true)
	c.testAt.Store(time.Now().Add(-2 * defaultT1).UnixNano())
	go c.keeper()
	defer c.Close()

	waitFor(t, "link marked dead", func() bool { return c.linkError() != nil })
	if c.linkError() == nil {
		t.Fatal("keeper did not mark the link dead")
	}
}

// 收到 S 帧确认后 keeper 仍然要继续工作（不能因为一次确认卡住）。
func TestKeeperContinuesAfterAck(t *testing.T) {
	w := newWireLink()
	c := newTestConn(t, w, testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.testIdle = time.Hour
	go c.keeper()
	defer c.Close()

	// 让接收到的 I 帧累积一次待确认，keeper 应当发 S 帧。
	c.pendingAck.Add(1)
	waitFor(t, "S frame", func() bool {
		got := w.last()
		return len(got) >= 6 && got[2] == 0x01
	})
	if c.pendingAck.Load() != 0 {
		t.Fatalf("pendingAck = %d", c.pendingAck.Load())
	}
}

// ---------------------------------------------------------------------------
// 生命周期细节
// ---------------------------------------------------------------------------

// ctx 取消时 Open 的握手必须立刻返回，不能等满设备超时。
func TestOpenHonoursCancelledContext(t *testing.T) {
	hh := newH(t)
	hh.cfg.Channel.Timeout = model.Duration(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := hh.drv.Open(ctx, hh.cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("open took %v", elapsed)
	}
	// 取消发生在拨号阶段（链路还没建立），所以会被归类为断链；两者都
	// 是「不可能重试成功」的错误，上层行为一致。
	if k := driver.KindOf(err); k != driver.ErrTimeout && k != driver.ErrDisconnected {
		t.Fatalf("kind = %v", k)
	}
}

// Close 在链路已经死掉时也必须返回 nil（不能把「关不掉」传给上层）。
func TestCloseAfterLinkFailure(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	c.fail(errors.New("boom"))
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Probe(context.Background()); err == nil {
		t.Fatal("probe after failure must fail")
	}
}

// 链路错误后 Write 必须立刻返回断链，而不是等激活确认超时。
func TestWriteOnBrokenLink(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	c.fail(errors.New("boom"))
	start := time.Now()
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointFloat, Offset: 4002, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("err = %v / %v", err, driver.KindOf(err))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("write took %v", elapsed)
	}
}

// 地址非法时写必须在本地拒绝（不发报文）。
func TestWriteInvalidAddress(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "bad", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: "nope", Offset: 1, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 1),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if res[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s", res[0].Quality)
	}
}

// 设备用否定确认拒绝命令（未知类型/未知 IOA）时，错误分类必须可区分。
func TestWriteNegativeConfirmations(t *testing.T) {
	// 未知 IOA → 地址错。
	hh := newH(t)
	c := hh.open()
	hh.waitRead(c, "temp")
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "cmd", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 4001, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	_ = res
	_ = err
	// 用一个不存在的 IOA 走否定确认路径（4001 存在，改 9999）。
	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "ghost", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 9999, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
}

// 命令没有激活确认时写入必须超时。
func TestWriteTimesOutWithoutConfirmation(t *testing.T) {
	dev := testDevice(pt("c", AreaSingleCommand, 4001, model.TypeBool, true))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	c.timeout = 80 * time.Millisecond
	go c.readLoop()
	defer c.Close()

	start := time.Now()
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "c", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 4001, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if driver.KindOf(err) != driver.ErrTimeout || res[0].Quality != model.QualityTimeout {
		t.Fatalf("kind = %v quality = %s", driver.KindOf(err), res[0].Quality)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("write took %v", elapsed)
	}
}

// 写入成功时缓存必须被填充成「刚写下的值、质量 uncertain」，这样紧接着
// 的 read-back 确认不会读到上一轮的旧值。
func TestWriteUpdatesCacheBeforeReadBack(t *testing.T) {
	dev := testDevice(pt("sp", AreaSetpointFloat, 4002, model.TypeFloat32, true))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	go c.readLoop()
	defer c.Close()

	go func() {
		// 等命令帧出现，再回一条激活确认。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(w.frames()) > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		raw, _ := iec104.BuildASDU(iec104.ASDU{
			TypeID: iec104.TypeSetpointFloat, Cause: iec104.CauseActivationCon,
			CommonAddr: 1, Objects: []iec104.Object{{IOA: 4002, Num: 12.5}},
		})
		f, _ := iec104.BuildIFrame(0, 0, raw)
		w.feed(f)
	}()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointFloat, Offset: 4002, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 12.5),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	c.mu.Lock()
	cv, ok := c.values[4002]
	c.mu.Unlock()
	if !ok || cv.value.Num != 12.5 || cv.q != model.QualityUncertain {
		t.Fatalf("cache = %+v/%v", cv, ok)
	}
}

// 否定确认时缓存不得被污染。
func TestWriteNegativeDoesNotUpdateCache(t *testing.T) {
	dev := testDevice(pt("c", AreaSingleCommand, 4001, model.TypeBool, true))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	go c.readLoop()
	defer c.Close()

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(w.frames()) > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		raw, _ := iec104.BuildASDU(iec104.ASDU{
			TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseUnknownIOA, Negative: true,
			CommonAddr: 1, Objects: []iec104.Object{{IOA: 4001, Bool: true}},
		})
		f, _ := iec104.BuildIFrame(0, 0, raw)
		w.feed(f)
	}()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "c", DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSingleCommand, Offset: 4001, Bit: -1},
		Value:   model.BoolValue(true),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	c.mu.Lock()
	_, cached := c.values[4001]
	c.mu.Unlock()
	if cached {
		t.Fatal("failed write must not populate the cache")
	}
}

// ---------------------------------------------------------------------------
// inspect/诊断
// ---------------------------------------------------------------------------

func TestReadUnknownPointIsAddressError(t *testing.T) {
	hh := newH(t)
	c := hh.open()
	out, err := c.Read(context.Background(), []driver.ReadSpan{{
		Points: []driver.PointRef{{ID: "ghost"}},
	}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 1 || out[0].Quality != model.QualityAddressError {
		t.Fatalf("out = %+v", out)
	}
}

// 从站报告 IV 时值必须是 bad，并且带一条可诊断的错误。
func TestReadInvalidQuality(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false))
	c := newTestConn(t, newWireLink(), dev)
	c.mu.Lock()
	c.values[1001] = cachedValue{value: model.NumValue(model.TypeFloat32, 1), q: model.QualityBad}
	c.mu.Unlock()
	out, err := c.Read(context.Background(), []driver.ReadSpan{{
		Points: []driver.PointRef{{ID: "f"}},
	}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityBad || out[0].Err == nil {
		t.Fatalf("out = %+v", out[0])
	}
}

// 未收到任何数据的 IOA：bad + 明确提示「不在上送集里」。
func TestReadNeverSeenPoint(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false)))
	out, err := c.Read(context.Background(), []driver.ReadSpan{{
		Points: []driver.PointRef{{ID: "f"}},
	}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityBad || out[0].Err == nil {
		t.Fatalf("out = %+v", out[0])
	}
}

// Probe 在链路错误时立刻失败；成功后等待者被清空。
func TestProbeStates(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	c.timeout = 100 * time.Millisecond
	go c.readLoop()
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Probe(context.Background()) }()
	waitFor(t, "TESTFR act", func() bool {
		got := w.last()
		return len(got) >= 6 && got[2] == iec104.UTestAct
	})
	con, _ := iec104.BuildUFrame(iec104.UTestCon)
	w.feed(con)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not complete")
	}

	// 链路错误后再 Probe 必须快速失败。
	c.fail(errors.New("boom"))
	if err := c.Probe(context.Background()); err == nil {
		t.Fatal("probe on dead link must fail")
	}
}

// Probe 超时必须返回超时类错误。
func TestProbeTimeout(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false))
	c := newTestConn(t, newWireLink(), dev)
	c.timeout = 60 * time.Millisecond
	start := time.Now()
	err := c.Probe(context.Background())
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v / %v", err, driver.KindOf(err))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe took %v", elapsed)
	}
}

// ctx 取消让 Probe 立刻返回。
func TestProbeHonoursCancelledContext(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	c.timeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Probe(ctx); err == nil {
		t.Fatal("expected error")
	}
}

// 读循环遇到超长长度字段必须断链，而不是分配一块巨大缓冲。
func TestReadLoopRejectsBadLength(t *testing.T) {
	w := newWireLink()
	c := newTestConn(t, w, testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	go c.readLoop()
	defer c.Close()
	w.feed([]byte{iec104.StartByte, 0xFF})
	waitFor(t, "length rejected", func() bool { return c.linkError() != nil })
}

// 读循环遇到对端断开：记录断链而不是空转。
func TestReadLoopEndsOnEOF(t *testing.T) {
	w := newWireLink()
	c := newTestConn(t, w, testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	go c.readLoop()
	_ = w.Close()
	waitFor(t, "eof handled", func() bool { return c.linkError() != nil })
}

// 没接入的 METADATA 细节：确保 driver 的 Meta 里描述的命令类型齐全。
func TestMetaPointFieldsCoverAllAreas(t *testing.T) {
	m := New().Meta()
	var found bool
	for _, f := range m.PointFields {
		if f.Name != "address.area" {
			continue
		}
		found = true
		have := map[string]bool{}
		for _, e := range f.Enum {
			have[e] = true
		}
		for _, want := range []string{
			AreaIOA, AreaSingleCommand, AreaDoubleCommand,
			AreaSetpointFloat, AreaSetpointScaled, AreaSetpointNormalized,
		} {
			if !have[want] {
				t.Fatalf("address.area enum missing %q", want)
			}
		}
	}
	if !found {
		t.Fatal("address.area point field missing")
	}
}

// ---------------------------------------------------------------------------
// 旁路表：设备实际送了什么
// ---------------------------------------------------------------------------

// 旁路表与点位表是两条独立的路径：没配置的 IOA 也要记下来，否则「初次
// 接入一台设备」这个场景就永远需要先有一份正确的点表。
func TestObservedRecordsUnconfiguredIOA(t *testing.T) {
	dev := testDevice(pt("f", AreaIOA, 1001, model.TypeFloat32, false))
	c := newTestConn(t, newWireLink(), dev)

	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec104.Object{
			{IOA: 1001, Num: 3.5},
			{IOA: 7777, Num: 9},
		},
	})
	c.dispatch(raw)

	obs := c.Observed()
	if len(obs) != 2 {
		t.Fatalf("observed = %d, want 2", len(obs))
	}
	// 按 IOA 升序。
	if obs[0].IOA != 1001 || obs[1].IOA != 7777 {
		t.Fatalf("order = %d,%d", obs[0].IOA, obs[1].IOA)
	}
	if obs[0].TypeID != iec104.TypeMeasuredFloat || obs[0].Cause != iec104.CauseSpontaneous {
		t.Fatalf("obs[0] = %+v", obs[0])
	}
	// 已配置的 IOA 要标出映射到的点位；未配置的留空。
	if len(obs[0].Points) != 1 || obs[0].Points[0] != "f" {
		t.Fatalf("points = %v", obs[0].Points)
	}
	if len(obs[1].Points) != 0 {
		t.Fatalf("unconfigured points = %v", obs[1].Points)
	}
	// 保留原始对象：双点仍然是 0..3，不被压成布尔。
	if obs[1].Object.Num != 9 {
		t.Fatalf("raw object lost: %+v", obs[1].Object)
	}
}

// 同一个 IOA 上的多个点位必须全部列出来。
func TestObservedListsAllPointsOfIOA(t *testing.T) {
	dev := testDevice(
		pt("a", AreaIOA, 1001, model.TypeFloat32, false),
		pt("b", AreaIOA, 1001, model.TypeInt16, false),
		pt("cmd", AreaSingleCommand, 1001, model.TypeBool, true),
	)
	c := newTestConn(t, newWireLink(), dev)
	raw, _ := iec104.BuildASDU(iec104.ASDU{
		TypeID: iec104.TypeMeasuredScaled, Cause: iec104.CauseInterrogated, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 1001, Num: 7}},
	})
	c.dispatch(raw)

	obs := c.Observed()
	if len(obs) != 1 {
		t.Fatalf("observed = %d", len(obs))
	}
	if len(obs[0].Points) != 3 {
		t.Fatalf("points = %v", obs[0].Points)
	}
	if obs[0].Points[0] != "a" || obs[0].Points[1] != "b" || obs[0].Points[2] != "cmd" {
		t.Fatalf("points not sorted: %v", obs[0].Points)
	}
}

// 旁路表有上限：异常从站可以合法地送几百万个 IOA，无上限会把内存吃光。
func TestObservedIsBounded(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	now := time.Now()
	c.mu.Lock()
	for i := uint32(1); i <= maxObserved+50; i++ {
		c.observeLocked(iec104.ASDU{TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous},
			iec104.Object{IOA: i, Num: float64(i)}, now)
	}
	c.mu.Unlock()

	if got := len(c.Observed()); got != maxObserved {
		t.Fatalf("observed = %d, want %d", got, maxObserved)
	}
	if c.Dropped() != 50 {
		t.Fatalf("dropped = %d, want 50", c.Dropped())
	}
	// 已知 IOA 仍然可以继续更新（不是「满了就全废」）。
	c.mu.Lock()
	c.observeLocked(iec104.ASDU{TypeID: iec104.TypeMeasuredFloat, Cause: iec104.CauseSpontaneous},
		iec104.Object{IOA: 1, Num: 1.5}, now)
	c.mu.Unlock()
	if v := c.Observed()[0]; v.Object.Num != 1.5 {
		t.Fatalf("known IOA not updated: %+v", v)
	}
	if c.Dropped() != 50 {
		t.Fatalf("dropped changed on known IOA: %d", c.Dropped())
	}
}

// 空连接上 Observed 返回空切片而不是 nil 恐慌。
func TestObservedEmpty(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(pt("f", AreaIOA, 1, model.TypeFloat32, false)))
	if got := c.Observed(); len(got) != 0 {
		t.Fatalf("observed = %v", got)
	}
	if c.Dropped() != 0 {
		t.Fatalf("dropped = %d", c.Dropped())
	}
}

// 控制方向的点位在监视方向不存在：报错必须说清楚，而不是「还没收到」。
func TestReadControlDirectionPointExplainsItself(t *testing.T) {
	c := newTestConn(t, newWireLink(), testDevice(
		pt("sp", AreaSetpointFloat, 4002, model.TypeFloat32, true),
	))
	out, err := c.Read(context.Background(), []driver.ReadSpan{{Points: []driver.PointRef{{ID: "sp"}}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityBad || out[0].Err == nil {
		t.Fatalf("out = %+v", out[0])
	}
	if !strings.Contains(out[0].Err.Error(), "control direction") {
		t.Fatalf("err = %v", out[0].Err)
	}

	// 同一个 IOA 同时出现在监视方向时，恢复成「还没收到」。
	c2 := newTestConn(t, newWireLink(), testDevice(
		pt("sp", AreaSetpointFloat, 4002, model.TypeFloat32, true),
		pt("fb", AreaIOA, 4002, model.TypeFloat32, false),
	))
	out, err = c2.Read(context.Background(), []driver.ReadSpan{{Points: []driver.PointRef{{ID: "sp"}}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out[0].Err.Error(), "no data received") {
		t.Fatalf("err = %v", out[0].Err)
	}
}

// 命令确认到达之前，从站可能已经把新状态主动上送了（104 的突发上送常
// 紧跟激活确认，甚至先于本协程被唤醒）。回填缓存只能用请求值补「没有」
// 或「比命令更旧」的条目，不能覆盖一条带真实品质位的更新——104 只在变化
// 时上送，被覆盖的值可能很久都不会再来一条。
func TestWriteDoesNotOverwriteNewerSpontaneousValue(t *testing.T) {
	dev := testDevice(pt("sp", AreaSetpointFloat, 4002, model.TypeFloat32, true))
	w := newWireLink()
	c := newTestConn(t, w, dev)
	go c.readLoop()
	defer c.Close()

	go func() {
		// 等命令帧真的发出去：此刻 writeOne 已经记住 sentAt 并在等确认。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(w.frames()) > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		// 模拟「确认之前就已经收到的突发上送」：带 good 质量的新值。
		c.mu.Lock()
		c.values[4002] = cachedValue{
			value: model.NumValue(model.TypeFloat32, 20), q: model.QualityGood,
			at: time.Now(),
		}
		c.mu.Unlock()

		raw, _ := iec104.BuildASDU(iec104.ASDU{
			TypeID: iec104.TypeSetpointFloat, Cause: iec104.CauseActivationCon,
			CommonAddr: 1, Objects: []iec104.Object{{IOA: 4002, Num: 12.5}},
		})
		f, _ := iec104.BuildIFrame(0, 0, raw)
		w.feed(f)
	}()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: AreaSetpointFloat, Offset: 4002, Bit: -1},
		Value:   model.NumValue(model.TypeFloat32, 12.5),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	c.mu.Lock()
	cv := c.values[4002]
	c.mu.Unlock()
	if cv.value.Num != 20 {
		t.Fatalf("cache value = %v, want the spontaneous 20 (write must not clobber newer upstream)", cv.value.Num)
	}
	if cv.q != model.QualityGood {
		t.Fatalf("cache quality = %s, want good (real upstream quality must survive)", cv.q)
	}
}
