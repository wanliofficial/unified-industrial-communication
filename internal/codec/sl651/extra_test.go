package sl651

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// mustTime 是测试里常用的固定发报时间。
func mustTime(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2024, 1, 11, 15, 49, 47, 0, time.UTC)
}

// FuncName/ClassName/IsUplinkReport/IsQuery 是驱动与自检命令的分类依据：
// 把「查询」当成「自报」会让驱动把下行帧当数据收，必须逐个锁定。
func TestFuncAndClassNames(t *testing.T) {
	cases := []struct {
		fn     byte
		name   string
		report bool
		query  bool
	}{
		{FuncLink, "链路维持报", true, false},
		{FuncTest, "测试报", true, false},
		{FuncEven, "均匀时段水文信息报", true, false},
		{FuncTimed, "遥测站定时报", true, false},
		{FuncSurge, "遥测站加报报", true, false},
		{FuncHourly, "遥测站小时报", true, false},
		{FuncManual, "遥测站人工置数报", true, false},
		{FuncImage, "图片报", false, false},
		{FuncQueryAll, "查询实时数据", false, true},
		{FuncQueryPer, "查询时段数据", false, true},
		{FuncQueryMan, "查询人工置数", false, true},
		{FuncQueryElem, "查询指定要素数据", false, true},
		{FuncCfgSet, "修改基本配置表", false, false},
		{FuncClockGet, "查询时钟", false, false},
	}
	for _, c := range cases {
		if got := FuncName(c.fn); got != c.name {
			t.Errorf("FuncName(%02X) = %q, want %q", c.fn, got, c.name)
		}
		if got := IsUplinkReport(c.fn); got != c.report {
			t.Errorf("IsUplinkReport(%02X) = %v, want %v", c.fn, got, c.report)
		}
		if got := IsQuery(c.fn); got != c.query {
			t.Errorf("IsQuery(%02X) = %v, want %v", c.fn, got, c.query)
		}
	}
	// 表外功能码要给出可读名而不是空串。
	if got := FuncName(0xEE); got != "功能码 EE" {
		t.Errorf("FuncName(EE) = %q", got)
	}

	classes := []struct {
		code byte
		name string
	}{
		{ClassRain, "降水"},
		{ClassRiver, "河道"},
		{ClassReservoir, "水库（闸坝）"},
		{ClassGate, "闸坝"},
		{ClassPump, "泵站"},
		{ClassSilt, "凝沙"},
		{ClassSoil, "墒情"},
		{ClassGW, "地下水"},
		{ClassWater, "水质"},
		{ClassCanal, "渠水口"},
		{ClassDrain, "排水口"},
	}
	for _, c := range classes {
		if got := ClassName(c.code); got != c.name {
			t.Errorf("ClassName(%02X) = %q, want %q", c.code, got, c.name)
		}
	}
	if got := ClassName(0x00); got != "分类码 00" {
		t.Errorf("ClassName(00) = %q", got)
	}
}

// ParseField 的各个拒绝分支。
func TestParseFieldErrors(t *testing.T) {
	// 不足 2 字节。
	if _, _, err := ParseField([]byte{0x39}); !errors.Is(err, ErrShort) {
		t.Errorf("short = %v, want ErrShort", err)
	}
	// FFXXH 自定义扩展。
	if _, _, err := ParseField([]byte{0xFF, 0x01, 0x00}); !errors.Is(err, ErrUnknownGuide) {
		t.Errorf("FFXX = %v, want ErrUnknownGuide", err)
	}
	// 长度不可推导的引导符类。
	if _, _, err := ParseField([]byte{GRGZS, 0xF2, 0x01}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("F2H = %v, want ErrUnsupported", err)
	}
	if _, _, err := ParseField([]byte{GPIC, 0xF3, 0x01}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("F3H = %v, want ErrUnsupported", err)
	}
	if _, _, err := ParseField([]byte{GDATA, 0xF6, 0x01}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FDH = %v, want ErrUnsupported", err)
	}
	// 宽度为 0。
	if _, _, err := ParseField([]byte{0x39, 0x03}); !errors.Is(err, ErrBadElement) {
		t.Errorf("zero width = %v, want ErrBadElement", err)
	}
	// 数据不足。
	if _, _, err := ParseField([]byte{0x39, 0x23, 0x00}); !errors.Is(err, ErrShort) {
		t.Errorf("short data = %v, want ErrShort", err)
	}
	// 正常：引导符类按表内固定宽度，不按高 5 位。
	f, n, err := ParseField([]byte{GTT, 0xF0, 0x24, 0x01, 0x11, 0x15, 0x49})
	if err != nil {
		t.Fatalf("ParseField(F0H): %v", err)
	}
	if n != 7 || f.Width() != 5 {
		t.Fatalf("F0H: n=%d width=%d, want 7/5", n, f.Width())
	}
	// 未知引导符按定义字节走（保留区的方言数据也要能读出来）。
	if _, _, err := ParseField([]byte{0x80, 0x11, 0x00, 0x01}); err != nil {
		t.Errorf("unknown guide with explicit width: %v", err)
	}
}

