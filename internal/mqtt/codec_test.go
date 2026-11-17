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
		Will: &Will{
			Topic:   "uic/site/$death",
			Payload: []byte(`{"online":false}`),
			QoS:     1,
			Retain:  true,
		},
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
		{ClientID: "a", Will: &Will{Payload: []byte("x")}}, // 有遗嘱体无主题
		{ClientID: "a", Will: &Will{Topic: "t", QoS: 3}},
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

// ---------------------------------------------------------------------------
// 订阅侧编解码
// ---------------------------------------------------------------------------

func TestSubscribePacketGolden(t *testing.T) {
	pkt, err := SubscribePacket([]TopicFilter{
		{Filter: "a/b", QoS: 1},
		{Filter: "c/#", QoS: 0},
	}, 0x1234)
	if err != nil {
		t.Fatal(err)
	}
	// 固定头低 4 位必须是 0b0010（§3.8.1）。写错不会被本地发现，只会被
	// Broker 以「协议错误」断链，抓包里极难看出来。
	if pkt[0] != TypeSubscribe<<4|0x02 {
		t.Fatalf("fixed header = %#x, want %#x", pkt[0], TypeSubscribe<<4|0x02)
	}
	want := []byte{
		0x82, 0x0e, // 固定头 + 剩余长度 14
		0x12, 0x34, // packet id
		0x00, 0x03, 'a', '/', 'b', 0x01, // 过滤器 1 + 期望 QoS
		0x00, 0x03, 'c', '/', '#', 0x00, // 过滤器 2
	}
	if !bytes.Equal(pkt, want) {
		t.Fatalf("pkt = % x\n want % x", pkt, want)
	}
}

func TestSubscribePacketValidation(t *testing.T) {
	if _, err := SubscribePacket(nil, 1); err == nil {
		t.Fatal("empty filter list must fail")
	}
	if _, err := SubscribePacket([]TopicFilter{{Filter: ""}}, 1); err == nil {
		t.Fatal("empty filter must fail")
	}
	// packet id 0 是「无 id」，Broker 无法把 SUBACK 对回来。
	if _, err := SubscribePacket([]TopicFilter{{Filter: "a"}}, 0); err == nil {
		t.Fatal("packet id 0 must fail")
	}
	if _, err := SubscribePacket([]TopicFilter{{Filter: "a", QoS: 3}}, 1); err == nil {
		t.Fatal("qos 3 must fail")
	}
}

func TestParseSuback(t *testing.T) {
	pid, granted, err := ParseSuback([]byte{0x00, 0x07, 0x01, 0x80})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pid != 7 {
		t.Fatalf("pid = %d", pid)
	}
	if len(granted) != 2 || granted[0] != 1 || granted[1] != SubackFailure {
		t.Fatalf("granted = %v", granted)
	}
	if _, _, err := ParseSuback([]byte{0x00}); err == nil {
		t.Fatal("short suback must fail")
	}
}

