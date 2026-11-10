package hostlink

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeStream 是最小的 transport.Stream 实现：按字节从一段固定缓冲区交货，
// 交货完返回 io.EOF。它能精确控制「一次读到多少」，从而复现现场最常见的
// 两种故障：噪声前缀、一帧分多次到达。
type fakeStream struct {
	buf   []byte
	pos   int
	chunk int // 每次 ReadFull(n) 最多返回多少字节（0 表示不限）
	// maxReads 为 0 时不限；>0 时读到上限后返回 io.EOF，用来模拟「对端
	// 一直不说话」而不必真的等超时。
	maxReads int
	reads    int
}

func (f *fakeStream) Write(p []byte) error { return nil }

func (f *fakeStream) ReadFull(n int) ([]byte, error) {
	if f.pos >= len(f.buf) || (f.maxReads > 0 && f.reads >= f.maxReads) {
		return nil, io.EOF
	}
	f.reads++
	want := n
	if f.chunk > 0 && want > f.chunk {
		want = f.chunk
	}
	end := f.pos + want
	if end > len(f.buf) {
		end = len(f.buf)
	}
	out := append([]byte(nil), f.buf[f.pos:end]...)
	f.pos = end
	return out, nil
}

func (f *fakeStream) Close() error { return nil }

func TestReaderReadsOneFrame(t *testing.T) {
	want := withCR(goldenReadResponse)
	r := NewReader(&fakeStream{buf: want})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = %q, want %q", got, want)
	}
	if r.Dropped() != 0 {
		t.Fatalf("Dropped = %d, want 0", r.Dropped())
	}
}

// TestReaderHandlesPartialDelivery：一帧分多次到达时不能把半帧当整帧。
func TestReaderHandlesPartialDelivery(t *testing.T) {
	want := withCR(goldenReadResponse)
	for _, chunk := range []int{1, 2, 3, 5, 8} {
		r := NewReader(&fakeStream{buf: append([]byte(nil), want...), chunk: chunk})
		got, err := r.Next()
		if err != nil {
			t.Fatalf("chunk=%d: %v", chunk, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk=%d: frame = %q", chunk, got)
		}
	}
}

// TestReaderDropsNoisePrefix：`@` 之前的一切都是噪声，必须丢弃并计数。
func TestReaderDropsNoisePrefix(t *testing.T) {
	noise := []byte("garbage\x00\xff\r\n")
	r := NewReader(&fakeStream{buf: append(noise, withCR(goldenReadResponse)...)})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, withCR(goldenReadResponse)) {
		t.Fatalf("frame = %q", got)
	}
	if r.Dropped() != len(noise) {
		t.Fatalf("Dropped = %d, want %d", r.Dropped(), len(noise))
	}
}

// TestReaderIgnoresBareCR 是这套协议最容易踩的一个坑：`*`+CR 才是帧尾，
// 裸 CR 只是分段/噪声。按第一个 CR 收尾会把一帧切成两半，FCS 永远不对。
func TestReaderIgnoresBareCR(t *testing.T) {
	// 在正文里插一个裸 CR：真实场景是分段定界符或噪声。
	body := "@00FA" + "00" + "40" + "00" + "00" + "00" + "0101" + "0000"
	sum := FCS([]byte(body))
	frame := withCR(body + "\r" + hexUpper(sum) + "*")
	r := NewReader(&fakeStream{buf: frame})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("frame = %q, want the whole frame including the bare CR", got)
	}
}

