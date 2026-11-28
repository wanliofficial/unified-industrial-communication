package driver_test

// 本文件是「能力声明与规范文档对齐」的集中回归测试。
//
// docs/03-product/04-driver-sdk-spec.md §5.2 是一张人读的「Wave 1 驱动能力黄金
// 参考表」：17 个驱动，每个 7 个能力维度（Read/Write/Subscribe/Browse/Discovery/
// MultiRead/BitAccess）的 supported/unsupported/experimental 取值。此前这些取值
// 散落在各驱动自己的 *_test.go 里零散断言，没有任何一处集中校验「代码里的能力
// 声明 == 规范表」——表是人读文档，会随时间悄悄漂移，而代码里的 Meta().Capabilities
// 是机器读的运行时事实，一旦两者分叉，配置界面会给用户错误的协议建议（例如把
// 协议不支持的订阅能力标成 unsupported，引导用户换协议，方向就错了）。
//
// 这里把 §5.2 表逐行翻译成 Go 字面量作为唯一事实源，对每个驱动的真实 New().Meta()
// 逐维度断言。新驱动（sl651/mtconnect/coap 等，不在 §5.2 Wave 1 表内）不受此表约束，
// 由 conformance 套件的自洽性检查（能力四态合法、read 必 supported、能力与寻址匹配）
// 守护；本表只钉死 Wave 1 基线。
//
// 用外部测试包 driver_test：被导入的驱动子包本身 import 了 internal/driver，若本测试
// 用内部包 driver 会形成 import cycle；外部测试包与 driver 包分别编译，可干净导入。

import (
	"testing"

	"uic/internal/driver"
	"uic/internal/driver/ads"
	"uic/internal/driver/bacnet"
	"uic/internal/driver/cjt188"
	"uic/internal/driver/computed"
	"uic/internal/driver/dlt645"
	"uic/internal/driver/enip"
	"uic/internal/driver/fins"
	"uic/internal/driver/iec104"
	"uic/internal/driver/mc"
	"uic/internal/driver/modbus"
	"uic/internal/driver/mqtt"
	"uic/internal/driver/opcua"
	"uic/internal/driver/s7"
	"uic/internal/driver/snmp"
	"uic/internal/driver/virtual"
)

// goldenEntry 是 §5.2 表的一行：驱动 ID + 七维能力声明。
//
// 只覆盖 §5.2 表列出的 7 个维度；StructTypes 不在表内，故不在此断言（它的取值由
// conformance 自洽性检查守护）。所有 §5.2 脚注（¹–¹⁰）只澄清能力语义、不改变
// CapState 取值本身，因此预期值就是表里写的 supported/unsupported/experimental。
type goldenEntry struct {
	id   string
	caps driver.Capabilities
}

// wave1Golden 是 spec §5.2「Wave 1 驱动能力黄金参考表」的逐行机读副本。
//
// 这是唯一事实源：任何驱动的 New().Meta().Capabilities 偏离此表，对应行就变红。
// 表的来源是 docs/03-product/04-driver-sdk-spec.md §5.2（第 9952 行起）。
func wave1Golden() []goldenEntry {
	sup := driver.CapSupported
	unsup := driver.CapUnsupported

	// masterRW 是 Modbus/S7/MC/FINS/ENIP 这类「可读可写、不支持订阅/浏览/发现、
	// 支持批量读与位访问」的工业主站通用形态，§5.2 里它们七维取值完全一致。
	masterRW := driver.Capabilities{
		Read: sup, Write: sup, Subscribe: unsup, Browse: unsup,
		Discovery: unsup, MultiRead: sup, BitAccess: sup,
	}
	// meterRW 是 645/188 这类「可读可写但逐点串行（MultiRead unsupported）、无位访问」
	// 的计量仪表形态。
	meterRW := driver.Capabilities{
		Read: sup, Write: sup, Subscribe: unsup, Browse: unsup,
		Discovery: unsup, MultiRead: unsup, BitAccess: unsup,
	}

	return []goldenEntry{
		{"modbus-tcp", masterRW},
		{"modbus-rtu", masterRW},
		{"modbus-ascii", masterRW},
		{"s7", masterRW},
		{"mc", masterRW},
		{"fins", masterRW},
		{"enip", masterRW},
		// BACnet 连批量读都不支持（对象属性逐条读），MultiRead/BitAccess 均 unsupported。
		{"bacnet", driver.Capabilities{
			Read: sup, Write: sup, Subscribe: unsup, Browse: unsup,
			Discovery: unsup, MultiRead: unsup, BitAccess: unsup,
		}},
		// IEC 104 从站变化主动上送：驱动内部消化成最新值表，Subscribe 声明 supported
		// （脚注⁵），但 MultiRead 也 supported（只读内存表，脚注⁶）。
		{"iec104", driver.Capabilities{
			Read: sup, Write: sup, Subscribe: sup, Browse: unsup,
			Discovery: unsup, MultiRead: sup, BitAccess: unsup,
		}},
		{"dlt645", meterRW},
		{"cjt188", meterRW},
		// SNMP 是协议真实支持「一条 GET 带多 varbind」，MultiRead supported（脚注⁹）。
		{"snmp", driver.Capabilities{
			Read: sup, Write: sup, Subscribe: unsup, Browse: unsup,
			Discovery: unsup, MultiRead: sup, BitAccess: unsup,
		}},
		// ADS 主动放弃 MultiRead（一次失败无法逐点归因，脚注¹⁰），故 unsupported。
		{"ads", meterRW},
		// OPC UA 服务端推送：Subscribe supported（脚注⁴）。Discovery 如实声明
		// unsupported——驱动没有 GetEndpoints/FindServers 发现入口，早先误标
		// experimental 会让 UI 出现死按钮（脚注¹¹，以代码为准，§5.2 表已追齐）。
		{"opcua", driver.Capabilities{
			Read: sup, Write: sup, Subscribe: sup, Browse: unsup,
			Discovery: unsup, MultiRead: sup, BitAccess: unsup,
		}},
		// MQTT 南向订阅推送：Subscribe supported（脚注¹），批量读查内存表
		// MultiRead supported（脚注²）。
		{"mqtt", driver.Capabilities{
			Read: sup, Write: sup, Subscribe: sup, Browse: unsup,
			Discovery: unsup, MultiRead: sup, BitAccess: unsup,
		}},
		// 计算点语义上不可写回（无地址，脚注³），故 Write unsupported；其余读类能力
		// 全 supported（内存计算，无协议交互）。
		{"computed", driver.Capabilities{
			Read: sup, Write: unsup, Subscribe: unsup, Browse: unsup,
			Discovery: unsup, MultiRead: sup, BitAccess: unsup,
		}},
		// virtual 是内存模拟设备：批量读/位访问 supported。Subscribe/Browse 如实
		// unsupported——Conn 契约只有 Probe/Read/Write/Close，没有任何订阅/浏览
		// 代码路径；虚标 supported 会让 UI 出现死按钮（脚注¹²，以代码为准，§5.2 表已追齐）。
		{"virtual", driver.Capabilities{
			Read: sup, Write: sup, Subscribe: unsup, Browse: unsup,
			Discovery: unsup, MultiRead: sup, BitAccess: sup,
		}},
	}
}

