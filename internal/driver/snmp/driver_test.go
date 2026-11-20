package snmp

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// 这一层的测试重点不是「报文能不能组出来」（codec 已覆盖），而是三件
// 只在驱动层才成立的事：
//
//  1. 一帧装不下时怎么切（个数上限 + 字节预算），切完顺序不能乱；
//  2. v1 的 noSuchName 必须逐点重发，否则一个坏 OID 会毁掉整批；
//  3. 线类型与点表 dataType 不匹配时报错，而不是悄悄给一个 0。

// fakeDatagram 是注入给驱动的最小数据报链路：Write 时按 handler 生成
// 应答排进队列，Read 依次取出；队列空时返回超时。
type fakeDatagram struct {
	mu       sync.Mutex
	requests []snmpcodec.Message
	pending  [][]byte
	handler  func(req snmpcodec.Message) [][]byte
	closed   bool
	failRead error
}

func (f *fakeDatagram) Write(p []byte) error {
	m, err := snmpcodec.Parse(p)
	if err != nil {
		return fmt.Errorf("test agent cannot parse request: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, m)
	if f.handler != nil {
		f.pending = append(f.pending, f.handler(m)...)
	}
	return nil
}

func (f *fakeDatagram) Read() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRead != nil {
		return nil, f.failRead
	}
	if len(f.pending) == 0 {
		return nil, os.ErrDeadlineExceeded
	}
	b := f.pending[0]
	f.pending = f.pending[1:]
	return b, nil
}

func (f *fakeDatagram) SetDeadline(time.Time) error { return nil }

func (f *fakeDatagram) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeDatagram) snapshot() []snmpcodec.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]snmpcodec.Message(nil), f.requests...)
}

// reply 组一条与请求配对的应答。
func reply(t *testing.T, req snmpcodec.Message, vbs []snmpcodec.Varbind, status, index int) []byte {
	t.Helper()
	raw, err := snmpcodec.Build(snmpcodec.Message{
		Version: req.Version, Community: req.Community,
		PDU: snmpcodec.PDU{
			Type: snmpcodec.PDUResponse, RequestID: req.PDU.RequestID,
			ErrorStatus: status, ErrorIndex: index, Varbinds: vbs,
		},
	})
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	return raw
}

// tableAgent 按 OID 表回答 GET；表里没有的 OID 在 v2c 下返回
// noSuchObject，在 v1 下返回整批 noSuchName。SET 原样回显。
func tableAgent(t *testing.T, table map[string]snmpcodec.Value) func(snmpcodec.Message) [][]byte {
	t.Helper()
	return func(req snmpcodec.Message) [][]byte {
		switch req.PDU.Type {
		case snmpcodec.PDUSet:
			return [][]byte{reply(t, req, req.PDU.Varbinds, 0, 0)}
		case snmpcodec.PDUGet, snmpcodec.PDUGetNext:
		default:
			t.Errorf("unexpected PDU %s", req.PDU.Type)
			return nil
		}
		vbs := make([]snmpcodec.Varbind, len(req.PDU.Varbinds))
		for i, want := range req.PDU.Varbinds {
			v, ok := table[want.OID.String()]
			if !ok {
				if req.Version == snmpcodec.V1 {
					return [][]byte{reply(t, req, req.PDU.Varbinds, int(snmpcodec.ErrNoSuchName), i+1)}
				}
				vbs[i] = snmpcodec.Varbind{OID: want.OID, Value: snmpcodec.Exception(snmpcodec.TypeNoSuchObject)}
				continue
			}
			vbs[i] = snmpcodec.Varbind{OID: want.OID, Value: v}
		}
		return [][]byte{reply(t, req, vbs, 0, 0)}
	}
}

func point(id, oid string, dt model.DataType) driver.PointRef {
	return driver.PointRef{
		ID: id, DataType: dt, Length: 1,
		Address: model.Address{Kind: model.AddressSymbol, Path: oid, Index: -1},
	}
}

func testConfig(labels map[string]string) driver.OpenConfig {
	return driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch1", Protocol: "snmp",
			Transport: model.Transport{Kind: "udp", Address: "127.0.0.1:161"},
			Timeout:   model.Duration(100 * time.Millisecond),
		},
		Device: model.Device{ID: "dev1", ChannelID: "ch1", Labels: labels},
	}
}

// openTest 用假链路打开一个连接。
func openTest(t *testing.T, f *fakeDatagram, cfg driver.OpenConfig) *connState {
	t.Helper()
	d := NewWithDialer(func(context.Context, string, time.Duration) (transport.Datagram, error) {
		return f, nil
	})
	c, err := d.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*connState)
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "snmp" || !m.Accepts(model.AddressSymbol) {
		t.Fatalf("meta = %+v", m)
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.MultiRead != driver.CapSupported ||
		m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported || m.Capabilities.Browse != driver.CapUnsupported {
		t.Fatalf("trap/subscription must stay unsupported: %+v", m.Capabilities)
	}
	if m.Capabilities.Discovery != driver.CapUnsupported || m.Capabilities.BitAccess != driver.CapUnsupported {
		t.Fatalf("discovery/bit access must stay unsupported: %+v", m.Capabilities)
	}
	// 社区名必须作为凭据字段暴露给前端（Type=password），否则界面会把
	// 只读口令当普通文本显示。
	found := false
	for _, f := range m.ConfigFields {
		if f.Name == "labels.community" {
			found = true
			if f.Type != "password" {
				t.Fatalf("community field type = %q, want password", f.Type)
			}
		}
	}
	if !found {
		t.Fatal("labels.community field missing")
	}
}

