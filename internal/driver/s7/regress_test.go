package s7

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	s7codec "uic/internal/codec/s7"
	"uic/internal/driver"
	"uic/internal/model"
	s7sim "uic/internal/sim/s7"
	"uic/internal/transport"
)

// 本文件是 S7 驱动层的回归测试。钉住的核心契约（docs §3.3.1 / §3.4）：
//
//   - 逐项返回：每个写请求恰好一个结果，不重复、不消失；
//   - 链路级失败立即上抛：不被前一块的点级错误顶掉；
//   - 配置错（地址/权限/不支持/越界）不熔断整台设备；
//   - 发报文之前拦截越界/带小数/超长/超 PDU 的请求，绝不静默改写。

// ---- 链路注入 -------------------------------------------------------------

type failCounter struct {
	allow    int // arm 之后还允许成功的次数
	count    int
	arm      bool
	failRead bool
}

type scriptedStream struct {
	transport.Stream
	fc *failCounter
}

func (s *scriptedStream) Write(p []byte) error {
	if !s.fc.arm || s.fc.failRead {
		return s.Stream.Write(p)
	}
	if s.fc.count < s.fc.allow {
		s.fc.count++
		return s.Stream.Write(p)
	}
	return net.ErrClosed
}

func (s *scriptedStream) ReadFull(n int) ([]byte, error) {
	if !s.fc.arm || !s.fc.failRead {
		return s.Stream.ReadFull(n)
	}
	if s.fc.count < s.fc.allow {
		s.fc.count++
		return s.Stream.ReadFull(n)
	}
	return nil, net.ErrClosed
}

func openWith(t *testing.T, srv *s7sim.Server, fc *failCounter) driver.Conn {
	t.Helper()
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		s, err := transport.DialTCP(ctx, addr, timeout)
		if err != nil {
			return nil, err
		}
		return &scriptedStream{Stream: s, fc: fc}, nil
	})
	ch := model.Channel{
		ID: "ch-s7", Protocol: "s7",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "plc1", ChannelID: ch.ID, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---- codec 层：写响应截断（回归 ParseWriteAck） -----------------------------

// 写响应数据区被截断时，缺失项被静默当成功。
func TestWriteAckTruncatedIsBad(t *testing.T) {
	pdu := s7codec.PDU{ROSCTR: s7codec.ROSCTRAckData,
		Param: []byte{s7codec.FuncWrite, 3},
		Data:  []byte{s7codec.CodeOK}}
	errs, err := s7codec.ParseWriteAck(pdu, 3)
	if err == nil {
		for i, e := range errs {
			if e == nil {
				t.Fatalf("item %d missing from truncated write ack reported as success (errs=%v)", i, errs)
			}
		}
		return // 上抛错误也算正确
	}
	t.Logf("truncated ack rejected: %v", err)
}

// 完整写响应必须全部成功（严格解析不误杀）。
func TestWriteAckFullOK(t *testing.T) {
	pdu := s7codec.PDU{ROSCTR: s7codec.ROSCTRAckData,
		Param: []byte{s7codec.FuncWrite, 3},
		Data:  []byte{s7codec.CodeOK, s7codec.CodeOK, s7codec.CodeOK}}
	errs, err := s7codec.ParseWriteAck(pdu, 3)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("item %d: %v", i, e)
		}
	}
}

// ---- 读路径 ---------------------------------------------------------------

// 分块读时前块里有一个点级错误（对象不存在 → address），随后一块链路失败：
// 链路失败必须作为顶层错误上抛，不能被前一块的点级错误顶掉——否则 collector
// 拿到 address 就不会重连，链路永远不重建。
func TestReadLinkFailureNotMaskedByPointError(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	const n = 120
	for i := 0; i < n; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(i))
		srv.SetBytes(s7codec.AreaDB, 1, i*4, b[:])
	}

	fc := &failCounter{}
	c := openWith(t, srv, fc)

	pts := []driver.PointRef{pt("bad", "db", 99, 0, model.TypeUint16)}
	for i := 0; i < n; i++ {
		pts = append(pts, pt("p"+itoa(i), "db", 1, i*4, model.TypeFloat32))
	}

	// 先不 arm 跑一轮，确认块数与基线。
	base, berr := c.Read(context.Background(), []driver.ReadSpan{span("s", pts...)})
	if berr != nil || len(base) != len(pts) {
		t.Fatalf("baseline: samples=%d err=%v", len(base), berr)
	}
	t.Logf("baseline samples=%d requests=%d", len(base), srv.RequestCount())

	// 前 2 块完整读完（每块 2 次 ReadFull），第 3 块的头读失败。
	fc.arm = true
	fc.failRead = true
	fc.allow = 2 * 2

	samples, rerr := c.Read(context.Background(), []driver.ReadSpan{span("s", pts...)})
	t.Logf("after failure: samples=%d err=%v kind=%s", len(samples), rerr, driver.KindOf(rerr))
	if rerr == nil {
		t.Fatalf("later-chunk link failure must be reported, got nil (samples=%d)", len(samples))
	}
	if !driver.IsLinkFailure(rerr) {
		t.Fatalf("link failure masked by point error: %v (kind=%s)", rerr, driver.KindOf(rerr))
	}
	if len(samples) != len(pts) {
		t.Fatalf("earlier good samples must be preserved, got %d of %d", len(samples), len(pts))
	}
}

