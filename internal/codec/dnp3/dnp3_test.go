package dnp3

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"
)

// CRC 的正确性只有一个可信锚点：标准校验值。自己和自己对齐的测试
// （编码 → 解码 → 相等）在多项式写错时依然会通过。
func TestCRC(t *testing.T) {
	if got := CRC([]byte("123456789")); got != 0xEA82 {
		t.Fatalf("CRC(123456789) = %04X, want EA82", got)
	}
	if got := CRC(nil); got != 0xFFFF {
		t.Fatalf("CRC(nil) = %04X, want FFFF", got)
	}
	if CRC([]byte{0x00}) == CRC([]byte{0x01}) {
		t.Fatal("CRC ignores data")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	// 复位链路帧是现场抓包见得最多的一条：0x0564 + LEN=5 + 0xC0。
	raw, err := EncodeFrame(ControlDir|ControlPrm|FuncResetLink, 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x05, 0x64, 0x05, 0xC0, 0x01, 0x00, 0x00, 0x04, 0x28, 0x4D}
	if !bytes.Equal(raw, want) {
		t.Fatalf("reset frame = % X, want % X", raw, want)
	}

	data := bytes.Repeat([]byte{0xA5}, MaxUserData)
	raw, err = EncodeFrame(ControlDir|ControlPrm|FuncConfirmedUserData, 10, 20, data)
	if err != nil {
		t.Fatal(err)
	}
	var d Decoder
	d.Feed(raw)
	f, ok, err := d.Next()
	if err != nil || !ok {
		t.Fatalf("Next = %v %v", ok, err)
	}
	if f.Dest != 10 || f.Source != 20 || f.Function() != FuncConfirmedUserData || !f.Primary() {
		t.Fatalf("frame = %+v", f)
	}
	if !bytes.Equal(f.Data, data) {
		t.Fatalf("data mismatch: %d vs %d bytes", len(f.Data), len(data))
	}
	if _, ok, _ := d.Next(); ok {
		t.Fatal("decoder produced a second frame")
	}
}

// TCP 会把帧切在任何位置，包括 0x05 与 0x64 之间。逐字节喂进去必须
// 只在完整时返回。
func TestDecoderFeedsInPieces(t *testing.T) {
	raw, err := EncodeFrame(ControlDir|ControlPrm|FuncConfirmedUserData, 1, 2, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	var d Decoder
	for i := 0; i < len(raw)-1; i++ {
		d.Feed(raw[i : i+1])
		if _, ok, err := d.Next(); ok || err != nil {
			t.Fatalf("frame completed at byte %d: ok=%v err=%v", i, ok, err)
		}
	}
	d.Feed(raw[len(raw)-1:])
	f, ok, err := d.Next()
	if err != nil || !ok {
		t.Fatalf("final = %v %v", ok, err)
	}
	if string(f.Data) != "hello world" {
		t.Fatalf("data = %q", f.Data)
	}
}

func TestDecoderRejectsBadCRC(t *testing.T) {
	raw, err := EncodeFrame(ControlDir|ControlPrm|FuncConfirmedUserData, 1, 2, []byte("abcdefghijklmnopq"))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-3] ^= 0xFF // 破坏最后一块的数据字节
	var d Decoder
	d.Feed(raw)
	if _, ok, err := d.Next(); ok || err == nil {
		t.Fatalf("bad CRC accepted: ok=%v err=%v", ok, err)
	}
}

func TestDecoderSkipsGarbage(t *testing.T) {
	raw, err := EncodeFrame(ControlDir|ControlPrm|FuncConfirmedUserData, 1, 2, []byte("ok"))
	if err != nil {
		t.Fatal(err)
	}
	var d Decoder
	d.Feed([]byte{0x00, 0x11, 0x22})
	d.Feed(raw)
	f, ok, err := d.Next()
	if err != nil || !ok || string(f.Data) != "ok" {
		t.Fatalf("resync failed: %+v %v %v", f, ok, err)
	}
}

func TestTransportRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5A}, MaxAppPerFrame*2+17)
	chunks := EncodeTransport(payload, 9)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	var r Reassembler
	var got []byte
	for i, c := range chunks {
		out, done, err := r.Push(c[0]&TransportFIR != 0, c[0]&TransportFIN != 0, c[0]&TransportSeq, c[1:])
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if done {
			got = out
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(payload))
	}
}

