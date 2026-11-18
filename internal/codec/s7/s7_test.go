package s7

import (
	"bytes"
	"encoding/hex"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

// COTP 连接请求必须与现场抓包逐字节一致。这里的值不是随便写的：
// 源 TSAP 0x0100、目标 TSAP 0x0102、TPDU size 1024（编码值 0x0A、长度恒
// 1 字节）是 snap7 的 CliConnect 与 S7-300/400/1200/1500 都接受的最小集合。
func TestCOTPConnectGolden(t *testing.T) {
	got := COTPConnect()
	want := mustHex(t, "03000016"+"11e00000000100"+"c1020100"+"c2020102"+"c0010a")
	if !bytes.Equal(got, want) {
		t.Fatalf("cotp cr =\n% x\nwant\n% x", got, want)
	}
}

// 槽号必须真的进入目的 TSAP：S7-1200/1500 在槽 1，S7-300/400 在槽 2，
// 写死一个值会让另一半设备连不上。
func TestCOTPConnectRackSlot(t *testing.T) {
	got := COTPConnectWith(0, 1)
	want := mustHex(t, "03000016"+"11e00000000100"+"c1020100"+"c2020101"+"c0010a")
	if !bytes.Equal(got, want) {
		t.Fatalf("frame =\n% x\nwant\n% x", got, want)
	}
	// rack 1 slot 4 → 0x0100 + 0x20 + 4 = 0x0124（位于 0xC2 参数的值字节）
	got = COTPConnectWith(1, 4)
	if tsap := got[17:19]; !bytes.Equal(tsap, []byte{0x01, 0x24}) {
		t.Fatalf("dst tsap = % x, want 01 24", tsap)
	}
}

func TestParseCOTPConnect(t *testing.T) {
	// CC 与 CR 同构：TSAP 源/目标对调、DST-REF 回填 SRC-REF。
	cc := mustHex(t, "03000016"+"11d00001000100"+"c2020102"+"c1020100"+"c0010a")
	if err := ParseCOTPConnect(cc); err != nil {
		t.Fatalf("cc should parse: %v", err)
	}
	// 用 CR 冒充 CC 必须被拒绝：把连接请求当确认会让后续所有报文错位。
	if err := ParseCOTPConnect(COTPConnect()); err == nil {
		t.Fatal("CR must not be accepted as CC")
	}
}

// 建立通信请求是最容易写错的一帧：参数长度 8、PDU 长度在最后两字节。
func TestSetupRequestGolden(t *testing.T) {
	pdu := BuildPDU(ROSCTRJob, 1, SetupParams(480), nil)
	want := mustHex(t, "32010000000100080000f0000001000101e0")
	if !bytes.Equal(pdu, want) {
		t.Fatalf("setup =\n% x\nwant\n% x", pdu, want)
	}
}

func TestWrapDT(t *testing.T) {
	pdu := mustHex(t, "32010000000100080000f0000001000101e0")
	frame := WrapDT(pdu)
	// TPKT 总长 = 4 + 3(COTP DT) + len(pdu) = 25
	if len(frame) != 25 {
		t.Fatalf("frame len = %d, want 25", len(frame))
	}
	want := append(mustHex(t, "0300001902f080"), pdu...)
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame =\n% x\nwant\n% x", frame, want)
	}
}

func TestParseSetupAck(t *testing.T) {
	// 典型 S7-300 应答：协商 240 字节（0x00F0）。
	pdu := PDU{
		ROSCTR: ROSCTRAckData,
		Param:  mustHex(t, "f0000001000100f0"),
	}
	got, err := ParseSetupAck(pdu)
	if err != nil {
		t.Fatalf("setup ack: %v", err)
	}
	if got != 240 {
		t.Fatalf("negotiated = %d, want 240", got)
	}
}

func TestParseSetupAckRejectsSmall(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: mustHex(t, "f000000100010008")}
	if _, err := ParseSetupAck(pdu); err == nil {
		t.Fatal("pdu length 8 must be rejected")
	}
}