// 单个超大项超过协商 PDU：本地判坏（address_error），不发出任何请求。
func TestReadOversizedItemRejectedLocally(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Ensure(s7codec.AreaDB, 1, 1024)
	c := openWith(t, srv, &failCounter{})

	raws, err := c.Read(context.Background(), []driver.ReadSpan{span("s",
		driver.PointRef{ID: "big", DataType: model.TypeString, Length: 400,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1}},
	)})
	t.Logf("raws=%d err=%v", len(raws), err)
	if len(raws) != 1 {
		t.Fatalf("samples = %d, want 1", len(raws))
	}
	if raws[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %v, want address_error (%v)", raws[0].Quality, raws[0].Err)
	}
	if driver.CountsTowardBreaker(raws[0].Err) {
		t.Fatal("oversized item is a config error, must not trip breaker")
	}
	if n := srv.RequestCount(); n != 0 {
		t.Fatalf("requests = %d, want 0 (must be rejected locally)", n)
	}
}

// ---- 写路径 ---------------------------------------------------------------

// 写分块时第一块成功、第二块起断链：每个请求仍要有一个结果。
func TestWritePartialFailureKeepsEveryResult(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Ensure(s7codec.AreaDB, 1, 4096)

	fc := &failCounter{}
	c := openWith(t, srv, fc)

	reqs := make([]driver.WriteRequest, 0, 60)
	for i := 0; i < 60; i++ {
		reqs = append(reqs, driver.WriteRequest{
			PointID: "w" + itoa(i), DataType: model.TypeUint16,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: i * 2, Bit: -1},
			Value:   model.NumValue(model.TypeUint16, 1),
		})
	}

	// 第 1 块写完（写响应是 2 次 ReadFull），第 2 块的写发出去即断。
	fc.arm = true
	fc.allow = 1

	results, werr := c.Write(context.Background(), reqs)
	t.Logf("results=%d err=%v", len(results), werr)
	if len(results) != len(reqs) {
		t.Fatalf("results = %d, want %d (later chunks' entries disappeared)", len(results), len(reqs))
	}
	if !results[0].OK {
		t.Fatalf("first chunk should have succeeded: %+v", results[0])
	}
	if results[len(reqs)-1].OK {
		t.Fatalf("later chunks must be marked failed: %+v", results[len(reqs)-1])
	}
	// 失败条目必须带着**链路错误**（不是「未记录结果」的内部占位）：这样
	// 上层才能知道是链路断了，而不是驱动自己漏写。
	if k := driver.KindOf(results[len(reqs)-1].Err); k != driver.ErrDisconnected {
		t.Fatalf("later result kind = %v, want disconnected (link failure)", k)
	}
	// 前一块已成功的条目保持成功；其余显式标坏。
	if !results[0].OK {
		t.Fatalf("first chunk should have succeeded: %+v", results[0])
	}
	if results[len(reqs)-1].OK {
		t.Fatalf("later chunks must be marked failed: %+v", results[len(reqs)-1])
	}
}

// 字符串超长写入必须拒绝，不能静默截断。
func TestWriteStringTooLongIsRejected(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Ensure(s7codec.AreaDB, 1, 4096)
	c := openWith(t, srv, &failCounter{})

	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "s", DataType: model.TypeString,
		Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1, Length: 4},
		Value:   model.TextValue(model.TypeString, "ABCDEFGHIJ"),
	}})
	t.Logf("err=%v results=%+v", err, results)
	if err != nil {
		t.Fatalf("per-item failure must not fail the whole call: %v", err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("oversized string must fail: %+v", results)
	}
	if driver.KindOf(results[0].Err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(results[0].Err))
	}
}

