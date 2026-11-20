package s7

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	s7codec "uic/internal/codec/s7"
	"uic/internal/driver"
	"uic/internal/model"
	s7sim "uic/internal/sim/s7"
)

// 这些用例全部跑在真实 TCP 上：S7 的失败模式里有一半来自「帧边界与连接
// 生命周期」（TPKT 长度、COTP 分流、断链后残留缓冲），用内存流测不出来。

func newSim(t *testing.T, pduLen int) *s7sim.Server {
	t.Helper()
	srv, err := s7sim.NewServer("127.0.0.1:0", pduLen)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func open(t *testing.T, srv *s7sim.Server, ch model.Channel) driver.Conn {
	t.Helper()
	if ch.ID == "" {
		ch = model.Channel{
			ID: "ch-s7", Protocol: "s7",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
			Rack:      0, Slot: 2,
		}
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "plc1", ChannelID: ch.ID, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func span(id string, pts ...driver.PointRef) driver.ReadSpan {
	return driver.ReadSpan{ID: id, Points: pts}
}

func pt(id, area string, db, offset int, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: dt,
		Address: model.Address{Kind: model.AddressRegister, Area: area, DB: db, Offset: offset, Bit: -1},
	}
}

func bitPt(id, area string, db, offset, bit int) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: model.TypeBool,
		Address: model.Address{Kind: model.AddressRegister, Area: area, DB: db, Offset: offset, Bit: bit},
	}
}

func byID(t *testing.T, raws []driver.RawSample) map[string]driver.RawSample {
	t.Helper()
	out := make(map[string]driver.RawSample, len(raws))
	for _, r := range raws {
		out[r.PointID] = r
	}
	return out
}

func TestMetaDeclaresTCPOnly(t *testing.T) {
	m := New().Meta()
	if m.ID != "s7" {
		t.Fatalf("id = %q", m.ID)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "tcp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if !m.Accepts(model.AddressRegister) {
		t.Fatal("s7 must accept register addressing")
	}
	if m.Capabilities.BitAccess != driver.CapSupported {
		t.Fatal("s7 must declare bit access")
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported {
		t.Fatal("s7 must not claim subscribe")
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "s7",
			Transport: model.Transport{Kind: "serial", Port: "/dev/ttyS0"}},
		Device: model.Device{ID: "d"},
	})
	if err == nil {
		t.Fatal("serial transport must be rejected")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

func TestReadAcrossAreasAndTypes(t *testing.T) {
	srv := newSim(t, 480)
	// DB1: REAL 12.5 @0, INT16 -3 @4, DINT 100000 @6, STRING "PUMP-01" @10
	// （STRING 带 2 字节头：max=7, cur=7，总宽 9）
	db1 := make([]byte, 19)
	binary.BigEndian.PutUint32(db1[0:], math.Float32bits(12.5))
	binary.BigEndian.PutUint16(db1[4:], uint16(0xFFFD))
	binary.BigEndian.PutUint32(db1[6:], uint32(100000))
	db1[10], db1[11] = 7, 7
	copy(db1[12:], "PUMP-01")
	srv.SetBytes(s7codec.AreaDB, 1, 0, db1)
	// DB2: WORD 0xBEEF @0
	srv.SetBytes(s7codec.AreaDB, 2, 0, []byte{0xBE, 0xEF})
	srv.SetBytes(s7codec.AreaInput, 0, 0, []byte{0x00, 0x2A})
	srv.SetBit(s7codec.AreaMerker, 0, 0, 3, true)

	c := open(t, srv, model.Channel{})
	raws, err := c.Read(context.Background(), []driver.ReadSpan{
		span("db1", pt("temp", "db", 1, 0, model.TypeFloat32),
			pt("count", "db", 1, 4, model.TypeInt16),
			pt("big", "db", 1, 6, model.TypeInt32),
			driver.PointRef{ID: "label", DataType: model.TypeString,
				Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 10, Bit: -1}, Length: 9}),
		span("db2", pt("word", "db", 2, 0, model.TypeUint16)),
		span("io", pt("input", "input", 0, 0, model.TypeUint16), bitPt("flag", "merker", 0, 0, 3)),
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := byID(t, raws)
	want := map[string]float64{"temp": 12.5, "count": -3, "big": 100000, "word": 0xBEEF, "input": 0x2A}
	for id, v := range want {
		if got[id].Quality != model.QualityGood {
			t.Fatalf("%s quality = %v (%v)", id, got[id].Quality, got[id].Err)
		}
		if got[id].Value.Num != v {
			t.Fatalf("%s = %v, want %v", id, got[id].Value.Num, v)
		}
	}
	if got["label"].Value.Text != "PUMP-01" {
		t.Fatalf("label = %q", got["label"].Value.Text)
	}
	if !got["flag"].Value.Bool {
		t.Fatal("flag should be true")
	}
	// 六个点位跨三个区，理想情况一次事务读完。
	if n := srv.RequestCount(); n != 1 {
		t.Fatalf("requests = %d, want 1 (multi-item read)", n)
	}
}

func TestWriteThenReadBack(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})

	reqs := []driver.WriteRequest{
		{PointID: "setpoint", DataType: model.TypeFloat32,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1},
			Value:   model.NumValue(model.TypeFloat32, 3.25)},
		{PointID: "run", DataType: model.TypeBool,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 8, Bit: 2},
			Value:   model.BoolValue(true)},
		{PointID: "name", DataType: model.TypeString,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 10, Length: 8, Bit: -1},
			Value:   model.TextValue(model.TypeString, "B-01")},
	}
	res, err := c.Write(context.Background(), reqs)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("results = %d", len(res))
	}
	for _, r := range res {
		if !r.OK {
			t.Fatalf("%s write failed: %v", r.PointID, r.Err)
		}
	}

	raws, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		pt("sp", "db", 1, 0, model.TypeFloat32),
		bitPt("run", "db", 1, 8, 2),
		driver.PointRef{ID: "name", DataType: model.TypeString,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 10, Bit: -1}, Length: 8},
	)})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := byID(t, raws)
	if got["sp"].Value.Num != 3.25 {
		t.Fatalf("sp = %v", got["sp"].Value.Num)
	}
	if !got["run"].Value.Bool {
		t.Fatal("run bit not set")
	}
	if got["name"].Value.Text != "B-01" {
		t.Fatalf("name = %q", got["name"].Value.Text)
	}
	// 位写不能破坏同字节的其它位。
	srv.SetBit(s7codec.AreaDB, 1, 8, 0, true)
	srv.SetBit(s7codec.AreaDB, 1, 8, 7, true)
	raws, _ = c.Read(context.Background(), []driver.ReadSpan{span("s", bitPt("run", "db", 1, 8, 2))})
	if !byID(t, raws)["run"].Value.Bool {
		t.Fatal("bit 2 lost after writing neighbouring bits")
	}
}

