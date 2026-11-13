package sl651

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// 金向量说明见 io.go。这两帧是用「CRC 覆盖 7E7E..结束符、高位在前、长度 =
// STX 之后 ETX 之前的字节数」三条规则逐字节算出来的，不是抄的。
func TestGoldenFramesParse(t *testing.T) {
	want := []struct {
		fn     byte
		length int
		body   []byte
		crc    [2]byte
	}{
		{
			fn:     FuncLink,
			length: 8,
			body:   []byte{0x7E, 0x08, 0x22, 0x10, 0x11, 0x10, 0x07, 0x47},
			crc:    [2]byte{0xC3, 0xAE},
		},
		{
			fn:     FuncTest,
			length: 0x2B,
			crc:    [2]byte{0x71, 0x7A},
		},
	}
	for i, raw := range goldenFrames {
		f, err := Parse(raw)
		if err != nil {
			t.Fatalf("frame %d: Parse: %v", i, err)
		}
		if f.Func != want[i].fn {
			t.Fatalf("frame %d: Func = %02X, want %02X", i, f.Func, want[i].fn)
		}
		if f.Len != want[i].length {
			t.Fatalf("frame %d: Len = %d, want %d", i, f.Len, want[i].length)
		}
		if len(f.Body) != f.Len {
			t.Fatalf("frame %d: len(Body) = %d, Len = %d", i, len(f.Body), f.Len)
		}
		if f.CRC != want[i].crc {
			t.Fatalf("frame %d: CRC = %02X, want %02X", i, f.CRC, want[i].crc)
		}
		if f.End != ETX {
			t.Fatalf("frame %d: End = %02X, want ETX", i, f.End)
		}
		if f.Dir != DirUp {
			t.Fatalf("frame %d: Dir = %04X, want up", i, f.Dir)
		}
		if FrameLen(f.Len) != len(raw) {
			t.Fatalf("frame %d: FrameLen(%d) = %d, actual %d", i, f.Len, FrameLen(f.Len), len(raw))
		}
		if want[i].body != nil && !bytes.Equal(f.Body, want[i].body) {
			t.Fatalf("frame %d: Body = % x, want % x", i, f.Body, want[i].body)
		}
	}
}

// 金向量 1：正文首字节就是 0x7E，且长度字段 0008 的小端读法是 0x0800。
// 这两点合起来锁死「按长度分帧、不扫 7E」。
func TestGoldenLinkReport(t *testing.T) {
	f, err := Parse(goldenFrames[0])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.Serial != [2]byte{0x7E, 0x08} {
		t.Fatalf("Serial = % x, want 7E 08", r.Serial)
	}
	want := time.Date(2022, 10, 11, 10, 7, 47, 0, time.UTC)
	if !r.ReportTime.Equal(want) {
		t.Fatalf("ReportTime = %v, want %v", r.ReportTime, want)
	}
	if len(r.Fields) != 0 || r.Voltage != nil {
		t.Fatalf("link report should have no fields, got %d", len(r.Fields))
	}
}