// 读 1 字节（DB1.DBB0）的请求必须是 14 字节参数 + 10 字节头。
// 注意地址字段是「字节地址×8 + 位号」，写错一位就会读到别的字节。
func TestBuildReadGolden(t *testing.T) {
	pdu, err := BuildRead(2, []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := mustHex(t, "320100000002000e0000040112"+"0a10020001000184000000")
	if !bytes.Equal(pdu, want) {
		t.Fatalf("read =\n% x\nwant\n% x", pdu, want)
	}
}

// 位地址：DB1.DBX0.3 → 字节地址 0×8+3 = 3。
func TestBuildReadBitAddress(t *testing.T) {
	pdu, err := BuildRead(3, []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: 3, BitAccess: true}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 最后三字节是地址 00 00 03。
	if got := pdu[len(pdu)-3:]; !bytes.Equal(got, []byte{0, 0, 3}) {
		t.Fatalf("address bytes = % x, want 00 00 03", got)
	}
}

func TestBuildReadRejectsBadItems(t *testing.T) {
	cases := []Item{
		{Area: AreaDB, DB: 0, ByteAddr: 0, Bytes: 1},                // DB 区缺 DB 号
		{Area: AreaMerker, DB: 1, ByteAddr: 0, Bytes: 1},            // 非 DB 区带 DB 号
		{Area: AreaDB, DB: 1, ByteAddr: 0, Bytes: 0},                // 长度 0
		{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: 8, BitAccess: true}, // 位号越界
		{Area: 0x99, ByteAddr: 0, Bytes: 1},                         // 未知区
	}
	for i, it := range cases {
		if _, err := BuildRead(1, []Item{it}); err == nil {
			t.Fatalf("case %d must be rejected: %+v", i, it)
		}
	}
}

func TestParseReadAck(t *testing.T) {
	// ACK_DATA 是 12 字节头：数据长度之后还有 error class / error code。
	// TPKT 总长 = 4 + 3 + (12 头 + 2 参数 + 5 数据) = 26
	frame := mustHex(t, "0300001a02f080"+
		"32030000000200020005"+"0000"+
		"0401"+
		"ff0400087f")
	pdu, err := Parse(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	items := []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 1}}
	vals, errs, err := ParseReadAck(pdu, items)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if errs[0] != nil {
		t.Fatalf("unexpected item error: %v", errs[0])
	}
	if !bytes.Equal(vals[0], []byte{0x7f}) {
		t.Fatalf("value = % x", vals[0])
	}
}

// 长度单位：BIT/BYTE/WORD/DWORD 用位，INT/DINT/REAL 用字节。混淆会读出半个值。
func TestParseReadAckLengthUnits(t *testing.T) {
	// 4 字节 REAL：传输尺寸 0x07（REAL），长度 4 → 4 字节数据。
	// TPKT 总长 = 4 + 3 + (12 头 + 2 参数 + 8 数据) = 29
	frame := mustHex(t, "0300001d02f080"+
		"32030000000200020008"+"0000"+
		"0401"+
		"ff0700043f800000")
	pdu, err := Parse(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	items := []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 4}}
	vals, errs, err := ParseReadAck(pdu, items)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if errs[0] != nil {
		t.Fatalf("item error: %v", errs[0])
	}
	if len(vals[0]) != 4 {
		t.Fatalf("value len = %d, want 4", len(vals[0]))
	}
}

func TestParseReadAckItemError(t *testing.T) {
	pdu := PDU{
		ROSCTR: ROSCTRAckData,
		Param:  []byte{FuncRead, 1},
		Data:   []byte{CodeObjectNotFound, 0x00, 0x00, 0x00},
	}
	_, errs, err := ParseReadAck(pdu, []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bytes: 1}})
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	ie, ok := errs[0].(ItemError)
	if !ok {
		t.Fatalf("err = %v, want ItemError", errs[0])
	}
	if ie.Code != CodeObjectNotFound {
		t.Fatalf("code = %#x", ie.Code)
	}
	if ie.Kind() != driver.ErrAddress {
		t.Fatalf("kind = %v, want ErrAddress", ie.Kind())
	}
}

