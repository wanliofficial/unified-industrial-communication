package finscore

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	finscodec "uic/internal/codec/fins"
	"uic/internal/driver"
	"uic/internal/model"
)

// fakeTr 是一个不起眼的信封：它记录每次请求，并按「读出请求里要的字数」
// 生成应答。用它测共享层，能把「读多少、切多大块、怎么解」和「帧怎么封」
// 彻底分开——信封的真实行为由两个驱动各自的测试负责。
type fakeTr struct {
	hdr  finscodec.Header
	reqs [][]byte
	// fill 决定读应答的内容：第 i 个字的值。
	fill func(i int) uint16
	// data 是写请求收到的载荷。
	writes [][]byte
	// endCode 非 0 时所有应答都带这个结束码。
	endCode uint16
	closed  bool
}

func (f *fakeTr) Header() finscodec.Header {
	if f.hdr.ICF == 0 && f.hdr.DA2 == 0 && f.hdr.SA2 == 0 && f.hdr.SID == 0 {
		return finscodec.Header{ICF: 0x00, GCT: 2}
	}
	return f.hdr
}

func (f *fakeTr) Close() error { f.closed = true; return nil }

func (f *fakeTr) RoundTrip(ctx context.Context, req []byte) (finscodec.Frame, error) {
	if err := ctx.Err(); err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrTimeout, "request", "d", "ctx", true, err)
	}
	f.reqs = append(f.reqs, append([]byte(nil), req...))
	parsed, err := finscodec.Parse(req)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", "d", err.Error(), false, err)
	}
	if f.endCode != 0 {
		return finscodec.Frame{}, &finscodec.EndCodeError{Code: f.endCode}
	}
	if parsed.SRC == finscodec.SRCWrite {
		f.writes = append(f.writes, append([]byte(nil), parsed.Data[6:]...))
		raw := finscodec.BuildResponse(parsed, 0, nil)
		out, _ := finscodec.Parse(raw)
		return out, nil
	}
	// 读：数据前 6 字节是参数（区码 + 起始字 + 位号 + 数量）。
	if len(parsed.Data) < 6 {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", "d", "short read params", false, nil)
	}
	count := int(binary.BigEndian.Uint16(parsed.Data[4:6]))
	// 用区码表判断字/位访问，而不是「看低四位是不是 0」——DM 的 0x82 低四位
	// 也是 2，按位猜会把每一次字读都当成位读。
	_, bitAccess := finscodec.ByBitCode(parsed.Data[0])
	var data []byte
	if bitAccess {
		data = make([]byte, count)
		for i := range data {
			data[i] = 1
		}
	} else {
		data = make([]byte, count*2)
		start := int(binary.BigEndian.Uint16(parsed.Data[1:3]))
		for i := 0; i < count; i++ {
			v := uint16(0x1000 + start + i)
			if f.fill != nil {
				v = f.fill(start + i)
			}
			binary.BigEndian.PutUint16(data[i*2:], v)
		}
	}
	out, err := finscodec.Parse(finscodec.BuildResponse(parsed, 0, data))
	if err != nil {
		return finscodec.Frame{}, err
	}
	return out, nil
}

func newConn(tr *fakeTr, lim Limits) *Conn {
	cfg := driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Timeout: model.Duration(time.Second)},
		Device:  model.Device{ID: "dev", ChannelID: "ch"},
	}
	return New(cfg, tr, lim)
}

func wordSpan(id, area string, offset, length int) driver.ReadSpan {
	return driver.ReadSpan{ID: id, Points: []driver.PointRef{{
		ID:       id,
		Length:   length,
		Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: offset, Length: length, Bit: -1},
		DataType: model.TypeUint16,
	}}}
}

func TestReadWordsChunksByLimit(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, Limits{WordsPerRead: 4, WordsPerWrite: 4, BitsPerRead: 64, BitsPerWrite: 64})

	// 十个各占一字的点位，上限 4 字 → 三帧（4+4+2）。
	span := driver.ReadSpan{ID: "many"}
	for i := 0; i < 10; i++ {
		span.Points = append(span.Points, driver.PointRef{
			ID:       fmt.Sprintf("p%d", i),
			Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: i, Length: 1, Bit: -1},
			DataType: model.TypeUint16,
		})
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 10 {
		t.Fatalf("samples = %d", len(samples))
	}
	for i, s := range samples {
		if s.Quality != model.QualityGood {
			t.Fatalf("point %d quality = %q (%v)", i, s.Quality, s.Err)
		}
		if v, _ := s.Value.AsFloat(); v != float64(0x1000+i) {
			t.Fatalf("point %d = %v, want 0x%04X", i, s.Value, 0x1000+i)
		}
	}
	if len(tr.reqs) != 3 {
		t.Fatalf("frames = %d, want 3", len(tr.reqs))
	}
}