// metaOf 返回指定驱动 ID 的真实能力元数据（直接调 New().Meta()，不依赖注册状态：
// Meta() 是静态声明，与运行时注册表无关，这样测试不依赖哪些子包恰好被注册）。
func metaOf(id string) driver.Meta {
	switch id {
	case "modbus-tcp":
		return modbus.New().Meta()
	case "modbus-rtu":
		return modbus.NewRTU().Meta()
	case "modbus-ascii":
		return modbus.NewASCII().Meta()
	case "s7":
		return s7.New().Meta()
	case "mc":
		return mc.New().Meta()
	case "fins":
		return fins.New().Meta()
	case "enip":
		return enip.New().Meta()
	case "bacnet":
		return bacnet.New().Meta()
	case "iec104":
		return iec104.New().Meta()
	case "dlt645":
		return dlt645.New().Meta()
	case "cjt188":
		return cjt188.New().Meta()
	case "snmp":
		return snmp.New().Meta()
	case "ads":
		return ads.New().Meta()
	case "opcua":
		return opcua.New().Meta()
	case "mqtt":
		return mqtt.New().Meta()
	case "computed":
		return computed.New().Meta()
	case "virtual":
		return virtual.New().Meta()
	default:
		panic("spec52: unknown driver id " + id)
	}
}

// TestSpec52Capabilities 把 §5.2 黄金表逐行钉死：每个 Wave 1 驱动的真实
// New().Meta().Capabilities 必须与规范表逐维度吻合，且 Meta().ID 必须与表行 ID
// 一致（防止驱动改名后表与实现对不上）。
func TestSpec52Capabilities(t *testing.T) {
	for _, e := range wave1Golden() {
		t.Run(e.id, func(t *testing.T) {
			m := metaOf(e.id)
			if m.ID != e.id {
				t.Fatalf("Meta().ID = %q, want %q（驱动 ID 与 §5.2 表行不一致）", m.ID, e.id)
			}
			assertCaps(t, "Meta().Capabilities", m.Capabilities, e.caps)
		})
	}
}

// assertCaps 逐维度比对两个 Capabilities，只比对 §5.2 表覆盖的 7 个维度
// （StructTypes 不在表内，跳过）。
func assertCaps(t *testing.T, where string, got, want driver.Capabilities) {
	t.Helper()
	type dim struct {
		name string
		got  driver.CapState
		want driver.CapState
	}
	dims := []dim{
		{"Read", got.Read, want.Read},
		{"Write", got.Write, want.Write},
		{"Subscribe", got.Subscribe, want.Subscribe},
		{"Browse", got.Browse, want.Browse},
		{"Discovery", got.Discovery, want.Discovery},
		{"MultiRead", got.MultiRead, want.MultiRead},
		{"BitAccess", got.BitAccess, want.BitAccess},
	}
	for _, d := range dims {
		if d.got != d.want {
			t.Errorf("%s.%s = %q, want %q（与 spec §5.2 表不符）", where, d.name, d.got, d.want)
		}
	}
}
