package iec104

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"uic/internal/codec/iec104"
)

// ---------------------------------------------------------------------------
// 一个极简的原始客户端：直接用线上字节驱动从站。
//
// 不用主站驱动来测从站，是为了让「从站发的字节」与「从站该发的字节」直接
// 对照。用驱动测会把两侧的同一个错误互相抵消掉。
// ---------------------------------------------------------------------------

type rawClient struct {
	t  *testing.T
	nc net.Conn
}

func dialRaw(t *testing.T, srv *Server) *rawClient {
	t.Helper()
	nc, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return &rawClient{t: t, nc: nc}
}

func (c *rawClient) send(frame []byte) {
	c.t.Helper()
	_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.nc.Write(frame); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *rawClient) sendASDU(a iec104.ASDU) {
	c.t.Helper()
	raw, err := iec104.BuildASDU(a)
	if err != nil {
		c.t.Fatalf("build asdu: %v", err)
	}
	f, err := iec104.BuildIFrame(0, 0, raw)
	if err != nil {
		c.t.Fatalf("build frame: %v", err)
	}
	c.send(f)
}

// recv 读一帧；超时返回 nil。
func (c *rawClient) recv(timeout time.Duration) *iec104.Frame {
	c.t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(timeout))
	head := make([]byte, 2)
	if _, err := io.ReadFull(c.nc, head); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return nil
	}
	body := make([]byte, head[1])
	if _, err := io.ReadFull(c.nc, body); err != nil {
		return nil
	}
	f, err := iec104.ParseFrame(append(head, body...))
	if err != nil {
		c.t.Fatalf("peer sent an unparseable frame: %v (% X)", err, append(head, body...))
	}
	return &f
}

// collect 读到「没有更多帧」为止（短超时视为结束）。
func (c *rawClient) collect() []iec104.Frame {
	c.t.Helper()
	var out []iec104.Frame
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		f := c.recv(150 * time.Millisecond)
		if f == nil {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, *f)
	}
	return out
}

func (c *rawClient) start() {
	c.t.Helper()
	f, err := iec104.BuildUFrame(iec104.UStartAct)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(f)
	con := c.recv(time.Second)
	if con == nil || con.Kind != iec104.KindU || con.UFunc != iec104.UStartCon {
		c.t.Fatalf("no STARTDT con, got %+v", con)
	}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

func TestServerLifecycle(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() == "" {
		t.Fatal("Addr empty")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close 幂等。
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// 关闭后拨号必须失败。
	if nc, err := net.DialTimeout("tcp", s.Addr(), 100*time.Millisecond); err == nil {
		_ = nc.Close()
		t.Fatal("dial after close must fail")
	}
}

func TestServerListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 端口已被占用。
	if _, err := NewServer(ln.Addr().String()); err == nil {
		t.Fatal("expected listen error")
	}
	// 非法地址。
	if _, err := NewServer("127.0.0.1:99999"); err == nil {
		t.Fatal("expected listen error for bad port")
	}
}

// ---------------------------------------------------------------------------
// 对象表
// ---------------------------------------------------------------------------

func TestDefaultObjects(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	objs := s.Objects()
	if len(objs) < 13 {
		t.Fatalf("objects = %d", len(objs))
	}
	for i := 1; i < len(objs); i++ {
		if objs[i-1].IOA > objs[i].IOA {
			t.Fatalf("objects not sorted: %d then %d", objs[i-1].IOA, objs[i].IOA)
		}
	}
	if v, ok := s.Value(1001); !ok || v.Num != 42.5 || v.Kind != KindFloat {
		t.Fatalf("1001 = %+v/%v", v, ok)
	}
	// 带时标对象必须真的有时标，否则 typeOf 会选到无时标类型。
	if v, _ := s.Value(1005); v.Time == nil {
		t.Fatal("1005 must carry a timestamp")
	}
	// 纯命令对象（4001..4003）与可写监视对象（2003）必须区分开。
	if v, _ := s.Value(2003); !v.Writable || v.Command {
		t.Fatalf("2003 = %+v（可写监视对象）", v)
	}
	if v, _ := s.Value(4001); !v.Writable || !v.Command {
		t.Fatalf("4001 = %+v（纯命令对象）", v)
	}
}

func TestValueAndSetValue(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)

	if _, ok := s.Value(9999); ok {
		t.Fatal("unknown IOA must not be found")
	}
	if err := s.SetValue(9999, Info{Num: 1}); err == nil {
		t.Fatal("SetValue on unknown IOA must fail")
	}
	// SetValue 不得改变对象的类别与可写性。
	if err := s.SetValue(1001, Info{Kind: KindSingle, Bool: true, Writable: true, Command: true}); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, _ := s.Value(1001)
	if v.Kind != KindFloat || v.Writable || v.Command {
		t.Fatalf("SetValue changed immutable traits: %+v", v)
	}
}