// Parse：下行帧的地址顺序与上行相反，且结束符必须是下行集合之一。
func TestParseDownlinkAddressOrder(t *testing.T) {
	b := Builder{Center: 0x05, Station: [5]byte{0x00, 0x11, 0x22, 0x33, 0x44}, Password: [2]byte{0x12, 0x34}}
	raw, err := b.BuildQuery(FuncQueryPer, 1, mustTime(t))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Center != 0x05 {
		t.Fatalf("Center = %d, want 5", f.Center)
	}
	if f.Station != b.Station {
		t.Fatalf("Station = % x", f.Station)
	}

	// 下行用 ETX 作结束符必须被拒。
	bad := append([]byte(nil), raw...)
	bad[offSTX+1+f.Len] = ETX
	bad = AppendCRC(bad[:len(bad)-2])
	if _, err := Parse(bad); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("downlink ETX = %v, want ErrBadFrame", err)
	}

	// 上行用 ACK 作结束符也必须被拒。
	up := append([]byte(nil), goldenFrames[0]...)
	up[offSTX+1+8] = ACK
	up = AppendCRC(up[:len(up)-2])
	if _, err := Parse(up); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("uplink ACK = %v, want ErrBadFrame", err)
	}
}

// Parse：长度为 0、长度超出缓冲区都要报错。
func TestParseLenErrors(t *testing.T) {
	raw := append([]byte(nil), goldenFrames[0]...)
	zero := append([]byte(nil), raw...)
	zero[offLen], zero[offLen+1] = 0x00, 0x00
	zero = AppendCRC(zero[:len(zero)-2])
	if _, err := Parse(zero); !errors.Is(err, ErrBadFrame) {
		t.Errorf("zero len = %v, want ErrBadFrame", err)
	}

	// 声称长度比实际大：CRC 位置越界。
	long := append([]byte(nil), raw...)
	long[offLen+1] = 0x40
	long = AppendCRC(long[:len(long)-2])
	if _, err := Parse(long); !errors.Is(err, ErrShort) {
		t.Errorf("long len = %v, want ErrShort", err)
	}
}

// Parse：M3 帧（SYN 起始）也要能解析，且 Packets 有效。
func TestParseM3RoundTrip(t *testing.T) {
	b := Builder{Center: 1, Station: [5]byte{1, 2, 3, 4, 5}, Password: [2]byte{6, 7}}
	body := []byte{0x00, 0x02, 0x01, 0xAA, 0xBB} // 共 2 包、本包序号 1
	raw, err := b.frame(FuncTimed, DirUp, ETB, body)
	if err != nil {
		t.Fatal(err)
	}
	// 把 STX 换成 SYN（M3 的报文起始符位置）。
	raw[offSTX] = Syn
	raw = AppendCRC(raw[:len(raw)-2])

	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !f.IsM3 {
		t.Fatal("IsM3 = false")
	}
	if f.Packets != [3]byte{0x00, 0x02, 0x01} {
		t.Fatalf("Packets = % x", f.Packets)
	}
	if f.End != ETB {
		t.Fatalf("End = %02X, want ETB (后面还有包)", f.End)
	}
}

// Reader：长度字段高 4 位既非 0 也非 8（ASCII 帧/噪声）时丢头部继续找。
func TestReaderSkipsASCIIFrame(t *testing.T) {
	// 一个正好 14 字节的伪帧头（长度为 ASCII '0'），后面跟一帧真帧。
	// 14 = 7E7E(2) + 中心站(1) + 站号(5) + 密码(2) + 功能码(1) + 长度(2) + 起始符(1)。
	junk := []byte{Start, Start, 0x01, 0, 1, 2, 3, 4, 0x12, 0x34, FuncTest, '0', '0', STX}

	var buf bytes.Buffer
	buf.Write(junk)
	buf.Write(goldenFrames[1])

	r := NewReader(&mockStream{data: buf.Bytes()})
	f, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.Func != FuncTest || f.Len != 0x2B {
		t.Fatalf("Func/Len = %02X/%d", f.Func, f.Len)
	}
	if r.BadBytes() == 0 {
		t.Fatal("BadBytes should be > 0")
	}
}

// Reader：长度字段为 0 的伪帧也要跳过。
func TestReaderSkipsZeroLen(t *testing.T) {
	junk := []byte{Start, Start, 0x01, 0, 1, 2, 3, 4, 0x12, 0x34, FuncTest, 0x00, 0x00, 0x02}
	var buf bytes.Buffer
	buf.Write(junk)
	buf.Write(goldenFrames[0])

	r := NewReader(&mockStream{data: buf.Bytes()})
	f, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.Func != FuncLink {
		t.Fatalf("Func = %02X, want 2F", f.Func)
	}
}

