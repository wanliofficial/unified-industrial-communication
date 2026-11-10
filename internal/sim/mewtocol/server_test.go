package mewtocol

import (
	"context"
	"net"
	"testing"
	"time"

	mewcodec "uic/internal/codec/mewtocol"
	"uic/internal/driver"
	mewdrv "uic/internal/driver/mewtocol"
	"uic/internal/model"
)

// 内置从站与主站的回环测试跑在真实 TCP 上：Mewtocol 的失败模式几乎都来自
// 「帧边界 + 站号过滤 + 字内低字节在前」，内存流测不出「两次读之间缓冲区里
// 还压着一帧」这类真实故障。

func newSim(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func open(t *testing.T, srv *Server) driver.Conn {
	t.Helper()
	ch := model.Channel{
		ID:        "ch-mew",
		Protocol:  "mewtocol",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := mewdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "plc1", ChannelID: ch.ID, UnitID: 1,
			Interval: model.Duration(time.Second), Enabled: true,
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx() context.Context { return context.Background() }

func ref(id, area string, off, length int) driver.PointRef {
	return driver.PointRef{
		ID: id, Address: model.Address{Kind: model.AddressRegister, Area: area, Offset: off, Length: length, Bit: -1},
		DataType: model.TypeUint16,
	}
}

// 字区往返：写 DT0..2 再读回，验证驱动编解码与从站内存表在字节序上一致。
func TestWordRoundTrip(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	res, err := c.Write(ctx(), []driver.WriteRequest{
		{PointID: "d0", Address: model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 0x0063)},
		{PointID: "d1", Address: model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 1, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 0x3344)},
		{PointID: "d2", Address: model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 2, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 0x000A)},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, r := range res {
		if !r.OK {
			t.Fatalf("write %s not ok: %v", r.PointID, r.Err)
		}
	}

	span := driver.ReadSpan{ID: "s1", Points: []driver.PointRef{
		ref("d0", "dt", 0, 1), ref("d1", "dt", 1, 1), ref("d2", "dt", 2, 1),
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d", len(samples))
	}
	want := []float64{0x0063, 0x3344, 0x000A}
	for i, s := range samples {
		if s.Err != nil {
			t.Fatalf("sample %d: %v", i, s.Err)
		}
		if s.Value.Num != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, s.Value.Num, want[i])
		}
		if s.Quality != model.QualityGood {
			t.Fatalf("sample %d quality = %v", i, s.Quality)
		}
	}
}

// 接点区按字读 + 位取：RCC 一次读回一个字（16 个接点），驱动按位拆开。
func TestContactWordReadBit(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	// X0.0 = 0, X0.1 = 1, X0.5 = 1，其余为 0。
	srv.SetBit("x", 0, false)
	srv.SetBit("x", 1, true)
	srv.SetBit("x", 5, true)

	span := driver.ReadSpan{ID: "sx", Points: []driver.PointRef{
		{ID: "x0", Address: model.Address{Kind: model.AddressRegister, Area: "x", Offset: 0, Length: 1, Bit: 0}, DataType: model.TypeBool},
		{ID: "x1", Address: model.Address{Kind: model.AddressRegister, Area: "x", Offset: 0, Length: 1, Bit: 1}, DataType: model.TypeBool},
		{ID: "x5", Address: model.Address{Kind: model.AddressRegister, Area: "x", Offset: 0, Length: 1, Bit: 5}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []bool{false, true, true}
	for i, s := range samples {
		if s.Err != nil {
			t.Fatalf("sample %d: %v", i, s.Err)
		}
		if s.Value.Bool != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, s.Value.Bool, want[i])
		}
	}
}

// 布尔量单点写（WCS）：从站读回接点状态。
func TestWriteContactSingle(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	res, err := c.Write(ctx(), []driver.WriteRequest{
		{PointID: "y0", Address: model.Address{Kind: model.AddressRegister, Area: "y", Offset: 0, Length: 1, Bit: 3}, DataType: model.TypeBool, Value: model.BoolValue(true)},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write not ok: %v", res[0].Err)
	}
	if !srv.Bit("y", 3) {
		t.Fatal("Y0.3 should be set")
	}
}

// 字区上的布尔点位写必须被拒绝：协议没有原子写，驱动宁可不写也不做非原子写。
func TestWriteBitOnWordAreaRejected(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	_, err := c.Write(ctx(), []driver.WriteRequest{
		{PointID: "d0b", Address: model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 0, Length: 1, Bit: 0}, DataType: model.TypeBool, Value: model.BoolValue(true)},
	})
	if err == nil {
		t.Fatal("writing a bool point on a word area must be rejected")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("error kind = %v, want unsupported", driver.KindOf(err))
	}
}

// 32 位值跨字：验证「低字在前」的字节序在驱动与从站之间一致。
func TestWordRoundTrip32(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	val := model.NumValue(model.TypeUint32, 0x00010002)
	_, err := c.Write(ctx(), []driver.WriteRequest{
		{PointID: "d0", Address: model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 0, Length: 2, Bit: -1}, DataType: model.TypeUint32, Value: val},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	span := driver.ReadSpan{ID: "s32", Points: []driver.PointRef{
		{ID: "d0", Address: model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 0, Length: 2, Bit: -1}, DataType: model.TypeUint32},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Err != nil {
		t.Fatalf("sample: %v", samples[0].Err)
	}
	if samples[0].Value.Num != 0x00010002 {
		t.Fatalf("value = %v, want 0x00010002", samples[0].Value.Num)
	}
}

// 地址越界：读超出范围时从站回 66H，驱动应映射为地址类错误。
func TestAddressError(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	// 用一个字区但请求一个不存在的极大范围。驱动会先按地址校验，这里用正常
	// 地址但让从站越界更直接：读 Y 区第 5 字（从站有该映射但无数据不应出错）。
	// 改用「索引寄存器 ID 只读 0..1」：读 ID 5 越界。
	span := driver.ReadSpan{ID: "sid", Points: []driver.PointRef{
		{ID: "id5", Address: model.Address{Kind: model.AddressRegister, Area: "id", Offset: 5, Length: 1, Bit: -1}, DataType: model.TypeUint16},
	}}
	_, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatal("expected an error for out-of-range index register")
	}
	if k := driver.KindOf(err); k != driver.ErrAddress && k != driver.ErrProtocol {
		t.Fatalf("error kind = %v, want address or protocol", k)
	}
}

// 站号过滤：发给不同站号的请求，从站不应当作本机处理。这里直接构造原始
// 连接验证过滤行为——从站对站号 2 的请求静默。
func TestStationFiltering(t *testing.T) {
	srv := newSim(t)

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 站号 2 的请求：从站（站号 1）不应当应答。
	frame := mewcodec.AppendBCC([]byte("%02#RDD0000000002"))
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("station 2 request got a response from station 1: %q", buf[:n])
	}

	// 站号 1 的请求：应当得到应答。
	frame = mewcodec.AppendBCC([]byte("%01#RDD0000000002"))
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("station 1 request should be answered: %v", err)
	}
	resp, rerr := mewcodec.ParseResponse(buf[:n])
	if rerr != nil {
		t.Fatalf("parse response: %v", rerr)
	}
	if resp.Station != 1 {
		t.Fatalf("response station = %d, want 1", int(resp.Station))
	}
}

// T/C 接点按点读（RCS/RCP），不带位号。这条防的是「用按字读去读 T/C」——
// 那会把 T/C 按每字 16 点重新编号，值看起来有但对应关系全错。
func TestTimerContactRead(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	srv.SetBit("t", 10, true)
	srv.SetBit("c", 999, true)

	// 规划层按区分组：T 与 C 是两个区，必须分开读。
	spanT := driver.ReadSpan{ID: "st", Points: []driver.PointRef{
		{ID: "t10", Address: model.Address{Kind: model.AddressRegister, Area: "t", Offset: 10, Length: 1, Bit: -1}, DataType: model.TypeBool},
		{ID: "t11", Address: model.Address{Kind: model.AddressRegister, Area: "t", Offset: 11, Length: 1, Bit: -1}, DataType: model.TypeBool},
	}}
	spanC := driver.ReadSpan{ID: "sc", Points: []driver.PointRef{
		{ID: "c999", Address: model.Address{Kind: model.AddressRegister, Area: "c", Offset: 999, Length: 1, Bit: -1}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{spanT, spanC})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d", len(samples))
	}
	got := map[string]bool{}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("sample %s: %v", s.PointID, s.Err)
		}
		got[s.PointID] = s.Value.Bool
	}
	want := map[string]bool{"t10": true, "t11": false, "c999": true}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s = %v, want %v", id, got[id], w)
		}
	}
}

