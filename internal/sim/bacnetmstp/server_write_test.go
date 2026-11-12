package bacnetmstp

import (
	"context"
	"testing"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	drvmstp "uic/internal/driver/bacnetmstp"
	"uic/internal/model"
)

// openDriverTo 起一个 sim 从站并把驱动接到它上面，返回连接。
func openDriverTo(t *testing.T, srv *Server) driver.Conn {
	t.Helper()
	ch := model.Channel{
		ID: "ch", Protocol: "bacnet-mstp",
		Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
		Timeout:   model.Duration(time.Second),
	}
	dev := model.Device{ID: "dev", ChannelID: "ch", Interval: model.Duration(time.Second),
		Enabled: true, UnitID: 128}
	c, err := drvmstp.New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// 写路径覆盖 writeProperty / coerce / decodeIndex：可写模拟量、字符串、
// 以及数组下标越界与只读属性。
func TestWritePropertyPaths(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	srv.DisableAutoIncrement()
	c := openDriverTo(t, srv)
	ctx := context.Background()

	write := func(id string, addr model.Address, dt model.DataType, v model.Value) ([]driver.WriteResult, error) {
		return c.Write(ctx, []driver.WriteRequest{{PointID: id, Address: addr, DataType: dt, Value: v}})
	}

	// 可写模拟量。
	av := model.Address{Kind: model.AddressObject, ObjectType: "analog-value", Instance: 2, Property: "present-value"}
	res, err := write("av", av, model.TypeFloat32, model.NumValue(model.TypeFloat32, 33.5))
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("write analog-value: %v %+v", err, res)
	}
	if v, _ := srv.Value("analog-value", 2, "present-value"); v != 33.5 {
		t.Fatalf("analog-value = %v", v)
	}

	// 可写字符串。
	cs := model.Address{Kind: model.AddressObject, ObjectType: "character-string", Instance: 1, Property: "present-value"}
	if _, err := write("cs", cs, model.TypeString, model.TextValue(model.TypeString, "FAN-09")); err != nil {
		t.Fatalf("write string: %v", err)
	}
	if v, _ := srv.Value("character-string", 1, "present-value"); v != "FAN-09" {
		t.Fatalf("character-string = %v", v)
	}

	// 只读属性。
	ai := model.Address{Kind: model.AddressObject, ObjectType: "analog-input", Instance: 1, Property: "present-value"}
	if _, err := write("ai", ai, model.TypeFloat32, model.NumValue(model.TypeFloat32, 1)); err == nil {
		t.Fatal("write to read-only property succeeded")
	}

	// 数组下标越界：priority-array 只有 16 级。
	pa := model.Address{Kind: model.AddressObject, ObjectType: "analog-value", Instance: 2,
		Property: "priority-array", PropIndex: 17}
	if _, err := write("pa", pa, model.TypeFloat32, model.NumValue(model.TypeFloat32, 1)); err == nil {
		t.Fatal("write past the end of the priority array succeeded")
	}

	// 不存在的对象。
	miss := model.Address{Kind: model.AddressObject, ObjectType: "analog-value", Instance: 99, Property: "present-value"}
	if _, err := write("miss", miss, model.TypeFloat32, model.NumValue(model.TypeFloat32, 1)); err == nil {
		t.Fatal("write to a missing object succeeded")
	}
}

// 直接覆盖 setProp / SetValue / 各种 Set* 便捷方法。
func TestSimSetters(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	srv.DisableAutoIncrement()

	if err := srv.SetAnalog(1, 11.5); err != nil {
		t.Fatalf("SetAnalog: %v", err)
	}
	if v, _ := srv.Value("analog-input", 1, "present-value"); v != 11.5 {
		t.Fatalf("AI-1 = %v", v)
	}
	if err := srv.SetAnalogValue(2, 7.5); err != nil {
		t.Fatalf("SetAnalogValue: %v", err)
	}
	if v, _ := srv.Value("analog-value", 2, "present-value"); v != 7.5 {
		t.Fatalf("AV-2 = %v", v)
	}
	if err := srv.SetBinary(1, false); err != nil {
		t.Fatalf("SetBinary: %v", err)
	}
	if v, _ := srv.Value("binary-input", 1, "present-value"); v != uint64(0) {
		t.Fatalf("BI-1 = %v", v)
	}
	if err := srv.SetString(1, "PUMP-77"); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	if v, _ := srv.Value("character-string", 1, "present-value"); v != "PUMP-77" {
		t.Fatalf("CS-1 = %v", v)
	}

	// SetValue 的多种值类型。
	cases := []struct {
		name  string
		value any
	}{
		{"float", float64(1.5)},
		{"float32", float32(2.5)},
		{"int", 3},
		{"int32", int32(-4)},
		{"uint64", uint64(5)},
		{"bool", true},
		{"string", "x"},
		{"bytes", []byte{0x01, 0x02}},
	}
	for _, tc := range cases {
		if err := srv.SetValue("analog-value", 9, "present-value", tc.value); err != nil {
			t.Fatalf("SetValue %s: %v", tc.name, err)
		}
		if _, ok := srv.Value("analog-value", 9, "present-value"); !ok {
			t.Fatalf("SetValue %s: value not stored", tc.name)
		}
	}
	if err := srv.SetValue("no-such", 1, "present-value", 1); err == nil {
		t.Fatal("SetValue accepted an unknown object type")
	}
	if err := srv.SetValue("analog-value", 1, "no-such", 1); err == nil {
		t.Fatal("SetValue accepted an unknown property")
	}
	if err := srv.SetValue("analog-value", 1, "present-value", struct{}{}); err == nil {
		t.Fatal("SetValue accepted an unsupported value type")
	}
}

// readProperty 的下标越界、未知对象、未知属性都要回 Error（而不是静默 nil）。
func TestReadPropertyErrorPaths(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	mkReq := func(objType, instance, prop, index int) []byte {
		req, err := bacnetcodec.ReadPropertyRequest(1, objType, instance, prop, index)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		return req
	}
	isError := func(apdu []byte) bool {
		return len(apdu) > 0 && apdu[0]&0xF0 == 0x50
	}

	if !isError(srv.handleAPDU(mkReq(bacnetcodec.ObjectAnalogInput, 999, bacnetcodec.PropPresentValue, 0))) {
		t.Fatal("unknown object did not return Error")
	}
	if !isError(srv.handleAPDU(mkReq(bacnetcodec.ObjectAnalogInput, 1, 999, 0))) {
		t.Fatal("unknown property did not return Error")
	}
	// 对标量属性带下标。
	if !isError(srv.handleAPDU(mkReq(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, 2))) {
		t.Fatal("index on scalar property did not return Error")
	}
	// 优先级数组下标越界。
	if !isError(srv.handleAPDU(mkReq(bacnetcodec.ObjectAnalogValue, 2, bacnetcodec.PropPriorityArray, 99))) {
		t.Fatal("priority-array index out of range did not return Error")
	}
}

// 非法 ConfirmedRequest（参数被截断）应回 Reject，不应 panic。
func TestHandleAPDUMalformedParams(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	// 声明 ReadProperty 但没有对象/属性参数。
	apdu := []byte{0x00, 0x00, 0x01, bacnetcodec.ServiceReadProperty}
	if got := srv.handleAPDU(apdu); got == nil {
		t.Fatal("malformed request dropped silently")
	}
	// 更短的 APDU 直接解析失败，应丢弃（nil）而不是 panic。
	if got := srv.handleAPDU([]byte{0x00}); got != nil {
		t.Fatalf("truncated APDU -> % X", got)
	}
}
