package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	adscodec "uic/internal/codec/ads"
	"uic/internal/driver"
	"uic/internal/model"
)

// 本文件补两类主站测试：
//
//  1. 纯函数（checkRange / encodeValue / decodeValue / valueLen / deadline）——
//     它们不依赖网络，直接用表驱动覆盖边界；
//  2. 真实设备不会配合的路径（临时性 AMS 错误、迟到应答、通知帧、畸形帧）——
//     用一个脚本化 TCP 服务端按帧回放，这正是回环仿真做不到的部分。

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func TestParseOptionsFull(t *testing.T) {
	dev := model.Device{
		ID: "plc",
		Labels: map[string]string{
			"netid":      "10.20.30.40.1.1",
			"port":       "801",
			"localnetid": "10.20.30.1.1.1",
			"localport":  "33000",
			"options":    " RETRIES = 3 , strlen=200 , ",
		},
	}
	opt, err := parseOptions(dev)
	if err != nil {
		t.Fatal(err)
	}
	if opt.Target.NetID.String() != "10.20.30.40.1.1" || opt.Target.Port != 801 {
		t.Fatalf("target = %v", opt.Target)
	}
	if opt.Source.NetID.String() != "10.20.30.1.1.1" || opt.Source.Port != 33000 {
		t.Fatalf("source = %v", opt.Source)
	}
	if opt.Retries != 3 || opt.StringLen != 200 {
		t.Fatalf("retries=%d strlen=%d", opt.Retries, opt.StringLen)
	}

	// 默认值：只给 netid 时，端口与本地端点都是默认。
	opt, err = parseOptions(model.Device{ID: "plc", Labels: map[string]string{"netid": "1.2.3.4.5.6"}})
	if err != nil {
		t.Fatal(err)
	}
	if opt.Target.Port != defaultAMSPort || opt.Source.Port != defaultLocalPort ||
		opt.Source.NetID != defaultLocalNetID || opt.Retries != defaultRetries ||
		opt.StringLen != defaultStrLen {
		t.Fatalf("defaults = %+v", opt)
	}

	// labels 整体为 nil：必须明确说 netid 必填，而不是空指针。
	if _, err := parseOptions(model.Device{ID: "plc"}); err == nil ||
		!strings.Contains(err.Error(), "labels.netid") {
		t.Fatalf("nil labels: %v", err)
	}
	if _, err := parseOptions(model.Device{ID: "plc", Labels: map[string]string{
		"netid": "1.2.3.4.5.6", "options": "strlen",
	}}); err == nil || !strings.Contains(err.Error(), "strlen") {
		t.Fatalf("strlen without value: %v", err)
	}
	if _, err := parseOptions(model.Device{ID: "plc", Labels: map[string]string{
		"netid": "1.2.3.4.5.6", "options": "strlen=99999999",
	}}); err == nil || !strings.Contains(err.Error(), "1..") {
		t.Fatalf("strlen too large: %v", err)
	}
}

func TestCheckRange(t *testing.T) {
	ok := []struct {
		dt model.DataType
		v  float64
	}{
		{model.TypeInt16, math.MinInt16}, {model.TypeInt16, math.MaxInt16}, {model.TypeInt16, 0},
		{model.TypeUint16, 0}, {model.TypeUint16, math.MaxUint16},
		{model.TypeInt32, math.MinInt32}, {model.TypeInt32, math.MaxInt32},
		{model.TypeUint32, 0}, {model.TypeUint32, math.MaxUint32},
		{model.TypeInt64, math.MinInt64}, {model.TypeInt64, 0},
		{model.TypeUint64, 0},
		{model.TypeFloat32, 1.5}, {model.TypeFloat64, -1e300},
		{model.TypeBool, 0}, {model.TypeString, 0},
	}
	for _, c := range ok {
		if err := checkRange(c.dt, c.v); err != nil {
			t.Fatalf("%s(%v): %v", c.dt, c.v, err)
		}
	}
	bad := []struct {
		dt model.DataType
		v  float64
	}{
		{model.TypeInt16, math.MaxInt16 + 1}, {model.TypeInt16, math.MinInt16 - 1}, {model.TypeInt16, 0.5},
		{model.TypeUint16, -1}, {model.TypeUint16, math.MaxUint16 + 1}, {model.TypeUint16, 2.5},
		{model.TypeInt32, math.MaxInt32 + 1}, {model.TypeInt32, math.MinInt32 - 1}, {model.TypeInt32, 0.1},
		{model.TypeUint32, -1}, {model.TypeUint32, math.MaxUint32 + 1},
		{model.TypeInt64, 9223372036854775808.0}, {model.TypeInt64, -1e300},
		{model.TypeUint64, -1}, {model.TypeUint64, 18446744073709551616.0},
	}
	for _, c := range bad {
		if err := checkRange(c.dt, c.v); err == nil {
			t.Fatalf("%s(%v) accepted", c.dt, c.v)
		}
	}
}

