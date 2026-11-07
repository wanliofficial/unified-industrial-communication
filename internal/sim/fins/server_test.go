package fins

import (
	"context"
	"testing"
	"time"

	"uic/internal/driver"
	finsdrv "uic/internal/driver/fins"
	"uic/internal/model"
)

// 内置从站与主站的回环测试全部跑在真实 TCP 上：FINS 的失败模式很多来自
// 节点号协商、帧长度与连接生命周期，内存流测不出来。

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
		ID: "ch-fins", Protocol: "fins",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := finsdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "plc1", ChannelID: ch.ID, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx() context.Context { return context.Background() }

func TestReadWriteWords(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	res, err := c.Write(ctx(), []driver.WriteRequest{
		{PointID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 0x1234)},
		{PointID: "dm1", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Length: 1, Bit: -1}, DataType: model.TypeUint16, Value: model.NumValue(model.TypeUint16, 0x5678)},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, r := range res {
		if !r.OK {
			t.Fatalf("write %s not ok: %v", r.PointID, r.Err)
		}
	}
	if srv.GetWord("dm", 0) != 0x1234 || srv.GetWord("dm", 1) != 0x5678 {
		t.Fatalf("memory = %04X %04X", srv.GetWord("dm", 0), srv.GetWord("dm", 1))
	}

	span := driver.ReadSpan{ID: "s1", Points: []driver.PointRef{
		{ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
		{ID: "dm1", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Length: 1, Bit: -1}, DataType: model.TypeUint16},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d", len(samples))
	}
	if v, ok := samples[0].Value.AsFloat(); !ok || uint16(v) != 0x1234 {
		t.Fatalf("dm0 = %v", samples[0].Value)
	}
	if v, ok := samples[1].Value.AsFloat(); !ok || uint16(v) != 0x5678 {
		t.Fatalf("dm1 = %v", samples[1].Value)
	}
}

func TestReadFloat32LowWordFirst(t *testing.T) {
	srv := newSim(t)
	// 48.0 = 0x42400000，低字在前：DM2=0x0000，DM3=0x4240。
	srv.SetWord("dm", 2, 0x0000)
	srv.SetWord("dm", 3, 0x4240)
	c := open(t, srv)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "temp", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 2, Length: 2, Bit: -1},
		DataType: model.TypeFloat32,
	}}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if v, ok := samples[0].Value.AsFloat(); !ok || v != 48 {
		t.Fatalf("temp = %v", samples[0].Value)
	}
}

func TestBitReadWriteSharesWordMemory(t *testing.T) {
	srv := newSim(t)
	srv.SetBit("cio", 10, 0, true)
	srv.SetWord("cio", 11, 0xFFFF)
	c := open(t, srv)

	span := driver.ReadSpan{ID: "bits", Points: []driver.PointRef{
		{ID: "run", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 10, Bit: 0}, DataType: model.TypeBool},
		{ID: "fault", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 10, Bit: 1}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !samples[0].Value.Bool || samples[1].Value.Bool {
		t.Fatalf("run=%v fault=%v", samples[0].Value.Bool, samples[1].Value.Bool)
	}

	res, err := c.Write(ctx(), []driver.WriteRequest{{
		PointID:  "fault",
		Address:  model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 10, Bit: 1},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	// 位写不能破坏同字的其它位：run 仍在 bit0。
	if got := srv.GetWord("cio", 10); got != 0x0003 {
		t.Fatalf("CIO10 = %04X", got)
	}
	if srv.GetWord("cio", 11) != 0xFFFF {
		t.Fatalf("neighbour word changed: %04X", srv.GetWord("cio", 11))
	}
}

func TestMixedWordAndBitSpan(t *testing.T) {
	srv := newSim(t)
	srv.SetWord("dm", 0, 0x1234)
	srv.SetBit("dm", 0, 0, true)
	c := open(t, srv)

	span := driver.ReadSpan{ID: "mixed", Points: []driver.PointRef{
		{ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
		{ID: "dm0.0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: 0}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if v, ok := samples[0].Value.AsFloat(); !ok || uint16(v) != 0x1235 {
		t.Fatalf("dm0 = %v", samples[0].Value)
	}
	if !samples[1].Value.Bool {
		t.Fatal("dm0.0 = false")
	}
}

func TestStringRoundTrip(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	res, err := c.Write(ctx(), []driver.WriteRequest{{
		PointID:  "name",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 100, Length: 4, Bit: -1},
		DataType: model.TypeString, Value: model.TextValue(model.TypeString, "PUMP-01"),
	}})
	if err != nil || !res[0].OK {
		t.Fatalf("write: %v %+v", err, res)
	}
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "name", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 100, Length: 4, Bit: -1},
		DataType: model.TypeString, Length: 4,
	}}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := samples[0].Value.Text; got != "PUMP-01" {
		t.Fatalf("name = %q", got)
	}
}

func TestAddressOutOfRange(t *testing.T) {
	srv := newSim(t)
	c := open(t, srv)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "last", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0xFFFF, Length: 2, Bit: -1},
		DataType: model.TypeUint16, Length: 2,
	}}}
	samples, _ := c.Read(ctx(), []driver.ReadSpan{span})
	if len(samples) != 1 || samples[0].Quality != model.QualityAddressError {
		t.Fatalf("sample = %+v", samples[0])
	}
}

func TestUnitIDOverridesDestNode(t *testing.T) {
	srv := newSim(t)
	srv.SetWord("dm", 0, 0x00AA)
	ch := model.Channel{
		ID: "ch", Protocol: "fins",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := finsdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "plc", ChannelID: "ch", UnitID: 1, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "dm0", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
	}}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if v, ok := samples[0].Value.AsFloat(); !ok || uint16(v) != 0x00AA {
		t.Fatalf("dm0 = %v", samples[0].Value)
	}
}