func TestParseReadAckRejectsCountMismatch(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncRead, 2}, Data: []byte{0xff, 4, 0, 8, 1, 0}}
	if _, _, err := ParseReadAck(pdu, []Item{{Area: AreaDB, DB: 1, Bytes: 1}}); err == nil {
		t.Fatal("count mismatch must be rejected")
	}
}

func TestBuildWriteGolden(t *testing.T) {
	items := []Item{{Area: AreaDB, DB: 1, ByteAddr: 4, Bit: -1, Bytes: 2}}
	pdu, err := BuildWrite(9, items, [][]byte{{0x12, 0x34}})
	if err != nil {
		t.Fatalf("build write: %v", err)
	}
	// 参数：05 01 + item（0x02=BYTE，长度按字节）；数据：00 04 0010 12 34
	wantParam := mustHex(t, "0501"+"120a10020002"+"0001"+"84"+"000020")
	wantData := mustHex(t, "000400101234")
	if !bytes.Equal(pdu[10:10+len(wantParam)], wantParam) {
		t.Fatalf("param = % x\nwant % x", pdu[10:10+len(wantParam)], wantParam)
	}
	if !bytes.Equal(pdu[10+len(wantParam):], wantData) {
		t.Fatalf("data = % x\nwant % x", pdu[10+len(wantParam):], wantData)
	}
}

func TestBuildWritePadsOddLength(t *testing.T) {
	items := []Item{{Area: AreaMerker, ByteAddr: 0, Bit: -1, Bytes: 3}}
	pdu, err := BuildWrite(1, items, [][]byte{{1, 2, 3}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 数据区：4 字节头 + 3 字节 + 1 字节对齐 = 8
	dataLen := int(pdu[9])
	if dataLen != 8 {
		t.Fatalf("data len = %d, want 8", dataLen)
	}
	if pdu[len(pdu)-1] != 0x00 {
		t.Fatalf("last byte = %#x, want pad 0", pdu[len(pdu)-1])
	}
}

func TestBuildWriteRejectsLengthMismatch(t *testing.T) {
	items := []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bytes: 2}}
	if _, err := BuildWrite(1, items, [][]byte{{0x01}}); err == nil {
		t.Fatal("value length mismatch must be rejected")
	}
	if _, err := BuildWrite(1, items, nil); err == nil {
		t.Fatal("missing values must be rejected")
	}
}

func TestParseWriteAck(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncWrite, 2}, Data: []byte{0xFF, CodeAccessDenied}}
	errs, err := ParseWriteAck(pdu, 2)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if errs[0] != nil {
		t.Fatalf("item 0 should succeed: %v", errs[0])
	}
	ie, ok := errs[1].(ItemError)
	if !ok || ie.Kind() != driver.ErrPermission {
		t.Fatalf("item 1 = %v", errs[1])
	}
}

