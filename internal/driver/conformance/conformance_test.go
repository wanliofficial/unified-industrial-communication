package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

// recordingReporter 收集失败信息，用于断言「套件真的会报错」——没有这层
// 自测，套件本身可能因为写错条件而永远绿。
type recordingReporter struct {
	failures []string
}

func (r *recordingReporter) Helper() {}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) joined() string { return strings.Join(r.failures, "\n") }

// fakeDriver 让 Meta() 可注入；Open 不会被调用。
type fakeDriver struct {
	meta func() driver.Meta
}

func (f fakeDriver) Meta() driver.Meta { return f.meta() }

func (f fakeDriver) Open(context.Context, driver.OpenConfig) (driver.Conn, error) {
	return nil, nil
}

func unsupported() driver.Capabilities {
	return driver.Capabilities{
		Read: driver.CapSupported, Write: driver.CapUnsupported,
		Subscribe: driver.CapUnsupported, Browse: driver.CapUnsupported,
		Discovery: driver.CapUnsupported, MultiRead: driver.CapUnsupported,
		BitAccess: driver.CapUnsupported, StructTypes: driver.CapUnsupported,
	}
}

// goodMeta 是一份完全合规的元数据：任何一条检查都不该报错。
func goodMeta() driver.Meta {
	return driver.Meta{
		ID:           "good",
		Name:         "Good Driver",
		Version:      "1.2.3",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp", "serial"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: func() driver.Capabilities {
			c := unsupported()
			c.Write = driver.CapSupported
			c.BitAccess = driver.CapSupported
			return c
		}(),
		ConfigFields: []driver.ConfigField{
			{Name: "timeout", Label: "超时", Type: "duration", Scope: "channel"},
			{Name: "unitId", Label: "站号", Type: "int", Scope: "device"},
		},
		PointFields: []driver.ConfigField{
			{Name: "dataType", Label: "类型", Type: "enum", Scope: "point", Enum: []string{"uint16", "float32"}},
		},
		Description: "good",
	}
}

// 合规驱动必须零失败——否则套件会「因为报错太多而没人看」。
func TestCheckAcceptsGoodDriver(t *testing.T) {
	r := &recordingReporter{}
	Check(r, fakeDriver{meta: goodMeta})
	if len(r.failures) != 0 {
		t.Fatalf("合规驱动被误报 %d 条：\n%s", len(r.failures), r.joined())
	}
}

