package iec101

// 本文件测的是内置从站自己。它与 driver_test.go 的分工是：
//
//   - driver_test.go 走「主站 + 从站」全链路，验证两端咬合；
//   - 这里直接按帧驱动从站，验证**从站单方面的责任**：FCB 去重、ACD、
//     召唤的边界与终止类型、命令两段确认、地址宽度。
//
// 为什么值得单独测：从站错了，主站测试也可能「绿」——两端按同一份错误
// 假设实现时，回环是自洽的。用原始字节从外面敲进来，才能发现「只有我方
// 这么理解」的那类错误。

import (
	"context"
	"testing"
	"time"

	ft12 "uic/internal/codec/iec101"
	"uic/internal/codec/iec60870"
	"uic/internal/transport"
)

type simClient struct {
	st     transport.Stream
	rd     *ft12.Reader
	addr   uint16
	addrSz int
	prof   iec60870.Profile
}

// newSim 起一个从站并接上一条客户端连接。
//
// 用真实 TCP 而不是内存管道：ServeStream 的登记/清理、写满、断连都要走
// 一遍，这些在内存管道上不会暴露问题。
func newSim(t *testing.T, opts ...Option) (*Server, *simClient) {
	t.Helper()
	srv, err := NewServer("", opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	st, err := transport.DialTCP(context.Background(), srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	c := &simClient{st: st, addr: srv.link, addrSz: srv.lsize, prof: srv.prof}
	c.rd = ft12.NewReader(st, srv.lsize)
	return srv, c
}

func (c *simClient) deadline(t *testing.T) {
	t.Helper()
	if d, ok := c.st.(interface{ SetDeadline(time.Time) error }); ok {
		if err := d.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}
	}
}

func (c *simClient) fixed(t *testing.T, fc byte, prm bool) {
	t.Helper()
	raw, err := ft12.BuildFixed(ft12.Control{PRM: prm, Func: fc}, c.addr, c.addrSz)
	if err != nil {
		t.Fatalf("build fixed: %v", err)
	}
	c.deadline(t)
	if err := c.st.Write(raw); err != nil {
		t.Fatalf("write fixed: %v", err)
	}
}

// user 发一条带用户数据的帧。fcb 与 fcv 分开给，是为了能构造「重传」：
// 重传必须复用同一组帧字节，也就是同样的 FCB。
func (c *simClient) user(t *testing.T, a iec60870.ASDU, fcb, fcv bool) {
	t.Helper()
	asdu, err := iec60870.BuildASDUProfile(a, c.prof)
	if err != nil {
		t.Fatalf("build asdu: %v", err)
	}
	raw, err := ft12.BuildVariable(ft12.Control{
		PRM: true, FCB: fcb, FCV: fcv, Func: ft12.FCSendConfirm,
	}, c.addr, c.addrSz, asdu)
	if err != nil {
		t.Fatalf("build variable: %v", err)
	}
	c.deadline(t)
	if err := c.st.Write(raw); err != nil {
		t.Fatalf("write variable: %v", err)
	}
}

func (c *simClient) next(t *testing.T) ft12.Frame {
	t.Helper()
	c.deadline(t)
	fr, err := c.rd.Next()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return fr
}

// asdu 取出可变帧里的应用层报文并解析。
func (c *simClient) asdu(t *testing.T, fr ft12.Frame) iec60870.ASDU {
	t.Helper()
	if fr.Kind != ft12.KindVariable {
		t.Fatalf("frame kind = %v, want variable", fr.Kind)
	}
	a, err := iec60870.ParseASDUProfile(fr.Payload, c.prof)
	if err != nil {
		t.Fatalf("parse asdu: %v", err)
	}
	return a
}

// hello 用一次链路状态请求把连接「敲实」。
//
// 从站是在 accept 之后才把连接登记进 conns 的，登记完成前 SetValue 的
// 自发上送会入到空集合里。写完第一个往返之后，登记一定已经发生。
func (c *simClient) hello(t *testing.T) {
	t.Helper()
	c.fixed(t, ft12.FCRequestLinkStatus, true)
	if fr := c.next(t); fr.Ctrl.Func != ft12.FCLinkStatus {
		t.Fatalf("link status reply = %+v", fr)
	}
}

// ---------------------------------------------------------------------------
// 链路过程
// ---------------------------------------------------------------------------

func TestResetLinkAndLinkStatus(t *testing.T) {
	_, c := newSim(t)

	c.fixed(t, ft12.FCResetLink, true)
	fr := c.next(t)
	if fr.Kind != ft12.KindFixed || fr.Ctrl.PRM {
		t.Fatalf("reset reply = %+v", fr)
	}
	if fr.Ctrl.Func != ft12.FCAck {
		t.Fatalf("reset reply func = %d, want ACK(0)", fr.Ctrl.Func)
	}

	c.fixed(t, ft12.FCRequestLinkStatus, true)
	fr = c.next(t)
	if fr.Ctrl.Func != ft12.FCLinkStatus {
		t.Fatalf("link status func = %d, want 11", fr.Ctrl.Func)
	}

	// 没有数据时默认回单字节确认：串口上这比一个固定帧省 4 个字节，
	// 也是现场最常见的应答形态。
	c.fixed(t, ft12.FCRequestClass1, true)
	fr = c.next(t)
	if fr.Kind != ft12.KindSingle || fr.Byte != ft12.AckByte {
		t.Fatalf("empty class 1 reply = %+v, want single E5", fr)
	}
}

func TestNoDataFixedFrame(t *testing.T) {
	_, c := newSim(t, WithNoDataFixed(true))

	c.fixed(t, ft12.FCRequestClass2, true)
	fr := c.next(t)
	if fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCNoData {
		t.Fatalf("reply = %+v, want fixed FC=9", fr)
	}
}

// 未知功能码必须回否定确认，而不是静默。静默在主站侧表现为超时，
// 现场会被误判成「串口参数不对」。
func TestUnknownFunctionIsNacked(t *testing.T) {
	_, c := newSim(t)

	c.fixed(t, 0x0C, true)
	fr := c.next(t)
	if fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCNack {
		t.Fatalf("reply = %+v, want NACK", fr)
	}
}

// 从站自己发出的固定帧（PRM=0）不应触发任何应答——那在 485 上就是
// 两台从站互相抢总线。
func TestSlaveFixedRequestIgnored(t *testing.T) {
	srv, c := newSim(t)
	c.fixed(t, ft12.FCResetLink, false)

	c.deadline(t)
	if _, err := c.rd.Next(); err == nil {
		t.Fatal("a PRM=0 fixed frame from the peer must not be answered")
	}
	if srv.Writes() != 0 {
		t.Fatalf("writes = %d", srv.Writes())
	}
}

// ---------------------------------------------------------------------------
// 总召唤
// ---------------------------------------------------------------------------

// TestInterrogationSnapshot 验证「快照 + 终止」的完整形态。
//
// 三个断言各自对应一个现场故障：
//   - 对象集合：纯命令对象（4001..4003）与累计量都不该出现在站召唤里；
//   - ACD：最后一条数据之后必须清零，否则主站会一直问下去；
//   - 终止报文：用类型 100 + 激活终止，而不是靠超时判断结束。
func TestInterrogationSnapshot(t *testing.T) {
	srv, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeInterrogation, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{Qualifier: iec60870.QOIStation}},
	}, true, true)

	// 第一帧是链路确认。它此时就应当带 ACD：数据已经排进队列。
	fr := c.next(t)
	if fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCAck {
		t.Fatalf("link confirm = %+v", fr)
	}
	if !fr.Ctrl.ACD {
		t.Fatal("ACD must be set when the snapshot is queued")
	}

	var ioas []uint32
	var lastACD bool
	for i := 0; i < 64; i++ {
		c.fixed(t, ft12.FCRequestClass1, true)
		fr = c.next(t)
		if fr.Kind == ft12.KindSingle {
			t.Fatalf("unexpected end of data at %d objects", len(ioas))
		}
		a := c.asdu(t, fr)
		if a.TypeID == iec60870.TypeInterrogation && a.Cause == iec60870.CauseActivationTerm {
			lastACD = fr.Ctrl.ACD
			break
		}
		if a.Cause != iec60870.CauseInterrogated {
			t.Fatalf("object %d cause = %s, want interrogated by station", i, iec60870.CauseName(a.Cause))
		}
		ioas = append(ioas, a.Objects[0].IOA)
		if !fr.Ctrl.ACD && len(ioas) < 9 {
			t.Fatalf("ACD cleared after %d objects, want it kept until the last", len(ioas))
		}
	}
	if len(ioas) != 9 {
		t.Fatalf("snapshot has %d objects (%v), want 9 (commands and counters excluded)", len(ioas), ioas)
	}
	for i := 1; i < len(ioas); i++ {
		if ioas[i] <= ioas[i-1] {
			t.Fatalf("snapshot is not sorted: %v", ioas)
		}
	}
	if lastACD {
		t.Fatal("ACD must be clear on the final frame")
	}
	if _, ok := srv.Info(4001); !ok {
		t.Fatal("command object must still exist in the table")
	}

	// 终止之后队列为空：再问一次应当拿到单字节确认。
	c.fixed(t, ft12.FCRequestClass1, true)
	if fr := c.next(t); fr.Kind != ft12.KindSingle {
		t.Fatalf("queue not drained: %+v", fr)
	}
}

