package enip

import (
	"context"
	"encoding/binary"
	"testing"

	enipcodec "uic/internal/codec/enip"
	"uic/internal/driver"
	"uic/internal/model"
)

// unitReply 生成一条 SendUnitData 应答（已连接报文）。
func unitReply(session uint32, service, status byte, connID uint32, seq uint16, data []byte) []byte {
	reply, err := enipcodec.BuildReply(service, status, nil, data)
	if err != nil {
		panic(err)
	}
	payload, err := enipcodec.BuildSendUnitData(connID, seq, reply)
	if err != nil {
		panic(err)
	}
	return packet(enipcodec.CmdSendUnitData, session, 0, [8]byte{}, payload)
}

// forwardOpenReply 生成 Forward Open 应答体。
func forwardOpenReply(ot, to uint32, serial, vendor uint16, orig uint32) []byte {
	b := make([]byte, 26)
	binary.LittleEndian.PutUint32(b[0:], ot)
	binary.LittleEndian.PutUint32(b[4:], to)
	binary.LittleEndian.PutUint16(b[8:], serial)
	binary.LittleEndian.PutUint16(b[10:], vendor)
	binary.LittleEndian.PutUint32(b[12:], orig)
	return b
}

func TestReadBytesFragmented(t *testing.T) {
	srv := newSim(t)
	want := make([]byte, 1000)
	for i := range want {
		want[i] = byte(i)
	}
	if err := srv.SetTag("BIG_SINT", enipcodec.TypeSINT, len(want), want); err != nil {
		t.Fatal(err)
	}
	// 每帧只给 64 字节：1000 字节要读 16 片，足以覆盖续读与偏移累加。
	srv.SetFragmentSize(64)

	c := openSim(t, srv)
	p := ref("big", "BIG_SINT", model.TypeBytes)
	p.Address.Length = len(want)
	s := readOne(t, c, p)
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %q err=%v", s.Quality, s.Err)
	}
	if s.Value.Text != string(want) {
		t.Fatalf("bytes = %d, want %d", len(s.Value.Text), len(want))
	}
}

func TestReadNumericOffsetFragmented(t *testing.T) {
	srv := newSim(t)
	values := make([]int32, 300)
	for i := range values {
		values[i] = int32(i * 3)
	}
	if err := srv.SetArray("LONG_ARRAY", values); err != nil {
		t.Fatal(err)
	}
	srv.SetFragmentSize(64) // 251 个 DINT = 1004 字节，必然分片

	c := openSim(t, srv)
	p := ref("last", "LONG_ARRAY", model.TypeInt32)
	p.Address.Offset = 250
	s := readOne(t, c, p)
	if s.Quality != model.QualityGood {
		t.Fatalf("quality = %q err=%v", s.Quality, s.Err)
	}
	if got, _ := s.Value.AsFloat(); got != 750 {
		t.Fatalf("value = %v, want 750", s.Value)
	}
}

func TestWriteBytesFragmented(t *testing.T) {
	srv := newSim(t)
	if err := srv.SetTag("RAW_SINT", enipcodec.TypeSINT, 1000, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	srv.SetFragmentSize(64)

	c := openSim(t, srv)
	want := make([]byte, 1000)
	for i := range want {
		want[i] = byte(255 - i%251)
	}
	p := ref("raw", "RAW_SINT", model.TypeBytes)
	p.Address.Length = len(want)
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: p.ID, Address: p.Address, DataType: p.DataType, Value: model.BytesValue(want),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	_, _, got, ok := srv.Tag("RAW_SINT")
	if !ok || string(got) != string(want) {
		t.Fatalf("tag bytes = %d, want %d", len(got), len(want))
	}
}

func TestWriteStringFragmented(t *testing.T) {
	srv := newSim(t)
	if err := srv.SetString("LONG_STRING", "", 500); err != nil {
		t.Fatal(err)
	}
	srv.SetFragmentSize(64)

	c := openSim(t, srv)
	text := ""
	for len(text) < 400 {
		text += "PUMP-01;"
	}
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "name",
		Address:  model.Address{Kind: model.AddressRegister, Area: "LONG_STRING", Bit: -1, Length: 500},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, text),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	_, _, got, _ := srv.Tag("LONG_STRING")
	if n := int(binary.LittleEndian.Uint32(got)); n != len(text) || string(got[4:4+n]) != text {
		t.Fatalf("string length = %d, want %d", n, len(text))
	}
}

func TestFragmentedNeedsForwardOpen(t *testing.T) {
	// 设备回 0x06 说明数据要分片；Forward Open 被拒时必须报「不支持」，
	// 而不是把分片失败的半截数据当成功上报。
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0x06, nil,
			[]byte{enipcodec.TypeDINT, 0, 1, 0, 0, 0})).
		add(rrReply(0x11, enipcodec.ServiceForwardOpen, 0x08, nil, nil))
	c, _ := openScript(t, s)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		ref("x", "DINT_TAG", model.TypeInt32),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v (%v)", err, driver.KindOf(err))
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestFragmentedReopensLostConnection(t *testing.T) {
	// 连接被目标释放后回 0x07（connection lost）。驱动应重开一次连接并
	// 继续读，而不是把整点标坏。
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0x06, nil,
			[]byte{enipcodec.TypeDINT, 0, 1, 0, 0, 0})).
		add(rrReply(0x11, enipcodec.ServiceForwardOpen, 0, nil,
			forwardOpenReply(0x70000001, 0x70000002, 1, 0x1234, 42))).
		add(unitReply(0x11, enipcodec.ServiceReadFrag, 0x07, 0x70000001, 1, nil)).
		add(rrReply(0x11, enipcodec.ServiceForwardOpen, 0, nil,
			forwardOpenReply(0x70000003, 0x70000004, 2, 0x1234, 42))).
		add(unitReply(0x11, enipcodec.ServiceReadFrag, 0, 0x70000003, 1,
			[]byte{enipcodec.TypeDINT, 0, 2, 0, 0, 0}))
	c, _ := openScript(t, s)
	p := ref("second", "DINT_ARRAY", model.TypeInt32)
	p.Address.Offset = 1
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityGood {
		t.Fatalf("samples = %+v", samples)
	}
	if got, _ := samples[0].Value.AsFloat(); got != 2 {
		t.Fatalf("value = %v, want 2", samples[0].Value)
	}
}

func TestFragmentedReadRejectsTypeChange(t *testing.T) {
	// 分片途中类型变了说明偏移/应答已经错位，继续拼接会得到一段看似合法、
	// 实际混了两种类型的字节。
	s := newScript().add(registerReply(0x11, 0, 1)).
		add(rrReply(0x11, enipcodec.ServiceReadTag, 0x06, nil,
			[]byte{enipcodec.TypeDINT, 0, 1, 0, 0, 0})).
		add(rrReply(0x11, enipcodec.ServiceForwardOpen, 0, nil,
			forwardOpenReply(0x70000001, 0x70000002, 1, 0x1234, 42))).
		add(unitReply(0x11, enipcodec.ServiceReadFrag, 0, 0x70000001, 1,
			[]byte{enipcodec.TypeINT, 0, 1, 0}))
	c, _ := openScript(t, s)
	p := ref("x", "DINT_TAG", model.TypeInt32)
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{p}}})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("err = %v (%v)", err, driver.KindOf(err))
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityBad {
		t.Fatalf("samples = %+v", samples)
	}
}
