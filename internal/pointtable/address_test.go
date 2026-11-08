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