// TestCounterInterrogationTerminatesWithType101 是这组测试里最容易写错的一条。
//
// 电能量召唤（类型 101）的终止报文必须也是 101。用 100 终止，主站会把
// 「电能量召唤结束」当成「站总召唤结束」，然后提前收工——症状是总召唤
// 只回来头几个点，而测试如果只断言「收到终止」，仍然会绿。
func TestCounterInterrogationTerminatesWithType101(t *testing.T) {
	_, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeCounterInterrogation, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{Qualifier: 5}},
	}, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}

	var sawCounter bool
	for i := 0; i < 8; i++ {
		c.fixed(t, ft12.FCRequestClass1, true)
		a := c.asdu(t, c.next(t))
		switch {
		case a.TypeID == iec60870.TypeCounterInterrogation:
			if a.Cause != iec60870.CauseActivationTerm {
				t.Fatalf("counter termination cause = %s", iec60870.CauseName(a.Cause))
			}
			if !sawCounter {
				t.Fatal("termination before any counter object")
			}
			return
		case a.TypeID == iec60870.TypeIntegratedTotals:
			if a.Objects[0].IOA != 3001 {
				t.Fatalf("counter IOA = %d", a.Objects[0].IOA)
			}
			sawCounter = true
		case a.TypeID == iec60870.TypeInterrogation:
			t.Fatal("counter interrogation must not terminate with type 100")
		default:
			t.Fatalf("unexpected ASDU type %s", iec60870.TypeName(a.TypeID))
		}
	}
	t.Fatal("no termination within 8 frames")
}