// TestCheckCatchesBrokenMetas 是套件自身的质量门：每一种违约都必须被抓到。
// 少一条检查，这里就少一个用例变红。
func TestCheckCatchesBrokenMetas(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *driver.Meta)
		want   string
	}{
		{"空 ID", func(m *driver.Meta) { m.ID = "" }, "ID 为空"},
		{"大写 ID", func(m *driver.Meta) { m.ID = "Bad" }, "不合法"},
		{"名称首尾空白", func(m *driver.Meta) { m.Name = " good " }, "首尾有空白"},
		{"名称空", func(m *driver.Meta) { m.Name = "" }, "name 为空"},
		{"版本为空", func(m *driver.Meta) { m.Version = "" }, "version 为空"},
		{"版本非 semver", func(m *driver.Meta) { m.Version = "1.0" }, "不是 x.y.z"},
		{"SDK 版本不符", func(m *driver.Meta) { m.SDKVersion = "0.9" }, "sdkVersion"},
		{"角色为空", func(m *driver.Meta) { m.Roles = nil }, "roles 为空"},
		{"未知角色", func(m *driver.Meta) { m.Roles = []driver.Role{"server"} }, "未知 role"},
		{"角色重复", func(m *driver.Meta) {
			m.Roles = []driver.Role{driver.RoleMaster, driver.RoleMaster}
		}, "role"},
		{"传输为空", func(m *driver.Meta) { m.Transports = nil }, "transports 为空"},
		{"未知传输", func(m *driver.Meta) { m.Transports = []string{"bluetooth"} }, "未知 transport"},
		{"传输重复", func(m *driver.Meta) { m.Transports = []string{"tcp", "tcp"} }, "transport"},
		{"寻址为空", func(m *driver.Meta) { m.AddressKinds = nil }, "addressKinds 为空"},
		{"未知寻址", func(m *driver.Meta) {
			m.AddressKinds = []model.AddressKind{"bogus"}
		}, "未知 addressKind"},
		{"read 不可用", func(m *driver.Meta) { m.Capabilities.Read = driver.CapUnsupported }, "必须 supported"},
		{"能力状态非法", func(m *driver.Meta) { m.Capabilities.Write = "yes" }, "capabilities.write"},
		{"位访问无寄存器", func(m *driver.Meta) {
			m.AddressKinds = []model.AddressKind{model.AddressSymbol}
			m.Capabilities.BitAccess = driver.CapSupported
		}, "bitAccess"},
		{"字段名为空", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{{Name: "", Label: "x", Type: "string", Scope: "channel"}}
		}, "字段名"},
		{"字段名含空格", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{{Name: "bad name", Label: "x", Type: "string", Scope: "channel"}}
		}, "字段名"},
		{"label 为空", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{{Name: "x", Label: "", Type: "string", Scope: "channel"}}
		}, "label 为空"},
		{"未知类型", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{{Name: "x", Label: "x", Type: "text", Scope: "channel"}}
		}, "未知 type"},
		{"未知 scope", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{{Name: "x", Label: "x", Type: "string", Scope: "global"}}
		}, "未知 scope"},
		{"枚举无值", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{{Name: "x", Label: "x", Type: "enum", Scope: "channel"}}
		}, "没有可选值"},
		{"同 scope 字段重名", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{
				{Name: "x", Label: "x", Type: "string", Scope: "channel"},
				{Name: "x", Label: "y", Type: "string", Scope: "channel"},
			}
		}, "重复"},
		{"凭据字段未声明 secret", func(m *driver.Meta) {
			m.ConfigFields = []driver.ConfigField{
				{Name: "labels.community", Label: "团体名", Type: "string", Scope: "device"},
			}
		}, "未声明 secret"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodMeta()
			tc.mutate(&m)
			r := &recordingReporter{}
			Check(r, fakeDriver{meta: func() driver.Meta { return m }})
			if len(r.failures) == 0 {
				t.Fatalf("违约（%s）未被检出", tc.name)
			}
			if !strings.Contains(r.joined(), tc.want) {
				t.Fatalf("失败信息未包含 %q：\n%s", tc.want, r.joined())
			}
		})
	}
}

// Meta() 每次返回不同内容（例如内部累加、或共享切片被改）也必须被抓到。
func TestCheckCatchesUnstableMeta(t *testing.T) {
	n := 0
	d := fakeDriver{meta: func() driver.Meta {
		n++
		m := goodMeta()
		m.Version = fmt.Sprintf("1.0.%d", n)
		return m
	}}
	r := &recordingReporter{}
	Check(r, d)
	if !strings.Contains(r.joined(), "两次调用结果不一致") {
		t.Fatalf("Meta() 不稳定未被检出：\n%s", r.joined())
	}
}

// 空注册表必须显式报错：否则「遍历空列表」会让所有逐驱动测试假绿。
func TestCheckRegistryRejectsEmpty(t *testing.T) {
	// conformance 包不 import 任何驱动包，因此这里的注册表为空。
	r := &recordingReporter{}
	CheckRegistry(r)
	if !strings.Contains(r.joined(), "注册表为空") {
		t.Fatalf("空注册表未被检出：\n%s", r.joined())
	}
}

// 套件内置的最小地址/通道必须是合法样本，否则「合规驱动被误报」会是套件
// 自己的 bug。
func TestMinimalSamplesAreValid(t *testing.T) {
	for _, k := range []model.AddressKind{
		model.AddressRegister, model.AddressSymbol, model.AddressObject,
		model.AddressFile, model.AddressComputed,
	} {
		addr, ok := minimalAddress(k)
		if !ok {
			t.Fatalf("minimalAddress(%q) 未覆盖", k)
		}
		if err := addr.Validate(); err != nil {
			t.Fatalf("minimalAddress(%q) 不合法：%v", k, err)
		}
		if addr.Canonical() == "" {
			t.Fatalf("minimalAddress(%q) 的 Canonical 为空", k)
		}
	}
	for _, transport := range []string{"tcp", "udp", "serial", "memory"} {
		if err := minimalChannel("good", transport).Validate(); err != nil {
			t.Fatalf("minimalChannel(%q) 不合法：%v", transport, err)
		}
	}
	if _, ok := minimalAddress("bogus"); ok {
		t.Fatal("未知寻址范式应返回 false")
	}
}
