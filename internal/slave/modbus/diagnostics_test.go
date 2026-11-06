package modbus

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	mb "uic/internal/codec/modbus"
	"uic/internal/model"
)

// ---------------------------------------------------------------------------
// 诊断与识别（FC8 / FC11 / FC17 / FC43）
//
// 这四类功能码不碰映射表，所以测试也不该依赖映射：它们验证的是「从站在
// 上位机眼里是不是一台可识别的设备」。
// ---------------------------------------------------------------------------

// diagSlave 建一个最小从站（一个空单元即可，诊断类请求不需要映射）。
func diagSlave(t *testing.T, id *model.SlaveIdentity) *Server {
	t.Helper()
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	s := newServer(t, model.Slave{
		ID: "s", Identity: id,
		Units: []model.SlaveUnit{{
			UnitID:   1,
			Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
		}},
	}, points, &fakeSource{}, &fakeWriter{})
	return s
}

// newServerWithRevision 允许指定上报版本，用来验证 runtime 的接线。
func newServerWithRevision(t *testing.T, revision string) *Server {
	t.Helper()
	points := map[string]model.Point{"plc/a": pt(model.TypeUint16, 1, false)}
	s, err := New(Options{
		Slave: model.Slave{ID: "s", Listen: "127.0.0.1:0", Units: []model.SlaveUnit{{
			UnitID:   1,
			Mappings: []model.SlaveMapping{{Point: "plc/a", Area: model.MappingHolding, Offset: 0}},
		}}},
		Points: points, Source: &fakeSource{}, Writer: &fakeWriter{},
		WriteTimeout: time.Second, Revision: revision,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func fc8(sub, data uint16) []byte {
	pdu := []byte{mb.FC08, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:3], sub)
	binary.BigEndian.PutUint16(pdu[3:5], data)
	return pdu
}

// parseFC43 解析读设备标识响应。[0]=fc [1]=mei [2]=code [3]=conformity
// [4]=more [5]=next [6]=count，之后是 (id, len, value)*。
func parseFC43(t *testing.T, resp []byte) map[byte]string {
	t.Helper()
	if len(resp) < 7 || resp[0] != mb.FC43 {
		t.Fatalf("response = % x", resp)
	}
	out := map[byte]string{}
	body := resp[7:]
	for i := 0; i < int(resp[6]); i++ {
		if len(body) < 2 {
			t.Fatalf("truncated object header in % x", resp)
		}
		id, n := body[0], int(body[1])
		if len(body) < 2+n {
			t.Fatalf("truncated object value in % x", resp)
		}
		out[id] = string(body[2 : 2+n])
		body = body[2+n:]
	}
	if len(body) != 0 {
		t.Fatalf("trailing bytes in % x", resp)
	}
	return out
}

// --- FC8 -------------------------------------------------------------------

func TestFC8Echo(t *testing.T) {
	c := dial(t, diagSlave(t, nil), 1)
	resp := c.roundTrip(t, 1, fc8(0x0000, 0xABCD))
	want := []byte{mb.FC08, 0x00, 0x00, 0xAB, 0xCD}
	if !bytes.Equal(resp, want) {
		t.Fatalf("echo = % x, want % x", resp, want)
	}
}

func TestFC8CountsAndClear(t *testing.T) {
	c := dial(t, diagSlave(t, nil), 1)

	// 先制造一次异常，让 exception 计数非零。
	expectException(t, c.roundTrip(t, 1, fc8(0x0099, 0)), mb.FC08, 0x01)
	// 再发几次正常请求（0x0000 回显）。
	for i := 0; i < 3; i++ {
		c.roundTrip(t, 1, fc8(0x0000, 1))
	}

	msgCount := func() uint16 {
		resp := c.roundTrip(t, 1, fc8(0x000B, 0))
		if len(resp) != 5 || resp[0] != mb.FC08 || resp[1] != 0x00 || resp[2] != 0x0B {
			t.Fatalf("bus message count response = % x", resp)
		}
		return binary.BigEndian.Uint16(resp[3:5])
	}
	if got := msgCount(); got == 0 {
		t.Fatalf("bus message count = 0 after 5 requests")
	}

	exc := c.roundTrip(t, 1, fc8(0x000D, 0))
	if got := binary.BigEndian.Uint16(exc[3:5]); got == 0 {
		t.Fatalf("bus exception count = 0 after an exception")
	}

	// 清计数：协议计数（requests/exceptions/events）归零；指标不动。
	before := c.roundTrip(t, 1, fc8(0x000A, 0))
	if len(before) != 5 || before[1] != 0x00 || before[2] != 0x0A {
		t.Fatalf("clear response = % x", before)
	}
	// 清完之后的下一条消息就是这次查询本身，因此计数为 1。
	if got := msgCount(); got != 1 {
		t.Fatalf("bus message count after clear = %d, want 1 (the query itself)", got)
	}
	if got := c.roundTrip(t, 1, fc8(0x000D, 0)); binary.BigEndian.Uint16(got[3:5]) != 0 {
		t.Fatalf("exception count after clear = %d, want 0", binary.BigEndian.Uint16(got[3:5]))
	}
	// TCP 上这些计数恒为 0：它们描述的是串口的字符层/CRC 错误。
	for _, sub := range []uint16{0x000C, 0x000F, 0x0010, 0x0011, 0x0012} {
		resp := c.roundTrip(t, 1, fc8(sub, 0))
		if got := binary.BigEndian.Uint16(resp[3:5]); got != 0 {
			t.Fatalf("sub %#04x = %d, want 0 on TCP", sub, got)
		}
	}
}

func TestFC8RejectsBadRequests(t *testing.T) {
	c := dial(t, diagSlave(t, nil), 1)
	// 未知子功能。
	expectException(t, c.roundTrip(t, 1, fc8(0x00FF, 0)), mb.FC08, 0x01)
	// 报文太短（诊断请求固定 5 字节）。
	expectException(t, c.roundTrip(t, 1, []byte{mb.FC08, 0x00, 0x00}), mb.FC08, 0x03)
}

// --- FC11 ------------------------------------------------------------------

func TestFC11EventCounter(t *testing.T) {
	c := dial(t, diagSlave(t, nil), 1)

	read := func() uint16 {
		resp := c.roundTrip(t, 1, []byte{mb.FC11})
		if len(resp) != 5 || resp[0] != mb.FC11 {
			t.Fatalf("fc11 response = % x", resp)
		}
		if got := binary.BigEndian.Uint16(resp[1:3]); got != 0 {
			t.Fatalf("status = %d, want 0 (no pending event)", got)
		}
		return binary.BigEndian.Uint16(resp[3:5])
	}

	// FC11 自身不计数：连续两次查询必须得到同一个值。
	first := read()
	second := read()
	if first != second {
		t.Fatalf("FC11 changed the counter: %d then %d", first, second)
	}

	// 别的成功请求会让计数增长。
	c.roundTrip(t, 1, fc8(0x0000, 1))
	c.roundTrip(t, 1, fc8(0x0000, 2))
	if got := read(); got != second+2 {
		t.Fatalf("event counter = %d, want %d", got, second+2)
	}

	// 异常响应也算一条被处理过的消息吗？按规范「成功处理」才计数，
	// 所以异常不增加计数。这里把这条语义固定下来。
	before := read()
	expectException(t, c.roundTrip(t, 1, fc8(0x00FF, 0)), mb.FC08, 0x01)
	if got := read(); got != before {
		t.Fatalf("event counter = %d after an exception, want %d", got, before)
	}

	// 长度不对的 FC11 是协议错误。
	expectException(t, c.roundTrip(t, 1, []byte{mb.FC11, 0x00}), mb.FC11, 0x03)
}

// --- FC17 ------------------------------------------------------------------

func TestFC17ReportServerID(t *testing.T) {
	c := dial(t, diagSlave(t, nil), 1)
	resp := c.roundTrip(t, 1, []byte{mb.FC17})
	if len(resp) < 3 || resp[0] != mb.FC17 {
		t.Fatalf("response = % x", resp)
	}
	body := resp[2:]
	if int(resp[1]) != len(body) {
		t.Fatalf("byte count = %d, payload = %d", resp[1], len(body))
	}
	if body[len(body)-1] != 0xFF {
		t.Fatalf("run indicator = %#x, want 0xFF (ON)", body[len(body)-1])
	}
	if !strings.Contains(string(body[:len(body)-1]), "uic-slave") {
		t.Fatalf("product code = %q", body[:len(body)-1])
	}

	// 配置覆盖后必须如实上报。
	s := diagSlave(t, &model.SlaveIdentity{ProductCode: "ACME-PLC"})
	resp = dial(t, s, 1).roundTrip(t, 1, []byte{mb.FC17})
	if got := string(resp[2 : len(resp)-1]); got != "ACME-PLC" {
		t.Fatalf("product code = %q", got)
	}

	expectException(t, dial(t, s, 1).roundTrip(t, 1, []byte{mb.FC17, 0x00}), mb.FC17, 0x03)
}

// --- FC43 ------------------------------------------------------------------

func fc43(code byte, objectID byte) []byte {
	return []byte{mb.FC43, 0x0E, code, objectID}
}

func TestFC43BasicRegularExtended(t *testing.T) {
	id := &model.SlaveIdentity{
		VendorName: "ACME", ProductCode: "PLC-2000", MajorMinorRevision: "2.3",
		VendorURL: "https://acme.example", ProductName: "ACME Controller",
		ModelName: "PLC-2000-X", UserApplicationName: "Line 3",
	}
	c := dial(t, diagSlave(t, id), 1)

	basic := c.roundTrip(t, 1, fc43(1, 0))
	objs := parseFC43(t, basic)
	// Conformity 描述设备能力（extended + individual access），与本次请求
	// 的访问级别无关。
	if basic[3] != 0x83 {
		t.Fatalf("conformity = %#x, want 0x83", basic[3])
	}
	if objs[0x00] != "ACME" || objs[0x01] != "PLC-2000" || objs[0x02] != "2.3" {
		t.Fatalf("basic objects = %v", objs)
	}
	if _, ok := objs[0x04]; ok {
		t.Fatalf("basic must not include regular objects: %v", objs)
	}

	regular := c.roundTrip(t, 1, fc43(2, 0))
	objs = parseFC43(t, regular)
	if regular[3] != 0x83 {
		t.Fatalf("conformity = %#x, want 0x83", regular[3])
	}
	if objs[0x03] != "https://acme.example" || objs[0x04] != "ACME Controller" || objs[0x05] != "PLC-2000-X" {
		t.Fatalf("regular objects = %v", objs)
	}

	extended := c.roundTrip(t, 1, fc43(3, 0))
	objs = parseFC43(t, extended)
	if extended[3] != 0x83 {
		t.Fatalf("conformity = %#x, want 0x83", extended[3])
	}
	if objs[0x06] != "Line 3" {
		t.Fatalf("extended objects = %v", objs)
	}
}

// TestFC43Defaults 验证「没配 identity 也能被识别」：basic 三件套必须有值。
func TestFC43Defaults(t *testing.T) {
	c := dial(t, diagSlave(t, nil), 1)
	objs := parseFC43(t, c.roundTrip(t, 1, fc43(1, 0)))
	for _, id := range []byte{0x00, 0x01, 0x02} {
		if objs[id] == "" {
			t.Fatalf("default object %#x is empty: %v", id, objs)
		}
	}
}

func TestFC43SpecificObject(t *testing.T) {
	c := dial(t, diagSlave(t, &model.SlaveIdentity{VendorName: "ACME"}), 1)
	resp := c.roundTrip(t, 1, fc43(4, 0x00))
	objs := parseFC43(t, resp)
	if len(objs) != 1 || objs[0x00] != "ACME" {
		t.Fatalf("specific objects = %v", objs)
	}
	// 不存在的对象 id：0x02（非法数据地址）。
	expectException(t, c.roundTrip(t, 1, fc43(4, 0x63)), mb.FC43, 0x02)
	// 非法访问级别：0x03（非法数据值）。
	expectException(t, c.roundTrip(t, 1, fc43(9, 0)), mb.FC43, 0x03)
	// 未知 MEI：0x01（非法功能）。
	expectException(t, c.roundTrip(t, 1, []byte{mb.FC43, 0x0D, 0x01, 0x00}), mb.FC43, 0x01)
	// 短报文。
	expectException(t, c.roundTrip(t, 1, []byte{mb.FC43, 0x0E, 0x01}), mb.FC43, 0x03)
}

// TestFC43Pagination 验证响应放不进 253 字节 PDU 时按 more/nextObjectID 分页。
func TestFC43Pagination(t *testing.T) {
	long := strings.Repeat("A", 120)
	id := &model.SlaveIdentity{
		VendorName: long, ProductCode: long, MajorMinorRevision: strings.Repeat("B", 32),
		VendorURL: long, ProductName: long, ModelName: long, UserApplicationName: long,
	}
	c := dial(t, diagSlave(t, id), 1)

	resp := c.roundTrip(t, 1, fc43(3, 0))
	if len(resp) > 253 {
		t.Fatalf("PDU too large: %d bytes", len(resp))
	}
	if resp[4] != 0xFF {
		t.Fatalf("more = %#x, want 0xFF (paged)", resp[4])
	}
	next := resp[5]
	if next == 0 {
		t.Fatal("nextObjectID is 0")
	}
	seen := parseFC43(t, resp)

	// 顺着 nextObjectID 续读，直到 more=0；所有对象必须恰好出现一次。
	for pages := 0; resp[4] != 0; pages++ {
		if pages > 8 {
			t.Fatal("pagination does not terminate")
		}
		resp = c.roundTrip(t, 1, fc43(3, next))
		if len(resp) > 253 {
			t.Fatalf("PDU too large: %d bytes", len(resp))
		}
		for k, v := range parseFC43(t, resp) {
			if _, dup := seen[k]; dup {
				t.Fatalf("object %#x returned twice", k)
			}
			seen[k] = v
		}
		next = resp[5]
	}
	if len(seen) != 7 {
		t.Fatalf("objects = %d, want 7: %v", len(seen), seen)
	}
	if seen[0x06] != long {
		t.Fatalf("application name truncated")
	}
}

// TestFC43ReportsRevision 验证 Revision 选项被如实上报——runtime 用它把
// 构建版本传给从站。
func TestFC43ReportsRevision(t *testing.T) {
	c := dial(t, newServerWithRevision(t, "9.9.9-rc1"), 1)
	objs := parseFC43(t, c.roundTrip(t, 1, fc43(1, 0)))
	if objs[0x02] != "9.9.9-rc1" {
		t.Fatalf("revision = %q", objs[0x02])
	}
}
