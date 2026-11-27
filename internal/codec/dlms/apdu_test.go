package dlms

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

// AARQ 的逐字节金向量（IEC 62056-53 Annex C.3，LN 引用、无安全）。
//
// 注意实现细节：标准示例的 conformance 是 0x00 7E 1F。本测试直接用这个值
// 构造请求，验证「位表 + 组帧 + 三层长度」全都对得上标准逐字节输出。
func TestAARQGolden(t *testing.T) {
	want := mustHex(t, "601DA109060760857405080101"+"BE10040E01000000065F1F0400007E1F04B0")
	req := AssociateRequest{
		Initiate: InitiateRequest{
			DLMSVersion:   6,
			Conformance:   Conformance(0x007E1F),
			MaxReceivePDU: 0x04B0,
		},
	}
	got, err := req.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("AARQ =\n % X\nwant\n % X", got, want)
	}
}

// AARE 的逐字节金向量（Annex C.8，LN 引用、无安全、接受）。
func TestAAREGolden(t *testing.T) {
	raw := mustHex(t, "6129A10906076085740508010"+"1A203020100A305A103020100"+
		"BE10040E0800065F1F040000501F01F40007")
	resp, err := ParseAssociateResponse(raw)
	if err != nil {
		t.Fatalf("ParseAssociateResponse: %v", err)
	}
	if !resp.ApplicationOK {
		t.Fatal("应被接受")
	}
	if !resp.HasInitiate {
		t.Fatal("缺少 InitiateResponse")
	}
	if resp.Initiate.DLMSVersion != 6 {
		t.Fatalf("DLMS 版本 = %d", resp.Initiate.DLMSVersion)
	}
	if resp.Initiate.MaxReceivePDU != 0x01F4 {
		t.Fatalf("max PDU = %d", resp.Initiate.MaxReceivePDU)
	}
	if resp.Initiate.VAAName != 0x0007 {
		t.Fatalf("VAA = %04X", resp.Initiate.VAAName)
	}
	// AARE 协商出的是 0x00 50 1F（不是 AARQ 请求的 0x00 7E 1F）：
	//   bit9 priority-mgmt、bit11 block-transfer-get、bit19 get、bit20 set、
	//   bit21 selective-access、bit22 event-notification、bit23 action。
	// 服务端**砍掉了** attribute0-get（bit10）与 block-transfer-set/action
	// （bit12/13）——这正是协商的意义：客户端可以全要，服务端按自己支持的
	// 回一个子集。驱动必须按应答里的位判断能力，不能按自己请求的位。
	if !resp.Initiate.Conformance.Has(ConfGet) {
		t.Error("conformance 缺 get")
	}
	if !resp.Initiate.Conformance.Has(ConfPriorityMgmt) {
		t.Error("conformance 缺 priority-mgmt")
	}
	if resp.Initiate.Conformance.Has(ConfAttribute0Get) {
		t.Error("标准示例里服务端未协商 attribute0-get")
	}
	if resp.Initiate.Conformance.Has(ConfRead) {
		t.Error("LN 引用的 conformance 不应含 SN 的 read 位")
	}
}

// Conformance 的 3 字节编码：标准 Annex C.1 的 0x00 7E 1F 位表。
func TestConformanceBits(t *testing.T) {
	c := Conformance(0x007E1F)
	for _, bit := range []Conformance{
		ConfPriorityMgmt, ConfAttribute0Get, ConfBlockTransferGet,
		ConfBlockTransferSet, ConfBlockTransferAction, ConfMultipleReferences,
		ConfGet, ConfSet, ConfSelectiveAccess, ConfEventNotification, ConfAction,
	} {
		if !c.Has(bit) {
			t.Errorf("0x007E1F 应含 %v", bit)
		}
	}
	for _, bit := range []Conformance{
		ConfRead, ConfWrite, ConfUnconfirmedWrite, ConfAttribute0Set,
		ConfInformationReport, ConfParameterizedAccess,
	} {
		if c.Has(bit) {
			t.Errorf("0x007E1F 不应含 %v", bit)
		}
	}
	if got := c.Bytes(); !bytes.Equal(got, []byte{0x00, 0x7E, 0x1F}) {
		t.Fatalf("Bytes = % X", got)
	}
}

// Get-Request-Normal 的金字节：读 1.0.1.8.0.255（class 3、attribute 2）。
func TestGetRequestGolden(t *testing.T) {
	req := GetRequest{Attribute: AttributeDescriptor{
		ClassID:    3,
		InstanceID: [6]byte{0x01, 0x00, 0x01, 0x08, 0x00, 0xFF},
		Attribute:  2,
	}}
	// C0 01 81 + 描述符（class 2B + OBIS 6B + attr 1B），无长度字节。
	want := mustHex(t, "C0018100030100010800FF02")
	if got := req.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("Get-Request = % X, want % X", got, want)
	}
}