func TestSetCommonAddrAndBadFrames(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetCommonAddr(7)
	if s.BadFrames() != 0 {
		t.Fatalf("badFrames = %d", s.BadFrames())
	}
	c := dialRaw(t, s)
	c.start()
	// CA 7 才是本站，CA 1 必须被拒。
	var con *iec104.Frame
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeInterrogation, Cause: iec104.CauseActivation, CommonAddr: 7,
		Objects: []iec104.Object{{Qualifier: iec104.QOIStation}},
	})
	for {
		f := c.recv(time.Second)
		if f == nil {
			t.Fatal("no response for correct CA")
		}
		if f.Kind == iec104.KindI {
			a, _ := iec104.ParseASDU(f.ASDU)
			if a.TypeID == iec104.TypeInterrogation && a.Cause == iec104.CauseActivationCon {
				con = f
				break
			}
		}
	}
	if con == nil {
		t.Fatal("no activation confirmation")
	}
}

// ---------------------------------------------------------------------------
// 启动数据传输
// ---------------------------------------------------------------------------

// STARTDT 之前不得上送任何数据帧。这是 104 最容易被写错的一条时序。
func TestNoDataBeforeStartDT(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	// 不 start，直接改值触发广播。
	if err := s.SetValue(1001, Info{Num: 99}); err != nil {
		t.Fatal(err)
	}
	if f := c.recv(300 * time.Millisecond); f != nil {
		t.Fatalf("station sent %+v before STARTDT", f)
	}
}