func TestWriteToInputAreaIsPermissionError(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})
	res, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "bad", DataType: model.TypeUint16,
		Address: model.Address{Kind: model.AddressRegister, Area: "input", Offset: 0, Bit: -1},
		Value:   model.NumValue(model.TypeUint16, 1),
	}})
	if len(res) != 1 || res[0].OK {
		t.Fatalf("write to input must fail: %+v", res)
	}
	if driver.KindOf(res[0].Err) != driver.ErrPermission {
		t.Fatalf("kind = %v, want permission", driver.KindOf(res[0].Err))
	}
}

func TestAddressOutOfRangeKeepsGoodSamples(t *testing.T) {
	srv := newSim(t, 480)
	srv.SetBytes(s7codec.AreaDB, 1, 0, []byte{0x12, 0x34})
	c := open(t, srv, model.Channel{})

	raws, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		pt("ok", "db", 1, 0, model.TypeUint16),
		pt("oob", "db", 1, 4090, model.TypeUint16),
		pt("nodb", "db", 99, 0, model.TypeUint16),
	)})
	// 部分点位失败不该让整台设备失败：Modbus 驱动也是同一策略。
	if err != nil {
		t.Fatalf("partial read must not fail device: %v", err)
	}
	got := byID(t, raws)
	if got["ok"].Quality != model.QualityGood || got["ok"].Value.Num != 0x1234 {
		t.Fatalf("good sample lost: %+v", got["ok"])
	}
	for _, id := range []string{"oob", "nodb"} {
		if got[id].Quality != model.QualityAddressError {
			t.Fatalf("%s quality = %v, want address error", id, got[id].Quality)
		}
		if !strings.Contains(got[id].Err.Error(), "object does not exist") &&
			!strings.Contains(got[id].Err.Error(), "address out of range") {
			t.Fatalf("%s err = %v", id, got[id].Err)
		}
	}
}

func TestAllPointsFailingReturnsError(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})
	_, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		pt("missing", "db", 42, 0, model.TypeUint16))})
	if err == nil {
		t.Fatal("all-point failure must surface an error")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
}

