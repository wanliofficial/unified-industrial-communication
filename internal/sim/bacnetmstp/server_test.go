package bacnetmstp

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	drvmstp "uic/internal/driver/bacnetmstp"
	"uic/internal/model"
)

func TestServerDefaultsAndObjects(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	srv.DisableAutoIncrement()
	objs := srv.Objects()
	for _, want := range []string{
		"analog-input:1", "analog-value:2", "analog-value:3",
		"binary-input:1", "character-string:1", "device:1",
	} {
		found := false
		for _, got := range objs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("object %s missing from %v", want, objs)
		}
	}
	if v, ok := srv.Value("analog-input", 1, "present-value"); !ok || v != 42.5 {
		t.Fatalf("AI-1 = %v", v)
	}
	if v, ok := srv.Value("analog-input", 1, "object-name"); !ok || v != "AI-1" {
		t.Fatalf("AI-1 name = %v", v)
	}
	if v, ok := srv.Value("analog-input", 1, "units"); !ok || v != uint64(62) {
		t.Fatalf("AI-1 units = %v", v)
	}
	if v, ok := srv.Value("binary-input", 1, "present-value"); !ok || v != uint64(1) {
		t.Fatalf("BI-1 = %v", v)
	}
	if v, ok := srv.Value("character-string", 1, "present-value"); !ok || v != "PUMP-01" {
		t.Fatalf("CS-1 = %v", v)
	}
	// priority-array 是数组属性，Value 明确返回 false 而不是伪装成单值。
	if _, ok := srv.Value("analog-value", 2, "priority-array"); ok {
		t.Fatal("priority-array reported as a scalar")
	}
}

func TestServerMACValidation(t *testing.T) {
	if _, err := NewServerWithOptions(Options{Addr: "127.0.0.1:0", MACs: []byte{0x05}}); err == nil {
		t.Fatal("master MAC accepted as a sim slave address")
	}
	srv, err := NewServerWithOptions(Options{Addr: "127.0.0.1:0", MACs: []byte{128, 130}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	macs := srv.MACs()
	if len(macs) != 2 || macs[0] != 128 || macs[1] != 130 {
		t.Fatalf("MACs() = %v", macs)
	}
}

func TestAutoIncrementTicks(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	start, _ := srv.Value("analog-value", 3, "present-value")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := srv.Value("analog-value", 3, "present-value")
		if v.(float64) > start.(float64) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("analog-value:3 did not auto-increment within 3s")
}

// ---------------------------------------------------------------------------
// 帧循环（直接喂字节流，不经过驱动的完整实现）
// ---------------------------------------------------------------------------

func TestHandleFrameRoles(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	srv.DisableAutoIncrement()

	// 从站不应答 Token / PollForMaster / ReplyToPollForMaster，
	// 因为它们只属于主站之间的握手。
	for _, typ := range []byte{
		bacnetcodec.FrameToken, bacnetcodec.FramePollForMaster,
		bacnetcodec.FrameReplyToPollForMaster, bacnetcodec.FrameReplyPostponed,
	} {
		if got := srv.handleFrame(bacnetcodec.MSTPFrame{Type: typ, Dest: 128, Src: 0}); got != nil {
			t.Fatalf("%s: slave answered with %+v", bacnetcodec.FrameTypeName(typ), got)
		}
	}

	// 广播 TestRequest 要应答，点名叫别的站的 TestRequest 不应答。
	if got := srv.handleFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameTestRequest, Dest: bacnetcodec.MSTPBroadcast, Src: 0,
	}); got == nil || got.Type != bacnetcodec.FrameTestResponse || got.Dest != 0 || got.Src != bacnetcodec.MSTPBroadcast {
		t.Fatalf("broadcast test request -> %+v", got)
	}
	if got := srv.handleFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameTestRequest, Dest: 129, Src: 0,
	}); got != nil {
		t.Fatalf("test request for another MAC answered: %+v", got)
	}

	// 点名本机的读请求要回数据帧；点名 129 的不回。
	req, err := bacnetcodec.ReadPropertyRequest(1, bacnetcodec.ObjectAnalogInput, 1,
		bacnetcodec.PropPresentValue, 0)
	if err != nil {
		t.Fatal(err)
	}
	npdu := bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlExpectReply, req)
	got := srv.handleFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataExpectingReply, Dest: 128, Src: 0, Data: npdu,
	})
	if got == nil {
		t.Fatal("read request not answered")
	}
	if got.Type != bacnetcodec.FrameDataNotExpectingReply || got.Src != 128 || got.Dest != 0 {
		t.Fatalf("response frame = %+v", got)
	}
	if got.Data[1] != bacnetcodec.NPDUControlNone {
		t.Fatalf("response NPDU control = 0x%02X, want 0x%02X", got.Data[1], bacnetcodec.NPDUControlNone)
	}
	if got := srv.handleFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataExpectingReply, Dest: 129, Src: 0, Data: npdu,
	}); got != nil {
		t.Fatalf("read request for another MAC answered: %+v", got)
	}
}

