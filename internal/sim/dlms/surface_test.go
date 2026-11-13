package dlms

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	dl "uic/internal/codec/dlms"
)

// 本文件补的是从站"对外可观测但不在正常链路里"的那层：TCP 承载、计数器、
// 故障开关、时钟访问器。
//
// 这些接口只服务自检命令与测试（`-scan` 的进度、`sim` 的日志、断言用的
// 计数），但断掉其中任何一个都会让自检给出**看起来正常**的错误结论：
// 计数器不涨会被读成"表没收到请求"，而实际上是我们自己没数。

// TCP 承载：acceptLoop → serveConn → tcpStream 的 ReadFull/Write/Close。
// 前面所有用例都走 net.Pipe，这条专门走真实 socket，覆盖另一端适配器。
func TestServerOverTCP(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	if srv.Addr() == "" {
		t.Fatal("Addr 不应为空")
	}

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// 直接发 SNRM 金帧，收回 UA。这条向量在 codec 层锁过，这里验的是
	// 从站真的把它接上了。
	if _, err := conn.Write([]byte{0x7E, 0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01, 0x7E}); err != nil {
		t.Fatalf("写 SNRM: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("读 UA: %v", err)
	}
	if n == 0 {
		t.Fatal("没收到 UA")
	}
	if buf[0] != dl.Flag {
		t.Errorf("应答首字节 = %02X，应为标志 %02X", buf[0], dl.Flag)
	}
	if srv.Requests() == 0 {
		t.Error("Requests 计数应已增长")
	}
	if snrm, _, _, _ := srv.Counts(); snrm != 1 {
		t.Errorf("SNRM 计数 = %d", snrm)
	}
}

// 断开连接后从站必须清理该连接（conns 不会泄漏）。连完再关，然后
// Close 整个从站不能卡住。
func TestServerConnCleanup(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
	// 给 serveConn 一点时间退出。
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close 耗时 %v，连接清理可能没生效", d)
	}
}

