package sl651

import (
	"errors"
	"testing"
	"time"
)

// 用 Builder 造一帧再解析，是「编码器与解析器互为逆运算」的最强回归：
// 两侧都错却能自洽的情况确实存在（例如长度字段两边都少数了 STX），
// 因此关键帧另用 io_test.go 的逐字节金向量锁死。
func mustReport(t *testing.T, b Builder, fn byte, serial uint16,
	rt, ot time.Time, fields []Field, volt []byte) *Report {
	t.Helper()
	station := [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}
	raw, err := b.BuildReport(fn, serial, rt, station, ClassRiver, ot, fields, volt)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	return r
}

func testBuilder() Builder {
	return Builder{
		Center:   1,
		Station:  [5]byte{0x00, 0x12, 0x34, 0x56, 0x78},
		Password: [2]byte{0x12, 0x34},
	}
}

// 定时报（32H）往返：多要素 + 电压，顺序与数值都要保持。
func TestReportTimedRoundTrip(t *testing.T) {
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	ot := time.Date(2024, 1, 11, 15, 45, 0, 0, time.UTC)

	z, err := NewField(GZ, 127) // 0.127 m
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewField(GP1, 5) // 0.5 mm
	if err != nil {
		t.Fatal(err)
	}
	q, err := NewField(GQ, 1234567) // 1234.567 m³/s
	if err != nil {
		t.Fatal(err)
	}
	r := mustReport(t, testBuilder(), FuncTimed, 0x0003, rt, ot,
		[]Field{p, z, q}, []byte{0x11, 0x15})

	if r.Serial != [2]byte{0x00, 0x03} {
		t.Fatalf("Serial = % x", r.Serial)
	}
	if !r.ReportTime.Equal(rt) {
		t.Fatalf("ReportTime = %v, want %v", r.ReportTime, rt)
	}
	if !r.ObsTime.Equal(ot) {
		t.Fatalf("ObsTime = %v, want %v", r.ObsTime, ot)
	}
	if r.Station != [5]byte{0x00, 0x12, 0x34, 0x56, 0x78} {
		t.Fatalf("Station = % x", r.Station)
	}
	if r.ClassCode != ClassRiver {
		t.Fatalf("ClassCode = %02X", r.ClassCode)
	}
	if len(r.Fields) != 3 {
		t.Fatalf("Fields = %d, want 3 (voltage must not be counted)", len(r.Fields))
	}
	if r.Voltage == nil {
		t.Fatal("Voltage is nil")
	}
	if v, err := r.Voltage.Number(0); err != nil || v != 11.15 {
		t.Fatalf("voltage = %v, %v", v, err)
	}

	// 顺序必须与报文一致：先 P1，再 Z，再 Q。
	want := []struct {
		guide byte
		value float64
	}{{GP1, 0.5}, {GZ, 0.127}, {GQ, 1234.567}}
	for i, w := range want {
		if r.Fields[i].Guide != w.guide {
			t.Fatalf("field %d guide = %02X, want %02X", i, r.Fields[i].Guide, w.guide)
		}
		v, err := r.Fields[i].Number(0)
		if err != nil {
			t.Fatalf("field %d Number: %v", i, err)
		}
		if v != w.value {
			t.Fatalf("field %d value = %v, want %v", i, v, w.value)
		}
	}
	if r.Rest != nil {
		t.Fatalf("Rest = % x, want empty", r.Rest)
	}
}

// 字段名/单位/标识符可读形式都要能取到，自检输出依赖它们。
func TestFieldMetadata(t *testing.T) {
	p, _ := NewField(GP1, 5)
	if p.Name() != "1h 时段降水量" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Unit() != "mm" {
		t.Errorf("Unit = %q", p.Unit())
	}
	if p.Key() != "1A19" {
		t.Errorf("Key = %q, want 1A19", p.Key())
	}
	if p.Width() != 3 || p.Decimals() != 1 || p.Count() != 1 {
		t.Errorf("Width/Decimals/Count = %d/%d/%d", p.Width(), p.Decimals(), p.Count())
	}
}