func TestReassemblerSequenceGap(t *testing.T) {
	var r Reassembler
	if _, _, err := r.Push(true, false, 5, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Push(false, true, 7, []byte("b")); err == nil {
		t.Fatal("sequence gap accepted")
	}
	// 出错后必须回到「没有在重组」，否则下一条单帧消息会被拼到旧数据上。
	out, done, err := r.Push(true, true, 0, []byte("fresh"))
	if err != nil || !done || string(out) != "fresh" {
		t.Fatalf("after gap: %q %v %v", out, done, err)
	}
}

func TestAppRoundTrip(t *testing.T) {
	ctrl := AppControl{FIR: true, FIN: true, Seq: 7}
	req := EncodeRequest(ctrl, AppRead, []byte{GroupClass, 1, QualAllObjects})
	if len(req) != 5 || req[0] != 0xC7 || req[1] != AppRead {
		t.Fatalf("request = % X", req)
	}
	m, err := DecodeApp(req)
	if err != nil {
		t.Fatal(err)
	}
	if m.Function != AppRead || m.Control.Seq != 7 || !m.Control.FIR || m.IIN != 0 {
		t.Fatalf("decoded request = %+v", m)
	}

	ctrl.CON = true
	resp := EncodeResponse(ctrl, AppResponse, IINClass1Events|IINDeviceRestart, []byte{1, 2, 3})
	m, err = DecodeApp(resp)
	if err != nil {
		t.Fatal(err)
	}
	if m.IIN != IINClass1Events|IINDeviceRestart || !m.Control.CON || !bytes.Equal(m.Objects, []byte{1, 2, 3}) {
		t.Fatalf("decoded response = %+v", m)
	}
}

func TestParseStaticBits(t *testing.T) {
	// 组 1 变体 2：每个点一个「状态 + 品质」字节，状态在 bit7。
	raw := []byte{
		GroupBinaryInput, 2, QualStartStop8, 0, 2,
		0x81, 0x01, 0x80,
	}
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("objects = %d", len(objs))
	}
	want := []bool{true, false, true}
	for i, o := range objs {
		if o.Index != uint32(i) || o.Value != want[i] || !o.HasFlags {
			t.Fatalf("obj %d = %+v", i, o)
		}
	}

	// 变体 1（无品质位）只有位图：0b010 = 点 1 为 on。
	objs, err = ParseObjects([]byte{GroupBinaryInput, 1, QualStartStop8, 0, 2, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	got := []bool{objs[0].Value.(bool), objs[1].Value.(bool), objs[2].Value.(bool)}
	if got[0] || !got[1] || got[2] {
		t.Fatalf("packed bits = %v", got)
	}
}

// 变体 2 只有 flags，没有额外的位图字节。多读一个字节会把后面所有对象
// 整体错位——这个用例用「flags 之后紧跟另一个对象头」把错误钉死。
func TestStaticVar2HasNoPackedField(t *testing.T) {
	raw := []byte{
		GroupBinaryInput, 2, QualStartStop8, 0, 2,
		0x81, 0x01, 0x81,
		GroupAnalogInput, 5, QualStartStop8, 0, 0,
		0x01, 0x00, 0x00, 0x00, 0x00, // 0.0
	}
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 4 {
		t.Fatalf("objects = %d, want 4", len(objs))
	}
	if objs[3].Group != GroupAnalogInput || objs[3].Value != 0.0 {
		t.Fatalf("analog object = %+v", objs[3])
	}
}

func TestParseDoubleBit(t *testing.T) {
	// 组 3 变体 1：2 位一组，0b11100100 -> 点0=0, 点1=1, 点2=2, 点3=3。
	objs, err := ParseObjects([]byte{GroupDoubleBitInput, 1, QualStartStop8, 0, 3, 0xE4})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{0, 1, 2, 3}
	for i, o := range objs {
		if o.Value != want[i] {
			t.Fatalf("obj %d = %v, want %d", i, o.Value, want[i])
		}
	}
}

// 双点量跨字节时最容易写错：5 个点占 2 字节，不是 1 字节。这个用例用
// 相邻的字节值把「按 1 位/点」的实现钉死。
func TestParseDoubleBitAcrossBytes(t *testing.T) {
	// 8 个点：0,1,2,3,0,1,2,3 -> 每字节 0xE4。
	objs, err := ParseObjects([]byte{GroupDoubleBitInput, 1, QualStartStop8, 0, 7, 0xE4, 0xE4})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{0, 1, 2, 3, 0, 1, 2, 3}
	for i, o := range objs {
		if o.Value != want[i] {
			t.Fatalf("obj %d = %v, want %d", i, o.Value, want[i])
		}
	}

	// 变体 2：一个点一个 flags 字节，状态在 bit6-7，没有独立位图。
	raw := []byte{GroupDoubleBitInput, 2, QualStartStop8, 0, 3,
		0x41, 0x82, 0xC3, 0x01,
	}
	objs, err = ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	want = []uint64{1, 2, 3, 0}
	for i, o := range objs {
		if o.Value != want[i] {
			t.Fatalf("var2 obj %d = %v, want %d", i, o.Value, want[i])
		}
	}
}

func TestParseAnalogWithFlags(t *testing.T) {
	var body []byte
	for _, v := range []float32{1.5, -2.5} {
		body = append(body, FlagOnline)
		body = binary.LittleEndian.AppendUint32(body, math.Float32bits(v))
	}
	raw := append([]byte{GroupAnalogInput, 5, QualStartStop8, 0, 1}, body...)
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("objects = %d", len(objs))
	}
	if objs[0].Value != 1.5 || objs[1].Value != -2.5 {
		t.Fatalf("values = %v %v", objs[0].Value, objs[1].Value)
	}
	if !objs[0].HasFlags || objs[0].Flags != FlagOnline {
		t.Fatalf("flags = %+v", objs[0])
	}
}

func TestParseCounterWithIndexPrefix(t *testing.T) {
	raw := []byte{
		GroupCounter, 1, QualIndex8, 1, // 1 个对象
		5, // 索引前缀
		FlagOnline,
	}
	raw = binary.LittleEndian.AppendUint32(raw, 42)
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Index != 5 || objs[0].Value != uint64(42) {
		t.Fatalf("object = %+v", objs)
	}
}

func TestParseEventWithTime(t *testing.T) {
	ts := time.UnixMilli(1700000000123).UTC()
	raw := []byte{GroupBinaryEvent, 2, QualIndex8, 1, 3, 0x81}
	raw = append(raw, EncodeTime48(ts)...)
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects = %d", len(objs))
	}
	o := objs[0]
	if o.Index != 3 || o.Value != true || !o.HasTime || !o.When.Equal(ts) {
		t.Fatalf("event = %+v", o)
	}
}

