package ads

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	adscodec "uic/internal/codec/ads"
)

// 仿真端与真实设备的差异已在包注释里列清；测试要钉住的是「差异之外的
// 行为与真实设备一致」：句柄、错误码、只读标记、长度校验。因此下面的
// 用例一律走真实 TCP 回环 + codec 构造的请求，而不是直接调用 handle()。
// 只有「畸形载荷」这类 TCP 上难以构造的输入才直接调 handle()。

// peerFor 返回以仿真端为目标的编解码端点。
func peerFor(s *Server) adscodec.Peer {
	return adscodec.Peer{
		Source: adscodec.Endpoint{NetID: adscodec.MustParseNetID("127.0.0.1.1.1"), Port: 32905},
		Target: adscodec.Endpoint{NetID: s.NetID(), Port: s.Port()},
	}
}

// roundTrip 建立一条新连接，发一帧收一帧。每条用例独立连接，避免上一条
// 用例的句柄/状态泄漏到下一条。
func roundTrip(t *testing.T, s *Server, req adscodec.Frame) (adscodec.Frame, error) {
	t.Helper()
	nc, err := net.DialTimeout("tcp", s.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(2 * time.Second))
	raw, err := req.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := nc.Write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	var hdr [adscodec.TCPHeaderLen]byte
	if _, err := readFull(nc, hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n, err := adscodec.FrameLen(hdr[:])
	if err != nil {
		t.Fatalf("frame len: %v", err)
	}
	buf := make([]byte, n)
	copy(buf, hdr[:])
	if _, err := readFull(nc, buf[adscodec.TCPHeaderLen:]); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return adscodec.Decode(buf)
}

func readFull(c net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// session 在同一条连接上完成一串请求：句柄必须属于同一会话才有意义。
type session struct {
	t   *testing.T
	nc  net.Conn
	srv *Server
}

func dialSession(t *testing.T, s *Server) *session {
	t.Helper()
	nc, err := net.DialTimeout("tcp", s.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = nc.Close() })
	return &session{t: t, nc: nc, srv: s}
}

func (ss *session) do(req adscodec.Frame) adscodec.Frame {
	ss.t.Helper()
	raw, err := req.Encode()
	if err != nil {
		ss.t.Fatalf("encode: %v", err)
	}
	if _, err := ss.nc.Write(raw); err != nil {
		ss.t.Fatalf("write: %v", err)
	}
	var hdr [adscodec.TCPHeaderLen]byte
	if _, err := readFull(ss.nc, hdr[:]); err != nil {
		ss.t.Fatalf("read header: %v", err)
	}
	n, err := adscodec.FrameLen(hdr[:])
	if err != nil {
		ss.t.Fatalf("frame len: %v", err)
	}
	buf := make([]byte, n)
	copy(buf, hdr[:])
	if _, err := readFull(ss.nc, buf[adscodec.TCPHeaderLen:]); err != nil {
		ss.t.Fatalf("read body: %v", err)
	}
	f, err := adscodec.Decode(buf)
	if err != nil {
		ss.t.Fatalf("decode: %v", err)
	}
	return f
}

func (ss *session) handle(name string) uint32 {
	ss.t.Helper()
	resp := ss.do(peerFor(ss.srv).HandleByName(7, name))
	if resp.Error != adscodec.CodeOK {
		ss.t.Fatalf("handle %s: AMS %v", name, resp.Error)
	}
	data, code, err := adscodec.ParseReadWrite(resp)
	if err != nil {
		ss.t.Fatalf("handle %s: %v", name, err)
	}
	if code != adscodec.CodeOK {
		ss.t.Fatalf("handle %s: %v", name, code)
	}
	if len(data) != 4 {
		ss.t.Fatalf("handle %s: %d bytes", name, len(data))
	}
	return binary.LittleEndian.Uint32(data)
}

func newSession(t *testing.T, s *Server) *session { return dialSession(t, s) }

func startServer(t *testing.T, opt ...Options) *Server {
	t.Helper()
	var o Options
	if len(opt) > 0 {
		o = opt[0]
	}
	s, err := NewServerWithOptions("127.0.0.1:0", o)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---------------------------------------------------------------------------
// 生命周期与夹具
// ---------------------------------------------------------------------------

func TestDefaults(t *testing.T) {
	s := startServer(t)
	if s.NetID().String() != DefaultNetID {
		t.Fatalf("netid = %v", s.NetID())
	}
	if s.Port() != DefaultAMSPort {
		t.Fatalf("port = %d", s.Port())
	}
	if s.State() != adscodec.StateRun {
		t.Fatalf("state = %v", s.State())
	}
	names := s.Variables()
	if len(names) != 13 {
		t.Fatalf("variables = %d: %v", len(names), names)
	}
	// 排序后返回，便于自检输出做 diff。
	// 按字符串排序（'B' < '['），因此 arrBytes 排在 arr[0] 之前。
	if names[0] != "MAIN.arrBytes BYTE[4]" || names[1] != "MAIN.arr[0] INT16" {
		t.Fatalf("first variables = %q %q", names[0], names[1])
	}
	if !contains(names, "MAIN.fSpeed LREAL") || !contains(names, "MAIN.sRecipe STRING(80)") {
		t.Fatalf("variables = %v", names)
	}

	raw, ok := s.Get("MAIN.nCounter")
	if !ok || binary.LittleEndian.Uint16(raw) != 1234 {
		t.Fatalf("nCounter = % x (%v)", raw, ok)
	}
	if _, ok := s.Get("MAIN.nope"); ok {
		t.Fatal("unknown variable returned ok")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestFixtureSetters(t *testing.T) {
	s := startServer(t)
	if err := s.SetValue("MAIN.nCounter", []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	raw, _ := s.Get("MAIN.nCounter")
	if !bytes.Equal(raw, []byte{1, 2}) {
		t.Fatalf("raw = % x", raw)
	}
	if err := s.SetValue("MAIN.nCounter", []byte{1}); err == nil {
		t.Fatal("wrong width accepted")
	}
	if err := s.SetValue("MAIN.nope", []byte{1}); err == nil {
		t.Fatal("unknown variable accepted")
	}
	if err := s.SetWritable("MAIN.nCounter", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWritable("MAIN.nope", true); err == nil {
		t.Fatal("unknown variable accepted")
	}
}

func TestBadOptionsAndClose(t *testing.T) {
	if _, err := NewServerWithOptions("127.0.0.1:0", Options{NetID: "1.2.3"}); err == nil {
		t.Fatal("bad netid accepted")
	}
	// 端口被占用时必须报错而不是假装启动成功。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := NewServer(ln.Addr().String()); err == nil {
		t.Fatal("busy address accepted")
	}

	s := startServer(t, Options{Name: "line-7 PLC", Port: 801})
	if s.Port() != 801 {
		t.Fatalf("port = %d", s.Port())
	}
	ss := newSession(t, s)
	resp := ss.do(peerFor(s).ReadDeviceInfo(1))
	info, code, err := adscodec.ParseReadDeviceInfo(resp)
	if err != nil || code != adscodec.CodeOK {
		t.Fatalf("info: %v %v", err, code)
	}
	if info.Name != "line-7 PLC" {
		t.Fatalf("name = %q", info.Name)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 会话：变量名 → 句柄 → 值
// ---------------------------------------------------------------------------

func TestReadWriteByHandle(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)

	h := ss.handle("MAIN.nSetpoint")
	resp := ss.do(p.ReadByHandle(8, h, 2))
	data, code, err := adscodec.ParseRead(resp)
	if err != nil || code != adscodec.CodeOK {
		t.Fatalf("read: %v %v", err, code)
	}
	if got := int16(binary.LittleEndian.Uint16(data)); got != 500 {
		t.Fatalf("setpoint = %d", got)
	}

	resp = ss.do(p.WriteByHandle(9, h, []byte{0xBF, 0xFE})) // -321（小端）
	if code, err := adscodec.ParseWrite(resp); err != nil || code != adscodec.CodeOK {
		t.Fatalf("write: %v %v", err, code)
	}
	if raw, _ := s.Get("MAIN.nSetpoint"); binary.LittleEndian.Uint16(raw) != 0xFEBF {
		t.Fatalf("raw = % x", raw)
	}
	if s.Writes() != 1 {
		t.Fatalf("writes = %d", s.Writes())
	}

	// 再读一次必须看到新值——句柄映射没有指错变量。
	resp = ss.do(p.ReadByHandle(10, h, 2))
	data, _, _ = adscodec.ParseRead(resp)
	if got := int16(binary.LittleEndian.Uint16(data)); got != -321 {
		t.Fatalf("reread = %d", got)
	}

	// 释放后同一句柄必须失效，且释放本身幂等。
	if code, err := adscodec.ParseWrite(ss.do(p.ReleaseHandle(11, h))); err != nil || code != adscodec.CodeOK {
		t.Fatalf("release: %v %v", err, code)
	}
	if code, err := adscodec.ParseWrite(ss.do(p.ReleaseHandle(12, h))); err != nil || code != adscodec.CodeOK {
		t.Fatalf("second release: %v %v", err, code)
	}
	_, code, err = adscodec.ParseRead(ss.do(p.ReadByHandle(13, h, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if code != adscodec.CodeInvalidOffset {
		t.Fatalf("read after release = %v", code)
	}
	if got := s.Requests(); got < 7 {
		t.Fatalf("requests = %d", got)
	}
}

func TestReadErrors(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)
	h := ss.handle("MAIN.nCounter")

	cases := []struct {
		name string
		req  adscodec.Frame
		want adscodec.Code
	}{
		{"wrong length", p.ReadByHandle(1, h, 4), adscodec.CodeInvalidSize},
		{"unknown handle", p.ReadByHandle(2, 0xFEEDFACE, 2), adscodec.CodeInvalidOffset},
		{"unknown group", p.Read(3, 0xF004, 0, 2), adscodec.CodeInvalidGroup},
	}
	for _, tc := range cases {
		_, code, err := adscodec.ParseRead(ss.do(tc.req))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if code != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, code, tc.want)
		}
	}
}

func TestWriteErrors(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)

	ro := ss.handle("MAIN.nCounter")
	rw := ss.handle("MAIN.nSetpoint")

	cases := []struct {
		name string
		req  adscodec.Frame
		want adscodec.Code
	}{
		{"read-only", p.WriteByHandle(1, ro, []byte{0, 0}), adscodec.CodeInvalidAccess},
		{"wrong width", p.WriteByHandle(2, rw, []byte{0}), adscodec.CodeInvalidSize},
		{"unknown handle", p.WriteByHandle(3, 0xDEAD, []byte{0, 0}), adscodec.CodeInvalidOffset},
		{"unknown group", p.Write(4, 0x4020, 0, []byte{1}), adscodec.CodeInvalidGroup},
	}
	for _, tc := range cases {
		code, err := adscodec.ParseWrite(ss.do(tc.req))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if code != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, code, tc.want)
		}
	}
	if s.Writes() != 0 {
		t.Fatalf("writes = %d", s.Writes())
	}
	// 失败写入不得改变设备值。
	if raw, _ := s.Get("MAIN.nSetpoint"); binary.LittleEndian.Uint16(raw) != 500 {
		t.Fatalf("setpoint changed to % x", raw)
	}
}

func TestHandleByNameErrors(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)

	for name, req := range map[string]adscodec.Frame{
		"unknown symbol":  p.HandleByName(1, "MAIN.nope"),
		"empty name":      p.HandleByName(2, ""),
		"array no index":  p.HandleByName(3, "MAIN.arr"),
		"wrong group":     p.ReadWrite(4, 0xF005, 0, 4, []byte("MAIN.nCounter\x00")),
		"wrong read size": p.ReadWrite(5, adscodec.IGSymHandleByName, 0, 8, []byte("MAIN.nCounter\x00")),
	} {
		_, code, err := adscodec.ParseReadWrite(ss.do(req))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := adscodec.CodeSymbolNotFound
		switch name {
		case "wrong group":
			want = adscodec.CodeInvalidGroup
		case "wrong read size":
			want = adscodec.CodeInvalidSize
		}
		if code != want {
			t.Fatalf("%s = %v, want %v", name, code, want)
		}
	}
}

func TestInvalidateHandles(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)
	h := ss.handle("MAIN.nCounter")
	if s.Requests() == 0 {
		t.Fatal("no requests")
	}
	s.InvalidateHandles()
	_, code, err := adscodec.ParseRead(ss.do(p.ReadByHandle(2, h, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if code != adscodec.CodeInvalidOffset {
		t.Fatalf("stale handle = %v", code)
	}
	// 重新解析后必须仍然指向同一个变量（句柄是新的）。
	h2 := ss.handle("MAIN.nCounter")
	if h2 == h {
		t.Fatalf("handle reused: %#x", h2)
	}
	data, _, _ := adscodec.ParseRead(ss.do(p.ReadByHandle(3, h2, 2)))
	if int16(binary.LittleEndian.Uint16(data)) != 1234 {
		t.Fatalf("value after re-resolve = % x", data)
	}
}

// ---------------------------------------------------------------------------
// 设备信息 / 状态 / 控制
// ---------------------------------------------------------------------------

func TestDeviceInfoAndState(t *testing.T) {
	s := startServer(t, Options{Name: "sim-plc"})
	ss := newSession(t, s)
	p := peerFor(s)

	info, code, err := adscodec.ParseReadDeviceInfo(ss.do(p.ReadDeviceInfo(1)))
	if err != nil || code != adscodec.CodeOK {
		t.Fatalf("info: %v %v", err, code)
	}
	if info.Name != "sim-plc" || info.VersionString() != "3.1.4024" {
		t.Fatalf("info = %+v", info)
	}

	st, code, err := adscodec.ParseReadState(ss.do(p.ReadState(2)))
	if err != nil || code != adscodec.CodeOK {
		t.Fatalf("state: %v %v", err, code)
	}
	if st.ADS != adscodec.StateRun || st.Device != 0 {
		t.Fatalf("state = %+v", st)
	}

	for _, target := range []adscodec.State{adscodec.StateStop, adscodec.StateReset, adscodec.StateRun} {
		if code, err := adscodec.ParseWriteControl(ss.do(p.WriteControl(3, target, 0, nil))); err != nil || code != adscodec.CodeOK {
			t.Fatalf("control %v: %v %v", target, err, code)
		}
		if s.State() != target {
			t.Fatalf("state = %v, want %v", s.State(), target)
		}
	}
	// 不支持的运行状态：设备拒绝，且状态保持不变。
	if code, _ := adscodec.ParseWriteControl(ss.do(p.WriteControl(4, adscodec.State(99), 0, nil))); code != adscodec.CodeInvalidState {
		t.Fatalf("invalid state = %v", code)
	}
	if s.State() != adscodec.StateRun {
		t.Fatalf("state changed to %v", s.State())
	}
}

// ---------------------------------------------------------------------------
// AMS 寻址
// ---------------------------------------------------------------------------

func TestWrongTarget(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)

	badNet := peerFor(s)
	badNet.Target.NetID = adscodec.MustParseNetID("10.0.0.1.1.1")
	resp := ss.do(badNet.ReadDeviceInfo(1))
	if resp.Error != adscodec.CodeTargetMachineNotFound {
		t.Fatalf("netid error = %v", resp.Error)
	}
	// AMS 错误帧不应带 ADS 载荷。
	if len(resp.Data) != 0 {
		t.Fatalf("payload = % x", resp.Data)
	}

	badPort := peerFor(s)
	badPort.Target.Port = 852
	if resp := ss.do(badPort.ReadDeviceInfo(2)); resp.Error != adscodec.CodeTargetPortNotFound {
		t.Fatalf("port error = %v", resp.Error)
	}
	if s.Requests() != 0 {
		t.Fatalf("requests = %d, want 0 (rejected before dispatch)", s.Requests())
	}
}

func TestUnknownCommand(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)
	resp := ss.do(adscodec.Frame{
		Target: p.Target, Source: p.Source,
		Command: adscodec.Command(99), Flags: adscodec.FlagADS, Invoke: 1,
	})
	if resp.Error != adscodec.CodeUnknownCommandID {
		t.Fatalf("error = %v", resp.Error)
	}
}

// 通知帧是单向的：仿真端不回包。用一条连接发通知，紧接着发设备信息；
// 若仿真端错误地给通知回了包，读到的第一帧会是通知的应答而不是设备信息。
func TestNotificationIgnored(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)
	notify := adscodec.Frame{
		Target: p.Target, Source: p.Source,
		Command: adscodec.CmdDeviceNotification, Flags: adscodec.FlagADS, Invoke: 1,
	}
	raw, err := notify.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.nc.Write(raw); err != nil {
		t.Fatal(err)
	}
	// 通知不应答；紧接着的设备信息应答必须是读到的第一帧。
	resp := ss.do(p.ReadDeviceInfo(2))
	if resp.Command != adscodec.CmdReadDeviceInfo || resp.Invoke != 2 {
		t.Fatalf("first response = %v invoke=%d", resp.Command, resp.Invoke)
	}
}

// ---------------------------------------------------------------------------
// 畸形载荷与坏帧
// ---------------------------------------------------------------------------

func TestMalformedPayloads(t *testing.T) {
	s := startServer(t)
	cases := []struct {
		name string
		cmd  adscodec.Command
		data []byte
		want adscodec.Code
	}{
		{"read short", adscodec.CmdRead, make([]byte, 8), adscodec.CodeInvalidSize},
		{"write short", adscodec.CmdWrite, make([]byte, 8), adscodec.CodeInvalidSize},
		{"write length overruns", adscodec.CmdWrite, func() []byte {
			b := make([]byte, 12)
			binary.LittleEndian.PutUint32(b[0:4], adscodec.IGSymValueByHandle)
			binary.LittleEndian.PutUint32(b[8:12], 99)
			return b
		}(), adscodec.CodeInvalidSize},
		{"release short value", adscodec.CmdWrite, func() []byte {
			b := make([]byte, 13)
			binary.LittleEndian.PutUint32(b[0:4], adscodec.IGSymReleaseHandle)
			binary.LittleEndian.PutUint32(b[8:12], 1)
			return b
		}(), adscodec.CodeInvalidSize},
		{"readwrite short", adscodec.CmdReadWrite, make([]byte, 12), adscodec.CodeInvalidSize},
		{"readwrite length overruns", adscodec.CmdReadWrite, func() []byte {
			b := make([]byte, 16)
			binary.LittleEndian.PutUint32(b[0:4], adscodec.IGSymHandleByName)
			binary.LittleEndian.PutUint32(b[12:16], 99)
			return b
		}(), adscodec.CodeInvalidSize},
		{"control short", adscodec.CmdWriteControl, make([]byte, 4), adscodec.CodeInvalidSize},
	}
	req := adscodec.Frame{
		Target: adscodec.Endpoint{NetID: s.NetID(), Port: s.Port()},
		Source: adscodec.Endpoint{NetID: adscodec.MustParseNetID("127.0.0.1.1.1"), Port: 32905},
		Flags:  adscodec.FlagADS, Invoke: 1,
	}
	for _, tc := range cases {
		req.Command = tc.cmd
		req.Data = tc.data
		resp, ok := s.handle(req)
		if !ok {
			t.Fatalf("%s: no response", tc.name)
		}
		if len(resp.Data) < 4 {
			t.Fatalf("%s: payload = % x", tc.name, resp.Data)
		}
		if got := adscodec.Code(binary.LittleEndian.Uint32(resp.Data[0:4])); got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// 坏帧（长度字段与真实长度不符）只能断链，不能把仿真端带崩；后续连接
// 必须仍然可用。
func TestBadFrameDropsConnection(t *testing.T) {
	s := startServer(t)
	nc, err := net.DialTimeout("tcp", s.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.SetDeadline(time.Now().Add(2 * time.Second))
	// TCP 头声称 0x1000 字节的 AMS 报文，实际只发 8 字节。
	bad := []byte{0, 0, 0x00, 0x10, 0, 0, 1, 2, 3, 4}
	if _, err := nc.Write(bad); err != nil {
		t.Fatal(err)
	}
	// 仿真端读到一半就断链：Read 最终应返回错误而不是永久阻塞。
	one := make([]byte, 1)
	_, _ = nc.Read(one)
	_ = nc.Close()

	ss := newSession(t, s)
	if _, code, err := adscodec.ParseReadDeviceInfo(ss.do(peerFor(s).ReadDeviceInfo(1))); err != nil || code != adscodec.CodeOK {
		t.Fatalf("server unusable after bad frame: %v %v", err, code)
	}
}

// ---------------------------------------------------------------------------
// 变量表细节
// ---------------------------------------------------------------------------

func TestArrayAndStrings(t *testing.T) {
	s := startServer(t)
	ss := newSession(t, s)
	p := peerFor(s)

	for i, want := range []int16{10, 20, 30, 40} {
		name := fmt.Sprintf("MAIN.arr[%d]", i)
		h := ss.handle(name)
		data, _, _ := adscodec.ParseRead(ss.do(p.ReadByHandle(1, h, 2)))
		if got := int16(binary.LittleEndian.Uint16(data)); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	// 只有 [2] 可写。
	if code, _ := adscodec.ParseWrite(ss.do(p.WriteByHandle(2, ss.handle("MAIN.arr[0]"), []byte{0, 1}))); code != adscodec.CodeInvalidAccess {
		t.Fatalf("arr[0] write = %v", code)
	}
	if code, _ := adscodec.ParseWrite(ss.do(p.WriteByHandle(3, ss.handle("MAIN.arr[2]"), []byte{7, 0}))); code != adscodec.CodeOK {
		t.Fatalf("arr[2] write = %v", code)
	}

	h := ss.handle("MAIN.sRecipe")
	data, _, _ := adscodec.ParseRead(ss.do(p.ReadByHandle(4, h, DefaultStrLen)))
	if got := strings.TrimRight(string(data), "\x00"); got != "batch-42" {
		t.Fatalf("recipe = %q", got)
	}
	if len(data) != DefaultStrLen {
		t.Fatalf("recipe length = %d", len(data))
	}

	h = ss.handle("MAIN.fSpeed")
	data, _, _ = adscodec.ParseRead(ss.do(p.ReadByHandle(5, h, 8)))
	if got := math.Float64frombits(binary.LittleEndian.Uint64(data)); got != 1450.75 {
		t.Fatalf("speed = %v", got)
	}
}
