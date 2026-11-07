package pointtable

import (
	"strings"
	"testing"
	"time"
)

func TestParseCSVValid(t *testing.T) {
	raw := "\uFEFFpoint,address,type,name,unit,scale,bias,deadband,writable,enabled,byteOrder,interval,length,description\r\n" +
		"voltage,holding:0,float32,\"A 相电压, 线电压\",V,0.1,0,0.01,no,yes,CDAB,5s,,\"主进线\"\r\n" +
		"current,holding:2,float32,电流,A,,,0,false,true,CDAB,,\r\n" +
		"running,coil:5,bool,运行状态,,,,,true,true,,,\r\n" +
		"label,holding:10,string,设备铭牌,,,,,false,true,,,8,\r\n"
	res, err := ParseCSV(strings.NewReader(raw), "modbus-tcp")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Points) != 4 {
		t.Fatalf("points = %d, want 4", len(res.Points))
	}
	v := res.Points[0]
	if v.ID != "voltage" || v.Name != "A 相电压, 线电压" {
		t.Fatalf("voltage = %+v", v)
	}
	if v.Unit != "V" || v.Scale != 0.1 || v.Deadband != 0.01 {
		t.Fatalf("voltage fields = %+v", v)
	}
	if v.Writable || !v.Enabled || v.Interval.Duration() != 5*time.Second {
		t.Fatalf("voltage flags = %+v", v)
	}
	if v.Address.Canonical() != "holding:0:2" {
		t.Fatalf("address = %s", v.Address.Canonical())
	}
	if v.Description != "主进线" {
		t.Fatalf("description = %q", v.Description)
	}
	c := res.Points[1]
	if c.Bias != 0 || c.Deadband != 0 || c.Scale != 0 {
		t.Fatalf("current numbers = %+v", c)
	}
	if !res.Points[2].Writable {
		t.Fatal("running must be writable")
	}
	// 字符串宽度只能由 length 列提供：地址里只有偏移。
	if got := res.Points[3].Address.Length; got != 8 {
		t.Fatalf("explicit string length = %d, want 8", got)
	}
}

func TestParseCSVWarnsOnDuplicateAddress(t *testing.T) {
	raw := "point,address,type\n" +
		"a,holding:0,uint16\n" +
		"b,holding:0,uint16\n"
	res, err := ParseCSV(strings.NewReader(raw), "modbus-tcp")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Points) != 2 {
		t.Fatalf("points = %d", len(res.Points))
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Message, "same address") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestParseCSVWarnsOnLengthMismatch(t *testing.T) {
	raw := "point,address,type,length\na,holding:0,float32,1\n"
	res, err := ParseCSV(strings.NewReader(raw), "modbus-tcp")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Message, "width") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestParseCSVErrors(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		protocol string
		want     string
	}{
		{"empty", "", "modbus-tcp", "header required"},
		{"unknown column", "point,address,type,colour\na,holding:0,uint16,red\n", "modbus-tcp", "unknown column"},
		{"duplicate column", "point,address,type,id\na,holding:0,uint16,a\n", "modbus-tcp", "duplicate column"},
		{"missing column", "point,address\na,holding:0\n", "modbus-tcp", "required column"},
		{"missing id", "point,address,type\n,holding:0,uint16\n", "modbus-tcp", "point id is required"},
		{"bad id", "point,address,type\nA,holding:0,uint16\n", "modbus-tcp", "must match"},
		{"bad type", "point,address,type\na,holding:0,float\n", "modbus-tcp", "unknown dataType"},
		{"bad address", "point,address,type\na,zzz,uint16\n", "modbus-tcp", "symbol"},
		{"protocol area", "point,address,type\na,db1:0,uint16\n", "modbus-tcp", "not valid"},
		{"bad length", "point,address,type,length\na,holding:0,uint16,x\n", "modbus-tcp", "positive integer"},
		{"length on symbol", "point,address,type,length\na,ns=2;s=x,uint16,2\n", "opcua", "only valid for register"},
		{"bad scale", "point,address,type,scale\na,holding:0,uint16,x\n", "modbus-tcp", "must be a number"},
		{"zero scale", "point,address,type,scale\na,holding:0,uint16,0\n", "modbus-tcp", "must not be 0"},
		{"bad writable", "point,address,type,writable\na,holding:0,uint16,maybe\n", "modbus-tcp", "true/false"},
		{"bad interval", "point,address,type,interval\na,holding:0,uint16,10ms\n", "modbus-tcp", ">= 100ms"},
		{"bad byteOrder", "point,address,type,byteOrder\na,holding:0,uint16,XYZW\n", "modbus-tcp", "ABCD"},
		{"duplicate id", "point,address,type\na,holding:0,uint16\na,holding:1,uint16\n", "modbus-tcp", "duplicate point id"},
		{"length twice mismatch", "point,address,type,length\na,holding:0:2,uint16,4\n", "modbus-tcp", "given twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCSV(strings.NewReader(tc.raw), tc.protocol)
			if err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
			var es Errors
			if !asErrors(err, &es) {
				t.Fatalf("error type = %T, want Errors", err)
			}
		})
	}
}

