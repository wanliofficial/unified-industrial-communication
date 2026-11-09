package coap

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	coapcodec "uic/internal/codec/coap"
)

// startServer 启动一台内置服务端并返回它。地址用 127.0.0.1:0，
// 端口由内核分配——测试并发跑时不会因为抢固定端口而偶发失败。
func startServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func dial(t *testing.T, s *Server) *net.UDPConn {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp", s.Addr())
	if err != nil {
		t.Fatalf("resolve %q: %v", s.Addr(), err)
	}
	c, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		t.Fatalf("dial %q: %v", s.Addr(), err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readMsg 读一条报文；超时按测试失败处理。
func readMsg(t *testing.T, c *net.UDPConn) coapcodec.Message {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m, err := coapcodec.Decode(buf[:n])
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

// send 编码并发送一条报文。
func send(t *testing.T, c *net.UDPConn, m coapcodec.Message) {
	t.Helper()
	b, err := coapcodec.Encode(m)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

// getRequest 构造一个 GET。
func getRequest(mid uint16, path string) coapcodec.Message {
	m := coapcodec.Message{Type: coapcodec.TypeConfirmable, Code: coapcodec.CodeGET,
		MID: mid, Token: []byte{byte(mid), 0xAA}}
	m.SetPath(path)
	return m
}

// TestConfirmableRequest 钉住最基本的「CON 请求 → ACK 搭载响应」。
// MID 与 Token 都必须原样回显，否则客户端的配对逻辑会把它当成迟到响应丢弃。
func TestConfirmableRequest(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	req := getRequest(0x0101, "/sensors/temp")
	send(t, c, req)
	resp := readMsg(t, c)

	if resp.Type != coapcodec.TypeAck {
		t.Fatalf("type = %v, want ACK", resp.Type)
	}
	if resp.MID != req.MID {
		t.Fatalf("mid = %#x, want %#x", resp.MID, req.MID)
	}
	if !bytes.Equal(resp.Token, req.Token) {
		t.Fatalf("token = %x, want %x", resp.Token, req.Token)
	}
	if resp.Code != coapcodec.CodeContent {
		t.Fatalf("code = %v, want 2.05", resp.Code)
	}
	if string(resp.Payload) != "21.5" {
		t.Fatalf("payload = %q, want 21.5", resp.Payload)
	}
	if v, ok := resp.UintOption(coapcodec.OptContentFormat); !ok || uint16(v) != coapcodec.FormatTextPlain {
		t.Fatalf("content-format = %d ok=%v", v, ok)
	}
	if s.Requests() != 1 {
		t.Fatalf("requests = %d, want 1", s.Requests())
	}
}

// TestNonConfirmableRequest 覆盖 NON：请求不需要 ACK，响应也是 NON，
// 并且必须换一个新的 MID（NON 的响应不是对原 MID 的确认）。
func TestNonConfirmableRequest(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	req := getRequest(0x0202, "/sensors/humidity")
	req.Type = coapcodec.TypeNonConfirmable
	send(t, c, req)
	resp := readMsg(t, c)

	if resp.Type != coapcodec.TypeNonConfirmable {
		t.Fatalf("type = %v, want NON", resp.Type)
	}
	if resp.MID == req.MID {
		t.Fatalf("response reused request MID %#x; NON responses must use a fresh MID", resp.MID)
	}
	if !bytes.Equal(resp.Token, req.Token) {
		t.Fatalf("token = %x, want %x", resp.Token, req.Token)
	}
	if string(resp.Payload) != "45.0" {
		t.Fatalf("payload = %q, want 45.0", resp.Payload)
	}
}

// TestDuplicateMIDIsDeduplicated 是这套服务端最重要的一条性质：
// 同一个客户端用同一个 MID 重发 CON 时，必须重发**同一条**响应，而不是
// 重新执行。写请求不幂等，没有这条缓存，「客户端以为丢了、补发一次」
// 就会变成「阀门动作了两次」。
func TestDuplicateMIDIsDeduplicated(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	req := coapcodec.Message{Type: coapcodec.TypeConfirmable, Code: coapcodec.CodePUT,
		MID: 0x0303, Token: []byte{9}}
	req.SetPath("/actuator/setpoint")
	req.SetOption(coapcodec.OptContentFormat, coapcodec.EncodeUint(uint32(coapcodec.FormatTextPlain)))
	req.Payload = []byte("42")

	send(t, c, req)
	first := readMsg(t, c)
	if first.Code != coapcodec.CodeChanged {
		t.Fatalf("first code = %v, want 2.04", first.Code)
	}

	// 逐字节重发同一条报文。
	send(t, c, req)
	second := readMsg(t, c)

	if s.Writes() != 1 {
		t.Fatalf("writes = %d, want 1 (duplicate MID must not re-execute)", s.Writes())
	}
	if !bytes.Equal(first.Payload, second.Payload) || first.Code != second.Code {
		t.Fatalf("duplicate response differs: first=%v/%q second=%v/%q",
			first.Code, first.Payload, second.Code, second.Payload)
	}
	if got, _ := s.Value("/actuator/setpoint"); got != "42" {
		t.Fatalf("value = %q, want 42", got)
	}

	// 换一个 MID 的同样写入才会真的执行（并再次改变计数）。
	again := req
	again.MID = 0x0304
	send(t, c, again)
	if r := readMsg(t, c); r.Code != coapcodec.CodeChanged {
		t.Fatalf("second write code = %v, want 2.04", r.Code)
	}
	if s.Writes() != 2 {
		t.Fatalf("writes = %d, want 2", s.Writes())
	}
}

// TestBlock2Transfer 覆盖大资源的 Block2 分块与越界。
func TestBlock2Transfer(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	// 首个请求不带 Block2：服务端自己决定分块，回第一块并置 M=1。
	req := getRequest(0x0404, "/large")
	send(t, c, req)
	resp := readMsg(t, c)
	if resp.Code != coapcodec.CodeContent {
		t.Fatalf("code = %v, want 2.05", resp.Code)
	}
	bv, ok := resp.Get(coapcodec.OptBlock2)
	if !ok {
		t.Fatal("first block has no Block2 option")
	}
	blk, err := coapcodec.DecodeBlock(bv)
	if err != nil {
		t.Fatalf("decode block: %v", err)
	}
	if blk.Num != 0 || !blk.More {
		t.Fatalf("first block = %+v, want num=0 more=true", blk)
	}
	body := append([]byte(nil), resp.Payload...)
	size := blk.Size
	mid := uint16(0x0500)

	for blk.More {
		next := getRequest(mid, "/large")
		mid++
		enc, err := coapcodec.EncodeBlock(coapcodec.Block{Num: blk.Num + 1, Size: size})
		if err != nil {
			t.Fatalf("encode block: %v", err)
		}
		next.SetOption(coapcodec.OptBlock2, enc)
		send(t, c, next)
		r := readMsg(t, c)
		if r.Code != coapcodec.CodeContent {
			t.Fatalf("block %d code = %v", blk.Num+1, r.Code)
		}
		v, ok := r.Get(coapcodec.OptBlock2)
		if !ok {
			t.Fatalf("block %d missing Block2", blk.Num+1)
		}
		b, err := coapcodec.DecodeBlock(v)
		if err != nil {
			t.Fatalf("decode block %d: %v", blk.Num+1, err)
		}
		if b.Num != blk.Num+1 || b.Size != size {
			t.Fatalf("block = %+v, want num=%d size=%d", b, blk.Num+1, size)
		}
		body = append(body, r.Payload...)
		blk = b
	}

	want, _ := s.Value("/large")
	if string(body) != want {
		t.Fatalf("reassembled %d bytes, want %d", len(body), len(want))
	}
	if len(body) != 3000 {
		t.Fatalf("body = %d bytes, want 3000", len(body))
	}
	if s.BlockGets() == 0 {
		t.Fatal("server did not count any Block2 GET")
	}

	// 越界块号：请求一个远超资源长度的块，必须回 4.02，而不是空 2.05。
	oor := getRequest(0x0600, "/large")
	enc, _ := coapcodec.EncodeBlock(coapcodec.Block{Num: 100000, Size: 128})
	oor.SetOption(coapcodec.OptBlock2, enc)
	send(t, c, oor)
	if r := readMsg(t, c); r.Code != coapcodec.CodeBadOption {
		t.Fatalf("out-of-range block code = %v, want 4.02", r.Code)
	}
}

// TestErrorCodes 覆盖 4.04 / 4.05 / Block1 拒绝。
func TestErrorCodes(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	// 不存在的资源 → 4.04。
	send(t, c, getRequest(0x0701, "/no/such/thing"))
	if r := readMsg(t, c); r.Code != coapcodec.CodeNotFound {
		t.Fatalf("unknown resource code = %v, want 4.04", r.Code)
	}

	// 只支持 GET 的资源收到 POST → 4.05。
	post := coapcodec.Message{Type: coapcodec.TypeConfirmable, Code: coapcodec.CodePOST,
		MID: 0x0702, Token: []byte{1}}
	post.SetPath("/sensors/temp")
	post.Payload = []byte("x")
	send(t, c, post)
	if r := readMsg(t, c); r.Code != coapcodec.CodeMethodNotAllowed {
		t.Fatalf("POST read-only code = %v, want 4.05", r.Code)
	}

	// 带 Block1 的写请求 → 4.02（本服务端不做分块发送）。
	put := coapcodec.Message{Type: coapcodec.TypeConfirmable, Code: coapcodec.CodePUT,
		MID: 0x0703, Token: []byte{2}}
	put.SetPath("/actuator/valve")
	blk1, _ := coapcodec.EncodeBlock(coapcodec.Block{Num: 0, More: true, Size: 64})
	put.SetOption(coapcodec.OptBlock1, blk1)
	put.Payload = []byte("true")
	send(t, c, put)
	if r := readMsg(t, c); r.Code != coapcodec.CodeBadOption {
		t.Fatalf("Block1 code = %v, want 4.02", r.Code)
	}

	// 写请求缺载荷 → 4.00。
	empty := coapcodec.Message{Type: coapcodec.TypeConfirmable, Code: coapcodec.CodePUT,
		MID: 0x0704, Token: []byte{3}}
	empty.SetPath("/actuator/valve")
	send(t, c, empty)
	if r := readMsg(t, c); r.Code != coapcodec.CodeBadRequest {
		t.Fatalf("empty PUT code = %v, want 4.00", r.Code)
	}
}

// getFull 读取一个资源并自动续读 Block2，返回首条响应与完整载荷。
// 首条响应保留原始选项（Content-Format 就在里面）。
func getFull(t *testing.T, c *net.UDPConn, path string, mid *uint16) (coapcodec.Message, []byte) {
	t.Helper()
	req := getRequest(*mid, path)
	*mid++
	send(t, c, req)
	resp := readMsg(t, c)
	if resp.Code != coapcodec.CodeContent {
		t.Fatalf("GET %s = %v", path, resp.Code)
	}
	bv, ok := resp.Get(coapcodec.OptBlock2)
	if !ok {
		return resp, resp.Payload
	}
	blk, err := coapcodec.DecodeBlock(bv)
	if err != nil {
		t.Fatalf("decode first block of %s: %v", path, err)
	}
	body := append([]byte(nil), resp.Payload...)
	for blk.More {
		next := getRequest(*mid, path)
		*mid++
		enc, err := coapcodec.EncodeBlock(coapcodec.Block{Num: blk.Num + 1, Size: blk.Size})
		if err != nil {
			t.Fatalf("encode block: %v", err)
		}
		next.SetOption(coapcodec.OptBlock2, enc)
		send(t, c, next)
		r := readMsg(t, c)
		if r.Code != coapcodec.CodeContent {
			t.Fatalf("block %d of %s = %v", blk.Num+1, path, r.Code)
		}
		v, ok := r.Get(coapcodec.OptBlock2)
		if !ok {
			t.Fatalf("block %d of %s missing Block2", blk.Num+1, path)
		}
		b, err := coapcodec.DecodeBlock(v)
		if err != nil {
			t.Fatalf("decode block %d of %s: %v", blk.Num+1, path, err)
		}
		body = append(body, r.Payload...)
		blk = b
	}
	return resp, body
}

// TestDiscovery 覆盖 `/.well-known/core` 与 link-format 的自洽性。
func TestDiscovery(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	req := getRequest(0x0808, "/.well-known/core")
	req.SetUintOption(coapcodec.OptAccept, uint32(coapcodec.FormatLink))
	send(t, c, req)
	resp := readMsg(t, c)

	if resp.Code != coapcodec.CodeContent {
		t.Fatalf("discovery code = %v, want 2.05", resp.Code)
	}
	if v, ok := resp.UintOption(coapcodec.OptContentFormat); !ok || uint16(v) != coapcodec.FormatLink {
		t.Fatalf("content-format = %d ok=%v, want 40", v, ok)
	}

	// 默认块大小下清单会超过一块，续读后再解析。
	mid := uint16(0x0810)
	_, body := getFull(t, c, "/.well-known/core", &mid)
	links, err := coapcodec.ParseLinkFormat(body)
	if err != nil {
		t.Fatalf("parse link-format %q: %v", body, err)
	}
	seen := map[string]coapcodec.Link{}
	for _, l := range links {
		seen[l.Path] = l
	}
	for _, path := range []string{"/sensors/temp", "/sensors/env", "/large", "/actuator/valve"} {
		if _, ok := seen[path]; !ok {
			t.Errorf("discovery missing %s in %q", path, body)
		}
	}
	if _, ok := seen["/.well-known/core"]; ok {
		t.Error("discovery listed itself; it must be excluded")
	}
	if got := seen["/sensors/env"].Params["ct"]; got != "50" {
		t.Fatalf("/sensors/env ct = %q, want 50", got)
	}
	if got := seen["/sensors/temp"].Params["rt"]; got != "temperature" {
		t.Fatalf("/sensors/temp rt = %q", got)
	}
}

// TestSeparateResponse 覆盖「空 ACK + 新 MID 的 CON」这一分离响应路径。
//
// 客户端必须先收到一条空 ACK（证明请求被接受、别重发），随后真正的响应
// 从另一条报文回来。这条路径的失败模式很隐蔽：只处理 ACK 的实现会认为
// 「响应是空的」，只等分离响应的实现则会对空 ACK 重传。
func TestSeparateResponse(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	req := getRequest(0x0909, "/separate")
	send(t, c, req)

	ack := readMsg(t, c)
	if ack.Type != coapcodec.TypeAck || ack.Code != coapcodec.CodeEmpty {
		t.Fatalf("first message = %v/%v, want empty ACK", ack.Type, ack.Code)
	}
	if ack.MID != req.MID {
		t.Fatalf("ack mid = %#x, want %#x", ack.MID, req.MID)
	}

	resp := readMsg(t, c)
	if resp.Type != coapcodec.TypeConfirmable {
		t.Fatalf("separate response type = %v, want CON", resp.Type)
	}
	if resp.MID == req.MID {
		t.Fatalf("separate response reused request MID %#x", resp.MID)
	}
	if !bytes.Equal(resp.Token, req.Token) {
		t.Fatalf("token = %x, want %x", resp.Token, req.Token)
	}
	if resp.Code != coapcodec.CodeContent || string(resp.Payload) != "7.5" {
		t.Fatalf("separate response = %v/%q", resp.Code, resp.Payload)
	}

	// 客户端确认这条 CON；服务端据此把 SeparateAcked 置真。
	ackBack, err := coapcodec.Encode(coapcodec.Message{
		Type: coapcodec.TypeAck, Code: coapcodec.CodeEmpty, MID: resp.MID,
	})
	if err != nil {
		t.Fatalf("encode ack: %v", err)
	}
	if _, err := c.Write(ackBack); err != nil {
		t.Fatalf("write ack: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !s.SeparateAcked() {
		if time.Now().After(deadline) {
			t.Fatal("server never observed the ACK for the separate response")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestResetResponse 覆盖「直接回 RST」的拒绝路径。
func TestResetResponse(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	req := getRequest(0x0A0A, "/reset")
	send(t, c, req)
	resp := readMsg(t, c)

	if resp.Type != coapcodec.TypeReset {
		t.Fatalf("type = %v, want RST", resp.Type)
	}
	if resp.Code != coapcodec.CodeEmpty {
		t.Fatalf("code = %v, want 0.00", resp.Code)
	}
	if resp.MID != req.MID {
		t.Fatalf("mid = %#x, want %#x", resp.MID, req.MID)
	}
}

// TestMalformedDatagram 覆盖畸形报文计数。
//
// 顺序很关键：服务端单线程处理，只要随后那条合法请求的响应回来了，
// 就说明前面的畸形包一定已经被处理过，不需要 sleep 赌时序。
func TestMalformedDatagram(t *testing.T) {
	s := startServer(t)
	c := dial(t, s)

	if _, err := c.Write([]byte{0x40, 0x01}); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	send(t, c, getRequest(0x0B0B, "/sensors/temp"))
	if r := readMsg(t, c); r.Code != coapcodec.CodeContent {
		t.Fatalf("valid request after malformed = %v", r.Code)
	}
	if s.BadPackets() != 1 {
		t.Fatalf("bad packets = %d, want 1", s.BadPackets())
	}
}

// TestValueAccessors 覆盖测试夹具用的读写接口。
func TestValueAccessors(t *testing.T) {
	s := startServer(t)

	if !s.SetValue("/sensors/temp", "99.9") {
		t.Fatal("SetValue on existing resource returned false")
	}
	if got, ok := s.Value("/sensors/temp"); !ok || got != "99.9" {
		t.Fatalf("Value = %q ok=%v", got, ok)
	}
	if s.SetValue("/nope", "x") {
		t.Fatal("SetValue on missing resource returned true")
	}
	if _, ok := s.Value("/nope"); ok {
		t.Fatal("Value on missing resource returned true")
	}

	res := s.Resources()
	if len(res) == 0 {
		t.Fatal("Resources is empty")
	}
	for i := 1; i < len(res); i++ {
		if res[i] < res[i-1] {
			t.Fatalf("Resources not sorted: %v", res)
		}
	}
	if !strings.Contains(strings.Join(res, ","), "/sensors/temp") {
		t.Fatalf("Resources missing /sensors/temp: %v", res)
	}
}

// TestCustomBlockSize 覆盖块大小参数与非法值回落。
func TestCustomBlockSize(t *testing.T) {
	s, err := NewServerWithOptions("127.0.0.1:0", Options{BlockSize: 64})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()
	if s.blockSize != 64 {
		t.Fatalf("blockSize = %d, want 64", s.blockSize)
	}

	// 非 2 的幂回落到默认值，而不是启动失败。
	s2, err := NewServerWithOptions("127.0.0.1:0", Options{BlockSize: 100})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s2.Close()
	if s2.blockSize != DefaultBlockSize {
		t.Fatalf("invalid blockSize = %d, want default %d", s2.blockSize, DefaultBlockSize)
	}
}