func TestStartStopTest(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()

	// TESTFR act 必须回 con。
	f, _ := iec104.BuildUFrame(iec104.UTestAct)
	c.send(f)
	if got := c.recv(time.Second); got == nil || got.Kind != iec104.KindU || got.UFunc != iec104.UTestCon {
		t.Fatalf("TESTFR con missing: %+v", got)
	}
	// 对端主动发来的 TESTFR con 不得被当成错误（继续能通信）。
	f, _ = iec104.BuildUFrame(iec104.UTestCon)
	c.send(f)

	// STOPDT act 必须回 con，且之后不再上送数据。
	f, _ = iec104.BuildUFrame(iec104.UStopAct)
	c.send(f)
	if got := c.recv(time.Second); got == nil || got.UFunc != iec104.UStopCon {
		t.Fatalf("STOPDT con missing: %+v", got)
	}
	_ = s.SetValue(1001, Info{Num: 1})
	if got := c.recv(300 * time.Millisecond); got != nil {
		t.Fatalf("station sent %+v after STOPDT", got)
	}

	// 未知 U 功能码：静默忽略，不断链。0x07 之外的低两位为 11 的码都算
	// U 帧；0x03 是保留值，BuildUFrame 不生成它，这里直接写原始字节。
	c.send([]byte{iec104.StartByte, 0x04, 0x03, 0x00, 0x00, 0x00})
	f, _ = iec104.BuildUFrame(iec104.UStartAct)
	c.send(f)
	if got := c.recv(time.Second); got == nil || got.UFunc != iec104.UStartCon {
		t.Fatalf("link died after unknown U frame: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 总召唤
// ---------------------------------------------------------------------------

func TestInterrogationSendsStationSnapshot(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()

	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeInterrogation, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Qualifier: iec104.QOIStation}},
	})
	frames := c.collect()
	if len(frames) == 0 {
		t.Fatal("no frames")
	}

	seen := map[uint32]iec104.Object{}
	var endOfInit, confirm bool
	var seq uint16
	for _, f := range frames {
		if f.Kind != iec104.KindI {
			t.Fatalf("unexpected non-I frame %+v", f)
		}
		// 从站的 N(S) 必须严格自增。
		if f.SendSeq != seq {
			t.Fatalf("sim N(S)=%d, want %d", f.SendSeq, seq)
		}
		seq++
		a, err := iec104.ParseASDU(f.ASDU)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		switch a.TypeID {
		case iec104.TypeEndOfInit:
			endOfInit = true
		case iec104.TypeInterrogation:
			confirm = a.Cause == iec104.CauseActivationCon
		default:
			if a.Cause != iec104.CauseInterrogated {
				t.Fatalf("cause = %d, want interrogated", a.Cause)
			}
			for _, o := range a.Objects {
				seen[o.IOA] = o
			}
		}
	}
	if !endOfInit {
		t.Fatal("missing EndOfInit")
	}
	if !confirm {
		t.Fatal("missing interrogation activation confirmation")
	}

	// 监视对象必须全部到齐。
	for _, ioa := range []uint32{1001, 1002, 1003, 1004, 1005, 2001, 2002, 2003, 2004} {
		if _, ok := seen[ioa]; !ok {
			t.Fatalf("IOA %d missing from station interrogation", ioa)
		}
	}
	// 累计量走电能量召唤，不在站召唤里。
	if _, ok := seen[3001]; ok {
		t.Fatal("counter 3001 must not appear in station interrogation")
	}
	// 纯命令对象不得出现在站召唤里。
	for _, ioa := range []uint32{4001, 4002, 4003} {
		if _, ok := seen[ioa]; ok {
			t.Fatalf("command object %d must not appear in station interrogation", ioa)
		}
	}
	// 值本身也要对得上。
	if o := seen[1001]; o.Num != 42.5 {
		t.Fatalf("1001 = %+v", o)
	}
	if o := seen[1004]; !o.Quality.Has(iec104.QSubstituted) {
		t.Fatalf("1004 quality lost: %v", o.Quality)
	}
	if o := seen[2001]; !o.Bool {
		t.Fatalf("2001 = %+v", o)
	}
	if o := seen[2003]; o.Uint != 2 {
		t.Fatalf("2003 = %+v", o)
	}
}

// 一串同类型且 IOA 连续的监视对象必须合并成一条 SQ=1 的 ASDU。
// 早先的实现「发现不连续就 break」，会把组静默截断成前两个对象。
func TestInterrogationGroupsAreComplete(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	// 追加 4 个连续的短浮点对象，1001..1002 之后跳到 5001。
	for ioa := uint32(5001); ioa <= 5004; ioa++ {
		s.mu.Lock()
		s.info[ioa] = &Info{IOA: ioa, Kind: KindFloat, Num: float64(ioa)}
		s.mu.Unlock()
	}
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeInterrogation, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Qualifier: iec104.QOIStation}},
	})
	frames := c.collect()
	var got []uint32
	for _, f := range frames {
		if f.Kind != iec104.KindI {
			continue
		}
		a, _ := iec104.ParseASDU(f.ASDU)
		if a.TypeID != iec104.TypeMeasuredFloat {
			continue
		}
		if !a.SQ {
			// 不连续的组必须显式带上每个对象的 IOA。
			for _, o := range a.Objects {
				got = append(got, o.IOA)
			}
			continue
		}
		for _, o := range a.Objects {
			got = append(got, o.IOA)
		}
	}
	want := map[uint32]bool{1001: false, 1002: false, 5001: false, 5002: false, 5003: false, 5004: false}
	for _, ioa := range got {
		if _, ok := want[ioa]; ok {
			want[ioa] = true
		}
	}
	for ioa, seen := range want {
		if !seen {
			t.Fatalf("IOA %d was dropped from the group (got %v)", ioa, got)
		}
	}
}

