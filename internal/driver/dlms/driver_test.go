package dlms

import (
	"context"
	"fmt"
	"testing"
	"time"

	dl "uic/internal/codec/dlms"
	"uic/internal/driver"
	"uic/internal/model"
	simdlms "uic/internal/sim/dlms"
)

// 端到端：建链（SNRM + AARQ）→ 逐点 Get → 断链。
//
// 这条用例覆盖三层全部真实字节：主站组帧 → TCP → 从站解帧 → 应答 →
// 主站解帧。任何一层偏一字节都会在这里失败，而不是等到现场。
func TestEndToEndRead(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{
			ID:       "meter",
			Interval: model.Duration(0),
			Labels:   map[string]string{"options": "client=16,server=1,addr-size=1,retries=1,gap=1ms"},
		},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(3 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	// 三个点位：电能（uint32）、电压（uint16 + scaler）、表号（octet-string）。
	// OBIS 的六段是**十进制**数：1.0.32.7.0.255 编码为 01 00 20 07 00 FF，
	// 而不是 01 00 32 07 00 FF（0x32 = 50，那是另一个对象）。
	obisEnergy := mustOBIS(t, "1.0.1.8.0.255")
	obisVolt := mustOBIS(t, "1.0.32.7.0.255")
	obisSerial := mustOBIS(t, "0.0.96.1.0.255")

	spans := []driver.ReadSpan{{
		ID: "all",
		Points: []driver.PointRef{
			{ID: "energy", Address: model.Address{Kind: model.AddressSymbol, Path: obisEnergy.String()}},
			{ID: "voltage", Address: model.Address{Kind: model.AddressSymbol, Path: obisVolt.String()}},
			{ID: "serial", Address: model.Address{Kind: model.AddressSymbol, Path: obisSerial.String()}},
		},
	}}
	samples, err := conn.Read(context.Background(), spans)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("样本数 = %d, want 3", len(samples))
	}
	got := map[string]driver.RawSample{}
	for _, s := range samples {
		got[s.PointID] = s
	}
	for _, id := range []string{"energy", "voltage", "serial"} {
		if got[id].Quality != model.QualityGood {
			t.Fatalf("%s: quality = %s, err = %v", id, got[id].Quality, got[id].Err)
		}
	}
	if n, _ := got["energy"].Value.AsFloat(); n != 12345678 {
		t.Errorf("电能 = %v, want 12345678", n)
	}
	// 电压在表上以 0.1 V 为单位存储（attribute 3 的 scaler=-1），驱动必须
	// 读出 attribute 3 并应用缩放：2201 × 10^-1 = 220.1。若驱动只报原始值，
	// 现场会看到 2201 V —— 数值"看起来是个数"，不会报错。
	if n, _ := got["voltage"].Value.AsFloat(); n != 220.1 {
		t.Errorf("电压 = %v, want 220.1（已应用 scaler=-1）", n)
	}
	if got["serial"].Value.Text != "12345678" {
		t.Errorf("表号 = %q, want 12345678", got["serial"].Value.Text)
	}

	// SNRM/AARQ 各一次；GET = 3 个点位 + 2 次 scaler_unit（电能与电压是
	// 两个不同的 Register 对象，各需要读一次 attribute 3）。
	//
	// 表号是 class 1 Data，没有 scaler_unit，因此不产生额外往返。
	snrm, aarq, get, _ := srv.Counts()
	if snrm != 1 {
		t.Errorf("SNRM 次数 = %d", snrm)
	}
	if aarq != 1 {
		t.Errorf("AARQ 次数 = %d", aarq)
	}
	if get != 5 {
		t.Errorf("GET 次数 = %d, want 5（3 点位 + 2 scaler_unit）", get)
	}

	// 第二轮不该再读 scaler_unit（已缓存）：总往返回落到 3。
	before := get
	if _, err := conn.Read(context.Background(), spans); err != nil {
		t.Fatalf("第二轮 Read: %v", err)
	}
	_, _, after, _ := srv.Counts()
	if got := after - before; got != 3 {
		t.Errorf("第二轮 GET 次数 = %d, want 3（scaler 已缓存）", got)
	}
}

// 建链失败：从站拒绝关联时，Open 必须报协议错误而不是超时。
func TestOpenRejectsAssociation(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()
	srv.SetRejectAssociation(true)

	d := New()
	_, err = d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err == nil {
		t.Fatal("拒绝关联时 Open 应失败")
	}
	if k := driver.KindOf(err); k != driver.ErrProtocol {
		t.Fatalf("错误分类 = %s, want protocol（err=%v）", k, err)
	}
}

// 地址错：OBIS 不存在时归 ErrAddress，而不是链路错误。
func TestReadUnknownOBIS(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	// 显式类号 + 一个表上不存在的 OBIS。
	spans := []driver.ReadSpan{{
		ID: "s",
		Points: []driver.PointRef{{
			ID: "nope",
			Address: model.Address{Kind: model.AddressSymbol,
				Path: "1.0.99.99.0.255:3:2"},
		}},
	}}
	samples, _ := conn.Read(context.Background(), spans)
	if len(samples) != 1 {
		t.Fatalf("样本数 = %d", len(samples))
	}
	if samples[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s, want address_error", samples[0].Quality)
	}
	if k := driver.KindOf(samples[0].Err); k != driver.ErrAddress {
		t.Fatalf("错误分类 = %s, want address", k)
	}
}