// TestReaderRecoversAfterTrailingCRLF：有的串口服务器在 `*CR` 后补一个 LF，
// 下一轮读到的第一个字节就是 LF；不能被当成新帧的开头。
func TestReaderRecoversAfterTrailingCRLF(t *testing.T) {
	a := append(withCR(goldenReadResponse), '\n')
	second := "@00FA" + "00" + "40" + "00" + "00" + "00" + "0101" + "0000" + "1234"
	sum := FCS([]byte(second))
	b := append(withCR(second+hexUpper(sum)+"*"), '\n')

	r := NewReader(&fakeStream{buf: append(a, b...)})
	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(withCR(goldenReadResponse)) {
		t.Fatalf("first = %q", first)
	}
	secondFrame, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(secondFrame) != string(withCR(second+hexUpper(sum)+"*")) {
		t.Fatalf("second = %q", secondFrame)
	}
	// 第一帧后面的 LF 被丢掉了。Reader 是逐字节取货的，所以帧一被切走
	// 缓冲区就空了——这里的计数恰好只统计「确实扫描过」的噪声。
	if r.Dropped() != 1 {
		t.Fatalf("Dropped = %d, want 1", r.Dropped())
	}
	if r.Buffered() != 0 {
		t.Fatalf("Buffered = %d, want 0", r.Buffered())
	}
}

// TestReaderBackToBackFrames：两帧连在一起到达，不能粘成一帧。
func TestReaderBackToBackFrames(t *testing.T) {
	a := withCR(goldenReadResponse)
	r := NewReader(&fakeStream{buf: append(append([]byte(nil), a...), a...)})
	for i := 0; i < 2; i++ {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, a) {
			t.Fatalf("frame %d = %q", i, got)
		}
	}
}

// TestReaderGivesUpOnRunawayInput：一直没有 `*`+CR 时必须放弃，否则一段
// 噪声会让驱动无限吞字节、永远不返回。
func TestReaderGivesUpOnRunawayInput(t *testing.T) {
	r := NewReader(&fakeStream{buf: bytes.Repeat([]byte{'@'}, MaxFrame+10)})
	_, err := r.Next()
	if err == nil {
		t.Fatal("runaway input must produce an error")
	}
	if !strings.Contains(err.Error(), "no frame terminator") {
		t.Fatalf("err = %v", err)
	}
}

func TestReaderReadFrameChecksUnitAndSID(t *testing.T) {
	r := NewReader(&fakeStream{buf: withCR(goldenReadResponse)})
	if _, err := r.ReadFrame(3, 0); err == nil {
		t.Fatal("unit mismatch must be an error")
	} else {
		var mm *UnitMismatch
		if !errors.As(err, &mm) {
			t.Fatalf("err = %v, want *UnitMismatch", err)
		}
	}

	r = NewReader(&fakeStream{buf: withCR(goldenReadResponse)})
	if _, err := r.ReadFrame(0, 9); err == nil {
		t.Fatal("SID mismatch must be an error")
	} else {
		var mm *SIDMismatch
		if !errors.As(err, &mm) {
			t.Fatalf("err = %v, want *SIDMismatch", err)
		}
	}

	r = NewReader(&fakeStream{buf: withCR(goldenReadResponse)})
	if _, err := r.ReadFrame(0, 0); err != nil {
		t.Fatalf("matching unit/SID must pass: %v", err)
	}
}

func TestReaderReadFrameSurfacesEndCode(t *testing.T) {
	body := "@00FA" + "14" + "40" + "00" + "00" + "00" + "0101" + "0000" + "1234"
	sum := FCS([]byte(body))
	r := NewReader(&fakeStream{buf: withCR(body + hexUpper(sum) + "*")})
	_, err := r.ReadFrame(0, 0)
	var ec *EndCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("err = %v, want *EndCodeError", err)
	}
	if ec.Code != 0x14 || !ec.Format() {
		t.Fatalf("code = 0x%02X format = %v", ec.Code, ec.Format())
	}
}

func TestFrameLenRejectsNonStart(t *testing.T) {
	if _, ok := FrameLen([]byte("x00FA")); ok {
		t.Fatal("a frame not starting with @ must not be considered complete")
	}
}

func TestFrameLenNeedsStarBeforeCR(t *testing.T) {
	if _, ok := FrameLen([]byte("@00FA0000\r")); ok {
		t.Fatal("bare CR must not terminate a frame")
	}
}
