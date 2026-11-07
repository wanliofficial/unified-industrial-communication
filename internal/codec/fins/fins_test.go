package fins

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func hex(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*3)
	for i, x := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[x>>4], digits[x&0x0F])
	}
	return string(out)
}

func TestBuildNodeRequestGolden(t *testing.T) {
	got := BuildNodeRequest(0)
	want := []byte{0x46, 0x49, 0x4E, 0x53, 0, 0, 0, 0x0C,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("node request = %s, want %s", hex(got), hex(want))
	}
}

func TestBuildNodeResponseGolden(t *testing.T) {
	got := BuildNodeResponse(2, 1)
	if len(got) != 24 {
		t.Fatalf("len = %d", len(got))
	}
	if string(got[:4]) != "FINS" {
		t.Fatalf("magic = % x", got[:4])
	}
	if binary.BigEndian.Uint32(got[4:]) != 16 {
		t.Fatalf("length = %d", binary.BigEndian.Uint32(got[4:]))
	}
	if binary.BigEndian.Uint32(got[8:]) != 1 || binary.BigEndian.Uint32(got[12:]) != 0 {
		t.Fatalf("command/error = %d/%d", binary.BigEndian.Uint32(got[8:]), binary.BigEndian.Uint32(got[12:]))
	}
	if binary.BigEndian.Uint32(got[16:]) != 2 || binary.BigEndian.Uint32(got[20:]) != 1 {
		t.Fatalf("nodes = %d/%d", binary.BigEndian.Uint32(got[16:]), binary.BigEndian.Uint32(got[20:]))
	}
}

func TestBuildMemoryReadGolden(t *testing.T) {
	dm, ok := Lookup("dm")
	if !ok {
		t.Fatal("dm not found")
	}
	frame, err := BuildMemoryRead(CommandHeader(1, 2, 3), WordAccess(dm), 100, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x80, 0x00, 0x02, 0x00, 0x01, 0x00, 0x00, 0x02, 0x00, 0x03,
		0x01, 0x01, 0x82, 0x00, 0x64, 0x00, 0x00, 0x02}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = %s\nwant    %s", hex(frame), hex(want))
	}
}