// 缺测（数据位全 F）必须报 ErrNoData，不能当成 9999 那样的数值。
func TestFieldNoData(t *testing.T) {
	f := Field{Guide: GZ, DataDef: 0x23, Values: [][]byte{{0xFF, 0xFF, 0xFF, 0xFF}}}
	if !f.NoData(0) {
		t.Fatal("NoData(0) = false")
	}
	if _, err := f.Number(0); !errors.Is(err, ErrNoData) {
		t.Fatalf("Number = %v, want ErrNoData", err)
	}
	// 越界的索引也按「无数据」处理，避免调用方写 pani 保护。
	if !f.NoData(5) {
		t.Fatal("NoData(5) = false, want true for out of range")
	}
	if _, err := f.Number(5); err == nil {
		t.Fatal("Number(5) should fail")
	}
	// 单字节 0xFF 也是缺测。
	g := Field{Guide: GNS, DataDef: 0x08, Values: [][]byte{{0xFF}}}
	if !g.NoData(0) {
		t.Fatal("single 0xFF byte should be no-data")
	}
}

// DRP/DRZ 是 HEX 整数：0x01F4 = 500 → 5.00m，不是 BCD 的 194。
func TestFieldHexCoded(t *testing.T) {
	// DRZ1 的默认宽度是 24 字节（12 组 × 2 字节）；拆成 12 组后每组 2 字节。
	drz, err := NewFieldRaw(GDRZ1, make([]byte, 24))
	if err != nil {
		t.Fatal(err)
	}
	drz.Values[0][0], drz.Values[0][1] = 0x01, 0xF4
	parts := SplitBytes(drz.Values[0], 2)
	if len(parts) != 12 {
		t.Fatalf("SplitBytes = %d parts, want 12", len(parts))
	}
	f1 := Field{Guide: GDRZ1, DataDef: drz.DataDef, Values: [][]byte{parts[0]}}
	if v, err := f1.Number(0); err != nil || v != 5.00 {
		t.Fatalf("DRZ1 = %v, %v, want 5", v, err)
	}
	f0 := Field{Guide: GDRZ1, DataDef: drz.DataDef, Values: [][]byte{parts[1]}}
	if v, err := f0.Number(0); err != nil || v != 0 {
		t.Fatalf("DRZ1 second = %v, %v, want 0", v, err)
	}

	// 雨量：1 字节 HEX × 0.1mm，0xFF 是缺测。DRP 的默认宽度是 12 字节。
	drpData := make([]byte, 12)
	drpData[0] = 0x19
	drpData[1] = 0xFF
	drp, err := NewFieldRaw(GDRP, drpData)
	if err != nil {
		t.Fatal(err)
	}
	r0 := Field{Guide: GDRP, DataDef: drp.DataDef, Values: [][]byte{drp.Values[0][0:1]}}
	if v, err := r0.Number(0); err != nil || v != 2.5 {
		t.Fatalf("DRP = %v, %v, want 2.5 (0x19=25 -> 2.5mm)", v, err)
	}
	r1 := Field{Guide: GDRP, DataDef: drp.DataDef, Values: [][]byte{drp.Values[0][1:2]}}
	if _, err := r1.Number(0); !errors.Is(err, ErrNoData) {
		t.Fatalf("DRP 0xFF = %v, want ErrNoData", err)
	}

	// 状态字没有数值含义。
	zt := Field{Guide: GZT, DataDef: 0x20, Values: [][]byte{{0x00, 0x00, 0x00, 0x01}}}
	if _, err := zt.Number(0); !errors.Is(err, ErrBadElement) {
		t.Fatalf("ZT Number = %v, want ErrBadElement", err)
	}
	if zt.Hex(0) != 1 {
		t.Fatalf("ZT Hex = %X, want 1", zt.Hex(0))
	}
}

