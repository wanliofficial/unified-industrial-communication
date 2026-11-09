package secs_test

import (
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	secssim "uic/internal/sim/secs"
)

func newSim(t *testing.T) *secssim.Server {
	t.Helper()
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// S1F11 的两种语义：给编号就回那几个，给空列表就回全部。设备对不认识的
// 编号不回行——主站要能容忍「请求 3 个回 2 行」。
func TestSimNameList(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	ask := func(ids []uint32) []secscodec.StatusVariable {
		body := must(secscodec.EncodeIDList(ids))
		cl.send(secscodec.DataMessage{Stream: 1, Function: 11, WBit: true, Body: body}.ToMessage())
		d, err := secscodec.ParseData(cl.recv())
		if err != nil {
			t.Fatal(err)
		}
		if d.Stream != 1 || d.Function != 12 {
			t.Fatalf("应答是 S%dF%d，want S1F12", d.Stream, d.Function)
		}
		vars, err := secscodec.DecodeStatusNameReply(d.Body)
		if err != nil {
			t.Fatal(err)
		}
		return vars
	}

	got := ask([]uint32{1, 2})
	if len(got) != 2 || got[0].Name != "Temperature" || got[0].Units != "C" {
		t.Fatalf("S1F11{1,2} = %+v", got)
	}
	// 不存在的编号在应答里缺席。
	if got := ask([]uint32{1, 9999}); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("含未知编号时应只回已知项: %+v", got)
	}
	// 空列表 = 全部 SVID（3 个）；ECID 不应混进来。
	if got := ask(nil); len(got) != 3 {
		t.Fatalf("空列表应回全部 SVID（3 个），得到 %d 个: %+v", len(got), got)
	}
}

// S5F1 报警推送：ALCD 的 bit7 表示发生/解除，低 7 位是类别。
func TestSimPushAlarm(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	srv.PushAlarm(secscodec.Alarm{IsSet: true, Kind: 3, ALID: 42, ALTX: "OVERHEAT"})
	m := cl.recv()
	if !m.IsData() || m.Stream != 5 || m.Function != 1 {
		t.Fatalf("推送不是 S5F1: %s", m.String())
	}
	d, err := secscodec.ParseData(m)
	if err != nil {
		t.Fatal(err)
	}
	a, err := secscodec.DecodeAlarmReport(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsSet || a.Kind != 3 || a.ALID != 42 || a.ALTX != "OVERHEAT" {
		t.Fatalf("报警 = %+v", a)
	}

	// 解除报警：bit7 清零，类别仍保留。
	srv.PushAlarm(secscodec.Alarm{IsSet: false, Kind: 3, ALID: 42})
	d, _ = secscodec.ParseData(cl.recv())
	a, err = secscodec.DecodeAlarmReport(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	if a.IsSet || a.Kind != 3 {
		t.Fatalf("解除报警 = %+v", a)
	}
}

// 未选中时不能推数据：仿真与真设备一样，只对 selected 链路推送。
func TestSimPushSkipsUnselected(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())

	// 连上但不 Select：推送应被忽略，之后 Select 也不该收到那条推送。
	srv.PushAlarm(secscodec.Alarm{IsSet: true, Kind: 1, ALID: 1})
	cl.roundTrip(secscodec.NewSelectReq(0))
	cl.send(secscodec.NewLinktestReq(0))
	if m := cl.recv(); secscodec.TypeName(m.MsgType) != "LinktestRsp" {
		t.Fatalf("未选中的推送不应排队，收到 %s", secscodec.TypeName(m.MsgType))
	}
}

// Deselect 之后 DATA 必须重新被 Reject：链路回到未选中状态。
func TestSimDeselectStopsData(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))
	cl.roundTrip(secscodec.NewDeselectReq(0))

	cl.send(secscodec.DataMessage{
		Stream: 1, Function: 3, WBit: true,
		Body: must(secscodec.EncodeIDList([]uint32{1})),
	}.ToMessage())
	if m := cl.recv(); m.MsgType != secscodec.MsgTypeRejectReq {
		t.Fatalf("Deselect 后应 Reject，得到 %s", secscodec.TypeName(m.MsgType))
	}
}

// S1F1 不带 W 位时设备不回 S1F2（那是「通知」，不是「询问」）。
func TestSimOnLineWithoutWBit(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	cl.send(secscodec.DataMessage{Stream: 1, Function: 1, WBit: false}.ToMessage())
	// 紧接着发 Linktest：如果 S1F1 被错误应答，这里读到的会是 S1F2。
	cl.send(secscodec.NewLinktestReq(0))
	if m := cl.recv(); secscodec.TypeName(m.MsgType) != "LinktestRsp" {
		t.Fatalf("S1F1 无 W 位不应应答，收到 %s", secscodec.TypeName(m.MsgType))
	}
}

