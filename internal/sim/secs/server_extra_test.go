package secs_test

import (
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	secssim "uic/internal/sim/secs"
)

// 仿真器的自检面：命名、默认值、地址、选项、事件推送的编码。

func TestServerNameAndAddr(t *testing.T) {
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Addr() == "" {
		t.Fatal("Addr 不能为空")
	}
	if srv.Name() == "" {
		t.Fatal("默认应有名字")
	}

	named, err := secssim.NewServerWithOptions("127.0.0.1:0", secssim.Options{Name: "EQP-01"})
	if err != nil {
		t.Fatal(err)
	}
	defer named.Close()
	if named.Name() != "EQP-01" {
		t.Fatalf("Name = %q", named.Name())
	}
}

// 监听失败要报错，而不是返回一个不能用的 Server。
func TestServerListenError(t *testing.T) {
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// 同一个地址再监听一次必然失败。
	if _, err := secssim.NewServer(srv.Addr()); err == nil {
		t.Fatal("重复监听应报错")
	}
}

// 事件推送是 S6F11：驱动必须能把它当「不是我等的应答」忽略掉。
// 这里只验证编码与线格式，驱动侧的忽略行为由 driver 包钉住。
func TestServerPushEventWire(t *testing.T) {
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	srv.PushEvent(100, secscodec.Report{
		ReportID: 1,
		Values: []secscodec.StatusValue{
			{ID: 1, Value: secscodec.Int(secscodec.FormatU4, 25)},
		},
	})
	m := cl.recv()
	if !m.IsData() || m.Stream != 6 || m.Function != 11 {
		t.Fatalf("推送不是 S6F11: %s", m.String())
	}
	if m.WBit {
		t.Fatal("事件推送不应带 W 位（设备不期待应答）")
	}
	d, err := secscodec.ParseData(m)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := secscodec.DecodeEventReport(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.CEID != 100 || len(ev.Reports) != 1 || ev.Reports[0].ReportID != 1 {
		t.Fatalf("事件 = %+v", ev)
	}
}

// 自定义变量后 S1F11 返回新名字：仿真器对外呈现的就是它自己的变量表。
func TestServerCustomVariableVisible(t *testing.T) {
	srv := newSim(t)
	srv.SetVIDNamed(20, secscodec.Float(secscodec.FormatF8, 1.25), "Pressure", "kPa")

	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))
	cl.send(secscodec.DataMessage{
		Stream: 1, Function: 11, WBit: true,
		Body: must(secscodec.EncodeIDList([]uint32{20})),
	}.ToMessage())
	d, err := secscodec.ParseData(cl.recv())
	if err != nil {
		t.Fatal(err)
	}
	vars, err := secscodec.DecodeStatusNameReply(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 1 || vars[0].Name != "Pressure" || vars[0].Units != "kPa" {
		t.Fatalf("变量 = %+v", vars)
	}

	// 值也能读回来，且是 F8。
	cl.send(secscodec.DataMessage{
		Stream: 1, Function: 3, WBit: true,
		Body: must(secscodec.EncodeIDList([]uint32{20})),
	}.ToMessage())
	d, _ = secscodec.ParseData(cl.recv())
	vals, err := secscodec.DecodeStatusReply(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0].Value.Format != secscodec.FormatF8 {
		t.Fatalf("取值 = %+v", vals)
	}
}

// TestServerRejectWritesKeepsAck 模拟「写被 ACK 接受但值不生效」的设备：
// S2F16 仍回 ACKC7=0，但编号的值保持不变。自检命令的写后重读确认依赖
// 这种设备来验证「OK 不等于生效」这条判定真的会失败——若这条设备不存在，
// verify 分支就没有任何测试覆盖，等于一个开关被打开了却从没有人按过。
func TestServerRejectWritesKeepsAck(t *testing.T) {
	srv, err := secssim.NewServerWithOptions("127.0.0.1:0", secssim.Options{RejectWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	// S2F15：把 ECID 10 改成 999（默认是 300）。
	body, _ := secscodec.EncodeConstantSend([]secscodec.Constant{
		{ID: 10, Value: secscodec.Int(secscodec.FormatU4, 999)},
	})
	dm := secscodec.DataMessage{Stream: 2, Function: 15, WBit: true, Body: body}
	resp := cl.roundTrip(dm.ToMessage())
	if resp.MsgType != secscodec.MsgTypeDataMsg {
		t.Fatalf("应答类型 = %s", secscodec.TypeName(resp.MsgType))
	}
	ack, err := secscodec.DecodeAck(resp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if ack != 0 {
		t.Fatalf("设备应照回 ACKC7=0（请求被接受），得到 %d", ack)
	}

	// 但读回仍是旧值 300：这正是「OK 不等于生效」。
	got, ok := srv.ValueOf(secscodec.ObjectECID, 10)
	if !ok {
		t.Fatal("ECID 10 应存在")
	}
	v, _ := got.U4()
	if v != 300 {
		t.Fatalf("rejectWrites 设备上 ECID 10 = %d，应保留旧值 300", v)
	}
}

// 不存在的 SVID 请求：设备回空列表（S1F4），主站据此判 address_error。
func TestServerUnknownSVIDReturnsEmpty(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))
	cl.send(secscodec.DataMessage{
		Stream: 1, Function: 3, WBit: true,
		Body: must(secscodec.EncodeIDList([]uint32{8888})),
	}.ToMessage())
	d, err := secscodec.ParseData(cl.recv())
	if err != nil {
		t.Fatal(err)
	}
	vals, err := secscodec.DecodeStatusReply(d.Body)
	if err != nil {
		t.Fatalf("空列表应能解码: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("未知编号不该有值: %+v", vals)
	}
}

// 空闲连接不会被仿真器自己掐断：设备侧不能因为「主站两秒没说话」就断链。
// 这一条钉住 SetDeadline → SetWriteDeadline 的修正。
func TestServerIdleConnectionSurvives(t *testing.T) {
	srv := newSim(t)
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	time.Sleep(2500 * time.Millisecond) // 超过旧实现里的 2 秒

	cl.send(secscodec.NewLinktestReq(0))
	if m := cl.recv(); secscodec.TypeName(m.MsgType) != "LinktestRsp" {
		t.Fatalf("空闲 2.5 秒后链路应仍可用，收到 %s", secscodec.TypeName(m.MsgType))
	}
}
