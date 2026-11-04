package mqtt

import (
	"bytes"
	"testing"
)

// 这些用例锁定线上字节。MQTT 的坑几乎都在编码细节上：变长整数、UTF-8
// 长度前缀、connect flags 的位序。任何一处写错，Broker 的表现都是
// 「连接被拒」或「静默丢弃」，而不会告诉你哪一位错了。

func TestAppendLengthEncoding(t *testing.T) {
	cases := []struct {
		n    int
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xFF, 0x7F}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{2097151, []byte{0xFF, 0xFF, 0x7F}},
		{2097152, []byte{0x80, 0x80, 0x80, 0x01}},
		{MaxRemainingLength, []byte{0xFF, 0xFF, 0xFF, 0x7F}},
	}
	for _, c := range cases {
		got := appendLength(nil, c.n)
		if !bytes.Equal(got, c.want) {
			t.Fatalf("%d: got %x want %x", c.n, got, c.want)
		}
		// 回读必须一致，否则分帧会错位。
		back, err := readLength(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("%d: read: %v", c.n, err)
		}
		if back != c.n {
			t.Fatalf("%d: round-trip got %d", c.n, back)
		}
	}
}

func TestReadLengthRejectsOverlong(t *testing.T) {
	// 5 个续位字节是非法编码；必须报错而不是回绕成一个小数字。
	_, err := readLength(bytes.NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x01}))
	if err == nil {
		t.Fatal("overlong remaining length must be rejected")
	}
}

func TestConnectPacketGolden(t *testing.T) {
	got, err := ConnectPacket(ConnectOptions{
		ClientID:     "uic-1",
		KeepAlive:    30,
		CleanSession: true,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []byte{
		0x10, 0x11, // CONNECT, remaining length 17
		0x00, 0x04, 'M', 'Q', 'T', 'T', // protocol name
		0x04,       // level 3.1.1
		0x02,       // clean session
		0x00, 0x1E, // keepalive 30
		0x00, 0x05, 'u', 'i', 'c', '-', '1', // client id
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
}

func TestConnectPacketWithAuthAndWill(t *testing.T) {
	got, err := ConnectPacket(ConnectOptions{
		ClientID:     "gw",
		Username:     "u",
		Password:     "p",
		KeepAlive:    60,
		CleanSession: true,
		WillTopic:    "uic/site/$death",
		WillPayload:  []byte(`{"online":false}`),
		WillQoS:      1,
		WillRetain:   true,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got[0] != TypeConnect<<4 {
		t.Fatalf("type nibble = %#x", got[0])
	}
	// connect flags 位于固定头(1) + 长度(1) + 协议名(6) + level(1) 之后。
	flags := got[9]
	wantFlags := byte(0x02 | 0x04 | 0x08 | 0x20 | 0x80 | 0x40)
	if flags != wantFlags {
		t.Fatalf("flags = %#x want %#x", flags, wantFlags)
	}
	if !bytes.Contains(got, []byte("uic/site/$death")) {
		t.Fatal("will topic missing")
	}
	if !bytes.Contains(got, []byte(`{"online":false}`)) {
		t.Fatal("will payload missing")
	}
}

func TestConnectPacketValidation(t *testing.T) {
	cases := []ConnectOptions{
		{},                             // 无 clientId
		{ClientID: "a", Password: "p"}, // 有密码无用户名
		{ClientID: "a", WillPayload: []byte("x")}, // 有遗嘱体无主题
		{ClientID: "a", WillTopic: "t", WillQoS: 3},
	}
	for i, c := range cases {
		if _, err := ConnectPacket(c); err == nil {
			t.Fatalf("case %d should fail: %+v", i, c)
		}
	}
}

func TestPublishPacketQoS0(t *testing.T) {
	got, err := PublishPacket(Message{Topic: "a/b", Payload: []byte("hi")}, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []byte{
		0x30, 0x07, // PUBLISH qos0, remaining 7
		0x00, 0x03, 'a', '/', 'b',
		'h', 'i',
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
}

func TestPublishPacketQoS1Retain(t *testing.T) {
	got, err := PublishPacket(Message{Topic: "a", Payload: []byte("x"), QoS: 1, Retain: true}, 0x1234)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got[0] != 0x33 { // 0x30 | qos1<<1 | retain
		t.Fatalf("first byte = %#x", got[0])
	}
	if !bytes.Contains(got, []byte{0x12, 0x34}) {
		t.Fatalf("packet id missing: %x", got)
	}
}

func TestPublishPacketValidation(t *testing.T) {
	cases := []struct {
		m   Message
		pid uint16
	}{
		{Message{}, 0},                   // 无主题
		{Message{Topic: "t", QoS: 3}, 1}, // QoS 越界
		{Message{Topic: "t", QoS: 1}, 0}, // QoS1 必须有非零 pid
	}
	for i, c := range cases {
		if _, err := PublishPacket(c.m, c.pid); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
}

func TestParseConnack(t *testing.T) {
	present, code, err := ParseConnack([]byte{0x01, ConnAccepted})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !present || code != ConnAccepted {
		t.Fatalf("present=%v code=%d", present, code)
	}

	if _, _, err := ParseConnack([]byte{0x00}); err == nil {
		t.Fatal("short connack must fail")
	}
	if _, _, err := ParseConnack([]byte{0x02, 0x00}); err == nil {
		t.Fatal("reserved bits must be rejected")
	}
}

func TestConnackCodeMessages(t *testing.T) {
	// 错误信息必须能指向下一步动作，而不是只给一个数字。
	for _, code := range []byte{1, 2, 3, 4, 5} {
		msg := ConnackCode(code)
		if msg == "" || msg == "unknown" {
			t.Fatalf("code %d has no message", code)
		}
	}
}

func TestReadPacketRoundTrip(t *testing.T) {
	// 用一条 200 字节载荷验证「长度 >= 128 时用两字节变长整数」。
	payload := bytes.Repeat([]byte{'x'}, 200)
	pkt, err := PublishPacket(Message{Topic: "t", Payload: payload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPacket(bytes.NewReader(pkt), 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Type != TypePublish {
		t.Fatalf("type = %d", got.Type)
	}
	if !bytes.HasSuffix(got.Body, payload) {
		t.Fatal("payload mismatch")
	}
}

func TestReadPacketRejectsOversize(t *testing.T) {
	pkt, _ := PublishPacket(Message{Topic: "t", Payload: make([]byte, 5000)}, 0)
	_, err := ReadPacket(bytes.NewReader(pkt), 1024)
	if err == nil {
		t.Fatal("oversize packet must be rejected, not allocated")
	}
}

func TestParsePuback(t *testing.T) {
	pid, err := ParsePuback([]byte{0x12, 0x34})
	if err != nil || pid != 0x1234 {
		t.Fatalf("pid = %#x err = %v", pid, err)
	}
	if _, err := ParsePuback([]byte{0x12}); err == nil {
		t.Fatal("short puback must fail")
	}
}

func TestNormalizeAddr(t *testing.T) {
	good := map[string]string{
		"tcp://127.0.0.1:1883": "127.0.0.1:1883",
		"127.0.0.1:1883":       "127.0.0.1:1883",
		"broker:1883":          "broker:1883",
	}
	for in, want := range good {
		got, err := NormalizeAddr(in)
		if err != nil || got != want {
			t.Fatalf("%q -> %q, %v", in, got, err)
		}
	}
	// TLS/WS 必须显式失败：静默当 tcp 会变成「连上了但没加密」。
	for _, in := range []string{"", "ssl://h:8883", "tls://h:8883", "ws://h:80", "wss://h:443"} {
		if _, err := NormalizeAddr(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}
