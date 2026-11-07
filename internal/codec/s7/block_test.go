package s7

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ListAll 的数据区是 `0A 00 0000`，不是其它查询用的 `FF 09`。
func TestBuildListBlocksGolden(t *testing.T) {
	got := BuildListBlocks(1)
	want := mustHex(t, "32070000000100080004"+
		"0001120411430100"+
		"0a000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("list blocks =\n% x\nwant\n% x", got, want)
	}
}

func TestBuildListBlocksOfTypeGolden(t *testing.T) {
	got := BuildListBlocksOfType(2, BlockDB)
	want := mustHex(t, "32070000000200080006"+
		"0001120411430200"+
		"ff0900023041")
	if !bytes.Equal(got, want) {
		t.Fatalf("list db =\n% x\nwant\n% x", got, want)
	}
}

func TestBuildBlockInfoGolden(t *testing.T) {
	got, err := BuildBlockInfo(3, BlockDB, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := mustHex(t, "3207000000030008000c"+
		"0001120411430300"+
		"ff0900083041303030303141")
	if !bytes.Equal(got, want) {
		t.Fatalf("block info =\n% x\nwant\n% x", got, want)
	}
	// 块号必须补足 5 位；99999 是上界，100000 直接拒绝。
	if _, err := BuildBlockInfo(1, BlockDB, 100000); err == nil {
		t.Fatal("block number 100000 must be rejected")
	}
	if _, err := BuildBlockInfo(1, 0x99, 1); err == nil {
		t.Fatal("unknown block type must be rejected")
	}
}

func TestParseListBlocks(t *testing.T) {
	payload := mustHex(t, "30380001"+"30410002"+"30420000"+"30430003"+"30440000"+"30450000"+"30460000")
	got, err := ParseListBlocks(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("entries = %d", len(got))
	}
	if got[0].Type != BlockOB || got[0].Count != 1 {
		t.Fatalf("ob = %+v", got[0])
	}
	if got[1].Type != BlockDB || got[1].Count != 2 {
		t.Fatalf("db = %+v", got[1])
	}
	if got[3].Type != BlockFC || got[3].Count != 3 {
		t.Fatalf("fc = %+v", got[3])
	}

	if _, err := ParseListBlocks(payload[:24]); err == nil {
		t.Fatal("short payload must be rejected")
	}
	bad := append([]byte(nil), payload...)
	bad[0] = 0x31
	if _, err := ParseListBlocks(bad); err == nil {
		t.Fatal("bad marker must be rejected")
	}
	dup := append([]byte(nil), payload...)
	copy(dup[4:8], dup[0:4]) // 第二个条目也写成 OB
	if _, err := ParseListBlocks(dup); err == nil {
		t.Fatal("duplicate block type must be rejected")
	}
	unknown := append([]byte(nil), payload...)
	unknown[1] = 0x99
	if _, err := ParseListBlocks(unknown); err == nil {
		t.Fatal("unknown block type must be rejected")
	}
}

func TestParseListBlocksOfType(t *testing.T) {
	got, err := ParseListBlocksOfType(mustHex(t, "00010001"+"00030004"+"00070000"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 7 {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseListBlocksOfType([]byte{0, 1, 2}); err == nil {
		t.Fatal("non-multiple-of-4 payload must be rejected")
	}
}

func TestParseBlockInfo(t *testing.T) {
	p := make([]byte, 78)
	p[0] = 0x70
	p[1] = BlockDB
	p[9] = 0x01
	p[10] = 0x05
	binary.BigEndian.PutUint16(p[12:14], 17)
	binary.BigEndian.PutUint32(p[14:18], 1234)
	copy(p[22:28], []byte{1, 2, 3, 4, 5, 6})
	copy(p[28:34], []byte{7, 8, 9, 10, 11, 12})
	binary.BigEndian.PutUint16(p[34:36], 11)
	binary.BigEndian.PutUint16(p[38:40], 22)
	binary.BigEndian.PutUint16(p[40:42], 333)
	copy(p[42:50], "AUTHOR\x00\x00")
	copy(p[50:58], "FAMILY\x00\x00")
	copy(p[58:66], "HEADER\x00\x00")
	p[66] = 3
	binary.BigEndian.PutUint16(p[68:70], 0xBEEF)

	got, err := ParseBlockInfo(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Type != BlockDB || got.Number != 17 || got.LoadSize != 1234 {
		t.Fatalf("basic = %+v", got)
	}
	if got.SBBLength != 11 || got.LocalData != 22 || got.MC7Size != 333 {
		t.Fatalf("sizes = %+v", got)
	}
	if got.Author != "AUTHOR" || got.Family != "FAMILY" || got.Header != "HEADER" {
		t.Fatalf("strings = %+v", got)
	}
	if got.Version != 3 || got.Checksum != 0xBEEF {
		t.Fatalf("version/checksum = %+v", got)
	}
	if !bytes.Equal(got.CodeDate, []byte{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("code date = % x", got.CodeDate)
	}
	if !bytes.Equal(got.IntfDate, []byte{7, 8, 9, 10, 11, 12}) {
		t.Fatalf("intf date = % x", got.IntfDate)
	}
	if _, err := ParseBlockInfo(p[:77]); err == nil {
		t.Fatal("short payload must be rejected")
	}
}

func TestBuildStartUploadGolden(t *testing.T) {
	got, err := BuildStartUpload(4, BlockDB, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := mustHex(t, "32010000000400120000"+
		"1d0000000000000009"+
		"5f3041303030303141")
	if !bytes.Equal(got, want) {
		t.Fatalf("start upload =\n% x\nwant\n% x", got, want)
	}
}

func TestBuildUploadAndEndGolden(t *testing.T) {
	up := BuildUpload(5, 7)
	want := mustHex(t, "32010000000500080000"+"1e00000000000007")
	if !bytes.Equal(up, want) {
		t.Fatalf("upload =\n% x\nwant\n% x", up, want)
	}
	end := BuildEndUpload(6, 7)
	want = mustHex(t, "32010000000600080000"+"1f00000000000007")
	if !bytes.Equal(end, want) {
		t.Fatalf("end =\n% x\nwant\n% x", end, want)
	}
}

func TestParseStartUpload(t *testing.T) {
	param := mustHex(t, "1d000000000000070000003030313233")
	got, err := ParseStartUpload(PDU{ROSCTR: ROSCTRAckData, Param: param})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ID != 7 || got.Length != 123 {
		t.Fatalf("start = %+v", got)
	}
	if _, err := ParseStartUpload(PDU{ROSCTR: ROSCTRAckData, Param: param[:15]}); err == nil {
		t.Fatal("short param must be rejected")
	}
	bad := append([]byte(nil), param...)
	copy(bad[11:16], "00A23")
	if _, err := ParseStartUpload(PDU{ROSCTR: ROSCTRAckData, Param: bad}); err == nil {
		t.Fatal("non-decimal length must be rejected")
	}
}

func TestParseUpload(t *testing.T) {
	data := []byte{0x00, 0x03, 0x00, 0xFB, 0xAA, 0xBB, 0xCC}
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncUpload, 0x00}, Data: data}
	got, err := ParseUpload(pdu)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Last || !bytes.Equal(got.Data, []byte{0xAA, 0xBB, 0xCC}) {
		t.Fatalf("fragment = %+v", got)
	}

	// param[1]=1 表示「后面还有」，不是最后一片。
	pdu.Param[1] = 1
	got, err = ParseUpload(pdu)
	if err != nil || got.Last {
		t.Fatalf("more flag: %+v, %v", got, err)
	}

	pdu.Param[1] = 2
	if _, err := ParseUpload(pdu); err == nil {
		t.Fatal("flag > 1 must be rejected")
	}

	pdu.Param[1] = 0
	pdu.Data[3] = 0xFA
	if _, err := ParseUpload(pdu); err == nil {
		t.Fatal("bad marker must be rejected")
	}

	pdu.Data = []byte{0, 4, 0x00, 0xFB, 1, 2, 3}
	if _, err := ParseUpload(pdu); err == nil {
		t.Fatal("length mismatch must be rejected")
	}
}

func TestParseEndUpload(t *testing.T) {
	if err := ParseEndUpload(PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncEndUpload}}); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := ParseEndUpload(PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncEndUpload, 0}}); err == nil {
		t.Fatal("extra param byte must be rejected")
	}
	if err := ParseEndUpload(PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncEndUpload}, Data: []byte{0}}); err == nil {
		t.Fatal("data must be rejected")
	}
}