// Get-Response-Normal 成功：`C4 01 81 00 <A-XDR 值>`。
// 用 long-unsigned 12345 做值，验证「无长度字节 + 大端」。
func TestGetResponseRoundTrip(t *testing.T) {
	apdu, err := EncodeGetResponseNormal(InvokeID, U32(123456))
	if err == nil {
		want := mustHex(t, "C40181"+"00"+"060001E240")
		if !bytes.Equal(apdu, want) {
			t.Fatalf("Get-Response = % X, want % X", apdu, want)
		}
	}
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	resp, err := ParseGetResponse(apdu)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if resp.Kind != 1 || !resp.HasData || resp.IsResult {
		t.Fatalf("resp = %+v", resp)
	}
	n, ok := resp.Data.Number()
	if !ok || n != 123456 {
		t.Fatalf("值 = %v", n)
	}
}

// Get-Response-Normal 失败：Data-Access-Result = object-undefined(4)。
func TestGetResponseError(t *testing.T) {
	apdu := EncodeGetResponseResult(InvokeID, DARObjectUndefined)
	if !bytes.Equal(apdu, mustHex(t, "C4018101"+"04")) {
		t.Fatalf("Get-Response 错误帧 = % X", apdu)
	}
	resp, err := ParseGetResponse(apdu)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !resp.IsResult || resp.HasData {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Result != DARObjectUndefined {
		t.Fatalf("result = %d", resp.Result)
	}
	if resp.Result.String() == "" {
		t.Fatal("结果码缺中文说明")
	}
}

// A-XDR 的每种标量都要能往返，且标签值必须与标准一致。
func TestAXDRScalars(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		hex  string
	}{
		{"null", Value{Tag: TagNull}, "00"},
		{"bool-true", Bool(true), "0301"},
		{"bool-false", Bool(false), "0300"},
		{"int8", I8(-5), "0FFB"},
		{"int16", I16(300), "10012C"},
		{"int32", I32(-70000), "05FFFEEE90"},
		{"uint8", U8(200), "11C8"},
		{"uint16", U16(65535), "12FFFF"},
		{"uint32", U32(1000000), "06000F4240"},
		{"enum", Enum(3), "1603"},
		{"visible", Text("AB"), "0A024142"},
		{"octets", Octets([]byte{0xDE, 0xAD}), "0902DEAD"},
	}
	for _, c := range cases {
		enc, err := c.v.Encode()
		if err != nil {
			t.Fatalf("%s: Encode: %v", c.name, err)
		}
		want := mustHex(t, c.hex)
		if !bytes.Equal(enc, want) {
			t.Errorf("%s: 编码 = % X, want % X", c.name, enc, want)
		}
		dec, used, err := Decode(enc)
		if err != nil {
			t.Fatalf("%s: Decode: %v", c.name, err)
		}
		if used != len(enc) {
			t.Errorf("%s: consumed %d != %d", c.name, used, len(enc))
		}
		// 再编一次必须一致（标签可能被规范化，但字节必须稳定）。
		again, _ := dec.Encode()
		if !bytes.Equal(again, want) {
			t.Errorf("%s: 往返 = % X, want % X", c.name, again, want)
		}
	}
}

// scaler_unit 是 A-XDR 里最常见的结构体：
// `{ integer scaler, enum unit }`，例如 `02 02 0F FE 16 1E`
// = structure(2) { integer(-2), enum(30) }。
func TestAXDRStructure(t *testing.T) {
	raw := mustHex(t, "02020FFE161E")
	v, used, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if used != len(raw) {
		t.Fatalf("consumed %d != %d", used, len(raw))
	}
	if len(v.Elems) != 2 {
		t.Fatalf("元素数 = %d", len(v.Elems))
	}
	if v.Elems[0].Int != -2 {
		t.Errorf("scaler = %d, want -2", v.Elems[0].Int)
	}
	if v.Elems[1].Int != 30 {
		t.Errorf("unit = %d, want 30", v.Elems[1].Int)
	}
	if got := v.String(); got != "{-2, 30}" {
		t.Errorf("String = %q", got)
	}
}

// 空号标签必须报错而不是猜成别的类型。
func TestAXDRUnknownTag(t *testing.T) {
	for _, tag := range []byte{0x07, 0x08, 0x0B, 0x0C, 0x0E, 0x1C} {
		if _, _, err := Decode([]byte{tag, 0x00}); err == nil {
			t.Errorf("标签 %02X 应被拒绝", tag)
		}
	}
}

