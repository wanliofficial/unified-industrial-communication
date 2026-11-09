package secscodec_test

import (
	"bytes"
	"math"
	"testing"

	secscodec "uic/internal/codec/secs"
)

// 这一层是「SECS-II 消息体 ↔ 平台结构」的纯函数。测试重点是三件容易
// 悄悄出错的事：
//
//  1. 编号字段的宽度容忍（U1/U2/U4/I4 现场都出现过）；
//  2. 应答体的结构校验（S1F4 与 S2F14 布局相同、语义相反，靠形状区分）；
//  3. ALCD 的 bit7 与低 7 位的拆分（把整字节当布尔会让「解除」变「发生」）。

// ---------------------------------------------------------------------------
// S1F3 / S2F13：编号列表
// ---------------------------------------------------------------------------

func TestIDListRoundTrip(t *testing.T) {
	ids := []uint32{1, 1001, 65535, 70000}
	body, err := secscodec.EncodeIDList(ids)
	if err != nil {
		t.Fatal(err)
	}
	got, err := secscodec.DecodeIDList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("编号个数 = %d, want %d", len(got), len(ids))
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("第 %d 个编号 = %d, want %d", i, got[i], ids[i])
		}
	}
}

// 空列表是合法的：S1F3 用 L,0 表示「全部 SVID」。
func TestIDListEmpty(t *testing.T) {
	body, err := secscodec.EncodeIDList(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte{0x01, 0x00}) {
		t.Fatalf("空编号列表 = % x, want 01 00", body)
	}
	got, err := secscodec.DecodeIDList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("编号个数 = %d, want 0", len(got))
	}
}

// 现场设备常用 U1/U2 编码编号，甚至用有符号 I4；解码必须都接受，
// 否则「能连上但一个值都读不到」，而错误信息只会说「格式不符」。
func TestIDListAcceptsNarrowerTypes(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want uint32
	}{
		{"U1", []byte{0x01, 0x01, 0xA5, 0x01, 0x07}, 7},
		{"U2", []byte{0x01, 0x01, 0xA9, 0x02, 0x03, 0xE8}, 1000},
		{"I4", []byte{0x01, 0x01, 0x71, 0x04, 0x00, 0x00, 0x00, 0x2A}, 42},
		{"U8", []byte{0x01, 0x01, 0xA3, 0x00, 0x00, 0x08, 0, 0, 0, 0, 0, 0, 0, 0x08}, 8},
	}
	for _, tc := range cases {
		got, err := secscodec.DecodeIDList(tc.body)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: got %v, want [%d]", tc.name, got, tc.want)
		}
	}
}

func TestIDListRejectsBadShapes(t *testing.T) {
	if _, err := secscodec.DecodeIDList(nil); err == nil {
		t.Fatal("空消息体必须失败")
	}
	// 顶层不是 L：U4 1
	if _, err := secscodec.DecodeIDList([]byte{0xB1, 0x04, 0, 0, 0, 1}); err == nil {
		t.Fatal("顶层不是 L 必须失败")
	}
	// 编号是 A 串
	if _, err := secscodec.DecodeIDList([]byte{0x01, 0x01, 0x41, 0x01, 'x'}); err == nil {
		t.Fatal("字符串编号必须失败")
	}
	// 负数编号
	if _, err := secscodec.DecodeIDList([]byte{0x01, 0x01, 0x69, 0x02, 0xFF, 0xFF}); err == nil {
		t.Fatal("负数编号必须失败")
	}
	// 超出 32 位的 U8
	big := []byte{0x01, 0x01, 0xA3, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}
	if _, err := secscodec.DecodeIDList(big); err == nil {
		t.Fatal("超出 32 位的编号必须失败")
	}
	// 消息体尾部有多余字节
	body, _ := secscodec.EncodeIDList([]uint32{1})
	if _, err := secscodec.DecodeIDList(append(body, 0xA5, 0x01, 0x01)); err == nil {
		t.Fatal("尾部多余字节必须失败")
	}
}