// PDU 协商值必须真的被用来切块：模拟一台只支持 240 字节的 S7-300。
func TestChunkingHonoursNegotiatedPDULen(t *testing.T) {
	srv := newSim(t, 240)
	// 40 个 float32，每项请求 12 字节、响应 8 字节，必然超过 240。
	for i := 0; i < 40; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(float32(i)))
		srv.SetBytes(s7codec.AreaDB, 1, i*4, b[:])
	}
	c := open(t, srv, model.Channel{})

	var pts []driver.PointRef
	for i := 0; i < 40; i++ {
		pts = append(pts, pt("p"+itoa(i), "db", 1, i*4, model.TypeFloat32))
	}
	raws, err := c.Read(context.Background(), []driver.ReadSpan{span("s", pts...)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(raws) != 40 {
		t.Fatalf("samples = %d, want 40", len(raws))
	}
	for _, r := range raws {
		if r.Quality != model.QualityGood {
			t.Fatalf("%s: %v", r.PointID, r.Err)
		}
	}
	if n := srv.RequestCount(); n < 2 {
		t.Fatalf("requests = %d, want >1 (chunked by pdu budget)", n)
	}
}

func TestChunkPlannerRespectsItemLimit(t *testing.T) {
	// 协商 960 时，20 项上限必须先于长度上限生效。
	var items []s7codec.Item
	for i := 0; i < 45; i++ {
		items = append(items, s7codec.Item{Area: s7codec.AreaDB, DB: 1, ByteAddr: i, Bit: -1, Bytes: 1})
	}
	chunks := planChunks(items, 960, false)
	got := make([]int, len(chunks))
	for i, c := range chunks {
		got[i] = len(c)
	}
	for i, n := range got {
		if n > 20 {
			t.Fatalf("chunk %d has %d items, limit 20", i, n)
		}
	}
	total := 0
	for _, n := range got {
		total += n
	}
	if total != 45 {
		t.Fatalf("chunked %d items, want 45", total)
	}
}

func TestReadDetectsBrokenLink(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})
	_ = srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Read(ctx, []driver.ReadSpan{span("s", pt("p", "db", 1, 0, model.TypeUint16))})
	if err == nil {
		t.Fatal("read after server shutdown must fail")
	}
	if k := driver.KindOf(err); k != driver.ErrDisconnected && k != driver.ErrTimeout {
		t.Fatalf("kind = %v", k)
	}
}

func TestProbeTreatsItemErrorAsAlive(t *testing.T) {
	// 探活读 M0；删掉 merker 区让从站返回「对象不存在」。只要还应答，
	// 就说明链路是通的。
	srv, err := s7sim.NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c := open(t, srv, model.Channel{})
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestHandshakeFailureIsProtocolError(t *testing.T) {
	// 一个只接受连接、不回任何数据的「哑服务器」。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = New().Open(ctx, driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "s7",
			Transport: model.Transport{Kind: "tcp", Address: ln.Addr().String()},
			Timeout:   model.Duration(300 * time.Millisecond)},
		Device: model.Device{ID: "d"},
	})
	if err == nil {
		t.Fatal("handshake against silent server must fail")
	}
	if k := driver.KindOf(err); k != driver.ErrTimeout && k != driver.ErrDisconnected {
		t.Fatalf("kind = %v", k)
	}
}

func TestRejectsDBWithoutNumber(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})
	raws, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		driver.PointRef{ID: "bad", DataType: model.TypeUint16,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", Offset: 0, Bit: -1}})})
	if err == nil {
		t.Fatal("db area without db number must fail")
	}
	if len(raws) != 1 || raws[0].Quality != model.QualityBad {
		t.Fatalf("bad point must be reported: %+v", raws)
	}
}

// itoa 避免为一个测试引入 strconv 的噪音。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 写请求的数据区也占 PDU：只按「12 字节/项」切块会在长字符串上爆掉。
func TestPlanChunksWriteCountsPayload(t *testing.T) {
	items := make([]s7codec.Item, 10)
	for i := range items {
		items[i] = s7codec.Item{Area: s7codec.AreaDB, DB: 1, ByteAddr: i * 40, Bit: -1, Bytes: 40}
	}
	chunks := planChunks(items, 240, true)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >1", len(chunks))
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != len(items) {
		t.Fatalf("lost items: %d/%d", total, len(items))
	}
	// 一个 60 字节项：10 头 + 2 参数 + 12 规格 + 4 数据头 + 60 = 88 < 240。
	one := planChunks([]s7codec.Item{{Area: s7codec.AreaDB, DB: 1, Bytes: 60}}, 240, true)
	if len(one) != 1 || len(one[0]) != 1 {
		t.Fatalf("single oversized-ish item = %+v", one)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	srv := newSim(t, 480)
	c := New()
	conn, err := c.Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "s7",
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(time.Second)},
		Device: model.Device{ID: "d"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := conn.Probe(context.Background()); err == nil {
		t.Fatal("probe after close must fail")
	}
}

func TestWriteToMissingDBReportsAddressError(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "ghost", DataType: model.TypeUint16,
		Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 77, Offset: 0, Bit: -1},
		Value:   model.NumValue(model.TypeUint16, 1),
	}})
	if err == nil {
		t.Fatal("write to non-existent DB must fail")
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("result = %+v", res)
	}
	if driver.KindOf(res[0].Err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(res[0].Err))
	}
}

func TestReadWithCancelledContextDoesNotTouchSocket(t *testing.T) {
	srv := newSim(t, 480)
	c := open(t, srv, model.Channel{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raws, err := c.Read(ctx, []driver.ReadSpan{span("s",
		pt("a", "db", 1, 0, model.TypeUint16),
		pt("b", "db", 1, 2, model.TypeUint16))})
	if err == nil {
		t.Fatal("cancelled read must fail")
	}
	if len(raws) != 2 {
		t.Fatalf("samples = %d, want 2 (both explicitly bad)", len(raws))
	}
	for _, r := range raws {
		if r.Quality != model.QualityTimeout {
			t.Fatalf("%s quality = %v", r.PointID, r.Quality)
		}
	}
}