// 编号表的两套命名空间与自检用的访问器。
func TestSimVariableAccessors(t *testing.T) {
	srv := newSim(t)

	// SVID 1 与 ECID 1 互不影响。
	srv.SetVID(1, secscodec.Int(secscodec.FormatU4, 11))
	srv.SetECID(1, secscodec.Int(secscodec.FormatU4, 22))
	v, ok := srv.Value(1)
	if !ok {
		t.Fatal("Value(1) 应取到 SVID 1")
	}
	if n, _ := v.U4(); n != 11 {
		t.Fatalf("SVID 1 = %d, want 11", n)
	}
	v, ok = srv.ValueOf(secscodec.ObjectECID, 1)
	if !ok {
		t.Fatal("ValueOf(ecid,1) 应取到")
	}
	if n, _ := v.U4(); n != 22 {
		t.Fatalf("ECID 1 = %d, want 22", n)
	}

	if ids := srv.IDs(); len(ids) != 3 { // 默认 1,2,3
		t.Fatalf("SVID 编号 = %v", ids)
	}
	// 默认 10,11，加上这里新增的 ECID 1：三套编号在同一张表里但互不干扰。
	if ids := srv.IDsOf(secscodec.ObjectECID); len(ids) != 3 || ids[2] != 1 {
		t.Fatalf("ECID 编号 = %v", ids)
	}
	if got := srv.SortedIDs(); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("SortedIDs = %v", got)
	}
	// 未定义的编号取不到值。
	if _, ok := srv.Value(500); ok {
		t.Fatal("不存在的编号不应有值")
	}
}

// 计数器是自检输出的依据：请求数随 DATA 增长，写成功数只随 ACK=0 增长。
func TestSimCounters(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	// 控制消息不计入请求数。
	if n := srv.Requests(); n != 0 {
		t.Fatalf("Select 不应计入 DATA 请求数，得到 %d", n)
	}
	cl.send(secscodec.DataMessage{
		Stream: 1, Function: 3, WBit: true, Body: must(secscodec.EncodeIDList([]uint32{1})),
	}.ToMessage())
	cl.recv()
	if n := srv.Requests(); n != 1 {
		t.Fatalf("DATA 请求数 = %d, want 1", n)
	}

	cl.send(secscodec.DataMessage{
		Stream: 2, Function: 15, WBit: true,
		Body: must(secscodec.EncodeConstantSend([]secscodec.Constant{
			{ID: 10, Value: secscodec.Int(secscodec.FormatU4, 1)},
		})),
	}.ToMessage())
	cl.recv()
	if n := srv.Writes(); n != 1 {
		t.Fatalf("成功写入数 = %d, want 1", n)
	}

	// 写未知 ECID：ACK 非 0，写入数不增加。
	cl.send(secscodec.DataMessage{
		Stream: 2, Function: 15, WBit: true,
		Body: must(secscodec.EncodeConstantSend([]secscodec.Constant{
			{ID: 999, Value: secscodec.Int(secscodec.FormatU4, 1)},
		})),
	}.ToMessage())
	d, err := secscodec.ParseData(cl.recv())
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := secscodec.DecodeAck(d.Body); err != nil || ack == 0 {
		t.Fatalf("未知 ECID 应被拒（ACK≠0），得到 %d err=%v", ack, err)
	}
	if n := srv.Writes(); n != 1 {
		t.Fatalf("被拒的写入不应计数，得到 %d", n)
	}
}

// Separate.req 之后设备关闭连接：主站的 Close 不应因此报错。
func TestSimSeparateClosesConnection(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	cl.send(secscodec.NewSeparateReq(0))
	_ = cl.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [1]byte
	if _, err := cl.c.Read(buf[:]); err == nil {
		t.Fatal("Separate 之后连接应被关闭")
	}
	// 重复 Close 幂等。
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("二次 Close: %v", err)
	}
}

// 畸形请求体不能让仿真 panic：设备对无法解析的 DATA 静默丢弃。
func TestSimIgnoresMalformedData(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	// S1F3 的消息体不是 L 项。
	cl.send(secscodec.DataMessage{Stream: 1, Function: 3, WBit: true, Body: []byte{0xA5, 0x01, 0x01}}.ToMessage())
	// 未实现的消息也不应答。
	cl.send(secscodec.DataMessage{Stream: 2, Function: 17, WBit: true}.ToMessage())
	// 链路仍然可用。
	cl.send(secscodec.NewLinktestReq(0))
	if m := cl.recv(); secscodec.TypeName(m.MsgType) != "LinktestRsp" {
		t.Fatalf("畸形请求后链路应仍可用，收到 %s", secscodec.TypeName(m.MsgType))
	}
}