func TestHandleAPDUSegmentedRejected(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	// 分段位（首字节 bit3）置位的 ConfirmedRequest 必须回 Reject。
	apdu := []byte{0x08, 0x00, 0x07, bacnetcodec.ServiceReadProperty}
	resp := srv.handleAPDU(apdu)
	if len(resp) < 3 || resp[0]&0xF0 != 0x60 {
		t.Fatalf("segmented request -> % X", resp)
	}
	if resp[2] != bacnetcodec.RejectReasonSegmentedNotSup {
		t.Fatalf("reject reason = 0x%02X", resp[2])
	}
}

func TestHandleAPDUUnsupportedService(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	// Who-Is（unconfirmed）不是 ConfirmedRequest，直接丢弃。
	if got := srv.handleAPDU([]byte{0x10, 0x08}); got != nil {
		t.Fatalf("unconfirmed request answered: % X", got)
	}
	// 未实现的服务回 Error(services, other)。
	apdu := []byte{0x00, 0x00, 0x03, 0x0E}
	resp := srv.handleAPDU(apdu)
	if len(resp) < 3 || resp[0]&0xF0 != 0x50 {
		t.Fatalf("unsupported service -> % X", resp)
	}
}

func TestReadFrameFromStream(t *testing.T) {
	frame := bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataExpectingReply, Dest: 128, Src: 0,
		Data: []byte{0x01, 0x04, 0x00, 0x0C},
	}
	wire, err := bacnetcodec.BuildMSTPFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	// 前置噪声 + 假 preamble（头 CRC 错）+ 真帧 + 截断尾巴。
	junk := []byte{0x00, 0x55, 0x00, 0x55, 0xFF, 0x05, 0x01, 0x02, 0x00, 0x00, 0x00, 0x00}
	stream := append(append([]byte(nil), junk...), wire...)
	br := bufio.NewReader(bytes.NewReader(stream))
	got, err := readFrame(br)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if got.Type != frame.Type || got.Dest != frame.Dest || got.Src != frame.Src ||
		!bytes.Equal(got.Data, frame.Data) {
		t.Fatalf("frame = %+v, want %+v", got, frame)
	}

	// 只有噪声时返回 io 错误而不是构造一个假帧。
	br = bufio.NewReader(bytes.NewReader(junk))
	if _, err := readFrame(br); err == nil {
		t.Fatal("noise parsed as a frame")
	}
}

// ---------------------------------------------------------------------------
// 端到端：驱动 ↔ 从站
// ---------------------------------------------------------------------------

func TestEndToEndWithDriver(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	defer srv.Close()
	srv.DisableAutoIncrement()

	ch := model.Channel{
		ID: "ch", Protocol: "bacnet-mstp",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(time.Second),
	}
	dev := model.Device{ID: "dev", ChannelID: "ch", Interval: model.Duration(time.Second),
		Enabled: true, UnitID: 128}
	d := drvmstp.New()
	c, err := d.Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "temp", DataType: model.TypeFloat32, Address: model.Address{
			Kind: model.AddressObject, ObjectType: "analog-input", Instance: 1, Property: "present-value"}},
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if samples[0].Value.Num != 42.5 {
		t.Fatalf("temp = %v", samples[0].Value.Num)
	}
}

// 确认 sim 监听的是 TCP：串口服务器/DTU 透传是现场最常见的自检接法。
func TestServerListensOnTCP(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	// Close 幂等。
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	_ = srv.Close()
}
