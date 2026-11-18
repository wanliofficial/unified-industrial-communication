package dlms

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// 本文件补的是「自检命令、日志与错误路径会用到、但不参与正常采集主路径」
// 的那部分表面：结果码翻译、一致性位打印、地址校验、A-XDR 各分支的
// 编解码、Get-Response 的三个分支、OBIS 六段访问器。
//
// 这些代码在成功路径上一次都不会跑到，于是很容易在覆盖率上被忽略；
// 但现场排障时看到的每一行中文都是从它们出来的——`-raw` 打的结果码、
// `-probe` 打的能力位、地址写错时的报错原因，全靠这一层。

// ---------------------------------------------------------------------------
// 结果码与一致性位
// ---------------------------------------------------------------------------

// 每个 Data-Access-Result 都要有中文说明，且未知值要显式说"未知"而不是
// 返回空串——表格里一列空白会让人以为"没有错误"。
func TestDataAccessResultNames(t *testing.T) {
	cases := map[DataAccessResult]string{
		DARSuccess:                 "成功",
		DARHardwareFault:           "硬件故障",
		DARTemporaryFailure:        "暂时失败",
		DARReadWriteDenied:         "拒绝读写",
		DARObjectUndefined:         "对象未定义",
		DARObjectClassInconsistent: "对象类不一致",
		DARObjectUnavailable:       "对象不可用",
		DARTypeUnmatched:           "类型不匹配",
		DARScopeOfAccessViolated:   "访问范围越界",
		DARDataBlockUnavailable:    "数据块不可用",
		DARLongGetAborted:          "长读被中止",
		DARNoLongGetInProgress:     "没有进行中的长读",
		DARLongSetAborted:          "长写被中止",
		DARNoLongSetInProgress:     "没有进行中的长写",
		DAROtherReason:             "其它原因",
	}
	for code, want := range cases {
		got := code.String()
		if !strings.Contains(got, want) {
			t.Errorf("结果码 %d 的说明 %q 不含 %q", int(code), got, want)
		}
		// 现场最容易混淆的两个码必须能区分开。
		if code == DARObjectUndefined && strings.Contains(got, "类不一致") {
			t.Errorf("object-undefined 的说明混进了类不一致")
		}
	}
	if got := DataAccessResult(99).String(); !strings.Contains(got, "未知") {
		t.Errorf("未知结果码应显式说未知，得到 %q", got)
	}
	if !DARSuccess.OK() {
		t.Error("success 应算成功")
	}
	// 其余一律不算成功：把 0 之外的值判成功会让驱动把错误码当成读数。
	for _, code := range []DataAccessResult{DARHardwareFault, DARObjectUndefined, DAROtherReason} {
		if code.OK() {
			t.Errorf("结果码 %d 不应算成功", int(code))
		}
	}
}

// conformance 打印出来的是 `uic dlmsread -probe` 的那一行。位名必须
// 与标准服务名一致，否则现场对照 Blue Book 会找不到。
func TestConformanceString(t *testing.T) {
	c := ConfGet | ConfAttribute0Get | ConfBlockTransferGet | ConfMultipleReferences
	got := c.String()
	for _, want := range []string{"get", "attribute0-get", "block-transfer-get", "multiple-references"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q 缺 %q", got, want)
		}
	}
	// 未置位的服务不能出现。
	for _, bad := range []string{"write", "set", "action"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q 不应含未置位的 %q", got, bad)
		}
	}
	// 全零位块要说"空"，不能返回空串（表格里会出现一列空白）。
	if got := Conformance(0).String(); got != "（空）" {
		t.Errorf("全零 = %q", got)
	}
}

// 默认建链参数只声明真正会用到的能力。多声明会让表按"你支持分块"回话。
func TestDefaultInitiateRequest(t *testing.T) {
	r := DefaultInitiateRequest()
	if r.DLMSVersion != 6 {
		t.Errorf("DLMS 版本 = %d", r.DLMSVersion)
	}
	if r.MaxReceivePDU == 0 {
		t.Error("max-receive-PDU 不能为 0")
	}
	if !r.Conformance.Has(ConfGet) {
		t.Error("必须声明 get")
	}
	if r.Conformance.Has(ConfBlockTransferGet) {
		t.Error("未实现分块读，不应声明 block-transfer-get")
	}
	if r.Conformance.Has(ConfWrite) || r.Conformance.Has(ConfSet) || r.Conformance.Has(ConfAction) {
		t.Error("未实现写方向，不应声明 write/set/action")
	}

	// 编出来的字节必须能被自己的解析器读回来（请求分支）。
	enc := r.Encode()
	got, used, err := parseInitiate(enc, true)
	if err != nil {
		t.Fatalf("parseInitiate: %v", err)
	}
	if used != len(enc) {
		t.Errorf("消耗 %d 字节，编码是 %d 字节", used, len(enc))
	}
	if got.DLMSVersion != r.DLMSVersion || got.MaxReceivePDU != r.MaxReceivePDU {
		t.Errorf("往返 = %+v", got)
	}
	if got.Conformance != r.Conformance {
		t.Errorf("conformance 往返 = %06X，应为 %06X", uint32(got.Conformance), uint32(r.Conformance))
	}
}