// 非 BCD 字节必须报错而不是解出一个看似合理的数。
func TestFieldBadBCD(t *testing.T) {
	f := Field{Guide: GZ, DataDef: 0x23, Values: [][]byte{{0x00, 0x00, 0x01, 0x2A}}}
	if _, err := f.Number(0); !errors.Is(err, ErrBadElement) {
		t.Fatalf("Number = %v, want ErrBadElement", err)
	}
}

// 均匀时段报（31H）：1 个标识符 + n 个等宽数据，时间步长码 3 字节 BCD。
func TestReportEvenRoundTrip(t *testing.T) {
	rt := time.Date(2024, 1, 11, 16, 0, 0, 0, time.UTC)
	ot := time.Date(2024, 1, 11, 15, 30, 0, 0, time.UTC)

	// 5min 步长，4 个时段的水位（单位 0.001m）。
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = EncodeBCD(uint64(100+i*10), 4)
	}
	fld, err := NewFieldSeries(GZ, vals...)
	if err != nil {
		t.Fatal(err)
	}

	b := testBuilder()
	station := [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}
	raw, err := b.BuildEvenReport(0x0010, rt, station, ClassRiver, ot, StepMin(5), fld)
	if err != nil {
		t.Fatalf("BuildEvenReport: %v", err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Func != FuncEven {
		t.Fatalf("Func = %02X, want 31", f.Func)
	}
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}

	if r.TimeStep == nil {
		t.Fatal("TimeStep is nil")
	}
	if m, ok := r.TimeStep.Minutes(); !ok || m != 5 {
		t.Fatalf("TimeStep = %v, %v, want 5min", m, ok)
	}
	if !r.ObsTime.Equal(ot) {
		t.Fatalf("ObsTime = %v, want %v", r.ObsTime, ot)
	}
	if len(r.Fields) != 1 {
		t.Fatalf("Fields = %d, want 1", len(r.Fields))
	}
	z := r.Fields[0]
	if z.Guide != GZ {
		t.Fatalf("guide = %02X, want 39", z.Guide)
	}
	if z.Count() != 4 {
		t.Fatalf("Count = %d, want 4", z.Count())
	}
	for i := 0; i < 4; i++ {
		want := float64(100+i*10) / 1000
		got, err := z.Number(i)
		if err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("value %d = %v, want %v", i, got, want)
		}
	}
}

// 31H 的时段数完全由剩余字节数 ÷ 宽度决定：多一字节就报错，不能默默取整。
func TestReportEvenBadTail(t *testing.T) {
	b := testBuilder()
	station := [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}
	fld, _ := NewFieldSeries(GZ, EncodeBCD(100, 4), EncodeBCD(110, 4))
	rt := time.Date(2024, 1, 11, 16, 0, 0, 0, time.UTC)
	ot := time.Date(2024, 1, 11, 15, 30, 0, 0, time.UTC)
	body, err := b.BuildEvenReport(1, rt, station, ClassRiver, ot, StepMin(5), fld)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	// 人为在尾部塞 1 个多余字节（宽度 4，除不尽）。
	extra := append(append([]byte(nil), f.Body...), 0x00)
	rf := *f
	rf.Body = extra
	if _, err := ParseReport(&rf); !errors.Is(err, ErrBadReport) {
		t.Fatalf("ParseReport = %v, want ErrBadReport", err)
	}
}

// 31H 的时间步长码标识符必须存在，且必须是 04H/18H。
func TestReportEvenMissingStep(t *testing.T) {
	f := &Frame{Func: FuncEven, Dir: DirUp, Body: make([]byte, headLen)}
	// 只填 head 的引导符位置，其他留 0（时间会解析成 2000-00-00，但错误点在
	// 时间步长码之前，所以先校验 head 结构）。
	f.Body[8], f.Body[9] = GSI, 0xF1
	f.Body[16], f.Body[17] = GTT, 0xF0
	if _, err := ParseReport(f); err == nil {
		t.Fatal("ParseReport should fail without time step")
	}
}