// DB STRING 的 2 字节头语义：写 [max, cur, 字符...]，字符数按 length-2
// 校验；读回剥掉头。bytes 类型保持裸字节（逃生口，不含头）。
func TestStringHeaderRoundTrip(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Ensure(s7codec.AreaDB, 1, 4096)
	c := openWith(t, srv, &failCounter{})

	// 写 "LINE-2"（6 字符）→ length 8 = 2 头 + 6 字符。
	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "s", DataType: model.TypeString,
		Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1, Length: 8},
		Value:   model.TextValue(model.TypeString, "LINE-2"),
	}})
	if err != nil || len(results) != 1 || !results[0].OK {
		t.Fatalf("write: %v %+v", err, results)
	}
	// 落到 sim 的字节必须是 [max=6, cur=6, LINE-2, 0]：用 bytes 类型
	// 裸读回验证（bytes 不剥头）。
	raws, err := c.Read(context.Background(), []driver.ReadSpan{{
		ID: "db1", Points: []driver.PointRef{{
			ID: "sraw", DataType: model.TypeBytes,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1}, Length: 8,
		}},
	}})
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if got := raws[0].Value.Text; got != "\x06\x06LINE-2" {
		t.Fatalf("raw = % X, want 06 06 'LINE-2'（length 8 恰好填满，无填充位）", got)
	}
	// 读回剥头：文本恰为 LINE-2，尾部填充不进文本。
	raws, err = c.Read(context.Background(), []driver.ReadSpan{{
		ID: "db1", Points: []driver.PointRef{{
			ID: "s", DataType: model.TypeString,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1}, Length: 8,
		}},
	}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if raws[0].Value.Text != "LINE-2" {
		t.Fatalf("text = %q, want LINE-2", raws[0].Value.Text)
	}

	// 裸字节（bytes 类型）不带头：原样写原样读。
	results, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "b", DataType: model.TypeBytes,
		Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 20, Bit: -1, Length: 4},
		Value:   model.TextValue(model.TypeBytes, "ABCD"),
	}})
	if err != nil || len(results) != 1 || !results[0].OK {
		t.Fatalf("bytes write: %v %+v", err, results)
	}
	raws, err = c.Read(context.Background(), []driver.ReadSpan{{
		ID: "db1", Points: []driver.PointRef{{
			ID: "b", DataType: model.TypeBytes,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 20, Bit: -1}, Length: 4,
		}},
	}})
	if err != nil {
		t.Fatalf("bytes read: %v", err)
	}
	if raws[0].Value.Text != "ABCD" {
		t.Fatalf("bytes text = %q, want ABCD（裸字节不带头）", raws[0].Value.Text)
	}
}

// 字符数超出 length-2 在写报文前拒绝（头占 2 字节）；length<2 直接拒绝。
func TestWriteStringHeaderLimits(t *testing.T) {
	cases := []struct {
		length int
		text   string
		wantOK bool
	}{
		{8, "LINE-2", true},   // 2 头 + 6 字符，恰好填满
		{8, "LINE-23", false}, // 7 字符 > 6
		{2, "", true},         // 空 STRING：max=0
		{1, "", false},        // 头都不够
	}
	for _, tc := range cases {
		srv, err := s7sim.NewServer("127.0.0.1:0", 480)
		if err != nil {
			t.Fatal(err)
		}
		srv.Ensure(s7codec.AreaDB, 1, 4096)
		c := openWith(t, srv, &failCounter{})
		results, err := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: "s", DataType: model.TypeString,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1, Length: tc.length},
			Value:   model.TextValue(model.TypeString, tc.text),
		}})
		ok := err == nil && len(results) == 1 && results[0].OK
		if ok != tc.wantOK {
			t.Fatalf("length=%d text=%q: ok=%v (%v), want %v", tc.length, tc.text, ok, err, tc.wantOK)
		}
		srv.Close()
	}
}