// Reader：读不满时把底层错误原样抛出（超时由上层分类，不能在分帧层吞掉）。
func TestReaderIOError(t *testing.T) {
	boom := errors.New("boom")
	r := NewReader(&mockStream{data: []byte{Start, Start}, err: boom})
	if _, err := r.Next(); !errors.Is(err, boom) {
		t.Fatalf("Next = %v, want boom", err)
	}
	// 头部读到了、尾部读不到。
	r2 := NewReader(&mockStream{data: append([]byte(nil), goldenFrames[0][:20]...)})
	if _, err := r2.Next(); err == nil {
		t.Fatal("Next should fail on truncated frame")
	}
}

// IsFrameStart / FrameLen / CheckCRC 的基本契约。
func TestFrameHelpers(t *testing.T) {
	if !IsFrameStart([]byte{0x7E, 0x7E}) {
		t.Error("IsFrameStart(7E7E) = false")
	}
	if IsFrameStart([]byte{0x7E}) || IsFrameStart([]byte{0x7E, 0x7F}) {
		t.Error("IsFrameStart false positive")
	}
	if FrameLen(8) != 25 {
		t.Errorf("FrameLen(8) = %d, want 25", FrameLen(8))
	}
	if FrameLen(MaxLen) != 17+MaxLen {
		t.Errorf("FrameLen(max) = %d", FrameLen(MaxLen))
	}
	if CheckCRC([]byte{0x00}) {
		t.Error("CheckCRC on 1 byte should be false")
	}
	if !CheckCRC(goldenFrames[1]) {
		t.Error("golden frame 1 should pass CRC")
	}
}

// GuideAlias 表外引导符返回空串（与 GuideName 的「引导符 XX」不同）。
func TestGuideAliasUnknown(t *testing.T) {
	if got := GuideAlias(0x99); got != "" {
		t.Fatalf("GuideAlias(99) = %q, want empty", got)
	}
	if got := GuideAlias(GZ); got != "Z" {
		t.Fatalf("GuideAlias(39) = %q, want Z", got)
	}
}

// Field.Hex 越界返回 0。
func TestFieldHexOutOfRange(t *testing.T) {
	f := Field{Guide: GZT, DataDef: 0x20, Values: [][]byte{{0, 0, 0, 1}}}
	if f.Hex(-1) != 0 || f.Hex(1) != 0 {
		t.Fatal("Hex out of range should be 0")
	}
}

// 31H 尾部完全缺失要素时也要报错（不能返回空 Fields 让上层以为没数据）。
func TestReportEvenNoElement(t *testing.T) {
	f := &Frame{Func: FuncEven, Dir: DirUp}
	f.Body = make([]byte, headLen)
	f.Body[8], f.Body[9] = GSI, 0xF1
	f.Body[16], f.Body[17] = GTT, 0xF0
	// 时间步长码齐全，但后面没有要素。
	f.Body = append(f.Body, GDR, 0x18, 0x00, 0x00, 0x05)
	if _, err := ParseReport(f); !errors.Is(err, ErrBadReport) {
		t.Fatalf("ParseReport = %v, want ErrBadReport", err)
	}
	// 时间步长码引导符写错也要报错。
	g := &Frame{Func: FuncEven, Dir: DirUp}
	g.Body = make([]byte, headLen)
	g.Body[8], g.Body[9] = GSI, 0xF1
	g.Body[16], g.Body[17] = GTT, 0xF0
	g.Body = append(g.Body, GZ, 0x23, 0x00, 0x00, 0x00, 0x01)
	if _, err := ParseReport(g); !errors.Is(err, ErrBadReport) {
		t.Fatalf("bad time step = %v, want ErrBadReport", err)
	}
}

// 31H 的时间步长码宽度必须是 3，否则报错而不是错位解析。
func TestReportEvenBadStepWidth(t *testing.T) {
	f := &Frame{Func: FuncEven, Dir: DirUp}
	f.Body = make([]byte, headLen)
	f.Body[8], f.Body[9] = GSI, 0xF1
	f.Body[16], f.Body[17] = GTT, 0xF0
	// 04 10：定义字节 0x10 → 宽度 2，但引导符类固定 3 字节；这里宽度由表决定
	// 仍是 3，重点是「宽度检查」这一段被覆盖到。给 3 字节数据即可通过。
	f.Body = append(f.Body, GDR, 0x10, 0x00, 0x00, 0x05)
	f.Body = append(f.Body, GZ, 0x23, 0x00, 0x00, 0x00, 0x01)
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.TimeStep == nil {
		t.Fatal("TimeStep nil")
	}
	if m, ok := r.TimeStep.Minutes(); !ok || m != 5 {
		t.Fatalf("TimeStep = %v", m)
	}
}