// ---------------------------------------------------------------------------
// S1F4 / S2F14：编号 + 取值
// ---------------------------------------------------------------------------

func TestStatusReplyRoundTrip(t *testing.T) {
	vals := []secscodec.StatusValue{
		{ID: 1, Value: secscodec.Int(secscodec.FormatU4, 1234)},
		{ID: 2, Value: secscodec.ASCII("RUN")},
		{ID: 3, Value: secscodec.Float(secscodec.FormatF4, 21.5)},
		{ID: 4, Value: secscodec.Float(secscodec.FormatF8, 1e-9)},
		{ID: 5, Value: secscodec.Binary([]byte{0x01, 0x02})},
		{ID: 6, Value: secscodec.Bool(true)},
	}
	body, err := secscodec.EncodeStatusReply(vals)
	if err != nil {
		t.Fatal(err)
	}
	got, err := secscodec.DecodeStatusReply(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(vals) {
		t.Fatalf("项数 = %d, want %d", len(got), len(vals))
	}
	for i := range vals {
		if got[i].ID != vals[i].ID {
			t.Fatalf("第 %d 项编号 = %d, want %d", i, got[i].ID, vals[i].ID)
		}
		if got[i].Value.Format != vals[i].Value.Format {
			t.Fatalf("第 %d 项格式 = %d, want %d", i, got[i].Value.Format, vals[i].Value.Format)
		}
	}
	// 抽查具体值：字符串与浮点最容易在「值被当成字节」时悄悄变成空。
	if s, err := got[1].Value.String(); err != nil || s != "RUN" {
		t.Fatalf("第 2 项 = %q err=%v", s, err)
	}
	if f, err := got[2].Value.F4(); err != nil || math.Abs(float64(f-21.5)) > 1e-6 {
		t.Fatalf("第 3 项 = %v err=%v", f, err)
	}
}

func TestStatusReplyGolden(t *testing.T) {
	// L1 { L2 { U4 1, U1 7 } }
	body, err := secscodec.EncodeStatusReply([]secscodec.StatusValue{
		{ID: 1, Value: secscodec.Int(secscodec.FormatU1, 7)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x01, 0x01, // 外层 L，1 项
		0x01, 0x02, // 内层 L，2 项
		0xB1, 0x04, 0x00, 0x00, 0x00, 0x01, // U4 1
		0xA5, 0x01, 0x07, // U1 7
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("S1F4 = % x, want % x", body, want)
	}
}

func TestStatusReplyNilValue(t *testing.T) {
	if _, err := secscodec.EncodeStatusReply([]secscodec.StatusValue{{ID: 1}}); err == nil {
		t.Fatal("没有值必须失败")
	}
}

func TestStatusReplyRejectsBadShape(t *testing.T) {
	// 内层 L 只有 1 项（应为 L,2）
	body := []byte{0x01, 0x01, 0x01, 0x01, 0xB1, 0x04, 0x00, 0x00, 0x00, 0x01}
	if _, err := secscodec.DecodeStatusReply(body); err == nil {
		t.Fatal("L,1 必须失败")
	}
	// 内层 L 有 3 项
	body = []byte{0x01, 0x01, 0x01, 0x03, 0xB1, 0x04, 0, 0, 0, 1, 0xA5, 0x01, 0x07, 0xA5, 0x01, 0x08}
	if _, err := secscodec.DecodeStatusReply(body); err == nil {
		t.Fatal("L,3 也必须失败（S1F4 只定义了 L,2）")
	}
	// 内层不是 L
	if _, err := secscodec.DecodeStatusReply([]byte{0x01, 0x01, 0xA5, 0x01, 0x01}); err == nil {
		t.Fatal("内层不是 L 必须失败")
	}
}

// ---------------------------------------------------------------------------
// S1F12：状态变量名
// ---------------------------------------------------------------------------

func TestStatusNameReplyRoundTrip(t *testing.T) {
	names := []secscodec.StatusVariable{
		{ID: 1, Name: "Temperature", Units: "C"},
		{ID: 2, Name: "Pressure", Units: "Pa"},
		{ID: 3, Name: "Mode", Units: ""},
	}
	body, err := secscodec.EncodeStatusNameReply(names)
	if err != nil {
		t.Fatal(err)
	}
	got, err := secscodec.DecodeStatusNameReply(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(names) {
		t.Fatalf("项数 = %d, want %d", len(got), len(names))
	}
	for i := range names {
		if got[i].ID != names[i].ID || got[i].Name != names[i].Name || got[i].Units != names[i].Units {
			t.Fatalf("第 %d 项 = %+v, want %+v", i, got[i], names[i])
		}
	}
}

// 部分设备的 S1F12 每项是 L,4（多带上下限）。多出来的一项要忽略而不是报错，
// 否则整张点表都读不到名字。
func TestStatusNameReplyToleratesFourthChild(t *testing.T) {
	body := []byte{
		0x01, 0x01, // L，1 项
		0x01, 0x04, // L，4 项
		0xB1, 0x04, 0, 0, 0, 1, // U4 1
		0x41, 0x04, 'N', 'a', 'm', 'e', // A "Name"
		0x41, 0x01, 'V', // A "V"
		0x41, 0x03, '0', '1', '0', // 多出来的第 4 项
	}
	got, err := secscodec.DecodeStatusNameReply(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Name" || got[0].Units != "V" {
		t.Fatalf("got %+v", got)
	}
}

func TestStatusNameReplyRejectsBadShape(t *testing.T) {
	// L,2（少了单位）
	body := []byte{0x01, 0x01, 0x01, 0x02, 0xB1, 0x04, 0, 0, 0, 1, 0x41, 0x01, 'N'}
	if _, err := secscodec.DecodeStatusNameReply(body); err == nil {
		t.Fatal("L,2 必须失败")
	}
	// L,5（多两项，超出容忍范围）
	body = []byte{0x01, 0x01, 0x01, 0x05,
		0xB1, 0x04, 0, 0, 0, 1,
		0x41, 0x01, 'N', 0x41, 0x01, 'V',
		0x41, 0x01, 'a', 0x41, 0x01, 'b'}
	if _, err := secscodec.DecodeStatusNameReply(body); err == nil {
		t.Fatal("L,5 必须失败")
	}
}

// ---------------------------------------------------------------------------
// S2F15 / S2F16
// ---------------------------------------------------------------------------

func TestConstantSendAndAck(t *testing.T) {
	cs := []secscodec.Constant{
		{ID: 10, Value: secscodec.Int(secscodec.FormatU4, 300)},
		{ID: 11, Value: secscodec.ASCII("AUTO")},
	}
	body, err := secscodec.EncodeConstantSend(cs)
	if err != nil {
		t.Fatal(err)
	}
	// S2F15 与 S1F4 布局相同，必须能被 DecodeStatusReply 解回
	vals, err := secscodec.DecodeStatusReply(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 || vals[0].ID != 10 || vals[1].ID != 11 {
		t.Fatalf("S2F15 往返 = %+v", vals)
	}
	if _, err := secscodec.EncodeConstantSend([]secscodec.Constant{{ID: 1}}); err == nil {
		t.Fatal("没有值必须失败")
	}
}

func TestAckRoundTrip(t *testing.T) {
	for _, ok := range []bool{true, false} {
		body, err := secscodec.EncodeConstantAck(ok)
		if err != nil {
			t.Fatal(err)
		}
		code, err := secscodec.DecodeAck(body)
		if err != nil {
			t.Fatal(err)
		}
		if (code == 0) != ok {
			t.Fatalf("ok=%v 时应答码 = %d", ok, code)
		}
	}
	if _, err := secscodec.DecodeAck([]byte{0x41, 0x01, 'x'}); err == nil {
		t.Fatal("A 串应答必须失败")
	}
	if _, err := secscodec.DecodeAck([]byte{0x21, 0x02, 0x00, 0x01}); err == nil {
		t.Fatal("2 字节 B 必须失败")
	}
	if _, err := secscodec.DecodeAck(nil); err == nil {
		t.Fatal("空应答必须失败")
	}
}

// ---------------------------------------------------------------------------
// S1F13 / S1F14
// ---------------------------------------------------------------------------

func TestCommEstablished(t *testing.T) {
	req, err := secscodec.EncodeEmptyList()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req, []byte{0x01, 0x00}) {
		t.Fatalf("空 L = % x", req)
	}
	for _, ok := range []bool{true, false} {
		body, err := secscodec.EncodeCommAck(ok)
		if err != nil {
			t.Fatal(err)
		}
		got, err := secscodec.DecodeCommAck(body)
		if err != nil {
			t.Fatal(err)
		}
		if got != ok {
			t.Fatalf("ok=%v 时解回 %v", ok, got)
		}
	}
	// 常见设备的 S1F14 第二项是 L,0，解码时只看第一项。
	if _, err := secscodec.DecodeCommAck([]byte{0x01, 0x02, 0x21, 0x01, 0x00, 0x01, 0x00}); err != nil {
		t.Fatalf("标准 S1F14 应能解: %v", err)
	}
	if _, err := secscodec.DecodeCommAck([]byte{0x01, 0x00}); err == nil {
		t.Fatal("L,0 必须失败")
	}
	// COMMACK 宽度不对
	if _, err := secscodec.DecodeCommAck([]byte{0x01, 0x01, 0x21, 0x02, 0x00, 0x00}); err == nil {
		t.Fatal("2 字节 COMMACK 必须失败")
	}
}

// ---------------------------------------------------------------------------
// S1F2
// ---------------------------------------------------------------------------

func TestOnLineData(t *testing.T) {
	body, err := secscodec.EncodeOnLineData("MODEL-01", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	mdln, rev, err := secscodec.DecodeOnLineData(body)
	if err != nil {
		t.Fatal(err)
	}
	if mdln != "MODEL-01" || rev != "1.2.3" {
		t.Fatalf("got %q %q", mdln, rev)
	}
	if _, _, err := secscodec.DecodeOnLineData([]byte{0x01, 0x01, 0x41, 0x01, 'x'}); err == nil {
		t.Fatal("L,1 必须失败")
	}
	if _, _, err := secscodec.DecodeOnLineData(nil); err == nil {
		t.Fatal("空体必须失败")
	}
}

// ---------------------------------------------------------------------------
// S6F11 事件报告
// ---------------------------------------------------------------------------

func TestEventReportRoundTrip(t *testing.T) {
	ev := secscodec.Event{
		DataID: 100,
		CEID:   200,
		Reports: []secscodec.Report{
			{ReportID: 1, Values: []secscodec.StatusValue{
				{ID: 11, Value: secscodec.Int(secscodec.FormatU4, 25)},
				{ID: 12, Value: secscodec.ASCII("RUN")},
			}},
			{ReportID: 2, Values: []secscodec.StatusValue{
				{ID: 13, Value: secscodec.Float(secscodec.FormatF8, 1.25)},
			}},
		},
	}
	body, err := secscodec.EncodeEventReport(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := secscodec.DecodeEventReport(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.DataID != 100 || got.CEID != 200 {
		t.Fatalf("头 = DATAID %d CEID %d", got.DataID, got.CEID)
	}
	if len(got.Reports) != 2 {
		t.Fatalf("报告份数 = %d, want 2", len(got.Reports))
	}
	if got.Reports[1].ReportID != 2 || len(got.Reports[1].Values) != 1 {
		t.Fatalf("第 2 份报告 = %+v", got.Reports[1])
	}
	// 两份报告必须各自独立：拍平成一张表会让同名 VID 互相覆盖。
	if len(got.Reports[0].Values) != 2 || got.Reports[0].Values[1].ID != 12 {
		t.Fatalf("第 1 份报告 = %+v", got.Reports[0])
	}
}

func TestEventLookupVID(t *testing.T) {
	ev := secscodec.Event{
		DataID: 1, CEID: 2,
		Reports: []secscodec.Report{
			{ReportID: 1, Values: []secscodec.StatusValue{
				{ID: 11, Value: secscodec.Int(secscodec.FormatU4, 1)},
			}},
			{ReportID: 2, Values: []secscodec.StatusValue{
				{ID: 11, Value: secscodec.Int(secscodec.FormatU4, 9)},
			}},
		},
	}
	v, ok := ev.LookupVID(11)
	if !ok {
		t.Fatal("应找到 VID 11")
	}
	n, err := v.U4()
	if err != nil || n != 9 {
		t.Fatalf("同名 VID 应取最后一份（%d err=%v）", n, err)
	}
	if _, ok := ev.LookupVID(99); ok {
		t.Fatal("不存在的 VID 必须返回 false")
	}
}

func TestEventReportRejectsBadShape(t *testing.T) {
	// L,2（少了报告列表）
	if _, err := secscodec.DecodeEventReport([]byte{
		0x01, 0x02, 0xB1, 0x04, 0, 0, 0, 1, 0xB1, 0x04, 0, 0, 0, 2,
	}); err == nil {
		t.Fatal("L,2 必须失败")
	}
	// 报告列表不是 L
	if _, err := secscodec.DecodeEventReport([]byte{
		0x01, 0x03,
		0xB1, 0x04, 0, 0, 0, 1,
		0xB1, 0x04, 0, 0, 0, 2,
		0xA5, 0x01, 0x01,
	}); err == nil {
		t.Fatal("报告列表不是 L 必须失败")
	}
	// 一份报告是 L,1（少了取值列表）
	if _, err := secscodec.DecodeEventReport([]byte{
		0x01, 0x03,
		0xB1, 0x04, 0, 0, 0, 1,
		0xB1, 0x04, 0, 0, 0, 2,
		0x01, 0x01, 0x01, 0x01, 0xB1, 0x04, 0, 0, 0, 1,
	}); err == nil {
		t.Fatal("报告不是 L,2 必须失败")
	}
	// 取值不是 L
	if _, err := secscodec.DecodeEventReport([]byte{
		0x01, 0x03,
		0xB1, 0x04, 0, 0, 0, 1,
		0xB1, 0x04, 0, 0, 0, 2,
		0x01, 0x01, 0x01, 0x02, 0xB1, 0x04, 0, 0, 0, 1, 0xA5, 0x01, 0x01,
	}); err == nil {
		t.Fatal("取值列表不是 L 必须失败")
	}
	// DATAID 是 A 串
	if _, err := secscodec.DecodeEventReport([]byte{
		0x01, 0x03,
		0x41, 0x01, 'x',
		0xB1, 0x04, 0, 0, 0, 2,
		0x01, 0x00,
	}); err == nil {
		t.Fatal("DATAID 是字符串必须失败")
	}
}

func TestEncodeEventReportNilValue(t *testing.T) {
	_, err := secscodec.EncodeEventReport(secscodec.Event{
		Reports: []secscodec.Report{{ReportID: 1, Values: []secscodec.StatusValue{{ID: 1}}}},
	})
	if err == nil {
		t.Fatal("没有值必须失败")
	}
}

// ---------------------------------------------------------------------------
// S5F1 报警报告：ALCD 的 bit7 与低 7 位
// ---------------------------------------------------------------------------

func TestAlarmReportSetAndClear(t *testing.T) {
	// bit7 置位 = 报警发生，低 7 位 = 类别 5
	set := []byte{
		0x01, 0x03,
		0x21, 0x01, 0x85,
		0xB1, 0x04, 0x00, 0x00, 0x03, 0xE8, // ALID 1000
		0x41, 0x04, 'T', 'E', 'M', 'P',
	}
	a, err := secscodec.DecodeAlarmReport(set)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsSet {
		t.Fatal("bit7 置位时 IsSet 必须为 true")
	}
	if a.Kind != 5 {
		t.Fatalf("类别 = %d, want 5", a.Kind)
	}
	if a.ALCD != 0x85 || a.ALID != 1000 || a.ALTX != "TEMP" {
		t.Fatalf("报警 = %+v", a)
	}

	// bit7 清零 = 解除，但低 7 位仍是类别 5。把整字节当布尔会读成「发生」。
	clear := []byte{
		0x01, 0x03,
		0x21, 0x01, 0x05,
		0xB1, 0x04, 0x00, 0x00, 0x03, 0xE8,
		0x41, 0x04, 'T', 'E', 'M', 'P',
	}
	a, err = secscodec.DecodeAlarmReport(clear)
	if err != nil {
		t.Fatal(err)
	}
	if a.IsSet {
		t.Fatal("bit7 清零时 IsSet 必须为 false")
	}
	if a.Kind != 5 {
		t.Fatalf("解除时的类别 = %d, want 5", a.Kind)
	}
}

func TestAlarmReportRoundTrip(t *testing.T) {
	for _, src := range []secscodec.Alarm{
		{IsSet: true, Kind: 3, ALID: 7, ALTX: "OVEN HOT"},
		{IsSet: false, Kind: 3, ALID: 7, ALTX: "OVEN COOL"},
	} {
		body, err := secscodec.EncodeAlarmReport(src)
		if err != nil {
			t.Fatal(err)
		}
		got, err := secscodec.DecodeAlarmReport(body)
		if err != nil {
			t.Fatal(err)
		}
		if got.IsSet != src.IsSet || got.Kind != src.Kind || got.ALID != src.ALID || got.ALTX != src.ALTX {
			t.Fatalf("got %+v, want %+v", got, src)
		}
	}
}

func TestAlarmReportRejectsBadShape(t *testing.T) {
	// ALCD 宽度不对
	if _, err := secscodec.DecodeAlarmReport([]byte{
		0x01, 0x02, 0x21, 0x02, 0x00, 0x01, 0xB1, 0x04, 0, 0, 0, 1,
	}); err == nil {
		t.Fatal("2 字节 ALCD 必须失败")
	}
	// 只有一项
	if _, err := secscodec.DecodeAlarmReport([]byte{0x01, 0x01, 0x21, 0x01, 0x80}); err == nil {
		t.Fatal("L,1 必须失败")
	}
	// 没有 ALTX 也应能解（部分设备只带两项）
	a, err := secscodec.DecodeAlarmReport([]byte{
		0x01, 0x02, 0x21, 0x01, 0x80, 0xB1, 0x04, 0, 0, 0, 1,
	})
	if err != nil {
		t.Fatalf("L,2 应能解: %v", err)
	}
	if !a.IsSet || a.Kind != 0 || a.ALTX != "" {
		t.Fatalf("报警 = %+v", a)
	}
}

// ---------------------------------------------------------------------------
// SF 常量与内部工具
// ---------------------------------------------------------------------------

func TestSFString(t *testing.T) {
	if got := secscodec.SFEventReport.String(); got != "S6F11" {
		t.Fatalf("SF = %q", got)
	}
	if got := (secscodec.SF{Stream: 1, Function: 2}).String(); got != "S1F2" {
		t.Fatalf("SF = %q", got)
	}
}

// decodeSingle 要求「正好一个顶层项」——多出来的字节说明对端布局搞错了，
// 按第一项硬解会把真正的字段读成别的。
func TestDecodeSingleTrailingBytes(t *testing.T) {
	body, _ := secscodec.EncodeEmptyList()
	_, err := secscodec.DecodeStatusReply(append(body, 0x01, 0x00))
	if err == nil {
		t.Fatal("尾部多余项必须失败")
	}
	if _, err := secscodec.DecodeStatusReply([]byte{}); err == nil {
		t.Fatal("空体必须失败")
	}
}
