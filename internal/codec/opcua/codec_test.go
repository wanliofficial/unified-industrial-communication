package opcua

import (
	"bytes"
	"math"
	"testing"
	"time"

	"uic/internal/model"
)

// 这一层的测试重点不是「能不能往返」，而是「字节布局是否符合规范」。
// 编解码器自己往返一致只能证明它自洽；与规范（以及第三方实现）对齐
// 才能证明它可互操作。因此这里对关键结构做了逐字节断言。

func TestNodeIDParseAndString(t *testing.T) {
	cases := []struct {
		in   string
		want NodeID
		str  string
	}{
		{"ns=2;s=Line1.Temperature", StringNode(2, "Line1.Temperature"), "ns=2;s=Line1.Temperature"},
		{"ns=2;i=1001", Numeric(2, 1001), "ns=2;i=1001"},
		{"i=2258", Numeric(0, 2258), "ns=0;i=2258"},
		{"s=Temperature", StringNode(0, "Temperature"), "ns=0;s=Temperature"},
		{"Temperature", StringNode(0, "Temperature"), "ns=0;s=Temperature"},
		{"", NullNodeID, "ns=0;i=0"},
		{"ns=0;i=0", NullNodeID, "ns=0;i=0"},
	}
	for _, c := range cases {
		got, err := ParseNodeID(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %+v want %+v", c.in, got, c.want)
		}
		if got.String() != c.str {
			t.Fatalf("%q: string %q want %q", c.in, got.String(), c.str)
		}
	}

	for _, bad := range []string{
		"ns=x;i=1", "ns=2", "nsu=http://x;i=1", "i=abc", "ns=2;x=1", "ns=99999;i=1",
	} {
		if _, err := ParseNodeID(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

// 数值 NodeId 的三种编码形式必须按规范选择：两字节、四字节、完整。
// 用错不会立刻出错（服务端仍能解析），但会让抓包与规范对不上，
// 排查别人的实现时反而怀疑自己。
func TestNodeIDEncodingWidths(t *testing.T) {
	cases := []struct {
		n    NodeID
		want []byte
	}{
		{Numeric(0, 7), []byte{0x00, 0x07}},
		{Numeric(3, 0x1234), []byte{0x01, 0x03, 0x34, 0x12}},
		{Numeric(2, 0x12345678), []byte{0x02, 0x02, 0x00, 0x78, 0x56, 0x34, 0x12}},
		{StringNode(2, "ab"), append([]byte{0x03, 0x02, 0x00, 0x02, 0x00, 0x00, 0x00}, 'a', 'b')},
	}
	for _, c := range cases {
		e := &encoder{}
		c.n.Encode(e)
		if !bytes.Equal(e.Bytes(), c.want) {
			t.Fatalf("%s: got % x want % x", c.n, e.Bytes(), c.want)
		}
		d := newDecoder(c.want)
		got := d.NodeID()
		if err := d.Err(); err != nil {
			t.Fatalf("%s: decode: %v", c.n, err)
		}
		if got != c.n {
			t.Fatalf("%s: round trip got %s", c.n, got)
		}
	}
}

// TestNodeIDGUIDWireEncoding 锁定 GUID NodeId 的线序：Data1/Data2/Data3 在
// 线上是小端、Data4 原样（OPC UA Part 6 §5.1.4）。
//
// 这条之前没有锚点：Encode/Decode 的 GUID 分支互逆，自洽的 round-trip 测不出
// 端序——把 Encode 退化成「直接 append 原字节」（大端）后，本包全部用例仍绿，
// 但与任何标准 OPC UA 服务端交换出的 GUID 都对不上（症状是「节点不存在」且
// 指不回配置）。GUID 在浏览结果里会出现，必须能原样透传。
//
// 取 GUID 00112233-4455-6677-8899-aabbccddeeff、ns=2：struct 按文本序（大端）
// 存 [00 11 22 33 44 55 66 77 88 99 aa bb cc dd ee ff]，线上应是
//
//	04 | 02 00 | 33 22 11 00 | 55 44 | 77 66 | 88 99 aa bb cc dd ee ff
//	编码  ns    Data1(LE)     D2(LE)  D3(LE)   Data4(原样)
func TestNodeIDGUIDWireEncoding(t *testing.T) {
	n := NodeID{
		Kind:      NodeIDGUID,
		Namespace: 2,
		GUID: [16]byte{
			0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
			0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		},
	}
	want := []byte{
		0x04, 0x02, 0x00,
		0x33, 0x22, 0x11, 0x00, // Data1 小端
		0x55, 0x44, // Data2 小端
		0x77, 0x66, // Data3 小端
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, // Data4 原样
	}
	e := &encoder{}
	n.Encode(e)
	if got := e.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("GUID encode = % x, want % x", got, want)
	}
	d := newDecoder(want)
	got := d.NodeID()
	if err := d.Err(); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != n {
		t.Fatalf("GUID round trip got %s (% x), want %s", got, got.GUID, n)
	}
}

// Null 字符串必须写成 -1（0xFFFFFFFF）而不是长度 0。两者语义上都表示
// 空，但长度 0 会让部分实现把「空 LocalizedText」与「有 Locale 无 Text」
// 混为一谈。
func TestStringNullEncoding(t *testing.T) {
	e := &encoder{}
	e.String("")
	if !bytes.Equal(e.Bytes(), []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatalf("null string = % x", e.Bytes())
	}
	e = &encoder{}
	e.String("hi")
	if !bytes.Equal(e.Bytes(), []byte{0x02, 0x00, 0x00, 0x00, 'h', 'i'}) {
		t.Fatalf("string = % x", e.Bytes())
	}
}

func TestDateTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 11, 4, 12, 34, 56, 123456700, time.UTC)
	e := &encoder{}
	e.Time(want)
	d := newDecoder(e.Bytes())
	got := d.Time()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestVariantScalarRoundTrip(t *testing.T) {
	cases := []Variant{
		{Type: TypeBoolean, Bool: true},
		{Type: TypeSByte, Int: -5},
		{Type: TypeByte, Uint: 200},
		{Type: TypeInt16, Int: -1234},
		{Type: TypeUInt16, Uint: 60000},
		{Type: TypeInt32, Int: -70000},
		{Type: TypeUInt32, Uint: 4000000000},
		{Type: TypeInt64, Int: -1 << 50},
		{Type: TypeUInt64, Uint: 1 << 60},
		{Type: TypeFloat, Float: 1.5},
		{Type: TypeDouble, Float: math.Pi},
		{Type: TypeString, Str: "hello 世界"},
		{Type: TypeByteString, Bytes: []byte{1, 2, 3}},
		{Type: TypeStatusCode, Uint: uint64(StatusBadTimeout)},
	}
	for _, c := range cases {
		e := &encoder{}
		c.Encode(e)
		d := newDecoder(e.Bytes())
		got := d.Variant()
		if err := d.Err(); err != nil {
			t.Fatalf("type %d: %v", c.Type, err)
		}
		if got.Type != c.Type {
			t.Fatalf("type %d: got type %d", c.Type, got.Type)
		}
		switch c.Type {
		case TypeBoolean:
			if got.Bool != c.Bool {
				t.Fatalf("bool %v", got.Bool)
			}
		case TypeFloat, TypeDouble:
			if got.Float != c.Float {
				t.Fatalf("float %v want %v", got.Float, c.Float)
			}
		case TypeString:
			if got.Str != c.Str {
				t.Fatalf("string %q", got.Str)
			}
		case TypeByteString:
			if !bytes.Equal(got.Bytes, c.Bytes) {
				t.Fatalf("bytes % x", got.Bytes)
			}
		default:
			if c.Int != 0 && got.Int != c.Int {
				t.Fatalf("type %d: int %d want %d", c.Type, got.Int, c.Int)
			}
			if c.Uint != 0 && got.Uint != c.Uint {
				t.Fatalf("type %d: uint %d want %d", c.Type, got.Uint, c.Uint)
			}
		}
	}
}

func TestVariantArray(t *testing.T) {
	v := Variant{Type: TypeInt32, Array: true, Values: []Variant{
		{Type: TypeInt32, Int: 1}, {Type: TypeInt32, Int: 2}, {Type: TypeInt32, Int: 3},
	}}
	e := &encoder{}
	v.Encode(e)
	// mask 0x86 = 数组 + Int32
	if e.Bytes()[0] != 0x80|TypeInt32 {
		t.Fatalf("mask = %#x", e.Bytes()[0])
	}
	d := newDecoder(e.Bytes())
	got := d.Variant()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	if !got.Array || len(got.Values) != 3 || got.Values[2].Int != 3 {
		t.Fatalf("array round trip: %+v", got)
	}
}

func TestDataValueMask(t *testing.T) {
	v := Scalar(TypeInt16)
	v.Int = 42
	dv := DataValue{Value: &v, SourceTimestamp: time.Unix(1700000000, 0).UTC()}
	e := &encoder{}
	dv.Encode(e)
	// 0x01 值 + 0x04 源时间；没有状态码与服务器时间。
	if e.Bytes()[0] != 0x05 {
		t.Fatalf("mask = %#x", e.Bytes()[0])
	}
	d := newDecoder(e.Bytes())
	got := d.DataValue()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	if got.Value == nil || got.Value.Type != TypeInt16 || got.Value.Int != 42 {
		t.Fatalf("value: %+v", got.Value)
	}
	if !got.SourceTimestamp.Equal(dv.SourceTimestamp) {
		t.Fatalf("source time %v", got.SourceTimestamp)
	}
	if got.Status != 0 {
		t.Fatalf("status should be absent, got %v", got.Status)
	}
}

func TestVariantDecodeRejectsBadType(t *testing.T) {
	// 类型 40 不在内置类型表里；必须报错而不是静默当成标量继续读。
	d := newDecoder([]byte{40, 0, 0, 0, 0})
	_ = d.Variant()
	if d.Err() == nil {
		t.Fatal("unknown variant type must fail")
	}
}

func TestArrayLengthGuard(t *testing.T) {
	// 声称有 1e9 个元素必须被上界拦住，否则一次坏报文就能吃光内存。
	d := newDecoder([]byte{0x00, 0x00, 0x00, 0x40})
	if n := d.decodeArrayLen(1024); n != 0 {
		t.Fatalf("length = %d", n)
	}
	if d.Err() == nil {
		t.Fatal("oversized array must fail")
	}
}

func TestExtensionObjectRoundTrip(t *testing.T) {
	x := NewExtensionObject(SvcReadRequest, []byte{1, 2, 3, 4})
	e := &encoder{}
	x.Encode(e)
	d := newDecoder(e.Bytes())
	got := d.ExtensionObject()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	if got.TypeID.ID != SvcReadRequest || got.Encoding != 0x01 || !bytes.Equal(got.Body, x.Body) {
		t.Fatalf("got %+v", got)
	}
}

// varint 风格的长度字段是协议解析器最常见的攻击面，这里显式覆盖
// 「长度声称超过剩余字节」的情况。
func TestDecoderShortBuffer(t *testing.T) {
	d := newDecoder([]byte{0x0A, 0x00, 0x00, 0x00, 'a'})
	if s := d.String(); s != "" {
		t.Fatalf("string = %q", s)
	}
	if d.Err() == nil {
		t.Fatal("truncated string must fail")
	}
}

func TestHelloAndAck(t *testing.T) {
	frame := EncodeHello(Hello{
		Version: 0, ReceiveBufSize: 65535, SendBufSize: 65535,
		MaxMessageSize: 0, MaxChunkCount: 0, EndpointURL: "opc.tcp://127.0.0.1:4840",
	})
	h, err := DecodeConnHeader(frame[:8])
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != MsgHello || h.Size != uint32(len(frame)) {
		t.Fatalf("header %+v", h)
	}
	got, err := DecodeHello(frame[8:])
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiveBufSize != 65535 || got.EndpointURL != "opc.tcp://127.0.0.1:4840" {
		t.Fatalf("hello %+v", got)
	}

	ack := EncodeAcknowledge(Acknowledge{Version: 0, ReceiveBufSize: 8192, SendBufSize: 8192})
	ah, err := DecodeConnHeader(ack[:8])
	if err != nil {
		t.Fatal(err)
	}
	if ah.Type != MsgAck {
		t.Fatalf("type %q", ah.Type)
	}
	gotAck, err := DecodeAcknowledge(ack[8:])
	if err != nil {
		t.Fatal(err)
	}
	if gotAck.ReceiveBufSize != 8192 {
		t.Fatalf("ack %+v", gotAck)
	}
}

func TestSecureMessageRoundTrip(t *testing.T) {
	body := []byte{9, 9, 9}
	frame := EncodeSecureMessage(MsgMessage, 7, 3,
		SequenceHeader{Number: 11, RequestID: 12}, SvcReadRequest, body)
	m, err := DecodeSecureMessage(frame)
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgMessage || m.ChannelID != 7 || m.TokenID != 3 {
		t.Fatalf("message %+v", m)
	}
	if m.Seq.Number != 11 || m.Seq.RequestID != 12 {
		t.Fatalf("seq %+v", m.Seq)
	}
	if m.ServiceID != SvcReadRequest || !bytes.Equal(m.Body, body) {
		t.Fatalf("service %d body % x", m.ServiceID, m.Body)
	}

	open := EncodeOpenMessage(0, AsymmetricSecurityHeader{PolicyURI: PolicyNone},
		SequenceHeader{Number: 1, RequestID: 2}, SvcOpenSecureChannelRequest, body)
	om, err := DecodeSecureMessage(open)
	if err != nil {
		t.Fatal(err)
	}
	if om.Security.PolicyURI != PolicyNone || om.ServiceID != SvcOpenSecureChannelRequest {
		t.Fatalf("open %+v", om)
	}
}

// 分块响应在本实现里直接报错：静默丢掉后半段会让客户端把「半个值」
// 当成完整值，比失败更危险。
func TestSecureMessageRejectsChunks(t *testing.T) {
	frame := EncodeSecureMessage(MsgMessage, 1, 1,
		SequenceHeader{Number: 1, RequestID: 1}, SvcReadResponse, nil)
	frame[3] = ChunkIntermediate
	if _, err := DecodeSecureMessage(frame); err == nil {
		t.Fatal("chunked message must be rejected")
	}
}

func TestReadRequestGolden(t *testing.T) {
	req := ReadRequest{
		Header: RequestHeader{
			AuthenticationToken: Numeric(1, 5),
			Timestamp:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			RequestHandle:       3,
		},
		MaxAge:             0,
		TimestampsToReturn: TimestampsBoth,
		Nodes: []ReadValueID{{
			NodeID:      StringNode(2, "T"),
			AttributeID: AttrValue,
		}},
	}
	body := req.Encode()
	got, err := DecodeReadRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.RequestHandle != 3 || len(got.Nodes) != 1 {
		t.Fatalf("decoded %+v", got)
	}
	if got.Nodes[0].NodeID.String() != "ns=2;s=T" || got.Nodes[0].AttributeID != AttrValue {
		t.Fatalf("node %+v", got.Nodes[0])
	}
	if got.TimestampsToReturn != TimestampsBoth {
		t.Fatalf("timestamps %d", got.TimestampsToReturn)
	}
}

func TestWriteResponseRoundTrip(t *testing.T) {
	resp := WriteResponse{
		Header:  ResponseHeader{RequestHandle: 7, ServiceResult: StatusGood},
		Results: []StatusCode{StatusGood, StatusBadNotWritable, StatusBadNodeIDUnknown},
	}
	got, err := DecodeWriteResponse(resp.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.RequestHandle != 7 || len(got.Results) != 3 {
		t.Fatalf("decoded %+v", got)
	}
	if got.Results[1] != StatusBadNotWritable {
		t.Fatalf("result[1] = %v", got.Results[1])
	}
}

func TestBrowseRoundTrip(t *testing.T) {
	resp := BrowseResponse{
		Header: ResponseHeader{RequestHandle: 1},
		Results: []BrowseResult{{
			Status: StatusGood,
			References: []ReferenceDescription{{
				ReferenceTypeID: Numeric(0, RefOrganizes),
				IsForward:       true,
				Target:          ExpandedNodeID{NodeID: StringNode(2, "Plant")},
				BrowseName:      QualifiedName{Namespace: 2, Name: "Plant"},
				DisplayName:     LocalizedText{Text: "Plant"},
				NodeClass:       NodeClassObject,
				TypeDefinition:  ExpandedNodeID{NodeID: Numeric(0, 58)},
			}},
		}},
	}
	got, err := DecodeBrowseResponse(resp.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || len(got.Results[0].References) != 1 {
		t.Fatalf("decoded %+v", got)
	}
	ref := got.Results[0].References[0]
	if ref.Target.NodeID.String() != "ns=2;s=Plant" || ref.BrowseName.Name != "Plant" {
		t.Fatalf("ref %+v", ref)
	}
}

func TestCreateSessionResponseRoundTrip(t *testing.T) {
	resp := CreateSessionResponse{
		Header:                ResponseHeader{RequestHandle: 5},
		SessionID:             Numeric(1, 9),
		AuthenticationToken:   Numeric(1, 9),
		RevisedSessionTimeout: 60000,
		ServerEndpoints: []EndpointDescription{{
			EndpointURL:       "opc.tcp://h:4840",
			SecurityMode:      SecurityModeNone,
			SecurityPolicyURI: PolicyNone,
			UserIdentityTokens: []UserTokenPolicy{{
				PolicyID: "anonymous", TokenType: TokenTypeAnonymous,
			}},
			TransportProfileURI: TransportBinary,
		}},
	}
	got, err := DecodeCreateSessionResponse(resp.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID.ID != 9 || got.AuthenticationToken.ID != 9 {
		t.Fatalf("ids %+v %+v", got.SessionID, got.AuthenticationToken)
	}
	if len(got.ServerEndpoints) != 1 {
		t.Fatalf("endpoints %d", len(got.ServerEndpoints))
	}
	if policy, ok := anonymousPolicyIDOf(got.ServerEndpoints); !ok || policy != "anonymous" {
		t.Fatalf("policy %q %v", policy, ok)
	}
}

// anonymousPolicyIDOf 只是把驱动里的同名逻辑搬到测试里做交叉验证，
// 避免测试与实现共用同一处 bug。
func anonymousPolicyIDOf(eps []EndpointDescription) (string, bool) {
	for _, ep := range eps {
		for _, t := range ep.UserIdentityTokens {
			if t.TokenType == TokenTypeAnonymous {
				return t.PolicyID, true
			}
		}
	}
	return "", false
}

func TestActivateSessionAnonymousToken(t *testing.T) {
	x := AnonymousIdentityToken("anonymous")
	if x.TypeID.ID != 321 || x.Encoding != 0x01 {
		t.Fatalf("token %+v", x)
	}
	policy, err := DecodeAnonymousIdentityToken(x)
	if err != nil {
		t.Fatal(err)
	}
	if policy != "anonymous" {
		t.Fatalf("policy %q", policy)
	}
}

func TestServiceFaultRoundTrip(t *testing.T) {
	f := ServiceFault{Header: ResponseHeader{
		RequestHandle: 4,
		ServiceResult: StatusBadSessionIDInvalid,
		StringTable:   []string{"session not found"},
	}}
	got, err := DecodeServiceFault(f.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.ServiceResult != StatusBadSessionIDInvalid || len(got.Header.StringTable) != 1 {
		t.Fatalf("fault %+v", got.Header)
	}
}

func TestStatusCodeClasses(t *testing.T) {
	if !StatusGood.IsGood() || StatusGood.IsBad() {
		t.Fatal("Good misclassified")
	}
	if StatusGoodClamped.IsGood() == false {
		t.Fatal("GoodClamped must be good")
	}
	if !StatusBadTimeout.IsBad() {
		t.Fatal("BadTimeout must be bad")
	}
	if StatusCode(0x40000000).IsUncertain() == false {
		t.Fatal("Uncertain must be uncertain")
	}
	if StatusBadNodeIDUnknown.Text() != "BadNodeIdUnknown" {
		t.Fatalf("text %q", StatusBadNodeIDUnknown.Text())
	}
}

func TestQualityMap(t *testing.T) {
	cases := map[StatusCode]model.Quality{
		StatusGood:              model.QualityGood,
		StatusGoodClamped:       model.QualityGood,
		StatusCode(0x40000000):  model.QualityUncertain,
		StatusBadTimeout:        model.QualityBad,
		StatusBadWaitingForData: model.QualityStale,
		StatusBadNotConnected:   model.QualityCommError,
		StatusBadNodeIDUnknown:  model.QualityAddressError,
	}
	for s, want := range cases {
		if got := QualityOf(s); got != want {
			t.Fatalf("%v: %s want %s", s, got, want)
		}
	}
}
