package iec60870

// 本文件测试「同一份应用层、不同剖面」。远动协议的地址宽度不是协议常量，
// 而是工程参数：同一个类型标识在哪台装置上占几个字节，取决于该装置的
// 参数设置。宽度算错时帧仍然能收发，只是 ASDU 解出来全是垃圾——这类
// 错误不会以编译错误或明显的异常出现，所以必须逐宽度验证一遍。

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

// TestProfileRoundTripAllWidths 覆盖 1/2/3 字节 IOA 与 0/1 字节源发地址的
// 组合。8 种组合全部走一遍编码 → 解码，断言对象取回原值、头部长度与
// 声明一致。
func TestProfileRoundTripAllWidths(t *testing.T) {
	orig := time.Date(2026, 2, 3, 4, 5, 6, 789000000, time.Local)
	for _, orgSize := range []int{0, 1} {
		for _, caSize := range []int{1, 2} {
			for _, ioaSize := range []int{1, 2, 3} {
				p := Profile{OrgSize: orgSize, CASize: caSize, IOASize: ioaSize}
				if err := p.Validate(); err != nil {
					t.Fatalf("%+v: %v", p, err)
				}
				ca := uint16(0x7F)
				if caSize == 2 {
					ca = 0x1234
				}
				ioa := uint32(0x7F)
				if ioaSize == 2 {
					ioa = 0x1234
				}
				if ioaSize == 3 {
					ioa = 0x123456
				}
				in := ASDU{
					TypeID: TypeMeasuredFloatTime, Cause: CauseSpontaneous, CommonAddr: ca,
					Objects: []Object{{IOA: ioa, Num: 2.5, Quality: QSubstituted, Time: &orig}},
				}
				raw, err := BuildASDUProfile(in, p)
				if err != nil {
					t.Fatalf("%+v: build: %v", p, err)
				}
				out, err := ParseASDUProfile(raw, p)
				if err != nil {
					t.Fatalf("%+v: parse: %v", p, err)
				}
				if out.CommonAddr != ca || len(out.Objects) != 1 {
					t.Fatalf("%+v: header = %+v", p, out)
				}
				o := out.Objects[0]
				if o.IOA != ioa || o.Num != 2.5 || !o.Quality.Has(QSubstituted) {
					t.Fatalf("%+v: object = %+v", p, o)
				}
				if o.Time == nil || !o.Time.Equal(orig) {
					t.Fatalf("%+v: time = %v, want %v", p, o.Time, orig)
				}

				// 头部长度必须与声明一致：多一字节会让类型标识之后的所有
				// 字段整体错位。
				elem, ok := ElementSize(in.TypeID)
				if !ok {
					t.Fatalf("%+v: element size unknown for type %d", p, in.TypeID)
				}
				// 头部（含类型标识）+ 信息对象地址 + 元素。
				if got, want := len(raw), p.HeaderLen()+p.IOASize+elem; got != want {
					t.Fatalf("%+v: length = %d, want %d (head %d + ioa %d + element %d)",
						p, got, want, p.HeaderLen(), p.IOASize, elem)
				}
				// 公共地址按低字节在前写入（与信息对象地址同序），宽度即 CASize。
				caStart := 1 + 2 + orgSize
				var gotCA uint32
				switch caSize {
				case 1:
					gotCA = uint32(raw[caStart])
				case 2:
					gotCA = uint32(binary.LittleEndian.Uint16(raw[caStart:]))
				}
				if gotCA != uint32(ca) {
					t.Fatalf("%+v: CA bytes = %d, want %d", p, gotCA, ca)
				}
			}
		}
	}
}

// TestProfileRejectsOutOfRange 保证错误的剖面在编码前就被拒绝，而不是
// 静默截断。静默截断的症状是「地址看起来对、数据来自另一个点」。
func TestProfileRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		p    Profile
	}{
		{"org 2", Profile{OrgSize: 2, CASize: 1, IOASize: 2}},
		{"ca 0", Profile{OrgSize: 0, CASize: 0, IOASize: 2}},
		{"ca 3", Profile{OrgSize: 0, CASize: 3, IOASize: 2}},
		{"ioa 0", Profile{OrgSize: 0, CASize: 1, IOASize: 0}},
		{"ioa 4", Profile{OrgSize: 0, CASize: 1, IOASize: 4}},
	}
	for _, c := range cases {
		if err := c.p.Validate(); err == nil {
			t.Fatalf("%s: Validate accepted %+v", c.name, c.p)
		}
	}
}

// TestProfileRejectsTooWideValues 检查超宽地址在编码时被拒绝。
func TestProfileRejectsTooWideValues(t *testing.T) {
	one := Profile{OrgSize: 0, CASize: 1, IOASize: 1}
	if _, err := BuildASDUProfile(ASDU{
		TypeID: TypeMeasuredFloat, Cause: CauseSpontaneous, CommonAddr: 300,
		Objects: []Object{{IOA: 1, Num: 1}},
	}, one); err == nil {
		t.Fatal("CA 300 must not fit in a 1-byte common address")
	}
	if _, err := BuildASDUProfile(ASDU{
		TypeID: TypeMeasuredFloat, Cause: CauseSpontaneous, CommonAddr: 1,
		Objects: []Object{{IOA: 300, Num: 1}},
	}, one); err == nil {
		t.Fatal("IOA 300 must not fit in a 1-byte information object address")
	}
}

