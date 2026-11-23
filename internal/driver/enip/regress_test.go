package enip

import (
	"context"
	"strings"
	"testing"

	enipcodec "uic/internal/codec/enip"
	"uic/internal/driver"
	"uic/internal/model"
)

// cat 把多个封装报文拼进同一段字节：模拟真实 TCP（包边界不可见）。
func cat(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func samplesByID(samples []driver.RawSample) map[string]driver.RawSample {
	m := make(map[string]driver.RawSample, len(samples))
	for _, s := range samples {
		m[s.PointID] = s
	}
	return m
}

func dintReply(v int32) []byte {
	// Read Tag 应答数据：[类型, 0, v0..v3]（小端），没有元素数字段。
	b := []byte{enipcodec.TypeDINT, 0, byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
	return rrReply(0x11, enipcodec.ServiceReadTag, 0, nil, b)
}

// ---------------------------------------------------------------------------
// 候选 A：命令/上下文不匹配时必须排空滞留的报文体，否则整条流错位
// ---------------------------------------------------------------------------

func TestStaleFrameDoesNotDesyncStream(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	// 一条命令错的滞留报文（带 8 字节体）紧跟着一条正确应答，拼在同一帧里。
	bad := packet(enipcodec.CmdNOP, 0x11, 0, [8]byte{}, make([]byte, 8))
	s.add(cat(bad, dintReply(1234)))
	c, _ := openScript(t, s)

	// 第一次读撞上滞留帧：协议错，但驱动必须把那 8 字节体排空。
	if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("a", "DINT_TAG", model.TypeInt32),
	}}}); err == nil {
		t.Fatal("expected protocol error on stale frame")
	}
	// 第二次读必须命中正确应答——若没排空残留体，这里会把残留体当头部读到错位。
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("b", "DINT_TAG", model.TypeInt32),
	}}})
	if err != nil {
		t.Fatalf("stream not realigned: %v", err)
	}
	if got, _ := samples[0].Value.AsFloat(); got != 1234 {
		t.Fatalf("value = %v, want 1234", samples[0].Value)
	}
}

// ---------------------------------------------------------------------------
// 候选 B：readBatch 已拿到前面点位的好应答，又遇到后续错误时不能整段丢弃
// ---------------------------------------------------------------------------

func TestPartialBatchKeepsEarlierGoodSamples(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	c.(*conn).msp = false // 逐条请求：第一条成功、第二条脚本耗尽（链路级错误）
	s.add(dintReply(1234))

	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("good", "DINT_TAG", model.TypeInt32),
		ref("bad", "DINT_TAG", model.TypeInt32),
	}}})
	m := samplesByID(samples)
	if g := m["good"]; g.Err != nil || g.Quality != model.QualityGood {
		t.Fatalf("good sample lost on later failure: %+v", g)
	}
	if b := m["bad"]; b.Err == nil {
		t.Fatalf("bad sample should be marked bad: %+v", b)
	}
}

// ---------------------------------------------------------------------------
// 候选 C：单个点位构造请求失败（标签/类型配错）不连累同 span 的其它好点位
// ---------------------------------------------------------------------------

func TestBuildErrorDoesNotPoisonSpan(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	c.(*conn).msp = false
	s.add(dintReply(1234))

	bad := ref("bad", "DINT_TAG", model.TypeBytes) // bytes 缺 address.length → 构造失败
	good := ref("good", "DINT_TAG", model.TypeInt32)
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{bad, good}}})
	m := samplesByID(samples)
	if g := m["good"]; g.Err != nil {
		t.Fatalf("good poisoned by bad build error: %+v", g)
	}
	if b := m["bad"]; b.Err == nil {
		t.Fatalf("bad should be marked bad: %+v", b)
	}
}

// ---------------------------------------------------------------------------
// 候选 D：数值越界/带小数必须在发报文前拒绝（70000 不能静默变 4464）
// ---------------------------------------------------------------------------

func TestWriteRejectsOutOfRangeInteger(t *testing.T) {
	c := openSim(t, newSim(t))
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "INT_TAG", Bit: -1},
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 70000),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("70000 into INT16 accepted: ok=%v err=%v", res[0].OK, err)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	// 没发报文，设备上的值应保持原样。
	if s := readOne(t, c, ref("chk", "INT_TAG", model.TypeInt16)); func() float64 { v, _ := s.Value.AsFloat(); return v }() != 5678 {
		t.Fatalf("device value changed: %v", s.Value)
	}
}

