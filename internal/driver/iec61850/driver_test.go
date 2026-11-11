package iec61850

import (
	"context"
	"strings"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	iecsim "uic/internal/sim/iec61850"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

func testChannel(addr string) model.Channel {
	return model.Channel{
		ID: "ch", Protocol: "iec61850",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
}

func newSim(t *testing.T) *iecsim.Server {
	t.Helper()
	srv, err := iecsim.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// openSim 打开一个连到内置 IED 的驱动连接。mut 可改设备（如设 labels.defaultFC）。
func openSim(t *testing.T, srv *iecsim.Server, mut func(*model.Device)) *conn {
	t.Helper()
	dev := model.Device{ID: "ied1", ChannelID: "ch"}
	if mut != nil {
		mut(&dev)
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device:  dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// ref 构造一个 IEC 61850 点位视图：对象引用 + 平台类型。
func ref(id, objectRef string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID:       id,
		Address:  model.Address{Kind: model.AddressSymbol, Path: objectRef, Index: -1},
		DataType: dt,
	}
}

func readOne(t *testing.T, c driver.Conn, r driver.PointRef) driver.RawSample {
	t.Helper()
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{r}}})
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	return samples[0]
}

// ---------------------------------------------------------------------------
// Meta / 配置
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "iec61850" {
		t.Fatalf("id = %q", m.ID)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "tcp" {
		t.Fatalf("transports = %v", m.Transports)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("read/write = %v/%v", m.Capabilities.Read, m.Capabilities.Write)
	}
	// 浏览是协议支持但驱动能力声明为 unsupported：它是独立枚举整模型的操作，
	// 与调度式采集是两种语义，自检命令直接用 codec 的浏览函数实现。
	if m.Capabilities.Browse != driver.CapUnsupported {
		t.Fatalf("browse = %v", m.Capabilities.Browse)
	}
	if !m.Accepts(model.AddressSymbol) {
		t.Fatal("symbol addressing must be accepted")
	}
	if m.Accepts(model.AddressRegister) {
		t.Fatal("register addressing must be rejected")
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "iec61850",
			Transport: model.Transport{Kind: "rtu", Address: "127.0.0.1:502"}},
		Device: model.Device{ID: "ied1", ChannelID: "ch"},
	})
	if err == nil {
		t.Fatal("expected an error for a non-tcp transport")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Errorf("kind = %v, want unsupported", driver.KindOf(err))
	}
}

func TestOpenRejectsBadObjectRef(t *testing.T) {
	srv := newSim(t)
	// 对象引用缺逻辑设备前缀：点表层就该拦下，而不是连到设备再失败。
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: testChannel(srv.Addr()),
		Device: model.Device{ID: "ied1", ChannelID: "ch", Points: []model.Point{
			{ID: "p", Address: model.Address{Kind: model.AddressSymbol, Path: "GGIO1.ST.Ind1.stVal", Index: -1}, DataType: model.TypeBool, Enabled: true},
		}},
	})
	if err == nil {
		t.Fatal("expected an error for an object reference without a logical device")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Errorf("kind = %v, want address", driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 关联 + 读
// ---------------------------------------------------------------------------

func TestProbe(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, func(d *model.Device) {
		d.Labels = map[string]string{"probeObject": "simpleIO/GGIO1.ST.Ind1.stVal"}
	})
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if srv.AssocCount() < 1 {
		t.Fatalf("assoc count = %d, want >= 1", srv.AssocCount())
	}
}

func TestReadBoolIntFloat(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)

	cases := []struct {
		id   string
		ref  string
		dt   model.DataType
		want func(model.Value) bool
	}{
		{"ind1", "simpleIO/GGIO1.ST.Ind1.stVal", model.TypeBool,
			func(v model.Value) bool { return v.Bool }},
		{"health", "simpleIO/LLN0.ST.Health", model.TypeInt32,
			func(v model.Value) bool { n, ok := v.AsFloat(); return ok && n == 1 }},
		{"phv", "simpleIO/GGIO1.MX.PhV.mag.f", model.TypeFloat32,
			func(v model.Value) bool { n, ok := v.AsFloat(); return ok && n == 230.5 }},
	}
	for _, tc := range cases {
		s := readOne(t, c, ref(tc.id, tc.ref, tc.dt))
		if s.Err != nil {
			t.Errorf("read %s: %v", tc.id, s.Err)
			continue
		}
		if s.Quality != model.QualityGood {
			t.Errorf("read %s: quality = %v", tc.id, s.Quality)
		}
		if !tc.want(s.Value) {
			t.Errorf("read %s: value = %+v", tc.id, s.Value)
		}
	}
	if srv.ReadCount() < 3 {
		t.Errorf("read count = %d, want >= 3", srv.ReadCount())
	}
}

// defaultFC 补齐：对象引用省略 FC 时，由 labels.defaultFC 填上。
func TestReadWithDefaultFC(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, func(d *model.Device) {
		d.Labels = map[string]string{"defaultFC": "ST"}
	})
	s := readOne(t, c, ref("ind1", "simpleIO/GGIO1.Ind1.stVal", model.TypeBool))
	if s.Err != nil {
		t.Fatalf("read with defaultFC: %v", s.Err)
	}
	if !s.Value.Bool {
		t.Errorf("value = %+v, want true", s.Value)
	}
}

// 未知对象：object-non-existent 必须被分类成地址错误，而不是协议错误。
func TestReadObjectNonExistent(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("ghost", "simpleIO/GGIO1.ST.Ghost.stVal", model.TypeBool))
	if s.Err == nil {
		t.Fatal("expected an error for a non-existent object")
	}
	if driver.KindOf(s.Err) != driver.ErrAddress {
		t.Errorf("kind = %v, want address", driver.KindOf(s.Err))
	}
	if s.Quality != model.QualityAddressError {
		t.Errorf("quality = %v, want address-error", s.Quality)
	}
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func TestWriteThenRead(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)

	const name = "simpleIO$GGIO1$CO$SPCs$stVal"
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "spcs",
		Address:  model.Address{Kind: model.AddressSymbol, Path: "simpleIO/GGIO1.CO.SPCs.stVal", Index: -1},
		DataType: model.TypeBool,
		Value:    model.BoolValue(true),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("write result = %+v", res)
	}
	got, ok := srv.Get(name)
	if !ok {
		t.Fatalf("sim lost the variable %q", name)
	}
	if !got.Bool {
		t.Errorf("sim value = %+v, want true", got)
	}
	// 读回确认。
	s := readOne(t, c, ref("spcs", "simpleIO/GGIO1.CO.SPCs.stVal", model.TypeBool))
	if s.Err != nil || !s.Value.Bool {
		t.Errorf("read back = %+v err=%v", s.Value, s.Err)
	}
}

// 写不存在的对象：object-non-existent 分类成地址错误。
func TestWriteObjectNonExistent(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	_, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "ghost",
		Address:  model.Address{Kind: model.AddressSymbol, Path: "simpleIO/GGIO1.CO.Ghost.stVal", Index: -1},
		DataType: model.TypeBool,
		Value:    model.BoolValue(true),
	}})
	if err == nil {
		t.Fatal("expected an error writing a non-existent object")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Errorf("kind = %v, want address", driver.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// 故障路径
// ---------------------------------------------------------------------------

// 关联被拒：分类成协议错误（永久，重试无用），且不计入成功关联。
func TestAssociationRejected(t *testing.T) {
	srv := newSim(t)
	srv.RejectAssociation(true)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("ind1", "simpleIO/GGIO1.ST.Ind1.stVal", model.TypeBool))
	if s.Err == nil {
		t.Fatal("expected an error when the association is rejected")
	}
	if driver.KindOf(s.Err) != driver.ErrProtocol {
		t.Errorf("kind = %v, want protocol", driver.KindOf(s.Err))
	}
	if srv.AssocCount() != 0 {
		t.Errorf("assoc count = %d, want 0 (rejected associations must not count)", srv.AssocCount())
	}
}

// 读超时：从站不回响应时分类成超时（临时，可重试），而不是协议错误。
func TestReadTimeout(t *testing.T) {
	srv := newSim(t)
	srv.DropReads(true)
	c := openSim(t, srv, nil)
	s := readOne(t, c, ref("ind1", "simpleIO/GGIO1.ST.Ind1.stVal", model.TypeBool))
	if s.Err == nil {
		t.Fatal("expected a timeout error")
	}
	if driver.KindOf(s.Err) != driver.ErrTimeout {
		t.Errorf("kind = %v, want timeout", driver.KindOf(s.Err))
	}
}

// ---------------------------------------------------------------------------
// 模型浏览
// ---------------------------------------------------------------------------

// Browse 必须翻完所有页并按字典序返回，且不依赖采集路径（独立建链）。
func TestBrowseListsEveryVariable(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	got, err := c.Browse(context.Background(), "simpleIO")
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	want := itemNames(srv.Names(), "simpleIO")
	if len(got) != len(want) {
		t.Fatalf("browse returned %d names, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("name[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// 浏览走的是独立入口，不该额外产生读请求。
	if srv.ReadCount() != 0 {
		t.Errorf("read count = %d, want 0", srv.ReadCount())
	}
}

// 域不存在时返回空列表而不是错误：getNamellist 对空域不是故障。
func TestBrowseUnknownDomain(t *testing.T) {
	srv := newSim(t)
	c := openSim(t, srv, nil)
	got, err := c.Browse(context.Background(), "noSuchLD")
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("names = %v, want empty", got)
	}
}

// itemNames 把从站变量树的整键折成 MMS 项名（域由 objectScope 携带）。
func itemNames(keys []string, domain string) []string {
	prefix := domain + "$"
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k[len(prefix):])
		}
	}
	return out
}
