package fins

import (
	"bytes"
	"strings"
	"testing"
)

func TestResponseHeaderSwapsEndpoints(t *testing.T) {
	req := CommandHeader(0x11, 0x22, 0x33)
	resp := ResponseHeader(req)
	if resp.ICF != 0xC0 {
		t.Fatalf("ICF = %02X", resp.ICF)
	}
	if resp.DA1 != 0x22 || resp.SA1 != 0x11 {
		t.Fatalf("nodes = da %02X sa %02X", resp.DA1, resp.SA1)
	}
	if resp.SID != 0x33 || resp.GCT != req.GCT {
		t.Fatalf("sid/gct = %02X/%02X", resp.SID, resp.GCT)
	}
}

func TestBuildResponseGolden(t *testing.T) {
	req := Frame{Header: CommandHeader(1, 2, 3), MRC: 0x01, SRC: 0x02}
	frame := BuildResponse(req, 0x1103, nil)
	if len(frame) != 14 {
		t.Fatalf("len = %d", len(frame))
	}
	want := []byte{0xC0, 0x00, 0x02, 0x00, 0x02, 0x00, 0x00, 0x01, 0x00, 0x03,
		0x01, 0x02, 0x11, 0x03}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = %s\nwant    %s", hex(frame), hex(want))
	}
}

func TestBuildNodeError(t *testing.T) {
	got := BuildNodeError(0x24)
	if len(got) != 16 {
		t.Fatalf("len = %d", len(got))
	}
	th, err := ParseTCPHeader(got)
	if err != nil {
		t.Fatal(err)
	}
	if th.Length != 8 || th.Command != TCPCommandNodeResponse || th.Error != 0x24 {
		t.Fatalf("header = %+v", th)
	}
	// 长度 8 表示「没有节点号」，握手时不能再读 8 字节负载。
	if th.Length-8 != 0 {
		t.Fatalf("payload = %d", th.Length-8)
	}
}

func TestEndCodeErrorText(t *testing.T) {
	e := &EndCodeError{Code: 0x1103}
	if !strings.Contains(e.Error(), "0x1103") || !strings.Contains(e.Error(), "out of range") {
		t.Fatalf("error = %q", e.Error())
	}
	// 分类表是安全边界：链路错不该被当成地址错（这决定重试策略）。
	link := &EndCodeError{Code: 0x0301}
	if !link.Link() || link.Address() {
		t.Fatalf("link classification: link=%v address=%v", link.Link(), link.Address())
	}
	for _, code := range []uint16{0x0101, 0x0201, 0x0202, 0x0203, 0x0205, 0x0302,
		0x0401, 0x1101, 0x1102, 0x1104} {
		if EndCodeText(code) == "unknown end code" {
			t.Fatalf("code %04X has no text", code)
		}
	}
	if EndCodeText(0x7777) != "unknown end code" {
		t.Fatalf("unknown code text = %q", EndCodeText(0x7777))
	}
}

func TestTCPErrorTextTable(t *testing.T) {
	for _, code := range []uint32{0x01, 0x02, 0x03, 0x20, 0x21, 0x22, 0x23, 0x25} {
		if TCPErrorText(code) == "unknown tcp error" {
			t.Fatalf("code %02X has no text", code)
		}
	}
	if TCPErrorText(0x7777) != "unknown tcp error" {
		t.Fatalf("unknown code text = %q", TCPErrorText(0x7777))
	}
	if (&TCPError{Code: 0x21}).Error() == "" {
		t.Fatal("empty TCPError text")
	}
}