// ---------------------------------------------------------------------------
// FCB 去重
// ---------------------------------------------------------------------------

// TestFCBRetransmissionExecutesOnce 是「开关被拉合两次」这个现场故障的
// 回归测试：主站没收到确认就会原样重发（FCB 不变），从站必须只执行一次。
func TestFCBRetransmissionExecutesOnce(t *testing.T) {
	srv, c := newSim(t)

	cmd := iec60870.ASDU{
		TypeID: iec60870.TypeSingleCommand, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{IOA: 4001, Bool: true}},
	}
	c.user(t, cmd, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCAck {
		t.Fatalf("first link confirm = %+v", fr)
	}
	// 重传：同样的 FCB，同样的字节。
	c.user(t, cmd, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCAck {
		t.Fatalf("retransmission must still be acknowledged: %+v", fr)
	}
	if n := srv.Writes(); n != 1 {
		t.Fatalf("writes = %d, want 1 (retransmission must not re-execute)", n)
	}

	// FCB 翻转 = 新帧，必须再执行一次。
	c.user(t, cmd, false, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}
	if n := srv.Writes(); n != 2 {
		t.Fatalf("writes = %d, want 2 after FCB toggle", n)
	}

	// 复位链路把 FCB 记忆清掉：重连后第一条命令即使沿用旧 FCB 也必须执行。
	c.fixed(t, ft12.FCResetLink, true)
	if fr := c.next(t); fr.Ctrl.Func != ft12.FCAck {
		t.Fatalf("reset reply = %+v", fr)
	}
	c.user(t, cmd, false, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}
	if n := srv.Writes(); n != 3 {
		t.Fatalf("writes = %d, want 3 after link reset", n)
	}
}

// ---------------------------------------------------------------------------
// 命令
// ---------------------------------------------------------------------------

// TestCommandTwoPhaseAndFeedback 验证三段结果：激活确认、对象状态改变、
// 激活终止，并且监视方向上也回填一条自发上送。
func TestCommandTwoPhaseAndFeedback(t *testing.T) {
	srv, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeDoubleCommand, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{IOA: 2003, Uint: 1}},
	}, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}

	var gotConf, gotTerm, gotSpont bool
	for i := 0; i < 5 && (!gotConf || !gotTerm); i++ {
		c.fixed(t, ft12.FCRequestClass1, true)
		a := c.asdu(t, c.next(t))
		switch {
		case a.TypeID != iec60870.TypeDoubleCommand:
			if a.Cause == iec60870.CauseSpontaneous {
				gotSpont = true
				continue
			}
			t.Fatalf("unexpected ASDU %s cause %s", iec60870.TypeName(a.TypeID), iec60870.CauseName(a.Cause))
		case a.Cause == iec60870.CauseActivationCon:
			gotConf = true
			if a.Negative {
				t.Fatal("activation confirmation must be positive")
			}
		case a.Cause == iec60870.CauseActivationTerm:
			gotTerm = true
		}
	}
	if !gotConf || !gotTerm {
		t.Fatalf("confirm = %v, term = %v", gotConf, gotTerm)
	}
	if !gotSpont {
		t.Fatal("a writable monitored object must report its new state spontaneously")
	}
	info, ok := srv.Info(2003)
	if !ok || info.Num != 1 {
		t.Fatalf("info = %+v/%v, want double state 1", info, ok)
	}
}

