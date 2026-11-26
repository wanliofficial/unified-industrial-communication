// Package coap 是内置的 CoAP 服务端（测试 / 演示用）。
//
// 它实现的是驱动用得到的那一小块 CoAP：GET/PUT/POST、CON 的 ACK、NON 的
// NON 响应、重复 MID 去重、Block2 分块、`/.well-known/core` 资源发现，以及
// 一条用于验证「分离响应」的路径（先回空 ACK，再用新的 MID 发响应）。
//
// **Observe**（RFC 7641）：服务端按 GET 请求里的 Observe 选项维护观察关系，
// 资源内容变化（SetValue）时向观察者主动推送带递增序号的通知。用于验证
// 客户端的注册、新鲜度判定、ACK CON 通知与退回 GET 的降级路径。
//
// 刻意不做：
//
//   - **DTLS**：内置服务端只讲明文 UDP，与驱动一致。
//   - **Block1**：收到带 Block1 的写请求直接回 4.02；驱动也不发。
//
// 去重缓存的语义与真实服务端一致：同一个客户端用同一个 MID 重复发 CON，
// 必须重发**同一条**响应而不是重新执行。POST 不是幂等的，没这条缓存时
// 「客户端以为丢了、重发一次」会变成「设备动作执行了两次」。
package coap

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	coapcodec "uic/internal/codec/coap"
)

// DefaultBlockSize 是内置服务端的默认块大小。
//
// 取 128 而不是 1024：常见的小 JSON 资源能一块装下，而演示用的 `/large`
// 会自然分成多块，测试不需要构造几十 KB 的载荷。
const DefaultBlockSize = 128

// Options 是内置服务端的可调参数。
type Options struct {
	// BlockSize 是服务端允许的最大块大小；0 使用 DefaultBlockSize。
	BlockSize int
	// DisableBlock2 模拟「不认识 Block2 的老服务端」：带 Block2 的请求
	// 回 4.02，其余请求一律整包返回。用它验证客户端的降级路径。
	DisableBlock2 bool
}

// resource 是一个 CoAP 资源。
type resource struct {
	typ      string // 资源类型（link-format 的 rt）
	format   uint16
	methods  map[coapcodec.Code]bool
	body     []byte
	onGet    func() []byte
	separate bool // 先回空 ACK，再用新 MID 发 CON 响应
	reset    bool // 直接回 RST
}

// observer 是一个已注册观察关系的客户端（RFC 7641）。
type observer struct {
	peer   *net.UDPAddr
	token  []byte
	seq    uint32 // 最近一次通知的 Observe 序号
	maxAge time.Duration
}

// Server 是内置 CoAP 服务端。
type Server struct {
	conn      *net.UDPConn
	blockSize int
	noBlock2  bool

	mu        sync.Mutex
	resources map[string]*resource
	order     []string

	// observers 按资源路径索引观察者，供 SetValue 推送通知。token 区分同一
	// 客户端对同一资源的多次观察（重新注册会替换旧的）。
	observers  map[string][]*observer
	nextObsSeq uint32

	dedup      map[dedupKey]cachedResponse
	dedupOrder []dedupKey
	nextMID    uint16

	requests   int
	writes     int
	blockGets  int
	badPackets int
	separateOK bool

	// notify 统计主动推送的通知数（测试/诊断用）。
	notify int
}

type dedupKey struct {
	peer string
	mid  uint16
}

type cachedResponse struct {
	emptyAck    []byte
	response    []byte
	separate    bool
	separateMID uint16
}

// NewServer 在 addr（如 "127.0.0.1:0"）上监听 UDP。
func NewServer(addr string) (*Server, error) {
	return NewServerWithOptions(addr, Options{})
}

// NewServerWithOptions 带参数启动服务端。
func NewServerWithOptions(addr string, opt Options) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("coap sim: resolve %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, fmt.Errorf("coap sim: listen %q: %w", addr, err)
	}
	blockSize := opt.BlockSize
	if blockSize <= 0 || !validBlockSize(blockSize) {
		blockSize = DefaultBlockSize
	}
	s := &Server{
		conn:      conn,
		blockSize: blockSize,
		noBlock2:  opt.DisableBlock2,
		resources: map[string]*resource{},
		observers: map[string][]*observer{},
		dedup:     map[dedupKey]cachedResponse{},
		nextMID:   0x8000,
	}
	s.loadDefaults()
	go s.serve()
	return s, nil
}

