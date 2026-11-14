package toledo

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeStream 是最小的 transport.Stream 实现：按字节从固定缓冲区交货，交货完
// 返回 io.EOF。它能精确控制「一次读到多少」，从而复现现场最常见的两种故障：
// 一帧分多次到达、线上噪声前缀。
type fakeStream struct {
	buf   []byte
	pos   int
	chunk int // 每次 ReadFull(n) 最多返回多少字节（0 表示不限）
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

// 正常路径：一帧完整到达，含行尾 CR LF。
func TestReaderReadsOneFrame(t *testing.T) {
	want := []byte("ST,GS,+00149.0kg\r\n")
	r := NewReader(&fakeStream{buf: want})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("ST,GS,+00149.0kg")) {
		t.Fatalf("frame = %q, want %q", got, "ST,GS,+00149.0kg")
	}
}

// 一帧分多次到达：Reader 必须等到 CR/LF 才返回，不能把半帧当整帧。
func TestReaderHandlesPartialDelivery(t *testing.T) {
	want := []byte("ST,NT,-00012.5kg\r\n")
	for _, chunk := range []int{1, 2, 3, 5} {
		r := NewReader(&fakeStream{buf: want, chunk: chunk})
		got, err := r.Next()
		if err != nil {
			t.Fatalf("chunk=%d: %v", chunk, err)
		}
		if !bytes.Equal(got, []byte("ST,NT,-00012.5kg")) {
			t.Fatalf("chunk=%d: frame = %q", chunk, got)
		}
	}
}

// 噪声前缀必须丢弃，不能当成帧的一部分。
//
// 这是串着多设备的线上最重要的一条。噪声可能来自另一台设备的串话、上电时的
// 横幅、或电平抖动。如果 Reader 把第一个 CR/LF 之前的所有字节都当一帧，解析
// 出来的读数就是垃圾，而错误会表现为「值看着合理、其实全错」。噪声不能含状态
// 首字母 S/T/U/O/L，否则会被当成帧起点（这是首字母初筛的已知取舍）。
func TestReaderSkipsNoise(t *testing.T) {
	want := []byte("ST,GS,+00149.0kg\r\n")
	noisy := append([]byte("\x00\x01welcome\x00\xff"), want...)
	r := NewReader(&fakeStream{buf: noisy})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("ST,GS,+00149.0kg")) {
		t.Fatalf("frame = %q, want %q", got, "ST,GS,+00149.0kg")
	}
	if r.Dropped() != len("\x00\x01welcome\x00\xff") {
		t.Fatalf("Dropped() = %d, want %d", r.Dropped(), len("\x00\x01welcome\x00\xff"))
	}
}

// 两帧连在一起：Reader 必须一帧一帧交出去，并把 CRLF 都消费掉（不产空帧）。
func TestReaderReturnsConsecutiveFrames(t *testing.T) {
	stream := []byte("ST,GS,+00149.0kg\r\nST,US,+00150.0kg\r\n")
	r := NewReader(&fakeStream{buf: stream})
	for _, want := range []string{"ST,GS,+00149.0kg", "ST,US,+00150.0kg"} {
		got, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("frame = %q, want %q", got, want)
		}
	}
}

// 噪声里恰好含一个状态首字母：不能因此认定帧结束，仍要等到 CR/LF。
func TestReaderSplitsOnCRLFNotOnHeader(t *testing.T) {
	// 噪声 "SomeNoise" 里有一个 S，Reader 应等 CR 才切帧。
	stream := []byte("SomeNoise\rST,GS,+00149.0kg\r\n")
	r := NewReader(&fakeStream{buf: stream})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	// 首个合法首字节是 "SomeNoise" 的 S，切到第一个 CR → "SomeNoise"。
	if string(got) != "SomeNoise" {
		t.Fatalf("frame = %q, want %q", got, "SomeNoise")
	}
	// 下一帧是真读数。
	got, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ST,GS,+00149.0kg" {
		t.Fatalf("second frame = %q, want %q", got, "ST,GS,+00149.0kg")
	}
}

// 一直不来的 CR/LF：必须在 MaxFrame 处放弃，而不是无限吞字节。
func TestReaderGivesUpWithoutTerminator(t *testing.T) {
	big := append([]byte("ST"), bytes.Repeat([]byte{'A'}, MaxFrame+10)...)
	r := NewReader(&fakeStream{buf: big})
	if _, err := r.Next(); err == nil {
		t.Fatal("expected an error for a frame with no terminator")
	}
}

