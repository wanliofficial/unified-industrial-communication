package ads

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"uic/internal/driver"
)

// 这一层不做 I/O，所有断言都应当能只靠字节完成：金帧钉住线格式，
// 逐字段断言钉住语义。只做「encode 再 decode 相等」会掩盖两边一起错。

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad hex in test: %v", err)
	}
	return b
}

func TestNetID(t *testing.T) {
	n, err := ParseNetID("192.168.1.10.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if n != (NetID{192, 168, 1, 10, 1, 1}) {
		t.Fatalf("netid = %v", n)
	}
	if got := n.String(); got != "192.168.1.10.1.1" {
		t.Fatalf("String = %q", got)
	}
	for _, bad := range []string{"", "1.2.3", "1.2.3.4.5.6.7", "1.2.3.4.5.256", "1.2.3.4.5.x"} {
		if _, err := ParseNetID(bad); err == nil {
			t.Fatalf("ParseNetID(%q) accepted", bad)
		}
	}
}

// HandleByName 的金帧。变量名 "MAIN.nCounter"、invoke 0x01020304。
// 注意写入长度是 len(name)+1（结尾 NUL），少写这一个字节设备会读到
// 名字后面的内存。
func TestHandleByNameGolden(t *testing.T) {
	p := Peer{
		Source: Endpoint{NetID: MustParseNetID("127.0.0.1.1.1"), Port: 32905},
		Target: Endpoint{NetID: MustParseNetID("192.168.1.10.1.1"), Port: 851},
	}
	got, err := p.HandleByName(0x01020304, "MAIN.nCounter").Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := hexBytes(t, `
		00 00 3e 00 00 00
		  c0 a8 01 0a 01 01 53 03
		  7f 00 00 01 01 01 89 80
		  09 00 04 00
		  1e 00 00 00
		  00 00 00 00
		  04 03 02 01
		    03 f0 00 00
		    00 00 00 00
		    04 00 00 00
		    0e 00 00 00
		    4d 41 49 4e 2e 6e 43 6f 75 6e 74 65 72 00`)
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = % x\nwant  = % x", got, want)
	}
	// 解回来必须得到同样的结构。
	f, err := Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	if f.Command != CmdReadWrite || f.Invoke != 0x01020304 || f.Flags != FlagADS {
		t.Fatalf("header = %v invoke=%#x flags=%#x", f.Command, f.Invoke, f.Flags)
	}
	if f.Target.String() != "192.168.1.10.1.1:851" || f.Source.String() != "127.0.0.1.1.1:32905" {
		t.Fatalf("endpoints = %v -> %v", f.Source, f.Target)
	}
	if len(f.Data) != 30 {
		t.Fatalf("payload = %d bytes", len(f.Data))
	}
}