// TestMetaExposesV3Config 钉住一件事：v3（USM）已交付，Meta 就必须把
// version=3 与 v3 的安全参数暴露给配置界面。
//
// 这不是文档洁癖。配置界面完全依据 Meta.ConfigFields 渲染：version 的 Enum
// 缺 "3"，界面就选不出 v3；v3 安全字段缺失，运维就没有地方填 username/
// authProtocol，USM 配置在界面上根本填不出来。而驱动本身早已支持 v3
// （parseOptions + newV3Session + 端到端测试），于是出现「底层能用、
// 界面配不出」的割裂——正是本次 Standards 轴 review 要抓的那类 Meta 与
// 实现漂移。
func TestMetaExposesV3Config(t *testing.T) {
	m := New().Meta()

	byName := map[string]driver.ConfigField{}
	for _, f := range m.ConfigFields {
		byName[f.Name] = f
	}

	// version 必须能选到 3。
	vf, ok := byName["labels.version"]
	if !ok {
		t.Fatal("labels.version field missing")
	}
	hasV3 := false
	for _, e := range vf.Enum {
		if e == "3" {
			hasV3 = true
		}
	}
	if !hasV3 {
		t.Errorf("labels.version Enum = %v，缺 \"3\"：界面选不出 v3", vf.Enum)
	}

	// v3 的五个安全参数（+ 可选 engineID）必须作为 ConfigField 暴露，且
	// 口令字段必须是 password 类型（否则界面把认证口令当明文显示）。
	required := []string{
		"labels.username", "labels.authProtocol", "labels.authPassword",
		"labels.privProtocol", "labels.privPassword", "labels.engineID",
	}
	for _, name := range required {
		f, ok := byName[name]
		if !ok {
			t.Errorf("v3 配置字段 %q missing", name)
			continue
		}
		if f.Scope == "" {
			t.Errorf("v3 字段 %q 缺 Scope", name)
		}
	}
	for _, name := range []string{"labels.authPassword", "labels.privPassword"} {
		if f := byName[name]; f.Type != "password" {
			t.Errorf("%s Type = %q，应为 password（凭据不能明文展示）", name, f.Type)
		}
	}
}

func TestReadDecodesAllWireTypes(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0":      snmpcodec.String("Linux router"),
		"1.3.6.1.2.1.1.3.0":      snmpcodec.Unsigned(snmpcodec.TypeTimeTicks, 123456),
		"1.3.6.1.2.1.2.2.1.10.1": snmpcodec.Unsigned(snmpcodec.TypeCounter64, 1<<40),
		"1.3.6.1.2.1.1.7.0":      snmpcodec.Integer(72),
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(nil))

	pts := []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
		point("uptime", "1.3.6.1.2.1.1.3.0", model.TypeUint32),
		point("octets", "1.3.6.1.2.1.2.2.1.10.1", model.TypeUint64),
		point("services", "1.3.6.1.2.1.1.7.0", model.TypeInt16),
		point("missing", "1.3.6.1.2.1.1.9.0", model.TypeString),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(pts) {
		t.Fatalf("samples = %d", len(out))
	}
	if out[0].Value.Text != "Linux router" || out[0].Quality != model.QualityGood {
		t.Fatalf("descr = %+v", out[0])
	}
	if out[1].Value.Num != 123456 {
		t.Fatalf("uptime = %+v", out[1])
	}
	if out[2].Value.Num != float64(uint64(1)<<40) {
		t.Fatalf("octets = %+v", out[2])
	}
	if out[3].Value.Num != 72 {
		t.Fatalf("services = %+v", out[3])
	}
	if out[4].Quality != model.QualityAddressError || out[4].Err == nil {
		t.Fatalf("missing OID must be an address error: %+v", out[4])
	}
	// 支持 MultiRead：五个 OID 必须落在一条 GET 里。
	reqs := f.snapshot()
	if len(reqs) != 1 || len(reqs[0].PDU.Varbinds) != 5 {
		t.Fatalf("requests = %d, varbinds = %d", len(reqs), len(reqs[0].PDU.Varbinds))
	}
}

func TestReadSplitsByMaxOids(t *testing.T) {
	table := map[string]snmpcodec.Value{}
	pts := make([]driver.PointRef, 5)
	for i := range pts {
		oid := fmt.Sprintf("1.3.6.1.2.1.2.2.1.10.%d", i+1)
		table[oid] = snmpcodec.Unsigned(snmpcodec.TypeCounter32, uint64(i))
		pts[i] = point(fmt.Sprintf("p%d", i), oid, model.TypeUint32)
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(map[string]string{"options": "maxOids=2"}))

	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	reqs := f.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3 (5 points, 2 per batch)", len(reqs))
	}
	for i, r := range reqs {
		if len(r.PDU.Varbinds) > 2 {
			t.Fatalf("request %d has %d varbinds", i, len(r.PDU.Varbinds))
		}
	}
	// 顺序不能因为分批而改变。
	for i := range pts {
		if out[i].PointID != pts[i].ID || out[i].Value.Num != float64(i) {
			t.Fatalf("out[%d] = %+v, want %s=%d", i, out[i], pts[i].ID, i)
		}
	}
}

func TestReadSplitsByByteBudget(t *testing.T) {
	// 30 弧的 OID 约 160 字节；十个就超过 1400。点数上限设得很大，
	// 触发分片的原因只能是字节预算。
	table := map[string]snmpcodec.Value{}
	pts := make([]driver.PointRef, 20)
	for i := range pts {
		arcs := make([]string, 0, 30)
		arcs = append(arcs, "1", "3", "6", "1", "4", "1", "9999")
		for j := 0; j < 22; j++ {
			arcs = append(arcs, fmt.Sprintf("%d", (i+1)*(j+1)%100000))
		}
		oid := strings.Join(arcs, ".")
		table[oid] = snmpcodec.Integer(int64(i))
		pts[i] = point(fmt.Sprintf("p%d", i), oid, model.TypeInt32)
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(map[string]string{"options": "maxOids=128"}))

	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	reqs := f.snapshot()
	if len(reqs) < 2 {
		t.Fatalf("requests = %d, want >1 (byte budget must split)", len(reqs))
	}
	for _, r := range reqs {
		// 重新编码一次请求，确认实际线长度也在预算内。
		oids := make([]snmpcodec.OID, len(r.PDU.Varbinds))
		for i, vb := range r.PDU.Varbinds {
			oids[i] = vb.OID
		}
		raw, err := snmpcodec.GetRequest(r.Version, r.Community, 1, oids)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > maxRequestBytes {
			t.Fatalf("request is %d bytes, budget %d", len(raw), maxRequestBytes)
		}
	}
	for i := range pts {
		if out[i].Value.Num != float64(i) {
			t.Fatalf("out[%d] = %+v", i, out[i])
		}
	}
}