// TestParseCounterEventTimed 钉住带时标的计数器事件（22v5/v6）的对象布局。
//
// 这两个变体是「值 + 品质 + 6 字节绝对时标」，合计 11/9 字节。此前实现误用了
// 组 20「无品质」的 4/2 字节布局：从站回一批 22v5/22v6 事件时，解析按 4/2 字节
// 切对象，第一个对象就把品质字节与时标当成下一个计数器的值，整批对象全部读歪。
// 这里用两个对象拼一个缓冲区，既验证单对象的值/品质/时标，又验证不会错位。
func TestParseCounterEventTimed(t *testing.T) {
	t1 := time.UnixMilli(1700000000123).UTC()
	t2 := time.UnixMilli(1700000000456).UTC()

	// 22v5：32 位计数器 + 品质 + 时标（11 字节/对象）。
	var raw []byte
	raw = append(raw, GroupCounterEvent, 5, QualIndex8, 2)
	raw = append(raw, 5, FlagOnline)
	raw = binary.LittleEndian.AppendUint32(raw, 42)
	raw = append(raw, EncodeTime48(t1)...)
	raw = append(raw, 6, FlagOnline)
	raw = binary.LittleEndian.AppendUint32(raw, 100)
	raw = append(raw, EncodeTime48(t2)...)

	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("objects = %d, want 2 (mis-sized layout would misalign and mis-parse)", len(objs))
	}
	if objs[0].Index != 5 || objs[0].Value != uint64(42) || !objs[0].HasFlags ||
		objs[0].Flags != FlagOnline || !objs[0].HasTime || !objs[0].When.Equal(t1) {
		t.Fatalf("object 0 = %+v", objs[0])
	}
	if objs[1].Index != 6 || objs[1].Value != uint64(100) || !objs[1].HasTime ||
		!objs[1].When.Equal(t2) {
		t.Fatalf("object 1 = %+v (second object misaligned)", objs[1])
	}

	// 22v6：16 位计数器 + 品质 + 时标（9 字节/对象）。
	var raw16 []byte
	raw16 = append(raw16, GroupCounterEvent, 6, QualIndex8, 1)
	raw16 = append(raw16, 7, FlagOnline)
	raw16 = binary.LittleEndian.AppendUint16(raw16, 7)
	raw16 = append(raw16, EncodeTime48(t1)...)

	objs, err = ParseObjects(raw16)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Index != 7 || objs[0].Value != uint64(7) ||
		!objs[0].HasFlags || !objs[0].HasTime || !objs[0].When.Equal(t1) {
		t.Fatalf("16-bit timed counter = %+v", objs)
	}
}

