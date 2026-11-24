package runtime

import (
	"testing"

	"uic/internal/driver"
	"uic/internal/driver/conformance"
)

// TestDriversConform 把一致性套件跑遍注册表：任何一个内置驱动的元数据违约
// 都会在这里变红。套件本身的自测在 internal/driver/conformance 里。
//
// 为什么放在 runtime 包：它 import 了全部驱动包，注册表天然是完整的；
// 放到 driver 包会成环（驱动 → driver → 驱动）。
func TestDriversConform(t *testing.T) {
	if err := RegisterBuiltins(); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	conformance.CheckRegistry(t)

	for _, m := range driver.List() {
		d, ok := driver.Get(m.ID)
		if !ok {
			t.Fatalf("注册表里的 %q 取不到实例", m.ID)
		}
		t.Run(m.ID, func(t *testing.T) {
			conformance.Check(t, d)
		})
	}
}