func TestUserDataResponseRoundTrip(t *testing.T) {
	payload := []byte{0x30, 0x38, 0x00, 0x01}
	raw := BuildUserDataResponse(9, UserDataGroupBlocks, UserDataListAll, true, payload)
	pdu, err := Parse(TPKT(append([]byte{0x02, 0xF0, 0x80}, raw...)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ud, err := ParseUserData(pdu)
	if err != nil {
		t.Fatalf("userdata: %v", err)
	}
	if ud.Group != UserDataGroupBlocks || ud.Subfunc != UserDataListAll || !ud.More {
		t.Fatalf("header = %+v", ud)
	}
	if !bytes.Equal(ud.Payload, payload) {
		t.Fatalf("payload = % x", ud.Payload)
	}

	// 参数区长度、方法字节、数据头任一不对都必须报错。
	if _, err := ParseUserData(PDU{ROSCTR: ROSCTRUserData, Param: []byte{0}}); err == nil {
		t.Fatal("short param must be rejected")
	}
	bad := append([]byte(nil), pdu.Param...)
	bad[4] = 0x11
	if _, err := ParseUserData(PDU{ROSCTR: ROSCTRUserData, Param: bad, Data: pdu.Data}); err == nil {
		t.Fatal("request method byte must be rejected")
	}
	badData := append([]byte(nil), pdu.Data...)
	badData[0] = 0x0A
	if _, err := ParseUserData(PDU{ROSCTR: ROSCTRUserData, Param: pdu.Param, Data: badData}); err == nil {
		t.Fatal("bad data header must be rejected")
	}
}

func TestParseUserDataReportsErrorCode(t *testing.T) {
	param := make([]byte, 12)
	param[2], param[4], param[5], param[6] = 0x12, 0x12, 0x43, UserDataListAll
	param[10], param[11] = 0x0A, 0x0D
	if _, err := ParseUserData(PDU{ROSCTR: ROSCTRUserData, Param: param}); err == nil {
		t.Fatal("non-zero userdata error code must be reported")
	}
}

func TestBlockTypeNames(t *testing.T) {
	for _, name := range []string{"OB", "DB", "SDB", "FC", "SFC", "FB", "SFB"} {
		t1, err := BlockTypeOf(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := BlockTypeName(t1); got != name {
			t.Fatalf("%s -> %#02x -> %q", name, t1, got)
		}
	}
	if got, err := BlockTypeOf(" db "); err != nil || got != BlockDB {
		t.Fatalf("whitespace/case must be tolerated: %#02x, %v", got, err)
	}
	if _, err := BlockTypeOf("XX"); err == nil {
		t.Fatal("unknown name must be rejected")
	}
	if got := BlockTypeName(0x99); got != "0x99" {
		t.Fatalf("unknown type = %q", got)
	}
}
