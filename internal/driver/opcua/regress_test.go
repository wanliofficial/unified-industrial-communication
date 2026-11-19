package opcua

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/driver"
	"uic/internal/model"
	opcuaserver "uic/internal/opcuaserver"
	"uic/internal/transport"
)

// 本文件是 OPC UA 驱动层（读写分块、错误分类、质量码、帧长）的回归测试。
//
// 钉住的核心契约（docs §3.3.1 / §3.4）：
//   - 逐项返回：每个写请求恰好一个结果，不重复、不消失；
//   - 链路级失败立即上抛：不被前一块的点级错误顶掉；
//   - 配置错（地址/权限/不支持）不熔断整台设备；
//   - 写路径的数值越界/带小数在本地拒绝，归 address。

// ---- 测试基础设施 -------------------------------------------------------

type failCounter struct {
	allow    int // arm 之后还允许成功的次数；<0 表示永不允许
	count    int
	arm      bool
	failRead bool
}

type scriptedStream struct {
	transport.Stream
	fc *failCounter
}

func (s *scriptedStream) Write(p []byte) error {
	if !s.fc.arm {
		return s.Stream.Write(p)
	}
	if s.fc.failRead {
		return s.Stream.Write(p)
	}
	if s.fc.count < s.fc.allow {
		s.fc.count++
		return s.Stream.Write(p)
	}
	return net.ErrClosed
}

func (s *scriptedStream) ReadFull(n int) ([]byte, error) {
	if !s.fc.arm {
		return s.Stream.ReadFull(n)
	}
	if !s.fc.failRead {
		return s.Stream.ReadFull(n)
	}
	if s.fc.count < s.fc.allow {
		s.fc.count++
		return s.Stream.ReadFull(n)
	}
	return nil, net.ErrClosed
}

func openWith(t *testing.T, srv *opcuaserver.Server, fc *failCounter) driver.Conn {
	t.Helper()
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		s, err := transport.DialTCP(ctx, addr, timeout)
		if err != nil {
			return nil, err
		}
		return &scriptedStream{Stream: s, fc: fc}, nil
	})
	ch := channel(srv.Addr())
	c, err := d.Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device:  model.Device{ID: "ua1", ChannelID: ch.ID, Interval: model.Duration(time.Second), Enabled: true},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func symNode(id string) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		DataType: model.TypeFloat64,
		Address:  model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Setpoint", Index: -1},
	}
}

func writeReqs(n int, path string) []driver.WriteRequest {
	reqs := make([]driver.WriteRequest, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, driver.WriteRequest{
			PointID: "w", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressSymbol, Path: path, Index: -1},
			Value:   model.NumValue(model.TypeFloat64, float64(i)),
		})
	}
	return reqs
}

// ---- 读路径 -------------------------------------------------------------

// Read 分块时后一块链路失败：链路错误必须上抛，且前一块已读到的好样本保留。
func TestReadLinkFailureReportsAndKeepsGoodSamples(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	fc := &failCounter{}
	c := openWith(t, srv, fc)

	pts := make([]driver.PointRef, 0, 250)
	for i := 0; i < 250; i++ {
		pts = append(pts, symNode("t"))
	}
	fc.arm = true
	fc.failRead = true
	fc.allow = 2 // 第 1 块（头 + 体）成功，之后读失败

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err == nil {
		t.Fatalf("link failure in a later chunk must be reported, got nil (samples=%d)", len(samples))
	}
	if k := driver.KindOf(err); k != driver.ErrDisconnected && k != driver.ErrTimeout {
		t.Fatalf("kind = %v, want link failure", k)
	}
	if len(samples) == 0 {
		t.Fatal("earlier good samples must be preserved")
	}
}

// 链路失败必须作为顶层错误上抛，不能被前一块的点级错误（BadNodeIdUnknown）
// 顶掉——否则 collector 拿到 address 就不会重连，链路永远不重建。
func TestReadLinkFailureNotMaskedByPointError(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	fc := &failCounter{}
	c := openWith(t, srv, fc)

	pts := make([]driver.PointRef, 0, 250)
	pts = append(pts, driver.PointRef{
		ID: "bad", DataType: model.TypeFloat64,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=No.Such.Node", Index: -1},
	})
	for i := 0; i < 249; i++ {
		pts = append(pts, symNode("g"))
	}

	fc.arm = true
	fc.failRead = true
	fc.allow = 2

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err == nil {
		t.Fatal("link failure must be surfaced")
	}
	if !driver.IsLinkFailure(err) {
		t.Fatalf("link failure must not be masked by a prior point error, got %v", err)
	}
	if len(samples) != len(pts) {
		t.Fatalf("earlier good samples must be preserved, got %d of %d", len(samples), len(pts))
	}
}