func validBlockSize(n int) bool {
	for szx := 0; szx <= 6; szx++ {
		if n == 1<<(szx+4) {
			return true
		}
	}
	return false
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.conn.LocalAddr().String() }

// Close 停止服务端。
func (s *Server) Close() error { return s.conn.Close() }

// Requests 返回收到的合法请求数。
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Writes 返回成功执行的写请求数。
func (s *Server) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// BlockGets 返回带 Block2 选项的读请求数（含续块）。
func (s *Server) BlockGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blockGets
}

// BadPackets 返回无法解码的报文数。
func (s *Server) BadPackets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badPackets
}

// SeparateAcked 表示分离响应的 CON 是否被客户端 ACK 过。
func (s *Server) SeparateAcked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.separateOK
}

// SetValue 直接改写某个资源的内容（测试夹具用）。
//
// 若该资源有观察者，改动后会向每个观察者推送一条通知（带递增 Observe 序号、
// 资源当前的 Content-Format 与内容）。推送在释放锁之后进行，避免持锁做 I/O。
func (s *Server) SetValue(path string, body string) bool {
	s.mu.Lock()
	r, ok := s.resources[path]
	if !ok {
		s.mu.Unlock()
		return false
	}
	r.body = []byte(body)
	r.onGet = nil
	obs := append([]*observer(nil), s.observers[path]...)
	format := r.format
	s.mu.Unlock()

	for _, o := range obs {
		s.pushNotify(path, o, format, []byte(body))
	}
	return true
}

// pushNotify 向单个观察者推送一条通知（NON，避免与服务端对 CON 的 ACK 状态
// 机纠缠；RFC 7641 允许通知用 NON）。序号每资源递增。
func (s *Server) pushNotify(path string, o *observer, format uint16, body []byte) {
	s.mu.Lock()
	s.nextObsSeq++
	seq := s.nextObsSeq & coapcodec.ObserveSeqMask
	mid := s.nextMID
	s.nextMID++
	if s.nextMID == 0 {
		s.nextMID = 1
	}
	s.notify++
	s.mu.Unlock()

	msg := coapcodec.Message{
		Type: coapcodec.TypeNonConfirmable, Code: coapcodec.CodeContent,
		MID: mid, Token: append([]byte(nil), o.token...),
		Options: []coapcodec.Option{
			{Number: coapcodec.OptObserve, Value: coapcodec.EncodeUint(seq)},
			{Number: coapcodec.OptContentFormat, Value: coapcodec.EncodeUint(uint32(format))},
			{Number: coapcodec.OptMaxAge, Value: coapcodec.EncodeUint(uint32(o.maxAge / time.Second))},
		},
		Payload: append([]byte(nil), body...),
	}
	b, err := coapcodec.Encode(msg)
	if err != nil {
		return
	}
	_, _ = s.conn.WriteToUDP(b, o.peer)
}

// Notifications 返回主动推送的通知数。
func (s *Server) Notifications() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify
}

// Observers 返回某资源的观察者数量。
func (s *Server) Observers(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.observers[path])
}

// Value 返回某个资源当前的内容。
func (s *Server) Value(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.resources[path]
	if !ok {
		return "", false
	}
	if r.onGet != nil {
		return string(r.onGet()), true
	}
	return string(r.body), true
}

// Resources 返回全部资源路径（已排序）。
func (s *Server) Resources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.order...)
	sort.Strings(out)
	return out
}

