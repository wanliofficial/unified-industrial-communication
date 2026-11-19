package finscore

import (
	"context"
	"testing"

	finscodec "uic/internal/codec/fins"
	"uic/internal/driver"
	"uic/internal/model"
)

// 本文件钉住 finscore 写路径的三条硬约束（与 ENIP/ADS/DNP3 一致）：
//   - 数值越界/带小数/float32 溢出在写报文前拒绝（K）；
//   - STRING/bytes 超长拒绝而不是静默截断（J）；
//   - 写路径遇链路级失败短路，剩余条目显式标坏（F）。
//
// finscore 同时被 FINS/TCP 与 Host Link 复用，这些修复对两个驱动一并生效。

func u16w(id string, off int, v uint16) driver.WriteRequest {
	return driver.WriteRequest{
		PointID: id, Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: off, Bit: -1},
		DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, float64(v)),
	}
}

// ---------------------------------------------------------------------------
// K：数值范围校验
// ---------------------------------------------------------------------------

func TestWriteRejectsOutOfRangeInteger(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1},
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 70000),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("70000 into INT16 accepted: ok=%v writes=% x", res[0].OK, tr.writes)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	if len(tr.writes) != 0 {
		t.Fatalf("range check must run before any write; got %d frames", len(tr.writes))
	}
}

func TestWriteRejectsFractionalInteger(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1},
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1.5),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("1.5 into INT16 accepted: err=%v", err)
	}
}

func TestWriteRejectsFloat32Overflow(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "t", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1},
		DataType: model.TypeFloat32, Value: model.NumValue(model.TypeFloat32, 1e300),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("1e300 into REAL accepted: err=%v", err)
	}
}

func TestWriteAcceptsInRangeInteger(t *testing.T) {
	// 反向：合法值必须照常写下去，别把范围校验写成「拒绝一切」。
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1},
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, -1234),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("in-range write rejected: %+v err=%v", res[0], err)
	}
}

// ---------------------------------------------------------------------------
// J：STRING/bytes 超长拒绝
// ---------------------------------------------------------------------------

func TestWriteRejectsOverlongString(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	// dm0 容量 4 字 = 8 字节，写 20 字符。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "s", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 4, Bit: -1},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, "ABCDEFGHIJKLMNOPQRST"),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("20-char string into 4 words accepted: ok=%v writes=% x", res[0].OK, tr.writes)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	if len(tr.writes) != 0 {
		t.Fatalf("overlong string must be rejected before any write; got %d frames", len(tr.writes))
	}
}

func TestWriteRejectsOverlongBytes(t *testing.T) {
	tr := &fakeTr{}
	c := newConn(tr, DefaultLimits())
	// 4 字 = 8 字节，写 12 字节。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 4, Bit: -1},
		DataType: model.TypeBytes, Value: model.TextValue(model.TypeBytes, "AAAABBBBCCCC"),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("12 bytes into 4 words accepted: ok=%v writes=% x", res[0].OK, tr.writes)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
}

func TestWriteAcceptsExactFitString(t *testing.T) {
	// 反向：正好填满容量的字符串必须照常写（不误判为超长）。
	tr := &fakeTr{}
	c := newConn(tr, Limits{WordsPerRead: 4, WordsPerWrite: 3, BitsPerRead: 64, BitsPerWrite: 64})
	text := "0123456789ABCDEF" // 16 字符 = 8 字
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "s", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 8, Bit: -1},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, text),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("exact-fit string rejected: %+v err=%v", res[0], err)
	}
}

// ---------------------------------------------------------------------------
// F：写路径链路级失败短路
// ---------------------------------------------------------------------------

func TestWriteShortCircuitsOnLinkFailure(t *testing.T) {
	linkErr := driver.E(driver.ErrDisconnected, "write", "d", "connection lost", true, nil)
	tr := &fakeTr{failOn: map[int]error{1: linkErr}} // 第一次写就链路失败
	c := newConn(tr, DefaultLimits())
	before := len(tr.reqs)
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		u16w("a", 0, 1), u16w("b", 1, 2), u16w("c", 2, 3),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := len(tr.reqs) - before; n > 1 {
		t.Fatalf("link failure still sent %d writes (want <=1)", n)
	}
	if len(res) != 3 {
		t.Fatalf("results = %d, want 3 (every entry must be reported)", len(res))
	}
	// 首条失败、后两条被短路标坏，没有一条误报 OK。
	for i, r := range res {
		if r.OK {
			t.Fatalf("result %d (%s) reported OK after link failure", i, r.PointID)
		}
	}
}