// bareConn 是不带网络的连接状态，只为调用值编解码。
func bareConn(t *testing.T) *connState {
	t.Helper()
	c := &connState{
		cfg:     driver.OpenConfig{Device: model.Device{ID: "plc"}},
		strlen:  81,
		handles: map[string]uint32{},
	}
	return c
}

func TestEncodeValueTypes(t *testing.T) {
	c := bareConn(t)
	intAddr := model.Address{Kind: model.AddressSymbol, Path: "MAIN.x", Index: -1}
	cases := []struct {
		dt    model.DataType
		value model.Value
		want  []byte
	}{
		{model.TypeBool, model.BoolValue(true), []byte{1}},
		{model.TypeBool, model.BoolValue(false), []byte{0}},
		{model.TypeBool, model.NumValue(model.TypeBool, 1), []byte{1}},
		{model.TypeInt16, model.NumValue(model.TypeInt16, -321), []byte{0xBF, 0xFE}},
		{model.TypeUint16, model.NumValue(model.TypeUint16, 0x1234), []byte{0x34, 0x12}},
		{model.TypeInt32, model.NumValue(model.TypeInt32, -2), []byte{0xFE, 0xFF, 0xFF, 0xFF}},
		{model.TypeUint32, model.NumValue(model.TypeUint32, 0x01020304), []byte{0x04, 0x03, 0x02, 0x01}},
		{model.TypeInt64, model.NumValue(model.TypeInt64, 1), []byte{1, 0, 0, 0, 0, 0, 0, 0}},
		{model.TypeUint64, model.NumValue(model.TypeUint64, 1), []byte{1, 0, 0, 0, 0, 0, 0, 0}},
		{model.TypeFloat32, model.NumValue(model.TypeFloat32, 23.5), func() []byte {
			b := make([]byte, 4)
			binary.LittleEndian.PutUint32(b, math.Float32bits(23.5))
			return b
		}()},
		{model.TypeFloat64, model.NumValue(model.TypeFloat64, 1450.75), func() []byte {
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, math.Float64bits(1450.75))
			return b
		}()},
	}
	for _, tc := range cases {
		raw, err := c.encodeValue(driver.WriteRequest{
			PointID: "p", Address: intAddr, DataType: tc.dt, Value: tc.value,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.dt, err)
		}
		if !bytes.Equal(raw, tc.want) {
			t.Fatalf("%s(%v) = % x, want % x", tc.dt, tc.value.Num, raw, tc.want)
		}
		got, err := decodeValue(raw, tc.dt)
		if err != nil {
			t.Fatalf("%s decode: %v", tc.dt, err)
		}
		if tc.dt == model.TypeBool {
			if got.Bool != tc.value.Bool && tc.value.Num == 0 {
				t.Fatalf("bool round trip = %+v", got)
			}
			continue
		}
		if got.Num != tc.value.Num {
			t.Fatalf("%s round trip = %v, want %v", tc.dt, got.Num, tc.value.Num)
		}
	}
}

