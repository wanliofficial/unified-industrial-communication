package mewtocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeStream 是最小的 transport.Stream 实现：按字节从一段固定缓冲区里
// 交货，交货完返回 io.EOF。它能精确控制「一次读到多少」，从而复现现场
// 最常见的两种故障：噪声前缀、一帧分多次到达。
type fakeStream struct {
	buf []byte
	pos int
	// chunk 表示每次 ReadFull(n) 最多返回多少字节（0 表示不限）。
	chunk int
}

func (f *fakeStream) Write(p []byte) error { return nil }

func (f *fakeStream) ReadFull(n int) ([]byte, error) {
	if f.pos >= len(f.buf) {
		return nil, io.EOF
	}
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

func frame(s string) []byte { return AppendBCC([]byte(s)) }

// 正常路径：一帧完整到达。
func TestReaderReadsOneFrame(t *testing.T) {
	want := frame("%01$RD0063")
	r := NewReader(&fakeStream{buf: want})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

// 一帧分多次到达：Reader 必须等到 CR 才返回，不能把半帧当整帧。
func TestReaderHandlesPartialDelivery(t *testing.T) {
	want := frame("%01$RD006344330A00")
	for _, chunk := range []int{1, 2, 3, 5} {
		r := NewReader(&fakeStream{buf: want, chunk: chunk})
		got, err := r.Next()
		if err != nil {
			t.Fatalf("chunk=%d: %v", chunk, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk=%d: frame = %q, want %q", chunk, got, want)
		}
	}
}

// 噪声前缀：必须丢弃，不能把它当成帧的一部分。
//
// 这是半双工总线上最重要的一条。噪声可能来自上一轮迟到的应答、别的主站
// 的串话、或者上电时的电平抖动；如果 Reader 把第一个 CR 之前的所有字节
// 都当一帧，解析出来的站号就是垃圾，而错误会表现为「站号错」——把人
// 引向完全错误的方向。
func TestReaderSkipsNoise(t *testing.T) {
	want := frame("%01$RD0063")
	noisy := append([]byte("garbage\x00\xff"), want...)
	r := NewReader(&fakeStream{buf: noisy})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = %q, want %q", got, want)
	}
	if r.Dropped() != len("garbage\x00\xff") {
		t.Fatalf("Dropped() = %d, want %d", r.Dropped(), len("garbage\x00\xff"))
	}
}

// 两帧连在一起（对端把两条应答挤在同一次发送里）：Reader 必须一帧一帧
// 交出去，不能把两帧并成一帧。
func TestReaderReturnsConsecutiveFrames(t *testing.T) {
	f1 := frame("%01$RD0063")
	f2 := frame("%01$WD")
	r := NewReader(&fakeStream{buf: append(append([]byte(nil), f1...), f2...)})
	for i, want := range [][]byte{f1, f2} {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %q, want %q", i, got, want)
		}
	}
}

// 一帧之后跟着一段噪声再接一帧：中间那段必须丢弃。
func TestReaderSkipsNoiseBetweenFrames(t *testing.T) {
	f1 := frame("%01$RD0063")
	f2 := frame("%01$RC00")
	stream := append(append(append([]byte(nil), f1...), []byte("\x00\x11junk")...), f2...)
	r := NewReader(&fakeStream{buf: stream})
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f2) {
		t.Fatalf("second frame = %q, want %q", got, f2)
	}
	if r.Dropped() == 0 {
		t.Fatal("noise between frames should be counted as dropped")
	}
}

// 一直不来的 CR：必须在 MaxFrame 处放弃，而不是无限吞字节。
func TestReaderGivesUpWithoutTerminator(t *testing.T) {
	big := append([]byte{'%'}, bytes.Repeat([]byte{'A'}, MaxFrame+10)...)
	r := NewReader(&fakeStream{buf: big})
	if _, err := r.Next(); err == nil {
		t.Fatal("expected an error for a frame with no terminator")
	}
}

// 流结束（EOF）必须原样返回，不能被误当成「还没读完」。
func TestReaderPropagatesEOF(t *testing.T) {
	r := NewReader(&fakeStream{})
	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// 只收到头部就断了：返回 EOF，且缓冲区里那半个字节不产生错误帧。
func TestReaderPropagatesEOFMidFrame(t *testing.T) {
	r := NewReader(&fakeStream{buf: []byte("%01$RD0063")})
	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// 长帧头 `<` 也要认：FP7 的 LAN 通信会发扩展头。
func TestReaderAcceptsLongHeader(t *testing.T) {
	want := append([]byte("<01$RD0063"), 0x00, '\r')
	want = assignBCC(want)
	r := NewReader(&fakeStream{buf: want})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

// assignBCC 重算并写入 BCC 位（倒数第三、第二位）。
func assignBCC(frame []byte) []byte {
	body := frame[:len(frame)-3]
	b := BCC(body)
	out := append([]byte(nil), frame...)
	out[len(out)-3] = hexDigits[b>>4]
	out[len(out)-2] = hexDigits[b&0x0f]
	return out
}

// FrameLen 的边界：不含 CR 时返回 false，含 CR 时返回含 CR 的长度。
func TestFrameLen(t *testing.T) {
	if _, ok := FrameLen([]byte("%01$RD0063")); ok {
		t.Fatal("frame without CR should not be complete")
	}
	if n, ok := FrameLen([]byte("%01$RD0063\r")); !ok || n != 11 {
		t.Fatalf("FrameLen = %d,%v, want 11,true", n, ok)
	}
	if n, ok := FrameLen([]byte("%01$RD0063\rMORE")); !ok || n != 11 {
		t.Fatalf("FrameLen with trailing data = %d,%v, want 11,true", n, ok)
	}
}

// 噪声里恰好含 `%`：不能因此认定是一帧的开头，还要等 CR；等到 CR 后
// 解析会失败——但那属于调用方的事，Reader 只负责按 CR 切帧。
func TestReaderSplitsOnCRNotOnHeader(t *testing.T) {
	// 这段噪声里有一个 `%`，后面紧接真帧。
	want := frame("%01$RC00")
	noisy := append([]byte("%noise"), want...)
	r := NewReader(&fakeStream{buf: noisy})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	// 首帧会是 "%noise%01$RC00"，解析必然失败；这里只断言「Reader 按 CR
	// 切出了东西」这个事实，把语义判断留给 ParseResponse。
	if !bytes.HasSuffix(got, []byte("\r")) {
		t.Fatalf("frame should end with CR: %q", got)
	}
}

// 诊断计数不应把正常字节算成噪声。
func TestReaderDroppedStaysZero(t *testing.T) {
	r := NewReader(&fakeStream{buf: frame("%01$RD0063")})
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if r.Dropped() != 0 {
		t.Fatalf("Dropped() = %d, want 0", r.Dropped())
	}
	if !strings.HasPrefix(string(frame("%01$RD0063")), "%01$") {
		t.Fatal("sanity")
	}
}
