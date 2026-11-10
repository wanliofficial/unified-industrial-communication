package pointtable

import (
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