// 多行都有错时一次报出来，而不是只报第一行：1000 行点表不应该修一行跑一次。
func TestParseCSVReportsAllRows(t *testing.T) {
	raw := "point,address,type\n" +
		"a,holding:0,nope\n" +
		"b,holding:1,uint16\n" +
		"c,holding:2,nope\n"
	_, err := ParseCSV(strings.NewReader(raw), "modbus-tcp")
	if err == nil {
		t.Fatal("must fail")
	}
	var es Errors
	if !asErrors(err, &es) {
		t.Fatalf("type = %T", err)
	}
	if len(es) != 2 {
		t.Fatalf("errors = %d, want 2\n%v", len(es), es)
	}
	if es[0].Line != 2 || es[1].Line != 4 {
		t.Fatalf("lines = %d, %d", es[0].Line, es[1].Line)
	}
}

func TestParseCSVS7AndSymbols(t *testing.T) {
	raw := "point,address,type\n" +
		"temp,db1:0,float32\n" +
		"flag,db1:4.3,bool\n" +
		"node,ns=2;s=Plant.Temp,float64\n"
	res, err := ParseCSV(strings.NewReader(raw), "s7")
	if err == nil {
		t.Fatalf("symbol address must be rejected for s7: %+v", res)
	}
	if !strings.Contains(err.Error(), "symbol") {
		t.Fatalf("error = %v", err)
	}

	// opcua 没有二级选择器：`#...` 是 MQTT 载荷路径，贴错列必须报错。
	raw = "point,address,type\n" +
		"power,plant/line#data.power,float32\n"
	if _, err := ParseCSV(strings.NewReader(raw), "opcua"); err == nil {
		t.Fatal("selector must be rejected for opcua")
	}

	raw = "point,address,type\n" +
		"temp,plant/line#data.temp,float32\n"
	res, err = ParseCSV(strings.NewReader(raw), "mqtt")
	if err != nil {
		t.Fatalf("mqtt parse: %v", err)
	}
	if res.Points[0].Address.Selector != "data.temp" {
		t.Fatalf("selector = %q", res.Points[0].Address.Selector)
	}
}

func TestParseCSVSkipsBlankLines(t *testing.T) {
	raw := "point,address,type\n" +
		"\n" +
		"a,holding:0,uint16\n" +
		"   \n"
	res, err := ParseCSV(strings.NewReader(raw), "modbus-tcp")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Points) != 1 {
		t.Fatalf("points = %d", len(res.Points))
	}
}

// 错误信息只展开前几条，剩下只报数量——1000 行全错时刷屏没有意义。
func TestErrorsTruncates(t *testing.T) {
	var es Errors
	for i := 0; i < 12; i++ {
		es = append(es, &RowError{Line: i + 2, Message: "boom"})
	}
	msg := es.Error()
	if !strings.Contains(msg, "and 4 more") {
		t.Fatalf("message = %q", msg)
	}
}

// asErrors 用类型断言判断错误集合。单独写一层是为了让用例读起来
// 不依赖具体的错误包装方式。
func asErrors(err error, target *Errors) bool {
	if es, ok := err.(Errors); ok {
		*target = es
		return true
	}
	return false
}