// initiate 解析的错误路径：标签不对、长度不够、conformance 长度非法，
// 都必须报错而不是猜一个默认值。
func TestParseInitiateErrors(t *testing.T) {
	good := DefaultInitiateRequest().Encode()
	if _, _, err := parseInitiate(good, false); err == nil {
		t.Error("把请求标签当应答解析应报错")
	}
	// 截断到每个长度都应报"不完整"，不能越界读。
	for i := 0; i < len(good); i++ {
		if _, _, err := parseInitiate(good[:i], true); err == nil {
			t.Fatalf("截断到 %d 字节时应报错", i)
		}
	}
	// conformance 长度字段被改小：不是 4 就非法。
	bad := append([]byte(nil), good...)
	idx := bytes.Index(bad, []byte{TagConformance, TagConformance2, 0x04})
	if idx < 0 {
		t.Fatal("没找到 conformance 段")
	}
	bad[idx+2] = 0x02
	if _, _, err := parseInitiate(bad, true); err == nil {
		t.Error("conformance 长度 2 应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// 描述符与 Get-Response 的三个分支
// ---------------------------------------------------------------------------

func TestAttributeDescriptorRoundTrip(t *testing.T) {
	a := AttributeDescriptor{
		ClassID:    ClassRegister,
		InstanceID: [6]byte{1, 0, 1, 8, 0, 255},
		Attribute:  AttrValue,
	}
	enc := a.Encode()
	if len(enc) != 9 {
		t.Fatalf("描述符应为 9 字节，得到 %d", len(enc))
	}
	got, used, err := DecodeAttributeDescriptor(enc)
	if err != nil || used != 9 {
		t.Fatalf("DecodeAttributeDescriptor: %v used=%d", err, used)
	}
	if got != a {
		t.Fatalf("往返 = %+v，应为 %+v", got, a)
	}
	if _, _, err := DecodeAttributeDescriptor(enc[:8]); !errors.Is(err, ErrIncomplete) {
		t.Errorf("8 字节应报 ErrIncomplete，得到 %v", err)
	}
}

// with-datablock 分支：表在应答超过 max-PDU 时会走这里。驱动虽然不做
// 拼块，但**必须**能识别出来并报告，否则现场只看到"值读出来是半截"。
func TestGetResponseDataBlock(t *testing.T) {
	// C4 + 类型 02（with-datablock）+ invoke 81；last=1；
	// block-number=1；raw-data [0] 长度 3 内容 01 02 03。
	apdu := []byte{TagGetResponse, GetKindNext, InvokeID,
		0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x03, 0x01, 0x02, 0x03}
	resp, err := ParseGetResponse(apdu)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if resp.Kind != 2 {
		t.Fatalf("kind = %d", resp.Kind)
	}
	if !resp.Block.LastBlock {
		t.Error("last-block 应为 1")
	}
	if resp.Block.BlockNo != 1 {
		t.Errorf("块号 = %d", resp.Block.BlockNo)
	}
	if !bytes.Equal(resp.Block.Raw, []byte{1, 2, 3}) {
		t.Errorf("块内容 = % X", resp.Block.Raw)
	}
	if resp.Block.IsError {
		t.Error("raw-data 分支不应标成错误")
	}
}

// 分块里的错误分支：表用 data-access-result 表示"这一块拿不到"。
func TestGetResponseDataBlockError(t *testing.T) {
	// C4 + 类型 02 + invoke；last-block(1) + block-number(4) + 标签(1) + 结果码(1)。
	apdu := []byte{TagGetResponse, GetKindNext, InvokeID,
		0x01, 0x00, 0x00, 0x00, 0x02, 0x01, byte(DARDataBlockUnavailable)}
	resp, err := ParseGetResponse(apdu)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !resp.Block.IsError {
		t.Fatal("应标成错误分支")
	}
	if resp.Block.Result != DARDataBlockUnavailable {
		t.Errorf("结果 = %d", resp.Block.Result)
	}
	if len(resp.Block.Raw) != 0 {
		t.Errorf("错误分支不应有内容：% X", resp.Block.Raw)
	}
}

// with-list 分支（multiple-references）。本驱动不声明这个能力，但表可能
// 主动这么回，解析必须能识别而不是当成坏帧。
func TestGetResponseWithList(t *testing.T) {
	a, err := EncodeGetResponseNormal(InvokeID, U32(1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeGetResponseNormal(InvokeID, U32(2))
	if err != nil {
		t.Fatal(err)
	}
	// 取两条 normal 应答的「Get-Data-Result」部分（去掉 C4、类型、invoke）。
	partA := a[3:]
	partB := b[3:]
	body := []byte{InvokeID, 0x02}
	body = append(body, partA...)
	body = append(body, partB...)
	apdu := append([]byte{TagGetResponse, GetKindList}, body...)

	resp, err := ParseGetResponse(apdu)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if resp.Kind != 3 {
		t.Fatalf("kind = %d", resp.Kind)
	}
	if len(resp.WithList) != 2 {
		t.Fatalf("元素数 = %d", len(resp.WithList))
	}
	for i, want := range []float64{1, 2} {
		n, ok := resp.WithList[i].Data.Number()
		if !ok || n != want {
			t.Errorf("第 %d 条 = %v", i+1, n)
		}
	}
}

// DataBlock-G 的错误路径：raw-data 截断、未知 result 标签。
func TestParseDataBlockErrors(t *testing.T) {
	// 只有 5 字节：不够 last-block + block-number。
	if _, _, err := parseDataBlock([]byte{0x01, 0, 0, 0, 0}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("5 字节应报 ErrIncomplete，得到 %v", err)
	}
	// raw-data 分支但缺长度字节。
	if _, _, err := parseDataBlock([]byte{0x00, 0, 0, 0, 1, 0x00}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("缺长度应报 ErrIncomplete，得到 %v", err)
	}
	// raw-data 长度声称 3 但只有 1 字节内容。
	if _, _, err := parseDataBlock([]byte{0x00, 0, 0, 0, 1, 0x00, 0x03, 0xAA}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("内容截断应报 ErrIncomplete，得到 %v", err)
	}
	// 既不是 raw-data 也不是 data-access-result。
	if _, _, err := parseDataBlock([]byte{0x00, 0, 0, 0, 1, 0x7F}); err == nil {
		t.Error("未知 result 标签应报错")
	}
}

// Get-Response 的结构错误必须报错：坏标签、长度越界、payload 截断。
func TestGetResponseErrors(t *testing.T) {
	if _, err := ParseGetResponse([]byte{TagGetResponse}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("单字节应报 ErrIncomplete，得到 %v", err)
	}
	if _, err := ParseGetResponse([]byte{TagGetResponse, GetKindNormal}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("缺 invoke 应报 ErrIncomplete，得到 %v", err)
	}
	// 不是 Get-Response 的标签。
	if _, err := ParseGetResponse([]byte{TagAARQ, 0x01, 0x00}); err == nil {
		t.Error("AARQ 标签不应被当成 Get-Response")
	}
	// Set-Response（0xC5）不能被当成 Get-Response 的变体。
	if _, err := ParseGetResponse([]byte{TagSetResponse, GetKindNormal, InvokeID, 0x00}); err == nil {
		t.Error("Set-Response 标签应明确报错")
	}
	// normal 分支但正文为空。
	if _, err := ParseGetResponse([]byte{TagGetResponse, GetKindNormal, InvokeID}); err == nil {
		t.Error("空正文应报错")
	}
	// Get-Data-Result 的标签不认识。
	bad := []byte{TagGetResponse, GetKindNormal, InvokeID, 0x7F, 0x00}
	if _, err := ParseGetResponse(bad); err == nil {
		t.Error("未知 Get-Data-Result 标签应报错")
	}
}

// data-access-result 在 A-XDR 里是 `[1] IMPLICIT ENUMERATED`，定长 8 位、
// 无长度前缀，线上只有 `01 <值>` 两个字节。
//
// 这里特意用 hardware-fault(1) 做向量：它的标准形态是 `01 01`，与"带长度
// 前缀的 `01 01 xx`"前两字节完全相同。解析端若去兼容那种非标写法，就会把
// 所有 hardware-fault 读成"数据不完整"。
func TestDataAccessResultForm(t *testing.T) {
	noLen := []byte{TagDataAccessResult, byte(DARHardwareFault)}
	dr, used, err := parseGetDataResult(noLen)
	if err != nil || used != 2 {
		t.Fatalf("标准形态: %v used=%d", err, used)
	}
	if dr.Result != DARHardwareFault || !dr.IsResult || dr.HasData {
		t.Errorf("标准形态解析 = %+v", dr)
	}
	// 结果码本身也要能区分成功与非成功。
	ok, _, err := parseGetDataResult([]byte{TagDataAccessResult, byte(DARSuccess)})
	if err != nil || !ok.Result.OK() {
		t.Errorf("success 形态解析 = %+v err=%v", ok, err)
	}
	if _, _, err := parseGetDataResult(nil); !errors.Is(err, ErrIncomplete) {
		t.Errorf("空输入应报 ErrIncomplete，得到 %v", err)
	}
	if _, _, err := parseGetDataResult([]byte{TagDataAccessResult}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("只有标签时应报 ErrIncomplete，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// A-XDR 的全部标签
// ---------------------------------------------------------------------------

// 每种标量的编解码、Kind、Number、String 都要自洽。标签值已在
// TestAXDRScalars 锁过，这里补的是"读出值之后怎么用"这一层。
func TestAXDRAllKinds(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		kind Kind
		num  float64
		str  string
	}{
		{"null", Value{Tag: TagNull}, KindNull, 0, "null"},
		{"dontcare", Value{Tag: TagDontCare}, KindNull, 0, "null"},
		{"bool-true", Bool(true), KindBool, 1, "true"},
		{"bool-false", Bool(false), KindBool, 0, "false"},
		{"i8", I8(-5), KindInt, -5, "-5"},
		{"i16", I16(-300), KindInt, -300, "-300"},
		{"i32", I32(-70000), KindInt, -70000, "-70000"},
		{"i64", I64(-1), KindInt, -1, "-1"},
		{"u8", U8(200), KindUint, 200, "200"},
		{"u16", U16(65535), KindUint, 65535, "65535"},
		{"u32", U32(4000000000), KindUint, 4000000000, "4000000000"},
		{"u64", U64(1 << 40), KindUint, float64(uint64(1) << 40), "1099511627776"},
		{"float32", Float32(1.5), KindFloat, 1.5, "1.5"},
		{"enum", Enum(3), KindInt, 3, "3"},
		{"visible", Text("AB"), KindString, 0, "AB"},
	}
	for _, c := range cases {
		if got := c.v.Kind(); got != c.kind {
			t.Errorf("%s: Kind = %v，应为 %v", c.name, got, c.kind)
		}
		n, ok := c.v.Number()
		switch c.kind {
		case KindInt, KindUint, KindFloat, KindBool:
			if !ok || n != c.num {
				t.Errorf("%s: Number = %v ok=%v，应为 %v", c.name, n, ok, c.num)
			}
		default:
			if ok {
				t.Errorf("%s: 非数值不应给出 Number", c.name)
			}
		}
		if got := c.v.String(); got != c.str {
			t.Errorf("%s: String = %q，应为 %q", c.name, got, c.str)
		}
	}
}

// octet-string / bit-string 的 String：前者十六进制，后者带位数与二进制。
// 现场核对表号与对象表时看的就是这两种形态。
func TestAXDRBytesFormatting(t *testing.T) {
	o := Octets([]byte{0x01, 0x00, 0x0D, 0x07})
	if got := o.String(); got != "01 00 0D 07" {
		t.Errorf("octet-string String = %q", got)
	}
	if got := o.Kind(); got != KindBytes {
		t.Errorf("Kind = %v", got)
	}
	bits := Value{Tag: TagBitString, Bytes: []byte{0xF0}, Int: 4}
	if got := bits.String(); !strings.Contains(got, "bits(8)") {
		t.Errorf("bit-string String = %q，应说明位数", got)
	}
	// BCD 走 Int 通道，但 String 要显式标出 bcd，避免被当成普通整数。
	bcd := Value{Tag: TagBCD, Int: 12}
	if got := bcd.String(); !strings.HasPrefix(got, "bcd(") {
		t.Errorf("bcd String = %q", got)
	}
}

// 每个标签都要有可读名。空名会让「未知标签 0x00」这种错误失去线索。
func TestTagName(t *testing.T) {
	tags := []byte{TagNull, TagArray, TagStructure, TagBoolean, TagBitString,
		TagDoubleLong, TagDoubleLongUns, TagOctetString, TagVisibleString, TagBCD,
		TagInteger, TagLong, TagUnsigned, TagLongUnsigned, TagCompactArray,
		TagLong64, TagLong64Unsigned, TagEnum, TagFloat32, TagFloat64,
		TagDateTime, TagDate, TagTime, TagDontCare}
	for _, tag := range tags {
		name := tagName(tag)
		if name == "" || strings.HasPrefix(name, "未知") {
			t.Errorf("标签 %02X 没有可读名：%q", tag, name)
		}
	}
	if got := tagName(0x2A); !strings.Contains(got, "未知") {
		t.Errorf("未定义标签应说未知，得到 %q", got)
	}
}

// 序列 / 数组的 String 用括号展开；空序列也要能打印。
func TestAXDRSequenceFormatting(t *testing.T) {
	if got := Structure(I8(-1), Enum(34)).String(); got != "{-1, 34}" {
		t.Errorf("structure = %q", got)
	}
	if got := Array(U16(1), U16(2), U16(3)).String(); got != "{1, 2, 3}" {
		t.Errorf("array = %q", got)
	}
	if got := Structure().String(); got != "{}" {
		t.Errorf("空结构 = %q", got)
	}
	// 嵌套：scaler_unit 放在 structure 里是常见形态。
	nested := Structure(Structure(I8(-2), Enum(30)), Text("x"))
	if got := nested.String(); got != "{{-2, 30}, x}" {
		t.Errorf("嵌套 = %q", got)
	}
}

// 编码侧的边界：octet-string / visible-string 超过 255 字节要报错，
// 不能静默按 byte 截断长度（那会让接收方少读一半字节）。
func TestAXDREncodeLimits(t *testing.T) {
	big := make([]byte, 256)
	if _, err := Octets(big).Encode(); err == nil {
		t.Error("256 字节 octet-string 应被拒绝")
	}
	if _, err := Text(strings.Repeat("x", 256)).Encode(); err == nil {
		t.Error("256 字节 visible-string 应被拒绝")
	}
	// 255 字节是边界，必须能编出来。
	if _, err := Octets(big[:255]).Encode(); err != nil {
		t.Errorf("255 字节应可编码: %v", err)
	}
	// 256 个元素的序列也要拒绝。
	elems := make([]Value, 256)
	for i := range elems {
		elems[i] = U8(0)
	}
	if _, err := Structure(elems...).Encode(); err == nil {
		t.Error("256 元素序列应被拒绝")
	}
	// 未知标签编码要报错而不是输出空字节。
	if _, err := (&Value{Tag: 0x30}).Encode(); err == nil {
		t.Error("未知标签应报错")
	}
}

// Decode 的截断检查：每个标签切在每一个位置都不能越界读。
func TestAXDRDecodeTruncation(t *testing.T) {
	values := []Value{
		Bool(true), I8(-1), I16(-1), I32(-1), I64(-1),
		U8(1), U16(1), U32(1), U64(1),
		Float32(1), Float64(1), Octets([]byte{1, 2}), Text("ab"),
		Structure(I8(-1), Enum(0)),
		Value{Tag: TagDateTime, Raw: make([]byte, 12)},
		Value{Tag: TagDate, Raw: make([]byte, 5)},
		Value{Tag: TagTime, Raw: make([]byte, 4)},
	}
	for _, v := range values {
		enc, err := v.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for i := 0; i < len(enc); i++ {
			if _, _, err := Decode(enc[:i]); err == nil {
				t.Errorf("标签 %02X 截断到 %d/%d 字节时应报错", v.Tag, i, len(enc))
			}
		}
	}
	if _, _, err := Decode(nil); !errors.Is(err, ErrIncomplete) {
		t.Errorf("空输入应报 ErrIncomplete，得到 %v", err)
	}
}

// date_time 的 String 在解不出时间时要保留原始字节，不能输出零值时间。
func TestAXDRDateTimeUnparsable(t *testing.T) {
	v := Value{Tag: TagDateTime, Raw: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0}}
	got := v.String()
	if strings.Contains(got, "0001-01-01") {
		t.Errorf("未指定的时钟不应显示成零值时间：%q", got)
	}
	if !strings.Contains(got, "FF") {
		t.Errorf("应保留原始字节：%q", got)
	}
}

// 时区偏差是按分钟的有符号数，负偏差（西半球）也要正确。
func TestAXDRDateTimeNegativeZone(t *testing.T) {
	raw := []byte{0x07, 0xEA, 0x01, 0x02, 0x06, 0x0C, 0x00, 0x00, 0x00, 0xFE, 0xD4, 0x00}
	// 0xFED4 = -300 分钟 = UTC-5。
	tm, ok := (Value{Tag: TagDateTime, Raw: raw}).DateTime()
	if !ok {
		t.Fatal("应能解出")
	}
	if _, off := tm.Zone(); off != -5*3600 {
		t.Errorf("时区偏移 = %d 秒，应为 -18000", off)
	}
	if tm.Year() != 2026 || tm.Month() != 1 || tm.Day() != 2 {
		t.Errorf("日期 = %v", tm.Format("2006-01-02"))
	}
}

// ---------------------------------------------------------------------------
// 帧与地址
// ---------------------------------------------------------------------------

// 地址的三种宽度都要能编解码往返，越界要报错。
func TestAddressCheckAndBroadcast(t *testing.T) {
	bad := []Address{
		{Upper: 0x80, Size: 1},
		{Upper: 1, Lower: 1, Size: 1}, // 1 字节不应有 Lower
		{Upper: 0x80, Size: 2},
		{Upper: 1, Lower: 0x80, Size: 2},
		{Upper: 0x4000, Size: 4},
		{Upper: 1, Size: 3}, // 3 字节非法
		{Upper: 1, Size: 0}, // 0 字节非法
	}
	for _, a := range bad {
		if err := a.Check(); err == nil {
			t.Errorf("%+v 应被拒绝", a)
		}
		if _, err := a.Encode(); err == nil {
			t.Errorf("%+v 编码应报错", a)
		}
	}
	// 广播地址：1 字节全站是 0x7F，4 字节是 0x3FFF。
	if !ClientAddress(AddrAllStation).Broadcast() {
		t.Error("0x7F 应为广播")
	}
	if !ServerAddress(AddrAllStation2, AddrAllStation2, 4).Broadcast() {
		t.Error("4 字节全站地址应为广播")
	}
	if ClientAddress(1).Broadcast() {
		t.Error("1 号地址不是广播")
	}
	if got := ClientAddress(0x3A).String(); got != "3A" {
		t.Errorf("1 字节 String = %q", got)
	}
	if got := ServerAddress(0x12, 0x34, 2).String(); got != "12/34" {
		t.Errorf("2 字节 String = %q", got)
	}
}

// DecodeAddress 的失步与越界：没有结束位、超过 4 字节、3 字节。
func TestDecodeAddressErrors(t *testing.T) {
	if _, _, err := DecodeAddress(nil); !errors.Is(err, ErrIncomplete) {
		t.Errorf("空输入应报 ErrIncomplete，得到 %v", err)
	}
	// 全都不带结束位。
	if _, _, err := DecodeAddress([]byte{0x02, 0x04}); err == nil {
		t.Error("缺结束位应报错")
	}
	// 5 个"继续"字节，超过 4 字节上限。
	if _, _, err := DecodeAddress([]byte{0x02, 0x02, 0x02, 0x02, 0x03}); err == nil {
		t.Error("超过 4 字节应报错")
	}
	// 3 字节地址：结束位出现在第 3 字节，但 3 不是合法宽度。
	if _, _, err := DecodeAddress([]byte{0x02, 0x02, 0x03}); err == nil {
		t.Error("3 字节地址应报错")
	}
}

// 帧类型名要能打印未知值（日志里不会出现空字符串）。
func TestFrameKindString(t *testing.T) {
	known := map[FrameKind]string{
		KindI: "I", KindRR: "RR", KindRNR: "RNR", KindSNRM: "SNRM",
		KindDISC: "DISC", KindUA: "UA", KindDM: "DM", KindFRMR: "FRMR", KindUI: "UI",
	}
	for k, want := range known {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q，应为 %q", int(k), got, want)
		}
	}
	if got := KindUnknown.String(); got == "" {
		t.Error("未知帧类型也应有可读名")
	}
}

// 帧长超过 11 位上限时必须报错，不能静默截断长度字段——截断后接收方
// 会永远等不到结束标志，症状是"第一帧之后就超时"。
func TestFrameEncodeTooLong(t *testing.T) {
	fr := Frame{
		Dest:    ServerAddress(1, 0, 1),
		Src:     ClientAddress(16),
		Control: Control{Kind: KindI, PF: true},
		Info:    make([]byte, MaxFrameLength),
	}
	if _, err := fr.Encode(); err == nil {
		t.Error("超长帧应被拒绝")
	}
}

// Parse 的错误路径：格式类型不对、长度越界、HCS/FCS 坏、无 info 但长度不为 7。
func TestParseErrors(t *testing.T) {
	// 格式类型 0x20（类型 2）不是本实现支持的格式。
	if _, _, err := Parse([]byte{0x20, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01}); err == nil {
		t.Error("格式类型 2 应被拒绝")
	}
	// 长度字段 0 越界。
	if _, _, err := Parse([]byte{0xA0, 0x00}); err == nil {
		t.Error("长度 0 应被拒绝")
	}
	// 长度声称 7 但只给 3 字节（不含结束标志）。
	if _, _, err := Parse([]byte{0xA0, 0x07, 0x03}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("截断应报 ErrIncomplete，得到 %v", err)
	}
	// HCS 坏。
	bad := []byte{0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x00}
	if _, _, err := Parse(bad); !errors.Is(err, ErrChecksum) {
		t.Errorf("坏 HCS 应报 ErrChecksum，得到 %v", err)
	}
	// 单字节标志在 ParseRaw 里是"还没到"，不是错误。
	if _, n, err := ParseRaw([]byte{Flag}); !errors.Is(err, ErrIncomplete) || n != 1 {
		t.Errorf("只有标志时应报 ErrIncomplete 且跳过 1 字节，得到 %v/%d", err, n)
	}
	// 长度字段越界的 ParseRaw：丢 1 字节重扫。
	if _, n, err := ParseRaw([]byte{Flag, 0xA0, 0x00}); err == nil || n != 2 {
		t.Errorf("长度越界应报错并跳过标志+1，得到 %v/%d", err, n)
	}
}

// 分段位在组帧与解帧两侧都要保留：多帧传输靠它判断"后面还有"。
func TestFrameSegmented(t *testing.T) {
	fr := Frame{
		Dest:      ServerAddress(1, 0, 1),
		Src:       ClientAddress(16),
		Control:   Control{Kind: KindI, PF: true},
		Info:      []byte{0x01, 0x02},
		Segmented: true,
	}
	body, err := fr.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if body[0]&0x08 == 0 {
		t.Fatalf("分段位未置：%02X", body[0])
	}
	got, _, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Segmented {
		t.Error("分段位丢失")
	}
	// 长度字段仍是低 11 位，不能被分段位污染。
	if got.Length != len(body) {
		t.Errorf("长度 = %d，应为 %d", got.Length, len(body))
	}
}

// FCS 的固定余数性质是分帧的基础，随手改一个比特就必须失效。
func TestGoodFCSDetectsCorruption(t *testing.T) {
	// A0 07 03 21 93 是头（HCS 覆盖范围），其后的 0F 01 才是校验序列。
	head := []byte{0xA0, 0x07, 0x03, 0x21, 0x93}
	if GoodFCS(head) {
		t.Fatal("只有头、不含校验序列时不应通过")
	}
	full := appendFCS(append([]byte(nil), head...))
	if !bytes.Equal(full, []byte{0xA0, 0x07, 0x03, 0x21, 0x93, 0x0F, 0x01}) {
		t.Fatalf("FCS = % X，应为 0F 01", full[5:])
	}
	if !GoodFCS(full) {
		t.Fatalf("加上校验序列后应通过：% X", full)
	}
	corrupt := append([]byte(nil), full...)
	corrupt[0] ^= 0x01
	if GoodFCS(corrupt) {
		t.Error("改一个比特后应失效")
	}
}

// ---------------------------------------------------------------------------
// OBIS 与内置表
// ---------------------------------------------------------------------------

// OBIS 六段访问器各自取对应位置，别把 C 段与 D 段搞混（32 是电压、
// 7 是瞬时值，写反会读到另一个量）。
func TestOBISAccessors(t *testing.T) {
	o, err := ParseOBIS("1.0.32.7.0.255")
	if err != nil {
		t.Fatal(err)
	}
	if o.Group() != 1 {
		t.Errorf("A 段 = %d", o.Group())
	}
	if o.Channel() != 0 {
		t.Errorf("B 段 = %d", o.Channel())
	}
	if o.Quantity() != 32 {
		t.Errorf("C 段 = %d，应为 32", o.Quantity())
	}
	if o.Rate() != 7 {
		t.Errorf("D 段 = %d，应为 7", o.Rate())
	}
	if o.Tariff() != 0 {
		t.Errorf("E 段 = %d", o.Tariff())
	}
	if o.Storage() != 255 {
		t.Errorf("F 段 = %d", o.Storage())
	}
	if !o.IsCurrent() {
		t.Error("F=255 是当前值")
	}
}

// SplitRef 是驱动与 CLI 唯一的切分点，必须把「OBIS 内部冒号」和
// 「地址后缀冒号」区分开。
func TestSplitRef(t *testing.T) {
	ok := []struct {
		in    string
		obis  string
		class int
		attr  int
	}{
		{"1.0.1.8.0.255", "1.0.1.8.0.255", -1, -1},
		{"1.0.1.8.0.255:3", "1.0.1.8.0.255", 3, -1},
		{"1.0.1.8.0.255:3:2", "1.0.1.8.0.255", 3, 2},
		// 标准正文写法：冒号在 OBIS 内部。
		{"1-0:1-8-0-255", "1-0:1-8-0-255", -1, -1},
		{"1-0:1-8-0-255:3:2", "1-0:1-8-0-255", 3, 2},
		// 带空格：现场从文档里复制粘贴经常带空格。
		{" 1-0:1-8-0-255 : 3 : 2 ", "1-0:1-8-0-255", 3, 2},
	}
	for _, c := range ok {
		obis, class, attr, err := SplitRef(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if obis != c.obis || class != c.class || attr != c.attr {
			t.Errorf("%q → (%q,%d,%d)，应为 (%q,%d,%d)",
				c.in, obis, class, attr, c.obis, c.class, c.attr)
		}
	}

	bad := []string{
		"",
		"1.0.1.8.0",           // 只有 5 段
		"1.0.1.8.0.255:x",     // 类号不是数字
		"1.0.1.8.0.255:3:2:1", // 后缀三段
		"1.0.1.8.0.255:3:200", // 属性号超过 127
		"1.0.1.8.0.255:70000", // 类号超过 65535
		"1.0.1.8.0.256",       // OBIS 段越界
	}
	for _, in := range bad {
		if _, _, _, err := SplitRef(in); err == nil {
			t.Errorf("%q 应被拒绝", in)
		}
	}
}

// 单位表与类名表对未知值要显式说"未知"，不能返回某个默认单位
// （单位错会让换算结果差 1000 倍）。
func TestUnknownNames(t *testing.T) {
	if got := UnitName(0); got != "" {
		t.Errorf("单位 0（无量纲）应为空，得到 %q", got)
	}
	// 常见单位抽查：29=Wh、31=varh、32=A、33=C、34=V。这些是点表里最常见的几个。
	for u, want := range map[int]string{29: "Wh", 31: "varh", 32: "A", 33: "C", 34: "V"} {
		if got := UnitName(u); got != want {
			t.Errorf("单位 %d = %q，应为 %q", u, got, want)
		}
	}
	// 未列出的单位返回 `unit(N)`：驱动会原样上报数字，而不是猜一个单位。
	if got := UnitName(1000); got != "unit(1000)" {
		t.Errorf("未列出的单位 = %q，应为 unit(1000)", got)
	}
	if got := ClassName(65535); !strings.Contains(got, "未知") {
		t.Errorf("未知类应说未知，得到 %q", got)
	}
}

// 全部类号常量都要有中文名，且名字里带类号（现场对照标准查表时靠它）。
func TestClassNameAll(t *testing.T) {
	ids := []uint16{
		ClassData, ClassRegister, ClassExtendedRegister, ClassDemandRegister,
		ClassRegisterActivation, ClassProfileGeneric, ClassClock, ClassScriptTable,
		ClassSchedule, ClassSpecialDaysTable, ClassAssociationSN, ClassAssociationLN,
		ClassSAPAssignment, ClassImageTransfer, ClassActivityCalendar, ClassRegisterMonitor,
		ClassSingleActionSch, ClassIECLocalPortSetup, ClassIECTwistedPairSetup,
		ClassMBusSlavePortSetup, ClassUtilityTables, ClassModemConfiguration,
		ClassAutoAnswer, ClassAutoConnect, ClassPushSetup, ClassTCPUDPSetup,
		ClassIPTwistedPairSetup, ClassPPPSetup, ClassGPRSModemSetup, ClassSMTP,
		ClassGSMDiagnostic, ClassIPv4Setup,
	}
	for _, id := range ids {
		name := ClassName(id)
		if name == "" || strings.Contains(name, "未知") {
			t.Errorf("类 %d 没有可读名：%q", id, name)
		}
		if !strings.Contains(name, itoa(int(id))) {
			t.Errorf("类 %d 的名字 %q 里没有类号", id, name)
		}
	}
	// 类号必须两两不同，否则表里会缺条目而看不出来。
	seen := map[uint16]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("类号 %d 重复定义", id)
		}
		seen[id] = true
	}
}

// 单位表逐个扫一遍：已定义的必须返回非空、未定义的必须给 `unit(N)`。
// 这里不锁具体字符串（标准 Annex A 有上百项），只锁"不会返回空"这一条——
// 空单位会让自检表格少一列，而少一列比给错单位安全。
func TestUnitNameAll(t *testing.T) {
	if got := UnitName(0); got != "" {
		t.Errorf("单位 0 应为空，得到 %q", got)
	}
	for u := 1; u <= 254; u++ {
		got := UnitName(u)
		if got == "" {
			t.Errorf("单位 %d 返回空串", u)
		}
		if strings.HasPrefix(got, "unit(") && !strings.Contains(got, itoa(u)) {
			t.Errorf("未定义单位 %d 的兜底写法 %q 里没有数值", u, got)
		}
	}
}

// CommonObjects 必须返回副本：CLI 的 -list 若拿到共享切片，某处排序会
// 悄悄改掉全局表。
func TestCommonObjectsIsCopy(t *testing.T) {
	a := CommonObjects()
	if len(a) == 0 {
		t.Fatal("内置表不应为空")
	}
	a[0].Name = "被改掉了"
	b := CommonObjects()
	if b[0].Name == "被改掉了" {
		t.Error("CommonObjects 返回的是共享切片，不是副本")
	}
	// 每一条都要能被 LookupOBIS 查回来（含 `-` 与 `.` 两种写法）。
	for _, o := range b {
		if _, ok := LookupOBIS(o.OBIS); !ok {
			t.Errorf("%s 查不回来", o.OBIS)
		}
		dash := strings.ReplaceAll(o.OBIS, ".", "-")
		if got, ok := LookupOBIS(dash); !ok || got.OBIS != o.OBIS {
			t.Errorf("连字符写法 %s 查不回来", dash)
		}
	}
}

// 被拒绝的 AARE 也要能解析出 result 与 diagnostic：现场密码错、权限不够
// 时看到的就是这一段。
func TestParseAssociateResponseRejected(t *testing.T) {
	// 61 xx A1 09 <OID> A2 03 02 01 01 A3 05 A1 03 02 01 0E
	// body = 11 + 5 + 7 = 23 = 0x17
	raw := mustHex(t, "6117"+
		"A109060760857405080101"+
		"A203020101"+
		"A305A10302010E")
	resp, err := ParseAssociateResponse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if resp.ApplicationOK {
		t.Error("result=1 不应算接受")
	}
	if resp.Result != 1 {
		t.Errorf("result = %d", resp.Result)
	}
	if resp.Diagnostic != 0x0E {
		t.Errorf("diagnostic = %d，应为 14", resp.Diagnostic)
	}
	if resp.HasInitiate {
		t.Error("被拒绝的 AARE 不应带 InitiateResponse")
	}
	// 标签错、长度越界的错误路径。
	if _, err := ParseAssociateResponse([]byte{TagAARQ, 0x00}); err == nil {
		t.Error("AARQ 标签不应被当成 AARE")
	}
	if _, err := ParseAssociateResponse([]byte{TagAARE, 0x10, 0x00}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("长度越界应报 ErrIncomplete，得到 %v", err)
	}
}

// AARQ 的长度上限：单字节 BER 长度装不下超长上下文时要在编码期报错，
// 而不是编出一个长度字段与实际不符的报文。
func TestAssociateRequestTooLong(t *testing.T) {
	req := AssociateRequest{
		ApplicationContext: make([]byte, 200),
		Initiate:           DefaultInitiateRequest(),
	}
	if _, err := req.Encode(); err == nil {
		t.Error("超长 AARQ 应被拒绝")
	}
}