// TestUnknownIOAIsNegative 验证拒绝路径用的是「否定 + 未知 IOA」，而不是
// 一条静默丢弃。主站据此把错误归类成 address 而不是超时。
func TestUnknownIOAIsNegative(t *testing.T) {
	_, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeSingleCommand, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{IOA: 9001, Bool: true}},
	}, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}
	c.fixed(t, ft12.FCRequestClass1, true)
	a := c.asdu(t, c.next(t))
	if !a.Negative || a.Cause != iec60870.CauseUnknownIOA {
		t.Fatalf("asdu = %+v, want negative/unknown-IOA", a)
	}
}

// 只读的监视对象（1001 短浮点）不接受命令：它存在，但不可写。
func TestReadOnlyObjectRejectsCommand(t *testing.T) {
	_, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeSetpointFloat, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{IOA: 1001, Num: 1}},
	}, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}
	c.fixed(t, ft12.FCRequestClass1, true)
	if a := c.asdu(t, c.next(t)); !a.Negative || a.Cause != iec60870.CauseUnknownIOA {
		t.Fatalf("asdu = %+v, want negative", a)
	}
}

// 不认识的应用层类型回「未知类型」。这类报文不算解析失败：帧是好的，
// 只是本从站不实现这个方向。
func TestUnknownTypeIsNegative(t *testing.T) {
	srv, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeSinglePoint, Cause: iec60870.CauseSpontaneous, CommonAddr: 1,
		Objects: []iec60870.Object{{IOA: 1001, Bool: true}},
	}, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}
	c.fixed(t, ft12.FCRequestClass1, true)
	if a := c.asdu(t, c.next(t)); !a.Negative || a.Cause != iec60870.CauseUnknownType {
		t.Fatalf("asdu = %+v, want unknown type", a)
	}
	if srv.BadFrames() != 0 {
		t.Fatalf("badFrames = %d, want 0 (a valid frame with an unsupported type is not a parse error)", srv.BadFrames())
	}
}

