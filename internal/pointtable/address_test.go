package pointtable

import (
	"fmt"
	"strings"
	"testing"

	"uic/internal/model"
)

func TestParseAddressRegister(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		dt       model.DataType
		protocol string
		want     string
	}{
		{"modbus float", "holding:100", model.TypeFloat32, "modbus-tcp", "holding:100:2"},
		{"modbus int16", "input:7", model.TypeInt16, "modbus-tcp", "input:7:1"},
		{"modbus coil", "coil:5", model.TypeBool, "modbus-tcp", "coil:5:1"},
		{"modbus discrete", "discrete:9", model.TypeBool, "modbus-tcp", "discrete:9:1"},
		{"modbus bit", "holding:100.3", model.TypeBool, "modbus-tcp", "holding:100.3:1"},
		{"s7 float", "db1:0", model.TypeFloat32, "s7", "db1:0:4"},
		{"s7 bit", "db1:0.3", model.TypeBool, "s7", "db1:0.3:1"},
		{"s7 explicit length", "db1:0:7", model.TypeString, "s7", "db1:0:7"},
		{"s7 db colon form", "db:2:4:2", model.TypeUint16, "s7", "db2:4:2"},
		{"s7 input", "input:0", model.TypeUint16, "s7", "input:0:2"},
		{"symbol", "ns=2;s=Plant.Temp", model.TypeFloat64, "opcua", "ns=2;s=Plant.Temp"},
		{"mqtt selector", "plant/line1#data.temp", model.TypeFloat32, "mqtt", "plant/line1#data.temp"},
		{"symbol index", "Kitchen.Temp[3]", model.TypeFloat64, "opcua", "Kitchen.Temp[3]"},
		{"computed", "expr:line-a:power / line-a:rate", model.TypeFloat64, "computed", "expr:line-a:power / line-a:rate"},
		{"file", "file:1/0:5", model.TypeFloat64, "custom", "file:1/0:5"},
		{"object", "analogInput:1.presentValue", model.TypeFloat32, "custom", "analogInput:1.presentValue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, tc.protocol)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Canonical(); got != tc.want {
				t.Fatalf("canonical = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseAddressRejects(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		dt       model.DataType
		protocol string
		want     string
	}{
		{"empty", "", model.TypeFloat32, "modbus-tcp", "required"},
		{"control char", "holding:1\x01", model.TypeUint16, "modbus-tcp", "control"},
		{"negative offset", "holding:-1", model.TypeUint16, "modbus-tcp", "non-negative"},
		{"bad offset", "holding:abc", model.TypeUint16, "modbus-tcp", "non-negative"},
		{"bit on float", "holding:1.2", model.TypeFloat32, "modbus-tcp", "only valid for bool"},
		{"s7 bit 8", "db1:0.8", model.TypeBool, "s7", "0..7"},
		{"bit 16", "holding:0.16", model.TypeBool, "modbus-tcp", "0..15"},
		{"holding bool no bit", "holding:0", model.TypeBool, "modbus-tcp", "needs a bit"},
		{"s7 bool no bit", "db1:0", model.TypeBool, "s7", "needs a bit"},
		{"length zero", "holding:0:0", model.TypeUint16, "modbus-tcp", "positive"},
		{"length too big", "holding:0:126", model.TypeUint16, "modbus-tcp", "<= 125"},
		{"too many fields", "holding:0:1:2", model.TypeUint16, "modbus-tcp", "too many"},
		{"string needs length", "db1:0", model.TypeString, "s7", "length is required"},
		{"modbus area", "db1:0", model.TypeUint16, "modbus-tcp", "not valid"},
		{"modbus symbol", "ns=2;s=x", model.TypeFloat32, "modbus-tcp", "symbol"},
		{"s7 area", "holding:0", model.TypeUint16, "s7", "not valid"},
		{"opcua register", "holding:0", model.TypeUint16, "opcua", "register"},
		{"opcua object", "analogInput:1.presentValue", model.TypeFloat32, "opcua", "object"},
		{"mqtt computed", "expr:1+1", model.TypeFloat64, "mqtt", "computed"},
		{"s7 db zero", "db0:0:2", model.TypeUint16, "s7", "db"},
		{"empty expr", "expr:  ", model.TypeFloat64, "computed", "empty"},
		{"empty selector", "plant/x#", model.TypeFloat32, "mqtt", "empty"},
		{"bad file", "file:1/0", model.TypeFloat64, "custom", "file:"},
		{"unknown width", "holding:0", model.TypeFloat32, "custom", "length is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddress(tc.expr, tc.dt, tc.protocol)
			if err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 点表导入发生在 CI 里，此时没有任何驱动；地址策略必须是静态表，
// 不能依赖「连上设备试试」。
func TestAddressPolicyStatic(t *testing.T) {
	if p := PolicyFor("modbus-rtu"); p.Width != WidthRegisters || !p.AllowRegister {
		t.Fatalf("modbus-rtu = %+v", p)
	}
	if p := PolicyFor("s7"); p.Width != WidthBytes {
		t.Fatalf("s7 = %+v", p)
	}
	if p := PolicyFor("opcua"); p.AllowRegister || !p.AllowSymbol {
		t.Fatalf("opcua = %+v", p)
	}
	if p := PolicyFor("snmp"); p.AllowRegister || !p.AllowSymbol || p.AllowSelector {
		t.Fatalf("snmp = %+v", p)
	}
	if p := PolicyFor("ads"); !p.AllowSymbol || !p.AllowSymbolLength || !p.SymbolIndexInPath || p.AllowSelector {
		t.Fatalf("ads = %+v", p)
	}
	// SECS 用对象地址：编号是协议原生整数，宽度由点表声明（同一编号可能是
	// U4、A 或 F8，点表层不复制 SECS-II 的格式表）。
	if p := PolicyFor("secs"); !p.AllowObject || p.AllowRegister || p.AllowSymbol || p.Width != WidthProtocol {
		t.Fatalf("secs = %+v", p)
	}
	if p := PolicyFor("weird"); !p.AllowRegister || !p.AllowSymbol || p.Width != WidthExplicit {
		t.Fatalf("unknown = %+v", p)
	}
}

// DL/T 645 这类协议按「数据标识」寻址：地址里没有区域名，第一段就是 8 位
// 十六进制的 DI，也没有真正的偏移概念。这一组用例钉住它的三种写法与
// 各种应当被拦住的错法。
func TestParseAddressDI(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		hint int
		want string
	}{
		{"bare DI", "02010100", model.TypeFloat64, 0, "02010100:0:0"},
		{"lowercase DI", "0201ff00", model.TypeBytes, 0, "0201FF00:0:0"},
		{"explicit zero offset", "02010100:0", model.TypeFloat64, 0, "02010100:0:0"},
		{"block with length", "0201FF00:0:20", model.TypeBytes, 0, "0201FF00:0:20"},
		{"length column", "0201FF00", model.TypeBytes, 12, "0201FF00:0:12"},
		{"energy family", "00000000", model.TypeFloat64, 0, "00000000:0:0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddressWithLength(tc.expr, tc.dt, "dlt645", tc.hint)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Canonical(); got != tc.want {
				t.Fatalf("canonical = %q, want %q", got, tc.want)
			}
			if a.Offset != 0 {
				t.Fatalf("offset = %d, want 0", a.Offset)
			}
		})
	}
}

func TestParseAddressDIRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		hint int
		want string
	}{
		{"short DI", "020101", model.TypeFloat64, 0, "8 hex digits"},
		{"non hex", "0201010G", model.TypeFloat64, 0, "non-hex"},
		{"non zero offset", "02010100:4", model.TypeFloat64, 0, "must be 0"},
		{"too many fields", "02010100:0:2:9", model.TypeFloat64, 0, "too many"},
		{"bad length", "02010100:0:0", model.TypeFloat64, 0, "positive"},
		{"all FF", "FFFFFFFF", model.TypeFloat64, 0, "not a valid data identifier"},
		{"bool point", "04000402", model.TypeBool, 0, "bit addressing"},
		{"length conflict", "0201FF00:0:8", model.TypeBytes, 12, "given twice"},
		{"length too big", "0201FF00:0:126", model.TypeBytes, 0, "<= 125"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddressWithLength(tc.expr, tc.dt, "dlt645", tc.hint)
			if err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// DI 策略必须拦住「拿别的协议的地址去喂 645」和「把 645 的 DI 喂给别的
// 协议」两个方向，否则现场会得到「能启动但每个点都读不到」。
func TestPolicyForDI(t *testing.T) {
	p := PolicyFor("dlt645")
	if !p.AllowRegister || p.DIBits != 8 || p.Width != WidthProtocol {
		t.Fatalf("dlt645 = %+v", p)
	}
	if p.AllowSymbol || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
		t.Fatalf("dlt645 accepts non-DI kinds: %+v", p)
	}
	if _, err := ParseAddress("holding:0:2", model.TypeUint16, "dlt645"); err == nil {
		t.Fatal("dlt645 must reject Modbus-style area names")
	}
	if _, err := ParseAddress("02010100", model.TypeFloat64, "modbus-tcp"); err == nil {
		t.Fatal("modbus must reject DI-style addresses")
	}
}

// CJ/T 188 的 DI 只有 2 字节，而一个 DI 是一组字段：offset 是数据域内的
// 字节偏移，必须允许非 0；这与 645「一个 DI 一个值、offset 恒 0」相反。
func TestParseAddressCJT188(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		dt     model.DataType
		hint   int
		want   string
		offset int
	}{
		{"volume", "901F:0", model.TypeFloat64, 0, "901F:0:0", 0},
		{"settlement", "901F:5", model.TypeFloat64, 0, "901F:5:0", 5},
		{"bare DI defaults to 0", "901F", model.TypeFloat64, 0, "901F:0:0", 0},
		{"time", "901F:10", model.TypeFloat64, 0, "901F:10:0", 10},
		{"status", "901F:17", model.TypeUint16, 0, "901F:17:0", 17},
		{"historical", "d12b:0", model.TypeFloat64, 0, "D12B:0:0", 0},
		{"explicit length", "901F:0:5", model.TypeFloat64, 0, "901F:0:5", 0},
		{"length column", "901F:0", model.TypeFloat64, 5, "901F:0:5", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddressWithLength(tc.expr, tc.dt, "cjt188", tc.hint)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Canonical(); got != tc.want {
				t.Fatalf("canonical = %q, want %q", got, tc.want)
			}
			if a.Offset != tc.offset {
				t.Fatalf("offset = %d, want %d", a.Offset, tc.offset)
			}
		})
	}

	bad := []struct {
		name string
		expr string
		dt   model.DataType
		want string
	}{
		{"8-digit DI", "9901F", model.TypeFloat64, "4 hex digits"},
		{"645 DI", "02010100", model.TypeFloat64, "4 hex digits"},
		{"all FF", "FFFF", model.TypeFloat64, "not a valid data identifier"},
		{"non hex", "90G1", model.TypeFloat64, "non-hex"},
		{"negative offset", "901F:-1", model.TypeFloat64, "non-negative"},
		{"bool", "901F:17", model.TypeBool, "bit addressing"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddress(tc.expr, tc.dt, "cjt188")
			if err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestPolicyForCJT188(t *testing.T) {
	p := PolicyFor("cjt188")
	if !p.AllowRegister || p.DIBits != 4 || !p.DIField || p.Width != WidthProtocol {
		t.Fatalf("cjt188 = %+v", p)
	}
	if p.AllowSymbol || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
		t.Fatalf("cjt188 accepts non-DI kinds: %+v", p)
	}
	if _, err := ParseAddress("holding:0:2", model.TypeUint16, "cjt188"); err == nil {
		t.Fatal("cjt188 must reject Modbus-style area names")
	}
	if _, err := ParseAddress("901F:0", model.TypeFloat64, "modbus-tcp"); err == nil {
		t.Fatal("modbus must reject DI-style addresses")
	}
}

func TestParseAddressSNMP(t *testing.T) {
	a, err := ParseAddress("1.3.6.1.2.1.1.1.0", model.TypeString, "snmp")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Kind != model.AddressSymbol || a.Path != "1.3.6.1.2.1.1.1.0" {
		t.Fatalf("address = %+v", a)
	}
	if a.Selector != "" || a.Index > 0 {
		t.Fatalf("snmp address must not gain selector/index: %+v", a)
	}

	// 寄存器/对象/文件写法都必须在导入时被拦下：SNMP 只有 OID 一种地址。
	for _, expr := range []string{"holding:100", "analogInput:1.presentValue", "file:1/0:5"} {
		if _, err := ParseAddress(expr, model.TypeUint16, "snmp"); err == nil {
			t.Fatalf("%q must be rejected for snmp", expr)
		}
	}
	// selector 是 MQTT 的载荷字段语法，SNMP 没有这个位置。
	if _, err := ParseAddress("1.3.6.1.2.1.1.1.0#x", model.TypeString, "snmp"); err == nil {
		t.Fatal("selector must be rejected for snmp")
	}
}

func TestParseAddressADS(t *testing.T) {
	// 数组下标属于符号名：拆成 Address.Index 再拼回去会让 `[0]` 与
	// 「未配置」在 JSON 里无法区分。
	a, err := ParseAddress("MAIN.arr[2]", model.TypeInt16, "ads")
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if a.Kind != model.AddressSymbol || a.Path != "MAIN.arr[2]" || a.Index != -1 {
		t.Fatalf("array address = %+v", a)
	}
	if got := a.Canonical(); got != "MAIN.arr[2]" {
		t.Fatalf("array canonical = %q", got)
	}

	// STRING/bytes 的宽度由点表给出：地址里的 `:81` 与 length 列等价。
	for _, tc := range []struct {
		expr   string
		length int
	}{{"MAIN.sRecipe:81", 0}, {"MAIN.sRecipe", 81}} {
		a, err := ParseAddressWithLength(tc.expr, model.TypeString, "ads", tc.length)
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		if a.Length != 81 || a.Path != "MAIN.sRecipe" {
			t.Fatalf("%q = %+v", tc.expr, a)
		}
		if got := a.Canonical(); got != "MAIN.sRecipe:81" {
			t.Fatalf("%q canonical = %q", tc.expr, got)
		}
	}

	// 两处长度不一致必须在导入期报错，而不是随便选一个。
	if _, err := ParseAddressWithLength("MAIN.sRecipe:81", model.TypeString, "ads", 80); err == nil ||
		!strings.Contains(err.Error(), "given twice") {
		t.Fatalf("conflicting length: %v", err)
	}

	// 寄存器/对象/selector 都必须在导入期拒绝：ADS 只有符号地址。
	for _, expr := range []string{"holding:100", "analogInput:1.presentValue", "MAIN.x#field"} {
		if _, err := ParseAddress(expr, model.TypeUint16, "ads"); err == nil {
			t.Fatalf("%q must be rejected for ads", expr)
		}
	}

	// 其它符号协议不接受显式长度：OPC UA 的宽度来自服务端类型。
	if _, err := ParseAddressWithLength("ns=2;s=Plant.Temp", model.TypeString, "opcua", 81); err == nil {
		t.Fatal("opcua must reject explicit symbol length")
	}
}

// SECS 的对象地址是 `svid:1001.value`：类型 + 编号 + 属性。它必须只对声明
// 了 AllowObject 的协议可用，modbus 这类寄存器协议要明确拒绝而不是当成
// 符号名。
func TestParseSecsObjectAddress(t *testing.T) {
	a, err := ParseAddress("svid:1001.value", model.TypeUint32, "secs")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != model.AddressObject || a.ObjectType != "svid" || a.Instance != 1001 || a.Property != "value" {
		t.Fatalf("地址 = %+v", a)
	}
	if _, err := ParseAddress("ecid:200.value", model.TypeUint32, "secs"); err != nil {
		t.Fatalf("ecid 也应可解析: %v", err)
	}
	// 寄存器协议不接受对象地址。
	if _, err := ParseAddress("svid:1.value", model.TypeUint32, "modbus-tcp"); err == nil {
		t.Fatal("modbus-tcp 必须拒绝对象地址")
	}
	// 属性为空、编号非数字都要在导入时被挡住。
	if _, err := ParseAddress("svid:1.", model.TypeUint32, "secs"); err == nil {
		t.Fatal("空属性必须失败")
	}
	if _, err := ParseAddress("svid:x.value", model.TypeUint32, "secs"); err == nil {
		t.Fatal("非数字编号必须失败")
	}
}

// 远动协议（IEC 60870-5-101/104）用「对象类别 + 信息对象地址」寻址，与
// 寄存器协议最容易混淆：形状一样（`area:offset`），语义完全不同。
//
// 这一组测试钉住四件事，它们各自对应一个现场会踩到的坑：
//
//  1. 地址里**不表达宽度**（`WidthNone`）。值有多宽由 ASDU 类型标识决定，
//     写 `:2` 不表达「读两个寄存器」，只会让驱动在 Open 阶段拒掉整条点位。
//  2. **布尔点不需要位号**。0-based 寄存器里 bool 必须写 `.3`，远动里一个
//     布尔量就是一个单点对象——按寄存器习惯校验会让一份完全正确的远动点表
//     在导入期全数被拒。
//  3. `area` 是**对象类别白名单**：`ioa` 是监视方向，另外五个是控制方向。
//     把 Modbus 的 `holding` 写进来必须明确拒绝，而不是当成未知区域静默
//     接受、等到启动后每个周期返回 address_error。
//  4. 101 与 104 共用同一套地址形态：两者只是承载层不同（串口 / TCP），
//     点表没有任何理由不同。
func TestPolicyForIEC60870(t *testing.T) {
	for _, proto := range []string{"iec101", "iec104"} {
		p := PolicyFor(proto)
		if p.Width != WidthNone {
			t.Fatalf("%s: width = %v, want WidthNone", proto, p.Width)
		}
		if !p.AllowRegister || p.AllowSymbol || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
			t.Fatalf("%s: policy accepts non-register kinds: %+v", proto, p)
		}
		if p.DIBits != 0 || p.DIField || p.AllowSymbolLength || p.SymbolIndexInPath {
			t.Fatalf("%s: policy carries unrelated fields: %+v", proto, p)
		}
		// 六个对象类别一个不能少：少了任何一个，对应的点位在导入期就被拒。
		for _, area := range []string{"ioa", "single-command", "double-command",
			"setpoint-float", "setpoint-scaled", "setpoint-normalized"} {
			if !p.RegisterAreas[area] {
				t.Fatalf("%s: area %q is missing from the whitelist", proto, area)
			}
		}
		if p.RegisterAreas["holding"] || p.RegisterAreas["coil"] || p.RegisterAreas["db"] {
			t.Fatalf("%s: register-protocol areas leaked into the whitelist: %v", proto, p.RegisterAreas)
		}
	}
}