func (s *Server) loadDefaults() {
	add := func(path string, r *resource) {
		s.resources[path] = r
		s.order = append(s.order, path)
	}
	getPut := func(typ string, format uint16, body string) *resource {
		return &resource{
			typ: typ, format: format, body: []byte(body),
			methods: map[coapcodec.Code]bool{coapcodec.CodeGET: true, coapcodec.CodePUT: true},
		}
	}
	readOnly := func(typ string, format uint16, body string) *resource {
		return &resource{
			typ: typ, format: format, body: []byte(body),
			methods: map[coapcodec.Code]bool{coapcodec.CodeGET: true},
		}
	}
	// getPost 模拟「提交/触发」型资源：POST 不幂等，正好用来验证去重语义。
	getPost := func(typ string, format uint16, body string) *resource {
		return &resource{
			typ: typ, format: format, body: []byte(body),
			methods: map[coapcodec.Code]bool{coapcodec.CodeGET: true, coapcodec.CodePOST: true},
		}
	}

	add("/sensors/temp", readOnly("temperature", coapcodec.FormatTextPlain, "21.5"))
	add("/sensors/humidity", readOnly("humidity", coapcodec.FormatTextPlain, "45.0"))
	add("/sensors/env", readOnly("environment", coapcodec.FormatJSON,
		`{"temperature":21.5,"humidity":45.0,"pressure":1013.25,"ok":true}`))
	add("/actuator/valve", getPut("valve", coapcodec.FormatTextPlain, "false"))
	add("/actuator/setpoint", getPut("setpoint", coapcodec.FormatTextPlain, "0"))
	add("/actuator/trigger", getPost("trigger", coapcodec.FormatTextPlain, "false"))
	add("/config", getPut("config", coapcodec.FormatJSON, `{"mode":"auto","threshold":10}`))
	add("/blob", readOnly("binary-blob", coapcodec.FormatOctets, "\x00\x01\x02\xff"))
	add("/large", readOnly("large", coapcodec.FormatTextPlain, largeBody(3000)))
	add("/separate", &resource{
		typ: "separate", format: coapcodec.FormatTextPlain, body: []byte("7.5"),
		methods:  map[coapcodec.Code]bool{coapcodec.CodeGET: true},
		separate: true,
	})
	add("/reset", &resource{
		typ: "test", format: coapcodec.FormatTextPlain, body: []byte("x"),
		methods: map[coapcodec.Code]bool{coapcodec.CodeGET: true},
		reset:   true,
	})
	// 发现入口本身不列进清单（否则每次发现都会看到自己）。
	add("/.well-known/core", &resource{
		typ: "core", format: coapcodec.FormatLink,
		methods: map[coapcodec.Code]bool{coapcodec.CodeGET: true},
		onGet:   s.linkFormat,
	})
}

func largeBody(n int) string {
	var b bytes.Buffer
	for b.Len() < n {
		fmt.Fprintf(&b, "%04d-", b.Len())
	}
	return b.String()[:n]
}

// linkFormat 生成资源发现清单。
func (s *Server) linkFormat() []byte {
	var b bytes.Buffer
	for _, path := range s.order {
		if path == "/.well-known/core" {
			// 发现入口本身不列进清单（否则每次发现都会看到自己）。
			continue
		}
		r := s.resources[path]
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "<%s>;rt=%q;ct=%d", path, r.typ, r.format)
	}
	return b.Bytes()
}

func (s *Server) serve() {
	buf := make([]byte, 65535)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		dg := append([]byte(nil), buf[:n]...)
		s.handle(dg, from)
	}
}

func (s *Server) handle(dg []byte, from *net.UDPAddr) {
	msg, err := coapcodec.Decode(dg)
	if err != nil {
		s.mu.Lock()
		s.badPackets++
		s.mu.Unlock()
		return
	}
	if msg.Type == coapcodec.TypeAck && msg.Code == coapcodec.CodeEmpty {
		// 客户端确认了某条分离响应。
		s.mu.Lock()
		for key, c := range s.dedup {
			if c.separate && c.separateMID == msg.MID && key.peer == from.String() {
				s.separateOK = true
				break
			}
		}
		s.mu.Unlock()
		return
	}
	if !msg.Code.IsRequest() {
		return
	}

	s.mu.Lock()
	s.requests++
	if msg.Code == coapcodec.CodeGET {
		if _, ok := msg.Get(coapcodec.OptBlock2); ok {
			s.blockGets++
		}
	}
	s.mu.Unlock()

	if msg.Type == coapcodec.TypeConfirmable {
		key := dedupKey{peer: from.String(), mid: msg.MID}
		s.mu.Lock()
		cached, ok := s.dedup[key]
		s.mu.Unlock()
		if ok {
			// 重发同一条响应，不重复执行。分离响应的空 ACK 与真正的
			// 响应是两条报文，两条都要重发——否则客户端会一直等下去。
			if len(cached.emptyAck) > 0 {
				_, _ = s.conn.WriteToUDP(cached.emptyAck, from)
			}
			_, _ = s.conn.WriteToUDP(cached.response, from)
			return
		}
	}

	out := s.build(msg, from)
	if out.reset {
		b, err := coapcodec.Encode(coapcodec.Message{
			Type: coapcodec.TypeReset, Code: coapcodec.CodeEmpty, MID: msg.MID,
		})
		if err == nil {
			_, _ = s.conn.WriteToUDP(b, from)
		}
		return
	}

	if msg.Type == coapcodec.TypeNonConfirmable {
		resp := coapcodec.Message{
			Type: coapcodec.TypeNonConfirmable, Code: out.code,
			MID: s.takeMID(), Token: msg.Token,
			Options: out.options, Payload: out.payload,
		}
		b, err := coapcodec.Encode(resp)
		if err == nil {
			_, _ = s.conn.WriteToUDP(b, from)
		}
		return
	}

	// CON：默认搭载响应（ACK 携带同一个 MID 与 Token）。
	if !out.separate {
		resp := coapcodec.Message{
			Type: coapcodec.TypeAck, Code: out.code,
			MID: msg.MID, Token: msg.Token,
			Options: out.options, Payload: out.payload,
		}
		b, err := coapcodec.Encode(resp)
		if err != nil {
			return
		}
		s.cacheResponse(dedupKey{peer: from.String(), mid: msg.MID}, cachedResponse{response: b})
		_, _ = s.conn.WriteToUDP(b, from)
		return
	}

	// 分离响应：先空 ACK，再用新的 MID 发一条 CON。
	ack, err := coapcodec.Encode(coapcodec.Message{
		Type: coapcodec.TypeAck, Code: coapcodec.CodeEmpty, MID: msg.MID,
	})
	if err != nil {
		return
	}
	sepMID := s.takeMID()
	resp := coapcodec.Message{
		Type: coapcodec.TypeConfirmable, Code: out.code,
		MID: sepMID, Token: msg.Token,
		Options: out.options, Payload: out.payload,
	}
	rb, err := coapcodec.Encode(resp)
	if err != nil {
		return
	}
	s.cacheResponse(dedupKey{peer: from.String(), mid: msg.MID},
		cachedResponse{emptyAck: ack, response: rb, separate: true, separateMID: sepMID})
	_, _ = s.conn.WriteToUDP(ack, from)
	_, _ = s.conn.WriteToUDP(rb, from)
}

