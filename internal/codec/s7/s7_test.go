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
// TPDU 256、TSAP 0x0100/0x0102 是 S7-300/400/1200/1500 都接受的最小集合。
func TestCOTPConnectGolden(t *testing.T) {
	got := COTPConnect()
	want := mustHex(t, "0300001711e00000000100c1020100c2020100c0020102")
	if !bytes.Equal(got, want) {
		t.Fatalf("cotp cr =\n% x\nwant\n% x", got, want)
	}
}

// 槽号必须真的进入目的 TSAP：S7-1200/1500 在槽 1，S7-300/400 在槽 2，
// 写死一个值会让另一半设备连不上。
func TestCOTPConnectRackSlot(t *testing.T) {
	got := COTPConnectWith(0, 1)
	want := mustHex(t, "0300001711e00000000100c1020100c2020100c0020101")
	if !bytes.Equal(got, want) {
		t.Fatalf("frame =\n% x\nwant\n% x", got, want)
	}
	// rack 1 slot 4 → 0x0100 + 0x20 + 4 = 0x0124
	got = COTPConnectWith(1, 4)
	if tail := got[len(got)-2:]; !bytes.Equal(tail, []byte{0x01, 0x24}) {
		t.Fatalf("dst tsap = % x, want 01 24", tail)
	}
}

func TestParseCOTPConnect(t *testing.T) {
	cc := mustHex(t, "0300001711d00001000100c0020102c1020100c2020100")
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
	// TPKT 总长 = 4 + 3 + (10 头 + 2 参数 + 5 数据) = 24
	frame := mustHex(t, "0300001802f080"+
		"32030000000200020005"+
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
	// TPKT 总长 = 4 + 3 + (10 头 + 2 参数 + 8 数据) = 27
	frame := mustHex(t, "0300001b02f080"+
		"32030000000200020008"+
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
	frame := mustHex(t, "0300001802f080"+
		"32030000000200020005"+
		"0401"+
		"0a0000")
	// 上面 data 只有 3 字节，补一个 0 让它达到声明的 5 字节。
	frame = append(frame, 0x00)
	_ = frame

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