// 链路维持报（2FH）：只有流水号 + 发报时间；多余字节进 Rest。
func TestReportLinkExtraBytes(t *testing.T) {
	f := &Frame{Func: FuncLink, Dir: DirUp}
	f.Body = append(f.Body, 0x12, 0x34)
	rt := EncodeBCDTime(time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC))
	f.Body = append(f.Body, rt[:]...)
	f.Body = append(f.Body, 0xAA, 0xBB)
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Rest) != 2 || r.Rest[0] != 0xAA {
		t.Fatalf("Rest = % x", r.Rest)
	}
}

// 标准报文尾部有多余字节时不能丢整帧：Rest 收下，已解析要素保留。
func TestReportStdRest(t *testing.T) {
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	b := testBuilder()
	p, _ := NewField(GP1, 5)
	raw, err := b.BuildReport(FuncTimed, 1, rt, [5]byte{0, 0, 1, 2, 3}, ClassRain, rt, []Field{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 追加一个 F2H（人工置数）——长度不可推导，必须落进 Rest 而不是乱解。
	f.Body = append(f.Body, GRGZS, 0xF2, 0x11, 0x22)
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Fields) != 1 {
		t.Fatalf("Fields = %d, want 1 (parsed part must be kept)", len(r.Fields))
	}
	if len(r.Rest) != 4 {
		t.Fatalf("Rest = % x, want 4 bytes", r.Rest)
	}
}

// 上行帧才解析；下行查询帧报 ErrBadReport。
func TestReportRejectsDownlink(t *testing.T) {
	f := &Frame{Func: FuncQueryAll, Dir: DirDown, Body: make([]byte, 8)}
	if _, err := ParseReport(f); !errors.Is(err, ErrBadReport) {
		t.Fatalf("ParseReport = %v, want ErrBadReport", err)
	}
	if _, err := ParseReport(nil); !errors.Is(err, ErrBadReport) {
		t.Fatalf("ParseReport(nil) = %v, want ErrBadReport", err)
	}
}

// 不在本次范围内的功能码（配置表、控制命令…）报 ErrUnsupported。
func TestReportUnsupportedFunc(t *testing.T) {
	for _, fn := range []byte{FuncCfgSet, FuncParamSet, FuncImage, FuncManual, 0x4C} {
		f := &Frame{Func: fn, Dir: DirUp, Body: make([]byte, headLen)}
		_, err := ParseReport(f)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("func %02X: err = %v, want ErrUnsupported", fn, err)
		}
	}
}