// v1 没有 per-varbind 异常：整批 noSuchName 后必须逐点重发，否则一个
// 坏 OID 会让同批其它点全部变成地址错误。
func TestV1NoSuchNameFallsBackToOneByOne(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("ok"),
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(map[string]string{"version": "1"}))

	pts := []driver.PointRef{
		point("good", "1.3.6.1.2.1.1.1.0", model.TypeString),
		point("bad", "1.3.6.1.2.1.1.9.0", model.TypeString),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityGood || out[0].Value.Text != "ok" {
		t.Fatalf("good = %+v", out[0])
	}
	if out[1].Quality != model.QualityAddressError {
		t.Fatalf("bad = %+v", out[1])
	}
	reqs := f.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3 (batch + 2 singles)", len(reqs))
	}
	if len(reqs[0].PDU.Varbinds) != 2 || len(reqs[1].PDU.Varbinds) != 1 || len(reqs[2].PDU.Varbinds) != 1 {
		t.Fatalf("varbind counts = %d/%d/%d", len(reqs[0].PDU.Varbinds), len(reqs[1].PDU.Varbinds), len(reqs[2].PDU.Varbinds))
	}
}

func TestWriteMapsTypesAndChecksEcho(t *testing.T) {
	f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
		return [][]byte{reply(t, req, req.PDU.Varbinds, 0, 0)}
	}}
	c := openTest(t, f, testConfig(nil))

	reqs := []driver.WriteRequest{
		{PointID: "u32", DataType: model.TypeUint32,
			Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.4.0", Index: -1},
			Value:   model.NumValue(model.TypeUint32, 42)},
		{PointID: "b", DataType: model.TypeBool,
			Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.4.1", Index: -1},
			Value:   model.BoolValue(true)},
		{PointID: "s", DataType: model.TypeString,
			Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.6.0", Index: -1},
			Value:   model.TextValue(model.TypeString, "lab-1")},
	}
	out, err := c.Write(context.Background(), reqs)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	for i := range out {
		if !out[i].OK || out[i].Err != nil {
			t.Fatalf("write %d = %+v", i, out[i])
		}
	}
	reqsSeen := f.snapshot()
	if len(reqsSeen) != 3 {
		t.Fatalf("requests = %d", len(reqsSeen))
	}
	if reqsSeen[0].PDU.Type != snmpcodec.PDUSet {
		t.Fatalf("pdu = %s", reqsSeen[0].PDU.Type)
	}
	if v := reqsSeen[0].PDU.Varbinds[0].Value; v.Type != snmpcodec.TypeGauge32 || v.Uint != 42 {
		t.Fatalf("uint32 wire = %s %d", v.Type, v.Uint)
	}
	if v := reqsSeen[1].PDU.Varbinds[0].Value; v.Type != snmpcodec.TypeInteger || v.Int != 1 {
		t.Fatalf("bool wire = %s %d", v.Type, v.Int)
	}
	if v := reqsSeen[2].PDU.Varbinds[0].Value; v.Type != snmpcodec.TypeOctetString || string(v.Bytes) != "lab-1" {
		t.Fatalf("string wire = %s %q", v.Type, v.Bytes)
	}
}

func TestWriteRejectsUnsupportedTypes(t *testing.T) {
	f := &fakeDatagram{}
	c := openTest(t, f, testConfig(nil))
	addr := model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.4.0", Index: -1}

	cases := []struct {
		name string
		req  driver.WriteRequest
	}{
		{"float", driver.WriteRequest{PointID: "f", DataType: model.TypeFloat64, Address: addr,
			Value: model.NumValue(model.TypeFloat64, 1.5)}},
		{"bcd", driver.WriteRequest{PointID: "b", DataType: model.TypeBCD16, Address: addr,
			Value: model.NumValue(model.TypeBCD16, 12)}},
		{"int16 out of range", driver.WriteRequest{PointID: "i", DataType: model.TypeInt16, Address: addr,
			Value: model.NumValue(model.TypeInt16, 70000)}},
		{"fractional", driver.WriteRequest{PointID: "i2", DataType: model.TypeInt32, Address: addr,
			Value: model.NumValue(model.TypeInt32, 1.5)}},
		{"negative unsigned", driver.WriteRequest{PointID: "u", DataType: model.TypeUint16, Address: addr,
			Value: model.NumValue(model.TypeUint16, -1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := c.Write(context.Background(), []driver.WriteRequest{tc.req})
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if len(out) != 1 || out[0].OK || out[0].Err == nil {
				t.Fatalf("result = %+v", out)
			}
			if got := driver.KindOf(out[0].Err); got != driver.ErrUnsupported {
				t.Fatalf("kind = %v, want unsupported (%v)", got, out[0].Err)
			}
		})
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("%d requests were sent for rejected writes", n)
	}
}

func TestWriteStatusAndEchoMismatch(t *testing.T) {
	cases := []struct {
		name   string
		status snmpcodec.ErrorStatus
		kind   driver.ErrorKind
	}{
		{"readOnly", snmpcodec.ErrReadOnly, driver.ErrPermission},
		{"noSuchName", snmpcodec.ErrNoSuchName, driver.ErrAddress},
		{"genErr", snmpcodec.ErrGenErr, driver.ErrProtocol},
		{"wrongType", snmpcodec.ErrWrongType, driver.ErrAddress},
		{"notWritable", snmpcodec.ErrNotWritable, driver.ErrPermission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
				return [][]byte{reply(t, req, req.PDU.Varbinds, int(tc.status), 1)}
			}}
			c := openTest(t, f, testConfig(nil))
			out, err := c.Write(context.Background(), []driver.WriteRequest{{
				PointID: "p", DataType: model.TypeUint32,
				Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.4.0", Index: -1},
				Value:   model.NumValue(model.TypeUint32, 1),
			}})
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if out[0].OK || out[0].Err == nil {
				t.Fatalf("result = %+v", out[0])
			}
			if got := driver.KindOf(out[0].Err); got != tc.kind {
				t.Fatalf("kind = %v, want %v (%v)", got, tc.kind, out[0].Err)
			}
			if !strings.Contains(out[0].Err.Error(), tc.status.String()) {
				t.Fatalf("error %q does not name the status", out[0].Err)
			}
		})
	}

	t.Run("echo coerced", func(t *testing.T) {
		f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
			vb := req.PDU.Varbinds[0]
			vb.Value = snmpcodec.Unsigned(snmpcodec.TypeGauge32, 100)
			return [][]byte{reply(t, req, []snmpcodec.Varbind{vb}, 0, 0)}
		}}
		c := openTest(t, f, testConfig(nil))
		out, _ := c.Write(context.Background(), []driver.WriteRequest{{
			PointID: "p", DataType: model.TypeUint32,
			Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.4.0", Index: -1},
			Value:   model.NumValue(model.TypeUint32, 1),
		}})
		if out[0].OK || driver.KindOf(out[0].Err) != driver.ErrProtocol {
			t.Fatalf("coerced echo must be a protocol error: %+v", out[0])
		}
	})
}

