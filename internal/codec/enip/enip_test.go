package enip

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"uic/internal/driver"
)

func TestHeaderRoundTrip(t *testing.T) {
	ctx := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	pkt, err := BuildPacket(CmdRegisterSession, 0x11223344, 0, ctx, []byte{0, 1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt) != HeaderLen+4 {
		t.Fatalf("len = %d", len(pkt))
	}
	h, err := ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if h.Command != CmdRegisterSession || h.Length != 4 || h.Session != 0x11223344 || h.Context != ctx {
		t.Fatalf("header = %+v", h)
	}
	// 长度字段只算负载，不含 24 字节头：按整包长度填会让对端一直等数据。
	if h.Length != uint16(len(pkt)-HeaderLen) {
		t.Fatalf("length = %d", h.Length)
	}
}

func TestParseHeaderRejectsShort(t *testing.T) {
	if _, err := ParseHeader(make([]byte, HeaderLen-1)); err == nil {
		t.Fatal("short header accepted")
	}
}

func TestBuildPacketTooLarge(t *testing.T) {
	if _, err := BuildPacket(CmdSendRRData, 0, 0, [8]byte{}, make([]byte, MaxPayload)); err != nil {
		t.Fatalf("boundary rejected: %v", err)
	}
	if _, err := BuildPacket(CmdSendRRData, 0, 0, [8]byte{}, make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestCPFOddItemIsPadded(t *testing.T) {
	items := []Item{
		{Type: ItemNullAddress},
		{Type: ItemUnconnectedData, Data: []byte{1, 2, 3}},
	}
	b, err := BuildCPF(items)
	if err != nil {
		t.Fatal(err)
	}
	// 8 字节头 + (4+0) + (4+3+1 补齐) = 20
	if len(b) != 20 {
		t.Fatalf("len = %d, want 20", len(b))
	}
	got, err := ParseCPF(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Type != ItemUnconnectedData || !bytes.Equal(got[1].Data, []byte{1, 2, 3}) {
		t.Fatalf("items = %+v", got)
	}
}

func TestParseCPFRejectsOverrun(t *testing.T) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[6:], 1)
	b = append(b, 0xB2, 0x00, 0xFF, 0x00) // 声称 255 字节，实际没有
	if _, err := ParseCPF(b); err == nil {
		t.Fatal("overrun accepted")
	}
	if _, err := ParseCPF(make([]byte, 7)); err == nil {
		t.Fatal("short CPF accepted")
	}
}

func TestSendRRDataRoundTrip(t *testing.T) {
	cip := []byte{0x4C, 0x02, 0x91, 0x01, 'A', 0x00, 0x01, 0x00}
	payload, err := BuildSendRRData(cip)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSendRRData(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, cip) {
		t.Fatalf("cip = % x", got)
	}
	// 没有数据条目必须报错，而不是当成空报文继续解析。
	empty, _ := BuildCPF([]Item{{Type: ItemNullAddress}})
	if _, err := ParseSendRRData(empty); err == nil {
		t.Fatal("missing data item accepted")
	}
}

func TestRegisterSessionPayload(t *testing.T) {
	v, o, err := ParseRegisterSession(BuildRegisterSession(1))
	if err != nil || v != 1 || o != 0 {
		t.Fatalf("version/options = %d/%d err=%v", v, o, err)
	}
	if _, _, err := ParseRegisterSession([]byte{1, 0}); err == nil {
		t.Fatal("short payload accepted")
	}
}

func TestBuildRequestValidation(t *testing.T) {
	if _, err := BuildRequest(0x4C, nil, nil); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := BuildRequest(0x4C, []byte{0x91}, nil); err == nil {
		t.Fatal("odd path accepted")
	}
	if _, err := BuildRequest(0x4C, make([]byte, 512), nil); err == nil {
		t.Fatal("overlong path accepted")
	}
	req, err := BuildRequest(0x4C, []byte{0x20, 0x02}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	// 服务 + 路径字数(1) + 路径 + 数据
	if req[0] != 0x4C || req[1] != 1 || len(req) != 5 {
		t.Fatalf("request = % x", req)
	}
}

func TestParseRequest(t *testing.T) {
	req, err := ParseRequest([]byte{0x4C, 0x01, 0x20, 0x02, 0xAA})
	if err != nil {
		t.Fatal(err)
	}
	if req.Service != 0x4C || !bytes.Equal(req.Path, []byte{0x20, 0x02}) || !bytes.Equal(req.Data, []byte{0xAA}) {
		t.Fatalf("request = %+v", req)
	}
	if _, err := ParseRequest([]byte{0x4C}); err == nil {
		t.Fatal("truncated request accepted")
	}
	if _, err := ParseRequest([]byte{0x4C, 0x02, 0x20}); err == nil {
		t.Fatal("path overrun accepted")
	}
	if _, err := ParseRequest([]byte{0x4C, 0x00}); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestReplyRoundTripWithExtra(t *testing.T) {
	b, err := BuildReply(ServiceReadTag, 0x05, []byte{0x01, 0x00}, []byte{0xDE, 0xAD})
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 0x4C|ServiceReplyMask {
		t.Fatalf("service = %02X", b[0])
	}
	r, err := ParseReply(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 0x05 || !bytes.Equal(r.Extra, []byte{0x01, 0x00}) || !bytes.Equal(r.Data, []byte{0xDE, 0xAD}) {
		t.Fatalf("reply = %+v", r)
	}
	if _, err := BuildReply(ServiceReadTag, 0, []byte{1}, nil); err == nil {
		t.Fatal("odd additional status accepted")
	}
}

func TestParseReplyRejectsTruncated(t *testing.T) {
	if _, err := ParseReply([]byte{0xCC, 0, 0}); err == nil {
		t.Fatal("short reply accepted")
	}
	// 附加状态声称 4 字（8 字节），实际只有 0 字节。
	if _, err := ParseReply([]byte{0xCC, 0, 0, 4}); err == nil {
		t.Fatal("additional status overrun accepted")
	}
}

func TestStatusKindAndText(t *testing.T) {
	cases := []struct {
		code byte
		kind driver.ErrorKind
	}{
		{0x04, driver.ErrAddress},
		{0x05, driver.ErrAddress},
		{0x15, driver.ErrAddress},
		{0x16, driver.ErrAddress},
		{0x03, driver.ErrAddress},
		{0x06, driver.ErrAddress},
		{0x11, driver.ErrAddress},
		{0x08, driver.ErrUnsupported},
		{0x0E, driver.ErrPermission},
		{0x01, driver.ErrDisconnected},
		{0x07, driver.ErrDisconnected},
		{0x1F, driver.ErrProtocol},
	}
	for _, c := range cases {
		e := &StatusError{Service: ServiceReadTag, Code: c.code}
		if e.Kind() != c.kind {
			t.Fatalf("code %02X kind = %v, want %v", c.code, e.Kind(), c.kind)
		}
		if !strings.Contains(e.Error(), "0x") {
			t.Fatalf("error text = %q", e.Error())
		}
		if StatusText(c.code) == "unknown CIP status" {
			t.Fatalf("code %02X has no text", c.code)
		}
	}
	if StatusText(0xFE) != "unknown CIP status" {
		t.Fatalf("unknown text = %q", StatusText(0xFE))
	}
	for code, want := range map[uint32]string{
		0x01: "invalid command", 0x64: "invalid session handle", 0x69: "unsupported protocol revision",
	} {
		if EncapsulationStatusText(code) != want {
			t.Fatalf("encap %02X = %q", code, EncapsulationStatusText(code))
		}
	}
}

func TestSymbolicPathEncoding(t *testing.T) {
	p, err := SymbolicPath("A")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, []byte{0x91, 0x01, 'A', 0x00}) {
		t.Fatalf("path = % x", p)
	}
	p, err = SymbolicPath("AB")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, []byte{0x91, 0x02, 'A', 'B'}) {
		t.Fatalf("path = % x", p)
	}
	name, err := ParseSymbolicPath(p)
	if err != nil || name != "AB" {
		t.Fatalf("name = %q err=%v", name, err)
	}
	if _, err := SymbolicPath(""); err == nil {
		t.Fatal("empty tag accepted")
	}
	if _, err := SymbolicPath("a\x00b"); err == nil {
		t.Fatal("control char accepted")
	}
	if _, err := SymbolicPath(strings.Repeat("A", 256)); err == nil {
		t.Fatal("overlong tag accepted")
	}
	if _, err := ParseSymbolicPath([]byte{0x20, 0x01}); err == nil {
		t.Fatal("non-symbolic path accepted")
	}
}

func TestLogicalPath(t *testing.T) {
	p, err := LogicalPath(0x02, 0x01)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, []byte{0x20, 0x02, 0x24, 0x01}) {
		t.Fatalf("path = % x", p)
	}
	p, _ = LogicalPath(0x0102, 0x0304)
	if !bytes.Equal(p, []byte{0x21, 0x02, 0x01, 0x25, 0x04, 0x03}) {
		t.Fatalf("16-bit path = % x", p)
	}
	if _, err := LogicalPath(-1, 1); err == nil {
		t.Fatal("negative class accepted")
	}
	if _, err := LogicalPath(0x10000, 1); err == nil {
		t.Fatal("oversized class accepted")
	}
}

func TestReadTagRequest(t *testing.T) {
	b, err := BuildReadTag("A", 2)
	if err != nil {
		t.Fatal(err)
	}
	req, err := ParseRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if req.Service != ServiceReadTag {
		t.Fatalf("service = %02X", req.Service)
	}
	if !bytes.Equal(req.Data, []byte{0x02, 0x00}) {
		t.Fatalf("data = % x", req.Data)
	}
	name, _ := ParseSymbolicPath(req.Path)
	if name != "A" {
		t.Fatalf("tag = %q", name)
	}
	if _, err := BuildReadTag("A", 0); err == nil {
		t.Fatal("zero element count accepted")
	}
}

func TestParseReadTagReply(t *testing.T) {
	// 类型 DINT(0x00C4) + 数据 0x000004D2（线上小端）→ 1234。
	// 应答里没有元素数字段：类型之后直接就是数据。
	reply := Reply{Service: ServiceReadTag | ServiceReplyMask, Data: []byte{0xC4, 0x00, 0xD2, 0x04, 0x00, 0x00}}
	typ, data, err := ParseReadTagReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeDINT || binary.LittleEndian.Uint32(data) != 1234 {
		t.Fatalf("type/data = %02X/% x", typ, data)
	}
	// STRUCT（0x02A0）在数据前多一个 2 字节结构句柄。
	st := Reply{Data: []byte{0xA0, 0x02, 0x0B, 0x00, 0x01, 0x02}}
	typ, data, err = ParseReadTagReply(st)
	if err != nil || typ != TypeStruct || !bytes.Equal(data, []byte{1, 2}) {
		t.Fatalf("struct = %02X/% x err=%v", typ, data, err)
	}
	bad := Reply{Status: 0x05}
	if _, _, err := ParseReadTagReply(bad); err == nil {
		t.Fatal("error reply accepted")
	}
	short := Reply{}
	if _, _, err := ParseReadTagReply(short); err == nil {
		t.Fatal("short reply accepted")
	}
}

func TestWriteTagRequestAndReply(t *testing.T) {
	b, err := BuildWriteTag("A", TypeDINT, 1, []byte{0x2C, 0x01, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	req, err := ParseRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if req.Service != ServiceWriteTag {
		t.Fatalf("service = %02X", req.Service)
	}
	if byte(binary.LittleEndian.Uint16(req.Data[0:])) != TypeDINT || binary.LittleEndian.Uint16(req.Data[2:]) != 1 {
		t.Fatalf("header = % x", req.Data[:4])
	}
	if !bytes.Equal(req.Data[4:], []byte{0x2C, 0x01, 0x00, 0x00}) {
		t.Fatalf("data = % x", req.Data[4:])
	}

	// 写应答没有数据区：只有状态，不能因为「没有类型/元素数」判成截断失败。
	if err := ParseWriteTagReply(Reply{Data: nil}); err != nil {
		t.Fatalf("empty write reply: %v", err)
	}
	if err := ParseWriteTagReply(Reply{Status: 0x0E}); err == nil {
		t.Fatal("error reply accepted")
	}
	if _, err := BuildWriteTag("A", TypeDINT, 0, nil); err == nil {
		t.Fatal("zero count accepted")
	}
}

func TestMultipleServicePacketRoundTrip(t *testing.T) {
	a, _ := BuildReadTag("A", 1)
	b, _ := BuildReadTag("B", 1)
	pkt, err := BuildMultiple([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	req, err := ParseRequest(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if req.Service != ServiceMultiple {
		t.Fatalf("service = %02X", req.Service)
	}
	name, _ := ParseSymbolicPath(nil)
	_ = name
	if !bytes.Equal(req.Path, []byte{0x20, 0x02, 0x24, 0x01}) {
		t.Fatalf("path = % x", req.Path)
	}
	if binary.LittleEndian.Uint16(req.Data) != 2 {
		t.Fatalf("count = %d", binary.LittleEndian.Uint16(req.Data))
	}
	// 偏移相对数据区起点（条数字段在 0），第一条 = 2+2n = 6。
	// 与 1756-PM020 示例/pycomm3 同一约定。
	off0 := int(binary.LittleEndian.Uint16(req.Data[2:]))
	off1 := int(binary.LittleEndian.Uint16(req.Data[4:]))
	if off0 != 6 || off1 != 6+len(a) {
		t.Fatalf("offsets = %d/%d", off0, off1)
	}
	if !bytes.Equal(req.Data[off0:off0+len(a)], a) || !bytes.Equal(req.Data[off1:], b) {
		t.Fatalf("embedded mismatch")
	}

	// 应答：两条嵌入 reply。
	r0, _ := BuildReply(ServiceReadTag, 0, nil, []byte{0xC4, 0, 1, 0, 1, 0, 0, 0})
	r1, _ := BuildReply(ServiceReadTag, 0x05, nil, nil)
	body := make([]byte, 6)
	binary.LittleEndian.PutUint16(body, 2)
	binary.LittleEndian.PutUint16(body[2:], 6)
	binary.LittleEndian.PutUint16(body[4:], uint16(6+len(r0)))
	body = append(body, r0...)
	body = append(body, r1...)
	replies, err := ParseMultipleReply(Reply{Data: body})
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 2 || !bytes.Equal(replies[0], r0) || !bytes.Equal(replies[1], r1) {
		t.Fatalf("replies = %d", len(replies))
	}
}

func TestParseMultipleReplyRejects(t *testing.T) {
	if _, err := BuildMultiple(nil); err == nil {
		t.Fatal("empty multiple accepted")
	}
	if _, err := ParseMultipleReply(Reply{Status: 0x08}); err == nil {
		t.Fatal("error reply accepted")
	}
	if _, err := ParseMultipleReply(Reply{Data: []byte{0}}); err == nil {
		t.Fatal("short reply accepted")
	}
	// 条数 2，但偏移表不完整。
	if _, err := ParseMultipleReply(Reply{Data: []byte{2, 0, 0, 0}}); err == nil {
		t.Fatal("truncated table accepted")
	}
	// 偏移越界。
	body := []byte{1, 0, 0xFF, 0x00}
	if _, err := ParseMultipleReply(Reply{Data: body}); err == nil {
		t.Fatal("offset overrun accepted")
	}
}

func TestTypeTables(t *testing.T) {
	for typ, size := range map[byte]int{
		TypeBOOL: 1, TypeSINT: 1, TypeUSINT: 1, TypeINT: 2, TypeUINT: 2,
		TypeDINT: 4, TypeUDINT: 4, TypeREAL: 4, TypeLINT: 8, TypeLREAL: 8,
	} {
		if got := TypeSize(typ); got != size {
			t.Fatalf("type %02X size = %d, want %d", typ, got, size)
		}
	}
	if TypeSize(TypeSTRING) != -1 || TypeSize(TypeStruct) != -1 {
		t.Fatal("variable type reported fixed size")
	}
	for _, name := range []string{"dint", "INT", "real", "string", "bool", "bytes"} {
		if _, ok := ParseTypeName(name); !ok {
			t.Fatalf("type name %q rejected", name)
		}
	}
	if _, ok := ParseTypeName("nope"); ok {
		t.Fatal("unknown type accepted")
	}
	if TypeText(TypeDINT) != "DINT" || !strings.HasPrefix(TypeText(0xFE), "0x") {
		t.Fatal("type text mismatch")
	}
}

func TestStringEncodeDecode(t *testing.T) {
	b, err := EncodeString("PUMP-01", 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 20 {
		t.Fatalf("len = %d", len(b))
	}
	if binary.LittleEndian.Uint32(b) != 7 {
		t.Fatalf("length field = %d", binary.LittleEndian.Uint32(b))
	}
	got, err := DecodeString(b)
	if err != nil || got != "PUMP-01" {
		t.Fatalf("decoded = %q err=%v", got, err)
	}
	// 超长按容量拒绝，而不是静默截断：截断会让调用方以为整个值都写下去了。
	if _, err := EncodeString("ABCDEFGH", 4); err == nil {
		t.Fatal("overlong string accepted (should be rejected)")
	}
	// 长度字段大于实际数据必须报错而不是截断：截断值会被当好值上报，
	// 「看起来成功、其实丢了一半」。尾部填充（n < 容量）是常态，不受影响。
	b = append([]byte{0x09, 0, 0, 0}, []byte("AB")...)
	if _, err := DecodeString(b); err == nil {
		t.Fatal("bad length must be rejected")
	}
	if _, err := EncodeString("x", 0); err == nil {
		t.Fatal("zero capacity accepted")
	}
	if _, err := DecodeString([]byte{1, 0}); err == nil {
		t.Fatal("short string accepted")
	}
}

func TestAllStatusTexts(t *testing.T) {
	// 每个状态码都要有一句人话：现场排障时日志里只剩一个 0x14 是没用的。
	for code := 0; code <= 0x27; code++ {
		if StatusText(byte(code)) == "unknown CIP status" {
			t.Fatalf("status %02X has no text", code)
		}
	}
	for _, code := range []uint32{0x00, 0x02, 0x03, 0x65} {
		if EncapsulationStatusText(code) == "unknown encapsulation status" {
			t.Fatalf("encapsulation %02X has no text", code)
		}
	}
}

func TestAllTypeTexts(t *testing.T) {
	for _, typ := range []byte{
		TypeBOOL, TypeSINT, TypeINT, TypeDINT, TypeLINT, TypeUSINT, TypeUINT,
		TypeUDINT, TypeULINT, TypeREAL, TypeLREAL, TypeSTRING, TypeBYTE,
		TypeWORD, TypeDWORD, TypeLWORD,
	} {
		if TypeText(typ) == "" || strings.HasPrefix(TypeText(typ), "0x") {
			t.Fatalf("type %02X has no name", typ)
		}
	}
	// 变长/复合类型不报固定宽度。
	for _, typ := range []byte{TypeSTRING, TypeStruct, TypeArray} {
		if TypeSize(typ) != -1 {
			t.Fatalf("type %02X reported fixed size", typ)
		}
	}
	if _, ok := ParseTypeName("  DINT  "); !ok {
		t.Fatal("padded type name rejected")
	}
	for _, name := range []string{"boolean", "int8", "int64", "uint8", "uint64", "lreal", "bytes"} {
		if _, ok := ParseTypeName(name); !ok {
			t.Fatalf("alias %q rejected", name)
		}
	}
}

func TestBuildCPFLimits(t *testing.T) {
	items := make([]Item, 65)
	for i := range items {
		items[i] = Item{Type: ItemNullAddress}
	}
	if _, err := BuildCPF(items); err == nil {
		t.Fatal("too many items accepted")
	}
	big := []Item{{Type: ItemUnconnectedData, Data: make([]byte, MaxPayload+1)}}
	if _, err := BuildCPF(big); err == nil {
		t.Fatal("oversized item accepted")
	}
	// 条数超过上限的报文在解析侧也要拒绝，避免按声称的条数分配内存。
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint16(raw[6:], 100)
	if _, err := ParseCPF(raw); err == nil {
		t.Fatal("oversized item count accepted")
	}
	// 条目头被截断。
	raw = make([]byte, 8)
	binary.LittleEndian.PutUint16(raw[6:], 1)
	raw = append(raw, 0xB2)
	if _, err := ParseCPF(raw); err == nil {
		t.Fatal("truncated item header accepted")
	}
}

func TestParseSendRRDataMultipleDataItems(t *testing.T) {
	// 两个数据条目说明报文不是我们理解的样子，必须拒绝而不是取第一个。
	payload, err := BuildCPF([]Item{
		{Type: ItemUnconnectedData, Data: []byte{1, 2}},
		{Type: ItemConnectedData, Data: []byte{3, 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSendRRData(payload); err == nil {
		t.Fatal("multiple data items accepted")
	}
}

func TestStatusErrorMessage(t *testing.T) {
	e := &StatusError{Service: ServiceReadTag, Code: 0x05, Extra: []byte{0xAA, 0xBB}}
	msg := e.Error()
	if !strings.Contains(msg, "0x05") || !strings.Contains(msg, "extra=") {
		t.Fatalf("message = %q", msg)
	}
	e = &StatusError{Service: ServiceWriteTag, Code: 0x0E}
	if strings.Contains(e.Error(), "extra=") {
		t.Fatalf("empty extra printed: %q", e.Error())
	}
}

func TestSymbolicPathTruncated(t *testing.T) {
	if _, err := ParseSymbolicPath([]byte{0x91, 0x05, 'A'}); err == nil {
		t.Fatal("truncated symbolic segment accepted")
	}
}

func TestBuildWriteTagRejectsBadTag(t *testing.T) {
	if _, err := BuildWriteTag("", TypeDINT, 1, make([]byte, 4)); err == nil {
		t.Fatal("empty tag accepted")
	}
}

func TestParseWriteTagReplyShort(t *testing.T) {
	// 写应答允许为空（真机就是这样）；状态非 0 仍然要报错。
	if err := ParseWriteTagReply(Reply{}); err != nil {
		t.Fatalf("empty write reply rejected: %v", err)
	}
	if err := ParseWriteTagReply(Reply{Status: 0x13}); err == nil {
		t.Fatal("error reply accepted")
	}
}

func TestParseWriteTagFragReply(t *testing.T) {
	// 末片：0x00 表示写完，more=false。
	if more, err := ParseWriteTagFragReply(Reply{Status: 0}); err != nil || more {
		t.Fatalf("final fragment: more=%v err=%v", more, err)
	}
	// 中间片：0x06 partial transfer 是「这片收了、继续发」，不是错误。
	if more, err := ParseWriteTagFragReply(Reply{Status: 0x06}); err != nil || !more {
		t.Fatalf("intermediate fragment: more=%v err=%v", more, err)
	}
	// 真正的错误码（非 0x06/0x00）必须报错，不能被当成「继续」吞掉。
	if _, err := ParseWriteTagFragReply(Reply{Status: 0x13}); err == nil {
		t.Fatal("mid-stream error status accepted as success")
	}
	if _, err := ParseWriteTagFragReply(Reply{Status: 0x01}); err == nil {
		t.Fatal("connection failure status accepted as success")
	}
}

func TestBuildMultipleLimits(t *testing.T) {
	req := make([][]byte, 33)
	for i := range req {
		req[i] = []byte{0x4C}
	}
	if _, err := BuildMultiple(req); err == nil {
		t.Fatal("too many embedded requests accepted")
	}
}

func TestParseMultipleReplyNonMonotonic(t *testing.T) {
	// 偏移表倒退：第二条指向第一条之前，必须拒绝。
	body := []byte{2, 0, 4, 0, 2, 0, 0, 0, 0, 0, 0, 0}
	if _, err := ParseMultipleReply(Reply{Data: body}); err == nil {
		t.Fatal("non-monotonic offsets accepted")
	}
	// 条数超过上限。
	body = make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 40)
	if _, err := ParseMultipleReply(Reply{Data: body}); err == nil {
		t.Fatal("oversized reply count accepted")
	}
}
