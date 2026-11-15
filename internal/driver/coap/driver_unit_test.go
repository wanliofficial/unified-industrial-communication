package coap

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	coapcodec "uic/internal/codec/coap"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// ---------------------------------------------------------------------------
// 元数据
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "coap" {
		t.Fatalf("id = %q", m.ID)
	}
	if m.Version == "" || m.SDKVersion == "" || m.Description == "" {
		t.Fatalf("incomplete meta: %+v", m)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "udp" {
		t.Fatalf("transports = %v, want [udp]", m.Transports)
	}
	if len(m.AddressKinds) != 1 || m.AddressKinds[0] != model.AddressSymbol {
		t.Fatalf("address kinds = %v", m.AddressKinds)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("read/write must be supported: %+v", m.Capabilities)
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported {
		t.Fatal("Observe is out of scope; Subscribe must be unsupported")
	}
	if m.Capabilities.Discovery != driver.CapSupported {
		t.Fatal("resource discovery is implemented")
	}
	if len(m.ConfigFields) == 0 || len(m.PointFields) == 0 {
		t.Fatalf("config/point fields missing: %d/%d", len(m.ConfigFields), len(m.PointFields))
	}
}

// TestMetaDataTypeEnumMatchesImplementation 钉住「声明支持的类型」与
// 「planPoint 实际接受、textval 实际能编解码」一致。
//
// 枚举漏一项的表现很隐蔽：驱动跑起来没问题，但配置界面里选不到那个类型，
// 只能手改 JSON；反过来枚举多一项则会在 Open 阶段报 address 错误。两者都是
// 声明与实现漂移，因此这里逐个枚举项跑一遍 planPoint。
func TestMetaDataTypeEnumMatchesImplementation(t *testing.T) {
	m := New().Meta()
	var enum []string
	for _, f := range m.PointFields {
		if f.Name == "dataType" {
			enum = f.Enum
		}
	}
	if len(enum) == 0 {
		t.Fatal("meta 未声明 dataType 枚举")
	}
	for _, name := range enum {
		ref := driver.PointRefOf(model.Device{ID: "dev"}, pt("p", "/r", "", name))
		if _, err := planPoint(ref); err != nil {
			t.Errorf("枚举声明支持 %s，planPoint 却拒绝: %v", name, err)
		}
	}
	// 64 位整型由 textval 支持、planPoint 接受，枚举不能漏（曾经漏过）。
	for _, want := range []string{"int64", "uint64"} {
		found := false
		for _, name := range enum {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("dataType 枚举缺少 %s", want)
		}
	}
}

// ---------------------------------------------------------------------------
// decodeValue
// ---------------------------------------------------------------------------

func planFor(sel, dt string, maxLen int) pointPlan {
	return pointPlan{
		ref:      driver.PointRef{ID: "p"},
		path:     "/x",
		selector: sel,
		typ:      model.DataType(dt),
		maxLen:   maxLen,
	}
}

func contentMsg(format uint16, payload string) coapcodec.Message {
	m := coapcodec.Message{Code: coapcodec.CodeContent, Payload: []byte(payload)}
	m.SetUintOption(coapcodec.OptContentFormat, uint32(format))
	return m
}

func TestDecodeValue(t *testing.T) {
	cases := []struct {
		name    string
		plan    pointPlan
		msg     coapcodec.Message
		wantNum float64
		wantTxt string
		wantErr bool
	}{
		{"text float", planFor("", "float64", 0), contentMsg(coapcodec.FormatTextPlain, " 21.5 "), 21.5, "", false},
		{"no content-format defaults to text", planFor("", "int16", 0),
			coapcodec.Message{Code: coapcodec.CodeContent, Payload: []byte("7")}, 7, "", false},
		{"text bytes", planFor("", "bytes", 0), contentMsg(coapcodec.FormatTextPlain, "abc"), 0, "abc", false},
		{"text length exceeded", planFor("", "string", 2), contentMsg(coapcodec.FormatTextPlain, "abc"), 0, "", true},
		{"json selector", planFor("a.b", "float64", 0),
			contentMsg(coapcodec.FormatJSON, `{"a":{"b":3.5}}`), 3.5, "", false},
		{"json whole object as string", planFor("", "string", 0),
			contentMsg(coapcodec.FormatJSON, `{"z":1,"a":2}`), 0, `{"a":2,"z":1}`, false},
		{"json into bytes", planFor("", "bytes", 0), contentMsg(coapcodec.FormatJSON, `{"a":1}`), 0, "", true},
		{"json invalid", planFor("", "float64", 0), contentMsg(coapcodec.FormatJSON, `{`), 0, "", true},
		{"json selector missing", planFor("nope", "float64", 0),
			contentMsg(coapcodec.FormatJSON, `{"a":1}`), 0, "", true},
		{"octets into bytes", planFor("", "bytes", 0), contentMsg(coapcodec.FormatOctets, "\x00\x01"), 0, "\x00\x01", false},
		{"octets into string", planFor("", "string", 0), contentMsg(coapcodec.FormatOctets, "raw"), 0, "raw", false},
		{"octets into number", planFor("", "float64", 0), contentMsg(coapcodec.FormatOctets, "1"), 0, "", true},
		{"unsupported cbor", planFor("", "float64", 0), contentMsg(coapcodec.FormatCBOR, "\xa1"), 0, "", true},
		{"empty payload", planFor("", "string", 0),
			coapcodec.Message{Code: coapcodec.CodeContent}, 0, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := decodeValue(c.plan, c.msg)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantTxt != "" || c.plan.typ == model.TypeString || c.plan.typ == model.TypeBytes {
				if v.Text != c.wantTxt {
					t.Fatalf("text = %q, want %q", v.Text, c.wantTxt)
				}
				return
			}
			if v.Num != c.wantNum {
				t.Fatalf("num = %v, want %v", v.Num, c.wantNum)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 写入编码
// ---------------------------------------------------------------------------

func TestEncodeWritePayload(t *testing.T) {
	if b, f, err := encodeWritePayload(planFor("", "float64", 0),
		model.NumValue(model.TypeFloat64, 1.5), FormatText); err != nil || string(b) != "1.5" || f != coapcodec.FormatTextPlain {
		t.Fatalf("text = %q/%d err=%v", b, f, err)
	}
	if b, f, err := encodeWritePayload(planFor("", "bytes", 0),
		model.BytesValue([]byte("xy")), FormatText); err != nil || string(b) != "xy" || f != coapcodec.FormatTextPlain {
		t.Fatalf("text bytes = %q/%d err=%v", b, f, err)
	}
	if _, _, err := encodeWritePayload(planFor("", "bytes", 0),
		model.BytesValue([]byte("xy")), FormatJSON); err == nil {
		t.Fatal("bytes as JSON: expected error")
	}
	if b, f, err := encodeWritePayload(planFor("", "string", 0),
		model.TextValue(model.TypeString, "raw"), FormatOctets); err != nil || string(b) != "raw" || f != coapcodec.FormatOctets {
		t.Fatalf("octets = %q/%d err=%v", b, f, err)
	}
	if _, _, err := encodeWritePayload(planFor("", "float64", 0),
		model.NumValue(model.TypeFloat64, 1), FormatOctets); err == nil {
		t.Fatal("octets numeric: expected error")
	}
	if _, _, err := encodeWritePayload(planFor("", "float64", 0),
		model.NumValue(model.TypeFloat64, 1), "xml"); err == nil {
		t.Fatal("unknown format: expected error")
	}
}

func TestJSONWritePayload(t *testing.T) {
	if b, err := jsonWritePayload("", model.NumValue(model.TypeInt32, 7), model.TypeInt32); err != nil || string(b) != "7" {
		t.Fatalf("scalar = %q err=%v", b, err)
	}
	if b, err := jsonWritePayload("a.b", model.TextValue(model.TypeString, "x"), model.TypeString); err != nil || string(b) != `{"a":{"b":"x"}}` {
		t.Fatalf("nested = %q err=%v", b, err)
	}
	if _, err := jsonWritePayload("a..b", model.NumValue(model.TypeInt32, 1), model.TypeInt32); err == nil {
		t.Fatal("empty segment: expected error")
	}
	if _, err := jsonWritePayload("a[0]", model.NumValue(model.TypeInt32, 1), model.TypeInt32); err == nil {
		t.Fatal("array element: expected error")
	}
	// bool 与浮点走各自的分支。
	if b, err := jsonWritePayload("", model.BoolValue(true), model.TypeBool); err != nil || string(b) != "true" {
		t.Fatalf("bool = %q err=%v", b, err)
	}
	if b, err := jsonWritePayload("", model.NumValue(model.TypeFloat32, 2.5), model.TypeFloat32); err != nil || string(b) != "2.5" {
		t.Fatalf("float = %q err=%v", b, err)
	}
	// 值类型与目标类型不兼容时报错。
	if _, err := jsonWritePayload("", model.TextValue(model.TypeString, "x"), model.TypeFloat64); err == nil {
		t.Fatal("string as float: expected error")
	}
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func TestCodeErrorMapping(t *testing.T) {
	c := &conn{devID: "d"}
	cases := []struct {
		code      coapcodec.Code
		kind      driver.ErrorKind
		temporary bool
	}{
		{coapcodec.CodeNotFound, driver.ErrAddress, false},
		{coapcodec.CodeUnauthorized, driver.ErrPermission, false},
		{coapcodec.CodeForbidden, driver.ErrPermission, false},
		{coapcodec.CodeBadOption, driver.ErrUnsupported, false},
		{coapcodec.CodeMethodNotAllowed, driver.ErrUnsupported, false},
		{coapcodec.CodeNotAcceptable, driver.ErrUnsupported, false},
		{coapcodec.CodeUnsupportedFormat, driver.ErrUnsupported, false},
		{coapcodec.CodeBadRequest, driver.ErrProtocol, false},
		{coapcodec.CodePreconditionFailed, driver.ErrProtocol, false},
		{coapcodec.CodeRequestEntityTooLarge, driver.ErrProtocol, false},
		{coapcodec.CodeUnavailable, driver.ErrProtocol, true},
		{coapcodec.CodeInternalError, driver.ErrProtocol, true},
	}
	for _, cc := range cases {
		err := c.codeError("read", coapcodec.Message{Code: cc.code, Payload: []byte("detail")})
		if k := driver.KindOf(err); k != cc.kind {
			t.Errorf("%v: kind = %s, want %s", cc.code, k, cc.kind)
		}
		if driver.IsRetryable(err) != cc.temporary {
			t.Errorf("%v: temporary = %v, want %v", cc.code, driver.IsRetryable(err), cc.temporary)
		}
		if !strings.Contains(err.Error(), "detail") {
			t.Errorf("%v: diagnostic payload lost: %v", cc.code, err)
		}
	}
}

func TestDiagnostic(t *testing.T) {
	if got := diagnostic(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := diagnostic([]byte("  a\n\tb   c ")); got != "a b c" {
		t.Fatalf("collapsed = %q", got)
	}
	long := strings.Repeat("x", 300)
	got := diagnostic([]byte(long))
	if len([]rune(got)) > 161 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated = %d bytes, suffix ok=%v", len(got), strings.HasSuffix(got, "…"))
	}
}

// ---------------------------------------------------------------------------
// 链路上的偶发情况：垃圾报文、无关 CON、读错误后重连
// ---------------------------------------------------------------------------

// autoDatagram 是一个可按请求脚本化应答的内存链路。
type autoDatagram struct {
	mu        sync.Mutex
	fn        func(coapcodec.Message) ([][]byte, error)
	pending   [][]byte
	failReads int
	writes    int
	resets    int
	closed    bool
}

func (a *autoDatagram) Write(p []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, err := coapcodec.Decode(p)
	if err != nil {
		return nil
	}
	if m.Type == coapcodec.TypeReset {
		a.resets++
		return nil
	}
	a.writes++
	if a.fn != nil {
		a.pending, _ = a.fn(m)
	}
	return nil
}

func (a *autoDatagram) Read() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failReads > 0 {
		a.failReads--
		return nil, io.ErrClosedPipe
	}
	if len(a.pending) == 0 {
		return nil, os.ErrDeadlineExceeded
	}
	b := a.pending[0]
	a.pending = a.pending[1:]
	return b, nil
}

func (a *autoDatagram) SetDeadline(time.Time) error { return nil }
func (a *autoDatagram) Close() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return nil
}

func (a *autoDatagram) stats() (writes, resets int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writes, a.resets
}

func openAuto(t *testing.T, a *autoDatagram, labels map[string]string) driver.Conn {
	t.Helper()
	if labels == nil {
		labels = map[string]string{"ackTimeout": "100ms", "maxRetransmit": "0"}
	}
	d := NewWithDialer(func(ctx context.Context, addr string, timeout time.Duration) (transport.Datagram, error) {
		return a, nil
	})
	dev := baseDevice(pt("p", "/x", "", "float64"))
	dev.Labels = labels
	conn, err := d.Open(context.Background(), baseConfig("127.0.0.1:1", dev))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// ackResponse 用请求的 MID 与 Token 造一条搭载响应。
func ackResponse(req coapcodec.Message, code coapcodec.Code, format uint16, payload string) []byte {
	m := coapcodec.Message{Type: coapcodec.TypeAck, Code: code, MID: req.MID, Token: req.Token}
	if format != 0 || payload != "" {
		m.SetUintOption(coapcodec.OptContentFormat, uint32(format))
	}
	if payload != "" {
		m.Payload = []byte(payload)
	}
	b, _ := coapcodec.Encode(m)
	return b
}

// TestBadDatagramIgnored 说明「解析失败的报文不会让当前请求失败」。
func TestBadDatagramIgnored(t *testing.T) {
	a := &autoDatagram{fn: func(req coapcodec.Message) ([][]byte, error) {
		return [][]byte{[]byte("not-a-coap-message"), ackResponse(req, coapcodec.CodeContent, coapcodec.FormatTextPlain, "5")}, nil
	}}
	ch := openAuto(t, a, nil)

	smp := readOne(t, ch, pt("p", "/x", "", "float64"))
	if smp.Err != nil || smp.Value.Num != 5 {
		t.Fatalf("sample = %+v", smp)
	}
	if n := ch.(*conn).BadDatagrams(); n != 1 {
		t.Fatalf("bad datagrams = %d, want 1", n)
	}
}

// TestUnrelatedConfirmableGetsReset 说明「别人的 CON 回 RST，但继续等自己
// 的响应」——比如上一次超时后迟到的通知。
func TestUnrelatedConfirmableGetsReset(t *testing.T) {
	a := &autoDatagram{fn: func(req coapcodec.Message) ([][]byte, error) {
		stray, _ := coapcodec.Encode(coapcodec.Message{
			Type: coapcodec.TypeConfirmable, Code: coapcodec.CodeContent,
			MID: req.MID + 100, Token: []byte{0xDE, 0xAD},
			Payload: []byte("stray"),
		})
		return [][]byte{stray, ackResponse(req, coapcodec.CodeContent, coapcodec.FormatTextPlain, "9")}, nil
	}}
	conn := openAuto(t, a, nil)

	smp := readOne(t, conn, pt("p", "/x", "", "float64"))
	if smp.Err != nil || smp.Value.Num != 9 {
		t.Fatalf("sample = %+v", smp)
	}
	if _, resets := a.stats(); resets != 1 {
		t.Fatalf("resets = %d, want 1", resets)
	}
}

// TestReadErrorDropsAndReconnects 说明读错误会丢弃链路，下一次读写重新建链。
func TestReadErrorDropsAndReconnects(t *testing.T) {
	a := &autoDatagram{
		failReads: 1,
		fn: func(req coapcodec.Message) ([][]byte, error) {
			return [][]byte{ackResponse(req, coapcodec.CodeContent, coapcodec.FormatTextPlain, "3")}, nil
		},
	}
	conn := openAuto(t, a, nil)

	// 第一次读：Read 返回非超时错误 → disconnected，链路被丢弃。
	got, err := conn.Read(context.Background(), spans(pt("p", "/x", "", "float64")))
	if err == nil {
		t.Fatal("expected disconnected error")
	}
	if k := driver.KindOf(err); k != driver.ErrDisconnected {
		t.Fatalf("kind = %s, want disconnected (%v)", k, err)
	}
	if len(got) != 1 || got[0].Quality != model.QualityCommError {
		t.Fatalf("sample = %+v", got)
	}

	// 第二次读：ensure 重新建链，成功。
	smp := readOne(t, conn, pt("p", "/x", "", "float64"))
	if smp.Err != nil || smp.Value.Num != 3 {
		t.Fatalf("after reconnect = %+v", smp)
	}
}

// TestProbeClassification 覆盖 Probe 对 2/4/5 类的判定。
func TestProbeClassification(t *testing.T) {
	// 4.04：端点活着，只是不支持发现 → 视为存活。
	a := &autoDatagram{fn: func(req coapcodec.Message) ([][]byte, error) {
		return [][]byte{ackResponse(req, coapcodec.CodeNotFound, coapcodec.FormatTextPlain, "")}, nil
	}}
	if err := openAuto(t, a, nil).Probe(context.Background()); err != nil {
		t.Fatalf("probe on 4.04 = %v, want nil", err)
	}

	// 5.03：服务端出错 → 失败且可重试。
	b := &autoDatagram{fn: func(req coapcodec.Message) ([][]byte, error) {
		return [][]byte{ackResponse(req, coapcodec.CodeUnavailable, coapcodec.FormatTextPlain, "busy")}, nil
	}}
	err := openAuto(t, b, nil).Probe(context.Background())
	if err == nil {
		t.Fatal("probe on 5.03: expected error")
	}
	if !driver.IsRetryable(err) {
		t.Fatalf("5.03 must be retryable: %v", err)
	}
}

// TestDiscoverMalformed 覆盖发现响应无法解析时返回协议错误。
func TestDiscoverMalformed(t *testing.T) {
	a := &autoDatagram{fn: func(req coapcodec.Message) ([][]byte, error) {
		return [][]byte{ackResponse(req, coapcodec.CodeContent, coapcodec.FormatLink, "not-a-link")}, nil
	}}
	conn := openAuto(t, a, nil)
	d := conn.(interface {
		Discover(context.Context) ([]coapcodec.Link, error)
	})
	if _, err := d.Discover(context.Background()); err == nil {
		t.Fatal("expected error for malformed link-format")
	} else if k := driver.KindOf(err); k != driver.ErrProtocol {
		t.Fatalf("kind = %s, want protocol", k)
	}
}

// TestEmptyDiscover 说明空清单不算错误。
func TestEmptyDiscover(t *testing.T) {
	a := &autoDatagram{fn: func(req coapcodec.Message) ([][]byte, error) {
		return [][]byte{ackResponse(req, coapcodec.CodeContent, coapcodec.FormatLink, "")}, nil
	}}
	conn := openAuto(t, a, nil)
	d := conn.(interface {
		Discover(context.Context) ([]coapcodec.Link, error)
	})
	links, err := d.Discover(context.Background())
	if err != nil || links != nil {
		t.Fatalf("empty discover = %v err=%v", links, err)
	}
}

// TestPlanPointNormalizesPath 说明省略前导斜杠时会被补上。
func TestPlanPointNormalizesPath(t *testing.T) {
	p, err := planPoint(driver.PointRef{
		ID: "p", DataType: model.TypeString,
		Address: model.Address{Kind: model.AddressSymbol, Path: "sensors/temp"},
	})
	if err != nil {
		t.Fatalf("planPoint: %v", err)
	}
	if p.path != "/sensors/temp" {
		t.Fatalf("path = %q", p.path)
	}
}

// TestPathOptions 覆盖路径拆分。
func TestPathOptions(t *testing.T) {
	opts := pathOptions("/a/b")
	if len(opts) != 2 || string(opts[0].Value) != "a" || opts[1].Number != coapcodec.OptURIPath {
		t.Fatalf("opts = %+v", opts)
	}
	if got := pathOptions(""); len(got) != 0 {
		t.Fatalf("empty path opts = %+v", got)
	}
}