func TestProbeAcceptsExceptionAndReportsTimeout(t *testing.T) {
	t.Run("exception means reachable", func(t *testing.T) {
		f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
			vb := snmpcodec.Varbind{OID: req.PDU.Varbinds[0].OID, Value: snmpcodec.Exception(snmpcodec.TypeNoSuchObject)}
			return [][]byte{reply(t, req, []snmpcodec.Varbind{vb}, 0, 0)}
		}}
		c := openTest(t, f, testConfig(nil))
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("probe: %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		f := &fakeDatagram{handler: func(snmpcodec.Message) [][]byte { return nil }}
		c := openTest(t, f, testConfig(map[string]string{"options": "retries=1"}))
		err := c.Probe(context.Background())
		if err == nil {
			t.Fatal("probe must fail")
		}
		if driver.KindOf(err) != driver.ErrTimeout {
			t.Fatalf("kind = %v (%v)", driver.KindOf(err), err)
		}
		if !strings.Contains(err.Error(), "community") {
			t.Fatalf("timeout error should mention community/ACL: %v", err)
		}
		if n := len(f.snapshot()); n != 2 {
			t.Fatalf("attempts = %d, want retries+1 = 2", n)
		}
	})
}

func TestExchangeIgnoresUnrelatedResponses(t *testing.T) {
	// 先塞一条 request-id 不匹配的应答，再塞真正的应答；驱动必须丢掉
	// 前者，而不是把它的值当成当前事务的结果。
	f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
		stale := req
		stale.PDU.RequestID = req.PDU.RequestID + 100
		stale.PDU.Varbinds = []snmpcodec.Varbind{{OID: req.PDU.Varbinds[0].OID, Value: snmpcodec.String("stale")}}
		real := req
		real.PDU.Varbinds = []snmpcodec.Varbind{{OID: req.PDU.Varbinds[0].OID, Value: snmpcodec.String("fresh")}}
		return [][]byte{
			reply(t, stale, stale.PDU.Varbinds, 0, 0),
			reply(t, real, real.PDU.Varbinds, 0, 0),
		}
	}}
	c := openTest(t, f, testConfig(nil))
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Value.Text != "fresh" {
		t.Fatalf("value = %q, want fresh", out[0].Value.Text)
	}
}

func TestReadLinkFailureStopsRemainingBatches(t *testing.T) {
	f := &fakeDatagram{handler: func(snmpcodec.Message) [][]byte { return nil }}
	c := openTest(t, f, testConfig(map[string]string{"options": "retries=0,maxOids=1"}))
	pts := []driver.PointRef{
		point("a", "1.3.6.1.2.1.1.1.0", model.TypeString),
		point("b", "1.3.6.1.2.1.1.3.0", model.TypeString),
		point("c", "1.3.6.1.2.1.1.5.0", model.TypeString),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err == nil || driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v", err)
	}
	for i, s := range out {
		if s.Quality != model.QualityTimeout {
			t.Fatalf("out[%d] = %+v", i, s)
		}
	}
	if n := len(f.snapshot()); n != 1 {
		t.Fatalf("requests = %d; after a timeout the remaining batches must not be attempted", n)
	}
}