// ---------------------------------------------------------------------------
// L：多块读时后面某块失败，前面已解出的好点位必须保留
// ---------------------------------------------------------------------------

func TestReadPartialChunkKeepsEarlierGoodSamples(t *testing.T) {
	// 第二块返回地址类结束码（例如点位越过区尾）。第一块已正常读回。
	tr := &fakeTr{failOn: map[int]error{2: &finscodec.EndCodeError{Code: 0x1103}}}
	c := newConn(tr, Limits{WordsPerRead: 1, WordsPerWrite: 8, BitsPerRead: 64, BitsPerWrite: 64})

	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "good", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1}, DataType: model.TypeUint16},
		{ID: "bad", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: -1}, DataType: model.TypeUint16},
	}}})
	if len(tr.reqs) < 2 {
		t.Fatalf("expected 2 chunk requests, got %d", len(tr.reqs))
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].Quality != model.QualityGood || samples[0].Err != nil {
		t.Fatalf("first chunk's good sample was overwritten with the later error: %+v", samples[0])
	}
	if samples[1].Quality == model.QualityGood || samples[1].Err == nil {
		t.Fatalf("failing chunk's point must be marked bad: %+v", samples[1])
	}
}

func TestReadPartialChunkKeepsGoodOnLinkFailure(t *testing.T) {
	// 同样的场景但第二块是链路级失败：前面已读到的好点位仍须保留，
	// 同时错误照常上抛（链路失败不得被部分失败闸门吞掉）。
	timeout := driver.E(driver.ErrTimeout, "read", "d", "no response", true, context.DeadlineExceeded)
	tr := &fakeTr{failOn: map[int]error{2: timeout}}
	c := newConn(tr, Limits{WordsPerRead: 1, WordsPerWrite: 8, BitsPerRead: 64, BitsPerWrite: 64})

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "good", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: -1}, DataType: model.TypeUint16},
		{ID: "bad", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: -1}, DataType: model.TypeUint16},
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("link failure must be reported: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].Quality != model.QualityGood {
		t.Fatalf("good sample lost on link failure: %+v", samples[0])
	}
	if samples[1].Quality != model.QualityTimeout {
		t.Fatalf("pending point quality = %q, want timeout", samples[1].Quality)
	}
}

func TestReadPartialBitChunkKeepsGood(t *testing.T) {
	// 位读的分块路径同样要保留已得样本（与字读是两段独立代码）。
	tr := &fakeTr{failOn: map[int]error{2: &finscodec.EndCodeError{Code: 0x1101}}}
	c := newConn(tr, Limits{WordsPerRead: 8, WordsPerWrite: 8, BitsPerRead: 16, BitsPerWrite: 16})

	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "b0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: 0}, DataType: model.TypeBool},
		{ID: "b1", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: 0}, DataType: model.TypeBool},
	}}})
	if len(tr.reqs) < 2 {
		t.Fatalf("expected 2 bit chunk requests, got %d", len(tr.reqs))
	}
	if samples[0].Quality != model.QualityGood {
		t.Fatalf("first bit chunk's good sample lost: %+v", samples[0])
	}
	if samples[1].Quality == model.QualityGood {
		t.Fatalf("failing bit chunk marked good: %+v", samples[1])
	}
}

func TestWriteLinkFailureKeepsEarlierSuccess(t *testing.T) {
	// 链路失败发生在第二条：第一条已成功（已落到从站），后两条短路标坏。
	// 成功的条目不得被回填错误（§3.4 规则 4）。
	linkErr := driver.E(driver.ErrDisconnected, "write", "d", "connection lost", true, nil)
	tr := &fakeTr{failOn: map[int]error{2: linkErr}}
	c := newConn(tr, DefaultLimits())
	res, _ := c.Write(context.Background(), []driver.WriteRequest{
		u16w("ok", 0, 1), u16w("bad", 1, 2), u16w("bad2", 2, 3),
	})
	if len(res) != 3 {
		t.Fatalf("results = %d, want 3", len(res))
	}
	if !res[0].OK || res[0].Err != nil {
		t.Fatalf("first (successful) write was altered: %+v", res[0])
	}
	if res[1].OK || res[2].OK {
		t.Fatalf("short-circuited writes reported OK: %+v %+v", res[1], res[2])
	}
}