func TestParsePublishFlags(t *testing.T) {
	// QoS1 + retain：固定头 0b0011_0011。
	pkt, err := PublishPacket(Message{Topic: "a/b", Payload: []byte("hi"), QoS: 1, Retain: true}, 9)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPacket(bytes.NewReader(pkt), 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	p, err := ParsePublish(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Topic != "a/b" || string(p.Payload) != "hi" {
		t.Fatalf("p = %+v", p)
	}
	if p.QoS != 1 || !p.Retain || p.Duplicate {
		t.Fatalf("flags = qos%d retain=%v dup=%v", p.QoS, p.Retain, p.Duplicate)
	}
	if p.PID != 9 {
		t.Fatalf("pid = %d", p.PID)
	}
}

func TestParsePublishDuplicateFlag(t *testing.T) {
	// DUP 位由 Broker 在重投时置位。驱动不需要特殊处理（QoS1 幂等靠上层的
	// 时间戳），但必须能读出来，否则排障时无法区分「新消息」与「重投」。
	body := append([]byte{0x00, 0x01, 't'}, []byte("x")...)
	pkt := Packet{Type: TypePublish, Flags: 0x08, Body: body}
	p, err := ParsePublish(pkt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.Duplicate {
		t.Fatal("dup flag lost")
	}
}

func TestParsePublishRejectsNonPublish(t *testing.T) {
	if _, err := ParsePublish(Packet{Type: TypePuback, Body: []byte{0, 1}}); err == nil {
		t.Fatal("puback must not parse as publish")
	}
	// 主题长度超过报文长度：必须报错，不能越界读。
	bad := Packet{Type: TypePublish, Body: []byte{0x00, 0x20, 'a'}}
	if _, err := ParsePublish(bad); err == nil {
		t.Fatal("truncated topic must fail")
	}
	// QoS1 但缺少 packet id。
	bad = Packet{Type: TypePublish, Flags: 0x02, Body: []byte{0x00, 0x01, 't'}}
	if _, err := ParsePublish(bad); err == nil {
		t.Fatal("qos1 without packet id must fail")
	}
}

func TestPublishQoS2IsRejectedLocally(t *testing.T) {
	// QoS2 需要 PUBREC/PUBREL/PUBCOMP 三条报文与两套状态，本项目不做。
	// 本地直接拒绝，好过发出去之后被 Broker 断链。
	if _, err := PublishPacket(Message{Topic: "t", QoS: 2}, 1); err == nil {
		t.Fatal("qos2 must be rejected at the codec layer")
	}
}

// ---------------------------------------------------------------------------
// ParseConnect
// ---------------------------------------------------------------------------

// connectBody 由 ConnectPacket 之外的路径构造：直接从我们自己的编码器取
// 报文体（跳过固定头），这样 golden 里出现的仍是「客户端实际发出的字节」。
func connectBody(t *testing.T, o ConnectOptions) []byte {
	t.Helper()
	pkt, err := ConnectPacket(o)
	if err != nil {
		t.Fatalf("encode connect: %v", err)
	}
	n, err := readLength(bytes.NewReader(pkt[1:]))
	if err != nil {
		t.Fatalf("read remaining length: %v", err)
	}
	hlen := 1 + len(appendLength(nil, n))
	return pkt[hlen:]
}

func TestParseConnectRoundTrip(t *testing.T) {
	body := connectBody(t, ConnectOptions{
		ClientID:     "gw",
		Username:     "u",
		Password:     "p",
		KeepAlive:    45,
		CleanSession: true,
		Will:         &Will{Topic: "spBv1.0/g/NDEATH/n", Payload: []byte{0x01, 0x02}, QoS: 1},
	})
	ci, err := ParseConnect(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ci.ClientID != "gw" || ci.Username != "u" || !ci.HasPassword {
		t.Fatalf("credentials = %+v", ci)
	}
	if ci.KeepAlive != 45 {
		t.Fatalf("keepalive = %d", ci.KeepAlive)
	}
	// CleanSession 必须是 true：false 会让 Broker 保留上一次会话的离线消息
	// 与订阅，重连后 Sparkplug 的 BIRTH 语义就乱了。
	if !ci.CleanSession {
		t.Fatal("clean session lost")
	}
	if !ci.HasWill {
		t.Fatal("will not parsed")
	}
	if ci.Will.Topic != "spBv1.0/g/NDEATH/n" {
		t.Fatalf("will topic = %q", ci.Will.Topic)
	}
	if !bytes.Equal(ci.Will.Payload, []byte{0x01, 0x02}) {
		t.Fatalf("will payload = %x", ci.Will.Payload)
	}
	// 遗嘱 QoS 必须是 1（[tck-id-payloads-ndeath-will-message-qos]）。
	if ci.Will.QoS != 1 {
		t.Fatalf("will qos = %d", ci.Will.QoS)
	}
	// 遗嘱 retain 必须是 false，否则 NDEATH 会一直挂在 Broker 上，
	// 之后每个订阅方都会读到一条几小时前的死亡记录。
	if ci.Will.Retain {
		t.Fatal("will retain must be false for NDEATH")
	}
}

func TestParseConnectRejects(t *testing.T) {
	valid := connectBody(t, ConnectOptions{ClientID: "a", CleanSession: true})
	mutate := func(fn func([]byte) []byte) []byte {
		b := append([]byte(nil), valid...)
		return fn(b)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"truncated protocol name", []byte{0x00, 0x04, 'M', 'Q'}},
		{"bad protocol name", mutate(func(b []byte) []byte {
			c := append([]byte(nil), b...)
			c[2] = 'X'
			return c
		})},
		{"bad protocol level", append([]byte{0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x02, 0x00, 0x00, 0x00, 0x01, 'a'}, nil...)},
		{"reserved flag set", []byte{0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x03, 0x00, 0x1E, 0x00, 0x01, 'a'}},
		{"bad will qos", []byte{0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x04 | 0x18, 0x00, 0x1E, 0x00, 0x01, 'a'}},
		{"truncated will topic", mutate(func(b []byte) []byte {
			// 打开 will flag 但不给出遗嘱段。
			c := append([]byte(nil), b[:8]...)
			c[7] |= 0x04
			return c
		})},
	}
	for _, c := range cases {
		if _, err := ParseConnect(c.body); err == nil {
			t.Fatalf("%s: must be rejected", c.name)
		}
	}
}

func TestParseConnectNoWill(t *testing.T) {
	body := connectBody(t, ConnectOptions{ClientID: "a", CleanSession: true})
	ci, err := ParseConnect(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ci.HasWill {
		t.Fatal("will flag set without will")
	}
	if ci.Will.Topic != "" {
		t.Fatalf("will topic = %q", ci.Will.Topic)
	}
}