// 故障开关：SetSilent 之后一条请求都不应答，Requests 仍然增长
// （说明"收到了但故意不回"，与"根本没收到"是两件事）。
func TestServerSilentSwitch(t *testing.T) {
	s, st := newPipe(t)
	c := newClient(t, st)

	c.snrm()
	c.aarq()

	s.SetSilent(true)
	before := s.Requests()
	req := dl.GetRequest{Attribute: dl.AttributeDescriptor{
		ClassID: dl.ClassRegister, InstanceID: [6]byte{1, 0, 1, 8, 0, 0xFF}, Attribute: 2}}
	body, err := (dl.Frame{Dest: c.dst, Src: c.src,
		Control: dl.Control{Kind: dl.KindI, NS: int(c.ns), NR: int(c.nr), PF: true},
		Info:    append([]byte(dl.LLCCommand), req.Encode()...)}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := dl.WriteFrame(st, body); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// 设个短截止：静默从站不会有应答。
	_ = st.SetDeadline(time.Now().Add(100 * time.Millisecond))
	rd := dl.NewReader(st)
	if _, err := rd.Next(); err == nil {
		t.Fatal("静默状态下不应有应答")
	}
	if s.Requests() <= before {
		t.Error("静默状态也应统计收到的请求帧")
	}
}

// SetRejectAssociation 让 AARE 带 rejected。自检里用它验证"认证失败"
// 的错误路径：驱动必须报协议错误而不是超时。
func TestServerRejectSwitch(t *testing.T) {
	s, st := newPipe(t)
	c := newClient(t, st)
	c.snrm()

	s.SetRejectAssociation(true)
	ap := c.aarq()
	resp, err := dl.ParseAssociateResponse(ap)
	if err != nil {
		t.Fatalf("ParseAssociateResponse: %v", err)
	}
	if resp.ApplicationOK {
		t.Fatal("应回拒绝")
	}
	if resp.Result != 1 {
		t.Errorf("result = %d，应为 1（rejected-permanent）", resp.Result)
	}
	if _, _, get, _ := s.Counts(); get != 0 {
		t.Errorf("被拒之后不应有 GET（%d）", get)
	}
}

// SetValue 用错的对象定位必须报错而不是静默成功：静默成功会让测试
// 以为"改了值"，实际改的是另一个对象（或什么都没改）。
func TestServerSetValueErrors(t *testing.T) {
	s, _ := newPipe(t)
	if err := s.SetValue(dl.ClassRegister, "1.0.1.8.0", 2, dl.U32(1)); err == nil {
		t.Error("非法 OBIS 应报错")
	}
	if err := s.SetValue(dl.ClassRegister, "1.0.99.1.0.255", 2, dl.U32(1)); err == nil {
		t.Error("不存在的对象应报错")
	}
	if err := s.SetValue(999, "1.0.1.8.0.255", 2, dl.U32(1)); err == nil {
		t.Error("不存在的类应报错")
	}
	// 正确的定位能读回。
	if err := s.SetValue(dl.ClassRegister, "1.0.1.8.0.255", 2, dl.U32(42)); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	v, ok := s.Value(dl.ClassRegister, "1.0.1.8.0.255", 2)
	if !ok {
		t.Fatal("Value 应能找到")
	}
	if n, _ := v.Number(); n != 42 {
		t.Errorf("读回 = %v", n)
	}
	// Value 的失败路径。
	if _, ok := s.Value(dl.ClassRegister, "1.0.99.1.0.255", 2); ok {
		t.Error("不存在的对象不应返回 ok")
	}
	if _, ok := s.Value(dl.ClassRegister, "bad", 2); ok {
		t.Error("非法 OBIS 不应返回 ok")
	}
}

// Objects 返回副本：调用方排序/改字段不能污染从站的内部表。
func TestServerObjectsCopy(t *testing.T) {
	s, _ := newPipe(t)
	a := s.Objects()
	if len(a) == 0 {
		t.Fatal("对象表不应为空")
	}
	a[0].Name = "被改掉了"
	b := s.Objects()
	if b[0].Name == "被改掉了" {
		t.Error("Objects 返回共享切片")
	}
	// sortObjects 也要返回副本。
	sorted := sortObjects(b)
	if len(sorted) != len(b) {
		t.Fatalf("排序后条数 = %d", len(sorted))
	}
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].OBIS.String() > sorted[i].OBIS.String() {
			t.Errorf("第 %d 条未按 OBIS 排序", i)
		}
	}
	sorted[0].Name = "又被改掉了"
	if s.Objects()[0].Name == "又被改掉了" {
		t.Error("sortObjects 改到了内部表")
	}
}

// Clock 只用来给测试构造期望值；它必须跟着真实时间走（从站的时钟对象
// 是固定字节，不要用它来断言"当前时间"）。
func TestServerClock(t *testing.T) {
	s, _ := newPipe(t)
	a := s.Clock()
	time.Sleep(2 * time.Millisecond)
	b := s.Clock()
	if b.Before(a) {
		t.Error("Clock 不应倒退")
	}
	if d := time.Since(a); d > time.Minute {
		t.Errorf("Clock 偏离真实时间 %v", d)
	}
}

// isClosed 的分类决定从站循环何时正常退出。把"对端关闭"误判成错误会在
// 每次客户端断开时打一条假错误日志。
func TestIsClosed(t *testing.T) {
	if isClosed(nil) {
		t.Error("nil 不算关闭")
	}
	if !isClosed(net.ErrClosed) || !isClosed(io.EOF) {
		t.Error("net.ErrClosed / io.EOF 应算关闭")
	}
	if !isClosed(errTimeout{}) {
		t.Error("超时也应算关闭（读截止到了就退出循环）")
	}
	if isClosed(errors.New("随便什么")) {
		t.Error("普通错误不算关闭")
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }
