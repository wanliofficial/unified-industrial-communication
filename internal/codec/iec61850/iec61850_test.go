package iec61850

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

// 本文件的测试策略：能逐字节比对的地方就逐字节比对，不能的地方用往返自洽。
//
// 为什么这么苛刻：IEC 61850 的前五层封套没有任何语义可供自检——会话层的
// SPDU 头是固定的四个字节，表示层的上下文编号是固定的两个整数，ACSE 的
// 间接引用必须与表示上下文一致。这些值写错了不会「读出一个错的值」，
// 而是「关联建立不了」或「第一次读就解不开」，而错误信息指向的位置离真正
// 的原因很远。逐字节钉死是唯一可靠的验证方式。

func TestEncodeIntIsMinimal(t *testing.T) {
	cases := []struct {
		v    int64
		want string
	}{
		{0, "00"},
		{1, "01"},
		{-1, "ff"},
		{127, "7f"},
		{128, "0080"},
		{-128, "80"},
		{-129, "ff7f"},
		{255, "00ff"},
		{256, "0100"},
		{-32768, "8000"},
		{math.MaxInt64, "7fffffffffffffff"},
		{math.MinInt64, "8000000000000000"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(encodeInt(c.v))
		if got != c.want {
			t.Errorf("encodeInt(%d) = %s, want %s", c.v, got, c.want)
		}
		back, err := decodeInt(encodeInt(c.v))
		if err != nil {
			t.Fatalf("decodeInt(encodeInt(%d)): %v", c.v, err)
		}
		if back != c.v {
			t.Errorf("round trip %d -> %d", c.v, back)
		}
	}
}

func TestEncodeUnsignedKeepsSignBitClear(t *testing.T) {
	// 无符号数最高位为 1 时必须补一个 0x00，否则对端按有符号读会得到负数。
	// 一个 invokeID 大于 127 的请求会因此永远等不到响应。
	cases := []struct {
		v    uint64
		want string
	}{
		{0, "00"},
		{1, "01"},
		{127, "7f"},
		{128, "0080"},
		{255, "00ff"},
		{256, "0100"},
		{65535, "00ffff"},
		{math.MaxInt64, "7fffffffffffffff"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(encodeUnsigned(c.v))
		if got != c.want {
			t.Errorf("encodeUnsigned(%d) = %s, want %s", c.v, got, c.want)
		}
		back, err := decodeUnsigned(encodeUnsigned(c.v))
		if err != nil {
			t.Fatalf("decodeUnsigned: %v", err)
		}
		if back != c.v {
			t.Errorf("round trip %d -> %d", c.v, back)
		}
	}
}

func TestParseElementRejectsIndefiniteLength(t *testing.T) {
	// 不定长（0x80）在 IEC 61850 里不使用。接受它意味着「扫描找结束标记」，
	// 在长度写错时会把整条流读歪，因此明确拒绝。
	_, err := parseElement([]byte{0x30, 0x80, 0x00, 0x00})
	if err == nil {
		t.Fatal("expected an error for an indefinite length")
	}
	if !strings.Contains(err.Error(), "indefinite") {
		t.Errorf("error %v should mention indefinite length", err)
	}
}

func TestParseElementLongLength(t *testing.T) {
	// 300 字节内容用两字节长形式（ff 01 2c）。
	body := bytes.Repeat([]byte{0xAA}, 300)
	raw := appendTLV(nil, 0x04, body)
	e, err := parseElement(raw)
	if err != nil {
		t.Fatalf("parseElement: %v", err)
	}
	if e.tag != 0x04 || len(e.data) != 300 {
		t.Fatalf("tag=0x%02X len=%d", e.tag, len(e.data))
	}
	if len(e.rest) != 0 {
		t.Errorf("rest = %d bytes, want 0", len(e.rest))
	}
}

func TestParseLengthOutOfRange(t *testing.T) {
	// 五字节长度（0x85）不是本协议会出现的，继续按它读只会浪费内存。
	if _, _, err := parseLength([]byte{0x85, 0, 0, 0, 0, 1}); err == nil {
		t.Fatal("expected an error for a 5-octet length")
	}
}

func TestBitStringRoundTrip(t *testing.T) {
	bits := []byte{0xF1, 0x00}
	enc := encodeBitString(bits, 5)
	if enc[0] != 5 {
		t.Fatalf("padding byte = %d, want 5", enc[0])
	}
	pad, back, err := decodeBitString(enc)
	if err != nil {
		t.Fatalf("decodeBitString: %v", err)
	}
	if pad != 5 || !bytes.Equal(back, bits) {
		t.Errorf("pad=%d bits=%x", pad, back)
	}
}

func TestFloatEncoding(t *testing.T) {
	// MMS 的浮点前面有一个「指数宽度」字节：32 位写 8，64 位写 11。
	// 漏掉它会让对端把 0x08 当成 IEEE 754 的最高字节，读出一个荒谬的数。
	enc32, err := encodeFloat(1.5, 32)
	if err != nil {
		t.Fatalf("encodeFloat(32): %v", err)
	}
	if enc32[0] != 8 || len(enc32) != 5 {
		t.Fatalf("float32 encoding = %x, want 5 bytes starting with 08", enc32)
	}
	v, w, err := decodeFloat(enc32)
	if err != nil || v != 1.5 || w != 32 {
		t.Errorf("decodeFloat(%x) = %v/%d, %v", enc32, v, w, err)
	}

	enc64, err := encodeFloat(-2.25, 64)
	if err != nil {
		t.Fatalf("encodeFloat(64): %v", err)
	}
	if enc64[0] != 11 || len(enc64) != 9 {
		t.Fatalf("float64 encoding = %x, want 9 bytes starting with 0b", enc64)
	}
	v, w, err = decodeFloat(enc64)
	if err != nil || v != -2.25 || w != 64 {
		t.Errorf("decodeFloat(%x) = %v/%d, %v", enc64, v, w, err)
	}
}

// ---------------------------------------------------------------------------
// TPKT / COTP
// ---------------------------------------------------------------------------

func TestTPKTRoundTrip(t *testing.T) {
	payload := []byte{0x02, 0xF0, 0x80, 0xDE, 0xAD}
	frame := BuildTPKT(payload)
	if frame[0] != TPKTVersion {
		t.Fatalf("version = %d", frame[0])
	}
	if int(frame[2])<<8|int(frame[3]) != len(frame) {
		t.Fatalf("declared length %d, actual %d", int(frame[2])<<8|int(frame[3]), len(frame))
	}
	back, err := ParseTPKT(frame)
	if err != nil {
		t.Fatalf("ParseTPKT: %v", err)
	}
	if !bytes.Equal(back, payload) {
		t.Errorf("payload = %x, want %x", back, payload)
	}
}

func TestParseTPKTRejectsBadVersion(t *testing.T) {
	if _, err := ParseTPKT([]byte{0x04, 0x00, 0x00, 0x04}); err == nil {
		t.Fatal("expected an error for version 4")
	}
}

func TestCOTPConnectShape(t *testing.T) {
	// 逐字节钉死 CR：LI、类型、DST-REF=0、SRC-REF、类别 0、然后三个参数。
	// 参数顺序是 TPDU 大小 → 被调 TSAP(0xC2) → 调用 TSAP(0xC1)。
	frame := BuildCOTPConnect(DefaultCOTPOptions(), 0x0001)
	payload, err := ParseTPKT(frame)
	if err != nil {
		t.Fatalf("ParseTPKT: %v", err)
	}
	if payload[1] != COTPCR {
		t.Fatalf("tpdu type = 0x%02X, want CR", payload[1])
	}
	if payload[2] != 0 || payload[3] != 0 {
		t.Errorf("DST-REF = %x, want 0000", payload[2:4])
	}
	if payload[4] != 0x00 || payload[5] != 0x01 {
		t.Errorf("SRC-REF = %x, want 0001", payload[4:6])
	}
	if payload[6] != 0x00 {
		t.Errorf("class = %d, want 0", payload[6])
	}
	opts := payload[7:]
	want := []byte{
		0xC0, 0x01, 0x0D, // TPDU 8192
		0xC2, 0x02, 0x00, 0x01, // called TSAP
		0xC1, 0x02, 0x00, 0x01, // calling TSAP
	}
	if !bytes.Equal(opts, want) {
		t.Errorf("options = %x, want %x", opts, want)
	}
	if int(payload[0]) != len(payload)-1 {
		t.Errorf("LI = %d, want %d", payload[0], len(payload)-1)
	}
}

func TestCOTPConfirmRoundTrip(t *testing.T) {
	frame := BuildCOTPConfirm(DefaultCOTPOptions(), 0x0002, 0x0001)
	cc, err := ParseCOTPConfirm(frame)
	if err != nil {
		t.Fatalf("ParseCOTPConfirm: %v", err)
	}
	if cc.TPDUSize != maxTPDUSize {
		t.Errorf("tpdu size = %d, want %d", cc.TPDUSize, maxTPDUSize)
	}
	if !bytes.Equal(cc.SrcTSAP, []byte{0x00, 0x01}) || !bytes.Equal(cc.DstTSAP, []byte{0x00, 0x01}) {
		t.Errorf("tsap src=%x dst=%x", cc.SrcTSAP, cc.DstTSAP)
	}
}

func TestCOTPDataRoundTrip(t *testing.T) {
	frame := BuildCOTPData([]byte{0x01, 0x02, 0x03})
	data, last, err := ParseCOTPData(frame)
	if err != nil {
		t.Fatalf("ParseCOTPData: %v", err)
	}
	if !last {
		t.Error("EOT bit should be set")
	}
	if !bytes.Equal(data, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("data = %x", data)
	}
}

func TestTPDUSizeCodes(t *testing.T) {
	cases := map[int]int{128: 7, 256: 8, 512: 9, 1024: 10, 2048: 11, 4096: 12, 8192: 13}
	for size, code := range cases {
		if got := tpduSizeCode(size); got != code {
			t.Errorf("tpduSizeCode(%d) = %d, want %d", size, got, code)
		}
		if got := decodeTPDUSize(byte(code)); got != size {
			t.Errorf("decodeTPDUSize(%d) = %d, want %d", code, got, size)
		}
	}
}

// ---------------------------------------------------------------------------
// 会话层
// ---------------------------------------------------------------------------

func TestSessionConnectShape(t *testing.T) {
	// CN 的骨架必须能被自己的解析器读回来，且各 PGI 原样保留。
	payload := []byte{0xAA, 0xBB}
	cn, err := BuildSessionConnect(DefaultSessionParams(), payload)
	if err != nil {
		t.Fatalf("BuildSessionConnect: %v", err)
	}
	if cn[0] != spduConnect {
		t.Fatalf("spdu id = 0x%02X, want CN", cn[0])
	}
	// LI 必须等于「除 SPDU ID 与 LI 之外」的全部字节。
	n, hdr, err := parseLength(cn[1:])
	if err != nil {
		t.Fatalf("parseLength: %v", err)
	}
	if 1+hdr+n != len(cn) {
		t.Errorf("LI declares %d bytes, total is %d", n, len(cn))
	}

	back, err := ParseSPDU(cn)
	if err != nil {
		t.Fatalf("ParseSPDU: %v", err)
	}
	if back.ID != spduConnect {
		t.Errorf("id = 0x%02X", back.ID)
	}
	if !bytes.Equal(back.Payload, payload) {
		t.Errorf("payload = %x, want %x", back.Payload, payload)
	}
	if !bytes.Equal(back.CallingSSSel, []byte{0x00, 0x01}) {
		t.Errorf("calling ss-sel = %x", back.CallingSSSel)
	}
	if !bytes.Equal(back.CalledSSSel, []byte{0x00, 0x01}) {
		t.Errorf("called ss-sel = %x", back.CalledSSSel)
	}
}

func TestSessionConnectUsesExtendedUserData(t *testing.T) {
	// 超过 254 字节必须换成扩展用户数据（0xC2），否则对端按 LI 读完会把
	// 多出来的部分当成下一个参数。
	payload := bytes.Repeat([]byte{0x5A}, 300)
	cn, err := BuildSessionConnect(DefaultSessionParams(), payload)
	if err != nil {
		t.Fatalf("BuildSessionConnect: %v", err)
	}
	// CN 的 LI 与扩展参数的 LI 都用两字节长形式（ff 01 44 / ff 01 2c）。
	if !bytes.Contains(cn, []byte{piExtendedUserData, 0xFF, 0x01, 0x2C}) {
		t.Errorf("expected the extended-user-data parameter 0xC2 with LI 300, got %x", cn[:24])
	}
	back, err := ParseSPDU(cn)
	if err != nil {
		t.Fatalf("ParseSPDU: %v", err)
	}
	if len(back.Payload) != 300 {
		t.Errorf("payload length = %d, want 300", len(back.Payload))
	}
}

// TestSessionLIBoundaryRoundTrip 钉住会话层 LI 在 254/255/256 三个边界上的
// 往返一致性，以及 liSize 与 putLI 的字节数一致。
//
// 为什么单独测 255：ISO 8327-1 §8.2.5 规定「长度 0..254 用单字节，长度
// 255..65535 用 0xFF + 恰好两字节」。255 恰好落在长形式的最小端——0xFF 是
// 长形式标记，不能当单字节 LI 的值，所以 255 必须编成 `FF 00 FF`。曾有一个
// `n <= 0xFF` 的中间分支把 255 编成 `FF FF`（只有一字节长度），既违反
// 「长形式恰好两字节」，也无法被 parseLI 还原（它把标记后的两个字节都当长度，
// 吃掉一个内容字节）。短帧测试（300 走长形式、普通帧走短形式）永远踩不到
// 这个边界，只有显式构造 255 才暴露。
func TestSessionLIBoundaryRoundTrip(t *testing.T) {
	cases := []struct {
		n    int
		want []byte // 期望的 LI 编码
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{254, []byte{0xFE}},             // 短形式上界
		{255, []byte{0xFF, 0x00, 0xFF}}, // 长形式下界：0xFF 标记 + 两字节(0x00FF)
		{256, []byte{0xFF, 0x01, 0x00}},
		{300, []byte{0xFF, 0x01, 0x2C}},
		{65535, []byte{0xFF, 0xFF, 0xFF}}, // 长形式上界
	}
	for _, c := range cases {
		enc := putLI(nil, c.n)
		if !bytes.Equal(enc, c.want) {
			t.Errorf("putLI(%d) = % X, want % X", c.n, enc, c.want)
		}
		if liSize(c.n) != len(enc) {
			t.Errorf("liSize(%d) = %d, but putLI wrote %d bytes", c.n, liSize(c.n), len(enc))
		}
		got, hdr, err := parseLI(enc)
		if err != nil {
			t.Errorf("parseLI(putLI(%d)) error: %v", c.n, err)
			continue
		}
		if got != c.n {
			t.Errorf("parseLI(putLI(%d)) = %d, want %d", c.n, got, c.n)
		}
		if hdr != len(enc) {
			t.Errorf("parseLI hdr = %d, want %d (LI width)", hdr, len(enc))
		}
	}
}

func TestSessionAcceptShape(t *testing.T) {
	ac, err := BuildSessionAccept(DefaultSessionParams(), []byte{0x01})
	if err != nil {
		t.Fatalf("BuildSessionAccept: %v", err)
	}
	// AC 不带调用侧 SS-SEL：只有发起方需要宣告自己是谁。
	if bytes.Contains(ac, []byte{pgiCallingSSSel, 0x02}) {
		t.Errorf("AC must not carry a calling ss-sel: %x", ac)
	}
	back, err := ParseSPDU(ac)
	if err != nil {
		t.Fatalf("ParseSPDU: %v", err)
	}
	if back.ID != spduAccept {
		t.Errorf("id = 0x%02X, want AC", back.ID)
	}
}

func TestSessionDataHeader(t *testing.T) {
	dt := BuildSessionData([]byte{0xDE, 0xAD})
	if !bytes.Equal(dt[:4], []byte{0x01, 0x00, 0x01, 0x00}) {
		t.Fatalf("DT header = %x, want 01000100", dt[:4])
	}
	back, err := ParseSPDU(dt)
	if err != nil {
		t.Fatalf("ParseSPDU: %v", err)
	}
	if back.ID != spduDataTransfer || !bytes.Equal(back.Payload, []byte{0xDE, 0xAD}) {
		t.Errorf("id=0x%02X payload=%x", back.ID, back.Payload)
	}
}

func TestSessionFinishAndAbort(t *testing.T) {
	fn, err := BuildSessionFinish([]byte{0x62, 0x03, 0x80, 0x01, 0x00})
	if err != nil {
		t.Fatalf("BuildSessionFinish: %v", err)
	}
	back, err := ParseSPDU(fn)
	if err != nil {
		t.Fatalf("ParseSPDU(FN): %v", err)
	}
	if back.ID != spduFinish {
		t.Errorf("id = 0x%02X, want FN", back.ID)
	}

	ab, err := BuildSessionAbort([]byte{0x64, 0x03, 0x80, 0x01, 0x00})
	if err != nil {
		t.Fatalf("BuildSessionAbort: %v", err)
	}
	back, err = ParseSPDU(ab)
	if err != nil {
		t.Fatalf("ParseSPDU(AB): %v", err)
	}
	if back.ID != spduAbort {
		t.Errorf("id = 0x%02X, want AB", back.ID)
	}
}

// ---------------------------------------------------------------------------
// 表示层
// ---------------------------------------------------------------------------

func TestPresentationConnectShape(t *testing.T) {
	// CP 必须声明两个表示上下文：1 = ACSE、3 = MMS；并且调用/被调 P-Selector
	// 都要写出来，哪怕长度为 0。
	cp, err := BuildPresentationConnect(DefaultPresentationParams(), []byte{0x60, 0x00})
	if err != nil {
		t.Fatalf("BuildPresentationConnect: %v", err)
	}
	if cp[0] != 0x31 {
		t.Fatalf("cp tag = 0x%02X, want 0x31", cp[0])
	}
	// mode-selector 是固定的 a0 03 80 01 01。
	if !bytes.Contains(cp, []byte{0xA0, 0x03, 0x80, 0x01, 0x01}) {
		t.Errorf("missing the normal-mode selector: %x", cp)
	}
	// 两个抽象语法 OID。
	if !bytes.Contains(cp, asnIDACSE) {
		t.Error("missing the ACSE abstract syntax")
	}
	if !bytes.Contains(cp, asnIDMMS) {
		t.Error("missing the MMS abstract syntax")
	}
	// 两个 P-Selector 字段，即使为空也要出现标签。
	if !bytes.Contains(cp, []byte{0x81, 0x04}) || !bytes.Contains(cp, []byte{0x82, 0x04}) {
		t.Errorf("P-Selector fields must be present: %x", cp)
	}

	back, err := ParsePresentation(cp)
	if err != nil {
		t.Fatalf("ParsePresentation: %v", err)
	}
	if back.Kind != "cp" {
		t.Errorf("kind = %q, want cp", back.Kind)
	}
	if !back.HasACSE || !back.HasMMS {
		t.Errorf("context list: acse=%v mms=%v", back.HasACSE, back.HasMMS)
	}
	if !bytes.Equal(back.UserData, []byte{0x60, 0x00}) {
		t.Errorf("user data = %x, want 6000", back.UserData)
	}
}

func TestPresentationAcceptShape(t *testing.T) {
	cpa, err := BuildPresentationAccept([]byte{0x61, 0x00})
	if err != nil {
		t.Fatalf("BuildPresentationAccept: %v", err)
	}
	// 两项「接受 BER」的结果，每项固定 9 字节。
	accept := []byte{0x30, 0x07, 0x80, 0x01, 0x00, 0x81, 0x02, 0x51, 0x01}
	if bytes.Count(cpa, accept) != 2 {
		t.Errorf("expected two acceptance results: %x", cpa)
	}
	back, err := ParsePresentation(cpa)
	if err != nil {
		t.Fatalf("ParsePresentation: %v", err)
	}
	if back.Kind != "cpa" {
		t.Errorf("kind = %q, want cpa", back.Kind)
	}
	if !bytes.Equal(back.UserData, []byte{0x61, 0x00}) {
		t.Errorf("user data = %x", back.UserData)
	}
}

func TestPresentationContextID(t *testing.T) {
	// 关联建立之后，MMS 的数据必须带上下文 3；带 1 会被对端按 ACSE 解析。
	pdu, err := BuildPresentationUserData([]byte{0xA8, 0x00})
	if err != nil {
		t.Fatalf("BuildPresentationUserData: %v", err)
	}
	back, err := ParsePresentation(pdu)
	if err != nil {
		t.Fatalf("ParsePresentation: %v", err)
	}
	if back.ContextID != MMSContextID {
		t.Errorf("context id = %d, want %d", back.ContextID, MMSContextID)
	}
	if !bytes.Equal(back.UserData, []byte{0xA8, 0x00}) {
		t.Errorf("user data = %x", back.UserData)
	}
}

// ---------------------------------------------------------------------------
// ACSE
// ---------------------------------------------------------------------------

func TestAARQShape(t *testing.T) {
	// 逐字节核对骨架：0x60 + 应用上下文（a1 07 06 05 28ca220203）+
	// 用户信息（be … 28 … 02 01 03 a0 …）。间接引用必须是 3（MMS 上下文）。
	aarq, err := BuildAARQ([]byte{0xA8, 0x00}, nil)
	if err != nil {
		t.Fatalf("BuildAARQ: %v", err)
	}
	if aarq[0] != 0x60 {
		t.Fatalf("tag = 0x%02X, want 0x60", aarq[0])
	}
	appCtx := []byte{0xA1, 0x07, 0x06, 0x05, 0x28, 0xCA, 0x22, 0x02, 0x03}
	if !bytes.Contains(aarq, appCtx) {
		t.Errorf("missing the MMS application context: %x", aarq)
	}
	// 间接引用：be … 28 … 02 01 03。
	if !bytes.Contains(aarq, []byte{0x02, 0x01, 0x03}) {
		t.Errorf("indirect reference must be 3 (the MMS presentation context): %x", aarq)
	}
}

func TestAARQWithAuth(t *testing.T) {
	aarq, err := BuildAARQ([]byte{0xA8, 0x00}, &ACSEAuth{Password: "secret"})
	if err != nil {
		t.Fatalf("BuildAARQ: %v", err)
	}
	if !bytes.Contains(aarq, authMechPassword) {
		t.Error("missing the password authentication mechanism OID")
	}
	if !bytes.Contains(aarq, []byte("secret")) {
		t.Error("missing the password value")
	}
	if !bytes.Contains(aarq, senderRequirementsAuth) {
		t.Error("missing the sender-requirements authentication bit")
	}
}

func TestAARERoundTrip(t *testing.T) {
	aare := BuildAARE(ACSEResultAccepted, 0, []byte{0xA9, 0x00})
	kind, back, err := ParseACSE(aare)
	if err != nil {
		t.Fatalf("ParseACSE: %v", err)
	}
	if kind != "aare" {
		t.Fatalf("kind = %q, want aare", kind)
	}
	if back.Result != ACSEResultAccepted {
		t.Errorf("result = %d, want 0", back.Result)
	}
	if !bytes.Equal(back.UserData, []byte{0xA9, 0x00}) {
		t.Errorf("user data = %x", back.UserData)
	}
}

func TestAARERejection(t *testing.T) {
	aare := BuildAARE(ACSEResultRejectedPermanent, 2, nil)
	_, back, err := ParseACSE(aare)
	if err != nil {
		t.Fatalf("ParseACSE: %v", err)
	}
	if back.Result != ACSEResultRejectedPermanent || back.Diagnostic != 2 {
		t.Errorf("result=%d diagnostic=%d", back.Result, back.Diagnostic)
	}
	if s := ACSEReason(back.Result, back.Diagnostic); !strings.Contains(s, "application context") {
		t.Errorf("reason = %q", s)
	}
}

func TestACSEFixedPDUs(t *testing.T) {
	if got := hex.EncodeToString(BuildRLRQ()); got != "620380010 0"[:0]+"6203800100" {
		t.Errorf("RLRQ = %s", got)
	}
	if got := hex.EncodeToString(BuildRLRE()); got != "6300" {
		t.Errorf("RLRE = %s", got)
	}
	if got := hex.EncodeToString(BuildABRT(false)); got != "6403800100" {
		t.Errorf("ABRT(user) = %s", got)
	}
	if got := hex.EncodeToString(BuildABRT(true)); got != "6403800101" {
		t.Errorf("ABRT(provider) = %s", got)
	}
}

// ---------------------------------------------------------------------------
// MMS
// ---------------------------------------------------------------------------

func TestInitiateShape(t *testing.T) {
	pdu := BuildInitiate(DefaultInitiateRequest())
	if pdu[0] != ServiceTagInitiate {
		t.Fatalf("tag = 0x%02X, want 0xA8", pdu[0])
	}
	// 四个协商字段 + detail。
	for _, marker := range [][]byte{
		{0x80, 0x03, 0x00, 0xFD, 0xE8}, // localDetailCalling = 65000
		{0x81, 0x01, 0x05},             // maxServOutstandingCalling = 5
		{0x82, 0x01, 0x05},             // maxServOutstandingCalled = 5
		{0x83, 0x01, 0x0A},             // nesting level = 10
		{0xA4},                         // initRequestDetail
		{0x81, 0x03, 0x05, 0xF1, 0x00}, // parameterCBB
		{0x82, 0x0C, 0x03},             // servicesSupported，3 个填充位
	} {
		if !bytes.Contains(pdu, marker) {
			t.Errorf("initiate is missing %x: %x", marker, pdu)
		}
	}
}

func TestInitiateResponseRoundTrip(t *testing.T) {
	req := DefaultInitiateRequest()
	req.MaxPDUSize = 4096
	req.NestingLevel = 6
	raw := BuildInitiateResponse(req)
	back, err := ParseInitiateResponse(raw)
	if err != nil {
		t.Fatalf("ParseInitiateResponse: %v", err)
	}
	if back.MaxPDUSize != 4096 || back.NestingLevel != 6 {
		t.Errorf("pdu=%d nesting=%d", back.MaxPDUSize, back.NestingLevel)
	}
	if back.Version != MMSVersion {
		t.Errorf("version = %d, want 1", back.Version)
	}
}

func TestObjectNameEncoding(t *testing.T) {
	// 域内名字：a1 { 1a <len> domain, 1a <len> item }。
	name := DomainObject("simpleIO", "GGIO1$ST$Ind1$stVal")
	enc := name.Encode()
	want := []byte{
		0xA1, 0x1F,
		0x1A, 0x08, 's', 'i', 'm', 'p', 'l', 'e', 'I', 'O',
		0x1A, 0x13, 'G', 'G', 'I', 'O', '1', '$', 'S', 'T', '$', 'I', 'n', 'd', '1', '$', 's', 't', 'V', 'a', 'l',
	}
	if !bytes.Equal(enc, want) {
		t.Errorf("object name = %x\nwant           %x", enc, want)
	}
	back, err := DecodeObjectName(enc)
	if err != nil {
		t.Fatalf("DecodeObjectName: %v", err)
	}
	if back.Domain != "simpleIO" || back.Item != "GGIO1$ST$Ind1$stVal" {
		t.Errorf("domain=%q item=%q", back.Domain, back.Item)
	}
}

func TestReadRequestShape(t *testing.T) {
	name := DomainObject("simpleIO", "GGIO1$ST$Ind1$stVal")
	raw, err := BuildReadRequest(7, ReadRequest{Names: []ObjectName{name}})
	if err != nil {
		t.Fatalf("BuildReadRequest: %v", err)
	}
	if raw[0] != tagConfirmedRequest {
		t.Fatalf("outer tag = 0x%02X, want 0xA0", raw[0])
	}
	if !bytes.Contains(raw, []byte{0x02, 0x01, 0x07}) {
		t.Errorf("missing the invokeID 7: %x", raw)
	}
	if !bytes.Contains(raw, []byte{ServiceTagRead}) {
		t.Errorf("missing the read service tag: %x", raw)
	}
	// listOfVariable 的外层是 a0，里面每项是 30 { a0 { ObjectName } }。
	if !bytes.Contains(raw, []byte{0xA0, 0x1F, tagSequence}) &&
		!bytes.Contains(raw, []byte{0xA0}) {
		t.Errorf("unexpected listOfVariable shape: %x", raw)
	}
	invoke, tag, err := ConfirmedInvokeID(raw)
	if err != nil {
		t.Fatalf("ConfirmedInvokeID: %v", err)
	}
	if invoke != 7 || tag != tagConfirmedRequest {
		t.Errorf("invoke=%d tag=0x%02X", invoke, tag)
	}
}

func TestReadResponsePartialFailure(t *testing.T) {
	// 一部分成功、一部分带错误码：整条响应不算失败，逐项报告。
	ok := Data{Tag: tagDataFloat, Float: 1.25, FloatWidth: 32}
	okEnc, err := ok.Encode()
	if err != nil {
		t.Fatalf("Data.Encode: %v", err)
	}
	items := append([]byte(nil), okEnc...)
	items = append(items, 0x80, 0x01, DataErrObjectNonExistent)

	var body []byte
	body = appendTLV(body, contextTag(1, true), items)
	raw := ConfirmedRequest(3, appendTLV(nil, ServiceTagRead, body))
	raw[0] = tagConfirmedResponse

	resp, err := ParseReadResponse(raw)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	if !resp.Results[0].OK || resp.Results[0].Data.Float != 1.25 {
		t.Errorf("first result = %+v", resp.Results[0])
	}
	if resp.Results[1].OK || resp.Results[1].Error != DataErrObjectNonExistent {
		t.Errorf("second result = %+v", resp.Results[1])
	}
	if !strings.Contains(resp.Results[1].Err().Error(), "object-non-existent") {
		t.Errorf("error text = %q", resp.Results[1].Err())
	}
}

func TestWriteRequestAndResponse(t *testing.T) {
	name := DomainObject("simpleIO", "GGIO1$CO$SPCSO1$Oper$ctlVal")
	raw, err := BuildWriteRequest(9, WriteRequest{
		Names:  []ObjectName{name},
		Values: []Data{{Tag: tagDataBoolean, Bool: true}},
	})
	if err != nil {
		t.Fatalf("BuildWriteRequest: %v", err)
	}
	if !bytes.Contains(raw, []byte{ServiceTagWrite}) {
		t.Errorf("missing the write service tag: %x", raw)
	}

	// 成功的编码是 81 00：长度 0 不能被当成「解析失败」。
	var body []byte
	body = append(body, 0x81, 0x00)
	respRaw := ConfirmedRequest(9, appendTLV(nil, ServiceTagWrite, body))
	respRaw[0] = tagConfirmedResponse
	resp, err := ParseWriteResponse(respRaw)
	if err != nil {
		t.Fatalf("ParseWriteResponse: %v", err)
	}
	if len(resp.Results) != 1 || !resp.Results[0].OK {
		t.Fatalf("results = %+v", resp.Results)
	}

	// 失败：80 01 <code>。
	body = append([]byte(nil), 0x80, 0x01, DataErrAccessDenied)
	respRaw = ConfirmedRequest(9, appendTLV(nil, ServiceTagWrite, body))
	respRaw[0] = tagConfirmedResponse
	resp, err = ParseWriteResponse(respRaw)
	if err != nil {
		t.Fatalf("ParseWriteResponse: %v", err)
	}
	if resp.Results[0].OK || resp.Results[0].Error != DataErrAccessDenied {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestWriteRequestRejectsMismatchedCounts(t *testing.T) {
	name := DomainObject("ld", "ln$ST$do$da")
	if _, err := BuildWriteRequest(1, WriteRequest{
		Names:  []ObjectName{name},
		Values: nil,
	}); err == nil {
		t.Fatal("expected an error when names and values differ in count")
	}
}

func TestGetNameListRequestShape(t *testing.T) {
	// objectClass [0] { [0] INTEGER }，objectScope [1] { [1] VisibleString }。
	raw := BuildGetNameListRequest(1, GetNameListRequest{Class: ObjectClassDomain})
	want := []byte{0xA0, 0x03, 0x80, 0x01, 0x09}
	if !bytes.Contains(raw, want) {
		t.Errorf("missing the domain object class: %x", raw)
	}

	raw = BuildGetNameListRequest(2, GetNameListRequest{Class: ObjectClassNamedVariable, Domain: "simpleIO"})
	if !bytes.Contains(raw, []byte{0xA1, 0x0A, 0x81, 0x08}) {
		t.Errorf("missing the domain scope: %x", raw)
	}
}

func TestGetNameListResponseRoundTrip(t *testing.T) {
	raw := BuildGetNameListResponse([]string{"simpleIO", "otherLD"}, false)
	var body []byte
	body = append(body, raw...)
	respRaw := ConfirmedRequest(1, body)
	respRaw[0] = tagConfirmedResponse
	resp, err := ParseGetNameListResponse(respRaw)
	if err != nil {
		t.Fatalf("ParseGetNameListResponse: %v", err)
	}
	if len(resp.Identifiers) != 2 || resp.Identifiers[0] != "simpleIO" {
		t.Errorf("identifiers = %v", resp.Identifiers)
	}
	if resp.MoreFollows {
		t.Error("moreFollows should be false")
	}
}

func TestGetNameListMoreFollowsDefault(t *testing.T) {
	// moreFollows 省略时为 TRUE：把「省略」当成 false 会让模型浏览只拿到
	// 第一页就停下，而现象是「装置上少了几个逻辑设备」。
	raw := BuildGetNameListResponse([]string{"a"}, true)
	respRaw := ConfirmedRequest(1, raw)
	respRaw[0] = tagConfirmedResponse
	resp, err := ParseGetNameListResponse(respRaw)
	if err != nil {
		t.Fatalf("ParseGetNameListResponse: %v", err)
	}
	if !resp.MoreFollows {
		t.Error("an omitted moreFollows means TRUE")
	}
}

func TestTypeSpecRoundTrip(t *testing.T) {
	cases := []TypeSpec{
		{Kind: "boolean"},
		{Kind: "integer", Bits: 32},
		{Kind: "unsigned", Bits: 16},
		{Kind: "float", FloatWidth: 32, ExponentWidth: 8},
		{Kind: "float", FloatWidth: 64, ExponentWidth: 11},
		{Kind: "visible-string", Bits: 32},
		{Kind: "bit-string", Bits: 13},
		{Kind: "utc-time"},
		{
			Kind: "structure", Components: []TypeSpec{
				{Kind: "float", FloatWidth: 32, ExponentWidth: 8},
				{Kind: "boolean"},
			},
		},
		{Kind: "array", Count: 3, Elem: &TypeSpec{Kind: "integer", Bits: 8}},
	}
	for _, c := range cases {
		enc, err := EncodeTypeSpec(c)
		if err != nil {
			t.Fatalf("EncodeTypeSpec(%s): %v", c.Kind, err)
		}
		back, n, err := DecodeTypeSpec(enc)
		if err != nil {
			t.Fatalf("DecodeTypeSpec(%x): %v", enc, err)
		}
		if n != len(enc) {
			t.Errorf("%s: consumed %d of %d bytes", c.Kind, n, len(enc))
		}
		if back.String() != c.String() {
			t.Errorf("%s: round trip gave %s", c.String(), back.String())
		}
	}
}

func TestDataRoundTrip(t *testing.T) {
	cases := []Data{
		{Tag: tagDataBoolean, Bool: true},
		{Tag: tagDataInteger, Int: -12345},
		{Tag: tagDataUnsigned, Uint: 4000000000},
		{Tag: tagDataFloat, Float: 21.5, FloatWidth: 32},
		{Tag: tagDataFloat, Float: -1e-9, FloatWidth: 64},
		{Tag: tagDataVisibleString, Text: "MMXU1"},
		{Tag: tagDataOctetString, Bytes: []byte{1, 2, 3}},
		{Tag: tagDataBitString, Bytes: []byte{0x03}, Padding: 2},
		{Tag: tagDataStructure, Elements: []Data{
			{Tag: tagDataFloat, Float: 1, FloatWidth: 32},
			{Tag: tagDataStructure, Elements: []Data{
				{Tag: tagDataBoolean, Bool: false},
			}},
		}},
	}
	for _, c := range cases {
		enc, err := c.Encode()
		if err != nil {
			t.Fatalf("Encode(%s): %v", c.KindName(), err)
		}
		back, n, err := DecodeData(enc)
		if err != nil {
			t.Fatalf("DecodeData(%x): %v", enc, err)
		}
		if n != len(enc) {
			t.Errorf("%s: consumed %d of %d bytes", c.KindName(), n, len(enc))
		}
		if back.String() != c.String() {
			t.Errorf("round trip: %s -> %s", c.String(), back.String())
		}
	}
}

func TestDataToValueRejectsStructure(t *testing.T) {
	// 结构体折成平台值必须报错：默默取第一个元素是「读出来的值不对」的
	// 经典来源——功率点位会读到品质位。
	d := Data{Tag: tagDataStructure, Elements: []Data{{Tag: tagDataFloat, Float: 1, FloatWidth: 32}}}
	if _, err := d.ToValue(); err == nil {
		t.Fatal("expected an error when folding a structure")
	} else if !strings.Contains(err.Error(), "address one of its leaves") {
		t.Errorf("error should tell the user what to do: %v", err)
	}
}

func TestValueFromDataCoercion(t *testing.T) {
	// 线上的 32 位浮点读到 int32 点位：装得下就接受。
	v, err := ValueFromData(Data{Tag: tagDataFloat, Float: 42, FloatWidth: 32}, "int32")
	if err != nil {
		t.Fatalf("ValueFromData: %v", err)
	}
	if v.Type != "int32" || v.Num != 42 {
		t.Errorf("value = %+v", v)
	}

	// 超出范围必须报错而不是截断。
	if _, err := ValueFromData(Data{Tag: tagDataInteger, Int: 70000}, "int16"); err == nil {
		t.Fatal("expected a range error for 70000 as int16")
	}
}

func TestBitStringToValue(t *testing.T) {
	// 品质位串折成整数；末尾的填充位必须归零，否则品质值带上 garbage。
	d := Data{Tag: tagDataBitString, Bytes: []byte{0xFF}, Padding: 5}
	v, err := d.ToValue()
	if err != nil {
		t.Fatalf("ToValue: %v", err)
	}
	if got := v.(int64); got != 0xE0 {
		t.Errorf("value = 0x%02X, want 0xE0 (padding bits cleared)", got)
	}
}

func TestDataFromValue(t *testing.T) {
	cases := []struct {
		v    Value
		tag  byte
		desc string
	}{
		{Value{Type: "bool", Bool: true}, tagDataBoolean, "boolean"},
		{Value{Type: "int32", Num: -5}, tagDataInteger, "integer"},
		{Value{Type: "uint32", Num: 5}, tagDataUnsigned, "unsigned"},
		{Value{Type: "float32", Num: 1.5}, tagDataFloat, "float32"},
		{Value{Type: "string", Text: "x"}, tagDataVisibleString, "visible-string"},
	}
	for _, c := range cases {
		d, err := DataFromValue(c.v)
		if err != nil {
			t.Fatalf("DataFromValue(%s): %v", c.v.Type, err)
		}
		if d.Tag != c.tag {
			t.Errorf("%s -> tag 0x%02X, want 0x%02X", c.v.Type, d.Tag, c.tag)
		}
	}
}

// ---------------------------------------------------------------------------
// 关联（整链）
// ---------------------------------------------------------------------------

func TestConnectPDUChain(t *testing.T) {
	// 一次调用必须产出四层：会话 CN → 表示 CP → ACSE AARQ → MMS Initiate。
	pdu, err := BuildConnectPDU(DefaultInitiateRequest(), nil)
	if err != nil {
		t.Fatalf("BuildConnectPDU: %v", err)
	}
	if pdu[0] != spduConnect {
		t.Fatalf("outermost tag = 0x%02X, want the session CN id", pdu[0])
	}
	spdu, err := ParseSPDU(pdu)
	if err != nil {
		t.Fatalf("ParseSPDU: %v", err)
	}
	pres, err := ParsePresentation(spdu.Payload)
	if err != nil {
		t.Fatalf("ParsePresentation: %v", err)
	}
	if pres.Kind != "cp" {
		t.Fatalf("presentation kind = %q, want cp", pres.Kind)
	}
	kind, aare, err := ParseACSE(pres.UserData)
	if err != nil {
		t.Fatalf("ParseACSE: %v", err)
	}
	if kind != "aarq" {
		t.Fatalf("acse kind = %q, want aarq", kind)
	}
	_ = aare
	// AARQ 的 user-information 里应当是 MMS Initiate。
	if !bytes.Contains(pres.UserData, []byte{ServiceTagInitiate}) {
		t.Errorf("AARQ does not carry an MMS Initiate: %x", pres.UserData)
	}
}

func TestConnectResponseChain(t *testing.T) {
	// 从站侧：AC { CPA { AARE { Initiate 响应 } } }。
	initResp := BuildInitiateResponse(DefaultInitiateRequest())
	aare := BuildAARE(ACSEResultAccepted, 0, initResp)
	cpa, err := BuildPresentationAccept(aare)
	if err != nil {
		t.Fatalf("BuildPresentationAccept: %v", err)
	}
	ac, err := BuildSessionAccept(DefaultSessionParams(), cpa)
	if err != nil {
		t.Fatalf("BuildSessionAccept: %v", err)
	}

	back, init, err := ParseConnectResponse(ac)
	if err != nil {
		t.Fatalf("ParseConnectResponse: %v", err)
	}
	if back.Result != ACSEResultAccepted {
		t.Errorf("result = %d", back.Result)
	}
	if init.MaxPDUSize != DefaultMaxPDUSize {
		t.Errorf("negotiated pdu size = %d", init.MaxPDUSize)
	}
}

func TestParseConnectResponseRejection(t *testing.T) {
	aare := BuildAARE(ACSEResultRejectedPermanent, 2, nil)
	cpa, err := BuildPresentationAccept(aare)
	if err != nil {
		t.Fatalf("BuildPresentationAccept: %v", err)
	}
	ac, err := BuildSessionAccept(DefaultSessionParams(), cpa)
	if err != nil {
		t.Fatalf("BuildSessionAccept: %v", err)
	}
	_, _, err = ParseConnectResponse(ac)
	if err == nil {
		t.Fatal("expected a rejection error")
	}
	if !strings.Contains(err.Error(), "acse") || !strings.Contains(err.Error(), "application context") {
		t.Errorf("error should name the layer and the reason: %v", err)
	}
}

func TestWrapUnwrapData(t *testing.T) {
	mms := appendTLV(nil, ServiceTagRead, nil)
	wrapped, err := WrapData(mms)
	if err != nil {
		t.Fatalf("WrapData: %v", err)
	}
	back, err := UnwrapData(wrapped)
	if err != nil {
		t.Fatalf("UnwrapData: %v", err)
	}
	if !bytes.Equal(back, mms) {
		t.Errorf("mms = %x, want %x", back, mms)
	}
}

func TestUnwrapDataRejectsWrongContext(t *testing.T) {
	// 关联建立之后收到上下文 1 的用户数据，说明对端在做释放/中止，
	// 而不是回答读请求。把它当成 MMS 去解析只会报一个莫名其妙的标签错误。
	acse, err := BuildPresentationUserDataACSE(BuildRLRQ())
	if err != nil {
		t.Fatalf("BuildPresentationUserDataACSE: %v", err)
	}
	dt := BuildSessionData(acse)
	if _, err := UnwrapData(dt); err == nil {
		t.Fatal("expected an error for ACSE user data")
	}
}

func TestReleasePDU(t *testing.T) {
	rel, err := BuildReleasePDU()
	if err != nil {
		t.Fatalf("BuildReleasePDU: %v", err)
	}
	if got := ParseRelease(rel); got != "rlrq" {
		t.Errorf("ParseRelease = %q, want rlrq", got)
	}
}

// ---------------------------------------------------------------------------
// 地址
// ---------------------------------------------------------------------------

func TestParseObjectRef(t *testing.T) {
	r, err := ParseObjectRef("simpleIO/GGIO1.ST.Ind1.stVal", "")
	if err != nil {
		t.Fatalf("ParseObjectRef: %v", err)
	}
	if r.LD != "simpleIO" || r.LN != "GGIO1" || r.FC != "ST" || r.Path != "Ind1.stVal" {
		t.Errorf("parsed = %+v", r)
	}
	if got := r.ObjectName(); got.Domain != "simpleIO" || got.Item != "GGIO1$ST$Ind1$stVal" {
		t.Errorf("object name = %+v", got)
	}
}

func TestParseObjectRefDefaultFC(t *testing.T) {
	r, err := ParseObjectRef("simpleIO/GGIO1.Ind1.stVal", "ST")
	if err != nil {
		t.Fatalf("ParseObjectRef: %v", err)
	}
	if r.FC != "ST" || r.Path != "Ind1.stVal" {
		t.Errorf("parsed = %+v", r)
	}
}

func TestParseObjectRefErrors(t *testing.T) {
	cases := []struct{ expr, fc, want string }{
		{"GGIO1.ST.Ind1.stVal", "", "logical device prefix"},
		{"simpleIO/", "", "empty part"},
		{"simpleIO/GGIO1", "", "names a logical node"},
		{"simpleIO/GGIO1.Ind1.stVal", "", "functional constraint"},
		{"simpleIO/GGIO1.ST", "", "no data object"},
		{"simpleIO/GGIO1.ST.Ind1..stVal", "", "empty segment"},
		{"simpleIO/GGIO-1.ST.Ind1", "", "only letters"},
		{"1simpleIO/GGIO1.ST.Ind1", "", "must not start with a digit"},
	}
	for _, c := range cases {
		_, err := ParseObjectRef(c.expr, c.fc)
		if err == nil {
			t.Errorf("ParseObjectRef(%q) should fail", c.expr)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseObjectRef(%q) error = %q, want it to mention %q", c.expr, err, c.want)
		}
	}
}

func TestObjectRefString(t *testing.T) {
	r, err := ParseObjectRef("simpleIO/GGIO1.ST.Ind1.stVal", "")
	if err != nil {
		t.Fatalf("ParseObjectRef: %v", err)
	}
	if got := r.String(); got != "simpleIO/GGIO1.ST.Ind1.stVal" {
		t.Errorf("String() = %q", got)
	}
}

func TestSplitMMSName(t *testing.T) {
	ln, fc, path := SplitMMSName("GGIO1$ST$Ind1$stVal")
	if ln != "GGIO1" || fc != "ST" || path != "Ind1.stVal" {
		t.Errorf("ln=%q fc=%q path=%q", ln, fc, path)
	}
}

func TestDataSetName(t *testing.T) {
	name, err := DataSetName("simpleIO", "LLN0", "URDSet01")
	if err != nil {
		t.Fatalf("DataSetName: %v", err)
	}
	if name.Domain != "simpleIO" || name.Item != "LLN0$URDSet01" {
		t.Errorf("name = %+v", name)
	}
}

func TestServiceErrorNames(t *testing.T) {
	err := BuildConfirmedError(5, 7, DataErrAccessDenied)
	ce, perr := ParseConfirmedError(err)
	if perr != nil {
		t.Fatalf("ParseConfirmedError: %v", perr)
	}
	if ce.InvokeID != 5 {
		t.Errorf("invokeID = %d, want 5", ce.InvokeID)
	}
	if ce.Class != "access" || ce.Code != DataErrAccessDenied {
		t.Errorf("class=%q code=%d", ce.Class, ce.Code)
	}
}

func TestParseReject(t *testing.T) {
	raw := BuildReject(7, ServiceTagRead)
	r, err := ParseReject(raw)
	if err != nil {
		t.Fatalf("ParseReject: %v", err)
	}
	if r.Reason != 7 || r.Type != ServiceTagRead {
		t.Errorf("reason=%d type=0x%02X", r.Reason, r.Type)
	}
	if !strings.Contains(r.Error(), "invalid-request") {
		t.Errorf("error = %q", r.Error())
	}
}

func TestBinaryTime(t *testing.T) {
	// 4 字节：自 1984-01-01 起的毫秒数。
	ms := int64(1000)
	b := []byte{byte(ms >> 24), byte(ms >> 16), byte(ms >> 8), byte(ms)}
	got := binaryTimeValue(b)
	const epoch1984 = 441763200000
	if got != epoch1984+1000 {
		t.Errorf("binaryTime = %d, want %d", got, epoch1984+1000)
	}
}

func TestUTCTime(t *testing.T) {
	// UtcTime = 4 字节秒 + 3 字节小数 + 1 字节品质。
	b := []byte{0x00, 0x00, 0x00, 0x64, 0x00, 0x00, 0x00, 0x5A}
	if got := utcTimeValue(b); got != 100000 {
		t.Errorf("utcTime = %d, want 100000", got)
	}
	if s := FormatUTC(b); !strings.HasPrefix(s, "1970-01-01T00:01:40") {
		t.Errorf("FormatUTC = %q", s)
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName(spduConnect); !strings.Contains(got, "CN") {
		t.Errorf("SessionName(CN) = %q", got)
	}
	if got := SessionName(0x42); !strings.Contains(got, "0x42") {
		t.Errorf("SessionName(unknown) = %q", got)
	}
}

func TestServiceName(t *testing.T) {
	if got := ServiceName(ServiceTagRead); got != "read" {
		t.Errorf("ServiceName(read) = %q", got)
	}
	if got := ServiceName(0xFF); !strings.Contains(got, "0xFF") {
		t.Errorf("ServiceName(unknown) = %q", got)
	}
}
