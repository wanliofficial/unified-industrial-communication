package hostlink

import (
	"context"
	"strings"
	"testing"
	"time"

	hlcodec "uic/internal/codec/hostlink"
	"uic/internal/driver"
	hostlinkdrv "uic/internal/driver/hostlink"
	"uic/internal/model"
)

// 内置从站与主站的回环测试全部跑在真实 TCP 上：Host Link 的失败模式几乎
// 都来自帧边界（`*`+CR）、FCS 范围、单元号过滤与连接生命周期，内存流测
// 不出来。

func newSim(t *testing.T, unit byte) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0", unit)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func open(t *testing.T, srv *Server, unit int) driver.Conn {
	t.Helper()
	ch := model.Channel{
		ID: "ch-hl", Protocol: "hostlink",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := hostlinkdrv.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{ID: "plc1", ChannelID: ch.ID, UnitID: unit,
			Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx() context.Context { return context.Background() }

func TestReadWriteWords(t *testing.T) {
	srv := newSim(t, 0)
	srv.SetWord("dm", 0, 0x1234)
	srv.SetWord("dm", 1, 0x5678)
	c := open(t, srv, 0)

	res, err := c.Write(ctx(), []driver.WriteRequest{{
		PointID:  "dm0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1},
		DataType: model.TypeUint16,
		Value:    model.NumValue(model.TypeUint16, 0xBEEF),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write not ok: %+v", res[0])
	}
	if got := srv.GetWord("dm", 0); got != 0xBEEF {
		t.Fatalf("dm0 = 0x%04X, want 0xBEEF", got)
	}

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		{ID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16},
		{ID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Length: 1, Bit: -1}, DataType: model.TypeUint16},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d", len(samples))
	}
	if samples[0].Value.Num != 0xBEEF || samples[1].Value.Num != 0x5678 {
		t.Fatalf("values = %v %v", samples[0].Value, samples[1].Value)
	}
}

func TestReadWriteBits(t *testing.T) {
	srv := newSim(t, 0)
	srv.SetBit("cio", 0, 1, true)
	srv.SetBit("cio", 0, 5, true)
	c := open(t, srv, 0)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{
		{ID: "b0", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 0}, DataType: model.TypeBool},
		{ID: "b1", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 1}, DataType: model.TypeBool},
		{ID: "b5", Address: model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 5}, DataType: model.TypeBool},
	}}
	samples, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Bool || !samples[1].Value.Bool || !samples[2].Value.Bool {
		t.Fatalf("bits = %v %v %v", samples[0].Value.Bool, samples[1].Value.Bool, samples[2].Value.Bool)
	}

	if _, err := c.Write(ctx(), []driver.WriteRequest{{
		PointID:  "b0",
		Address:  model.Address{Kind: model.AddressRegister, Area: "cio", Offset: 0, Bit: 0},
		DataType: model.TypeBool, Value: model.BoolValue(true),
	}}); err != nil {
		t.Fatalf("write bit: %v", err)
	}
	if !srv.Mem().GetBit("cio", 0, 0) {
		t.Fatal("cio0.0 not set")
	}
	// 邻位不能被碰：位写是原子的单点写，不是「读字改位写字」。
	if !srv.Mem().GetBit("cio", 0, 1) || !srv.Mem().GetBit("cio", 0, 5) {
		t.Fatal("neighbouring bits were cleared by a bit write")
	}
}

// TestWrongUnitIsSilent 检查单元号过滤：真 PLC 在共享总线上只回答点名到自己
// 的帧，模拟器如果对任何单元号都应答，「单元号配错」就测不出来。
func TestWrongUnitIsSilent(t *testing.T) {
	srv := newSim(t, 1)
	srv.SetWord("dm", 0, 1)
	c := open(t, srv, 2)

	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16,
	}}}
	start := time.Now()
	_, err := c.Read(context.Background(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatal("a request to a silent unit must time out, not succeed")
	}
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %q (%v), want timeout", driver.KindOf(err), err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestProbe(t *testing.T) {
	srv := newSim(t, 0)
	c := open(t, srv, 0)
	if err := c.Probe(ctx()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestUnknownAreaIsAddressError(t *testing.T) {
	srv := newSim(t, 0)
	c := open(t, srv, 0)
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "x", Address: model.Address{Kind: model.AddressRegister, Area: "nope", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16,
	}}}
	_, err := c.Read(ctx(), []driver.ReadSpan{span})
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
}

