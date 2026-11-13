package sl651

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// 下行帧的地址字段顺序与上行相反（表 12：2=遥测站地址 5 字节，3=中心站地址
// 1 字节）。只做自报解析时这个差别永远暴露不出来，一旦中心站发查询就会把
// 自己的站号写进遥测站字段，对端无视或拒绝。这里逐字节锁死。
func TestBuildQueryFrameLayout(t *testing.T) {
	b := Builder{
		Center:   0x01,
		Station:  [5]byte{0x00, 0x12, 0x34, 0x56, 0x78},
		Password: [2]byte{0x12, 0x34},
	}
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	raw, err := b.BuildQuery(FuncQueryAll, 0x0007, rt)
	if err != nil {
		t.Fatalf("BuildQuery: %v", err)
	}

	want := []byte{
		Start, Start,
		0x00, 0x12, 0x34, 0x56, 0x78, // 遥测站地址 5 字节在前
		0x01,       // 中心站地址在后
		0x12, 0x34, // 密码
		FuncQueryAll, // 功能码
		0x80, 0x08,   // 上下行标识 1000 + 长度 8
		STX,
		0x00, 0x07, // 流水号
		0x24, 0x01, 0x11, 0x15, 0x49, 0x47, // 发报时间
		ENQ, // 下行结束符
	}
	if len(raw) != len(want)+2 {
		t.Fatalf("frame len = %d, want %d", len(raw), len(want)+2)
	}
	if !bytes.Equal(raw[:len(want)], want) {
		t.Fatalf("frame = % x\nwant  = % x", raw[:len(want)], want)
	}
	if !CheckCRC(raw) {
		t.Fatal("CRC should be valid")
	}

	// 解析回来，方向/地址/长度/功能码都要对。
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Dir != DirDown {
		t.Fatalf("Dir = %04X, want DirDown", f.Dir)
	}
	if f.Center != 0x01 {
		t.Fatalf("Center = %d, want 1", f.Center)
	}
	if f.Station != b.Station {
		t.Fatalf("Station = % x, want % x", f.Station, b.Station)
	}
	if f.Password != b.Password {
		t.Fatalf("Password = % x", f.Password)
	}
	if f.End != ENQ {
		t.Fatalf("End = %02X, want ENQ", f.End)
	}
	if f.Len != 8 {
		t.Fatalf("Len = %d, want 8", f.Len)
	}
}

// 上行帧的地址字段顺序是「中心站 1 字节 + 遥测站 5 字节」（表 11）。
func TestBuildReportFrameLayout(t *testing.T) {
	b := Builder{Center: 0x00, Station: [5]byte{0x98, 0x76, 0x54, 0x32, 0x10}, Password: [2]byte{0x12, 0x34}}
	rt := time.Date(2022, 10, 11, 10, 7, 47, 0, time.UTC)
	raw, err := b.BuildLinkReport(0x7E08, rt)
	if err != nil {
		t.Fatalf("BuildLinkReport: %v", err)
	}

	want := []byte{
		Start, Start,
		0x00,                         // 中心站地址在前
		0x98, 0x76, 0x54, 0x32, 0x10, // 遥测站地址
		0x12, 0x34,
		FuncLink,
		0x00, 0x08,
		STX,
		0x7E, 0x08, 0x22, 0x10, 0x11, 0x10, 0x07, 0x47,
		ETX,
	}
	if !bytes.Equal(raw[:len(want)], want) {
		t.Fatalf("frame = % x\nwant  = % x", raw[:len(want)], want)
	}
	if !CheckCRC(raw) {
		t.Fatal("CRC should be valid")
	}

	// 与 io.go 的金向量必须完全一致：编码器与解析器两侧都错也能自洽，
	// 只有和手算金向量比才能发现。
	if !bytes.Equal(raw, goldenFrames[0]) {
		t.Fatalf("BuildLinkReport = % x\nwant            = % x", raw, goldenFrames[0])
	}
}

// 测试报（30H）与金向量逐字节一致。
func TestBuildTestReportGolden(t *testing.T) {
	b := Builder{Center: 0x01, Station: [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}, Password: [2]byte{0x12, 0x34}}
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
	ot := time.Date(2024, 1, 11, 15, 49, 0, 0, time.UTC)

	pj, err := NewField(GPJ, 5)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := NewField(GPT, 5)
	if err != nil {
		t.Fatal(err)
	}
	z, err := NewField(GZ, 127)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b.BuildReport(FuncTest, 0x0003, rt, b.Station, ClassRiver, ot,
		[]Field{pj, pt, z}, []byte{0x11, 0x15})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if !bytes.Equal(raw, goldenFrames[1]) {
		t.Fatalf("BuildReport = % x\nwant        = % x", raw, goldenFrames[1])
	}
}

