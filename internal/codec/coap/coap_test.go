package coap

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// TestGoldenGet 用一条真实抓包里的报文钉住头部布局。
//
// `40 01 12 34 b4 74 65 73 74` 是最经典的 CoAP 请求：CON、TKL=0、
// Code=0.01、MID=0x1234、Uri-Path="test"。任何头部字段的位置写错，
// 这一串字节都对不上——比逐个字段断言更能抓住「版本/类型/TKL 打包」
// 这类整体性错误。
func TestGoldenGet(t *testing.T) {
	raw := mustHex(t, "40011234b474657374")
	m, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Type != TypeConfirmable {
		t.Fatalf("type = %v, want CON", m.Type)
	}
	if m.Code != CodeGET {
		t.Fatalf("code = %v, want GET", m.Code)
	}
	if m.MID != 0x1234 {
		t.Fatalf("mid = %#x, want 0x1234", m.MID)
	}
	if len(m.Token) != 0 {
		t.Fatalf("token = %x, want empty", m.Token)
	}
	if got := m.Path(); got != "/test" {
		t.Fatalf("path = %q, want /test", got)
	}

	// 反方向：同一条报文重新编码必须逐字节相同。
	out, err := Encode(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("re-encode = %x, want %x", out, raw)
	}
}

// TestOptionsExtension 覆盖 delta/length 的 13/14 扩展。
//
// 两个扩展字节是「先 delta 后 length」依次排列，不是一个整体；把顺序搞反
// 会在同一选项上同时出现两种扩展时立刻错位，所以这里专门构造一个 14/14
// 的选项钉住它。
func TestOptionsExtension(t *testing.T) {
	big := bytes.Repeat([]byte{0x5A}, 300)
	m := Message{
		Type: TypeNonConfirmable, Code: CodePOST, MID: 7,
		Token:   []byte{0xAA, 0xBB},
		Options: []Option{{Number: 300, Value: big}},
	}
	raw, err := Encode(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 头 4 字节 + token 2 字节 + 选项头 1 字节 + delta 扩展 2 + length 扩展 2
	// + 值 300 字节 = 311。
	if len(raw) != 4+2+1+2+2+300 {
		t.Fatalf("encoded length = %d, want 311", len(raw))
	}
	if raw[6] != 0xEE {
		t.Fatalf("option head = %#x, want 0xEE (delta 14, length 14)", raw[6])
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Options) != 1 || got.Options[0].Number != 300 {
		t.Fatalf("options = %+v, want one option #300", got.Options)
	}
	if !bytes.Equal(got.Options[0].Value, big) {
		t.Fatalf("option value mismatch: %d bytes", len(got.Options[0].Value))
	}

	// 13 扩展：delta 13..268、length 13..268。
	mid := Message{Type: TypeAck, Code: CodeContent, MID: 9}
	mid.SetUintOption(OptSize1, 0xABCD) // 值长度 2，选项号 60
	mid.SetOption(OptIfMatch, bytes.Repeat([]byte{1}, 40))
	r2, err := Encode(mid)
	if err != nil {
		t.Fatalf("encode 13-ext: %v", err)
	}
	back, err := Decode(r2)
	if err != nil {
		t.Fatalf("decode 13-ext: %v", err)
	}
	if v, ok := back.UintOption(OptSize1); !ok || v != 0xABCD {
		t.Fatalf("Size1 = %#x ok=%v", v, ok)
	}
	if f, ok := back.Get(OptIfMatch); !ok || len(f) != 40 {
		t.Fatalf("If-Match len = %d ok=%v", len(f), ok)
	}
}

// TestZeroLengthIntegerOption 钉住「0 编码成零长度」。
//
// Content-Format=0（text/plain）在报文里是**没有值的选项**，不是
// `0x00`。给 0 补一个字节会让后面的 delta 计算整体偏移一格。
func TestZeroLengthIntegerOption(t *testing.T) {
	m := Message{Type: TypeConfirmable, Code: CodeContent, MID: 1}
	m.SetUintOption(OptContentFormat, uint32(FormatTextPlain))
	raw, err := Encode(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 4 字节头 + 1 字节选项头（delta 12, length 0）。没有值字节。
	if len(raw) != 5 {
		t.Fatalf("encoded = %x (len %d), want 5 bytes", raw, len(raw))
	}
	want := mustHex(t, "40450001c0")
	if !bytes.Equal(raw, want) {
		t.Fatalf("encoded = %x, want %x", raw, want)
	}
	back, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := back.UintOption(OptContentFormat)
	if !ok || v != 0 {
		t.Fatalf("content-format = %d ok=%v, want 0", v, ok)
	}
}

// TestEncodeSortsOptions 说明「选项必须升序」在编码端是被自动满足的。
//
// 调用方可以把选项乱序塞进来；编码器排序后再做 delta，这样上层不需要
// 记住选项号顺序。解码端永远不会看到递减的 delta（它是无符号的），所以
// 「乱序」在协议层不可表达——这是一条值得写进测试的事实。
func TestEncodeSortsOptions(t *testing.T) {
	m := Message{
		Type: TypeConfirmable, Code: CodeGET, MID: 2,
		Options: []Option{
			{Number: OptURIPath, Value: []byte("a")},
			{Number: OptContentFormat, Value: nil},
			{Number: OptURIPath, Value: []byte("b")},
		},
	}
	raw, err := Encode(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 不同选项号必须升序，同一选项号的多个段必须保持插入顺序。
	if got := back.Path(); got != "/a/b" {
		t.Fatalf("path = %q, want /a/b", got)
	}
	for i := 1; i < len(back.Options); i++ {
		if back.Options[i].Number < back.Options[i-1].Number {
			t.Fatalf("options not ascending: %d then %d", back.Options[i-1].Number, back.Options[i].Number)
		}
	}
	if back.Options[len(back.Options)-1].Number != OptContentFormat {
		t.Fatalf("last option = %d, want %d", back.Options[len(back.Options)-1].Number, OptContentFormat)
	}
}

// TestDecodeErrors 覆盖畸形报文。UDP 的报文边界由内核保证，所以这里
// 把「多一个字节 / 少一个字节 / 保留 nibble / 载荷标记后为空」都当成
// 硬错误，而不是尽力而为地截断解析。
func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name string
		hex  string
	}{
		{"too short", "4001"},
		{"bad version", "80011234"},
		{"token truncated", "44011234aa"},
		{"token too long", "49011234aabbccddeeff001122"},
		{"payload marker only", "40011234ff"},
		{"reserved nibble delta", "40011234f0"},
		{"reserved nibble length", "400112340f"},
		{"option value truncated", "40011234b47465"},
		{"delta extension truncated", "40011234d0"},
		{"length extension truncated", "400112340d"},
		{"option number overflow", ""}, // 见下方单独构造
	}
	for _, c := range cases {
		if c.hex == "" {
			continue
		}
		if _, err := Decode(mustHex(t, c.hex)); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}

	// 选项号溢出：前一个选项 65530，delta 14 扩展给 65535，两者相加 > 65535。
	b := mustHex(t, "40011234e0ffff")
	if _, err := Decode(b); err == nil {
		t.Error("option number overflow: expected error")
	}
}

// TestEncodeErrors 覆盖编码端的约束。
func TestEncodeErrors(t *testing.T) {
	if _, err := Encode(Message{Code: CodeGET, Token: bytes.Repeat([]byte{1}, 9)}); err == nil {
		t.Error("token length 9: expected error")
	}
	if _, err := Encode(Message{Code: CodeEmpty, Options: []Option{{Number: OptURIPath, Value: []byte("x")}}}); err == nil {
		t.Error("empty code with options: expected error")
	}
	if _, err := Encode(Message{Code: CodeEmpty, Payload: []byte("x")}); err == nil {
		t.Error("empty code with payload: expected error")
	}
	// CodeEmpty 空报文（ACK/RST/ping）必须能编码。
	if _, err := Encode(Message{Type: TypeAck, Code: CodeEmpty, MID: 3}); err != nil {
		t.Errorf("empty ACK: %v", err)
	}
}

// TestBlock 覆盖 Block1/Block2 的值编码。
func TestBlock(t *testing.T) {
	cases := []struct {
		num  uint32
		more bool
		size uint32
		want string
	}{
		{0, false, 16, ""},
		{0, true, 16, "08"},
		{1, false, 16, "10"},
		{16, false, 1024, "0106"},
		{0xFFFFF, true, 1024, "fffffe"}, // 20 位块号上限
	}
	for _, c := range cases {
		got, err := EncodeBlock(Block{Num: c.num, More: c.more, Size: c.size})
		if err != nil {
			t.Fatalf("encode block %+v: %v", c, err)
		}
		if hex.EncodeToString(got) != c.want {
			t.Fatalf("encode block %+v = %x, want %s", c, got, c.want)
		}
		back, err := DecodeBlock(got)
		if err != nil {
			t.Fatalf("decode block %s: %v", c.want, err)
		}
		if back.Num != c.num || back.More != c.more || back.Size != c.size {
			t.Fatalf("roundtrip block %+v = %+v", c, back)
		}
	}

	if _, err := EncodeBlock(Block{Size: 100}); err == nil {
		t.Error("block size 100: expected error")
	}
	if _, err := EncodeBlock(Block{Size: 2048}); err == nil {
		t.Error("block size 2048: expected error")
	}
	if _, err := DecodeBlock([]byte{1, 2, 3, 4}); err == nil {
		t.Error("block option > 3 bytes: expected error")
	}
	// SZX=7（保留值）。
	if _, err := DecodeBlock([]byte{0x07}); err == nil {
		t.Error("block SZX 7: expected error")
	}
	// 零长度 Block 选项等价于 Num=0,M=false,Size=16。
	b, err := DecodeBlock(nil)
	if err != nil || b.Num != 0 || b.More || b.Size != 16 {
		t.Fatalf("empty block = %+v err=%v", b, err)
	}
}

// TestSplitURI 覆盖地址解析。协议前缀可选；没有端口时补 5683；
// IPv6 字面量要加方括号。
func TestSplitURI(t *testing.T) {
	cases := []struct {
		in    string
		host  string
		path  string
		query string
		err   bool
	}{
		{"coap://127.0.0.1/sensors/temp", "127.0.0.1:5683", "/sensors/temp", "", false},
		{"coap://127.0.0.1:5684/a?x=1&y=2", "127.0.0.1:5684", "/a", "x=1&y=2", false},
		{"127.0.0.1/a", "127.0.0.1:5683", "/a", "", false},
		{"coap://example.com", "example.com:5683", "/", "", false},
		{"coap://[::1]/a", "[::1]:5683", "/a", "", false},
		{"coap://[::1]:5683/a", "[::1]:5683", "/a", "", false},
		{"coap://host/a#frag", "host:5683", "/a", "", false},
		{"http://host/a", "", "", "", true},
		{"", "", "", "", true},
		{"coap://", "", "", "", true},
	}
	for _, c := range cases {
		h, p, q, err := SplitURI(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if h != c.host || p != c.path || q != c.query {
			t.Errorf("%q = (%q,%q,%q), want (%q,%q,%q)", c.in, h, p, q, c.host, c.path, c.query)
		}
	}
}

// TestHostPort 覆盖配置里常见的 `host` / `host:port` 写法。
func TestHostPort(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":      "127.0.0.1:5683",
		"127.0.0.1:9999": "127.0.0.1:9999",
		"[::1]":          "[::1]:5683",
		"[::1]:5684":     "[::1]:5684",
		"example.com":    "example.com:5683",
		" example.com ":  "example.com:5683",
	}
	for in, want := range cases {
		got, err := HostPort(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "::1"} {
		if _, err := HostPort(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

// TestParseLinkFormat 覆盖资源发现返回的 link-format。
//
// 引号里的 `;` 与 `,` 不是分隔符，转义（`\"`、`\\`）必须还原；如果按字符串
// 直接 split，会把 `rt="a;b"` 切坏，运维看到的资源清单就少了一半参数。
func TestParseLinkFormat(t *testing.T) {
	raw := `</sensors/temp>;rt="temperature-c";if="sensor",</sensors/light>;rt="light-lux";if="sensor";obs,</cfg>;ct=50,</title>;title="x\;y\,z"`
	links, err := ParseLinkFormat([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(links) != 4 {
		t.Fatalf("links = %d, want 4", len(links))
	}
	if links[0].Path != "/sensors/temp" || links[0].Params["rt"] != "temperature-c" || links[0].Params["if"] != "sensor" {
		t.Fatalf("link[0] = %+v", links[0])
	}
	if links[1].Params["obs"] != "" {
		t.Fatalf("link[1] obs param = %q, want empty (flag)", links[1].Params["obs"])
	}
	if links[2].Params["ct"] != "50" {
		t.Fatalf("link[2] ct = %q", links[2].Params["ct"])
	}
	if links[3].Path != "/title" {
		t.Fatalf("link[3] path = %q", links[3].Path)
	}
	if links[3].Params["title"] != `x;y,z` {
		t.Fatalf("link[3] title = %q", links[3].Params["title"])
	}

	if got, err := ParseLinkFormat(nil); err != nil || got != nil {
		t.Fatalf("empty = %v err=%v", got, err)
	}
	for _, bad := range []string{"no-angle", "<no-close", `</a>;x="unterminated`} {
		if _, err := ParseLinkFormat([]byte(bad)); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

// TestPayload 覆盖「有载荷必须有标记、有标记必须有载荷」。
func TestPayload(t *testing.T) {
	m := Message{Type: TypeConfirmable, Code: CodePUT, MID: 10, Payload: []byte(`{"v":1}`)}
	m.SetPath("/cfg")
	m.SetUintOption(OptContentFormat, uint32(FormatJSON))
	raw, err := Encode(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 载荷标记 0xFF 只能出现在选项区之后、且后面必须有字节。
	idx := bytes.IndexByte(raw, 0xFF)
	if idx < 0 || idx == len(raw)-1 {
		t.Fatalf("payload marker missing or dangling: %x", raw)
	}
	back, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(back.Payload, m.Payload) {
		t.Fatalf("payload = %q, want %q", back.Payload, m.Payload)
	}
	if back.Path() != "/cfg" {
		t.Fatalf("path = %q", back.Path())
	}
	if v, _ := back.UintOption(OptContentFormat); v != uint32(FormatJSON) {
		t.Fatalf("content-format = %d", v)
	}
}

func TestCodeHelpers(t *testing.T) {
	if CodeGET.Class() != 0 || CodeGET.Detail() != 1 {
		t.Fatalf("GET class/detail = %d/%d", CodeGET.Class(), CodeGET.Detail())
	}
	if !CodeContent.Success() || CodeNotFound.Success() {
		t.Fatal("success classification wrong")
	}
	if !CodeGET.IsRequest() || CodeContent.IsRequest() {
		t.Fatal("request classification wrong")
	}
	if !CodeContent.IsResponse() || CodeGET.IsResponse() {
		t.Fatal("response classification wrong")
	}
	if got := CodeContent.String(); got != "2.05 Content" {
		t.Fatalf("content string = %q", got)
	}
	if got := TypeReset.String(); got != "RST" {
		t.Fatalf("reset string = %q", got)
	}
	if got := ContentFormatName(FormatJSON); got != "application/json" {
		t.Fatalf("format name = %q", got)
	}
	if got := ContentFormatName(9999); !strings.Contains(got, "9999") {
		t.Fatalf("unknown format name = %q", got)
	}
}

// TestEncodeUint 钉住最短大端编码。
func TestEncodeUint(t *testing.T) {
	cases := map[uint32]string{
		0:        "",
		1:        "01",
		255:      "ff",
		256:      "0100",
		65535:    "ffff",
		0x123456: "123456",
	}
	for v, want := range cases {
		if got := hex.EncodeToString(EncodeUint(v)); got != want {
			t.Errorf("EncodeUint(%d) = %q, want %q", v, got, want)
		}
		if back := DecodeUint(EncodeUint(v)); back != v {
			t.Errorf("DecodeUint(EncodeUint(%d)) = %d", v, back)
		}
	}
}

// TestQueryAndPath 覆盖路径/查询串的拆分与重组。
func TestQueryAndPath(t *testing.T) {
	m := Message{Type: TypeConfirmable, Code: CodeGET, MID: 4}
	m.SetPath("/a/b")
	m.SetQuery("x=1&y=2")
	if got := m.Path(); got != "/a/b" {
		t.Fatalf("path = %q", got)
	}
	if got := m.Query(); got != "x=1&y=2" {
		t.Fatalf("query = %q", got)
	}

	// 空路径 → 根；空段被跳过。
	root := Message{Type: TypeConfirmable, Code: CodeGET}
	root.SetPath("")
	if got := root.Path(); got != "/" {
		t.Fatalf("root path = %q", got)
	}
	root.SetPath("//a//b/")
	if got := root.Path(); got != "/a/b" {
		t.Fatalf("skipped segments path = %q", got)
	}
	root.SetQuery("")
	if got := root.Query(); got != "" {
		t.Fatalf("empty query = %q", got)
	}
}

// TestCodeNames 遍历所有已定义的码，确保名字表与编码一一对应。
//
// 这类表驱动测试的价值不在「断言某个字符串」，而在于**没有遗漏**：
// 新增一个码却忘了加名字，或名字拼错到与协议语义不符，都会在这里暴露。
func TestCodeNames(t *testing.T) {
	codes := []Code{
		CodeEmpty, CodeGET, CodePOST, CodePUT, CodeDELETE,
		CodeCreated, CodeDeleted, CodeValid, CodeChanged, CodeContent, CodeContinue,
		CodeBadRequest, CodeUnauthorized, CodeBadOption, CodeForbidden, CodeNotFound,
		CodeMethodNotAllowed, CodeNotAcceptable, CodePreconditionFailed,
		CodeRequestEntityTooLarge, CodeUnsupportedFormat,
		CodeInternalError, CodeNotImplemented, CodeBadGateway, CodeUnavailable,
		CodeGatewayTimeout, CodeProxyingNotSup,
	}
	for _, c := range codes {
		if CodeName(c) == "" {
			t.Errorf("code %#x has no name", byte(c))
		}
	}
	if CodeName(Code(0x1F)) != "" {
		t.Error("unknown code should have empty name")
	}

	formats := []uint16{FormatTextPlain, FormatJSON, FormatCBOR, FormatOctets, FormatLink}
	for _, f := range formats {
		if ContentFormatName(f) == "" {
			t.Errorf("format %d has no name", f)
		}
	}

	types := []Type{TypeConfirmable, TypeNonConfirmable, TypeAck, TypeReset}
	for _, ty := range types {
		if ty.String() == "" {
			t.Errorf("type %d has no name", ty)
		}
	}
	if got := Type(9).String(); got != "type(9)" {
		t.Fatalf("unknown type = %q", got)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}