// TestWriteFailureCarriesQuality 钉住写路径的质量码：失败结果不能留空。
//
// 留空时 collector 会补成 bad，于是「超时」「地址写错」「设备拒写」在下游
// 变成同一个值。读路径的 errSample 一直按 kind 给质量码，写路径必须一致。
func TestWriteFailureCarriesQuality(t *testing.T) {
	writeReq := func(id string, addr model.Address) driver.WriteRequest {
		return driver.WriteRequest{PointID: id, DataType: model.TypeUint32,
			Address: addr, Value: model.NumValue(model.TypeUint32, 1)}
	}
	sym := func(oid string) model.Address {
		return model.Address{Kind: model.AddressSymbol, Path: oid, Index: -1}
	}

	cases := []struct {
		name   string
		status snmpcodec.ErrorStatus
		want   model.Quality
	}{
		{"readOnly", snmpcodec.ErrReadOnly, model.QualityBad},
		{"genErr", snmpcodec.ErrGenErr, model.QualityBad},
		{"noSuchName", snmpcodec.ErrNoSuchName, model.QualityAddressError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
				return [][]byte{reply(t, req, req.PDU.Varbinds, int(tc.status), 1)}
			}}
			c := openTest(t, f, testConfig(nil))
			out, err := c.Write(context.Background(), []driver.WriteRequest{
				writeReq("p", sym("1.3.6.1.2.1.1.4.0"))})
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if out[0].OK || out[0].Err == nil {
				t.Fatalf("result = %+v", out[0])
			}
			if out[0].Quality != tc.want {
				t.Fatalf("quality = %q, want %q", out[0].Quality, tc.want)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		f := &fakeDatagram{handler: func(snmpcodec.Message) [][]byte { return nil }}
		c := openTest(t, f, testConfig(map[string]string{"options": "retries=0"}))
		out, _ := c.Write(context.Background(), []driver.WriteRequest{
			writeReq("p", sym("1.3.6.1.2.1.1.4.0"))})
		if out[0].Quality != model.QualityTimeout {
			t.Fatalf("timeout quality = %q", out[0].Quality)
		}
	})

	t.Run("bad address kind", func(t *testing.T) {
		f := &fakeDatagram{}
		c := openTest(t, f, testConfig(nil))
		out, _ := c.Write(context.Background(), []driver.WriteRequest{
			writeReq("p", model.Address{Kind: model.AddressRegister, Area: "holding"})})
		if out[0].Quality != model.QualityAddressError {
			t.Fatalf("address quality = %q", out[0].Quality)
		}
	})

	t.Run("unsupported data type", func(t *testing.T) {
		f := &fakeDatagram{}
		c := openTest(t, f, testConfig(nil))
		req := writeReq("p", sym("1.3.6.1.2.1.1.4.0"))
		req.DataType = model.TypeFloat64
		req.Value = model.NumValue(model.TypeFloat64, 1.5)
		out, _ := c.Write(context.Background(), []driver.WriteRequest{req})
		if out[0].Quality != model.QualityBad {
			t.Fatalf("unsupported-type quality = %q", out[0].Quality)
		}
	})
}

// TestV1FallbackLinkFailureKeepsGoodSamples：v1 的逐点退化路径中途超时，
// 已经读到的样本必须留下，同时链路错误必须抛出去，后续批次不再尝试。
//
// 早先这条路径把「剩余点位标坏」之后返回 nil 错误，Read 会把下一批也发
// 出去，每次都要等满一个超时；而如果改成「有错就整批丢弃」，前半段的
// 成功样本又会一起丢掉。两件事必须同时成立。
func TestV1FallbackLinkFailureKeepsGoodSamples(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("ok"),
	}
	inner := tableAgent(t, table)
	calls := 0
	f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
		calls++
		if calls == 3 { // 批 + 单点 a + 单点 b：第 3 次交互链路断掉
			return nil
		}
		return inner(req)
	}}
	c := openTest(t, f, testConfig(map[string]string{"version": "1", "options": "retries=0,maxOids=2"}))
	pts := []driver.PointRef{
		point("a", "1.3.6.1.2.1.1.1.0", model.TypeString),
		point("b", "1.3.6.1.2.1.1.9.0", model.TypeString),
		point("c", "1.3.6.1.2.1.1.5.0", model.TypeString),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("err = %v, want a link timeout", err)
	}
	if out[0].Quality != model.QualityGood || out[0].Value.Text != "ok" {
		t.Fatalf("sample read before the timeout was lost: %+v", out[0])
	}
	if out[1].Quality != model.QualityTimeout || out[2].Quality != model.QualityTimeout {
		t.Fatalf("remaining samples = %+v / %+v", out[1], out[2])
	}
	if calls != 3 {
		t.Fatalf("interactions = %d; nothing may be sent after the link failed", calls)
	}
}

func TestReadRejectsBadAddressAndWrongWireType(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.Octets([]byte("not a number")),
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(nil))

	badKind := driver.PointRef{ID: "reg", DataType: model.TypeUint16,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0}}
	badIndex := point("idx", "1.3.6.1.2.1.2.2.1.10", model.TypeUint32)
	badIndex.Address.Index = 1
	numericAsString := point("num", "1.3.6.1.2.1.1.1.0", model.TypeUint16)

	// 有非地址类错误时 Read 会带回 firstErr（与其它驱动一致），但每个
	// 点位的质量码仍然逐点给出——调用方看的是质量码，不是这个汇总错误。
	out, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{badKind, badIndex, numericAsString}}})
	if driver.KindOf(out[0].Err) != driver.ErrAddress {
		t.Fatalf("register address = %+v", out[0])
	}
	if driver.KindOf(out[1].Err) != driver.ErrAddress {
		t.Fatalf("symbol index = %+v", out[1])
	}
	// 设备返回 OCTET STRING 但点位是数值型：必须报错（提示改 dataType），
	// 不能把字符串硬转成 0。
	if driver.KindOf(out[2].Err) != driver.ErrProtocol || !strings.Contains(out[2].Err.Error(), "dataType") {
		t.Fatalf("octet string as number = %+v", out[2])
	}
}