// 金向量 2：30H 测试报，水位 0.127 m + 当前降水量 0.5 mm + 电压 11.15 V。
func TestGoldenTestReport(t *testing.T) {
	f, err := Parse(goldenFrames[1])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := ParseReport(f)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}

	if r.Station != [5]byte{0x00, 0x12, 0x34, 0x56, 0x78} {
		t.Fatalf("Station = % x", r.Station)
	}
	if r.ClassCode != ClassRiver {
		t.Fatalf("ClassCode = %02X, want %02X (河道)", r.ClassCode, ClassRiver)
	}
	if want := time.Date(2024, 1, 11, 15, 49, 0, 0, time.UTC); !r.ObsTime.Equal(want) {
		t.Fatalf("ObsTime = %v, want %v", r.ObsTime, want)
	}
	if len(r.Fields) != 3 {
		t.Fatalf("Fields = %d, want 3", len(r.Fields))
	}
	if r.Rest != nil && len(r.Rest) != 0 {
		t.Fatalf("Rest = % x, want empty", r.Rest)
	}

	// 当前降水量：引导符 20H，1 位小数，005 → 0.5 mm。
	p := r.Fields[0]
	if p.Guide != GPJ {
		t.Fatalf("field 0 guide = %02X, want %02X", p.Guide, GPJ)
	}
	if p.Width() != 3 || p.Decimals() != 1 {
		t.Fatalf("field 0 width/decimals = %d/%d, want 3/1", p.Width(), p.Decimals())
	}
	if v, err := p.Number(0); err != nil || v != 0.5 {
		t.Fatalf("field 0 number = %v, %v, want 0.5", v, err)
	}
	if p.Name() != "当前降水量" || p.Unit() != "mm" {
		t.Fatalf("field 0 name/unit = %q/%q", p.Name(), p.Unit())
	}

	// 降水量累计值：引导符 26H，与 P1 同宽同小数位。
	if c := r.Fields[1]; c.Guide != GPT {
		t.Fatalf("field 1 guide = %02X, want %02X", c.Guide, GPT)
	} else if v, err := c.Number(0); err != nil || v != 0.5 {
		t.Fatalf("field 1 number = %v, %v, want 0.5", v, err)
	}

	// 瞬时水位：引导符 39H，3 位小数，00000127 → 0.127 m。
	z := r.Fields[2]
	if z.Guide != GZ {
		t.Fatalf("field 2 guide = %02X, want %02X", z.Guide, GZ)
	}
	if v, err := z.Number(0); err != nil || v != 0.127 {
		t.Fatalf("field 2 number = %v, %v, want 0.127", v, err)
	}

	// 电压：引导符 38H，11.15 V。
	if r.Voltage == nil {
		t.Fatal("Voltage is nil")
	}
	if v, err := r.Voltage.Number(0); err != nil || v != 11.15 {
		t.Fatalf("voltage = %v, %v, want 11.15", v, err)
	}
}

// CRC 必须高位字节在前：对调两字节后校验应当失败。
func TestCRCHighByteFirst(t *testing.T) {
	raw := append([]byte(nil), goldenFrames[0]...)
	if !CheckCRC(raw) {
		t.Fatal("golden frame should pass")
	}
	raw[len(raw)-1], raw[len(raw)-2] = raw[len(raw)-2], raw[len(raw)-1]
	if CheckCRC(raw) {
		t.Fatal("byte-swapped CRC should fail")
	}
	if _, err := Parse(raw); !errors.Is(err, ErrCRC) {
		t.Fatalf("Parse = %v, want ErrCRC", err)
	}
}

// 长度字段数的是「STX 之后、ETX 之前」的字节数，不含 STX/ETX。
func TestLengthSemantics(t *testing.T) {
	raw := goldenFrames[0]
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 显式定位：7E7E(2) + 中心站(1) + 站号(5) + 密码(2) + 功能码(1) = 11，
	// 长度字段 11..12，STX 在 13，正文 14..14+Len-1，ETX 在 14+Len。
	if raw[offSTX] != STX {
		t.Fatalf("raw[%d] = %02X, want STX", offSTX, raw[offSTX])
	}
	end := offSTX + 1 + f.Len
	if raw[end] != ETX {
		t.Fatalf("raw[%d] = %02X, want ETX", end, raw[end])
	}
	if end+2 != len(raw)-1 {
		t.Fatalf("CRC should be 2 bytes at %d..%d, frame len %d", end+1, end+2, len(raw))
	}
}

// Reader：正文含 0x7E 时仍按长度取整帧。
func TestReaderBodyWith0x7E(t *testing.T) {
	r := NewReader(&mockStream{data: append([]byte(nil), goldenFrames[0]...)})
	f, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.Func != FuncLink {
		t.Fatalf("Func = %02X, want 2F", f.Func)
	}
	if r.BadBytes() != 0 {
		t.Fatalf("BadBytes = %d, want 0", r.BadBytes())
	}
}

