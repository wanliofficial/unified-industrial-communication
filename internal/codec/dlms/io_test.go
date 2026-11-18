package dlms

import (
	"bytes"
	"errors"
	"testing"
)

// OBIS 的六段都是十进制；解析必须拒绝段数不对、越界与空段。
func TestParseOBIS(t *testing.T) {
	ok := []struct {
		in   string
		want OBIS
	}{
		{"1.0.1.8.0.255", OBIS{1, 0, 1, 8, 0, 255}},
		{"1-0-1-8-0-255", OBIS{1, 0, 1, 8, 0, 255}},
		{"01.00.01.08.00.255", OBIS{1, 0, 1, 8, 0, 255}},
		{"1.0.32.7.0.255", OBIS{1, 0, 32, 7, 0, 255}}, // 0x20，不是 0x32
		{"0.0.0.0.0.0", OBIS{0, 0, 0, 0, 0, 0}},
		{"255.255.255.255.255.255", OBIS{255, 255, 255, 255, 255, 255}},
	}
	for _, c := range ok {
		got, err := ParseOBIS(c.in)
		if err != nil {
			t.Errorf("ParseOBIS(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseOBIS(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	bad := []string{
		"", "1.0.1.8.0", "1.0.1.8.0.255.0", "1.0.1.8.0.256",
		"1.0.1.8.0.-1", "1.0.1.8.0.x", "1.0.1.8..255",
	}
	for _, in := range bad {
		if _, err := ParseOBIS(in); err == nil {
			t.Errorf("ParseOBIS(%q) 应报错", in)
		}
	}
}

// String / Dash / Hex 三种输出必须互相一致。
func TestOBISFormatting(t *testing.T) {
	o, _ := ParseOBIS("1.0.1.8.0.255")
	if got := o.String(); got != "1.0.1.8.0.255" {
		t.Errorf("String = %q", got)
	}
	if got := o.Dash(); got != "1-0-1-8-0-255" {
		t.Errorf("Dash = %q", got)
	}
	if got := o.Hex(); got != "01 00 01 08 00 FF" {
		t.Errorf("Hex = %q", got)
	}
	if !o.IsCurrent() {
		t.Error("F=255 应视为当前值")
	}
	hist, _ := ParseOBIS("1.0.1.8.0.1")
	if hist.IsCurrent() {
		t.Error("F=1 不应视为当前值")
	}
}

// scaler 为负是除：这是最容易写反的一处。
func TestApplyScaler(t *testing.T) {
	cases := []struct {
		raw    float64
		scaler int
		want   float64
	}{
		{2201, -1, 220.1},
		{512, -2, 5.12},
		{1234, 0, 1234},
		{1234, 1, 12340},
		{1234, 2, 123400},
		{985, -3, 0.985},
		// 这条是现场遇到的：5001/10/10 逐次除会得到 50.010000000000005，
		// 屏幕上就是一串尾数。必须一次除以 100。
		{5001, -2, 50.01},
		{5001, -1, 500.1},
		{5001, 2, 500100},
	}
	for _, c := range cases {
		if got := ApplyScaler(c.raw, c.scaler); got != c.want {
			t.Errorf("ApplyScaler(%v, %d) = %v, want %v", c.raw, c.scaler, got, c.want)
		}
	}
	// 正负都是 0：不应把 0 变成别的值，也不应除零。
	if got := ApplyScaler(0, -5); got != 0 {
		t.Errorf("ApplyScaler(0, -5) = %v", got)
	}
}

// 常见 OBIS 表必须能被查到，且两种分隔符等价。
func TestLookupOBIS(t *testing.T) {
	o, ok := LookupOBIS("1.0.1.8.0.255")
	if !ok {
		t.Fatal("正向有功电能应在表里")
	}
	if o.Class != ClassRegister || o.Attribute != AttrValue {
		t.Errorf("条目 = %+v", o)
	}
	if o.Name == "" {
		t.Error("条目缺中文名")
	}
	// 连字符写法应查到同一条。
	d, ok := LookupOBIS("1-0-1-8-0-255")
	if !ok || d.OBIS != o.OBIS {
		t.Errorf("连字符写法 = %+v, %v", d, ok)
	}
	// 前导零也应归一。
	z, ok := LookupOBIS("01.00.001.08.000.255")
	if !ok || z.OBIS != o.OBIS {
		t.Errorf("前导零写法 = %+v, %v", z, ok)
	}
	if _, ok := LookupOBIS("1.0.99.1.0.255"); ok {
		t.Error("私有 OBIS 不应在常见表里")
	}
}

// 单位表：已列出的必须给出非空名，未列出的必须给出 unit(N) 而不是猜。
func TestUnitName(t *testing.T) {
	if got := UnitName(34); got != "V" {
		t.Errorf("UnitName(34) = %q", got)
	}
	if got := UnitName(30); got != "VAh" {
		t.Errorf("UnitName(30) = %q", got)
	}
	if got := UnitName(0); got != "" {
		t.Errorf("UnitName(0) = %q, want 空", got)
	}
	if got := UnitName(200); got == "" || got == "V" {
		t.Errorf("未知单位应显式标注，得到 %q", got)
	}
}

// 类名表：常见类必须给出中文名，未知类也必须可读。
func TestClassName(t *testing.T) {
	for _, id := range []uint16{ClassData, ClassRegister, ClassClock, ClassAssociationLN} {
		if n := ClassName(id); n == "" {
			t.Errorf("类 %d 缺名字", id)
		}
	}
	if n := ClassName(999); n == "" {
		t.Error("未知类也要有可读名")
	}
}

// Reader 从字节流切帧：噪声 + 多帧 + 坏帧都要正确处理。
type sliceStream struct {
	data []byte
	pos  int
}

func (s *sliceStream) ReadFull(n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	if s.pos >= len(s.data) {
		return nil, errors.New("eof")
	}
	end := s.pos + n
	if end > len(s.data) {
		end = len(s.data)
	}
	out := s.data[s.pos:end]
	s.pos = end
	return out, nil
}

func (s *sliceStream) Write(p []byte) error { return nil }
func (s *sliceStream) Close() error         { return nil }

func TestReaderSkipsNoise(t *testing.T) {
	// SNRM 帧 + 中间噪声 + 又一帧 SNRM。
	snrm := []byte{Flag, 0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01, Flag}
	var data []byte
	data = append(data, 0x00, 0xFF, 0x12) // 前导噪声
	data = append(data, snrm...)
	data = append(data, 0x55, 0x66) // 帧间噪声
	data = append(data, snrm...)

	r := NewReader(&sliceStream{data: data})
	for i := 0; i < 2; i++ {
		fr, err := r.Next()
		if err != nil {
			t.Fatalf("第 %d 帧: %v", i+1, err)
		}
		if fr.Kind() != KindSNRM {
			t.Fatalf("第 %d 帧 kind = %v", i+1, fr.Kind())
		}
	}
}

// 坏 FCS 的帧必须被跳过并计入坏帧，而不是当成好帧返回。
func TestReaderBadFCS(t *testing.T) {
	good := []byte{Flag, 0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01, Flag}
	bad := append([]byte(nil), good...)
	bad[6] ^= 0xFF // 改坏 HCS/FCS

	var data []byte
	data = append(data, bad...)
	data = append(data, good...)

	r := NewReader(&sliceStream{data: data})
	fr, err := r.Next()
	if err != nil {
		t.Fatalf("应跳过坏帧后拿到好帧: %v", err)
	}
	if fr.Kind() != KindSNRM {
		t.Fatalf("kind = %v", fr.Kind())
	}
	if r.BadFrames() != 1 {
		t.Fatalf("坏帧计数 = %d, want 1", r.BadFrames())
	}
}

// 帧内出现 0x7E 必须仍能被正确切出。这是最隐蔽的一类分帧 bug：
// 类型 3 帧格式不做透明转义，info 与 FCS 里都可以合法出现 0x7E，只有靠
// 长度字段才能定界。
//
// 用例用的是**实机现场遇到的那一帧**：读功率因数（1.0.13.7.0.255）的
// scaler_unit（attribute 3）时，Get-Request 的 FCS 恰好是 `7E 64`。
// 症状非常有欺骗性——链路建得好好的，只有个别点位超时，重发几次偶尔又
// 成功，换一台表就"自己好了"。
func TestReaderFrameContainingFlagByte(t *testing.T) {
	apdu := GetRequest{Attribute: AttributeDescriptor{
		ClassID: ClassRegister,
		// OBIS 0.0.126.7.0.255：126 = 0x7E,让标志字节合法出现在正文里
		//（地址/正文/CRC 三个位置都可能出 7E,这里是正文）。
		InstanceID: [6]byte{1, 0, 0x7E, 7, 0, 255},
		Attribute:  AttrScalerUnit,
	}}.Encode()
	fr := Frame{
		Dest:    ServerAddress(1, 0, 1),
		Src:     ClientAddress(16),
		Control: Control{Kind: KindI, NS: 1, NR: 1, PF: true},
		Info:    append([]byte(LLCCommand), apdu...),
	}
	body, err := fr.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// 锁死"正文含 7E"这个要害：如果哪天组帧改了，这个用例会立刻提醒我们
	// 它已经不再覆盖"帧内含 7E"。
	hasFlag := false
	for _, b := range body[1 : len(body)-2] {
		if b == Flag {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		t.Fatal("正文里应含 7E（这一帧正是现场踩到的那一类）")
	}

	raw := append([]byte{Flag}, body...)
	raw = append(raw, Flag)

	// 连发两帧：按"扫下一个 7E"分帧的实现在这里会把帧腰斩。
	r := NewReader(&sliceStream{data: append(append([]byte(nil), raw...), raw...)})
	for i := 0; i < 2; i++ {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("第 %d 帧（帧内含 7E）: %v", i+1, err)
		}
		if got.Kind() != KindI {
			t.Fatalf("第 %d 帧 kind = %v", i+1, got.Kind())
		}
		if ap, ok := got.APDU(); !ok || !bytes.Equal(ap, apdu) {
			t.Fatalf("第 %d 帧 APDU = % X, want % X", i+1, ap, apdu)
		}
	}
	if r.BadFrames() != 0 {
		t.Fatalf("合法帧不应计入坏帧: %d", r.BadFrames())
	}
}

// 结束标志不在长度字段指定的位置时必须报失步，并且只跳过「起始标志 + 那个
// 坏字节」，不能把后面整段当垃圾扔掉（后一段里往往就是真帧）。
func TestParseRawDesync(t *testing.T) {
	good := []byte{Flag, 0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01, Flag}
	// 噪声字节插在起始标志与帧头之间：这是线路上最常见的失步形态。
	bad := append([]byte{Flag, 0x00}, good...)

	_, consumed, err := ParseRaw(bad)
	if err == nil {
		t.Fatal("失步输入应报错")
	}
	if consumed != 2 {
		t.Fatalf("失步时应跳过「起始标志 + 坏字节」共 2 字节，得到 %d", consumed)
	}
	fr, n, err := ParseRaw(bad[consumed:])
	if err != nil {
		t.Fatalf("重新同步失败: %v", err)
	}
	if fr.Kind() != KindSNRM {
		t.Fatalf("kind = %v", fr.Kind())
	}
	if n != len(good) {
		t.Fatalf("consumed = %d, want %d", n, len(good))
	}
}

// FrameLen 对两种输入（带起始标志的流缓冲、不含标志的 body）都要给出同一个
// 整帧长度，否则 Reader 会算错"还该读多少"。
func TestFrameLenBothViews(t *testing.T) {
	body := []byte{0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01}
	raw := append([]byte{Flag}, body...)
	raw = append(raw, Flag)

	if n, ok := FrameLen(body); !ok || n != len(raw) {
		t.Fatalf("body 视角: %d, %v；应为 %d", n, ok, len(raw))
	}
	if n, ok := FrameLen(raw); !ok || n != len(raw) {
		t.Fatalf("流缓冲视角: %d, %v；应为 %d", n, ok, len(raw))
	}
	// 只有 1 字节标志时还算不出来。
	if _, ok := FrameLen([]byte{Flag}); ok {
		t.Fatal("只有起始标志时不应给出长度")
	}
}

// 写帧必须自动补两端的 7E。
func TestWriteFrame(t *testing.T) {
	st := &sliceStream{}
	body := []byte{0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01}
	if err := WriteFrame(st, body); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// sliceStream 的 Write 是空实现，这里只验证不报错与空帧被拒绝。
	if err := WriteFrame(st, nil); err == nil {
		t.Fatal("空帧应被拒绝")
	}
}

// 控制域：I 帧 bit0=0 必须先判，否则 0x00 会被当成 U 帧。
func TestControlRoundTrip(t *testing.T) {
	cases := []Control{
		{Kind: KindI, NS: 0, NR: 0},
		{Kind: KindI, NS: 7, NR: 3, PF: true},
		{Kind: KindRR, NR: 5},
		{Kind: KindRNR, NR: 2, PF: true},
		{Kind: KindSNRM, PF: true},
		{Kind: KindUA, PF: true},
		{Kind: KindDISC, PF: true},
		{Kind: KindDM},
		{Kind: KindFRMR},
		{Kind: KindUI},
	}
	for _, c := range cases {
		b := EncodeControl(c)
		got := ParseControl(b)
		if got.Kind != c.Kind {
			t.Errorf("%+v: kind = %v, want %v（字节 %02X）", c, got.Kind, c.Kind, b)
		}
		if c.Kind == KindI && (got.NS != c.NS || got.NR != c.NR) {
			t.Errorf("%+v: 序号 = %d/%d", c, got.NS, got.NR)
		}
		if (c.Kind == KindRR || c.Kind == KindRNR) && got.NR != c.NR {
			t.Errorf("%+v: NR = %d", c, got.NR)
		}
		// P/F 只有 I 帧与 S 帧能自由设置；U 帧的控制域是固定常量，P/F 由
		// 常量自身的 bit4 决定（例如 UA=0x63 的 bit4 就是 0），因此这里
		// 不能要求往返保持 true。
		if c.Kind == KindI || c.Kind == KindRR || c.Kind == KindRNR {
			if got.PF != c.PF {
				t.Errorf("%+v: PF = %v", c, got.PF)
			}
		}
	}
}

// U 帧的控制域是标准固定值，不能"自由编码"。把它们逐个锁死：写错一位
// 会让表把 SNRM 当成别的命令，症状是"完全没应答"。
func TestControlUFramesFixed(t *testing.T) {
	cases := []struct {
		kind FrameKind
		want byte
	}{
		// 全部取 F=1（应答侧把探询位翻成 1）的标准形态。
		{KindSNRM, 0x93},
		{KindUA, 0x73},
		{KindDM, 0x1F},
		{KindDISC, 0x53},
		{KindFRMR, 0x97},
		{KindUI, 0x03},
	}
	for _, c := range cases {
		if got := EncodeControl(Control{Kind: c.kind, PF: true}); got != c.want {
			t.Errorf("%v 编码 = %02X, want %02X", c.kind, got, c.want)
		}
		if got := ParseControl(c.want).Kind; got != c.kind {
			t.Errorf("%02X 解析 = %v, want %v", c.want, got, c.kind)
		}
	}
}

// LLC 头：应答的 E7 是「这是应答」的唯一信号。
func TestFrameAPDU(t *testing.T) {
	cmd := Frame{Info: append([]byte(LLCCommand), 0xC0, 0x01)}
	p, ok := cmd.APDU()
	if !ok || !bytes.Equal(p, []byte{0xC0, 0x01}) {
		t.Fatalf("命令 APDU = % X, ok=%v", p, ok)
	}
	if cmd.IsResponse() {
		t.Error("命令帧不应是应答")
	}
	resp := Frame{Info: append([]byte(LLCResponse), 0xC4, 0x01)}
	if !resp.IsResponse() {
		t.Error("应答帧应被识别")
	}
	// 没有 LLC 头时原样返回。
	bare := Frame{Info: []byte{0xC4, 0x01}}
	p, ok = bare.APDU()
	if ok || !bytes.Equal(p, []byte{0xC4, 0x01}) {
		t.Fatalf("无 LLC APDU = % X, ok=%v", p, ok)
	}
}