// 公共地址不符的帧：链路层照样确认（帧本身是好的），但不产生任何数据，
// 并且记一次坏帧。主站看到的是「确认了但没有数据」，从站侧看到的是
// 「收到过、地址不对」——两条信息合起来才够定位。
func TestWrongCommonAddressIsDropped(t *testing.T) {
	srv, c := newSim(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeInterrogation, Cause: iec60870.CauseActivation, CommonAddr: 9,
		Objects: []iec60870.Object{{Qualifier: iec60870.QOIStation}},
	}, true, true)

	fr := c.next(t)
	if fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCAck {
		t.Fatalf("link confirm = %+v", fr)
	}
	if fr.Ctrl.ACD {
		t.Fatal("a frame for another common address must not queue data")
	}
	c.fixed(t, ft12.FCRequestClass1, true)
	if fr := c.next(t); fr.Kind != ft12.KindSingle {
		t.Fatalf("data leaked for another common address: %+v", fr)
	}
	if srv.BadFrames() != 1 {
		t.Fatalf("badFrames = %d, want 1", srv.BadFrames())
	}
}

// ---------------------------------------------------------------------------
// 自发上送与对象表
// ---------------------------------------------------------------------------

// TestSetValueEnqueuesSpontaneous 验证「从站被点名才说话」：SetFloat 只
// 入队，主站不问就不会发出去。
func TestSetValueEnqueuesSpontaneous(t *testing.T) {
	srv, c := newSim(t)
	c.hello(t)

	if err := srv.SetFloat(1002, 55.5); err != nil {
		t.Fatalf("SetFloat: %v", err)
	}
	c.fixed(t, ft12.FCRequestClass1, true)
	a := c.asdu(t, c.next(t))
	if a.Cause != iec60870.CauseSpontaneous || a.Objects[0].Num != 55.5 {
		t.Fatalf("asdu = %+v", a)
	}
	if a.Objects[0].IOA != 1002 {
		t.Fatalf("IOA = %d", a.Objects[0].IOA)
	}
}

// 纯命令对象在监视方向不存在：即使被改写也不能自发上送，否则主站会看到
// 一个「自己冒出来的点位」。
func TestCommandObjectIsNotSpontaneous(t *testing.T) {
	srv, c := newSim(t)
	c.hello(t)

	if err := srv.SetValue(4002, Info{Kind: KindFloat, Num: 3}); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	c.fixed(t, ft12.FCRequestClass1, true)
	fr := c.next(t)
	if fr.Kind != ft12.KindSingle {
		t.Fatalf("command object leaked into the monitoring direction: %+v", fr)
	}
}

func TestSetValueUnknownIOA(t *testing.T) {
	srv, _ := newSim(t)
	if err := srv.SetValue(7777, Info{Kind: KindFloat}); err == nil {
		t.Fatal("SetValue on an unknown IOA must fail")
	}
	if err := srv.SetBool(7777, true); err == nil {
		t.Fatal("SetBool on an unknown IOA must fail")
	}
	if _, ok := srv.Info(7777); ok {
		t.Fatal("unknown IOA must not be created by SetValue")
	}
}

// 双点对象用 SetBool 改写时，数值必须同步成 1/2，而不是留下旧的双点值。
func TestSetBoolKeepsDoubleState(t *testing.T) {
	srv, _ := newSim(t)
	if err := srv.SetBool(2003, false); err != nil {
		t.Fatalf("SetBool: %v", err)
	}
	info, _ := srv.Info(2003)
	if info.Num != 1 {
		t.Fatalf("double state = %v, want 1 (off)", info.Num)
	}
	if err := srv.SetBool(2003, true); err != nil {
		t.Fatalf("SetBool: %v", err)
	}
	info, _ = srv.Info(2003)
	if info.Num != 2 {
		t.Fatalf("double state = %v, want 2 (on)", info.Num)
	}
}