// TestWidePointIsNotSplit：一个点位跨多字时不能被切到两帧里去——一帧应答
// 只能回一个值，拆开就没法拼回来。上限小于点位宽度时宁可整块读（超帧长时
// 对端会明确报「帧太长」，比「响应比请求短」好查得多）。
func TestWidePointIsNotSplit(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, Limits{WordsPerRead: 4, WordsPerWrite: 4, BitsPerRead: 64, BitsPerWrite: 64})
	samples, err := c.Read(context.Background(), []driver.ReadSpan{wordSpan("wide", "dm", 0, 10)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(tr.reqs) != 1 {
		t.Fatalf("frames = %d, want 1 whole read", len(tr.reqs))
	}
	if samples[0].Quality != model.QualityGood {
		t.Fatalf("quality = %q (%v)", samples[0].Quality, samples[0].Err)
	}
	// 请求里回显的字数必须是 10，不是上限 4。
	if got := int(binary.BigEndian.Uint16(tr.reqs[0][16:18])); got != 10 {
		t.Fatalf("requested %d words, want 10", got)
	}
}

// TestReadBitmapSharesOneFrame 检查同一字上的多个位点位被合并成一次事务。
func TestReadBitmapSharesOneFrame(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	span := driver.ReadSpan{ID: "bits", Points: []driver.PointRef{
		{ID: "b0", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 0}, DataType: model.TypeBool},
		{ID: "b1", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 1}, DataType: model.TypeBool},
		{ID: "b2", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 2}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(tr.reqs) != 1 {
		t.Fatalf("frames = %d, want 1 (all three bits are on the same word)", len(tr.reqs))
	}
	for i, s := range samples {
		if !s.Value.Bool {
			t.Fatalf("bit %d = false", i)
		}
	}
}

// TestReadMixesWordAndBitPoints：FINS 的同一个区既能按字读也能按位读，两种
// 点位混在一个 span 里时必须分成两次事务，并各自回填到正确的位置。
func TestReadMixesWordAndBitPoints(t *testing.T) {
	tr := &fakeTr{fill: func(i int) uint16 { return 0xFFFF }}
	c := newConn(tr, DefaultLimits())
	span := driver.ReadSpan{ID: "mix", Points: []driver.PointRef{
		{ID: "w", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
		{ID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: 3}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d", len(samples))
	}
	if samples[0].PointID != "w" || samples[1].PointID != "b" {
		t.Fatalf("order = %s,%s", samples[0].PointID, samples[1].PointID)
	}
	if v, _ := samples[0].Value.AsFloat(); v != 0xFFFF {
		t.Fatalf("word = %v", samples[0].Value)
	}
	if !samples[1].Value.Bool {
		t.Fatal("bit = false")
	}
	if len(tr.reqs) != 2 {
		t.Fatalf("frames = %d, want 2 (one word read + one bit read)", len(tr.reqs))
	}
}

// TestReadSpanFailureKeepsOtherSpans：一串 span 里有一个坏地址，其余的好
// span 必须仍然有数据——现场最需要的是「哪几个点坏了」。
func TestReadSpanFailureKeepsOtherSpans(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	samples, err := c.Read(context.Background(), []driver.ReadSpan{
		wordSpan("good", "dm", 0, 1),
		wordSpan("bad", "nope", 0, 1),
	})
	if err != nil {
		t.Fatalf("a partially failing batch must not fail as a whole: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d", len(samples))
	}
	if samples[0].Quality != model.QualityGood {
		t.Fatalf("good span quality = %q", samples[0].Quality)
	}
	if samples[1].Quality != model.QualityAddressError {
		t.Fatalf("bad span quality = %q", samples[1].Quality)
	}
}

// TestReadAllSpansFailReturnsError：全部失败时必须把错误交出去，否则调度器
// 会以为设备还好着。
func TestReadAllSpansFailReturnsError(t *testing.T) {
	tr := &fakeTr{endCode: 0x0202}
	c := newConn(tr, DefaultLimits())
	_, err := c.Read(context.Background(), []driver.ReadSpan{wordSpan("p", "dm", 0, 1)})
	if err == nil {
		t.Fatal("all-failing batch must return an error")
	}
}

func TestWriteWithoutLengthInfersWidth(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "p",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1},
		DataType: model.TypeUint32,
		Value:    model.NumValue(model.TypeUint32, 0x12345678),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("res = %+v", res[0])
	}
	if len(tr.writes) != 1 || len(tr.writes[0]) != 4 {
		t.Fatalf("payload = % x (want 4 bytes for a uint32)", tr.writes)
	}
}

// TestWriteStringChunks：字符串很容易超过窄信封的上限，必须拆帧而不是报错。
func TestWriteStringChunks(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, Limits{WordsPerRead: 4, WordsPerWrite: 3, BitsPerRead: 64, BitsPerWrite: 64})
	text := "0123456789ABCDEF" // 16 字符 = 8 字
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "s",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 8, Bit: -1},
		DataType: model.TypeString,
		Value:    model.TextValue(model.TypeString, text),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("res = %+v", res[0])
	}
	if len(tr.writes) != 3 {
		t.Fatalf("frames = %d, want 3 (8 words at 3 per frame)", len(tr.writes))
	}
	var got []byte
	for _, w := range tr.writes {
		got = append(got, w...)
	}
	if string(got) != text {
		t.Fatalf("payload = %q, want %q", got, text)
	}
}