// 正向用例：六种对象类别、四种平台类型、两个承载，都应当解析成
// `area:offset` 且 length 恒为 0。
func TestParseAddressIEC60870(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		want string
	}{
		{"float measurement", "ioa:1001", model.TypeFloat32, "ioa:1001:0"},
		{"scaled measurement", "ioa:1003", model.TypeInt16, "ioa:1003:0"},
		{"counter", "ioa:3001", model.TypeUint32, "ioa:3001:0"},
		// 布尔量在远动里是单点对象：**不带位号**也必须被接受。
		{"single point", "ioa:2001", model.TypeBool, "ioa:2001:0"},
		{"single command", "single-command:4001", model.TypeBool, "single-command:4001:0"},
		{"double command", "double-command:2003", model.TypeUint16, "double-command:2003:0"},
		{"float setpoint", "setpoint-float:4002", model.TypeFloat32, "setpoint-float:4002:0"},
		{"scaled setpoint", "setpoint-scaled:4003", model.TypeInt16, "setpoint-scaled:4003:0"},
		{"normalized setpoint", "setpoint-normalized:4004", model.TypeFloat32, "setpoint-normalized:4004:0"},
		// area 大小写不敏感：点表是人手写的，`IOA` 与 `ioa` 必须等价。
		{"uppercase area", "IOA:1001", model.TypeFloat32, "ioa:1001:0"},
		// 大 IOA（3 字节剖面能到 16777215）不能被 16 位宽度截断。
		{"3-byte IOA", "ioa:16777215", model.TypeFloat32, "ioa:16777215:0"},
	}
	for _, proto := range []string{"iec101", "iec104"} {
		for _, tc := range cases {
			t.Run(proto+"/"+tc.name, func(t *testing.T) {
				a, err := ParseAddress(tc.expr, tc.dt, proto)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if got := a.Canonical(); got != tc.want {
					t.Fatalf("canonical = %q, want %q", got, tc.want)
				}
				if a.Kind != model.AddressRegister {
					t.Fatalf("kind = %q, want register", a.Kind)
				}
				if a.Length != 0 {
					t.Fatalf("length = %d, want 0 (width comes from the ASDU type)", a.Length)
				}
				if a.Bit != -1 {
					t.Fatalf("bit = %d, want -1", a.Bit)
				}
			})
		}
	}
}