// BadNotReadable 是权限/配置问题：归 permission，不熔断。
func TestReadBadNotReadableIsPermissionNotBreaker(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Update(ua.StringNode(2, "Plant.Setpoint"), ua.Variant{Type: ua.TypeDouble, Float: 1},
		ua.StatusBadNotReadable, time.Now())
	c := openWith(t, srv, &failCounter{})

	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{symNode("sp")}}})
	if len(samples) != 1 {
		t.Fatalf("samples = %d", len(samples))
	}
	if k := driver.KindOf(samples[0].Err); k != driver.ErrPermission {
		t.Fatalf("BadNotReadable kind = %s, want permission", k)
	}
	if driver.CountsTowardBreaker(samples[0].Err) {
		t.Fatal("BadNotReadable must not count toward breaker")
	}
}

// BadTypeMismatch 是点位配置错：归 address，不熔断。
func TestReadBadTypeMismatchIsAddressNotBreaker(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Update(ua.StringNode(2, "Plant.Setpoint"), ua.Variant{Type: ua.TypeDouble, Float: 1},
		ua.StatusBadTypeMismatch, time.Now())
	c := openWith(t, srv, &failCounter{})

	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{symNode("sp")}}})
	if len(samples) != 1 {
		t.Fatalf("samples = %d", len(samples))
	}
	if k := driver.KindOf(samples[0].Err); k != driver.ErrAddress {
		t.Fatalf("BadTypeMismatch kind = %s, want address", k)
	}
	if driver.CountsTowardBreaker(samples[0].Err) {
		t.Fatal("BadTypeMismatch must not count toward breaker")
	}
}

// ---- 写路径 -------------------------------------------------------------

// 写分块时第一块成功、第二块失败：每个请求仍要有一个结果，后续块显式标坏，
// 不能从结果里消失（collector 会把缺失项兜底成「driver returned no result」，
// 丢掉真正的链路原因）。
func TestWritePartialFailureKeepsEveryResult(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	fc := &failCounter{}
	c := openWith(t, srv, fc)

	reqs := writeReqs(250, "ns=2;s=Plant.Setpoint")
	fc.arm = true
	fc.allow = 1

	results, err := c.Write(context.Background(), reqs)
	if len(results) != len(reqs) {
		t.Fatalf("results = %d, want %d (later chunks' entries disappeared)", len(results), len(reqs))
	}
	if !results[0].OK {
		t.Fatalf("first chunk should have succeeded: %+v", results[0])
	}
	if results[len(reqs)-1].OK {
		t.Fatalf("later chunks must be marked failed: %+v", results[len(reqs)-1])
	}
}

// 写路径的数值越界/带小数在本地拒绝，归 address（与 MC / FINS 的 checkRange
// 一致），不熔断。
func TestWriteOutOfRangeIsAddress(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := openWith(t, srv, &failCounter{})

	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeInt16,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Setpoint", Index: -1},
		Value:   model.NumValue(model.TypeFloat64, 1.5),
	}})
	if err != nil {
		t.Fatalf("per-item failure must not fail the whole call: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	if k := driver.KindOf(results[0].Err); k != driver.ErrAddress {
		t.Fatalf("out-of-range write kind = %s, want address", k)
	}
	if driver.CountsTowardBreaker(results[0].Err) {
		t.Fatal("out-of-range write must not count toward breaker")
	}
}

// 单帧超过协商缓冲区且本驱动不支持分块：本地 fail-closed 给出明确诊断，
// 而不是发出去让对端按规范断连。
func TestWriteOversizedFrameRejectedLocally(t *testing.T) {
	srv, err := opcuaserver.NewSimulator("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := openWith(t, srv, &failCounter{})

	big := strings.Repeat("A", 200000)
	results, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "sp", DataType: model.TypeString,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=Plant.Label", Index: -1},
		Value:   model.TextValue(model.TypeString, big),
	}})
	if err == nil {
		t.Fatal("oversized frame must be rejected locally")
	}
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("kind = %s, want protocol", driver.KindOf(err))
	}
	if driver.IsLinkFailure(err) {
		t.Fatal("oversized frame is not a link failure, must not trigger reconnect")
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("the point must be explicitly marked failed: %+v", results)
	}
}

// ---- 分类/质量码 --------------------------------------------------------

func TestQualityForUnsupportedIsNotDisabled(t *testing.T) {
	// ErrUnsupported 是服务端能力问题，不是「点位被关掉」：QualityDisabled 会被
	// 历史跳过、被运行时计入 Disabled，必须报 bad。
	q := qualityFor(driver.E(driver.ErrUnsupported, "read", "dev", "unsupported", false, nil))
	if q == model.QualityDisabled {
		t.Fatalf("ErrUnsupported must not map to QualityDisabled")
	}
}