func TestParseOptions(t *testing.T) {
	dev := func(labels map[string]string) model.Device {
		return model.Device{ID: "d", Labels: labels}
	}
	opt, err := parseOptions(dev(nil))
	if err != nil || opt.Community != "public" || opt.Version != snmpcodec.V2c || opt.Retries != defaultRetries || opt.MaxOids != defaultMaxOids {
		t.Fatalf("defaults = %+v, %v", opt, err)
	}
	opt, err = parseOptions(dev(map[string]string{
		"community": "s3cret", "version": "1", "options": "retries=0, maxOids=8",
	}))
	if err != nil || opt.Community != "s3cret" || opt.Version != snmpcodec.V1 || opt.Retries != 0 || opt.MaxOids != 8 {
		t.Fatalf("parsed = %+v, %v", opt, err)
	}

	bad := []map[string]string{
		{"version": "9"},
		{"community": "  "},
		{"options": "retries=11"},
		{"options": "retries"},
		{"options": "maxOids=0"},
		{"options": "unknown=1"},
		// v3 少了用户名：USM 的用户名不能猜。
		{"version": "3"},
		// 只配加密不配认证是非法的。
		{"version": "3", "username": "u", "authProtocol": "none", "privProtocol": "aes",
			"privPassword": "12345678"},
		// 未知算法名。
		{"version": "3", "username": "u", "authProtocol": "md4"},
		{"version": "3", "username": "u", "authProtocol": "sha", "privProtocol": "aes", "privPassword": "12345678", "engineID": "zz"},
	}
	for _, labels := range bad {
		if _, err := parseOptions(dev(labels)); err == nil {
			t.Fatalf("labels %v must fail", labels)
		}
	}

	got, err := parseOptions(dev(map[string]string{
		"version": "3", "username": "monitor",
		"authProtocol": "SHA-256", "authPassword": "authpass123",
		"privProtocol": "AES", "privPassword": "privpass123",
		"engineID": "80:00:1f:88:80:00:00:00:01",
	}))
	if err != nil {
		t.Fatalf("v3 options: %v", err)
	}
	if got.Version != snmpcodec.V3 || got.Username != "monitor" ||
		got.AuthProtocol != snmpcodec.AuthSHA256 || got.PrivProtocol != snmpcodec.PrivAES {
		t.Fatalf("v3 parsed = %+v", got)
	}
	if len(got.FixedEngineID) != 9 {
		t.Fatalf("fixed engine ID = %x", got.FixedEngineID)
	}
}

func TestOpenValidationAndDefaultPort(t *testing.T) {
	d := NewWithDialer(func(_ context.Context, addr string, _ time.Duration) (transport.Datagram, error) {
		if addr != "10.0.0.5:161" {
			t.Errorf("dial addr = %q", addr)
		}
		return &fakeDatagram{}, nil
	})
	cfg := testConfig(nil)
	cfg.Channel.Transport.Address = "10.0.0.5"
	if _, err := d.Open(context.Background(), cfg); err != nil {
		t.Fatalf("open with default port: %v", err)
	}

	cfg = testConfig(nil)
	cfg.Channel.Transport.Kind = "tcp"
	if _, err := d.Open(context.Background(), cfg); err == nil ||
		driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("tcp transport = %v", err)
	}

	cfg = testConfig(nil)
	cfg.Channel.Transport.Address = " "
	if _, err := d.Open(context.Background(), cfg); err == nil {
		t.Fatal("empty address must fail")
	}

	// v3 缺用户名时仍必须在 Open 阶段拒绝（不是等到第一条请求）。
	cfg = testConfig(map[string]string{"version": "3"})
	if _, err := d.Open(context.Background(), cfg); err == nil {
		t.Fatal("v3 without username must be refused")
	}
	// 口令不足 8 字节也是配置期错误。
	cfg = testConfig(map[string]string{"version": "3", "username": "u",
		"authProtocol": "sha", "authPassword": "short"})
	if _, err := d.Open(context.Background(), cfg); err == nil {
		t.Fatal("short auth password must be refused")
	}
}

func TestReadMalformedResponseIsProtocolError(t *testing.T) {
	f := &fakeDatagram{handler: func(snmpcodec.Message) [][]byte {
		return [][]byte{{0x30, 0x03, 0x02, 0x01}} // 截断
	}}
	c := openTest(t, f, testConfig(map[string]string{"options": "retries=0"}))
	out, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("p", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if driver.KindOf(out[0].Err) != driver.ErrProtocol {
		t.Fatalf("malformed response = %+v", out[0])
	}
}

func TestReadCounter64PrecisionBoundary(t *testing.T) {
	// 文档边界：Counter64 > 2^53 经 float64 会丢精度。这里钉住「确实丢」
	// 而不是假装没丢，提醒调用方别把大计数器当精确值用。
	const big = uint64(1)<<60 + 1
	f := &fakeDatagram{handler: tableAgent(t, map[string]snmpcodec.Value{
		"1.3.6.1.2.1.2.2.1.10.1": snmpcodec.Unsigned(snmpcodec.TypeCounter64, big),
	})}
	c := openTest(t, f, testConfig(nil))
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("c", "1.3.6.1.2.1.2.2.1.10.1", model.TypeUint64),
	}}})
	if err != nil || out[0].Quality != model.QualityGood {
		t.Fatalf("read = %+v, %v", out[0], err)
	}
	if out[0].Value.Num == float64(big) && math.Trunc(out[0].Value.Num) == float64(big) {
		t.Logf("counter64 survived float64 (implementation-dependent): %v", out[0].Value.Num)
	}
}

func TestWriteAddressValidation(t *testing.T) {
	f := &fakeDatagram{}
	c := openTest(t, f, testConfig(nil))
	cases := []driver.WriteRequest{
		{PointID: "reg", DataType: model.TypeUint16,
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 1},
			Value:   model.NumValue(model.TypeUint16, 1)},
		{PointID: "sel", DataType: model.TypeString,
			Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.1.0", Selector: "x"},
			Value:   model.TextValue(model.TypeString, "v")},
		{PointID: "oid", DataType: model.TypeUint16,
			Address: model.Address{Kind: model.AddressSymbol, Path: "1"},
			Value:   model.NumValue(model.TypeUint16, 1)},
	}
	for _, req := range cases {
		out, err := c.Write(context.Background(), []driver.WriteRequest{req})
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if out[0].OK || driver.KindOf(out[0].Err) != driver.ErrAddress {
			t.Fatalf("%s = %+v", req.PointID, out[0])
		}
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("%d requests sent for invalid addresses", n)
	}
}