// TestWriteBitOnWordOnlyAreaDoesReadModifyWrite：当前 EM 区没有位访问码，
// 只能「读整字 → 改目标位 → 写整字」。邻位必须保留。
func TestWriteBitOnWordOnlyAreaDoesReadModifyWrite(t *testing.T) {
	tr := &fakeTr{fill: func(int) uint16 { return 0b1010 }}
	c := newConn(tr, DefaultLimits())
	if _, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "b",
		Address:  model.Address{Kind: model.AddressRegister, Area: "em", Offset: 0, Bit: 1},
		DataType: model.TypeBool,
		Value:    model.BoolValue(false),
	}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(tr.writes) != 1 {
		t.Fatalf("frames = %d, want 1", len(tr.writes))
	}
	if got := binary.BigEndian.Uint16(tr.writes[0]); got != 0b1000 {
		t.Fatalf("word = %04b, want 1000 (bit 1 cleared, bit 3 kept)", got)
	}
}

func TestWriteFailsOnUnknownArea(t *testing.T) {
	c := newConn(&fakeTr{}, DefaultLimits())
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "nope", Offset: 0, Bit: -1},
		DataType: model.TypeUint16,
		Value:    model.NumValue(model.TypeUint16, 1),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("err = %v", err)
	}
	if res[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %q", res[0].Quality)
	}
}

// TestWriteFailureMarksAllResults：一条写失败后，同一批里「已经成功」的条目
// 也要带上错误——否则调用方会以为整批只有一条坏了。
func TestWriteFailureMarksAllResults(t *testing.T) {
	tr := &fakeTr{endCode: 0x1103}
	c := newConn(tr, DefaultLimits())
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1)},
		{PointID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 2)},
	})
	if err == nil {
		t.Fatal("failing batch must return an error")
	}
	if len(res) != 2 {
		t.Fatalf("results = %d", len(res))
	}
	for i := range res {
		if res[i].Err == nil {
			t.Fatalf("result %d has no error: %+v", i, res[i])
		}
	}
}

func TestProbeReadsDM0(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(tr.reqs) != 1 {
		t.Fatalf("frames = %d", len(tr.reqs))
	}
	if tr.reqs[0][12] != 0x82 {
		t.Fatalf("probe read area code = 0x%02X, want 0x82 (DM)", tr.reqs[0][12])
	}
}

func TestCloseClosesTransactor(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !tr.closed {
		t.Fatal("transactor was not closed")
	}
}

func TestAreaErrorListsSupportedAreas(t *testing.T) {
	c := newConn(&fakeTr{}, DefaultLimits())
	err := c.areaError("read", "nope")
	msg := err.Error()
	for _, want := range []string{"nope", "dm", "em12"} {
		if !contains(msg, want) {
			t.Fatalf("message %q does not mention %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