func TestParseRequestHeaders(t *testing.T) {
	raw, err := ClassObjects(0, 1, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := ParseRequestHeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 4 {
		t.Fatalf("headers = %d", len(hs))
	}
	for i, h := range hs {
		if h.Group != GroupClass || h.Variation != byte(i+1) || !h.All {
			t.Fatalf("header %d = %+v", i, h)
		}
	}

	// 带范围的头：0..7。
	hs, err = ParseRequestHeaders([]byte{GroupAnalogInput, 1, QualStartStop8, 0, 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].Start != 0 || hs[0].Stop != 7 || hs[0].Count != 8 {
		t.Fatalf("range header = %+v", hs[0])
	}
}

func TestControlObjectsRoundTrip(t *testing.T) {
	raw, err := CROBObjects(300, CROBLatchOn, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Index != 300 {
		t.Fatalf("crob = %+v", objs)
	}
	c, ok := objs[0].Value.(CROB)
	if !ok || c.Code != CROBLatchOn || c.Count != 1 || !c.Success() {
		t.Fatalf("crob value = %#v", objs[0].Value)
	}

	raw, err = AnalogOutputObjects(7, 3, 12.5)
	if err != nil {
		t.Fatal(err)
	}
	objs, err = ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Value != 12.5 {
		t.Fatalf("analog output = %+v", objs)
	}
}

// 不认识的组不能让整条响应作废：已解析的对象要保留。
func TestParseUnsupportedGroupKeepsPartial(t *testing.T) {
	raw := []byte{
		GroupBinaryInput, 2, QualStartStop8, 0, 0, 0x81,
		GroupTime, 1, QualStartStop8, 0, 0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xEE, 0x01, QualAllObjects,
	}
	objs, err := ParseObjects(raw)
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want UnsupportedError", err)
	}
	if len(objs) != 1 || objs[0].Value != true {
		t.Fatalf("objects before error = %+v", objs)
	}
	if ue.Group != 0xEE {
		t.Fatalf("unsupported group = %d", ue.Group)
	}
}

func TestParseTruncated(t *testing.T) {
	cases := [][]byte{
		{GroupBinaryInput, 2, QualStartStop8},          // 范围缺失
		{GroupBinaryInput, 2, QualStartStop8, 0, 2},    // 品质位缺失
		{GroupAnalogInput, 1, QualStartStop8, 0, 0, 1}, // 值缺失
		{GroupBinaryInput, 2, 0x99, 0, 0},              // 未知限定词
	}
	for _, c := range cases {
		if _, err := ParseObjects(c); err == nil {
			t.Fatalf("% X accepted", c)
		}
	}
}

// 索引 >0xFFFF 必须用 QualIndex32 前缀：点表允许 24 位索引，早先把
// uint16(index) 静默截断——index=0x12345 的控制命令会下发到 0x2345 上。
// 读路径本就解析 32 位前缀，写路径对齐。
func TestControlObjects32BitIndex(t *testing.T) {
	const big = uint32(0x12345)

	raw, err := CROBObjects(big, CROBLatchOn, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if raw[2] != QualIndex32 {
		t.Fatalf("crob qualifier = 0x%02X, want 0x%02X", raw[2], QualIndex32)
	}
	objs, err := ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Index != big {
		t.Fatalf("crob = %+v", objs)
	}
	c, ok := objs[0].Value.(CROB)
	if !ok || c.Code != CROBLatchOn || c.Count != 1 || !c.Success() {
		t.Fatalf("crob value = %#v", objs[0].Value)
	}

	raw, err = AnalogOutputObjects(big, 3, 12.5)
	if err != nil {
		t.Fatal(err)
	}
	if raw[2] != QualIndex32 {
		t.Fatalf("analog qualifier = 0x%02X, want 0x%02X", raw[2], QualIndex32)
	}
	objs, err = ParseObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Index != big || objs[0].Value != 12.5 {
		t.Fatalf("analog output = %+v", objs)
	}

	// 8/16 位分支不回归。
	if raw, err = CROBObjects(300, CROBLatchOn, 1, 0, 0); err != nil || raw[2] != QualIndex16 {
		t.Fatalf("16-bit qualifier = 0x%02X err %v", raw[2], err)
	}
	if raw, err = CROBObjects(7, CROBLatchOn, 1, 0, 0); err != nil || raw[2] != QualIndex8 {
		t.Fatalf("8-bit qualifier = 0x%02X err %v", raw[2], err)
	}
}
