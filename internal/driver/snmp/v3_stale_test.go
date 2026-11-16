package snmp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
)

// TestStaleEngineWindow 钉住 RFC 3414 §3.2 的入站检查：应答的 engineID、
// engineBoots 与 engineTime 必须与本地缓存一致，时间差不得超过 150 秒。
//
// 为什么主站侧也要查：USM 摘要只证明报文**没被改过**，不证明它是**新的**。
// 一条带正确摘要的旧应答可以被原样重放；若不比 boots/time，主站会把旧值
// 当成新采集结果写进缓存——对计量/计数类点位就是静默回退。
func TestStaleEngineWindow(t *testing.T) {
	id := []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x09}
	s := &v3Session{engineID: id, engineBoots: 3, engineTime: 100, engineTimeAt: time.Now()}

	fresh := snmpcodec.USMSecurityParameters{EngineID: id, EngineBoots: 3, EngineTime: 100}
	if got := s.staleEngine(fresh); got != nil {
		t.Fatalf("fresh response rejected: %v", got)
	}
	// 窗口内的时间偏差必须放行：主站与代理之间本来就有往返与时钟误差。
	inside := fresh
	inside.EngineTime = 100 - 100
	if delta := inside.EngineTime - s.estimatedEngineTime(); delta < -engineTimeWindow {
		t.Fatalf("test setup: %d s is already outside the window", delta)
	}
	if got := s.staleEngine(inside); got != nil {
		t.Fatalf("response %d s behind rejected: %v", 100-inside.EngineTime, got)
	}

	bad := []struct {
		name string
		sec  snmpcodec.USMSecurityParameters
	}{
		{"no engine ID", snmpcodec.USMSecurityParameters{EngineBoots: 3, EngineTime: 100}},
		{"different engine ID", snmpcodec.USMSecurityParameters{
			EngineID:    []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x0A},
			EngineBoots: 3, EngineTime: 100,
		}},
		{"agent rebooted", snmpcodec.USMSecurityParameters{EngineID: id, EngineBoots: 4, EngineTime: 1}},
		{"replay from a previous boot", snmpcodec.USMSecurityParameters{EngineID: id, EngineBoots: 2, EngineTime: 100}},
		{"future time outside window", snmpcodec.USMSecurityParameters{EngineID: id, EngineBoots: 3, EngineTime: 100 + engineTimeWindow + 1}},
		{"past time outside window", snmpcodec.USMSecurityParameters{EngineID: id, EngineBoots: 3, EngineTime: 100 - engineTimeWindow - 1}},
	}
	for _, b := range bad {
		if got := s.staleEngine(b.sec); got == nil {
			t.Errorf("%s: accepted, want rejection", b.name)
		}
	}
}

// timeoutDatagram 记录写出的报文，读取永远超时。
//
// 用它能测「重发」这件事本身：真实 UDP + 内置代理无法按命令丢包，而重发
// 只丢一个包就会失效的那类缺陷，恰好只在真实网络里出现。
type timeoutDatagram struct {
	writes [][]byte
}

func (d *timeoutDatagram) Write(p []byte) error {
	d.writes = append(d.writes, append([]byte(nil), p...))
	return nil
}

func (d *timeoutDatagram) Read() ([]byte, error)       { return nil, os.ErrDeadlineExceeded }
func (d *timeoutDatagram) SetDeadline(time.Time) error { return nil }
func (d *timeoutDatagram) Close() error                { return nil }