func TestAreaOf(t *testing.T) {
	cases := map[string]byte{
		"db": AreaDB, "DB": 0, "input": AreaInput, "i": AreaInput, "e": AreaInput,
		"output": AreaOutput, "q": AreaOutput, "merker": AreaMerker, "m": AreaMerker,
		"counter": AreaCounter, "timer": AreaTimer,
	}
	for in, want := range cases {
		got, err := AreaOf(in)
		if want == 0 {
			if err == nil {
				t.Fatalf("%q should be rejected (case-sensitive)", in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q -> %#x, want %#x", in, got, want)
		}
	}
	if _, err := AreaOf("nope"); err == nil {
		t.Fatal("unknown area must be rejected")
	}
}

func TestItemOf(t *testing.T) {
	// bool → 位访问
	it, err := ItemOf(driver.PointRef{
		ID: "run", DataType: model.TypeBool,
		Address: model.Address{Area: "db", DB: 1, Offset: 2, Bit: 5},
	})
	if err != nil {
		t.Fatalf("bool: %v", err)
	}
	if !it.BitAccess || it.Bit != 5 || it.ByteAddr != 2 || it.DB != 1 {
		t.Fatalf("item = %+v", it)
	}

	// float32 → 4 字节
	it, err = ItemOf(driver.PointRef{
		ID: "temp", DataType: model.TypeFloat32,
		Address: model.Address{Area: "db", DB: 3, Offset: 10},
	})
	if err != nil {
		t.Fatalf("real: %v", err)
	}
	if it.Bytes != 4 || it.DB != 3 || it.ByteAddr != 10 {
		t.Fatalf("item = %+v", it)
	}

	// string 必须显式长度
	if _, err := ItemOf(driver.PointRef{
		ID: "name", DataType: model.TypeString,
		Address: model.Address{Area: "db", DB: 1, Offset: 0},
	}); err == nil {
		t.Fatal("string without length must be rejected")
	}
}

// ItemError 的文案要能让现场直接对照手册。
func TestItemErrorText(t *testing.T) {
	cases := map[byte]string{
		CodeObjectNotFound: "object does not exist",
		CodeInvalidAddr:    "address out of range",
		CodeAccessDenied:   "access denied",
	}
	for code, want := range cases {
		if got := (ItemError{Code: code}).CodeText(); got != want {
			t.Fatalf("%#x -> %q, want %q", code, got, want)
		}
	}
}

// ACK / ACK_DATA 是 12 字节头，Job / UserData 是 10 字节头。这条差别
// 决定了参数区从 10 还是 12 开始；混用会让整帧数据错位两字节。
func TestHeaderLen(t *testing.T) {
	cases := map[byte]int{
		ROSCTRJob:      10,
		ROSCTRJobAck:   12,
		ROSCTRAckData:  12,
		ROSCTRUserData: 10,
	}
	for rosctr, want := range cases {
		if got := HeaderLen(rosctr); got != want {
			t.Errorf("HeaderLen(%#02x) = %d, want %d", rosctr, got, want)
		}
	}
}

func TestBuildPDUAddsErrorBytesForAck(t *testing.T) {
	pdu := BuildPDU(ROSCTRAckData, 7, []byte{0x04, 0x01}, []byte{0xFF})
	if len(pdu) != 12+2+1 {
		t.Fatalf("len = %d, want 15", len(pdu))
	}
	if pdu[10] != 0 || pdu[11] != 0 {
		t.Fatalf("error bytes = % x, want 00 00", pdu[10:12])
	}
	if !bytes.Equal(pdu[12:14], []byte{0x04, 0x01}) || pdu[14] != 0xFF {
		t.Fatalf("param/data misplaced: % x", pdu[12:])
	}

	// Job 不应该被塞进两个字节。
	job := BuildPDU(ROSCTRJob, 7, []byte{0x01}, nil)
	if len(job) != 10+1 {
		t.Fatalf("job len = %d, want 11", len(job))
	}
}

func TestBuildPDUError(t *testing.T) {
	pdu := BuildPDUError(ROSCTRAckData, 1, 0x81, 0x04, nil, nil)
	if len(pdu) != 12 || pdu[10] != 0x81 || pdu[11] != 0x04 {
		t.Fatalf("pdu = % x", pdu)
	}
}

func TestParseACKErrorBytes(t *testing.T) {
	// 12 字节头 + 0 参数 + 0 数据，error class/code = 81 04。
	pdu := mustHex(t, "3203"+"0000"+"0001"+"0000"+"0000"+"8104")
	got, err := Parse(TPKT(append([]byte{0x02, 0xF0, 0x80}, pdu...)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ErrClass != 0x81 || got.ErrCode != 0x04 {
		t.Fatalf("error = %#02x/%#02x", got.ErrClass, got.ErrCode)
	}
	if got.Error() == nil {
		t.Fatal("Error() must report a non-zero header error")
	}
	if len(got.Param) != 0 || len(got.Data) != 0 {
		t.Fatalf("param/data = % x / % x", got.Param, got.Data)
	}

	// 同样的字节按 Job 解析会少读两个字节——这正是修复前的行为。
	// 这里验证 UserData 的 10 字节头仍然正确。
	ud := BuildPDU(ROSCTRUserData, 1, []byte{1, 2}, nil)
	got, err = Parse(TPKT(append([]byte{0x02, 0xF0, 0x80}, ud...)))
	if err != nil {
		t.Fatalf("userdata parse: %v", err)
	}
	if len(got.Param) != 2 || got.Param[0] != 1 || got.Param[1] != 2 {
		t.Fatalf("userdata param = % x", got.Param)
	}
}
