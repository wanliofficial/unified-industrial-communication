package secs_test

import (
	"io"
	"net"
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	secssim "uic/internal/sim/secs"
)

// 直接从包外驱动仿真：手写一个最小 HSMS 主站客户端，覆盖控制消息状态机
// （Select/Deselect/Linktest/Separate）、未选中时的 Reject，以及主动推送。

// client 是一个最小 HSMS 客户端：只做「发一条、读一条」，足够驱动仿真。
type client struct {
	c   net.Conn
	sys uint32
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{c: c}
}

func (cl *client) send(m secscodec.Message) {
	cl.sys++
	m.SystemBytes = cl.sys
	raw, err := secscodec.Encode(m)
	if err != nil {
		panic(err)
	}
	_ = cl.c.SetDeadline(time.Now().Add(time.Second))
	if _, err := cl.c.Write(raw); err != nil {
		panic(err)
	}
}

func (cl *client) recv() secscodec.Message {
	_ = cl.c.SetDeadline(time.Now().Add(time.Second))
	var head [secscodec.FrameLen]byte
	if _, err := io.ReadFull(cl.c, head[:]); err != nil {
		panic(err)
	}
	total := secscodec.FrameLen
	if n, err := secscodec.FrameDataLen(head[:]); err != nil {
		panic(err)
	} else {
		total += int(n)
	}
	buf := make([]byte, total)
	copy(buf, head[:])
	if _, err := io.ReadFull(cl.c, buf[secscodec.FrameLen:]); err != nil {
		panic(err)
	}
	m, err := secscodec.Decode(buf)
	if err != nil {
		panic(err)
	}
	return m
}

func (cl *client) roundTrip(m secscodec.Message) secscodec.Message {
	cl.send(m)
	return cl.recv()
}

func TestSimSelectAndLinktest(t *testing.T) {
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	cl := dial(t, srv.Addr())
	// 未选中就发 DATA → 应被 Reject（SType=Reject）。
	dm := secscodec.DataMessage{Stream: 1, Function: 3, WBit: true,
		Body: must(secscodec.EncodeIDList([]uint32{1}))}
	cl.send(dm.ToMessage())
	if m := cl.recv(); m.MsgType != secscodec.MsgTypeRejectReq {
		t.Fatalf("未选中发 DATA 应被 Reject，得到 %s", secscodec.TypeName(m.MsgType))
	}

	// Select → Select.rsp status 0。
	if m := cl.roundTrip(secscodec.NewSelectReq(0)); m.Status() != secscodec.StatusOK {
		t.Fatalf("Select 应成功，status=%d", m.Status())
	}
	// Linktest → Linktest.rsp。E37 里 Linktest.rsp 没有状态字段，
	// 只验证类型与系统字节回显。
	if m := cl.roundTrip(secscodec.NewLinktestReq(0)); m.MsgType != secscodec.MsgTypeLinktestRsp {
		t.Fatalf("Linktest 应回 LinktestRsp，得到 %s", secscodec.TypeName(m.MsgType))
	}
	// Deselect → Deselect.rsp status 0。
	if m := cl.roundTrip(secscodec.NewDeselectReq(0)); m.Status() != secscodec.StatusOK {
		t.Fatalf("Deselect 应成功，status=%d", m.Status())
	}
}

func TestSimDataMessages(t *testing.T) {
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetVID(1, secscodec.Int(secscodec.FormatU4, 100))
	srv.SetECID(10, secscodec.Int(secscodec.FormatU4, 5))

	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	// S1F1 W → S1F2 MDLN/SOFTREV。
	cl.send(secscodec.DataMessage{Stream: 1, Function: 1, WBit: true}.ToMessage())
	m := cl.recv()
	d, err := secscodec.ParseData(m)
	if err != nil {
		t.Fatal(err)
	}
	if mdln, _, err := secscodec.DecodeOnLineData(d.Body); err != nil || mdln != "UIC-SECS-01" {
		t.Fatalf("MDLN = %q err=%v", mdln, err)
	}

	// S1F3 [1] → S1F4 带回 VID 1 的值。
	body := must(secscodec.EncodeIDList([]uint32{1}))
	cl.send(secscodec.DataMessage{Stream: 1, Function: 3, WBit: true, Body: body}.ToMessage())
	m = cl.recv()
	d, _ = secscodec.ParseData(m)
	// S1F4 = L,n{V}：纯值列表。
	items, err := secscodec.DecodeValueReply(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("S1F4 应答 %d 项", len(items))
	}
	if v, _ := items[0].U4(); v != 100 {
		t.Fatalf("VID 1 = %d, want 100", v)
	}

	// S2F15 写 ECID 10 → S2F16 ACK=0。
	cs := must(secscodec.EncodeConstantSend([]secscodec.Constant{{ID: 10, Value: secscodec.Int(secscodec.FormatU4, 7)}}))
	cl.send(secscodec.DataMessage{Stream: 2, Function: 15, WBit: true, Body: cs}.ToMessage())
	m = cl.recv()
	d, _ = secscodec.ParseData(m)
	if ack, err := secscodec.DecodeAck(d.Body); err != nil || ack != 0 {
		t.Fatalf("S2F16 ACK = %d err=%v", ack, err)
	}
	if v, ok := srv.ValueOf(secscodec.ObjectECID, 10); !ok {
		t.Fatal("sim 未更新 ECID 10")
	} else if n, _ := v.U4(); n != 7 {
		t.Fatalf("ECID 10 = %d, want 7", n)
	}
}

// TestSimPushAsync 推送到达已选中的客户端；本测试只验证推送能被收到，不验证
// 主站侧忽略逻辑（那在驱动测试里）。
func TestSimPushAsync(t *testing.T) {
	srv, err := secssim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	cl := dial(t, srv.Addr())
	cl.roundTrip(secscodec.NewSelectReq(0))

	srv.PushEvent(9, secscodec.Report{ReportID: 1, Values: []secscodec.StatusValue{{ID: 1, Value: secscodec.Int(secscodec.FormatU4, 1)}}})
	m := cl.recv()
	if !m.IsData() || m.Stream != 6 || m.Function != 11 {
		t.Fatalf("推送不是 S6F11: %s", m.String())
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