func TestBuildMemoryReadBitGolden(t *testing.T) {
	cio, _ := Lookup("cio")
	acc, err := BitAccess(cio)
	if err != nil {
		t.Fatal(err)
	}
	// CIO 10.3，读 4 位：区码 0x30，字地址 00 0A，位号 03。
	frame, err := BuildMemoryRead(CommandHeader(1, 2, 0), acc, 10, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	body := frame[12:]
	want := []byte{0x30, 0x00, 0x0A, 0x03, 0x00, 0x04}
	if !bytes.Equal(body, want) {
		t.Fatalf("body = %s, want %s", hex(body), hex(want))
	}
}

func TestParseResponse(t *testing.T) {
	raw := []byte{0xC0, 0x00, 0x02, 0x00, 0x02, 0x00, 0x00, 0x01, 0x00, 0x03,
		0x01, 0x01, 0x00, 0x00, 0x12, 0x34, 0x56, 0x78}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsResponse() || f.Header.SID != 3 {
		t.Fatalf("header = %+v", f.Header)
	}
	if f.EndCode != 0 || !bytes.Equal(f.Data, []byte{0x12, 0x34, 0x56, 0x78}) {
		t.Fatalf("end=%04X data=%s", f.EndCode, hex(f.Data))
	}
}

func TestParseEndCode(t *testing.T) {
	raw := []byte{0xC0, 0x00, 0x02, 0x00, 0x02, 0x00, 0x00, 0x01, 0x00, 0x03,
		0x01, 0x01, 0x11, 0x03}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.EndCode != 0x1103 {
		t.Fatalf("end code = %04X", f.EndCode)
	}
	e := &EndCodeError{Code: f.EndCode}
	if !e.Address() || e.Link() {
		t.Fatalf("classification: address=%v link=%v", e.Address(), e.Link())
	}
	if got := EndCodeText(0x1103); got == "unknown end code" {
		t.Fatalf("text = %q", got)
	}
}

func TestParseTooShort(t *testing.T) {
	if _, err := Parse([]byte{0x80, 0x00}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Parse([]byte{0xC0, 0, 2, 0, 1, 0, 0, 2, 0, 3, 1, 1, 0}); err == nil {
		t.Fatal("expected short response error")
	}
}

func TestAreaLookup(t *testing.T) {
	cases := []struct {
		in   string
		key  string
		word byte
		bit  byte
	}{
		{"DM", "dm", 0x82, 0x02},
		{"d", "dm", 0x82, 0x02},
		{"cio", "cio", 0xB0, 0x30},
		{"W", "wr", 0xB1, 0x31},
		{"H", "hr", 0xB2, 0x32},
		{"A", "ar", 0xB3, 0x33},
		{"em0", "em0", 0xA0, 0x20},
		{"em12", "em12", 0xAC, 0x2C},
		{"em", "em", 0x98, 0},
	}
	for _, c := range cases {
		a, ok := Lookup(c.in)
		if !ok {
			t.Fatalf("%s not found", c.in)
		}
		if a.Key != c.key || a.Word != c.word || a.Bit != c.bit {
			t.Fatalf("%s = %+v", c.in, a)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("unknown area accepted")
	}
}

func TestAreaCodesRoundTrip(t *testing.T) {
	for _, key := range Areas() {
		a, _ := Lookup(key)
		got, ok := ByWordCode(a.Word)
		if !ok || got.Key != a.Key {
			t.Fatalf("word code %02X -> %+v ok=%v", a.Word, got, ok)
		}
		if a.BitCapable() {
			got, ok := ByBitCode(a.Bit)
			if !ok || got.Key != a.Key {
				t.Fatalf("bit code %02X -> %+v ok=%v", a.Bit, got, ok)
			}
			wc, ok := BitToWordCode(a.Bit)
			if !ok || wc != a.Word {
				t.Fatalf("bit->word %02X = %02X ok=%v", a.Bit, wc, ok)
			}
		}
	}
}

func TestBitAccessUnsupported(t *testing.T) {
	em, _ := Lookup("em")
	if _, err := BitAccess(em); err == nil {
		t.Fatal("current EM bank bit access should be rejected")
	}
}

func TestBuildMemoryWriteBitWithBitZero(t *testing.T) {
	cio, _ := Lookup("cio")
	acc, _ := BitAccess(cio)
	frame, err := BuildMemoryWrite(CommandHeader(1, 2, 0), acc, 100, 0, 1, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	// 位访问时 data 是 1 字节；bit=0 不能被误判成字访问。
	if len(frame) != 19 {
		t.Fatalf("len = %d", len(frame))
	}
	if frame[12] != 0x30 || frame[18] != 1 {
		t.Fatalf("frame = %s", hex(frame))
	}
	// 字访问写 1 个字需要 2 字节数据。
	if _, err := BuildMemoryWrite(CommandHeader(1, 2, 0), WordAccess(cio), 100, 0, 1, []byte{1}); err == nil {
		t.Fatal("word write with 1 byte accepted")
	}
}

func TestBuildTCPFrame(t *testing.T) {
	f, _ := BuildMemoryRead(CommandHeader(1, 2, 3), WordAccess(Area{Word: 0x82}), 0, 0, 1)
	pkt, err := BuildTCPFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(pkt[:4]) != "FINS" {
		t.Fatalf("magic = % x", pkt[:4])
	}
	if got := binary.BigEndian.Uint32(pkt[4:]); got != uint32(8+len(f)) {
		t.Fatalf("length = %d", got)
	}
	th, err := ParseTCPHeader(pkt[:16])
	if err != nil {
		t.Fatal(err)
	}
	if th.Command != TCPCommandFrame || th.Error != 0 || th.Length != 8+len(f) {
		t.Fatalf("header = %+v", th)
	}
	if _, err := ParseTCPHeader([]byte("NOPE")); err == nil {
		t.Fatal("bad magic accepted")
	}
}

func TestBuildMemoryReadLimits(t *testing.T) {
	a := Area{Word: 0x82}
	if _, err := BuildMemoryRead(CommandHeader(1, 2, 0), WordAccess(a), 0, 0, 0); err == nil {
		t.Fatal("count 0 accepted")
	}
	if _, err := BuildMemoryRead(CommandHeader(1, 2, 0), WordAccess(a), 0, 16, 1); err == nil {
		t.Fatal("bit 16 accepted")
	}
	if _, err := BuildMemoryRead(CommandHeader(1, 2, 0), WordAccess(a), 0xFFFF, 0, 2); err == nil {
		t.Fatal("word overflow accepted")
	}
}

func TestTCPErrorText(t *testing.T) {
	if TCPErrorText(0) != "normal" || TCPErrorText(0x24) == "unknown tcp error" {
		t.Fatalf("text table broken")
	}
	e := &TCPError{Code: 0x20}
	if e.Error() == "" {
		t.Fatal("empty error")
	}
}