// 头部结构错（缺 F1F1/F0F0）必须报错，不能靠偏移硬解出垃圾点位。
func TestReportBadHead(t *testing.T) {
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	b := testBuilder()
	raw, err := b.BuildReport(FuncTimed, 1, rt, [5]byte{0, 0, 1, 2, 3}, ClassRain, rt, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 站址标识符错。
	f, _ := Parse(raw)
	bad := *f
	bad.Body = append([]byte(nil), f.Body...)
	bad.Body[8] = 0x00
	if _, err := ParseReport(&bad); !errors.Is(err, ErrBadReport) {
		t.Errorf("bad address id = %v, want ErrBadReport", err)
	}

	// 观测时间标识符错。
	f2, _ := Parse(raw)
	bad2 := *f2
	bad2.Body = append([]byte(nil), f2.Body...)
	bad2.Body[16] = 0x00
	if _, err := ParseReport(&bad2); !errors.Is(err, ErrBadReport) {
		t.Errorf("bad time id = %v, want ErrBadReport", err)
	}

	// 正文太短。
	f3 := &Frame{Func: FuncTimed, Dir: DirUp, Body: make([]byte, headLen-1)}
	if _, err := ParseReport(f3); !errors.Is(err, ErrBadReport) {
		t.Errorf("short body = %v, want ErrBadReport", err)
	}
}

// 查询应答（37H/38H/3AH 上行）与定时报同形，走同一条解析路径。
func TestReportQueryReply(t *testing.T) {
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	for _, fn := range []byte{FuncQueryAll, FuncQueryPer, FuncQueryElem} {
		z, _ := NewField(GZ, 127)
		r := mustReport(t, testBuilder(), fn, 7, rt, rt, []Field{z}, nil)
		if r.Frame.Func != fn {
			t.Fatalf("func = %02X, want %02X", r.Frame.Func, fn)
		}
		if len(r.Fields) != 1 || r.Fields[0].Guide != GZ {
			t.Fatalf("func %02X: fields = %+v", fn, r.Fields)
		}
	}
}

// Elems/First：同一引导符出现多次时要能全取，也要能取第一个。
func TestReportElemsFirst(t *testing.T) {
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	z1, _ := NewField(GZ, 127)
	z2, _ := NewField(GZ, 200)
	p, _ := NewField(GP1, 5)
	r := mustReport(t, testBuilder(), FuncSurge, 1, rt, rt, []Field{z1, p, z2}, nil)

	if got := r.Elems(GZ); len(got) != 2 {
		t.Fatalf("Elems(GZ) = %d, want 2", len(got))
	}
	f, ok := r.First(GZ)
	if !ok {
		t.Fatal("First(GZ) not found")
	}
	if v, _ := f.Number(0); v != 0.127 {
		t.Fatalf("First(GZ) = %v, want 0.127 (must be the first one)", v)
	}
	if _, ok := r.First(GQ); ok {
		t.Fatal("First(GQ) should not be found")
	}
	if got := r.Elems(GQ); got != nil {
		t.Fatalf("Elems(GQ) = %v, want nil", got)
	}
}

// SplitBytes：按宽度切分，除不尽时返回 nil（而不是返回半截）。
func TestSplitBytes(t *testing.T) {
	v := []byte{1, 2, 3, 4, 5, 6}
	got := SplitBytes(v, 2)
	if len(got) != 3 || len(got[0]) != 2 || got[2][0] != 5 {
		t.Fatalf("SplitBytes = %v", got)
	}
	// 返回的切片必须是副本：改它不能影响原数据。
	got[0][0] = 0xFF
	if v[0] != 1 {
		t.Fatal("SplitBytes must copy")
	}
	if SplitBytes(v, 4) != nil {
		t.Fatal("non-multiple should return nil")
	}
	if SplitBytes(v, 0) != nil {
		t.Fatal("zero width should return nil")
	}
}

// 时间解析：非法 BCD 与不足长度都要报错。
func TestParseTimes(t *testing.T) {
	if _, err := ParseBCDTime([]byte{0x24, 0x01, 0x11, 0x15, 0x49}); !errors.Is(err, ErrShort) {
		t.Errorf("ParseBCDTime short = %v", err)
	}
	if _, err := ParseBCDTime([]byte{0x24, 0x01, 0x11, 0x15, 0x49, 0x4A}); !errors.Is(err, ErrBadElement) {
		t.Errorf("ParseBCDTime bad BCD = %v", err)
	}
	if _, err := ParseObsTime([]byte{0x24, 0x01, 0x11, 0x15}); !errors.Is(err, ErrShort) {
		t.Errorf("ParseObsTime short = %v", err)
	}
	got, err := ParseBCDTime([]byte{0x24, 0x01, 0x11, 0x15, 0x49, 0x47})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ParseBCDTime = %v, want %v", got, want)
	}
}

// BCD：展示用的宽容解码（非法半字节不报错）。
func TestBCDLoose(t *testing.T) {
	if got := BCD([]byte{0x01, 0x2A}); got != 130 {
		t.Fatalf("BCD = %v, want 130", got)
	}
	if pow10(0) != 1 || pow10(7) != 10000000 {
		t.Fatalf("pow10 wrong")
	}
}