// 反向用例：每一条都必须给出**能指路的**错误，而不是让点表带着错进运行期。
func TestParseAddressIEC60870Rejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		hint int
		want string
	}{
		// 寄存器协议的区名与远动无关：写进来一定是抄错了列。
		{"modbus area", "holding:0", model.TypeUint16, 0, "not valid for protocol"},
		{"coil area", "coil:5", model.TypeBool, 0, "not valid for protocol"},
		{"s7 area", "db1:0", model.TypeUint16, 0, "not valid for protocol"},
		// 地址里没有长度这个概念，两种写法（地址内 / length 列）都要拒。
		{"length in address", "ioa:1001:2", model.TypeFloat32, 0, "length is not expressed"},
		{"length column", "ioa:1001", model.TypeFloat32, 2, "length is not expressed"},
		// 布尔量没有位号：按寄存器习惯写 `.3` 是误配。
		{"bit on measured", "ioa:1001.3", model.TypeFloat32, 0, "only valid for bool"},
		{"bit on bool", "ioa:2001.3", model.TypeBool, 0, "bit addressing is not used"},
		// 寄存器协议最常见的两种错法：符号地址与对象地址。
		{"symbol", "Plant.Temp", model.TypeFloat32, 0, "does not accept symbol"},
		{"object", "analog_input:1.present_value", model.TypeFloat32, 0, "does not accept object"},
		// 负偏移与非数字偏移。
		{"negative offset", "ioa:-1", model.TypeFloat32, 0, "not a non-negative integer"},
		{"non numeric offset", "ioa:x", model.TypeFloat32, 0, "not a non-negative integer"},
		{"empty offset", "ioa:", model.TypeFloat32, 0, "misses offset"},
	}
	for _, proto := range []string{"iec101", "iec104"} {
		for _, tc := range cases {
			t.Run(proto+"/"+tc.name, func(t *testing.T) {
				_, err := ParseAddressWithLength(tc.expr, tc.dt, proto, tc.hint)
				if err == nil {
					t.Fatal("must fail")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want substring %q", err, tc.want)
				}
			})
		}
	}
}

// 反向的跨协议检查：远动协议的地址不能被寄存器协议接受。这条防的是
// 「点表整段贴错协议」——那种错法在导入期放过，现场表现是每个点地址错。
func TestOtherProtocolsRejectIECAreas(t *testing.T) {
	for _, proto := range []string{"modbus-tcp", "s7", "dlt645"} {
		if _, err := ParseAddress("ioa:1001", model.TypeFloat32, proto); err == nil {
			t.Fatalf("%s must reject an IEC object-category area", proto)
		}
	}
}

// 点表层只做静态校验，不替协议判断 IOA 的合法范围：0 与「超过当前剖面
// 上限」都放行，由驱动在 Open 阶段按剖面给出准确报错（那里知道设备配了
// 几个字节的 IOA）。这一条写下来是为了防止有人后来「顺手」在点表层加上
// `offset >= 1` 的校验——那会让 Profile 的差异无法被测到。
func TestParseAddressIECLeavesRangeToDriver(t *testing.T) {
	a, err := ParseAddress("ioa:0", model.TypeFloat32, "iec101")
	if err != nil {
		t.Fatalf("offset 0 must parse (the driver rejects it with a clearer message): %v", err)
	}
	if a.Offset != 0 || a.Length != 0 {
		t.Fatalf("address = %+v", a)
	}
}

// Mewtocol 的点表形态：字地址 + 位号，一个地址是一个 16 位字。
func TestPolicyForMewtocol(t *testing.T) {
	p := PolicyFor("mewtocol")
	if p.Width != WidthRegisters {
		t.Fatalf("width = %v, want WidthRegisters", p.Width)
	}
	if !p.AllowRegister || p.AllowSymbol || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
		t.Fatalf("policy accepts non-register kinds: %+v", p)
	}
	if p.DIBits != 0 || p.DIField {
		t.Fatalf("mewtocol is not a DI protocol: %+v", p)
	}
	// 十四个区一个不能少：少了任何一个，对应点位在导入期就被拒。
	for _, area := range []string{"x", "y", "r", "l", "t", "c", "dt", "ld", "fl", "ix", "iy", "id", "sv", "ev"} {
		if !p.RegisterAreas[area] {
			t.Fatalf("area %q is missing from the whitelist", area)
		}
	}
	// 别的协议的区不能漏进来。
	for _, area := range []string{"holding", "coil", "db", "ioa"} {
		if p.RegisterAreas[area] {
			t.Fatalf("foreign area %q leaked into the whitelist", area)
		}
	}
	// T/C 是「一个号一个接点」，布尔量不带位号。
	if !p.BitlessAreas["t"] || !p.BitlessAreas["c"] {
		t.Fatalf("T/C must be bit-less bool areas: %v", p.BitlessAreas)
	}
	if p.BitlessAreas["x"] || p.BitlessAreas["dt"] {
		t.Fatalf("X/DT must require a bit: %v", p.BitlessAreas)
	}
}

func TestParseAddressMewtocol(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		want string
	}{
		// 字区：16 位类型长度 1，32 位类型长度 2。
		{"data register u16", "dt:0", model.TypeUint16, "dt:0:1"},
		{"data register float32", "dt:100", model.TypeFloat32, "dt:100:2"},
		{"file register", "fl:0", model.TypeInt16, "fl:0:1"},
		{"link data register", "ld:5", model.TypeUint32, "ld:5:2"},
		// 接点区：字 + 位。
		{"input relay bit", "x:1.15", model.TypeBool, "x:1.15:1"},
		{"output relay bit", "y:0.0", model.TypeBool, "y:0.0:1"},
		{"internal relay bit", "r:16.3", model.TypeBool, "r:16.3:1"},
		// 字区上的布尔量（可读不可写）：仍写位号。
		{"bool on word area", "dt:0.3", model.TypeBool, "dt:0.3:1"},
		// T/C：一个号一个接点，不带位号。
		{"timer contact", "t:10", model.TypeBool, "t:10:1"},
		{"counter contact", "c:999", model.TypeBool, "c:999:1"},
		// 索引寄存器与定时器值区。
		{"index register", "ix:0", model.TypeUint16, "ix:0:1"},
		{"timer set value", "sv:5", model.TypeUint16, "sv:5:1"},
		// 区名大小写不敏感。
		{"uppercase area", "DT:0", model.TypeUint16, "dt:0:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "mewtocol")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Canonical(); got != tc.want {
				t.Fatalf("canonical = %q, want %q", got, tc.want)
			}
			// 往返：Canonical 的文本必须能被重新解析成同一个地址。
			b, err := ParseAddress(a.Canonical(), tc.dt, "mewtocol")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if b != a {
				t.Fatalf("round trip: %+v != %+v", b, a)
			}
		})
	}
}

func TestParseAddressMewtocolRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		want string
	}{
		// 未知区名必须在导入期被拒。
		{"unknown area", "d:0", model.TypeUint16, "d"},
		// 布尔量在 X/Y/R/L 上必须带位号。
		{"bit required on relay", "x:1", model.TypeBool, "bool point needs a bit"},
		// 位号范围 0..15。
		{"bit out of range", "x:1.16", model.TypeBool, "bit must be 0..15"},
		// 布尔量在字区也不能省位号。
		{"bit required on word area", "dt:0", model.TypeBool, "bool point needs a bit"},
		// 位号只对布尔量有意义。
		{"bit on u16", "dt:0.3", model.TypeUint16, "only valid for bool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddress(tc.expr, tc.dt, "mewtocol")
			if err == nil {
				t.Fatalf("expected an error for %q", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 别的协议不能借用 mewtocol 的区名。
func TestOtherProtocolsRejectMewAreas(t *testing.T) {
	for _, proto := range []string{"modbus-tcp", "s7", "iec104"} {
		if _, err := ParseAddress("dt:0", model.TypeUint16, proto); err == nil {
			t.Fatalf("%s must reject a Mewtocol area", proto)
		}
	}
}

// Host Link / FINS 共用同一套内存区：点表写法必须完全一致。
func TestPolicyForHostLink(t *testing.T) {
	for _, proto := range []string{"hostlink", "fins"} {
		p := PolicyFor(proto)
		if p.Width != WidthRegisters {
			t.Fatalf("%s width = %v, want WidthRegisters", proto, p.Width)
		}
		if !p.AllowRegister || p.AllowSymbol || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
			t.Fatalf("%s policy accepts non-register kinds: %+v", proto, p)
		}
		if p.DIBits != 0 || p.DIField {
			t.Fatalf("%s is not a DI protocol: %+v", proto, p)
		}
		// 五个固定区 + 当前 EM 区 + 13 个 bank，一个不能少。
		want := []string{"cio", "wr", "hr", "ar", "dm", "em"}
		for i := 0; i <= 12; i++ {
			want = append(want, fmt.Sprintf("em%d", i))
		}
		for _, area := range want {
			if !p.RegisterAreas[area] {
				t.Fatalf("%s: area %q is missing from the whitelist", proto, area)
			}
		}
		// 别的协议的区不能漏进来；EM 的越界 bank 同理。
		for _, area := range []string{"holding", "coil", "db", "dt", "x", "em13", "dmm"} {
			if p.RegisterAreas[area] {
				t.Fatalf("%s: foreign area %q leaked into the whitelist", proto, area)
			}
		}
		// FINS 的区都能按位访问（EM 当前区降级为读字取位，仍算支持位点位）。
		if p.BitlessAreas != nil {
			t.Fatalf("%s: no bit-less areas expected, got %v", proto, p.BitlessAreas)
		}
	}
}

func TestParseAddressHostLink(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		want string
	}{
		{"data memory u16", "dm:100", model.TypeUint16, "dm:100:1"},
		{"data memory float32", "dm:200", model.TypeFloat32, "dm:200:2"},
		{"cio bit", "cio:0.5", model.TypeBool, "cio:0.5:1"},
		{"work relay bit", "wr:10.15", model.TypeBool, "wr:10.15:1"},
		{"holding relay bit", "hr:1.0", model.TypeBool, "hr:1.0:1"},
		{"aux relay bit", "ar:2.3", model.TypeBool, "ar:2.3:1"},
		{"current em", "em:5", model.TypeUint16, "em:5:1"},
		{"em bank 0", "em0:5", model.TypeUint16, "em0:5:1"},
		{"em bank 12", "em12:7", model.TypeInt16, "em12:7:1"},
		// 现场简写归一化成规范名：Canonical 只输出一种写法。
		{"short data memory", "d:0", model.TypeUint16, "dm:0:1"},
		{"short work relay", "w:0", model.TypeUint16, "wr:0:1"},
		{"short holding relay", "h:0", model.TypeUint16, "hr:0:1"},
		{"short aux relay", "a:0", model.TypeUint16, "ar:0:1"},
		// 区名大小写不敏感。
		{"uppercase area", "DM:0", model.TypeUint16, "dm:0:1"},
		// 显式长度：一次读多个字。
		{"explicit length", "dm:0:10", model.TypeUint16, "dm:0:10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "hostlink")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Canonical(); got != tc.want {
				t.Fatalf("canonical = %q, want %q", got, tc.want)
			}
			// 同一份地址在 FINS/TCP 上必须解析成同一个结果：这是「点表
			// 换个 protocol 就能跑」的可执行证据。
			b, err := ParseAddress(tc.expr, tc.dt, "fins")
			if err != nil {
				t.Fatalf("parse as fins: %v", err)
			}
			if b != a {
				t.Fatalf("fins = %+v, hostlink = %+v", b, a)
			}
			// 往返：Canonical 的文本必须能被重新解析成同一个地址。
			c, err := ParseAddress(a.Canonical(), tc.dt, "hostlink")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if c != a {
				t.Fatalf("round trip: %+v != %+v", c, a)
			}
		})
	}
}

func TestParseAddressHostLinkRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		want string
	}{
		// 写错的区名必须在导入期被拒，而不是运行时每个周期报一次。
		{"unknown area", "dmm:0", model.TypeUint16, "dmm"},
		{"em bank out of range", "em13:0", model.TypeUint16, "em13"},
		// 布尔量必须带位号：FINS 没有「一个号一个接点」的区。
		{"bool needs bit", "dm:0", model.TypeBool, "bool point needs a bit"},
		// 位号范围 0..15。
		{"bit out of range", "cio:0.16", model.TypeBool, "bit must be 0..15"},
		// 位号只对布尔量有意义。
		{"bit on u16", "dm:0.3", model.TypeUint16, "only valid for bool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddress(tc.expr, tc.dt, "hostlink")
			if err == nil {
				t.Fatalf("expected an error for %q", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 别的协议不能借用 FINS 的区名（尤其 DM：Modbus 里没有这个名字）。
func TestOtherProtocolsRejectFinsAreas(t *testing.T) {
	for _, proto := range []string{"modbus-tcp", "s7", "iec104", "mewtocol"} {
		for _, expr := range []string{"dm:0", "cio:0.1", "em5:0"} {
			if _, err := ParseAddress(expr, model.TypeUint16, proto); err == nil {
				t.Fatalf("%s must reject FINS address %q", proto, expr)
			}
		}
	}
}

// KNX 的组地址必须带数据点类型：组地址本身不携带「值有多宽」，缺了 DPT
// 驱动连该读几个字节都不知道，因此它是必填项而不是可选元数据。
func TestPolicyForKNX(t *testing.T) {
	p := PolicyFor("knx")
	if p.Width != WidthNone {
		t.Fatalf("width = %v, want WidthNone (the DPT decides the width)", p.Width)
	}
	if !p.AllowSymbol || !p.AllowDPT {
		t.Fatalf("knx must accept symbol addresses with a DPT: %+v", p)
	}
	if p.AllowRegister || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
		t.Fatalf("knx accepts non-symbol kinds: %+v", p)
	}
	if p.DIBits != 0 || p.DIField || p.AllowSymbolLength || p.SymbolIndexInPath {
		t.Fatalf("knx policy leaked DI/symbol-length flags: %+v", p)
	}
	// 别的协议不能借用 DPT：`1/2/3@9.001` 在 MQTT 里只是一个奇怪的主题。
	for _, proto := range []string{"modbus-tcp", "mqtt", "opcua", "snmp"} {
		if PolicyFor(proto).AllowDPT {
			t.Fatalf("%s must not accept DPT addresses", proto)
		}
	}
}

func TestParseAddressKNX(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		path string
		dpt  string
	}{
		{"three-level temperature", "1/1/45@9.001", model.TypeFloat32, "1/1/45", "9.001"},
		{"switch", "0/0/1@1.001", model.TypeBool, "0/0/1", "1.001"},
		{"percent", "2/3/4@5.001", model.TypeFloat32, "2/3/4", "5.001"},
		// 只写主类型也接受：线格式对同一主类型是一样的，具体子类型由
		// 工程的组地址表决定。
		{"main type only", "1/2/3@9", model.TypeFloat32, "1/2/3", "9"},
		// 现场的原始 16 位写法与两级写法都保留原文本：它们是同一批组地址
		// 的另一种记法，归一化成三级形式会让点表与工程文档对不上。
		{"two-level", "1/300@9.001", model.TypeFloat32, "1/300", "9.001"},
		{"raw decimal", "2350@9.001", model.TypeFloat32, "2350", "9.001"},
		// 前后空白由解析器吃掉。
		{"padded", "  1 / 2 / 3 @ 9.001  ", model.TypeFloat32, "1/2/3", "9.001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "knx")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Kind != model.AddressSymbol {
				t.Fatalf("kind = %q, want symbol", a.Kind)
			}
			if a.Path != tc.path || a.DPT != tc.dpt {
				t.Fatalf("address = %+v, want path %q dpt %q", a, tc.path, tc.dpt)
			}
			// length 不表达任何东西：宽度由 DPT 决定。
			if a.Length != 0 {
				t.Fatalf("length = %d, want 0", a.Length)
			}
			if got, want := a.Canonical(), tc.path+"@"+tc.dpt; got != want {
				t.Fatalf("canonical = %q, want %q", got, want)
			}
			// 往返：Canonical 的文本必须能重新解析成同一个地址。
			b, err := ParseAddress(a.Canonical(), tc.dt, "knx")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if b != a {
				t.Fatalf("round trip: %+v != %+v", b, a)
			}
		})
	}
}

func TestParseAddressKNXRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		hint int
		want string
	}{
		// 缺 DPT 是最常见的错法：从其它点表模板复制过来只填了组地址。
		{"missing dpt", "1/2/3", model.TypeFloat32, 0, "dpt is required"},
		{"empty dpt", "1/2/3@", model.TypeFloat32, 0, "dpt after '@' is empty"},
		{"empty address", "@9.001", model.TypeFloat32, 0, "address before '@' is empty"},
		// 宽度由 DPT 决定，两种 length 写法都要拒。
		{"length in address", "1/2/3@9.001:2", model.TypeFloat32, 0, "length is not expressed"},
		{"length column", "1/2/3@9.001", model.TypeFloat32, 2, "length is not expressed"},
		// 寄存器/对象/文件写法：KNX 只有组地址。
		{"register", "holding:0", model.TypeUint16, 0, "does not accept register"},
		{"object", "analogInput:1.presentValue", model.TypeFloat32, 0, "does not accept object"},
		{"computed", "expr:1/2/3", model.TypeFloat32, 0, "does not accept computed"},
		// 组地址本身不是寄存器地址：`1/2/3:1` 这种写法多半是把别的协议
		// 抄了过来。
		{"register style group", "holding:1/2/3", model.TypeFloat32, 0, "does not accept register"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddressWithLength(tc.expr, tc.dt, "knx", tc.hint)
			if err == nil {
				t.Fatalf("expected an error for %q", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 组地址的语法校验（`1/8/1` 的中组越界、`0/0/0` 无效）不在点表层做：
// 那里没有组地址的结构知识，复制一份规则只会漂移。点表层只保证 DPT 在，
// 具体地址由驱动在 Open 阶段用协议 codec 校验。
func TestParseAddressKNXLeavesGroupSyntaxToDriver(t *testing.T) {
	for _, expr := range []string{"1/8/1@1.001", "0/0/0@1.001", "1/2/3/4@1.001"} {
		if _, err := ParseAddress(expr, model.TypeBool, "knx"); err != nil {
			t.Fatalf("%q must parse at the point-table layer (the driver rejects it): %v", expr, err)
		}
	}
}

// IEC 61850 的对象引用是符号地址，宽度由装置侧的 MMS 类型决定：点表层不能
// 要求也不能接受 length，否则会诱导人写出「读 4 个字节」这种不存在的概念。
func TestPolicyForIEC61850(t *testing.T) {
	p := PolicyFor("iec61850")
	if p.Width != WidthNone {
		t.Fatalf("width = %v, want WidthNone (the object's MMS type decides)", p.Width)
	}
	if !p.AllowSymbol {
		t.Fatalf("iec61850 must accept symbol addresses: %+v", p)
	}
	if p.AllowRegister || p.AllowObject || p.AllowFile || p.AllowComputed ||
		p.AllowSelector || p.AllowDPT {
		t.Fatalf("iec61850 accepts other address kinds: %+v", p)
	}
	if p.DIBits != 0 || p.DIField || p.AllowSymbolLength || p.SymbolIndexInPath {
		t.Fatalf("iec61850 policy leaked DI/symbol-length flags: %+v", p)
	}
}

func TestParseAddressIEC61850(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		path string
	}{
		{"status", "simpleIO/GGIO1.ST.Ind1.stVal", model.TypeBool, "simpleIO/GGIO1.ST.Ind1.stVal"},
		{"measurement", "simpleIO/GGIO1.MX.PhV.mag.f", model.TypeFloat32, "simpleIO/GGIO1.MX.PhV.mag.f"},
		{"health", "simpleIO/LLN0.ST.Health", model.TypeInt32, "simpleIO/LLN0.ST.Health"},
		// 前后空白由解析器吃掉；路径里的点与斜杠原样保留（对象引用的
		// 结构由驱动用 codec 校验，点表层不复制一份「差不多的」语法）。
		{"padded", "  simpleIO/GGIO1.ST.Ind1.stVal  ", model.TypeBool, "simpleIO/GGIO1.ST.Ind1.stVal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "iec61850")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Kind != model.AddressSymbol || a.Path != tc.path {
				t.Fatalf("address = %+v, want symbol %q", a, tc.path)
			}
			// length 不表达任何东西：宽度由数据对象的 MMS 类型决定。
			if a.Length != 0 {
				t.Fatalf("length = %d, want 0", a.Length)
			}
			// 往返：Canonical 的文本必须能重新解析成同一个地址。
			b, err := ParseAddress(a.Canonical(), tc.dt, "iec61850")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if b != a {
				t.Fatalf("round trip: %+v != %+v", b, a)
			}
		})
	}
}

// 反向用例里刻意区分「点表层能判」与「必须留给驱动判」两类：
// 前一类在这里报错，后一类（对象引用的语法）必须放行。
func TestParseAddressIEC61850Rejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		hint int
		want string
	}{
		// 宽度由装置侧决定，`AllowSymbolLength` 为 false：点表的 length 列
		// 没有含义，直接拒绝；地址里的 `:2` 也不会被切掉当作长度（它是
		// 名字的一部分，由驱动在 Open 阶段连同对象引用一起判）。
		{"length column", "simpleIO/GGIO1.ST.Ind1.stVal", model.TypeFloat32, 2, "length is only valid for register addresses"},
		{"register", "holding:0", model.TypeUint16, 0, "does not accept register"},
		{"object", "analogInput:1.presentValue", model.TypeFloat32, 0, "does not accept object"},
		{"computed", "expr:simpleIO/GGIO1.ST.Ind1.stVal", model.TypeBool, 0, "does not accept computed"},
		{"empty", "   ", model.TypeBool, 0, "address is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddressWithLength(tc.expr, tc.dt, "iec61850", tc.hint)
			if err == nil {
				t.Fatalf("expected an error for %q", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 对象引用的结构知识（几段、FC 是否合法、LD/LN 命名规则）属于 codec，
// 点表层只保证「是一个非空的符号地址」。复制一份规则的结果是两份校验迟早
// 不一致，而不一致的那一侧通常是宽松的那一侧。
func TestParseAddressIEC61850LeavesRefSyntaxToDriver(t *testing.T) {
	for _, expr := range []string{
		"simpleIO/GGIO1.Ind1.stVal", // 缺 FC：由 labels.defaultFC 补
		"simpleIO/GGIO1.MOD.Beh",    // MOD 不是功能约束：驱动拒绝
		"GGIO1.ST.Ind1.stVal",       // 缺 LD：驱动拒绝
		"a/b/c/d/e/f/g",             // 任意多段：驱动拒绝
	} {
		if _, err := ParseAddress(expr, model.TypeBool, "iec61850"); err != nil {
			t.Fatalf("%q must parse at the point-table layer (the driver rejects it): %v", expr, err)
		}
	}
}

// ---------------------------------------------------------------------------
// MTConnect：数据项 ID 寻址
// ---------------------------------------------------------------------------

func TestPolicyForMTConnect(t *testing.T) {
	p := PolicyFor("mtconnect")
	if p.Width != WidthNone {
		t.Fatalf("width = %v, want WidthNone (the data item's text + dataType decide)", p.Width)
	}
	if !p.AllowSymbol {
		t.Fatalf("mtconnect must accept symbol addresses: %+v", p)
	}
	if p.AllowRegister || p.AllowObject || p.AllowFile || p.AllowComputed ||
		p.AllowSelector || p.AllowDPT {
		t.Fatalf("mtconnect accepts other address kinds: %+v", p)
	}
	if p.DIBits != 0 || p.DIField || p.AllowSymbolLength || p.SymbolIndexInPath {
		t.Fatalf("mtconnect policy leaked DI/symbol-length flags: %+v", p)
	}
}

func TestParseAddressMTConnect(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		path string
	}{
		{"axis position", "Xabs", model.TypeFloat64, "Xabs"},
		{"execution", "exec", model.TypeString, "exec"},
		{"availability", "avail", model.TypeString, "avail"},
		// 数据项 ID 允许点（Agent 侧的命名习惯），也允许下划线与连字符。
		{"with underscore", "Sload", model.TypeFloat64, "Sload"},
		// 前后空白由解析器吃掉。
		{"padded", "  toolOffset  ", model.TypeString, "toolOffset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "mtconnect")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Kind != model.AddressSymbol || a.Path != tc.path {
				t.Fatalf("address = %+v, want symbol %q", a, tc.path)
			}
			if a.Length != 0 {
				t.Fatalf("length = %d, want 0", a.Length)
			}
			// 往返：Canonical 的文本必须能重新解析成同一个地址。
			b, err := ParseAddress(a.Canonical(), tc.dt, "mtconnect")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if b != a {
				t.Fatalf("round trip: %+v != %+v", b, a)
			}
		})
	}
}

func TestParseAddressMTConnectRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		dt   model.DataType
		hint int
		want string
	}{
		// 宽度由数据项决定，length 列没有含义。
		{"length column", "Xabs", model.TypeFloat64, 2, "length is only valid for register addresses"},
		{"register", "holding:0", model.TypeUint16, 0, "does not accept register"},
		{"object", "analogInput:1.presentValue", model.TypeFloat32, 0, "does not accept object"},
		{"computed", "expr:Xabs", model.TypeFloat64, 0, "does not accept computed"},
		{"empty", "   ", model.TypeString, 0, "address is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddressWithLength(tc.expr, tc.dt, "mtconnect", tc.hint)
			if err == nil {
				t.Fatalf("expected an error for %q", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// dataItemId 的结构知识（是不是合法 ID、Agent 侧到底有没有这个 id）属于
// codec / 驱动，点表层只保证「是一个非空的符号地址」。
func TestParseAddressMTConnectLeavesIDSyntaxToDriver(t *testing.T) {
	for _, expr := range []string{
		"Xabs",
		"avail",
		"system.1", // 带点：Agent 的命名习惯，驱动不拦
	} {
		if _, err := ParseAddress(expr, model.TypeFloat64, "mtconnect"); err != nil {
			t.Fatalf("%q must parse at the point-table layer (the driver validates it): %v", expr, err)
		}
	}
}

// ---------------------------------------------------------------------------
// BACnet：对象属性寻址（BACnet/IP 与 MS/TP 共用）
// ---------------------------------------------------------------------------

func TestPolicyForBACnet(t *testing.T) {
	for _, proto := range []string{"bacnet", "bacnet-mstp"} {
		p := PolicyFor(proto)
		if p.Width != WidthProtocol {
			t.Fatalf("%s: width = %v, want WidthProtocol (the property type decides)", proto, p.Width)
		}
		if !p.AllowObject {
			t.Fatalf("%s must accept object addresses: %+v", proto, p)
		}
		if p.AllowRegister || p.AllowSymbol || p.AllowFile || p.AllowComputed ||
			p.AllowSelector || p.AllowDPT || p.AllowSymbolLength {
			t.Fatalf("%s accepts other address kinds: %+v", proto, p)
		}
		if p.DIBits != 0 || p.DIField || p.SymbolIndexInPath {
			t.Fatalf("%s policy leaked DI/symbol flags: %+v", proto, p)
		}
	}
}

func TestParseAddressBACnet(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		dt       model.DataType
		objType  string
		instance int
		property string
		index    int
	}{
		{"analog input", "analog-input:1.present-value", model.TypeFloat32,
			"analog-input", 1, "present-value", 0},
		{"object name", "analog-input:1.object-name", model.TypeString,
			"analog-input", 1, "object-name", 0},
		{"priority element", "analog-value:2.priority-array[3]", model.TypeFloat32,
			"analog-value", 2, "priority-array", 3},
		{"numeric object type", "0:1.present-value", model.TypeFloat32,
			"0", 1, "present-value", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, proto := range []string{"bacnet", "bacnet-mstp"} {
				a, err := ParseAddress(tc.expr, tc.dt, proto)
				if err != nil {
					t.Fatalf("%s: parse: %v", proto, err)
				}
				if a.Kind != model.AddressObject || a.ObjectType != tc.objType ||
					a.Instance != tc.instance || a.Property != tc.property ||
					a.PropIndex != tc.index {
					t.Fatalf("%s: address = %+v", proto, a)
				}
				b, err := ParseAddress(a.Canonical(), tc.dt, proto)
				if err != nil {
					t.Fatalf("%s: reparse %q: %v", proto, a.Canonical(), err)
				}
				if b != a {
					t.Fatalf("%s: round trip: %+v != %+v", proto, b, a)
				}
			}
		})
	}
}

func TestParseAddressBACnetRejectsNonObject(t *testing.T) {
	for _, proto := range []string{"bacnet", "bacnet-mstp"} {
		for _, expr := range []string{"holding:0", "SOME_SYMBOL", "db1:0"} {
			if _, err := ParseAddress(expr, model.TypeFloat32, proto); err == nil {
				t.Fatalf("%s accepted non-object address %q", proto, expr)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Toledo：读数字段寻址
// ---------------------------------------------------------------------------

// Toledo 的点位地址是「当前一帧读数的一个字段名」，不是寄存器、不是对象。
//
// 这条策略要同时挡住两个方向：别把 Modbus 风格的 `holding:0:2` 喂给秤（现场
// 会以为「读到了寄存器」，实际连续输出帧里根本没有线性寄存器结构），也别让
// 秤的字段名混进只接受 DI/寄存器/对象的协议被误当成合法地址。
//
// 刻意**不**校验字段名合法性：这是仓库的既定分工——点表层只管「地址种类放不放
// 行」，字段名与 dataType 的匹配由驱动在 Open 时校验（与 SNMP 的 OID 数值约束
// 同例）。若这里再维护一份字段白名单，两处必然漂移。
func TestPolicyForToledo(t *testing.T) {
	p := PolicyFor("toledo")
	if p.Width != WidthNone {
		t.Fatalf("toledo width = %v，应为 WidthNone（数值段宽度由仪表小数位决定）", p.Width)
	}
	if !p.AllowSymbol {
		t.Fatal("toledo 必须允许符号地址")
	}
	if p.AllowRegister || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
		t.Fatalf("toledo 只接受字段名，不应放行其它地址种类：%+v", p)
	}
}

// 字段名地址必须能经 ParseAddress 解析成 symbol，并在 Canonical 后往返一致。
func TestParseAddressToledo(t *testing.T) {
	cases := []struct {
		expr string
		dt   model.DataType
		want string
	}{
		{"weight", model.TypeFloat64, "weight"},
		{"status", model.TypeString, "status"},
		{"unit", model.TypeString, "unit"},
		{"mode", model.TypeString, "mode"},
		{"stable", model.TypeBool, "stable"},
		{"motion", model.TypeBool, "motion"},
		{"decimals", model.TypeInt64, "decimals"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "toledo")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Kind != model.AddressSymbol || a.Path != tc.want {
				t.Fatalf("address = %+v，应为 symbol/%s", a, tc.want)
			}
			b, err := ParseAddress(a.Canonical(), tc.dt, "toledo")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if b != a {
				t.Fatalf("round trip: %+v != %+v", b, a)
			}
		})
	}
}

// 寄存器/对象/文件/selector 这些地址种类在 Toledo 下必须在导入期就被拒，
// 而不是等到启动后每个周期返回 address_error。
//
// 注意「不是被拒」的几种：`12345`、`calc:weight*2`、`weight:4` 在点表层都会
// 被当成**符号名**收下（本包不校验符号内容——那是驱动的职责，与 SNMP 不校验
// OID 数值约束同例）。它们会在驱动 Open 时被 `KnownSymbol` 拒绝，报
// ErrAddress 并列出可用字段，而不是等到采集周期里静默失败。
func TestParseAddressToledoRejectsOtherKinds(t *testing.T) {
	for _, expr := range []string{
		"holding:0:2",
		"holding:100",
		"db1:0",
		"expr:weight*2",
		"file:1",
		"weight#raw",
		"weight#",
	} {
		if _, err := ParseAddress(expr, model.TypeFloat64, "toledo"); err == nil {
			t.Fatalf("toledo 接受了非字段名地址 %q", expr)
		}
	}
}

// `length` 在 Toledo 下必须在导入期拒绝（点表的 length 列）。
//
// 连续输出帧的数值段宽度由仪表的小数位数决定（D39 的 WEIGHT(7) 是 5 位整数 +
// '.' + 1 位小数），点表里写 length 只会与帧里的实际宽度冲突，读出来的值会
// 被按错误的宽度切分。这类「配了但不生效」的列必须在导入期报错。
func TestParseAddressToledoRejectsLength(t *testing.T) {
	if _, err := ParseAddressWithLength("weight", model.TypeFloat64, "toledo", 4); err == nil {
		t.Fatal("toledo 应拒绝 length 列（数值段宽度由仪表小数位决定）")
	}
}

// 反向：秤的字段名不该被只接受 DI/寄存器的协议收下。
func TestToledoSymbolsRejectedByOtherProtocols(t *testing.T) {
	// 这三类协议的地址是严格形态（寄存器区名 / DI 十六进制 / 对象引用），
	// 裸字段名必须被拒。
	for _, proto := range []string{"modbus-tcp", "s7", "dlt645", "cjt188", "bacnet"} {
		for _, expr := range []string{"weight", "status", "decimals"} {
			if _, err := ParseAddress(expr, model.TypeFloat64, proto); err == nil {
				t.Fatalf("%s 接受了 Toledo 字段名 %q", proto, expr)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SL 651：要素标识符寻址
// ---------------------------------------------------------------------------

// SL 651 的点位地址是「要素标识符」（引导符 + 数据定义字节），不是寄存器、不是对象。
//
// 与 Toledo 同类：报文是「要素信息组」，每个要素自带「我是什么、多长」，没有「第
// n 个字节」这种线性结构，故 WidthNone；引导符是规约定义的常量（39H 永远是瞬时
// 河道水位），故 AllowSymbol。地址写成 ASCII 简称（Z/PT/VT）或两位十六进制（39/26）。
func TestPolicyForSL651(t *testing.T) {
	p := PolicyFor("sl651")
	if p.Width != WidthNone {
		t.Fatalf("sl651 width = %v，应为 WidthNone（要素宽度由数据定义字节声明）", p.Width)
	}
	if !p.AllowSymbol {
		t.Fatal("sl651 必须允许符号地址（要素标识符）")
	}
	if p.AllowRegister || p.AllowObject || p.AllowFile || p.AllowComputed || p.AllowSelector {
		t.Fatalf("sl651 只接受要素标识符，不应放行其它地址种类：%+v", p)
	}
}

// 要素标识符地址必须能解析成 symbol，并在 Canonical 后往返一致。
func TestParseAddressSL651(t *testing.T) {
	cases := []struct {
		expr string
		dt   model.DataType
		want string
	}{
		{"Z", model.TypeFloat64, "Z"},     // 瞬时河道水位
		{"PT", model.TypeFloat64, "PT"},   // 降水量累计值
		{"VT", model.TypeFloat64, "VT"},   // 电压
		{"39", model.TypeString, "39"},    // 两位十六进制写法
		{"DRP", model.TypeFloat64, "DRP"}, // 批量雨量
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			a, err := ParseAddress(tc.expr, tc.dt, "sl651")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Kind != model.AddressSymbol || a.Path != tc.want {
				t.Fatalf("address = %+v，应为 symbol/%s", a, tc.want)
			}
			b, err := ParseAddress(a.Canonical(), tc.dt, "sl651")
			if err != nil {
				t.Fatalf("reparse %q: %v", a.Canonical(), err)
			}
			if b != a {
				t.Fatalf("round trip: %+v != %+v", b, a)
			}
		})
	}
}

// 寄存器/对象/文件/计算/selector 地址在 SL 651 下必须在导入期就被拒。
//
// 注意「不是被拒」的：`39`、`Z` 本就是合法要素标识符；而像 `1.3.6.1...` 这种 OID
// 形态的串在点表层会被当成符号名收下（本包不校验符号内容，是否真能解析成要素由
// 驱动在 Open 时用 LookupGuide 校验）。这里只拦「地址种类」不对的。
func TestParseAddressSL651RejectsOtherKinds(t *testing.T) {
	for _, expr := range []string{
		"holding:0:2",
		"db1:0",
		"expr:Z*2",
		"Z#raw",
	} {
		if _, err := ParseAddress(expr, model.TypeFloat64, "sl651"); err == nil {
			t.Fatalf("sl651 接受了非要素标识符地址 %q", expr)
		}
	}
}

// `length` 列在 SL 651 下必须在导入期拒绝。
//
// 要素宽度由报文里的数据定义字节声明，点表里写 length 只会与报文声明冲突；读错位
// 不会报错，只会把降水量记到水位上。这类「配了但不生效」的列必须导入期报错。
func TestParseAddressSL651RejectsLength(t *testing.T) {
	if _, err := ParseAddressWithLength("Z", model.TypeFloat64, "sl651", 4); err == nil {
		t.Fatal("sl651 应拒绝 length 列（要素宽度由数据定义字节声明）")
	}
}