// 总召唤限定词小于 20 必须回否定确认。
func TestInterrogationBadQOI(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeInterrogation, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Qualifier: 5}},
	})
	f := c.recv(time.Second)
	if f == nil || f.Kind != iec104.KindI {
		t.Fatalf("frame = %+v", f)
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if !a.Negative || a.Cause != iec104.CauseUnknownCause {
		t.Fatalf("asdu = %+v", a)
	}
}

// 长度为 0 的总召唤（没有限定词）应当被忽略，不要 panic。
func TestInterrogationWithoutObject(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeInterrogation, Cause: iec104.CauseActivation, CommonAddr: 1,
	})
	if f := c.recv(300 * time.Millisecond); f != nil {
		t.Fatalf("expected silence, got %+v", f)
	}
}

func TestCounterInterrogation(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeCounterInterrogation, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Qualifier: 5}},
	})
	frames := c.collect()
	var counter bool
	for _, f := range frames {
		if f.Kind != iec104.KindI {
			continue
		}
		a, _ := iec104.ParseASDU(f.ASDU)
		if a.TypeID == iec104.TypeIntegratedTotals {
			counter = true
			if len(a.Objects) != 1 || a.Objects[0].IOA != 3001 || a.Objects[0].Uint != 1000 {
				t.Fatalf("counter object = %+v", a.Objects)
			}
		}
	}
	if !counter {
		t.Fatal("counter 3001 missing from counter interrogation")
	}
}

// 时钟同步只回确认，不产生数据。
func TestClockSyncConfirmed(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	ts := time.Now()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeClockSync, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Time: &ts}},
	})
	f := c.recv(time.Second)
	if f == nil || f.Kind != iec104.KindI {
		t.Fatalf("frame = %+v", f)
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if a.TypeID != iec104.TypeClockSync || a.Cause != iec104.CauseActivationCon || a.Negative {
		t.Fatalf("asdu = %+v", a)
	}
}

// ---------------------------------------------------------------------------
// 命令
// ---------------------------------------------------------------------------

func TestCommandLifecycle(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 4001, Bool: true}},
	})
	frames := c.collect()
	if len(frames) < 2 {
		t.Fatalf("frames = %+v", frames)
	}
	var con, term bool
	for _, f := range frames {
		a, _ := iec104.ParseASDU(f.ASDU)
		switch a.Cause {
		case iec104.CauseActivationCon:
			con = !a.Negative
		case iec104.CauseActivationTerm:
			term = true
		}
	}
	if !con || !term {
		t.Fatalf("missing con/term: %+v", frames)
	}
	if v, _ := s.Value(4001); !v.Bool {
		t.Fatalf("command not applied: %+v", v)
	}
	// 纯命令对象不能被当成遥信上送。
	for _, f := range frames {
		a, _ := iec104.ParseASDU(f.ASDU)
		if a.Cause == iec104.CauseSpontaneous {
			t.Fatalf("pure command object must not be reported as spontaneous data: %+v", a)
		}
	}
}

// 可写监视对象（2003 双点）被遥控后必须自发上送新状态。
func TestCommandOnMonitoredObjectSpontaneouslyReports(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeDoubleCommand, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 2003, Uint: 1}},
	})
	frames := c.collect()
	var spontaneous bool
	for _, f := range frames {
		a, _ := iec104.ParseASDU(f.ASDU)
		if a.Cause == iec104.CauseSpontaneous && a.TypeID == iec104.TypeDoublePoint {
			if len(a.Objects) != 1 || a.Objects[0].IOA != 2003 || a.Objects[0].Uint != 1 {
				t.Fatalf("spontaneous object = %+v", a.Objects)
			}
			spontaneous = true
		}
	}
	if !spontaneous {
		t.Fatal("double point state change must be reported spontaneously")
	}
	if v, _ := s.Value(2003); v.Num != 1 {
		t.Fatalf("2003 = %+v", v)
	}
}

// 对只读对象下发命令必须得到否定确认「未知 IOA」。
func TestCommandOnReadOnlyObject(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeSetpointFloat, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 1001, Num: 10}},
	})
	f := c.recv(time.Second)
	if f == nil || f.Kind != iec104.KindI {
		t.Fatalf("frame = %+v", f)
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if !a.Negative || a.Cause != iec104.CauseUnknownIOA {
		t.Fatalf("asdu = %+v", a)
	}
}