// date_time 的 12 字节布局是「年(2) 月 日 星期 时 分 秒 百分秒(2) 偏差(2) 状态」。
// 星期夹在日和时之间——按"年 月 日 时 分 秒"的直觉写会把日当成月、星期当成
// 小时，症状是时钟解不出来或相差一个月。这个字段最容易记错，必须锁死。
func TestAXDRDateTimeLayout(t *testing.T) {
	// 2026-11-13 08:30:00，星期五，UTC。
	raw := []byte{
		0x07, 0xEA, // 2026
		0x0B,       // 月 = 11
		0x0D,       // 日 = 13
		0x05,       // 星期 = 周五
		0x08,       // 时 = 8
		0x1E,       // 分 = 30
		0x00,       // 秒 = 0
		0x00,       // 百分秒 = 0
		0x00, 0x00, // 与 UTC 偏差 = 0 分钟
		0x00, // 状态
	}
	v := Value{Tag: TagDateTime, Raw: raw}
	got, ok := v.DateTime()
	if !ok {
		t.Fatal("应能解出时间")
	}
	if got.Year() != 2026 || got.Month() != 11 || got.Day() != 13 {
		t.Errorf("日期 = %v，应为 2026-11-13", got.Format("2006-01-02"))
	}
	if got.Hour() != 8 || got.Minute() != 30 || got.Second() != 0 {
		t.Errorf("时间 = %v，应为 08:30:00", got.Format("15:04:05"))
	}
	if want := "2026-11-13T08:30:00"; v.String() != want {
		t.Errorf("String = %q，应为 %q", v.String(), want)
	}

	// 编码必须与解码对称：编出来的 12 字节要和输入一致。
	enc, err := v.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(enc, append([]byte{TagDateTime}, raw...)) {
		t.Fatalf("编码 = % X", enc)
	}

	// 时区偏差是带符号分钟数：+480 分钟 = UTC+8。
	raw8 := append([]byte(nil), raw...)
	raw8[9], raw8[10] = 0x01, 0xE0 // 480
	v8 := Value{Tag: TagDateTime, Raw: raw8}
	t8, ok := v8.DateTime()
	if !ok {
		t.Fatal("带时区偏差的时钟应能解出")
	}
	if _, off := t8.Zone(); off != 8*3600 {
		t.Errorf("时区偏移 = %d 秒，应为 28800", off)
	}

	// 0x8000 表示"未指定"，按 UTC 处理，但不能因此判失败。
	rawU := append([]byte(nil), raw...)
	rawU[9], rawU[10] = 0x80, 0x00
	if _, ok := (Value{Tag: TagDateTime, Raw: rawU}).DateTime(); !ok {
		t.Error("时区未指定不应导致整条时钟解不出来")
	}

	// 年字段"未指定"的哨兵是整段 0xFFFF，而不是任一字节 0xFF——按字节判会把
	// 2047(0x07FF) 这类低字节恰为 0xFF 的合法年份误拒。
	badYear := append([]byte(nil), raw...)
	badYear[0], badYear[1] = 0xFF, 0xFF
	if _, ok := (Value{Tag: TagDateTime, Raw: badYear}).DateTime(); ok {
		t.Error("年 0xFFFF（未指定）不应解出时间")
	}
	// 年 2047 = 0x07FF：低字节是 0xFF，但它是合法年份，必须能解出。
	if t2047, ok := (Value{Tag: TagDateTime, Raw: []byte{
		0x07, 0xFF, 0x0B, 0x0D, 0x05, 0x08, 0x1E, 0x00, 0x00, 0x00, 0x00, 0x00,
	}}).DateTime(); !ok || t2047.Year() != 2047 {
		t.Errorf("年 2047(0x07FF) 应能解出，得到 ok=%v year=%d", ok, t2047.Year())
	}
	// 月/日任一为 0xFF（未指定）就构不成时间点。
	for _, i := range []int{2, 3} {
		bad := append([]byte(nil), raw...)
		bad[i] = 0xFF
		if _, ok := (Value{Tag: TagDateTime, Raw: bad}).DateTime(); ok {
			t.Errorf("第 %d 字节为 0xFF 时不应解出时间", i+1)
		}
	}
	// 时/分/秒为 0xFF（未指定）也不能静默归一化成一个错的时间点。
	for _, i := range []int{5, 6, 7} {
		bad := append([]byte(nil), raw...)
		bad[i] = 0xFF
		if _, ok := (Value{Tag: TagDateTime, Raw: bad}).DateTime(); ok {
			t.Errorf("时间字节第 %d 位为 0xFF 时不应解出时间", i+1)
		}
	}
	// 月份越界必须判失败，不能"宽容地"返回 13 月。
	bad := append([]byte(nil), raw...)
	bad[2] = 0x0D
	if _, ok := (Value{Tag: TagDateTime, Raw: bad}).DateTime(); ok {
		t.Error("月份 13 应被拒绝")
	}
	// 长度不对直接失败。
	if _, ok := (Value{Tag: TagDateTime, Raw: raw[:11]}).DateTime(); ok {
		t.Error("11 字节不应被当成 date_time")
	}
}

// date 是 5 字节：年(2) 月 日 星期。
func TestAXDRDateLayout(t *testing.T) {
	v := Value{Tag: TagDate, Raw: []byte{0x07, 0xEA, 0x03, 0x0F, 0x07}}
	got, ok := v.DateTime()
	if !ok {
		t.Fatal("应能解出日期")
	}
	if got.Format("2006-01-02") != "2026-03-15" {
		t.Errorf("日期 = %v", got.Format("2006-01-02"))
	}
	// time 只有时分秒，构不成日期，必须明确返回 false 而不是造一个 0000 年。
	if _, ok := (Value{Tag: TagTime, Raw: []byte{8, 30, 0, 0}}).DateTime(); ok {
		t.Error("time 不应被当成完整日期")
	}
}