// 数值越界写入必须拒绝（70000 写 int16 不能静默回绕成 4464）。
func TestWriteOutOfRangeIsAddress(t *testing.T) {
	cases := []struct {
		dt  model.DataType
		val float64
	}{
		{model.TypeInt16, 70000},
		{model.TypeInt16, -40000},
		{model.TypeUint16, 70000},
		{model.TypeUint16, -1},
		{model.TypeInt32, 3e9},
		{model.TypeUint32, -1},
		{model.TypeFloat32, 1e300},
		{model.TypeFloat32, 1e-300},
		{model.TypeFloat64, math.Inf(1)},
		{model.TypeInt16, 1.5}, // 带小数
	}
	for _, tc := range cases {
		req := driver.WriteRequest{
			PointID: "n", DataType: tc.dt,
			Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1},
			Value:   model.NumValue(tc.dt, tc.val),
		}
		_, raw, err := encodeWrite(req)
		t.Logf("%s %v -> raw=%x err=%v", tc.dt, tc.val, raw, err)
		if err == nil {
			t.Fatalf("%s %v accepted: raw=%x", tc.dt, tc.val, raw)
		}
		if driver.KindOf(err) != driver.ErrAddress {
			t.Fatalf("%s %v kind = %v, want address", tc.dt, tc.val, driver.KindOf(err))
		}
	}
}

// 单个超大写请求超过协商 PDU：本地判坏，不发出。
func TestWriteOversizedItemRejectedLocally(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Ensure(s7codec.AreaDB, 1, 1024)
	c := openWith(t, srv, &failCounter{})

	big := strings.Repeat("X", 400)
	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "big", DataType: model.TypeString,
		Address: model.Address{Kind: model.AddressRegister, Area: "db", DB: 1, Offset: 0, Bit: -1, Length: 400},
		Value:   model.TextValue(model.TypeString, big),
	}})
	t.Logf("results=%+v err=%v", results, err)
	if len(results) != 1 || results[0].OK {
		t.Fatalf("oversized write must fail: %+v", results)
	}
	if driver.KindOf(results[0].Err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(results[0].Err))
	}
	if n := srv.RequestCount(); n != 0 {
		t.Fatalf("requests = %d, want 0 (must be rejected locally)", n)
	}
}

// fitsPDU 的预算公式：单项独占一帧时的需求字节数必须与实际成帧一致。
func TestFitsPDBudget(t *testing.T) {
	// 读：400 字节项在 240 的 PDU 下装不下，480 装得下。
	big := s7codec.Item{Area: s7codec.AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 400}
	if ok, need := fitsPDU(big, 240, false); ok || need <= 240 {
		t.Fatalf("read item should not fit in 240: ok=%v need=%d", ok, need)
	}
	if ok, _ := fitsPDU(big, 480, false); !ok {
		t.Fatal("read item should fit in 480")
	}
	if ok, _ := fitsPDU(big, 480, true); !ok {
		t.Fatal("write item should fit in 480")
	}
	// 位访问：宽度按 1 字节计。
	bit := s7codec.Item{Area: s7codec.AreaMerker, DB: 1, ByteAddr: 0, Bit: 3, BitAccess: true, Bytes: 1}
	if ok, _ := fitsPDU(bit, 64, false); !ok {
		t.Fatal("bit access should fit in a 64-byte PDU")
	}
}

// decodeSample 的解码失败归 ErrAddress（点位类型/宽度配错），不归 ErrProtocol
// ——否则一个配错的点会在 5 个周期后把整台设备拖进熔断，而其余点其实都正常。
func TestDecodeShortPayloadIsAddressNotBreaker(t *testing.T) {
	p := driver.PointRef{ID: "x", DataType: model.TypeFloat32}
	it := s7codec.Item{Area: s7codec.AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 4}
	s := decodeSample("dev", p, it, []byte{0x01, 0x02}) // 2 字节，float32 要 4
	if s.Quality == model.QualityGood {
		t.Fatalf("short decode reported good: %+v", s)
	}
	if k := driver.KindOf(s.Err); k != driver.ErrAddress {
		t.Fatalf("decode failure kind = %v, want address", k)
	}
	if driver.CountsTowardBreaker(s.Err) {
		t.Fatal("decode failure must not trip the breaker")
	}
}

// 正常值必须解出来（分类改动不能误杀）。
func TestDecodeSampleSucceeds(t *testing.T) {
	p := driver.PointRef{ID: "x", DataType: model.TypeUint16}
	it := s7codec.Item{Area: s7codec.AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 2}
	s := decodeSample("dev", p, it, []byte{0x12, 0x34})
	if s.Quality != model.QualityGood {
		t.Fatalf("good decode reported bad: %+v", s)
	}
}
