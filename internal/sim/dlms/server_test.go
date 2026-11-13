package dlms

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	dl "uic/internal/codec/dlms"
)

// pipeStream 把 net.Pipe 包成 transport.Stream，用于不经 TCP 直接驱动从站。
type pipeStream struct{ c net.Conn }

func (p *pipeStream) ReadFull(n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (p *pipeStream) Write(b []byte) error {
	_, err := p.c.Write(b)
	return err
}

func (p *pipeStream) Close() error { return p.c.Close() }

// SetDeadline 设置读写截止时间（net.Pipe 支持）。
func (p *pipeStream) SetDeadline(t time.Time) error { return p.c.SetDeadline(t) }

// newPipe 建一对 pipe，并在其中一端跑从站循环。
func newPipe(t *testing.T, opts ...Option) (*Server, *pipeStream) {
	t.Helper()
	s := newServer(opts...)
	a, b := net.Pipe()
	go func() { _ = s.ServeStream(&pipeStream{c: b}) }()
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return s, &pipeStream{c: a}
}

// client 是测试用的最小主站：发一帧、收一帧。
type client struct {
	st  *pipeStream
	t   *testing.T
	ns  uint8
	nr  uint8
	src dl.Address
	dst dl.Address
}

func newClient(t *testing.T, st *pipeStream) *client {
	return &client{
		st:  st,
		t:   t,
		src: dl.ClientAddress(0x10),
		dst: dl.ServerAddress(1, 0, 1),
	}
}

// send 发送一帧（自动补标志）。
func (c *client) send(fr dl.Frame) {
	c.t.Helper()
	body, err := fr.Encode()
	if err != nil {
		c.t.Fatalf("Encode: %v", err)
	}
	if err := dl.WriteFrame(c.st, body); err != nil {
		c.t.Fatalf("WriteFrame: %v", err)
	}
}

// recv 收一帧。
func (c *client) recv() dl.Frame {
	c.t.Helper()
	_ = c.st.SetDeadline(time.Time{})
	r := dl.NewReader(c.st)
	fr, err := r.Next()
	if err != nil {
		c.t.Fatalf("Next: %v", err)
	}
	return fr
}

// snrm 建链。
func (c *client) snrm() dl.Frame {
	c.t.Helper()
	c.send(dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindSNRM, PF: true}})
	fr := c.recv()
	if fr.Kind() != dl.KindUA {
		c.t.Fatalf("SNRM 应答 = %v", fr.Kind())
	}
	c.ns, c.nr = 0, 0
	return fr
}

// aarq 建应用关联。
func (c *client) aarq() []byte {
	c.t.Helper()
	req := dl.AssociateRequest{Initiate: dl.InitiateRequest{
		DLMSVersion: 6, Conformance: dl.ConfGet | dl.ConfAttribute0Get, MaxReceivePDU: 512}}
	apdu, err := req.Encode()
	if err != nil {
		c.t.Fatalf("AARQ Encode: %v", err)
	}
	c.send(dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindI, NS: int(c.ns), NR: int(c.nr), PF: true},
		Info:    append([]byte(dl.LLCCommand), apdu...)})
	fr := c.recv()
	if fr.Kind() != dl.KindI {
		c.t.Fatalf("AARQ 应答帧 = %v", fr.Kind())
	}
	if !fr.IsResponse() {
		c.t.Fatal("AARQ 应答缺 LLC 应答头")
	}
	c.ns = (c.ns + 1) & 0x07
	c.nr = (uint8(fr.Control.NS) + 1) & 0x07
	ap, ok := fr.APDU()
	if !ok {
		c.t.Fatal("AARQ 应答缺 APDU")
	}
	return ap
}