// 3AH 查询指定要素：正文只写「引导符 + 数据定义字节」，不写数据。
func TestBuildQueryElem(t *testing.T) {
	b := Builder{Center: 1, Station: [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}, Password: [2]byte{0x12, 0x34}}
	rt := time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)

	raw, err := b.BuildQueryElem(0x0009, rt, []byte{GZ, GP1, GVT})
	if err != nil {
		t.Fatalf("BuildQueryElem: %v", err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Func != FuncQueryElem || f.Dir != DirDown {
		t.Fatalf("Func/Dir = %02X/%04X", f.Func, f.Dir)
	}

	// 正文 = 流水号 2 + 时间 6 + 3 组「引导符 + 定义字节」= 8 + 6 = 14。
	if f.Len != 14 {
		t.Fatalf("Len = %d, want 14", f.Len)
	}
	body := f.Body[8:]
	want := []struct {
		guide byte
		def   byte
	}{
		{GZ, 0x23},  // N(7,3) → 4 字节 / 3 位小数
		{GP1, 0x19}, // N(5,1) → 3 字节 / 1 位小数
		{GVT, 0x12}, // N(4,2) → 2 字节 / 2 位小数
	}
	if len(body) != 2*len(want) {
		t.Fatalf("body element bytes = %d, want %d", len(body), 2*len(want))
	}
	for i, w := range want {
		if body[2*i] != w.guide || body[2*i+1] != w.def {
			t.Fatalf("element %d = %02X %02X, want %02X %02X",
				i, body[2*i], body[2*i+1], w.guide, w.def)
		}
	}
}

// 参数校验：不该构造的帧一律报错，而不是送出半条报文。
func TestBuildErrors(t *testing.T) {
	b := Builder{Center: 1, Password: [2]byte{1, 2}}
	rt := time.Now()

	if _, err := b.BuildQuery(FuncTest, 1, rt); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BuildQuery(30H) = %v, want ErrUnsupported", err)
	}
	if _, err := b.BuildQueryElem(1, rt, nil); !errors.Is(err, ErrBadReport) {
		t.Errorf("BuildQueryElem(no guide) = %v, want ErrBadReport", err)
	}
	if _, err := b.BuildQueryElem(1, rt, []byte{0x99}); !errors.Is(err, ErrUnknownGuide) {
		t.Errorf("BuildQueryElem(unknown) = %v, want ErrUnknownGuide", err)
	}
	if _, err := b.BuildReport(FuncQueryAll+0x10, 1, rt, [5]byte{}, 0, rt, nil, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BuildReport(bad func) = %v, want ErrUnsupported", err)
	}
	if _, err := b.BuildReport(FuncTimed, 1, rt, [5]byte{}, 0, rt, nil, []byte{1}); !errors.Is(err, ErrBadReport) {
		t.Errorf("BuildReport(bad voltage) = %v, want ErrBadReport", err)
	}
	// 人工置数的宽度不可推导，不能当普通要素编码。
	if _, err := b.BuildReport(FuncTimed, 1, rt, [5]byte{}, 0, rt,
		[]Field{{Guide: GRGZS, DataDef: 0xF2, Values: [][]byte{{1, 2}}}}, nil); err == nil {
		t.Error("BuildReport(F2H) should fail")
	}
}

// encodeField：多组数据必须同宽，不同宽就拒绝而不是截断。
func TestEncodeFieldWidth(t *testing.T) {
	f := Field{Guide: GZ, DataDef: 0x23, Values: [][]byte{
		{0, 0, 0, 1}, {0, 0, 1}, // 第二组 3 字节，与宽度 4 不符
	}}
	if _, err := encodeField(f); !errors.Is(err, ErrWidth) {
		t.Fatalf("encodeField = %v, want ErrWidth", err)
	}
	// 宽度为 0。
	if _, err := encodeField(Field{Guide: GZ, DataDef: 0x00}); !errors.Is(err, ErrWidth) {
		t.Fatalf("zero width = %v, want ErrWidth", err)
	}
	// 正常多组。
	ok := Field{Guide: GZ, DataDef: 0x23, Values: [][]byte{{0, 0, 0, 1}, {0, 0, 0, 2}}}
	got, err := encodeField(ok)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2+4*2 {
		t.Fatalf("encodeField len = %d, want 10", len(got))
	}
}