func TestStringAndBytesEncoding(t *testing.T) {
	c := bareConn(t)
	addr := model.Address{Kind: model.AddressSymbol, Path: "MAIN.s", Index: -1, Length: 10}

	raw, err := c.encodeValue(driver.WriteRequest{
		PointID: "p", Address: addr, DataType: model.TypeString,
		Value: model.TextValue(model.TypeString, "abc"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 10 || string(raw[:3]) != "abc" || raw[3] != 0 {
		t.Fatalf("string = % x", raw)
	}
	// 值短于宽度：尾部全部补 NUL，而不是只补一个。
	for _, b := range raw[3:] {
		if b != 0 {
			t.Fatalf("string padding = % x", raw)
		}
	}

	// bytes 允许刚好占满，不需要留 NUL。
	raw, err = c.encodeValue(driver.WriteRequest{
		PointID: "p", Address: addr, DataType: model.TypeBytes,
		Value: model.BytesValue([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("bytes = % x", raw)
	}

	// string 刚好占满：没有结尾 NUL，必须拒绝（真实 PLC 会把它与内存里
	// 下一个字节拼起来）。
	if _, err := c.encodeValue(driver.WriteRequest{
		PointID: "p", Address: addr, DataType: model.TypeString,
		Value: model.TextValue(model.TypeString, "0123456789"),
	}); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("exact-length string: %v", err)
	}

	// 地址省略长度时用 options.strlen。
	noLen := addr
	noLen.Length = 0
	raw, err = c.encodeValue(driver.WriteRequest{
		PointID: "p", Address: noLen, DataType: model.TypeString,
		Value: model.TextValue(model.TypeString, "x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 81 {
		t.Fatalf("default string length = %d", len(raw))
	}
}

func TestDecodeValueErrors(t *testing.T) {
	if _, err := decodeValue(nil, model.TypeBool); err == nil {
		t.Fatal("empty bool accepted")
	}
	if _, err := decodeValue([]byte{2}, model.TypeBool); err == nil ||
		!strings.Contains(err.Error(), "neither 0 nor 1") {
		t.Fatalf("bool 2: %v", err)
	}
	if _, err := decodeValue([]byte{1, 2}, model.TypeBCD16); err == nil {
		t.Fatal("BCD accepted")
	}
	if v, err := decodeValue([]byte("a\x00\x00"), model.TypeString); err != nil || v.Text != "a" {
		t.Fatalf("string = %+v %v", v, err)
	}
	if v, err := decodeValue([]byte{9, 8}, model.TypeBytes); err != nil || v.Text != string([]byte{9, 8}) {
		t.Fatalf("bytes = %+v %v", v, err)
	}
	// 数值宽度不足由 codec 层报错，不能静默截断。
	if _, err := decodeValue([]byte{1}, model.TypeInt32); err == nil {
		t.Fatal("short int32 accepted")
	}
}

func TestValueLen(t *testing.T) {
	c := bareConn(t)
	addr := model.Address{Kind: model.AddressSymbol, Path: "MAIN.x", Index: -1}
	if n, err := c.valueLen(addr, model.TypeBool); err != nil || n != 1 {
		t.Fatalf("bool = %d %v", n, err)
	}
	if n, err := c.valueLen(addr, model.TypeFloat64); err != nil || n != 8 {
		t.Fatalf("float64 = %d %v", n, err)
	}
	if n, err := c.valueLen(addr, model.TypeString); err != nil || n != 81 {
		t.Fatalf("string default = %d %v", n, err)
	}
	addr.Length = 4
	if n, err := c.valueLen(addr, model.TypeBytes); err != nil || n != 4 {
		t.Fatalf("bytes length = %d %v", n, err)
	}
	addr.Length = 0
	if _, err := c.valueLen(addr, model.TypeString); err != nil {
		t.Fatal(err)
	}
	c.strlen = 0
	if _, err := c.valueLen(addr, model.TypeString); err == nil ||
		!strings.Contains(err.Error(), "没有确定的字节宽度") {
		t.Fatalf("zero strlen: %v", err)
	}
	addr.Length = maxValueLen + 1
	if _, err := c.valueLen(addr, model.TypeBytes); err == nil ||
		!strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("oversized length: %v", err)
	}
	if _, err := c.valueLen(addr, model.TypeBCD32); err == nil ||
		!strings.Contains(err.Error(), "BCD") {
		t.Fatalf("BCD: %v", err)
	}
}

func TestNextInvokeAndDeadline(t *testing.T) {
	c := bareConn(t)
	c.invoke = math.MaxUint32
	// 0 被保留为「未分配」，回绕后必须跳到 1。
	if got := c.nextInvoke(); got != 1 {
		t.Fatalf("wrap = %d", got)
	}
	if got := c.nextInvoke(); got != 2 {
		t.Fatalf("after wrap = %d", got)
	}

	c.timeout = time.Second
	far := time.Now().Add(time.Hour)
	near := time.Now().Add(10 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), near)
	defer cancel()
	if got := c.deadline(ctx); !got.Equal(near) {
		t.Fatalf("deadline = %v, want ctx deadline %v", got, near)
	}
	if got := c.deadline(context.Background()); got.After(far) {
		t.Fatalf("deadline without ctx = %v", got)
	}
}

// ---------------------------------------------------------------------------
// 畸形帧
// ---------------------------------------------------------------------------

func TestReadFrameBadInput(t *testing.T) {
	cases := map[string][]byte{
		"reserved non-zero": {1, 0, 32, 0, 0, 0},
		"ams length zero":   {0, 0, 0, 0, 0, 0},
		"ams length huge":   {0, 0, 0xFF, 0xFF, 0xFF, 0x7F},
		"truncated body":    {0, 0, 40, 0, 0, 0, 1, 2, 3},
		"bad payload len": func() []byte {
			raw, _ := (adscodec.Peer{}).ReadDeviceInfo(1).Encode()
			binary.LittleEndian.PutUint32(raw[6+20:6+24], 9)
			return raw
		}(),
	}
	for name, raw := range cases {
		_, err := readFrame(bytes.NewReader(raw))
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if !errors.Is(err, errBadFrame) {
			t.Fatalf("%s: err = %v, want errBadFrame", name, err)
		}
	}
	// 头都没读满时是普通 I/O 错误，不是「坏帧」：连接被对端关闭与协议
	// 错误必须分开。
	_, err := readFrame(bytes.NewReader([]byte{0, 0}))
	if err == nil || errors.Is(err, errBadFrame) {
		t.Fatalf("short header: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 脚本化服务端
// ---------------------------------------------------------------------------

// scriptedServer 按 handler 返回的原始帧依次写回。返回 nil 表示只写这些
// 帧、不关闭连接。
type scriptedServer struct {
	ln      net.Listener
	mu      sync.Mutex
	conns   []net.Conn
	handler func(req adscodec.Frame) [][]byte
}

func startScripted(t *testing.T, h func(req adscodec.Frame) [][]byte) *scriptedServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &scriptedServer{ln: ln, handler: h}
	go s.accept()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *scriptedServer) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		go s.serve(nc)
	}
}

func (s *scriptedServer) serve(nc net.Conn) {
	defer nc.Close()
	for {
		f, err := readFrame(nc)
		if err != nil {
			return
		}
		for _, raw := range s.handler(f) {
			if _, err := nc.Write(raw); err != nil {
				return
			}
		}
	}
}

func (s *scriptedServer) Close() error {
	_ = s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, nc := range s.conns {
		_ = nc.Close()
	}
	return nil
}

func (s *scriptedServer) config(t *testing.T) driver.OpenConfig {
	t.Helper()
	return driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "ads",
			Transport: model.Transport{Kind: "tcp", Address: s.ln.Addr().String()},
			Timeout:   model.Duration(2 * time.Second),
		},
		Device: model.Device{
			ID: "plc", ChannelID: "ch",
			Labels: map[string]string{"netid": "127.0.0.1.1.1", "options": "retries=1"},
		},
	}
}

func reply(req adscodec.Frame, data []byte) adscodec.Frame {
	return adscodec.Frame{
		Target:  req.Source,
		Source:  req.Target,
		Command: req.Command,
		Flags:   adscodec.FlagADS | adscodec.FlagResponse,
		Invoke:  req.Invoke,
		Data:    data,
	}
}

// encodeFrames 在 handler goroutine 里调用，不能碰 *testing.T（FailNow
// 只能从测试 goroutine 调用）。这里的帧都由测试构造、必然合法，编码失败
// 只可能是代码 bug，直接 panic 让测试立即失败。
func encodeFrames(frames ...adscodec.Frame) [][]byte {
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		raw, err := f.Encode()
		if err != nil {
			panic(err)
		}
		out = append(out, raw)
	}
	return out
}

func okDeviceInfo(req adscodec.Frame) adscodec.Frame {
	data := make([]byte, 24)
	copy(data[8:], "scripted")
	return reply(req, data)
}

func TestOpenRetriesTemporaryAMSError(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	s := startScripted(t, func(req adscodec.Frame) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			bad := reply(req, nil)
			bad.Error = adscodec.CodeMailboxFull
			return encodeFrames(bad)
		}
		return encodeFrames(okDeviceInfo(req))
	})
	c, err := New().Open(context.Background(), s.config(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestExchangeSkipsNoise(t *testing.T) {
	var mu sync.Mutex
	var calls int
	s := startScripted(t, func(req adscodec.Frame) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls != 1 {
			return encodeFrames(okDeviceInfo(req))
		}
		// 第一次：先来一条通知帧、再来一条 invoke 不匹配的迟到应答，
		// 最后才是真正的应答。三者都在同一批写出，驱动必须逐帧丢弃。
		notify := reply(req, nil)
		notify.Command = adscodec.CmdDeviceNotification
		notify.Flags = adscodec.FlagADS // 通知不是「应答」
		stale := okDeviceInfo(req)
		stale.Invoke++
		return encodeFrames(notify, stale, okDeviceInfo(req))
	})
	c, err := New().Open(context.Background(), s.config(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestExchangeMalformedFrame(t *testing.T) {
	s := startScripted(t, func(req adscodec.Frame) [][]byte {
		// AMS 头声称载荷 100 字节，TCP 长度却只覆盖头本身。
		raw, err := okDeviceInfo(req).Encode()
		if err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint32(raw[6+20:6+24], 100)
		return [][]byte{raw}
	})
	_, err := New().Open(context.Background(), s.config(t))
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("open = %v", err)
	}
	if !strings.Contains(err.Error(), "malformed AMS frame") {
		t.Fatalf("message = %v", err)
	}
}

func TestHandleZeroAndShort(t *testing.T) {
	makeServer := func(handleData []byte) *scriptedServer {
		return startScripted(t, func(req adscodec.Frame) [][]byte {
			if req.Command == adscodec.CmdReadDeviceInfo {
				return encodeFrames(okDeviceInfo(req))
			}
			data := make([]byte, 8+len(handleData))
			binary.LittleEndian.PutUint32(data[4:8], uint32(len(handleData)))
			copy(data[8:], handleData)
			return encodeFrames(reply(req, data))
		})
	}

	s := makeServer([]byte{0, 0, 0, 0})
	c, err := New().Open(context.Background(), s.config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, rerr := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
	}}})
	if rerr != nil {
		t.Fatalf("handle-0 read: %v", rerr)
	}
	if out[0].Err == nil || driver.KindOf(out[0].Err) != driver.ErrAddress ||
		!strings.Contains(out[0].Err.Error(), "handle 0") {
		t.Fatalf("handle-0 sample = %+v", out[0])
	}

	s2 := makeServer([]byte{1, 2})
	c2, err := New().Open(context.Background(), s2.config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	out, _ = c2.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
	}}})
	if out[0].Err == nil || driver.KindOf(out[0].Err) != driver.ErrProtocol {
		t.Fatalf("short handle sample = %+v", out[0])
	}
}

// ---------------------------------------------------------------------------
// 写入路径
// ---------------------------------------------------------------------------

func TestWriteStaleHandleReResolves(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	req := driver.WriteRequest{
		PointID: "setpoint", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 42),
	}
	if res, err := c.Write(context.Background(), []driver.WriteRequest{req}); err != nil || !res[0].OK {
		t.Fatalf("first write: %v %+v", err, res)
	}
	srv.InvalidateHandles()
	if res, err := c.Write(context.Background(), []driver.WriteRequest{req}); err != nil || !res[0].OK {
		t.Fatalf("write after invalidate: %v %+v", err, res)
	}
	if raw, _ := srv.Get("MAIN.nSetpoint"); binary.LittleEndian.Uint16(raw) != 42 {
		t.Fatalf("value = % x", raw)
	}
}

func TestWriteLinkFailureStopsRound(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	_ = srv.Close()
	req := driver.WriteRequest{
		PointID: "p", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1),
	}
	res, err := c.Write(context.Background(), []driver.WriteRequest{req, req})
	if err == nil {
		t.Fatal("write after close succeeded")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
	}
	if len(res) != 2 || res[0].Err == nil || res[1].Err == nil {
		t.Fatalf("results = %+v", res)
	}
}

func TestWriteAddressErrorIsPerPoint(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	// 寄存器地址不是 ADS 的地址形态：逐点报错，连接仍然可用。
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{
			PointID: "reg", Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 1},
			DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1),
		},
		{
			PointID: "ok", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
			DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 3),
		},
	})
	if err != nil {
		t.Fatalf("round error: %v", err)
	}
	if res[0].Err == nil || driver.KindOf(res[0].Err) != driver.ErrAddress {
		t.Fatalf("register result = %+v", res[0])
	}
	if !res[1].OK {
		t.Fatalf("second result = %+v", res[1])
	}
}

func TestReadContextCancelled(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
		sym("MAIN.nSetpoint", model.TypeInt16),
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
	if len(out) != 2 || out[0].Err == nil || out[1].Err == nil {
		t.Fatalf("samples = %+v", out)
	}
}

func TestReadFrameUnsupportedDataType(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	// 未注册的 dataType 没有字节宽度：必须在发报文之前拒绝。
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "weird", Address: sym("MAIN.nCounter", model.TypeInt16).Address, DataType: model.DataType("quad")},
	}}})
	if err == nil || driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
	if out[0].Err == nil || !strings.Contains(out[0].Err.Error(), "没有确定的字节宽度") {
		t.Fatalf("sample = %+v", out[0])
	}
}

// 句柄缓存按变量名索引：两个点位指向同一变量时只解析一次。
func TestHandleCacheSharedByName(t *testing.T) {
	srv := startSim(t)
	c := openTest(t, srv)
	refs := []driver.PointRef{
		sym("MAIN.nCounter", model.TypeInt16),
		sym("MAIN.nCounter", model.TypeInt16),
	}
	if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}}); err != nil {
		t.Fatal(err)
	}
	before := srv.Requests()
	if len(c.handles) != 1 {
		t.Fatalf("handles = %v", c.handles)
	}
	if _, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}}); err != nil {
		t.Fatal(err)
	}
	// 第二轮只应有 2 次 Read，没有 HandleByName。
	if got := srv.Requests() - before; got != 2 {
		t.Fatalf("second round requests = %d, want 2", got)
	}
}