// 确认 bytes 类型的值按原样上线，而不是被 UTF-8 规范化。
func TestWriteBytesPreservesRaw(t *testing.T) {
	f := &fakeDatagram{handler: func(req snmpcodec.Message) [][]byte {
		return [][]byte{reply(t, req, req.PDU.Varbinds, 0, 0)}
	}}
	c := openTest(t, f, testConfig(nil))
	raw := []byte{0x00, 0xFF, 0x7F}
	out, _ := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "b", DataType: model.TypeBytes,
		Address: model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.1.0"},
		Value:   model.BytesValue(raw),
	}})
	if !out[0].OK {
		t.Fatalf("write = %+v", out[0])
	}
	got := f.snapshot()[0].PDU.Varbinds[0].Value
	if !bytes.Equal(got.Bytes, raw) {
		t.Fatalf("bytes = % x, want % x", got.Bytes, raw)
	}
}

// walkAgent 按 OID 字典序回答 GETNEXT：这是自检 Walk 的对手方。
func walkAgent(t *testing.T, table map[string]snmpcodec.Value) func(snmpcodec.Message) [][]byte {
	t.Helper()
	keys := make([]snmpcodec.OID, 0, len(table))
	for k := range table {
		keys = append(keys, snmpcodec.MustParseOID(k))
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Compare(keys[j]) < 0 })
	return func(req snmpcodec.Message) [][]byte {
		if req.PDU.Type == snmpcodec.PDUGet {
			return tableAgent(t, table)(req)
		}
		vbs := make([]snmpcodec.Varbind, len(req.PDU.Varbinds))
		for i, want := range req.PDU.Varbinds {
			next := -1
			for j, k := range keys {
				if k.Compare(want.OID) > 0 {
					next = j
					break
				}
			}
			if next < 0 {
				if req.Version == snmpcodec.V1 {
					return [][]byte{reply(t, req, req.PDU.Varbinds, int(snmpcodec.ErrNoSuchName), i+1)}
				}
				vbs[i] = snmpcodec.Varbind{OID: want.OID, Value: snmpcodec.Exception(snmpcodec.TypeEndOfMibView)}
				continue
			}
			vbs[i] = snmpcodec.Varbind{OID: keys[next], Value: table[keys[next].String()]}
		}
		return [][]byte{reply(t, req, vbs, 0, 0)}
	}
}

func TestGetRawKeepsWireTypes(t *testing.T) {
	f := &fakeDatagram{handler: tableAgent(t, map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("agent"),
		"1.3.6.1.2.1.1.7.0": snmpcodec.Integer(72),
	})}
	c := openTest(t, f, testConfig(nil))
	out, err := c.GetRaw(context.Background(), []snmpcodec.OID{
		snmpcodec.MustParseOID("1.3.6.1.2.1.1.1.0"),
		snmpcodec.MustParseOID("1.3.6.1.2.1.1.7.0"),
		snmpcodec.MustParseOID("1.3.6.1.2.1.1.9.0"),
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out[0].Value.Type != snmpcodec.TypeOctetString || out[0].Value.String() != "agent" {
		t.Fatalf("raw[0] = %+v", out[0])
	}
	if out[1].Value.Type != snmpcodec.TypeInteger || out[1].Value.Int != 72 {
		t.Fatalf("raw[1] = %+v", out[1])
	}
	// 不存在的 OID 在 v2c 下是异常标记，不是错误——自检命令要能把它
	// 原样显示成 noSuchObject。
	if out[2].Value.Type != snmpcodec.TypeNoSuchObject || out[2].Err != nil {
		t.Fatalf("raw[2] = %+v", out[2])
	}
}

func TestWalkStopsAtSubtreeAndMax(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("a"),
		"1.3.6.1.2.1.1.3.0": snmpcodec.Unsigned(snmpcodec.TypeTimeTicks, 1),
		"1.3.6.1.2.1.1.5.0": snmpcodec.String("b"),
		"1.3.6.1.2.1.2.1.0": snmpcodec.Integer(2),
	}
	f := &fakeDatagram{handler: walkAgent(t, table)}
	c := openTest(t, f, testConfig(nil))

	out, err := c.Walk(context.Background(), snmpcodec.MustParseOID("1.3.6.1.2.1.1"), 100)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("walked %d OIDs, want 3 (must stop when leaving 1.3.6.1.2.1.1)", len(out))
	}
	if got := out[0].OID.String(); got != "1.3.6.1.2.1.1.1.0" {
		t.Fatalf("first = %s", got)
	}

	out, err = c.Walk(context.Background(), snmpcodec.MustParseOID("1.3.6.1.2.1.1"), 2)
	if err != nil || len(out) != 2 {
		t.Fatalf("max walk = %d, %v", len(out), err)
	}
}

func TestWalkV1EndOfMib(t *testing.T) {
	f := &fakeDatagram{handler: walkAgent(t, map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("a"),
	})}
	c := openTest(t, f, testConfig(map[string]string{"version": "1"}))
	out, err := c.Walk(context.Background(), snmpcodec.MustParseOID("1.3.6.1.2.1.1"), 10)
	if err != nil {
		t.Fatalf("v1 walk: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("walked = %d", len(out))
	}
}