// get 读一个属性，返回应答 APDU。
func (c *client) get(class uint16, obis string, attr int8) []byte {
	c.t.Helper()
	o, err := dl.ParseOBIS(obis)
	if err != nil {
		c.t.Fatalf("ParseOBIS: %v", err)
	}
	apdu := dl.GetRequest{Attribute: dl.AttributeDescriptor{
		ClassID: class, InstanceID: [6]byte(o), Attribute: attr}}.Encode()
	c.send(dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindI, NS: int(c.ns), NR: int(c.nr), PF: true},
		Info:    append([]byte(dl.LLCCommand), apdu...)})
	fr := c.recv()
	if fr.Kind() != dl.KindI {
		c.t.Fatalf("GET 应答帧 = %v", fr.Kind())
	}
	c.ns = (c.ns + 1) & 0x07
	c.nr = (uint8(fr.Control.NS) + 1) & 0x07
	ap, ok := fr.APDU()
	if !ok {
		c.t.Fatal("GET 应答缺 APDU")
	}
	return ap
}

// 从站必须按 SNRM → UA、DISC → UA 应答，且计数正确。
func TestServerLinkControl(t *testing.T) {
	s, st := newPipe(t)
	c := newClient(t, st)

	c.snrm()
	if snrm, _, _, _ := s.Counts(); snrm != 1 {
		t.Fatalf("SNRM 计数 = %d", snrm)
	}

	// DISC → UA。
	c.send(dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindDISC, PF: true}})
	if fr := c.recv(); fr.Kind() != dl.KindUA {
		t.Fatalf("DISC 应答 = %v", fr.Kind())
	}
	if _, _, _, disc := s.Counts(); disc != 1 {
		t.Fatalf("DISC 计数 = %d", disc)
	}
}

// AARQ 必须被接受，且 AARE 带 InitiateResponse 与协商后的 conformance。
func TestServerAssociation(t *testing.T) {
	s, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()

	ap := c.aarq()
	resp, err := dl.ParseAssociateResponse(ap)
	if err != nil {
		t.Fatalf("ParseAssociateResponse: %v", err)
	}
	if !resp.ApplicationOK {
		t.Fatal("从站应接受无安全关联")
	}
	if !resp.HasInitiate {
		t.Fatal("AARE 缺 InitiateResponse")
	}
	if resp.Initiate.DLMSVersion != 6 {
		t.Errorf("DLMS 版本 = %d", resp.Initiate.DLMSVersion)
	}
	if resp.Initiate.MaxReceivePDU != 0x0200 {
		t.Errorf("max PDU = %d", resp.Initiate.MaxReceivePDU)
	}
	if !resp.Initiate.Conformance.Has(dl.ConfGet) {
		t.Error("协商结果应含 get")
	}
	if _, aarq, _, _ := s.Counts(); aarq != 1 {
		t.Errorf("AARQ 计数 = %d", aarq)
	}
}

// 拒绝关联时 result 必须非 0，主站据此报协议错误而不是超时。
func TestServerRejectAssociation(t *testing.T) {
	s, st := newPipe(t, func(s *Server) { s.rejectAssociation = true })
	c := newClient(t, st)
	c.snrm()

	ap := c.aarq()
	resp, err := dl.ParseAssociateResponse(ap)
	if err != nil {
		t.Fatalf("ParseAssociateResponse: %v", err)
	}
	if resp.ApplicationOK {
		t.Fatal("拒绝模式下不应被接受")
	}
	if resp.Result == 0 {
		t.Fatal("拒绝时 result 不应为 0")
	}
	_ = s
}

