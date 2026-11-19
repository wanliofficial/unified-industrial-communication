package mc

import (
	"context"
	"testing"

	mccodec "uic/internal/codec/mc"
	"uic/internal/driver"
	"uic/internal/model"
)

// 本文件钉住 MC 3E 双轴复核的 8 条约束。每条都做过变异验证：把修复去掉
// 后对应用例必须变红。

func u16Pt(id, area string, off int) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: off, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
	}
}

func writeU16(id, area string, off int) driver.WriteRequest {
	return driver.WriteRequest{
		PointID:  id,
		Address:  model.Address{Kind: model.AddressRegister, Area: area, Offset: off, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
		Value:    model.NumValue(model.TypeUint16, 1),
	}
}

// ---------------------------------------------------------------------------
// A：结束码必须带 Kind，否则地址错会被当成 ErrInternal 计进熔断
// ---------------------------------------------------------------------------

func TestEndCodeCarriesKind(t *testing.T) {
	cases := []struct {
		code uint16
		kind driver.ErrorKind
	}{
		{0xC052, driver.ErrAddress},     // head device invalid
		{0xC053, driver.ErrAddress},     // points invalid
		{0xC054, driver.ErrAddress},     // head + points out of range
		{0xC056, driver.ErrAddress},     // device not supported
		{0xC058, driver.ErrAddress},     // data length mismatch
		{0xC050, driver.ErrUnsupported}, // command not supported
		{0xC051, driver.ErrUnsupported}, // subcommand not supported
		{0xC06F, driver.ErrPermission},  // write protected
		{0xC060, driver.ErrProtocol},    // network number invalid
		{0xC05F, driver.ErrProtocol},    // cannot be processed
	}
	for _, tc := range cases {
		e := &mccodec.EndCodeError{Code: tc.code}
		if got := driver.KindOf(e); got != tc.kind {
			t.Fatalf("end code %#04x kind = %v, want %v", tc.code, got, tc.kind)
		}
		if driver.CountsTowardBreaker(e) != (tc.kind == driver.ErrProtocol) {
			t.Fatalf("end code %#04x breaker = %v (kind %v)", tc.code, driver.CountsTowardBreaker(e), tc.kind)
		}
		if driver.IsRetryable(e) {
			t.Fatalf("end code %#04x must not be retryable: device answered deterministically", tc.code)
		}
	}
}

// A（端到端）：一个点位名写错的地址错不能计入熔断，否则整台 PLC 被熔断。
func TestEndCodeAddressErrorDoesNotTripBreaker(t *testing.T) {
	stream := &memStream{}
	stream.frames = append(stream.frames, mccodec.BuildResponse(0xC052, nil))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{u16Pt("d0", "d", 0)}}})
	if err == nil {
		t.Fatal("want error for end code")
	}
	if driver.CountsTowardBreaker(err) {
		t.Fatalf("0xC052 counts toward breaker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// H：副头部不匹配时必须排空报文体，否则整条流错位
// ---------------------------------------------------------------------------

func TestStaleFrameDoesNotDesyncStream(t *testing.T) {
	bad := mccodec.BuildResponse(0, make([]byte, 8))
	bad[0], bad[1] = 0x34, 0x12 // 副头部错（带 8 字节体）
	good := mccodec.BuildResponse(0, []byte{0x34, 0x12})
	stream := &memStream{}
	stream.frames = append(stream.frames, append(append([]byte(nil), bad...), good...))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{u16Pt("d0", "d", 0)}}}); err == nil {
		t.Fatal("expected protocol error on stale frame")
	}
	// 第二次读必须命中正确应答；若没排空残留体，这里会把体当头部读到错位。
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{u16Pt("d0", "d", 0)}}})
	if err != nil {
		t.Fatalf("stream not realigned: %v", err)
	}
	if v, _ := samples[0].Value.AsFloat(); v != 0x1234 {
		t.Fatalf("value = %v, want 0x1234", samples[0].Value)
	}
}

// ---------------------------------------------------------------------------
// B：读路径链路失败必须短路，剩余 span 显式标坏
// ---------------------------------------------------------------------------

func TestReadShortCircuitsOnLinkFailure(t *testing.T) {
	stream := &memStream{} // 无应答帧 → eof → 链路失败
	c := openMem(t, stream)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{
		{ID: "a", Points: []driver.PointRef{u16Pt("a", "d", 0)}},
		{ID: "b", Points: []driver.PointRef{u16Pt("b", "d", 1)}},
		{ID: "c", Points: []driver.PointRef{u16Pt("c", "d", 2)}},
	})
	if err == nil {
		t.Fatal("a link failure must be reported")
	}
	if len(stream.requests) != 1 {
		t.Fatalf("frames = %d, want 1 (remaining spans must not each wait out a timeout)", len(stream.requests))
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3 (remaining points must be marked, not dropped)", len(samples))
	}
	for _, s := range samples {
		if s.Quality == model.QualityGood || s.Err == nil {
			t.Fatalf("point %q = %+v, want marked bad", s.PointID, s)
		}
	}
}

// ---------------------------------------------------------------------------
// C：跨块读第二块失败，第一块已解出的好点位必须保留
// ---------------------------------------------------------------------------

func TestReadPartialChunkKeepsEarlierGoodSamples(t *testing.T) {
	stream := &memStream{}
	// 第一块（0..960 字）正常应答，第二块没有应答帧 → 链路失败。
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, make([]byte, 960*2)))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		u16Pt("good", "d", 0),
		u16Pt("bad", "d", 1000), // 落在第二块
	}}})
	if err == nil {
		t.Fatal("link failure in the second chunk must be reported")
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].Quality != model.QualityGood || samples[0].Err != nil {
		t.Fatalf("first chunk's good sample was overwritten with the later error: %+v", samples[0])
	}
	if samples[1].Quality == model.QualityGood {
		t.Fatalf("unread point marked good: %+v", samples[1])
	}
}

