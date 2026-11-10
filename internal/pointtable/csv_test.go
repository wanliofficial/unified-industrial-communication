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

// 远动点表必须能从 CSV 一路跑到结构化地址，否则 `uic import` 这条路对
// IEC 101/104 就是废的——而 CI 里没有设备，这条路径没人会手测。
//
// 这条用例是冲着一次真实缺陷来的：`PolicyFor("iec101")` 接上 `WidthNone`
// 之后，`parseRegister` 里仍然保留着「bool 必须有位号」与「必须推导长度」
// 两条寄存器协议的习惯，结果一份**完全正确**的远动点表在导入期被全数拒绝
// （`bool point needs a bit (e.g. ioa:2001.0)`）。直接写 JSON 的示例配置
// 绕过了这一层，所以自检通过也发现不了。
func TestParseCSVIEC101(t *testing.T) {
	raw := "point,address,type,name,writable\n" +
		"temp,ioa:1001,float32,温度,\n" +
		"scaled,ioa:1003,int16,标度化,\n" +
		"run,ioa:2001,bool,运行,\n" +
		"breaker,ioa:2003,bool,双点遥信,\n" +
		"breaker-cmd,double-command:2003,uint16,双点遥控,true\n" +
		"cmd,single-command:4001,bool,单点命令,true\n" +
		"sp,setpoint-float:4002,float32,设定值,true\n" +
		"sps,setpoint-scaled:4003,int16,标度化设定值,true\n" +
		"spn,setpoint-normalized:4004,float32,归一化设定值,true\n" +
		"counter,ioa:3001,uint32,累计量,\n"
	res, err := ParseCSV(strings.NewReader(raw), "iec101")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Points) != 10 {
		t.Fatalf("points = %d, want 10", len(res.Points))
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	// 同一个 IOA 挂了两套语义（遥信 + 遥控），地址必须原样保留，不能被
	// 任何一层「去重」或「按 IOA 归一」抹平成同一条。
	if got := res.Points[3].Address.Canonical(); got != "ioa:2003:0" {
		t.Fatalf("monitored 2003 = %s", got)
	}
	if got := res.Points[4].Address.Canonical(); got != "double-command:2003:0" {
		t.Fatalf("command 2003 = %s", got)
	}
	if !res.Points[4].Writable || !res.Points[5].Writable {
		t.Fatalf("command points must stay writable: %+v", res.Points[4:6])
	}
	for _, p := range res.Points {
		if p.Address.Length != 0 {
			t.Fatalf("point %q: length = %d, want 0", p.ID, p.Address.Length)
		}
	}
}

// 远动点表里的 length 列不是「可选」而是「不该有」：写上去说明这份点表是
// 按寄存器协议抄的。导入期就要拒绝，不能留给启动后的 Open 去炸。
func TestParseCSVIEC101RejectsLengthColumn(t *testing.T) {
	raw := "point,address,type,length\ntemp,ioa:1001,float32,2\n"
	_, err := ParseCSV(strings.NewReader(raw), "iec101")
	if err == nil {
		t.Fatal("a length column must be rejected for IEC 101")
	}
	if !strings.Contains(err.Error(), "length is not expressed") {
		t.Fatalf("error = %v", err)
	}
}