// Get 成功：电能返回 uint32 原始值，电压返回 uint16，表号返回 octet-string。
func TestServerGet(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()
	c.aarq()

	cases := []struct {
		obis  string
		class uint16
		n     float64
		text  string
	}{
		{"1.0.1.8.0.255", dl.ClassRegister, 12345678, ""},
		{"1.0.32.7.0.255", dl.ClassRegister, 2201, ""},
		{"0.0.96.1.0.255", dl.ClassData, 0, "12345678"},
	}
	for _, tc := range cases {
		ap := c.get(tc.class, tc.obis, dl.AttrValue)
		resp, err := dl.ParseGetResponse(ap)
		if err != nil {
			t.Fatalf("%s: ParseGetResponse: %v", tc.obis, err)
		}
		if resp.IsResult || !resp.HasData {
			t.Fatalf("%s: resp = %+v", tc.obis, resp)
		}
		if tc.n != 0 {
			if n, ok := resp.Data.Number(); !ok || n != tc.n {
				t.Errorf("%s: 值 = %v, want %v", tc.obis, n, tc.n)
			}
		}
		if tc.text != "" {
			if got := string(resp.Data.Bytes); got != tc.text {
				t.Errorf("%s: 文本 = %q, want %q", tc.obis, got, tc.text)
			}
		}
	}
}

// 读 scaler_unit（attribute 3）必须返回结构体 {integer, enum}。
func TestServerGetScalerUnit(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()
	c.aarq()

	ap := c.get(dl.ClassRegister, "1.0.32.7.0.255", dl.AttrScalerUnit)
	resp, err := dl.ParseGetResponse(ap)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !resp.HasData {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Data.Elems) != 2 {
		t.Fatalf("scaler_unit 元素数 = %d", len(resp.Data.Elems))
	}
	if resp.Data.Elems[0].Int != -1 {
		t.Errorf("scaler = %d, want -1", resp.Data.Elems[0].Int)
	}
	if resp.Data.Elems[1].Int != 34 {
		t.Errorf("unit = %d, want 34(V)", resp.Data.Elems[1].Int)
	}
}

// 不存在的 OBIS 必须回 object-undefined(4)：这是 OBIS 写错。
func TestServerObjectUndefined(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()
	c.aarq()

	ap := c.get(dl.ClassRegister, "1.0.99.99.0.255", dl.AttrValue)
	resp, err := dl.ParseGetResponse(ap)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !resp.IsResult {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Result != dl.DARObjectUndefined {
		t.Fatalf("结果码 = %d(%s), want object-undefined", resp.Result, resp.Result)
	}
}

// OBIS 存在但类号错，必须回 object-class-inconsistent(9)：这是类号写错。
// 与 object-undefined 区分开，现场才知道该改 OBIS 还是该改类号。
func TestServerClassInconsistent(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()
	c.aarq()

	ap := c.get(dl.ClassData, "1.0.1.8.0.255", dl.AttrValue)
	resp, err := dl.ParseGetResponse(ap)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !resp.IsResult {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Result != dl.DARObjectClassInconsistent {
		t.Fatalf("结果码 = %d(%s), want object-class-inconsistent", resp.Result, resp.Result)
	}
}

// 未建关联就读：必须回明确结果码，而不是静默无应答。
func TestServerGetBeforeAssociate(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)
	c.snrm() // 只建 HDLC，不建应用关联

	ap := c.get(dl.ClassRegister, "1.0.1.8.0.255", dl.AttrValue)
	resp, err := dl.ParseGetResponse(ap)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !resp.IsResult || resp.Result != dl.DARReadWriteDenied {
		t.Fatalf("resp = %+v, want read-write-denied", resp)
	}
}

// 静默模式：一个字都不回。
func TestServerSilent(t *testing.T) {
	s, st := newPipe(t, func(s *Server) { s.silent = true })
	c := newClient(t, st)
	_ = s

	body, err := (dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindSNRM, PF: true}}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := dl.WriteFrame(c.st, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// 设一个读超时，确认没有应答。
	_ = c.st.SetDeadline(time.Now().Add(200 * time.Millisecond))
	r := dl.NewReader(c.st)
	if _, err := r.Next(); err == nil {
		t.Fatal("静默模式下不应收到应答")
	}
}

// 坏校验：应答的 FCS 被改坏，主站必须靠校验序列发现（Reader 计入坏帧）。
//
// 注意开关要在建链**之后**才打开：AARE 也带信息域，若一开始就坏，建链会
// 直接失败，测不到「Get 应答坏了但链路本身是通的」这个现场最常见的形态。
func TestServerBadChecksum(t *testing.T) {
	s, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()
	c.aarq()
	s.SetBadChecksum(true)

	o, _ := dl.ParseOBIS("1.0.1.8.0.255")
	apdu := dl.GetRequest{Attribute: dl.AttributeDescriptor{
		ClassID: dl.ClassRegister, InstanceID: [6]byte(o), Attribute: dl.AttrValue}}.Encode()
	c.send(dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindI, NS: int(c.ns), NR: int(c.nr), PF: true},
		Info:    append([]byte(dl.LLCCommand), apdu...)})

	_ = c.st.SetDeadline(time.Now().Add(300 * time.Millisecond))
	r := dl.NewReader(c.st)
	if _, err := r.Next(); err == nil {
		t.Fatal("坏 FCS 的帧不应被接受")
	}
	if r.BadFrames() == 0 {
		t.Fatal("坏帧计数应为正")
	}
}