func TestSetCounter(t *testing.T) {
	srv, _ := newSim(t)
	if err := srv.SetCounter(3001, 999); err != nil {
		t.Fatalf("SetCounter: %v", err)
	}
	info, _ := srv.Info(3001)
	if info.Counter != 999 {
		t.Fatalf("counter = %d", info.Counter)
	}
}

// ---------------------------------------------------------------------------
// 剖面
// ---------------------------------------------------------------------------

// TestSmallProfile 覆盖「1 字节 IOA」的小剖面。
//
// 默认对象集的 IOA 是四位数，在 1 字节地址空间里根本编码不出去；从站会
// 在组帧时失败并断连，主站只看到一个 EOF。因此这里必须 WithNoDefaults
// 自己 Add——这条测试同时也是那类「莫名其妙断连」的回归。
func TestSmallProfile(t *testing.T) {
	srv, c := newSim(t,
		WithNoDefaults(true),
		WithProfile(iec60870.Profile{OrgSize: 0, CASize: 1, IOASize: 1}),
	)
	srv.Add(Info{IOA: 5, Kind: KindFloat, Num: 1.5})
	srv.Add(Info{IOA: 6, Kind: KindSingle, Bool: true})
	c.hello(t)

	c.user(t, iec60870.ASDU{
		TypeID: iec60870.TypeInterrogation, Cause: iec60870.CauseActivation, CommonAddr: 1,
		Objects: []iec60870.Object{{Qualifier: iec60870.QOIStation}},
	}, true, true)
	if fr := c.next(t); fr.Kind != ft12.KindFixed {
		t.Fatalf("link confirm = %+v", fr)
	}

	var ioas []uint32
	for i := 0; i < 8; i++ {
		c.fixed(t, ft12.FCRequestClass1, true)
		fr := c.next(t)
		a := c.asdu(t, fr)
		if a.TypeID == iec60870.TypeInterrogation && a.Cause == iec60870.CauseActivationTerm {
			break
		}
		ioas = append(ioas, a.Objects[0].IOA)
		// 1 字节 IOA 剖面下，短浮点对象的 ASDU = 4 字节头 + 1 字节 IOA + 5 字节值。
		if a.TypeID == iec60870.TypeMeasuredFloat && len(fr.Payload) != 10 {
			t.Fatalf("1-byte IOA payload = %d bytes, want 10", len(fr.Payload))
		}
	}
	if len(ioas) != 2 || ioas[0] != 5 || ioas[1] != 6 {
		t.Fatalf("snapshot = %v, want [5 6]", ioas)
	}
}

// TestAddresslessLink 覆盖 linkAddressSize=0：固定帧只有 4 字节，解析也
// 必须按 0 字节地址去切。
func TestAddresslessLink(t *testing.T) {
	srv, err := NewServer("", WithLinkAddress(0, 0))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	st, err := transport.DialTCP(context.Background(), srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer st.Close()
	rd := ft12.NewReader(st, 0)

	raw, err := ft12.BuildFixed(ft12.Control{PRM: true, Func: ft12.FCRequestLinkStatus}, 0, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(raw) != 4 {
		t.Fatalf("addressless fixed frame = %d bytes, want 4", len(raw))
	}
	if err := st.Write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	fr, err := rd.Next()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if fr.Kind != ft12.KindFixed || fr.Ctrl.Func != ft12.FCLinkStatus {
		t.Fatalf("reply = %+v", fr)
	}
}

// TestNewServerRejectsBadProfile 保证非法剖面在监听之前就报错，而不是
// 等到第一帧才炸。
func TestNewServerRejectsBadProfile(t *testing.T) {
	if _, err := NewServer("", WithProfile(iec60870.Profile{CASize: 3, IOASize: 2})); err == nil {
		t.Fatal("CA size 3 must be rejected")
	}
	if _, err := NewServer("", WithLinkAddress(1, 3)); err == nil {
		t.Fatal("link address size 3 must be rejected")
	}
}

// Close 必须幂等：调度层会在多个路径上关闭同一连接。
func TestCloseIsIdempotent(t *testing.T) {
	srv, err := NewServer("")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