func TestDeleteOfUnknownOptionError(t *testing.T) {
	// options 里只允许 retries/strlen；拼错时必须报错而不是静默忽略。
	_, err := parseOptions(model.Device{ID: "p", Labels: map[string]string{
		"netid": "1.2.3.4.5.6", "options": "retry=1",
	}})
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// 协议错误分支
// ---------------------------------------------------------------------------

// branchServer 按当前 mode 对指定命令返回 AMS 错误 / 短载荷 / 业务错误码，
// 用来覆盖「真实设备出错时」的分支。Open 期间 mode=0（正常）。
type branchServer struct {
	*scriptedServer
	mu   sync.Mutex
	mode int
}

func newBranchServer(t *testing.T) *branchServer {
	b := &branchServer{}
	b.scriptedServer = startScripted(t, func(req adscodec.Frame) [][]byte {
		b.mu.Lock()
		mode := b.mode
		b.mu.Unlock()
		switch mode {
		case 1: // AMS 层错误
			bad := reply(req, nil)
			bad.Error = adscodec.CodeInternalError
			return encodeFrames(bad)
		case 2: // 只有 result，字段缺失
			return encodeFrames(reply(req, []byte{0, 0, 0, 0}))
		case 3: // result 非 0
			return encodeFrames(reply(req, []byte{0x05, 0x07, 0, 0}))
		case 4: // 只回 2 字节，连 result 都不完整
			return encodeFrames(reply(req, []byte{0, 0}))
		case 5: // 按名称取句柄：回一个合法句柄
			data := make([]byte, 12)
			binary.LittleEndian.PutUint32(data[4:8], 4)
			binary.LittleEndian.PutUint32(data[8:12], 1)
			return encodeFrames(reply(req, data))
		}
		switch req.Command {
		case adscodec.CmdReadDeviceInfo:
			return encodeFrames(okDeviceInfo(req))
		case adscodec.CmdReadState:
			data := make([]byte, 8)
			binary.LittleEndian.PutUint16(data[4:6], uint16(adscodec.StateRun))
			return encodeFrames(reply(req, data))
		case adscodec.CmdReadWrite:
			data := make([]byte, 12)
			binary.LittleEndian.PutUint32(data[4:8], 4)
			binary.LittleEndian.PutUint32(data[8:12], 1)
			return encodeFrames(reply(req, data))
		case adscodec.CmdRead:
			data := make([]byte, 10)
			binary.LittleEndian.PutUint32(data[4:8], 2)
			return encodeFrames(reply(req, data))
		case adscodec.CmdWrite, adscodec.CmdWriteControl:
			return encodeFrames(reply(req, []byte{0, 0, 0, 0}))
		}
		return encodeFrames(reply(req, nil))
	})
	return b
}

func (b *branchServer) set(mode int) {
	b.mu.Lock()
	b.mode = mode
	b.mu.Unlock()
}

func TestDeviceInfoAndStateErrors(t *testing.T) {
	b := newBranchServer(t)
	conn, err := New().Open(context.Background(), b.config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := conn.(*connState)
	ctx := context.Background()

	b.set(1)
	if _, err := c.DeviceInfo(ctx); err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("info AMS error: %v", err)
	}
	if err := c.Probe(ctx); err == nil {
		t.Fatal("probe accepted AMS error")
	}
	b.set(2)
	if _, err := c.DeviceInfo(ctx); err == nil || driver.KindOf(err) != driver.ErrProtocol ||
		!strings.Contains(err.Error(), "24") {
		t.Fatalf("info short: %v", err)
	}
	b.set(3)
	if _, err := c.DeviceInfo(ctx); err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("info result: %v", err)
	}

	b.set(1)
	if _, err := c.DeviceState(ctx); err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("state AMS error: %v", err)
	}
	b.set(2)
	if _, err := c.DeviceState(ctx); err == nil || !strings.Contains(err.Error(), "state fields") {
		t.Fatalf("state short: %v", err)
	}
	b.set(3)
	if _, err := c.DeviceState(ctx); err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("state result: %v", err)
	}

	b.set(1)
	if err := c.WriteControl(ctx, adscodec.StateRun); err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("control AMS error: %v", err)
	}
	b.set(4)
	if err := c.WriteControl(ctx, adscodec.StateRun); err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("control short: %v", err)
	}
	b.set(3)
	if err := c.WriteControl(ctx, adscodec.StateRun); err == nil || driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("control result: %v", err)
	}
}

