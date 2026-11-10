package hostlink

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// 测试向量全部来自手册的算例与一份独立的可运行实现（脚本语言），由
// 脚本自己算 FCS。用被测代码去生成「期望值」只能证明代码没变，不能证明
// 它是对的；所以这里的十六进制串是手写的。

const (
	// 请求：单元号 00、等待 0、直连、DM 读 D0..D2（区码 82，字数 0003）。
	goldenReadRequest = "@00FA0" + "00" + "00" + "00" + "00" + "0101" + "82" + "0000" + "00" + "0003" + "7E*"
	// 应答：结束码 00、ICF 40、DM 读回两个字 1234 5678。
	goldenReadResponse = "@00FA" + "00" + "40" + "00" + "00" + "00" + "0101" + "0000" + "1234" + "5678" + "4B*"
	// 请求：CIO 位读，起始字 0000 的第 0 位起 16 位（区码 30）。
	goldenBitRequest = "@00FA0" + "00" + "00" + "00" + "00" + "0101" + "30" + "0000" + "00" + "0010" + "75*"
)

func withCR(s string) []byte { return append([]byte(s), CR) }

func build(t *testing.T, r Request) string {
	t.Helper()
	b, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b[len(b)-1] != CR || b[len(b)-2] != Terminator {
		t.Fatalf("frame does not end with * CR: %q", b)
	}
	return string(b[:len(b)-1]) // 去掉 CR 便于与手册对照
}

func TestBuildReadRequestMatchesGolden(t *testing.T) {
	text, _ := hex.DecodeString("82000000" + "0003")
	got := build(t, Request{Unit: 0, Wait: 0, ICF: 0x00, MRC: 1, SRC: 1, Text: text})
	if got != goldenReadRequest {
		t.Fatalf("frame mismatch\n got %q\nwant %q", got, goldenReadRequest)
	}
}

func TestBuildBitReadRequestMatchesGolden(t *testing.T) {
	text, _ := hex.DecodeString("300000" + "00" + "0010")
	got := build(t, Request{Unit: 0, Wait: 0, ICF: 0x00, MRC: 1, SRC: 1, Text: text})
	if got != goldenBitRequest {
		t.Fatalf("frame mismatch\n got %q\nwant %q", got, goldenBitRequest)
	}
}

func TestBuildResponseMatchesGolden(t *testing.T) {
	data, _ := hex.DecodeString("12345678")
	b, err := BuildResponse(Response{
		Unit: 0, EndCode: 0, ICF: 0x40, DA2: 0, SA2: 0, SID: 0,
		MRC: 1, SRC: 1, FinsEndCode: 0, Data: data,
	})
	if err != nil {
		t.Fatalf("BuildResponse: %v", err)
	}
	if got := string(b[:len(b)-1]); got != goldenReadResponse {
		t.Fatalf("frame mismatch\n got %q\nwant %q", got, goldenReadResponse)
	}
}

func TestParseResponseRoundTrip(t *testing.T) {
	f, err := ParseResponse(withCR(goldenReadResponse))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if f.Unit != 0 || f.EndCode != 0 || f.ICF != 0x40 || f.MRC != 1 || f.SRC != 1 {
		t.Fatalf("header = %+v", f)
	}
	if f.FinsEndCode != 0 {
		t.Fatalf("FINS end code = 0x%04X", f.FinsEndCode)
	}
	if want, _ := hex.DecodeString("12345678"); !bytes.Equal(f.Data, want) {
		t.Fatalf("data = % x, want % x", f.Data, want)
	}
}

func TestParseRequestRoundTrip(t *testing.T) {
	r, err := ParseRequest(withCR(goldenReadRequest))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if r.Unit != 0 || r.Wait != 0 || r.ICF != 0 || r.MRC != 1 || r.SRC != 1 {
		t.Fatalf("header = %+v", r)
	}
	if want, _ := hex.DecodeString("820000000003"); !bytes.Equal(r.Text, want) {
		t.Fatalf("text = % x, want % x", r.Text, want)
	}
}

