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
	// failOn 非 nil 时，第 failAt 次（1 起算）事务返回它的错误，之后
	// 恢复正常。用来测「链路级失败必须短路剩余 span」。
	failOn map[int]error
	closed bool
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
	if err := f.failOn[len(f.reqs)]; err != nil {
		return finscodec.Frame{}, err
	}
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
// 会以为设备还活着。
func TestReadAllSpansFailReturnsError(t *testing.T) {
	tr := &fakeTr{endCode: 0x0202}
	c := newConn(tr, DefaultLimits())
	_, err := c.Read(context.Background(), []driver.ReadSpan{wordSpan("p", "dm", 0, 1)})
	if err == nil {
		t.Fatal("all-failing batch must return an error")
	}
}

// TestReadAllPointsBadButSpanOKReturnsError：span 本身没报错、但每个点位都在
// 解码阶段失败（EM 当前区的 bool 没有位访问码，缺位号只能判坏），整轮结果
// 里一个成功点位都没有——此时 `Read` 必须返回错误。
//
// 按 span 计数会把这个场景判成「成功」（readSpan 返回 nil error），于是上层
// 看到一台「在采但全坏」的设备，熔断器与告警永远不会动——正是 docs §3.3.1
// 点名禁止的静默。
func TestReadAllPointsBadButSpanOKReturnsError(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	span := driver.ReadSpan{ID: "em", Points: []driver.PointRef{{
		ID:       "no-bit",
		Address:  model.Address{Kind: model.AddressRegister, Area: "em", Offset: 0, Bit: -1},
		DataType: model.TypeBool,
	}}}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatalf("a batch with zero good points must return an error, got samples %+v", samples)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}
}

// TestReadLinkFailureShortCircuitsRemainingSpans：链路级失败（超时/断连）后
// 剩余 span 不再发帧（半双工串口上继续等只会把一轮采集拖成 N 倍时长），
// 并且它们必须显式出现在结果里、标成坏值——消失的点位在采集层看起来像
// 「设备安静」，会把真正的超时原因盖掉。
func TestReadLinkFailureShortCircuitsRemainingSpans(t *testing.T) {
	timeout := driver.E(driver.ErrTimeout, "request", "d", "no response", true, context.DeadlineExceeded)
	tr := &fakeTr{failOn: map[int]error{2: timeout}}
	c := newConn(tr, DefaultLimits())

	samples, err := c.Read(context.Background(), []driver.ReadSpan{
		wordSpan("good", "dm", 0, 1),
		wordSpan("timeout", "dm", 1, 1),
		wordSpan("never-sent", "dm", 2, 1),
	})
	if err == nil {
		t.Fatal("a link-level failure must be reported even when earlier spans succeeded")
	}
	if got := driver.KindOf(err); got != driver.ErrTimeout {
		t.Fatalf("kind = %q, want timeout", got)
	}
	if len(tr.reqs) != 2 {
		t.Fatalf("frames = %d, want 2 (the third span must not be sent)", len(tr.reqs))
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3 (remaining points must be marked, not dropped)", len(samples))
	}
	if samples[0].Quality != model.QualityGood {
		t.Fatalf("good span quality = %q", samples[0].Quality)
	}
	for _, s := range samples[1:] {
		if s.Quality != model.QualityTimeout {
			t.Fatalf("point %q quality = %q, want timeout", s.PointID, s.Quality)
		}
		if s.Err == nil {
			t.Fatalf("point %q must carry the link error", s.PointID)
		}
	}
}

// TestReadLinkFailureAfterAllSpansFail…：链路级失败的短路判定不能只看「还有
// 好样本」。全部 span 都超时时也走同一条分支（不再逐个等超时）。
func TestReadLinkFailureStopsAfterFirstTimeout(t *testing.T) {
	timeout := driver.E(driver.ErrTimeout, "request", "d", "no response", true, context.DeadlineExceeded)
	tr := &fakeTr{failOn: map[int]error{1: timeout}}
	c := newConn(tr, DefaultLimits())

	_, err := c.Read(context.Background(), []driver.ReadSpan{
		wordSpan("a", "dm", 0, 1),
		wordSpan("b", "dm", 1, 1),
		wordSpan("c", "dm", 2, 1),
	})
	if err == nil {
		t.Fatal("all spans timed out; Read must return an error")
	}
	if len(tr.reqs) != 1 {
		t.Fatalf("frames = %d, want 1 (no point in waiting for the other two timeouts)", len(tr.reqs))
	}
}

// TestReadContextDoneMarksRemainingSpans：ctx 已到期时剩余 span 必须显式标坏。
func TestReadContextDoneMarksRemainingSpans(t *testing.T) {
	c := newConn(&fakeTr{}, DefaultLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	samples, err := c.Read(ctx, []driver.ReadSpan{
		wordSpan("a", "dm", 0, 1),
		wordSpan("b", "dm", 1, 1),
	})
	if err == nil {
		t.Fatal("a done context must produce an error")
	}
	if got := driver.KindOf(err); got != driver.ErrTimeout {
		t.Fatalf("kind = %q, want timeout", got)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	for _, s := range samples {
		if s.Quality != model.QualityTimeout || s.Err == nil {
			t.Fatalf("point %q = %+v", s.PointID, s)
		}
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

// TestWriteFailureKeepsEarlierSuccessesIntact：批量写里后面的条目失败，不能
// 把**已经成功**的条目也标上错误（docs §3.4「逐项返回结果」）。
//
// 早先这里做的是「把 firstErr 回填进所有 Err==nil 的结果」，看起来像「整批
// 失败更安全」，实际后果是：调用方拿到的逐项结果全部是失败，会去重发一个
// 已经写下去的点位——对计数类点位就是重复累加；而真正失败的那条反而淹没在
// 一片错误里。
func TestWriteFailureKeepsEarlierSuccessesIntact(t *testing.T) {
	// 第 2 次事务（第二条写）返回地址错，第 1 次正常。
	addrErr := endCodeError(0x1103, "address out of range")
	tr := &fakeTr{failOn: map[int]error{2: addrErr}}
	c := newConn(tr, DefaultLimits())
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "ok", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 1)},
		{PointID: "bad", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 2)},
	})
	if err == nil {
		t.Fatal("a batch with a failing item must return an error")
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if !res[0].OK || res[0].Err != nil || res[0].Quality != model.QualityGood {
		t.Fatalf("first item succeeded but was reported as %+v", res[0])
	}
	if res[1].OK || res[1].Err == nil {
		t.Fatalf("second item failed but was reported as %+v", res[1])
	}
	if res[1].Quality == "" {
		t.Fatal("a failed write must carry a quality, not the empty string")
	}
}

// TestWriteAllFailReturnsFirstError：整批都失败时把第一个错误交出去。
func TestWriteAllFailReturnsFirstError(t *testing.T) {
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
		if res[i].Err == nil || res[i].Quality == "" {
			t.Fatalf("result %d: %+v", i, res[i])
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