// 地址过滤：dest 不是我的帧必须静默，不能替别人的地址应答。
func TestServerAddressFilter(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)

	// dest = 逻辑设备 2（从站配的是 1）。
	body, err := (dl.Frame{
		Dest:    dl.ServerAddress(2, 0, 1),
		Src:     c.src,
		Control: dl.Control{Kind: dl.KindSNRM, PF: true},
	}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := dl.WriteFrame(c.st, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = c.st.SetDeadline(time.Now().Add(200 * time.Millisecond))
	r := dl.NewReader(c.st)
	if _, err := r.Next(); err == nil {
		t.Fatal("地址不匹配的帧不应被应答")
	}
}

// 客户端地址不匹配也必须静默。
func TestServerClientFilter(t *testing.T) {
	_, st := newPipe(t)
	c := newClient(t, st)
	c.src = dl.ClientAddress(0x20) // 从站配的是 0x10

	body, err := (dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindSNRM, PF: true}}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := dl.WriteFrame(c.st, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = c.st.SetDeadline(time.Now().Add(200 * time.Millisecond))
	r := dl.NewReader(c.st)
	if _, err := r.Next(); err == nil {
		t.Fatal("客户端地址不匹配时不应应答")
	}
}

// SetValue 改值后，Get 必须读到新值。
func TestServerSetValue(t *testing.T) {
	s, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()
	c.aarq()

	if err := s.SetValue(dl.ClassRegister, "1.0.32.7.0.255", dl.AttrValue, dl.U16(2305)); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	ap := c.get(dl.ClassRegister, "1.0.32.7.0.255", dl.AttrValue)
	resp, err := dl.ParseGetResponse(ap)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n, ok := resp.Data.Number(); !ok || n != 2305 {
		t.Fatalf("值 = %v, want 2305", n)
	}

	// Value 也要能读回同一个值。
	v, ok := s.Value(dl.ClassRegister, "1.0.32.7.0.255", dl.AttrValue)
	if !ok {
		t.Fatal("Value 应能读到对象")
	}
	if n, _ := v.Number(); n != 2305 {
		t.Fatalf("Value = %v", n)
	}
	// 不存在的对象：SetValue 必须报错而不是静默创建。
	if err := s.SetValue(dl.ClassRegister, "1.0.99.1.0.255", dl.AttrValue, dl.U16(1)); err == nil {
		t.Fatal("SetValue 对不存在的对象应报错")
	}
}

// WithObjects 替换整张表；WithAddress 改变地址域。
func TestServerOptions(t *testing.T) {
	o, _ := dl.ParseOBIS("1.0.1.8.0.255")
	s := newServer(
		WithAddress(0x11, 0x22, 2),
		WithObjects([]Object{{ClassID: dl.ClassRegister, OBIS: o,
			Attribute: dl.AttrValue, Value: dl.U32(42), Name: "只有一条"}}),
	)
	objs := s.Objects()
	if len(objs) != 1 {
		t.Fatalf("对象数 = %d, want 1", len(objs))
	}
	if objs[0].ClassID != dl.ClassRegister {
		t.Errorf("对象 = %+v", objs[0])
	}
	if s.clientAddr != 0x11 || s.serverAddr != 0x22 || s.addrSize != 2 {
		t.Errorf("地址配置 = client %d server %d size %d", s.clientAddr, s.serverAddr, s.addrSize)
	}
	// 非法值必须被忽略而不是写入。
	s2 := newServer(WithAddress(0x200, 0x8000, 3))
	if s2.clientAddr != 0x10 || s2.serverAddr != 0x01 || s2.addrSize != 1 {
		t.Errorf("非法地址应被忽略，得到 %d/%d/%d", s2.clientAddr, s2.serverAddr, s2.addrSize)
	}
}

// Close 必须幂等，且关闭后 TCP 连接不可再写。
func TestServerCloseIdempotent(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Addr() == "" {
		t.Fatal("Addr 应为监听地址")
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("第一次 Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("第二次 Close 应为 nil: %v", err)
	}
}

// 默认对象表必须覆盖 A-XDR 的主要分支，且 scaler_unit 与 logical_name 齐备。
func TestDefaultObjects(t *testing.T) {
	objs := DefaultObjects()
	if len(objs) < 30 {
		t.Fatalf("默认对象数 = %d，太少了", len(objs))
	}
	var haveScaler, haveClock, haveText, haveOctets bool
	for _, o := range objs {
		switch o.Attribute {
		case dl.AttrScalerUnit:
			if len(o.Value.Elems) == 2 {
				haveScaler = true
			}
		case dl.AttrValue:
			switch o.Value.Kind() {
			case dl.KindDateTime:
				haveClock = true
			case dl.KindString:
				haveText = true
			case dl.KindBytes:
				haveOctets = true
			}
		}
	}
	if !haveScaler {
		t.Error("默认表缺 scaler_unit")
	}
	if !haveClock {
		t.Error("默认表缺 date_time（时钟）")
	}
	if !haveText {
		t.Error("默认表缺 visible-string")
	}
	if !haveOctets {
		t.Error("默认表缺 octet-string")
	}
}

// 每个 Register 对象都必须同时有 attribute 1/2/3：缺任一项会让主站
// 的 scaler 缓存或 logical_name 校验落空。
func TestDefaultObjectsComplete(t *testing.T) {
	objs := DefaultObjects()
	byKey := map[string]Object{}
	for _, o := range objs {
		byKey[objKey(o.ClassID, o.OBIS, o.Attribute)] = o
	}
	for _, o := range objs {
		if o.ClassID != dl.ClassRegister {
			continue
		}
		for _, attr := range []int8{dl.AttrLogicalName, dl.AttrValue, dl.AttrScalerUnit} {
			if _, ok := byKey[objKey(o.ClassID, o.OBIS, attr)]; !ok {
				t.Errorf("%s 缺 attribute %d", o.OBIS, attr)
			}
		}
	}
}

// sortObjects 供自检命令稳定输出，必须按 OBIS / class / attribute 排序。
func TestSortObjects(t *testing.T) {
	a, _ := dl.ParseOBIS("1.0.1.8.0.255")
	b, _ := dl.ParseOBIS("0.0.96.1.0.255")
	in := []Object{
		{ClassID: dl.ClassRegister, OBIS: a, Attribute: 3},
		{ClassID: dl.ClassRegister, OBIS: a, Attribute: 2},
		{ClassID: dl.ClassData, OBIS: b, Attribute: 2},
	}
	out := sortObjects(in)
	if out[0].OBIS != b {
		t.Errorf("首项 = %s, want %s", out[0].OBIS, b)
	}
	if out[1].Attribute != 2 || out[2].Attribute != 3 {
		t.Errorf("同 OBIS 内未按 attribute 排序: %d, %d", out[1].Attribute, out[2].Attribute)
	}
}

var _ = errors.Is