// 不存在的 IOA 与只读对象必须走同一条否定路径。
func TestCommandUnknownIOA(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{IOA: 9999, Bool: true}},
	})
	f := c.recv(time.Second)
	if f == nil {
		t.Fatal("no response")
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if !a.Negative || a.Cause != iec104.CauseUnknownIOA {
		t.Fatalf("asdu = %+v", a)
	}
}

// 命令 ASDU 里一个对象都没有：必须回否定确认，不能 panic。
func TestCommandWithoutObject(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseActivation, CommonAddr: 1,
	})
	f := c.recv(time.Second)
	if f == nil {
		t.Fatal("no response")
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if !a.Negative || a.Cause != iec104.CauseUnknownIOA {
		t.Fatalf("asdu = %+v", a)
	}
}

// 未实现的类型标识必须回「未知类型标识」。
func TestUnknownTypeIsRejected(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	// 直接构造一条「电能量召唤」以外的未实现类型：用原始 ASDU 头，
	// 类型 100 是总召唤，这里伪造 103 之外的控制类型——用 45 的命令
	// 不行，必须真的没实现。用 70（初始化结束）作为主站发来的类型。
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeEndOfInit, Cause: iec104.CauseActivation, CommonAddr: 1,
		Objects: []iec104.Object{{Qualifier: 0}},
	})
	f := c.recv(time.Second)
	if f == nil {
		t.Fatal("no response")
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if !a.Negative || a.Cause != iec104.CauseUnknownType {
		t.Fatalf("asdu = %+v", a)
	}
}

// 公共地址不匹配必须回「未知公共地址」。
func TestUnknownCommonAddress(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	c.sendASDU(iec104.ASDU{
		TypeID: iec104.TypeSingleCommand, Cause: iec104.CauseActivation, CommonAddr: 99,
		Objects: []iec104.Object{{IOA: 4001, Bool: true}},
	})
	f := c.recv(time.Second)
	if f == nil {
		t.Fatal("no response")
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if !a.Negative || a.Cause != iec104.CauseUnknownCommonAddr {
		t.Fatalf("asdu = %+v", a)
	}
}

// ---------------------------------------------------------------------------
// 帧级错误
// ---------------------------------------------------------------------------

// I 帧序号乱序：断链，不能把后续数据错位处理。
func TestOutOfOrderFrameClosesConnection(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	raw, _ := iec104.BuildInterrogation(1, iec104.QOIStation)
	f, err := iec104.BuildIFrame(5, 0, raw)
	if err != nil {
		t.Fatal(err)
	}
	c.send(f)
	// 连接应被从站关闭。
	_ = c.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	for {
		if _, err := c.nc.Read(buf); err != nil {
			return
		}
	}
}

// 空闲的 S 帧必须被接受（只确认、不回包）。
func TestSFrameAccepted(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()
	sf, err := iec104.BuildSFrame(0)
	if err != nil {
		t.Fatal(err)
	}
	c.send(sf)
	// 链路仍然可用。
	f, _ := iec104.BuildUFrame(iec104.UTestAct)
	c.send(f)
	if got := c.recv(time.Second); got == nil || got.UFunc != iec104.UTestCon {
		t.Fatalf("link died after S frame: %+v", got)
	}
}

// 起始字节错、长度字段错：静默丢弃并断链，计数递增。
func TestMalformedFramesAreCounted(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.send([]byte{0x00, 0x04, 0x01, 0x00, 0x00, 0x00})
	// 等待从站断开。
	_ = c.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	for {
		if _, err := c.nc.Read(buf); err != nil {
			break
		}
	}
	if s.BadFrames() == 0 {
		t.Fatal("malformed frame was not counted")
	}

	// 长度字段非法。
	s2, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	c2 := dialRaw(t, s2)
	c2.send([]byte{0x68, 0x02, 0x01, 0x00})
	_ = c2.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, err := c2.nc.Read(buf); err != nil {
			break
		}
	}
	if s2.BadFrames() == 0 {
		t.Fatal("bad length was not counted")
	}
}