// TestFCSRangeIncludesStart 检查 FCS 的范围从 `@` 起算。少算 `@` 是最常见的
// 写法错误，而且本地回环两边都少算时会「看起来通过」，所以这里直接对比。
func TestFCSRangeIncludesStart(t *testing.T) {
	body := []byte(goldenReadResponse[:len(goldenReadResponse)-3]) // 去掉 FCS*CR
	full := FCS(body)
	without := FCS(body[1:])
	if full == without {
		t.Fatalf("FCS ignoring '@' must differ from the correct value (%02X)", full)
	}
	if got := body[len(body):]; len(got) != 0 {
		t.Fatal("unreachable")
	}
	if want := goldenReadResponse[len(goldenReadResponse)-3 : len(goldenReadResponse)-1]; want != hexUpper(full) {
		t.Fatalf("FCS = %02X, golden %s", full, want)
	}
}

func hexUpper(b byte) string {
	const d = "0123456789ABCDEF"
	return string([]byte{d[b>>4], d[b&0x0f]})
}

func TestParseResponseRejectsBadFCS(t *testing.T) {
	bad := strings.Replace(goldenReadResponse, "4B*", "4C*", 1)
	_, err := ParseResponse(withCR(bad))
	var mm *FCSMismatch
	if !errors.As(err, &mm) {
		t.Fatalf("err = %v, want *FCSMismatch", err)
	}
	if len(mm.Raw) == 0 {
		t.Fatal("FCSMismatch must carry the raw frame for diagnosis")
	}
	if mm.Want != 0x4c {
		t.Fatalf("Want = %02X", mm.Want)
	}
}

func TestParseResponseRejectsCmodeHeader(t *testing.T) {
	// C 模式应答（RR 命令，读 8 个通道）：线上是合法的 Host Link 帧，但不是
	// FINS 命令。按 FINS 的布局去解它会得到「ICF=00、DA2=00…」，一路错下去。
	frame := "@00RR" + "00" + "0000" + "0001" + "0002" + "0003" + "0004" + "0005" + "0006" + "0007"
	sum := FCS([]byte(frame))
	_, err := ParseResponse(withCR(frame + hexUpper(sum) + "*"))
	if err == nil || !strings.Contains(err.Error(), "C-mode") {
		t.Fatalf("err = %v, want a C-mode complaint", err)
	}
}

func TestParseRejectsMissingTerminator(t *testing.T) {
	noTerm := strings.TrimSuffix(goldenReadResponse, "*") + "4B" // FCS 后面直接 CR
	if _, err := ParseResponse(withCR(noTerm)); err == nil {
		t.Fatal("frame without '*' must be rejected")
	}
	// 反过来：缺 CR。
	if _, err := ParseResponse([]byte(goldenReadResponse)); err == nil {
		t.Fatal("frame without CR must be rejected")
	}
}

func TestParseRejectsOddDataField(t *testing.T) {
	// 在数据段插一个半字节。
	body := "@00FA004000000001010000" + "123" // 3 个字符
	sum := FCS([]byte(body))
	frame := body + hexUpper(sum) + "*"
	if _, err := ParseResponse(withCR(frame)); err == nil {
		t.Fatal("odd-length data field must be rejected")
	}
}

func TestEndCodeClassification(t *testing.T) {
	cases := map[byte]bool{0x13: true, 0x14: true, 0x15: true, 0x18: true, 0x01: false, 0x21: false}
	for code, want := range cases {
		if got := (&EndCodeError{Code: code}).Format(); got != want {
			t.Fatalf("Format(0x%02X) = %v, want %v", code, got, want)
		}
	}
}

func TestEndCodeErrorIsNotSilent(t *testing.T) {
	body := "@00FA" + "13" + "40" + "00" + "00" + "00" + "0101" + "0000" + "1234"
	sum := FCS([]byte(body))
	f, err := ParseResponse(withCR(body + hexUpper(sum) + "*"))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if f.EndCode != 0x13 {
		t.Fatalf("EndCode = 0x%02X", f.EndCode)
	}
	if !(&EndCodeError{Code: f.EndCode}).Format() {
		t.Fatal("0x13 must be classified as a framing error")
	}
}

func TestBuildRejectsOutOfRange(t *testing.T) {
	if _, err := Build(Request{Unit: 32}); err == nil {
		t.Fatal("unit 32 must be rejected")
	}
	if _, err := Build(Request{Unit: 0, Wait: 0x10}); err == nil {
		t.Fatal("wait 0x10 must be rejected (single hex digit)")
	}
	if _, err := Build(Request{Unit: 0, Text: make([]byte, MaxTextChars/2+1)}); err == nil {
		t.Fatal("oversized text must be rejected")
	}
}

// TestFrameBudget 检查推导出来的元素上限真的能塞进帧里，而且再多一个就
// 塞不进去。这两个数字（269/267）是照抄 FINS/TCP 的 999 时最先撞墙的地方。
func TestFrameBudget(t *testing.T) {
	// 读 269 字：正文只有 12 字符参数。
	readText := make([]byte, 6)
	if n := cmdOverhead + 12; n > 1000 {
		t.Fatalf("read command overhead %d", n)
	}
	if MaxReadWords*4 != MaxDataChars {
		t.Fatalf("MaxReadWords = %d", MaxReadWords)
	}
	if MaxWriteWords*4+memParamChars != MaxTextChars {
		t.Fatalf("MaxWriteWords = %d", MaxWriteWords)
	}
	// 写 267 字：正文 6 字节参数 + 534 字节数据 = 540 字节 = 1080 字符。
	text := make([]byte, 6+MaxWriteWords*2)
	if _, err := Build(Request{Unit: 0, MRC: 1, SRC: 2, Text: text}); err != nil {
		t.Fatalf("write at the word limit must fit: %v", err)
	}
	text = append(text, 0, 0)
	if _, err := Build(Request{Unit: 0, MRC: 1, SRC: 2, Text: text}); err == nil {
		t.Fatal("one word beyond the limit must be rejected")
	}
	_ = readText
}

func TestFINSRoundTrip(t *testing.T) {
	data, _ := hex.DecodeString("12345678")
	// 构造请求 → 转 FINS → 回填应答 → 解回 FINS。
	text, _ := hex.DecodeString("82000000" + "0002")
	req := Request{Unit: 7, Wait: 2, ICF: 0x00, MRC: 1, SRC: 1, Text: text}
	resp := Reply(req, 0x1103, data)
	if resp.Unit != 7 || resp.SID != 0 || resp.MRC != 1 || resp.SRC != 1 {
		t.Fatalf("reply header = %+v", resp)
	}
	if resp.ICF != 0x40 {
		t.Fatalf("直连应答 ICF 必须是 40，got %02X", resp.ICF)
	}
	raw := resp.FINS()
	if len(raw) != 14+len(data) {
		t.Fatalf("FINS frame length = %d", len(raw))
	}
	if raw[12] != 0x11 || raw[13] != 0x03 {
		t.Fatalf("FINS end code = %02X%02X", raw[12], raw[13])
	}
	// 再压回 Host Link 信封，验证有损字段被正确丢弃。
	back, err := FromFINS(7, 0, 0x00, raw)
	if err != nil {
		t.Fatalf("FromFINS: %v", err)
	}
	if back.MRC != 1 || back.SRC != 1 || back.Unit != 7 {
		t.Fatalf("header = %+v", back)
	}
}

func TestReplyEchoesNetworkBit(t *testing.T) {
	if got := Reply(Request{ICF: 0x80}, 0, nil).ICF; got != 0xc0 {
		t.Fatalf("跨网请求的应答 ICF = %02X, want C0", got)
	}
	if got := Reply(Request{ICF: 0x00}, 0, nil).ICF; got != 0x40 {
		t.Fatalf("直连请求的应答 ICF = %02X, want 40", got)
	}
}

func TestUnitOutOfRangeRejected(t *testing.T) {
	frame := "@32" + "FA" + "0" + "00000000" + "0101"
	sum := FCS([]byte(frame))
	if _, err := ParseRequest(withCR(frame + hexUpper(sum) + "*")); err == nil {
		t.Fatal("unit 32 must be rejected")
	}
}