// ---------------------------------------------------------------------------
// G：所有点位都解码失败时 Read 必须上抛错误（§3.3.1 禁止静默）
// ---------------------------------------------------------------------------

func TestAllPointsFailedToDecodeReturnsError(t *testing.T) {
	stream := &memStream{}
	// 位软元件应答（1 字节），但点位配成数值类型 → 解码必然失败。
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, mccodec.PackBits([]bool{true})))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
		ID:       "m0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "m", Offset: 0, Bit: -1},
		DataType: model.TypeUint16,
	}}}})
	if err == nil {
		t.Fatal("every point failed to decode but Read returned nil")
	}
	if len(samples) != 1 || samples[0].Err == nil {
		t.Fatalf("samples = %+v, want one bad sample", samples)
	}
}

// ---------------------------------------------------------------------------
// D：写路径链路失败短路，剩余条目显式标坏
// ---------------------------------------------------------------------------

func TestWriteShortCircuitsOnLinkFailure(t *testing.T) {
	stream := &memStream{} // 无应答帧
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{
		writeU16("a", "d", 0), writeU16("b", "d", 1), writeU16("c", "d", 2),
	})
	if err == nil {
		t.Fatal("link failure must be reported")
	}
	if len(stream.requests) != 1 {
		t.Fatalf("frames = %d, want 1 (remaining entries must not each wait out a timeout)", len(stream.requests))
	}
	if len(res) != 3 {
		t.Fatalf("results = %d, want 3 (every entry must be reported)", len(res))
	}
	for _, r := range res {
		if r.OK {
			t.Fatalf("write %s reported OK after link failure", r.PointID)
		}
	}
}

func TestWriteLinkFailureKeepsEarlierSuccess(t *testing.T) {
	stream := &memStream{}
	// 第一条写成功；第二条没有应答帧 → 链路失败；第三条必须被短路。
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, nil))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	res, _ := c.Write(context.Background(), []driver.WriteRequest{
		writeU16("ok", "d", 0), writeU16("bad", "d", 1), writeU16("bad2", "d", 2),
	})
	if len(res) != 3 {
		t.Fatalf("results = %d, want 3", len(res))
	}
	if !res[0].OK || res[0].Err != nil {
		t.Fatalf("successful write was altered: %+v", res[0])
	}
	if res[1].OK || res[2].OK {
		t.Fatalf("short-circuited writes reported OK: %+v %+v", res[1], res[2])
	}
}

// ---------------------------------------------------------------------------
// E/F：写路径数值范围校验与超长拒绝（发报文之前）
// ---------------------------------------------------------------------------

func TestWriteRejectsOutOfRangeInteger(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeInt16,
		Value:    model.NumValue(model.TypeInt16, 70000),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("70000 into int16 accepted: ok=%v err=%v", res[0].OK, err)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	if len(stream.requests) != 0 {
		t.Fatalf("range check must run before sending; got %d frames", len(stream.requests))
	}
}

func TestWriteRejectsFractionalInteger(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeInt16,
		Value:    model.NumValue(model.TypeInt16, 1.5),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("1.5 into int16 accepted: err=%v", err)
	}
}

func TestWriteRejectsFloat32Overflow(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "t",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 2, Bit: -1},
		DataType: model.TypeFloat32,
		Value:    model.NumValue(model.TypeFloat32, 1e300),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("1e300 into float32 accepted: err=%v", err)
	}
}

func TestWriteAcceptsInRangeInteger(t *testing.T) {
	// 反向：合法值必须照常写下去，别把范围校验写成「拒绝一切」。
	stream := &memStream{}
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, nil))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "x",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeInt16,
		Value:    model.NumValue(model.TypeInt16, -1234),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("in-range write rejected: %+v err=%v", res[0], err)
	}
}

func TestWriteRejectsOverlongString(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "s",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 4, Bit: -1},
		DataType: model.TypeString,
		Value:    model.TextValue(model.TypeString, "ABCDEFGHIJKLMNOPQRST"), // 20 字节 > 8
	}})
	if err == nil || res[0].OK {
		t.Fatalf("20-byte string into 4 words accepted: ok=%v err=%v", res[0].OK, err)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	if len(stream.requests) != 0 {
		t.Fatalf("overlong string must be rejected before sending; got %d frames", len(stream.requests))
	}
}

func TestWriteRejectsOverlongBytes(t *testing.T) {
	stream := &memStream{}
	c := openMem(t, stream)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "b",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 2, Bit: -1},
		DataType: model.TypeBytes,
		Value:    model.TextValue(model.TypeBytes, "ABCDEF"), // 6 字节 > 4
	}})
	if err == nil || res[0].OK {
		t.Fatalf("6 bytes into 2 words accepted: ok=%v err=%v", res[0].OK, err)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
}

func TestWriteAcceptsExactFitString(t *testing.T) {
	// 反向：正好填满容量的字符串必须照常写（补零语义不变）。
	stream := &memStream{}
	stream.frames = append(stream.frames, mccodec.BuildResponse(0, nil))
	stream.pos = append(stream.pos, 0)
	c := openMem(t, stream)

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "s",
		Address:  model.Address{Kind: model.AddressRegister, Area: "d", Offset: 0, Length: 4, Bit: -1},
		DataType: model.TypeString,
		Value:    model.TextValue(model.TypeString, "ABCDEFGH"), // 正好 8 字节
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("exact-fit string rejected: %+v err=%v", res[0], err)
	}
}