func TestFrameRoundTrip(t *testing.T) {
	p := Peer{
		Source: Endpoint{NetID: NetID{1, 2, 3, 4, 5, 6}, Port: 32905},
		Target: Endpoint{NetID: NetID{10, 20, 30, 40, 50, 60}, Port: 801},
	}
	big := bytes.Repeat([]byte{0xA5}, 300)
	cases := []Frame{
		p.Read(1, IGSymValueByHandle, 7, 4),
		p.Read(2, 0xF004, 0, 128),
		p.Write(3, 0x4020, 0, big),
		p.ReadWrite(4, IGSymHandleByName, 0, 4, []byte("MAIN.x")),
		p.ReadDeviceInfo(5),
		p.ReadState(6),
		p.WriteControl(7, StateRun, 0, nil),
		p.ReleaseHandle(8, 0x00010001),
	}
	for _, f := range cases {
		f.Flags |= FlagResponse
		f.Error = CodeInvalidOffset
		raw, err := f.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(raw)
		if err != nil {
			t.Fatalf("%v: %v", f.Command, err)
		}
		if got.Command != f.Command || got.Invoke != f.Invoke || got.Flags != f.Flags ||
			got.Error != f.Error || got.Target != f.Target || got.Source != f.Source {
			t.Fatalf("%v round trip mismatch: %+v", f.Command, got)
		}
		if !bytes.Equal(got.Data, f.Data) {
			t.Fatalf("%v payload mismatch", f.Command)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	good, err := Peer{}.Read(1, 0, 0, 1).Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"short":              good[:10],
		"reserved non-zero":  append([]byte{1, 0}, good[2:]...),
		"tcp length too big": append([]byte{0, 0, 0xFF, 0, 0, 0}, good[6:]...),
		"tcp length too low": append([]byte{0, 0, 1, 0, 0, 0}, good[6:]...),
	}
	for name, raw := range cases {
		if _, err := Decode(raw); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// AMS 头声称的载荷长度与 TCP 长度不一致。
	bad := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(bad[6+20:6+24], 99)
	if _, err := Decode(bad); err == nil {
		t.Fatal("payload length mismatch accepted")
	}
	// AMS 长度小于头。
	bad = append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(bad[2:6], 31)
	if _, err := Decode(bad); err == nil {
		t.Fatal("ams length below header accepted")
	}
}

func TestFrameLen(t *testing.T) {
	raw, err := Peer{}.ReadDeviceInfo(1).Encode()
	if err != nil {
		t.Fatal(err)
	}
	n, err := FrameLen(raw[:6])
	if err != nil {
		t.Fatal(err)
	}
	if n != len(raw) {
		t.Fatalf("FrameLen = %d, want %d", n, len(raw))
	}
	if _, err := FrameLen(raw[:4]); err == nil {
		t.Fatal("short tcp header accepted")
	}
}

func TestParseRead(t *testing.T) {
	payload := make([]byte, 8+2)
	binary.LittleEndian.PutUint32(payload[4:8], 2) // length
	payload[8], payload[9] = 0x2A, 0x00
	data, code, err := ParseRead(Frame{Data: payload})
	if err != nil || code != CodeOK {
		t.Fatalf("parse: %v code=%v", err, code)
	}
	if !bytes.Equal(data, []byte{0x2A, 0x00}) {
		t.Fatalf("data = % x", data)
	}

	// 错误应答可能只有 4 字节。
	_, code, err = ParseRead(Frame{Data: []byte{0x02, 0x07, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if code != CodeInvalidGroup {
		t.Fatalf("code = %v", code)
	}

	for name, p := range map[string][]byte{
		"no result":   {0, 0},
		"no length":   {0, 0, 0, 0},
		"length lies": {0, 0, 0, 0, 9, 0, 0, 0, 1},
	} {
		if _, _, err := ParseRead(Frame{Data: p}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestParseWrite(t *testing.T) {
	code, err := ParseWrite(Frame{Data: []byte{0, 0, 0, 0}})
	if err != nil || code != CodeOK {
		t.Fatalf("write: %v %v", code, err)
	}
	code, err = ParseWrite(Frame{Data: []byte{0x04, 0x07, 0, 0}})
	if err != nil || code != CodeInvalidAccess {
		t.Fatalf("write error: %v %v", code, err)
	}
	if _, err := ParseWrite(Frame{Data: []byte{1, 2}}); err == nil {
		t.Fatal("short write response accepted")
	}
}

func TestParseDeviceInfo(t *testing.T) {
	p := make([]byte, 24)
	copy(p[8:24], []byte("UIC PLC sim\x00\x00\x00\x00\x00"))
	p[4], p[5] = 3, 1
	binary.LittleEndian.PutUint16(p[6:8], 4024)
	info, code, err := ParseReadDeviceInfo(Frame{Data: p})
	if err != nil || code != CodeOK {
		t.Fatalf("info: %v %v", err, err)
	}
	if info.Name != "UIC PLC sim" || info.VersionString() != "3.1.4024" {
		t.Fatalf("info = %+v", info)
	}
	if _, _, err := ParseReadDeviceInfo(Frame{Data: p[:16]}); err == nil {
		t.Fatal("short device info accepted")
	}
}

func TestParseReadState(t *testing.T) {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint16(p[4:6], uint16(StateRun))
	binary.LittleEndian.PutUint16(p[6:8], 7)
	st, code, err := ParseReadState(Frame{Data: p})
	if err != nil || code != CodeOK {
		t.Fatal(err)
	}
	if st.ADS != StateRun || st.Device != 7 {
		t.Fatalf("state = %+v", st)
	}
}

func TestCodeKind(t *testing.T) {
	cases := []struct {
		code Code
		kind driver.ErrorKind
		temp bool
	}{
		{CodeOK, "", false},
		{CodeTargetPortNotFound, driver.ErrAddress, false},
		{CodeInvalidGroup, driver.ErrAddress, false},
		{CodeSymbolNotFound, driver.ErrAddress, false},
		{CodeInvalidAccess, driver.ErrPermission, false},
		{CodeServiceNotSupported, driver.ErrUnsupported, false},
		{CodeTargetMachineNotFound, driver.ErrDisconnected, true},
		{CodeAMSTimeout, driver.ErrTimeout, true},
		{CodeMailboxFull, driver.ErrProtocol, true},
		{CodeDeviceError, driver.ErrProtocol, true},
		{Code(0x9999), driver.ErrProtocol, false},
	}
	for _, c := range cases {
		if got := c.code.Kind(); got != c.kind {
			t.Fatalf("%v kind = %v, want %v", c.code, got, c.kind)
		}
		if got := c.code.Temporary(); got != c.temp {
			t.Fatalf("%v temporary = %v, want %v", c.code, got, c.temp)
		}
	}
	if got := CodeInvalidGroup.String(); !strings.Contains(got, "0x00000702") || !strings.Contains(got, "index group") {
		t.Fatalf("String = %q", got)
	}
}

func TestCommandAndStateStrings(t *testing.T) {
	if CmdReadWrite.String() != "ReadWrite" || Command(99).String() != "Command(99)" {
		t.Fatal("command string")
	}
	if StateRun.String() != "Run" || State(99).String() != "State(99)" {
		t.Fatal("state string")
	}
	for _, c := range []Command{CmdReadDeviceInfo, CmdRead, CmdWrite, CmdReadState,
		CmdWriteControl, CmdAddDeviceNotification, CmdDeleteDeviceNotification,
		CmdDeviceNotification} {
		if strings.HasPrefix(c.String(), "Command(") {
			t.Fatalf("command %d has no name", uint16(c))
		}
	}
	if got := Code(0x1234).String(); got != "0x00001234" {
		t.Fatalf("unknown code String = %q", got)
	}
}

// 句柄读写的 IndexGroup/Offset 必须落在正确位置：设备不会替我们纠正
// 字段顺序，只会返回 invalid group 或读到一个不存在的句柄。
func TestAccessorPayloads(t *testing.T) {
	p := Peer{}
	rd, err := p.ReadByHandle(1, 0x00010002, 8).Encode()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Decode(rd)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(f.Data[0:4]); got != IGSymValueByHandle {
		t.Fatalf("read group = 0x%X", got)
	}
	if got := binary.LittleEndian.Uint32(f.Data[4:8]); got != 0x00010002 {
		t.Fatalf("read handle = 0x%X", got)
	}
	if got := binary.LittleEndian.Uint32(f.Data[8:12]); got != 8 {
		t.Fatalf("read length = %d", got)
	}

	wr, err := p.WriteByHandle(2, 0x00010003, []byte{1, 2, 3}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	f, err = Decode(wr)
	if err != nil {
		t.Fatal(err)
	}
	if f.Command != CmdWrite || binary.LittleEndian.Uint32(f.Data[4:8]) != 0x00010003 {
		t.Fatalf("write header = %v handle=%#x", f.Command, binary.LittleEndian.Uint32(f.Data[4:8]))
	}
	if got := int(binary.LittleEndian.Uint32(f.Data[8:12])); got != 3 || len(f.Data) != 15 {
		t.Fatalf("write length = %d payload=%d", got, len(f.Data))
	}

	rel, err := p.ReleaseHandle(3, 0x00010004).Encode()
	if err != nil {
		t.Fatal(err)
	}
	f, err = Decode(rel)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(f.Data[0:4]) != IGSymReleaseHandle ||
		binary.LittleEndian.Uint32(f.Data[12:16]) != 0x00010004 {
		t.Fatalf("release payload = % x", f.Data)
	}
}

func TestParseAliases(t *testing.T) {
	p := make([]byte, 8+1)
	binary.LittleEndian.PutUint32(p[4:8], 1)
	p[8] = 7
	if data, code, err := ParseReadWrite(Frame{Data: p}); err != nil || code != CodeOK || data[0] != 7 {
		t.Fatalf("readwrite alias: %v %v % x", err, code, data)
	}
	if code, err := ParseWriteControl(Frame{Data: []byte{0, 0, 0, 0}}); err != nil || code != CodeOK {
		t.Fatalf("writecontrol alias: %v %v", code, err)
	}
	if !(Frame{Flags: FlagResponse | FlagADS}).IsResponse() {
		t.Fatal("IsResponse")
	}
	if (Frame{}).IsResponse() {
		t.Fatal("IsResponse on request")
	}
	if !(NetID{}).IsZero() || (NetID{1}).IsZero() {
		t.Fatal("IsZero")
	}
	if _, err := (Frame{Command: CmdInvalid}).Encode(); err == nil {
		t.Fatal("command 0 accepted")
	}
	if _, err := (Frame{Command: CmdRead, Data: make([]byte, MaxDatagram+1)}).Encode(); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, _, err := ParseReadState(Frame{Data: []byte{0, 0, 0, 0}}); err == nil {
		t.Fatal("short state accepted")
	}
	if _, code, err := ParseReadState(Frame{Data: []byte{2, 7, 0, 0}}); err != nil || code != CodeInvalidGroup {
		t.Fatalf("error state should not be malformed: %v %v", code, err)
	}
}