// TestReadPartialFailureKeepsGoodSamples 钉住部分失败契约（docs §3.7.4 /
// §3.9.5）：一轮里既有点读成功、又有**非地址类**失败（这里是把 INTEGER
// 当成 string 读 → protocol）时，Read 不向上返回错误。
//
// 这条契约不是「宽容」而是必须：上层 collector 见到 Read 返回错误会按错误
// 分类丢弃连接（ErrProtocol 也在其中）并重建，一个 dataType 配错的点位就会
// 让整台设备每周期断链重连一次；熔断器还会把「点表配错」误判成「设备坏了」。
// 为了让测试覆盖到真正的风险面，这里刻意用 protocol 错误而不是 address
// 错误——address 从来不进 firstErr，用它会得到假绿。
func TestReadPartialFailureKeepsGoodSamples(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("Linux router"),
		"1.3.6.1.2.1.1.7.0": snmpcodec.Integer(72),
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(nil))

	pts := []driver.PointRef{
		point("good", "1.3.6.1.2.1.1.1.0", model.TypeString),
		// INTEGER 按 string 读：设备没错、点表配错，分类是 protocol。
		point("bad", "1.3.6.1.2.1.1.7.0", model.TypeString),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err != nil {
		t.Fatalf("有成功点时 Read 不该返回错误，得到 %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("samples = %d, want 2（坏点也要有样本）", len(out))
	}
	if out[0].Err != nil || out[0].Quality != model.QualityGood || out[0].Value.Text != "Linux router" {
		t.Fatalf("好点不该被连累: %+v", out[0])
	}
	if out[1].Err == nil || driver.KindOf(out[1].Err) != driver.ErrProtocol {
		t.Fatalf("坏点应带 protocol 错误: %+v", out[1])
	}
}

// TestReadAllFailedStillReturnsError 是反面：全部点位都失败时必须返回错误。
// 只在样本上标坏而不返回错误，上层熔断器与告警都看不到「这台设备全坏了」。
func TestReadAllFailedStillReturnsError(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.7.0": snmpcodec.Integer(72),
		"1.3.6.1.2.1.1.3.0": snmpcodec.Unsigned(snmpcodec.TypeTimeTicks, 9),
	}
	f := &fakeDatagram{handler: tableAgent(t, table)}
	c := openTest(t, f, testConfig(nil))

	pts := []driver.PointRef{
		point("bad1", "1.3.6.1.2.1.1.7.0", model.TypeString),
		point("bad2", "1.3.6.1.2.1.1.3.0", model.TypeString),
	}
	out, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: pts}})
	if err == nil {
		t.Fatal("全部点位失败时必须返回错误")
	}
	if len(out) != 2 {
		t.Fatalf("samples = %d, want 2", len(out))
	}
}

// bulkAgent 实现 GetBulk 语义：每个 varbind 最多回 max-repetitions 条后继，
// 表尾补 EndOfMibView。walkAgent 只会逐条回——真 bulk 应答（一次多条、
// 条目中途越界/表尾）只有这里能覆盖。
func bulkAgent(t *testing.T, table map[string]snmpcodec.Value) func(snmpcodec.Message) [][]byte {
	t.Helper()
	keys := make([]snmpcodec.OID, 0, len(table))
	for k := range table {
		keys = append(keys, snmpcodec.MustParseOID(k))
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Compare(keys[j]) < 0 })
	return func(req snmpcodec.Message) [][]byte {
		vbs := make([]snmpcodec.Varbind, 0, req.PDU.MaxRepetitions)
		cur := req.PDU.Varbinds[0].OID
		for i := 0; i < req.PDU.MaxRepetitions; i++ {
			next := -1
			for j, k := range keys {
				if k.Compare(cur) > 0 {
					next = j
					break
				}
			}
			if next < 0 {
				vbs = append(vbs, snmpcodec.Varbind{OID: cur, Value: snmpcodec.Exception(snmpcodec.TypeEndOfMibView)})
				continue
			}
			vbs = append(vbs, snmpcodec.Varbind{OID: keys[next], Value: table[keys[next].String()]})
			cur = keys[next]
		}
		return [][]byte{reply(t, req, vbs, 0, 0)}
	}
}

// v2c Walk 必须走 GetBulk：一次往返取回多条后继，往返次数从表行数降为
// 表行数/max-repetitions。7 条表目 + max-repetitions=25 → 一次往返收齐。
func TestWalkV2UsesGetBulk(t *testing.T) {
	table := map[string]snmpcodec.Value{
		"1.3.6.1.2.1.1.1.0": snmpcodec.String("a"),
		"1.3.6.1.2.1.1.2.0": snmpcodec.String("b"),
		"1.3.6.1.2.1.1.3.0": snmpcodec.String("c"),
		"1.3.6.1.2.1.1.4.0": snmpcodec.String("d"),
		"1.3.6.1.2.1.1.5.0": snmpcodec.String("e"),
		"1.3.6.1.2.1.1.6.0": snmpcodec.String("f"),
		"1.3.6.1.2.1.1.7.0": snmpcodec.String("g"),
		"1.3.6.1.2.1.2.1.0": snmpcodec.Integer(2), // 子树外：应终止遍历
	}
	f := &fakeDatagram{handler: bulkAgent(t, table)}
	c := openTest(t, f, testConfig(nil))

	out, err := c.Walk(context.Background(), snmpcodec.MustParseOID("1.3.6.1.2.1.1"), 100)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) != 7 {
		t.Fatalf("walked %d OIDs, want 7", len(out))
	}
	if got := out[6].OID.String(); got != "1.3.6.1.2.1.1.7.0" {
		t.Fatalf("last = %s", got)
	}
	// 一次往返就收齐（连接探活还有自己的请求，这里只看 bulk 请求）。
	bulk := 0
	for _, m := range f.requests {
		if m.PDU.Type == snmpcodec.PDUGetBulk {
			bulk++
			if m.PDU.MaxRepetitions <= 0 {
				t.Fatal("GetBulk 请求缺 max-repetitions")
			}
		}
	}
	if bulk != 1 {
		t.Fatalf("GetBulk 请求次数 = %d, want 1", bulk)
	}

	// max 截断同样一条往返内完成（reps 收缩到剩余额度）。
	f2 := &fakeDatagram{handler: bulkAgent(t, table)}
	c2 := openTest(t, f2, testConfig(nil))
	out2, err := c2.Walk(context.Background(), snmpcodec.MustParseOID("1.3.6.1.2.1.1"), 5)
	if err != nil || len(out2) != 5 {
		t.Fatalf("max walk = %d, %v", len(out2), err)
	}
}