// TestInterrogationProfileWidths 验证按剖面构造的请求头部宽度。
//
// 总召唤是最常被写死成 104 剖面的报文：它的公共地址在 104 里固定 2 字节，
// 而 101 现场常见 1 字节。写错时从站按「公共地址不匹配」静默丢弃，主站
// 只看到超时——一条永远查不出原因的超时。
func TestInterrogationProfileWidths(t *testing.T) {
	one := Profile{OrgSize: 0, CASize: 1, IOASize: 2}
	raw, err := BuildInterrogationProfile(1, QOIStation, one)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// C_IC_NA_1 的报文没有信息对象地址：限定词紧跟在公共地址之后。
	want := []byte{TypeInterrogation, 0x01, CauseActivation, 0x01, QOIStation}
	if !bytes.Equal(raw, want) {
		t.Fatalf("bytes = % X, want % X", raw, want)
	}
	out, err := ParseASDUProfile(raw, one)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.TypeID != TypeInterrogation || out.Cause != CauseActivation || out.CommonAddr != 1 {
		t.Fatalf("asdu = %+v", out)
	}
	if len(out.Objects) != 1 || out.Objects[0].Qualifier != QOIStation {
		t.Fatalf("objects = %+v", out.Objects)
	}
}

// TestCommandProfileWidths 验证控制命令也遵守剖面：1 字节公共地址的
// 装置收到的命令必须短一个字节，否则整条 ASDU 会错位。
func TestCommandProfileWidths(t *testing.T) {
	one := Profile{OrgSize: 0, CASize: 1, IOASize: 2}
	raw, _, err := BuildCommandProfile("single-command", 4001, model.TypeBool,
		driver.WriteRequest{PointID: "cmd", Value: model.BoolValue(true)}, 1, one)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := ParseASDUProfile(raw, one)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.TypeID != TypeSingleCommand || out.Cause != CauseActivation || out.CommonAddr != 1 {
		t.Fatalf("asdu = %+v", out)
	}
	if len(out.Objects) != 1 || out.Objects[0].IOA != 4001 || !out.Objects[0].Bool {
		t.Fatalf("objects = %+v", out.Objects)
	}
}

// TestNoIOATypesGoldenBytes 把「没有信息对象地址」的四类报文钉成黄金字节。
//
// 这四类报文最容易按「每个对象都有 IOA」的通式写错：多写 1..3 个零字节，
// 外层长度字段照样自洽，只有对端会把整条 ASDU 当成未知报文丢弃。用黄金
// 字节固定下来，比任何注释都可靠。
func TestNoIOATypesGoldenBytes(t *testing.T) {
	one := Profile{OrgSize: 0, CASize: 1, IOASize: 2}
	ts := time.Date(2026, 2, 3, 4, 5, 6, 700*int(time.Millisecond), time.Local)

	cases := []struct {
		name string
		asdu ASDU
		want []byte
	}{
		{
			"总召唤（101 剖面）",
			ASDU{TypeID: TypeInterrogation, Cause: CauseActivation, CommonAddr: 1,
				Objects: []Object{{Qualifier: QOIStation}}},
			[]byte{TypeInterrogation, 0x01, CauseActivation, 0x01, QOIStation},
		},
		{
			"电能量召唤（101 剖面）",
			ASDU{TypeID: TypeCounterInterrogation, Cause: CauseActivation, CommonAddr: 1,
				Objects: []Object{{Qualifier: 5}}},
			[]byte{TypeCounterInterrogation, 0x01, CauseActivation, 0x01, 5},
		},
		{
			"初始化结束（101 剖面）",
			ASDU{TypeID: TypeEndOfInit, Cause: CauseInitialized, CommonAddr: 1,
				Objects: []Object{{Qualifier: 0}}},
			[]byte{TypeEndOfInit, 0x01, CauseInitialized, 0x01, 0},
		},
		{
			"时钟同步（101 剖面）",
			ASDU{TypeID: TypeClockSync, Cause: CauseActivation, CommonAddr: 1,
				Objects: []Object{{Time: &ts}}},
			append([]byte{TypeClockSync, 0x01, CauseActivation, 0x01}, EncodeCP56Time2a(ts)...),
		},
	}
	for _, c := range cases {
		raw, err := BuildASDUProfile(c.asdu, one)
		if err != nil {
			t.Fatalf("%s: build: %v", c.name, err)
		}
		if !bytes.Equal(raw, c.want) {
			t.Fatalf("%s: bytes = % X, want % X", c.name, raw, c.want)
		}
		back, err := ParseASDUProfile(raw, one)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		if back.TypeID != c.asdu.TypeID || back.Cause != c.asdu.Cause || len(back.Objects) != 1 {
			t.Fatalf("%s: round trip = %+v", c.name, back)
		}
	}

	// 104 剖面：ORG 1 字节、CA 2 字节小端、无 IOA。
	raw, err := BuildInterrogation(1, QOIStation)
	if err != nil {
		t.Fatal(err)
	}
	want104 := []byte{TypeInterrogation, 0x01, CauseActivation, 0x00, 0x01, 0x00, QOIStation}
	if !bytes.Equal(raw, want104) {
		t.Fatalf("104 interrogation = % X, want % X", raw, want104)
	}
}

// TestNoIOATypesRejectIOA 防止误配：这几类报文带 IOA 只会让对方整条丢弃。
func TestNoIOATypesRejectIOA(t *testing.T) {
	one := Profile{OrgSize: 0, CASize: 1, IOASize: 2}
	_, err := BuildASDUProfile(ASDU{
		TypeID: TypeInterrogation, Cause: CauseActivation, CommonAddr: 1,
		Objects: []Object{{IOA: 100, Qualifier: QOIStation}},
	}, one)
	if err == nil {
		t.Fatal("interrogation with an IOA must be rejected")
	}
}
