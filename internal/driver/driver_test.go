package driver

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"uic/internal/model"
)

type metaDriver struct{ m Meta }

func (d metaDriver) Meta() Meta { return d.m }
func (d metaDriver) Open(context.Context, OpenConfig) (Conn, error) {
	return nil, errors.New("not used")
}

func baseMeta() Meta {
	return Meta{
		ID: "t-meta", Name: "meta", SDKVersion: SDKVersion,
		Roles:        []Role{RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: Capabilities{Read: CapSupported, Write: CapSupported},
	}
}

func TestRegisterRejectsInvalidMeta(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Meta)
		want string
	}{
		{"empty id", func(m *Meta) { m.ID = "" }, "empty id"},
		{"sdk mismatch", func(m *Meta) { m.SDKVersion = "0.9" }, "sdkVersion"},
		{"no address kinds", func(m *Meta) { m.AddressKinds = nil }, "addressKinds required"},
	}
	for _, tc := range cases {
		m := baseMeta()
		tc.mut(&m)
		err := Register(metaDriver{m: m})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
		if _, ok := Get(m.ID); ok && m.ID != "" {
			t.Fatalf("%s: invalid driver must not be registered", tc.name)
		}
	}

	// 合法元数据可以注册，重复注册必须失败。
	ok := baseMeta()
	ok.ID = "t-dup"
	d := metaDriver{m: ok}
	if err := Register(d); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := Register(d); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate register err = %v", err)
	}
	if _, found := Get("t-dup"); !found {
		t.Fatal("Get after register")
	}
	if _, found := Get("nope"); found {
		t.Fatal("Get for unknown id")
	}

	// List 必须按 ID 排序。
	list := List()
	for i := 1; i < len(list); i++ {
		if list[i-1].ID > list[i].ID {
			t.Fatalf("List not sorted: %s > %s", list[i-1].ID, list[i].ID)
		}
	}
}

// 驱动元数据：Accepts 决定配置校验是否放行。
func TestMetaAccepts(t *testing.T) {
	m := baseMeta()
	if !m.Accepts(model.AddressRegister) {
		t.Fatal("should accept register")
	}
	if m.Accepts(model.AddressSymbol) {
		t.Fatal("should not accept symbol")
	}
}

func TestPointRefOfInheritsDeviceByteOrder(t *testing.T) {
	dev := model.Device{ByteOrder: model.OrderCDAB}
	p := model.Point{
		ID: "p1", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Bit: -1},
	}
	ref := PointRefOf(dev, p)
	if ref.ByteOrder != model.OrderCDAB {
		t.Fatalf("byteOrder = %q, want device default CDAB", ref.ByteOrder)
	}
	if ref.Length != 2 {
		t.Fatalf("length = %d, want 2 (float32)", ref.Length)
	}

	// 点位显式覆盖设备默认。
	p.ByteOrder = model.OrderBADC
	if ref := PointRefOf(dev, p); ref.ByteOrder != model.OrderBADC {
		t.Fatalf("point override lost: %q", ref.ByteOrder)
	}
}

func TestErrorClassification(t *testing.T) {
	oe := E(ErrTimeout, "read", "d1", "no response", true, nil)
	if KindOf(oe) != ErrTimeout {
		t.Fatalf("kind = %s", KindOf(oe))
	}
	if !IsRetryable(oe) {
		t.Fatal("timeout should be retryable")
	}
	if !CountsTowardBreaker(oe) {
		t.Fatal("timeout should count toward breaker")
	}

	// 地址/权限/不支持错误：重试无意义，也不应熔断设备。
	addr := E(ErrAddress, "read", "d1", "bad register", false, nil)
	if IsRetryable(addr) {
		t.Fatal("address error must not be retryable")
	}
	if CountsTowardBreaker(addr) {
		t.Fatal("address error must not trip the breaker")
	}
	perm := E(ErrPermission, "write", "d1", "read-only", false, nil)
	if CountsTowardBreaker(perm) {
		t.Fatal("permission error must not trip the breaker")
	}

	// context 超时归类为 timeout；未知错误归 internal。
	if KindOf(context.DeadlineExceeded) != ErrTimeout {
		t.Fatal("deadline must map to timeout")
	}
	plain := errors.New("boom")
	if KindOf(plain) != ErrInternal {
		t.Fatalf("kind = %s", KindOf(plain))
	}
	if IsRetryable(plain) || !CountsTowardBreaker(plain) {
		t.Fatal("plain error classification wrong")
	}

	// 错误链：包装后仍能取回分类与临时性。
	wrapped := errors.Join(oe)
	var target *OpError
	if !errors.As(wrapped, &target) || target.Kind != ErrTimeout || !target.Temporary {
		t.Fatalf("As lost the kind: %+v", target)
	}
	if !strings.Contains(wrapped.Error(), "no response") {
		t.Fatalf("error text = %q", wrapped.Error())
	}
}