// 类号错：同一个 OBIS 换了类号必须报 object-class-inconsistent。
func TestReadClassMismatch(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	// 电能是 class 3；故意写 class 1。
	spans := []driver.ReadSpan{{
		ID: "s",
		Points: []driver.PointRef{{
			ID: "wrongclass",
			Address: model.Address{Kind: model.AddressSymbol,
				Path: "1.0.1.8.0.255:1:2"},
		}},
	}}
	samples, _ := conn.Read(context.Background(), spans)
	if len(samples) != 1 {
		t.Fatalf("样本数 = %d", len(samples))
	}
	if samples[0].Quality != model.QualityAddressError {
		t.Fatalf("quality = %s, want address_error", samples[0].Quality)
	}
	if samples[0].Err == nil {
		t.Fatal("应有错误说明")
	}
	// 错误文本里应出现「类不一致」这个可操作的提示。
	if got := samples[0].Err.Error(); !contains(got, "类不一致") {
		t.Fatalf("错误信息未指出类号不一致: %s", got)
	}
}

// 静默从站：必须报超时，且不能被误判成"地址错误"。
func TestReadSilentMeter(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()
	srv.SetSilent(true)

	d := New()
	_, err = d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
	})
	if err == nil {
		t.Fatal("静默从站时 Open 应超时失败")
	}
	if k := driver.KindOf(err); k != driver.ErrTimeout {
		t.Fatalf("错误分类 = %s, want timeout（err=%v）", k, err)
	}
}

// Probe 在已建链时应直接成功（不重复 AARQ）。
func TestProbeReusesAssociation(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	if err := conn.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	_, aarq, _, _ := srv.Counts()
	if aarq != 1 {
		t.Fatalf("Probe 不应重复 AARQ（次数=%d）", aarq)
	}
}

// Write 必须明确声明不支持，而不是静默成功。
func TestWriteUnsupported(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	res, err := conn.Write(context.Background(), []driver.WriteRequest{{PointID: "x"}})
	if err != nil {
		t.Fatalf("Write 应返回结果而不是错误: %v", err)
	}
	if len(res) != 1 || driver.KindOf(res[0].Err) != driver.ErrUnsupported {
		t.Fatalf("Write 结果 = %+v", res)
	}
}

// Meta 的能力声明必须诚实：Write/Subscribe/MultiRead 都是 unsupported。
func TestMetaHonest(t *testing.T) {
	m := New().Meta()
	if m.Capabilities.Read != driver.CapSupported {
		t.Error("Read 应 supported")
	}
	for name, st := range map[string]driver.CapState{
		"Write":     m.Capabilities.Write,
		"Subscribe": m.Capabilities.Subscribe,
		"MultiRead": m.Capabilities.MultiRead,
		"BitAccess": m.Capabilities.BitAccess,
	} {
		if st != driver.CapUnsupported {
			t.Errorf("%s = %s, want unsupported", name, st)
		}
	}
	if m.Capabilities.StructTypes != driver.CapSupported {
		t.Error("StructTypes 应 supported（scaler_unit 是结构体）")
	}
}

// 地址解析：只写 OBIS 时从内置表推导类号；不在表里则明确报错。
func TestAttributeOf(t *testing.T) {
	a, err := attributeOf(model.Address{Kind: model.AddressSymbol, Path: "1.0.1.8.0.255"})
	if err != nil {
		t.Fatalf("attributeOf: %v", err)
	}
	if a.ClassID != dl.ClassRegister || a.Attribute != dl.AttrValue {
		t.Fatalf("推导结果 = class %d attr %d", a.ClassID, a.Attribute)
	}
	if a.InstanceID != [6]byte{0x01, 0x00, 0x01, 0x08, 0x00, 0xFF} {
		t.Fatalf("OBIS = % X", a.InstanceID)
	}

	// 显式类号与属性号。
	b, err := attributeOf(model.Address{Kind: model.AddressSymbol, Path: "0.0.42.0.0.255:1:2"})
	if err != nil {
		t.Fatalf("attributeOf: %v", err)
	}
	if b.ClassID != dl.ClassData || b.Attribute != dl.AttrValue {
		t.Fatalf("显式 = class %d attr %d", b.ClassID, b.Attribute)
	}

	// 私有 OBIS 未给类号：必须报地址错误。
	_, err = attributeOf(model.Address{Kind: model.AddressSymbol, Path: "1.0.99.1.0.255"})
	if err == nil {
		t.Fatal("私有 OBIS 未给类号应报错")
	}
	if k := driver.KindOf(err); k != driver.ErrAddress {
		t.Fatalf("错误分类 = %s", k)
	}
}

// 断链：Close 后从站应收到 DISC。
func TestCloseSendsDisc(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, _, disc := srv.Counts()
	if disc != 1 {
		t.Fatalf("DISC 次数 = %d, want 1", disc)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = fmt.Sprintf

// mustOBIS 解析 OBIS 字符串，失败即测试失败。
func mustOBIS(t *testing.T, s string) dl.OBIS {
	t.Helper()
	o, err := dl.ParseOBIS(s)
	if err != nil {
		t.Fatalf("ParseOBIS(%q): %v", s, err)
	}
	return o
}
