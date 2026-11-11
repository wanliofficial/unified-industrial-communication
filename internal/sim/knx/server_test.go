package knx

import (
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"

	knxcodec "uic/internal/codec/knx"
	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// newServer 启动一个内置网关，默认关掉周期自增（断言固定值时，后台每秒改
// 一个对象会让测试变成偶发失败）。
func newServer(t *testing.T, opt Options) *Server {
	t.Helper()
	opt.DisableAutoIncrement = true
	return newServerRaw(t, opt)
}

// newServerRaw 保留调用方给的 DisableAutoIncrement（用于测自增本身）。
func newServerRaw(t *testing.T, opt Options) *Server {
	t.Helper()
	s, err := NewServerWithOptions("127.0.0.1:0", opt)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// client 是一个手写的最小隧道客户端。
//
// 刻意不复用驱动：从站测试要验证的正是「驱动做错时从站怎么反应」，用驱动
// 来测等于用被测对象验证自己。
type client struct {
	t    *testing.T
	conn *net.UDPConn
	peer *net.UDPAddr
	ch   uint8
}

func dial(t *testing.T, s *Server) *client {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	peer, err := net.ResolveUDPAddr("udp", s.Addr())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return &client{t: t, conn: pc, peer: peer}
}

func (c *client) send(service uint16, payload []byte) {
	c.t.Helper()
	c.sendRaw(knxcodec.BuildFrame(service, payload))
}

func (c *client) sendRaw(raw []byte) {
	c.t.Helper()
	if _, err := c.conn.WriteToUDP(raw, c.peer); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// recvOne 读一条报文；超时返回 nil（「什么都没回」本身经常就是断言）。
func (c *client) recvOne(timeout time.Duration) knxcodec.Frame {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		c.t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, _, err := c.conn.ReadFromUDP(buf)
	if err != nil {
		return knxcodec.Frame{}
	}
	f, err := knxcodec.ParseFrame(buf[:n])
	if err != nil {
		c.t.Fatalf("parse: %v", err)
	}
	return f
}

// recvN 读恰好 n 条报文。少收到一条就直接失败——这是「网关该发几条」的断言。
func (c *client) recvN(n int, timeout time.Duration) []knxcodec.Frame {
	c.t.Helper()
	out := make([]knxcodec.Frame, 0, n)
	for i := 0; i < n; i++ {
		f := c.recvOne(timeout)
		if f.Service == 0 {
			c.t.Fatalf("wanted %d frames, got %d (timed out waiting for #%d)", n, i, i+1)
		}
		out = append(out, f)
	}
	return out
}

// expectSilence 断言在 timeout 内没有新报文。
func (c *client) expectSilence(timeout time.Duration) {
	c.t.Helper()
	if f := c.recvOne(timeout); f.Service != 0 {
		c.t.Fatalf("expected silence, got service 0x%04X", f.Service)
	}
}

// connect 建立隧道。
func (c *client) connect() knxcodec.ConnectResponse {
	c.t.Helper()
	local, err := knxcodec.NewHPAI(c.conn.LocalAddr().String())
	if err != nil {
		c.t.Fatalf("hpai: %v", err)
	}
	c.send(knxcodec.SvcConnectRequest, knxcodec.ConnectRequest{
		Control: local, Data: local, Layer: knxcodec.LayerLink,
	}.Encode())
	fs := c.recvN(1, 500*time.Millisecond)
	resp, err := knxcodec.ParseConnectResponse(fs[0].Payload)
	if err != nil {
		c.t.Fatalf("parse response: %v", err)
	}
	c.ch = resp.Channel
	return resp
}

// tunnel 发一条 TUNNELLING_REQUEST。
func (c *client) tunnel(seq uint8, cemi []byte) {
	c.t.Helper()
	c.send(knxcodec.SvcTunnellingRequest,
		knxcodec.TunnellingRequest{Channel: c.ch, Seq: seq, CEMI: cemi}.Encode())
}

// read 发一次组地址读，返回收到的三条报文：ACK、L_Data.con、总线指示。
func (c *client) read(seq uint8, group string) (ack knxcodec.TunnellingACK, con, ind knxcodec.LData) {
	c.t.Helper()
	c.tunnel(seq, groupRead(c.t, group))
	fs := c.recvN(3, 500*time.Millisecond)
	return decodeACK(c.t, fs[0]), decodeCEMI(c.t, fs[1]), decodeCEMI(c.t, fs[2])
}

// write 发一次组地址写，返回 ACK、L_Data.con 与总线指示。
func (c *client) write(seq uint8, group string, data []byte, bit bool) (knxcodec.TunnellingACK, knxcodec.LData, knxcodec.LData) {
	c.t.Helper()
	c.tunnel(seq, groupWrite(c.t, group, data, bit))
	fs := c.recvN(3, 500*time.Millisecond)
	return decodeACK(c.t, fs[0]), decodeCEMI(c.t, fs[1]), decodeCEMI(c.t, fs[2])
}

func decodeACK(t *testing.T, f knxcodec.Frame) knxcodec.TunnellingACK {
	t.Helper()
	if f.Service != knxcodec.SvcTunnellingACK {
		t.Fatalf("service = 0x%04X, want TUNNELLING_ACK", f.Service)
	}
	ack, err := knxcodec.ParseTunnellingACK(f.Payload)
	if err != nil {
		t.Fatalf("parse ack: %v", err)
	}
	return ack
}

func decodeCEMI(t *testing.T, f knxcodec.Frame) knxcodec.LData {
	t.Helper()
	if f.Service != knxcodec.SvcTunnellingRequest {
		t.Fatalf("service = 0x%04X, want TUNNELLING_REQUEST", f.Service)
	}
	req, err := knxcodec.ParseTunnellingRequest(f.Payload)
	if err != nil {
		t.Fatalf("parse tunnelling request: %v", err)
	}
	m, err := knxcodec.ParseLData(req.CEMI)
	if err != nil {
		t.Fatalf("parse cEMI: %v", err)
	}
	return m
}

func groupRead(t *testing.T, g string) []byte {
	t.Helper()
	ga, err := knxcodec.ParseGroupAddress(g)
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	cemi, err := knxcodec.BuildLData(knxcodec.MsgLDataReq, 0x11FA, uint16(ga), true,
		knxcodec.APCIGroupValueRead, nil, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return cemi
}

func groupWrite(t *testing.T, g string, data []byte, bit bool) []byte {
	t.Helper()
	ga, err := knxcodec.ParseGroupAddress(g)
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	cemi, err := knxcodec.BuildLData(knxcodec.MsgLDataReq, 0x11FA, uint16(ga), true,
		knxcodec.APCIGroupValueWrite, data, bit)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return cemi
}

// ---------------------------------------------------------------------------
// 隧道建立
// ---------------------------------------------------------------------------

func TestConnectAssignsChannel(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	resp := c.connect()
	if resp.Status != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", resp.Status)
	}
	if resp.Channel == 0 {
		t.Fatal("channel 0 is reserved for errors")
	}
	if !resp.HasCRD {
		t.Fatal("the gateway must hand out a physical address")
	}
	// 分配的地址不能是 0.0.0：那会被总线当成「由网关替换」。
	if resp.Assigned == 0 {
		t.Fatal("assigned address = 0.0.0")
	}
	if n := s.Tunnels(); n != 1 {
		t.Fatalf("tunnels = %d", n)
	}
}

func TestConnectRefusesWhenSlotsAreFull(t *testing.T) {
	s := newServer(t, Options{MaxTunnels: 1})
	if resp := dial(t, s).connect(); resp.Status != knxcodec.StatusNoError {
		t.Fatalf("first status = 0x%02X", resp.Status)
	}
	resp := dial(t, s).connect()
	if resp.Status != knxcodec.StatusNoMoreConnections {
		t.Fatalf("status = 0x%02X, want 0x%02X", resp.Status, knxcodec.StatusNoMoreConnections)
	}
	if n := s.Refusals(); n != 1 {
		t.Fatalf("refusals = %d", n)
	}
}

func TestConnectRefusesNonTunnelType(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	local, _ := knxcodec.NewHPAI(c.conn.LocalAddr().String())
	// 手工拼 CRI：codec 的 ParseConnectRequest 本来就拒绝非隧道类型，因此
	// 这里绕过它，验证从站在 CRI 不合法时是「明确拒绝」而不是「静默丢弃」。
	payload := knxcodec.AppendHPAI(nil, local)
	payload = knxcodec.AppendHPAI(payload, local)
	payload = append(payload, 4, knxcodec.ConnTypeDeviceMgmt, knxcodec.LayerLink, 0x00)
	c.send(knxcodec.SvcConnectRequest, payload)
	fs := c.recvN(1, 500*time.Millisecond)
	resp, err := knxcodec.ParseConnectResponse(fs[0].Payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Status != knxcodec.StatusConnectionType {
		t.Fatalf("status = 0x%02X, want 0x%02X", resp.Status, knxcodec.StatusConnectionType)
	}
	if n := s.Tunnels(); n != 0 {
		t.Fatalf("a refused connection must not occupy a slot, tunnels = %d", n)
	}
}

func TestDisconnectReleasesSlot(t *testing.T) {
	s := newServer(t, Options{MaxTunnels: 1})
	c := dial(t, s)
	c.connect()
	local, _ := knxcodec.NewHPAI(c.conn.LocalAddr().String())
	c.send(knxcodec.SvcDisconnectRequest,
		knxcodec.DisconnectRequest{Channel: c.ch, Control: local}.Encode())
	fs := c.recvN(1, 500*time.Millisecond)
	if fs[0].Service != knxcodec.SvcDisconnectResponse {
		t.Fatalf("service = 0x%04X", fs[0].Service)
	}
	resp, err := knxcodec.ParseDisconnectResponse(fs[0].Payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Status != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", resp.Status)
	}
	if n := s.Tunnels(); n != 0 {
		t.Fatalf("tunnels = %d", n)
	}
	// 槽位已释放：第二条隧道应该能连上。
	if resp := dial(t, s).connect(); resp.Status != knxcodec.StatusNoError {
		t.Fatalf("second status = 0x%02X", resp.Status)
	}
}

func TestDisconnectUnknownChannel(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	local, _ := knxcodec.NewHPAI(c.conn.LocalAddr().String())
	c.send(knxcodec.SvcDisconnectRequest,
		knxcodec.DisconnectRequest{Channel: 0x42, Control: local}.Encode())
	fs := c.recvN(1, 500*time.Millisecond)
	resp, _ := knxcodec.ParseDisconnectResponse(fs[0].Payload)
	if resp.Status != knxcodec.StatusConnectionID {
		t.Fatalf("status = 0x%02X", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// 通道状态
// ---------------------------------------------------------------------------

func TestConnectionStateAliveAndReclaimed(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	local, _ := knxcodec.NewHPAI(c.conn.LocalAddr().String())

	ask := func() uint8 {
		c.send(knxcodec.SvcConnectionStateReq,
			knxcodec.ConnectionStateRequest{Channel: c.ch, Control: local}.Encode())
		fs := c.recvN(1, 500*time.Millisecond)
		resp, err := knxcodec.ParseConnectionStateResponse(fs[0].Payload)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return resp.Status
	}
	if got := ask(); got != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", got)
	}
	s.ExpireTunnels()
	if got := ask(); got != knxcodec.StatusConnectionID {
		t.Fatalf("after reclaim: status = 0x%02X, want 0x%02X", got, knxcodec.StatusConnectionID)
	}
}

// ---------------------------------------------------------------------------
// 序号
// ---------------------------------------------------------------------------

func TestSequenceIsCheckedStrictly(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()

	// 期望 0，发 1：必须被拒，且不处理报文。
	c.tunnel(1, groupRead(t, "2/1/1"))
	ack := decodeACK(t, c.recvN(1, 500*time.Millisecond)[0])
	if ack.Status != knxcodec.StatusSequenceNumber {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	// 拒绝之后什么都不该跟来。
	c.expectSilence(150 * time.Millisecond)
	if n := s.SeqErrors(); n != 1 {
		t.Fatalf("seqErrors = %d", n)
	}
	if n := s.Reads(); n != 0 {
		t.Fatalf("a rejected frame must not be executed, reads = %d", n)
	}

	// 从站没有因为一次错乱而推进期望值：序号 0 仍然可用。
	_, _, ind := c.read(0, "2/1/1")
	if ind.APCI != knxcodec.APCIGroupValueResponse {
		t.Fatalf("apci = 0x%04X", ind.APCI)
	}
	if n := s.Reads(); n != 1 {
		t.Fatalf("reads = %d", n)
	}
}

func TestRetransmissionIsAckedButNotRepeated(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()

	write := groupWrite(t, "5/1/1", []byte{0x22, 0xB8}, false) // 13.010 = 8888
	c.tunnel(0, write)
	fs := c.recvN(3, 500*time.Millisecond)
	if ack := decodeACK(t, fs[0]); ack.Status != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	if n := s.Writes(); n != 1 {
		t.Fatalf("writes = %d", n)
	}
	before, _ := s.Payload("5/1/1")

	// 同一序号再发一次：从站必须重发 ACK，但不能把写入再执行一遍。
	c.tunnel(0, write)
	ack := decodeACK(t, c.recvN(1, 500*time.Millisecond)[0])
	if ack.Status != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	if n := s.Writes(); n != 1 {
		t.Fatalf("a retransmission must not be executed twice, writes = %d", n)
	}
	if n := s.Retransmits(); n != 1 {
		t.Fatalf("retransmits = %d", n)
	}
	after, _ := s.Payload("5/1/1")
	if after.Bit != before.Bit || string(after.Data) != string(before.Data) {
		t.Fatalf("payload changed on a retransmission: %x → %x", before.Data, after.Data)
	}
	// 重传只回 ACK：它不该再发一条 con 或总线指示。
	c.expectSilence(150 * time.Millisecond)
}

func TestSequenceWrapsAtEightBits(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	// 序号 255 之后必须绕回 0，而不是溢出报错。逐帧只读 ACK，不排空后面
	// 的两条报文，否则每一次往返都要多等一个超时。
	for i := 0; i < 256; i++ {
		c.tunnel(uint8(i), groupRead(t, "1/1/1"))
		fs := c.recvN(3, 500*time.Millisecond)
		if ack := decodeACK(t, fs[0]); ack.Status != knxcodec.StatusNoError {
			t.Fatalf("seq %d rejected: 0x%02X", i, ack.Status)
		}
	}
	if n := s.SeqErrors(); n != 0 {
		t.Fatalf("seqErrors = %d, want 0 (255 → 0 must wrap cleanly)", n)
	}
	// 255 处理完之后期望值绕回 0，所以「序号 0」是一次新帧，不是重传；
	// 再发一次同样的序号 0 才是重传。这一对断言钉住的正是 8 位计数器绕回
	// 时「新帧」与「重传」的边界。
	c.read(0, "1/1/1")
	if n := s.Retransmits(); n != 0 {
		t.Fatalf("retransmits = %d, want 0 (seq 0 after 255 is a fresh frame)", n)
	}
	c.tunnel(0, groupRead(t, "1/1/1"))
	ack := decodeACK(t, c.recvN(1, 500*time.Millisecond)[0])
	if ack.Status != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	if n := s.Retransmits(); n != 1 {
		t.Fatalf("retransmits = %d", n)
	}
}

// ---------------------------------------------------------------------------
// 组地址读写
// ---------------------------------------------------------------------------

func TestReadReturnsStoredPayload(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	ack, con, ind := c.read(0, "2/1/1")
	if ack.Status != knxcodec.StatusNoError {
		t.Fatalf("ack status = 0x%02X", ack.Status)
	}
	if con.MsgCode != knxcodec.MsgLDataCon || con.ConfirmationError() {
		t.Fatalf("con = %+v", con)
	}
	if ind.MsgCode != knxcodec.MsgLDataInd {
		t.Fatalf("msg = 0x%02X", ind.MsgCode)
	}
	if !ind.IsGroup || ind.APCI != knxcodec.APCIGroupValueResponse {
		t.Fatalf("frame = %+v", ind)
	}
	// 2/1/1 = 主组 2 << 11 | 中间组 1 << 8 | 子组 1 = 0x1101。
	if ind.Dest != 0x1101 {
		t.Fatalf("dest = 0x%04X", ind.Dest)
	}
	d, _ := knxcodec.ParseDPT("9.001")
	p, err := knxcodec.DecodeGroupValue(ind, d)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, err := d.Decode(p, model.TypeFloat32)
	if err != nil {
		t.Fatalf("value: %v", err)
	}
	if v.Num != 21.5 {
		t.Fatalf("value = %v", v.Num)
	}
}

// 读一个总线上没人拥有的组地址：除 ACK 与链路层确认外没有任何回应。
func TestReadUnknownGroupAddressIsSilent(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	c.tunnel(0, groupRead(t, "9/7/9"))
	fs := c.recvN(2, 500*time.Millisecond) // ACK + L_Data.con
	if ack := decodeACK(t, fs[0]); ack.Status != knxcodec.StatusNoError {
		t.Fatalf("ack status = 0x%02X", ack.Status)
	}
	if con := decodeCEMI(t, fs[1]); con.MsgCode != knxcodec.MsgLDataCon {
		t.Fatalf("msg = 0x%02X", con.MsgCode)
	}
	// 关键：没有任何 GroupValueResponse。客户端会因此超时，而那个超时正是
	// 它应该报告给运维的事实。
	c.expectSilence(150 * time.Millisecond)
	if n := s.Reads(); n != 0 {
		t.Fatalf("reads = %d", n)
	}
}

func TestWriteOneBitUpdatesObject(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	_, con, ind := c.write(0, "1/1/1", nil, true)
	if con.ConfirmationError() {
		t.Fatal("a group-addressed write on a healthy bus must confirm cleanly")
	}
	if ind.MsgCode != knxcodec.MsgLDataInd || ind.APCI != knxcodec.APCIGroupValueWrite || !ind.Bit {
		t.Fatalf("indication = %+v", ind)
	}
	p, ok := s.Payload("1/1/1")
	if !ok || !p.Bit {
		t.Fatalf("payload = %+v ok=%v", p, ok)
	}
	if n := s.Writes(); n != 1 {
		t.Fatalf("writes = %d", n)
	}
}

func TestWriteTwoBytesUpdatesObject(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	d, _ := knxcodec.ParseDPT("9.001")
	pl, err := d.Encode(model.NumValue(model.TypeFloat32, 24.5), model.TypeFloat32)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	c.write(0, "2/1/2", pl.Data, false)
	p, ok := s.Payload("2/1/2")
	if !ok {
		t.Fatal("object missing")
	}
	if len(p.Data) != 2 {
		t.Fatalf("payload = %x", p.Data)
	}
	if binary.BigEndian.Uint16(p.Data) != binary.BigEndian.Uint16(pl.Data) {
		t.Fatalf("payload = %x, want %x", p.Data, pl.Data)
	}
	if p.Data[0] == 0x0C && p.Data[1] == 0x33 {
		t.Fatalf("payload still holds the default 21.5: %x", p.Data)
	}
}

// 写一个从站不认识的组地址：从站不落地、不发指示，但链路层确认仍报正常。
//
// 这是协议里容易搞混的一处：组地址是广播语义，写一个没人订阅的地址在链路
// 层是「成功」的（帧确实上了总线），没有任何设备需要为它改变状态。驱动
// 不能把这种情况当成总线故障。
func TestWriteUnknownGroupAddressIsForwardedButNotStored(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	c.tunnel(0, groupWrite(t, "9/7/7", nil, true))
	fs := c.recvN(2, 500*time.Millisecond) // ACK + L_Data.con
	if con := decodeCEMI(t, fs[1]); con.ConfirmationError() {
		t.Fatal("the link layer must report a clean transmission")
	}
	c.expectSilence(150 * time.Millisecond)
	if n := s.Writes(); n != 0 {
		t.Fatalf("writes = %d", n)
	}
	if _, ok := s.Payload("9/7/7"); ok {
		t.Fatal("the sim must not invent an object for an unknown group address")
	}
}

// 「确认有错」位反映的是总线级故障，不是「有没有人在听」。
func TestFailedConfirmationIsReportedToTheClient(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	s.FailNextConfirms(1)
	_, con, _ := c.write(0, "1/1/1", nil, true)
	if !con.ConfirmationError() {
		t.Fatal("the injected bus failure must surface in L_Data.con")
	}
	// 一次只有一条：下一条恢复正常。
	_, con2, _ := c.write(1, "1/1/1", nil, false)
	if con2.ConfirmationError() {
		t.Fatal("only one confirmation should have been failed")
	}
}

func TestDisableConfirmSuppressesLinkConfirmation(t *testing.T) {
	s := newServer(t, Options{DisableConfirm: true})
	c := dial(t, s)
	c.connect()
	c.tunnel(0, groupWrite(t, "1/1/1", nil, true))
	// 只有 ACK + 总线指示：没有 L_Data.con。
	fs := c.recvN(2, 500*time.Millisecond)
	if ack := decodeACK(t, fs[0]); ack.Status != knxcodec.StatusNoError {
		t.Fatalf("ack status = 0x%02X", ack.Status)
	}
	if m := decodeCEMI(t, fs[1]); m.MsgCode == knxcodec.MsgLDataCon {
		t.Fatal("DisableConfirm must suppress L_Data.con")
	}
	c.expectSilence(150 * time.Millisecond)
	if p, ok := s.Payload("1/1/1"); !ok || !p.Bit {
		t.Fatalf("the write must still land: %+v ok=%v", p, ok)
	}
}

// ---------------------------------------------------------------------------
// 故障注入
// ---------------------------------------------------------------------------

func TestDropNextACKsKeepsTheFrame(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	s.DropNextACKs(1)
	c.tunnel(0, groupRead(t, "1/1/1"))
	// ACK 被丢掉，但 con 与总线指示照常发出：真实 UDP 丢的是单个数据报。
	fs := c.recvN(2, 500*time.Millisecond)
	if fs[0].Service == knxcodec.SvcTunnellingACK {
		t.Fatal("the ACK should have been dropped")
	}
	// 从站已经把序号推进了；重传同一序号必须被识别为重传并重新 ACK。
	c.tunnel(0, groupRead(t, "1/1/1"))
	ack := decodeACK(t, c.recvN(1, 500*time.Millisecond)[0])
	if ack.Status != knxcodec.StatusNoError {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	if n := s.Retransmits(); n != 1 {
		t.Fatalf("retransmits = %d", n)
	}
}

func TestFailNextSequenceRejectsOnce(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	s.FailNextSequence()
	c.tunnel(0, groupRead(t, "1/1/1"))
	ack := decodeACK(t, c.recvN(1, 500*time.Millisecond)[0])
	if ack.Status != knxcodec.StatusSequenceNumber {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	// 只失败一次：同一序号再来就应该成功。
	if _, _, ind := c.read(0, "1/1/1"); ind.APCI != knxcodec.APCIGroupValueResponse {
		t.Fatalf("apci = 0x%04X", ind.APCI)
	}
}

func TestUnknownChannelIsRejected(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	c.connect()
	c.ch = 0x77
	c.tunnel(0, groupRead(t, "1/1/1"))
	ack := decodeACK(t, c.recvN(1, 500*time.Millisecond)[0])
	if ack.Status != knxcodec.StatusConnectionID {
		t.Fatalf("status = 0x%02X", ack.Status)
	}
	// 没有任何后续报文：报文没有被处理。
	c.expectSilence(150 * time.Millisecond)
}

func TestBadDatagramIsCounted(t *testing.T) {
	s := newServer(t, Options{})
	c := dial(t, s)
	// 一个总长度字段与实际不符的报文。
	c.sendRaw([]byte{0x06, 0x10, 0x02, 0x05, 0x00, 0x40, 0x04})
	time.Sleep(80 * time.Millisecond)
	if n := s.BadPackets(); n == 0 {
		t.Fatal("badPackets = 0")
	}
	if n := s.Requests(); n != 0 {
		t.Fatalf("a malformed frame must not count as a request, requests = %d", n)
	}
}

// ---------------------------------------------------------------------------
// 对象表
// ---------------------------------------------------------------------------

func TestDefaultObjectsCoverTheDocumentedAddresses(t *testing.T) {
	s := newServer(t, Options{})
	want := []string{"1/1/1", "1/1/2", "2/1/1", "2/1/2", "3/1/1", "4/1/1", "5/1/1"}
	for _, g := range want {
		if _, ok := s.Payload(g); !ok {
			t.Fatalf("default object %s missing", g)
		}
	}
	if n := len(s.Objects()); n != len(want) {
		t.Fatalf("objects = %d (%v)", n, s.Objects())
	}
	// 每个对象的宽度必须与点表里的 DPT 一致：4/1/1 是 4 字节浮点，
	// 2/1/1 是 2 字节浮点。宽度错了，客户端读回来就是一个荒谬的数。
	widths := map[string]int{"1/1/1": 0, "2/1/1": 2, "3/1/1": 1, "4/1/1": 4, "5/1/1": 4}
	for g, w := range widths {
		p, _ := s.Payload(g)
		if len(p.Data) != w {
			t.Fatalf("%s width = %d, want %d (%x)", g, len(p.Data), w, p.Data)
		}
	}
}

func TestSetValueAndReadBack(t *testing.T) {
	s := newServer(t, Options{})
	if err := s.SetBool("1/1/1", true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if p, _ := s.Payload("1/1/1"); !p.Bit {
		t.Fatalf("payload = %+v", p)
	}
	if err := s.SetNumber("2/1/1", "9.001", -5.25, model.TypeFloat32); err != nil {
		t.Fatalf("set number: %v", err)
	}
	p, _ := s.Payload("2/1/1")
	if len(p.Data) != 2 {
		t.Fatalf("payload = %x", p.Data)
	}
	if err := s.SetPayload("1/2/3", knxcodec.Payload{}); err != nil {
		t.Fatalf("set payload: %v", err)
	}
	if _, ok := s.Payload("1/2/3"); !ok {
		t.Fatal("object missing after SetPayload")
	}
	// 9.001 的上限是 670760：越界值必须报错，而不是编码成一个荒谬的字节。
	if err := s.SetNumber("2/1/1", "9.001", 1e6, model.TypeFloat32); err == nil {
		t.Fatal("a value outside the DPT range must be rejected")
	}
}

func TestSetPayloadRejectsBadGroupAddress(t *testing.T) {
	s := newServer(t, Options{})
	if err := s.SetPayload("1/9/1", knxcodec.Payload{}); err == nil {
		t.Fatal("want an error for an invalid group address")
	}
}

func TestAutoIncrementTicksThePowerObject(t *testing.T) {
	s := newServerRaw(t, Options{DisableAutoIncrement: false})
	before, _ := s.Payload("4/1/1")
	time.Sleep(1300 * time.Millisecond)
	after, _ := s.Payload("4/1/1")
	if len(before.Data) != 4 || len(after.Data) != 4 {
		t.Fatalf("payloads = %x / %x", before.Data, after.Data)
	}
	b := mathFloat32(before.Data)
	a := mathFloat32(after.Data)
	if a != b+10 {
		t.Fatalf("power = %v → %v, want %v", b, a, b+10)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func mathFloat32(b []byte) float32 {
	return math.Float32frombits(binary.BigEndian.Uint32(b))
}