type stubConn struct{ closes atomic.Int64 }

func (s *stubConn) Probe(context.Context) error { return nil }
func (s *stubConn) Read(context.Context, []ReadSpan) ([]RawSample, error) {
	return nil, nil
}
func (s *stubConn) Write(context.Context, []WriteRequest) ([]WriteResult, error) {
	return nil, nil
}
func (s *stubConn) Close() error { s.closes.Add(1); return nil }

// Conn 契约：Close 必须幂等（重连路径会重复关闭）。
func TestConnContractWithStub(t *testing.T) {
	c := &stubConn{}
	ctx := context.Background()
	if err := c.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

var (
	_ Driver = metaDriver{}
	_ Conn   = (*stubConn)(nil)
)

// TestIsLinkFailure 钉住「链路级」判据的边界。
//
// 这个谓词决定「一个点位失败之后还要不要继续读剩下的点位」。放宽到地址错误
// 会让一个写错的属性名短路整轮、好点位跟着遭殃；收紧到只看 internal 会让
// 真超时被当成点级问题，一轮采集拖成 N 倍超时。两条边界都必须锁住。
func TestIsLinkFailure(t *testing.T) {
	link := []error{
		E(ErrTimeout, "read", "d1", "no response", true, nil),
		E(ErrDisconnected, "read", "d1", "peer closed", true, nil),
		context.DeadlineExceeded,
	}
	for _, err := range link {
		if !IsLinkFailure(err) {
			t.Fatalf("%v 应判为链路级", err)
		}
	}
	point := []error{
		E(ErrAddress, "read", "d1", "unknown property", false, nil),
		E(ErrPermission, "read", "d1", "denied", false, nil),
		E(ErrUnsupported, "read", "d1", "unsupported", false, nil),
		E(ErrProtocol, "read", "d1", "malformed reply", false, nil),
		errors.New("boom"),
	}
	for _, err := range point {
		if IsLinkFailure(err) {
			t.Fatalf("%v 不该判为链路级：会把点级问题升级成整轮失败", err)
		}
	}
	if IsLinkFailure(nil) {
		t.Fatal("nil 不是链路失败")
	}
}

// TestPartialReadError 钉住部分失败契约的判定本身（docs §3.3.1）。
//
// 这是全仓复用的一条闸门：有成功点则吞掉错误（好样本必须保留），
// 全失败则上抛第一个错误（熔断器要看到「设备全坏」）。两个方向都要锁，
// 只锁一个方向的闸门写法会在另一个方向上静默失效。
func TestPartialReadError(t *testing.T) {
	e := E(ErrProtocol, "read", "d1", "boom", false, nil)
	if got := PartialReadError(3, e); got != nil {
		t.Fatalf("有成功点时必须返回 nil，得到 %v", got)
	}
	if got := PartialReadError(0, e); got != e {
		t.Fatalf("全部失败时必须返回第一个错误，得到 %v", got)
	}
	if got := PartialReadError(0, nil); got != nil {
		t.Fatalf("没有错误时返回 nil，得到 %v", got)
	}
	if got := PartialReadError(5, nil); got != nil {
		t.Fatalf("没有错误时返回 nil，得到 %v", got)
	}
}

// TestLooksSecret 钉住「名字看起来像凭据」的启发式。它是脱敏与一致性套件
// 共用的单一事实源：漏掉一种写法，该字段就会在 GET /config 里明文回显。
func TestLooksSecret(t *testing.T) {
	secret := []string{
		"password", "Password", "db.password", "labels.authPassword",
		"passwd", "pwd", "token", "accessToken", "secret", "clientSecret",
		"apikey", "api_key", "credential", "credentialId",
		// community 是 SNMP 团体名——名字里没有 password/token，但它是
		// 实打实的只读口令。这条用例防止有人把 community 从片段表里删掉。
		"community", "labels.community", "snmp.community",
		"privateKey", "private_key",
	}
	for _, name := range secret {
		if !LooksSecret(name) {
			t.Errorf("LooksSecret(%q) = false，want true", name)
		}
	}
	plain := []string{
		"", "timeout", "unitId", "transport.address", "address.offset",
		"dataType", "interval", "concurrency", "labels.version", "labels.username",
	}
	for _, name := range plain {
		if LooksSecret(name) {
			t.Errorf("LooksSecret(%q) = true，want false（普通字段被误当凭据会导致无法查看配置）", name)
		}
	}
}