func TestWriteRejectsFractionalInteger(t *testing.T) {
	c := openSim(t, newSim(t))
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "INT_TAG", Bit: -1},
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1.5),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("1.5 into INT16 accepted: err=%v", err)
	}
}

func TestWriteRejectsFloat32Overflow(t *testing.T) {
	c := openSim(t, newSim(t))
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "t", Address: model.Address{Kind: model.AddressRegister, Area: "REAL_TAG", Bit: -1},
		DataType: model.TypeFloat32, Value: model.NumValue(model.TypeFloat32, 1e300),
	}})
	if err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("1e300 into REAL accepted: err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// 候选 E：字符串/字节超长必须拒绝而不是静默截断
// ---------------------------------------------------------------------------

func TestWriteRejectsOverlongString(t *testing.T) {
	c := openSim(t, newSim(t))
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "name", Address: model.Address{Kind: model.AddressRegister, Area: "STRING_TAG", Bit: -1, Length: 16},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, strings.Repeat("A", 100)),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("100-char string into capacity 16 accepted: ok=%v", res[0].OK)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
}

func TestWriteRejectsOverlongBytes(t *testing.T) {
	c := openSim(t, newSim(t))
	// DINT_ARRAY 默认 4 个 DINT = 16 字节。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "arr", Address: model.Address{Kind: model.AddressRegister, Area: "DINT_ARRAY", Bit: -1, Length: 16},
		DataType: model.TypeBytes, Value: model.TextValue(model.TypeBytes, strings.Repeat("\x00", 20)),
	}})
	if err == nil || res[0].OK {
		t.Fatalf("20 bytes into 16-byte tag accepted: ok=%v", res[0].OK)
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 候选 F：写路径遇链路级失败要短路，剩余条目显式标坏而不是每条都等满超时
// ---------------------------------------------------------------------------

func TestWriteShortCircuitsOnLinkFailure(t *testing.T) {
	s := newScript().add(registerReply(0x11, 0, 1))
	c, _ := openScript(t, s)
	// 脚本已耗尽：第一条写会在读应答时失败并被判为链路级（断连）。
	before := s.writes
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "A", Bit: -1}, DataType: model.TypeInt32, Value: model.NumValue(model.TypeInt32, 1)},
		{PointID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "B", Bit: -1}, DataType: model.TypeInt32, Value: model.NumValue(model.TypeInt32, 2)},
		{PointID: "c", Address: model.Address{Kind: model.AddressRegister, Area: "C", Bit: -1}, DataType: model.TypeInt32, Value: model.NumValue(model.TypeInt32, 3)},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := s.writes - before; n > 1 {
		t.Fatalf("link failure still sent %d writes (want <= 1)", n)
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

// ---------------------------------------------------------------------------
// 候选 G：partial transfer / 过长应答等点级错误不能计入熔断
// ---------------------------------------------------------------------------

func TestPartialTransferContinuesViaFragments(t *testing.T) {
	// 0x06（partial transfer）不再是终态点级错误：它表示「这一片成功、还有后续」，
	// 驱动应改走 Read Tag Fragmented 把剩下的数据读完。这里让分片续读命中一个
	// 不存在的标签，验证：点被标坏、整轮仍返回样本、链路不被熔断（后续读仍可用）。
	s := newScript().add(registerReply(0x11, 0, 1)).
		// 首帧 Read Tag 回 0x06 + 一片 DINT 数据。
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0x06, nil,
			[]byte{enipcodec.TypeDINT, 0, 1, 0, 0, 0})).
		// Forward Open 成功。
		add(rrReply(0x11, enipcodec.ServiceForwardOpen, 0, nil, forwardOpenReply(0x70000001, 0x70000002, 1, 0x1234, 42))).
		// 分片续读回标签不存在（0x05）。
		add(unitReply(0x11, enipcodec.ServiceReadFrag, 0x05, 0x70000001, 1, nil))
	c, _ := openScript(t, s)

	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("a", "DINT_TAG", model.TypeInt32),
	}}})
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("samples = %+v", samples)
	}

	// 链路未熔断：再读一次（单帧即可满足）应当成功。
	s.add(dintReply(7))
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("b", "DINT_TAG", model.TypeInt32),
	}}})
	if err != nil {
		t.Fatalf("link should still be usable after a point-level fragment failure: %v", err)
	}
	if got, _ := samples[0].Value.AsFloat(); got != 7 {
		t.Fatalf("recovery read = %v, want 7", samples[0].Value)
	}
}