// 流结束（EOF）必须原样返回，不能被误当成「还没读完」。
func TestReaderPropagatesEOF(t *testing.T) {
	r := NewReader(&fakeStream{})
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// 只收到部分帧就断了：返回 EOF，缓冲区里那半截不产生错误帧。
func TestReaderPropagatesEOFMidFrame(t *testing.T) {
	r := NewReader(&fakeStream{buf: []byte("ST,GS,+0014")})
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// 诊断计数不应把正常字节算成噪声。
func TestReaderDroppedStaysZero(t *testing.T) {
	r := NewReader(&fakeStream{buf: []byte("ST,GS,+00149.0kg\r\n")})
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if r.Dropped() != 0 {
		t.Fatalf("Dropped() = %d, want 0", r.Dropped())
	}
}

// 只用 LF 作行尾也要认（部分仪表只发 LF）。
func TestReaderAcceptsLoneLF(t *testing.T) {
	r := NewReader(&fakeStream{buf: []byte("ST,GS,+00149.0kg\n")})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), "kg") {
		t.Fatalf("frame = %q", got)
	}
}

// 帧内数值段含 '.'，不应被误判为行尾。
func TestReaderDotNotTreatedAsTerminator(t *testing.T) {
	r := NewReader(&fakeStream{buf: []byte("ST,GS,+00149.0kg\r\n")})
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("ST,GS,+00149.0kg")) {
		t.Fatalf("frame = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 收帧诊断计数
// ---------------------------------------------------------------------------

// BytesRead 统计从链路真正读到的原始字节数（含被当噪声丢掉的）。
//
// 它是驱动区分「链路上没有字节」与「有字节但一帧都解不出」的唯一依据：
// 前者查接线/供电，后者查仪表帧格式。少统计这部分，驱动就只能在两种故障下
// 都报超时，把现场引向错误的排查方向。
//
// 注意语义是「已从链路读走的字节」，不是「线上总共有多少字节」：分帧是逐字节
// 读的，遇到 CR 就返回成帧，尾随的 LF 要等下一次 Next 才会被读走。
func TestReaderCountsBytesRead(t *testing.T) {
	data := []byte("XYST,GS,+00149.0kg\r\n")
	r := NewReader(&fakeStream{buf: data})
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	// 2 字节噪声 + 16 字节帧体 + 1 字节 CR = 19；LF 尚未读入。
	if got, want := r.BytesRead(), len(data)-1; got != want {
		t.Fatalf("BytesRead() = %d，应为已读入的 %d 字节（尾随 LF 尚未读）", got, want)
	}
	if got := r.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d，应为 2（前导 \"XY\"）", got)
	}
	// 再取一次：把尾随 LF 读进来后 BytesRead 补齐，然后 EOF。
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("第二次 Next 应返回 EOF，得到 %v", err)
	}
	if got := r.BytesRead(); got != len(data) {
		t.Fatalf("读完尾随 LF 后 BytesRead() = %d，应为 %d", got, len(data))
	}
}

// 一帧都没成（全是噪声）时：BytesRead > 0 且 Dropped == BytesRead，没有成帧。
//
// 这正是「仪表被设成二进制格式 / 波特率配错」在收帧层的表现，驱动据此报
// ErrProtocol 而不是 ErrTimeout。
func TestReaderCountsBytesWhenNoFrameForms(t *testing.T) {
	// 小写状态 + 任意字节：isFrameStart 只认大写 S/T/U/O/L 与 STX，
	// 所以这些字节会被全部当噪声丢掉。
	noise := []byte("lorem ipsum dolor sit amet\r\n")
	r := NewReader(&fakeStream{buf: noise})
	_, err := r.Next()
	if err == nil {
		t.Fatal("无合法帧起点应返回错误（此处是 EOF）")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v，应为 EOF", err)
	}
	if r.BytesRead() == 0 {
		t.Fatal("BytesRead() 必须记录读到的字节，否则驱动无法区分「没字节」与「解不出」")
	}
	if r.Dropped() == 0 {
		t.Fatal("全部字节都应计入 Dropped")
	}
}

// 二进制 STX 帧在收帧层被当作合法起点切出（CR 定界），由解析层拒绝。
//
// 这条边界很重要：收帧层不能把 STX 当噪声丢掉——那样驱动就只能看到「没有
// 帧」，而看不到「收到的是二进制帧」，那句「请切到 ASCII」的指引就永远发不出去。
func TestReaderAcceptsSTXAsFrameStart(t *testing.T) {
	frame := []byte{0x02, 'S', 'T', ' ', '0', '0', '1', '4', '9', 0x0D}
	r := NewReader(&fakeStream{buf: frame})
	got, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(got) == 0 || got[0] != 0x02 {
		t.Fatalf("frame = %x，应以 STX 开头", got)
	}
	// 交给解析层：必须得到「切到 ASCII」的可执行指引，而不是被硬解成垃圾值。
	if _, perr := Parse(got); perr == nil {
		t.Fatal("二进制帧不应被 Parse 接受")
	} else if !strings.Contains(perr.Error(), "ASCII") {
		t.Fatalf("解析错误应给出切到 ASCII 的指引，得到：%v", perr)
	}
}