func TestReadAndWriteProtocolErrors(t *testing.T) {
	b := newBranchServer(t)
	conn, err := New().Open(context.Background(), b.config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := conn.(*connState)

	ref := sym("MAIN.nCounter", model.TypeInt16)
	read := func() ([]driver.RawSample, error) {
		return c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{ref}}})
	}

	// 正常路径先建立一个句柄，避免句柄解析本身干扰后面的断言。
	if out, err := read(); err != nil || out[0].Err != nil {
		t.Fatalf("baseline read: %v %+v", err, out)
	}

	b.set(1) // Read 的 AMS 错误 → 链路级
	if _, err := read(); err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("read AMS error: %v", err)
	}
	b.set(3) // 业务错误码（invalid size）→ 逐点地址错误
	out, err := read()
	if err != nil {
		t.Fatalf("result-code read should not fail the round: %v", err)
	}
	if out[0].Err == nil || driver.KindOf(out[0].Err) != driver.ErrAddress {
		t.Fatalf("read result sample = %+v", out[0])
	}
	b.set(2) // 载荷缺 length 字段 → 协议错误
	if _, err := read(); err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("read short: %v", err)
	}

	// 未缓存的变量：句柄解析返回业务错误码（address 类，不中断整轮）。
	b.set(0)
	c.mu.Lock()
	delete(c.handles, "MAIN.nSetpoint")
	c.mu.Unlock()
	b.set(3)
	out, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		sym("MAIN.nSetpoint", model.TypeInt16),
	}}})
	if err != nil {
		t.Fatalf("handle error should stay per-point: %v", err)
	}
	if out[0].Err == nil || driver.KindOf(out[0].Err) != driver.ErrAddress {
		t.Fatalf("handle error sample = %+v", out[0])
	}

	// 写入：AMS 错误是链路级失败，后续写入不再尝试。
	c.mu.Lock()
	delete(c.handles, "MAIN.nSetpoint")
	c.mu.Unlock()
	b.set(0)
	if res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "p", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1),
	}}); err != nil || !res[0].OK {
		t.Fatalf("baseline write: %v %+v", err, res)
	}
	b.set(1)
	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "a", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
			DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1)},
		{PointID: "b", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
			DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1)},
	})
	if err == nil || driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("write AMS error: %v", err)
	}
	if len(res) != 2 || res[0].Err == nil || res[1].Err == nil {
		t.Fatalf("write results = %+v", res)
	}

	// Write 应答缺 result → 协议错误，但连接仍可用（不是链路失败）。
	// 句柄仍在缓存里，这样请求直接落到 Write 而不是先重新解析名字。
	b.set(4)
	res, err = c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "p", Address: sym("MAIN.nSetpoint", model.TypeInt16).Address,
		DataType: model.TypeInt16, Value: model.NumValue(model.TypeInt16, 1),
	}})
	if err != nil {
		t.Fatalf("short write response should be per-point: %v", err)
	}
	if res[0].Err == nil || driver.KindOf(res[0].Err) != driver.ErrProtocol {
		t.Fatalf("short write result = %+v", res[0])
	}
}

// 对端一直不回包时，ctx 先到期的那条路径：错误分类必须是 timeout，
// 而不是把 deadline 当成断链。
func TestExchangeContextDeadline(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	s := startScripted(t, func(req adscodec.Frame) [][]byte {
		if req.Command == adscodec.CmdReadDeviceInfo {
			return encodeFrames(okDeviceInfo(req))
		}
		<-blocked // 其余请求一律不回
		return nil
	})
	cfg := s.config(t)
	cfg.Channel.Timeout = model.Duration(2 * time.Second)
	conn, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := conn.(*connState)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.DeviceState(ctx)
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "context done") && !strings.Contains(msg, "timeout") {
		t.Fatalf("message = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "ads",
			Transport: model.Transport{Kind: "tcp", Address: addr},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
		Device: model.Device{ID: "plc", ChannelID: "ch", Labels: map[string]string{
			"netid": "127.0.0.1.1.1", "options": "retries=0",
		}},
	}
	_, err = New().Open(context.Background(), cfg)
	if err == nil || driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("dial: %v", err)
	}
	if !strings.Contains(err.Error(), "connect") {
		t.Fatalf("message = %v", err)
	}
}