type outcome struct {
	reset    bool
	separate bool
	code     coapcodec.Code
	options  []coapcodec.Option
	payload  []byte
}

func (s *Server) build(msg coapcodec.Message, from *net.UDPAddr) outcome {
	path := msg.Path()

	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.resources[path]
	if !ok {
		return outcome{code: coapcodec.CodeNotFound, payload: []byte("no such resource")}
	}
	if r.reset {
		return outcome{reset: true}
	}
	if !r.methods[msg.Code] {
		return outcome{code: coapcodec.CodeMethodNotAllowed,
			payload: []byte("method not allowed")}
	}

	// Block1 不在能力范围内；明确拒绝而不是把半条载荷当成完整值。
	if _, has := msg.Get(coapcodec.OptBlock1); has {
		return outcome{code: coapcodec.CodeBadOption, payload: []byte("Block1 is not supported")}
	}

	switch msg.Code {
	case coapcodec.CodeGET:
		body := r.body
		if r.onGet != nil {
			body = r.onGet()
		}
		out := s.buildContent(r, msg, body)
		// Observe：按 GET 里的 Observe 选项注册（值=0）、注销（值=1）。注册时
		// 在首条响应里带上当前序号，之后 SetValue 推送递增序号。
		if ov, has := msg.UintOption(coapcodec.OptObserve); has {
			switch ov & coapcodec.ObserveSeqMask {
			case coapcodec.ObserveRegister:
				if out.code == coapcodec.CodeContent {
					seq := s.registerObserver(path, from, msg.Token)
					out.options = append(out.options, coapcodec.Option{
						Number: coapcodec.OptObserve, Value: coapcodec.EncodeUint(seq),
					})
				}
			case coapcodec.ObserveCancel:
				s.deregisterObserver(path, from, msg.Token)
			}
		}
		// 分离响应是资源属性：只有 `/separate` 会先回空 ACK。漏掉这一行，
		// 客户端就只能看到一条搭载响应，分离路径完全测不到。
		out.separate = r.separate
		return out
	case coapcodec.CodePUT, coapcodec.CodePOST:
		if len(msg.Payload) == 0 {
			return outcome{code: coapcodec.CodeBadRequest, payload: []byte("empty payload")}
		}
		r.body = append([]byte(nil), msg.Payload...)
		if v, ok := msg.UintOption(coapcodec.OptContentFormat); ok {
			r.format = uint16(v)
		}
		r.onGet = nil
		s.writes++
		return outcome{code: coapcodec.CodeChanged}
	default:
		return outcome{code: coapcodec.CodeMethodNotAllowed}
	}
}

