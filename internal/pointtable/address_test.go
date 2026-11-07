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
	if p := PolicyFor("weird"); !p.AllowRegister || !p.AllowSymbol || p.Width != WidthExplicit {
		t.Fatalf("unknown = %+v", p)
	}
}