func TestBuildTCPFrameTooLarge(t *testing.T) {
	// MaxPayload 是含 8 字节「命令 + 错误码」的整帧上限。
	if _, err := BuildTCPFrame(make([]byte, MaxPayload-8)); err != nil {
		t.Fatalf("boundary rejected: %v", err)
	}
	if _, err := BuildTCPFrame(make([]byte, MaxPayload-7)); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestParseTCPHeaderRejects(t *testing.T) {
	bad := make([]byte, 16)
	copy(bad, TCPMagic)
	// 长度 4：连「命令 + 错误码」都装不下。
	bad[7] = 4
	if _, err := ParseTCPHeader(bad); err == nil {
		t.Fatal("short length accepted")
	}
	// 长度超过 MaxPayload。
	bad[6], bad[7] = 0x7F, 0xFF
	if _, err := ParseTCPHeader(bad); err == nil {
		t.Fatal("oversized length accepted")
	}
	if _, err := ParseTCPHeader(make([]byte, 8)); err == nil {
		t.Fatal("short header accepted")
	}
}

func TestAreaLookupUnknownCodes(t *testing.T) {
	if _, ok := ByWordCode(0xEE); ok {
		t.Fatal("unknown word code accepted")
	}
	if _, ok := ByBitCode(0xEE); ok {
		t.Fatal("unknown bit code accepted")
	}
	if _, ok := ByBitCode(0x98); ok {
		t.Fatal("word-only EM code treated as bit code")
	}
	if _, ok := BitToWordCode(0xEE); ok {
		t.Fatal("unknown bit code mapped")
	}
	areas := Areas()
	if len(areas) != 6+13 || areas[0] != "cio" {
		t.Fatalf("areas = %v", areas)
	}
}

func TestBuildMemoryReadBoundaries(t *testing.T) {
	cio, _ := Lookup("cio")
	bitAcc, err := BitAccess(cio)
	if err != nil {
		t.Fatal(err)
	}
	wordAcc := WordAccess(cio)
	h := CommandHeader(1, 2, 0)

	if _, err := BuildMemoryRead(h, wordAcc, 0, 0, MaxWords+1); err == nil {
		t.Fatal("word count over limit accepted")
	}
	if _, err := BuildMemoryRead(h, bitAcc, 0, 0, MaxBits+1); err == nil {
		t.Fatal("bit count over limit accepted")
	}
	// 位读跨过地址空间末尾：0xFFFF 字 + bit15 + 2 位 = 0x10000 字。
	if _, err := BuildMemoryRead(h, bitAcc, 0xFFFF, 15, 2); err == nil {
		t.Fatal("bit range overflow accepted")
	}
	// 位读正好落在最后一个字的最后一位是合法的。
	if _, err := BuildMemoryRead(h, bitAcc, 0xFFFF, 15, 1); err != nil {
		t.Fatalf("last bit rejected: %v", err)
	}
	if _, err := BuildMemoryRead(h, wordAcc, 0xFFFF, 0, 1); err != nil {
		t.Fatalf("last word rejected: %v", err)
	}
}

func TestBuildMemoryWriteLimits(t *testing.T) {
	cio, _ := Lookup("cio")
	bitAcc, _ := BitAccess(cio)
	wordAcc := WordAccess(cio)
	h := CommandHeader(1, 2, 0)

	if _, err := BuildMemoryWrite(h, wordAcc, 0, 0, MaxWords+1, make([]byte, (MaxWords+1)*2)); err == nil {
		t.Fatal("word count over limit accepted")
	}
	if _, err := BuildMemoryWrite(h, bitAcc, 0, 0, MaxBits+1, make([]byte, MaxBits+1)); err == nil {
		t.Fatal("bit count over limit accepted")
	}
	// 单帧上限：999 字的数据区加头部会超过 2012 字节。
	if _, err := BuildMemoryWrite(h, wordAcc, 0, 0, MaxWords, make([]byte, MaxWords*2)); err == nil {
		t.Fatal("oversized write accepted")
	}
	if _, err := BuildMemoryWrite(h, wordAcc, 0, 0, 0, nil); err == nil {
		t.Fatal("zero count accepted")
	}
	if _, err := BuildMemoryWrite(h, wordAcc, 0, 16, 1, make([]byte, 2)); err == nil {
		t.Fatal("bit 16 accepted")
	}
}