func TestOpenRejectsBadUnit(t *testing.T) {
	srv := newSim(t, 0)
	ch := model.Channel{
		ID: "ch", Protocol: "hostlink",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(time.Second),
	}
	for _, unit := range []int{-1, 32, 99} {
		_, err := hostlinkdrv.New().Open(context.Background(), driver.OpenConfig{
			Channel: ch,
			Device:  model.Device{ID: "plc", ChannelID: ch.ID, UnitID: unit},
		})
		if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
			t.Fatalf("unit %d: err = %v", unit, err)
		}
	}
}

func TestMeta(t *testing.T) {
	m := hostlinkdrv.New().Meta()
	if m.ID != "hostlink" || !m.Accepts(model.AddressRegister) {
		t.Fatalf("meta = %+v", m)
	}
	if len(m.Transports) != 2 {
		t.Fatalf("transports = %v", m.Transports)
	}
	if m.Capabilities.BitAccess != driver.CapSupported {
		t.Fatalf("bit access = %q", m.Capabilities.BitAccess)
	}
}

// TestChunkingRespectsFrameBudget 检查超过 267 字的写会被拆成多帧，而不是
// 被对端以帧长错拒掉。
func TestChunkingRespectsFrameBudget(t *testing.T) {
	srv := newSim(t, 0)
	c := open(t, srv, 0)

	words := hlcodec.MaxWriteWords + 10
	text := make([]byte, words*2)
	for i := range text {
		text[i] = byte('A' + i%26)
	}
	res, err := c.Write(ctx(), []driver.WriteRequest{{
		PointID:  "str",
		Address:  model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: words, Bit: -1},
		DataType: model.TypeString,
		Value:    model.TextValue(model.TypeString, string(text)),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res[0].OK {
		t.Fatalf("write not ok: %+v", res[0])
	}
	for i := 0; i < words; i++ {
		want := uint16(text[i*2])<<8 | uint16(text[i*2+1])
		if got := srv.GetWord("dm", i); got != want {
			t.Fatalf("dm%d = 0x%04X, want 0x%04X", i, got, want)
		}
	}
}

// TestSimAnswersFCSWithEndCode 检查从站对坏 FCS 回 0x13，而不是静默丢弃。
func TestSimAnswersFCSWithEndCode(t *testing.T) {
	srv := newSim(t, 0)
	body := "@00FA0" + "00000000" + "0101" + "82" + "0000" + "00" + "0001"
	frame := body + "00*" // 故意写错 FCS
	conn, err := dialRaw(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(append([]byte(frame), '\r')); err != nil {
		t.Fatal(err)
	}
	resp, err := readOne(conn)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := hlcodec.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v (%q)", err, resp)
	}
	if parsed.EndCode != 0x13 {
		t.Fatalf("end code = 0x%02X, want 0x13 (FCS error)", parsed.EndCode)
	}
}

// TestSimRejectsCModeFrame 检查拿 C 模式帧（不是 FA）发过来时回 0x16。
func TestSimRejectsCModeFrame(t *testing.T) {
	srv := newSim(t, 0)
	body := "@00RR0000" + "0001"
	sum := hlcodec.FCS([]byte(body))
	frame := body + string([]byte{"0123456789ABCDEF"[sum>>4], "0123456789ABCDEF"[sum&0x0f]}) + "*"

	conn, err := dialRaw(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(append([]byte(frame), '\r')); err != nil {
		t.Fatal(err)
	}
	resp, err := readOne(conn)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := hlcodec.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v (%q)", err, resp)
	}
	if parsed.EndCode != 0x16 {
		t.Fatalf("end code = 0x%02X, want 0x16 (command not supported)", parsed.EndCode)
	}
}

// TestSimIgnoresBareCRAndNoise 检查从站不会被裸 CR、噪声前缀或 `*CR` 后的
// CRLF 打乱帧边界。
func TestSimIgnoresBareCRAndNoise(t *testing.T) {
	srv := newSim(t, 0)
	srv.SetWord("dm", 0, 0x4242)
	conn, err := dialRaw(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body := "@00FA0" + "00000000" + "0101" + "82" + "0000" + "00" + "0001"
	sum := hlcodec.FCS([]byte(body))
	frame := body + string([]byte{"0123456789ABCDEF"[sum>>4], "0123456789ABCDEF"[sum&0x0f]}) + "*"
	// 噪声 + 一个裸 CR + 真帧 + `*CR` 后的 CRLF。
	payload := append([]byte("junk\r"), []byte(frame)...)
	payload = append(payload, '\r', '\n')
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	resp, err := readOne(conn)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := hlcodec.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v (%q)", err, resp)
	}
	if parsed.EndCode != 0 || parsed.FinsEndCode != 0 {
		t.Fatalf("end codes = %02X/%04X", parsed.EndCode, parsed.FinsEndCode)
	}
	if len(parsed.Data) != 2 || parsed.Data[0] != 0x42 || parsed.Data[1] != 0x42 {
		t.Fatalf("data = % x", parsed.Data)
	}
}

func TestSimEndCodeOnUnknownArea(t *testing.T) {
	srv := newSim(t, 0)
	conn, err := dialRaw(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 区码 0x7F：既不是字访问码也不是位访问码。
	body := "@00FA0" + "00000000" + "0101" + "7F" + "0000" + "00" + "0001"
	sum := hlcodec.FCS([]byte(body))
	frame := body + string([]byte{"0123456789ABCDEF"[sum>>4], "0123456789ABCDEF"[sum&0x0f]}) + "*"
	if _, err := conn.Write(append([]byte(frame), '\r')); err != nil {
		t.Fatal(err)
	}
	resp, err := readOne(conn)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := hlcodec.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v (%q)", err, resp)
	}
	if parsed.FinsEndCode != 0x1101 {
		t.Fatalf("FINS end code = 0x%04X, want 0x1101", parsed.FinsEndCode)
	}
}

func TestSimServesSeveralRequestsOnOneConnection(t *testing.T) {
	srv := newSim(t, 0)
	srv.SetWord("dm", 3, 7)
	conn, err := dialRaw(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < 5; i++ {
		body := "@00FA0" + "00000000" + "0101" + "82" + "0003" + "00" + "0001"
		sum := hlcodec.FCS([]byte(body))
		frame := body + string([]byte{"0123456789ABCDEF"[sum>>4], "0123456789ABCDEF"[sum&0x0f]}) + "*"
		if _, err := conn.Write(append([]byte(frame), '\r')); err != nil {
			t.Fatal(err)
		}
		resp, err := readOne(conn)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		parsed, err := hlcodec.ParseResponse(resp)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if len(parsed.Data) != 2 || parsed.Data[1] != 7 {
			t.Fatalf("round %d: data = % x", i, parsed.Data)
		}
	}
}

// TestClosedConnIsDisconnected 检查关闭后再用会得到可重试的断链分类，
// 而不是「协议错」——后者会被熔断，前者的重连才是对的处置。
func TestClosedConnIsDisconnected(t *testing.T) {
	srv := newSim(t, 0)
	c := open(t, srv, 0)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	span := driver.ReadSpan{ID: "s", Points: []driver.PointRef{{
		ID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Length: 1, Bit: -1}, DataType: model.TypeUint16,
	}}}
	_, err := c.Read(ctx(), []driver.ReadSpan{span})
	if err == nil {
		t.Fatal("read on a closed conn must fail")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %q (%v)", driver.KindOf(err), err)
	}
	if !driver.IsRetryable(err) {
		t.Fatal("a closed connection must be retryable")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err = %v", err)
	}
	// Close 必须幂等：调度器会在重连路径上重复调用。
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
