package iec104

import (
	"bytes"
	"testing"

	"uic/internal/codec/iec60870"
)

// ---------------------------------------------------------------------------
// APCI 帧
// ---------------------------------------------------------------------------

func TestBuildAndParseIFrame(t *testing.T) {
	asdu := []byte{0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x14}
	frame, err := BuildIFrame(0, 0, asdu)
	if err != nil {
		t.Fatal(err)
	}
	// 0x68 长度=4+7 控制域四字节全零 ASDU
	want := append([]byte{0x68, 11, 0x00, 0x00, 0x00, 0x00}, asdu...)
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = % X, want % X", frame, want)
	}
	got, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindI || got.SendSeq != 0 || got.RecvSeq != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	if !bytes.Equal(got.ASDU, asdu) {
		t.Fatalf("asdu = % X", got.ASDU)
	}
}

func TestIFrameSequenceBitLayout(t *testing.T) {
	// N(S)=0x1234 时，控制域低 16 位应是 0x2468（左移一位）。
	frame, err := BuildIFrame(0x1234, 0x0ABC, []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	if frame[2] != 0x68 || frame[3] != 0x24 {
		t.Fatalf("send seq bytes = %02X %02X", frame[2], frame[3])
	}
	// N(R)=0x0ABC -> 0x1578
	if frame[4] != 0x78 || frame[5] != 0x15 {
		t.Fatalf("recv seq bytes = %02X %02X", frame[4], frame[5])
	}
	got, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.SendSeq != 0x1234 || got.RecvSeq != 0x0ABC {
		t.Fatalf("parsed seq = %d/%d", got.SendSeq, got.RecvSeq)
	}
}

func TestSequenceRangeChecked(t *testing.T) {
	if _, err := BuildIFrame(0x8000, 0, []byte{0x01}); err == nil {
		t.Fatal("send seq 0x8000 must be rejected")
	}
	if _, err := BuildIFrame(0, 0x8000, []byte{0x01}); err == nil {
		t.Fatal("recv seq 0x8000 must be rejected")
	}
	if _, err := BuildIFrame(0, 0, nil); err == nil {
		t.Fatal("I frame without ASDU must be rejected")
	}
	big := make([]byte, iec60870.MaxASDU+1)
	if _, err := BuildIFrame(0, 0, big); err == nil {
		t.Fatal("oversized ASDU must be rejected")
	}
	if _, err := BuildSFrame(0x8000); err == nil {
		t.Fatal("S frame seq 0x8000 must be rejected")
	}
}

func TestBuildAndParseSFrame(t *testing.T) {
	frame, err := BuildSFrame(5)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x68, 0x04, 0x01, 0x00, 0x0A, 0x00}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = % X", frame)
	}
	got, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindS || got.RecvSeq != 5 {
		t.Fatalf("parsed = %+v", got)
	}
}

func TestBuildAndParseUFrame(t *testing.T) {
	for _, fn := range []byte{UStartAct, UStartCon, UStopAct, UStopCon, UTestAct, UTestCon} {
		frame, err := BuildUFrame(fn)
		if err != nil {
			t.Fatalf("%s: %v", UName(fn), err)
		}
		got, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("%s: %v", UName(fn), err)
		}
		if got.Kind != KindU || got.UFunc != fn {
			t.Fatalf("%s parsed = %+v", UName(fn), got)
		}
	}
	if _, err := BuildUFrame(0x03); err == nil {
		t.Fatal("bare 0x03 is not a valid U function")
	}
}

func TestParseFrameRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"short":              {0x68},
		"bad start":          {0x67, 0x04, 0x01, 0x00, 0x00, 0x00},
		"length below 4":     {0x68, 0x03, 0x01, 0x00, 0x00, 0x00},
		"length above max":   {0x68, 0xFF, 0x01, 0x00, 0x00, 0x00},
		"length mismatch":    {0x68, 0x05, 0x01, 0x00, 0x00, 0x00},
		"unknown ctrl":       {0x68, 0x04, 0x02, 0x00, 0x00, 0x00},
		"U with extra bytes": {0x68, 0x04, 0x43, 0x00, 0x00, 0x01},
	}
	for name, b := range cases {
		if _, err := ParseFrame(b); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

// S 帧必须是 ctrl[0]==0x01 且 ctrl[1]==0；把 ctrl 高位置 1 会变成 I 帧。
func TestSFrameDiscrimination(t *testing.T) {
	f, err := ParseFrame([]byte{0x68, 0x04, 0x01, 0x00, 0x02, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindS || f.RecvSeq != 1 {
		t.Fatalf("parsed = %+v", f)
	}
	// ctrl[0]=0x03 -> U 帧
	f, err = ParseFrame([]byte{0x68, 0x04, 0x03, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindU || f.UFunc != 0x03 {
		t.Fatalf("parsed = %+v", f)
	}
	if kind := KindU.String(); kind != "U" {
		t.Fatalf("KindU.String = %q", kind)
	}
	if KindI.String() != "I" || KindS.String() != "S" || Kind(9).String() != "?" {
		t.Fatal("Kind.String coverage")
	}
}