// NewField / NewFieldRaw / NewFieldSeries 的边界。
func TestNewFieldErrors(t *testing.T) {
	if _, err := NewField(0x99, 1); !errors.Is(err, ErrUnknownGuide) {
		t.Errorf("unknown guide = %v", err)
	}
	// 4 字节 BCD 上限 99999999，超一位就拒绝。
	if _, err := NewField(GZ, 100000000); !errors.Is(err, ErrBadElement) {
		t.Errorf("overflow = %v, want ErrBadElement", err)
	}
	if _, err := NewField(GZ, 99999999); err != nil {
		t.Errorf("max value should fit: %v", err)
	}
	// HEX 类不接受整数。
	if _, err := NewField(GDRZ1, 1); !errors.Is(err, ErrBadElement) {
		t.Errorf("NewField(DRZ1) = %v, want ErrBadElement", err)
	}
	// 宽度不可推导的引导符类。
	if _, err := NewFieldRaw(GRGZS, []byte{1}); !errors.Is(err, ErrBadElement) {
		t.Errorf("NewFieldRaw(RGZS) = %v, want ErrBadElement", err)
	}
	if _, err := NewFieldRaw(GZ, []byte{1, 2}); !errors.Is(err, ErrBadElement) {
		t.Errorf("NewFieldRaw(short) = %v, want ErrBadElement", err)
	}
	if _, err := NewFieldSeries(GZ); !errors.Is(err, ErrBadElement) {
		t.Errorf("NewFieldSeries(empty) = %v, want ErrBadElement", err)
	}
	if _, err := NewFieldSeries(GZ, []byte{1}); !errors.Is(err, ErrBadElement) {
		t.Errorf("NewFieldSeries(bad width) = %v, want ErrBadElement", err)
	}
	if _, err := NewFieldSeries(GZ, []byte{0, 0, 0, 1}, []byte{0, 0, 0, 2}); err != nil {
		t.Errorf("NewFieldSeries ok = %v", err)
	}
}

// BCD 编码/时间编码的边界。
func TestEncodeBCDBasics(t *testing.T) {
	if got := EncodeBCD(1234, 3); !bytes.Equal(got, []byte{0x00, 0x12, 0x34}) {
		t.Fatalf("EncodeBCD = % x", got)
	}
	if got := EncodeBCD(99, 1); !bytes.Equal(got, []byte{0x99}) {
		t.Fatalf("EncodeBCD = % x", got)
	}
	got := EncodeBCDTime(time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC))
	if !bytes.Equal(got[:], []byte{0x24, 0x01, 0x11, 0x15, 0x49, 0x47}) {
		t.Fatalf("EncodeBCDTime = % x", got)
	}
	obs := EncodeObsTime(time.Date(2024, 1, 11, 15, 49, 0, 0, time.UTC))
	if !bytes.Equal(obs[:], []byte{0x24, 0x01, 0x11, 0x15, 0x49}) {
		t.Fatalf("EncodeObsTime = % x", obs)
	}
	if fitsBCD(99999999, 4) != true || fitsBCD(100000000, 4) != false {
		t.Fatal("fitsBCD wrong")
	}
}