// T/C 单点写（WCS）也走「一个号一个接点」，不碰邻号。
func TestTimerContactWrite(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	res, err := c.Write(ctx(), []driver.WriteRequest{
		{PointID: "t10", Address: model.Address{Kind: model.AddressRegister, Area: "t", Offset: 10, Length: 1, Bit: -1}, DataType: model.TypeBool, Value: model.BoolValue(true)},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write not ok: %v", res[0].Err)
	}
	if !srv.Bit("t", 10) {
		t.Fatal("T10 should be set")
	}
	if srv.Bit("t", 11) {
		t.Fatal("T11 must be untouched")
	}
}

// WCP 多接点写：一次改多位，但不碰未列出的接点。这条同时验证从站把
// 「数据位」读对了——读错时症状是「写 1 变成写 0」，值看起来有、方向反了。
func TestMultiContactWrite(t *testing.T) {
	srv := newSim(t)

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	frame, err := mewcodec.BuildWriteContacts(1, []mewcodec.Contact{
		{Area: mustArea(t, "y"), Word: 0, Bit: 1, Value: true, HasValue: true},
		{Area: mustArea(t, "y"), Word: 0, Bit: 5, Value: true, HasValue: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp, err := mewcodec.ParseResponse(buf[:n])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.ErrCode != 0 {
		t.Fatalf("response carries error %02XH", resp.ErrCode)
	}

	if !srv.Bit("y", 1) || !srv.Bit("y", 5) {
		t.Fatal("Y0.1 and Y0.5 should be set")
	}
	for _, b := range []int{0, 2, 3, 4, 6, 7} {
		if srv.Bit("y", b) {
			t.Fatalf("Y0.%d must be untouched", b)
		}
	}
}

func mustArea(t *testing.T, key string) mewcodec.Area {
	t.Helper()
	a, ok := mewcodec.Lookup(key)
	if !ok {
		t.Fatalf("area %q missing", key)
	}
	return a
}
