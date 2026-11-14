package runtime

import (
	"testing"

	"uic/internal/driver"
)

// TestBuiltinDriversMetaContract 对**全部**内置驱动跑一遍元数据契约检查。
//
// 为什么需要它：能力位是 8 个字段的字符串枚举，漏写一个字段会序列化成
// 空串——既不是 "supported" 也不是 "unsupported"，前端会把它渲染成空白，
// 而每个驱动各自的 TestMetaContract 只断言它「记得写」的那几项，正好漏掉
// 忘写的那个。SL 651 曾长期只声明 6/8（缺 Discovery/BitAccess），全仓测试
// 全绿，就是这个盲区造成的。
//
// 单点自检要求每个驱动自己记得检查全部 8 项；这里再加一道**全仓**兜底：
// 新写的驱动哪怕完全没写元数据测试，也会被这条拦下。
func TestBuiltinDriversMetaContract(t *testing.T) {
	// RegisterBuiltins 是幂等的：其它测试可能已经注册过。
	if err := RegisterBuiltins(); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	all := driver.List()
	if len(all) == 0 {
		t.Fatal("no drivers registered")
	}

	states := map[driver.CapState]bool{
		driver.CapSupported:        true,
		driver.CapUnsupported:      true,
		driver.CapRuntimeDependent: true,
		driver.CapExperimental:     true,
	}

	for _, m := range all {
		t.Run(m.ID, func(t *testing.T) {
			if m.Name == "" {
				t.Error("Name 为空")
			}
			if m.Description == "" {
				t.Error("Description 为空（前端展示与排障都依赖它）")
			}
			if len(m.Roles) == 0 {
				t.Error("Roles 为空")
			}
			if len(m.Transports) == 0 {
				t.Error("Transports 为空")
			}
			if len(m.AddressKinds) == 0 {
				t.Error("AddressKinds 为空（点表导入依赖它放行地址种类）")
			}
			if m.SDKVersion != driver.SDKVersion {
				t.Errorf("SDKVersion = %q, want %q", m.SDKVersion, driver.SDKVersion)
			}

			// 8 个能力位必须全部落在四态内：漏写即空串，在这里立刻暴露。
			caps := []struct {
				name string
				st   driver.CapState
			}{
				{"Read", m.Capabilities.Read},
				{"Write", m.Capabilities.Write},
				{"Subscribe", m.Capabilities.Subscribe},
				{"Browse", m.Capabilities.Browse},
				{"Discovery", m.Capabilities.Discovery},
				{"MultiRead", m.Capabilities.MultiRead},
				{"BitAccess", m.Capabilities.BitAccess},
				{"StructTypes", m.Capabilities.StructTypes},
			}
			for _, c := range caps {
				if !states[c.st] {
					t.Errorf("Capabilities.%s = %q，不是能力四态之一（漏写字段会序列化成空串）", c.name, c.st)
				}
			}

			// 「声明支持读」的驱动必须真的能被点表地址放行：AddressKinds
			// 与 pointtable 策略是两处独立维护的事实，漂移的表现是点表
			// 导入通过、运行时每个周期都 address_error。
			if m.Capabilities.Read == driver.CapSupported && len(m.AddressKinds) == 0 {
				t.Error("声明 Read=supported 却没有 AddressKinds")
			}
			for _, f := range m.ConfigFields {
				if f.Name == "" {
					t.Error("ConfigField 缺 Name")
				}
				if f.Scope == "" {
					t.Errorf("ConfigField %q 缺 Scope", f.Name)
				}
			}
		})
	}
}