// buildContent 处理 GET 的 Block2。
func (s *Server) buildContent(r *resource, msg coapcodec.Message, body []byte) outcome {
	format := coapcodec.Option{Number: coapcodec.OptContentFormat,
		Value: coapcodec.EncodeUint(uint32(r.format))}

	// 模拟不支持 Block2 的老服务端：带 Block2 的请求直接 4.02，
	// 其余请求整包返回。客户端据此应退回「不带 Block2」再取一次。
	if s.noBlock2 {
		if _, ok := msg.Get(coapcodec.OptBlock2); ok {
			return outcome{code: coapcodec.CodeBadOption, payload: []byte("Block2 is not supported")}
		}
		return outcome{code: coapcodec.CodeContent, options: []coapcodec.Option{format},
			payload: append([]byte(nil), body...)}
	}

	size := s.blockSize
	num := uint32(0)
	requested := false
	if bv, ok := msg.Get(coapcodec.OptBlock2); ok {
		blk, err := coapcodec.DecodeBlock(bv)
		if err != nil {
			return outcome{code: coapcodec.CodeBadOption, payload: []byte("bad Block2 option")}
		}
		requested = true
		num = blk.Num
		if int(blk.Size) < size {
			size = int(blk.Size)
		}
	}
	if size <= 0 {
		size = s.blockSize
	}
	offset := uint64(num) * uint64(size)
	if offset > uint64(len(body)) || (offset == uint64(len(body)) && num > 0) {
		return outcome{code: coapcodec.CodeBadOption, payload: []byte("Block2 out of range")}
	}
	end := offset + uint64(size)
	if end > uint64(len(body)) {
		end = uint64(len(body))
	}
	more := end < uint64(len(body))

	// 小载荷且客户端没有点名要分块时，按普通响应返回，避免每条报文都带
	// 一个 Block2 选项。Content-Format 仍要带：读取端靠它决定按文本还是
	// JSON 解码，省掉它会让 `{"a":1}` 被当成字符串。
	if !requested && !more && num == 0 {
		return outcome{code: coapcodec.CodeContent, options: []coapcodec.Option{format},
			payload: append([]byte(nil), body...)}
	}
	blk, err := coapcodec.EncodeBlock(coapcodec.Block{Num: num, More: more, Size: uint32(size)})
	if err != nil {
		return outcome{code: coapcodec.CodeInternalError}
	}
	return outcome{
		code:    coapcodec.CodeContent,
		options: []coapcodec.Option{format, {Number: coapcodec.OptBlock2, Value: blk}},
		payload: append([]byte(nil), body[offset:end]...),
	}
}

func (s *Server) takeMID() uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextMID++
	if s.nextMID == 0 {
		s.nextMID = 1
	}
	return s.nextMID
}

func (s *Server) cacheResponse(key dedupKey, c cachedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.dedup[key]; !exists {
		s.dedupOrder = append(s.dedupOrder, key)
		// 限制缓存规模，避免长时间运行时无限增长。
		if len(s.dedupOrder) > 1024 {
			old := s.dedupOrder[0]
			s.dedupOrder = s.dedupOrder[1:]
			delete(s.dedup, old)
		}
	}
	s.dedup[key] = c
}

// registerObserver 登记一个观察者并返回当前 Observe 序号。同 peer+token 重复
// 注册会替换旧的（客户端重注册是常态）。调用方持有 s.mu。
func (s *Server) registerObserver(path string, from *net.UDPAddr, token []byte) uint32 {
	peer := from.String()
	list := s.observers[path]
	for _, o := range list {
		if o.peer.String() == peer && bytesEqual(o.token, token) {
			// 重复注册：保留序号继续递增，刷新 maxAge。
			return o.seq
		}
	}
	s.nextObsSeq++
	seq := s.nextObsSeq & coapcodec.ObserveSeqMask
	o := &observer{
		peer:   from,
		token:  append([]byte(nil), token...),
		seq:    seq,
		maxAge: 60 * time.Second,
	}
	s.observers[path] = append(list, o)
	return seq
}

// deregisterObserver 移除某个观察者。调用方持有 s.mu。
func (s *Server) deregisterObserver(path string, from *net.UDPAddr, token []byte) {
	peer := from.String()
	list := s.observers[path]
	out := list[:0]
	for _, o := range list {
		if o.peer.String() == peer && bytesEqual(o.token, token) {
			continue
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		delete(s.observers, path)
	} else {
		s.observers[path] = out
	}
}

// bytesEqual 比较两个切片。
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