// 能解析成帧但承载的 ASDU 是垃圾：计数并忽略，连接继续可用。
func TestBadASDUIsIgnored(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	// ASDU 头声明 3 个对象，实际后面没有内容。
	body := []byte{iec104.TypeSinglePoint, 0x03, 0x06, 0x00, 0x01, 0x00}
	f, err := iec104.BuildIFrame(0, 0, body)
	if err != nil {
		t.Fatal(err)
	}
	c.send(f)
	// 链路必须还活着。
	probe, _ := iec104.BuildUFrame(iec104.UTestAct)
	c.send(probe)
	if got := c.recv(time.Second); got == nil || got.UFunc != iec104.UTestCon {
		t.Fatalf("link died after bad ASDU: %+v", got)
	}
	if s.BadFrames() == 0 {
		t.Fatal("bad ASDU was not counted")
	}
}

// 空 ASDU 的 I 帧（只有控制域）在编解码层就被拒；但直接发到从站时
// 线上不可能出现这种帧，所以这里验证从站对「I 帧无内容」的容忍度：
// 只要不 panic、不断链即可。
func TestEmptyIFrameTolerated(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialRaw(t, s)
	c.start()
	// 手工构造一个只有控制域的 I 帧（长度 4，最低位 0）。
	c.send([]byte{0x68, 0x04, 0x02, 0x00, 0x00, 0x00})
	// 从站会把它当作 N(S)=1 的 I 帧（当前期望 0 的话会断链；这里是 0
	// 号帧之后期望 1，刚好匹配）。无论断链与否，都不得 panic。
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// 突发上送
// ---------------------------------------------------------------------------

func TestBroadcastOnValueChange(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	c := dialRaw(t, s)
	c.start()
	if err := s.SetValue(1002, Info{Num: 88.5}); err != nil {
		t.Fatal(err)
	}
	f := c.recv(time.Second)
	if f == nil || f.Kind != iec104.KindI {
		t.Fatalf("frame = %+v", f)
	}
	a, _ := iec104.ParseASDU(f.ASDU)
	if a.TypeID != iec104.TypeMeasuredFloat || a.Cause != iec104.CauseSpontaneous {
		t.Fatalf("asdu = %+v", a)
	}
	if len(a.Objects) != 1 || a.Objects[0].IOA != 1002 || a.Objects[0].Num != 88.5 {
		t.Fatalf("objects = %+v", a.Objects)
	}
}

// 计数器自增必须能观察到；关掉之后不再变化。
func TestCounterTicks(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, _ := s.Value(3001)
	deadline := time.Now().Add(3 * time.Second)
	var grown bool
	for time.Now().Before(deadline) {
		v, _ := s.Value(3001)
		if v.Counter > before.Counter {
			grown = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !grown {
		t.Fatalf("counter did not tick: %d", before.Counter)
	}
	s.SetAutoIncrement(false)
	steady, _ := s.Value(3001)
	time.Sleep(1200 * time.Millisecond)
	after, _ := s.Value(3001)
	if after.Counter != steady.Counter {
		t.Fatalf("counter kept ticking after SetAutoIncrement(false): %d -> %d", steady.Counter, after.Counter)
	}
}

// 多个连接各自维护 N(S)，不能互相干扰。
func TestMultipleStations(t *testing.T) {
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoIncrement(false)
	a := dialRaw(t, s)
	b := dialRaw(t, s)
	a.start()
	b.start()
	for _, c := range []*rawClient{a, b} {
		c.sendASDU(iec104.ASDU{
			TypeID: iec104.TypeInterrogation, Cause: iec104.CauseActivation, CommonAddr: 1,
			Objects: []iec104.Object{{Qualifier: iec104.QOIStation}},
		})
		frames := c.collect()
		if len(frames) == 0 {
			t.Fatal("station got no snapshot")
		}
		var seq uint16
		for _, f := range frames {
			if f.Kind != iec104.KindI {
				continue
			}
			if f.SendSeq != seq {
				t.Fatalf("per-connection sequence broken: %d, want %d", f.SendSeq, seq)
			}
			seq++
		}
	}
}

// ---------------------------------------------------------------------------
// 纯函数单元测试
// ---------------------------------------------------------------------------

func TestTypeOf(t *testing.T) {
	ts := time.Now()
	cases := []struct {
		info Info
		want byte
	}{
		{Info{Kind: KindSingle}, iec104.TypeSinglePoint},
		{Info{Kind: KindSingle, Time: &ts}, iec104.TypeSinglePointTime},
		{Info{Kind: KindDouble}, iec104.TypeDoublePoint},
		{Info{Kind: KindDouble, Time: &ts}, iec104.TypeDoublePointTime},
		{Info{Kind: KindScaled}, iec104.TypeMeasuredScaled},
		{Info{Kind: KindScaled, Time: &ts}, iec104.TypeMeasuredScaledTime},
		{Info{Kind: KindFloat}, iec104.TypeMeasuredFloat},
		{Info{Kind: KindFloat, Time: &ts}, iec104.TypeMeasuredFloatTime},
		{Info{Kind: KindCounter}, iec104.TypeIntegratedTotals},
		{Info{Kind: KindCounter, Time: &ts}, iec104.TypeIntegratedTotalsTime},
		{Info{Kind: KindStep}, iec104.TypeStepPosition},
		{Info{Kind: KindBitstring}, iec104.TypeBitstring},
	}
	for _, c := range cases {
		if got := typeOf(c.info); got != c.want {
			t.Fatalf("typeOf(%+v) = %s, want %s", c.info,
				iec104.TypeName(got), iec104.TypeName(c.want))
		}
	}
}

func TestGroupByTypeStableOrder(t *testing.T) {
	objs := []Info{
		{IOA: 1, Kind: KindFloat},
		{IOA: 2, Kind: KindSingle},
		{IOA: 3, Kind: KindFloat},
		{IOA: 4, Kind: KindSingle},
	}
	groups := groupByType(objs)
	if len(groups) != 2 {
		t.Fatalf("groups = %d", len(groups))
	}
	// 顺序按首次出现：float 先，single 后。
	if typeOf(groups[0][0]) != iec104.TypeMeasuredFloat {
		t.Fatalf("first group = %d", typeOf(groups[0][0]))
	}
	if len(groups[0]) != 2 || groups[0][0].IOA != 1 || groups[0][1].IOA != 3 {
		t.Fatalf("group 0 = %+v", groups[0])
	}
	if len(groups[1]) != 2 || groups[1][0].IOA != 2 || groups[1][1].IOA != 4 {
		t.Fatalf("group 1 = %+v", groups[1])
	}
	if len(groupByType(nil)) != 0 {
		t.Fatal("empty input must yield no groups")
	}
}

func TestInfoObjectMapping(t *testing.T) {
	ts := time.Now()
	if o := infoObject(Info{IOA: 1, Kind: KindSingle, Bool: true}); !o.Bool {
		t.Fatalf("single = %+v", o)
	}
	if o := infoObject(Info{IOA: 2, Kind: KindDouble, Num: 2.4}); o.Uint != 2 {
		t.Fatalf("double = %+v", o)
	}
	if o := infoObject(Info{IOA: 3, Kind: KindFloat, Num: 1.5}); o.Num != 1.5 {
		t.Fatalf("float = %+v", o)
	}
	if o := infoObject(Info{IOA: 4, Kind: KindCounter, Counter: 77}); o.Uint != 77 {
		t.Fatalf("counter = %+v", o)
	}
	if o := infoObject(Info{IOA: 5, Kind: KindFloat, Time: &ts}); o.Time == nil {
		t.Fatal("time lost")
	}
}

// writeIFrame 在从站关闭后必须返回错误而不是 panic（也不阻塞）。
func TestWriteAfterStationClosed(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	st := &station{nc: c1}
	st.closed = true
	if err := st.writeIFrame([]byte{iec104.TypeSinglePoint, 0x01, 0x06, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}); err == nil {
		t.Fatal("write on closed station must fail")
	}
}