// Reader：前置噪声应被跳过，坏帧（CRC 错）也应被跳过并计数。
func TestReaderSkipsNoiseAndBadCRC(t *testing.T) {
	var buf []byte
	buf = append(buf, 0x00, 0xFF, 0x7E, 0x13) // 噪声
	bad := append([]byte(nil), goldenFrames[0]...)
	bad[len(bad)-1] ^= 0xFF // 破坏 CRC
	buf = append(buf, bad...)
	buf = append(buf, goldenFrames[1]...)

	r := NewReader(&mockStream{data: buf})
	f, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.Func != FuncTest {
		t.Fatalf("Func = %02X, want 30 (bad frame should be skipped)", f.Func)
	}
	if r.BadBytes() == 0 {
		t.Fatal("BadBytes should count skipped bytes")
	}
}

// ASCII 字符编码帧（长度字段是 4 个 ASCII 字符）必须报 ErrUnsupported。
func TestParseRejectsASCIIFrame(t *testing.T) {
	raw := append([]byte(nil), goldenFrames[0]...)
	raw[offLen] = '0' // '0' = 0x30，既不是 00 也不是 80
	if _, err := Parse(raw); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Parse = %v, want ErrUnsupported", err)
	}
}

// M3 多包：报文起始符位置是 SYN，后面 3 字节包信息也计在长度之内，本包解析
// 标记但不重组。
func TestParseM3Flag(t *testing.T) {
	// 包信息 00 00 01（共 1 包、本包序号 1）+ 5 字节载荷。
	body := []byte{0x00, 0x00, 0x01, 0x02, 0x00, 0x01, 0x02, 0x03}
	frame := make([]byte, 0, 64)
	frame = append(frame, Start, Start, 0x01, 0x00, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, FuncTimed)
	rawLen := DirUp | len(body)
	frame = append(frame, byte(rawLen>>8), byte(rawLen&0xFF), Syn)
	frame = append(frame, body...)
	frame = append(frame, ETX)
	frame = AppendCRC(frame)

	f, err := Parse(frame)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !f.IsM3 {
		t.Fatal("IsM3 = false, want true")
	}
	if f.Packets != [3]byte{0x00, 0x00, 0x01} {
		t.Fatalf("Packets = % x", f.Packets)
	}
	if f.Len != len(body) {
		t.Fatalf("Len = %d, want %d", f.Len, len(body))
	}
}

// 结束符与方向必须匹配：上行用 ETX/ETB，下行用 ENQ/ACK/NAK/EOT/ESC。
func TestParseEndByteMismatch(t *testing.T) {
	frame := make([]byte, 0, 64)
	body := []byte{0x02, 0x01}
	frame = append(frame, Start, Start, 0x01, 0x00, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, FuncTest)
	rawLen := DirUp | len(body)
	frame = append(frame, byte(rawLen>>8), byte(rawLen&0xFF), STX)
	frame = append(frame, body...)
	frame = append(frame, ACK) // 上行不能用 ACK 作结束符
	frame = AppendCRC(frame)
	if _, err := Parse(frame); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("Parse = %v, want ErrBadFrame", err)
	}
}

// 帧起始符错误 / 字节不足 / 报文起始符错误。
func TestParseStructuralErrors(t *testing.T) {
	if _, err := Parse([]byte{0x7E, 0x7E, 0x01}); !errors.Is(err, ErrShort) {
		t.Fatalf("short = %v, want ErrShort", err)
	}
	raw := append([]byte(nil), goldenFrames[0]...)
	raw[0] = 0x7F
	if _, err := Parse(raw); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("bad start = %v, want ErrBadFrame", err)
	}
	raw = append([]byte(nil), goldenFrames[0]...)
	raw[offSTX] = 0x55
	raw = AppendCRC(raw[:len(raw)-2]) // 重算 CRC，确保失败点是 STX
	if _, err := Parse(raw); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("bad stx = %v, want ErrBadFrame", err)
	}
}

// mockStream 模拟 transport.Stream 的 ReadFull 语义。err 非空时优先返回它，
// 用来验证分帧层不会吞掉底层超时错误。
type mockStream struct {
	data []byte
	pos  int
	err  error
}

func (m *mockStream) ReadFull(n int) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.pos+n > len(m.data) {
		return nil, errors.New("short read")
	}
	buf := make([]byte, n)
	copy(buf, m.data[m.pos:m.pos+n])
	m.pos += n
	return buf, nil
}