// 均匀时段报（31H）的逐字节结构与金向量。
//
// 正文 = 流水号2 + 发报时间6 + F1F1 + 站号5 + 分类码1 + F0F0 + 观测时间5
//   - 04 18 + 时间步长码3 + 要素标识符2 + n×宽
func TestBuildEvenReportLayout(t *testing.T) {
	b := Builder{Center: 0x01, Station: [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}, Password: [2]byte{0x12, 0x34}}
	rt := time.Date(2024, 1, 11, 16, 0, 0, 0, time.UTC)
	ot := time.Date(2024, 1, 11, 15, 30, 0, 0, time.UTC)

	// 3 个 5min 时段的 1h 降水量（0.1mm 单位，3 字节 BCD）。
	fld, err := NewFieldSeries(GP1,
		[]byte{0x00, 0x00, 0x05},
		[]byte{0x00, 0x00, 0x12},
		[]byte{0x00, 0x00, 0x00},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b.BuildEvenReport(0x0020, rt, b.Station, ClassRain, ot, StepMin(5), fld)
	if err != nil {
		t.Fatalf("BuildEvenReport: %v", err)
	}

	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 正文长度 = 8 + 7(地址标识符+站号) + 1 + 7(时间标识符+观测时间) + 5(步长)
	//          + 2 + 3*3
	if want := 8 + 7 + 1 + 7 + 5 + 2 + 9; f.Len != want {
		t.Fatalf("Len = %d, want %d", f.Len, want)
	}

	// 时间步长码必须写 04 18 00 00 05（定义字节固定 18H，不是按宽度换算的 0x10）。
	if got := f.Body[headLen : headLen+2]; !bytes.Equal(got, []byte{GDR, 0x18}) {
		t.Fatalf("time step id = % x, want 04 18", got)
	}
	if got := f.Body[headLen+2 : headLen+5]; !bytes.Equal(got, []byte{0x00, 0x00, 0x05}) {
		t.Fatalf("time step = % x, want 00 00 05", got)
	}
	// 要素标识符 + 3 组数据。
	if got := f.Body[headLen+5 : headLen+7]; !bytes.Equal(got, []byte{GP1, 0x19}) {
		t.Fatalf("element id = % x, want 1A 19", got)
	}
}

// 时间步长码不合法时必须拒绝构造。
func TestBuildEvenReportBadStep(t *testing.T) {
	b := Builder{Center: 1, Password: [2]byte{1, 2}}
	rt := time.Now()
	fld, _ := NewFieldSeries(GZ, []byte{0, 0, 0, 1})

	if _, err := b.BuildEvenReport(1, rt, [5]byte{}, 0, rt, TimeStep{0, 1, 0x30}, fld); !errors.Is(err, ErrBadReport) {
		t.Errorf("two nonzero bytes = %v, want ErrBadReport", err)
	}
	if _, err := b.BuildEvenReport(1, rt, [5]byte{}, 0, rt, StepMin(5), Field{Guide: 0x99}); !errors.Is(err, ErrUnknownGuide) {
		t.Errorf("unknown guide = %v, want ErrUnknownGuide", err)
	}
	empty := Field{Guide: GZ, DataDef: 0x23}
	if _, err := b.BuildEvenReport(1, rt, [5]byte{}, 0, rt, StepMin(5), empty); !errors.Is(err, ErrBadReport) {
		t.Errorf("no values = %v, want ErrBadReport", err)
	}
}

// 正文超长（> 4095）必须拒绝，而不是让长度字段溢出到上下行标识位。
func TestBuildBodyTooLong(t *testing.T) {
	b := Builder{Center: 1, Password: [2]byte{1, 2}}
	if _, err := b.frame(FuncTimed, DirUp, ETX, make([]byte, MaxLen+1)); !errors.Is(err, ErrBadReport) {
		t.Fatalf("too long = %v, want ErrBadReport", err)
	}
	if _, err := b.frame(FuncTimed, DirUp, ETX, nil); !errors.Is(err, ErrBadReport) {
		t.Fatalf("empty = %v, want ErrBadReport", err)
	}
}

// 小时报（34H）与定时报同形：要素是 DRP(12B)、PT、DRZ(24B) 这样的定宽批量数据。
func TestBuildHourlyReport(t *testing.T) {
	b := Builder{Center: 0x01, Station: [5]byte{0x00, 0x12, 0x34, 0x56, 0x78}, Password: [2]byte{0x12, 0x34}}
	rt := time.Date(2024, 1, 11, 16, 0, 0, 0, time.UTC)

	drpData := make([]byte, 12)
	for i := range drpData {
		drpData[i] = byte(10 * (i + 1)) // 1.0, 2.0, ..., 12.0 mm
	}
	drp, err := NewFieldRaw(GDRP, drpData)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := NewField(GPT, 660) // 66.0mm
	if err != nil {
		t.Fatal(err)
	}
	drzData := make([]byte, 24)
	// 前 12 组每组 2 字节 HEX，单位 cm：第 1 组 500cm = 5.00m。
	drzData[0], drzData[1] = 0x01, 0xF4
	drz, err := NewFieldRaw(GDRZ1, drzData)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := b.BuildReport(FuncHourly, 0x0005, rt, b.Station, ClassRain, rt,
		[]Field{drp, pt, drz}, []byte{0x12, 0x34})
	if err != nil {
		t.Fatalf("BuildReport(34H): %v", err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Fields) != 3 {
		t.Fatalf("Fields = %d, want 3", len(r.Fields))
	}

	// 雨量：12 组 1 字节。
	if r.Fields[0].Guide != GDRP || r.Fields[0].Width() != 12 {
		t.Fatalf("DRP: guide=%02X width=%d", r.Fields[0].Guide, r.Fields[0].Width())
	}
	slots := SplitBytes(r.Fields[0].Values[0], 1)
	if len(slots) != 12 {
		t.Fatalf("DRP slots = %d, want 12", len(slots))
	}
	for i, s := range slots {
		got := r.Fields[0]
		got.Values = [][]byte{s}
		v, err := got.Number(0)
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		if want := float64((i + 1)); v != want {
			t.Fatalf("slot %d = %v, want %v", i, v, want)
		}
	}

	// 累计值。
	if v, err := r.Fields[1].Number(0); err != nil || v != 66.0 {
		t.Fatalf("PT = %v, %v, want 66", v, err)
	}

	// 相对水位：12 组，第 1 组 5.00m。
	if r.Fields[2].Width() != 24 {
		t.Fatalf("DRZ width = %d, want 24", r.Fields[2].Width())
	}
	zs := SplitBytes(r.Fields[2].Values[0], 2)
	if len(zs) != 12 {
		t.Fatalf("DRZ slots = %d, want 12", len(zs))
	}
	first := r.Fields[2]
	first.Values = [][]byte{zs[0]}
	if v, err := first.Number(0); err != nil || v != 5.00 {
		t.Fatalf("DRZ[0] = %v, %v, want 5", v, err)
	}
}