// TestV3RetransmitsWithFreshSalt：超时必须重发，且每次重发都要重新加密
// （新的 salt / IV）。
//
// 两条缺陷叠在一起时才看得出来：早先 v3 的重发循环拿不到触发条件，retries
// 配置实际无效（丢一个包就是一轮采集失败）；而一旦重发，复用同一份密文会
// 让同一个 (key, IV) 加密两次，违反 RFC 3414 §8。
func TestV3RetransmitsWithFreshSalt(t *testing.T) {
	dg := &timeoutDatagram{}
	s := &v3Session{
		user: "monitor", auth: snmpcodec.AuthSHA256, priv: snmpcodec.PrivAES,
		engineID:    []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x01},
		engineBoots: 1, engineTime: 5, engineTimeAt: time.Now(),
	}
	keys := &snmpcodec.V3Keys{
		Auth: s.auth, Priv: s.priv,
		AuthKey: bytes.Repeat([]byte{0x11}, 32),
		PrivKey: bytes.Repeat([]byte{0x22}, 32),
	}
	s.keys = keys
	s.keysReady.Store(true)

	pdu, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, 1, []snmpcodec.OID{sysDescrOID})
	if err != nil {
		t.Fatalf("build pdu: %v", err)
	}
	c := &connState{
		cfg:     driver.OpenConfig{Device: model.Device{ID: "dev"}},
		conn:    dg,
		retries: 1,
		maxSize: 1400,
		v3:      s,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.v3Round(ctx, pdu, keys, "test"); err == nil {
		t.Fatal("no response at all: want a timeout")
	} else if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("kind = %q, want timeout (%v)", driver.KindOf(err), err)
	}
	if len(dg.writes) != 2 {
		t.Fatalf("datagrams = %d, want 2 (retries=1 must resend exactly once)", len(dg.writes))
	}
	if bytes.Equal(dg.writes[0], dg.writes[1]) {
		t.Fatal("retransmission reused the same ciphertext; salt/IV must be fresh per encryption")
	}
	first, err := snmpcodec.ParseV3(dg.writes[0])
	if err != nil {
		t.Fatalf("parse first datagram: %v", err)
	}
	second, err := snmpcodec.ParseV3(dg.writes[1])
	if err != nil {
		t.Fatalf("parse second datagram: %v", err)
	}
	if bytes.Equal(first.Security.PrivParameters, second.Security.PrivParameters) {
		t.Fatalf("privParameters reused (%x); AES-CFB requires a unique salt per encryption",
			first.Security.PrivParameters)
	}
	// request-id 与 msgID 必须保持不变：重发的是**同一条请求**，代理要能
	// 把它与第一次的上游状态合并，而不是当成两条业务请求。request-id 在
	// 密文里，这一层只能直接校验 msgID。
	if !first.Encrypted || !second.Encrypted {
		t.Fatal("privacy is enabled but the datagram was not encrypted")
	}
	if first.Header.MsgID != second.Header.MsgID {
		t.Fatalf("msgID changed across retransmission: %d -> %d", first.Header.MsgID, second.Header.MsgID)
	}
}

// scriptedDatagram 依次吐出预置报文，之后超时。
type scriptedDatagram struct {
	frames [][]byte
	idx    int
}

func (d *scriptedDatagram) Write(p []byte) error        { return nil }
func (d *scriptedDatagram) SetDeadline(time.Time) error { return nil }
func (d *scriptedDatagram) Close() error                { return nil }
func (d *scriptedDatagram) Read() ([]byte, error) {
	if d.idx >= len(d.frames) {
		return nil, os.ErrDeadlineExceeded
	}
	f := d.frames[d.idx]
	d.idx++
	return f, nil
}

// TestReadV3RoundRejectsStaleEngine 钉住接线：readV3Round 必须真的调用
// staleEngine，而不是只把它写在那里。
//
// 一条无认证的明文 Response（认证校验会跳过）如果来自另一个 engineID，
// 必须被判为 stale——这类报文正是「重放/串台」最省事的形态，若放行，
// 上层会把它当成当前采集结果。
func TestReadV3RoundRejectsStaleEngine(t *testing.T) {
	mine := []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x01}
	other := []byte{0x80, 0x00, 0x1F, 0x88, 0x80, 0x00, 0x00, 0x00, 0x02}
	pdu := snmpcodec.PDU{Type: snmpcodec.PDUResponse, RequestID: 42}
	scoped, err := snmpcodec.BuildScopedPDU(other, "", pdu)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	raw, err := snmpcodec.BuildV3(
		snmpcodec.HeaderData{MsgID: 7, MaxSize: 1400, Flags: snmpcodec.FlagReportable, SecModel: snmpcodec.SecurityModelUSM},
		snmpcodec.USMSecurityParameters{EngineID: other, EngineBoots: 1, EngineTime: 10, UserName: "monitor"},
		scoped, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	s := &v3Session{engineID: mine, engineBoots: 1, engineTime: 10, engineTimeAt: time.Now()}
	c := &connState{
		cfg:  driver.OpenConfig{Device: model.Device{ID: "dev"}},
		conn: &scriptedDatagram{frames: [][]byte{raw}}, retries: 0, maxSize: 1400, v3: s,
	}
	_, err = c.readV3Round(context.Background(), 42, &snmpcodec.V3Keys{}, "test")
	var stale *staleEngineError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want *staleEngineError", err)
	}
}
