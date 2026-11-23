package enip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuildForwardOpenGolden(t *testing.T) {
	path := []byte{0x20, 0x02, 0x24, 0x01} // Message Router: class 2 / instance 1
	b, err := BuildForwardOpen(ForwardOpen{
		OTConnectionID:    0x11223344,
		TOConnectionID:    0x55667788,
		SerialNumber:      0x99AA,
		VendorID:          0xBBCC,
		OriginatorSerial:  0xDDEEFF00,
		OTRPI:             0x00112233,
		TORPI:             0x44556677,
		ConnectionSize:    400,
		Priority:          0x0A,
		TimeoutTicks:      0x0E,
		TimeoutMultiplier: 0x07,
		ConnectionPath:    path,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x54, 0x02, 0x20, 0x06, 0x24, 0x01, // 服务 + 路径（Connection Manager 6/1）
		0x0A, 0x0E, // priority / timeout ticks
		0x44, 0x33, 0x22, 0x11, // O→T 连接 ID
		0x88, 0x77, 0x66, 0x55, // T→O 连接 ID
		0xAA, 0x99, // 连接序列号
		0xCC, 0xBB, // 厂商号
		0x00, 0xFF, 0xEE, 0xDD, // 发起方序列号
		0x07, 0x00, 0x00, 0x00, // 超时乘数 + 保留(3)
		0x33, 0x22, 0x11, 0x00, // O→T RPI
		0x90, 0x43, // 网络连接参数：0x4200 | 400
		0x77, 0x66, 0x55, 0x44, // T→O RPI
		0x90, 0x43, // 网络连接参数
		0xA3, 0x02, // 传输类别 + 连接路径字数
		0x20, 0x02, 0x24, 0x01, // 连接路径
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("Forward Open =\n% x\nwant\n% x", b, want)
	}
}

func TestBuildLargeForwardOpenGolden(t *testing.T) {
	b, err := BuildForwardOpen(ForwardOpen{
		Extended:       true,
		ConnectionSize: 400,
		ConnectionPath: []byte{0x20, 0x02, 0x24, 0x01},
		Priority:       0x0A,
		TimeoutTicks:   0x0E,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != ServiceLargeForwardOpen {
		t.Fatalf("service = %02X", b[0])
	}
	// Large 格式的网络连接参数是 32 位：0x4200 在高 16 位，尺寸在低 16 位。
	wantParams := []byte{0x90, 0x01, 0x00, 0x42}
	if !bytes.Contains(b, wantParams) {
		t.Fatalf("missing large net params % x in\n% x", wantParams, b)
	}
}

func TestBuildForwardOpenRejectsOversize(t *testing.T) {
	// 标准格式只有 9 位装尺寸；超过 511 必须显式要求 Large 格式，
	// 不能把高位静默写进方向/类型位。
	if _, err := BuildForwardOpen(ForwardOpen{
		ConnectionSize: 512,
		ConnectionPath: []byte{0x20, 0x02, 0x24, 0x01},
	}); err == nil {
		t.Fatal("oversized standard Forward Open accepted")
	}
	if _, err := BuildForwardOpen(ForwardOpen{ConnectionSize: 100}); err == nil {
		t.Fatal("Forward Open without a connection path accepted")
	}
	if _, err := BuildForwardOpen(ForwardOpen{
		ConnectionSize: 100,
		ConnectionPath: []byte{0x20, 0x02, 0x24}, // 奇数字节
	}); err == nil {
		t.Fatal("misaligned connection path accepted")
	}
}

func TestParseForwardOpenReply(t *testing.T) {
	data := make([]byte, 26)
	binary.LittleEndian.PutUint32(data[0:], 0x20000002)
	binary.LittleEndian.PutUint32(data[4:], 0x7FFF0001)
	binary.LittleEndian.PutUint16(data[8:], 0x99AA)
	binary.LittleEndian.PutUint16(data[10:], 0xBBCC)
	binary.LittleEndian.PutUint32(data[12:], 0xDDEEFF00)
	got, err := ParseForwardOpenReply(Reply{Service: ServiceForwardOpen | ServiceReplyMask, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if got.OTConnectionID != 0x20000002 || got.TOConnectionID != 0x7FFF0001 ||
		got.SerialNumber != 0x99AA || got.VendorID != 0xBBCC || got.OriginatorSerial != 0xDDEEFF00 {
		t.Fatalf("reply = %+v", got)
	}
	if _, err := ParseForwardOpenReply(Reply{Service: ServiceForwardOpen | ServiceReplyMask, Data: data[:25]}); err == nil {
		t.Fatal("short Forward Open reply accepted")
	}
	if _, err := ParseForwardOpenReply(Reply{
		Service: ServiceForwardOpen | ServiceReplyMask,
		Status:  0x06,
		Data:    data,
	}); err == nil {
		t.Fatal("error Forward Open reply accepted")
	}
}

func TestBuildForwardCloseGolden(t *testing.T) {
	b, err := BuildForwardClose(0x0A, 0x0E, 0x99AA, 0xBBCC, 0xDDEEFF00)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x4E, 0x02, 0x20, 0x06, 0x24, 0x01,
		0x0A, 0x0E,
		0xAA, 0x99,
		0xCC, 0xBB,
		0x00, 0xFF, 0xEE, 0xDD,
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("Forward Close =\n% x\nwant\n% x", b, want)
	}
}

func TestSendUnitDataGolden(t *testing.T) {
	b, err := BuildSendUnitData(0x11223344, 0x5566, []byte{0x4C, 0x02, 0x20, 0x24})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x00, // 接口句柄
		0x00, 0x00, // 超时
		0x02, 0x00, // 条目数 = 2
		0xA1, 0x00, 0x04, 0x00, // 连接地址条目
		0x44, 0x33, 0x22, 0x11,
		0xB1, 0x00, 0x06, 0x00, // 连接数据条目（序列号 + CIP）
		0x66, 0x55,
		0x4C, 0x02, 0x20, 0x24,
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("SendUnitData =\n% x\nwant\n% x", b, want)
	}

	conn, seq, cip, err := ParseSendUnitData(b)
	if err != nil {
		t.Fatal(err)
	}
	if conn != 0x11223344 || seq != 0x5566 || !bytes.Equal(cip, []byte{0x4C, 0x02, 0x20, 0x24}) {
		t.Fatalf("parsed = %08X/%04X/% x", conn, seq, cip)
	}
}

func TestParseSendUnitDataRejectsBadCPF(t *testing.T) {
	// 只有数据条目、没有地址条目：结构不对，不能继续把后面的字节当 CIP。
	noAddr, _ := BuildCPF([]Item{{Type: ItemConnectedData, Data: []byte{0, 0, 0x4C}}})
	if _, _, _, err := ParseSendUnitData(noAddr); err == nil {
		t.Fatal("CPF without connected address accepted")
	}
	// 两个数据条目：不知道哪个才是真正的应答。
	twoData, _ := BuildCPF([]Item{
		{Type: ItemConnectedAddress, Data: []byte{1, 2, 3, 4}},
		{Type: ItemConnectedData, Data: []byte{0, 0}},
		{Type: ItemConnectedData, Data: []byte{0, 0}},
	})
	if _, _, _, err := ParseSendUnitData(twoData); err == nil {
		t.Fatal("CPF with two data items accepted")
	}
	short, _ := BuildCPF([]Item{
		{Type: ItemConnectedAddress, Data: []byte{1, 2, 3, 4}},
		{Type: ItemConnectedData, Data: []byte{0}},
	})
	if _, _, _, err := ParseSendUnitData(short); err == nil {
		t.Fatal("connected data shorter than sequence accepted")
	}
}

func TestBuildReadTagFragGolden(t *testing.T) {
	b, err := BuildReadTagFrag("A", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x52, 0x02, 0x91, 0x01, 'A', 0x00,
		0x01, 0x00, // 总元素数
		0x00, 0x00, 0x00, 0x00, // 字节偏移
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("Read Tag Fragmented =\n% x\nwant\n% x", b, want)
	}
	if _, err := BuildReadTagFrag("A", 0, 0); err == nil {
		t.Fatal("zero element count accepted")
	}
}

func TestBuildWriteTagFragGolden(t *testing.T) {
	b, err := BuildWriteTagFrag("A", TypeDINT, 1, 4, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x53, 0x02, 0x91, 0x01, 'A', 0x00,
		0xC4, 0x00, // 数据类型
		0x01, 0x00, // 总元素数
		0x04, 0x00, 0x00, 0x00, // 字节偏移
		0x01, 0x02, 0x03, 0x04,
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("Write Tag Fragmented =\n% x\nwant\n% x", b, want)
	}
	if _, err := BuildWriteTagFrag("A", TypeDINT, 1, 0, nil); err == nil {
		t.Fatal("empty fragment accepted")
	}
}

func TestParseReadTagFragReply(t *testing.T) {
	// 0x06 表示「这一片成功、还有后续」，应答体仍是类型 + 数据。
	reply := Reply{
		Service: ServiceReadFrag | ServiceReplyMask,
		Status:  0x06,
		Data:    []byte{0xC4, 0x00, 0x01, 0x00, 0x00, 0x00},
	}
	typ, data, more, err := ParseReadTagFragReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeDINT || !more || !bytes.Equal(data, []byte{1, 0, 0, 0}) {
		t.Fatalf("frag = %02X/%v/% x", typ, more, data)
	}

	last := Reply{Service: ServiceReadFrag | ServiceReplyMask, Data: []byte{0xC4, 0x00, 0x02, 0x00, 0x00, 0x00}}
	_, _, more, err = ParseReadTagFragReply(last)
	if err != nil || more {
		t.Fatalf("final frag: more=%v err=%v", more, err)
	}

	if _, _, _, err := ParseReadTagFragReply(Reply{Status: 0x05}); err == nil {
		t.Fatal("error fragment accepted")
	}
}
